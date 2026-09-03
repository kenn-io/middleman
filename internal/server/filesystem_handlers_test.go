package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

// TestFilesystemComplete covers GET /api/v1/filesystem/complete: directory
// completions for a partial path, used by project-registration UIs to browse
// the daemon's local filesystem.
func TestFilesystemComplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(root, "proto"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(root, "other"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(root, "profile.txt"), []byte("x"), 0o644,
	))

	decode := func(partial string) []string {
		resp := httpDo(t, ts, http.MethodGet,
			"/api/v1/filesystem/complete?path="+url.QueryEscape(partial), nil)
		require.Equal(http.StatusOK, resp.StatusCode)
		var body struct {
			Completions []string `json:"completions"`
		}
		require.NoError(json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()
		return body.Completions
	}

	// Prefix completion matches directories only, never files.
	completions := decode(filepath.Join(root, "pro"))
	assert.ElementsMatch([]string{
		filepath.Join(root, "projects") + "/",
		filepath.Join(root, "proto") + "/",
	}, completions)

	// A trailing slash lists every directory inside.
	completions = decode(root + "/")
	assert.Len(completions, 3)

	// Nonexistent and unreadable paths complete to nothing, not errors.
	completions = decode(filepath.Join(root, "missing", "x"))
	assert.Empty(completions)
}

// TestFilesystemValidateRepo covers GET /api/v1/filesystem/validate-repo:
// resolving an arbitrary path to a registerable repository root.
func TestFilesystemValidateRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	assert := assert.New(t)

	srv, _ := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(os.MkdirAll(repo, 0o755))
	gitfixture.Run(t, repo, "init", "--initial-branch=main")
	gitfixture.Run(t, repo, "config", "user.email", "test@example.com")
	gitfixture.Run(t, repo, "config", "user.name", "Test User")
	gitfixture.Run(t, repo, "commit", "--allow-empty", "-m", "initial")
	require.NoError(os.MkdirAll(filepath.Join(repo, "subdir"), 0o755))

	decode := func(path string) (bool, string, string) {
		resp := httpDo(t, ts, http.MethodGet,
			"/api/v1/filesystem/validate-repo?path="+url.QueryEscape(path), nil)
		require.Equal(http.StatusOK, resp.StatusCode)
		var body struct {
			IsValid  bool   `json:"is_valid"`
			RootPath string `json:"root_path"`
			Message  string `json:"message"`
		}
		require.NoError(json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()
		return body.IsValid, body.RootPath, body.Message
	}

	valid, rootPath, _ := decode(repo)
	assert.True(valid)
	assert.Equal(filepath.Base(repo), filepath.Base(rootPath))

	// A subdirectory inside the checkout resolves to the repository root
	// so it cannot register as its own project.
	valid, rootPath, _ = decode(filepath.Join(repo, "subdir"))
	assert.True(valid)
	assert.Equal(filepath.Base(repo), filepath.Base(rootPath))

	valid, _, message := decode(t.TempDir())
	assert.False(valid)
	assert.Equal("Not a git repository", message)

	valid, _, message = decode(filepath.Join(t.TempDir(), "missing"))
	assert.False(valid)
	assert.Equal("Directory not found", message)
}
