package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnWorkspaceWithAgentCallsDirectServicesAndUsesAuthoritativeEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-new", "runtime-new", "coding-new")
	workspaceReads := 0
	backend.createPullWorkspaceFn = func(_ context.Context, item ItemIdentity, suppress bool) (Workspace, error) {
		assert.Equal(testItemIdentity("pr", 42), item)
		assert.True(suppress)
		return Workspace{ID: "ws-new", Status: "creating", Created: true}, nil
	}
	backend.getWorkspaceFn = func(context.Context, string) (Workspace, error) {
		workspaceReads++
		status := "creating"
		if workspaceReads > 1 {
			status = "ready"
		}
		return Workspace{ID: "ws-new", Status: status}, nil
	}
	backend.submitInitialMessageFn = func(_ context.Context, req InitialMessageRequest) (InitialMessageStatus, error) {
		assert.Equal(InitialMessageRequest{
			WorkspaceID: "ws-new", RuntimeSessionKey: "runtime-new",
			TargetKey: "codex", Message: "review this\nthen implement",
		}, req)
		deliveredAt := time.Date(2026, 8, 7, 15, 0, 2, 0, time.UTC)
		return InitialMessageStatus{State: "delivered", MessageBytes: 26, DeliveredAt: &deliveredAt}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), spawnWorkspaceWithAgentInput{
		Source: &workspaceSourceInput{Type: "item", Item: &itemRefInput{
			Type: "pr", Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-acme-widget",
			Owner:          "acme", Name: "widget", Number: 42,
		}},
		AgentTarget: "codex", InitialMessage: "review this\r\nthen implement", Timeout: "2s",
	})

	require.NoError(err)
	assert.Equal("coding_session_observed", out.Stage)
	assert.Equal("delivered", out.InitialMessage.State)
	assert.Equal("ws-new", out.Workspace.ID)
	assert.False(out.Workspace.Reused)
	assert.Equal("runtime-new", out.Runtime.SessionKey)
	require.NotNil(out.CodingSession)
	assert.Equal("coding-new", out.CodingSession.SessionID)
	raw, err := json.Marshal(out)
	require.NoError(err)
	assert.NotContains(string(raw), `"message_delivered":`)
}

func TestSpawnWorkspaceWithAgentCustomTargetObservesCanonicalHookAgent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-custom", "runtime-custom", "coding-custom")
	backend.listLaunchTargetsFn = func(context.Context) ([]LaunchTarget, error) {
		return []LaunchTarget{{Key: "custom-worker", Kind: "agent", Available: true}}, nil
	}
	backend.launchWorkspaceRuntimeFn = func(_ context.Context, workspaceID, target string) (RuntimeSession, error) {
		assert.Equal("ws-custom", workspaceID)
		assert.Equal("custom-worker", target)
		return RuntimeSession{Key: "runtime-custom", TargetKey: target, Kind: "agent", Status: "running"}, nil
	}
	backend.getWorkspaceRuntimeFn = func(context.Context, string) (WorkspaceRuntime, error) {
		return WorkspaceRuntime{Sessions: []RuntimeSession{{
			Key: "runtime-custom", TargetKey: "custom-worker", Kind: "agent", Status: "running",
		}}}, nil
	}
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		return []WorkspaceAgentSession{
			{Agent: "codex", SessionID: "a-other-runtime", RuntimeSessionKey: "runtime-other", TargetKey: "custom-worker"},
			{Agent: "codex", SessionID: "b-other-target", RuntimeSessionKey: "runtime-custom", TargetKey: "other-worker"},
			{Agent: "codex", SessionID: "coding-custom", RuntimeSessionKey: "runtime-custom", TargetKey: "custom-worker"},
		}, nil
	}
	submissions := 0
	backend.submitInitialMessageFn = func(_ context.Context, req InitialMessageRequest) (InitialMessageStatus, error) {
		submissions++
		assert.Equal("custom-worker", req.TargetKey)
		return InitialMessageStatus{State: "delivered", MessageBytes: len(req.Message)}, nil
	}
	s := newMCPTestServer(t, backend)
	input := prSpawnInput("start")
	input.AgentTarget = "custom-worker"

	out, err := s.spawnWorkspaceWithAgent(t.Context(), input)

	require.NoError(err)
	assert.Equal(1, submissions)
	assert.Equal("coding_session_observed", out.Stage)
	require.NotNil(out.CodingSession)
	assert.Equal("coding-custom", out.CodingSession.SessionID)
	assert.Equal("codex", out.CodingSession.Agent)
	assert.Equal("custom-worker", out.CodingSession.TargetKey)
}

