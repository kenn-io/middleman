package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

const (
	defaultPreviewLimit = 200
	maxPreviewLimit     = 1000
	defaultPageSize     = 100
)

type ClientOption func(*clientOptions)

type clientOptions struct {
	baseURL           string
	foregroundTimeout time.Duration
	rateTracker       platform.RateObserver
	transport         http.RoundTripper
	disableRetries    bool
	optionalContext   func(context.Context) context.Context
}

type Client struct {
	host              string
	baseURL           string
	api               *gitlab.Client
	httpClient        *http.Client
	foregroundTimeout time.Duration

	// userIDMu guards userIDs, a username -> user ID cache for
	// assignee/reviewer mutations. GitLab addresses users by numeric ID
	// while kenn-forge stores usernames, so lookups are cached for the
	// client lifetime (user IDs are immutable).
	userIDMu sync.Mutex
	userIDs  map[string]int64

	optionalContext   func(context.Context) context.Context
	projectCloneURLMu sync.Mutex
	projectCloneURLs  map[int64]string
}

type PreviewOptions struct {
	Limit int
	// IncludeArchived keeps archived projects in the listing.
	// Import previews leave it unset so archived projects stay
	// hidden; configuration enumeration sets it so archived
	// repositories can match configured globs.
	IncludeArchived bool
}

type PreviewResult struct {
	Repositories  []platform.Repository
	Limit         int
	ReturnedCount int
	ScannedCount  int
	Truncated     bool
	PartialErrors []PartialError
}

type PartialError struct {
	Code      string
	Namespace string
	Page      int64
}

func WithBaseURLForTesting(baseURL string) ClientOption {
	return func(opts *clientOptions) {
		opts.baseURL = strings.TrimRight(baseURL, "/")
	}
}

func WithForegroundTimeoutForTesting(timeout time.Duration) ClientOption {
	return func(opts *clientOptions) {
		opts.foregroundTimeout = timeout
	}
}

func WithRateTracker(rateTracker platform.RateObserver) ClientOption {
	return func(opts *clientOptions) {
		opts.rateTracker = rateTracker
	}
}

// WithTransport supplies the caller's HTTP transport, including admission.
func WithTransport(transport http.RoundTripper) ClientOption {
	return func(opts *clientOptions) { opts.transport = transport }
}

// WithOptionalRequestContext lets the caller demote optional enrichment work.
func WithOptionalRequestContext(adapt func(context.Context) context.Context) ClientOption {
	return func(opts *clientOptions) { opts.optionalContext = adapt }
}

func WithoutRetriesForTesting() ClientOption {
	return func(opts *clientOptions) {
		opts.disableRetries = true
	}
}

func NewClient(host string, source platform.CredentialSource, options ...ClientOption) (*Client, error) {
	opts := clientOptions{
		baseURL:           "https://" + strings.TrimRight(host, "/") + "/api/v4",
		foregroundTimeout: 20 * time.Second,
	}
	for _, option := range options {
		option(&opts)
	}
	if opts.transport == nil {
		return nil, &platform.Error{Code: platform.ErrCodeInvalidArgument, Field: "transport"}
	}

	clientOptions := []gitlab.ClientOptionFunc{gitlab.WithBaseURL(opts.baseURL)}
	if opts.disableRetries {
		clientOptions = append(clientOptions, gitlab.WithoutRetries())
	}
	baseTransport := opts.transport
	if opts.rateTracker != nil {
		baseTransport = &rateTrackingTransport{
			base:        baseTransport,
			rateTracker: opts.rateTracker,
		}
	}
	authRT := platform.AuthTransport{
		Source:              source,
		Base:                baseTransport,
		SetHeader:           platform.PrivateTokenHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       opts.baseURL,
	}
	httpClient := &http.Client{
		Transport: authRT,
	}
	clientOptions = append(clientOptions, gitlab.WithHTTPClient(httpClient))

	api, err := gitlab.NewClient("", clientOptions...)
	if err != nil {
		return nil, err
	}
	return &Client{
		host:              host,
		baseURL:           opts.baseURL,
		api:               api,
		httpClient:        httpClient,
		foregroundTimeout: opts.foregroundTimeout,
		userIDs:           make(map[string]int64),
		projectCloneURLs:  make(map[int64]string),
		optionalContext:   opts.optionalContext,
	}, nil
}

