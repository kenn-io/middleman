package github

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

type nativeStackSyncTestClient struct {
	*mockClient
	mu        sync.Mutex
	pulls     []*gh.PullRequest
	hints     map[int]*platformgithub.NativeStackHint
	pages     map[int]platformgithub.NativeStackPage
	errors    map[int]error
	pageCalls []int
	// listErrors is consumed one entry per open-PR list call so a test can model
	// a 304 arriving on a later sync.
	listErrors []error
	listCalls  atomic.Int32
	// onPage runs before each stacks page is served so a test can model state
	// changing while the sync is in flight.
	onPage func()
}

func (c *nativeStackSyncTestClient) ListOpenPullRequestsWithNativeStackHints(
	context.Context, string, string,
) ([]*gh.PullRequest, map[int]*platformgithub.NativeStackHint, error) {
	c.listCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.listErrors) > 0 {
		err := c.listErrors[0]
		c.listErrors = c.listErrors[1:]
		if err != nil {
			return nil, nil, err
		}
	}
	return c.pulls, c.hints, nil
}

// notModified is the error the ETag transport surfaces for a 304 open-PR list.
func notModified() error {
	return &gh.ErrorResponse{Response: &http.Response{
		StatusCode: http.StatusNotModified,
		Request: &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: "https", Host: "api.github.com", Path: "/repos/owner/repo/pulls"},
		},
	}}
}

func (c *nativeStackSyncTestClient) ListNativeStacksPage(
	_ context.Context, _, _ string, page int,
) (platformgithub.NativeStackPage, error) {
	if c.onPage != nil {
		c.onPage()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pageCalls = append(c.pageCalls, page)
	if err := c.errors[page]; err != nil {
		return platformgithub.NativeStackPage{}, err
	}
	return c.pages[page], nil
}

func TestRefreshGitHubNativeStackCacheReusesConsistentCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "cached", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
			{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "b", HeadSHA: "bbb"},
		},
	}))
	client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)

	result := syncer.refreshGitHubNativeStackCache(t.Context(), RepoRef{
		Owner: "acme", Name: "widgets", PlatformHost: "github.com",
	}, repoID, map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
		102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
	}, false)

	assert.Equal([]int{42}, result.ConfirmedNumbers)
	assert.Empty(client.pageCalls)
	unchanged := syncer.refreshGitHubNativeStackCache(
		t.Context(), RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"},
		repoID, nil, true,
	)
	assert.Equal([]int{42}, unchanged.ConfirmedNumbers)
}

func TestRefreshGitHubNativeStackCacheStopsAfterTargetIsFoundOrPassed(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	stack := func(number int) platformgithub.NativeStack {
		return platformgithub.NativeStack{
			ID: int64(1000 + number), Number: number, BaseRef: "main",
			Open: true, CreatedAt: now,
			Members: []platformgithub.NativeStackMember{
				{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
				{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "b", HeadSHA: "bbb"},
			},
		}
	}
	cases := []struct {
		name              string
		page              platformgithub.NativeStackPage
		hintSize          int
		wantConfirmed     []int
		wantCached        int
		wantInvalidations int32
	}{
		{
			name: "target found on first page",
			page: platformgithub.NativeStackPage{Stacks: []platformgithub.NativeStack{
				stack(50), stack(42), stack(40),
			}, NextPage: 2},
			wantConfirmed: []int{42}, wantCached: 1,
		},
		{
			name: "target passed on first page",
			page: platformgithub.NativeStackPage{Stacks: []platformgithub.NativeStack{
				stack(50), stack(41),
			}, NextPage: 2},
			wantConfirmed: []int{}, wantCached: 0,
		},
		{
			// A rejected row leaves the target unaccounted for, so the pass is
			// partial: nothing projects and the next sync must re-list.
			name:     "target resource disagrees with pull request size",
			page:     platformgithub.NativeStackPage{Stacks: []platformgithub.NativeStack{stack(42)}},
			hintSize: 3, wantConfirmed: nil, wantCached: 0, wantInvalidations: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := openTestDB(t)
			repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
			require.NoError(err)
			client := &nativeStackSyncTestClient{
				mockClient: &mockClient{}, pages: map[int]platformgithub.NativeStackPage{1: tc.page},
			}
			syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
			syncer.now = func() time.Time { return now }
			hintSize := tc.hintSize
			if hintSize == 0 {
				hintSize = 2
			}

			result := syncer.refreshGitHubNativeStackCache(t.Context(), RepoRef{
				Owner: "acme", Name: "widgets", PlatformHost: "github.com",
			}, repoID, map[int]*platformgithub.NativeStackHint{
				101: {Number: 42, Size: hintSize, Position: 1, BaseRef: "main"},
				102: {Number: 42, Size: hintSize, Position: 2, BaseRef: "main"},
			}, false)

			assert.Equal(tc.wantConfirmed, result.ConfirmedNumbers)
			assert.Equal([]int{1}, client.pageCalls)
			assert.Equal(tc.wantInvalidations, client.invalidateCalls.Load())
			cached, err := database.ListGitHubNativeStacks(t.Context(), repoID)
			require.NoError(err)
			assert.Len(cached, tc.wantCached)
		})
	}
}

