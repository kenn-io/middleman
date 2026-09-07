package workspaceapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
)

type pushedHeadProviderClient struct {
	ghclient.Client
	getPullRequest       func(context.Context, string, string, int) (*gh.PullRequest, error)
	getRepository        func(context.Context, string, string) (*gh.Repository, error)
	beforeCombinedStatus func()
	ciCalls              atomic.Int64
}

func (c *pushedHeadProviderClient) GetRepository(
	ctx context.Context, owner, name string,
) (*gh.Repository, error) {
	if c.getRepository != nil {
		return c.getRepository(ctx, owner, name)
	}
	return c.Client.GetRepository(ctx, owner, name)
}

func (c *pushedHeadProviderClient) GetPullRequest(
	ctx context.Context, owner, name string, number int,
) (*gh.PullRequest, error) {
	return c.getPullRequest(ctx, owner, name, number)
}

func (c *pushedHeadProviderClient) GetCombinedStatus(
	ctx context.Context, owner, name, ref string,
) (*gh.CombinedStatus, error) {
	c.ciCalls.Add(1)
	if c.beforeCombinedStatus != nil {
		c.beforeCombinedStatus()
	}
	return c.Client.GetCombinedStatus(ctx, owner, name, ref)
}

type pushedHeadIntegrationFixture struct {
	database *db.DB
	handler  *Handler
	events   []Event
	jobs     []func()
}

func newPushedHeadIntegrationFixture(
	t *testing.T,
	provider *pushedHeadProviderClient,
) *pushedHeadIntegrationFixture {
	t.Helper()
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": provider},
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		time.Minute,
		nil,
		nil,
	)
	t.Cleanup(syncer.Stop)
	fixture := &pushedHeadIntegrationFixture{database: database}
	manager := workspace.NewManager(database, t.TempDir())
	fixture.handler = New(Deps{
		DB: database, Workspaces: manager, Syncer: syncer,
		Broadcast: func(event Event) uint64 {
			fixture.events = append(fixture.events, event)
			return uint64(len(fixture.events))
		},
		EnqueueDetailSync: func(
			_ string,
			_ []any,
			run func(context.Context) error,
			onSuccess func(context.Context),
		) bool {
			fixture.jobs = append(fixture.jobs, func() {
				ctx := t.Context()
				if err := run(ctx); err == nil {
					onSuccess(ctx)
				}
			})
			return true
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, fixture.handler.Shutdown(ctx))
	})
	return fixture
}

func newPushedHeadProvider(
	getPullRequest func(context.Context, string, string, int) (*gh.PullRequest, error),
) *pushedHeadProviderClient {
	return &pushedHeadProviderClient{
		Client:         testutil.NewFixtureClient(),
		getPullRequest: getPullRequest,
	}
}

func TestWorkspacePushedHeadPassRefreshesProviderAndPublishesWireEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var newHead string
	newTitle := "Updated after push"
	provider := newPushedHeadProvider(func(
		_ context.Context, owner, name string, number int,
	) (*gh.PullRequest, error) {
		require.Equal("acme", owner)
		require.Equal("widget", name)
		require.Equal(1, number)
		return pushedHeadPullRequest(newTitle, newHead), nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, oldHead := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, oldHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	newHead = pushPushedHeadIntegrationCommit(t, worktreePath)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())

	require.Len(fixture.events, 2)
	assert.Equal("workspace_pushed_head_changed", fixture.events[0].Type)
	assert.Equal("workspace_pr_refresh_queued", fixture.events[1].Type)
	require.Len(fixture.jobs, 1)
	payloadJSON, err := json.Marshal(fixture.events[0].Data)
	require.NoError(err)
	var payload WorkspacePushedHeadChangedPayload
	require.NoError(json.Unmarshal(payloadJSON, &payload))
	assert.Equal("ws-pr", payload.WorkspaceID)
	assert.Equal("github", payload.Provider)
	assert.Equal("github.com", payload.PlatformHost)
	assert.Equal("acme/widget", payload.RepoPath)
	assert.Equal(oldHead, payload.OldSHA)
	assert.Equal(newHead, payload.NewSHA)

	fixture.jobs[0]()
	require.Len(fixture.events, 3)
	assert.Equal("pr_detail_refreshed", fixture.events[2].Type)
	stored, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(newTitle, stored.Title)
	assert.Equal(newHead, stored.PlatformHeadSHA)
}

