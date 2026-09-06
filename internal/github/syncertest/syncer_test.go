package syncertest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t)
}

type mockClient struct {
	openPRs           []*gh.PullRequest
	listOpenPRsFn     func(context.Context, string, string) ([]*gh.PullRequest, error)
	listOpenPRsCalled bool
	getRepositoryFn   func(context.Context, string, string) (*gh.Repository, error)
}

func (m *mockClient) ListOpenPullRequests(
	ctx context.Context, owner, repo string,
) ([]*gh.PullRequest, error) {
	m.listOpenPRsCalled = true
	if m.listOpenPRsFn != nil {
		return m.listOpenPRsFn(ctx, owner, repo)
	}
	return m.openPRs, nil
}

func (m *mockClient) GetRepository(
	ctx context.Context, owner, repo string,
) (*gh.Repository, error) {
	if m.getRepositoryFn != nil {
		return m.getRepositoryFn(ctx, owner, repo)
	}
	id := int64(1)
	nodeID := "repo-" + owner + "-" + repo
	return &gh.Repository{
		ID:       &id,
		NodeID:   &nodeID,
		Name:     &repo,
		Owner:    &gh.User{Login: &owner},
		Archived: new(bool),
	}, nil
}

func (m *mockClient) GetPullRequest(context.Context, string, string, int) (*gh.PullRequest, error) {
	return nil, nil
}
func (m *mockClient) GetUser(context.Context, string) (*gh.User, error) { return nil, nil }
func (m *mockClient) ListRepositoriesByOwner(context.Context, string) ([]*gh.Repository, error) {
	return nil, nil
}
func (m *mockClient) ListReleases(context.Context, string, string, int) ([]*gh.RepositoryRelease, error) {
	return nil, nil
}
func (m *mockClient) ListTags(context.Context, string, string, int) ([]*gh.RepositoryTag, error) {
	return nil, nil
}
func (m *mockClient) ListOpenIssues(context.Context, string, string) ([]*gh.Issue, error) {
	return nil, nil
}
func (m *mockClient) GetIssue(context.Context, string, string, int) (*gh.Issue, error) {
	return nil, nil
}
func (m *mockClient) CreateIssue(context.Context, string, string, string, string) (*gh.Issue, error) {
	return nil, nil
}
func (m *mockClient) ListIssueComments(context.Context, string, string, int) ([]*gh.IssueComment, error) {
	return nil, nil
}
func (m *mockClient) ListIssueCommentsIfChanged(context.Context, string, string, int) ([]*gh.IssueComment, error) {
	return nil, nil
}
func (m *mockClient) ListReviews(context.Context, string, string, int) ([]*gh.PullRequestReview, error) {
	return nil, nil
}
func (m *mockClient) ListPullRequestReviewThreads(context.Context, string, string, int) ([]platformgithub.PullRequestReviewThread, error) {
	return nil, nil
}
func (m *mockClient) ListCommits(context.Context, string, string, int) ([]*gh.RepositoryCommit, error) {
	return nil, nil
}
func (m *mockClient) ListPullRequestTimelineEvents(context.Context, string, string, int) ([]platformgithub.PullRequestTimelineEvent, error) {
	return nil, nil
}
func (m *mockClient) ListForcePushEvents(context.Context, string, string, int) ([]platformgithub.ForcePushEvent, error) {
	return nil, nil
}
func (m *mockClient) GetCombinedStatus(context.Context, string, string, string) (*gh.CombinedStatus, error) {
	return nil, nil
}
func (m *mockClient) ListCheckRunsForRef(context.Context, string, string, string) ([]*gh.CheckRun, error) {
	return nil, nil
}
func (m *mockClient) ListWorkflowRunsForHeadSHA(context.Context, string, string, string) ([]*gh.WorkflowRun, error) {
	return nil, nil
}
func (m *mockClient) ApproveWorkflowRun(context.Context, string, string, int64) error {
	return nil
}
func (m *mockClient) CreateIssueComment(context.Context, string, string, int, string) (*gh.IssueComment, error) {
	return nil, nil
}
func (m *mockClient) EditIssueComment(context.Context, string, string, int64, string) (*gh.IssueComment, error) {
	return nil, nil
}
func (m *mockClient) DeleteIssueComment(context.Context, string, string, int64) error {
	return nil
}
func (m *mockClient) CreatePullRequestReviewCommentReply(
	context.Context, string, string, int, string, int64,
) (*gh.PullRequestComment, error) {
	return nil, nil
}
func (m *mockClient) CreateReview(context.Context, string, string, int, string, string) (*gh.PullRequestReview, error) {
	return nil, nil
}
func (m *mockClient) CreateReviewWithComments(
	context.Context,
	string,
	string,
	int,
	string,
	string,
	string,
	[]*gh.DraftReviewComment,
) (*gh.PullRequestReview, error) {
	return nil, nil
}
func (m *mockClient) ApplyReviewSuggestions(
	_ context.Context,
	_ string,
	_ string,
	_ int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	return nil, nil
}
func (m *mockClient) MarkPullRequestReadyForReview(context.Context, string, string, int) (*gh.PullRequest, error) {
	return nil, nil
}
func (m *mockClient) ConvertPullRequestToDraft(context.Context, string, string, int) (*gh.PullRequest, error) {
	return nil, nil
}
func (m *mockClient) DismissReview(context.Context, string, string, int, int64, string) (*gh.PullRequestReview, error) {
	return nil, nil
}
func (m *mockClient) MergePullRequest(context.Context, string, string, int, string, string, string, string) (*gh.PullRequestMergeResult, error) {
	return nil, nil
}
func (m *mockClient) EditPullRequest(context.Context, string, string, int, platformgithub.EditPullRequestOpts) (*gh.PullRequest, error) {
	return nil, nil
}
func (m *mockClient) EditIssue(context.Context, string, string, int, string) (*gh.Issue, error) {
	return nil, nil
}
func (m *mockClient) EditIssueContent(context.Context, string, string, int, *string, *string) (*gh.Issue, error) {
	return nil, nil
}
func (m *mockClient) ListPullRequestsPage(context.Context, string, string, string, int) ([]*gh.PullRequest, bool, error) {
	return nil, false, nil
}
func (m *mockClient) ListIssuesPage(context.Context, string, string, string, int) ([]*gh.Issue, bool, error) {
	return nil, false, nil
}
func (m *mockClient) ListNotifications(context.Context, ghclient.NotificationListOptions) ([]ghclient.NotificationThread, bool, error) {
	return nil, false, nil
}
func (m *mockClient) MarkNotificationThreadRead(context.Context, string) error { return nil }
func (m *mockClient) InvalidateListETagsForRepo(string, string, ...string)     {}

