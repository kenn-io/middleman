package workspacetest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestWorkspaceCreateUsesPRBranchAndFallbackBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	seedPROnHost(t, fixture.database, "github.com", "acme", "widget", 2)

	create := func(number int64) *generated.WorkspaceResponse {
		resp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
			t.Context(), generated.CreateWorkspaceInputBody{
				Provider: "github", PlatformHost: "github.com",
				Owner: "acme", Name: "widget", MrNumber: number,
			},
		)
		require.NoError(err)
		require.Equal(http.StatusAccepted, resp.StatusCode())
		require.NotNil(resp.JSON202)
		return waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	}

	tracked := create(1)
	assert.Equal("feature", gitOutputForLifecycle(t, tracked.WorktreePath, "branch", "--show-current"))
	assert.Equal("origin", gitOutputForLifecycle(t, tracked.WorktreePath, "config", "--get", "branch.feature.remote"))
	gitfixture.Run(t, fixture.bare, "fetch", "--prune", "origin")

	fallback := create(2)
	assert.Equal("kenn-forge/pr-2", gitOutputForLifecycle(t, fallback.WorktreePath, "branch", "--show-current"))
	assert.Equal(gitfixture.SHA(t, tracked.WorktreePath, "HEAD"), gitfixture.SHA(t, fallback.WorktreePath, "HEAD"))
}

func TestWorkspaceDeleteRecreatesForkBranchName(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	repo, err := fixture.database.GetRepoByIdentity(
		t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	headSHA := gitfixture.SHA(t, fixture.remote, "feature")
	gitfixture.Run(t, fixture.remote, "update-ref", "refs/heads/fork-feature", headSHA)
	gitfixture.Run(
		t, fixture.bare, "config", "--add",
		"url."+fixture.remote+".insteadOf", "https://github.com/fork/widget.git",
	)
	now := time.Now().UTC().Truncate(time.Second)
	prID, err := fixture.database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repo.ID, PlatformID: 2000, Number: 2,
		URL: "https://github.com/acme/widget/pull/2", Title: "Fork PR #2",
		Author: "fork-user", State: "open", Body: "fork test body",
		HeadBranch: "fork-feature", BaseBranch: "main",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)
	require.NoError(fixture.database.EnsureKanbanState(t.Context(), prID))

	create := func() *generated.WorkspaceResponse {
		resp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
			t.Context(), generated.CreateWorkspaceInputBody{
				Provider: "github", PlatformHost: "github.com",
				Owner: "acme", Name: "widget", MrNumber: 2,
			},
		)
		require.NoError(err)
		require.Equal(http.StatusAccepted, resp.StatusCode())
		require.NotNil(resp.JSON202)
		return waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	}

	first := create()
	assert.Equal("fork-feature", gitOutputForLifecycle(t, first.WorktreePath, "branch", "--show-current"))
	force := true
	deleted, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		t.Context(), first.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, deleted.StatusCode())

	second := create()
	assert.Equal("fork-feature", gitOutputForLifecycle(t, second.WorktreePath, "branch", "--show-current"))
	assert.Equal(headSHA, gitfixture.SHA(t, second.WorktreePath, "HEAD"))
}

func gitOutputForLifecycle(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitcmd.New().Output(context.Background(), dir, args...)
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}
