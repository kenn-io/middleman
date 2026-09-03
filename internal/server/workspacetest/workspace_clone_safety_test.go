package workspacetest

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func setupLifecycleWorkspaceServer(t *testing.T) (*apiclient.Client, *db.DB, string, string) {
	t.Helper()
	fixture := setupWorkspaceServerFixture(t, nil)
	return fixture.client, fixture.database, fixture.bare, fixture.remote
}

func TestWorkspaceForceDeleteToleratesCorruptWorktreeGitfileE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	client, database, _, _ := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, client)

	gitfile := filepath.Join(ws.WorktreePath, ".git")
	require.FileExists(gitfile)
	require.NoError(os.Truncate(gitfile, 0))

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)
	stored, err := database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
}

// TestWorkspaceForceDeleteQuarantinesReplacedWorktreeAndAllowsRecreateE2E
// covers the user-visible recovery path when a workspace directory no longer
// contains the linked worktree registered by its managed clone.
func TestWorkspaceForceDeleteQuarantinesReplacedWorktreeAndAllowsRecreateE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	client, database, _, _ := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, client)
	worktreePath := ws.WorktreePath

	require.NoError(os.RemoveAll(worktreePath))
	require.NoError(os.MkdirAll(worktreePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "recover.txt"),
		[]byte("preserve me\n"),
		0o644,
	))

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, delResp.StatusCode(), string(delResp.Body),
	)

	got, err := database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(got)
	_, err = os.Lstat(worktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
	require.NoError(err)
	require.Len(recoveryPaths, 1)
	contents, err := os.ReadFile(filepath.Join(recoveryPaths[0], "recover.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))

	recreateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	recreated := createReadyWorkspace(t, recreateCtx, client)
	assert.NotEqual(ws.Id, recreated.Id)
	assert.Equal(worktreePath, recreated.WorktreePath)
	require.FileExists(filepath.Join(recreated.WorktreePath, ".git"))
}

func TestWorkspaceForceDeleteQuarantinesReplacementFileAndAllowsRecreateE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	client, database, _, _ := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, client)
	worktreePath := ws.WorktreePath

	require.NoError(os.RemoveAll(worktreePath))
	require.NoError(os.WriteFile(
		worktreePath, []byte("preserve replacement file\n"), 0o644,
	))

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	_, err = os.Lstat(worktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
	require.NoError(err)
	require.Len(recoveryPaths, 1)
	contents, err := os.ReadFile(recoveryPaths[0])
	require.NoError(err)
	assert.Equal("preserve replacement file\n", string(contents))

	recreated := createReadyWorkspace(t, ctx, client)
	assert.NotEqual(ws.Id, recreated.Id)
	assert.Equal(worktreePath, recreated.WorktreePath)
	require.FileExists(filepath.Join(recreated.WorktreePath, ".git"))
}

func TestWorkspaceForceDeleteReplacementCloneClearsManagedRegistrationE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	// Force setup onto the managed fallback branch so deletion has both a
	// linked-worktree registration and a Kenn Forge branch to clear.
	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/feature", "refs/heads/main")
	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)

	require.NoError(os.RemoveAll(worktreePath))
	gitfixture.Run(t, filepath.Dir(worktreePath), "clone", fixture.remote, worktreePath)
	gitfixture.Run(
		t, worktreePath, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "foreign.txt"), []byte("preserve me\n"), 0o644,
	))
	foreignHead := gitfixture.SHA(t, worktreePath, "HEAD")

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	assert.Equal(foreignHead, gitfixture.SHA(t, worktreePath, "HEAD"))
	contents, err := os.ReadFile(filepath.Join(worktreePath, "foreign.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))
	assert.NotContains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		worktreePath,
	)
	requireGitRefMissing(t, fixture.bare, "refs/heads/kenn-forge/pr-1")

	require.NoError(os.RemoveAll(worktreePath))
	recreated := createReadyWorkspace(t, ctx, fixture.client)
	assert.Equal(worktreePath, recreated.WorktreePath)
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)
}

