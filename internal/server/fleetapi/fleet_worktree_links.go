package fleetapi

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/fleet"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
)

// worktreeLinkRecomputeMu serializes every branch-match recompute — the
// sync-completed hook and mutation-triggered recomputes alike. The recompute
// reads refs and merge requests, then replaces the whole link table; without
// serialization an older recompute's replace could land after (and clobber) a
// newer one's, leaving stale or missing PR overlays until the next recompute.
var worktreeLinkRecomputeMu sync.Mutex

// WatchedMRSetter receives the set of merge requests the branch-match recompute
// wants synced on the fast interval. *github.Syncer satisfies it.
type WatchedMRSetter interface {
	SetWatchedMRs([]ghclient.WatchedMR)
}

// WorktreeLinksSyncHook returns a Syncer.SetOnSyncCompleted callback that
// recomputes the worktree-to-merge-request links after each sync pass. When the
// matched set changed, onRecomputed fires (the hub broadcasts a
// worktree_links_changed refetch hint). The hook always chains to next, and a
// canceled ctx (terminal shutdown) skips the recompute while still chaining.
func WorktreeLinksSyncHook(
	ctx context.Context,
	database *db.DB,
	watcher WatchedMRSetter,
	onRecomputed func(),
	next func([]ghclient.RepoSyncResult),
) func([]ghclient.RepoSyncResult) {
	return func(results []ghclient.RepoSyncResult) {
		defer func() {
			if next != nil {
				next(results)
			}
		}()
		if ctx.Err() != nil {
			return
		}
		changed, err := recomputeWorktreeLinks(ctx, database, watcher, time.Now().UTC())
		if err != nil {
			slog.Error("worktree link recompute failed", "err", err)
			return
		}
		if changed && onRecomputed != nil {
			onRecomputed()
		}
	}
}

// recomputeWorktreeLinksNow re-derives the branch-match links immediately after
// a worktree or project mutation so a new worktree's PR overlay appears without
// waiting for the next sync. It fires OnWorktreeLinksRecomputed when the link
// set changed. A nil syncer (no watched-MR setter) skips the recompute.
func (s *Handler) recomputeWorktreeLinksNow(ctx context.Context) {
	if s.syncer == nil {
		return
	}
	changed, err := recomputeWorktreeLinks(ctx, s.db, s.syncer, time.Now().UTC())
	if err != nil {
		slog.Error("worktree link recompute after mutation failed", "err", err)
		return
	}
	if changed {
		s.notifyWorktreeLinksChanged()
	}
}

// recomputeWorktreeLinks rebuilds the durable worktree-to-merge-request links
// from the current registry and synced merge requests by matching each
// registered worktree's branch to an open merge request's head branch in the
// same repo. It always re-applies the derived watched-MR set (cheap, in-memory,
// and lost across a syncer restart) but rewrites the link table and reports
// changed=true only when the matched set differs from what is persisted, so an
// unchanged sync neither rewrites rows nor triggers a snapshot refresh.
func recomputeWorktreeLinks(
	ctx context.Context, database *db.DB, watcher WatchedMRSetter, now time.Time,
) (bool, error) {
	worktreeLinkRecomputeMu.Lock()
	defer worktreeLinkRecomputeMu.Unlock()
	refs, err := database.ListWorktreesForBranchMatch(ctx)
	if err != nil {
		return false, fmt.Errorf("list worktrees for branch match: %w", err)
	}
	openMRs, err := database.ListMergeRequests(ctx, db.ListMergeRequestsOpts{State: "open"})
	if err != nil {
		return false, fmt.Errorf("list open merge requests: %w", err)
	}

	mrByBranch := make(map[string]db.MergeRequest, len(openMRs))
	for _, mr := range openMRs {
		key := repoBranchKey(mr.RepoID, mr.HeadBranch)
		if _, ok := mrByBranch[key]; !ok {
			// openMRs are ordered last_activity DESC, so the first (most
			// recently active) open MR on a branch wins any rare collision.
			mrByBranch[key] = mr
		}
	}

	var links []db.WorktreeLink
	watched := newWatchedSet()
	for _, ref := range refs {
		mr, ok := mrByBranch[repoBranchKey(ref.RepoID, ref.Branch)]
		if !ok {
			continue
		}
		links = append(links, db.WorktreeLink{
			MergeRequestID: mr.ID,
			WorktreeKey:    fleet.WorktreeScopedKey(ref.Path),
			WorktreePath:   fleet.NormPath(ref.Path),
			WorktreeBranch: ref.Branch,
			LinkedAt:       now,
		})
		watched.add(ghclient.WatchedMR{
			Owner:        ref.Owner,
			Name:         ref.Name,
			Number:       mr.Number,
			Platform:     platform.Kind(ref.Platform),
			PlatformHost: ref.Host,
		})
	}

	watcher.SetWatchedMRs(watched.slice())

	existing, err := database.GetAllWorktreeLinks(ctx)
	if err != nil {
		return false, fmt.Errorf("get existing worktree links: %w", err)
	}
	if linksEqual(existing, links) {
		return false, nil
	}
	if err := database.SetWorktreeLinks(ctx, links); err != nil {
		return false, fmt.Errorf("set worktree links: %w", err)
	}
	return true, nil
}

