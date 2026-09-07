package e2etest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

type archiveQuotaBurstSource struct {
	refs   []platform.RepoRef
	client ghclient.Client
}

func (s archiveQuotaBurstSource) ConfiguredRepositories(context.Context) ([]platform.RepoRef, error) {
	return s.refs, nil
}

func (archiveQuotaBurstSource) ArchiveItemSyncCost(platform.Kind, db.ArchiveItemType) int {
	return 1
}

func (s archiveQuotaBurstSource) SyncArchiveItem(
	ctx context.Context,
	ref platform.RepoRef,
	_ db.ArchiveItemType,
	number int,
) (archive.ItemSyncResult, error) {
	attempted := false
	for range 10 {
		_, err := s.client.GetIssue(ctx, ref.Owner, ref.Name, number)
		if err != nil {
			return archive.ItemSyncResult{ProviderAttempted: attempted}, err
		}
		attempted = true
	}
	return archive.ItemSyncResult{ProviderAttempted: attempted}, nil
}

func TestArchiveAPIStopsProviderBurstAtObservedQuotaHeadroomE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Now().UTC()
	reset := now.Add(30 * time.Minute).Truncate(time.Second)
	var upstreamCalls atomic.Int32
	var upstreamRemaining atomic.Int32
	upstreamRemaining.Store(1003)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := upstreamCalls.Add(1)
		remaining := upstreamRemaining.Add(-2)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		_, _ = fmt.Fprintf(w, `{
			"id": %d,
			"number": 1,
			"title": "archive issue",
			"state": "open",
			"user": {"login": "ada"},
			"html_url": "https://github.com/acme/widget/issues/1",
			"repository_url": "https://api.github.com/repos/acme/widget",
			"created_at": "2026-07-01T00:00:00Z",
			"updated_at": "2026-07-01T00:00:00Z"
		}`, call)
	}))
	t.Cleanup(upstream.Close)

	database := dbtest.Open(t)
	quotaRegistry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.com", Principal: "user:7"}
	bucket := ghclient.RateBucketKey("github", "github.com", "user:7")
	budget := ghclient.NewSyncBudget(100000)
	client, err := ghclient.NewClient(
		staticTokenSource("archive-token"),
		"github.com",
		nil,
		budget,
		ghclient.WithBaseURLForTesting(upstream.URL),
		ghclient.WithQuotaAccounting(quotaRegistry, identity, identity),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(map[string]ghclient.Client{
		"github.com": client,
	})
	require.NoError(err)
	syncRef := ghclient.RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-acme-widget",
	}
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-acme-widget",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{syncRef},
		time.Hour,
		nil,
		map[string]*ghclient.SyncBudget{bucket: budget},
	)
	t.Cleanup(syncer.Stop)
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key:           ghclient.RouteKey{Host: "github.com", Owner: "acme"},
		Client:        client,
		ReadIdentity:  identity,
		WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	syncer.SetQuotaRegistry(quotaRegistry)
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceREST, ghclient.Rate{
		Limit: 5000, Remaining: 1003, Reset: reset,
	})
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL, ghclient.Rate{
		Limit: 5000, Remaining: 1003, Reset: reset,
	})

	source := archiveQuotaBurstSource{refs: []platform.RepoRef{ref}, client: client}
	archiveService, err := archive.NewService(
		database, registry, syncer, source, nil, nil,
	)
	require.NoError(err)
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		Archive:                       archiveService,
		HostCheckAllowLoopbackAnyPort: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)

	status, body := postJSON(
		t,
		forge.Client(),
		forge.URL+"/api/v1/archive/start",
		map[string]any{"all": false, "repositories": []map[string]string{{
			"provider": "github", "platform_host": "github.com",
			"owner": "acme", "name": "widget", "repo_path": "acme/widget",
		}}},
	)
	require.Equal(http.StatusOK, status, body)

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	require.NoError(database.CommitArchiveInventoryPage(t.Context(), db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeIssue,
		RefreshReason: db.ArchiveRefreshReasonInitial,
		Items: []db.ArchiveInventoryItem{{
			Number: 1, ProviderItemID: "issue-1",
			ProviderCreatedAt: now.Add(-time.Hour), ProviderUpdatedAt: now,
		}},
		ScanGeneration: states[0].IssueInventory.Generation,
		Exhausted:      true, Coverage: db.ArchiveCoverageSupported, Now: now,
	}))
	require.NoError(database.CommitArchiveInventoryPage(t.Context(), db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest,
		RefreshReason:  db.ArchiveRefreshReasonInitial,
		ScanGeneration: states[0].MergeRequestInventory.Generation,
		Exhausted:      true, Coverage: db.ArchiveCoverageSupported, Now: now,
	}))

	runErr := archiveService.RunEligible(t.Context())
	require.ErrorIs(runErr, platform.ErrArchiveAttemptBudget)
	assert.Equal(int32(1), upstreamCalls.Load())
	// Provider-reserved attempts are metered by the quota registry, not the
	// local sync budget.
	assert.Zero(budget.ArchiveSpent())
	pool, ok := quotaRegistry.Get(identity, ghclient.QuotaResourceREST)
	require.True(ok)
	assert.Equal(1001, pool.Remaining)
	progress, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressFailed, progress.Status)
}