func TestRefreshGitHubNativeStackCacheTreatsPreviewNotFoundAsFallback(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		errors:     map[int]error{1: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}},
	}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)

	result := syncer.refreshGitHubNativeStackCache(t.Context(), RepoRef{
		Owner: "acme", Name: "widgets", PlatformHost: "github.com",
	}, repoID, map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
	}, false)

	assert.Empty(t, result.ConfirmedNumbers)
}

func TestRefreshGitHubNativeStackCacheDoesNotReconfirmSuspectCacheAfterNotModified(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "cached", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
			{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "b", HeadSHA: "bbb"},
		},
	}))
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		errors: map[int]error{1: &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusServiceUnavailable},
		}},
	}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
	repo := RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}

	failed := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 3, Position: 1, BaseRef: "main"},
	}, false)
	assert.Empty(t, failed.ConfirmedNumbers)
	unchanged := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, nil, true)
	assert.Empty(t, unchanged.ConfirmedNumbers)
}

func TestRefreshGitHubNativeStackCacheRefetchesUnobservableMembersOnSchedule(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		lastObservedAt time.Time
		wantPageCalls  []int
	}{
		{
			name:           "recent observation reuses the cache",
			lastObservedAt: now.Add(-time.Hour),
			wantPageCalls:  nil,
		},
		{
			name:           "stale observation refetches the stack",
			lastObservedAt: now.Add(-nativeStackObservationTTL - time.Hour),
			wantPageCalls:  []int{1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := openTestDB(t)
			repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
			require.NoError(err)
			// PR 100 is merged, so no open-PR hint can attest to its position.
			require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
				RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
				BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
				ContentFingerprint: "cached", LastObservedAt: tc.lastObservedAt,
				Members: []db.GitHubNativeStackMember{
					{Position: 1, PullRequestNumber: 100, State: "merged", HeadRef: "a", HeadSHA: "aaa"},
					{Position: 2, PullRequestNumber: 101, State: "open", HeadRef: "b", HeadSHA: "bbb"},
				},
			}))
			client := &nativeStackSyncTestClient{
				mockClient: &mockClient{},
				pages: map[int]platformgithub.NativeStackPage{1: {Stacks: []platformgithub.NativeStack{{
					ID: 900, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
					Members: []platformgithub.NativeStackMember{
						{Position: 1, PullRequestNumber: 100, State: "merged", HeadRef: "a", HeadSHA: "aaa"},
						{Position: 2, PullRequestNumber: 101, State: "open", HeadRef: "b", HeadSHA: "bbb"},
					},
				}}}},
			}
			syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
			syncer.now = func() time.Time { return now }

			result := syncer.refreshGitHubNativeStackCache(t.Context(), RepoRef{
				Owner: "acme", Name: "widgets", PlatformHost: "github.com",
			}, repoID, map[int]*platformgithub.NativeStackHint{
				101: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
			}, false)

			assert.Equal([]int{42}, result.ConfirmedNumbers)
			assert.Equal(tc.wantPageCalls, client.pageCalls)
		})
	}
}

