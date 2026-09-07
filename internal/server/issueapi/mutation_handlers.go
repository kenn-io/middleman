package issueapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server/httpapi"
)

func (s *Handler) postIssueComment(ctx context.Context, input *postIssueCommentInput) (*postIssueCommentOutput, error) {
	if strings.TrimSpace(input.Body.Body) == "" {
		return nil, httpapi.Validation("body.body", "comment body must not be empty")
	}
	repo, err := s.resolver.RequireRouteCapability(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}
	issueID, err := s.lookupIssueID(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}
	mutator, err := s.syncer.CommentMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityCommentMutation)
	}
	providerEvent, err := mutator.CreateIssueComment(ctx, httpapi.PlatformRepoRef(*repo), input.Number, input.Body.Body)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo),
			"create comment on provider failed",
		)
	}
	event := platformdb.DBIssueEvent(issueID, providerEvent)
	// Preserve the established best-effort local write: provider success is
	// authoritative and the next detail sync can recover a failed cache write.
	_ = s.db.UpsertIssueEvents(ctx, []db.IssueEvent{event})
	return &postIssueCommentOutput{Status: http.StatusCreated, Body: event}, nil
}

func (s *Handler) editIssueComment(ctx context.Context, input *editIssueCommentInput) (*editIssueCommentOutput, error) {
	if strings.TrimSpace(input.Body.Body) == "" {
		return nil, httpapi.Validation("body.body", "comment body must not be empty")
	}
	repo, err := s.resolver.RequireRouteCapability(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}
	issueID, err := s.lookupIssueID(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}
	mutator, err := s.syncer.CommentMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityCommentMutation)
	}
	exists, err := s.db.IssueCommentEventExists(ctx, issueID, input.CommentID)
	if err != nil {
		return nil, httpapi.Internal("validate comment target failed")
	}
	if !exists {
		return nil, httpapi.NotFound(httpapi.CodeCommentNotFound, "comment not found for issue", nil)
	}
	providerEvent, err := mutator.EditIssueComment(
		ctx, httpapi.PlatformRepoRef(*repo), input.Number, input.CommentID, input.Body.Body,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo),
			"edit comment on provider failed",
		)
	}
	providerEvent.IssueNumber = input.Number
	event := platformdb.DBIssueEvent(issueID, providerEvent)
	existingEvents, err := s.db.ListIssueEvents(ctx, issueID)
	if err != nil {
		return nil, httpapi.Internal("load existing comment metadata failed")
	}
	for _, existing := range existingEvents {
		if existing.EventType == "issue_comment" && existing.PlatformID != nil &&
			*existing.PlatformID == input.CommentID {
			event.MetadataJSON = platformdb.PreserveProviderHiddenMetadata(
				existing.MetadataJSON, event.MetadataJSON,
			)
			break
		}
	}
	if err := s.db.UpsertIssueEvents(ctx, []db.IssueEvent{event}); err != nil {
		return nil, httpapi.Internal("persist edited comment failed")
	}
	return &editIssueCommentOutput{Body: event}, nil
}

func (s *Handler) deleteIssueComment(ctx context.Context, input *deleteIssueCommentInput) (*deleteIssueCommentOutput, error) {
	repo, err := s.resolver.RequireRouteCapability(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}
	issueID, err := s.lookupIssueID(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}
	mutator, err := s.syncer.CommentMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityCommentMutation)
	}
	exists, err := s.db.IssueCommentEventExists(ctx, issueID, input.CommentID)
	if err != nil {
		return nil, httpapi.Internal("validate comment target failed")
	}
	if !exists {
		return nil, httpapi.NotFound(httpapi.CodeCommentNotFound, "comment not found for issue", nil)
	}
	if err := mutator.DeleteIssueComment(ctx, httpapi.PlatformRepoRef(*repo), input.Number, input.CommentID); err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo),
			"delete comment on provider failed",
		)
	}
	return &deleteIssueCommentOutput{Status: http.StatusNoContent}, nil
}

func (s *Handler) lookupIssueID(ctx context.Context, repo *db.Repo, number int) (int64, error) {
	issue, err := s.requireVisibleIssue(ctx, repo, number)
	if err != nil {
		return 0, err
	}
	return issue.ID, nil
}