func TestWorkspacePushedHeadPassIgnoresLocalOnlyCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var detailSyncCalls atomic.Int64
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		detailSyncCalls.Add(1)
		return nil, nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, pushedHead := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, pushedHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "feature.txt"), []byte("local only\n"), 0o644,
	))
	runGit(t, worktreePath, "add", ".")
	runGit(t, worktreePath, "commit", "-m", "local-only commit")

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())

	assert.Empty(fixture.events)
	assert.Empty(fixture.jobs)
	assert.Zero(detailSyncCalls.Load())
	stored, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("Old title", stored.Title)
	assert.Equal(pushedHead, stored.PlatformHeadSHA)
}

func TestWorkspacePushedHeadPassStopsAfterNonConvergingRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	providerHead := "e2e0000000000000000000000000000000000000"
	var detailSyncCalls atomic.Int64
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		detailSyncCalls.Add(1)
		return pushedHeadPullRequest("Moved elsewhere", providerHead), nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, _ := setupPushedHeadIntegrationWorktree(t)
	seedPushedHeadIntegrationPR(t, fixture.database, providerHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())
	require.Len(fixture.jobs, 1)
	fixture.jobs[0]()
	require.Equal(int64(1), detailSyncCalls.Load())
	eventsAfterRefresh := len(fixture.events)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())
	assert.Len(fixture.events, eventsAfterRefresh)
	assert.Len(fixture.jobs, 1)
	assert.Equal(int64(1), detailSyncCalls.Load())

	pushPushedHeadIntegrationCommit(t, worktreePath)
	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())
	require.Len(fixture.jobs, 2)
	assert.Equal("workspace_pushed_head_changed", fixture.events[eventsAfterRefresh].Type)
	assert.Equal("workspace_pr_refresh_queued", fixture.events[eventsAfterRefresh+1].Type)
	fixture.jobs[1]()
	assert.Equal(int64(2), detailSyncCalls.Load())
}

func TestWorkspacePushedHeadPassIgnoresRemovedPullRequest(t *testing.T) {
	require := require.New(t)
	var detailSyncCalls atomic.Int64
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		detailSyncCalls.Add(1)
		return nil, nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, oldHead := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, oldHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	pushPushedHeadIntegrationCommit(t, worktreePath)
	markPushedHeadIntegrationPRRemoved(t, fixture.database, repoID)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())

	require.Empty(fixture.events)
	require.Empty(fixture.jobs)
	require.Zero(detailSyncCalls.Load())
	require.Zero(provider.ciCalls.Load())
}

func TestWorkspacePushedHeadQueuedRefreshRechecksRemovedPullRequest(t *testing.T) {
	require := require.New(t)
	var detailSyncCalls atomic.Int64
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		detailSyncCalls.Add(1)
		return nil, nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, oldHead := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, oldHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	pushPushedHeadIntegrationCommit(t, worktreePath)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())
	require.Len(fixture.jobs, 1)
	markPushedHeadIntegrationPRRemoved(t, fixture.database, repoID)
	fixture.jobs[0]()

	require.Zero(detailSyncCalls.Load())
	require.Zero(provider.ciCalls.Load())
	require.Len(fixture.events, 2, "removed pull must not publish refresh success")
}

func TestWorkspacePushedHeadQueuedCIRefreshRechecksRemovedPullRequest(t *testing.T) {
	require := require.New(t)
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		return nil, nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, headSHA := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, headSHA)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	require.NoError(fixture.database.UpdateMRCIStatusForHead(
		t.Context(), repoID, 1, headSHA, "pending", `[]`, true,
	))
	change := workspace.PushedHeadUpdate{
		WorkspaceID: "ws-pr", RepoID: repoID, Provider: platform.KindGitHub,
		PlatformHost: "github.com", RepoPath: "acme/widget",
		Owner: "acme", Name: "widget", Number: 1, NewSHA: headSHA,
	}

	fixture.handler.maybeEnqueuePushedHeadCIRefresh(t.Context(), change)
	require.Len(fixture.jobs, 1)
	markPushedHeadIntegrationPRRemoved(t, fixture.database, repoID)
	fixture.jobs[0]()

	require.Zero(provider.ciCalls.Load())
	require.Len(fixture.events, 1, "removed pull must not publish CI refresh success")
}

