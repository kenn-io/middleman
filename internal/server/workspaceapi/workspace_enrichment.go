package workspaceapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
)

const (
	// workspaceEnrichmentTTL is how long a divergence result stays fresh
	// before the background worker re-checks it. The re-check compares a
	// stat-only git-directory fingerprint first and only spawns git when
	// that fingerprint moved or the forced interval elapsed, so polling
	// clients no longer keep every workspace on a git probe cycle.
	workspaceEnrichmentTTL = 30 * time.Second
	// workspaceEnrichmentForcedProbeInterval bounds how long an unchanged
	// fingerprint may suppress git: the first refresh a read requests after
	// it elapses runs git again. Worktree edits that never touch the index
	// are invisible to the fingerprint, so this is how dirty state from
	// them is eventually observed. No timer probes an unread workspace.
	workspaceEnrichmentForcedProbeInterval = 3 * time.Minute
	// workspaceTmuxEnrichmentTTL is the tmux activity cadence, kept apart
	// from the git cadence. Activity and workspace reads promise that
	// changed tmux output shows up within a few seconds, so it stays short;
	// hidden tabs stop requesting it, and the tracker's own sample interval
	// bounds the actual tmux spawns.
	workspaceTmuxEnrichmentTTL        = 5 * time.Second
	workspaceEnrichmentRefreshTimeout = 2 * time.Second
	workspaceTmuxPruneInterval        = 30 * time.Second
	workspaceTmuxRecencyMinInterval   = time.Minute
	workspaceEnrichmentNotApplicable  = "not_applicable"
	workspaceEnrichmentPending        = "pending"
	workspaceEnrichmentFresh          = "fresh"
	workspaceEnrichmentStale          = "stale"
	workspaceEnrichmentFailed         = "failed"
)

// EnrichmentTTL is the cache freshness interval for workspace reconciliation.
const EnrichmentTTL = workspaceEnrichmentTTL

type workspaceEnrichmentCacheEntry struct {
	response               workspaceResponse
	hasDivergence          bool
	hasTmux                bool
	divergenceRefreshedAt  time.Time
	tmuxRefreshedAt        time.Time
	tmuxObservedOutputAt   time.Time
	tmuxPublishedOutputAt  time.Time
	tmuxRecencyPublishedAt time.Time
	lastAttemptAt          time.Time
	// divergenceProbedAt is when git last actually ran for this entry;
	// divergenceRefreshedAt also advances on fingerprint-verified skips.
	divergenceProbedAt  time.Time
	gitFingerprint      string
	divergenceError     string
	tmuxError           string
	divergenceAttemptAt time.Time
	tmuxAttemptAt       time.Time
}

type workspaceEnrichmentKind uint8

const (
	workspaceEnrichmentFull workspaceEnrichmentKind = iota
	workspaceEnrichmentTmux
)

type workspaceEnrichmentJob struct {
	summary    db.WorkspaceSummary
	generation uint64
	kind       workspaceEnrichmentKind
	flightID   uint64
}

type workspaceEnrichmentProbeResult struct {
	response           workspaceResponse
	divergenceComplete bool
	// divergenceUnchanged reports that the git fingerprint matched the
	// cached one, so the cached divergence was re-validated without git.
	divergenceUnchanged bool
	gitFingerprint      string
	tmuxComplete        bool
	// tmuxSkipped and divergenceSkipped report that a component was not
	// attempted because it was still within its own cadence; the record
	// step must leave that component's attempt time untouched.
	tmuxSkipped       bool
	divergenceSkipped bool
	divergenceErr     error
	tmuxErr           error
	err               error
	kind              workspaceEnrichmentKind
}

