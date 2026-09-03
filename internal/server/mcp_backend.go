package server

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/mcpserver"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/issueapi"
	"go.kenn.io/forge/internal/server/pullapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
)

func (s *Server) MCPBackend() mcpserver.Backend {
	backend := mcpBackend{server: s}
	return mcpserver.NewFederatedBackend(backend, backend)
}

type mcpBackend struct {
	server *Server
}

func (b mcpBackend) ListRepositories(ctx context.Context) ([]mcpserver.RepositorySummary, error) {
	var (
		rows []repoSummaryResponse
		err  error
	)
	if b.server.providerSource != nil {
		rows, err = b.server.providerSource.ListRepositorySummaries(ctx)
	} else {
		rows, err = b.server.listRepoSummariesService(ctx)
	}
	if err != nil {
		return nil, mcpBackendError(err)
	}
	out := make([]mcpserver.RepositorySummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpserver.RepositorySummary{
			Repository:  repositoryIdentityFromResponse(row.Repo),
			OpenPRCount: row.OpenPRCount, OpenIssueCount: row.OpenIssueCount,
			LastSyncCompletedAt: row.LastSyncCompletedAt, LastSyncError: row.LastSyncError,
		})
	}
	return out, nil
}

func (b mcpBackend) ListActivity(
	ctx context.Context, query mcpserver.ActivityQuery,
) (mcpserver.ActivityPage, error) {
	var resolved resolvedMCPRepository
	if query.Repository.Provider != "" {
		var err error
		resolved, err = b.resolveProviderRepositoryFence(ctx, query.Repository)
		if err != nil {
			return mcpserver.ActivityPage{}, err
		}
	}
	body, err := b.server.listActivityService(ctx, &listActivityInput{
		Repo: mcpRepositoryFilter(query.Repository), Types: query.ActivityTypes,
		ItemTypes: query.ItemTypes, Search: query.Search, After: query.After, Since: query.Since,
	})
	if err != nil {
		return mcpserver.ActivityPage{}, mcpBackendError(err)
	}
	if resolved.repo != nil {
		if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
			return mcpserver.ActivityPage{}, err
		}
	}
	out := mcpserver.ActivityPage{
		Items: make([]mcpserver.ActivityItem, 0, len(body.Items)), Capped: body.Capped,
	}
	for _, row := range body.Items {
		item := mcpserver.ActivityItem{
			ID: row.ID, Cursor: row.Cursor, ActivityType: row.ActivityType,
			Repository: mcpserver.RepositoryIdentity{
				Provider: row.Repo.Provider, PlatformHost: row.Repo.PlatformHost,
				PlatformRepoID: row.Repo.PlatformRepoID,
				RepoPath:       row.Repo.RepoPath, Owner: row.Repo.Owner, Name: row.Repo.Name,
			},
			ItemType: row.ItemType, ItemNumber: row.ItemNumber,
			ItemTitle: row.ItemTitle, ItemURL: row.ItemURL, ItemState: row.ItemState,
			Author: row.Author, ItemAuthor: row.ItemAuthor, CreatedAt: row.CreatedAt,
			BodyPreview: row.BodyPreview, BranchName: row.BranchName,
			CommitSHA: row.CommitSHA, BeforeSHA: row.BeforeSHA, AfterSHA: row.AfterSHA,
			AuthorName: row.AuthorName, AuthorEmail: row.AuthorEmail,
			CommitterName: row.CommitterName, CommitterEmail: row.CommitterEmail,
			AuthoredAt: row.AuthoredAt, CommittedAt: row.CommittedAt,
			ActivityURL: row.ActivityURL, SubjectState: row.SubjectState,
		}
		item.Workspace = mcpWorkspaceRef(row.Workspace)
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (b mcpBackend) ListPulls(
	ctx context.Context, query mcpserver.ItemListQuery,
) ([]mcpserver.Pull, error) {
	var resolved resolvedMCPRepository
	if query.Repository.Provider != "" {
		var err error
		resolved, err = b.resolveProviderRepositoryFence(ctx, query.Repository)
		if err != nil {
			return nil, err
		}
	}
	rows, err := b.server.pullAPI.ListService(ctx, pullapi.ListQuery{
		Repo: mcpRepositoryFilter(query.Repository), State: query.State,
		Text: query.Text, Limit: query.Limit, Offset: query.Offset,
	})
	if err != nil {
		return nil, mcpBackendError(err)
	}
	if resolved.repo != nil {
		if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
			return nil, err
		}
	}
	out := make([]mcpserver.Pull, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpPull(row))
	}
	return out, nil
}

func (b mcpBackend) ListIssues(
	ctx context.Context, query mcpserver.ItemListQuery,
) ([]mcpserver.Issue, error) {
	var resolved resolvedMCPRepository
	if query.Repository.Provider != "" {
		var err error
		resolved, err = b.resolveProviderRepositoryFence(ctx, query.Repository)
		if err != nil {
			return nil, err
		}
	}
	rows, err := b.server.issueAPI.ListService(ctx, issueapi.ListQuery{
		Repo: mcpRepositoryFilter(query.Repository), State: query.State,
		Text: query.Text, Limit: query.Limit, Offset: query.Offset,
	})
	if err != nil {
		return nil, mcpBackendError(err)
	}
	if resolved.repo != nil {
		if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
			return nil, err
		}
	}
	out := make([]mcpserver.Issue, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpIssue(row))
	}
	return out, nil
}