func TestWorkspacePushedHeadQueuedCIRefreshRejectsRouteReuseDuringFetch(t *testing.T) {
	require := require.New(t)
	provider := newPushedHeadProvider(nil)
	fixture := newPushedHeadIntegrationFixture(t, provider)
	headSHA := "old-head"
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, headSHA)
	require.NoError(fixture.database.UpdateMRCIStatusForHead(
		t.Context(), repoID, 1, headSHA, "pending", `[]`, true,
	))
	change := workspace.PushedHeadUpdate{
		WorkspaceID: "ws-pr", RepoID: repoID, Provider: platform.KindGitHub,
		PlatformHost: "github.com", RepoPath: "acme/widget",
		Owner: "acme", Name: "widget", Number: 1, NewSHA: headSHA,
	}

	fixture.handler.maybeEnqueuePushedHeadCIRefresh(t.Context(), change)
	require.Len(fixture.jobs, 1)
	provider.beforeCombinedStatus = func() {
		replacementIdentity := db.GitHubRepoIdentity("github.com", "acme", "widget")
		replacementIdentity.PlatformRepoID = "repo-acme-widget-replacement"
		_, _, err := fixture.database.ReconcileRepositoryObservation(
			context.Background(), replacementIdentity, time.Now().UTC().Add(time.Hour),
		)
		require.NoError(err)
	}

	fixture.jobs[0]()

	require.Equal(int64(1), provider.ciCalls.Load())
	stored, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(stored)
	require.Equal("pending", stored.CIStatus)
	require.True(stored.CIHadPending)
}

func TestLookupPushedHeadMRDoesNotFollowReusedRepositoryRoute(t *testing.T) {
	require := require.New(t)
	fixture := newPushedHeadIntegrationFixture(t, newPushedHeadProvider(nil))
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, "old-head")
	replacementIdentity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	replacementIdentity.PlatformRepoID = "repo-acme-widget-replacement"
	replacement, _, err := fixture.database.ReconcileRepositoryObservation(
		t.Context(), replacementIdentity, time.Now().UTC().Add(time.Hour),
	)
	require.NoError(err)
	require.NotNil(replacement)

	repo, mr := fixture.handler.lookupPushedHeadMR(t.Context(), workspace.PushedHeadUpdate{
		WorkspaceID: "ws-pr", RepoID: repoID, Provider: platform.KindGitHub,
		PlatformHost: "github.com", RepoPath: "acme/widget",
		Owner: "acme", Name: "widget", Number: 1,
	})
	require.Nil(repo)
	require.Nil(mr)
}

func TestWorkspacePushedHeadQueuedRefreshStopsWhenRouteIsReusedDuringSync(t *testing.T) {
	require := require.New(t)
	var detailSyncCalls atomic.Int64
	provider := newPushedHeadProvider(func(
		context.Context, string, string, int,
	) (*gh.PullRequest, error) {
		detailSyncCalls.Add(1)
		return pushedHeadPullRequest("replacement title", "replacement-head"), nil
	})
	fixture := newPushedHeadIntegrationFixture(t, provider)
	worktreePath, oldHead := setupPushedHeadIntegrationWorktree(t)
	repoID := seedPushedHeadIntegrationPR(t, fixture.database, oldHead)
	insertPushedHeadIntegrationWorkspace(t, fixture.database, worktreePath)
	pushPushedHeadIntegrationCommit(t, worktreePath)

	fixture.handler.runWorkspacePushedHeadObserverPass(t.Context())
	require.Len(fixture.jobs, 1)
	provider.getRepository = func(
		ctx context.Context, owner, name string,
	) (*gh.Repository, error) {
		replacementIdentity := db.GitHubRepoIdentity("github.com", owner, name)
		replacementIdentity.PlatformRepoID = "repo-acme-widget-replacement"
		_, _, err := fixture.database.ReconcileRepositoryObservation(
			ctx, replacementIdentity, time.Now().UTC().Add(time.Hour),
		)
		require.NoError(err)
		allowed := true
		nodeID := replacementIdentity.PlatformRepoID
		return &gh.Repository{
			Name: &name, NodeID: &nodeID, Owner: &gh.User{Login: &owner},
			AllowSquashMerge: &allowed, AllowMergeCommit: &allowed,
			AllowRebaseMerge: &allowed,
		}, nil
	}

	fixture.jobs[0]()

	require.Zero(detailSyncCalls.Load())
	stored, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(stored)
	require.Equal("Old title", stored.Title)
	require.Equal(oldHead, stored.PlatformHeadSHA)
}

