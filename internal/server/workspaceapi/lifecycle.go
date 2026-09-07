package workspaceapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.kenn.io/forge/internal/workspace/localruntime"
)

// Start launches workspace-owned background work once. The parent context is
// observed in addition to explicit Shutdown calls so root cancellation cannot
// strand the domain.
func (h *Handler) Start(parent context.Context, disableMonitors bool) {
	if h == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	h.lifecycleMu.Lock()
	if h.lifecycleStarted || h.lifecycleStopping {
		h.lifecycleMu.Unlock()
		return
	}
	h.lifecycleStarted = true
	h.lifecycleMu.Unlock()

	if h.db != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := h.db.FailInterruptedWorkspaceSetups(
			recoveryCtx, interruptedWorkspaceSetupMessage,
		); err != nil {
			slog.Error("recover interrupted workspace setups", "err", err)
		}
		if err := h.db.FailInterruptedWorkspaceDeletions(
			recoveryCtx, interruptedWorkspaceDeletionMessage,
		); err != nil {
			slog.Error("recover interrupted workspace deletions", "err", err)
		}
		cancel()
	}

	h.runBackground(func(ctx context.Context) {
		select {
		case <-parent.Done():
			h.stopBackground()
		case <-ctx.Done():
		}
	})
	if h.workspaces != nil && !disableMonitors {
		h.runBackground(h.runWorkspacePRMonitorLoop)
		h.runBackground(h.runWorkspacePushedHeadObserverLoop)
	}
}

func (h *Handler) runBackground(run func(context.Context)) bool {
	if h == nil || run == nil {
		return false
	}
	h.lifecycleMu.Lock()
	if h.lifecycleStopping {
		h.lifecycleMu.Unlock()
		return false
	}
	h.lifecycleWG.Add(1)
	ctx := h.lifecycleCtx
	h.lifecycleMu.Unlock()
	go func() {
		defer h.lifecycleWG.Done()
		run(ctx)
	}()
	return true
}

func (h *Handler) stopBackground() {
	h.lifecycleMu.Lock()
	if h.lifecycleStopping {
		h.lifecycleMu.Unlock()
		return
	}
	h.lifecycleStopping = true
	h.lifecycleCancel()
	done := h.lifecycleDone
	h.lifecycleMu.Unlock()
	go func() {
		h.lifecycleWG.Wait()
		if h.workspaceDiffCache != nil {
			h.workspaceDiffCache.Wait()
		}
		close(done)
	}()
}