func (s *Handler) toCachedWorkspaceResponse(
	summary *db.WorkspaceSummary,
) (resp workspaceResponse) {
	resp = toWorkspaceResponse(summary)
	defer s.applyAgentActivity(&resp, summary)
	resp.Repo = s.repoRefFromParts(
		summary.Platform, summary.PlatformHost, summary.RepoOwner, summary.RepoName,
	)
	if s.workspaceEnrichmentDisabled {
		return
	}
	if s.workspaces == nil || summary.Status != "ready" {
		return
	}

	entry, refreshDue := s.cachedWorkspaceEnrichment(summary.ID, workspaceEnrichmentFull)
	resp = s.workspaceResponseFromEnrichmentCacheEntry(summary, entry)
	if !refreshDue {
		return
	}
	// Keep the cadences independent: a read that finds only tmux due
	// schedules tmux-only work so it cannot advance the divergence clock.
	if entry != nil {
		if _, divergenceDue := entry.componentsDue(s.now()); !divergenceDue {
			s.scheduleWorkspaceTmuxEnrichment(*summary)
			return
		}
	}
	s.scheduleWorkspaceEnrichment(*summary)
	return
}

func (s *Handler) workspaceResponseFromEnrichmentCacheEntry(
	summary *db.WorkspaceSummary,
	entry *workspaceEnrichmentCacheEntry,
) workspaceResponse {
	resp := toWorkspaceResponse(summary)
	resp.Repo = s.repoRefFromParts(
		summary.Platform, summary.PlatformHost, summary.RepoOwner, summary.RepoName,
	)
	resp.EnrichmentStatus = workspaceEnrichmentPending
	if entry == nil {
		return resp
	}

	applyWorkspaceEnrichmentCacheEntry(&resp, *entry)
	hasResponse := entry.hasDivergence || entry.hasTmux
	switch {
	case entry.errorMessage() != "":
		resp.EnrichmentStatus = workspaceEnrichmentFailed
		errMessage := entry.errorMessage()
		resp.EnrichmentError = &errMessage
	case hasResponse:
		now := s.now()
		divergenceStale := entry.hasDivergence &&
			now.Sub(entry.divergenceRefreshedAt) >= workspaceEnrichmentTTL
		tmuxStale := entry.hasTmux &&
			now.Sub(entry.tmuxRefreshedAt) >= workspaceTmuxEnrichmentTTL
		if divergenceStale || tmuxStale {
			resp.EnrichmentStatus = workspaceEnrichmentStale
		} else if entry.hasDivergence && entry.hasTmux {
			resp.EnrichmentStatus = workspaceEnrichmentFresh
		}
	}
	return resp
}

func (entry workspaceEnrichmentCacheEntry) errorMessage() string {
	messages := make([]string, 0, 2)
	if entry.divergenceError != "" {
		messages = append(messages, entry.divergenceError)
	}
	if entry.tmuxError != "" {
		messages = append(messages, entry.tmuxError)
	}
	return strings.Join(messages, "\n")
}

func (entry workspaceEnrichmentCacheEntry) oldestRefreshedAt() (time.Time, bool) {
	var oldest time.Time
	if entry.hasDivergence {
		oldest = entry.divergenceRefreshedAt
	}
	if entry.hasTmux && (oldest.IsZero() || entry.tmuxRefreshedAt.Before(oldest)) {
		oldest = entry.tmuxRefreshedAt
	}
	return oldest, !oldest.IsZero()
}

func applyCachedWorkspaceTmux(
	resp *workspaceResponse,
	cached workspaceResponse,
) {
	resp.TmuxPaneTitle = cached.TmuxPaneTitle
	resp.TmuxWorking = cached.TmuxWorking
	resp.TmuxActivitySource = cached.TmuxActivitySource
	resp.TmuxLastOutputAt = cached.TmuxLastOutputAt
}

