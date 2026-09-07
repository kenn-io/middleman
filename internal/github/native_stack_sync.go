package github

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/forge/internal/db"
	platformgithub "go.kenn.io/forge/platform/github"
)

// nativeStackObservationTTL bounds how long a cached stack whose membership
// cannot be fully reconciled against open-PR hints stays confirmed without a
// refetch.
const nativeStackObservationTTL = 12 * time.Hour

// nativeStackConfirmation is the confirmation a 304 pull-request list reuses.
// revalidateAfter is set only when the confirmation covers a stack with members
// no open-PR hint can attest to; an unchanged pull-request list cannot report
// that such a stack changed, so the confirmation itself has to age out.
type nativeStackConfirmation struct {
	numbers         []int
	revalidateAfter time.Time
}

func (c nativeStackConfirmation) expired(now time.Time) bool {
	return !c.revalidateAfter.IsZero() && now.After(c.revalidateAfter)
}

// GitHubNativeStackSyncResult carries the native cache rows that were safe to
// use for this repository sync. Numbers omitted here must fall back to branch
// inference even when an older cache row exists.
type GitHubNativeStackSyncResult struct {
	ConfirmedNumbers []int
	// generation is the preference generation this result was produced under.
	// The completion hook discards results whose generation is no longer
	// current so a disabled preview cannot be reinstated by an in-flight sync.
	generation uint64
}

func nativeStackHintsFromBulk(result *RepoBulkResult) map[int]*platformgithub.NativeStackHint {
	if result == nil {
		return nil
	}
	hints := make(map[int]*platformgithub.NativeStackHint, len(result.PullRequests))
	for i := range result.PullRequests {
		bulk := &result.PullRequests[i]
		hints[bulk.PR.GetNumber()] = bulk.NativeStack
	}
	return hints
}

// RunUnderStackProjection serializes fn with the stack projection performed by
// the sync completion hook. Callers that reconcile stacks after a native-stack
// preference change use it so an in-flight sync cannot write its projection
// after the reconciliation already restored branch inference.
func (s *Syncer) RunUnderStackProjection(fn func()) {
	s.stackProjectionMu.Lock()
	defer s.stackProjectionMu.Unlock()
	fn()
}

// dropStaleNativeStackResults clears native confirmations captured under a
// superseded preference generation. It must run with stackProjectionMu held so
// the decision cannot be invalidated while the hook projects.
func (s *Syncer) dropStaleNativeStackResults(results []RepoSyncResult) {
	generation := s.nativeStackGeneration.Load()
	enabled := s.preferGitHubNativeStacks.Load()
	for i := range results {
		native := results[i].GitHubNativeStacks
		if native == nil {
			continue
		}
		if !enabled || native.generation != generation {
			results[i].GitHubNativeStacks = nil
		}
	}
}

