package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func verifiedGitHubRepoIdentity(host, owner, name string) db.RepoIdentity {
	identity := db.GitHubRepoIdentity(host, owner, name)
	identity.PlatformRepoID = "repo-" + strings.ToLower(owner+"-"+name)
	return identity
}

func verifiedDBRepoIdentity(ref platform.RepoRef) db.RepoIdentity {
	identity := platform.DBRepoIdentity(ref)
	if identity.PlatformRepoID == "" {
		identity.PlatformRepoID = "test-" + strings.ToLower(
			string(ref.Platform)+"-"+ref.Host+"-"+ref.DisplayName(),
		)
	}
	return identity
}

// openTestDB opens a temporary SQLite database for the duration of the test.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t)
}

func mergeRequestSnapshotRevision(t *testing.T, database *db.DB, repoID int64, number int) int64 {
	t.Helper()
	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, number)
	require.NoError(t, err)
	require.NotNil(t, mr)
	return mr.SnapshotRevision
}

func TestCommitIssueParentSnapshotRejectsStaleLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "test-github-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	newer := &db.Issue{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1,
		Title: "newer", State: "open", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		LastActivityAt: now,
	}
	issueID, err := database.UpsertIssue(ctx, newer)
	require.NoError(err)
	require.NoError(database.ReplaceIssueLabels(ctx, repoID, issueID, []db.Label{{
		RepoID: repoID, PlatformID: 1, Name: "newer", Color: "ffffff", UpdatedAt: now,
	}}))

	stale := *newer
	stale.Title = "stale"
	stale.UpdatedAt = now.Add(-time.Minute)
	stale.LastActivityAt = stale.UpdatedAt
	stale.Labels = []db.Label{{
		RepoID: repoID, PlatformID: 2, Name: "stale", Color: "000000", UpdatedAt: stale.UpdatedAt,
	}}
	syncer := &Syncer{db: database}
	committedID, _, accepted, err := syncer.commitIssueParentSnapshot(ctx, RepoRef{}, &stale)
	require.NoError(err)
	assert.Equal(issueID, committedID)
	assert.False(accepted)

	stored, err := database.GetIssueByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("newer", stored.Title)
	require.Len(stored.Labels, 1)
	assert.Equal("newer", stored.Labels[0].Name)
}

func TestNormalSyncRejectsIssueCommentsAfterParentAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	oldUpdatedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	currentUpdatedAt := oldUpdatedAt.Add(time.Minute)
	repo := RepoRef{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "widget"}
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "test-github-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	issueID, err := database.UpsertIssue(ctx, &db.Issue{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1,
		Title: "old", State: "open", CreatedAt: oldUpdatedAt.Add(-time.Hour),
		UpdatedAt: oldUpdatedAt, LastActivityAt: oldUpdatedAt,
	})
	require.NoError(err)
	stale, err := database.GetIssueByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(stale)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1,
		Title: "current", State: "open", CreatedAt: oldUpdatedAt.Add(-time.Hour),
		UpdatedAt: currentUpdatedAt, LastActivityAt: currentUpdatedAt,
	})
	require.NoError(err)
	require.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{{
		IssueID: issueID, EventType: "issue_comment", DedupeKey: "current-comment",
		CreatedAt: currentUpdatedAt,
	}}))

	applied, err := (&Syncer{db: database}).commitIssueCommentsSnapshot(
		ctx, repo, issueID, 1, stale.SnapshotRevision, []db.IssueEvent{{
			IssueID: issueID, EventType: "issue_comment", DedupeKey: "stale-comment",
			CreatedAt: oldUpdatedAt,
		}}, nil, nil,
	)
	require.NoError(err)
	assert.False(applied)
	events, err := database.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("current-comment", events[0].DedupeKey)
}

func TestNormalSyncRejectsAllMergeRequestChildrenAfterParentAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	oldUpdatedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	currentUpdatedAt := oldUpdatedAt.Add(time.Minute)
	repo := RepoRef{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "widget"}
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "test-github-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "mr-1", Number: 1,
		Title: "old", State: db.MergeRequestStateOpen, CreatedAt: oldUpdatedAt.Add(-time.Hour),
		UpdatedAt: oldUpdatedAt, LastActivityAt: oldUpdatedAt,
	})
	require.NoError(err)
	staleRevision := mergeRequestSnapshotRevision(t, database, repoID, 1)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "mr-1", Number: 1,
		Title: "current", State: db.MergeRequestStateOpen, CreatedAt: oldUpdatedAt.Add(-time.Hour),
		UpdatedAt: currentUpdatedAt, LastActivityAt: currentUpdatedAt,
	})
	require.NoError(err)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{
		{MergeRequestID: mrID, EventType: "issue_comment", DedupeKey: "current-comment", CreatedAt: currentUpdatedAt},
		{MergeRequestID: mrID, EventType: "review", DedupeKey: "current-review", CreatedAt: currentUpdatedAt},
		{MergeRequestID: mrID, EventType: "review_comment", DedupeKey: "current-inline", CreatedAt: currentUpdatedAt},
	}))
	require.NoError(database.UpsertMRReviewThreads(ctx, mrID, []db.MRReviewThread{{
		ProviderThreadID: "current-thread", CreatedAt: currentUpdatedAt, UpdatedAt: currentUpdatedAt,
	}}))

	applied, err := (&Syncer{db: database}).commitMergeRequestDatasets(
		ctx, repo, mrID, 1, staleRevision,
		[]db.MREvent{{EventType: "issue_comment", DedupeKey: "stale-comment", CreatedAt: oldUpdatedAt}}, true,
		[]db.MREvent{{EventType: "review", DedupeKey: "stale-review", CreatedAt: oldUpdatedAt}},
		[]db.MREvent{{EventType: "review_comment", DedupeKey: "stale-inline", CreatedAt: oldUpdatedAt}},
		[]db.MRReviewThread{{ProviderThreadID: "stale-thread", CreatedAt: oldUpdatedAt, UpdatedAt: oldUpdatedAt}},
		true, nil, nil, "",
	)
	require.NoError(err)
	assert.False(applied)
	events, err := database.ListMREvents(ctx, mrID)
	require.NoError(err)
	keys := make([]string, len(events))
	for i := range events {
		keys[i] = events[i].DedupeKey
	}
	assert.ElementsMatch([]string{"current-comment", "current-review", "current-inline"}, keys)
	threads, err := database.ListMRReviewThreads(ctx, mrID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("current-thread", threads[0].ProviderThreadID)
}

func TestSyncArchiveItemClassifiesOnlyConfirmedParentNotFound(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitLab, Host: "gitlab.example.com",
		Owner: "group", Name: "project", RepoPath: "group/project",
	}
	issue := platform.Issue{
		Repo: ref, PlatformID: 7, PlatformExternalID: "issue-7", Number: 7,
		Title: "issue", State: "open", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}
	tests := []struct {
		name            string
		issues          []platform.Issue
		getIssueErr     error
		listEventsErr   error
		repositoryErr   error
		wantNotPresent  bool
		wantRepoQueries int32
	}{
		{
			name: "accessible repository confirms missing parent", getIssueErr: platform.ErrNotFound,
			wantNotPresent: true, wantRepoQueries: 2,
		},
		{
			name:           "provider metadata already confirms missing parent",
			getIssueErr:    errors.Join(platform.ErrLookupNotPresent, platform.ErrNotFound),
			wantNotPresent: true, wantRepoQueries: 1,
		},
		{
			name: "child event not found stays retryable", issues: []platform.Issue{issue},
			listEventsErr: platform.ErrNotFound, wantRepoQueries: 1,
		},
		{
			name: "missing repository does not prove missing parent", getIssueErr: platform.ErrNotFound,
			repositoryErr: platform.ErrNotFound, wantRepoQueries: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := openTestDB(t)
			identity := verifiedDBRepoIdentity(ref)
			_, err := database.UpsertRepoByProviderID(t.Context(), identity)
			require.NoError(err)
			provider := &syncTestRepositoryReadProvider{
				syncTestReadProvider: &syncTestReadProvider{
					kind: ref.Platform, host: ref.Host,
					issues: test.issues, getIssueErr: test.getIssueErr,
					listIssueEventsErr: test.listEventsErr,
				},
				repository: platform.Repository{
					Ref: ref, PlatformExternalID: identity.PlatformRepoID,
				},
				repositoryErr: test.repositoryErr,
			}
			registry, err := platform.NewRegistry(provider)
			require.NoError(err)
			syncer := NewSyncerWithRegistry(registry, database, nil, []RepoRef{{
				Platform: ref.Platform, PlatformHost: ref.Host,
				Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
			}}, time.Minute, nil, nil)

			_, err = syncer.SyncArchiveItem(t.Context(), ref, db.ArchiveItemTypeIssue, issue.Number)
			require.Error(err)
			assert.Equal(test.wantNotPresent, errors.Is(err, platform.ErrLookupNotPresent))
			assert.Equal(test.wantRepoQueries, provider.getRepositoryCalls.Load())
		})
	}
}

func TestArchiveItemSyncCostIncludesProviderConfirmationAndAuthRetry(t *testing.T) {
	syncer := &Syncer{}
	tests := []struct {
		name     string
		kind     platform.Kind
		itemType db.ArchiveItemType
		want     int
	}{
		{name: "GitHub pull request", kind: platform.KindGitHub, itemType: db.ArchiveItemTypeMergeRequest, want: 20},
		{name: "GitLab pull request", kind: platform.KindGitLab, itemType: db.ArchiveItemTypeMergeRequest, want: 22},
		{name: "Gitea pull request", kind: platform.KindGitea, itemType: db.ArchiveItemTypeMergeRequest, want: 22},
		{name: "Forgejo pull request", kind: platform.KindForgejo, itemType: db.ArchiveItemTypeMergeRequest, want: 22},
		{name: "GitHub issue", kind: platform.KindGitHub, itemType: db.ArchiveItemTypeIssue, want: 4},
		{name: "Forgejo issue", kind: platform.KindForgejo, itemType: db.ArchiveItemTypeIssue, want: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, syncer.ArchiveItemSyncCost(test.kind, test.itemType))
		})
	}
}

func TestCommitMergeRequestParentSnapshotRollsBackParentWhenLabelsFail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	stored := &db.MergeRequest{
		RepoID: repoID, PlatformID: 1, PlatformExternalID: "mr-1", Number: 1,
		Title: "stored", State: db.MergeRequestStateOpen, CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now, LastActivityAt: now,
	}
	mrID, err := database.UpsertMergeRequest(ctx, stored)
	require.NoError(err)
	require.NoError(database.ReplaceMergeRequestLabels(ctx, repoID, mrID, []db.Label{{
		RepoID: repoID, PlatformID: 1, Name: "stored", Color: "ffffff", UpdatedAt: now,
	}}))
	_, err = database.WriteDB().ExecContext(ctx, `
		CREATE TRIGGER reject_new_mr_label BEFORE INSERT ON forge_merge_request_labels
		BEGIN SELECT RAISE(ABORT, 'synthetic label failure'); END`)
	require.NoError(err)

	updated := *stored
	updated.Title = "must roll back"
	updated.UpdatedAt = now.Add(time.Minute)
	updated.LastActivityAt = updated.UpdatedAt
	updated.Labels = []db.Label{{
		RepoID: repoID, PlatformID: 2, Name: "new", Color: "000000", UpdatedAt: updated.UpdatedAt,
	}}
	syncer := &Syncer{db: database}
	_, _, _, err = syncer.CommitMergeRequestParentSnapshot(ctx, RepoRef{}, &updated)
	require.Error(err)

	result, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal("stored", result.Title)
	require.Len(result.Labels, 1)
	assert.Equal("stored", result.Labels[0].Name)
}

func setupBareRemoteForSyncTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	cmd := gitcmd.New().Command(t.Context(), dir, "init", "--bare", "--initial-branch=main", remote)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)
	return remote
}

type syncBranchActivityFixture struct {
	DB       *db.DB
	Repo     RepoRef
	Remote   string
	Work     string
	Provider *syncTestRepositoryReadProvider
	Syncer   *Syncer
}

func setupSyncBranchActivityFixture(t *testing.T, defaultBranch string) syncBranchActivityFixture {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	syncActivityGitRun(t, dir, "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(dir, "work")
	syncActivityGitRun(t, dir, "clone", remote, work)
	syncActivityGitRun(t, work, "config", "user.email", "alice@example.com")
	syncActivityGitRun(t, work, "config", "user.name", "Alice")

	d := openTestDB(t)
	clones := gitclone.New(t.TempDir(), nil)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformExternalID: "gid://gitlab/Project/branch-activity",
		CloneURL:           remote,
		DefaultBranch:      defaultBranch,
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "gitlab.example.com",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform:           platform.KindGitLab,
				Host:               "gitlab.example.com",
				Owner:              "group",
				Name:               "project",
				RepoPath:           "group/project",
				PlatformExternalID: "gid://gitlab/Project/branch-activity",
				CloneURL:           remote,
				DefaultBranch:      defaultBranch,
			},
			PlatformExternalID: "gid://gitlab/Project/branch-activity",
			CloneURL:           remote,
			DefaultBranch:      defaultBranch,
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	syncer := NewSyncerWithRegistry(registry, d, clones, []RepoRef{repo}, time.Minute, nil, nil)
	return syncBranchActivityFixture{
		DB:       d,
		Repo:     repo,
		Remote:   remote,
		Work:     work,
		Provider: provider,
		Syncer:   syncer,
	}
}

func syncActivityGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
	return strings.TrimSpace(string(out))
}

func syncActivityCommit(t *testing.T, work, fileName, contents, subject string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(work, fileName), []byte(contents), 0o644))
	syncActivityGitRun(t, work, "add", fileName)
	syncActivityGitRun(t, work, "commit", "-m", subject)
	return syncActivityGitRun(t, work, "rev-parse", "HEAD")
}

func syncActivityCommitAndPush(
	t *testing.T,
	work, fileName, contents, subject, branch string,
) string {
	t.Helper()
	sha := syncActivityCommit(t, work, fileName, contents, subject)
	syncActivityGitRun(t, work, "push", "origin", "HEAD:"+branch)
	return sha
}

func syncActivityItems(
	t *testing.T,
	d *db.DB,
	types ...string,
) []db.ActivityItem {
	t.Helper()
	items, err := d.ListActivity(t.Context(), db.ListActivityOpts{
		Limit: 50,
		Types: types,
	})
	require.NoError(t, err)
	return items
}

func syncActivityBranchCommits(t *testing.T, d *db.DB) []db.ActivityItem {
	t.Helper()
	return syncActivityItems(t, d, "default_branch_commit")
}

func syncActivityForcePushes(t *testing.T, d *db.DB) []db.ActivityItem {
	t.Helper()
	return syncActivityItems(t, d, "default_branch_force_push")
}

func requireSyncActivityRepoRow(t *testing.T, d *db.DB) db.Repo {
	t.Helper()
	repoRow, err := d.GetRepoByIdentity(t.Context(), db.RepoIdentity{
		Platform:     string(platform.KindGitLab),
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
	})
	require.NoError(t, err)
	require.NotNil(t, repoRow)
	return *repoRow
}

// recordingCloneRoutes records the platform every managed-Git credential
// lookup is made with, so tests can prove clone calls route by the same
// normalized identity the rest of sync uses.
type recordingCloneRoutes struct {
	mu        sync.Mutex
	platforms []string
}

func (r *recordingCloneRoutes) SourceForRepo(
	platformName, _, _, _ string,
) tokenauth.Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.platforms = append(r.platforms, platformName)
	return nil
}

func (r *recordingCloneRoutes) FallbackSource(string) tokenauth.Source {
	return nil
}

func (r *recordingCloneRoutes) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.platforms)
}

// A RepoRef may carry no platform: every other sync path normalizes that to
// GitHub, so the clone manager must too. Handing it the empty string instead
// picks no credential route, which drops private fetches and lets public ones
// run outside identity routing.
func TestSyncRepoRoutesCloneCredentialsForUnqualifiedGitHubRepoRef(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	syncActivityGitRun(t, dir, "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(dir, "work")
	syncActivityGitRun(t, dir, "clone", remote, work)
	syncActivityGitRun(t, work, "config", "user.email", "alice@example.com")
	syncActivityGitRun(t, work, "config", "user.name", "Alice")
	syncActivityCommitAndPush(
		t, work, "direct.txt", "direct work\n", "direct work", "main",
	)

	d := openTestDB(t)
	routes := &recordingCloneRoutes{}
	clones := gitclone.New(t.TempDir(), routes)
	repo := RepoRef{
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformExternalID: "R_unqualified_ref",
		CloneURL:           remote,
		DefaultBranch:      "main",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitHub,
			host: "github.com",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform:           platform.KindGitHub,
				Host:               "github.com",
				Owner:              "acme",
				Name:               "widget",
				RepoPath:           "acme/widget",
				PlatformExternalID: "R_unqualified_ref",
				CloneURL:           remote,
				DefaultBranch:      "main",
			},
			PlatformExternalID: "R_unqualified_ref",
			CloneURL:           remote,
			DefaultBranch:      "main",
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, d, clones, []RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	require.NoError(syncer.syncRepo(t.Context(), repo))

	seen := routes.seen()
	require.NotEmpty(seen, "managed Git resolved a credential for the clone")
	for _, platformName := range seen {
		check.Equal(string(platform.KindGitHub), platformName,
			"an unqualified GitHub ref must not bypass credential routing")
	}
}

func TestSyncRepoRecordsDefaultBranchCommits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupSyncBranchActivityFixture(t, "main")
	sha := syncActivityCommitAndPush(
		t,
		fixture.Work,
		"direct.txt",
		"direct work\n",
		"direct work",
		"main",
	)

	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	commits := syncActivityBranchCommits(t, fixture.DB)
	require.NotEmpty(commits)
	var direct db.ActivityItem
	for _, item := range commits {
		if item.CommitSHA == sha {
			direct = item
			break
		}
	}
	require.NotEmpty(direct.CommitSHA)
	assert.Equal("default_branch_commit", direct.ActivityType)
	assert.Equal("main", direct.BranchName)
	assert.Equal("direct work", direct.BodyPreview)
	assert.Equal("Alice", direct.AuthorName)
	assert.Equal("alice@example.com", direct.AuthorEmail)
	assert.Equal("Alice", direct.CommitterName)
	assert.Equal("alice@example.com", direct.CommitterEmail)
	assert.NotNil(direct.AuthoredAt)
	assert.NotNil(direct.CommittedAt)

	repoRow := requireSyncActivityRepoRow(t, fixture.DB)
	tip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "main")
	require.NoError(err)
	require.NotNil(tip)
	assert.Equal(sha, tip.TipSHA)
	assert.Empty(syncActivityForcePushes(t, fixture.DB))
}

func TestSyncRepoCapsDefaultBranchCommits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupSyncBranchActivityFixture(t, "main")
	fixture.Syncer.SetBranchActivityLimits(90*24*time.Hour, 2)

	var shas []string
	for i := range 4 {
		suffix := string(rune('0' + i))
		shas = append(shas, syncActivityCommit(
			t,
			fixture.Work,
			"direct-"+suffix+".txt",
			"direct "+suffix+"\n",
			"direct work "+suffix,
		))
	}
	syncActivityGitRun(t, fixture.Work, "push", "origin", "HEAD:main")

	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	commits := syncActivityBranchCommits(t, fixture.DB)
	require.Len(commits, 2)
	var got []string
	for _, item := range commits {
		got = append(got, item.CommitSHA)
	}
	assert.ElementsMatch([]string{shas[3], shas[2]}, got)
}

func TestSyncRepoRecordsDefaultBranchForcePushBeforeUpdatingTip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupSyncBranchActivityFixture(t, "main")
	beforeSHA := syncActivityCommitAndPush(
		t,
		fixture.Work,
		"before.txt",
		"before\n",
		"before rewrite",
		"main",
	)
	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	syncActivityGitRun(t, fixture.Work, "checkout", "--orphan", "rewrite")
	syncActivityGitRun(t, fixture.Work, "rm", "-r", "--cached", ".")
	afterSHA := syncActivityCommit(t, fixture.Work, "after.txt", "after\n", "after rewrite")
	syncActivityGitRun(t, fixture.Work, "push", "--force", "origin", "HEAD:main")

	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	forcePushes := syncActivityForcePushes(t, fixture.DB)
	require.Len(forcePushes, 1)
	assert.Equal("default_branch_force_push", forcePushes[0].ActivityType)
	assert.Equal("main", forcePushes[0].BranchName)
	assert.Equal(beforeSHA, forcePushes[0].BeforeSHA)
	assert.Equal(afterSHA, forcePushes[0].AfterSHA)

	repoRow := requireSyncActivityRepoRow(t, fixture.DB)
	tip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "main")
	require.NoError(err)
	require.NotNil(tip)
	assert.Equal(afterSHA, tip.TipSHA)
}

func TestSyncRepoSkipsBranchActivityWhenCloneFetchFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupSyncBranchActivityFixture(t, "main")
	initialSHA := syncActivityCommitAndPush(
		t,
		fixture.Work,
		"initial.txt",
		"initial\n",
		"initial branch activity",
		"main",
	)
	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))
	repoRow := requireSyncActivityRepoRow(t, fixture.DB)
	initialTip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "main")
	require.NoError(err)
	require.NotNil(initialTip)
	initialCommitCount := len(syncActivityBranchCommits(t, fixture.DB))

	newSHA := syncActivityCommitAndPush(
		t,
		fixture.Work,
		"new.txt",
		"new\n",
		"new branch activity",
		"main",
	)
	offlineRemote := fixture.Remote + ".offline"
	require.NoError(os.Rename(fixture.Remote, offlineRemote))
	defer func() {
		if _, err := os.Stat(fixture.Remote); errors.Is(err, os.ErrNotExist) {
			require.NoError(os.Rename(offlineRemote, fixture.Remote))
		}
	}()

	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	tip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "main")
	require.NoError(err)
	require.NotNil(tip)
	assert.Equal(initialSHA, tip.TipSHA)
	assert.Equal(initialTip.ObservedAt, tip.ObservedAt)
	assert.Len(syncActivityBranchCommits(t, fixture.DB), initialCommitCount)
	for _, item := range syncActivityBranchCommits(t, fixture.DB) {
		assert.NotEqual(newSHA, item.CommitSHA)
	}
	assert.Empty(syncActivityForcePushes(t, fixture.DB))
}

func TestSyncMRDiffPreservesCloneContextCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	cancelFile := filepath.Join(dir, "cancel-context")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
case "$*" in
	"config --get remote.origin.url")
		echo "https://github.com/acme/widgets.git"
		;;
	"config --get-all remote.origin.fetch")
		echo "+refs/heads/*:refs/remotes/origin/*"
		echo "+refs/pull/*/head:refs/pull/*/head"
		;;
	"for-each-ref --format=%(refname)%09%(objectname)%09%(symref)")
		;;
	"fetch --prune --no-tags origin")
		;;
	"remote set-head origin -a")
		touch "${KENN_FORGE_TEST_CANCEL_FILE:?}"
		sleep 10
		;;
	merge-base*)
		echo "merge-base should not run after cancellation" >&2
		exit 2
		;;
	*)
		echo "unexpected git $*" >&2
		exit 2
		;;
esac
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_CANCEL_FILE", cancelFile)

	database := openTestDB(t)
	clones := gitclone.New(filepath.Join(dir, "clones"), nil)
	repo := RepoRef{
		Owner:        "acme",
		Name:         "widgets",
		PlatformHost: "github.com",
		CloneURL:     "https://github.com/acme/widgets.git",
	}
	clonePath, err := clones.ClonePath("github", "github.com", repo.Owner, repo.Name)
	require.NoError(err)
	require.NoError(os.MkdirAll(clonePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(clonePath, "HEAD"),
		[]byte("ref: refs/heads/main\n"),
		0o644,
	))
	repoID, err := database.UpsertRepo(
		t.Context(),
		verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	routeFence, found, err := database.CurrentRepositoryRouteFence(
		t.Context(), platform.DBRepoIdentity(platformRepoRef(repo)), repoID,
	)
	require.NoError(err)
	require.True(found)
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		database,
		clones,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(cancelFile); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	err = syncer.syncMRDiff(
		ctx,
		repo,
		repoID,
		0,
		0,
		1,
		&gh.PullRequest{},
		&db.MergeRequest{
			PlatformHeadSHA: "head-sha",
			PlatformBaseSHA: "base-sha",
		},
		routeFence,
	)

	require.Error(err)
	require.ErrorIs(err, context.Canceled)
	var diffErr *DiffSyncError
	require.ErrorAs(err, &diffErr)
	assert.Equal(DiffSyncCodeCloneUnavailable, diffErr.Code)
}

func TestSyncRepoDefaultBranchRenameDoesNotRecordForcePush(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupSyncBranchActivityFixture(t, "main")
	mainSHA := syncActivityCommitAndPush(
		t,
		fixture.Work,
		"main.txt",
		"main\n",
		"main branch work",
		"main",
	)
	require.NoError(fixture.Syncer.syncRepo(t.Context(), fixture.Repo))

	syncActivityGitRun(t, fixture.Work, "checkout", "--orphan", "trunk")
	syncActivityGitRun(t, fixture.Work, "rm", "-r", "--cached", ".")
	trunkSHA := syncActivityCommit(t, fixture.Work, "trunk.txt", "trunk\n", "trunk branch work")
	syncActivityGitRun(t, fixture.Work, "push", "origin", "HEAD:trunk")
	syncActivityGitRun(t, fixture.Remote, "symbolic-ref", "HEAD", "refs/heads/trunk")

	renamedRepo := fixture.Repo
	fixture.Provider.repository.Ref.DefaultBranch = "trunk"
	fixture.Provider.repository.DefaultBranch = "trunk"
	require.NoError(fixture.Syncer.syncRepo(t.Context(), renamedRepo))

	assert.Empty(syncActivityForcePushes(t, fixture.DB))
	repoRow := requireSyncActivityRepoRow(t, fixture.DB)
	mainTip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "main")
	require.NoError(err)
	require.NotNil(mainTip)
	assert.Equal(mainSHA, mainTip.TipSHA)
	trunkTip, err := fixture.DB.GetBranchTip(t.Context(), repoRow.ID, "trunk")
	require.NoError(err)
	require.NotNil(trunkTip)
	assert.Equal(trunkSHA, trunkTip.TipSHA)
}

// testBudget builds a per-host budget map for use in NewSyncer calls.
func testBudget(limit int) map[string]*SyncBudget {
	return map[string]*SyncBudget{
		"github.com": NewSyncBudget(limit),
	}
}

// mockClient implements Client with configurable canned responses.
type mockClient struct {
	budget                          *SyncBudget // optional: simulates transport counting
	openPRs                         []*gh.PullRequest
	openIssues                      []*gh.Issue
	listOpenPRsErr                  error
	listOpenIssuesErr               error
	singlePR                        *gh.PullRequest
	createIssueFn                   func(context.Context, string, string, string, string) (*gh.Issue, error)
	getRepositoryFn                 func(context.Context, string, string) (*gh.Repository, error)
	getPullRequestFn                func(context.Context, string, string, int) (*gh.PullRequest, error)
	getIssueFn                      func(context.Context, string, string, int) (*gh.Issue, error)
	getUserFn                       func(context.Context, string) (*gh.User, error)
	authenticatedViewerLoginFn      func(context.Context) (string, error)
	authenticatedViewerCalls        atomic.Int32
	listReposByOwnerFn              func(context.Context, string) ([]*gh.Repository, error)
	listReleases                    []*gh.RepositoryRelease
	listReleasesErr                 error
	listReleasesFn                  func(context.Context, string, string, int) ([]*gh.RepositoryRelease, error)
	listTags                        []*gh.RepositoryTag
	listTagsErr                     error
	listTagsFn                      func(context.Context, string, string, int) ([]*gh.RepositoryTag, error)
	listOpenPRsFn                   func(context.Context, string, string) ([]*gh.PullRequest, error)
	listPullRequestsPageFn          func(context.Context, string, string, string, int) ([]*gh.PullRequest, bool, error)
	listIssuesPageFn                func(context.Context, string, string, string, int) ([]*gh.Issue, bool, error)
	listNotificationsFn             func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error)
	bypassNotificationReadReserve   bool
	getNotificationThreadFn         func(context.Context, string) (NotificationThread, error)
	markNotificationThreadReadFn    func(context.Context, string) error
	comments                        []*gh.IssueComment
	reviews                         []*gh.PullRequestReview
	reviewThreads                   []PullRequestReviewThread
	commits                         []*gh.RepositoryCommit
	timelineEvents                  []PullRequestTimelineEvent
	timelineEventsErr               error
	forcePushEvents                 []ForcePushEvent
	forcePushEventsErr              error
	ciStatus                        *gh.CombinedStatus
	ciStatusErr                     error
	checkRuns                       []*gh.CheckRun
	checkRunsErr                    error
	listCheckRunsForRefFn           func(context.Context, string, string, string) ([]*gh.CheckRun, error)
	workflowRuns                    []*gh.WorkflowRun
	listWorkflowRunsFn              func(context.Context, string, string, string) ([]*gh.WorkflowRun, error)
	approveWorkflowRunFn            func(context.Context, string, string, int64) error
	listOpenPRsCalled               atomic.Bool
	getUserCalls                    atomic.Int32
	getCombinedCalls                atomic.Int32
	invalidateCalls                 atomic.Int32
	listIssueCommentsCalled         atomic.Int32
	listIssueCommentsIfChangedCalls atomic.Int32
	listIssueCommentsErr            error
	listIssueCommentsFn             func(context.Context, string, string, int) ([]*gh.IssueComment, error)
	listIssueCommentsIfChangedFn    func(context.Context, string, string, int) ([]*gh.IssueComment, error)
	createReviewWithCommentsFn      func(context.Context, string, string, int, string, string, string, []*gh.DraftReviewComment) (*gh.PullRequestReview, error)
	createdReviewEvent              string
	createdReviewBody               string
	createdReviewCommitID           string
	createdReviewComments           []*gh.DraftReviewComment
	applyReviewSuggestionsFn        func(context.Context, string, string, int, platform.ApplyReviewSuggestionsInput) (*platform.AppliedReviewSuggestions, error)
	appliedReviewSuggestions        []platform.ApplyReviewSuggestionsInput
	dismissReviewErr                error
	dismissedReviewID               int64
	dismissedReviewMessage          string
	dismissReviewCalls              atomic.Int32
}

type issueTimelineMockClient struct {
	mockClient
	issueTimelineCalls atomic.Int32
	issueTimelineErr   error
}

func (c *issueTimelineMockClient) ListIssueTimelineEvents(
	context.Context, string, string, int,
) ([]PullRequestTimelineEvent, error) {
	c.issueTimelineCalls.Add(1)
	return nil, c.issueTimelineErr
}

func (m *mockClient) bypassNotificationReadRateReserve() bool {
	return m.bypassNotificationReadReserve
}

type rateLimitSnapshotMockClient struct {
	*mockClient
	snapshot           *RateLimitSnapshot
	snapshotCalls      atomic.Int32
	syncBudgetContexts atomic.Int32
}

type credentialRateLimitSnapshotMockClient struct {
	*mockClient
	appSnapshot  *RateLimitSnapshot
	userSnapshot *RateLimitSnapshot
	appCalls     atomic.Int32
	userCalls    atomic.Int32
}

func (m *credentialRateLimitSnapshotMockClient) GetRateLimitSnapshot(
	ctx context.Context,
) (*RateLimitSnapshot, error) {
	if tokenauth.IsMutationAuth(ctx) {
		m.userCalls.Add(1)
		return m.userSnapshot, nil
	}
	m.appCalls.Add(1)
	return m.appSnapshot, nil
}

func (m *rateLimitSnapshotMockClient) GetRateLimitSnapshot(ctx context.Context) (*RateLimitSnapshot, error) {
	m.snapshotCalls.Add(1)
	if IsSyncBudgetContext(ctx) {
		m.syncBudgetContexts.Add(1)
	}
	return m.snapshot, nil
}

type labelCatalogTestClient struct {
	*mockClient
	labels           []*gh.Label
	listRepoLabelsFn func(context.Context, string, string) ([]*gh.Label, error)
	calls            atomic.Int32
}

func (c *labelCatalogTestClient) ListRepoLabels(
	ctx context.Context, owner, repo string,
) ([]*gh.Label, error) {
	c.calls.Add(1)
	if c.listRepoLabelsFn != nil {
		return c.listRepoLabelsFn(ctx, owner, repo)
	}
	return append([]*gh.Label(nil), c.labels...), nil
}

func (c *labelCatalogTestClient) ReplaceIssueLabels(
	_ context.Context, _, _ string, _ int, names []string,
) ([]*gh.Label, error) {
	byName := make(map[string]*gh.Label, len(c.labels))
	for _, label := range c.labels {
		byName[label.GetName()] = label
	}
	labels := make([]*gh.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, byName[name])
	}
	return labels, nil
}

type syncTestProvider struct {
	kind platform.Kind
	host string
}

func (p syncTestProvider) Platform() platform.Kind {
	return p.kind
}

func (p syncTestProvider) Host() string {
	return p.host
}

func (p syncTestProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{}
}

type syncTestReadProvider struct {
	syncTestProvider
	mergeRequests       []platform.MergeRequest
	issues              []platform.Issue
	getIssueErr         error
	listIssueEventsErr  error
	listMRCalls         atomic.Int32
	listIssueCalls      atomic.Int32
	getMRCalls          atomic.Int32
	getIssueCalls       atomic.Int32
	listReviewThreads   atomic.Int32
	listMRMergeEvents   []platform.MergeRequestEvent
	reviewThreads       []platform.MergeRequestReviewThread
	listReviewThreadsFn func(context.Context, platform.RepoRef, int) ([]platform.MergeRequestReviewThread, error)
	listIssueReadEvents []platform.IssueEvent
	readReviewThreads   bool
	requireRepoPath     string
}

func (p *syncTestReadProvider) GetRepository(
	_ context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	identity := verifiedDBRepoIdentity(ref)
	return platform.Repository{
		Ref:                ref,
		PlatformExternalID: identity.PlatformRepoID,
	}, nil
}

func (p *syncTestReadProvider) ListRepositories(
	context.Context,
	string,
	platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return nil, nil
}

type syncTestRepositoryReadProvider struct {
	*syncTestReadProvider
	repository         platform.Repository
	repositoryErr      error
	getRepositoryFn    func(context.Context, platform.RepoRef) (platform.Repository, error)
	getRepositoryCalls atomic.Int32
}

type syncTestMergeRequestOnlyProvider struct {
	syncTestProvider
	mergeRequests []platform.MergeRequest
	listMRCalls   atomic.Int32
}

type syncTestBudgetThenBrokenIssueProvider struct {
	syncTestProvider
	getMRErr error
}

func (p *syncTestBudgetThenBrokenIssueProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadMergeRequests: true, ReadIssues: true}
}

func (p *syncTestBudgetThenBrokenIssueProvider) ListOpenMergeRequests(
	context.Context,
	platform.RepoRef,
) ([]platform.MergeRequest, error) {
	return nil, nil
}

func (p *syncTestBudgetThenBrokenIssueProvider) GetMergeRequest(
	context.Context,
	platform.RepoRef,
	int,
) (platform.MergeRequest, error) {
	return platform.MergeRequest{}, p.getMRErr
}

func (p *syncTestBudgetThenBrokenIssueProvider) ListMergeRequestEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestEvent, error) {
	return nil, nil
}

func (p *syncTestMergeRequestOnlyProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadMergeRequests: true}
}

func (p *syncTestMergeRequestOnlyProvider) ListOpenMergeRequests(
	context.Context,
	platform.RepoRef,
) ([]platform.MergeRequest, error) {
	p.listMRCalls.Add(1)
	return p.mergeRequests, nil
}

func (p *syncTestMergeRequestOnlyProvider) GetMergeRequest(
	context.Context,
	platform.RepoRef,
	int,
) (platform.MergeRequest, error) {
	return platform.MergeRequest{}, errors.New("missing merge request")
}

type syncTestIssueOnlyProvider struct {
	syncTestProvider
	issues         []platform.Issue
	listIssueCalls atomic.Int32
}

func (p *syncTestIssueOnlyProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadIssues: true}
}

func (p *syncTestIssueOnlyProvider) ListOpenIssues(
	context.Context,
	platform.RepoRef,
) ([]platform.Issue, error) {
	p.listIssueCalls.Add(1)
	return p.issues, nil
}

func (p *syncTestIssueOnlyProvider) GetIssue(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.Issue, error) {
	for _, issue := range p.issues {
		if issue.Number == number {
			return issue, nil
		}
	}
	return platform.Issue{}, errors.New("missing issue")
}

func (p *syncTestIssueOnlyProvider) ListIssueEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.IssueEvent, error) {
	return nil, nil
}

func (p *syncTestMergeRequestOnlyProvider) ListMergeRequestEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestEvent, error) {
	return nil, nil
}

func (p *syncTestReadProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadMergeRequests: true,
		ReadIssues:        true,
		ReadReviewThreads: p.readReviewThreads,
	}
}

func (p *syncTestRepositoryReadProvider) GetRepository(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	p.getRepositoryCalls.Add(1)
	if p.getRepositoryFn != nil {
		return p.getRepositoryFn(ctx, ref)
	}
	return p.repository, p.repositoryErr
}

func (p *syncTestRepositoryReadProvider) ListRepositories(
	context.Context,
	string,
	platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return nil, nil
}

func (p *syncTestReadProvider) ListOpenMergeRequests(
	_ context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	p.listMRCalls.Add(1)
	if p.requireRepoPath != "" && ref.RepoPath != p.requireRepoPath {
		return nil, fmt.Errorf("merge request list used stale repository path %q", ref.RepoPath)
	}
	return p.mergeRequests, nil
}

func (p *syncTestReadProvider) GetMergeRequest(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	p.getMRCalls.Add(1)
	for _, mr := range p.mergeRequests {
		if mr.Number == number {
			return mr, nil
		}
	}
	return platform.MergeRequest{}, errors.New("missing merge request")
}

func (p *syncTestReadProvider) ListMergeRequestEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestEvent, error) {
	return p.listMRMergeEvents, nil
}

func (p *syncTestReadProvider) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	p.listReviewThreads.Add(1)
	if p.listReviewThreadsFn != nil {
		return p.listReviewThreadsFn(ctx, ref, number)
	}
	return p.reviewThreads, nil
}

func (p *syncTestReadProvider) ListOpenIssues(
	_ context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	p.listIssueCalls.Add(1)
	if p.requireRepoPath != "" && ref.RepoPath != p.requireRepoPath {
		return nil, fmt.Errorf("issue list used stale repository path %q", ref.RepoPath)
	}
	return p.issues, nil
}

func (p *syncTestReadProvider) GetIssue(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.Issue, error) {
	p.getIssueCalls.Add(1)
	if p.getIssueErr != nil {
		return platform.Issue{}, p.getIssueErr
	}
	for _, issue := range p.issues {
		if issue.Number == number {
			return issue, nil
		}
	}
	return platform.Issue{}, errors.New("missing issue")
}

func (p *syncTestReadProvider) ListIssueEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.IssueEvent, error) {
	return p.listIssueReadEvents, p.listIssueEventsErr
}

func (m *mockClient) trackCall() {
	if m.budget != nil {
		m.budget.Spend(1)
	}
}

func (m *mockClient) InvalidateListETagsForRepo(_, _ string, _ ...string) {
	m.invalidateCalls.Add(1)
}

func (m *mockClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	m.trackCall()
	m.listOpenPRsCalled.Store(true)
	if m.listOpenPRsFn != nil {
		return m.listOpenPRsFn(ctx, owner, repo)
	}
	if m.listOpenPRsErr != nil {
		return nil, m.listOpenPRsErr
	}
	return m.openPRs, nil
}

func (m *mockClient) ListOpenIssues(_ context.Context, _, _ string) ([]*gh.Issue, error) {
	m.trackCall()
	if m.listOpenIssuesErr != nil {
		return nil, m.listOpenIssuesErr
	}
	return m.openIssues, nil
}

func (m *mockClient) GetIssue(
	ctx context.Context, owner, repo string, number int,
) (*gh.Issue, error) {
	m.trackCall()
	if m.getIssueFn != nil {
		return m.getIssueFn(ctx, owner, repo, number)
	}
	return nil, nil
}

func (m *mockClient) CreateIssue(
	ctx context.Context, owner, repo, title, body string,
) (*gh.Issue, error) {
	m.trackCall()
	if m.createIssueFn != nil {
		return m.createIssueFn(ctx, owner, repo, title, body)
	}
	return nil, nil
}

func (m *mockClient) GetUser(ctx context.Context, login string) (*gh.User, error) {
	m.trackCall()
	m.getUserCalls.Add(1)
	if m.getUserFn != nil {
		return m.getUserFn(ctx, login)
	}
	name := "Display " + login
	return &gh.User{Login: &login, Name: &name}, nil
}

func (m *mockClient) AuthenticatedViewerLogin(ctx context.Context) (string, error) {
	m.trackCall()
	m.authenticatedViewerCalls.Add(1)
	if m.authenticatedViewerLoginFn != nil {
		return m.authenticatedViewerLoginFn(ctx)
	}
	return "", nil
}

func (m *mockClient) ListRepositoriesByOwner(
	ctx context.Context, owner string,
) ([]*gh.Repository, error) {
	m.trackCall()
	if m.listReposByOwnerFn != nil {
		return m.listReposByOwnerFn(ctx, owner)
	}
	return nil, nil
}

func (m *mockClient) ListReleases(
	ctx context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryRelease, error) {
	m.trackCall()
	if m.listReleasesFn != nil {
		return m.listReleasesFn(ctx, owner, repo, perPage)
	}
	if m.listReleasesErr != nil {
		return nil, m.listReleasesErr
	}
	return m.listReleases, nil
}

func (m *mockClient) ListTags(
	ctx context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryTag, error) {
	m.trackCall()
	if m.listTagsFn != nil {
		return m.listTagsFn(ctx, owner, repo, perPage)
	}
	if m.listTagsErr != nil {
		return nil, m.listTagsErr
	}
	return m.listTags, nil
}

func (m *mockClient) GetPullRequest(
	ctx context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	m.trackCall()
	if m.getPullRequestFn != nil {
		return m.getPullRequestFn(ctx, owner, repo, number)
	}
	if m.singlePR != nil {
		return m.singlePR, nil
	}
	// Fall back to matching from the open PRs list
	for _, pr := range m.openPRs {
		if pr.GetNumber() == number {
			return pr, nil
		}
	}
	return nil, nil
}

func (m *mockClient) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	m.trackCall()
	m.listIssueCommentsCalled.Add(1)
	if m.listIssueCommentsFn != nil {
		return m.listIssueCommentsFn(ctx, owner, repo, number)
	}
	if m.listIssueCommentsErr != nil {
		return nil, m.listIssueCommentsErr
	}
	return m.comments, nil
}

func (m *mockClient) ListIssueCommentsIfChanged(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	m.listIssueCommentsIfChangedCalls.Add(1)
	if m.listIssueCommentsIfChangedFn != nil {
		return m.listIssueCommentsIfChangedFn(ctx, owner, repo, number)
	}
	if m.listIssueCommentsErr != nil {
		return nil, m.listIssueCommentsErr
	}
	if m.comments == nil {
		return nil, notModifiedErr()
	}
	return m.ListIssueComments(ctx, owner, repo, number)
}

func (m *mockClient) ListReviews(_ context.Context, _, _ string, _ int) ([]*gh.PullRequestReview, error) {
	m.trackCall()
	return m.reviews, nil
}

func (m *mockClient) ListPullRequestReviewThreads(
	_ context.Context,
	_ string,
	_ string,
	_ int,
) ([]PullRequestReviewThread, error) {
	m.trackCall()
	return m.reviewThreads, nil
}

func (m *mockClient) ListCommits(_ context.Context, _, _ string, _ int) ([]*gh.RepositoryCommit, error) {
	m.trackCall()
	return m.commits, nil
}

func (m *mockClient) ListForcePushEvents(_ context.Context, _, _ string, _ int) ([]ForcePushEvent, error) {
	m.trackCall()
	return m.forcePushEvents, m.forcePushEventsErr
}

func (m *mockClient) GetCombinedStatus(_ context.Context, _, _, _ string) (*gh.CombinedStatus, error) {
	m.trackCall()
	m.getCombinedCalls.Add(1)
	if m.ciStatusErr != nil {
		return nil, m.ciStatusErr
	}
	return m.ciStatus, nil
}

func (m *mockClient) ListCheckRunsForRef(ctx context.Context, owner, repo, ref string) ([]*gh.CheckRun, error) {
	m.trackCall()
	if m.listCheckRunsForRefFn != nil {
		return m.listCheckRunsForRefFn(ctx, owner, repo, ref)
	}
	if m.checkRunsErr != nil {
		return nil, m.checkRunsErr
	}
	return m.checkRuns, nil
}

func (m *mockClient) ListWorkflowRunsForHeadSHA(
	ctx context.Context, owner, repo, headSHA string,
) ([]*gh.WorkflowRun, error) {
	m.trackCall()
	if m.listWorkflowRunsFn != nil {
		return m.listWorkflowRunsFn(ctx, owner, repo, headSHA)
	}
	return m.workflowRuns, nil
}

func (m *mockClient) ApproveWorkflowRun(
	ctx context.Context, owner, repo string, runID int64,
) error {
	m.trackCall()
	if m.approveWorkflowRunFn != nil {
		return m.approveWorkflowRunFn(ctx, owner, repo, runID)
	}
	return nil
}

func (m *mockClient) CreateIssueComment(
	_ context.Context, _, _ string, _ int, _ string,
) (*gh.IssueComment, error) {
	m.trackCall()
	return nil, nil
}

func (m *mockClient) EditIssueComment(
	_ context.Context, _, _ string, _ int64, _ string,
) (*gh.IssueComment, error) {
	m.trackCall()
	return nil, nil
}

func (m *mockClient) DeleteIssueComment(context.Context, string, string, int64) error {
	m.trackCall()
	return nil
}

func (m *mockClient) CreatePullRequestReviewCommentReply(
	_ context.Context, _, _ string, _ int, _ string, _ int64,
) (*gh.PullRequestComment, error) {
	m.trackCall()
	return nil, nil
}

func (m *mockClient) GetRepository(
	ctx context.Context, owner, repo string,
) (*gh.Repository, error) {
	m.trackCall()
	if m.getRepositoryFn != nil {
		return m.getRepositoryFn(ctx, owner, repo)
	}
	id := int64(1)
	nodeID := "repo-" + owner + "-" + repo
	return &gh.Repository{
		ID:     &id,
		NodeID: &nodeID,
		Name:   &repo,
		Owner:  &gh.User{Login: &owner},
	}, nil
}

func (m *mockClient) CreateReview(
	ctx context.Context, owner, repo string, number int, event string, body string,
) (*gh.PullRequestReview, error) {
	return m.CreateReviewWithComments(ctx, owner, repo, number, event, body, "", nil)
}

func (m *mockClient) CreateReviewWithComments(
	ctx context.Context,
	owner, repo string,
	number int,
	event string,
	body string,
	commitID string,
	comments []*gh.DraftReviewComment,
) (*gh.PullRequestReview, error) {
	m.trackCall()
	if m.createReviewWithCommentsFn != nil {
		return m.createReviewWithCommentsFn(ctx, owner, repo, number, event, body, commitID, comments)
	}
	id := int64(1)
	submittedAt := gh.Timestamp{Time: time.Now().UTC()}
	m.createdReviewEvent = event
	m.createdReviewBody = body
	m.createdReviewCommitID = commitID
	m.createdReviewComments = comments
	return &gh.PullRequestReview{ID: &id, State: &event, SubmittedAt: &submittedAt}, nil
}

func (m *mockClient) ApplyReviewSuggestions(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	m.trackCall()
	if m.applyReviewSuggestionsFn != nil {
		return m.applyReviewSuggestionsFn(ctx, owner, repo, number, input)
	}
	m.appliedReviewSuggestions = append(m.appliedReviewSuggestions, input)
	return &platform.AppliedReviewSuggestions{CommitSHA: "suggestion-sha"}, nil
}

func TestGitHubProviderPublishDiffReviewDraftMapsReviewComments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	startLine := 10
	mock := &mockClient{}
	provider := gitHubClientProvider{client: mock, host: "github.com"}

	result, err := provider.PublishDiffReviewDraft(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7, platform.PublishDiffReviewDraftInput{
		Body:    "review summary",
		Action:  platform.ReviewActionRequestChanges,
		HeadSHA: "validated-head-sha",
		Comments: []platform.LocalDiffReviewDraftComment{{
			Body: "inline note",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				StartSide:   "right",
				StartLine:   &startLine,
				Line:        12,
				DiffHeadSHA: "head-sha",
				CommitSHA:   "stale-line-commit",
			},
		}},
	})

	require.NoError(err)
	require.NotNil(result)
	assert.Equal("1", result.ProviderReviewID)
	assert.False(result.SubmittedAt.IsZero())
	assert.Equal("REQUEST_CHANGES", mock.createdReviewEvent)
	assert.Equal("review summary", mock.createdReviewBody)
	assert.Equal("validated-head-sha", mock.createdReviewCommitID)
	require.Len(mock.createdReviewComments, 1)
	comment := mock.createdReviewComments[0]
	require.NotNil(comment)
	assert.Equal("src/main.go", comment.GetPath())
	assert.Equal("inline note", comment.GetBody())
	assert.Equal("RIGHT", comment.GetSide())
	require.NotNil(comment.StartSide)
	assert.Equal("RIGHT", *comment.StartSide)
	require.NotNil(comment.StartLine)
	assert.Equal(10, *comment.StartLine)
	assert.Equal(12, comment.GetLine())
}

func TestGitHubProviderPublishDiffReviewDraftApproveSubmitsReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockClient{}
	provider := gitHubClientProvider{client: mock, host: "github.com"}
	line := 12

	result, err := provider.PublishDiffReviewDraft(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7, platform.PublishDiffReviewDraftInput{
		Body:    "ship it",
		Action:  platform.ReviewActionApprove,
		HeadSHA: "reviewed-head",
		Comments: []platform.LocalDiffReviewDraftComment{{
			Body: "inline note",
			Range: platform.DiffReviewLineRange{
				Path: "src/main.go",
				Side: "right",
				Line: line,
			},
		}},
	})

	require.NoError(err)
	require.NotNil(result)
	assert.Equal("APPROVE", mock.createdReviewEvent)
	assert.Equal("ship it", mock.createdReviewBody)
	assert.Equal("reviewed-head", mock.createdReviewCommitID)
	require.Len(mock.createdReviewComments, 1)
	assert.Equal("inline note", mock.createdReviewComments[0].GetBody())
}

func TestGitHubProviderViewerAuthoredMergeRequestRefreshesExpiredCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logins := []string{"marius", "octocat"}
	mock := &mockClient{
		authenticatedViewerLoginFn: func(context.Context) (string, error) {
			next := logins[0]
			logins = logins[1:]
			return next, nil
		},
	}
	provider := &gitHubClientProvider{client: mock, host: "github.com"}
	mr := platform.MergeRequest{Author: "marius"}

	authored, err := provider.ViewerAuthoredMergeRequest(t.Context(), mr)
	require.NoError(err)
	assert.True(authored)

	authored, err = provider.ViewerAuthoredMergeRequest(t.Context(), mr)
	require.NoError(err)
	assert.True(authored)
	assert.EqualValues(1, mock.authenticatedViewerCalls.Load())

	provider.viewerMu.Lock()
	for key, entry := range provider.viewerLogins {
		entry.fetchedAt = time.Now().Add(-authenticatedViewerLoginTTL - time.Minute)
		provider.viewerLogins[key] = entry
	}
	provider.viewerMu.Unlock()

	authored, err = provider.ViewerAuthoredMergeRequest(t.Context(), mr)
	require.NoError(err)
	assert.False(authored)
	assert.EqualValues(2, mock.authenticatedViewerCalls.Load())
}

func TestGitHubProviderApplyReviewSuggestionsDelegatesToClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockClient{}
	provider := gitHubClientProvider{client: mock, host: "github.com"}
	input := platform.ApplyReviewSuggestionsInput{
		HeadBranch:      "feature",
		ExpectedHeadSHA: "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 12},
			Replacement: "return nil",
		}},
	}

	result, err := provider.ApplyReviewSuggestions(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7, input)

	require.NoError(err)
	assert.Equal("suggestion-sha", result.CommitSHA)
	require.Len(mock.appliedReviewSuggestions, 1)
	assert.Equal(input, mock.appliedReviewSuggestions[0])
}

func TestGitHubProviderCapabilitiesExposeReviewThreadReads(t *testing.T) {
	require := require.New(t)
	provider := gitHubClientProvider{client: &mockClient{}, host: "github.com"}

	caps := provider.Capabilities()

	require.True(caps.ReadReviewThreads)
	require.True(caps.ReviewDraftMutation)
	require.True(caps.ReviewSuggestionApplication)
	require.False(caps.ReviewThreadResolution)
	require.True(caps.ReviewMutation)
	require.Contains(caps.SupportedReviewActions, platform.ReviewActionApprove)
}

func TestGitHubProviderListMergeRequestReviewThreadsMapsGraphQLThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	startLine := 10
	createdAt := time.Date(2026, 5, 27, 16, 1, 31, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	mock := &mockClient{
		reviewThreads: []PullRequestReviewThread{{
			NodeID:     "PRRT_1",
			IsResolved: true,
			IsOutdated: false,
			Path:       "src/main.go",
			Side:       "RIGHT",
			StartLine:  &startLine,
			Line:       12,
			Comments: []PullRequestReviewThreadComment{{
				NodeID:           "PRRC_1",
				DatabaseID:       101,
				ReviewDatabaseID: 201,
				Body:             "inline note",
				AuthorLogin:      "reviewer",
				URL:              "https://github.com/acme/widget/pull/7#discussion_r101",
				CommitID:         "head-sha",
				OriginalCommitID: "original-sha",
				IsMinimized:      true,
				MinimizedReason:  "OFF_TOPIC",
				CreatedAt:        createdAt,
				UpdatedAt:        updatedAt,
			}, {
				NodeID:           "PRRC_2",
				DatabaseID:       102,
				ReviewDatabaseID: 201,
				Body:             "reply note",
				AuthorLogin:      "maintainer",
				CommitID:         "head-sha",
				OriginalCommitID: "original-sha",
				IsMinimized:      true,
				MinimizedReason:  "ABUSE",
				CreatedAt:        createdAt.Add(time.Minute),
				UpdatedAt:        updatedAt.Add(time.Minute),
			}},
		}},
	}
	provider := gitHubClientProvider{client: mock, host: "github.com"}

	threads, err := provider.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7)

	require.NoError(err)
	require.Len(threads, 2)
	thread := threads[0]
	assert.Equal("PRRT_1", thread.ProviderThreadID)
	assert.Equal("201", thread.ProviderReviewID)
	assert.Equal("101", thread.ProviderCommentID)
	assert.Equal("inline note", thread.Body)
	assert.Equal("reviewer", thread.AuthorLogin)
	assert.Equal("https://github.com/acme/widget/pull/7#discussion_r101", thread.DirectURL)
	assert.True(thread.Resolved)
	assert.Equal(createdAt, thread.CreatedAt)
	assert.Equal(updatedAt, thread.UpdatedAt)
	assert.Equal("src/main.go", thread.Range.Path)
	assert.Equal("right", thread.Range.Side)
	assert.Equal("right", thread.Range.StartSide)
	require.NotNil(thread.Range.StartLine)
	assert.Equal(startLine, *thread.Range.StartLine)
	assert.Equal(12, thread.Range.Line)
	require.NotNil(thread.Range.NewLine)
	assert.Equal(12, *thread.Range.NewLine)
	assert.Nil(thread.Range.OldLine)
	assert.Equal("add", thread.Range.LineType)
	assert.Equal("head-sha", thread.Range.DiffHeadSHA)
	assert.Equal("head-sha", thread.Range.CommitSHA)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`, thread.MetadataJSON)
	assert.Equal("PRRT_1", threads[1].ProviderThreadID)
	assert.Equal("102", threads[1].ProviderCommentID)
	assert.Equal("reply note", threads[1].Body)
	assert.Equal("maintainer", threads[1].AuthorLogin)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, threads[1].MetadataJSON)
}

func TestGitHubProviderListMergeRequestReviewThreadsMapsFileSubject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 5, 27, 16, 1, 31, 0, time.UTC)
	mock := &mockClient{
		reviewThreads: []PullRequestReviewThread{{
			NodeID:       "PRRT_file",
			Path:         ".golangci.yml",
			Side:         "RIGHT",
			Line:         1,
			OriginalLine: 1,
			Comments: []PullRequestReviewThreadComment{{
				NodeID:      "PRRC_file",
				DatabaseID:  101,
				SubjectType: "FILE",
				Body:        "file note",
				AuthorLogin: "reviewer",
				CommitID:    "head-sha",
				CreatedAt:   createdAt,
				UpdatedAt:   createdAt,
			}},
		}},
	}
	provider := gitHubClientProvider{client: mock, host: "github.com"}

	threads, err := provider.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7)

	require.NoError(err)
	require.Len(threads, 1)
	thread := threads[0]
	assert.Equal(".golangci.yml", thread.Range.Path)
	assert.Equal(1, thread.Range.Line)
	assert.Equal("file", thread.Range.LineType)
	assert.Nil(thread.Range.NewLine)
	assert.Nil(thread.Range.OldLine)
}

func TestGitHubProviderPublishDiffReviewDraftHandlesMissingSubmittedAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockClient{
		createReviewWithCommentsFn: func(
			_ context.Context,
			_ string, _ string,
			_ int,
			_ string, _ string, _ string,
			_ []*gh.DraftReviewComment,
		) (*gh.PullRequestReview, error) {
			id := int64(99)
			return &gh.PullRequestReview{ID: &id}, nil
		},
	}
	provider := gitHubClientProvider{client: mock, host: "github.com"}

	result, err := provider.PublishDiffReviewDraft(t.Context(), platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action:  platform.ReviewActionComment,
		HeadSHA: "validated-head-sha",
	})

	require.NoError(err)
	require.NotNil(result)
	assert.Equal("99", result.ProviderReviewID)
	assert.True(result.SubmittedAt.IsZero())
}

func (m *mockClient) MarkPullRequestReadyForReview(
	_ context.Context, _, _ string, number int,
) (*gh.PullRequest, error) {
	m.trackCall()
	draft := false
	return &gh.PullRequest{Number: &number, Draft: &draft}, nil
}

func (m *mockClient) ConvertPullRequestToDraft(
	_ context.Context, _, _ string, number int,
) (*gh.PullRequest, error) {
	m.trackCall()
	draft := true
	state := "open"
	return &gh.PullRequest{Number: &number, State: &state, Draft: &draft}, nil
}

func (m *mockClient) DismissReview(
	_ context.Context, _, _ string, _ int, reviewID int64, message string,
) (*gh.PullRequestReview, error) {
	m.trackCall()
	m.dismissReviewCalls.Add(1)
	m.dismissedReviewID = reviewID
	m.dismissedReviewMessage = message
	if m.dismissReviewErr != nil {
		return nil, m.dismissReviewErr
	}
	return &gh.PullRequestReview{ID: &reviewID}, nil
}

func (m *mockClient) MergePullRequest(
	_ context.Context, _, _ string, _ int, _, _, _, _ string,
) (*gh.PullRequestMergeResult, error) {
	m.trackCall()
	merged := true
	sha := "abc123"
	msg := "merged"
	return &gh.PullRequestMergeResult{
		Merged: &merged, SHA: &sha, Message: &msg,
	}, nil
}

func (m *mockClient) EditPullRequest(
	_ context.Context, _, _ string, _ int, opts EditPullRequestOpts,
) (*gh.PullRequest, error) {
	m.trackCall()
	pr := &gh.PullRequest{}
	if opts.State != nil {
		pr.State = opts.State
	}
	return pr, nil
}

func (m *mockClient) EditIssue(
	_ context.Context, _, _ string, _ int, state string,
) (*gh.Issue, error) {
	m.trackCall()
	return &gh.Issue{State: &state}, nil
}

func (m *mockClient) EditIssueContent(
	_ context.Context, _, _ string, _ int, title *string, body *string,
) (*gh.Issue, error) {
	m.trackCall()
	out := &gh.Issue{}
	if title != nil {
		out.Title = title
	}
	if body != nil {
		out.Body = body
	}
	return out, nil
}

func (m *mockClient) ListPullRequestsPage(
	ctx context.Context, owner, repo, state string, page int,
) ([]*gh.PullRequest, bool, error) {
	m.trackCall()
	if m.listPullRequestsPageFn != nil {
		return m.listPullRequestsPageFn(ctx, owner, repo, state, page)
	}
	return nil, false, nil
}

func (m *mockClient) ListIssuesPage(
	ctx context.Context, owner, repo, state string, page int,
) ([]*gh.Issue, bool, error) {
	m.trackCall()
	if m.listIssuesPageFn != nil {
		return m.listIssuesPageFn(ctx, owner, repo, state, page)
	}
	return nil, false, nil
}

func (m *mockClient) ListNotifications(ctx context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
	m.trackCall()
	if m.listNotificationsFn != nil {
		return m.listNotificationsFn(ctx, opts)
	}
	return nil, false, nil
}

func (m *mockClient) GetNotificationThread(ctx context.Context, threadID string) (NotificationThread, error) {
	m.trackCall()
	if m.getNotificationThreadFn != nil {
		return m.getNotificationThreadFn(ctx, threadID)
	}
	return NotificationThread{}, nil
}

func (m *mockClient) MarkNotificationThreadRead(ctx context.Context, threadID string) error {
	m.trackCall()
	if m.markNotificationThreadReadFn != nil {
		return m.markNotificationThreadReadFn(ctx, threadID)
	}
	return nil
}

// makeTimestamp is a helper for constructing go-github Timestamp values.
func makeTimestamp(t time.Time) *gh.Timestamp {
	return &gh.Timestamp{Time: t}
}

// buildOpenPR constructs a minimal open *gh.PullRequest for tests.
func buildOpenPR(number int, updatedAt time.Time) *gh.PullRequest {
	sha := "abc123def456"
	state := "open"
	title := "test PR"
	url := "https://github.com/owner/repo/pull/1"
	id := int64(number) * 1000
	headRef := "feature-branch"
	baseRef := "main"
	return &gh.PullRequest{
		ID:        &id,
		Number:    &number,
		Title:     &title,
		HTMLURL:   &url,
		State:     &state,
		UpdatedAt: makeTimestamp(updatedAt),
		CreatedAt: makeTimestamp(updatedAt),
		Head: &gh.PullRequestBranch{
			Ref: &headRef,
			SHA: &sha,
		},
		Base: &gh.PullRequestBranch{
			Ref: &baseRef,
		},
	}
}

func buildOpenIssue(number int, updatedAt time.Time) *gh.Issue {
	state := "open"
	title := fmt.Sprintf("test issue %d", number)
	url := fmt.Sprintf("https://github.com/owner/repo/issues/%d", number)
	id := int64(number) * 1000
	author := "alice"
	return &gh.Issue{
		ID:        &id,
		Number:    &number,
		Title:     &title,
		HTMLURL:   &url,
		State:     &state,
		User:      &gh.User{Login: &author},
		UpdatedAt: makeTimestamp(updatedAt),
		CreatedAt: makeTimestamp(updatedAt),
	}
}

func buildGitHubLabel(id int64, name, description, color string, isDefault bool) *gh.Label {
	return &gh.Label{
		ID:          &id,
		Name:        &name,
		Description: &description,
		Color:       &color,
		Default:     &isDefault,
	}
}

func TestSyncerStopIsIdempotent(t *testing.T) {
	syncer := NewSyncer(map[string]Client{"github.com": &mockClient{}}, nil, nil, nil, time.Minute, nil, nil)
	syncer.Stop()
	syncer.Stop() // must not panic
}

func TestSyncNotificationsContinuesAfterHostError(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("ghe.example.com", "acme", "widget"))
	require.NoError(err)
	_, err = d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("aaa.example.com", "acme", "broken"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	boom := errors.New("boom")
	okNumber := 7
	syncer := NewSyncer(
		map[string]Client{
			"aaa.example.com": &mockClient{
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					return nil, false, boom
				},
			},
			"ghe.example.com": &mockClient{
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					return []NotificationThread{{
						ID:            "thread-ok",
						RepoOwner:     "acme",
						RepoName:      "widget",
						SubjectType:   "PullRequest",
						SubjectTitle:  "Review requested",
						WebURL:        "https://ghe.example.com/acme/widget/pull/7",
						ItemNumber:    &okNumber,
						ItemType:      "pr",
						Reason:        "mention",
						Unread:        true,
						Participating: true,
						UpdatedAt:     now,
					}}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{
			{Owner: "acme", Name: "broken", PlatformHost: "aaa.example.com"},
			{Owner: "acme", Name: "widget", PlatformHost: "ghe.example.com"},
		},
		time.Minute,
		nil,
		nil,
	)

	syncErr := syncer.SyncNotifications(t.Context())
	require.Error(syncErr)
	items, listErr := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(listErr)
	require.Len(items, 1)
	require.ErrorIs(syncErr, boom)
	check.Equal("ghe.example.com", items[0].PlatformHost)
	check.Equal("thread-ok", items[0].PlatformNotificationID)
}

func TestSyncNotificationsContinuesAfterRepoErrorOnSameHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	for _, name := range []string{"broken", "widget"} {
		_, err := database.UpsertRepo(
			t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", name),
		)
		require.NoError(err)
	}
	boom := errors.New("bad scoped credential")
	number := 7
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	client := &mockClient{
		listNotificationsFn: func(
			_ context.Context, opts NotificationListOptions,
		) ([]NotificationThread, bool, error) {
			if opts.RepoName == "broken" {
				return nil, false, boom
			}
			if opts.Participating {
				return nil, false, nil
			}
			return []NotificationThread{{
				ID: "thread-ok", RepoOwner: "acme", RepoName: "widget",
				SubjectType: "PullRequest", SubjectTitle: "Review requested",
				WebURL:     "https://github.com/acme/widget/pull/7",
				ItemNumber: &number, ItemType: "pr", Reason: "mention",
				Unread: true, UpdatedAt: now,
			}}, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{
			{Owner: "acme", Name: "broken", PlatformHost: "github.com"},
			{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)

	err := syncer.SyncNotifications(t.Context())
	require.ErrorIs(err, boom)
	items, listErr := database.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all"},
	)
	require.NoError(listErr)
	require.Len(items, 1)
	assert.Equal("thread-ok", items[0].PlatformNotificationID)

	brokenWatermark, watermarkErr := database.GetNotificationSyncWatermark(
		t.Context(), "github", "github.com", "acme", "broken",
	)
	require.NoError(watermarkErr)
	assert.Nil(brokenWatermark, "a failing repository must not advance its own watermark")
	widgetWatermark, watermarkErr := database.GetNotificationSyncWatermark(
		t.Context(), "github", "github.com", "acme", "widget",
	)
	require.NoError(watermarkErr)
	require.NotNil(widgetWatermark,
		"a healthy repository must advance its watermark despite a failing sibling on the host")
	assert.False(widgetWatermark.LastSuccessfulSyncAt.IsZero())
}

func TestSyncNotificationsSkipsArchivedRepos(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	for _, name := range []string{"frozen", "widget"} {
		_, err := database.UpsertRepo(
			t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", name),
		)
		require.NoError(err)
	}
	var listedRepos sync.Map
	client := &mockClient{
		listNotificationsFn: func(
			_ context.Context, opts NotificationListOptions,
		) ([]NotificationThread, bool, error) {
			if opts.RepoName != "" {
				listedRepos.Store(opts.RepoName, true)
			}
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{
			{Owner: "acme", Name: "frozen", PlatformHost: "github.com", Archived: true},
			{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	_, listedWidget := listedRepos.Load("widget")
	assert.True(listedWidget, "live repo notifications should sync")
	_, listedFrozen := listedRepos.Load("frozen")
	assert.False(listedFrozen,
		"archived repo must not receive notification polling")
}

func TestAckRepoBucketsIncludesArchivedTrackedRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}}, database, nil,
		[]RepoRef{
			{RepoID: 41, Owner: "acme", Name: "frozen", PlatformHost: "github.com", Archived: true},
			{RepoID: 42, Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)
	queuedRepoID := int64(42)
	queued := []db.Notification{{
		ID: 1, RepoID: &queuedRepoID, RepoOwner: "acme", RepoName: "widget",
	}}

	byBucket, byNotification := syncer.ackRepoBuckets(
		platform.KindGitHub, "github.com", queued,
	)

	bucket, ok := byNotification[1]
	require.True(ok)
	names := make([]string, 0, len(byBucket[bucket]))
	repoIDs := make([]int64, 0, len(byBucket[bucket]))
	for _, repo := range byBucket[bucket] {
		names = append(names, repo.Name)
		repoIDs = append(repoIDs, repo.RepoID)
	}
	assert.Contains(names, "frozen",
		"archived repos must stay in ack deferral buckets: their queued "+
			"acknowledgements share the credential's rate limit")
	assert.ElementsMatch([]int64{41, 42}, repoIDs,
		"credential buckets must retain stable repository IDs across renames")
}

func TestSyncNotificationsSkipsUnroutedRepoAndAdvancesRoutedSibling(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	for _, identity := range []db.RepoIdentity{
		verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
		verifiedGitHubRepoIdentity("github.com", "unrouted", "thing"),
	} {
		_, err := database.UpsertRepo(t.Context(), identity)
		require.NoError(err)
	}
	number := 7
	client := &mockClient{
		listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
			if opts.Participating || opts.RepoName != "widget" {
				return nil, false, nil
			}
			return []NotificationThread{{
				ID: "thread-ok", RepoOwner: "acme", RepoName: "widget",
				SubjectType: "PullRequest", SubjectTitle: "Review requested",
				WebURL:     "https://github.com/acme/widget/pull/7",
				ItemNumber: &number, ItemType: "pr", Reason: "mention",
				Unread: true, UpdatedAt: time.Now().UTC(),
			}}, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{
			{Platform: platform.KindGitHub, Owner: "acme", Name: "widget", PlatformHost: "github.com"},
			{Platform: platform.KindGitHub, Owner: "unrouted", Name: "thing", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com", &Route{
		Key:           RouteKey{Host: "github.com", Owner: "acme"},
		Client:        client,
		ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:9"},
		WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:9"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	err = syncer.SyncNotifications(t.Context())
	require.Error(err, "the unrouted repository must surface its failure")
	widgetWatermark, wmErr := database.GetNotificationSyncWatermark(
		t.Context(), "github", "github.com", "acme", "widget",
	)
	require.NoError(wmErr)
	require.NotNil(widgetWatermark,
		"a routed sibling must sync and advance despite an unroutable repository on the host")
	unroutedWatermark, wmErr := database.GetNotificationSyncWatermark(
		t.Context(), "github", "github.com", "unrouted", "thing",
	)
	require.NoError(wmErr)
	assert.Nil(unroutedWatermark)
	items, listErr := database.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all"},
	)
	require.NoError(listErr)
	require.Len(items, 1)
	assert.Equal("thread-ok", items[0].PlatformNotificationID)
}

func TestSyncNotificationsIgnoresReadRateReserveWhenNotificationClientBypassesReserve(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	number := 7
	now := time.Now().UTC()
	rt := NewRateTracker(d, "github.com", "host", "rest")
	rt.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: RateReserveBuffer,
		Reset:     time.Now().UTC().Add(time.Hour),
	})
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				bypassNotificationReadReserve: true,
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return []NotificationThread{{
						ID:           "thread-7",
						RepoOwner:    "acme",
						RepoName:     "widget",
						SubjectType:  "PullRequest",
						SubjectTitle: "Review requested",
						WebURL:       "https://github.com/acme/widget/pull/7",
						ItemNumber:   &number,
						ItemType:     "pr",
						Reason:       "mention",
						Unread:       true,
						UpdatedAt:    now,
					}}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*RateTracker{"github.com": rt},
		map[string]*SyncBudget{"github.com": NewSyncBudget(10)},
	)

	syncErr := syncer.SyncNotifications(t.Context())

	require.NoError(syncErr)
	assert.Equal(int32(2), calls.Load())
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("thread-7", items[0].PlatformNotificationID)
}

func TestSyncNotificationsStopsBeforeListingWhenSharedReadRateReserveExhausted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	rt := NewRateTracker(d, "github.com", "host", "rest")
	rt.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: RateReserveBuffer,
		Reset:     time.Now().UTC().Add(time.Hour),
	})
	var identityCalls atomic.Int32
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
					identityCalls.Add(1)
					return &gh.Repository{
						ID: new(int64(1)), NodeID: new("repo-acme-widget"), Name: &name,
						Owner: &gh.User{Login: &owner},
					}, nil
				},
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*RateTracker{"github.com": rt},
		map[string]*SyncBudget{"github.com": NewSyncBudget(10)},
	)

	syncErr := syncer.SyncNotifications(t.Context())

	require.Error(syncErr)
	require.ErrorContains(syncErr, "rate reserve exhausted")
	assert.Equal(int32(0), identityCalls.Load())
	assert.Equal(int32(0), calls.Load())
}

func TestSyncNotificationsStopsBeforeListingWhenSyncBudgetExhausted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		map[string]*SyncBudget{"github.com": NewSyncBudget(0)},
	)

	syncErr := syncer.SyncNotifications(t.Context())

	require.Error(syncErr)
	require.ErrorContains(syncErr, "sync budget exhausted")
	assert.Equal(int32(0), calls.Load())
}

func TestSyncNotificationsDeduplicatesSharedIdentityLookupBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	budget := NewSyncBudget(1)
	var identityCalls atomic.Int32
	var notificationCalls atomic.Int32
	client := &mockClient{
		budget: budget,
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			identityCalls.Add(1)
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-acme-widget"), Name: &name,
				Owner: &gh.User{Login: &owner},
			}, nil
		},
		listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
			notificationCalls.Add(1)
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute, nil, map[string]*SyncBudget{"github.com": budget},
	)

	syncErr := syncer.SyncNotifications(t.Context())

	require.Error(syncErr)
	require.ErrorContains(syncErr, "sync budget exhausted")
	assert.Equal(int32(1), identityCalls.Load())
	assert.Equal(int32(0), notificationCalls.Load())
	assert.Equal(1, budget.Spent())
}

func TestSyncNotificationsReadsAllRepositoryNotificationPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	var participatingCalls atomic.Int32
	var listCalls atomic.Int32
	var seen []NotificationListOptions
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	prNumber := 7
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					seen = append(seen, opts)
					if opts.Participating {
						participatingCalls.Add(1)
						return nil, false, nil
					}
					listCalls.Add(1)
					if opts.Page < 6 {
						return nil, true, nil
					}
					return []NotificationThread{
						{
							ID:           "thread-page-6",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Review requested",
							WebURL:       "https://github.com/acme/widget/pull/7",
							ItemNumber:   &prNumber,
							ItemType:     "pr",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
					}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		map[string]*SyncBudget{"github.com": NewSyncBudget(100)},
	)

	syncErr := syncer.SyncNotifications(t.Context())

	require.NoError(syncErr)
	assert.Equal(int32(1), participatingCalls.Load())
	assert.Equal(int32(6), listCalls.Load())
	if assert.Len(seen, 7) {
		assert.Equal("acme", seen[0].RepoOwner)
		assert.Equal("widget", seen[0].RepoName)
		assert.True(seen[0].Participating)
		last := seen[len(seen)-1]
		assert.Equal(6, last.Page)
		assert.Equal("acme", last.RepoOwner)
		assert.Equal("widget", last.RepoName)
		assert.False(last.Participating)
	}
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	if assert.Len(items, 1) {
		assert.Equal("thread-page-6", items[0].PlatformNotificationID)
	}
}

func TestSyncNotificationsReadsAllParticipatingNotificationPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	var participatingCalls atomic.Int32
	var listCalls atomic.Int32
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	prNumber := 7
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					if opts.Participating {
						participatingCalls.Add(1)
						if opts.Page < 6 {
							return nil, true, nil
						}
						return []NotificationThread{{ID: "thread-pr"}}, false, nil
					}
					listCalls.Add(1)
					return []NotificationThread{
						{
							ID:           "thread-pr",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Review requested",
							WebURL:       "https://github.com/acme/widget/pull/7",
							ItemNumber:   &prNumber,
							ItemType:     "pr",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
					}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		map[string]*SyncBudget{"github.com": NewSyncBudget(100)},
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	assert.Equal(int32(6), participatingCalls.Load())
	assert.Equal(int32(1), listCalls.Load())
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	if assert.Len(items, 1) {
		assert.True(items[0].Participating)
	}
}

func TestSyncMRMarksLinkedNotificationDone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	seededRepo, err := d.GetRepoByID(t.Context(), repoID)
	require.NoError(err)
	require.NotNil(seededRepo)
	require.Equal("repo-acme-widget", seededRepo.PlatformRepoID)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	number := 7
	openNumber := 9
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       7000,
		Number:           number,
		URL:              "https://github.com/acme/widget/pull/7",
		Title:            "Close me",
		Author:           "alice",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "head",
		PlatformBaseSHA:  "base",
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
	})
	require.NoError(err)
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       900,
		Number:           openNumber,
		URL:              "https://github.com/acme/widget/pull/9",
		Title:            "Stay open",
		Author:           "carol",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "head-open",
		PlatformBaseSHA:  "base",
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
	})
	require.NoError(err)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "thread-pr-closed",
			RepoID:                 &repoID,
			RepoOwner:              "acme",
			RepoName:               "widget",
			SubjectType:            "PullRequest",
			SubjectTitle:           "Close me",
			WebURL:                 "https://github.com/acme/widget/pull/7",
			ItemNumber:             &number,
			ItemType:               "pr",
			Reason:                 "mention",
			Unread:                 true,
			SourceUpdatedAt:        now,
			SyncedAt:               now,
		},
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "thread-pr-open",
			RepoID:                 &repoID,
			RepoOwner:              "acme",
			RepoName:               "widget",
			SubjectType:            "PullRequest",
			SubjectTitle:           "Stay open",
			WebURL:                 "https://github.com/acme/widget/pull/9",
			ItemNumber:             &openNumber,
			ItemType:               "pr",
			Reason:                 "mention",
			Unread:                 true,
			SourceUpdatedAt:        now,
			SyncedAt:               now,
		},
	}))
	closedAt := now.Add(time.Hour)
	closedPR := buildOpenPR(number, closedAt)
	closedPR.State = new("closed")
	closedPR.ClosedAt = makeTimestamp(closedAt)
	closedPR.User = &gh.User{Login: new("alice")}
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
			return closedPR, nil
		}}},
		d,
		nil,
		[]RepoRef{{
			Owner: "acme", Name: "widget", PlatformHost: "github.com",
			PlatformExternalID: "repo-acme-widget",
		}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncMR(t.Context(), "acme", "widget", number))
	active, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "active"})
	require.NoError(err)
	require.Len(active, 1)
	assert.Equal("thread-pr-open", active[0].PlatformNotificationID)
	done, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "done"})
	require.NoError(err)
	require.Len(done, 1)
	assert.Equal("thread-pr-closed", done[0].PlatformNotificationID)
	assert.Equal("closed", done[0].DoneReason)
}

func TestSyncIssueMarksLinkedNotificationDone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	number := 8
	openNumber := 10
	_, err = d.UpsertIssue(t.Context(), &db.Issue{
		RepoID:         repoID,
		PlatformID:     800,
		Number:         number,
		URL:            "https://github.com/acme/widget/issues/8",
		Title:          "Close issue",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	_, err = d.UpsertIssue(t.Context(), &db.Issue{
		RepoID:         repoID,
		PlatformID:     1000,
		Number:         openNumber,
		URL:            "https://github.com/acme/widget/issues/10",
		Title:          "Stay open issue",
		Author:         "dana",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "thread-issue-closed",
			RepoID:                 &repoID,
			RepoOwner:              "acme",
			RepoName:               "widget",
			SubjectType:            "Issue",
			SubjectTitle:           "Close issue",
			WebURL:                 "https://github.com/acme/widget/issues/8",
			ItemNumber:             &number,
			ItemType:               "issue",
			Reason:                 "mention",
			Unread:                 true,
			SourceUpdatedAt:        now,
			SyncedAt:               now,
		},
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "thread-issue-open",
			RepoID:                 &repoID,
			RepoOwner:              "acme",
			RepoName:               "widget",
			SubjectType:            "Issue",
			SubjectTitle:           "Stay open issue",
			WebURL:                 "https://github.com/acme/widget/issues/10",
			ItemNumber:             &openNumber,
			ItemType:               "issue",
			Reason:                 "mention",
			Unread:                 true,
			SourceUpdatedAt:        now,
			SyncedAt:               now,
		},
	}))
	closedAt := now.Add(time.Hour)
	closedIssue := &gh.Issue{
		ID:        gh.Ptr[int64](800),
		Number:    new(number),
		HTMLURL:   new("https://github.com/acme/widget/issues/8"),
		Title:     new("Close issue"),
		User:      &gh.User{Login: new("bob")},
		State:     new("closed"),
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(closedAt),
		ClosedAt:  makeTimestamp(closedAt),
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			return closedIssue, nil
		}}},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncIssue(t.Context(), "acme", "widget", number))
	active, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "active"})
	require.NoError(err)
	require.Len(active, 1)
	assert.Equal("thread-issue-open", active[0].PlatformNotificationID)
	done, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "done"})
	require.NoError(err)
	require.Len(done, 1)
	assert.Equal("thread-issue-closed", done[0].PlatformNotificationID)
	assert.Equal("closed", done[0].DoneReason)
}

func TestSyncNotificationsEnrichesItemAuthorFromLinkedItems(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	prNumber := 7
	issueNumber := 8
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       700,
		Number:           prNumber,
		URL:              "https://github.com/acme/widget/pull/7",
		Title:            "Please review",
		Author:           "alice",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "head",
		PlatformBaseSHA:  "base",
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
	})
	require.NoError(err)
	_, err = d.UpsertIssue(t.Context(), &db.Issue{
		RepoID:         repoID,
		PlatformID:     800,
		Number:         issueNumber,
		URL:            "https://github.com/acme/widget/issues/8",
		Title:          "Issue author",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					if opts.Participating {
						return nil, false, nil
					}
					return []NotificationThread{
						{
							ID:           "thread-pr",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Please review",
							WebURL:       "https://github.com/acme/widget/pull/7",
							ItemNumber:   &prNumber,
							ItemType:     "pr",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
						{
							ID:           "thread-issue",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "Issue",
							SubjectTitle: "Issue author",
							WebURL:       "https://github.com/acme/widget/issues/8",
							ItemNumber:   &issueNumber,
							ItemType:     "issue",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
					}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	prItems, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Search: "alice"})
	require.NoError(err)
	require.Len(prItems, 1)
	assert.Equal("thread-pr", prItems[0].PlatformNotificationID)
	assert.Equal("alice", prItems[0].ItemAuthor)

	issueItems, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Search: "bob"})
	require.NoError(err)
	require.Len(issueItems, 1)
	assert.Equal("thread-issue", issueItems[0].PlatformNotificationID)
	assert.Equal("bob", issueItems[0].ItemAuthor)
}

func TestSyncNotificationsEnrichesItemAuthorFromProviderScopedRepo(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	forgejoRepoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "forgejo",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "forgejo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)
	githubRepoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "github-acme-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	number := 7
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           forgejoRepoID,
		PlatformID:       700,
		Number:           number,
		URL:              "https://code.example.com/acme/widget/pulls/7",
		Title:            "Wrong provider",
		Author:           "wrong-author",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "forgejo-head",
		PlatformBaseSHA:  "forgejo-base",
		HeadRepoCloneURL: "https://code.example.com/acme/widget.git",
	})
	require.NoError(err)
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           githubRepoID,
		PlatformID:       701,
		Number:           number,
		URL:              "https://code.example.com/acme/widget/pull/7",
		Title:            "Right provider",
		Author:           "right-author",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "github-head",
		PlatformBaseSHA:  "github-base",
		HeadRepoCloneURL: "https://code.example.com/acme/widget.git",
	})
	require.NoError(err)
	syncer := NewSyncer(
		map[string]Client{
			"code.example.com": &mockClient{
				getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
					return &gh.Repository{
						ID: new(int64(1)), NodeID: new("github-acme-widget"), Name: &name,
						Owner: &gh.User{Login: &owner},
					}, nil
				},
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					if opts.Participating {
						return nil, false, nil
					}
					return []NotificationThread{{
						ID:           "thread-pr",
						RepoOwner:    "acme",
						RepoName:     "widget",
						SubjectType:  "PullRequest",
						SubjectTitle: "Review requested",
						WebURL:       "https://code.example.com/acme/widget/pull/7",
						ItemNumber:   &number,
						ItemType:     "pr",
						Reason:       "mention",
						Unread:       true,
						UpdatedAt:    now,
					}}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Platform: platform.KindGitHub, Owner: "acme", Name: "widget", PlatformHost: "code.example.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))

	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("github", items[0].Platform)
	require.NotNil(items[0].RepoID)
	assert.Equal(githubRepoID, *items[0].RepoID)
	assert.Equal("right-author", items[0].ItemAuthor)
}

func TestSyncNotificationsMarksParticipatingThreads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	prNumber := 7
	issueNumber := 8
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					if opts.Participating {
						return []NotificationThread{{ID: "thread-1"}}, false, nil
					}
					return []NotificationThread{
						{
							ID:           "thread-1",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Review requested",
							WebURL:       "https://github.com/acme/widget/pull/7",
							ItemNumber:   &prNumber,
							ItemType:     "pr",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
						{
							ID:           "thread-2",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "Issue",
							SubjectTitle: "FYI",
							WebURL:       "https://github.com/acme/widget/issues/8",
							ItemNumber:   &issueNumber,
							ItemType:     "issue",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
					}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 2)
	participatingByThread := map[string]bool{}
	for _, item := range items {
		participatingByThread[item.PlatformNotificationID] = item.Participating
	}
	assert.True(participatingByThread["thread-1"])
	assert.False(participatingByThread["thread-2"])
}

func TestSyncNotificationsSkipsNonPRIssueSubjects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	prNumber := 7
	authorNumber := 9
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					if opts.Participating {
						return nil, false, nil
					}
					return []NotificationThread{
						{
							// CheckSuite/CI notifications carry no subject URL or
							// number from GitHub, so they cannot anchor to a PR or
							// issue and must be skipped entirely.
							ID:           "thread-ci",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "CheckSuite",
							SubjectTitle: "CI workflow run failed for some-branch branch",
							ItemType:     "other",
							Reason:       "ci_activity",
							Unread:       true,
							UpdatedAt:    now,
						},
						{
							// "author" notifications duplicate feed activity on
							// the user's own thread and must be skipped even when
							// anchored to a PR.
							ID:           "thread-author",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Your own PR",
							WebURL:       "https://github.com/acme/widget/pull/9",
							ItemNumber:   &authorNumber,
							ItemType:     "pr",
							Reason:       "author",
							Unread:       true,
							UpdatedAt:    now,
						},
						{
							ID:           "thread-pr",
							RepoOwner:    "acme",
							RepoName:     "widget",
							SubjectType:  "PullRequest",
							SubjectTitle: "Review requested",
							WebURL:       "https://github.com/acme/widget/pull/7",
							ItemNumber:   &prNumber,
							ItemType:     "pr",
							Reason:       "mention",
							Unread:       true,
							UpdatedAt:    now,
						},
					}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("thread-pr", items[0].PlatformNotificationID)
}

func TestProcessQueuedNotificationReadsStopsRetryMetadataAtMaxAttempts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	number := 7
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	queuedAt := now.Add(time.Minute)
	notification := db.Notification{
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
		Unread:                   false,
		Participating:            true,
		SourceUpdatedAt:          now,
		SyncedAt:                 now,
		SourceAckQueuedAt:        &queuedAt,
		SourceAckAttempts:        defaultNotificationPropagationMaxAttempts - 1,
		SourceAckError:           "temporary failure",
		SourceLastAcknowledgedAt: &queuedAt,
	}
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{notification}))
	var markedThreads []string
	mc := &mockClient{markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
		markedThreads = append(markedThreads, threadID)
		return errors.New("still failing")
	}}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	require.NoError(syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10))

	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal([]string{"thread-1"}, markedThreads)
	assert.Equal("max_attempts_exceeded", items[0].SourceAckError)
	assert.Nil(items[0].SourceAckNextAttemptAt)
	queued, err := d.ListQueuedNotificationAcks(t.Context(), "github", "github.com", 10, now.Add(time.Hour))
	require.NoError(err)
	assert.Empty(queued)
}

func TestProcessQueuedNotificationReadsPausesOnRateLimitWithoutConsumingAttempts(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	numberOne := 7
	numberTwo := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
			WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &numberOne, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-2", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review again",
			WebURL: "https://github.com/acme/widget/pull/8", ItemNumber: &numberTwo, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID, items[1].ID}, queuedAt)
	require.NoError(err)
	renamedIdentity := verifiedGitHubRepoIdentity("github.com", "acme", "renamed")
	renamedIdentity.PlatformRepoID = "repo-acme-widget"
	_, accepted, err := d.ReconcileRepositoryObservation(
		t.Context(), renamedIdentity, time.Now().UTC().Add(time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	resetAt := time.Now().UTC().Add(time.Hour).Round(0)
	var markedThreads []string
	mc := &mockClient{markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
		markedThreads = append(markedThreads, threadID)
		return &gh.RateLimitError{
			Rate: gh.Rate{Reset: gh.Timestamp{Time: resetAt}},
			Response: &http.Response{
				StatusCode: http.StatusForbidden,
				Request:    httptest.NewRequest(http.MethodPatch, "https://api.github.com/notifications/threads/"+threadID, nil),
			},
			Message: "API rate limit exceeded",
		}
	}}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	err = syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10)
	require.Error(err)

	items, err = d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	attemptsByThread := map[string]int{}
	errorsByThread := map[string]string{}
	nextAttemptByThread := map[string]*time.Time{}
	queuedByThread := map[string]*time.Time{}
	for _, item := range items {
		attemptsByThread[item.PlatformNotificationID] = item.SourceAckAttempts
		errorsByThread[item.PlatformNotificationID] = item.SourceAckError
		nextAttemptByThread[item.PlatformNotificationID] = item.SourceAckNextAttemptAt
		queuedByThread[item.PlatformNotificationID] = item.SourceAckQueuedAt
	}
	check.Equal([]string{"thread-1"}, markedThreads)
	check.Equal(map[string]int{"thread-1": 0, "thread-2": 0}, attemptsByThread)
	check.Equal(map[string]string{"thread-1": "rate_limited", "thread-2": "rate_limited"}, errorsByThread)
	if check.NotNil(nextAttemptByThread["thread-1"]) {
		check.Equal(resetAt, *nextAttemptByThread["thread-1"])
	}
	if check.NotNil(nextAttemptByThread["thread-2"]) {
		check.Equal(resetAt, *nextAttemptByThread["thread-2"])
	}
	check.NotNil(queuedByThread["thread-1"])
	check.NotNil(queuedByThread["thread-2"])
}

func TestProcessQueuedNotificationReadsDefersWhenRefetchRateLimited(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	numberOne := 7
	numberTwo := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
			WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &numberOne, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-2", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review again",
			WebURL: "https://github.com/acme/widget/pull/8", ItemNumber: &numberTwo, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID, items[1].ID}, queuedAt)
	require.NoError(err)
	resetAt := time.Now().UTC().Add(time.Hour).Round(0)
	var refetchedThreads []string
	var markedThreads []string
	mc := &mockClient{
		getNotificationThreadFn: func(_ context.Context, threadID string) (NotificationThread, error) {
			refetchedThreads = append(refetchedThreads, threadID)
			return NotificationThread{}, &gh.RateLimitError{
				Rate: gh.Rate{Reset: gh.Timestamp{Time: resetAt}},
				Response: &http.Response{
					StatusCode: http.StatusForbidden,
					Request:    httptest.NewRequest(http.MethodGet, "https://api.github.com/notifications/threads/"+threadID, nil),
				},
				Message: "API rate limit exceeded",
			}
		},
		markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
			markedThreads = append(markedThreads, threadID)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	err = syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10)
	require.Error(err)

	// The refetch budget is shared with mark-read, so a rate-limited refetch
	// must not reach the mark-read call and must defer every queued ack.
	check.Equal([]string{"thread-1"}, refetchedThreads)
	check.Empty(markedThreads)

	items, err = d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	attemptsByThread := map[string]int{}
	errorsByThread := map[string]string{}
	nextAttemptByThread := map[string]*time.Time{}
	queuedByThread := map[string]*time.Time{}
	for _, item := range items {
		attemptsByThread[item.PlatformNotificationID] = item.SourceAckAttempts
		errorsByThread[item.PlatformNotificationID] = item.SourceAckError
		nextAttemptByThread[item.PlatformNotificationID] = item.SourceAckNextAttemptAt
		queuedByThread[item.PlatformNotificationID] = item.SourceAckQueuedAt
	}
	check.Equal(map[string]int{"thread-1": 0, "thread-2": 0}, attemptsByThread)
	check.Equal(map[string]string{"thread-1": "rate_limited", "thread-2": "rate_limited"}, errorsByThread)
	if check.NotNil(nextAttemptByThread["thread-1"]) {
		check.Equal(resetAt, *nextAttemptByThread["thread-1"])
	}
	if check.NotNil(nextAttemptByThread["thread-2"]) {
		check.Equal(resetAt, *nextAttemptByThread["thread-2"])
	}
	check.NotNil(queuedByThread["thread-1"])
	check.NotNil(queuedByThread["thread-2"])
}

func TestRunNotificationSyncFiresCompletionHook(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	syncer := NewSyncer(map[string]Client{"github.com": &mockClient{}}, d, nil, nil, time.Minute, nil, nil)
	var calls int
	syncer.SetOnNotificationSyncComplete(func() { calls++ })

	require.NoError(syncer.RunNotificationSync(t.Context()))
	check.Equal(1, calls)

	// A second run fires the hook again so an already-open feed keeps
	// reloading after later syncs.
	require.NoError(syncer.RunNotificationSync(t.Context()))
	check.Equal(2, calls)
}

func TestProcessQueuedNotificationReadsPreservesUpstreamReadOnPreAckRefetch(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	number := 7
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
		RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
	}}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	require.Len(items, 1)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID}, queuedAt)
	require.NoError(err)

	// The thread advanced upstream, but GitHub reports it already read (the
	// user read the newer activity elsewhere). The pre-ack refetch must keep
	// it read locally rather than resurrecting it as unread.
	newer := now.Add(2 * time.Minute)
	var markedThreads []string
	mc := &mockClient{
		getNotificationThreadFn: func(_ context.Context, threadID string) (NotificationThread, error) {
			return NotificationThread{
				ID: threadID, RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest",
				SubjectTitle: "Please review", WebURL: "https://github.com/acme/widget/pull/7",
				ItemNumber: &number, ItemType: "pr", Reason: "mention",
				Unread: false, UpdatedAt: newer,
			}, nil
		},
		markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
			markedThreads = append(markedThreads, threadID)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	require.NoError(syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10))

	// No mark-read: the advanced thread is already read upstream.
	check.Empty(markedThreads)
	all, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(all, 1)
	check.False(all[0].Unread)
	check.Equal(newer, all[0].SourceUpdatedAt)
	check.Nil(all[0].SourceAckQueuedAt)

	unread, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	check.Empty(unread)
}

func TestQueuedNotificationRefreshPreservesDoneAckAfterRouteChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repoID, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "original-repo",
		Owner: "acme", Name: "alpha",
	})
	require.NoError(err)
	number := 7
	originalUpdatedAt := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7007, Number: number,
		URL: "https://github.com/acme/alpha/pull/7", Title: "Pull request",
		State: db.MergeRequestStateOpen, CreatedAt: originalUpdatedAt,
		UpdatedAt: originalUpdatedAt, LastActivityAt: originalUpdatedAt,
	})
	require.NoError(err)
	require.NoError(database.UpsertNotifications(ctx, []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1",
		RepoID: &repoID, RepoOwner: "acme", RepoName: "alpha",
		SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/alpha/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: originalUpdatedAt, SyncedAt: originalUpdatedAt,
	}}))
	items, err := database.ListNotifications(ctx, db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	queuedAt := originalUpdatedAt.Add(time.Minute)
	_, err = database.MarkNotificationsDone(ctx, []int64{items[0].ID}, queuedAt, true)
	require.NoError(err)

	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	var marked atomic.Int32
	client := &mockClient{
		getNotificationThreadFn: func(context.Context, string) (NotificationThread, error) {
			if fetches.Add(1) == 1 {
				close(started)
				<-release
			}
			return NotificationThread{
				ID: "thread-1", RepoOwner: "acme", RepoName: "beta",
				SubjectType: "PullRequest", SubjectTitle: "Please review",
				WebURL: "https://github.com/acme/beta/pull/7", ItemNumber: &number, ItemType: "pr",
				Reason: "mention", Unread: false, UpdatedAt: originalUpdatedAt,
			}, nil
		},
		markNotificationThreadReadFn: func(context.Context, string) error {
			marked.Add(1)
			return nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "alpha", PlatformExternalID: "original-repo",
		}},
		time.Minute, nil, testBudget(100),
	)

	done := make(chan error, 1)
	go func() {
		done <- syncer.ProcessQueuedNotificationReads(ctx, platform.KindGitHub, "github.com", 10)
	}()
	<-started
	_, _, err = database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "original-repo",
		Owner: "acme", Name: "beta", RepoPath: "acme/beta",
	}, time.Now().UTC().Add(time.Hour))
	require.NoError(err)
	close(release)
	require.NoError(<-done)

	// The route changed after this pass captured alpha. The stable linked
	// acknowledgement must remain queued so the next pass can resolve repoID
	// to beta instead of discarding the upstream read intent.
	items, err = database.ListNotifications(ctx, db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("acme", items[0].RepoOwner)
	assert.Equal("alpha", items[0].RepoName)
	assert.Equal("Please review", items[0].SubjectTitle)
	assert.Equal(originalUpdatedAt, items[0].SourceUpdatedAt)
	assert.False(items[0].Unread)
	assert.NotNil(items[0].DoneAt)
	assert.NotEmpty(items[0].DoneReason)
	assert.NotNil(items[0].SourceAckQueuedAt)
	assert.Nil(items[0].SourceAckSyncedAt)
	require.NotNil(items[0].RepoID)
	assert.Equal(repoID, *items[0].RepoID)
	assert.Zero(marked.Load())
	queued, err := database.ListQueuedNotificationAcks(
		ctx, "github", "github.com", 10, time.Now().UTC().Add(2*time.Hour),
	)
	require.NoError(err)
	require.Len(queued, 1)
	assert.Equal("acme", queued[0].RepoOwner)
	assert.Equal("beta", queued[0].RepoName)

	require.NoError(syncer.ProcessQueuedNotificationReads(
		ctx, platform.KindGitHub, "github.com", 10,
	))
	assert.Equal(int32(1), marked.Load())
	items, err = database.ListNotifications(ctx, db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	assert.False(items[0].Unread)
	assert.NotNil(items[0].DoneAt)
	assert.Nil(items[0].SourceAckQueuedAt)
	assert.NotNil(items[0].SourceAckSyncedAt)
}

func TestQueuedNotificationRefreshLinksLegacyUnownedRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	number := 7
	originalUpdatedAt := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	require.NoError(database.UpsertNotifications(ctx, []db.Notification{{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "legacy-thread",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL:     "https://github.com/acme/widget/pull/7",
		ItemNumber: &number, ItemType: "pr", Reason: "mention", Unread: true,
		SourceUpdatedAt: originalUpdatedAt, SyncedAt: originalUpdatedAt,
	}}))
	repoID, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	items, err := database.ListNotifications(
		ctx, db.ListNotificationsOpts{State: "all"},
	)
	require.NoError(err)
	require.Len(items, 1)
	require.Nil(items[0].RepoID)
	queuedAt := originalUpdatedAt.Add(time.Minute)
	_, err = database.MarkNotificationsDone(
		ctx, []int64{items[0].ID}, queuedAt, true,
	)
	require.NoError(err)

	newer := originalUpdatedAt.Add(2 * time.Minute)
	client := &mockClient{getNotificationThreadFn: func(
		context.Context, string,
	) (NotificationThread, error) {
		return NotificationThread{
			ID: "legacy-thread", RepoOwner: "acme", RepoName: "widget",
			SubjectType: "PullRequest", SubjectTitle: "New activity",
			WebURL:     "https://github.com/acme/widget/pull/7",
			ItemNumber: &number, ItemType: "pr", Reason: "mention",
			Unread: true, UpdatedAt: newer,
		}, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil, nil,
		time.Minute, nil, nil,
	)
	require.NoError(syncer.ProcessQueuedNotificationReads(
		ctx, platform.KindGitHub, "github.com", 10,
	))

	items, err = database.ListNotifications(
		ctx, db.ListNotificationsOpts{State: "all"},
	)
	require.NoError(err)
	require.Len(items, 1)
	require.NotNil(items[0].RepoID)
	assert.Equal(repoID, *items[0].RepoID)
	assert.Equal(newer, items[0].SourceUpdatedAt)
	assert.Equal("New activity", items[0].SubjectTitle)
	assert.True(items[0].Unread)
	assert.Nil(items[0].DoneAt)
	assert.Nil(items[0].SourceAckQueuedAt)
}

func TestProcessQueuedNotificationReadsBacksOffRowAndContinuesOnRefetchError(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	numberOne := 7
	numberTwo := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
			WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &numberOne, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-2", RepoID: &repoID,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review again",
			WebURL: "https://github.com/acme/widget/pull/8", ItemNumber: &numberTwo, ItemType: "pr",
			Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID, items[1].ID}, queuedAt)
	require.NoError(err)
	var markedThreads []string
	mc := &mockClient{
		getNotificationThreadFn: func(_ context.Context, threadID string) (NotificationThread, error) {
			if threadID == "thread-1" {
				return NotificationThread{}, errors.New("boom")
			}
			// thread-2 has not advanced, so it proceeds to mark-read.
			return NotificationThread{ID: threadID, UpdatedAt: now}, nil
		},
		markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
			markedThreads = append(markedThreads, threadID)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	// A per-row refetch error must not abort the batch.
	require.NoError(syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10))
	check.Equal([]string{"thread-2"}, markedThreads)

	items, err = d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"})
	require.NoError(err)
	require.Len(items, 2)
	byThread := map[string]db.Notification{}
	for _, item := range items {
		byThread[item.PlatformNotificationID] = item
	}
	// thread-1 backed off for a retry without being acked.
	failed := byThread["thread-1"]
	check.Equal(1, failed.SourceAckAttempts)
	check.Contains(failed.SourceAckError, "boom")
	check.NotNil(failed.SourceAckNextAttemptAt)
	check.NotNil(failed.SourceAckQueuedAt)
	check.Nil(failed.SourceAckSyncedAt)
	// thread-2 was acked normally.
	acked := byThread["thread-2"]
	check.NotNil(acked.SourceAckSyncedAt)
	check.Nil(acked.SourceAckQueuedAt)
}

func TestProcessQueuedNotificationReadsReopensRemoteActivityAfterPatchRace(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	number := 7
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
		RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
	}}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	require.Len(items, 1)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID}, queuedAt)
	require.NoError(err)

	newer := now.Add(2 * time.Minute)
	var getCalls int
	var markedThreads []string
	mc := &mockClient{
		getNotificationThreadFn: func(_ context.Context, threadID string) (NotificationThread, error) {
			getCalls++
			thread := NotificationThread{
				ID:           threadID,
				RepoOwner:    "acme",
				RepoName:     "widget",
				SubjectType:  "PullRequest",
				SubjectTitle: "Please review",
				WebURL:       "https://github.com/acme/widget/pull/7",
				ItemNumber:   &number,
				ItemType:     "pr",
				Reason:       "mention",
				Unread:       false,
				UpdatedAt:    now,
				LastReadAt:   &queuedAt,
			}
			if getCalls > 1 {
				thread.Unread = true
				thread.UpdatedAt = newer
				thread.LastReadAt = nil
			}
			return thread, nil
		},
		markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
			markedThreads = append(markedThreads, threadID)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	require.NoError(syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10))

	unread, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	require.Len(unread, 1)
	check.Equal([]string{"thread-1"}, markedThreads)
	check.Equal(2, getCalls)
	check.Equal(newer, unread[0].SourceUpdatedAt)
	check.Nil(unread[0].SourceAckQueuedAt)
	check.Nil(unread[0].SourceAckSyncedAt)
	check.Nil(unread[0].SourceAckGenerationAt)
}

func TestProcessQueuedNotificationReadsReopensAfterPostAckRefetchError(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	number := 7
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
		RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
	}}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	require.Len(items, 1)
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID}, queuedAt)
	require.NoError(err)

	var getCalls int
	var markedThreads []string
	mc := &mockClient{
		getNotificationThreadFn: func(_ context.Context, threadID string) (NotificationThread, error) {
			getCalls++
			if getCalls == 1 {
				return NotificationThread{
					ID:           threadID,
					RepoOwner:    "acme",
					RepoName:     "widget",
					SubjectType:  "PullRequest",
					SubjectTitle: "Please review",
					WebURL:       "https://github.com/acme/widget/pull/7",
					ItemNumber:   &number,
					ItemType:     "pr",
					Reason:       "mention",
					Unread:       false,
					UpdatedAt:    now,
					LastReadAt:   &queuedAt,
				}, nil
			}
			return NotificationThread{}, errors.New("post-ack refetch failed")
		},
		markNotificationThreadReadFn: func(_ context.Context, threadID string) error {
			markedThreads = append(markedThreads, threadID)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)

	require.NoError(syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10))

	unread, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "unread"})
	require.NoError(err)
	require.Len(unread, 1)
	check.Equal([]string{"thread-1"}, markedThreads)
	check.Equal(2, getCalls)
	check.Equal("thread-1", unread[0].PlatformNotificationID)
	check.True(unread[0].Unread)
	check.Nil(unread[0].SourceAckQueuedAt)
	check.Nil(unread[0].SourceAckSyncedAt)
	check.Nil(unread[0].SourceAckGenerationAt)
	check.Empty(unread[0].SourceAckError)
}

func TestSyncNotificationsSkipsHostsWithoutTrackedRepos(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		nil,
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	require.Equal(int32(0), calls.Load())
}

func TestSyncNotificationsCatalogsRepoBeforeFetching(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	number := 7
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	client := &mockClient{
		listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
			if opts.Participating {
				return nil, false, nil
			}
			return []NotificationThread{{
				ID: "thread-1", RepoOwner: "acme", RepoName: "widget",
				SubjectType: "PullRequest", SubjectTitle: "Please review",
				WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number,
				ItemType: "pr", Reason: "mention", Unread: true, UpdatedAt: now,
			}}, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	repo, err := d.GetRepoByIdentity(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NotNil(repo)
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	require.NotNil(items[0].RepoID)
	assert.Equal(repo.ID, *items[0].RepoID)
}

func TestSyncNotificationsReconcilesOccupiedRouteBeforeFetching(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	oldRepoID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_old",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	number := 7
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	client := &mockClient{
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(2)), NodeID: new("R_new"), Name: &name,
				Owner: &gh.User{Login: &owner}, Archived: new(bool),
			}, nil
		},
		listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
			if opts.Participating {
				return nil, false, nil
			}
			return []NotificationThread{{
				ID: "replacement-thread", RepoOwner: "acme", RepoName: "widget",
				SubjectType: "PullRequest", SubjectTitle: "Replacement repository",
				WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number,
				ItemType: "pr", Reason: "mention", Unread: true, UpdatedAt: now,
			}}, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncNotifications(ctx))
	active, err := d.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NotNil(active)
	assert.Equal("R_new", active.PlatformRepoID)
	assert.NotEqual(oldRepoID, active.ID)
	items, err := d.ListNotifications(ctx, db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	require.NotNil(items[0].RepoID)
	assert.Equal(active.ID, *items[0].RepoID)
}

func TestSyncNotificationsReportsReconciledRepoSettingsPersistenceFailure(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	_, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "R_old",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	})
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(ctx, `
		CREATE TRIGGER reject_notification_repo_settings
		BEFORE UPDATE OF allow_squash_merge ON forge_repos
		BEGIN SELECT RAISE(ABORT, 'reject settings'); END`)
	require.NoError(err)

	var listCalls atomic.Int32
	client := &mockClient{
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			return &gh.Repository{
				NodeID: new("R_new"), Name: &name, Owner: &gh.User{Login: &owner},
				DefaultBranch: new("main"), AllowSquashMerge: new(true),
				AllowMergeCommit: new(false), AllowRebaseMerge: new(false),
			}, nil
		},
		listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
			listCalls.Add(1)
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "widget"}},
		time.Minute, nil, nil,
	)

	err = syncer.SyncNotifications(ctx)
	require.ErrorContains(err, "reject settings")
	require.Zero(listCalls.Load(), "notification listing must not run with stale repository settings")
}

func TestGitHubPlatformRepositoryTreatsIncompleteMergeSettingsAsUnknown(t *testing.T) {
	repo := gitHubPlatformRepository("github.com", "acme", &gh.Repository{
		NodeID: new("repo-1"), Name: new("widget"),
		Owner:            &gh.User{Login: new("acme")},
		AllowSquashMerge: new(true),
	})

	require.Nil(t, repo.MergeSettings)
}

func TestGitHubPlatformRepositoryPreservesExplicitAllDisabledMergeSettings(t *testing.T) {
	assert := assert.New(t)
	repo := gitHubPlatformRepository("github.com", "acme", &gh.Repository{
		NodeID: new("repo-1"), Name: new("widget"),
		Owner:            &gh.User{Login: new("acme")},
		AllowSquashMerge: new(false), AllowMergeCommit: new(false),
		AllowRebaseMerge: new(false),
	})

	require.NotNil(t, repo.MergeSettings)
	assert.False(repo.MergeSettings.AllowSquashMerge)
	assert.False(repo.MergeSettings.AllowMergeCommit)
	assert.False(repo.MergeSettings.AllowRebaseMerge)
}

func TestRepoProviderObservationRetainsUnknownSettingsAndRepairsFromCompleteSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-1",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	firstObservedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	entry, accepted, err := database.ReconcileRepositoryObservation(ctx, identity, firstObservedAt)
	require.NoError(err)
	require.True(accepted)
	require.NoError(database.UpdateRepoMergeSettings(ctx, entry.Repository.ID, false, false, false))
	syncer := &Syncer{db: database}

	unknownObservedAt := firstObservedAt.Add(time.Minute)
	_, accepted, err = database.ReconcileRepositoryObservation(ctx, identity, unknownObservedAt)
	require.NoError(err)
	require.True(accepted)
	applied, err := syncer.updateRepoSettingsFromProviderObservation(
		ctx, entry.Repository.ID, unknownObservedAt,
		platform.Repository{PlatformExternalID: "repo-1"},
	)
	require.NoError(err)
	require.True(applied)
	stored, err := database.GetRepoByID(ctx, entry.Repository.ID)
	require.NoError(err)
	assert.False(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)

	repairObservedAt := unknownObservedAt.Add(time.Minute)
	_, accepted, err = database.ReconcileRepositoryObservation(ctx, identity, repairObservedAt)
	require.NoError(err)
	require.True(accepted)
	applied, err = syncer.updateRepoSettingsFromProviderObservation(
		ctx, entry.Repository.ID, repairObservedAt,
		platform.Repository{
			PlatformExternalID: "repo-1",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
		},
	)
	require.NoError(err)
	require.True(applied)
	stored, err = database.GetRepoByID(ctx, entry.Repository.ID)
	require.NoError(err)
	assert.True(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)
}

func TestSyncNotificationsRetriesSettingsAfterABARouteReuse(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	observedAt := time.Now().UTC()
	original, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, observedAt)
	require.NoError(err)
	require.NoError(database.UpdateRepoMergeSettings(
		ctx, original.Repository.ID, false, false, false,
	))

	var repositoryCalls atomic.Int32
	client := &mockClient{
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			allowSquash := repositoryCalls.Add(1) == 1
			return &gh.Repository{
				NodeID: new("repo-a"), Name: &name, Owner: &gh.User{Login: &owner},
				AllowSquashMerge: &allowSquash, AllowMergeCommit: new(false),
				AllowRebaseMerge: new(false),
			}, nil
		},
		listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget", PlatformExternalID: "repo-a",
		}},
		time.Minute, nil, nil,
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	syncer.afterNotificationRepoIdentityReconciled = func() {
		blockOnce.Do(func() {
			close(started)
			<-release
		})
	}
	done := make(chan error, 1)
	go func() { done <- syncer.SyncNotifications(ctx) }()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	require.NoError(database.UpdateRepoMergeSettings(
		ctx, original.Repository.ID, false, false, false,
	))
	close(release)
	require.NoError(<-done)

	stored, err := database.GetRepoByID(ctx, original.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.False(stored.AllowSquashMerge)
	require.Equal(int32(2), repositoryCalls.Load(), "stale settings must trigger one retry")
}

func TestEnsureCloneForRouteRemovesFetchedCloneAfterABARouteReuse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	entry, _, err := database.ReconcileRepositoryObservation(ctx, identity, observedAt)
	require.NoError(err)
	routeFence, found, err := database.CurrentRepositoryRouteFence(
		ctx, identity, entry.Repository.ID,
	)
	require.NoError(err)
	require.True(found)

	clones := gitclone.New(t.TempDir(), nil)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a", CloneURL: setupBareRemoteForSyncTest(t),
	}
	syncer := &Syncer{db: database, clones: clones}
	cloneFetched := make(chan struct{})
	releaseValidation := make(chan struct{})
	syncer.beforeCloneRouteValidation = func() {
		close(cloneFetched)
		<-releaseValidation
	}
	done := make(chan error, 1)
	go func() {
		done <- syncer.ensureCloneForRoute(ctx, repo, entry.Repository.ID, routeFence)
	}()
	<-cloneFetched
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(releaseValidation)
	require.ErrorIs(<-done, db.ErrRepositoryRouteFenceChanged)

	clonePath, err := clones.ClonePathForContext(
		withCloneRepositoryIdentity(ctx, repo),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	assert.NoDirExists(clonePath)
}

func TestSyncMRDiffRejectsCloneFetchedAcrossABARouteReuse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	entry, _, err := database.ReconcileRepositoryObservation(ctx, identity, observedAt)
	require.NoError(err)
	routeFence, found, err := database.CurrentRepositoryRouteFence(
		ctx, identity, entry.Repository.ID,
	)
	require.NoError(err)
	require.True(found)

	clones := gitclone.New(t.TempDir(), nil)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a", CloneURL: setupBareRemoteForSyncTest(t),
	}
	syncer := &Syncer{db: database, clones: clones}
	cloneFetched := make(chan struct{})
	releaseValidation := make(chan struct{})
	syncer.beforeCloneRouteValidation = func() {
		close(cloneFetched)
		<-releaseValidation
	}
	done := make(chan error, 1)
	go func() {
		done <- syncer.syncMRDiff(
			ctx, repo, entry.Repository.ID, 0, 0, 1,
			&gh.PullRequest{}, &db.MergeRequest{}, routeFence,
		)
	}()
	<-cloneFetched
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(releaseValidation)
	err = <-done
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	var diffErr *DiffSyncError
	require.ErrorAs(err, &diffErr)
	assert.Equal(DiffSyncCodeCloneUnavailable, diffErr.Code)

	clonePath, err := clones.ClonePathForContext(
		withCloneRepositoryIdentity(ctx, repo),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	assert.NoDirExists(clonePath)
}

func TestSyncNotificationsRetriesSettingsAfterNewerSameRouteObservation(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, identity, time.Now().UTC(),
	)
	require.NoError(err)
	require.NoError(database.UpdateRepoMergeSettings(
		ctx, original.Repository.ID, false, false, false,
	))

	var repositoryCalls atomic.Int32
	client := &mockClient{
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			allowSquash := repositoryCalls.Add(1) == 1
			return &gh.Repository{
				NodeID: new("repo-a"), Name: &name, Owner: &gh.User{Login: &owner},
				AllowSquashMerge: &allowSquash, AllowMergeCommit: new(false),
				AllowRebaseMerge: new(false),
			}, nil
		},
		listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget", PlatformExternalID: "repo-a",
		}},
		time.Minute, nil, nil,
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	syncer.afterNotificationRepoIdentityReconciled = func() {
		blockOnce.Do(func() {
			close(started)
			<-release
		})
	}
	done := make(chan error, 1)
	go func() { done <- syncer.SyncNotifications(ctx) }()
	<-started
	_, accepted, err := database.ReconcileRepositoryObservation(
		ctx, identity, time.Now().UTC(),
	)
	require.NoError(err)
	require.True(accepted)
	require.NoError(database.UpdateRepoMergeSettings(
		ctx, original.Repository.ID, false, false, false,
	))
	close(release)
	require.NoError(<-done)

	stored, err := database.GetRepoByID(ctx, original.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.False(stored.AllowSquashMerge)
	require.Equal(int32(2), repositoryCalls.Load(), "stale settings must trigger one retry")
}

func TestSyncNotificationsRetriesBeforeListingWhenRepositoryObservationIsStale(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, identity, time.Now().UTC(),
	)
	require.NoError(err)
	require.NoError(database.UpdateRepoMergeSettings(
		ctx, original.Repository.ID, false, false, false,
	))

	var repositoryCalls atomic.Int32
	var listCalls atomic.Int32
	client := &mockClient{
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			call := repositoryCalls.Add(1)
			if call == 1 {
				// This observation lands after syncRepoIdentity captured its
				// timestamp but before that snapshot is reconciled.
				_, accepted, reconcileErr := database.ReconcileRepositoryObservation(
					ctx, identity, time.Now().UTC(),
				)
				require.NoError(reconcileErr)
				require.True(accepted)
			}
			allowSquash := call == 1
			return &gh.Repository{
				NodeID: new("repo-a"), Name: &name, Owner: &gh.User{Login: &owner},
				AllowSquashMerge: &allowSquash, AllowMergeCommit: new(false),
				AllowRebaseMerge: new(false),
			}, nil
		},
		listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
			listCalls.Add(1)
			return nil, false, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget", PlatformExternalID: "repo-a",
		}},
		time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncNotifications(ctx))
	stored, err := database.GetRepoByID(ctx, original.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.False(stored.AllowSquashMerge)
	require.Equal(int32(2), repositoryCalls.Load(), "stale identity must retry")
	require.Equal(int32(2), listCalls.Load(), "only the successful attempt may list")
}

func TestSyncNotificationsUsesPersistedSinceWatermark(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	watermark := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	lastFullSyncAt := time.Now().UTC()
	require.NoError(d.UpdateNotificationSyncWatermark(t.Context(), "github", "github.com", "acme", "widget", watermark, &lastFullSyncAt))
	var seen []NotificationListOptions
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					seen = append(seen, opts)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	require.Len(seen, 2)
	assert.True(seen[0].All)
	assert.True(seen[0].Participating)
	assert.Equal(1, seen[0].Page)
	assert.Equal("acme", seen[0].RepoOwner)
	assert.Equal("widget", seen[0].RepoName)
	if assert.NotNil(seen[0].Since) {
		assert.True(watermark.Add(-notificationSyncSinceOverlap).Equal(*seen[0].Since))
	}
	assert.True(seen[1].All)
	assert.False(seen[1].Participating)
	assert.Equal(1, seen[1].Page)
	assert.Equal("acme", seen[1].RepoOwner)
	assert.Equal("widget", seen[1].RepoName)
	if assert.NotNil(seen[1].Since) {
		assert.True(watermark.Add(-notificationSyncSinceOverlap).Equal(*seen[1].Since))
	}
}

func TestSyncNotificationsFullSyncsAfterConflictingProviderIDAtPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepoByProviderID(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_displaced",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	watermark := time.Now().UTC().Add(-time.Minute)
	lastFullSyncAt := watermark
	require.NoError(d.UpdateNotificationSyncWatermark(
		t.Context(), "github", "github.com", "acme", "widget", watermark, &lastFullSyncAt,
	))
	_, err = d.UpsertRepoByProviderID(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_incoming",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)

	var seen []NotificationListOptions
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					seen = append(seen, opts)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	require.Len(seen, 2)
	assert.Nil(seen[0].Since)
	assert.Nil(seen[1].Since)
}

func TestSyncNotificationsRetriesAfterRepositoryReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	_, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_original",
		Owner:          "acme",
		Name:           "alpha",
	})
	require.NoError(err)
	watermark := time.Now().UTC().Add(-time.Minute)
	require.NoError(d.UpdateNotificationSyncWatermark(
		ctx, "github", "github.com", "acme", "alpha", watermark, &watermark,
	))

	started := make(chan struct{})
	release := make(chan struct{})
	number := 7
	var calls atomic.Int32
	var providerRepoID atomic.Value
	providerRepoID.Store("R_original")
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
					return &gh.Repository{
						ID: new(int64(1)), NodeID: new(providerRepoID.Load().(string)), Name: &name,
						Owner: &gh.User{Login: &owner},
					}, nil
				},
				listNotificationsFn: func(_ context.Context, _ NotificationListOptions) ([]NotificationThread, bool, error) {
					call := calls.Add(1)
					if call == 1 {
						close(started)
						<-release
						return nil, false, nil
					}
					if call > 2 {
						return nil, false, nil
					}
					return []NotificationThread{{
						ID: "stale-thread", RepoOwner: "acme", RepoName: "alpha",
						SubjectType: "PullRequest", SubjectTitle: "stale notification",
						WebURL:     "https://github.com/acme/alpha/pull/7",
						ItemNumber: &number, ItemType: "pr", Reason: "mention",
						Unread: true, UpdatedAt: watermark,
					}}, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "alpha", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	done := make(chan error, 1)
	go func() { done <- syncer.SyncNotifications(ctx) }()
	<-started
	observedAt := time.Now().UTC()
	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_original",
		Owner:          "acme",
		Name:           "beta",
		RepoPath:       "acme/beta",
	}, observedAt)
	require.NoError(err)
	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "R_replacement",
		Owner:          "acme",
		Name:           "alpha",
		RepoPath:       "acme/alpha",
	}, observedAt.Add(time.Nanosecond))
	require.NoError(err)
	providerRepoID.Store("R_replacement")
	close(release)
	require.NoError(<-done)

	restored, err := d.GetNotificationSyncWatermark(
		ctx, "github", "github.com", "acme", "alpha",
	)
	require.NoError(err)
	assert.NotNil(restored)
	assert.Equal(int32(4), calls.Load())
	notifications, err := d.ListNotifications(ctx, db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	assert.Empty(notifications)
}

func TestSyncNotificationsDoesPeriodicFullSyncForReadState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	watermark := time.Now().UTC().Add(-2 * notificationFullSyncInterval)
	lastFullSyncAt := watermark
	require.NoError(d.UpdateNotificationSyncWatermark(t.Context(), "github", "github.com", "acme", "widget", watermark, &lastFullSyncAt))
	var seen []NotificationListOptions
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					seen = append(seen, opts)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	require.Len(seen, 2)
	assert.True(seen[0].All)
	assert.True(seen[0].Participating)
	assert.Equal(1, seen[0].Page)
	assert.Equal("acme", seen[0].RepoOwner)
	assert.Equal("widget", seen[0].RepoName)
	assert.Nil(seen[0].Since)
	assert.True(seen[1].All)
	assert.False(seen[1].Participating)
	assert.Equal(1, seen[1].Page)
	assert.Equal("acme", seen[1].RepoOwner)
	assert.Equal("widget", seen[1].RepoName)
	assert.Nil(seen[1].Since)
}

func TestSyncNotificationsNewRepoFullSyncsWithoutResettingSiblings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	_, err = d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "new-repo"))
	require.NoError(err)
	watermark := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	lastFullSyncAt := time.Now().UTC()
	require.NoError(d.UpdateNotificationSyncWatermark(t.Context(), "github", "github.com", "acme", "widget", watermark, &lastFullSyncAt))
	var seen []NotificationListOptions
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				listNotificationsFn: func(_ context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
					seen = append(seen, opts)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{
			{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
			{Owner: "acme", Name: "new-repo", PlatformHost: "github.com"},
		},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncNotifications(t.Context()))
	require.Len(seen, 4)
	// Repos scan in sorted order: the newly tracked repository has no
	// watermark and full-syncs, while the established sibling keeps its
	// incremental since window instead of being reset by the tracked-set
	// change.
	assert.True(seen[0].All)
	assert.True(seen[0].Participating)
	assert.Equal(1, seen[0].Page)
	assert.Equal("acme", seen[0].RepoOwner)
	assert.Equal("new-repo", seen[0].RepoName)
	assert.Nil(seen[0].Since)
	assert.True(seen[1].All)
	assert.False(seen[1].Participating)
	assert.Equal(1, seen[1].Page)
	assert.Equal("acme", seen[1].RepoOwner)
	assert.Equal("new-repo", seen[1].RepoName)
	assert.Nil(seen[1].Since)
	assert.True(seen[2].All)
	assert.True(seen[2].Participating)
	assert.Equal(1, seen[2].Page)
	assert.Equal("acme", seen[2].RepoOwner)
	assert.Equal("widget", seen[2].RepoName)
	if assert.NotNil(seen[2].Since) {
		assert.True(watermark.Add(-notificationSyncSinceOverlap).Equal(*seen[2].Since))
	}
	assert.True(seen[3].All)
	assert.False(seen[3].Participating)
	assert.Equal(1, seen[3].Page)
	assert.Equal("acme", seen[3].RepoOwner)
	assert.Equal("widget", seen[3].RepoName)
	if assert.NotNil(seen[3].Since) {
		assert.True(watermark.Add(-notificationSyncSinceOverlap).Equal(*seen[3].Since))
	}
}

func TestRepoSyncMarksClosedLinkedNotificationsDone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	number := 7
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       700,
		Number:           number,
		URL:              "https://github.com/acme/widget/pull/7",
		Title:            "Close me",
		State:            "open",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
		PlatformHeadSHA:  "head-open",
		PlatformBaseSHA:  "base-open",
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
	})
	require.NoError(err)
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform:               "github",
		PlatformHost:           "github.com",
		PlatformNotificationID: "thread-closed-pr",
		RepoID:                 &repoID,
		RepoOwner:              "acme",
		RepoName:               "widget",
		SubjectType:            "PullRequest",
		SubjectTitle:           "Close me",
		WebURL:                 "https://github.com/acme/widget/pull/7",
		ItemNumber:             &number,
		ItemType:               "pr",
		Reason:                 "mention",
		Unread:                 true,
		SourceUpdatedAt:        now,
		SyncedAt:               now,
	}}))
	closed := now.Add(time.Hour)
	state := "closed"
	title := "Close me"
	url := "https://github.com/acme/widget/pull/7"
	headSHA := "head-closed"
	baseSHA := "base-closed"
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				openPRs:    []*gh.PullRequest{},
				openIssues: []*gh.Issue{},
				getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
					return &gh.PullRequest{
						Number:    &number,
						Title:     &title,
						HTMLURL:   &url,
						State:     &state,
						UpdatedAt: makeTimestamp(closed),
						CreatedAt: makeTimestamp(now),
						ClosedAt:  makeTimestamp(closed),
						Head:      &gh.PullRequestBranch{SHA: &headSHA},
						Base:      &gh.PullRequestBranch{SHA: &baseSHA},
					}, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{
			Owner: "acme", Name: "widget", PlatformHost: "github.com",
			PlatformExternalID: "repo-acme-widget",
		}},
		time.Minute,
		nil,
		nil,
	)

	syncer.RunOnce(t.Context())
	active, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "active"})
	require.NoError(err)
	assert.Empty(active)
	done, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "done"})
	require.NoError(err)
	require.Len(done, 1)
	assert.Equal("closed", done[0].DoneReason)
	assert.NotNil(done[0].DoneAt)
}

func TestSyncIssueOnProviderMarksClosedLinkedNotificationsDone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	number := 11
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     2001,
		Number:         number,
		URL:            "https://gitlab.com/acme/widget/-/issues/11",
		Title:          "stale issue",
		Author:         "grace",
		State:          "open",
		CreatedAt:      now.Add(-time.Hour),
		UpdatedAt:      now.Add(-time.Hour),
		LastActivityAt: now.Add(-time.Hour),
	})
	require.NoError(err)
	require.NoError(d.UpsertNotifications(ctx, []db.Notification{{
		Platform:               "gitlab",
		PlatformHost:           "gitlab.com",
		PlatformNotificationID: "thread-closed-issue",
		RepoID:                 &repoID,
		RepoOwner:              "acme",
		RepoName:               "widget",
		SubjectType:            "Issue",
		SubjectTitle:           "Close me",
		WebURL:                 "https://gitlab.com/acme/widget/-/issues/11",
		ItemNumber:             &number,
		ItemType:               "issue",
		Reason:                 "mention",
		Unread:                 true,
		SourceUpdatedAt:        now,
		SyncedAt:               now,
	}}))

	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		issues: []platform.Issue{{
			Repo:           platformRepoRef(repo),
			PlatformID:     2001,
			Number:         number,
			URL:            "https://gitlab.com/acme/widget/-/issues/11",
			Title:          "closed issue",
			Author:         "grace",
			State:          "closed",
			CreatedAt:      now,
			UpdatedAt:      now.Add(time.Minute),
			LastActivityAt: now.Add(time.Minute),
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	active, err := d.ListNotifications(ctx, db.ListNotificationsOpts{State: "active"})
	require.NoError(err)
	require.Len(active, 1)

	require.NoError(syncer.SyncIssueOnProvider(ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", number))

	active, err = d.ListNotifications(ctx, db.ListNotificationsOpts{State: "active"})
	require.NoError(err)
	assert.Empty(active)
	done, err := d.ListNotifications(ctx, db.ListNotificationsOpts{State: "done"})
	require.NoError(err)
	require.Len(done, 1)
	assert.Equal("closed", done[0].DoneReason)
	assert.NotNil(done[0].DoneAt)
}

func TestDiffSyncErrorUserMessageSanitized(t *testing.T) {
	assert := assert.New(t)
	// A representative leak: clone path, ref, SHA, and command stderr.
	leaky := fmt.Errorf(
		"rev-parse refs/pull/42/head for merged PR #42: " +
			"exec /home/user/.kenn-forge/clones/github.com/owner/repo.git: " +
			"fatal: ambiguous argument 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef'")

	cases := []struct {
		name string
		code DiffSyncErrorCode
	}{
		{"clone unavailable", DiffSyncCodeCloneUnavailable},
		{"commit unreachable", DiffSyncCodeCommitUnreachable},
		{"merge base failed", DiffSyncCodeMergeBaseFailed},
		{"internal", DiffSyncCodeInternal},
		{"unknown code", DiffSyncErrorCode("not_a_real_code")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &DiffSyncError{Code: tc.code, Err: leaky}
			msg := e.UserMessage()
			assert.NotEmpty(msg, "user message should never be empty")
			assert.NotContains(msg, "/home/user", "user message must not leak filesystem paths")
			assert.NotContains(msg, "refs/pull/", "user message must not leak git refs")
			assert.NotContains(msg, "deadbeef", "user message must not leak SHAs")
			assert.NotContains(msg, "rev-parse", "user message must not leak git command names")
			assert.NotContains(msg, "fatal:", "user message must not leak git stderr")
		})
	}

	// Error() (used for server-side logs) is allowed to include the
	// underlying detail; only UserMessage() is the public surface.
	e := &DiffSyncError{Code: DiffSyncCodeCommitUnreachable, Err: leaky}
	assert.Contains(e.Error(), "commit_unreachable",
		"server-side Error() should include the categorization")
	assert.Contains(e.Error(), "deadbeef",
		"server-side Error() may include underlying detail for debugging")
}

func TestSyncCreatesAndUpdatesPRs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	commitMsg := "initial commit"
	commitSHA := "abc123def456"
	commitDate := makeTimestamp(now.Add(-1 * time.Hour))
	ciState := "success"

	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(1, now)},
		commits: []*gh.RepositoryCommit{
			{
				SHA: &commitSHA,
				Commit: &gh.Commit{
					Message: &commitMsg,
					Author: &gh.CommitAuthor{
						Name: new("dev"),
						Date: commitDate,
					},
				},
			},
		},
		reviews:  []*gh.PullRequestReview{},
		comments: []*gh.IssueComment{},
		ciStatus: &gh.CombinedStatus{State: &ciState},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	// PR should be in the DB.
	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(1, pr.Number)

	// Kanban state should have been created.
	ks, err := d.GetKanbanState(ctx, pr.ID)
	require.NoError(err)
	require.NotNil(ks)
	assert.Equal("new", ks.Status)

	// Commit event should have been stored (via detail drain).
	events, err := d.ListMREvents(ctx, pr.ID)
	require.NoError(err)
	require.NotEmpty(events)
	found := false
	for _, e := range events {
		if e.EventType == "commit" {
			found = true
			break
		}
	}
	assert.True(found)
}

func TestSyncRepoOverviewPreservesTimelineWhenCloneUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	oldPublishedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	oldTimelineUpdatedAt := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	oldCommitsSince := 7
	err = d.UpsertRepoOverview(ctx, repoID, db.RepoOverview{
		LatestRelease: &db.RepoRelease{
			TagName:     "v1.0.0",
			Name:        "Version 1.0.0",
			URL:         "https://github.com/owner/repo/releases/tag/v1.0.0",
			PublishedAt: &oldPublishedAt,
		},
		Releases: []db.RepoRelease{{
			TagName:     "v1.0.0",
			Name:        "Version 1.0.0",
			URL:         "https://github.com/owner/repo/releases/tag/v1.0.0",
			PublishedAt: &oldPublishedAt,
		}},
		CommitsSinceRelease: &oldCommitsSince,
		CommitTimeline: []db.RepoCommitTimelinePoint{{
			SHA:         "abc123",
			Message:     "Keep cached timeline",
			CommittedAt: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC),
		}},
		TimelineUpdatedAt: &oldTimelineUpdatedAt,
	})
	require.NoError(err)

	releaseName := "Version 1.0.0"
	client := &mockClient{
		listReleases: []*gh.RepositoryRelease{{
			TagName:     "v1.0.0",
			Name:        &releaseName,
			HTMLURL:     "https://github.com/owner/repo/releases/tag/v1.0.0",
			PublishedAt: &gh.Timestamp{Time: oldPublishedAt},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d,
		nil,
		nil,
		time.Minute,
		nil,
		nil,
	)

	syncer.syncRepoOverview(
		ctx,
		client,
		RepoRef{PlatformHost: "github.com", Owner: "owner", Name: "repo"},
		repoID,
		false,
	)

	summaries, err := d.ListRepoSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 1)
	overview := summaries[0].Overview
	require.NotNil(overview.LatestRelease)
	require.NotNil(overview.CommitsSinceRelease)
	require.Len(overview.CommitTimeline, 1)
	require.NotNil(overview.TimelineUpdatedAt)

	assert.Equal("v1.0.0", overview.LatestRelease.TagName)
	assert.Equal(oldPublishedAt, *overview.LatestRelease.PublishedAt)
	assert.Equal(oldCommitsSince, *overview.CommitsSinceRelease)
	assert.Equal("abc123", overview.CommitTimeline[0].SHA)
	assert.Equal("Keep cached timeline", overview.CommitTimeline[0].Message)
	assert.Equal(oldTimelineUpdatedAt, *overview.TimelineUpdatedAt)
}

func TestSyncRepoOverviewUsesTagsWhenRepoHasNoReleases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	tagName := "v1.2.3"
	sha := "abcdef1234567890abcdef1234567890abcdef12"
	client := &mockClient{
		listReleases: []*gh.RepositoryRelease{},
		listTags: []*gh.RepositoryTag{{
			Name: &tagName,
			Commit: &gh.Commit{
				SHA: &sha,
			},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d,
		nil,
		nil,
		time.Minute,
		nil,
		nil,
	)

	syncer.syncRepoOverview(
		ctx,
		client,
		RepoRef{PlatformHost: "github.com", Owner: "owner", Name: "repo"},
		repoID,
		false,
	)

	summaries, err := d.ListRepoSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 1)
	overview := summaries[0].Overview
	require.NotNil(overview.LatestRelease)
	require.Len(overview.Releases, 1)

	assert.Equal("v1.2.3", overview.LatestRelease.TagName)
	assert.Equal("v1.2.3", overview.LatestRelease.Name)
	assert.Equal("https://github.com/owner/repo/tree/v1.2.3", overview.LatestRelease.URL)
	assert.Equal(sha, overview.LatestRelease.TargetCommitish)
	assert.Nil(overview.LatestRelease.PublishedAt)
	assert.False(overview.LatestRelease.Prerelease)
}

func TestSyncRepoOverviewClearsReleasesWhenTagFallbackFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	publishedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	commitsSince := 9
	err = d.UpsertRepoOverview(ctx, repoID, db.RepoOverview{
		LatestRelease: &db.RepoRelease{
			TagName:     "v1.0.0",
			Name:        "Version 1.0.0",
			URL:         "https://github.com/owner/repo/releases/tag/v1.0.0",
			PublishedAt: &publishedAt,
		},
		Releases: []db.RepoRelease{{
			TagName:     "v1.0.0",
			Name:        "Version 1.0.0",
			URL:         "https://github.com/owner/repo/releases/tag/v1.0.0",
			PublishedAt: &publishedAt,
		}},
		CommitsSinceRelease: &commitsSince,
		CommitTimeline: []db.RepoCommitTimelinePoint{{
			SHA:         "abc123",
			Message:     "Old release timeline",
			CommittedAt: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC),
		}},
	})
	require.NoError(err)

	client := &mockClient{
		listReleases: []*gh.RepositoryRelease{},
		listTagsErr:  errors.New("tags unavailable"),
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d,
		nil,
		nil,
		time.Minute,
		nil,
		nil,
	)

	syncer.syncRepoOverview(
		ctx,
		client,
		RepoRef{PlatformHost: "github.com", Owner: "owner", Name: "repo"},
		repoID,
		false,
	)

	summaries, err := d.ListRepoSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 1)
	overview := summaries[0].Overview

	assert.Nil(overview.LatestRelease)
	assert.Empty(overview.Releases)
	assert.Nil(overview.CommitsSinceRelease)
	assert.Empty(overview.CommitTimeline)
}

func TestSyncStoresForcePushEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	commitSHA := "abc123def456"
	commitMsg := "fix: tighten validation"
	ciState := "success"

	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(1, now)},
		commits: []*gh.RepositoryCommit{{
			SHA: &commitSHA,
			Commit: &gh.Commit{
				Message: &commitMsg,
				Author:  &gh.CommitAuthor{Name: new("dev"), Date: makeTimestamp(now.Add(-1 * time.Hour))},
			},
		}},
		timelineEvents: []PullRequestTimelineEvent{{
			EventType: "force_push",
			Actor:     "alice",
			BeforeSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			AfterSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Ref:       "feature",
			CreatedAt: now.Add(-30 * time.Minute),
		}},
		reviews:  []*gh.PullRequestReview{},
		comments: []*gh.IssueComment{},
		ciStatus: &gh.CombinedStatus{State: &ciState},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)

	events, err := d.ListMREvents(ctx, pr.ID)
	require.NoError(err)
	require.NotEmpty(events)

	var forcePush *db.MREvent
	var commit *db.MREvent
	for i := range events {
		if events[i].EventType == "force_push" {
			forcePush = &events[i]
		}
		if events[i].EventType == "commit" {
			commit = &events[i]
		}
	}
	require.NotNil(forcePush)
	require.NotNil(commit)
	assert.Equal("alice", forcePush.Author)
	assert.Equal("aaaaaaa -> bbbbbbb", forcePush.Summary)
	assert.Contains(forcePush.MetadataJSON, `"ref":"feature"`)
	assert.Contains(commit.MetadataJSON, `"commit_order":1`)
}

func TestRefreshTimelineDoesNotRewriteProviderPullRequestActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	providerActivity := now.Add(-2 * time.Hour)
	forcePushAt := now.Add(30 * time.Minute)
	commitSHA := "abc123def456"
	commitMsg := "fix: tighten validation"

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "force push activity",
		Author:         "dev",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now.Add(-3 * time.Hour),
		UpdatedAt:      providerActivity,
		LastActivityAt: providerActivity,
	})
	require.NoError(err)

	mc := &mockClient{
		commits: []*gh.RepositoryCommit{{
			SHA: &commitSHA,
			Commit: &gh.Commit{
				Message: &commitMsg,
				Author:  &gh.CommitAuthor{Name: new("dev"), Date: makeTimestamp(now.Add(-1 * time.Hour))},
			},
		}},
		timelineEvents: []PullRequestTimelineEvent{{
			EventType: "force_push",
			Actor:     "alice",
			BeforeSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			AfterSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Ref:       "feature",
			CreatedAt: forcePushAt,
		}},
		reviews:  []*gh.PullRequestReview{},
		comments: []*gh.IssueComment{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{repo}, time.Minute, nil, testBudget(500))
	require.NoError(syncer.refreshTimeline(ctx, repo, mrID,
		mergeRequestSnapshotRevision(t, d, repoID, 1), buildOpenPR(1, now), ""))

	pr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(providerActivity, pr.LastActivityAt)
}

func TestRefreshTimelineFetchFailureDoesNotRewriteProviderPullRequestActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	forcePushAt := now.Add(30 * time.Minute)
	commitSHA := "abc123def456"
	commitMsg := "fix: tighten validation"

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "force push activity",
		Author:         "dev",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now.Add(-3 * time.Hour),
		UpdatedAt:      now.Add(-2 * time.Hour),
		LastActivityAt: forcePushAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		EventType:      "force_push",
		Author:         "alice",
		Summary:        "aaaaaaa -> bbbbbbb",
		MetadataJSON:   `{"ref":"feature"}`,
		CreatedAt:      forcePushAt,
		DedupeKey:      "force-push-feature-bbbbbbbbbbbb",
	}}))

	mc := &mockClient{
		commits: []*gh.RepositoryCommit{{
			SHA: &commitSHA,
			Commit: &gh.Commit{
				Message: &commitMsg,
				Author:  &gh.CommitAuthor{Name: new("dev"), Date: makeTimestamp(now.Add(-1 * time.Hour))},
			},
		}},
		timelineEventsErr: errors.New("graphql unavailable"),
		reviews:           []*gh.PullRequestReview{},
		comments:          []*gh.IssueComment{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{repo}, time.Minute, nil, testBudget(500))
	require.NoError(syncer.refreshTimeline(ctx, repo, mrID,
		mergeRequestSnapshotRevision(t, d, repoID, 1), buildOpenPR(1, now), ""))

	pr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(forcePushAt, pr.LastActivityAt)
}

func TestSyncAssignsStableCommitOrderKeysAcrossForcePushReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	oldBaseSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldHeadSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	newBaseSHA := "cccccccccccccccccccccccccccccccccccccccc"
	newHeadSHA := "dddddddddddddddddddddddddddddddddddddddd"

	commit := func(sha, msg string, committedAt time.Time) *gh.RepositoryCommit {
		return &gh.RepositoryCommit{
			SHA: &sha,
			Commit: &gh.Commit{
				Message: &msg,
				Author:  &gh.CommitAuthor{Name: new("dev"), Date: makeTimestamp(committedAt)},
			},
		}
	}

	mc := &mockClient{
		commits: []*gh.RepositoryCommit{
			commit(newBaseSHA, "new base", now.Add(-30*time.Minute)),
			commit(newHeadSHA, "new head", now.Add(-20*time.Minute)),
		},
		timelineEvents: []PullRequestTimelineEvent{{
			EventType: "force_push",
			Actor:     "alice",
			BeforeSHA: oldHeadSHA,
			AfterSHA:  newHeadSHA,
			Ref:       "feature",
			CreatedAt: now.Add(-10 * time.Minute),
		}},
		reviews:  []*gh.PullRequestReview{},
		comments: []*gh.IssueComment{},
	}
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "force push",
		Author:         "dev",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now.Add(-3 * time.Hour),
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: mrID,
			EventType:      "commit",
			Summary:        oldBaseSHA,
			Body:           "old base",
			MetadataJSON:   `{"commit_order":1,"commit_order_key":1}`,
			CreatedAt:      now.Add(-2 * time.Hour),
			DedupeKey:      "commit-" + oldBaseSHA[:12],
		},
		{
			MergeRequestID: mrID,
			EventType:      "commit",
			Summary:        oldHeadSHA,
			Body:           "old head",
			MetadataJSON:   `{"commit_order":2,"commit_order_key":2}`,
			CreatedAt:      now.Add(-1 * time.Hour),
			DedupeKey:      "commit-" + oldHeadSHA[:12],
		},
	}))

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{repo}, time.Minute, nil, testBudget(500))
	require.NoError(syncer.refreshTimeline(ctx, repo, mrID,
		mergeRequestSnapshotRevision(t, d, repoID, 1), buildOpenPR(1, now), ""))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)

	findCommit := func(sha string) *db.MREvent {
		for i := range events {
			if events[i].EventType == "commit" && events[i].Summary == sha {
				return &events[i]
			}
		}
		return nil
	}

	oldHead := findCommit(oldHeadSHA)
	newHead := findCommit(newHeadSHA)
	require.NotNil(oldHead)
	require.NotNil(newHead)
	assert.Contains(oldHead.MetadataJSON, `"commit_order":2`)
	assert.Contains(oldHead.MetadataJSON, `"commit_order_key":2`)
	assert.Contains(newHead.MetadataJSON, `"commit_order":2`)
	assert.Contains(newHead.MetadataJSON, `"commit_order_key":4`)
}

func TestSyncStoresPullRequestTimelineEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		timelineEvents: []PullRequestTimelineEvent{
			{
				NodeID:            "CRE_1",
				EventType:         "cross_referenced",
				Actor:             "alice",
				CreatedAt:         now.Add(-3 * time.Minute),
				SourceType:        "Issue",
				SourceOwner:       "other",
				SourceRepo:        "repo",
				SourceNumber:      77,
				SourceTitle:       "Related bug",
				SourceURL:         "https://github.com/other/repo/issues/77",
				IsCrossRepository: true,
			},
			{
				NodeID:          "BRC_1",
				EventType:       "base_ref_changed",
				Actor:           "bob",
				CreatedAt:       now.Add(-2 * time.Minute),
				PreviousRefName: "main",
				CurrentRefName:  "release",
			},
			{
				NodeID:        "RTE_1",
				EventType:     "renamed_title",
				Actor:         "carol",
				CreatedAt:     now.Add(-1 * time.Minute),
				PreviousTitle: "Old",
				CurrentTitle:  "New",
			},
			{
				NodeID:               "CDE_1",
				EventType:            "comment_deleted",
				Actor:                "maintainer",
				DeletedCommentAuthor: "reviewer",
				CreatedAt:            now.Add(-30 * time.Second),
			},
		},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)

	events, err := d.ListMREvents(ctx, pr.ID)
	require.NoError(err)

	byType := map[string]db.MREvent{}
	for _, event := range events {
		byType[event.EventType] = event
	}
	assert.Contains(byType, "cross_referenced")
	assert.Contains(byType, "base_ref_changed")
	assert.Contains(byType, "renamed_title")
	assert.Contains(byType, "comment_deleted")
	assert.Contains(byType["cross_referenced"].MetadataJSON, `"source_title":"Related bug"`)
	assert.Equal("main -> release", byType["base_ref_changed"].Summary)
	assert.Equal(`"Old" -> "New"`, byType["renamed_title"].Summary)
	assert.Equal("deleted a comment from reviewer", byType["comment_deleted"].Summary)
}

func TestSyncIgnoresPullRequestTimelineFetchFailures(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	commentID := int64(123)
	body := "human comment"
	user := &gh.User{Login: new("alice")}

	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			User:      user,
			Body:      &body,
			CreatedAt: makeTimestamp(now.Add(-time.Minute)),
		}},
		reviews:           []*gh.PullRequestReview{},
		commits:           []*gh.RepositoryCommit{},
		timelineEventsErr: errors.New("graphql unavailable"),
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)
	events, err := d.ListMREvents(ctx, pr.ID)
	require.NoError(err)
	require.NotEmpty(events)
	require.Equal("issue_comment", events[0].EventType)
}

func TestSyncStoresPRLabels(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 2, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(1, now)
	pr.Labels = []*gh.Label{
		buildGitHubLabel(501, "needs-review", "Needs another reviewer", "fbca04", true),
	}

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{pr},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	require.Len(stored.Labels, 1)
	require.Equal("needs-review", stored.Labels[0].Name)
	require.Equal("Needs another reviewer", stored.Labels[0].Description)
	require.Equal("fbca04", stored.Labels[0].Color)
	require.True(stored.Labels[0].IsDefault)
	require.Equal(int64(501), stored.Labels[0].PlatformID)
	require.True(stored.Labels[0].UpdatedAt.Equal(now))
}

func TestSyncRefreshesRepoLabelCatalog(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	client := &labelCatalogTestClient{
		mockClient: &mockClient{
			openPRs:   []*gh.PullRequest{},
			comments:  []*gh.IssueComment{},
			reviews:   []*gh.PullRequestReview{},
			commits:   []*gh.RepositoryCommit{},
			ciStatus:  &gh.CombinedStatus{},
			checkRuns: []*gh.CheckRun{},
		},
		labels: []*gh.Label{buildGitHubLabel(901, "triage", "Needs review", "fbca04", false)},
	}

	syncer := NewSyncer(map[string]Client{"github.com": client}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	repo, err := d.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	require.NotNil(repo)
	labels, freshness, err := d.ListRepoLabelCatalog(ctx, repo.ID)
	require.NoError(err)
	require.Len(labels, 1)
	require.Equal("triage", labels[0].Name)
	require.NotNil(freshness.CheckedAt)
	require.NotNil(freshness.SyncedAt)
	require.Equal(int32(1), client.calls.Load())
}

func TestRefreshRepoLabelCatalogRejectsABARoutePayload(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	observedAt := time.Now().UTC()
	original, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, observedAt)
	require.NoError(err)

	started := make(chan struct{})
	release := make(chan struct{})
	client := &labelCatalogTestClient{
		mockClient: &mockClient{},
		listRepoLabelsFn: func(context.Context, string, string) ([]*gh.Label, error) {
			close(started)
			<-release
			return []*gh.Label{buildGitHubLabel(901, "replacement", "", "fbca04", false)}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil, nil,
		time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		done <- syncer.RefreshRepoLabelCatalog(ctx, original.Repository)
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	labels, _, err := database.ListRepoLabelCatalog(ctx, original.Repository.ID)
	require.NoError(err)
	require.Empty(labels)
}

func TestSyncMRReplacesLabelsOnResync(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(1, now)
	pr.Labels = []*gh.Label{
		buildGitHubLabel(701, "bug", "Bug fix", "d73a4a", true),
	}

	mc := &mockClient{singlePR: pr, comments: []*gh.IssueComment{}, reviews: []*gh.PullRequestReview{}, commits: []*gh.RepositoryCommit{}}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))

	require.NoError(syncer.SyncMR(ctx, "owner", "repo", 1))

	pr.Labels = []*gh.Label{
		buildGitHubLabel(702, "feature", "New feature", "0e8a16", false),
	}
	pr.UpdatedAt = makeTimestamp(now.Add(time.Minute))

	require.NoError(syncer.SyncMR(ctx, "owner", "repo", 1))

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	require.Len(stored.Labels, 1)
	require.Equal("feature", stored.Labels[0].Name)
	require.Equal("New feature", stored.Labels[0].Description)
	require.Equal("0e8a16", stored.Labels[0].Color)
	require.False(stored.Labels[0].IsDefault)
	require.Equal(int64(702), stored.Labels[0].PlatformID)
}

func TestSyncIssueReplacesLabelsOnResync(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 4, 12, 0, 0, 0, time.UTC)
	issueNumber := 42
	issueTitle := "broken thing"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/42"
	issueBody := ""
	issueID := int64(900042)
	issue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
		Labels: []*gh.Label{
			buildGitHubLabel(801, "bug", "Something is broken", "d73a4a", true),
		},
	}

	mc := &mockClient{getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
		return issue, nil
	}, comments: []*gh.IssueComment{}}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.SyncIssue(ctx, "owner", "repo", issueNumber))

	issue.Labels = []*gh.Label{
		buildGitHubLabel(802, "docs", "Documentation", "0075ca", false),
	}
	issue.UpdatedAt = makeTimestamp(now.Add(time.Minute))

	require.NoError(syncer.SyncIssue(ctx, "owner", "repo", issueNumber))

	stored, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(stored)
	require.Len(stored.Labels, 1)
	require.Equal("docs", stored.Labels[0].Name)
	require.Equal("Documentation", stored.Labels[0].Description)
	require.Equal("0075ca", stored.Labels[0].Color)
	require.False(stored.Labels[0].IsDefault)
	require.Equal(int64(802), stored.Labels[0].PlatformID)
}

// TestSyncIssueNilUpdatedAt verifies refreshIssueTimeline
// tolerates a GitHub issue whose updated_at is null. Before
// the nil guard this panicked the sync goroutine when GitHub
// occasionally returned missing timestamps.
func TestSyncIssueNilUpdatedAt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 4, 12, 0, 0, 0, time.UTC)
	issueNumber := 7
	issueTitle := "no updated_at"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/7"
	issueBody := ""
	issueID := int64(900007)
	issue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: nil, // the case under test
	}

	commentID := int64(9001)
	commentBody := "later comment"
	commentURL := "https://github.com/owner/repo/issues/7#issuecomment-9001"
	commentTime := now.Add(2 * time.Hour)
	commentAuthor := "alice"
	comment := &gh.IssueComment{
		ID:        &commentID,
		Body:      &commentBody,
		HTMLURL:   &commentURL,
		CreatedAt: makeTimestamp(commentTime),
		UpdatedAt: makeTimestamp(commentTime),
		User:      &gh.User{Login: &commentAuthor},
	}

	mc := &mockClient{
		getIssueFn: func(
			context.Context, string, string, int,
		) (*gh.Issue, error) {
			return issue, nil
		},
		comments: []*gh.IssueComment{comment},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner:        "owner",
			Name:         "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, nil,
	)

	// Must not panic and must succeed.
	require.NoError(
		syncer.SyncIssue(ctx, "owner", "repo", issueNumber),
	)

	stored, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(stored)
	// last_activity_at should track the comment timestamp
	// even though the issue had no updated_at.
	assert.Equal(commentTime.UTC(), stored.LastActivityAt.UTC())
}

// TestSyncIssueNilUpdatedAtNoComments verifies the CreatedAt
// fallback when UpdatedAt is nil and there are no comments.
// Without the fallback, lastActivity would be zero time and
// the issue would sort incorrectly in activity views.
func TestSyncIssueNilUpdatedAtNoComments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	created := time.Date(2024, 6, 4, 12, 0, 0, 0, time.UTC)
	issueNumber := 8
	issueTitle := "no updated_at, no comments"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/8"
	issueBody := ""
	issueID := int64(900008)
	issue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(created),
		UpdatedAt: nil,
	}

	mc := &mockClient{
		getIssueFn: func(
			context.Context, string, string, int,
		) (*gh.Issue, error) {
			return issue, nil
		},
		comments: []*gh.IssueComment{},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner:        "owner",
			Name:         "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, nil,
	)

	require.NoError(
		syncer.SyncIssue(ctx, "owner", "repo", issueNumber),
	)

	stored, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(created.UTC(), stored.LastActivityAt.UTC(),
		"lastActivity should fall back to CreatedAt, not zero time")
}

// TestHostForConcurrentSetRepos verifies that concurrent
// SetRepos calls don't race with hostFor readers. Run under
// go test -race to catch regressions in the reposMu locking
// inside hostFor. Readers exercise all three hostFor return
// paths (tracked-with-host, tracked-with-empty-host, not-found)
// so a future refactor that reintroduces unsynchronized access
// on any branch is caught.
func TestSyncerOwnsConstructorRepositorySlice(t *testing.T) {
	assert := assert.New(t)
	repos := []RepoRef{{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "org-a", Name: "old-name", RepoPath: "org-a/old-name",
	}}
	syncer := NewSyncerWithRegistry(
		nil, nil, nil, repos, time.Minute, nil, nil,
	)

	syncer.publishResolvedRepository(repos[0], RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "org-a", Name: "new-name", RepoPath: "org-a/new-name",
	}, true)

	assert.Equal("old-name", repos[0].Name)
	assert.Equal("org-a/old-name", repos[0].RepoPath)
}

func TestHostForConcurrentSetRepos(t *testing.T) {
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}}, nil, nil,
		[]RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	const iterations = 1000

	// Writer: rotate between three shapes so readers see each
	// hostFor branch at some point in the run.
	wg.Go(func() {
		<-start
		withHost := []RepoRef{
			{Owner: "o", Name: "r", PlatformHost: "ghe.example.com"},
			{Owner: "o2", Name: "r2", PlatformHost: "github.com"},
		}
		emptyHost := []RepoRef{
			{Owner: "o", Name: "r", PlatformHost: ""},
		}
		orig := []RepoRef{
			{Owner: "o", Name: "r", PlatformHost: "github.com"},
		}
		for range iterations {
			syncer.SetRepos(withHost)
			syncer.SetRepos(emptyHost)
			syncer.SetRepos(orig)
		}
	})

	// Readers: hit every unlocked hostFor caller, including
	// the not-found branch (ghost/missing) and the empty-host
	// branch driven by the writer's emptyHost state.
	for range 4 {
		wg.Go(func() {
			<-start
			for range iterations {
				_ = syncer.HostForRepo("o", "r")
				_ = syncer.HostForRepo("ghost", "missing")
				_ = syncer.IsTrackedRepo("o", "r")
			}
		})
	}

	close(start)
	wg.Wait()
}

// TestIsTrackedRepoConcurrentPublishResolvedRepository verifies that
// IsTrackedRepo readers don't race with publishResolvedRepository,
// which mutates elements of the tracked slice in place rather than
// swapping the slice like SetRepos. Run under go test -race.
func TestIsTrackedRepoConcurrentPublishResolvedRepository(t *testing.T) {
	orig := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	renamed := orig
	renamed.Name = "widget-renamed"
	renamed.RepoPath = "acme/widget-renamed"
	syncer := NewSyncerWithRegistry(
		nil, nil, nil, []RepoRef{orig}, time.Minute, nil, nil,
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	const iterations = 1000

	wg.Go(func() {
		<-start
		for range iterations {
			syncer.publishResolvedRepository(orig, renamed, true)
			syncer.publishResolvedRepository(renamed, orig, true)
		}
	})
	for range 4 {
		wg.Go(func() {
			<-start
			for range iterations {
				_ = syncer.IsTrackedRepo("acme", "widget")
			}
		})
	}

	close(start)
	wg.Wait()
}

// TestReconcileRepoIdentityDiscardsStaleSameRouteMetadata seeds the
// catalog with an observation newer than the sync's own, so the
// catalog rejects the sync's observation even though the route is
// unchanged. The rejected snapshot's provider metadata must be
// discarded, not published over the newer catalog state.
func TestReconcileRepoIdentityDiscardsStaleSameRouteMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	future := time.Now().UTC().Add(time.Hour)
	entry, _, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "R_widget",
		Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
	}, future)
	require.NoError(err)
	require.NoError(d.UpdateRepoProviderMetadata(
		ctx, entry.Repository.ID, db.RepoProviderMetadata{
			PlatformRepoID: "R_widget",
			WebURL:         "https://github.com/acme/widget",
			CloneURL:       "https://github.com/acme/widget.git",
			DefaultBranch:  "release",
		},
	))

	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, staleMetadataRepositoryReader{
			kind: platform.KindGitHub, host: "github.com",
			repo: platform.Repository{
				Ref: platform.RepoRef{
					Platform: platform.KindGitHub, Host: "github.com",
					Owner: "acme", Name: "widget", RepoPath: "acme/widget",
					PlatformExternalID: "R_widget",
				},
				DefaultBranch: "stale-main",
				WebURL:        "https://github.com/acme/widget-stale",
				CloneURL:      "https://github.com/acme/widget-stale.git",
			},
		}),
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	authoritative, repoID, resolved, _, accepted, err :=
		syncer.reconcileRepoIdentityObservation(ctx, repo)
	require.NoError(err)
	require.False(accepted)

	assert.Equal(entry.Repository.ID, repoID)
	assert.Nil(resolved,
		"rejected observation must not surface provider metadata")
	assert.Equal("release", authoritative.DefaultBranch)
	assert.Equal("https://github.com/acme/widget", authoritative.WebURL)
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Equal("release", tracked[0].DefaultBranch,
		"published repo must keep the newer catalog metadata")
}

type staleMetadataRepositoryReader struct {
	resolverTestProvider
	repo platform.Repository
}

func (r staleMetadataRepositoryReader) GetRepository(
	context.Context, platform.RepoRef,
) (platform.Repository, error) {
	return r.repo, nil
}

func (r staleMetadataRepositoryReader) ListRepositories(
	context.Context,
	string,
	platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return nil, nil
}

func TestSyncIgnoresForcePushFetchFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	commitSHA := "abc123def456"
	commitMsg := "fix: tighten validation"
	ciState := "success"
	commentBody := "Looks good to me"
	commentID := int64(41)
	commentURL := "https://github.com/owner/repo/pull/1#issuecomment-41"
	forcePushErr := errors.New("graphql unavailable")

	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			HTMLURL:   &commentURL,
			CreatedAt: makeTimestamp(now.Add(-45 * time.Minute)),
			User:      &gh.User{Login: new("alice")},
		}},
		commits: []*gh.RepositoryCommit{{
			SHA: &commitSHA,
			Commit: &gh.Commit{
				Message: &commitMsg,
				Author:  &gh.CommitAuthor{Name: new("dev"), Date: makeTimestamp(now.Add(-1 * time.Hour))},
			},
		}},
		forcePushEventsErr: forcePushErr,
		reviews:            []*gh.PullRequestReview{},
		ciStatus:           &gh.CombinedStatus{State: &ciState},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)

	events, err := d.ListMREvents(ctx, pr.ID)
	require.NoError(err)
	require.Len(events, 2)

	var sawCommit bool
	var sawComment bool
	for _, event := range events {
		if event.EventType == "commit" {
			sawCommit = true
		}
		if event.EventType == "issue_comment" {
			sawComment = true
		}
		assert.NotEqual("force_push", event.EventType)
	}
	assert.True(sawCommit)
	assert.True(sawComment)
	assert.Equal(1, pr.CommentCount)
	assert.Equal(now, pr.LastActivityAt)
}

func TestSyncSingleFlight(t *testing.T) {
	ctx := t.Context()
	d := openTestDB(t)

	callCount := 0
	mc := &mockClient{
		openPRs: []*gh.PullRequest{},
	}
	// Wrap in a counter client to detect calls.
	_ = mc

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))

	// Simulate a concurrent run already in progress.
	syncer.running.Store(true)
	syncer.RunOnce(ctx) // should be a no-op
	syncer.running.Store(false)

	// Verify no DB side-effects: repo row should not exist because the RunOnce was skipped.
	repo, err := d.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(t, err)
	assert.Nil(t, repo)

	_ = callCount
}

func TestSyncPreservesMergeableState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	pr := buildOpenPR(1, now)
	additions := 10
	deletions := 5
	baseSHA := "base123"
	mergeableState := "dirty"
	pr.Additions = &additions
	pr.Deletions = &deletions
	pr.MergeableState = &mergeableState
	pr.Base.SHA = &baseSHA

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{pr},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))

	// First sync: full fetch occurs, MergeableState is stored.
	syncer.RunOnce(ctx)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("dirty", stored.MergeableState)

	// Second sync: same UpdatedAt means no full fetch. The list endpoint
	// does not return MergeableState, so the preservation branch runs.
	// Reset the mock so the list PR has no MergeableState (as the real
	// list endpoint would return).
	listPR := buildOpenPR(1, now) // same UpdatedAt, no MergeableState set
	listPR.Additions = nil
	listPR.Deletions = nil
	listPR.Base.SHA = &baseSHA
	mc.openPRs = []*gh.PullRequest{listPR}
	// Ensure full fetch would return empty MergeableState if it ran.
	mc.getPullRequestFn = func(_ context.Context, _, _ string, _ int) (*gh.PullRequest, error) {
		p := buildOpenPR(1, now)
		p.Base.SHA = &baseSHA
		return p, nil
	}

	syncer.RunOnce(ctx)

	stored2, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored2)
	assert.Equal("dirty", stored2.MergeableState, "MergeableState should be preserved when full fetch is skipped")
}

func TestIndexUpsertMergeRequestUpdatesKnownMergeableState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	baseMR := platform.MergeRequest{
		PlatformID:     1001,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "Conflicted PR",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		HeadSHA:        "abc123",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, platform.DBMergeRequest(repoID, baseMR))
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, testBudget(500))
	incoming := baseMR
	incoming.MergeableState = "dirty"
	err = syncer.indexUpsertMergeRequest(
		ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID,
		incoming,
		false,
	)
	require.NoError(err)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("dirty", stored.MergeableState)
}

func TestIndexUpsertMergeRequestUpdatesKnownDiffMetricsAcrossSyncs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindGitea, Host: "gitea.example.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(ref))
	require.NoError(err)
	filesChanged := 2
	baseMR := platform.MergeRequest{
		PlatformID: 1001, Number: 1, URL: "https://gitea.example.com/owner/repo/pulls/1",
		Title: "Metrics", State: "open", HeadBranch: "feature", BaseBranch: "main",
		HeadSHA: "head", BaseSHA: "base", Additions: 10, Deletions: 5,
		FilesChanged: &filesChanged, CreatedAt: now, UpdatedAt: now,
		LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, platform.DBMergeRequest(repoID, baseMR))
	require.NoError(err)
	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, nil)
	repo := RepoRef{
		Platform: platform.KindGitea, PlatformHost: ref.Host,
		Owner: ref.Owner, Name: ref.Name,
	}

	filesChanged = 4
	incoming := baseMR
	incoming.Additions = 21
	incoming.AdditionsKnown = true
	incoming.Deletions = 7
	incoming.DeletionsKnown = true
	incoming.FilesChanged = &filesChanged
	incoming.UpdatedAt = now.Add(time.Minute)
	incoming.LastActivityAt = incoming.UpdatedAt
	require.NoError(syncer.indexUpsertMergeRequest(
		ctx, repo, repoID, incoming, false,
	))
	stored, err := d.GetMergeRequest(
		ctx, string(ref.Platform), ref.Host, ref.Owner, ref.Name, 1,
	)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(21, stored.Additions)
	assert.Equal(7, stored.Deletions)
	require.NotNil(stored.FilesChanged)
	assert.Equal(4, *stored.FilesChanged)

	filesChanged = 0
	incoming.Additions = 0
	incoming.Deletions = 0
	incoming.FilesChanged = &filesChanged
	incoming.UpdatedAt = now.Add(2 * time.Minute)
	incoming.LastActivityAt = incoming.UpdatedAt
	require.NoError(syncer.indexUpsertMergeRequest(
		ctx, repo, repoID, incoming, false,
	))
	stored, err = d.GetMergeRequest(
		ctx, string(ref.Platform), ref.Host, ref.Owner, ref.Name, 1,
	)
	require.NoError(err)
	require.NotNil(stored)
	assert.Zero(stored.Additions)
	assert.Zero(stored.Deletions)
	require.NotNil(stored.FilesChanged)
	assert.Zero(*stored.FilesChanged)
}

func TestIndexUpsertMergeRequestPreservesCachedCIForSameHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "Cached CI PR",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "same-head",
		CIStatus:        "failure",
		CIChecksJSON:    `[{"name":"build","status":"completed","conclusion":"failure"}]`,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, testBudget(500))
	err = syncer.indexUpsertMergeRequest(
		ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID,
		platform.MergeRequest{
			PlatformID:     1001,
			Number:         1,
			URL:            "https://github.com/owner/repo/pull/1",
			Title:          "Cached CI PR",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "same-head",
			CreatedAt:      now,
			UpdatedAt:      now.Add(time.Minute),
			LastActivityAt: now.Add(time.Minute),
		},
		false,
	)
	require.NoError(err)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("failure", stored.CIStatus)
	assert.Contains(stored.CIChecksJSON, "build")
}

func TestIndexUpsertMergeRequestPreservesReviewDecisionWhenOmitted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "Approved PR",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "same-head",
		ReviewDecision:  "APPROVED",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, testBudget(500))
	err = syncer.indexUpsertMergeRequest(
		ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID,
		platform.MergeRequest{
			PlatformID:     1001,
			Number:         1,
			URL:            "https://github.com/owner/repo/pull/1",
			Title:          "Approved PR",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "same-head",
			CreatedAt:      now,
			UpdatedAt:      now.Add(time.Minute),
			LastActivityAt: now.Add(time.Minute),
		},
		false,
	)
	require.NoError(err)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("APPROVED", stored.ReviewDecision)
}

func TestIndexUpsertMergeRequestReclassifiesWorkspaceHeadRepoOnForkRetarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	baseMR := platform.MergeRequest{
		PlatformID:     1001,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "Same-repo PR",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		HeadSHA:        "abc123",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, platform.DBMergeRequest(repoID, baseMR))
	require.NoError(err)

	ws := &db.Workspace{
		ID:           "ws-fork-reclassify",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "owner",
		RepoName:     "repo",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "abc123",
		MRHeadRepo:   nil, // classified same-repo when the workspace was created
		WorktreePath: t.TempDir(),
		TmuxSession:  "session",
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, testBudget(500))
	incoming := baseMR
	incoming.UpdatedAt = now.Add(time.Minute)
	incoming.LastActivityAt = now.Add(time.Minute)
	incoming.HeadRepoCloneURL = "https://github.com/forker/repo.git"
	err = syncer.indexUpsertMergeRequest(
		ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID,
		incoming,
		false,
	)
	require.NoError(err)

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal("https://github.com/forker/repo.git", *reclassified.MRHeadRepo)
}

func TestIndexUpsertMergeRequestKeepsKnownWorkspaceHeadRepoOnUnknownSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	baseMR := platform.MergeRequest{
		PlatformID:       1001,
		Number:           1,
		URL:              "https://github.com/owner/repo/pull/1",
		Title:            "Fork PR",
		State:            "open",
		HeadBranch:       "feature",
		BaseBranch:       "main",
		HeadSHA:          "abc123",
		HeadRepoCloneURL: "https://github.com/forker/repo.git",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}
	_, err = d.UpsertMergeRequest(ctx, platform.DBMergeRequest(repoID, baseMR))
	require.NoError(err)

	forkURL := "https://github.com/forker/repo.git"
	ws := &db.Workspace{
		ID:           "ws-fork-known",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "owner",
		RepoName:     "repo",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "abc123",
		MRHeadRepo:   &forkURL,
		WorktreePath: t.TempDir(),
		TmuxSession:  "session",
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	// A later list-page sync that cannot determine the head repo (e.g. a
	// GitLab list endpoint) must not downgrade an already-known fork
	// classification back to "unknown".
	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, testBudget(500))
	incoming := baseMR
	incoming.UpdatedAt = now.Add(time.Minute)
	incoming.LastActivityAt = now.Add(time.Minute)
	incoming.HeadRepoCloneURL = ""
	incoming.HeadRepoCloneURLUnknown = true
	err = syncer.indexUpsertMergeRequest(
		ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID,
		incoming,
		false,
	)
	require.NoError(err)

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal(forkURL, *reclassified.MRHeadRepo)
}

// TestCommitMergeRequestParentSnapshotReclassifiesWorkspaceHeadRepo proves
// the reclassification fan-out lives in CommitMergeRequestParentSnapshot
// itself, not just in the indexUpsertMergeRequest (list sync) caller. It
// calls CommitMergeRequestParentSnapshot directly, the same way the GraphQL
// bulk path (syncOpenMRFromBulk), detail refresh (fetchMRDetail,
// fetchProviderMRDetail, syncMRForRepo), and closed-MR refresh
// (fetchAndUpdateClosed, fetchAndUpdateClosedMergeRequest) all reach it, so
// a fork retarget discovered through any of those paths reclassifies a
// tracking workspace exactly like a plain list sync does.
func TestCommitMergeRequestParentSnapshotReclassifiesWorkspaceHeadRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	stored := &db.MergeRequest{
		RepoID: repoID, PlatformID: 1001, PlatformExternalID: "mr-1", Number: 1,
		Title: "Same-repo PR", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main", PlatformHeadSHA: "abc123",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, stored)
	require.NoError(err)

	ws := &db.Workspace{
		ID:           "ws-choke-point-reclassify",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "owner",
		RepoName:     "repo",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "abc123",
		MRHeadRepo:   nil, // classified same-repo when the workspace was created
		WorktreePath: t.TempDir(),
		TmuxSession:  "session",
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	updated := *stored
	updated.UpdatedAt = now.Add(time.Minute)
	updated.LastActivityAt = updated.UpdatedAt
	updated.HeadRepoCloneURL = "https://github.com/forker/repo.git"

	syncer := &Syncer{db: d}
	_, _, accepted, err := syncer.CommitMergeRequestParentSnapshot(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, &updated,
	)
	require.NoError(err)
	require.True(accepted)

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal("https://github.com/forker/repo.git", *reclassified.MRHeadRepo)
}

func TestCommitMergeRequestParentSnapshotKeepsHistoryBoundDuringReconciliation(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	sourceID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "old-group",
		Name:           "old-name",
	})
	require.NoError(err)
	destinationID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "new-group",
		Name:         "new-name",
	})
	require.NoError(err)

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	stored := &db.MergeRequest{
		RepoID: sourceID, PlatformID: 1001, PlatformExternalID: "mr-1", Number: 1,
		Title: "Same-repo MR", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main",
		HeadRepoCloneURL: "https://gitlab.com/old-group/old-name.git",
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, stored)
	require.NoError(err)

	ws := &db.Workspace{
		ID:           "ws-parent-snapshot-repository-reconciliation",
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		RepoOwner:    "old-group",
		RepoName:     "old-name",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   stored.Number,
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	writerQueued := make(chan struct{})
	restoreWriteLockHook := d.SetBeforeRepositoryReconciliationWriteLockForTest(
		func() { close(writerQueued) },
	)
	defer restoreWriteLockHook()

	reconciliationDone := make(chan error, 1)
	syncer := &Syncer{db: d}
	syncer.afterMergeRequestParentSnapshotCommit = func() {
		go func() {
			_, _, reconcileErr := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
				Platform:       "gitlab",
				PlatformHost:   "gitlab.com",
				PlatformRepoID: "gid://gitlab/Project/42",
				Owner:          "new-group",
				Name:           "new-name",
			}, time.Now().UTC())
			reconciliationDone <- reconcileErr
		}()
		<-writerQueued
	}

	forkURL := "https://gitlab.com/forker/repo.git"
	updated := *stored
	updated.HeadRepoCloneURL = forkURL
	updated.UpdatedAt = now.Add(time.Minute)
	updated.LastActivityAt = updated.UpdatedAt
	_, _, accepted, err := syncer.CommitMergeRequestParentSnapshot(
		ctx,
		RepoRef{
			Platform: platform.KindGitLab, PlatformHost: "gitlab.com",
			Owner: "old-group", Name: "old-name",
		},
		&updated,
	)
	require.NoError(err)
	require.True(accepted)
	require.NoError(<-reconciliationDone)

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	assert.Equal("new-group", reclassified.RepoOwner)
	assert.Equal("new-name", reclassified.RepoName)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal(forkURL, *reclassified.MRHeadRepo)

	moved, err := d.GetMergeRequestByRepoIDAndNumber(ctx, destinationID, stored.Number)
	require.NoError(err)
	assert.Nil(moved)
	preserved, err := d.GetMergeRequestByRepoIDAndNumber(ctx, sourceID, stored.Number)
	require.NoError(err)
	require.NotNil(preserved)
	assert.Equal(forkURL, preserved.HeadRepoCloneURL)
}

func TestReclassifyWorkspaceHeadRepoTrustRetriesAfterRevisionChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	stored := &db.MergeRequest{
		RepoID: repoID, PlatformID: 1001, PlatformExternalID: "mr-1", Number: 1,
		Title: "Same-repo PR", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main",
		HeadRepoCloneURL: "https://github.com/owner/repo.git",
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, stored)
	require.NoError(err)

	ws := &db.Workspace{
		ID:           "ws-reclassify-revision-race",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "owner",
		RepoName:     "repo",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
	}
	syncer := &Syncer{db: d}
	var advanced bool
	syncer.afterHeadRepoSnapshotRead = func() {
		if advanced {
			return
		}
		advanced = true
		updated := *stored
		updated.HeadRepoCloneURL = "https://github.com/forker/repo.git"
		updated.UpdatedAt = now.Add(time.Minute)
		updated.LastActivityAt = updated.UpdatedAt
		_, accepted, err := d.UpsertMergeRequestSnapshot(ctx, &updated)
		require.NoError(err)
		require.True(accepted)
	}

	syncer.reclassifyWorkspaceHeadRepoTrust(ctx, repo, repoID, stored.Number)

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal("https://github.com/forker/repo.git", *reclassified.MRHeadRepo)
}

func TestReclassifyWorkspaceHeadRepoTrustIgnoresAssociatedWorkspace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 1002, PlatformExternalID: "mr-1", Number: 1,
		Title: "Fork PR", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main",
		HeadRepoCloneURL: "https://github.com/new-fork/repo.git",
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)

	associatedPR := 1
	oldHeadRepo := "https://github.com/old-fork/repo.git"
	ws := &db.Workspace{
		ID:                 "ws-associated-reclassification-guard",
		Platform:           "github",
		PlatformHost:       "github.com",
		RepoOwner:          "owner",
		RepoName:           "repo",
		ItemType:           db.WorkspaceItemTypeAdHoc,
		ItemKey:            db.AdHocWorkspaceItemKey("feature"),
		AssociatedPRNumber: &associatedPR,
		MRHeadRepo:         &oldHeadRepo,
		WorkspaceBranch:    "feature",
		WorktreePath:       t.TempDir(),
		Status:             "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	syncer := &Syncer{db: d}
	syncer.reclassifyWorkspaceHeadRepoTrust(ctx, RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}, repoID, associatedPR)

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.NotNil(stored.MRHeadRepo)
	assert.Equal(oldHeadRepo, *stored.MRHeadRepo)
}

func TestReclassifyWorkspaceHeadRepoTrustKeepsHistoryBoundDuringReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	sourceID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "old-group",
		Name:           "old-name",
	})
	require.NoError(err)
	destinationID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "new-group",
		Name:         "new-name",
	})
	require.NoError(err)

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	forkURL := "https://gitlab.com/forker/repo.git"
	stored := &db.MergeRequest{
		RepoID: sourceID, PlatformID: 1001, PlatformExternalID: "mr-1", Number: 1,
		Title: "Fork MR", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main",
		HeadRepoCloneURL: forkURL,
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	}
	_, err = d.UpsertMergeRequest(ctx, stored)
	require.NoError(err)

	ws := &db.Workspace{
		ID:           "ws-reclassify-repository-reconciliation",
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		RepoOwner:    "old-group",
		RepoName:     "old-name",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   stored.Number,
		MRHeadRepo:   &forkURL,
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	repo := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.com",
		Owner: "old-group", Name: "old-name",
	}
	syncer := &Syncer{db: d}
	snapshotRead := make(chan struct{})
	continueReclassification := make(chan struct{})
	writeLockAttempted := make(chan struct{})
	restoreWriteLockHook := d.SetBeforeRepositoryReconciliationWriteLockForTest(
		func() { close(writeLockAttempted) },
	)
	defer restoreWriteLockHook()
	var snapshotHook sync.Once
	syncer.afterHeadRepoSnapshotRead = func() {
		snapshotHook.Do(func() {
			close(snapshotRead)
			<-continueReclassification
		})
	}

	reclassificationDone := make(chan struct{})
	go func() {
		syncer.reclassifyWorkspaceHeadRepoTrust(
			ctx, repo, sourceID, stored.Number,
		)
		close(reclassificationDone)
	}()
	<-snapshotRead

	reconciliationDone := make(chan error, 1)
	go func() {
		_, _, reconcileErr := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
			Platform:       "gitlab",
			PlatformHost:   "gitlab.com",
			PlatformRepoID: "gid://gitlab/Project/42",
			Owner:          "new-group",
			Name:           "new-name",
		}, time.Now().UTC())
		reconciliationDone <- reconcileErr
	}()
	<-writeLockAttempted
	select {
	case reconcileErr := <-reconciliationDone:
		require.NoError(reconcileErr)
		require.Fail("repository reconciliation bypassed the active read barrier")
	default:
	}
	close(continueReclassification)

	select {
	case <-reclassificationDone:
	case <-time.After(5 * time.Second):
		require.Fail("head-repository reclassification did not finish")
	}
	select {
	case upsertErr := <-reconciliationDone:
		require.NoError(upsertErr)
	case <-time.After(5 * time.Second):
		require.Fail("repository reconciliation did not resume after reclassification")
	}

	reclassified, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(reclassified)
	assert.Equal("new-group", reclassified.RepoOwner)
	assert.Equal("new-name", reclassified.RepoName)
	require.NotNil(reclassified.MRHeadRepo)
	assert.Equal(forkURL, *reclassified.MRHeadRepo)

	moved, err := d.GetMergeRequestByRepoIDAndNumber(ctx, destinationID, stored.Number)
	require.NoError(err)
	assert.Nil(moved)
	preserved, err := d.GetMergeRequestByRepoIDAndNumber(ctx, sourceID, stored.Number)
	require.NoError(err)
	require.NotNil(preserved)
	assert.Equal("Fork MR", preserved.Title)
}

func TestPreserveMergeableStateSkipsChangedOrUnknownHeadOrBase(t *testing.T) {
	assert := assert.New(t)
	tests := []struct {
		name       string
		normalized db.MergeRequest
		existing   db.MergeRequest
	}{
		{
			name:       "head changed",
			normalized: db.MergeRequest{PlatformHeadSHA: "new-head"},
			existing:   db.MergeRequest{PlatformHeadSHA: "old-head", MergeableState: "dirty"},
		},
		{
			name:       "base changed",
			normalized: db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "new-base"},
			existing:   db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "old-base", MergeableState: "dirty"},
		},
		{
			name:       "refreshed head missing",
			normalized: db.MergeRequest{PlatformBaseSHA: "same-base"},
			existing:   db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "same-base", MergeableState: "dirty"},
		},
		{
			name:       "existing head missing",
			normalized: db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "same-base"},
			existing:   db.MergeRequest{PlatformBaseSHA: "same-base", MergeableState: "dirty"},
		},
		{
			name:       "refreshed base missing",
			normalized: db.MergeRequest{PlatformHeadSHA: "same-head"},
			existing:   db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "same-base", MergeableState: "dirty"},
		},
		{
			name:       "existing base missing",
			normalized: db.MergeRequest{PlatformHeadSHA: "same-head", PlatformBaseSHA: "same-base"},
			existing:   db.MergeRequest{PlatformHeadSHA: "same-head", MergeableState: "dirty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveMergeableStateIfOmitted(&tt.normalized, &tt.existing)
			assert.Empty(tt.normalized.MergeableState)
		})
	}
}

func TestPreserveCIStateSkipsChangedOrUnknownHead(t *testing.T) {
	assert := assert.New(t)
	tests := []struct {
		name       string
		normalized db.MergeRequest
		existing   db.MergeRequest
	}{
		{
			name:       "head changed",
			normalized: db.MergeRequest{PlatformHeadSHA: "new-head"},
			existing:   db.MergeRequest{PlatformHeadSHA: "old-head", CIStatus: "success", CIChecksJSON: `[{"name":"build"}]`},
		},
		{
			name:       "refreshed head missing",
			normalized: db.MergeRequest{},
			existing:   db.MergeRequest{PlatformHeadSHA: "same-head", CIStatus: "success", CIChecksJSON: `[{"name":"build"}]`},
		},
		{
			name:       "existing head missing",
			normalized: db.MergeRequest{PlatformHeadSHA: "same-head"},
			existing:   db.MergeRequest{CIStatus: "success", CIChecksJSON: `[{"name":"build"}]`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveCIStateIfOmitted(&tt.normalized, &tt.existing)
			assert.Empty(tt.normalized.CIStatus)
			assert.Empty(tt.normalized.CIChecksJSON)
		})
	}
}

func TestPreserveCIStateKeepsOmittedStateForMatchingHead(t *testing.T) {
	assert := assert.New(t)
	normalized := db.MergeRequest{PlatformHeadSHA: "same-head"}
	existing := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		CIStatus:        "success",
		CIChecksJSON:    `[{"name":"build","status":"completed","conclusion":"success"}]`,
	}

	preserveCIStateIfOmitted(&normalized, &existing)

	assert.Equal("success", normalized.CIStatus)
	assert.Contains(normalized.CIChecksJSON, "build")
}

func TestPreserveReviewDecisionKeepsOmittedDecisionForMatchingHead(t *testing.T) {
	assert := assert.New(t)
	normalized := db.MergeRequest{PlatformHeadSHA: "same-head"}
	existing := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		ReviewDecision:  "CHANGES_REQUESTED",
	}

	preserveReviewDecisionIfOmitted(&normalized, &existing)

	assert.Equal("CHANGES_REQUESTED", normalized.ReviewDecision)
}

func TestPreserveReviewDecisionSkipsChangedHead(t *testing.T) {
	assert := assert.New(t)
	normalized := db.MergeRequest{PlatformHeadSHA: "new-head"}
	existing := db.MergeRequest{
		PlatformHeadSHA: "old-head",
		ReviewDecision:  "APPROVED",
	}

	preserveReviewDecisionIfOmitted(&normalized, &existing)

	assert.Empty(normalized.ReviewDecision)
}

func TestPreserveCIStateClearsCachedChecksWhenStatusChanges(t *testing.T) {
	assert := assert.New(t)
	normalized := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		CIStatus:        "success",
	}
	existing := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		CIStatus:        "failure",
		CIChecksJSON:    `[{"name":"build","status":"completed","conclusion":"failure"}]`,
	}

	needsCIDetailRefresh := preserveCIStateIfOmitted(&normalized, &existing)

	assert.True(needsCIDetailRefresh)
	assert.Equal("success", normalized.CIStatus)
	assert.Empty(normalized.CIChecksJSON)
}

func TestPreserveMergeableStateKeepsOmittedStateForMatchingKnownIdentity(t *testing.T) {
	assert := assert.New(t)
	normalized := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		PlatformBaseSHA: "same-base",
	}
	existing := db.MergeRequest{
		PlatformHeadSHA: "same-head",
		PlatformBaseSHA: "same-base",
		MergeableState:  "dirty",
	}

	preserveMergeableStateIfOmitted(&normalized, &existing)

	assert.Equal("dirty", normalized.MergeableState)
}

func TestSyncTriggersFullFetchForUnknownMergeableState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	// Build a list PR with diff stats set so the zero-stats condition
	// doesn't trigger the full fetch independently.
	listPR := buildOpenPR(1, now)
	additions := 10
	deletions := 5
	listPR.Additions = &additions
	listPR.Deletions = &deletions

	// First full-fetch returns "unknown".
	fetchCount := 0
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{listPR},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	mc.getPullRequestFn = func(_ context.Context, _, _ string, _ int) (*gh.PullRequest, error) {
		fetchCount++
		p := buildOpenPR(1, now)
		a, d2 := 10, 5
		p.Additions = &a
		p.Deletions = &d2
		state := "unknown"
		if fetchCount >= 2 {
			state = "clean"
		}
		p.MergeableState = &state
		return p, nil
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))

	// First sync: index scan upserts list data, detail drain fetches
	// full PR (returns "unknown" MergeableState).
	syncer.RunOnce(ctx)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("unknown", stored.MergeableState)
	assert.Equal(1, fetchCount, "first sync should trigger one full fetch via detail drain")
}

func TestSyncPreservesFieldsOnFullFetchFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	// First sync: full fetch succeeds, sets fields.
	pr := buildOpenPR(1, now)
	additions := 10
	deletions := 5
	baseSHA := "base123"
	mergeableState := "dirty"
	pr.Additions = &additions
	pr.Deletions = &deletions
	pr.MergeableState = &mergeableState
	pr.Base.SHA = &baseSHA

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{pr},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.Equal("dirty", stored.MergeableState)
	require.Equal(10, stored.Additions)

	// Second sync: bump UpdatedAt so needsTimeline triggers, but full
	// fetch fails. Fields from the existing row should be preserved.
	later := now.Add(time.Hour)
	listPR := buildOpenPR(1, later)
	listPR.Base.SHA = &baseSHA
	mc.openPRs = []*gh.PullRequest{listPR}
	mc.getPullRequestFn = func(_ context.Context, _, _ string, _ int) (*gh.PullRequest, error) {
		return nil, fmt.Errorf("transient network error")
	}

	syncer.RunOnce(ctx)

	stored2, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	assert.Equal("dirty", stored2.MergeableState, "MergeableState should survive a failed full fetch")
	assert.Equal(10, stored2.Additions, "Additions should survive a failed full fetch")
	assert.Equal(5, stored2.Deletions, "Deletions should survive a failed full fetch")
}

func TestSyncStatusUpdated(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))

	before := time.Now()
	syncer.RunOnce(t.Context())
	after := time.Now()

	status := syncer.Status()
	assert.False(status.Running)
	assert.False(status.LastRunAt.IsZero())
	assert.Condition(func() bool {
		return !status.LastRunAt.Before(before) && !status.LastRunAt.After(after)
	}, "status.LastRunAt %v should be between %v and %v", status.LastRunAt, before, after)
	assert.Empty(status.LastError)
}

func TestRunOncePreservesLocalCeilingStatusAcrossLaterRepoFailure(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	ceilingRecorded := make(chan struct{})

	client := &mockClient{
		listOpenPRsFn: func(ctx context.Context, _, repo string) ([]*gh.PullRequest, error) {
			if repo == "budget-limited" {
				return nil, fmt.Errorf(
					"budget-limited list failure: %w", platform.ErrSyncBudgetExhausted,
				)
			}
			select {
			case <-ceilingRecorded:
				return nil, errors.New("unrelated repository failure")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{
			{Owner: "owner", Name: "budget-limited", PlatformHost: "github.com"},
			{Owner: "owner", Name: "other", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)
	syncer.SetParallelism(2)
	var closeOnce sync.Once
	syncer.SetOnStatusChange(func(status *SyncStatus) {
		if status.Progress == "1/2" {
			closeOnce.Do(func() { close(ceilingRecorded) })
		}
	})

	syncer.RunOnce(t.Context())

	status := syncer.Status()
	assert.Contains(status.LastError, "budget-limited list failure")
	assert.NotContains(status.LastError, "unrelated repository failure")
	assert.Equal(SyncErrorCodeLocalCeilingExhausted, status.LastErrorCode)
	assert.Equal("github.com", status.LastErrorCeilingKey)
}

func TestRunOncePreservesItemCeilingStatusAcrossLaterHardRepoFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(35 * time.Minute)
	repo := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "repo-acme-widget",
	}
	repoID, err := database.UpsertRepo(t.Context(), verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), platform.DBMergeRequest(repoID, platform.MergeRequest{
		PlatformID:     700,
		Number:         7,
		URL:            "https://github.com/acme/widget/pull/7",
		Title:          "Previously open",
		State:          "open",
		HeadSHA:        "abc123",
		BaseSHA:        "def456",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}))
	require.NoError(err)

	provider := &syncTestBudgetThenBrokenIssueProvider{
		kind: platform.KindGitHub, host: "github.com",
		getMRErr: newSyncBudgetExhaustedError(resetAt),
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	syncer.RunOnce(t.Context())

	status := syncer.Status()
	assert.Contains(status.LastError, "one or more merge request sync items failed")
	assert.Contains(status.LastError, "resolve issue reader")
	assert.Equal(SyncErrorCodeLocalCeilingExhausted, status.LastErrorCode)
	assert.Equal("github.com", status.LastErrorCeilingKey)
	assert.Equal(resetAt.Format(time.RFC3339), status.LastErrorCeilingResetAt)
}

func TestSyncStatusUpdatedUsesUTC(t *testing.T) {
	d := openTestDB(t)
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(t.Context())

	status := syncer.Status()
	assert.Equal(t, time.UTC, status.LastRunAt.Location())
}

// syncedWriter wraps an io.Writer with a mutex for safe concurrent
// writes from multiple goroutines, used to capture slog output in
// tests where workers run in parallel.
type syncedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncedWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

func captureDefaultLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(
		&syncedWriter{w: &buf},
		&slog.HandlerOptions{Level: slog.LevelInfo},
	)))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// TestSyncMRReturnsErrorOnNilPullRequest verifies SyncMR returns
// a clear error when a Client returns (nil, nil) from
// GetPullRequest, instead of dereferencing nil in NormalizePR.
func TestSyncMRReturnsErrorOnNilPullRequest(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)

	mc := &mockClient{
		getPullRequestFn: func(_ context.Context, _, _ string, _ int) (*gh.PullRequest, error) {
			return nil, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	err := syncer.SyncMR(t.Context(), "owner", "repo", 1)
	require.Error(err)
	require.Contains(err.Error(), "nil pull request")
}

// TestRunOnceSyncOpenMRSurvivesNilFullPR verifies the periodic
// sync path does not panic when GetPullRequest returns (nil, nil)
// during syncOpenMR's full-PR fetch. It must fall back to the
// list-derived data and complete the sync.
func TestRunOnceSyncOpenMRSurvivesNilFullPR(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(7, now)},
		getPullRequestFn: func(_ context.Context, _, _ string, _ int) (*gh.PullRequest, error) {
			// Contract violation: return (nil, nil). Periodic
			// sync must not panic on this.
			return nil, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// Must complete without panic.
	syncer.RunOnce(ctx)

	// The list-derived PR should still be persisted because the
	// nil full-PR fetch is non-fatal.
	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(7, pr.Number)
}

// trackingClient records every ListOpenPullRequests invocation
// so a test can assert that runWorker did not start any work.
type trackingClient struct {
	mockClient
	listCalls atomic.Int32
}

func (c *trackingClient) ListOpenPullRequests(
	_ context.Context, _, _ string,
) ([]*gh.PullRequest, error) {
	c.listCalls.Add(1)
	return nil, nil
}

// TestRunWorkerBailsOnCanceledCtx verifies the worker-side ctx
// check fires before any work is done. The dispatch race fix
// pre-checks ctx before the select, but a cancel can still land
// in the micro-window between pre-check and send and Go may pick
// the send branch. The worker must then discard the repo before
// logging "syncing repo" or calling syncRepo.
//
// This test exercises that path directly: it pre-loads a buffered
// work channel, cancels ctx, and calls runWorker with the
// canceled ctx. With the worker-side check the function returns
// without invoking the client; without the check it would log
// "syncing repo" and increment the completed counter.
func TestRunWorkerBailsOnCanceledCtx(t *testing.T) {
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

	tc := &trackingClient{}
	syncer := NewSyncer(
		map[string]Client{"github.com": tc}, d, nil,
		[]RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// Pre-load three repos so the worker would naturally drain
	// all three if the ctx check were missing.
	work := make(chan repoWork, 3)
	for i := range 3 {
		work <- repoWork{
			index: i,
			repo: RepoRef{
				Owner:        "o",
				Name:         fmt.Sprintf("r%d", i),
				PlatformHost: "github.com",
			},
		}
	}
	close(work)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var (
		completed atomic.Int32
		maxShown  atomic.Int32
		errMu     sync.Mutex
		lastErr   string
		canceled  atomic.Bool
	)
	state := &runState{
		completed: &completed,
		maxShown:  &maxShown,
		errMu:     &errMu,
		lastErr:   &lastErr,
		canceled:  &canceled,
		total:     3,
		results:   make([]RepoSyncResult, 3),
	}
	syncer.runWorker(ctx, work, state)

	sw.mu.Lock()
	output := buf.String()
	sw.mu.Unlock()

	assert.Zero(strings.Count(output, `msg="syncing repo"`),
		"runWorker must not log 'syncing repo' when ctx is canceled")
	assert.Zero(int(completed.Load()),
		"runWorker must not increment completed when ctx is canceled")
	assert.Zero(int(tc.listCalls.Load()),
		"runWorker must not call the GitHub client when ctx is canceled")
	assert.Empty(lastErr, "runWorker must not record an error when bailing on ctx")
}

// dedupGetUserClient blocks on GetUser calls to force concurrent
// display-name lookups into a race. It also tracks how many
// GetUser calls actually hit it.
type dedupGetUserClient struct {
	mockClient
	getUserCount atomic.Int32
	block        chan struct{}
	listEntered  chan struct{}
	listRelease  chan struct{}
	userEntered  chan struct{}
	author       string
	now          time.Time
}

func (c *dedupGetUserClient) ListOpenPullRequests(
	_ context.Context, _, repo string,
) ([]*gh.PullRequest, error) {
	c.listEntered <- struct{}{}
	<-c.listRelease
	number := 1
	if repo == "r2" {
		number = 2
	}
	pr := buildOpenPR(number, c.now)
	pr.User = &gh.User{Login: &c.author}
	return []*gh.PullRequest{pr}, nil
}

func (c *dedupGetUserClient) GetUser(
	_ context.Context, login string,
) (*gh.User, error) {
	c.getUserCount.Add(1)
	c.userEntered <- struct{}{}
	<-c.block
	name := "Display " + login
	return &gh.User{Login: &login, Name: &name}, nil
}

func TestResolveDisplayNameDedupsConcurrentLookups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	author := "alice"
	now := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)

	mc := &dedupGetUserClient{
		block:       make(chan struct{}),
		listEntered: make(chan struct{}, 2),
		listRelease: make(chan struct{}),
		userEntered: make(chan struct{}, 1),
		author:      author,
		now:         now,
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{
			{Owner: "o", Name: "r1", PlatformHost: "github.com"},
			{Owner: "o", Name: "r2", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)
	syncer.SetParallelism(2)

	done := make(chan struct{})
	go func() {
		syncer.RunOnce(t.Context())
		close(done)
	}()

	// Hold both repository workers at the PR-list boundary. This keeps the
	// assertion focused on display-name coalescing instead of imposing a short
	// wall-clock deadline on the complete repository-sync startup path.
	startupDeadline := time.NewTimer(30 * time.Second)
	defer startupDeadline.Stop()
	for range 2 {
		select {
		case <-mc.listEntered:
		case <-startupDeadline.C:
			require.Fail("both workers did not reach pull request listing")
			return
		}
	}
	close(mc.listRelease)

	select {
	case <-mc.userEntered:
	case <-time.After(30 * time.Second):
		require.Fail("no worker reached GetUser")
	}

	// Give the second worker plenty of time to enter singleflight.
	time.Sleep(100 * time.Millisecond)

	close(mc.block)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		require.Fail("RunOnce did not complete")
	}

	assert.Equal(int32(1), mc.getUserCount.Load(),
		"concurrent display-name lookups for same author "+
			"should coalesce into one GetUser call")
}

func TestIsTrackedRepo(t *testing.T) {
	assert := assert.New(t)
	database := openTestDB(t)
	mc := &mockClient{}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		{Owner: "corp", Name: "lib", PlatformHost: "github.com"},
	}, time.Minute, nil, nil)

	assert.True(syncer.IsTrackedRepo("acme", "widget"))
	assert.True(syncer.IsTrackedRepo("Acme", "Widget"))
	assert.True(syncer.IsTrackedRepo("corp", "lib"))
	assert.False(syncer.IsTrackedRepo("acme", "other"))
	assert.False(syncer.IsTrackedRepo("nobody", "widget"))
}

func TestClientForRepoMatchesCaseInsensitively(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	mc := &mockClient{}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{
		{Owner: "Acme", Name: "Widget", PlatformHost: "github.com"},
	}, time.Minute, nil, nil)

	client, err := syncer.ClientForRepo("acme", "widget")
	require.NoError(err)
	require.Same(mc, client)
}

func TestDirectClientForHostReportsMissingProvider(t *testing.T) {
	require := require.New(t)
	syncer := NewSyncer(nil, openTestDB(t), nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)

	client, err := syncer.DirectClientForHost("github.com")
	require.Nil(client)
	require.ErrorIs(err, platform.ErrProviderNotConfigured)

	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.Equal(platform.ErrCodeProviderNotConfigured, platformErr.Code)
	require.Equal(platform.KindGitHub, platformErr.Provider)
	require.Equal("github.com", platformErr.PlatformHost)
}

func TestSyncerClientLookupReportsMissingProvider(t *testing.T) {
	require := require.New(t)
	syncer := NewSyncer(nil, openTestDB(t), nil, []RepoRef{{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	}}, time.Minute, nil, nil)

	_, err := syncer.mergeRequestReaderFor(RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	})

	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrProviderNotConfigured)
	require.Equal(platform.ErrCodeProviderNotConfigured, platformErr.Code)
	require.Equal(platform.KindGitLab, platformErr.Provider)
	require.Equal("gitlab.com", platformErr.PlatformHost)
}

func TestSyncerClientLookupReportsMissingOptionalReader(t *testing.T) {
	require := require.New(t)
	syncer := NewSyncer(nil, openTestDB(t), nil, []RepoRef{{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	}}, time.Minute, nil, nil)
	registry, err := platform.NewRegistry(syncTestProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
	})
	require.NoError(err)
	syncer.clients = registry

	_, err = syncer.mergeRequestReaderFor(RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	})

	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	require.Equal(platform.ErrCodeUnsupportedCapability, platformErr.Code)
	require.Equal("read_merge_requests", platformErr.Capability)
}

func TestSyncItemByNumber_Issue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	number := 42
	title := "Bug report"
	state := "closed"
	author := "testuser"
	now := time.Now()
	ghTime := &gh.Timestamp{Time: now}

	mc := &mockClient{
		getIssueFn: func(_ context.Context, _, _ string, n int) (*gh.Issue, error) {
			if n != number {
				return nil, fmt.Errorf("unexpected number %d", n)
			}
			return &gh.Issue{
				ID:        new(int64(9999)),
				Number:    &number,
				Title:     &title,
				State:     &state,
				User:      &gh.User{Login: &author},
				HTMLURL:   new("https://github.com/acme/widget/issues/42"),
				CreatedAt: ghTime,
				UpdatedAt: ghTime,
			}, nil
		},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
	}, time.Minute, nil, nil)

	itemType, err := syncer.SyncItemByNumber(ctx, "acme", "widget", number)
	require.NoError(err)
	assert.Equal("issue", itemType)

	issue, err := database.GetIssue(ctx, "github", "github.com", "acme", "widget", number)
	require.NoError(err)
	assert.NotNil(issue)
	assert.Equal(title, issue.Title)
}

func TestSyncItemByNumber_PR(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	number := 10
	title := "Add feature"
	state := "open"
	author := "testuser"
	now := time.Now()
	ghTime := &gh.Timestamp{Time: now}
	prURL := "https://github.com/acme/widget/pull/10"

	mc := &mockClient{
		getIssueFn: func(_ context.Context, _, _ string, n int) (*gh.Issue, error) {
			return &gh.Issue{
				ID:      new(int64(8888)),
				Number:  &number,
				Title:   &title,
				State:   &state,
				User:    &gh.User{Login: &author},
				HTMLURL: new(prURL),
				PullRequestLinks: &gh.PullRequestLinks{
					URL: &prURL,
				},
				CreatedAt: ghTime,
				UpdatedAt: ghTime,
			}, nil
		},
		singlePR: &gh.PullRequest{
			ID:      new(int64(8888)),
			Number:  &number,
			Title:   &title,
			State:   &state,
			User:    &gh.User{Login: &author},
			HTMLURL: &prURL,
			Head: &gh.PullRequestBranch{
				Ref: new("feature"),
				SHA: new("abc123"),
			},
			Base:      &gh.PullRequestBranch{Ref: new("main")},
			CreatedAt: ghTime,
			UpdatedAt: ghTime,
		},
	}

	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
	}, time.Minute, nil, nil)

	itemType, err := syncer.SyncItemByNumber(ctx, "acme", "widget", number)
	require.NoError(err)
	assert.Equal("pr", itemType)

	pr, err := database.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", number)
	require.NoError(err)
	assert.NotNil(pr)
	assert.Equal(title, pr.Title)
}

func TestRepoFailKeyIncludesProvider(t *testing.T) {
	assert := assert.New(t)
	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}

	assert.NotEqual(repoFailKey(githubRepo), repoFailKey(gitlabRepo))
	assert.Equal("github/code.example.com/acme/widget", repoFailKey(githubRepo))
	assert.Equal("gitlab/code.example.com/acme/widget", repoFailKey(gitlabRepo))
}

func TestPlatformRepoRefPreservesFullProviderRef(t *testing.T) {
	assert := assert.New(t)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "Group/SubGroup",
		Name:               "Project",
		RepoPath:           "Group/SubGroup/Project",
		PlatformRepoID:     42,
		PlatformExternalID: "gid://gitlab/Project/42",
		WebURL:             "https://gitlab.example.com/Group/SubGroup/Project",
		CloneURL:           "https://gitlab.example.com/Group/SubGroup/Project.git",
		DefaultBranch:      "main",
	}

	ref := platformRepoRef(repo)

	assert.Equal(platform.KindGitLab, ref.Platform)
	assert.Equal("gitlab.example.com", ref.Host)
	assert.Equal("Group/SubGroup", ref.Owner)
	assert.Equal("Project", ref.Name)
	assert.Equal("Group/SubGroup/Project", ref.RepoPath)
	assert.Equal(int64(42), ref.PlatformID)
	assert.Equal("gid://gitlab/Project/42", ref.PlatformExternalID)
	assert.Equal("https://gitlab.example.com/Group/SubGroup/Project", ref.WebURL)
	assert.Equal("https://gitlab.example.com/Group/SubGroup/Project.git", ref.CloneURL)
	assert.Equal("main", ref.DefaultBranch)
}

func TestCloneRemoteURLUsesProviderCloneURLAndRepoPath(t *testing.T) {
	assert := assert.New(t)

	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "Group/SubGroup",
		Name:         "Project",
		RepoPath:     "Group/SubGroup/Project",
		CloneURL:     "https://gitlab.example.com/Group/SubGroup/Project.git",
	}
	assert.Equal(
		"https://gitlab.example.com/Group/SubGroup/Project.git",
		cloneRemoteURL(gitlabRepo),
	)

	gitlabRepo.CloneURL = ""
	assert.Equal(
		"https://gitlab.example.com/Group/SubGroup/Project.git",
		cloneRemoteURL(gitlabRepo),
	)

	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	assert.Equal("https://github.com/acme/widget.git", cloneRemoteURL(githubRepo))
}

func TestFetcherForSkipsNonGitHubRepoOnSameHost(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	fetcher := NewGraphQLFetcher(testTokenSource("token"), "code.example.com", nil, nil)
	syncer := NewSyncer(nil, d, nil, nil, time.Minute, nil, nil)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"code.example.com": fetcher,
	})

	assert.Same(fetcher, syncer.fetcherFor(RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}))
	assert.Nil(syncer.fetcherFor(RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}))
}

func TestSyncRepoUsesProviderIDToPreserveRenamedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	originalID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "old-group",
		Name:           "old-project",
		RepoPath:       "old-group/old-project",
	})
	require.NoError(err)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "new-group",
		Name:               "new-project",
		RepoPath:           "new-group/new-project",
		PlatformExternalID: "gid://gitlab/Project/42",
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.example.com",
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	require.NoError(syncer.syncRepo(ctx, repo))

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal(originalID, repos[0].ID)
	assert.Equal("new-group", repos[0].Owner)
	assert.Equal("new-project", repos[0].Name)
	assert.Equal("new-group/new-project", repos[0].RepoPath)
	catalog, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/42",
	)
	require.NoError(err)
	require.NotNil(catalog)
	require.Len(catalog.Routes, 2)
	assert.False(catalog.Routes[0].Current)
	assert.True(catalog.Routes[1].Current)
}

func TestSyncRepoPublishesProviderResolvedRenameForBackgroundAndTargetedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	originalID, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "old-group",
		Name:           "old-project",
		RepoPath:       "old-group/old-project",
	})
	require.NoError(err)
	configured := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "old-group",
		Name:               "old-project",
		RepoPath:           "old-group/old-project",
		PlatformExternalID: "gid://gitlab/Project/42",
		RepoID:             originalID,
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind:            platform.KindGitLab,
			host:            "gitlab.example.com",
			requireRepoPath: "new-group/new-project",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab,
				Host:     "gitlab.example.com",
				Owner:    "new-group",
				Name:     "new-project",
				RepoPath: "new-group/new-project",
			},
			PlatformExternalID: "gid://gitlab/Project/42",
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{configured}, time.Minute, nil, nil,
	)

	require.NoError(syncer.syncRepo(ctx, configured))
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Equal("new-group", tracked[0].Owner)
	assert.Equal("new-project", tracked[0].Name)
	assert.Equal("new-group/new-project", tracked[0].RepoPath)
	assert.Equal(originalID, tracked[0].RepoID)
	require.NoError(syncer.SyncRepoOnProvider(
		ctx, platform.KindGitLab, "gitlab.example.com", "new-group", "new-project",
	))
}

func TestConcurrentSyncRepoKeepsNewerProviderObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	oldLookupStarted := make(chan struct{})
	releaseOldLookup := make(chan struct{})
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "gitlab.example.com",
		},
		getRepositoryFn: func(
			_ context.Context, ref platform.RepoRef,
		) (platform.Repository, error) {
			if ref.RepoPath == "group/old-project" {
				close(oldLookupStarted)
				<-releaseOldLookup
			}
			return platform.Repository{
				Ref: ref, PlatformExternalID: "gid://gitlab/Project/42",
				CloneURL: "https://gitlab.example.com/" + ref.RepoPath + ".git",
			}, nil
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	oldRef := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.example.com",
		Owner: "group", Name: "old-project", RepoPath: "group/old-project",
	}
	newRef := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.example.com",
		Owner: "group", Name: "new-project", RepoPath: "group/new-project",
	}
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{oldRef, newRef}, time.Minute, nil, nil,
	)

	oldDone := make(chan error, 1)
	go func() { oldDone <- syncer.syncRepo(ctx, oldRef) }()
	<-oldLookupStarted
	require.NoError(syncer.syncRepo(ctx, newRef))
	close(releaseOldLookup)
	require.NoError(<-oldDone)

	entry, err := database.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/42",
	)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal("group/new-project", entry.Repository.RepoPath)
	assert.Equal(
		"https://gitlab.example.com/group/new-project.git",
		entry.Repository.CloneURL,
	)
}

func TestSyncRepoKeepsHistoryWhenNewIDReusesRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	oldID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/old",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	})
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID: oldID, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1,
		Title: "historical issue", State: "open",
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		LastActivityAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)

	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformExternalID: "gid://gitlab/Project/old",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "gitlab.example.com",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab,
				Host:     "gitlab.example.com",
				Owner:    "group",
				Name:     "project",
				RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/new",
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.syncRepo(ctx, repo))
	assert.Equal(int32(1), provider.getRepositoryCalls.Load())
	newEntry, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/new",
	)
	require.NoError(err)
	require.NotNil(newEntry)
	assert.NotEqual(oldID, newEntry.Repository.ID)
	assert.Equal(db.RepositoryLifecycleActive, newEntry.Lifecycle)
	oldEntry, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/old",
	)
	require.NoError(err)
	require.NotNil(oldEntry)
	assert.Equal(db.RepositoryLifecycleInactive, oldEntry.Lifecycle)
	var issueRepoID int64
	require.NoError(d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_issues WHERE number = 1`,
	).Scan(&issueRepoID))
	assert.Equal(oldID, issueRepoID)
}

func TestSyncRepoUpdatesViewerCanMergeWithoutMergeSettings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	when := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	entry, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gitlab-project",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	}, when)
	require.NoError(err)
	require.True(accepted)
	repoID := entry.Repository.ID
	require.NoError(d.UpdateRepoSettings(ctx, repoID, true, false, true, true))
	syncer := &Syncer{db: d}
	viewerCanMerge := false

	applied, err := syncer.updateRepoSettingsFromProviderObservation(
		ctx, repoID, when, platform.Repository{
			PlatformExternalID: "gitlab-project",
			ViewerCanMerge:     &viewerCanMerge,
		},
	)
	require.NoError(err)
	require.True(applied)

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.True(repos[0].AllowSquashMerge)
	assert.False(repos[0].AllowMergeCommit)
	assert.True(repos[0].AllowRebaseMerge)
	assert.False(repos[0].ViewerCanMerge)
}

func TestSyncRepoPersistsGitHubProviderMetadataWhenIdentityPrefilled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	// Repo resolution (glob listing and explicit lookup) pre-fills the
	// platform repo id, so syncRepoIdentity never resolves the repository
	// itself and the GitHub settings-refresh branch is the row's only
	// metadata writer. It must persist provider metadata from its own
	// settings fetch, or the row keeps an empty default branch forever and
	// the worktree diff sampler degrades to a bare HEAD diff.
	repo := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "widgets",
		RepoPath:           "acme/widgets",
		PlatformExternalID: "R_kgDOexample",
	}
	client := &mockClient{getRepositoryFn: func(
		context.Context,
		string,
		string,
	) (*gh.Repository, error) {
		return &gh.Repository{
			Name:          new("widgets"),
			NodeID:        new("R_kgDOexample"),
			HTMLURL:       new("https://github.com/acme/widgets"),
			CloneURL:      new("https://github.com/acme/widgets.git"),
			DefaultBranch: new("main"),
		}, nil
	}}
	syncer := NewSyncer(map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{repo}, time.Minute, nil, nil)

	require.NoError(syncer.syncRepo(ctx, repo))

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal("main", repos[0].DefaultBranch)
	assert.Equal("https://github.com/acme/widgets", repos[0].WebURL)
	assert.Equal("https://github.com/acme/widgets.git", repos[0].CloneURL)
	assert.Equal("R_kgDOexample", repos[0].PlatformRepoID)
}

func TestRefreshRepoSettingsPreservesViewerCanMergeWhenGitHubOmitsPermissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	identity := db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-widgets",
		Owner:          "acme",
		Name:           "widgets",
		RepoPath:       "acme/widgets",
	}
	observedAt := time.Now().UTC().Add(-time.Minute)
	entry, accepted, err := d.ReconcileRepositoryObservation(ctx, identity, observedAt)
	require.NoError(err)
	require.True(accepted)
	repoID := entry.Repository.ID
	require.NoError(d.UpdateRepoSettings(ctx, repoID, true, true, true, false))
	client := &mockClient{getRepositoryFn: func(
		context.Context,
		string,
		string,
	) (*gh.Repository, error) {
		return &gh.Repository{
			Name:             new("widgets"),
			NodeID:           new("repo-acme-widgets"),
			AllowSquashMerge: new(false),
			AllowMergeCommit: new(true),
			AllowRebaseMerge: new(false),
		}, nil
	}}
	syncer := NewSyncer(map[string]Client{"github.com": client}, d, nil, []RepoRef{{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widgets",
		RepoPath:     "acme/widgets",
	}}, time.Minute, nil, nil)
	routeFence, found, err := d.CurrentRepositoryRouteFence(
		ctx, identity, repoID,
	)
	require.NoError(err)
	require.True(found)

	require.NoError(syncer.refreshRepoSettings(
		ctx, syncer.repos[0], repoID, nil, observedAt, routeFence,
	))

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.False(repos[0].AllowSquashMerge)
	assert.True(repos[0].AllowMergeCommit)
	assert.False(repos[0].AllowRebaseMerge)
	assert.False(repos[0].ViewerCanMerge)
}

func TestSyncRepoPreservesViewerCanMergeWhenMergeSettingsOmitPermission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	when := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	entry, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gitlab-project",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	}, when)
	require.NoError(err)
	require.True(accepted)
	repoID := entry.Repository.ID
	require.NoError(d.UpdateRepoSettings(ctx, repoID, true, true, true, false))
	syncer := &Syncer{db: d}

	applied, err := syncer.updateRepoSettingsFromProviderObservation(
		ctx, repoID, when, platform.Repository{
			PlatformExternalID: "gitlab-project",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: false,
				AllowMergeCommit: true,
				AllowRebaseMerge: false,
			},
		},
	)
	require.NoError(err)
	require.True(applied)

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.False(repos[0].AllowSquashMerge)
	assert.True(repos[0].AllowMergeCommit)
	assert.False(repos[0].AllowRebaseMerge)
	assert.False(repos[0].ViewerCanMerge)
}

func TestSyncRepoRefreshesProviderRepoSettingsWhenIdentityKnown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	})
	require.NoError(err)
	require.NoError(d.UpdateRepoSettings(ctx, repoID, true, true, true, true))
	viewerCanMerge := false
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "gitlab.example.com",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.example.com",
				Owner: "group", Name: "project", RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/42",
			ViewerCanMerge:     &viewerCanMerge,
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, d, nil, []RepoRef{{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformExternalID: "gid://gitlab/Project/42",
	}}, time.Minute, nil, nil)

	require.NoError(syncer.syncRepo(ctx, syncer.repos[0]))

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal(int32(1), provider.getRepositoryCalls.Load())
	assert.False(repos[0].ViewerCanMerge)
}

func TestSyncRepoUsesProviderCloneURLForNestedGitLabRepo(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	remote := setupBareRemoteForSyncTest(t)
	clones := gitclone.New(t.TempDir(), nil)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.example.com",
		Owner:              "group/subgroup",
		Name:               "project",
		RepoPath:           "group/subgroup/project",
		PlatformExternalID: "gid://gitlab/Project/43",
		CloneURL:           remote,
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.example.com",
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, d, clones, []RepoRef{repo}, time.Minute, nil, nil)

	require.NoError(syncer.syncRepo(ctx, repo))
	clonePath, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(ctx, repo.PlatformExternalID),
		"gitlab", "gitlab.example.com", "group/subgroup", "project",
	)
	require.NoError(err)
	require.FileExists(filepath.Join(clonePath, "HEAD"))
}

func TestDetailDrainUsesProviderCloneURLForNestedGitLabRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	remote := setupBareRemoteForSyncTest(t)
	clones := gitclone.New(t.TempDir(), nil)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group/subgroup",
		Name:         "project",
		RepoPath:     "group/subgroup/project",
		CloneURL:     remote,
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.example.com/group/subgroup/project/-/merge_requests/7",
		Title:          "stale MR",
		Author:         "ada",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "gitlab.example.com",
			mergeRequests: []platform.MergeRequest{{
				Repo:           platformRepoRef(repo),
				PlatformID:     1001,
				Number:         7,
				URL:            "https://gitlab.example.com/group/subgroup/project/-/merge_requests/7",
				Title:          "fresh MR",
				Author:         "ada",
				State:          "open",
				HeadBranch:     "feature",
				BaseBranch:     "main",
				CreatedAt:      now,
				UpdatedAt:      now,
				LastActivityAt: now,
			}},
		},
		repository: platform.Repository{
			Ref:                platformRepoRef(repo),
			PlatformExternalID: verifiedDBRepoIdentity(platformRepoRef(repo)).PlatformRepoID,
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	rateKey := RateBucketKey("gitlab", "gitlab.example.com", "host")
	syncer := NewSyncerWithRegistry(registry, d, clones, []RepoRef{repo}, time.Minute, nil, map[string]*SyncBudget{
		rateKey: NewSyncBudget(100),
	})

	syncer.drainDetailQueue(ctx, map[string]bool{rateKey: true}, syncer.TrackedRepos())

	assert.Equal(int32(1), provider.getRepositoryCalls.Load())
	assert.Equal(int32(1), provider.getMRCalls.Load())
	clonePath, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(
			ctx, verifiedDBRepoIdentity(platformRepoRef(repo)).PlatformRepoID,
		),
		"gitlab", "gitlab.example.com", "group/subgroup", "project",
	)
	require.NoError(err)
	require.FileExists(filepath.Join(clonePath, "HEAD"))
}

func TestDetailDrainCompletesWhenProviderExceedsAdmittedCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitea,
		PlatformHost: "gitea.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := database.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitea.example.com/acme/widget/pulls/7",
		Title:          "cached MR",
		Author:         "ada",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now.Add(-time.Hour),
		UpdatedAt:      now.Add(-time.Hour),
		LastActivityAt: now.Add(-time.Hour),
	})
	require.NoError(err)
	require.NoError(database.UpsertMRReviewThreads(ctx, mrID, []db.MRReviewThread{{
		ProviderThreadID: "cached-thread",
		Body:             "cached review note",
		CreatedAt:        now.Add(-time.Hour),
		UpdatedAt:        now.Add(-time.Hour),
	}}))

	var wireAttempts atomic.Int32
	provider := &syncTestReadProvider{
		kind: platform.KindGitea, host: "gitea.example.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     1001,
			Number:         7,
			URL:            "https://gitea.example.com/acme/widget/pulls/7",
			Title:          "fresh MR",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		readReviewThreads: true,
		listReviewThreadsFn: func(ctx context.Context, _ platform.RepoRef, _ int) ([]platform.MergeRequestReviewThread, error) {
			for range PRDetailWorstCase + 1 {
				if !ConsumeArchiveAttemptAllowance(ctx) {
					return nil, platform.ErrArchiveAttemptBudget
				}
				wireAttempts.Add(1)
			}
			return []platform.MergeRequestReviewThread{{
				ProviderThreadID: "fresh-thread",
				Body:             "fresh review note",
				CreatedAt:        now,
				UpdatedAt:        now,
			}}, nil
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	rateKey := RateBucketKey("gitea", "gitea.example.com", "host")
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{repo}, time.Minute, nil,
		map[string]*SyncBudget{rateKey: NewSyncBudget(100)},
	)

	syncer.drainDetailQueue(ctx, map[string]bool{rateKey: true}, syncer.TrackedRepos())

	assert.Equal(int32(PRDetailWorstCase+1), wireAttempts.Load())
	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	assert.NotNil(stored.DetailFetchedAt)
	threads, err := database.ListMRReviewThreads(ctx, mrID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("fresh-thread", threads[0].ProviderThreadID)
	assert.Equal("fresh review note", threads[0].Body)
}

func TestSyncMRUsesConfiguredProviderRegistry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo: platform.RepoRef{
				Platform: platform.KindGitLab,
				Host:     "gitlab.com",
				Owner:    "acme",
				Name:     "widget",
				RepoPath: "acme/widget",
			},
			PlatformID: 42,
			Number:     10,
			Title:      "gitlab mr",
			State:      "open",
			Author:     "author",
			CreatedAt:  time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
			UpdatedAt:  time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]RepoRef{{
			Platform:     platform.KindGitLab,
			PlatformHost: "gitlab.com",
			Owner:        "acme",
			Name:         "widget",
		}},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.SyncMR(ctx, "acme", "widget", 10))

	mr, err := database.GetMergeRequest(ctx, "gitlab", "gitlab.com", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("gitlab mr", mr.Title)
	assert.Equal(int32(1), provider.getMRCalls.Load())
}

func TestSyncItemByNumberRejectsNonGitHubProviderWithoutForcingGitHub(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]RepoRef{{
			Platform:     platform.KindGitLab,
			PlatformHost: "gitlab.com",
			Owner:        "acme",
			Name:         "widget",
		}},
		time.Minute,
		nil,
		nil,
	)

	_, err = syncer.SyncItemByNumber(ctx, "acme", "widget", 10)

	require.Error(err)
	assert.Contains(err.Error(), "requires an item type")
	assert.Equal(int32(0), provider.getIssueCalls.Load())
	assert.Equal(int32(0), provider.getMRCalls.Load())
}

func TestSyncMRRejectsAmbiguousProviderIdentity(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	registry, err := platform.NewRegistry(
		&syncTestReadProvider{
			kind: platform.KindGitLab,
			host: "code.example.com",
		},
	)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]RepoRef{
			{
				Platform:     platform.KindGitHub,
				PlatformHost: "code.example.com",
				Owner:        "acme",
				Name:         "widget",
			},
			{
				Platform:     platform.KindGitLab,
				PlatformHost: "code.example.com",
				Owner:        "acme",
				Name:         "widget",
			},
		},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.SyncMR(ctx, "acme", "widget", 10)

	require.Error(err)
	require.Contains(err.Error(), "ambiguous")
}

func TestIndexUpsertMRReadsExistingByRepoID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(gitlabRepo)))
	require.NoError(err)

	detailFetchedAt := now.Add(-time.Minute)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          gitlabRepoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://code.example.com/acme/widget/-/merge_requests/7",
		Title:           "gitlab MR",
		Author:          "ada",
		State:           "open",
		Additions:       123,
		Deletions:       45,
		MergeableState:  "checking",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "gitlab-head",
		PlatformBaseSHA: "base",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, []RepoRef{githubRepo, gitlabRepo}, time.Minute, nil, nil)
	require.NoError(syncer.indexUpsertMR(ctx, &mockClient{}, githubRepo, githubRepoID, buildOpenPR(7, now)))

	githubMR, err := d.GetMergeRequestByRepoIDAndNumber(ctx, githubRepoID, 7)
	require.NoError(err)
	require.NotNil(githubMR)
	assert.Zero(githubMR.Additions)
	assert.Zero(githubMR.Deletions)
	assert.Empty(githubMR.MergeableState)
	assert.Nil(githubMR.DetailFetchedAt)

	gitlabMR, err := d.GetMergeRequestByRepoIDAndNumber(ctx, gitlabRepoID, 7)
	require.NoError(err)
	require.NotNil(gitlabMR)
	assert.Equal(123, gitlabMR.Additions)
	assert.NotNil(gitlabMR.DetailFetchedAt)
}

func TestIndexUpsertMRPersistsMergedActorEventFromPullRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(time.Minute)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}
	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	require.NoError(syncer.indexUpsertMR(ctx, &mockClient{}, repo, repoID, pr))
	require.NoError(syncer.indexUpsertMR(ctx, &mockClient{}, repo, repoID, pr))

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.MergeRequestStateMerged, got.State)

	events, err := d.ListMREvents(ctx, got.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(mergedAt))
}

func TestFetchMRDetailUsesRepoIDForPendingAndCallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(gitlabRepo)))
	require.NoError(err)

	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         gitlabRepoID,
		PlatformID:     7001,
		Number:         7,
		URL:            "https://code.example.com/acme/widget/-/merge_requests/7",
		Title:          "gitlab MR",
		Author:         "ada",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
		CIChecksJSON:   `[{"status":"in_progress"}]`,
	})
	require.NoError(err)

	pr := buildOpenPR(7, now)
	pr.Title = new("github MR")
	mc := &mockClient{
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		ciStatus: &gh.CombinedStatus{State: new("success")},
	}
	syncer := NewSyncer(
		map[string]Client{"code.example.com": mc},
		d, nil,
		[]RepoRef{githubRepo, gitlabRepo},
		time.Minute,
		nil,
		nil,
	)
	var callbackMR *db.MergeRequest
	syncer.onMRSynced = func(_, _ string, mr *db.MergeRequest) {
		callbackMR = mr
	}

	_, err = syncer.fetchMRDetail(ctx, githubRepo, githubRepoID, 7, true)
	require.NoError(err)

	githubMR, err := d.GetMergeRequestByRepoIDAndNumber(ctx, githubRepoID, 7)
	require.NoError(err)
	require.NotNil(githubMR)
	assert.False(githubMR.CIHadPending)
	assert.NotNil(githubMR.DetailFetchedAt)
	require.NotNil(callbackMR)
	assert.Equal(githubMR.ID, callbackMR.ID)

	gitlabMR, err := d.GetMergeRequestByRepoIDAndNumber(ctx, gitlabRepoID, 7)
	require.NoError(err)
	require.NotNil(gitlabMR)
	assert.False(gitlabMR.CIHadPending)
	assert.Nil(gitlabMR.DetailFetchedAt)
}

// TestFetchMRDetailPersistsWorkflowApproval verifies the budgeted
// detail drain (the path the periodic sync uses) also persists the
// workflow approval snapshot. Without this, the Approve workflows
// button would stay hidden for any PR whose detail came in through
// the queue rather than an explicit POST /sync.
func TestFetchMRDetailPersistsWorkflowApproval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	pr := buildOpenPR(7, now)
	headSHA := pr.GetHead().GetSHA()
	require.NotEmpty(headSHA)
	mc := &mockClient{
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		ciStatus: &gh.CombinedStatus{State: new("success")},
		workflowRuns: []*gh.WorkflowRun{{
			ID:           new(int64(4242)),
			HeadSHA:      &headSHA,
			Event:        new("pull_request"),
			PullRequests: []*gh.PullRequest{{Number: new(7)}},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 7, true)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	require.NotNil(got.WorkflowApprovalCheckedAt,
		"detail drain must populate workflow_approval_checked_at")
	assert.Equal(headSHA, got.WorkflowApprovalHeadSHA)
	assert.True(got.WorkflowApprovalRequired)
	assert.Equal(1, got.WorkflowApprovalCount)
}

func TestFetchMRDetailPersistsMergedActorEventFromPullRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(now.Add(time.Minute))
	pr.ClosedAt = makeTimestamp(now.Add(time.Minute))
	pr.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	pr.MergedBy = &gh.User{Login: &mergedBy}
	mc := &mockClient{
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 7, true)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.MergeRequestStateMerged, got.State)

	events, err := d.ListMREvents(ctx, got.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(now.Add(time.Minute)))
}

func TestFetchProviderMRDetailPersistsMergedActorEventFromMergeRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(time.Minute)

	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
		RepoPath:     "group/project",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	provider := &syncTestReadProvider{
		kind: platform.KindGitLab, host: "gitlab.example.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Merged GitLab MR",
			Author:         "ada",
			State:          "merged",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CreatedAt:      now,
			UpdatedAt:      mergedAt,
			LastActivityAt: mergedAt,
			MergedAt:       &mergedAt,
			ClosedAt:       &mergedAt,
			MergedBy:       "merge-admin",
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 7, true)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.MergeRequestStateMerged, got.State)

	events, err := d.ListMREvents(ctx, got.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(mergedAt))
}

func TestFetchMRDetailDoesNotDuplicateMergedTimelineEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(time.Minute)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}
	mc := &mockClient{
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		timelineEvents: []PullRequestTimelineEvent{{
			NodeID:    "ME_1",
			EventType: "merged",
			Actor:     mergedBy,
			CreatedAt: mergedAt,
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 7, true)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.MergeRequestStateMerged, got.State)

	events, err := d.ListMREvents(ctx, got.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(mergedAt))
}

func TestRefreshTimelineSkipsMergedEventWhenAuthoredMergedEventAlreadyExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(time.Minute)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}
	normalized, err := NormalizePR(repoID, pr)
	require.NoError(err)
	mrID, err := d.UpsertMergeRequest(ctx, normalized)
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         mergedBy,
		Summary:        "merged this",
		CreatedAt:      mergedAt,
		DedupeKey:      "timeline-fallback",
	}}))

	mc := &mockClient{
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		timelineEvents: []PullRequestTimelineEvent{{
			NodeID:    "ME_1",
			EventType: "merged",
			Actor:     mergedBy,
			CreatedAt: mergedAt.Add(time.Second),
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	require.NoError(syncer.refreshTimeline(ctx, repo, mrID,
		mergeRequestSnapshotRevision(t, d, repoID, 7), pr, ""))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("timeline-fallback", events[0].DedupeKey)
	assert.True(events[0].CreatedAt.Equal(mergedAt))
}

func TestSyncOpenMRFromBulkPersistsMergedActorEventFromPullRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(time.Minute)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:                    pr,
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: true,
		CommitsComplete:       true,
		TimelineComplete:      true,
		CIComplete:            true,
	}, true)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.MergeRequestStateMerged, got.State)

	events, err := d.ListMREvents(ctx, got.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(mergedAt))
}

func TestSyncOpenMRFromBulkPreservesReviewDecisionWhenReviewsConnectionReturnsEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	reviewer := "alice"
	approvedState := "APPROVED"
	reviewID := int64(501)
	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR: buildOpenPR(1, now),
		Reviews: []*gh.PullRequestReview{{
			ID: &reviewID, User: &gh.User{Login: &reviewer}, State: &approvedState,
		}},
		CommentsComplete: true, ReviewsComplete: true, CommitsComplete: true,
		TimelineComplete: true, CIComplete: true,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("approved", mr.ReviewDecision)

	// A later bulk fetch that legitimately completes (ReviewsComplete is
	// true, so the connection was not truncated) but observes zero
	// reviews must not clear the persisted decision: review history is
	// additive, and the earlier APPROVED review is retained even though
	// this fetch's Reviews connection came back empty.
	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               buildOpenPR(1, now.Add(time.Minute)),
		Reviews:          nil,
		CommentsComplete: true, ReviewsComplete: true, CommitsComplete: true,
		TimelineComplete: true, CIComplete: true,
	}, false)
	require.NoError(err)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("approved", mr.ReviewDecision)
}

func TestSyncOpenMRFromBulkResolvesReviewDecisionIndependentOfNestedCompleteness(t *testing.T) {
	reviewPtr := func(id int64, login, state string) *gh.PullRequestReview {
		return &gh.PullRequestReview{
			ID: &id, User: &gh.User{Login: &login}, State: &state,
		}
	}

	// Every case first establishes a persisted APPROVED decision via a fully
	// complete sync, then applies a second fetch whose completeness/scalar
	// combination is under test. The provider's reviewDecision scalar is
	// authoritative over the PR's whole review history, so nested-connection
	// truncation on the second fetch must not gate it; only a null/empty
	// scalar falls back to deriving from a complete reviews connection, and
	// an incomplete one preserves the additive history already persisted.
	cases := []struct {
		name             string
		scalar           string
		reviews          []*gh.PullRequestReview
		reviewsComplete  bool
		commitsComplete  bool
		timelineComplete bool
		ciComplete       bool
		want             string
	}{
		{
			name:             "authoritative scalar wins when reviews connection incomplete",
			scalar:           "CHANGES_REQUESTED",
			reviews:          nil,
			reviewsComplete:  false,
			commitsComplete:  true,
			timelineComplete: true,
			ciComplete:       true,
			want:             "changes_requested",
		},
		{
			name:             "authoritative scalar wins when an unrelated connection incomplete",
			scalar:           "CHANGES_REQUESTED",
			reviews:          []*gh.PullRequestReview{reviewPtr(710, "carol", "COMMENTED")},
			reviewsComplete:  true,
			commitsComplete:  false, // unrelated truncation must not gate the scalar
			timelineComplete: true,
			ciComplete:       true,
			want:             "changes_requested",
		},
		{
			name:             "authoritative scalar wins over a partial review page",
			scalar:           "CHANGES_REQUESTED",
			reviews:          []*gh.PullRequestReview{reviewPtr(711, "bob", "COMMENTED")},
			reviewsComplete:  true,
			commitsComplete:  true,
			timelineComplete: true,
			ciComplete:       true,
			want:             "changes_requested",
		},
		{
			name:             "empty scalar derives from a complete reviews connection",
			scalar:           "",
			reviews:          []*gh.PullRequestReview{reviewPtr(712, "alice", "CHANGES_REQUESTED")},
			reviewsComplete:  true,
			commitsComplete:  true,
			timelineComplete: true,
			ciComplete:       true,
			want:             "changes_requested",
		},
		{
			name:             "empty scalar preserves persisted history when reviews incomplete",
			scalar:           "",
			reviews:          nil,
			reviewsComplete:  false,
			commitsComplete:  true,
			timelineComplete: true,
			ciComplete:       true,
			want:             "approved",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			d := openTestDB(t)

			repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
			require.NoError(err)

			now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
			syncer := NewSyncer(
				map[string]Client{"github.com": &mockClient{}},
				d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
				time.Minute, nil, nil,
			)
			repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

			require.NoError(syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
				PR:               buildOpenPR(1, now),
				ReviewDecision:   "APPROVED",
				CommentsComplete: true, ReviewsComplete: true, CommitsComplete: true,
				TimelineComplete: true, CIComplete: true,
			}, false))
			seeded, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
			require.NoError(err)
			require.NotNil(seeded)
			require.Equal("approved", seeded.ReviewDecision)

			require.NoError(syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
				PR:               buildOpenPR(1, now.Add(time.Minute)),
				ReviewDecision:   tc.scalar,
				Reviews:          tc.reviews,
				CommentsComplete: true,
				ReviewsComplete:  tc.reviewsComplete,
				CommitsComplete:  tc.commitsComplete,
				TimelineComplete: tc.timelineComplete,
				CIComplete:       tc.ciComplete,
			}, false))

			mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
			require.NoError(err)
			require.NotNil(mr)
			assert.Equal(tc.want, mr.ReviewDecision)
		})
	}
}

func TestSyncOpenMRFromBulkSkipsMergedActorFallbackWhenAuthoredMergedEventExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	existingMergedAt := now.Add(time.Minute)
	incomingMergedAt := existingMergedAt.Add(time.Second)

	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)

	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     7000,
		Number:         7,
		URL:            "https://github.com/acme/widget/pull/7",
		Title:          "Merged PR",
		Author:         "alice",
		State:          db.MergeRequestStateMerged,
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      now,
		UpdatedAt:      existingMergedAt,
		LastActivityAt: existingMergedAt,
		MergedAt:       &existingMergedAt,
		ClosedAt:       &existingMergedAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      existingMergedAt,
		DedupeKey:      "timeline-existing",
	}}))

	merged := true
	mergedBy := "merge-admin"
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(incomingMergedAt)
	pr.ClosedAt = makeTimestamp(incomingMergedAt)
	pr.UpdatedAt = makeTimestamp(incomingMergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               pr,
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		TimelineComplete: true,
		CIComplete:       true,
	}, true)
	require.NoError(err)

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("timeline-existing", events[0].DedupeKey)
	assert.True(events[0].CreatedAt.Equal(incomingMergedAt),
		"the authored event must follow an accepted merged_at correction")
}

func TestFetchProviderMRDetailSyncsReviewThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	ref := platform.RepoRef{
		Platform:      platform.KindForgejo,
		Host:          "codeberg.org",
		Owner:         "acme",
		Name:          "widgets",
		RepoPath:      "acme/widgets",
		DefaultBranch: "main",
	}
	repo := RepoRef{
		Platform:      platform.KindForgejo,
		PlatformHost:  "codeberg.org",
		Owner:         "acme",
		Name:          "widgets",
		RepoPath:      "acme/widgets",
		DefaultBranch: "main",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(ref))
	require.NoError(err)

	line := 12
	provider := &syncTestReadProvider{
		kind: platform.KindForgejo, host: "codeberg.org",
		readReviewThreads: true,
		mergeRequests: []platform.MergeRequest{{
			Repo:               ref,
			PlatformID:         9001,
			PlatformExternalID: "9001",
			Number:             42,
			URL:                "https://codeberg.org/acme/widgets/pulls/42",
			Title:              "inline review",
			Author:             "ada",
			State:              "open",
			HeadBranch:         "feature",
			BaseBranch:         "main",
			HeadSHA:            "head-sha",
			BaseSHA:            "base-sha",
			CreatedAt:          now,
			UpdatedAt:          now,
			LastActivityAt:     now,
		}},
		reviewThreads: []platform.MergeRequestReviewThread{{
			ProviderThreadID:  "thread-42",
			ProviderReviewID:  "review-42",
			ProviderCommentID: "comment-42",
			Body:              "synced inline note",
			AuthorLogin:       "reviewer",
			DirectURL:         "https://codeberg.org/acme/widgets/pulls/42#issuecomment-comment-42",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				Line:        line,
				NewLine:     &line,
				LineType:    "add",
				DiffHeadSHA: "head-sha",
			},
			CreatedAt: now,
			UpdatedAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	routeFence, found, err := d.CurrentRepositoryRouteFence(
		ctx, platform.DBRepoIdentity(ref), repoID,
	)
	require.NoError(err)
	require.True(found)

	calls, err := syncer.fetchProviderMRDetail(
		ctx, provider, repo, repoID, 42, routeFence,
	)
	require.NoError(err)
	assert.Equal(3, calls)
	assert.Equal(int32(1), provider.listReviewThreads.Load())

	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 42)
	require.NoError(err)
	require.NotNil(mr)
	threads, err := d.ListMRReviewThreads(ctx, mr.ID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("thread-42", threads[0].ProviderThreadID)
	assert.Equal("synced inline note", threads[0].Body)

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("review_comment", events[0].EventType)
	assert.Equal("comment-42", events[0].PlatformExternalID)
	assert.Equal("https://codeberg.org/acme/widgets/pulls/42#issuecomment-comment-42", events[0].DirectURL)
	require.NotNil(events[0].ThreadID)
	assert.Equal("thread-42", *events[0].ThreadID)

	provider.reviewThreads = nil
	calls, err = syncer.fetchProviderMRDetail(
		ctx, provider, repo, repoID, 42, routeFence,
	)
	require.NoError(err)
	assert.Equal(3, calls)

	threads, err = d.ListMRReviewThreads(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(threads)
	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestFetchGitHubMRDetailSyncsReviewThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 5, 27, 16, 1, 31, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: platform.DefaultGitHubHost,
		Owner:        "acme",
		Name:         "widgets",
		RepoPath:     "acme/widgets",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widgets"))
	require.NoError(err)

	prID := int64(9001)
	prNodeID := "PR_kwDO123"
	number := 42
	title := "inline review"
	state := "open"
	author := "ada"
	headRef := "feature"
	baseRef := "main"
	headSHA := "head-sha"
	baseSHA := "base-sha"
	url := "https://github.com/acme/widgets/pull/42"
	line := 12
	commentID := int64(101)
	reviewID := int64(201)
	commentURL := "https://github.com/acme/widgets/pull/42#discussion_r101"
	mock := &mockClient{
		singlePR: &gh.PullRequest{
			ID:      &prID,
			NodeID:  &prNodeID,
			Number:  &number,
			HTMLURL: &url,
			Title:   &title,
			State:   &state,
			User:    &gh.User{Login: &author},
			Head: &gh.PullRequestBranch{
				Ref: &headRef,
				SHA: &headSHA,
				Repo: &gh.Repository{
					CloneURL: new("https://github.com/acme/widgets.git"),
				},
			},
			Base: &gh.PullRequestBranch{
				Ref: &baseRef,
				SHA: &baseSHA,
			},
			CreatedAt: &gh.Timestamp{Time: now},
			UpdatedAt: &gh.Timestamp{Time: now},
		},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		ciStatus: &gh.CombinedStatus{State: new("success")},
		reviewThreads: []PullRequestReviewThread{{
			NodeID: "PRRT_1",
			Path:   ".golangci.yml",
			Side:   "RIGHT",
			Line:   line,
			Comments: []PullRequestReviewThreadComment{{
				NodeID:           "PRRC_1",
				DatabaseID:       commentID,
				ReviewDatabaseID: reviewID,
				Body:             "inline note",
				AuthorLogin:      "reviewer",
				URL:              commentURL,
				CommitID:         headSHA,
				CreatedAt:        now,
				UpdatedAt:        now,
			}, {
				NodeID:           "PRRC_2",
				DatabaseID:       commentID + 1,
				ReviewDatabaseID: reviewID,
				Body:             "reply note",
				AuthorLogin:      "maintainer",
				CommitID:         headSHA,
				CreatedAt:        now.Add(time.Minute),
				UpdatedAt:        now.Add(time.Minute),
			}},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{repo},
		time.Minute,
		nil,
		nil,
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, number, true)
	require.NoError(err)

	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
	require.NoError(err)
	require.NotNil(mr)
	threads, err := d.ListMRReviewThreads(ctx, mr.ID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("PRRT_1", threads[0].ProviderThreadID)
	assert.Equal("101", threads[0].ProviderCommentID)
	assert.Equal(".golangci.yml", threads[0].Range.Path)
	assert.Equal(line, threads[0].Range.Line)

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 2)
	assert.Equal("review_comment", events[0].EventType)
	assert.Equal("102", events[0].PlatformExternalID)
	assert.Equal("reply note", events[0].Body)
	require.NotNil(events[0].ThreadID)
	assert.Equal("PRRT_1", *events[0].ThreadID)
	assert.Equal("review_comment", events[1].EventType)
	assert.Equal("101", events[1].PlatformExternalID)
	assert.Equal("inline note", events[1].Body)
	assert.Equal(commentURL, events[1].DirectURL)
	require.NotNil(events[1].ThreadID)
	assert.Equal("PRRT_1", *events[1].ThreadID)
}

func TestSyncOpenIssueReadsExistingByRepoID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(gitlabRepo)))
	require.NoError(err)

	detailFetchedAt := now.Add(-time.Minute)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:          gitlabRepoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://code.example.com/acme/widget/-/issues/7",
		Title:           "gitlab issue",
		Author:          "ada",
		State:           "open",
		CommentCount:    12,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	issueNumber := 7
	issueTitle := "github issue"
	issueState := "open"
	mc := &mockClient{
		comments: []*gh.IssueComment{},
	}
	syncer := NewSyncer(
		map[string]Client{"code.example.com": mc},
		d, nil,
		[]RepoRef{githubRepo, gitlabRepo},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.syncOpenIssue(ctx, mc, githubRepo, githubRepoID, &gh.Issue{
		ID:        new(int64(1007)),
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   new("https://code.example.com/acme/widget/issues/7"),
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}, false)
	require.NoError(err)

	githubIssue, err := d.GetIssueByRepoIDAndNumber(ctx, githubRepoID, 7)
	require.NoError(err)
	require.NotNil(githubIssue)
	assert.Zero(githubIssue.CommentCount)
	assert.Nil(githubIssue.DetailFetchedAt)

	gitlabIssue, err := d.GetIssueByRepoIDAndNumber(ctx, gitlabRepoID, 7)
	require.NoError(err)
	require.NotNil(gitlabIssue)
	assert.Equal(12, gitlabIssue.CommentCount)
	assert.NotNil(gitlabIssue.DetailFetchedAt)
}

func TestSyncMRReturnsErrorWhenClientReturnsNilPR(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	mc := &mockClient{
		getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
			return nil, nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{{
		Owner: "acme", Name: "widget", PlatformHost: "github.com",
	}}, time.Minute, nil, nil)

	err := syncer.SyncMR(ctx, "acme", "widget", 10)
	require.Error(err)
	require.ErrorContains(err, "client returned nil pull request")

	stored, getErr := database.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 10)
	require.NoError(getErr)
	require.Nil(stored)
}

func TestSyncIssueReturnsErrorWhenClientReturnsNilIssue(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()

	mc := &mockClient{
		getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			return nil, nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{{
		Owner: "acme", Name: "widget", PlatformHost: "github.com",
	}}, time.Minute, nil, nil)

	err := syncer.SyncIssue(ctx, "acme", "widget", 5)
	require.Error(err)
	require.ErrorContains(err, "client returned nil issue")

	stored, getErr := database.GetIssue(ctx, "github", "github.com", "acme", "widget", 5)
	require.NoError(getErr)
	require.Nil(stored)
}

func TestSyncItemByNumber_UntrackedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)

	mc := &mockClient{}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, database, nil, []RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
	}, time.Minute, nil, nil)

	_, err := syncer.SyncItemByNumber(t.Context(), "other", "repo", 1)
	require.Error(err)
	assert.Contains(err.Error(), "not tracked")
}

func TestSyncerMultiHostClientDispatch(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	gheMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	clients := map[string]Client{
		"github.com":   ghMock,
		"ghe.corp.com": gheMock,
	}
	repos := []RepoRef{
		{Owner: "pub", Name: "repo", PlatformHost: "github.com"},
		{Owner: "corp", Name: "internal", PlatformHost: "ghe.corp.com"},
	}

	syncer := NewSyncer(clients, d, nil, repos, time.Minute, nil, nil)
	syncer.RunOnce(t.Context())

	assert.True(ghMock.listOpenPRsCalled.Load(),
		"github.com mock should have been called")
	assert.True(gheMock.listOpenPRsCalled.Load(),
		"ghe.corp.com mock should have been called")
}

func TestSyncRunUsesProviderReadersForIndexSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "gid://gitlab/Project/100",
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     1001,
			Number:         7,
			URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
			Title:          "Provider MR",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "abc123",
			BaseSHA:        "def456",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		issues: []platform.Issue{{
			Repo:           platformRepoRef(repo),
			PlatformID:     2001,
			Number:         11,
			URL:            "https://gitlab.com/acme/widget/-/issues/11",
			Title:          "Provider issue",
			Author:         "grace",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry
	syncer.RunOnce(t.Context())

	assert.Equal(int32(1), provider.listMRCalls.Load())
	assert.Equal(int32(1), provider.listIssueCalls.Load())
	mr, err := d.GetMergeRequest(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("Provider MR", mr.Title)
	assert.Equal("abc123", mr.PlatformHeadSHA)
	issue, err := d.GetIssue(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 11)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Provider issue", issue.Title)
}

func TestSyncRunAllowsMergeRequestOnlyProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "gid://gitlab/Project/101",
	}
	provider := &syncTestMergeRequestOnlyProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     1001,
			Number:         7,
			URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
			Title:          "MR-only provider",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "abc123",
			BaseSHA:        "def456",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry

	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(r []RepoSyncResult) {
		results = r
	})
	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.Equal(platform.KindGitLab, results[0].Platform)
	require.Equal("gitlab.com", results[0].PlatformHost)
	require.Empty(results[0].Error)
	assert.Equal(int32(1), provider.listMRCalls.Load())
	mr, err := d.GetMergeRequest(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("MR-only provider", mr.Title)
}

func TestSyncRunAllowsIssueOnlyProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "gid://gitlab/Project/102",
	}
	provider := &syncTestIssueOnlyProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		issues: []platform.Issue{{
			Repo:           platformRepoRef(repo),
			PlatformID:     2001,
			Number:         11,
			URL:            "https://gitlab.com/acme/widget/-/issues/11",
			Title:          "Issue-only provider",
			Author:         "grace",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry

	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(r []RepoSyncResult) {
		results = r
	})
	syncer.RunOnce(t.Context())

	require.Len(results, 1)
	require.Equal(platform.KindGitLab, results[0].Platform)
	require.Equal("gitlab.com", results[0].PlatformHost)
	require.Empty(results[0].Error)
	assert.Equal(int32(1), provider.listIssueCalls.Load())
	issue, err := d.GetIssue(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 11)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Issue-only provider", issue.Title)
}

func TestSyncMRUsesProviderMergeRequestReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     1001,
			Number:         7,
			URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
			Title:          "Provider MR detail",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "abc123",
			BaseSHA:        "def456",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry

	err = syncer.SyncMR(t.Context(), "acme", "widget", 7)

	require.NoError(err)
	assert.Equal(int32(1), provider.getMRCalls.Load())
	mr, err := d.GetMergeRequest(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("Provider MR detail", mr.Title)
	assert.Equal("abc123", mr.PlatformHeadSHA)
}

func TestSyncIssueUsesProviderIssueReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	}
	commentID := int64(2101)
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		issues: []platform.Issue{{
			Repo:           platformRepoRef(repo),
			PlatformID:     2001,
			Number:         11,
			URL:            "https://gitlab.com/acme/widget/-/issues/11",
			Title:          "Provider issue detail",
			Author:         "grace",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
			CommentCount:   1,
		}},
		listIssueReadEvents: []platform.IssueEvent{{
			Repo:        platformRepoRef(repo),
			PlatformID:  commentID,
			IssueNumber: 11,
			EventType:   "issue_comment",
			Author:      "grace",
			Body:        "remove after provider refresh",
			CreatedAt:   now,
			DedupeKey:   "gitlab-issue-comment-2101",
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry

	err = syncer.SyncIssue(t.Context(), "acme", "widget", 11)

	require.NoError(err)
	assert.Equal(int32(1), provider.getIssueCalls.Load())
	issue, err := d.GetIssue(t.Context(), "gitlab", "gitlab.com", "acme", "widget", 11)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Provider issue detail", issue.Title)
	assert.NotNil(issue.DetailFetchedAt)
	events, err := d.ListIssueEvents(t.Context(), issue.ID)
	require.NoError(err)
	require.Len(events, 1)

	provider.listIssueReadEvents = nil
	provider.issues[0].CommentCount = 0
	require.NoError(syncer.SyncIssue(t.Context(), "acme", "widget", 11))
	events, err = d.ListIssueEvents(t.Context(), issue.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestDirectMRSyncReplacesConflictingPathOccupant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	displacedID, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.com", PlatformRepoID: "old-repo",
		Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NoError(database.UpdateRepoSettings(ctx, displacedID, false, false, false, false))

	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.com",
		Owner: "acme", Name: "widget", PlatformExternalID: "incoming-repo",
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab, host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo: platformRepoRef(repo), PlatformID: 7007, Number: 7,
			Title: "incoming merge request", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncMROnProvider(
		ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", 7,
	))

	stored, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.com", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NotNil(stored)
	assert.NotEqual(displacedID, stored.ID)
	assert.Equal("incoming-repo", stored.PlatformRepoID)
	assert.True(stored.AllowSquashMerge)
	assert.False(stored.ViewerCanMerge,
		"replacement must fail closed when the provider omits viewer permission")
}

func TestDirectIssueSyncReplacesConflictingPathOccupant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	displacedID, err := database.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.com", PlatformRepoID: "old-repo",
		Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NoError(database.UpdateRepoSettings(ctx, displacedID, false, false, false, false))

	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitLab, PlatformHost: "gitlab.com",
		Owner: "acme", Name: "widget", PlatformExternalID: "incoming-repo",
	}
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab, host: "gitlab.com",
		issues: []platform.Issue{{
			Repo: platformRepoRef(repo), PlatformID: 8011, Number: 11,
			Title: "incoming issue", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.SyncIssueOnProvider(
		ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", 11,
	))

	stored, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.com", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	require.NotNil(stored)
	assert.NotEqual(displacedID, stored.ID)
	assert.Equal("incoming-repo", stored.PlatformRepoID)
	assert.True(stored.AllowMergeCommit)
	assert.False(stored.ViewerCanMerge,
		"replacement must fail closed when the provider omits viewer permission")
}

func TestSyncIssueProviderCommentReplacementRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	detailFetchedAt := now.Add(time.Minute)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	}
	repoID, err := database.UpsertRepo(t.Context(), verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	issue := &db.Issue{
		RepoID:          repoID,
		PlatformID:      2001,
		Number:          11,
		URL:             "https://gitlab.com/acme/widget/-/issues/11",
		Title:           "Provider issue detail",
		Author:          "grace",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	}
	issue.ID, err = database.UpsertIssue(t.Context(), issue)
	require.NoError(err)
	oldCommentID := int64(2101)
	require.NoError(database.UpsertIssueEvents(t.Context(), []db.IssueEvent{{
		IssueID: issue.ID, PlatformID: &oldCommentID, EventType: "issue_comment",
		Body: "old", CreatedAt: now, DedupeKey: "old-comment",
	}}))
	_, err = database.WriteDB().ExecContext(t.Context(), `
		CREATE TRIGGER reject_new_provider_issue_comment
		BEFORE INSERT ON forge_issue_events
		WHEN NEW.dedupe_key = 'new-comment'
		BEGIN SELECT RAISE(ABORT, 'reject new comment'); END`)
	require.NoError(err)

	newCommentID := int64(2102)
	secondNewCommentID := int64(2103)
	provider := &syncTestReadProvider{
		kind: platform.KindGitLab, host: "gitlab.com",
		issues: []platform.Issue{{
			Repo: platformRepoRef(repo), PlatformID: 2001, Number: 11,
			URL: issue.URL, Title: issue.Title, Author: issue.Author, State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now, CommentCount: 2,
		}},
		listIssueReadEvents: []platform.IssueEvent{
			{
				Repo: platformRepoRef(repo), PlatformID: newCommentID, IssueNumber: 11,
				EventType: "issue_comment", Body: "new", CreatedAt: now, DedupeKey: "new-comment",
			},
			{
				Repo: platformRepoRef(repo), PlatformID: secondNewCommentID, IssueNumber: 11,
				EventType: "issue_comment", Body: "second", CreatedAt: now, DedupeKey: "second-new-comment",
			},
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncer(nil, database, nil, []RepoRef{repo}, time.Minute, nil, nil)
	syncer.clients = registry

	require.Error(syncer.SyncIssue(t.Context(), "acme", "widget", 11))
	events, err := database.ListIssueEvents(t.Context(), issue.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("old-comment", events[0].DedupeKey)
	stored, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(1, stored.CommentCount)
	assert.NotNil(stored.DetailFetchedAt)
}

func TestOnMRSyncedCalledDuringSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, testBudget(500),
	)

	type hookCall struct {
		owner        string
		name         string
		number       int
		ciChecksJSON string
		updatedAt    time.Time
	}
	var called []hookCall
	syncer.SetOnMRSynced(func(owner, name string, mr *db.MergeRequest) {
		called = append(called, hookCall{
			owner:        owner,
			name:         name,
			number:       mr.Number,
			ciChecksJSON: mr.CIChecksJSON,
			updatedAt:    mr.UpdatedAt,
		})
	})

	syncer.RunOnce(t.Context())

	require.Len(called, 1)
	assert.Equal("owner", called[0].owner)
	assert.Equal("repo", called[0].name)
	assert.Equal(1, called[0].number)
	assert.True(called[0].updatedAt.Equal(now),
		"UpdatedAt should match the PR's UpdatedAt")
}

func TestOnMRSyncedIncludesCIChecksJSON(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ciState := "success"
	checkName := "build"
	checkStatus := "completed"
	checkConclusion := "success"
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		ciStatus: &gh.CombinedStatus{State: &ciState},
	}
	mc.checkRuns = []*gh.CheckRun{
		{
			Name:       &checkName,
			Status:     &checkStatus,
			Conclusion: &checkConclusion,
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "owner", Name: "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, testBudget(500),
	)

	var gotJSON string
	syncer.SetOnMRSynced(
		func(_ string, _ string, mr *db.MergeRequest) {
			gotJSON = mr.CIChecksJSON
		},
	)

	syncer.RunOnce(t.Context())

	assert.Contains(gotJSON, "build",
		"CIChecksJSON should contain check run name")
}

func TestOnSyncCompletedCalledAfterSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{
			{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
			{Owner: "acme", Name: "lib", PlatformHost: "github.com"},
		},
		time.Minute, nil, nil,
	)

	var gotResults []RepoSyncResult
	syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
		gotResults = results
	})

	syncer.RunOnce(t.Context())

	require.Len(gotResults, 2)
	assert.Equal("acme", gotResults[0].Owner)
	assert.Equal("widget", gotResults[0].Name)
	assert.Equal(platform.KindGitHub, gotResults[0].Platform)
	assert.Equal("github.com", gotResults[0].PlatformHost)
	assert.Empty(gotResults[0].Error)
	assert.Equal("acme", gotResults[1].Owner)
	assert.Equal("lib", gotResults[1].Name)
	assert.Equal(platform.KindGitHub, gotResults[1].Platform)
	assert.Equal("github.com", gotResults[1].PlatformHost)
	assert.Empty(gotResults[1].Error)
}

func TestNilHooksNoOp(t *testing.T) {
	d := openTestDB(t)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// No hooks set -- should not panic.
	syncer.RunOnce(t.Context())
}

func TestWatchedMRsSyncedOnFastInterval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(7, now)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app",
			PlatformHost: "github.com",
		}},
		time.Hour, nil, nil, // bulk sync at 1h -- won't fire during test
	)
	syncer.SetWatchInterval(50 * time.Millisecond)

	var mu sync.Mutex
	var hookCalls []int
	syncer.SetOnMRSynced(
		func(_ string, _ string, mr *db.MergeRequest) {
			mu.Lock()
			hookCalls = append(hookCalls, mr.Number)
			mu.Unlock()
		},
	)

	syncer.SetWatchedMRs([]WatchedMR{
		{Owner: "acme", Name: "app", Number: 7},
	})

	syncer.Start(ctx)
	defer syncer.Stop()

	// Wait for at least one fast-sync tick.
	assert.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hookCalls) >= 1
	}, 10*time.Second, 20*time.Millisecond)

	// Verify the MR was persisted.
	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "app", 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(7, mr.Number)
}

func TestEmptyWatchListNoOp(t *testing.T) {
	d := openTestDB(t)

	mc := &mockClient{
		openPRs: []*gh.PullRequest{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app",
			PlatformHost: "github.com",
		}},
		time.Hour, nil, nil,
	)
	callCount := 0
	syncer.SetOnMRSynced(
		func(_ string, _ string, _ *db.MergeRequest) {
			callCount++
		},
	)

	syncer.syncWatchedMRs(t.Context())

	assert.Equal(t, 0, callCount,
		"empty watch list should not trigger any MR syncs")
}

func TestSetWatchedMRsReplacesList(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	// Return different PRs based on the requested number.
	mc.getPullRequestFn = func(
		_ context.Context, _, _ string, number int,
	) (*gh.PullRequest, error) {
		return buildOpenPR(number, now), nil
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app",
			PlatformHost: "github.com",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchInterval(50 * time.Millisecond)

	var mu sync.Mutex
	syncedNumbers := map[int]int{} // number -> count
	syncer.SetOnMRSynced(
		func(_ string, _ string, mr *db.MergeRequest) {
			mu.Lock()
			syncedNumbers[mr.Number]++
			mu.Unlock()
		},
	)

	// Start with PR #1 on the watch list.
	syncer.SetWatchedMRs([]WatchedMR{
		{Owner: "acme", Name: "app", Number: 1},
	})
	syncer.Start(t.Context())
	defer syncer.Stop()

	// Wait for PR #1 to be synced.
	assert.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return syncedNumbers[1] >= 1
	}, 10*time.Second, 20*time.Millisecond)

	// Replace with PR #2 only.
	mu.Lock()
	countPR1Before := syncedNumbers[1]
	mu.Unlock()

	syncer.SetWatchedMRs([]WatchedMR{
		{Owner: "acme", Name: "app", Number: 2},
	})

	// Wait for PR #2 to be synced.
	assert.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return syncedNumbers[2] >= 1
	}, 10*time.Second, 20*time.Millisecond)

	// PR #1 should not accumulate many more syncs after replacement.
	// Allow at most 1 extra (for an in-flight tick at replacement time).
	mu.Lock()
	countPR1After := syncedNumbers[1]
	mu.Unlock()
	assert.LessOrEqual(countPR1After, countPR1Before+1,
		"PR #1 should stop being synced after watch list replacement")
}

func TestWatchedMRsForFastSyncSkipsArchivedRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	liveID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-live", Owner: "acme", Name: "live",
	})
	require.NoError(err)
	frozenID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-frozen", Owner: "acme", Name: "frozen",
	})
	require.NoError(err)
	seedMR := func(repoID int64, number int) {
		_, upsertErr := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number,
			Title: "PR", Author: "octo", State: db.MergeRequestStateOpen,
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: now.Add(-30 * time.Minute), LastActivityAt: now.Add(-30 * time.Minute),
		})
		require.NoError(upsertErr)
	}
	seedMR(liveID, 1)
	seedMR(frozenID, 2)

	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{
			{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "live"},
			{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "frozen", Archived: true},
		},
		time.Hour, nil, nil,
	)
	syncer.SetActiveMRWindow(4 * time.Hour)

	got := syncer.watchedMRsForFastSync(ctx, now)
	assert.ElementsMatch([]WatchedMR{{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "live", Number: 1,
	}}, got, "open MRs on archived repos must not enter fast sync")
}

func TestWatchedMRsIncludeRecentlyActiveOpenPRs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	githubRepoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-app",
		Owner:          "acme",
		Name:           "app",
	})
	require.NoError(err)
	gheRepoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "ghe.example.com",
		PlatformRepoID: "ghe-acme-app",
		Owner:          "acme",
		Name:           "app",
	})
	require.NoError(err)

	seedMR := func(repoID int64, number int, state db.MergeRequestState, lastActivity time.Time) {
		_, upsertErr := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number,
			Title: "PR", Author: "octo", State: state,
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: lastActivity, LastActivityAt: lastActivity,
		})
		require.NoError(upsertErr)
	}
	seedMR(githubRepoID, 1, db.MergeRequestStateOpen, now.Add(-30*time.Minute))
	seedMR(githubRepoID, 2, db.MergeRequestStateOpen, now.Add(-5*time.Hour))
	seedMR(githubRepoID, 3, db.MergeRequestStateClosed, now.Add(-10*time.Minute))
	seedMR(gheRepoID, 4, db.MergeRequestStateOpen, now.Add(-3*time.Hour))

	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{
			{Platform: platform.KindGitHub, PlatformHost: "github.com", Owner: "acme", Name: "app"},
			{Platform: platform.KindGitHub, PlatformHost: "ghe.example.com", Owner: "acme", Name: "app"},
		},
		time.Hour, nil, nil,
	)
	syncer.SetActiveMRWindow(4 * time.Hour)
	syncer.SetWatchedMRs([]WatchedMR{{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "app", Number: 99,
	}})

	got := syncer.watchedMRsForFastSync(ctx, now)
	assert.ElementsMatch([]WatchedMR{
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 1,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "ghe.example.com",
			Owner: "acme", Name: "app", Number: 4,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 99,
		},
	}, got)
}

func TestWatchedMRsUsePersistedHotAndWarmCadences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-app",
		Owner:          "acme",
		Name:           "app",
	})
	require.NoError(err)

	seedMR := func(number int, lastActivity time.Time, detailFetchedAt *time.Time) int64 {
		id, upsertErr := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number,
			Title: "PR", Author: "octo", State: db.MergeRequestStateOpen,
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: lastActivity, LastActivityAt: lastActivity,
			DetailFetchedAt: detailFetchedAt,
		})
		require.NoError(upsertErr)
		return id
	}
	hotNotDue := now.Add(-1 * time.Minute)
	hotDue := now.Add(-2 * time.Minute)
	warmNotDue := now.Add(-9 * time.Minute)
	warmDue := now.Add(-10 * time.Minute)
	hotNotDueID := seedMR(1, now.Add(-10*time.Minute), &hotNotDue)
	hotDueID := seedMR(2, now.Add(-10*time.Minute), &hotDue)
	seedMR(3, now.Add(-45*time.Minute), &warmNotDue)
	seedMR(4, now.Add(-45*time.Minute), &warmDue)
	hotOutsideWindowID := seedMR(5, now.Add(-5*time.Hour), &hotDue)
	seedMR(6, now.Add(-45*time.Minute), nil)
	hotNeverFetchedID := seedMR(7, now.Add(-5*time.Hour), nil)
	require.NoError(d.RecordHotMergeRequestView(ctx, hotNotDueID, now.Add(-3*time.Minute)))
	require.NoError(d.RecordHotMergeRequestView(ctx, hotDueID, now.Add(-2*time.Minute)))
	require.NoError(d.RecordHotMergeRequestView(ctx, hotOutsideWindowID, now.Add(-time.Minute)))
	require.NoError(d.RecordHotMergeRequestView(ctx, hotNeverFetchedID, now))

	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchInterval(2 * time.Minute)
	syncer.SetActiveMRWindow(4 * time.Hour)

	got := syncer.watchedMRsForFastSync(ctx, now)
	assert.ElementsMatch([]WatchedMR{
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 2,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 4,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 5,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 6,
		},
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 7,
		},
	}, got)
}

func TestWatchedMRsUseNotificationActivityForWarmPRCadence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-app",
		Owner:          "acme",
		Name:           "app",
	})
	require.NoError(err)

	seedMR := func(number int, state db.MergeRequestState, detailFetchedAt time.Time) {
		_, upsertErr := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number,
			Title: "PR", Author: "octo", State: state,
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: now.Add(-5 * time.Hour), LastActivityAt: now.Add(-5 * time.Hour),
			DetailFetchedAt: &detailFetchedAt,
		})
		require.NoError(upsertErr)
	}
	seedMR(1, db.MergeRequestStateOpen, now.Add(-9*time.Minute))
	seedMR(2, db.MergeRequestStateOpen, now.Add(-10*time.Minute))
	seedMR(3, db.MergeRequestStateOpen, now.Add(-10*time.Minute))
	seedMR(4, db.MergeRequestStateClosed, now.Add(-10*time.Minute))

	notification := func(id string, number int, updatedAt time.Time) db.Notification {
		return db.Notification{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: id,
			RepoID:                 &repoID,
			RepoOwner:              "acme",
			RepoName:               "app",
			SubjectType:            "PullRequest",
			SubjectTitle:           "PR activity",
			ItemNumber:             &number,
			ItemType:               "pr",
			Reason:                 "comment",
			Unread:                 true,
			SourceUpdatedAt:        updatedAt,
			SyncedAt:               updatedAt,
		}
	}
	require.NoError(d.UpsertNotifications(ctx, []db.Notification{
		notification("hot-not-due", 1, now.Add(-10*time.Minute)),
		notification("hot-due", 2, now.Add(-10*time.Minute)),
		notification("expired", 3, now.Add(-5*time.Hour)),
		notification("closed", 4, now.Add(-10*time.Minute)),
	}))

	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchInterval(2 * time.Minute)
	syncer.SetActiveMRWindow(4 * time.Hour)

	got := syncer.watchedMRsForFastSync(ctx, now)
	assert.ElementsMatch([]WatchedMR{
		{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app", Number: 2,
		},
	}, got)
}

func TestWatchedMRsNotifyOnceAfterFastSync(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(5, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app", PlatformHost: "github.com",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchedMRs([]WatchedMR{{
		Owner: "acme", Name: "app", Number: 5, PlatformHost: "github.com",
	}})

	calls := 0
	syncer.SetOnWatchedMRSyncCompleted(func() {
		calls++
	})

	syncer.syncWatchedMRs(t.Context())

	assert.Equal(1, calls)
}

func TestSyncWatchedMRsSerializesConcurrentPasses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getPullRequestFn: func(
			_ context.Context,
			_, _ string,
			_ int,
		) (*gh.PullRequest, error) {
			current := inFlight.Add(1)
			for observed := maxInFlight.Load(); current > observed; observed = maxInFlight.Load() {
				if maxInFlight.CompareAndSwap(observed, current) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			inFlight.Add(-1)
			return buildOpenPR(5, now), nil
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app", PlatformHost: "github.com",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchInterval(2 * time.Minute)
	syncer.SetWatchedMRs([]WatchedMR{{
		Owner: "acme", Name: "app", Number: 5, PlatformHost: "github.com",
	}})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		syncer.SyncWatchedMRs(t.Context())
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow("first watched pass did not enter provider work")
	}

	secondDone := make(chan struct{})
	secondStarted := make(chan struct{})
	go func() {
		defer close(secondDone)
		close(secondStarted)
		syncer.SyncWatchedMRs(t.Context())
	}()
	<-secondStarted

	secondEntered := false
	select {
	case <-entered:
		secondEntered = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	for name, done := range map[string]<-chan struct{}{
		"first": firstDone, "second": secondDone,
	} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			require.Failf("watched pass did not finish", "%s pass timed out", name)
		}
	}

	assert.False(secondEntered, "a second watched pass entered provider work concurrently")
	assert.Equal(int32(1), maxInFlight.Load())
}

func TestWatchedMRsSkipRateLimitedHost(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(5, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	rt := NewRateTracker(d, "github.com", "host", "rest")
	// Exhaust the rate limit with a future reset.
	futureReset := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(Rate{
		Remaining: 0,
		Reset:     futureReset,
	})

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app",
			PlatformHost: "github.com",
		}},
		time.Hour,
		map[string]*RateTracker{"github.com": rt}, nil,
	)
	syncer.SetWatchInterval(50 * time.Millisecond)

	callCount := 0
	syncer.SetOnMRSynced(
		func(_ string, _ string, _ *db.MergeRequest) {
			callCount++
		},
	)

	syncer.SetWatchedMRs([]WatchedMR{
		{
			Owner: "acme", Name: "app",
			Number: 5, PlatformHost: "github.com",
		},
	})

	// Call syncWatchedMRs directly to avoid the bulk RunOnce goroutine.
	syncer.syncWatchedMRs(t.Context())

	assert.Equal(0, callCount,
		"watched MRs should be skipped when host is rate-limited")
}

func TestWatchedMROnGHEHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	gheMC := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(3, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	syncer := NewSyncer(
		map[string]Client{"ghes.corp.com": gheMC}, d, nil,
		[]RepoRef{{
			Owner: "corp", Name: "internal",
			PlatformHost:       "ghes.corp.com",
			PlatformExternalID: "repo-corp-internal",
		}},
		time.Hour, nil, nil,
	)

	var hookedOwner, hookedName string
	syncer.SetOnMRSynced(
		func(owner, name string, _ *db.MergeRequest) {
			hookedOwner = owner
			hookedName = name
		},
	)

	syncer.SetWatchedMRs([]WatchedMR{
		{
			Owner: "corp", Name: "internal",
			Number: 3, PlatformHost: "ghes.corp.com",
		},
	})

	syncer.syncWatchedMRs(ctx)

	// The MR should have been synced via the GHE client.
	mr, err := d.GetMergeRequest(ctx, "github", "ghes.corp.com", "corp", "internal", 3)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(3, mr.Number)
	assert.Equal("corp", hookedOwner)
	assert.Equal("internal", hookedName)

	// Verify the MR is associated with the GHE repo row, not github.com.
	repo, err := d.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("ghes.corp.com", "corp", "internal"))
	require.NoError(err)
	require.NotNil(repo)
	assert.Equal("ghes.corp.com", repo.PlatformHost)
	assert.Equal(repo.ID, mr.RepoID)
}

func TestWatchedMRRejectsUnmatchedHost(t *testing.T) {
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(1, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	// Track acme/app only on github.com.
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "acme", Name: "app",
			PlatformHost: "github.com",
		}},
		time.Hour, nil, nil,
	)

	callCount := 0
	syncer.SetOnMRSynced(
		func(_ string, _ string, _ *db.MergeRequest) {
			callCount++
		},
	)

	// Watch the same owner/name but on a different host.
	syncer.SetWatchedMRs([]WatchedMR{
		{
			Owner: "acme", Name: "app",
			Number: 1, PlatformHost: "ghes.other.com",
		},
	})

	syncer.syncWatchedMRs(t.Context())

	assert.Equal(t, 0, callCount,
		"watched MR on untracked host should not be synced")
}

func TestRunOnceSkipsThrottledHosts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	gheMock := &mockClient{
		openPRs:  []*gh.PullRequest{buildOpenPR(1, now)},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}

	// Set up GHE tracker with remaining below reserve buffer.
	gheTracker := NewRateTracker(d, "ghe.corp.com", "host", "rest")
	gheTracker.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: 100, // below RateReserveBuffer (200)
		Reset:     time.Now().Add(30 * time.Minute),
	})

	clients := map[string]Client{
		"github.com":   ghMock,
		"ghe.corp.com": gheMock,
	}
	trackers := map[string]*RateTracker{
		"ghe.corp.com": gheTracker,
	}
	repos := []RepoRef{
		{Owner: "pub", Name: "repo", PlatformHost: "github.com"},
		{Owner: "corp", Name: "internal", PlatformHost: "ghe.corp.com"},
	}

	syncer := NewSyncer(clients, d, nil, repos, time.Minute, trackers, nil)

	var gotResults []RepoSyncResult
	syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
		gotResults = results
	})

	syncer.RunOnce(t.Context())

	require.Len(gotResults, 2)

	// github.com repo should have synced (no error).
	assert.Equal("pub", gotResults[0].Owner)
	assert.Equal("repo", gotResults[0].Name)
	assert.Equal(platform.KindGitHub, gotResults[0].Platform)
	assert.Equal("github.com", gotResults[0].PlatformHost)
	assert.Empty(gotResults[0].Error,
		"github.com repo should sync normally")

	// ghe.corp.com repo should be skipped due to throttle.
	assert.Equal("corp", gotResults[1].Owner)
	assert.Equal("internal", gotResults[1].Name)
	assert.Equal(platform.KindGitHub, gotResults[1].Platform)
	assert.Equal("ghe.corp.com", gotResults[1].PlatformHost)
	assert.Equal("skipped: rate limit throttled", gotResults[1].Error,
		"ghe.corp.com repo should be skipped when paused")

	// github.com mock should have been called, GHE should not.
	assert.True(ghMock.listOpenPRsCalled.Load(),
		"github.com client should have been called")
	assert.False(gheMock.listOpenPRsCalled.Load(),
		"ghe.corp.com client should NOT have been called")
}

func TestRunOnceSkipsArchivedRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			archived := repo == "frozen"
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}
	repos := []RepoRef{
		{Owner: "acme", Name: "live", PlatformHost: "github.com"},
		{Owner: "acme", Name: "frozen", PlatformHost: "github.com", Archived: true},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": ghMock}, d, nil, repos,
		time.Minute, nil, nil,
	)

	var gotResults []RepoSyncResult
	syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
		gotResults = results
	})

	syncer.RunOnce(t.Context())

	require.Len(gotResults, 1,
		"archived repo should not produce a live sync result")
	assert.Equal("live", gotResults[0].Name)
	assert.True(ghMock.listOpenPRsCalled.Load())
}

func TestRunOnceRestoresLiveSyncForUnarchivedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	// The default mockClient GetRepository reports the repo unarchived,
	// so the tracked archived flag is stale.
	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	repos := []RepoRef{
		{Owner: "acme", Name: "thawed", PlatformHost: "github.com", Archived: true},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": ghMock}, d, nil, repos,
		time.Minute, nil, nil,
	)

	var gotResults []RepoSyncResult
	syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
		gotResults = results
	})

	syncer.RunOnce(t.Context())

	require.Len(gotResults, 1,
		"provider-unarchived repo should rejoin the live pass")
	assert.Equal("thawed", gotResults[0].Name)
	assert.True(ghMock.listOpenPRsCalled.Load())
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.False(tracked[0].Archived,
		"tracked ref should be live again after the provider unarchives")
}

func TestRunOnceDefersArchivedRefreshForThrottledBucket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	var getRepositoryCalled atomic.Bool
	gheMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			getRepositoryCalled.Store(true)
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			archived := false
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}

	gheTracker := NewRateTracker(d, "ghe.corp.com", "host", "rest")
	gheTracker.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: 100, // below RateReserveBuffer (200)
		Reset:     time.Now().Add(30 * time.Minute),
	})

	repos := []RepoRef{
		{Owner: "corp", Name: "frozen", PlatformHost: "ghe.corp.com", Archived: true},
	}
	syncer := NewSyncer(
		map[string]Client{"ghe.corp.com": gheMock}, d, nil, repos,
		time.Minute, map[string]*RateTracker{"ghe.corp.com": gheTracker}, nil,
	)

	syncer.RunOnce(t.Context())

	assert.False(getRepositoryCalled.Load(),
		"archived metadata refresh must not spend budget on a throttled bucket")
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived,
		"deferred archived ref must keep its archived flag")
}

func TestRunOnceAdvancesCadenceForArchivedOnlyBucket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	var getRepositoryCalls atomic.Int32
	gheMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			getRepositoryCalls.Add(1)
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			archived := true
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}

	gheTracker := NewRateTracker(d, "ghe.corp.com", "host", "rest")
	gheTracker.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: 4000,
		Reset:     time.Now().Add(30 * time.Minute),
	})

	repos := []RepoRef{
		{Owner: "corp", Name: "frozen", PlatformHost: "ghe.corp.com", Archived: true},
	}
	syncer := NewSyncer(
		map[string]Client{"ghe.corp.com": gheMock}, d, nil, repos,
		time.Minute, map[string]*RateTracker{"ghe.corp.com": gheTracker}, nil,
	)

	syncer.RunOnce(t.Context())
	require.Equal(int32(1), getRepositoryCalls.Load(),
		"first pass refreshes the archived ref")

	syncer.RunOnce(t.Context())
	assert.Equal(int32(1), getRepositoryCalls.Load(),
		"second pass inside the cadence gate must defer the archived refresh")
}

func TestRunOnceRegistersProviderWorkForArchivedRefresh(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)

	var syncer *Syncer
	var workActive atomic.Bool
	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			workActive.Store(syncer.higherPriorityProviderWorkActive(
				"github.com", archive.PriorityFullArchive,
			))
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			archived := true
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}
	repos := []RepoRef{
		{Owner: "acme", Name: "frozen", PlatformHost: "github.com", Archived: true},
	}
	syncer = NewSyncer(
		map[string]Client{"github.com": ghMock}, d, nil, repos,
		time.Minute, nil, nil,
	)

	syncer.RunOnce(t.Context())

	require.True(workActive.Load(),
		"archived metadata refresh must register provider work so an "+
			"admitted archive lease on the credential is preempted")
}

func TestReconcileRepoIdentityObservationReturnsMidflightArchivedFlip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	// The default mockClient GetRepository reports the repo unarchived.
	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
	}
	// The tracked ref was flipped archived by a concurrent resolution
	// after this operation snapshotted it as live.
	repos := []RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com", Archived: true},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": ghMock}, d, nil, repos,
		time.Minute, nil, nil,
	)

	snapshot := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	authoritative, _, _, _, _, err := syncer.reconcileRepoIdentityObservation(
		t.Context(), snapshot,
	)
	require.NoError(err)

	assert.True(authoritative.Archived,
		"returned ref must carry the newer tracked archived flip the publication preserved")
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
}

func TestPublishResolvedRepositoryEmptySnapshotIDKeepsSuccessorArchivedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	syncer := NewSyncer(nil, d, nil, []RepoRef{
		{
			Owner: "acme", Name: "widget", PlatformHost: "github.com",
			PlatformExternalID: "repo-x", Archived: true,
		},
	}, time.Minute, nil, nil)

	// The operation's snapshot predates identity resolution and carries no
	// provider id; fresh metadata resolves the route to a different
	// repository. The displaced occupant's archived flag must not stamp
	// the successor.
	previous := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	resolved := RepoRef{
		Owner: "acme", Name: "widget", PlatformHost: "github.com",
		PlatformExternalID: "repo-y",
	}
	syncer.publishResolvedRepository(previous, resolved, true)

	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Equal("repo-y", tracked[0].PlatformExternalID)
	assert.False(tracked[0].Archived,
		"authoritative resolved metadata must apply across identities even "+
			"when the snapshot id is empty")
}

func TestDetailDrainSkipsRepoArchivedMidPass(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	// A stale open item queued before the repository archived.
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(
		"github.com", "owner", "repo",
	))
	require.NoError(err)
	seededAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "stale PR",
		Author:          "alice",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123",
		CreatedAt:       seededAt,
		UpdatedAt:       seededAt,
		LastActivityAt:  seededAt,
	})
	require.NoError(err)

	mc := &detailTrackingClient{}
	mc.openPRs = []*gh.PullRequest{}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	archived := true
	mc.getRepositoryFn = func(
		_ context.Context, owner, repo string,
	) (*gh.Repository, error) {
		id := int64(1)
		nodeID := "repo-" + owner + "-" + repo
		return &gh.Repository{
			ID:       &id,
			NodeID:   &nodeID,
			Name:     &repo,
			Owner:    &gh.User{Login: &owner},
			Archived: &archived,
		}, nil
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, testBudget(500),
	)

	syncer.RunOnce(ctx)

	assert.Zero(int(mc.getPRCalls.Load()),
		"detail drain must not fetch details for a repo that archived mid-pass")
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
}

func TestDetailDrainRechecksRemovedUpstreamAfterQueueConstruction(t *testing.T) {
	for _, itemType := range []db.ArchiveItemType{
		db.ArchiveItemTypeMergeRequest,
		db.ArchiveItemTypeIssue,
	} {
		t.Run(string(itemType), func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			d := openTestDB(t)
			now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(
				"github.com", "owner", "repo",
			))
			require.NoError(err)
			pr := buildOpenPR(7, now)
			issue := buildOpenIssue(7, now)
			if itemType == db.ArchiveItemTypeMergeRequest {
				normalized, normalizeErr := NormalizePR(repoID, pr)
				require.NoError(normalizeErr)
				_, err = d.UpsertMergeRequest(ctx, normalized)
			} else {
				normalized, normalizeErr := NormalizeIssue(repoID, issue)
				require.NoError(normalizeErr)
				_, err = d.UpsertIssue(ctx, normalized)
			}
			require.NoError(err)

			var detailCalls atomic.Int32
			client := &mockClient{
				getPullRequestFn: func(
					context.Context, string, string, int,
				) (*gh.PullRequest, error) {
					detailCalls.Add(1)
					return pr, nil
				},
				getIssueFn: func(
					context.Context, string, string, int,
				) (*gh.Issue, error) {
					detailCalls.Add(1)
					return issue, nil
				},
			}
			client.getRepositoryFn = func(
				_ context.Context, owner, name string,
			) (*gh.Repository, error) {
				_, insertErr := d.WriteDB().ExecContext(ctx, `
					INSERT OR IGNORE INTO forge_archive_items (
						repo_id, item_type, item_number, provider_item_id,
						provider_created_at, provider_updated_at, lifecycle_state
					) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
					repoID, itemType, 7, string(itemType)+"-7", now, now,
				)
				require.NoError(insertErr)
				id := int64(1)
				nodeID := "repo-" + owner + "-" + name
				return &gh.Repository{
					ID: &id, NodeID: &nodeID, Name: &name,
					Owner: &gh.User{Login: &owner},
				}, nil
			}
			syncer := NewSyncer(
				map[string]Client{"github.com": client}, d, nil,
				[]RepoRef{{
					Platform: platform.KindGitHub, PlatformHost: "github.com",
					Owner: "owner", Name: "repo",
				}},
				time.Minute, nil, testBudget(500),
			)
			bucket, err := syncer.bucketKeyForRepo(RepoRef{
				Platform: platform.KindGitHub, PlatformHost: "github.com",
				Owner: "owner", Name: "repo",
			}, false)
			require.NoError(err)

			syncer.drainDetailQueue(ctx, map[string]bool{bucket: true}, []RepoRef{{
				Platform: platform.KindGitHub, PlatformHost: "github.com",
				Owner: "owner", Name: "repo",
			}})

			require.Zero(detailCalls.Load(),
				"detail drain must recheck a queued item after repository resolution")
		})
	}
}

func TestSyncWatchedMRsSkipsRepoArchivedMidPass(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	archived := true
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(7, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "acme", Name: "app", PlatformHost: "github.com"}},
		time.Hour, nil, nil,
	)
	hookCalls := 0
	syncer.SetOnMRSynced(func(_ string, _ string, _ *db.MergeRequest) {
		hookCalls++
	})
	syncer.SetWatchedMRs([]WatchedMR{{Owner: "acme", Name: "app", Number: 7}})

	syncer.syncWatchedMRs(ctx)

	assert.Zero(hookCalls,
		"watched-MR sync must stop when resolution reports the repo archived")
	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "app", 7)
	require.NoError(err)
	assert.Nil(mr, "no MR detail should be persisted for the archived repo")
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
}

func TestSyncWatchedMRsSkipsRemovedUpstreamPR(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(
		"github.com", "acme", "app",
	))
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalized, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalized)
	require.NoError(err)
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', ?, ?, ?, ?, 'removed_upstream')`,
		repoID, 7, "pr-7", now, now,
	)
	require.NoError(err)

	client := &detailTrackingClient{}
	client.singlePR = pr
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "app",
		}},
		time.Hour, nil, nil,
	)
	syncer.SetWatchedMRs([]WatchedMR{{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "app", Number: 7,
	}})

	syncer.syncWatchedMRs(ctx)

	require.Zero(client.getPRCalls.Load(),
		"a stale watch entry must not fetch a removed PR")
}

func TestSyncMRForRepoHydratesArchivedRepoUnderArchiveBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	archived := true
	mc := &mockClient{
		openPRs:  []*gh.PullRequest{},
		singlePR: buildOpenPR(7, now),
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}
	repo := RepoRef{
		Owner: "acme", Name: "frozen", PlatformHost: "github.com", Archived: true,
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, []RepoRef{repo},
		time.Hour, nil, nil,
	)

	err := syncer.syncMRForRepo(WithArchiveSyncBudget(ctx), repo, 7, false, nil)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "frozen", 7)
	require.NoError(err)
	require.NotNil(mr,
		"archive hydration must still sync an archived repo's items")
	assert.Equal(7, mr.Number)
}

func TestRunOnceStopsLiveSyncWhenRepoArchivesMidPass(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	archived := true
	ghMock := &mockClient{
		openPRs:  []*gh.PullRequest{},
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			id := int64(1)
			nodeID := "repo-" + owner + "-" + repo
			return &gh.Repository{
				ID:       &id,
				NodeID:   &nodeID,
				Name:     &repo,
				Owner:    &gh.User{Login: &owner},
				Archived: &archived,
			}, nil
		},
	}
	repos := []RepoRef{
		{Owner: "acme", Name: "closing", PlatformHost: "github.com"},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": ghMock}, d, nil, repos,
		time.Minute, nil, nil,
	)

	syncer.RunOnce(t.Context())

	assert.False(ghMock.listOpenPRsCalled.Load(),
		"live item sync must stop once resolution reports the repo archived")
	tracked := syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
}

func TestRepoRefFromCatalogKeepsArchived(t *testing.T) {
	assert := assert.New(t)
	previous := RepoRef{
		Owner: "acme", Name: "frozen",
		PlatformHost: "github.com", Archived: true,
	}
	stored := db.Repo{
		ID: 7, Platform: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "frozen",
	}

	// Catalog republication without a fresh provider resolve keeps the
	// previously known archived flag.
	assert.True(repoRefFromCatalog(previous, stored, nil).Archived)

	// A fresh provider resolve is authoritative in both directions.
	assert.True(repoRefFromCatalog(previous, stored, &platform.Repository{
		Archived: true,
	}).Archived)
	assert.False(repoRefFromCatalog(previous, stored, &platform.Repository{
		Archived: false,
	}).Archived)
}

func TestRepoRefFromCatalogKeepsConfiguredRepoPath(t *testing.T) {
	assert := assert.New(t)
	previous := RepoRef{
		Owner: "acme", Name: "tools-new",
		PlatformHost:       "github.com",
		ConfiguredRepoPath: "acme/tools",
	}
	stored := db.Repo{
		ID: 7, Platform: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "tools-new",
	}

	// Catalog republication must not erase the config-entry provenance
	// that correlates a renamed route with its config entry on reload.
	assert.Equal("acme/tools",
		repoRefFromCatalog(previous, stored, nil).ConfiguredRepoPath)
	assert.Equal("acme/tools",
		repoRefFromCatalog(previous, stored, &platform.Repository{}).ConfiguredRepoPath)
}

func TestPublishResolvedRepositoryPreservesNewerArchivedState(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	tracked := RepoRef{
		Owner: "acme", Name: "frozen", PlatformHost: "github.com",
		RepoPath: "acme/frozen", Archived: true,
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{tracked}, time.Hour, nil, nil,
	)

	// A sync that began before the repo was marked archived publishes a
	// catalog identity built from its stale snapshot. Without fresh
	// provider metadata the currently tracked archived flag must survive.
	stale := tracked
	stale.Archived = false
	syncer.publishResolvedRepository(stale, stale, false)
	assert.True(syncer.TrackedRepos()[0].Archived,
		"a stale catalog republication must not clear archived state")

	// Fresh provider metadata is authoritative in both directions.
	unarchived := tracked
	unarchived.Archived = false
	syncer.publishResolvedRepository(tracked, unarchived, true)
	assert.False(syncer.TrackedRepos()[0].Archived)
}

func TestPublishResolvedRepositoryPreservesMidflightArchivedFlip(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	tracked := RepoRef{
		Owner: "acme", Name: "frozen", PlatformHost: "github.com",
		RepoPath: "acme/frozen", Archived: true,
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{tracked}, time.Hour, nil, nil,
	)

	// The operation snapshotted the ref before a concurrent resolution
	// flipped the tracked archived flag. Its own provider response cannot
	// be ordered against that flip, so the newer tracked value survives
	// even a publication carrying fresh provider metadata.
	stale := tracked
	stale.Archived = false
	syncer.publishResolvedRepository(stale, stale, true)
	assert.True(syncer.TrackedRepos()[0].Archived,
		"a mid-flight archived flip must not be clobbered by an older publication")
}

func TestPublishResolvedRepositoryMatchesByStableIdentityFirst(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	tracked := RepoRef{
		Owner: "acme", Name: "tools-new", PlatformHost: "github.com",
		RepoPath: "acme/tools-new", PlatformExternalID: "repo-x",
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{tracked}, time.Hour, nil, nil,
	)

	// A slower operation publishes with a snapshot bearing the old route
	// after the tracked ref already moved to the renamed one. The stable
	// provider id still locates the tracked entry.
	previous := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-x",
	}
	resolved := tracked
	resolved.DefaultBranch = "main"
	syncer.publishResolvedRepository(previous, resolved, true)
	assert.Equal("main", syncer.TrackedRepos()[0].DefaultBranch,
		"identity match must publish onto the renamed tracked entry")
}

func TestPublishResolvedRepositoryDoesNotOverwriteRouteSuccessor(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	successor := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-y",
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{successor}, time.Hour, nil, nil,
	)

	// A stale sync of the renamed-away repository publishes with the old
	// route while a different repository now occupies it. Conflicting
	// stable ids mean the route match is reuse, not a rename: the
	// successor's tracked state must survive.
	previous := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-x",
	}
	resolved := previous
	resolved.Archived = true
	syncer.publishResolvedRepository(previous, resolved, true)
	got := syncer.TrackedRepos()[0]
	assert.Equal("repo-y", got.PlatformExternalID,
		"a stale publication must not overwrite the route successor")
	assert.False(got.Archived)
}

func TestPublishResolvedRepositoryLandsCrossIdentityLookupOnSuccessor(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	renamed := RepoRef{
		Owner: "acme", Name: "tools-new", PlatformHost: "github.com",
		RepoPath: "acme/tools-new", PlatformExternalID: "repo-x",
	}
	successor := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-y",
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{renamed, successor}, time.Hour, nil, nil,
	)

	// An in-flight lookup keyed by the old route resolved the successor:
	// the snapshot carries the renamed repo's id, the response the
	// successor's. The publication belongs to the successor — it must not
	// overwrite the renamed repository's entry.
	previous := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-x",
	}
	resolved := successor
	resolved.DefaultBranch = "main"
	syncer.publishResolvedRepository(previous, resolved, true)

	byID := make(map[string]RepoRef)
	for _, repo := range syncer.TrackedRepos() {
		byID[repo.PlatformExternalID] = repo
	}
	assert.Len(byID, 2, "the renamed repository must not be overwritten")
	assert.Equal("tools-new", byID["repo-x"].Name)
	assert.Equal("main", byID["repo-y"].DefaultBranch,
		"the publication lands on the repository the provider identified")
}

func TestPublishResolvedRepositoryCrossIdentityUsesAuthoritativeArchived(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	renamed := RepoRef{
		Owner: "acme", Name: "tools-new", PlatformHost: "github.com",
		RepoPath: "acme/tools-new", PlatformExternalID: "repo-x",
	}
	successor := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-y", Archived: true,
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil,
		[]RepoRef{renamed, successor}, time.Hour, nil, nil,
	)

	// The publication lands on the successor, but the snapshot belongs to
	// the renamed repository — its archived flag says nothing about the
	// successor, so authoritative resolved metadata applies.
	previous := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-x",
	}
	resolved := successor
	resolved.Archived = false
	syncer.publishResolvedRepository(previous, resolved, true)
	for _, repo := range syncer.TrackedRepos() {
		if repo.PlatformExternalID == "repo-y" {
			assert.False(repo.Archived,
				"authoritative metadata must apply on a cross-identity landing")
		}
	}

	// Without fresh provider metadata the successor's tracked flag stands.
	stale := resolved
	stale.Archived = true
	syncer.publishResolvedRepository(previous, stale, false)
	for _, repo := range syncer.TrackedRepos() {
		if repo.PlatformExternalID == "repo-y" {
			assert.False(repo.Archived,
				"non-authoritative publication must preserve the successor's state")
		}
	}
}

func TestPublishResolvedRepositoryReplacementIgnoresDisplacedArchivedFlip(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	// The configured route's occupant was archived after this operation
	// snapshotted it; meanwhile the route now resolves to an untracked
	// replacement repository. The displaced repo's archived flip says
	// nothing about the replacement — authoritative metadata applies.
	displaced := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-x", Archived: true,
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{displaced}, time.Hour, nil, nil,
	)

	previous := displaced
	previous.Archived = false
	replacement := RepoRef{
		Owner: "acme", Name: "tools", PlatformHost: "github.com",
		RepoPath: "acme/tools", PlatformExternalID: "repo-y", Archived: false,
	}
	syncer.publishResolvedRepository(previous, replacement, true)

	got := syncer.TrackedRepos()[0]
	assert.Equal("repo-y", got.PlatformExternalID)
	assert.False(got.Archived,
		"the displaced repo's archived flip must not stamp the replacement")
}

func TestPublishResolvedRepositoryPreservesNewerConfiguredRepoPath(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	tracked := RepoRef{
		Owner: "acme", Name: "frozen", PlatformHost: "github.com",
		RepoPath: "acme/frozen", ConfiguredRepoPath: "acme/frozen",
	}
	syncer := NewSyncer(
		map[string]Client{}, d, nil, []RepoRef{tracked}, time.Hour, nil, nil,
	)

	// A sync snapshot taken before a config reload rewired the entry
	// carries stale provenance. Provider resolution never authors
	// config-entry provenance, so the currently tracked value survives
	// even a publication with fresh provider metadata.
	stale := tracked
	stale.ConfiguredRepoPath = ""
	syncer.publishResolvedRepository(stale, stale, true)
	assert.Equal("acme/frozen", syncer.TrackedRepos()[0].ConfiguredRepoPath,
		"a stale republication must not erase config-entry provenance")
}

func TestRunOnceScopesGitHubProviderReserveToRepoCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	now := time.Now().UTC()
	var ownersMu sync.Mutex
	var listedOwners []string
	base := &mockClient{}
	base.listOpenPRsFn = func(
		_ context.Context, owner, _ string,
	) ([]*gh.PullRequest, error) {
		ownersMu.Lock()
		listedOwners = append(listedOwners, owner)
		ownersMu.Unlock()
		return nil, nil
	}
	// The App installation is parked at its REST reserve; the user credential
	// is healthy. Only the repository routed to the user may sync.
	appClient := &credentialRateLimitSnapshotMockClient{
		mockClient: base,
		appSnapshot: &RateLimitSnapshot{
			Core:    &Rate{Limit: 15000, Remaining: RateReserveBuffer, Reset: now.Add(time.Hour)},
			GraphQL: &Rate{Limit: 10000, Remaining: 9000, Reset: now.Add(time.Hour)},
		},
	}
	userClient := &credentialRateLimitSnapshotMockClient{
		mockClient: base,
		appSnapshot: &RateLimitSnapshot{
			Core:    &Rate{Limit: 5000, Remaining: 4900, Reset: now.Add(time.Hour)},
			GraphQL: &Rate{Limit: 5000, Remaining: 4800, Reset: now.Add(time.Hour)},
		},
	}
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:42"}
	userIdentity := IdentityKey{Host: "github.com", Principal: "user:7"}
	appBucket := RateBucketKey("github", "github.com", "installation:42")
	userBucket := RateBucketKey("github", "github.com", "user:7")
	syncer := NewSyncer(
		map[string]Client{"github.com": appClient}, database, nil,
		[]RepoRef{
			{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
			{Owner: "other", Name: "tool", PlatformHost: "github.com"},
		},
		time.Minute,
		map[string]*RateTracker{
			appBucket:  NewRateTracker(database, "github.com", "installation:42", "rest"),
			userBucket: NewRateTracker(database, "github.com", "user:7", "rest"),
		},
		nil,
	)
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: appClient,
			ReadIdentity: appIdentity, WriteIdentity: appIdentity,
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: userClient,
			ReadIdentity: userIdentity, WriteIdentity: userIdentity,
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	syncer.SetQuotaRegistry(NewQuotaRegistry())
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(t.Context())

	require.Len(results, 2)
	assert.Equal("skipped: rate limit throttled", results[0].Error)
	assert.Empty(results[1].Error)
	ownersMu.Lock()
	assert.Equal([]string{"other"}, listedOwners)
	ownersMu.Unlock()
}

// ignoresCancelClient embeds mockClient and triggers an outer
// cancel() on the first ListOpenIssues call while still returning
// (nil, nil) successfully. This simulates a Client implementation
// that ignores ctx cancellation mid-call -- the defensive case
// the RunOnce cancel latch must handle.
type ignoresCancelClient struct {
	mockClient
	cancel context.CancelFunc
	once   sync.Once
}

func (c *ignoresCancelClient) ListOpenIssues(
	_ context.Context, _, _ string,
) ([]*gh.Issue, error) {
	c.once.Do(c.cancel)
	return nil, nil
}

// TestRunOnceLatchesCancelWhenSyncRepoIgnoresCtx covers the
// defense-in-depth gap flagged on commit 45a5421: if a Client
// ignores ctx cancellation mid-sync and every call still returns
// success, syncRepo will return nil after ctx has been canceled.
// Under the old completed-count heuristic (`completed < total`)
// the run was misreported as a clean completion -- onSyncCompleted
// fired even though the user had asked to cancel. The latched
// cancel flag must catch this case and route through the cancel
// status path instead.
func TestRunOnceLatchesCancelWhenSyncRepoIgnoresCtx(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	c := &ignoresCancelClient{cancel: cancel}

	syncer := NewSyncer(
		map[string]Client{"github.com": c}, d, nil,
		[]RepoRef{{Owner: "o", Name: "r", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	var syncCompletedCalls atomic.Int32
	syncer.SetOnSyncCompleted(func(_ []RepoSyncResult) {
		syncCompletedCalls.Add(1)
	})

	syncer.RunOnce(ctx)

	assert.Zero(int(syncCompletedCalls.Load()),
		"onSyncCompleted must not fire when ctx was canceled "+
			"during the run, even if syncRepo returned success")
	status := syncer.Status()
	assert.False(status.Running, "sync must stop")
	assert.NotEmpty(status.LastError,
		"status must record the cancel as an error")
}

// --- Index/Detail Split Tests ---

// detailTrackingClient tracks which API methods are called so tests
// can verify that the index phase does NOT call GetPullRequest while
// the detail drain does.
type detailTrackingClient struct {
	mockClient
	getPRCalls atomic.Int32
}

func (c *detailTrackingClient) GetPullRequest(
	ctx context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	c.trackCall()
	c.getPRCalls.Add(1)
	return c.mockClient.GetPullRequest(ctx, owner, repo, number)
}

type conditionalPRTrackingClient struct {
	detailTrackingClient
	receivedETag     string
	conditionalCalls atomic.Int32
	notModified      bool
	nextETag         string
	beforeReturn     func()
}

func (c *conditionalPRTrackingClient) GetPullRequestIfChanged(
	ctx context.Context,
	owner, repo string,
	number int,
	etag string,
) (*gh.PullRequest, string, bool, error) {
	c.conditionalCalls.Add(1)
	c.receivedETag = etag
	if c.beforeReturn != nil {
		c.beforeReturn()
	}
	if c.notModified {
		return nil, etag, true, nil
	}
	c.getPRCalls.Add(1)
	pr, err := c.mockClient.GetPullRequest(ctx, owner, repo, number)
	return pr, c.nextETag, false, err
}

type conditionalIssueTrackingClient struct {
	mockClient
	receivedETag     string
	conditionalCalls atomic.Int32
	notModified      bool
	nextETag         string
	beforeReturn     func()
}

type conditionalIssueLifecycleClient struct {
	conditionalIssueTrackingClient
	unconditionalCalls atomic.Int32
	timelineCalls      atomic.Int32
	timelineEvents     []PullRequestTimelineEvent
}

func (c *conditionalIssueLifecycleClient) GetIssue(
	ctx context.Context,
	owner, repo string,
	number int,
) (*gh.Issue, error) {
	c.unconditionalCalls.Add(1)
	return c.mockClient.GetIssue(ctx, owner, repo, number)
}

func (c *conditionalIssueLifecycleClient) ListIssueTimelineEvents(
	context.Context, string, string, int,
) ([]PullRequestTimelineEvent, error) {
	c.timelineCalls.Add(1)
	return c.timelineEvents, nil
}

func (c *conditionalIssueTrackingClient) GetIssueIfChanged(
	ctx context.Context,
	owner, repo string,
	number int,
	etag string,
) (*gh.Issue, string, bool, error) {
	c.conditionalCalls.Add(1)
	c.receivedETag = etag
	if c.beforeReturn != nil {
		c.beforeReturn()
	}
	if c.notModified {
		return nil, etag, true, nil
	}
	issue, err := c.GetIssue(ctx, owner, repo, number)
	return issue, c.nextETag, false, err
}

func TestRunOnceIndexOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &detailTrackingClient{}
	mc.openPRs = []*gh.PullRequest{
		buildOpenPR(1, now),
		buildOpenPR(2, now),
	}

	// Budget=0 disables detail drain entirely.
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "owner", Name: "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, nil,
	)

	syncer.RunOnce(ctx)

	// ListOpenPullRequests should have been called.
	assert.True(mc.listOpenPRsCalled.Load(),
		"index scan should call ListOpenPullRequests")

	// GetPullRequest should NOT have been called (no detail fetch).
	assert.Zero(int(mc.getPRCalls.Load()),
		"index-only sync should not call GetPullRequest")

	// PRs should be in DB with nil detail_fetched_at.
	pr1, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr1)
	assert.Equal(1, pr1.Number)
	assert.Nil(pr1.DetailFetchedAt,
		"detail_fetched_at should be nil after index-only sync")

	pr2, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 2)
	require.NoError(err)
	require.NotNil(pr2)
	assert.Equal(2, pr2.Number)
	assert.Nil(pr2.DetailFetchedAt,
		"detail_fetched_at should be nil after index-only sync")

	// No timeline events should exist (no detail fetch).
	events, err := d.ListMREvents(ctx, pr1.ID)
	require.NoError(err)
	assert.Empty(events,
		"no events should exist after index-only sync")
}

func TestRunOnceDetailDrain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ciState := "success"

	mc := &detailTrackingClient{}
	mc.openPRs = []*gh.PullRequest{
		buildOpenPR(1, now),
		buildOpenPR(2, now),
	}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: &ciState}

	// Budget=500 allows detail drain to run.
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "owner", Name: "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, testBudget(500),
	)

	syncer.RunOnce(ctx)

	// GetPullRequest should have been called for each PR
	// during detail drain.
	assert.GreaterOrEqual(int(mc.getPRCalls.Load()), 2,
		"detail drain should call GetPullRequest for open PRs")

	// Both PRs should have detail_fetched_at set.
	pr1, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr1)
	assert.NotNil(pr1.DetailFetchedAt,
		"detail_fetched_at should be set after detail drain")

	pr2, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 2)
	require.NoError(err)
	require.NotNil(pr2)
	assert.NotNil(pr2.DetailFetchedAt,
		"detail_fetched_at should be set after detail drain")
}

func TestFetchMRDetailRefreshesCommentVisibilityOnParent304(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	detailFetchedAt := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           "open",
		HeadBranch:      "feature-branch",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123def456",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	commentID := int64(10401)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &commentID,
		EventType:      "issue_comment",
		Body:           "moderated without changing the parent",
		CreatedAt:      updatedAt,
		DedupeKey:      "comment-10401",
	}}))
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	mc := &conditionalPRTrackingClient{notModified: true}
	mc.comments = []*gh.IssueComment{{ID: new(int64)}}
	mc.reviews = []*gh.PullRequestReview{{ID: new(int64)}}
	mc.commits = []*gh.RepositoryCommit{{SHA: new(string)}}
	inlineCommentID := int64(10402)
	inlineLine := 12
	mc.reviewThreads = []PullRequestReviewThread{{
		NodeID: "PRRT_10402",
		Path:   "src/main.go",
		Side:   "RIGHT",
		Line:   inlineLine,
		Comments: []PullRequestReviewThreadComment{{
			NodeID:          "PRRC_10402",
			DatabaseID:      inlineCommentID,
			Body:            "inline moderation changed without changing the parent",
			AuthorLogin:     "reviewer",
			CommitID:        "abc123def456",
			IsMinimized:     true,
			MinimizedReason: "ABUSE",
			CreatedAt:       updatedAt,
			UpdatedAt:       updatedAt,
		}},
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)
	gqlSrv := currentCommentVisibilityServer(
		t, "pullRequest", commentID,
		CommentVisibility{Hidden: true, Reason: "ABUSE"},
		CommentVisibility{},
	)
	defer gqlSrv.Close()
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
		),
	})

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.NoError(err)
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 2)
	metadataByType := make(map[string]string, len(events))
	for _, event := range events {
		metadataByType[event.EventType] = event.MetadataJSON
	}
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, metadataByType["issue_comment"])
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, metadataByType["review_comment"])
	threads, err := d.ListMRReviewThreads(ctx, mrID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, threads[0].MetadataJSON)

	mc.reviewThreads[0].Comments[0].IsMinimized = false
	mc.reviewThreads[0].Comments[0].MinimizedReason = ""
	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.NoError(err)
	events, err = d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 2)
	for _, event := range events {
		assert.Empty(event.MetadataJSON)
	}
	threads, err = d.ListMRReviewThreads(ctx, mrID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Empty(threads[0].MetadataJSON)
	for _, event := range events {
		if event.EventType == "issue_comment" {
			assert.Equal("moderated without changing the parent", event.Body)
		}
	}

	assert.Equal(int32(2), mc.conditionalCalls.Load())
	assert.Equal(`"etag-v1"`, mc.receivedETag)
	assert.Zero(int(mc.getPRCalls.Load()),
		"304 should skip the unconditional PR detail fetch")
	assert.Zero(int(mc.listIssueCommentsCalled.Load()),
		"304 should skip timeline/comment refresh")
}

func TestFetchMRDetailDoesNotBackfillMergedActorOn304(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	mergedAt := updatedAt.Add(time.Minute)
	detailFetchedAt := updatedAt.Add(-time.Hour)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           db.MergeRequestStateMerged,
		HeadBranch:      "feature-branch",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123def456",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		MergedAt:        &mergedAt,
		ClosedAt:        &mergedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	mc := &conditionalPRTrackingClient{
		timelineEventsErr: errors.New("304 detail path must not fetch timeline events"),
		notModified:       true,
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	calls, err := syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.NoError(err)

	assert.Equal(1, calls)
	assert.Equal(int32(1), mc.conditionalCalls.Load())
	assert.Equal(`"etag-v1"`, mc.receivedETag)
	assert.Zero(int(mc.getPRCalls.Load()),
		"304 should skip the unconditional PR detail fetch")
	assert.Zero(int(mc.listIssueCommentsCalled.Load()),
		"304 should skip timeline/comment refresh")
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	assert.Empty(events)
	fresh, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(fresh)
	require.NotNil(fresh.DetailFetchedAt)
	assert.True(fresh.DetailFetchedAt.After(detailFetchedAt))
}

func TestWatchedSyncMRUsesPersistedPullRequestETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	detailFetchedAt := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           "open",
		HeadBranch:      "feature-branch",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123def456",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	mc := &conditionalPRTrackingClient{notModified: true}
	mc.singlePR = buildOpenPR(1, updatedAt)
	mc.comments = []*gh.IssueComment{{ID: new(int64)}}
	mc.reviews = []*gh.PullRequestReview{{ID: new(int64)}}
	mc.commits = []*gh.RepositoryCommit{{SHA: new(string)}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	require.NoError(syncer.syncMRWithWatchedRef(ctx, WatchedMR{
		Owner: "owner", Name: "repo", Number: 1, PlatformHost: "github.com",
	}))

	assert.Equal(int32(1), mc.conditionalCalls.Load())
	assert.Equal(`"etag-v1"`, mc.receivedETag)
	assert.Zero(int(mc.getPRCalls.Load()),
		"304 should skip the unconditional PR detail fetch")
	assert.Zero(int(mc.listIssueCommentsCalled.Load()),
		"304 should skip timeline/comment refresh")
}

func TestWatchedSyncMRDoesNotBackfillMergedActorOn304(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	mergedAt := updatedAt.Add(time.Minute)
	detailFetchedAt := updatedAt.Add(-time.Hour)
	mrID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           db.MergeRequestStateMerged,
		HeadBranch:      "feature-branch",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123def456",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		MergedAt:        &mergedAt,
		ClosedAt:        &mergedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	mc := &conditionalPRTrackingClient{
		timelineEventsErr: errors.New("304 watched sync must not fetch timeline events"),
		notModified:       true,
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	require.NoError(syncer.syncMRWithWatchedRef(ctx, WatchedMR{
		Owner: "owner", Name: "repo", Number: 1, PlatformHost: "github.com",
	}))

	assert.Equal(int32(1), mc.conditionalCalls.Load())
	assert.Equal(`"etag-v1"`, mc.receivedETag)
	assert.Zero(int(mc.getPRCalls.Load()),
		"304 should skip the unconditional PR detail fetch")
	assert.Zero(int(mc.listIssueCommentsCalled.Load()),
		"304 should skip timeline/comment refresh")
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	assert.Empty(events)
	fresh, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(fresh)
	require.NotNil(fresh.DetailFetchedAt)
	assert.True(fresh.DetailFetchedAt.After(detailFetchedAt))
}

func TestSyncMRBypassesPersistedPullRequestETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	detailFetchedAt := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 1000, Number: 1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           "open",
		HeadBranch:      "feature-branch",
		BaseBranch:      "main",
		PlatformHeadSHA: "abc123def456",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	ciState := "success"
	mc := &conditionalPRTrackingClient{notModified: true}
	mc.singlePR = buildOpenPR(1, updatedAt)
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: &ciState}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	require.NoError(syncer.SyncMR(ctx, "owner", "repo", 1))

	assert.Zero(int(mc.conditionalCalls.Load()))
	assert.Equal(1, int(mc.getPRCalls.Load()))
	assert.Equal(int32(1), mc.listIssueCommentsCalled.Load(),
		"manual SyncMR should refresh timeline/comments")
}

func TestFetchMRDetailPersistsPullRequestETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	mc := &conditionalPRTrackingClient{nextETag: `"etag-v2"`}
	mc.singlePR = buildOpenPR(1, updatedAt)
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: new(string)}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.NoError(err)

	etag, err := d.GetHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1,
	)
	require.NoError(err)
	assert.Equal(`"etag-v2"`, etag)
}

func TestFetchMRDetailDoesNotPersistPullRequestETagWhenDetailRefreshFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1, `"etag-v1"`,
	))

	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	mc := &conditionalPRTrackingClient{nextETag: `"etag-v2"`}
	mc.singlePR = buildOpenPR(1, updatedAt)
	mc.listIssueCommentsErr = fmt.Errorf("transient comments failure")
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.Error(err)

	etag, err := d.GetHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 1,
	)
	require.NoError(err)
	assert.Equal(`"etag-v1"`, etag)
}

func TestFetchIssueDetailRefreshesCommentVisibilityOnParent304(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	detailFetchedAt := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	issueID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/issues/1",
		Title:           "test issue",
		Author:          "alice",
		State:           "open",
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
		LastActivityAt:  updatedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	commentID := int64(10801)
	require.NoError(d.UpsertIssueEvents(ctx, []db.IssueEvent{{
		IssueID:    issueID,
		PlatformID: &commentID,
		EventType:  "issue_comment",
		Body:       "moderated without changing the parent",
		CreatedAt:  updatedAt,
		DedupeKey:  "issue-comment-10801",
	}}))
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"issue", 1, `"issue-etag-v1"`,
	))

	mc := &conditionalIssueTrackingClient{notModified: true}
	mc.comments = []*gh.IssueComment{{ID: new(int64)}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)
	gqlSrv := currentCommentVisibilityServer(
		t, "issue", commentID,
		CommentVisibility{Hidden: true, Reason: "ABUSE"},
		CommentVisibility{},
	)
	defer gqlSrv.Close()
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
		),
	})

	_, err = syncer.fetchIssueDetail(ctx, repo, repoID, 1)
	require.NoError(err)
	events, err := d.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, events[0].MetadataJSON)

	_, err = syncer.fetchIssueDetail(ctx, repo, repoID, 1)
	require.NoError(err)
	events, err = d.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Empty(events[0].MetadataJSON)
	assert.Equal("moderated without changing the parent", events[0].Body)

	assert.Equal(int32(2), mc.conditionalCalls.Load())
	assert.Equal(`"issue-etag-v1"`, mc.receivedETag)
	assert.Zero(int(mc.listIssueCommentsCalled.Load()),
		"304 should skip issue comment refresh")
}

func TestSyncArchiveIssueBypassesPersistedETagForLifecycleBackfill(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	closedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	issue := buildOpenIssue(7, closedAt)
	closedState := "closed"
	issue.State = &closedState
	issue.ClosedAt = makeTimestamp(closedAt)
	normalized, err := NormalizeIssue(repoID, issue)
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, normalized)
	require.NoError(err)
	require.NoError(database.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"issue", 7, `"legacy-etag"`,
	))

	client := &conditionalIssueLifecycleClient{
		getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			return issue, nil
		},
		comments:    []*gh.IssueComment{},
		notModified: true,
		timelineEvents: []PullRequestTimelineEvent{{
			NodeID: "closed-7", EventType: "closed",
			Actor: "closer", CreatedAt: closedAt,
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx),
		platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: repo.Owner, Name: repo.Name,
		},
		db.ArchiveItemTypeIssue, 7,
	)
	require.NoError(err)
	assert.True(result.ProviderAttempted)
	assert.Zero(int(client.conditionalCalls.Load()))
	assert.Equal(int32(1), client.unconditionalCalls.Load())
	assert.Equal(int32(1), client.timelineCalls.Load())

	stored, err := database.GetIssueByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	events, err := database.ListIssueEvents(ctx, stored.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("closed", events[0].EventType)
	assert.Equal("closer", events[0].Author)
}

func TestSyncArchiveMRRepairsMetricsFromMergedAtOnlyRejectedSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, Owner: "owner", Name: "repo",
		PlatformHost: "github.com",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	canonicalUpdatedAt := time.Date(2026, 7, 28, 0, 41, 21, 0, time.UTC)
	localUpdatedAt := canonicalUpdatedAt.Add(835 * time.Millisecond)
	mergedAt := canonicalUpdatedAt.Add(-time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7000, Number: 7, Title: "newer local title",
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "pre-merge-test-sha",
		CreatedAt:      canonicalUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)
	before, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(before)

	canonical := buildOpenPR(7, canonicalUpdatedAt)
	closed := "closed"
	mergeSHA := "merge-sha"
	filesChanged := 4
	canonical.State = &closed
	canonical.Merged = nil
	canonical.MergedAt = makeTimestamp(mergedAt)
	canonical.ClosedAt = makeTimestamp(mergedAt)
	canonical.MergeCommitSHA = &mergeSHA
	canonical.ChangedFiles = &filesChanged
	canonical.Head.SHA = new("head-sha")
	canonical.MergedBy = &gh.User{Login: new("merge-admin")}
	client := &mockClient{singlePR: canonical}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)
	var raceStarted atomic.Bool
	writeLockAttempted := make(chan struct{})
	var writeLockAttemptedOnce sync.Once
	restoreWriteLockHook := database.SetBeforeRepositoryReconciliationWriteLockForTest(func() {
		if raceStarted.Load() {
			writeLockAttemptedOnce.Do(func() { close(writeLockAttempted) })
		}
	})
	t.Cleanup(restoreWriteLockHook)
	reconciliationDone := make(chan error, 1)
	syncer.afterMergedMRMetricsRepair = func() {
		raceStarted.Store(true)
		go func() {
			_, _, reconcileErr := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
				Platform: "github", PlatformHost: "github.com",
				PlatformRepoID: "repo-owner-repo", Owner: "owner", Name: "renamed",
				RepoPath: "owner/renamed",
			}, time.Now().UTC().Add(time.Hour))
			reconciliationDone <- reconcileErr
		}()
		<-writeLockAttempted
	}

	require.NoError(syncer.syncMRForRepo(
		WithArchiveSyncBudget(ctx), repo, 7, false, nil,
	))
	require.NoError(<-reconciliationDone)
	after, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal("merge-sha", after.MergeCommitSHA)
	require.NotNil(after.FilesChanged)
	assert.Equal(4, *after.FilesChanged)
	assert.Equal("newer local title", after.Title)
	assert.Equal(before.UpdatedAt, after.UpdatedAt)
	assert.Equal(before.SnapshotRevision, after.SnapshotRevision)
	events, err := database.ListMREvents(ctx, after.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merge-admin", events[0].Author)
}

func TestSyncArchiveMRRetriesWhenMergedResponseCannotRepairNewerOpenSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, Owner: "owner", Name: "repo",
		PlatformHost: "github.com",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	providerUpdatedAt := time.Date(2026, 7, 28, 0, 41, 21, 0, time.UTC)
	localUpdatedAt := providerUpdatedAt.Add(835 * time.Millisecond)
	mergedAt := providerUpdatedAt.Add(-time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7000, Number: 7,
		State: db.MergeRequestStateOpen, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "pre-merge-test-sha",
		CreatedAt:      providerUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt,
	})
	require.NoError(err)

	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.State = new("closed")
	canonical.Merged = new(true)
	canonical.MergedAt = makeTimestamp(mergedAt)
	canonical.ClosedAt = makeTimestamp(mergedAt)
	canonical.MergeCommitSHA = new("merge-sha")
	canonical.ChangedFiles = new(4)
	canonical.Head.SHA = new("head-sha")
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{singlePR: canonical}},
		database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx), platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: repo.Owner, Name: repo.Name,
		}, db.ArchiveItemTypeMergeRequest, 7,
	)
	require.True(result.ProviderAttempted)
	require.ErrorContains(err, "merge_state")

	after, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(db.MergeRequestStateOpen, after.State)
	assert.Nil(after.MergedAt)
	assert.Equal("pre-merge-test-sha", after.MergeCommitSHA)
	assert.Nil(after.FilesChanged)
	events, err := database.ListMREvents(ctx, after.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestSyncArchiveMRChecksMetricsByResolvedRepositoryIDAfterRename(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a",
	}
	entry, accepted, err := database.ReconcileRepositoryObservation(
		ctx,
		db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		},
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.True(accepted)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	canonical := buildOpenPR(7, now)
	canonical.State = new("closed")
	canonical.Merged = new(true)
	canonical.MergedAt = makeTimestamp(now)
	canonical.ClosedAt = makeTimestamp(now)
	canonical.MergeCommitSHA = new("merge-sha")
	canonical.ChangedFiles = new(4)
	client := &mockClient{
		singlePR: canonical,
		getRepositoryFn: func(
			context.Context, string, string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-a"), Name: new("renamed"),
				Owner: &gh.User{Login: new("acme")}, Archived: new(false),
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	_, err = syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx), platformRepoRef(repo),
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.NoError(err)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, entry.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(mr)
	require.Equal("merge-sha", mr.MergeCommitSHA)
	require.Equal(4, *mr.FilesChanged)
}

func TestSyncArchiveMRPreservesStoredMergedAtWhenAcceptedResponseOmitsIt(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a",
	}
	entry, accepted, err := database.ReconcileRepositoryObservation(
		ctx,
		db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		},
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.True(accepted)

	mergedAt := time.Date(2026, 8, 2, 11, 59, 59, 0, time.UTC)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: entry.Repository.ID, PlatformID: 7, Number: 7,
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "merge-sha", FilesChanged: new(4),
		CreatedAt: mergedAt.Add(-time.Hour), UpdatedAt: mergedAt,
		LastActivityAt: mergedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)

	providerUpdatedAt := mergedAt.Add(time.Minute)
	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.State = new("closed")
	canonical.Merged = new(true)
	canonical.MergedAt = nil
	canonical.ClosedAt = makeTimestamp(mergedAt)
	canonical.MergeCommitSHA = new("merge-sha")
	canonical.ChangedFiles = new(4)
	canonical.Head.SHA = new("head-sha")
	canonical.MergedBy = &gh.User{Login: new("merge-admin")}
	client := &mockClient{
		singlePR: canonical,
		getRepositoryFn: func(
			context.Context, string, string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-a"), Name: new("widget"),
				Owner: &gh.User{Login: new("acme")}, Archived: new(false),
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx), platformRepoRef(repo),
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.True(result.ProviderAttempted)
	require.NoError(err)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, entry.Repository.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	require.NotNil(mr.MergedAt)
	require.Equal(mergedAt, *mr.MergedAt)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal("merge-admin", events[0].Author)
}

func TestSyncArchiveMRRetriesWhenAcceptedSnapshotLosesRouteFenceBeforeActor(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a",
	}
	entry, accepted, err := database.ReconcileRepositoryObservation(
		ctx,
		db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		},
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.True(accepted)

	mergedAt := time.Date(2026, 8, 2, 11, 59, 59, 0, time.UTC)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: entry.Repository.ID, PlatformID: 7, Number: 7,
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "merge-sha", FilesChanged: new(4),
		CreatedAt: mergedAt.Add(-time.Hour), UpdatedAt: mergedAt,
		LastActivityAt: mergedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)

	providerUpdatedAt := mergedAt.Add(time.Minute)
	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.State = new("closed")
	canonical.Merged = new(true)
	canonical.MergedAt = makeTimestamp(mergedAt)
	canonical.ClosedAt = makeTimestamp(mergedAt)
	canonical.MergeCommitSHA = new("merge-sha")
	canonical.ChangedFiles = new(4)
	canonical.Head.SHA = new("head-sha")
	canonical.MergedBy = &gh.User{Login: new("merge-admin")}
	client := &mockClient{
		singlePR: canonical,
		getRepositoryFn: func(
			context.Context, string, string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-a"), Name: new("widget"),
				Owner: &gh.User{Login: new("acme")}, Archived: new(false),
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	var raceStarted atomic.Bool
	writeLockAttempted := make(chan struct{})
	var writeLockAttemptedOnce sync.Once
	restoreWriteLockHook := database.SetBeforeRepositoryReconciliationWriteLockForTest(func() {
		if raceStarted.Load() {
			writeLockAttemptedOnce.Do(func() { close(writeLockAttempted) })
		}
	})
	t.Cleanup(restoreWriteLockHook)
	reconciliationDone := make(chan error, 1)
	syncer.afterMergeRequestParentSnapshotCommit = func() {
		raceStarted.Store(true)
		go func() {
			_, _, reconcileErr := database.ReconcileRepositoryObservation(
				ctx,
				db.RepoIdentity{
					Platform: "github", PlatformHost: "github.com",
					PlatformRepoID: "repo-a", Owner: "acme", Name: "renamed",
					RepoPath: "acme/renamed",
				},
				time.Now().UTC().Add(time.Hour),
			)
			reconciliationDone <- reconcileErr
		}()
		<-writeLockAttempted
	}

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx), platformRepoRef(repo),
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.True(result.ProviderAttempted)
	require.ErrorContains(err, "lifecycle persistence")
	require.NoError(<-reconciliationDone)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, entry.Repository.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Empty(events)
}

func TestSyncArchiveMRRetriesWhenRejectedRepairLosesRouteFence(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		PlatformExternalID: "repo-a",
	}
	entry, accepted, err := database.ReconcileRepositoryObservation(
		ctx,
		db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		},
		time.Now().UTC().Add(-time.Hour),
	)
	require.NoError(err)
	require.True(accepted)

	providerUpdatedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	localUpdatedAt := providerUpdatedAt.Add(835 * time.Millisecond)
	mergedAt := providerUpdatedAt.Add(-time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: entry.Repository.ID, PlatformID: 7, Number: 7,
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "merge-sha", FilesChanged: new(4),
		CreatedAt: providerUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)

	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.State = new("closed")
	canonical.Merged = new(true)
	canonical.MergedAt = makeTimestamp(mergedAt)
	canonical.ClosedAt = makeTimestamp(mergedAt)
	canonical.MergeCommitSHA = new("merge-sha")
	canonical.ChangedFiles = nil
	canonical.Head.SHA = new("head-sha")
	canonical.MergedBy = &gh.User{Login: new("merge-admin")}
	client := &mockClient{
		getRepositoryFn: func(
			context.Context, string, string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-a"), Name: new("widget"),
				Owner: &gh.User{Login: new("acme")}, Archived: new(false),
			}, nil
		},
		getPullRequestFn: func(
			context.Context, string, string, int,
		) (*gh.PullRequest, error) {
			_, accepted, reconcileErr := database.ReconcileRepositoryObservation(
				ctx,
				db.RepoIdentity{
					Platform: "github", PlatformHost: "github.com",
					PlatformRepoID: "repo-a", Owner: "acme", Name: "renamed",
					RepoPath: "acme/renamed",
				},
				time.Now().UTC().Add(time.Hour),
			)
			require.NoError(reconcileErr)
			require.True(accepted)
			return canonical, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx), platformRepoRef(repo),
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.True(result.ProviderAttempted)
	require.ErrorContains(err, "repository was not resolved")

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, entry.Repository.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Empty(events)
}

func TestSyncArchiveMRRejectsMissingMergedMetrics(t *testing.T) {
	tests := []struct {
		name              string
		storedMergeSHA    string
		storedFiles       *int
		storedHeadSHA     string
		providerMergeSHA  string
		providerFiles     *int
		providerHeadSHA   string
		missingMergedAt   bool
		missingFieldLabel string
	}{
		{
			name: "canonical merge commit SHA", storedMergeSHA: "pre-merge-test-sha",
			storedFiles: new(4), providerFiles: new(4),
			missingFieldLabel: "merge_commit_sha",
		},
		{
			name: "files changed", storedMergeSHA: "merge-sha",
			providerMergeSHA: "merge-sha", missingFieldLabel: "files_changed",
		},
		{
			name: "merged timestamp", storedMergeSHA: "merge-sha", storedFiles: new(4),
			providerMergeSHA: "merge-sha", providerFiles: new(4), missingMergedAt: true,
			missingFieldLabel: "merged_at",
		},
		{
			name: "canonical head SHA", storedMergeSHA: "pre-merge-test-sha",
			storedFiles: new(4), storedHeadSHA: "newer-head-sha",
			providerMergeSHA: "merge-sha", providerFiles: new(4),
			providerHeadSHA: "canonical-head-sha", missingFieldLabel: "platform_head_sha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			database := openTestDB(t)
			repo := RepoRef{
				Platform: platform.KindGitHub, Owner: "owner", Name: "repo",
				PlatformHost: "github.com",
			}
			repoID, err := database.UpsertRepo(
				ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
			)
			require.NoError(err)
			providerUpdatedAt := time.Date(2026, 7, 28, 0, 41, 21, 0, time.UTC)
			localUpdatedAt := providerUpdatedAt.Add(835 * time.Millisecond)
			mergedAt := providerUpdatedAt.Add(-time.Second)
			var storedMergedAt *time.Time
			if !tt.missingMergedAt {
				storedMergedAt = &mergedAt
			}
			storedHeadSHA := tt.storedHeadSHA
			if storedHeadSHA == "" {
				storedHeadSHA = "head-sha"
			}
			_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
				RepoID: repoID, PlatformID: 7000, Number: 7,
				State: db.MergeRequestStateMerged, PlatformHeadSHA: storedHeadSHA,
				MergeCommitSHA: tt.storedMergeSHA, FilesChanged: tt.storedFiles,
				CreatedAt: providerUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
				LastActivityAt: localUpdatedAt, MergedAt: storedMergedAt, ClosedAt: &mergedAt,
			})
			require.NoError(err)

			canonical := buildOpenPR(7, providerUpdatedAt)
			canonical.State = new("closed")
			canonical.Merged = new(true)
			if !tt.missingMergedAt {
				canonical.MergedAt = makeTimestamp(mergedAt)
			}
			canonical.ClosedAt = makeTimestamp(mergedAt)
			canonical.MergeCommitSHA = &tt.providerMergeSHA
			canonical.ChangedFiles = tt.providerFiles
			providerHeadSHA := tt.providerHeadSHA
			if providerHeadSHA == "" {
				providerHeadSHA = "head-sha"
			}
			canonical.Head.SHA = &providerHeadSHA
			syncer := NewSyncer(
				map[string]Client{"github.com": &mockClient{singlePR: canonical}},
				database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
			)

			result, err := syncer.SyncArchiveItem(
				WithArchiveSyncBudget(ctx),
				platform.RepoRef{
					Platform: platform.KindGitHub, Host: "github.com",
					Owner: repo.Owner, Name: repo.Name,
				},
				db.ArchiveItemTypeMergeRequest, 7,
			)
			require.ErrorContains(err, tt.missingFieldLabel)
			require.True(result.ProviderAttempted)
		})
	}
}

func TestSyncArchiveMRRejectsCanonicalUnmergedStoredMergedDisagreement(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, Owner: "owner", Name: "repo",
		PlatformHost: "github.com",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	providerUpdatedAt := time.Date(2026, 7, 28, 0, 41, 21, 0, time.UTC)
	localUpdatedAt := providerUpdatedAt.Add(time.Second)
	mergedAt := providerUpdatedAt.Add(-time.Minute)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7000, Number: 7,
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "merge-sha", FilesChanged: new(4),
		CreatedAt: providerUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)

	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.State = new("closed")
	canonical.Merged = new(false)
	canonical.MergedAt = nil
	canonical.ClosedAt = makeTimestamp(providerUpdatedAt.Add(-time.Minute))
	canonical.MergeCommitSHA = nil
	canonical.Head.SHA = new("head-sha")
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{singlePR: canonical}},
		database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx),
		platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: repo.Owner, Name: repo.Name,
		},
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.ErrorContains(err, "merge_state")
	require.True(result.ProviderAttempted)

	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	require.Equal(db.MergeRequestStateMerged, stored.State)
	require.NotNil(stored.MergedAt)
	require.Equal(mergedAt, *stored.MergedAt)
	require.Equal("merge-sha", stored.MergeCommitSHA)
	require.NotNil(stored.FilesChanged)
	require.Equal(4, *stored.FilesChanged)
}

func TestRequireGitHubArchiveMergedMRMetricsRejectsMismatchedFilesChanged(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	mergedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7, Number: 7,
		State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-sha",
		MergeCommitSHA: "merge-sha", FilesChanged: new(9),
		CreatedAt: mergedAt.Add(-time.Hour), UpdatedAt: mergedAt,
		LastActivityAt: mergedAt, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	})
	require.NoError(err)

	err = (&Syncer{db: database}).requireGitHubArchiveMergedMRMetrics(
		ctx, repoID, 7, mergeRequestFetchEvidence{
			merged: true, headSHA: "head-sha", mergeCommitSHA: "merge-sha",
			filesChanged: new(4),
		}, true,
	)
	require.ErrorContains(err, "files_changed")
}

func TestSyncArchiveMROpenPullRequestDoesNotRequireMergeMetrics(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, Owner: "owner", Name: "repo",
		PlatformHost: "github.com",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	providerUpdatedAt := time.Date(2026, 7, 28, 0, 41, 21, 0, time.UTC)
	localUpdatedAt := providerUpdatedAt.Add(time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7000, Number: 7,
		State: db.MergeRequestStateOpen, PlatformHeadSHA: "head-sha",
		CreatedAt: providerUpdatedAt.Add(-time.Hour), UpdatedAt: localUpdatedAt,
		LastActivityAt: localUpdatedAt,
	})
	require.NoError(err)
	canonical := buildOpenPR(7, providerUpdatedAt)
	canonical.Head.SHA = new("head-sha")
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{singlePR: canonical}},
		database, nil, []RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx),
		platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: repo.Owner, Name: repo.Name,
		},
		db.ArchiveItemTypeMergeRequest, 7,
	)
	require.NoError(err)
	require.True(result.ProviderAttempted)
}

func TestSyncArchiveIssuePropagatesTimelineFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	issue := buildOpenIssue(7, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	timelineErr := errors.New("timeline temporarily unavailable")
	client := &issueTimelineMockClient{
		getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			return issue, nil
		},
		comments:         []*gh.IssueComment{},
		issueTimelineErr: timelineErr,
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, testBudget(1000),
	)

	result, err := syncer.SyncArchiveItem(
		WithArchiveSyncBudget(ctx),
		platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: repo.Owner, Name: repo.Name,
		},
		db.ArchiveItemTypeIssue, 7,
	)

	require.ErrorIs(err, timelineErr)
	assert.True(result.ProviderAttempted)
	assert.Equal(int32(1), client.issueTimelineCalls.Load())
}

func TestFetchIssueDetailPersistsIssueETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	issueID := int64(1000)
	issueNumber := 1
	issueTitle := "test issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/1"

	mc := &conditionalIssueTrackingClient{nextETag: `"issue-etag-v2"`}
	mc.getIssueFn = func(context.Context, string, string, int) (*gh.Issue, error) {
		return &gh.Issue{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			CreatedAt: makeTimestamp(updatedAt),
			UpdatedAt: makeTimestamp(updatedAt),
		}, nil
	}
	mc.comments = []*gh.IssueComment{}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	_, err = syncer.fetchIssueDetail(ctx, repo, repoID, 1)
	require.NoError(err)

	etag, err := d.GetHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"issue", 1,
	)
	require.NoError(err)
	assert.Equal(`"issue-etag-v2"`, etag)
}

func TestFetchIssueDetailDoesNotPersistIssueETagWhenDetailRefreshFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)
	require.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"issue", 1, `"issue-etag-v1"`,
	))

	updatedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	issueID := int64(1000)
	issueNumber := 1
	issueTitle := "test issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/1"

	mc := &conditionalIssueTrackingClient{nextETag: `"issue-etag-v2"`}
	mc.getIssueFn = func(context.Context, string, string, int) (*gh.Issue, error) {
		return &gh.Issue{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			CreatedAt: makeTimestamp(updatedAt),
			UpdatedAt: makeTimestamp(updatedAt),
		}, nil
	}
	mc.listIssueCommentsErr = fmt.Errorf("transient comments failure")
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(1000),
	)

	_, err = syncer.fetchIssueDetail(ctx, repo, repoID, 1)
	require.Error(err)

	etag, err := d.GetHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"issue", 1,
	)
	require.NoError(err)
	assert.Equal(`"issue-etag-v1"`, etag)
}

func TestBulkGraphQLGateUsesLocalMergeRequestCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	for number := 1; number <= largeRepoBulkGraphQLThreshold; number++ {
		_, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID:         repoID,
			PlatformID:     int64(number * 1000),
			Number:         number,
			URL:            fmt.Sprintf("https://github.com/owner/repo/pull/%d", number),
			Title:          fmt.Sprintf("test PR %d", number),
			Author:         "alice",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		})
		require.NoError(err)
	}

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	assert.False(syncer.shouldUseBulkGraphQLForMRs(ctx, repo, repoID, 1),
		"local open count should gate large-repo bulk behavior even when the fetched set is small")
}

func TestBulkGraphQLGateUsesLocalIssueCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	for number := 1; number <= largeRepoBulkGraphQLThreshold; number++ {
		_, err := d.UpsertIssue(ctx, &db.Issue{
			RepoID:         repoID,
			PlatformID:     int64(number * 1000),
			Number:         number,
			URL:            fmt.Sprintf("https://github.com/owner/repo/issues/%d", number),
			Title:          fmt.Sprintf("test issue %d", number),
			Author:         "alice",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		})
		require.NoError(err)
	}

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	assert.False(syncer.shouldUseBulkGraphQLForIssues(ctx, repo, repoID, 1),
		"local open count should gate large-repo bulk behavior even when the fetched set is small")
}

func TestRunOnceLargeExistingRepoSkipsBulkGraphQLAndFetchesChangedPRDetail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{
		Owner:              "owner",
		Name:               "repo",
		PlatformHost:       "github.com",
		PlatformExternalID: "repo-owner-repo",
	}
	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   repo.PlatformHost,
		PlatformRepoID: repo.PlatformExternalID,
		Owner:          repo.Owner,
		Name:           repo.Name,
	})
	require.NoError(err)

	unchangedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	changedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	detailFetchedAt := time.Date(2024, 6, 1, 11, 0, 0, 0, time.UTC)
	openPRs := make([]*gh.PullRequest, 0, syncProgressLogInterval+1)
	for number := 1; number <= syncProgressLogInterval+1; number++ {
		updatedAt := unchangedAt
		if number == 1 {
			updatedAt = changedAt
		}
		openPRs = append(openPRs, buildOpenPR(number, updatedAt))
		_, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID:          repoID,
			PlatformID:      int64(number * 1000),
			Number:          number,
			URL:             fmt.Sprintf("https://github.com/owner/repo/pull/%d", number),
			Title:           fmt.Sprintf("test PR %d", number),
			Author:          "alice",
			State:           "open",
			HeadBranch:      "feature-branch",
			BaseBranch:      "main",
			PlatformHeadSHA: "abc123def456",
			CreatedAt:       unchangedAt,
			UpdatedAt:       unchangedAt,
			LastActivityAt:  unchangedAt,
			DetailFetchedAt: &detailFetchedAt,
		})
		require.NoError(err)
	}

	mc := &detailTrackingClient{}
	mc.openPRs = openPRs
	mc.listOpenIssuesErr = notModifiedErr()
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: new(string)}

	var graphQLPRCalls atomic.Int32
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "pullRequests") {
			graphQLPRCalls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"message":"bulk PR fetch should be skipped"}]}`))
	}))
	defer gqlSrv.Close()

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo},
		time.Minute, nil, testBudget(10000),
	)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()),
			nil,
		),
	})

	syncer.RunOnce(ctx)

	assert.True(mc.listOpenPRsCalled.Load(),
		"large repo refresh should still read the open PR index")
	assert.Zero(int(graphQLPRCalls.Load()),
		"large existing repo refresh should not bulk-fetch every PR through GraphQL")
	assert.Equal(int32(1), mc.getPRCalls.Load(),
		"only the changed PR should be fetched by the detail drain")
	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.True(pr.UpdatedAt.Equal(changedAt))
}

func TestDetailDrainUsesProviderReadersForNonGitHub(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          7,
		URL:             "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:           "stale MR",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "old",
		PlatformBaseSHA: "base",
		CreatedAt:       now.Add(-time.Hour),
		UpdatedAt:       now.Add(-time.Hour),
		LastActivityAt:  now.Add(-time.Hour),
	})
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     2001,
		Number:         11,
		URL:            "https://gitlab.com/acme/widget/-/issues/11",
		Title:          "stale issue",
		Author:         "grace",
		State:          "open",
		CreatedAt:      now.Add(-time.Hour),
		UpdatedAt:      now.Add(-time.Hour),
		LastActivityAt: now.Add(-time.Hour),
	})
	require.NoError(err)

	provider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: "gitlab.com",
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(repo),
			PlatformID:     1001,
			Number:         7,
			URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
			Title:          "fresh MR detail",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "new",
			BaseSHA:        "base",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		issues: []platform.Issue{{
			Repo:           platformRepoRef(repo),
			PlatformID:     2001,
			Number:         11,
			URL:            "https://gitlab.com/acme/widget/-/issues/11",
			Title:          "fresh issue detail",
			Author:         "grace",
			State:          "open",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	rateKey := RateBucketKey("gitlab", "gitlab.com", "host")
	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, map[string]*SyncBudget{
		rateKey: NewSyncBudget(100),
	})
	syncer.clients = registry

	syncer.drainDetailQueue(ctx, map[string]bool{rateKey: true}, syncer.TrackedRepos())

	assert.Equal(int32(1), provider.getMRCalls.Load())
	assert.Equal(int32(1), provider.getIssueCalls.Load())
	mr, err := d.GetMergeRequest(ctx, "gitlab", "gitlab.com", "acme", "widget", 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("fresh MR detail", mr.Title)
	assert.NotNil(mr.DetailFetchedAt)
	issue, err := d.GetIssue(ctx, "gitlab", "gitlab.com", "acme", "widget", 11)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("fresh issue detail", issue.Title)
	assert.NotNil(issue.DetailFetchedAt)
}

func TestDetailDrainDisambiguatesSameHostOwnerNameAcrossProviders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	host := "code.example.com"
	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: host,
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: host,
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(gitlabRepo)))
	require.NoError(err)
	require.NotEqual(githubRepoID, gitlabRepoID)

	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          gitlabRepoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://code.example.com/acme/widget/-/merge_requests/7",
		Title:           "stale gitlab MR",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "old",
		PlatformBaseSHA: "base",
		CreatedAt:       now.Add(-time.Hour),
		UpdatedAt:       now.Add(-time.Hour),
		LastActivityAt:  now.Add(-time.Hour),
	})
	require.NoError(err)

	gitlabProvider := &syncTestReadProvider{
		kind: platform.KindGitLab,
		host: host,
		mergeRequests: []platform.MergeRequest{{
			Repo:           platformRepoRef(gitlabRepo),
			PlatformID:     7001,
			Number:         7,
			URL:            "https://code.example.com/acme/widget/-/merge_requests/7",
			Title:          "fresh gitlab MR",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "new",
			BaseSHA:        "base",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
	}
	githubClient := &mockClient{
		getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
			return nil, errors.New("wrong provider")
		},
	}
	registry, err := platform.NewRegistry(
		&gitHubClientProvider{host: host, client: githubClient},
		gitlabProvider,
	)
	require.NoError(err)
	rateKey := RateBucketKey("gitlab", host, "host")
	syncer := NewSyncer(nil, d, nil, []RepoRef{
		githubRepo,
		gitlabRepo,
	}, time.Minute, nil, map[string]*SyncBudget{
		rateKey: NewSyncBudget(100),
	})
	syncer.clients = registry

	syncer.drainDetailQueue(ctx, map[string]bool{rateKey: true}, syncer.TrackedRepos())

	assert.Equal(int32(1), gitlabProvider.getMRCalls.Load())
	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, gitlabRepoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("fresh gitlab MR", mr.Title)
	assert.NotNil(mr.DetailFetchedAt)
}

func TestDetailQueueWatchedKeyIncludesProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	host := "code.example.com"
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: host,
		Owner:        "acme",
		Name:         "widget",
	}
	gitlabRepo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: host,
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(gitlabRepo)))
	require.NoError(err)
	for _, repoID := range []int64{githubRepoID, gitlabRepoID} {
		_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID:         repoID,
			PlatformID:     repoID * 100,
			Number:         7,
			Title:          "same number",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		})
		require.NoError(err)
	}
	syncer := NewSyncer(nil, d, nil, []RepoRef{
		githubRepo,
		gitlabRepo,
	}, time.Minute, nil, nil)
	syncer.SetWatchedMRs([]WatchedMR{{
		Platform:     platform.KindGitLab,
		PlatformHost: host,
		Owner:        "acme",
		Name:         "widget",
		Number:       7,
	}})

	items := syncer.buildDetailQueueItems(ctx, syncer.TrackedRepos())

	require.Len(items, 2)
	watchedByPlatform := map[platform.Kind]bool{}
	for _, item := range items {
		watchedByPlatform[item.Platform] = item.Watched
	}
	assert.False(watchedByPlatform[platform.KindGitHub])
	assert.True(watchedByPlatform[platform.KindGitLab])
}

func TestDetailQueueDerivesPendingCIFromCachedChecks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-5 * time.Minute)
	repo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "owner",
		Name:         "repo",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "pending ci",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		CIChecksJSON:    `[{"name":"build","status":"in_progress","conclusion":""}]`,
		CIHadPending:    false,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &fetchedAt,
	})
	require.NoError(err)

	syncer := NewSyncer(nil, d, nil, []RepoRef{repo}, time.Minute, nil, nil)

	items := syncer.buildDetailQueueItems(ctx, syncer.TrackedRepos())
	require.Len(items, 1)
	assert.True(items[0].CIHadPending)
	queue := BuildQueue(items, now)
	require.Len(queue, 1)
	assert.Equal(1, queue[0].Number)
}

func TestDetailDrainRespectsBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ciState := "success"

	// Create 5 PRs.
	var prs []*gh.PullRequest
	for i := 1; i <= 5; i++ {
		prs = append(prs, buildOpenPR(i, now))
	}

	// Index overhead: GetRepo(1) + releases(1) + tags(1) + ListPRs(1) +
	// ListIssues(1) + GetUser(1, deduplicated by singleflight) = 6 calls.
	// The PR detail admission reserve is 20 wire attempts. Budget of 26
	// leaves enough nominal capacity for one detail, but not a second.
	budget := testBudget(26)
	mc := &detailTrackingClient{}
	mc.budget = budget["github.com"]
	mc.openPRs = prs
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: &ciState}

	// Budget covers index overhead + 1 PR detail fetch, not 2.
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Owner: "owner", Name: "repo",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, budget,
	)

	syncer.RunOnce(ctx)

	// All 5 PRs should be in DB (index scan).
	for i := 1; i <= 5; i++ {
		pr, err := d.GetMergeRequest(
			ctx, "github", "github.com", "owner", "repo", i,
		)
		require.NoError(err)
		require.NotNil(pr, "PR #%d should exist", i)
	}

	// Only 1 PR should have detail_fetched_at set (budget
	// allows at most 1 full detail fetch).
	detailCount := 0
	for i := 1; i <= 5; i++ {
		pr, _ := d.GetMergeRequest(
			ctx, "github", "github.com", "owner", "repo", i,
		)
		if pr != nil && pr.DetailFetchedAt != nil {
			detailCount++
		}
	}
	assert.Equal(1, detailCount,
		"budget should limit detail fetches to 1 PR")

	// Budget should be spent.
	hostBudget := syncer.Budgets()["github.com"]
	require.NotNil(hostBudget)
	assert.Positive(hostBudget.Spent(),
		"budget should have been spent")
}

func TestScopedRunDrainsDetailsOnlyForSelectedRepos(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{
		{
			Owner: "owner", Name: "selected", PlatformHost: "github.com",
			PlatformExternalID: "repo-owner-selected",
		},
		{
			Owner: "owner", Name: "unrelated", PlatformHost: "github.com",
			PlatformExternalID: "repo-owner-unrelated",
		},
	}
	for i, repo := range repos {
		repoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
		require.NoError(err)
		_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID:          repoID,
			PlatformID:      int64(i + 1),
			Number:          i + 1,
			URL:             "https://github.com/owner/" + repo.Name + "/pull/1",
			Title:           repo.Name,
			Author:          "ada",
			State:           "open",
			HeadBranch:      "feature",
			BaseBranch:      "main",
			PlatformHeadSHA: "head",
			PlatformBaseSHA: "base",
			CreatedAt:       now,
			UpdatedAt:       now,
			LastActivityAt:  now,
		})
		require.NoError(err)
	}

	var detailRepos []string
	mc := &detailTrackingClient{}
	mc.budget = NewSyncBudget(100)
	mc.listOpenPRsFn = func(_ context.Context, _, repo string) ([]*gh.PullRequest, error) {
		if repo == "selected" {
			return []*gh.PullRequest{buildOpenPR(1, now)}, nil
		}
		return []*gh.PullRequest{}, nil
	}
	mc.getPullRequestFn = func(_ context.Context, _, repo string, number int) (*gh.PullRequest, error) {
		detailRepos = append(detailRepos, repo)
		return buildOpenPR(number, now), nil
	}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: new("success")}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, repos,
		time.Minute, nil,
		map[string]*SyncBudget{"github.com": mc.budget},
	)
	syncer.runOnce(ctx, true, nil, repos[:1])

	assert.Equal(t, []string{"selected"}, detailRepos)
}

// If queued repo intents follow only a mutable route, a rename drops the
// intended repository and route reuse can select an unrelated successor.
func TestRepoIntentMatchingUsesStableProviderIdentity(t *testing.T) {
	renamed := RepoRef{
		Platform:           platform.KindGitHub,
		Owner:              "new-owner",
		Name:               "new-name",
		PlatformHost:       "github.com",
		PlatformExternalID: "stable-repo-id",
	}
	successor := RepoRef{
		Platform:           platform.KindGitHub,
		Owner:              "old-owner",
		Name:               "old-name",
		PlatformHost:       "github.com",
		PlatformExternalID: "successor-repo-id",
	}
	requestedBeforeRename := RepoRef{
		Platform:           platform.KindGitHub,
		Owner:              "old-owner",
		Name:               "old-name",
		PlatformHost:       "github.com",
		PlatformExternalID: "stable-repo-id",
	}

	assert.Equal(t,
		[]RepoRef{renamed},
		selectRepos([]RepoRef{successor, renamed}, []RepoRef{requestedBeforeRename}),
	)
	assert.Equal(t,
		[]RepoRef{renamed, successor},
		prioritizeRepos([]RepoRef{successor, renamed}, []RepoRef{requestedBeforeRename}),
	)
}

func TestScopedRunDoesNotDelayNextFullRunOnSameHost(t *testing.T) {
	ctx := t.Context()
	d := openTestDB(t)
	bucket := RateBucketKey("github", "github.com", "host")
	repos := []RepoRef{
		{Owner: "owner", Name: "selected", PlatformHost: "github.com"},
		{Owner: "owner", Name: "unrelated", PlatformHost: "github.com"},
	}
	var mu sync.Mutex
	var indexRepos []string
	mc := &mockClient{
		listOpenPRsFn: func(_ context.Context, _, repo string) ([]*gh.PullRequest, error) {
			mu.Lock()
			defer mu.Unlock()
			indexRepos = append(indexRepos, repo)
			return []*gh.PullRequest{}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, repos,
		time.Hour,
		map[string]*RateTracker{bucket: NewRateTracker(d, "github.com", "host", "rest")},
		nil,
	)
	syncer.SetParallelism(1)

	syncer.runOnce(ctx, true, nil, repos[:1])
	syncer.RunOnce(ctx)

	mu.Lock()
	got := append([]string(nil), indexRepos...)
	mu.Unlock()
	assert.Equal(t, []string{"selected", "selected", "unrelated"}, got)
}

func TestScheduledFullRunRetriesAfterOverlappingScopedRun(t *testing.T) {
	tests := []struct {
		name                string
		cadenceGated        bool
		wantUnrelatedSynced bool
	}{
		{name: "runs when host is due", wantUnrelatedSynced: true},
		{name: "honors a future host cadence gate", cadenceGated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			d := openTestDB(t)
			bucket := RateBucketKey("github", "github.com", "host")
			repos := []RepoRef{
				{Owner: "owner", Name: "selected", PlatformHost: "github.com"},
				{Owner: "owner", Name: "unrelated", PlatformHost: "github.com"},
			}
			selectedEntered := make(chan struct{}, 1)
			releaseSelected := make(chan struct{})
			var unrelatedSynced atomic.Bool
			mc := &mockClient{
				listOpenPRsFn: func(ctx context.Context, _, repo string) ([]*gh.PullRequest, error) {
					switch repo {
					case "selected":
						select {
						case selectedEntered <- struct{}{}:
							select {
							case <-releaseSelected:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						default:
						}
					case "unrelated":
						unrelatedSynced.Store(true)
					}
					return []*gh.PullRequest{}, nil
				},
			}
			var rateTrackers map[string]*RateTracker
			if tt.cadenceGated {
				rateTrackers = map[string]*RateTracker{
					bucket: NewRateTracker(d, "github.com", "host", "rest"),
				}
			}
			syncer := NewSyncer(
				map[string]Client{"github.com": mc}, d, nil, repos,
				time.Hour, rateTrackers, nil,
			)
			if tt.cadenceGated {
				syncer.nextSyncAfter[bucket] = time.Now().Add(time.Hour)
			}
			syncer.SetParallelism(1)
			t.Cleanup(syncer.Stop)
			fullRunCompleted := make(chan struct{}, 1)
			syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
				if len(results) == len(repos) {
					select {
					case fullRunCompleted <- struct{}{}:
					default:
					}
				}
			})

			scopedDone := make(chan struct{})
			go func() {
				defer close(scopedDone)
				syncer.runOnce(ctx, true, nil, repos[:1])
			}()

			select {
			case <-selectedEntered:
			case <-time.After(5 * time.Second):
				require.FailNow("scoped run did not start within 5s")
			}
			syncer.RunOnce(ctx)
			close(releaseSelected)

			select {
			case <-fullRunCompleted:
			case <-time.After(5 * time.Second):
				require.FailNow("scheduled full run was not retried after scoped run")
			}
			assert.Equal(t, tt.wantUnrelatedSynced, unrelatedSynced.Load())
			<-scopedDone
		})
	}
}

// If an asynchronous trigger returns before taking the admission lock, the
// server can return 202 before the request is retained by the active run.
func TestTriggerRunForReposReturnsOnlyAfterAdmission(t *testing.T) {
	require := require.New(t)
	repo := RepoRef{
		Owner: "owner", Name: "selected", PlatformHost: "github.com",
	}
	syncer := NewSyncer(
		nil, openTestDB(t), nil, []RepoRef{repo}, time.Hour, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	// Model an active scoped pass with cadence-respecting full work
	// already retained behind it.
	syncer.runMu.Lock()
	syncer.running.Store(true)
	syncer.exclusiveRun = true
	syncer.pendingRun = &pendingSyncRun{full: true}
	syncer.runMu.Unlock()

	accepted := syncer.TriggerRunForRepos(context.Background(), []RepoRef{repo})
	require.True(accepted)

	syncer.runMu.Lock()
	bypassRepos := slices.Clone(syncer.pendingRun.bypassRepos)
	syncer.runMu.Unlock()
	require.Equal([]RepoRef{repo}, bypassRepos)
}

// If a scoped refresh turns a queued cadence-respecting full pass into a
// global bypass, the provider is called for unrelated repositories too.
func TestQueuedScopedRefreshDoesNotBypassFullRunCadence(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	bucket := RateBucketKey("github", "github.com", "host")
	database := openTestDB(t)
	repos := []RepoRef{
		{Owner: "owner", Name: "selected", PlatformHost: "github.com"},
		{Owner: "owner", Name: "unrelated", PlatformHost: "github.com"},
	}
	disabledErr := platform.RepositoryFeatureDisabled(
		platform.KindGitHub,
		"github.com",
		platform.RepositoryFeatureMergeRequests,
		errors.New("repository pull requests disabled"),
	)
	firstSelectedEntered := make(chan struct{})
	releaseFirstSelected := make(chan struct{})
	completions := make(chan struct{}, 2)
	var (
		selectedCalls    atomic.Int32
		unrelatedCalls   atomic.Int32
		releaseFirstOnce sync.Once
	)
	mc := &mockClient{
		listOpenPRsFn: func(ctx context.Context, _, repo string) ([]*gh.PullRequest, error) {
			switch repo {
			case "selected":
				if selectedCalls.Add(1) == 1 {
					close(firstSelectedEntered)
					select {
					case <-releaseFirstSelected:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return nil, disabledErr
				}
			case "unrelated":
				unrelatedCalls.Add(1)
			}
			return []*gh.PullRequest{}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, database, nil, repos,
		time.Hour,
		map[string]*RateTracker{
			bucket: NewRateTracker(database, "github.com", "host", "rest"),
		},
		nil,
	)
	syncer.SetParallelism(1)
	syncer.nextSyncAfter[bucket] = time.Now().Add(time.Hour)
	require.True(syncer.recordRepositoryFeatureDisabled(
		repos[0], platform.RepositoryFeatureMergeRequests, disabledErr,
	))
	require.True(syncer.recordRepositoryFeatureDisabled(
		repos[1], platform.RepositoryFeatureMergeRequests, disabledErr,
	))
	syncer.SetOnSyncCompleted(func([]RepoSyncResult) {
		completions <- struct{}{}
	})
	t.Cleanup(func() {
		releaseFirstOnce.Do(func() { close(releaseFirstSelected) })
		syncer.Stop()
	})

	syncer.TriggerRunForRepos(ctx, repos[:1])
	select {
	case <-firstSelectedEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("initial scoped refresh did not reach the provider")
	}

	// Retain a cadence-respecting full pass, then merge a user refresh for
	// only the selected repository while the first pass still owns the slot.
	syncer.RunOnce(ctx)
	syncer.TriggerRunForRepos(ctx, repos[:1])
	releaseFirstOnce.Do(func() { close(releaseFirstSelected) })

	for range 2 {
		select {
		case <-completions:
		case <-time.After(5 * time.Second):
			require.FailNow("queued sync passes did not complete")
		}
	}
	syncer.Stop()

	require.Equal(int32(2), selectedCalls.Load())
	require.Zero(unrelatedCalls.Load())
}

// If the run slot is released before its terminal status is ordered, a new
// run can publish Running:true before the completed run overwrites it with
// Running:false.
func TestTerminalStatusPublicationKeepsRunSlotUntilOrdered(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	providerEntered := make(chan struct{})
	releaseProvider := make(chan struct{})
	firstCompleted := make(chan struct{})
	var (
		providerCalls   atomic.Int32
		completionCalls atomic.Int32
		releaseOnce     sync.Once
	)
	mock := &mockClient{
		listOpenPRsFn: func(ctx context.Context, _, _ string) ([]*gh.PullRequest, error) {
			if providerCalls.Add(1) == 1 {
				close(providerEntered)
				select {
				case <-releaseProvider:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []*gh.PullRequest{}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock}, openTestDB(t), nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Hour, nil, nil,
	)
	syncer.SetOnSyncCompleted(func([]RepoSyncResult) {
		if completionCalls.Add(1) == 1 {
			close(firstCompleted)
		}
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseProvider) })
		syncer.Stop()
	})

	initialDone := make(chan struct{})
	go func() {
		defer close(initialDone)
		syncer.RunOnce(ctx)
	}()
	select {
	case <-providerEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("initial sync did not reach the provider")
	}

	// Widen the terminal-publication boundary without replacing it: the
	// provider pass is real, and publishStatus still owns statusMu.
	syncer.statusMu.Lock()
	statusLocked := true
	t.Cleanup(func() {
		if statusLocked {
			syncer.statusMu.Unlock()
		}
	})
	releaseOnce.Do(func() { close(releaseProvider) })
	select {
	case <-firstCompleted:
	case <-time.After(5 * time.Second):
		require.FailNow("initial sync did not reach completion")
	}
	require.Never(
		func() bool { return !syncer.running.Load() },
		100*time.Millisecond,
		time.Millisecond,
		"run slot was released while terminal status publication was blocked",
	)

	require.True(syncer.TriggerRun(ctx))
	syncer.statusMu.Unlock()
	statusLocked = false
	require.Eventually(
		func() bool {
			return completionCalls.Load() == 2 &&
				providerCalls.Load() == 2 && !syncer.Status().Running
		},
		5*time.Second,
		time.Millisecond,
		"coalesced follow-up did not reach terminal status",
	)
	select {
	case <-initialDone:
	case <-time.After(5 * time.Second):
		require.FailNow("initial sync did not finish its handoff")
	}
}

// If a queued pass loses the single-flight handoff to another run, the
// accepted work is dropped and provider data can remain stale.
func TestQueuedRunSurvivesSingleFlightHandoff(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	repos := []RepoRef{{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
	}}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstCompleted := make(chan struct{})
	releaseFirstCompletion := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	thirdEntered := make(chan struct{})
	var (
		listCalls                  atomic.Int32
		completionCalls            atomic.Int32
		releaseFirstOnce           sync.Once
		releaseFirstCompletionOnce sync.Once
		releaseSecondOnce          sync.Once
		unlockLifecycleOnce        sync.Once
	)
	mc := &mockClient{
		listOpenPRsFn: func(ctx context.Context, _, _ string) ([]*gh.PullRequest, error) {
			switch listCalls.Add(1) {
			case 1:
				close(firstEntered)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case 2:
				close(secondEntered)
				select {
				case <-releaseSecond:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case 3:
				close(thirdEntered)
			}
			return []*gh.PullRequest{}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, openTestDB(t), nil, repos,
		time.Hour, nil, nil,
	)
	syncer.SetParallelism(1)
	syncer.SetOnSyncCompleted(func([]RepoSyncResult) {
		if completionCalls.Add(1) != 1 {
			return
		}
		close(firstCompleted)
		select {
		case <-releaseFirstCompletion:
		case <-ctx.Done():
		}
	})
	t.Cleanup(func() {
		releaseFirstOnce.Do(func() { close(releaseFirst) })
		releaseFirstCompletionOnce.Do(func() { close(releaseFirstCompletion) })
		releaseSecondOnce.Do(func() { close(releaseSecond) })
		syncer.Stop()
	})

	initialDone := make(chan struct{})
	go func() {
		defer close(initialDone)
		syncer.runOnce(ctx, true, nil, repos)
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("initial scoped run did not start within 5s")
	}

	// Queue a cadence-respecting full pass while the scoped pass owns the slot.
	syncer.RunOnce(ctx)
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstCompleted:
	case <-time.After(5 * time.Second):
		require.FailNow("initial scoped run did not reach completion within 5s")
	}

	competingDone := make(chan struct{})
	// Once the first run drops the single-flight slot, hold its asynchronous
	// replay registration long enough for the competing run to claim the slot.
	syncer.lifecycleMu.Lock()
	t.Cleanup(func() { unlockLifecycleOnce.Do(syncer.lifecycleMu.Unlock) })
	releaseFirstCompletionOnce.Do(func() { close(releaseFirstCompletion) })
	require.Eventually(func() bool {
		syncer.runMu.Lock()
		defer syncer.runMu.Unlock()
		return !syncer.exclusiveRun
	}, 5*time.Second, time.Millisecond, "initial run did not reach the handoff")
	go func() {
		defer close(competingDone)
		// This user-triggered run can claim the slot during the old handoff gap.
		syncer.runOnce(ctx, true, nil, nil)
	}()
	select {
	case <-competingDone:
		// The atomic handoff kept the slot, so this run coalesced behind it.
	case <-secondEntered:
		// The old handoff exposed the slot and this run claimed it.
	case <-time.After(5 * time.Second):
		require.FailNow("competing run did not reach the single-flight slot")
	}
	unlockLifecycleOnce.Do(syncer.lifecycleMu.Unlock)

	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("second run did not start within 5s")
	}
	releaseSecondOnce.Do(func() { close(releaseSecond) })
	select {
	case <-thirdEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("queued run was dropped during the single-flight handoff")
	}
	<-initialDone
	<-competingDone
	assert.Equal(t, int32(3), listCalls.Load())
}

// If an explicit empty scope is widened to a full pass while queued, every
// configured provider repository is fetched unexpectedly.
func TestQueuedEmptyRepoScopeRemainsEmpty(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	repos := []RepoRef{{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
	}}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	completions := make(chan []RepoSyncResult, 2)
	var (
		listCalls        atomic.Int32
		releaseFirstOnce sync.Once
	)
	mc := &mockClient{
		listOpenPRsFn: func(ctx context.Context, _, _ string) ([]*gh.PullRequest, error) {
			if listCalls.Add(1) == 1 {
				close(firstEntered)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []*gh.PullRequest{}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, openTestDB(t), nil, repos,
		time.Hour, nil, nil,
	)
	syncer.SetParallelism(1)
	syncer.SetOnSyncCompleted(func(results []RepoSyncResult) {
		completions <- slices.Clone(results)
	})
	t.Cleanup(func() {
		releaseFirstOnce.Do(func() { close(releaseFirst) })
		syncer.Stop()
	})

	initialDone := make(chan struct{})
	go func() {
		defer close(initialDone)
		syncer.runOnce(ctx, true, nil, repos)
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("initial scoped run did not start within 5s")
	}

	// An empty non-nil scope means there is intentionally no provider work.
	syncer.runOnce(ctx, true, nil, []RepoRef{})
	releaseFirstOnce.Do(func() { close(releaseFirst) })

	var got [][]RepoSyncResult
	for range 2 {
		select {
		case results := <-completions:
			got = append(got, results)
		case <-time.After(5 * time.Second):
			require.FailNow("queued empty-scope run did not complete within 5s")
		}
	}
	<-initialDone
	require.Len(got[0], 1)
	assert.Empty(t, got[1])
	assert.Equal(t, int32(1), listCalls.Load())
}

func TestBudgetResetOnRateWindowReset(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	rt := NewRateTracker(d, "github.com", "host", "rest")
	budget := NewSyncBudget(100)
	rt.SetOnWindowReset(budget.Reset)

	// Simulate some spending.
	budget.Spend(50)
	assert.Equal(50, budget.Spent())

	// First rate update sets remaining to 4999.
	rt.UpdateFromRate(Rate{
		Remaining: 4999,
		Limit:     5000,
		Reset:     time.Now().Add(time.Hour),
	})

	// No window reset yet (first contact).
	assert.Equal(50, budget.Spent(),
		"budget should not reset on first contact")

	// Simulate rate decrease (normal usage).
	rt.UpdateFromRate(Rate{
		Remaining: 4990,
		Limit:     5000,
		Reset:     time.Now().Add(time.Hour),
	})
	assert.Equal(50, budget.Spent(),
		"budget should not reset on normal decrease")

	// Simulate window expiry: move resetAt to the past.
	pastReset := time.Now().Add(-1 * time.Second)
	rt.SetResetAtForTesting(pastReset)

	// Simulate window reset (remaining jumps up + old resetAt passed).
	rt.UpdateFromRate(Rate{
		Remaining: 5000,
		Limit:     5000,
		Reset:     time.Now().Add(2 * time.Hour),
	})
	assert.Equal(0, budget.Spent(),
		"budget should reset when rate window resets")
}

func TestSyncMRSkipsGetUserWhenDisplayNameCached(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	sha := "abc123"
	number := 1
	author := "testuser"
	title := "Test PR"
	state := "open"
	url := "https://github.com/acme/widget/pull/1"
	now := &gh.Timestamp{Time: time.Now()}

	mock := &mockClient{
		singlePR: &gh.PullRequest{
			Number:    &number,
			Title:     &title,
			State:     &state,
			HTMLURL:   &url,
			User:      &gh.User{Login: &author},
			UpdatedAt: now,
			CreatedAt: now,
			Head:      &gh.PullRequestBranch{SHA: &sha, Ref: new("feature")},
			Base:      &gh.PullRequestBranch{Ref: new("main")},
		},
		checkRuns: []*gh.CheckRun{{
			Name:       new("ci"),
			Status:     new("completed"),
			Conclusion: new("success"),
		}},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	// First sync: GetUser should be called to resolve display name
	err := syncer.SyncMR(t.Context(), "acme", "widget", 1)
	require.NoError(t, err)
	assert.Equal(int32(1), mock.getUserCalls.Load())

	// Second sync: display name is in DB, GetUser should be skipped
	err = syncer.SyncMR(t.Context(), "acme", "widget", 1)
	require.NoError(t, err)
	assert.Equal(int32(1), mock.getUserCalls.Load(),
		"GetUser should not be called again when display name is cached")
}

func TestRefreshCIStatusAlwaysFetchesCombined(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	mock := &mockClient{
		checkRuns: []*gh.CheckRun{{
			Name:       new("ci"),
			Status:     new("completed"),
			Conclusion: new("success"),
		}},
		ciStatus: &gh.CombinedStatus{
			State:      new("success"),
			TotalCount: new(1),
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	repoID, _ := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	err := syncer.refreshCIStatus(
		t.Context(),
		RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		repoID,
		1,
		"abc123",
	)
	require.NoError(t, err)

	// GetCombinedStatus should always be called for correctness
	// (legacy commit statuses exist alongside check runs).
	assert.Equal(int32(1), mock.getCombinedCalls.Load(),
		"GetCombinedStatus should always be called")
}

func TestRefreshCIStatusPreservesExistingStatusWhenChecksFail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Now().UTC()
	_, err = d.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		Title:           "pending",
		State:           "open",
		PlatformHeadSHA: "abc123",
		CIStatus:        "pending",
		CIChecksJSON:    `[{"name":"build","status":"in_progress"}]`,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	mock := &mockClient{checkRunsErr: errors.New("temporary provider failure")}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.refreshCIStatus(
		t.Context(),
		RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		repoID,
		1,
		"abc123",
	)
	require.NoError(err)

	mr, err := d.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("pending", mr.CIStatus)
	assert.Contains(mr.CIChecksJSON, "in_progress")
}

func TestRefreshCIStatusForHeadRetainsMixedPendingChecks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Now().UTC()
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		Title:           "mixed checks",
		State:           "open",
		PlatformHeadSHA: "abc123",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	mock := &mockClient{
		checkRuns: []*gh.CheckRun{
			{Name: new("failed"), Status: new("completed"), Conclusion: new("failure")},
			{Name: new("running"), Status: new("in_progress")},
		},
		ciStatus: &gh.CombinedStatus{},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	err = syncer.refreshCIStatus(
		ctx,
		RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		repoID,
		1,
		"abc123",
	)
	require.NoError(err)
	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("failure", mr.CIStatus)
	assert.True(mr.CIHadPending)
	assert.Contains(mr.CIChecksJSON, "in_progress")
}

func TestRefreshCIStatusFallsBackToCombinedWhenNoCheckRuns(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	mock := &mockClient{
		checkRuns: nil,
		ciStatus: &gh.CombinedStatus{
			State:      new("success"),
			TotalCount: new(1),
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	repoID, _ := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	err := syncer.refreshCIStatus(
		t.Context(),
		RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
		repoID,
		1,
		"abc123",
	)
	require.NoError(t, err)

	// No check runs: GetCombinedStatus should be called as fallback
	assert.Equal(int32(1), mock.getCombinedCalls.Load(),
		"GetCombinedStatus should be called when no check runs exist")
}

// TestSyncer_OnStatusChangeCallback verifies the onStatusChange
// callback fires for each status transition during RunOnce. The
// SSE server uses this to broadcast live sync state.
func TestSyncer_OnStatusChangeCallback(t *testing.T) {
	assert := assert.New(t)
	mock := &mockClient{openPRs: []*gh.PullRequest{}}
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "o", "n"))
	require.NoError(t, err)
	repos := []RepoRef{{Owner: "o", Name: "n", PlatformHost: "github.com"}}
	s := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil, repos, time.Hour, nil, nil,
	)

	var mu sync.Mutex
	var statuses []*SyncStatus
	s.SetOnStatusChange(func(status *SyncStatus) {
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
	})

	s.RunOnce(t.Context())

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(statuses), 2,
		"should fire at least started + completed")
	assert.True(statuses[0].Running,
		"first callback should be running=true")
	assert.False(statuses[len(statuses)-1].Running,
		"last callback should be running=false")
}

func TestFormatRateLimitWaitUsesSecondsOnlyBelowOneMinute(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		wait time.Duration
		want string
	}{
		{name: "sub-second waits round up to one second", wait: 364 * time.Millisecond, want: "1s"},
		{name: "sub-minute waits show seconds", wait: 38*time.Second + 364*time.Millisecond, want: "39s"},
		{name: "minute-scale waits hide seconds", wait: 25*time.Minute + 38*time.Second + 364*time.Millisecond, want: "26m"},
		{name: "hour-scale waits hide seconds", wait: 2*time.Hour + time.Minute + time.Second, want: "2h2m"},
		{name: "exact hours hide zero minutes", wait: 2 * time.Hour, want: "2h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(tt.want, formatRateLimitWait(tt.wait))
		})
	}
}

func TestSyncerRateLimitProgressUsesMinuteScaleWaits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	rt := NewRateTracker(d, "github.com", "host", "rest")
	rt.UpdateFromRate(Rate{
		Remaining: 0,
		Reset:     time.Now().Add(25*time.Minute + 38*time.Second + 364*time.Millisecond),
	})

	s := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{{Owner: "o", Name: "n", PlatformHost: "github.com"}},
		time.Hour,
		map[string]*RateTracker{"github.com": rt},
		nil,
	)

	progress := make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.SetOnStatusChange(func(status *SyncStatus) {
		if strings.Contains(status.Progress, "rate limited, waiting") {
			select {
			case progress <- status.Progress:
			default:
			}
			cancel()
		}
	})

	done := make(chan struct{})
	go func() {
		s.RunOnce(ctx)
		close(done)
	}()

	var got string
	select {
	case got = <-progress:
	case <-time.After(2 * time.Second):
		require.FailNow("RunOnce did not publish rate-limit progress")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow("RunOnce did not return after cancel")
	}

	assert.Contains(got, "rate limited, waiting ")
	assert.NotContains(got, ".")
	assert.NotContains(got, "s")
	assert.Regexp(`rate limited, waiting \d+m$`, got)
}

// notModifiedErr returns the error shape go-github surfaces when the
// HTTP transport receives a 304 Not Modified response. The etag
// transport intercepts list-endpoint requests and adds If-None-Match
// headers; on a cache hit GitHub responds 304, which go-github wraps
// as *gh.ErrorResponse. The sync code calls IsNotModified to detect
// this and treat it as a no-op.
func notModifiedErr() error {
	return &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusNotModified},
	}
}

// TestSyncerHandles304OnPRList verifies that a 304 response from
// the open-PR list is treated as "list unchanged, nothing to do"
// rather than a fatal sync error. Before the fix, IsNotModified
// was unused at the call site and the wrapped 304 was returned
// as "list open PRs: ...", failing the repo sync entirely.
func TestSyncerHandles304OnPRList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &mockClient{
		listOpenPRsErr: notModifiedErr(),
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d,
		nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	var (
		results   []RepoSyncResult
		gotResult sync.WaitGroup
	)
	gotResult.Add(1)
	syncer.SetOnSyncCompleted(func(r []RepoSyncResult) {
		results = r
		gotResult.Done()
	})

	syncer.RunOnce(t.Context())
	gotResult.Wait()

	require.Len(results, 1)
	assert.Empty(results[0].Error,
		"304 on open-PR list must not surface as a sync error")
}

func TestSyncerReconcilesMergedActorOnPRList304(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	merged := true
	actor := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &actor}
	mc := &mockClient{listOpenPRsErr: notModifiedErr(), singlePR: pr}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	syncer.SetClock(func() time.Time { return now.Add(time.Hour) })

	syncer.RunOnce(ctx)

	stored, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	events, err := d.ListMREvents(ctx, stored.ID)
	require.NoError(err)
	require.Condition(func() bool {
		for _, event := range events {
			if event.EventType == "merged" && event.Author == actor {
				return true
			}
		}
		return false
	}, "a successful 304 sync must still reconcile missing merged actors")
}

// TestSyncerHandles304OnIssueList verifies the same short-circuit
// for the open-issue list endpoint. syncIssues is called from
// doSyncRepo with its error treated as non-fatal (logged only),
// so even before the fix the repo would not be marked failed —
// but the per-issue upserts and closure detection would still be
// skipped erroneously due to the early return path. After the
// fix, the function explicitly returns nil on 304 and the
// happy-path PR sync still completes cleanly.
func TestSyncerHandles304OnIssueList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	mc := &mockClient{
		openPRs:           []*gh.PullRequest{},
		listOpenIssuesErr: notModifiedErr(),
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d,
		nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		nil,
	)

	var (
		results   []RepoSyncResult
		gotResult sync.WaitGroup
	)
	gotResult.Add(1)
	syncer.SetOnSyncCompleted(func(r []RepoSyncResult) {
		results = r
		gotResult.Done()
	})

	syncer.RunOnce(t.Context())
	gotResult.Wait()

	require.Len(results, 1)
	assert.Empty(results[0].Error,
		"304 on open-issue list must not surface as a sync error")
}

// TestSyncerPRList304MakesNoAPICalls verifies that a 304 on the open-PR
// list endpoint triggers zero additional API calls for that repo's PRs.
// CI freshness for unchanged PRs is handled by the detail drain's
// priority scoring (ci_had_pending items get expedited refetches).
func TestSyncerPRList304MakesNoAPICalls(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	// Seed one open PR with pending CI via a full sync.
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	inProgress := "in_progress"
	seedClient := &mockClient{
		openPRs:   []*gh.PullRequest{buildOpenPR(1, now)},
		checkRuns: []*gh.CheckRun{{Status: &inProgress}},
	}
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}
	seedSyncer := NewSyncer(
		map[string]Client{"github.com": seedClient},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)
	seedSyncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.Equal("pending", pr.CIStatus)

	// Second sync: PR list returns 304. The mock has CI data that
	// would change the status if called, but the 304 path must not
	// call any CI endpoints.
	completed := "completed"
	success := "success"
	spy := &callCountingClient{
		listOpenPRsErr: notModifiedErr(),
		checkRuns: []*gh.CheckRun{
			{Status: &completed, Conclusion: &success},
		},
	}
	// budgetPerHour=0 disables detail drain so only index phase runs.
	refreshSyncer := NewSyncer(
		map[string]Client{"github.com": spy},
		d, nil, repos, time.Minute, nil, nil,
	)
	refreshSyncer.RunOnce(ctx)

	require.Equal(0, spy.ciCalls,
		"304 on PR list must not trigger any CI API calls")

	// CI state should be unchanged — still pending from seed.
	pr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.Equal("pending", pr.CIStatus,
		"CI should remain stale until detail drain refreshes it")
}

// callCountingClient wraps mockClient and counts CI-related API calls.
type callCountingClient struct {
	mockClient
	ciCalls int
}

func (c *callCountingClient) ListCheckRunsForRef(
	ctx context.Context, owner, repo, ref string,
) ([]*gh.CheckRun, error) {
	c.ciCalls++
	return c.mockClient.ListCheckRunsForRef(ctx, owner, repo, ref)
}

func (c *callCountingClient) GetCombinedStatus(
	ctx context.Context, owner, repo, ref string,
) (*gh.CombinedStatus, error) {
	c.ciCalls++
	return c.mockClient.GetCombinedStatus(ctx, owner, repo, ref)
}

// TestSyncerSyncsIssuesOnPRList304 verifies that a 304 on the open-PR
// list does not short-circuit issue sync. Issues have an independent
// ETag and their own open-list endpoint, so a PR-list 304 must not
// prevent new issues from being picked up.
func TestSyncerSyncsIssuesOnPRList304(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	issueNumber := 42
	issueTitle := "broken thing"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/42"
	issueBody := ""
	issueID := int64(900042)
	mc := &mockClient{
		listOpenPRsErr: notModifiedErr(),
		openIssues: []*gh.Issue{
			{
				ID:        &issueID,
				Number:    &issueNumber,
				Title:     &issueTitle,
				State:     &issueState,
				HTMLURL:   &issueURL,
				Body:      &issueBody,
				CreatedAt: makeTimestamp(now),
				UpdatedAt: makeTimestamp(now),
			},
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	syncer.RunOnce(ctx)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue, "issue sync must run even when PR list returns 304")
	assert.Equal(issueNumber, issue.Number)
	assert.Equal(issueTitle, issue.Title)
}

func TestSyncRepoSkipsRemovedUpstreamPeriodicCandidates(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo", RepoPath: "owner/repo",
		PlatformExternalID: "repo-owner-repo",
	}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(
		"github.com", "owner", "repo",
	))
	require.NoError(err)

	openPR := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, openPR)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	openIssue := buildOpenIssue(8, now)
	normalizedIssue, err := NormalizeIssue(repoID, openIssue)
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, normalizedIssue)
	require.NoError(err)
	mergedPR := buildOpenPR(9, now)
	normalizedMerged, err := NormalizePR(repoID, mergedPR)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedMerged)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(
		ctx, repoID, 9, "merged", &mergedAt, &mergedAt,
	))

	for _, item := range []struct {
		itemType db.ArchiveItemType
		number   int
	}{
		{itemType: db.ArchiveItemTypeMergeRequest, number: 7},
		{itemType: db.ArchiveItemTypeIssue, number: 8},
		{itemType: db.ArchiveItemTypeMergeRequest, number: 9},
	} {
		_, err = d.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
			repoID, item.itemType, item.number,
			fmt.Sprintf("%s-%d", item.itemType, item.number), now, now,
		)
		require.NoError(err)
	}

	var pullCalls atomic.Int32
	var issueCalls atomic.Int32
	client := &mockClient{
		getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
			pullCalls.Add(1)
			return nil, errors.New("removed pull must not be fetched")
		},
		getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			issueCalls.Add(1)
			return nil, errors.New("removed issue must not be fetched")
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	syncer.SetClock(func() time.Time { return now.Add(time.Hour) })

	require.NoError(syncer.fetchAndUpdateClosed(ctx, repo, repoID, 7, false))
	require.NoError(syncer.fetchAndUpdateClosedIssue(ctx, repo, repoID, 8))
	changed, err := syncer.backfillMergedActorEvent(ctx, repo, repoID, 9)
	require.NoError(err)
	require.False(changed)
	require.NoError(syncer.syncRepo(ctx, repo))
	require.Zero(pullCalls.Load())
	require.Zero(issueCalls.Load())
}

func TestSyncStoresIssueLabels(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 4, 12, 0, 0, 0, time.UTC)
	issueNumber := 42
	issueTitle := "broken thing"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/42"
	issueBody := ""
	issueID := int64(900042)
	mc := &mockClient{
		openIssues: []*gh.Issue{{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			Body:      &issueBody,
			CreatedAt: makeTimestamp(now),
			UpdatedAt: makeTimestamp(now),
			Labels: []*gh.Label{
				buildGitHubLabel(801, "bug", "Something is broken", "d73a4a", true),
			},
		}},
		comments: []*gh.IssueComment{},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	syncer.RunOnce(ctx)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	require.Equal("bug", issue.Labels[0].Name)
	require.Equal("Something is broken", issue.Labels[0].Description)
	require.Equal("d73a4a", issue.Labels[0].Color)
	require.True(issue.Labels[0].IsDefault)
	require.Equal(int64(801), issue.Labels[0].PlatformID)
	require.True(issue.Labels[0].UpdatedAt.Equal(now))
}

func TestFetchAndUpdateClosedRefreshesPRLabels(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	closedAt := makeTimestamp(now)
	pr.ClosedAt = closedAt
	pr.Labels = []*gh.Label{buildGitHubLabel(901, "bug", "Old bug", "d73a4a", true)}
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	storedBefore, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, storedBefore.ID, []db.Label{{
		PlatformID:  901,
		Name:        "bug",
		Description: "Old bug",
		Color:       "d73a4a",
		IsDefault:   true,
		UpdatedAt:   now,
	}}))

	pr.Labels = []*gh.Label{buildGitHubLabel(902, "release", "Ready to release", "5319e7", false)}
	pr.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.fetchAndUpdateClosed(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, 7, false))

	storedAfter, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.Len(storedAfter.Labels, 1)
	require.Equal("release", storedAfter.Labels[0].Name)
	require.Equal(int64(902), storedAfter.Labels[0].PlatformID)
}

func TestFetchAndUpdateClosedPersistsMergedActorEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)

	merged := true
	mergedBy := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(now.Add(time.Minute))
	pr.ClosedAt = makeTimestamp(now.Add(time.Minute))
	pr.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	pr.MergedBy = &gh.User{Login: &mergedBy}

	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.fetchAndUpdateClosed(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, 7, false))

	storedAfter, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NotNil(storedAfter)
	assert.Equal(db.MergeRequestStateMerged, storedAfter.State)

	events, err := d.ListMREvents(ctx, storedAfter.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("merged", events[0].EventType)
	assert.Equal("merge-admin", events[0].Author)
	assert.Equal("merged this", events[0].Summary)
	assert.True(events[0].CreatedAt.Equal(now.Add(time.Minute)))
}

func TestFetchAndUpdateClosedRefreshesPRLabelsWithSameRepoOnAnotherHost(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	otherRepoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("ghe.corp.com", "owner", "repo"))
	require.NoError(err)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	otherPR := buildOpenPR(7, now)
	otherPR.State = new("closed")
	otherPR.ClosedAt = makeTimestamp(now)
	otherPR.Labels = []*gh.Label{buildGitHubLabel(990, "other-host", "Other host label", "333333", false)}
	otherNormalizedPR, err := NormalizePR(otherRepoID, otherPR)
	require.NoError(err)
	otherMRID, err := d.UpsertMergeRequest(ctx, otherNormalizedPR)
	require.NoError(err)
	require.NoError(d.ReplaceMergeRequestLabels(ctx, otherRepoID, otherMRID, []db.Label{{
		PlatformID:  990,
		Name:        "other-host",
		Description: "Other host label",
		Color:       "333333",
		UpdatedAt:   now,
	}}))

	pr := buildOpenPR(7, now)
	pr.State = new("closed")
	pr.ClosedAt = makeTimestamp(now)
	pr.Labels = []*gh.Label{buildGitHubLabel(901, "bug", "Old bug", "d73a4a", true)}
	targetNormalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	targetMRID, err := d.UpsertMergeRequest(ctx, targetNormalizedPR)
	require.NoError(err)
	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, targetMRID, []db.Label{{
		PlatformID:  901,
		Name:        "bug",
		Description: "Old bug",
		Color:       "d73a4a",
		IsDefault:   true,
		UpdatedAt:   now,
	}}))

	pr.Labels = []*gh.Label{buildGitHubLabel(902, "release", "Ready to release", "5319e7", false)}
	pr.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.fetchAndUpdateClosed(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, 7, false))

	var labelName string
	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT l.name
		FROM forge_merge_request_labels ml
		JOIN forge_labels l ON l.id = ml.label_id
		WHERE ml.merge_request_id = ?`, targetMRID,
	).Scan(&labelName)
	require.NoError(err)
	require.Equal("release", labelName)

	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT l.name
		FROM forge_merge_request_labels ml
		JOIN forge_labels l ON l.id = ml.label_id
		WHERE ml.merge_request_id = ?`, otherMRID,
	).Scan(&labelName)
	require.NoError(err)
	require.Equal("other-host", labelName)
}

func TestFetchAndUpdateClosedRefreshesIssueLabels(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 6, 12, 0, 0, 0, time.UTC)
	issueNumber := 9
	issueTitle := "closed issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/9"
	issueBody := ""
	issueID := int64(900009)
	issue := &gh.Issue{ID: &issueID, Number: &issueNumber, Title: &issueTitle, State: &issueState, HTMLURL: &issueURL, Body: &issueBody, CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now), Labels: []*gh.Label{buildGitHubLabel(1001, "bug", "Old bug", "d73a4a", true)}}
	normalizedIssue, err := NormalizeIssue(repoID, issue)
	require.NoError(err)
	issueRowID, err := d.UpsertIssue(ctx, normalizedIssue)
	require.NoError(err)
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueRowID, []db.Label{{PlatformID: 1001, Name: "bug", Description: "Old bug", Color: "d73a4a", IsDefault: true, UpdatedAt: now}}))

	closedState := "closed"
	issue.State = &closedState
	issue.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	issue.Labels = []*gh.Label{buildGitHubLabel(1002, "docs", "Documentation", "0075ca", false)}
	closedAt := makeTimestamp(now.Add(2 * time.Minute))
	issue.ClosedAt = closedAt
	mc := &mockClient{getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) { return issue, nil }}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.fetchAndUpdateClosedIssue(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, issueNumber))

	stored, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.Len(stored.Labels, 1)
	require.Equal("docs", stored.Labels[0].Name)
	require.Equal(int64(1002), stored.Labels[0].PlatformID)
}

func TestFetchAndUpdateClosedRefreshesIssueLabelsWithSameRepoOnAnotherHost(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	otherRepoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("ghe.corp.com", "owner", "repo"))
	require.NoError(err)
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 6, 12, 0, 0, 0, time.UTC)
	issueNumber := 9

	otherState := "open"
	otherTitle := "other closed issue"
	otherURL := "https://ghe.corp.com/owner/repo/issues/9"
	otherBody := ""
	otherID := int64(800009)
	otherIssue := &gh.Issue{ID: &otherID, Number: &issueNumber, Title: &otherTitle, State: &otherState, HTMLURL: &otherURL, Body: &otherBody, CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now), Labels: []*gh.Label{buildGitHubLabel(1901, "other-host", "Other host label", "333333", false)}}
	otherNormalizedIssue, err := NormalizeIssue(otherRepoID, otherIssue)
	require.NoError(err)
	otherIssueRowID, err := d.UpsertIssue(ctx, otherNormalizedIssue)
	require.NoError(err)
	require.NoError(d.ReplaceIssueLabels(ctx, otherRepoID, otherIssueRowID, []db.Label{{PlatformID: 1901, Name: "other-host", Description: "Other host label", Color: "333333", UpdatedAt: now}}))

	issueTitle := "closed issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/9"
	issueBody := ""
	issueID := int64(900009)
	issue := &gh.Issue{ID: &issueID, Number: &issueNumber, Title: &issueTitle, State: &issueState, HTMLURL: &issueURL, Body: &issueBody, CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now), Labels: []*gh.Label{buildGitHubLabel(1001, "bug", "Old bug", "d73a4a", true)}}
	normalizedIssue, err := NormalizeIssue(repoID, issue)
	require.NoError(err)
	issueRowID, err := d.UpsertIssue(ctx, normalizedIssue)
	require.NoError(err)
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueRowID, []db.Label{{PlatformID: 1001, Name: "bug", Description: "Old bug", Color: "d73a4a", IsDefault: true, UpdatedAt: now}}))

	closedState := "closed"
	issue.State = &closedState
	issue.UpdatedAt = makeTimestamp(now.Add(time.Minute))
	issue.Labels = []*gh.Label{buildGitHubLabel(1002, "docs", "Documentation", "0075ca", false)}
	closedAt := makeTimestamp(now.Add(2 * time.Minute))
	issue.ClosedAt = closedAt
	mc := &mockClient{getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) { return issue, nil }}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)

	require.NoError(syncer.fetchAndUpdateClosedIssue(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, issueNumber))

	var labelName string
	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT l.name
		FROM forge_issue_labels il
		JOIN forge_labels l ON l.id = il.label_id
		WHERE il.issue_id = ?`, issueRowID,
	).Scan(&labelName)
	require.NoError(err)
	require.Equal("docs", labelName)

	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT l.name
		FROM forge_issue_labels il
		JOIN forge_labels l ON l.id = il.label_id
		WHERE il.issue_id = ?`, otherIssueRowID,
	).Scan(&labelName)
	require.NoError(err)
	require.Equal("other-host", labelName)
}

type partialFailureMock struct {
	mockClient
	issuesCached         bool
	prsCached            bool
	listOpenIssuesFn     func(context.Context, string, string) ([]*gh.Issue, error)
	listOpenPRsFn        func(context.Context, string, string) ([]*gh.PullRequest, error)
	listIssueCommentsFn  func(context.Context, string, string, int) ([]*gh.IssueComment, error)
	listReviewsFn        func(context.Context, string, string, int) ([]*gh.PullRequestReview, error)
	listOpenPRsErr       error // injected error for ListOpenPullRequests
	listIssueCommentsErr error // injected error for ListIssueComments
	listReviewsErr       error // injected error for ListReviews (MR timeline)
	getIssueErr          error // injected error for GetIssue (closure path)
}

func (m *partialFailureMock) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	if m.listOpenPRsFn != nil {
		return m.listOpenPRsFn(ctx, owner, repo)
	}
	if m.listOpenPRsErr != nil {
		return nil, m.listOpenPRsErr
	}
	if m.prsCached {
		return nil, notModifiedErr()
	}
	m.prsCached = true
	return m.openPRs, nil
}

func (m *partialFailureMock) ListOpenIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	if m.listOpenIssuesFn != nil {
		return m.listOpenIssuesFn(ctx, owner, repo)
	}
	if m.listOpenIssuesErr != nil {
		return nil, m.listOpenIssuesErr
	}
	if m.issuesCached {
		return nil, notModifiedErr()
	}
	m.issuesCached = true
	return m.openIssues, nil
}

func (m *partialFailureMock) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	if m.listIssueCommentsFn != nil {
		return m.listIssueCommentsFn(ctx, owner, repo, number)
	}
	if m.listIssueCommentsErr != nil {
		return nil, m.listIssueCommentsErr
	}
	return m.comments, nil
}

func (m *partialFailureMock) ListIssueCommentsIfChanged(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	if m.listIssueCommentsErr != nil {
		return nil, m.listIssueCommentsErr
	}
	if m.comments == nil {
		return nil, notModifiedErr()
	}
	return m.ListIssueComments(ctx, owner, repo, number)
}

func (m *partialFailureMock) ListReviews(ctx context.Context, owner, repo string, number int) ([]*gh.PullRequestReview, error) {
	if m.listReviewsFn != nil {
		return m.listReviewsFn(ctx, owner, repo, number)
	}
	if m.listReviewsErr != nil {
		return nil, m.listReviewsErr
	}
	return m.reviews, nil
}

func (m *partialFailureMock) GetIssue(ctx context.Context, owner, repo string, number int) (*gh.Issue, error) {
	if m.getIssueErr != nil {
		return nil, m.getIssueErr
	}
	if m.getIssueFn != nil {
		return m.getIssueFn(ctx, owner, repo, number)
	}
	return nil, nil
}

func (m *partialFailureMock) InvalidateListETagsForRepo(_, _ string, endpoints ...string) {
	m.invalidateCalls.Add(1)
	if len(endpoints) == 0 {
		m.prsCached = false
		m.issuesCached = false
		return
	}
	for _, ep := range endpoints {
		switch ep {
		case "pulls":
			m.prsCached = false
		case "issues":
			m.issuesCached = false
		}
	}
}

// TestSyncerSyncOpenIssueFailureMarksRepoFailed verifies that when
// the open-issue list succeeds but syncOpenIssue fails for an
// individual item (here via a ListIssueComments error during timeline
// refresh), syncIssues returns an error, doSyncRepo calls
// markFailure, and the next cycle forces a timeline refresh via
// forceRefresh even though UpdatedAt hasn't changed.
func TestSyncerSyncOpenIssueFailureMarksRepoFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	issueNumber := 7
	issueTitle := "per-item failure issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/7"
	issueBody := ""
	issueID := int64(777)
	openIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}

	commentID := int64(999)
	commentBody := "recovery comment"
	commentUser := "commenter"
	recoveryComment := &gh.IssueComment{
		ID:        &commentID,
		Body:      &commentBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
		User:      &gh.User{Login: &commentUser},
	}

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.openIssues = []*gh.Issue{openIssue}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	// Issue list succeeds, but timeline refresh fails for the item.
	mc.listIssueCommentsErr = fmt.Errorf("transient comments failure")

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: issue list succeeds, issue is upserted to DB, but
	// refreshIssueTimeline fails → syncOpenIssue returns error →
	// hadItemFailure → syncIssues returns error → markFailure.
	syncer.RunOnce(ctx)

	// Issue row lands in DB (upsert happened before timeline).
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue, "issue should be upserted even though timeline failed")

	// No events should exist because timeline refresh failed.
	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Empty(events, "no events should exist after failed timeline refresh")

	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.True(flagged, "failedRepos must be set after per-item syncOpenIssue failure")

	// Clear the error, provide a comment, simulate warm cache.
	mc.listIssueCommentsErr = nil
	mc.comments = []*gh.IssueComment{recoveryComment}
	mc.issuesCached = true

	invalidateBefore := mc.invalidateCalls.Load()

	// Cycle 2: forceRefresh overrides needsTimeline even though
	// UpdatedAt hasn't changed → timeline refresh retried → comment lands.
	syncer.RunOnce(ctx)

	assert.Greater(mc.invalidateCalls.Load(), invalidateBefore,
		"next cycle should call InvalidateListETagsForRepo")

	// Verify timeline was actually refreshed: the comment should be in DB.
	issue, err = d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	events, err = d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Len(events, 1, "comment should be persisted after forced timeline retry")

	_, flagged = syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged, "failedRepos must be cleared after successful retry")
}

// TestSyncerClosedIssueFailureMarksRepoFailed verifies that when
// the open-issue list succeeds but fetchAndUpdateClosedIssue fails
// for a previously-open issue (here via a GetIssue API error),
// syncIssues returns an error, doSyncRepo marks the repo failed,
// and the next cycle retries after ETag invalidation.
func TestSyncerClosedIssueFailureMarksRepoFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	issueNumber := 7
	issueTitle := "will-close issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/7"
	issueBody := ""
	issueID := int64(777)
	openIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}

	// Seed issue #7 as open in DB via an initial sync with the
	// issue present in the open list.
	seedMC := &mockClient{
		openPRs:    []*gh.PullRequest{buildOpenPR(1, now)},
		openIssues: []*gh.Issue{openIssue},
		comments:   []*gh.IssueComment{},
		reviews:    []*gh.PullRequestReview{},
		commits:    []*gh.RepositoryCommit{},
	}

	seedSyncer := NewSyncer(
		map[string]Client{"github.com": seedMC},
		d, nil, repos, time.Minute, nil, nil,
	)
	seedSyncer.RunOnce(ctx)

	seeded, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(seeded, "seed cycle should persist issue #7")

	// Now build the real mock: open list returns EMPTY (issue #7
	// no longer open) → closure detection finds #7. GetIssue for
	// the closure path fails.
	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.openIssues = []*gh.Issue{} // issue #7 not in open list
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.getIssueErr = fmt.Errorf("transient API failure fetching closed issue")

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: list succeeds (empty), closure detection finds #7,
	// fetchAndUpdateClosedIssue fails → hadItemFailure → markFailure.
	syncer.RunOnce(ctx)

	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.True(flagged, "failedRepos must be set after fetchAndUpdateClosedIssue failure")

	// Verify issue is still open in DB (closure update failed).
	stillOpen, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(stillOpen)
	assert.Equal("open", stillOpen.State, "issue should still be open because closure update failed")

	// Clear error, simulate warm cache, provide closed issue data.
	mc.getIssueErr = nil
	closedState := "closed"
	closedIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &closedState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now.Add(time.Hour)),
	}
	mc.getIssueFn = func(_ context.Context, _, _ string, n int) (*gh.Issue, error) {
		if n == issueNumber {
			return closedIssue, nil
		}
		return nil, nil
	}
	mc.issuesCached = true

	invalidateBefore := mc.invalidateCalls.Load()

	// Cycle 2: invalidation → fresh list (empty) → closure
	// detection re-finds #7 → fetchAndUpdateClosedIssue succeeds.
	syncer.RunOnce(ctx)

	assert.Greater(mc.invalidateCalls.Load(), invalidateBefore,
		"next cycle should call InvalidateListETagsForRepo")

	updated, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(updated)
	assert.Equal("closed", updated.State, "issue should be closed after successful retry")

	_, flagged = syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged, "failedRepos must be cleared after successful retry")
}

func TestDisabledIssuesStopClosureDetectionAfterFirstLookup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}

	seedClient := &mockClient{
		openPRs:    []*gh.PullRequest{buildOpenPR(1, now)},
		openIssues: []*gh.Issue{buildOpenIssue(7, now), buildOpenIssue(8, now)},
		comments:   []*gh.IssueComment{},
		reviews:    []*gh.PullRequestReview{},
		commits:    []*gh.RepositoryCommit{},
	}
	NewSyncer(
		map[string]Client{"github.com": seedClient}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	).RunOnce(ctx)
	for _, number := range []int{7, 8} {
		issue, err := database.GetIssue(
			ctx, "github", "github.com", "owner", "repo", number,
		)
		require.NoError(err)
		require.NotNil(issue)
	}

	var getIssueCalls atomic.Int32
	client := &partialFailureMock{}
	client.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	client.openIssues = []*gh.Issue{}
	client.comments = []*gh.IssueComment{}
	client.reviews = []*gh.PullRequestReview{}
	client.commits = []*gh.RepositoryCommit{}
	client.getIssueFn = func(context.Context, string, string, int) (*gh.Issue, error) {
		getIssueCalls.Add(1)
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusGone},
			Message:  "Issues are disabled for this repo",
		}
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.RunOnce(ctx)

	assert.Equal(int32(1), getIssueCalls.Load())
	_, failed := syncer.failedRepos.Load(repoFailKey(repo))
	assert.False(failed)
}

func TestSyncIssuesFromListStopsAfterWrappedRawDisabledResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedDBRepoIdentity(platformRepoRef(repo)),
	)
	require.NoError(err)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	issues := []*gh.Issue{buildOpenIssue(1, now), buildOpenIssue(2, now)}
	rawDisabledErr := fmt.Errorf("list issue comments: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Issues are disabled for this repo",
	})
	var commentCalls atomic.Int32
	client := &mockClient{
		comments: []*gh.IssueComment{},
		listIssueCommentsFn: func(context.Context, string, string, int) ([]*gh.IssueComment, error) {
			commentCalls.Add(1)
			return nil, rawDisabledErr
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)

	err = syncer.syncIssuesFromList(ctx, client, repo, repoID, issues, false)
	require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
	assert.Equal(int32(1), commentCalls.Load())
}

func TestSyncIssuesFromListStopsAfterWrappedRawDisabledTimelineResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := database.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	client := &issueTimelineMockClient{
		comments: []*gh.IssueComment{},
		issueTimelineErr: fmt.Errorf("list issue timeline: %w", &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusGone},
			Message:  "Issues are disabled for this repo",
		}),
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)

	err = syncer.syncIssuesFromList(
		ctx, client, repo, repoID,
		[]*gh.Issue{buildOpenIssue(1, now), buildOpenIssue(2, now)}, false,
	)

	require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
	assert.Equal(int32(1), client.issueTimelineCalls.Load())
	assert.Equal(int32(1), client.listIssueCommentsCalled.Load())
}

func TestSyncIssuesFromListRetainsBestEffortTimelineErrors(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := database.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	client := &issueTimelineMockClient{
		comments:         []*gh.IssueComment{},
		issueTimelineErr: errors.New("timeline temporarily unavailable"),
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)

	err = syncer.syncIssuesFromList(
		ctx, client, repo, repoID, []*gh.Issue{buildOpenIssue(1, now)}, false,
	)

	require.NoError(err)
	require.Equal(int32(1), client.issueTimelineCalls.Load())
}

// TestSyncerMRListFailureMarksRepoFailed verifies that when the
// PR list fails, the MR path is marked failed, and the next cycle
// invalidates the ETag and retries. Also verifies issue path is NOT
// force-refreshed when only MR path failed (scoped failure tracking).
func TestSyncerMRListFailureMarksRepoFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.openIssues = []*gh.Issue{}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	// PR list fails on first call.
	mc.listOpenPRsErr = fmt.Errorf("transient PR list failure")
	var issueListCalls atomic.Int32
	mc.listOpenIssuesFn = func(context.Context, string, string) ([]*gh.Issue, error) {
		issueListCalls.Add(1)
		return nil, nil
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: PR list fails → failMR set, issues unaffected.
	syncer.RunOnce(ctx)
	assert.Zero(int(issueListCalls.Load()),
		"a non-disabled PR list failure must abort before issue sync")

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	assert.Nil(mr, "MR should not be upserted when PR list failed")

	v, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.True(flagged, "failedRepos must be set after PR list failure")
	assert.Equal(failMR, v.(failScope), "only failMR scope should be set")

	// Clear error, simulate warm caches.
	mc.listOpenPRsErr = nil
	mc.prsCached = false // allow next list to succeed
	mc.issuesCached = true

	invalidateBefore := mc.invalidateCalls.Load()

	// Cycle 2: ETag invalidated for pulls only → fresh PR list → MR upserted.
	// Issue cache should remain warm (only pulls invalidated).
	syncer.RunOnce(ctx)

	assert.Greater(mc.invalidateCalls.Load(), invalidateBefore,
		"next cycle should call InvalidateListETagsForRepo")

	// Issue cache must still be warm — MR-only failure should not
	// invalidate issue ETags.
	assert.True(mc.issuesCached,
		"issue cache should stay warm when only MR path failed")

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr, "MR should be upserted after successful retry")

	_, flagged = syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged, "failedRepos must be cleared after successful retry")
}

// TestSyncerIssuesOnlyRepoDisablesMergeRequestFeature verifies that a
// repository with pull requests disabled (GitHub issues-only repositories
// report has_pull_requests=false and 404 on the pulls API) enters the
// merge-request feature cooldown instead of hard-failing every cycle, and
// that its issues still sync in the same cycle. Without the classification
// the pulls 404 aborts the repo sync before the issue phase, so an
// issues-only tracker never syncs the one thing it exists for.
func TestSyncerIssuesOnlyRepoDisablesMergeRequestFeature(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	issueNumber := 7
	issueTitle := "product issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/7"
	issueBody := ""
	issueID := int64(777)
	openIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}

	var prListCalls atomic.Int32
	mc := &partialFailureMock{}
	mc.listOpenPRsFn = func(context.Context, string, string) ([]*gh.PullRequest, error) {
		prListCalls.Add(1)
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotFound},
			Message:  "Not Found",
		}
	}
	mc.getRepositoryFn = func(_ context.Context, owner, repo string) (*gh.Repository, error) {
		id := int64(1)
		nodeID := "repo-" + owner + "-" + repo
		prsDisabled := false
		return &gh.Repository{
			ID:              &id,
			NodeID:          &nodeID,
			Name:            &repo,
			Owner:           &gh.User{Login: &owner},
			HasPullRequests: &prsDisabled,
		}, nil
	}
	mc.openIssues = []*gh.Issue{openIssue}
	mc.comments = []*gh.IssueComment{}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: pulls 404 classifies as feature-disabled; issues still sync.
	syncer.RunOnce(ctx)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue,
		"issues must sync even though the pulls API is unavailable")
	assert.Equal("open", issue.State)

	assert.Empty(syncer.Status().LastError,
		"an issues-only repository is not a sync failure")
	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged, "feature-disabled must not mark the repo failed")
	callsAfterFirstCycle := prListCalls.Load()

	// Cycle 2: the merge-request feature cooldown skips the pulls list.
	syncer.RunOnce(ctx)
	assert.Equal(callsAfterFirstCycle, prListCalls.Load(),
		"deferred merge-request feature must not re-probe next cycle")
}

// TestSyncerRemovedIssueTombstonedInsteadOfFailing verifies that an issue
// deleted upstream (open in a previous cycle, absent from the open list,
// direct lookup classified not_found) is closed locally instead of failing
// the repo's issue sync. Without the tombstone the number stays "previously
// open" forever: every cycle re-fetches it, fails the repo, and spends a
// lookup plus repository probe on an item that can never resolve.
func TestSyncerRemovedIssueTombstonedInsteadOfFailing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	issueNumber := 7
	issueTitle := "will be deleted upstream"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/7"
	issueBody := ""
	issueID := int64(777)
	openIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Body:      &issueBody,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}

	var getIssueCalls atomic.Int32
	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{}
	mc.openIssues = []*gh.Issue{openIssue}
	mc.comments = []*gh.IssueComment{}
	mc.getIssueFn = func(context.Context, string, string, int) (*gh.Issue, error) {
		getIssueCalls.Add(1)
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotFound},
			Message:  "Not Found",
		}
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: issue is open and synced.
	syncer.RunOnce(ctx)
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	require.Equal("open", issue.State)

	// Cycle 2: issue vanished from the open list; direct lookup 404s.
	mc.openIssues = []*gh.Issue{}
	mc.issuesCached = false
	syncer.RunOnce(ctx)

	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged,
		"removed issue must not mark the repo failed")
	issue, err = d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue, "local copy should remain as a tombstone")
	assert.Equal("closed", issue.State,
		"removed issue must be closed locally")
	callsAfterTombstone := getIssueCalls.Load()

	// Cycle 3: closure detection no longer retries the removed issue.
	mc.issuesCached = false
	syncer.RunOnce(ctx)
	assert.Equal(callsAfterTombstone, getIssueCalls.Load(),
		"tombstoned issue must not be re-fetched")
}

// TestFetchAndUpdateClosedIssueTombstonesPRShapedResponse verifies that the
// issue-only closure path does not normalize a pull request returned by the
// Issues API back into an issue. Without the tombstone, the stale open row
// remains eligible for the same lookup on every repository sync.
func TestFetchAndUpdateClosedIssueTombstonesPRShapedResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	issue := buildOpenIssue(7, now)
	normalized, err := NormalizeIssue(repoID, issue)
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, normalized)
	require.NoError(err)

	pullURL := "https://api.github.com/repos/owner/repo/pulls/7"
	issue.PullRequestLinks = &gh.PullRequestLinks{URL: &pullURL}
	client := &mockClient{
		getIssueFn: func(context.Context, string, string, int) (*gh.Issue, error) {
			return issue, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.fetchAndUpdateClosedIssue(ctx, repo, repoID, 7))
	stored, err := database.GetIssueByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("closed", stored.State)
}

// TestSyncerBudgetRefusedPRListSkipsETagEviction verifies that when the
// open-PR list fetch is refused by the local sync budget ceiling, the repo
// is not marked for ETag eviction: the refusal happened before any wire
// attempt, so the cached list validators are still correct and the next
// cycle must reuse them instead of forcing an unconditional refetch that
// spends budget the refusal just proved is unavailable.
func TestSyncerBudgetRefusedPRListSkipsETagEviction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: clean sync warms the list caches.
	syncer.RunOnce(ctx)
	require.True(mc.prsCached, "first cycle should warm the PR list cache")
	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	require.False(flagged, "clean cycle must not flag the repo")

	// Cycle 2: the budget transport refuses the PR list before any I/O.
	mc.listOpenPRsErr = fmt.Errorf(
		"Get %q: %w",
		"https://api.github.com/repos/owner/repo/pulls",
		platform.ErrSyncBudgetExhausted,
	)
	syncer.RunOnce(ctx)

	_, flagged = syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged,
		"budget refusal must not mark the repo for ETag eviction")
	assert.Contains(syncer.Status().LastError, "list open PRs",
		"budget refusal must still surface as a sync error")
	assert.Equal(SyncErrorCodeLocalCeilingExhausted, syncer.Status().LastErrorCode,
		"budget refusal must identify the local ceiling independently of provider quota")

	// Cycle 3: budget available again. The list must go through the
	// conditional path (warm cache → 304), not an eviction-forced refetch.
	mc.listOpenPRsErr = nil
	invalidateBefore := mc.invalidateCalls.Load()
	syncer.RunOnce(ctx)

	assert.Equal(invalidateBefore, mc.invalidateCalls.Load(),
		"recovery cycle must not invalidate list ETags")
	assert.True(mc.prsCached, "cached PR list validator must stay warm")
}

func TestSyncerBudgetCauseSurvivesLaterIssueListFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	normalized, err := NormalizePR(repoID, buildOpenPR(1, now))
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, normalized)
	require.NoError(err)

	client := &partialFailureMock{}
	client.openPRs = []*gh.PullRequest{}
	client.getPullRequestFn = func(context.Context, string, string, int) (*gh.PullRequest, error) {
		return nil, fmt.Errorf("get closed merge request: %w", platform.ErrSyncBudgetExhausted)
	}
	client.listOpenIssuesFn = func(context.Context, string, string) ([]*gh.Issue, error) {
		return nil, errors.New("issue list unavailable")
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		repos, time.Minute, nil, nil,
	)
	syncer.RunOnce(ctx)

	assert.Equal(SyncErrorCodeLocalCeilingExhausted, syncer.Status().LastErrorCode,
		"a later non-budget failure must not mask the earlier local-ceiling cause")
}

// TestSyncerBudgetRefusedIssueListSkipsETagEviction verifies the same
// no-eviction rule for the open-issue list: a budget refusal fails the
// cycle but leaves the issue list validators warm for the next cycle.
func TestSyncerBudgetRefusedIssueListSkipsETagEviction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, nil,
	)

	// Cycle 1: clean sync warms both list caches.
	syncer.RunOnce(ctx)
	require.True(mc.issuesCached, "first cycle should warm the issue list cache")

	// Cycle 2: budget refuses the issue list; the PR list stays warm (304).
	mc.listOpenIssuesErr = fmt.Errorf(
		"Get %q: %w",
		"https://api.github.com/repos/owner/repo/issues",
		platform.ErrSyncBudgetExhausted,
	)
	syncer.RunOnce(ctx)

	_, flagged := syncer.failedRepos.Load(repoFailKey(repos[0]))
	assert.False(flagged,
		"budget refusal must not mark the repo for ETag eviction")

	// Cycle 3: budget available again — no eviction-forced refetch.
	mc.listOpenIssuesErr = nil
	invalidateBefore := mc.invalidateCalls.Load()
	syncer.RunOnce(ctx)

	assert.Equal(invalidateBefore, mc.invalidateCalls.Load(),
		"recovery cycle must not invalidate list ETags")
	assert.True(mc.issuesCached, "cached issue list validator must stay warm")
}

// TestSyncerListFetchesUseEssentialBudgetContext verifies that the open-PR
// and open-issue list fetches — the calls that discover new and closed items
// — run under the essential sync-budget context so they can spend the
// reserved headroom, while per-item detail fetches stay optional.
func TestSyncerListFetchesUseEssentialBudgetContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	var prListEssential, issueListEssential atomic.Bool
	var reviewsCalled, reviewsEssential atomic.Bool

	mc := &partialFailureMock{}
	// openPRs also backs the mock's GetPullRequest for the detail drain.
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.listOpenPRsFn = func(fnCtx context.Context, _, _ string) ([]*gh.PullRequest, error) {
		prListEssential.Store(IsEssentialSyncBudgetContext(fnCtx))
		return mc.openPRs, nil
	}
	mc.listOpenIssuesFn = func(fnCtx context.Context, _, _ string) ([]*gh.Issue, error) {
		issueListEssential.Store(IsEssentialSyncBudgetContext(fnCtx))
		return nil, nil
	}
	mc.listReviewsFn = func(fnCtx context.Context, _, _ string, _ int) ([]*gh.PullRequestReview, error) {
		reviewsCalled.Store(true)
		reviewsEssential.Store(IsEssentialSyncBudgetContext(fnCtx))
		return []*gh.PullRequestReview{}, nil
	}
	mc.comments = []*gh.IssueComment{}
	mc.commits = []*gh.RepositoryCommit{}
	ciState := "success"
	mc.ciStatus = &gh.CombinedStatus{State: &ciState}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)
	syncer.RunOnce(ctx)

	assert.True(prListEssential.Load(),
		"open-PR list fetch must run under the essential budget context")
	assert.True(issueListEssential.Load(),
		"open-issue list fetch must run under the essential budget context")
	require.True(reviewsCalled.Load(),
		"PR timeline reviews fetch should have run")
	assert.False(reviewsEssential.Load(),
		"detail fetches must stay on the optional budget")
}

// TestSyncerMRDetailFailureRetries verifies that when fetchMRDetail
// fails during timeline refresh (via ListReviews error), the MR's
// detail_fetched_at stays nil so the detail queue picks it up again
// on the next cycle.
func TestSyncerMRDetailFailureRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}
	ciState := "success"

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}
	mc.openIssues = []*gh.Issue{}
	mc.comments = []*gh.IssueComment{}
	mc.reviews = []*gh.PullRequestReview{}
	mc.commits = []*gh.RepositoryCommit{}
	mc.ciStatus = &gh.CombinedStatus{State: &ciState}
	// Timeline refresh fails at ListReviews during detail fetch.
	mc.listReviewsErr = fmt.Errorf("transient reviews failure")

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)

	// Cycle 1: index upserts MR, detail drain calls fetchMRDetail →
	// refreshTimeline fails at ListReviews → detail_fetched_at stays nil.
	syncer.RunOnce(ctx)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr, "MR should be upserted by index phase")
	assert.Nil(mr.DetailFetchedAt,
		"detail_fetched_at should be nil after failed detail fetch")

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events, "no events should exist after failed timeline refresh")

	// Clear error, add a review for cycle 2.
	mc.listReviewsErr = nil
	reviewID := int64(500)
	reviewState := "APPROVED"
	reviewUser := "reviewer"
	reviewBody := "lgtm"
	mc.reviews = []*gh.PullRequestReview{{
		ID:          &reviewID,
		State:       &reviewState,
		Body:        &reviewBody,
		SubmittedAt: makeTimestamp(now),
		User:        &gh.User{Login: &reviewUser},
	}}

	// Cycle 2: detail drain picks up MR again (detail_fetched_at nil)
	// → fetchMRDetail succeeds → timeline events land.
	syncer.RunOnce(ctx)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.NotNil(mr.DetailFetchedAt,
		"detail_fetched_at should be set after successful detail fetch")

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.NotEmpty(events, "review event should be persisted after detail retry")
}

func TestSyncerRefreshesEditedPRCommentWhenPRListIsUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	commentID := int64(7001)
	commentUser := "reviewer"
	commentBody := "original body"
	commentUpdatedAt := now.Add(2 * time.Minute)

	mc := &mockClient{
		openIssues: []*gh.Issue{},
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			User:      &gh.User{Login: &commentUser},
			CreatedAt: makeTimestamp(commentUpdatedAt),
			UpdatedAt: makeTimestamp(commentUpdatedAt),
		}},
	}
	mc.getPullRequestFn = func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
		require.Equal(1, number)
		return buildOpenPR(1, now), nil
	}
	prListCalls := 0
	mc.listOpenPRsFn = func(_ context.Context, _, _ string) ([]*gh.PullRequest, error) {
		prListCalls++
		if prListCalls == 1 {
			return []*gh.PullRequest{buildOpenPR(1, now)}, nil
		}
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotModified},
		}
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)

	syncer.RunOnce(ctx)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("original body", events[0].Body)

	editedBody := "edited body"
	editedAt := now.Add(4 * time.Minute)
	mc.comments = []*gh.IssueComment{{
		ID:        &commentID,
		Body:      &editedBody,
		User:      &gh.User{Login: &commentUser},
		CreatedAt: makeTimestamp(commentUpdatedAt),
		UpdatedAt: makeTimestamp(editedAt),
	}}

	syncer.RunOnce(ctx)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("edited body", events[0].Body)
}

func TestSyncerRemovesDeletedPRCommentWhenPRListIsUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	commentID := int64(7002)
	commentUser := "reviewer"
	commentBody := "to be deleted"
	commentTime := now.Add(2 * time.Minute)

	mc := &mockClient{
		openIssues: []*gh.Issue{},
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			User:      &gh.User{Login: &commentUser},
			CreatedAt: makeTimestamp(commentTime),
			UpdatedAt: makeTimestamp(commentTime),
		}},
	}
	mc.getPullRequestFn = func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
		require.Equal(1, number)
		return buildOpenPR(1, now), nil
	}
	prListCalls := 0
	mc.listOpenPRsFn = func(_ context.Context, _, _ string) ([]*gh.PullRequest, error) {
		prListCalls++
		if prListCalls == 1 {
			return []*gh.PullRequest{buildOpenPR(1, now)}, nil
		}
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotModified},
		}
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)

	syncer.RunOnce(ctx)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	require.Equal(1, mr.CommentCount)

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)

	mc.comments = []*gh.IssueComment{}

	syncer.RunOnce(ctx)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(0, mr.CommentCount)

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events)
	assert.Equal(now.UTC(), mr.LastActivityAt.UTC())
}

func TestSyncerRemovesDeletedIssueCommentWhenIssueListIsUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repos := []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}

	issueID := int64(801)
	issueNumber := 8
	issueTitle := "edited issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/8"
	openIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		CreatedAt: makeTimestamp(now),
		UpdatedAt: makeTimestamp(now),
	}

	commentID := int64(810)
	commentUser := "reviewer"
	commentBody := "issue comment"
	commentTime := now.Add(2 * time.Minute)

	mc := &partialFailureMock{}
	mc.openPRs = []*gh.PullRequest{}
	mc.openIssues = []*gh.Issue{openIssue}
	mc.comments = []*gh.IssueComment{{
		ID:        &commentID,
		Body:      &commentBody,
		User:      &gh.User{Login: &commentUser},
		CreatedAt: makeTimestamp(commentTime),
		UpdatedAt: makeTimestamp(commentTime),
	}}
	mc.listOpenPRsErr = notModifiedErr()
	mc.getIssueFn = func(_ context.Context, _, _ string, number int) (*gh.Issue, error) {
		require.Equal(issueNumber, number)
		return openIssue, nil
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, repos, time.Minute, nil, testBudget(10000),
	)

	syncer.RunOnce(ctx)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	require.Equal(1, issue.CommentCount)

	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 1)

	mc.comments = []*gh.IssueComment{}
	mc.issuesCached = true

	syncer.RunOnce(ctx)

	issue, err = d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal(0, issue.CommentCount)

	events, err = d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Empty(events)
	assert.Equal(now.UTC(), issue.LastActivityAt.UTC())
}

func TestFetchMRDetailRemovesDeletedCommentDuringFullRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 2, 12, 0, 0, 0, time.UTC)
	firstUpdatedAt := now.Add(3 * time.Minute)
	secondUpdatedAt := now.Add(4 * time.Minute)
	commentID := int64(7101)
	commentAuthor := "reviewer"
	commentBody := "full refresh comment"
	commentTime := now.Add(2 * time.Minute)

	fetches := 0
	mc := &mockClient{
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: makeTimestamp(commentTime),
			UpdatedAt: makeTimestamp(commentTime),
		}},
		getPullRequestFn: func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
			require.Equal(1, number)
			fetches++
			if fetches == 1 {
				return buildOpenPR(1, firstUpdatedAt), nil
			}
			return buildOpenPR(1, secondUpdatedAt), nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	_, err = syncer.fetchMRDetail(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, 1, false,
	)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	require.Equal(1, mr.CommentCount)
	assert.Equal(firstUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)

	mc.comments = []*gh.IssueComment{}

	_, err = syncer.fetchMRDetail(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, 1, false,
	)
	require.NoError(err)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(0, mr.CommentCount)
	assert.Equal(secondUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestFetchIssueDetailRemovesDeletedCommentDuringFullRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 2, 12, 0, 0, 0, time.UTC)
	firstUpdatedAt := now
	secondUpdatedAt := now.Add(time.Minute)
	issueID := int64(820)
	issueNumber := 8
	issueTitle := "full refresh issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/8"
	commentID := int64(821)
	commentAuthor := "reviewer"
	commentBody := "full refresh issue comment"
	commentTime := now.Add(2 * time.Minute)

	fetches := 0
	mc := &mockClient{
		comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: makeTimestamp(commentTime),
			UpdatedAt: makeTimestamp(commentTime),
		}},
		getIssueFn: func(_ context.Context, _, _ string, number int) (*gh.Issue, error) {
			require.Equal(issueNumber, number)
			fetches++
			updatedAt := firstUpdatedAt
			if fetches > 1 {
				updatedAt = secondUpdatedAt
			}
			return &gh.Issue{
				ID:        &issueID,
				Number:    &issueNumber,
				Title:     &issueTitle,
				State:     &issueState,
				HTMLURL:   &issueURL,
				CreatedAt: makeTimestamp(now),
				UpdatedAt: makeTimestamp(updatedAt),
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	_, err = syncer.fetchIssueDetail(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, issueNumber,
	)
	require.NoError(err)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	require.Equal(1, issue.CommentCount)
	assert.Equal(commentTime.UTC(), issue.LastActivityAt.UTC())

	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 1)

	mc.comments = []*gh.IssueComment{}

	_, err = syncer.fetchIssueDetail(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, issueNumber,
	)
	require.NoError(err)

	issue, err = d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal(0, issue.CommentCount)
	assert.Equal(secondUpdatedAt.UTC(), issue.LastActivityAt.UTC())

	events, err = d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestFetchMRDetailRejectsABARoutePayload(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &mockClient{getPullRequestFn: func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		close(started)
		<-release
		pr := buildOpenPR(7, observedAt.Add(time.Hour))
		pr.Title = new("replacement payload")
		return pr, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchMRDetail(
			ctx, repo, original.Repository.ID, 7, false,
		)
		done <- fetchErr
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.Nil(mr)
}

func TestFetchMRDetailRejectsChildSnapshotAfterABARouteReuse(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	aboutToWriteChild := make(chan struct{})
	abaDone := make(chan struct{})
	abaErr := make(chan error, 1)
	commentID := int64(91)
	commentBody := "replacement repository comment"
	client := &mockClient{
		getPullRequestFn: func(
			context.Context, string, string, int,
		) (*gh.PullRequest, error) {
			return buildOpenPR(7, observedAt.Add(time.Hour)), nil
		},
		listIssueCommentsFn: func(
			context.Context, string, string, int,
		) ([]*gh.IssueComment, error) {
			close(aboutToWriteChild)
			<-abaDone
			return []*gh.IssueComment{{
				ID: &commentID, Body: &commentBody,
				CreatedAt: makeTimestamp(observedAt.Add(time.Hour)),
				UpdatedAt: makeTimestamp(observedAt.Add(time.Hour)),
			}}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.afterMergeRequestParentSnapshotCommit = func() {
		go func() {
			abaErr <- reconcileRepositoryRouteABA(
				ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
			)
			close(abaDone)
		}()
	}

	_, err = syncer.fetchMRDetail(ctx, repo, original.Repository.ID, 7, false)
	require.NoError(err)
	select {
	case <-aboutToWriteChild:
	default:
	}
	<-abaDone
	require.NoError(<-abaErr)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Empty(events)
}

func TestFetchIssueDetailRejectsABARoutePayload(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &mockClient{getIssueFn: func(
		context.Context, string, string, int,
	) (*gh.Issue, error) {
		close(started)
		<-release
		issue := buildOpenIssue(7, observedAt.Add(time.Hour))
		issue.Title = new("replacement payload")
		return issue, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchIssueDetail(
			ctx, repo, original.Repository.ID, 7,
		)
		done <- fetchErr
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	issue, err := database.GetIssueByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.Nil(issue)
}

func TestFetchIssueDetailRejectsChildSnapshotAfterABARouteReuse(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	childFetchStarted := make(chan struct{})
	releaseChildFetch := make(chan struct{})
	commentID := int64(92)
	commentBody := "replacement repository issue comment"
	client := &mockClient{
		getIssueFn: func(
			context.Context, string, string, int,
		) (*gh.Issue, error) {
			return buildOpenIssue(7, observedAt.Add(time.Hour)), nil
		},
		listIssueCommentsFn: func(
			context.Context, string, string, int,
		) ([]*gh.IssueComment, error) {
			close(childFetchStarted)
			<-releaseChildFetch
			return []*gh.IssueComment{{
				ID: &commentID, Body: &commentBody,
				CreatedAt: makeTimestamp(observedAt.Add(time.Hour)),
				UpdatedAt: makeTimestamp(observedAt.Add(time.Hour)),
			}}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchIssueDetail(
			ctx, repo, original.Repository.ID, 7,
		)
		done <- fetchErr
	}()
	<-childFetchStarted
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(releaseChildFetch)
	require.NoError(<-done)

	issue, err := database.GetIssueByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(issue)
	events, err := database.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Empty(events)
}

func TestDeferredCommentRefreshRejectsABARoutePayload(t *testing.T) {
	t.Run("pull request", func(t *testing.T) {
		require := require.New(t)
		database := openTestDB(t)
		ctx := t.Context()
		observedAt := time.Now().UTC()
		repo := RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
			PlatformExternalID: "repo-a",
		}
		original, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		}, observedAt)
		require.NoError(err)
		detailFetchedAt := observedAt
		_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: original.Repository.ID, PlatformID: 7007, Number: 7,
			URL: "https://github.com/acme/widget/pull/7", Title: "original",
			State: db.MergeRequestStateOpen, CreatedAt: observedAt,
			UpdatedAt: observedAt, LastActivityAt: observedAt,
			DetailFetchedAt: &detailFetchedAt,
		})
		require.NoError(err)

		started := make(chan struct{})
		release := make(chan struct{})
		commentID, body, login := int64(91), "replacement comment", "reviewer"
		client := &mockClient{listIssueCommentsIfChangedFn: func(
			context.Context, string, string, int,
		) ([]*gh.IssueComment, error) {
			close(started)
			<-release
			return []*gh.IssueComment{{
				ID: &commentID, Body: &body, User: &gh.User{Login: &login},
				CreatedAt: makeTimestamp(observedAt.Add(time.Hour)),
				UpdatedAt: makeTimestamp(observedAt.Add(time.Hour)),
			}}, nil
		}}
		syncer := NewSyncer(
			map[string]Client{"github.com": client}, database, nil,
			[]RepoRef{repo}, time.Minute, nil, nil,
		)
		syncer.queuePRCommentSync(repo, original.Repository.ID, 7)
		done := make(chan struct{})
		go func() {
			syncer.drainPendingCommentSyncs(ctx, map[string]bool{"github.com": true})
			close(done)
		}()
		<-started
		require.NoError(reconcileRepositoryRouteABA(
			ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
		))
		close(release)
		<-done

		stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, original.Repository.ID, 7)
		require.NoError(err)
		require.NotNil(stored)
		events, err := database.ListMREvents(ctx, stored.ID)
		require.NoError(err)
		require.Empty(events)
	})

	t.Run("issue", func(t *testing.T) {
		require := require.New(t)
		database := openTestDB(t)
		ctx := t.Context()
		observedAt := time.Now().UTC()
		repo := RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
			PlatformExternalID: "repo-a",
		}
		original, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		}, observedAt)
		require.NoError(err)
		detailFetchedAt := observedAt
		_, err = database.UpsertIssue(ctx, &db.Issue{
			RepoID: original.Repository.ID, PlatformID: 8007, Number: 7,
			URL: "https://github.com/acme/widget/issues/7", Title: "original",
			State: "open", CreatedAt: observedAt, UpdatedAt: observedAt,
			LastActivityAt: observedAt, DetailFetchedAt: &detailFetchedAt,
		})
		require.NoError(err)

		started := make(chan struct{})
		release := make(chan struct{})
		commentID, body, login := int64(92), "replacement comment", "reviewer"
		client := &mockClient{listIssueCommentsIfChangedFn: func(
			context.Context, string, string, int,
		) ([]*gh.IssueComment, error) {
			close(started)
			<-release
			return []*gh.IssueComment{{
				ID: &commentID, Body: &body, User: &gh.User{Login: &login},
				CreatedAt: makeTimestamp(observedAt.Add(time.Hour)),
				UpdatedAt: makeTimestamp(observedAt.Add(time.Hour)),
			}}, nil
		}}
		syncer := NewSyncer(
			map[string]Client{"github.com": client}, database, nil,
			[]RepoRef{repo}, time.Minute, nil, nil,
		)
		syncer.queueIssueCommentSync(repo, original.Repository.ID, 7)
		done := make(chan struct{})
		go func() {
			syncer.drainPendingCommentSyncs(ctx, map[string]bool{"github.com": true})
			close(done)
		}()
		<-started
		require.NoError(reconcileRepositoryRouteABA(
			ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
		))
		close(release)
		<-done

		stored, err := database.GetIssueByRepoIDAndNumber(ctx, original.Repository.ID, 7)
		require.NoError(err)
		require.NotNil(stored)
		events, err := database.ListIssueEvents(ctx, stored.ID)
		require.NoError(err)
		require.Empty(events)
	})
}

func TestDeferredCommentRefreshSkipsRemovedUpstreamItems(t *testing.T) {
	for _, itemType := range []db.ArchiveItemType{
		db.ArchiveItemTypeMergeRequest,
		db.ArchiveItemTypeIssue,
	} {
		t.Run(string(itemType), func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			database := openTestDB(t)
			now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			repo := RepoRef{
				Platform: platform.KindGitHub, PlatformHost: "github.com",
				Owner: "acme", Name: "widget",
			}
			repoID, err := database.UpsertRepo(
				ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
			)
			require.NoError(err)
			detailFetchedAt := now
			var parentID int64
			if itemType == db.ArchiveItemTypeMergeRequest {
				parentID, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
					RepoID: repoID, PlatformID: 7007, Number: 7,
					URL: "https://github.com/acme/widget/pull/7", Title: "queued PR",
					Author: "ada", State: db.MergeRequestStateOpen,
					CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
					DetailFetchedAt: &detailFetchedAt,
				})
			} else {
				parentID, err = database.UpsertIssue(ctx, &db.Issue{
					RepoID: repoID, PlatformID: 8007, Number: 7,
					URL: "https://github.com/acme/widget/issues/7", Title: "queued issue",
					Author: "ada", State: "open",
					CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
					DetailFetchedAt: &detailFetchedAt,
				})
			}
			require.NoError(err)

			var providerCalls atomic.Int32
			commentID, body := int64(91), "must not persist"
			client := &mockClient{listIssueCommentsIfChangedFn: func(
				context.Context, string, string, int,
			) ([]*gh.IssueComment, error) {
				providerCalls.Add(1)
				return []*gh.IssueComment{{
					ID: &commentID, Body: &body,
					CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now),
				}}, nil
			}}
			syncer := NewSyncer(
				map[string]Client{"github.com": client}, database, nil,
				[]RepoRef{repo}, time.Minute, nil, nil,
			)
			if itemType == db.ArchiveItemTypeMergeRequest {
				syncer.queuePRCommentSync(repo, repoID, 7)
			} else {
				syncer.queueIssueCommentSync(repo, repoID, 7)
			}
			_, err = database.WriteDB().ExecContext(ctx, `
				INSERT INTO forge_archive_items (
					repo_id, item_type, item_number, provider_item_id,
					provider_created_at, provider_updated_at, lifecycle_state
				) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
				repoID, itemType, 7, string(itemType)+"-7", now, now,
			)
			require.NoError(err)

			syncer.drainPendingCommentSyncs(
				ctx, map[string]bool{"github.com": true},
			)

			require.Zero(providerCalls.Load(),
				"a queued comment refresh must recheck parent visibility")
			if itemType == db.ArchiveItemTypeMergeRequest {
				events, listErr := database.ListMREvents(ctx, parentID)
				require.NoError(listErr)
				require.Empty(events)
			} else {
				events, listErr := database.ListIssueEvents(ctx, parentID)
				require.NoError(listErr)
				require.Empty(events)
			}
		})
	}
}

func TestFetchMRDetailRejectsABAOnNotModified(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: original.Repository.ID, PlatformID: 7007, Number: 7,
		URL: "https://github.com/acme/widget/pull/7", Title: "original",
		State: db.MergeRequestStateOpen, CreatedAt: observedAt,
		UpdatedAt: observedAt, LastActivityAt: observedAt,
	})
	require.NoError(err)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &conditionalPRTrackingClient{notModified: true}
	client.beforeReturn = func() {
		close(started)
		<-release
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchMRDetail(
			ctx, repo, original.Repository.ID, 7, false,
		)
		done <- fetchErr
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(mr)
	require.Nil(mr.DetailFetchedAt)
}

func TestFetchMRDetailRejectsCIFromABAOnNotModified(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-a",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, observedAt)
	require.NoError(err)
	const oldChecks = `[{"name":"old","status":"in_progress"}]`
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: original.Repository.ID, PlatformID: 7007, Number: 7,
		URL: "https://github.com/acme/widget/pull/7", Title: "original",
		State: db.MergeRequestStateOpen, PlatformHeadSHA: "head-a",
		CIStatus: "pending", CIChecksJSON: oldChecks, CIHadPending: true,
		CreatedAt: observedAt, UpdatedAt: observedAt, LastActivityAt: observedAt,
	})
	require.NoError(err)

	started := make(chan struct{})
	release := make(chan struct{})
	client := &conditionalPRTrackingClient{notModified: true}
	client.ciStatus = &gh.CombinedStatus{State: new("success")}
	client.listCheckRunsForRefFn = func(
		context.Context, string, string, string,
	) ([]*gh.CheckRun, error) {
		close(started)
		<-release
		return []*gh.CheckRun{{
			Name: new("replacement"), Status: new("completed"), Conclusion: new("success"),
		}}, nil
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchMRDetail(
			ctx, repo, original.Repository.ID, 7, false,
		)
		done <- fetchErr
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, original.Repository.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	require.Equal("pending", mr.CIStatus)
	require.JSONEq(oldChecks, mr.CIChecksJSON)
	require.Nil(mr.DetailFetchedAt)
}

func TestFetchIssueDetailRejectsABAOnNotModified(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID: original.Repository.ID, PlatformID: 8007, Number: 7,
		URL: "https://github.com/acme/widget/issues/7", Title: "original",
		State: "open", CreatedAt: observedAt,
		UpdatedAt: observedAt, LastActivityAt: observedAt,
	})
	require.NoError(err)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &conditionalIssueTrackingClient{notModified: true}
	client.beforeReturn = func() {
		close(started)
		<-release
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() {
		_, fetchErr := syncer.fetchIssueDetail(
			ctx, repo, original.Repository.ID, 7,
		)
		done <- fetchErr
	}()
	<-started
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(release)
	require.NoError(<-done)

	issue, err := database.GetIssueByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(issue)
	require.Nil(issue.DetailFetchedAt)
}

func TestSyncRepoRejectsListSnapshotAfterABARouteReuse(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	client := &mockClient{
		getRepositoryFn: func(
			context.Context, string, string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				ID: new(int64(1)), NodeID: new("repo-a"),
				Owner: &gh.User{Login: new("acme")}, Name: new("widget"),
				Archived: new(false),
			}, nil
		},
		listOpenPRsFn: func(
			context.Context, string, string,
		) ([]*gh.PullRequest, error) {
			close(listStarted)
			<-releaseList
			pr := buildOpenPR(7, observedAt.Add(time.Hour))
			pr.Title = new("replacement repository PR")
			return []*gh.PullRequest{pr}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	done := make(chan error, 1)
	go func() { done <- syncer.syncRepo(ctx, repo) }()
	<-listStarted
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(releaseList)
	require.NoError(<-done)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, original.Repository.ID, 7,
	)
	require.NoError(err)
	require.Nil(mr)
}

func reconcileRepositoryRouteABA(
	ctx context.Context,
	database *db.DB,
	observedAt time.Time,
	originalProviderID, replacementProviderID, owner, name string,
) error {
	if now := time.Now().UTC(); now.After(observedAt) {
		observedAt = now
	}
	_, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: originalProviderID, Owner: owner, Name: "renamed",
		RepoPath: owner + "/renamed",
	}, observedAt.Add(time.Nanosecond))
	if err != nil {
		return err
	}
	_, _, err = database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: replacementProviderID, Owner: owner, Name: name,
		RepoPath: owner + "/" + name,
	}, observedAt.Add(2*time.Nanosecond))
	if err != nil {
		return err
	}
	_, _, err = database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: replacementProviderID, Owner: owner, Name: "elsewhere",
		RepoPath: owner + "/elsewhere",
	}, observedAt.Add(3*time.Nanosecond))
	if err != nil {
		return err
	}
	_, _, err = database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: originalProviderID, Owner: owner, Name: name,
		RepoPath: owner + "/" + name,
	}, observedAt.Add(4*time.Nanosecond))
	return err
}

func TestSyncClosedMROnProviderDropsSnapshotAcrossABARouteReuse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-a", Owner: "acme", Name: "widget",
		RepoPath: "acme/widget",
	}
	entry, _, err := database.ReconcileRepositoryObservation(
		ctx, identity, observedAt,
	)
	require.NoError(err)
	now := observedAt.Add(-time.Hour)
	original := buildOpenPR(7, now)
	original.Title = new("repository A")
	normalized, err := NormalizePR(entry.Repository.ID, original)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, normalized)
	require.NoError(err)

	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	client := &mockClient{getPullRequestFn: func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		close(fetchStarted)
		<-releaseFetch
		stale := buildOpenPR(7, observedAt.Add(time.Hour))
		stale.Title = new("repository B")
		stale.State = new("closed")
		stale.ClosedAt = makeTimestamp(observedAt.Add(time.Hour))
		return stale, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			PlatformExternalID: "repo-a", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}},
		time.Minute, nil, testBudget(100),
	)
	done := make(chan error, 1)
	go func() {
		done <- syncer.SyncClosedMROnProvider(
			ctx, entry.Repository.ID, 7,
		)
	}()
	<-fetchStarted
	require.NoError(reconcileRepositoryRouteABA(
		ctx, database, observedAt, "repo-a", "repo-b", "acme", "widget",
	))
	close(releaseFetch)
	require.NoError(<-done)

	stored, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, entry.Repository.ID, 7,
	)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("repository A", stored.Title)
	assert.Equal(db.MergeRequestStateOpen, stored.State)
}

func currentCommentVisibilityServer(
	t *testing.T,
	item string,
	commentID int64,
	states ...CommentVisibility,
) *httptest.Server {
	t.Helper()
	require.NotEmpty(t, states)
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), item+"(number:") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"unexpected GraphQL query"}]}`))
			return
		}
		index := int(calls.Add(1)) - 1
		if index >= len(states) {
			index = len(states) - 1
		}
		state := states[index]
		reasonJSON := "null"
		if state.Reason != "" {
			encoded, _ := json.Marshal(state.Reason)
			reasonJSON = string(encoded)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"repository":{"%s":{"comments":{"nodes":[{"databaseId":%d,"isMinimized":%t,"minimizedReason":%s}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`, item, commentID, state.Hidden, reasonJSON)
	}))
}

func TestFetchMRDetailUsesCurrentCommentVisibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(repo.PlatformHost, repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	commentID := int64(18201)
	commentBody := "hidden after the REST detail refresh"
	commentAuthor := "reviewer"
	mock := &mockClient{
		comments: []*gh.IssueComment{{
			ID: &commentID, Body: &commentBody, User: &gh.User{Login: &commentAuthor},
			CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now),
		}},
		getPullRequestFn: func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
			require.Equal(1, number)
			return buildOpenPR(1, now), nil
		},
	}
	gqlSrv := currentCommentVisibilityServer(
		t, "pullRequest", commentID, CommentVisibility{Hidden: true, Reason: "ABUSE"},
	)
	defer gqlSrv.Close()
	syncer := NewSyncer(
		map[string]Client{"github.com": mock}, d, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
		),
	})

	_, err = syncer.fetchMRDetail(ctx, repo, repoID, 1, false)
	require.NoError(err)

	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(mr)
	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, events[0].MetadataJSON)
}

func TestFetchIssueDetailUsesCurrentCommentVisibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity(repo.PlatformHost, repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	issueID := int64(18300)
	issueNumber := 8
	commentID := int64(18301)
	commentBody := "hidden after the REST detail refresh"
	commentAuthor := "reviewer"
	mock := &mockClient{
		comments: []*gh.IssueComment{{
			ID: &commentID, Body: &commentBody, User: &gh.User{Login: &commentAuthor},
			CreatedAt: makeTimestamp(now), UpdatedAt: makeTimestamp(now),
		}},
		getIssueFn: func(_ context.Context, _, _ string, number int) (*gh.Issue, error) {
			require.Equal(issueNumber, number)
			issue := buildOpenIssue(issueNumber, now)
			issue.ID = &issueID
			return issue, nil
		},
	}
	gqlSrv := currentCommentVisibilityServer(
		t, "issue", commentID, CommentVisibility{Hidden: true, Reason: "ABUSE"},
	)
	defer gqlSrv.Close()
	syncer := NewSyncer(
		map[string]Client{"github.com": mock}, d, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcherWithClient(
			githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
		),
	})

	_, err = syncer.fetchIssueDetail(ctx, repo, repoID, issueNumber)
	require.NoError(err)

	issue, err := d.GetIssue(ctx, "github", "github.com", repo.Owner, repo.Name, issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, events[0].MetadataJSON)
}

func TestSyncOpenMRFromBulkRemovesDeletedCommentsWhenCommentsAreComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	firstUpdatedAt := now.Add(3 * time.Minute)
	secondUpdatedAt := now.Add(4 * time.Minute)
	commentID := int64(9101)
	commentAuthor := "reviewer"
	commentBody := "bulk PR comment"
	commentURL := "https://github.com/owner/repo/pull/1#issuecomment-9101"
	commentTime := gh.Timestamp{Time: now.Add(2 * time.Minute)}

	commentTotal := 1
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR: buildOpenPR(1, firstUpdatedAt),
		Comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			HTMLURL:   &commentURL,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: &commentTime,
			UpdatedAt: &commentTime,
		}},
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		TimelineComplete: true,
		CIComplete:       true,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	require.Equal(1, mr.CommentCount)
	assert.Equal(firstUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal(commentURL, events[0].DirectURL)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:                    buildOpenPR(1, secondUpdatedAt),
		Comments:              []*gh.IssueComment{},
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: true,
		CommitsComplete:       true,
		TimelineComplete:      true,
		CIComplete:            true,
	}, false)
	require.NoError(err)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(0, mr.CommentCount)
	assert.Equal(secondUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events)
	_ = commentTotal
}

func TestSyncOpenMRFromBulkLeavesDetailStaleWhenReviewThreadsAreIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := database.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	line := 12
	client := &mockClient{}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:                    buildOpenPR(1, now),
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: true,
		ReviewThreads: []platform.MergeRequestReviewThread{{
			ProviderThreadID: "thread-1", ProviderCommentID: "comment-1",
			Body: "stored inline comment", AuthorLogin: "reviewer",
			Range: platform.DiffReviewLineRange{
				Path: "src/main.go", Side: "right", Line: line, NewLine: &line,
			},
			CreatedAt: now, UpdatedAt: now,
		}},
		CommitsComplete:  true,
		TimelineComplete: true,
		CIComplete:       true,
	}, false)
	require.NoError(err)

	mr, err := database.GetMergeRequest(ctx, "github", "github.com", repo.Owner, repo.Name, 1)
	require.NoError(err)
	require.NotNil(mr)
	require.NotNil(mr.DetailFetchedAt)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:                    buildOpenPR(1, now),
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: false,
		CommitsComplete:       true,
		TimelineComplete:      true,
		CIComplete:            true,
	}, false)
	require.NoError(err)

	mr, err = database.GetMergeRequest(ctx, "github", "github.com", repo.Owner, repo.Name, 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Nil(mr.DetailFetchedAt)
	threads, err := database.ListMRReviewThreads(ctx, mr.ID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("thread-1", threads[0].ProviderThreadID)
	assert.Zero(client.listIssueCommentsCalled.Load())
}

// TestSyncOpenMRFromBulkPersistsWorkflowApproval verifies the GraphQL
// bulk path persists the workflow approval snapshot on fully-synced
// PRs. Without this, the periodic GraphQL sync would mark a PR as
// detail-fetched while leaving workflow_approval_checked_at nil, so
// the DB-only GET would hide the Approve workflows button.
func TestSyncOpenMRFromBulkPersistsWorkflowApproval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	pr := buildOpenPR(1, now)
	headSHA := pr.GetHead().GetSHA()
	require.NotEmpty(headSHA)

	budgets := testBudget(2)
	mc := &mockClient{
		budget: budgets["github.com"],
		workflowRuns: []*gh.WorkflowRun{{
			ID:           new(int64(9001)),
			HeadSHA:      &headSHA,
			Event:        new("pull_request"),
			PullRequests: []*gh.PullRequest{{Number: new(1)}},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, budgets,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:                    pr,
		Comments:              []*gh.IssueComment{},
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: true,
		CommitsComplete:       true,
		TimelineComplete:      true,
		CIComplete:            true,
	}, false)
	require.NoError(err)

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(got)
	require.NotNil(got.WorkflowApprovalCheckedAt,
		"bulk allComplete must populate workflow_approval_checked_at")
	assert.Equal(headSHA, got.WorkflowApprovalHeadSHA)
	assert.True(got.WorkflowApprovalRequired)
	assert.Equal(1, got.WorkflowApprovalCount)
	assert.Equal(1, budgets["github.com"].Spent())
}

func TestSyncOpenMRFromBulkSkipsWorkflowApprovalWhenBudgetExhausted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2026, 4, 21, 12, 30, 0, 0, time.UTC)
	pr := buildOpenPR(1, now)
	headSHA := pr.GetHead().GetSHA()
	require.NotEmpty(headSHA)

	budgets := testBudget(1)
	budgets["github.com"].Spend(1)
	mc := &mockClient{
		budget: budgets["github.com"],
		workflowRuns: []*gh.WorkflowRun{{
			ID:           new(int64(9001)),
			HeadSHA:      &headSHA,
			Event:        new("pull_request"),
			PullRequests: []*gh.PullRequest{{Number: new(1)}},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, budgets,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               pr,
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		TimelineComplete: true,
		CIComplete:       true,
	}, false)
	require.NoError(err)

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(got)
	assert.Nil(got.WorkflowApprovalCheckedAt)
	assert.Equal(1, budgets["github.com"].Spent())
}

// TestSyncOpenMRFromBulkSkipsWorkflowApprovalWhenIncomplete verifies
// that a partial bulk sync (CI not complete) does not advance the
// workflow approval snapshot. Such PRs stay eligible for REST detail
// drain, which is the path that refreshes the snapshot.
func TestSyncOpenMRFromBulkSkipsWorkflowApprovalWhenIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2026, 4, 21, 13, 0, 0, 0, time.UTC)
	pr := buildOpenPR(1, now)
	headSHA := pr.GetHead().GetSHA()
	require.NotEmpty(headSHA)

	// workflowRuns is populated so that, if the refresh were
	// triggered, the snapshot would land with required=true. The
	// allComplete gate must prevent that.
	mc := &mockClient{
		workflowRuns: []*gh.WorkflowRun{{
			ID:           new(int64(9001)),
			HeadSHA:      &headSHA,
			Event:        new("pull_request"),
			PullRequests: []*gh.PullRequest{{Number: new(1)}},
		}},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               pr,
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		TimelineComplete: true,
		CIComplete:       false, // partial — skip workflow approval
	}, false)
	require.NoError(err)

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(got)
	assert.Nil(got.WorkflowApprovalCheckedAt,
		"partial bulk sync must not advance workflow_approval_checked_at")
	assert.False(got.WorkflowApprovalRequired)
	assert.Equal(0, got.WorkflowApprovalCount)
}

func TestSyncOpenMRFromBulkUpdatesCommentFieldsWhenOnlyCommentsAreComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 13, 0, 0, 0, time.UTC)
	firstUpdatedAt := now.Add(3 * time.Minute)
	secondUpdatedAt := now.Add(4 * time.Minute)
	commentID := int64(9301)
	commentAuthor := "reviewer"
	commentBody := "partial bulk PR comment"
	commentTime := gh.Timestamp{Time: now.Add(2 * time.Minute)}

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR: buildOpenPR(1, firstUpdatedAt),
		Comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: &commentTime,
			UpdatedAt: &commentTime,
		}},
		CommentsComplete: true,
		ReviewsComplete:  false,
		CommitsComplete:  false,
		CIComplete:       false,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(1, mr.CommentCount)
	assert.Equal(firstUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 1)

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               buildOpenPR(1, secondUpdatedAt),
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		ReviewsComplete:  false,
		CommitsComplete:  false,
		CIComplete:       false,
	}, false)
	require.NoError(err)

	mr, err = d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(0, mr.CommentCount)
	assert.Equal(secondUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err = d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestSyncOpenMRFromBulkStoresTimelineEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 14, 0, 0, 0, time.UTC)
	timelineAt := now.Add(3 * time.Minute)
	providerUpdatedAt := timelineAt.Add(2 * time.Minute)
	commitSHA := "abc123def456"
	commitMsg := "fix: preserve timeline commit order"
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR: buildOpenPR(1, providerUpdatedAt),
		Commits: []*gh.RepositoryCommit{{
			SHA: &commitSHA,
			Commit: &gh.Commit{
				Message: &commitMsg,
				Author: &gh.CommitAuthor{
					Name: new("dev"),
					Date: makeTimestamp(now.Add(-time.Minute)),
				},
			},
		}},
		TimelineEvents: []PullRequestTimelineEvent{{
			NodeID:          "BRC_1",
			EventType:       "base_ref_changed",
			Actor:           "alice",
			PreviousRefName: "main",
			CurrentRefName:  "release",
			CreatedAt:       timelineAt,
		}, {
			NodeID:               "CDE_1",
			EventType:            "comment_deleted",
			Actor:                "maintainer",
			DeletedCommentAuthor: "reviewer",
			CreatedAt:            timelineAt.Add(time.Minute),
		}},
		CommentsComplete:      true,
		ReviewsComplete:       true,
		ReviewThreadsComplete: true,
		CommitsComplete:       true,
		TimelineComplete:      true,
		CIComplete:            true,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	require.NotNil(mr.DetailFetchedAt)
	assert.Equal(providerUpdatedAt.UTC(), mr.LastActivityAt.UTC())

	events, err := d.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.Len(events, 3)
	assert.Equal("comment_deleted", events[0].EventType)
	assert.Equal("deleted a comment from reviewer", events[0].Summary)
	assert.Equal("base_ref_changed", events[1].EventType)
	assert.Equal("main -> release", events[1].Summary)
	var commit *db.MREvent
	for i := range events {
		if events[i].EventType == "commit" {
			commit = &events[i]
			break
		}
	}
	require.NotNil(commit)
	assert.Contains(commit.MetadataJSON, `"commit_order":1`)
}

// buildOpenPRWithSHA mirrors buildOpenPR but lets the caller set the
// head SHA so head-change scenarios can be exercised.
func buildOpenPRWithSHA(number int, updatedAt time.Time, headSHA string) *gh.PullRequest {
	pr := buildOpenPR(number, updatedAt)
	pr.Head.SHA = &headSHA
	return pr
}

func TestSyncOpenMRFromBulkClearsCIWhenHeadSHAChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	// Seed an existing MR with the old head SHA and stored CI status.
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      int64(1) * 1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           "open",
		PlatformHeadSHA: "oldhead",
		CIStatus:        "success",
		CIChecksJSON:    `[{"name":"tests","status":"completed","conclusion":"success"}]`,
		// Seed as true so the cleared assertion below catches the case
		// where syncOpenMRFromBulk carries the stale flag forward.
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	// New bulk fetch reports a new head SHA with CIComplete=false (the
	// CI page was truncated). Without the head-SHA guard, the upsert
	// would carry the old CI fields forward onto the new commit.
	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               buildOpenPRWithSHA(1, now.Add(time.Minute), "newhead"),
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		CIComplete:       false,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("newhead", mr.PlatformHeadSHA)
	assert.Empty(mr.CIStatus)
	assert.Empty(mr.CIChecksJSON)
	assert.False(mr.CIHadPending)
}

func TestSyncOpenMRFromBulkPreservesCIWhenHeadSHAUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	// Same head SHA as buildOpenPR uses by default.
	const sameSHA = "abc123def456"
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      int64(1) * 1000,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "test PR",
		Author:          "alice",
		State:           "open",
		PlatformHeadSHA: sameSHA,
		CIStatus:        "success",
		CIChecksJSON:    `[{"name":"tests","status":"completed","conclusion":"success"}]`,
		// Seed as true so the preserved assertion below distinguishes
		// the preserve path from a default-zero pending flag.
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	// CIComplete=false would normally skip the CI write. The existing
	// CI must be preserved because the head SHA is unchanged.
	err = syncer.syncOpenMRFromBulk(ctx, repo, repoID, &BulkPR{
		PR:               buildOpenPR(1, now.Add(time.Minute)),
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		ReviewsComplete:  true,
		CommitsComplete:  true,
		CIComplete:       false,
	}, false)
	require.NoError(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(sameSHA, mr.PlatformHeadSHA)
	assert.Equal("success", mr.CIStatus)
	assert.Contains(mr.CIChecksJSON, "tests")
	assert.True(mr.CIHadPending)
}

func TestSyncOpenIssueFromBulkRemovesDeletedCommentsWhenCommentsAreComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	firstUpdatedAt := gh.Timestamp{Time: now}
	secondUpdatedAt := gh.Timestamp{Time: now.Add(time.Minute)}
	issueID := int64(9201)
	issueNumber := 9
	issueTitle := "bulk issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/9"
	issueAuthor := "alice"
	commentID := int64(9202)
	commentAuthor := "reviewer"
	commentBody := "bulk issue comment"
	commentURL := "https://github.com/owner/repo/issues/9#issuecomment-9202"
	commentTime := gh.Timestamp{Time: now.Add(2 * time.Minute)}
	commentTotal := 1

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenIssueFromBulk(ctx, repo, repoID, &BulkIssue{
		Issue: &gh.Issue{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			Comments:  &commentTotal,
			User:      &gh.User{Login: &issueAuthor},
			CreatedAt: &firstUpdatedAt,
			UpdatedAt: &firstUpdatedAt,
		},
		Comments: []*gh.IssueComment{{
			ID:        &commentID,
			Body:      &commentBody,
			HTMLURL:   &commentURL,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: &commentTime,
			UpdatedAt: &commentTime,
		}},
		CommentsComplete: true,
		TimelineComplete: true,
	})
	require.NoError(err)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	require.Equal(1, issue.CommentCount)
	assert.Equal(commentTime.UTC(), issue.LastActivityAt.UTC())

	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal(commentURL, events[0].DirectURL)

	zeroComments := 0
	err = syncer.syncOpenIssueFromBulk(ctx, repo, repoID, &BulkIssue{
		Issue: &gh.Issue{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			Comments:  &zeroComments,
			User:      &gh.User{Login: &issueAuthor},
			CreatedAt: &firstUpdatedAt,
			UpdatedAt: &secondUpdatedAt,
		},
		Comments:         []*gh.IssueComment{},
		CommentsComplete: true,
		TimelineComplete: true,
	})
	require.NoError(err)

	issue, err = d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal(0, issue.CommentCount)
	assert.Equal(secondUpdatedAt.UTC(), issue.LastActivityAt.UTC())

	events, err = d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Empty(events)
}

func TestSyncOpenIssueFromBulkMergesPartialCommentVisibilityWithStoredState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	issueID := int64(9251)
	issueNumber := 9
	issueTitle := "partial comment visibility"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/9"
	issueAuthor := "alice"
	updatedAt := gh.Timestamp{Time: now}
	commentAuthor := "reviewer"
	firstCommentID := int64(9252)
	secondCommentID := int64(9253)
	firstCommentBody := "observed by GraphQL"
	secondCommentBody := "outside the partial GraphQL page"
	firstCommentURL := issueURL + "#issuecomment-9252"
	secondCommentURL := issueURL + "#issuecomment-9253"
	firstCommentTime := gh.Timestamp{Time: now.Add(time.Minute)}
	secondCommentTime := gh.Timestamp{Time: now.Add(2 * time.Minute)}
	commentTotal := 2
	comments := []*gh.IssueComment{
		{
			ID:        &firstCommentID,
			Body:      &firstCommentBody,
			HTMLURL:   &firstCommentURL,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: &firstCommentTime,
			UpdatedAt: &firstCommentTime,
		},
		{
			ID:        &secondCommentID,
			Body:      &secondCommentBody,
			HTMLURL:   &secondCommentURL,
			User:      &gh.User{Login: &commentAuthor},
			CreatedAt: &secondCommentTime,
			UpdatedAt: &secondCommentTime,
		},
	}
	issue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		Comments:  &commentTotal,
		User:      &gh.User{Login: &issueAuthor},
		CreatedAt: &updatedAt,
		UpdatedAt: &updatedAt,
	}

	mock := &mockClient{comments: comments}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenIssueFromBulk(ctx, repo, repoID, &BulkIssue{
		Issue:             issue,
		Comments:          comments,
		CommentVisibility: map[int64]CommentVisibility{firstCommentID: {Hidden: true}, secondCommentID: {Hidden: true}},
		CommentsComplete:  true,
		TimelineComplete:  true,
	})
	require.NoError(err)

	err = syncer.syncOpenIssueFromBulk(ctx, repo, repoID, &BulkIssue{
		Issue:             issue,
		Comments:          comments[:1],
		CommentVisibility: map[int64]CommentVisibility{firstCommentID: {}},
		CommentsComplete:  false,
		TimelineComplete:  true,
	})
	require.NoError(err)

	storedIssue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(storedIssue)
	events, err := d.ListIssueEvents(ctx, storedIssue.ID)
	require.NoError(err)
	require.Len(events, 2)

	metadataByCommentID := make(map[int64]string, len(events))
	for _, event := range events {
		require.NotNil(event.PlatformID)
		metadataByCommentID[*event.PlatformID] = event.MetadataJSON
	}
	assert.NotContains(metadataByCommentID[firstCommentID], `"provider_hidden":true`)
	assert.Contains(metadataByCommentID[secondCommentID], `"provider_hidden":true`)
}

func TestSyncOpenIssueFromBulkStoresTimelineEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	issueID := int64(9301)
	issueNumber := 9
	issueTitle := "bulk issue timeline"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/9"
	issueAuthor := "alice"
	updatedAt := gh.Timestamp{Time: now}

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	err = syncer.syncOpenIssueFromBulk(ctx, repo, repoID, &BulkIssue{
		Issue: &gh.Issue{
			ID:        &issueID,
			Number:    &issueNumber,
			Title:     &issueTitle,
			State:     &issueState,
			HTMLURL:   &issueURL,
			User:      &gh.User{Login: &issueAuthor},
			CreatedAt: &updatedAt,
			UpdatedAt: &updatedAt,
		},
		TimelineEvents: []PullRequestTimelineEvent{
			{
				NodeID:       "CRE_issue_1",
				EventType:    "cross_referenced",
				Actor:        "reviewer",
				CreatedAt:    now.Add(time.Minute),
				SourceType:   "PullRequest",
				SourceOwner:  "owner",
				SourceRepo:   "repo",
				SourceNumber: 22,
				SourceTitle:  "Fix issue",
				SourceURL:    "https://github.com/owner/repo/pull/22",
			},
			{
				NodeID:    "CE_issue_1",
				EventType: "closed",
				Actor:     "closer",
				CreatedAt: now.Add(2 * time.Minute),
			},
			{
				NodeID:    "RE_issue_1",
				EventType: "reopened",
				Actor:     "opener",
				CreatedAt: now.Add(3 * time.Minute),
			},
		},
		CommentsComplete: true,
		TimelineComplete: true,
	})
	require.NoError(err)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", issueNumber)
	require.NoError(err)
	require.NotNil(issue)
	assert.NotNil(issue.DetailFetchedAt)

	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 3)

	byType := make(map[string]db.IssueEvent, len(events))
	for _, event := range events {
		byType[event.EventType] = event
	}

	crossReferenced, ok := byType["cross_referenced"]
	require.True(ok)
	assert.Equal("reviewer", crossReferenced.Author)
	assert.Equal("Referenced from owner/repo#22", crossReferenced.Summary)
	assert.Contains(crossReferenced.MetadataJSON, `"source_title":"Fix issue"`)
	assert.Equal("timeline-CRE_issue_1", crossReferenced.DedupeKey)

	closed, ok := byType["closed"]
	require.True(ok)
	assert.Equal("closer", closed.Author)
	assert.Equal("closed this", closed.Summary)
	assert.Equal("timeline-CE_issue_1", closed.DedupeKey)

	reopened, ok := byType["reopened"]
	require.True(ok)
	assert.Equal("opener", reopened.Author)
	assert.Equal("reopened this", reopened.Summary)
	assert.Equal("timeline-RE_issue_1", reopened.DedupeKey)
}

func TestListFetchProgressLogger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		item       string
		source     string
		knownTotal bool
		newLogger  func(RepoRef, string) *listFetchProgressLogger
	}{
		{"REST issues without reported total", "issue", "rest", false, newIssueListFetchProgressLogger},
		{"GraphQL merge requests with reported total", "merge request", "graphql", true, newMergeRequestListFetchProgressLogger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			logs := captureDefaultLogs(t)
			progress := tc.newLogger(RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, tc.source)

			if tc.knownTotal {
				progress.setTotal(3)
			}
			progress.recordPage(1, true)
			progress.recordPage(1, true)
			progress.recordPage(1, false)
			progress.done()

			output := logs.String()
			attributes := " repo=owner/repo platform=github host=github.com source=" + tc.source
			knownTotal := ""
			if tc.knownTotal {
				knownTotal = " total=3"
			}
			assert.Contains(output, `msg="`+tc.item+` list fetch started"`+attributes+" fetched=1"+knownTotal)
			assert.Contains(output, `msg="`+tc.item+` list fetch progress"`+attributes+" fetched=2"+knownTotal)
			assert.Contains(output, `msg="`+tc.item+` list fetch completed"`+attributes+" fetched=3 total=3")
		})
	}
}

func TestSyncIssuesFromListLogsProgressForLargeIssueSets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	issues := make([]*gh.Issue, 0, 201)
	for number := 1; number <= 201; number++ {
		issues = append(issues, buildOpenIssue(number, now))
	}

	logs := captureDefaultLogs(t)

	client := &mockClient{}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	err = syncer.syncIssuesFromList(ctx, client, repo, repoID, issues, false)
	require.NoError(err)

	output := logs.String()
	assert.Contains(output, `msg="issue sync started"`)
	assert.Contains(output, "repo=owner/repo")
	assert.Contains(output, "platform=github")
	assert.Contains(output, "host=github.com")
	assert.Contains(output, "source=rest")
	assert.Contains(output, "total=201")
	assert.Contains(output, `msg="issue sync progress"`)
	assert.Contains(output, "processed=100")
	assert.Contains(output, "processed=200")
	assert.Contains(output, `msg="issue sync completed"`)
	assert.Contains(output, "processed=201")
}

func TestSyncRepoGraphQLIssues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	mock := &mockClient{}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	issueID := int64(10000)
	issueNumber := 10
	issueTitle := "Bug report"
	issueState := "open"
	issueBody := "Something broke"
	issueURL := "https://github.com/owner/repo/issues/10"
	issueAuthor := "alice"
	commentID := int64(501)
	commentBody := "I see this too"
	commentLogin := "bob"
	commentTime := gh.Timestamp{Time: now}
	// TotalCount (5) deliberately > len(nodes) (1). Proves the sync
	// uses GraphQL's TotalCount, not node length.
	issueCommentTotal := 5
	result := &RepoBulkResult{
		Issues: []BulkIssue{
			{
				Issue: &gh.Issue{
					ID:        &issueID,
					Number:    &issueNumber,
					Title:     &issueTitle,
					State:     &issueState,
					Body:      &issueBody,
					HTMLURL:   &issueURL,
					Comments:  &issueCommentTotal,
					User:      &gh.User{Login: &issueAuthor},
					CreatedAt: &commentTime,
					UpdatedAt: &commentTime,
				},
				Comments: []*gh.IssueComment{
					{
						ID:        &commentID,
						Body:      &commentBody,
						User:      &gh.User{Login: &commentLogin},
						CreatedAt: &commentTime,
						UpdatedAt: &commentTime,
					},
				},
				CommentsComplete: true,
				TimelineComplete: true,
			},
		},
	}

	err = syncer.doSyncRepoGraphQLIssues(ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, result,
	)
	require.NoError(err)

	// Verify issue in DB.
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 10)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Bug report", issue.Title)
	assert.Equal("alice", issue.Author)
	assert.Equal("open", issue.State)
	// Complete replacement derives the count from the unique rows persisted,
	// rather than trusting a contradictory aggregate total.
	assert.Equal(1, issue.CommentCount)

	// Verify comment event.
	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Len(events, 1)
	assert.Equal("I see this too", events[0].Body)

	// Comments were complete — ListIssueComments should NOT be called.
	assert.Equal(int32(0), mock.listIssueCommentsCalled.Load())

	// detail_fetched_at should be set for complete bulk issues.
	assert.NotNil(issue.DetailFetchedAt)
}

// displayNameTestRepo is the repository whose credential display-name lookups
// are routed through.
var displayNameTestRepo = RepoRef{
	Platform: platform.KindGitHub, PlatformHost: "github.com",
	Owner: "acme", Name: "widget",
}

func TestResolveDisplayName(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name          string
		login         string
		getUserFn     func(context.Context, string) (*gh.User, error)
		wantName      string
		wantOK        bool
		wantAPICalled bool
	}{
		{
			name:  "regular user with display name",
			login: "alice",
			getUserFn: func(_ context.Context, login string) (*gh.User, error) {
				name := "Alice Smith"
				return &gh.User{Login: &login, Name: &name}, nil
			},
			wantName:      "Alice Smith",
			wantOK:        true,
			wantAPICalled: true,
		},
		{
			name:  "regular user without display name",
			login: "bob",
			getUserFn: func(_ context.Context, login string) (*gh.User, error) {
				return &gh.User{Login: &login}, nil
			},
			wantName:      "",
			wantOK:        true,
			wantAPICalled: true,
		},
		{
			name:  "bot login skips API call",
			login: "renovate[bot]",
			getUserFn: func(_ context.Context, _ string) (*gh.User, error) {
				return nil, nil
			},
			wantName:      "renovate[bot]",
			wantOK:        true,
			wantAPICalled: false,
		},
		{
			name:  "API-returned bot uses login as display name",
			login: "ci-helper",
			getUserFn: func(_ context.Context, login string) (*gh.User, error) {
				botType := "Bot"
				return &gh.User{Login: &login, Type: &botType}, nil
			},
			wantName:      "ci-helper",
			wantOK:        true,
			wantAPICalled: true,
		},
		{
			name:  "user not found returns false",
			login: "ghost",
			getUserFn: func(_ context.Context, _ string) (*gh.User, error) {
				return nil, fmt.Errorf("GET https://api.github.com/users/ghost: 404 Not Found")
			},
			wantName:      "",
			wantOK:        false,
			wantAPICalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			apiCalled := false
			mc := &mockClient{getUserFn: func(ctx context.Context, login string) (*gh.User, error) {
				apiCalled = true
				return tt.getUserFn(ctx, login)
			}}
			syncer := NewSyncer(
				map[string]Client{"github.com": mc}, nil, nil, nil,
				time.Minute, nil, nil,
			)
			name, ok := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, tt.login)
			assert.Equal(tt.wantName, name)
			assert.Equal(tt.wantOK, ok)
			assert.Equal(tt.wantAPICalled, apiCalled, "GetUser call expectation")
		})
	}
}

func TestResolveDisplayName_CachesNegativeResult(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()

	callCount := 0
	mc := &mockClient{
		getUserFn: func(_ context.Context, _ string) (*gh.User, error) {
			callCount++
			return nil, fmt.Errorf("404 Not Found")
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, nil, nil, nil,
		time.Minute, nil, nil,
	)

	// First call: hits API, returns failure.
	name1, ok1 := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "renovate")
	assert.Empty(name1)
	assert.False(ok1)
	assert.Equal(1, callCount)

	// Second call: should use cache, no additional API call.
	name2, ok2 := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "renovate")
	assert.Empty(name2)
	assert.False(ok2)
	assert.Equal(1, callCount, "GetUser should not be called again for cached failure")
}

func TestResolveDisplayName_CachesSuccessfulEmptyName(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()

	callCount := 0
	mc := &mockClient{
		getUserFn: func(_ context.Context, login string) (*gh.User, error) {
			callCount++
			return &gh.User{Login: &login}, nil // no display name
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, nil, nil, nil,
		time.Minute, nil, nil,
	)

	// First call: hits API, succeeds with empty name.
	name1, ok1 := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "no-profile")
	assert.Empty(name1)
	assert.True(ok1, "successful lookup of empty name should return ok=true")
	assert.Equal(1, callCount)

	// Second call: cache hit must still return ok=true, not flip to false.
	name2, ok2 := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "no-profile")
	assert.Empty(name2)
	assert.True(ok2, "cached empty name must remain ok=true")
	assert.Equal(1, callCount, "GetUser should not be called again for cached success")
}

func TestSyncRepoGraphQLIssuesCommentsIncomplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	commentTime := gh.Timestamp{Time: now}

	commentID := int64(777)
	commentBody := "REST comment"
	commentLogin := "carol"

	mock := &mockClient{
		comments: []*gh.IssueComment{
			{
				ID:        &commentID,
				Body:      &commentBody,
				User:      &gh.User{Login: &commentLogin},
				CreatedAt: &commentTime,
				UpdatedAt: &commentTime,
			},
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	issueID := int64(20000)
	issueNumber := 20
	issueTitle := "Lots of comments"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/20"
	issueLogin := "dave"
	result := &RepoBulkResult{
		Issues: []BulkIssue{
			{
				Issue: &gh.Issue{
					ID:        &issueID,
					Number:    &issueNumber,
					Title:     &issueTitle,
					State:     &issueState,
					HTMLURL:   &issueURL,
					User:      &gh.User{Login: &issueLogin},
					CreatedAt: &commentTime,
					UpdatedAt: &commentTime,
				},
				CommentsComplete: false,
			},
		},
	}

	err = syncer.doSyncRepoGraphQLIssues(ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, result,
	)
	require.NoError(err)

	// REST fallback should have been called
	assert.Equal(int32(1), mock.listIssueCommentsCalled.Load())

	// Verify the REST comment landed
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 20)
	require.NoError(err)
	require.NotNil(issue)

	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.Len(events, 1)
	assert.Equal("REST comment", events[0].Body)
}

func TestSyncRepoGraphQLIssuesStopsAfterWrappedRawDisabledFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "owner", Name: "repo",
	}
	repoID, err := database.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	rawDisabledErr := fmt.Errorf("list issue comments: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Issues are disabled for this repo",
	})
	client := &mockClient{
		listIssueCommentsFn: func(context.Context, string, string, int) ([]*gh.IssueComment, error) {
			return nil, rawDisabledErr
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	result := &RepoBulkResult{Issues: []BulkIssue{
		{Issue: buildOpenIssue(1, now), CommentsComplete: false},
		{Issue: buildOpenIssue(2, now), CommentsComplete: false},
	}}

	err = syncer.doSyncRepoGraphQLIssues(ctx, repo, repoID, result)

	require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
	assert.Equal(int32(1), client.listIssueCommentsCalled.Load())
}

func TestSyncRepoGraphQLIssuesClosureDetection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)

	// Pre-seed an open issue that will not appear in GraphQL results
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     30000,
		Number:         30,
		URL:            "https://github.com/owner/repo/issues/30",
		Title:          "Will be closed",
		Author:         "eve",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	closedAt := gh.Timestamp{Time: now}
	closedState := "closed"
	closedIssueID := int64(30000)
	closedNumber := 30
	closedTitle := "Will be closed"

	mock := &mockClient{
		getIssueFn: func(_ context.Context, _, _ string, number int) (*gh.Issue, error) {
			if number == 30 {
				return &gh.Issue{
					ID:        &closedIssueID,
					Number:    &closedNumber,
					Title:     &closedTitle,
					State:     &closedState,
					CreatedAt: &closedAt,
					UpdatedAt: &closedAt,
					ClosedAt:  &closedAt,
				}, nil
			}
			return nil, fmt.Errorf("unexpected issue %d", number)
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// GraphQL returns no issues (issue #30 was closed)
	result := &RepoBulkResult{Issues: []BulkIssue{}}

	err = syncer.doSyncRepoGraphQLIssues(ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, result,
	)
	require.NoError(err)

	// Issue should now be closed
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 30)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("closed", issue.State)
	assert.NotNil(issue.ClosedAt)
}

func TestSyncRepoGraphQLIssuesPreservesExistingFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	fetchedAt := now.Add(-time.Hour)

	// Pre-seed issue with existing derived fields
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:          repoID,
		PlatformID:      40000,
		Number:          40,
		URL:             "https://github.com/owner/repo/issues/40",
		Title:           "Existing issue",
		Author:          "frank",
		State:           "open",
		CommentCount:    5,
		DetailFetchedAt: &fetchedAt,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	commentTime := gh.Timestamp{Time: now}
	mock := &mockClient{}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// GraphQL returns the same issue with no comments (incomplete)
	issueID := int64(40000)
	issueNumber := 40
	issueTitle := "Existing issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/40"
	issueLogin := "frank"
	result := &RepoBulkResult{
		Issues: []BulkIssue{
			{
				Issue: &gh.Issue{
					ID:        &issueID,
					Number:    &issueNumber,
					Title:     &issueTitle,
					State:     &issueState,
					HTMLURL:   &issueURL,
					User:      &gh.User{Login: &issueLogin},
					CreatedAt: &commentTime,
					UpdatedAt: &commentTime,
				},
				CommentsComplete: false,
			},
		},
	}

	err = syncer.doSyncRepoGraphQLIssues(ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, result,
	)
	require.NoError(err)

	// DetailFetchedAt is cleared before REST fallback, then re-set
	// after successful refreshIssueTimeline. CommentCount is updated
	// by the REST fallback (0 comments returned by the mock).
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 40)
	require.NoError(err)
	require.NotNil(issue)
	assert.NotNil(issue.DetailFetchedAt)
	assert.Equal(0, issue.CommentCount)
}

func TestSyncRepoGraphQLIssuesClearsDetailFetchedAtOnFailedFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	fetchedAt := now.Add(-time.Hour)

	// Pre-seed issue with non-nil DetailFetchedAt (previously fetched).
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:          repoID,
		PlatformID:      45000,
		Number:          45,
		URL:             "https://github.com/owner/repo/issues/45",
		Title:           "Previously fetched",
		Author:          "grace",
		State:           "open",
		CommentCount:    3,
		DetailFetchedAt: &fetchedAt,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	commentTime := gh.Timestamp{Time: now}
	mock := &mockClient{
		listIssueCommentsErr: fmt.Errorf("transient API failure"),
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	issueID := int64(45000)
	issueNumber := 45
	issueTitle := "Previously fetched"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/45"
	issueLogin := "grace"
	result := &RepoBulkResult{
		Issues: []BulkIssue{
			{
				Issue: &gh.Issue{
					ID:        &issueID,
					Number:    &issueNumber,
					Title:     &issueTitle,
					State:     &issueState,
					HTMLURL:   &issueURL,
					User:      &gh.User{Login: &issueLogin},
					CreatedAt: &commentTime,
					UpdatedAt: &commentTime,
				},
				CommentsComplete: false, // triggers REST fallback
			},
		},
	}

	// REST fallback will fail due to listIssueCommentsErr.
	err = syncer.doSyncRepoGraphQLIssues(ctx,
		RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"},
		repoID, result,
	)
	// Partial failure expected.
	require.Error(err)

	// DetailFetchedAt must be nil so the detail drain re-queues this issue.
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 45)
	require.NoError(err)
	require.NotNil(issue)
	assert.Nil(issue.DetailFetchedAt)
}

func TestSyncRepoGraphQLIssuesFallbackToREST(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	issueTime := makeTimestamp(now)
	issueID := int64(50000)
	issueNumber := 50
	issueTitle := "REST issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/50"
	issueLogin := "grace"

	ghIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		User:      &gh.User{Login: &issueLogin},
		CreatedAt: issueTime,
		UpdatedAt: issueTime,
	}

	mock := &mockClient{
		listOpenPRsErr: notModifiedErr(),
		openIssues:     []*gh.Issue{ghIssue},
		getIssueFn: func(_ context.Context, _, _ string, _ int) (*gh.Issue, error) {
			return ghIssue, nil
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, testBudget(1000),
	)

	// Configure a GraphQL fetcher that returns errors. The HTTP server
	// responds with a GraphQL error, so FetchRepoIssues fails and the
	// sync engine falls back to REST using the already-fetched issue list.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"errors":[{"message":"server error"}]}`))
	}))
	defer errSrv.Close()
	gqlClient := githubv4.NewEnterpriseClient(errSrv.URL, errSrv.Client())
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": {client: gqlClient},
	})

	syncer.RunOnce(ctx)

	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 50)
	require.NoError(t, err)
	require.NotNil(t, issue)
	assert.Equal("REST issue", issue.Title)
	assert.Equal("grace", issue.Author)
}

// TestSyncRepoGraphQLIssuesFullFlow exercises the full GraphQL issue
// sync path end-to-end: real GraphQLFetcher with a real HTTP backend
// returning canned JSON, through JSON parsing → gqlIssue adapter →
// NormalizeIssue → UpsertIssue. Validates that struct tags, adapter
// mapping, and the full data flow work together.
func TestSyncRepoGraphQLIssuesFullFlow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)

	// GraphQL server responds with canned issue data. The request
	// body distinguishes PR queries from issue queries; respond with
	// empty PRs and a single issue.
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if bytes.Contains(body, []byte("pullRequests")) {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
			return
		}
		resp := `{"data":{"repository":{"issues":{"nodes":[{
			"databaseId":70000,
			"number":70,
			"title":"Full flow issue",
			"state":"OPEN",
			"body":"End to end test",
			"url":"https://github.com/owner/repo/issues/70",
			"author":{"login":"heidi"},
			"createdAt":"` + now + `",
			"updatedAt":"` + now + `",
			"closedAt":null,
			"labels":{"nodes":[{"name":"bug","color":"d73a4a","description":"","isDefault":false}]},
			"comments":{"totalCount":1,"nodes":[{"databaseId":701,"author":{"login":"commenter"},"body":"Full flow comment","createdAt":"` + now + `","updatedAt":"` + now + `"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}
		}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`
		_, _ = w.Write([]byte(resp))
	}))
	defer gqlSrv.Close()

	// REST mock: returns the same issue in list (for ETag gate pass),
	// and also lists PRs as 304 to focus on issues.
	issueID := int64(70000)
	issueNumber := 70
	issueTitle := "Full flow issue"
	issueState := "open"
	issueURL := "https://github.com/owner/repo/issues/70"
	issueLogin := "heidi"
	issueTime := gh.Timestamp{Time: time.Now().UTC().Truncate(time.Second)}
	ghIssue := &gh.Issue{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &issueTitle,
		State:     &issueState,
		HTMLURL:   &issueURL,
		User:      &gh.User{Login: &issueLogin},
		CreatedAt: &issueTime,
		UpdatedAt: &issueTime,
	}
	mock := &mockClient{
		listOpenPRsErr: notModifiedErr(),
		openIssues:     []*gh.Issue{ghIssue},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, testBudget(1000),
	)

	gqlClient := githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client())
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": {client: gqlClient},
	})

	syncer.RunOnce(ctx)

	// Verify issue persisted with GraphQL data.
	issue, err := d.GetIssue(ctx, "github", "github.com", "owner", "repo", 70)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Full flow issue", issue.Title)
	assert.Equal("heidi", issue.Author)
	assert.Equal("open", issue.State)
	assert.Equal("End to end test", issue.Body)
	assert.Equal(1, issue.CommentCount)
	assert.NotNil(issue.DetailFetchedAt)

	// Labels persisted from GraphQL.
	require.Len(issue.Labels, 1)
	assert.Equal("bug", issue.Labels[0].Name)

	// Comment events persisted from GraphQL bulk (no REST fallback).
	events, err := d.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("Full flow comment", events[0].Body)
	assert.Equal("commenter", events[0].Author)

	// GraphQL path skipped REST ListIssueComments.
	assert.Equal(int32(0), mock.listIssueCommentsCalled.Load())
}

func TestPersistGitHubCommentsRollsBackRecoveryWrites(t *testing.T) {
	t.Run("pull request", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		database := openTestDB(t)
		repo := RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "owner", Name: "repo", RepoPath: "owner/repo",
		}
		repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
		require.NoError(err)
		now := time.Now().UTC().Truncate(time.Second)
		mr := &db.MergeRequest{RepoID: repoID, PlatformID: 101, Number: 1, URL: "https://github.com/owner/repo/pull/1", Title: "PR", Author: "alice", State: "open", CreatedAt: now, UpdatedAt: now, LastActivityAt: now, CommentCount: 1}
		mr.ID, err = database.UpsertMergeRequest(t.Context(), mr)
		require.NoError(err)
		mr, err = database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(mr)
		oldID := int64(11)
		require.NoError(database.UpsertMREvents(t.Context(), []db.MREvent{{MergeRequestID: mr.ID, PlatformID: &oldID, EventType: "issue_comment", CreatedAt: now, DedupeKey: "old"}}))
		_, err = database.WriteDB().ExecContext(t.Context(), `CREATE TRIGGER reject_github_pr_comment_count BEFORE UPDATE OF comment_count ON forge_merge_requests BEGIN SELECT RAISE(ABORT, 'reject count'); END`)
		require.NoError(err)

		newID, body, login := int64(12), "new", "bob"
		syncer := NewSyncerWithRegistry(nil, database, nil, []RepoRef{repo}, time.Minute, nil, nil)
		err = syncer.persistPRComments(t.Context(), repo, mr, []*gh.IssueComment{{ID: &newID, Body: &body, User: &gh.User{Login: &login}, CreatedAt: &gh.Timestamp{Time: now}}})
		require.Error(err)
		events, err := database.ListMREvents(t.Context(), mr.ID)
		require.NoError(err)
		require.Len(events, 1)
		assert.Equal("old", events[0].DedupeKey)
		stored, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(stored)
		assert.Equal(1, stored.CommentCount)
	})

	t.Run("issue", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		database := openTestDB(t)
		repo := RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "owner", Name: "repo", RepoPath: "owner/repo",
		}
		repoID, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
		require.NoError(err)
		now := time.Now().UTC().Truncate(time.Second)
		issue := &db.Issue{RepoID: repoID, PlatformID: 201, Number: 2, URL: "https://github.com/owner/repo/issues/2", Title: "Issue", Author: "alice", State: "open", CreatedAt: now, UpdatedAt: now, LastActivityAt: now, CommentCount: 1}
		issue.ID, err = database.UpsertIssue(t.Context(), issue)
		require.NoError(err)
		issue, err = database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 2)
		require.NoError(err)
		require.NotNil(issue)
		oldID := int64(21)
		require.NoError(database.UpsertIssueEvents(t.Context(), []db.IssueEvent{{IssueID: issue.ID, PlatformID: &oldID, EventType: "issue_comment", CreatedAt: now, DedupeKey: "old"}}))
		_, err = database.WriteDB().ExecContext(t.Context(), `CREATE TRIGGER reject_github_issue_comment_count BEFORE UPDATE OF comment_count ON forge_issues BEGIN SELECT RAISE(ABORT, 'reject count'); END`)
		require.NoError(err)

		newID, body, login := int64(22), "new", "bob"
		syncer := NewSyncerWithRegistry(nil, database, nil, []RepoRef{repo}, time.Minute, nil, nil)
		err = syncer.persistIssueComments(t.Context(), repo, issue, []*gh.IssueComment{{ID: &newID, Body: &body, User: &gh.User{Login: &login}, CreatedAt: &gh.Timestamp{Time: now}}})
		require.Error(err)
		events, err := database.ListIssueEvents(t.Context(), issue.ID)
		require.NoError(err)
		require.Len(events, 1)
		assert.Equal("old", events[0].DedupeKey)
		stored, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 2)
		require.NoError(err)
		require.NotNil(stored)
		assert.Equal(1, stored.CommentCount)
	})
}

func TestRefreshRepoPRCommentsUsesFullFetchForLargeThreads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	detailFetchedAt := now.Add(time.Minute)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      101,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "Large thread",
		Author:          "alice",
		State:           "open",
		CommentCount:    100,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	mock := &mockClient{
		listIssueCommentsFn: func(_ context.Context, _, _ string, number int) ([]*gh.IssueComment, error) {
			require.Equal(1, number)
			return []*gh.IssueComment{}, nil
		},
		listIssueCommentsIfChangedFn: func(_ context.Context, _, _ string, _ int) ([]*gh.IssueComment, error) {
			require.FailNow("conditional comment refresh should not be used for 100+ comment PRs")
			return nil, nil
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	syncer.refreshRepoPRComments(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"})

	assert.Equal(int32(1), mock.listIssueCommentsCalled.Load())
	assert.Equal(int32(0), mock.listIssueCommentsIfChangedCalls.Load())
}

func TestRefreshRepoIssueCommentsUsesFullFetchForLargeThreads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	detailFetchedAt := now.Add(time.Minute)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:          repoID,
		PlatformID:      201,
		Number:          2,
		URL:             "https://github.com/owner/repo/issues/2",
		Title:           "Large thread issue",
		Author:          "bob",
		State:           "open",
		CommentCount:    100,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	mock := &mockClient{
		listIssueCommentsFn: func(_ context.Context, _, _ string, number int) ([]*gh.IssueComment, error) {
			require.Equal(2, number)
			return []*gh.IssueComment{}, nil
		},
		listIssueCommentsIfChangedFn: func(_ context.Context, _, _ string, _ int) ([]*gh.IssueComment, error) {
			require.FailNow("conditional comment refresh should not be used for 100+ comment issues")
			return nil, nil
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mock},
		d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	syncer.refreshRepoIssueComments(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"})

	assert.Equal(int32(1), mock.listIssueCommentsCalled.Load())
	assert.Equal(int32(0), mock.listIssueCommentsIfChangedCalls.Load())
}

func TestDrainPendingCommentSyncsReadsQueuedItemsByProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	detailFetchedAt := now.Add(-time.Minute)

	codeRepo := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "code.example.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "repo-code-acme-widget",
	}
	githubRepo := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "widget",
		PlatformExternalID: "repo-github-acme-widget",
	}
	codeRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(codeRepo)))
	require.NoError(err)
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)

	codeMRID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          codeRepoID,
		PlatformID:      1001,
		Number:          7,
		URL:             "https://code.example.com/acme/widget/pull/7",
		Title:           "code host MR",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	githubMRID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          githubRepoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://github.com/acme/widget/pull/7",
		Title:           "github.com MR",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID:          codeRepoID,
		PlatformID:      1002,
		Number:          8,
		URL:             "https://code.example.com/acme/widget/issues/8",
		Title:           "code host issue",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	githubIssueID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:          githubRepoID,
		PlatformID:      7002,
		Number:          8,
		URL:             "https://github.com/acme/widget/issues/8",
		Title:           "github.com issue",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	commentBody := "code host refreshed"
	commentID := int64(501)
	mock := &mockClient{
		listIssueCommentsIfChangedFn: func(_ context.Context, _, _ string, number int) ([]*gh.IssueComment, error) {
			require.Contains([]int{7, 8}, number)
			return []*gh.IssueComment{{
				ID:        &commentID,
				Body:      &commentBody,
				User:      &gh.User{Login: new("reviewer")},
				CreatedAt: makeTimestamp(now),
				UpdatedAt: makeTimestamp(now),
			}}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"code.example.com": mock},
		d, nil,
		[]RepoRef{codeRepo, githubRepo},
		time.Minute, nil, nil,
	)
	syncer.queuePRCommentSync(codeRepo, codeRepoID, 7)
	syncer.queueIssueCommentSync(codeRepo, codeRepoID, 8)

	syncer.drainPendingCommentSyncs(ctx, map[string]bool{"code.example.com": true})

	codeMREvents, err := d.ListMREvents(ctx, codeMRID)
	require.NoError(err)
	require.Len(codeMREvents, 1)
	assert.Equal(commentBody, codeMREvents[0].Body)
	githubMREvents, err := d.ListMREvents(ctx, githubMRID)
	require.NoError(err)
	assert.Empty(githubMREvents)

	codeIssue, err := d.GetIssueByRepoIDAndNumber(ctx, codeRepoID, 8)
	require.NoError(err)
	require.NotNil(codeIssue)
	codeIssueEvents, err := d.ListIssueEvents(ctx, codeIssue.ID)
	require.NoError(err)
	require.Len(codeIssueEvents, 1)
	assert.Equal(commentBody, codeIssueEvents[0].Body)
	githubIssueEvents, err := d.ListIssueEvents(ctx, githubIssueID)
	require.NoError(err)
	assert.Empty(githubIssueEvents)
}

func TestRefreshRepoCommentsFiltersByHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	detailFetchedAt := now.Add(-time.Minute)

	codeRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}
	githubRepo := RepoRef{
		Platform:     platform.KindGitHub,
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
	}
	codeRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(codeRepo)))
	require.NoError(err)
	githubRepoID, err := d.UpsertRepo(ctx, verifiedDBRepoIdentity(platformRepoRef(githubRepo)))
	require.NoError(err)

	codeMRID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          codeRepoID,
		PlatformID:      1001,
		Number:          7,
		URL:             "https://code.example.com/acme/widget/pull/7",
		Title:           "code host MR",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	githubMRID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          githubRepoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://github.com/acme/widget/pull/7",
		Title:           "github.com MR",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	codeIssueID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:          codeRepoID,
		PlatformID:      1002,
		Number:          8,
		URL:             "https://code.example.com/acme/widget/issues/8",
		Title:           "code host issue",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	githubIssueID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:          githubRepoID,
		PlatformID:      7002,
		Number:          8,
		URL:             "https://github.com/acme/widget/issues/8",
		Title:           "github.com issue",
		Author:          "ada",
		State:           "open",
		CommentCount:    1,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)

	commentBody := "code host refreshed"
	commentID := int64(501)
	mock := &mockClient{
		listIssueCommentsIfChangedFn: func(_ context.Context, _, _ string, number int) ([]*gh.IssueComment, error) {
			require.Contains([]int{7, 8}, number)
			return []*gh.IssueComment{{
				ID:        &commentID,
				Body:      &commentBody,
				User:      &gh.User{Login: new("reviewer")},
				CreatedAt: makeTimestamp(now),
				UpdatedAt: makeTimestamp(now),
			}}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"code.example.com": mock},
		d, nil,
		[]RepoRef{codeRepo, githubRepo},
		time.Minute, nil, nil,
	)

	syncer.refreshRepoPRComments(ctx, codeRepo)
	syncer.refreshRepoIssueComments(ctx, codeRepo)

	codeMREvents, err := d.ListMREvents(ctx, codeMRID)
	require.NoError(err)
	require.Len(codeMREvents, 1)
	assert.Equal(commentBody, codeMREvents[0].Body)
	githubMREvents, err := d.ListMREvents(ctx, githubMRID)
	require.NoError(err)
	assert.Empty(githubMREvents)

	codeIssueEvents, err := d.ListIssueEvents(ctx, codeIssueID)
	require.NoError(err)
	require.Len(codeIssueEvents, 1)
	assert.Equal(commentBody, codeIssueEvents[0].Body)
	githubIssueEvents, err := d.ListIssueEvents(ctx, githubIssueID)
	require.NoError(err)
	assert.Empty(githubIssueEvents)
}

func TestDeferredCommentRefreshYieldsBudgetToDetailDrain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	budget := testBudget(23)
	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-owner-repo", Owner: "owner", Name: "repo",
	})
	require.NoError(err)
	repo := RepoRef{
		Owner: "owner", Name: "repo", PlatformHost: "github.com",
		PlatformExternalID: "repo-owner-repo",
	}

	pr1UpdatedAt := now.Add(-10 * time.Minute)
	detailFetchedAt := now.Add(-5 * time.Minute)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          1,
		URL:             "https://github.com/owner/repo/pull/1",
		Title:           "Large unchanged thread",
		Author:          "alice",
		State:           "open",
		CommentCount:    100,
		HeadBranch:      "feature/large-thread",
		BaseBranch:      "main",
		PlatformHeadSHA: "1111111",
		PlatformBaseSHA: "aaaaaaa",
		CreatedAt:       pr1UpdatedAt,
		UpdatedAt:       pr1UpdatedAt,
		LastActivityAt:  pr1UpdatedAt,
		DetailFetchedAt: &detailFetchedAt,
	})
	require.NoError(err)
	pr2ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1002,
		Number:          2,
		URL:             "https://github.com/owner/repo/pull/2",
		Title:           "Needs detail drain",
		Author:          "alice",
		State:           "open",
		CommentCount:    0,
		HeadBranch:      "feature/detail-drain",
		BaseBranch:      "main",
		PlatformHeadSHA: "2222222",
		PlatformBaseSHA: "aaaaaaa",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	require.NoError(d.EnsureKanbanState(ctx, pr2ID))

	var commentCalls []int
	mc := &mockClient{
		budget: budget["github.com"],
		getPullRequestFn: func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
			require.Equal(2, number)
			return &gh.PullRequest{
				ID:        new(int64(1002)),
				Number:    new(2),
				Title:     new("Needs detail drain"),
				State:     new("open"),
				HTMLURL:   new("https://github.com/owner/repo/pull/2"),
				User:      &gh.User{Login: new("alice")},
				CreatedAt: &gh.Timestamp{Time: now},
				UpdatedAt: &gh.Timestamp{Time: now},
				Head:      &gh.PullRequestBranch{Ref: new("feature/detail-drain"), SHA: new("2222222")},
				Base:      &gh.PullRequestBranch{Ref: new("main"), SHA: new("aaaaaaa")},
			}, nil
		},
		listIssueCommentsFn: func(_ context.Context, _, _ string, number int) ([]*gh.IssueComment, error) {
			commentCalls = append(commentCalls, number)
			return []*gh.IssueComment{}, nil
		},
		reviews:  []*gh.PullRequestReview{},
		commits:  []*gh.RepositoryCommit{},
		ciStatus: &gh.CombinedStatus{State: new("success")},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": mc},
		d, nil,
		[]RepoRef{repo},
		time.Minute, nil, budget,
	)

	syncer.queuePRCommentSync(repo, repoID, 1)
	budget["github.com"].Spend(3)
	syncer.drainDetailQueue(ctx, map[string]bool{"github.com": true}, syncer.TrackedRepos())
	syncer.drainPendingCommentSyncs(ctx, map[string]bool{"github.com": true})

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 2)
	require.NoError(err)
	require.NotNil(pr)
	assert.NotNil(pr.DetailFetchedAt,
		"detail drain should win before unchanged large-thread refresh")
	assert.Equal([]int{2, 1}, commentCalls,
		"detail drain should spend first, before the deferred large-thread refresh")
}

func TestSyncerGQLRateTrackers(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	rt := NewRateTracker(d, "github.com", "host", "rest")
	gqlRT := NewRateTracker(d, "github.com", "host", "graphql")

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*RateTracker{"github.com": rt},
		nil,
	)

	fetcher := NewGraphQLFetcher(testTokenSource("token"), "github.com", gqlRT, nil)
	syncer.SetFetchers(map[string]*GraphQLFetcher{"github.com": fetcher})

	gqlTrackers := syncer.GQLRateTrackers()
	assert.Len(gqlTrackers, 1)
	assert.Same(gqlRT, gqlTrackers["github.com"])
}

func TestRunOnceRefreshesGitHubRateLimitSnapshotOutsideSyncBudget(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	restReset := now.Add(time.Hour)
	gqlReset := now.Add(90 * time.Minute)
	restRT := NewRateTracker(d, "github.com", "host", "rest")
	gqlRT := NewRateTracker(d, "github.com", "host", "graphql")
	budget := NewSyncBudget(100)
	client := &rateLimitSnapshotMockClient{
		mockClient: &mockClient{},
		snapshot: &RateLimitSnapshot{
			Core: &Rate{
				Limit:     5000,
				Remaining: 4991,
				Reset:     restReset,
			},
			GraphQL: &Rate{
				Limit:     5000,
				Remaining: 4988,
				Reset:     gqlReset,
			},
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil,
		nil,
		time.Minute,
		map[string]*RateTracker{"github.com": restRT},
		map[string]*SyncBudget{"github.com": budget},
	)
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com": NewGraphQLFetcher(testTokenSource("token"), "github.com", gqlRT, nil),
	})

	syncer.RunOnce(t.Context())

	assert.Equal(int32(1), client.snapshotCalls.Load())
	assert.Equal(int32(0), client.syncBudgetContexts.Load())
	assert.Equal(0, budget.Spent())
	assert.Equal(0, restRT.RequestsThisHour())
	assert.Equal(4991, restRT.Remaining())
	assert.Equal(5000, restRT.RateLimit())
	if assert.NotNil(restRT.ResetAt()) {
		assert.Equal(restReset, *restRT.ResetAt())
	}
	assert.Equal(0, gqlRT.RequestsThisHour())
	assert.Equal(4988, gqlRT.Remaining())
	assert.Equal(5000, gqlRT.RateLimit())
	if assert.NotNil(gqlRT.ResetAt()) {
		assert.Equal(gqlReset, *gqlRT.ResetAt())
	}
}

// failingSnapshotClient always fails the snapshot call, standing in for a
// credential whose /rate_limit request is rejected or times out.
type failingSnapshotClient struct {
	*mockClient
	calls atomic.Int32
}

func (m *failingSnapshotClient) GetRateLimitSnapshot(
	_ context.Context,
) (*RateLimitSnapshot, error) {
	m.calls.Add(1)
	return nil, errors.New("rate limit snapshot unavailable")
}

// Every route in a credential bucket authenticates as the same principal, so a
// failing snapshot must cost one request per window, not one per repository
// owner sharing the installation and not a fresh burst on the next pass.
func TestRefreshRateLimitSnapshotsSpendsOneRequestPerCredentialWhenRefreshFails(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)
	database := openTestDB(t)
	registry := NewQuotaRegistry()
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:42"}
	appREST := NewRateTracker(database, "github.com", "installation:42", "rest")
	// Startup builds a separate client per route, so the routes must not share
	// a client pointer here either; only the credential behind them is shared.
	clients := make([]*failingSnapshotClient, 0, 5)
	routes := make([]*Route, 0, 5)
	for _, owner := range []string{"acme", "other", "third", "fourth", "fifth"} {
		client := &failingSnapshotClient{mockClient: &mockClient{}}
		clients = append(clients, client)
		routes = append(routes, &Route{
			Key:           RouteKey{Host: "github.com", Owner: owner},
			CredentialKey: "github\x00github.com\x00app:1",
			Client:        client,
			ReadIdentity:  appIdentity, WriteIdentity: appIdentity,
		})
	}
	router, err := NewHostRouter("github.com", routes...)
	require.NoError(err)
	// A pre-observed pool proves a failed refresh leaves the last known facts
	// alone instead of blanking the credential's quota.
	reset := time.Now().UTC().Add(time.Hour)
	registry.UpdateSnapshot(appIdentity, QuotaResourceREST, Rate{
		Limit: 15000, Remaining: 14900, Reset: reset,
	})
	syncer := &Syncer{
		clients: registryFromGitHubClients(
			map[string]Client{"github.com": clients[0]},
		),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "installation:42"): appREST,
		},
		quotaRegistry:            registry,
		rateLimitSnapshotRefresh: make(map[string]time.Time),
		now:                      time.Now,
	}

	syncer.RefreshRateLimitSnapshots(t.Context())
	syncer.RefreshRateLimitSnapshots(t.Context())

	var total int32
	for _, client := range clients {
		total += client.calls.Load()
	}
	assert.Equal(int32(1), total,
		"a failed snapshot must not retry once per route, nor again next pass")
	pool, ok := registry.Get(appIdentity, QuotaResourceREST)
	require.True(ok, "the last known pool must survive a failed refresh")
	assert.Equal(14900, pool.Remaining)
}

func TestRefreshRateLimitSnapshotsReconcilesEachCredentialEveryThreeMinutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	database := openTestDB(t)
	registry := NewQuotaRegistry()
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:42"}
	userIdentity := IdentityKey{Host: "github.com", Principal: "user:7"}
	appClient := &credentialRateLimitSnapshotMockClient{
		mockClient: &mockClient{},
		appSnapshot: &RateLimitSnapshot{
			Core:    &Rate{Limit: 15000, Remaining: 14900, Reset: now.Add(time.Hour)},
			GraphQL: &Rate{Limit: 10000, Remaining: 9900, Reset: now.Add(time.Hour)},
		},
	}
	userClient := &credentialRateLimitSnapshotMockClient{
		mockClient: &mockClient{},
		appSnapshot: &RateLimitSnapshot{
			Core:    &Rate{Limit: 5000, Remaining: 4900, Reset: now.Add(time.Hour)},
			GraphQL: &Rate{Limit: 5000, Remaining: 4800, Reset: now.Add(time.Hour)},
		},
	}
	appREST := NewRateTracker(database, "github.com", "installation:42", "rest")
	userREST := NewRateTracker(database, "github.com", "user:7", "rest")
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: appClient,
			ReadIdentity: appIdentity, WriteIdentity: appIdentity,
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: userClient,
			ReadIdentity: userIdentity, WriteIdentity: userIdentity,
		},
	)
	require.NoError(err)
	syncer := &Syncer{
		clients: registryFromGitHubClients(map[string]Client{"github.com": appClient}),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "installation:42"): appREST,
			RateBucketKey("github", "github.com", "user:7"):          userREST,
		},
		quotaRegistry:            registry,
		rateLimitSnapshotRefresh: make(map[string]time.Time),
		now:                      time.Now,
	}

	syncer.RefreshRateLimitSnapshots(t.Context())
	// The second pass falls inside the three-minute reconcile window, so it
	// must not spend another snapshot call on either credential.
	syncer.RefreshRateLimitSnapshots(t.Context())

	assert.Equal(int32(1), appClient.appCalls.Load())
	assert.Equal(int32(1), userClient.appCalls.Load())
	appRESTPool, ok := registry.Get(appIdentity, QuotaResourceREST)
	require.True(ok)
	appGraphQL, ok := registry.Get(appIdentity, QuotaResourceGraphQL)
	require.True(ok)
	userRESTPool, ok := registry.Get(userIdentity, QuotaResourceREST)
	require.True(ok)
	userGraphQL, ok := registry.Get(userIdentity, QuotaResourceGraphQL)
	require.True(ok)
	assert.Equal(14900, appRESTPool.Remaining)
	assert.Equal(9900, appGraphQL.Remaining)
	assert.Equal(4900, userRESTPool.Remaining)
	assert.Equal(4800, userGraphQL.Remaining)
}

func TestRunOnceSnapshotWindowResetResetsSyncBudget(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	oldReset := now.Add(time.Hour)
	newReset := now.Add(2 * time.Hour)
	restRT := NewRateTracker(d, "github.com", "host", "rest")
	restRT.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: 4999,
		Reset:     oldReset,
	})
	for range 50 {
		restRT.RecordRequest()
	}
	restRT.SetResetAtForTesting(now.Add(-time.Minute))
	budget := NewSyncBudget(100)
	budget.Spend(100)
	client := &rateLimitSnapshotMockClient{
		mockClient: &mockClient{},
		snapshot: &RateLimitSnapshot{
			Core: &Rate{
				Limit:     5000,
				Remaining: 4990,
				Reset:     newReset,
			},
		},
	}

	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil,
		nil,
		time.Minute,
		map[string]*RateTracker{"github.com": restRT},
		map[string]*SyncBudget{"github.com": budget},
	)

	syncer.RunOnce(t.Context())

	assert.Equal(int32(1), client.snapshotCalls.Load())
	assert.Equal(0, restRT.RequestsThisHour())
	assert.Equal(0, budget.Spent())
	assert.Equal(4990, restRT.Remaining())
	if assert.NotNil(restRT.ResetAt()) {
		assert.Equal(newReset, *restRT.ResetAt())
	}
}

func TestRunOnceRecoveredRateLimitSnapshotClearsStaleThrottleGate(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	rt := NewRateTracker(d, "github.com", "host", "rest")
	rt.UpdateFromRate(Rate{
		Limit:     5000,
		Remaining: 400,
		Reset:     resetAt,
	})
	client := &rateLimitSnapshotMockClient{
		mockClient: &mockClient{},
		snapshot: &RateLimitSnapshot{
			Core: &Rate{
				Limit:     5000,
				Remaining: 4900,
				Reset:     resetAt,
			},
		},
	}
	interval := time.Minute

	mock := &mockClient{}
	client.mockClient = mock
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		interval,
		map[string]*RateTracker{"github.com": rt},
		nil,
	)
	syncer.nextSyncAfter["github.com"] = time.Now().UTC().Add(8 * interval)

	beforeRun := time.Now().UTC()
	syncer.RunOnce(t.Context())

	assert.True(mock.listOpenPRsCalled.Load())
	nextSyncAfter, ok := syncer.nextSyncAfter["github.com"]
	if assert.True(ok) {
		assert.Less(nextSyncAfter.Sub(beforeRun), 2*interval)
	}
	assert.Equal(4900, rt.Remaining())
}

func TestSyncerGQLRateTrackersSkipsNil(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil, nil,
	)

	// Nil fetcher entry and a fetcher with no tracker both skipped.
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"github.com":           nil,
		"ghe.corp.example.com": NewGraphQLFetcher(testTokenSource("tok"), "ghe.corp.example.com", nil, nil),
	})

	assert.Empty(syncer.GQLRateTrackers())
}

func TestSyncerGQLRateTrackersMixed(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	validRT := NewRateTracker(d, "github.com", "host", "graphql")

	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil, nil,
	)

	// Mix of nil fetcher, fetcher-without-tracker, and valid fetcher.
	syncer.SetFetchers(map[string]*GraphQLFetcher{
		"nil.example.com":        nil,
		"no-tracker.example.com": NewGraphQLFetcher(testTokenSource("tok"), "no-tracker.example.com", nil, nil),
		"github.com":             NewGraphQLFetcher(testTokenSource("tok"), "github.com", validRT, nil),
	})

	got := syncer.GQLRateTrackers()
	assert.Len(got, 1)
	assert.Same(validRT, got["github.com"])
}

// TestDisplayNameCacheSurvivesRunOnce verifies the key
// behavioral change: the cache persists across RunOnce
// invocations instead of being reset. With the old per-run
// map, the second RunOnce would re-fetch every author. With
// the TTL cache, the second RunOnce sees a fresh cache hit
// and makes zero /users calls.
func TestDisplayNameCacheSurvivesRunOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	prNumber := 1
	prTitle := "test"
	prState := "open"
	prURL := "https://github.com/owner/repo/pull/1"
	prBody := ""
	prAuthor := "alice"
	prDisplayName := "Alice Smith"

	getUserCalls := 0
	mc := &mockClient{
		openPRs: []*gh.PullRequest{buildOpenPR(prNumber, now)},
		getUserFn: func(_ context.Context, login string) (*gh.User, error) {
			getUserCalls++
			return &gh.User{Login: &login, Name: &prDisplayName}, nil
		},
	}
	// Patch the open PR to have the author we care about.
	mc.openPRs[0].User = &gh.User{Login: &prAuthor}
	mc.openPRs[0].Title = &prTitle
	mc.openPRs[0].State = &prState
	mc.openPRs[0].HTMLURL = &prURL
	mc.openPRs[0].Body = &prBody

	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	// First RunOnce: resolves display name for "alice".
	syncer.RunOnce(ctx)
	firstRunCalls := getUserCalls
	assert.Positive(firstRunCalls,
		"first RunOnce should have fetched the display name")

	// Verify the display name landed in SQLite.
	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", prNumber)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("Alice Smith", mr.AuthorDisplayName,
		"AuthorDisplayName must be persisted to SQLite after first sync")

	// Second RunOnce: cache hit, no new GetUser calls.
	syncer.RunOnce(ctx)
	assert.Equal(firstRunCalls, getUserCalls,
		"second RunOnce must not re-fetch cached display names")

	// DB still has the name after the cache-hit sync pass.
	mr2, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", prNumber)
	require.NoError(err)
	require.NotNil(mr2)
	assert.Equal("Alice Smith", mr2.AuthorDisplayName,
		"AuthorDisplayName must survive a cache-hit sync pass")
}

// TestResolveDisplayName_StaleWhileErrorBacksOff verifies the
// behavior when a successful cache entry has expired and the
// refresh call keeps failing:
//
//  1. Stale name is returned instead of "" (stale-while-error).
//  2. Follow-up calls within failureTTL do NOT hit the API — the
//     expiry is rewritten to failureTTL so retries back off.
//  3. After failureTTL elapses, one retry fires again.
//
// Without the backoff step 2, every subsequent sync would hit
// /users while the outage persists, defeating the cache.
func TestResolveDisplayName_StaleWhileErrorBacksOff(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()

	callCount := 0
	shouldFail := false
	mc := &mockClient{
		getUserFn: func(_ context.Context, login string) (*gh.User, error) {
			callCount++
			if shouldFail {
				return nil, fmt.Errorf("upstream outage")
			}
			name := "Alice Smith"
			return &gh.User{Login: &login, Name: &name}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, nil, nil, nil,
		time.Minute, nil, nil,
	)

	// Inject a fake clock into the cache so we can expire
	// entries without waiting 24 hours.
	fakeNow := time.Unix(1_700_000_000, 0)
	syncer.displayNames.now = func() time.Time { return fakeNow }

	// Warm the cache with a successful lookup.
	name, ok := syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "alice")
	assert.Equal("Alice Smith", name)
	assert.True(ok)
	assert.Equal(1, callCount)

	// Flip upstream to failing and expire the successful entry.
	shouldFail = true
	fakeNow = fakeNow.Add(displayNameSuccessTTL + time.Second)

	// First refresh: API hit fails, stale name is returned.
	name, ok = syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "alice")
	assert.Equal("Alice Smith", name,
		"stale name must be returned on refresh failure")
	assert.True(ok)
	assert.Equal(2, callCount, "refresh should hit the API once")

	// Second refresh inside failureTTL: no API call, still
	// serves stale name.
	fakeNow = fakeNow.Add(displayNameFailureTTL / 2)
	name, ok = syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "alice")
	assert.Equal("Alice Smith", name)
	assert.True(ok)
	assert.Equal(2, callCount,
		"retries within failureTTL must reuse the cached stale entry",
	)

	// Past failureTTL: one more API attempt is allowed.
	fakeNow = fakeNow.Add(displayNameFailureTTL + time.Second)
	name, ok = syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "alice")
	assert.Equal("Alice Smith", name)
	assert.True(ok)
	assert.Equal(3, callCount,
		"a retry should fire once failureTTL has elapsed",
	)

	// Recovered upstream: next call refreshes successfully.
	shouldFail = false
	fakeNow = fakeNow.Add(displayNameFailureTTL + time.Second)
	name, ok = syncer.resolveDisplayName(ctx, mc, displayNameTestRepo, "alice")
	assert.Equal("Alice Smith", name)
	assert.True(ok)
	assert.Equal(4, callCount)
}

func TestGitHubProviderApproveSubmitsReviewForReviewedHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockClient{}
	provider := gitHubClientProvider{client: mock, host: "github.com"}

	event, err := provider.ApproveMergeRequest(
		t.Context(), platform.RepoRef{Owner: "acme", Name: "widget"}, 7,
		"ship it", "reviewed-head",
	)
	require.NoError(err)
	assert.Equal("review", event.EventType)
	assert.Equal("APPROVE", mock.createdReviewEvent)
	assert.Equal("ship it", mock.createdReviewBody)
	assert.Equal("reviewed-head", mock.createdReviewCommitID)
}

func TestIsGitHubHeadModified(t *testing.T) {
	assert := assert.New(t)
	mismatch := func(status int, message string) error {
		return &gh.ErrorResponse{
			Response: &http.Response{StatusCode: status},
			Message:  message,
		}
	}
	assert.True(isGitHubHeadModified(mismatch(405, "Head branch was modified. Review and try the merge again.")))
	assert.True(isGitHubHeadModified(mismatch(409, "Head branch was modified.")))
	assert.False(isGitHubHeadModified(mismatch(405, "Pull Request is not mergeable")))
	assert.False(isGitHubHeadModified(mismatch(422, "Head branch was modified.")))
	assert.False(isGitHubHeadModified(errOther))
}

var errOther = fmt.Errorf("transport down")

// A rate limit belongs to the credential that hit it. With two owners on one
// host routed to different PATs, exhausting one must not stall the other: the
// exhausted owner's queued acks defer, while the healthy owner's ack still
// propagates in the same pass.
func TestProcessQueuedNotificationReadsDefersOnlyRateLimitedIdentity(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	limitedRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	healthyRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "other", "thing"),
	)
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	limitedNumber := 7
	healthyNumber := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "limited-thread", RepoID: &limitedRepo,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest",
			SubjectTitle: "Exhausted owner", ItemNumber: &limitedNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "healthy-thread", RepoID: &healthyRepo,
			RepoOwner: "other", RepoName: "thing", SubjectType: "PullRequest",
			SubjectTitle: "Healthy owner", ItemNumber: &healthyNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	require.Len(items, 2)
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	_, err = d.QueueNotificationIDsRead(t.Context(), ids, now.Add(time.Minute))
	require.NoError(err)

	resetAt := time.Now().UTC().Add(time.Hour).Round(0)
	var marked []string
	mc := &mockClient{markNotificationThreadReadFn: func(
		_ context.Context, threadID string,
	) error {
		marked = append(marked, threadID)
		if threadID != "limited-thread" {
			return nil
		}
		return &gh.RateLimitError{
			Rate: gh.Rate{Reset: gh.Timestamp{Time: resetAt}},
			Response: &http.Response{
				StatusCode: http.StatusForbidden,
				Request: httptest.NewRequest(
					http.MethodPatch,
					"https://api.github.com/notifications/threads/"+threadID, nil,
				),
			},
			Message: "API rate limit exceeded",
		}
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:1"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:1"},
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:2"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:2"},
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	err = syncer.ProcessQueuedNotificationReads(
		t.Context(), platform.KindGitHub, "github.com", 10,
	)
	require.Error(err, "the rate limit is still reported for the host")

	items, err = d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	byThread := map[string]db.Notification{}
	for _, item := range items {
		byThread[item.PlatformNotificationID] = item
	}
	check.ElementsMatch([]string{"limited-thread", "healthy-thread"}, marked,
		"the healthy owner's ack is still attempted")
	check.Equal("rate_limited", byThread["limited-thread"].SourceAckError)
	check.Nil(byThread["limited-thread"].SourceAckSyncedAt,
		"the exhausted owner's ack stays queued")
	check.Empty(byThread["healthy-thread"].SourceAckError,
		"another owner's exhausted PAT must not defer this ack")
	check.NotNil(byThread["healthy-thread"].SourceAckSyncedAt,
		"the healthy owner's ack propagated in the same pass")
}

// Deferral must be scoped even for queued rows this pass never reaches. With a
// batch that holds only the exhausted owner's ack, the other owner's queued row
// must stay due: a host-wide deferral would push it out by the exhausted
// credential's reset window even though its own PAT has quota.
func TestProcessQueuedNotificationReadsLeavesOtherIdentityQueuedRowsDue(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	limitedRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	healthyRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "other", "thing"),
	)
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	limitedNumber := 7
	healthyNumber := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "limited-thread", RepoID: &limitedRepo,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest",
			SubjectTitle: "Exhausted owner", ItemNumber: &limitedNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "healthy-thread", RepoID: &healthyRepo,
			RepoOwner: "other", RepoName: "thing", SubjectType: "PullRequest",
			SubjectTitle: "Healthy owner", ItemNumber: &healthyNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	require.Len(items, 2)
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	_, err = d.QueueNotificationIDsRead(t.Context(), ids, now.Add(time.Minute))
	require.NoError(err)

	resetAt := time.Now().UTC().Add(time.Hour).Round(0)
	mc := &mockClient{markNotificationThreadReadFn: func(
		_ context.Context, threadID string,
	) error {
		return &gh.RateLimitError{
			Rate: gh.Rate{Reset: gh.Timestamp{Time: resetAt}},
			Response: &http.Response{
				StatusCode: http.StatusForbidden,
				Request: httptest.NewRequest(
					http.MethodPatch,
					"https://api.github.com/notifications/threads/"+threadID, nil,
				),
			},
			Message: "API rate limit exceeded",
		}
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:1"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:1"},
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:2"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:2"},
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	// Batch of one: only the exhausted owner's ack is due this pass.
	err = syncer.ProcessQueuedNotificationReads(
		t.Context(), platform.KindGitHub, "github.com", 1,
	)
	require.Error(err)

	items, err = d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	byThread := map[string]db.Notification{}
	for _, item := range items {
		byThread[item.PlatformNotificationID] = item
	}
	check.Equal("rate_limited", byThread["limited-thread"].SourceAckError)
	check.Empty(byThread["healthy-thread"].SourceAckError,
		"an untouched owner's queued ack must not inherit the deferral")
	check.Nil(byThread["healthy-thread"].SourceAckNextAttemptAt,
		"the healthy owner's ack stays due instead of waiting for another reset")
}

// The post-ack reconciliation refetch spends the same credential's budget, so a
// rate limit there is also owned by one identity. The exhausted owner's queued
// acks defer, while the healthy owner's queued row must still be reached and
// propagated in the same pass.
func TestProcessQueuedNotificationReadsScopesPostAckRefetchRateLimit(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	limitedRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	siblingRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "gadget"),
	)
	require.NoError(err)
	healthyRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "other", "thing"),
	)
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	limitedNumber := 7
	siblingNumber := 9
	healthyNumber := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "limited-thread", RepoID: &limitedRepo,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest",
			SubjectTitle: "Exhausted owner", ItemNumber: &limitedNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "sibling-thread", RepoID: &siblingRepo,
			RepoOwner: "acme", RepoName: "gadget", SubjectType: "PullRequest",
			SubjectTitle: "Same credential", ItemNumber: &siblingNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "healthy-thread", RepoID: &healthyRepo,
			RepoOwner: "other", RepoName: "thing", SubjectType: "PullRequest",
			SubjectTitle: "Healthy owner", ItemNumber: &healthyNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	require.Len(items, 3)
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), ids, queuedAt)
	require.NoError(err)

	resetAt := time.Now().UTC().Add(time.Hour).Round(0)
	getCalls := map[string]int{}
	var marked []string
	mc := &mockClient{
		getNotificationThreadFn: func(
			_ context.Context, threadID string,
		) (NotificationThread, error) {
			getCalls[threadID]++
			// Only the exhausted owner's reconciliation refetch is rate
			// limited; both pre-ack refetches report an unadvanced thread.
			if threadID == "limited-thread" && getCalls[threadID] == 2 {
				return NotificationThread{}, &gh.RateLimitError{
					Rate: gh.Rate{Reset: gh.Timestamp{Time: resetAt}},
					Response: &http.Response{
						StatusCode: http.StatusForbidden,
						Request: httptest.NewRequest(
							http.MethodGet,
							"https://api.github.com/notifications/threads/"+threadID,
							nil,
						),
					},
					Message: "API rate limit exceeded",
				}
			}
			return NotificationThread{
				ID: threadID, SubjectType: "PullRequest",
				Reason: "mention", UpdatedAt: now, LastReadAt: &queuedAt,
			}, nil
		},
		markNotificationThreadReadFn: func(
			_ context.Context, threadID string,
		) error {
			marked = append(marked, threadID)
			return nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:1"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:1"},
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: mc,
			ReadIdentity:  IdentityKey{Host: "github.com", Principal: "user:2"},
			WriteIdentity: IdentityKey{Host: "github.com", Principal: "user:2"},
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	err = syncer.ProcessQueuedNotificationReads(
		t.Context(), platform.KindGitHub, "github.com", 10,
	)
	require.Error(err, "the rate limit is still reported for the host")

	items, err = d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	byThread := map[string]db.Notification{}
	for _, item := range items {
		byThread[item.PlatformNotificationID] = item
	}
	check.ElementsMatch([]string{"limited-thread", "healthy-thread"}, marked,
		"the healthy owner's ack is still attempted after the other's limit")
	check.Nil(byThread["limited-thread"].SourceAckSyncedAt,
		"the exhausted owner's ack reopens instead of clearing")
	check.True(byThread["limited-thread"].Unread,
		"the reconciliation refetch could not prove the thread was unchanged")
	check.Equal("rate_limited", byThread["sibling-thread"].SourceAckError,
		"the exhausted credential's other queued acks back off")
	check.NotNil(byThread["sibling-thread"].SourceAckNextAttemptAt)
	check.Empty(byThread["healthy-thread"].SourceAckError,
		"another owner's exhausted PAT must not defer this ack")
	check.Nil(byThread["healthy-thread"].SourceAckNextAttemptAt,
		"the healthy owner's ack is not pushed out by another reset window")
	check.NotNil(byThread["healthy-thread"].SourceAckSyncedAt,
		"the healthy owner's ack propagated in the same pass")
}

// Notification traffic always resolves to the user credential. A split-auth
// client bypasses the legacy shared read tracker, so the user REST pool in the
// quota registry is the only thing standing between background notification
// work and the reserve held for foreground mutations.
func TestSyncNotificationsStopsWhenUserRESTPoolAtReserve(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				bypassNotificationReadReserve: true,
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		map[string]*SyncBudget{"github.com": NewSyncBudget(1000)},
	)
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(
		HostIdentity("github.com"), QuotaResourceREST,
		Rate{
			Limit:     5000,
			Remaining: RateReserveBuffer,
			Reset:     time.Now().UTC().Add(time.Hour),
		},
	)
	syncer.SetQuotaRegistry(registry)

	syncErr := syncer.SyncNotifications(t.Context())

	require.Error(syncErr)
	require.ErrorContains(syncErr, "user rate reserve exhausted")
	assert.Equal(int32(0), calls.Load())
}

// An unknown pool must not pause notification work: ordinary response headers
// are what populate the registry in the first place.
func TestSyncNotificationsProceedsWhenUserRESTPoolUnknown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	var calls atomic.Int32
	syncer := NewSyncer(
		map[string]Client{
			"github.com": &mockClient{
				bypassNotificationReadReserve: true,
				listNotificationsFn: func(context.Context, NotificationListOptions) ([]NotificationThread, bool, error) {
					calls.Add(1)
					return nil, false, nil
				},
			},
		},
		d,
		nil,
		[]RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		nil,
		map[string]*SyncBudget{"github.com": NewSyncBudget(1000)},
	)
	syncer.SetQuotaRegistry(NewQuotaRegistry())

	require.NoError(syncer.SyncNotifications(t.Context()))
	assert.Positive(calls.Load())
}

// Queued acknowledgement propagation spends the user credential too, so it must
// stop at the reserve instead of discovering it as a per-row rate-limit error.
// The reserve gate is scoped to the credential that would spend it. One
// credential sitting at its reserve must not stop queued acknowledgements
// belonging to a healthy credential on the same host, matching how an actual
// rate-limit response is handled.
func TestProcessQueuedNotificationReadsReserveStopsOnlyItsOwnCredential(t *testing.T) {
	require := require.New(t)
	check := assert.New(t)
	d := openTestDB(t)
	limitedRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	healthyRepo, err := d.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "other", "thing"),
	)
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	limitedNumber := 7
	healthyNumber := 8
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "limited-thread", RepoID: &limitedRepo,
			RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest",
			SubjectTitle: "Exhausted credential", ItemNumber: &limitedNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
		{
			Platform: "github", PlatformHost: "github.com",
			PlatformNotificationID: "healthy-thread", RepoID: &healthyRepo,
			RepoOwner: "other", RepoName: "thing", SubjectType: "PullRequest",
			SubjectTitle: "Healthy credential", ItemNumber: &healthyNumber,
			ItemType: "pr", Reason: "mention", Unread: true,
			SourceUpdatedAt: now, SyncedAt: now,
		},
	}))
	items, err := d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	require.Len(items, 2)
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	queuedAt := now.Add(time.Minute)
	_, err = d.QueueNotificationIDsRead(t.Context(), ids, queuedAt)
	require.NoError(err)

	var marked []string
	mc := &mockClient{
		getNotificationThreadFn: func(
			_ context.Context, threadID string,
		) (NotificationThread, error) {
			return NotificationThread{
				ID: threadID, SubjectType: "PullRequest",
				Reason: "mention", UpdatedAt: now, LastReadAt: &queuedAt,
			}, nil
		},
		markNotificationThreadReadFn: func(
			_ context.Context, threadID string,
		) error {
			marked = append(marked, threadID)
			return nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	limitedIdentity := IdentityKey{Host: "github.com", Principal: "user:1"}
	healthyIdentity := IdentityKey{Host: "github.com", Principal: "user:2"}
	router, err := NewHostRouter("github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: mc,
			ReadIdentity: limitedIdentity, WriteIdentity: limitedIdentity,
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: mc,
			ReadIdentity: healthyIdentity, WriteIdentity: healthyIdentity,
		},
	)
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	reset := time.Now().UTC().Add(time.Hour)
	registry.UpdateSnapshot(limitedIdentity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: RateReserveBuffer, Reset: reset,
	})
	registry.UpdateSnapshot(healthyIdentity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: 4000, Reset: reset,
	})
	syncer.SetQuotaRegistry(registry)

	err = syncer.ProcessQueuedNotificationReads(
		t.Context(), platform.KindGitHub, "github.com", 10,
	)

	require.Error(err, "the exhausted credential is still reported")
	check.Equal([]string{"healthy-thread"}, marked,
		"only the healthy credential's ack may reach upstream")
	items, err = d.ListNotifications(
		t.Context(), db.ListNotificationsOpts{State: "all", Sort: "updated"},
	)
	require.NoError(err)
	byThread := map[string]db.Notification{}
	for _, item := range items {
		byThread[item.PlatformNotificationID] = item
	}
	check.Nil(byThread["limited-thread"].SourceAckSyncedAt,
		"the exhausted credential's ack stays queued")
	check.NotNil(byThread["healthy-thread"].SourceAckSyncedAt,
		"another credential's reserve must not hold back this ack")
}

func TestProcessQueuedNotificationReadsStopsWhenUserRESTPoolAtReserve(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	number := 7
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
		RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
	}}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID}, now.Add(time.Minute))
	require.NoError(err)

	var marked atomic.Int32
	mc := &mockClient{
		bypassNotificationReadReserve: true,
		markNotificationThreadReadFn: func(context.Context, string) error {
			marked.Add(1)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(
		HostIdentity("github.com"), QuotaResourceREST,
		Rate{
			Limit:     5000,
			Remaining: RateReserveBuffer,
			Reset:     time.Now().UTC().Add(time.Hour),
		},
	)
	syncer.SetQuotaRegistry(registry)

	err = syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10)

	require.Error(err)
	require.ErrorContains(err, "user rate reserve exhausted")
	assert.Equal(int32(0), marked.Load())
}

// A queued ack spends up to three user-credential requests, so headroom for
// only one must not admit it: starting with two units above the reserve would
// cross the reserve partway through, after the mark-read already landed.
func TestProcessQueuedNotificationReadsRequiresWorstCaseHeadroom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID, err := d.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	number := 7
	require.NoError(d.UpsertNotifications(t.Context(), []db.Notification{{
		Platform: "github", PlatformHost: "github.com", PlatformNotificationID: "thread-1", RepoID: &repoID,
		RepoOwner: "acme", RepoName: "widget", SubjectType: "PullRequest", SubjectTitle: "Please review",
		WebURL: "https://github.com/acme/widget/pull/7", ItemNumber: &number, ItemType: "pr",
		Reason: "mention", Unread: true, SourceUpdatedAt: now, SyncedAt: now,
	}}))
	items, err := d.ListNotifications(t.Context(), db.ListNotificationsOpts{State: "all"})
	require.NoError(err)
	require.Len(items, 1)
	_, err = d.QueueNotificationIDsRead(t.Context(), []int64{items[0].ID}, now.Add(time.Minute))
	require.NoError(err)

	var marked atomic.Int32
	mc := &mockClient{
		bypassNotificationReadReserve: true,
		markNotificationThreadReadFn: func(context.Context, string) error {
			marked.Add(1)
			return nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, nil, time.Minute, nil, nil)
	registry := NewQuotaRegistry()
	// Two units above the reserve: enough for one request, not for three.
	registry.UpdateSnapshot(
		HostIdentity("github.com"), QuotaResourceREST,
		Rate{
			Limit:     5000,
			Remaining: RateReserveBuffer + 2,
			Reset:     time.Now().UTC().Add(time.Hour),
		},
	)
	syncer.SetQuotaRegistry(registry)

	err = syncer.ProcessQueuedNotificationReads(t.Context(), platform.KindGitHub, "github.com", 10)

	require.Error(err)
	require.ErrorContains(err, "user rate reserve exhausted")
	assert.Equal(int32(0), marked.Load())
}

// The fetcher's GraphQL tracker is host-wide. In quota-registry mode the bulk
// decision must read the repository credential's own pool, so an App
// installation sitting at zero cannot suppress bulk GraphQL for a PAT-backed
// repository on the same host (or the reverse).
func TestGraphQLReadAllowedIsolatesCredentialGraphQLPools(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	appRepo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	userRepo := RepoRef{Owner: "other", Name: "gadget", PlatformHost: "github.com"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil, []RepoRef{appRepo, userRepo}, time.Minute, nil, nil,
	)
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:42"}
	userIdentity := IdentityKey{Host: "github.com", Principal: "user:7"}
	router, routerErr := NewHostRouter(
		"github.com",
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
			ReadIdentity: appIdentity, WriteIdentity: appIdentity,
		},
		&Route{
			Key: RouteKey{Host: "github.com", Owner: "other"}, Client: client,
			ReadIdentity: userIdentity, WriteIdentity: userIdentity,
		},
	)
	require.NoError(t, routerErr)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	// The App pool is exhausted; the user pool is healthy.
	registry.UpdateSnapshot(appIdentity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 0, Reset: time.Now().UTC().Add(time.Hour),
	})
	registry.UpdateSnapshot(userIdentity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 4000, Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)

	// A host-wide tracker reporting exhaustion would block both repos.
	exhausted := NewRateTracker(d, "github.com", "host", "graphql")
	exhausted.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 0, Reset: time.Now().UTC().Add(time.Hour),
	})
	fetcher := &GraphQLFetcher{rateTracker: exhausted}
	backoff, _ := fetcher.ShouldBackoff()
	assert.True(backoff, "host-wide tracker should report exhaustion")

	assert.False(syncer.graphQLReadAllowed(t.Context(), appRepo, fetcher),
		"exhausted App credential must back off")
	assert.True(syncer.graphQLReadAllowed(t.Context(), userRepo, fetcher),
		"healthy user credential must not inherit the App pool's exhaustion")
}

func TestGraphQLReadAllowedUsesArchiveCredentialPool(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	normalIdentity := IdentityKey{Host: "github.com", Principal: "user:7"}
	archiveIdentity := IdentityKey{Host: "github.com", Principal: "installation:20"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity:  normalIdentity,
		ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme"},
		ArchiveClient: client, ArchiveFetcher: &GraphQLFetcher{},
		ArchiveReadIdentity: archiveIdentity,
	})
	require.NoError(err)
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	reset := time.Now().UTC().Add(time.Hour)
	registry.UpdateSnapshot(normalIdentity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 0, Reset: reset,
	})
	registry.UpdateSnapshot(archiveIdentity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 4000, Reset: reset,
	})
	syncer.SetQuotaRegistry(registry)
	fetcher := &GraphQLFetcher{}

	assert.False(syncer.graphQLReadAllowed(
		WithSyncBudget(t.Context()), repo, fetcher,
	), "ordinary GraphQL must honor the exhausted ordinary pool")
	assert.True(syncer.graphQLReadAllowed(
		WithArchiveSyncBudget(t.Context()), repo, fetcher,
	), "archive GraphQL must use the healthy archive pool")
}

// Bulk GraphQL is an optional optimization with a REST fallback, so a
// credential whose GraphQL pool is exhausted must keep syncing over REST
// instead of being held out of background scheduling entirely.
func TestRepoEligibilityIgnoresExhaustedGraphQLWhenRESTHasCapacity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	reset := time.Now().UTC().Add(time.Hour)
	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: 4000, Reset: reset,
	})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 0, Reset: reset,
	})
	syncer.SetQuotaRegistry(registry)

	bucket, err := syncer.bucketKeyForRepo(repo, false)
	require.NoError(err)
	eligible := syncer.repoEligibility([]RepoRef{repo}, map[string]time.Time{})

	assert.True(eligible[bucket],
		"REST capacity must keep the repository schedulable despite exhausted GraphQL")
}

// An exhausted REST pool still stops background scheduling even when the
// GraphQL pool has never been observed: unknown quota must not mask it.
func TestRepoEligibilityStopsOnExhaustedRESTWithUnobservedGraphQL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: RateReserveBuffer,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)

	bucket, err := syncer.bucketKeyForRepo(repo, false)
	require.NoError(err)
	eligible := syncer.repoEligibility([]RepoRef{repo}, map[string]time.Time{})

	assert.False(eligible[bucket],
		"an exhausted REST reserve must stop scheduling regardless of GraphQL")
}

// Background admission gates on REST alone, so graphQLReadAllowed is where the
// GraphQL reserve is applied. A background sync holding GraphQL
// headroom inside the reserve must fall back to REST rather than spend the
// capacity held for foreground work, while the same pool stays usable for an
// explicit foreground sync.
func TestGraphQLReadAllowedAppliesReserveOnlyToBackgroundSyncs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	// Inside the reserve but not empty.
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: RateReserveBuffer / 2,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)
	fetcher := &GraphQLFetcher{}

	assert.False(
		syncer.graphQLReadAllowed(WithSyncBudget(t.Context()), repo, fetcher),
		"background bulk GraphQL must not spend the foreground reserve")
	assert.True(
		syncer.graphQLReadAllowed(t.Context(), repo, fetcher),
		"an explicit foreground sync may use GraphQL headroom inside the reserve")
}

func TestCurrentCommentVisibilitySkipsGraphQLInsideBackgroundReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	client := &credentialRateLimitSnapshotMockClient{mockClient: &mockClient{}}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	var requests atomic.Int32
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"query should not run"}]}`))
	}))
	defer gqlSrv.Close()
	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(gqlSrv.URL, gqlSrv.Client()), nil,
	)
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, d, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		Fetcher: fetcher, ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: RateReserveBuffer / 2,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)
	ctx := WithSyncBudget(t.Context())

	_, prObserved := syncer.currentPRCommentVisibility(ctx, repo, 7)
	_, issueObserved := syncer.currentIssueCommentVisibility(ctx, repo, 8)

	assert.False(prObserved)
	assert.False(issueObserved)
	assert.Zero(requests.Load())
}

// End-to-end through the syncer and SQLite: a credential whose GraphQL pool is
// exhausted but whose REST pool has capacity must still complete a real sync
// and persist its results. The eligibility unit tests above cover the decision;
// this covers the outcome the decision exists for.
func TestRunOnceSyncsOverRESTWhenOnlyGraphQLIsExhausted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &detailTrackingClient{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now), buildOpenPR(2, now)}

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "owner"}, Client: mc,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	reset := time.Now().UTC().Add(time.Hour)
	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: 4000, Reset: reset,
	})
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: 0, Reset: reset,
	})
	syncer.SetQuotaRegistry(registry)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(ctx)

	require.Len(results, 1)
	assert.Empty(results[0].Error,
		"exhausted GraphQL must not skip a repository with REST capacity")
	assert.True(mc.listOpenPRsCalled.Load(),
		"the sync must reach upstream over REST")
	for _, number := range []int{1, 2} {
		pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", number)
		require.NoError(err)
		require.NotNil(pr, "pull request %d must be persisted", number)
		assert.Equal(number, pr.Number)
	}
}

// Issue comment refreshes spend the same credential as pull-request refreshes,
// so an issue-only queue must stop at the reserve too.
func TestCommentDrainStopsIssueRefreshesAtTheReserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", repo.Owner, repo.Name),
	)
	require.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = d.UpsertIssue(ctx, &db.Issue{
		RepoID: repoID, PlatformID: 2001, Number: 11,
		URL:   "https://github.com/owner/repo/issues/11",
		Title: "needs comments", Author: "grace", State: "open",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)

	var listed atomic.Int32
	mc := &mockClient{}
	mc.listIssueCommentsFn = func(
		_ context.Context, _, _ string, _ int,
	) ([]*gh.IssueComment, error) {
		listed.Add(1)
		return nil, nil
	}
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: RateReserveBuffer,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil, []RepoRef{repo},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "owner"}, Client: mc,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	syncer.SetQuotaRegistry(registry)
	syncer.queueIssueCommentSync(repo, repoID, 11)

	bucket := RateBucketKey("github", "github.com", "user:7")
	eligible := map[string]bool{bucket: true}
	syncer.drainPendingCommentSyncs(ctx, eligible)

	assert.Zero(listed.Load(),
		"an issue-only comment queue must not reach upstream at the reserve")
	assert.False(eligible[bucket],
		"the issue loop must revoke the exhausted credential, as the "+
			"pull-request loop does, so its remaining items stop too")
}

// After a restart the quota registry is empty while the write tracker still
// remembers the notification credential's pool. A three-request acknowledgement
// must not start on 201 remaining just because the registry is silent.
func TestNotificationBudgetUsesPersistedWriteTrackerAfterRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	bucket := RateBucketKey("github", "github.com", "user:7")
	tracker := NewRateTracker(d, "github.com", "user:7", "rest")
	tracker.UpdateFromSnapshot(Rate{
		Limit: 5000, Remaining: RateReserveBuffer + 2,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	mc := &mockClient{}
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "owner"}, Client: mc,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer := &Syncer{
		routers:           map[string]*HostRouter{"github.com": router},
		writeRateTrackers: map[string]*RateTracker{bucket: tracker},
		quotaRegistry:     NewQuotaRegistry(),
	}

	err = syncer.ensureNotificationBudget(
		repo, mc, notificationAckWorstCaseRequests,
	)

	require.Error(err,
		"a three-request ack must not start two requests above the reserve")
	assert.Contains(err.Error(), "user rate reserve exhausted")
}

func TestRunOnceSkipsSyncWhenRESTPoolExhausted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mc := &detailTrackingClient{}
	mc.openPRs = []*gh.PullRequest{buildOpenPR(1, now)}

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{repo}, time.Minute, nil, nil,
	)
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "owner"}, Client: mc,
		ReadIdentity: identity, WriteIdentity: identity,
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: RateReserveBuffer,
		Reset: time.Now().UTC().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)
	var results []RepoSyncResult
	syncer.SetOnSyncCompleted(func(got []RepoSyncResult) { results = got })

	syncer.RunOnce(ctx)

	require.Len(results, 1)
	assert.Equal("skipped: rate limit throttled", results[0].Error)
	assert.False(mc.listOpenPRsCalled.Load(),
		"an exhausted REST reserve must stop work before the wire")
	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	assert.Nil(pr, "nothing may be persisted when the sync was skipped")
}

// Without a registry the host-wide tracker remains authoritative.
func TestGraphQLReadAllowedFallsBackToHostTracker(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	repo := RepoRef{Owner: "acme", Name: "widget", PlatformHost: "github.com"}
	syncer := NewSyncer(
		map[string]Client{"github.com": &mockClient{}},
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	exhausted := NewRateTracker(d, "github.com", "host", "graphql")
	exhausted.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 0, Reset: time.Now().UTC().Add(time.Hour),
	})

	assert.False(syncer.graphQLReadAllowed(t.Context(), repo, &GraphQLFetcher{rateTracker: exhausted}))
	assert.True(syncer.graphQLReadAllowed(t.Context(), repo, &GraphQLFetcher{}))
}

func TestPublishResolvedRepositoryAliasesCredentialRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	router, err := NewHostRouter("github.com",
		&Route{
			Key:          RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "widget-bot"},
		},
		&Route{
			Key:          RouteKey{Host: "github.com"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "fallback-bot"},
		},
	)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		nil, nil, nil, []RepoRef{configured}, time.Minute, nil, nil,
	)
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})

	renamed := configured
	renamed.Name = "gadget"
	renamed.RepoPath = "acme/gadget"
	syncer.publishResolvedRepository(configured, renamed, true)

	identity, ok := syncer.ReadIdentityForRepo(renamed)
	require.True(ok)
	assert.Equal("widget-bot", identity.Principal,
		"renamed repo must keep its configured credential route")
}

// TestReconcileRepoIdentityAbortsWhenRejectedObservationIsInactive covers a
// sync racing a route replacement: the catalog rejects the sync's stale
// observation and resolves it to the displaced, inactive repository. The sync
// must abort rather than publish that entry and fetch the reused route's
// content into the preserved repository's history.
func TestReconcileRepoIdentityAbortsWhenRejectedObservationIsInactive(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	_, _, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "R_old",
		Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
	}, time.Now().UTC().Add(30*time.Minute))
	require.NoError(err)
	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "R_new",
		Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
	}, time.Now().UTC().Add(time.Hour))
	require.NoError(err)

	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, staleMetadataRepositoryReader{
			kind: platform.KindGitHub, host: "github.com",
			repo: platform.Repository{
				Ref: platform.RepoRef{
					Platform: platform.KindGitHub, Host: "github.com",
					Owner: "acme", Name: "widget", RepoPath: "acme/widget",
					PlatformExternalID: "R_old",
				},
			},
		}),
		d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	_, _, _, _, _, err = syncer.reconcileRepoIdentityObservation(ctx, repo)
	require.ErrorContains(err, "stale")
}

func seedDisplacedRepository(t *testing.T, database *db.DB) int64 {
	t.Helper()
	now := time.Now().UTC()
	displaced, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-old",
			Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
		}, now,
	)
	require.NoError(t, err)
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-new",
			Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
		}, now.Add(time.Hour),
	)
	require.NoError(t, err)
	return displaced.Repository.ID
}

func TestCommitMergeRequestParentSnapshotRejectsDisplacedRepository(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	displacedID := seedDisplacedRepository(t, database)
	syncer := NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	now := time.Now().UTC()

	_, _, _, err := syncer.CommitMergeRequestParentSnapshot(
		t.Context(),
		RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		},
		&db.MergeRequest{
			RepoID: displacedID, PlatformID: 7001, Number: 7,
			URL:   "https://github.com/acme/widget/pull/7",
			Title: "stale sync write", Author: "ada", State: "open",
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.ErrorContains(err, "route",
		"a displaced repository must not receive the route's new data")
}

func TestCommitIssueParentSnapshotRejectsDisplacedRepository(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	displacedID := seedDisplacedRepository(t, database)
	syncer := NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	now := time.Now().UTC()

	_, _, _, err := syncer.commitIssueParentSnapshot(
		t.Context(),
		RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		},
		&db.Issue{
			RepoID: displacedID, PlatformID: 8001, Number: 8,
			URL:   "https://github.com/acme/widget/issues/8",
			Title: "stale sync write", Author: "ada", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.ErrorContains(err, "route",
		"a displaced repository must not receive the route's new data")
}

func TestCommitIssueCommentsSnapshotBindsToParentID(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC()
	displaced, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-old",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now)
	require.NoError(err)
	oldIssueID, oldRevision, accepted, err := database.UpsertIssueSnapshotWithLabels(
		ctx, &db.Issue{
			RepoID: displaced.Repository.ID, PlatformID: 5001, Number: 5,
			URL: "https://github.com/acme/widget/issues/5", Title: "displaced",
			Author: "ada", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.NoError(err)
	require.True(accepted)
	replacement, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-new",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now.Add(time.Hour))
	require.NoError(err)
	newIssueID, _, accepted, err := database.UpsertIssueSnapshotWithLabels(
		ctx, &db.Issue{
			RepoID: replacement.Repository.ID, PlatformID: 5002, Number: 5,
			URL: "https://github.com/acme/widget/issues/5", Title: "replacement",
			Author: "bo", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.NoError(err)
	require.True(accepted)

	syncer := &Syncer{db: database}
	applied, err := syncer.commitIssueCommentsSnapshot(
		ctx,
		RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		},
		oldIssueID, 5, oldRevision,
		[]db.IssueEvent{{
			IssueID: oldIssueID, EventType: "comment", Author: "ada",
			Body: "stale comment", CreatedAt: now, DedupeKey: "comment-1",
		}},
		nil, nil,
	)
	require.NoError(err)
	require.True(applied)

	oldEvents, err := database.ListIssueEvents(ctx, oldIssueID)
	require.NoError(err)
	require.Len(oldEvents, 1,
		"child snapshot must attach to the parent the caller fetched for")
	newEvents, err := database.ListIssueEvents(ctx, newIssueID)
	require.NoError(err)
	require.Empty(newEvents,
		"the replacement repository's issue must not receive the stale comments")
}

func TestCommitMergeRequestDatasetsBindsToParentID(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC()
	displaced, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-old",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now)
	require.NoError(err)
	oldMRID, oldRevision, accepted, err := database.UpsertMergeRequestSnapshotWithLabels(
		ctx, &db.MergeRequest{
			RepoID: displaced.Repository.ID, PlatformID: 7001, Number: 7,
			URL: "https://github.com/acme/widget/pull/7", Title: "displaced",
			Author: "ada", State: "open", HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.NoError(err)
	require.True(accepted)
	replacement, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-new",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now.Add(time.Hour))
	require.NoError(err)
	newMRID, _, accepted, err := database.UpsertMergeRequestSnapshotWithLabels(
		ctx, &db.MergeRequest{
			RepoID: replacement.Repository.ID, PlatformID: 7002, Number: 7,
			URL: "https://github.com/acme/widget/pull/7", Title: "replacement",
			Author: "bo", State: "open", HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		},
	)
	require.NoError(err)
	require.True(accepted)

	syncer := &Syncer{db: database}
	applied, err := syncer.commitMergeRequestDatasets(
		ctx,
		RepoRef{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		},
		oldMRID, 7, oldRevision,
		[]db.MREvent{{
			MergeRequestID: oldMRID, EventType: "comment", Author: "ada",
			Body: "stale comment", CreatedAt: now, DedupeKey: "comment-1",
		}},
		true, nil, nil, nil, false, nil, nil, "",
	)
	require.NoError(err)
	require.True(applied)

	oldEvents, err := database.ListMREvents(ctx, oldMRID)
	require.NoError(err)
	require.Len(oldEvents, 1,
		"child snapshot must attach to the parent the caller fetched for")
	newEvents, err := database.ListMREvents(ctx, newMRID)
	require.NoError(err)
	require.Empty(newEvents,
		"the replacement repository's MR must not receive the stale datasets")
}

func TestReconcileMergedActorEventsBackfillsForgeMergedMR(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	// Simulate a merge performed through forge: the mutation eagerly marks
	// the MR merged, so the sync's open->closed transition never fires and
	// no authored merged event is ever written.
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	merged := true
	mergedBy := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.ClosedAt = makeTimestamp(mergedAt)
	pr.UpdatedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &mergedBy}
	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)
	syncer.SetClock(func() time.Time { return now.Add(time.Hour) })

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	syncer.reconcileMergedActorEvents(ctx, repo, repoID)

	stored, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	events, err := d.ListMREvents(ctx, stored.ID)
	require.NoError(err)
	require.NotEmpty(events)
	found := false
	for _, event := range events {
		if event.EventType == "merged" && event.Author == "merge-admin" {
			found = true
		}
	}
	assert.True(found, "authored merged event must be backfilled")
}

func TestReconcileMergedActorEventsEnrichesActorlessMergedEvent(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))
	stored, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: stored.ID,
		EventType:      "merged",
		Summary:        "merged this",
		CreatedAt:      mergedAt.Add(time.Minute),
		DedupeKey:      "provider-merged-event",
	}}))

	merged := true
	actor := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &actor}
	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	syncer.SetClock(func() time.Time { return now.Add(time.Hour) })

	syncer.reconcileMergedActorEvents(
		ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID,
	)

	events, err := d.ListMREvents(ctx, stored.ID)
	require.NoError(err)
	mergedEvents := make([]db.MREvent, 0, 1)
	for _, event := range events {
		if event.EventType == "merged" {
			mergedEvents = append(mergedEvents, event)
		}
	}
	require.Len(mergedEvents, 1)
	require.Equal("provider-merged-event", mergedEvents[0].DedupeKey)
	require.Equal("merge-admin", mergedEvents[0].Author)
	require.Equal(mergedAt, mergedEvents[0].CreatedAt)
}

func TestBackfillMergedActorRejectsReusedRepositoryRoute(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	displacedID := seedDisplacedRepository(t, d)

	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(displacedID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, displacedID, 7, "merged", &mergedAt, &mergedAt))

	providerCalls := 0
	mc := &mockClient{getPullRequestFn: func(
		_ context.Context, _, _ string, _ int,
	) (*gh.PullRequest, error) {
		providerCalls++
		merged := true
		actor := "replacement-admin"
		pr.State = new("closed")
		pr.Merged = &merged
		pr.MergedAt = makeTimestamp(mergedAt)
		pr.MergedBy = &gh.User{Login: &actor}
		return pr, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			PlatformExternalID: "repo-new",
			Owner:              "acme", Name: "widget", RepoPath: "acme/widget",
		}},
		time.Minute, nil, nil,
	)

	inserted, err := syncer.BackfillMergedActorEventOnProvider(ctx, displacedID, 7)
	require.ErrorContains(err, "provider ID")
	require.False(inserted)
	require.Zero(providerCalls,
		"a route replacement must be rejected before fetching its pull request")
}

func TestBackfillMergedActorUsesProviderMatchedRouteAfterRename(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	providerID := "repo-stable"

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: providerID,
		Owner: "acme", Name: "old-name", RepoPath: "acme/old-name",
	})
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	merged := true
	actor := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &actor}
	mc := &mockClient{
		getPullRequestFn: func(_ context.Context, owner, name string, _ int) (*gh.PullRequest, error) {
			if owner != "acme" || name != "new-name" {
				return nil, fmt.Errorf("backfill used stale route %s/%s", owner, name)
			}
			return pr, nil
		},
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			if owner != "acme" || name != "new-name" {
				return nil, fmt.Errorf("identity check used stale route %s/%s", owner, name)
			}
			id := int64(42)
			return &gh.Repository{
				ID: &id, NodeID: &providerID, Name: &name,
				Owner: &gh.User{Login: &owner},
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			PlatformExternalID: providerID,
			Owner:              "acme", Name: "new-name", RepoPath: "acme/new-name",
		}},
		time.Minute, nil, nil,
	)

	inserted, err := syncer.BackfillMergedActorEventOnProvider(ctx, repoID, 7)
	require.NoError(err)
	require.True(inserted)
}

func TestBackfillMergedActorRevalidatesProviderIdentityBeforePersisting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	providerID := "repo-old"

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: providerID,
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	})
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	merged := true
	actor := "replacement-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &actor}
	replacementID := "repo-new"
	mc := &mockClient{
		getPullRequestFn: func(context.Context, string, string, int) (*gh.PullRequest, error) {
			return pr, nil
		},
		getRepositoryFn: func(_ context.Context, owner, name string) (*gh.Repository, error) {
			id := int64(43)
			return &gh.Repository{
				ID: &id, NodeID: &replacementID, Name: &name,
				Owner: &gh.User{Login: &owner},
			}, nil
		},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			PlatformExternalID: providerID,
			Owner:              "acme", Name: "widget", RepoPath: "acme/widget",
		}},
		time.Minute, nil, nil,
	)

	inserted, err := syncer.BackfillMergedActorEventOnProvider(ctx, repoID, 7)
	require.ErrorContains(err, "provider ID")
	require.False(inserted)
	stored, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	events, err := d.ListMREvents(ctx, stored.ID)
	require.NoError(err)
	require.Empty(events, "replacement repository actor must not be persisted")
}

func TestBackfillMergedActorVerifiesRouteWhenTrackedProviderIDMissing(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	merged := true
	actor := "merge-admin"
	pr.State = new("closed")
	pr.Merged = &merged
	pr.MergedAt = makeTimestamp(mergedAt)
	pr.MergedBy = &gh.User{Login: &actor}
	mc := &mockClient{singlePR: pr}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)

	inserted, err := syncer.BackfillMergedActorEventOnProvider(ctx, repoID, 7)
	require.NoError(err)
	require.True(inserted)
}

func TestReconcileMergedActorEventsSweepsPastPersistentlyMissingActors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(err)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	// Twelve forge-merged MRs missing the actor. The provider permanently
	// reports no merged_by for the ten newest (e.g. the merging account was
	// deleted), so a fixed newest-first batch would retry them every cycle
	// and never reach the two oldest.
	actorless := map[int]bool{}
	for number := 3; number <= 12; number++ {
		actorless[number] = true
	}
	for number := 1; number <= 12; number++ {
		pr := buildOpenPR(number, now)
		normalizedPR, err := NormalizePR(repoID, pr)
		require.NoError(err)
		_, err = d.UpsertMergeRequest(ctx, normalizedPR)
		require.NoError(err)
		mergedAt := now.Add(time.Duration(number) * time.Minute)
		require.NoError(d.UpdateMRState(ctx, repoID, number, "merged", &mergedAt, &mergedAt))
	}

	mc := &mockClient{
		getPullRequestFn: func(_ context.Context, _, _ string, number int) (*gh.PullRequest, error) {
			pr := buildOpenPR(number, now)
			merged := true
			mergedAt := now.Add(time.Duration(number) * time.Minute)
			pr.State = new("closed")
			pr.Merged = &merged
			pr.MergedAt = makeTimestamp(mergedAt)
			pr.ClosedAt = makeTimestamp(mergedAt)
			pr.UpdatedAt = makeTimestamp(mergedAt)
			if !actorless[number] {
				actor := "merge-admin"
				pr.MergedBy = &gh.User{Login: &actor}
			}
			return pr, nil
		},
	}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)
	syncer.SetClock(func() time.Time { return now.Add(time.Hour) })

	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}
	syncer.reconcileMergedActorEvents(ctx, repo, repoID)
	syncer.reconcileMergedActorEvents(ctx, repo, repoID)

	for _, number := range []int{1, 2} {
		stored, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, number)
		require.NoError(err)
		require.NotNil(stored)
		events, err := d.ListMREvents(ctx, stored.ID)
		require.NoError(err)
		found := false
		for _, event := range events {
			if event.EventType == "merged" && event.Author == "merge-admin" {
				found = true
			}
		}
		assert.True(found,
			"MR #%d must receive its merged actor even while newer candidates stay unresolved", number)
	}
}

func TestReconcileMergedActorEventsCoolsDownAfterSweepExhaustion(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	clock := now.Add(time.Hour)

	repoID, err := d.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	pr := buildOpenPR(7, now)
	normalizedPR, err := NormalizePR(repoID, pr)
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, normalizedPR)
	require.NoError(err)
	mergedAt := now.Add(time.Minute)
	require.NoError(d.UpdateMRState(ctx, repoID, 7, "merged", &mergedAt, &mergedAt))

	providerCalls := 0
	mc := &mockClient{getPullRequestFn: func(
		_ context.Context, _, _ string, _ int,
	) (*gh.PullRequest, error) {
		providerCalls++
		merged := true
		pr.State = new("closed")
		pr.Merged = &merged
		pr.MergedAt = makeTimestamp(mergedAt)
		pr.MergedBy = nil
		return pr, nil
	}}
	syncer := NewSyncer(
		map[string]Client{"github.com": mc}, d, nil,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	syncer.SetClock(func() time.Time { return clock })
	repo := RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}

	syncer.reconcileMergedActorEvents(ctx, repo, repoID)
	require.Equal(1, providerCalls)
	clock = clock.Add(59 * time.Minute)
	syncer.reconcileMergedActorEvents(ctx, repo, repoID)
	require.Equal(1, providerCalls, "an exhausted sweep must retain its cooldown")
	clock = clock.Add(time.Minute)
	syncer.reconcileMergedActorEvents(ctx, repo, repoID)
	require.Equal(2, providerCalls, "a new sweep must start after the bounded cooldown")
}

func TestWithObsoleteMetadata(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		obsolete bool
		want     string
		changed  bool
	}{
		{"set on existing metadata", `{"commit_order_key":3}`, true, `{"commit_order_key":3,"obsolete":true}`, true},
		{"set on empty metadata", ``, true, `{"obsolete":true}`, true},
		{"already set", `{"commit_order_key":3,"obsolete":true}`, true, `{"commit_order_key":3,"obsolete":true}`, false},
		{"clear removes key", `{"commit_order_key":3,"obsolete":true}`, false, `{"commit_order_key":3}`, true},
		{"clear when absent", `{"commit_order_key":3}`, false, `{"commit_order_key":3}`, false},
		{"clear normalizes non-bool garbage", `{"obsolete":"yes"}`, false, `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withObsoleteMetadata(tc.in, tc.obsolete)
			assert.Equal(t, tc.changed, changed)
			if changed {
				assert.JSONEq(t, tc.want, got)
			} else {
				assert.Equal(t, tc.in, got)
			}
		})
	}
}

func TestWithCommitOrderMetadataPreservesCommitAuthor(t *testing.T) {
	withOrder := withCommitOrderMetadata(`{"commit_author":"original-author"}`, 2, 4)
	assert.JSONEq(t, `{"commit_author":"original-author","commit_order":2,"commit_order_key":4}`, withOrder)

	withObsolete, changed := withObsoleteMetadata(withOrder, true)
	assert.True(t, changed)
	assert.JSONEq(t, `{"commit_author":"original-author","commit_order":2,"commit_order_key":4,"obsolete":true}`, withObsolete)
}

// TestSyncRepoDropsStaleSettingsSnapshotBehindNewerObservation simulates a
// notification sync committing fresher repository settings between this
// sync's provider snapshot capture and its settings write. The delayed full
// sync must drop its stale snapshot instead of overwriting the newer
// settings; the route generation cannot catch this because same-route
// observations do not advance it.
func TestSyncRepoDropsStaleSettingsSnapshotBehindNewerObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	identity := db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}
	seeded, accepted, err := d.ReconcileRepositoryObservation(
		ctx, identity, time.Now().UTC().Add(-time.Hour),
	)
	require.NoError(err)
	require.True(accepted)
	repoID := seeded.Repository.ID

	injectNewerObservation := func() {
		// Stamp strictly after any timestamp the sync captured before this
		// provider call: wall-clock reads can collide at microsecond
		// granularity, and an equal-timestamp observation ties instead of
		// winning the watermark comparison.
		observedAt := time.Now().UTC().Add(time.Millisecond)
		_, accepted, err := d.ReconcileRepositoryObservation(ctx, identity, observedAt)
		require.NoError(err)
		require.True(accepted)
		applied, err := d.UpdateRepoProviderObservation(
			ctx, repoID, observedAt,
			db.RepoProviderMetadata{
				PlatformRepoID: "gid://gitlab/Project/42",
				WebURL:         "https://gitlab.example.com/group/project",
				CloneURL:       "https://gitlab.example.com/group/project.git",
				DefaultBranch:  "newer-main",
			},
			&db.RepoMergeSettings{},
			new(false),
		)
		require.NoError(err)
		require.True(applied)
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
		},
		getRepositoryFn: func(context.Context, platform.RepoRef) (platform.Repository, error) {
			injectNewerObservation()
			return platform.Repository{
				Ref: platform.RepoRef{
					Platform: platform.KindGitLab, Host: "gitlab.example.com",
					Owner: "group", Name: "project", RepoPath: "group/project",
				},
				PlatformExternalID: "gid://gitlab/Project/42",
				DefaultBranch:      "stale-main",
				WebURL:             "https://gitlab.example.com/group/project-stale",
				CloneURL:           "https://gitlab.example.com/group/project-stale.git",
				MergeSettings: &platform.RepositoryMergeSettings{
					AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true,
				},
				ViewerCanMerge: new(true),
			}, nil
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{{
			Platform:           platform.KindGitLab,
			PlatformHost:       "gitlab.example.com",
			Owner:              "group",
			Name:               "project",
			RepoPath:           "group/project",
			PlatformExternalID: "gid://gitlab/Project/42",
		}}, time.Minute, nil, nil,
	)

	// Both the reconciled snapshot and the refetch lose to the injected
	// newer observations, so the sync aborts rather than indexing against
	// unverified settings — and the newer settings survive untouched.
	require.ErrorContains(syncer.syncRepo(ctx, syncer.repos[0]), "kept losing")

	stored, err := d.GetRepoByID(ctx, repoID)
	require.NoError(err)
	require.NotNil(stored)
	assert.False(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)
	assert.False(stored.ViewerCanMerge)
	assert.Equal("newer-main", stored.DefaultBranch)
	assert.Equal("https://gitlab.example.com/group/project", stored.WebURL)
	assert.Equal(int32(2), provider.getRepositoryCalls.Load())
}

// TestSyncMRForRepoPersistsSettingsForReplacementRepository covers a reused
// route: the tracked path now belongs to a different provider repository.
// Direct MR sync creates the replacement catalog row; it must persist the
// verified provider snapshot so the row does not advertise the permissive
// schema defaults (all merge methods allowed, viewer can merge).
func TestSyncMRForRepoPersistsSettingsForReplacementRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	_, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}, now.Add(-time.Hour))
	require.NoError(err)
	require.True(accepted)

	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
		RepoPath:     "group/project",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
			mergeRequests: []platform.MergeRequest{{
				Repo:           platformRepoRef(repo),
				PlatformID:     1001,
				Number:         7,
				URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
				Title:          "replacement MR",
				Author:         "ada",
				State:          "open",
				HeadBranch:     "feature",
				BaseBranch:     "main",
				CreatedAt:      now,
				UpdatedAt:      now,
				LastActivityAt: now,
			}},
		},
		repository: platform.Repository{
			Ref:                platformRepoRef(repo),
			PlatformExternalID: "gid://gitlab/Project/99",
			DefaultBranch:      "main",
			WebURL:             "https://gitlab.example.com/group/project",
			CloneURL:           "https://gitlab.example.com/group/project.git",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
			ViewerCanMerge: new(false),
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.syncMRForRepo(ctx, repo, 7, false, nil))

	entry, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/99",
	)
	require.NoError(err)
	require.NotNil(entry, "direct sync must catalog the replacement repository")
	stored, err := d.GetRepoByID(ctx, entry.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.True(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)
	assert.False(stored.ViewerCanMerge)
	assert.Equal("main", stored.DefaultBranch)
}

// TestSyncIssueForRepoPersistsSettingsForReplacementRepository is the issue
// analog of the direct MR sync case: the replacement repository row created
// during a direct issue sync must carry the provider's verified settings.
func TestSyncIssueForRepoPersistsSettingsForReplacementRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	_, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}, now.Add(-time.Hour))
	require.NoError(err)
	require.True(accepted)

	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
		RepoPath:     "group/project",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
			issues: []platform.Issue{{
				Repo:           platformRepoRef(repo),
				PlatformID:     2001,
				Number:         11,
				URL:            "https://gitlab.example.com/group/project/-/issues/11",
				Title:          "replacement issue",
				Author:         "grace",
				State:          "open",
				CreatedAt:      now,
				UpdatedAt:      now,
				LastActivityAt: now,
			}},
		},
		repository: platform.Repository{
			Ref:                platformRepoRef(repo),
			PlatformExternalID: "gid://gitlab/Project/99",
			DefaultBranch:      "main",
			WebURL:             "https://gitlab.example.com/group/project",
			CloneURL:           "https://gitlab.example.com/group/project.git",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
			ViewerCanMerge: new(false),
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	require.NoError(syncer.syncIssueForRepo(ctx, repo, 11, nil))

	entry, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/99",
	)
	require.NoError(err)
	require.NotNil(entry, "direct sync must catalog the replacement repository")
	stored, err := d.GetRepoByID(ctx, entry.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.True(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)
	assert.False(stored.ViewerCanMerge)
	assert.Equal("main", stored.DefaultBranch)
}

// TestPersistRepoSettingsObservationDistinguishesFenceErrorFromStale pins the
// helper's contract: a route fence mismatch surfaces as an error callers can
// stop on, while a snapshot that merely lost to a newer observation reports
// applied=false with no error so syncs continue on the fresher data.
func TestPersistRepoSettingsObservationDistinguishesFenceErrorFromStale(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-old", Owner: "acme", Name: "widget",
		RepoPath: "acme/widget",
	}
	entry, accepted, err := d.ReconcileRepositoryObservation(ctx, identity, now)
	require.NoError(err)
	require.True(accepted)
	repoID := entry.Repository.ID
	fence, found, err := d.CurrentRepositoryRouteFence(ctx, identity, repoID)
	require.NoError(err)
	require.True(found)
	syncer := &Syncer{db: d}
	repo := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	snapshot := platform.Repository{
		Ref: platform.RepoRef{
			Platform: platform.KindGitHub, Host: "github.com",
			Owner: "acme", Name: "widget", RepoPath: "acme/widget",
		},
		PlatformExternalID: "repo-old",
		MergeSettings:      &platform.RepositoryMergeSettings{AllowSquashMerge: true},
	}

	// A newer same-route observation makes the captured snapshot stale:
	// no error, not applied.
	_, accepted, err = d.ReconcileRepositoryObservation(ctx, identity, now.Add(time.Second))
	require.NoError(err)
	require.True(accepted)
	applied, err := syncer.persistRepoSettingsObservation(
		ctx, repo, repoID, now, snapshot, fence,
	)
	require.NoError(err)
	assert.False(applied)

	// A different repository claiming the route invalidates the captured
	// fence: the mismatch is an error, not a silent skip.
	_, accepted, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-new", Owner: "acme", Name: "widget",
		RepoPath: "acme/widget",
	}, now.Add(2*time.Second))
	require.NoError(err)
	require.True(accepted)
	_, err = syncer.persistRepoSettingsObservation(
		ctx, repo, repoID, now, snapshot, fence,
	)
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
}

// watermarkAdvancingArchiveLifecycle mimics the real archive service: route
// changes and first encounters make reconcileArchiveRepositoryIfNeeded call
// EnsureConfigured, which records its own identity observation for tracked
// repositories and thereby advances the observation watermark past the
// snapshot the surrounding sync is still holding.
type watermarkAdvancingArchiveLifecycle struct {
	db *db.DB
}

func (l watermarkAdvancingArchiveLifecycle) EnsureConfigured(
	ctx context.Context, refs []platform.RepoRef,
) ([]platform.RepoRef, error) {
	for _, ref := range refs {
		identity := platform.DBRepoIdentity(ref)
		if identity.PlatformRepoID == "" {
			continue
		}
		entry, _, err := l.db.ReconcileRepositoryObservation(
			ctx, identity, time.Now().UTC(),
		)
		if err != nil {
			return nil, err
		}
		if err := l.db.ReconcileDiscoveryArchives(
			ctx, []int64{entry.Repository.ID}, time.Now().UTC(),
		); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func (watermarkAdvancingArchiveLifecycle) RetryAuthentication(
	context.Context, []platform.RepoRef,
) error {
	return nil
}

// TestSyncMRForRepoPersistsSettingsDespiteArchiveWatermarkAdvance covers the
// production wiring the plain replacement test misses: the archive lifecycle
// runs between identity reconciliation and the settings write and advances
// the watermark, so the sync's first settings commit is rejected as stale.
// The sync must re-resolve and persist instead of populating the replacement
// row while it still advertises the permissive schema defaults.
func TestSyncMRForRepoPersistsSettingsDespiteArchiveWatermarkAdvance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	_, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}, now.Add(-time.Hour))
	require.NoError(err)
	require.True(accepted)

	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
		RepoPath:     "group/project",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
			mergeRequests: []platform.MergeRequest{{
				Repo:           platformRepoRef(repo),
				PlatformID:     1001,
				Number:         7,
				URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
				Title:          "replacement MR",
				Author:         "ada",
				State:          "open",
				HeadBranch:     "feature",
				BaseBranch:     "main",
				CreatedAt:      now,
				UpdatedAt:      now,
				LastActivityAt: now,
			}},
		},
		repository: platform.Repository{
			Ref:                platformRepoRef(repo),
			PlatformExternalID: "gid://gitlab/Project/99",
			DefaultBranch:      "main",
			WebURL:             "https://gitlab.example.com/group/project",
			CloneURL:           "https://gitlab.example.com/group/project.git",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
			ViewerCanMerge: new(false),
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	syncer.archiveLifecycle = watermarkAdvancingArchiveLifecycle{db: d}

	require.NoError(syncer.syncMRForRepo(ctx, repo, 7, false, nil))

	entry, err := d.GetRepositoryByProviderID(
		ctx, "gitlab", "gitlab.example.com", "gid://gitlab/Project/99",
	)
	require.NoError(err)
	require.NotNil(entry, "direct sync must catalog the replacement repository")
	stored, err := d.GetRepoByID(ctx, entry.Repository.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.True(stored.AllowSquashMerge)
	assert.False(stored.AllowMergeCommit)
	assert.False(stored.AllowRebaseMerge)
	assert.False(stored.ViewerCanMerge)
}

// TestSyncMRForRepoErrsWhenSettingsObservationKeepsLosing covers the rejected
// observation branch of direct sync: when the catalog holds a newer
// observation than every attempt this sync makes, the provider snapshot is
// discarded and the repository row's merge settings remain unverified. The
// sync must fail — a nil snapshot from a rejected observation is not the same
// as a provider without repository reading — instead of syncing the item
// against potentially default merge availability.
func TestSyncMRForRepoErrsWhenSettingsObservationKeepsLosing(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	_, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}, time.Now().UTC().Add(time.Hour))
	require.NoError(err)
	require.True(accepted)

	repo := RepoRef{
		Platform:     platform.KindGitLab,
		PlatformHost: "gitlab.example.com",
		Owner:        "group",
		Name:         "project",
		RepoPath:     "group/project",
	}
	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
			mergeRequests: []platform.MergeRequest{{
				Repo:           platformRepoRef(repo),
				PlatformID:     1001,
				Number:         7,
				URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
				Title:          "stale observation MR",
				Author:         "ada",
				State:          "open",
				HeadBranch:     "feature",
				BaseBranch:     "main",
				CreatedAt:      now,
				UpdatedAt:      now,
				LastActivityAt: now,
			}},
		},
		repository: platform.Repository{
			Ref:                platformRepoRef(repo),
			PlatformExternalID: "gid://gitlab/Project/42",
			DefaultBranch:      "main",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
			ViewerCanMerge: new(false),
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)

	err = syncer.syncMRForRepo(ctx, repo, 7, false, nil)
	require.ErrorContains(err, "kept losing")
}

// TestSyncRepoErrsWhenSettingsObservationKeepsLosing pins periodic sync to
// the same contract as direct item sync: when every settings observation this
// sync makes loses to a newer catalog observation, the repository's merge
// settings remain unverified and indexing must not proceed as if they were
// committed.
func TestSyncRepoErrsWhenSettingsObservationKeepsLosing(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	_, accepted, err := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/42",
		Owner:          "group", Name: "project", RepoPath: "group/project",
	}, time.Now().UTC().Add(time.Hour))
	require.NoError(err)
	require.True(accepted)

	provider := &syncTestRepositoryReadProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitLab, host: "gitlab.example.com",
		},
		repository: platform.Repository{
			Ref: platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.example.com",
				Owner: "group", Name: "project", RepoPath: "group/project",
			},
			PlatformExternalID: "gid://gitlab/Project/42",
			DefaultBranch:      "main",
			MergeSettings: &platform.RepositoryMergeSettings{
				AllowSquashMerge: true,
			},
			ViewerCanMerge: new(false),
		},
	}
	syncer := NewSyncerWithRegistry(
		mustRegistry(t, provider), d, nil, []RepoRef{{
			Platform:     platform.KindGitLab,
			PlatformHost: "gitlab.example.com",
			Owner:        "group",
			Name:         "project",
			RepoPath:     "group/project",
		}}, time.Minute, nil, nil,
	)

	err = syncer.syncRepo(ctx, syncer.repos[0])
	require.ErrorContains(err, "kept losing")

	// The aborted attempt must reach the repository row's sync health, or
	// the UI keeps reporting the previous outcome as current.
	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	require.Contains(repos[0].LastSyncError, "kept losing")
	require.NotNil(repos[0].LastSyncStartedAt)
}
