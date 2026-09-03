package workspacetest

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func workspaceGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitcmd.New().Output(t.Context(), dir, args...)
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func seedIssueOnHost(
	t *testing.T, database *db.DB, host, owner, name string, number int,
	state, title string,
) int64 {
	t.Helper()
	repoID, err := database.UpsertRepo(
		t.Context(), db.GitHubRepoIdentity(host, owner, name),
	)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	issue := &db.Issue{
		RepoID: repoID, PlatformID: int64(number) * 1000, Number: number,
		URL:   "https://" + host + "/" + owner + "/" + name + "/issues/" + strconv.Itoa(number),
		Title: title, Author: "testuser", State: state,
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}
	issueID, err := database.UpsertIssue(t.Context(), issue)
	require.NoError(t, err)
	return issueID
}

func TestWorkspaceCRUDE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client := fixture.client
	ctx := t.Context()

	// 1. List workspaces -- initially empty.
	listResp, err := client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp.StatusCode())
	require.NotNil(listResp.JSON200)
	require.NotNil(listResp.JSON200.Workspaces)
	assert.Empty(listResp.JSON200.Workspaces)

	// 2. Create workspace.
	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	wsID := createResp.JSON202.Id
	assert.NotEmpty(wsID)
	assert.Equal("github.com", createResp.JSON202.PlatformHost)
	assert.Equal("acme", createResp.JSON202.RepoOwner)
	assert.Equal("widget", createResp.JSON202.RepoName)
	assert.Equal(db.WorkspaceItemTypePullRequest, createResp.JSON202.ItemType)
	assert.Equal(int64(1), createResp.JSON202.ItemNumber)

	// Wait for the async clone to finish before exercising the rest of the
	// flow. Deleting (or letting the test end) while the clone subprocess is
	// still writing into the workspace's TempDir races t.TempDir cleanup,
	// which then fails with "directory not empty".
	waitForWorkspaceReady(t, ctx, client, wsID)

	// 3. Get workspace by ID.
	getResp, err := client.HTTP.GetWorkspaceWithResponse(
		ctx, wsID,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, getResp.StatusCode())
	require.NotNil(getResp.JSON200)
	assert.Equal(wsID, getResp.JSON200.Id)

	// 4. List workspaces -- now has one.
	listResp2, err := client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp2.StatusCode())
	require.NotNil(listResp2.JSON200)
	require.NotNil(listResp2.JSON200.Workspaces)
	assert.Len(listResp2.JSON200.Workspaces, 1)

	// 5. Delete workspace (force).
	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, delResp.StatusCode())

	// 6. Verify deleted -- GET returns 404.
	getResp2, err := client.HTTP.GetWorkspaceWithResponse(
		ctx, wsID,
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, getResp2.StatusCode())

	// 7. List workspaces -- deleted workspace is absent from the public list.
	listResp3, err := client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp3.StatusCode())
	require.NotNil(listResp3.JSON200)
	require.NotNil(listResp3.JSON200.Workspaces)
	assert.Empty(listResp3.JSON200.Workspaces)
}

func TestWorkspaceRetryErroredWorkspaceE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, database := fixture.client, fixture.database
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	wsID := createResp.JSON202.Id
	waitForWorkspaceReady(t, ctx, client, wsID)

	msg := "ensure clone: git fetch: fork/exec /opt/homebrew/bin/git: resource temporarily unavailable"
	err = database.UpdateWorkspaceStatus(ctx, wsID, "error", &msg)
	require.NoError(err)

	retryResp, err := client.HTTP.RetryWorkspaceWithResponse(ctx, wsID)
	require.NoError(err)
	require.Equal(http.StatusAccepted, retryResp.StatusCode())
	require.NotNil(retryResp.JSON202)
	retryBody := retryResp.JSON202
	assert.Equal(wsID, retryBody.Id)
	assert.Equal("creating", retryBody.Status)
	assert.Nil(retryBody.ErrorMessage)

	ready := waitForWorkspaceReady(t, ctx, client, wsID)
	assert.Equal(wsID, ready.Id)
	assert.Nil(ready.ErrorMessage)
}

