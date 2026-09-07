package e2etest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

type archiveFeatureClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *archiveFeatureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *archiveFeatureClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestArchiveAPIRecoversWhenGitHubIssuesAreReenabledE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	clock := &archiveFeatureClock{
		now: time.Now().UTC().Truncate(time.Second),
	}
	issueTimestamp := clock.Now().Add(30 * time.Second).Format(time.RFC3339)
	var historicalIssueListCalls atomic.Int32
	var updatedIssueListCalls atomic.Int32
	var issueDetailCalls atomic.Int32
	updatedIssueStarted := make(chan struct{})
	releaseUpdatedIssue := make(chan struct{})
	updatedIssueSucceeded := make(chan struct{})
	var releaseUpdatedIssueOnce sync.Once
	var updatedIssueStartedOnce sync.Once
	var updatedIssueSucceededOnce sync.Once
	t.Cleanup(func() {
		releaseUpdatedIssueOnce.Do(func() { close(releaseUpdatedIssue) })
	})

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/graphql":
			var request struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, `{"message":"invalid GraphQL request"}`, http.StatusBadRequest)
				return
			}
			if strings.Contains(request.Query, "timelineItems") {
				_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
				return
			}
			if !strings.Contains(request.Query, "issues(first: 100") {
				http.Error(w, `{"message":"unexpected GraphQL query"}`, http.StatusBadRequest)
				return
			}
			orderField, _ := request.Variables["orderField"].(string)
			switch orderField {
			case "CREATED_AT":
				if historicalIssueListCalls.Add(1) == 1 {
					w.WriteHeader(http.StatusGone)
					_, _ = w.Write([]byte(`{"message":"Issues are disabled for this repo"}`))
					return
				}
			case "UPDATED_AT":
				updatedIssueListCalls.Add(1)
				updatedIssueStartedOnce.Do(func() { close(updatedIssueStarted) })
				<-releaseUpdatedIssue
			default:
				http.Error(w, `{"message":"unexpected issue order"}`, http.StatusBadRequest)
				return
			}
			_, writeErr := w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{
				"id":"I_17","databaseId":17,"number":17,"title":"enabled issue",
				"state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/17",
				"author":{"login":"author"},"createdAt":"` + issueTimestamp + `",
				"updatedAt":"` + issueTimestamp + `","closedAt":"` + issueTimestamp + `",
				"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}
			}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
			if orderField == "UPDATED_AT" && writeErr == nil {
				updatedIssueSucceededOnce.Do(func() { close(updatedIssueSucceeded) })
			}
		case "/api/v3/repos/acme/widget/pulls":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case "/api/v3/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4999,"reset":4102444800}}}`))
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{
				"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget",
				"owner":{"login":"acme"},"has_pull_requests":false
			}`))
		case "/api/v3/repos/acme/widget/issues/17":
			issueDetailCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id":17,"node_id":"I_17","number":17,"title":"enabled issue","state":"closed",
				"body":"","html_url":"https://github.com/acme/widget/issues/17",
				"user":{"login":"author"},"created_at":"` + issueTimestamp + `",
				"updated_at":"` + issueTimestamp + `","closed_at":"` + issueTimestamp + `",
				"comments":0
			}`))
		case "/api/v3/repos/acme/widget/issues/17/comments":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(providerServer.Close)

	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "R_widget",
	}
	quotaRegistry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.com", Principal: "user:7"}
	rateKey := ghclient.RateBucketKey("github", ref.Host, identity.Principal)
	budget := ghclient.NewSyncBudget(5000)
	providerClient, err := ghclient.NewClient(
		staticTokenSource("archive-token"), "github.com", nil, nil,
		ghclient.WithBaseURLForTesting(providerServer.URL),
		ghclient.WithQuotaAccounting(quotaRegistry, identity, identity),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(map[string]ghclient.Client{
		"github.com": providerClient,
	})
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, nil, time.Hour,
		nil, map[string]*ghclient.SyncBudget{rateKey: budget},
	)
	router, err := ghclient.NewHostRouter(ref.Host, &ghclient.Route{
		Key: ghclient.RouteKey{Host: ref.Host, Owner: ref.Owner}, Client: providerClient,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{ref.Host: router})
	syncer.SetQuotaRegistry(quotaRegistry)
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceREST, ghclient.Rate{
		Limit: 5000, Remaining: 4999, Reset: clock.Now().Add(time.Hour),
	})
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL, ghclient.Rate{
		Limit: 5000, Remaining: 4999, Reset: clock.Now().Add(time.Hour),
	})
	syncer.SetClock(clock.Now)
	archiveService, err := archive.NewService(
		database, registry, syncer, syncer, nil, clock,
	)
	require.NoError(err)
	archiveService.SetMaintenanceInterval(time.Second)
	archiveService.SetWake(syncer.WakeArchive)
	syncer.SetArchiveService(archiveService)
	syncer.SetArchivePollIntervalForTesting(time.Millisecond)

	repo := ghclient.RepoRef{
		Platform: ref.Platform, PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		PlatformExternalID: ref.PlatformExternalID,
	}
	require.NoError(syncer.SetReposWithContext(ctx, []ghclient.RepoRef{repo}, false))

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		Archive:                       archiveService,
		HostCheckAllowLoopbackAnyPort: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	forgeServer := httptest.NewServer(srv)
	t.Cleanup(forgeServer.Close)
	api, err := apiclient.NewWithHTTPClient(forgeServer.URL, forgeServer.Client())
	require.NoError(err)
	repositories := []generated.ArchiveRepositoryRef{{
		Provider: "github", PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
	}}
	started, err := api.HTTP.StartArchivesWithResponse(
		ctx, generated.ArchiveMutationBody{Repositories: &repositories},
	)
	require.NoError(err)
	require.NotNil(started.JSON200)

	// Start the worker only after the API has promoted the repository to full
	// archive mode. This makes the first CREATED_AT request unambiguously part
	// of the full initial scan rather than discovery work woken by SetRepos.
	syncer.Start(ctx)
	t.Cleanup(syncer.Stop)

	storedRepo, err := database.GetRepoByIdentity(ctx, platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(storedRepo)

	select {
	case <-updatedIssueStarted:
	case <-time.After(3 * time.Second):
		require.Fail("maintenance did not issue an UPDATED_AT request")
	}
	var unsupportedGeneration int64
	require.Eventually(func() bool {
		states, stateErr := database.ListArchiveRepoStates(ctx, []int64{storedRepo.ID})
		ready := stateErr == nil && len(states) == 1 &&
			historicalIssueListCalls.Load() >= 1 &&
			states[0].IssuesCoverage == db.ArchiveCoverageUnsupported &&
			states[0].MergeRequestsCoverage == db.ArchiveCoverageUnsupported &&
			states[0].IssueInventory.Complete() && states[0].InitialCompletedAt != nil
		if ready {
			unsupportedGeneration = states[0].IssueInventory.Generation
		}
		return ready
	}, 3*time.Second, 5*time.Millisecond)
	clock.Advance(time.Minute)
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceREST, ghclient.Rate{
		Limit: 5000, Remaining: 4999, Reset: clock.Now().Add(time.Hour),
	})
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL, ghclient.Rate{
		Limit: 5000, Remaining: 4999, Reset: clock.Now().Add(time.Hour),
	})
	releaseUpdatedIssueOnce.Do(func() { close(releaseUpdatedIssue) })
	syncer.WakeArchive()
	select {
	case <-updatedIssueSucceeded:
	case <-time.After(3 * time.Second):
		require.Fail("UPDATED_AT maintenance request did not succeed")
	}

	require.Eventually(func() bool {
		stored, getErr := database.GetIssueByRepoIDAndNumber(ctx, storedRepo.ID, 17)
		return getErr == nil && stored != nil && stored.Title == "enabled issue"
	}, 3*time.Second, 10*time.Millisecond)

	states, err := database.ListArchiveRepoStates(ctx, []int64{storedRepo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveCoverageSupported, states[0].IssuesCoverage)
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].MergeRequestsCoverage)
	assert.True(states[0].IssueInventory.Complete())
	assert.Greater(states[0].IssueInventory.Generation, unsupportedGeneration,
		"successful maintenance must reopen the unsupported historical inventory")
	assert.GreaterOrEqual(historicalIssueListCalls.Load(), int32(2),
		"maintenance recovery must replay the historical inventory")
	assert.GreaterOrEqual(updatedIssueListCalls.Load(), int32(1))
	assert.GreaterOrEqual(issueDetailCalls.Load(), int32(1))

	require.Eventually(func() bool {
		status, statusErr := api.HTTP.ListArchiveStatusWithResponse(ctx, nil)
		return statusErr == nil && status.JSON200 != nil && len(*status.JSON200) == 1 &&
			(*status.JSON200)[0].Status == generated.ArchiveStatusResponseStatusPartial
	}, 3*time.Second, 10*time.Millisecond)
}