// Shutdown stops Workspace background work and waits for it within ctx. It is
// safe to call concurrently and repeatedly; later callers can wait with a
// longer context after an earlier deadline expires.
func (h *Handler) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.stopBackground()
	select {
	case <-h.lifecycleDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RestoreRuntimeSessions restores persisted workspace runtime sessions after
// the runtime manager has been constructed.
func (h *Handler) RestoreRuntimeSessions(ctx context.Context) error {
	if h == nil || h.db == nil || h.runtime == nil || h.workspaces == nil {
		return nil
	}
	h.restoreWorkspaceTerminals(ctx)
	stored, err := h.db.ListAllWorkspaceRuntimeSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range stored {
		summary, err := h.workspaces.GetSummary(ctx, session.WorkspaceID)
		if err != nil {
			return err
		}
		if summary == nil {
			continue
		}
		restored := localruntime.RestoredRuntimeSession{
			WorkspaceID: session.WorkspaceID,
			SessionKey:  session.SessionKey,
			TargetKey:   session.TargetKey,
			Label:       session.Label,
			Kind:        localruntime.LaunchTargetKind(session.Kind),
			TmuxSession: session.TmuxSession,
			CWD:         summary.WorktreePath,
			CreatedAt:   session.CreatedAt,
		}
		err = h.runtime.RestoreRuntimeSessions(ctx, []localruntime.RestoredRuntimeSession{restored})
		if err == nil {
			continue
		}
		if errors.Is(err, localruntime.ErrSessionNotFound) {
			if summary.Status == "ready" && session.Kind == string(localruntime.LaunchTargetAgent) {
				if resumeErr := h.resumeWorkspaceAgent(ctx, session, restored); resumeErr == nil {
					continue
				} else {
					slog.Warn("could not resume workspace agent", "workspace_id", session.WorkspaceID, "session_key", session.SessionKey, "err", resumeErr)
				}
			}
			if _, forgetErr := h.workspaces.ForgetRuntimeSessionCreatedAt(
				ctx, session.WorkspaceID, session.SessionKey, session.CreatedAt,
			); forgetErr != nil {
				return forgetErr
			}
			// The agent exited while the daemon was down, so no exit hook
			// removed its activity report; reports do not expire on their
			// own, so the forgotten runtime takes its report with it.
			h.removeAgentActivityRuntimeSession(session.SessionKey)
			continue
		}
		if errors.Is(err, localruntime.ErrSessionUnavailable) {
			slog.Warn("runtime session unavailable after restore",
				"workspace_id", session.WorkspaceID,
				"session_key", session.SessionKey,
				"target_key", session.TargetKey,
				"tmux_session", session.TmuxSession,
				"err", err)
			continue
		}
		return err
	}
	h.reconcileAgentActivityReports(ctx)
	return nil
}

// reconcileAgentActivityReports drops agent activity reports whose runtime
// session is neither persisted nor live. Startup pruning and the periodic
// missing-tmux prune delete runtime rows without an exit hook, and reports do
// not expire on their own, so this is what keeps the report directory from
// collecting sessions that died while the daemon was down.
func (h *Handler) reconcileAgentActivityReports(ctx context.Context) {
	if h == nil || h.agentActivity == nil || h.db == nil {
		return
	}
	keep := map[string]struct{}{}
	stored, err := h.db.ListAllWorkspaceRuntimeSessions(ctx)
	if err != nil {
		slog.Warn("reconcile agent activity reports: list runtime sessions", "err", err)
		return
	}
	for _, session := range stored {
		keep[session.SessionKey] = struct{}{}
	}
	if h.runtime != nil {
		summaries, err := h.db.ListWorkspaceSummaries(ctx)
		if err != nil {
			slog.Warn("reconcile agent activity reports: list workspaces", "err", err)
			return
		}
		for _, summary := range summaries {
			for _, session := range h.runtime.ListSessions(summary.ID) {
				keep[session.Key] = struct{}{}
			}
		}
	}
	if err := h.agentActivity.RetainRuntimeSessions(keep); err != nil {
		slog.Warn("reconcile agent activity reports", "err", err)
	}
}

// HandleRuntimeSessionExit reconciles a workspace runtime exit with persisted
// session and enrichment state.
func (h *Handler) HandleRuntimeSessionExit(info localruntime.SessionInfo) {
	if h == nil {
		return
	}
	h.removeAgentActivityRuntimeSession(info.Key)
	if h.workspaces == nil {
		return
	}
	h.invalidateWorkspaceEnrichment(info.WorkspaceID)
	h.runBackground(func(ctx context.Context) {
		cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := h.workspaces.ForgetRuntimeSessionAfterExit(
			cleanupCtx, info.WorkspaceID, info.Key, info.CreatedAt, info.TmuxSession,
		); err != nil {
			slog.Warn("forget exited runtime session",
				"workspace_id", info.WorkspaceID,
				"session_key", info.Key,
				"err", err)
		}
	})
}

func (h *Handler) removeAgentActivityRuntimeSession(sessionKey string) {
	if h == nil || h.agentActivity == nil || sessionKey == "" {
		return
	}
	if err := h.agentActivity.RemoveRuntimeSession(sessionKey); err != nil {
		slog.Warn("remove agent activity report",
			"session_key", sessionKey,
			"err", err)
	}
}
