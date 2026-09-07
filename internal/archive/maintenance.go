package archive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

// promptMaintenance runs one maintenance scan: a fixed durable boundary, then
// both updated-item streams page by page through the canonical readers with
// StateAll ordered by update time, watermark advanced only after both streams
// reach explicit end-of-pagination.
func (s *Service) promptMaintenance(
	ctx context.Context,
	repo resolvedRepository,
	state db.ArchiveRepoState,
) error {
	since := state.MaintenanceWatermark
	if since == nil {
		since = state.InitialStartedAt
	}
	if since == nil {
		return errors.New("run archive prompt maintenance: initial completion time is missing")
	}
	state, err := s.db.BeginArchivePromptMaintenance(ctx, repo.ID, since.UTC(), s.now())
	if err != nil {
		return err
	}
	if state.PromptSince == nil || state.PromptScanStartedAt == nil {
		return errors.New("run archive prompt maintenance: durable scan boundary is missing")
	}
	issueScan := state.MaintenanceIssues
	var deferred error
	providerWorkCompleted := false
	if !issueScan.Complete() && !issueScan.Blocked() {
		if err := s.promptPages(ctx, repo, db.ArchiveItemTypeIssue, state.PromptSince.UTC(), issueScan); err != nil {
			if !featureDeferredBeforeProvider(err) {
				return err
			}
			deferred = err
		} else {
			providerWorkCompleted = true
		}
	}
	mrScan := state.MaintenanceMergeRequests
	if !mrScan.Complete() && !mrScan.Blocked() {
		if err := s.promptPages(ctx, repo, db.ArchiveItemTypeMergeRequest, state.PromptSince.UTC(), mrScan); err != nil {
			if !featureDeferredBeforeProvider(err) {
				return err
			}
			deferred = err
		} else {
			providerWorkCompleted = true
		}
	}
	if deferred != nil && !providerWorkCompleted {
		return deferred
	}
	refreshed, err := s.db.ListArchiveRepoStates(ctx, []int64{repo.ID})
	if err != nil {
		return err
	}
	if len(refreshed) != 1 ||
		!refreshed[0].MaintenanceIssues.Complete() ||
		!refreshed[0].MaintenanceMergeRequests.Complete() {
		// A blocked stream leaves the boundary in place: the watermark must
		// not advance past pages that were never enumerated.
		return nil
	}
	return s.db.CompleteArchivePromptMaintenance(ctx, repo.ID, state.PromptScanStartedAt.UTC(), s.now())
}

// promptPages drains one updated-item stream from its durable cursor. Every
// page re-acquires admission and commits through the same inventory commit as
// historical pages, with prompt refresh semantics.
func (s *Service) promptPages(
	ctx context.Context,
	repo resolvedRepository,
	itemType db.ArchiveItemType,
	since time.Time,
	scan db.ArchiveScanState,
) error {
	kind := db.ArchiveScanMaintenanceIssues
	if itemType == db.ArchiveItemTypeMergeRequest {
		kind = db.ArchiveScanMaintenanceMergeRequests
	}
	cursor := scan.Cursor()
	for {
		commit := db.ArchiveInventoryCommit{
			RepoID: repo.ID, ItemType: itemType,
			RefreshReason:  db.ArchiveRefreshReasonPrompt,
			ScanGeneration: scan.Generation, InputCursor: cursor,
			Now: s.now(),
		}
		requestCtx, complete, err := s.admit(
			withInventoryProbe(ctx), repo, itemType,
			archiveFeatureReadAttemptCost(repo.Ref.Platform, itemType),
		)
		if err != nil {
			if errors.Is(err, errAdmissionDeferred) {
				return err
			}
			return s.recordInventoryFailure(ctx, repo.ID, err)
		}
		query := platform.ItemPageQuery{
			Order:        platform.ItemOrderUpdated,
			UpdatedSince: &since, Cursor: cursor,
		}
		featureAvailable := false
		switch itemType {
		case db.ArchiveItemTypeIssue:
			page, err := repo.Issues.ListIssuesPage(requestCtx, repo.Ref, query)
			preempted := archivePreempted(ctx, requestCtx)
			featureDisabled := archiveInventoryFeatureDisabled(err, itemType)
			deferred := complete(err, true)
			if featureDisabled {
				commit.Exhausted = true
				break
			}
			if deferred != nil {
				return &featureDeferredError{FeatureDeferral: *deferred, providerAttempted: true}
			}
			if err != nil {
				if preempted {
					return errRequestPreempted
				}
				return s.recordScanFailure(ctx, repo, kind, scan.Generation, fmt.Errorf(
					"list updated issues for %s: %w", archiveRepoIdentityKey(repo.Ref), err,
				))
			}
			featureAvailable = true
			commit.NextCursor, commit.Exhausted = page.NextCursor, page.Exhausted
			commit.Items = make([]db.ArchiveInventoryItem, 0, len(page.Items))
			for _, item := range page.Items {
				commit.Items = append(commit.Items, archiveInventoryIssue(item))
			}
		case db.ArchiveItemTypeMergeRequest:
			page, err := repo.MergeRequests.ListMergeRequestsPage(requestCtx, repo.Ref, query)
			preempted := archivePreempted(ctx, requestCtx)
			featureDisabled := archiveInventoryFeatureDisabled(err, itemType)
			deferred := complete(err, true)
			if featureDisabled {
				commit.Exhausted = true
				break
			}
			if deferred != nil {
				return &featureDeferredError{FeatureDeferral: *deferred, providerAttempted: true}
			}
			if err != nil {
				if preempted {
					return errRequestPreempted
				}
				return s.recordScanFailure(ctx, repo, kind, scan.Generation, fmt.Errorf(
					"list updated merge requests for %s: %w", archiveRepoIdentityKey(repo.Ref), err,
				))
			}
			featureAvailable = true
			commit.NextCursor, commit.Exhausted = page.NextCursor, page.Exhausted
			commit.Items = make([]db.ArchiveInventoryItem, 0, len(page.Items))
			for _, item := range page.Items {
				commit.Items = append(commit.Items, archiveInventoryMergeRequest(item))
			}
		default:
			invalidErr := fmt.Errorf("archive maintenance: invalid item type %q", itemType)
			complete(invalidErr, false)
			return invalidErr
		}
		commit.InventoryAvailable = featureAvailable &&
			archiveInventorySupported(repo, itemType)
		halted, err := s.commitInventoryPage(ctx, commit)
		if err != nil {
			return err
		}
		if halted || commit.Exhausted {
			return nil
		}
		cursor = commit.NextCursor
	}
}

func promptMaintenanceDue(state db.ArchiveRepoState, now time.Time, interval time.Duration) bool {
	if state.InitialCompletedAt == nil {
		return false
	}
	if state.MaintenanceWatermark == nil || state.MaintenanceSucceededAt == nil {
		return true
	}
	return !state.MaintenanceSucceededAt.Add(interval).After(now)
}
