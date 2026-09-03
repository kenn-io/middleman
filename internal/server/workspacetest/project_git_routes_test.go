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
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestCloneProject(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	source := initLifecycleRouteRepo(t)
	gitfixture.Run(t, source, "branch", "feat/clone")
	dest := filepath.Join(t.TempDir(), "cloned")

	body := mustMarshal(t, map[string]any{
		"url":  source,
		"path": dest,
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects/clone", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var created struct {
		ID        string `json:"id"`
		LocalPath string `json:"local_path"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&created))
	resp.Body.Close()
	require.NotEmpty(created.ID)
	assert.Equal(dest, created.LocalPath)

	// The checkout exists on disk and the project is registered with
	// its root worktree discovered.
	_, err := os.Stat(filepath.Join(dest, ".git"))
	require.NoError(err)
	project, err := database.GetProjectByID(t.Context(), created.ID)
	require.NoError(err)
	assert.Equal(dest, project.LocalPath)
	rows := listWorktreeRows(t, ts, created.ID)
	require.NotEmpty(rows, "discovery registers the root checkout")

	// Cloning onto an existing destination is refused.
	resp = httpDo(t, ts, http.MethodPost, "/api/v1/projects/clone", body)
	assert.Equal(http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

// TestCloneProjectBranchAndHomePath covers the branch option and
// home-relative destination expansion: fleet clients send "~/..." paths
// because only the executing host knows its home.
func TestCloneProjectBranchAndHomePath(t *testing.T) {
	acquireWorkspaceGitSlot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	source := initLifecycleRouteRepo(t)
	gitfixture.Run(t, source, "branch", "feat/clone")

	body := mustMarshal(t, map[string]any{
		"url":    source,
		"path":   "~/clones/widget",
		"branch": "feat/clone",
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects/clone", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var created struct {
		LocalPath string `json:"local_path"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&created))
	resp.Body.Close()
	assert.Equal(filepath.Join(fakeHome, "clones", "widget"), created.LocalPath)

	out, err := procutil.Command(
		"git", "-C", created.LocalPath,
		"rev-parse", "--abbrev-ref", "HEAD",
	).Output()
	require.NoError(err)
	assert.Equal("feat/clone", string(out[:len(out)-1]))
}

// TestCloneProjectFailureCleansOwnedDestination pins the rollback
// contract: a failed clone removes the destination directory this
// request reserved, so an immediate retry reaches git again instead of
// tripping destinationExists over a leftover partial checkout.
func TestCloneProjectFailureCleansOwnedDestination(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "clones", "broken")
	body := mustMarshal(t, map[string]any{
		// A file URL that does not exist: git fails after the
		// destination has been reserved.
		"url":  "file://" + filepath.Join(t.TempDir(), "no-such-repo.git"),
		"path": dest,
	})

	for attempt := 1; attempt <= 2; attempt++ {
		resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects/clone", body)
		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(json.NewDecoder(resp.Body).Decode(&problem))
		resp.Body.Close()
		require.Equal(http.StatusBadRequest, resp.StatusCode,
			"attempt %d must fail at git, not at destination reservation", attempt)
		assert.NotEqual("destinationExists", problem.Code,
			"attempt %d must not trip over a leftover partial checkout", attempt)
		_, statErr := os.Stat(dest)
		assert.True(os.IsNotExist(statErr),
			"attempt %d must remove the reserved destination", attempt)
	}
}