func (s *Handler) requireVisibleIssue(
	ctx context.Context,
	repo *db.Repo,
	number int,
) (*db.Issue, error) {
	issue, err := s.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, number)
	if err != nil {
		return nil, httpapi.Internal("get issue failed: " + err.Error())
	}
	if issue == nil {
		return nil, httpapi.NotFound(
			httpapi.CodeIssueNotFound,
			fmt.Sprintf("issue %s/%s#%d on %s not found", repo.Owner, repo.Name, number, repo.PlatformHost),
			nil,
		)
	}
	return issue, nil
}

func (s *Handler) setIssueLabels(ctx context.Context, input *setIssueLabelsInput) (*setLabelsOutput, error) {
	repo, issue, names, err := s.resolveRequestedLabelNames(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
		input.Number, input.Body.LabelNames(),
	)
	if err != nil {
		return nil, err
	}
	if s.syncer == nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityLabelMutation)
	}
	mutator, err := s.syncer.LabelMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityLabelMutation)
	}
	providerLabels, err := mutator.SetIssueLabels(ctx, httpapi.PlatformRepoRef(*repo), input.Number, names)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo), "provider API error: "+err.Error(),
		)
	}
	labels := platformdb.DBLabels(providerLabels, time.Now().UTC())
	if err := s.db.ReplaceIssueLabels(ctx, repo.ID, issue.ID, labels); err != nil {
		return nil, httpapi.Internal("save issue labels failed")
	}
	stored, err := s.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, input.Number)
	if err != nil || stored == nil {
		return nil, httpapi.Internal("get issue failed")
	}
	return &setLabelsOutput{Body: httpapi.ItemLabelsResponse{Labels: stored.Labels}}, nil
}

func (s *Handler) resolveRequestedLabelNames(
	ctx context.Context,
	provider, platformHost, owner, name string,
	number int,
	names []string,
) (*db.Repo, *db.Issue, []string, error) {
	repo, err := s.resolver.LookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, nil, nil, httpapi.ProviderRouteLookupError(err)
	}
	caps := s.resolver.CapabilitiesForRepo(*repo)
	if !httpapi.CapabilityEnabled(caps, capabilityReadLabels) {
		return nil, nil, nil, httpapi.UnsupportedCapability(*repo, capabilityReadLabels)
	}
	if !httpapi.CapabilityEnabled(caps, capabilityLabelMutation) {
		return nil, nil, nil, httpapi.UnsupportedCapability(*repo, capabilityLabelMutation)
	}
	issue, err := s.requireVisibleIssue(ctx, repo, number)
	if err != nil {
		return nil, nil, nil, err
	}
	if names == nil {
		return nil, nil, nil, httpapi.Validation("body.labels", "labels must be an array")
	}
	catalog, freshness, err := s.db.ListRepoLabelCatalog(ctx, repo.ID)
	if err != nil {
		return nil, nil, nil, httpapi.Internal("list repo labels failed")
	}
	if labelCatalogStale(freshness, time.Now().UTC()) && s.syncer != nil {
		_ = s.syncer.RefreshRepoLabelCatalog(ctx, *repo)
		catalog, _, err = s.db.ListRepoLabelCatalog(ctx, repo.ID)
		if err != nil {
			return nil, nil, nil, httpapi.Internal("list repo labels failed")
		}
	}
	catalogByName := make(map[string]struct{}, len(catalog))
	for _, label := range catalog {
		catalogByName[label.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(names))
	resolved := make([]string, 0, len(names))
	for _, raw := range names {
		label := strings.TrimSpace(raw)
		if label == "" {
			return nil, nil, nil, httpapi.Validation("body.labels", "label names must not be empty")
		}
		if _, ok := seen[label]; ok {
			return nil, nil, nil, httpapi.Validation("body.labels", fmt.Sprintf("duplicate label %q", label))
		}
		if _, ok := catalogByName[label]; !ok {
			return nil, nil, nil, httpapi.NewProblem(
				http.StatusBadRequest, httpapi.CodeValidationError,
				fmt.Sprintf("label %q is not in the repository label catalog", label),
				map[string]any{"field": "body.labels", "label": label},
			)
		}
		seen[label] = struct{}{}
		resolved = append(resolved, label)
	}
	return repo, issue, resolved, nil
}

