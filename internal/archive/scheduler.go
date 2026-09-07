package archive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

type WorkPriority int

const (
	PriorityNormalIndex WorkPriority = iota
	PriorityNotificationRefresh
	PriorityActiveDetail
	PriorityFullArchive
	PriorityDiscoveryInventory
)

type Scheduler struct{}

func NewScheduler() *Scheduler { return &Scheduler{} }

// Run executes work for every provider-host group concurrently and reports
// whether any group attempted provider work.
func (s *Scheduler) Run(
	ctx context.Context,
	groups map[string][]resolvedRepository,
	work func(context.Context, []resolvedRepository) (bool, error),
) (bool, error) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	errCh := make(chan error, len(keys))
	var worked atomic.Bool
	var wg sync.WaitGroup
	for _, key := range keys {
		key, repos := key, groups[key]
		wg.Go(func() {
			handled, err := work(ctx, repos)
			if handled {
				worked.Store(true)
			}
			if err != nil {
				errCh <- fmt.Errorf("archive provider worker %s: %w", key, err)
			}
		})
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return worked.Load(), errors.Join(errs...)
}

var errAdmissionDeferred = errors.New("archive request deferred by admission")

// errRequestPreempted marks an admitted provider request that live work
// canceled mid-flight. It is an admission deferral for retry purposes, but the
// provider was reached, so the pass counts as work.
var errRequestPreempted = fmt.Errorf("archive request preempted by live work: %w", errAdmissionDeferred)

type featureDeferredError struct {
	FeatureDeferral
	providerAttempted bool
}

func (e *featureDeferredError) Error() string { return e.Detail }
func (e *featureDeferredError) Unwrap() error { return errAdmissionDeferred }

func featureDeferredBeforeProvider(err error) bool {
	var deferred *featureDeferredError
	return errors.As(err, &deferred) && deferred != nil && !deferred.providerAttempted
}

// RunEligible performs one worker pass. Tests and one-shot callers use it;
// the worker loop calls RunPass to learn whether the pass attempted work.
func (s *Service) RunEligible(ctx context.Context) error {
	_, err := s.RunPass(ctx)
	return err
}

// RunPass performs one worker pass over every configured repository grouped by
// provider host and reports whether any provider work was attempted, so the
// loop can back off while nothing is eligible. A pass with unchanged
// configuration performs no repository resolution queries.
func (s *Service) RunPass(ctx context.Context) (bool, error) {
	resolved, err := s.workerRepositories(ctx)
	if err != nil {
		return false, err
	}
	groups := make(map[string][]resolvedRepository)
	for _, repo := range resolved {
		key := string(repo.Ref.Platform) + "\x00" + repo.Ref.Host
		groups[key] = append(groups[key], repo)
	}
	return s.scheduler.Run(ctx, groups, s.runProviderHostWork)
}

func (s *Service) runProviderHostWork(ctx context.Context, repos []resolvedRepository) (bool, error) {
	states, err := s.db.ListArchiveRepoStates(ctx, resolvedRepoIDs(repos))
	if err != nil {
		return false, err
	}
	stateByID := make(map[int64]db.ArchiveRepoState, len(states))
	for _, state := range states {
		stateByID[state.RepoID] = state
	}

	// Give both independent inventories their first bounded page before
	// hydration so a large issue history cannot starve merge-request discovery.
	for _, repo := range repos {
		state := stateByID[repo.ID]
		if state.OperatorState != db.ArchiveOperatorStateActive ||
			state.CollectionMode != db.ArchiveCollectionModeFull || archiveRepoDeferred(state, s.now()) {
			continue
		}
		if archiveScanNotStarted(state.IssueInventory) {
			err := s.inventoryPage(ctx, repo, state, db.ArchiveItemTypeIssue)
			if !featureDeferredBeforeProvider(err) {
				return s.finishWork(err)
			}
		}
		if archiveScanNotStarted(state.MergeRequestInventory) {
			err := s.inventoryPage(ctx, repo, state, db.ArchiveItemTypeMergeRequest)
			if featureDeferredBeforeProvider(err) {
				continue
			}
			return s.finishWork(err)
		}
	}

	// Once a maintenance scan has established its durable boundary, finish
	// both inventory streams before newly observed equality-boundary work can
	// enter hydration. Otherwise an overlapped item can consume the next
	// admitted request and indefinitely delay a persisted inventory cursor.
	// The boundary only holds priority while it can actually advance: a
	// boundary whose streams are all blocked stays durable for reporting but
	// must not consume every poll and starve hydration and other
	// repositories on the same provider host.
	for _, repo := range repos {
		state := stateByID[repo.ID]
		if state.CollectionMode == db.ArchiveCollectionModeFull &&
			state.OperatorState == db.ArchiveOperatorStateActive &&
			state.PromptScanStartedAt != nil && !archiveRepoDeferred(state, s.now()) &&
			maintenanceBoundaryActionable(state) {
			err := s.promptMaintenance(ctx, repo, state)
			if featureDeferredBeforeProvider(err) {
				continue
			}
			return s.finishWork(err)
		}
	}

	fullIDs := make([]int64, 0, len(repos))
	for _, repo := range repos {
		state := stateByID[repo.ID]
		if state.CollectionMode == db.ArchiveCollectionModeFull &&
			state.OperatorState == db.ArchiveOperatorStateActive && !archiveRepoDeferred(state, s.now()) {
			fullIDs = append(fullIDs, repo.ID)
		}
	}
	if handled, err := s.runNextHydrationWork(ctx, repos, fullIDs); handled || err != nil {
		return handled, err
	}

	if handled, err := s.runNextInventoryWork(
		ctx, repos, stateByID, db.ArchiveCollectionModeFull,
	); handled || err != nil {
		return handled, err
	}
	for _, repo := range repos {
		state := stateByID[repo.ID]
		if state.CollectionMode != db.ArchiveCollectionModeFull ||
			state.OperatorState != db.ArchiveOperatorStateActive || archiveRepoDeferred(state, s.now()) {
			continue
		}
		if promptMaintenanceDue(state, s.now(), s.maintenanceInterval) && maintenanceBoundaryActionable(state) {
			err := s.promptMaintenance(ctx, repo, state)
			if featureDeferredBeforeProvider(err) {
				continue
			}
			return s.finishWork(err)
		}
	}
	if handled, err := s.runNextInventoryWork(
		ctx, repos, stateByID, db.ArchiveCollectionModeDiscovery,
	); handled || err != nil {
		return handled, err
	}
	return false, nil
}