func pushedHeadPullRequest(title, headSHA string) *gh.PullRequest {
	state := "open"
	body := "updated body"
	url := "https://github.com/acme/widget/pull/1"
	now := gh.Timestamp{Time: time.Date(2026, 5, 20, 14, 15, 0, 0, time.UTC)}
	cloneURL := "https://github.com/acme/widget.git"
	fullName := "acme/widget"
	return &gh.PullRequest{
		ID:        new(int64(1001)),
		Number:    new(1),
		Title:     &title,
		Body:      &body,
		State:     &state,
		HTMLURL:   &url,
		User:      &gh.User{Login: new("octocat")},
		CreatedAt: &now,
		UpdatedAt: &now,
		Head: &gh.PullRequestBranch{
			Ref: new("feature"), SHA: &headSHA,
			Repo: &gh.Repository{CloneURL: &cloneURL, FullName: &fullName},
		},
		Base: &gh.PullRequestBranch{Ref: new("main"), SHA: new("base-sha")},
	}
}

func seedPushedHeadIntegrationPR(t *testing.T, database *db.DB, oldHead string) int64 {
	t.Helper()
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(
		t.Context(), identity,
	)
	require.NoError(t, err)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 1001, Number: 1,
		URL: "https://github.com/acme/widget/pull/1", Title: "Old title",
		Author: "octocat", State: db.MergeRequestStateOpen, Body: "old body",
		HeadBranch: "feature", BaseBranch: "main", PlatformHeadSHA: oldHead,
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	return repoID
}

func setupPushedHeadIntegrationWorktree(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	worktree := filepath.Join(dir, "worktree")
	runGit(t, dir, "init", "--bare", "--initial-branch=main", remote)
	runGit(t, dir, "clone", remote, worktree)
	runGit(t, worktree, "config", "user.email", "test@test.com")
	runGit(t, worktree, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "base.txt"), []byte("base\n"), 0o644))
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "base commit")
	runGit(t, worktree, "push", "origin", "main")
	runGit(t, worktree, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("first\n"), 0o644))
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "feature commit")
	runGit(t, worktree, "push", "-u", "origin", "feature")
	return worktree, gitSHA(t, worktree, "refs/remotes/origin/feature")
}

func pushPushedHeadIntegrationCommit(t *testing.T, worktree string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("second\n"), 0o644))
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "second feature commit")
	runGit(t, worktree, "push", "origin", "feature")
	return gitSHA(t, worktree, "refs/remotes/origin/feature")
}

func gitSHA(t *testing.T, dir, ref string) string {
	t.Helper()
	return runGitOutput(t, dir, "rev-parse", ref)
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitcmd.New().Output(t.Context(), dir, args...)
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func insertPushedHeadIntegrationWorkspace(
	t *testing.T, database *db.DB, worktreePath string,
) {
	t.Helper()
	require.NoError(t, database.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "ws-pr", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1,
		GitHeadRef: "feature", WorkspaceBranch: "feature",
		WorktreePath: worktreePath, TmuxSession: "kenn-forge-ws-pr", Status: "ready",
	}))
	require.NoError(t, database.PutWorkspaceLaunchSpec(
		t.Context(), "ws-pr", workspaceLaunchSpecForRequest(
			providerplane.WorkspaceLaunchRequest{
				Repository: providerplane.RepositoryRoute{
					Provider: "github", PlatformHost: "github.com",
					Owner: "acme", Name: "widget",
				},
				ItemType:   db.WorkspaceItemTypePullRequest,
				ItemNumber: 1, ItemKey: "1", GitHeadRef: "feature",
			},
			time.Now().UTC(),
		),
	))
}

func markPushedHeadIntegrationPRRemoved(
	t *testing.T, database *db.DB, repoID int64,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', 1, 'pull-1', ?, ?, 'removed_upstream')`,
		repoID, now, now,
	)
	require.NoError(t, err)
}
