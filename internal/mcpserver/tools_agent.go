package mcpserver

import (
	"context"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kit/agenthook"
)

type listAgentTargetsInput struct{}

type agentTargetRow struct {
	Key            string `json:"key"`
	Label          string `json:"label"`
	Source         string `json:"source"`
	Available      bool   `json:"available"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	configOrder    int
}

type listAgentTargetsOutput struct {
	Targets []agentTargetRow `json:"targets"`
}

type listWorkspaceAgentSessionsInput struct {
	WorkspaceID string `json:"workspace_id" jsonschema:"persisted Kenn Forge workspace ID"`
}

type sendAgentMessageInput struct {
	WorkspaceID       string `json:"workspace_id" jsonschema:"persisted Kenn Forge workspace ID"`
	RuntimeSessionKey string `json:"runtime_session_key" jsonschema:"live agent runtime session key"`
	Message           string `json:"message" jsonschema:"message to submit to the running coding agent"`
}

type sendAgentMessageOutput struct {
	WorkspaceID       string `json:"workspace_id"`
	RuntimeSessionKey string `json:"runtime_session_key"`
	TargetKey         string `json:"target_key"`
	MessageBytes      int    `json:"message_bytes"`
	SubmittedAt       string `json:"submitted_at"`
}

type agentInitialMessageRow struct {
	State        string `json:"state"`
	MessageBytes int    `json:"message_bytes"`
	DeliveredAt  string `json:"delivered_at,omitempty"`
}

type workspaceAgentSessionRow struct {
	Agent             string                  `json:"agent"`
	SessionID         string                  `json:"session_id"`
	RuntimeSessionKey string                  `json:"runtime_session_key"`
	TargetKey         string                  `json:"target_key"`
	State             string                  `json:"state"`
	UpdatedAt         string                  `json:"updated_at"`
	InitialMessage    *agentInitialMessageRow `json:"initial_message,omitempty"`
}

type listWorkspaceAgentSessionsOutput struct {
	Runtimes []workspaceAgentRuntimeRow `json:"runtimes"`
	Sessions []workspaceAgentSessionRow `json:"sessions"`
}

type workspaceAgentRuntimeRow struct {
	RuntimeSessionKey string `json:"runtime_session_key"`
	TargetKey         string `json:"target_key"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
	HookObserved      bool   `json:"hook_observed"`
}

func (s *Server) registerAgentTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "kenn_forge_list_agent_targets",
		Description: "List configured launch targets that can report supported coding-agent hook sessions. " +
			"Unavailable targets remain visible, but command arguments are never returned.",
	}, wrapTool(s.listAgentTargets))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "kenn_forge_list_workspace_agent_sessions",
		Description: "List live agent runtimes and their fresh hook-authoritative coding sessions for one workspace. " +
			"A runtime with hook_observed=false has launched but has not reported its first hook. This is a live projection, not session history.",
	}, wrapTool(s.listWorkspaceAgentSessions))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "kenn_forge_send_agent_message",
		Description: "Submit a follow-up message to one existing live coding-agent runtime. " +
			"Use the workspace ID and runtime session key returned by Forge.",
	}, wrapTool(s.sendAgentMessage))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "kenn_forge_spawn_workspace_with_agent",
		Description: "Create or reuse a workspace, launch one configured coding agent, submit exactly one initial message, " +
			"and observe the resulting hook session. When agent_target is omitted for a new handoff, the most used " +
			"available agent from the previous 14 days is selected. Resume can continue an existing runtime without " +
			"launching another agent. Partial resources are never cleaned up automatically.",
	}, wrapTool(s.spawnWorkspaceWithAgent))
}

func (s *Server) sendAgentMessage(
	ctx context.Context,
	in sendAgentMessageInput,
) (sendAgentMessageOutput, error) {
	workspaceID := strings.TrimSpace(in.WorkspaceID)
	runtimeSessionKey := strings.TrimSpace(in.RuntimeSessionKey)
	result, err := s.backend.SubmitAgentMessage(ctx, AgentMessageRequest{
		WorkspaceID: workspaceID, RuntimeSessionKey: runtimeSessionKey, Message: in.Message,
	})
	if err != nil {
		return sendAgentMessageOutput{}, err
	}
	return sendAgentMessageOutput{
		WorkspaceID: workspaceID, RuntimeSessionKey: runtimeSessionKey,
		TargetKey: result.TargetKey, MessageBytes: result.MessageBytes,
		SubmittedAt: formatMCPTime(result.SubmittedAt),
	}, nil
}