func TestSyncerStopIsIdempotent(t *testing.T) {
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockClient{}},
		nil, nil, nil, time.Minute, nil, nil,
	)
	syncer.Stop()
	syncer.Stop()
}

type blockingMockClient struct {
	mockClient
	entered chan struct{}
	blocked chan struct{}
}

func (b *blockingMockClient) ListOpenPullRequests(
	_ context.Context, _, _ string,
) ([]*gh.PullRequest, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	<-b.blocked
	return nil, nil
}

func TestSyncerStopWaitsForRunOnce(t *testing.T) {
	entered := make(chan struct{})
	blocked := make(chan struct{})
	mock := &blockingMockClient{
		entered: entered,
		blocked: blocked,
	}

	database := openTestDB(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock}, database, nil,
		[]ghclient.RepoRef{{
			Owner:              "o",
			Name:               "r",
			PlatformHost:       "github.com",
			PlatformExternalID: "repo-o-r",
		}},
		time.Hour, nil, nil,
	)

	syncer.Start(t.Context())
	<-entered

	stopped := make(chan struct{})
	go func() {
		syncer.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		require.Fail(t, "Stop returned while RunOnce was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(blocked)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Stop did not return within timeout")
	}
}

type parallelMockClient struct {
	mockClient
	inflight         atomic.Int32
	maxInflight      atomic.Int32
	saturationTarget int32
	saturated        chan struct{}
	saturatedOnce    sync.Once
	block            chan struct{}
}

