package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/platform"
)

var ErrRepoPathRequired = errors.New("repo_path is required")
var ErrRepoNotFound = errors.New("repo not found")
var ErrRepositoryStoreUnavailable = errors.New("repository store unavailable")

type RepositoryResolver struct {
	db                   *db.DB
	providerCapabilities func(platform.Kind, string) (platform.Capabilities, error)
}

// LookupRoute resolves the provider-aware route tuple through the canonical
// repository identity path.
func (r *RepositoryResolver) LookupRoute(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (*db.Repo, error) {
	owner = strings.Trim(owner, "/ ")
	name = strings.Trim(name, "/ ")
	if owner == "" || name == "" {
		return nil, ErrRepoPathRequired
	}
	return r.Lookup(ctx, provider, platformHost, owner+"/"+name)
}

// RequireRouteCapability combines canonical route lookup with the shared
// provider-capability fallback policy.
func (r *RepositoryResolver) RequireRouteCapability(
	ctx context.Context,
	provider, platformHost, owner, name, capability string,
) (*db.Repo, error) {
	repo, err := r.LookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, ProviderRouteLookupError(err)
	}
	if !CapabilityEnabled(r.Ref(*repo).Capabilities, capability) {
		return nil, UnsupportedCapability(*repo, capability)
	}
	return repo, nil
}

func ProviderRouteLookupError(err error) error {
	if errors.Is(err, ErrRepoPathRequired) {
		return BadRequest(CodeBadRequest, err.Error(), nil)
	}
	if errors.Is(err, ErrRepoNotFound) {
		return NotFound(CodeRepoNotFound, "repo not found", nil)
	}
	if strings.Contains(err.Error(), "platform_host is required") ||
		strings.Contains(err.Error(), "unsupported platform") {
		return BadRequest(CodeBadRequest, err.Error(), nil)
	}
	return Internal("get repo failed")
}

func ProviderKind(repo db.Repo) platform.Kind {
	if strings.TrimSpace(repo.Platform) == "" {
		return platform.KindGitHub
	}
	return platform.Kind(repo.Platform)
}

func ProviderHost(repo db.Repo) string {
	if strings.TrimSpace(repo.PlatformHost) != "" {
		return repo.PlatformHost
	}
	if host, ok := platform.DefaultHost(ProviderKind(repo)); ok {
		return host
	}
	return platform.DefaultGitHubHost
}

func PlatformRepoRef(repo db.Repo) platform.RepoRef {
	repoPath := strings.TrimSpace(repo.RepoPath)
	if repoPath == "" {
		repoPath = repo.Owner + "/" + repo.Name
	}
	numericID, err := strconv.ParseInt(strings.TrimSpace(repo.PlatformRepoID), 10, 64)
	if err != nil || numericID <= 0 {
		numericID = 0
	}
	return platform.RepoRef{
		Platform:           ProviderKind(repo),
		Host:               ProviderHost(repo),
		Owner:              repo.Owner,
		Name:               repo.Name,
		RepoPath:           repoPath,
		PlatformID:         numericID,
		PlatformExternalID: repo.PlatformRepoID,
		WebURL:             repo.WebURL,
		CloneURL:           repo.CloneURL,
		DefaultBranch:      repo.DefaultBranch,
	}
}

func CapabilityEnabled(caps ProviderCapabilitiesResponse, capability string) bool {
	switch capability {
	case "comment_mutation":
		return caps.CommentMutation
	case "state_mutation":
		return caps.StateMutation
	case "merge_mutation":
		return caps.MergeMutation
	case "review_mutation":
		return caps.ReviewMutation
	case "workflow_approval":
		return caps.WorkflowApproval
	case "workflow_dispatch":
		return caps.WorkflowDispatch
	case "ready_for_review":
		return caps.ReadyForReview
	case "draft_mutation":
		return caps.DraftMutation
	case "issue_mutation":
		return caps.IssueMutation
	case "read_labels":
		return caps.ReadLabels
	case "read_markdown_images":
		return caps.ReadMarkdownImages
	case "read_workflows":
		return caps.ReadWorkflows
	case "read_workflow_runs":
		return caps.ReadWorkflowRuns
	case "label_mutation":
		return caps.LabelMutation
	case "assignee_mutation":
		return caps.AssigneeMutation
	case "reviewer_mutation":
		return caps.ReviewerMutation
	case "thread_reply":
		return caps.ThreadReply
	case "thread_resolve":
		return caps.ThreadResolve
	case "review_draft_mutation":
		return caps.ReviewDraftMutation
	case "review_thread_resolution":
		return caps.ReviewThreadResolution
	case "review_suggestion_application":
		return caps.ReviewSuggestionApplication
	case "read_review_threads":
		return caps.ReadReviewThreads
	case "mutation_head_binding":
		return caps.MutationHeadBinding
	default:
		return false
	}
}