type archiveInventoryScope struct {
	repoID   int64
	itemType db.ArchiveItemType
}

func (s *Service) runNextHydrationWork(
	ctx context.Context,
	repos []resolvedRepository,
	repoIDs []int64,
) (bool, error) {
	var excluded []db.ArchiveItemScope
	for {
		item, err := s.db.ClaimArchiveItem(ctx, db.ClaimArchiveItemOpts{
			RepoIDs: repoIDs, Now: s.now(), ExcludedScopes: excluded,
		})
		if err != nil {
			return false, err
		}
		if item == nil {
			return false, nil
		}
		repo := resolvedRepoByID(repos, item.RepoID)
		err = s.hydrateItem(ctx, repo, *item)
		if featureDeferredBeforeProvider(err) {
			excluded = append(excluded, db.ArchiveItemScope{
				RepoID: item.RepoID, ItemType: item.ItemType,
			})
			continue
		}
		return s.finishWork(err)
	}
}

func (s *Service) runNextInventoryWork(
	ctx context.Context,
	repos []resolvedRepository,
	states map[int64]db.ArchiveRepoState,
	mode db.ArchiveCollectionMode,
) (bool, error) {
	skipped := make(map[archiveInventoryScope]struct{})
	for {
		repo, state, itemType, ok := nextInventoryWork(
			repos, states, mode, s.now(), skipped,
		)
		if !ok {
			return false, nil
		}
		err := s.inventoryPage(ctx, repo, state, itemType)
		if featureDeferredBeforeProvider(err) {
			skipped[archiveInventoryScope{repoID: repo.ID, itemType: itemType}] = struct{}{}
			continue
		}
		return s.finishWork(err)
	}
}

// finishWork converts a work unit's outcome into the pass result. A unit that
// reached the provider is work, even when the provider answered with a feature
// deferral or live work preempted the request: other repositories on the host
// may be eligible right now, so the loop keeps its pacing interval. Only an
// admission denial before any provider request is idle: nothing was attempted
// and the deferral names when to look again, so the worker may back off.
func (s *Service) finishWork(err error) (bool, error) {
	var deferred *featureDeferredError
	switch {
	case errors.As(err, &deferred):
		return deferred.providerAttempted, nil
	case errors.Is(err, errRequestPreempted):
		return true, nil
	case errors.Is(err, errAdmissionDeferred):
		return false, nil
	}
	return true, err
}

// archiveScanNotStarted reports a scan generation that has not committed any
// page yet: eligible for its bootstrap first page.
func archiveScanNotStarted(scan db.ArchiveScanState) bool {
	return scan.Status == db.ArchiveScanPending && scan.PageCount == 0
}

// archiveScanEligible reports a scan that still has pages to fetch and is not
// durably blocked.
func archiveScanEligible(scan db.ArchiveScanState) bool {
	return !scan.Complete() && !scan.Blocked()
}

