package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/platform"
)

// Client is the interface for interacting with the GitHub API.
type API interface {
	ListOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error)
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error)
	GetUser(ctx context.Context, login string) (*gh.User, error)
	ListRepositoriesByOwner(ctx context.Context, owner string) ([]*gh.Repository, error)
	ListReleases(ctx context.Context, owner, repo string, perPage int) ([]*gh.RepositoryRelease, error)
	ListTags(ctx context.Context, owner, repo string, perPage int) ([]*gh.RepositoryTag, error)
	ListOpenIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, error)
	GetIssue(ctx context.Context, owner, repo string, number int) (*gh.Issue, error)
	CreateIssue(ctx context.Context, owner, repo, title, body string) (*gh.Issue, error)
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error)
	ListIssueCommentsIfChanged(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error)
	ListReviews(ctx context.Context, owner, repo string, number int) ([]*gh.PullRequestReview, error)
	ListPullRequestReviewThreads(ctx context.Context, owner, repo string, number int) ([]PullRequestReviewThread, error)
	ListCommits(ctx context.Context, owner, repo string, number int) ([]*gh.RepositoryCommit, error)
	ListPullRequestTimelineEvents(ctx context.Context, owner, repo string, number int) ([]PullRequestTimelineEvent, error)
	ListForcePushEvents(ctx context.Context, owner, repo string, number int) ([]ForcePushEvent, error)
	GetCombinedStatus(ctx context.Context, owner, repo, ref string) (*gh.CombinedStatus, error)
	ListCheckRunsForRef(ctx context.Context, owner, repo, ref string) ([]*gh.CheckRun, error)
	ListWorkflowRunsForHeadSHA(ctx context.Context, owner, repo, headSHA string) ([]*gh.WorkflowRun, error)
	ApproveWorkflowRun(ctx context.Context, owner, repo string, runID int64) error
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*gh.IssueComment, error)
	EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) (*gh.IssueComment, error)
	DeleteIssueComment(ctx context.Context, owner, repo string, commentID int64) error
	CreatePullRequestReviewCommentReply(ctx context.Context, owner, repo string, number int, body string, commentID int64) (*gh.PullRequestComment, error)
	GetRepository(ctx context.Context, owner, repo string) (*gh.Repository, error)
	CreateReview(ctx context.Context, owner, repo string, number int, event string, body string) (*gh.PullRequestReview, error)
	CreateReviewWithComments(
		ctx context.Context,
		owner, repo string,
		number int,
		event string,
		body string,
		commitID string,
		comments []*gh.DraftReviewComment,
	) (*gh.PullRequestReview, error)
	ApplyReviewSuggestions(
		ctx context.Context,
		owner, repo string,
		number int,
		input platform.ApplyReviewSuggestionsInput,
	) (*platform.AppliedReviewSuggestions, error)
	// DismissReview revokes a submitted review. Approvals are not
	// head-gated by GitHub, so a head that moves while an approval
	// submits is backed out through dismissal.
	DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) (*gh.PullRequestReview, error)
	MarkPullRequestReadyForReview(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error)
	ConvertPullRequestToDraft(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error)
	MergePullRequest(ctx context.Context, owner, repo string, number int, commitTitle, commitMessage, method, expectedHeadSHA string) (*gh.PullRequestMergeResult, error)
	EditPullRequest(ctx context.Context, owner, repo string, number int, opts EditPullRequestOpts) (*gh.PullRequest, error)
	EditIssue(ctx context.Context, owner, repo string, number int, state string) (*gh.Issue, error)
	EditIssueContent(ctx context.Context, owner, repo string, number int, title *string, body *string) (*gh.Issue, error)
	ListPullRequestsPage(ctx context.Context, owner, repo, state string, page int) ([]*gh.PullRequest, bool, error)
	ListIssuesPage(ctx context.Context, owner, repo, state string, page int) ([]*gh.Issue, bool, error)
	ListNotifications(ctx context.Context, opts platform.NotificationListOptions) ([]platform.NotificationThread, bool, error)
	MarkNotificationThreadRead(ctx context.Context, threadID string) error
	// InvalidateListETagsForRepo drops cached conditional-GET
	// validators for the given repo's list endpoints so the next
	// list call issues an unconditional fetch. The endpoints
	// parameter selects which caches to clear ("pulls", "issues",
	// "comments"); passing no endpoints clears every supported
	// repo-scoped list path. Used to recover from a partial-failure
	// sync.
	InvalidateListETagsForRepo(owner, repo string, endpoints ...string)
}

// markdownImageClient carries the full repository identity even though the
// attachment URL is host-scoped: credential selection is per repository, so a
// repo-scoped route must be able to pick its own token for the fetch.
type markdownImageClient interface {
	GetMarkdownImage(
		ctx context.Context, owner, repo, sourceURL string,
	) (platform.MarkdownImage, error)
}

type Provider struct {
	host           string
	client         API
	now            func() time.Time
	viewerCacheTTL time.Duration
	warning        func(string, ...any)
	viewerMu       sync.Mutex
	viewerLogins   map[string]authenticatedViewerLoginCacheEntry
}

type authenticatedViewerLoginCacheEntry struct {
	login     string
	fetchedAt time.Time
}

type ViewerAPI interface {
	AuthenticatedViewerLogin(ctx context.Context) (string, error)
}

type ViewerCacheKeyAPI interface {
	AuthenticatedViewerCacheKey() string
}

type LabelAPI interface {
	ListRepoLabels(ctx context.Context, owner, repo string) ([]*gh.Label, error)
	ReplaceIssueLabels(ctx context.Context, owner, repo string, number int, names []string) ([]*gh.Label, error)
}

type AssigneeAPI interface {
	ReplaceIssueAssignees(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.Issue, error)
}

