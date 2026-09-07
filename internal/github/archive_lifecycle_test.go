package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

type blockingArchiveRunner struct {
	calls   atomic.Int64
	started chan struct{}
	done    chan struct{}
}

type archiveLifecycleRecorder struct {
	ensured []platform.RepoRef
	retried []platform.RepoRef
	// ensureResult, when set, is returned from EnsureConfigured in place of
	// the full ref list to simulate refs skipped by seeding.
	ensureResult []platform.RepoRef
}

type archiveWorkerProvider struct{ ref platform.RepoRef }

type archiveLifecycleClock struct{ now func() time.Time }

func (c archiveLifecycleClock) Now() time.Time { return c.now() }

type archivePageMockClient struct {
	*mockClient
	listInventoryIssuesPageFn func(
		context.Context, string, string, string, string, string,
	) ([]*gh.Issue, string, bool, error)
	listInventoryPullRequestsPageFn func(
		context.Context, string, string, string, int,
	) ([]*gh.PullRequest, bool, error)
}

func (m *archivePageMockClient) ListInventoryIssuesPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	cursor string,
	since string,
) ([]*gh.Issue, string, bool, error) {
	m.trackCall()
	return m.listInventoryIssuesPageFn(ctx, owner, repo, sortBy, cursor, since)
}

func (m *archivePageMockClient) ListInventoryPullRequestsPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	page int,
) ([]*gh.PullRequest, bool, error) {
	m.trackCall()
	return m.listInventoryPullRequestsPageFn(ctx, owner, repo, sortBy, page)
}

type priorityWorkOperation string

const (
	priorityWorkIndex priorityWorkOperation = "index"
	priorityWorkMR    priorityWorkOperation = "merge_request"
	priorityWorkIssue priorityWorkOperation = "issue"
)

type blockingPriorityProvider struct {
	syncTestProvider
	ref       platform.RepoRef
	operation priorityWorkOperation
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (p *blockingPriorityProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories: true, ReadMergeRequests: true, ReadIssues: true,
	}
}

func (p *blockingPriorityProvider) GetRepository(
	context.Context, platform.RepoRef,
) (platform.Repository, error) {
	return platform.Repository{
		Ref: p.ref, PlatformID: 1, PlatformExternalID: "repo-1",
		DefaultBranch: "main",
	}, nil
}

func (*blockingPriorityProvider) ListRepositories(
	context.Context, string, platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return nil, nil
}

func (p *blockingPriorityProvider) wait(operation priorityWorkOperation) {
	if p.operation != operation {
		return
	}
	p.startOnce.Do(func() { close(p.started) })
	<-p.release
}

func (p *blockingPriorityProvider) ListOpenMergeRequests(
	context.Context, platform.RepoRef,
) ([]platform.MergeRequest, error) {
	p.wait(priorityWorkIndex)
	return nil, nil
}

func (p *blockingPriorityProvider) GetMergeRequest(
	context.Context, platform.RepoRef, int,
) (platform.MergeRequest, error) {
	p.wait(priorityWorkMR)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	return platform.MergeRequest{
		Repo: p.ref, PlatformID: 7, PlatformExternalID: "mr-7", Number: 7,
		Title: "Synthetic MR", State: "open", CreatedAt: now, UpdatedAt: now,
		LastActivityAt: now,
	}, nil
}

func (*blockingPriorityProvider) ListMergeRequestEvents(
	context.Context, platform.RepoRef, int,
) ([]platform.MergeRequestEvent, error) {
	return nil, nil
}

func (*blockingPriorityProvider) ListOpenIssues(
	context.Context, platform.RepoRef,
) ([]platform.Issue, error) {
	return nil, nil
}

func (p *blockingPriorityProvider) GetIssue(
	context.Context, platform.RepoRef, int,
) (platform.Issue, error) {
	p.wait(priorityWorkIssue)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	return platform.Issue{
		Repo: p.ref, PlatformID: 8, PlatformExternalID: "issue-8", Number: 8,
		Title: "Synthetic issue", State: "open", CreatedAt: now, UpdatedAt: now,
		LastActivityAt: now,
	}, nil
}

func (*blockingPriorityProvider) ListIssueEvents(
	context.Context, platform.RepoRef, int,
) ([]platform.IssueEvent, error) {
	return nil, nil
}

func (p *archiveWorkerProvider) Platform() platform.Kind { return p.ref.Platform }
func (p *archiveWorkerProvider) Host() string            { return p.ref.Host }
func (*archiveWorkerProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadIssues: true,
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues: true, HistoricalMergeRequests: true,
		},
	}
}
func (p *archiveWorkerProvider) archivedIssue() platform.Issue {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	return platform.Issue{
		Repo: p.ref, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1,
		URL: "https://github.test/acme/widget/issues/1", Title: "Archived issue",
		Author: "alice", State: "closed", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}
}
func (p *archiveWorkerProvider) ListIssuesPage(_ context.Context, _ platform.RepoRef, query platform.ItemPageQuery) (platform.Page[platform.Issue], error) {
	if query.UpdatedSince != nil {
		return platform.Page[platform.Issue]{Exhausted: true}, nil
	}
	return platform.Page[platform.Issue]{Items: []platform.Issue{p.archivedIssue()}, Exhausted: true}, nil
}
func (*archiveWorkerProvider) ListMergeRequestsPage(context.Context, platform.RepoRef, platform.ItemPageQuery) (platform.Page[platform.MergeRequest], error) {
	return platform.Page[platform.MergeRequest]{Exhausted: true}, nil
}
func (*archiveWorkerProvider) ListOpenIssues(context.Context, platform.RepoRef) ([]platform.Issue, error) {
	return nil, nil
}
func (p *archiveWorkerProvider) GetIssue(context.Context, platform.RepoRef, int) (platform.Issue, error) {
	return p.archivedIssue(), nil
}
func (*archiveWorkerProvider) ListIssueEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.IssueEvent, error) {
	return nil, nil
}

// preemptibleArchiveProvider blocks its first archive issue read until the
// admitted request context is canceled, so a test can drive real live-work
// preemption of an in-flight archive request. Subsequent reads succeed.
type preemptibleArchiveProvider struct {
	*archiveWorkerProvider
	readStarted chan struct{}
	blockOnce   sync.Once
}

func (p *preemptibleArchiveProvider) GetIssue(
	ctx context.Context, ref platform.RepoRef, number int,
) (platform.Issue, error) {
	blocked := false
	p.blockOnce.Do(func() { blocked = true })
	if blocked {
		close(p.readStarted)
		<-ctx.Done()
		return platform.Issue{}, ctx.Err()
	}
	return p.archiveWorkerProvider.GetIssue(ctx, ref, number)
}

func TestArchiveHydrationPRShapedIssueBecomesTerminalInSQLite(t *testing.T) {
	testArchiveHydrationMissingGitHubIssueBecomesTerminalInSQLite(
		t,
		http.StatusOK,
		`{"id":11,"node_id":"PR_11","number":11,"repository_url":"https://api.github.com/repos/acme/widget","html_url":"https://github.com/acme/widget/pull/11","title":"actually a pull request","state":"closed","user":{"login":"author"},"pull_request":{"url":"https://api.github.com/repos/acme/widget/pulls/11"},"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","closed_at":"2025-01-02T00:00:00Z"}`,
		"terminal archive lookup must not hydrate the PR-shaped issue again",
	)
}

func TestArchiveHydrationDeletedGitHubIssueBecomesTerminalInSQLite(t *testing.T) {
	testArchiveHydrationMissingGitHubIssueBecomesTerminalInSQLite(
		t,
		http.StatusGone,
		`{"message":"This issue was deleted"}`,
		"terminal archive lookup must not hydrate the deleted issue again",
	)
}

