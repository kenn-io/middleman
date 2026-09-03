package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeBackend struct {
	listRepositoriesFn           func(context.Context) ([]RepositorySummary, error)
	listActivityFn               func(context.Context, ActivityQuery) (ActivityPage, error)
	listPullsFn                  func(context.Context, ItemListQuery) ([]Pull, error)
	listIssuesFn                 func(context.Context, ItemListQuery) ([]Issue, error)
	getPullFn                    func(context.Context, ItemIdentity) (PullDetail, error)
	getIssueFn                   func(context.Context, ItemIdentity) (IssueDetail, error)
	getPullDiffFn                func(context.Context, ItemIdentity, bool) (Diff, error)
	getPullStackFn               func(context.Context, ItemIdentity) (Stack, error)
	listWorkflowStatesFn         func(context.Context, WorkflowQuery) (WorkflowPage, error)
	setWorkflowStateFn           func(context.Context, ItemIdentity, WorkflowUpdate) (WorkflowMutation, error)
	listLaunchTargetsFn          func(context.Context) ([]LaunchTarget, error)
	preferredWorkspaceAgentFn    func(context.Context, time.Time, []string) (string, bool, error)
	listWorkspaceAgentSessionsFn func(context.Context, string) ([]WorkspaceAgentSession, error)
	getWorkspaceFn               func(context.Context, string) (Workspace, error)
	createPullWorkspaceFn        func(context.Context, ItemIdentity, bool) (Workspace, error)
	createIssueWorkspaceFn       func(context.Context, ItemIdentity, bool) (Workspace, error)
	createAdHocWorkspaceFn       func(context.Context, RepositoryIdentity, string) (Workspace, error)
	launchWorkspaceRuntimeFn     func(context.Context, string, string) (RuntimeSession, error)
	getWorkspaceRuntimeFn        func(context.Context, string) (WorkspaceRuntime, error)
	submitAgentMessageFn         func(context.Context, AgentMessageRequest) (AgentMessageResult, error)
	submitInitialMessageFn       func(context.Context, InitialMessageRequest) (InitialMessageStatus, error)
	getInitialMessageFn          func(context.Context, string, string) (InitialMessageStatus, error)
}

func (b *fakeBackend) ListRepositories(ctx context.Context) ([]RepositorySummary, error) {
	if b.listRepositoriesFn != nil {
		return b.listRepositoriesFn(ctx)
	}
	return nil, nil
}

func (b *fakeBackend) ListActivity(ctx context.Context, query ActivityQuery) (ActivityPage, error) {
	if b.listActivityFn != nil {
		return b.listActivityFn(ctx, query)
	}
	return ActivityPage{}, nil
}

func (b *fakeBackend) ListPulls(ctx context.Context, query ItemListQuery) ([]Pull, error) {
	if b.listPullsFn != nil {
		return b.listPullsFn(ctx, query)
	}
	return nil, nil
}

func (b *fakeBackend) ListIssues(ctx context.Context, query ItemListQuery) ([]Issue, error) {
	if b.listIssuesFn != nil {
		return b.listIssuesFn(ctx, query)
	}
	return nil, nil
}

func (b *fakeBackend) GetPull(ctx context.Context, item ItemIdentity) (PullDetail, error) {
	if b.getPullFn != nil {
		return b.getPullFn(ctx, item)
	}
	return PullDetail{}, nil
}

func (b *fakeBackend) GetIssue(ctx context.Context, item ItemIdentity) (IssueDetail, error) {
	if b.getIssueFn != nil {
		return b.getIssueFn(ctx, item)
	}
	return IssueDetail{}, nil
}

func (b *fakeBackend) GetPullDiff(ctx context.Context, item ItemIdentity, patches bool) (Diff, error) {
	if b.getPullDiffFn != nil {
		return b.getPullDiffFn(ctx, item, patches)
	}
	return Diff{}, nil
}

func (b *fakeBackend) GetPullStack(ctx context.Context, item ItemIdentity) (Stack, error) {
	if b.getPullStackFn != nil {
		return b.getPullStackFn(ctx, item)
	}
	return Stack{}, &Error{Kind: "not_found", Code: "notFound", Message: "not stacked"}
}

func (b *fakeBackend) ListWorkflowStates(ctx context.Context, query WorkflowQuery) (WorkflowPage, error) {
	if b.listWorkflowStatesFn != nil {
		return b.listWorkflowStatesFn(ctx, query)
	}
	return WorkflowPage{}, nil
}