type ReviewerAPI interface {
	RequestPullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.PullRequest, error)
	RemovePullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) error
}

func (p *Provider) Platform() platform.Kind {
	return platform.KindGitHub
}

func (p *Provider) Host() string {
	return p.host
}

func (p *Provider) Capabilities() platform.Capabilities {
	_, labels := p.client.(LabelAPI)
	_, assignees := p.client.(AssigneeAPI)
	_, reviewers := p.client.(ReviewerAPI)
	_, archivePages := p.client.(InventoryAPI)
	_, markdownImages := p.client.(markdownImageClient)
	_, directViewer := p.client.(ViewerAPI)
	_, routedViewer := p.client.(interface {
		AuthenticatedViewerLoginForRepo(context.Context, string, string) (string, error)
	})
	return platform.Capabilities{
		ReadRepositories:            true,
		ReadMergeRequests:           true,
		ReadIssues:                  true,
		ReadIssuePRReferences:       true,
		ReadComments:                true,
		ReadReleases:                true,
		ReadCI:                      true,
		ReadLabels:                  labels,
		ReadMarkdownImages:          markdownImages,
		ReadAuthenticatedUser:       directViewer || routedViewer,
		ReadNotifications:           true,
		CommentMutation:             true,
		StateMutation:               true,
		MergeMutation:               true,
		ReviewMutation:              true,
		MutationHeadBinding:         true,
		WorkflowApproval:            true,
		ReadyForReview:              true,
		DraftMutation:               true,
		IssueMutation:               true,
		LabelMutation:               labels,
		AssigneeMutation:            assignees,
		ReviewerMutation:            reviewers,
		NotificationMutation:        true,
		ThreadReply:                 true,
		ReviewDraftMutation:         true,
		ReviewSuggestionApplication: true,
		ReadReviewThreads:           true,
		NativeMultilineRanges:       true,
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues:        archivePages,
			HistoricalMergeRequests: archivePages,
			OrdinaryComments:        archivePages,
			SubmittedReviews:        archivePages,
			InlineReviewComments:    archivePages,
		},
		SupportedReviewActions: []platform.ReviewAction{
			platform.ReviewActionComment,
			platform.ReviewActionApprove,
			platform.ReviewActionRequestChanges,
		},
	}
}

func (p *Provider) AuthenticatedUser(
	ctx context.Context,
	ref platform.RepoRef,
) (string, error) {
	return p.authenticatedViewerLoginForRepo(ctx, ref.Owner, ref.Name)
}

