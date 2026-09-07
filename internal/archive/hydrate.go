package archive

import (
	"context"
	"errors"
	"fmt"
	"go.kenn.io/forge/internal/platformdb"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

// hydrateItem runs the existing full item sync under archive admission. The
// syncer remains the sole owner of provider reads, normalization, and domain
// persistence; archive records only scheduling progress and outcomes.
func (s *Service) hydrateItem(
	ctx context.Context,
	repo resolvedRepository,
	work db.ArchiveItemWork,
) error {
	if s.items == nil {
		return errors.New("archive item syncer is required")
	}
	requestCtx, complete, err := s.admit(
		ctx, repo, work.ItemType,
		s.items.ArchiveItemSyncCost(repo.Ref.Platform, work.ItemType),
	)
	if err != nil {
		return err
	}
	syncResult, syncErr := s.items.SyncArchiveItem(
		requestCtx, repo.Ref, work.ItemType, work.ItemNumber,
	)
	preempted := archivePreempted(ctx, requestCtx)
	deferred := complete(syncErr, syncResult.ProviderAttempted)
	if preempted {
		return errRequestPreempted
	}
	if deferred != nil {
		return &featureDeferredError{FeatureDeferral: *deferred, providerAttempted: true}
	}
	commit := db.ArchiveItemSyncCommit{
		RepoID: work.RepoID, ItemType: work.ItemType, ItemNumber: work.ItemNumber,
		ScanGeneration: work.ScanGeneration, Now: s.now(),
		MergeRequestEvidence: syncResult.MergeRequestEvidence,
	}
	if syncErr == nil {
		commit.Outcome = db.ArchiveLookupPresent
		if err := s.db.CommitArchiveItemSync(ctx, commit); err != nil {
			if errors.Is(err, db.ErrArchiveItemEvidenceChanged) {
				return s.recordItemSyncFailure(ctx, commit, work.AttemptCount, err)
			}
			return err
		}
		if finalizer, ok := s.items.(ItemSyncFinalizer); ok {
			finalizer.FinalizeArchiveItemSync(
				ctx, work.RepoID, work.ItemType, work.ItemNumber,
			)
		}
		return nil
	}

	outcome, destination, terminal := archiveTerminalSyncOutcome(syncErr)
	if terminal {
		commit.Outcome = outcome
		commit.ErrorCode = archiveSyncErrorCode(syncErr)
		commit.ErrorDetail = syncErr.Error()
		if destination != nil {
			dbDestination := platformdb.DBRepoIdentity(*destination)
			commit.Destination = &dbDestination
		}
		return s.db.CommitArchiveItemSync(ctx, commit)
	}
	return s.recordItemSyncFailure(ctx, commit, work.AttemptCount, syncErr)
}

func archiveTerminalSyncOutcome(
	err error,
) (db.ArchiveLookupOutcome, *platform.RepoRef, bool) {
	if errors.Is(err, platform.ErrLookupInaccessible) {
		return db.ArchiveLookupInaccessible, nil, true
	}
	if !errors.Is(err, platform.ErrLookupNotPresent) {
		return "", nil, false
	}
	var platformErr *platform.Error
	if !errors.As(err, &platformErr) {
		return "", nil, false
	}
	switch platformErr.Code {
	case platform.ErrCodeNotFound:
		if platformErr.Destination != nil {
			return db.ArchiveLookupMoved, platformErr.Destination, true
		}
		return db.ArchiveLookupRemoved, nil, true
	default:
		return "", nil, false
	}
}

func archiveSyncErrorCode(err error) string {
	if platformErr, ok := errors.AsType[*platform.Error](err); ok {
		return string(platformErr.Code)
	}
	return string(db.ArchiveErrorCodeTransient)
}

func (s *Service) recordItemSyncFailure(
	ctx context.Context,
	commit db.ArchiveItemSyncCommit,
	attempt int,
	cause error,
) error {
	decision := s.retries.Classify(cause, attempt, s.now())
	if decision.Code == "" {
		decision.Code = db.ArchiveErrorCodeTransient
	}
	commit.ErrorDetail = cause.Error()
	repositoryFailure := decision.Code == db.ArchiveErrorCodeAuthentication ||
		decision.Code == db.ArchiveErrorCodeRepoBlocked
	if err := s.db.FailArchiveItemSync(
		ctx, commit, decision.Code, decision.RetryAt, repositoryFailure,
	); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("sync archive %s %d: %w", commit.ItemType, commit.ItemNumber, cause)
}
