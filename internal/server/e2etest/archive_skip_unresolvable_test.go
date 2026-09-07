package e2etest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A configured repository that archive seeding skipped stays in the syncer's
// tracked set. The archive worker must keep collecting for every healthy
// repository regardless: the tracked ghost is repository-scoped noise, not a
// reason to fail the pass.
func TestArchiveWorkerSkipsUnresolvableTrackedRepoE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if !strings.Contains(request.Query, "issues(first: 100") {
				http.Error(w, `{"message":"unexpected GraphQL query"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
		case "/api/v3/repos/acme/widget/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}`))
		default:
			// The ghost repository resolves nowhere, matching a renamed or
			// deleted route whose config entry went stale.
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
	healthy := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "R_widget",
	}
	ghost := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "ghost", RepoPath: "acme/ghost",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{
			{
				Platform: healthy.Platform, PlatformHost: healthy.Host,
				Owner: healthy.Owner, Name: healthy.Name,
				RepoPath:           healthy.RepoPath,
				PlatformExternalID: healthy.PlatformExternalID,
			},
			{
				Platform: ghost.Platform, PlatformHost: ghost.Host,
				Owner: ghost.Owner, Name: ghost.Name, RepoPath: ghost.RepoPath,
			},
		},
		time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	archiveService, err := archive.NewService(
		database, registry, nil, syncer, nil, nil,
	)
	require.NoError(err)

	// Seeding degrades per repository: the ghost is skipped, the healthy
	// repository seeds, and the call as a whole succeeds.
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{healthy, ghost})

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
		Provider: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}}
	started, err := api.HTTP.StartArchivesWithResponse(
		t.Context(), generated.ArchiveMutationBody{Repositories: &repositories},
	)
	require.NoError(err)
	require.NotNil(started.JSON200)
	require.Len(*started.JSON200, 1)

	// The worker pass sees both tracked refs from the syncer and must not
	// fail on the ghost: issue then merge-request inventory for the healthy
	// repository complete across two passes.
	require.NoError(archiveService.RunEligible(t.Context()))
	require.NoError(archiveService.RunEligible(t.Context()))

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(healthy))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.True(states[0].IssueInventory.Complete(),
		"healthy issue inventory must complete despite the tracked ghost")
	assert.True(states[0].MergeRequestInventory.Complete(),
		"healthy merge-request inventory must complete despite the tracked ghost")
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState)

	status, err := api.HTTP.ListArchiveStatusWithResponse(t.Context(), nil)
	require.NoError(err)
	require.NotNil(status.JSON200)
	require.Len(*status.JSON200, 1,
		"only the healthy repository has archive state; the ghost never seeded")
}