func (p *Provider) GetMarkdownImage(
	ctx context.Context,
	ref platform.RepoRef,
	sourceURL string,
) (platform.MarkdownImage, error) {
	reader, ok := p.client.(markdownImageClient)
	if !ok {
		return platform.MarkdownImage{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_markdown_images")
	}
	return reader.GetMarkdownImage(ctx, ref.Owner, ref.Name, sourceURL)
}

func (p *Provider) OperationRateLimitBuckets(
	operation platform.OperationName,
) ([]platform.RateLimitBucket, bool) {
	if operation != platform.OperationApplyReviewSuggestion {
		return nil, false
	}
	return []platform.RateLimitBucket{
		platform.RateLimitBucketREST,
		platform.RateLimitBucketGraphQL,
	}, true
}

func (p *Provider) GitHubClient() API {
	return p.client
}

func (p *Provider) ViewerAuthoredMergeRequest(
	ctx context.Context,
	mr platform.MergeRequest,
) (bool, error) {
	author := strings.TrimSpace(mr.Author)
	if author == "" {
		return false, nil
	}
	viewer, err := p.authenticatedViewerLoginForRepo(
		ctx, mr.Repo.Owner, mr.Repo.Name,
	)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(viewer, author), nil
}

func (p *Provider) authenticatedViewerLoginForRepo(
	ctx context.Context, owner, name string,
) (string, error) {
	cacheKey := p.authenticatedViewerLookupKeyForRepo(owner, name)
	p.viewerMu.Lock()
	defer p.viewerMu.Unlock()
	if entry, ok := p.viewerLogins[cacheKey]; ok && p.now().Sub(entry.fetchedAt) < p.viewerCacheTTL {
		return entry.login, nil
	}

	var login string
	var err error
	if routed, ok := p.client.(interface {
		AuthenticatedViewerLoginForRepo(context.Context, string, string) (string, error)
	}); ok {
		login, err = routed.AuthenticatedViewerLoginForRepo(ctx, owner, name)
	} else {
		client, ok := p.client.(ViewerAPI)
		if !ok {
			return "", fmt.Errorf("github client does not resolve authenticated viewer")
		}
		login, err = client.AuthenticatedViewerLogin(ctx)
	}
	if err != nil {
		return "", err
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return "", fmt.Errorf("authenticated viewer login is empty")
	}
	if p.viewerLogins == nil {
		p.viewerLogins = make(map[string]authenticatedViewerLoginCacheEntry)
	}
	p.viewerLogins[cacheKey] = authenticatedViewerLoginCacheEntry{login: login, fetchedAt: p.now()}
	return login, nil
}

func (p *Provider) AuthenticatedUserCacheKey(ref platform.RepoRef) string {
	return p.authenticatedViewerLookupKeyForRepo(ref.Owner, ref.Name)
}

func (p *Provider) authenticatedViewerLookupKeyForRepo(owner, name string) string {
	if cacheKey := p.authenticatedViewerCacheKeyForRepo(owner, name); cacheKey != "" {
		return cacheKey
	}
	return "repository:" + strings.ToLower(strings.TrimSpace(owner)) + "/" +
		strings.ToLower(strings.TrimSpace(name))
}

func (p *Provider) authenticatedViewerCacheKeyForRepo(owner, name string) string {
	if routed, ok := p.client.(interface {
		AuthenticatedViewerCacheKeyForRepo(string, string) string
	}); ok {
		return routed.AuthenticatedViewerCacheKeyForRepo(owner, name)
	}
	client, ok := p.client.(ViewerCacheKeyAPI)
	if !ok {
		return ""
	}
	return client.AuthenticatedViewerCacheKey()
}

func (p *Provider) ListNotifications(
	ctx context.Context,
	opts platform.NotificationListOptions,
) ([]platform.NotificationThread, bool, error) {
	return p.client.ListNotifications(ctx, opts)
}

func (p *Provider) MarkNotificationThreadRead(
	ctx context.Context,
	threadID string,
) error {
	return p.client.MarkNotificationThreadRead(ctx, threadID)
}

func (p *Provider) GetNotificationThreadForRepo(
	ctx context.Context, owner, name, threadID string,
) (platform.NotificationThread, error) {
	if routed, ok := p.client.(interface {
		GetNotificationThreadForRepo(context.Context, string, string, string) (platform.NotificationThread, error)
	}); ok {
		return routed.GetNotificationThreadForRepo(ctx, owner, name, threadID)
	}
	getter, ok := p.client.(interface {
		GetNotificationThread(context.Context, string) (platform.NotificationThread, error)
	})
	if !ok {
		return platform.NotificationThread{}, fmt.Errorf(
			"github client does not fetch notification threads",
		)
	}
	return getter.GetNotificationThread(ctx, threadID)
}

func (p *Provider) MarkNotificationThreadReadForRepo(
	ctx context.Context, owner, name, threadID string,
) error {
	if routed, ok := p.client.(interface {
		MarkNotificationThreadReadForRepo(context.Context, string, string, string) error
	}); ok {
		return routed.MarkNotificationThreadReadForRepo(
			ctx, owner, name, threadID,
		)
	}
	return p.client.MarkNotificationThreadRead(ctx, threadID)
}

func (p *Provider) GetRepository(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	repo, err := p.client.GetRepository(ctx, ref.Owner, ref.Name)
	if err != nil {
		return platform.Repository{}, err
	}
	return GitHubPlatformRepository(p.host, ref.Owner, repo), nil
}

// GitHubPlatformRepository converts a GitHub REST repository into the
// provider-neutral snapshot, preferring the canonical owner the provider
// reports over the requested route owner.
func GitHubPlatformRepository(
	host, requestedOwner string, repo *gh.Repository,
) platform.Repository {
	owner := requestedOwner
	if repo.GetOwner().GetLogin() != "" {
		owner = repo.GetOwner().GetLogin()
	}
	viewerCanMerge := GitHubViewerCanMerge(repo)
	var mergeSettings *platform.RepositoryMergeSettings
	if MergeSettingsComplete(repo) {
		mergeSettings = &platform.RepositoryMergeSettings{
			AllowSquashMerge: repo.GetAllowSquashMerge(),
			AllowMergeCommit: repo.GetAllowMergeCommit(),
			AllowRebaseMerge: repo.GetAllowRebaseMerge(),
		}
	}
	return platform.Repository{
		Ref: platform.RepoRef{
			Platform:           platform.KindGitHub,
			Host:               host,
			Owner:              strings.ToLower(owner),
			Name:               strings.ToLower(repo.GetName()),
			RepoPath:           strings.ToLower(owner) + "/" + strings.ToLower(repo.GetName()),
			PlatformID:         repo.GetID(),
			PlatformExternalID: repo.GetNodeID(),
			WebURL:             repo.GetHTMLURL(),
			CloneURL:           repo.GetCloneURL(),
			DefaultBranch:      repo.GetDefaultBranch(),
		},
		PlatformID:         repo.GetID(),
		PlatformExternalID: repo.GetNodeID(),
		Description:        repo.GetDescription(),
		Private:            repo.GetPrivate(),
		Archived:           repo.GetArchived(),
		MergeSettings:      mergeSettings,
		ViewerCanMerge:     viewerCanMerge,
		DefaultBranch:      repo.GetDefaultBranch(),
		WebURL:             repo.GetHTMLURL(),
		CloneURL:           repo.GetCloneURL(),
	}
}

func GitHubViewerCanMerge(repo *gh.Repository) *bool {
	if repo == nil || repo.Permissions == nil {
		return nil
	}
	canMerge := repo.Permissions.GetPush() ||
		repo.Permissions.GetMaintain() ||
		repo.Permissions.GetAdmin()
	return &canMerge
}

func (p *Provider) ListRepositories(
	ctx context.Context,
	owner string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos, err := p.client.ListRepositoriesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]platform.Repository, 0, len(repos))
	for _, repo := range repos {
		repoOwner := owner
		if repo.GetOwner().GetLogin() != "" {
			repoOwner = repo.GetOwner().GetLogin()
		}
		repoName := repo.GetName()
		out = append(out, platform.Repository{
			Ref: platform.RepoRef{
				Platform:           platform.KindGitHub,
				Host:               p.host,
				Owner:              strings.ToLower(repoOwner),
				Name:               strings.ToLower(repoName),
				RepoPath:           strings.ToLower(repoOwner) + "/" + strings.ToLower(repoName),
				PlatformID:         repo.GetID(),
				PlatformExternalID: repo.GetNodeID(),
				WebURL:             repo.GetHTMLURL(),
				CloneURL:           repo.GetCloneURL(),
				DefaultBranch:      repo.GetDefaultBranch(),
			},
			PlatformID:         repo.GetID(),
			PlatformExternalID: repo.GetNodeID(),
			Description:        repo.GetDescription(),
			Private:            repo.GetPrivate(),
			Archived:           repo.GetArchived(),
			DefaultBranch:      repo.GetDefaultBranch(),
			WebURL:             repo.GetHTMLURL(),
			CloneURL:           repo.GetCloneURL(),
		})
	}
	return out, nil
}

