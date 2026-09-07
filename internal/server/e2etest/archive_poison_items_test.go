package e2etest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

type archivePoisonClock struct{ now time.Time }

func (c *archivePoisonClock) Now() time.Time { return c.now }

func TestArchiveAPIPersistsTerminalAndBackoffOutcomesE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	clock := &archivePoisonClock{
		now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}
	var prShapedCalls atomic.Int32
	var deletedCalls atomic.Int32
	var transientCalls atomic.Int32
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
			_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[
				{"id":"I_11","databaseId":11,"number":11,"title":"stale issue identity","state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/11","author":{"login":"author"},"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2025-01-02T00:00:00Z","closedAt":"2025-01-02T00:00:00Z","comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}},
				{"id":"I_12","databaseId":12,"number":12,"title":"transient issue","state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/12","author":{"login":"author"},"createdAt":"2025-01-03T00:00:00Z","updatedAt":"2025-01-04T00:00:00Z","closedAt":"2025-01-04T00:00:00Z","comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}},
				{"id":"I_13","databaseId":13,"number":13,"title":"deleted issue","state":"CLOSED","body":"","url":"https://github.com/acme/widget/issues/13","author":{"login":"author"},"createdAt":"2025-01-05T00:00:00Z","updatedAt":"2025-01-06T00:00:00Z","closedAt":"2025-01-06T00:00:00Z","comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}}
			],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
		case "/api/v3/repos/acme/widget/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}`))
		case "/api/v3/repos/acme/widget/issues/11":
			prShapedCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":11,"node_id":"PR_11","number":11,"repository_url":"https://api.github.com/repos/acme/widget","html_url":"https://github.com/acme/widget/pull/11","title":"actually a pull request","state":"closed","user":{"login":"author"},"pull_request":{"url":"https://api.github.com/repos/acme/widget/pulls/11"},"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","closed_at":"2025-01-02T00:00:00Z"}`))
		case "/api/v3/repos/acme/widget/issues/12":
			transientCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"temporary outage"}`))
		case "/api/v3/repos/acme/widget/issues/13":
			deletedCalls.Add(1)
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"message":"This issue was deleted"}`))
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
	assert.Equal(generated.ArchiveStatusResponseStatusRunning, (*started.JSON200)[0].Status)

	require.NoError(archiveService.RunEligible(t.Context())) // issue inventory
	require.NoError(archiveService.RunEligible(t.Context())) // merge-request inventory
	require.NoError(archiveService.RunEligible(t.Context())) // PR-shaped issue hydration
	require.Error(archiveService.RunEligible(t.Context()))   // first transient failure
	require.NoError(archiveService.RunEligible(t.Context())) // deleted issue hydration

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	terminal, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 11, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressTerminal, terminal.Status)
	assert.Zero(terminal.AttemptCount)
	assert.Nil(terminal.NextRetryAt)
	require.NotNil(terminal.LastErrorCode)
	assert.Equal(string(platform.ErrCodeNotFound), *terminal.LastErrorCode)

	var lifecycle db.ArchiveLifecycleState
	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT lifecycle_state FROM forge_archive_items
		WHERE repo_id = ? AND item_type = ? AND item_number = ?`,
		repo.ID, db.ArchiveItemTypeIssue, 11,
	).Scan(&lifecycle))
	assert.Equal(db.ArchiveLifecycleStateRemovedUpstream, lifecycle)
	deleted, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 13, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressTerminal, deleted.Status)
	assert.Zero(deleted.AttemptCount)
	assert.Nil(deleted.NextRetryAt)
	require.NotNil(deleted.LastErrorCode)
	assert.Equal(string(platform.ErrCodeNotFound), *deleted.LastErrorCode)

	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT lifecycle_state FROM forge_archive_items
		WHERE repo_id = ? AND item_type = ? AND item_number = ?`,
		repo.ID, db.ArchiveItemTypeIssue, 13,
	).Scan(&lifecycle))
	assert.Equal(db.ArchiveLifecycleStateRemovedUpstream, lifecycle)

	failed, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 12, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressFailed, failed.Status)
	assert.Equal(1, failed.AttemptCount)
	require.NotNil(failed.NextRetryAt)
	assert.Equal(clock.now.Add(time.Minute), *failed.NextRetryAt)

	status, err := api.HTTP.ListArchiveStatusWithResponse(t.Context(), nil)
	require.NoError(err)
	require.NotNil(status.JSON200)
	require.Len(*status.JSON200, 1)
	assert.Equal(int64(3), (*status.JSON200)[0].Counts.Items)
	assert.Equal(int64(2), (*status.JSON200)[0].Counts.CompleteItems)
	assert.Equal(int64(1), (*status.JSON200)[0].Counts.FailedItems)
	assert.Equal(int32(1), prShapedCalls.Load())
	assert.Equal(int32(1), deletedCalls.Load())
	assert.Equal(int32(1), transientCalls.Load())

	// Neither terminal work nor a failed item whose retry is in the future may
	// spend another provider call.
	require.NoError(archiveService.RunEligible(t.Context()))
	assert.Equal(int32(1), prShapedCalls.Load())
	assert.Equal(int32(1), deletedCalls.Load())
	assert.Equal(int32(1), transientCalls.Load())

	clock.now = clock.now.Add(time.Minute)
	require.Error(archiveService.RunEligible(t.Context()))
	failed, err = database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 12, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(2, failed.AttemptCount)
	require.NotNil(failed.NextRetryAt)
	assert.Equal(clock.now.Add(2*time.Minute), *failed.NextRetryAt)

	clock.now = clock.now.Add(time.Minute)
	require.NoError(archiveService.RunEligible(t.Context()))
	assert.Equal(int32(2), transientCalls.Load(), "retry before next_retry_at must stay provider-free")

	clock.now = clock.now.Add(time.Minute)
	require.Error(archiveService.RunEligible(t.Context()))
	failed, err = database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 12, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(3, failed.AttemptCount)
	require.NotNil(failed.NextRetryAt)
	assert.Equal(clock.now.Add(4*time.Minute), *failed.NextRetryAt)

	clock.now = clock.now.Add(3 * time.Minute)
	require.NoError(archiveService.RunEligible(t.Context()))
	assert.Equal(int32(1), prShapedCalls.Load(), "terminal issue must never be fetched again")
	assert.Equal(int32(1), deletedCalls.Load(), "deleted issue must never be fetched again")
	assert.Equal(int32(3), transientCalls.Load(), "retry before next_retry_at must stay provider-free")

	status, err = api.HTTP.ListArchiveStatusWithResponse(t.Context(), nil)
	require.NoError(err)
	require.NotNil(status.JSON200)
	require.Len(*status.JSON200, 1)
	assert.Equal(int64(2), (*status.JSON200)[0].Counts.CompleteItems)
	assert.Equal(int64(1), (*status.JSON200)[0].Counts.FailedItems)
	assert.Equal(int32(1), prShapedCalls.Load(), "status reads must not call the provider")
	assert.Equal(int32(1), deletedCalls.Load(), "status reads must not call the provider")
	assert.Equal(int32(3), transientCalls.Load(), "status reads must not call the provider")
}