func TestRefreshGitHubNativeStackCacheExpiresConfirmationsReusedByNotModified(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		elapsed       time.Duration
		wantConfirmed []int
		wantInvalidem int32
	}{
		{
			name: "within the revalidation window", elapsed: time.Hour,
			wantConfirmed: []int{42}, wantInvalidem: 0,
		},
		{
			name: "past the revalidation window", elapsed: nativeStackObservationTTL + time.Hour,
			wantConfirmed: nil, wantInvalidem: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := openTestDB(t)
			repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
			require.NoError(err)
			// PR 100 is merged, so the confirmation covers membership that an
			// unchanged pull-request list can never contradict.
			require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
				RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
				BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
				ContentFingerprint: "cached", LastObservedAt: now,
				Members: []db.GitHubNativeStackMember{
					{Position: 1, PullRequestNumber: 100, State: "merged", HeadRef: "a", HeadSHA: "aaa"},
					{Position: 2, PullRequestNumber: 101, State: "open", HeadRef: "b", HeadSHA: "bbb"},
				},
			}))
			client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
			syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
			clock := now
			syncer.now = func() time.Time { return clock }
			repo := RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}

			seeded := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, map[int]*platformgithub.NativeStackHint{
				101: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
			}, false)
			require.Equal([]int{42}, seeded.ConfirmedNumbers)

			clock = now.Add(tc.elapsed)
			unchanged := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, nil, true)

			assert.Equal(tc.wantConfirmed, unchanged.ConfirmedNumbers)
			assert.Equal(tc.wantInvalidem, client.invalidateCalls.Load())
		})
	}
}

func TestRefreshGitHubNativeStackCacheKeepsDeadlineTiedToStackObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	observed := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC)
	// The row is already 11 hours old and still holds a merged member.
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: observed,
		ContentFingerprint: "cached", LastObservedAt: observed,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 100, State: "merged", HeadRef: "a", HeadSHA: "aaa"},
			{Position: 2, PullRequestNumber: 101, State: "open", HeadRef: "b", HeadSHA: "bbb"},
		},
	}))
	client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
	clock := observed.Add(11 * time.Hour)
	syncer.now = func() time.Time { return clock }
	repo := RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}

	// An unrelated 200 response reconfirms the stack from cache without
	// refetching the catalog.
	seeded := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
	}, false)
	require.Equal([]int{42}, seeded.ConfirmedNumbers)
	require.Empty(client.pageCalls)

	// Two hours later the stack is 13 hours past its own observation, so the
	// confirmation must not survive on a deadline this refresh granted.
	clock = observed.Add(13 * time.Hour)
	unchanged := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, nil, true)

	assert.Empty(unchanged.ConfirmedNumbers)
	assert.EqualValues(1, client.invalidateCalls.Load())
}

// TestRunOnceWithdrawsAgedNativeStacksFromProjectionInput drives the aging rule
// through a real sync rather than the cache helper: the projection only ever
// sees what RunOnce publishes, so a deadline that survives the sync path is the
// one that would keep a stale predecessor out of the merge safeguard.
func TestRunOnceWithdrawsAgedNativeStacksFromProjectionInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	observed := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC)
	repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-owner-repo",
		Owner:          "owner",
		Name:           "repo",
	})
	require.NoError(err)
	// The cached stack claims a merged leading member, which no open-PR hint can
	// attest to, and it was last observed 11 hours ago.
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: observed,
		ContentFingerprint: "cached", LastObservedAt: observed,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 900, State: "merged", HeadRef: "feature/z", HeadSHA: "zzz"},
			{Position: 2, PullRequestNumber: 101, State: "open", HeadRef: "feature/b", HeadSHA: "bbb"},
		},
	}))
	tip := buildOpenPR(101, observed)
	tip.Head.Ref = new("feature/b")
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pulls:      []*gh.PullRequest{tip},
		hints: map[int]*platformgithub.NativeStackHint{
			101: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
		},
		// A second sync finds the open-PR list byte-identical.
		listErrors: []error{nil, notModified()},
	}
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	clock := observed.Add(11 * time.Hour)
	syncer.now = func() time.Time { return clock }
	syncer.SetPreferGitHubNativeStacks(true)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())
	require.Len(results, 1)
	require.NotNil(results[0].GitHubNativeStacks)
	require.Equal([]int{42}, results[0].GitHubNativeStacks.ConfirmedNumbers,
		"a cache-confirmed stack still inside its window must project")
	require.Empty(client.pageCalls, "reconfirming from cache must not refetch the catalog")

	// Enabling the preference and the first sync both invalidate ETags, so only
	// the delta across the aging sync says anything.
	invalidatesBefore := client.invalidateCalls.Load()

	// Two hours later the stack is past its own 12h window. The 304 must not
	// reuse a confirmation that this sync's own clock would have extended.
	clock = observed.Add(13 * time.Hour)
	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.NotNil(results[0].GitHubNativeStacks)
	assert.Empty(results[0].GitHubNativeStacks.ConfirmedNumbers,
		"an aged stack must be withheld from projection so branch inference owns the repo")
	assert.Equal(invalidatesBefore+1, client.invalidateCalls.Load(),
		"the pull-request list ETag must be evicted so the next sync refetches the catalog")
}