// mergeRequestsDisabledByRepository classifies a pulls-list 404 against the
// repository record. GitHub issues-only repositories report
// has_pull_requests=false and return a bare 404 from the pulls API for every
// credential, so without this probe the sync retries the repo as a hard
// failure every cycle and never reaches its issue phase. A repository that
// cannot be read, or that does not report the field, keeps the original
// error: only an explicit has_pull_requests=false is proof.
func (p *Provider) mergeRequestsDisabledByRepository(
	ctx context.Context,
	ref platform.RepoRef,
	err error,
) error {
	if StatusCode(err) != http.StatusNotFound {
		return nil
	}
	repo, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name)
	if repoErr != nil || repo == nil || repo.HasPullRequests == nil ||
		repo.GetHasPullRequests() {
		return nil
	}
	return platform.RepositoryFeatureDisabled(
		platform.KindGitHub, p.host, platform.RepositoryFeatureMergeRequests, err,
	)
}

func (p *Provider) ListOpenMergeRequests(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	prs, err := p.client.ListOpenPullRequests(ctx, ref.Owner, ref.Name)
	if err != nil {
		if disabledErr := RepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureMergeRequests, err,
		); disabledErr != nil {
			return nil, disabledErr
		}
		if disabledErr := p.mergeRequestsDisabledByRepository(ctx, ref, err); disabledErr != nil {
			return nil, disabledErr
		}
		return nil, err
	}
	out := make([]platform.MergeRequest, 0, len(prs))
	for _, pr := range prs {
		mr, err := NormalizePullRequest(ref, pr)
		if err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, nil
}

func (p *Provider) ListOpenMergeRequestsWithNativeStackHints(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, map[int]*NativeStackHint, error) {
	nativeClient, ok := p.client.(NativeStackClient)
	if !ok {
		mrs, err := p.ListOpenMergeRequests(ctx, ref)
		return mrs, nil, err
	}
	prs, hints, err := nativeClient.ListOpenPullRequestsWithNativeStackHints(
		ctx, ref.Owner, ref.Name,
	)
	if err != nil {
		// Same classification as ListOpenMergeRequests: a repository with pull
		// requests disabled must enter the feature cooldown, not be retried as a
		// hard failure every cycle just because the preview is enabled.
		if disabledErr := RepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureMergeRequests, err,
		); disabledErr != nil {
			return nil, nil, disabledErr
		}
		if disabledErr := p.mergeRequestsDisabledByRepository(ctx, ref, err); disabledErr != nil {
			return nil, nil, disabledErr
		}
		return nil, nil, err
	}
	out := make([]platform.MergeRequest, 0, len(prs))
	for _, pr := range prs {
		mr, err := NormalizePullRequest(ref, pr)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, mr)
	}
	return out, hints, nil
}

func (p *Provider) GetMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	_, mr, err := p.GetGitHubPullRequest(ctx, ref, number)
	return mr, err
}

func (p *Provider) GetGitHubPullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (*gh.PullRequest, platform.MergeRequest, error) {
	pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
	// The optimized detail path needs the full SDK object, so it fetches
	// raw; the failure and transfer outcomes still route through the one
	// canonical lookup classification.
	if outcomeErr := p.MergeRequestLookupOutcomeError(ctx, ref, number, pr, err); outcomeErr != nil {
		return nil, platform.MergeRequest{}, outcomeErr
	}
	mr, err := NormalizePullRequest(ref, pr)
	if err != nil {
		return nil, platform.MergeRequest{}, err
	}
	return pr, mr, nil
}

