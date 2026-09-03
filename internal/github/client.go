package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/platform"
	platformgithub "go.kenn.io/forge/internal/platform/github"
	"go.kenn.io/forge/internal/tokenauth"
)

type ForcePushEvent struct {
	Actor     string
	BeforeSHA string
	AfterSHA  string
	Ref       string
	CreatedAt time.Time
}

type PullRequestTimelineEvent struct {
	NodeID               string
	EventType            string
	Actor                string
	Assignee             string
	CreatedAt            time.Time
	DeletedCommentAuthor string
	BeforeSHA            string
	AfterSHA             string
	Ref                  string
	PreviousTitle        string
	CurrentTitle         string
	PreviousRefName      string
	CurrentRefName       string
	SourceType           string
	SourceOwner          string
	SourceRepo           string
	SourceNumber         int
	SourceTitle          string
	SourceURL            string
	IsCrossRepository    bool
	WillCloseTarget      bool
}

type PullRequestReviewThread struct {
	NodeID            string
	IsResolved        bool
	IsOutdated        bool
	Path              string
	Side              string
	StartLine         *int
	OriginalStartLine *int
	Line              int
	OriginalLine      int
	Comments          []PullRequestReviewThreadComment
}

type PullRequestReviewThreadComment struct {
	NodeID           string
	DatabaseID       int64
	ReviewDatabaseID int64
	SubjectType      string
	Body             string
	AuthorLogin      string
	Path             string
	Line             int
	OriginalLine     int
	DiffHunk         string
	URL              string
	CommitID         string
	OriginalCommitID string
	IsMinimized      bool
	MinimizedReason  string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Notification list options and threads are provider-neutral types;
// the aliases keep this package's Client interface and its many
// implementations/mocks in their historical shape while letting the
// platform registry treat GitHub as one NotificationReader among the
// providers.
type NotificationListOptions = platform.NotificationListOptions

type NotificationThread = platform.NotificationThread

// EditPullRequestOpts holds optional fields for editing a pull request.
// Nil pointer fields are omitted from the GitHub API call.
type EditPullRequestOpts struct {
	State *string
	Title *string
	Body  *string
}

// Client is the interface for interacting with the GitHub API.
type Client interface {
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
	ListNotifications(ctx context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error)
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

// repoUserClient resolves a login with the credential serving a repository.
// User lookups are host-scoped on the wire, but they happen during repository
// sync, so an owner-scoped or App-only configuration with no host fallback
// route must still be able to reach /users through the repository's own
// credential rather than failing every display-name enrichment.
type repoUserClient interface {
	GetUserForRepo(
		ctx context.Context, owner, repo, login string,
	) (*gh.User, error)
}

type conditionalPullRequestGetter interface {
	GetPullRequestIfChanged(
		ctx context.Context,
		owner, repo string,
		number int,
		etag string,
	) (*gh.PullRequest, string, bool, error)
}

type conditionalIssueGetter interface {
	GetIssueIfChanged(
		ctx context.Context,
		owner, repo string,
		number int,
		etag string,
	) (*gh.Issue, string, bool, error)
}

type issueTimelineLister interface {
	ListIssueTimelineEvents(
		ctx context.Context,
		owner, repo string,
		number int,
	) ([]PullRequestTimelineEvent, error)
}

func normalizedPlatformHost(platformHost string) string {
	if platformHost == "" {
		return "github.com"
	}
	return strings.ToLower(platformHost)
}

func graphQLEndpointForHost(platformHost string) string {
	if platformHost == "" || platformHost == "github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + platformHost + "/api/graphql"

}

// ClientOption adjusts NewClient construction.
type ClientOption func(*clientOptions)

type clientOptions struct {
	baseURLOverride         string
	notificationRateTracker *RateTracker
	notificationBudget      *SyncBudget
	mutationsDisabled       bool
	quotaRegistry           *QuotaRegistry
	readIdentity            IdentityKey
	writeIdentity           IdentityKey
}

// WithBaseURLForTesting points the client's REST and GraphQL traffic
// at a local fake server (GHES-shaped /api/v3 and /api/graphql
// paths). Wire-level tests use it to exercise the real transport
// stack, including the read/write credential split, against an
// httptest server.
func WithBaseURLForTesting(base string) ClientOption {
	return func(o *clientOptions) {
		o.baseURLOverride = strings.TrimRight(base, "/")
	}
}

// ErrMissingWriteIdentity is returned when startup established an App-only
// read route without a user identity for mutations. A token that appears later
// cannot be used until restart assigns its stable accounting identity.
var ErrMissingWriteIdentity = errors.New("missing startup-resolved GitHub write identity")

// WithMutationsDisabled prevents mutation and notification transports from
// sending requests for an App-only route without a startup-resolved user.
func WithMutationsDisabled() ClientOption {
	return func(o *clientOptions) {
		o.mutationsDisabled = true
	}
}

// WithNotificationAccounting binds user-scoped notification traffic to the
// identity that authenticates it. GitHub App routes use installation tokens
// for reads but PATs for notifications, so those requests must spend the PAT
// budget and update the PAT rate tracker.
func WithNotificationAccounting(
	rateTracker *RateTracker, budget *SyncBudget,
) ClientOption {
	return func(o *clientOptions) {
		o.notificationRateTracker = rateTracker
		o.notificationBudget = budget
	}
}

// WithQuotaAccounting records each transport chain's observed GitHub quota
// against the principal that authenticates it. Reads spend readIdentity;
// mutations and notifications spend writeIdentity, which differs from
// readIdentity on split-auth routes where an App token serves reads.
func WithQuotaAccounting(
	registry *QuotaRegistry, readIdentity, writeIdentity IdentityKey,
) ClientOption {
	return func(o *clientOptions) {
		o.quotaRegistry = registry
		o.readIdentity = readIdentity
		o.writeIdentity = writeIdentity
	}
}

// NewClient creates a GitHub Client authenticated with the given
// token source. platformHost selects the API endpoint: "" or "github.com"
// uses the public API; any other value creates an Enterprise
// client. rateTracker and budget may be nil.
func NewClient(
	source tokenauth.Source,
	platformHost string,
	rateTracker *RateTracker,
	budget *SyncBudget,
	opts ...ClientOption,
) (Client, error) {
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}
	allowedOrigin := restAPIOriginForHost(platformHost)
	if options.baseURLOverride != "" {
		allowedOrigin = options.baseURLOverride
	}
	// wrapQuota attributes a chain's rate-limit headers to the principal that
	// authenticates it. It sits directly above the budget transport so every
	// wire attempt is observed, including AuthTransport's 401-retry.
	wrapQuota := func(
		base http.RoundTripper, identity IdentityKey,
	) http.RoundTripper {
		if options.quotaRegistry == nil || identity.Principal == "" {
			return base
		}
		return &quotaTransport{
			base: base, registry: options.quotaRegistry,
			identity: identity, resource: QuotaResourceREST,
		}
	}
	readBase := wrapQuota(
		WrapSyncBudgetTransport(http.DefaultTransport, budget),
		options.readIdentity,
	)
	authRT := tokenauth.AuthTransport{
		Source:              source,
		Base:                readBase,
		SetHeader:           tokenauth.BearerAuthHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       allowedOrigin,
		GitHubOwner:         githubOwnerFromRequest,
	}
	et := &etagTransport{base: authRT}
	httpClient := &http.Client{Transport: wrapPublicGitHubAPIGuard(et)}
	writeBudget := budget
	if options.notificationBudget != nil {
		writeBudget = options.notificationBudget
	}
	mutationAuthRT := authRT
	// The write path also serves background reads (the viewer-permission
	// overlay in GetRepository), so it charges the write identity's sync
	// budget. The budget transport is context-gated: foreground mutations
	// stay uncharged.
	mutationAuthRT.Base = wrapQuota(
		WrapSyncBudgetTransport(http.DefaultTransport, writeBudget),
		options.writeIdentity,
	)
	var mutationBase http.RoundTripper = mutationAuthTransport{base: mutationAuthRT}
	if options.mutationsDisabled {
		mutationBase = errorTransport{err: ErrMissingWriteIdentity}
	}
	// Mutations resolve auth with the mutation marker so a configured
	// GitHub App is skipped and writes stay attributed to the user's
	// own credential. The write path is a separate gh.Client because
	// go-github caches rate limits per client instance: sharing one
	// client would let an exhausted PAT (reported by a write response)
	// preemptively block app-token reads until the PAT window resets.
	// No etag transport: etags exist for sync reads.
	writeHTTPClient := &http.Client{Transport: wrapPublicGitHubAPIGuard(
		mutationBase,
	)}
	notificationAuthRT := authRT
	notificationAuthRT.Base = wrapQuota(
		WrapSyncBudgetTransport(http.DefaultTransport, writeBudget),
		options.writeIdentity,
	)
	var notificationRoundTripper http.RoundTripper = mutationAuthTransport{
		base: notificationAuthRT,
	}
	if options.mutationsDisabled {
		notificationRoundTripper = errorTransport{err: ErrMissingWriteIdentity}
	}
	notificationHTTPClient := &http.Client{Transport: wrapPublicGitHubAPIGuard(
		notificationRoundTripper,
	)}

	newGHClient := func(hc *http.Client) (*gh.Client, error) {
		opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(hc)}
		if options.baseURLOverride != "" {
			opts = append(opts, gh.WithEnterpriseURLs(
				options.baseURLOverride+"/api/v3/",
				options.baseURLOverride+"/api/uploads/",
			))
		} else if platformHost != "" && platformHost != "github.com" {
			baseURL := "https://" + platformHost + "/api/v3/"
			uploadURL := "https://" + platformHost + "/api/uploads/"
			opts = append(opts, gh.WithEnterpriseURLs(baseURL, uploadURL))
		}
		client, err := gh.NewClient(opts...)
		if err != nil {
			return nil, fmt.Errorf("create github client: %w", err)
		}
		return client, nil
	}
	ghClient, err := newGHClient(httpClient)
	if err != nil {
		return nil, err
	}
	ghWriteClient, err := newGHClient(writeHTTPClient)
	if err != nil {
		return nil, err
	}
	ghNotificationClient, err := newGHClient(notificationHTTPClient)
	if err != nil {
		return nil, err
	}
	graphQLEndpoint := graphQLEndpointForHost(platformHost)
	if options.baseURLOverride != "" {
		graphQLEndpoint = options.baseURLOverride + "/api/graphql"
	}
	return &liveClient{
		gh:                      ghClient,
		ghWrite:                 ghWriteClient,
		ghNotifications:         ghNotificationClient,
		source:                  source,
		httpClient:              httpClient,
		httpWriteClient:         writeHTTPClient,
		httpNotificationClient:  notificationHTTPClient,
		markdownImageHTTPClient: &http.Client{Transport: wrapPublicGitHubAPIGuard(http.DefaultTransport)},
		rateTracker:             rateTracker,
		notificationRateTracker: options.notificationRateTracker,
		platformHost:            normalizedPlatformHost(platformHost),
		graphQLEndpoint:         graphQLEndpoint,
		etag:                    et,
		quotaRegistry:           options.quotaRegistry,
	}, nil
}

func githubOwnerFromRequest(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	if owner := githubOwnerFromPath(req.URL.Path); owner != "" {
		return owner
	}
	if req.GetBody == nil {
		return ""
	}
	body, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	var payload struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return ""
	}
	owner, _ := payload.Variables["owner"].(string)
	return owner
}

func githubOwnerFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if part == "repos" && i+2 < len(parts) {
			return parts[i+1]
		}
		if (part == "orgs" || part == "users") && i+2 < len(parts) && parts[i+2] == "repos" {
			return parts[i+1]
		}
	}
	return ""
}

// mutationAuthTransport marks every request's context with
// tokenauth.WithMutationAuth before auth resolution, steering token
// selection away from github_app installation tokens.
type errorTransport struct {
	err error
}

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

type mutationAuthTransport struct {
	base http.RoundTripper
}

func (t mutationAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	marked := req.Clone(tokenauth.WithMutationAuth(req.Context()))
	if req.Body != nil && req.Body != http.NoBody {
		marked.Body = req.Body
	}
	return t.base.RoundTrip(marked)
}