// maintenanceBoundaryActionable reports whether a maintenance pass could make
// progress right now. Before a boundary exists one can always be started.
// With a durable boundary, progress requires at least one stream that can
// still advance, or both streams complete (the watermark advance itself is
// the remaining work). A boundary whose outstanding streams are all blocked
// is not actionable: it stays durable for reporting, and only an explicit
// reset revives it.
func maintenanceBoundaryActionable(state db.ArchiveRepoState) bool {
	if state.PromptScanStartedAt == nil {
		return true
	}
	if archiveScanEligible(state.MaintenanceIssues) || archiveScanEligible(state.MaintenanceMergeRequests) {
		return true
	}
	return state.MaintenanceIssues.Complete() && state.MaintenanceMergeRequests.Complete()
}

func nextInventoryWork(
	repos []resolvedRepository,
	states map[int64]db.ArchiveRepoState,
	mode db.ArchiveCollectionMode,
	now time.Time,
	skipped map[archiveInventoryScope]struct{},
) (resolvedRepository, db.ArchiveRepoState, db.ArchiveItemType, bool) {
	for _, repo := range repos {
		state := states[repo.ID]
		if state.CollectionMode != mode || state.OperatorState != db.ArchiveOperatorStateActive || archiveRepoDeferred(state, now) {
			continue
		}
		if _, skip := skipped[archiveInventoryScope{repoID: repo.ID, itemType: db.ArchiveItemTypeIssue}]; !skip && archiveScanEligible(state.IssueInventory) {
			return repo, state, db.ArchiveItemTypeIssue, true
		}
		if _, skip := skipped[archiveInventoryScope{repoID: repo.ID, itemType: db.ArchiveItemTypeMergeRequest}]; !skip && archiveScanEligible(state.MergeRequestInventory) {
			return repo, state, db.ArchiveItemTypeMergeRequest, true
		}
	}
	return resolvedRepository{}, db.ArchiveRepoState{}, "", false
}

func archiveRepoDeferred(state db.ArchiveRepoState, now time.Time) bool {
	if state.NextRetryAt != nil && state.NextRetryAt.After(now) {
		return true
	}
	if state.LastErrorCode == nil {
		return false
	}
	return *state.LastErrorCode == string(db.ArchiveErrorCodeAuthentication) ||
		*state.LastErrorCode == string(db.ArchiveErrorCodeRepoBlocked)
}

func resolvedRepoByID(repos []resolvedRepository, repoID int64) resolvedRepository {
	for _, repo := range repos {
		if repo.ID == repoID {
			return repo
		}
	}
	return resolvedRepository{}
}

func (s *Service) admit(
	ctx context.Context,
	repo resolvedRepository,
	itemType db.ArchiveItemType,
	cost int,
) (context.Context, AdmissionComplete, error) {
	if err := s.db.ClearArchiveRepositoryError(ctx, repo.ID, s.now()); err != nil {
		return nil, nil, err
	}
	if s.admission == nil {
		return ctx, func(error, bool) *FeatureDeferral { return nil }, nil
	}
	result, err := s.admission.Admit(ctx, repo.Ref, itemType, cost)
	if err != nil {
		return nil, nil, err
	}
	if result.FeatureDeferred != nil {
		return nil, nil, &featureDeferredError{FeatureDeferral: *result.FeatureDeferred}
	}
	if !result.Allowed {
		if result.RetryAt == nil {
			return nil, nil, errors.New("archive admission denied without retry time")
		}
		if err := s.db.DeferArchiveRepository(ctx, repo.ID, *result.RetryAt, result.Detail, s.now()); err != nil {
			return nil, nil, err
		}
		return nil, nil, errAdmissionDeferred
	}
	requestCtx := ctx
	if result.Context != nil {
		requestCtx = result.Context
	}
	complete := result.Complete
	if complete == nil {
		complete = func(error, bool) *FeatureDeferral { return nil }
	}
	return requestCtx, complete, nil
}

type fixedClock struct{ value time.Time }

func (c fixedClock) Now() time.Time { return c.value }

// archiveAttemptCost converts a declared logical request count into the
// admission cost archive work must reserve. Archive admission cost models
// worst-case wire attempts, not logical requests: a 401-invalidate-retry
// spends two wire attempts for one logical request, so every admit call
// must declare double its logical cost or it can overspend the admitted
// ceiling and the live floor it protects.
func archiveAttemptCost(logical int) int {
	return logical * 2
}

func archiveFeatureReadAttemptCost(kind platform.Kind, itemType db.ArchiveItemType) int {
	logicalRequests := 1
	if kind != platform.KindGitHub || itemType == db.ArchiveItemTypeMergeRequest {
		logicalRequests++ // repository metadata confirmation after a candidate feature error
	}
	return archiveAttemptCost(logicalRequests)
}

// archivePreempted reports whether a provider read failed because live
// work preempted the admitted request context (canceled between admit and
// release), not because the caller itself is shutting down. Preemption is
// not a failure: the caller must record nothing and leave the work
// immediately claimable. requestCtx must be sampled before release() is
// called, since release always cancels its context and would otherwise
// make every request look preempted.
func archivePreempted(outerCtx, requestCtx context.Context) bool {
	return requestCtx.Err() != nil && outerCtx.Err() == nil
}