func TestWorkspaceForceDeletePreservesSameOriginForeignLinkedWorktreeE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/feature", "refs/heads/main")
	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)

	require.NoError(os.RemoveAll(worktreePath))
	foreignGitDir := filepath.Join(t.TempDir(), "foreign.git")
	gitfixture.Run(t, filepath.Dir(foreignGitDir), "clone", "--bare", fixture.remote, foreignGitDir)
	gitfixture.Run(
		t, foreignGitDir, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	gitfixture.Run(
		t, foreignGitDir, "worktree", "add", worktreePath,
		"-b", "foreign/scratch", "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "base.txt"), []byte("foreign dirty data\n"), 0o644,
	))
	foreignHead := gitfixture.SHA(t, worktreePath, "HEAD")

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	require.FileExists(filepath.Join(worktreePath, ".git"))
	contents, err := os.ReadFile(filepath.Join(worktreePath, "base.txt"))
	require.NoError(err)
	assert.Equal("foreign dirty data\n", string(contents))
	assert.Equal(foreignHead, gitfixture.SHA(t, worktreePath, "HEAD"))
	assert.Contains(
		workspaceGitOutput(t, worktreePath, "status", "--porcelain"),
		"M base.txt",
	)
	assert.Contains(
		workspaceGitOutput(t, foreignGitDir, "worktree", "list", "--porcelain"),
		worktreePath,
	)
	assert.NotContains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		worktreePath,
	)
	requireGitRefMissing(t, fixture.bare, "refs/heads/kenn-forge/pr-1")

	gitfixture.Run(t, foreignGitDir, "worktree", "remove", "--force", worktreePath)
	recreated := createReadyWorkspace(t, ctx, fixture.client)
	assert.Equal(worktreePath, recreated.WorktreePath)
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)
}

func TestWorkspaceForceDeletePreservesForeignLinkedWorktreeAfterManagedPruneE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/feature", "refs/heads/main")
	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)

	require.NoError(os.RemoveAll(worktreePath))
	gitfixture.Run(t, fixture.bare, "worktree", "prune")
	assert.NotContains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		worktreePath,
	)

	foreignGitDir := filepath.Join(t.TempDir(), "foreign.git")
	gitfixture.Run(t, filepath.Dir(foreignGitDir), "clone", "--bare", fixture.remote, foreignGitDir)
	gitfixture.Run(
		t, foreignGitDir, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	gitfixture.Run(
		t, foreignGitDir, "worktree", "add", worktreePath,
		"-b", "foreign/scratch", "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "base.txt"), []byte("foreign dirty data\n"), 0o644,
	))
	foreignHead := gitfixture.SHA(t, worktreePath, "HEAD")

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	require.FileExists(filepath.Join(worktreePath, ".git"))
	contents, err := os.ReadFile(filepath.Join(worktreePath, "base.txt"))
	require.NoError(err)
	assert.Equal("foreign dirty data\n", string(contents))
	assert.Equal(foreignHead, gitfixture.SHA(t, worktreePath, "HEAD"))
	assert.Contains(
		workspaceGitOutput(t, worktreePath, "status", "--porcelain"),
		"M base.txt",
	)
	assert.Contains(
		workspaceGitOutput(t, foreignGitDir, "worktree", "list", "--porcelain"),
		worktreePath,
	)
}

func TestWorkspaceForceDeleteRemovesSameRepoReplacementAfterManagedPruneE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/feature", "refs/heads/main")
	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)

	require.NoError(os.RemoveAll(worktreePath))
	gitfixture.Run(t, fixture.bare, "worktree", "prune")
	gitfixture.Run(
		t, fixture.bare, "worktree", "add", worktreePath,
		"-b", "foreign/scratch", "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "base.txt"), []byte("foreign dirty data\n"), 0o644,
	))
	foreignHead := gitfixture.SHA(t, worktreePath, "HEAD")

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	require.NoDirExists(worktreePath)
	assert.NotContains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		worktreePath,
	)
	assert.Equal(
		foreignHead,
		gitfixture.SHA(t, fixture.bare, "refs/heads/foreign/scratch"),
	)
}

func TestWorkspaceForceDeleteRemovesPreMarkerWorkspaceAfterUpgradeE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/feature", "refs/heads/main")
	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, worktreePath, "branch", "--show-current"),
	)
	metadataDir := workspaceGitOutput(
		t, worktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	require.NoError(os.Remove(filepath.Join(metadataDir, "kenn-forge-workspace-id")))

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	require.NoDirExists(worktreePath)
	assert.NotContains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		worktreePath,
	)
	requireGitRefMissing(t, fixture.bare, "refs/heads/kenn-forge/pr-1")
}

