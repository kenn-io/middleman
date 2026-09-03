package workspacetest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/testutil/servertest"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func setupProjectServer(t *testing.T) (*server.Server, *db.DB) {
	t.Helper()
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(
		t, database, syncer, nil, "/", nil,
		server.ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
	return srv, database
}

func setupProjectServerWithConfigContent(
	t *testing.T, content string, _ any,
) (*server.Server, *db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(
		t, database, syncer, nil, nil, cfg, path,
		server.ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
	return srv, database, path
}

func initLocalOnlyGitRepo(ctx context.Context, dir string) error {
	if _, _, err := gitcmd.New().Run(ctx, dir, nil, "init", "--initial-branch=main"); err != nil {
		return err
	}
	return nil
}

func initLifecycleRouteRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitfixture.Run(t, dir, "init", "--initial-branch=main")
	gitfixture.Run(t, dir, "config", "user.email", "test@example.com")
	gitfixture.Run(t, dir, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644))
	gitfixture.Run(t, dir, "add", "README.md")
	gitfixture.Run(t, dir, "commit", "-m", "initial")
	return dir
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	require.NoError(t, err)
	return payload
}

func httpDo(
	t *testing.T, ts *httptest.Server, method, path string, body []byte,
) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, strings.NewReader(string(body)))
	require.NoError(t, err)
	if body != nil || method == http.MethodPost || method == http.MethodDelete ||
		method == http.MethodPut || method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func listWorktreeRows(
	t *testing.T, ts *httptest.Server, projectID string,
) []map[string]any {
	t.Helper()
	resp := httpDo(t, ts, http.MethodGet, "/api/v1/projects/"+projectID+"/worktrees", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()
	var body struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Worktrees
}

func worktreeRowByBranch(rows []map[string]any, branch string) map[string]any {
	for _, row := range rows {
		if row["branch"] == branch {
			return row
		}
	}
	return nil
}

func worktreeRowByPathBase(rows []map[string]any, base string) map[string]any {
	for _, row := range rows {
		path, _ := row["path"].(string)
		if filepath.Base(path) == base {
			return row
		}
	}
	return nil
}