func (b mcpBackend) GetPull(
	ctx context.Context, item mcpserver.ItemIdentity,
) (mcpserver.PullDetail, error) {
	resolved, err := b.resolveProviderRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.PullDetail{}, err
	}
	detail, err := b.server.pullAPI.GetService(ctx, pullServiceIdentity(item))
	if err != nil {
		return mcpserver.PullDetail{}, mcpBackendError(err)
	}
	if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
		return mcpserver.PullDetail{}, err
	}
	out := mcpserver.PullDetail{
		DetailLoaded: detail.DetailLoaded, DetailFetchedAt: detail.DetailFetchedAt,
		Workspace: mcpWorkspaceRef(detail.Workspace),
		Events:    make([]mcpserver.DetailEvent, 0, len(detail.Events)),
		Checks:    make([]mcpserver.Check, 0, len(detail.Checks)),
	}
	if detail.MergeRequest != nil {
		pull := mcpserver.Pull{
			Number: detail.MergeRequest.Number, Title: detail.MergeRequest.Title,
			State: string(detail.MergeRequest.State), Author: detail.MergeRequest.Author,
			URL: detail.MergeRequest.URL, IsDraft: detail.MergeRequest.IsDraft,
			Body:           detail.MergeRequest.Body,
			WorkflowStatus: string(detail.MergeRequest.KanbanStatus),
			LastActivityAt: detail.MergeRequest.LastActivityAt,
			Repository:     repositoryIdentityFromResponse(detail.Repo),
			Workspace:      mcpWorkspaceRef(detail.Workspace),
			DetailLoaded:   detail.DetailLoaded, DetailFetchedAt: detail.DetailFetchedAt,
		}
		out.Pull = &pull
	}
	for _, event := range detail.Events {
		out.Events = append(out.Events, mcpserver.DetailEvent{
			EventType: event.EventType, Author: event.Author, Summary: event.Summary,
			Body: event.Body, CreatedAt: event.CreatedAt,
		})
	}
	if detail.Stack != nil {
		stack := mcpserver.Stack{
			Position: detail.Stack.Position, Size: detail.Stack.Size,
			Health:  detail.Stack.Health,
			Members: make([]mcpserver.StackMember, 0, len(detail.Stack.Members)),
		}
		for _, member := range detail.Stack.Members {
			stack.Members = append(stack.Members, mcpserver.StackMember{
				Number: member.Number, Title: member.Title, State: member.State,
				Position: member.Position, IsDraft: member.IsDraft,
			})
		}
		out.Stack = &stack
	}
	for _, check := range detail.Checks {
		out.Checks = append(out.Checks, mcpserver.Check{
			Name: check.Name, Status: check.Status, Conclusion: check.Conclusion,
			URL: check.URL, App: check.App, DurationSeconds: check.DurationSeconds,
		})
	}
	return out, nil
}

func (b mcpBackend) GetIssue(
	ctx context.Context, item mcpserver.ItemIdentity,
) (mcpserver.IssueDetail, error) {
	resolved, err := b.resolveProviderRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.IssueDetail{}, err
	}
	detail, err := b.server.issueAPI.GetService(ctx, issueServiceIdentity(item))
	if err != nil {
		return mcpserver.IssueDetail{}, mcpBackendError(err)
	}
	if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
		return mcpserver.IssueDetail{}, err
	}
	out := mcpserver.IssueDetail{
		DetailLoaded: detail.DetailLoaded, DetailFetchedAt: detail.DetailFetchedAt,
		Workspace: mcpWorkspaceRef(detail.Workspace),
		Events:    make([]mcpserver.DetailEvent, 0, len(detail.Events)),
	}
	if detail.Issue != nil {
		issue := mcpserver.Issue{
			Number: detail.Issue.Number, Title: detail.Issue.Title,
			State: detail.Issue.State, Author: detail.Issue.Author, URL: detail.Issue.URL,
			Body: detail.Issue.Body, WorkflowStatus: string(detail.Issue.WorkflowStatus),
			LastActivityAt: detail.Issue.LastActivityAt,
			Repository:     repositoryIdentityFromResponse(detail.Repo),
			Workspace:      mcpWorkspaceRef(detail.Workspace),
			DetailLoaded:   detail.DetailLoaded, DetailFetchedAt: detail.DetailFetchedAt,
		}
		out.Issue = &issue
	}
	for _, event := range detail.Events {
		out.Events = append(out.Events, mcpserver.DetailEvent{
			EventType: event.EventType, Author: event.Author, Summary: event.Summary,
			Body: event.Body, CreatedAt: event.CreatedAt,
		})
	}
	if detail.Workflow != nil {
		workflow := mcpserver.WorkflowState{
			Status: string(detail.Workflow.Status), UpdatedAt: detail.Workflow.UpdatedAt,
			UpdatedSource: detail.Workflow.UpdatedSource,
			UpdatedActor:  detail.Workflow.UpdatedActor,
			UpdatedReason: detail.Workflow.UpdatedReason,
		}
		out.Workflow = &workflow
	}
	return out, nil
}

