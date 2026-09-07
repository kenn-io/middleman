package issueapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"go.kenn.io/forge/internal/platformdb"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
)

func (s *Handler) listIssues(ctx context.Context, input *listIssuesInput) (*listIssuesOutput, error) {
	query := ListQuery{
		Repo: input.Repo, State: input.State, Starred: input.Starred,
		InvolvesMe: input.InvolvesMe, Unassigned: input.Unassigned, ReferencedByPR: input.ReferencedByPR,
		Text: input.Q, Assignee: input.Assignee, Limit: input.Limit, Offset: input.Offset,
	}
	var rows []IssueResponse
	var err error
	if _, federationRequest := federationauth.PrincipalFromContext(ctx); federationRequest {
		rows, err = s.ListProviderService(ctx, query)
	} else {
		rows, err = s.ListService(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	return &listIssuesOutput{Body: rows}, nil
}

func (s *Handler) listIssuesRouteCore(ctx context.Context, input *listIssuesInput) (*listIssuesOutput, error) {
	if input.State != "" && input.State != "open" && input.State != "closed" && input.State != "all" {
		return nil, httpapi.Validation("query.state", "state must be one of: open, closed, all", "open", "closed", "all")
	}
	if hasInvalidRepoFilter(input.Repo) {
		return nil, httpapi.Validation("query.repo", "repo filter must be provider|platform_host/repo_path")
	}
	snapshot := workspaceapi.WorkspaceSubjectSnapshot{
		OwnReferences: map[db.WorkspaceSubjectKey]workspaceapi.WorkspaceRef{},
		Subjects:      map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{},
	}
	var err error
	if s.workspaceSubjects != nil {
		snapshot, err = s.workspaceSubjects(ctx)
		if err != nil {
			return nil, httpapi.Internal("load workspace activity failed")
		}
	}
	var overrides []db.ItemActivityOverride
	if s.ConfigSnapshot().UseWorkspaceActivityForRecency {
		overrides = make([]db.ItemActivityOverride, 0, len(snapshot.Subjects))
		for key, activity := range snapshot.Subjects {
			if key.ItemType == db.WorkspaceItemTypeIssue && activity.ActivityAt != nil {
				overrides = append(overrides, db.ItemActivityOverride{
					RepoID: key.RepoID, ItemNumber: key.ItemNumber, ActivityAt: *activity.ActivityAt,
				})
			}
		}
	}
	opts := db.ListIssuesOpts{
		State: input.State, Search: input.Q, Starred: input.Starred,
		Unassigned: input.Unassigned, Assignee: input.Assignee, Limit: input.Limit, Offset: input.Offset,
		RepoFilters:       parseRepoFilters(input.Repo),
		WorkspaceActivity: overrides,
		ReferencedByPR:    input.ReferencedByPR,
	}
	if input.InvolvesMe {
		if s.viewerLogins == nil {
			return nil, httpapi.Internal("authenticated viewer lookup unavailable")
		}
		opts.ViewerLogins, err = s.viewerLogins(ctx, opts.RepoFilters)
		if err != nil {
			return nil, err
		}
	}
	issues, err := s.db.ListIssues(ctx, opts)
	if err != nil {
		return nil, httpapi.Internal("list issues failed")
	}
	repos, err := s.lookupRepoMap(ctx)
	if err != nil {
		return nil, httpapi.Internal("repo lookup failed")
	}
	out := make([]IssueResponse, 0, len(issues))
	for _, issue := range issues {
		repo, ok := repos[issue.RepoID]
		if !ok {
			continue
		}
		key := db.WorkspaceSubjectKey{RepoID: issue.RepoID, ItemType: db.WorkspaceItemTypeIssue, ItemNumber: issue.Number}
		var workspaceRef *workspaceapi.WorkspaceRef
		if ref, ok := snapshot.OwnReferences[key]; ok {
			copy := ref
			workspaceRef = &copy
		}
		response := IssueResponse{
			Issue: issueResponseModel(issue), Repo: s.resolver.Ref(repo),
			PlatformHost: repo.PlatformHost, RepoOwner: repo.Owner, RepoName: repo.Name,
			Workspace:    workspaceRef,
			DetailLoaded: issue.DetailFetchedAt != nil,
		}
		if activity, ok := snapshot.Subjects[key]; ok && activity.ActivityAt != nil {
			response.LastWorkspaceActivityAt = formatUTCRFC3339(*activity.ActivityAt)
		}
		if issue.DetailFetchedAt != nil {
			response.DetailFetchedAt = formatUTCRFC3339(*issue.DetailFetchedAt)
		}
		out = append(out, response)
	}
	return &listIssuesOutput{Body: out}, nil
}

func (s *Handler) createIssue(ctx context.Context, input *createIssueInput) (*createIssueOutput, error) {
	title := strings.TrimSpace(input.Body.Title)
	if title == "" {
		return nil, httpapi.Validation("body.title", "issue title must not be empty")
	}
	repo, err := s.resolver.RequireRouteCapability(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityIssueMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityIssueMutation); err != nil {
		return nil, err
	}
	mutator, err := s.syncer.IssueMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityIssueMutation)
	}
	providerIssue, err := mutator.CreateIssue(ctx, httpapi.PlatformRepoRef(*repo), title, input.Body.Body)
	if err != nil {
		return nil, httpapi.ProviderMutationProblem(err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo))
	}
	issue := platformdb.DBIssue(repo.ID, providerIssue)
	issueID, err := s.db.UpsertIssue(ctx, issue)
	if err != nil {
		return nil, createIssuePersistenceProblem(*repo)
	}
	if err := s.db.ReplaceIssueLabels(ctx, repo.ID, issueID, issue.Labels); err != nil {
		return nil, createIssuePersistenceProblem(*repo)
	}
	saved, err := s.db.GetIssueByRepoIDAndNumber(ctx, repo.ID, issue.Number)
	if err != nil || saved == nil {
		return nil, createIssuePersistenceProblem(*repo)
	}
	saved.ID = issueID
	response := IssueResponse{
		Issue: issueResponseModel(*saved), Repo: s.resolver.Ref(*repo),
		PlatformHost: repo.PlatformHost, RepoOwner: repo.Owner, RepoName: repo.Name,
		DetailLoaded: saved.DetailFetchedAt != nil,
	}
	if saved.DetailFetchedAt != nil {
		response.DetailFetchedAt = formatUTCRFC3339(*saved.DetailFetchedAt)
	}
	return &createIssueOutput{Status: http.StatusCreated, Body: response}, nil
}