type rateTrackingTransport struct {
	base        http.RoundTripper
	rateTracker platform.RateObserver
}

func (t *rateTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp == nil || t.rateTracker == nil {
		return resp, err
	}
	t.rateTracker.RecordRequest()
	if rate, ok := parseRateLimitHeaders(resp); ok {
		t.rateTracker.UpdateFromRate(rate)
	}
	return resp, err
}

func parseRateLimitHeaders(resp *http.Response) (platform.Rate, bool) {
	remaining, remainingOK := parseHeaderInt(resp, "RateLimit-Remaining", "X-RateLimit-Remaining")
	if !remainingOK {
		return platform.Rate{}, false
	}
	limit, _ := parseHeaderInt(resp, "RateLimit-Limit", "X-RateLimit-Limit")
	// Leave Reset at its zero value when the provider doesn't supply a
	// reset header, matching the GitHub REST transport's behavior
	// (parseRateLimitHeaders in graphql.go). A synthesized near-future
	// reset here would look "plausible" to the archive budget's
	// SyncBudget.ArchiveSpendCeiling and release archive surplus that no
	// provider signal actually justifies; only a provider-observed reset
	// may do that.
	var resetAt time.Time
	if resetUnix, ok := parseHeaderInt64(resp, "RateLimit-Reset", "X-RateLimit-Reset"); ok {
		resetAt = time.Unix(resetUnix, 0).UTC()
	}
	return platform.Rate{
		Limit:     limit,
		Remaining: remaining,
		Reset:     resetAt,
	}, true
}

func parseHeaderInt(resp *http.Response, names ...string) (int, bool) {
	for _, name := range names {
		raw := strings.TrimSpace(resp.Header.Get(name))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func parseHeaderInt64(resp *http.Response, names ...string) (int64, bool) {
	for _, name := range names {
		raw := strings.TrimSpace(resp.Header.Get(name))
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func (c *Client) Platform() platform.Kind {
	return platform.KindGitLab
}

func (c *Client) Host() string {
	return c.host
}

func (c *Client) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories:       true,
		ReadMergeRequests:      true,
		ReadIssues:             true,
		ReadIssuePRReferences:  true,
		ReadComments:           true,
		ReadReleases:           true,
		ReadCI:                 true,
		ReadLabels:             true,
		ReadMarkdownImages:     true,
		ReadAuthenticatedUser:  true,
		CommentMutation:        true,
		StateMutation:          true,
		MergeMutation:          true,
		ReviewMutation:         true,
		IssueMutation:          true,
		LabelMutation:          true,
		AssigneeMutation:       true,
		ReviewerMutation:       true,
		ThreadReply:            true,
		ThreadResolve:          true,
		ReviewDraftMutation:    true,
		ReviewThreadResolution: true,
		ReadReviewThreads:      true,
		NativeMultilineRanges:  false,
		MutationHeadBinding:    true,
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues:     true,
			OrdinaryComments:     true,
			InlineReviewComments: true,
		},
		// GitLab has no native "request changes" review state, so
		// request_changes is intentionally absent.
		SupportedReviewActions: []platform.ReviewAction{
			platform.ReviewActionComment,
			platform.ReviewActionApprove,
		},
	}
}

func (c *Client) AuthenticatedUser(
	ctx context.Context,
	_ platform.RepoRef,
) (string, error) {
	user, _, err := c.api.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		return "", c.mapGitLabError("read_authenticated_user", err)
	}
	if user == nil || strings.TrimSpace(user.Username) == "" {
		return "", errors.New("authenticated GitLab username is empty")
	}
	return strings.TrimSpace(user.Username), nil
}

func (c *Client) GetRepository(ctx context.Context, ref platform.RepoRef) (platform.Repository, error) {
	pid, err := projectLookupArg(ref)
	if err != nil {
		return platform.Repository{}, err
	}
	project, _, err := c.api.Projects.GetProject(pid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return platform.Repository{}, mapGitLabError("get_repository", err)
	}
	return NormalizeProject(c.host, project)
}

