package gitealike

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/forge/platform"
)

type Provider struct {
	kind      platform.Kind
	host      string
	transport Transport
	options   options
}

type options struct {
	ReadActions bool
	Mutations   bool
}

type Option func(*options)

func WithReadActions() Option {
	return func(options *options) {
		options.ReadActions = true
	}
}

func WithMutations() Option {
	return func(options *options) {
		options.Mutations = true
	}
}

func NewProvider(
	kind platform.Kind,
	host string,
	transport Transport,
	opts ...Option,
) *Provider {
	var options options
	for _, opt := range opts {
		opt(&options)
	}
	return &Provider{
		kind:      kind,
		host:      host,
		transport: transport,
		options:   options,
	}
}

func (p *Provider) Platform() platform.Kind {
	return p.kind
}

func (p *Provider) Host() string {
	return p.host
}

func (p *Provider) Capabilities() platform.Capabilities {
	caps := platform.Capabilities{
		ReadRepositories:      true,
		ReadMergeRequests:     true,
		ReadIssues:            true,
		ReadIssuePRReferences: false,
		ReadComments:          true,
		ReadReleases:          true,
		ReadCI:                true,
	}
	_, caps.ReadIssuePRReferences = p.transport.(TimelineTransport)
	if _, ok := p.transport.(ArchiveTransport); ok {
		caps.Archive = platform.ArchiveCapabilities{
			HistoricalIssues: true, HistoricalMergeRequests: true,
			OrdinaryComments: true, SubmittedReviews: true,
		}
	}
	_, hasLabels := p.transport.(LabelTransport)
	caps.ReadLabels = hasLabels
	_, caps.ReadAuthenticatedUser = p.transport.(AuthenticatedUserTransport)
	if p.options.Mutations {
		caps.CommentMutation = true
		caps.StateMutation = true
		caps.MergeMutation = true
		caps.IssueMutation = true
		caps.MutationHeadBinding = true
		caps.LabelMutation = hasLabels
		caps.AssigneeMutation = true
		if _, ok := p.transport.(ReviewMutationTransport); ok {
			caps.ReviewMutation = true
			caps.SupportedReviewActions = []platform.ReviewAction{
				platform.ReviewActionComment,
				platform.ReviewActionApprove,
				platform.ReviewActionRequestChanges,
			}
		}
		if _, ok := p.transport.(ReviewRequestTransport); ok {
			caps.ReviewerMutation = true
		}
	}
	return caps
}

func (p *Provider) AuthenticatedUser(
	ctx context.Context,
	_ platform.RepoRef,
) (string, error) {
	transport, ok := p.transport.(AuthenticatedUserTransport)
	if !ok {
		return "", platform.UnsupportedCapability(p.kind, p.host, "read_authenticated_user")
	}
	user, err := transport.GetAuthenticatedUser(ctx)
	if err != nil {
		return "", err
	}
	username := strings.TrimSpace(user.UserName)
	if username == "" {
		return "", fmt.Errorf("authenticated %s username is empty", p.kind)
	}
	return username, nil
}

func (p *Provider) GetRepository(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	repo, err := p.transport.GetRepository(ctx, ref.Owner, ref.Name)
	if err != nil {
		return platform.Repository{}, p.mapError(err)
	}
	return NormalizeRepository(p.kind, p.host, repo)
}

func (p *Provider) ListRepositories(
	ctx context.Context,
	owner string,
	opts platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos, err := p.listRepositories(ctx, owner, p.transport.ListUserRepositories)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			repos, err = p.listRepositories(ctx, owner, p.transport.ListOrgRepositories)
		}
	}
	if err != nil {
		return nil, err
	}
	if opts.Limit <= 0 && opts.Offset <= 0 {
		return repos, nil
	}
	return applyRepositoryListOptions(repos, opts), nil
}

