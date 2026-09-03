package workspacetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestW1SliceAGate(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))

	// 1) Register a project from a path with no `gh` context and no
	//    parseable remote. The response must include a server-assigned
	//    project_id and must omit platform_identity.
	registerBody := mustMarshal(t, map[string]any{
		"local_path":   repoDir,
		"display_name": "no-remote-repo",
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", registerBody)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var registered map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	resp.Body.Close()
	projectID, _ := registered["id"].(string)
	require.NotEmpty(projectID)
	assert.True(strings.HasPrefix(projectID, "prj_"))
	assert.NotContains(registered, "platform_identity",
		"platform_identity must be absent when no remote is parseable")
	assert.Equal("no-remote-repo", registered["display_name"])
	assert.NotContains(registered, "host",
		"host column was speculative; the response must not include it")

	// 2) GET /projects must list the registered project.
	resp = httpDo(t, ts, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var listed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&listed))
	resp.Body.Close()
	require.Len(listed.Projects, 1)
	assert.Equal(projectID, listed.Projects[0]["id"])
	assert.NotContains(listed.Projects[0], "platform_identity")

	// 3) GET /projects/{project_id} must round-trip the record with
	//    platform_identity still absent.
	resp = httpDo(t, ts, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var fetched map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&fetched))
	resp.Body.Close()
	assert.Equal(projectID, fetched["id"])
	assert.NotContains(fetched, "platform_identity")

	// 4) Register a worktree the daemon already created on disk.
	//    Kenn Forge just persists the metadata - the path validity
	//    contract is the daemon's, not Kenn Forge's.
	worktreePath := filepath.Join(t.TempDir(), "wt-feature-x")
	wtBody := mustMarshal(t, map[string]any{
		"branch": "feature-x",
		"path":   worktreePath,
	})
	resp = httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees", wtBody,
	)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var worktree map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&worktree))
	resp.Body.Close()
	worktreeID, _ := worktree["id"].(string)
	require.NotEmpty(worktreeID)
	assert.True(strings.HasPrefix(worktreeID, "wtr_"))
	assert.Equal(projectID, worktree["project_id"])
	assert.Equal("feature-x", worktree["branch"])
	assert.Equal(worktreePath, worktree["path"])

	// 5) Listing the project's worktrees must return the new record.
	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees", nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	var wtList struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&wtList))
	resp.Body.Close()
	require.Len(wtList.Worktrees, 2,
		"root checkout row plus the registered worktree")
	registeredRow := worktreeRowByBranch(wtList.Worktrees, "feature-x")
	require.NotNil(registeredRow, "registered worktree is listed")
	assert.Equal(worktreeID, registeredRow["id"])
	assert.NotNil(
		worktreeRowByPathBase(wtList.Worktrees, filepath.Base(repoDir)),
		"the project root checkout has a registry row")

	// 6) Launch-target discovery must include plain_shell with
	//    available: true. Configured-agent presence depends on PATH;
	//    only plain_shell is required.
	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/launch-targets", nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	var ltList struct {
		LaunchTargets []map[string]any `json:"launch_targets"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&ltList))
	resp.Body.Close()
	require.NotEmpty(ltList.LaunchTargets)

	var plainShell map[string]any
	for _, target := range ltList.LaunchTargets {
		if target["key"] == "plain_shell" {
			plainShell = target
			break
		}
	}
	require.NotNil(plainShell, "plain_shell must be present")
	assert.Equal(true, plainShell["available"])
	assert.Equal("plain_shell", plainShell["kind"])

	// 7) The live OpenAPI document must register the gate's operation
	//    IDs and must not bake PR/MR/issue terms into them - the
	//    generic registry must be a generic registry.
	resp = httpDo(t, ts, http.MethodGet, "/api/v1/openapi.json", nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&doc))
	resp.Body.Close()

	expectedOps := map[string]string{
		"POST /projects":                                        "register-project",
		"GET /projects":                                         "list-projects",
		"GET /projects/{project_id}":                            "get-project",
		"POST /projects/{project_id}/worktrees":                 "register-worktree",
		"DELETE /projects/{project_id}/worktrees/{worktree_id}": "delete-worktree",
		"GET /projects/{project_id}/worktrees":                  "list-worktrees",
		"GET /projects/{project_id}/launch-targets":             "list-launch-targets",
	}
	for spec, wantID := range expectedOps {
		method, path, _ := strings.Cut(spec, " ")
		gotPath, ok := doc.Paths[path]
		require.Truef(ok, "OpenAPI doc missing path %q", path)
		gotOp, ok := gotPath[strings.ToLower(method)]
		require.Truef(ok, "OpenAPI doc missing %s on %s", method, path)
		assert.Equalf(wantID, gotOp.OperationID,
			"unexpected operation id for %s %s", method, path)
	}

	// 8) Negative: no operation ID on a generic project route may
	//    contain "pull-request", "issue", or "mr" terms. This is the
	//    "generic, not a PR fork" assertion from the convergence plan.
	for path, methods := range doc.Paths {
		if !strings.HasPrefix(path, "/projects") {
			continue
		}
		for method, op := range methods {
			id := op.OperationID
			for _, banned := range []string{"pull-request", "pullrequest", "pr-", "issue", "mr-"} {
				assert.NotContainsf(id, banned,
					"op id %q on %s %s contains banned term %q",
					id, method, path, banned)
			}
		}
	}
}

func TestRegisterProject_RejectsMissingPath(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := mustMarshal(t, map[string]any{"local_path": ""})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(err)
	assert.Contains(string(payload), "local_path")
}

func TestRegisterProject_PreservesExplicitProviderIdentity(t *testing.T) {
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

	body := mustMarshal(t, map[string]any{
		"local_path": repoDir,
		"platform_identity": map[string]string{
			"platform":      "gitlab",
			"platform_host": "git.example.com",
			"owner":         "platform",
			"name":          "runner",
		},
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	defer resp.Body.Close()

	var registered struct {
		ID               string `json:"id"`
		PlatformIdentity struct {
			Platform     string `json:"platform"`
			PlatformHost string `json:"platform_host"`
			Owner        string `json:"owner"`
			Name         string `json:"name"`
		} `json:"platform_identity"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	assert.Equal("gitlab", registered.PlatformIdentity.Platform)
	assert.Equal("git.example.com", registered.PlatformIdentity.PlatformHost)

	project, err := database.GetProjectByID(t.Context(), registered.ID)
	require.NoError(err)
	require.NotNil(project.PlatformIdentity)
	assert.Equal(&db.PlatformIdentity{
		Platform: "gitlab",
		Host:     "git.example.com",
		Owner:    "platform",
		Name:     "runner",
	}, project.PlatformIdentity)
}

