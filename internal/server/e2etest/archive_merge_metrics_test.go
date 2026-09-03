package e2etest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
)

type archiveMergeMetricsClock struct{ now time.Time }

func (c archiveMergeMetricsClock) Now() time.Time { return c.now }

type interveningMergeSnapshotSyncer struct {
	*ghclient.Syncer
	database *db.DB
	repoID   int64
	now      time.Time
}

func (s *interveningMergeSnapshotSyncer) SyncArchiveItem(
	ctx context.Context,
	ref platform.RepoRef,
	itemType db.ArchiveItemType,
	number int,
) (archive.ItemSyncResult, error) {
	result, err := s.Syncer.SyncArchiveItem(ctx, ref, itemType, number)
	if err != nil || itemType != db.ArchiveItemTypeMergeRequest {
		return result, err
	}
	current, err := s.database.GetMergeRequestByRepoIDAndNumber(ctx, s.repoID, number)
	if err != nil || current == nil {
		return result, err
	}
	intervening := *current
	intervening.MergeCommitSHA = "intervening-merge-sha"
	intervening.FilesChanged = new(9)
	intervening.UpdatedAt = s.now.Add(time.Minute)
	intervening.LastActivityAt = intervening.UpdatedAt
	_, err = s.database.UpsertMergeRequest(ctx, &intervening)
	return result, err
}

type archiveMergeMetricsCase struct {
	name                 string
	state                db.MergeRequestState
	storeMergedTime      bool
	storeMergeMetrics    bool
	storeMergeActor      bool
	storedMergeSHA       string
	storedFilesChanged   int
	providerFilesChanged bool
	omitProviderMergedAt bool
	expectMissingFiles   bool
}

func TestArchiveReportRepairsMergedMetricsAcrossRepositoryRenameE2E(t *testing.T) {
	tests := []archiveMergeMetricsCase{
		{
			name: "merged timestamp only", state: db.MergeRequestStateOpen,
			storeMergedTime: true, providerFilesChanged: true,
		},
		{
			name: "merged state only", state: db.MergeRequestStateMerged,
			providerFilesChanged: true,
		},
		{
			name: "complete metrics missing actor", state: db.MergeRequestStateMerged,
			storeMergedTime: true, storeMergeMetrics: true, expectMissingFiles: true,
		},
		{
			name: "state only with stored metrics", state: db.MergeRequestStateMerged,
			storeMergeMetrics: true, providerFilesChanged: true,
		},
		{
			name: "stored timestamp with provider actor", state: db.MergeRequestStateMerged,
			storeMergedTime: true, storeMergeMetrics: true, providerFilesChanged: true,
			omitProviderMergedAt: true,
		},
		{
			name: "old generation with stale canonical metrics", state: db.MergeRequestStateMerged,
			storeMergedTime: true, storeMergeMetrics: true, storeMergeActor: true,
			storedMergeSHA: "pre-merge-test-sha", storedFilesChanged: 9,
			providerFilesChanged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testArchiveReportRepairsMergedMetricsAcrossRepositoryRename(t, tt)
		})
	}
}