func (p *Provider) ListOpenMergeRequests(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]PullRequestDTO, Page, error) {
		return p.transport.ListOpenPullRequests(ctx, ref, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	out := make([]platform.MergeRequest, 0, len(items))
	for _, item := range items {
		out = append(out, NormalizePullRequest(ref, item))
	}
	return out, nil
}

func (p *Provider) GetMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	pr, err := p.transport.GetPullRequest(ctx, ref, number)
	if err != nil {
		return platform.MergeRequest{}, p.repositoryItemLookupError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	return NormalizePullRequest(ref, pr), nil
}

func (p *Provider) ListMergeRequestEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	comments, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]CommentDTO, Page, error) {
		return p.transport.ListPullRequestComments(ctx, ref, number, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	reviews, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]ReviewDTO, Page, error) {
		return p.transport.ListPullRequestReviews(ctx, ref, number, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	commits, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]CommitDTO, Page, error) {
		return p.transport.ListPullRequestCommits(ctx, ref, number, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	events := NormalizeMergeRequestEvents(p.kind, ref, number, comments, reviews, commits)
	timeline, err := p.listTimelineEvents(
		ctx, ref, number, platform.RepositoryFeatureMergeRequests,
	)
	if err != nil {
		return nil, err
	}
	events = append(events, NormalizeMergeRequestTimelineEvents(p.kind, ref, number, timeline)...)
	return events, nil
}

func (p *Provider) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]IssueDTO, Page, error) {
		return p.transport.ListOpenIssues(ctx, ref, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureIssues, err,
		)
	}
	out := make([]platform.Issue, 0, len(items))
	for _, item := range items {
		if !item.IsPullRequest {
			out = append(out, NormalizeIssue(ref, item))
		}
	}
	return out, nil
}

func (p *Provider) GetIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.Issue, error) {
	issue, err := p.transport.GetIssue(ctx, ref, number)
	if err != nil {
		return platform.Issue{}, p.repositoryItemLookupError(
			ctx, ref, platform.RepositoryFeatureIssues, err,
		)
	}
	return NormalizeIssue(ref, issue), nil
}

func (p *Provider) ListIssueEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	comments, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]CommentDTO, Page, error) {
		return p.transport.ListIssueComments(ctx, ref, number, opts)
	})
	if err != nil {
		return nil, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureIssues, err,
		)
	}
	events := NormalizeIssueComments(p.kind, ref, number, comments)
	timeline, err := p.listTimelineEvents(
		ctx, ref, number, platform.RepositoryFeatureIssues,
	)
	if err != nil {
		return nil, err
	}
	events = append(events, NormalizeIssueTimelineEvents(p.kind, ref, number, timeline)...)
	return events, nil
}

func (p *Provider) listTimelineEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	feature string,
) ([]TimelineEventDTO, error) {
	timelineTransport, ok := p.transport.(TimelineTransport)
	if !ok {
		return nil, nil
	}
	timeline, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]TimelineEventDTO, Page, error) {
		return timelineTransport.ListIssueTimeline(ctx, ref, number, opts)
	})
	if err != nil {
		err = p.repositoryFeatureError(ctx, ref, feature, err)
		if errors.Is(err, platform.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return timeline, nil
}

func (p *Provider) ListReleases(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.Release, error) {
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]ReleaseDTO, Page, error) {
		return p.transport.ListReleases(ctx, ref, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	out := make([]platform.Release, 0, len(items))
	for _, item := range items {
		out = append(out, NormalizeRelease(ref, item))
	}
	return out, nil
}

func (p *Provider) ListTags(ctx context.Context, ref platform.RepoRef) ([]platform.Tag, error) {
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]TagDTO, Page, error) {
		return p.transport.ListTags(ctx, ref, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	out := make([]platform.Tag, 0, len(items))
	for _, item := range items {
		out = append(out, NormalizeTag(ref, item))
	}
	return out, nil
}

func (p *Provider) ListCIChecks(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
) ([]platform.CICheck, error) {
	statuses, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]StatusDTO, Page, error) {
		return p.transport.ListStatuses(ctx, ref, sha, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	var actionRuns []ActionRunDTO
	if p.options.ReadActions {
		if actionsTransport, ok := p.transport.(ActionsTransport); ok {
			actionRuns, err = collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]ActionRunDTO, Page, error) {
				return actionsTransport.ListActionRuns(ctx, ref, sha, opts)
			})
			if err != nil {
				return nil, p.mapError(err)
			}
		}
	}
	return NormalizeStatuses(ref, statuses, actionRuns), nil
}