func testArchiveHydrationMissingGitHubIssueBecomesTerminalInSQLite(
	t *testing.T,
	hydrationCode int,
	hydrationBody string,
	retryAssertion string,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "R_widget",
	}
	var hydrationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/graphql":
			var request struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, `{"message":"invalid GraphQL request"}`, http.StatusBadRequest)
				return
			}
			switch {
			case strings.Contains(request.Query, "issues(first: 100"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{"id":"I_11","databaseId":11,"number":11,"title":"historical issue identity","state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/11","author":{"login":"author"},"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2025-01-02T00:00:00Z","closedAt":"2025-01-02T00:00:00Z","comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
			case strings.Contains(request.Query, "pullRequests(first: 100"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
			default:
				http.Error(w, `{"message":"unexpected GraphQL query"}`, http.StatusBadRequest)
			}
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}`))
		case "/api/v3/repos/acme/widget/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/repos/acme/widget/issues/11":
			hydrationCalls.Add(1)
			w.WriteHeader(hydrationCode)
			_, _ = w.Write([]byte(hydrationBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	provider := newArchiveTestGitHubProvider(t, server.URL)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil,
		[]RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
			PlatformExternalID: ref.PlatformExternalID,
		}},
		time.Hour, nil, nil,
	)
	syncer.now = func() time.Time { return now }
	service, err := archive.NewService(
		database, registry, nil, syncer, nil,
		archiveLifecycleClock{now: func() time.Time { return now }},
	)
	require.NoError(err)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context())) // issue inventory
	require.NoError(service.RunEligible(t.Context())) // pull-request inventory
	require.NoError(service.RunEligible(t.Context())) // issue hydration

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	progress, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 11, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressTerminal, progress.Status)
	assert.Zero(progress.AttemptCount)
	assert.Nil(progress.NextRetryAt)
	require.NotNil(progress.LastErrorCode)
	assert.Equal(string(platform.ErrCodeNotFound), *progress.LastErrorCode)
	assert.NotNil(progress.CompletedAt)

	var lifecycle db.ArchiveLifecycleState
	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT lifecycle_state FROM forge_archive_items
		WHERE repo_id = ? AND item_type = ? AND item_number = ?`,
		repo.ID, db.ArchiveItemTypeIssue, 11,
	).Scan(&lifecycle))
	assert.Equal(db.ArchiveLifecycleStateRemovedUpstream, lifecycle)
	assert.Equal(int32(1), hydrationCalls.Load())

	require.NoError(service.RunEligible(t.Context()))
	assert.Equal(int32(1), hydrationCalls.Load(), retryAssertion)
}

func TestArchiveHydrationKeepsIncompleteMergedGitHubPRFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	mergedAt := now.Add(-time.Hour)
	item := buildOpenPR(7, mergedAt)
	item.State = new("closed")
	item.Merged = new(true)
	item.MergedAt = makeTimestamp(mergedAt)
	item.ClosedAt = makeTimestamp(mergedAt)
	item.MergeCommitSHA = new("merge-sha")
	item.ChangedFiles = nil
	client := &archivePageMockClient{
		mockClient: &mockClient{singlePR: item},
		listInventoryIssuesPageFn: func(
			context.Context, string, string, string, string, string,
		) ([]*gh.Issue, string, bool, error) {
			return nil, "", true, nil
		},
		listInventoryPullRequestsPageFn: func(
			context.Context, string, string, string, int,
		) ([]*gh.PullRequest, bool, error) {
			return []*gh.PullRequest{item}, false, nil
		},
	}
	tracker := NewPlatformRateTracker(database, "github", "github.com", "host", "rest")
	tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute),
	})
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: ref.Owner, Name: ref.Name,
		}},
		time.Hour, map[string]*RateTracker{"github.com": tracker}, testBudget(5000),
	)
	syncer.now = func() time.Time { return now }
	service, err := archive.NewService(
		database, syncer.clients, syncer, syncer, nil,
		archiveLifecycleClock{now: func() time.Time { return now }},
	)
	require.NoError(err)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	for range 2 {
		require.NoError(service.RunEligible(t.Context()))
	}
	require.ErrorContains(service.RunEligible(t.Context()), "files_changed")

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	progress, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeMergeRequest, 7,
		db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressFailed, progress.Status)
	assert.Equal(1, progress.AttemptCount)
	assert.Nil(progress.CompletedAt)
	require.NotNil(progress.LastErrorDetail)
	assert.Contains(*progress.LastErrorDetail, "files_changed")
}

