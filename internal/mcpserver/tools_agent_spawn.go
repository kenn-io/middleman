package mcpserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/forge/platform"
)

const (
	defaultAgentHandoffTimeout      = 5 * time.Minute
	maxAgentHandoffTimeout          = 15 * time.Minute
	defaultAgentHandoffPollInterval = 250 * time.Millisecond
	messageStatusRecoveryTimeout    = 6 * time.Second
	maxAgentInitialMessage          = 64 << 10
	workspaceAgentPreferenceWindow  = 14 * 24 * time.Hour
)

type workspaceSourceInput struct {
	Type  string                `json:"type" jsonschema:"source type: item or adhoc"`
	Item  *itemRefInput         `json:"item,omitempty"`
	AdHoc *adHocWorkspaceSource `json:"adhoc,omitempty"`
}

type adHocWorkspaceSource struct {
	Repo   repoFilterInput `json:"repo"`
	Branch string          `json:"branch,omitempty"`
}

type spawnWorkspaceWithAgentInput struct {
	Source         *workspaceSourceInput `json:"source,omitempty"`
	Resume         *agentHandoffResume   `json:"resume,omitempty"`
	AgentTarget    string                `json:"agent_target,omitempty" jsonschema:"configured coding-agent target; omit on a new handoff to use recent workspace launch history"`
	InitialMessage string                `json:"initial_message"`
	Timeout        string                `json:"timeout,omitempty"`
}

type agentHandoffResume struct {
	WorkspaceID       string `json:"workspace_id"`
	RuntimeSessionKey string `json:"runtime_session_key"`
}

type spawnedWorkspace struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reused bool   `json:"reused"`
}

