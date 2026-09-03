package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisteredToolsResourcesAndPromptsAreCurated(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := newMCPTestServer(t, &fakeBackend{})
	cs := connectMCPTestSession(t, s)

	tools, err := cs.ListTools(t.Context(), nil)
	require.NoError(err)
	var toolNames []string
	for _, tool := range tools.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	slices.Sort(toolNames)
	assert.Equal([]string{
		"kenn_forge_find_review_candidates",
		"kenn_forge_get_item_context",
		"kenn_forge_get_item_diff",
		"kenn_forge_get_stack_context",
		"kenn_forge_list_activity",
		"kenn_forge_list_agent_targets",
		"kenn_forge_list_items_by_workflow_state",
		"kenn_forge_list_repos",
		"kenn_forge_list_workspace_agent_sessions",
		"kenn_forge_search_items",
		"kenn_forge_send_agent_message",
		"kenn_forge_set_item_workflow_state",
		"kenn_forge_spawn_workspace_with_agent",
	}, toolNames)

	resources, err := cs.ListResources(t.Context(), nil)
	require.NoError(err)
	require.Len(resources.Resources, 1)
	assert.Equal("kenn-forge://mcp/guidance", resources.Resources[0].URI)
	read, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "kenn-forge://mcp/guidance"})
	require.NoError(err)
	require.Len(read.Contents, 1)
	assert.Contains(read.Contents[0].Text, "kenn_forge_find_review_candidates")

	prompts, err := cs.ListPrompts(t.Context(), nil)
	require.NoError(err)
	require.Len(prompts.Prompts, 1)
	prompt, err := cs.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "kenn-forge-review-candidates"})
	require.NoError(err)
	require.Len(prompt.Messages, 1)
	content, ok := prompt.Messages[0].Content.(*mcp.TextContent)
	require.True(ok)
	assert.Contains(content.Text, "kenn_forge_get_item_diff")
	assert.Contains(content.Text, "expected_status")
}

func TestServerUses20260728ProtocolCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := newMCPTestServer(t, &fakeBackend{})

	initialized := connectMCPTestSession(t, s).InitializeResult()

	require.NotNil(initialized)
	assert.Equal("2026-07-28", initialized.ProtocolVersion)
	//nolint:staticcheck // Verify the 2026-07-28 server does not advertise deprecated logging.
	assert.Nil(initialized.Capabilities.Logging)
	require.NotNil(initialized.Capabilities.Tools)
	assert.False(initialized.Capabilities.Tools.ListChanged)
	require.NotNil(initialized.Capabilities.Prompts)
	assert.False(initialized.Capabilities.Prompts.ListChanged)
	require.NotNil(initialized.Capabilities.Resources)
	assert.False(initialized.Capabilities.Resources.ListChanged)
	assert.False(initialized.Capabilities.Resources.Subscribe)
}

func TestToolErrorsPreserveStructuredEvidenceThroughClientSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakeBackend{listRepositoriesFn: func(context.Context) ([]RepositorySummary, error) {
		return nil, &Error{
			Kind: "agent_handoff_failed", Code: "runtime_launch_failed",
			Message: "agent launch outcome is unknown", Ambiguous: true,
			Details: map[string]any{
				"workspace_id": "ws-1", "runtime_session_key": "runtime-1",
				"failed_stage": "message_delivered",
			},
		}
	}}
	s := newMCPTestServer(t, backend)
	result, err := connectMCPTestSession(t, s).CallTool(
		t.Context(), &mcp.CallToolParams{Name: "kenn_forge_list_repos"},
	)

	require.NoError(err)
	require.NotNil(result)
	assert.True(result.IsError)
	type evidence struct {
		Kind      string         `json:"kind"`
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		Retryable bool           `json:"retryable"`
		Ambiguous bool           `json:"ambiguous"`
		Details   map[string]any `json:"details"`
	}
	metadata, found := result.Meta[toolErrorMetaKey]
	require.True(found)
	data, err := json.Marshal(metadata)
	require.NoError(err)
	var got evidence
	require.NoError(json.Unmarshal(data, &got))
	assert.Equal("agent_handoff_failed", got.Kind)
	assert.Equal("runtime_launch_failed", got.Code)
	assert.Equal("agent launch outcome is unknown", got.Message)
	assert.False(got.Retryable)
	assert.True(got.Ambiguous)
	assert.Equal("ws-1", got.Details["workspace_id"])
	assert.Equal("runtime-1", got.Details["runtime_session_key"])
	assert.Equal("message_delivered", got.Details["failed_stage"])

	require.Len(result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(ok)
	var payload struct {
		Error evidence `json:"error"`
	}
	require.NoError(json.Unmarshal([]byte(text.Text), &payload))
	assert.Equal(got, payload.Error)
}