func (p *Provider) ListLabels(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.LabelCatalog, error) {
	transport, ok := p.transport.(LabelTransport)
	if !ok {
		return platform.LabelCatalog{}, platform.UnsupportedCapability(p.kind, p.host, "read_labels")
	}
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]LabelDTO, Page, error) {
		return transport.ListRepoLabels(ctx, ref, opts)
	})
	if err != nil {
		return platform.LabelCatalog{}, p.mapError(err)
	}
	return platform.LabelCatalog{Labels: NormalizeLabels(ref, items)}, nil
}

func (p *Provider) SetMergeRequestLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	return p.setIssueLikeLabels(ctx, ref, number, names)
}

func (p *Provider) SetIssueLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	return p.setIssueLikeLabels(ctx, ref, number, names)
}

// setIssueLikeLabels assigns labels by name on a pull request or issue.
// Forgejo and Gitea assign labels by numeric ID and use the issues
// endpoint for both, so names are resolved against the repository label
// catalog first.
func (p *Provider) setIssueLikeLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	transport, err := p.labelMutationTransport()
	if err != nil {
		return nil, err
	}
	catalog, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]LabelDTO, Page, error) {
		return transport.ListRepoLabels(ctx, ref, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	idsByName := make(map[string]int64, len(catalog))
	for _, label := range catalog {
		idsByName[label.Name] = label.ID
	}
	labelIDs := make([]int64, 0, len(names))
	for _, name := range names {
		id, ok := idsByName[name]
		if !ok {
			return nil, &platform.Error{
				Code:         platform.ErrCodeNotFound,
				Provider:     p.kind,
				PlatformHost: p.host,
				Capability:   "label_mutation",
				Err:          fmt.Errorf("label %q not found in repository %s/%s", name, ref.Owner, ref.Name),
			}
		}
		labelIDs = append(labelIDs, id)
	}
	labels, err := transport.ReplaceIssueLabels(ctx, ref, number, labelIDs)
	if err != nil {
		return nil, p.mapError(err)
	}
	return NormalizeLabels(ref, labels), nil
}

func (p *Provider) labelMutationTransport() (LabelTransport, error) {
	if !p.options.Mutations {
		return nil, platform.UnsupportedCapability(p.kind, p.host, "label_mutation")
	}
	transport, ok := p.transport.(LabelTransport)
	if !ok {
		return nil, platform.UnsupportedCapability(p.kind, p.host, "label_mutation")
	}
	return transport, nil
}

func (p *Provider) CreateMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.MergeRequestEvent, error) {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	comment, err := transport.CreateIssueComment(ctx, ref, number, body)
	if err != nil {
		return platform.MergeRequestEvent{}, p.mapError(err)
	}
	return NormalizeMergeRequestEvents(p.kind, ref, number, []CommentDTO{comment}, nil, nil)[0], nil
}

func (p *Provider) EditMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.MergeRequestEvent, error) {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	comment, err := transport.EditIssueComment(ctx, ref, commentID, body)
	if err != nil {
		return platform.MergeRequestEvent{}, p.mapError(err)
	}
	return NormalizeMergeRequestEvents(p.kind, ref, number, []CommentDTO{comment}, nil, nil)[0], nil
}

func (p *Provider) DeleteMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return err
	}
	if err := transport.DeleteIssueComment(ctx, ref, commentID); err != nil {
		return p.mapError(err)
	}
	return nil
}

func (p *Provider) CreateIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.IssueEvent, error) {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return platform.IssueEvent{}, err
	}
	comment, err := transport.CreateIssueComment(ctx, ref, number, body)
	if err != nil {
		return platform.IssueEvent{}, p.mapError(err)
	}
	return NormalizeIssueComments(p.kind, ref, number, []CommentDTO{comment})[0], nil
}

