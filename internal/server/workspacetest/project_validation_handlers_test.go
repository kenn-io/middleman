package workspacetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterProject_RejectsPartialPlatformIdentity(t *testing.T) {
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

	missingFieldBody := mustMarshal(t, map[string]any{
		"local_path": repoDir,
		"platform_identity": map[string]string{
			"platform":      "github",
			"platform_host": "github.com",
			"owner":         "acme",
			// missing "name" — Huma's schema rejects with 422
		},
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", missingFieldBody)
	require.Equal(http.StatusUnprocessableEntity, resp.StatusCode)
	resp.Body.Close()

	whitespaceBody := mustMarshal(t, map[string]any{
		"local_path": repoDir,
		"platform_identity": map[string]string{
			"platform":      "github",
			"platform_host": "github.com",
			"owner":         "acme",
			"name":          "   ",
		},
	})
	resp = httpDo(t, ts, http.MethodPost, "/api/v1/projects", whitespaceBody)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(err)
	assert.Contains(string(payload), "platform_identity")
}

// TestRegisterProject_RejectsPathThatIsAFile guards against an embedder
// passing a regular file as local_path (e.g. a config file or symlink the
// host resolved to the wrong target). The handler must reject before
// recording the row.

func TestRegisterProject_RejectsPathThatIsAFile(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir.txt")
	require.NoError(os.WriteFile(filePath, []byte(""), 0o600))

	body := mustMarshal(t, map[string]any{"local_path": filePath})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(err)
	assert.Contains(string(payload), "not a directory")
}

// TestRegisterWorktree_RejectsBlankFields covers the required worktree
// fields under both Huma schema validation (missing branch → 422) and the
// handler's checks (missing/whitespace path without create_on_disk and
// whitespace branch → 400). Both contracts are embedder-facing; pinning
// both guards against either layer regressing. Path is schema-optional
// because create_on_disk derives it; without create_on_disk the handler
// still requires it.

func TestRegisterWorktree_RejectsBlankFields(t *testing.T) {
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

	regBody := mustMarshal(t, map[string]any{"local_path": repoDir})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", regBody)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var registered map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&registered))
	resp.Body.Close()
	projectID, _ := registered["id"].(string)

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantBody   string
	}{
		{
			name:       "missing branch returns 422 from schema",
			body:       map[string]any{"path": "/tmp/whatever"},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "missing path without create_on_disk returns 400 from handler",
			body:       map[string]any{"branch": "feature-x"},
			wantStatus: http.StatusBadRequest,
			wantBody:   "path",
		},
		{
			name:       "whitespace branch returns 400 from handler",
			body:       map[string]any{"branch": "   ", "path": "/tmp/whatever"},
			wantStatus: http.StatusBadRequest,
			wantBody:   "branch",
		},
		{
			name:       "whitespace path returns 400 from handler",
			body:       map[string]any{"branch": "feature-x", "path": "   "},
			wantStatus: http.StatusBadRequest,
			wantBody:   "path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := mustMarshal(t, tc.body)
			resp := httpDo(t, ts, http.MethodPost,
				"/api/v1/projects/"+projectID+"/worktrees", body,
			)
			defer resp.Body.Close()
			require.Equal(tc.wantStatus, resp.StatusCode)
			if tc.wantBody != "" {
				payload, err := io.ReadAll(resp.Body)
				require.NoError(err)
				assert.Contains(string(payload), tc.wantBody)
			}
		})
	}
}

// TestRegisterWorktree_NotFoundReturns404 pins the failure mode an embedder
// hits if the project_id is wrong or the project was deleted between
// register-project and register-worktree.

func TestRegisterWorktree_NotFoundReturns404(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := mustMarshal(t, map[string]any{
		"branch": "feature-x",
		"path":   "/tmp/wt-1",
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/prj_nope/worktrees", body,
	)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestListProjects_ReturnsEmptyArrayNotNull pins that the JSON response
// always emits an empty array when no projects are registered. An embedder
// iterating the response with `for (const p of resp.projects)` would crash
// on null but works on []. The Go side initializes the slice non-nil; this
// test catches a regression that lets the empty case marshal to null.

func TestListProjects_ReturnsEmptyArrayNotNull(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := httpDo(t, ts, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()
	var listed map[string]json.RawMessage
	require.NoError(json.NewDecoder(resp.Body).Decode(&listed))
	raw, ok := listed["projects"]
	require.True(ok, "response must include a projects key")
	assert.Equal("[]", string(raw),
		"empty list must serialize as [] for embedder iteration safety")
}