func TestWorkspaceForceDeleteRetainsLockedWorktreeE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	ws := createReadyWorkspace(t, ctx, fixture.client)
	gitfixture.Run(
		t, fixture.bare, "worktree", "lock", "--reason", "test lock",
		ws.WorktreePath,
	)

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusInternalServerError,
		deleteResp.StatusCode(),
		string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(ws.Id, stored.ID)
	require.FileExists(filepath.Join(ws.WorktreePath, ".git"))
	assert.Contains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		ws.WorktreePath,
	)
	metadataDir := workspaceGitOutput(
		t, ws.WorktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	marker, err := os.ReadFile(filepath.Join(metadataDir, "kenn-forge-workspace-id"))
	require.NoError(err)
	assert.Equal(ws.Id+"\n", string(marker))

	gitfixture.Run(t, fixture.bare, "worktree", "unlock", ws.WorktreePath)
	deleteResp, err = fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)
	stored, err = fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
}

func TestWorkspaceForceDeleteForgetsSymlinkToSameRepoWorktreeE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	ws := createReadyWorkspace(t, ctx, fixture.client)
	worktreePath := ws.WorktreePath
	require.NoError(os.RemoveAll(worktreePath))
	gitfixture.Run(t, fixture.bare, "worktree", "prune")

	targetPath := filepath.Join(t.TempDir(), "replacement-worktree")
	gitfixture.Run(
		t, fixture.bare, "worktree", "add", targetPath,
		"-b", "foreign/symlink-target", "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(targetPath, "base.txt"), []byte("foreign dirty data\n"), 0o644,
	))
	require.NoError(os.Symlink(targetPath, worktreePath))
	foreignHead := gitfixture.SHA(t, targetPath, "HEAD")

	force := true
	deleteResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, deleteResp.StatusCode(), string(deleteResp.Body),
	)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	pathInfo, err := os.Lstat(worktreePath)
	require.NoError(err)
	assert.NotZero(pathInfo.Mode() & os.ModeSymlink)
	contents, err := os.ReadFile(filepath.Join(targetPath, "base.txt"))
	require.NoError(err)
	assert.Equal("foreign dirty data\n", string(contents))
	assert.Equal(foreignHead, gitfixture.SHA(t, targetPath, "HEAD"))
	assert.Contains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		targetPath,
	)
}

func TestWorkspaceCreateRejectsSymlinkedReusableWorktreeE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	worktreePath := filepath.Join(
		fixture.worktreeDir, "github", "github.com", "acme", "widget",
		fmt.Sprintf("repo-%d", fixture.repoID), "pr-1",
	)
	targetPath := filepath.Join(t.TempDir(), "linked-worktree")
	gitfixture.Run(t, fixture.bare, "worktree", "add", targetPath, "feature")
	require.NoError(os.MkdirAll(filepath.Dir(worktreePath), 0o755))
	require.NoError(os.Symlink(targetPath, worktreePath))
	wantHead := gitfixture.SHA(t, targetPath, "HEAD")

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

	var terminal *generated.WorkspaceResponse
	require.Eventually(func() bool {
		getResp, getErr := fixture.client.HTTP.GetWorkspaceWithResponse(
			ctx, createResp.JSON202.Id,
		)
		if getErr != nil || getResp.JSON200 == nil ||
			getResp.JSON200.Status == "creating" {
			return false
		}
		terminal = getResp.JSON200
		return true
	}, 10*time.Second, 25*time.Millisecond)
	require.NotNil(terminal)
	assert.Equal("error", terminal.Status)

	pathInfo, err := os.Lstat(worktreePath)
	require.NoError(err)
	assert.NotZero(pathInfo.Mode() & os.ModeSymlink)
	assert.Equal(wantHead, gitfixture.SHA(t, targetPath, "HEAD"))
	assert.Contains(
		workspaceGitOutput(t, fixture.bare, "worktree", "list", "--porcelain"),
		targetPath,
	)
	metadataDir := workspaceGitOutput(
		t, targetPath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	_, err = os.Lstat(filepath.Join(metadataDir, "kenn-forge-workspace-id"))
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestWorkspaceRetryAcceptsPreMarkerWorkspaceAfterUpgradeE2E(
	t *testing.T,
) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	ws := createReadyWorkspace(t, ctx, fixture.client)
	branch := workspaceGitOutput(t, ws.WorktreePath, "branch", "--show-current")
	metadataDir := workspaceGitOutput(
		t, ws.WorktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	require.NoError(os.Remove(filepath.Join(metadataDir, "kenn-forge-workspace-id")))
	errorMessage := "simulate setup failure before upgrade"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, ws.Id, "error", &errorMessage,
	))

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(
		http.StatusAccepted, retryResp.StatusCode(), string(retryResp.Body),
	)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, ws.Id)
	assert.Equal(branch, workspaceGitOutput(
		t, ready.WorktreePath, "branch", "--show-current",
	))
	require.FileExists(filepath.Join(ready.WorktreePath, ".git"))
	newMetadataDir := workspaceGitOutput(
		t, ready.WorktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	marker, err := os.ReadFile(filepath.Join(
		newMetadataDir, "kenn-forge-workspace-id",
	))
	require.NoError(err)
	assert.Equal(ws.Id+"\n", string(marker))
}