func (b mcpBackend) GetPullDiff(
	ctx context.Context, item mcpserver.ItemIdentity, includePatches bool,
) (mcpserver.Diff, error) {
	resolved, err := b.resolveRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.Diff{}, err
	}
	identity := pullServiceIdentity(item)
	var (
		stale bool
		files []mcpserver.DiffFile
	)
	if includePatches {
		result, err := b.server.pullAPI.GetDiffService(ctx, identity, pullapi.DiffQuery{})
		if err != nil {
			return mcpserver.Diff{}, mcpBackendError(err)
		}
		stale = result.Stale
		files = mcpDiffFiles(result.Files)
	} else {
		result, err := b.server.pullAPI.GetFilesService(ctx, identity)
		if err != nil {
			return mcpserver.Diff{}, mcpBackendError(err)
		}
		stale = result.Stale
		files = mcpDiffFiles(result.Files)
	}
	if err := b.confirmRepositoryRoute(ctx, resolved); err != nil {
		return mcpserver.Diff{}, err
	}
	return mcpserver.Diff{Stale: stale, Files: files}, nil
}

func (b mcpBackend) GetPullStack(
	ctx context.Context, item mcpserver.ItemIdentity,
) (mcpserver.Stack, error) {
	resolved, err := b.resolveProviderRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.Stack{}, err
	}
	var stack pullapi.StackContext
	if b.server.providerSource != nil {
		stack, err = b.server.providerSource.GetPullStack(ctx, item)
	} else {
		stack, err = b.server.pullAPI.GetStackService(ctx, pullServiceIdentity(item))
	}
	if err != nil {
		return mcpserver.Stack{}, mcpBackendError(err)
	}
	if err := b.confirmProviderRepositoryRoute(ctx, resolved); err != nil {
		return mcpserver.Stack{}, err
	}
	return mcpStack(stack), nil
}

func (b mcpBackend) ListWorkflowStates(
	ctx context.Context, query mcpserver.WorkflowQuery,
) (mcpserver.WorkflowPage, error) {
	if b.server.providerSource != nil {
		page, err := b.server.providerSource.ListWorkflowStates(ctx, query)
		if err != nil {
			return mcpserver.WorkflowPage{}, mcpBackendError(err)
		}
		return page, nil
	}
	return b.listWorkflowStatesLocal(ctx, query)
}

func (b mcpBackend) listWorkflowStatesLocal(
	ctx context.Context, query mcpserver.WorkflowQuery,
) (mcpserver.WorkflowPage, error) {
	var resolved resolvedMCPRepository
	if query.Repository.Provider != "" {
		var err error
		resolved, err = b.resolveRepositoryFence(ctx, query.Repository)
		if err != nil {
			return mcpserver.WorkflowPage{}, err
		}
	}
	rows, next, err := b.server.db.ListItemWorkflowStates(ctx, db.ListWorkflowStatesOpts{
		RepoFilters: mcpRepoFilters(query.Repository), ItemTypes: query.ItemTypes,
		States: query.States, IncludeClosed: query.IncludeClosed,
		ExcludeRemovedUpstream: true, Limit: query.Limit, Cursor: query.Cursor,
	})
	if err != nil {
		if errors.Is(err, db.ErrInvalidWorkflowCursor) {
			return mcpserver.WorkflowPage{}, &mcpserver.Error{
				Kind: "invalid_request", Code: string(httpapi.CodeValidationError),
				Message: "invalid cursor",
			}
		}
		return mcpserver.WorkflowPage{}, mcpBackendError(err)
	}
	if resolved.repo != nil {
		if err := b.confirmRepositoryRoute(ctx, resolved); err != nil {
			return mcpserver.WorkflowPage{}, err
		}
	}
	out := mcpserver.WorkflowPage{
		Items: make([]mcpserver.WorkflowItem, 0, len(rows)), NextCursor: next,
	}
	for _, row := range rows {
		workflow := mcpserver.WorkflowState{Status: string(normalizeMCPWorkflowStatus(row.Status))}
		if row.HasRow && row.UpdatedAt != nil {
			workflow.UpdatedAt = formatUTCRFC3339(*row.UpdatedAt)
			workflow.UpdatedSource = row.UpdatedSource
			workflow.UpdatedActor = row.UpdatedActor
			workflow.UpdatedReason = row.UpdatedReason
		}
		repository := mcpserver.RepositoryIdentity{
			Provider: row.Platform, PlatformHost: row.PlatformHost,
			PlatformRepoID: row.PlatformRepoID,
			RepoPath:       row.RepoPath, Owner: row.Owner, Name: row.Name,
		}
		out.Items = append(out.Items, mcpserver.WorkflowItem{
			Identity: mcpserver.ItemIdentity{
				Type: row.ItemType, Provider: row.Platform, PlatformHost: row.PlatformHost,
				PlatformRepoID: row.PlatformRepoID,
				Owner:          row.Owner, Name: row.Name, Number: row.Number,
			},
			Repository: repository, Title: row.Title, State: row.State,
			URL: row.URL, Author: row.Author, IsDraft: row.IsDraft,
			LastActivityAt: formatUTCRFC3339(row.LastActivityAt), Workflow: workflow,
		})
	}
	return out, nil
}

func (b mcpBackend) SetWorkflowState(
	ctx context.Context, item mcpserver.ItemIdentity, update mcpserver.WorkflowUpdate,
) (mcpserver.WorkflowMutation, error) {
	if b.server.providerSource != nil {
		mutation, err := b.server.providerSource.SetWorkflowState(ctx, item, update)
		if err != nil {
			return mcpserver.WorkflowMutation{}, mcpBackendMutationError(err)
		}
		return mutation, nil
	}
	return b.setWorkflowStateLocal(ctx, item, update)
}