func restAPIOriginForHost(platformHost string) string {
	if platformHost == "" || platformHost == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + platformHost
}

type liveClient struct {
	gh *gh.Client
	// ghWrite/httpWriteClient carry mutation traffic on the user's own
	// credential (mutation-marked auth context, own go-github rate
	// cache). Their responses feed writeRateTracker, not rateTracker:
	// the credentials have separate budgets when a GitHub App is
	// configured, and mutation availability gates on the write
	// credential's budget. Nil in hand-built test clients; accessors
	// fall back to the read client.
	ghWrite *gh.Client
	// ghNotifications/httpNotificationClient carry user-scoped
	// notification APIs on the user's own credential, through the
	// background sync budget transport. In GitHub App split-auth mode
	// their PAT rate headers must not feed the app-token read tracker or
	// the mutation write tracker.
	ghNotifications *gh.Client
	// source is the credential chain reads resolve through. Split
	// behavior (the viewer-permission overlay in GetRepository) is
	// derived from its current descriptor on every call, so a config
	// reload that adds or removes a GitHub App candidate takes effect
	// without restart and repo-override chains that exclude the app
	// keep single-credential behavior.
	source                  tokenauth.Source
	httpClient              *http.Client
	httpWriteClient         *http.Client
	httpNotificationClient  *http.Client
	markdownImageHTTPClient *http.Client
	rateTracker             *RateTracker
	writeRateTracker        *RateTracker
	notificationRateTracker *RateTracker
	graphQLRateTracker      *RateTracker
	writeGraphQLRateTracker *RateTracker
	platformHost            string
	graphQLEndpoint         string
	etag                    *etagTransport
	quotaRegistry           *QuotaRegistry
	viewerMu                sync.Mutex
	viewerLogin             string
	viewerLoginAt           time.Time
	viewerLoginCacheKey     string
}

func (c *liveClient) writeGH() *gh.Client {
	if c.ghWrite != nil {
		return c.ghWrite
	}
	return c.gh
}

func (c *liveClient) notificationGH() *gh.Client {
	if c.ghNotifications != nil {
		return c.ghNotifications
	}
	return c.writeGH()
}

func (c *liveClient) writeHTTPClient() *http.Client {
	if c.httpWriteClient != nil {
		return c.httpWriteClient
	}
	return c.httpClient
}

// SetGraphQLRateTracker attaches the tracker used by liveClient's direct
// GraphQL HTTP helpers.
func (c *liveClient) SetGraphQLRateTracker(rateTracker *RateTracker) {
	c.graphQLRateTracker = rateTracker
}

// SetWriteRateTracker attaches the tracker fed by mutation-path
// responses (the user's own credential when a GitHub App handles
// sync reads).
func (c *liveClient) SetWriteRateTracker(rateTracker *RateTracker) {
	c.writeRateTracker = rateTracker
}

// SetWriteGraphQLRateTracker attaches the tracker fed by GraphQL
// mutation responses (ready-for-review), which consume the write
// credential's GraphQL budget rather than its REST budget.
func (c *liveClient) SetWriteGraphQLRateTracker(rateTracker *RateTracker) {
	c.writeGraphQLRateTracker = rateTracker
}

// splitAuthActive reports whether reads currently resolve through a
// GitHub App installation token while writes use the user's own
// credential (the mutation-marked chain skips the app candidate).
func (c *liveClient) splitAuthActive() bool {
	if c.source == nil {
		return false
	}
	return c.source.Descriptor().HasActiveGitHubApp()
}

// splitAuthActiveForOwner is splitAuthActive scoped to a single owner:
// it reports whether reads for owner actually resolve through an app
// installation token. A host descriptor may carry app candidates for
// several owners, but a candidate scoped to one installation account is
// skipped for any other owner during token resolution. Gate
// installation-token-only endpoints on this so a PAT-backed owner on a
// host that also hosts another owner's app is not routed there.
func (c *liveClient) splitAuthActiveForOwner(owner string) bool {
	if c.source == nil {
		return false
	}
	return c.source.Descriptor().HasActiveGitHubAppForOwner(owner)
}

// ProbeWriteCredential resolves the mutation side of the route without
// sending a request. Identity-bound sources use this to detect a removed PAT
// or a token that rotated to another GitHub user before the UI offers writes.
func (c *liveClient) ProbeWriteCredential(ctx context.Context) error {
	if c.source == nil {
		return tokenauth.ErrMissingToken
	}
	_, err := c.source.Token(tokenauth.WithMutationAuth(ctx))
	return err
}

func (c *liveClient) bypassNotificationReadRateReserve() bool {
	return c.splitAuthActive()
}

// InvalidateListETagsForRepo evicts cached ETag entries for the repo's
// list endpoints. Pass any combination of "pulls", "issues", and
// "comments" to scope the invalidation; omitting endpoints clears
// every supported repo-scoped list path. Safe to call when the
// transport is nil (tests).
func (c *liveClient) InvalidateListETagsForRepo(owner, repo string, endpoints ...string) {
	if c.etag == nil {
		return
	}
	c.etag.invalidateRepo(owner, repo, endpoints...)
}

func (c *liveClient) ListNotifications(ctx context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
	page := max(opts.Page, 1)
	ghOpts := &gh.NotificationListOptions{
		All:           opts.All,
		Participating: opts.Participating,
		Page:          page, PerPage: 100,
	}
	if opts.Since != nil {
		ghOpts.Since = opts.Since.UTC()
	}
	var notifications []*gh.Notification
	var resp *gh.Response
	var err error
	if opts.RepoOwner != "" && opts.RepoName != "" {
		notifications, resp, err = c.notificationGH().Activity.ListRepositoryNotifications(ctx, opts.RepoOwner, opts.RepoName, ghOpts)
	} else {
		notifications, resp, err = c.notificationGH().Activity.ListNotifications(ctx, ghOpts)
	}
	c.trackNotificationRate(resp)
	if err != nil {
		return nil, false, err
	}
	threads := make([]NotificationThread, 0, len(notifications))
	for _, notification := range notifications {
		threads = append(threads, c.normalizeNotification(notification))
	}
	return threads, resp != nil && resp.NextPage != 0, nil
}

func (c *liveClient) GetNotificationThread(ctx context.Context, threadID string) (NotificationThread, error) {
	notification, resp, err := c.notificationGH().Activity.GetThread(ctx, threadID)
	c.trackNotificationRate(resp)
	if err != nil {
		return NotificationThread{}, err
	}
	return c.normalizeNotification(notification), nil
}

func (c *liveClient) MarkNotificationThreadRead(ctx context.Context, threadID string) error {
	resp, err := c.notificationGH().Activity.MarkThreadRead(ctx, threadID)
	c.trackNotificationRate(resp)
	return err
}

func (c *liveClient) normalizeNotification(n *gh.Notification) NotificationThread {
	if n == nil {
		return NotificationThread{}
	}
	thread := NotificationThread{
		ID:     n.GetID(),
		Reason: n.GetReason(),
		Unread: n.GetUnread(),
	}
	if updated := n.GetUpdatedAt(); !updated.IsZero() {
		thread.UpdatedAt = updated.UTC()
	}
	if lastRead := n.GetLastReadAt(); !lastRead.IsZero() {
		lastReadAt := lastRead.UTC()
		thread.LastReadAt = &lastReadAt
	}
	if repo := n.GetRepository(); repo != nil {
		thread.RepoName = strings.ToLower(repo.GetName())
		if owner := repo.GetOwner(); owner != nil {
			thread.RepoOwner = strings.ToLower(owner.GetLogin())
		}
	}
	if subject := n.GetSubject(); subject != nil {
		thread.SubjectType = subject.GetType()
		thread.SubjectTitle = subject.GetTitle()
		thread.SubjectURL = subject.GetURL()
		thread.SubjectLatestCommentURL = subject.GetLatestCommentURL()
		thread.ItemType, thread.ItemNumber, thread.WebURL = c.notificationItem(subject.GetType(), subject.GetURL(), thread.RepoOwner, thread.RepoName)
	}
	return thread
}

func (c *liveClient) notificationItem(subjectType, apiURL, owner, repo string) (string, *int, string) {
	itemType := "other"
	subjectLower := strings.ToLower(subjectType)
	switch subjectLower {
	case "pullrequest":
		itemType = "pr"
	case "issue":
		itemType = "issue"
	case "release":
		itemType = "release"
	case "commit":
		itemType = "commit"
	}

	if owner == "" || repo == "" || apiURL == "" {
		return itemType, nil, ""
	}
	segments := strings.Split(strings.TrimRight(apiURL, "/"), "/")
	lastSegment := func(prefix string) (string, bool) {
		for i := 0; i < len(segments)-1; i++ {
			if segments[i] == prefix {
				return segments[i+1], true
			}
		}
		return "", false
	}
	host := c.platformHost
	if host == "" {
		host = "github.com"
	}
	switch itemType {
	case "pr":
		if value, ok := lastSegment("pulls"); ok {
			if number, err := strconv.Atoi(value); err == nil {
				return itemType, &number, fmt.Sprintf("https://%s/%s/%s/pull/%d", host, owner, repo, number)
			}
		}
		if value, ok := lastSegment("issues"); ok {
			if number, err := strconv.Atoi(value); err == nil {
				return itemType, &number, fmt.Sprintf("https://%s/%s/%s/pull/%d", host, owner, repo, number)
			}
		}
	case "issue":
		if value, ok := lastSegment("issues"); ok {
			if number, err := strconv.Atoi(value); err == nil {
				return itemType, &number, fmt.Sprintf("https://%s/%s/%s/issues/%d", host, owner, repo, number)
			}
		}
	case "commit":
		if sha, ok := lastSegment("commits"); ok && sha != "" {
			return itemType, nil, fmt.Sprintf("https://%s/%s/%s/commit/%s", host, owner, repo, sha)
		}
	case "release":
		return itemType, nil, ""
	}
	return itemType, nil, ""
}

func (c *liveClient) ListReleases(
	ctx context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryRelease, error) {
	if perPage < 1 {
		perPage = 1
	}
	releases, resp, err := c.gh.Repositories.ListReleases(ctx, owner, repo, &gh.ListOptions{
		PerPage: perPage,
	})
	c.trackRate(resp)
	if err != nil {
		return nil, err
	}
	return releases, nil
}