// A large pool sitting at its own limit/5 reserve must stop hydration end to
// end — through the HTTP API, scheduler, SQLite state, and provider
// transport — even though the smallest pool still has headroom, and the
// persisted deferral must retry when that deficient pool resets.
func TestArchiveAPIDefersHydrationAtLargerPoolReserveE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Now().UTC()
	restReset := now.Add(10 * time.Minute).Truncate(time.Second)
	graphQLReset := now.Add(30 * time.Minute).Truncate(time.Second)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstream.Close)

	database := dbtest.Open(t)
	quotaRegistry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.com", Principal: "user:7"}
	bucket := ghclient.RateBucketKey("github", "github.com", "user:7")
	budget := ghclient.NewSyncBudget(100000)
	client, err := ghclient.NewClient(
		staticTokenSource("archive-token"),
		"github.com",
		nil,
		budget,
		ghclient.WithBaseURLForTesting(upstream.URL),
		ghclient.WithQuotaAccounting(quotaRegistry, identity, identity),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(map[string]ghclient.Client{
		"github.com": client,
	})
	require.NoError(err)
	syncRef := ghclient.RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-acme-widget",
	}
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-acme-widget",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{syncRef},
		time.Hour,
		nil,
		map[string]*ghclient.SyncBudget{bucket: budget},
	)
	t.Cleanup(syncer.Stop)
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key:           ghclient.RouteKey{Host: "github.com", Owner: "acme"},
		Client:        client,
		ReadIdentity:  identity,
		WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	syncer.SetQuotaRegistry(quotaRegistry)
	// REST sits exactly at its own limit/5 reserve (3000) and resets first;
	// GraphQL has plenty of headroom and resets later.
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceREST, ghclient.Rate{
		Limit: 15000, Remaining: 3000, Reset: restReset,
	})
	quotaRegistry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL, ghclient.Rate{
		Limit: 5000, Remaining: 4800, Reset: graphQLReset,
	})

	source := archiveQuotaBurstSource{refs: []platform.RepoRef{ref}, client: client}
	archiveService, err := archive.NewService(
		database, registry, syncer, source, nil, nil,
	)
	require.NoError(err)
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		Archive:                       archiveService,
		HostCheckAllowLoopbackAnyPort: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)

	status, body := postJSON(
		t,
		forge.Client(),
		forge.URL+"/api/v1/archive/start",
		map[string]any{"all": false, "repositories": []map[string]string{{
			"provider": "github", "platform_host": "github.com",
			"owner": "acme", "name": "widget", "repo_path": "acme/widget",
		}}},
	)
	require.Equal(http.StatusOK, status, body)

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	require.NoError(database.CommitArchiveInventoryPage(t.Context(), db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeIssue,
		RefreshReason: db.ArchiveRefreshReasonInitial,
		Items: []db.ArchiveInventoryItem{{
			Number: 1, ProviderItemID: "issue-1",
			ProviderCreatedAt: now.Add(-time.Hour), ProviderUpdatedAt: now,
		}},
		ScanGeneration: states[0].IssueInventory.Generation,
		Exhausted:      true, Coverage: db.ArchiveCoverageSupported, Now: now,
	}))
	require.NoError(database.CommitArchiveInventoryPage(t.Context(), db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest,
		RefreshReason:  db.ArchiveRefreshReasonInitial,
		ScanGeneration: states[0].MergeRequestInventory.Generation,
		Exhausted:      true, Coverage: db.ArchiveCoverageSupported, Now: now,
	}))

	require.NoError(archiveService.RunEligible(t.Context()))
	assert.Zero(upstreamCalls.Load(), "no upstream hydration below the larger pool's reserve")

	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	require.NotNil(states[0].LastErrorCode)
	assert.Equal(string(db.ArchiveErrorCodeBudgetExhausted), *states[0].LastErrorCode)
	require.NotNil(states[0].NextRetryAt)
	assert.Equal(restReset, states[0].NextRetryAt.UTC())
	progress, err := database.GetDatasetProgress(
		t.Context(), repo.ID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressPending, progress.Status)
}

func requireEnsureConfigured(t *testing.T, s *archive.Service, refs []platform.RepoRef) {
	t.Helper()
	_, err := s.EnsureConfigured(t.Context(), refs)
	require.NoError(t, err)
}