func recordCachedWorkspaceTmux(
	entry workspaceEnrichmentCacheEntry,
	observed workspaceResponse,
	now time.Time,
) workspaceEnrichmentCacheEntry {
	entry.response.TmuxPaneTitle = observed.TmuxPaneTitle
	entry.response.TmuxWorking = observed.TmuxWorking
	entry.response.TmuxActivitySource = observed.TmuxActivitySource

	observedAt := parseWorkspaceTmuxOutputAt(observed.TmuxLastOutputAt)
	if observedAt.IsZero() {
		entry.response.TmuxLastOutputAt = nil
		entry.tmuxObservedOutputAt = time.Time{}
		entry.tmuxPublishedOutputAt = time.Time{}
		entry.tmuxRecencyPublishedAt = time.Time{}
		return entry
	}
	if observedAt.After(entry.tmuxObservedOutputAt) {
		entry.tmuxObservedOutputAt = observedAt
	}

	now = now.UTC()
	publish := entry.tmuxPublishedOutputAt.IsZero()
	if !publish && entry.tmuxObservedOutputAt.After(entry.tmuxPublishedOutputAt) {
		publish = entry.tmuxRecencyPublishedAt.IsZero() ||
			!now.Before(entry.tmuxRecencyPublishedAt.Add(workspaceTmuxRecencyMinInterval))
	}
	if publish {
		entry.tmuxPublishedOutputAt = entry.tmuxObservedOutputAt
		entry.tmuxRecencyPublishedAt = now
	}
	formatted := entry.tmuxPublishedOutputAt.Format(time.RFC3339)
	entry.response.TmuxLastOutputAt = &formatted
	return entry
}

