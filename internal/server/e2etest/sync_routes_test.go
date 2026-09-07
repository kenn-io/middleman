package e2etest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
	platformgitlab "go.kenn.io/forge/platform/gitlab"
)

func TestSyncRoutesWithoutProviderSyncerE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := dbtest.Open(t)
	srv := servertest.New(t, database, nil, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client, err := apiclient.NewWithHTTPClient(ts.URL, ts.Client())
	require.NoError(err)

	status, err := client.HTTP.GetSyncStatusWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, status.StatusCode(), string(status.Body))
	require.NotNil(status.JSON200)
	assert.False(status.JSON200.Running)
	assert.Nil(status.JSON200.LastRunAt)
	assert.Nil(status.JSON200.LastError)

	rates, err := client.HTTP.GetRateLimitsWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, rates.StatusCode(), string(rates.Body))
	require.NotNil(rates.JSON200)
	assert.Empty(rates.JSON200.ProviderPools)
	assert.Empty(rates.JSON200.LocalCeilings)

	trigger, err := client.HTTP.TriggerSyncWithResponse(
		t.Context(),
		nil,
		func(_ context.Context, req *http.Request) error {
			req.Header.Set("Content-Type", "application/json")
			return nil
		},
	)
	require.NoError(err)
	require.Equal(http.StatusServiceUnavailable, trigger.StatusCode(), string(trigger.Body))
	require.NotNil(trigger.ApplicationproblemJSONDefault)
	assert.Equal(generated.ServiceUnavailable, trigger.ApplicationproblemJSONDefault.Code)
	require.NotNil(trigger.ApplicationproblemJSONDefault.Detail)
	assert.Equal("syncer not configured", *trigger.ApplicationproblemJSONDefault.Detail)
}

// If an accepted queued sync reports completion before its provider pass,
// clients stop waiting while SQLite still holds the earlier snapshot.
func TestAcceptedFullSyncStaysRunningUntilQueuedProviderDataPersistsE2E(t *testing.T) {
	require := require.New(t)

	firstSnapshot := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var providerFresh atomic.Bool
	var listCalls atomic.Int32
	var statusMu sync.Mutex
	var runningStates []bool
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			nodeID := "repo-acme-widget"
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
		listOpenPullRequestsFn: func(
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
	}

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform:           platform.KindGitHub,
			Owner:              "acme",
			Name:               "widget",
			PlatformHost:       "github.com",
			PlatformExternalID: "repo-acme-widget",
		}},
		time.Minute,
		nil,
		nil,
	)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		syncer.Stop()
	})
	syncer.SetOnStatusChange(func(status *ghclient.SyncStatus) {
		statusMu.Lock()
		runningStates = append(runningStates, status.Running)
		statusMu.Unlock()
	})

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)
	api, err := apiclient.NewWithHTTPClient(forge.URL, forge.Client())
	require.NoError(err)

	triggerSync := func() {
		t.Helper()
		response, err := api.HTTP.TriggerSyncWithResponse(
			t.Context(),
			nil,
			func(_ context.Context, req *http.Request) error {
				req.Header.Set("Content-Type", "application/json")
				return nil
			},
		)
		require.NoError(err)
		require.Equal(http.StatusAccepted, response.StatusCode(), string(response.Body))
	}

	triggerSync()
	select {
	case <-firstSnapshot:
	case <-time.After(5 * time.Second):
		require.FailNow("initial sync did not reach the provider snapshot")
	}

	providerFresh.Store(true)
	triggerSync()
	releaseOnce.Do(func() { close(releaseFirst) })

	require.Eventually(func() bool {
		if listCalls.Load() != 2 {
			return false
		}
		status, err := api.HTTP.GetSyncStatusWithResponse(t.Context())
		if err != nil || status.StatusCode() != http.StatusOK ||
			status.JSON200 == nil || status.JSON200.Running {
			return false
		}
		repos, err := database.ListRepos(t.Context())
		return err == nil && len(repos) == 1 && repos[0].DefaultBranch == "fresh"
	}, 5*time.Second, 10*time.Millisecond,
		"accepted full sync did not persist the fresh provider snapshot")

	statusMu.Lock()
	states := append([]bool(nil), runningStates...)
	statusMu.Unlock()
	require.NotEmpty(states)
	terminalSeen := false
	for _, running := range states {
		if !running {
			terminalSeen = true
			continue
		}
		require.False(terminalSeen,
			"sync status returned to running after reporting completion")
	}
	require.False(states[len(states)-1])
}

