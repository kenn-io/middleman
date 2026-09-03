package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	mcpserver "go.kenn.io/forge/internal/mcpserver"
	forgeserver "go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestMCPToolsUseTheInProcessForgeBackend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	repoID, pullID := seedPull(t, database, 1, "Cache review")
	_, basePullID := seedPull(t, database, 2, "Base dependency")
	issueID := seedIssue(t, database, 3, "Cache follow-up")
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: pullID, EventType: "issue_comment", Author: "reviewer",
		Body: "please inspect the cache change", CreatedAt: now.Add(-30 * time.Minute),
		DedupeKey: "mcp-e2e-pull-comment",
	}}))
	require.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{{
		IssueID: issueID, EventType: "comment", Author: "triager",
		Body: "cache follow-up needs a look", CreatedAt: now.Add(-20 * time.Minute),
		DedupeKey: "mcp-e2e-issue-comment",
	}}))
	stackID, err := database.UpsertStack(ctx, repoID, 2, "cache-stack")
	require.NoError(err)
	require.NoError(database.ReplaceStackMembers(ctx, stackID, []db.StackMember{
		{StackID: stackID, MergeRequestID: basePullID, Position: 1},
		{StackID: stackID, MergeRequestID: pullID, Position: 2},
	}))

	diffRoot := t.TempDir()
	diffRepo, err := testutil.SetupDiffRepo(ctx, diffRoot, database)
	require.NoError(err)
	_, stderr, err := gitcmd.New().Run(
		ctx, filepath.Join(diffRoot, "workrepo"), nil,
		"update-ref", "refs/pull/1/head", diffRepo.HeadSHA,
	)
	require.NoError(err, string(stderr))

	dataDir := t.TempDir()
	tmuxPath := filepath.Join(dataDir, "fake-tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte(`#!/bin/sh
if [ "${1:-}" = "-u" ]; then shift; fi
case "${1:-}" in
  has-session) exit 1 ;;
  *) exit 0 ;;