func TestArchivePreemptedItemRecordsNoFailureAndCompletesOnNextPass(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-1",
	}
	provider := &preemptibleArchiveProvider{
		archiveWorkerProvider: &archiveWorkerProvider{ref: ref},
		readStarted:           make(chan struct{}),
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	now := time.Now().UTC()
	key := RateBucketKey("github", ref.Host, "host")
	tracker := NewPlatformRateTracker(database, "github", ref.Host, "host", "rest")
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute)})
	budget := NewSyncBudget(5000)
	syncer := NewSyncerWithRegistry(registry, database, nil, []RepoRef{{
		Platform: ref.Platform, PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		PlatformExternalID: ref.PlatformExternalID,
	}}, time.Hour, map[string]*RateTracker{key: tracker}, map[string]*SyncBudget{key: budget})
	syncer.now = func() time.Time { return now }

	// Real Syncer admission (not nil): the admitted archive request gets a real
	// cancellable provider lease that live work can preempt, unlike a fake
	// already-canceled admission.
	service, err := archive.NewService(database, registry, syncer, syncer, nil, nil)
	require.NoError(err)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context())) // issue inventory
	require.NoError(service.RunEligible(t.Context())) // merge-request inventory

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)

	// The archive pass delegates to the existing issue sync, which blocks in
	// its canonical lookup until live work begins.
	hydrationDone := make(chan error, 1)
	go func() { hydrationDone <- service.RunEligible(t.Context()) }()
	select {
	case <-provider.readStarted:
	case <-time.After(2 * time.Second):
		require.Fail("archive item sync did not start")
	}

	// beginProviderWork cancels the in-flight archive lease and waits for it to
	// release; the hydration pass observes preemption and records nothing.
	releaseLive := syncer.beginProviderWork(key, archive.PriorityActiveDetail)
	require.NoError(<-hydrationDone)

	lookup, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressPending, lookup.Status)
	assert.Zero(lookup.AttemptCount)

	var lastErrorCode *string
	var nextRetryAt *time.Time
	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT last_error_code, next_retry_at FROM forge_archive_repos WHERE repo_id = ?`, repo.ID,
	).Scan(&lastErrorCode, &nextRetryAt))
	assert.Nil(lastErrorCode)
	assert.Nil(nextRetryAt)

	// Live work releases; the next archive pass claims the still-pending item
	// and completes hydration.
	releaseLive()
	require.NoError(service.RunEligible(t.Context()))

	lookup, err = database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressComplete, lookup.Status)
	assert.NotNil(lookup.CompletedAt)
}

func TestArchiveDisabledIssueInventoryCompletesUnsupportedWithoutBlockingMergeRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	rawDisabled := fmt.Errorf("list archive issues: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Issues are disabled for this repo",
	})
	var issueCalls atomic.Int32
	var issueInventoryCalls atomic.Int32
	var mergeRequestCalls atomic.Int32
	client := &archivePageMockClient{
		mockClient: &mockClient{},
		listInventoryIssuesPageFn: func(
			_ context.Context, _, _, sortBy, _, _ string,
		) ([]*gh.Issue, string, bool, error) {
			issueCalls.Add(1)
			if sortBy == "created" {
				issueInventoryCalls.Add(1)
			}
			return nil, "", false, rawDisabled
		},
		listInventoryPullRequestsPageFn: func(
			context.Context, string, string, string, int,
		) ([]*gh.PullRequest, bool, error) {
			mergeRequestCalls.Add(1)
			return nil, false, nil
		},
	}
	tracker := NewPlatformRateTracker(database, "github", "github.com", "host", "rest")
	tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute),
	})
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Hour, map[string]*RateTracker{"github.com": tracker}, testBudget(5000),
	)
	syncer.now = func() time.Time { return now }
	clock := archiveLifecycleClock{now: func() time.Time { return now }}
	service, err := archive.NewService(
		database, syncer.clients, syncer, syncer, nil, clock,
	)
	require.NoError(err)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))

	assert.Equal(int32(1), issueInventoryCalls.Load())
	assert.Greater(issueCalls.Load(), issueInventoryCalls.Load(),
		"maintenance must verify the provider stream despite the inventory cooldown")
	assert.GreaterOrEqual(mergeRequestCalls.Load(), int32(1))
	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.True(states[0].IssueInventory.Complete())
	assert.True(states[0].MergeRequestInventory.Complete())
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].IssuesCoverage)
	assert.Equal(db.ArchiveCoverageSupported, states[0].MergeRequestsCoverage)

	now = now.Add(repositoryFeatureProbeInterval)
	tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute),
	})
	require.NoError(service.RunEligible(t.Context()))
	assert.Equal(int32(1), issueInventoryCalls.Load(), "unsupported inventory must not be retried")
	assert.Greater(issueCalls.Load(), issueInventoryCalls.Load(),
		"maintenance may continue probing the independently disabled live feature")
}

type disabledArchiveHydrationFixture struct {
	database       *db.DB
	now            time.Time
	ref            platform.RepoRef
	item           *gh.Issue
	disabled       atomic.Bool
	hydrationCalls atomic.Int32
	client         *archivePageMockClient
	tracker        *RateTracker
	syncer         *Syncer
	service        *archive.Service
}

func newDisabledArchiveHydrationFixture(t *testing.T) *disabledArchiveHydrationFixture {
	t.Helper()
	require := require.New(t)
	fixture := &disabledArchiveHydrationFixture{
		database: dbtest.Open(t),
		now:      time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
		ref: platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		},
	}
	fixture.disabled.Store(true)
	fixture.item = buildOpenIssue(7, fixture.now)
	state := "closed"
	fixture.item.State = &state
	rawDisabled := fmt.Errorf("get archive issue: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Issues are disabled for this repo",
	})
	fixture.client = &archivePageMockClient{
		mockClient: &mockClient{
			getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
				fixture.hydrationCalls.Add(1)
				if fixture.disabled.Load() {
					return nil, rawDisabled
				}
				return fixture.item, nil
			},
		},
		listInventoryIssuesPageFn: func(
			context.Context, string, string, string, string, string,
		) ([]*gh.Issue, string, bool, error) {
			return []*gh.Issue{fixture.item}, "", true, nil
		},
		listInventoryPullRequestsPageFn: func(
			context.Context, string, string, string, int,
		) ([]*gh.PullRequest, bool, error) {
			return nil, false, nil
		},
	}
	fixture.tracker = NewPlatformRateTracker(
		fixture.database, "github", "github.com", "host", "rest",
	)
	fixture.tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999, Reset: fixture.now.Add(time.Minute),
	})
	fixture.syncer = NewSyncer(
		map[string]Client{"github.com": fixture.client}, fixture.database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Hour,
		map[string]*RateTracker{"github.com": fixture.tracker},
		testBudget(5000),
	)
	fixture.syncer.now = func() time.Time { return fixture.now }
	clock := archiveLifecycleClock{now: func() time.Time { return fixture.now }}
	var err error
	fixture.service, err = archive.NewService(
		fixture.database, fixture.syncer.clients,
		fixture.syncer, fixture.syncer, nil, clock,
	)
	require.NoError(err)
	requireEnsureConfigured(t, fixture.service, []platform.RepoRef{fixture.ref})
	_, err = fixture.service.Start(t.Context(), []platform.RepoRef{fixture.ref})
	require.NoError(err)
	for range 3 {
		require.NoError(fixture.service.RunEligible(t.Context()))
	}
	return fixture
}

func (f *disabledArchiveHydrationFixture) progress(t *testing.T) db.ArchiveDatasetProgress {
	t.Helper()
	require := require.New(t)
	repo, err := f.database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(f.ref))
	require.NoError(err)
	require.NotNil(repo)
	progress, err := f.database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	return progress
}

func TestArchiveDisabledIssueHydrationRecoversImmediatelyAfterRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDisabledArchiveHydrationFixture(t)
	progress := fixture.progress(t)
	assert.Equal(db.ArchiveDatasetProgressPending, progress.Status)
	assert.Zero(progress.AttemptCount)
	assert.Nil(progress.NextRetryAt)
	assert.Equal(int32(1), fixture.hydrationCalls.Load())

	fixture.disabled.Store(false)
	fixture.now = fixture.now.Add(time.Minute)
	restartedTracker := NewPlatformRateTracker(
		fixture.database, "github", "github.com", "host", "rest",
	)
	restartedTracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999, Reset: fixture.now.Add(time.Minute),
	})
	restarted := NewSyncer(
		map[string]Client{"github.com": fixture.client}, fixture.database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Hour,
		map[string]*RateTracker{"github.com": restartedTracker},
		testBudget(5000),
	)
	restarted.now = func() time.Time { return fixture.now }
	service, err := archive.NewService(
		fixture.database, restarted.clients, restarted, restarted, nil,
		archiveLifecycleClock{now: func() time.Time { return fixture.now }},
	)
	require.NoError(err)
	require.NoError(service.RunEligible(t.Context()))
	assert.Equal(db.ArchiveDatasetProgressComplete, fixture.progress(t).Status)
	assert.Equal(int32(2), fixture.hydrationCalls.Load())
}

func TestArchiveDisabledIssueHydrationRecoversAfterManualProbe(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDisabledArchiveHydrationFixture(t)
	fixture.disabled.Store(false)

	require.NoError(fixture.syncer.SyncRepoOnProvider(
		t.Context(), platform.KindGitHub, "github.com", "acme", "widget",
	))
	require.NoError(fixture.service.RunEligible(t.Context()))
	assert.Equal(db.ArchiveDatasetProgressComplete, fixture.progress(t).Status)
	assert.Equal(int32(2), fixture.hydrationCalls.Load())
}

func (*archiveLifecycleRecorder) RunPass(context.Context) (bool, error) { return false, nil }

func (r *archiveLifecycleRecorder) EnsureConfigured(_ context.Context, refs []platform.RepoRef) ([]platform.RepoRef, error) {
	r.ensured = append(r.ensured, refs...)
	if r.ensureResult != nil {
		return r.ensureResult, nil
	}
	return refs, nil
}

func (r *archiveLifecycleRecorder) RetryAuthentication(_ context.Context, refs []platform.RepoRef) error {
	r.retried = append(r.retried, refs...)
	return nil
}

func newBlockingArchiveRunner() *blockingArchiveRunner {
	return &blockingArchiveRunner{started: make(chan struct{}), done: make(chan struct{})}
}

func (r *blockingArchiveRunner) RunPass(ctx context.Context) (bool, error) {
	if r.calls.Add(1) == 1 {
		close(r.started)
	}
	<-ctx.Done()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return false, ctx.Err()
}

func TestArchiveWorkerJoinsSyncerShutdown(t *testing.T) {
	database := dbtest.Open(t)
	syncer := NewSyncerWithRegistry(nil, database, nil, nil, time.Hour, nil, nil)
	runner := newBlockingArchiveRunner()
	syncer.SetArchiveService(runner)
	syncer.Start(t.Context())
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		require.Fail(t, "archive worker did not start")
	}

	syncer.Stop()
	select {
	case <-runner.done:
	case <-time.After(time.Second):
		require.Fail(t, "syncer stop returned before archive worker exited")
	}
}

func TestArchiveWorkerAdvancesRealServiceAfterStart(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-1",
	}
	provider := &archiveWorkerProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, database, nil, []RepoRef{{
		Platform: ref.Platform, PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		PlatformExternalID: ref.PlatformExternalID,
	}}, time.Hour, nil, nil)
	service, err := archive.NewService(database, registry, nil, syncer, nil, nil)
	require.NoError(err)
	service.SetWake(syncer.WakeArchive)
	syncer.SetArchiveService(service)
	syncer.SetArchivePollIntervalForTesting(time.Millisecond)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	syncer.Start(t.Context())
	t.Cleanup(syncer.Stop)

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		states, stateErr := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
		require.NoError(stateErr)
		if len(states) == 1 && states[0].IssueInventory.Complete() {
			assert.Equal(db.ArchiveCollectionModeFull, states[0].CollectionMode)
			var itemCount int
			require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
				SELECT COUNT(*) FROM forge_archive_items
				WHERE repo_id = ? AND item_type = 'issue'`, repo.ID,
			).Scan(&itemCount))
			assert.Equal(1, itemCount)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Fail("archive worker did not advance the real archive service")
}