func (b mcpBackend) setWorkflowStateLocal(
	ctx context.Context, item mcpserver.ItemIdentity, update mcpserver.WorkflowUpdate,
) (mcpserver.WorkflowMutation, error) {
	if b.server.providerWriteGate != nil {
		release, err := b.server.providerWriteGate.Admit(ctx)
		if err != nil {
			if errors.Is(err, providerplane.ErrSpokePreparationInProgress) {
				return mcpserver.WorkflowMutation{}, mcpBackendError(spokePreparationProblem())
			}
			return mcpserver.WorkflowMutation{}, mcpBackendMutationError(err)
		}
		defer release()
	}
	repo, err := b.resolveRepository(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.WorkflowMutation{}, err
	}
	switch item.Type {
	case db.ItemTypePR:
		visible, readErr := b.server.db.GetVisibleMergeRequestByRepoIDAndNumber(ctx, repo.ID, item.Number)
		if readErr != nil {
			return mcpserver.WorkflowMutation{}, mcpBackendError(httpapi.Internal("read pull request failed"))
		}
		if visible == nil {
			return mcpserver.WorkflowMutation{}, mcpBackendError(httpapi.NotFound(
				httpapi.CodePullNotFound, "pull request not found", nil,
			))
		}
	case db.ItemTypeIssue:
		visible, readErr := b.server.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, item.Number)
		if readErr != nil {
			return mcpserver.WorkflowMutation{}, mcpBackendError(httpapi.Internal("read issue failed"))
		}
		if visible == nil {
			return mcpserver.WorkflowMutation{}, mcpBackendError(httpapi.NotFound(
				httpapi.CodeIssueNotFound, "issue not found", nil,
			))
		}
	default:
		return mcpserver.WorkflowMutation{}, &mcpserver.Error{
			Kind: "invalid_request", Code: string(httpapi.CodeValidationError),
			Message: "item type must be pr or issue",
		}
	}
	result, err := b.server.db.SetItemWorkflowState(ctx, db.SetItemWorkflowStateParams{
		RepoID: repo.ID, ItemType: item.Type, ItemNumber: item.Number,
		Status: update.Status, ExpectedStatus: update.ExpectedStatus,
		Source: update.Source, Actor: update.Actor, Reason: update.Reason,
	})
	if conflict, ok := errors.AsType[*db.WorkflowStateConflictError](err); ok {
		return mcpserver.WorkflowMutation{}, &mcpserver.Error{
			Kind: "conflict", Code: string(httpapi.CodeConflict),
			Message: "workflow state changed", Details: map[string]any{
				"current_status": conflict.Current, "expected_status": conflict.Expected,
			},
		}
	}
	if err != nil {
		return mcpserver.WorkflowMutation{}, mcpBackendMutationError(err)
	}
	return mcpserver.WorkflowMutation{
		PreviousStatus: string(normalizeMCPWorkflowStatus(result.PreviousStatus)),
		State: mcpserver.WorkflowState{
			Status:        string(normalizeMCPWorkflowStatus(result.State.Status)),
			UpdatedAt:     formatUTCRFC3339(result.State.UpdatedAt),
			UpdatedSource: result.State.UpdatedSource,
			UpdatedActor:  result.State.UpdatedActor,
			UpdatedReason: result.State.UpdatedReason,
		},
	}, nil
}

func (b mcpBackend) ListLaunchTargets(context.Context) ([]mcpserver.LaunchTarget, error) {
	targets := b.server.runtime.LaunchTargets()
	out := make([]mcpserver.LaunchTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, mcpserver.LaunchTarget{
			Key: target.Key, Label: target.Label, Kind: string(target.Kind),
			Source: target.Source, Available: target.Available,
			DisabledReason: target.DisabledReason,
		})
	}
	return out, nil
}

func (b mcpBackend) ListWorkspaceAgentSessions(
	ctx context.Context, workspaceID string,
) ([]mcpserver.WorkspaceAgentSession, error) {
	sessions, err := b.server.workspaceAPI.ListWorkspaceAgentSessionsService(ctx, workspaceID)
	if err != nil {
		return nil, mcpBackendError(err)
	}
	out := make([]mcpserver.WorkspaceAgentSession, 0, len(sessions))
	for _, session := range sessions {
		row := mcpserver.WorkspaceAgentSession{
			Agent: session.Agent, SessionID: session.SessionID,
			RuntimeSessionKey: session.RuntimeSessionKey, TargetKey: session.TargetKey,
			State: string(session.State), UpdatedAt: session.UpdatedAt,
		}
		if session.InitialMessage != nil {
			row.InitialMessage = mcpInitialMessage(*session.InitialMessage)
		}
		out = append(out, row)
	}
	return out, nil
}

func (b mcpBackend) GetWorkspace(
	ctx context.Context, workspaceID string,
) (mcpserver.Workspace, error) {
	result, err := b.server.workspaceAPI.GetWorkspaceService(ctx, workspaceID)
	if err != nil {
		return mcpserver.Workspace{}, mcpBackendError(err)
	}
	return mcpWorkspace(result), nil
}

