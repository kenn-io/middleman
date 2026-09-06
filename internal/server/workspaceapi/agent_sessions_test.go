package workspaceapi

import (
	"encoding/json"
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
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/ptyowner"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func TestListWorkspaceAgentSessionsProjectsOnlySupportedLiveAgentReports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("KENN_FORGE_AGENT_SESSION_HELPER", "1")
	ctx := t.Context()
	database := dbtest.Open(t)
	worktree := t.TempDir()
	workspaceID := "ws-agent-sessions"
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID:              workspaceID,
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widgets",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/agent-sessions",
		WorkspaceBranch: "feature/agent-sessions",
		WorktreePath:    worktree,
		TmuxSession:     "kenn-forge-agent-sessions",
		Status:          "ready",
	}))

	helperCommand := []string{
		os.Args[0], "-test.run=^TestWorkspaceAgentSessionHelper$", "--", "sleep",
	}
	runtime := localruntime.NewManager(localruntime.Options{
		Targets: []localruntime.LaunchTarget{
			{
				Key: "custom-worker", Label: "Custom worker", Kind: localruntime.LaunchTargetAgent,
				Source: "test", Command: helperCommand, Available: true,
			},
			{
				Key: "shell", Label: "Shell", Kind: localruntime.LaunchTargetShell,
				Source: "test", Command: helperCommand, Available: true,
			},
		},
		PtyOwnerRuntime: ptyownerruntime.New(&ptyowner.Client{
			Root: filepath.Join(t.TempDir(), "pty-owner"), InProcess: true,
		}, nil),
	})
	t.Cleanup(func() {
		runtime.StopWorkspace(t.Context(), workspaceID)
		runtime.Shutdown()
	})
	agentRuntime, err := runtime.Launch(ctx, workspaceID, worktree, "custom-worker")
	require.NoError(err)
	shellRuntime, err := runtime.Launch(ctx, workspaceID, worktree, "shell")
	require.NoError(err)

	activity := agentactivity.NewStore(t.TempDir())
	require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
		SessionID: "shared-session", CWD: worktree,
		HookEventName: "UserPromptSubmit",
	}, agentRuntime.Key))
	require.NoError(activity.HandleEvent("claude", agentactivity.HookEvent{
		SessionID: "shared-session", CWD: worktree,
		HookEventName: "Stop",
	}, agentRuntime.Key))
	require.NoError(activity.HandleEvent("opencode", agentactivity.HookEvent{
		SessionID: "unsupported", CWD: worktree,
		HookEventName: "UserPromptSubmit",
	}, agentRuntime.Key))
	require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
		SessionID: "shell", CWD: worktree,
		HookEventName: "UserPromptSubmit",
	}, shellRuntime.Key))
	require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
		SessionID: "dead", CWD: worktree,
		HookEventName: "UserPromptSubmit",
	}, "dead-runtime"))
	require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
		SessionID: "wrong-cwd", CWD: t.TempDir(),
		HookEventName: "UserPromptSubmit",
	}, agentRuntime.Key))

	require.NoError(database.UpsertWorkspaceRuntimeSession(ctx, &db.WorkspaceRuntimeSession{
		WorkspaceID: workspaceID, SessionKey: agentRuntime.Key,
		TargetKey: "custom-worker", Label: "Custom worker", Kind: "agent", Scope: "session",
		CreatedAt: agentRuntime.CreatedAt,
	}))
	handler := New(Deps{
		DB: database, Workspaces: workspace.NewManager(database, t.TempDir()),
		Runtime: runtime, AgentActivity: activity,
	})
	_, reserved := handler.reserveInitialMessageAttempt(
		workspaceID, agentRuntime.Key,
		initialMessageAttempt{TargetKey: "custom-worker", Message: "review this!"},
	)
	require.True(reserved)
	handler.finishInitialMessageAttempt(workspaceID, agentRuntime.Key, initialMessageDelivered)
	mux := http.NewServeMux()
	api := humago.NewWithPrefix(
		mux, "/api/v1", huma.DefaultConfig("workspace agent session test", "1"),
	)
	handler.Register(api)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/agent-sessions", nil,
	))
	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Sessions []struct {
			Agent             string              `json:"agent"`
			SessionID         string              `json:"session_id"`
			RuntimeSessionKey string              `json:"runtime_session_key"`
			TargetKey         string              `json:"target_key"`
			State             agentactivity.State `json:"state"`
			UpdatedAt         time.Time           `json:"updated_at"`
			InitialMessage    *struct {
				State        string     `json:"state"`
				MessageBytes int        `json:"message_bytes"`
				DeliveredAt  *time.Time `json:"delivered_at"`
			} `json:"initial_message,omitempty"`
		} `json:"sessions"`
	}
	require.NoError(json.NewDecoder(recorder.Body).Decode(&response))
	require.Len(response.Sessions, 2)
	assert.Equal("claude", response.Sessions[0].Agent)
	assert.Equal(agentactivity.StateDone, response.Sessions[0].State)
	require.NotNil(response.Sessions[0].InitialMessage)
	assert.Equal(initialMessageDelivered, response.Sessions[0].InitialMessage.State)
	assert.Equal("codex", response.Sessions[1].Agent)
	assert.Equal("shared-session", response.Sessions[1].SessionID)
	assert.Equal(agentRuntime.Key, response.Sessions[1].RuntimeSessionKey)
	assert.Equal("custom-worker", response.Sessions[1].TargetKey)
	assert.Equal(agentactivity.StateWorking, response.Sessions[1].State)
	assert.Equal(time.UTC, response.Sessions[1].UpdatedAt.Location())
	require.NotNil(response.Sessions[1].InitialMessage)
	assert.Equal(initialMessageDelivered, response.Sessions[1].InitialMessage.State)
	assert.Equal(12, response.Sessions[1].InitialMessage.MessageBytes)
	require.NotNil(response.Sessions[1].InitialMessage.DeliveredAt)
	assert.Equal(time.UTC, response.Sessions[1].InitialMessage.DeliveredAt.Location())

	// Item lists use the same aggregate state and live-runtime filtering.
	summary := &db.WorkspaceSummary{ID: workspaceID, WorktreePath: worktree, Status: "ready"}
	ref := handler.workspaceReference(summary)
	require.NotNil(ref.AgentState)
	assert.Equal("working", *ref.AgentState)
	runtime.StopWorkspace(ctx, workspaceID)
	assert.Nil(handler.workspaceReference(summary).AgentState)
}

func TestWorkspaceAgentSessionHelper(t *testing.T) {
	if os.Getenv("KENN_FORGE_AGENT_SESSION_HELPER") != "1" {
		return
	}
	args := os.Args
	mode := ""
	for index, arg := range args {
		if arg == "--" && index+1 < len(args) {
			mode = args[index+1]
			break
		}
	}
	if mode != "sleep" {
		_, _ = fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
	time.Sleep(time.Hour)
}