func TestRefreshGitHubNativeStackCacheMarksFailedPersistenceIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pages: map[int]platformgithub.NativeStackPage{1: {Stacks: []platformgithub.NativeStack{{
			ID: 900, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
			Members: []platformgithub.NativeStackMember{
				{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
				{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "b", HeadSHA: "bbb"},
			},
		}}}},
	}
	// Cancel after the catalog page is served so persistence, not the catalog
	// fetch, is what fails.
	client.onPage = cancel
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
	syncer.now = func() time.Time { return now }
	repo := RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}
	hints := map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
		102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
	}

	result := syncer.refreshGitHubNativeStackCache(ctx, repo, repoID, hints, false)

	assert.Empty(result.ConfirmedNumbers)
	assert.EqualValues(1, client.invalidateCalls.Load(),
		"a refresh whose persistence failed must force the next sync to re-list")
	unchanged := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, nil, true)
	assert.Empty(unchanged.ConfirmedNumbers)
}

func TestRefreshGitHubNativeStackCacheRejectsMemberClaimedByAnotherStack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	// Cached stack 42 still lists PR 103 from when it was closed.
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "cached", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
			{Position: 2, PullRequestNumber: 103, State: "closed", HeadRef: "c", HeadSHA: "ccc"},
		},
	}))
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		// The refetch of stack 42 still reports the reopened PR as closed.
		pages: map[int]platformgithub.NativeStackPage{1: {Stacks: []platformgithub.NativeStack{{
			ID: 900, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
			Members: []platformgithub.NativeStackMember{
				{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
				{Position: 2, PullRequestNumber: 103, State: "closed", HeadRef: "c", HeadSHA: "ccc"},
			},
		}}}},
	}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
	syncer.now = func() time.Time { return now }

	// PR 103 is open again and GitHub now reports it in stack 43.
	result := syncer.refreshGitHubNativeStackCache(t.Context(), RepoRef{
		Owner: "acme", Name: "widgets", PlatformHost: "github.com",
	}, repoID, map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
		103: {Number: 43, Size: 2, Position: 2, BaseRef: "main"},
	}, false)

	assert.NotContains(result.ConfirmedNumbers, 42,
		"a stack whose member now belongs to another stack must not stay confirmed")
}

func TestRefreshGitHubNativeStackCacheDoesNotReuseIncompleteRefreshAfterNotModified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(database.ReplaceGitHubNativeStack(t.Context(), db.GitHubNativeStack{
		RepoID: repoID, GitHubID: 900, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "cached", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "a", HeadSHA: "aaa"},
			{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "b", HeadSHA: "bbb"},
		},
	}))
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		errors: map[int]error{1: &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusServiceUnavailable},
		}},
	}
	syncer := NewSyncer(map[string]Client{"github.com": client}, database, nil, nil, time.Minute, nil, nil)
	repo := RepoRef{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}
	// Stack 42 is confirmable from cache, while PR 103 points at an uncached
	// stack whose catalog fetch fails: a partial refresh.
	hints := map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
		102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
		103: {Number: 43, Size: 1, Position: 1, BaseRef: "main"},
	}

	partial := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, hints, false)
	// Stack 42 alone cannot be projected either: stack 43 stayed unresolved, so
	// detection's overlap scan cannot see whether 42 claims a pull request 43
	// also holds, and projecting 42 could hide 43's predecessor.
	assert.Empty(partial.ConfirmedNumbers,
		"an incomplete refresh must project nothing rather than an unverifiable subset")
	assert.EqualValues(1, client.invalidateCalls.Load(),
		"an incomplete refresh must evict the pull-request list ETag so the next sync retries")

	unchanged := syncer.refreshGitHubNativeStackCache(t.Context(), repo, repoID, nil, true)
	assert.Empty(unchanged.ConfirmedNumbers,
		"a 304 must not reuse confirmations from an incomplete refresh")
}