func parseWorkspaceTmuxOutputAt(value *string) time.Time {
	if value == nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func applyWorkspaceEnrichmentCacheEntry(
	resp *workspaceResponse,
	entry workspaceEnrichmentCacheEntry,
) {
	if entry.hasDivergence {
		resp.CommitsAhead = entry.response.CommitsAhead
		resp.CommitsBehind = entry.response.CommitsBehind
		resp.BranchUpstreamMissing = entry.response.BranchUpstreamMissing
		resp.WorktreeDirty = entry.response.WorktreeDirty
	}
	if entry.hasTmux {
		applyCachedWorkspaceTmux(resp, entry.response)
	}
	if refreshedAt, ok := entry.oldestRefreshedAt(); ok {
		formatted := refreshedAt.UTC().Format(time.RFC3339)
		resp.EnrichmentRefreshedAt = &formatted
	}
}

func (s *Handler) cachedWorkspaceEnrichment(
	workspaceID string,
	kind workspaceEnrichmentKind,
) (*workspaceEnrichmentCacheEntry, bool) {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()

	entry, ok := s.workspaceEnrichmentCache[workspaceID]
	if !ok {
		return nil, true
	}
	copy := entry
	tmuxDue, divergenceDue := entry.componentsDue(s.now())
	if kind == workspaceEnrichmentTmux {
		return &copy, tmuxDue
	}
	return &copy, tmuxDue || divergenceDue
}

// componentsDue reports which enrichment components have aged past their
// own cadence: tmux against workspaceTmuxEnrichmentTTL and divergence
// against workspaceEnrichmentTTL. Each uses only its own attempt and
// refresh times.
func (entry workspaceEnrichmentCacheEntry) componentsDue(now time.Time) (tmux, divergence bool) {
	componentDue := func(attemptedAt, refreshedAt time.Time, ttl time.Duration) bool {
		latest := attemptedAt
		if refreshedAt.After(latest) {
			latest = refreshedAt
		}
		return latest.IsZero() || now.Sub(latest) >= ttl
	}
	tmux = componentDue(entry.tmuxAttemptAt, entry.tmuxRefreshedAt, workspaceTmuxEnrichmentTTL)
	divergence = componentDue(
		entry.divergenceAttemptAt, entry.divergenceRefreshedAt, workspaceEnrichmentTTL,
	)
	// An elapsed forced interval makes divergence due regardless of a recent
	// fingerprint-only re-validation, so a read just past the boundary does
	// not queue tmux-only work and delay git by another TTL.
	if entry.hasDivergence && !entry.divergenceProbedAt.IsZero() &&
		now.Sub(entry.divergenceProbedAt) >= workspaceEnrichmentForcedProbeInterval {
		divergence = true
	}
	return tmux, divergence
}

// gitProbeSkippable reports whether a full refresh may re-validate the cached
// divergence from an unchanged git fingerprint instead of spawning git: the
// cache must hold a successful result, the fingerprint must match, and the
// last real probe must be younger than the forced interval.
func (entry workspaceEnrichmentCacheEntry) gitProbeSkippable(fingerprint string, now time.Time) bool {
	return fingerprint != "" &&
		entry.hasDivergence &&
		entry.divergenceError == "" &&
		entry.gitFingerprint == fingerprint &&
		!entry.divergenceProbedAt.IsZero() &&
		now.Sub(entry.divergenceProbedAt) < workspaceEnrichmentForcedProbeInterval
}

func (s *Handler) refreshWorkspaceResponse(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) (resp workspaceResponse) {
	defer s.applyAgentActivity(&resp, summary)
	generation := s.supersedeWorkspaceEnrichment(summary.ID)
	result := s.workspaceResponseWithEnrichment(ctx, summary)
	if summary.Status == "ready" {
		entry, recorded, _ := s.recordWorkspaceEnrichmentResult(
			summary.ID, generation, result,
		)
		resp = s.workspaceResponseAfterEnrichmentAttempt(
			summary, result, entry, recorded,
		)
		return
	}
	resp = result.response
	return
}

func (s *Handler) workspaceResponseAfterEnrichmentAttempt(
	summary *db.WorkspaceSummary,
	result workspaceEnrichmentProbeResult,
	entry workspaceEnrichmentCacheEntry,
	recorded bool,
) workspaceResponse {
	if !recorded {
		return s.workspaceResponseFromEnrichmentCacheEntry(summary, &entry)
	}
	applyWorkspaceEnrichmentCacheEntry(&result.response, entry)
	return result.response
}

func (s *Handler) scheduleWorkspaceEnrichment(summary db.WorkspaceSummary) {
	s.scheduleWorkspaceEnrichmentKind(summary, workspaceEnrichmentFull)
}

func (s *Handler) scheduleWorkspaceTmuxEnrichment(summary db.WorkspaceSummary) {
	s.scheduleWorkspaceEnrichmentKind(summary, workspaceEnrichmentTmux)
}

func (s *Handler) scheduleWorkspaceEnrichmentKind(
	summary db.WorkspaceSummary,
	kind workspaceEnrichmentKind,
) {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	if s.workspaceEnrichmentGenerations == nil {
		s.workspaceEnrichmentGenerations = make(map[string]uint64)
	}
	if _, ok := s.workspaceEnrichmentGenerations[summary.ID]; !ok {
		s.workspaceEnrichmentGenerations[summary.ID] = 0
	}
	generation := s.workspaceEnrichmentGenerations[summary.ID]
	if inFlight, ok := s.workspaceEnrichmentInFlight[summary.ID]; ok &&
		inFlight == generation {
		if s.workspaceEnrichmentFlightKinds[summary.ID] == workspaceEnrichmentFull ||
			kind == workspaceEnrichmentTmux {
			return
		}
	}
	if pending, ok := s.workspaceEnrichmentPending[summary.ID]; ok &&
		pending.generation == generation {
		pending.summary = summary
		if kind == workspaceEnrichmentFull {
			pending.kind = workspaceEnrichmentFull
		}
		s.workspaceEnrichmentPending[summary.ID] = pending
		return
	}
	if s.workspaceEnrichmentPending == nil {
		s.workspaceEnrichmentPending = make(map[string]workspaceEnrichmentJob)
	}
	s.workspaceEnrichmentPending[summary.ID] = workspaceEnrichmentJob{
		summary:    summary,
		generation: generation,
		kind:       kind,
	}
	s.startWorkspaceEnrichmentWorkersLocked()
}

func (s *Handler) startWorkspaceEnrichmentWorkersLocked() {
	pending := len(s.workspaceEnrichmentPending)
	if s.workspaceTmuxPrunePending {
		pending++
	}
	for s.workspaceEnrichmentWorkers < cap(s.workspaceEnrichmentSlots) &&
		s.workspaceEnrichmentWorkers < pending {
		s.workspaceEnrichmentWorkers++
		if !s.runBackground(s.runWorkspaceEnrichmentWorker) {
			s.workspaceEnrichmentWorkers--
			return
		}
	}
}

func (s *Handler) runWorkspaceEnrichmentWorker(ctx context.Context) {
	select {
	case s.workspaceEnrichmentSlots <- struct{}{}:
		defer func() { <-s.workspaceEnrichmentSlots }()
	case <-ctx.Done():
		s.workspaceEnrichmentMu.Lock()
		s.workspaceEnrichmentWorkers--
		s.workspaceEnrichmentMu.Unlock()
		return
	}
	for {
		job, prune, ok := s.nextWorkspaceEnrichmentJob()
		if !ok {
			return
		}
		if prune {
			s.runWorkspaceTmuxPrune(ctx)
			continue
		}
		s.runWorkspaceEnrichmentJob(ctx, job)
	}
}

func (s *Handler) nextWorkspaceEnrichmentJob() (
	workspaceEnrichmentJob,
	bool,
	bool,
) {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	if s.workspaceTmuxPrunePending {
		s.workspaceTmuxPrunePending = false
		s.workspaceTmuxPruneInFlight = true
		return workspaceEnrichmentJob{}, true, true
	}
	for workspaceID, job := range s.workspaceEnrichmentPending {
		if s.workspaceEnrichmentGenerations[workspaceID] != job.generation {
			delete(s.workspaceEnrichmentPending, workspaceID)
			continue
		}
		if _, active := s.workspaceEnrichmentInFlight[workspaceID]; active {
			continue
		}
		delete(s.workspaceEnrichmentPending, workspaceID)
		s.workspaceEnrichmentNextFlight++
		job.flightID = s.workspaceEnrichmentNextFlight
		s.workspaceEnrichmentInFlight[workspaceID] = job.generation
		if s.workspaceEnrichmentFlightKinds == nil {
			s.workspaceEnrichmentFlightKinds = make(map[string]workspaceEnrichmentKind)
		}
		s.workspaceEnrichmentFlightKinds[workspaceID] = job.kind
		if s.workspaceEnrichmentFlightIDs == nil {
			s.workspaceEnrichmentFlightIDs = make(map[string]uint64)
		}
		s.workspaceEnrichmentFlightIDs[workspaceID] = job.flightID
		return job, false, true
	}
	s.workspaceEnrichmentWorkers--
	return workspaceEnrichmentJob{}, false, false
}

func (s *Handler) runWorkspaceEnrichmentJob(
	ctx context.Context,
	job workspaceEnrichmentJob,
) {
	defer s.finishWorkspaceEnrichment(job.summary.ID, job.generation, job.flightID)
	probeCtx, cancel := context.WithTimeout(
		ctx, workspaceEnrichmentRefreshTimeout,
	)
	defer cancel()
	var result workspaceEnrichmentProbeResult
	if job.kind == workspaceEnrichmentTmux {
		result = s.workspaceResponseWithTmuxEnrichment(probeCtx, &job.summary)
	} else {
		result = s.workspaceResponseWithChangeAwareEnrichment(probeCtx, &job.summary)
	}
	result.kind = job.kind
	if _, recorded, changed := s.recordWorkspaceEnrichmentResult(
		job.summary.ID, job.generation, result,
	); recorded && changed {
		s.broadcastWorkspaceStatus(job.summary.ID)
	}
}

func (s *Handler) workspaceEnrichmentGeneration(workspaceID string) uint64 {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	return s.workspaceEnrichmentGenerations[workspaceID]
}

func (s *Handler) invalidateWorkspaceEnrichment(workspaceID string) uint64 {
	return s.advanceWorkspaceEnrichmentGeneration(workspaceID, false)
}

func (s *Handler) supersedeWorkspaceEnrichment(workspaceID string) uint64 {
	return s.advanceWorkspaceEnrichmentGeneration(workspaceID, true)
}

func (s *Handler) advanceWorkspaceEnrichmentGeneration(
	workspaceID string,
	preserveCache bool,
) uint64 {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	if s.workspaceEnrichmentGenerations == nil {
		s.workspaceEnrichmentGenerations = make(map[string]uint64)
	}
	generation := s.workspaceEnrichmentGenerations[workspaceID] + 1
	s.workspaceEnrichmentGenerations[workspaceID] = generation
	if !preserveCache {
		delete(s.workspaceEnrichmentCache, workspaceID)
	}
	return generation
}

func (s *Handler) recordWorkspaceEnrichmentResult(
	workspaceID string,
	generation uint64,
	result workspaceEnrichmentProbeResult,
) (workspaceEnrichmentCacheEntry, bool, bool) {
	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	currentGeneration, ok := s.workspaceEnrichmentGenerations[workspaceID]
	if !ok || currentGeneration != generation {
		return s.workspaceEnrichmentCache[workspaceID], false, false
	}
	now := s.now()
	prior := s.workspaceEnrichmentCache[workspaceID]
	entry := prior
	if result.divergenceComplete {
		entry.response.CommitsAhead = result.response.CommitsAhead
		entry.response.CommitsBehind = result.response.CommitsBehind
		entry.response.BranchUpstreamMissing = result.response.BranchUpstreamMissing
		entry.response.WorktreeDirty = result.response.WorktreeDirty
		entry.hasDivergence = true
		entry.divergenceRefreshedAt = now
		entry.divergenceProbedAt = now
		entry.gitFingerprint = result.gitFingerprint
	} else if result.divergenceUnchanged && prior.hasDivergence {
		entry.divergenceRefreshedAt = now
	}
	if result.kind == workspaceEnrichmentFull && !result.divergenceSkipped {
		entry.divergenceAttemptAt = now
		if result.divergenceErr != nil {
			entry.divergenceError = result.divergenceErr.Error()
		} else if result.divergenceComplete {
			entry.divergenceError = ""
		}
	}
	if result.tmuxComplete {
		entry = recordCachedWorkspaceTmux(entry, result.response, now)
		entry.hasTmux = true
		entry.tmuxRefreshedAt = now
	}
	if !result.tmuxSkipped {
		entry.tmuxAttemptAt = now
	}
	entry.lastAttemptAt = now
	if result.tmuxErr != nil {
		entry.tmuxError = result.tmuxErr.Error()
	} else if result.tmuxComplete {
		entry.tmuxError = ""
	}
	s.workspaceEnrichmentCache[workspaceID] = entry
	return entry, true, workspaceEnrichmentBroadcastWorthy(prior, entry)
}

// workspaceEnrichmentBroadcastWorthy reports whether a recorded enrichment
// result should notify SSE clients. Completion of the first probe (the
// pending → fresh transition clients wait on), a git-state change, and an
// error-state change are notification-worthy. Tmux-activity-only changes are
// not: an active agent moves the activity timestamp on every probe, and
// broadcasting those turned each completion into a client refetch that
// scheduled the next enrichment — a permanent refresh loop. The workspace
// list's own polling still picks activity changes up.
func workspaceEnrichmentBroadcastWorthy(prior, next workspaceEnrichmentCacheEntry) bool {
	if prior.hasDivergence != next.hasDivergence || prior.hasTmux != next.hasTmux {
		return true
	}
	if prior.divergenceError != next.divergenceError ||
		prior.tmuxError != next.tmuxError {
		return true
	}
	return next.hasDivergence &&
		(!intPointerEqual(prior.response.CommitsAhead, next.response.CommitsAhead) ||
			!intPointerEqual(prior.response.CommitsBehind, next.response.CommitsBehind) ||
			!boolPointerEqual(prior.response.BranchUpstreamMissing, next.response.BranchUpstreamMissing) ||
			!boolPointerEqual(prior.response.WorktreeDirty, next.response.WorktreeDirty))
}

func intPointerEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPointerEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Handler) finishWorkspaceEnrichment(
	workspaceID string,
	generation uint64,
	flightID uint64,
) {
	s.workspaceEnrichmentMu.Lock()
	if s.workspaceEnrichmentInFlight[workspaceID] == generation &&
		s.workspaceEnrichmentFlightIDs[workspaceID] == flightID {
		delete(s.workspaceEnrichmentInFlight, workspaceID)
		delete(s.workspaceEnrichmentFlightKinds, workspaceID)
		delete(s.workspaceEnrichmentFlightIDs, workspaceID)
	}
	s.workspaceEnrichmentMu.Unlock()
}