func TestSpawnWorkspaceWithAgentSubmitsPromptBeforeHookObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-prompt-first", "runtime-prompt-first", "coding-prompt-first")
	promptSubmitted := false
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		if !promptSubmitted {
			return nil, nil
		}
		return []WorkspaceAgentSession{{
			Agent: "codex", SessionID: "coding-prompt-first",
			RuntimeSessionKey: "runtime-prompt-first", TargetKey: "codex",
			State: "working", UpdatedAt: time.Now().UTC(),
		}}, nil
	}
	backend.submitInitialMessageFn = func(_ context.Context, req InitialMessageRequest) (InitialMessageStatus, error) {
		assert.Equal("codex", req.TargetKey)
		promptSubmitted = true
		return InitialMessageStatus{State: "delivered", MessageBytes: 5}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	require.NoError(err)
	assert.True(promptSubmitted)
	assert.Equal("coding_session_observed", out.Stage)
	require.NotNil(out.CodingSession)
	assert.Equal("coding-prompt-first", out.CodingSession.SessionID)
}

func TestSpawnWorkspaceWithAgentResumesExistingRuntimeAfterHookTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-resume", "runtime-resume", "coding-resume")
	launches := 0
	deliveries := 0
	submissions := 0
	hookVisible := false
	backend.launchWorkspaceRuntimeFn = func(context.Context, string, string) (RuntimeSession, error) {
		launches++
		return RuntimeSession{
			Key: "runtime-resume", TargetKey: "codex", Kind: "agent", Status: "running",
			CreatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
		}, nil
	}
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		submissions++
		if deliveries == 0 {
			deliveries++
		}
		return InitialMessageStatus{State: "delivered", MessageBytes: 5}, nil
	}
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		if !hookVisible {
			return nil, nil
		}
		return []WorkspaceAgentSession{{
			Agent: "codex", SessionID: "coding-resume",
			RuntimeSessionKey: "runtime-resume", TargetKey: "codex",
			State: "working", UpdatedAt: time.Now().UTC(),
		}}, nil
	}
	s := newMCPTestServer(t, backend)
	first := prSpawnInput("start")
	first.Timeout = "20ms"

	_, err := s.spawnWorkspaceWithAgent(t.Context(), first)
	var timeoutErr *Error
	require.ErrorAs(err, &timeoutErr)
	assert.Equal("message_delivered", timeoutErr.Details["last_completed_stage"])
	assert.Equal("coding_session_observed", timeoutErr.Details["failed_stage"])
	assert.Equal("delivered", timeoutErr.Details["initial_message_state"])

	hookVisible = true
	out, err := s.spawnWorkspaceWithAgent(t.Context(), spawnWorkspaceWithAgentInput{
		Resume: &agentHandoffResume{
			WorkspaceID: "ws-resume", RuntimeSessionKey: "runtime-resume",
		},
		AgentTarget: "codex", InitialMessage: "start", Timeout: "2s",
	})

	require.NoError(err)
	assert.Equal("coding_session_observed", out.Stage)
	assert.Equal(1, launches)
	assert.Equal(2, submissions)
	assert.Equal(1, deliveries)
}

