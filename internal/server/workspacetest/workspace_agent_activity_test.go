package workspacetest

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestWorkspaceAgentActivityFlowsThroughHTTPResponsesE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		Agents: []config.Agent{{
			Key: "hook-agent", Label: "Hook agent",
			Command: []string{
				"/bin/sh", "-c",
				"printf '\033[?2004h'; while IFS= read -r _; do :; done",
			},
		}},
		Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions},
	}
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launch, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{TargetKey: "hook-agent"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launch.StatusCode())
	require.NotNil(launch.JSON200)

	reportHook := func(agent, sessionID, runtimeKey, cwd, event string) {
		t.Helper()
		response, hookErr := fixture.client.HTTP.ReceiveAgentHookWithResponse(
			ctx, agent,
			&generated.ReceiveAgentHookParams{
				XKennForgeRuntimeSessionKey: &runtimeKey,
			},
			generated.HookEvent{
				SessionId: sessionID, Cwd: cwd, HookEventName: event,
			},
		)
		require.NoError(hookErr)
		require.Equal(http.StatusOK, response.StatusCode(), string(response.Body))
	}
	for _, report := range []struct {
		sessionID  string
		runtimeKey string
		event      string
	}{
		{sessionID: "wrong-agent", runtimeKey: "wrong-runtime", event: "PermissionRequest"},
		{sessionID: "wrong-worktree", runtimeKey: launch.JSON200.Key, event: "PermissionRequest"},
		{sessionID: "live-agent", runtimeKey: launch.JSON200.Key, event: "UserPromptSubmit"},
	} {
		cwd := ws.WorktreePath
		if report.sessionID == "wrong-worktree" {
			cwd = t.TempDir()
		}
		reportHook("CoDeX", report.sessionID, report.runtimeKey, cwd, report.event)
	}

	sessionsResponse, err := fixture.client.HTTP.ListWorkspaceAgentSessionsWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, sessionsResponse.StatusCode(), string(sessionsResponse.Body))
	require.NotNil(sessionsResponse.JSON200)
	require.NotNil(sessionsResponse.JSON200.Sessions)
	require.Len(sessionsResponse.JSON200.Sessions, 1)
	assert.Equal("codex", sessionsResponse.JSON200.Sessions[0].Agent)
	assert.Equal("live-agent", sessionsResponse.JSON200.Sessions[0].SessionId)
	assert.Equal(launch.JSON200.Key, sessionsResponse.JSON200.Sessions[0].RuntimeSessionKey)

	messageResponse, err := fixture.client.HTTP.SubmitWorkspaceRuntimeSessionInitialMessageWithResponse(
		ctx, ws.Id, launch.JSON200.Key,
		generated.SubmitInitialMessageInputBody{
			TargetKey: "hook-agent", Message: "review this",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, messageResponse.StatusCode(), string(messageResponse.Body))
	require.NotNil(messageResponse.JSON200)
	assert.Equal("hook-agent", messageResponse.JSON200.TargetKey)
	assert.Equal("delivered", messageResponse.JSON200.State)
	assert.Equal(int64(11), messageResponse.JSON200.MessageBytes)
	assert.Equal(time.UTC, messageResponse.JSON200.ReservedAt.Location())
	require.NotNil(messageResponse.JSON200.DeliveredAt)
	assert.Equal(time.UTC, messageResponse.JSON200.DeliveredAt.Location())

	sessionsResponse, err = fixture.client.HTTP.ListWorkspaceAgentSessionsWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, sessionsResponse.StatusCode(), string(sessionsResponse.Body))
	require.NotNil(sessionsResponse.JSON200)
	require.NotNil(sessionsResponse.JSON200.Sessions)
	require.Len(sessionsResponse.JSON200.Sessions, 1)
	assert.Nil(sessionsResponse.JSON200.Sessions[0].InitialMessage)

	getResponse, err := fixture.client.HTTP.GetWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, getResponse.StatusCode())
	require.NotNil(getResponse.JSON200)
	require.NotNil(getResponse.JSON200.AgentState)
	require.NotNil(getResponse.JSON200.AgentStateUpdatedAt)
	assert.Equal(generated.Working, *getResponse.JSON200.AgentState)
	assert.Equal(time.UTC, getResponse.JSON200.AgentStateUpdatedAt.Location())

	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "activity.txt"), []byte("activity\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "agent-activity@example.invalid")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Agent Activity Fixture")
	gitfixture.Run(t, ws.WorktreePath, "add", "activity.txt")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "add activity fixture")
	pushResponse, err := fixture.client.HTTP.PushWorkspaceBranchWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, pushResponse.StatusCode(), string(pushResponse.Body))
	require.NotNil(pushResponse.JSON200)
	require.NotNil(pushResponse.JSON200.AgentState)
	assert.Equal(generated.Working, *pushResponse.JSON200.AgentState)

	reportHook("codex", "live-agent", launch.JSON200.Key, ws.WorktreePath, "Stop")
	getResponse, err = fixture.client.HTTP.GetWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, getResponse.StatusCode())
	require.NotNil(getResponse.JSON200)
	require.NotNil(getResponse.JSON200.AgentState)
	assert.Equal(generated.Done, *getResponse.JSON200.AgentState)

	stopResponse, err := fixture.client.HTTP.StopWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id, launch.JSON200.Key,
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, stopResponse.StatusCode())
	sessionsResponse, err = fixture.client.HTTP.ListWorkspaceAgentSessionsWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, sessionsResponse.StatusCode(), string(sessionsResponse.Body))
	require.NotNil(sessionsResponse.JSON200)
	require.NotNil(sessionsResponse.JSON200.Sessions)
	assert.Empty(sessionsResponse.JSON200.Sessions)

	getResponse, err = fixture.client.HTTP.GetWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, getResponse.StatusCode())
	require.NotNil(getResponse.JSON200)
	assert.Nil(getResponse.JSON200.AgentState)
}