func TestWorkspaceRetryCleansStalePreMarkerRegistrationE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	ws := createReadyWorkspace(t, ctx, fixture.client)
	branch := workspaceGitOutput(t, ws.WorktreePath, "branch", "--show-current")
	metadataDir := workspaceGitOutput(
		t, ws.WorktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	require.NoError(os.Remove(filepath.Join(metadataDir, "kenn-forge-workspace-id")))
	require.NoError(os.RemoveAll(ws.WorktreePath))
	errorMessage := "simulate missing worktree after upgrade"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, ws.Id, "error", &errorMessage,
	))

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(
		http.StatusAccepted, retryResp.StatusCode(), string(retryResp.Body),
	)

	ready := waitForWorkspaceReady(t, ctx, fixture.client, ws.Id)
	assert.Equal(branch, workspaceGitOutput(
		t, ready.WorktreePath, "branch", "--show-current",
	))
	require.FileExists(filepath.Join(ready.WorktreePath, ".git"))
	newMetadataDir := workspaceGitOutput(
		t, ready.WorktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	marker, err := os.ReadFile(filepath.Join(
		newMetadataDir, "kenn-forge-workspace-id",
	))
	require.NoError(err)
	assert.Equal(ws.Id+"\n", string(marker))
}

func TestWorkspaceCreateOccupiedPathCreatesNoBranchesE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	const (
		mrNumber        = 2
		preferredBranch = "topic/occupied-path"
		fallbackBranch  = "kenn-forge/pr-2"
	)
	seedPRWithHeadRepo(
		t, fixture.database, "github.com", "acme", "widget", mrNumber,
		"https://github.com/contributor/widget.git",
	)
	headSHA := gitfixture.SHA(t, fixture.remote, "refs/heads/feature")
	gitfixture.Run(t, fixture.remote, "update-ref", "refs/pull/2/head", headSHA)
	_, err := fixture.database.WriteDB().ExecContext(
		ctx,
		`UPDATE forge_merge_requests SET head_branch = ? WHERE number = ?`,
		preferredBranch, mrNumber,
	)
	require.NoError(err)

	worktreePath := filepath.Join(
		fixture.worktreeDir, "github", "github.com", "acme", "widget",
		fmt.Sprintf("repo-%d", fixture.repoID), "pr-2",
	)
	require.NoError(os.MkdirAll(worktreePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "keep.txt"), []byte("preserve me\n"), 0o644,
	))

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     mrNumber,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)

	var errored *generated.WorkspaceResponse
	require.Eventually(func() bool {
		getResp, getErr := fixture.client.HTTP.GetWorkspaceWithResponse(
			ctx, createResp.JSON202.Id,
		)
		if getErr != nil || getResp.JSON200 == nil ||
			getResp.JSON200.Status != "error" {
			return false
		}
		errored = getResp.JSON200
		return true
	}, 10*time.Second, 25*time.Millisecond)
	require.NotNil(errored.ErrorMessage)
	assert.NotEmpty(*errored.ErrorMessage)

	stored, err := fixture.database.GetWorkspace(ctx, createResp.JSON202.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
	contents, err := os.ReadFile(filepath.Join(worktreePath, "keep.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))
	requireGitRefMissing(t, fixture.bare, "refs/heads/"+preferredBranch)
	requireGitRefMissing(t, fixture.bare, "refs/heads/"+fallbackBranch)
}

func TestWorkspaceCreateSameRepoHeadCloneURLTracksOriginBranchE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	client, database, clonePath, remotePath := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()

	headSHA := gitfixture.SHA(t, remotePath, "refs/heads/feature")
	gitfixture.Run(t, remotePath, "update-ref", "refs/pull/2/head", headSHA)
	gitfixture.Run(t, clonePath, "update-ref", "refs/pull/2/head", headSHA)
	seedPROnHost(t, database, "github.com", "acme", "widget", 2)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     2,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)

	ws := waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)
	require.NotNil(ws.MrHeadRepoKind)
	assert.Equal(generated.WorkspaceResponseMrHeadRepoKindSameRepo, *ws.MrHeadRepoKind)
	stored, err := database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Nil(stored.MRHeadRepo)
	assert.Empty(stored.WorkspaceBranch)
	assert.Equal("feature", workspaceGitOutput(t, ws.WorktreePath, "branch", "--show-current"))
	assert.Equal(headSHA, gitfixture.SHA(t, ws.WorktreePath, "HEAD"))
	assert.Equal(
		"origin/feature",
		workspaceGitOutput(
			t, ws.WorktreePath,
			"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}",
		),
	)
	assert.Equal(
		"refs/heads/feature",
		workspaceGitOutput(
			t, ws.WorktreePath,
			"config", "--get", "branch.feature.merge",
		),
	)
}