// If an HTTP-scoped refresh loses its repository binding while coalescing
// with background work, unrelated repositories bypass their cadence gate.
func TestQueuedScopedHTTPRefreshKeepsBypassRepositoryBoundE2E(t *testing.T) {
	require := require.New(t)

	selectedEntered := make(chan struct{})
	releaseSelected := make(chan struct{})
	completions := make(chan struct{}, 2)
	var (
		phase               atomic.Int32
		selectedCalls       atomic.Int32
		unrelatedCalls      atomic.Int32
		releaseSelectedOnce sync.Once
	)
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			if repo == "unrelated" {
				id = 2
			}
			nodeID := "repo-acme-" + repo
			defaultBranch := "seed"
			if repo == "selected" {
				switch phase.Load() {
				case 1:
					defaultBranch = "active"
				case 2:
					defaultBranch = "queued"
				}
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
		listOpenPullRequestsFn: func(
			ctx context.Context, _, repo string,
		) ([]*gh.PullRequest, error) {
			switch repo {
			case "selected":
				if selectedCalls.Add(1) == 2 {
					close(selectedEntered)
					select {
					case <-releaseSelected:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			case "unrelated":
				unrelatedCalls.Add(1)
			}
			return []*gh.PullRequest{}, nil
		},
	}

	database := dbtest.Open(t)
	bucket := ghclient.RateBucketKey("github", "github.com", "host")
	repos := []ghclient.RepoRef{
		{
			Platform:           platform.KindGitHub,
			Owner:              "acme",
			Name:               "selected",
			PlatformHost:       "github.com",
			PlatformExternalID: "repo-acme-selected",
		},
		{
			Platform:           platform.KindGitHub,
			Owner:              "acme",
			Name:               "unrelated",
			PlatformHost:       "github.com",
			PlatformExternalID: "repo-acme-unrelated",
		},
	}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database,
		nil,
		repos,
		time.Hour,
		map[string]*ghclient.RateTracker{
			bucket: ghclient.NewRateTracker(database, "github.com", "host", "rest"),
		},
		nil,
	)
	syncer.SetParallelism(1)
	t.Cleanup(func() {
		releaseSelectedOnce.Do(func() { close(releaseSelected) })
		syncer.Stop()
	})

	// A completed background pass seeds the shared host cadence window.
	syncer.RunOnce(t.Context())
	require.Equal(int32(1), selectedCalls.Load())
	require.Equal(int32(1), unrelatedCalls.Load())
	syncer.SetOnSyncCompleted(func([]ghclient.RepoSyncResult) {
		completions <- struct{}{}
	})

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)
	api, err := apiclient.NewWithHTTPClient(forge.URL, forge.Client())
	require.NoError(err)
	onlySelected := []string{"github|github.com/acme/selected"}
	triggerSelected := func() {
		t.Helper()
		response, err := api.HTTP.TriggerSyncWithResponse(
			t.Context(),
			&generated.TriggerSyncParams{OnlyRepo: &onlySelected},
			func(_ context.Context, req *http.Request) error {
				req.Header.Set("Content-Type", "application/json")
				return nil
			},
		)
		require.NoError(err)
		require.Equal(http.StatusAccepted, response.StatusCode(), string(response.Body))
	}

	phase.Store(1)
	triggerSelected()
	select {
	case <-selectedEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("HTTP-scoped refresh did not reach the provider")
	}

	// Queue normal background work, then merge another HTTP-scoped refresh
	// while the first one still owns the single-flight slot.
	syncer.RunOnce(t.Context())
	triggerSelected()
	phase.Store(2)
	releaseSelectedOnce.Do(func() { close(releaseSelected) })

	for range 2 {
		select {
		case <-completions:
		case <-time.After(5 * time.Second):
			require.FailNow("coalesced sync passes did not complete")
		}
	}

	require.Equal(int32(3), selectedCalls.Load())
	require.Equal(int32(1), unrelatedCalls.Load())
	dbRepos, err := database.ListRepos(t.Context())
	require.NoError(err)
	require.Len(dbRepos, 2)
	branches := make(map[string]string, len(dbRepos))
	for _, repo := range dbRepos {
		branches[repo.Name] = repo.DefaultBranch
	}
	require.Equal("queued", branches["selected"])
	require.Equal("seed", branches["unrelated"])
}