func (p *Provider) EditIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.IssueEvent, error) {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return platform.IssueEvent{}, err
	}
	comment, err := transport.EditIssueComment(ctx, ref, commentID, body)
	if err != nil {
		return platform.IssueEvent{}, p.mapError(err)
	}
	return NormalizeIssueComments(p.kind, ref, number, []CommentDTO{comment})[0], nil
}

func (p *Provider) DeleteIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	transport, err := p.mutationTransport("comment_mutation")
	if err != nil {
		return err
	}
	if err := transport.DeleteIssueComment(ctx, ref, commentID); err != nil {
		return p.mapError(err)
	}
	return nil
}

func (p *Provider) CreateIssue(
	ctx context.Context,
	ref platform.RepoRef,
	title string,
	body string,
) (platform.Issue, error) {
	transport, err := p.mutationTransport("issue_mutation")
	if err != nil {
		return platform.Issue{}, err
	}
	issue, err := transport.CreateIssue(ctx, ref, title, body)
	if err != nil {
		return platform.Issue{}, p.mapError(err)
	}
	return NormalizeIssue(ref, issue), nil
}

func (p *Provider) SetMergeRequestState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.MergeRequest, error) {
	transport, err := p.mutationTransport("state_mutation")
	if err != nil {
		return platform.MergeRequest{}, err
	}
	pr, err := transport.EditPullRequest(ctx, ref, number, PullRequestMutationOptions{State: &state})
	if err != nil {
		return platform.MergeRequest{}, p.mapError(err)
	}
	return NormalizePullRequest(ref, pr), nil
}

func (p *Provider) SetIssueState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.Issue, error) {
	transport, err := p.mutationTransport("state_mutation")
	if err != nil {
		return platform.Issue{}, err
	}
	issue, err := transport.EditIssue(ctx, ref, number, IssueMutationOptions{State: &state})
	if err != nil {
		return platform.Issue{}, p.mapError(err)
	}
	return NormalizeIssue(ref, issue), nil
}

// MergeMergeRequest sends expectedHeadSHA as the Gitea/Forgejo merge
// head_commit_id: the provider rejects the merge when the PR head moved
// past the reviewed commit, and that rejection is classified as
// stale_state.
func (p *Provider) MergeMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commitTitle string,
	commitMessage string,
	method string,
	expectedHeadSHA string,
) (platform.MergeResult, error) {
	transport, err := p.mutationTransport("merge_mutation")
	if err != nil {
		return platform.MergeResult{}, err
	}
	result, err := transport.MergePullRequest(ctx, ref, number, MergeOptions{
		CommitTitle:     commitTitle,
		CommitMessage:   commitMessage,
		Method:          method,
		ExpectedHeadSHA: expectedHeadSHA,
	})
	if err != nil {
		if expectedHeadSHA != "" && p.mergeRejectedForStaleHead(
			ctx, ref, number, expectedHeadSHA, err,
		) {
			return platform.MergeResult{}, &platform.Error{
				Code:         platform.ErrCodeStaleState,
				Provider:     p.kind,
				PlatformHost: p.host,
				Capability:   "merge_merge_request",
				Err:          err,
			}
		}
		return platform.MergeResult{}, p.mapError(err)
	}
	return platform.MergeResult{Merged: result.Merged, SHA: result.SHA, Message: result.Message}, nil
}

func (p *Provider) mergeRejectedForStaleHead(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	expectedHeadSHA string,
	err error,
) bool {
	if isHeadMismatchConflict(err) {
		return true
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr == nil ||
		(httpErr.StatusCode != http.StatusConflict && httpErr.StatusCode != http.StatusMethodNotAllowed) {
		return false
	}
	current, readErr := p.transport.GetPullRequest(ctx, ref, number)
	if readErr != nil || strings.TrimSpace(current.Head.SHA) == "" {
		return false
	}
	return current.Head.SHA != expectedHeadSHA
}

func (p *Provider) ApproveMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) (platform.MergeRequestEvent, error) {
	transport, ok := p.transport.(ReviewMutationTransport)
	if !ok || !p.options.Mutations {
		return platform.MergeRequestEvent{},
			platform.UnsupportedCapability(p.kind, p.host, "approve_merge_request")
	}
	review, err := transport.CreatePullReview(ctx, ref, number, ReviewOptions{
		State:    "APPROVED",
		Body:     body,
		CommitID: expectedHeadSHA,
	})
	if err != nil {
		return platform.MergeRequestEvent{}, p.mapError(err)
	}
	events := NormalizeMergeRequestEvents(p.kind, ref, number, nil, []ReviewDTO{review}, nil)
	if len(events) == 0 {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no review event")
	}
	return events[0], nil
}