func TestRegisterProject_UsesConfiguredProviderForRemoteIdentity(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _, _ := setupProjectServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[platforms]]
type = "gitlab"
host = "code.example.com"
`, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	gitfixture.Run(t, repoDir, "init", "-q")
	gitfixture.Run(t, repoDir, "remote", "add", "origin", "git@code.example.com:group/subgroup/project.git")

	body := mustMarshal(t, map[string]any{"local_path": repoDir})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	defer resp.Body.Close()

	var registered map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	identity, ok := registered["platform_identity"].(map[string]any)
	require.True(ok, "platform_identity must be present")
	assert.Equal("gitlab", identity["platform"])
	assert.Equal("code.example.com", identity["platform_host"])
	assert.Equal("group/subgroup", identity["owner"])
	assert.Equal("project", identity["name"])
}

func TestRegisterProject_UsesDefaultPlatformHostForRemoteIdentity(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _, _ := setupProjectServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
default_platform_host = "ghe.example.com"
host = "127.0.0.1"
port = 8091
`, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	gitfixture.Run(t, repoDir, "init", "-q")
	gitfixture.Run(t, repoDir, "remote", "add", "origin", "git@ghe.example.com:acme/widget.git")

	body := mustMarshal(t, map[string]any{"local_path": repoDir})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	defer resp.Body.Close()

	var registered map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	identity, ok := registered["platform_identity"].(map[string]any)
	require.True(ok, "platform_identity must be present")
	assert.Equal("github", identity["platform"])
	assert.Equal("ghe.example.com", identity["platform_host"])
	assert.Equal("acme", identity["owner"])
	assert.Equal("widget", identity["name"])
}