func TestSetReposSeedsArchiveDiscoveryAndRetriesCredentialsBeforeCutover(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := NewSyncerWithRegistry(nil, database, nil, nil, time.Hour, nil, nil)
	recorder := &archiveLifecycleRecorder{}
	syncer.SetArchiveService(recorder)
	repos := []RepoRef{{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group/subgroup", Name: "project", RepoPath: "group/subgroup/project",
	}}

	require.NoError(syncer.SetReposWithContext(t.Context(), repos, true))
	require.Len(recorder.ensured, 1)
	require.Len(recorder.retried, 1)
	assert.Equal(platform.KindGitLab, recorder.ensured[0].Platform)
	assert.Equal("gitlab.test", recorder.ensured[0].Host)
	assert.Equal("group/subgroup/project", recorder.ensured[0].RepoPath)
	assert.Equal(recorder.ensured, recorder.retried)
	assert.Equal(repos, syncer.TrackedRepos())
}

func TestSetReposSeedsActiveArchiveForArchivedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	registry, err := platform.NewRegistry(syncTestProvider{
		kind: platform.KindGitHub, host: "github.test",
	})
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, database, nil, nil, time.Hour, nil, nil)
	service, err := archive.NewService(database, registry, nil, syncer, nil, nil)
	require.NoError(err)
	syncer.SetArchiveService(service)
	ref := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.test",
		Owner: "acme", Name: "frozen", RepoPath: "acme/frozen",
		PlatformExternalID: "repo-frozen", Archived: true,
	}

	require.NoError(syncer.SetReposWithContext(t.Context(), []RepoRef{ref}, false))

	repo, err := database.GetRepoByIdentity(
		t.Context(), platformdb.DBRepoIdentity(platformRepoRef(ref)),
	)
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState,
		"an archived configured repo keeps an active archive")
}

func TestSetReposPassesOnlySeededRefsToRetryAuthentication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := NewSyncerWithRegistry(nil, database, nil, nil, time.Hour, nil, nil)
	seeded := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "good", RepoPath: "acme/good",
	}
	recorder := &archiveLifecycleRecorder{
		ensureResult: []platform.RepoRef{seeded},
	}
	syncer.SetArchiveService(recorder)
	repos := []RepoRef{
		{Owner: "acme", Name: "good", PlatformHost: "github.com"},
		{Owner: "acme", Name: "ghost", PlatformHost: "github.com"},
	}

	require.NoError(syncer.SetReposWithContext(t.Context(), repos, true))
	require.Len(recorder.ensured, 2)
	assert.Equal([]platform.RepoRef{seeded}, recorder.retried,
		"refs skipped by seeding must not reach authentication retry")
}

func TestSyncRepoReplacementReconcilesArchiveLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	configured := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group", Name: "project", RepoPath: "group/project",
		PlatformExternalID: "gid://gitlab/Project/old",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.test",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.test",
				Owner: "group", Name: "project", RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/old",
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{configured}, time.Hour, nil, nil,
	)
	service, err := archive.NewService(database, registry, nil, syncer, nil, nil)
	require.NoError(err)
	syncer.SetArchiveService(service)
	_, err = service.EnsureConfigured(
		ctx, []platform.RepoRef{platformRepoRef(configured)},
	)
	require.NoError(err)
	oldEntry, err := database.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.test", "gid://gitlab/Project/old",
	)
	require.NoError(err)
	require.NotNil(oldEntry)

	provider.repository.PlatformExternalID = "gid://gitlab/Project/new"
	require.NoError(syncer.syncRepo(ctx, configured))

	newEntry, err := database.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.test", "gid://gitlab/Project/new",
	)
	require.NoError(err)
	require.NotNil(newEntry)
	states, err := database.ListArchiveRepoStates(ctx, nil)
	require.NoError(err)
	require.Len(states, 2)
	stateByRepoID := map[int64]db.ArchiveRepoState{}
	for _, state := range states {
		stateByRepoID[state.RepoID] = state
	}
	assert.Equal(
		db.ArchiveOperatorStatePaused,
		stateByRepoID[oldEntry.Repository.ID].OperatorState,
	)
	require.NotNil(stateByRepoID[oldEntry.Repository.ID].LastErrorCode)
	assert.Equal(
		string(db.ArchiveErrorCodeConfigurationRemoved),
		*stateByRepoID[oldEntry.Repository.ID].LastErrorCode,
	)
	assert.Equal(
		db.ArchiveOperatorStateActive,
		stateByRepoID[newEntry.Repository.ID].OperatorState,
	)
}

func TestSyncReusedRouteResolvingSuccessorKeepsBothReposTracked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	// The renamed repository moved to a new route; a different repository
	// reused its old route. Both are tracked. A sync of the old route
	// whose snapshot still carries the renamed repository's id resolves
	// the successor — neither repository may be lost or duplicated.
	renamed := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group", Name: "project-moved", RepoPath: "group/project-moved",
		PlatformExternalID: "gid://gitlab/Project/old",
	}
	successor := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group", Name: "project", RepoPath: "group/project",
		PlatformExternalID: "gid://gitlab/Project/new",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.test",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.test",
				Owner: "group", Name: "project", RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/new",
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{renamed, successor}, time.Hour, nil, nil,
	)
	service, err := archive.NewService(database, registry, nil, syncer, nil, nil)
	require.NoError(err)
	syncer.SetArchiveService(service)
	_, err = service.EnsureConfigured(ctx, []platform.RepoRef{
		platformRepoRef(renamed), platformRepoRef(successor),
	})
	require.NoError(err)

	stale := successor
	stale.PlatformExternalID = renamed.PlatformExternalID
	require.NoError(syncer.syncRepo(ctx, stale))

	tracked := syncer.TrackedRepos()
	require.Len(tracked, 2, "neither repository may be lost or duplicated")
	byID := map[string]RepoRef{}
	for _, repo := range tracked {
		byID[repo.PlatformExternalID] = repo
	}
	assert.Equal("project-moved", byID["gid://gitlab/Project/old"].Name,
		"the renamed repository keeps its tracked entry")
	assert.Equal("project", byID["gid://gitlab/Project/new"].Name)

	oldEntry, err := database.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.test", "gid://gitlab/Project/old",
	)
	require.NoError(err)
	require.NotNil(oldEntry)
	newEntry, err := database.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.test", "gid://gitlab/Project/new",
	)
	require.NoError(err)
	require.NotNil(newEntry)
	assert.NotEqual(oldEntry.Repository.ID, newEntry.Repository.ID)
}

func TestSyncRouteReplacementIgnoresDisplacedArchivedFlipE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	// The tracked occupant of the configured route was archived after this
	// sync pass snapshotted it, and the route meanwhile resolves to an
	// untracked replacement repository. The displaced repo's archived flip
	// must not stamp the replacement, which would silently exclude it from
	// live sync.
	displaced := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group", Name: "project", RepoPath: "group/project",
		PlatformExternalID: "gid://gitlab/Project/old", Archived: true,
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.test",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.test",
				Owner: "group", Name: "project", RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/new",
			Archived:           false,
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{displaced}, time.Hour, nil, nil,
	)
	service, err := archive.NewService(database, registry, nil, syncer, nil, nil)
	require.NoError(err)
	syncer.SetArchiveService(service)
	_, err = service.EnsureConfigured(
		ctx, []platform.RepoRef{platformRepoRef(displaced)},
	)
	require.NoError(err)

	snapshot := displaced
	snapshot.Archived = false
	require.NoError(syncer.syncRepo(ctx, snapshot))

	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Equal("gid://gitlab/Project/new", tracked[0].PlatformExternalID)
	assert.False(tracked[0].Archived,
		"the replacement keeps its authoritative unarchived state")
}