func (c *liveClient) ListTags(
	ctx context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryTag, error) {
	if perPage < 1 {
		perPage = 1
	}
	tags, resp, err := c.gh.Repositories.ListTags(ctx, owner, repo, &gh.ListOptions{
		PerPage: perPage,
	})
	c.trackRate(resp)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func (c *liveClient) ListRepoLabels(
	ctx context.Context, owner, repo string,
) ([]*gh.Label, error) {
	var all []*gh.Label
	opts := &gh.ListOptions{PerPage: 100}
	for {
		labels, resp, err := c.gh.Issues.ListLabels(ctx, owner, repo, opts)
		c.trackRate(resp)
		if err != nil {
			return nil, err
		}
		all = append(all, labels...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func (c *liveClient) ReplaceIssueLabels(
	ctx context.Context, owner, repo string, number int, names []string,
) ([]*gh.Label, error) {
	labels, resp, err := c.writeGH().Issues.ReplaceLabelsForIssue(ctx, owner, repo, number, names)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, err
	}
	return labels, nil
}

// ReplaceIssueAssignees replaces the full assignee set on an issue or
// pull request (GitHub assigns PRs through the issues API).
func (c *liveClient) ReplaceIssueAssignees(
	ctx context.Context, owner, repo string, number int, usernames []string,
) (*gh.Issue, error) {
	if usernames == nil {
		usernames = []string{}
	}
	issue, resp, err := c.writeGH().Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{
		Assignees: &usernames,
	})
	c.trackWriteRate(resp)
	if err != nil {
		return nil, err
	}
	return issue, nil
}

// RequestPullRequestReviewers requests reviews from the given users and
// returns the updated pull request.
func (c *liveClient) RequestPullRequestReviewers(
	ctx context.Context, owner, repo string, number int, usernames []string,
) (*gh.PullRequest, error) {
	pr, resp, err := c.writeGH().PullRequests.RequestReviewers(ctx, owner, repo, number, gh.ReviewersRequest{
		Reviewers: usernames,
	})
	c.trackWriteRate(resp)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

// RemovePullRequestReviewers removes pending review requests for the
// given users.
func (c *liveClient) RemovePullRequestReviewers(
	ctx context.Context, owner, repo string, number int, usernames []string,
) error {
	resp, err := c.writeGH().PullRequests.RemoveReviewers(ctx, owner, repo, number, gh.ReviewersRequest{
		Reviewers: usernames,
	})
	c.trackWriteRate(resp)
	return err
}

func (c *liveClient) CreateIssue(
	ctx context.Context, owner, repo, title, body string,
) (*gh.Issue, error) {
	req := &gh.IssueRequest{
		Title: &title,
	}
	if body != "" {
		req.Body = &body
	}
	issue, resp, err := c.writeGH().Issues.Create(ctx, owner, repo, req)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, err
	}
	return issue, nil
}

const pullRequestTimelineEventsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      timelineItems(itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT, COMMENT_DELETED_EVENT, CROSS_REFERENCED_EVENT, RENAMED_TITLE_EVENT, BASE_REF_CHANGED_EVENT, ASSIGNED_EVENT, UNASSIGNED_EVENT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT], first: 100, after: $cursor) {
        nodes {
          __typename
          ... on Node {
            id
          }
          ... on HeadRefForcePushedEvent {
            actor { login }
            beforeCommit { oid }
            afterCommit { oid }
            createdAt
            ref { name }
          }
          ... on CommentDeletedEvent {
            actor { login }
            createdAt
            deletedCommentAuthor { login }
          }
          ... on CrossReferencedEvent {
            actor { login }
            createdAt
            isCrossRepository
            willCloseTarget
            source {
              __typename
              ... on Issue {
                number
                title
                url
                repository {
                  owner { login }
                  name
                }
              }
              ... on PullRequest {
                number
                title
                url
                repository {
                  owner { login }
                  name
                }
              }
            }
          }
          ... on RenamedTitleEvent {
            actor { login }
            createdAt
            previousTitle
            currentTitle
          }
          ... on BaseRefChangedEvent {
            actor { login }
            createdAt
            previousRefName
            currentRefName
          }
          ... on AssignedEvent {
            actor { login }
            assignee {
              __typename
              ... on Bot { login }
              ... on Mannequin { login }
              ... on Organization { login }
              ... on User { login }
            }
            createdAt
          }
          ... on UnassignedEvent {
            actor { login }
            assignee {
              __typename
              ... on Bot { login }
              ... on Mannequin { login }
              ... on Organization { login }
              ... on User { login }
            }
            createdAt
          }
          ... on MergedEvent {
            actor { login }
            createdAt
          }
          ... on ClosedEvent {
            actor { login }
            createdAt
          }
          ... on ReopenedEvent {
            actor { login }
            createdAt
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

const issueTimelineEventsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    issue(number: $number) {
      timelineItems(itemTypes: [ASSIGNED_EVENT, UNASSIGNED_EVENT, CROSS_REFERENCED_EVENT, CLOSED_EVENT, REOPENED_EVENT], first: 100, after: $cursor) {
        nodes {
          __typename
          ... on Node {
            id
          }
          ... on CrossReferencedEvent {
            actor { login }
            createdAt
            isCrossRepository
            willCloseTarget
            source {
              __typename
              ... on Issue {
                number
                title
                url
                repository {
                  owner { login }
                  name
                }
              }
              ... on PullRequest {
                number
                title
                url
                repository {
                  owner { login }
                  name
                }
              }
            }
          }
          ... on AssignedEvent {
            actor { login }
            assignee {
              __typename
              ... on Bot { login }
              ... on Mannequin { login }
              ... on Organization { login }
              ... on User { login }
            }
            createdAt
          }
          ... on UnassignedEvent {
            actor { login }
            assignee {
              __typename
              ... on Bot { login }
              ... on Mannequin { login }
              ... on Organization { login }
              ... on User { login }
            }
            createdAt
          }
          ... on ClosedEvent {
            actor { login }
            createdAt
          }
          ... on ReopenedEvent {
            actor { login }
            createdAt
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

const readyForReviewIDQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id
    }
  }
}`

const readyForReviewMutation = `
mutation($pullRequestId: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $pullRequestId}) {
    pullRequest {
      databaseId
      number
      title
      state
      isDraft
      locked
      body
      url
      author {
        login
      }
      createdAt
      updatedAt
      mergedAt
      closedAt
      additions
      deletions
      mergeable
      reviewDecision
      headRefName
      baseRefName
      headRefOid
      baseRefOid
      headRepository {
        url
      }
      labels(first: 100) {
        nodes {
          name
          color
          description
          isDefault
        }
      }
    }
  }
}`

const convertToDraftMutation = `
mutation($pullRequestId: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $pullRequestId}) {
    pullRequest {
      id
      updatedAt
    }
  }
}`

const pullRequestReviewThreadCommentsQuery = `
query($threadID: ID!, $cursor: String) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $cursor) {
        nodes {
          id
          databaseId
          fullDatabaseId
          body
          path
          line
          originalLine
          subjectType
          diffHunk
          url
          author { login }
          commit { oid }
          originalCommit { oid }
          pullRequestReview { databaseId }
          isMinimized
          minimizedReason
          createdAt
          updatedAt
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type graphQLReviewThreadComment struct {
	NodeID         string       `json:"id"`
	DatabaseID     graphQLInt64 `json:"databaseId"`
	FullDatabaseID graphQLInt64 `json:"fullDatabaseId"`
	Body           string       `json:"body"`
	Path           string       `json:"path"`
	Line           int          `json:"line"`
	OriginalLine   int          `json:"originalLine"`
	SubjectType    string       `json:"subjectType"`
	DiffHunk       string       `json:"diffHunk"`
	URL            string       `json:"url"`
	Author         *struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	OriginalCommit *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
	PullRequestReview *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"pullRequestReview"`
	IsMinimized     bool      `json:"isMinimized"`
	MinimizedReason string    `json:"minimizedReason"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type graphQLInt64 int64

func (value *graphQLInt64) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*value = 0
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		unquoted, err := strconv.Unquote(text)
		if err != nil {
			return fmt.Errorf("decode GraphQL int64: %w", err)
		}
		text = unquoted
		if text == "" {
			*value = 0
			return nil
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("decode GraphQL int64 %q: %w", text, err)
	}
	*value = graphQLInt64(parsed)
	return nil
}

type graphQLReviewThreadCommentConnection struct {
	Nodes    []graphQLReviewThreadComment `json:"nodes"`
	PageInfo struct {
		HasNextPage bool    `json:"hasNextPage"`
		EndCursor   *string `json:"endCursor"`
	} `json:"pageInfo"`
}

type readyForReviewError struct {
	err        error
	statusCode int
	staleState bool
}

func (e *readyForReviewError) Error() string      { return e.err.Error() }
func (e *readyForReviewError) Unwrap() error      { return e.err }
func (e *readyForReviewError) StatusCode() int    { return e.statusCode }
func (e *readyForReviewError) IsStaleState() bool { return e.staleState }

func newReadyForReviewError(err error, statusCode int, staleState bool) error {
	return &readyForReviewError{
		err:        err,
		statusCode: statusCode,
		staleState: staleState,
	}
}

func readyForReviewGraphQLErrorMeta(graphQLErrors []graphQLError) (int, bool) {
	for _, graphQLError := range graphQLErrors {
		if strings.EqualFold(graphQLError.Type, "NOT_FOUND") {
			return http.StatusNotFound, true
		}
		if strings.Contains(graphQLError.Message, "Could not resolve to a PullRequest") ||
			strings.Contains(graphQLError.Message, "Could not resolve to a node with the global id") {
			return http.StatusNotFound, true
		}
	}
	return 0, false
}

func joinGraphQLErrorMessages(graphQLErrors []graphQLError) string {
	messages := make([]string, 0, len(graphQLErrors))
	for _, graphQLError := range graphQLErrors {
		if graphQLError.Message != "" {
			messages = append(messages, graphQLError.Message)
		}
	}
	if len(messages) == 0 {
		return "unknown GraphQL error"
	}
	return strings.Join(messages, "; ")
}

// trackRate records the request and updates rate limit state
// from the response. Safe to call with nil response or nil
// tracker.
func (c *liveClient) trackRate(resp *gh.Response) {
	if resp == nil || c.rateTracker == nil {
		return
	}
	c.rateTracker.RecordRequest()
	c.rateTracker.UpdateFromRate(rateFromGitHub(resp.Rate))
}

// trackWriteRate records a write-path response against the mutation
// credential's tracker, so PAT exhaustion disables writes without
// pausing app-token reads. Hosts without a write tracker share one
// credential across reads and writes; their write responses feed the
// shared tracker, same as before the split existed.
func (c *liveClient) trackWriteRate(resp *gh.Response) {
	if c.writeRateTracker == nil {
		c.trackRate(resp)
		return
	}
	if resp == nil {
		return
	}
	c.writeRateTracker.RecordRequest()
	c.writeRateTracker.UpdateFromRate(rateFromGitHub(resp.Rate))
}

// trackNotificationRate records notification API responses only when the
// read client and notification client use the same credential. In GitHub App
// split-auth mode, reads use an installation token while notifications use
// the user's PAT, so PAT headers cannot update the installation read tracker.
func (c *liveClient) trackNotificationRate(resp *gh.Response) {
	if c.notificationRateTracker != nil {
		if resp == nil {
			return
		}
		c.notificationRateTracker.RecordRequest()
		c.notificationRateTracker.UpdateFromRate(rateFromGitHub(resp.Rate))
		return
	}
	if c.splitAuthActive() {
		return
	}
	c.trackRate(resp)
}

func (c *liveClient) GetRateLimitSnapshot(ctx context.Context) (*RateLimitSnapshot, error) {
	limits, _, err := c.gh.RateLimit.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get GitHub rate limit snapshot: %w", err)
	}
	if limits == nil {
		return &RateLimitSnapshot{}, nil
	}
	snapshot := &RateLimitSnapshot{}
	if limits.Core != nil {
		rate := rateFromGitHub(*limits.Core)
		snapshot.Core = &rate
	}
	if limits.GraphQL != nil {
		rate := rateFromGitHub(*limits.GraphQL)
		snapshot.GraphQL = &rate
	}
	return snapshot, nil
}

func (c *liveClient) trackGraphQLRateHeaders(resp *http.Response) {
	if resp == nil || c.graphQLRateTracker == nil {
		return
	}
	c.graphQLRateTracker.RecordRequest()
	remaining, err := parseRateHeaderInt(resp.Header, "X-RateLimit-Remaining")
	if err != nil {
		return
	}
	limit, err := parseRateHeaderInt(resp.Header, "X-RateLimit-Limit")
	if err != nil {
		return
	}
	resetUnix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return
	}
	c.graphQLRateTracker.UpdateFromRate(rateFromGitHubHeaders(
		limit, remaining, time.Unix(resetUnix, 0).UTC(),
	))
}

// trackWriteGraphQLRateHeaders records a GraphQL mutation response
// against the write credential's GraphQL tracker. Hosts without one
// share a single credential, so the shared GraphQL tracker absorbs
// the response instead.
func (c *liveClient) trackWriteGraphQLRateHeaders(resp *http.Response) {
	tracker := c.writeGraphQLRateTracker
	if tracker == nil {
		c.trackGraphQLRateHeaders(resp)
		return
	}
	if resp == nil {
		return
	}
	tracker.RecordRequest()
	remaining, err := parseRateHeaderInt(resp.Header, "X-RateLimit-Remaining")
	if err != nil {
		return
	}
	limit, err := parseRateHeaderInt(resp.Header, "X-RateLimit-Limit")
	if err != nil {
		return
	}
	resetUnix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return
	}
	tracker.UpdateFromRate(rateFromGitHubHeaders(
		limit, remaining, time.Unix(resetUnix, 0).UTC(),
	))
}