func TestSpawnWorkspaceWithAgentResumesPromptSubmissionOnExistingRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-submit-resume", "runtime-submit-resume", "coding-submit-resume")
	launches := 0
	inputReady := false
	backend.launchWorkspaceRuntimeFn = func(context.Context, string, string) (RuntimeSession, error) {
		launches++
		return RuntimeSession{
			Key: "runtime-submit-resume", TargetKey: "codex", Kind: "agent", Status: "running",
			CreatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
		}, nil
	}
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		if !inputReady {
			return InitialMessageStatus{}, &Error{
				Kind: "unavailable", Code: ErrorCodeInitialMessageInputModeNotReady,
				Message: "agent terminal input mode is not ready", Retryable: true,
			}
		}
		return InitialMessageStatus{State: "delivered", MessageBytes: 5}, nil
	}
	s := newMCPTestServer(t, backend)
	first := prSpawnInput("start")
	first.Timeout = "20ms"

	_, err := s.spawnWorkspaceWithAgent(t.Context(), first)
	var timeoutErr *Error
	require.ErrorAs(err, &timeoutErr)
	assert.Equal("runtime_launched", timeoutErr.Details["last_completed_stage"])
	assert.Equal("message_delivered", timeoutErr.Details["failed_stage"])

	inputReady = true
	out, err := s.spawnWorkspaceWithAgent(t.Context(), spawnWorkspaceWithAgentInput{
		Resume: &agentHandoffResume{
			WorkspaceID: "ws-submit-resume", RuntimeSessionKey: "runtime-submit-resume",
		},
		AgentTarget: "codex", InitialMessage: "start", Timeout: "2s",
	})

	require.NoError(err)
	assert.Equal("coding_session_observed", out.Stage)
	assert.Equal(1, launches)
}

func TestSpawnWorkspaceWithAgentReusesWorkspaceAndLaunchesFreshRuntime(t *testing.T) {
	backend := successfulSpawnBackend("ws-existing", "runtime-fresh", "coding-fresh")
	backend.getPullFn = func(context.Context, ItemIdentity) (PullDetail, error) {
		return PullDetail{
			Pull:      &Pull{Number: 42, Repository: testRepository()},
			Workspace: &WorkspaceRef{ID: "ws-existing", Status: "ready"},
		}, nil
	}
	backend.createPullWorkspaceFn = func(context.Context, ItemIdentity, bool) (Workspace, error) {
		return Workspace{}, errors.New("workspace create must not run")
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	require.NoError(t, err)
	assert.True(t, out.Workspace.Reused)
	assert.Equal(t, "runtime-fresh", out.Runtime.SessionKey)
}

func TestSpawnWorkspaceWithAgentDefaultsToMostUsedRecentAgent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := successfulSpawnBackend("ws-default", "runtime-default", "coding-default")
	backend.listLaunchTargetsFn = func(context.Context) ([]LaunchTarget, error) {
		return []LaunchTarget{
			{Key: "codex", Label: "Codex", Kind: "agent", Source: "config", Available: true},
			{Key: "claude", Label: "Claude", Kind: "agent", Source: "config", Available: true},
		}, nil
	}
	backend.preferredWorkspaceAgentFn = func(_ context.Context, since time.Time, candidates []string) (string, bool, error) {
		assert.WithinDuration(time.Now().Add(-14*24*time.Hour), since, time.Second)
		assert.ElementsMatch([]string{"codex", "claude"}, candidates)
		return "claude", true, nil
	}
	backend.launchWorkspaceRuntimeFn = func(_ context.Context, workspaceID, target string) (RuntimeSession, error) {
		assert.Equal("ws-default", workspaceID)
		assert.Equal("claude", target)
		return RuntimeSession{
			Key: "runtime-default", TargetKey: "claude", Status: "running",
			CreatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
		}, nil
	}
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		return []WorkspaceAgentSession{{
			Agent: "claude", SessionID: "coding-default", RuntimeSessionKey: "runtime-default",
			TargetKey: "claude", State: "working",
			UpdatedAt: time.Date(2026, 8, 7, 15, 0, 1, 0, time.UTC),
		}}, nil
	}
	s := newMCPTestServer(t, backend)

	result, err := connectMCPTestSession(t, s).CallTool(
		t.Context(), &mcp.CallToolParams{
			Name: "kenn_forge_spawn_workspace_with_agent",
			Arguments: map[string]any{
				"source": map[string]any{
					"type": "item",
					"item": map[string]any{
						"type": "pr", "provider": "github",
						"platform_repo_id": "repo-acme-widget",
						"owner":            "acme", "name": "widget", "number": 42,
					},
				},
				"initial_message": "start",
				"timeout":         "2s",
			},
		},
	)

	require.NoError(err)
	require.NotNil(result)
	assert.False(result.IsError)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	var out spawnWorkspaceWithAgentOutput
	require.NoError(json.Unmarshal(raw, &out))
	assert.Equal("claude", out.Runtime.TargetKey)
}