func TestWorkspaceRetryReadyWorkspaceConflictE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, database := fixture.client, fixture.database
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	wsID := createResp.JSON202.Id

	waitForWorkspaceReady(t, ctx, client, wsID)
	before, err := database.GetWorkspace(ctx, wsID)
	require.NoError(err)
	require.NotNil(before)
	require.Equal("ready", before.Status)
	require.Nil(before.ErrorMessage)
	require.NotEmpty(before.WorktreePath)
	beforeEvents, err := database.ListWorkspaceSetupEvents(ctx, wsID)
	require.NoError(err)

	retryResp, err := client.HTTP.RetryWorkspaceWithResponse(ctx, wsID)
	require.NoError(err)
	require.Equal(http.StatusConflict, retryResp.StatusCode())

	after, err := database.GetWorkspace(ctx, wsID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal("ready", after.Status)
	assert.Nil(after.ErrorMessage)
	assert.Equal(before.WorktreePath, after.WorktreePath)
	assert.Equal(before.WorkspaceBranch, after.WorkspaceBranch)

	afterEvents, err := database.ListWorkspaceSetupEvents(ctx, wsID)
	require.NoError(err)
	assert.Len(afterEvents, len(beforeEvents))
}

// TestWorkspaceReadyStatusImpliesReadySetupEventE2E pins the write order
// in Manager.Setup: the final "setup ready" event must be recorded before
// status flips to "ready". When the order was reversed, pollers that
// reacted to status=ready could read a setup-event list still missing its
// last row, which made retry-conflict event-count assertions flake on CI.
func TestWorkspaceReadyStatusImpliesReadySetupEventE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, database := fixture.client, fixture.database
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	wsID := createResp.JSON202.Id

	// Read the event log immediately after the first observation of
	// status=ready: the ready event must already be there.
	waitForWorkspaceReady(t, ctx, client, wsID)
	events, err := database.ListWorkspaceSetupEvents(ctx, wsID)
	require.NoError(err)
	sawReadyEvent := false
	for _, event := range events {
		if event.Stage == "setup" && event.Outcome == "ready" {
			sawReadyEvent = true
		}
	}
	require.True(
		sawReadyEvent,
		"workspace reported status=ready before the setup ready "+
			"event was recorded; events: %d", len(events),
	)
}

func TestWorkspaceCreateNotFound(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client := fixture.client
	ctx := t.Context()

	// Non-existent repo.
	resp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "nope",
			Name:         "missing",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, resp.StatusCode())

	// Existing repo, non-existent MR.
	resp2, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     999,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, resp2.StatusCode())
}

