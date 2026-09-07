package workspaceapi

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

func (s *Handler) autoAssignWorkspaceItem(
	ctx context.Context,
	repo db.Repo,
	number int,
	issue bool,
	suppress bool,
) error {
	if suppress || !s.configSnapshot().AutoAssignOnCreate {
		return nil
	}
	return s.applyWorkspaceAutoAssignment(ctx, repo, number, issue)
}

func (s *Handler) applyWorkspaceAutoAssignment(
	ctx context.Context, repo db.Repo, number int, issue bool,
) error {
	if s.syncer == nil {
		return nil
	}
	if s.providerWriteGate != nil {
		release, err := s.providerWriteGate.Admit(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	if issue {
		stored, err := s.db.GetVisibleIssueByRepoIDAndNumber(ctx, repo.ID, number)
		if err != nil {
			return fmt.Errorf("check issue visibility: %w", err)
		}
		if stored == nil {
			return fmt.Errorf("issue %d is not visible", number)
		}
	} else {
		stored, err := s.db.GetVisibleMergeRequestByRepoIDAndNumber(ctx, repo.ID, number)
		if err != nil {
			return fmt.Errorf("check pull request visibility: %w", err)
		}
		if stored == nil {
			return fmt.Errorf("pull request %d is not visible", number)
		}
	}

	kind := httpapi.ProviderKind(repo)
	host := httpapi.ProviderHost(repo)
	caps, err := s.syncer.ProviderCapabilities(kind, host)
	if err != nil {
		return fmt.Errorf("load provider capabilities: %w", err)
	}
	if !caps.AssigneeMutation || !caps.ReadAuthenticatedUser {
		return nil
	}

	registry := s.syncer.Registry()
	userResolver, err := registry.AuthenticatedUserResolver(kind, host)
	if err != nil {
		return fmt.Errorf("resolve authenticated user capability: %w", err)
	}
	mutator, err := registry.AssigneeMutator(kind, host)
	if err != nil {
		return fmt.Errorf("resolve assignee capability: %w", err)
	}
	ref := httpapi.PlatformRepoRef(repo)
	username, err := userResolver.AuthenticatedUser(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve authenticated user: %w", err)
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("authenticated username is empty")
	}

	if issue {
		return s.assignIssueWorkspaceUser(ctx, repo, ref, number, username, registry, mutator)
	}
	return s.assignPullWorkspaceUser(ctx, repo, ref, number, username, registry, mutator)
}

func (s *Handler) autoAssignWorkspaceItemForRoute(
	ctx context.Context,
	request ProviderWorkspaceItemRequest,
	suppress bool,
) error {
	if suppress || !s.configSnapshot().AutoAssignOnCreate {
		return nil
	}
	if s.providerWorkspaceAutomation != nil {
		return s.providerWorkspaceAutomation.AutoAssignWorkspaceItem(ctx, request)
	}
	return s.AutoAssignProviderWorkspaceItem(ctx, request)
}

// AutoAssignProviderWorkspaceItem applies hub-owned assignment policy.
// Federation callers reach it through the provider plane; local workspaces use
// the same service directly.
func (s *Handler) AutoAssignProviderWorkspaceItem(
	ctx context.Context, request ProviderWorkspaceItemRequest,
) error {
	var issue bool
	switch request.ItemType {
	case db.WorkspaceItemTypePullRequest:
	case db.WorkspaceItemTypeIssue:
		issue = true
	default:
		return httpapi.BadRequest(
			httpapi.CodeValidationError,
			"workspace item type must be pull_request or issue", nil,
		)
	}
	repo, err := s.lookupRepoByProviderRoute(
		ctx, request.Repository.Provider, request.Repository.PlatformHost,
		request.Repository.Owner, request.Repository.Name,
	)
	if err != nil {
		return providerRouteLookupError(err)
	}
	return s.applyWorkspaceAutoAssignment(
		ctx, *repo, request.ItemNumber, issue,
	)
}

func (s *Handler) assignPullWorkspaceUser(
	ctx context.Context,
	repo db.Repo,
	ref platform.RepoRef,
	number int,
	username string,
	registry *platform.Registry,
	mutator platform.AssigneeMutator,
) error {
	reader, err := registry.MergeRequestReader(httpapi.ProviderKind(repo), httpapi.ProviderHost(repo))
	if err != nil {
		return fmt.Errorf("resolve pull request reader: %w", err)
	}
	pull, err := reader.GetMergeRequest(ctx, ref, number)
	if err != nil {
		return fmt.Errorf("read pull request assignees: %w", err)
	}
	next, added := addUsername(pull.Assignees, username)
	assignees := next
	if added {
		assignees, err = mutator.SetMergeRequestAssignees(ctx, ref, number, next)
		if err != nil {
			return fmt.Errorf("assign pull request: %w", err)
		}
	}
	stored, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, number)
	if err != nil {
		return fmt.Errorf("load stored pull request: %w", err)
	}
	if stored == nil {
		return fmt.Errorf("stored pull request %d not found", number)
	}
	if err := s.db.UpdateMergeRequestAssignees(ctx, repo.ID, stored.ID, assignees); err != nil {
		return fmt.Errorf("save pull request assignees: %w", err)
	}
	return nil
}

func (s *Handler) assignIssueWorkspaceUser(
	ctx context.Context,
	repo db.Repo,
	ref platform.RepoRef,
	number int,
	username string,
	registry *platform.Registry,
	mutator platform.AssigneeMutator,
) error {
	reader, err := registry.IssueReader(httpapi.ProviderKind(repo), httpapi.ProviderHost(repo))
	if err != nil {
		return fmt.Errorf("resolve issue reader: %w", err)
	}
	item, err := reader.GetIssue(ctx, ref, number)
	if err != nil {
		return fmt.Errorf("read issue assignees: %w", err)
	}
	next, added := addUsername(item.Assignees, username)
	assignees := next
	if added {
		assignees, err = mutator.SetIssueAssignees(ctx, ref, number, next)
		if err != nil {
			return fmt.Errorf("assign issue: %w", err)
		}
	}
	stored, err := s.db.GetIssueByRepoIDAndNumber(ctx, repo.ID, number)
	if err != nil {
		return fmt.Errorf("load stored issue: %w", err)
	}
	if stored == nil {
		return fmt.Errorf("stored issue %d not found", number)
	}
	if err := s.db.UpdateIssueAssignees(ctx, repo.ID, stored.ID, assignees); err != nil {
		return fmt.Errorf("save issue assignees: %w", err)
	}
	return nil
}

func addUsername(usernames []string, username string) ([]string, bool) {
	for _, existing := range usernames {
		if strings.EqualFold(existing, username) {
			return usernames, false
		}
	}
	next := append([]string(nil), usernames...)
	return append(next, username), true
}