func (b mcpBackend) CreatePullWorkspace(
	ctx context.Context, item mcpserver.ItemIdentity, suppressAutoAssign bool,
) (mcpserver.Workspace, error) {
	resolved, err := b.resolveWorkspaceRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.Workspace{}, err
	}
	ctx = b.routeFenceContext(ctx, resolved)
	result, err := b.server.workspaceAPI.CreatePullWorkspace(ctx, workspaceapi.CreatePullWorkspaceRequest{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		Owner: item.Owner, Name: item.Name, Number: item.Number,
		SuppressAutoAssign: suppressAutoAssign,
	})
	if err != nil {
		return mcpserver.Workspace{}, mcpBackendMutationError(err)
	}
	return mcpWorkspace(result), nil
}

func (b mcpBackend) CreateIssueWorkspace(
	ctx context.Context, item mcpserver.ItemIdentity, suppressAutoAssign bool,
) (mcpserver.Workspace, error) {
	resolved, err := b.resolveWorkspaceRepositoryFence(ctx, itemRepositoryIdentity(item))
	if err != nil {
		return mcpserver.Workspace{}, err
	}
	ctx = b.routeFenceContext(ctx, resolved)
	result, err := b.server.workspaceAPI.CreateIssueWorkspaceService(ctx, workspaceapi.CreateIssueWorkspaceRequest{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		Owner: item.Owner, Name: item.Name, Number: item.Number,
		SuppressAutoAssign: suppressAutoAssign,
	})
	if err != nil {
		return mcpserver.Workspace{}, mcpBackendMutationError(err)
	}
	return mcpWorkspace(result), nil
}

func (b mcpBackend) CreateAdHocWorkspace(
	ctx context.Context, repo mcpserver.RepositoryIdentity, branch string,
) (mcpserver.Workspace, error) {
	resolved, err := b.resolveWorkspaceRepositoryFence(ctx, repo)
	if err != nil {
		return mcpserver.Workspace{}, err
	}
	ctx = b.routeFenceContext(ctx, resolved)
	var branchPtr *string
	if branch != "" {
		branchPtr = &branch
	}
	result, err := b.server.workspaceAPI.CreateAdHocWorkspaceService(ctx, workspaceapi.CreateAdHocWorkspaceRequest{
		Provider: repo.Provider, PlatformHost: repo.PlatformHost,
		Owner: repo.Owner, Name: repo.Name, Branch: branchPtr,
	})
	if err != nil {
		return mcpserver.Workspace{}, mcpBackendMutationError(err)
	}
	return mcpWorkspace(result), nil
}

func (b mcpBackend) LaunchWorkspaceRuntime(
	ctx context.Context, workspaceID, targetKey string,
) (mcpserver.RuntimeSession, error) {
	session, err := b.server.workspaceAPI.LaunchWorkspaceRuntimeService(ctx, workspaceID, targetKey)
	if err != nil {
		return mcpserver.RuntimeSession{}, mcpBackendMutationError(err)
	}
	return mcpserver.RuntimeSession{
		Key: session.Key, TargetKey: session.TargetKey,
		Kind: string(session.Kind), Status: string(session.Status), CreatedAt: session.CreatedAt,
	}, nil
}

func (b mcpBackend) PreferredWorkspaceAgentTarget(
	ctx context.Context, since time.Time, targetKeys []string,
) (string, bool, error) {
	target, found, err := b.server.workspaceAPI.PreferredWorkspaceAgentTargetService(
		ctx, since, targetKeys,
	)
	if err != nil {
		return "", false, mcpBackendError(err)
	}
	return target, found, nil
}

func (b mcpBackend) GetWorkspaceRuntime(
	ctx context.Context, workspaceID string,
) (mcpserver.WorkspaceRuntime, error) {
	result, err := b.server.workspaceAPI.GetWorkspaceRuntimeService(ctx, workspaceID)
	if err != nil {
		return mcpserver.WorkspaceRuntime{}, mcpBackendError(err)
	}
	out := mcpserver.WorkspaceRuntime{Sessions: make([]mcpserver.RuntimeSession, 0, len(result.Sessions))}
	for _, session := range result.Sessions {
		out.Sessions = append(out.Sessions, mcpserver.RuntimeSession{
			Key: session.Key, TargetKey: session.TargetKey,
			Kind: string(session.Kind), Status: string(session.Status), CreatedAt: session.CreatedAt,
		})
	}
	return out, nil
}

func (b mcpBackend) SubmitAgentMessage(
	ctx context.Context, req mcpserver.AgentMessageRequest,
) (mcpserver.AgentMessageResult, error) {
	result, err := b.server.workspaceAPI.SubmitAgentMessageService(
		ctx, req.WorkspaceID, req.RuntimeSessionKey, req.Message,
	)
	if errors.Is(err, workspaceapi.ErrInitialMessageInputModeNotReady) {
		return mcpserver.AgentMessageResult{}, &mcpserver.Error{
			Kind: "unavailable", Code: mcpserver.ErrorCodeInitialMessageInputModeNotReady,
			Message: err.Error(), Retryable: true,
		}
	}
	if err != nil {
		return mcpserver.AgentMessageResult{}, mcpBackendError(err)
	}
	return mcpserver.AgentMessageResult{
		TargetKey: result.TargetKey, MessageBytes: result.MessageBytes,
		SubmittedAt: result.SubmittedAt,
	}, nil
}