func TestWorkspaceCreateHidesRemovedUpstreamItems(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	seedIssueOnHost(t, fixture.database, "github.com", "acme", "widget", 1, "open", "Test Issue")
	repo, err := fixture.database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = fixture.database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 1, 'pr-1', ?, ?, 'removed_upstream'),
			(?, 'issue', 1, 'issue-1', ?, ?, 'removed_upstream')`,
		repo.ID, now, now, repo.ID, now, now,
	)
	require.NoError(err)

	prResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx, generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, prResp.StatusCode(), string(prResp.Body))

	issueResp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		ctx, "gh", "acme", "widget", 1,
		generated.CreateIssueWorkspaceInputBody{},
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, issueResp.StatusCode(), string(issueResp.Body))

	listed, err := fixture.client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listed.StatusCode())
	require.NotNil(listed.JSON200)
	require.NotNil(listed.JSON200.Workspaces)
	require.Empty(listed.JSON200.Workspaces)
}

func TestWorkspaceListRetainsWorkspaceWithoutRemovedPullMetadata(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	repo, err := fixture.database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	require.NoError(fixture.database.InsertWorkspace(ctx, &db.Workspace{
		ID: "ws-removed-pr", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1,
		GitHeadRef: "feature", WorktreePath: fixture.worktreeDir + "/removed-pr",
		TmuxSession: "forge-ws-removed-pr", Status: "creating",
	}))
	now := time.Now().UTC().Truncate(time.Second)
	_, err = fixture.database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', 1, 'pull-1', ?, ?, 'removed_upstream')`,
		repo.ID, now, now,
	)
	require.NoError(err)

	listed, err := fixture.client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listed.StatusCode(), string(listed.Body))
	require.NotNil(listed.JSON200)
	require.NotNil(listed.JSON200.Workspaces)
	require.Len(listed.JSON200.Workspaces, 1)
	workspace := listed.JSON200.Workspaces[0]
	require.Equal("ws-removed-pr", workspace.Id)
	require.Equal(int64(1), workspace.ItemNumber)
	require.Nil(workspace.MrTitle)
	require.Nil(workspace.MrState)
	require.Nil(workspace.MrIsDraft)
	require.Nil(workspace.MrCiStatus)
	require.Nil(workspace.MrReviewDecision)
	require.Nil(workspace.MrAdditions)
	require.Nil(workspace.MrDeletions)
	require.Nil(workspace.ItemLastActivityAt)
}

func TestWorkspaceMRDetailHasWorkspace(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client := fixture.client
	ctx := t.Context()

	// Create a workspace for PR #1.
	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	wsID := createResp.JSON202.Id

	// MR detail should include the workspace reference.
	mrResp, err := client.HTTP.GetPullWithResponse(
		ctx, "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, mrResp.StatusCode())
	require.NotNil(mrResp.JSON200)
	require.NotNil(mrResp.JSON200.Workspace)
	assert.Equal(wsID, mrResp.JSON200.Workspace.Id)
	assert.NotEmpty(mrResp.JSON200.Workspace.Status)

	waitForWorkspaceReady(t, ctx, client, wsID)

	// Clean up: delete the workspace.
	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, delResp.StatusCode())
}

func TestWorkspaceCreateDuplicate(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client := fixture.client
	ctx := t.Context()

	body := generated.CreateWorkspaceInputBody{
		Provider:     "github",
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
		MrNumber:     1,
	}

	// First create succeeds.
	resp1, err := client.HTTP.CreateWorkspaceWithResponse(ctx, body)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp1.StatusCode())
	require.NotNil(resp1.JSON202)

	// Duplicate create returns 409.
	resp2, err := client.HTTP.CreateWorkspaceWithResponse(ctx, body)
	require.NoError(err)
	require.Equal(http.StatusConflict, resp2.StatusCode())

	// Drain the first create's async setup before the test returns. Otherwise
	// the background clone can keep writing into the bare-clone temp dir and
	// race t.TempDir cleanup, which fails RemoveAll with "directory not empty".
	waitForWorkspaceReady(t, ctx, client, resp1.JSON202.Id)
}

func TestWorkspaceCreateFetchesCloneThroughAPI(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	remoteWork := filepath.Join(t.TempDir(), "remote-work")
	gitfixture.Run(t, t.TempDir(), "clone", fixture.remote, remoteWork)
	gitfixture.Run(t, remoteWork, "config", "user.email", "test@test.com")
	gitfixture.Run(t, remoteWork, "config", "user.name", "Test")
	gitfixture.Run(t, remoteWork, "checkout", "feature")
	require.NoError(os.WriteFile(
		filepath.Join(remoteWork, "after-fetch.txt"),
		[]byte("fetched through workspace API\n"),
		0o644,
	))
	gitfixture.Run(t, remoteWork, "add", ".")
	gitfixture.Run(t, remoteWork, "commit", "-m", "feature after fixture clone")
	gitfixture.Run(t, remoteWork, "push", "origin", "feature")
	featureSHA := workspaceGitOutput(t, remoteWork, "rev-parse", "HEAD")
	gitfixture.Run(t, fixture.remote, "update-ref", "refs/pull/1/head", featureSHA)

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, createResp.JSON202.Id)
	assert.Equal("ready", ready.Status)
	assert.FileExists(filepath.Join(ready.WorktreePath, "after-fetch.txt"))
}