func (c *Client) ListRepositories(
	ctx context.Context,
	owner string,
	opts platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	preview, err := c.PreviewNamespace(ctx, owner, PreviewOptions{
		Limit:           opts.Limit,
		IncludeArchived: opts.IncludeArchived,
	})
	if err != nil {
		return nil, err
	}
	return preview.Repositories, nil
}

func (c *Client) PreviewNamespace(
	ctx context.Context,
	namespace string,
	opts PreviewOptions,
) (PreviewResult, error) {
	if err := ctx.Err(); err != nil {
		return PreviewResult{}, err
	}

	limit, capped := normalizePreviewLimit(opts.Limit)
	result := PreviewResult{Limit: limit, Truncated: capped}
	ctx, cancel := c.withForegroundTimeout(ctx)
	defer cancel()

	result, err := c.previewGroup(ctx, namespace, result, opts.IncludeArchived)
	if err == nil {
		return result.finish(), nil
	}
	if !isGitLabNotFound(err) {
		return PreviewResult{}, mapGitLabError("preview_group", err)
	}
	if err := ctx.Err(); err != nil {
		return PreviewResult{}, err
	}
	result = PreviewResult{Limit: limit, Truncated: capped}
	result, err = c.previewUser(ctx, namespace, result, opts.IncludeArchived)
	if err != nil {
		return PreviewResult{}, mapGitLabError("preview_user", err)
	}
	return result.finish(), nil
}

func (c *Client) optionalHeadRepoCloneURL(
	ctx context.Context,
	ref platform.RepoRef,
	targetProjectID int64,
	sourceProjectID int64,
) (string, bool, error) {
	if sourceProjectID == 0 {
		return "", true, nil
	}
	cloneURL, err := c.headRepoCloneURL(ctx, ref, targetProjectID, sourceProjectID)
	if err == nil {
		return cloneURL, false, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, ctxErr
	}
	if isUnavailableSourceProjectError(err) {
		return "", true, nil
	}
	return "", false, err
}

func (c *Client) headRepoCloneURL(
	ctx context.Context,
	ref platform.RepoRef,
	targetProjectID int64,
	sourceProjectID int64,
) (string, error) {
	if sourceProjectID == targetProjectID {
		return ref.CloneURL, nil
	}
	return c.projectCloneURL(ctx, sourceProjectID)
}

func (c *Client) projectCloneURL(ctx context.Context, projectID int64) (string, error) {
	c.projectCloneURLMu.Lock()
	cached, ok := c.projectCloneURLs[projectID]
	c.projectCloneURLMu.Unlock()
	if ok {
		return cached, nil
	}

	project, _, err := c.api.Projects.GetProject(
		projectID,
		nil,
		gitlab.WithContext(ctx),
		gitlab.WithRequestRetry(func(context.Context, *http.Response, error) (bool, error) {
			return false, nil
		}),
	)
	if err != nil || project == nil {
		if err == nil {
			err = errors.New("source project response was empty")
		}
		return "", mapSourceProjectLookupError(err)
	}
	cloneURL := strings.TrimSpace(project.HTTPURLToRepo)
	c.projectCloneURLMu.Lock()
	c.projectCloneURLs[projectID] = cloneURL
	c.projectCloneURLMu.Unlock()
	return cloneURL, nil
}