func (s *Syncer) refreshGitHubNativeStackCache(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	hints map[int]*platformgithub.NativeStackHint,
	listUnchanged bool,
) *GitHubNativeStackSyncResult {
	result := &GitHubNativeStackSyncResult{
		generation: s.nativeStackGeneration.Load(),
	}
	confirmationKey := repoFailKey(repo)
	if listUnchanged {
		if entry, ok := s.nativeStackConfirmations.Load(confirmationKey); ok {
			confirmation := entry.(nativeStackConfirmation)
			if !confirmation.expired(s.now().UTC()) {
				result.ConfirmedNumbers = slices.Clone(confirmation.numbers)
				return result
			}
			// The confirmation covers membership no current hint can attest to
			// and has aged out. Withhold it and re-list so the catalog refreshes
			// rather than projecting it for as long as 304s keep arriving.
			s.nativeStackConfirmations.Delete(confirmationKey)
		}
		if client, ok := s.optionalGitHubClientFor(repo); ok {
			// This also covers the setting changing during an in-flight list
			// request. Force a fresh representation on the next sync rather than
			// projecting cache rows that this ETag lifecycle never confirmed.
			client.InvalidateListETagsForRepo(repo.Owner, repo.Name, "pulls")
		}
		return result
	}
	// Only a refresh that resolved every target may seed the confirmation a
	// later 304 reuses. Caching a partial result would suppress the unresolved
	// stacks for as long as the pull-request list keeps returning 304.
	complete := true
	// The deadline follows each stack's own observation, not this refresh: a
	// stack confirmed from cache at 11 hours old must not earn another full
	// window merely because an unrelated 200 response arrived.
	var revalidateAt time.Time
	noteRevalidation := func(observedAt time.Time) {
		deadline := observedAt.Add(nativeStackObservationTTL)
		if revalidateAt.IsZero() || deadline.Before(revalidateAt) {
			revalidateAt = deadline
		}
	}
	defer func() {
		if complete {
			s.nativeStackConfirmations.Store(confirmationKey, nativeStackConfirmation{
				numbers:         slices.Clone(result.ConfirmedNumbers),
				revalidateAfter: revalidateAt,
			})
			return
		}
		// An unresolved target is invisible to the overlap scan, so projecting the
		// stacks this pass did confirm could let one of them claim a pull request
		// the unresolved stack also holds -- shortening that stack and hiding a
		// preceding merge blocker. The whole pass falls back to branch inference.
		result.ConfirmedNumbers = nil
		s.nativeStackConfirmations.Delete(confirmationKey)
		if client, ok := s.optionalGitHubClientFor(repo); ok {
			client.InvalidateListETagsForRepo(repo.Owner, repo.Name, "pulls")
		}
	}()
	cached, err := s.db.ListGitHubNativeStacks(ctx, repoID)
	if err != nil {
		slog.Warn("load github native stack cache failed",
			"platform", repoPlatform(repo), "host", repoHost(repo),
			"repo", repo.Owner+"/"+repo.Name, "err", err)
		complete = false
		return result
	}
	if hints == nil {
		return result
	}

	cachedByNumber := make(map[int]db.GitHubNativeStack, len(cached))
	for _, stack := range cached {
		cachedByNumber[stack.Number] = stack
	}
	targets := make(map[int]bool)
	confirmed := make(map[int]bool)

	for prNumber, hint := range hints {
		if hint == nil {
			continue
		}
		stack, ok := cachedByNumber[hint.Number]
		if !ok || !cachedStackMatchesHint(stack, prNumber, *hint) {
			targets[hint.Number] = true
		}
	}
	now := s.now().UTC()
	for _, stack := range cached {
		if !stack.IsOpen {
			continue
		}
		if cachedStackMatchesCurrentHints(stack, hints) && !targets[stack.Number] &&
			!nativeStackObservationExpired(stack, hints, now) {
			confirmed[stack.Number] = true
			if cachedStackHasUnobservableMember(stack, hints) {
				noteRevalidation(stack.LastObservedAt)
			}
		} else {
			targets[stack.Number] = true
			delete(confirmed, stack.Number)
		}
	}
	if len(targets) == 0 {
		result.ConfirmedNumbers = sortedStackNumbers(confirmed)
		return result
	}

	client, ok := s.optionalGitHubClientFor(repo)
	if !ok {
		result.ConfirmedNumbers = sortedStackNumbers(confirmed)
		return result
	}
	nativeClient, ok := client.(platformgithub.NativeStackClient)
	if !ok {
		result.ConfirmedNumbers = sortedStackNumbers(confirmed)
		return result
	}

	pageNumber := 1
	for len(targets) > 0 {
		page, err := nativeClient.ListNativeStacksPage(ctx, repo.Owner, repo.Name, pageNumber)
		if err != nil {
			if platformgithub.StatusCode(err) == http.StatusNotFound {
				return result
			}
			slog.Warn("refresh github native stack cache failed",
				"platform", repoPlatform(repo), "host", repoHost(repo),
				"repo", repo.Owner+"/"+repo.Name, "page", pageNumber, "err", err)
			complete = false
			result.ConfirmedNumbers = sortedStackNumbers(confirmed)
			return result
		}

		lowest := 0
		for _, stack := range page.Stacks {
			if lowest == 0 || stack.Number < lowest {
				lowest = stack.Number
			}
			if !targets[stack.Number] {
				continue
			}
			delete(targets, stack.Number)
			// A dropped target that was never persisted leaves membership this
			// pass cannot account for, exactly like a fetch failure: the stack is
			// still out there and may share pull requests with the stacks that did
			// confirm, so the refresh is partial however the row was rejected.
			if err := platformgithub.ValidateNativeStack(stack); err != nil {
				slog.Warn("ignore malformed github native stack",
					"platform", repoPlatform(repo), "host", repoHost(repo),
					"repo", repo.Owner+"/"+repo.Name,
					"stack_number", stack.Number, "err", err)
				complete = false
				continue
			}
			if !nativeStackMatchesCurrentHints(stack, hints) {
				slog.Warn("ignore github native stack inconsistent with pull request hints",
					"platform", repoPlatform(repo), "host", repoHost(repo),
					"repo", repo.Owner+"/"+repo.Name,
					"stack_number", stack.Number)
				complete = false
				continue
			}
			if err := s.db.ReplaceGitHubNativeStack(ctx, dbGitHubNativeStack(repoID, stack, s.now().UTC())); err != nil {
				slog.Warn("persist github native stack failed",
					"platform", repoPlatform(repo), "host", repoHost(repo),
					"repo", repo.Owner+"/"+repo.Name,
					"stack_number", stack.Number, "err", err)
				// The target was already dropped, so nothing else marks this
				// refresh partial: a 304 would otherwise reuse confirmations
				// that never included this stack.
				complete = false
				continue
			}
			if stack.Open {
				confirmed[stack.Number] = true
				if nativeStackHasUnobservableMember(stack, hints) {
					// Freshly persisted with this refresh's timestamp.
					noteRevalidation(now)
				}
			}
		}

		var absent []int
		if page.NextPage == 0 {
			for number := range targets {
				absent = append(absent, number)
			}
		} else if lowest > 0 {
			for number := range targets {
				if number > lowest {
					absent = append(absent, number)
				}
			}
		}
		if err := s.db.DeleteGitHubNativeStacks(ctx, repoID, absent); err != nil {
			slog.Warn("delete absent github native stacks failed",
				"platform", repoPlatform(repo), "host", repoHost(repo),
				"repo", repo.Owner+"/"+repo.Name, "err", err)
			complete = false
			result.ConfirmedNumbers = sortedStackNumbers(confirmed)
			return result
		}
		for _, number := range absent {
			delete(targets, number)
			delete(confirmed, number)
		}
		if page.NextPage == 0 {
			break
		}
		pageNumber = page.NextPage
	}

	result.ConfirmedNumbers = sortedStackNumbers(confirmed)
	return result
}

