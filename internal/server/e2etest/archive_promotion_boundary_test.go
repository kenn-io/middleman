package e2etest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

type archivePromotionClock struct{ now time.Time }

func (c *archivePromotionClock) Now() time.Time { return c.now }

func TestArchiveAPIPromotionMaintainsFromDiscoveryBoundaryE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	discoveredAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	gapUpdatedAt := discoveredAt.Add(2 * time.Hour)
	promotedAt := discoveredAt.Add(24 * time.Hour)
	clock := &archivePromotionClock{now: discoveredAt}
	var gapItemAvailable atomic.Bool
	var baselineContentChanged atomic.Bool
	var maintenanceSince atomic.Pointer[time.Time]
	var baselineDetailCalls atomic.Int64
	var gapDetailCalls atomic.Int64

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/graphql":
			var request struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
				http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
				return
			}
			if strings.Contains(request.Query, "timelineItems") {
				_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
				return
			}
			order, _ := request.Variables["orderField"].(string)
			if order == "CREATED_AT" {
				_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{
					"id":"I_1","databaseId":1,"number":1,"title":"baseline issue",
					"state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/1",
					"author":{"login":"author"},"createdAt":"2025-01-01T00:15:00Z",
					"updatedAt":"2025-01-01T01:00:00Z","closedAt":"2025-01-01T01:00:00Z",
					"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}
				}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
				return
			}
			if !assert.Equal("UPDATED_AT", order) {
				http.Error(w, "unexpected GraphQL query", http.StatusBadRequest)
				return
			}
			since, _ := request.Variables["since"].(string)
			parsedSince, err := time.Parse(time.RFC3339Nano, since)
			if !assert.NoError(err) {
				http.Error(w, "invalid maintenance boundary", http.StatusBadRequest)
				return
			}
			maintenanceSince.Store(&parsedSince)
			if gapItemAvailable.Load() && !parsedSince.After(gapUpdatedAt) {
				_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{
					"id":"I_1","databaseId":1,"number":1,"title":"baseline issue",
					"state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/1",
					"author":{"login":"author"},"createdAt":"2025-01-01T00:15:00Z",
					"updatedAt":"2025-01-01T01:00:00Z","closedAt":"2025-01-01T01:00:00Z",
					"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}
				},{
					"id":"I_17","databaseId":17,"number":17,"title":"gap issue",
					"state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/17",
					"author":{"login":"author"},"createdAt":"2025-01-01T00:30:00Z",
					"updatedAt":"2025-01-01T02:00:00Z","closedAt":"2025-01-01T02:00:00Z",
					"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}
				}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
		case "/api/v3/repos/acme/widget/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{
				"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget",
				"owner":{"login":"acme"}
			}`))
		case "/api/v3/repos/acme/widget/issues/1":
			baselineDetailCalls.Add(1)
			title := "baseline issue"
			if baselineContentChanged.Load() {
				title = "same-second baseline update"
			}
			_, _ = w.Write([]byte(`{
				"id":1,"node_id":"I_1","number":1,"title":` + strconv.Quote(title) + `,"state":"closed",
				"body":"","html_url":"https://github.com/acme/widget/issues/1",
				"user":{"login":"author"},"created_at":"2025-01-01T00:15:00Z",
				"updated_at":"2025-01-01T01:00:00Z","closed_at":"2025-01-01T01:00:00Z",
				"comments":0
			}`))
		case "/api/v3/repos/acme/widget/issues/1/comments":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/repos/acme/widget/issues/17":
			gapDetailCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id":17,"node_id":"I_17","number":17,"title":"gap issue","state":"closed",
				"body":"","html_url":"https://github.com/acme/widget/issues/17",
				"user":{"login":"author"},"created_at":"2025-01-01T00:30:00Z",
				"updated_at":"2025-01-01T02:00:00Z","closed_at":"2025-01-01T02:00:00Z",
				"comments":0
			}`))
		case "/api/v3/repos/acme/widget/issues/17/comments":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(providerServer.Close)

	providerClient, err := ghclient.NewClient(
		staticTokenSource("archive-token"), "github.com", nil, nil,
		ghclient.WithBaseURLForTesting(providerServer.URL),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(map[string]ghclient.Client{
		"github.com": providerClient,
	})
	require.NoError(err)
	database := dbtest.Open(t)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "R_widget",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
			PlatformExternalID: ref.PlatformExternalID,
		}},
		time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	archiveService, err := archive.NewService(
		database, registry, nil, syncer, nil, clock,
	)
	require.NoError(err)
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})

	// Discovery completes while the repository is not yet a full archive.
	require.NoError(archiveService.RunEligible(ctx))
	require.NoError(archiveService.RunEligible(ctx))
	clock.now = gapUpdatedAt
	gapItemAvailable.Store(true)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		Archive:                       archiveService,
		HostCheckAllowLoopbackAnyPort: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	forgeServer := httptest.NewServer(srv)
	t.Cleanup(forgeServer.Close)
	api, err := apiclient.NewWithHTTPClient(forgeServer.URL, forgeServer.Client())
	require.NoError(err)

	clock.now = promotedAt
	repositories := []generated.ArchiveRepositoryRef{{
		Provider: "github", PlatformHost: "github.com",
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
	}}
	started, err := api.HTTP.StartArchivesWithResponse(
		ctx, generated.ArchiveMutationBody{Repositories: &repositories},
	)
	require.NoError(err)
	require.NotNil(started.JSON200)

	// Promotion first hydrates the discovery item. Its provider content then
	// changes without advancing the second-granularity timestamp. Maintenance
	// must refresh that equal-timestamp item as well as discover the gap.
	require.NoError(archiveService.RunEligible(ctx))
	baselineContentChanged.Store(true)
	require.NoError(archiveService.RunEligible(ctx))
	require.NoError(archiveService.RunEligible(ctx))
	require.NoError(archiveService.RunEligible(ctx))

	repo, err := database.GetRepoByIdentity(ctx, platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(ctx, []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	require.NotNil(states[0].InitialStartedAt)
	assert.Equal(discoveredAt, *states[0].InitialStartedAt)
	requestedSince := maintenanceSince.Load()
	require.NotNil(requestedSince)
	assert.Equal(discoveredAt.Add(-time.Second), *requestedSince)
	assert.Equal(int64(2), baselineDetailCalls.Load())
	assert.Equal(int64(1), gapDetailCalls.Load())

	baseline, err := database.GetIssueByRepoIDAndNumber(ctx, repo.ID, 1)
	require.NoError(err)
	require.NotNil(baseline)
	assert.Equal("same-second baseline update", baseline.Title)
	baselineProgress, err := database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressComplete, baselineProgress.Status)

	stored, err := database.GetIssueByRepoIDAndNumber(ctx, repo.ID, 17)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("gap issue", stored.Title)
	require.NotNil(stored.DetailFetchedAt)
	progress, err := database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeIssue, 17, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressComplete, progress.Status)

	status, err := api.HTTP.ListArchiveStatusWithResponse(ctx, nil)
	require.NoError(err)
	require.NotNil(status.JSON200)
	require.Len(*status.JSON200, 1)
	assert.Equal(generated.ArchiveStatusResponseStatusCurrent, (*status.JSON200)[0].Status)
}