func TestSpawnToolFailurePreservesPartialHandoffEvidenceThroughClientSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-1", "runtime-1", "coding-1")
	backend.launchWorkspaceRuntimeFn = func(context.Context, string, string) (RuntimeSession, error) {
		return RuntimeSession{}, &Error{
			Kind: "unavailable", Code: "runtimeLaunchFailed",
			Message: "agent runtime failed to launch", Retryable: true,
		}
	}
	s := newMCPTestServer(t, backend)

	result, err := connectMCPTestSession(t, s).CallTool(
		t.Context(), &mcp.CallToolParams{
			Name: "kenn_forge_spawn_workspace_with_agent",
			Arguments: map[string]any{
				"source": map[string]any{
					"type": "item",
					"item": map[string]any{
						"type": "pr", "provider": "github", "platform_host": "github.com",
						"platform_repo_id": "repo-acme-widget",
						"owner":            "acme", "name": "widget", "number": 42,
					},
				},
				"agent_target":    "codex",
				"initial_message": "review this",
				"timeout":         "2s",
			},
		},
	)

	require.NoError(err)
	require.NotNil(result)
	assert.True(result.IsError)

	metadata, found := result.Meta[toolErrorMetaKey]
	require.True(found)
	raw, err := json.Marshal(metadata)
	require.NoError(err)
	var evidence toolErrorEvidence
	require.NoError(json.Unmarshal(raw, &evidence))
	assert.Equal("unavailable", evidence.Kind)
	assert.Equal("runtimeLaunchFailed", evidence.Code)
	assert.Equal("runtime_launched", evidence.Details["failed_stage"])
	assert.Equal("workspace_ready", evidence.Details["last_completed_stage"])
	assert.Equal("ws-1", evidence.Details["workspace_id"])
	assert.Equal("ready", evidence.Details["workspace_status"])

	// The partial handoff output survives alongside the structured error so
	// clients can locate the workspace that was already created.
	structured, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	var partial struct {
		Stage     string `json:"stage"`
		Workspace struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"workspace"`
	}
	require.NoError(json.Unmarshal(structured, &partial))
	assert.Equal("workspace_ready", partial.Stage)
	assert.Equal("ws-1", partial.Workspace.ID)
	assert.Equal("ready", partial.Workspace.Status)
}

func TestHTTPHandlerServesOnlyStatelessMCPPath(t *testing.T) {
	s := newMCPTestServer(t, &fakeBackend{})
	handler := s.HTTPHandler()

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "http://localhost/other", nil))
	assert.Equal(t, http.StatusNotFound, missing.Code)

	mcpResponse := httptest.NewRecorder()
	handler.ServeHTTP(mcpResponse, httptest.NewRequest(http.MethodGet, "http://localhost/mcp", nil))
	assert.NotEqual(t, http.StatusNotFound, mcpResponse.Code)
}

func connectMCPTestSession(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.mcp.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, serverSession.Close()) })
	client := mcp.NewClient(&mcp.Implementation{Name: "kenn-forge-test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientSession.Close()) })
	return clientSession
}