func (p *parallelMockClient) ListOpenPullRequests(
	_ context.Context, _, _ string,
) ([]*gh.PullRequest, error) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		current := p.maxInflight.Load()
		if n <= current || p.maxInflight.CompareAndSwap(current, n) {
			break
		}
	}
	if n == p.saturationTarget && p.saturated != nil {
		p.saturatedOnce.Do(func() { close(p.saturated) })
	}
	<-p.block
	return nil, nil
}

func TestRunOnceSyncesReposInParallel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	const parallelism = 3
	const repoCount = 5

	mc := &parallelMockClient{
		block:            make(chan struct{}),
		saturated:        make(chan struct{}),
		saturationTarget: parallelism,
	}
	repos := make([]ghclient.RepoRef, repoCount)
	for i := range repos {
		repos[i] = ghclient.RepoRef{
			Owner:        "o",
			Name:         fmt.Sprintf("r%d", i),
			PlatformHost: "github.com",
		}
	}

	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mc}, d, nil, repos,
		time.Minute, nil, nil,
	)
	syncer.SetParallelism(parallelism)

	done := make(chan struct{})
	go func() {
		syncer.RunOnce(t.Context())
		close(done)
	}()

	select {
	case <-mc.saturated:
	case <-time.After(10 * time.Second):
		require.Failf(
			"expected worker pool to saturate",
			"expected %d concurrent syncs, got %d",
			parallelism, mc.inflight.Load(),
		)
	}
	assert.LessOrEqual(mc.maxInflight.Load(), int32(parallelism),
		"max concurrency exceeded bound")

	close(mc.block)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail("RunOnce did not complete after unblocking workers")
	}

	assert.Equal(int32(parallelism), mc.maxInflight.Load(),
		"should have reached the parallelism bound exactly")
}