func TestSpawnWorkspaceWithAgentFallsBackToFirstConfiguredAgent(t *testing.T) {
	backend := successfulSpawnBackend("ws-fallback", "runtime-fallback", "coding-fallback")
	backend.listLaunchTargetsFn = func(context.Context) ([]LaunchTarget, error) {
		return []LaunchTarget{
			{Key: "codex", Label: "Codex", Kind: "agent", Source: "config", Available: true},
			{Key: "claude", Label: "Claude", Kind: "agent", Source: "config", Available: true},
		}, nil
	}
	s := newMCPTestServer(t, backend)
	input := prSpawnInput("start")
	input.AgentTarget = ""

	out, err := s.spawnWorkspaceWithAgent(t.Context(), input)

	require.NoError(t, err)
	assert.Equal(t, "codex", out.Runtime.TargetKey)
}

func TestSpawnWorkspaceWithAgentReusesPRWorkspaceCreatedConcurrently(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-raced", "runtime-raced", "coding-raced")
	pullReads := 0
	backend.getPullFn = func(context.Context, ItemIdentity) (PullDetail, error) {
		pullReads++
		detail := PullDetail{Pull: &Pull{Number: 42, Repository: testRepository()}}
		if pullReads > 1 {
			detail.Workspace = &WorkspaceRef{ID: "ws-raced", Status: "ready"}
		}
		return detail, nil
	}
	backend.createPullWorkspaceFn = func(context.Context, ItemIdentity, bool) (Workspace, error) {
		return Workspace{}, &Error{
			Kind: "conflict", Code: ErrorCodeWorkspaceAlreadyExists,
			Message: "workspace already exists for this pull request",
		}
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	require.NoError(t, err)
	assert.True(out.Workspace.Reused)
	assert.Equal("ws-raced", out.Workspace.ID)
	assert.Equal(2, pullReads)
}

func TestSpawnWorkspaceWithAgentTimeoutReturnsStageWithoutDeliveryClaim(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-timeout", "runtime", "coding")
	backend.createPullWorkspaceFn = func(context.Context, ItemIdentity, bool) (Workspace, error) {
		return Workspace{ID: "ws-timeout", Status: "creating", Created: true}, nil
	}
	backend.getWorkspaceFn = func(ctx context.Context, _ string) (Workspace, error) {
		<-ctx.Done()
		return Workspace{ID: "ws-timeout", Status: "creating"}, context.Cause(ctx)
	}
	s := newMCPTestServer(t, backend)
	input := prSpawnInput("start")
	input.Timeout = "20ms"

	_, err := s.spawnWorkspaceWithAgent(t.Context(), input)

	var backendErr *Error
	require.ErrorAs(t, err, &backendErr)
	assert.Equal("agent_handoff_timeout", backendErr.Kind)
	assert.Equal("workspace_created", backendErr.Details["last_completed_stage"])
	assert.Equal("workspace_ready", backendErr.Details["failed_stage"])
	assert.Equal("ws-timeout", backendErr.Details["workspace_id"])
	assert.NotContains(backendErr.Details, "message_delivered")
}

func TestSpawnWorkspaceWithAgentReportsWorkspaceError(t *testing.T) {
	message := "clone failed"
	backend := successfulSpawnBackend("ws-error", "runtime", "coding")
	backend.createPullWorkspaceFn = func(context.Context, ItemIdentity, bool) (Workspace, error) {
		return Workspace{ID: "ws-error", Status: "creating", Created: true}, nil
	}
	backend.getWorkspaceFn = func(context.Context, string) (Workspace, error) {
		return Workspace{ID: "ws-error", Status: "error", ErrorMessage: &message}, nil
	}
	s := newMCPTestServer(t, backend)

	_, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	var backendErr *Error
	require.ErrorAs(t, err, &backendErr)
	assert.Equal(t, "error", backendErr.Details["workspace_status"])
	assert.Contains(t, backendErr.Message, "clone failed")
}

func TestSpawnWorkspaceWithAgentCreatesIssueAndAdHocWorkspaces(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		backend := successfulSpawnBackend("ws-issue", "runtime-issue", "coding-issue")
		backend.getIssueFn = func(context.Context, ItemIdentity) (IssueDetail, error) {
			return IssueDetail{Issue: &Issue{Number: 7, Repository: testRepository()}}, nil
		}
		backend.createIssueWorkspaceFn = func(_ context.Context, item ItemIdentity, suppress bool) (Workspace, error) {
			assert.Equal(t, testItemIdentity("issue", 7), item)
			assert.True(t, suppress)
			return Workspace{ID: "ws-issue", Status: "ready", Created: true}, nil
		}
		s := newMCPTestServer(t, backend)

		out, err := s.spawnWorkspaceWithAgent(t.Context(), spawnWorkspaceWithAgentInput{
			Source: &workspaceSourceInput{Type: "item", Item: &itemRefInput{
				Type: "issue", Provider: "github", PlatformRepoID: "repo-acme-widget",
				Owner: "acme", Name: "widget", Number: 7,
			}},
			AgentTarget: "codex", InitialMessage: "fix the issue", Timeout: "2s",
		})
		require.NoError(t, err)
		assert.Equal(t, "ws-issue", out.Workspace.ID)
	})

	t.Run("ad hoc", func(t *testing.T) {
		backend := successfulSpawnBackend("ws-adhoc", "runtime-adhoc", "coding-adhoc")
		backend.createAdHocWorkspaceFn = func(_ context.Context, repo RepositoryIdentity, branch string) (Workspace, error) {
			assert.Equal(t, testRepository(), repo)
			assert.Empty(t, branch)
			return Workspace{
				ID: "ws-adhoc", Status: "ready", Created: true,
				GitHeadRef: "kenn-forge/work-abc123",
			}, nil
		}
		s := newMCPTestServer(t, backend)

		out, err := s.spawnWorkspaceWithAgent(t.Context(), spawnWorkspaceWithAgentInput{
			Source: &workspaceSourceInput{Type: "adhoc", AdHoc: &adHocWorkspaceSource{
				Repo: repoFilterInput{
					Provider: "github", PlatformRepoID: "repo-acme-widget",
					Owner: "acme", Name: "widget",
				},
			}},
			AgentTarget: "codex", InitialMessage: "start work", Timeout: "2s",
		})
		require.NoError(t, err)
		assert.Equal(t, "kenn-forge/work-abc123", out.Source.AdHoc.Branch)
	})
}

