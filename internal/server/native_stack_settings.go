package server

import (
	"context"
	"log/slog"

	"go.kenn.io/forge/internal/stacks"
	"go.kenn.io/forge/platform"
)

// swapGitHubNativeStackPreferenceLocked publishes the preference while the
// caller still holds cfgMu, so the order the syncer observes matches the order
// the config was persisted in. Applying it after the unlock let two concurrent
// writers with opposite values land out of order and leave the runtime
// preference disagreeing with the saved config.
//
// It returns the previous value. Reconciliation keys on that transition rather
// than on a config snapshot read earlier: concurrent writers each hold their own
// snapshot, and reconciling from those could run the restore twice or skip it.
func (s *Server) swapGitHubNativeStackPreferenceLocked(enabled bool) bool {
	if s.syncer == nil {
		return false
	}
	return s.syncer.SetPreferGitHubNativeStacks(enabled)
}

// reconcileGitHubNativeStackProjection restores branch-derived projections after
// the preference was swapped off. It runs after cfgMu is released and must not
// borrow the request context: the setting is already persisted and the syncer
// already switched, so a client that disconnects mid-request would otherwise
// leave native ordering in place until some later sync happened to re-detect.
func (s *Server) reconcileGitHubNativeStackProjection(previous, enabled bool) {
	if s.syncer == nil || !previous || enabled {
		return
	}
	s.restoreBranchDerivedStackProjections()
}

// restoreBranchDerivedStackProjections re-derives every stored GitHub
// repository's stacks from branch relationships. Callers must have established
// that the preview is off; the projection lock then keeps that decision stable
// for the whole sweep.
func (s *Server) restoreBranchDerivedStackProjections() {
	if s.syncer == nil {
		return
	}
	ctx := s.bgCtx

	// Serialize with the sync completion hook: an in-flight sync that captured
	// the preview as enabled must not write native ordering after this
	// reconciliation restores branch inference.
	reconciled := false
	s.syncer.RunUnderStackProjection(func() {
		// A later enable may have already landed and projected while this
		// reconciliation waited for the lock. Replaying the older disable would
		// overwrite the current preference's projection with branch inference.
		if s.syncer.PrefersGitHubNativeStacks() {
			return
		}
		for _, repoID := range s.nativeStackProjectionRepoIDs(ctx) {
			if err := stacks.RunDetection(ctx, s.db, repoID); err != nil {
				slog.Warn("reconcile stacks after disabling github native metadata",
					"repo_id", repoID, "err", err)
				continue
			}
			reconciled = true
		}
	})
	if reconciled {
		s.hub.Broadcast(Event{Type: "data_changed", Data: struct{}{}})
	}
}

// nativeStackProjectionRepoIDs returns every stored GitHub repository. The
// preview may have written native ordering into any of them, and the evidence
// that it did is not reliable to scan for: a repository can be dropped from
// config so no sync revisits it, and its cached native rows can be deleted
// independently of the projection they produced. Re-deriving branch inference
// for a repository the preview never touched is a no-op, so reconciliation
// covers the whole set rather than guessing which rows are native.
func (s *Server) nativeStackProjectionRepoIDs(ctx context.Context) []int64 {
	repos, err := s.db.ListRepos(ctx)
	if err != nil {
		slog.Warn("list repos to reconcile after disabling github native metadata",
			"err", err)
		return nil
	}
	repoIDs := make([]int64, 0, len(repos))
	for _, repo := range repos {
		kind, err := platform.NormalizeKind(repo.Platform)
		if err != nil || kind != platform.KindGitHub {
			continue
		}
		repoIDs = append(repoIDs, repo.ID)
	}
	return repoIDs
}