func TestRunOnceCancelDuringBackoffDoesNotReportSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	rt := ghclient.NewRateTracker(d, "github.com", "host", "rest")
	resetAt := time.Now().Add(time.Hour)
	rt.UpdateFromRate(ghclient.Rate{
		Remaining: 0,
		Reset:     resetAt,
	})

	mc := &mockClient{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mc}, d, nil,
		[]ghclient.RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*ghclient.RateTracker{"github.com": rt}, nil,
	)

	var completedCalled atomic.Bool
	syncer.SetOnSyncCompleted(func([]ghclient.RepoSyncResult) {
		completedCalled.Store(true)
	})
	backoffReached := make(chan struct{})
	var backoffReachedOnce sync.Once
	syncer.SetOnStatusChange(func(status *ghclient.SyncStatus) {
		if strings.Contains(status.Progress, "rate limited, waiting") {
			backoffReachedOnce.Do(func() { close(backoffReached) })
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		syncer.RunOnce(ctx)
		close(done)
	}()

	select {
	case <-backoffReached:
	case <-time.After(2 * time.Second):
		require.Fail("RunOnce did not reach rate-limit backoff")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail("RunOnce did not return after ctx cancel")
	}

	assert.False(completedCalled.Load(),
		"onSyncCompleted must not fire when RunOnce is canceled")
	status := syncer.Status()
	assert.False(status.Running)
	assert.NotEmpty(status.LastError,
		"LastError should reflect the cancellation")
}

func TestRunOnceCancelAfterCompleteReportsSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &mockClient{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mc}, d, nil,
		[]ghclient.RepoRef{}, time.Minute, nil, nil,
	)

	var completedCalled atomic.Bool
	syncer.SetOnSyncCompleted(func([]ghclient.RepoSyncResult) {
		completedCalled.Store(true)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		syncer.RunOnce(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail("RunOnce did not return")
	}

	assert.True(completedCalled.Load(),
		"onSyncCompleted should fire when no work was outstanding "+
			"at cancel time")
	status := syncer.Status()
	assert.False(status.Running)
	assert.Empty(status.LastError,
		"LastError should be empty when all work completed before cancel")
}

type cancelDuringSyncMockClient struct {
	mockClient
	entered chan struct{}
}

func (c *cancelDuringSyncMockClient) GetRepository(
	ctx context.Context, _, _ string,
) (*gh.Repository, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunOnceCancelDuringSyncRepoDoesNotReportSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &cancelDuringSyncMockClient{
		entered: make(chan struct{}, 1),
	}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mc}, d, nil,
		[]ghclient.RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	var completedCalled atomic.Bool
	syncer.SetOnSyncCompleted(func([]ghclient.RepoSyncResult) {
		completedCalled.Store(true)
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		syncer.RunOnce(ctx)
		close(done)
	}()

	select {
	case <-mc.entered:
	case <-time.After(2 * time.Second):
		require.Fail("worker did not enter GetRepository")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail("RunOnce did not return")
	}

	assert.False(completedCalled.Load(),
		"onSyncCompleted must not fire when syncRepo was canceled "+
			"mid-flight")
	status := syncer.Status()
	assert.False(status.Running)
	assert.NotEmpty(status.LastError,
		"LastError should reflect the cancellation")
}

type deadlineExceededMockClient struct {
	mockClient
}

func (c *deadlineExceededMockClient) ListOpenPullRequests(
	_ context.Context, _, _ string,
) ([]*gh.PullRequest, error) {
	return nil, fmt.Errorf("list timed out: %w", context.DeadlineExceeded)
}

func TestRunOncePerRequestDeadlineRecordsError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &deadlineExceededMockClient{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mc}, d, nil,
		[]ghclient.RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	var completedCalled atomic.Bool
	syncer.SetOnSyncCompleted(func([]ghclient.RepoSyncResult) {
		completedCalled.Store(true)
	})

	syncer.RunOnce(t.Context())

	status := syncer.Status()
	assert.False(status.Running)
	assert.NotEmpty(status.LastError,
		"per-request DeadlineExceeded should be recorded in LastError")
	assert.Contains(status.LastError, "list timed out",
		"LastError should preserve the wrapped error message")
	require.True(completedCalled.Load(),
		"onSyncCompleted should fire on a finished run with errors")
}

type syncedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncedWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

func TestRunOnceDispatchHonorsCanceledCtx(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	var buf bytes.Buffer
	sw := &syncedWriter{w: &buf}
	h := slog.NewTextHandler(sw, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	orig := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(orig) })

	repos := make([]ghclient.RepoRef, 100)
	for i := range repos {
		repos[i] = ghclient.RepoRef{
			Owner:        "o",
			Name:         fmt.Sprintf("r%d", i),
			PlatformHost: "github.com",
		}
	}

	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockClient{}}, d, nil,
		repos, time.Minute, nil, nil,
	)
	syncer.SetParallelism(4)

	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		syncer.RunOnce(ctx)
	}

	sw.mu.Lock()
	output := buf.String()
	sw.mu.Unlock()

	count := strings.Count(output, `msg="syncing repo"`)
	assert.Zero(count,
		"dispatch must not enqueue repos when ctx is pre-canceled "+
			"(observed %d 'syncing repo' log lines)", count)
}