func (p *Provider) ListMergeRequestEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	comments, err := p.client.ListIssueComments(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	reviews, err := p.client.ListReviews(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	commits, err := p.client.ListCommits(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	timelineEvents, err := p.client.ListPullRequestTimelineEvents(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		p.warn("github provider timeline event fetch failed",
			"repo", ref.DisplayName(),
			"number", number,
			"err", err,
		)
		timelineEvents = nil
	}

	out := make([]platform.MergeRequestEvent, 0, len(comments)+len(reviews)+len(commits)+len(timelineEvents))
	for _, comment := range comments {
		out = append(out, NormalizeCommentEvent(ref, number, comment))
	}
	for _, review := range reviews {
		out = append(out, NormalizeReviewEvent(ref, number, review))
	}
	for i, commit := range commits {
		event := NormalizeCommitEvent(ref, number, commit)
		event.MetadataJSON = WithCommitOrderMetadata(event.MetadataJSON, i+1, i+1)
		out = append(out, event)
	}
	for _, timelineEvent := range timelineEvents {
		event := NormalizeTimelineEvent(ref, number, PullRequestTimelineEvent{
			NodeID:               timelineEvent.NodeID,
			EventType:            timelineEvent.EventType,
			Actor:                timelineEvent.Actor,
			Assignee:             timelineEvent.Assignee,
			CreatedAt:            timelineEvent.CreatedAt,
			DeletedCommentAuthor: timelineEvent.DeletedCommentAuthor,
			BeforeSHA:            timelineEvent.BeforeSHA,
			AfterSHA:             timelineEvent.AfterSHA,
			Ref:                  timelineEvent.Ref,
			PreviousTitle:        timelineEvent.PreviousTitle,
			CurrentTitle:         timelineEvent.CurrentTitle,
			PreviousRefName:      timelineEvent.PreviousRefName,
			CurrentRefName:       timelineEvent.CurrentRefName,
			SourceType:           timelineEvent.SourceType,
			SourceOwner:          timelineEvent.SourceOwner,
			SourceRepo:           timelineEvent.SourceRepo,
			SourceNumber:         timelineEvent.SourceNumber,
			SourceTitle:          timelineEvent.SourceTitle,
			SourceURL:            timelineEvent.SourceURL,
			IsCrossRepository:    timelineEvent.IsCrossRepository,
			WillCloseTarget:      timelineEvent.WillCloseTarget,
		})
		if event != nil {
			out = append(out, *event)
		}
	}
	return out, nil
}

// lookupNotPresentError renders the typed error a live caller receives when a
// single-item lookup resolves to a non-present outcome. Live callers require
// present; archive callers inspect the outcome instead. The outcomes must not
// collapse: removed is not_found, inaccessible is permission_denied (the
// behavior a raw 403 produced before lookup classification), and moved is
// not_found carrying the destination repository so callers can retarget the
// reference.
func (p *Provider) lookupNotPresentError(
	ref platform.RepoRef,
	number int,
	outcome lookupOutcome,
	destination *platform.RepoRef,
) error {
	code := platform.ErrCodeNotFound
	cause := fmt.Errorf("%s#%d is not present (%s)", ref.DisplayName(), number, outcome)
	if outcome == lookupInaccessible {
		code = platform.ErrCodePermissionDenied
		cause = errors.Join(platform.ErrLookupInaccessible, cause)
	} else {
		cause = errors.Join(platform.ErrLookupNotPresent, cause)
	}
	return &platform.Error{
		Code:         code,
		Provider:     platform.KindGitHub,
		PlatformHost: p.host,
		Destination:  destination,
		Err:          cause,
	}
}

// ListOpenGitHubIssues is the raw ETag-gated open-issue bulk read backing both
// the canonical listOpenIssuesPage normalization and the optimized GitHub
// index sync consumer. Because the raw slice is consumed without passing
// through the validating canonical reader wrapper, this method applies the
// equivalent contract checks itself, so no caller ever observes an unvalidated
// bulk observation.
func (p *Provider) ListOpenGitHubIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]*gh.Issue, error) {
	issues, err := p.client.ListOpenIssues(ctx, ref.Owner, ref.Name)
	if err != nil {
		if disabledErr := RepositoryFeatureDisabled(
			p.host, platform.RepositoryFeatureIssues, err,
		); disabledErr != nil {
			return nil, disabledErr
		}
		return nil, err
	}
	if err := p.validateOpenIssuesContract(ref, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func (p *Provider) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	issues, err := p.ListOpenGitHubIssues(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]platform.Issue, 0, len(issues))
	for _, issue := range issues {
		normalized, err := NormalizeIssue(ref, issue)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

// validateOpenIssuesContract applies the contract checks the validating
// canonical reader wrapper would run on a normalized open-issue page to the
// raw bulk result: items must be non-nil, item numbers positive and unique
// within the single exhausted open list, and every item bound to the
// requested repository. Monotonic-order checks do not apply because the open
// scan leaves traversal order contractually unspecified. Violations are typed
// provider contract errors so consumers reject the whole list instead of
// persisting from it.
func (p *Provider) validateOpenIssuesContract(
	ref platform.RepoRef,
	issues []*gh.Issue,
) error {
	seen := make(map[int]bool, len(issues))
	for _, issue := range issues {
		if issue == nil {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item",
				fmt.Errorf("provider returned a nil issue in the open list for %s", ref.DisplayName()),
			)
		}
		number := issue.GetNumber()
		if number <= 0 {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_number",
				fmt.Errorf("provider returned nonpositive issue number %d", number),
			)
		}
		if seen[number] {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_number",
				fmt.Errorf("provider returned duplicate issue number %d in one open list", number),
			)
		}
		seen[number] = true
		if destination := ArchiveDestination(ref, issue.GetRepositoryURL()); destination != nil {
			return platform.ProviderContract(
				platform.KindGitHub, p.host, "item_repo",
				fmt.Errorf(
					"provider returned issue %d bound to repository %s for requested %s",
					number, destination.RepoPath, ref.RepoPath,
				),
			)
		}
	}
	return nil
}

func (p *Provider) ListLabels(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.LabelCatalog, error) {
	client, ok := p.client.(LabelAPI)
	if !ok {
		return platform.LabelCatalog{}, platform.UnsupportedCapability(platform.KindGitHub, p.host, "read_labels")
	}
	labels, err := client.ListRepoLabels(ctx, ref.Owner, ref.Name)
	if err != nil {
		return platform.LabelCatalog{}, err
	}
	return platform.LabelCatalog{Labels: NormalizeLabels(ref, labels)}, nil
}

func (p *Provider) GetGitHubIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (*gh.Issue, error) {
	issue, err := p.client.GetIssue(ctx, ref.Owner, ref.Name, number)
	// Raw fetch for the optimized detail path; outcomes still route
	// through the one canonical lookup classification.
	if outcomeErr := p.IssueLookupOutcomeError(ctx, ref, number, issue, err); outcomeErr != nil {
		return nil, outcomeErr
	}
	if outcomeErr := p.IssuePullRequestOutcomeError(ref, number, issue); outcomeErr != nil {
		return nil, outcomeErr
	}
	return issue, nil
}

func (p *Provider) GetIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.Issue, error) {
	issue, err := p.GetGitHubIssue(ctx, ref, number)
	if err != nil {
		return platform.Issue{}, err
	}
	return NormalizeIssue(ref, issue)
}