func parseRateHeaderInt(header http.Header, name string) (int, error) {
	value, err := strconv.ParseInt(header.Get(name), 10, strconv.IntSize)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

func (c *liveClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:   "open",
		PerPage: 100,
	}
	progress := newMergeRequestListFetchProgressLogger(RepoRef{
		Owner:        owner,
		Name:         repo,
		PlatformHost: c.platformHost,
	}, "rest")
	all, err := collectPagesWithProgress(ctx, func(pageOpts *gh.ListOptions) ([]*gh.PullRequest, *gh.Response, error) {
		opts.ListOptions = *pageOpts
		page, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("listing open pull requests for %s/%s: %w", owner, repo, err)
		}
		return page, resp, nil
	}, c.trackRate, progress.recordPage)
	if err != nil {
		return nil, err
	}
	progress.done()
	return all, nil
}

func (c *liveClient) ListOpenIssues(
	ctx context.Context, owner, repo string,
) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	progress := newIssueListFetchProgressLogger(RepoRef{
		Owner:        owner,
		Name:         repo,
		PlatformHost: c.platformHost,
	}, "rest")
	issues, err := collectPagesWithProgress(ctx, func(pageOpts *gh.ListOptions) ([]*gh.Issue, *gh.Response, error) {
		opts.ListOptions = *pageOpts
		issues, resp, err := c.gh.Issues.ListByRepo(
			ctx, owner, repo, opts,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"listing issues for %s/%s: %w", owner, repo, err,
			)
		}
		return issues, resp, nil
	}, c.trackRate, progress.recordPage)
	if err != nil {
		return nil, err
	}
	progress.done()

	var all []*gh.Issue
	// GitHub's Issues API returns PRs too — filter them out. Nil entries
	// pass through so contract validation can reject them typed instead of
	// the filter panicking on the dereference.
	for _, issue := range issues {
		if issue == nil || issue.PullRequestLinks == nil {
			all = append(all, issue)
		}
	}
	return all, nil
}

func (c *liveClient) ListRepositoriesByOwner(
	ctx context.Context, owner string,
) ([]*gh.Repository, error) {
	if c.splitAuthActiveForOwner(owner) {
		ctx = tokenauth.WithGitHubOwner(ctx, owner)
		repos, err := collectPages(
			ctx,
			func(opts *gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
				result, resp, err := c.gh.Apps.ListRepos(ctx, opts)
				if err != nil {
					return nil, resp, err
				}
				return result.Repositories, resp, nil
			},
			c.trackRate,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"listing installation repositories for %s: %w", owner, err,
			)
		}
		filtered := repos[:0]
		for _, repo := range repos {
			if strings.EqualFold(repo.GetOwner().GetLogin(), owner) {
				filtered = append(filtered, repo)
			}
		}
		return filtered, nil
	}

	viewerLogin, viewerErr := c.authenticatedLogin(ctx)
	if viewerErr == nil && strings.EqualFold(owner, viewerLogin) {
		repos, err := collectPages(
			ctx,
			func(opts *gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
				page, resp, err := c.gh.Repositories.ListByAuthenticatedUser(
					ctx, &gh.RepositoryListByAuthenticatedUserOptions{
						Affiliation: "owner",
						ListOptions: *opts,
					},
				)
				if err != nil {
					return nil, resp, err
				}
				return page, resp, nil
			},
			c.trackRate,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"listing repositories for authenticated owner %s: %w",
				owner, err,
			)
		}
		return repos, nil
	}

	orgRepos, err := collectPages(
		ctx,
		func(opts *gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
			page, resp, err := c.gh.Repositories.ListByOrg(
				ctx, owner, &gh.RepositoryListByOrgOptions{
					Type:        "all",
					ListOptions: *opts,
				},
			)
			if err != nil {
				return nil, resp, err
			}
			return page, resp, nil
		},
		c.trackRate,
	)
	if err == nil {
		return orgRepos, nil
	}

	userRepos, userErr := collectPages(
		ctx,
		func(opts *gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
			page, resp, err := c.gh.Repositories.ListByUser(
				ctx, owner, &gh.RepositoryListByUserOptions{
					Type:        "owner",
					ListOptions: *opts,
				},
			)
			if err != nil {
				return nil, resp, err
			}
			return page, resp, nil
		},
		c.trackRate,
	)
	if userErr != nil {
		return nil, fmt.Errorf(
			"listing repositories for %s: org=%v user=%w",
			owner, err, userErr,
		)
	}
	return userRepos, nil
}

func (c *liveClient) authenticatedLogin(ctx context.Context) (string, error) {
	cacheKey := c.authenticatedViewerCacheKey()
	c.viewerMu.Lock()
	defer c.viewerMu.Unlock()
	if c.viewerLogin != "" &&
		c.viewerLoginCacheKey == cacheKey &&
		time.Since(c.viewerLoginAt) < authenticatedViewerLoginTTL {
		return c.viewerLogin, nil
	}
	user, resp, err := c.writeGH().Users.Get(ctx, "")
	c.trackWriteRate(resp)
	if err != nil {
		return "", fmt.Errorf("getting authenticated user: %w", err)
	}
	login := user.GetLogin()
	if login == "" {
		return "", fmt.Errorf("authenticated user login is empty")
	}
	c.viewerLogin = login
	c.viewerLoginAt = time.Now()
	c.viewerLoginCacheKey = cacheKey
	return login, nil
}

func (c *liveClient) AuthenticatedViewerLogin(ctx context.Context) (string, error) {
	return c.authenticatedLogin(ctx)
}

func (c *liveClient) AuthenticatedViewerCacheKey() string {
	return c.authenticatedViewerCacheKey()
}

func (c *liveClient) authenticatedViewerCacheKey() string {
	if c.source == nil {
		return ""
	}
	return c.source.Descriptor().CanonicalSourceString()
}

func (c *liveClient) GetIssue(
	ctx context.Context, owner, repo string, number int,
) (*gh.Issue, error) {
	issue, resp, err := c.gh.Issues.Get(ctx, owner, repo, number)
	c.trackRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"getting issue %s/%s#%d: %w", owner, repo, number, err,
		)
	}
	return issue, nil
}

func (c *liveClient) GetIssueIfChanged(
	ctx context.Context,
	owner, repo string,
	number int,
	etag string,
) (*gh.Issue, string, bool, error) {
	u := fmt.Sprintf("repos/%v/%v/issues/%v", owner, repo, number)
	req, err := c.gh.NewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	issue := new(gh.Issue)
	resp, err := c.gh.Do(req, issue)
	c.trackRate(resp)
	if err != nil {
		if IsNotModified(err) {
			return nil, etag, true, nil
		}
		return nil, "", false, fmt.Errorf(
			"getting issue %s/%s#%d: %w", owner, repo, number, err,
		)
	}
	if resp != nil && resp.Response != nil {
		etag = resp.Header.Get("ETag")
	}
	return issue, etag, false, nil
}