func (c *Client) ListOpenMergeRequests(
	ctx context.Context,
	ref platform.RepoRef,
) ([]platform.MergeRequest, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	state := "opened"
	recheck := true
	opt := &gitlab.ListProjectMergeRequestsOptions{
		State: &state, WithMergeStatusRecheck: &recheck,
		Page: 1, PerPage: defaultPageSize,
	}
	var out []platform.MergeRequest
	// Fork-source clone URL lookups are enrichment, not discovery: they run
	// on the optional budget so they cannot drain the essential reserve, and
	// a budget-refused lookup degrades to an unknown head repo instead of
	// discarding the fetched list.
	enrichCtx := ctx
	if c.optionalContext != nil {
		enrichCtx = c.optionalContext(ctx)
	}
	for {
		mrs, resp, err := c.api.MergeRequests.ListProjectMergeRequests(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, c.repositoryFeatureError(
				ctx, normalizedRef, platform.RepositoryFeatureMergeRequests,
				"list_merge_requests", err,
			)
		}
		for _, mr := range mrs {
			normalized := NormalizeMergeRequest(normalizedRef, mr, nil)
			normalized.HeadRepoCloneURL, normalized.HeadRepoCloneURLUnknown, err =
				c.optionalHeadRepoCloneURL(enrichCtx, normalizedRef, mr.ProjectID, mr.SourceProjectID)
			if err != nil {
				if errors.Is(err, platform.ErrSyncBudgetExhausted) {
					normalized.HeadRepoCloneURL = ""
					normalized.HeadRepoCloneURLUnknown = true
				} else {
					return nil, err
				}
			}
			out = append(out, normalized)
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) GetMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	mr, _, err := c.api.MergeRequests.GetMergeRequest(pid, int64(number), nil, gitlab.WithContext(ctx))
	if err != nil {
		return platform.MergeRequest{}, c.repositoryItemLookupError(
			ctx, normalizedRef, platform.RepositoryFeatureMergeRequests,
			"get_merge_request", err,
		)
	}
	normalized := NormalizeDetailedMergeRequest(normalizedRef, mr)
	normalized.HeadRepoCloneURL, normalized.HeadRepoCloneURLUnknown, err =
		c.optionalHeadRepoCloneURL(ctx, normalizedRef, mr.ProjectID, mr.SourceProjectID)
	return normalized, err
}

func (c *Client) ListMergeRequestEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	events, err := c.listMergeRequestDiscussionEvents(ctx, pid, normalizedRef, number)
	if err != nil {
		return nil, err
	}
	commits, err := c.listMergeRequestCommits(ctx, pid, normalizedRef, number)
	if err != nil {
		return nil, err
	}
	for _, commit := range commits {
		events = append(events, NormalizeCommitEvent(normalizedRef, number, commit))
	}
	return events, nil
}

func (c *Client) ListMergeRequestComments(ctx context.Context, ref platform.RepoRef, number int) ([]platform.MergeRequestEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.listMergeRequestDiscussionEvents(ctx, pid, normalizedRef, number)
}

// listMergeRequestDiscussionEvents drains the shared discussions page fetcher
// without the canonical ordinary-comment filter: the live event surface keeps
// system-note events (assignment and lifecycle history) and position-less
// replies inside inline threads, which the archive comments dataset
// deliberately excludes.
func (c *Client) listMergeRequestDiscussionEvents(
	ctx context.Context,
	pid any,
	normalizedRef platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	discussions, err := collectGitLabPages(ctx, func(ctx context.Context, page int64) ([]*gitlab.Discussion, int64, error) {
		return c.listMergeRequestDiscussionsPage(ctx, pid, normalizedRef, number, page)
	})
	if err != nil {
		return nil, err
	}
	return NormalizeMergeRequestDiscussions(normalizedRef, number, gitLabMergeRequestURL(normalizedRef, number), discussions), nil
}

func (c *Client) ListIssueEvents(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	discussions, err := c.listIssueDiscussions(ctx, pid, normalizedRef, number)
	if err != nil {
		return nil, err
	}
	events := NormalizeIssueDiscussions(
		normalizedRef, number, gitLabIssueURL(normalizedRef, number), discussions,
	)
	related, err := c.listIssueRelatedMergeRequests(ctx, pid, normalizedRef, number)
	if err != nil {
		return nil, err
	}
	events = append(events, NormalizeIssueRelatedMergeRequests(normalizedRef, number, related)...)
	return events, nil
}