func TestArchiveHydrationRejectsInterveningMergeRequestSnapshotE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC)
	mergedAt := now.Add(-time.Hour)

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}`))
		case "/api/v3/repos/acme/widget/pulls/7":
			_, _ = w.Write([]byte(`{
				"id":7,"node_id":"PR_7","number":7,
				"html_url":"https://github.com/acme/widget/pull/7",
				"title":"canonical title","state":"closed","merged":true,
				"created_at":"2026-08-02T10:00:00Z",
				"updated_at":"2026-08-02T12:00:00Z",
				"closed_at":"2026-08-02T12:00:00Z",
				"merged_at":"2026-08-02T12:00:00Z",
				"merged_by":{"login":"merge-admin"},
				"merge_commit_sha":"merge-sha","changed_files":4,
				"head":{"ref":"feature","sha":"head-sha","repo":{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}},
				"base":{"ref":"main","sha":"base-sha","repo":{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}}
			}`))
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
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}},
		time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	wrapped := &interveningMergeSnapshotSyncer{
		Syncer: syncer, database: database, now: now,
	}
	archiveService, err := archive.NewService(
		database, registry, nil, wrapped, nil, archiveMergeMetricsClock{now: now},
	)
	require.NoError(err)
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})
	repo, err := database.GetRepoByIdentity(ctx, platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	wrapped.repoID = repo.ID
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repo.ID, PlatformID: 7, PlatformExternalID: "PR_7", Number: 7,
		URL: "https://github.com/acme/widget/pull/7", Title: "stored title",
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now, LastActivityAt: now,
		MergedAt: &mergedAt, ClosedAt: &mergedAt,
		FilesChanged: new(4), MergeCommitSHA: "merge-sha",
	})
	require.NoError(err)
	require.NoError(database.StartFullArchives(ctx, []int64{repo.ID}, now))
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeIssue,
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest,
		Items: []db.ArchiveInventoryItem{{
			Number: 7, ProviderItemID: "PR_7",
			ProviderCreatedAt: now.Add(-3 * time.Hour), ProviderUpdatedAt: mergedAt,
		}},
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))

	runErr := archiveService.RunEligible(ctx)
	require.ErrorIs(runErr, db.ErrArchiveItemEvidenceChanged)
	progress, err := database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressFailed, progress.Status)
	require.NotNil(progress.LastErrorCode)
	assert.Equal(string(db.ArchiveErrorCodeTransient), *progress.LastErrorCode)
	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 7)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("intervening-merge-sha", stored.MergeCommitSHA)
	require.NotNil(stored.FilesChanged)
	assert.Equal(9, *stored.FilesChanged)
}

func TestArchiveReactivationReclassifiesWorkspaceHeadRepoE2E(t *testing.T) {
	tests := []struct {
		name       string
		headOwner  string
		headRepoID int64
		headClone  string
		expectKind generated.WorkspaceResponseMrHeadRepoKind
	}{
		{
			name: "same repository", headOwner: "acme", headRepoID: 1,
			headClone:  "https://github.com/acme/widget.git",
			expectKind: generated.WorkspaceResponseMrHeadRepoKindSameRepo,
		},
		{
			name: "fork", headOwner: "contributor", headRepoID: 2,
			headClone:  "https://github.com/contributor/widget.git",
			expectKind: generated.WorkspaceResponseMrHeadRepoKindFork,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testArchiveReactivationReclassifiesWorkspaceHeadRepo(t, tt.headOwner, tt.headRepoID, tt.headClone, tt.expectKind)
		})
	}
}

func testArchiveReactivationReclassifiesWorkspaceHeadRepo(
	t *testing.T,
	headOwner string,
	headRepoID int64,
	headClone string,
	expectKind generated.WorkspaceResponseMrHeadRepoKind,
) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	pullStarted := make(chan struct{})
	releasePull := make(chan struct{})
	var startOnce sync.Once

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		baseRepo := map[string]any{
			"id": int64(1), "node_id": "R_widget", "name": "widget",
			"full_name": "acme/widget", "clone_url": "https://github.com/acme/widget.git",
			"owner": map[string]any{"login": "acme"},
		}
		switch r.URL.Path {
		case "/api/graphql":
			var request struct {
				Query string `json:"query"`
			}
			assert.NoError(json.NewDecoder(r.Body).Decode(&request))
			switch {
			case strings.Contains(request.Query, "reviewThreads"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"edges":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
			case strings.Contains(request.Query, "timelineItems"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
			default:
				http.Error(w, `{"message":"unexpected GraphQL query"}`, http.StatusBadRequest)
			}
		case "/api/v3/repos/acme/widget":
			assert.NoError(json.NewEncoder(w).Encode(baseRepo))
		case "/api/v3/repos/acme/widget/pulls/7":
			startOnce.Do(func() { close(pullStarted) })
			<-releasePull
			headRepo := map[string]any{
				"id": headRepoID, "node_id": fmt.Sprintf("R_%d", headRepoID),
				"name": "widget", "full_name": headOwner + "/widget",
				"clone_url": headClone, "owner": map[string]any{"login": headOwner},
			}
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"id": int64(7), "node_id": "PR_7", "number": 7,
				"html_url": "https://github.com/acme/widget/pull/7",
				"title":    "Reappeared pull", "state": "open", "merged": false,
				"created_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
				"updated_at": now.Format(time.RFC3339),
				"head":       map[string]any{"ref": "feature", "sha": "head-sha", "repo": headRepo},
				"base":       map[string]any{"ref": "main", "sha": "base-sha", "repo": baseRepo},
			}))
		case "/api/v3/repos/acme/widget/commits/head-sha/check-runs":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "check_runs": []any{},
			}))
		case "/api/v3/repos/acme/widget/commits/head-sha/status":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"state": "success", "sha": "head-sha", "statuses": []any{},
			}))
		case "/api/v3/repos/acme/widget/actions/runs":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "workflow_runs": []any{},
			}))
		default:
			assert.NoError(json.NewEncoder(w).Encode([]any{}))
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
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}},
		time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	archiveService, err := archive.NewService(
		database, registry, nil, syncer, nil,
		archiveMergeMetricsClock{now: now.Add(2 * time.Minute)},
	)
	require.NoError(err)
	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		Archive: archiveService, HostCheckAllowLoopbackAnyPort: true,
		WorktreeDir: t.TempDir(), DisableWorkspaceBackgroundMonitors: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	forgeServer := httptest.NewServer(srv)
	t.Cleanup(forgeServer.Close)
	api, err := apiclient.NewWithHTTPClient(forgeServer.URL, forgeServer.Client())
	require.NoError(err)
	repositories := []generated.ArchiveRepositoryRef{{
		Provider: "github", PlatformHost: "github.com",
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
	}}
	started, err := api.HTTP.StartArchivesWithResponse(
		ctx, generated.ArchiveMutationBody{Repositories: &repositories},
	)
	require.NoError(err)
	require.NotNil(started.JSON200)

	repo, err := database.GetRepoByIdentity(ctx, platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repo.ID, PlatformID: 7, PlatformExternalID: "PR_7", Number: 7,
		URL: "https://github.com/acme/widget/pull/7", Title: "Stored pull",
		State: db.MergeRequestStateOpen, PlatformHeadSHA: "old-head",
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		LastActivityAt: now.Add(-time.Hour),
	})
	require.NoError(err)
	unknownHead := ""
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID: "ws-reappeared", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 7,
		GitHeadRef: "feature", MRHeadRepo: &unknownHead,
		WorktreePath: t.TempDir(), Status: "creating",
	}))
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeIssue,
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest,
		Items: []db.ArchiveInventoryItem{{
			Number: 7, ProviderItemID: "PR_7",
			ProviderCreatedAt: now.Add(-3 * time.Hour), ProviderUpdatedAt: now,
		}},
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))
	progress, err := database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)

	runDone := make(chan error, 1)
	go func() { runDone <- archiveService.RunEligible(ctx) }()
	select {
	case <-pullStarted:
	case <-time.After(2 * time.Second):
		require.Fail("archive hydration did not reach the provider pull request")
	}
	require.NoError(database.CommitArchiveItemSync(ctx, db.ArchiveItemSyncCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest, ItemNumber: 7,
		ScanGeneration: progress.ScanGeneration, Outcome: db.ArchiveLookupRemoved,
		Now: now.Add(time.Minute),
	}))
	close(releasePull)
	require.NoError(<-runDone)

	removed, err := database.IsArchiveItemRemovedUpstream(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7,
	)
	require.NoError(err)
	require.False(removed)
	detail, err := api.HTTP.GetWorkspaceWithResponse(ctx, "ws-reappeared")
	require.NoError(err)
	require.Equal(http.StatusOK, detail.StatusCode(), string(detail.Body))
	require.NotNil(detail.JSON200)
	require.NotNil(detail.JSON200.MrHeadRepoKind)
	require.Equal(expectKind, *detail.JSON200.MrHeadRepoKind)
}

func testArchiveReportRepairsMergedMetricsAcrossRepositoryRename(
	t *testing.T,
	tt archiveMergeMetricsCase,
) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC)
	canonicalUpdatedAt := now.Add(-time.Hour)
	localUpdatedAt := canonicalUpdatedAt.Add(835 * time.Millisecond)
	mergedAt := canonicalUpdatedAt.Add(-time.Second)
	changedFilesField := ""
	if tt.providerFilesChanged {
		changedFilesField = `,"changed_files":4`
	}
	mergedAtField := `"merged_at":"2026-08-02T11:59:59Z",`
	if tt.omitProviderMergedAt {
		mergedAtField = ""
	}
	var renamed atomic.Bool

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/repos/acme/widget":
			if renamed.Load() {
				_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"renamed","full_name":"acme/renamed","owner":{"login":"acme"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget","owner":{"login":"acme"}}`))
		case "/api/v3/repos/acme/renamed/pulls/7":
			_, _ = fmt.Fprintf(w, `{
				"id":7,"node_id":"PR_7","number":7,
				"html_url":"https://github.com/acme/renamed/pull/7",
				"title":"canonical title","state":"closed",
				"created_at":"2026-08-02T10:00:00Z",
				"updated_at":"2026-08-02T12:00:00Z",
				"closed_at":"2026-08-02T11:59:59Z",
				"merged":true,%s
				"merged_by":{"login":"merge-admin"},
				"merge_commit_sha":"merge-sha"%s,
				"head":{"ref":"feature","sha":"head-sha","repo":{"id":1,"node_id":"R_widget","name":"renamed","full_name":"acme/renamed","owner":{"login":"acme"}}},
				"base":{"ref":"main","sha":"base-sha","repo":{"id":1,"node_id":"R_widget","name":"renamed","full_name":"acme/renamed","owner":{"login":"acme"}}}
			}`, mergedAtField, changedFilesField)
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
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{{
			Platform: ref.Platform, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}},
		time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	clock := archiveMergeMetricsClock{now: now}
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
		Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
	}}
	started, err := api.HTTP.StartArchivesWithResponse(
		ctx, generated.ArchiveMutationBody{Repositories: &repositories},
	)
	require.NoError(err)
	require.NotNil(started.JSON200)

	repo, err := database.GetRepoByIdentity(ctx, platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	var storedMergedAt *time.Time
	if tt.storeMergedTime {
		storedMergedAt = &mergedAt
	}
	var storedFilesChanged *int
	storedMergeCommitSHA := ""
	if tt.storeMergeMetrics {
		storedFilesChanged = new(tt.storedFilesChanged)
		if tt.storedFilesChanged == 0 {
			storedFilesChanged = new(4)
		}
		storedMergeCommitSHA = tt.storedMergeSHA
		if storedMergeCommitSHA == "" {
			storedMergeCommitSHA = "merge-sha"
		}
	}
	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repo.ID, PlatformID: 7, PlatformExternalID: "PR_7", Number: 7,
		URL: "https://github.com/acme/widget/pull/7", Title: "newer local title",
		State: tt.state, PlatformHeadSHA: "head-sha",
		CreatedAt: canonicalUpdatedAt.Add(-2 * time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt, MergedAt: storedMergedAt, ClosedAt: &mergedAt,
		FilesChanged: storedFilesChanged, MergeCommitSHA: storedMergeCommitSHA,
	})
	require.NoError(err)
	if tt.storeMergeActor {
		require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
			MergeRequestID: mrID, EventType: "merged", Author: "merge-admin",
			Summary: "merged this", CreatedAt: mergedAt,
			DedupeKey: "merged-existing",
		}}))
	}
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeIssue,
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))
	require.NoError(database.CommitArchiveInventoryPage(ctx, db.ArchiveInventoryCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest,
		Items: []db.ArchiveInventoryItem{{
			Number: 7, ProviderItemID: "PR_7",
			ProviderCreatedAt: canonicalUpdatedAt.Add(-2 * time.Hour),
			ProviderUpdatedAt: canonicalUpdatedAt,
		}},
		ScanGeneration: 1, Exhausted: true, Coverage: db.ArchiveCoverageSupported,
		Now: now,
	}))
	progress, err := database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	require.NoError(database.CommitArchiveItemSync(ctx, db.ArchiveItemSyncCommit{
		RepoID: repo.ID, ItemType: db.ArchiveItemTypeMergeRequest, ItemNumber: 7,
		ScanGeneration: progress.ScanGeneration, Outcome: db.ArchiveLookupPresent,
		Now: now,
	}))
	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_archive_dataset_progress
		SET scan_generation = ?
		WHERE repo_id = ? AND item_type = 'merge_request'
		  AND item_number = 7 AND dataset = 'lookup'`, int64(1<<32), repo.ID)
	require.NoError(err)

	requireEnsureConfigured(t, archiveService, []platform.RepoRef{ref})
	progress, err = database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressPending, progress.Status)

	renamed.Store(true)
	runErr := archiveService.RunEligible(ctx)
	progress, err = database.GetDatasetProgress(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, 7, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	if tt.expectMissingFiles {
		require.ErrorContains(runErr, "files_changed")
		assert.NotEqual(db.ArchiveDatasetProgressComplete, progress.Status)
		storedMR, readErr := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 7)
		require.NoError(readErr)
		require.NotNil(storedMR)
		events, eventsErr := database.ListMREvents(ctx, storedMR.ID)
		require.NoError(eventsErr)
		require.Len(events, 1)
		assert.Equal("merge-admin", events[0].Author)
		return
	}
	require.NoError(runErr)
	assert.Equal(db.ArchiveDatasetProgressComplete, progress.Status)

	storedRepo, err := database.GetRepoByID(ctx, repo.ID)
	require.NoError(err)
	require.NotNil(storedRepo)
	assert.Equal("renamed", storedRepo.Name)
	storedMR, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 7)
	require.NoError(err)
	require.NotNil(storedMR)
	assert.Equal("merge-sha", storedMR.MergeCommitSHA)
	require.NotNil(storedMR.FilesChanged)
	assert.Equal(4, *storedMR.FilesChanged)
	require.NotNil(storedMR.MergedAt)
	assert.Equal(mergedAt, *storedMR.MergedAt)
	assert.Equal("newer local title", storedMR.Title)

	verbose := true
	reportResponse, err := api.HTTP.GetArchiveReportWithResponse(ctx, &generated.GetArchiveReportParams{
		Start: mergedAt.Add(-time.Minute).Format(time.RFC3339),
		End:   mergedAt.Add(time.Minute).Format(time.RFC3339), Verbose: &verbose,
	})
	require.NoError(err)
	require.NotNil(reportResponse.JSON200)
	require.NotNil(reportResponse.JSON200.Activity)
	require.Len(*reportResponse.JSON200.Activity, 1)
	merged := (*reportResponse.JSON200.Activity)[0]
	assert.Equal(generated.ArchiveReportActivityResponseKindMergeRequestMerged, merged.Kind)
	require.NotNil(merged.Actor)
	assert.Equal("merge-admin", *merged.Actor)
	require.NotNil(merged.MergeCommitSha)
	assert.Equal("merge-sha", *merged.MergeCommitSha)
	require.NotNil(merged.FilesChanged)
	assert.Equal(int64(4), *merged.FilesChanged)
	require.NotNil(reportResponse.JSON200.Repositories)
	require.Len(reportResponse.JSON200.Repositories, 1)
	assert.Equal("renamed", reportResponse.JSON200.Repositories[0].Repository.Name)
}
