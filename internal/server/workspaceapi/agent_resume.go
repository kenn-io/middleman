package workspaceapi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func (h *Handler) resumeWorkspaceAgent(ctx context.Context, stored db.WorkspaceRuntimeSession, restored localruntime.RestoredRuntimeSession) error {
	if h.agentActivity == nil || restored.Kind != localruntime.LaunchTargetAgent {
		return fmt.Errorf("no saved agent conversation")
	}
	reports := h.agentActivity.LiveReportsForWorkspace(restored.CWD, []string{restored.SessionKey})
	if len(reports) == 0 {
		return fmt.Errorf("no saved agent conversation")
	}
	report := reports[0]
	if err := h.workspaces.PrepareAgentLaunchContext(ctx, workspace.PrepareAgentLaunchContextOptions{
		WorkspaceID: restored.WorkspaceID, TargetKey: restored.TargetKey,
	}); err != nil {
		return fmt.Errorf("prepare resumed agent context: %w", err)
	}
	session, err := h.runtime.Resume(ctx, restored, report.Agent, report.SessionID)
	if err != nil {
		return err
	}
	session.DisplayRegion = stored.DisplayRegion
	if err := h.recordRuntimeSession(ctx, restored.WorkspaceID, session, stored.Scope); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if cleanupErr := h.runtime.RollbackLaunch(cleanupCtx, session); cleanupErr != nil {
			slog.Warn("roll back resumed agent", "session_key", session.Key, "err", cleanupErr)
		}
		return err
	}
	h.forgetRecordedRuntimeSessionIfExited(ctx, session)
	slog.Info("resumed workspace agent", "workspace_id", session.WorkspaceID, "target_key", session.TargetKey)
	return nil
}

// Restore base terminals before agents create new tmux sessions. Otherwise the
// periodic prune would mistake the remaining missing bases for individual exits.
func (h *Handler) restoreWorkspaceTerminals(ctx context.Context) {
	workspaces, err := h.db.ListWorkspaces(ctx)
	if err != nil {
		slog.Warn("list workspace terminals for recovery", "err", err)
		return
	}
	for _, ws := range workspaces {
		if ws.Status != "ready" || ws.TmuxSession == "" {
			continue
		}
		info, err := os.Stat(ws.WorktreePath)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := h.workspaces.EnsureTerminal(ctx, &ws); err != nil {
			slog.Warn("restore workspace terminal", "workspace_id", ws.ID, "err", err)
		}
	}
}