func TestArchiveAdmissionSharesSyncBudgetAndProviderReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	budget := NewSyncBudget(100)
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	now := time.Now().UTC()
	reset := now.Add(time.Minute)
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: reset})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker}, map[string]*SyncBudget{key: budget},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget"}

	allowed, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(allowed.Allowed)
	assert.True(IsSyncBudgetContext(allowed.Context))
	assert.True(IsArchiveSyncBudgetContext(allowed.Context))

	budget.Spend(100)
	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	require.NotNil(denied.RetryAt)
	assert.Equal(reset, *denied.RetryAt)

	budget.Reset()
	reserveTracker := NewPlatformRateTracker(database, "github", "reserve.test", "host", "rest")
	reserveTracker.UpdateFromRate(Rate{Limit: 5000, Remaining: RateReserveBuffer, Reset: reset})
	reserveRef := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "reserve.test", Owner: "acme", Name: "widget",
	}
	reserveKey := RateBucketKey("github", reserveRef.Host, "host")
	syncer.rateTrackers[reserveKey] = reserveTracker
	syncer.budgets[reserveKey] = NewSyncBudget(100)
	denied, err = syncer.Admit(t.Context(), reserveRef, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "reserve")

	syncer.running.Store(true)
	denied, err = syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "normal sync")
}

func TestGitHubArchiveAdmissionUsesCredentialRESTAndGraphQLPools(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Now().UTC()
	reset := now.Add(time.Minute)
	registry := NewQuotaRegistry()
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	appIdentity := IdentityKey{Host: "github.test", Principal: "installation:42"}
	userIdentity := IdentityKey{Host: "github.test", Principal: "user:7"}
	appBucket := RateBucketKey("github", "github.test", "installation:42")
	userBucket := RateBucketKey("github", "github.test", "user:7")
	syncer := NewSyncer(
		map[string]Client{"github.test": client},
		database, nil,
		[]RepoRef{
			{Platform: platform.KindGitHub, PlatformHost: "github.test", Owner: "acme", Name: "widget"},
			{Platform: platform.KindGitHub, PlatformHost: "github.test", Owner: "other", Name: "tool"},
		},
		time.Hour, nil,
		map[string]*SyncBudget{
			appBucket:  NewSyncBudget(100),
			userBucket: NewSyncBudget(100),
		},
	)
	router, err := NewHostRouter(
		"github.test",
		&Route{
			Key: RouteKey{Host: "github.test", Owner: "acme"}, Client: client,
			ReadIdentity: appIdentity, WriteIdentity: appIdentity,
		},
		&Route{
			Key: RouteKey{Host: "github.test", Owner: "other"}, Client: client,
			ReadIdentity: userIdentity, WriteIdentity: userIdentity,
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.test": router})
	syncer.SetQuotaRegistry(registry)
	syncer.now = func() time.Time { return now }
	appRef := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget",
	}
	userRef := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "other", Name: "tool",
	}
	registry.UpdateSnapshot(appIdentity, QuotaResourceREST,
		Rate{Limit: 15000, Remaining: 14000, Reset: reset})

	unknownGraphQL, err := syncer.Admit(
		t.Context(), appRef, db.ArchiveItemTypeIssue, 1,
	)
	require.NoError(err)
	assert.False(unknownGraphQL.Allowed)
	assert.Contains(unknownGraphQL.Detail, "provider quota unknown")

	registry.UpdateSnapshot(appIdentity, QuotaResourceGraphQL,
		Rate{Limit: 10000, Remaining: RateReserveBuffer, Reset: reset})
	appAtReserve, err := syncer.Admit(
		t.Context(), appRef, db.ArchiveItemTypeIssue, 1,
	)
	require.NoError(err)
	assert.False(appAtReserve.Allowed)
	assert.Contains(appAtReserve.Detail, "provider rate reserve")
	assert.Equal(reset, *appAtReserve.RetryAt)

	// The user credential has its own pools, so the App installation sitting
	// at its reserve must not hold back work routed to the user.
	registry.UpdateSnapshot(userIdentity, QuotaResourceREST,
		Rate{Limit: 5000, Remaining: 4900, Reset: reset})
	registry.UpdateSnapshot(userIdentity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4800, Reset: reset})
	userAllowed, err := syncer.Admit(
		t.Context(), userRef, db.ArchiveItemTypeIssue, 1,
	)
	require.NoError(err)
	require.True(userAllowed.Allowed)
	userAllowed.Complete(nil, false)
}

func TestGitHubArchiveAdmissionCapsAttemptAllowanceByProviderQuota(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	registry := NewQuotaRegistry()
	registry.now = func() time.Time { return now }
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	bucket := RateBucketKey("github", "github.test", "user:7")
	syncer := NewSyncer(
		map[string]Client{"github.test": client},
		database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.test",
			Owner: "acme", Name: "widget",
		}},
		time.Hour, nil,
		map[string]*SyncBudget{bucket: NewSyncBudget(100000)},
	)
	router, err := NewHostRouter("github.test", &Route{
		Key: RouteKey{Host: "github.test", Owner: "acme"}, Client: client,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.test": router})
	syncer.SetQuotaRegistry(registry)
	syncer.now = func() time.Time { return now }
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "acme", Name: "widget",
	}

	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, false) })
	// remaining 4500 minus the limit/5 archive reserve (1000).
	for range 3500 {
		assert.True(ConsumeArchiveAttemptAllowance(admission.Context))
	}
	assert.False(ConsumeArchiveAttemptAllowance(admission.Context))
}

func TestArchiveAdmissionAttemptAllowanceUsesAvailableSurplus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	budget := NewSyncBudget(100)
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	now := time.Now().UTC()
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute)})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker}, map[string]*SyncBudget{key: budget},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget"}

	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, true) })
	for range PRDetailWorstCase {
		assert.True(ConsumeArchiveAttemptAllowance(admission.Context))
	}
}

func TestGitealikeArchiveAdmissionDoesNotTruncateAdmittedMergeRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	budget := NewSyncBudget(50)
	giteaKey := RateBucketKey("gitea", "gitea.test", "host")
	giteaTracker := NewPlatformRateTracker(database, "gitea", "gitea.test", "host", "rest")
	giteaTracker.RecordRequest()
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{giteaKey: giteaTracker},
		map[string]*SyncBudget{giteaKey: budget},
	)
	giteaRef := platform.RepoRef{
		Platform: platform.KindGitea, Host: "gitea.test", Owner: "acme", Name: "widget",
	}

	admission, err := syncer.Admit(
		t.Context(), giteaRef, db.ArchiveItemTypeMergeRequest, 22,
	)
	require.NoError(err)
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, true) })
	for range 64 {
		assert.True(ConsumeArchiveAttemptAllowance(admission.Context))
	}

	githubKey := RateBucketKey("github", "github.test", "host")
	githubTracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	githubTracker.RecordRequest()
	githubSyncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{githubKey: githubTracker},
		map[string]*SyncBudget{githubKey: NewSyncBudget(50)},
	)
	githubRef := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget",
	}
	denied, err := githubSyncer.Admit(
		t.Context(), githubRef, db.ArchiveItemTypeMergeRequest, 1,
	)
	require.NoError(err)
	assert.False(denied.Allowed)
}

func TestArchiveAdmissionPreservesProviderReserveForDeclaredCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	now := time.Now().UTC()
	reset := now.Add(time.Minute)
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	// Every wire attempt, including an authentication retry, is counted
	// against admission, so archive.archiveAttemptCost declares twice the
	// logical request count. Remaining sits exactly at the reserve margin
	// for the undoubled logical cost (2): before that doubling, a request
	// declaring cost 2 would have been wrongly allowed here. The doubled
	// retry-headroom cost (4) must be denied to preserve the reserve.
	logicalCost := 2
	retryHeadroomCost := logicalCost * 2
	tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: RateReserveBuffer + logicalCost, Reset: reset,
	})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget"}

	denied, err := syncer.Admit(
		t.Context(), ref, db.ArchiveItemTypeIssue, retryHeadroomCost,
	)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "reserve")
	assert.Equal(reset, *denied.RetryAt)
}