func TestRegisterProject_RejectsNonexistentPath(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := mustMarshal(t, map[string]any{
		"local_path": "/this/path/should/never/exist",
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestRegisterProject_DuplicatePathReturns409(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))

	body := mustMarshal(t, map[string]any{"local_path": repoDir})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	resp = httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

func TestRegisterProject_AcceptsCallerProvidedIdentity(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))

	// Even though the repo has no remote, the caller can provide
	// platform_identity directly. Caller-provided wins, and the handler
	// upserts a forge_repos row to give the project a stable FK
	// target - no sync subscription is created (sync is driven by TOML
	// config, not by forge_repos rows).
	body := mustMarshal(t, map[string]any{
		"local_path": repoDir,
		"platform_identity": map[string]string{
			"platform":      "github",
			"platform_host": "github.com",
			"owner":         "acme",
			"name":          "widget",
		},
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var got map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&got))
	resp.Body.Close()
	identity, _ := got["platform_identity"].(map[string]any)
	require.NotNil(identity)
	assert.Equal("github", identity["platform"])
	assert.Equal("github.com", identity["platform_host"])
	assert.NotContains(identity, "host")
	assert.Equal("acme", identity["owner"])
	assert.Equal("widget", identity["name"])

	// Re-fetching reads the identity off the joined forge_repos
	// row - confirms the FK linkage is what the response is built from
	// (not a stale duplicate copy on forge_projects).
	projectID, _ := got["id"].(string)
	require.NotEmpty(projectID)
	resp = httpDo(t, ts, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	var fetched map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&fetched))
	resp.Body.Close()
	identity2, _ := fetched["platform_identity"].(map[string]any)
	require.NotNil(identity2)
	assert.Equal("github", identity2["platform"])
	assert.Equal("github.com", identity2["platform_host"])
	assert.NotContains(identity2, "host")
	assert.Equal("acme", identity2["owner"])
	assert.Equal("widget", identity2["name"])
}

// TestRegisterProject_DoesNotSubscribeRepoToSync pins the load-bearing
// invariant that registering a project does NOT subscribe the linked
// repo to sync. registerProject calls db.UpsertRepo to give the project
// a stable forge_repos FK target, but UpsertRepo is pure DDL and
// must not touch the syncer's in-memory tracked-repos list - sync
// subscription is driven exclusively by the user's TOML config and
// SetRepos.
//
// If a future refactor accidentally couples UpsertRepo (or the
// project-registration path) to sync, this test fails and flags the
// regression: an embedder could otherwise quietly add unwanted repos
// to the sync set just by registering a project.

func TestGetProject_NotFoundReturns404(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := httpDo(t, ts, http.MethodGet, "/api/v1/projects/prj_nope", nil)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestRegisterWorktree_SamePathSameProjectConverges asserts that re-registering
// a worktree at a path the same project already owns is idempotent rather than a
// conflict: the row keeps its id and refreshes its branch. This lets explicit
// registration converge with a row the background discovery pass created.
func TestRegisterWorktree_SamePathSameProjectConverges(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))
	projectID := registerProjectForTest(t, ts, repoDir)

	wtPath := filepath.Join(t.TempDir(), "wt-1")
	firstID := registerWorktreeForTest(t, ts, projectID, "feature-x", wtPath, http.StatusCreated)
	require.NotEmpty(firstID)

	adopted := mustMarshal(t, map[string]any{"branch": "feature-y", "path": wtPath})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees", adopted,
	)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var second map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&second))
	resp.Body.Close()
	require.Equal(firstID, second["id"])
	require.Equal("feature-y", second["branch"])
}

// TestSetWorktreeSessionBackendRoute covers the wave-2 write-through target:
// PUT .../session-backend persists the override, the worktree list reflects it,
// and a worktree id under the valid project that does not exist is a 404.
func TestSetWorktreeSessionBackendRoute(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))
	projectID := registerProjectForTest(t, ts, repoDir)
	wtPath := filepath.Join(t.TempDir(), "wt-feat")
	worktreeID := registerWorktreeForTest(
		t, ts, projectID, "feat", wtPath, http.StatusCreated,
	)

	body := mustMarshal(t, map[string]any{"session_backend": "localTmux"})
	resp := httpDo(t, ts, http.MethodPut,
		"/api/v1/projects/"+projectID+"/worktrees/"+worktreeID+"/session-backend",
		body,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	var updated map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&updated))
	resp.Body.Close()
	require.Equal("localTmux", updated["session_backend"])

	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees", nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	var wtList struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&wtList))
	resp.Body.Close()
	require.Len(wtList.Worktrees, 2, "root checkout row plus the worktree")
	featRow := worktreeRowByBranch(wtList.Worktrees, "feat")
	require.NotNil(featRow)
	require.Equal("localTmux", featRow["session_backend"])

	resp = httpDo(t, ts, http.MethodPut,
		"/api/v1/projects/"+projectID+"/worktrees/wtr_missing/session-backend",
		body,
	)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// A value outside the canonical vocabulary must be rejected, not
	// persisted into worktree rows and fleet snapshots.
	body = mustMarshal(t, map[string]any{"session_backend": "carrierPigeon"})
	resp = httpDo(t, ts, http.MethodPut,
		"/api/v1/projects/"+projectID+"/worktrees/"+worktreeID+"/session-backend",
		body,
	)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees", nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	require.NoError(json.NewDecoder(resp.Body).Decode(&wtList))
	resp.Body.Close()
	require.Len(wtList.Worktrees, 2)
	featRow = worktreeRowByBranch(wtList.Worktrees, "feat")
	require.NotNil(featRow)
	require.Equal("localTmux", featRow["session_backend"],
		"a rejected value must not overwrite the stored override")

	// Explicit null still clears the override.
	body = mustMarshal(t, map[string]any{"session_backend": nil})
	resp = httpDo(t, ts, http.MethodPut,
		"/api/v1/projects/"+projectID+"/worktrees/"+worktreeID+"/session-backend",
		body,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	require.NoError(json.NewDecoder(resp.Body).Decode(&updated))
	resp.Body.Close()
	require.Empty(updated["session_backend"])
}