func (p *Provider) RequestChanges(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) error {
	transport, ok := p.transport.(ReviewMutationTransport)
	if !ok || !p.options.Mutations {
		return platform.UnsupportedCapability(p.kind, p.host, "review_action_request_changes")
	}
	_, err := transport.CreatePullReview(ctx, ref, number, ReviewOptions{
		State:    "REQUEST_CHANGES",
		Body:     body,
		CommitID: expectedHeadSHA,
	})
	if err != nil {
		return p.mapError(err)
	}
	return nil
}

func (p *Provider) EditMergeRequestContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.MergeRequest, error) {
	transport, err := p.mutationTransport("state_mutation")
	if err != nil {
		return platform.MergeRequest{}, err
	}
	pr, err := transport.EditPullRequest(ctx, ref, number, PullRequestMutationOptions{Title: title, Body: body})
	if err != nil {
		return platform.MergeRequest{}, p.mapError(err)
	}
	return NormalizePullRequest(ref, pr), nil
}

func (p *Provider) EditIssueContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.Issue, error) {
	transport, err := p.mutationTransport("state_mutation")
	if err != nil {
		return platform.Issue{}, err
	}
	issue, err := transport.EditIssue(ctx, ref, number, IssueMutationOptions{Title: title, Body: body})
	if err != nil {
		return platform.Issue{}, p.mapError(err)
	}
	return NormalizeIssue(ref, issue), nil
}

func (p *Provider) SetMergeRequestAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	transport, err := p.mutationTransport("assignee_mutation")
	if err != nil {
		return nil, err
	}
	if usernames == nil {
		usernames = []string{}
	}
	pr, err := transport.EditPullRequest(ctx, ref, number, PullRequestMutationOptions{Assignees: &usernames})
	if err != nil {
		return nil, p.mapError(err)
	}
	if pr.Assignees == nil {
		// The SDK response omitted the field; the request set is the
		// best available truth after a successful edit.
		return usernames, nil
	}
	return userDTONames(pr.Assignees), nil
}

func (p *Provider) SetIssueAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	transport, err := p.mutationTransport("assignee_mutation")
	if err != nil {
		return nil, err
	}
	if usernames == nil {
		usernames = []string{}
	}
	issue, err := transport.EditIssue(ctx, ref, number, IssueMutationOptions{Assignees: &usernames})
	if err != nil {
		return nil, p.mapError(err)
	}
	assignees := make([]string, 0, len(issue.Assignees))
	for _, user := range issue.Assignees {
		if user.UserName != "" {
			assignees = append(assignees, user.UserName)
		}
	}
	return assignees, nil
}

func (p *Provider) RequestMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	transport, err := p.reviewRequestTransport()
	if err != nil {
		return nil, err
	}
	if len(usernames) == 0 {
		// An empty request is the interface's read primitive: report
		// the provider's current requested-reviewer set untouched.
		return p.currentRequestedReviewers(ctx, ref, number)
	}
	if err := transport.CreateReviewRequests(ctx, ref, number, usernames); err != nil {
		return nil, p.mapError(err)
	}
	return p.currentRequestedReviewers(ctx, ref, number)
}

func (p *Provider) RemoveMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	transport, err := p.reviewRequestTransport()
	if err != nil {
		return nil, err
	}
	if err := transport.DeleteReviewRequests(ctx, ref, number, usernames); err != nil {
		return nil, p.mapError(err)
	}
	return p.currentRequestedReviewers(ctx, ref, number)
}

