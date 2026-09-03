package workspaceapi

import (
	"context"
	"slices"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/fleet"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

// FleetSnapshot is the canonical workspace-owned projection consumed by
// Fleet. Its summaries are detached from the manager's mutable state.
type FleetSnapshot struct {
	Workspaces []fleet.RawWorkspace
}

// RuntimeSnapshot is the canonical Workspace-owned runtime-session projection.
type RuntimeSnapshot []localruntime.SessionInfo

// FleetSnapshot returns the current local workspace inventory.
func (h *Handler) FleetSnapshot(ctx context.Context) (FleetSnapshot, error) {
	return h.fleetSnapshot(ctx, true)
}

// FleetStatsSnapshot returns the same detached inventory without scheduling
// list maintenance. The immediate background stats pass runs during startup,
// before newly admitted workspaces exist, and must not consume the user's
// first tmux-prune window.
func (h *Handler) FleetStatsSnapshot(ctx context.Context) (FleetSnapshot, error) {
	return h.fleetSnapshot(ctx, false)
}

func (h *Handler) fleetSnapshot(
	ctx context.Context, scheduleMaintenance bool,
) (FleetSnapshot, error) {
	var summaries []db.WorkspaceSummary
	var err error
	if h == nil || h.workspaces == nil {
		if h == nil || h.db == nil {
			return FleetSnapshot{}, nil
		}
		summaries, err = h.db.ListWorkspaceSummaries(ctx)
	} else {
		if scheduleMaintenance {
			h.scheduleWorkspaceTmuxPrune()
		}
		summaries, err = h.workspaces.ListSummaries(ctx)
	}
	if err != nil {
		return FleetSnapshot{}, err
	}
	if h.workspaces != nil {
		h.trimWorkspaceEnrichmentCache(summaries)
	}
	workspaces := make([]fleet.RawWorkspace, len(summaries))
	for index := range summaries {
		response := h.toCachedWorkspaceResponse(&summaries[index])
		workspaces[index] = rawWorkspaceFromSummary(summaries[index], response)
	}
	return FleetSnapshot{Workspaces: workspaces}, nil
}

func rawWorkspaceFromSummary(
	summary db.WorkspaceSummary,
	response workspaceResponse,
) fleet.RawWorkspace {
	workspace := fleet.RawWorkspace{
		ID: summary.ID,
		Repository: fleet.RepositoryIdentity{
			Provider: summary.Platform, PlatformHost: summary.PlatformHost,
			PlatformRepoID: summary.RepoPlatformID,
			Owner:          summary.RepoOwner, Name: summary.RepoName,
		},
		ItemType: summary.ItemType, ItemNumber: summary.ItemNumber,
		SourceItemVisible: summary.SourceItemVisible,
		ItemKey:           summary.ItemKey, GitHeadRef: summary.GitHeadRef,
		WorktreePath: response.WorktreePath, TmuxSession: response.TmuxSession,
		SessionBackend: fleetWorkspaceSessionBackend(summary.TerminalBackend),
		TmuxPaneTitle:  response.TmuxPaneTitle, TmuxWorking: response.TmuxWorking,
		TmuxActivitySource: response.TmuxActivitySource,
		TmuxLastOutputAt:   response.TmuxLastOutputAt,
		AgentState:         response.AgentState, AgentStateUpdatedAt: response.AgentStateUpdatedAt,
		Status: response.Status, ErrorMessage: response.ErrorMessage,
		CreatedAt:    response.CreatedAt,
		CommitsAhead: response.CommitsAhead, CommitsBehind: response.CommitsBehind,
		BranchUpstreamMissing: response.BranchUpstreamMissing,
		WorktreeDirty:         response.WorktreeDirty,
		EnrichmentStatus:      response.EnrichmentStatus,
		EnrichmentRefreshedAt: response.EnrichmentRefreshedAt,
		EnrichmentError:       response.EnrichmentError,
		AssociatedPRNumber:    response.AssociatedPRNumber,
	}
	if summary.KataMetadata != nil {
		workspace.Kata = &fleet.RawWorkspaceKata{
			DaemonID:    summary.KataMetadata.DaemonID,
			ProjectUID:  summary.KataMetadata.ProjectUID,
			ProjectName: summary.KataMetadata.ProjectName,
			IssueUID:    summary.KataMetadata.IssueUID,
			ShortID:     summary.KataMetadata.ShortID,
			QualifiedID: summary.KataMetadata.QualifiedID,
			Title:       summary.KataMetadata.Title,
		}
	}
	return workspace
}

func fleetWorkspaceSessionBackend(backend string) string {
	switch backend {
	case workspace.TerminalBackendPtyOwner:
		return fleet.SessionBackendLocalPTY
	case workspace.TerminalBackendTmux:
		return fleet.SessionBackendLocalTmux
	default:
		return ""
	}
}

// RuntimeSnapshot returns a detached view of runtime sessions for a workspace
// or project-worktree scope.
func (h *Handler) RuntimeSnapshot(scope string) RuntimeSnapshot {
	if h == nil || h.runtime == nil {
		return nil
	}
	return slices.Clone(h.runtime.ListSessions(scope))
}