type spawnedRuntime struct {
	SessionKey string `json:"session_key"`
	TargetKey  string `json:"target_key"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

type spawnWorkspaceWithAgentOutput struct {
	Stage          string                    `json:"stage"`
	Source         *workspaceSourceInput     `json:"source,omitempty"`
	Workspace      spawnedWorkspace          `json:"workspace"`
	Runtime        spawnedRuntime            `json:"runtime"`
	CodingSession  *workspaceAgentSessionRow `json:"coding_session,omitempty"`
	InitialMessage *agentInitialMessageRow   `json:"initial_message,omitempty"`
}

func (s *Server) spawnWorkspaceWithAgent(
	ctx context.Context,
	in spawnWorkspaceWithAgentInput,
) (spawnWorkspaceWithAgentOutput, error) {
	normalized, timeout, err := validateSpawnWorkspaceInput(in)
	if err != nil {
		return spawnWorkspaceWithAgentOutput{}, err
	}
	in = normalized
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := spawnWorkspaceWithAgentOutput{Source: in.Source}
	workspace, runtime, err := s.prepareAgentHandoff(ctx, in, &out)
	if err != nil {
		return out, err
	}

	messageStatus, err := s.submitInitialAgentMessage(
		ctx, workspace.ID, runtime.Key, runtime.TargetKey, in.InitialMessage,
	)
	if messageStatus.State != "" {
		out.InitialMessage = &messageStatus
	}
	if err != nil {
		return out, handoffFailure(ctx, err, out, "runtime_launched", "message_delivered")
	}
	if messageStatus.State != "delivered" {
		stateErr := &Error{
			Kind:      "agent_handoff_failed",
			Message:   fmt.Sprintf("initial message state is %s", messageStatus.State),
			Ambiguous: messageStatus.State == "pending" || messageStatus.State == "uncertain",
			Details:   map[string]any{"initial_message_state": messageStatus.State},
		}
		return out, handoffFailure(
			ctx, stateErr, out, "runtime_launched", "message_delivered",
		)
	}
	out.Stage = "message_delivered"

	codingSession, err := s.waitForCodingSession(
		ctx, workspace.ID, runtime.Key, runtime.TargetKey,
	)
	if err != nil {
		return out, handoffFailure(ctx, err, out, "message_delivered", "coding_session_observed")
	}
	out.Stage = "coding_session_observed"
	out.CodingSession = &codingSession
	return out, nil
}

func validateSpawnWorkspaceInput(
	in spawnWorkspaceWithAgentInput,
) (spawnWorkspaceWithAgentInput, time.Duration, error) {
	timeout := defaultAgentHandoffTimeout
	if strings.TrimSpace(in.Timeout) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(in.Timeout))
		if err != nil || parsed <= 0 {
			return in, 0, fmt.Errorf("timeout must be a positive duration")
		}
		timeout = parsed
	}
	if timeout > maxAgentHandoffTimeout {
		return in, 0, fmt.Errorf("timeout must not exceed 15m")
	}
	in.AgentTarget = strings.ToLower(strings.TrimSpace(in.AgentTarget))
	message, err := normalizeSpawnInitialMessage(in.InitialMessage)
	if err != nil {
		return in, 0, err
	}
	in.InitialMessage = message
	if (in.Source == nil) == (in.Resume == nil) {
		return in, 0, fmt.Errorf("provide exactly one of source or resume")
	}
	if in.Resume != nil {
		if in.AgentTarget == "" {
			return in, 0, fmt.Errorf("agent_target is required when resume is provided")
		}
		in.Resume.WorkspaceID = strings.TrimSpace(in.Resume.WorkspaceID)
		in.Resume.RuntimeSessionKey = strings.TrimSpace(in.Resume.RuntimeSessionKey)
		if in.Resume.WorkspaceID == "" {
			return in, 0, fmt.Errorf("resume.workspace_id is required")
		}
		if in.Resume.RuntimeSessionKey == "" {
			return in, 0, fmt.Errorf("resume.runtime_session_key is required")
		}
		return in, timeout, nil
	}
	in.Source.Type = strings.ToLower(strings.TrimSpace(in.Source.Type))
	switch in.Source.Type {
	case "item":
		if in.Source.Item == nil || in.Source.AdHoc != nil {
			return in, 0, fmt.Errorf("source must contain exactly one tagged item")
		}
		item, err := normalizeSpawnItem(*in.Source.Item)
		if err != nil {
			return in, 0, err
		}
		in.Source.Item = &item
	case "adhoc":
		if in.Source.AdHoc == nil || in.Source.Item != nil {
			return in, 0, fmt.Errorf("source must contain exactly one tagged adhoc repo")
		}
		repo, err := normalizeSpawnRepo(in.Source.AdHoc.Repo)
		if err != nil {
			return in, 0, err
		}
		in.Source.AdHoc.Repo = repo
		in.Source.AdHoc.Branch = strings.TrimSpace(in.Source.AdHoc.Branch)
	default:
		return in, 0, fmt.Errorf("source.type must be item or adhoc")
	}
	return in, timeout, nil
}

func (s *Server) prepareAgentHandoff(
	ctx context.Context,
	in spawnWorkspaceWithAgentInput,
	out *spawnWorkspaceWithAgentOutput,
) (Workspace, RuntimeSession, error) {
	if in.Resume != nil {
		return s.resumeAgentHandoff(ctx, in, out)
	}
	targets, err := s.listAgentTargets(ctx, listAgentTargetsInput{})
	if err != nil {
		return Workspace{}, RuntimeSession{}, handoffFailure(ctx, err, *out, "", "")
	}
	if in.AgentTarget == "" {
		in.AgentTarget, err = s.defaultAgentTarget(ctx, targets.Targets)
		if err != nil {
			return Workspace{}, RuntimeSession{}, handoffFailure(ctx, err, *out, "", "")
		}
	}
	target, ok := findAgentTarget(targets.Targets, in.AgentTarget)
	if !ok {
		return Workspace{}, RuntimeSession{}, fmt.Errorf(
			"agent_target %q is not a configured coding-agent target", in.AgentTarget,
		)
	}
	if !target.Available {
		return Workspace{}, RuntimeSession{}, fmt.Errorf(
			"agent_target %q is unavailable: %s", in.AgentTarget, target.DisabledReason,
		)
	}

	workspace, reused, err := s.resolveOrCreateWorkspace(ctx, *in.Source)
	if err != nil {
		return Workspace{}, RuntimeSession{}, handoffFailure(
			ctx, err, *out, "", "workspace_created",
		)
	}
	if in.Source.AdHoc != nil && in.Source.AdHoc.Branch == "" {
		out.Source.AdHoc.Branch = workspace.GitHeadRef
	}
	out.Stage = "workspace_created"
	out.Workspace = spawnedWorkspace{ID: workspace.ID, Status: workspace.Status, Reused: reused}

	workspace, err = s.waitForWorkspaceReady(ctx, workspace.ID)
	out.Workspace.Status = workspace.Status
	if err != nil {
		return workspace, RuntimeSession{}, handoffFailure(
			ctx, err, *out, "workspace_created", "workspace_ready",
		)
	}
	out.Stage = "workspace_ready"

	runtime, err := s.backend.LaunchWorkspaceRuntime(ctx, workspace.ID, in.AgentTarget)
	if err != nil {
		return workspace, RuntimeSession{}, handoffFailure(
			ctx, err, *out, "workspace_ready", "runtime_launched",
		)
	}
	out.Stage = "runtime_launched"
	out.Runtime = spawnedRuntimeFrom(runtime)
	return workspace, runtime, nil
}

func (s *Server) resumeAgentHandoff(
	ctx context.Context,
	in spawnWorkspaceWithAgentInput,
	out *spawnWorkspaceWithAgentOutput,
) (Workspace, RuntimeSession, error) {
	workspace, err := s.backend.GetWorkspace(ctx, in.Resume.WorkspaceID)
	if err != nil {
		return Workspace{}, RuntimeSession{}, handoffFailure(
			ctx, err, *out, "", "workspace_ready",
		)
	}
	out.Workspace = spawnedWorkspace{ID: workspace.ID, Status: workspace.Status, Reused: true}
	if workspace.Status != "ready" {
		return workspace, RuntimeSession{}, handoffFailure(
			ctx, fmt.Errorf("workspace is not ready: %s", workspace.Status), *out, "", "workspace_ready",
		)
	}
	out.Stage = "workspace_ready"

	runtimeState, err := s.backend.GetWorkspaceRuntime(ctx, workspace.ID)
	if err != nil {
		return workspace, RuntimeSession{}, handoffFailure(
			ctx, err, *out, "workspace_ready", "runtime_launched",
		)
	}
	for _, runtime := range runtimeState.Sessions {
		if runtime.Key != in.Resume.RuntimeSessionKey {
			continue
		}
		out.Runtime = spawnedRuntimeFrom(runtime)
		if runtime.TargetKey != in.AgentTarget {
			return workspace, runtime, handoffFailure(
				ctx, fmt.Errorf("agent_target does not match the existing runtime"),
				*out, "workspace_ready", "runtime_launched",
			)
		}
		if runtime.Kind != "agent" || (runtime.Status != "starting" && runtime.Status != "running") {
			return workspace, runtime, handoffFailure(
				ctx, fmt.Errorf("agent runtime is not live"), *out,
				"workspace_ready", "runtime_launched",
			)
		}
		out.Stage = "runtime_launched"
		return workspace, runtime, nil
	}
	return workspace, RuntimeSession{}, handoffFailure(
		ctx, fmt.Errorf("agent runtime was not found"), *out,
		"workspace_ready", "runtime_launched",
	)
}

func spawnedRuntimeFrom(runtime RuntimeSession) spawnedRuntime {
	return spawnedRuntime{
		SessionKey: runtime.Key,
		TargetKey:  runtime.TargetKey,
		Status:     runtime.Status,
		CreatedAt:  formatMCPTime(runtime.CreatedAt),
	}
}

func normalizeSpawnItem(item itemRefInput) (itemRefInput, error) {
	item.Type = strings.ToLower(strings.TrimSpace(item.Type))
	item.Provider = strings.ToLower(strings.TrimSpace(item.Provider))
	item.PlatformHost = strings.TrimSpace(item.PlatformHost)
	item.PlatformRepoID = strings.TrimSpace(item.PlatformRepoID)
	item.Owner = strings.Trim(strings.TrimSpace(item.Owner), "/")
	item.Name = strings.Trim(strings.TrimSpace(item.Name), "/")
	if err := validateItemRef(item); err != nil {
		return item, err
	}
	kind, err := platform.NormalizeKind(item.Provider)
	if err != nil {
		return item, err
	}
	metadata, ok := platform.MetadataFor(kind)
	if !ok {
		return item, fmt.Errorf("unsupported provider %q", item.Provider)
	}
	item.Provider = string(kind)
	if item.PlatformHost == "" {
		item.PlatformHost = metadata.DefaultHost
	}
	return item, nil
}

func normalizeSpawnRepo(repo repoFilterInput) (repoFilterInput, error) {
	repo.Provider = strings.ToLower(strings.TrimSpace(repo.Provider))
	repo.PlatformHost = strings.TrimSpace(repo.PlatformHost)
	repo.PlatformRepoID = strings.TrimSpace(repo.PlatformRepoID)
	repo.RepoPath = strings.Trim(strings.TrimSpace(repo.RepoPath), "/")
	repo.Owner = strings.Trim(strings.TrimSpace(repo.Owner), "/")
	repo.Name = strings.Trim(strings.TrimSpace(repo.Name), "/")
	if repo.Provider == "" {
		return repo, fmt.Errorf("repo provider is required")
	}
	if repo.PlatformRepoID == "" {
		return repo, fmt.Errorf("repo platform_repo_id is required")
	}
	kind, err := platform.NormalizeKind(repo.Provider)
	if err != nil {
		return repo, err
	}
	metadata, ok := platform.MetadataFor(kind)
	if !ok {
		return repo, fmt.Errorf("unsupported provider %q", repo.Provider)
	}
	repo.Provider = string(kind)
	if repo.PlatformHost == "" {
		repo.PlatformHost = metadata.DefaultHost
	}
	if repo.RepoPath != "" {
		parts := strings.Split(repo.RepoPath, "/")
		if len(parts) < 2 || slicesContainEmpty(parts) {
			return repo, fmt.Errorf("repo_path must contain an owner and repository name")
		}
		pathOwner := strings.Join(parts[:len(parts)-1], "/")
		pathName := parts[len(parts)-1]
		if (repo.Owner != "" || repo.Name != "") &&
			(repo.Owner != pathOwner || repo.Name != pathName) {
			return repo, fmt.Errorf("repo_path conflicts with repo owner or name")
		}
		repo.Owner = pathOwner
		repo.Name = pathName
	}
	if repo.Owner == "" {
		return repo, fmt.Errorf("repo owner is required")
	}
	if repo.Name == "" {
		return repo, fmt.Errorf("repo name is required")
	}
	if repo.RepoPath == "" {
		repo.RepoPath = repo.Owner + "/" + repo.Name
	}
	return repo, nil
}

func slicesContainEmpty(values []string) bool {
	return slices.Contains(values, "")
}

func normalizeSpawnInitialMessage(message string) (string, error) {
	if !utf8.ValidString(message) {
		return "", fmt.Errorf("initial_message must be valid UTF-8")
	}
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\n")
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("initial_message must not be blank")
	}
	for _, value := range message {
		if value != '\n' && !unicode.IsPrint(value) {
			return "", fmt.Errorf("initial_message contains an unsafe control character")
		}
	}
	if len(message) > maxAgentInitialMessage {
		return "", fmt.Errorf("initial_message must not exceed 64 KiB")
	}
	return message, nil
}

func findAgentTarget(targets []agentTargetRow, key string) (agentTargetRow, bool) {
	for _, target := range targets {
		if target.Key == key {
			return target, true
		}
	}
	return agentTargetRow{}, false
}

func (s *Server) defaultAgentTarget(
	ctx context.Context,
	targets []agentTargetRow,
) (string, error) {
	available := make([]agentTargetRow, 0, len(targets))
	keys := make([]string, 0, len(targets))
	for _, target := range targets {
		if !target.Available {
			continue
		}
		available = append(available, target)
		keys = append(keys, target.Key)
	}
	if len(available) == 0 {
		return "", fmt.Errorf("no available coding-agent targets are configured")
	}
	preferred, found, err := s.backend.PreferredWorkspaceAgentTarget(
		ctx, time.Now().UTC().Add(-workspaceAgentPreferenceWindow), keys,
	)
	if err != nil {
		return "", err
	}
	if found {
		return preferred, nil
	}
	slices.SortFunc(available, func(a, b agentTargetRow) int {
		return cmp.Compare(a.configOrder, b.configOrder)
	})
	return available[0].Key, nil
}

func (s *Server) resolveOrCreateWorkspace(
	ctx context.Context,
	source workspaceSourceInput,
) (Workspace, bool, error) {
	if source.Item != nil {
		switch source.Item.Type {
		case "pr":
			return s.resolveOrCreatePRWorkspace(ctx, *source.Item)
		case "issue":
			return s.resolveOrCreateIssueWorkspace(ctx, *source.Item)
		}
	}
	if source.AdHoc != nil {
		return s.createAdHocWorkspace(ctx, *source.AdHoc)
	}
	return Workspace{}, false, fmt.Errorf("unsupported workspace source")
}

func (s *Server) resolveOrCreatePRWorkspace(
	ctx context.Context,
	item itemRefInput,
) (Workspace, bool, error) {
	detail, err := s.backend.GetPull(ctx, itemIdentity(item))
	if err != nil {
		return Workspace{}, false, err
	}
	if detail.Workspace != nil {
		return Workspace{ID: detail.Workspace.ID, Status: detail.Workspace.Status}, true, nil
	}
	workspace, err := s.backend.CreatePullWorkspace(ctx, itemIdentity(item), true)
	if err == nil {
		return workspace, !workspace.Created, nil
	}
	if !isWorkspaceAlreadyExistsError(err) {
		return Workspace{}, false, err
	}
	detail, readErr := s.backend.GetPull(ctx, itemIdentity(item))
	if readErr != nil {
		return Workspace{}, false, readErr
	}
	if detail.Workspace == nil {
		return Workspace{}, false, err
	}
	return Workspace{ID: detail.Workspace.ID, Status: detail.Workspace.Status}, true, nil
}

func isWorkspaceAlreadyExistsError(err error) bool {
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr == nil {
		return false
	}
	return backendErr.Kind == "conflict" &&
		backendErr.Code == ErrorCodeWorkspaceAlreadyExists
}

func (s *Server) resolveOrCreateIssueWorkspace(
	ctx context.Context,
	item itemRefInput,
) (Workspace, bool, error) {
	detail, err := s.backend.GetIssue(ctx, itemIdentity(item))
	if err != nil {
		return Workspace{}, false, err
	}
	if detail.Workspace != nil {
		return Workspace{ID: detail.Workspace.ID, Status: detail.Workspace.Status}, true, nil
	}
	workspace, err := s.backend.CreateIssueWorkspace(ctx, itemIdentity(item), true)
	if err != nil {
		return Workspace{}, false, err
	}
	return workspace, !workspace.Created, nil
}

func (s *Server) createAdHocWorkspace(
	ctx context.Context,
	source adHocWorkspaceSource,
) (Workspace, bool, error) {
	repository, err := source.Repo.repositoryIdentity()
	if err != nil {
		return Workspace{}, false, err
	}
	workspace, err := s.backend.CreateAdHocWorkspace(ctx, repository, source.Branch)
	if err != nil {
		return Workspace{}, false, err
	}
	return workspace, !workspace.Created, nil
}

func (s *Server) waitForWorkspaceReady(
	ctx context.Context,
	workspaceID string,
) (Workspace, error) {
	for {
		workspace, err := s.backend.GetWorkspace(ctx, workspaceID)
		if err != nil {
			return workspace, err
		}
		switch workspace.Status {
		case "ready":
			return workspace, nil
		case "error":
			message := "workspace setup failed"
			if workspace.ErrorMessage != nil && strings.TrimSpace(*workspace.ErrorMessage) != "" {
				message += ": " + strings.TrimSpace(*workspace.ErrorMessage)
			}
			return workspace, errors.New(message)
		}
		if err := s.waitAgentHandoffPoll(ctx); err != nil {
			return workspace, err
		}
	}
}

func (s *Server) waitForCodingSession(
	ctx context.Context,
	workspaceID string,
	runtimeSessionKey string,
	targetKey string,
) (workspaceAgentSessionRow, error) {
	for {
		response, err := s.listWorkspaceAgentSessions(
			ctx, listWorkspaceAgentSessionsInput{WorkspaceID: workspaceID},
		)
		if err != nil {
			return workspaceAgentSessionRow{}, err
		}
		for _, session := range response.Sessions {
			if session.RuntimeSessionKey == runtimeSessionKey &&
				session.TargetKey == targetKey {
				return session, nil
			}
		}
		if err := s.ensureRuntimeStillLive(ctx, workspaceID, runtimeSessionKey); err != nil {
			return workspaceAgentSessionRow{}, err
		}
		if err := s.waitAgentHandoffPoll(ctx); err != nil {
			return workspaceAgentSessionRow{}, err
		}
	}
}

func (s *Server) ensureRuntimeStillLive(
	ctx context.Context,
	workspaceID string,
	runtimeSessionKey string,
) error {
	runtime, err := s.backend.GetWorkspaceRuntime(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, session := range runtime.Sessions {
		if session.Key != runtimeSessionKey {
			continue
		}
		if session.Status == "starting" || session.Status == "running" {
			return nil
		}
		break
	}
	return fmt.Errorf("agent runtime exited before its coding session was observed")
}

func (s *Server) waitAgentHandoffPoll(ctx context.Context) error {
	interval := s.agentHandoffPollInterval
	if interval <= 0 {
		interval = defaultAgentHandoffPollInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Server) submitInitialAgentMessage(
	ctx context.Context,
	workspaceID string,
	runtimeSessionKey string,
	targetKey string,
	message string,
) (agentInitialMessageRow, error) {
	var messageStatus InitialMessageStatus
	for {
		status, err := s.backend.SubmitInitialMessage(ctx, InitialMessageRequest{
			WorkspaceID: workspaceID, RuntimeSessionKey: runtimeSessionKey,
			TargetKey: targetKey, Message: message,
		})
		messageStatus = status
		if err == nil {
			break
		}
		var backendErr *Error
		if !errors.As(err, &backendErr) || backendErr == nil {
			return initialMessageStatusRow(messageStatus), err
		}
		if backendErr.Code == ErrorCodeInitialMessageInputModeNotReady &&
			backendErr.Retryable && !backendErr.Ambiguous {
			if err := s.waitAgentHandoffPoll(ctx); err != nil {
				return agentInitialMessageRow{}, err
			}
			continue
		}
		if !backendErr.Ambiguous {
			return initialMessageStatusRow(messageStatus), err
		}
		recoveredStatus, recoveryErr := s.recoverInitialMessageStatus(
			ctx, workspaceID, runtimeSessionKey, backendErr,
		)
		if recoveryErr != nil {
			return initialMessageStatusRow(messageStatus), recoveryErr
		}
		messageStatus = recoveredStatus
		break
	}
	return initialMessageStatusRow(messageStatus), nil
}

func initialMessageStatusRow(messageStatus InitialMessageStatus) agentInitialMessageRow {
	row := agentInitialMessageRow{
		State: messageStatus.State, MessageBytes: messageStatus.MessageBytes,
	}
	if messageStatus.DeliveredAt != nil {
		row.DeliveredAt = formatMCPTime(*messageStatus.DeliveredAt)
	}
	return row
}

func (s *Server) recoverInitialMessageStatus(
	ctx context.Context,
	workspaceID string,
	runtimeSessionKey string,
	original *Error,
) (InitialMessageStatus, error) {
	timeout := messageStatusRecoveryTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	for {
		recovered, err := s.backend.GetInitialMessage(recoveryCtx, workspaceID, runtimeSessionKey)
		if err != nil {
			return InitialMessageStatus{}, original
		}
		if recovered.State == "delivered" {
			return recovered, nil
		}
		if recovered.State != "pending" {
			return InitialMessageStatus{}, initialMessageRecoveryError(original, recovered.State)
		}
		if err := s.waitAgentHandoffPoll(recoveryCtx); err != nil {
			return InitialMessageStatus{}, initialMessageRecoveryError(original, recovered.State)
		}
	}
}

func initialMessageRecoveryError(original *Error, state string) *Error {
	recovered := *original
	recovered.Details = maps.Clone(original.Details)
	if recovered.Details == nil {
		recovered.Details = make(map[string]any)
	}
	recovered.Details["initial_message_state"] = state
	return &recovered
}

func handoffFailure(
	ctx context.Context,
	cause error,
	state spawnWorkspaceWithAgentOutput,
	lastCompletedStage string,
	failedStage string,
) *Error {
	result := &Error{
		Kind: "agent_handoff_failed", Message: cause.Error(), Retryable: false,
		Details: map[string]any{},
	}
	var backendErr *Error
	if errors.As(cause, &backendErr) {
		result.Kind = backendErr.Kind
		result.Code = backendErr.Code
		result.Message = backendErr.Message
		result.Ambiguous = backendErr.Ambiguous
		maps.Copy(result.Details, backendErr.Details)
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		result.Kind = "agent_handoff_timeout"
		result.Message = "agent handoff timed out"
	} else if backendErr == nil && errors.Is(cause, context.DeadlineExceeded) {
		result.Kind = "agent_handoff_timeout"
		result.Message = "agent handoff timed out"
	}
	if failedStage != "" {
		result.Details["failed_stage"] = failedStage
	}
	if lastCompletedStage != "" {
		result.Details["last_completed_stage"] = lastCompletedStage
	}
	if state.Workspace.ID != "" {
		result.Details["workspace_id"] = state.Workspace.ID
		result.Details["workspace_status"] = state.Workspace.Status
		result.Details["workspace_reused"] = state.Workspace.Reused
	}
	if state.Runtime.SessionKey != "" {
		result.Details["runtime_session_key"] = state.Runtime.SessionKey
		result.Details["target_key"] = state.Runtime.TargetKey
	}
	if state.CodingSession != nil {
		result.Details["agent"] = state.CodingSession.Agent
		result.Details["session_id"] = state.CodingSession.SessionID
	}
	if state.InitialMessage != nil {
		result.Details["initial_message_state"] = state.InitialMessage.State
	}
	return result
}