func createIssuePersistenceProblem(repo db.Repo) error {
	return httpapi.MutationOutcomeUnknown(
		"The provider created the issue, but kenn-forge could not confirm its local state.",
		string(httpapi.ProviderKind(repo)),
		httpapi.ProviderHost(repo),
	)
}

func (s *Handler) getIssue(ctx context.Context, input *issueRepoNumberInput) (*getIssueOutput, error) {
	item := ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	}
	var body IssueDetailResponse
	var err error
	if _, federationRequest := federationauth.PrincipalFromContext(ctx); federationRequest {
		body, err = s.GetProviderService(ctx, item)
	} else {
		body, err = s.GetService(ctx, item)
	}
	if err != nil {
		return nil, err
	}
	return &getIssueOutput{Body: body}, nil
}

func (s *Handler) getIssueRouteCore(ctx context.Context, input *issueRepoNumberInput) (*getIssueOutput, error) {
	repo, err := s.resolver.LookupRoute(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name)
	if err != nil {
		return nil, httpapi.ProviderRouteLookupError(err)
	}
	issue, err := s.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get issue failed")
	}
	if issue == nil {
		return nil, httpapi.NotFound(httpapi.CodeIssueNotFound, "issue not found", nil)
	}
	response, err := s.BuildDetail(ctx, repo, issue)
	if err != nil {
		return nil, err
	}
	return &getIssueOutput{Body: response}, nil
}