func TestListProjectBranches(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := initLifecycleRouteRepo(t)
	gitfixture.Run(t, repo, "branch", "feat/one")
	gitfixture.Run(t, repo, "branch", "feat/two")
	projectID := registerProjectForTest(t, ts, repo)

	resp := httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/branches", nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var body struct {
		Branches []string `json:"branches"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	assert.Equal([]string{"feat/one", "feat/two", "main"}, body.Branches)

	missing := httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/prj_nope/branches", nil)
	assert.Equal(http.StatusNotFound, missing.StatusCode)
	missing.Body.Close()
}

// TestInspectProjectWorktree covers
// GET /api/v1/projects/{pid}/worktrees/{wid}/inspect: dirty state, live
// session count, and branch-delete eligibility for delete confirmation UIs.
func TestInspectProjectWorktree(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := initLifecycleRouteRepo(t)
	projectID := registerProjectForTest(t, ts, repo)

	// Create a linked worktree on its own branch through the API.
	body := mustMarshal(t, map[string]any{
		"branch":         "feat/inspect",
		"create_on_disk": true,
	})
	created := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees", body)
	require.Equal(http.StatusCreated, created.StatusCode)
	created.Body.Close()

	rows := listWorktreeRows(t, ts, projectID)
	linked := worktreeRowByBranch(rows, "feat/inspect")
	require.NotNil(linked)
	root := worktreeRowByPathBase(rows, filepath.Base(repo))
	require.NotNil(root)

	type inspection struct {
		IsDirty                   bool     `json:"is_dirty"`
		DirtyFileCount            int      `json:"dirty_file_count"`
		AliveSessionCount         int      `json:"alive_session_count"`
		CanDeleteBranch           bool     `json:"can_delete_branch"`
		BranchDeleteBlockedReason string   `json:"branch_delete_blocked_reason"`
		SiblingWorktreeIDs        []string `json:"sibling_worktree_ids"`
	}
	inspect := func(worktreeID string) inspection {
		resp := httpDo(t, ts, http.MethodGet,
			"/api/v1/projects/"+projectID+"/worktrees/"+
				worktreeID+"/inspect", nil)
		require.Equal(http.StatusOK, resp.StatusCode)
		var got inspection
		require.NoError(json.NewDecoder(resp.Body).Decode(&got))
		resp.Body.Close()
		return got
	}

	// A clean linked worktree on its own branch is fully deletable.
	got := inspect(linked["id"].(string))
	assert.False(got.IsDirty)
	assert.Zero(got.DirtyFileCount)
	assert.Zero(got.AliveSessionCount)
	assert.True(got.CanDeleteBranch)
	assert.Empty(got.BranchDeleteBlockedReason)

	// Dirty the worktree: two untracked files are counted.
	require.NoError(os.WriteFile(
		filepath.Join(linked["path"].(string), "a.txt"), []byte("x"), 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(linked["path"].(string), "b.txt"), []byte("y"), 0o644))
	got = inspect(linked["id"].(string))
	assert.True(got.IsDirty)
	assert.Equal(2, got.DirtyFileCount)

	// The primary root row protects its branch (the default branch).
	got = inspect(root["id"].(string))
	assert.False(got.CanDeleteBranch)
	assert.NotEmpty(got.BranchDeleteBlockedReason)
}

// TestInspectProjectWorktreeCountsStoredTmuxSessions proves a durable
// tmux session that survived a daemon restart (stored row, no in-memory
// runtime session) still counts as alive — otherwise delete confirmation
// under-reports live work after a restart.
func TestInspectProjectWorktreeCountsStoredTmuxSessions(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := initLifecycleRouteRepo(t)
	projectID := registerProjectForTest(t, ts, repo)
	rows := listWorktreeRows(t, ts, projectID)
	root := worktreeRowByPathBase(rows, filepath.Base(repo))
	require.NotNil(root)
	worktreeID := root["id"].(string)

	require.NoError(database.UpsertProjectWorktreeTmuxSession(
		t.Context(), &db.ProjectWorktreeTmuxSession{
			WorktreeID:  worktreeID,
			SessionKey:  "restart-survivor",
			SessionName: "kenn-forge-restart-survivor",
			Label:       "Survivor",
			CreatedAt:   time.Now().UTC(),
		},
	))

	resp := httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees/"+worktreeID+"/inspect", nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var got struct {
		AliveSessionCount int `json:"alive_session_count"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&got))
	resp.Body.Close()
	assert.Equal(1, got.AliveSessionCount,
		"stored tmux sessions count as alive after a restart")
}

// TestInspectProjectWorktreeCountsStoredTmuxSessionsWithRuntime is the
// restart case with a runtime manager configured: the in-memory runtime
// has no entry for the stored row (the daemon restarted), and the
// merged listing must still count the durable session as alive.
