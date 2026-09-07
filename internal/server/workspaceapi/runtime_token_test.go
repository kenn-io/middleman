package workspaceapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
	"go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestWorkspaceRuntimeLaunchMissingTokenReturnsBadRequestE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	database := dbtest.Open(t)
	manager := workspace.NewManager(database, filepath.Join(dir, "worktrees"))
	runtime := localruntime.NewManager(localruntime.Options{
		Targets: []localruntime.LaunchTarget{{
			Key:       "tokenfail",
			Label:     "Token fail",
			Kind:      localruntime.LaunchTargetAgent,
			Available: true,
			Command:   []string{"/bin/true"},
		}},
		PtyOwnerRuntime: missingTokenRuntimePtyOwner{},
	})
	h := New(Deps{DB: database, Workspaces: manager, Runtime: runtime})
	h.Start(t.Context(), true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(h.Shutdown(ctx))
	})
	seedReadyWorkspaceForRuntimeTokenTest(t, database, filepath.Join(dir, "workspace"))

	mux := http.NewServeMux()
	api := humago.NewWithPrefix(mux, "/api/v1", huma.DefaultConfig("workspace test", "1"))
	h.Register(api)
	body := bytes.NewBufferString(`{"target_key":"tokenfail"}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/workspaces/ws-runtime-token/runtime/sessions",
		body,
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(http.StatusBadRequest, rec.Code)
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(json.NewDecoder(rec.Body).Decode(&problem))
	assert.Equal("badRequest", problem.Code)
}

type missingTokenRuntimePtyOwner struct{}

func (missingTokenRuntimePtyOwner) HasState(string) bool { return false }

func (missingTokenRuntimePtyOwner) Attach(context.Context, string) (ptyownerruntime.PTY, error) {
	return nil, errors.New("unexpected attach")
}

func (missingTokenRuntimePtyOwner) Start(
	context.Context, string, string, []string, []string, map[string]string,
) (ptyownerruntime.PTY, error) {
	return nil, fmt.Errorf("resolve runtime token: %w", tokenauth.ErrMissingToken)
}

func (missingTokenRuntimePtyOwner) Stop(context.Context, string) error { return nil }

func seedReadyWorkspaceForRuntimeTokenTest(
	t *testing.T, database *db.DB, worktreePath string,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	runTokenRuntimeTestGit(t, worktreePath, "init", "--initial-branch=main")
	runTokenRuntimeTestGit(t, worktreePath, "config", "user.email", "test@example.test")
	runTokenRuntimeTestGit(t, worktreePath, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "README.md"), []byte("# Test\n"), 0o644))
	runTokenRuntimeTestGit(t, worktreePath, "add", "README.md")
	runTokenRuntimeTestGit(t, worktreePath, "commit", "-m", "initial")
	require.NoError(t, database.InsertWorkspace(t.Context(), &db.Workspace{
		ID:              "ws-runtime-token",
		Platform:        string(platform.KindGitHub),
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeAdHoc,
		ItemKey:         db.AdHocWorkspaceItemKey("feature"),
		GitHeadRef:      "feature",
		WorkspaceBranch: "feature",
		WorktreePath:    worktreePath,
		Status:          "ready",
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
	}))
}

func runTokenRuntimeTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
}