func TestArchiveRampDenialRetriesWithinCurrentWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	now := time.Now().UTC()
	reset := now.Add(59 * time.Minute)
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: reset})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget"}

	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	require.NotNil(denied.RetryAt)
	assert.Equal(now.Add(time.Minute), *denied.RetryAt)
	assert.True(denied.RetryAt.Before(reset))
}

func TestArchiveAdmissionDefersToNotificationAndActiveDetailWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	now := time.Now().UTC()
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute)})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget"}

	for _, priority := range []archive.WorkPriority{
		archive.PriorityNotificationRefresh,
		archive.PriorityActiveDetail,
	} {
		release := syncer.beginProviderWork(key, priority)
		denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
		require.NoError(err)
		assert.False(denied.Allowed)
		assert.Contains(denied.Detail, "higher-priority sync work is active")
		release()
	}

	allowed, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.True(allowed.Allowed)
}

func TestArchiveAdmissionDenialAbandonsExpiredFeatureProbeReservation(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	key := RateBucketKey("github", "github.test", "host")
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		nil, map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget",
	}
	repo := RepoRef{
		Platform: ref.Platform, PlatformHost: ref.Host, Owner: ref.Owner, Name: ref.Name,
	}
	disabledErr := platform.RepositoryFeatureDisabled(
		platform.KindGitHub, ref.Host, platform.RepositoryFeatureIssues,
		errors.New("repository issues disabled"),
	)
	require.True(syncer.recordRepositoryFeatureDisabled(
		repo, platform.RepositoryFeatureIssues, disabledErr,
	))
	now = now.Add(repositoryFeatureProbeInterval)

	syncer.running.Store(true)
	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.False(denied.Allowed)
	syncer.running.Store(false)

	first, due := syncer.beginRepositoryFeatureProbe(
		t.Context(), repo, platform.RepositoryFeatureIssues,
	)
	require.True(due)
	defer first.release()
	_, due = syncer.beginRepositoryFeatureProbe(
		t.Context(), repo, platform.RepositoryFeatureIssues,
	)
	require.False(due)
}

func TestArchiveCompletionWithoutProviderAttemptAbandonsExpiredFeatureProbeReservation(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	key := RateBucketKey("github", "github.test", "host")
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	tracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 4999,
		Reset: now.Add(repositoryFeatureProbeInterval + time.Minute),
	})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget",
	}
	repo := RepoRef{
		Platform: ref.Platform, PlatformHost: ref.Host, Owner: ref.Owner, Name: ref.Name,
	}
	require.True(syncer.recordRepositoryFeatureDisabled(
		repo,
		platform.RepositoryFeatureIssues,
		platform.RepositoryFeatureDisabled(
			platform.KindGitHub, ref.Host, platform.RepositoryFeatureIssues,
			errors.New("repository issues disabled"),
		),
	))
	now = now.Add(repositoryFeatureProbeInterval)

	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(admission.Allowed, admission.Detail)
	result, syncErr := syncer.SyncArchiveItem(
		admission.Context, ref, db.ArchiveItemTypeIssue, 7,
	)
	require.Error(syncErr)
	require.False(result.ProviderAttempted)
	require.Nil(admission.Complete(syncErr, result.ProviderAttempted))

	first, due := syncer.beginRepositoryFeatureProbe(
		t.Context(), repo, platform.RepositoryFeatureIssues,
	)
	require.True(due)
	defer first.release()
	_, due = syncer.beginRepositoryFeatureProbe(
		t.Context(), repo, platform.RepositoryFeatureIssues,
	)
	require.False(due)
}

func TestArchiveAdmissionLeaseSerializesProviderRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	now := time.Now().UTC()
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute)})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "acme", Name: "widget",
	}

	first, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(first.Allowed)
	require.NotNil(first.Complete)

	second, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(second.Allowed)
	assert.Contains(second.Detail, "higher-priority sync work is active")

	first.Complete(nil, true)
	first.Complete(nil, true)
	third, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(third.Allowed)
	require.NotNil(third.Complete)
	third.Complete(nil, true)
}

func TestLiveProviderWorkCancelsAndWaitsForArchiveRequest(t *testing.T) {
	require := require.New(t)
	syncer := NewSyncerWithRegistry(nil, dbtest.Open(t), nil, nil, time.Hour, nil, nil)
	key := RateBucketKey("github", "github.test", "host")
	archiveCtx, releaseArchive, allowed := syncer.tryBeginArchiveProviderRequest(t.Context(), key)
	require.True(allowed)

	liveStarted := make(chan struct{})
	liveDone := make(chan struct{})
	go func() {
		releaseLive := syncer.beginProviderWork(key, archive.PriorityActiveDetail)
		close(liveStarted)
		releaseLive()
		close(liveDone)
	}()

	select {
	case <-archiveCtx.Done():
	case <-time.After(time.Second):
		require.Fail("live work did not cancel archive request")
	}
	select {
	case <-liveStarted:
		require.Fail("live work started before archive lease released")
	case <-time.After(25 * time.Millisecond):
	}

	releaseArchive()
	select {
	case <-liveDone:
	case <-time.After(time.Second):
		require.Fail("live work did not proceed after archive lease released")
	}
}

func TestBackfillMergedActorCancelsAndWaitsForArchiveRequest(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitLab, Host: "gitlab.test",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	provider := &blockingPriorityProvider{
		kind: ref.Platform, host: ref.Host,
		ref: ref, operation: priorityWorkMR,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	releaseProvider := sync.OnceFunc(func() { close(provider.release) })
	t.Cleanup(releaseProvider)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: string(ref.Platform), PlatformHost: ref.Host,
		PlatformRepoID: "repo-1", Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
	})
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7, PlatformExternalID: "mr-7", Number: 7,
		Title: "Synthetic MR", State: db.MergeRequestStateMerged,
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now, MergedAt: &now,
	})
	require.NoError(err)
	key := RateBucketKey(string(ref.Platform), ref.Host, "host")
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host, PlatformExternalID: "repo-1",
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}}, time.Hour, nil, map[string]*SyncBudget{key: NewSyncBudget(10)},
	)
	t.Cleanup(syncer.Stop)

	archiveCtx, releaseArchive, allowed := syncer.tryBeginArchiveProviderRequest(ctx, key)
	require.True(allowed)
	done := make(chan error, 1)
	go func() {
		_, backfillErr := syncer.BackfillMergedActorEventOnProvider(ctx, repoID, 7)
		done <- backfillErr
	}()

	select {
	case <-archiveCtx.Done():
	case <-provider.started:
		releaseProvider()
		releaseArchive()
		require.NoError(<-done)
		require.Fail("merged-actor backfill overlapped an active archive request")
	case <-time.After(time.Second):
		releaseArchive()
		require.Fail("merged-actor backfill did not cancel the active archive request")
	}
	select {
	case <-provider.started:
		require.Fail("merged-actor backfill started before the archive lease released")
	case <-time.After(25 * time.Millisecond):
	}
	releaseArchive()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		require.Fail("merged-actor backfill did not proceed after the archive lease released")
	}
	releaseProvider()
	require.NoError(<-done)
}