func (s *Handler) setIssueAssignees(ctx context.Context, input *setIssueAssigneesInput) (*setAssigneesOutput, error) {
	repo, names, err := s.resolveAssignees(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Body.Assignees,
	)
	if err != nil {
		return nil, err
	}
	issue, err := s.requireVisibleIssue(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}
	mutator, err := s.syncer.AssigneeMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityAssigneeMutation)
	}
	assignees, err := mutator.SetIssueAssignees(ctx, httpapi.PlatformRepoRef(*repo), input.Number, names)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo), "provider API error: "+err.Error(),
		)
	}
	if err := s.db.UpdateIssueAssignees(ctx, repo.ID, issue.ID, assignees); err != nil {
		return nil, httpapi.Internal("save issue assignees failed")
	}
	return &setAssigneesOutput{Body: httpapi.ItemAssigneesResponse{Assignees: emptyIfNil(assignees)}}, nil
}

func (s *Handler) resolveAssignees(
	ctx context.Context,
	provider, platformHost, owner, name string,
	raw *[]string,
) (*db.Repo, []string, error) {
	repo, err := s.resolver.RequireRouteCapability(
		ctx, provider, platformHost, owner, name, capabilityAssigneeMutation,
	)
	if err != nil {
		return nil, nil, err
	}
	if s.syncer == nil {
		return nil, nil, httpapi.UnsupportedCapability(*repo, capabilityAssigneeMutation)
	}
	if raw == nil {
		return nil, nil, httpapi.Validation("body.assignees", "value must be an array of usernames")
	}
	seen := make(map[string]struct{}, len(*raw))
	resolved := make([]string, 0, len(*raw))
	for _, value := range *raw {
		username := strings.TrimSpace(value)
		if username == "" {
			return nil, nil, httpapi.Validation("body.assignees", "usernames must not be empty")
		}
		key := strings.ToLower(username)
		if _, ok := seen[key]; ok {
			return nil, nil, httpapi.Validation("body.assignees", fmt.Sprintf("duplicate username %q", username))
		}
		seen[key] = struct{}{}
		resolved = append(resolved, username)
	}
	return repo, resolved, nil
}

func (s *Handler) setIssueGitHubState(ctx context.Context, input *githubStateInput) (*githubStateOutput, error) {
	if input.Body.State != "open" && input.Body.State != "closed" {
		return nil, httpapi.Validation("body.state", "state must be 'open' or 'closed'", "open", "closed")
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
	mutator, err := s.syncer.StateMutator(httpapi.ProviderKind(*repo), httpapi.ProviderHost(*repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, capabilityStateMutation)
	}
	if _, err := mutator.SetIssueState(ctx, httpapi.PlatformRepoRef(*repo), input.Number, input.Body.State); err != nil {
		var githubError *gh.ErrorResponse
		if errors.As(err, &githubError) && githubError != nil && githubError.Response != nil &&
			githubError.Response.StatusCode == http.StatusUnprocessableEntity {
			client, clientErr := s.syncer.ClientForHost(repo.PlatformHost)
			if clientErr != nil {
				return nil, httpapi.ProviderCallProblemWithDetail(
					clientErr, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo),
					"GitHub API error: "+err.Error(),
				)
			}
			githubIssue, fetchErr := client.GetIssue(ctx, input.Owner, input.Name, input.Number)
			if fetchErr == nil {
				if githubIssue == nil {
					return nil, httpapi.Upstream("GitHub API returned no issue", string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo))
				}
				normalized, normalizeErr := ghclient.NormalizeIssue(repo.ID, githubIssue)
				if normalizeErr != nil {
					return nil, httpapi.Upstream("GitHub API error: "+normalizeErr.Error(), string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo))
				}
				_, _ = s.db.UpsertIssue(ctx, normalized)
				s.markClosedLinkedNotificationsDone(ctx)
				if githubIssue.GetState() == input.Body.State {
					output := &githubStateOutput{}
					output.Body.State = input.Body.State
					return output, nil
				}
			}
		}
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(httpapi.ProviderKind(*repo)), httpapi.ProviderHost(*repo), "GitHub API error: "+err.Error(),
		)
	}
	var closedAt *time.Time
	if input.Body.State == "closed" {
		now := s.now().UTC()
		closedAt = &now
	}
	if err := s.db.UpdateIssueState(ctx, repo.ID, issue.Number, input.Body.State, closedAt); err != nil {
		return nil, httpapi.Internal("update issue state: " + err.Error())
	}
	if input.Body.State == "closed" {
		s.markClosedLinkedNotificationsDone(ctx)
	}
	output := &githubStateOutput{}
	output.Body.State = input.Body.State
	return output, nil
}