func nativeStackMatchesCurrentHints(stack platformgithub.NativeStack, hints map[int]*platformgithub.NativeStackHint) bool {
	for prNumber, hint := range hints {
		if hint == nil || hint.Number != stack.Number {
			continue
		}
		if hint.Size != len(stack.Members) || hint.BaseRef != stack.BaseRef ||
			hint.Position < 1 || hint.Position > len(stack.Members) ||
			stack.Members[hint.Position-1].PullRequestNumber != prNumber {
			return false
		}
	}
	// Reconcile against observation rather than the state the stack payload
	// reports. A member the payload calls closed may already be open in another
	// stack, and confirming both would project overlapping stacks whose member
	// eviction can drop a preceding merge blocker.
	for _, member := range stack.Members {
		hint, observed := hints[member.PullRequestNumber]
		if !observed {
			continue
		}
		if hint == nil || hint.Number != stack.Number ||
			hint.Size != len(stack.Members) || hint.BaseRef != stack.BaseRef ||
			hint.Position != member.Position {
			return false
		}
	}
	return true
}

// nativeStackObservationExpired reports whether a cached stack holds a member
// that current hints cannot speak for and has not been refetched recently.
// Open-PR hints only prove the positions of members that are still open, so a
// stack containing merged or closed members can drift from GitHub's membership
// without any hint disagreeing. Refetching such a stack on a bounded schedule
// keeps the projection from diverging indefinitely while preserving the cache
// for stacks whose members are all observable.
func nativeStackObservationExpired(
	stack db.GitHubNativeStack, hints map[int]*platformgithub.NativeStackHint, now time.Time,
) bool {
	if !cachedStackHasUnobservableMember(stack, hints) {
		return false
	}
	return now.Sub(stack.LastObservedAt) > nativeStackObservationTTL
}