func (c *Client) ListOpenIssues(ctx context.Context, ref platform.RepoRef) ([]platform.Issue, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	state := "opened"
	opt := &gitlab.ListProjectIssuesOptions{
		State: &state, Page: 1, PerPage: defaultPageSize,
	}
	var out []platform.Issue
	for {
		issues, resp, err := c.api.Issues.ListProjectIssues(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, c.repositoryFeatureError(
				ctx, normalizedRef, platform.RepositoryFeatureIssues,
				"list_issues", err,
			)
		}
		for _, issue := range issues {
			out = append(out, NormalizeIssue(normalizedRef, issue))
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) GetIssue(ctx context.Context, ref platform.RepoRef, number int) (platform.Issue, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Issue{}, err
	}
	issue, _, err := c.api.Issues.GetIssue(pid, int64(number), nil, gitlab.WithContext(ctx))
	if err != nil {
		return platform.Issue{}, c.repositoryItemLookupError(
			ctx, normalizedRef, platform.RepositoryFeatureIssues,
			"get_issue", err,
		)
	}
	return NormalizeIssue(normalizedRef, issue), nil
}

func (c *Client) ListIssueComments(ctx context.Context, ref platform.RepoRef, number int) ([]platform.IssueEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	discussions, err := c.listIssueDiscussions(ctx, pid, normalizedRef, number)
	if err != nil {
		return nil, err
	}
	return normalizeIssueDiscussionComments(
		normalizedRef, number, gitLabIssueURL(normalizedRef, number), discussions,
	), nil
}

func (c *Client) listIssueDiscussions(
	ctx context.Context,
	pid any,
	ref platform.RepoRef,
	number int,
) ([]*gitlab.Discussion, error) {
	return collectGitLabPages(ctx, func(ctx context.Context, page int64) ([]*gitlab.Discussion, int64, error) {
		return c.listIssueDiscussionsPage(ctx, pid, ref, number, page)
	})
}

func (c *Client) ListReleases(ctx context.Context, ref platform.RepoRef) ([]platform.Release, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	opt := &gitlab.ListReleasesOptions{Page: 1, PerPage: defaultPageSize}

	var out []platform.Release
	for {
		releases, resp, err := c.api.Releases.ListReleases(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, mapGitLabError("list_releases", err)
		}
		for _, release := range releases {
			out = append(out, NormalizeRelease(normalizedRef, release))
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) ListTags(ctx context.Context, ref platform.RepoRef) ([]platform.Tag, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	opt := &gitlab.ListTagsOptions{Page: 1, PerPage: defaultPageSize}

	var out []platform.Tag
	for {
		tags, resp, err := c.api.Tags.ListTags(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, mapGitLabError("list_tags", err)
		}
		for _, tag := range tags {
			out = append(out, NormalizeTag(normalizedRef, tag))
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func gitLabMergeRequestURL(ref platform.RepoRef, number int) string {
	return gitLabItemURL(ref, "merge_requests", number)
}

func gitLabIssueURL(ref platform.RepoRef, number int) string {
	return gitLabItemURL(ref, "issues", number)
}

func gitLabItemURL(ref platform.RepoRef, kind string, number int) string {
	if ref.WebURL == "" || number <= 0 {
		return ""
	}
	return strings.TrimRight(ref.WebURL, "/") + "/-/" + kind + "/" + strconv.Itoa(number)
}

func (c *Client) ListCIChecks(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
) ([]platform.CICheck, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	opt := &gitlab.ListProjectPipelinesOptions{
		SHA:     &sha,
		Page:    1,
		PerPage: 1,
	}
	pipelines, _, err := c.api.Pipelines.ListProjectPipelines(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapGitLabError("list_ci_checks", err)
	}
	if len(pipelines) == 0 {
		return nil, nil
	}
	return []platform.CICheck{NormalizePipeline(normalizedRef, pipelines[0])}, nil
}

func (c *Client) listMergeRequestCommits(
	ctx context.Context,
	pid any,
	ref platform.RepoRef,
	number int,
) ([]*gitlab.Commit, error) {
	opt := &gitlab.GetMergeRequestCommitsOptions{Page: 1, PerPage: defaultPageSize}
	var out []*gitlab.Commit
	for {
		commits, resp, err := c.api.MergeRequests.GetMergeRequestCommits(pid, int64(number), opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, c.repositoryFeatureError(
				ctx, ref, platform.RepositoryFeatureMergeRequests,
				"list_merge_request_commits", err,
			)
		}
		out = append(out, commits...)
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) previewGroup(
	ctx context.Context,
	namespace string,
	result PreviewResult,
	includeArchived bool,
) (PreviewResult, error) {
	includeSubGroups := true
	opt := &gitlab.ListGroupProjectsOptions{
		IncludeSubGroups: &includeSubGroups,
		Page:             1,
		PerPage:          pageSizeForRemaining(result.Limit),
	}
	if !includeArchived {
		archived := false
		opt.Archived = &archived
	}
	for {
		projects, resp, err := c.api.Groups.ListGroupProjects(namespace, opt, gitlab.WithContext(ctx))
		if err != nil {
			if len(result.Repositories) > 0 {
				result.Truncated = true
				result.PartialErrors = append(result.PartialErrors, partialError(namespace, opt.Page, err))
				return result, nil
			}
			return result, err
		}
		result = appendPreviewProjects(result, c.host, namespace, projects, includeArchived)
		if result.ReturnedCount >= result.Limit {
			result.Truncated = true
			return result, nil
		}
		if resp == nil || resp.NextPage == 0 {
			return result, nil
		}
		opt.Page = resp.NextPage
		opt.PerPage = pageSizeForRemaining(result.Limit - result.ReturnedCount)
	}
}

func (c *Client) previewUser(
	ctx context.Context,
	namespace string,
	result PreviewResult,
	includeArchived bool,
) (PreviewResult, error) {
	opt := &gitlab.ListProjectsOptions{
		Page:    1,
		PerPage: pageSizeForRemaining(result.Limit),
	}
	if !includeArchived {
		archived := false
		opt.Archived = &archived
	}
	for {
		projects, resp, err := c.api.Projects.ListUserProjects(namespace, opt, gitlab.WithContext(ctx))
		if err != nil {
			if len(result.Repositories) > 0 {
				result.Truncated = true
				result.PartialErrors = append(result.PartialErrors, partialError(namespace, opt.Page, err))
				return result, nil
			}
			return result, err
		}
		result = appendPreviewProjects(result, c.host, namespace, projects, includeArchived)
		if result.ReturnedCount >= result.Limit {
			result.Truncated = true
			return result, nil
		}
		if resp == nil || resp.NextPage == 0 {
			return result, nil
		}
		opt.Page = resp.NextPage
		opt.PerPage = pageSizeForRemaining(result.Limit - result.ReturnedCount)
	}
}

func appendPreviewProjects(
	result PreviewResult,
	host string,
	namespace string,
	projects []*gitlab.Project,
	includeArchived bool,
) PreviewResult {
	for _, project := range projects {
		if project == nil {
			continue
		}
		result.ScannedCount++
		if project.Archived && !includeArchived {
			continue
		}
		if !namespaceMatches(namespace, project.PathWithNamespace) {
			continue
		}
		if result.ReturnedCount >= result.Limit {
			result.Truncated = true
			continue
		}
		repo, err := NormalizeProject(host, project)
		if err != nil {
			result.PartialErrors = append(result.PartialErrors, PartialError{
				Code:      "unsafe_project_path",
				Namespace: namespace,
			})
			continue
		}
		result.Repositories = append(result.Repositories, repo)
		result.ReturnedCount++
	}
	return result
}

func (r PreviewResult) finish() PreviewResult {
	r.ReturnedCount = len(r.Repositories)
	return r
}

func normalizePreviewLimit(limit int) (int, bool) {
	if limit <= 0 {
		return defaultPreviewLimit, false
	}
	if limit > maxPreviewLimit {
		return maxPreviewLimit, true
	}
	return limit, false
}

func pageSizeForRemaining(remaining int) int64 {
	if remaining <= 0 {
		return 1
	}
	if remaining < defaultPageSize {
		return int64(remaining)
	}
	return defaultPageSize
}

func namespaceMatches(namespace, repoPath string) bool {
	return repoPath == namespace || strings.HasPrefix(repoPath, namespace+"/")
}

func (c *Client) withForegroundTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.foregroundTimeout <= 0 {
		return ctx, func() {}
	}
	deadline := time.Now().Add(c.foregroundTimeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func (c *Client) projectScopedArg(ctx context.Context, ref platform.RepoRef) (any, platform.RepoRef, error) {
	if ref.PlatformID != 0 {
		return ref.PlatformID, c.normalizeRef(ref, ref.PlatformID), nil
	}
	repo, err := c.GetRepository(ctx, ref)
	if err != nil {
		return nil, platform.RepoRef{}, err
	}
	return repo.Ref.PlatformID, repo.Ref, nil
}

func (c *Client) normalizeRef(ref platform.RepoRef, id int64) platform.RepoRef {
	ref.Platform = platform.KindGitLab
	ref.Host = c.host
	ref.PlatformID = id
	if ref.PlatformExternalID == "" && id != 0 {
		ref.PlatformExternalID = strconv.FormatInt(id, 10)
	}
	return ref
}

func projectLookupArg(ref platform.RepoRef) (any, error) {
	if ref.PlatformID != 0 {
		return ref.PlatformID, nil
	}
	return rawProjectPath(ref)
}

func rawProjectPath(ref platform.RepoRef) (string, error) {
	repoPath := strings.Trim(ref.RepoPath, "/")
	if repoPath == "" {
		repoPath = strings.Trim(strings.Trim(ref.Owner, "/")+"/"+strings.Trim(ref.Name, "/"), "/")
	}
	if repoPath == "" || !strings.Contains(repoPath, "/") || hasEscapedSlash(repoPath) {
		return "", &platform.Error{
			Code:       platform.ErrCodeInvalidRepoRef,
			Provider:   platform.KindGitLab,
			Field:      "repo_path",
			Capability: "project_lookup",
		}
	}
	return repoPath, nil
}

func hasEscapedSlash(value string) bool {
	for {
		lower := strings.ToLower(value)
		if strings.Contains(lower, "%2f") {
			return true
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return true
		}
		if decoded == value {
			return false
		}
		if strings.Count(decoded, "/") > strings.Count(value, "/") {
			return true
		}
		value = decoded
	}
}

func (c *Client) mapGitLabError(capability string, err error) error {
	return mapGitLabErrorForHost(c.host, capability, err)
}

func mapGitLabError(capability string, err error) error {
	return mapGitLabErrorForHost("", capability, err)
}

func mapGitLabErrorForHost(platformHost, capability string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code := platform.ErrCodeProviderContract
	if errors.Is(err, gitlab.ErrNotFound) {
		code = platform.ErrCodeNotFound
	} else if gitlabErr, ok := errors.AsType[*gitlab.ErrorResponse](err); ok {
		switch {
		case gitlabErr.HasStatusCode(http.StatusUnauthorized), gitlabErr.HasStatusCode(http.StatusForbidden):
			code = platform.ErrCodePermissionDenied
		case gitlabErr.HasStatusCode(http.StatusNotFound):
			code = platform.ErrCodeNotFound
		case gitlabErr.HasStatusCode(http.StatusConflict):
			code = platform.ErrCodeConflict
		case gitlabErr.HasStatusCode(http.StatusTooManyRequests):
			code = platform.ErrCodeRateLimited
		}
	}
	return &platform.Error{
		Code:         code,
		Provider:     platform.KindGitLab,
		PlatformHost: platformHost,
		Capability:   capability,
		Err:          err,
	}
}

func mapSourceProjectLookupError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gitlab.ErrNotFound) {
		return err
	}
	if gitlabErr, ok := errors.AsType[*gitlab.ErrorResponse](err); ok {
		switch {
		case gitlabErr.HasStatusCode(http.StatusUnauthorized),
			gitlabErr.HasStatusCode(http.StatusForbidden),
			gitlabErr.HasStatusCode(http.StatusNotFound),
			gitlabErr.HasStatusCode(http.StatusTooManyRequests):
			return mapGitLabError("get_source_project", err)
		}
	}
	return err
}

// isGitLabStatus reports whether err carries a typed GitLab response with the
// given status. It matches only on the typed *gitlab.ErrorResponse (errors.As
// unwraps platform.Error). It deliberately does NOT fall back to substring
// matching on err.Error(): that string embeds the request URL, so an unrelated
// host:port (for example an ephemeral httptest port like 127.0.0.1:40404, or a
// project ID) could contain "404"/"403" and misclassify a transient 429/5xx as
// a not-found/forbidden error that callers then silently swallow.
//
// Note: go-gitlab does not return a typed *gitlab.ErrorResponse for 404s; it
// returns the sentinel gitlab.ErrNotFound. Use isGitLabNotFound for those.
func isGitLabStatus(err error, status int) bool {
	var gitlabErr *gitlab.ErrorResponse
	return errors.As(err, &gitlabErr) && gitlabErr.HasStatusCode(status)
}

// isGitLabNotFound reports whether err is a GitLab 404. go-gitlab's
// CheckResponse returns the sentinel gitlab.ErrNotFound (a plain error, not a
// *gitlab.ErrorResponse) for every 404, so errors.Is is the only reliable
// detection. errors.Is unwraps platform.Error, so wrapped errors match too.
func isGitLabNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound)
}

func isUnavailableSourceProjectError(err error) bool {
	if platformErr, ok := errors.AsType[*platform.Error](err); ok {
		if platformErr.Code == platform.ErrCodePermissionDenied ||
			platformErr.Code == platform.ErrCodeNotFound {
			return true
		}
	}
	return isGitLabNotFound(err) || isGitLabStatus(err, http.StatusForbidden)
}

func partialError(namespace string, page int64, err error) PartialError {
	code := "upstream_error"
	if platformErr, ok := errors.AsType[*platform.Error](mapGitLabError("preview_page", err)); ok {
		code = string(platformErr.Code)
		if code == string(platform.ErrCodeProviderContract) {
			code = "upstream_error"
		}
	}
	return PartialError{Code: code, Namespace: namespace, Page: page}
}

func pipelineInfo(mr *gitlab.MergeRequest) *gitlab.PipelineInfo {
	if mr == nil {
		return nil
	}
	if mr.Pipeline != nil {
		return mr.Pipeline
	}
	if mr.HeadPipeline != nil {
		return &gitlab.PipelineInfo{
			ID:        mr.HeadPipeline.ID,
			IID:       mr.HeadPipeline.IID,
			ProjectID: mr.HeadPipeline.ProjectID,
			Status:    mr.HeadPipeline.Status,
			Ref:       mr.HeadPipeline.Ref,
			SHA:       mr.HeadPipeline.SHA,
			WebURL:    mr.HeadPipeline.WebURL,
			CreatedAt: mr.HeadPipeline.CreatedAt,
			UpdatedAt: mr.HeadPipeline.UpdatedAt,
		}
	}
	return nil
}

var _ platform.Provider = (*Client)(nil)
var _ platform.RepositoryReader = (*Client)(nil)
var _ platform.MergeRequestReader = (*Client)(nil)
var _ platform.IssueReader = (*Client)(nil)
var _ platform.ReleaseReader = (*Client)(nil)
var _ platform.TagReader = (*Client)(nil)
var _ platform.CIReader = (*Client)(nil)
var _ platform.ThreadReplier = (*Client)(nil)
var _ platform.ThreadResolver = (*Client)(nil)
var _ platform.AssigneeMutator = (*Client)(nil)
var _ platform.ReviewerMutator = (*Client)(nil)
var _ platform.CommentMutator = (*Client)(nil)
var _ platform.StateMutator = (*Client)(nil)
var _ platform.MergeMutator = (*Client)(nil)
var _ platform.IssueMutator = (*Client)(nil)
var _ platform.ReviewMutator = (*Client)(nil)
var _ platform.MergeRequestContentMutator = (*Client)(nil)
var _ platform.IssueContentMutator = (*Client)(nil)
var _ platform.DiffReviewDraftMutator = (*Client)(nil)
var _ platform.DiffReviewThreadResolver = (*Client)(nil)
var _ platform.MergeRequestReviewThreadReader = (*Client)(nil)