func (s *Server) listAgentTargets(
	ctx context.Context,
	_ listAgentTargetsInput,
) (listAgentTargetsOutput, error) {
	targets, err := s.backend.ListLaunchTargets(ctx)
	if err != nil {
		return listAgentTargetsOutput{}, err
	}
	supported := make(map[string]struct{})
	for _, profile := range agenthook.Profiles() {
		supported[string(profile.Agent)] = struct{}{}
	}
	out := listAgentTargetsOutput{Targets: make([]agentTargetRow, 0)}
	for index, target := range targets {
		key := strings.ToLower(strings.TrimSpace(target.Key))
		if target.Kind != "agent" {
			continue
		}
		if _, ok := supported[key]; !ok {
			continue
		}
		out.Targets = append(out.Targets, agentTargetRow{
			Key:            key,
			Label:          target.Label,
			Source:         target.Source,
			Available:      target.Available,
			DisabledReason: target.DisabledReason,
			configOrder:    index,
		})
	}
	slices.SortFunc(out.Targets, func(a, b agentTargetRow) int {
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

func (s *Server) listWorkspaceAgentSessions(
	ctx context.Context,
	in listWorkspaceAgentSessionsInput,
) (listWorkspaceAgentSessionsOutput, error) {
	workspaceID := strings.TrimSpace(in.WorkspaceID)
	runtime, err := s.backend.GetWorkspaceRuntime(ctx, workspaceID)
	if err != nil {
		return listWorkspaceAgentSessionsOutput{}, err
	}
	sessions, err := s.backend.ListWorkspaceAgentSessions(ctx, workspaceID)
	if err != nil {
		return listWorkspaceAgentSessionsOutput{}, err
	}
	slices.SortFunc(sessions, compareWorkspaceAgentSessions)
	out := listWorkspaceAgentSessionsOutput{
		Runtimes: make([]workspaceAgentRuntimeRow, 0),
		Sessions: make([]workspaceAgentSessionRow, 0, len(sessions)),
	}
	observedRuntimeKeys := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		observedRuntimeKeys[session.RuntimeSessionKey] = struct{}{}
		row := workspaceAgentSessionRow{
			Agent:             session.Agent,
			SessionID:         session.SessionID,
			RuntimeSessionKey: session.RuntimeSessionKey,
			TargetKey:         session.TargetKey,
			State:             session.State,
			UpdatedAt:         formatMCPTime(session.UpdatedAt),
		}
		if session.InitialMessage != nil {
			row.InitialMessage = &agentInitialMessageRow{
				State:        session.InitialMessage.State,
				MessageBytes: session.InitialMessage.MessageBytes,
			}
			if session.InitialMessage.DeliveredAt != nil {
				row.InitialMessage.DeliveredAt = formatMCPTime(
					*session.InitialMessage.DeliveredAt,
				)
			}
		}
		out.Sessions = append(out.Sessions, row)
	}
	for _, session := range runtime.Sessions {
		if session.Kind != "agent" || (session.Status != "starting" && session.Status != "running") {
			continue
		}
		_, hookObserved := observedRuntimeKeys[session.Key]
		out.Runtimes = append(out.Runtimes, workspaceAgentRuntimeRow{
			RuntimeSessionKey: session.Key,
			TargetKey:         session.TargetKey,
			Status:            session.Status,
			CreatedAt:         formatMCPTime(session.CreatedAt),
			HookObserved:      hookObserved,
		})
	}
	slices.SortFunc(out.Runtimes, func(a, b workspaceAgentRuntimeRow) int {
		if order := strings.Compare(a.CreatedAt, b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.RuntimeSessionKey, b.RuntimeSessionKey)
	})
	return out, nil
}

func compareWorkspaceAgentSessions(a, b WorkspaceAgentSession) int {
	if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
		return order
	}
	if order := strings.Compare(a.Agent, b.Agent); order != 0 {
		return order
	}
	if order := strings.Compare(a.SessionID, b.SessionID); order != 0 {
		return order
	}
	return strings.Compare(a.RuntimeSessionKey, b.RuntimeSessionKey)
}