func TestSyncListNotModifiedDoesNotChangeRateLimitBudgetE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var pulls304 atomic.Int32
	var issues304 atomic.Int32
	var forcePullRefresh atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		if forcePullRefresh.Swap(false) {
			r.Header.Del("If-None-Match")
		}
		writeGitHubListResponse(w, r, `"pulls-v1"`, &pulls304)
	})
	mux.HandleFunc("/api/v3/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		writeGitHubListResponse(w, r, `"issues-v1"`, &issues304)
	})
	githubAPI := httptest.NewServer(mux)
	defer githubAPI.Close()

	database := dbtest.Open(t)
	restTracker := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	// Leave one unit of reservation headroom so each conditional request can
	// reserve before I/O; a 304 refunds that unit for the next endpoint.
	budget := ghclient.NewSyncBudget(3)
	client, err := ghclient.NewClient(
		staticTokenSource("token"),
		"github.com",
		restTracker,
		budget,
		ghclient.WithBaseURLForTesting(githubAPI.URL),
	)
	require.NoError(err)

	registry, err := platform.NewRegistry(gitHubIndexListProvider{
		host:   "github.com",
		client: client,
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Owner: "acme", Name: "widget",
			PlatformHost:       "github.com",
			PlatformRepoID:     101,
			PlatformExternalID: "R_101",
		}},
		time.Minute,
		map[string]*ghclient.RateTracker{"github.com": restTracker},
		map[string]*ghclient.SyncBudget{"github.com": budget},
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	defer forge.Close()

	api, err := apiclient.NewWithHTTPClient(forge.URL, forge.Client())
	require.NoError(err)

	syncLastRunAt := func() *time.Time {
		t.Helper()
		resp, err := api.HTTP.GetSyncStatusWithResponse(t.Context())
		require.NoError(err)
		require.Equal(http.StatusOK, resp.StatusCode(), string(resp.Body))
		require.NotNil(resp.JSON200)
		return resp.JSON200.LastRunAt
	}
	triggerSync := func() {
		t.Helper()
		before := syncLastRunAt()
		resp, err := api.HTTP.TriggerSyncWithResponse(
			t.Context(),
			nil,
			func(_ context.Context, req *http.Request) error {
				req.Header.Set("Content-Type", "application/json")
				return nil
			},
		)
		require.NoError(err)
		require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
		require.Eventually(func() bool {
			resp, err := api.HTTP.GetSyncStatusWithResponse(t.Context())
			if err != nil || resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
				return false
			}
			if resp.JSON200.Running || resp.JSON200.LastRunAt == nil {
				return false
			}
			return before == nil || resp.JSON200.LastRunAt.After(*before)
		}, 5*time.Second, 10*time.Millisecond)
	}
	budgetSpent := func() int64 {
		t.Helper()
		resp, err := api.HTTP.GetRateLimitsWithResponse(t.Context())
		require.NoError(err)
		require.Equal(http.StatusOK, resp.StatusCode(), string(resp.Body))
		require.NotNil(resp.JSON200)
		ceiling, ok := resp.JSON200.LocalCeilings["github.com"]
		require.True(ok)
		return ceiling.Spent
	}

	triggerSync()
	firstSpent := budgetSpent()
	require.Equal(int64(2), firstSpent)

	triggerSync()
	assert.Equal(int32(1), pulls304.Load())
	assert.Equal(int32(1), issues304.Load())
	assert.Equal(firstSpent, budgetSpent())

	// Make the PR list consume the final budget unit so the following issue
	// list request is the one refused by the local ceiling.
	forcePullRefresh.Store(true)
	triggerSync()
	status, err := api.HTTP.GetSyncStatusWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, status.StatusCode(), string(status.Body))
	require.NotNil(status.JSON200)
	require.NotNil(status.JSON200.LastErrorCode)
	assert.Equal(generated.LocalSyncCeilingExhausted, *status.JSON200.LastErrorCode)

	rates, err := api.HTTP.GetRateLimitsWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, rates.StatusCode(), string(rates.Body))
	require.NotNil(rates.JSON200)
	ceiling, ok := rates.JSON200.LocalCeilings["github.com"]
	require.True(ok)
	resetAt, err := time.Parse(time.RFC3339, ceiling.ResetAt)
	require.NoError(err)
	assert.True(resetAt.After(time.Now().UTC()))
}

