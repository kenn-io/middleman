package workspaceapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/ptyowner"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func TestRestoreRuntimeSessionsResumesSavedConversationAfterTmuxLoss(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	cwd := t.TempDir()
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID: "workspace", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeAdHoc,
		ItemKey: db.AdHocWorkspaceItemKey("work/resume"), GitHeadRef: "work/resume",
		WorkspaceBranch: "work/resume", WorktreePath: cwd, Status: "ready",
	}))
	require.NoError(database.UpsertWorkspaceRuntimeSession(ctx, &db.WorkspaceRuntimeSession{
		WorkspaceID: "workspace", SessionKey: "saved-runtime", TargetKey: "custom-worker",
		Label: "Worker 2", Kind: "agent", Scope: "session", TmuxSession: "gone",
		DisplayRegion: "workflow",
	}))
	activity := agentactivity.NewStore(t.TempDir())
	require.NoError(activity.HandleEvent("claude", agentactivity.HookEvent{
		SessionID: "saved-conversation", CWD: cwd, HookEventName: "Stop",
	}, "saved-runtime"))
	dir := t.TempDir()
	tmux := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmux, []byte("#!/bin/sh\necho 'no server running on test socket' >&2\nexit 1\n"), 0o755))
	agent := filepath.Join(dir, "agent")
	require.NoError(os.WriteFile(agent, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > args\nexec sleep 60\n"), 0o755))
	runtime := localruntime.NewManager(localruntime.Options{
		TmuxCommand:     []string{tmux},
		Targets:         []localruntime.LaunchTarget{{Key: "custom-worker", Kind: localruntime.LaunchTargetAgent, Available: true, Command: []string{agent, "--model", "model-a"}}},
		PtyOwnerRuntime: ptyownerruntime.New(&ptyowner.Client{Root: t.TempDir(), InProcess: true}, nil),
	})
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runtime.StopWorkspace(cleanupCtx, "workspace")
		runtime.Shutdown()
	})
	handler := New(Deps{DB: database, Workspaces: workspace.NewManager(database, t.TempDir()), Runtime: runtime, AgentActivity: activity})
	require.NoError(handler.RestoreRuntimeSessions(ctx))
	require.Eventually(func() bool { _, err := os.Stat(filepath.Join(cwd, "args")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	args, err := os.ReadFile(filepath.Join(cwd, "args"))
	require.NoError(err)
	assert.Equal("--model\nmodel-a\n--resume\nsaved-conversation\n", string(args))
	sessions := runtime.ListSessions("workspace")
	require.Len(sessions, 1)
	assert.Equal("saved-runtime", sessions[0].Key)
	assert.Equal("Worker 2", sessions[0].Label)
	stored, err := database.ListAllWorkspaceRuntimeSessions(ctx)
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal("workflow", stored[0].DisplayRegion)
	assert.Len(activity.LiveReportsForWorkspace(cwd, []string{"saved-runtime"}), 1)
	require.NoError(handler.RestoreRuntimeSessions(ctx))
	assert.Len(runtime.ListSessions("workspace"), 1)
}