func TestSyncerTriggerRunRunsRunOnce(t *testing.T) {
	assert := assert.New(t)
	mock := &mockClient{openPRs: []*gh.PullRequest{}}
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), db.GitHubRepoIdentity("github.com", "o", "n"))
	require.NoError(t, err)
	repos := []ghclient.RepoRef{{Owner: "o", Name: "n", PlatformHost: "github.com"}}
	s := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		d, nil, repos, time.Hour, nil, nil,
	)

	done := make(chan struct{}, 1)
	s.SetOnStatusChange(func(status *ghclient.SyncStatus) {
		if !status.Running {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})

	s.TriggerRun(t.Context())

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond,
		"TriggerRun did not complete RunOnce")
	s.Stop()
	assert.True(mock.listOpenPRsCalled,
		"TriggerRun should invoke ListOpenPullRequests")
}

func TestSyncerTriggerRunWithPrioritySyncsSelectedReposFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const completionTimeout = 30 * time.Second

	var mu sync.Mutex
	var calls []string
	mock := &mockClient{
		openPRs: []*gh.PullRequest{},
		listOpenPRsFn: func(
			_ context.Context, owner, repo string,
		) ([]*gh.PullRequest, error) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, owner+"/"+repo)
			return []*gh.PullRequest{}, nil
		},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			ids := map[string]int64{
				"first":  1,
				"second": 2,
				"third":  3,
			}
			id := ids[repo]
			nodeID := "repo-" + owner + "-" + repo
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: new(bool),
			}, nil
		},
	}
	d := openTestDB(t)
	repos := []ghclient.RepoRef{
		{Owner: "o", Name: "first", PlatformHost: "github.com"},
		{Owner: "o", Name: "second", PlatformHost: "github.com"},
		{Owner: "o", Name: "third", PlatformHost: "github.com"},
	}
	s := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		d, nil, repos, time.Hour, nil, nil,
	)
	s.SetParallelism(1)

	done := make(chan struct{}, 1)
	s.SetOnStatusChange(func(status *ghclient.SyncStatus) {
		if !status.Running {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})

	s.TriggerRunWithPriority(t.Context(), []ghclient.RepoRef{{
		Owner:        "o",
		Name:         "third",
		PlatformHost: "github.com",
	}})

	select {
	case <-done:
	case <-time.After(completionTimeout):
		require.FailNowf(
			"priority TriggerRun did not complete",
			"timeout=%s",
			completionTimeout,
		)
	}
	s.Stop()

	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	assert.Equal([]string{"o/third", "o/first", "o/second"}, got)
}

func TestSyncerTriggerRunForReposSyncsOnlySelectedRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	var calls []string
	mock := &mockClient{
		openPRs: []*gh.PullRequest{},
		listOpenPRsFn: func(
			_ context.Context, owner, repo string,
		) ([]*gh.PullRequest, error) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, owner+"/"+repo)
			return []*gh.PullRequest{}, nil
		},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			ids := map[string]int64{
				"first":  1,
				"second": 2,
				"third":  3,
			}
			id := ids[repo]
			nodeID := "repo-" + owner + "-" + repo
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: new(bool),
			}, nil
		},
	}
	d := openTestDB(t)
	repos := []ghclient.RepoRef{
		{Owner: "o", Name: "first", PlatformHost: "github.com"},
		{Owner: "o", Name: "second", PlatformHost: "github.com"},
		{Owner: "o", Name: "third", PlatformHost: "github.com"},
	}
	s := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		d, nil, repos, time.Hour, nil, nil,
	)
	s.SetParallelism(1)

	done := make(chan struct{}, 1)
	s.SetOnStatusChange(func(status *ghclient.SyncStatus) {
		if !status.Running {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})

	s.TriggerRunForRepos(t.Context(), []ghclient.RepoRef{{
		Owner:        "o",
		Name:         "third",
		PlatformHost: "github.com",
	}})

	require.Eventually(func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond,
		"scoped TriggerRun did not complete")
	s.Stop()

	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	assert.Equal([]string{"o/third"}, got)
}