func TestRunOnceDropsNativeStacksDisabledDuringSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	first := buildOpenPR(101, now)
	second := buildOpenPR(102, now)
	first.Head.Ref = new("feature/a")
	second.Head.Ref = new("feature/b")
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pulls:      []*gh.PullRequest{first, second},
		hints: map[int]*platformgithub.NativeStackHint{
			101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
			102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
		},
		pages: map[int]platformgithub.NativeStackPage{1: {
			Stacks: []platformgithub.NativeStack{{
				ID: 9001, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
				Members: []platformgithub.NativeStackMember{
					{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "feature/a", HeadSHA: "aaa"},
					{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "feature/b", HeadSHA: "bbb"},
				},
			}},
		}},
	}
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.SetPreferGitHubNativeStacks(true)
	// Model the user turning the preview off while this sync is still running.
	client.onPage = func() { syncer.SetPreferGitHubNativeStacks(false) }
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	assert.Nil(results[0].GitHubNativeStacks,
		"a result captured under the enabled preference must not project after it is disabled")
}

func TestRunOnceKeepsRESTHintsWhenGraphQLRejectsNativeStackFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	first := buildOpenPR(101, now)
	second := buildOpenPR(102, now)
	first.Head.Ref = new("feature/a")
	second.Head.Ref = new("feature/b")
	// GraphQL rejects the preview fields; the fallback query succeeds but says
	// nothing about stack membership.
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte("stackEntry")) {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Field 'stackEntry' doesn't exist on type 'PullRequest'"}]}`))
			return
		}
		if bytes.Contains(body, []byte("pullRequests")) {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
	}))
	defer gqlSrv.Close()
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pulls:      []*gh.PullRequest{first, second},
		hints: map[int]*platformgithub.NativeStackHint{
			101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
			102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
		},
		pages: map[int]platformgithub.NativeStackPage{1: {
			Stacks: []platformgithub.NativeStack{{
				ID: 9001, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
				Members: []platformgithub.NativeStackMember{
					{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "feature/a", HeadSHA: "aaa"},
					{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "feature/b", HeadSHA: "bbb"},
				},
			}},
		}},
	}
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
		),
	})
	syncer.SetPreferGitHubNativeStacks(true)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.NotNil(results[0].GitHubNativeStacks,
		"REST-derived hints must survive a GraphQL query that dropped the preview fields")
	assert.Equal([]int{42}, results[0].GitHubNativeStacks.ConfirmedNumbers)
}