func TestWorkspaceCreateIssueE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()

	seedIssueOnHost(t, fixture.database, "github.com", "acme", "widget", 7, "open", "Test Issue")

	createRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodPost,
		"/api/v1/issues/gh/acme/widget/7/workspace",
		map[string]string{},
	)
	require.Equal(http.StatusAccepted, createRR.Code, createRR.Body.String())

	var created generated.WorkspaceResponse
	require.NoError(json.NewDecoder(createRR.Body).Decode(&created))
	require.NotEmpty(created.Id)
	assert.Equal("issue", created.ItemType)
	assert.Equal(int64(7), created.ItemNumber)
	// seedIssue uses title "Test Issue" → slug style appends "-test-issue".
	assert.Equal("kenn-forge/issue-7-test-issue", created.GitHeadRef)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, created.Id)
	assert.Equal(
		"kenn-forge/issue-7-test-issue",
		workspaceGitOutput(t, ready.WorktreePath, "branch", "--show-current"),
	)
	assert.Equal(
		gitfixture.SHA(t, fixture.remote, "refs/heads/main"),
		gitfixture.SHA(t, ready.WorktreePath, "HEAD"),
	)

	getIssueRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodGet,
		"/api/v1/issues/gh/acme/widget/7",
		nil,
	)
	require.Equal(http.StatusOK, getIssueRR.Code, getIssueRR.Body.String())

	var issueDetail generated.IssueDetailResponse
	require.NoError(json.NewDecoder(getIssueRR.Body).Decode(&issueDetail))
	require.NotNil(issueDetail.Workspace)
	assert.Equal(created.Id, issueDetail.Workspace.Id)
	assert.NotEmpty(issueDetail.Workspace.Status)
}

func TestWorkspaceCreateIssueUsesTitleSlugInBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()

	// Replace the seed title with a multi-word issue title to make
	// sure the slug appears in the issue-workspace branch name.
	seedIssueOnHost(
		t, fixture.database, "github.com", "acme", "widget", 8,
		"open", "Add foo to bar",
	)

	createRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodPost,
		"/api/v1/issues/gh/acme/widget/8/workspace",
		map[string]string{},
	)
	require.Equal(http.StatusAccepted, createRR.Code, createRR.Body.String())

	var created generated.WorkspaceResponse
	require.NoError(json.NewDecoder(createRR.Body).Decode(&created))
	assert.Equal("kenn-forge/issue-8-add-foo-to-bar", created.GitHeadRef)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, created.Id)
	assert.Equal(
		"kenn-forge/issue-8-add-foo-to-bar",
		workspaceGitOutput(t, ready.WorktreePath, "branch", "--show-current"),
	)
}

func TestWorkspaceCreateIssueBareStyleConfigOptOut(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	cfg := &config.Config{
		IssueWorkspaceBranchStyle: config.IssueWorkspaceBranchStyleBare,
	}
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := context.Background()

	seedIssueOnHost(
		t, fixture.database, "github.com", "acme", "widget", 9,
		"open", "Add foo to bar",
	)

	createRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodPost,
		"/api/v1/issues/gh/acme/widget/9/workspace",
		map[string]string{},
	)
	require.Equal(http.StatusAccepted, createRR.Code, createRR.Body.String())

	var created generated.WorkspaceResponse
	require.NoError(json.NewDecoder(createRR.Body).Decode(&created))
	assert.Equal("kenn-forge/issue-9", created.GitHeadRef)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, created.Id)
	assert.Equal(
		"kenn-forge/issue-9",
		workspaceGitOutput(t, ready.WorktreePath, "branch", "--show-current"),
	)
}