func TestSpawnWorkspaceWithAgentRetriesMultilineMessageUntilInputModeIsReady(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-paste", "runtime-paste", "coding-paste")
	messagePosts := 0
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		messagePosts++
		if messagePosts < 3 {
			return InitialMessageStatus{}, &Error{
				Kind: "unavailable", Code: ErrorCodeInitialMessageInputModeNotReady,
				Message: "agent terminal input mode is not ready", Retryable: true,
			}
		}
		return InitialMessageStatus{State: "delivered", MessageBytes: 12}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("first\nsecond"))

	require.NoError(t, err)
	assert.Equal("coding_session_observed", out.Stage)
	assert.Equal("delivered", out.InitialMessage.State)
	assert.Equal(3, messagePosts)
}

func TestSpawnWorkspaceWithAgentRecoversAmbiguousMessageFromSameBackend(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-recovery", "runtime-recovery", "coding-recovery")
	messagePosts := 0
	statusReads := 0
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		messagePosts++
		return InitialMessageStatus{}, &Error{
			Kind: "internal_error", Message: "result unknown", Ambiguous: true,
		}
	}
	backend.getInitialMessageFn = func(context.Context, string, string) (InitialMessageStatus, error) {
		statusReads++
		deliveredAt := time.Date(2026, 8, 7, 15, 0, 2, 0, time.UTC)
		return InitialMessageStatus{State: "delivered", MessageBytes: 5, DeliveredAt: &deliveredAt}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	require.NoError(t, err)
	assert.Equal("coding_session_observed", out.Stage)
	assert.Equal("delivered", out.InitialMessage.State)
	assert.Equal(1, messagePosts)
	assert.Equal(1, statusReads)
}