// If an accepted trigger is dropped behind an in-flight provider snapshot,
// data changed after that snapshot stays stale until an unrelated later run.
func TestSyncerAcceptedTriggerQueuesBehindInFlightRun(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(*ghclient.Syncer, context.Context, []ghclient.RepoRef)
	}{
		{
			name: "full",
			trigger: func(syncer *ghclient.Syncer, ctx context.Context, _ []ghclient.RepoRef) {
				syncer.TriggerRun(ctx)
			},
		},
		{
			name: "priority",
			trigger: func(syncer *ghclient.Syncer, ctx context.Context, repos []ghclient.RepoRef) {
				syncer.TriggerRunWithPriority(ctx, repos)
			},
		},
		{
			name: "scoped",
			trigger: func(syncer *ghclient.Syncer, ctx context.Context, repos []ghclient.RepoRef) {
				syncer.TriggerRunForRepos(ctx, repos)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			database := openTestDB(t)
			ctx := t.Context()
			repos := []ghclient.RepoRef{{
				Owner:              "o",
				Name:               "r",
				PlatformHost:       "github.com",
				PlatformExternalID: "repo-o-r",
			}}
			repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
				Platform:       "github",
				PlatformHost:   "github.com",
				PlatformRepoID: "repo-o-r",
				Owner:          "o",
				Name:           "r",
			})
			require.NoError(err)

			firstSnapshot := make(chan struct{})
			releaseFirst := make(chan struct{})
			var releaseOnce sync.Once
			var listCalls atomic.Int32
			var providerFresh atomic.Bool
			mock := &mockClient{
				listOpenPRsFn: func(
					ctx context.Context, _, _ string,
				) ([]*gh.PullRequest, error) {
					if listCalls.Add(1) == 1 {
						close(firstSnapshot)
						select {
						case <-releaseFirst:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return []*gh.PullRequest{}, nil
				},
				getRepositoryFn: func(
					_ context.Context, owner, repo string,
				) (*gh.Repository, error) {
					id := int64(1)
					nodeID := "repo-o-r"
					defaultBranch := "stale"
					if providerFresh.Load() {
						defaultBranch = "fresh"
					}
					return &gh.Repository{
						ID:            &id,
						NodeID:        &nodeID,
						Name:          &repo,
						Owner:         &gh.User{Login: &owner},
						Archived:      new(bool),
						DefaultBranch: &defaultBranch,
					}, nil
				},
			}
			syncer := ghclient.NewSyncer(
				map[string]ghclient.Client{"github.com": mock},
				database, nil, repos, time.Hour, nil, nil,
			)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseFirst) })
				syncer.Stop()
			})

			syncer.TriggerRun(ctx)
			select {
			case <-firstSnapshot:
			case <-time.After(5 * time.Second):
				require.FailNow("initial sync did not reach the provider snapshot")
			}

			providerFresh.Store(true)
			tt.trigger(syncer, ctx, repos)
			releaseOnce.Do(func() { close(releaseFirst) })

			require.Eventually(
				func() bool { return listCalls.Load() == 2 },
				5*time.Second,
				10*time.Millisecond,
				"accepted trigger did not run after the active sync",
			)
			syncer.Stop()

			stored, err := database.GetRepoByID(ctx, repoID)
			require.NoError(err)
			require.NotNil(stored)
			require.Equal("fresh", stored.DefaultBranch)
		})
	}
}

type blockingCtxMockClient struct {
	mockClient
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCtxMockClient) ListOpenPullRequests(
	ctx context.Context, _, _ string,
) ([]*gh.PullRequest, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	select {
	case <-b.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSyncerStopCancelsTriggerRun(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mock := &blockingCtxMockClient{
		entered: entered,
		release: release,
	}

	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		d, nil,
		[]ghclient.RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Hour, nil, nil,
	)

	syncer.TriggerRun(context.WithoutCancel(t.Context()))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow("TriggerRun did not start ListOpenPullRequests")
	}

	stopped := make(chan struct{})
	go func() {
		syncer.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		close(release)
		require.FailNow("Stop did not return after ctx cancellation")
	}
}

var _ ghclient.Client = (*mockClient)(nil)
