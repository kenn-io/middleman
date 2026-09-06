package syncertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitlab"
)

type staticGitLabToken string

func (s staticGitLabToken) Token(context.Context) (string, error) { return string(s), nil }

func (s staticGitLabToken) Invalidate(string) {}

func (s staticGitLabToken) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "gitlab", Host: "gitlab.example.com"}}
}

// TestGitLabProviderSyncPersistsAndRetainsInaccessibleItems drives the real
// Syncer against a real GitLab provider client backed by a fake GitLab
// server, registered through the real provider registry. It proves the
// provider-neutral sync path end to end over SQLite:
//
//  1. inventory sync persists issue and merge-request rows;
//  2. comments and inline review threads persist through the neutral event
//     path, including a system-note lifecycle event;
//  3. an item that turns inaccessible (item 404 while the project stays
//     readable — GitLab's confidential-content ambiguity) surfaces as a
//     partial issue-scope failure and retains the cached row untouched.
func TestGitLabProviderSyncPersistsAndRetainsInaccessibleItems(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	projectJSON := `{
		"id": 42,
		"path": "project",
		"path_with_namespace": "group/project",
		"name": "Project",
		"default_branch": "main",
		"web_url": "https://gitlab.example.com/group/project",
		"http_url_to_repo": "https://gitlab.example.com/group/project.git",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-02T00:00:00Z"
	}`
	var issueOpen atomic.Bool
	issueOpen.Store(true)
	var mrMerged atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject", "/api/v4/projects/42":
			_, _ = w.Write([]byte(projectJSON))
		case "/api/v4/projects/42/merge_requests":
			_, _ = w.Write([]byte(`[{
				"id": 1001, "iid": 8, "project_id": 42,
				"title": "open MR", "state": "opened",
				"author": {"username": "lin"},
				"source_branch": "feature", "target_branch": "main", "sha": "abc123",
				"web_url": "https://gitlab.example.com/group/project/-/merge_requests/8",
				"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-02T00:00:00Z"
			}]`))
		case "/api/v4/projects/42/merge_requests/8":
			if mrMerged.Load() {
				_, _ = w.Write([]byte(`{
					"id": 1001, "iid": 8, "project_id": 42,
					"title": "merged MR", "state": "merged",
					"author": {"username": "lin"}, "merge_user": {"username": "maintainer"},
					"source_branch": "feature", "target_branch": "main", "sha": "abc123",
					"merged_at": "2026-06-03T00:00:00Z",
					"merge_commit_sha": "merge-sha", "squash_commit_sha": "squash-sha",
					"web_url": "https://gitlab.example.com/group/project/-/merge_requests/8",
					"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-03T00:00:00Z"
				}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"id": 1001, "iid": 8, "project_id": 42,
				"title": "open MR", "state": "opened",
				"author": {"username": "lin"},
				"source_branch": "feature", "target_branch": "main", "sha": "abc123",
				"web_url": "https://gitlab.example.com/group/project/-/merge_requests/8",
				"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-02T00:00:00Z"
			}`))
		case "/api/v4/projects/42/merge_requests/8/discussions":
			_, _ = w.Write([]byte(`[
				{"id": "ordinary", "notes": [{"id": 401, "body": "ordinary comment", "author": {"username": "omar"}, "created_at": "2026-06-01T10:00:00Z"}]},
				{"id": "system", "notes": [{"id": 402, "body": "merged", "system": true, "author": {"username": "maintainer"}, "created_at": "2026-06-01T11:00:00Z"}]},
				{"id": "inline", "notes": [{
					"id": 403, "body": "inline note", "author": {"username": "rhea"},
					"resolvable": true, "resolved": false,
					"created_at": "2026-06-01T12:00:00Z", "updated_at": "2026-06-01T12:00:00Z",
					"position": {"base_sha": "base", "start_sha": "base", "head_sha": "head", "position_type": "text", "new_path": "main.go", "new_line": 9}
				}]}
			]`))
		case "/api/v4/projects/42/issues":
			if issueOpen.Load() {
				_, _ = w.Write([]byte(`[{
					"id": 2001, "iid": 7, "project_id": 42,
					"title": "tracked issue", "state": "opened",
					"author": {"username": "ada"},
					"web_url": "https://gitlab.example.com/group/project/-/issues/7",
					"created_at": "2026-05-01T00:00:00Z", "updated_at": "2026-05-02T00:00:00Z"
				}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case "/api/v4/projects/42/issues/7":
			if issueOpen.Load() {
				_, _ = w.Write([]byte(`{
					"id": 2001, "iid": 7, "project_id": 42,
					"title": "tracked issue", "state": "opened",
					"author": {"username": "ada"},
					"web_url": "https://gitlab.example.com/group/project/-/issues/7",
					"created_at": "2026-05-01T00:00:00Z", "updated_at": "2026-05-02T00:00:00Z"
				}`))
				return
			}
			// GitLab hides confidential content behind an item 404 while the
			// project stays readable.
			http.NotFound(w, r)
		case "/api/v4/projects/42/issues/7/discussions":
			_, _ = w.Write([]byte(`[{"id": "issue-thread", "notes": [{"id": 301, "body": "issue comment", "author": {"username": "ivy"}, "created_at": "2026-05-01T10:00:00Z"}]}]`))
		default:
			// Releases, tags, pipelines, labels, commits, and other list
			// surfaces the sync cycle touches are empty in this fixture.
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()

	client, err := gitlab.NewClient(
		"gitlab.example.com", staticGitLabToken("token"),
		gitlab.WithBaseURLForTesting(server.URL+"/api/v4"),
		gitlab.WithoutRetriesForTesting(), gitlab.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(nil, client)
	require.NoError(err)
	repo := ghclient.RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.example.com",
		Owner: "group", Name: "project", RepoPath: "group/project",
	}
	// The detail drain (MR events, review threads) only runs for budgeted
	// buckets, mirroring the production provider startup wiring.
	budgets := func() map[string]*ghclient.SyncBudget {
		return map[string]*ghclient.SyncBudget{
			ghclient.RateBucketKey("gitlab", "gitlab.example.com", "host"): ghclient.NewSyncBudget(1000),
		}
	}

	seedSyncer := ghclient.NewSyncerWithRegistry(
		registry, d, nil, []ghclient.RepoRef{repo}, time.Minute, nil, budgets(),
	)
	var seedResults []ghclient.RepoSyncResult
	seedSyncer.SetOnSyncCompleted(func(results []ghclient.RepoSyncResult) {
		seedResults = results
	})
	seedSyncer.RunOnce(ctx)

	require.Len(seedResults, 1)
	require.Empty(seedResults[0].Error, "seed cycle must sync cleanly")

	issue, err := d.GetIssue(ctx, "gitlab", "gitlab.example.com", "group", "project", 7)
	require.NoError(err)
	require.NotNil(issue, "inventory sync must persist the open issue")
	assert.Equal("tracked issue", issue.Title)
	assert.Equal("open", issue.State)

	issueEvents, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(issueEvents, 1, "issue comments must persist through the neutral path")
	assert.Equal("issue comment", issueEvents[0].Body)

	mr, err := d.GetMergeRequest(ctx, "gitlab", "gitlab.example.com", "group", "project", 8)
	require.NoError(err)
	require.NotNil(mr, "inventory sync must persist the open merge request")
	assert.Equal("open MR", mr.Title)

	mrEvents, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	bodies := make(map[string]bool, len(mrEvents))
	types := make(map[string]bool, len(mrEvents))
	for _, event := range mrEvents {
		bodies[event.Body] = true
		types[event.EventType] = true
	}
	assert.True(bodies["ordinary comment"], "ordinary MR comments must persist")
	assert.True(types["merged"], "system-note lifecycle events must persist")

	threads, err := d.ListMRReviewThreads(ctx, mr.ID)
	require.NoError(err)
	require.NotEmpty(threads, "inline review threads must persist through the neutral path")
	assert.Equal("inline", threads[0].ProviderThreadID)

	repoRow, err := d.GetRepoByIdentity(ctx, platformdb.DBRepoIdentity(platform.RepoRef{
		Platform: repo.Platform, Host: repo.PlatformHost, Owner: repo.Owner,
		Name: repo.Name, RepoPath: repo.RepoPath,
	}))
	require.NoError(err)
	require.NotNil(repoRow)
	mrMerged.Store(true)
	mergedMR, err := client.GetMergeRequest(ctx, platform.RepoRef{
		Platform: repo.Platform, Host: repo.PlatformHost, Owner: repo.Owner,
		Name: repo.Name, RepoPath: repo.RepoPath, PlatformID: 42,
	}, 8)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, platformdb.DBMergeRequest(repoRow.ID, mergedMR))
	require.NoError(err)
	storedMergedMR, err := d.GetMergeRequest(
		ctx, "gitlab", "gitlab.example.com", "group", "project", 8,
	)
	require.NoError(err)
	require.NotNil(storedMergedMR)
	assert.Equal("merge-sha", storedMergedMR.MergeCommitSHA)

	tx, err := d.ReadDB().BeginTx(ctx, nil)
	require.NoError(err)
	activity, err := db.LoadArchiveReportActivity(
		ctx, tx, []int64{repoRow.ID},
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.NoError(tx.Commit())
	var mergedActivity *db.ArchiveReportActivityRow
	for i := range activity {
		if activity[i].Kind == db.ArchiveReportActivityMergeRequestMerged {
			mergedActivity = &activity[i]
			break
		}
	}
	require.NotNil(mergedActivity)
	assert.Equal("merge-sha", mergedActivity.MergeCommitSHA)

	// Second cycle: the issue leaves the open inventory and its item fetch
	// turns 404 while the project stays readable. The canonical lookup
	// classifies this as inaccessible; the sync must retain the cached row
	// and surface the failure as an issue-scope partial failure.
	issueOpen.Store(false)
	retainSyncer := ghclient.NewSyncerWithRegistry(
		registry, d, nil, []ghclient.RepoRef{repo}, time.Minute, nil, budgets(),
	)
	var retainResults []ghclient.RepoSyncResult
	retainSyncer.SetOnSyncCompleted(func(results []ghclient.RepoSyncResult) {
		retainResults = results
	})
	retainSyncer.RunOnce(ctx)

	require.Len(retainResults, 1)
	assert.NotEmpty(retainResults[0].Error,
		"an inaccessible item must stay visible in sync health")
	require.NotNil(retainResults[0].PartialFailure,
		"an item-scope failure must be typed as partial")
	assert.True(retainResults[0].PartialFailure.Issues)
	assert.False(retainResults[0].PartialFailure.MergeRequests)

	retained, err := d.GetIssue(ctx, "gitlab", "gitlab.example.com", "group", "project", 7)
	require.NoError(err)
	require.NotNil(retained, "the cached row must survive the inaccessible refresh")
	assert.Equal("tracked issue", retained.Title,
		"the cached row must keep its content")
	assert.Equal("open", retained.State,
		"the cached row must not be flipped by an inaccessible refresh")
}

func TestGitLabArchiveIssueLifecyclePersistsCloseActorInReport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	closedAt := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42":
			_, _ = w.Write([]byte(`{
				"id": 42,
				"path": "project",
				"path_with_namespace": "group/project",
				"web_url": "https://gitlab.example.com/group/project",
				"default_branch": "main"
			}`))
		case "/api/v4/projects/42/issues/7":
			_, _ = w.Write([]byte(`{
				"id": 2001, "iid": 7, "project_id": 42,
				"title": "closed issue", "state": "closed",
				"author": {"username": "author"},
				"closed_at": "2026-06-03T12:00:00Z",
				"web_url": "https://gitlab.example.com/group/project/-/issues/7",
				"created_at": "2026-06-01T00:00:00Z",
				"updated_at": "2026-06-03T12:00:00Z"
			}`))
		case "/api/v4/projects/42/issues/7/discussions":
			_, _ = w.Write([]byte(`[
				{"id": "ordinary", "notes": [{
					"id": 301, "body": "ordinary comment", "author": {"username": "commenter"},
					"created_at": "2026-06-02T10:00:00Z"
				}]},
				{"id": "lifecycle", "notes": [{
					"id": 302, "body": "closed", "system": true,
					"author": {"username": "closer"},
					"created_at": "2026-06-03T12:00:00Z"
				}]}
			]`))
		case "/api/v4/projects/42/issues/7/related_merge_requests":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := gitlab.NewClient(
		"gitlab.example.com", staticGitLabToken("token"),
		gitlab.WithBaseURLForTesting(server.URL+"/api/v4"),
		gitlab.WithoutRetriesForTesting(), gitlab.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(nil, client)
	require.NoError(err)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "42", Owner: "group", Name: "project",
		RepoPath: "group/project",
	})
	require.NoError(err)
	repo := ghclient.RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.example.com",
		PlatformRepoID: 42, PlatformExternalID: "42", RepoID: repoID,
		Owner: "group", Name: "project", RepoPath: "group/project",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo},
		time.Minute, nil, nil,
	)

	result, err := syncer.SyncArchiveItem(
		ghclient.WithArchiveSyncBudget(ctx),
		platform.RepoRef{
			Platform: platform.KindGitLab, Host: "gitlab.example.com",
			PlatformID: 42, PlatformExternalID: "42",
			Owner: "group", Name: "project", RepoPath: "group/project",
		},
		db.ArchiveItemTypeIssue, 7,
	)
	require.NoError(err)
	assert.True(result.ProviderAttempted)

	issue, err := database.GetIssue(
		ctx, "gitlab", "gitlab.example.com", "group", "project", 7,
	)
	require.NoError(err)
	require.NotNil(issue)
	require.NotNil(issue.ClosedAt)
	assert.Equal(closedAt, *issue.ClosedAt)

	events, err := database.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 2)
	assert.Equal("closed", events[0].EventType)
	assert.Equal("closer", events[0].Author)

	tx, err := database.ReadDB().BeginTx(ctx, nil)
	require.NoError(err)
	activity, err := db.LoadArchiveReportActivity(
		ctx, tx, []int64{repoID},
		time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.NoError(tx.Commit())
	require.Len(activity, 1)
	assert.Equal(db.ArchiveReportActivityIssueClosed, activity[0].Kind)
	assert.Equal("closer", activity[0].Actor)
}
