package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendAgentMessageSubmitsToExistingRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	submittedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	var got AgentMessageRequest
	backend := &fakeBackend{submitAgentMessageFn: func(
		_ context.Context, request AgentMessageRequest,
	) (AgentMessageResult, error) {
		got = request
		return AgentMessageResult{
			TargetKey: "codex", MessageBytes: 10, SubmittedAt: submittedAt,
		}, nil
	}}
	s := newMCPTestServer(t, backend)

	out, err := s.sendAgentMessage(t.Context(), sendAgentMessageInput{
		WorkspaceID: " ws-1 ", RuntimeSessionKey: " runtime-1 ", Message: "keep going",
	})

	require.NoError(err)
	assert.Equal(AgentMessageRequest{
		WorkspaceID: "ws-1", RuntimeSessionKey: "runtime-1", Message: "keep going",
	}, got)
	assert.Equal("ws-1", out.WorkspaceID)
	assert.Equal("runtime-1", out.RuntimeSessionKey)
	assert.Equal("codex", out.TargetKey)
	assert.Equal(10, out.MessageBytes)
	assert.Equal("2026-09-03T12:00:00Z", out.SubmittedAt)
}
