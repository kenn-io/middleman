package workflowapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server/httpapi"
)

// EventTypeWorkflowDispatchProgress reports progress for the run identified by
// the dispatch response. Clients react to these events instead of polling.
const EventTypeWorkflowDispatchProgress = "workflow_dispatch_progress"

const (
	dispatchStatusLocated    = "located"
	dispatchStatusUpdated    = "updated"
	dispatchStatusUnresolved = "unresolved"
)

// WorkflowDispatchProgressPayload is the data of a workflow_dispatch_progress event.
type WorkflowDispatchProgressPayload struct {
	Provider     string               `json:"provider"`
	PlatformHost string               `json:"platform_host"`
	RepoPath     string               `json:"repo_path"`
	Owner        string               `json:"owner"`
	Name         string               `json:"name"`
	WorkflowID   string               `json:"workflow_id"`
	DispatchID   string               `json:"dispatch_id"`
	Status       string               `json:"status"`
	Run          *WorkflowRunResponse `json:"run,omitempty"`
}

type dispatchFollowConfig struct {
	watchInterval time.Duration
	watchTimeout  time.Duration
}

var defaultDispatchFollowConfig = dispatchFollowConfig{
	watchInterval: 10 * time.Second,
	watchTimeout:  30 * time.Minute,
}

type dispatchFollow struct {
	repo       db.Repo
	ref        platform.RepoRef
	reader     platform.WorkflowRunReader
	request    platform.WorkflowDispatchRequest
	result     platform.WorkflowDispatchResult
	dispatchID string
}

func newDispatchID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(raw[:])
}

// followDispatch watches only the run returned by the accepted dispatch.
// If no run ID was returned, report that tracking is unresolved without
// guessing which run belongs to this dispatch or retrying the mutation.
func (h *Handler) followDispatch(ctx context.Context, follow dispatchFollow) {
	if follow.result.Run == nil || follow.result.Run.ID == "" {
		if ctx.Err() == nil {
			h.publishProgress(follow, dispatchStatusUnresolved, nil)
		}
		return
	}
	run := *follow.result.Run
	if follow.reader != nil && run.Status == "" {
		if current, err := follow.reader.GetWorkflowRun(ctx, follow.ref, run.ID); err == nil {
			run = current
		}
	}
	if ctx.Err() != nil {
		return
	}
	h.publishProgress(follow, dispatchStatusLocated, &run)
	if follow.reader == nil || isTerminalRun(run) {
		return
	}
	deadline := time.Now().Add(h.follow.watchTimeout)
	for time.Now().Before(deadline) {
		if !sleepContext(ctx, h.follow.watchInterval) {
			return
		}
		current, err := follow.reader.GetWorkflowRun(ctx, follow.ref, follow.result.Run.ID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("workflow run watch failed", "dispatch_id", follow.dispatchID, "run_id", follow.result.Run.ID, "err", err)
			continue
		}
		if current.Status != run.Status || current.Conclusion != run.Conclusion || !current.UpdatedAt.Equal(run.UpdatedAt) {
			run = current
			h.publishProgress(follow, dispatchStatusUpdated, &run)
		}
		if isTerminalRun(current) {
			return
		}
	}
}

func (h *Handler) publishProgress(follow dispatchFollow, status string, run *platform.WorkflowRun) {
	payload := WorkflowDispatchProgressPayload{
		Provider:     string(httpapi.ProviderKind(follow.repo)),
		PlatformHost: httpapi.ProviderHost(follow.repo),
		RepoPath:     follow.repo.RepoPath,
		Owner:        follow.repo.Owner,
		Name:         follow.repo.Name,
		WorkflowID:   follow.request.WorkflowID,
		DispatchID:   follow.dispatchID,
		Status:       status,
	}
	if run != nil {
		response := workflowRun(*run)
		payload.Run = &response
	}
	h.publish(EventTypeWorkflowDispatchProgress, payload)
}

func isTerminalRun(run platform.WorkflowRun) bool {
	return run.Status == "completed"
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
