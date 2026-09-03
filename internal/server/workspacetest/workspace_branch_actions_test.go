package workspacetest

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace"
)

func TestWorkspacePushBranchRoutePushesAheadBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)
	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "ahead.txt"), []byte("ahead\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "ahead")

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.Id+"/push", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	div, ok, err := workspace.WorktreeDivergence(ctx, ws.WorktreePath)
	require.NoError(err)
	require.True(ok)
	assert.Equal(workspace.Divergence{}, div)
}

func TestWorkspacePullBranchRouteFastForwardsBehindBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)
	gitfixture.Run(t, ws.WorktreePath, "push", "-u", "origin", "HEAD")
	upstreamRef := workspaceGitOutput(
		t, ws.WorktreePath,
		"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}",
	)
	upstreamBranch := strings.TrimPrefix(upstreamRef, "origin/")

	other := filepath.Join(t.TempDir(), "other")
	originURL := workspaceGitOutput(t, ws.WorktreePath, "remote", "get-url", "origin")
	gitfixture.Run(t, t.TempDir(), "clone", originURL, other)
	gitfixture.Run(t, other, "config", "user.email", "test@test.com")
	gitfixture.Run(t, other, "config", "user.name", "Test")
	gitfixture.Run(t, other, "checkout", "-b", upstreamBranch, upstreamRef)
	require.NoError(os.WriteFile(
		filepath.Join(other, "remote.txt"), []byte("remote\n"), 0o644,
	))
	gitfixture.Run(t, other, "add", ".")
	gitfixture.Run(t, other, "commit", "-m", "remote")
	gitfixture.Run(t, other, "push", "origin", upstreamBranch)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.Id+"/pull", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	contents, err := os.ReadFile(filepath.Join(ws.WorktreePath, "remote.txt"))
	require.NoError(err)
	assert.Equal("remote\n", string(contents))
}

func TestWorkspacePullBranchRouteRejectsDirtyWorktree(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0o644,
	))

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.Id+"/pull", nil)

	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.New(t).Equal(httpapi.CodeWorktreeDirty, problem.Code)
}