type RepositoryResolverDeps struct {
	DB                   *db.DB
	ProviderCapabilities func(platform.Kind, string) (platform.Capabilities, error)
}

func NewRepositoryResolver(deps RepositoryResolverDeps) *RepositoryResolver {
	return &RepositoryResolver{
		db:                   deps.DB,
		providerCapabilities: deps.ProviderCapabilities,
	}
}

func (r *RepositoryResolver) Lookup(
	ctx context.Context,
	provider, platformHost, repoPath string,
) (*db.Repo, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryStoreUnavailable
	}
	provider = strings.TrimSpace(provider)
	platformHost = strings.TrimSpace(platformHost)
	repoPath = strings.Trim(repoPath, "/ ")
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return nil, err
	}
	provider = string(kind)
	if platformHost == "" {
		var ok bool
		platformHost, ok = platform.DefaultHost(kind)
		if !ok {
			return nil, fmt.Errorf("platform_host is required for provider %q", kind)
		}
	}
	if repoPath == "" {
		return nil, ErrRepoPathRequired
	}
	repo, err := r.db.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     provider,
		PlatformHost: platformHost,
		RepoPath:     repoPath,
	})
	if err != nil {
		return nil, fmt.Errorf("lookup repo: %w", err)
	}
	if repo == nil {
		return nil, ErrRepoNotFound
	}
	return repo, nil
}

func (r *RepositoryResolver) List(ctx context.Context) ([]db.Repo, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryStoreUnavailable
	}
	return r.db.ListRepos(ctx)
}

func (r *RepositoryResolver) CaptureRepositoryRouteFence(
	ctx context.Context, repo db.Repo,
) (db.RepositoryRouteFence, bool, error) {
	if r == nil || r.db == nil {
		return db.RepositoryRouteFence{}, false, ErrRepositoryStoreUnavailable
	}
	return r.db.CurrentRepositoryRouteFence(ctx, repositoryRouteIdentity(repo), repo.ID)
}

func (r *RepositoryResolver) RepositoryRouteFenceMatches(
	ctx context.Context, repo db.Repo, fence db.RepositoryRouteFence,
) (bool, error) {
	current, found, err := r.CaptureRepositoryRouteFence(ctx, repo)
	if err != nil || !found {
		return false, err
	}
	return current == fence, nil
}

// GuardRepositoryRouteFence holds repository reconciliation stable while a
// caller publishes work derived from the exact captured route generation.
func (r *RepositoryResolver) GuardRepositoryRouteFence(
	ctx context.Context,
	repo db.Repo,
	fence db.RepositoryRouteFence,
	publish func() error,
) (bool, error) {
	if r == nil || r.db == nil {
		return false, ErrRepositoryStoreUnavailable
	}
	release, err := r.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	matches, err := r.db.RepositoryRouteFenceMatchesUnderRepositoryReconciliationRead(
		ctx, repositoryRouteIdentity(repo), fence,
	)
	if err != nil || !matches {
		return false, err
	}
	if err := publish(); err != nil {
		return true, err
	}
	return true, nil
}

func repositoryRouteIdentity(repo db.Repo) db.RepoIdentity {
	return db.RepoIdentity{
		Platform:       repo.Platform,
		PlatformHost:   repo.PlatformHost,
		PlatformRepoID: repo.PlatformRepoID,
		Owner:          repo.Owner,
		Name:           repo.Name,
		RepoPath:       repo.RepoPath,
	}
}

func (r *RepositoryResolver) AdoptLegacyClonesIfSafe(
	ctx context.Context, repo db.Repo, adopt func() error,
) (bool, error) {
	if r == nil || r.db == nil {
		return false, ErrRepositoryStoreUnavailable
	}
	return r.db.AdoptLegacyClonesIfSafe(
		ctx, repositoryRouteIdentity(repo), repo.ID, adopt,
	)
}