func (b mcpBackend) SubmitInitialMessage(
	ctx context.Context, req mcpserver.InitialMessageRequest,
) (mcpserver.InitialMessageStatus, error) {
	result, err := b.server.workspaceAPI.SubmitInitialMessageService(ctx, workspaceapi.InitialMessageRequest{
		WorkspaceID: req.WorkspaceID, RuntimeSessionKey: req.RuntimeSessionKey,
		TargetKey: req.TargetKey, Message: req.Message,
	})
	status := *mcpInitialMessage(result)
	if errors.Is(err, workspaceapi.ErrInitialMessageInputModeNotReady) {
		return status, &mcpserver.Error{
			Kind: "unavailable", Code: mcpserver.ErrorCodeInitialMessageInputModeNotReady,
			Message: err.Error(), Retryable: true,
		}
	}
	if err != nil {
		converted := mcpBackendError(err)
		if result.State == "pending" || result.State == "uncertain" {
			if backendErr, ok := errors.AsType[*mcpserver.Error](converted); ok {
				copy := *backendErr
				copy.Ambiguous = true
				copy.Retryable = false
				copy.Details = cloneMCPErrorDetails(backendErr.Details)
				copy.Details["initial_message_state"] = result.State
				converted = &copy
			}
		}
		return status, converted
	}
	return status, nil
}

func (b mcpBackend) GetInitialMessage(
	ctx context.Context, workspaceID, runtimeSessionKey string,
) (mcpserver.InitialMessageStatus, error) {
	result, err := b.server.workspaceAPI.GetInitialMessageService(ctx, workspaceID, runtimeSessionKey)
	if err != nil {
		return mcpserver.InitialMessageStatus{}, mcpBackendError(err)
	}
	return *mcpInitialMessage(result), nil
}

// resolvedMCPRepository binds a stable-identity-validated repository to either
// its spoke-local route generation or the hub authority that resolved
// it.
type resolvedMCPRepository struct {
	repo  *db.Repo
	fence db.RepositoryRouteFence
	hub   bool
}

func mcpRepositoryIdentityChangedError() error {
	return &mcpserver.Error{
		Kind: "not_found", Code: string(httpapi.CodeRepoNotFound),
		Message: "repository identity no longer matches this route",
	}
}

func (b mcpBackend) resolveRepositoryFence(
	ctx context.Context, identity mcpserver.RepositoryIdentity,
) (resolvedMCPRepository, error) {
	repo, err := b.resolveRepository(ctx, identity)
	if err != nil {
		return resolvedMCPRepository{}, err
	}
	fence, found, err := b.server.repoResolver.CaptureRepositoryRouteFence(ctx, *repo)
	if err != nil {
		return resolvedMCPRepository{}, mcpBackendError(err)
	}
	if !found {
		return resolvedMCPRepository{}, mcpRepositoryIdentityChangedError()
	}
	return resolvedMCPRepository{repo: repo, fence: fence}, nil
}

func (b mcpBackend) resolveWorkspaceRepositoryFence(
	ctx context.Context, identity mcpserver.RepositoryIdentity,
) (resolvedMCPRepository, error) {
	if b.server.providerSource == nil {
		return b.resolveRepositoryFence(ctx, identity)
	}
	if err := validateMCPRepositoryIdentity(identity); err != nil {
		return resolvedMCPRepository{}, err
	}
	descriptor, err := b.server.providerSource.GetRepositoryDescriptor(
		ctx, providerplane.RepositoryRoute{
			Provider: identity.Provider, PlatformHost: identity.PlatformHost,
			Owner: identity.Owner, Name: identity.Name,
		},
	)
	if err != nil {
		return resolvedMCPRepository{}, mcpBackendError(err)
	}
	if descriptor.PlatformRepoID != strings.TrimSpace(identity.PlatformRepoID) {
		return resolvedMCPRepository{}, mcpRepositoryIdentityChangedError()
	}
	identity.Provider = descriptor.Provider
	identity.PlatformHost = descriptor.PlatformHost
	identity.PlatformRepoID = descriptor.PlatformRepoID
	identity.Owner = descriptor.Owner
	identity.Name = descriptor.Name
	return b.resolveRepositoryFence(ctx, identity)
}

func (b mcpBackend) resolveProviderRepositoryFence(
	ctx context.Context, identity mcpserver.RepositoryIdentity,
) (resolvedMCPRepository, error) {
	if b.server.providerSource == nil {
		return b.resolveRepositoryFence(ctx, identity)
	}
	if err := validateMCPRepositoryIdentity(identity); err != nil {
		return resolvedMCPRepository{}, err
	}
	repo, err := b.server.providerSource.ResolveRepository(ctx, identity)
	if err != nil {
		return resolvedMCPRepository{}, mcpBackendError(err)
	}
	if !mcpRepositoryStableIdentityMatches(*repo, identity) {
		return resolvedMCPRepository{}, mcpRepositoryIdentityChangedError()
	}
	return resolvedMCPRepository{repo: repo, hub: true}, nil
}

// confirmRepositoryRoute fails a route-addressed read closed when repository
// reconciliation reassigned the validated route while the read was running.
// The fence generation changes on every ownership change, including
// A -> B -> A reuse, so equal captures before and after the read prove the
// read observed only the validated repository.
func (b mcpBackend) confirmRepositoryRoute(
	ctx context.Context, resolved resolvedMCPRepository,
) error {
	matches, err := b.server.repoResolver.RepositoryRouteFenceMatches(
		ctx, *resolved.repo, resolved.fence,
	)
	if err != nil {
		return mcpBackendError(err)
	}
	if !matches {
		return mcpRepositoryIdentityChangedError()
	}
	return nil
}