func (c *liveClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error) {
	pr, resp, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	c.trackRate(resp)
	if err != nil {
		return nil, fmt.Errorf("getting pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return pr, nil
}

func (c *liveClient) GetPullRequestIfChanged(
	ctx context.Context,
	owner, repo string,
	number int,
	etag string,
) (*gh.PullRequest, string, bool, error) {
	u := fmt.Sprintf("repos/%v/%v/pulls/%v", owner, repo, number)
	req, err := c.gh.NewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	pr := new(gh.PullRequest)
	resp, err := c.gh.Do(req, pr)
	c.trackRate(resp)
	if err != nil {
		if IsNotModified(err) {
			return nil, etag, true, nil
		}
		return nil, "", false, fmt.Errorf("getting pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	if resp != nil && resp.Response != nil {
		etag = resp.Header.Get("ETag")
	}
	return pr, etag, false, nil
}

func (c *liveClient) GetUser(ctx context.Context, login string) (*gh.User, error) {
	user, resp, err := c.gh.Users.Get(ctx, login)
	c.trackRate(resp)
	if err != nil {
		return nil, fmt.Errorf("getting user %s: %w", login, err)
	}
	return user, nil
}

func (c *liveClient) ListIssueComments(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	return c.listIssueComments(withBypassETag(ctx), owner, repo, number)
}

func (c *liveClient) ListIssueCommentsIfChanged(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	return c.listIssueComments(ctx, owner, repo, number)
}

func (c *liveClient) listIssueComments(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	return platform.CollectPages(ctx, "", func(
		ctx context.Context, cursor string,
	) (platform.Page[*gh.IssueComment], error) {
		page, err := strconv.Atoi(cursor)
		if err != nil || page < 1 {
			page = 1
		}
		items, more, err := c.ListIssueCommentsPage(ctx, owner, repo, number, page)
		if err != nil {
			return platform.Page[*gh.IssueComment]{}, err
		}
		if !more {
			return platform.Page[*gh.IssueComment]{Items: items, Exhausted: true}, nil
		}
		return platform.Page[*gh.IssueComment]{
			Items: items, NextCursor: strconv.Itoa(page + 1),
		}, nil
	})
}

func (c *liveClient) ListReviews(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.PullRequestReview, error) {
	return platform.CollectPages(ctx, "", func(
		ctx context.Context, cursor string,
	) (platform.Page[*gh.PullRequestReview], error) {
		page, err := strconv.Atoi(cursor)
		if err != nil || page < 1 {
			page = 1
		}
		items, more, err := c.ListReviewsPage(ctx, owner, repo, number, page)
		if err != nil {
			return platform.Page[*gh.PullRequestReview]{}, err
		}
		if !more {
			return platform.Page[*gh.PullRequestReview]{Items: items, Exhausted: true}, nil
		}
		return platform.Page[*gh.PullRequestReview]{
			Items: items, NextCursor: strconv.Itoa(page + 1),
		}, nil
	})
}

func (c *liveClient) ListPullRequestReviewThreads(
	ctx context.Context,
	owner string,
	repo string,
	number int,
) ([]PullRequestReviewThread, error) {
	var threads []PullRequestReviewThread
	cursor := ""
	for {
		page, next, exhausted, err := c.ListInventoryReviewThreadsPage(
			ctx, c.platformHost, owner, repo, number, cursor,
		)
		if err != nil {
			return nil, err
		}
		for _, fragment := range page {
			if len(threads) > 0 && threads[len(threads)-1].NodeID == fragment.NodeID {
				threads[len(threads)-1].Comments = append(
					threads[len(threads)-1].Comments,
					fragment.Comments...,
				)
				continue
			}
			threads = append(threads, fragment)
		}
		if exhausted {
			return threads, nil
		}
		if next == "" || next == cursor {
			return nil, errors.New("review thread page did not advance its cursor")
		}
		cursor = next
	}
}

func githubReviewThreadCommentFromGraphQL(
	comment graphQLReviewThreadComment,
) PullRequestReviewThreadComment {
	next := PullRequestReviewThreadComment{
		NodeID:          comment.NodeID,
		DatabaseID:      firstPositiveInt64(int64(comment.FullDatabaseID), int64(comment.DatabaseID)),
		SubjectType:     comment.SubjectType,
		Body:            comment.Body,
		Path:            comment.Path,
		Line:            comment.Line,
		OriginalLine:    comment.OriginalLine,
		DiffHunk:        comment.DiffHunk,
		URL:             comment.URL,
		IsMinimized:     comment.IsMinimized,
		MinimizedReason: comment.MinimizedReason,
		CreatedAt:       comment.CreatedAt,
		UpdatedAt:       comment.UpdatedAt,
	}
	if comment.Author != nil {
		next.AuthorLogin = comment.Author.Login
	}
	if comment.Commit != nil {
		next.CommitID = comment.Commit.OID
	}
	if comment.OriginalCommit != nil {
		next.OriginalCommitID = comment.OriginalCommit.OID
	}
	if comment.PullRequestReview != nil {
		next.ReviewDatabaseID = comment.PullRequestReview.DatabaseID
	}
	return next
}

func firstPositiveInt64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func (c *liveClient) ListCommits(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.RepositoryCommit, error) {
	all, err := collectPages(ctx, func(opts *gh.ListOptions) ([]*gh.RepositoryCommit, *gh.Response, error) {
		page, resp, err := c.gh.PullRequests.ListCommits(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("listing commits for %s/%s#%d: %w", owner, repo, number, err)
		}
		return page, resp, nil
	}, c.trackRate)
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *liveClient) ListPullRequestTimelineEvents(
	ctx context.Context, owner, repo string, number int,
) ([]PullRequestTimelineEvent, error) {
	type graphQLResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				PullRequest *struct {
					TimelineItems struct {
						Nodes []struct {
							TypeName string `json:"__typename"`
							ID       string `json:"id"`
							Actor    *struct {
								Login string `json:"login"`
							} `json:"actor"`
							Assignee *struct {
								TypeName string `json:"__typename"`
								Login    string `json:"login"`
							} `json:"assignee"`
							BeforeCommit *struct {
								OID string `json:"oid"`
							} `json:"beforeCommit"`
							AfterCommit *struct {
								OID string `json:"oid"`
							} `json:"afterCommit"`
							CreatedAt            time.Time              `json:"createdAt"`
							Ref                  *struct{ Name string } `json:"ref"`
							DeletedCommentAuthor *struct {
								Login string `json:"login"`
							} `json:"deletedCommentAuthor"`
							PreviousTitle   string `json:"previousTitle"`
							CurrentTitle    string `json:"currentTitle"`
							PreviousRefName string `json:"previousRefName"`
							CurrentRefName  string `json:"currentRefName"`
							Source          *struct {
								TypeName   string `json:"__typename"`
								Number     int    `json:"number"`
								Title      string `json:"title"`
								URL        string `json:"url"`
								Repository *struct {
									Owner *struct {
										Login string `json:"login"`
									} `json:"owner"`
									Name string `json:"name"`
								} `json:"repository"`
							} `json:"source"`
							IsCrossRepository bool `json:"isCrossRepository"`
							WillCloseTarget   bool `json:"willCloseTarget"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"timelineItems"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}

	var events []PullRequestTimelineEvent
	var cursor *string
	for {
		payload, err := json.Marshal(graphQLRequest{
			Query: pullRequestTimelineEventsQuery,
			Variables: map[string]any{
				"owner":  owner,
				"repo":   repo,
				"number": number,
				"cursor": cursor,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal pull request timeline query: %w", err)
		}

		req, err := http.NewRequestWithContext(
			withQuotaResource(ctx, QuotaResourceGraphQL),
			http.MethodPost,
			c.graphQLEndpoint,
			bytes.NewReader(payload),
		)
		if err != nil {
			return nil, fmt.Errorf("create pull request timeline request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf(
				"list pull request timeline events for %s/%s#%d: %w",
				owner, repo, number, err,
			)
		}
		c.trackGraphQLRateHeaders(resp)
		if resp.StatusCode != http.StatusOK {
			responseErr := gh.CheckResponse(resp)
			_ = resp.Body.Close()
			if responseErr != nil {
				return nil, fmt.Errorf(
					"list pull request timeline events for %s/%s#%d: %w",
					owner, repo, number, responseErr,
				)
			}
			return nil, fmt.Errorf(
				"list pull request timeline events for %s/%s#%d: graphql status %s",
				owner, repo, number, resp.Status,
			)
		}

		var decoded graphQLResponse
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf(
				"decode pull request timeline events for %s/%s#%d: %w",
				owner, repo, number, err,
			)
		}
		_ = resp.Body.Close()

		if len(decoded.Errors) > 0 {
			return nil, fmt.Errorf(
				"list pull request timeline events for %s/%s#%d: graphql errors: %s",
				owner, repo, number, joinGraphQLErrorMessages(decoded.Errors),
			)
		}

		if decoded.Data.Repository == nil {
			return nil, fmt.Errorf(
				"list pull request timeline events for %s/%s#%d: missing repository in graphql response",
				owner, repo, number,
			)
		}
		if decoded.Data.Repository.PullRequest == nil {
			return nil, fmt.Errorf(
				"list pull request timeline events for %s/%s#%d: missing pull request in graphql response",
				owner, repo, number,
			)
		}

		for _, node := range decoded.Data.Repository.PullRequest.TimelineItems.Nodes {
			event := PullRequestTimelineEvent{
				NodeID:            node.ID,
				CreatedAt:         node.CreatedAt,
				PreviousTitle:     node.PreviousTitle,
				CurrentTitle:      node.CurrentTitle,
				PreviousRefName:   node.PreviousRefName,
				CurrentRefName:    node.CurrentRefName,
				IsCrossRepository: node.IsCrossRepository,
				WillCloseTarget:   node.WillCloseTarget,
			}
			switch node.TypeName {
			case "HeadRefForcePushedEvent":
				event.EventType = "force_push"
			case "CommentDeletedEvent":
				event.EventType = "comment_deleted"
			case "CrossReferencedEvent":
				event.EventType = "cross_referenced"
			case "RenamedTitleEvent":
				event.EventType = "renamed_title"
			case "BaseRefChangedEvent":
				event.EventType = "base_ref_changed"
			case "AssignedEvent":
				event.EventType = "assigned"
			case "UnassignedEvent":
				event.EventType = "unassigned"
			case "MergedEvent":
				event.EventType = "merged"
			case "ClosedEvent":
				event.EventType = "closed"
			case "ReopenedEvent":
				event.EventType = "reopened"
			default:
				continue
			}
			if node.Actor != nil {
				event.Actor = node.Actor.Login
			}
			if node.Assignee != nil {
				event.Assignee = node.Assignee.Login
			}
			if node.BeforeCommit != nil {
				event.BeforeSHA = node.BeforeCommit.OID
			}
			if node.AfterCommit != nil {
				event.AfterSHA = node.AfterCommit.OID
			}
			if node.Ref != nil {
				event.Ref = node.Ref.Name
			}
			if node.DeletedCommentAuthor != nil {
				event.DeletedCommentAuthor = node.DeletedCommentAuthor.Login
			}
			if node.Source != nil {
				event.SourceType = node.Source.TypeName
				event.SourceNumber = node.Source.Number
				event.SourceTitle = node.Source.Title
				event.SourceURL = node.Source.URL
				if node.Source.Repository != nil {
					event.SourceRepo = node.Source.Repository.Name
					if node.Source.Repository.Owner != nil {
						event.SourceOwner = node.Source.Repository.Owner.Login
					}
				}
			}
			events = append(events, event)
		}

		pageInfo := decoded.Data.Repository.PullRequest.TimelineItems.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		cursor = pageInfo.EndCursor
	}

	return events, nil
}

func (c *liveClient) ListIssueTimelineEvents(
	ctx context.Context, owner, repo string, number int,
) ([]PullRequestTimelineEvent, error) {
	type graphQLResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				Issue *struct {
					TimelineItems struct {
						Nodes []struct {
							TypeName string `json:"__typename"`
							ID       string `json:"id"`
							Actor    *struct {
								Login string `json:"login"`
							} `json:"actor"`
							Assignee *struct {
								TypeName string `json:"__typename"`
								Login    string `json:"login"`
							} `json:"assignee"`
							CreatedAt time.Time `json:"createdAt"`
							Source    *struct {
								TypeName   string `json:"__typename"`
								Number     int    `json:"number"`
								Title      string `json:"title"`
								URL        string `json:"url"`
								Repository *struct {
									Owner *struct {
										Login string `json:"login"`
									} `json:"owner"`
									Name string `json:"name"`
								} `json:"repository"`
							} `json:"source"`
							IsCrossRepository bool `json:"isCrossRepository"`
							WillCloseTarget   bool `json:"willCloseTarget"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"timelineItems"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}

	var events []PullRequestTimelineEvent
	var cursor *string
	for {
		payload, err := json.Marshal(graphQLRequest{
			Query: issueTimelineEventsQuery,
			Variables: map[string]any{
				"owner":  owner,
				"repo":   repo,
				"number": number,
				"cursor": cursor,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal issue timeline query: %w", err)
		}

		req, err := http.NewRequestWithContext(
			withQuotaResource(ctx, QuotaResourceGraphQL),
			http.MethodPost,
			c.graphQLEndpoint,
			bytes.NewReader(payload),
		)
		if err != nil {
			return nil, fmt.Errorf("create issue timeline request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf(
				"list issue timeline events for %s/%s#%d: %w",
				owner, repo, number, err,
			)
		}
		c.trackGraphQLRateHeaders(resp)
		if resp.StatusCode != http.StatusOK {
			responseErr := gh.CheckResponse(resp)
			_ = resp.Body.Close()
			if responseErr != nil {
				return nil, fmt.Errorf(
					"list issue timeline events for %s/%s#%d: %w",
					owner, repo, number, responseErr,
				)
			}
			return nil, fmt.Errorf(
				"list issue timeline events for %s/%s#%d: graphql status %s",
				owner, repo, number, resp.Status,
			)
		}

		var decoded graphQLResponse
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf(
				"decode issue timeline events for %s/%s#%d: %w",
				owner, repo, number, err,
			)
		}
		_ = resp.Body.Close()

		if len(decoded.Errors) > 0 {
			return nil, fmt.Errorf(
				"list issue timeline events for %s/%s#%d: graphql errors: %s",
				owner, repo, number, joinGraphQLErrorMessages(decoded.Errors),
			)
		}
		if decoded.Data.Repository == nil {
			return nil, fmt.Errorf(
				"list issue timeline events for %s/%s#%d: missing repository in graphql response",
				owner, repo, number,
			)
		}
		if decoded.Data.Repository.Issue == nil {
			return nil, fmt.Errorf(
				"list issue timeline events for %s/%s#%d: missing issue in graphql response",
				owner, repo, number,
			)
		}

		for _, node := range decoded.Data.Repository.Issue.TimelineItems.Nodes {
			event := PullRequestTimelineEvent{
				NodeID:    node.ID,
				CreatedAt: node.CreatedAt,
			}
			switch node.TypeName {
			case "AssignedEvent":
				event.EventType = "assigned"
			case "UnassignedEvent":
				event.EventType = "unassigned"
			case "CrossReferencedEvent":
				event.EventType = "cross_referenced"
			case "ClosedEvent":
				event.EventType = "closed"
			case "ReopenedEvent":
				event.EventType = "reopened"
			default:
				continue
			}
			if node.Actor != nil {
				event.Actor = node.Actor.Login
			}
			if node.Assignee != nil {
				event.Assignee = node.Assignee.Login
			}
			if node.Source != nil {
				event.SourceType = node.Source.TypeName
				event.SourceNumber = node.Source.Number
				event.SourceTitle = node.Source.Title
				event.SourceURL = node.Source.URL
				if node.Source.Repository != nil {
					event.SourceRepo = node.Source.Repository.Name
					if node.Source.Repository.Owner != nil {
						event.SourceOwner = node.Source.Repository.Owner.Login
					}
				}
			}
			event.IsCrossRepository = node.IsCrossRepository
			event.WillCloseTarget = node.WillCloseTarget
			events = append(events, event)
		}

		pageInfo := decoded.Data.Repository.Issue.TimelineItems.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		cursor = pageInfo.EndCursor
	}

	return events, nil
}

func (c *liveClient) ListForcePushEvents(
	ctx context.Context, owner, repo string, number int,
) ([]ForcePushEvent, error) {
	timelineEvents, err := c.ListPullRequestTimelineEvents(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}

	events := make([]ForcePushEvent, 0, len(timelineEvents))
	for _, timelineEvent := range timelineEvents {
		if timelineEvent.EventType != "force_push" {
			continue
		}
		events = append(events, ForcePushEvent{
			Actor:     timelineEvent.Actor,
			BeforeSHA: timelineEvent.BeforeSHA,
			AfterSHA:  timelineEvent.AfterSHA,
			Ref:       timelineEvent.Ref,
			CreatedAt: timelineEvent.CreatedAt,
		})
	}
	return events, nil
}

func (c *liveClient) GetCombinedStatus(
	ctx context.Context, owner, repo, ref string,
) (*gh.CombinedStatus, error) {
	status, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, ref, nil)
	c.trackRate(resp)
	if err != nil {
		return nil, fmt.Errorf("getting combined status for %s/%s@%s: %w", owner, repo, ref, err)
	}
	return status, nil
}

func (c *liveClient) ListCheckRunsForRef(
	ctx context.Context, owner, repo, ref string,
) ([]*gh.CheckRun, error) {
	opts := &gh.ListCheckRunsOptions{
		PerPage: 100,
	}
	all, err := collectPages(ctx, func(pageOpts *gh.ListOptions) ([]*gh.CheckRun, *gh.Response, error) {
		opts.ListOptions = *pageOpts
		result, resp, err := c.gh.Checks.ListCheckRunsForRef(
			ctx, owner, repo, ref, opts,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"listing check runs for %s/%s@%s: %w",
				owner, repo, ref, err,
			)
		}
		return result.CheckRuns, resp, nil
	}, c.trackRate)
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *liveClient) ListWorkflowRunsForHeadSHA(
	ctx context.Context, owner, repo, headSHA string,
) ([]*gh.WorkflowRun, error) {
	opts := &gh.ListWorkflowRunsOptions{
		HeadSHA: headSHA,
		Status:  "action_required",
		PerPage: 100,
	}
	all, err := collectPages(ctx, func(pageOpts *gh.ListOptions) ([]*gh.WorkflowRun, *gh.Response, error) {
		opts.ListOptions = *pageOpts
		result, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(
			ctx, owner, repo, opts,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"listing workflow runs for %s/%s@%s: %w",
				owner, repo, headSHA, err,
			)
		}
		return result.WorkflowRuns, resp, nil
	}, c.trackRate)
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *liveClient) ApproveWorkflowRun(
	ctx context.Context, owner, repo string, runID int64,
) error {
	req, err := c.writeGH().NewRequest(
		ctx,
		"POST",
		fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, runID),
		nil,
	)
	if err != nil {
		return fmt.Errorf(
			"building workflow approval request for %s/%s run %d: %w",
			owner, repo, runID, err,
		)
	}

	resp, err := c.writeGH().Do(req, nil)
	c.trackWriteRate(resp)
	if err != nil {
		return fmt.Errorf(
			"approving workflow run %s/%s#%d: %w",
			owner, repo, runID, err,
		)
	}
	return nil
}

func (c *liveClient) CreateIssueComment(
	ctx context.Context, owner, repo string, number int, body string,
) (*gh.IssueComment, error) {
	comment, resp, err := c.writeGH().Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{
		Body: new(body),
	})
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf("creating comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return comment, nil
}

func (c *liveClient) EditIssueComment(
	ctx context.Context, owner, repo string, commentID int64, body string,
) (*gh.IssueComment, error) {
	comment, resp, err := c.writeGH().Issues.EditComment(
		ctx, owner, repo, commentID, &gh.IssueComment{Body: new(body)},
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"editing comment %d on %s/%s: %w", commentID, owner, repo, err,
		)
	}
	return comment, nil
}

func (c *liveClient) DeleteIssueComment(
	ctx context.Context, owner, repo string, commentID int64,
) error {
	resp, err := c.writeGH().Issues.DeleteComment(ctx, owner, repo, commentID)
	c.trackWriteRate(resp)
	if err != nil {
		var responseErr *gh.ErrorResponse
		if errors.As(err, &responseErr) && responseErr.Response != nil && responseErr.Response.StatusCode == http.StatusNotFound {
			return &platform.Error{
				Code:         platform.ErrCodeNotFound,
				Provider:     platform.KindGitHub,
				PlatformHost: c.platformHost,
				Err:          err,
			}
		}
		return fmt.Errorf("deleting comment %d on %s/%s: %w", commentID, owner, repo, err)
	}
	return nil
}

func (c *liveClient) CreatePullRequestReviewCommentReply(
	ctx context.Context, owner, repo string, number int, body string, commentID int64,
) (*gh.PullRequestComment, error) {
	comment, resp, err := c.writeGH().PullRequests.CreateCommentInReplyTo(
		ctx, owner, repo, number, body, commentID,
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"replying to review comment %d on %s/%s#%d: %w",
			commentID, owner, repo, number, err,
		)
	}
	return comment, nil
}