// currentRequestedReviewers reads the requested-reviewer set back after
// a review-request mutation. The Gitea SDK carries the field on the pull
// request itself; the Forgejo SDK does not, so pending requests are
// derived from review rows in the REQUEST_REVIEW state.
func (p *Provider) currentRequestedReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]string, error) {
	pr, err := p.transport.GetPullRequest(ctx, ref, number)
	if err != nil {
		return nil, p.mapError(err)
	}
	if pr.RequestedReviewers != nil {
		return userDTONames(pr.RequestedReviewers), nil
	}
	reviews, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]ReviewDTO, Page, error) {
		return p.transport.ListPullRequestReviews(ctx, ref, number, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	seen := make(map[string]bool)
	requested := make([]string, 0, len(reviews))
	for _, review := range reviews {
		if review.State != "REQUEST_REVIEW" || review.User.UserName == "" {
			continue
		}
		if seen[review.User.UserName] {
			continue
		}
		seen[review.User.UserName] = true
		requested = append(requested, review.User.UserName)
	}
	return requested, nil
}

func (p *Provider) reviewRequestTransport() (ReviewRequestTransport, error) {
	if !p.options.Mutations {
		return nil, platform.UnsupportedCapability(p.kind, p.host, "reviewer_mutation")
	}
	transport, ok := p.transport.(ReviewRequestTransport)
	if !ok {
		return nil, platform.UnsupportedCapability(p.kind, p.host, "reviewer_mutation")
	}
	return transport, nil
}

func (p *Provider) listRepositories(
	ctx context.Context,
	owner string,
	list func(context.Context, string, PageOptions) ([]RepositoryDTO, Page, error),
) ([]platform.Repository, error) {
	items, err := collectTransportPages(ctx, func(ctx context.Context, opts PageOptions) ([]RepositoryDTO, Page, error) {
		return list(ctx, owner, opts)
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	out := make([]platform.Repository, 0, len(items))
	for _, item := range items {
		repo, err := NormalizeRepository(p.kind, p.host, item)
		if err != nil {
			return nil, err
		}
		if repo.Ref.Owner == owner {
			out = append(out, repo)
		}
	}
	return out, nil
}

func (p *Provider) mutationTransport(capability string) (MutationTransport, error) {
	if !p.options.Mutations {
		return nil, platform.UnsupportedCapability(p.kind, p.host, capability)
	}
	transport, ok := p.transport.(MutationTransport)
	if !ok {
		return nil, platform.UnsupportedCapability(p.kind, p.host, capability)
	}
	return transport, nil
}

// headMismatchPhrases are the messages the Gitea and Forgejo merge
// endpoints return when head_commit_id no longer matches the PR head
// (the IsErrSHADoesNotMatch branch in routers/api/v1/repo/pull.go).
// Current Gitea and Forgejo lines say "head out of date"; older Gitea
// releases said "head target does not match". Validated live against
// the container fixtures.
var headMismatchPhrases = []string{
	"head out of date",
	"head target does not match",
}

// isHeadMismatchConflict reports whether a transport error is the
// Gitea/Forgejo 409 returned when head_commit_id no longer matches the
// PR head. The merge endpoints also answer 409 for ordinary merge
// conflicts and out-of-date pushes, so the status alone is not enough:
// only the head-mismatch messages classify as stale.
func isHeadMismatchConflict(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr == nil || httpErr.StatusCode != 409 {
		return false
	}
	text := strings.ToLower(httpErr.Error())
	for _, phrase := range headMismatchPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func (p *Provider) mapError(err error) error {
	return p.MapError(err)
}

// MapError attaches provider identity and stable platform classification to a
// transport error returned by a concrete provider extension.
func (p *Provider) MapError(err error) error {
	return mapTransportError(p.kind, p.host, err)
}

func applyRepositoryListOptions(
	repos []platform.Repository,
	opts platform.RepositoryListOptions,
) []platform.Repository {
	start := max(opts.Offset, 0)
	if start >= len(repos) {
		return nil
	}
	end := len(repos)
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	return repos[start:end]
}