func TestWorkspaceCreateIssueIsIdempotent(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()
	seedIssueOnHost(t, fixture.database, "github.com", "acme", "widget", 7, "open", "Test Issue")

	path := "/api/v1/issues/gh/acme/widget/7/workspace"

	firstRR := testutil.DoJSON(
		t, fixture.server, http.MethodPost, path, map[string]string{},
	)
	require.Equal(http.StatusAccepted, firstRR.Code, firstRR.Body.String())

	var first generated.WorkspaceResponse
	require.NoError(json.NewDecoder(firstRR.Body).Decode(&first))
	require.NotEmpty(first.Id)
	require.NotNil(first.Created)
	assert.True(*first.Created, "the fresh create response must mark itself as newly created")

	secondRR := testutil.DoJSON(
		t, fixture.server, http.MethodPost, path, map[string]string{},
	)
	require.Equal(http.StatusAccepted, secondRR.Code, secondRR.Body.String())

	var second generated.WorkspaceResponse
	require.NoError(json.NewDecoder(secondRR.Body).Decode(&second))
	assert.Equal(first.Id, second.Id)
	assert.Equal("issue", second.ItemType)
	assert.Equal(int64(7), second.ItemNumber)
	assert.Nil(second.Created, "a reused existing workspace must not be marked as newly created")

	waitForWorkspaceReady(t, ctx, fixture.client, second.Id)
}

func TestWorkspaceCreateIssueAfterDeleteRecreatesBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()

	seedIssueOnHost(t, fixture.database, "github.com", "acme", "widget", 7, "open", "Test Issue")

	createRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodPost,
		"/api/v1/issues/gh/acme/widget/7/workspace",
		map[string]string{},
	)
	require.Equal(http.StatusAccepted, createRR.Code, createRR.Body.String())

	var created generated.WorkspaceResponse
	require.NoError(json.NewDecoder(createRR.Body).Decode(&created))
	ready := waitForWorkspaceReady(t, ctx, fixture.client, created.Id)
	assert.Equal(
		"kenn-forge/issue-7-test-issue",
		workspaceGitOutput(t, ready.WorktreePath, "branch", "--show-current"),
	)

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx,
		created.Id,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, deleteResp.StatusCode())

	recreateRR := testutil.DoJSON(
		t,
		fixture.server,
		http.MethodPost,
		"/api/v1/issues/gh/acme/widget/7/workspace",
		map[string]string{},
	)
	require.Equal(http.StatusAccepted, recreateRR.Code, recreateRR.Body.String())

	var recreated generated.WorkspaceResponse
	require.NoError(json.NewDecoder(recreateRR.Body).Decode(&recreated))
	recreatedReady := waitForWorkspaceReady(t, ctx, fixture.client, recreated.Id)
	assert.Equal(
		"kenn-forge/issue-7-test-issue",
		workspaceGitOutput(t, recreatedReady.WorktreePath, "branch", "--show-current"),
	)
}

func TestWorkspaceCreatePRAndIssueCanCoexistForSameRepoNumber(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()

	seedIssueOnHost(t, fixture.database, "github.com", "acme", "widget", 1, "open", "Test Issue")

	prResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, prResp.StatusCode())
	require.NotNil(prResp.JSON202)
	assert.Equal("pull_request", prResp.JSON202.ItemType)
	assert.Equal(int64(1), prResp.JSON202.ItemNumber)

	issueResp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		ctx,
		"gh",
		"acme",
		"widget",
		1,
		generated.CreateIssueWorkspaceInputBody{},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, issueResp.StatusCode())
	require.NotNil(issueResp.JSON202)
	assert.Equal("issue", issueResp.JSON202.ItemType)
	assert.Equal(int64(1), issueResp.JSON202.ItemNumber)
	assert.NotEqual(prResp.JSON202.Id, issueResp.JSON202.Id)

	listResp, err := fixture.client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp.StatusCode())
	require.NotNil(listResp.JSON200)
	require.NotNil(listResp.JSON200.Workspaces)
	require.Len(listResp.JSON200.Workspaces, 2)
}