func TestArchiveAdmissionDefersToForegroundSyncEntryPoints(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation priorityWorkOperation
		run       func(context.Context, *Syncer, platform.RepoRef) error
	}{
		{
			name: "manual repository refresh", operation: priorityWorkIndex,
			run: func(ctx context.Context, syncer *Syncer, ref platform.RepoRef) error {
				return syncer.SyncRepoOnProvider(ctx, ref.Platform, ref.Host, ref.Owner, ref.Name)
			},
		},
		{
			name: "merge request detail", operation: priorityWorkMR,
			run: func(ctx context.Context, syncer *Syncer, ref platform.RepoRef) error {
				return syncer.SyncMROnProvider(ctx, ref.Platform, ref.Host, ref.Owner, ref.Name, 7)
			},
		},
		{
			name: "issue detail", operation: priorityWorkIssue,
			run: func(ctx context.Context, syncer *Syncer, ref platform.RepoRef) error {
				return syncer.SyncIssueOnProvider(ctx, ref.Platform, ref.Host, ref.Owner, ref.Name, 8)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := dbtest.Open(t)
			ref := platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.test",
				Owner: "acme", Name: "widget", RepoPath: "acme/widget",
			}
			provider := &blockingPriorityProvider{
				kind: ref.Platform, host: ref.Host,
				ref: ref, operation: tc.operation,
				started: make(chan struct{}), release: make(chan struct{}),
			}
			registry, err := platform.NewRegistry(provider)
			require.NoError(err)
			key := RateBucketKey(string(ref.Platform), ref.Host, "host")
			syncer := NewSyncerWithRegistry(
				registry, database, nil, []RepoRef{{
					Platform: ref.Platform, PlatformHost: ref.Host,
					Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
				}}, time.Hour, nil, map[string]*SyncBudget{key: NewSyncBudget(10)},
			)
			t.Cleanup(syncer.Stop)

			done := make(chan error, 1)
			go func() { done <- tc.run(t.Context(), syncer, ref) }()
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				require.Fail("foreground provider call did not start")
			}
			admission, err := syncer.Admit(
				t.Context(), ref, db.ArchiveItemTypeIssue, 1,
			)
			require.NoError(err)
			assert.False(admission.Allowed)
			assert.Contains(admission.Detail, "higher-priority sync work is active")
			close(provider.release)
			require.NoError(<-done)
		})
	}
}

func TestSyncRepoRegistersReadAndWriteIdentityProviderWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mc := &mockClient{getRepositoryFn: func(context.Context, string, string) (*gh.Repository, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, errors.New("stop after identity resolution")
	}}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, nil, time.Hour, nil, nil)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com", &Route{
		Key:           RouteKey{Host: "github.com", Owner: "acme"},
		Client:        mc,
		ReadIdentity:  IdentityKey{Host: "github.com", Principal: "installation:11"},
		WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:9"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget",
	}

	done := make(chan error, 1)
	go func() { done <- syncer.syncRepo(t.Context(), repo) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.Fail("repository sync did not reach the provider")
	}
	readBucket := RateBucketKey("github", "github.com", "installation:11")
	writeBucket := RateBucketKey("github", "github.com", "user:9")
	assert.True(
		syncer.higherPriorityProviderWorkActive(readBucket, archive.PriorityFullArchive),
		"read identity work must preempt archives during repository sync",
	)
	assert.True(
		syncer.higherPriorityProviderWorkActive(writeBucket, archive.PriorityFullArchive),
		"write identity work must preempt archives while the viewer permission overlay can run",
	)
	close(release)
	require.Error(<-done)
	assert.False(syncer.higherPriorityProviderWorkActive(readBucket, archive.PriorityFullArchive))
	assert.False(syncer.higherPriorityProviderWorkActive(writeBucket, archive.PriorityFullArchive))
}

func TestSyncNotificationsPreemptsArchivesForSplitAndReconciledIdentities(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	getRepoStarted := make(chan struct{})
	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	var getRepoOnce sync.Once
	var listOnce sync.Once
	mc := &mockClient{
		getRepositoryFn: func(context.Context, string, string) (*gh.Repository, error) {
			getRepoOnce.Do(func() { close(getRepoStarted) })
			owner := "acme"
			name := "widget"
			nodeID := "repo-new"
			id := int64(1)
			return &gh.Repository{
				ID: &id, NodeID: &nodeID, Owner: &gh.User{Login: &owner}, Name: &name,
			}, nil
		},
		listNotificationsFn: func(
			context.Context, NotificationListOptions,
		) ([]NotificationThread, bool, error) {
			listOnce.Do(func() { close(listStarted) })
			<-releaseList
			return nil, false, nil
		},
	}
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget",
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, database, nil,
		[]RepoRef{repo}, time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "installation:22"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:8"},
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "legacy"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "installation:11"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:9"},
		},
	)
	require.NoError(err)
	router.RegisterRepoCredentialAlias("acme", "widget", RouteKey{
		Host: "github.com", Owner: "legacy",
	}, "repo-old")
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	oldReadBucket := RateBucketKey("github", "github.com", "installation:11")
	oldWriteBucket := RateBucketKey("github", "github.com", "user:9")
	newReadBucket := RateBucketKey("github", "github.com", "installation:22")
	newWriteBucket := RateBucketKey("github", "github.com", "user:8")
	oldArchiveCtx, releaseOldArchive, allowed := syncer.tryBeginArchiveProviderRequest(
		t.Context(), oldReadBucket,
	)
	require.True(allowed)
	t.Cleanup(releaseOldArchive)
	newArchiveCtx, releaseNewArchive, allowed := syncer.tryBeginArchiveProviderRequest(
		t.Context(), newReadBucket,
	)
	require.True(allowed)
	t.Cleanup(releaseNewArchive)
	t.Cleanup(func() {
		select {
		case <-releaseList:
		default:
			close(releaseList)
		}
	})

	done := make(chan error, 1)
	go func() { done <- syncer.SyncNotifications(t.Context()) }()
	select {
	case <-oldArchiveCtx.Done():
	case <-time.After(time.Second):
		require.Fail("notification sync did not preempt the initial read identity archive")
	}
	select {
	case <-getRepoStarted:
		require.Fail("repository verification started before the initial read archive released")
	case <-time.After(25 * time.Millisecond):
	}
	releaseOldArchive()
	select {
	case <-getRepoStarted:
	case <-time.After(time.Second):
		require.Fail("notification sync did not begin repository verification")
	}
	select {
	case <-newArchiveCtx.Done():
	case <-time.After(time.Second):
		require.Fail("notification sync did not preempt the reconciled read identity archive")
	}
	select {
	case <-listStarted:
		require.Fail("notification listing started before the reconciled read archive released")
	case <-time.After(25 * time.Millisecond):
	}
	releaseNewArchive()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		require.Fail("notification sync did not begin listing notifications")
	}
	for bucket, label := range map[string]string{
		oldReadBucket: "initial read", oldWriteBucket: "initial write",
		newReadBucket: "reconciled read", newWriteBucket: "reconciled write",
	} {
		assert.True(
			syncer.higherPriorityProviderWorkActive(bucket, archive.PriorityFullArchive),
			label+" identity work must preempt archives during notification sync",
		)
	}
	close(releaseList)
	require.NoError(<-done)
	for _, bucket := range []string{oldReadBucket, oldWriteBucket, newReadBucket, newWriteBucket} {
		assert.False(syncer.higherPriorityProviderWorkActive(bucket, archive.PriorityFullArchive))
	}
}

func TestProcessQueuedNotificationReadsHoldsWriteIdentityProviderWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	queuedAt := now.Add(time.Minute)
	number := 7
	require.NoError(database.UpsertNotifications(t.Context(), []db.Notification{{
		Platform:                 "github",
		PlatformHost:             "github.com",
		PlatformNotificationID:   "thread-1",
		RepoID:                   &repoID,
		RepoOwner:                "acme",
		RepoName:                 "widget",
		SubjectType:              "PullRequest",
		SubjectTitle:             "Please review",
		WebURL:                   "https://github.com/acme/widget/pull/7",
		ItemNumber:               &number,
		ItemType:                 "pr",
		Reason:                   "mention",
		SourceUpdatedAt:          now,
		SyncedAt:                 now,
		SourceAckQueuedAt:        &queuedAt,
		SourceLastAcknowledgedAt: &queuedAt,
	}}))
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mc := &mockClient{
		getNotificationThreadFn: func(context.Context, string) (NotificationThread, error) {
			once.Do(func() { close(started) })
			<-release
			return NotificationThread{}, errors.New("refetch failed")
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, nil, time.Hour, nil, nil)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com", &Route{
		Key:           RouteKey{Host: "github.com", Owner: "acme"},
		Client:        mc,
		ReadIdentity:  IdentityKey{Host: "github.com", Principal: "installation:11"},
		WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:9"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	done := make(chan error, 1)
	go func() {
		done <- syncer.ProcessQueuedNotificationReads(
			t.Context(), platform.KindGitHub, "github.com", 10,
		)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.Fail("queued read propagation did not reach the provider")
	}
	writeBucket := RateBucketKey("github", "github.com", "user:9")
	assert.True(
		syncer.higherPriorityProviderWorkActive(writeBucket, archive.PriorityFullArchive),
		"queued acknowledgment propagation must preempt archives on the write identity",
	)
	close(release)
	require.NoError(<-done)
	assert.False(syncer.higherPriorityProviderWorkActive(writeBucket, archive.PriorityFullArchive))
}