func (b mcpBackend) confirmProviderRepositoryRoute(
	ctx context.Context, resolved resolvedMCPRepository,
) error {
	if !resolved.hub {
		return b.confirmRepositoryRoute(ctx, resolved)
	}
	identity := mcpserver.RepositoryIdentity{
		Provider: resolved.repo.Platform, PlatformHost: resolved.repo.PlatformHost,
		PlatformRepoID: resolved.repo.PlatformRepoID,
		Owner:          resolved.repo.Owner, Name: resolved.repo.Name,
	}
	repo, err := b.server.providerSource.ResolveRepository(ctx, identity)
	if err != nil {
		return mcpBackendError(err)
	}
	if !mcpRepositoryStableIdentityMatches(*repo, identity) {
		return mcpRepositoryIdentityChangedError()
	}
	return nil
}

// routeFenceContext binds the validated route generation to the request so
// every downstream database write re-validates it under the reconciliation
// read lock, failing workspace mutations closed instead of persisting rows
// for a replacement repository that took over the route mid-request.
func (b mcpBackend) routeFenceContext(
	ctx context.Context, resolved resolvedMCPRepository,
) context.Context {
	repo := resolved.repo
	return b.server.db.WithRepositoryRouteFence(ctx, db.RepoIdentity{
		Platform: repo.Platform, PlatformHost: repo.PlatformHost,
		PlatformRepoID: repo.PlatformRepoID,
		Owner:          repo.Owner, Name: repo.Name, RepoPath: repo.RepoPath,
	}, resolved.fence)
}

func (b mcpBackend) resolveRepository(
	ctx context.Context, identity mcpserver.RepositoryIdentity,
) (*db.Repo, error) {
	if err := validateMCPRepositoryIdentity(identity); err != nil {
		return nil, err
	}
	repo, err := b.server.repoResolver.LookupRoute(
		ctx, identity.Provider, identity.PlatformHost, identity.Owner, identity.Name,
	)
	if err != nil {
		return nil, mcpBackendError(httpapi.ProviderRouteLookupError(err))
	}
	if repo.PlatformRepoID != strings.TrimSpace(identity.PlatformRepoID) {
		return nil, &mcpserver.Error{
			Kind: "not_found", Code: string(httpapi.CodeRepoNotFound),
			Message: "repository identity no longer matches this route",
		}
	}
	return repo, nil
}

func validateMCPRepositoryIdentity(identity mcpserver.RepositoryIdentity) error {
	if strings.TrimSpace(identity.Provider) == "" {
		return &mcpserver.Error{
			Kind: "invalid_request", Code: string(httpapi.CodeValidationError),
			Message: "provider is required",
		}
	}
	if strings.TrimSpace(identity.PlatformRepoID) == "" {
		return &mcpserver.Error{
			Kind: "invalid_request", Code: string(httpapi.CodeValidationError),
			Message: "platform_repo_id is required",
		}
	}
	return nil
}

func mcpRepositoryStableIdentityMatches(
	repo db.Repo, identity mcpserver.RepositoryIdentity,
) bool {
	actual := providerplane.RepositoryIdentity{
		Provider: repo.Platform, PlatformHost: repo.PlatformHost,
		PlatformRepoID: repo.PlatformRepoID,
	}.Canonical()
	expected := providerplane.RepositoryIdentity{
		Provider: identity.Provider, PlatformHost: identity.PlatformHost,
		PlatformRepoID: identity.PlatformRepoID,
	}.Canonical()
	return actual.Valid() && actual == expected
}

func itemRepositoryIdentity(item mcpserver.ItemIdentity) mcpserver.RepositoryIdentity {
	return mcpserver.RepositoryIdentity{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		PlatformRepoID: item.PlatformRepoID,
		Owner:          item.Owner, Name: item.Name,
	}
}

func mcpBackendError(err error) error {
	if err == nil {
		return nil
	}
	if existing, ok := errors.AsType[*mcpserver.Error](err); ok {
		return existing
	}
	var problem *httpapi.ProblemError
	if !errors.As(err, &problem) {
		return &mcpserver.Error{Kind: "internal_error", Message: err.Error()}
	}
	kind := "internal_error"
	retryable := false
	switch problem.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		kind = "invalid_request"
	case http.StatusUnauthorized:
		kind = "unauthorized"
	case http.StatusForbidden:
		kind = "forbidden"
	case http.StatusNotFound:
		kind = "not_found"
	case http.StatusConflict:
		kind = "conflict"
	case http.StatusTooManyRequests:
		kind = "rate_limited"
		retryable = true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = "unavailable"
		retryable = true
	}
	ambiguous := problem.Code == httpapi.CodeMutationOutcomeUnknown
	if ambiguous {
		retryable = false
	}
	return &mcpserver.Error{
		Kind: kind, Code: string(problem.Code), Message: problem.Error(),
		Retryable: retryable, Ambiguous: ambiguous, Details: problem.Details,
	}
}

func mcpBackendMutationError(err error) error {
	converted := mcpBackendError(err)
	var backendErr *mcpserver.Error
	if !errors.As(converted, &backendErr) || backendErr.Kind != "internal_error" {
		return converted
	}
	copy := *backendErr
	copy.Ambiguous = true
	copy.Retryable = false
	copy.Details = cloneMCPErrorDetails(backendErr.Details)
	return &copy
}