func (r *RepositoryResolver) Ref(repo db.Repo) RepoRefResponse {
	provider := strings.TrimSpace(repo.Platform)
	if provider == "" {
		provider = string(platform.KindGitHub)
	}
	host := strings.TrimSpace(repo.PlatformHost)
	if host == "" {
		host, _ = platform.DefaultHost(platform.Kind(provider))
	}
	repoPath := strings.TrimSpace(repo.RepoPath)
	if repoPath == "" {
		repoPath = repo.Owner + "/" + repo.Name
	}
	return RepoRefResponse{
		Provider:       provider,
		PlatformHost:   host,
		PlatformRepoID: repo.PlatformRepoID,
		RepoPath:       repoPath,
		Owner:          repo.Owner,
		Name:           repo.Name,
		DefaultBranch:  repo.DefaultBranch,
		Capabilities:   r.Capabilities(platform.Kind(provider), host),
	}
}

func (r *RepositoryResolver) RefFromParts(
	provider, host, owner, name string,
) RepoRefResponse {
	return r.Ref(db.Repo{
		Platform:     provider,
		PlatformHost: host,
		Owner:        owner,
		Name:         name,
		RepoPath:     owner + "/" + name,
	})
}

func (r *RepositoryResolver) CapabilitiesForRepo(repo db.Repo) ProviderCapabilitiesResponse {
	return r.Capabilities(ProviderKind(repo), ProviderHost(repo))
}

// Capabilities returns only facts reported by the live provider registry. A
// missing registry must not advertise operations the daemon cannot execute.
func (r *RepositoryResolver) Capabilities(kind platform.Kind, host string) ProviderCapabilitiesResponse {
	if r != nil && r.providerCapabilities != nil {
		caps, err := r.providerCapabilities(kind, host)
		if err == nil {
			return ProviderCapabilitiesFromPlatform(caps)
		}
	}
	return ProviderCapabilitiesResponse{}
}

func ProviderCapabilitiesFromPlatform(caps platform.Capabilities) ProviderCapabilitiesResponse {
	reviewActions := make([]string, 0, len(caps.SupportedReviewActions))
	for _, action := range caps.SupportedReviewActions {
		reviewActions = append(reviewActions, string(action))
	}
	return ProviderCapabilitiesResponse{
		ReadRepositories:            caps.ReadRepositories,
		ReadMergeRequests:           caps.ReadMergeRequests,
		ReadIssues:                  caps.ReadIssues,
		ReadIssuePRReferences:       caps.ReadIssuePRReferences,
		ReadComments:                caps.ReadComments,
		ReadReleases:                caps.ReadReleases,
		ReadCI:                      caps.ReadCI,
		ReadWorkflows:               caps.ReadWorkflows,
		ReadWorkflowRuns:            caps.ReadWorkflowRuns,
		ReadLabels:                  caps.ReadLabels,
		ReadMarkdownImages:          caps.ReadMarkdownImages,
		ReadAuthenticatedUser:       caps.ReadAuthenticatedUser,
		CommentMutation:             caps.CommentMutation,
		StateMutation:               caps.StateMutation,
		MergeMutation:               caps.MergeMutation,
		ReviewMutation:              caps.ReviewMutation,
		WorkflowApproval:            caps.WorkflowApproval,
		WorkflowDispatch:            caps.WorkflowDispatch,
		ReadyForReview:              caps.ReadyForReview,
		DraftMutation:               caps.DraftMutation,
		IssueMutation:               caps.IssueMutation,
		LabelMutation:               caps.LabelMutation,
		AssigneeMutation:            caps.AssigneeMutation,
		ReviewerMutation:            caps.ReviewerMutation,
		ThreadReply:                 caps.ThreadReply,
		ThreadResolve:               caps.ThreadResolve,
		ReviewDraftMutation:         caps.ReviewDraftMutation,
		ReviewThreadResolution:      caps.ReviewThreadResolution,
		ReviewSuggestionApplication: caps.ReviewSuggestionApplication,
		ReadReviewThreads:           caps.ReadReviewThreads,
		NativeMultilineRanges:       caps.NativeMultilineRanges,
		MutationHeadBinding:         caps.MutationHeadBinding,
		SupportedReviewActions:      reviewActions,
	}
}