func TestSyncerConfiguredRepositoriesCarryFullProviderIdentity(t *testing.T) {
	database := dbtest.Open(t)
	syncer := NewSyncerWithRegistry(nil, database, nil, []RepoRef{{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.test",
		Owner: "group/subgroup", Name: "project",
	}}, time.Hour, nil, nil)

	refs, err := syncer.ConfiguredRepositories(t.Context())
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, platform.RepoRef{
		Platform: platform.KindGitLab, Host: "gitlab.test",
		Owner: "group/subgroup", Name: "project", RepoPath: "group/subgroup/project",
	}, refs[0])
}

func TestArchiveAdmitNotDeferredByDisplacedRepositoryCooldown(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	key := RateBucketKey("github", "github.test", "host")
	tracker := NewPlatformRateTracker(database, "github", "github.test", "host", "rest")
	now := time.Now().UTC()
	tracker.UpdateFromRate(Rate{Limit: 5000, Remaining: 4999, Reset: now.Add(time.Minute)})
	syncer := NewSyncerWithRegistry(
		nil, database, nil, nil, time.Hour,
		map[string]*RateTracker{key: tracker},
		map[string]*SyncBudget{key: NewSyncBudget(100)},
	)
	syncer.now = func() time.Time { return now }
	displaced := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.test",
		Owner: "acme", Name: "widget", PlatformExternalID: "R_old",
	}
	syncer.featureCooldowns.deferUntil(
		displaced, platform.RepositoryFeatureIssues, now.Add(time.Hour),
	)

	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "acme", Name: "widget", PlatformExternalID: "R_new",
	}
	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.Nil(admission.FeatureDeferred,
		"a replacement repository must not inherit the displaced repository's cooldown")
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, true) })
}

func TestConfiguredRepositoriesCarryStableProviderIdentity(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := NewSyncer(
		nil, database, nil, []RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.test",
			Owner: "acme", Name: "widget", PlatformExternalID: "R_1",
		}}, time.Minute, nil, nil,
	)

	refs, err := syncer.ConfiguredRepositories(t.Context())
	require.NoError(err)
	require.Len(refs, 1)
	require.Equal("R_1", refs[0].PlatformExternalID,
		"archive scheduling needs the stable provider identity for cooldown keys")
}

func newProviderQuotaAdmissionSyncer(
	t *testing.T, now time.Time, budget *SyncBudget,
) (*Syncer, *QuotaRegistry, platform.RepoRef) {
	t.Helper()
	database := dbtest.Open(t)
	registry := NewQuotaRegistry()
	registry.now = func() time.Time { return now }
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	bucket := RateBucketKey("github", "github.test", "user:7")
	syncer := NewSyncer(
		map[string]Client{"github.test": client},
		database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.test",
			Owner: "acme", Name: "widget",
		}},
		time.Hour, nil,
		map[string]*SyncBudget{bucket: budget},
	)
	router, err := NewHostRouter("github.test", &Route{
		Key: RouteKey{Host: "github.test", Owner: "acme"}, Client: client,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(t, err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.test": router})
	syncer.SetQuotaRegistry(registry)
	syncer.now = func() time.Time { return now }
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "acme", Name: "widget",
	}
	return syncer, registry, ref
}

// A fresh provider window grants the full surplus above the archive reserve
// immediately: archive availability is a floor on remaining quota, not a ramp
// across the window.
func TestGitHubArchiveAdmissionGrantsFullSurplusAtWindowStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(59 * time.Minute)
	syncer, registry, ref := newProviderQuotaAdmissionSyncer(
		t, now, NewSyncBudget(100000),
	)
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})

	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 2)
	require.NoError(err)
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, false) })
	// remaining 4500 minus the limit/5 reserve (1000) is fully consumable.
	for range 3500 {
		assert.True(ConsumeArchiveAttemptAllowance(admission.Context))
	}
	assert.False(ConsumeArchiveAttemptAllowance(admission.Context))
}

// The local sync budget meters live sync only; provider-paced archive
// admission must not defer because live sync spent the configured hourly
// ceiling while provider quota still has surplus above the archive reserve.
func TestGitHubArchiveAdmissionIgnoresLocalSyncBudget(t *testing.T) {
	require := require.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	budget := NewSyncBudget(100)
	budget.Spend(100)
	syncer, registry, ref := newProviderQuotaAdmissionSyncer(t, now, budget)
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})

	admission, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	require.True(admission.Allowed)
	t.Cleanup(func() { admission.Complete(nil, false) })
}

// Remaining quota at or below the archive reserve defers admission until the
// provider window resets, even though the global rate reserve buffer still
// has headroom.
func TestGitHubArchiveAdmissionDefersAtArchiveReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	syncer, registry, ref := newProviderQuotaAdmissionSyncer(
		t, now, NewSyncBudget(100000),
	)
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 5000, Remaining: 1000, Reset: reset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4500, Reset: reset})

	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "provider rate reserve")
	require.NotNil(denied.RetryAt)
	assert.Equal(reset, *denied.RetryAt)
}

// A pool sitting at its own limit/5 reserve blocks admission even when the
// smallest-limit pool still has headroom: reserves are per pool, and the
// min-limit pool's reserve must not be applied to a larger pool.
func TestGitHubArchiveAdmissionHonorsEachPoolsOwnReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	syncer, registry, ref := newProviderQuotaAdmissionSyncer(
		t, now, NewSyncBudget(100000),
	)
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 15000, Remaining: 3000, Reset: reset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4800, Reset: reset})

	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "provider rate reserve")
	require.NotNil(denied.RetryAt)
	assert.Equal(reset, *denied.RetryAt)
}

// When admission defers on the archive reserve, the retry time comes from the
// pools that actually lack headroom. Waiting for the latest reset across all
// pools would leave archives paused after the exhausted pool has reset.
func TestGitHubArchiveAdmissionRetriesWhenDeficientPoolResets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	restReset := now.Add(10 * time.Minute)
	graphQLReset := now.Add(30 * time.Minute)
	syncer, registry, ref := newProviderQuotaAdmissionSyncer(
		t, now, NewSyncBudget(100000),
	)
	identity := IdentityKey{Host: "github.test", Principal: "user:7"}
	// REST sits at its own reserve and resets first; GraphQL has headroom
	// and resets later.
	registry.UpdateSnapshot(identity, QuotaResourceREST,
		Rate{Limit: 15000, Remaining: 3000, Reset: restReset})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL,
		Rate{Limit: 5000, Remaining: 4800, Reset: graphQLReset})

	denied, err := syncer.Admit(t.Context(), ref, db.ArchiveItemTypeIssue, 1)
	require.NoError(err)
	assert.False(denied.Allowed)
	assert.Contains(denied.Detail, "provider rate reserve")
	require.NotNil(denied.RetryAt)
	assert.Equal(restReset, *denied.RetryAt)
}

func requireEnsureConfigured(t *testing.T, s *archive.Service, refs []platform.RepoRef) {
	t.Helper()
	_, err := s.EnsureConfigured(t.Context(), refs)
	require.NoError(t, err)
}