func TestSetPreferGitHubNativeStacksReportsTransitionToExactlyOneCaller(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}}, database, nil, nil,
		time.Minute, nil, nil,
	)
	syncer.SetPreferGitHubNativeStacks(true)

	// Competing config writers each disable the preference. Only the caller
	// whose swap observed the enabled value may reconcile projections, so the
	// restore cannot run twice or be skipped no matter how they interleave.
	const callers = 8
	var transitions atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Go(func() {
			<-start
			if previous := syncer.SetPreferGitHubNativeStacks(false); previous {
				transitions.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	assert.EqualValues(1, transitions.Load())
	assert.False(syncer.preferGitHubNativeStacks.Load())
}

func TestSetPreferGitHubNativeStacksRefreshesHintsOnEnable(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{Owner: "acme", Name: "widgets", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	syncer.SetPreferGitHubNativeStacks(true)
	assert.EqualValues(1, client.invalidateCalls.Load())
	syncer.SetPreferGitHubNativeStacks(true)
	assert.EqualValues(1, client.invalidateCalls.Load())
	syncer.SetPreferGitHubNativeStacks(false)
	assert.EqualValues(1, client.invalidateCalls.Load())
	syncer.SetPreferGitHubNativeStacks(true)
	assert.EqualValues(2, client.invalidateCalls.Load())
}

func TestRunOncePublishesConfirmedNativeStackNumbers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	first := buildOpenPR(101, now)
	second := buildOpenPR(102, now)
	first.Head.Ref = new("feature/a")
	second.Head.Ref = new("feature/b")
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pulls:      []*gh.PullRequest{first, second},
		hints: map[int]*platformgithub.NativeStackHint{
			101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
			102: {Number: 42, Size: 2, Position: 2, BaseRef: "main"},
		},
		pages: map[int]platformgithub.NativeStackPage{1: {
			Stacks: []platformgithub.NativeStack{{
				ID: 9001, Number: 42, BaseRef: "main", Open: true, CreatedAt: now,
				Members: []platformgithub.NativeStackMember{
					{Position: 1, PullRequestNumber: 101, State: "open", HeadRef: "feature/a", HeadSHA: "aaa"},
					{Position: 2, PullRequestNumber: 102, State: "open", HeadRef: "feature/b", HeadSHA: "bbb"},
				},
			}},
		}},
	}
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.SetPreferGitHubNativeStacks(true)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.NotNil(results[0].GitHubNativeStacks)
	assert.Equal([]int{42}, results[0].GitHubNativeStacks.ConfirmedNumbers)
}

// TestSetPreferGitHubNativeStacksWaitsForStackProjection pins the ordering the
// disable-time recheck depends on. Rechecking the preference under the
// projection lock is only sound if a transition cannot land while that lock is
// held; otherwise an enable can slip in mid-reconciliation and the older
// disable overwrites the projection it just published.
func TestSetPreferGitHubNativeStacksWaitsForStackProjection(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}}, database, nil, nil,
		time.Minute, nil, nil,
	)
	holding := make(chan struct{})
	release := make(chan struct{})
	projectionDone := make(chan struct{})
	go func() {
		defer close(projectionDone)
		syncer.RunUnderStackProjection(func() {
			close(holding)
			<-release
		})
	}()
	<-holding

	swapped := make(chan bool, 1)
	go func() { swapped <- syncer.SetPreferGitHubNativeStacks(true) }()
	select {
	case <-swapped:
		assert.Fail("preference swapped while a projection held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	assert.False(syncer.PrefersGitHubNativeStacks(),
		"the projection must observe a stable preference for its whole run")

	close(release)
	<-projectionDone
	select {
	case previous := <-swapped:
		assert.False(previous)
		assert.True(syncer.PrefersGitHubNativeStacks())
	case <-time.After(5 * time.Second):
		assert.Fail("preference swap did not proceed after the projection finished")
	}
}