// cachedStackHasUnobservableMember and nativeStackHasUnobservableMember report
// whether a stack holds a member the current open-PR hints cannot speak for.
// Such a stack can drift without any hint disagreeing, so its confirmation must
// age out instead of surviving every 304.
func cachedStackHasUnobservableMember(
	stack db.GitHubNativeStack, hints map[int]*platformgithub.NativeStackHint,
) bool {
	for _, member := range stack.Members {
		if _, observed := hints[member.PullRequestNumber]; !observed {
			return true
		}
	}
	return false
}

func nativeStackHasUnobservableMember(
	stack platformgithub.NativeStack, hints map[int]*platformgithub.NativeStackHint,
) bool {
	for _, member := range stack.Members {
		if _, observed := hints[member.PullRequestNumber]; !observed {
			return true
		}
	}
	return false
}

func cachedStackMatchesHint(stack db.GitHubNativeStack, prNumber int, hint platformgithub.NativeStackHint) bool {
	if !stack.IsOpen || stack.Number != hint.Number || stack.Size != hint.Size || stack.BaseRef != hint.BaseRef {
		return false
	}
	for _, member := range stack.Members {
		if member.PullRequestNumber == prNumber {
			return member.Position == hint.Position
		}
	}
	return false
}

func cachedStackMatchesCurrentHints(stack db.GitHubNativeStack, hints map[int]*platformgithub.NativeStackHint) bool {
	if !stack.IsOpen || len(stack.Members) != stack.Size {
		return false
	}
	// Cached member state can be stale, so trust the current observation: a
	// member that is open now must still point at this stack.
	foundOpen := false
	for _, member := range stack.Members {
		hint, observed := hints[member.PullRequestNumber]
		if !observed {
			continue
		}
		foundOpen = true
		if hint == nil || !cachedStackMatchesHint(stack, member.PullRequestNumber, *hint) {
			return false
		}
	}
	return foundOpen
}

func sortedStackNumbers(numbers map[int]bool) []int {
	result := make([]int, 0, len(numbers))
	for number := range numbers {
		result = append(result, number)
	}
	slices.Sort(result)
	return result
}

func dbGitHubNativeStack(repoID int64, stack platformgithub.NativeStack, observedAt time.Time) db.GitHubNativeStack {
	result := db.GitHubNativeStack{
		RepoID: repoID, GitHubID: stack.ID, Number: stack.Number,
		Size: len(stack.Members), BaseRef: stack.BaseRef, IsOpen: stack.Open,
		GitHubCreatedAt:    stack.CreatedAt.UTC(),
		ContentFingerprint: platformgithub.NativeStackFingerprint(stack),
		LastObservedAt:     observedAt,
		Members:            make([]db.GitHubNativeStackMember, 0, len(stack.Members)),
	}
	for _, member := range stack.Members {
		var mergedAt *time.Time
		if member.MergedAt != nil {
			utc := member.MergedAt.UTC()
			mergedAt = &utc
		}
		result.Members = append(result.Members, db.GitHubNativeStackMember{
			Position: member.Position, PullRequestNumber: member.PullRequestNumber,
			State: member.State, Draft: member.Draft, MergedAt: mergedAt,
			HeadRef: member.HeadRef, HeadSHA: member.HeadSHA,
		})
	}
	return result
}