func requireGitRefMissing(t *testing.T, dir, ref string) {
	t.Helper()

	_, stderr, err := gitcmd.New().Run(
		t.Context(), dir, nil, "show-ref", "--verify", "--quiet", ref,
	)
	var exitErr *exec.ExitError
	require.ErrorAs(
		t, err, &exitErr, "git ref %q unexpectedly exists: %s", ref, stderr,
	)
	require.Equal(t, 1, exitErr.ExitCode(), "git show-ref failed: %s", stderr)
}

func TestWorkspaceRetryUnknownHeadRepoFailsClosedE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	seedPRWithoutHeadRepo(t, fixture.database, "github.com", "acme", "widget", 2)
	errMessage := "retry legacy workspace"
	const workspaceID = "legacy-unknown-head-repo"
	require.NoError(fixture.database.InsertWorkspace(ctx, &db.Workspace{
		ID:              workspaceID,
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      2,
		GitHeadRef:      "feature",
		MRHeadRepo:      nil,
		WorkspaceBranch: "__kenn_forge_unknown__",
		WorktreePath:    filepath.Join(t.TempDir(), workspaceID),
		TmuxSession:     "kenn-forge-" + workspaceID,
		Status:          "error",
		ErrorMessage:    &errMessage,
	}))
	issuedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(fixture.database.PutWorkspaceLaunchSpec(ctx, workspaceID, db.WorkspaceLaunchSpec{
		Version: db.WorkspaceLaunchSpecVersion,
		Repository: db.WorkspaceLaunchRepository{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
			CloneURL: "https://github.com/acme/widget.git", DefaultBranch: "main",
		},
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 2,
		ItemKey: "2", GitHeadRef: "feature",
		Pull: &db.WorkspaceLaunchPull{
			HeadBranch: "feature", HeadRepoKind: "unknown", SnapshotRevision: 1,
		},
		SourceVisible: true, IssuedAt: issuedAt,
		SourceVisibleUntil: issuedAt.Add(db.WorkspaceLaunchSpecVisibilityLease),
	}))

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, workspaceID)
	require.NoError(err)
	require.Equal(http.StatusAccepted, retryResp.StatusCode())

	var stored *db.Workspace
	require.Eventually(func() bool {
		stored, err = fixture.database.GetWorkspace(ctx, workspaceID)
		return err == nil && stored != nil && stored.Status == "error" &&
			stored.ErrorMessage != nil && strings.Contains(
			*stored.ErrorMessage, "head repository identity is unavailable",
		)
	}, 5*time.Second, 10*time.Millisecond)
	require.NotNil(stored)
	require.NotNil(stored.MRHeadRepo)
	assert.Empty(*stored.MRHeadRepo)
	assert.NoDirExists(stored.WorktreePath)

	response, err := fixture.client.HTTP.GetWorkspaceWithResponse(ctx, workspaceID)
	require.NoError(err)
	require.Equal(http.StatusOK, response.StatusCode())
	require.NotNil(response.JSON200)
	require.NotNil(response.JSON200.MrHeadRepoKind)
	assert.Equal(
		generated.WorkspaceResponseMrHeadRepoKindUnknown,
		*response.JSON200.MrHeadRepoKind,
	)
}