esac
`), 0o755))
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		Repos: []config.Repo{{
			Owner: "acme", Name: "widgets", Platform: "github", PlatformHost: "github.com",
		}},
		Agents: []config.Agent{{
			Key: "codex", Label: "Codex",
			Command: []string{
				"/bin/sh", "-c",
				"printf '\033[?2004h'; while IFS= read -r _; do :; done",
			},
		}},
		PullRequests: config.PullRequests{PreferGitHubNativeStacks: true},
		Workspaces:   config.Workspaces{AutoAssignOnCreate: true},
		Tmux: config.Tmux{
			Command: []string{tmuxPath}, AgentSessions: &disableTmuxAgentSessions,
		},
	}
	syncer := ghclient.NewSyncer(
		nil, database, diffRepo.Manager,
		[]ghclient.RepoRef{{
			Platform: "github", RepoID: repoID, PlatformHost: "github.com",
			Owner: "acme", Name: "widgets", RepoPath: "acme/widgets",
		}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	const token = "mcp-e2e-token"
	forge := forgeserver.New(database, syncer, nil, "/", cfg, forgeserver.ServerOptions{
		DaemonAccess: forgeserver.DaemonAccessOptions{Token: token, RequireAPIAuth: true},
		Clones:       diffRepo.Manager, WorktreeDir: t.TempDir(),
		DisableWorkspaceBackgroundMonitors: true,
		DisableWorkspaceEnrichment:         true,
		PtyOwnerInProcess:                  true,
		HostCheck: forgeserver.HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8080"},
		},
		HostCheckAllowLoopbackAnyPort: true,
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(forge.Shutdown(shutdownCtx))
	})

	forgeHTTP := httptest.NewServer(forge)
	t.Cleanup(forgeHTTP.Close)
	companion, err := mcpserver.New(mcpserver.Options{Backend: forge.MCPBackend(), Version: "test"})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(companion.Close()) })
	mcpHTTP := httptest.NewUnstartedServer(nil)
	bind, err := config.ParseHostKey(mcpHTTP.Listener.Addr().String())
	require.NoError(err)
	mcpHTTP.Config.Handler = forgeserver.NewMCPHTTPGuard(
		companion.HTTPHandler(),
		forgeserver.MCPHTTPGuardOptions{Bind: bind, Token: token, RequireAuth: true},
	)
	mcpHTTP.Start()
	t.Cleanup(mcpHTTP.Close)
	session := connectHTTPMCP(t, mcpHTTP.URL+"/mcp", token)
	t.Cleanup(func() {
		if err := session.Close(); err != nil && !strings.Contains(err.Error(), "EOF") {
			require.NoError(err)
		}
	})

	repos := callTool[struct {
		Repos []struct {
			Provider       string `json:"provider"`
			PlatformRepoID string `json:"platform_repo_id"`
			RepoPath       string `json:"repo_path"`
		} `json:"repos"`
	}](t, session, "kenn_forge_list_repos", map[string]any{})
	require.Len(repos.Repos, 1)
	assert.Equal("github", repos.Repos[0].Provider)
	assert.Equal("repo-acme-widgets", repos.Repos[0].PlatformRepoID)
	assert.Equal("acme/widgets", repos.Repos[0].RepoPath)

	candidates := callTool[struct {
		Candidates []struct {
			Item struct {
				Type   string `json:"type"`
				Number int    `json:"number"`
			} `json:"item"`
			Stack struct {
				Present bool `json:"present"`
			} `json:"stack"`
		} `json:"candidates"`
	}](t, session, "kenn_forge_find_review_candidates", map[string]any{
		"since": "2h", "item_types": []string{"pr", "issue"},
		"repo": map[string]any{
			"provider": "github", "platform_host": "github.com",
			"platform_repo_id": "repo-acme-widgets", "repo_path": "acme/widgets",
		},
	})
	require.Len(candidates.Candidates, 3)
	var foundIssue, foundRequestedPull bool
	for _, candidate := range candidates.Candidates {
		if candidate.Item.Type == "issue" && candidate.Item.Number == 3 {
			foundIssue = true
		}
		if candidate.Item.Type == "pr" && candidate.Item.Number == 1 {
			foundRequestedPull = candidate.Stack.Present
		}
	}
	assert.True(foundIssue)
	assert.True(foundRequestedPull)

	item := map[string]any{
		"type": "pr", "provider": "github", "platform_host": "github.com",
		"platform_repo_id": "repo-acme-widgets",
		"owner":            "acme", "name": "widgets", "number": 1,
	}
	contextResult := callTool[struct {
		Item struct {
			Title string `json:"title"`
		} `json:"item"`
		Workflow struct {
			Status string `json:"status"`
		} `json:"workflow"`
		Events []struct {
			Author string `json:"author"`
		} `json:"events"`
	}](t, session, "kenn_forge_get_item_context", map[string]any{
		"item": item, "event_limit": 1,
	})
	assert.Equal("Cache review", contextResult.Item.Title)
	assert.Equal("new", contextResult.Workflow.Status)
	require.Len(contextResult.Events, 1)
	assert.Equal("reviewer", contextResult.Events[0].Author)

	claim := callTool[struct {
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		UpdatedSource  string `json:"updated_source"`
	}](t, session, "kenn_forge_set_item_workflow_state", map[string]any{
		"item": item, "status": "reviewing", "expected_status": "new",
		"actor": "mcp-e2e", "reason": "reviewing cache change",
	})
	assert.Equal("new", claim.PreviousStatus)
	assert.Equal("reviewing", claim.Status)
	assert.Equal("mcp", claim.UpdatedSource)
	storedWorkflow, err := database.GetItemWorkflowState(ctx, repoID, db.ItemTypePR, 1)
	require.NoError(err)
	require.NotNil(storedWorkflow)
	assert.Equal("reviewing", storedWorkflow.Status)

	existingWorkspace, err := database.GetWorkspaceByMRForProvider(
		ctx, "github", "github.com", "acme", "widgets", 1,
	)
	require.NoError(err)
	require.Nil(existingWorkspace)
	type toolCall struct {
		result *mcp.CallToolResult
		err    error
	}
	spawnDone := make(chan toolCall, 1)
	go func() {
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "kenn_forge_spawn_workspace_with_agent",
			Arguments: map[string]any{
				"source":       map[string]any{"type": "item", "item": item},
				"agent_target": "codex", "initial_message": "review cache change", "timeout": "5s",
			},
		})
		spawnDone <- toolCall{result: result, err: callErr}
	}()

	var workspace *db.Workspace
	require.Eventually(func() bool {
		workspace, err = database.GetWorkspaceByMRForProvider(
			ctx, "github", "github.com", "acme", "widgets", 1,
		)
		return err == nil && workspace != nil && workspace.Status == "ready"
	}, 5*time.Second, 10*time.Millisecond)
	var runtimeSession db.WorkspaceRuntimeSession
	require.Eventually(func() bool {
		sessions, listErr := database.ListWorkspaceRuntimeSessions(ctx, workspace.ID)
		if listErr != nil || len(sessions) != 1 {
			return false
		}
		runtimeSession = sessions[0]
		return runtimeSession.TargetKey == "codex"
	}, 5*time.Second, 10*time.Millisecond)

	hookBody, err := json.Marshal(map[string]any{
		"session_id": "coding-e2e", "cwd": workspace.WorktreePath,
		"hook_event_name": "UserPromptSubmit",
	})
	require.NoError(err)
	hookRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, forgeHTTP.URL+"/api/v1/agent-hooks/codex", bytes.NewReader(hookBody),
	)
	require.NoError(err)
	hookRequest.Header.Set("Authorization", "Bearer "+token)
	hookRequest.Header.Set("Content-Type", "application/json")
	hookRequest.Header.Set("X-Kenn-Forge-Runtime-Session-Key", runtimeSession.SessionKey)
	hookResponse, err := forgeHTTP.Client().Do(hookRequest)
	require.NoError(err)
	require.Equal(http.StatusOK, hookResponse.StatusCode)
	require.NoError(hookResponse.Body.Close())

	var completed toolCall
	select {
	case completed = <-spawnDone:
	case <-time.After(5 * time.Second):
		require.FailNow("MCP handoff did not complete after hook correlation")
	}
	require.NoError(completed.err)
	require.NotNil(completed.result)
	require.False(completed.result.IsError, "handoff returned error content: %#v", completed.result.Content)
	data, err := json.Marshal(completed.result.StructuredContent)
	require.NoError(err)
	var spawn struct {
		Stage     string `json:"stage"`
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
		Runtime struct {
			SessionKey string `json:"session_key"`
		} `json:"runtime"`
		Initial *struct {
			State string `json:"state"`
		} `json:"initial_message"`
		LegacyClaim *bool `json:"message_delivered"`
	}
	require.NoError(json.Unmarshal(data, &spawn))
	assert.Equal("coding_session_observed", spawn.Stage)
	assert.Equal(workspace.ID, spawn.Workspace.ID)
	assert.Equal(runtimeSession.SessionKey, spawn.Runtime.SessionKey)
	require.NotNil(spawn.Initial)
	assert.Equal("delivered", spawn.Initial.State)
	assert.Nil(spawn.LegacyClaim)

	followUp := callTool[struct {
		WorkspaceID       string `json:"workspace_id"`
		RuntimeSessionKey string `json:"runtime_session_key"`
		TargetKey         string `json:"target_key"`
		MessageBytes      int    `json:"message_bytes"`
	}](t, session, "kenn_forge_send_agent_message", map[string]any{
		"workspace_id": workspace.ID, "runtime_session_key": runtimeSession.SessionKey,
		"message": "keep going",
	})
	assert.Equal(workspace.ID, followUp.WorkspaceID)
	assert.Equal(runtimeSession.SessionKey, followUp.RuntimeSessionKey)
	assert.Equal("codex", followUp.TargetKey)
	assert.Equal(10, followUp.MessageBytes)
}

func connectHTTPMCP(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "kenn-forge-http-test-client", Version: "test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			token: token, base: http.DefaultTransport,
		}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	return session
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (r bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+r.token)
	return r.base.RoundTrip(clone)
}

func callTool[T any](t *testing.T, session *mcp.ClientSession, name string, args any) T {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, result.IsError, "tool %s returned error content: %#v", name, result.Content)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var output T
	require.NoError(t, json.Unmarshal(data, &output))
	return output
}

func seedPull(t *testing.T, database *db.DB, number int, title string) (int64, int64) {
	t.Helper()
	identity := db.GitHubRepoIdentity("github.com", "acme", "widgets")
	identity.PlatformRepoID = "repo-acme-widgets"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	pullID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: int64(number) * 1000, Number: number,
		URL:   fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		Title: title, Author: "testuser", State: db.MergeRequestStateOpen,
		Body: "cached pull body", HeadBranch: "feature/caching",
		HeadRepoCloneURL: "https://github.com/acme/widgets.git", BaseBranch: "main",
		Additions: 5, Deletions: 2, CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	return repoID, pullID
}

func seedIssue(t *testing.T, database *db.DB, number int, title string) int64 {
	t.Helper()
	identity := db.GitHubRepoIdentity("github.com", "acme", "widgets")
	identity.PlatformRepoID = "repo-acme-widgets"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	issueID, err := database.UpsertIssue(t.Context(), &db.Issue{
		RepoID: repoID, PlatformID: int64(number) * 1000, Number: number,
		URL:   fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number),
		Title: title, Author: "reporter", State: "open", Body: "cached issue body",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	return issueID
}