func (p *Provider) ListIssueEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	comments, err := p.client.ListIssueComments(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	var timelineEvents []PullRequestTimelineEvent
	if timelineClient, ok := p.client.(issueTimelineLister); ok {
		timelineEvents, err = timelineClient.ListIssueTimelineEvents(ctx, ref.Owner, ref.Name, number)
		if err != nil {
			p.warn("github provider issue timeline event fetch failed",
				"repo", ref.DisplayName(),
				"number", number,
				"err", err,
			)
			timelineEvents = nil
		}
	}

	out := make([]platform.IssueEvent, 0, len(comments)+len(timelineEvents))
	for _, comment := range comments {
		out = append(out, NormalizeIssueCommentEvent(ref, number, comment))
	}
	for _, timelineEvent := range timelineEvents {
		event := NormalizeIssueTimelineEvent(ref, number, PullRequestTimelineEvent{
			NodeID:            timelineEvent.NodeID,
			EventType:         timelineEvent.EventType,
			Actor:             timelineEvent.Actor,
			Assignee:          timelineEvent.Assignee,
			CreatedAt:         timelineEvent.CreatedAt,
			SourceType:        timelineEvent.SourceType,
			SourceOwner:       timelineEvent.SourceOwner,
			SourceRepo:        timelineEvent.SourceRepo,
			SourceNumber:      timelineEvent.SourceNumber,
			SourceTitle:       timelineEvent.SourceTitle,
			SourceURL:         timelineEvent.SourceURL,
			IsCrossRepository: timelineEvent.IsCrossRepository,
			WillCloseTarget:   timelineEvent.WillCloseTarget,
		})
		if event != nil {
			out = append(out, *event)
		}
	}
	return out, nil
}

func (p *Provider) CreateMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.MergeRequestEvent, error) {
	comment, err := p.client.CreateIssueComment(ctx, ref.Owner, ref.Name, number, body)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no comment")
	}
	return NormalizeCommentEvent(ref, number, comment), nil
}

func (p *Provider) EditMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.MergeRequestEvent, error) {
	comment, err := p.client.EditIssueComment(ctx, ref.Owner, ref.Name, commentID, body)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no comment")
	}
	return NormalizeCommentEvent(ref, number, comment), nil
}

func (p *Provider) DeleteMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	return p.client.DeleteIssueComment(ctx, ref.Owner, ref.Name, commentID)
}

func (p *Provider) ReplyToThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	threadID string,
	body string,
) (platform.MergeRequestEvent, error) {
	commentID, err := strconv.ParseInt(strings.TrimSpace(threadID), 10, 64)
	if err != nil || commentID <= 0 {
		return platform.MergeRequestEvent{}, fmt.Errorf("invalid review comment ID")
	}
	comment, err := p.client.CreatePullRequestReviewCommentReply(
		ctx, ref.Owner, ref.Name, number, body, commentID,
	)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if comment == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no review comment")
	}
	return NormalizeReviewCommentEvent(ref, number, comment), nil
}

func (p *Provider) CreateIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.IssueEvent, error) {
	comment, err := p.client.CreateIssueComment(ctx, ref.Owner, ref.Name, number, body)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	if comment == nil {
		return platform.IssueEvent{}, fmt.Errorf("provider returned no comment")
	}
	return NormalizeIssueCommentEvent(ref, number, comment), nil
}

func (p *Provider) EditIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.IssueEvent, error) {
	comment, err := p.client.EditIssueComment(ctx, ref.Owner, ref.Name, commentID, body)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	if comment == nil {
		return platform.IssueEvent{}, fmt.Errorf("provider returned no comment")
	}
	return NormalizeIssueCommentEvent(ref, number, comment), nil
}

func (p *Provider) DeleteIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	_ int,
	commentID int64,
) error {
	return p.client.DeleteIssueComment(ctx, ref.Owner, ref.Name, commentID)
}

func (p *Provider) SetMergeRequestState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.MergeRequest, error) {
	ghPR, err := p.client.EditPullRequest(
		ctx, ref.Owner, ref.Name, number, EditPullRequestOpts{State: &state},
	)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if ghPR == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return NormalizePullRequest(ref, ghPR)
}

func (p *Provider) SetIssueState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.Issue, error) {
	ghIssue, err := p.client.EditIssue(ctx, ref.Owner, ref.Name, number, state)
	if err != nil {
		return platform.Issue{}, err
	}
	if ghIssue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return NormalizeIssue(ref, ghIssue)
}

// MergeMergeRequest passes expectedHeadSHA as the GitHub merge sha
// parameter: GitHub rejects the merge when the PR head moved past the
// reviewed commit, and that rejection is classified as stale_state.
func (p *Provider) MergeMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commitTitle string,
	commitMessage string,
	method string,
	expectedHeadSHA string,
) (platform.MergeResult, error) {
	result, err := p.client.MergePullRequest(
		ctx, ref.Owner, ref.Name, number, commitTitle, commitMessage, method, expectedHeadSHA,
	)
	if err != nil {
		if expectedHeadSHA != "" && IsGitHubHeadModified(err) {
			return platform.MergeResult{}, &platform.Error{
				Code:         platform.ErrCodeStaleState,
				Provider:     platform.KindGitHub,
				PlatformHost: p.host,
				Capability:   "merge_merge_request",
				Err:          err,
			}
		}
		return platform.MergeResult{}, err
	}
	if result == nil {
		return platform.MergeResult{}, fmt.Errorf("provider returned no merge result")
	}
	return platform.MergeResult{
		Merged:  result.GetMerged(),
		SHA:     result.GetSHA(),
		Message: result.GetMessage(),
	}, nil
}

// IsGitHubHeadModified reports whether a GitHub merge rejection is the
// sha-mismatch refusal ("Head branch was modified. Review and try the
// merge again.").
func IsGitHubHeadModified(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr == nil || ghErr.Response == nil {
		return false
	}
	if ghErr.Response.StatusCode != http.StatusConflict &&
		ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return strings.Contains(strings.ToLower(ghErr.Message), "head branch was modified")
}