func normalizeMCPWorkflowStatus(status string) db.KanbanStatus {
	switch db.KanbanStatus(status) {
	case db.KanbanStatusNew, db.KanbanStatusReviewing,
		db.KanbanStatusWaiting, db.KanbanStatusAwaitingMerge:
		return db.KanbanStatus(status)
	default:
		return db.KanbanStatusNew
	}
}

func repositoryIdentityFromResponse(repo httpapi.RepoRefResponse) mcpserver.RepositoryIdentity {
	return mcpserver.RepositoryIdentity{
		Provider: repo.Provider, PlatformHost: repo.PlatformHost,
		PlatformRepoID: repo.PlatformRepoID,
		RepoPath:       repo.RepoPath, Owner: repo.Owner, Name: repo.Name,
	}
}

func mcpRepositoryFilter(repo mcpserver.RepositoryIdentity) string {
	if repo.Provider == "" {
		return ""
	}
	path := strings.Trim(repo.RepoPath, "/")
	if path == "" {
		path = strings.Trim(repo.Owner, "/") + "/" + strings.Trim(repo.Name, "/")
	}
	return repo.Provider + "|" + repo.PlatformHost + "/" + path
}

func mcpRepoFilters(repo mcpserver.RepositoryIdentity) []db.RepoFilter {
	if repo.Provider == "" {
		return nil
	}
	return []db.RepoFilter{{
		Platform: repo.Provider, PlatformHost: repo.PlatformHost,
		PlatformRepoID: repo.PlatformRepoID,
		RepoPath:       repo.RepoPath, RepoOwner: repo.Owner, RepoName: repo.Name,
	}}
}

func mcpWorkspaceRef(ref *workspaceapi.WorkspaceRef) *mcpserver.WorkspaceRef {
	if ref == nil {
		return nil
	}
	return &mcpserver.WorkspaceRef{ID: ref.ID, Status: ref.Status}
}

func mcpPull(row pullapi.MergeRequestResponse) mcpserver.Pull {
	return mcpserver.Pull{
		Number: row.Number, Title: row.Title, State: string(row.State),
		Author: row.Author, URL: row.URL, IsDraft: row.IsDraft, Body: row.Body,
		WorkflowStatus: string(row.KanbanStatus), LastActivityAt: row.LastActivityAt,
		Repository:   repositoryIdentityFromResponse(row.Repo),
		Workspace:    mcpWorkspaceRef(row.Workspace),
		DetailLoaded: row.DetailLoaded, DetailFetchedAt: row.DetailFetchedAt,
	}
}

func mcpIssue(row issueapi.IssueResponse) mcpserver.Issue {
	return mcpserver.Issue{
		Number: row.Number, Title: row.Title, State: row.State,
		Author: row.Author, URL: row.URL, Body: row.Body,
		WorkflowStatus: string(row.WorkflowStatus), LastActivityAt: row.LastActivityAt,
		Repository:   repositoryIdentityFromResponse(row.Repo),
		Workspace:    mcpWorkspaceRef(row.Workspace),
		DetailLoaded: row.DetailLoaded, DetailFetchedAt: row.DetailFetchedAt,
	}
}

func pullServiceIdentity(item mcpserver.ItemIdentity) pullapi.ItemIdentity {
	return pullapi.ItemIdentity{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		Owner: item.Owner, Name: item.Name, Number: item.Number,
	}
}

func issueServiceIdentity(item mcpserver.ItemIdentity) issueapi.ItemIdentity {
	return issueapi.ItemIdentity{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		Owner: item.Owner, Name: item.Name, Number: item.Number,
	}
}

func mcpDiffFiles(files []gitclone.DiffFile) []mcpserver.DiffFile {
	out := make([]mcpserver.DiffFile, 0, len(files))
	for _, file := range files {
		out = append(out, mcpserver.DiffFile{
			Path: file.Path, OldPath: file.OldPath, Status: file.Status,
			IsBinary: file.IsBinary, IsGenerated: file.IsGenerated,
			Additions: file.Additions, Deletions: file.Deletions, Patch: file.Patch,
		})
	}
	return out
}

func mcpStack(stack pullapi.StackContext) mcpserver.Stack {
	out := mcpserver.Stack{
		Position: stack.Position, Size: stack.Size, Health: stack.Health,
		Members: make([]mcpserver.StackMember, 0, len(stack.Members)),
	}
	for _, member := range stack.Members {
		out.Members = append(out.Members, mcpserver.StackMember{
			Number: member.Number, Title: member.Title, State: member.State,
			Position: member.Position, IsDraft: member.IsDraft,
		})
	}
	return out
}

func mcpWorkspace(result workspaceapi.WorkspaceResult) mcpserver.Workspace {
	workspace := result.Workspace
	return mcpserver.Workspace{
		ID: workspace.ID, Status: workspace.Status, Created: workspace.Created,
		GitHeadRef: workspace.GitHeadRef, ErrorMessage: workspace.ErrorMessage,
	}
}

func cloneMCPErrorDetails(details map[string]any) map[string]any {
	cloned := maps.Clone(details)
	if cloned == nil {
		cloned = make(map[string]any)
	}
	return cloned
}

func mcpInitialMessage(result workspaceapi.InitialMessageResult) *mcpserver.InitialMessageStatus {
	return &mcpserver.InitialMessageStatus{
		State: result.State, MessageBytes: result.MessageBytes,
		DeliveredAt: result.DeliveredAt,
	}
}

var _ mcpserver.Backend = mcpBackend{}
