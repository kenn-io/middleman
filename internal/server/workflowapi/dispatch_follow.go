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

// EventTypeWorkflowDispatchProgress carries dispatch follow-through to clients.
// Providers accept a manual dispatch without naming the run it creates, so the
// server locates that run afterwards and then reports status changes until it
// finishes. Clients react to these events instead of polling.
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
	locateInitialDelay time.Duration
	locateInterval     time.Duration
	locateTimeout      time.Duration
	clockSkew          time.Duration
	watchInterval      time.Duration
	watchTimeout       time.Duration
}

var defaultDispatchFollowConfig = dispatchFollowConfig{
	locateInitialDelay: 2 * time.Second,
	locateInterval:     3 * time.Second,
	locateTimeout:      time.Minute,
	clockSkew:          5 * time.Second,
	watchInterval:      10 * time.Second,
	watchTimeout:       30 * time.Minute,
}

type dispatchFollow struct {
	repo       db.Repo
	ref        platform.RepoRef
	reader     platform.WorkflowRunReader
	request    platform.WorkflowDispatchRequest
	result     platform.WorkflowDispatchResult
	dispatchID string
	startedAt  time.Time
}

func newDispatchID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(raw[:])
}

// followDispatch runs after the provider accepted a dispatch. It publishes a
// located event once the run is known, updated events while the run changes,
// and an unresolved event when no matching run appears in time.
func (h *Handler) followDispatch(ctx context.Context, follow dispatchFollow) {
	run, ok := h.locateDispatchedRun(ctx, follow)
	if !ok {
		if ctx.Err() == nil {
			h.publishProgress(follow, dispatchStatusUnresolved, nil)
		}
		return
	}
	h.publishProgress(follow, dispatchStatusLocated, &run)
	h.watchDispatchedRun(ctx, follow, run)
}

func (h *Handler) locateDispatchedRun(ctx context.Context, follow dispatchFollow) (platform.WorkflowRun, bool) {
	if follow.result.Run != nil && follow.result.Run.ID != "" {
		return h.enrichNamedRun(ctx, follow, *follow.result.Run), true
	}
	if follow.reader == nil {
		return platform.WorkflowRun{}, false
	}
	deadline := time.Now().Add(h.follow.locateTimeout)
	if !sleepContext(ctx, h.follow.locateInitialDelay) {
		return platform.WorkflowRun{}, false
	}
	for {
		runs, err := h.listDispatchRuns(ctx, follow)
		if err != nil {
			if ctx.Err() != nil {
				return platform.WorkflowRun{}, false
			}
			slog.Debug("workflow dispatch run lookup failed", "dispatch_id", follow.dispatchID, "err", err)
		}
		if run, found := h.matchDispatchedRun(follow, runs); found {
			return run, true
		}
		if !time.Now().Before(deadline) {
			return platform.WorkflowRun{}, false
		}
		if !sleepContext(ctx, h.follow.locateInterval) {
			return platform.WorkflowRun{}, false
		}
	}
}

// enrichNamedRun replaces a provider-named run that carries only an ID with
// the listed run when one read finds it, so clients get status, number, and
// timestamps in the first progress event rather than after the first watch tick.
func (h *Handler) enrichNamedRun(ctx context.Context, follow dispatchFollow, run platform.WorkflowRun) platform.WorkflowRun {
	if follow.reader == nil || run.Status != "" {
		return run
	}
	runs, err := h.listDispatchRuns(ctx, follow)
	if err != nil {
		return run
	}
	for _, candidate := range runs {
		if candidate.ID == run.ID {
			return candidate
		}
	}
	return run
}

func (h *Handler) listDispatchRuns(ctx context.Context, follow dispatchFollow) ([]platform.WorkflowRun, error) {
	page, err := follow.reader.ListWorkflowRuns(ctx, follow.ref, platform.WorkflowRunQuery{
		WorkflowID: follow.request.WorkflowID,
		Event:      "workflow_dispatch",
		Branch:     follow.request.Ref,
		PerPage:    20,
	})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// matchDispatchedRun picks the newest run created at or after the dispatch
// by the dispatching actor. An unknown actor matches any actor, since the
// branch, event, and creation window already narrow the candidates.
func (h *Handler) matchDispatchedRun(follow dispatchFollow, runs []platform.WorkflowRun) (platform.WorkflowRun, bool) {
	earliest := follow.startedAt.Add(-h.follow.clockSkew)
	var best platform.WorkflowRun
	found := false
	for _, run := range runs {
		if run.CreatedAt.IsZero() || run.CreatedAt.Before(earliest) {
			continue
		}
		if follow.result.Actor != "" && run.Actor != follow.result.Actor {
			continue
		}
		if !found || run.CreatedAt.After(best.CreatedAt) {
			best = run
			found = true
		}
	}
	return best, found
}

func (h *Handler) watchDispatchedRun(ctx context.Context, follow dispatchFollow, run platform.WorkflowRun) {
	if follow.reader == nil || isTerminalRun(run) {
		return
	}
	deadline := time.Now().Add(h.follow.watchTimeout)
	for time.Now().Before(deadline) {
		if !sleepContext(ctx, h.follow.watchInterval) {
			return
		}
		runs, err := h.listDispatchRuns(ctx, follow)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("workflow run watch failed", "dispatch_id", follow.dispatchID, "run_id", run.ID, "err", err)
			continue
		}
		for _, candidate := range runs {
			if candidate.ID != run.ID {
				continue
			}
			if candidate.Status != run.Status || candidate.Conclusion != run.Conclusion || !candidate.UpdatedAt.Equal(run.UpdatedAt) {
				run = candidate
				h.publishProgress(follow, dispatchStatusUpdated, &run)
			}
			if isTerminalRun(run) {
				return
			}
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
