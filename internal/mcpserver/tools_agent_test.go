package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListAgentTargetsIncludesCustomAgentsWithoutCommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakeBackend{listLaunchTargetsFn: func(context.Context) ([]LaunchTarget, error) {
		return []LaunchTarget{
			{Key: "gemini", Label: "Gemini", Kind: "agent", Source: "builtin", Available: true},
			{Key: "opencode", Label: "OpenCode", Kind: "agent", Source: "builtin", Available: true},
			{Key: "claude", Label: "Claude", Kind: "shell", Source: "config", Available: true},
			{Key: "codex", Label: "Codex", Kind: "agent", Source: "config", DisabledReason: "disabled by config"},
			{Key: "custom", Label: "Custom", Kind: "agent", Source: "config", Available: true},
		}, nil
	}}
	s := newMCPTestServer(t, backend)

	out, err := s.listAgentTargets(t.Context(), listAgentTargetsInput{})

	require.NoError(err)
	require.Len(out.Targets, 4)
	assert.Equal("codex", out.Targets[0].Key)
	assert.False(out.Targets[0].Available)
	assert.Equal("custom", out.Targets[1].Key)
	assert.Equal("gemini", out.Targets[2].Key)
	assert.Equal("opencode", out.Targets[3].Key)
	raw, err := json.Marshal(out)
	require.NoError(err)
	assert.NotContains(string(raw), "command")
}

func TestListWorkspaceAgentSessionsMapsLiveProjectionDeterministically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	deliveredAt := time.Date(2026, 8, 7, 14, 59, 1, 0, time.UTC)
	var workspaceID string
	backend := &fakeBackend{listWorkspaceAgentSessionsFn: func(
		_ context.Context, id string,
	) ([]WorkspaceAgentSession, error) {
		workspaceID = id
		return []WorkspaceAgentSession{
			{
				Agent: "codex", SessionID: "session-b", RuntimeSessionKey: "runtime-b",
				TargetKey: "codex", State: "done",
				UpdatedAt: time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC),
			},
			{
				Agent: "claude", SessionID: "session-a", RuntimeSessionKey: "runtime-a",
				TargetKey: "claude", State: "working",
				UpdatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
				InitialMessage: &InitialMessageStatus{
					State: "delivered", MessageBytes: 12, DeliveredAt: &deliveredAt,
				},
			},
		}, nil
	}}
	backend.getWorkspaceRuntimeFn = func(context.Context, string) (WorkspaceRuntime, error) {
		return WorkspaceRuntime{Sessions: []RuntimeSession{
			{
				Key: "runtime-b", TargetKey: "codex", Kind: "agent", Status: "running",
				CreatedAt: time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC),
			},
			{
				Key: "runtime-a", TargetKey: "claude", Kind: "agent", Status: "running",
				CreatedAt: time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC),
			},
		}}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.listWorkspaceAgentSessions(
		t.Context(), listWorkspaceAgentSessionsInput{WorkspaceID: "ws-1"},
	)

	require.NoError(err)
	assert.Equal("ws-1", workspaceID)
	require.Len(out.Sessions, 2)
	assert.Equal("claude", out.Sessions[0].Agent)
	require.NotNil(out.Sessions[0].InitialMessage)
	assert.Equal("delivered", out.Sessions[0].InitialMessage.State)
	assert.Equal("2026-08-07T14:59:01Z", out.Sessions[0].InitialMessage.DeliveredAt)
	assert.Equal("codex", out.Sessions[1].Agent)
	require.Len(out.Runtimes, 2)
	assert.True(out.Runtimes[0].HookObserved)
	assert.True(out.Runtimes[1].HookObserved)
}

func TestListWorkspaceAgentSessionsShowsRuntimeBeforeFirstHook(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakeBackend{
		getWorkspaceRuntimeFn: func(context.Context, string) (WorkspaceRuntime, error) {
			return WorkspaceRuntime{Sessions: []RuntimeSession{
				{
					Key: "runtime-waiting", TargetKey: "codex", Kind: "agent", Status: "running",
					CreatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
				},
				{Key: "shell", TargetKey: "shell", Kind: "shell", Status: "running"},
			}}, nil
		},
		listWorkspaceAgentSessionsFn: func(context.Context, string) ([]WorkspaceAgentSession, error) {
			return nil, nil
		},
	}
	s := newMCPTestServer(t, backend)

	out, err := s.listWorkspaceAgentSessions(
		t.Context(), listWorkspaceAgentSessionsInput{WorkspaceID: "ws-1"},
	)

	require.NoError(err)
	require.Len(out.Runtimes, 1)
	assert.Equal("runtime-waiting", out.Runtimes[0].RuntimeSessionKey)
	assert.False(out.Runtimes[0].HookObserved)
	assert.Empty(out.Sessions)
}
