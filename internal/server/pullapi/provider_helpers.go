package pullapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

const (
	capabilityCommentMutation             = "comment_mutation"
	capabilityStateMutation               = "state_mutation"
	capabilityMergeMutation               = "merge_mutation"
	capabilityReviewMutation              = "review_mutation"
	capabilityWorkflowApproval            = "workflow_approval"
	capabilityReadyForReview              = "ready_for_review"
	capabilityDraftMutation               = "draft_mutation"
	capabilityReadLabels                  = "read_labels"
	capabilityLabelMutation               = "label_mutation"
	capabilityAssigneeMutation            = "assignee_mutation"
	capabilityReviewerMutation            = "reviewer_mutation"
	capabilityThreadReply                 = "thread_reply"
	capabilityThreadResolve               = "thread_resolve"
	capabilityReviewDraftMutation         = "review_draft_mutation"
	capabilityReviewThreadResolution      = "review_thread_resolution"
	capabilityReviewSuggestionApplication = "review_suggestion_application"
	capabilityReadReviewThreads           = "read_review_threads"
	capabilityMutationHeadBinding         = "mutation_head_binding"
)

func providerRouteLookupError(err error) error {
	return httpapi.ProviderRouteLookupError(err)
}

func (s *Handler) lookupRepoByProviderRoute(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (*db.Repo, error) {
	return s.resolver.LookupRoute(ctx, provider, platformHost, owner, name)
}

func (s *Handler) requireRepoRouteCapability(
	ctx context.Context,
	provider, platformHost, owner, name, capability string,
) (*db.Repo, error) {
	return s.resolver.RequireRouteCapability(
		ctx, provider, platformHost, owner, name, capability,
	)
}

func capabilityEnabled(
	caps httpapi.ProviderCapabilitiesResponse,
	capability string,
) bool {
	return httpapi.CapabilityEnabled(caps, capability)
}

func (s *Handler) capabilitiesForRepo(repo db.Repo) httpapi.ProviderCapabilitiesResponse {
	return s.resolver.Ref(repo).Capabilities
}

func (s *Handler) requireSyncerCapability(repo db.Repo, capability string) error {
	if s.syncer == nil {
		return httpapi.UnsupportedCapability(repo, capability)
	}
	return nil
}

func unsupportedCapabilityProblem(repo db.Repo, capability string) huma.StatusError {
	return httpapi.UnsupportedCapability(repo, capability)
}

func repoProviderKind(repo db.Repo) platform.Kind {
	return httpapi.ProviderKind(repo)
}

func repoProviderHost(repo db.Repo) string {
	return httpapi.ProviderHost(repo)
}

func platformRepoRefFromDB(repo db.Repo) platform.RepoRef {
	return httpapi.PlatformRepoRef(repo)
}

func (s *Handler) repoRefFromRepo(repo db.Repo) httpapi.RepoRefResponse {
	return s.resolver.Ref(repo)
}

func (s *Handler) repoRefWithMergeRequestOperations(
	ctx context.Context,
	repo db.Repo,
	mr db.MergeRequest,
) httpapi.RepoRefResponse {
	ref := s.resolver.Ref(repo)
	if s.repoOperationsForMergeRequest != nil {
		operations := s.repoOperationsForMergeRequest(ctx, repo, mr)
		ref.Operations = &operations
	}
	return ref
}

func (s *Handler) operations(repo db.Repo) httpapi.RepoOperations {
	if s.repoOperations == nil {
		return httpapi.RepoOperations{}
	}
	return s.repoOperations(repo)
}

func (s *Handler) mergeRequestAuthoredByViewer(
	ctx context.Context,
	repo db.Repo,
	mr db.MergeRequest,
) bool {
	if s.syncer == nil || s.syncer.Registry() == nil {
		return false
	}
	resolver, err := s.syncer.Registry().MergeRequestViewerResolver(
		repoProviderKind(repo), repoProviderHost(repo),
	)
	if err != nil {
		return false
	}
	authored, err := resolver.ViewerAuthoredMergeRequest(ctx, platform.MergeRequest{
		Repo: platformRepoRefFromDB(repo), Number: mr.Number, Author: mr.Author,
	})
	return err == nil && authored
}

func selfApprovalProblem(repo db.Repo) huma.StatusError {
	return httpapi.Forbidden(
		"You cannot approve your own pull request",
		map[string]any{
			"reason":       "self_approval",
			"provider":     string(repoProviderKind(repo)),
			"platformHost": repoProviderHost(repo),
		},
	)
}

func operationRateLimitedProblem(
	repo db.Repo,
	availability httpapi.OperationAvailability,
) huma.StatusError {
	detail := availability.UnavailableReason
	if detail == "" {
		detail = "Upstream rate limit exceeded"
	}
	details := map[string]any{
		"reason":       "rate_limited",
		"provider":     string(repoProviderKind(repo)),
		"platformHost": repoProviderHost(repo),
	}
	if availability.RetryAt != "" {
		details["retryAfter"] = availability.RetryAt
	}
	return httpapi.NewProblem(
		http.StatusTooManyRequests,
		httpapi.CodeRateLimited,
		detail,
		details,
	)
}