func (p *Provider) ApproveWorkflow(
	ctx context.Context,
	ref platform.RepoRef,
	runID string,
) error {
	parsed, err := strconv.ParseInt(strings.TrimSpace(runID), 10, 64)
	if err != nil {
		return err
	}
	return p.client.ApproveWorkflowRun(ctx, ref.Owner, ref.Name, parsed)
}

func (p *Provider) MarkReadyForReview(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	pr, err := p.client.MarkPullRequestReadyForReview(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if pr == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return NormalizePullRequest(ref, pr)
}

func (p *Provider) ConvertMergeRequestToDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (time.Time, error) {
	pr, err := p.client.ConvertPullRequestToDraft(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return time.Time{}, err
	}
	if pr == nil {
		return time.Time{}, fmt.Errorf("provider returned no pull request")
	}
	if pr.UpdatedAt == nil || pr.UpdatedAt.IsZero() {
		return time.Time{}, fmt.Errorf("provider returned pull request without updated time")
	}
	return pr.UpdatedAt.UTC(), nil
}

func (p *Provider) CreateIssue(
	ctx context.Context,
	ref platform.RepoRef,
	title string,
	body string,
) (platform.Issue, error) {
	issue, err := p.client.CreateIssue(ctx, ref.Owner, ref.Name, title, body)
	if err != nil {
		return platform.Issue{}, err
	}
	if issue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return NormalizeIssue(ref, issue)
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

func (p *Provider) setIssueLikeLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	client, ok := p.client.(LabelAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "label_mutation")
	}
	labels, err := client.ReplaceIssueLabels(ctx, ref.Owner, ref.Name, number, names)
	if err != nil {
		return nil, err
	}
	return NormalizeLabels(ref, labels), nil
}

func (p *Provider) SetMergeRequestAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return p.setIssueLikeAssignees(ctx, ref, number, usernames)
}

func (p *Provider) SetIssueAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return p.setIssueLikeAssignees(ctx, ref, number, usernames)
}

func (p *Provider) setIssueLikeAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	client, ok := p.client.(AssigneeAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "assignee_mutation")
	}
	issue, err := client.ReplaceIssueAssignees(ctx, ref.Owner, ref.Name, number, usernames)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("provider returned no issue")
	}
	assignees := make([]string, 0, len(issue.Assignees))
	for _, user := range issue.Assignees {
		if user.GetLogin() != "" {
			assignees = append(assignees, user.GetLogin())
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
	client, ok := p.client.(ReviewerAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "reviewer_mutation")
	}
	if len(usernames) == 0 {
		// An empty request is the interface's read primitive: report
		// the provider's current requested-reviewer set untouched.
		pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
		if err != nil {
			return nil, err
		}
		if pr == nil {
			return nil, fmt.Errorf("provider returned no pull request")
		}
		return GithubRequestedReviewerLogins(pr), nil
	}
	pr, err := client.RequestPullRequestReviewers(ctx, ref.Owner, ref.Name, number, usernames)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("provider returned no pull request")
	}
	return GithubRequestedReviewerLogins(pr), nil
}

func (p *Provider) RemoveMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	client, ok := p.client.(ReviewerAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(platform.KindGitHub, p.host, "reviewer_mutation")
	}
	if err := client.RemovePullRequestReviewers(ctx, ref.Owner, ref.Name, number, usernames); err != nil {
		return nil, err
	}
	// The removal endpoint has no useful body; re-read the pull request
	// for the authoritative requested-reviewer set.
	pr, err := p.client.GetPullRequest(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("provider returned no pull request")
	}
	return GithubRequestedReviewerLogins(pr), nil
}

func GithubRequestedReviewerLogins(pr *gh.PullRequest) []string {
	logins := make([]string, 0, len(pr.RequestedReviewers))
	for _, user := range pr.RequestedReviewers {
		if user.GetLogin() != "" {
			logins = append(logins, user.GetLogin())
		}
	}
	return logins
}

func (p *Provider) ApproveMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) (platform.MergeRequestEvent, error) {
	review, err := p.client.CreateReviewWithComments(
		ctx,
		ref.Owner,
		ref.Name,
		number,
		"APPROVE",
		body,
		expectedHeadSHA,
		nil,
	)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	if review == nil {
		return platform.MergeRequestEvent{}, fmt.Errorf("provider returned no review")
	}
	return NormalizeReviewEvent(ref, number, review), nil
}

// RequestChanges submits a blocking review with exactly the head-binding
// contract of ApproveMergeRequest: the pin is forwarded as the review
// commit and GitHub attaches the review to it. No client-side head
// verification or post-submit revocation is layered on top — a change
// request from the review form must not carry a stronger submission
// contract than an approval from the same form.
func (p *Provider) RequestChanges(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) error {
	review, err := p.client.CreateReviewWithComments(
		ctx, ref.Owner, ref.Name, number, "REQUEST_CHANGES", body, expectedHeadSHA, nil,
	)
	if err != nil {
		return err
	}
	if review == nil {
		return fmt.Errorf("provider returned no review")
	}
	return nil
}

func (p *Provider) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	threads, err := p.client.ListPullRequestReviewThreads(ctx, ref.Owner, ref.Name, number)
	if err != nil {
		return nil, err
	}
	out := make([]platform.MergeRequestReviewThread, 0, len(threads))
	for _, thread := range threads {
		if len(thread.Comments) == 0 {
			continue
		}
		for _, comment := range thread.Comments {
			normalized := GithubReviewThreadComment(thread, comment)
			if normalized.ProviderThreadID == "" || normalized.ProviderCommentID == "" {
				continue
			}
			out = append(out, normalized)
		}
	}
	return out, nil
}