func TestSpawnWorkspaceWithAgentTreatsPendingMessageStateAsAmbiguous(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-pending", "runtime-pending", "coding-pending")
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		return InitialMessageStatus{State: "pending", MessageBytes: 5}, nil
	}
	s := newMCPTestServer(t, backend)

	_, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	var backendErr *Error
	require.ErrorAs(t, err, &backendErr)
	assert.True(backendErr.Ambiguous)
	assert.Equal("pending", backendErr.Details["initial_message_state"])
	assert.NotContains(backendErr.Details, "message_delivered")
}

func TestSpawnWorkspaceWithAgentKeepsUncertainRecoveryAmbiguous(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-recovery", "runtime-recovery", "coding-recovery")
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		return InitialMessageStatus{State: "uncertain", MessageBytes: 5}, &Error{
			Kind: "internal_error", Message: "result unknown", Ambiguous: true,
		}
	}
	backend.getInitialMessageFn = func(context.Context, string, string) (InitialMessageStatus, error) {
		return InitialMessageStatus{State: "uncertain", MessageBytes: 5}, nil
	}
	s := newMCPTestServer(t, backend)

	out, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	var backendErr *Error
	require.ErrorAs(t, err, &backendErr)
	assert.True(backendErr.Ambiguous)
	assert.Equal("uncertain", backendErr.Details["initial_message_state"])
	assert.Equal("message_delivered", backendErr.Details["failed_stage"])
	assert.NotContains(backendErr.Details, "message_delivered")
	require.NotNil(t, out.InitialMessage)
	assert.Equal("uncertain", out.InitialMessage.State)
}

func TestSpawnWorkspaceWithAgentReportsRuntimeExitBeforeHookSession(t *testing.T) {
	assert := assert.New(t)
	backend := successfulSpawnBackend("ws-exit", "runtime-exit", "coding")
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		return nil, nil
	}
	backend.getWorkspaceRuntimeFn = func(context.Context, string) (WorkspaceRuntime, error) {
		return WorkspaceRuntime{Sessions: []RuntimeSession{{Key: "runtime-exit", Status: "error"}}}, nil
	}
	messagePosts := 0
	backend.submitInitialMessageFn = func(context.Context, InitialMessageRequest) (InitialMessageStatus, error) {
		messagePosts++
		return InitialMessageStatus{State: "delivered", MessageBytes: 5}, nil
	}
	s := newMCPTestServer(t, backend)

	_, err := s.spawnWorkspaceWithAgent(t.Context(), prSpawnInput("start"))

	var backendErr *Error
	require.ErrorAs(t, err, &backendErr)
	assert.Equal("coding_session_observed", backendErr.Details["failed_stage"])
	assert.Contains(backendErr.Message, "runtime exited")
	assert.Equal(1, messagePosts)
}