// TestListOpenPullRequestsWithNativeStackHintsSurvivesUnreadableHint pins the
// blast radius of the preview field. It rides along on the primary open-PR list,
// so a shape change on GitHub's side must cost the hint for that pull request,
// not the whole list and with it ordinary synchronization for the repository.
func TestListOpenPullRequestsWithNativeStackHintsSurvivesUnreadableHint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// PR 101 carries a stack field that is no longer an object.
		_, _ = w.Write([]byte(`[
			{"number":101,"state":"open","title":"tip","stack":"stack-42"},
			{"number":100,"state":"open","title":"base",
			 "stack":{"id":9042,"number":42,"size":2,"position":1,"base":{"ref":"main"}}}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ghClient, err := newEnterpriseGHClient(srv.Client(), srv.URL+"/api/v3/", srv.URL+"/api/uploads/")
	require.NoError(err)
	client, err := platformgithub.NewClient(platformgithub.ClientConfig{
		Host: "github.com", Read: ghClient.Client(), Write: ghClient.Client(), Notifications: ghClient.Client(),
		Clock: time.Now, APIBase: ghClient.BaseURL(),
	})
	require.NoError(err)

	prs, hints, err := client.ListOpenPullRequestsWithNativeStackHints(t.Context(), "acme", "widget")

	require.NoError(err, "an unreadable preview field must not fail the pull-request list")
	require.Len(prs, 2)
	assert.Equal([]int{101, 100}, []int{prs[0].GetNumber(), prs[1].GetNumber()})
	assert.Nil(hints[101], "a rejected hint leaves the pull request unclaimed")
	require.NotNil(hints[100])
	assert.Equal(42, hints[100].Number)
	assert.Equal("main", hints[100].BaseRef)
}

// TestNativeStackHintListingClassifiesDisabledPullRequests keeps the preview on
// the same failure taxonomy as the plain list. A repository with pull requests
// disabled answers 410; classified, it enters the feature cooldown, and
// unclassified it would be retried as a hard sync failure every cycle purely
// because the preview is enabled.
func TestNativeStackHintListingClassifiesDisabledPullRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	disabled := &gh.ErrorResponse{
		Response: &http.Response{
			StatusCode: http.StatusGone,
			Request: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Scheme: "https", Host: "api.github.com", Path: "/pulls"},
			},
		},
		Message: "Pull requests are disabled for this repository",
	}
	client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
	client.listErrors = []error{disabled}
	provider := newTestGitHubProvider(t, "github.com", client)

	_, _, err := provider.ListOpenMergeRequestsWithNativeStackHints(
		t.Context(), platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: "acme", Name: "widget",
		},
	)

	require.Error(err)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeRepositoryFeatureDisabled, platformErr.Code)
	assert.Equal(platform.RepositoryFeatureMergeRequests, platformErr.Capability)
}

// TestRunOncePersistsPullRequestsWhenHintIsUnclaimed carries the lenient hint
// decode through the sync engine: a pull request GitHub declined to claim (or
// whose hint could not be read) must still be indexed and fall back to branch
// inference, not be dropped along with its stack membership.
func TestRunOncePersistsPullRequestsWhenHintIsUnclaimed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	first := buildOpenPR(101, now)
	second := buildOpenPR(102, now)
	first.Head.Ref = new("feature/a")
	second.Head.Ref = new("feature/b")
	client := &nativeStackSyncTestClient{
		mockClient: &mockClient{},
		pulls:      []*gh.PullRequest{first, second},
		// 101 is claimed by no stack; 102's hint was rejected at decode and is
		// indistinguishable from that. Neither may cost the pull request itself.
		hints: map[int]*platformgithub.NativeStackHint{101: nil, 102: nil},
	}
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.now = func() time.Time { return now }
	syncer.SetPreferGitHubNativeStacks(true)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.NotNil(results[0].GitHubNativeStacks)
	assert.Empty(results[0].GitHubNativeStacks.ConfirmedNumbers,
		"unclaimed pull requests confirm no stack")
	repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	for _, number := range []int{101, 102} {
		mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, number)
		require.NoError(err)
		assert.NotNil(mr, "pull request %d must be indexed regardless of its hint", number)
	}
}

// TestRunOncePutsDisabledPullRequestsIntoCooldownWithHintsEnabled proves the
// classification reaches the cooldown machinery. A repository with pull requests
// disabled answers 410 on the hint listing; without classification the syncer
// would treat that as a hard failure and re-list every cycle.
func TestRunOncePutsDisabledPullRequestsIntoCooldownWithHintsEnabled(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	disabled := fmt.Errorf("list open pull requests: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Pull requests are disabled for this repository",
	})
	client := &nativeStackSyncTestClient{mockClient: &mockClient{}}
	client.listErrors = []error{disabled, disabled}
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo", PlatformExternalID: "repo-owner-repo",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.now = func() time.Time { return now }
	syncer.SetPreferGitHubNativeStacks(true)

	syncer.RunOnce(t.Context())
	syncer.RunOnce(t.Context())

	assert.EqualValues(1, client.listCalls.Load(),
		"the second sync must respect the feature cooldown instead of re-listing")
	_, failed := syncer.failedRepos.Load(repoFailKey(repo))
	assert.False(failed, "a disabled feature is a cooldown, not a repository failure")
}
