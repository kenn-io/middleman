package github

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v7"
	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/platform"
	platformgithub "go.kenn.io/forge/internal/platform/github"
	"go.kenn.io/forge/internal/shutdownbudget"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/workspace"
	"golang.org/x/sync/singleflight"
)

func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func withCommitOrderMetadata(metadataJSON string, listOrder int, stableOrder int) string {
	metadata := map[string]any{}
	if metadataJSON != "" {
		var existing map[string]any
		if err := json.Unmarshal([]byte(metadataJSON), &existing); err == nil && existing != nil {
			metadata = existing
		}
	}
	metadata["commit_order"] = listOrder
	metadata["commit_order_key"] = stableOrder
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return metadataJSON
	}
	return string(encoded)
}

// withObsoleteMetadata records whether a commit event's commit is still
// reachable from the merge request head. Unchanged input returns the original
// JSON so callers can skip rewriting untouched rows.
func withObsoleteMetadata(metadataJSON string, obsolete bool) (string, bool) {
	metadata := map[string]any{}
	if metadataJSON != "" {
		var existing map[string]any
		if err := json.Unmarshal([]byte(metadataJSON), &existing); err == nil && existing != nil {
			metadata = existing
		}
	}
	value, present := metadata["obsolete"]
	if obsolete {
		if value == true {
			return metadataJSON, false
		}
		metadata["obsolete"] = true
	} else {
		if !present {
			return metadataJSON, false
		}
		delete(metadata, "obsolete")
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return metadataJSON, false
	}
	return string(encoded), true
}

func commitMetadataOrder(metadataJSON string) int {
	if metadataJSON == "" {
		return 0
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return 0
	}
	if order := metadataInt(metadata["commit_order_key"]); order > 0 {
		return order
	}
	return metadataInt(metadata["commit_order"])
}

func metadataInt(value any) int {
	switch v := value.(type) {
	case float64:
		if v > 0 && v == float64(int(v)) {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case json.Number:
		if n, err := strconv.Atoi(v.String()); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

type commitOrderAssigner struct {
	bySHA map[string]int
	next  int
}

func newCommitOrderAssigner(events []db.MREvent) commitOrderAssigner {
	assigner := commitOrderAssigner{bySHA: map[string]int{}}
	for _, event := range events {
		if event.EventType != "commit" {
			continue
		}
		order := commitMetadataOrder(event.MetadataJSON)
		if order == 0 && event.ID > 0 {
			order = int(event.ID)
		}
		if order > assigner.next {
			assigner.next = order
		}
		sha := commitOrderSHA(event.Summary)
		if sha != "" && order > 0 {
			assigner.bySHA[sha] = order
		}
	}
	return assigner
}

func (a *commitOrderAssigner) apply(event *db.MREvent, listOrder int) {
	sha := commitOrderSHA(event.Summary)
	stableOrder := a.bySHA[sha]
	if stableOrder == 0 {
		a.next++
		stableOrder = a.next
		if sha != "" {
			a.bySHA[sha] = stableOrder
		}
	}
	event.MetadataJSON = withCommitOrderMetadata(event.MetadataJSON, listOrder, stableOrder)
}

func (s *Syncer) commitOrderAssigner(ctx context.Context, mrID int64) (commitOrderAssigner, error) {
	events, err := s.db.ListMREvents(ctx, mrID)
	if err != nil {
		return commitOrderAssigner{}, err
	}
	return newCommitOrderAssigner(events), nil
}

// commitEventSHA returns the event's full commit SHA, or "" when neither
// provider field contains one. PlatformExternalID covers Gitealike events,
// whose Summary contains the commit message instead of the SHA.
func commitEventSHA(event db.MREvent) string {
	candidates := []string{
		strings.ToLower(event.PlatformExternalID),
		commitOrderSHA(event.Summary),
	}
	for _, sha := range candidates {
		if len(sha) != 40 && len(sha) != 64 {
			continue
		}
		valid := true
		for _, r := range sha {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				valid = false
				break
			}
		}
		if valid {
			return sha
		}
	}
	return ""
}

// stampableCommitEvent reports whether liveness computation may touch this
// event: it must be a commit event with a representable full SHA and, when
// metadata is present, that metadata must parse to a JSON object (corrupt
// metadata is left untouched rather than clobbered).
func stampableCommitEvent(event db.MREvent) (string, bool) {
	if event.EventType != "commit" {
		return "", false
	}
	sha := commitEventSHA(event)
	if sha == "" {
		return "", false
	}
	if event.MetadataJSON != "" {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil || metadata == nil {
			return "", false
		}
	}
	return sha, true
}

// commitLivenessMemo caches one verified reachability answer per MR.
// Reachability from a fixed head over a fixed candidate set is a pure
// function of immutable git history, so an entry never needs invalidation:
// a changed head or candidate set simply misses the key and recomputes, and
// concurrent rounds writing the same key write the same value. Only verified
// walks are memoized — an unverifiable head proves nothing durable.
type commitLivenessMemo struct {
	key  string
	live map[string]bool
}

// commitLivenessMemoEntry is the LRU list payload for one MR's memo.
type commitLivenessMemoEntry struct {
	mrID int64
	memo commitLivenessMemo
}

// The memo retains provider-controlled input (candidate sets grow with every
// force push), so it is bounded on both axes: at most defaultLivenessMemoLimit
// MR entries with least-recently-used eviction, and results for candidate
// sets larger than defaultLivenessCandidateLimit are computed normally but
// never memoized. Neither bound affects flag correctness — a miss or an
// unmemoized round only costs one budget-capped walk.
const (
	defaultLivenessCandidateLimit = 4096
	defaultLivenessMemoLimit      = 1024
)

// computeCommitLiveness derives commit-obsolescence metadata for a sync round
// BEFORE the round commits, so the results ride the round's own
// revision-guarded snapshot write and a stale round is inert by construction.
// It mutates the incoming batch's commit events in place (their flags travel
// inside the normal upsert) and returns metadata updates for stored commit
// events the round does not re-list. Unverifiable heads return no updates and
// the affected events keep their last verified flags.
//
// A memo hit replaces only the ancestry walk: the verdicts still flow through
// the same injection code, so a hit can never skip a needed write — re-listed
// incoming events always receive their flags, and stored rows that drifted
// are repaired.
func (s *Syncer) computeCommitLiveness(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	headSHA string,
	incoming []db.MREvent,
) map[string]string {
	if s.clones == nil || headSHA == "" {
		return nil
	}
	stored, err := s.db.ListMREvents(ctx, mrID)
	if err != nil {
		slog.Warn("commit liveness: list events failed",
			"repo", repo.Owner+"/"+repo.Name, "mr", mrID, "err", err)
		return nil
	}
	return s.computeCommitLivenessForEvents(ctx, repo, mrID, headSHA, incoming, stored)
}

// computeCommitLivenessForEvents is the computation core behind
// computeCommitLiveness for callers that already hold the stored events —
// in particular the terminal-transition finalizer, which receives them from
// the parent snapshot's own transaction so the walk runs on tx-consistent
// data.
func (s *Syncer) computeCommitLivenessForEvents(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	headSHA string,
	incoming []db.MREvent,
	stored []db.MREvent,
) map[string]string {
	if s.clones == nil || headSHA == "" {
		return nil
	}
	headSHA = strings.ToLower(headSHA)

	candidateSHAs := make([]string, 0, len(stored)+len(incoming))
	seen := make(map[string]bool, len(stored)+len(incoming))
	addCandidate := func(sha string) {
		if !seen[sha] {
			seen[sha] = true
			candidateSHAs = append(candidateSHAs, sha)
		}
	}
	for _, event := range stored {
		if sha, ok := stampableCommitEvent(event); ok {
			addCandidate(sha)
		}
	}
	for i := range incoming {
		if sha, ok := stampableCommitEvent(incoming[i]); ok {
			addCandidate(sha)
		}
	}
	if len(candidateSHAs) == 0 {
		return nil
	}
	candidateLimit := s.livenessCandidateLimit
	if candidateLimit <= 0 {
		candidateLimit = defaultLivenessCandidateLimit
	}
	memoizable := len(candidateSHAs) <= candidateLimit

	memoKey := livenessMemoKey(headSHA, candidateSHAs)
	live, hit := s.livenessMemoLookup(mrID, memoKey)
	if !hit {
		reach, err := s.clones.CommitsReachableFrom(
			ctx, string(repoPlatform(repo)), repoHost(repo),
			repo.Owner, repo.Name, headSHA, candidateSHAs,
		)
		if err != nil {
			slog.Warn("commit liveness: reachability failed",
				"repo", repo.Owner+"/"+repo.Name, "mr", mrID, "err", err)
			return nil
		}
		if !reach.HeadVerified {
			return nil
		}
		live = reach.Live
		if memoizable {
			s.livenessMemoStore(mrID, memoKey, live)
		}
	}

	incomingKeys := make(map[string]bool, len(incoming))
	for i := range incoming {
		sha, ok := stampableCommitEvent(incoming[i])
		if !ok {
			continue
		}
		incomingKeys[incoming[i].DedupeKey] = true
		isLive, evaluated := live[sha]
		if !evaluated {
			continue
		}
		if metadataJSON, changed := withObsoleteMetadata(
			incoming[i].MetadataJSON, !isLive,
		); changed {
			incoming[i].MetadataJSON = metadataJSON
		}
	}

	updates := make(map[string]string)
	for _, event := range stored {
		sha, ok := stampableCommitEvent(event)
		if !ok || incomingKeys[event.DedupeKey] {
			continue
		}
		isLive, evaluated := live[sha]
		if !evaluated {
			continue
		}
		if metadataJSON, changed := withObsoleteMetadata(
			event.MetadataJSON, !isLive,
		); changed {
			updates[event.DedupeKey] = metadataJSON
		}
	}
	return updates
}

// livenessMemoKey hashes the walk's exact inputs: the head plus the sorted,
// deduplicated candidate set. Any change to either produces a different key.
func livenessMemoKey(headSHA string, candidateSHAs []string) string {
	sorted := make([]string, len(candidateSHAs))
	copy(sorted, candidateSHAs)
	slices.Sort(sorted)
	digest := sha256.New()
	digest.Write([]byte(headSHA))
	for _, sha := range sorted {
		digest.Write([]byte{0})
		digest.Write([]byte(sha))
	}
	return string(digest.Sum(nil))
}

func (s *Syncer) livenessMemoLookup(mrID int64, key string) (map[string]bool, bool) {
	s.livenessMemoMu.Lock()
	defer s.livenessMemoMu.Unlock()
	elem, ok := s.livenessMemos[mrID]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*commitLivenessMemoEntry)
	if entry.memo.key != key {
		return nil, false
	}
	s.livenessMemoLRU.MoveToFront(elem)
	return entry.memo.live, true
}

func (s *Syncer) livenessMemoStore(mrID int64, key string, live map[string]bool) {
	s.livenessMemoMu.Lock()
	defer s.livenessMemoMu.Unlock()
	if s.livenessMemos == nil {
		s.livenessMemos = make(map[int64]*list.Element)
		s.livenessMemoLRU = list.New()
	}
	memo := commitLivenessMemo{key: key, live: live}
	if elem, exists := s.livenessMemos[mrID]; exists {
		elem.Value.(*commitLivenessMemoEntry).memo = memo
		s.livenessMemoLRU.MoveToFront(elem)
		return
	}
	limit := s.livenessMemoLimit
	if limit <= 0 {
		limit = defaultLivenessMemoLimit
	}
	for s.livenessMemoLRU.Len() >= limit {
		oldest := s.livenessMemoLRU.Back()
		if oldest == nil {
			break
		}
		s.livenessMemoLRU.Remove(oldest)
		delete(s.livenessMemos, oldest.Value.(*commitLivenessMemoEntry).mrID)
	}
	s.livenessMemos[mrID] = s.livenessMemoLRU.PushFront(
		&commitLivenessMemoEntry{mrID: mrID, memo: memo},
	)
}

func commitOrderSHA(summary string) string {
	return strings.ToLower(strings.TrimSpace(summary))
}

// SyncErrorCode identifies a sync failure that clients can recover from
// without parsing LastError.
type SyncErrorCode string

const (
	SyncErrorCodeLocalCeilingExhausted SyncErrorCode = "localSyncCeilingExhausted"
)

// SyncStatus holds the current state of the sync engine.
type SyncStatus struct {
	Running             bool          `json:"running"`
	CurrentRepo         string        `json:"current_repo,omitempty"`
	Progress            string        `json:"progress,omitempty"`
	LastRunAt           time.Time     `json:"last_run_at,omitzero"`
	LastError           string        `json:"last_error,omitempty"`
	LastErrorCode       SyncErrorCode `json:"last_error_code,omitempty" enum:"localSyncCeilingExhausted"`
	LastErrorCeilingKey string        `json:"last_error_ceiling_key,omitempty"`
	// LastErrorCeilingResetAt identifies the exact local budget window that
	// produced LastError. Clients must match it against the live ceiling row
	// before displaying counters or reset details from that row.
	LastErrorCeilingResetAt string `json:"last_error_ceiling_reset_at,omitempty" format:"date-time"`
}

func formatRateLimitWait(wait time.Duration) string {
	if wait <= 0 {
		return "0s"
	}
	if wait < time.Minute {
		seconds := int((wait + time.Second - time.Nanosecond) / time.Second)
		return fmt.Sprintf("%ds", seconds)
	}

	minutes := int((wait + time.Minute - time.Nanosecond) / time.Minute)
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if hours == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	if remainingMinutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, remainingMinutes)
}

// DiffSyncErrorCode categorizes the reason a diff sync failed. The frontend
// uses this category to render a user-facing message that does not leak local
// clone paths, refs, SHAs, or git stderr.
type DiffSyncErrorCode string

const (
	// DiffSyncCodeCloneUnavailable means the local bare clone could not be
	// created or updated (network failure, disk full, permission denied).
	DiffSyncCodeCloneUnavailable DiffSyncErrorCode = "clone_unavailable"
	// DiffSyncCodeCommitUnreachable means a commit needed to compute the diff
	// (PR head, merge commit, or its first parent) is not present in the local
	// clone and could not be fetched.
	DiffSyncCodeCommitUnreachable DiffSyncErrorCode = "commit_unreachable"
	// DiffSyncCodeMergeBaseFailed means git merge-base could not compute the
	// fork point between the PR head and the base.
	DiffSyncCodeMergeBaseFailed DiffSyncErrorCode = "merge_base_failed"
	// DiffSyncCodeInternal covers database failures and other unexpected
	// internal errors during diff computation.
	DiffSyncCodeInternal DiffSyncErrorCode = "internal"
)

// DiffSyncError reports a non-fatal failure to compute or update the diff SHAs
// for a PR. SyncMR returns this when only the diff portion of the sync failed:
// the PR row, timeline, and CI status were updated successfully, so callers
// should still treat the PR data as fresh, but the diff view will be stale or
// missing until the underlying problem is fixed.
//
// Code categorizes the failure for client-facing messaging via UserMessage.
// Err preserves the underlying detail for server-side logging only — never
// expose Err.Error() to API clients, since it can contain clone paths, refs,
// SHAs, and git stderr.
type DiffSyncError struct {
	Code DiffSyncErrorCode
	Err  error
}

func (e *DiffSyncError) Error() string {
	return fmt.Sprintf("diff sync failed (%s): %v", e.Code, e.Err)
}

func (e *DiffSyncError) Unwrap() error {
	return e.Err
}

// UserMessage returns a sanitized message safe to surface to API clients.
// It never includes clone paths, refs, SHAs, or other internal details from
// the underlying error.
func (e *DiffSyncError) UserMessage() string {
	switch e.Code {
	case DiffSyncCodeCloneUnavailable:
		return "Diff data is unavailable: the local repository clone could not be prepared."
	case DiffSyncCodeCommitUnreachable:
		return "Diff data is unavailable: a required commit is missing from the local clone."
	case DiffSyncCodeMergeBaseFailed:
		return "Diff data is unavailable: could not determine the merge base for this pull request."
	case DiffSyncCodeInternal:
		return "Diff data is unavailable: internal error while updating diff data."
	default:
		return "Diff data is unavailable."
	}
}

// RepoRef identifies a repository on a configured provider.
type RepoRef struct {
	Platform           platform.Kind
	RepoID             int64
	Owner              string
	Name               string
	PlatformHost       string
	RepoPath           string
	PlatformRepoID     int64
	PlatformExternalID string
	WebURL             string
	CloneURL           string
	DefaultBranch      string
	// Archived marks a provider-archived repository: configured for archive
	// collection only, skipped by live sync.
	Archived bool
	// ConfiguredRepoPath is the config-entry path this ref was resolved
	// from. It correlates a tracked repository with its config entry after
	// a provider-side rename, so a transient resolve failure falls back to
	// the tracked ref instead of synthesizing an identity-less duplicate.
	ConfiguredRepoPath string
}

// PartialSyncError reports a repo sync cycle whose index scan completed but
// failed to refresh one or more items in the listed scopes. It is recorded in
// repo and global sync health like any other sync failure, but consumers that
// depend only on an unaffected scope (stack detection over merge requests,
// workspace refresh flows) should proceed instead of treating the repository
// as failed wholesale.
type PartialSyncError struct {
	MergeRequests bool
	Issues        bool
	Cause         error
}

func (e *PartialSyncError) Error() string {
	var failedPaths []string
	if e.MergeRequests {
		failedPaths = append(failedPaths, "merge request")
	}
	if e.Issues {
		failedPaths = append(failedPaths, "issue")
	}
	message := fmt.Sprintf(
		"one or more %s sync items failed", strings.Join(failedPaths, " and "),
	)
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *PartialSyncError) Unwrap() error {
	return e.Cause
}

// ExclusivePartialSyncFailure returns the typed partial failure only when the
// complete error is the partial failure itself. A partial error joined or
// wrapped with any other failure (for example a hard post-sync bookkeeping
// error) describes more than its scopes, so consumers must treat it as a
// hard failure rather than proceeding on the strength of a scope report
// that does not cover the whole error.
func ExclusivePartialSyncFailure(err error) (*PartialSyncError, bool) {
	//nolint:errorlint // exclusivity is deliberate: joined or wrapped errors must not be typed partial
	partial, ok := err.(*PartialSyncError)
	return partial, ok
}

// RepoSyncResult holds the outcome of syncing a single repo.
type RepoSyncResult struct {
	Platform     platform.Kind
	Owner        string
	Name         string
	PlatformHost string
	Error        string // empty on success
	// PartialFailure carries the failed item scopes when Error records a
	// partial per-item failure; nil on success and on hard repository
	// failures.
	PartialFailure *PartialSyncError
	// GitHubNativeStacks is non-nil only when the preview setting participated
	// in this repository sync. Completion hooks may project only these numbers.
	GitHubNativeStacks *GitHubNativeStackSyncResult
}

// WatchedMR identifies a merge request to sync on a fast interval.
type WatchedMR struct {
	Owner        string
	Name         string
	Number       int
	Platform     platform.Kind
	PlatformHost string
}

// defaultParallelism is the worker pool size used by RunOnce when
// SetParallelism has not been called. Bounded so we don't burst the
// per-host GitHub rate limit / abuse-detection thresholds.
const defaultParallelism = 4
const rateLimitSnapshotRefreshInterval = 3 * time.Minute
const activeMRWarmRefreshInterval = 10 * time.Minute

// Display-name cache parameters. Display names rarely change,
// so the success TTL is long enough to skip lookups across many
// sync passes; failures use a shorter TTL so a transient 404
// does not suppress a real retry for hours. The size bound is
// well above any realistic author set for a fixed repo list.
const (
	displayNameCacheSize  = 1024
	displayNameSuccessTTL = 24 * time.Hour
	displayNameFailureTTL = 15 * time.Minute
)

type pendingSyncRun struct {
	bypassNextSyncAfter bool
	full                bool
	priorityRepos       []RepoRef
	onlyRepos           []RepoRef
	bypassRepos         []RepoRef
}

const syncProgressLogInterval = 100
const largeRepoBulkGraphQLThreshold = syncProgressLogInterval
const (
	defaultBranchActivityRetention  = 90 * 24 * time.Hour
	defaultBranchActivityMaxCommits = 5000
)

type itemSyncProgressLogger struct {
	repo   RepoRef
	source string
	item   string
	total  int
}

type listFetchProgressLogger struct {
	repo    RepoRef
	source  string
	item    string
	total   int
	fetched int
	started bool
}

func newIssueSyncProgressLogger(repo RepoRef, source string, total int) itemSyncProgressLogger {
	return newItemSyncProgressLogger(repo, source, "issue", total)
}

func newMergeRequestSyncProgressLogger(repo RepoRef, source string, total int) itemSyncProgressLogger {
	return newItemSyncProgressLogger(repo, source, "merge request", total)
}

func newItemSyncProgressLogger(
	repo RepoRef,
	source string,
	item string,
	total int,
) itemSyncProgressLogger {
	progress := itemSyncProgressLogger{repo: repo, source: source, item: item, total: total}
	if progress.enabled() {
		progress.log(progress.item+" sync started", 0)
	}
	return progress
}

func (p itemSyncProgressLogger) record(processed int) {
	if !p.enabled() || processed >= p.total || processed%syncProgressLogInterval != 0 {
		return
	}
	p.log(p.item+" sync progress", processed)
}

func (p itemSyncProgressLogger) done() {
	if p.enabled() {
		p.log(p.item+" sync completed", p.total)
	}
}

func (p itemSyncProgressLogger) enabled() bool {
	return p.total >= syncProgressLogInterval
}

func (p itemSyncProgressLogger) log(message string, processed int) {
	slog.Info(message,
		"repo", p.repo.Owner+"/"+p.repo.Name,
		"platform", string(repoPlatform(p.repo)),
		"host", repoHost(p.repo),
		"source", p.source,
		"processed", processed,
		"total", p.total,
	)
}

func newIssueListFetchProgressLogger(repo RepoRef, source string) *listFetchProgressLogger {
	return newListFetchProgressLogger(repo, source, "issue")
}

func newMergeRequestListFetchProgressLogger(repo RepoRef, source string) *listFetchProgressLogger {
	return newListFetchProgressLogger(repo, source, "merge request")
}

func newListFetchProgressLogger(repo RepoRef, source, item string) *listFetchProgressLogger {
	return &listFetchProgressLogger{repo: repo, source: source, item: item}
}

func (p *listFetchProgressLogger) setTotal(total int) {
	if p != nil && total > 0 {
		p.total = total
	}
}

func (p *listFetchProgressLogger) recordPage(fetched int, hasMore bool) {
	if p == nil || fetched <= 0 {
		return
	}
	p.fetched += fetched
	if !p.started {
		if !hasMore && p.fetched < syncProgressLogInterval {
			return
		}
		p.started = true
		p.log(p.item + " list fetch started")
		return
	}
	if hasMore {
		p.log(p.item + " list fetch progress")
	}
}

func (p *listFetchProgressLogger) done() {
	if p != nil && p.started {
		if p.total == 0 {
			p.total = p.fetched
		}
		p.log(p.item + " list fetch completed")
	}
}

func (p *listFetchProgressLogger) log(message string) {
	attrs := []any{
		"repo", p.repo.Owner + "/" + p.repo.Name,
		"platform", string(repoPlatform(p.repo)),
		"host", repoHost(p.repo),
		"source", p.source,
		"fetched", p.fetched,
	}
	if p.total > 0 {
		attrs = append(attrs, "total", p.total)
	}
	slog.Info(message, attrs...)
}

// Syncer periodically pulls PR data from GitHub into SQLite.
type Syncer struct {
	clients                  *platform.Registry
	directClients            *platform.Registry
	db                       *db.DB
	archiveRunner            archiveRunner
	archiveLifecycle         archiveRepositoryLifecycle
	archiveWake              chan struct{}
	archivePollInterval      time.Duration
	now                      func() time.Time
	clones                   *gitclone.Manager
	livenessMemoMu           sync.Mutex
	livenessMemos            map[int64]*list.Element // memoized verified reachability (LRU); see computeCommitLiveness
	livenessMemoLRU          *list.List              // recency order for livenessMemos; front = most recent
	livenessMemoLimit        int                     // max memo entries; 0 = defaultLivenessMemoLimit
	livenessCandidateLimit   int                     // max memoizable candidate SHAs; 0 = defaultLivenessCandidateLimit
	rateTrackers             map[string]*RateTracker // provider/host bucket -> tracker
	writeRateTrackers        map[string]*RateTracker // provider/host bucket -> mutation-credential REST tracker
	writeGQLRateTrackers     map[string]*RateTracker // provider/host bucket -> mutation-credential GraphQL tracker
	budgets                  map[string]*SyncBudget  // provider/host bucket -> budget
	quotaRegistry            *QuotaRegistry          // GitHub principal -> live provider quota
	reserveMu                sync.RWMutex
	reserves                 map[string]reserveVerdict  // bucket+resource -> cadence-cached reserve decision
	fetchers                 map[string]*GraphQLFetcher // host -> fallback GraphQL fetcher
	routers                  map[string]*HostRouter     // GitHub host -> credential router
	ratePrincipalLabels      map[string]string
	rateLimitSnapshotMu      sync.Mutex
	rateLimitSnapshotRefresh map[string]time.Time
	repos                    []RepoRef
	reposMu                  sync.Mutex
	mergedActorCursorMu      sync.Mutex
	mergedActorCursors       map[int64]mergedActorSweepState
	interval                 time.Duration
	watchInterval            time.Duration
	watchedMRs               []WatchedMR
	activeMRWindow           time.Duration
	watchMu                  sync.Mutex
	watchSyncMu              sync.Mutex
	branchActivityMu         sync.RWMutex
	branchActivityRetention  time.Duration
	branchActivityMaxCommits int
	preferGitHubNativeStacks atomic.Bool
	syncDisabled             atomic.Bool
	parallelism              atomic.Int32
	runMu                    sync.Mutex
	running                  atomic.Bool
	exclusiveRun             bool            // guarded by runMu
	pendingRun               *pendingSyncRun // guarded by runMu
	providerWorkMu           sync.Mutex
	providerWork             map[string]map[archive.WorkPriority]int
	// archiveDeniedHosts records provider hosts whose live work denied or
	// preempted an archive request; releasing such a host wakes the archive
	// worker, while hosts that never turned archive work away stay quiet.
	archiveDeniedHosts      map[string]struct{}
	archiveProviderRequests map[string]archiveProviderRequest
	status                  atomic.Value // stores *SyncStatus
	stopCh                  chan struct{}
	notificationSyncMu      sync.RWMutex
	notificationSync        NotificationSyncStatus
	stopOnce                sync.Once
	wg                      sync.WaitGroup
	// lifecycleMu serializes TriggerRun registration with Stop so
	// no wg.Add can happen after Stop begins wg.Wait.
	lifecycleMu        sync.Mutex
	stopped            bool                 // guarded by lifecycleMu
	nextSyncAfter      map[string]time.Time // provider/host bucket -> next eligible background sync time
	nextWatchSyncAfter map[string]time.Time // provider/host bucket -> next eligible watch-sync time
	// displayNames is a bounded TTL + LRU cache for resolved
	// GitHub display names. It spans the Syncer's lifetime so
	// cache hits survive across sync runs; per-entry TTL
	// handles profile-name changes without an explicit flush.
	displayNames     *displayNameCache
	displayNameGroup singleflight.Group // dedups concurrent GetUser calls
	onMRSynced       func(owner, name string, mr *db.MergeRequest)
	onSyncCompleted  func(results []RepoSyncResult)
	// onWatchedMRSyncCompleted fires once after a watched-MR fast-sync
	// pass refreshes at least one MR.
	onWatchedMRSyncCompleted func()
	// onMergedActorRepaired fires after scheduled reconciliation persists an
	// authored merge event so consumers can refresh the affected detail.
	onMergedActorRepaired func(context.Context, int64, int)
	onStatusChange        func(status *SyncStatus)
	// onNotificationSyncComplete fires after each notification sync run
	// (periodic, manual, or sidecar) so the server can broadcast the same
	// data-change signal the normal sync uses. Notification sync can insert
	// rows with older timestamps than the feed's top cursor, which the
	// feed's incremental poll would miss without a full reload nudge.
	onNotificationSyncComplete func()
	// statusMu serializes status publication and terminal run-slot release so
	// worker goroutines and later runs cannot deliver out-of-order snapshots.
	// When both locks are needed, statusMu must be acquired before runMu.
	statusMu sync.Mutex

	// failedRepos tracks repos whose last sync had a partial failure
	// (a per-PR, per-issue, or closure-detection step failed after
	// the ETag cache was populated by a successful 200 list fetch).
	// Values are failScope bitmasks indicating which path(s) failed.
	// The next sync cycle consults this set at the top of doSyncRepo
	// and forces an unconditional refetch of the list endpoints so
	// the failed items get re-applied from a fresh 200 response
	// instead of being skipped by a silent 304. Keyed by
	// "host/owner/name". Cleared on the next successful sync.
	failedRepos sync.Map
	// nativeStackResults transfers per-repository preview confirmation from the
	// index sync to the ordered completion hook without persisting run state.
	nativeStackResults sync.Map // map[string]*GitHubNativeStackSyncResult
	// nativeStackConfirmations pairs the in-memory PR-list ETag lifecycle with
	// the last native stack set confirmed from that exact representation.
	nativeStackConfirmations sync.Map // map[string][]int
	// nativeStackGeneration advances on every native-stack preference change.
	// A sync that captured an older generation must not project its native
	// result, or an in-flight run could reinstate native ordering after the
	// user turned the preview off.
	nativeStackGeneration atomic.Uint64
	// stackProjectionMu serializes stack projection between the sync
	// completion hook and preference-change reconciliation so the last write
	// always reflects the current preference.
	stackProjectionMu sync.Mutex

	featureCooldowns repositoryFeatureCooldowns

	// runCtx is the syncer's lifetime context. It is canceled in
	// Stop so in-flight RunOnce / TriggerRun goroutines observe
	// cancellation and unblock any long-running GitHub calls. Both
	// Start and TriggerRun derive their goroutine context from
	// runCtx (merged with any caller context), so Stop can unblock
	// the work it spawned regardless of whether the caller's ctx
	// is still live. runCtxMu guards lazy init and the Stop
	// handoff.
	runCtx    context.Context
	runCancel context.CancelFunc
	runCtxMu  sync.Mutex

	commentRefreshMu         sync.Mutex
	pendingPRCommentSyncs    []queuedPRCommentSync
	pendingIssueCommentSyncs []queuedIssueCommentSync

	afterMergeRequestParentSnapshotCommit   func()
	afterMergedMRMetricsRepair              func()
	afterHeadRepoSnapshotRead               func()
	afterNotificationRepoIdentityReconciled func()
	beforeCloneRouteValidation              func()
}

// DisableSync permanently prevents this Syncer from starting provider refresh
// work. It must be called during startup, before the Syncer is exposed.
func (s *Syncer) DisableSync() {
	if s != nil {
		s.syncDisabled.Store(true)
	}
}

func (s *Syncer) SyncEnabled() bool {
	return s != nil && !s.syncDisabled.Load()
}

func (s *Syncer) syncDisabledError() error {
	if !s.SyncEnabled() {
		return platform.ErrSyncDisabled
	}
	return nil
}

type archiveProviderRequest struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type archiveRunner interface {
	// RunPass performs one worker pass and reports whether it attempted
	// provider work.
	RunPass(context.Context) (bool, error)
}

type archiveRepositoryLifecycle interface {
	EnsureConfigured(context.Context, []platform.RepoRef) ([]platform.RepoRef, error)
	RetryAuthentication(context.Context, []platform.RepoRef) error
}

type queuedPRCommentSync struct {
	repo   RepoRef
	repoID int64
	number int
}

type queuedIssueCommentSync struct {
	repo   RepoRef
	repoID int64
	number int
}

// ensureRunCtx lazily initializes runCtx/runCancel. Safe to call
// multiple times; the first caller wins and later calls are no-ops.
func (s *Syncer) ensureRunCtx() context.Context {
	s.runCtxMu.Lock()
	defer s.runCtxMu.Unlock()
	if s.runCtx == nil {
		s.runCtx, s.runCancel = context.WithCancel(context.Background())
	}
	return s.runCtx
}

// mergeWithRunCtx returns a context that is canceled when either the
// caller's ctx or the syncer's lifetime ctx is canceled. The returned
// cancel function must be called to release resources. Used by
// TriggerRun so ad-hoc runs respect both the caller's deadline and
// Stop's global cancellation signal.
func (s *Syncer) mergeWithRunCtx(caller context.Context) (context.Context, context.CancelFunc) {
	runCtx := s.ensureRunCtx()
	merged, cancel := context.WithCancel(caller)
	go func() {
		select {
		case <-runCtx.Done():
			cancel()
		case <-merged.Done():
		}
	}()
	return merged, cancel
}

// failScope is a bitmask indicating which sync paths failed.
type failScope uint8

const (
	failMR     failScope = 1 << iota // PR/MR sync path failed
	failIssues                       // issue sync path failed
)

func preservePartialSyncFailure(scope failScope, failed bool, cause error) error {
	if !failed {
		return cause
	}
	return &PartialSyncError{
		MergeRequests: scope&failMR != 0,
		Issues:        scope&failIssues != 0,
		Cause:         cause,
	}
}

// retainSyncBudgetCause keeps the first local-ceiling error seen while item
// failures are aggregated. Other item errors intentionally retain the existing
// generic partial-failure message, but the budget sentinel must survive so the
// run status can classify local-ceiling exhaustion without parsing text.
func retainSyncBudgetCause(current, candidate error) error {
	if errors.Is(current, platform.ErrSyncBudgetExhausted) {
		return current
	}
	if errors.Is(candidate, platform.ErrSyncBudgetExhausted) {
		return candidate
	}
	return current
}

func partialItemFailureError(message string, budgetCause error) error {
	if budgetCause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, budgetCause)
}

func joinPartialFailureCause(budgetCause, cause error) error {
	if budgetCause == nil {
		return cause
	}
	return errors.Join(budgetCause, cause)
}

func partialSyncFailureScope(err error) failScope {
	var partial *PartialSyncError
	if !errors.As(err, &partial) {
		return 0
	}
	var scope failScope
	if partial.MergeRequests {
		scope |= failMR
	}
	if partial.Issues {
		scope |= failIssues
	}
	return scope
}

// markRepoFailed records that the most recent sync of this repo hit
// a partial failure after the ETag cache may have been populated, so
// the next cycle must force an unconditional refetch of the affected
// list endpoints. Successful attempted scopes clear their own bits.
func (s *Syncer) markRepoFailed(repo RepoRef, scope failScope) {
	key := repoFailKey(repo)
	for {
		prev, ok := s.failedRepos.Load(key)
		merged := scope
		if ok {
			merged |= prev.(failScope)
		}
		if ok {
			if s.failedRepos.CompareAndSwap(key, prev, merged) {
				return
			}
		} else {
			if _, loaded := s.failedRepos.LoadOrStore(key, merged); !loaded {
				return
			}
		}
		// Another goroutine raced us; retry.
	}
}

func (s *Syncer) clearRepoFailedScope(repo RepoRef, scope failScope) {
	key := repoFailKey(repo)
	for {
		value, ok := s.failedRepos.Load(key)
		if !ok {
			return
		}
		remaining := value.(failScope) &^ scope
		if remaining == 0 {
			if s.failedRepos.CompareAndDelete(key, value) {
				return
			}
			continue
		}
		if s.failedRepos.CompareAndSwap(key, value, remaining) {
			return
		}
	}
}

// repoFailKey returns the sync.Map key for a repo. Includes provider
// and host so multi-provider and multi-host setups don't cross-invalidate.
func repoFailKey(repo RepoRef) string {
	return string(repoPlatform(repo)) + "/" + repoHost(repo) + "/" +
		strings.ToLower(repo.Owner) + "/" + strings.ToLower(repo.Name)
}

// consumeRepoFailed returns the failScope bitmask for a previously
// failed repo. Returns 0 if the repo had no failure. The flag remains
// set until a subsequent successful sync explicitly clears it.
func (s *Syncer) consumeRepoFailed(repo RepoRef) failScope {
	v, ok := s.failedRepos.Load(repoFailKey(repo))
	if !ok {
		return 0
	}
	return v.(failScope)
}

// publishStatus stores a status snapshot and invokes the
// onStatusChange callback if one is registered. Used in place of
// s.status.Store so SSE subscribers see every state transition.
func (s *Syncer) publishStatus(status *SyncStatus) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.publishStatusLocked(status)
}

func (s *Syncer) publishStatusLocked(status *SyncStatus) {
	s.status.Store(status)
	if s.onStatusChange != nil {
		s.onStatusChange(status)
	}
}

// NewSyncer creates a Syncer that polls the given repos on the
// given interval. clients maps host -> Client; rateTrackers maps
// host -> RateTracker. Both may contain nil values. clones may
// be nil. budgets maps host -> SyncBudget; nil or empty disables
// detail drain and archive collection. Budgets are created by the caller
// (typically main.go) and wired into each Client's HTTP transport
// at construction time so every sync-context RoundTrip is
// automatically counted.
func NewSyncer(
	clients map[string]Client,
	database *db.DB,
	clones *gitclone.Manager,
	repos []RepoRef,
	interval time.Duration,
	rateTrackers map[string]*RateTracker,
	budgets map[string]*SyncBudget,
) *Syncer {
	return NewSyncerWithRegistry(
		registryFromGitHubClients(clients),
		database,
		clones,
		repos,
		interval,
		rateTrackers,
		budgets,
	)
}

func NewSyncerWithRegistry(
	registry *platform.Registry,
	database *db.DB,
	clones *gitclone.Manager,
	repos []RepoRef,
	interval time.Duration,
	rateTrackers map[string]*RateTracker,
	budgets map[string]*SyncBudget,
) *Syncer {
	if registry == nil {
		registry, _ = platform.NewRegistry()
	}
	if rateTrackers == nil {
		rateTrackers = make(map[string]*RateTracker)
	}
	if budgets == nil {
		budgets = make(map[string]*SyncBudget)
	}

	s := &Syncer{
		db:                       database,
		clones:                   clones,
		rateTrackers:             rateTrackers,
		budgets:                  budgets,
		repos:                    slices.Clone(repos),
		interval:                 interval,
		rateLimitSnapshotRefresh: make(map[string]time.Time),
		branchActivityRetention:  defaultBranchActivityRetention,
		branchActivityMaxCommits: defaultBranchActivityMaxCommits,
		nextSyncAfter:            make(map[string]time.Time),
		nextWatchSyncAfter:       make(map[string]time.Time),
		archiveProviderRequests:  make(map[string]archiveProviderRequest),
		stopCh:                   make(chan struct{}),
		archiveWake:              make(chan struct{}, 1),
		archivePollInterval:      time.Second,
		now:                      time.Now,
		displayNames: newDisplayNameCache(
			displayNameCacheSize,
			displayNameSuccessTTL,
			displayNameFailureTTL,
		),
	}
	s.directClients = registry
	s.clients = registry.WithProviderGate(s.syncDisabledError)
	s.parallelism.Store(defaultParallelism)
	s.status.Store(&SyncStatus{})

	// Wire budget reset to rate tracker window resets.
	for h, rt := range rateTrackers {
		if b, ok := budgets[h]; ok && rt != nil {
			rt.SetOnWindowReset(b.Reset)
		}
	}

	return s
}

// SetArchiveService attaches the provider-neutral archive worker before Start.
// It also extends provider window-reset callbacks so budget resets wake queued
// archive work immediately.
func (s *Syncer) SetArchiveService(runner archiveRunner) {
	s.archiveRunner = runner
	s.archiveLifecycle, _ = runner.(archiveRepositoryLifecycle)
	for key, tracker := range s.rateTrackers {
		if tracker == nil {
			continue
		}
		budget := s.budgets[key]
		tracker.SetOnWindowReset(func() {
			if budget != nil {
				budget.Reset()
			}
			s.WakeArchive()
		})
	}
}

func (s *Syncer) WakeArchive() {
	if !s.SyncEnabled() {
		return
	}
	select {
	case s.archiveWake <- struct{}{}:
	default:
	}
}

// SetArchivePollIntervalForTesting shortens the pacing between archive worker
// passes. It is the wait after a pass that attempted work or failed and the
// starting point of the idle backoff.
func (s *Syncer) SetArchivePollIntervalForTesting(interval time.Duration) {
	if interval > 0 {
		s.archivePollInterval = interval
	}
}

func (s *Syncer) ConfiguredRepositories(context.Context) ([]platform.RepoRef, error) {
	s.reposMu.Lock()
	repos := slices.Clone(s.repos)
	s.reposMu.Unlock()
	refs := make([]platform.RepoRef, 0, len(repos))
	for _, repo := range repos {
		kind := repoPlatform(repo)
		host := repoHost(repo)
		refs = append(refs, platform.RepoRef{
			Platform: kind, Host: host, Owner: repo.Owner, Name: repo.Name,
			RepoPath:           repo.Owner + "/" + repo.Name,
			PlatformExternalID: repo.PlatformExternalID,
		})
	}
	return refs, nil
}

func (s *Syncer) Admit(
	ctx context.Context,
	ref platform.RepoRef,
	itemType db.ArchiveItemType,
	cost int,
) (archive.AdmissionResult, error) {
	feature := ""
	switch itemType {
	case db.ArchiveItemTypeIssue:
		feature = platform.RepositoryFeatureIssues
	case db.ArchiveItemTypeMergeRequest:
		feature = platform.RepositoryFeatureMergeRequests
	default:
		return archive.AdmissionResult{}, fmt.Errorf("archive admission: invalid item type %q", itemType)
	}
	repo := RepoRef{
		Platform: ref.Platform, PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		PlatformExternalID: ref.PlatformExternalID,
	}
	if archive.InventoryProbeRequested(ctx) {
		ctx = withRepositoryFeatureCooldownBypass(
			ctx, s.featureCooldowns.currentGeneration(),
		)
	}
	probe, due, featureRetryAt := s.beginRepositoryFeatureProbeWithRetry(ctx, repo, feature)
	if !due {
		return archive.AdmissionResult{FeatureDeferred: &archive.FeatureDeferral{
			RetryAt: featureRetryAt,
			Detail:  "repository feature cooldown active",
		}}, nil
	}
	now := s.now().UTC()
	if s.running.Load() {
		probe.abandon()
		retryAt := now.Add(time.Second)
		return archive.AdmissionResult{RetryAt: &retryAt, Detail: "normal sync is active"}, nil
	}
	keyRepo := RepoRef{
		Platform:       ref.Platform,
		PlatformHost:   ref.Host,
		Owner:          ref.Owner,
		Name:           ref.Name,
		RepoPath:       ref.RepoPath,
		PlatformRepoID: ref.PlatformID,
	}
	identity, err := s.archiveIdentityForRepo(keyRepo)
	if err != nil {
		return archive.AdmissionResult{}, err
	}
	key := RateBucketKey(string(repoPlatform(keyRepo)), identity.Host, identity.Principal)
	if s.higherPriorityProviderWorkActive(key, archive.PriorityFullArchive) {
		probe.abandon()
		s.noteArchiveDenied(key)
		retryAt := now.Add(time.Second)
		return archive.AdmissionResult{RetryAt: &retryAt, Detail: "higher-priority sync work is active"}, nil
	}
	tracker := s.rateTrackers[key]
	var providerResetAt *time.Time
	var providerPacingWindow *QuotaPacingWindow
	var providerResources []QuotaResource
	identity, identityErr := s.archiveIdentityForRepo(repo)
	if ref.Platform == platform.KindGitHub && s.quotaRegistry != nil && identityErr == nil {
		providerResources = []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL}
		pacingWindow, pacingKnown := s.quotaRegistry.PacingWindow(identity, providerResources)
		if !pacingKnown || pacingWindow.ArchiveHeadroom < cost {
			probe.abandon()
			retryAt := now.Add(time.Minute)
			detail := "provider rate reserve reached"
			if !pacingKnown {
				detail = "provider quota unknown"
			} else if reset := pacingWindow.ArchiveRetryAt(cost); reset.After(now) {
				retryAt = reset.UTC()
			}
			return archive.AdmissionResult{RetryAt: &retryAt, Detail: detail}, nil
		}
		providerPacingWindow = &pacingWindow
		providerResetAt = &providerPacingWindow.ResetAt
	} else if tracker != nil && (tracker.IsPaused() ||
		tracker.Known() && tracker.Remaining()-cost < RateReserveBuffer) {
		probe.abandon()
		retryAt := now.Add(time.Minute)
		if reset := tracker.ResetAt(); reset != nil && reset.After(now) {
			retryAt = reset.UTC()
		}
		return archive.AdmissionResult{RetryAt: &retryAt, Detail: "provider rate reserve reached"}, nil
	}
	budget := s.budgets[key]
	resetAt := providerResetAt
	if resetAt == nil {
		resetAt = archiveBudgetResetAt(tracker)
	}
	available := 0
	if providerPacingWindow != nil {
		// Provider quota is authoritative: archive may spend everything above
		// each pool's own archive reserve, and the local sync budget meters
		// live sync only.
		available = max(providerPacingWindow.ArchiveHeadroom, 0)
	} else if budget != nil {
		liveFloor := archiveLiveFloor(ref.Platform)
		if resetAt == nil &&
			(ref.Platform == platform.KindGitea || ref.Platform == platform.KindForgejo) {
			available = budget.LocalArchiveSpendAvailable(liveFloor)
		} else {
			available = budget.ArchiveSpendAvailable(now, resetAt, liveFloor)
		}
	}
	if available < cost {
		probe.abandon()
		retryAt := now.Add(time.Minute)
		if resetAt != nil && resetAt.After(now) && resetAt.Before(retryAt) {
			retryAt = resetAt.UTC()
		}
		return archive.AdmissionResult{RetryAt: &retryAt, Detail: "archive surplus budget unavailable"}, nil
	}
	requestCtx, releaseProviderRequest, allowed := s.tryBeginArchiveProviderRequest(ctx, key)
	if !allowed {
		probe.abandon()
		retryAt := now.Add(time.Second)
		return archive.AdmissionResult{RetryAt: &retryAt, Detail: "higher-priority sync work is active"}, nil
	}
	requestCtx = WithArchiveSyncBudget(requestCtx)
	completeGitealikeMR := itemType == db.ArchiveItemTypeMergeRequest &&
		(ref.Platform == platform.KindGitea || ref.Platform == platform.KindForgejo)
	if !completeGitealikeMR {
		if providerPacingWindow != nil {
			requestCtx = WithArchiveProviderAttemptAllowance(
				requestCtx, available, identity, providerResources,
			)
		} else {
			requestCtx = WithArchiveAttemptAllowance(requestCtx, available)
		}
	}
	var completeOnce sync.Once
	var featureDeferred *archive.FeatureDeferral
	complete := func(cause error, providerAttempted bool) *archive.FeatureDeferral {
		completeOnce.Do(func() {
			if !providerAttempted {
				probe.abandon()
			} else if disabled := repositoryFeatureDisabledError(repo, feature, cause); disabled != nil {
				nextProbe, _ := s.recordRepositoryFeatureDisabledUntil(repo, feature, disabled)
				featureDeferred = &archive.FeatureDeferral{
					RetryAt: nextProbe,
					Detail:  disabled.Error(),
				}
			} else {
				probe.release()
			}
			releaseProviderRequest()
		})
		return featureDeferred
	}
	// The declared cost is the admission minimum. Gitealike merge-request reads
	// are atomic and data-complete, so they remain preemptible but are not cut
	// off mid-dataset by the admission estimate. Other archive reads may use the
	// currently available surplus without crossing the live floor.
	return archive.AdmissionResult{
		Allowed:  true,
		Context:  requestCtx,
		Complete: complete,
	}, nil
}

// noteArchiveDenied records that live work on key turned an archive request
// away, so releasing the host later wakes the archive worker.
func (s *Syncer) noteArchiveDenied(key string) {
	s.providerWorkMu.Lock()
	s.markArchiveDeniedLocked(key)
	s.providerWorkMu.Unlock()
}

func (s *Syncer) markArchiveDeniedLocked(key string) {
	if s.archiveDeniedHosts == nil {
		s.archiveDeniedHosts = make(map[string]struct{})
	}
	s.archiveDeniedHosts[key] = struct{}{}
}

func (s *Syncer) tryBeginArchiveProviderRequest(
	ctx context.Context,
	key string,
) (context.Context, func(), bool) {
	s.providerWorkMu.Lock()
	if active := s.providerWork[key]; len(active) > 0 {
		s.providerWorkMu.Unlock()
		return nil, nil, false
	}
	if s.providerWork == nil {
		s.providerWork = make(map[string]map[archive.WorkPriority]int)
	}
	s.providerWork[key] = map[archive.WorkPriority]int{archive.PriorityFullArchive: 1}
	requestCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.archiveProviderRequests[key] = archiveProviderRequest{cancel: cancel, done: done}
	s.providerWorkMu.Unlock()

	var once sync.Once
	return requestCtx, func() {
		once.Do(func() {
			cancel()
			s.providerWorkMu.Lock()
			active := s.providerWork[key]
			delete(active, archive.PriorityFullArchive)
			if len(active) == 0 {
				delete(s.providerWork, key)
			}
			delete(s.archiveProviderRequests, key)
			close(done)
			s.providerWorkMu.Unlock()
		})
	}, true
}

func archiveBudgetResetAt(tracker *RateTracker) *time.Time {
	if tracker == nil {
		return nil
	}
	return tracker.ResetAt()
}

func archiveLiveFloor(kind platform.Kind) int {
	if kind == platform.KindGitea || kind == platform.KindForgejo {
		return detailWorstCaseAttemptCost(kind, QueueItemPR)
	}
	floor := detailWorstCaseAttemptCost(kind, QueueItemPR) + wireAttemptsPerRequest
	if kind == platform.KindGitHub {
		floor += wireAttemptsPerRequest // one notification page
	}
	return floor
}

func (s *Syncer) beginProviderWork(key string, priority archive.WorkPriority) func() {
	s.providerWorkMu.Lock()
	if s.providerWork == nil {
		s.providerWork = make(map[string]map[archive.WorkPriority]int)
	}
	active := s.providerWork[key]
	if active == nil {
		active = make(map[archive.WorkPriority]int)
		s.providerWork[key] = active
	}
	active[priority]++
	archiveRequest, waitForArchive := s.archiveProviderRequests[key]
	if waitForArchive {
		archiveRequest.cancel()
		s.markArchiveDeniedLocked(key)
	}
	s.providerWorkMu.Unlock()
	if waitForArchive {
		<-archiveRequest.done
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			s.providerWorkMu.Lock()
			active := s.providerWork[key]
			active[priority]--
			if active[priority] == 0 {
				delete(active, priority)
			}
			hostFree := len(active) == 0
			if hostFree {
				delete(s.providerWork, key)
			}
			_, wake := s.archiveDeniedHosts[key]
			if hostFree && wake {
				delete(s.archiveDeniedHosts, key)
			}
			s.providerWorkMu.Unlock()
			if hostFree && wake {
				// This host turned archive work away while live work held it
				// and the worker backed off; tell it the host is free again.
				// Hosts that never denied archive work stay quiet, so a normal
				// sync's stream of releases does not trigger denied passes.
				s.WakeArchive()
			}
		})
	}
}

func (s *Syncer) higherPriorityProviderWorkActive(key string, threshold archive.WorkPriority) bool {
	s.providerWorkMu.Lock()
	defer s.providerWorkMu.Unlock()
	for priority, count := range s.providerWork[key] {
		if count > 0 && priority < threshold {
			return true
		}
	}
	return false
}

// verifyRepoRouteOwnershipUnderReconciliationRead confirms, while the caller
// holds the reconciliation read lock, that repoID may keep persisting data
// fetched from the given route. A concurrent reconciliation can hand the
// route to a replacement repository mid-sync; committing this pass's snapshot
// would then write the replacement's data into the displaced repository.
func (s *Syncer) verifyRepoRouteOwnershipUnderReconciliationRead(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
) error {
	if repo.Owner == "" || repo.Name == "" {
		// Callers without a fetch route still get an identity check
		// against the repository's own recorded route.
		row, err := s.db.GetRepoByID(ctx, repoID)
		if err != nil {
			return err
		}
		if row == nil {
			return fmt.Errorf(
				"repository %d is missing: dropping stale snapshot", repoID,
			)
		}
		repo = RepoRef{
			Platform:     platform.Kind(row.Platform),
			PlatformHost: row.PlatformHost,
			Owner:        row.Owner,
			Name:         row.Name,
		}
	}
	occupant, err := s.db.GetRepoByIdentityUnderRepositoryReconciliationRead(
		ctx, db.RepoIdentity{
			Platform:     string(repoPlatform(repo)),
			PlatformHost: repoHost(repo),
			RepoPath:     repo.Owner + "/" + repo.Name,
		},
	)
	if err != nil {
		return err
	}
	if occupant != nil {
		if occupant.ID == repoID {
			return nil
		}
		return fmt.Errorf(
			"repository route %s/%s now belongs to repository %d, not %d: dropping stale snapshot",
			repo.Owner, repo.Name, occupant.ID, repoID,
		)
	}
	row, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf(
			"repository %d for route %s/%s is missing: dropping stale snapshot",
			repoID, repo.Owner, repo.Name,
		)
	}
	if row.PlatformRepoID != "" {
		return fmt.Errorf(
			"repository route %s/%s no longer belongs to cataloged repository %d: dropping stale snapshot",
			repo.Owner, repo.Name, repoID,
		)
	}
	// Legacy route-only repositories never occupy a catalog route; their
	// writes stay bound by route exactly as before cataloging.
	return nil
}

func (s *Syncer) commitIssueParentSnapshot(
	ctx context.Context,
	repo RepoRef,
	issue *db.Issue,
) (int64, int64, bool, error) {
	return s.commitIssueParentSnapshotWithRouteFence(ctx, repo, issue, nil)
}

func (s *Syncer) commitIssueParentSnapshotIfRouteFence(
	ctx context.Context,
	repo RepoRef,
	issue *db.Issue,
	fence db.RepositoryRouteFence,
) (int64, int64, bool, error) {
	return s.commitIssueParentSnapshotWithRouteFence(
		ctx, repo, issue, &fence,
	)
}

func (s *Syncer) commitIssueParentSnapshotWithRouteFence(
	ctx context.Context,
	repo RepoRef,
	issue *db.Issue,
	fence *db.RepositoryRouteFence,
) (int64, int64, bool, error) {
	if fence != nil {
		ctx = s.db.WithRepositoryRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(repo)), *fence,
		)
	}
	lockedCtx, releaseReconciliation, err :=
		s.db.LockRepositoryReconciliationReadForWrite(ctx)
	if err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	defer releaseReconciliation()
	ctx = lockedCtx
	if err := s.verifyRepoRouteOwnershipUnderReconciliationRead(
		ctx, repo, issue.RepoID,
	); err != nil {
		return 0, 0, false, err
	}
	return s.db.UpsertIssueSnapshotWithLabels(ctx, issue)
}

// CommitMergeRequestParentSnapshot is the single choke point every accepted
// provider MR snapshot funnels through. Reclassifying workspace head-repo trust
// here guarantees every path that can change an MR's head_repo_clone_url fans
// that change out to a tracking workspace's mr_head_repo — see
// reclassifyWorkspaceHeadRepoTrust.
// CommitMergeRequestParentSnapshot applies a provider parent snapshot. When
// the accepted snapshot takes the merge request out of the open state, the
// db layer detects that transition inside the snapshot transaction and runs
// the terminal commit-liveness computation there, against the transaction's
// own view of the stored events — finalization is intrinsic to the choke
// point, so no caller can commit a terminal transition without it and no
// concurrent round can shift the data between compute and commit.
func (s *Syncer) CommitMergeRequestParentSnapshot(
	ctx context.Context,
	repo RepoRef,
	mr *db.MergeRequest,
) (int64, int64, bool, error) {
	return s.commitMergeRequestParentSnapshotWithRouteFence(ctx, repo, mr, nil)
}

func (s *Syncer) commitMergeRequestParentSnapshotIfRouteFence(
	ctx context.Context,
	repo RepoRef,
	mr *db.MergeRequest,
	fence db.RepositoryRouteFence,
) (int64, int64, bool, error) {
	return s.commitMergeRequestParentSnapshotWithRouteFence(
		ctx, repo, mr, &fence,
	)
}

func (s *Syncer) commitMergeRequestParentSnapshotWithRouteFence(
	ctx context.Context,
	repo RepoRef,
	mr *db.MergeRequest,
	fence *db.RepositoryRouteFence,
) (int64, int64, bool, error) {
	ctx = withCloneRepositoryIdentity(ctx, repo)
	if fence != nil {
		ctx = s.db.WithRepositoryRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(repo)), *fence,
		)
	}
	lockedCtx, releaseReconciliation, err :=
		s.db.LockRepositoryReconciliationReadForWrite(ctx)
	if err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	defer releaseReconciliation()
	ctx = lockedCtx

	if err := s.verifyRepoRouteOwnershipUnderReconciliationRead(
		ctx, repo, mr.RepoID,
	); err != nil {
		return 0, 0, false, err
	}
	mrID, revision, accepted, err :=
		s.db.UpsertMergeRequestSnapshotWithLabelsUnderRepositoryReconciliationRead(
			ctx, mr, s.terminalLivenessComputer(ctx, repo, mr),
		)
	if err != nil || !accepted {
		return mrID, revision, accepted, err
	}
	if s.afterMergeRequestParentSnapshotCommit != nil {
		s.afterMergeRequestParentSnapshotCommit()
	}
	s.reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead(
		ctx, repo, mr.RepoID, mr.Number,
	)
	return mrID, revision, accepted, err
}

func (s *Syncer) repositoryRouteFenceMatches(
	ctx context.Context,
	repo RepoRef,
	fence db.RepositoryRouteFence,
) (bool, error) {
	releaseReconciliation, err := s.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return false, err
	}
	defer releaseReconciliation()
	return s.db.RepositoryRouteFenceMatchesUnderRepositoryReconciliationRead(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), fence,
	)
}

func (s *Syncer) markMergeRequestDetailFetchedIfRouteFence(
	ctx context.Context,
	repo RepoRef,
	fence db.RepositoryRouteFence,
	mrID, revision int64,
	pending bool,
	eventMetadataUpdates map[string]string,
) (bool, error) {
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), fence,
	)
	applied, err := s.db.MarkMergeRequestDetailFetchedSnapshot(
		ctx, mrID, revision, pending, eventMetadataUpdates,
	)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return false, nil
	}
	return applied, err
}

func (s *Syncer) markIssueDetailFetchedIfRouteFence(
	ctx context.Context,
	repo RepoRef,
	fence db.RepositoryRouteFence,
	issueID, revision int64,
) (bool, error) {
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), fence,
	)
	applied, err := s.db.MarkIssueDetailFetchedSnapshot(ctx, issueID, revision)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return false, nil
	}
	return applied, err
}

// commitIssueCommentsSnapshot binds the child snapshot to the parent issue ID
// the caller fetched for. Resolving the parent by route here would let a
// concurrent route replacement redirect stale comments onto the replacement
// repository's issue when snapshot revisions coincide.
func (s *Syncer) commitIssueCommentsSnapshot(
	ctx context.Context,
	_ RepoRef,
	issueID int64,
	number int,
	expectedRevision int64,
	events []db.IssueEvent,
	otherEvents []db.IssueEvent,
	derived *db.IssueDerivedFields,
) (bool, error) {
	for i := range events {
		events[i].IssueID = issueID
	}
	for i := range otherEvents {
		otherEvents[i].IssueID = issueID
	}
	applied, err := s.db.CommitIssueChildSnapshot(ctx, db.IssueChildSnapshot{
		IssueID: issueID, ExpectedRevision: expectedRevision,
		Comments: events, OtherEvents: otherEvents, DerivedFields: derived,
	})
	if err != nil {
		return false, fmt.Errorf("commit child snapshot for issue #%d: %w", number, err)
	}
	return applied, nil
}

// commitMergeRequestDatasets binds the child snapshot to the parent MR ID the
// caller fetched for — see commitIssueCommentsSnapshot for why the route must
// not resolve the parent.
//
// livenessHeadSHA, when non-empty, is the open MR's current head for this
// round: commit-obsolescence flags are computed against it and ride this same
// revision-guarded snapshot (incoming commit events carry flags in their
// metadata; stored events the round does not re-list are updated in the same
// transaction). Rounds without a verifiable head commit their events without
// liveness changes. Callers pass "" for closed MRs and for batches that carry
// no commit events.
func (s *Syncer) commitMergeRequestDatasets(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	number int,
	expectedRevision int64,
	comments []db.MREvent,
	commentsComplete bool,
	reviews []db.MREvent,
	inline []db.MREvent,
	threads []db.MRReviewThread,
	inlineComplete bool,
	otherEvents []db.MREvent,
	derived *db.MRDerivedFields,
	livenessHeadSHA string,
) (bool, error) {
	for _, events := range [][]db.MREvent{comments, reviews, inline, otherEvents} {
		for i := range events {
			events[i].MergeRequestID = mrID
		}
	}
	metadataUpdates := s.computeCommitLiveness(
		ctx, repo, mrID, livenessHeadSHA, otherEvents,
	)
	applied, err := s.db.CommitMergeRequestChildSnapshot(ctx, db.MergeRequestChildSnapshot{
		MergeRequestID: mrID, ExpectedRevision: expectedRevision,
		Comments: comments, CommentsComplete: commentsComplete, Reviews: reviews,
		InlineComments: inline, ReviewThreads: threads, InlineCommentsComplete: inlineComplete,
		OtherEvents: otherEvents, DerivedFields: derived,
		EventMetadataUpdates: metadataUpdates,
	})
	if err != nil {
		return false, fmt.Errorf("commit child snapshot for MR #%d: %w", number, err)
	}
	return applied, nil
}

// terminalLivenessComputer returns the in-transaction finalizer for a parent
// snapshot that may take the merge request out of the open state. The db
// layer invokes it only when the accepted upsert is that transition, with the
// transaction's own view of the stored events: the transition round is the
// last round that will ever compute this MR's flags, so they are computed
// from data no concurrent round can shift and land atomically with the
// terminal state. Snapshots that cannot be such a transition return nil —
// open rounds' flags travel with the dataset commit, and any lost race there
// is repaired by the next round, which terminal MRs never get.
func (s *Syncer) terminalLivenessComputer(
	ctx context.Context,
	repo RepoRef,
	mr *db.MergeRequest,
) db.MREventMetadataComputer {
	if mr == nil || mr.State == db.MergeRequestStateOpen ||
		mr.PlatformHeadSHA == "" || s.clones == nil {
		return nil
	}
	return func(mrID int64, events []db.MREvent) map[string]string {
		return s.computeCommitLivenessForEvents(
			ctx, repo, mrID, mr.PlatformHeadSHA, nil, events,
		)
	}
}

// livenessHeadForRound picks the head against which a round computes commit
// liveness. Open MRs always compute against their current head. A round that
// takes an MR out of the open state computes once against the final head, so
// the flags it persists become the terminal record; rounds for
// already-terminal MRs return "" — merged and closed history is never
// refetched or recomputed.
func livenessHeadForRound(normalized, existing *db.MergeRequest) string {
	if normalized == nil {
		return ""
	}
	if normalized.State == db.MergeRequestStateOpen {
		return normalized.PlatformHeadSHA
	}
	if existing != nil && existing.State == db.MergeRequestStateOpen {
		return normalized.PlatformHeadSHA
	}
	return ""
}

type gitHubClientProvider struct {
	host         string
	client       Client
	viewerMu     sync.Mutex
	viewerLogins map[string]authenticatedViewerLoginCacheEntry
}

type authenticatedViewerLoginCacheEntry struct {
	login     string
	fetchedAt time.Time
}

type authenticatedViewerLoginClient interface {
	AuthenticatedViewerLogin(ctx context.Context) (string, error)
}

type authenticatedViewerCacheKeyClient interface {
	AuthenticatedViewerCacheKey() string
}

var errParentSnapshotAdvanced = errors.New("provider parent snapshot advanced during child refresh")

const authenticatedViewerLoginTTL = time.Hour

type githubLabelClient interface {
	ListRepoLabels(ctx context.Context, owner, repo string) ([]*gh.Label, error)
	ReplaceIssueLabels(ctx context.Context, owner, repo string, number int, names []string) ([]*gh.Label, error)
}

type githubAssigneeClient interface {
	ReplaceIssueAssignees(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.Issue, error)
}

type githubReviewerClient interface {
	RequestPullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.PullRequest, error)
	RemovePullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) error
}

type githubWorkflowCatalogClient interface {
	ListRepositoryWorkflows(context.Context, string, string) ([]*gh.Workflow, error)
	GetWorkflowDefinition(context.Context, string, string, string, string) (string, string, error)
	ListRepositoryEnvironments(context.Context, string, string) ([]*gh.Environment, error)
}

type githubWorkflowRunClient interface {
	ListManualWorkflowRuns(context.Context, string, string, int64, platform.WorkflowRunQuery) (platform.Page[*gh.WorkflowRun], error)
	ListManualWorkflowJobs(context.Context, string, string, int64) ([]*gh.WorkflowJob, error)
}

type githubWorkflowDispatchClient interface {
	DispatchManualWorkflow(context.Context, string, string, int64, gh.CreateWorkflowDispatchEventRequest) (*gh.WorkflowDispatchRunDetails, error)
}

func registryFromGitHubClients(clients map[string]Client) *platform.Registry {
	registry, err := platform.NewRegistry()
	if err != nil {
		panic(fmt.Sprintf("create empty provider registry: %v", err))
	}
	for host, client := range clients {
		if client == nil {
			continue
		}
		provider := &gitHubClientProvider{
			host:   canonicalRepoHost(host),
			client: client,
		}
		_ = registry.Register(provider)
	}
	return registry
}

func NewProviderRegistry(
	clients map[string]Client,
	providers ...platform.Provider,
) (*platform.Registry, error) {
	registry := registryFromGitHubClients(clients)
	for _, provider := range providers {
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (p *gitHubClientProvider) Platform() platform.Kind {
	return platform.KindGitHub
}

func (p *gitHubClientProvider) Host() string {
	return p.host
}

func (p *gitHubClientProvider) Capabilities() platform.Capabilities {
	_, labels := p.client.(githubLabelClient)
	_, assignees := p.client.(githubAssigneeClient)
	_, reviewers := p.client.(githubReviewerClient)
	_, workflows := p.client.(githubWorkflowCatalogClient)
	_, workflowRuns := p.client.(githubWorkflowRunClient)
	_, workflowDispatch := p.client.(githubWorkflowDispatchClient)
	_, archivePages := p.client.(pageClient)
	_, markdownImages := p.client.(markdownImageClient)
	_, directViewer := p.client.(authenticatedViewerLoginClient)
	_, routedViewer := p.client.(interface {
		AuthenticatedViewerLoginForRepo(context.Context, string, string) (string, error)
	})
	return platform.Capabilities{
		ReadRepositories:            true,
		ReadMergeRequests:           true,
		ReadIssues:                  true,
		ReadIssuePRReferences:       true,
		ReadComments:                true,
		ReadReleases:                true,
		ReadCI:                      true,
		ReadLabels:                  labels,
		ReadMarkdownImages:          markdownImages,
		ReadAuthenticatedUser:       directViewer || routedViewer,
		ReadNotifications:           true,
		ReadWorkflows:               workflows,
		ReadWorkflowRuns:            workflowRuns,
		CommentMutation:             true,
		StateMutation:               true,
		MergeMutation:               true,
		ReviewMutation:              true,
		MutationHeadBinding:         true,
		WorkflowApproval:            true,
		WorkflowDispatch:            workflowDispatch,
		ReadyForReview:              true,
		DraftMutation:               true,
		IssueMutation:               true,
		LabelMutation:               labels,
		AssigneeMutation:            assignees,
		ReviewerMutation:            reviewers,
		NotificationMutation:        true,
		ThreadReply:                 true,
		ReviewDraftMutation:         true,
		ReviewSuggestionApplication: true,
		ReadReviewThreads:           true,
		NativeMultilineRanges:       true,
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues:        archivePages,
			HistoricalMergeRequests: archivePages,
			OrdinaryComments:        archivePages,
			SubmittedReviews:        archivePages,
			InlineReviewComments:    archivePages,
		},
		SupportedReviewActions: []platform.ReviewAction{
			platform.ReviewActionComment,
			platform.ReviewActionApprove,
			platform.ReviewActionRequestChanges,
		},
	}
}

func (p *gitHubClientProvider) AuthenticatedUser(
	ctx context.Context,
	ref platform.RepoRef,
) (string, error) {
	return p.authenticatedViewerLoginForRepo(ctx, ref.Owner, ref.Name)
}

func (p *gitHubClientProvider) GetMarkdownImage(
	ctx context.Context,
	ref platform.RepoRef,
	sourceURL string,
) (platform.MarkdownImage, error) {
	reader, ok := p.client.(markdownImageClient)
	if !ok {
		return platform.MarkdownImage{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_markdown_images")
	}
	return reader.GetMarkdownImage(ctx, ref.Owner, ref.Name, sourceURL)
}

func (p *gitHubClientProvider) OperationRateLimitBuckets(
	operation platform.OperationName,
) ([]platform.RateLimitBucket, bool) {
	if operation != platform.OperationApplyReviewSuggestion {
		return nil, false
	}
	return []platform.RateLimitBucket{
		platform.RateLimitBucketREST,
		platform.RateLimitBucketGraphQL,
	}, true
}

func (p *gitHubClientProvider) GitHubClient() Client {
	return p.client
}

func (p *gitHubClientProvider) ViewerAuthoredMergeRequest(
	ctx context.Context,
	mr platform.MergeRequest,
) (bool, error) {
	author := strings.TrimSpace(mr.Author)
	if author == "" {
		return false, nil
	}
	viewer, err := p.authenticatedViewerLoginForRepo(
		ctx, mr.Repo.Owner, mr.Repo.Name,
	)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(viewer, author), nil
}

func (p *gitHubClientProvider) authenticatedViewerLoginForRepo(
	ctx context.Context, owner, name string,
) (string, error) {
	cacheKey := p.authenticatedViewerLookupKeyForRepo(owner, name)
	p.viewerMu.Lock()
	defer p.viewerMu.Unlock()
	if entry, ok := p.viewerLogins[cacheKey]; ok && time.Since(entry.fetchedAt) < authenticatedViewerLoginTTL {
		return entry.login, nil
	}

	var login string
	var err error
	if routed, ok := p.client.(interface {
		AuthenticatedViewerLoginForRepo(context.Context, string, string) (string, error)
	}); ok {
		login, err = routed.AuthenticatedViewerLoginForRepo(ctx, owner, name)
	} else {
		client, ok := p.client.(authenticatedViewerLoginClient)
		if !ok {
			return "", fmt.Errorf("github client does not resolve authenticated viewer")
		}
		login, err = client.AuthenticatedViewerLogin(ctx)
	}
	if err != nil {
		return "", err
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return "", fmt.Errorf("authenticated viewer login is empty")
	}
	if p.viewerLogins == nil {
		p.viewerLogins = make(map[string]authenticatedViewerLoginCacheEntry)
	}
	p.viewerLogins[cacheKey] = authenticatedViewerLoginCacheEntry{login: login, fetchedAt: time.Now()}
	return login, nil
}

func (p *gitHubClientProvider) AuthenticatedUserCacheKey(ref platform.RepoRef) string {
	return p.authenticatedViewerLookupKeyForRepo(ref.Owner, ref.Name)
}

func (p *gitHubClientProvider) authenticatedViewerLookupKeyForRepo(owner, name string) string {
	if cacheKey := p.authenticatedViewerCacheKeyForRepo(owner, name); cacheKey != "" {
		return cacheKey
	}
	return "repository:" + strings.ToLower(strings.TrimSpace(owner)) + "/" +
		strings.ToLower(strings.TrimSpace(name))
}

func (p *gitHubClientProvider) authenticatedViewerCacheKeyForRepo(owner, name string) string {
	if routed, ok := p.client.(interface {
		AuthenticatedViewerCacheKeyForRepo(string, string) string
	}); ok {
		return routed.AuthenticatedViewerCacheKeyForRepo(owner, name)
	}
	client, ok := p.client.(authenticatedViewerCacheKeyClient)
	if !ok {
		return ""
	}
	return client.AuthenticatedViewerCacheKey()
}

func (p *gitHubClientProvider) ListNotifications(
	ctx context.Context,
	opts platform.NotificationListOptions,
) ([]platform.NotificationThread, bool, error) {
	return p.client.ListNotifications(ctx, opts)
}

func (p *gitHubClientProvider) MarkNotificationThreadRead(
	ctx context.Context,
	threadID string,
) error {
	return p.client.MarkNotificationThreadRead(ctx, threadID)
}

func (p *gitHubClientProvider) GetNotificationThreadForRepo(
	ctx context.Context, owner, name, threadID string,
) (NotificationThread, error) {
	if routed, ok := p.client.(routedNotificationThreadGetter); ok {
		return routed.GetNotificationThreadForRepo(ctx, owner, name, threadID)
	}
	getter, ok := notificationThreadGetterFor(p.client)
	if !ok {
		return NotificationThread{}, fmt.Errorf(
			"github client does not fetch notification threads",
		)
	}
	return getter.GetNotificationThread(ctx, threadID)
}

func (p *gitHubClientProvider) MarkNotificationThreadReadForRepo(
	ctx context.Context, owner, name, threadID string,
) error {
	if routed, ok := p.client.(routedNotificationReadMarker); ok {
		return routed.MarkNotificationThreadReadForRepo(
			ctx, owner, name, threadID,
		)
	}
	return p.client.MarkNotificationThreadRead(ctx, threadID)
}

func (p *gitHubClientProvider) GetRepository(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	repo, err := p.client.GetRepository(ctx, ref.Owner, ref.Name)
	if err != nil {
		return platform.Repository{}, err
	}
	return gitHubPlatformRepository(p.host, ref.Owner, repo), nil
}

func workflowDefinitionReadMustAbort(err error) bool {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, platform.ErrRateLimited) {
		return true
	}
	if _, ok := errors.AsType[*gh.RateLimitError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*gh.AbuseRateLimitError](err); ok {
		return true
	}
	status := githubStatusCode(err)
	if status == http.StatusNotFound {
		return false
	}
	if status == http.StatusUnauthorized || status >= http.StatusInternalServerError {
		return true
	}
	_, transportFailure := errors.AsType[*url.Error](err)
	return transportFailure
}

func (p *gitHubClientProvider) ListManualWorkflows(
	ctx context.Context, ref platform.RepoRef,
) ([]platform.WorkflowDefinition, error) {
	client, ok := p.client.(githubWorkflowCatalogClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_workflows")
	}
	repo, err := p.client.GetRepository(ctx, ref.Owner, ref.Name)
	if err != nil {
		return nil, err
	}
	workflows, err := client.ListRepositoryWorkflows(ctx, ref.Owner, ref.Name)
	if err != nil {
		return nil, err
	}
	definitions := make([]platform.WorkflowDefinition, 0, len(workflows))
	for _, workflow := range workflows {
		if workflow == nil || workflow.GetState() != "active" {
			continue
		}
		content, sha, fileErr := client.GetWorkflowDefinition(
			ctx, ref.Owner, ref.Name, workflow.GetPath(), repo.GetDefaultBranch(),
		)
		if fileErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if workflowDefinitionReadMustAbort(fileErr) {
				return nil, fileErr
			}
			definitions = append(definitions, platform.WorkflowDefinition{
				ID: strconv.FormatInt(workflow.GetID(), 10), Name: workflow.GetName(),
				Path: workflow.GetPath(), State: workflow.GetState(), WebURL: workflow.GetHTMLURL(),
				Available: false, UnavailableReason: fileErr.Error(),
			})
			continue
		}
		definition, manual, parseErr := platformgithub.ParseManualWorkflow(
			workflow.GetName(), workflow.GetPath(), workflow.GetHTMLURL(), sha, []byte(content),
		)
		definition.ID = strconv.FormatInt(workflow.GetID(), 10)
		definition.State = workflow.GetState()
		if parseErr != nil {
			definition.Available = false
			definition.UnavailableReason = parseErr.Error()
			definitions = append(definitions, definition)
			continue
		}
		if manual {
			definitions = append(definitions, definition)
		}
	}
	return definitions, nil
}

func (p *gitHubClientProvider) ListWorkflowEnvironments(
	ctx context.Context, ref platform.RepoRef,
) ([]platform.WorkflowEnvironment, error) {
	client, ok := p.client.(githubWorkflowCatalogClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_workflows")
	}
	environments, err := client.ListRepositoryEnvironments(ctx, ref.Owner, ref.Name)
	if err != nil {
		return nil, err
	}
	result := make([]platform.WorkflowEnvironment, 0, len(environments))
	for _, environment := range environments {
		if environment != nil {
			result = append(result, platform.WorkflowEnvironment{Name: environment.GetName()})
		}
	}
	return result, nil
}

func parseGitHubWorkflowID(host, field, raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, &platform.Error{
			Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub,
			PlatformHost: host, Field: field, Err: fmt.Errorf("%s must be a positive decimal GitHub ID", field),
		}
	}
	return id, nil
}

func (p *gitHubClientProvider) ListWorkflowRuns(
	ctx context.Context, ref platform.RepoRef, query platform.WorkflowRunQuery,
) (platform.Page[platform.WorkflowRun], error) {
	client, ok := p.client.(githubWorkflowRunClient)
	if !ok {
		return platform.Page[platform.WorkflowRun]{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_workflow_runs")
	}
	workflowID, err := parseGitHubWorkflowID(p.host, "workflow_id", query.WorkflowID)
	if err != nil {
		return platform.Page[platform.WorkflowRun]{}, err
	}
	page, err := client.ListManualWorkflowRuns(ctx, ref.Owner, ref.Name, workflowID, query)
	if err != nil {
		return platform.Page[platform.WorkflowRun]{}, err
	}
	result := platform.Page[platform.WorkflowRun]{
		NextCursor: page.NextCursor,
		Exhausted:  page.Exhausted,
		Items:      make([]platform.WorkflowRun, 0, len(page.Items)),
	}
	for _, run := range page.Items {
		result.Items = append(result.Items, normalizeGitHubWorkflowRun(run))
	}
	return result, nil
}

func (p *gitHubClientProvider) ListWorkflowRunJobs(
	ctx context.Context, ref platform.RepoRef, rawRunID string,
) ([]platform.WorkflowRunJob, error) {
	client, ok := p.client.(githubWorkflowRunClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_workflow_runs")
	}
	runID, err := parseGitHubWorkflowID(p.host, "run_id", rawRunID)
	if err != nil {
		return nil, err
	}
	jobs, err := client.ListManualWorkflowJobs(ctx, ref.Owner, ref.Name, runID)
	if err != nil {
		return nil, err
	}
	result := make([]platform.WorkflowRunJob, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		normalized := platform.WorkflowRunJob{
			ID: strconv.FormatInt(job.GetID(), 10), Name: job.GetName(),
			Status: job.GetStatus(), Conclusion: job.GetConclusion(),
			StartedAt:   githubWorkflowTimestamp(job.StartedAt),
			CompletedAt: githubWorkflowTimestamp(job.CompletedAt), WebURL: job.GetHTMLURL(),
			Steps: make([]platform.WorkflowRunStep, 0, len(job.Steps)),
		}
		for _, step := range job.Steps {
			if step == nil {
				continue
			}
			normalized.Steps = append(normalized.Steps, platform.WorkflowRunStep{
				Number: int(step.GetNumber()), Name: step.GetName(), Status: step.GetStatus(),
				Conclusion: step.GetConclusion(), StartedAt: githubWorkflowTimestamp(step.StartedAt),
				CompletedAt: githubWorkflowTimestamp(step.CompletedAt),
			})
		}
		result = append(result, normalized)
	}
	return result, nil
}

func (p *gitHubClientProvider) DispatchWorkflow(
	ctx context.Context, ref platform.RepoRef, request platform.WorkflowDispatchRequest,
) (platform.WorkflowDispatchResult, error) {
	client, ok := p.client.(githubWorkflowDispatchClient)
	if !ok {
		return platform.WorkflowDispatchResult{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "workflow_dispatch")
	}
	workflowID, err := parseGitHubWorkflowID(p.host, "workflow_id", request.WorkflowID)
	if err != nil {
		return platform.WorkflowDispatchResult{}, err
	}
	actor, _ := p.AuthenticatedUser(ctx, ref)
	result := platform.WorkflowDispatchResult{Actor: actor}
	details, err := client.DispatchManualWorkflow(ctx, ref.Owner, ref.Name, workflowID, gh.CreateWorkflowDispatchEventRequest{
		Ref: request.Ref, Inputs: request.Inputs,
	})
	if err != nil {
		return result, err
	}
	result.Accepted = true
	if details != nil && details.GetWorkflowRunID() != 0 {
		run := platform.WorkflowRun{
			ID:         strconv.FormatInt(details.GetWorkflowRunID(), 10),
			WorkflowID: request.WorkflowID, Actor: actor, WebURL: details.GetHTMLURL(),
		}
		result.Run = &run
	}
	return result, nil
}

func normalizeGitHubWorkflowRun(run *gh.WorkflowRun) platform.WorkflowRun {
	if run == nil {
		return platform.WorkflowRun{}
	}
	return platform.WorkflowRun{
		ID: strconv.FormatInt(run.GetID(), 10), WorkflowID: strconv.FormatInt(run.GetWorkflowID(), 10),
		RunNumber: int64(run.GetRunNumber()), Name: run.GetName(), Event: run.GetEvent(),
		Ref: run.GetHeadBranch(), HeadSHA: run.GetHeadSHA(), Actor: run.GetActor().GetLogin(),
		Status: run.GetStatus(), Conclusion: run.GetConclusion(),
		CreatedAt: githubWorkflowTimestamp(run.CreatedAt), UpdatedAt: githubWorkflowTimestamp(run.UpdatedAt),
		WebURL: run.GetHTMLURL(),
	}
}

func githubWorkflowTimestamp(timestamp *gh.Timestamp) time.Time {
	if timestamp == nil {
		return time.Time{}
	}
	return timestamp.UTC()
}

// gitHubPlatformRepository converts a GitHub REST repository into the
// provider-neutral snapshot, preferring the canonical owner the provider
// reports over the requested route owner.
func gitHubPlatformRepository(
	host, requestedOwner string, repo *gh.Repository,
) platform.Repository {
	owner := requestedOwner
	if repo.GetOwner().GetLogin() != "" {
		owner = repo.GetOwner().GetLogin()
	}
	viewerCanMerge := gitHubViewerCanMerge(repo)
	var mergeSettings *platform.RepositoryMergeSettings
	if gitHubMergeSettingsComplete(repo) {
		mergeSettings = &platform.RepositoryMergeSettings{
			AllowSquashMerge: repo.GetAllowSquashMerge(),
			AllowMergeCommit: repo.GetAllowMergeCommit(),
			AllowRebaseMerge: repo.GetAllowRebaseMerge(),
		}
	}
	return platform.Repository{
		Ref: platform.RepoRef{
			Platform:           platform.KindGitHub,
			Host:               host,
			Owner:              canonicalRepoOwner(owner),
			Name:               canonicalRepoName(repo.GetName()),
			RepoPath:           canonicalRepoOwner(owner) + "/" + canonicalRepoName(repo.GetName()),
			PlatformID:         repo.GetID(),
			PlatformExternalID: repo.GetNodeID(),
			WebURL:             repo.GetHTMLURL(),
			CloneURL:           repo.GetCloneURL(),
			DefaultBranch:      repo.GetDefaultBranch(),
		},
		PlatformID:         repo.GetID(),
		PlatformExternalID: repo.GetNodeID(),
		Description:        repo.GetDescription(),
		Private:            repo.GetPrivate(),
		Archived:           repo.GetArchived(),
		MergeSettings:      mergeSettings,
		ViewerCanMerge:     viewerCanMerge,
		DefaultBranch:      repo.GetDefaultBranch(),
		WebURL:             repo.GetHTMLURL(),
		CloneURL:           repo.GetCloneURL(),
	}
}

func gitHubViewerCanMerge(repo *gh.Repository) *bool {
	if repo == nil || repo.Permissions == nil {
		return nil
	}
	canMerge := repo.Permissions.GetPush() ||
		repo.Permissions.GetMaintain() ||
		repo.Permissions.GetAdmin()
	return &canMerge
}

func (p *gitHubClientProvider) ListRepositories(
	ctx context.Context,
	owner string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos, err := p.client.ListRepositoriesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]platform.Repository, 0, len(repos))
	for _, repo := range repos {
		repoOwner := owner
		if repo.GetOwner().GetLogin() != "" {
			repoOwner = repo.GetOwner().GetLogin()
		}
		repoName := repo.GetName()
		out = append(out, platform.Repository{
			Ref: platform.RepoRef{
				Platform:           platform.KindGitHub,
				Host:               p.host,
				Owner:              canonicalRepoOwner(repoOwner),
				Name:               canonicalRepoName(repoName),
				RepoPath:           canonicalRepoOwner(repoOwner) + "/" + canonicalRepoName(repoName),
				PlatformID:         repo.GetID(),
				PlatformExternalID: repo.GetNodeID(),
				WebURL:             repo.GetHTMLURL(),
				CloneURL:           repo.GetCloneURL(),
				DefaultBranch:      repo.GetDefaultBranch(),
			},
			PlatformID:         repo.GetID(),
			PlatformExternalID: repo.GetNodeID(),
			Description:        repo.GetDescription(),
			Private:            repo.GetPrivate(),
			Archived:           repo.GetArchived(),
			DefaultBranch:      repo.GetDefaultBranch(),
			WebURL:             repo.GetHTMLURL(),
			CloneURL:           repo.GetCloneURL(),
		})
	}
	return out, nil
}

// mergeRequestsDisabledByRepository classifies a pulls-list 404 against the
// repository record. GitHub issues-only repositories report
// has_pull_requests=false and return a bare 404 from the pulls API for every
// credential, so without this probe the sync retries the repo as a hard
// failure every cycle and never reaches its issue phase. A repository that
// cannot be read, or that does not report the field, keeps the original
// error: only an explicit has_pull_requests=false is proof.
func (p *gitHubClientProvider) mergeRequestsDisabledByRepository(
	ctx context.Context,
	ref platform.RepoRef,
	err error,
) error {
	if githubStatusCode(err) != http.StatusNotFound {
		return nil
	}
	repo, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name)
	if repoErr != nil || repo == nil || repo.HasPullRequests == nil ||
		repo.GetHasPullRequests() {
		return nil
	}
	return platform.RepositoryFeatureDisabled(
		platform.KindGitHub, p.host, platform.RepositoryFeatureMergeRequests, err,
	)
}

func (p *gitHubClientProvider) ListOpenMergeRequests(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	prs, err := p.client.ListOpenPullRequests(ctx, ref.Owner, ref.Name)
	if err != nil {
		if disabledErr := githubRepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureMergeRequests, err,
		); disabledErr != nil {
			return nil, disabledErr
		}
		if disabledErr := p.mergeRequestsDisabledByRepository(ctx, ref, err); disabledErr != nil {
			return nil, disabledErr
		}
		return nil, err
	}
	out := make([]platform.MergeRequest, 0, len(prs))
	for _, pr := range prs {
		mr, err := platformgithub.NormalizePullRequest(ref, pr)
		if err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, nil
}

func (p *gitHubClientProvider) ListOpenMergeRequestsWithNativeStackHints(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, map[int]*NativeStackHint, error) {
	nativeClient, ok := p.client.(NativeStackClient)
	if !ok {
		mrs, err := p.ListOpenMergeRequests(ctx, ref)
		return mrs, nil, err
	}
	prs, hints, err := nativeClient.ListOpenPullRequestsWithNativeStackHints(
		ctx, ref.Owner, ref.Name,
	)
	if err != nil {
		// Same classification as ListOpenMergeRequests: a repository with pull
		// requests disabled must enter the feature cooldown, not be retried as a
		// hard failure every cycle just because the preview is enabled.
		if disabledErr := githubRepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureMergeRequests, err,
		); disabledErr != nil {
			return nil, nil, disabledErr
		}
		if disabledErr := p.mergeRequestsDisabledByRepository(ctx, ref, err); disabledErr != nil {
			return nil, nil, disabledErr
		}
		return nil, nil, err
	}
	out := make([]platform.MergeRequest, 0, len(prs))
	for _, pr := range prs {
		mr, err := platformgithub.NormalizePullRequest(ref, pr)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, mr)
	}
	return out, hints, nil
}

func (p *gitHubClientProvider) GetMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	_, mr, err := p.GetGitHubPullRequest(ctx, ref, number)
	return mr, err
}

func (p *gitHubClientProvider) GetGitHubPullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (*gh.PullRequest, platform.MergeRequest, error) {
	pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
	// The optimized detail path needs the full SDK object, so it fetches
	// raw; the failure and transfer outcomes still route through the one
	// canonical lookup classification.
	if outcomeErr := p.mergeRequestLookupOutcomeError(ctx, ref, number, pr, err); outcomeErr != nil {
		return nil, platform.MergeRequest{}, outcomeErr
	}
	mr, err := platformgithub.NormalizePullRequest(ref, pr)
	if err != nil {
		return nil, platform.MergeRequest{}, err
	}
	return pr, mr, nil
}

func (p *gitHubClientProvider) ListMergeRequestEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	comments, err := p.client.ListIssueComments(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	reviews, err := p.client.ListReviews(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	commits, err := p.client.ListCommits(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	timelineEvents, err := p.client.ListPullRequestTimelineEvents(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		slog.Warn("github provider timeline event fetch failed",
			"repo", ref.DisplayName(),
			"number", number,
			"err", err,
		)
		timelineEvents = nil
	}

	out := make([]platform.MergeRequestEvent, 0, len(comments)+len(reviews)+len(commits)+len(timelineEvents))
	for _, comment := range comments {
		out = append(out, platformgithub.NormalizeCommentEvent(ref, number, comment))
	}
	for _, review := range reviews {
		out = append(out, platformgithub.NormalizeReviewEvent(ref, number, review))
	}
	for i, commit := range commits {
		event := platformgithub.NormalizeCommitEvent(ref, number, commit)
		event.MetadataJSON = withCommitOrderMetadata(event.MetadataJSON, i+1, i+1)
		out = append(out, event)
	}
	for _, timelineEvent := range timelineEvents {
		event := platformgithub.NormalizeTimelineEvent(ref, number, platformgithub.PullRequestTimelineEvent{
			NodeID:               timelineEvent.NodeID,
			EventType:            timelineEvent.EventType,
			Actor:                timelineEvent.Actor,
			Assignee:             timelineEvent.Assignee,
			CreatedAt:            timelineEvent.CreatedAt,
			DeletedCommentAuthor: timelineEvent.DeletedCommentAuthor,
			BeforeSHA:            timelineEvent.BeforeSHA,
			AfterSHA:             timelineEvent.AfterSHA,
			Ref:                  timelineEvent.Ref,
			PreviousTitle:        timelineEvent.PreviousTitle,
			CurrentTitle:         timelineEvent.CurrentTitle,
			PreviousRefName:      timelineEvent.PreviousRefName,
			CurrentRefName:       timelineEvent.CurrentRefName,
			SourceType:           timelineEvent.SourceType,
			SourceOwner:          timelineEvent.SourceOwner,
			SourceRepo:           timelineEvent.SourceRepo,
			SourceNumber:         timelineEvent.SourceNumber,
			SourceTitle:          timelineEvent.SourceTitle,
			SourceURL:            timelineEvent.SourceURL,
			IsCrossRepository:    timelineEvent.IsCrossRepository,
			WillCloseTarget:      timelineEvent.WillCloseTarget,
		})
		if event != nil {
			out = append(out, *event)
		}
	}
	return out, nil
}

// lookupNotPresentError renders the typed error a live caller receives when a
// single-item lookup resolves to a non-present outcome. Live callers require
// present; archive callers inspect the outcome instead. The outcomes must not
// collapse: removed is not_found, inaccessible is permission_denied (the
// behavior a raw 403 produced before lookup classification), and moved is
// not_found carrying the destination repository so callers can retarget the
// reference.
func (p *gitHubClientProvider) lookupNotPresentError(
	ref platform.RepoRef,
	number int,
	outcome lookupOutcome,
	destination *platform.RepoRef,
) error {
	code := platform.ErrCodeNotFound
	cause := fmt.Errorf("%s#%d is not present (%s)", ref.DisplayName(), number, outcome)
	if outcome == lookupInaccessible {
		code = platform.ErrCodePermissionDenied
		cause = errors.Join(platform.ErrLookupInaccessible, cause)
	} else {
		cause = errors.Join(platform.ErrLookupNotPresent, cause)
	}
	return &platform.Error{
		Code:         code,
		Provider:     platform.KindGitHub,
		PlatformHost: p.host,
		Destination:  destination,
		Err:          cause,
	}
}

// issueFetchOutcomeError routes a raw single-issue fetch result through the
// canonical lookup classification when the repo's registered reader is the
// GitHub provider. A nil return means no outcome mapping applies (present
// item, non-GitHub reader, or a nil conditional result).
func (s *Syncer) issueFetchOutcomeError(
	ctx context.Context,
	repo RepoRef,
	number int,
	issue *gh.Issue,
	err error,
) error {
	reader, readerErr := s.issueReaderFor(repo)
	if readerErr != nil {
		return nil
	}
	provider, ok := reader.(*gitHubClientProvider)
	if !ok {
		return nil
	}
	return provider.issueLookupOutcomeError(ctx, platformRepoRef(repo), number, issue, err)
}

// issueOnlyFetchOutcomeError adds the GitHub Issues API's PR-shape
// classification for callers that require an issue. Kind-dispatching callers
// such as SyncItemByNumber use issueFetchOutcomeError directly so they can
// route a PR-shaped response to merge-request sync instead.
func (s *Syncer) issueOnlyFetchOutcomeError(
	ctx context.Context,
	repo RepoRef,
	number int,
	issue *gh.Issue,
	err error,
) error {
	if outcomeErr := s.issueFetchOutcomeError(ctx, repo, number, issue, err); outcomeErr != nil {
		return outcomeErr
	}
	reader, readerErr := s.issueReaderFor(repo)
	if readerErr != nil {
		return nil
	}
	provider, ok := reader.(*gitHubClientProvider)
	if !ok {
		return nil
	}
	return provider.issuePullRequestOutcomeError(platformRepoRef(repo), number, issue)
}

// mergeRequestFetchOutcomeError is the merge-request counterpart to
// issueFetchOutcomeError.
func (s *Syncer) mergeRequestFetchOutcomeError(
	ctx context.Context,
	repo RepoRef,
	number int,
	pr *gh.PullRequest,
	err error,
) error {
	reader, readerErr := s.mergeRequestReaderFor(repo)
	if readerErr != nil {
		return nil
	}
	provider, ok := reader.(*gitHubClientProvider)
	if !ok {
		return nil
	}
	return provider.mergeRequestLookupOutcomeError(ctx, platformRepoRef(repo), number, pr, err)
}

// ListOpenGitHubIssues is the raw ETag-gated open-issue bulk read backing both
// the canonical listOpenIssuesPage normalization and the optimized GitHub
// index sync consumer. Because the raw slice is consumed without passing
// through the validating canonical reader wrapper, this method applies the
// equivalent contract checks itself, so no caller ever observes an unvalidated
// bulk observation.
func (p *gitHubClientProvider) ListOpenGitHubIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]*gh.Issue, error) {
	issues, err := p.client.ListOpenIssues(ctx, ref.Owner, ref.Name)
	if err != nil {
		if disabledErr := githubRepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureIssues, err,
		); disabledErr != nil {
			return nil, disabledErr
		}
		return nil, err
	}
	if err := p.validateOpenIssuesContract(ref, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func (p *gitHubClientProvider) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	issues, err := p.ListOpenGitHubIssues(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]platform.Issue, 0, len(issues))
	for _, issue := range issues {
		normalized, err := platformgithub.NormalizeIssue(ref, issue)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

// validateOpenIssuesContract applies the contract checks the validating
// canonical reader wrapper would run on a normalized open-issue page to the
// raw bulk result: items must be non-nil, item numbers positive and unique
// within the single exhausted open list, and every item bound to the
// requested repository. Monotonic-order checks do not apply because the open
// scan leaves traversal order contractually unspecified. Violations are typed
// provider contract errors so consumers reject the whole list instead of
// persisting from it.
func (p *gitHubClientProvider) validateOpenIssuesContract(
	ref platform.RepoRef,
	issues []*gh.Issue,
) error {
	seen := make(map[int]bool, len(issues))
	for _, issue := range issues {
		if issue == nil {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item",
				fmt.Errorf("provider returned a nil issue in the open list for %s", ref.DisplayName()),
			)
		}
		number := issue.GetNumber()
		if number <= 0 {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_number",
				fmt.Errorf("provider returned nonpositive issue number %d", number),
			)
		}
		if seen[number] {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_number",
				fmt.Errorf("provider returned duplicate issue number %d in one open list", number),
			)
		}
		seen[number] = true
		if destination := githubArchiveDestination(ref, issue.GetRepositoryURL()); destination != nil {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_repo",
				fmt.Errorf(
					"provider returned issue %d bound to repository %s for requested %s",
					number, destination.RepoPath, ref.RepoPath,
				),
			)
		}
	}
	return nil
}

func (p *gitHubClientProvider) ListLabels(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.LabelCatalog, error) {
	client, ok := p.client.(githubLabelClient)
	if !ok {
		return platform.LabelCatalog{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_labels")
	}
	labels, err := client.ListRepoLabels(ctx, ref.Owner, ref.Name)
	if err != nil {
		return platform.LabelCatalog{}, err
	}
	return platform.LabelCatalog{Labels: platformgithub.NormalizeLabels(ref, labels)}, nil
}

func (p *gitHubClientProvider) GetGitHubIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (*gh.Issue, error) {
	issue, err := p.client.GetIssue(ctx, ref.Owner, ref.Name, number)
	// Raw fetch for the optimized detail path; outcomes still route
	// through the one canonical lookup classification.
	if outcomeErr := p.issueLookupOutcomeError(ctx, ref, number, issue, err); outcomeErr != nil {
		return nil, outcomeErr
	}
	if outcomeErr := p.issuePullRequestOutcomeError(ref, number, issue); outcomeErr != nil {
		return nil, outcomeErr
	}
	return issue, nil
}

func (p *gitHubClientProvider) GetIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.Issue, error) {
	issue, err := p.GetGitHubIssue(ctx, ref, number)
	if err != nil {
		return platform.Issue{}, err
	}
	return platformgithub.NormalizeIssue(ref, issue)
}

func (p *gitHubClientProvider) ListIssueEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	comments, err := p.client.ListIssueComments(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	var timelineEvents []PullRequestTimelineEvent
	if timelineClient, ok := p.client.(issueTimelineLister); ok {
		timelineEvents, err = timelineClient.ListIssueTimelineEvents(ctx, ref.Owner, ref.Name, number)
		if err != nil {
			slog.Warn("github provider issue timeline event fetch failed",
				"repo", ref.DisplayName(),
				"number", number,
				"err", err,
			)
			timelineEvents = nil
		}
	}

	out := make([]platform.IssueEvent, 0, len(comments)+len(timelineEvents))
	for _, comment := range comments {
		out = append(out, platformgithub.NormalizeIssueCommentEvent(ref, number, comment))
	}
	for _, timelineEvent := range timelineEvents {
		event := platformgithub.NormalizeIssueTimelineEvent(ref, number, platformgithub.PullRequestTimelineEvent{
			NodeID:            timelineEvent.NodeID,
			EventType:         timelineEvent.EventType,
			Actor:             timelineEvent.Actor,
			Assignee:          timelineEvent.Assignee,
			CreatedAt:         timelineEvent.CreatedAt,
			SourceType:        timelineEvent.SourceType,
			SourceOwner:       timelineEvent.SourceOwner,
			SourceRepo:        timelineEvent.SourceRepo,
			SourceNumber:      timelineEvent.SourceNumber,
			SourceTitle:       timelineEvent.SourceTitle,
			SourceURL:         timelineEvent.SourceURL,
			IsCrossRepository: timelineEvent.IsCrossRepository,
			WillCloseTarget:   timelineEvent.WillCloseTarget,
		})
		if event != nil {
			out = append(out, *event)
		}
	}
	return out, nil
}

func (p *gitHubClientProvider) CreateMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.MergeRequestEvent, error) {
	comment, err := p.client.CreateIssueComment(ctx, ref.Owner, ref.Name, number, body)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no comment")
	}
	return platformgithub.NormalizeCommentEvent(ref, number, comment), nil
}

func (p *gitHubClientProvider) EditMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.MergeRequestEvent, error) {
	comment, err := p.client.EditIssueComment(ctx, ref.Owner, ref.Name, commentID, body)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no comment")
	}
	return platformgithub.NormalizeCommentEvent(ref, number, comment), nil
}

func (p *gitHubClientProvider) DeleteMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	return p.client.DeleteIssueComment(ctx, ref.Owner, ref.Name, commentID)
}

func (p *gitHubClientProvider) ReplyToThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	threadID string,
	body string,
) (platform.MergeRequestEvent, error) {
	commentID, err := strconv.ParseInt(strings.TrimSpace(threadID), 10, 64)
	if err != nil || commentID <= 0 {
		return platform.MergeRequestEvent{}, fmt.Errorf("invalid review comment ID")
	}
	comment, err := p.client.CreatePullRequestReviewCommentReply(
		ctx, ref.Owner, ref.Name, number, body, commentID,
	)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no review comment")
	}
	return platformgithub.NormalizeReviewCommentEvent(ref, number, comment), nil
}

func (p *gitHubClientProvider) CreateIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.IssueEvent, error) {
	comment, err := p.client.CreateIssueComment(ctx, ref.Owner, ref.Name, number, body)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	if comment == nil {
		return platform.IssueEvent{}, fmt.Errorf("provider returned no comment")
	}
	return platformgithub.NormalizeIssueCommentEvent(ref, number, comment), nil
}

func (p *gitHubClientProvider) EditIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.IssueEvent, error) {
	comment, err := p.client.EditIssueComment(ctx, ref.Owner, ref.Name, commentID, body)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	if comment == nil {
		return platform.IssueEvent{}, fmt.Errorf("provider returned no comment")
	}
	return platformgithub.NormalizeIssueCommentEvent(ref, number, comment), nil
}

func (p *gitHubClientProvider) DeleteIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	return p.client.DeleteIssueComment(ctx, ref.Owner, ref.Name, commentID)
}

func (p *gitHubClientProvider) SetMergeRequestState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.MergeRequest, error) {
	ghPR, err := p.client.EditPullRequest(
		ctx, ref.Owner, ref.Name, number, EditPullRequestOpts{State: &state},
	)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if ghPR == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return platformgithub.NormalizePullRequest(ref, ghPR)
}

func (p *gitHubClientProvider) SetIssueState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.Issue, error) {
	ghIssue, err := p.client.EditIssue(ctx, ref.Owner, ref.Name, number, state)
	if err != nil {
		return platform.Issue{}, err
	}
	if ghIssue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return platformgithub.NormalizeIssue(ref, ghIssue)
}

// MergeMergeRequest passes expectedHeadSHA as the GitHub merge sha
// parameter: GitHub rejects the merge when the PR head moved past the
// reviewed commit, and that rejection is classified as stale_state.
func (p *gitHubClientProvider) MergeMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commitTitle string,
	commitMessage string,
	method string,
	expectedHeadSHA string,
) (platform.MergeResult, error) {
	result, err := p.client.MergePullRequest(
		ctx, ref.Owner, ref.Name, number, commitTitle, commitMessage, method, expectedHeadSHA,
	)
	if err != nil {
		if expectedHeadSHA != "" && isGitHubHeadModified(err) {
			return platform.MergeResult{}, &platform.Error{
				Code:         platform.ErrCodeStaleState,
				Provider:     platform.KindGitHub,
				PlatformHost: p.host,
				Capability:   "merge_merge_request",
				Err:          err,
			}
		}
		return platform.MergeResult{}, err
	}
	if result == nil {
		return platform.MergeResult{}, fmt.Errorf("provider returned no merge result")
	}
	return platform.MergeResult{
		Merged:  result.GetMerged(),
		SHA:     result.GetSHA(),
		Message: result.GetMessage(),
	}, nil
}

// isGitHubHeadModified reports whether a GitHub merge rejection is the
// sha-mismatch refusal ("Head branch was modified. Review and try the
// merge again.").
func isGitHubHeadModified(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr == nil || ghErr.Response == nil {
		return false
	}
	if ghErr.Response.StatusCode != http.StatusConflict &&
		ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return strings.Contains(strings.ToLower(ghErr.Message), "head branch was modified")
}

func (p *gitHubClientProvider) ApproveWorkflow(
	ctx context.Context,
	ref platform.RepoRef,
	runID string,
) error {
	parsed, err := parseInt64(runID)
	if err != nil {
		return err
	}
	return p.client.ApproveWorkflowRun(ctx, ref.Owner, ref.Name, parsed)
}

func (p *gitHubClientProvider) MarkReadyForReview(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	pr, err := p.client.MarkPullRequestReadyForReview(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if pr == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return platformgithub.NormalizePullRequest(ref, pr)
}

func (p *gitHubClientProvider) ConvertMergeRequestToDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (time.Time, error) {
	pr, err := p.client.ConvertPullRequestToDraft(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return time.Time{}, err
	}
	if pr == nil {
		return time.Time{}, fmt.Errorf("provider returned no pull request")
	}
	if pr.UpdatedAt == nil || pr.UpdatedAt.IsZero() {
		return time.Time{}, fmt.Errorf("provider returned pull request without updated time")
	}
	return pr.UpdatedAt.UTC(), nil
}

func (p *gitHubClientProvider) CreateIssue(
	ctx context.Context,
	ref platform.RepoRef,
	title string,
	body string,
) (platform.Issue, error) {
	issue, err := p.client.CreateIssue(ctx, ref.Owner, ref.Name, title, body)
	if err != nil {
		return platform.Issue{}, err
	}
	if issue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return platformgithub.NormalizeIssue(ref, issue)
}

func (p *gitHubClientProvider) SetMergeRequestLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	return p.setIssueLikeLabels(ctx, ref, number, names)
}

func (p *gitHubClientProvider) SetIssueLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	return p.setIssueLikeLabels(ctx, ref, number, names)
}

func (p *gitHubClientProvider) setIssueLikeLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	client, ok := p.client.(githubLabelClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "label_mutation")
	}
	labels, err := client.ReplaceIssueLabels(ctx, ref.Owner, ref.Name, number, names)
	if err != nil {
		return nil, err
	}
	return platformgithub.NormalizeLabels(ref, labels), nil
}

func (p *gitHubClientProvider) SetMergeRequestAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return p.setIssueLikeAssignees(ctx, ref, number, usernames)
}

func (p *gitHubClientProvider) SetIssueAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return p.setIssueLikeAssignees(ctx, ref, number, usernames)
}

func (p *gitHubClientProvider) setIssueLikeAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	client, ok := p.client.(githubAssigneeClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "assignee_mutation")
	}
	issue, err := client.ReplaceIssueAssignees(ctx, ref.Owner, ref.Name, number, usernames)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("provider returned no issue")
	}
	assignees := make([]string, 0, len(issue.Assignees))
	for _, user := range issue.Assignees {
		if user.GetLogin() != "" {
			assignees = append(assignees, user.GetLogin())
		}
	}
	return assignees, nil
}

func (p *gitHubClientProvider) RequestMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	client, ok := p.client.(githubReviewerClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "reviewer_mutation")
	}
	if len(usernames) == 0 {
		// An empty request is the interface's read primitive: report
		// the provider's current requested-reviewer set untouched.
		pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
		if err != nil {
			return nil, err
		}
		if pr == nil {
			return nil, fmt.Errorf("provider returned no pull request")
		}
		return githubRequestedReviewerLogins(pr), nil
	}
	pr, err := client.RequestPullRequestReviewers(ctx, ref.Owner, ref.Name, number, usernames)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("provider returned no pull request")
	}
	return githubRequestedReviewerLogins(pr), nil
}

func (p *gitHubClientProvider) RemoveMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	client, ok := p.client.(githubReviewerClient)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "reviewer_mutation")
	}
	if err := client.RemovePullRequestReviewers(ctx, ref.Owner, ref.Name, number, usernames); err != nil {
		return nil, err
	}
	// The removal endpoint has no useful body; re-read the pull request
	// for the authoritative requested-reviewer set.
	pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("provider returned no pull request")
	}
	return githubRequestedReviewerLogins(pr), nil
}

func githubRequestedReviewerLogins(pr *gh.PullRequest) []string {
	logins := make([]string, 0, len(pr.RequestedReviewers))
	for _, user := range pr.RequestedReviewers {
		if user.GetLogin() != "" {
			logins = append(logins, user.GetLogin())
		}
	}
	return logins
}

func (p *gitHubClientProvider) ApproveMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) (platform.MergeRequestEvent, error) {
	review, err := p.client.CreateReviewWithComments(
		ctx,
		ref.Owner,
		ref.Name,
		number,
		"APPROVE",
		body,
		expectedHeadSHA,
		nil,
	)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if review == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no review")
	}
	return platformgithub.NormalizeReviewEvent(ref, number, review), nil
}

// RequestChanges submits a blocking review with exactly the head-binding
// contract of ApproveMergeRequest: the pin is forwarded as the review
// commit and GitHub attaches the review to it. No client-side head
// verification or post-submit revocation is layered on top — a change
// request from the review form must not carry a stronger submission
// contract than an approval from the same form.
func (p *gitHubClientProvider) RequestChanges(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) error {
	review, err := p.client.CreateReviewWithComments(
		ctx, ref.Owner, ref.Name, number, "REQUEST_CHANGES", body, expectedHeadSHA, nil,
	)
	if err != nil {
		return err
	}
	if review == nil {
		return fmt.Errorf("provider returned no review")
	}
	return nil
}

func (p *gitHubClientProvider) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	threads, err := p.client.ListPullRequestReviewThreads(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	out := make([]platform.MergeRequestReviewThread, 0, len(threads))
	for _, thread := range threads {
		if len(thread.Comments) == 0 {
			continue
		}
		for _, comment := range thread.Comments {
			normalized := githubReviewThreadComment(thread, comment)
			if normalized.ProviderThreadID == "" || normalized.ProviderCommentID == "" {
				continue
			}
			out = append(out, normalized)
		}
	}
	return out, nil
}

func githubReviewThreadComment(
	thread PullRequestReviewThread,
	comment PullRequestReviewThreadComment,
) platform.MergeRequestReviewThread {
	createdAt := comment.CreatedAt.UTC()
	updatedAt := comment.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	return platform.MergeRequestReviewThread{
		ProviderThreadID:  thread.NodeID,
		ProviderReviewID:  githubInt64ID(comment.ReviewDatabaseID),
		ProviderCommentID: firstNonEmpty(githubInt64ID(comment.DatabaseID), comment.NodeID),
		Body:              comment.Body,
		AuthorLogin:       comment.AuthorLogin,
		DirectURL:         comment.URL,
		Range:             githubReviewLineRange(thread, comment),
		Resolved:          thread.IsResolved,
		MetadataJSON: normalizeCommentVisibilityMetadata(CommentVisibility{
			Hidden: comment.IsMinimized, Reason: comment.MinimizedReason,
		}),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func githubReviewLineRange(
	thread PullRequestReviewThread,
	comment PullRequestReviewThreadComment,
) platform.DiffReviewLineRange {
	side := strings.ToLower(thread.Side)
	if side != "left" {
		side = "right"
	}
	line := firstPositive(thread.Line, thread.OriginalLine, comment.Line, comment.OriginalLine)
	startLine := thread.StartLine
	if startLine == nil {
		startLine = thread.OriginalStartLine
	}
	lineType := "add"
	var oldLine *int
	var newLine *int
	if strings.EqualFold(comment.SubjectType, "FILE") {
		lineType = "file"
	} else if side == "left" {
		lineType = "delete"
		oldLine = &line
	} else {
		newLine = &line
	}
	commitSHA := firstNonEmpty(comment.CommitID, comment.OriginalCommitID)
	return platform.DiffReviewLineRange{
		Path:        firstNonEmpty(thread.Path, comment.Path),
		Side:        side,
		StartSide:   githubReviewStartSide(side, startLine),
		StartLine:   startLine,
		Line:        line,
		OldLine:     oldLine,
		NewLine:     newLine,
		LineType:    lineType,
		DiffHeadSHA: commitSHA,
		CommitSHA:   commitSHA,
	}
}

func githubReviewStartSide(side string, startLine *int) string {
	if startLine == nil {
		return ""
	}
	return side
}

func githubInt64ID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (p *gitHubClientProvider) PublishDiffReviewDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.PublishDiffReviewDraftInput,
) (*platform.PublishedDiffReview, error) {
	event, err := githubReviewEvent(input.Action)
	if err != nil {
		return nil, err
	}
	comments := make([]*gh.DraftReviewComment, 0, len(input.Comments))
	for _, comment := range input.Comments {
		comments = append(comments, githubDraftReviewComment(comment))
	}
	headSHA := githubReviewHeadSHA(input)
	review, err := p.client.CreateReviewWithComments(
		ctx,
		ref.Owner,
		ref.Name,
		number,
		event,
		input.Body,
		headSHA,
		comments,
	)
	if err != nil {
		return nil, err
	}
	if review == nil {
		return nil, fmt.Errorf("provider returned no review")
	}
	submittedAt := review.GetSubmittedAt() // zero Timestamp when GitHub omits submitted_at
	return &platform.PublishedDiffReview{
		ProviderReviewID: strconv.FormatInt(review.GetID(), 10),
		SubmittedAt:      submittedAt.Time,
	}, nil
}

func (p *gitHubClientProvider) ApplyReviewSuggestions(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	return p.client.ApplyReviewSuggestions(ctx, ref.Owner, ref.Name, number, input)
}

func githubReviewEvent(action platform.ReviewAction) (string, error) {
	switch action {
	case platform.ReviewActionComment:
		return "COMMENT", nil
	case platform.ReviewActionApprove:
		return "APPROVE", nil
	case platform.ReviewActionRequestChanges:
		return "REQUEST_CHANGES", nil
	default:
		return "", fmt.Errorf("unsupported github review action %q", action)
	}
}

func githubReviewHeadSHA(input platform.PublishDiffReviewDraftInput) string {
	if input.HeadSHA != "" {
		return input.HeadSHA
	}
	for _, comment := range input.Comments {
		if comment.Range.DiffHeadSHA != "" {
			return comment.Range.DiffHeadSHA
		}
		if comment.Range.CommitSHA != "" {
			return comment.Range.CommitSHA
		}
	}
	return ""
}

func githubDraftReviewComment(comment platform.LocalDiffReviewDraftComment) *gh.DraftReviewComment {
	lineRange := comment.Range
	side := strings.ToUpper(lineRange.Side)
	next := &gh.DraftReviewComment{
		Path: &lineRange.Path,
		Body: &comment.Body,
		Side: &side,
		Line: &lineRange.Line,
	}
	if lineRange.StartLine != nil && lineRange.StartSide != "" {
		startSide := strings.ToUpper(lineRange.StartSide)
		next.StartSide = &startSide
		next.StartLine = lineRange.StartLine
	}
	return next
}

func (p *gitHubClientProvider) EditMergeRequestContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.MergeRequest, error) {
	pr, err := p.client.EditPullRequest(
		ctx, ref.Owner, ref.Name, number, EditPullRequestOpts{Title: title, Body: body},
	)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if pr == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return platformgithub.NormalizePullRequest(ref, pr)
}

func (p *gitHubClientProvider) EditIssueContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.Issue, error) {
	ghIssue, err := p.client.EditIssueContent(
		ctx, ref.Owner, ref.Name, number, title, body,
	)
	if err != nil {
		return platform.Issue{}, err
	}
	if ghIssue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return platformgithub.NormalizeIssue(ref, ghIssue)
}

// SetWatchInterval sets the fast-sync interval for watched MRs.
func (s *Syncer) SetWatchInterval(d time.Duration) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	s.watchInterval = d
}

// SetActiveMRWindow sets the recency window used to add open, recently
// active MRs to the fast-sync watch list.
func (s *Syncer) SetActiveMRWindow(d time.Duration) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	s.activeMRWindow = d
}

// SetPreferGitHubNativeStacks enables GitHub's read-only preview metadata for
// subsequent repository syncs. Existing branch inference remains the fallback.
// It returns the previous value so a caller reconciles on the transition it
// actually performed: two concurrent config writers reading the preference from
// their own config snapshots could otherwise both believe they turned it off.
func (s *Syncer) SetPreferGitHubNativeStacks(enabled bool) bool {
	// The swap takes the projection lock so a transition cannot land while a
	// reconciliation or a sync hook is mid-projection. Rechecking the preference
	// inside that lock is only sound if no swap can interleave with the check.
	s.stackProjectionMu.Lock()
	defer s.stackProjectionMu.Unlock()
	previous := s.preferGitHubNativeStacks.Swap(enabled)
	if previous != enabled {
		// Invalidate results captured under the old preference before any
		// caller reconciles projections.
		s.nativeStackGeneration.Add(1)
	}
	if !enabled || previous {
		return previous
	}
	s.nativeStackConfirmations.Range(func(key, _ any) bool {
		s.nativeStackConfirmations.Delete(key)
		return true
	})
	// A cached validator may describe a PR response fetched before native
	// metadata was enabled. Force one fresh list response so the first enabled
	// sync can seed hints; later syncs retain the normal ETag optimization.
	for _, repo := range s.TrackedRepos() {
		if repoPlatform(repo) != platform.KindGitHub {
			continue
		}
		if client, ok := s.optionalGitHubClientFor(repo); ok {
			client.InvalidateListETagsForRepo(repo.Owner, repo.Name, "pulls")
		}
	}
	return previous
}

func (s *Syncer) watchSettings() (time.Duration, time.Duration) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return s.watchSettingsLocked()
}

// HasDiffSync reports whether the syncer has a clone manager configured
// and is therefore expected to populate diff SHAs for tracked PRs. The
// HTTP layer uses this to decide whether a missing diff is a sync issue
// worth warning about, or simply a deployment that opted out of diffs.
func (s *Syncer) HasDiffSync() bool {
	return s.clones != nil
}

// SetWatchedMRs replaces the fast-sync watch list. Each watched
// MR is synced on the watch interval via SyncMR, independent of
// the bulk sync cycle.
func (s *Syncer) SetWatchedMRs(mrs []WatchedMR) {
	s.watchMu.Lock()
	s.watchedMRs = slices.Clone(mrs)
	s.watchMu.Unlock()
}

// SetOnMRSynced registers a callback invoked after each MR
// is upserted during a sync pass.
//
// Concurrency: RunOnce processes repos in parallel (see
// SetParallelism), so the callback may be invoked from up to
// `parallelism` goroutines concurrently. Implementations must
// be safe for concurrent use. The callback also runs on the
// goroutine that is mid-sync for a repo, so it must not block
// indefinitely or it will stall sync progress.
//
// Call SetOnMRSynced before Start/RunOnce. Mutating the hook
// while a sync is in flight is not safe.
func (s *Syncer) SetOnMRSynced(
	fn func(owner, name string, mr *db.MergeRequest),
) {
	s.onMRSynced = fn
}

// SetOnSyncCompleted registers a callback invoked at the end
// of each RunOnce pass with per-repo sync results.
//
// Concurrency: this hook fires once per RunOnce pass on the
// goroutine that drives RunOnce, so it is not invoked
// concurrently with itself. Call SetOnSyncCompleted before
// Start/RunOnce; mutating the hook while a sync is in flight
// is not safe.
func (s *Syncer) SetOnSyncCompleted(
	fn func(results []RepoSyncResult),
) {
	s.onSyncCompleted = fn
}

// SetOnWatchedMRSyncCompleted registers a callback invoked once after a
// watched-MR fast-sync pass refreshes at least one MR.
func (s *Syncer) SetOnWatchedMRSyncCompleted(fn func()) {
	s.onWatchedMRSyncCompleted = fn
}

// SetOnMergedActorRepaired registers a callback invoked after scheduled
// reconciliation persists an authored merged event. RunOnce can process
// repositories in parallel, so the callback must be concurrency-safe.
// Register it before Start or RunOnce.
func (s *Syncer) SetOnMergedActorRepaired(
	fn func(context.Context, int64, int),
) {
	s.onMergedActorRepaired = fn
}

// SetParallelism sets the maximum number of repos synced
// concurrently in RunOnce. Values <= 0 are clamped to 1
// (sequential).
func (s *Syncer) SetParallelism(n int) {
	if n < 1 {
		n = 1
	}
	s.parallelism.Store(int32(n))
}

// SetBranchActivityLimits configures how much default-branch commit
// activity the syncer persists.
func (s *Syncer) SetBranchActivityLimits(
	retention time.Duration,
	maxCommits int,
) {
	if retention <= 0 {
		retention = defaultBranchActivityRetention
	}
	if maxCommits <= 0 {
		maxCommits = defaultBranchActivityMaxCommits
	}
	s.branchActivityMu.Lock()
	s.branchActivityRetention = retention
	s.branchActivityMaxCommits = maxCommits
	s.branchActivityMu.Unlock()
}

func (s *Syncer) branchActivityLimits() (time.Duration, int) {
	s.branchActivityMu.RLock()
	retention := s.branchActivityRetention
	maxCommits := s.branchActivityMaxCommits
	s.branchActivityMu.RUnlock()
	if retention <= 0 {
		retention = defaultBranchActivityRetention
	}
	if maxCommits <= 0 {
		maxCommits = defaultBranchActivityMaxCommits
	}
	return retention, maxCommits
}

// BranchActivityLimits reports the configured default-branch activity
// retention and per-branch commit cap.
func (s *Syncer) BranchActivityLimits() (time.Duration, int) {
	return s.branchActivityLimits()
}

// SetOnStatusChange registers a callback invoked whenever the
// sync status transitions (start, per-repo progress, rate-limit
// wait, completion). Used by the server to broadcast live sync
// state over SSE.
func (s *Syncer) SetOnStatusChange(fn func(status *SyncStatus)) {
	s.onStatusChange = fn
}

// SetOnNotificationSyncComplete registers a callback invoked after each
// notification sync run finishes. Register it before Start so it is never
// assigned concurrently with a running sidecar sync.
func (s *Syncer) SetOnNotificationSyncComplete(fn func()) {
	s.onNotificationSyncComplete = fn
}

// SetFetchers registers fallback GitHub GraphQL fetchers keyed by platform host.
func (s *Syncer) SetFetchers(fetchers map[string]*GraphQLFetcher) {
	s.fetchers = fetchers
}

// PrefersGitHubNativeStacks reports the preference currently in force. Callers
// reconciling a past transition recheck it under the projection lock: a newer
// transition may have already landed and projected, and replaying the older
// decision would overwrite it.
func (s *Syncer) PrefersGitHubNativeStacks() bool {
	return s.preferGitHubNativeStacks.Load()
}

// SetClock replaces the syncer's time source. Cache aging bounds -- notably the
// native-stack observation window -- span hours, so callers that need to observe
// those transitions inject a clock instead of waiting. The field is read without
// synchronization on the sync path, so this must not be called while a sync is
// running.
func (s *Syncer) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.now = now
}

// SetGitHubRouters registers configuration-bounded credential routers keyed by
// GitHub host. Route-specific GraphQL fetchers take precedence over legacy
// host fallback fetchers.
func (s *Syncer) SetGitHubRouters(routers map[string]*HostRouter) {
	s.routers = routers
}

// fetcherFor returns the GitHub GraphQL fetcher selected for a repository's
// credential route, or the host fallback fetcher when no router is configured.
func (s *Syncer) fetcherFor(repo RepoRef) *GraphQLFetcher {
	return s.fetcherForContext(context.Background(), repo)
}

func (s *Syncer) fetcherForContext(ctx context.Context, repo RepoRef) *GraphQLFetcher {
	if repoPlatform(repo) != platform.KindGitHub {
		return nil
	}
	host := repoHost(repo)
	if router := s.routers[host]; router != nil {
		fetcher, err := router.FetcherForRepo(ctx, repo.Owner, repo.Name)
		if err == nil {
			return fetcher
		}
		return nil
	}
	return s.fetchers[host]
}

// TriggerRun kicks off a non-blocking ad-hoc sync on the Syncer's
// wait group so callers can request an immediate run without
// blocking the caller. Ad-hoc runs bypass the normal nextSyncAfter
// cadence gate, but still respect hard rate-limit pauses and the
// syncer's lifecycle: Stop cancels the merged context so any
// in-flight GitHub call unblocks, then waits for the goroutine to
// exit. The caller's ctx is honored by a run that starts immediately.
// Once an accepted trigger is coalesced behind an active run, the follow-up
// is owned by the syncer's lifecycle so caller completion cannot retract it.
// It returns true only after the request is retained as active or pending work.
func (s *Syncer) TriggerRun(ctx context.Context) bool {
	return s.TriggerRunWithPriority(ctx, nil)
}

// TriggerRunWithPriority kicks off a non-blocking ad-hoc sync and dispatches
// matching repos before the rest of the configured set.
func (s *Syncer) TriggerRunWithPriority(
	ctx context.Context,
	priorityRepos []RepoRef,
) bool {
	return s.triggerRun(ctx, slices.Clone(priorityRepos), nil)
}

// TriggerRunForRepos kicks off a non-blocking ad-hoc sync restricted to the
// matching configured repositories.
func (s *Syncer) TriggerRunForRepos(ctx context.Context, repos []RepoRef) bool {
	return s.triggerRun(ctx, nil, slices.Clone(repos))
}

func (s *Syncer) triggerRun(
	ctx context.Context,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
) bool {
	return s.triggerRunWithCadence(ctx, true, priorityRepos, onlyRepos)
}

func (s *Syncer) triggerRunWithCadence(
	ctx context.Context,
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
) bool {
	if !s.SyncEnabled() {
		return false
	}

	// Admission and lifecycle registration are one operation from the
	// caller's perspective: a successful return means Stop cannot miss the
	// work and an active run has already retained the request.
	s.lifecycleMu.Lock()
	if s.stopped {
		s.lifecycleMu.Unlock()
		return false
	}
	s.runMu.Lock()
	if s.running.Load() {
		accepted := bypassNextSyncAfter || onlyRepos == nil && s.exclusiveRun
		if accepted {
			s.coalescePendingRunLocked(
				bypassNextSyncAfter, priorityRepos, onlyRepos,
			)
		}
		s.runMu.Unlock()
		s.lifecycleMu.Unlock()
		return accepted
	}
	s.running.Store(true)
	s.exclusiveRun = onlyRepos != nil
	s.runMu.Unlock()

	s.startClaimedRunLocked(
		ctx, bypassNextSyncAfter, priorityRepos, onlyRepos, nil,
	)
	s.lifecycleMu.Unlock()
	return true
}

// startClaimedRunLocked registers and starts work whose single-flight slot is
// already owned. The caller must hold lifecycleMu and must have checked stopped.
func (s *Syncer) startClaimedRunLocked(
	ctx context.Context,
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
	bypassRepos []RepoRef,
) {
	merged, cancel := s.mergeWithRunCtx(ctx)
	s.wg.Go(func() {
		defer cancel()
		s.runOnceWithSlot(
			merged,
			bypassNextSyncAfter,
			priorityRepos,
			onlyRepos,
			bypassRepos,
			true,
		)
	})
}

func (s *Syncer) launchClaimedRun(
	ctx context.Context,
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
	bypassRepos []RepoRef,
) bool {
	if !s.SyncEnabled() {
		return false
	}
	s.lifecycleMu.Lock()
	if s.stopped {
		s.lifecycleMu.Unlock()
		return false
	}
	s.startClaimedRunLocked(
		ctx, bypassNextSyncAfter, priorityRepos, onlyRepos, bypassRepos,
	)
	s.lifecycleMu.Unlock()
	return true
}

func repoPlatform(repo RepoRef) platform.Kind {
	if repo.Platform != "" {
		return repo.Platform
	}
	return platform.KindGitHub
}

func repoHost(repo RepoRef) string {
	if repo.PlatformHost != "" {
		return canonicalRepoHost(repo.PlatformHost)
	}
	if host, ok := platform.DefaultHost(repoPlatform(repo)); ok {
		return host
	}
	return platform.DefaultGitHubHost
}

func (s *Syncer) identityForRepo(repo RepoRef, write bool) (IdentityKey, error) {
	return s.identityForRepoContext(context.Background(), repo, write)
}

func (s *Syncer) identityForRepoContext(
	ctx context.Context, repo RepoRef, write bool,
) (IdentityKey, error) {
	if repoPlatform(repo) != platform.KindGitHub {
		return HostIdentity(repoHost(repo)), nil
	}
	host := repoHost(repo)
	router := s.routers[host]
	if router == nil {
		return HostIdentity(host), nil
	}
	if IsArchiveSyncBudgetContext(ctx) && !write {
		return router.ArchiveIdentityForRepo(repo.Owner, repo.Name)
	}
	if write {
		return router.WriteIdentityForRepo(repo.Owner, repo.Name)
	}
	return router.ReadIdentityForRepo(repo.Owner, repo.Name)
}

func (s *Syncer) archiveIdentityForRepo(repo RepoRef) (IdentityKey, error) {
	if repoPlatform(repo) != platform.KindGitHub {
		return HostIdentity(repoHost(repo)), nil
	}
	host := repoHost(repo)
	router := s.routers[host]
	if router == nil {
		return HostIdentity(host), nil
	}
	return router.ArchiveIdentityForRepo(repo.Owner, repo.Name)
}

func (s *Syncer) bucketKeyForRepo(repo RepoRef, write bool) (string, error) {
	return s.bucketKeyForRepoContext(context.Background(), repo, write)
}

func (s *Syncer) bucketKeyForRepoContext(
	ctx context.Context, repo RepoRef, write bool,
) (string, error) {
	identity, err := s.identityForRepoContext(ctx, repo, write)
	if err != nil {
		return "", err
	}
	return RateBucketKey(
		string(repoPlatform(repo)), identity.Host, identity.Principal,
	), nil
}

// RateTrackerForRepo returns the identity-scoped read tracker for apiType.
func (s *Syncer) RateTrackerForRepo(repo RepoRef, apiType string) (*RateTracker, bool) {
	bucket, err := s.bucketKeyForRepo(repo, false)
	if err != nil {
		return nil, false
	}
	if apiType == "graphql" {
		if fetcher := s.fetcherFor(repo); fetcher != nil && fetcher.RateTracker() != nil {
			return fetcher.RateTracker(), true
		}
		if rt := s.GQLRateTrackers()[bucket]; rt != nil {
			return rt, true
		}
	}
	rt := s.rateTrackers[bucket]
	if rt == nil {
		return nil, false
	}
	return rt, true
}

// ReadIdentityForRepo returns the restart-bound read identity selected for
// repo when a GitHub route router is configured.
func (s *Syncer) ReadIdentityForRepo(repo RepoRef) (IdentityKey, bool) {
	if repoPlatform(repo) != platform.KindGitHub {
		return IdentityKey{}, false
	}
	router := s.routers[repoHost(repo)]
	if router == nil {
		return IdentityKey{}, false
	}
	identity, err := router.ReadIdentityForRepo(repo.Owner, repo.Name)
	return identity, err == nil && identity.Principal != ""
}

// HasGitHubRouter reports whether startup configured route selection for the
// repository's GitHub host. A configured router can still reject a repository
// with MissingRouteError when no exact, owner, or fallback route matches.
func (s *Syncer) HasGitHubRouter(repo RepoRef) bool {
	if s == nil || repoPlatform(repo) != platform.KindGitHub {
		return false
	}
	return s.routers[repoHost(repo)] != nil
}

// WriteIdentityForRepo returns the restart-bound mutation identity selected
// for repo. App-only routes have no write identity and remain mutation-disabled
// until restart establishes one.
func (s *Syncer) WriteIdentityForRepo(repo RepoRef) (IdentityKey, bool) {
	identity, err := s.identityForRepo(repo, true)
	return identity, err == nil && identity.Principal != ""
}

type writeCredentialProber interface {
	ProbeWriteCredential(context.Context) error
}

func (s *Syncer) writeCredentialProberForRepo(
	repo RepoRef,
) (*Route, writeCredentialProber, error) {
	if repoPlatform(repo) != platform.KindGitHub {
		return nil, nil, nil
	}
	router := s.routers[repoHost(repo)]
	if router == nil {
		return nil, nil, nil
	}
	route, err := router.RouteForRepo(repo.Owner, repo.Name)
	if err != nil {
		return nil, nil, err
	}
	client := route.Client
	if route.WriteSnapshotClient != nil {
		client = route.WriteSnapshotClient
	}
	prober, ok := client.(writeCredentialProber)
	if !ok {
		return route, nil, nil
	}
	return route, prober, nil
}

// WriteCredentialProbeKeyForRepo identifies the routed mutation credential
// whose live availability is probed. Owner routes share one key across their
// repositories, while selected-installation routes remain repository-exact.
func (s *Syncer) WriteCredentialProbeKeyForRepo(repo RepoRef) (string, error) {
	route, prober, err := s.writeCredentialProberForRepo(repo)
	if err != nil || route == nil || prober == nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s",
		route.Key.Host, route.Key.Owner, route.Key.Name, route.WriteIdentity.String(),
	), nil
}

// ProbeWriteCredentialForRepo resolves the live mutation-bound credential for
// repo. The route identity is fixed at startup, but the underlying token file,
// environment, or gh CLI source can disappear or rotate afterwards.
func (s *Syncer) ProbeWriteCredentialForRepo(
	ctx context.Context, repo RepoRef,
) error {
	_, prober, err := s.writeCredentialProberForRepo(repo)
	if err != nil {
		return err
	}
	if prober == nil {
		return nil
	}
	return prober.ProbeWriteCredential(ctx)
}

// WriteRateTrackerForRepo returns the tracker for the credential that
// authenticates repository mutations.
func (s *Syncer) WriteRateTrackerForRepo(repo RepoRef, apiType string) (*RateTracker, bool) {
	identity, ok := s.WriteIdentityForRepo(repo)
	if !ok {
		return nil, false
	}
	bucket := RateBucketKey(string(repoPlatform(repo)), identity.Host, identity.Principal)
	trackers := s.writeRateTrackers
	if apiType == "graphql" {
		trackers = s.writeGQLRateTrackers
	}
	if rt := trackers[bucket]; rt != nil {
		return rt, true
	}
	return s.RateTrackerForRepo(repo, apiType)
}

// BudgetForRepo returns the background sync budget owned by the repository's
// effective read identity.
func (s *Syncer) BudgetForRepo(repo RepoRef) (*SyncBudget, bool) {
	bucket, err := s.bucketKeyForRepo(repo, false)
	if err != nil {
		return nil, false
	}
	budget := s.budgets[bucket]
	if budget == nil {
		return nil, false
	}
	return budget, true
}

func platformRepoRef(repo RepoRef) platform.RepoRef {
	repoPath := repo.RepoPath
	if repoPath == "" {
		repoPath = repo.Owner + "/" + repo.Name
	}
	return platform.RepoRef{
		Platform:           repoPlatform(repo),
		Host:               repoHost(repo),
		Owner:              repo.Owner,
		Name:               repo.Name,
		RepoPath:           repoPath,
		PlatformID:         repo.PlatformRepoID,
		PlatformExternalID: repo.PlatformExternalID,
		WebURL:             repo.WebURL,
		CloneURL:           repo.CloneURL,
		DefaultBranch:      repo.DefaultBranch,
	}
}

func cloneRemoteURL(repo RepoRef) string {
	if repo.CloneURL != "" {
		return repo.CloneURL
	}
	repoPath := repo.RepoPath
	if repoPath == "" {
		repoPath = strings.Trim(repo.Owner+"/"+repo.Name, "/")
	}
	return fmt.Sprintf("https://%s/%s.git", repoHost(repo), strings.Trim(repoPath, "/"))
}

func withCloneRepositoryIdentity(ctx context.Context, repo RepoRef) context.Context {
	return gitclone.WithRepositoryIdentity(ctx, repo.PlatformExternalID)
}

func (s *Syncer) ensureCloneForRoute(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	routeFence db.RepositoryRouteFence,
) error {
	identity := platform.DBRepoIdentity(platformRepoRef(repo))
	return s.clones.EnsureCloneValidated(
		ctx,
		string(repoPlatform(repo)),
		repoHost(repo),
		repo.Owner,
		repo.Name,
		cloneRemoteURL(repo),
		func(validationCtx context.Context) error {
			if s.beforeCloneRouteValidation != nil {
				s.beforeCloneRouteValidation()
			}
			current, found, err := s.db.CurrentRepositoryRouteFence(
				validationCtx, identity, repoID,
			)
			if err != nil {
				return err
			}
			if !found || current != routeFence {
				return db.ErrRepositoryRouteFenceChanged
			}
			return nil
		},
	)
}

func (s *Syncer) optionalGitHubClientFor(repo RepoRef) (Client, bool) {
	client, err := s.clientFor(repo)
	if err != nil {
		return nil, false
	}
	return client, true
}

// clientFor returns the legacy GitHub Client for the given repo's host.
// Repos with an empty host default to "github.com".
func clientForRegistry(registry *platform.Registry, repo RepoRef) (Client, error) {
	host := repoHost(repo)
	provider, err := registry.Provider(repoPlatform(repo), host)
	if err != nil {
		return nil, err
	}
	legacy, ok := provider.(interface{ GitHubClient() Client })
	if !ok || legacy.GitHubClient() == nil {
		return nil, fmt.Errorf("no GitHub client configured for host %s", host)
	}
	return legacy.GitHubClient(), nil
}

func (s *Syncer) clientFor(repo RepoRef) (Client, error) {
	return clientForRegistry(s.clients, repo)
}

func (s *Syncer) mergeRequestReaderFor(repo RepoRef) (platform.MergeRequestReader, error) {
	return s.clients.MergeRequestReader(repoPlatform(repo), repoHost(repo))
}

func (s *Syncer) issueReaderFor(repo RepoRef) (platform.IssueReader, error) {
	return s.clients.IssueReader(repoPlatform(repo), repoHost(repo))
}

func (s *Syncer) labelReaderFor(repo RepoRef) (platform.LabelReader, error) {
	return s.clients.LabelReader(repoPlatform(repo), repoHost(repo))
}

func (s *Syncer) releaseReaderFor(repo RepoRef) (platform.ReleaseReader, error) {
	return s.clients.ReleaseReader(repoPlatform(repo), repoHost(repo))
}

func (s *Syncer) tagReaderFor(repo RepoRef) (platform.TagReader, error) {
	return s.clients.TagReader(repoPlatform(repo), repoHost(repo))
}

func (s *Syncer) ciReaderFor(repo RepoRef) (platform.CIReader, error) {
	return s.clients.CIReader(repoPlatform(repo), repoHost(repo))
}

// ClientForRepo returns the Client for a tracked repo by
// owner/name, or an error if the repo is not tracked.
func (s *Syncer) ClientForRepo(
	owner, name string,
) (Client, error) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, r := range s.repos {
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) {
			return s.clientFor(r)
		}
	}
	return nil, fmt.Errorf(
		"repo %s/%s is not tracked", owner, name,
	)
}

// ClientForHost returns the Client for a specific host,
// or an error if no client is configured for that host.
func (s *Syncer) ClientForHost(
	host string,
) (Client, error) {
	return s.clientFor(RepoRef{PlatformHost: host})
}

// DirectClientForHost returns a legacy GitHub client for an explicit
// foreground provider operation.
func (s *Syncer) DirectClientForHost(host string) (Client, error) {
	return clientForRegistry(s.directClients, RepoRef{PlatformHost: host})
}

func (s *Syncer) ProviderCapabilities(
	kind platform.Kind,
	host string,
) (platform.Capabilities, error) {
	if kind == "" {
		kind = platform.KindGitHub
	}
	if strings.TrimSpace(host) == "" {
		defaultHost, ok := platform.DefaultHost(kind)
		if !ok {
			return platform.Capabilities{}, platform.ProviderNotConfigured(kind, "")
		}
		host = defaultHost
	}
	return s.directClients.Capabilities(kind, canonicalRepoHost(host))
}

func (s *Syncer) RepositoryReader(
	kind platform.Kind,
	host string,
) (platform.RepositoryReader, error) {
	return s.directClients.RepositoryReader(kind, canonicalRepoHost(host))
}

// Registry returns the boot-time provider registry for explicit foreground
// operations. Callers must not mutate it.
func (s *Syncer) Registry() *platform.Registry {
	return s.directClients
}

// SyncRegistry returns the provider-gated registry used by refresh work.
func (s *Syncer) SyncRegistry() *platform.Registry {
	return s.clients
}

func (s *Syncer) LabelReader(
	kind platform.Kind,
	host string,
) (platform.LabelReader, error) {
	return s.directClients.LabelReader(kind, canonicalRepoHost(host))
}

func (s *Syncer) CommentMutator(
	kind platform.Kind,
	host string,
) (platform.CommentMutator, error) {
	return s.directClients.CommentMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) StateMutator(
	kind platform.Kind,
	host string,
) (platform.StateMutator, error) {
	return s.directClients.StateMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) MergeMutator(
	kind platform.Kind,
	host string,
) (platform.MergeMutator, error) {
	return s.directClients.MergeMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) WorkflowApprovalMutator(
	kind platform.Kind,
	host string,
) (platform.WorkflowApprovalMutator, error) {
	return s.directClients.WorkflowApprovalMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) ReadyForReviewMutator(
	kind platform.Kind,
	host string,
) (platform.ReadyForReviewMutator, error) {
	return s.directClients.ReadyForReviewMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) DraftMutator(
	kind platform.Kind,
	host string,
) (platform.DraftMutator, error) {
	return s.directClients.DraftMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) IssueMutator(
	kind platform.Kind,
	host string,
) (platform.IssueMutator, error) {
	return s.directClients.IssueMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) LabelMutator(
	kind platform.Kind,
	host string,
) (platform.LabelMutator, error) {
	return s.directClients.LabelMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) ReviewMutator(
	kind platform.Kind,
	host string,
) (platform.ReviewMutator, error) {
	return s.directClients.ReviewMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) RequestChangesMutator(
	kind platform.Kind,
	host string,
) (platform.RequestChangesMutator, error) {
	return s.directClients.RequestChangesMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) AssigneeMutator(
	kind platform.Kind,
	host string,
) (platform.AssigneeMutator, error) {
	return s.directClients.AssigneeMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) ReviewerMutator(
	kind platform.Kind,
	host string,
) (platform.ReviewerMutator, error) {
	return s.directClients.ReviewerMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) DiffReviewDraftMutator(
	kind platform.Kind,
	host string,
) (platform.DiffReviewDraftMutator, error) {
	return s.directClients.DiffReviewDraftMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) ReviewSuggestionApplier(
	kind platform.Kind,
	host string,
) (platform.ReviewSuggestionApplier, error) {
	return s.directClients.ReviewSuggestionApplier(kind, canonicalRepoHost(host))
}

func (s *Syncer) DiffReviewThreadResolver(
	kind platform.Kind,
	host string,
) (platform.DiffReviewThreadResolver, error) {
	return s.directClients.DiffReviewThreadResolver(kind, canonicalRepoHost(host))
}

func (s *Syncer) MergeRequestReviewThreadReader(
	kind platform.Kind,
	host string,
) (platform.MergeRequestReviewThreadReader, error) {
	reader, err := s.directClients.MergeRequestReviewThreadReader(kind, canonicalRepoHost(host))
	if err != nil {
		return nil, err
	}
	return syncGatedReviewThreadReader{syncer: s, reader: reader}, nil
}

type syncGatedReviewThreadReader struct {
	syncer *Syncer
	reader platform.MergeRequestReviewThreadReader
}

func (r syncGatedReviewThreadReader) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	if err := r.syncer.syncDisabledError(); err != nil {
		return nil, err
	}
	return r.reader.ListMergeRequestReviewThreads(ctx, ref, number)
}

func (s *Syncer) MergeRequestContentMutator(
	kind platform.Kind,
	host string,
) (platform.MergeRequestContentMutator, error) {
	return s.directClients.MergeRequestContentMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) IssueContentMutator(
	kind platform.Kind,
	host string,
) (platform.IssueContentMutator, error) {
	return s.directClients.IssueContentMutator(kind, canonicalRepoHost(host))
}

func (s *Syncer) ResolveConfiguredRepo(
	ctx context.Context,
	repo config.Repo,
) (ConfiguredRepoStatus, []RepoRef, error) {
	return ResolveConfiguredRepoWithRegistry(ctx, s.directClients, repo)
}

func (s *Syncer) ResolveConfiguredRepoForSync(
	ctx context.Context,
	repo config.Repo,
) (ConfiguredRepoStatus, []RepoRef, error) {
	return ResolveConfiguredRepoWithRegistry(ctx, s.clients, repo)
}

func (s *Syncer) trackedRepoOnHost(owner, name, host string) (RepoRef, bool) {
	if host == "" {
		host = "github.com"
	}
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, r := range s.repos {
		rHost := repoHost(r)
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) &&
			strings.EqualFold(rHost, host) {
			return r, true
		}
	}
	return RepoRef{}, false
}

func (s *Syncer) trackedRepo(owner, name string) (RepoRef, bool, error) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()

	var matched RepoRef
	count := 0
	for _, r := range s.repos {
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) {
			matched = r
			count++
		}
	}
	if count == 0 {
		return RepoRef{}, false, nil
	}
	if count > 1 {
		return RepoRef{}, false, fmt.Errorf(
			"repo %s/%s is ambiguous across configured providers",
			owner, name,
		)
	}
	return matched, true, nil
}

func (s *Syncer) trackedRepoOnHostUnique(
	owner, name, host string,
) (RepoRef, bool, error) {
	if host == "" {
		host = "github.com"
	}
	s.reposMu.Lock()
	defer s.reposMu.Unlock()

	var matched RepoRef
	count := 0
	for _, r := range s.repos {
		rHost := repoHost(r)
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) &&
			strings.EqualFold(rHost, host) {
			matched = r
			count++
		}
	}
	if count == 0 {
		return RepoRef{}, false, nil
	}
	if count > 1 {
		return RepoRef{}, false, fmt.Errorf(
			"repo %s/%s on %s is ambiguous across configured providers",
			owner, name, host,
		)
	}
	return matched, true, nil
}

func (s *Syncer) trackedRepoByIdentity(
	kind platform.Kind,
	owner, name, host string,
) (RepoRef, bool) {
	if kind == "" {
		kind = platform.KindGitHub
	}
	host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, r := range s.repos {
		rHost := repoHost(r)
		if repoPlatform(r) == kind &&
			strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) &&
			strings.EqualFold(rHost, host) {
			return r, true
		}
	}
	return RepoRef{}, false
}

func (s *Syncer) trackedRepoByProviderID(
	kind platform.Kind,
	host, providerID string,
) (RepoRef, bool) {
	if kind == "" {
		kind = platform.KindGitHub
	}
	host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return RepoRef{}, false
	}
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, repo := range s.repos {
		if repoPlatform(repo) == kind &&
			strings.EqualFold(repoHost(repo), host) &&
			strings.TrimSpace(repo.PlatformExternalID) == providerID {
			return repo, true
		}
	}
	return RepoRef{}, false
}

func detailRepoKey(kind platform.Kind, host, owner, name string) string {
	if kind == "" {
		kind = platform.KindGitHub
	}
	host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
	return string(kind) + "\x00" + host + "\x00" +
		strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// hostFor returns the platform host for a repo identified by
// owner/name. Returns "github.com" if not found. Thread-safe.
func (s *Syncer) hostFor(owner, name string) string {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, r := range s.repos {
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) {
			if r.PlatformHost != "" {
				return r.PlatformHost
			}
			return "github.com"
		}
	}
	return "github.com"
}

// HostForRepo returns the platform host for a tracked repo.
// Thread-safe.
func (s *Syncer) HostForRepo(owner, name string) string {
	return s.hostFor(owner, name)
}

// SetRepos atomically replaces the list of repositories to sync.
func (s *Syncer) SetRepos(repos []RepoRef) {
	if err := s.SetReposWithContext(context.Background(), repos, false); err != nil {
		slog.Warn("update archive repositories", "err", err)
		s.reposMu.Lock()
		s.repos = slices.Clone(repos)
		s.reposMu.Unlock()
		s.WakeArchive()
	}
}

// SetReposWithContext prepares durable archive state before exposing a new
// configured repository set to sync workers. Credential reloads may also make
// authentication-blocked work eligible without resetting archive progress.
func (s *Syncer) SetReposWithContext(ctx context.Context, repos []RepoRef, retryAuthentication bool) error {
	refs := make([]platform.RepoRef, 0, len(repos))
	for _, repo := range repos {
		refs = append(refs, platformRepoRef(repo))
	}
	if s.SyncEnabled() && s.archiveLifecycle != nil {
		seeded, err := s.archiveLifecycle.EnsureConfigured(ctx, refs)
		if err != nil {
			return fmt.Errorf("seed archive discovery: %w", err)
		}
		if retryAuthentication {
			// Only refs that seeded can resolve; a ref skipped by seeding
			// must not fail the retry pass (and with it the config reload).
			if err := s.archiveLifecycle.RetryAuthentication(ctx, seeded); err != nil {
				return fmt.Errorf("retry archive authentication: %w", err)
			}
		}
	}
	s.reposMu.Lock()
	s.repos = slices.Clone(repos)
	s.reposMu.Unlock()
	s.WakeArchive()
	return nil
}

// Start runs an immediate sync then launches a background ticker.
// It returns as soon as the goroutine is started; call Stop to shut it down.
// A second goroutine runs watched-MR fast-syncs on a shorter interval.
//
// The caller's ctx and the syncer's internal lifetime ctx (canceled
// by Stop) are both honored: either one unblocks any in-flight work.
func (s *Syncer) Start(ctx context.Context) {
	if !s.SyncEnabled() {
		return
	}
	s.lifecycleMu.Lock()
	if s.stopped {
		s.lifecycleMu.Unlock()
		return
	}

	startMerged, startCancel := s.mergeWithRunCtx(ctx)
	s.wg.Add(1)

	watchMerged, watchCancel := s.mergeWithRunCtx(ctx)
	s.wg.Add(1)
	var archiveReady chan struct{}
	if s.archiveRunner != nil {
		archiveReady = make(chan struct{})
		s.wg.Add(1)
	}
	s.lifecycleMu.Unlock()

	go func() {
		defer s.wg.Done()
		defer startCancel()
		s.RunOnce(startMerged)
		if archiveReady != nil {
			close(archiveReady)
			s.WakeArchive()
		}
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.RunOnce(startMerged)
			case <-s.stopCh:
				return
			case <-startMerged.Done():
				return
			}
		}
	}()

	if archiveReady != nil {
		go func() {
			defer s.wg.Done()
			s.runArchiveLoop(startMerged, archiveReady)
		}()
	}

	go func() {
		defer s.wg.Done()
		defer watchCancel()
		for {
			watchInt, _ := s.watchSettings()
			timer := time.NewTimer(watchInt)
			select {
			case <-timer.C:
				s.syncWatchedMRs(watchMerged)
			case <-s.stopCh:
				timer.Stop()
				return
			case <-watchMerged.Done():
				timer.Stop()
				return
			}
		}
	}()
}

func (s *Syncer) runArchiveLoop(ctx context.Context, ready <-chan struct{}) {
	select {
	case <-ready:
	case <-s.stopCh:
		return
	case <-ctx.Done():
		return
	}
	interval := s.archivePollInterval
	if interval <= 0 {
		interval = time.Second
	}
	// Idle passes back off exponentially from the pacing interval to
	// archiveIdleWait. A pass that attempted work or failed, or a wake from
	// config reload, sync completion, a budget reset, or an archive start,
	// returns the loop to the pacing interval.
	idle := backoff.NewExponentialBackOff()
	idle.InitialInterval = interval
	idle.MaxInterval = archiveIdleWait
	idle.Multiplier = 2
	idle.RandomizationFactor = 0
	idle.Reset()
	for {
		worked, err := s.archiveRunner.RunPass(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("archive worker iteration failed", "err", err)
		}
		if worked || err != nil {
			idle.Reset()
		}
		timer := time.NewTimer(idle.NextBackOff())
		select {
		case <-timer.C:
		case <-s.archiveWake:
			timer.Stop()
			idle.Reset()
		case <-s.stopCh:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// archiveIdleWait caps the archive worker's idle backoff. Every source of
// time-driven archive work waits at least a minute, so a bounded delay in
// noticing it costs little, while an idle daemon stops paying for a pass
// every second.
const archiveIdleWait = 5 * time.Minute

// backgroundReserveCost is the request allowance every background reserve check
// asks for. One constant rather than a per-caller cost: against a 200-request
// buffer the difference between one request and a queue item's worst case is
// noise, and the local hourly ceiling still accounts for each caller's real
// spend atomically per wire attempt.
const backgroundReserveCost = 3

// reserveVerdict is one credential bucket's cached reserve decision for one
// resource and mode. known distinguishes "this credential has capacity" from
// "nothing has ever been observed", which callers with a host-wide fallback
// must tell apart.
type reserveVerdict struct {
	exhausted bool
	known     bool
	resetAt   *time.Time
	at        time.Time
}

// backgroundReserveExhausted reports whether background work on this
// repository's routed credential must stop.
//
// This is the only background reserve check in the syncer. It is evaluated at
// most once per credential per resource per snapshot cadence and cached, so
// repository admission, workers, the detail and comment drains, bulk GraphQL,
// and notification acknowledgements all answer from one decision rather than
// each re-deriving it per repository, per queue item, and per page. The
// provider quota it reads only changes when the three-minute snapshot refresh
// updates it, so evaluating it more often than that mostly re-reads the same
// numbers.
//
// The consequence is deliberate: a credential that crosses its reserve inside a
// cadence window keeps spending until the window turns. The reserve is a soft
// buffer held for foreground work, not a lease; the hard guard is the local
// hourly ceiling, enforced per wire attempt in budgetTransport.
func (s *Syncer) backgroundReserveExhausted(
	repo RepoRef, resource QuotaResource, writeIdentity bool,
) bool {
	return s.reserveVerdictFor(repo, resource, writeIdentity, true).exhausted
}

// reserveVerdictFor returns the cached decision for one credential, resource,
// and mode, recomputing it at most once per snapshot cadence. Background mode
// holds the foreground reserve; foreground mode stops only on a genuinely
// empty pool, because explicit user work is what the reserve is held for.
func (s *Syncer) reserveVerdictFor(
	repo RepoRef, resource QuotaResource, writeIdentity, background bool,
) reserveVerdict {
	return s.reserveVerdictForContext(
		context.Background(), repo, resource, writeIdentity, background,
	)
}

func (s *Syncer) reserveVerdictForContext(
	ctx context.Context,
	repo RepoRef,
	resource QuotaResource,
	writeIdentity, background bool,
) reserveVerdict {
	if repoPlatform(repo) != platform.KindGitHub || s.quotaRegistry == nil {
		return reserveVerdict{}
	}
	bucket, err := s.bucketKeyForRepoContext(ctx, repo, writeIdentity)
	if err != nil {
		return reserveVerdict{}
	}
	key := reserveCacheKey(bucket, resource, background)
	now := s.nowUTC()
	s.reserveMu.RLock()
	cached, ok := s.reserves[key]
	s.reserveMu.RUnlock()
	if ok && now.Sub(cached.at) < rateLimitSnapshotRefreshInterval {
		return cached
	}
	availability := s.evaluateBackgroundReserve(
		ctx, repo, resource, writeIdentity, background,
	)
	verdict := reserveVerdict{
		exhausted: availability.Exhausted,
		known:     availability.Known,
		resetAt:   availability.ResetAt,
		at:        now,
	}
	s.reserveMu.Lock()
	if s.reserves == nil {
		s.reserves = make(map[string]reserveVerdict)
	}
	s.reserves[key] = verdict
	s.reserveMu.Unlock()
	return verdict
}

func reserveCacheKey(
	bucket string, resource QuotaResource, background bool,
) string {
	mode := "foreground"
	if background {
		mode = "background"
	}
	return bucket + "\x00" + string(resource) + "\x00" + mode
}

// invalidateReserveVerdicts drops cached decisions for a credential bucket so
// the next check re-reads the snapshot that just landed.
func (s *Syncer) invalidateReserveVerdicts(bucket string) {
	s.reserveMu.Lock()
	defer s.reserveMu.Unlock()
	for _, resource := range []QuotaResource{
		QuotaResourceREST, QuotaResourceGraphQL,
	} {
		for _, background := range []bool{true, false} {
			delete(s.reserves, reserveCacheKey(bucket, resource, background))
		}
	}
}

func (s *Syncer) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// evaluateBackgroundReserve is the single reserve computation the cache wraps.
func (s *Syncer) evaluateBackgroundReserve(
	ctx context.Context,
	repo RepoRef, resource QuotaResource, writeIdentity, background bool,
) QuotaAvailability {
	identity, err := s.identityForRepoContext(ctx, repo, writeIdentity)
	if err != nil {
		return QuotaAvailability{Allowed: true}
	}
	reserve := 0
	if background {
		reserve = RateReserveBuffer
	}
	availability := s.quotaRegistry.CheckReserve(
		identity, []QuotaResource{resource}, backgroundReserveCost, reserve,
	)
	if availability.Known {
		return availability
	}
	return s.persistedReserve(ctx, repo, resource, writeIdentity, reserve, availability)
}

// persistedReserve answers from the SQLite-backed rate tracker when the
// registry has never observed this credential. The registry lives only in
// memory, so a restart starts it empty even though the tracker rehydrates the
// credential's last observed pool -- and the /rate_limit refresh that would
// repopulate the registry is exactly what fails when a credential is in
// trouble.
func (s *Syncer) persistedReserve(
	ctx context.Context,
	repo RepoRef,
	resource QuotaResource,
	writeIdentity bool,
	reserve int,
	unobserved QuotaAvailability,
) QuotaAvailability {
	bucket, err := s.bucketKeyForRepoContext(ctx, repo, writeIdentity)
	if err != nil {
		return unobserved
	}
	tracker := s.reserveTracker(ctx, repo, bucket, resource, writeIdentity)
	if tracker == nil || !tracker.Known() {
		return unobserved
	}
	// An elapsed window describes a quota that has since reset, so it cannot
	// speak for the current one.
	resetAt := tracker.ResetAt()
	if resetAt == nil || !s.nowUTC().Before(*resetAt) {
		return unobserved
	}
	if tracker.Remaining()-backgroundReserveCost < reserve {
		return QuotaAvailability{Known: true, Exhausted: true, ResetAt: resetAt}
	}
	return QuotaAvailability{Allowed: true, Known: true, ResetAt: resetAt}
}

func (s *Syncer) reserveTracker(
	ctx context.Context,
	repo RepoRef, bucket string, resource QuotaResource, writeIdentity bool,
) *RateTracker {
	if resource == QuotaResourceGraphQL {
		if writeIdentity {
			return s.writeGQLRateTrackers[bucket]
		}
		// GraphQL trackers hang off the route's fetcher rather than a
		// syncer-level map.
		fetcher := s.fetcherForContext(ctx, repo)
		if fetcher == nil {
			return nil
		}
		return fetcher.RateTracker()
	}
	if writeIdentity {
		if tracker := s.writeRateTrackers[bucket]; tracker != nil {
			return tracker
		}
	}
	return s.rateTrackers[bucket]
}

// repoEligibility computes which credential buckets may sync now. Buckets are
// the routed identity buckets, so one credential's exhaustion cannot throttle
// a repository routed to a different credential.
func (s *Syncer) repoEligibility(
	repos []RepoRef,
	nextAfter map[string]time.Time,
) map[string]bool {
	now := time.Now().UTC()
	eligible := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key, err := s.bucketKeyForRepo(repo, false)
		if err != nil {
			continue
		}
		if _, checked := eligible[key]; checked {
			continue
		}
		if after, ok := nextAfter[key]; ok && now.Before(after) {
			eligible[key] = false
			continue
		}
		if repoPlatform(repo) == platform.KindGitHub && s.quotaRegistry != nil {
			// Ordinary background sync may establish an unknown pool from
			// response headers, so unknown quota is not a stop. Archive
			// admission is stricter and waits for both pools to be known
			// before spending surplus.
			eligible[key] = !s.backgroundReserveExhausted(
				repo, QuotaResourceREST, false,
			)
			continue
		}
		tracker := s.rateTrackers[key]
		eligible[key] = tracker == nil || !tracker.IsPaused()
	}
	return eligible
}

// advanceNextSync updates the next-sync-after gate for buckets
// that were eligible, using each bucket's current throttle factor.
func (s *Syncer) advanceNextSync(
	eligible map[string]bool,
	nextAfter map[string]time.Time,
	interval time.Duration,
) {
	now := time.Now()
	for host, ok := range eligible {
		if !ok {
			continue
		}
		rt := s.rateTrackers[host]
		if rt == nil {
			continue
		}
		nextAfter[host] = now.Add(interval * time.Duration(rt.ThrottleFactor()))
	}
}

// graphQLReadAllowed reports whether optional GraphQL sync work may run for a
// repository. It reads the routed credential's pool so one exhausted identity
// cannot suppress a healthy identity sharing the same host tracker. Background
// work holds the GraphQL reserve; foreground work stops only on hard backoff.
func (s *Syncer) graphQLReadAllowed(
	ctx context.Context, repo RepoRef, fetcher *GraphQLFetcher,
) bool {
	verdict := s.reserveVerdictForContext(
		ctx,
		repo, QuotaResourceGraphQL, false, IsSyncBudgetContext(ctx),
	)
	// A known credential pool answers on its own. Falling through to the
	// fetcher's tracker would consult a host-wide signal that both credentials
	// on a split-auth host feed, letting one exhausted pool suppress bulk
	// fetches for a healthy one.
	if verdict.known {
		return !verdict.exhausted
	}
	backoff, _ := fetcher.ShouldBackoff()
	return !backoff
}

// SyncWatchedMRs runs one synchronous pass of the fast detail-refresh lane.
// Embedders normally call Start to run this lane on its configured cadence;
// this method is available when an immediate pass is required.
func (s *Syncer) SyncWatchedMRs(ctx context.Context) {
	s.syncWatchedMRs(ctx)
}

func (s *Syncer) syncWatchedMRs(ctx context.Context) {
	if !s.SyncEnabled() {
		return
	}
	s.watchSyncMu.Lock()
	defer s.watchSyncMu.Unlock()

	ctx = WithSyncBudget(ctx)

	mrs := s.watchedMRsForFastSync(ctx, time.Now().UTC())
	if len(mrs) == 0 {
		return
	}

	watchInt, _ := s.watchSettings()
	watchRepos := make([]RepoRef, len(mrs))
	for i, mr := range mrs {
		watchRepos[i] = RepoRef{
			Platform: watchedMRPlatform(mr), PlatformHost: watchedMRHost(mr),
			Owner: mr.Owner, Name: mr.Name,
		}
	}
	eligibleBuckets := s.repoEligibility(watchRepos, s.nextWatchSyncAfter)

	// Check backoff once per provider/host bucket to avoid redundant checks.
	blockedBuckets := make(map[string]bool)
	for i := range mrs {
		bucket, err := s.bucketKeyForRepo(watchRepos[i], false)
		if err != nil {
			continue
		}
		if _, checked := blockedBuckets[bucket]; checked {
			continue
		}
		if rt := s.rateTrackers[bucket]; rt != nil {
			if backoff, _ := rt.ShouldBackoff(); backoff {
				blockedBuckets[bucket] = true
				continue
			}
		}
		blockedBuckets[bucket] = false
	}

	syncedAny := false
	for _, mr := range mrs {
		host := watchedMRHost(mr)
		repo := RepoRef{
			Platform:     watchedMRPlatform(mr),
			PlatformHost: host,
			Owner:        mr.Owner,
			Name:         mr.Name,
		}
		bucket, bucketErr := s.bucketKeyForRepo(repo, false)
		if bucketErr != nil {
			slog.Warn("resolve fast-sync credential route",
				"host", host,
				"owner", mr.Owner,
				"name", mr.Name,
				"err", bucketErr,
			)
			continue
		}
		if !eligibleBuckets[bucket] {
			slog.Debug("skipping fast-sync for throttled host",
				"host", host,
				"owner", mr.Owner,
				"name", mr.Name,
				"number", mr.Number,
			)
			continue
		}
		if blockedBuckets[bucket] {
			slog.Debug("skipping fast-sync for rate-limited host",
				"host", host,
				"owner", mr.Owner,
				"name", mr.Name,
				"number", mr.Number,
			)
			continue
		}
		probe, due := s.beginRepositoryFeatureProbe(
			ctx, repo, platform.RepositoryFeatureMergeRequests,
		)
		if !due {
			slog.Debug("skipping fast-sync for disabled repository feature",
				"platform", repoPlatform(repo),
				"host", repoHost(repo),
				"repo", platformRepoRef(repo).RepoPath,
				"feature", platform.RepositoryFeatureMergeRequests,
			)
			continue
		}
		providerAttempted := false
		err := s.syncMRWithWatchedRefTracking(ctx, mr, &providerAttempted)
		if err != nil {
			disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureMergeRequests, err,
			)
			disabled := disabledErr != nil && s.recordRepositoryFeatureDisabled(
				repo, platform.RepositoryFeatureMergeRequests, disabledErr,
			)
			if providerAttempted {
				probe.release()
			} else {
				probe.abandon()
			}
			if disabled {
				continue
			}
			slog.Warn("fast-sync watched MR failed",
				"owner", mr.Owner,
				"name", mr.Name,
				"number", mr.Number,
				"err", err,
			)
			if _, ok := errors.AsType[*DiffSyncError](err); ok {
				syncedAny = true
			}
			continue
		}
		if providerAttempted {
			probe.release()
		} else {
			probe.abandon()
		}
		syncedAny = true
	}

	s.advanceNextSync(
		eligibleBuckets, s.nextWatchSyncAfter, watchInt,
	)
	if syncedAny && s.onWatchedMRSyncCompleted != nil {
		s.onWatchedMRSyncCompleted()
	}
}

func (s *Syncer) watchedMRsForFastSync(ctx context.Context, now time.Time) []WatchedMR {
	s.watchMu.Lock()
	mrs := slices.Clone(s.watchedMRs)
	watchInt, activeWindow := s.watchSettingsLocked()
	s.watchMu.Unlock()

	watched := newWatchedMRSet(mrs)
	for _, mr := range s.hotAndWarmOpenMRs(ctx, now, activeWindow, watchInt) {
		watched.add(mr)
	}
	items := watched.slice()
	archived := s.archivedRepoKeys()
	if len(archived) == 0 {
		return items
	}
	live := make([]WatchedMR, 0, len(items))
	for _, mr := range items {
		key := detailRepoKey(
			watchedMRPlatform(mr), watchedMRHost(mr), mr.Owner, mr.Name,
		)
		if _, ok := archived[key]; ok {
			continue
		}
		live = append(live, mr)
	}
	return live
}

// archivedRepoKeys returns detailRepoKey identities for tracked archived
// repositories so live lanes (fast sync, notifications) can skip them.
func (s *Syncer) archivedRepoKeys() map[string]struct{} {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	keys := make(map[string]struct{})
	for _, repo := range s.repos {
		if repo.Archived {
			keys[detailRepoKey(
				repoPlatform(repo), repo.PlatformHost, repo.Owner, repo.Name,
			)] = struct{}{}
		}
	}
	return keys
}

func (s *Syncer) watchSettingsLocked() (time.Duration, time.Duration) {
	watchInt := s.watchInterval
	if watchInt <= 0 {
		watchInt = 30 * time.Second
	}
	return watchInt, s.activeMRWindow
}

func (s *Syncer) hotAndWarmOpenMRs(
	ctx context.Context,
	now time.Time,
	window time.Duration,
	hotInterval time.Duration,
) []WatchedMR {
	prs, err := s.db.ListMergeRequests(ctx, db.ListMergeRequestsOpts{State: "open"})
	if err != nil {
		slog.Warn("fast-sync active MR selection failed", "err", err)
		return nil
	}
	hotIDs, err := s.db.ListHotMergeRequestIDs(ctx, db.HotMergeRequestLimit)
	if err != nil {
		slog.Warn("fast-sync hot MR selection failed", "err", err)
	}
	hot := make(map[int64]struct{}, len(hotIDs))
	for _, id := range hotIDs {
		hot[id] = struct{}{}
	}

	notificationActivity := make(map[int64]time.Time)
	cutoff := now.Add(-window)
	if window > 0 {
		hints, hintErr := s.db.LatestOpenPRNotificationActivity(ctx, cutoff)
		if hintErr != nil {
			slog.Warn("fast-sync active MR notification activity failed", "err", hintErr)
		} else {
			for _, hint := range hints {
				notificationActivity[hint.MergeRequestID] = hint.SourceUpdatedAt
			}
		}
	}
	repoByID := make(map[int64]*db.Repo)
	var watched []WatchedMR
	for _, pr := range prs {
		_, isHot := hot[pr.ID]
		refreshInterval := hotInterval
		if !isHot {
			if window <= 0 {
				continue
			}
			effectiveActivityAt := pr.LastActivityAt
			if hint := notificationActivity[pr.ID]; hint.After(effectiveActivityAt) {
				effectiveActivityAt = hint
			}
			if effectiveActivityAt.Before(cutoff) {
				continue
			}
			refreshInterval = activeMRWarmRefreshInterval
		}
		if !mergeRequestDetailDue(pr, now, refreshInterval) {
			continue
		}
		repo, ok := repoByID[pr.RepoID]
		if !ok {
			var repoErr error
			repo, repoErr = s.db.GetRepoByID(ctx, pr.RepoID)
			if repoErr != nil || repo == nil {
				if repoErr != nil {
					slog.Warn("fast-sync active MR repo lookup failed",
						"repo_id", pr.RepoID,
						"err", repoErr,
					)
				}
				continue
			}
			repoByID[pr.RepoID] = repo
		}
		if _, ok := s.trackedRepoByIdentity(
			platform.Kind(repo.Platform), repo.Owner, repo.Name, repo.PlatformHost,
		); !ok {
			continue
		}
		watched = append(watched, WatchedMR{
			Platform:     platform.Kind(repo.Platform),
			PlatformHost: repo.PlatformHost,
			Owner:        repo.Owner,
			Name:         repo.Name,
			Number:       pr.Number,
		})
	}
	return watched
}

func mergeRequestDetailDue(pr db.MergeRequest, now time.Time, interval time.Duration) bool {
	if pr.DetailFetchedAt == nil {
		return true
	}
	if interval <= 0 {
		return true
	}
	return !pr.DetailFetchedAt.Add(interval).After(now)
}

func watchedMRPlatform(mr WatchedMR) platform.Kind {
	if mr.Platform != "" {
		return mr.Platform
	}
	return platform.KindGitHub
}

func watchedMRHost(mr WatchedMR) string {
	return repoHost(RepoRef{
		Platform:     watchedMRPlatform(mr),
		PlatformHost: mr.PlatformHost,
	})
}

func watchedMRKey(mr WatchedMR) string {
	return detailRepoKey(
		watchedMRPlatform(mr), watchedMRHost(mr), mr.Owner, mr.Name,
	) + fmt.Sprintf("#%d", mr.Number)
}

type watchedMRSet struct {
	seen  map[string]struct{}
	items []WatchedMR
}

func newWatchedMRSet(initial []WatchedMR) *watchedMRSet {
	w := &watchedMRSet{seen: make(map[string]struct{}, len(initial))}
	for _, mr := range initial {
		w.add(mr)
	}
	return w
}

func (w *watchedMRSet) add(mr WatchedMR) {
	key := watchedMRKey(mr)
	if _, ok := w.seen[key]; ok {
		return
	}
	w.seen[key] = struct{}{}
	w.items = append(w.items, mr)
}

func (w *watchedMRSet) slice() []WatchedMR {
	return slices.Clone(w.items)
}

// Stop signals the background goroutine to exit. Safe to call
// multiple times. Cancels the syncer's lifetime context first so
// blocked RunOnce and TriggerRun goroutines can observe the
// cancellation and unwind their GitHub calls, then waits through the
// shared syncer shutdown budget. A dependency that ignores ctx cannot
// deadlock process shutdown.
func (s *Syncer) Stop() {
	s.stopOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.stopped = true
		s.lifecycleMu.Unlock()

		close(s.stopCh)
		s.runCtxMu.Lock()
		cancel := s.runCancel
		s.runCtxMu.Unlock()
		if cancel != nil {
			cancel()
		}
	})

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownbudget.Syncer):
		slog.Warn("syncer stop timed out; returning while work is still in flight",
			"grace", shutdownbudget.Syncer)
	}
}

// Status returns a snapshot of the current sync state.
func (s *Syncer) Status() *SyncStatus {
	return s.status.Load().(*SyncStatus)
}

// SetRatePrincipalLabels registers safe display labels keyed by rate bucket.
func (s *Syncer) SetRatePrincipalLabels(labels map[string]string) {
	s.ratePrincipalLabels = labels
}

// RatePrincipalLabels returns safe display labels keyed by rate bucket.
func (s *Syncer) RatePrincipalLabels() map[string]string {
	return s.ratePrincipalLabels
}

// RateTrackers returns the per-host rate trackers map.
func (s *Syncer) RateTrackers() map[string]*RateTracker {
	return s.rateTrackers
}

// SetWriteRateTrackers attaches the per-host trackers fed by
// mutation-path responses. Kept apart from rateTrackers: the write
// credential (the user's PAT when a GitHub App handles sync reads)
// has its own budget, and the sync tracker iteration paths
// (snapshot refresh, pause handling) must not treat it as a sync
// bucket. Mutation operation availability consults this map.
func (s *Syncer) SetWriteRateTrackers(trackers map[string]*RateTracker) {
	s.writeRateTrackers = trackers
}

// WriteRateTrackers returns the per-host mutation-credential rate
// trackers; empty when no host splits read and write credentials.
func (s *Syncer) WriteRateTrackers() map[string]*RateTracker {
	if s.writeRateTrackers == nil {
		return map[string]*RateTracker{}
	}
	return s.writeRateTrackers
}

// SetWriteGQLRateTrackers attaches the per-host trackers fed by the
// write credential's GraphQL mutation responses (ready-for-review).
func (s *Syncer) SetWriteGQLRateTrackers(trackers map[string]*RateTracker) {
	s.writeGQLRateTrackers = trackers
}

// WriteGQLRateTrackers returns the per-host write-credential GraphQL
// trackers; empty when no host splits read and write credentials.
func (s *Syncer) WriteGQLRateTrackers() map[string]*RateTracker {
	if s.writeGQLRateTrackers == nil {
		return map[string]*RateTracker{}
	}
	return s.writeGQLRateTrackers
}

// Budgets returns the per-host sync budgets map.
func (s *Syncer) Budgets() map[string]*SyncBudget {
	return s.budgets
}

func (s *Syncer) SetQuotaRegistry(registry *QuotaRegistry) {
	s.quotaRegistry = registry
}

func (s *Syncer) QuotaRegistry() *QuotaRegistry {
	return s.quotaRegistry
}

// GQLRateTrackers returns per-provider/host GraphQL rate trackers
// extracted from the registered GraphQL fetchers. Hosts with
// nil fetchers or trackers are skipped.
func (s *Syncer) GQLRateTrackers() map[string]*RateTracker {
	result := make(map[string]*RateTracker, len(s.fetchers))
	add := func(f *GraphQLFetcher) {
		if f == nil {
			return
		}
		if rt := f.RateTracker(); rt != nil {
			result[rt.BucketKey()] = rt
		}
	}
	for _, f := range s.fetchers {
		add(f)
	}
	for _, router := range s.routers {
		for _, route := range router.Routes() {
			add(route.Fetcher)
			add(route.ArchiveFetcher)
		}
	}
	return result
}

type rateLimitSnapshotter interface {
	GetRateLimitSnapshot(ctx context.Context) (*RateLimitSnapshot, error)
}

// RefreshRateLimitSnapshots refreshes GitHub REST and GraphQL quota facts from
// GitHub's /rate_limit endpoint. The snapshot call is intentionally not
// recorded as a kenn-forge request because GitHub does not charge it against the
// primary REST budget.
func (s *Syncer) RefreshRateLimitSnapshots(ctx context.Context) {
	s.refreshRateLimitSnapshots(ctx)
}

func (s *Syncer) refreshRateLimitSnapshots(ctx context.Context) map[string]struct{} {
	refreshed := make(map[string]struct{})
	if !s.SyncEnabled() || s.clients == nil {
		return refreshed
	}
	type snapshotCandidate struct {
		client  Client
		tracker *RateTracker
		fetcher *GraphQLFetcher
		owner   string
		// credential distinguishes routes backed by different token sources.
		// Startup builds a separate client per route, so client identity
		// cannot tell many routes on one credential apart from genuinely
		// independent credentials resolving to the same account.
		credential string
	}
	candidates := make(map[string][]snapshotCandidate)
	for _, router := range s.routers {
		for _, route := range router.Routes() {
			readBucket := RateBucketKey(
				string(platform.KindGitHub), route.ReadIdentity.Host,
				route.ReadIdentity.Principal,
			)
			candidates[readBucket] = append(candidates[readBucket], snapshotCandidate{
				client: route.Client, tracker: s.rateTrackers[readBucket],
				fetcher: route.Fetcher, owner: route.Key.Owner,
				credential: route.CredentialKey,
			})
			if route.ArchiveReadIdentity.Principal != "" {
				archiveBucket := RateBucketKey(
					string(platform.KindGitHub), route.ArchiveReadIdentity.Host,
					route.ArchiveReadIdentity.Principal,
				)
				candidates[archiveBucket] = append(candidates[archiveBucket], snapshotCandidate{
					client:     route.ArchiveClient,
					tracker:    s.rateTrackers[archiveBucket],
					fetcher:    route.ArchiveFetcher,
					owner:      route.ArchiveKey.Owner,
					credential: route.ArchiveCredentialKey,
				})
			}
			if route.WriteIdentity.Principal != "" && route.WriteIdentity != route.ReadIdentity {
				writeBucket := RateBucketKey(
					string(platform.KindGitHub), route.WriteIdentity.Host,
					route.WriteIdentity.Principal,
				)
				writeGQL := s.writeGQLRateTrackers[writeBucket]
				var writeFetcher *GraphQLFetcher
				if writeGQL != nil {
					writeFetcher = &GraphQLFetcher{rateTracker: writeGQL}
				}
				candidates[writeBucket] = append(candidates[writeBucket], snapshotCandidate{
					client:  route.WriteSnapshotClient,
					tracker: s.writeRateTrackers[writeBucket],
					fetcher: writeFetcher, owner: route.Key.Owner,
					credential: route.WriteCredentialKey,
				})
			}
		}
	}
	for _, bucket := range slices.Sorted(maps.Keys(candidates)) {
		// Claim the refresh window before the attempt, not after it succeeds.
		// Recording it only on success let a credential whose /rate_limit call
		// keeps failing re-attempt on every pass, so a broken App installation
		// produced a steady stream of failing requests instead of one per
		// window. A failure now leaves the previous snapshot in place and
		// waits for the next window.
		if !s.claimRateLimitSnapshotRefresh(bucket, time.Now().UTC()) {
			continue
		}
		// Routes sharing a credential may still hold distinct token sources, so
		// a route whose token is broken must not stop the credential from being
		// refreshed through a healthy one. Only distinct sources are worth
		// trying: asking the same credential once per repository owner under a
		// shared App installation cannot produce a different answer, and turns
		// one failure into one request per owner. A route with no known
		// credential key counts as its own source.
		attemptedCredentials := make(map[string]struct{}, len(candidates[bucket]))
		attemptedClients := make(map[Client]struct{}, len(candidates[bucket]))
		for _, candidate := range candidates[bucket] {
			if candidate.tracker == nil || candidate.client == nil {
				continue
			}
			if candidate.credential != "" {
				if _, seen := attemptedCredentials[candidate.credential]; seen {
					continue
				}
				attemptedCredentials[candidate.credential] = struct{}{}
			} else if _, seen := attemptedClients[candidate.client]; seen {
				continue
			}
			attemptedClients[candidate.client] = struct{}{}
			snapshotRoute := &Route{
				Client: candidate.client, Fetcher: candidate.fetcher,
			}
			snapshotCtx := tokenauth.WithGitHubOwner(ctx, candidate.owner)
			if s.refreshRateLimitSnapshotForRoute(
				snapshotCtx, snapshotRoute, candidate.tracker,
			) {
				refreshed[bucket] = struct{}{}
				break
			}
		}
	}
	for _, rt := range s.rateTrackers {
		if rt == nil || rt.Provider() != string(platform.KindGitHub) || rt.APIType() != "rest" {
			continue
		}
		if rt.Principal() != "host" {
			continue
		}
		key := rt.BucketKey()
		if !s.claimRateLimitSnapshotRefresh(key, time.Now().UTC()) {
			continue
		}
		client, err := s.clientFor(RepoRef{
			Platform: platform.KindGitHub, PlatformHost: rt.PlatformHost(),
		})
		if err != nil {
			continue
		}
		route := &Route{
			Client:  client,
			Fetcher: s.fetchers[canonicalRepoHost(rt.PlatformHost())],
		}
		if s.refreshRateLimitSnapshotForRoute(ctx, route, rt) {
			refreshed[key] = struct{}{}
		}
	}
	return refreshed
}

func (s *Syncer) refreshRateLimitSnapshotForRoute(
	ctx context.Context, route *Route, rt *RateTracker,
) bool {
	if route == nil || route.Client == nil {
		return false
	}
	snapshotter, ok := route.Client.(rateLimitSnapshotter)
	if !ok {
		return false
	}
	snapshot, err := snapshotter.GetRateLimitSnapshot(ctx)
	if err != nil {
		slog.Warn("refresh GitHub rate limit snapshot failed",
			"host", rt.PlatformHost(), "principal", rt.Principal(), "err", err)
		return false
	}
	if snapshot == nil {
		return false
	}
	// The tracker is already bound to one principal's bucket, so it names the
	// identity whose pools this snapshot describes. Reconciling the registry
	// here keeps provider quota and the local trackers reading the same facts.
	identity := IdentityKey{Host: rt.PlatformHost(), Principal: rt.Principal()}
	// The cached reserve verdicts for this credential were computed from the
	// numbers this snapshot replaces, so drop them and let the next check
	// re-read. This is what puts the reserve on the snapshot's cadence rather
	// than merely on the same interval.
	defer s.invalidateReserveVerdicts(rt.BucketKey())
	updated := false
	if snapshot.Core != nil {
		rt.UpdateFromSnapshot(*snapshot.Core)
		s.quotaRegistry.UpdateSnapshot(identity, QuotaResourceREST, *snapshot.Core)
		updated = true
	}
	if snapshot.GraphQL != nil {
		if route.Fetcher != nil && route.Fetcher.RateTracker() != nil {
			route.Fetcher.RateTracker().UpdateFromSnapshot(*snapshot.GraphQL)
		}
		s.quotaRegistry.UpdateSnapshot(identity, QuotaResourceGraphQL, *snapshot.GraphQL)
	}
	return updated
}

func (s *Syncer) canRefreshRateLimitSnapshot(key string, now time.Time) bool {
	s.rateLimitSnapshotMu.Lock()
	defer s.rateLimitSnapshotMu.Unlock()
	last := s.rateLimitSnapshotRefresh[key]
	return last.IsZero() || now.Sub(last) >= rateLimitSnapshotRefreshInterval
}

func (s *Syncer) markRateLimitSnapshotRefreshed(key string, now time.Time) {
	s.rateLimitSnapshotMu.Lock()
	if s.rateLimitSnapshotRefresh == nil {
		s.rateLimitSnapshotRefresh = make(map[string]time.Time)
	}
	s.rateLimitSnapshotRefresh[key] = now
	s.rateLimitSnapshotMu.Unlock()
}

func (s *Syncer) claimRateLimitSnapshotRefresh(key string, now time.Time) bool {
	if !s.canRefreshRateLimitSnapshot(key, now) {
		return false
	}
	s.markRateLimitSnapshotRefreshed(key, now)
	return true
}

func (s *Syncer) clearRecoveredRateLimitGates(
	refreshed map[string]struct{},
	nextAfter map[string]time.Time,
	interval time.Duration,
) {
	if len(refreshed) == 0 || nextAfter == nil || interval <= 0 {
		return
	}
	now := time.Now().UTC()
	for bucket := range refreshed {
		after, ok := nextAfter[bucket]
		if !ok || !now.Before(after) {
			continue
		}
		rt := s.rateTrackers[bucket]
		if rt == nil {
			continue
		}
		refreshedDelay := interval * time.Duration(rt.ThrottleFactor())
		if after.Sub(now) > refreshedDelay {
			delete(nextAfter, bucket)
		}
	}
}

// runState holds the per-RunOnce mutable state shared by the
// worker pool. Extracted into a struct so runWorker can be a
// directly testable method instead of an inline closure.
type runState struct {
	completed               *atomic.Int32
	maxShown                *atomic.Int32
	errMu                   *sync.Mutex
	lastErr                 *string
	lastErrorCode           *SyncErrorCode
	lastErrorCeilingKey     *string
	lastErrorCeilingResetAt *string
	// canceled is latched to true at the moment any goroutine
	// observes ctx cancellation while work is still outstanding.
	// RunOnce uses this flag (rather than a completed-count
	// heuristic) to decide whether the run was canceled, so a
	// misbehaving syncRepo that ignores ctx and returns success
	// cannot mask cancellation.
	canceled *atomic.Bool
	total    int
	// results is a preallocated slice indexed by repo position so
	// OnSyncCompleted receives results in the configured repo order
	// regardless of worker completion order. Each index is written
	// by exactly one worker, so no mutex is needed.
	results []RepoSyncResult
	// exhausted collects buckets that crossed their reserve while the pass
	// was running. Eligibility is computed once before dispatch, so without
	// this the detail and comment drains would keep spending a credential the
	// workers had already stopped using. Written by every worker, so it is a
	// sync.Map rather than the plain eligibility map.
	exhausted *sync.Map
}

// repoWork pairs a repo with its index in the configured repo list
// so workers can write results to the correct preallocated slot.
type repoWork struct {
	index int
	repo  RepoRef
}

func (s *Syncer) recordRunError(
	state *runState,
	repo RepoRef,
	err error,
	errMessage string,
) {
	errorCode := syncErrorCodeFor(err)
	ceilingKey := ""
	ceilingResetAt := ""
	if errorCode == SyncErrorCodeLocalCeilingExhausted {
		identity, identityErr := s.identityForRepo(repo, false)
		if identityErr == nil {
			ceilingKey = RateStatusKey(
				string(repoPlatform(repo)), identity.Host, identity.Principal,
			)
		}
		if exhausted, ok := errors.AsType[*syncBudgetExhaustedError](err); ok {
			ceilingResetAt = exhausted.resetAt.UTC().Format(time.RFC3339)
		}
	}

	state.errMu.Lock()
	defer state.errMu.Unlock()
	if *state.lastErrorCode == SyncErrorCodeLocalCeilingExhausted &&
		errorCode != SyncErrorCodeLocalCeilingExhausted {
		return
	}
	*state.lastErr = errMessage
	*state.lastErrorCode = errorCode
	*state.lastErrorCeilingKey = ceilingKey
	*state.lastErrorCeilingResetAt = ceilingResetAt
}

// runWorker drains the work channel until it is closed or ctx
// is canceled. It is the body of each goroutine spawned by
// RunOnce. Extracted from the inline closure so cancellation
// behavior can be unit-tested directly without racing against
// the dispatch loop.
func (s *Syncer) runWorker(
	ctx context.Context,
	work <-chan repoWork,
	state *runState,
) {
	for item := range work {
		repo := item.repo
		// Defense-in-depth against the dispatch race: the
		// dispatch loop pre-checks ctx before its select, but
		// a cancel can still land in the micro-window between
		// the pre-check and the select, in which case Go's
		// select may pick the send branch and hand this worker
		// a repo that should never have been enqueued. Bail
		// here before logging or starting any work, and latch
		// the canceled flag so RunOnce reports the run as
		// canceled regardless of how many repos happened to
		// finish in parallel.
		if ctx.Err() != nil {
			state.canceled.Store(true)
			return
		}
		bucket, bucketErr := s.bucketKeyForRepo(repo, false)
		if bucketErr != nil {
			errMessage := bucketErr.Error()
			s.recordRunError(state, repo, bucketErr, errMessage)
			state.results[item.index].Error = errMessage
			continue
		}
		// Provider reserve first: this credential's own GitHub pool. The
		// tracker backoff below is a separate signal (secondary limits and
		// Retry-After), so both gates apply.
		//
		// Crossing the reserve mid-pass skips the repository rather than
		// sleeping the worker until reset. Dispatch already reports the same
		// result for buckets that were over their reserve before the pass
		// began, and holding a worker for up to an hour would let one spent
		// credential occupy the whole pool while repositories on healthy
		// credentials wait behind it. The next pass re-admits this one.
		if s.backgroundReserveExhausted(repo, QuotaResourceREST, false) {
			state.results[item.index].Error = "skipped: rate limit throttled"
			if state.exhausted != nil {
				state.exhausted.Store(bucket, struct{}{})
			}
			done := state.completed.Add(1)
			s.publishMonotonicProgress(state, done)
			continue
		}
		if rt := s.rateTrackers[bucket]; rt != nil {
			if backoff, wait := rt.ShouldBackoff(); backoff {
				s.publishStatus(&SyncStatus{
					Running: true,
					Progress: fmt.Sprintf(
						"rate limited, waiting %s", formatRateLimitWait(wait),
					),
				})
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					state.canceled.Store(true)
					return
				}
			}
		}
		repoName := repo.Owner + "/" + repo.Name
		slog.Info("syncing repo", "repo", repoName)
		nativeResultKey := repoFailKey(repo)
		s.nativeStackResults.Delete(nativeResultKey)
		err := s.syncRepo(ctx, repo)
		if nativeResult, ok := s.nativeStackResults.LoadAndDelete(nativeResultKey); ok {
			state.results[item.index].GitHubNativeStacks = nativeResult.(*GitHubNativeStackSyncResult)
		}
		if err != nil {
			// Bail without counting this repo only when the
			// *run* context itself is canceled and the error
			// reflects that. Per-request timeouts also come
			// back as wrapped context.DeadlineExceeded but
			// must reach the normal error path so they're
			// captured in lastErr instead of being silently
			// dropped.
			if ctx.Err() != nil &&
				(errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded)) {
				state.canceled.Store(true)
				return
			}
			errStr := err.Error()
			slog.Error("sync repo failed",
				"repo", repoName, "err", err,
			)
			s.recordRunError(state, repo, err, errStr)
			// Each index is written by exactly one worker. The partial
			// typing requires the whole error to be the partial failure:
			// a partial joined with any other failure is hard.
			state.results[item.index].Error = errStr
			if partial, ok := ExclusivePartialSyncFailure(err); ok {
				state.results[item.index].PartialFailure = partial
			}
		}
		// Latch the canceled flag if ctx was canceled during
		// syncRepo. A misbehaving Client implementation can
		// ignore ctx and return nil (or a non-context error)
		// even after cancellation; without this check the run
		// would fall through to the success path and fire
		// onSyncCompleted for what the user asked to cancel.
		if ctx.Err() != nil {
			state.canceled.Store(true)
			return
		}
		done := state.completed.Add(1)
		s.publishMonotonicProgress(state, done)
	}
}

func syncErrorCodeFor(err error) SyncErrorCode {
	if errors.Is(err, platform.ErrSyncBudgetExhausted) {
		return SyncErrorCodeLocalCeilingExhausted
	}
	return ""
}

// revokeExhaustedBuckets clears eligibility for credentials that have reached
// their reserve since eligibility was computed. It folds in the markers workers
// recorded and re-reads the current pools, because a credential can cross its
// reserve on the very last repository that uses it, leaving no later worker to
// notice.
func (s *Syncer) revokeExhaustedBuckets(
	eligibleBuckets map[string]bool, state *runState, repos []RepoRef,
) {
	if state != nil && state.exhausted != nil {
		state.exhausted.Range(func(key, _ any) bool {
			eligibleBuckets[key.(string)] = false
			return true
		})
	}
	checked := make(map[string]struct{}, len(eligibleBuckets))
	for _, repo := range repos {
		bucket, err := s.bucketKeyForRepo(repo, false)
		if err != nil || !eligibleBuckets[bucket] {
			continue
		}
		if _, seen := checked[bucket]; seen {
			continue
		}
		checked[bucket] = struct{}{}
		if s.backgroundReserveExhausted(repo, QuotaResourceREST, false) {
			eligibleBuckets[bucket] = false
		}
	}
}

// publishMonotonicProgress publishes a progress update only if done
// is the highest value seen so far. Skips the final "total/total"
// because detail drain still runs after index completes.
// Both worker completions and throttled-repo skips use this to
// guarantee SSE progress never regresses.
func (s *Syncer) publishMonotonicProgress(
	state *runState, done int32,
) {
	if int(done) >= state.total {
		return
	}
	for {
		cur := state.maxShown.Load()
		if done <= cur {
			return
		}
		if state.maxShown.CompareAndSwap(cur, done) {
			s.publishStatus(&SyncStatus{
				Running:  true,
				Progress: fmt.Sprintf("%d/%d", done, state.total),
			})
			return
		}
	}
}

// RunOnce performs a single sync pass across all configured repos.
// If a sync is already in progress it returns immediately (single-flight).
//
// Repos are synced in parallel using a bounded worker pool sized by
// SetParallelism (default defaultParallelism). The bound keeps the
// per-host GitHub rate limit and abuse-detection thresholds happy
// while still capturing most of the wall-clock win on network I/O.
func (s *Syncer) RunOnce(ctx context.Context) {
	s.runOnce(ctx, false, nil, nil)
}

func (s *Syncer) runOnce(
	ctx context.Context,
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
) {
	s.runOnceWithSlot(ctx, bypassNextSyncAfter, priorityRepos, onlyRepos, nil, false)
}

func (s *Syncer) runOnceWithSlot(
	ctx context.Context,
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
	bypassRepos []RepoRef,
	slotClaimed bool,
) {
	if !s.SyncEnabled() {
		if slotClaimed {
			s.releaseRunSlot()
		}
		return
	}
	if !slotClaimed {
		s.runMu.Lock()
		if s.running.Load() {
			if bypassNextSyncAfter || onlyRepos == nil && s.exclusiveRun {
				s.coalescePendingRunLocked(
					bypassNextSyncAfter, priorityRepos, onlyRepos,
				)
			}
			s.runMu.Unlock()
			return
		}
		s.running.Store(true)
		s.exclusiveRun = onlyRepos != nil
		s.runMu.Unlock()
	}
	var terminalStatus *SyncStatus
	defer func() {
		// Serialize the terminal snapshot with slot release. A new run may
		// claim runMu as soon as running becomes false, but its Running:true
		// publication must follow this pass's terminal status.
		s.statusMu.Lock()
		s.runMu.Lock()
		pending := s.pendingRun
		s.pendingRun = nil
		if pending == nil {
			s.running.Store(false)
			s.exclusiveRun = false
		} else {
			// Keep ownership of the single-flight slot while the accepted
			// follow-up is registered. New arrivals coalesce behind it.
			s.exclusiveRun = !pending.full
		}
		s.runMu.Unlock()
		if pending == nil {
			if terminalStatus != nil {
				s.publishStatusLocked(terminalStatus)
			}
			s.statusMu.Unlock()
			// A completed sync can change archive eligibility, for example by
			// clearing a repository feature cooldown, so the idle archive
			// worker must not wait out its backoff to notice.
			s.WakeArchive()
			return
		}
		s.statusMu.Unlock()
		only := pending.onlyRepos
		if pending.full {
			only = nil
		}
		launched := s.launchClaimedRun(
			context.Background(),
			pending.bypassNextSyncAfter,
			pending.priorityRepos,
			only,
			pending.bypassRepos,
		)
		if !launched {
			s.releaseRunSlotWithStatus(terminalStatus)
		}
	}()

	rateLimitSnapshotCtx := ctx
	if bypassNextSyncAfter {
		ctx = withRepositoryFeatureCooldownBypass(
			ctx, s.featureCooldowns.currentGeneration(),
		)
	} else if len(bypassRepos) > 0 {
		ctx = withRepositoryFeatureCooldownBypassForRepos(
			ctx, s.featureCooldowns.currentGeneration(), bypassRepos,
		)
	}

	// Mark context so the budget transport counts HTTP calls
	// made during background sync. User-initiated server
	// handler paths do not carry this key and are not counted.
	ctx = WithSyncBudget(ctx)

	s.reposMu.Lock()
	repos := slices.Clone(s.repos)
	s.reposMu.Unlock()
	if onlyRepos != nil {
		repos = selectRepos(repos, onlyRepos)
	}
	nextAfter := s.nextSyncAfter
	if bypassNextSyncAfter {
		nextAfter = nil
	}
	// Computed over the selected set before still-archived refs drop
	// out, so buckets holding only archived repositories keep an entry
	// for the cadence advance below.
	archivedEligibility := s.repoEligibility(repos, nextAfter)
	archivedBypassEligibility := s.repoEligibility(
		selectRepos(repos, bypassRepos), nil,
	)
	repos = s.reconcileArchivedRepos(
		ctx,
		repos,
		archivedEligibility,
		bypassRepos,
		archivedBypassEligibility,
	)
	repos = prioritizeRepos(repos, priorityRepos)

	total := len(repos)
	s.publishStatus(&SyncStatus{
		Running:  true,
		Progress: fmt.Sprintf("0/%d", total),
	})
	slog.Info("sync started", "repos", total)
	s.resetPendingCommentSyncs()

	workers := min(max(int(s.parallelism.Load()), 1), total)

	work := make(chan repoWork)
	results := make([]RepoSyncResult, total)
	for i, r := range repos {
		host := r.PlatformHost
		if host == "" {
			host = "github.com"
		}
		results[i] = RepoSyncResult{
			Platform:     repoPlatform(r),
			Owner:        r.Owner,
			Name:         r.Name,
			PlatformHost: host,
		}
	}

	if rateLimitSnapshotCtx.Err() == nil {
		refreshed := s.refreshRateLimitSnapshots(rateLimitSnapshotCtx)
		s.clearRecoveredRateLimitGates(refreshed, nextAfter, s.interval)
	}
	cadenceEligibleBuckets := s.repoEligibility(repos, nextAfter)
	bypassEligibleBuckets := s.repoEligibility(
		selectRepos(repos, bypassRepos), nil,
	)
	eligibleBuckets := maps.Clone(cadenceEligibleBuckets)
	activeRepos := make([]RepoRef, 0, len(repos))

	var (
		completed               atomic.Int32
		maxShown                atomic.Int32
		errMu                   sync.Mutex
		lastErr                 string
		lastErrorCode           SyncErrorCode
		lastErrorCeilingKey     string
		lastErrorCeilingResetAt string
		canceled                atomic.Bool
		wg                      sync.WaitGroup
	)

	state := &runState{
		completed:               &completed,
		maxShown:                &maxShown,
		errMu:                   &errMu,
		lastErr:                 &lastErr,
		lastErrorCode:           &lastErrorCode,
		lastErrorCeilingKey:     &lastErrorCeilingKey,
		lastErrorCeilingResetAt: &lastErrorCeilingResetAt,
		canceled:                &canceled,
		total:                   total,
		results:                 results,
		exhausted:               &sync.Map{},
	}
	for range workers {
		wg.Go(func() {
			s.runWorker(ctx, work, state)
		})
	}

dispatch:
	for i, r := range repos {
		bucket, err := s.bucketKeyForRepo(r, false)
		if err != nil {
			results[i].Error = err.Error()
			done := completed.Add(1)
			s.publishMonotonicProgress(state, done)
			continue
		}
		eligible := cadenceEligibleBuckets[bucket]
		if !eligible && repoMatchesAnyIntent(r, bypassRepos) {
			eligible = bypassEligibleBuckets[bucket]
			if eligible {
				eligibleBuckets[bucket] = true
			}
		}
		if !eligible {
			results[i].Error = "skipped: rate limit throttled"
			done := completed.Add(1)
			s.publishMonotonicProgress(state, done)
			continue
		}
		activeRepos = append(activeRepos, r)
		// Check ctx before entering the select. Go's select picks
		// pseudo-randomly when both branches are ready, so a naked
		// `select { case work <- r: case <-ctx.Done(): }` can still
		// hand a repo to a ready worker after the run has been
		// canceled. The pre-check biases the loop toward cancel so
		// the dispatch reliably stops once ctx is done.
		if ctx.Err() != nil {
			canceled.Store(true)
			break dispatch
		}
		item := repoWork{index: i, repo: r}
		select {
		case work <- item:
		case <-ctx.Done():
			canceled.Store(true)
			break dispatch
		}
	}
	close(work)
	wg.Wait()

	// Detail drain: fetch full details for highest-priority items
	// within the per-host budget. Runs after index scan completes.
	if !canceled.Load() && ctx.Err() == nil {
		// Eligibility was computed before the pass, so it cannot know which
		// credentials ran out while the pass was in flight. Re-check every
		// bucket rather than trusting only the workers' markers: the last
		// repository on a credential can spend the headroom with no later
		// repository left to observe it.
		s.revokeExhaustedBuckets(eligibleBuckets, state, activeRepos)
		s.drainDetailQueue(ctx, eligibleBuckets, activeRepos)
	}

	if !canceled.Load() && ctx.Err() == nil {
		// The detail drain spends the same credentials, so re-check again
		// before the comment drain rather than reusing a map the drain that
		// just ran may have invalidated.
		s.revokeExhaustedBuckets(eligibleBuckets, state, activeRepos)
		s.drainPendingCommentSyncs(ctx, eligibleBuckets)
	}

	// Use a latched flag (set by the dispatch loop and workers at
	// the moment they observe ctx cancellation) rather than a
	// completed-count heuristic. A misbehaving syncRepo that
	// ignores ctx and returns success would otherwise let the
	// run fall through to onSyncCompleted even though the user
	// asked to cancel. A cancel that races in strictly *after*
	// every worker finished and returned never latches the flag,
	// so the late-cancel-after-clean-sync case still reports
	// success.
	if canceled.Load() {
		err := ctx.Err()
		if err == nil {
			err = context.Canceled
		}
		slog.Info("sync canceled", "repos", total, "err", err)
		terminalStatus = &SyncStatus{
			Running:   false,
			LastRunAt: time.Now().UTC(),
			LastError: err.Error(),
		}
		return
	}

	if rateLimitSnapshotCtx.Err() == nil {
		s.RefreshRateLimitSnapshots(rateLimitSnapshotCtx)
	}
	if onlyRepos == nil {
		for bucket, eligible := range cadenceEligibleBuckets {
			if eligible && !eligibleBuckets[bucket] {
				cadenceEligibleBuckets[bucket] = false
			}
		}
		// Archived-only buckets are absent from eligibleBuckets — their
		// refs dropped out before it was computed — but an attempted
		// archived refresh must advance the bucket's cadence gate too,
		// or the refresh would rerun every base interval regardless of
		// throttle factor. The post-reconciliation value wins for
		// buckets present in both maps.
		advance := maps.Clone(archivedEligibility)
		maps.Copy(advance, cadenceEligibleBuckets)
		s.advanceNextSync(advance, s.nextSyncAfter, s.interval)
	}

	slog.Info("sync complete", "repos", total)

	if s.onSyncCompleted != nil {
		s.RunUnderStackProjection(func() {
			s.dropStaleNativeStackResults(results)
			s.onSyncCompleted(results)
		})
	}

	terminalStatus = &SyncStatus{
		Running:                 false,
		LastRunAt:               time.Now().UTC(),
		LastError:               lastErr,
		LastErrorCode:           lastErrorCode,
		LastErrorCeilingKey:     lastErrorCeilingKey,
		LastErrorCeilingResetAt: lastErrorCeilingResetAt,
	}
}

func (s *Syncer) releaseRunSlot() {
	s.runMu.Lock()
	s.running.Store(false)
	s.exclusiveRun = false
	s.pendingRun = nil
	s.runMu.Unlock()
}

func (s *Syncer) releaseRunSlotWithStatus(status *SyncStatus) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.releaseRunSlot()
	if status != nil {
		s.publishStatusLocked(status)
	}
}

// coalescePendingRunLocked records work accepted while another pass owns the
// single-flight slot. A full pass subsumes scoped work; the scoped repositories
// become priorities so the stronger request still reaches them first.
func (s *Syncer) coalescePendingRunLocked(
	bypassNextSyncAfter bool,
	priorityRepos []RepoRef,
	onlyRepos []RepoRef,
) {
	full := onlyRepos == nil
	bypassAll := bypassNextSyncAfter && full
	var bypassRepos []RepoRef
	if bypassNextSyncAfter && !full {
		bypassRepos = onlyRepos
	}
	if s.pendingRun == nil {
		s.pendingRun = &pendingSyncRun{
			bypassNextSyncAfter: bypassAll,
			full:                full,
			priorityRepos:       appendUniqueRepoRefs(nil, priorityRepos),
			onlyRepos:           uniqueRepoRefsPreservingNil(onlyRepos),
			bypassRepos:         appendUniqueRepoRefs(nil, bypassRepos),
		}
		return
	}

	pending := s.pendingRun
	pending.bypassNextSyncAfter = pending.bypassNextSyncAfter || bypassAll
	pending.bypassRepos = appendUniqueRepoRefs(pending.bypassRepos, bypassRepos)
	if full {
		if !pending.full {
			pending.priorityRepos = appendUniqueRepoRefs(
				pending.priorityRepos, pending.onlyRepos,
			)
			pending.onlyRepos = nil
			pending.full = true
		}
		pending.priorityRepos = appendUniqueRepoRefs(
			pending.priorityRepos, priorityRepos,
		)
		return
	}

	if pending.full {
		pending.priorityRepos = appendUniqueRepoRefs(
			pending.priorityRepos, onlyRepos,
		)
	} else {
		pending.onlyRepos = appendUniqueRepoRefs(pending.onlyRepos, onlyRepos)
	}
	pending.priorityRepos = appendUniqueRepoRefs(
		pending.priorityRepos, priorityRepos,
	)
}

func uniqueRepoRefsPreservingNil(repos []RepoRef) []RepoRef {
	if repos == nil {
		return nil
	}
	return appendUniqueRepoRefs(make([]RepoRef, 0, len(repos)), repos)
}

func appendUniqueRepoRefs(dst []RepoRef, repos []RepoRef) []RepoRef {
	for _, repo := range repos {
		if slices.ContainsFunc(dst, func(existing RepoRef) bool {
			return sameRepoIntent(existing, repo)
		}) {
			continue
		}
		dst = append(dst, repo)
	}
	return dst
}

func prioritizeRepos(repos, priorityRepos []RepoRef) []RepoRef {
	if len(repos) == 0 || len(priorityRepos) == 0 {
		return repos
	}

	priorityOrder := make(map[RepoRef]int, len(repos))
	for _, repo := range repos {
		for i, priority := range priorityRepos {
			if sameRepoIntent(repo, priority) {
				priorityOrder[repo] = i
				break
			}
		}
	}
	if len(priorityOrder) == 0 {
		return repos
	}

	out := slices.Clone(repos)
	slices.SortStableFunc(out, func(a, b RepoRef) int {
		ai, aOK := priorityOrder[a]
		bi, bOK := priorityOrder[b]
		switch {
		case aOK && bOK:
			return ai - bi
		case aOK:
			return -1
		case bOK:
			return 1
		default:
			return 0
		}
	})
	return out
}

// excludeArchivedRepos drops provider-archived repositories from a live sync
// pass. Archived repos stay tracked in s.repos for archive collection and
// API surfaces; only live polling skips them.
// reconcileArchivedRepos refreshes provider metadata for archived
// tracked refs so an upstream unarchive is observed during normal
// sync passes. Refs the provider still reports archived (or whose
// metadata refresh fails) stay excluded from the pass; refs that
// resolved unarchived rejoin it with fresh metadata. Refs in
// credential buckets the pass would not dispatch — throttled,
// reserve-exhausted, or deferred by next-sync-after — are deferred
// without a provider call: the refresh must not spend sync budget a
// live repository's dispatch would be denied. Eligibility is read
// before the pass refreshes rate-limit snapshots, so a gate that has
// recovered upstream defers the refresh by at most one pass.
func (s *Syncer) reconcileArchivedRepos(
	ctx context.Context,
	repos []RepoRef,
	eligibility map[string]bool,
	bypassRepos []RepoRef,
	bypassEligibility map[string]bool,
) []RepoRef {
	live := make([]RepoRef, 0, len(repos))
	skipped := make([]string, 0)
	for _, repo := range repos {
		if !repo.Archived {
			live = append(live, repo)
			continue
		}
		if ctx.Err() != nil {
			skipped = append(skipped, repo.Owner+"/"+repo.Name)
			continue
		}
		bucket, err := s.bucketKeyForRepo(repo, false)
		eligible := eligibility[bucket]
		if !eligible && repoMatchesAnyIntent(repo, bypassRepos) {
			eligible = bypassEligibility[bucket]
		}
		if err != nil || !eligible {
			skipped = append(skipped, repo.Owner+"/"+repo.Name)
			continue
		}
		// Register provider work so an admitted archive request on the
		// same credential is preempted instead of overlapping the
		// refresh, matching the coordination live repo syncs get.
		release := s.beginProviderWork(bucket, archive.PriorityNormalIndex)
		resolved, _, _, _, _, err := s.reconcileRepoIdentityObservation(ctx, repo)
		release()
		if err != nil {
			slog.Debug("archived repo metadata refresh failed",
				"repo", repo.Owner+"/"+repo.Name, "err", err,
			)
			skipped = append(skipped, repo.Owner+"/"+repo.Name)
			continue
		}
		if resolved.Archived {
			skipped = append(skipped, repo.Owner+"/"+repo.Name)
			continue
		}
		slog.Info("repo unarchived upstream; resuming live sync",
			"repo", resolved.Owner+"/"+resolved.Name,
		)
		live = append(live, resolved)
	}
	if len(skipped) > 0 {
		slog.Debug("skipping archived repos in live sync", "repos", skipped)
	}
	return live
}

func excludeArchivedRepos(repos []RepoRef) []RepoRef {
	live := make([]RepoRef, 0, len(repos))
	skipped := make([]string, 0)
	for _, repo := range repos {
		if repo.Archived {
			skipped = append(skipped, repo.Owner+"/"+repo.Name)
			continue
		}
		live = append(live, repo)
	}
	if len(skipped) > 0 {
		slog.Debug("skipping archived repos in live sync", "repos", skipped)
	}
	return live
}

func selectRepos(repos, selectedRepos []RepoRef) []RepoRef {
	out := make([]RepoRef, 0, len(selectedRepos))
	for _, repo := range repos {
		if slices.ContainsFunc(selectedRepos, func(selected RepoRef) bool {
			return sameRepoIntent(repo, selected)
		}) {
			out = append(out, repo)
		}
	}
	return out
}

// sameRepoIntent matches stable provider identity whenever both refs have one.
// Route fallback is reserved for refs that have not yet been provider-verified.
func sameRepoIntent(a, b RepoRef) bool {
	if repoPlatform(a) != repoPlatform(b) ||
		!strings.EqualFold(repoHost(a), repoHost(b)) {
		return false
	}
	aID := strings.TrimSpace(a.PlatformExternalID)
	bID := strings.TrimSpace(b.PlatformExternalID)
	if aID != "" && bID != "" {
		return aID == bID
	}
	aRoute := repoPriorityKey(a)
	return aRoute != "" && aRoute == repoPriorityKey(b)
}

func repoMatchesAnyIntent(repo RepoRef, intents []RepoRef) bool {
	return slices.ContainsFunc(intents, func(intent RepoRef) bool {
		return sameRepoIntent(repo, intent)
	})
}

func repoPriorityKey(repo RepoRef) string {
	repoPath := strings.Trim(strings.TrimSpace(repo.RepoPath), "/")
	if repoPath == "" {
		owner := strings.Trim(strings.TrimSpace(repo.Owner), "/")
		name := strings.Trim(strings.TrimSpace(repo.Name), "/")
		if owner == "" || name == "" {
			return ""
		}
		repoPath = owner + "/" + name
	}
	return strings.ToLower(
		string(repoPlatform(repo)) + "/" + repoHost(repo) + "/" + repoPath,
	)
}

func (s *Syncer) syncRepoIdentity(
	ctx context.Context,
	repo RepoRef,
) (db.RepoIdentity, *platform.Repository, time.Time, error) {
	observedAt := time.Now().UTC()
	identity := platform.DBRepoIdentity(platformRepoRef(repo))
	reader, err := s.clients.RepositoryReader(repoPlatform(repo), repoHost(repo))
	if err != nil {
		if identity.PlatformRepoID != "" && errors.Is(err, platform.ErrUnsupportedCapability) {
			return identity, nil, observedAt, nil
		}
		return db.RepoIdentity{}, nil, time.Time{}, err
	}
	// A refused identity resolve aborts the whole repo sync before the
	// list fetches, so it shares their essential budget reserve.
	resolved, err := reader.GetRepository(WithEssentialSyncBudget(ctx), platformRepoRef(repo))
	if err != nil {
		return db.RepoIdentity{}, nil, time.Time{}, err
	}
	identity = platform.DBRepositoryIdentity(resolved)
	if identity.PlatformRepoID == "" {
		return db.RepoIdentity{}, nil, time.Time{}, fmt.Errorf("provider returned no repo id")
	}
	return identity, &resolved, observedAt, nil
}

func (s *Syncer) reconcileRepoIdentityObservation(
	ctx context.Context,
	repo RepoRef,
) (RepoRef, int64, *platform.Repository, time.Time, bool, error) {
	previousID := int64(0)
	previous, err := s.db.ResolveActiveRepositoryRoute(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)),
	)
	if err != nil {
		return RepoRef{}, 0, nil, time.Time{}, false, err
	}
	if previous != nil {
		previousID = previous.Repository.ID
	}
	identity, resolved, observedAt, err := s.syncRepoIdentity(ctx, repo)
	if err != nil {
		return RepoRef{}, 0, nil, time.Time{}, false, err
	}
	entry, accepted, err := s.db.ReconcileRepositoryObservation(ctx, identity, observedAt)
	if err != nil {
		return RepoRef{}, 0, nil, time.Time{}, false, err
	}
	if !accepted {
		// The catalog holds a newer observation, so this snapshot's
		// metadata is stale even when the route is unchanged. Dropping
		// it makes refreshRepoSettings refetch from the provider.
		resolved = nil
		if entry.Lifecycle != db.RepositoryLifecycleActive {
			// The stale observation resolved to a repository a route
			// replacement has displaced. Syncing would fetch the reused
			// route's content into the preserved repository's history.
			return RepoRef{}, 0, nil, time.Time{}, false, fmt.Errorf(
				"repository identity observation for %s/%s is stale and "+
					"resolves to %s catalog entry %d; awaiting revalidation",
				repo.Owner, repo.Name, entry.Lifecycle, entry.Repository.ID,
			)
		}
	}
	authoritative := repoRefFromCatalog(repo, entry.Repository, resolved)
	if published, ok := s.publishResolvedRepository(
		repo, authoritative, resolved != nil,
	); ok {
		// The publication may have kept a newer tracked archived flip
		// over this snapshot's metadata; callers deciding whether to
		// keep syncing must see the value that was actually published.
		authoritative = published
	}
	if err := s.reconcileArchiveRepositoryIfNeeded(
		ctx, previousID, entry.Repository.ID,
	); err != nil {
		return RepoRef{}, 0, nil, time.Time{}, false, err
	}
	return authoritative, entry.Repository.ID, resolved, observedAt, accepted, nil
}

func (s *Syncer) reconcileArchiveRepositoryIfNeeded(
	ctx context.Context,
	previousID int64,
	repoID int64,
) error {
	if s.archiveLifecycle == nil {
		return nil
	}
	needsReconcile := previousID != 0 && previousID != repoID
	if !needsReconcile {
		states, err := s.db.ListArchiveRepoStates(ctx, []int64{repoID})
		if err != nil {
			return fmt.Errorf("inspect archive repository state: %w", err)
		}
		needsReconcile = len(states) == 0
	}
	if !needsReconcile {
		return nil
	}
	s.reposMu.Lock()
	tracked := slices.Clone(s.repos)
	s.reposMu.Unlock()
	refs := make([]platform.RepoRef, 0, len(tracked))
	for _, trackedRepo := range tracked {
		refs = append(refs, platformRepoRef(trackedRepo))
	}
	if _, err := s.archiveLifecycle.EnsureConfigured(ctx, refs); err != nil {
		return fmt.Errorf("reconcile archive repository replacement: %w", err)
	}
	s.WakeArchive()
	return nil
}

func repoRefFromCatalog(previous RepoRef, stored db.Repo, resolved *platform.Repository) RepoRef {
	repo := RepoRef{
		Platform:           platform.Kind(stored.Platform),
		RepoID:             stored.ID,
		Owner:              stored.Owner,
		Name:               stored.Name,
		PlatformHost:       stored.PlatformHost,
		RepoPath:           stored.RepoPath,
		PlatformExternalID: stored.PlatformRepoID,
		WebURL:             stored.WebURL,
		CloneURL:           stored.CloneURL,
		DefaultBranch:      stored.DefaultBranch,
		// The repo catalog does not record archived state or config-entry
		// provenance; without a fresh provider resolve, the previously
		// tracked values stand.
		Archived:           previous.Archived,
		ConfiguredRepoPath: previous.ConfiguredRepoPath,
	}
	if repo.PlatformRepoID == 0 {
		repo.PlatformRepoID = previous.PlatformRepoID
	}
	if repo.WebURL == "" {
		repo.WebURL = previous.WebURL
	}
	if repo.CloneURL == "" {
		repo.CloneURL = previous.CloneURL
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = previous.DefaultBranch
	}
	if resolved == nil {
		return repo
	}
	repo.Archived = resolved.Archived
	repo.PlatformRepoID = resolved.PlatformID
	if repo.PlatformRepoID == 0 {
		repo.PlatformRepoID = resolved.Ref.PlatformID
	}
	if resolved.WebURL != "" {
		repo.WebURL = resolved.WebURL
	} else if resolved.Ref.WebURL != "" {
		repo.WebURL = resolved.Ref.WebURL
	}
	if resolved.CloneURL != "" {
		repo.CloneURL = resolved.CloneURL
	} else if resolved.Ref.CloneURL != "" {
		repo.CloneURL = resolved.Ref.CloneURL
	}
	if resolved.DefaultBranch != "" {
		repo.DefaultBranch = resolved.DefaultBranch
	} else if resolved.Ref.DefaultBranch != "" {
		repo.DefaultBranch = resolved.Ref.DefaultBranch
	}
	return repo
}

func (s *Syncer) publishResolvedRepository(
	previous, resolved RepoRef, archivedAuthoritative bool,
) (RepoRef, bool) {
	s.clearDisplacedCredentialAlias(resolved)
	s.aliasRenamedCredentialRoute(previous, resolved)
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	i, ok := s.trackedRepoSlotLocked(previous, resolved)
	if !ok {
		return resolved, false
	}
	// The snapshot comparison below only means anything when the
	// publication concerns the repository the snapshot named: differing
	// snapshot and resolved ids mean the data belongs to a route
	// successor — whether it landed on the successor's own entry or is
	// displacing the snapshot's — and the snapshot's archived flag says
	// nothing about it. A conflicting slot id is cross-identity even
	// when the snapshot carries no id: the slot's occupant is not the
	// repository the provider response describes.
	slotID := strings.TrimSpace(s.repos[i].PlatformExternalID)
	previousID := strings.TrimSpace(previous.PlatformExternalID)
	resolvedID := strings.TrimSpace(resolved.PlatformExternalID)
	crossIdentity := resolvedID != "" &&
		((previousID != "" && resolvedID != previousID) ||
			(slotID != "" && resolvedID != slotID))
	sameIdentity := !crossIdentity &&
		(slotID == "" || previousID == "" || slotID == previousID)
	if sameIdentity && s.repos[i].Archived != previous.Archived {
		// A concurrent resolution flipped archived state after this
		// operation snapshotted the ref. The in-flight provider
		// response cannot be ordered against that flip, so the newer
		// tracked value stands even over fresh provider metadata.
		resolved.Archived = s.repos[i].Archived
	} else if !archivedAuthoritative {
		// Without fresh provider metadata the archived flag was
		// reconstructed from the operation's snapshot, which may
		// predate a newer flip on the tracked ref. The current
		// tracked state stays authoritative.
		resolved.Archived = s.repos[i].Archived
	}
	// Config-entry provenance is authored only by configuration
	// resolution; a publication built from an older snapshot must
	// not overwrite a value a concurrent reload just updated.
	resolved.ConfiguredRepoPath = s.repos[i].ConfiguredRepoPath
	s.repos[i] = resolved
	return resolved, true
}

// trackedRepoSlotLocked locates the tracked entry a publication should land
// on: stable provider identity first — a renamed route must still find its
// repository — then the route key, rejected when the ids conflict because
// that means the route was reused by another repository whose tracked state
// a stale publication must not overwrite. The resolved id outranks the
// snapshot id: the provider response says whose data this is, so a lookup
// keyed by a reused route lands on the successor, never on the repository
// the snapshot named. Callers hold reposMu.
func (s *Syncer) trackedRepoSlotLocked(previous, resolved RepoRef) (int, bool) {
	previousID := strings.TrimSpace(previous.PlatformExternalID)
	resolvedID := strings.TrimSpace(resolved.PlatformExternalID)
	lookupID := resolvedID
	if lookupID == "" {
		lookupID = previousID
	}
	if lookupID != "" {
		for i := range s.repos {
			if repoPlatform(s.repos[i]) == repoPlatform(previous) &&
				strings.EqualFold(repoHost(s.repos[i]), repoHost(previous)) &&
				strings.TrimSpace(s.repos[i].PlatformExternalID) == lookupID {
				return i, true
			}
		}
	}
	// Route fallback: landing on the entry this operation snapshotted is
	// legitimate even under a new resolved identity — a configured route
	// reused by a replacement repository displaces its tracked occupant,
	// and the archive lifecycle pauses the old repository. Landing on an
	// entry whose id conflicts with the snapshot is not: that entry is a
	// different repository this publication knows nothing about.
	for i := range s.repos {
		if repoPriorityKey(s.repos[i]) != repoPriorityKey(previous) {
			continue
		}
		trackedID := strings.TrimSpace(s.repos[i].PlatformExternalID)
		if trackedID != "" && previousID != "" && trackedID != previousID {
			continue
		}
		return i, true
	}
	return 0, false
}

// aliasRenamedCredentialRoute keeps GitHub credential selection on the
// configured repository identity when reconciliation publishes a renamed or
// transferred route. Provider APIs receive the resolved owner/name; the
// credential router resolves it back to the configured route.
func (s *Syncer) aliasRenamedCredentialRoute(previous, resolved RepoRef) {
	if repoPlatform(resolved) != platform.KindGitHub {
		return
	}
	if strings.EqualFold(previous.Owner, resolved.Owner) &&
		strings.EqualFold(previous.Name, resolved.Name) {
		return
	}
	router := s.routers[repoHost(resolved)]
	if router == nil {
		return
	}
	router.RegisterRepoCredentialAlias(resolved.Owner, resolved.Name, RouteKey{
		Host:  repoHost(previous),
		Owner: previous.Owner,
		Name:  previous.Name,
	}, resolved.PlatformExternalID)
}

// clearDisplacedCredentialAlias drops a credential alias for the resolved
// repository's route when a different repository recorded it — a replacement
// repository on a reused route must not inherit the displaced repository's
// credential.
func (s *Syncer) clearDisplacedCredentialAlias(resolved RepoRef) {
	if repoPlatform(resolved) != platform.KindGitHub {
		return
	}
	router := s.routers[repoHost(resolved)]
	if router == nil {
		return
	}
	router.ClearDisplacedRepoCredentialAlias(
		resolved.Owner, resolved.Name, resolved.PlatformExternalID,
	)
}

// markClosedLinkedNotificationsDone sweeps every active notification for a
// closed or merged linked item. Per-item sync paths use the scoped variants
// below because they already know which item changed.
func (s *Syncer) markClosedLinkedNotificationsDone(ctx context.Context) error {
	if err := s.db.MarkClosedLinkedNotificationsDone(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("mark closed linked notifications done: %w", err)
	}
	return nil
}

func (s *Syncer) markClosedLinkedPRNotificationsDone(ctx context.Context, repoID int64, number int) error {
	if err := s.db.MarkClosedLinkedPRNotificationsDone(ctx, time.Now().UTC(), repoID, number); err != nil {
		return fmt.Errorf("mark closed linked notifications done for PR #%d: %w", number, err)
	}
	return nil
}

func (s *Syncer) markClosedLinkedIssueNotificationsDone(ctx context.Context, repoID int64, number int) error {
	if err := s.db.MarkClosedLinkedIssueNotificationsDone(ctx, time.Now().UTC(), repoID, number); err != nil {
		return fmt.Errorf("mark closed linked notifications done for issue #%d: %w", number, err)
	}
	return nil
}

// syncRepo syncs one repository: open PRs, timeline events, and stale closures.

func (s *Syncer) syncRepo(ctx context.Context, repo RepoRef) error {
	bucket, err := s.bucketKeyForRepo(repo, false)
	if err != nil {
		return fmt.Errorf("resolve sync credential route for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	releaseProviderWork := s.beginProviderWork(bucket, archive.PriorityNormalIndex)
	defer releaseProviderWork()
	// Split-auth repository sync also issues a viewer-permission request on
	// the write identity; register that principal too so an archive sharing
	// the write PAT is preempted rather than running concurrently.
	if writeIdentity, idErr := s.identityForRepo(repo, true); idErr == nil &&
		writeIdentity.Principal != "" {
		writeBucket := RateBucketKey(
			string(repoPlatform(repo)), writeIdentity.Host, writeIdentity.Principal,
		)
		if writeBucket != bucket {
			defer s.beginProviderWork(writeBucket, archive.PriorityNormalIndex)()
		}
	}

	resolvedRef, repoID, resolvedRepo, observedAt, _, err :=
		s.reconcileRepoIdentityObservation(ctx, repo)
	if err != nil {
		return fmt.Errorf("resolve repo identity %s/%s: %w", repo.Owner, repo.Name, err)
	}
	repo = resolvedRef
	if repo.Archived {
		// Identity resolution already published the archived flag, so
		// the repo drops out of future passes; stop before any live
		// clone, overview, or item syncing touches the archived repo.
		slog.Info("repo archived upstream; skipping live sync",
			"repo", repo.Owner+"/"+repo.Name,
		)
		return nil
	}
	ctx = withCloneRepositoryIdentity(ctx, repo)
	routeFence, found, err := s.db.CurrentRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), repoID,
	)
	if err != nil {
		return fmt.Errorf(
			"capture repository route for %s/%s: %w", repo.Owner, repo.Name, err,
		)
	}
	if !found {
		return nil
	}

	// Settings refresh runs before the route guard is attached: its refetch
	// branches record fresh identity observations, and reconciliation takes
	// the reconciliation write lock that a guarded context's transactions
	// would deadlock against. Its writes carry the fence explicitly. A
	// failed settings commit aborts the sync: a route-reuse replacement row
	// must not have items indexed under it while it still advertises the
	// permissive schema defaults.
	if err := s.refreshRepoSettings(
		ctx, repo, repoID, resolvedRepo, observedAt, routeFence,
	); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		err = fmt.Errorf(
			"refresh repo settings for %s/%s: %w", repo.Owner, repo.Name, err,
		)
		s.recordAbortedRepoSync(ctx, repo, repoID, routeFence, err)
		return err
	}

	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)

	if err := s.db.UpdateRepoSyncStarted(ctx, repoID, time.Now().UTC()); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		return fmt.Errorf("mark sync started for %s/%s: %w", repo.Owner, repo.Name, err)
	}

	// Fetch bare clone before PR data so refs are available for merge-base.
	cloneFetchOK := false
	defaultBranch := s.defaultBranchForActivity(ctx, repoID, repo)
	var previousTip *db.BranchTip
	if defaultBranch != "" {
		tip, err := s.db.GetBranchTip(ctx, repoID, defaultBranch)
		if err != nil {
			slog.Warn("get default branch tip failed",
				"repo", repo.Owner+"/"+repo.Name,
				"branch", defaultBranch,
				"err", err,
			)
		} else {
			previousTip = tip
		}
	}
	if s.clones != nil {
		if err := s.ensureCloneForRoute(ctx, repo, repoID, routeFence); err != nil {
			slog.Warn("bare clone fetch failed",
				"repo", repo.Owner+"/"+repo.Name, "err", err,
			)
		} else {
			cloneFetchOK = true
			s.syncDefaultBranchActivity(ctx, repo, repoID, defaultBranch, previousTip)
		}
	}

	if client, ok := s.optionalGitHubClientFor(repo); ok {
		s.syncRepoOverview(ctx, client, repo, repoID, cloneFetchOK)
	} else {
		s.syncProviderRepoOverview(ctx, repo, repoID, cloneFetchOK)
	}

	s.syncRepoLabelCatalog(ctx, repo, repoID)

	syncErr := s.indexSyncRepo(ctx, repo, repoID, cloneFetchOK)
	if errors.Is(syncErr, db.ErrRepositoryRouteFenceChanged) {
		return nil
	}
	if err := s.markClosedLinkedNotificationsDone(ctx); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		markErr := err
		if syncErr == nil {
			syncErr = markErr
		} else {
			syncErr = errors.Join(syncErr, markErr)
		}
	}

	syncErrStr := ""
	if syncErr != nil {
		syncErrStr = syncErr.Error()
	}
	if err := s.db.UpdateRepoSyncCompleted(ctx, repoID, time.Now().UTC(), syncErrStr); err != nil {
		slog.Error("mark sync completed", "repo", repo.Owner+"/"+repo.Name, "err", err)
	}

	return syncErr
}

// recordAbortedRepoSync surfaces a sync that stopped before item indexing on
// the repository row's sync health, so the UI reports the failed attempt
// instead of presenting the previous outcome as current. Best effort under
// the route fence: a route that changed owners no longer reports this
// repository's health.
func (s *Syncer) recordAbortedRepoSync(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	routeFence db.RepositoryRouteFence,
	syncErr error,
) {
	statusCtx := s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	now := time.Now().UTC()
	if err := s.db.UpdateRepoSyncStarted(statusCtx, repoID, now); err != nil {
		if !errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			slog.Warn("record aborted sync start failed",
				"repo", repo.Owner+"/"+repo.Name, "err", err,
			)
		}
		return
	}
	if err := s.db.UpdateRepoSyncCompleted(
		statusCtx, repoID, now, syncErr.Error(),
	); err != nil && !errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		slog.Warn("record aborted sync completion failed",
			"repo", repo.Owner+"/"+repo.Name, "err", err,
		)
	}
}

func (s *Syncer) defaultBranchForActivity(ctx context.Context, repoID int64, repo RepoRef) string {
	repoRow, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		slog.Warn("get repo default branch failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return strings.TrimSpace(repo.DefaultBranch)
	}
	if repoRow != nil && strings.TrimSpace(repoRow.DefaultBranch) != "" {
		return strings.TrimSpace(repoRow.DefaultBranch)
	}
	return strings.TrimSpace(repo.DefaultBranch)
}

func (s *Syncer) syncDefaultBranchActivity(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	preferredBranch string,
	previousTip *db.BranchTip,
) {
	if s.clones == nil {
		return
	}
	host := repoHost(repo)
	branch, currentTip, err := s.clones.ResolveDefaultBranch(
		ctx,
		string(repoPlatform(repo)), host,
		repo.Owner,
		repo.Name,
		preferredBranch,
	)
	if err != nil {
		slog.Warn("resolve default branch activity ref failed",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", preferredBranch,
			"err", err,
		)
		return
	}
	if branch == "" || currentTip == "" {
		slog.Warn("default branch activity skipped: no branch resolved",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", preferredBranch,
		)
		return
	}
	if previousTip == nil || previousTip.BranchName != branch {
		previousTip, err = s.db.GetBranchTip(ctx, repoID, branch)
		if err != nil {
			slog.Warn("get resolved default branch tip failed",
				"repo", repo.Owner+"/"+repo.Name,
				"branch", branch,
				"err", err,
			)
			return
		}
	}

	now := time.Now().UTC()
	retention, maxCommits := s.branchActivityLimits()
	retentionStart := now.Add(-retention)
	afterSHA := ""
	var beforeObservedAt time.Time
	forcePush := false
	if previousTip != nil && previousTip.TipSHA != "" {
		afterSHA = previousTip.TipSHA
		beforeObservedAt = previousTip.ObservedAt
		if previousTip.TipSHA != currentTip {
			ancestor, err := s.clones.IsAncestor(
				ctx,
				string(repoPlatform(repo)), host,
				repo.Owner,
				repo.Name,
				previousTip.TipSHA,
				currentTip,
			)
			if err != nil {
				slog.Warn("check default branch ancestry failed",
					"repo", repo.Owner+"/"+repo.Name,
					"branch", branch,
					"err", err,
				)
				return
			}
			forcePush = !ancestor
		}
	}

	gitCommits, err := s.clones.ListBranchCommitsSince(
		ctx,
		string(repoPlatform(repo)), host,
		repo.Owner,
		repo.Name,
		branch,
		retentionStart,
		afterSHA,
		maxCommits,
	)
	if err != nil {
		slog.Warn("list default branch commits failed",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", branch,
			"err", err,
		)
		return
	}
	if err := s.db.UpsertBranchCommits(
		ctx,
		dbBranchCommits(repoID, branch, gitCommits),
	); err != nil {
		slog.Warn("upsert default branch commits failed",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", branch,
			"err", err,
		)
		return
	}
	if forcePush {
		if err := s.db.InsertBranchForcePush(ctx, db.BranchForcePush{
			RepoID:           repoID,
			BranchName:       branch,
			BeforeSHA:        afterSHA,
			AfterSHA:         currentTip,
			BeforeObservedAt: beforeObservedAt,
			DetectedAt:       now,
		}); err != nil {
			slog.Warn("insert default branch force push failed",
				"repo", repo.Owner+"/"+repo.Name,
				"branch", branch,
				"err", err,
			)
			return
		}
	}
	if err := s.db.UpsertBranchTip(ctx, db.BranchTip{
		RepoID:     repoID,
		BranchName: branch,
		TipSHA:     currentTip,
		ObservedAt: now,
	}); err != nil {
		slog.Warn("upsert default branch tip failed",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", branch,
			"err", err,
		)
		return
	}
	if err := s.db.PruneBranchActivity(ctx, retentionStart, maxCommits); err != nil {
		slog.Warn("prune default branch activity failed",
			"repo", repo.Owner+"/"+repo.Name,
			"branch", branch,
			"err", err,
		)
	}
}

func dbBranchCommits(
	repoID int64,
	branch string,
	commits []gitclone.Commit,
) []db.BranchCommit {
	out := make([]db.BranchCommit, 0, len(commits))
	for _, commit := range commits {
		out = append(out, db.BranchCommit{
			RepoID:         repoID,
			BranchName:     branch,
			CommitSHA:      commit.SHA,
			AuthorName:     commit.AuthorName,
			AuthorEmail:    commit.AuthorEmail,
			AuthoredAt:     commit.AuthoredAt,
			CommitterName:  commit.CommitterName,
			CommitterEmail: commit.CommitterEmail,
			CommittedAt:    commit.CommittedAt,
			Subject:        commit.Message,
		})
	}
	return out
}

// refreshRepoSettings persists provider metadata and merge settings for a
// reconciled repository. ctx must not carry a repository route guard:
// refetch branches record fresh identity observations through the
// reconciliation write lock. All writes go through the observation
// watermark, so a snapshot that lost to a newer observation is refetched
// once and then dropped rather than overwriting fresher settings. A nil
// return means the settings are committed or the provider cannot report
// them; any other outcome is an error so callers do not index items
// against unverified merge availability.
func (s *Syncer) refreshRepoSettings(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	resolvedRepo *platform.Repository,
	observedAt time.Time,
	routeFence db.RepositoryRouteFence,
) error {
	if resolvedRepo != nil {
		applied, err := s.persistRepoSettingsObservation(
			ctx, repo, repoID, observedAt, *resolvedRepo, routeFence,
		)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		// The snapshot lost to a newer observation between capture and
		// commit; fall through and fetch fresh settings.
	}

	if client, ok := s.optionalGitHubClientFor(repo); ok {
		observedAt := time.Now().UTC()
		ghRepo, err := client.GetRepository(ctx, repo.Owner, repo.Name)
		if err != nil {
			return fmt.Errorf("get repo settings: %w", err)
		}
		return s.persistRefetchedRepoSettings(
			ctx, repo, repoID, observedAt,
			gitHubPlatformRepository(repoHost(repo), repo.Owner, ghRepo),
			routeFence,
		)
	}

	reader, err := s.clients.RepositoryReader(repoPlatform(repo), repoHost(repo))
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) || errors.Is(err, platform.ErrProviderNotConfigured) {
			return nil
		}
		return fmt.Errorf("resolve repo settings reader: %w", err)
	}
	observedAt = time.Now().UTC()
	providerRepo, err := reader.GetRepository(ctx, platformRepoRef(repo))
	if err != nil {
		return fmt.Errorf("get repo settings: %w", err)
	}
	return s.persistRefetchedRepoSettings(
		ctx, repo, repoID, observedAt, providerRepo, routeFence,
	)
}

// reconcileRepoForDirectSync resolves repository identity for a direct item
// sync and persists the verified provider snapshot under the current route
// fence. The archive lifecycle records its own identity observations between
// reconciliation and this write (route changes, first encounters), which
// advances the observation watermark and rejects the held snapshot as stale;
// re-resolving fetches a fresh snapshot with a fresh timestamp so settings
// still commit instead of leaving a replacement row on permissive schema
// defaults. found=false reports a vanished route; callers skip the sync unit.
func (s *Syncer) reconcileRepoForDirectSync(
	ctx context.Context,
	repo RepoRef,
) (RepoRef, int64, db.RepositoryRouteFence, bool, error) {
	var zero db.RepositoryRouteFence
	for range 2 {
		resolvedRef, repoID, providerRepo, observedAt, accepted, err :=
			s.reconcileRepoIdentityObservation(ctx, repo)
		if err != nil {
			return RepoRef{}, 0, zero, false, fmt.Errorf(
				"resolve repo identity %s/%s: %w", repo.Owner, repo.Name, err,
			)
		}
		if !accepted {
			// The catalog rejected this observation for a newer one, so the
			// provider snapshot was discarded and the row's settings are
			// unverified. This is not the same as a provider without
			// repository reading: retry rather than sync the item against
			// possibly default merge availability.
			repo = resolvedRef
			continue
		}
		routeFence, found, err := s.db.CurrentRepositoryRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(resolvedRef)), repoID,
		)
		if err != nil {
			return RepoRef{}, 0, zero, false, fmt.Errorf(
				"capture repository route for %s/%s: %w",
				resolvedRef.Owner, resolvedRef.Name, err,
			)
		}
		if !found {
			return resolvedRef, repoID, zero, false, nil
		}
		if providerRepo == nil {
			return resolvedRef, repoID, routeFence, true, nil
		}
		applied, err := s.persistRepoSettingsObservation(
			ctx, resolvedRef, repoID, observedAt, *providerRepo, routeFence,
		)
		if err != nil {
			if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
				return RepoRef{}, 0, zero, false, err
			}
			return RepoRef{}, 0, zero, false, fmt.Errorf(
				"persist repository settings for %s/%s: %w",
				resolvedRef.Owner, resolvedRef.Name, err,
			)
		}
		if applied {
			return resolvedRef, repoID, routeFence, true, nil
		}
		repo = resolvedRef
	}
	return RepoRef{}, 0, zero, false, fmt.Errorf(
		"repository settings observation for %s/%s kept losing to newer observations",
		repo.Owner, repo.Name,
	)
}

// persistRepoSettingsObservation commits a provider repository snapshot under
// both the observation watermark and the captured route fence. applied=false
// with a nil error means the snapshot lost to a newer observation, whose
// writer carries fresher data. Errors — including
// db.ErrRepositoryRouteFenceChanged — surface to the caller: a failed write
// can leave a just-reconciled replacement row on its permissive schema
// defaults, so item syncs must not continue as if settings were committed.
func (s *Syncer) persistRepoSettingsObservation(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	observedAt time.Time,
	providerRepo platform.Repository,
	routeFence db.RepositoryRouteFence,
) (bool, error) {
	fencedCtx := s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	return s.updateRepoSettingsFromProviderObservation(
		fencedCtx, repoID, observedAt, providerRepo,
	)
}

// persistRefetchedRepoSettings records a freshly fetched repository snapshot
// as its own identity observation so its settings commit under the
// observation watermark. A snapshot whose identity no longer resolves to
// repoID reports a changed route fence; one that keeps losing the watermark
// is an error, because the repository's settings remain unverified.
func (s *Syncer) persistRefetchedRepoSettings(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	observedAt time.Time,
	providerRepo platform.Repository,
	routeFence db.RepositoryRouteFence,
) error {
	entry, accepted, err := s.db.ReconcileRepositoryObservation(
		ctx, platform.DBRepositoryIdentity(providerRepo), observedAt,
	)
	if err != nil {
		return fmt.Errorf("record repo settings observation: %w", err)
	}
	if entry.Repository.ID != repoID {
		return fmt.Errorf(
			"repo settings observation: %w for %s/%s",
			db.ErrRepositoryRouteFenceChanged, repo.Owner, repo.Name,
		)
	}
	if accepted {
		applied, err := s.persistRepoSettingsObservation(
			ctx, repo, repoID, observedAt, providerRepo, routeFence,
		)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
	}
	return fmt.Errorf(
		"repository settings observation for %s/%s kept losing to newer observations",
		repo.Owner, repo.Name,
	)
}

func (s *Syncer) updateRepoSettingsFromProviderObservation(
	ctx context.Context,
	repoID int64,
	observedAt time.Time,
	repo platform.Repository,
) (bool, error) {
	var settings *db.RepoMergeSettings
	if repo.MergeSettings != nil {
		settings = &db.RepoMergeSettings{
			AllowSquashMerge: repo.MergeSettings.AllowSquashMerge,
			AllowMergeCommit: repo.MergeSettings.AllowMergeCommit,
			AllowRebaseMerge: repo.MergeSettings.AllowRebaseMerge,
		}
	}
	return s.db.UpdateRepoProviderObservation(
		ctx,
		repoID,
		observedAt,
		db.RepoProviderMetadata{
			PlatformRepoID: repo.PlatformExternalID,
			WebURL:         repo.WebURL,
			CloneURL:       repo.CloneURL,
			DefaultBranch:  repo.DefaultBranch,
		},
		settings,
		repo.ViewerCanMerge,
	)
}

func (s *Syncer) syncRepoLabelCatalog(ctx context.Context, repo RepoRef, repoID int64) {
	checkedAt := time.Now().UTC()
	reader, err := s.labelReaderFor(repo)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) || errors.Is(err, platform.ErrProviderNotConfigured) {
			return
		}
		_ = s.db.UpdateRepoLabelCatalogCheck(ctx, repoID, checkedAt, err.Error())
		return
	}
	catalog, err := reader.ListLabels(ctx, platformRepoRef(repo))
	if err != nil {
		_ = s.db.UpdateRepoLabelCatalogCheck(ctx, repoID, checkedAt, err.Error())
		return
	}
	if catalog.NotModified {
		if err := s.db.MarkRepoLabelCatalogSynced(ctx, repoID, checkedAt); err != nil {
			slog.Warn("mark label catalog synced", "repo", repo.Owner+"/"+repo.Name, "err", err)
		}
		return
	}
	labels := platform.DBLabels(catalog.Labels, checkedAt)
	if err := s.db.ReplaceRepoLabelCatalog(ctx, repoID, labels, checkedAt); err != nil {
		slog.Warn("replace label catalog", "repo", repo.Owner+"/"+repo.Name, "err", err)
	}
}

func (s *Syncer) RefreshRepoLabelCatalog(ctx context.Context, repo db.Repo) error {
	ref := RepoRef{
		Platform:           platform.Kind(repo.Platform),
		PlatformHost:       repoProviderHostFromDB(repo),
		Owner:              repo.Owner,
		Name:               repo.Name,
		RepoPath:           repo.RepoPath,
		PlatformExternalID: repo.PlatformRepoID,
		CloneURL:           repo.CloneURL,
		WebURL:             repo.WebURL,
		DefaultBranch:      repo.DefaultBranch,
	}
	identity := platform.DBRepoIdentity(platformRepoRef(ref))
	routeFence, found, err := s.db.CurrentRepositoryRouteFence(ctx, identity, repo.ID)
	if err != nil {
		return fmt.Errorf("capture repository route for label catalog: %w", err)
	}
	if !found {
		return nil
	}
	ctx = s.db.WithRepositoryRouteFence(ctx, identity, routeFence)
	checkedAt := time.Now().UTC()
	reader, err := s.labelReaderFor(ref)
	if err != nil {
		if updateErr := s.db.UpdateRepoLabelCatalogCheck(ctx, repo.ID, checkedAt, err.Error()); updateErr != nil {
			if errors.Is(updateErr, db.ErrRepositoryRouteFenceChanged) {
				return nil
			}
			return errors.Join(err, updateErr)
		}
		return err
	}
	catalog, err := reader.ListLabels(ctx, platformRepoRef(ref))
	if err != nil {
		if updateErr := s.db.UpdateRepoLabelCatalogCheck(ctx, repo.ID, checkedAt, err.Error()); updateErr != nil {
			if errors.Is(updateErr, db.ErrRepositoryRouteFenceChanged) {
				return nil
			}
			return errors.Join(err, updateErr)
		}
		return err
	}
	if catalog.NotModified {
		err := s.db.MarkRepoLabelCatalogSynced(ctx, repo.ID, checkedAt)
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		return err
	}
	err = s.db.ReplaceRepoLabelCatalog(ctx, repo.ID, platform.DBLabels(catalog.Labels, checkedAt), checkedAt)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return nil
	}
	return err
}

const repoOverviewTimelineLimit = 30

func repoProviderHostFromDB(repo db.Repo) string {
	if repo.PlatformHost != "" {
		return repo.PlatformHost
	}
	kind := platform.Kind(repo.Platform)
	if kind == "" {
		kind = platform.KindGitHub
	}
	if host, ok := platform.DefaultHost(kind); ok {
		return host
	}
	return platform.DefaultGitHubHost
}

func (s *Syncer) syncRepoOverview(
	ctx context.Context,
	client Client,
	repo RepoRef,
	repoID int64,
	cloneFetchOK bool,
) {
	releases, err := client.ListReleases(ctx, repo.Owner, repo.Name, 10)
	if err != nil {
		slog.Warn("list repo releases failed",
			"repo", repo.Owner+"/"+repo.Name, "err", err,
		)
		return
	}

	selectedReleases := displayReleases(releases, 3)
	overview := db.RepoOverview{}
	for _, release := range selectedReleases {
		overview.Releases = append(overview.Releases, repoReleaseFromGitHub(release))
	}
	if len(overview.Releases) > 0 {
		overview.LatestRelease = &overview.Releases[0]
	}

	var timelineTags []string
	selectedTags := []*gh.RepositoryTag(nil)
	if len(selectedReleases) == 0 {
		tags, err := client.ListTags(ctx, repo.Owner, repo.Name, 3)
		if err != nil {
			slog.Warn("list repo tags failed",
				"repo", repo.Owner+"/"+repo.Name, "err", err,
			)
		} else {
			selectedTags = displayTags(tags, 3)
			for _, tag := range selectedTags {
				overview.Releases = append(overview.Releases, repoReleaseFromTag(
					repo.PlatformHost,
					repo.Owner,
					repo.Name,
					tag,
				))
			}
			if len(overview.Releases) > 0 {
				overview.LatestRelease = &overview.Releases[0]
			}
			for _, tag := range selectedTags {
				timelineTags = append(timelineTags, tag.GetName())
			}
		}
	} else {
		for _, release := range selectedReleases {
			timelineTags = append(timelineTags, release.GetTagName())
		}
	}

	s.addRepoOverviewTimeline(ctx, repo, cloneFetchOK, timelineTags, &overview)

	if err := s.db.UpsertRepoOverview(ctx, repoID, overview); err != nil {
		slog.Warn("store repo overview failed",
			"repo", repo.Owner+"/"+repo.Name, "err", err,
		)
	}
}

func (s *Syncer) syncProviderRepoOverview(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	cloneFetchOK bool,
) {
	releaseReader, err := s.releaseReaderFor(repo)
	if err != nil {
		if !errors.Is(err, platform.ErrUnsupportedCapability) {
			slog.Warn("resolve repo release reader failed",
				"repo", repo.Owner+"/"+repo.Name, "err", err,
			)
		}
		return
	}

	releases, err := releaseReader.ListReleases(ctx, platformRepoRef(repo))
	if err != nil {
		slog.Warn("list repo releases failed",
			"repo", repo.Owner+"/"+repo.Name, "err", err,
		)
		return
	}

	selectedReleases := displayPlatformReleases(releases, 3)
	overview := db.RepoOverview{}
	for _, release := range selectedReleases {
		overview.Releases = append(overview.Releases, repoReleaseFromPlatform(release))
	}
	if len(overview.Releases) > 0 {
		overview.LatestRelease = &overview.Releases[0]
	}

	timelineTags := make([]string, 0, len(selectedReleases))
	if len(selectedReleases) == 0 {
		tagReader, tagErr := s.tagReaderFor(repo)
		if tagErr != nil {
			if !errors.Is(tagErr, platform.ErrUnsupportedCapability) {
				slog.Warn("resolve repo tag reader failed",
					"repo", repo.Owner+"/"+repo.Name, "err", tagErr,
				)
			}
		} else {
			tags, tagErr := tagReader.ListTags(ctx, platformRepoRef(repo))
			if tagErr != nil {
				slog.Warn("list repo tags failed",
					"repo", repo.Owner+"/"+repo.Name, "err", tagErr,
				)
			} else {
				selectedTags := displayPlatformTags(tags, 3)
				for _, tag := range selectedTags {
					overview.Releases = append(overview.Releases, repoReleaseFromPlatformTag(tag))
					timelineTags = append(timelineTags, tag.Name)
				}
				if len(overview.Releases) > 0 {
					overview.LatestRelease = &overview.Releases[0]
				}
			}
		}
	} else {
		for _, release := range selectedReleases {
			timelineTags = append(timelineTags, release.TagName)
		}
	}

	s.addRepoOverviewTimeline(ctx, repo, cloneFetchOK, timelineTags, &overview)

	if err := s.db.UpsertRepoOverview(ctx, repoID, overview); err != nil {
		slog.Warn("store repo overview failed",
			"repo", repo.Owner+"/"+repo.Name, "err", err,
		)
	}
}

func (s *Syncer) addRepoOverviewTimeline(
	ctx context.Context,
	repo RepoRef,
	cloneFetchOK bool,
	tags []string,
	overview *db.RepoOverview,
) {
	if len(tags) == 0 || s.clones == nil || !cloneFetchOK {
		return
	}
	host := repo.PlatformHost
	if host == "" {
		host = "github.com"
	}
	latestTag := tags[0]
	count, _, countErr := s.clones.CommitTimelineSinceTag(
		ctx, string(repoPlatform(repo)), host, repo.Owner, repo.Name, latestTag, 1,
	)
	if countErr != nil {
		slog.Warn("count commits since latest version failed",
			"repo", repo.Owner+"/"+repo.Name,
			"tag", latestTag, "err", countErr,
		)
	} else {
		overview.CommitsSinceRelease = &count
	}

	timelineTag := tags[len(tags)-1]
	_, points, err := s.clones.CommitTimelineSinceTag(
		ctx, string(repoPlatform(repo)), host, repo.Owner, repo.Name,
		timelineTag, repoOverviewTimelineLimit,
	)
	if err != nil {
		slog.Warn("build repo commit timeline failed",
			"repo", repo.Owner+"/"+repo.Name,
			"tag", timelineTag, "err", err,
		)
		return
	}
	overview.CommitTimeline = make([]db.RepoCommitTimelinePoint, 0, len(points))
	for _, point := range points {
		overview.CommitTimeline = append(overview.CommitTimeline, db.RepoCommitTimelinePoint{
			SHA:         point.SHA,
			Message:     point.Message,
			CommittedAt: point.CommittedAt.UTC(),
		})
	}
	now := time.Now().UTC()
	overview.TimelineUpdatedAt = &now
}

func displayReleases(
	releases []*gh.RepositoryRelease,
	limit int,
) []*gh.RepositoryRelease {
	if limit < 1 {
		limit = 1
	}
	out := make([]*gh.RepositoryRelease, 0, limit)
	for _, release := range releases {
		if release == nil || release.GetDraft() || release.GetTagName() == "" {
			continue
		}
		out = append(out, release)
		if len(out) == limit {
			return out
		}
	}
	return out
}

func displayTags(
	tags []*gh.RepositoryTag,
	limit int,
) []*gh.RepositoryTag {
	if limit < 1 {
		limit = 1
	}
	out := make([]*gh.RepositoryTag, 0, limit)
	for _, tag := range tags {
		if tag == nil || tag.GetName() == "" {
			continue
		}
		out = append(out, tag)
		if len(out) == limit {
			return out
		}
	}
	return out
}

func displayPlatformReleases(releases []platform.Release, limit int) []platform.Release {
	if limit < 1 {
		limit = 1
	}
	out := make([]platform.Release, 0, limit)
	for _, release := range releases {
		if release.TagName == "" {
			continue
		}
		out = append(out, release)
		if len(out) == limit {
			return out
		}
	}
	return out
}

func displayPlatformTags(tags []platform.Tag, limit int) []platform.Tag {
	if limit < 1 {
		limit = 1
	}
	out := make([]platform.Tag, 0, limit)
	for _, tag := range tags {
		if tag.Name == "" {
			continue
		}
		out = append(out, tag)
		if len(out) == limit {
			return out
		}
	}
	return out
}

func repoReleaseFromGitHub(release *gh.RepositoryRelease) db.RepoRelease {
	out := db.RepoRelease{
		TagName:         release.GetTagName(),
		Name:            release.GetName(),
		URL:             release.GetHTMLURL(),
		TargetCommitish: release.GetTargetCommitish(),
		Prerelease:      release.GetPrerelease(),
	}
	publishedAt := release.GetPublishedAt().Time
	if !publishedAt.IsZero() {
		publishedAt = publishedAt.UTC()
		out.PublishedAt = &publishedAt
	}
	return out
}

func repoReleaseFromPlatform(release platform.Release) db.RepoRelease {
	out := db.RepoRelease{
		TagName:         release.TagName,
		Name:            release.Name,
		URL:             release.URL,
		TargetCommitish: release.TargetCommitish,
		Prerelease:      release.Prerelease,
	}
	if release.PublishedAt != nil && !release.PublishedAt.IsZero() {
		publishedAt := release.PublishedAt.UTC()
		out.PublishedAt = &publishedAt
	}
	return out
}

func repoReleaseFromTag(platformHost, owner, repo string, tag *gh.RepositoryTag) db.RepoRelease {
	host := platformHost
	if host == "" {
		host = "github.com"
	}
	tagName := tag.GetName()
	return db.RepoRelease{
		TagName:         tagName,
		Name:            tagName,
		URL:             "https://" + host + "/" + owner + "/" + repo + "/tree/" + url.PathEscape(tagName),
		TargetCommitish: tag.GetCommit().GetSHA(),
	}
}

func repoReleaseFromPlatformTag(tag platform.Tag) db.RepoRelease {
	return db.RepoRelease{
		TagName:         tag.Name,
		Name:            tag.Name,
		URL:             tag.URL,
		TargetCommitish: tag.SHA,
	}
}

// indexSyncRepo performs the cheap index scan: list endpoints only,
// upserting basic data without detail fetches. This runs every cycle.
func (s *Syncer) indexSyncRepo(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	cloneFetchOK bool,
) error {
	caps, err := s.clients.Capabilities(repoPlatform(repo), repoHost(repo))
	if err != nil {
		return fmt.Errorf("resolve provider capabilities for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	gitHubClient, hasGitHubClient := s.optionalGitHubClientFor(repo)
	platformRef := platformRepoRef(repo)

	// If the previous sync of this repo partially failed after the
	// ETag cache was populated by a 200 list response, a naive next
	// cycle would see 304 and skip the per-item upserts that failed
	// last time, leaving the DB stale until the TTL expired. Evict
	// the repo's list ETags so the following calls are
	// unconditional, forcing a fresh 200 that we can re-apply.
	priorFail := s.consumeRepoFailed(repo)
	forceMR := priorFail&failMR != 0
	forceIssues := priorFail&failIssues != 0
	if priorFail != 0 {
		var endpoints []string
		if forceMR {
			endpoints = append(endpoints, "pulls")
		}
		if forceIssues {
			endpoints = append(endpoints, "issues")
		}
		if hasGitHubClient {
			gitHubClient.InvalidateListETagsForRepo(repo.Owner, repo.Name, endpoints...)
		}
	}

	// Track partial-failure and disabled-feature signals per path so
	// disabled repository features never enter transient recovery.
	var attemptedScope failScope
	var failedScope failScope
	var disabledScope failScope
	// Failures caused by the local budget refusing a list fetch before
	// any wire attempt. They fail the cycle but must not evict list
	// ETags: nothing upstream was touched, so the cached validators are
	// still correct and eviction would only add unconditional-refetch
	// spend to an already-exhausted window.
	var budgetRefusedScope failScope
	var partialCause error

	preferNativeStacks := s.preferGitHubNativeStacks.Load() &&
		repoPlatform(repo) == platform.KindGitHub
	var nativeStackHints map[int]*NativeStackHint

	prListUnchanged := false
	var mrProbe repositoryFeatureProbe
	mrProbeDue := false
	if caps.ReadMergeRequests {
		mrProbe, mrProbeDue = s.beginRepositoryFeatureProbe(
			ctx, repo, platform.RepositoryFeatureMergeRequests,
		)
	}
	if caps.ReadMergeRequests && mrProbeDue {
		mrProviderAttempted := false
		defer func() {
			if mrProviderAttempted {
				mrProbe.release()
			} else {
				mrProbe.abandon()
			}
		}()
		attemptedScope |= failMR
		mrReader, err := s.mergeRequestReaderFor(repo)
		if err != nil {
			return fmt.Errorf("resolve merge request reader for %s/%s: %w", repo.Owner, repo.Name, err)
		}
		mrProviderAttempted = true
		var openMRs []platform.MergeRequest
		// Discovery of new and closed PRs rides the essential budget
		// reserve so optional background spend cannot starve it.
		listCtx := WithEssentialSyncBudget(ctx)
		if nativeReader, ok := mrReader.(interface {
			ListOpenMergeRequestsWithNativeStackHints(
				context.Context, platform.RepoRef,
			) ([]platform.MergeRequest, map[int]*NativeStackHint, error)
		}); preferNativeStacks && ok {
			openMRs, nativeStackHints, err = nativeReader.ListOpenMergeRequestsWithNativeStackHints(listCtx, platformRef)
		} else {
			openMRs, err = mrReader.ListOpenMergeRequests(listCtx, platformRef)
		}
		mrListBlocked := false
		if err != nil {
			// 304 Not Modified means the open-PR list is byte-identical
			// to the previous fetch. No PR opened, no PR closed, no
			// metadata on any open PR changed. Skip per-PR upserts and
			// closure detection — both ran on the previous sync that
			// produced the cached etag.
			if IsNotModified(err) {
				prListUnchanged = true
			} else if errors.Is(err, platform.ErrSyncBudgetExhausted) {
				// The local budget refused the request before any wire
				// attempt, so the cached list validators are still
				// correct. Marking the repo failed would evict them and
				// force an unconditional refetch next cycle — extra
				// spend exactly when the budget is already exhausted.
				return fmt.Errorf("list open PRs: %w", err)
			} else if s.recordRepositoryFeatureDisabled(
				repo, platform.RepositoryFeatureMergeRequests, err,
			) {
				disabledScope |= failMR
				mrListBlocked = true
			} else {
				s.markRepoFailed(repo, failMR)
				return fmt.Errorf("list open PRs: %w", err)
			}
		}

		if prListUnchanged {
			// The open list is unchanged, but repair work is independent
			// of that ETag and must continue advancing on every healthy MR
			// sync cycle. The detail drain handles CI updates for PRs with
			// pending checks via priority scoring.
			s.reconcileMergedActorEvents(ctx, repo, repoID)
		} else if !mrListBlocked {
			// GraphQL path: if fetcher available and not rate-limited,
			// do a bulk fetch that replaces both index upsert and
			// detail drain for complete PRs. For large repos that
			// already have indexed rows, keep the refresh incremental:
			// the list phase updates timestamps and the detail drain
			// conditionally fetches individual stale PRs.
			graphQLDone := false
			if fetcher := s.fetcherForContext(ctx, repo); fetcher != nil &&
				s.shouldUseBulkGraphQLForMRs(ctx, repo, repoID, len(openMRs)) {
				if s.graphQLReadAllowed(ctx, repo, fetcher) {
					result, gqlErr := fetcher.FetchRepoPRs(
						ctx, repo.Owner, repo.Name, preferNativeStacks,
					)
					if gqlErr != nil {
						if s.recordRepositoryFeatureDisabled(
							repo, platform.RepositoryFeatureMergeRequests, gqlErr,
						) {
							disabledScope |= failMR
							graphQLDone = true
						} else {
							slog.Warn("GraphQL fetch failed, falling back to REST index",
								"repo", repo.Owner+"/"+repo.Name,
								"err", gqlErr,
							)
						}
					} else {
						// Only a query that asked for the preview fields can
						// speak for stack membership. A GraphQL fallback that
						// dropped them must leave REST-derived hints intact.
						if preferNativeStacks && result.NativeStacksQueried {
							nativeStackHints = nativeStackHintsFromBulk(result)
						}
						if err := s.doSyncRepoGraphQL(
							ctx, repo, repoID, result, cloneFetchOK,
						); err != nil {
							partialCause = retainSyncBudgetCause(partialCause, err)
							if s.recordRepositoryFeatureDisabled(
								repo, platform.RepositoryFeatureMergeRequests, err,
							) {
								disabledScope |= failMR
								failedScope |= partialSyncFailureScope(err) & failMR
							} else {
								failedScope |= failMR
							}
						}
						graphQLDone = true
					}
				}
			}

			if !graphQLDone {
				if err := s.syncMergeRequestsFromList(
					ctx, mrReader, repo, repoID, openMRs, cloneFetchOK,
				); err != nil {
					partialCause = retainSyncBudgetCause(partialCause, err)
					if s.recordRepositoryFeatureDisabled(
						repo, platform.RepositoryFeatureMergeRequests, err,
					) {
						disabledScope |= failMR
						failedScope |= partialSyncFailureScope(err) & failMR
					} else {
						slog.Error("merge request sync failed",
							"repo", repo.Owner+"/"+repo.Name,
							"err", err,
						)
						failedScope |= failMR
					}
				}
			}
		}
	}
	if preferNativeStacks && failedScope&failMR == 0 {
		nativeResult := s.refreshGitHubNativeStackCache(
			ctx, repo, repoID, nativeStackHints, prListUnchanged,
		)
		s.nativeStackResults.Store(repoFailKey(repo), nativeResult)
	}

	// Index issues — ETag-gated, with GraphQL when available.
	// Same structure as PR sync: REST list first (ETag gate),
	// then GraphQL if available, REST fallback if not.
	issueListUnchanged := false
	var issueProbe repositoryFeatureProbe
	issueProbeDue := false
	if caps.ReadIssues {
		issueProbe, issueProbeDue = s.beginRepositoryFeatureProbe(
			ctx, repo, platform.RepositoryFeatureIssues,
		)
	}
	if caps.ReadIssues && issueProbeDue {
		issueProviderAttempted := false
		defer func() {
			if issueProviderAttempted {
				issueProbe.release()
			} else {
				issueProbe.abandon()
			}
		}()
		attemptedScope |= failIssues
		issueReader, err := s.issueReaderFor(repo)
		if err != nil {
			slog.Error("resolve issue reader failed",
				"repo", repo.Owner+"/"+repo.Name,
				"err", err,
			)
			failedScope |= failIssues
			if failedScope != 0 {
				s.markRepoFailed(repo, failedScope)
			}
			return errors.Join(
				partialCause,
				fmt.Errorf("resolve issue reader for %s/%s: %w", repo.Owner, repo.Name, err),
			)
		}
		issueProviderAttempted = true

		var openIssues []platform.Issue
		var ghIssues []*gh.Issue
		_, useGitHubIssuePath := issueReader.(interface {
			ListOpenGitHubIssues(context.Context, platform.RepoRef) ([]*gh.Issue, error)
		})
		var issueListErr error
		// Same essential-reserve treatment as the open-PR list.
		issueListCtx := WithEssentialSyncBudget(ctx)
		if rawIssueReader, ok := issueReader.(interface {
			ListOpenGitHubIssues(context.Context, platform.RepoRef) ([]*gh.Issue, error)
		}); ok && hasGitHubClient {
			// Keep GitHub's ETag-gated bulk path so index sync retains the
			// provider-only fields used by per-item detail refreshes. The raw
			// reader performs the same contract checks before persistence.
			ghIssues, issueListErr = rawIssueReader.ListOpenGitHubIssues(issueListCtx, platformRef)
		} else {
			openIssues, issueListErr = issueReader.ListOpenIssues(issueListCtx, platformRef)
		}
		if issueListErr != nil {
			if IsNotModified(issueListErr) {
				// 304: open issue list unchanged, skip.
				issueListUnchanged = true
			} else if errors.Is(issueListErr, platform.ErrSyncBudgetExhausted) {
				failedScope |= failIssues
				budgetRefusedScope |= failIssues
				partialCause = issueListErr
			} else if s.recordRepositoryFeatureDisabled(
				repo, platform.RepositoryFeatureIssues, issueListErr,
			) {
				disabledScope |= failIssues
			} else {
				slog.Error("list open issues failed",
					"repo", repo.Owner+"/"+repo.Name,
					"err", issueListErr,
				)
				failedScope |= failIssues
				budgetCause := retainSyncBudgetCause(partialCause, issueListErr)
				if errors.Is(budgetCause, platform.ErrSyncBudgetExhausted) {
					partialCause = budgetCause
				} else {
					partialCause = issueListErr
				}
			}
		} else {
			graphQLIssuesDone := false
			if fetcher := s.fetcherForContext(ctx, repo); fetcher != nil &&
				s.shouldUseBulkGraphQLForIssues(ctx, repo, repoID, len(openIssues)+len(ghIssues)) {
				if s.graphQLReadAllowed(ctx, repo, fetcher) {
					issueResult, gqlErr := fetcher.FetchRepoIssues(
						ctx, repo.Owner, repo.Name,
					)
					if gqlErr != nil {
						if s.recordRepositoryFeatureDisabled(
							repo, platform.RepositoryFeatureIssues, gqlErr,
						) {
							disabledScope |= failIssues
							graphQLIssuesDone = true
						} else {
							slog.Warn("GraphQL issue fetch failed, falling back to REST",
								"repo", repo.Owner+"/"+repo.Name,
								"err", gqlErr,
							)
						}
					} else {
						if err := s.doSyncRepoGraphQLIssues(
							ctx, repo, repoID, issueResult,
						); err != nil {
							partialCause = retainSyncBudgetCause(partialCause, err)
							if s.recordRepositoryFeatureDisabled(
								repo, platform.RepositoryFeatureIssues, err,
							) {
								disabledScope |= failIssues
								failedScope |= partialSyncFailureScope(err) & failIssues
							} else {
								failedScope |= failIssues
							}
						}
						graphQLIssuesDone = true
					}
				}
			}

			if !graphQLIssuesDone {
				if useGitHubIssuePath && hasGitHubClient {
					if err := s.syncIssuesFromList(
						ctx, gitHubClient, repo, repoID, ghIssues, forceIssues,
					); err != nil {
						partialCause = retainSyncBudgetCause(partialCause, err)
						if s.recordRepositoryFeatureDisabled(
							repo, platform.RepositoryFeatureIssues, err,
						) {
							disabledScope |= failIssues
							failedScope |= partialSyncFailureScope(err) & failIssues
						} else {
							slog.Error("REST issue sync failed",
								"repo", repo.Owner+"/"+repo.Name,
								"err", err,
							)
							failedScope |= failIssues
						}
					}
				} else {
					if err := s.syncPlatformIssuesFromList(
						ctx, issueReader, repo, repoID, openIssues, forceIssues,
					); err != nil {
						partialCause = retainSyncBudgetCause(partialCause, err)
						if s.recordRepositoryFeatureDisabled(
							repo, platform.RepositoryFeatureIssues, err,
						) {
							disabledScope |= failIssues
							failedScope |= partialSyncFailureScope(err) & failIssues
						} else {
							slog.Error("issue sync failed",
								"repo", repo.Owner+"/"+repo.Name,
								"err", err,
							)
							failedScope |= failIssues
						}
					}
				}
			}
		}
	}
	if attemptedScope&failIssues != 0 &&
		failedScope&failIssues == 0 && disabledScope&failIssues == 0 {
		issueProbe.clear()
	}
	if attemptedScope&failMR != 0 &&
		failedScope&failMR == 0 && disabledScope&failMR == 0 {
		mrProbe.clear()
	}

	if evictScope := failedScope &^ budgetRefusedScope; evictScope != 0 {
		// One or more per-item steps failed. Record which paths
		// failed so the next cycle forces an unconditional refetch
		// only for the affected list endpoints. Budget-refused list
		// fetches are excluded: their cached validators are intact.
		s.markRepoFailed(repo, evictScope)
	}
	succeededScope := attemptedScope &^ failedScope &^ disabledScope
	if succeededScope != 0 {
		s.clearRepoFailedScope(repo, succeededScope)
	}

	if caps.ReadMergeRequests && prListUnchanged &&
		failedScope&failMR == 0 && disabledScope&failMR == 0 {
		s.refreshRepoPRComments(ctx, repo)
	}
	if caps.ReadIssues && issueListUnchanged &&
		failedScope&failIssues == 0 && disabledScope&failIssues == 0 {
		s.refreshRepoIssueComments(ctx, repo)
	}

	if failedScope != 0 {
		// Surface the partially-failed cycle to the caller so repo
		// sync health (last_sync_error, sync status) records it;
		// without this, per-item failures such as a transferred or
		// inaccessible item look like a clean sync from the API. The
		// typed partial error lets consumers that depend only on an
		// unaffected scope proceed.
		return &PartialSyncError{
			MergeRequests: failedScope&failMR != 0,
			Issues:        failedScope&failIssues != 0,
			Cause:         partialCause,
		}
	}

	return nil
}

func (s *Syncer) syncMergeRequestsFromList(
	ctx context.Context,
	reader platform.MergeRequestReader,
	repo RepoRef,
	repoID int64,
	mrs []platform.MergeRequest,
	cloneFetchOK bool,
) error {
	stillOpen := make(map[int]bool, len(mrs))
	for _, mr := range mrs {
		stillOpen[mr.Number] = true
	}

	var hadItemFailure bool
	var budgetCause error
	progress := newMergeRequestSyncProgressLogger(repo, "provider", len(mrs))
	for i, mr := range mrs {
		if err := s.indexUpsertMergeRequest(ctx, repo, repoID, mr, cloneFetchOK); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failMR, hadItemFailure, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("index upsert MR failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", mr.Number,
				"err", err,
			)
			hadItemFailure = true
		}
		progress.record(i + 1)
	}

	closedNumbers, err := s.db.GetPreviouslyOpenMRNumbers(
		ctx, repoID, stillOpen,
	)
	if err != nil {
		s.markRepoFailed(repo, failMR)
		return joinPartialFailureCause(
			budgetCause, fmt.Errorf("get previously open MRs: %w", err),
		)
	}
	for _, number := range closedNumbers {
		if err := s.fetchAndUpdateClosedMergeRequest(
			ctx, reader, repo, repoID, number, cloneFetchOK,
		); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failMR, hadItemFailure, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("update closed MR failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			hadItemFailure = true
		}
	}

	s.reconcileMergedActorEvents(ctx, repo, repoID)

	if hadItemFailure {
		return partialItemFailureError(
			"one or more merge request sync items failed", budgetCause,
		)
	}
	progress.done()
	return nil
}

// mergedActorBackfillPerSync caps how many merged MRs are re-fetched per
// sync cycle to backfill a missing merged actor; mergedActorBackfillWindow
// bounds how far back the backfill looks so a provider that never reports
// the actor cannot cause an unbounded refetch loop.
const (
	mergedActorBackfillPerSync       = 10
	mergedActorBackfillWindow        = 90 * 24 * time.Hour
	mergedActorBackfillSweepInterval = time.Hour
)

type mergedActorSweepState struct {
	Cursor    db.MergedMRMissingActorCursor
	RestartAt time.Time
}

// reconcileMergedActorEvents backfills authored merged events for MRs that
// were marked merged without one. Merges performed through forge itself
// eagerly write state=merged, which suppresses the open->closed transition
// in syncMergeRequestsFromList — the only sync path that fetches the MR
// with its merged_by actor. Failures are logged, not propagated: this is a
// repair pass and must not fail an otherwise healthy sync cycle.
//
// Each cycle takes the next batch below the per-repo sweep cursor instead
// of a fixed newest-first batch: candidates whose provider permanently
// reports no actor (e.g. the merging account was deleted) would otherwise
// occupy every batch and starve older candidates. An exhausted sweep retains
// a bounded cooldown, so persistent candidates are retried once per interval,
// not once per sync cycle.
func (s *Syncer) reconcileMergedActorEvents(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
) {
	now := s.nowUTC()
	since := now.Add(-mergedActorBackfillWindow)
	cursor, ready := s.mergedActorSweepCursor(repoID, now)
	if !ready {
		return
	}
	missing, err := s.db.GetMergedMRNumbersMissingMergedActor(
		ctx, repoID, since, cursor,
		mergedActorBackfillPerSync,
	)
	if err != nil {
		slog.Error("list merged MRs missing merged actor",
			"repo", repo.Owner+"/"+repo.Name, "err", err)
		return
	}
	if len(missing) < mergedActorBackfillPerSync {
		s.setMergedActorSweepState(repoID, mergedActorSweepState{
			RestartAt: now.Add(mergedActorBackfillSweepInterval),
		})
	} else {
		last := missing[len(missing)-1]
		s.setMergedActorSweepState(repoID, mergedActorSweepState{
			Cursor: db.MergedMRMissingActorCursor{
				MergedAt:       last.MergedAt,
				MergeRequestID: last.MergeRequestID,
			},
		})
	}
	for _, candidate := range missing {
		changed, err := s.backfillMergedActorEvent(ctx, repo, repoID, candidate.Number)
		if err != nil {
			slog.Warn("backfill merged actor failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", candidate.Number,
				"err", err)
			continue
		}
		if changed && s.onMergedActorRepaired != nil {
			s.onMergedActorRepaired(ctx, repoID, candidate.Number)
		}
	}
}

// mergedActorSweepCursor returns the exclusive composite upper bound for the
// repo's next backfill batch. Exhausted sweeps retain a cooldown before a new
// sweep starts from the top of the window; the initial timestamp sits ahead of
// now so a merge stamped by a marginally faster clock is still swept.
func (s *Syncer) mergedActorSweepCursor(
	repoID int64,
	now time.Time,
) (db.MergedMRMissingActorCursor, bool) {
	s.mergedActorCursorMu.Lock()
	defer s.mergedActorCursorMu.Unlock()
	state, ok := s.mergedActorCursors[repoID]
	if ok && !state.RestartAt.IsZero() {
		if now.Before(state.RestartAt) {
			return db.MergedMRMissingActorCursor{}, false
		}
		delete(s.mergedActorCursors, repoID)
		ok = false
	}
	if !ok {
		return db.MergedMRMissingActorCursor{
			MergedAt:       now.Add(time.Hour),
			MergeRequestID: 1<<63 - 1,
		}, true
	}
	return state.Cursor, true
}

func (s *Syncer) setMergedActorSweepState(
	repoID int64,
	state mergedActorSweepState,
) {
	s.mergedActorCursorMu.Lock()
	defer s.mergedActorCursorMu.Unlock()
	if s.mergedActorCursors == nil {
		s.mergedActorCursors = make(map[int64]mergedActorSweepState)
	}
	s.mergedActorCursors[repoID] = state
}

// BackfillMergedActorEventOnProvider records the authored merged lifecycle
// event for one MR by re-fetching it from the provider, reporting whether an
// event was inserted. The merge mutation calls this in the background after
// a successful merge: its eager state=merged write both suppresses the
// sync-side closed transition and advances updated_at past the provider's,
// so a full snapshot resync would be rejected as stale and never persist the
// actor. The provider target is resolved from the stable repository row for
// repoID so a configuration rename between the mutation and this background
// pass cannot fetch from a different repository than the row being updated.
func (s *Syncer) BackfillMergedActorEventOnProvider(
	ctx context.Context,
	repoID int64,
	number int,
) (bool, error) {
	stored, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		return false, fmt.Errorf("get repo %d for merged-actor backfill: %w", repoID, err)
	}
	if stored == nil {
		return false, fmt.Errorf("repo %d is not known for merged-actor backfill", repoID)
	}
	kind := platform.Kind(stored.Platform)
	providerID := strings.TrimSpace(stored.PlatformRepoID)
	if providerID == "" {
		return false, fmt.Errorf(
			"repo %d has no stable provider ID for merged-actor backfill", repoID,
		)
	}
	repo, ok := s.trackedRepoByProviderID(kind, stored.PlatformHost, providerID)
	if !ok {
		routed, routeOK := s.trackedRepoByIdentity(
			kind, stored.Owner, stored.Name, stored.PlatformHost,
		)
		if !routeOK {
			return false, fmt.Errorf(
				"repo %s/%s on %s/%s with provider ID %q is not tracked",
				stored.Owner, stored.Name, stored.Platform, stored.PlatformHost, providerID,
			)
		}
		if routedID := strings.TrimSpace(routed.PlatformExternalID); routedID != "" {
			return false, fmt.Errorf(
				"tracked repo %s/%s provider ID %q does not match stored provider ID %q",
				stored.Owner, stored.Name, routedID, providerID,
			)
		}
		repo = routed
	}
	repo = repoRefFromStoredIdentity(repo, *stored)
	bucket, err := s.bucketKeyForRepo(repo, false)
	if err != nil {
		return false, fmt.Errorf(
			"resolve merged-actor credential route for %s/%s: %w",
			repo.Owner, repo.Name, err,
		)
	}
	releaseProviderWork := s.beginProviderWork(bucket, archive.PriorityActiveDetail)
	defer releaseProviderWork()
	return s.backfillMergedActorEvent(ctx, repo, repoID, number)
}

// repoRefFromStoredIdentity preserves the current provider-verified route
// while attaching stable identity and non-routing metadata from persistence.
func repoRefFromStoredIdentity(tracked RepoRef, stored db.Repo) RepoRef {
	repo := tracked
	repo.Platform = platform.Kind(stored.Platform)
	repo.PlatformHost = stored.PlatformHost
	repo.RepoID = stored.ID
	repo.PlatformExternalID = stored.PlatformRepoID
	if repo.WebURL == "" {
		repo.WebURL = stored.WebURL
	}
	if repo.CloneURL == "" {
		repo.CloneURL = stored.CloneURL
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = stored.DefaultBranch
	}
	return repo
}

// backfillMergedActorEvent persists the authored merged event from a fresh
// provider fetch, bypassing the parent-snapshot staleness gate — the event
// is keyed to the MR row and does not depend on snapshot acceptance. It
// reports whether an event was inserted.
func (s *Syncer) backfillMergedActorEvent(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
) (bool, error) {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return false, fmt.Errorf(
			"check MR #%d for merged-actor backfill: %w", number, err,
		)
	}
	if removed {
		return false, nil
	}
	stored, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return false, fmt.Errorf("get MR #%d for merged-actor backfill: %w", number, err)
	}
	if stored == nil {
		return false, nil
	}
	mrReader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return false, fmt.Errorf(
			"resolve merge request reader for %s/%s: %w", repo.Owner, repo.Name, err,
		)
	}
	mr, err := mrReader.GetMergeRequest(ctx, platformRepoRef(repo), number)
	if err != nil {
		return false, fmt.Errorf("get MR #%d for merged-actor backfill: %w", number, err)
	}
	storedRepo, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		return false, fmt.Errorf("get repo %d for merged-actor identity check: %w", repoID, err)
	}
	if storedRepo == nil {
		return false, fmt.Errorf("repo %d disappeared during merged-actor backfill", repoID)
	}
	if err := s.verifyMergedActorBackfillIdentity(
		ctx, repo, strings.TrimSpace(storedRepo.PlatformRepoID),
	); err != nil {
		return false, err
	}
	inserted, err := s.persistMergedActorEvent(
		ctx, stored.ID, 0, mr.MergedBy, stored.MergedAt,
	)
	if err != nil {
		return false, fmt.Errorf(
			"persist merged lifecycle event for MR #%d: %w", number, err,
		)
	}
	return inserted, nil
}

func (s *Syncer) verifyMergedActorBackfillIdentity(
	ctx context.Context,
	repo RepoRef,
	expectedProviderID string,
) error {
	if expectedProviderID == "" {
		return errors.New("merged-actor backfill requires a stable provider ID")
	}
	reader, err := s.clients.RepositoryReader(repoPlatform(repo), repoHost(repo))
	if err != nil {
		return fmt.Errorf("resolve repository reader for merged-actor identity check: %w", err)
	}
	observed, err := reader.GetRepository(ctx, platformRepoRef(repo))
	if err != nil {
		return fmt.Errorf(
			"verify repository identity before merged-actor persistence: %w", err,
		)
	}
	observedProviderID := strings.TrimSpace(
		platform.DBRepositoryIdentity(observed).PlatformRepoID,
	)
	if observedProviderID == "" {
		return errors.New("provider returned no repository ID during merged-actor identity check")
	}
	if observedProviderID != expectedProviderID {
		return fmt.Errorf(
			"repository route %s/%s changed provider ID from %q to %q during merged-actor backfill",
			repo.Owner, repo.Name, expectedProviderID, observedProviderID,
		)
	}
	return nil
}

func (s *Syncer) shouldUseBulkGraphQLForMRs(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	listCount int,
) bool {
	localOpenCount, err := s.db.CountOpenMergeRequestsForRepo(ctx, repoID)
	if err != nil {
		slog.Warn("count existing merge requests before GraphQL bulk fetch failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return true
	}
	if localOpenCount < largeRepoBulkGraphQLThreshold {
		return true
	}
	slog.Info("skipping GraphQL merge request bulk fetch for large existing repo",
		"repo", repo.Owner+"/"+repo.Name,
		"platform", repoPlatform(repo),
		"host", repoHost(repo),
		"local_open_total", localOpenCount,
		"fetched_total", listCount,
	)
	return false
}

func (s *Syncer) shouldUseBulkGraphQLForIssues(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	listCount int,
) bool {
	localOpenCount, err := s.db.CountOpenIssuesForRepo(ctx, repoID)
	if err != nil {
		slog.Warn("count existing issues before GraphQL bulk fetch failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return true
	}
	if localOpenCount < largeRepoBulkGraphQLThreshold {
		return true
	}
	slog.Info("skipping GraphQL issue bulk fetch for large existing repo",
		"repo", repo.Owner+"/"+repo.Name,
		"platform", repoPlatform(repo),
		"host", repoHost(repo),
		"local_open_total", localOpenCount,
		"fetched_total", listCount,
	)
	return false
}

func (s *Syncer) indexUpsertMergeRequest(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	mr platform.MergeRequest,
	cloneFetchOK bool,
) error {
	normalized := platform.DBMergeRequest(repoID, mr)

	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, mr.Number,
	)
	if err != nil {
		return fmt.Errorf(
			"get existing MR #%d: %w", mr.Number, err,
		)
	}

	// Preserve fields list endpoints commonly omit.
	needsCIDetailRefresh := false
	if existing != nil {
		if !mr.AdditionsKnown {
			normalized.Additions = existing.Additions
		}
		if !mr.DeletionsKnown {
			normalized.Deletions = existing.Deletions
		}
		preservePlatformBaseSHAIfOmitted(normalized, existing)
		preserveReviewDecisionIfOmitted(normalized, existing)
		preserveMergeableStateIfOmitted(normalized, existing)
		needsCIDetailRefresh = preserveCIStateIfOmitted(normalized, existing)
	}

	if normalized.Author != "" &&
		normalized.AuthorDisplayName == "" {
		if client, ok := s.optionalGitHubClientFor(repo); ok {
			if name, found := s.resolveDisplayName(
				ctx, client, repo, normalized.Author,
			); found {
				normalized.AuthorDisplayName = name
			}
		}
		if normalized.AuthorDisplayName == "" && existing != nil {
			normalized.AuthorDisplayName =
				existing.AuthorDisplayName
		}
	}

	mrID, revision, accepted, err := s.CommitMergeRequestParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf(
			"upsert MR #%d: %w", mr.Number, err,
		)
	}
	if !accepted {
		return nil
	}

	if needsCIDetailRefresh {
		detailCleared, err := s.db.ClearMRDetailFetchedSnapshot(ctx, mrID, revision)
		if err != nil {
			return fmt.Errorf(
				"clear detail fetch marker for MR #%d: %w",
				mr.Number, err,
			)
		}
		if !detailCleared {
			return nil
		}
	}
	if _, err := s.persistMergedActorEvent(ctx, mrID, revision, mr.MergedBy, normalized.MergedAt); err != nil {
		return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", mr.Number, err)
	}

	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		return fmt.Errorf(
			"ensure kanban state for MR #%d: %w",
			mr.Number, err,
		)
	}

	// Record the reviewed diff snapshot so head-binding providers can
	// pin mutations after a plain list sync — without it the head
	// stays unreviewable and head-bound actions 409 with head_unknown.
	// Skipped when the stored snapshot already covers this head/base
	// pair: recomputing the merge-base for every open MR on every
	// cycle would add unbounded git work to large repos. Non-fatal
	// like the bulk GitHub path: a missed snapshot only delays
	// head-bound actions until the next successful sync.
	snapshotCurrent := existing != nil &&
		existing.DiffHeadSHA == normalized.PlatformHeadSHA &&
		existing.DiffBaseSHA == normalized.PlatformBaseSHA &&
		existing.MergeBaseSHA != ""
	if s.clones != nil && cloneFetchOK && !snapshotCurrent {
		if err := s.syncProviderMRDiff(
			ctx, repo, repoID, mrID, revision, mr.Number,
			normalized, false, db.RepositoryRouteFence{},
		); err != nil {
			if errors.Is(err, errParentSnapshotAdvanced) {
				return nil
			}
			slog.Warn("provider diff snapshot failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", mr.Number, "err", err,
			)
		}
	}

	if existing != nil &&
		existing.DetailFetchedAt != nil &&
		existing.UpdatedAt.Equal(normalized.UpdatedAt) {
		s.queuePRCommentSync(repo, existing.RepoID, existing.Number)
	}

	return nil
}

// reclassifyWorkspaceHeadRepoTrust recomputes and persists the head-repo
// trust classification for a workspace tracking this merge request, after a
// sync upsert may have changed head_repo_clone_url. It is called only from
// CommitMergeRequestParentSnapshot, so it runs for every MR-upsert path in
// this file (REST list sync, GraphQL bulk fetch, detail refresh, and
// closed-MR refresh) rather than just one of them. Without this fan-out, a
// workspace created (or last refreshed) while a PR was same-repo never
// learns that the PR's head was later retargeted to a fork until another
// workspace setup, retry, or agent-context refresh. Reads the just-upserted
// MR row rather than the incoming provider snapshot so a list sync that
// reports an unknown head (platform.MergeRequest.HeadRepoCloneURLUnknown)
// cannot downgrade a classification the stored row already resolved. The
// workspace lookup runs first and is a cheap no-op for the common case of no
// tracking workspace, since this now runs on every accepted MR upsert in
// every sync cycle. Failures are logged, not returned: like other non-fatal
// per-item persistence steps in this sync path (for example
// syncProviderMRDiff), a missed reclassification only delays the workspace
// catching up, so it must not fail the sync.
func (s *Syncer) reclassifyWorkspaceHeadRepoTrust(
	ctx context.Context, repo RepoRef, repoID int64, mrNumber int,
) {
	releaseReconciliation, err := s.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		slog.Error("lock repository reconciliation for head-repo trust reclassification failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", mrNumber, "err", err,
		)
		return
	}
	defer releaseReconciliation()

	s.reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead(
		ctx, repo, repoID, mrNumber,
	)
}

func (s *Syncer) reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead(
	ctx context.Context, repo RepoRef, repoID int64, mrNumber int,
) {
	ws, err := s.db.GetWorkspaceByMRForProvider(
		ctx, string(repoPlatform(repo)), repoHost(repo), repo.Owner, repo.Name, mrNumber,
	)
	if err != nil {
		slog.Error("look up workspace for head-repo trust reclassification failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", mrNumber, "err", err,
		)
		return
	}
	if ws == nil {
		return
	}

	for {
		stored, err := s.db.GetMergeRequestByRepoIDAndNumber(
			ctx, repoID, mrNumber,
		)
		if err != nil {
			slog.Error("look up stored merge request for head-repo trust reclassification failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", mrNumber, "err", err,
			)
			return
		}
		cloneURL := ""
		var snapshotRevision int64
		removed, visibilityErr := s.db.IsArchiveItemRemovedUpstream(
			ctx, repoID, db.ArchiveItemTypeMergeRequest, mrNumber,
		)
		if visibilityErr != nil {
			slog.Error("check workspace merge request visibility for head-repo trust reclassification failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", mrNumber, "err", visibilityErr,
			)
			return
		}
		if stored != nil {
			snapshotRevision = stored.SnapshotRevision
			if !removed {
				cloneURL = stored.HeadRepoCloneURL
			}
			if !removed && stored.HeadRepoIdentityStale {
				return
			}
		}

		refreshed := workspace.WorkspaceHeadRepo(
			ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName, cloneURL,
		)
		if s.afterHeadRepoSnapshotRead != nil {
			s.afterHeadRepoSnapshotRead()
		}
		applied, updateErr := s.db.UpdateWorkspaceMRHeadRepoForSnapshot(
			ctx,
			ws.ID,
			repoID,
			mrNumber,
			snapshotRevision,
			removed,
			refreshed,
		)
		if updateErr != nil {
			slog.Error("persist reclassified workspace head-repo trust failed",
				"workspace_id", ws.ID,
				"repo", repo.Owner+"/"+repo.Name,
				"number", mrNumber, "err", updateErr,
			)
			return
		}
		if applied {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// indexUpsertMR upserts a PR from list endpoint data only. No
// GetPullRequest, no timeline, no CI. Preserves fields that the
// list endpoint does not return (additions, deletions,
// mergeable_state, cached CI) from the existing DB row.
func (s *Syncer) indexUpsertMR(
	ctx context.Context,
	client Client,
	repo RepoRef,
	repoID int64,
	ghPR *gh.PullRequest,
) error {
	normalized, err := NormalizePR(repoID, ghPR)
	if err != nil {
		return fmt.Errorf("normalize MR #%d: %w", ghPR.GetNumber(), err)
	}

	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, ghPR.GetNumber(),
	)
	if err != nil {
		return fmt.Errorf(
			"get existing MR #%d: %w", ghPR.GetNumber(), err,
		)
	}

	// Preserve fields the list endpoint doesn't return.
	needsCIDetailRefresh := false
	if existing != nil {
		normalized.Additions = existing.Additions
		normalized.Deletions = existing.Deletions
		preservePlatformBaseSHAIfOmitted(normalized, existing)
		preserveReviewDecisionIfOmitted(normalized, existing)
		preserveMergeableStateIfOmitted(normalized, existing)
		needsCIDetailRefresh = preserveCIStateIfOmitted(normalized, existing)
	}

	if normalized.Author != "" &&
		normalized.AuthorDisplayName == "" {
		if name, ok := s.resolveDisplayName(
			ctx, client, repo, normalized.Author,
		); ok {
			normalized.AuthorDisplayName = name
		} else if existing != nil {
			normalized.AuthorDisplayName =
				existing.AuthorDisplayName
		}
	}

	mrID, revision, accepted, err := s.CommitMergeRequestParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf(
			"upsert MR #%d: %w", ghPR.GetNumber(), err,
		)
	}
	if !accepted {
		return nil
	}
	if needsCIDetailRefresh {
		detailCleared, err := s.db.ClearMRDetailFetchedSnapshot(ctx, mrID, revision)
		if err != nil {
			return fmt.Errorf(
				"clear detail fetch marker for MR #%d: %w",
				ghPR.GetNumber(), err,
			)
		}
		if !detailCleared {
			return nil
		}
	}
	if _, err := s.persistMergedTransitionEvent(ctx, mrID, revision, ghPR, normalized.MergedAt); err != nil {
		return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", ghPR.GetNumber(), err)
	}

	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		return fmt.Errorf(
			"ensure kanban state for MR #%d: %w",
			ghPR.GetNumber(), err,
		)
	}

	if existing != nil &&
		existing.DetailFetchedAt != nil &&
		existing.UpdatedAt.Equal(normalized.UpdatedAt) {
		s.queuePRCommentSync(repo, existing.RepoID, existing.Number)
	}

	return nil
}

const largeCommentThreadThreshold = 100

func (s *Syncer) listCommentsForRefresh(
	ctx context.Context,
	client Client,
	repo RepoRef,
	number int,
	knownCount int,
) ([]*gh.IssueComment, error) {
	if knownCount >= largeCommentThreadThreshold {
		return client.ListIssueComments(
			ctx, repo.Owner, repo.Name, number,
		)
	}
	return client.ListIssueCommentsIfChanged(
		ctx, repo.Owner, repo.Name, number,
	)
}

func (s *Syncer) refreshPRCommentsForItem(
	ctx context.Context,
	client Client,
	repo RepoRef,
	pr *db.MergeRequest,
) (bool, bool) {
	if pr == nil || pr.DetailFetchedAt == nil {
		return false, false
	}
	if !s.canSpendCommentRefresh(repo) {
		return false, false
	}
	comments, err := s.listCommentsForRefresh(
		ctx, client, repo, pr.Number, pr.CommentCount,
	)
	if err != nil {
		if IsNotModified(err) {
			return true, false
		}
		if s.recordGitHubRepositoryFeatureDisabled(
			repo, platform.RepositoryFeatureMergeRequests, err,
		) {
			return true, true
		}
		slog.Warn("comment refresh: list PR comments failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", pr.Number,
			"err", err,
		)
		return true, false
	}
	if err := s.persistPRComments(ctx, repo, pr, comments); err != nil {
		client.InvalidateListETagsForRepo(repo.Owner, repo.Name, "comments")
		slog.Warn("comment refresh: persist PR comments failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", pr.Number,
			"err", err,
		)
	}
	return true, false
}

func (s *Syncer) refreshIssueCommentsForItem(
	ctx context.Context,
	client Client,
	repo RepoRef,
	issue *db.Issue,
) (bool, bool) {
	if issue == nil || issue.DetailFetchedAt == nil {
		return false, false
	}
	if !s.canSpendCommentRefresh(repo) {
		return false, false
	}
	comments, err := s.listCommentsForRefresh(
		ctx, client, repo, issue.Number, issue.CommentCount,
	)
	if err != nil {
		if IsNotModified(err) {
			return true, false
		}
		if s.recordGitHubRepositoryFeatureDisabled(
			repo, platform.RepositoryFeatureIssues, err,
		) {
			return true, true
		}
		slog.Warn("comment refresh: list issue comments failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", issue.Number,
			"err", err,
		)
		return true, false
	}
	if err := s.persistIssueComments(ctx, repo, issue, comments); err != nil {
		client.InvalidateListETagsForRepo(repo.Owner, repo.Name, "comments")
		slog.Warn("comment refresh: persist issue comments failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", issue.Number,
			"err", err,
		)
	}
	return true, false
}

func (s *Syncer) resetPendingCommentSyncs() {
	s.commentRefreshMu.Lock()
	defer s.commentRefreshMu.Unlock()
	s.pendingPRCommentSyncs = nil
	s.pendingIssueCommentSyncs = nil
}

func (s *Syncer) queuePRCommentSync(repo RepoRef, repoID int64, number int) {
	s.commentRefreshMu.Lock()
	defer s.commentRefreshMu.Unlock()
	s.pendingPRCommentSyncs = append(s.pendingPRCommentSyncs, queuedPRCommentSync{
		repo: repo, repoID: repoID,
		number: number,
	})
}

func (s *Syncer) queueIssueCommentSync(repo RepoRef, repoID int64, number int) {
	s.commentRefreshMu.Lock()
	defer s.commentRefreshMu.Unlock()
	s.pendingIssueCommentSyncs = append(s.pendingIssueCommentSyncs, queuedIssueCommentSync{
		repo: repo, repoID: repoID,
		number: number,
	})
}

func (s *Syncer) commentRefreshRouteContext(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
) (context.Context, bool, error) {
	identity := platform.DBRepoIdentity(platformRepoRef(repo))
	fence, found, err := s.db.CurrentRepositoryRouteFence(ctx, identity, repoID)
	if err != nil || !found {
		return ctx, found, err
	}
	return s.db.WithRepositoryRouteFence(ctx, identity, fence), true, nil
}

func (s *Syncer) drainPendingCommentSyncs(
	ctx context.Context,
	eligibleHosts map[string]bool,
) {
	s.commentRefreshMu.Lock()
	prs := slices.Clone(s.pendingPRCommentSyncs)
	issues := slices.Clone(s.pendingIssueCommentSyncs)
	s.pendingPRCommentSyncs = nil
	s.pendingIssueCommentSyncs = nil
	s.commentRefreshMu.Unlock()

	for _, item := range prs {
		if ctx.Err() != nil {
			return
		}
		bucket, err := s.bucketKeyForRepo(item.repo, false)
		if err != nil || !eligibleHosts[bucket] {
			continue
		}
		// Re-read the reserve per item: this loop can be long, so a credential
		// with headroom when the drain started can reach its reserve partway
		// through. Revoking the bucket stops the rest of its items too.
		if s.backgroundReserveExhausted(item.repo, QuotaResourceREST, false) {
			eligibleHosts[bucket] = false
			continue
		}
		refreshCtx, found, err := s.commentRefreshRouteContext(
			ctx, item.repo, item.repoID,
		)
		if err != nil {
			slog.Warn("comment refresh: capture PR repo route failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		if !found {
			continue
		}
		client, err := s.clientFor(item.repo)
		if err != nil {
			slog.Warn("comment refresh: resolve client failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		pr, err := s.db.GetVisibleMergeRequestByRepoIDAndNumber(
			refreshCtx, item.repoID, item.number,
		)
		if err != nil {
			slog.Warn("comment refresh: get PR failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		if pr == nil {
			continue
		}
		probe, due := s.beginRepositoryFeatureProbe(
			refreshCtx, item.repo, platform.RepositoryFeatureMergeRequests,
		)
		if !due {
			continue
		}
		providerAttempted, _ := s.refreshPRCommentsForItem(
			refreshCtx, client, item.repo, pr,
		)
		if providerAttempted {
			probe.release()
		} else {
			probe.abandon()
		}
	}

	for _, item := range issues {
		if ctx.Err() != nil {
			return
		}
		bucket, err := s.bucketKeyForRepo(item.repo, false)
		if err != nil || !eligibleHosts[bucket] {
			continue
		}
		// Issue refreshes spend the same credential as the pull-request loop
		// above and need the same per-item reserve check: an issue-only queue
		// would otherwise keep going after the credential reaches its reserve.
		if s.backgroundReserveExhausted(item.repo, QuotaResourceREST, false) {
			eligibleHosts[bucket] = false
			continue
		}
		refreshCtx, found, err := s.commentRefreshRouteContext(
			ctx, item.repo, item.repoID,
		)
		if err != nil {
			slog.Warn("comment refresh: capture issue repo route failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		if !found {
			continue
		}
		client, err := s.clientFor(item.repo)
		if err != nil {
			slog.Warn("comment refresh: resolve client failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		issue, err := s.db.GetVisibleIssueByRepoIDAndNumber(
			refreshCtx, item.repoID, item.number,
		)
		if err != nil {
			slog.Warn("comment refresh: get issue failed",
				"repo", item.repo.Owner+"/"+item.repo.Name,
				"number", item.number,
				"err", err,
			)
			continue
		}
		if issue == nil {
			continue
		}
		probe, due := s.beginRepositoryFeatureProbe(
			refreshCtx, item.repo, platform.RepositoryFeatureIssues,
		)
		if !due {
			continue
		}
		providerAttempted, _ := s.refreshIssueCommentsForItem(
			refreshCtx, client, item.repo, issue,
		)
		if providerAttempted {
			probe.release()
		} else {
			probe.abandon()
		}
	}
}

// doSyncRepoGraphQL processes bulk GraphQL results for a repo.
func (s *Syncer) doSyncRepoGraphQL(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	result *RepoBulkResult,
	cloneFetchOK bool,
) error {
	var failedScope failScope
	var budgetCause error
	stillOpen := make(map[int]bool, len(result.PullRequests))
	progress := newMergeRequestSyncProgressLogger(repo, "graphql", len(result.PullRequests))

	for i := range result.PullRequests {
		bulk := &result.PullRequests[i]
		number := bulk.PR.GetNumber()
		stillOpen[number] = true

		if err := s.syncOpenMRFromBulk(
			ctx, repo, repoID, bulk, cloneFetchOK,
		); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failMR, failedScope&failMR != 0, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("GraphQL sync MR failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			failedScope |= failMR
		}
		progress.record(i + 1)
	}

	// Detect closed PRs — same as REST path.
	closedNumbers, err := s.db.GetPreviouslyOpenMRNumbers(
		ctx, repoID, stillOpen,
	)
	if err != nil {
		return joinPartialFailureCause(
			budgetCause, fmt.Errorf("get previously open MRs: %w", err),
		)
	}
	for _, number := range closedNumbers {
		if err := s.fetchAndUpdateClosed(
			ctx, repo, repoID, number, cloneFetchOK,
		); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failMR, failedScope&failMR != 0, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("update closed MR failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			failedScope |= failMR
		}
	}

	s.reconcileMergedActorEvents(ctx, repo, repoID)

	if failedScope != 0 {
		return partialItemFailureError("GraphQL sync had partial failures", budgetCause)
	}
	progress.done()
	return nil
}

// doSyncRepoGraphQLIssues processes bulk GraphQL results for issues.
func (s *Syncer) doSyncRepoGraphQLIssues(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	result *RepoBulkResult,
) error {
	var failedScope failScope
	var budgetCause error
	stillOpen := make(map[int]bool, len(result.Issues))
	progress := newIssueSyncProgressLogger(repo, "graphql", len(result.Issues))

	for i := range result.Issues {
		bulk := &result.Issues[i]
		number := bulk.Issue.GetNumber()
		stillOpen[number] = true

		if err := s.syncOpenIssueFromBulk(
			ctx, repo, repoID, bulk,
		); err != nil {
			if disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureIssues, err,
			); disabledErr != nil {
				return preservePartialSyncFailure(
					failIssues, failedScope&failIssues != 0,
					joinPartialFailureCause(budgetCause, disabledErr),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("GraphQL sync issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			failedScope |= failIssues
		}
		progress.record(i + 1)
	}

	// Detect closed issues — same as REST path.
	closedNumbers, err := s.db.GetPreviouslyOpenIssueNumbers(
		ctx, repoID, stillOpen,
	)
	if err != nil {
		return joinPartialFailureCause(
			budgetCause, fmt.Errorf("get previously open issues: %w", err),
		)
	}
	for _, number := range closedNumbers {
		if err := s.fetchAndUpdateClosedIssue(
			ctx, repo, repoID, number,
		); err != nil {
			if disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureIssues, err,
			); disabledErr != nil {
				return preservePartialSyncFailure(
					failIssues, failedScope&failIssues != 0,
					joinPartialFailureCause(budgetCause, disabledErr),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("update closed issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			failedScope |= failIssues
		}
	}

	if failedScope != 0 {
		return partialItemFailureError("GraphQL issue sync had partial failures", budgetCause)
	}
	progress.done()
	return nil
}

// syncOpenIssueFromBulk processes a single issue from GraphQL bulk
// results. Uses pre-fetched data instead of per-issue REST calls.
func (s *Syncer) syncOpenIssueFromBulk(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	bulk *BulkIssue,
) error {
	number := bulk.Issue.GetNumber()
	normalized, err := NormalizeIssue(repoID, bulk.Issue)
	if err != nil {
		return fmt.Errorf("normalize issue #%d: %w", number, err)
	}

	// Preserve derived fields that NormalizeIssue doesn't populate
	// from bulk data. Without this, upsert overwrites them with
	// zero values.
	existing, err := s.db.GetIssueByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if err != nil {
		return fmt.Errorf(
			"get existing issue #%d: %w", number, err,
		)
	}
	if existing != nil {
		// Only preserve DetailFetchedAt when timeline data is complete.
		// When incomplete, clear it so the detail drain re-queues
		// this issue if the REST fallback fails.
		if bulk.CommentsComplete && bulk.TimelineComplete {
			normalized.DetailFetchedAt = existing.DetailFetchedAt
		}
		// CommentCount comes from GraphQL Comments.TotalCount via
		// adaptIssue, so trust the fresh GraphQL value.
	}

	issueID, revision, accepted, err := s.commitIssueParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf("upsert issue #%d: %w", number, err)
	}
	if !accepted {
		return nil
	}

	// UpsertIssue uses COALESCE to preserve existing detail_fetched_at,
	// so passing nil doesn't clear it. When comments are incomplete,
	// explicitly clear it so the detail drain re-queues this issue
	// if the REST fallback fails.
	if !bulk.CommentsComplete || !bulk.TimelineComplete {
		var detailCleared bool
		detailCleared, err = s.db.ClearIssueDetailFetchedSnapshot(ctx, issueID, revision)
		if err != nil {
			return fmt.Errorf(
				"clear detail_fetched_at for issue #%d: %w", number, err,
			)
		}
		if !detailCleared {
			return nil
		}
	}

	if bulk.CommentsComplete && bulk.TimelineComplete {
		derived := db.IssueDerivedFields{
			CommentCount:   normalized.CommentCount,
			LastActivityAt: computeIssueCommentLastActivity(bulk.Issue, bulk.Comments),
		}
		applied, err := s.replaceIssueCommentEvents(
			ctx, repo, number, issueID, revision, bulk.Comments,
			normalizeIssueTimelineEvents(issueID, bulk.TimelineEvents), &derived,
			bulk.CommentVisibility,
		)
		if err != nil {
			return fmt.Errorf(
				"replace issue comment events for #%d: %w", number, err,
			)
		}
		if !applied {
			return nil
		}
		// Mark detail as fetched so the detail drain doesn't
		// re-queue this issue for REST detail fetches.
		detailApplied, err := s.db.MarkIssueDetailFetchedSnapshot(ctx, issueID, revision)
		if err != nil {
			slog.Warn("mark GraphQL issue detail fetched failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", err,
			)
		}
		if !detailApplied {
			return nil
		}
	} else {
		// Timeline data truncated — fall back to detail fetch.
		if err := s.refreshIssueTimeline(
			ctx, repo, issueID, revision, bulk.Issue, bulk.CommentVisibility,
		); err != nil {
			if errors.Is(err, errParentSnapshotAdvanced) {
				return nil
			}
			return fmt.Errorf(
				"refresh timeline for issue #%d: %w", number, err,
			)
		}
		// REST fallback succeeded — mark detail as fetched.
		detailApplied, err := s.db.MarkIssueDetailFetchedSnapshot(ctx, issueID, revision)
		if err != nil {
			slog.Warn("mark issue detail fetched after REST fallback failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", err,
			)
		}
		if !detailApplied {
			return nil
		}
	}

	return nil
}

// syncOpenMRFromBulk processes a single PR from GraphQL bulk
// results. It performs the same operations as fetchMRDetail but
// using pre-fetched data instead of per-PR REST calls.
func (s *Syncer) syncOpenMRFromBulk(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	bulk *BulkPR,
	cloneFetchOK bool,
) error {
	number := bulk.PR.GetNumber()
	normalized, err := NormalizePR(repoID, bulk.PR)
	if err != nil {
		return fmt.Errorf("normalize MR #%d: %w", number, err)
	}

	// Preserve derived fields that NormalizePR doesn't populate.
	// Without this, upsert overwrites them with zero values; if
	// nested connections are truncated the later allComplete guard
	// skips restoring them and correct data is lost.
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if err != nil {
		return fmt.Errorf(
			"get existing MR #%d: %w", number, err,
		)
	}
	headChanged := existing != nil &&
		existing.PlatformHeadSHA != normalized.PlatformHeadSHA
	if existing != nil {
		normalized.CommentCount = existing.CommentCount
		normalized.ReviewDecision = existing.ReviewDecision
		// CI is tied to the head SHA. If the head moved we must clear
		// the previous values; otherwise an incomplete bulk CI fetch
		// (CIComplete=false skips the UpdateMRCIStatus write below)
		// would leave stale checks attached to the new commit.
		if !headChanged {
			normalized.CIStatus = existing.CIStatus
			normalized.CIChecksJSON = existing.CIChecksJSON
			normalized.CIHadPending = existing.CIHadPending
		}
		normalized.DetailFetchedAt = existing.DetailFetchedAt
		if normalized.AuthorDisplayName == "" {
			normalized.AuthorDisplayName =
				existing.AuthorDisplayName
		}
	}

	// Resolve the review decision independent of nested-connection
	// pagination. GitHub's reviewDecision scalar is provider-authoritative:
	// it is computed over the PR's entire review history, so a truncated
	// reviews/comments/commits/CI page on this fetch must not gate it. Apply
	// it to the parent snapshot whenever the provider supplies it. Only when
	// the provider reports no decision (null/empty enum) do we fall back to
	// deriving from this fetch's reviews, and only when that reviews
	// connection is complete; otherwise the previously persisted decision is
	// retained (review history is additive).
	if decision, authoritative := mapGraphQLReviewDecision(bulk.ReviewDecision); authoritative {
		normalized.ReviewDecision = decision
	} else if bulk.ReviewsComplete && len(bulk.Reviews) > 0 {
		normalized.ReviewDecision = DeriveReviewDecision(bulk.Reviews)
	}

	// Resolve display name if missing.
	if normalized.Author != "" &&
		normalized.AuthorDisplayName == "" {
		client, clientErr := s.clientFor(repo)
		if clientErr == nil {
			if name, ok := s.resolveDisplayName(
				ctx, client, repo, normalized.Author,
			); ok {
				normalized.AuthorDisplayName = name
			}
		}
	}

	mrID, revision, accepted, err := s.CommitMergeRequestParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf("upsert MR #%d: %w", number, err)
	}
	if !accepted {
		return nil
	}
	if !bulk.CommentsComplete || !bulk.ReviewsComplete ||
		!bulk.ReviewThreadsComplete || !bulk.CommitsComplete ||
		!bulk.TimelineComplete || !bulk.CIComplete {
		detailCleared, err := s.db.ClearMRDetailFetchedSnapshot(ctx, mrID, revision)
		if err != nil {
			return fmt.Errorf("clear detail fetch marker for MR #%d: %w", number, err)
		}
		if !detailCleared {
			return nil
		}
	}

	// UpsertMergeRequest preserves ci_had_pending across upserts, so
	// the head-changed reset above doesn't actually persist that field
	// without an explicit clear. Drop the stale CI state here so it
	// doesn't outlive the old commit.
	if headChanged {
		ciCleared, err := s.db.ClearMRCISnapshot(
			ctx, mrID, revision, normalized.PlatformHeadSHA,
		)
		if err != nil {
			return fmt.Errorf(
				"clear stale CI for MR #%d: %w", number, err,
			)
		}
		if !ciCleared {
			return nil
		}
	}

	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		return fmt.Errorf(
			"ensure kanban state for MR #%d: %w", number, err,
		)
	}

	// Diff SHAs.
	repoHost := repo.PlatformHost
	if repoHost == "" {
		repoHost = "github.com"
	}
	if s.clones != nil && cloneFetchOK {
		headSHA := normalized.PlatformHeadSHA
		baseSHA := normalized.PlatformBaseSHA
		if headSHA != "" && baseSHA != "" {
			mb, mbErr := s.clones.MergeBase(
				ctx, string(repoPlatform(repo)), repoHost, repo.Owner,
				repo.Name, baseSHA, headSHA,
			)
			if mbErr != nil {
				slog.Warn("merge-base computation failed",
					"repo", repo.Owner+"/"+repo.Name,
					"number", number, "err", mbErr,
				)
			} else {
				diffApplied, dbErr := s.db.UpdateDiffSHAsSnapshot(
					ctx, mrID, revision, headSHA, baseSHA,
					headSHA, baseSHA, mb,
				)
				if dbErr != nil {
					slog.Warn("update diff SHAs failed",
						"repo", repo.Owner+"/"+repo.Name,
						"number", number, "err", dbErr,
					)
				} else if !diffApplied {
					return nil
				}
			}
		}
	}

	// Commit complete archival families atomically under the page's epoch.
	comments := make([]db.MREvent, 0, len(bulk.Comments))
	reviews := make([]db.MREvent, 0, len(bulk.Reviews))
	var events []db.MREvent
	commitOrderer, err := s.commitOrderAssigner(ctx, mrID)
	if err != nil {
		return fmt.Errorf("load commit order for MR #%d: %w", number, err)
	}
	for _, c := range bulk.Comments {
		comments = append(comments, NormalizeCommentEventWithVisibility(
			mrID, c, bulk.CommentVisibility[c.GetID()],
		))
	}
	for _, r := range bulk.Reviews {
		reviews = append(reviews, NormalizeReviewEvent(mrID, r))
	}
	for i, c := range bulk.Commits {
		event := NormalizeCommitEvent(mrID, c)
		commitOrderer.apply(&event, i+1)
		events = append(events, event)
	}
	for _, timelineEvent := range bulk.TimelineEvents {
		if event := NormalizeTimelineEvent(mrID, timelineEvent); event != nil {
			events = append(events, *event)
		}
	}
	events, err = s.filterDuplicateMergedLifecycleEvents(ctx, mrID, events)
	if err != nil {
		return fmt.Errorf("dedupe merged lifecycle events for MR #%d: %w", number, err)
	}
	bulkAllComplete := bulk.CommentsComplete &&
		bulk.ReviewsComplete &&
		bulk.ReviewThreadsComplete &&
		bulk.CommitsComplete &&
		bulk.TimelineComplete &&
		bulk.CIComplete
	var derived *db.MRDerivedFields
	if bulk.CommentsComplete {
		fields := db.MRDerivedFields{
			ReviewDecision: normalized.ReviewDecision,
			CommentCount:   len(bulk.Comments),
		}
		derived = &fields
	}
	var inline []db.MREvent
	var reviewThreads []db.MRReviewThread
	if bulk.ReviewThreadsComplete {
		inline, reviewThreads = platform.DBReviewThreads(bulk.ReviewThreads)
	}
	if bulk.CommentsComplete || bulk.ReviewsComplete || bulk.ReviewThreadsComplete || len(events) > 0 {
		applied, err := s.commitMergeRequestDatasets(
			ctx, repo, mrID, number, revision,
			comments, bulk.CommentsComplete,
			reviews,
			inline, reviewThreads, bulk.ReviewThreadsComplete, events, derived,
			livenessHeadForRound(normalized, nil),
		)
		if err != nil {
			return fmt.Errorf("commit archival events for MR #%d: %w", number, err)
		}
		if !applied {
			return nil
		}
	}
	allComplete := bulkAllComplete
	if _, err := s.persistMergedTransitionEvent(ctx, mrID, revision, bulk.PR, normalized.MergedAt); err != nil {
		return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
	}

	// CI status — only write if complete (don't write
	// truncated CI data that could hide failures).
	var ciChecks []db.CICheck
	var ciJSON []byte
	if bulk.CIComplete {
		ciChecks = normalizeBulkCI(bulk)
		if ciChecks == nil {
			ciChecks = []db.CICheck{}
		}
		ciJSON, _ = json.Marshal(ciChecks)
		ciStatus := deriveCIStatusFromChecks(ciChecks)
		ciApplied, err := s.db.UpdateMergeRequestCISnapshot(
			ctx, mrID, revision, ciStatus, string(ciJSON),
		)
		if err != nil {
			slog.Warn("update CI status failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", err,
			)
		}
		if !ciApplied {
			return nil
		}
	}

	// Mark detail as fetched only when
	// ALL connections are complete. Incomplete PRs leave
	// DetailFetchedAt stale so the detail drain picks it up for a
	// full REST fetch. Derived fields are committed atomically with
	// their complete mirrored datasets above.
	if allComplete {
		pending := ciHasPending(string(ciJSON))
		detailApplied, err := s.db.MarkMergeRequestDetailFetchedSnapshot(ctx, mrID, revision, pending, nil)
		if err != nil {
			slog.Warn("mark GraphQL detail fetched failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", err,
			)
		}
		if !detailApplied {
			return nil
		}
		// Refresh workflow approval state so the DB-only detail GET
		// can render the Approve workflows button without a foreground
		// sync. GraphQL doesn't return action_required runs, so this
		// stays a one-extra REST call per fully-synced PR, gated by
		// the same per-host budget as the REST detail drain. The
		// sync-budget transport spends the actual REST call; this is
		// only the admission check.
		if s.canSpendWorkflowApprovalRefresh(repo) {
			approvalApplied, _ := s.refreshWorkflowApproval(
				ctx, repo, repoID, number,
				normalized.PlatformHeadSHA, bulk.PR, normalized,
			)
			if !approvalApplied {
				return nil
			}
		}
	}

	// Fire onMRSynced hook.
	if s.onMRSynced != nil {
		fresh, fErr := s.db.GetMergeRequestByRepoIDAndNumber(
			ctx, repoID, number,
		)
		if fErr != nil {
			slog.Warn("get MR for onMRSynced hook failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", fErr,
			)
		} else {
			s.onMRSynced(repo.Owner, repo.Name, fresh)
		}
	}

	return nil
}

// deriveCIStatusFromChecks computes the overall CI status from
// a []db.CICheck. Mirrors DeriveOverallCIStatus but works on the
// normalized CICheck format produced by normalizeBulkCI.
func deriveCIStatusFromChecks(checks []db.CICheck) string {
	if len(checks) == 0 {
		return ""
	}
	hasPending := false
	hasFailed := false
	for _, c := range checks {
		if c.Status != "completed" {
			hasPending = true
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
			// OK
		default:
			if c.Conclusion != "" {
				hasFailed = true
			}
		}
	}
	if hasFailed {
		return "failure"
	}
	if hasPending {
		return "pending"
	}
	return "success"
}

// normalizeBulkCI converts GraphQL check runs and statuses to
// the db.CICheck slice format used by the rest of the codebase.
func normalizeBulkCI(bulk *BulkPR) []db.CICheck {
	return normalizeCIChecks(bulk.CheckRuns, bulk.Statuses)
}

// fetchMRDetail performs a full detail fetch for a single MR:
// GetPullRequest, refreshTimeline, refreshCIStatus. Returns the
// number of API calls made.
func (s *Syncer) fetchMRDetail(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	cloneFetchOK bool,
) (int, error) {
	removed, err := s.removedUpstreamForLiveSync(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return 0, fmt.Errorf("check PR #%d visibility: %w", number, err)
	}
	if removed {
		return 0, nil
	}
	calls, err := s.fetchMRDetailWithRouteFence(
		ctx, repo, repoID, number, cloneFetchOK,
	)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return calls, nil
	}
	return calls, err
}

func (s *Syncer) fetchMRDetailWithRouteFence(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	cloneFetchOK bool,
) (int, error) {
	ctx = withCloneRepositoryIdentity(ctx, repo)
	calls := 0
	mrReader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve merge request reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	routeFence, _, err := s.db.CurrentRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), repoID,
	)
	if err != nil {
		return calls, fmt.Errorf("capture repository route for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if _, ok := mrReader.(interface {
		GetGitHubPullRequest(context.Context, platform.RepoRef, int) (*gh.PullRequest, platform.MergeRequest, error)
	}); !ok {
		return s.fetchProviderMRDetail(
			ctx, mrReader, repo, repoID, number, routeFence,
		)
	}

	client, err := s.clientFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"get existing MR #%d: %w", number, err,
		)
	}

	fullPR, newETag, notModified, err := s.getPullRequestForDetail(
		ctx, client, repo, number,
	)
	calls++
	// Route fetch failures and detected transfers through the canonical
	// lookup classification so removed, inaccessible, and moved items
	// surface typed outcomes instead of generic upstream failures.
	if provider, ok := mrReader.(*gitHubClientProvider); ok {
		if outcomeErr := provider.mergeRequestLookupOutcomeError(
			ctx, platformRepoRef(repo), number, fullPR, err,
		); outcomeErr != nil {
			return calls, fmt.Errorf("get full PR #%d: %w", number, outcomeErr)
		}
	}
	if err == nil && fullPR == nil {
		if notModified && existing != nil {
			return s.markUnchangedMRDetailFetched(
				ctx, repo, repoID, number, existing, routeFence, calls,
			)
		}
		err = fmt.Errorf("client returned nil pull request")
	}
	if err != nil {
		return calls, fmt.Errorf(
			"get full PR #%d: %w", number, err,
		)
	}
	normalized, err := NormalizePR(repoID, fullPR)
	if err != nil {
		return calls, fmt.Errorf("normalize full PR #%d: %w", number, err)
	}
	preserveMergeableStateIfOmitted(normalized, existing)

	if normalized.Author != "" &&
		normalized.AuthorDisplayName == "" {
		if name, ok := s.resolveDisplayName(
			ctx, client, repo, normalized.Author,
		); ok {
			normalized.AuthorDisplayName = name
		}
		calls++ // GetUser
	}

	mrID, revision, accepted, err := s.commitMergeRequestParentSnapshotIfRouteFence(
		ctx, repo, normalized, routeFence,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"upsert MR #%d: %w", number, err,
		)
	}
	if !accepted {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)

	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return calls, nil
		}
		return calls, fmt.Errorf(
			"ensure kanban state for MR #%d: %w", number, err,
		)
	}

	// Diff SHAs if clone available.
	cloneRepoHost := repo.PlatformHost
	if cloneRepoHost == "" {
		cloneRepoHost = "github.com"
	}
	if s.clones != nil && cloneFetchOK {
		headSHA := normalized.PlatformHeadSHA
		baseSHA := normalized.PlatformBaseSHA
		if headSHA != "" && baseSHA != "" {
			mb, mbErr := s.clones.MergeBase(
				ctx, string(repoPlatform(repo)), cloneRepoHost, repo.Owner,
				repo.Name, baseSHA, headSHA,
			)
			if mbErr != nil {
				slog.Warn("merge-base computation failed",
					"repo", repo.Owner+"/"+repo.Name,
					"number", number, "err", mbErr,
				)
			} else {
				diffApplied, dbErr := s.db.UpdateDiffSHAsSnapshot(
					ctx, mrID, revision, headSHA, baseSHA,
					headSHA, baseSHA, mb,
				)
				if dbErr != nil {
					slog.Warn("update diff SHAs failed",
						"repo", repo.Owner+"/"+repo.Name,
						"number", number, "err", dbErr,
					)
				} else if !diffApplied {
					return calls, nil
				}
			}
		}
	}

	if err := s.refreshTimeline(
		ctx, repo, mrID, revision, fullPR,
		livenessHeadForRound(normalized, existing),
	); err != nil {
		// Timeline = 4 base calls (comments + reviews + commits + force-push);
		// provider review-thread sync is handled inside refreshTimeline.
		calls += 4
		if errors.Is(err, errParentSnapshotAdvanced) {
			return calls, nil
		}
		return calls, err
	}
	calls += 4
	if _, err := s.persistMergedTransitionEvent(ctx, mrID, revision, fullPR, normalized.MergedAt); err != nil {
		return calls, fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
	}

	ciHeadSHA := ""
	if fullPR.GetHead() != nil {
		ciHeadSHA = fullPR.GetHead().GetSHA()
	}
	ciApplied, err := s.refreshCIStatusSnapshot(
		ctx, repo, mrID, revision, number, ciHeadSHA,
	)
	if err != nil {
		// CI = 2 calls (combined status + check runs).
		calls += 2
		return calls, err
	}
	calls += 2
	if !ciApplied {
		return calls, nil
	}

	// Refresh workflow approval state so the DB-only detail GET
	// can render the Approve workflows button without a foreground
	// sync. Same path as syncMRForRepo, but the budgeted detail
	// drain needs to count this call too.
	approvalApplied, approvalCalls := s.refreshWorkflowApproval(
		ctx, repo, repoID, number, ciHeadSHA, fullPR, normalized,
	)
	calls += approvalCalls
	if !approvalApplied {
		return calls, nil
	}

	// Determine whether CI had pending checks for scoring by
	// reading the DB row that refreshCIStatus just wrote. Use
	// ciHasPending (checks individual statuses) rather than the
	// aggregate CIStatus, which becomes "failure" when any check
	// fails even if others are still running.
	pending := false
	freshMR, freshErr := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if freshErr == nil && freshMR != nil {
		pending = ciHasPending(freshMR.CIChecksJSON)
	}

	detailApplied, err := s.markMergeRequestDetailFetchedIfRouteFence(
		ctx, repo, routeFence, mrID, revision, pending, nil,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"mark detail fetched for MR #%d: %w", number, err,
		)
	}
	if !detailApplied {
		return calls, nil
	}

	// Fire onMRSynced hook.
	if s.onMRSynced != nil {
		fresh, fErr := s.db.GetMergeRequestByRepoIDAndNumber(
			ctx, repoID, number,
		)
		if fErr != nil {
			slog.Warn("get MR for onMRSynced hook failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", fErr,
			)
		} else {
			s.onMRSynced(repo.Owner, repo.Name, fresh)
		}
	}

	if newETag != "" {
		if _, err := s.db.UpsertHTTPEtagIfRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
			"pull_request", number, newETag,
		); err != nil {
			slog.Warn("persist pull request ETag failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
		}
	}

	return calls, nil
}

func (s *Syncer) getPullRequestForDetail(
	ctx context.Context,
	client Client,
	repo RepoRef,
	number int,
) (*gh.PullRequest, string, bool, error) {
	conditional, ok := client.(conditionalPullRequestGetter)
	if !ok {
		pr, err := client.GetPullRequest(ctx, repo.Owner, repo.Name, number)
		return pr, "", false, err
	}

	etag, err := s.db.GetHTTPEtag(
		ctx, string(repoPlatform(repo)), repoHost(repo),
		repo.Owner, repo.Name, "pull_request", number,
	)
	if err != nil {
		slog.Warn("load pull request ETag failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		pr, err := client.GetPullRequest(ctx, repo.Owner, repo.Name, number)
		return pr, "", false, err
	}
	return conditional.GetPullRequestIfChanged(
		ctx, repo.Owner, repo.Name, number, etag,
	)
}

func (s *Syncer) markUnchangedMRDetailFetched(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	existing *db.MergeRequest,
	routeFence db.RepositoryRouteFence,
	calls int,
) (int, error) {
	matches, err := s.repositoryRouteFenceMatches(ctx, repo, routeFence)
	if err != nil {
		return calls, err
	}
	if !matches {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	if err := s.refreshStoredPRCommentVisibility(
		ctx, repo, existing.ID, existing.SnapshotRevision, number,
	); err != nil {
		if errors.Is(err, errParentSnapshotAdvanced) ||
			errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return calls, nil
		}
		return calls, err
	}
	if fetcher := s.fetcherForContext(ctx, repo); fetcher != nil && s.graphQLReadAllowed(ctx, repo, fetcher) {
		threadCalls, err := s.syncProviderMRReviewThreads(
			ctx, repo, existing.ID, number, existing.SnapshotRevision, true,
		)
		calls += threadCalls
		if err != nil {
			if errors.Is(err, errParentSnapshotAdvanced) {
				return calls, nil
			}
			return calls, err
		}
	}
	pending := existing.CIHadPending
	if existing.CIHadPending && existing.PlatformHeadSHA != "" {
		ciApplied, err := s.refreshCIStatusSnapshot(
			ctx, repo, existing.ID, existing.SnapshotRevision,
			number, existing.PlatformHeadSHA,
		)
		if err != nil {
			calls += 2
			return calls, err
		}
		calls += 2
		if !ciApplied {
			return calls, nil
		}
		fresh, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		if err == nil && fresh != nil {
			pending = ciHasPending(fresh.CIChecksJSON)
		}
	}
	// Unchanged rounds have no dataset commit of their own, so commit
	// liveness travels with the detail-fetched marker under the same
	// revision guard instead. The round is unchanged, so no state
	// transition is possible here.
	metadataUpdates := s.computeCommitLiveness(
		ctx, repo, existing.ID, livenessHeadForRound(existing, existing), nil,
	)
	detailApplied, err := s.markMergeRequestDetailFetchedIfRouteFence(
		ctx, repo, routeFence, existing.ID, existing.SnapshotRevision, pending,
		metadataUpdates,
	)
	if err != nil {
		return calls, fmt.Errorf("mark unchanged detail fetched for MR #%d: %w", number, err)
	}
	if !detailApplied {
		return calls, nil
	}
	if s.onMRSynced != nil {
		fresh, fErr := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		if fErr != nil {
			slog.Warn("get MR for onMRSynced hook failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", fErr,
			)
		} else {
			s.onMRSynced(repo.Owner, repo.Name, fresh)
		}
	}
	return calls, nil
}

func (s *Syncer) fetchProviderMRDetail(
	ctx context.Context,
	reader platform.MergeRequestReader,
	repo RepoRef,
	repoID int64,
	number int,
	routeFence db.RepositoryRouteFence,
) (int, error) {
	calls := 0
	mrReader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve merge request reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	mr, err := mrReader.GetMergeRequest(ctx, platformRepoRef(repo), number)
	calls++
	if err != nil {
		return calls, fmt.Errorf(
			"get full MR #%d: %w", number,
			s.classifyProviderItemLookupError(ctx, repo, err),
		)
	}

	normalized := platform.DBMergeRequest(repoID, mr)
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"get existing MR #%d: %w", number, err,
		)
	}
	preserveMergeableStateIfOmitted(normalized, existing)

	mrID, revision, accepted, err := s.commitMergeRequestParentSnapshotIfRouteFence(
		ctx, repo, normalized, routeFence,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"upsert MR #%d: %w", number, err,
		)
	}
	if !accepted {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return calls, nil
		}
		return calls, fmt.Errorf(
			"ensure kanban state for MR #%d: %w", number, err,
		)
	}

	detailCalls, pending, err := s.syncProviderMRDetailExtras(
		ctx, reader, repo, mrID, number, revision, normalized.PlatformHeadSHA,
		livenessHeadForRound(normalized, existing),
	)
	calls += detailCalls
	if err != nil {
		if errors.Is(err, errParentSnapshotAdvanced) {
			return calls, nil
		}
		return calls, err
	}
	if _, err := s.persistMergedActorEvent(ctx, mrID, revision, mr.MergedBy, normalized.MergedAt); err != nil {
		return calls, fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
	}

	detailApplied, err := s.markMergeRequestDetailFetchedIfRouteFence(
		ctx, repo, routeFence, mrID, revision, pending, nil,
	)
	if err != nil {
		return calls, fmt.Errorf("mark detail fetched for MR #%d: %w", number, err)
	}
	if !detailApplied {
		return calls, nil
	}

	if s.onMRSynced != nil {
		fresh, fErr := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		if fErr != nil {
			slog.Warn("get MR for onMRSynced hook failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number, "err", fErr,
			)
		} else {
			s.onMRSynced(repo.Owner, repo.Name, fresh)
		}
	}

	return calls, nil
}

func (s *Syncer) syncProviderMRDetailExtras(
	ctx context.Context,
	reader platform.MergeRequestReader,
	repo RepoRef,
	mrID int64,
	number int,
	expectedRevision int64,
	headSHA string,
	livenessHeadSHA string,
) (int, bool, error) {
	calls := 0
	events, err := reader.ListMergeRequestEvents(ctx, platformRepoRef(repo), number)
	calls++
	if err != nil && !errors.Is(err, platform.ErrUnsupportedCapability) {
		return calls, false, fmt.Errorf("list MR events for #%d: %w", number, err)
	}
	if err == nil {
		dbEvents := make([]db.MREvent, 0, len(events))
		comments := make([]db.MREvent, 0, len(events))
		reviews := make([]db.MREvent, 0, len(events))
		commitOrderer, orderErr := s.commitOrderAssigner(ctx, mrID)
		if orderErr != nil {
			return calls, false, fmt.Errorf("load commit order for MR #%d: %w", number, orderErr)
		}
		commitListOrder := 0
		for _, event := range events {
			dbEvent := platform.DBMREvent(mrID, event)
			if dbEvent.EventType == "commit" {
				commitListOrder++
				commitOrderer.apply(&dbEvent, commitListOrder)
			}
			switch dbEvent.EventType {
			case "issue_comment":
				comments = append(comments, dbEvent)
			case "review":
				reviews = append(reviews, dbEvent)
			default:
				dbEvents = append(dbEvents, dbEvent)
			}
		}
		dbEvents, err = s.filterDuplicateMergedLifecycleEvents(ctx, mrID, dbEvents)
		if err != nil {
			return calls, false, fmt.Errorf("dedupe merged lifecycle events for MR #%d: %w", number, err)
		}
		applied, commitErr := s.commitMergeRequestDatasets(
			ctx, repo, mrID, number, expectedRevision,
			comments, true,
			reviews,
			nil, nil, false, dbEvents, nil,
			livenessHeadSHA,
		)
		if commitErr != nil {
			return calls, false, fmt.Errorf("replace provider MR events for #%d: %w", number, commitErr)
		}
		if !applied {
			return calls, false, errParentSnapshotAdvanced
		}
	}

	reviewThreadCalls, err := s.syncProviderMRReviewThreads(ctx, repo, mrID, number, expectedRevision, false)
	calls += reviewThreadCalls
	if err != nil {
		return calls, false, fmt.Errorf("sync review threads for MR #%d: %w", number, err)
	}

	pending := false
	if headSHA == "" {
		return calls, pending, nil
	}
	ciReader, err := s.ciReaderFor(repo)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) {
			return calls, pending, nil
		}
		return calls, false, fmt.Errorf("resolve CI reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	checks, err := ciReader.ListCIChecks(ctx, platformRepoRef(repo), headSHA)
	calls++
	if err != nil && !errors.Is(err, platform.ErrUnsupportedCapability) {
		return calls, false, fmt.Errorf("list CI checks for MR #%d: %w", number, err)
	}
	if err != nil {
		return calls, pending, nil
	}
	dbChecks := platform.DBCIChecks(checks)
	if dbChecks == nil {
		dbChecks = []db.CICheck{}
	}
	ciJSON, _ := json.Marshal(dbChecks)
	ciStatus := deriveCIStatusFromChecks(dbChecks)
	ciApplied, err := s.db.UpdateMergeRequestCISnapshot(
		ctx, mrID, expectedRevision, ciStatus, string(ciJSON),
	)
	if err != nil {
		return calls, false, fmt.Errorf("update CI status for MR #%d: %w", number, err)
	}
	if !ciApplied {
		return calls, false, errParentSnapshotAdvanced
	}
	pending = ciHasPending(string(ciJSON))
	return calls, pending, nil
}

func (s *Syncer) syncProviderMRReviewThreads(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	number int,
	expectedRevision int64,
	preserveOnReadFailure bool,
) (int, error) {
	caps, err := s.clients.Capabilities(repoPlatform(repo), repoHost(repo))
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) {
			return 0, nil
		}
		return 0, err
	}
	if !caps.ReadReviewThreads {
		return 0, nil
	}
	reader, err := s.clients.MergeRequestReviewThreadReader(repoPlatform(repo), repoHost(repo))
	if err != nil {
		return 0, err
	}
	threads, err := reader.ListMergeRequestReviewThreads(ctx, platformRepoRef(repo), number)
	calls := 1
	if err != nil {
		if preserveOnReadFailure {
			slog.Warn("current PR review-thread visibility fetch failed; preserving stored state",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			return calls, nil
		}
		return calls, err
	}

	events, dbThreads := platform.DBReviewThreads(threads)
	applied, err := s.commitMergeRequestDatasets(
		ctx, repo, mrID, number, expectedRevision,
		nil, false, nil, events, dbThreads, true, nil, nil, "",
	)
	if err != nil {
		return calls, err
	}
	if !applied {
		return calls, errParentSnapshotAdvanced
	}
	return calls, nil
}

// fetchIssueDetail performs a full detail fetch for a single
// issue: GetIssue + refreshIssueTimeline. Returns the number
// of API calls made.
func (s *Syncer) fetchIssueDetail(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
) (int, error) {
	removed, err := s.removedUpstreamForLiveSync(
		ctx, repoID, db.ArchiveItemTypeIssue, number,
	)
	if err != nil {
		return 0, fmt.Errorf("check issue #%d visibility: %w", number, err)
	}
	if removed {
		return 0, nil
	}
	calls := 0
	issueReader, err := s.issueReaderFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve issue reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	routeFence, _, err := s.db.CurrentRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), repoID,
	)
	if err != nil {
		return calls, fmt.Errorf("capture repository route for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if _, ok := issueReader.(interface {
		GetGitHubIssue(context.Context, platform.RepoRef, int) (*gh.Issue, error)
	}); !ok {
		return s.fetchProviderIssueDetail(
			ctx, issueReader, repo, repoID, number, routeFence,
		)
	}

	client, err := s.clientFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	existing, err := s.db.GetIssueByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return calls, fmt.Errorf("get existing issue #%d: %w", number, err)
	}

	ghIssue, newETag, notModified, err := s.getIssueForDetail(
		ctx, client, repo, number,
	)
	calls++
	// Route fetch failures and detected transfers through the canonical
	// lookup classification so removed, inaccessible, and moved items
	// surface typed outcomes instead of generic upstream failures.
	if provider, ok := issueReader.(*gitHubClientProvider); ok {
		if outcomeErr := provider.issueLookupOutcomeError(
			ctx, platformRepoRef(repo), number, ghIssue, err,
		); outcomeErr != nil {
			return calls, fmt.Errorf("get issue #%d: %w", number, outcomeErr)
		}
		if outcomeErr := provider.issuePullRequestOutcomeError(
			platformRepoRef(repo), number, ghIssue,
		); outcomeErr != nil {
			return calls, fmt.Errorf("get issue #%d: %w", number, outcomeErr)
		}
	}
	if err == nil && ghIssue == nil {
		if notModified {
			if existing == nil {
				return calls, fmt.Errorf("mark unchanged detail fetched for issue #%d: issue is missing", number)
			}
			return s.markUnchangedIssueDetailFetched(
				ctx, repo, number, existing, routeFence, calls,
			)
		}
		err = fmt.Errorf("client returned nil issue")
	}
	if err != nil {
		return calls, fmt.Errorf(
			"get issue #%d: %w", number, err,
		)
	}
	normalized, err := NormalizeIssue(repoID, ghIssue)
	if err != nil {
		return calls, fmt.Errorf("normalize issue #%d: %w", number, err)
	}
	issueID, revision, accepted, err := s.commitIssueParentSnapshotIfRouteFence(
		ctx, repo, normalized, routeFence,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"upsert issue #%d: %w", number, err,
		)
	}
	if !accepted {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)

	if err := s.refreshIssueTimeline(
		ctx, repo, issueID, revision, ghIssue, nil,
	); err != nil {
		calls++ // comments
		if errors.Is(err, errParentSnapshotAdvanced) {
			return calls, nil
		}
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return calls, nil
		}
		return calls, err
	}
	calls++ // comments

	detailApplied, err := s.markIssueDetailFetchedIfRouteFence(
		ctx, repo, routeFence, issueID, revision,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"mark detail fetched for issue #%d: %w", number, err,
		)
	}
	if !detailApplied {
		return calls, nil
	}

	if newETag != "" {
		if _, err := s.db.UpsertHTTPEtagIfRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
			"issue", number, newETag,
		); err != nil {
			slog.Warn("persist issue ETag failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
		}
	}

	return calls, nil
}

func (s *Syncer) markUnchangedIssueDetailFetched(
	ctx context.Context,
	repo RepoRef,
	number int,
	existing *db.Issue,
	routeFence db.RepositoryRouteFence,
	calls int,
) (int, error) {
	matches, err := s.repositoryRouteFenceMatches(ctx, repo, routeFence)
	if err != nil {
		return calls, err
	}
	if !matches {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	if err := s.refreshStoredIssueCommentVisibility(
		ctx, repo, existing.ID, existing.SnapshotRevision, number,
	); err != nil {
		if errors.Is(err, errParentSnapshotAdvanced) ||
			errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return calls, nil
		}
		return calls, err
	}
	detailApplied, err := s.markIssueDetailFetchedIfRouteFence(
		ctx, repo, routeFence, existing.ID, existing.SnapshotRevision,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"mark unchanged detail fetched for issue #%d: %w", number, err,
		)
	}
	if !detailApplied {
		return calls, nil
	}
	return calls, nil
}

func (s *Syncer) getIssueForDetail(
	ctx context.Context,
	client Client,
	repo RepoRef,
	number int,
) (*gh.Issue, string, bool, error) {
	if IsArchiveSyncBudgetContext(ctx) {
		issue, err := client.GetIssue(ctx, repo.Owner, repo.Name, number)
		return issue, "", false, err
	}
	conditional, ok := client.(conditionalIssueGetter)
	if !ok {
		issue, err := client.GetIssue(ctx, repo.Owner, repo.Name, number)
		return issue, "", false, err
	}

	etag, err := s.db.GetHTTPEtag(
		ctx, string(repoPlatform(repo)), repoHost(repo),
		repo.Owner, repo.Name, "issue", number,
	)
	if err != nil {
		slog.Warn("load issue ETag failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		issue, err := client.GetIssue(ctx, repo.Owner, repo.Name, number)
		return issue, "", false, err
	}
	return conditional.GetIssueIfChanged(
		ctx, repo.Owner, repo.Name, number, etag,
	)
}

func (s *Syncer) fetchProviderIssueDetail(
	ctx context.Context,
	reader platform.IssueReader,
	repo RepoRef,
	repoID int64,
	number int,
	routeFence db.RepositoryRouteFence,
) (int, error) {
	calls := 0
	issueReader, err := s.issueReaderFor(repo)
	if err != nil {
		return calls, fmt.Errorf("resolve issue reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	issue, err := issueReader.GetIssue(ctx, platformRepoRef(repo), number)
	calls++
	if err != nil {
		return calls, fmt.Errorf(
			"get issue #%d: %w", number,
			s.classifyProviderItemLookupError(ctx, repo, err),
		)
	}

	normalized := platform.DBIssue(repoID, issue)
	existing, err := s.db.GetIssueByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return calls, fmt.Errorf("get existing issue #%d: %w", number, err)
	}
	if existing != nil {
		normalized.CommentCount = existing.CommentCount
	}
	issueID, revision, accepted, err := s.commitIssueParentSnapshotIfRouteFence(
		ctx, repo, normalized, routeFence,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"upsert issue #%d: %w", number, err,
		)
	}
	if !accepted {
		return calls, nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	events, eventsErr := reader.ListIssueEvents(ctx, platformRepoRef(repo), number)
	calls++
	if eventsErr != nil && !errors.Is(eventsErr, platform.ErrUnsupportedCapability) {
		return calls, fmt.Errorf("list issue events for #%d: %w", number, eventsErr)
	}
	if eventsErr == nil {
		comments := make([]db.IssueEvent, 0, len(events))
		dbEvents := make([]db.IssueEvent, 0, len(events))
		for _, event := range events {
			dbEvent := platform.DBIssueEvent(issueID, event)
			if dbEvent.EventType == "issue_comment" {
				comments = append(comments, dbEvent)
			} else {
				dbEvents = append(dbEvents, dbEvent)
			}
		}
		applied, commitErr := s.commitIssueCommentsSnapshot(ctx, repo, issueID, number, revision, comments, dbEvents, nil)
		if commitErr != nil {
			if errors.Is(commitErr, db.ErrRepositoryRouteFenceChanged) {
				return calls, nil
			}
			return calls, fmt.Errorf("replace provider issue comments for #%d: %w", number, commitErr)
		}
		if !applied {
			return calls, nil
		}
	}

	detailApplied, err := s.markIssueDetailFetchedIfRouteFence(
		ctx, repo, routeFence, issueID, revision,
	)
	if err != nil {
		return calls, fmt.Errorf(
			"mark detail fetched for issue #%d: %w", number, err,
		)
	}
	if !detailApplied {
		return calls, nil
	}

	return calls, nil
}

func (s *Syncer) classifyProviderItemLookupError(
	ctx context.Context,
	repo RepoRef,
	err error,
) error {
	if errors.Is(err, platform.ErrLookupNotPresent) {
		return err
	}
	if !errors.Is(err, platform.ErrNotFound) {
		return err
	}
	repositoryReader, readerErr := s.clients.RepositoryReader(repoPlatform(repo), repoHost(repo))
	if readerErr != nil {
		return err
	}
	if _, probeErr := repositoryReader.GetRepository(ctx, platformRepoRef(repo)); probeErr != nil {
		return errors.Join(err, fmt.Errorf("probe repository after item not found: %w", probeErr))
	}
	return errors.Join(platform.ErrLookupNotPresent, err)
}

// refreshTimeline fetches comments, reviews, and commits for a PR and
// updates its derived fields (ReviewDecision, CommentCount, LastActivityAt, CIStatus).
func (s *Syncer) refreshTimeline(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	expectedRevision int64,
	ghPR *gh.PullRequest,
	livenessHeadSHA string,
) error {
	if ghPR == nil {
		return fmt.Errorf("nil pull request")
	}
	number := ghPR.GetNumber()
	client, err := s.clientFor(repo)
	if err != nil {
		return fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}

	comments, err := client.ListIssueComments(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return fmt.Errorf("list comments for MR #%d: %w", number, err)
	}

	reviews, err := client.ListReviews(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return fmt.Errorf("list reviews for MR #%d: %w", number, err)
	}

	commits, err := client.ListCommits(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return fmt.Errorf("list commits for MR #%d: %w", number, err)
	}

	timelineEvents, err := client.ListPullRequestTimelineEvents(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		if disabledErr := repositoryFeatureDisabledError(
			repo, platform.RepositoryFeatureMergeRequests, err,
		); disabledErr != nil {
			return disabledErr
		}
		slog.Warn("timeline event fetch failed during timeline refresh",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		timelineEvents = nil
	}

	commentEvents := make([]db.MREvent, 0, len(comments))
	reviewEvents := make([]db.MREvent, 0, len(reviews))
	var events []db.MREvent
	commitOrderer, err := s.commitOrderAssigner(ctx, mrID)
	if err != nil {
		return fmt.Errorf("load commit order for MR #%d: %w", number, err)
	}
	commentVisibility, err := s.storedPRCommentVisibility(ctx, mrID)
	if err != nil {
		return fmt.Errorf("load stored comment visibility for MR #%d: %w", number, err)
	}
	if observed, ok := s.currentPRCommentVisibility(ctx, repo, number); ok {
		commentVisibility = observed
	}
	for _, c := range comments {
		commentEvents = append(commentEvents, NormalizeCommentEventWithVisibility(
			mrID, c, commentVisibility[c.GetID()],
		))
	}
	for _, r := range reviews {
		reviewEvents = append(reviewEvents, NormalizeReviewEvent(mrID, r))
	}
	for i, c := range commits {
		event := NormalizeCommitEvent(mrID, c)
		commitOrderer.apply(&event, i+1)
		events = append(events, event)
	}
	for _, timelineEvent := range timelineEvents {
		event := NormalizeTimelineEvent(mrID, timelineEvent)
		if event == nil {
			continue
		}
		events = append(events, *event)
	}
	events, err = s.filterDuplicateMergedLifecycleEvents(ctx, mrID, events)
	if err != nil {
		return fmt.Errorf("dedupe merged lifecycle events for MR #%d: %w", number, err)
	}

	reviewDecision := DeriveReviewDecision(reviews)
	derived := db.MRDerivedFields{
		ReviewDecision: reviewDecision,
		CommentCount:   len(comments),
	}
	applied, err := s.commitMergeRequestDatasets(
		ctx, repo, mrID, number, expectedRevision,
		commentEvents, true,
		reviewEvents,
		nil, nil, false, events, &derived,
		livenessHeadSHA,
	)
	if err != nil {
		return fmt.Errorf("commit comments and reviews for MR #%d: %w", number, err)
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	if _, err := s.syncProviderMRReviewThreads(ctx, repo, mrID, number, expectedRevision, false); err != nil {
		return fmt.Errorf("sync review threads for MR #%d: %w", number, err)
	}
	return nil
}

// RefreshMRCIStatusOnProvider fetches only CI checks for a PR's head SHA and
// persists the derived CI fields. It intentionally skips the heavier PR detail
// sync path (timeline, diff, review, and body refreshes).
func (s *Syncer) RefreshMRCIStatusOnProvider(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	headSHA string,
) ([]string, error) {
	if headSHA == "" {
		return nil, nil
	}
	if repoPlatform(repo) == platform.KindGitHub {
		result, err := s.fetchGitHubCIStatus(ctx, repo, number, headSHA)
		if err != nil {
			return nil, err
		}
		if result.Warning != "" {
			return []string{result.Warning}, nil
		}
		if !result.Updated {
			return nil, nil
		}
		return nil, s.db.UpdateMRCIStatusForHead(
			ctx, repoID, number, headSHA,
			result.Status, result.ChecksJSON, ciHasPending(result.ChecksJSON),
		)
	}

	ciReader, err := s.ciReaderFor(repo)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve CI reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	checks, err := ciReader.ListCIChecks(ctx, platformRepoRef(repo), headSHA)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) {
			return nil, nil
		}
		slog.Warn("list CI checks failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return []string{ciRefreshWarning}, nil
	}
	dbChecks := platform.DBCIChecks(checks)
	if dbChecks == nil {
		dbChecks = []db.CICheck{}
	}
	ciJSON, _ := json.Marshal(dbChecks)
	ciStatus := deriveCIStatusFromChecks(dbChecks)
	if err := s.db.UpdateMRCIStatusForHead(
		ctx, repoID, number, headSHA,
		ciStatus, string(ciJSON), ciHasPending(string(ciJSON)),
	); err != nil {
		return nil, fmt.Errorf("update CI status for MR #%d: %w", number, err)
	}
	return nil, nil
}

// RefreshMRCIStatusForRepository refreshes CI only while repo still owns its
// captured route. Delayed background work uses this entry point so route reuse
// cannot write a replacement repository's provider response under repoID.
func (s *Syncer) RefreshMRCIStatusForRepository(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	headSHA string,
) ([]string, error) {
	identity := platform.DBRepoIdentity(platformRepoRef(repo))
	routeFence, found, err := s.db.CurrentRepositoryRouteFence(ctx, identity, repoID)
	if err != nil {
		return nil, fmt.Errorf("capture repository route for CI refresh %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if !found {
		return nil, nil
	}
	ctx = s.db.WithRepositoryRouteFence(ctx, identity, routeFence)
	warnings, err := s.RefreshMRCIStatusOnProvider(ctx, repo, repoID, number, headSHA)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return nil, nil
	}
	return warnings, err
}

// refreshCIStatus fetches combined status and check runs for a PR's head SHA.
// Called on every sync cycle for open PRs, since check runs change independently
// of the PR's updated_at field. Takes headSHA and number directly so it can be
// invoked from the 304 code path, where the caller holds DB rows rather than
// a *gh.PullRequest.
func (s *Syncer) refreshCIStatus(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	headSHA string,
) error {
	result, err := s.fetchGitHubCIStatus(ctx, repo, number, headSHA)
	if err != nil {
		return err
	}
	if !result.Updated {
		return nil
	}
	return s.db.UpdateMRCIStatusForHead(
		ctx, repoID, number, headSHA,
		result.Status, result.ChecksJSON, ciHasPending(result.ChecksJSON),
	)
}

// refreshCIStatusSnapshot is the detail-sync variant of refreshCIStatus. It
// rejects the write when another parent snapshot won while CI was in flight.
func (s *Syncer) refreshCIStatusSnapshot(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	expectedRevision int64,
	number int,
	headSHA string,
) (bool, error) {
	result, err := s.fetchGitHubCIStatus(ctx, repo, number, headSHA)
	if err != nil {
		return false, err
	}
	if !result.Updated {
		return true, nil
	}
	applied, err := s.db.UpdateMergeRequestCISnapshot(
		ctx, mrID, expectedRevision, result.Status, result.ChecksJSON,
	)
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return false, nil
	}
	return applied, err
}

const ciRefreshWarning = "Could not refresh CI checks; showing last known status."

type ciStatusFetchResult struct {
	Status     string
	ChecksJSON string
	Updated    bool
	Warning    string
}

func (s *Syncer) fetchGitHubCIStatus(
	ctx context.Context,
	repo RepoRef,
	number int,
	headSHA string,
) (ciStatusFetchResult, error) {
	if headSHA == "" {
		return ciStatusFetchResult{}, nil
	}

	// Fetch both sources. On failure, skip the DB write to preserve
	// existing data rather than wiping it with empty values.
	client, err := s.clientFor(repo)
	if err != nil {
		return ciStatusFetchResult{}, fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	checkRuns, err := client.ListCheckRunsForRef(ctx, repo.Owner, repo.Name, headSHA)
	if err != nil {
		slog.Warn("list check runs failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return ciStatusFetchResult{Warning: ciRefreshWarning}, nil
	}

	combined, err := client.GetCombinedStatus(ctx, repo.Owner, repo.Name, headSHA)
	if err != nil {
		slog.Warn("get combined status failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return ciStatusFetchResult{Warning: ciRefreshWarning}, nil
	}

	return ciStatusFetchResult{
		Status:     DeriveOverallCIStatus(checkRuns, combined),
		ChecksJSON: NormalizeCIChecks(checkRuns, combined),
		Updated:    true,
	}, nil
}

// refreshWorkflowApproval fetches action_required workflow runs at the
// given head SHA and persists the result on the merge request row.
//
// The persisted snapshot is keyed by head SHA for reads and guarded by the
// local parent revision so same-head workflow changes cannot race a newer
// equal-timestamp parent snapshot.
//
// Failures (no client, network errors, closed PR) are intentionally
// silent: this is a refresh, not a precondition. The previous
// persisted state stays in place rather than being clobbered.
func (s *Syncer) refreshWorkflowApproval(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
	headSHA string,
	ghPR *gh.PullRequest,
	normalized *db.MergeRequest,
) (bool, int) {
	if headSHA == "" {
		return true, 0
	}
	state := ""
	switch {
	case ghPR != nil:
		state = ghPR.GetState()
	case normalized != nil:
		state = string(normalized.State)
	}
	if state != "open" {
		return true, 0
	}

	client, err := s.clientFor(repo)
	if err != nil {
		return true, 0
	}
	headRepoFullName := ""
	headRef := ""
	if ghPR != nil && ghPR.GetHead() != nil {
		headRepoFullName = ghPR.GetHead().GetRepo().GetFullName()
		headRef = ghPR.GetHead().GetRef()
	}
	// GraphQL bulk fetch populates clone URL but not full name on the
	// head repo struct, so fall back to parsing the persisted clone
	// URL. Without this, fork PRs synced via bulk would lose the head
	// repo identity needed to match fork-triggered workflow runs whose
	// pull_requests array is empty.
	if headRepoFullName == "" && normalized != nil {
		headRepoFullName = ParseHeadRepoFullName(normalized.HeadRepoCloneURL)
	}
	if headRef == "" && normalized != nil {
		headRef = normalized.HeadBranch
	}

	runs, err := client.ListWorkflowRunsForHeadSHA(ctx, repo.Owner, repo.Name, headSHA)
	if err != nil {
		slog.Warn("list workflow runs for approval refresh failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return true, 1
	}
	approval := WorkflowApprovalStateFromRuns(
		FilterWorkflowRunsAwaitingApproval(runs, PRSource{
			Number:           number,
			HeadSHA:          headSHA,
			HeadRepoFullName: headRepoFullName,
			HeadRef:          headRef,
		}),
	)
	if err := s.db.UpdateMRWorkflowApproval(
		ctx, repoID, number, time.Now().UTC(), headSHA,
		approval.Required, approval.Count,
	); err != nil {
		slog.Warn("persist workflow approval state failed",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
	}
	return true, 1
}

// ciHasPending parses the CI checks JSON and returns true if any
// check has a status other than "completed".
func ciHasPending(ciChecksJSON string) bool {
	if ciChecksJSON == "" {
		return false
	}
	var checks []db.CICheck
	if err := json.Unmarshal([]byte(ciChecksJSON), &checks); err != nil {
		return false
	}
	for _, c := range checks {
		if c.Status != "completed" {
			return true
		}
	}
	return false
}

func computeIssueCommentLastActivity(
	ghIssue *gh.Issue,
	comments []*gh.IssueComment,
) time.Time {
	var latest time.Time
	if ghIssue != nil {
		if ghIssue.UpdatedAt != nil {
			latest = ghIssue.UpdatedAt.Time
		}
		if latest.IsZero() && ghIssue.CreatedAt != nil {
			latest = ghIssue.CreatedAt.Time
		}
	}
	for _, c := range comments {
		switch {
		case c.UpdatedAt != nil && c.UpdatedAt.After(latest):
			latest = c.UpdatedAt.Time
		case c.CreatedAt != nil && c.CreatedAt.After(latest):
			latest = c.CreatedAt.Time
		}
	}
	return latest
}

func computeIssueCommentRefreshLastActivity(
	issue *db.Issue,
	comments []*gh.IssueComment,
) time.Time {
	return computeIssueCommentLastActivity(&gh.Issue{
		CreatedAt: &gh.Timestamp{Time: issue.CreatedAt},
		UpdatedAt: &gh.Timestamp{Time: issue.UpdatedAt},
	}, comments)
}

func (s *Syncer) replacePRCommentEvents(
	ctx context.Context,
	repo RepoRef,
	number int,
	mrID int64,
	expectedRevision int64,
	comments []*gh.IssueComment,
	derived *db.MRDerivedFields,
	visibility map[int64]CommentVisibility,
) (bool, error) {
	if visibility == nil {
		var err error
		visibility, err = s.storedPRCommentVisibility(ctx, mrID)
		if err != nil {
			return false, err
		}
	}
	events := make([]db.MREvent, 0, len(comments))
	for _, c := range comments {
		event := NormalizeCommentEventWithVisibility(mrID, c, visibility[c.GetID()])
		events = append(events, event)
	}
	applied, err := s.commitMergeRequestDatasets(
		ctx, repo, mrID, number, expectedRevision, events, true, nil, nil, nil, false, nil, derived, "",
	)
	return applied, err
}

func (s *Syncer) replaceIssueCommentEvents(
	ctx context.Context,
	repo RepoRef,
	number int,
	issueID int64,
	expectedRevision int64,
	comments []*gh.IssueComment,
	otherEvents []db.IssueEvent,
	derived *db.IssueDerivedFields,
	visibility map[int64]CommentVisibility,
) (bool, error) {
	if visibility == nil {
		var err error
		visibility, err = s.storedIssueCommentVisibility(ctx, issueID)
		if err != nil {
			return false, err
		}
	}
	events := make([]db.IssueEvent, 0, len(comments))
	for _, c := range comments {
		event := NormalizeIssueCommentEventWithVisibility(issueID, c, visibility[c.GetID()])
		events = append(events, event)
	}
	return s.commitIssueCommentsSnapshot(ctx, repo, issueID, number, expectedRevision, events, otherEvents, derived)
}

func (s *Syncer) currentPRCommentVisibility(
	ctx context.Context,
	repo RepoRef,
	number int,
) (map[int64]CommentVisibility, bool) {
	fetcher := s.fetcherForContext(ctx, repo)
	if fetcher == nil || !s.graphQLReadAllowed(ctx, repo, fetcher) {
		return nil, false
	}
	visibility, err := fetcher.FetchPullRequestCommentVisibility(
		ctx, repo.Owner, repo.Name, number,
	)
	if err != nil {
		slog.Warn("current PR comment visibility fetch failed; preserving stored state",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return nil, false
	}
	return visibility, true
}

func (s *Syncer) currentIssueCommentVisibility(
	ctx context.Context,
	repo RepoRef,
	number int,
) (map[int64]CommentVisibility, bool) {
	fetcher := s.fetcherForContext(ctx, repo)
	if fetcher == nil || !s.graphQLReadAllowed(ctx, repo, fetcher) {
		return nil, false
	}
	visibility, err := fetcher.FetchIssueCommentVisibility(
		ctx, repo.Owner, repo.Name, number,
	)
	if err != nil {
		slog.Warn("current issue comment visibility fetch failed; preserving stored state",
			"repo", repo.Owner+"/"+repo.Name,
			"number", number,
			"err", err,
		)
		return nil, false
	}
	return visibility, true
}

func commentMetadataUpdates(
	visibility map[int64]CommentVisibility,
) []db.CommentMetadataUpdate {
	updates := make([]db.CommentMetadataUpdate, 0, len(visibility))
	for platformID, state := range visibility {
		updates = append(updates, db.CommentMetadataUpdate{
			PlatformID: platformID, MetadataJSON: normalizeCommentVisibilityMetadata(state),
		})
	}
	return updates
}

func (s *Syncer) refreshStoredPRCommentVisibility(
	ctx context.Context,
	repo RepoRef,
	mrID int64,
	expectedRevision int64,
	number int,
) error {
	visibility, ok := s.currentPRCommentVisibility(ctx, repo, number)
	if !ok {
		return nil
	}
	applied, err := s.db.UpdateMergeRequestCommentMetadataSnapshot(
		ctx, mrID, expectedRevision, commentMetadataUpdates(visibility),
	)
	if err != nil {
		return fmt.Errorf("update current PR comment visibility for #%d: %w", number, err)
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	return nil
}

func (s *Syncer) refreshStoredIssueCommentVisibility(
	ctx context.Context,
	repo RepoRef,
	issueID int64,
	expectedRevision int64,
	number int,
) error {
	visibility, ok := s.currentIssueCommentVisibility(ctx, repo, number)
	if !ok {
		return nil
	}
	applied, err := s.db.UpdateIssueCommentMetadataSnapshot(
		ctx, issueID, expectedRevision, commentMetadataUpdates(visibility),
	)
	if err != nil {
		return fmt.Errorf("update current issue comment visibility for #%d: %w", number, err)
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	return nil
}

func (s *Syncer) storedPRCommentVisibility(
	ctx context.Context,
	mrID int64,
) (map[int64]CommentVisibility, error) {
	visibility := make(map[int64]CommentVisibility)
	existing, err := s.db.ListMREvents(ctx, mrID)
	if err != nil {
		return nil, fmt.Errorf("list existing PR comments before visibility-preserving replacement: %w", err)
	}
	for _, event := range existing {
		if event.EventType != "issue_comment" || event.PlatformID == nil {
			continue
		}
		if state, ok := commentVisibilityFromMetadata(event.MetadataJSON); ok {
			visibility[*event.PlatformID] = state
		}
	}
	return visibility, nil
}

func (s *Syncer) storedIssueCommentVisibility(
	ctx context.Context,
	issueID int64,
) (map[int64]CommentVisibility, error) {
	visibility := make(map[int64]CommentVisibility)
	existing, err := s.db.ListIssueEvents(ctx, issueID)
	if err != nil {
		return nil, fmt.Errorf("list existing issue comments before visibility-preserving replacement: %w", err)
	}
	for _, event := range existing {
		if event.EventType != "issue_comment" || event.PlatformID == nil {
			continue
		}
		if state, ok := commentVisibilityFromMetadata(event.MetadataJSON); ok {
			visibility[*event.PlatformID] = state
		}
	}
	return visibility, nil
}

func commentVisibilityFromMetadata(metadataJSON string) (CommentVisibility, bool) {
	if metadataJSON == "" {
		return CommentVisibility{}, false
	}
	var metadata struct {
		Hidden bool   `json:"provider_hidden"`
		Reason string `json:"provider_hidden_reason"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil || !metadata.Hidden {
		return CommentVisibility{}, false
	}
	return CommentVisibility{Hidden: true, Reason: metadata.Reason}, true
}

// resolveDisplayName returns the GitHub display name for a
// login and whether the lookup succeeded. Returns ("", false)
// on API failure so callers can preserve existing data. Uses a
// TTL + LRU cache that spans the Syncer's lifetime plus
// singleflight dedup so concurrent workers racing on the same
// author only trigger one GetUser call. When a refetch fails
// but a stale cache entry exists, the stale value is returned
// (stale-while-error).
//
// Bot logins (ending with "[bot]") are returned as-is since bot
// accounts have no display name on the GitHub API.
//
// The lookup is routed by repo so a configuration with no host
// fallback route still resolves names, and so the request is
// billed to the identity serving that repository. A display name
// does not vary by credential, so the cache stays host-keyed.
func (s *Syncer) resolveDisplayName(
	ctx context.Context, client Client, repo RepoRef, login string,
) (string, bool) {
	key := repoHost(repo) + "\x00" + login
	if cached, fresh := s.displayNames.get(key); fresh {
		return cached.name, cached.ok
	}
	if strings.HasSuffix(login, "[bot]") {
		s.displayNames.putSuccess(key, login)
		return login, true
	}

	v, err, _ := s.displayNameGroup.Do(key, func() (any, error) {
		// Re-check the cache inside the singleflight slot:
		// another caller may have populated a fresh entry
		// while this one was waiting for its turn to run.
		if cached, fresh := s.displayNames.get(key); fresh {
			return cached, nil
		}
		user, err := s.getUserForRepo(ctx, client, repo, login)
		if err != nil {
			return displayNameEntry{}, err
		}
		name := nameOrEmpty(user)
		s.displayNames.putSuccess(key, name)
		return displayNameEntry{name: name, ok: true}, nil
	})
	if err != nil {
		// Fall back to a stale cached name if one exists so a
		// transient network error does not blank out an
		// already-known name. A zero entry has ok=false, so a
		// total miss falls through to the failure path below.
		//
		// Also back off the retry window: re-use the stored
		// name but with failureTTL so repeated failures do not
		// hit /users every sync for the life of successTTL.
		if stale, _ := s.displayNames.get(key); stale.ok {
			s.displayNames.putStaleFallback(key, stale.name)
			return stale.name, true
		}
		slog.Warn("get user display name failed",
			"login", login, "err", err,
		)
		s.displayNames.putFailure(key)
		return "", false
	}
	result := v.(displayNameEntry)
	return result.name, result.ok
}

// getUserForRepo prefers a repository-routed user lookup so routed hosts pick
// the repository's credential. A client without repository routing, or a repo
// ref missing owner/name, falls back to the plain host lookup.
func (s *Syncer) getUserForRepo(
	ctx context.Context, client Client, repo RepoRef, login string,
) (*gh.User, error) {
	if reader, ok := client.(repoUserClient); ok &&
		repo.Owner != "" && repo.Name != "" {
		return reader.GetUserForRepo(ctx, repo.Owner, repo.Name, login)
	}
	return client.GetUser(ctx, login)
}

// --- Issue sync ---

// syncIssuesFromList processes a pre-fetched list of open issues
// via the REST path. Handles per-issue upsert and closure detection.
func (s *Syncer) syncIssuesFromList(
	ctx context.Context,
	client Client,
	repo RepoRef,
	repoID int64,
	ghIssues []*gh.Issue,
	forceRefresh bool,
) error {
	stillOpen := make(map[int]bool, len(ghIssues))
	for _, issue := range ghIssues {
		stillOpen[issue.GetNumber()] = true
	}

	var hadItemFailure bool
	var budgetCause error
	progress := newIssueSyncProgressLogger(repo, "rest", len(ghIssues))
	for i, ghIssue := range ghIssues {
		if err := s.syncOpenIssue(ctx, client, repo, repoID, ghIssue, forceRefresh); err != nil {
			if disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureIssues, err,
			); disabledErr != nil {
				return preservePartialSyncFailure(
					failIssues, hadItemFailure, joinPartialFailureCause(budgetCause, disabledErr),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("sync issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", ghIssue.GetNumber(),
				"err", err,
			)
			hadItemFailure = true
		}
		progress.record(i + 1)
	}

	closedNumbers, err := s.db.GetPreviouslyOpenIssueNumbers(
		ctx, repoID, stillOpen,
	)
	if err != nil {
		return joinPartialFailureCause(
			budgetCause, fmt.Errorf("get previously open issues: %w", err),
		)
	}
	for _, number := range closedNumbers {
		if err := s.fetchAndUpdateClosedIssue(
			ctx, repo, repoID, number,
		); err != nil {
			if disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureIssues, err,
			); disabledErr != nil {
				return preservePartialSyncFailure(
					failIssues, hadItemFailure, joinPartialFailureCause(budgetCause, disabledErr),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("update closed issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			hadItemFailure = true
		}
	}

	if hadItemFailure {
		return partialItemFailureError("one or more issue sync items failed", budgetCause)
	}
	progress.done()
	return nil
}

func (s *Syncer) syncPlatformIssuesFromList(
	ctx context.Context,
	reader platform.IssueReader,
	repo RepoRef,
	repoID int64,
	issues []platform.Issue,
	forceRefresh bool,
) error {
	stillOpen := make(map[int]bool, len(issues))
	for _, issue := range issues {
		stillOpen[issue.Number] = true
	}

	var hadItemFailure bool
	var budgetCause error
	progress := newIssueSyncProgressLogger(repo, "provider", len(issues))
	for i, issue := range issues {
		if err := s.syncOpenPlatformIssue(ctx, reader, repo, repoID, issue, forceRefresh); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failIssues, hadItemFailure, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("sync issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", issue.Number,
				"err", err,
			)
			hadItemFailure = true
		}
		progress.record(i + 1)
	}

	closedNumbers, err := s.db.GetPreviouslyOpenIssueNumbers(
		ctx, repoID, stillOpen,
	)
	if err != nil {
		return joinPartialFailureCause(
			budgetCause, fmt.Errorf("get previously open issues: %w", err),
		)
	}
	for _, number := range closedNumbers {
		if err := s.fetchAndUpdateClosedPlatformIssue(
			ctx, repo, repoID, number,
		); err != nil {
			if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
				return preservePartialSyncFailure(
					failIssues, hadItemFailure, joinPartialFailureCause(budgetCause, err),
				)
			}
			budgetCause = retainSyncBudgetCause(budgetCause, err)
			slog.Error("update closed issue failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
			hadItemFailure = true
		}
	}

	if hadItemFailure {
		return partialItemFailureError("one or more issue sync items failed", budgetCause)
	}
	progress.done()
	return nil
}

func (s *Syncer) syncOpenPlatformIssue(
	ctx context.Context,
	reader platform.IssueReader,
	repo RepoRef,
	repoID int64,
	issue platform.Issue,
	forceRefresh bool,
) error {
	normalized := platform.DBIssue(repoID, issue)

	existing, err := s.db.GetIssueByRepoIDAndNumber(
		ctx, repoID, issue.Number,
	)
	if err != nil {
		return fmt.Errorf(
			"get existing issue #%d: %w", issue.Number, err,
		)
	}

	needsTimeline := forceRefresh || existing == nil ||
		!existing.UpdatedAt.Equal(normalized.UpdatedAt)

	issueID, revision, accepted, err := s.commitIssueParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf(
			"upsert issue #%d: %w", issue.Number, err,
		)
	}
	if !accepted {
		return nil
	}

	if !needsTimeline {
		if existing != nil && existing.DetailFetchedAt != nil {
			s.queueIssueCommentSync(repo, existing.RepoID, existing.Number)
		}
		return nil
	}

	events, err := reader.ListIssueEvents(ctx, platformRepoRef(repo), issue.Number)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupportedCapability) {
			return nil
		}
		return fmt.Errorf("list issue events for #%d: %w", issue.Number, err)
	}
	comments := make([]db.IssueEvent, 0, len(events))
	dbEvents := make([]db.IssueEvent, 0, len(events))
	for _, event := range events {
		dbEvent := platform.DBIssueEvent(issueID, event)
		if dbEvent.EventType == "issue_comment" {
			comments = append(comments, dbEvent)
		} else {
			dbEvents = append(dbEvents, dbEvent)
		}
	}
	applied, err := s.commitIssueCommentsSnapshot(ctx, repo, issueID, issue.Number, revision, comments, dbEvents, nil)
	if err != nil {
		return fmt.Errorf("replace issue comments for #%d: %w", issue.Number, err)
	}
	if !applied {
		return nil
	}
	return nil
}

func (s *Syncer) syncOpenIssue(
	ctx context.Context,
	client Client,
	repo RepoRef,
	repoID int64,
	ghIssue *gh.Issue,
	forceRefresh bool,
) error {
	normalized, err := NormalizeIssue(repoID, ghIssue)
	if err != nil {
		return fmt.Errorf("normalize issue #%d: %w", ghIssue.GetNumber(), err)
	}

	existing, err := s.db.GetIssueByRepoIDAndNumber(
		ctx, repoID, ghIssue.GetNumber(),
	)
	if err != nil {
		return fmt.Errorf(
			"get existing issue #%d: %w", ghIssue.GetNumber(), err,
		)
	}

	needsTimeline := forceRefresh || existing == nil ||
		!existing.UpdatedAt.Equal(normalized.UpdatedAt)

	issueID, revision, accepted, err := s.commitIssueParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf(
			"upsert issue #%d: %w", ghIssue.GetNumber(), err,
		)
	}
	if !accepted {
		return nil
	}

	if !needsTimeline {
		if existing != nil && existing.DetailFetchedAt != nil {
			s.queueIssueCommentSync(repo, existing.RepoID, existing.Number)
		}
		return nil
	}

	if err := s.refreshIssueTimeline(ctx, repo, issueID, revision, ghIssue, nil); err != nil {
		if errors.Is(err, errParentSnapshotAdvanced) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Syncer) refreshIssueTimeline(
	ctx context.Context,
	repo RepoRef,
	issueID int64,
	expectedRevision int64,
	ghIssue *gh.Issue,
	visibility map[int64]CommentVisibility,
) error {
	if ghIssue == nil {
		return fmt.Errorf("nil issue")
	}
	number := ghIssue.GetNumber()
	client, err := s.clientFor(repo)
	if err != nil {
		return fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}

	comments, err := client.ListIssueComments(
		ctx, repo.Owner, repo.Name, number,
	)
	if err != nil {
		return fmt.Errorf(
			"list comments for issue #%d: %w", number, err,
		)
	}
	if visibility == nil {
		visibility, err = s.storedIssueCommentVisibility(ctx, issueID)
		if err != nil {
			return fmt.Errorf(
				"load stored comment visibility for issue #%d: %w", number, err,
			)
		}
		if observed, ok := s.currentIssueCommentVisibility(ctx, repo, number); ok {
			visibility = observed
		}
	} else {
		storedVisibility, err := s.storedIssueCommentVisibility(ctx, issueID)
		if err != nil {
			return fmt.Errorf(
				"load stored comment visibility for issue #%d: %w", number, err,
			)
		}
		maps.Copy(storedVisibility, visibility)
		visibility = storedVisibility
	}

	derived := db.IssueDerivedFields{
		CommentCount:   len(comments),
		LastActivityAt: computeIssueCommentLastActivity(ghIssue, comments),
	}
	var otherEvents []db.IssueEvent
	if timelineClient, ok := client.(issueTimelineLister); ok {
		timelineEvents, err := timelineClient.ListIssueTimelineEvents(
			ctx, repo.Owner, repo.Name, number,
		)
		if err != nil {
			if disabledErr := repositoryFeatureDisabledError(
				repo, platform.RepositoryFeatureIssues, err,
			); disabledErr != nil {
				return disabledErr
			}
			if IsArchiveSyncBudgetContext(ctx) {
				return fmt.Errorf(
					"list timeline events for issue #%d: %w", number, err,
				)
			}
			slog.Warn("issue timeline event fetch failed during timeline refresh",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
		} else {
			otherEvents = normalizeIssueTimelineEvents(issueID, timelineEvents)
		}
	}
	applied, err := s.replaceIssueCommentEvents(
		ctx, repo, number, issueID, expectedRevision, comments, otherEvents, &derived, visibility,
	)
	if err != nil {
		return fmt.Errorf(
			"replace issue events for #%d: %w", number, err,
		)
	}
	if !applied {
		return errParentSnapshotAdvanced
	}

	return nil
}

func normalizeIssueTimelineEvents(
	issueID int64,
	timelineEvents []PullRequestTimelineEvent,
) []db.IssueEvent {
	events := make([]db.IssueEvent, 0, len(timelineEvents))
	for _, timelineEvent := range timelineEvents {
		event := NormalizeIssueTimelineEvent(issueID, timelineEvent)
		if event == nil {
			continue
		}
		events = append(events, *event)
	}
	return events
}

func (s *Syncer) refreshRepoPRComments(
	ctx context.Context,
	repo RepoRef,
) {
	probe, due := s.beginRepositoryFeatureProbe(
		ctx, repo, platform.RepositoryFeatureMergeRequests,
	)
	if !due {
		return
	}
	providerAttempted := false
	defer func() {
		if providerAttempted {
			probe.release()
		} else {
			probe.abandon()
		}
	}()

	prs, err := s.db.ListMergeRequests(ctx, db.ListMergeRequestsOpts{
		PlatformHost: repoHost(repo),
		RepoOwner:    repo.Owner,
		RepoName:     repo.Name,
		State:        "open",
	})
	if err != nil {
		slog.Warn("comment refresh: list open PRs failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return
	}

	client, err := s.clientFor(repo)
	if err != nil {
		slog.Warn("comment refresh: resolve client failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return
	}

	for i := range prs {
		if ctx.Err() != nil {
			return
		}
		attempted, disabled := s.refreshPRCommentsForItem(ctx, client, repo, &prs[i])
		providerAttempted = providerAttempted || attempted
		if disabled {
			return
		}
	}
}

func (s *Syncer) refreshRepoIssueComments(
	ctx context.Context,
	repo RepoRef,
) {
	probe, due := s.beginRepositoryFeatureProbe(
		ctx, repo, platform.RepositoryFeatureIssues,
	)
	if !due {
		return
	}
	providerAttempted := false
	defer func() {
		if providerAttempted {
			probe.release()
		} else {
			probe.abandon()
		}
	}()

	issues, err := s.db.ListIssues(ctx, db.ListIssuesOpts{
		PlatformHost: repoHost(repo),
		RepoOwner:    repo.Owner,
		RepoName:     repo.Name,
		State:        "open",
	})
	if err != nil {
		slog.Warn("comment refresh: list open issues failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return
	}

	client, err := s.clientFor(repo)
	if err != nil {
		slog.Warn("comment refresh: resolve client failed",
			"repo", repo.Owner+"/"+repo.Name,
			"err", err,
		)
		return
	}

	for i := range issues {
		if ctx.Err() != nil {
			return
		}
		attempted, disabled := s.refreshIssueCommentsForItem(ctx, client, repo, &issues[i])
		providerAttempted = providerAttempted || attempted
		if disabled {
			return
		}
	}
}

func (s *Syncer) canSpendCommentRefresh(repo RepoRef) bool {
	budget, ok := s.BudgetForRepo(repo)
	return !ok || budget == nil || budget.CanSpend(1)
}

func (s *Syncer) canSpendWorkflowApprovalRefresh(repo RepoRef) bool {
	budget, ok := s.BudgetForRepo(repo)
	return !ok || budget == nil || budget.CanSpend(1)
}

func (s *Syncer) persistPRComments(
	ctx context.Context,
	repo RepoRef,
	pr *db.MergeRequest,
	comments []*gh.IssueComment,
) error {
	applied, err := s.replacePRCommentEvents(
		ctx, repo, pr.Number, pr.ID, pr.SnapshotRevision, comments, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("replace PR comment events: %w", err)
	}
	if !applied {
		return nil
	}
	return nil
}

func (s *Syncer) persistIssueComments(
	ctx context.Context,
	repo RepoRef,
	issue *db.Issue,
	comments []*gh.IssueComment,
) error {
	derived := db.IssueDerivedFields{
		CommentCount:   len(comments),
		LastActivityAt: computeIssueCommentRefreshLastActivity(issue, comments),
	}
	applied, err := s.replaceIssueCommentEvents(
		ctx, repo, issue.Number, issue.ID, issue.SnapshotRevision, comments, nil, &derived, nil,
	)
	if err != nil {
		return fmt.Errorf("replace issue comment events: %w", err)
	}
	if !applied {
		return nil
	}

	return nil
}

func (s *Syncer) fetchAndUpdateClosedIssue(
	ctx context.Context, repo RepoRef, repoID int64, number int,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeIssue, number,
	)
	if err != nil {
		return fmt.Errorf("check closed issue #%d visibility: %w", number, err)
	}
	if removed {
		return nil
	}
	client, err := s.clientFor(repo)
	if err != nil {
		return fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	ghIssue, err := client.GetIssue(
		ctx, repo.Owner, repo.Name, number,
	)
	// Route fetch failures and detected transfers through the canonical
	// lookup classification so removed, inaccessible, and moved items
	// surface typed outcomes instead of generic upstream failures.
	if outcomeErr := s.issueOnlyFetchOutcomeError(ctx, repo, number, ghIssue, err); outcomeErr != nil {
		// A not_found without a destination is a true removal and gets a
		// terminal local state. A transfer carries the destination and
		// keeps failing the cycle so the maintainer sees the moved item
		// in repo sync health instead of it silently closing here.
		if errors.Is(outcomeErr, platform.ErrNotFound) &&
			lookupDestination(outcomeErr) == nil {
			return s.tombstoneRemovedIssue(ctx, repo, repoID, number)
		}
		return fmt.Errorf("get closed issue #%d: %w", number, outcomeErr)
	}
	if err != nil {
		return fmt.Errorf("get closed issue #%d: %w", number, err)
	}
	if ghIssue == nil {
		return fmt.Errorf("get closed issue #%d: client returned nil issue", number)
	}

	normalized, err := NormalizeIssue(repoID, ghIssue)
	if err != nil {
		return fmt.Errorf("normalize closed issue #%d: %w", number, err)
	}
	if existing, getErr := s.db.GetIssueByRepoIDAndNumber(ctx, repoID, number); getErr != nil {
		return fmt.Errorf("get closed issue #%d: %w", number, getErr)
	} else if existing != nil {
		normalized.DetailFetchedAt = existing.DetailFetchedAt
		normalized.Starred = existing.Starred
	}
	if _, _, accepted, commitErr := s.commitIssueParentSnapshot(ctx, repo, normalized); commitErr != nil {
		return fmt.Errorf("commit closed issue #%d: %w", number, commitErr)
	} else if !accepted {
		return nil
	}

	return s.markClosedLinkedIssueNotificationsDone(ctx, repoID, number)
}

// lookupDestination extracts the transfer destination from a typed lookup
// error, or nil when the error carries none (a true removal).
func lookupDestination(err error) *platform.RepoRef {
	var pErr *platform.Error
	if errors.As(err, &pErr) && pErr != nil {
		return pErr.Destination
	}
	return nil
}

// tombstoneRemovedIssue closes the local copy of a previously-open issue
// whose provider lookup is explicitly classified as not present (deleted or
// transferred upstream). Without a terminal state the number stays
// "previously open" forever: every cycle re-fetches it, fails the repo's
// issue sync, and spends a lookup plus repository probe on an item that can
// never resolve.
func (s *Syncer) tombstoneRemovedIssue(
	ctx context.Context, repo RepoRef, repoID int64, number int,
) error {
	existing, err := s.db.GetIssueByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return fmt.Errorf("get removed issue #%d: %w", number, err)
	}
	if existing == nil || existing.State != "open" {
		return nil
	}
	now := time.Now().UTC()
	tombstone := *existing
	tombstone.State = "closed"
	if tombstone.ClosedAt == nil {
		tombstone.ClosedAt = &now
	}
	// Upstream is gone; there is no detail left to fetch, so keep the
	// detail drain from retrying the lookup.
	if tombstone.DetailFetchedAt == nil {
		tombstone.DetailFetchedAt = &now
	}
	if _, _, _, err := s.commitIssueParentSnapshot(ctx, repo, &tombstone); err != nil {
		return fmt.Errorf("tombstone removed issue #%d: %w", number, err)
	}
	slog.Info("issue removed upstream; closed local copy",
		"repo", repo.Owner+"/"+repo.Name,
		"number", number,
	)
	return nil
}

func (s *Syncer) fetchAndUpdateClosedPlatformIssue(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeIssue, number,
	)
	if err != nil {
		return fmt.Errorf("check closed issue #%d visibility: %w", number, err)
	}
	if removed {
		return nil
	}
	issueReader, err := s.issueReaderFor(repo)
	if err != nil {
		return fmt.Errorf("resolve issue reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	issue, err := issueReader.GetIssue(ctx, platformRepoRef(repo), number)
	if err != nil {
		// No tombstone here: a bare provider 404 is ambiguous on the
		// neutral path (GitLab hides inaccessible items behind 404), so
		// the row is retained and the failure surfaces as partial. Only
		// GitHub's classified lookup can distinguish a true removal.
		return fmt.Errorf("get closed issue #%d: %w", number, err)
	}
	normalized := platform.DBIssue(repoID, issue)
	_, _, accepted, err := s.commitIssueParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf("upsert closed issue #%d: %w", number, err)
	}
	if !accepted {
		return nil
	}
	return nil
}

// --- Detail Drain ---

// drainDetailQueue builds a priority queue of items needing detail
// fetches and processes them within the per-provider/host budget.
func (s *Syncer) drainDetailQueue(
	ctx context.Context,
	eligibleBuckets map[string]bool,
	repos []RepoRef,
) {
	if len(s.budgets) == 0 {
		return
	}

	items := s.buildDetailQueueItems(ctx, repos)
	if len(items) == 0 {
		return
	}

	queue := BuildQueue(items, time.Now())
	if len(queue) == 0 {
		return
	}

	// Track which hosts are exhausted so we skip quickly.
	exhausted := make(map[string]bool)
	verifiedRepos := make(map[string]RepoRef)
	verifiedRepoIDs := make(map[string]int64)
	verifiedRouteFences := make(map[string]db.RepositoryRouteFence)
	rejectedRepos := make(map[string]bool)

	for i := range queue {
		if ctx.Err() != nil {
			return
		}
		qi := &queue[i]
		repo := RepoRef{
			Owner: qi.RepoOwner, Name: qi.RepoName,
			Platform: qi.Platform, PlatformHost: qi.PlatformHost,
		}
		host := repoHost(repo)
		if tracked, ok := s.trackedRepoByIdentity(qi.Platform, qi.RepoOwner, qi.RepoName, host); ok {
			repo = tracked
			repo.Owner = qi.RepoOwner
			repo.Name = qi.RepoName
			repo.PlatformHost = host
		}
		// The queue was built before the pass ran, so it can hold items
		// for a repository the pass just published as archived. Detail
		// work is live syncing; archived repos hydrate through the
		// archive budget path instead.
		if repo.Archived {
			continue
		}
		// Resolve the credential bucket from the tracked repository. Routing
		// keys off the owner and host that survive tracking, not the raw
		// queue row, so this must follow the lookup above.
		bucket, err := s.bucketKeyForRepo(repo, false)
		if err != nil || !eligibleBuckets[bucket] {
			continue
		}
		feature := platform.RepositoryFeatureIssues
		if qi.Type == QueueItemPR {
			feature = platform.RepositoryFeatureMergeRequests
		}
		probe, due := s.beginRepositoryFeatureProbe(ctx, repo, feature)
		if !due {
			continue
		}
		if exhausted[bucket] {
			probe.abandon()
			continue
		}

		budget := s.budgets[bucket]
		if budget == nil {
			probe.abandon()
			continue
		}

		// Soft admission gate: check if the budget has nominal
		// capacity for this item. The transport layer handles
		// actual per-RoundTrip accounting; this prevents starting
		// work we almost certainly can't afford.
		worstCase := qi.WorstCaseCost()
		if !budget.CanSpend(worstCase) {
			probe.abandon()
			exhausted[bucket] = true
			continue
		}
		// The provider reserve is re-read per item alongside the local
		// ceiling. A drain can be many items long, so a credential that had
		// headroom when the drain started can reach its reserve partway
		// through.
		if s.backgroundReserveExhausted(repo, QuotaResourceREST, false) {
			probe.abandon()
			exhausted[bucket] = true
			continue
		}
		repoKey := detailRepoKey(repoPlatform(repo), host, qi.RepoOwner, qi.RepoName)
		if rejectedRepos[repoKey] {
			probe.abandon()
			continue
		}
		repoID, verified := verifiedRepoIDs[repoKey]
		routeFence := verifiedRouteFences[repoKey]
		if verified {
			repo = verifiedRepos[repoKey]
		} else {
			resolvedRepo, resolvedRepoID, resolvedFence, found, resolveErr :=
				s.reconcileRepoForDirectSync(ctx, repo)
			if resolveErr != nil || !found {
				probe.abandon()
				rejectedRepos[repoKey] = true
				slog.Warn("detail drain: verify repo identity failed",
					"repo", qi.RepoOwner+"/"+qi.RepoName,
					"found", found,
					"err", resolveErr,
				)
				continue
			}
			repo = resolvedRepo
			repoID = resolvedRepoID
			routeFence = resolvedFence
			verifiedRepos[repoKey] = repo
			verifiedRepoIDs[repoKey] = repoID
			verifiedRouteFences[repoKey] = routeFence
		}
		if repo.Archived {
			// Identity verification just discovered the archived flip;
			// the publication already dropped the repo from live sync.
			probe.abandon()
			rejectedRepos[repoKey] = true
			continue
		}
		if repoID == 0 {
			probe.abandon()
			rejectedRepos[repoKey] = true
			slog.Warn("detail drain: verified repo has no database identity",
				"repo", qi.RepoOwner+"/"+qi.RepoName,
			)
			continue
		}
		itemCtx := withCloneRepositoryIdentity(ctx, repo)

		// Compute diff SHAs if clone available.
		cloneFetchOK := false
		if s.clones != nil {
			if cloneErr := s.ensureCloneForRoute(
				itemCtx, repo, repoID, routeFence,
			); cloneErr != nil {
				slog.Warn("detail drain: bare clone failed",
					"repo", qi.RepoOwner+"/"+qi.RepoName,
					"err", cloneErr,
				)
			} else {
				cloneFetchOK = true
			}
		}
		providerCalls := 0
		if qi.Type == QueueItemPR {
			providerCalls, err = s.fetchMRDetail(
				itemCtx, repo, repoID, qi.Number, cloneFetchOK,
			)
		} else {
			providerCalls, err = s.fetchIssueDetail(
				itemCtx, repo, repoID, qi.Number,
			)
		}

		if err != nil {
			disabledErr := repositoryFeatureDisabledError(repo, feature, err)
			disabled := disabledErr != nil &&
				s.recordRepositoryFeatureDisabled(repo, feature, disabledErr)
			if providerCalls > 0 {
				probe.release()
			} else {
				probe.abandon()
			}
			if disabled {
				continue
			}
			slog.Warn("detail drain: fetch failed",
				"repo", qi.RepoOwner+"/"+qi.RepoName,
				"number", qi.Number,
				"type", qi.Type,
				"err", err,
			)
			continue
		}
		if providerCalls > 0 {
			probe.release()
		} else {
			probe.abandon()
		}
	}
}

// buildDetailQueueItems queries the DB for open PRs and issues
// that may need a detail fetch, combining with starred/watched
// state to build queue items for scoring.
func (s *Syncer) buildDetailQueueItems(
	ctx context.Context,
	repos []RepoRef,
) []QueueItem {
	// Build the set of repos selected for this run. In addition to filtering
	// stale DB rows from removed repos, this keeps repository-scoped runs from
	// spending their detail budget on unrelated repositories on the same host.
	trackedRepos := make(map[string]bool, len(repos))
	for _, r := range repos {
		trackedRepos[detailRepoKey(repoPlatform(r), repoHost(r), r.Owner, r.Name)] = true
	}

	// Gather watched MR numbers for matching.
	s.watchMu.Lock()
	watched := make(map[string]bool, len(s.watchedMRs))
	for _, w := range s.watchedMRs {
		watched[watchedMRKey(w)] = true
	}
	s.watchMu.Unlock()

	var items []QueueItem

	// Open PRs.
	prs, err := s.db.ListMergeRequests(
		ctx, db.ListMergeRequestsOpts{State: "open"},
	)
	if err != nil {
		slog.Warn("detail drain: list open PRs failed",
			"err", err,
		)
		return nil
	}
	prCountsByRepoID := make(map[int64]int, len(prs))
	for _, pr := range prs {
		prCountsByRepoID[pr.RepoID]++
	}
	for _, pr := range prs {
		repo, rErr := s.db.GetRepoByID(ctx, pr.RepoID)
		if rErr != nil || repo == nil {
			continue
		}
		repoKey := detailRepoKey(platform.Kind(repo.Platform), repo.PlatformHost, repo.Owner, repo.Name)
		if !trackedRepos[repoKey] {
			continue
		}
		watchKey := detailRepoKey(
			platform.Kind(repo.Platform), repo.PlatformHost,
			repo.Owner, repo.Name,
		) + fmt.Sprintf("#%d", pr.Number)
		ciHadPending := pr.CIHadPending || ciHasPending(pr.CIChecksJSON)
		items = append(items, QueueItem{
			Type:            QueueItemPR,
			Platform:        platform.Kind(repo.Platform),
			RepoOwner:       repo.Owner,
			RepoName:        repo.Name,
			Number:          pr.Number,
			PlatformHost:    repo.PlatformHost,
			UpdatedAt:       pr.UpdatedAt,
			DetailFetchedAt: pr.DetailFetchedAt,
			CIHadPending:    ciHadPending,
			Starred:         pr.Starred,
			Watched:         watched[watchKey],
			IsOpen:          true,
			LargeRepo:       prCountsByRepoID[pr.RepoID] >= largeRepoBulkGraphQLThreshold,
		})
	}

	// Open issues.
	issues, err := s.db.ListIssues(
		ctx, db.ListIssuesOpts{State: "open"},
	)
	if err != nil {
		slog.Warn("detail drain: list open issues failed",
			"err", err,
		)
		return items
	}
	issueCountsByRepoID := make(map[int64]int, len(issues))
	for _, issue := range issues {
		issueCountsByRepoID[issue.RepoID]++
	}
	for _, issue := range issues {
		repo, rErr := s.db.GetRepoByID(ctx, issue.RepoID)
		if rErr != nil || repo == nil {
			continue
		}
		repoKey := detailRepoKey(platform.Kind(repo.Platform), repo.PlatformHost, repo.Owner, repo.Name)
		if !trackedRepos[repoKey] {
			continue
		}
		items = append(items, QueueItem{
			Type:            QueueItemIssue,
			Platform:        platform.Kind(repo.Platform),
			RepoOwner:       repo.Owner,
			RepoName:        repo.Name,
			Number:          issue.Number,
			PlatformHost:    repo.PlatformHost,
			UpdatedAt:       issue.UpdatedAt,
			DetailFetchedAt: issue.DetailFetchedAt,
			Starred:         issue.Starred,
			IsOpen:          true,
			LargeRepo:       issueCountsByRepoID[issue.RepoID] >= largeRepoBulkGraphQLThreshold,
		})
	}

	return items
}

// IsTrackedRepo checks whether the given repo is in the configured list.
func (s *Syncer) IsTrackedRepo(owner, name string) bool {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	for _, r := range s.repos {
		if strings.EqualFold(r.Owner, owner) &&
			strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// TrackedRepos returns a snapshot of the tracked repositories.
func (s *Syncer) TrackedRepos() []RepoRef {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()

	return slices.Clone(s.repos)
}

// isTrackedRepoOnHost checks whether the given repo on a specific host
// is in the configured list. Used by the watched-MR path where the
// host is known and must match exactly.
func (s *Syncer) isTrackedRepoOnHost(owner, name, host string) bool {
	_, ok := s.trackedRepoOnHost(owner, name, host)
	return ok
}

// IsTrackedRepoOnHost checks whether the given repo on a specific host
// is in the configured list.
func (s *Syncer) IsTrackedRepoOnHost(owner, name, host string) bool {
	return s.isTrackedRepoOnHost(owner, name, host)
}

// SyncRepoOnProvider performs the index sync for one configured repository.
// It is used by manual refresh paths that need fresh PR discovery before they
// can act on a specific item number.
func (s *Syncer) SyncRepoOnProvider(
	ctx context.Context,
	kind platform.Kind,
	host, owner, name string,
) error {
	repo, ok := s.trackedRepoByIdentity(kind, owner, name, host)
	if !ok {
		host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
		return fmt.Errorf(
			"repo %s/%s on %s/%s is not tracked",
			owner, name, kind, host,
		)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	return s.syncRepo(withRepositoryFeatureCooldownBypass(
		ctx, s.featureCooldowns.currentGeneration(),
	), repo)
}

// SyncMR fetches fresh data for a single MR from GitHub and updates the DB.
// Unlike the periodic sync, this always does a full fetch (details, timeline, CI).
// Returns an error if the repo is not in the configured repo list.
func (s *Syncer) SyncMR(ctx context.Context, owner, name string, number int) error {
	return s.syncMRWithHost(ctx, owner, name, number, "")
}

// SyncMROnProvider fetches fresh data for a single MR from a specific
// configured provider host.
// SyncClosedMROnProvider records a merge request the provider now reports as
// closed or merged, through the same close-detection flow the periodic sync
// uses. UI-driven terminal mutations call this instead of writing local state
// eagerly, so every terminal transition — user-initiated or detected — flows
// through the one parent-snapshot choke point, which finalizes commit
// liveness inside the transition's own transaction, and local rows always
// carry provider timestamps instead of eager local ones that would leave
// later resyncs rejected as stale. The provider target is resolved from the
// stable repository row for repoID — the same discipline the merged-actor
// backfill used — so a rename or route reuse between the mutation and this
// resync cannot fetch from a different repository than the row being updated.
func (s *Syncer) SyncClosedMROnProvider(
	ctx context.Context,
	repoID int64,
	number int,
) error {
	stored, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		return fmt.Errorf("get repo %d for closed-MR resync: %w", repoID, err)
	}
	if stored == nil {
		return fmt.Errorf("repo %d is not known for closed-MR resync", repoID)
	}
	kind := platform.Kind(stored.Platform)
	repo, ok := s.trackedRepoByProviderID(
		kind, stored.PlatformHost, strings.TrimSpace(stored.PlatformRepoID),
	)
	if !ok {
		routed, routeOK := s.trackedRepoByIdentity(
			kind, stored.Owner, stored.Name, stored.PlatformHost,
		)
		if !routeOK {
			return fmt.Errorf(
				"repo %s/%s on %s/%s is not tracked",
				stored.Owner, stored.Name, stored.Platform, stored.PlatformHost,
			)
		}
		if routedID := strings.TrimSpace(routed.PlatformExternalID); routedID != "" &&
			routedID != strings.TrimSpace(stored.PlatformRepoID) {
			return fmt.Errorf(
				"tracked repo %s/%s provider ID %q does not match stored provider ID %q",
				stored.Owner, stored.Name, routedID, stored.PlatformRepoID,
			)
		}
		repo = routed
	}
	repo = repoRefFromStoredIdentity(repo, *stored)
	identity := platform.DBRepoIdentity(platformRepoRef(repo))
	routeFence, found, err := s.db.CurrentRepositoryRouteFence(
		ctx, identity, repoID,
	)
	if err != nil {
		return fmt.Errorf(
			"capture repository route for closed-MR resync %s/%s: %w",
			repo.Owner, repo.Name, err,
		)
	}
	if !found {
		return nil
	}
	ctx = withCloneRepositoryIdentity(ctx, repo)
	ctx = s.db.WithRepositoryRouteFence(ctx, identity, routeFence)
	reader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return fmt.Errorf(
			"resolve merge request reader for %s/%s: %w",
			stored.Owner, stored.Name, err,
		)
	}
	return s.fetchAndUpdateClosedMergeRequest(ctx, reader, repo, repoID, number, true)
}

func (s *Syncer) SyncMROnProvider(
	ctx context.Context,
	kind platform.Kind,
	host, owner, name string,
	number int,
) error {
	repo, ok := s.trackedRepoByIdentity(kind, owner, name, host)
	if !ok {
		host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
		return fmt.Errorf(
			"repo %s/%s on %s/%s is not tracked",
			owner, name, kind, host,
		)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	return s.syncMRForRepo(ctx, repo, number, false, nil)
}

// SyncMRForRepository refreshes an MR only while repoID still owns repo's
// route. It is intended for delayed work that was queued for a stable
// repository identity rather than for an owner/name route.
func (s *Syncer) SyncMRForRepository(
	ctx context.Context,
	repo RepoRef,
	repoID int64,
	number int,
) error {
	return s.syncMRForRepoResolved(
		ctx, repo, number, false, nil, nil, nil, nil, &repoID,
	)
}

// syncMRWithHost is the internal implementation of SyncMR.
// When hostHint is non-empty it is used instead of resolving via
// s.hostFor, avoiding ambiguity when the same owner/name exists on
// multiple hosts.
func (s *Syncer) syncMRWithHost(
	ctx context.Context,
	owner, name string,
	number int,
	hostHint string,
) error {
	var (
		repo RepoRef
		ok   bool
		err  error
	)
	if hostHint == "" {
		repo, ok, err = s.trackedRepo(owner, name)
	} else {
		repo, ok, err = s.trackedRepoOnHostUnique(owner, name, hostHint)
	}
	if err != nil {
		return err
	}
	if !ok {
		host := hostHint
		if host == "" {
			host = s.hostFor(owner, name)
		}
		return fmt.Errorf(
			"repo %s/%s on %s is not tracked", owner, name, host,
		)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	return s.syncMRForRepo(ctx, repo, number, false, nil)
}

func (s *Syncer) syncMRWithWatchedRef(
	ctx context.Context,
	mr WatchedMR,
) error {
	return s.syncMRWithWatchedRefTracking(ctx, mr, nil)
}

func (s *Syncer) syncMRWithWatchedRefTracking(
	ctx context.Context,
	mr WatchedMR,
	providerAttempted *bool,
) error {
	kind := watchedMRPlatform(mr)
	repo, ok := s.trackedRepoByIdentity(
		kind, mr.Owner, mr.Name, mr.PlatformHost,
	)
	if !ok {
		host := repoHost(RepoRef{Platform: kind, PlatformHost: mr.PlatformHost})
		return fmt.Errorf(
			"repo %s/%s on %s/%s is not tracked",
			mr.Owner, mr.Name, kind, host,
		)
	}
	return s.syncMRForRepo(ctx, repo, mr.Number, true, providerAttempted)
}

func (s *Syncer) syncMRForRepo(
	ctx context.Context,
	repo RepoRef,
	number int,
	useConditionalPRDetail bool,
	providerAttempted *bool,
) error {
	return s.syncMRForRepoResolved(
		ctx, repo, number, useConditionalPRDetail, providerAttempted, nil, nil, nil, nil,
	)
}

// removedUpstreamForLiveSync fences delayed live work against an archive
// tombstone created after the work was queued. Archive hydration deliberately
// bypasses this check because it must fetch terminal items to repair them when
// they reappear upstream.
func (s *Syncer) removedUpstreamForLiveSync(
	ctx context.Context,
	repoID int64,
	itemType db.ArchiveItemType,
	number int,
) (bool, error) {
	if IsArchiveSyncBudgetContext(ctx) {
		return false, nil
	}
	return s.db.IsArchiveItemRemovedUpstream(ctx, repoID, itemType, number)
}

type mergeRequestFetchEvidence struct {
	merged         bool
	headSHA        string
	mergeCommitSHA string
	filesChanged   *int
}

func (s *Syncer) syncMRForRepoResolved(
	ctx context.Context,
	repo RepoRef,
	number int,
	useConditionalPRDetail bool,
	providerAttempted *bool,
	resolvedRepoID *int64,
	fetchedEvidence *mergeRequestFetchEvidence,
	lifecyclePersisted *bool,
	expectedRepoID *int64,
) error {
	if resolvedRepoID != nil {
		*resolvedRepoID = 0
	}
	if fetchedEvidence != nil {
		*fetchedEvidence = mergeRequestFetchEvidence{}
	}
	if lifecyclePersisted != nil {
		*lifecyclePersisted = false
	}
	if !IsArchiveSyncBudgetContext(ctx) {
		bucket, err := s.bucketKeyForRepo(repo, false)
		if err != nil {
			return fmt.Errorf("resolve detail credential route for %s/%s: %w", repo.Owner, repo.Name, err)
		}
		releaseProviderWork := s.beginProviderWork(bucket, archive.PriorityActiveDetail)
		defer releaseProviderWork()
	}

	owner := repo.Owner
	name := repo.Name
	mrReader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return fmt.Errorf("resolve merge request reader for %s/%s: %w", owner, name, err)
	}

	resolvedRef, repoID, routeFence, found, err := s.reconcileRepoForDirectSync(ctx, repo)
	if err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		return err
	}
	if !found {
		return nil
	}
	if expectedRepoID != nil && repoID != *expectedRepoID {
		return nil
	}
	if resolvedRepoID != nil {
		*resolvedRepoID = repoID
	}
	repo = resolvedRef
	if repo.Archived && !IsArchiveSyncBudgetContext(ctx) {
		// Live detail syncing stops on an archived repo; only archive
		// hydration, which runs under the archive budget, proceeds.
		slog.Debug("skipping MR detail sync for archived repo",
			"repo", owner+"/"+name, "number", number,
		)
		return nil
	}
	removed, err := s.removedUpstreamForLiveSync(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return fmt.Errorf("check MR #%d visibility: %w", number, err)
	}
	if removed {
		return nil
	}
	ctx = withCloneRepositoryIdentity(ctx, repo)

	// Preserve derived fields that provider detail doesn't populate. CI is
	// refreshed later in this sync path; keeping the previous values here
	// prevents detail reads from briefly seeing "no CI" during refresh.
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("get existing MR #%d: %w", number, err)
	}

	var ghPR *gh.PullRequest
	var platformMR platform.MergeRequest
	var normalized *db.MergeRequest
	var newETag string
	if rawReader, ok := mrReader.(interface {
		GetGitHubPullRequest(context.Context, platform.RepoRef, int) (*gh.PullRequest, platform.MergeRequest, error)
	}); ok {
		if client, ok := s.optionalGitHubClientFor(repo); ok && useConditionalPRDetail {
			var notModified bool
			if providerAttempted != nil {
				*providerAttempted = true
			}
			ghPR, newETag, notModified, err = s.getPullRequestForDetail(
				ctx, client, repo, number,
			)
			// Same canonical lookup classification as the raw
			// GetGitHubPullRequest branch below.
			if provider, ok := mrReader.(*gitHubClientProvider); ok {
				if outcomeErr := provider.mergeRequestLookupOutcomeError(
					ctx, platformRepoRef(repo), number, ghPR, err,
				); outcomeErr != nil {
					err = outcomeErr
				}
			}
			if err == nil && ghPR == nil {
				if notModified && existing != nil {
					_, err := s.markUnchangedMRDetailFetched(
						ctx, repo, repoID, number, existing, routeFence, 1,
					)
					return err
				}
				err = fmt.Errorf("client returned nil pull request")
			}
			if err == nil {
				normalized, err = NormalizePR(repoID, ghPR)
			}
		} else {
			if providerAttempted != nil {
				*providerAttempted = true
			}
			ghPR, platformMR, err = rawReader.GetGitHubPullRequest(ctx, platformRepoRef(repo), number)
			if err == nil {
				normalized = platform.DBMergeRequest(repoID, platformMR)
			}
		}
	} else {
		if providerAttempted != nil {
			*providerAttempted = true
		}
		platformMR, err = mrReader.GetMergeRequest(ctx, platformRepoRef(repo), number)
		if err == nil {
			normalized = platform.DBMergeRequest(repoID, platformMR)
		}
	}
	if err != nil {
		if errors.Is(err, ErrNilPullRequest) {
			return fmt.Errorf(
				"get MR %s/%s#%d: client returned nil pull request",
				owner, name, number,
			)
		}
		return fmt.Errorf("get MR %s/%s#%d: %w", owner, name, number, err)
	}
	if normalized == nil {
		return fmt.Errorf("get MR %s/%s#%d: provider returned no merge request", owner, name, number)
	}
	if fetchedEvidence != nil {
		*fetchedEvidence = mergeRequestFetchEvidence{
			merged:         normalized.State == db.MergeRequestStateMerged || normalized.MergedAt != nil,
			headSHA:        normalized.PlatformHeadSHA,
			mergeCommitSHA: normalized.MergeCommitSHA,
			filesChanged:   normalized.FilesChanged,
		}
	}
	headChanged := existing != nil &&
		existing.PlatformHeadSHA != normalized.PlatformHeadSHA
	if existing != nil {
		normalized.CommentCount = existing.CommentCount
		normalized.ReviewDecision = existing.ReviewDecision
		preserveMergeableStateIfOmitted(normalized, existing)
		preserveMergedAtIfOmitted(normalized, existing)
		// CI is tied to the head SHA. If the head moved we must clear the
		// previous values; otherwise a failed CI refresh would leave stale
		// checks attached to the new commit.
		if !headChanged {
			normalized.CIStatus = existing.CIStatus
			normalized.CIChecksJSON = existing.CIChecksJSON
			normalized.CIHadPending = existing.CIHadPending
		}
		normalized.DetailFetchedAt = existing.DetailFetchedAt
		if normalized.AuthorDisplayName == "" {
			normalized.AuthorDisplayName = existing.AuthorDisplayName
		}
	}

	if normalized.Author != "" && normalized.AuthorDisplayName == "" {
		// Resolve directly instead of using s.resolveDisplayName to
		// preserve existing display names on failure.
		if client, ok := s.optionalGitHubClientFor(repo); ok {
			if displayName, found := s.resolveDisplayName(ctx, client, repo, normalized.Author); found {
				normalized.AuthorDisplayName = displayName
			}
		}
		if normalized.AuthorDisplayName == "" && existing != nil {
			normalized.AuthorDisplayName = existing.AuthorDisplayName
		}
	}

	mrID, revision, accepted, err := s.commitMergeRequestParentSnapshotIfRouteFence(
		ctx, repo, normalized, routeFence,
	)
	if err != nil {
		return fmt.Errorf("upsert MR #%d: %w", number, err)
	}
	if !accepted {
		if ghPR != nil && pullRequestWasMerged(ghPR) {
			abandonRepair := func() {
				if resolvedRepoID != nil {
					*resolvedRepoID = 0
				}
			}
			repairCtx := s.db.WithRepositoryRouteFence(
				ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
			)
			repairCtx, releaseRepair, lockErr :=
				s.db.LockRepositoryReconciliationReadForWrite(repairCtx)
			if errors.Is(lockErr, db.ErrRepositoryRouteFenceChanged) {
				abandonRepair()
				return nil
			}
			if lockErr != nil {
				return fmt.Errorf("lock merged MR #%d repair: %w", number, lockErr)
			}
			defer releaseRepair()
			_, repairErr := s.db.FillMissingMergedMRMetrics(
				repairCtx,
				db.MergeRequestMergeMetrics{
					RepoID: repoID, Number: number,
					HeadSHA:        normalized.PlatformHeadSHA,
					MergeCommitSHA: &normalized.MergeCommitSHA,
					FilesChanged:   normalized.FilesChanged,
					MergedAt:       normalized.MergedAt,
				},
			)
			if errors.Is(repairErr, db.ErrRepositoryRouteFenceChanged) {
				abandonRepair()
				return nil
			}
			if repairErr != nil {
				return fmt.Errorf("repair merged MR #%d metrics: %w", number, repairErr)
			}
			if s.afterMergedMRMetricsRepair != nil {
				s.afterMergedMRMetricsRepair()
			}
			current, currentErr := s.db.GetMergeRequestByRepoIDAndNumber(
				repairCtx, repoID, number,
			)
			if currentErr != nil {
				return fmt.Errorf("read merged MR #%d after repair: %w", number, currentErr)
			}
			var currentMergedAt *time.Time
			if current != nil {
				currentMergedAt = current.MergedAt
			}
			if _, actorErr := s.persistMergedTransitionEvent(
				repairCtx, mrID, revision, ghPR, currentMergedAt,
			); errors.Is(actorErr, db.ErrRepositoryRouteFenceChanged) {
				abandonRepair()
				return nil
			} else if actorErr != nil {
				return fmt.Errorf("repair merged MR #%d actor: %w", number, actorErr)
			}
			if lifecyclePersisted != nil {
				*lifecyclePersisted = true
			}
		}
		return nil
	}
	ctx = s.db.WithRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
	)
	if err := s.markClosedLinkedPRNotificationsDone(ctx, repoID, number); err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		return err
	}
	// UpsertMergeRequest preserves ci_had_pending across upserts. Clear
	// it here when the head SHA changed so a stale pending flag from
	// the previous head doesn't survive across the refresh.
	if headChanged {
		ciCleared, err := s.db.ClearMRCISnapshot(
			ctx, mrID, revision, normalized.PlatformHeadSHA,
		)
		if err != nil {
			return fmt.Errorf("clear stale CI for MR #%d: %w", number, err)
		}
		if !ciCleared {
			return nil
		}
	}
	if err := s.db.EnsureKanbanState(ctx, mrID); err != nil {
		return fmt.Errorf("ensure kanban state for MR #%d: %w", number, err)
	}

	var diffErr error
	if ghPR != nil {
		// Run the diff sync, but don't let its failure abort the rest of SyncMR:
		// timeline and CI status are independent and the user still wants them
		// fresh. Capture the error and surface it via DiffSyncError at the end.
		diffErr = s.syncMRDiff(
			ctx, repo, repoID, mrID, revision, number,
			ghPR, normalized, routeFence,
		)
		if errors.Is(diffErr, errParentSnapshotAdvanced) {
			return nil
		}

		if err := s.refreshTimeline(
			ctx, repo, mrID, revision, ghPR,
			livenessHeadForRound(normalized, existing),
		); err != nil {
			if errors.Is(err, errParentSnapshotAdvanced) {
				return nil
			}
			return fmt.Errorf("refresh timeline for MR #%d: %w", number, err)
		}
		if _, err := s.persistMergedTransitionEvent(ctx, mrID, revision, ghPR, normalized.MergedAt); err != nil {
			return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
		}
		if lifecyclePersisted != nil {
			*lifecyclePersisted = true
		}

		syncMRHeadSHA := ""
		if ghPR.GetHead() != nil {
			syncMRHeadSHA = ghPR.GetHead().GetSHA()
		}
		ciApplied, err := s.refreshCIStatusSnapshot(
			ctx, repo, mrID, revision, number, syncMRHeadSHA,
		)
		if err != nil {
			return err
		}
		if !ciApplied {
			return nil
		}

		// Refresh workflow approval state for the current head SHA.
		// Persisting it here (instead of computing live on every GET)
		// means the DB-only detail path the frontend uses by default
		// can show the Approve Workflows button without a foreground
		// sync round-trip. The result is tied to syncMRHeadSHA so a
		// later read can detect a stale snapshot after force-push.
		approvalApplied, _ := s.refreshWorkflowApproval(
			ctx, repo, repoID, number, syncMRHeadSHA, ghPR, normalized,
		)
		if !approvalApplied {
			return nil
		}

		// Update ci_had_pending after refreshing CI status.
		fresh, freshErr := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		if freshErr == nil && fresh != nil {
			pending := ciHasPending(fresh.CIChecksJSON)
			detailApplied, detailErr := s.markMergeRequestDetailFetchedIfRouteFence(
				ctx, repo, routeFence, mrID, revision, pending, nil,
			)
			if detailErr != nil {
				return fmt.Errorf("mark detail fetched for MR #%d: %w", number, detailErr)
			}
			if !detailApplied {
				return nil
			}
		}
	} else {
		// Record the reviewed diff snapshot for non-GitHub providers
		// too: head-binding providers refuse merge/approve until
		// DiffHeadSHA matches the platform head, so a sync that never
		// writes it would leave head-bound actions permanently
		// disabled with 409 head_unknown.
		diffErr = s.syncProviderMRDiff(
			ctx, repo, repoID, mrID, revision, number,
			normalized, true, routeFence,
		)
		if errors.Is(diffErr, errParentSnapshotAdvanced) {
			return nil
		}

		pending := false
		_, pending, err = s.syncProviderMRDetailExtras(
			ctx, mrReader, repo, mrID, number, revision, normalized.PlatformHeadSHA,
			livenessHeadForRound(normalized, existing),
		)
		if err != nil {
			if errors.Is(err, errParentSnapshotAdvanced) {
				return nil
			}
			return err
		}
		if _, err := s.persistMergedActorEvent(ctx, mrID, revision, platformMR.MergedBy, normalized.MergedAt); err != nil {
			return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
		}
		if lifecyclePersisted != nil {
			*lifecyclePersisted = true
		}
		detailApplied, err := s.markMergeRequestDetailFetchedIfRouteFence(
			ctx, repo, routeFence, mrID, revision, pending, nil,
		)
		if err != nil {
			return fmt.Errorf("mark detail fetched for MR #%d: %w", number, err)
		}
		if !detailApplied {
			return nil
		}
	}

	if s.onMRSynced != nil {
		fresh, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		if err != nil {
			slog.Warn("get MR for onMRSynced hook in SyncMR",
				"repo", owner+"/"+name,
				"number", number,
				"err", err,
			)
		} else {
			s.onMRSynced(owner, name, fresh)
		}
	}

	if err := s.markClosedLinkedPRNotificationsDone(ctx, repoID, number); err != nil {
		if diffErr != nil {
			return errors.Join(diffErr, err)
		}
		return err
	}
	if diffErr != nil {
		return diffErr
	}
	if newETag != "" {
		if _, err := s.db.UpsertHTTPEtagIfRouteFence(
			ctx, platform.DBRepoIdentity(platformRepoRef(repo)), routeFence,
			"pull_request", number, newETag,
		); err != nil {
			slog.Warn("persist pull request ETag failed",
				"repo", repo.Owner+"/"+repo.Name,
				"number", number,
				"err", err,
			)
		}
	}
	return nil
}

// preservePlatformBaseSHAIfOmitted keeps the stored base SHA when the
// list endpoint omits it but the head is unchanged. GitLab list
// payloads carry no diff_refs, so a bare list-driven upsert would
// blank the base SHA that a prior detail sync recorded — invalidating
// the reviewed diff snapshot (diff_base_sha != platform_base_sha) and
// dropping reviewed_head_sha back to 409 head_unknown.
func preservePlatformBaseSHAIfOmitted(
	normalized *db.MergeRequest,
	existing *db.MergeRequest,
) {
	if normalized == nil || existing == nil {
		return
	}
	if normalized.PlatformBaseSHA != "" || existing.PlatformBaseSHA == "" {
		return
	}
	if normalized.PlatformHeadSHA == "" ||
		existing.PlatformHeadSHA == "" ||
		normalized.PlatformHeadSHA != existing.PlatformHeadSHA {
		return
	}
	normalized.PlatformBaseSHA = existing.PlatformBaseSHA
}

func preserveMergeableStateIfOmitted(
	normalized *db.MergeRequest,
	existing *db.MergeRequest,
) {
	if normalized == nil || existing == nil {
		return
	}
	if normalized.PlatformHeadSHA == "" ||
		existing.PlatformHeadSHA == "" ||
		normalized.PlatformHeadSHA != existing.PlatformHeadSHA {
		return
	}
	if normalized.PlatformBaseSHA == "" ||
		existing.PlatformBaseSHA == "" ||
		normalized.PlatformBaseSHA != existing.PlatformBaseSHA {
		return
	}
	if normalized.MergeableState == "" ||
		(normalized.MergeableState == "unknown" && existing.MergeableState != "") {
		normalized.MergeableState = existing.MergeableState
	}
}

func preserveMergedAtIfOmitted(
	normalized *db.MergeRequest,
	existing *db.MergeRequest,
) {
	if normalized.State == db.MergeRequestStateMerged &&
		normalized.MergedAt == nil && existing.MergedAt != nil {
		mergedAt := *existing.MergedAt
		normalized.MergedAt = &mergedAt
	}
}

func preserveReviewDecisionIfOmitted(
	normalized *db.MergeRequest,
	existing *db.MergeRequest,
) {
	if normalized == nil || existing == nil {
		return
	}
	if normalized.ReviewDecision != "" || existing.ReviewDecision == "" {
		return
	}
	if normalized.PlatformHeadSHA != "" &&
		existing.PlatformHeadSHA != "" &&
		normalized.PlatformHeadSHA != existing.PlatformHeadSHA {
		return
	}
	normalized.ReviewDecision = existing.ReviewDecision
}

func preserveCIStateIfOmitted(
	normalized *db.MergeRequest,
	existing *db.MergeRequest,
) bool {
	if normalized == nil || existing == nil {
		return false
	}
	if normalized.PlatformHeadSHA == "" ||
		existing.PlatformHeadSHA == "" ||
		normalized.PlatformHeadSHA != existing.PlatformHeadSHA {
		return false
	}
	ciStatusOmitted := normalized.CIStatus == ""
	ciStatusChanged := !ciStatusOmitted &&
		normalized.CIStatus != existing.CIStatus
	if normalized.CIStatus == "" {
		normalized.CIStatus = existing.CIStatus
	}
	if normalized.CIChecksJSON == "" && !ciStatusChanged {
		normalized.CIChecksJSON = existing.CIChecksJSON
	}
	return ciStatusChanged && normalized.CIChecksJSON == ""
}

// syncMRDiff fetches the bare clone and computes diff SHAs for a single PR.
// Returns nil when there is no clone manager (the caller has already opted
// out of diff support); otherwise returns an error wrapping a
// *DiffSyncError that describes the first failure encountered along the
// clone or diff path. Callers can recover the structured categorization via
// errors.As.
func (s *Syncer) syncMRDiff(
	ctx context.Context, repo RepoRef, repoID, mrID, expectedRevision int64, number int,
	ghPR *gh.PullRequest, normalized *db.MergeRequest,
	routeFence db.RepositoryRouteFence,
) error {
	ctx = withCloneRepositoryIdentity(ctx, repo)
	if s.clones == nil {
		return nil
	}
	host := repoHost(repo)
	if err := s.ensureCloneForRoute(ctx, repo, repoID, routeFence); err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeCloneUnavailable,
			Err:  fmt.Errorf("ensure bare clone for #%d: %w", number, err),
		}
	}

	if ghPR.GetMerged() {
		// Merged MRs need special merge-base logic via the pull ref.
		// Force recomputation to repair any previously incorrect SHAs.
		return s.computeMergedMRDiffSHAs(
			ctx, repo, repoID, mrID, expectedRevision, number,
			normalized.PlatformHeadSHA, normalized.PlatformBaseSHA,
			ghPR.GetMergeCommitSHA(), true,
		)
	}

	if normalized.PlatformHeadSHA == "" || normalized.PlatformBaseSHA == "" {
		return nil
	}
	mb, err := s.clones.MergeBase(ctx, string(repoPlatform(repo)), host, repo.Owner, repo.Name, normalized.PlatformBaseSHA, normalized.PlatformHeadSHA)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeMergeBaseFailed,
			Err:  fmt.Errorf("merge-base for #%d: %w", number, err),
		}
	}
	applied, err := s.db.UpdateDiffSHAsSnapshot(
		ctx, mrID, expectedRevision,
		normalized.PlatformHeadSHA, normalized.PlatformBaseSHA,
		normalized.PlatformHeadSHA, normalized.PlatformBaseSHA, mb,
	)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeInternal,
			Err:  fmt.Errorf("update diff SHAs for #%d: %w", number, err),
		}
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	return nil
}

// syncProviderMRDiff records the reviewed diff snapshot for a
// non-GitHub MR: the locally verified head/base SHAs plus merge-base.
// Head-binding providers gate merge/approve on this snapshot matching
// the platform head, so it must be written by the same sync that
// refreshed the MR row. Only open MRs are snapshotted — head pins are
// meaningless once merged/closed, and the merged-MR merge-base repair
// logic is GitHub-specific.
func (s *Syncer) syncProviderMRDiff(
	ctx context.Context, repo RepoRef, repoID, mrID, expectedRevision int64, number int,
	normalized *db.MergeRequest, ensureClone bool,
	routeFence db.RepositoryRouteFence,
) error {
	ctx = withCloneRepositoryIdentity(ctx, repo)
	if s.clones == nil {
		return nil
	}
	if normalized.State != db.MergeRequestStateOpen {
		return nil
	}
	if normalized.PlatformHeadSHA == "" || normalized.PlatformBaseSHA == "" {
		return nil
	}
	host := repoHost(repo)
	// List sync already fetched the repo clone once for this cycle;
	// refetching per MR would turn one repo sync into N network
	// round-trips. Per-MR detail syncs have no prior fetch and pass
	// ensureClone.
	if ensureClone {
		if err := s.ensureCloneForRoute(ctx, repo, repoID, routeFence); err != nil {
			return &DiffSyncError{
				Code: DiffSyncCodeCloneUnavailable,
				Err:  fmt.Errorf("ensure bare clone for #%d: %w", number, err),
			}
		}
	}
	mb, err := s.clones.MergeBase(ctx, string(repoPlatform(repo)), host, repo.Owner, repo.Name, normalized.PlatformBaseSHA, normalized.PlatformHeadSHA)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeMergeBaseFailed,
			Err:  fmt.Errorf("merge-base for #%d: %w", number, err),
		}
	}
	applied, err := s.db.UpdateDiffSHAsSnapshot(
		ctx, mrID, expectedRevision,
		normalized.PlatformHeadSHA, normalized.PlatformBaseSHA,
		normalized.PlatformHeadSHA, normalized.PlatformBaseSHA, mb,
	)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeInternal,
			Err:  fmt.Errorf("update diff SHAs for #%d: %w", number, err),
		}
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	return nil
}

// SyncIssue fetches fresh data for a single issue from GitHub and updates the DB.
// Returns an error if the repo is not in the configured repo list.
func (s *Syncer) SyncIssue(ctx context.Context, owner, name string, number int) error {
	return s.syncIssueWithHost(ctx, owner, name, number, "")
}

// SyncIssueOnHost fetches fresh issue data for a specific tracked host.
func (s *Syncer) SyncIssueOnHost(
	ctx context.Context,
	host, owner, name string,
	number int,
) error {
	return s.syncIssueWithHost(ctx, owner, name, number, host)
}

// SyncIssueOnProvider fetches fresh issue data for a specific configured
// provider host.
func (s *Syncer) SyncIssueOnProvider(
	ctx context.Context,
	kind platform.Kind,
	host, owner, name string,
	number int,
) error {
	repo, ok := s.trackedRepoByIdentity(kind, owner, name, host)
	if !ok {
		host = repoHost(RepoRef{Platform: kind, PlatformHost: host})
		return fmt.Errorf(
			"repo %s/%s on %s/%s is not tracked",
			owner, name, kind, host,
		)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	return s.syncIssueForRepo(ctx, repo, number, nil)
}

func (s *Syncer) syncIssueWithHost(
	ctx context.Context,
	owner, name string,
	number int,
	hostHint string,
) error {
	var (
		repo RepoRef
		ok   bool
		err  error
	)
	if hostHint == "" {
		repo, ok, err = s.trackedRepo(owner, name)
	} else {
		repo, ok, err = s.trackedRepoOnHostUnique(owner, name, hostHint)
	}
	if err != nil {
		return err
	}
	if !ok {
		host := hostHint
		if host == "" {
			host = s.hostFor(owner, name)
		}
		return fmt.Errorf(
			"repo %s/%s on %s is not tracked", owner, name, host,
		)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	return s.syncIssueForRepo(ctx, repo, number, nil)
}

func (s *Syncer) syncIssueForRepo(
	ctx context.Context,
	repo RepoRef,
	number int,
	providerAttempted *bool,
) error {
	if !IsArchiveSyncBudgetContext(ctx) {
		bucket, err := s.bucketKeyForRepo(repo, false)
		if err != nil {
			return fmt.Errorf("resolve issue credential route for %s/%s: %w", repo.Owner, repo.Name, err)
		}
		releaseProviderWork := s.beginProviderWork(bucket, archive.PriorityActiveDetail)
		defer releaseProviderWork()
	}

	resolvedRef, repoID, _, found, err := s.reconcileRepoForDirectSync(ctx, repo)
	if err != nil {
		if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
			return nil
		}
		return err
	}
	if !found {
		return nil
	}
	repo = resolvedRef
	if repo.Archived && !IsArchiveSyncBudgetContext(ctx) {
		// Live detail syncing stops on an archived repo; only archive
		// hydration, which runs under the archive budget, proceeds.
		slog.Debug("skipping issue detail sync for archived repo",
			"repo", repo.Owner+"/"+repo.Name, "number", number,
		)
		return nil
	}

	providerCalls, err := s.fetchIssueDetail(ctx, repo, repoID, number)
	if providerAttempted != nil && providerCalls > 0 {
		*providerAttempted = true
	}
	if err != nil {
		return err
	}
	return s.markClosedLinkedIssueNotificationsDone(ctx, repoID, number)
}

// ArchiveItemSyncCost returns the provider-aware admission estimate for the
// existing full item sync. Gitealike merge-request hydration may exceed this
// estimate after admission because its complete dataset is committed atomically.
func (s *Syncer) ArchiveItemSyncCost(kind platform.Kind, itemType db.ArchiveItemType) int {
	if itemType == db.ArchiveItemTypeMergeRequest {
		return detailWorstCaseAttemptCost(kind, QueueItemPR)
	}
	return detailWorstCaseAttemptCost(kind, QueueItemIssue)
}

// SyncArchiveItem runs archive hydration through the canonical live item
// sync. The caller has already acquired archive admission, so the archive
// budget context also prevents the live entry point from taking a nested
// provider-work lease.
func (s *Syncer) SyncArchiveItem(
	ctx context.Context,
	ref platform.RepoRef,
	itemType db.ArchiveItemType,
	number int,
) (archive.ItemSyncResult, error) {
	result := archive.ItemSyncResult{}
	repo, ok := s.trackedRepoByIdentity(ref.Platform, ref.Owner, ref.Name, ref.Host)
	if !ok {
		return result, fmt.Errorf(
			"repo %s/%s on %s/%s is not tracked",
			ref.Owner, ref.Name, ref.Platform, ref.Host,
		)
	}
	repo.Owner = ref.Owner
	repo.Name = ref.Name
	repo.PlatformHost = repoHost(repo)
	switch itemType {
	case db.ArchiveItemTypeIssue:
		providerAttempted := false
		err := s.syncIssueForRepo(ctx, repo, number, &providerAttempted)
		result.ProviderAttempted = providerAttempted
		return result, err
	case db.ArchiveItemTypeMergeRequest:
		providerAttempted := false
		var resolvedRepoID int64
		var fetchedEvidence mergeRequestFetchEvidence
		var lifecyclePersisted bool
		err := s.syncMRForRepoResolved(
			ctx, repo, number, false, &providerAttempted, &resolvedRepoID, &fetchedEvidence,
			&lifecyclePersisted, nil,
		)
		if _, onlyDiffFailed := err.(*DiffSyncError); onlyDiffFailed { //nolint:errorlint // joined hard failures must propagate
			err = nil
		}
		if err == nil && repoPlatform(repo) == platform.KindGitHub {
			err = s.requireGitHubArchiveMergedMRMetrics(
				ctx, resolvedRepoID, number, fetchedEvidence, lifecyclePersisted,
			)
			if err == nil {
				var filesChanged *int
				if fetchedEvidence.filesChanged != nil {
					value := *fetchedEvidence.filesChanged
					filesChanged = &value
				}
				result.MergeRequestEvidence = &db.ArchiveMergeRequestEvidence{
					Merged: fetchedEvidence.merged, HeadSHA: fetchedEvidence.headSHA,
					MergeCommitSHA: fetchedEvidence.mergeCommitSHA,
					FilesChanged:   filesChanged,
				}
			}
		}
		result.ProviderAttempted = providerAttempted
		return result, err
	default:
		return result, fmt.Errorf("sync archive item: invalid item type %q", itemType)
	}
}

// FinalizeArchiveItemSync applies persistence that depends on the archive
// lifecycle commit. A concurrent terminal observation can land after the item
// is claimed, so parent-snapshot reclassification may still see the item as
// removed. The successful archive commit reactivates it before this callback.
func (s *Syncer) FinalizeArchiveItemSync(
	ctx context.Context,
	repoID int64,
	itemType db.ArchiveItemType,
	number int,
) {
	if itemType != db.ArchiveItemTypeMergeRequest {
		return
	}
	stored, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		slog.Error("look up repository for archive item finalization failed",
			"repo_id", repoID, "number", number, "err", err,
		)
		return
	}
	if stored == nil {
		return
	}
	s.reclassifyWorkspaceHeadRepoTrust(ctx, RepoRef{
		Platform: platform.Kind(stored.Platform), PlatformHost: stored.PlatformHost,
		Owner: stored.Owner, Name: stored.Name, RepoPath: stored.RepoPath,
		PlatformExternalID: stored.PlatformRepoID,
	}, repoID, number)
}

func (s *Syncer) requireGitHubArchiveMergedMRMetrics(
	ctx context.Context,
	repoID int64,
	number int,
	fetched mergeRequestFetchEvidence,
	lifecyclePersisted bool,
) error {
	if repoID == 0 {
		return fmt.Errorf("verify GitHub archive MR #%d: repository was not resolved", number)
	}
	storedRepo, err := s.db.GetRepoByID(ctx, repoID)
	if err != nil {
		return fmt.Errorf("verify GitHub archive MR #%d repository: %w", number, err)
	}
	if storedRepo == nil {
		return fmt.Errorf("verify GitHub archive MR #%d: repository is not stored", number)
	}
	if fetched.merged && !lifecyclePersisted {
		return fmt.Errorf(
			"verify GitHub archive MR %s/%s#%d: lifecycle persistence incomplete",
			storedRepo.Owner, storedRepo.Name, number,
		)
	}
	mr, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return fmt.Errorf("verify GitHub archive MR #%d metrics: %w", number, err)
	}
	if mr == nil {
		return fmt.Errorf("verify GitHub archive MR #%d metrics: pull request is not stored", number)
	}
	storedMerged := mr.State == db.MergeRequestStateMerged || mr.MergedAt != nil
	if fetched.merged != storedMerged {
		return fmt.Errorf(
			"verify GitHub archive MR %s/%s#%d metrics: incomplete or mismatched merge_state",
			storedRepo.Owner, storedRepo.Name, number,
		)
	}
	if !fetched.merged {
		return nil
	}
	incomplete := make([]string, 0, 4)
	if mr.MergedAt == nil {
		incomplete = append(incomplete, "merged_at")
	}
	if fetched.merged {
		if fetched.headSHA == "" || mr.PlatformHeadSHA != fetched.headSHA {
			incomplete = append(incomplete, "platform_head_sha")
		}
		if fetched.mergeCommitSHA == "" || mr.MergeCommitSHA != fetched.mergeCommitSHA {
			incomplete = append(incomplete, "merge_commit_sha")
		}
		if fetched.filesChanged == nil || mr.FilesChanged == nil ||
			*mr.FilesChanged != *fetched.filesChanged {
			incomplete = append(incomplete, "files_changed")
		}
	} else {
		if mr.MergeCommitSHA == "" {
			incomplete = append(incomplete, "merge_commit_sha")
		}
		if mr.FilesChanged == nil {
			incomplete = append(incomplete, "files_changed")
		}
	}
	if len(incomplete) > 0 {
		return fmt.Errorf(
			"verify GitHub archive MR %s/%s#%d metrics: incomplete or mismatched %s",
			storedRepo.Owner, storedRepo.Name, number, strings.Join(incomplete, ", "),
		)
	}
	return nil
}

// SyncItemByNumber fetches an item by number from GitHub, determines
// whether it is a PR or issue, syncs it into the DB, and returns the
// item type ("pr" or "issue").
// Returns an error if the repo is not in the configured repo list.
func (s *Syncer) SyncItemByNumber(
	ctx context.Context, owner, name string, number int,
) (string, error) {
	repo, ok, err := s.trackedRepo(owner, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("repo %s/%s is not tracked", owner, name)
	}
	repo.Owner = owner
	repo.Name = name
	repo.PlatformHost = repoHost(repo)
	bucket, err := s.bucketKeyForRepo(repo, false)
	if err != nil {
		return "", fmt.Errorf("resolve item credential route for %s/%s: %w", owner, name, err)
	}
	releaseProviderWork := s.beginProviderWork(bucket, archive.PriorityActiveDetail)
	defer releaseProviderWork()

	if repoPlatform(repo) != platform.KindGitHub {
		return "", fmt.Errorf(
			"sync item by number for %s/%s on %s/%s requires an item type",
			owner, name, repoPlatform(repo), repo.PlatformHost,
		)
	}

	// GitHub's Issues API returns both issues and PRs. If the
	// response has PullRequestLinks, it's a PR.
	client, err := s.clientFor(repo)
	if err != nil {
		return "", fmt.Errorf("resolve client for %s/%s: %w", owner, name, err)
	}
	ghIssue, err := client.GetIssue(ctx, owner, name, number)
	// Route fetch failures and detected transfers through the canonical
	// lookup classification so removed, inaccessible, and moved items
	// surface typed outcomes instead of generic upstream failures.
	if outcomeErr := s.issueFetchOutcomeError(ctx, repo, number, ghIssue, err); outcomeErr != nil {
		return "", fmt.Errorf(
			"get item %s/%s#%d: %w", owner, name, number, outcomeErr,
		)
	}
	if err != nil {
		return "", fmt.Errorf(
			"get item %s/%s#%d: %w", owner, name, number, err,
		)
	}
	if ghIssue == nil {
		return "", fmt.Errorf(
			"get item %s/%s#%d: client returned nil issue", owner, name, number,
		)
	}

	if ghIssue.PullRequestLinks != nil {
		if err := s.SyncMR(ctx, owner, name, number); err != nil {
			// A DiffSyncError means the PR row, timeline, and CI status
			// were upserted successfully and only the diff computation
			// failed. The item type is known, so resolution can still
			// succeed; surface the error so callers that care about diff
			// freshness can react, but report itemType so callers that
			// just need to route the user (e.g. /items/{n}/resolve) can
			// proceed.
			if _, ok := errors.AsType[*DiffSyncError](err); ok {
				return "pr", err
			}
			return "", fmt.Errorf(
				"sync MR %s/%s#%d: %w", owner, name, number, err,
			)
		}
		return "pr", nil
	}

	if err := s.SyncIssue(ctx, owner, name, number); err != nil {
		return "", fmt.Errorf(
			"sync issue %s/%s#%d: %w", owner, name, number, err,
		)
	}
	return "issue", nil
}

// CarryMergeRequestDerivedFields copies sync-derived columns a provider
// snapshot cannot represent — comment count, review decision, and CI state —
// from the stored row onto normalized, so committing a fetched or
// mutation-returned snapshot does not erase them. CI status and checks are
// head-derived and carried only while the head is unchanged (see the
// SHA-sensitive sync rules). The ci_had_pending flag and the detail-fetched
// marker are owned by the snapshot upsert itself (the stored flag always
// wins, and a set marker is never cleared by a snapshot), so this helper
// deliberately leaves them alone.
func CarryMergeRequestDerivedFields(normalized, existing *db.MergeRequest) {
	if normalized == nil || existing == nil {
		return
	}
	normalized.CommentCount = existing.CommentCount
	normalized.ReviewDecision = existing.ReviewDecision
	if strings.EqualFold(normalized.PlatformHeadSHA, existing.PlatformHeadSHA) {
		normalized.CIStatus = existing.CIStatus
		normalized.CIChecksJSON = existing.CIChecksJSON
	}
}

// fetchAndUpdateClosed retrieves the final state of a now-closed PR from GitHub.
func (s *Syncer) fetchAndUpdateClosed(ctx context.Context, repo RepoRef, repoID int64, number int, cloneFetchOK bool) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return fmt.Errorf("check closed PR #%d visibility: %w", number, err)
	}
	if removed {
		return nil
	}
	client, err := s.clientFor(repo)
	if err != nil {
		return fmt.Errorf("resolve client for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	ghPR, err := client.GetPullRequest(ctx, repo.Owner, repo.Name, number)
	// Route fetch failures and detected transfers through the canonical
	// lookup classification so removed, inaccessible, and moved items
	// surface typed outcomes instead of generic upstream failures.
	if outcomeErr := s.mergeRequestFetchOutcomeError(ctx, repo, number, ghPR, err); outcomeErr != nil {
		return fmt.Errorf("get closed PR #%d: %w", number, outcomeErr)
	}
	if err != nil {
		return fmt.Errorf("get closed PR #%d: %w", number, err)
	}
	if ghPR == nil {
		return fmt.Errorf(
			"get closed PR #%d: client returned nil pull request",
			number,
		)
	}

	normalized, err := NormalizePR(repoID, ghPR)
	if err != nil {
		return fmt.Errorf("normalize closed PR #%d: %w", number, err)
	}
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return fmt.Errorf("get closed MR #%d: %w", number, err)
	}
	CarryMergeRequestDerivedFields(normalized, existing)
	mrID, revision, accepted, err := s.CommitMergeRequestParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf("commit closed MR #%d: %w", number, err)
	}
	if !accepted {
		return nil
	}
	if _, err := s.persistMergedTransitionEvent(ctx, mrID, revision, ghPR, normalized.MergedAt); err != nil {
		return fmt.Errorf("persist merged lifecycle event for MR #%d: %w", number, err)
	}
	if err := s.markClosedLinkedPRNotificationsDone(ctx, repoID, number); err != nil {
		return err
	}

	// Compute diff SHAs so the diff endpoint works.
	// For closed-but-not-merged PRs, use GitHub's head/base SHAs directly.
	// For merged PRs, use merge-base(merge_commit^1, refs/pull/<number>/head)
	// to find the fork point. This works for all merge strategies because ^1
	// is always a pre-merge commit on the base branch lineage, and the pull
	// ref always points to the original PR head. We only do this when no diff
	// SHAs exist yet; PRs synced while open already have valid diff SHAs.
	closedHost := repo.PlatformHost
	if closedHost == "" {
		closedHost = "github.com"
	}
	if s.clones != nil && cloneFetchOK {
		headSHA := ghPR.GetHead().GetSHA()
		baseSHA := ghPR.GetBase().GetSHA()

		if pullRequestWasMerged(ghPR) {
			if err := s.computeMergedMRDiffSHAs(
				ctx, repo, repoID, mrID, revision, number,
				normalized.PlatformHeadSHA, normalized.PlatformBaseSHA,
				ghPR.GetMergeCommitSHA(), false,
			); err != nil {
				if errors.Is(err, errParentSnapshotAdvanced) {
					return nil
				}
				slog.Warn("compute merged PR diff SHAs failed",
					"repo", repo.Owner+"/"+repo.Name,
					"number", number, "err", err,
				)
			}
		} else if headSHA != "" && baseSHA != "" {
			mb, err := s.clones.MergeBase(ctx, string(repoPlatform(repo)), closedHost, repo.Owner, repo.Name, baseSHA, headSHA)
			if err != nil {
				slog.Warn("merge-base for closed PR failed",
					"repo", repo.Owner+"/"+repo.Name,
					"number", number, "err", err,
				)
			} else {
				diffApplied, err := s.db.UpdateDiffSHAsSnapshot(
					ctx, mrID, revision, headSHA, baseSHA,
					headSHA, baseSHA, mb,
				)
				if err != nil {
					slog.Warn("update diff SHAs for closed PR failed",
						"repo", repo.Owner+"/"+repo.Name,
						"number", number, "err", err,
					)
				} else if !diffApplied {
					return nil
				}
			}
		}
	}
	return s.markClosedLinkedPRNotificationsDone(ctx, repoID, number)
}

func (s *Syncer) filterDuplicateMergedLifecycleEvents(
	ctx context.Context,
	mrID int64,
	events []db.MREvent,
) ([]db.MREvent, error) {
	if !slices.ContainsFunc(events, isAuthoredMergedLifecycleEvent) {
		return events, nil
	}
	existing, err := s.db.ListMREvents(ctx, mrID)
	if err != nil {
		return nil, err
	}
	out := events[:0]
	for _, event := range events {
		if isAuthoredMergedLifecycleEvent(event) &&
			authoredMergedLifecycleEventExists(existing) {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func authoredMergedLifecycleEventExists(events []db.MREvent) bool {
	return slices.ContainsFunc(events, isAuthoredMergedLifecycleEvent)
}

func isAuthoredMergedLifecycleEvent(event db.MREvent) bool {
	return event.EventType == "merged" &&
		strings.TrimSpace(event.Author) != ""
}

func (s *Syncer) persistMergedTransitionEvent(
	ctx context.Context,
	mrID int64,
	expectedRevision int64,
	ghPR *gh.PullRequest,
	mergedAt *time.Time,
) (bool, error) {
	if ghPR == nil || mergedAt == nil {
		return false, nil
	}
	mergedBy := ghPR.GetMergedBy()
	if mergedBy == nil {
		return false, nil
	}
	return s.persistMergedActorEvent(ctx, mrID, expectedRevision, mergedBy.GetLogin(), mergedAt)
}

func (s *Syncer) persistMergedActorEvent(
	ctx context.Context,
	mrID int64,
	_ int64,
	actor string,
	mergedAt *time.Time,
) (bool, error) {
	if mergedAt == nil {
		return false, nil
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return false, nil
	}
	event := NormalizeTimelineEvent(mrID, PullRequestTimelineEvent{
		EventType: "merged",
		Actor:     actor,
		CreatedAt: *mergedAt,
	})
	if event == nil {
		return false, nil
	}
	return s.db.UpsertMergedActorEvent(ctx, *event)
}

func (s *Syncer) fetchAndUpdateClosedMergeRequest(
	ctx context.Context,
	reader platform.MergeRequestReader,
	repo RepoRef,
	repoID int64,
	number int,
	cloneFetchOK bool,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return fmt.Errorf("check closed MR #%d visibility: %w", number, err)
	}
	if removed {
		return nil
	}
	if _, ok := reader.(interface {
		GetGitHubPullRequest(context.Context, platform.RepoRef, int) (*gh.PullRequest, platform.MergeRequest, error)
	}); ok {
		return s.fetchAndUpdateClosed(ctx, repo, repoID, number, cloneFetchOK)
	}

	mrReader, err := s.mergeRequestReaderFor(repo)
	if err != nil {
		return fmt.Errorf("resolve merge request reader for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	mr, err := mrReader.GetMergeRequest(ctx, platformRepoRef(repo), number)
	if err != nil {
		return fmt.Errorf("get closed MR #%d: %w", number, err)
	}
	normalized := platform.DBMergeRequest(repoID, mr)
	existing, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
	if err != nil {
		return fmt.Errorf("get stored closed MR #%d: %w", number, err)
	}
	CarryMergeRequestDerivedFields(normalized, existing)
	mrID, revision, accepted, err := s.CommitMergeRequestParentSnapshot(ctx, repo, normalized)
	if err != nil {
		return fmt.Errorf("upsert closed MR #%d: %w", number, err)
	}
	if !accepted {
		return nil
	}
	if _, err := s.persistMergedActorEvent(ctx, mrID, revision, mr.MergedBy, normalized.MergedAt); err != nil {
		return fmt.Errorf("persist merged lifecycle event for closed MR #%d: %w", number, err)
	}
	return nil
}

// computeMergedMRDiffSHAs computes diff SHAs for a merged PR.
// Uses merge-base(merge_commit^1, refs/pull/<number>/head) which works for all
// GitHub merge strategies:
//   - Merge commit: ^1 is the pre-merge base tip
//   - Squash: ^1 is the pre-squash base tip
//   - Rebase: ^1 is the previous rebased commit
//
// In all cases, merge-base with the original PR head (from the pull ref)
// correctly identifies the fork point.
//
// When force is false, skips PRs that already have diff SHAs (periodic sync).
// When force is true, always recomputes (on-demand SyncMR).
//
// Returns a *DiffSyncError (wrapped as an error) describing the failure when
// any git or DB operation fails. A nil return covers both success and the
// no-op skip cases (empty merge SHA, existing valid diff SHAs without force).
func (s *Syncer) computeMergedMRDiffSHAs(
	ctx context.Context, repo RepoRef, repoID, mrID, expectedRevision int64,
	number int, expectedHeadSHA, expectedBaseSHA, mergeCommitSHA string, force bool,
) error {
	if mergeCommitSHA == "" {
		return nil
	}

	if !force {
		existing, err := s.db.GetDiffSHAsByRepoID(ctx, repoID, number)
		if err != nil {
			return &DiffSyncError{
				Code: DiffSyncCodeInternal,
				Err:  fmt.Errorf("get diff SHAs for merged PR #%d: %w", number, err),
			}
		}
		if existing == nil || existing.DiffHeadSHA != "" {
			return nil // already has diff SHAs or PR not found
		}
	}

	// Resolve the PR head from the pull ref. GitHub keeps these refs
	// indefinitely, pointing to the original PR head commit regardless
	// of merge strategy.
	mergedHost := repo.PlatformHost
	if mergedHost == "" {
		mergedHost = "github.com"
	}
	pullRef := fmt.Sprintf("refs/pull/%d/head", number)
	prHead, err := s.clones.RevParse(ctx, string(repoPlatform(repo)), mergedHost, repo.Owner, repo.Name, pullRef)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeCommitUnreachable,
			Err:  fmt.Errorf("rev-parse %s for merged PR #%d: %w", pullRef, number, err),
		}
	}

	// Use the merge commit's first parent as the base for merge-base.
	// This avoids the post-merge ancestor problem where prHead is reachable
	// from the current base branch tip (making merge-base return prHead).
	preMergeBase, err := s.clones.RevParse(ctx, string(repoPlatform(repo)), mergedHost, repo.Owner, repo.Name, mergeCommitSHA+"^1")
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeCommitUnreachable,
			Err:  fmt.Errorf("rev-parse %s^1 for merged PR #%d: %w", mergeCommitSHA, number, err),
		}
	}

	mb, err := s.clones.MergeBase(ctx, string(repoPlatform(repo)), mergedHost, repo.Owner, repo.Name, preMergeBase, prHead)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeMergeBaseFailed,
			Err:  fmt.Errorf("merge-base for merged PR #%d: %w", number, err),
		}
	}

	if prHead == "" || mb == "" {
		return nil
	}

	applied, err := s.db.UpdateDiffSHAsSnapshot(
		ctx, mrID, expectedRevision, expectedHeadSHA, expectedBaseSHA,
		prHead, mb, mb,
	)
	if err != nil {
		return &DiffSyncError{
			Code: DiffSyncCodeInternal,
			Err:  fmt.Errorf("update diff SHAs for merged PR #%d: %w", number, err),
		}
	}
	if !applied {
		return errParentSnapshotAdvanced
	}
	return nil
}
