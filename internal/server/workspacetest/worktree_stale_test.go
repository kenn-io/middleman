package workspacetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
)

// TestRemoveStaleWorktreeRoute exercises the stale-worktree removal ported from
// the embedding host. A worktree that discovery flagged stale (its checkout
// vanished from `git worktree list`) is removed by fleet scoped key; removing a
// missing or already-removed key is a 404.
func TestRemoveStaleWorktreeRoute(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))
	projectID := registerProjectForTest(t, ts, repoDir)

	// Register a worktree whose checkout never materializes, then mark it stale
	// by reconciling an inventory that no longer lists it — the same transition
	// discovery performs when a checkout disappears.
	wtPath := filepath.Join(t.TempDir(), "wt-gone")
	registerWorktreeForTest(t, ts, projectID, "feat", wtPath, http.StatusCreated)
	require.NoError(database.ReconcileProjectInventory(
		t.Context(), projectID, db.ProjectInventory{}, time.Now(),
	))

	remove := func(scopedKey string) *http.Response {
		return httpDo(t, ts, http.MethodPost, "/api/v1/worktrees/remove-stale",
			mustMarshal(t, map[string]any{"scopedKey": scopedKey}))
	}

	// Unknown scoped key -> 404.
	resp := remove("worktree:" + filepath.Join(t.TempDir(), "never"))
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// Stale worktree with a vanished path -> removed.
	resp = remove("worktree:" + wtPath)
	require.Equal(http.StatusOK, resp.StatusCode)
	var out struct {
		Removed bool `json:"removed"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&out))
	resp.Body.Close()
	assert.True(out.Removed)

	// The worktree is gone from the project.
	listResp := httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees", nil)
	require.Equal(http.StatusOK, listResp.StatusCode)
	var wtList struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	require.NoError(json.NewDecoder(listResp.Body).Decode(&wtList))
	listResp.Body.Close()
	require.Len(wtList.Worktrees, 1, "only the root checkout row remains")
	assert.NotNil(worktreeRowByPathBase(wtList.Worktrees, filepath.Base(repoDir)),
		"the surviving row is the root checkout")
	assert.Nil(worktreeRowByBranch(wtList.Worktrees, "feat"),
		"the stale worktree row is gone")

	// A second removal of the same key is a 404 (already gone).
	resp = remove("worktree:" + wtPath)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

func TestRemoveStaleWorktreeRoute_RefusesNonStaleAndReappeared(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))
	projectID := registerProjectForTest(t, ts, repoDir)

	remove := func(scopedKey string) *http.Response {
		return httpDo(t, ts, http.MethodPost, "/api/v1/worktrees/remove-stale",
			mustMarshal(t, map[string]any{"scopedKey": scopedKey}))
	}

	// A registered, non-stale worktree is refused.
	livePath := filepath.Join(t.TempDir(), "wt-live")
	registerWorktreeForTest(t, ts, projectID, "live", livePath, http.StatusCreated)
	resp := remove("worktree:" + livePath)
	require.Equal(http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	// A stale worktree whose checkout reappeared on disk is refused.
	backPath := filepath.Join(t.TempDir(), "wt-back")
	registerWorktreeForTest(t, ts, projectID, "back", backPath, http.StatusCreated)
	require.NoError(database.ReconcileProjectInventory(
		t.Context(), projectID, db.ProjectInventory{}, time.Now(),
	))
	require.NoError(os.MkdirAll(backPath, 0o755))
	resp = remove("worktree:" + backPath)
	require.Equal(http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}