func (s *Handler) trimWorkspaceEnrichmentCache(
	summaries []db.WorkspaceSummary,
) {
	valid := make(map[string]struct{}, len(summaries))
	for i := range summaries {
		valid[summaries[i].ID] = struct{}{}
	}

	s.workspaceEnrichmentMu.Lock()
	defer s.workspaceEnrichmentMu.Unlock()
	for workspaceID := range s.workspaceEnrichmentCache {
		if _, ok := valid[workspaceID]; !ok {
			delete(s.workspaceEnrichmentCache, workspaceID)
			delete(s.workspaceEnrichmentGenerations, workspaceID)
			delete(s.workspaceEnrichmentPending, workspaceID)
		}
	}
	for workspaceID := range s.workspaceEnrichmentPending {
		if _, ok := valid[workspaceID]; !ok {
			delete(s.workspaceEnrichmentPending, workspaceID)
			delete(s.workspaceEnrichmentGenerations, workspaceID)
		}
	}
	for workspaceID := range s.workspaceEnrichmentGenerations {
		if _, ok := valid[workspaceID]; !ok {
			delete(s.workspaceEnrichmentGenerations, workspaceID)
		}
	}
}

func (s *Handler) scheduleWorkspaceTmuxPrune() {
	if s.workspaces == nil || s.workspaceEnrichmentDisabled {
		return
	}
	now := s.now()
	s.workspaceEnrichmentMu.Lock()
	if s.workspaceTmuxPrunePending || s.workspaceTmuxPruneInFlight ||
		(!s.workspaceTmuxPrunedAt.IsZero() &&
			now.Sub(s.workspaceTmuxPrunedAt) < workspaceTmuxPruneInterval) {
		s.workspaceEnrichmentMu.Unlock()
		return
	}
	s.workspaceTmuxPrunePending = true
	s.workspaceTmuxPrunedAt = now
	s.startWorkspaceEnrichmentWorkersLocked()
	s.workspaceEnrichmentMu.Unlock()
}

func (s *Handler) runWorkspaceTmuxPrune(ctx context.Context) {
	defer func() {
		s.workspaceEnrichmentMu.Lock()
		s.workspaceTmuxPruneInFlight = false
		s.workspaceEnrichmentMu.Unlock()
	}()
	pruneCtx, cancel := context.WithTimeout(
		ctx, workspaceEnrichmentRefreshTimeout,
	)
	defer cancel()
	pruned, err := s.workspaces.PruneMissingTmuxSessions(pruneCtx)
	if err != nil {
		slog.Debug("prune missing tmux sessions", "err", err)
	}
	if !pruned {
		return
	}
	// Rows may have been deleted even when the prune also failed part way,
	// so their reports are reconciled whenever anything was pruned, on a
	// budget of their own rather than the prune's remaining one.
	reconcileCtx, cancelReconcile := context.WithTimeout(
		ctx, workspaceEnrichmentRefreshTimeout,
	)
	defer cancelReconcile()
	s.reconcileAgentActivityReports(reconcileCtx)
	// Broadcast only when the pass changed state. The unconditional
	// broadcast made every open view refetch its workspace every prune
	// interval even though nothing happened.
	s.hub.Broadcast(Event{Type: "workspace_status", Data: map[string]string{}})
}