func TestDeleteWorktreeRoute(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))
	projectID := registerProjectForTest(t, ts, repoDir)
	wtPath := filepath.Join(t.TempDir(), "wt-feat")
	worktreeID := registerWorktreeForTest(
		t, ts, projectID, "feat", wtPath, http.StatusCreated,
	)

	resp := httpDo(t, ts, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/worktrees/"+worktreeID, nil,
	)
	require.Equal(http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID+"/worktrees", nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	var wtList struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&wtList))
	resp.Body.Close()
	require.Len(wtList.Worktrees, 1, "only the root checkout row remains")
	require.Nil(worktreeRowByBranch(wtList.Worktrees, "feat"),
		"the worktree is gone from the project")

	// The owning project survives the worktree delete.
	resp = httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/"+projectID, nil,
	)
	require.Equal(http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Deleting an unknown worktree id is a 404.
	resp = httpDo(t, ts, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/worktrees/wtr_missing", nil,
	)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestRegisterWorktree_SamePathDifferentProjectReturns409 keeps a genuine
// cross-project path collision a conflict; convergence only applies within the
// owning project.
func TestRegisterWorktree_SamePathDifferentProjectReturns409(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoA := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoA))
	repoB := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoB))
	projectA := registerProjectForTest(t, ts, repoA)
	projectB := registerProjectForTest(t, ts, repoB)

	wtPath := filepath.Join(t.TempDir(), "shared-wt")
	registerWorktreeForTest(t, ts, projectA, "feature-x", wtPath, http.StatusCreated)

	conflict := mustMarshal(t, map[string]any{"branch": "feature-x", "path": wtPath})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectB+"/worktrees", conflict,
	)
	require.Equal(http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

// registerProjectForTest registers a project at localPath and returns its id.
func registerProjectForTest(t *testing.T, ts *httptest.Server, localPath string) string {
	t.Helper()
	body := mustMarshal(t, map[string]any{"local_path": localPath})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var registered map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&registered))
	resp.Body.Close()
	id, _ := registered["id"].(string)
	require.NotEmpty(t, id)
	return id
}

// registerWorktreeForTest registers a worktree, asserts the status, and returns
// the worktree id from a successful response (empty otherwise).
func registerWorktreeForTest(
	t *testing.T, ts *httptest.Server, projectID, branch, path string, wantStatus int,
) string {
	t.Helper()
	body := mustMarshal(t, map[string]any{"branch": branch, "path": path})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees", body,
	)
	require.Equal(t, wantStatus, resp.StatusCode)
	defer resp.Body.Close()
	if wantStatus != http.StatusCreated {
		return ""
	}
	var created map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	id, _ := created["id"].(string)
	return id
}

func TestListLaunchTargets_NotFoundReturns404(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := httpDo(t, ts, http.MethodGet,
		"/api/v1/projects/prj_nope/launch-targets", nil,
	)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}
func TestDeleteProjectRouteRemovesProject(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repoDir := t.TempDir()
	require.NoError(initLocalOnlyGitRepo(t.Context(), repoDir))

	registerBody := mustMarshal(t, map[string]any{
		"local_path":   repoDir,
		"display_name": "doomed",
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", registerBody)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var registered struct {
		ID string `json:"id"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	resp.Body.Close()
	require.NotEmpty(registered.ID)

	resp = httpDo(t, ts, http.MethodDelete, "/api/v1/projects/"+registered.ID, nil)
	require.Equal(http.StatusNoContent, resp.StatusCode, "delete returns 204 No Content")
	resp.Body.Close()

	resp = httpDo(t, ts, http.MethodGet, "/api/v1/projects/"+registered.ID, nil)
	require.Equal(http.StatusNotFound, resp.StatusCode, "the project is gone after delete")
	resp.Body.Close()

	resp = httpDo(t, ts, http.MethodDelete, "/api/v1/projects/"+registered.ID, nil)
	require.Equal(http.StatusNotFound, resp.StatusCode,
		"deleting an already-gone project is 404, not 500")
	resp.Body.Close()
}