func (s *Handler) BuildDetail(ctx context.Context, repo *db.Repo, issue *db.Issue) (IssueDetailResponse, error) {
	events, err := s.db.ListIssueEvents(ctx, issue.ID)
	if err != nil {
		return IssueDetailResponse{}, httpapi.Internal("list issue events failed")
	}
	if events == nil {
		events = []db.IssueEvent{}
	}
	workflow, err := s.issueWorkflowMetaResponse(ctx, repo.ID, issue.Number)
	if err != nil {
		return IssueDetailResponse{}, err
	}
	model := issueResponseModel(*issue)
	ref := s.resolver.Ref(*repo)
	operations := s.operations(*repo)
	ref.Operations = &operations
	response := IssueDetailResponse{
		Issue: &model, Events: events, Repo: ref,
		PlatformHost: repo.PlatformHost, RepoOwner: repo.Owner, RepoName: repo.Name,
		DetailLoaded: issue.DetailFetchedAt != nil, Workflow: workflow,
	}
	if issue.DetailFetchedAt != nil {
		response.DetailFetchedAt = formatUTCRFC3339(*issue.DetailFetchedAt)
	}
	if s.workspaceSubjects != nil {
		snapshot, snapshotErr := s.workspaceSubjects(ctx)
		if snapshotErr != nil {
			slog.Warn(
				"load workspace activity for issue detail failed",
				"issue_id", issue.ID, "err", snapshotErr,
			)
		} else {
			key := db.WorkspaceSubjectKey{
				RepoID: repo.ID, ItemType: db.WorkspaceItemTypeIssue, ItemNumber: issue.Number,
			}
			if workspaceRef, ok := snapshot.OwnReferences[key]; ok {
				response.Workspace = &workspaceRef
			}
		}
	}
	return response, nil
}

func (s *Handler) issueWorkflowMetaResponse(ctx context.Context, repoID int64, number int) (*WorkflowStateMetaResponse, error) {
	row, err := s.db.GetItemWorkflowState(ctx, repoID, db.ItemTypeIssue, number)
	if err != nil {
		return nil, httpapi.Internal("read issue workflow state failed")
	}
	if row == nil {
		return &WorkflowStateMetaResponse{Status: db.KanbanStatusNew}, nil
	}
	return &WorkflowStateMetaResponse{
		Status:    normalizeWorkflowStatus(row.Status, "repo_id", repoID, "item_type", db.ItemTypeIssue, "item_number", number),
		UpdatedAt: formatUTCRFC3339(row.UpdatedAt), UpdatedSource: row.UpdatedSource,
		UpdatedActor: row.UpdatedActor, UpdatedReason: row.UpdatedReason,
	}, nil
}

func (s *Handler) editIssueContent(ctx context.Context, input *editIssueContentInput) (*editIssueContentOutput, error) {
	if input.Body.Title == nil && input.Body.Body == nil {
		return nil, httpapi.Validation("body", "at least one of title or body must be provided")
	}
	if input.Body.Title != nil && strings.TrimSpace(*input.Body.Title) == "" {
		return nil, httpapi.Validation("body.title", "title must not be blank")
	}
	repo, err := s.resolver.RequireRouteCapability(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityStateMutation,
	)
	if err != nil {
		return nil, err
	}
	issue, err := s.requireVisibleIssue(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityStateMutation); err != nil {
		return nil, err
	}
	mutator, err := s.syncer.IssueContentMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityStateMutation)
	}
	updated, err := mutator.EditIssueContent(ctx, httpapi.PlatformRepoRef(*repo), input.Number, input.Body.Title, input.Body.Body)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo), "provider API error: "+err.Error())
	}
	newTitle := issue.Title
	if updated.Title != "" {
		newTitle = updated.Title
	} else if input.Body.Title != nil {
		newTitle = *input.Body.Title
	}
	newBody := issue.Body
	if updated.Body != "" {
		newBody = updated.Body
	} else if input.Body.Body != nil {
		newBody = *input.Body.Body
	}
	updatedAt := s.now().UTC()
	if !updated.UpdatedAt.IsZero() {
		updatedAt = updated.UpdatedAt.UTC()
	}
	if err := s.db.UpdateIssueTitleBody(ctx, issue.ID, newTitle, newBody, updatedAt); err != nil {
		return nil, httpapi.Internal("update title/body failed")
	}
	issue, err = s.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, input.Number)
	if err != nil || issue == nil {
		return nil, httpapi.Internal("re-read issue failed")
	}
	response, err := s.BuildDetail(ctx, repo, issue)
	if err != nil {
		return nil, err
	}
	return &editIssueContentOutput{Body: response}, nil
}