func TestSpawnWorkspaceWithAgentRejectsInvalidInputBeforeBackendCalls(t *testing.T) {
	calls := 0
	backend := &fakeBackend{listLaunchTargetsFn: func(context.Context) ([]LaunchTarget, error) {
		calls++
		return nil, nil
	}}
	s := newMCPTestServer(t, backend)
	inputs := []spawnWorkspaceWithAgentInput{
		{AgentTarget: "codex", InitialMessage: "start", Timeout: "16m"},
		{
			Source: &workspaceSourceInput{Type: "item", Item: &itemRefInput{
				Type: "pr", Provider: "github", PlatformRepoID: "repo-acme-widget",
				Owner: "acme", Name: "widget", Number: 42,
			}},
			AgentTarget: "codex", InitialMessage: " \n\t",
		},
	}
	for _, input := range inputs {
		_, err := s.spawnWorkspaceWithAgent(t.Context(), input)
		require.Error(t, err)
	}
	assert.Equal(t, 0, calls)
}

func successfulSpawnBackend(workspaceID, runtimeKey, codingSessionID string) *fakeBackend {
	backend := &fakeBackend{}
	backend.listLaunchTargetsFn = func(context.Context) ([]LaunchTarget, error) {
		return []LaunchTarget{{
			Key: "codex", Label: "Codex", Kind: "agent", Source: "config", Available: true,
		}}, nil
	}
	backend.getPullFn = func(context.Context, ItemIdentity) (PullDetail, error) {
		return PullDetail{Pull: &Pull{Number: 42, Repository: testRepository()}}, nil
	}
	backend.createPullWorkspaceFn = func(context.Context, ItemIdentity, bool) (Workspace, error) {
		return Workspace{ID: workspaceID, Status: "ready", Created: true}, nil
	}
	backend.getWorkspaceFn = func(context.Context, string) (Workspace, error) {
		return Workspace{ID: workspaceID, Status: "ready"}, nil
	}
	backend.launchWorkspaceRuntimeFn = func(_ context.Context, gotWorkspace, target string) (RuntimeSession, error) {
		if gotWorkspace != workspaceID || target != "codex" {
			return RuntimeSession{}, errors.New("unexpected runtime launch")
		}
		return RuntimeSession{
			Key: runtimeKey, TargetKey: "codex", Kind: "agent", Status: "running",
			CreatedAt: time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC),
		}, nil
	}
	backend.listWorkspaceAgentSessionsFn = func(context.Context, string) ([]WorkspaceAgentSession, error) {
		return []WorkspaceAgentSession{{
			Agent: "codex", SessionID: codingSessionID, RuntimeSessionKey: runtimeKey,
			TargetKey: "codex", State: "working",
			UpdatedAt: time.Date(2026, 8, 7, 15, 0, 1, 0, time.UTC),
		}}, nil
	}
	backend.getWorkspaceRuntimeFn = func(context.Context, string) (WorkspaceRuntime, error) {
		return WorkspaceRuntime{Sessions: []RuntimeSession{{
			Key: runtimeKey, TargetKey: "codex", Kind: "agent", Status: "running",
		}}}, nil
	}
	backend.submitInitialMessageFn = func(_ context.Context, req InitialMessageRequest) (InitialMessageStatus, error) {
		return InitialMessageStatus{State: "delivered", MessageBytes: len(req.Message)}, nil
	}
	return backend
}

func prSpawnInput(message string) spawnWorkspaceWithAgentInput {
	return spawnWorkspaceWithAgentInput{
		Source: &workspaceSourceInput{Type: "item", Item: &itemRefInput{
			Type: "pr", Provider: "github", PlatformRepoID: "repo-acme-widget",
			Owner: "acme", Name: "widget", Number: 42,
		}},
		AgentTarget: "codex", InitialMessage: message, Timeout: "2s",
	}
}

func TestNormalizeSpawnInitialMessage(t *testing.T) {
	message, err := normalizeSpawnInitialMessage("first\r\nsecond")
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond", message)
	_, err = normalizeSpawnInitialMessage(strings.Repeat("a", (64<<10)+1))
	require.ErrorContains(t, err, "64 KiB")
}