func TestWorkspaceDeletePreservesUserCreatedBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	client, _, clonePath, _ := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()

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

	ws := waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)
	gitfixture.Run(t, ws.WorktreePath, "checkout", "-b", "user-scratch")
	scratchSHA := gitfixture.SHA(t, ws.WorktreePath, "HEAD")

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, createResp.JSON202.Id,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, deleteResp.StatusCode())

	assert.Equal(
		scratchSHA,
		gitfixture.SHA(t, clonePath, "refs/heads/user-scratch"),
	)
}

func TestWorkspaceDeleteDoesNotCleanupReplacementCloneE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	client, database, _, remotePath := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()
	const branch = "kenn-forge/pr-42"
	replacementClone := filepath.Join(t.TempDir(), "replacement-clone")
	gitfixture.Run(t, filepath.Dir(replacementClone), "clone", remotePath, replacementClone)
	gitfixture.Run(
		t, replacementClone, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	gitfixture.Run(t, replacementClone, "branch", branch, "HEAD")
	branchSHA := gitfixture.SHA(t, replacementClone, "refs/heads/"+branch)
	wsID := "ws-replacement-clone"
	require.NoError(database.InsertWorkspace(ctx, &workspace.Workspace{
		ID:              wsID,
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature",
		WorkspaceBranch: branch,
		WorktreePath:    replacementClone,
		TerminalBackend: workspace.TerminalBackendTmux,
		Status:          "ready",
	}))

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)

	require.NoError(err)
	require.Equal(http.StatusNoContent, deleteResp.StatusCode())
	assert.DirExists(replacementClone)
	assert.Equal(branchSHA, gitfixture.SHA(t, replacementClone, "refs/heads/"+branch))
	got, err := database.GetWorkspace(ctx, wsID)
	require.NoError(err)
	assert.Nil(got)
}

func TestWorkspaceCreatePreservesExistingLocalPreferredBranch(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	client, _, clonePath, remotePath := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()

	privateClone := filepath.Join(t.TempDir(), "private-clone")
	gitfixture.Run(t, filepath.Dir(privateClone), "clone", clonePath, privateClone)
	gitfixture.Run(t, privateClone, "config", "user.email", "test@test.com")
	gitfixture.Run(t, privateClone, "config", "user.name", "Test")
	gitfixture.Run(t, privateClone, "checkout", "feature")

	require.NoError(os.WriteFile(
		filepath.Join(privateClone, "private.txt"),
		[]byte("private\n"), 0o644,
	))
	gitfixture.Run(t, privateClone, "add", "private.txt")
	gitfixture.Run(t, privateClone, "commit", "-m", "private commit")
	privateSHA := gitfixture.SHA(t, privateClone, "HEAD")
	gitfixture.Run(t, privateClone, "push", clonePath, "HEAD:feature")

	originSHA := gitfixture.SHA(t, remotePath, "refs/heads/feature")
	assert.NotEqual(originSHA, privateSHA)
	assert.Equal(privateSHA, gitfixture.SHA(t, clonePath, "refs/heads/feature"))

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

	ws := waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, ws.WorktreePath, "branch", "--show-current"),
	)
	assert.Equal(originSHA, gitfixture.SHA(t, ws.WorktreePath, "HEAD"))
	assert.Equal(privateSHA, gitfixture.SHA(t, clonePath, "refs/heads/feature"))

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, createResp.JSON202.Id,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, deleteResp.StatusCode())

	assert.Equal(privateSHA, gitfixture.SHA(t, clonePath, "refs/heads/feature"))
}