// GetRepository reads repository metadata with the sync credential so
// app-token-only hosts keep working, then, on split-credential hosts,
// overlays the viewer-specific permissions block from the user's
// credential: it feeds viewer_can_merge, and the app installation
// token would report the read-only app's permissions instead of the
// user's. A failed overlay (missing or exhausted PAT) clears the
// permissions to "unknown" rather than keeping the app's, and never
// fails the metadata read sync depends on.
func (c *liveClient) GetRepository(
	ctx context.Context, owner, repo string,
) (*gh.Repository, error) {
	r, resp, err := c.gh.Repositories.Get(ctx, owner, repo)
	c.trackRate(resp)
	if err != nil {
		return nil, fmt.Errorf("getting repository %s/%s: %w", owner, repo, err)
	}
	if !c.splitAuthActive() {
		return r, nil
	}
	if IsArchiveSyncBudgetContext(ctx) {
		// Archive requests hold admission and provider-work leases for the
		// read identity only, so they must not spend the write credential on
		// the viewer overlay. Archive classification does not need viewer
		// permissions; clear the app's instead of persisting the wrong
		// viewer's.
		r.Permissions = nil
		return r, nil
	}
	viewerRepo, viewerResp, viewerErr := c.writeGH().Repositories.Get(ctx, owner, repo)
	c.trackWriteRate(viewerResp)
	if viewerErr != nil {
		slog.Warn(
			"viewer permission refresh failed; merge permission unknown until it succeeds",
			"repo", owner+"/"+repo, "err", viewerErr,
		)
		r.Permissions = nil
		return r, nil
	}
	r.Permissions = viewerRepo.Permissions
	if gitHubMergeSettingsComplete(viewerRepo) {
		r.AllowSquashMerge = viewerRepo.AllowSquashMerge
		r.AllowMergeCommit = viewerRepo.AllowMergeCommit
		r.AllowRebaseMerge = viewerRepo.AllowRebaseMerge
	}
	return r, nil
}

func gitHubMergeSettingsComplete(repo *gh.Repository) bool {
	return repo != nil &&
		repo.AllowSquashMerge != nil &&
		repo.AllowMergeCommit != nil &&
		repo.AllowRebaseMerge != nil
}

func (c *liveClient) CreateReview(
	ctx context.Context, owner, repo string, number int,
	event string, body string,
) (*gh.PullRequestReview, error) {
	return c.CreateReviewWithComments(ctx, owner, repo, number, event, body, "", nil)
}

func (c *liveClient) CreateReviewWithComments(
	ctx context.Context,
	owner, repo string,
	number int,
	event string,
	body string,
	commitID string,
	comments []*gh.DraftReviewComment,
) (*gh.PullRequestReview, error) {
	request := &gh.PullRequestReviewRequest{
		Event:    new(event),
		Body:     new(body),
		Comments: comments,
	}
	if commitID != "" {
		request.CommitID = &commitID
	}
	review, resp, err := c.writeGH().PullRequests.CreateReview(
		ctx, owner, repo, number, request,
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"creating review on %s/%s#%d: %w", owner, repo, number, err,
		)
	}
	return review, nil
}

func (c *liveClient) ApplyReviewSuggestions(
	ctx context.Context,
	owner, repo string,
	number int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	headOwner, headRepo, headFullName, err := githubSuggestionHeadRepo(owner, repo, input)
	if err != nil {
		return nil, err
	}
	headBranch := strings.TrimSpace(input.HeadBranch)
	expectedHeadSHA := strings.TrimSpace(input.ExpectedHeadSHA)
	if headBranch == "" {
		return nil, githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, "head branch is required", nil)
	}
	if expectedHeadSHA == "" {
		return nil, githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, "expected head SHA is required", nil)
	}

	byPath := make(map[string][]platform.ReviewSuggestion)
	for _, suggestion := range input.Suggestions {
		path := suggestion.Range.Path
		trimmedPath := strings.TrimSpace(path)
		if trimmedPath == "" {
			return nil, githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, "suggestion path is required", nil)
		}
		if path != trimmedPath {
			return nil, githubSuggestionPlatformError(
				platform.ErrCodeInvalidArgument,
				"suggestion path has leading or trailing whitespace",
				nil,
			)
		}
		byPath[path] = append(byPath[path], suggestion)
	}
	if len(byPath) == 0 {
		return nil, githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, "at least one suggestion is required", nil)
	}
	if err := c.ensureReviewSuggestionPullMutable(ctx, owner, repo, number, headFullName, headBranch, expectedHeadSHA); err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	additions := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		content, err := c.githubFileContentAtRef(ctx, headOwner, headRepo, path, expectedHeadSHA)
		if err != nil {
			return nil, err
		}
		nextContent, err := applyReviewSuggestionEdits(content, byPath[path])
		if err != nil {
			return nil, githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, err.Error(), err)
		}
		additions = append(additions, map[string]string{
			"path":     path,
			"contents": base64.StdEncoding.EncodeToString([]byte(nextContent)),
		})
	}

	return c.createCommitForReviewSuggestions(
		ctx,
		owner,
		repo,
		number,
		headFullName,
		headBranch,
		expectedHeadSHA,
		strings.TrimSpace(input.Message),
		additions,
	)
}

func (c *liveClient) ensureReviewSuggestionPullMutable(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	headFullName string,
	headBranch string,
	expectedHeadSHA string,
) error {
	pr, err := c.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	if !strings.EqualFold(pr.GetState(), "open") {
		return c.githubSuggestionConflict("not_open", "pull request is not open")
	}
	liveHeadFullName := githubSuggestionLiveHeadRepoFullName(pr)
	if liveHeadFullName == "" || !strings.EqualFold(liveHeadFullName, headFullName) {
		return c.githubSuggestionConflict("head_repo_unknown", "pull request head repository is unknown")
	}
	if pr.Head == nil {
		return c.githubSuggestionStaleState("pull request head changed since it was reviewed")
	}
	liveHeadBranch := strings.TrimSpace(pr.Head.GetRef())
	if liveHeadBranch == "" || liveHeadBranch != headBranch {
		return c.githubSuggestionStaleState("pull request head branch changed since it was reviewed")
	}
	liveHeadSHA := strings.TrimSpace(pr.Head.GetSHA())
	if liveHeadSHA == "" || !strings.EqualFold(liveHeadSHA, expectedHeadSHA) {
		return c.githubSuggestionStaleState("pull request head changed since it was reviewed")
	}
	headOwner, headRepo, ok := strings.Cut(liveHeadFullName, "/")
	if !ok || headOwner == "" || headRepo == "" {
		return c.githubSuggestionConflict("head_repo_unknown", "pull request head repository is unknown")
	}
	if err := c.ensureReviewSuggestionHeadRepoReachable(ctx, headOwner, headRepo); err != nil {
		return err
	}
	return nil
}

func (c *liveClient) ensureReviewSuggestionHeadRepoReachable(
	ctx context.Context,
	owner string,
	repo string,
) error {
	_, resp, err := c.writeGH().Repositories.Get(ctx, owner, repo)
	c.trackWriteRate(resp)
	if err != nil {
		if githubSuggestionHeadRepoUnavailable(err) {
			return c.githubSuggestionConflict("head_repo_unknown", "pull request head repository is unknown")
		}
		return err
	}
	return nil
}

func (c *liveClient) githubSuggestionConflict(reason string, message string) error {
	return &platform.Error{
		Code:         platform.ErrCodeConflict,
		Provider:     platform.KindGitHub,
		PlatformHost: c.platformHost,
		Details:      map[string]string{"reason": reason},
		Err:          fmt.Errorf("%s", message),
	}
}

func (c *liveClient) githubSuggestionStaleState(message string) error {
	return &platform.Error{
		Code:         platform.ErrCodeStaleState,
		Provider:     platform.KindGitHub,
		PlatformHost: c.platformHost,
		Err:          fmt.Errorf("%s", message),
	}
}

func githubSuggestionLiveHeadRepoFullName(pr *gh.PullRequest) string {
	if pr == nil || pr.Head == nil || pr.Head.Repo == nil {
		return ""
	}
	if fullName := strings.TrimSpace(pr.Head.Repo.GetFullName()); fullName != "" {
		return fullName
	}
	if fullName := ParseHeadRepoFullName(pr.Head.Repo.GetCloneURL()); fullName != "" {
		return fullName
	}
	owner := strings.TrimSpace(pr.Head.Repo.GetOwner().GetLogin())
	name := strings.TrimSpace(pr.Head.Repo.GetName())
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func githubSuggestionHeadRepoUnavailable(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	switch ghErr.Response.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return true
	default:
		return false
	}
}