func (b *fakeBackend) SetWorkflowState(ctx context.Context, item ItemIdentity, update WorkflowUpdate) (WorkflowMutation, error) {
	if b.setWorkflowStateFn != nil {
		return b.setWorkflowStateFn(ctx, item, update)
	}
	return WorkflowMutation{}, nil
}

func (b *fakeBackend) ListLaunchTargets(ctx context.Context) ([]LaunchTarget, error) {
	if b.listLaunchTargetsFn != nil {
		return b.listLaunchTargetsFn(ctx)
	}
	return nil, nil
}

func (b *fakeBackend) PreferredWorkspaceAgentTarget(
	ctx context.Context, since time.Time, targetKeys []string,
) (string, bool, error) {
	if b.preferredWorkspaceAgentFn != nil {
		return b.preferredWorkspaceAgentFn(ctx, since, targetKeys)
	}
	return "", false, nil
}

func (b *fakeBackend) ListWorkspaceAgentSessions(ctx context.Context, workspaceID string) ([]WorkspaceAgentSession, error) {
	if b.listWorkspaceAgentSessionsFn != nil {
		return b.listWorkspaceAgentSessionsFn(ctx, workspaceID)
	}
	return nil, nil
}

func (b *fakeBackend) GetWorkspace(ctx context.Context, workspaceID string) (Workspace, error) {
	if b.getWorkspaceFn != nil {
		return b.getWorkspaceFn(ctx, workspaceID)
	}
	return Workspace{}, nil
}

func (b *fakeBackend) CreatePullWorkspace(ctx context.Context, item ItemIdentity, suppress bool) (Workspace, error) {
	if b.createPullWorkspaceFn != nil {
		return b.createPullWorkspaceFn(ctx, item, suppress)
	}
	return Workspace{}, nil
}

func (b *fakeBackend) CreateIssueWorkspace(ctx context.Context, item ItemIdentity, suppress bool) (Workspace, error) {
	if b.createIssueWorkspaceFn != nil {
		return b.createIssueWorkspaceFn(ctx, item, suppress)
	}
	return Workspace{}, nil
}

func (b *fakeBackend) CreateAdHocWorkspace(ctx context.Context, repo RepositoryIdentity, branch string) (Workspace, error) {
	if b.createAdHocWorkspaceFn != nil {
		return b.createAdHocWorkspaceFn(ctx, repo, branch)
	}
	return Workspace{}, nil
}

func (b *fakeBackend) LaunchWorkspaceRuntime(ctx context.Context, workspaceID, targetKey string) (RuntimeSession, error) {
	if b.launchWorkspaceRuntimeFn != nil {
		return b.launchWorkspaceRuntimeFn(ctx, workspaceID, targetKey)
	}
	return RuntimeSession{}, nil
}

func (b *fakeBackend) GetWorkspaceRuntime(ctx context.Context, workspaceID string) (WorkspaceRuntime, error) {
	if b.getWorkspaceRuntimeFn != nil {
		return b.getWorkspaceRuntimeFn(ctx, workspaceID)
	}
	return WorkspaceRuntime{}, nil
}

func (b *fakeBackend) SubmitAgentMessage(ctx context.Context, req AgentMessageRequest) (AgentMessageResult, error) {
	if b.submitAgentMessageFn != nil {
		return b.submitAgentMessageFn(ctx, req)
	}
	return AgentMessageResult{}, nil
}

func (b *fakeBackend) SubmitInitialMessage(ctx context.Context, req InitialMessageRequest) (InitialMessageStatus, error) {
	if b.submitInitialMessageFn != nil {
		return b.submitInitialMessageFn(ctx, req)
	}
	return InitialMessageStatus{}, nil
}

func (b *fakeBackend) GetInitialMessage(ctx context.Context, workspaceID, runtimeKey string) (InitialMessageStatus, error) {
	if b.getInitialMessageFn != nil {
		return b.getInitialMessageFn(ctx, workspaceID, runtimeKey)
	}
	return InitialMessageStatus{}, nil
}

func newMCPTestServer(t *testing.T, backend Backend) *Server {
	t.Helper()
	server, err := New(Options{Backend: backend, Version: "test"})
	require.NoError(t, err)
	server.agentHandoffPollInterval = time.Millisecond
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func testRepository() RepositoryIdentity {
	return RepositoryIdentity{
		Provider: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget",
		RepoPath:       "acme/widget", Owner: "acme", Name: "widget",
	}
}

func testItemIdentity(itemType string, number int) ItemIdentity {
	return ItemIdentity{
		Type: itemType, Provider: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme", Name: "widget", Number: number,
	}
}