// applyLinkPR records only the stable item link on spoke-owned raw state.
// Provider display fields are hub-owned and are overlaid after raw
// snapshots have been aggregated.
func applyLinkPR(wt *fleet.RawWorktree, pr db.WorktreeLinkPR) {
	number := pr.Number
	wt.LinkedPRNumber = &number
}

func repoBranchKey(repoID int64, branch string) string {
	return fmt.Sprintf("%d\x00%s", repoID, branch)
}

// linksEqual reports whether two link sets describe the same matches, ignoring
// row id and link timestamp. Worktree keys are unique, so sorting the identity
// tuples gives an order-independent comparison.
func linksEqual(a, b []db.WorktreeLink) bool {
	if len(a) != len(b) {
		return false
	}
	ka, kb := linkIdentityTuples(a), linkIdentityTuples(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func linkIdentityTuples(links []db.WorktreeLink) []string {
	tuples := make([]string, len(links))
	for i, l := range links {
		tuples[i] = fmt.Sprintf("%d\x00%s\x00%s\x00%s",
			l.MergeRequestID, l.WorktreeKey, l.WorktreePath, l.WorktreeBranch)
	}
	sort.Strings(tuples)
	return tuples
}

// watchedSet collects watched merge requests, deduping by identity so two
// worktrees on the same branch report one entry.
type watchedSet struct {
	seen  map[string]struct{}
	items []ghclient.WatchedMR
}

func newWatchedSet() *watchedSet {
	return &watchedSet{seen: map[string]struct{}{}}
}

func (w *watchedSet) add(mr ghclient.WatchedMR) {
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s",
		mr.Owner, mr.Name, mr.Number, mr.Platform, mr.PlatformHost)
	if _, ok := w.seen[key]; ok {
		return
	}
	w.seen[key] = struct{}{}
	w.items = append(w.items, mr)
}

func (w *watchedSet) slice() []ghclient.WatchedMR {
	return w.items
}

// notifyWorktreeLinksChanged announces a changed branch-match link set:
// the event hub gets a payload-free worktree_links_changed refetch hint
// so any SSE subscriber can re-read the snapshot for the new PR
// overlay.
func (s *Handler) notifyWorktreeLinksChanged() {
	if s.broadcast != nil {
		s.broadcast(Event{
			Type: "worktree_links_changed", Data: struct{}{},
		})
	}
}

// NotifyWorktreeLinksChanged is the exported wrapper for callers wiring
// the sync-path link recompute outside this package (cmd/kenn-forge).
func (s *Handler) NotifyWorktreeLinksChanged() {
	s.notifyWorktreeLinksChanged()
}

// notifyWorktreeStatsChanged is the stats-sampler callback: it
// broadcasts a payload-free worktree_stats_changed refetch hint.
func (s *Handler) notifyWorktreeStatsChanged() {
	if s.revalidateDiffs != nil {
		s.revalidateDiffs()
	}
	if s.broadcast != nil {
		s.broadcast(Event{
			Type: "worktree_stats_changed", Data: struct{}{},
		})
	}
}