func (c *liveClient) githubFileContentAtRef(
	ctx context.Context,
	owner string,
	repo string,
	path string,
	ref string,
) (string, error) {
	file, directory, resp, err := c.writeGH().Repositories.GetContents(
		ctx,
		owner,
		repo,
		path,
		&gh.RepositoryContentGetOptions{Ref: ref},
	)
	c.trackWriteRate(resp)
	if err != nil {
		return "", fmt.Errorf("getting %s/%s:%s at %s: %w", owner, repo, path, ref, err)
	}
	if directory != nil || file == nil {
		return "", githubSuggestionPlatformError(platform.ErrCodeInvalidArgument, "suggestion target is not a file", nil)
	}
	content, err := file.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s/%s:%s at %s: %w", owner, repo, path, ref, err)
	}
	return content, nil
}

const createCommitOnBranchMutation = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) {
    commit {
      oid
      url
    }
  }
}`

func (c *liveClient) createCommitForReviewSuggestions(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	headFullName string,
	headBranch string,
	expectedHeadSHA string,
	message string,
	additions []map[string]string,
) (*platform.AppliedReviewSuggestions, error) {
	if err := c.ensureReviewSuggestionPullMutable(ctx, owner, repo, number, headFullName, headBranch, expectedHeadSHA); err != nil {
		return nil, err
	}

	type createCommitResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			CreateCommitOnBranch *struct {
				Commit *struct {
					OID string `json:"oid"`
					URL string `json:"url"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
	}
	if message == "" {
		message = "Apply review suggestion"
		if len(additions) > 1 {
			message = "Apply review suggestions"
		}
	}
	payload, err := json.Marshal(graphQLRequest{
		Query: createCommitOnBranchMutation,
		Variables: map[string]any{
			"input": map[string]any{
				"branch": map[string]string{
					"repositoryNameWithOwner": headFullName,
					"branchName":              headBranch,
				},
				"expectedHeadOid": expectedHeadSHA,
				"message": map[string]string{
					"headline": message,
				},
				"fileChanges": map[string]any{
					"additions": additions,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal review suggestion commit mutation: %w", err)
	}
	req, err := http.NewRequestWithContext(
		withQuotaResource(ctx, QuotaResourceGraphQL),
		http.MethodPost,
		c.graphQLEndpoint,
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("create review suggestion commit request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.writeHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("applying review suggestions on %s/%s#%d: %w", owner, repo, number, err)
	}
	c.trackWriteGraphQLRateHeaders(resp)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("applying review suggestions on %s/%s#%d: graphql status %s", owner, repo, number, resp.Status)
	}
	var decoded createCommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode review suggestion commit response for %s/%s#%d: %w", owner, repo, number, err)
	}
	if len(decoded.Errors) > 0 {
		return nil, githubCreateCommitGraphQLError(owner, repo, number, decoded.Errors)
	}
	if decoded.Data.CreateCommitOnBranch == nil || decoded.Data.CreateCommitOnBranch.Commit == nil {
		return nil, fmt.Errorf("applying review suggestions on %s/%s#%d: missing commit in graphql response", owner, repo, number)
	}
	commit := decoded.Data.CreateCommitOnBranch.Commit
	return &platform.AppliedReviewSuggestions{
		CommitSHA: commit.OID,
		CommitURL: commit.URL,
	}, nil
}

func githubSuggestionHeadRepo(
	owner string,
	repo string,
	input platform.ApplyReviewSuggestionsInput,
) (string, string, string, error) {
	fullName := ParseHeadRepoFullName(input.HeadRepoCloneURL)
	if fullName == "" {
		return "", "", "", githubSuggestionPlatformError(
			platform.ErrCodeInvalidArgument,
			"head repository is required",
			nil,
		)
	}
	headOwner, headRepo, ok := strings.Cut(fullName, "/")
	if !ok || headOwner == "" || headRepo == "" || strings.Contains(headRepo, "/") {
		return "", "", "", githubSuggestionPlatformError(
			platform.ErrCodeInvalidArgument,
			"head repository is required",
			nil,
		)
	}
	return headOwner, headRepo, fullName, nil
}

func githubCreateCommitGraphQLError(
	owner string,
	repo string,
	number int,
	graphQLErrors []graphQLError,
) error {
	message := joinGraphQLErrorMessages(graphQLErrors)
	wrapped := fmt.Errorf(
		"applying review suggestions on %s/%s#%d: graphql errors: %s",
		owner, repo, number, message,
	)
	if githubCreateCommitStaleError(graphQLErrors) {
		return &platform.Error{
			Code:     platform.ErrCodeStaleState,
			Provider: platform.KindGitHub,
			Err:      wrapped,
		}
	}
	for _, graphQLError := range graphQLErrors {
		if strings.EqualFold(graphQLError.Type, "NOT_FOUND") ||
			strings.Contains(graphQLError.Message, "Could not resolve") {
			// Post-preflight resolution failures are head repo/branch
			// races, not user-addressable 404s: fail closed with the
			// same stable reason as the preflight so clients run
			// conflict recovery.
			return &platform.Error{
				Code:     platform.ErrCodeConflict,
				Provider: platform.KindGitHub,
				Details:  map[string]string{"reason": "head_repo_unknown"},
				Err:      wrapped,
			}
		}
	}
	return wrapped
}

func githubCreateCommitStaleError(graphQLErrors []graphQLError) bool {
	for _, graphQLError := range graphQLErrors {
		message := strings.ToLower(graphQLError.Message)
		if strings.Contains(message, "expectedheadoid") ||
			strings.Contains(message, "expected head") ||
			strings.Contains(message, "stale") ||
			(strings.Contains(message, "head") &&
				strings.Contains(message, "expected") &&
				strings.Contains(message, "match")) {
			return true
		}
	}
	return false
}

func githubSuggestionPlatformError(
	code platform.PlatformErrorCode,
	message string,
	err error,
) error {
	if err == nil {
		err = errors.New(message)
	}
	return &platform.Error{
		Code:     code,
		Provider: platform.KindGitHub,
		Err:      err,
	}
}

type reviewSuggestionEdit struct {
	start       int
	end         int
	replacement []string
}

func applyReviewSuggestionEdits(
	content string,
	suggestions []platform.ReviewSuggestion,
) (string, error) {
	lines, newline, trailingNewline := splitGitHubSuggestionContent(content)
	if lines == nil {
		lines = []string{}
	}
	edits := make([]reviewSuggestionEdit, 0, len(suggestions))
	for _, suggestion := range suggestions {
		if strings.ToLower(strings.TrimSpace(suggestion.Range.Side)) != "right" {
			return "", fmt.Errorf("suggestion %q is not on the right side", suggestion.Range.Path)
		}
		start := suggestion.Range.Line
		if suggestion.Range.StartLine != nil {
			start = *suggestion.Range.StartLine
		}
		end := suggestion.Range.Line
		if start <= 0 || end <= 0 || start > end {
			return "", fmt.Errorf("suggestion %q has invalid line range", suggestion.Range.Path)
		}
		if end > len(lines) {
			return "", fmt.Errorf("suggestion %q range exceeds file length", suggestion.Range.Path)
		}
		edits = append(edits, reviewSuggestionEdit{
			start:       start,
			end:         end,
			replacement: githubSuggestionReplacementLines(suggestion.Replacement),
		})
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start < edits[j].start
	})
	for i := 1; i < len(edits); i += 1 {
		if edits[i].start <= edits[i-1].end {
			return "", fmt.Errorf("suggestions contain overlapping line ranges")
		}
	}
	for i := len(edits) - 1; i >= 0; i -= 1 {
		edit := edits[i]
		prefix := append([]string{}, lines[:edit.start-1]...)
		next := append(prefix, edit.replacement...)
		next = append(next, lines[edit.end:]...)
		lines = next
	}
	return joinGitHubSuggestionContent(lines, newline, trailingNewline), nil
}

func splitGitHubSuggestionContent(content string) ([]string, string, bool) {
	newline := "\n"
	if strings.Contains(content, "\r\n") {
		newline = "\r\n"
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	trailingNewline := strings.HasSuffix(content, "\n")
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return []string{}, newline, trailingNewline
	}
	return strings.Split(content, "\n"), newline, trailingNewline
}

func joinGitHubSuggestionContent(lines []string, newline string, trailingNewline bool) string {
	content := strings.Join(lines, newline)
	if trailingNewline && len(lines) > 0 {
		content += newline
	}
	return content
}

func githubSuggestionReplacementLines(replacement string) []string {
	replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	if replacement == "" {
		return nil
	}
	return strings.Split(replacement, "\n")
}

func (c *liveClient) DismissReview(
	ctx context.Context, owner, repo string, number int, reviewID int64, message string,
) (*gh.PullRequestReview, error) {
	review, resp, err := c.writeGH().PullRequests.DismissReview(
		ctx, owner, repo, number, reviewID,
		&gh.PullRequestReviewDismissalRequest{Message: &message},
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"dismissing review %d on %s/%s#%d: %w", reviewID, owner, repo, number, err,
		)
	}
	return review, nil
}

func (c *liveClient) MarkPullRequestReadyForReview(
	ctx context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	type readyForReviewIDResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				PullRequest *struct {
					ID string `json:"id"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	type readyForReviewMutationResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			MarkPullRequestReadyForReview *struct {
				PullRequest *gqlPR `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
	}

	postGraphQL := func(payload any, dest any) (*http.Response, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(
			withQuotaResource(ctx, QuotaResourceGraphQL),
			http.MethodPost,
			c.graphQLEndpoint,
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.writeHTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		c.trackWriteGraphQLRateHeaders(resp)
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return resp, newReadyForReviewError(
				fmt.Errorf("graphql status %s", resp.Status),
				resp.StatusCode,
				resp.StatusCode == http.StatusNotFound,
			)
		}
		if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
			_ = resp.Body.Close()
			return resp, err
		}
		_ = resp.Body.Close()
		return resp, nil
	}

	idPayload := graphQLRequest{
		Query: readyForReviewIDQuery,
		Variables: map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	}
	var idResult readyForReviewIDResponse
	if _, err := postGraphQL(idPayload, &idResult); err != nil {
		return nil, fmt.Errorf(
			"marking %s/%s#%d ready for review: resolve pull request id: %w",
			owner, repo, number, err,
		)
	}
	if len(idResult.Errors) > 0 {
		statusCode, staleState := readyForReviewGraphQLErrorMeta(idResult.Errors)
		return nil, newReadyForReviewError(fmt.Errorf(
			"marking %s/%s#%d ready for review: resolve pull request id: graphql errors: %s",
			owner, repo, number, joinGraphQLErrorMessages(idResult.Errors),
		), statusCode, staleState)
	}
	if idResult.Data.Repository == nil || idResult.Data.Repository.PullRequest == nil || idResult.Data.Repository.PullRequest.ID == "" {
		return nil, newReadyForReviewError(
			fmt.Errorf(
				"marking %s/%s#%d ready for review: resolve pull request id: missing pull request in graphql response",
				owner, repo, number,
			),
			http.StatusNotFound,
			true,
		)
	}

	mutationPayload := graphQLRequest{
		Query: readyForReviewMutation,
		Variables: map[string]any{
			"pullRequestId": idResult.Data.Repository.PullRequest.ID,
		},
	}
	var mutationResult readyForReviewMutationResponse
	if _, err := postGraphQL(mutationPayload, &mutationResult); err != nil {
		return nil, fmt.Errorf(
			"marking %s/%s#%d ready for review: %w",
			owner, repo, number, err,
		)
	}
	if len(mutationResult.Errors) > 0 {
		statusCode, staleState := readyForReviewGraphQLErrorMeta(mutationResult.Errors)
		return nil, newReadyForReviewError(fmt.Errorf(
			"marking %s/%s#%d ready for review: graphql errors: %s",
			owner, repo, number, joinGraphQLErrorMessages(mutationResult.Errors),
		), statusCode, staleState)
	}
	if mutationResult.Data.MarkPullRequestReadyForReview == nil || mutationResult.Data.MarkPullRequestReadyForReview.PullRequest == nil {
		return nil, newReadyForReviewError(
			fmt.Errorf(
				"marking %s/%s#%d ready for review: missing pull request in graphql response",
				owner, repo, number,
			),
			0,
			false,
		)
	}

	return adaptPR(mutationResult.Data.MarkPullRequestReadyForReview.PullRequest), nil
}