func GithubReviewThreadComment(
	thread PullRequestReviewThread,
	comment PullRequestReviewThreadComment,
) platform.MergeRequestReviewThread {
	createdAt := comment.CreatedAt.UTC()
	updatedAt := comment.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	return platform.MergeRequestReviewThread{
		ProviderThreadID:  thread.NodeID,
		ProviderReviewID:  GithubInt64ID(comment.ReviewDatabaseID),
		ProviderCommentID: FirstNonEmpty(GithubInt64ID(comment.DatabaseID), comment.NodeID),
		Body:              comment.Body,
		AuthorLogin:       comment.AuthorLogin,
		DirectURL:         comment.URL,
		Range:             GithubReviewLineRange(thread, comment),
		Resolved:          thread.IsResolved,
		MetadataJSON: NormalizeCommentVisibilityMetadata(CommentVisibility{
			Hidden: comment.IsMinimized, Reason: comment.MinimizedReason,
		}),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func GithubReviewLineRange(
	thread PullRequestReviewThread,
	comment PullRequestReviewThreadComment,
) platform.DiffReviewLineRange {
	side := strings.ToLower(thread.Side)
	if side != "left" {
		side = "right"
	}
	line := FirstPositive(thread.Line, thread.OriginalLine, comment.Line, comment.OriginalLine)
	startLine := thread.StartLine
	if startLine == nil {
		startLine = thread.OriginalStartLine
	}
	lineType := "add"
	var oldLine *int
	var newLine *int
	if strings.EqualFold(comment.SubjectType, "FILE") {
		lineType = "file"
	} else if side == "left" {
		lineType = "delete"
		oldLine = &line
	} else {
		newLine = &line
	}
	commitSHA := FirstNonEmpty(comment.CommitID, comment.OriginalCommitID)
	return platform.DiffReviewLineRange{
		Path:        FirstNonEmpty(thread.Path, comment.Path),
		Side:        side,
		StartSide:   GithubReviewStartSide(side, startLine),
		StartLine:   startLine,
		Line:        line,
		OldLine:     oldLine,
		NewLine:     newLine,
		LineType:    lineType,
		DiffHeadSHA: commitSHA,
		CommitSHA:   commitSHA,
	}
}

func GithubReviewStartSide(side string, startLine *int) string {
	if startLine == nil {
		return ""
	}
	return side
}

func GithubInt64ID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func FirstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (p *Provider) PublishDiffReviewDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.PublishDiffReviewDraftInput,
) (*platform.PublishedDiffReview, error) {
	event, err := GithubReviewEvent(input.Action)
	if err != nil {
		return nil, err
	}
	comments := make([]*gh.DraftReviewComment, 0, len(input.Comments))
	for _, comment := range input.Comments {
		comments = append(comments, GithubDraftReviewComment(comment))
	}
	headSHA := GithubReviewHeadSHA(input)
	review, err := p.client.CreateReviewWithComments(
		ctx,
		ref.Owner,
		ref.Name,
		number,
		event,
		input.Body,
		headSHA,
		comments,
	)
	if err != nil {
		return nil, err
	}
	if review == nil {
		return nil, fmt.Errorf("provider returned no review")
	}
	submittedAt := review.GetSubmittedAt() // zero Timestamp when GitHub omits submitted_at
	return &platform.PublishedDiffReview{
		ProviderReviewID: strconv.FormatInt(review.GetID(), 10),
		SubmittedAt:      submittedAt.Time,
	}, nil
}

func (p *Provider) ApplyReviewSuggestions(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	return p.client.ApplyReviewSuggestions(ctx, ref.Owner, ref.Name, number, input)
}

func GithubReviewEvent(action platform.ReviewAction) (string, error) {
	switch action {
	case platform.ReviewActionComment:
		return "COMMENT", nil
	case platform.ReviewActionApprove:
		return "APPROVE", nil
	case platform.ReviewActionRequestChanges:
		return "REQUEST_CHANGES", nil
	default:
		return "", fmt.Errorf("unsupported github review action %q", action)
	}
}

func GithubReviewHeadSHA(input platform.PublishDiffReviewDraftInput) string {
	if input.HeadSHA != "" {
		return input.HeadSHA
	}
	for _, comment := range input.Comments {
		if comment.Range.DiffHeadSHA != "" {
			return comment.Range.DiffHeadSHA
		}
		if comment.Range.CommitSHA != "" {
			return comment.Range.CommitSHA
		}
	}
	return ""
}

func GithubDraftReviewComment(comment platform.LocalDiffReviewDraftComment) *gh.DraftReviewComment {
	lineRange := comment.Range
	side := strings.ToUpper(lineRange.Side)
	next := &gh.DraftReviewComment{
		Path: &lineRange.Path,
		Body: &comment.Body,
		Side: &side,
		Line: &lineRange.Line,
	}
	if lineRange.StartLine != nil && lineRange.StartSide != "" {
		startSide := strings.ToUpper(lineRange.StartSide)
		next.StartSide = &startSide
		next.StartLine = lineRange.StartLine
	}
	return next
}

func (p *Provider) EditMergeRequestContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.MergeRequest, error) {
	pr, err := p.client.EditPullRequest(
		ctx, ref.Owner, ref.Name, number, EditPullRequestOpts{Title: title, Body: body},
	)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	if pr == nil {
		return platform.MergeRequest{}, fmt.Errorf("provider returned no pull request")
	}
	return NormalizePullRequest(ref, pr)
}

func (p *Provider) EditIssueContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.Issue, error) {
	ghIssue, err := p.client.EditIssueContent(
		ctx, ref.Owner, ref.Name, number, title, body,
	)
	if err != nil {
		return platform.Issue{}, err
	}
	if ghIssue == nil {
		return platform.Issue{}, fmt.Errorf("provider returned no issue")
	}
	return NormalizeIssue(ref, ghIssue)
}