func TestWorkspaceDeleteLegacySyntheticBranchAllowsRecreate(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)

	client, database, clonePath, remotePath := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()

	privateClone := filepath.Join(t.TempDir(), "legacy-private-clone")
	gitfixture.Run(t, filepath.Dir(privateClone), "clone", clonePath, privateClone)
	gitfixture.Run(t, privateClone, "config", "user.email", "test@test.com")
	gitfixture.Run(t, privateClone, "config", "user.name", "Test")
	gitfixture.Run(t, privateClone, "checkout", "feature")
	require.NoError(os.WriteFile(
		filepath.Join(privateClone, "legacy-private.txt"),
		[]byte("legacy private\n"), 0o644,
	))
	gitfixture.Run(t, privateClone, "add", "legacy-private.txt")
	gitfixture.Run(t, privateClone, "commit", "-m", "legacy private commit")
	privateSHA := gitfixture.SHA(t, privateClone, "HEAD")
	gitfixture.Run(t, privateClone, "push", clonePath, "HEAD:feature")
	originSHA := gitfixture.SHA(t, remotePath, "refs/heads/feature")
	assert.NotEqual(originSHA, privateSHA)

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

	ws := waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, ws.WorktreePath, "branch", "--show-current"),
	)

	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_workspaces
		SET workspace_branch = '__kenn_forge_unknown__'
		WHERE id = ?`,
		createResp.JSON202.Id,
	)
	require.NoError(err)

	force := true
	deleteResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, createResp.JSON202.Id,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, deleteResp.StatusCode())

	gitfixture.Run(t, clonePath, "fetch", "--prune", "origin")

	recreateResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	require.Equal(http.StatusAccepted, recreateResp.StatusCode())
	require.NotNil(recreateResp.JSON202)

	recreated := waitForWorkspaceReady(t, ctx, client, recreateResp.JSON202.Id)
	assert.Equal(
		"kenn-forge/pr-1",
		workspaceGitOutput(t, recreated.WorktreePath, "branch", "--show-current"),
	)
	assert.Equal(originSHA, gitfixture.SHA(t, recreated.WorktreePath, "HEAD"))
}

func TestWorkspaceDeleteDirty(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	client, database, _, _ := setupLifecycleWorkspaceServer(t)
	ctx := t.Context()

	// Create workspace.
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

	ready := waitForWorkspaceReady(t, ctx, client, wsID)
	wsPath := ready.WorktreePath

	// Write a dirty file into the worktree.
	require.NoError(os.WriteFile(
		filepath.Join(wsPath, "dirty.txt"),
		[]byte("uncommitted\n"), 0o644,
	))

	// DELETE without force -> 409.
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{},
	)
	require.NoError(err)
	assert.Equal(http.StatusConflict, delResp.StatusCode())

	// DELETE with force -> 204.
	force := true
	delResp2, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	assert.Equal(http.StatusNoContent, delResp2.StatusCode())

	// Verify deleted.
	getResp, err := client.HTTP.GetWorkspaceWithResponse(
		ctx, wsID,
	)
	require.NoError(err)
	assert.Equal(http.StatusNotFound, getResp.StatusCode())

	// --- Second scenario: corrupt/missing worktree ---
	// Seed a second PR and create a workspace for it.
	seedPROnHost(t, database, "github.com", "acme", "widget", 2)
	create2, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     2,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, create2.StatusCode())
	ws2ID := create2.JSON202.Id

	ready2 := waitForWorkspaceReady(t, ctx, client, ws2ID)
	ws2Path := ready2.WorktreePath

	// Nuke the worktree directory to simulate corruption.
	require.NoError(os.RemoveAll(ws2Path))

	// DELETE without force → 409 (dirty check fails on missing dir).
	del3, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws2ID, &generated.DeleteWorkspaceParams{},
	)
	require.NoError(err)
	assert.Equal(http.StatusConflict, del3.StatusCode())

	// DELETE with force → 204.
	del4, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws2ID,
		&generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	assert.Equal(http.StatusNoContent, del4.StatusCode())

	// Verify deleted.
	get2, err := client.HTTP.GetWorkspaceWithResponse(ctx, ws2ID)
	require.NoError(err)
	assert.Equal(http.StatusNotFound, get2.StatusCode())
}

func TestWorkspaceForceDeleteToleratesMissingWorktreeCommonDirE2E(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)

	client, database, _, _ := setupLifecycleWorkspaceServer(t)
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)
	installGitCommonDirReadFailure(t)

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(
		http.StatusNoContent, delResp.StatusCode(), string(delResp.Body),
	)

	got, err := database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(got)
}

func installGitCommonDirReadFailure(t *testing.T) {
	t.Helper()

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(`#!/bin/sh
set -eu
case " $* " in
	*" rev-parse --path-format=absolute --git-common-dir "*)
		echo "fatal: failed to read worktrees/pr-1/commondir: Success" >&2
		exit 128
		;;
esac
exec "$KENN_FORGE_TEST_REAL_GIT" "$@"
`), 0o755))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_REAL_GIT", realGit)
}