func (c *liveClient) ConvertPullRequestToDraft(
	ctx context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	type draftIDResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				PullRequest *struct {
					ID string `json:"id"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	type draftMutationResponse struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			ConvertPullRequestToDraft *struct {
				PullRequest *struct {
					ID        string    `json:"id"`
					UpdatedAt time.Time `json:"updatedAt"`
				} `json:"pullRequest"`
			} `json:"convertPullRequestToDraft"`
		} `json:"data"`
	}

	postGraphQL := func(payload any, dest any) (*http.Response, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(
			withQuotaResource(ctx, QuotaResourceGraphQL),
			http.MethodPost,
			c.graphQLEndpoint,
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.writeHTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		c.trackWriteGraphQLRateHeaders(resp)
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return resp, newReadyForReviewError(
				fmt.Errorf("graphql status %s", resp.Status),
				resp.StatusCode,
				resp.StatusCode == http.StatusNotFound,
			)
		}
		if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
			_ = resp.Body.Close()
			return resp, err
		}
		_ = resp.Body.Close()
		return resp, nil
	}

	idPayload := graphQLRequest{
		Query: readyForReviewIDQuery,
		Variables: map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	}
	var idResult draftIDResponse
	if _, err := postGraphQL(idPayload, &idResult); err != nil {
		return nil, fmt.Errorf(
			"converting %s/%s#%d to draft: resolve pull request id: %w",
			owner, repo, number, err,
		)
	}
	if len(idResult.Errors) > 0 {
		statusCode, staleState := readyForReviewGraphQLErrorMeta(idResult.Errors)
		return nil, newReadyForReviewError(fmt.Errorf(
			"converting %s/%s#%d to draft: resolve pull request id: graphql errors: %s",
			owner, repo, number, joinGraphQLErrorMessages(idResult.Errors),
		), statusCode, staleState)
	}
	if idResult.Data.Repository == nil || idResult.Data.Repository.PullRequest == nil || idResult.Data.Repository.PullRequest.ID == "" {
		return nil, newReadyForReviewError(
			fmt.Errorf(
				"converting %s/%s#%d to draft: resolve pull request id: missing pull request in graphql response",
				owner, repo, number,
			),
			http.StatusNotFound,
			true,
		)
	}

	mutationPayload := graphQLRequest{
		Query: convertToDraftMutation,
		Variables: map[string]any{
			"pullRequestId": idResult.Data.Repository.PullRequest.ID,
		},
	}
	var mutationResult draftMutationResponse
	if _, err := postGraphQL(mutationPayload, &mutationResult); err != nil {
		return nil, fmt.Errorf(
			"converting %s/%s#%d to draft: %w",
			owner, repo, number, err,
		)
	}
	if len(mutationResult.Errors) > 0 {
		statusCode, staleState := readyForReviewGraphQLErrorMeta(mutationResult.Errors)
		return nil, newReadyForReviewError(fmt.Errorf(
			"converting %s/%s#%d to draft: graphql errors: %s",
			owner, repo, number, joinGraphQLErrorMessages(mutationResult.Errors),
		), statusCode, staleState)
	}
	if mutationResult.Data.ConvertPullRequestToDraft == nil || mutationResult.Data.ConvertPullRequestToDraft.PullRequest == nil {
		return nil, newReadyForReviewError(
			fmt.Errorf(
				"converting %s/%s#%d to draft: missing pull request in graphql response",
				owner, repo, number,
			),
			0,
			false,
		)
	}

	draft := true
	state := "open"
	mutationPR := mutationResult.Data.ConvertPullRequestToDraft.PullRequest
	nodeID := mutationPR.ID
	updatedAt := gh.Timestamp{Time: mutationPR.UpdatedAt}
	return &gh.PullRequest{
		NodeID:    &nodeID,
		Number:    &number,
		State:     &state,
		Draft:     &draft,
		UpdatedAt: &updatedAt,
	}, nil
}

func (c *liveClient) EditPullRequest(
	ctx context.Context, owner, repo string, number int, opts EditPullRequestOpts,
) (*gh.PullRequest, error) {
	edit := &gh.PullRequest{}
	if opts.State != nil {
		edit.State = opts.State
	}
	if opts.Title != nil {
		edit.Title = opts.Title
	}
	if opts.Body != nil {
		edit.Body = opts.Body
	}
	pr, resp, err := c.writeGH().PullRequests.Edit(
		ctx, owner, repo, number, edit,
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"editing pull request %s/%s#%d: %w",
			owner, repo, number, err,
		)
	}
	return pr, nil
}

func (c *liveClient) EditIssue(
	ctx context.Context, owner, repo string, number int, state string,
) (*gh.Issue, error) {
	issue, resp, err := c.writeGH().Issues.Edit(
		ctx, owner, repo, number, &gh.IssueRequest{State: &state},
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"editing issue %s/%s#%d: %w",
			owner, repo, number, err,
		)
	}
	return issue, nil
}

func (c *liveClient) EditIssueContent(
	ctx context.Context, owner, repo string, number int, title *string, body *string,
) (*gh.Issue, error) {
	req := &gh.IssueRequest{}
	if title != nil {
		req.Title = title
	}
	if body != nil {
		req.Body = body
	}
	issue, resp, err := c.writeGH().Issues.Edit(ctx, owner, repo, number, req)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"editing issue %s/%s#%d: %w",
			owner, repo, number, err,
		)
	}
	return issue, nil
}

func (c *liveClient) ListPullRequestsPage(
	ctx context.Context, owner, repo, state string, page int,
) ([]*gh.PullRequest, bool, error) {
	opts := &gh.PullRequestListOptions{
		State:     state,
		Sort:      "updated",
		Direction: "desc",
		Page:      page,
		PerPage:   100,
	}
	prs, resp, err := c.gh.PullRequests.List(
		ctx, owner, repo, opts,
	)
	c.trackRate(resp)
	if err != nil {
		return nil, false, fmt.Errorf(
			"list %s PRs page %d for %s/%s: %w",
			state, page, owner, repo, err,
		)
	}
	hasMore := resp != nil && resp.NextPage > 0
	return prs, hasMore, nil
}

func (c *liveClient) ListIssuesPage(
	ctx context.Context, owner, repo, state string, page int,
) ([]*gh.Issue, bool, error) {
	opts := &gh.IssueListByRepoOptions{
		State:     state,
		Sort:      "updated",
		Direction: "desc",
		ListOptions: gh.ListOptions{
			Page:    page,
			PerPage: 100,
		},
	}
	issues, resp, err := c.gh.Issues.ListByRepo(
		ctx, owner, repo, opts,
	)
	c.trackRate(resp)
	if err != nil {
		return nil, false, fmt.Errorf(
			"list %s issues page %d for %s/%s: %w",
			state, page, owner, repo, err,
		)
	}
	// Filter out PRs (GitHub Issues API returns them). Nil entries pass
	// through so the canonical normalization rejects them typed instead of
	// the filter panicking on the dereference.
	var filtered []*gh.Issue
	for _, issue := range issues {
		if issue == nil || issue.PullRequestLinks == nil {
			filtered = append(filtered, issue)
		}
	}
	hasMore := resp != nil && resp.NextPage > 0
	return filtered, hasMore, nil
}

func (c *liveClient) ListRepositoryWorkflows(
	ctx context.Context, owner, repo string,
) ([]*gh.Workflow, error) {
	return collectPages(ctx, func(opts *gh.ListOptions) ([]*gh.Workflow, *gh.Response, error) {
		workflows, resp, err := c.gh.Actions.ListWorkflows(ctx, owner, repo, opts)
		if err != nil {
			return nil, resp, err
		}
		return workflows.Workflows, resp, nil
	}, c.trackRate)
}

func (c *liveClient) GetWorkflowDefinition(
	ctx context.Context, owner, repo, path, ref string,
) (string, string, error) {
	file, directory, resp, err := c.gh.Repositories.GetContents(
		ctx, owner, repo, path, &gh.RepositoryContentGetOptions{Ref: ref},
	)
	c.trackRate(resp)
	if err != nil {
		return "", "", err
	}
	if file == nil || directory != nil {
		return "", "", fmt.Errorf("workflow definition %q is not a file", path)
	}
	content, err := file.GetContent()
	if err != nil {
		return "", "", fmt.Errorf("decode workflow definition %q: %w", path, err)
	}
	if len(content) > platformgithub.MaxWorkflowDefinitionBytes {
		return "", "", fmt.Errorf(
			"workflow definition exceeds %d-byte limit",
			platformgithub.MaxWorkflowDefinitionBytes,
		)
	}
	return content, file.GetSHA(), nil
}

func (c *liveClient) ListRepositoryEnvironments(
	ctx context.Context, owner, repo string,
) ([]*gh.Environment, error) {
	return collectPages(ctx, func(opts *gh.ListOptions) ([]*gh.Environment, *gh.Response, error) {
		environments, resp, err := c.gh.Repositories.ListEnvironments(
			ctx, owner, repo, &gh.EnvironmentListOptions{ListOptions: *opts},
		)
		if err != nil {
			return nil, resp, err
		}
		return environments.Environments, resp, nil
	}, c.trackRate)
}

func (c *liveClient) ListManualWorkflowRuns(
	ctx context.Context,
	owner, repo string,
	workflowID int64,
	query platform.WorkflowRunQuery,
) (platform.Page[*gh.WorkflowRun], error) {
	page := 0
	if query.Cursor != "" {
		parsed, err := strconv.Atoi(query.Cursor)
		if err != nil || parsed <= 0 {
			return platform.Page[*gh.WorkflowRun]{}, &platform.Error{
				Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub,
				PlatformHost: c.platformHost, Field: "cursor",
				Err: fmt.Errorf("cursor must be a positive decimal GitHub page number"),
			}
		}
		page = parsed
	}
	runs, resp, err := c.gh.Actions.ListWorkflowRunsByID(
		ctx, owner, repo, workflowID, &gh.ListWorkflowRunsOptions{
			Event: query.Event, Branch: query.Branch,
			Page: page, PerPage: query.PerPage,
		},
	)
	c.trackRate(resp)
	if err != nil {
		return platform.Page[*gh.WorkflowRun]{}, err
	}
	result := platform.Page[*gh.WorkflowRun]{Items: runs.WorkflowRuns}
	if resp != nil && resp.NextPage > 0 {
		result.NextCursor = strconv.Itoa(resp.NextPage)
	} else {
		result.Exhausted = true
	}
	return result, nil
}

func (c *liveClient) ListManualWorkflowJobs(
	ctx context.Context, owner, repo string, runID int64,
) ([]*gh.WorkflowJob, error) {
	return collectPages(ctx, func(opts *gh.ListOptions) ([]*gh.WorkflowJob, *gh.Response, error) {
		jobs, resp, err := c.gh.Actions.ListWorkflowJobs(
			ctx, owner, repo, runID, &gh.ListWorkflowJobsOptions{ListOptions: *opts},
		)
		if err != nil {
			return nil, resp, err
		}
		return jobs.Jobs, resp, nil
	}, c.trackRate)
}

func (c *liveClient) DispatchManualWorkflow(
	ctx context.Context,
	owner, repo string,
	workflowID int64,
	request gh.CreateWorkflowDispatchEventRequest,
) (*gh.WorkflowDispatchRunDetails, error) {
	request.ReturnRunDetails = new(true)
	details, resp, err := c.writeGH().Actions.CreateWorkflowDispatchEventByID(
		ctx, owner, repo, workflowID, request,
	)
	c.trackWriteRate(resp)
	return details, err
}