func TestSyncItemBudgetExhaustionIdentifiesLocalCeilingE2E(t *testing.T) {
	require := require.New(t)

	var commentRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v3/repos/acme/widget/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id": 7,
			"number": 7,
			"title": "Budget regression",
			"state": "open",
			"html_url": "https://example.com/acme/widget/issues/7",
			"body": "",
			"created_at": "2026-08-07T20:00:00Z",
			"updated_at": "2026-08-07T20:00:00Z"
		}]`))
	})
	mux.HandleFunc("/api/v3/repos/acme/widget/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		commentRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	githubAPI := httptest.NewServer(mux)
	defer githubAPI.Close()

	database := dbtest.Open(t)
	restTracker := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	budget := ghclient.NewSyncBudget(2)
	client, err := ghclient.NewClient(
		staticTokenSource("token"),
		"github.com",
		restTracker,
		budget,
		ghclient.WithBaseURLForTesting(githubAPI.URL),
	)
	require.NoError(err)

	registry, err := platform.NewRegistry(gitHubIndexListProvider{
		host:   "github.com",
		client: client,
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Owner: "acme", Name: "widget",
			PlatformHost:       "github.com",
			PlatformRepoID:     101,
			PlatformExternalID: "R_101",
		}},
		time.Minute,
		map[string]*ghclient.RateTracker{"github.com": restTracker},
		map[string]*ghclient.SyncBudget{"github.com": budget},
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	defer forge.Close()

	trigger, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, forge.URL+"/api/v1/sync", nil,
	)
	require.NoError(err)
	trigger.Header.Set("Content-Type", "application/json")
	triggerResponse, err := forge.Client().Do(trigger)
	require.NoError(err)
	defer triggerResponse.Body.Close()
	require.Equal(http.StatusAccepted, triggerResponse.StatusCode)

	require.Eventually(func() bool {
		return !syncer.Status().Running && !syncer.Status().LastRunAt.IsZero()
	}, 5*time.Second, 10*time.Millisecond)

	statusResponse, err := forge.Client().Get(forge.URL + "/api/v1/sync/status")
	require.NoError(err)
	defer statusResponse.Body.Close()
	require.Equal(http.StatusOK, statusResponse.StatusCode)
	var status struct {
		LastErrorCode           string `json:"last_error_code"`
		LastErrorCeilingKey     string `json:"last_error_ceiling_key"`
		LastErrorCeilingResetAt string `json:"last_error_ceiling_reset_at"`
	}
	require.NoError(json.NewDecoder(statusResponse.Body).Decode(&status))
	require.Equal("localSyncCeilingExhausted", status.LastErrorCode)
	require.Equal("github.com", status.LastErrorCeilingKey)
	require.Equal(budget.ResetAt().UTC().Format(time.RFC3339), status.LastErrorCeilingResetAt)
	require.Zero(commentRequests.Load(), "the refused request must not reach the provider")
}

func TestGitLabSyncBudgetExhaustionIncludesWindowE2E(t *testing.T) {
	require := require.New(t)
	var requests atomic.Int32
	gitlabAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(gitlabAPI.Close)

	budget := ghclient.NewSyncBudget(1)
	budget.Spend(1)
	client, err := platformgitlab.NewClient(
		"gitlab.example.com",
		staticTokenSource("token"),
		platformgitlab.WithBaseURLForTesting(gitlabAPI.URL+"/api/v4"),
		platformgitlab.WithoutRetriesForTesting(),
		platformgitlab.WithTransport(ghclient.WrapSyncBudgetTransport(http.DefaultTransport, budget)))
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(nil, client)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform:           platform.KindGitLab,
			PlatformHost:       "gitlab.example.com",
			PlatformRepoID:     42,
			PlatformExternalID: "42",
			Owner:              "group",
			Name:               "project",
			RepoPath:           "group/project",
		}},
		time.Minute,
		nil,
		map[string]*ghclient.SyncBudget{
			ghclient.RateBucketKey("gitlab", "gitlab.example.com", "host"): budget,
		},
	)
	t.Cleanup(syncer.Stop)
	syncer.RunOnce(t.Context())

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)
	api, err := apiclient.NewWithHTTPClient(forge.URL, forge.Client())
	require.NoError(err)
	status, err := api.HTTP.GetSyncStatusWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, status.StatusCode(), string(status.Body))
	require.NotNil(status.JSON200)
	require.NotNil(status.JSON200.LastErrorCode)
	require.Equal(generated.LocalSyncCeilingExhausted, *status.JSON200.LastErrorCode)
	require.NotNil(status.JSON200.LastErrorCeilingKey)
	require.Equal("gitlab:gitlab.example.com", *status.JSON200.LastErrorCeilingKey)
	require.NotNil(status.JSON200.LastErrorCeilingResetAt)
	require.Equal(
		budget.ResetAt().UTC().Format(time.RFC3339),
		status.JSON200.LastErrorCeilingResetAt.UTC().Format(time.RFC3339),
	)
	require.Zero(requests.Load(), "the refused request must not reach GitLab")
}

type gitHubIndexListProvider struct {
	host   string
	client ghclient.Client
}

func (p gitHubIndexListProvider) Platform() platform.Kind {
	return platform.KindGitHub
}

func (p gitHubIndexListProvider) Host() string {
	return p.host
}

func (p gitHubIndexListProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadMergeRequests: true,
		ReadIssues:        true,
	}
}

func (p gitHubIndexListProvider) ListOpenMergeRequests(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	pulls, err := p.client.ListOpenPullRequests(ctx, ref.Owner, ref.Name)
	if err != nil {
		return nil, err
	}
	out := make([]platform.MergeRequest, 0, len(pulls))
	for _, pull := range pulls {
		normalized, err := platformgithub.NormalizePullRequest(ref, pull)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

func (p gitHubIndexListProvider) GetMergeRequest(
	context.Context,
	platform.RepoRef,
	int,
) (platform.MergeRequest, error) {
	return platform.MergeRequest{}, platform.UnsupportedCapability(
		platform.KindGitHub,
		p.host,
		"read_merge_request_detail",
	)
}

func (p gitHubIndexListProvider) ListMergeRequestEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestEvent, error) {
	return nil, platform.UnsupportedCapability(
		platform.KindGitHub,
		p.host,
		"read_merge_request_events",
	)
}

func (p gitHubIndexListProvider) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	issues, err := p.client.ListOpenIssues(ctx, ref.Owner, ref.Name)
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

func (p gitHubIndexListProvider) GetIssue(
	context.Context,
	platform.RepoRef,
	int,
) (platform.Issue, error) {
	return platform.Issue{}, platform.UnsupportedCapability(
		platform.KindGitHub,
		p.host,
		"read_issue_detail",
	)
}

func (p gitHubIndexListProvider) ListIssueEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	_, err := p.client.ListIssueComments(ctx, ref.Owner, ref.Name, number)
	return nil, err
}

func writeGitHubListResponse(
	w http.ResponseWriter,
	r *http.Request,
	etag string,
	notModified *atomic.Int32,
) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4990")
	w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
	if r.Header.Get("If-None-Match") == etag {
		notModified.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	_, _ = w.Write([]byte(`[]`))
}
