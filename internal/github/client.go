package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

type NotificationListOptions = platform.NotificationListOptions

type NotificationThread = platform.NotificationThread

func githubRateObserver(tracker *RateTracker) platform.RateObserver {
	if tracker == nil {
		return nil
	}
	return tracker
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
	ListPullRequestReviewThreads(ctx context.Context, owner, repo string, number int) ([]platformgithub.PullRequestReviewThread, error)
	ListCommits(ctx context.Context, owner, repo string, number int) ([]*gh.RepositoryCommit, error)
	ListPullRequestTimelineEvents(ctx context.Context, owner, repo string, number int) ([]platformgithub.PullRequestTimelineEvent, error)
	ListForcePushEvents(ctx context.Context, owner, repo string, number int) ([]platformgithub.ForcePushEvent, error)
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
	EditPullRequest(ctx context.Context, owner, repo string, number int, opts platformgithub.EditPullRequestOpts) (*gh.PullRequest, error)
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

// markdownImageClient carries the full repository identity even though the
// attachment URL is host-scoped: credential selection is per repository, so a
// repo-scoped route must be able to pick its own token for the fetch.
type markdownImageClient interface {
	GetMarkdownImage(
		ctx context.Context, owner, repo, sourceURL string,
	) (platform.MarkdownImage, error)
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
	) ([]platformgithub.PullRequestTimelineEvent, error)
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
	authRT := platform.AuthTransport{
		Source:              source,
		Base:                readBase,
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       allowedOrigin,
		TokenContext:        githubCredentialContext,
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

	apiBase, uploadBase, graphQL := "", "", ""
	if options.baseURLOverride != "" {
		apiBase = options.baseURLOverride + "/api/v3/"
		uploadBase = options.baseURLOverride + "/api/uploads/"
		graphQL = options.baseURLOverride + "/api/graphql"
	}
	return platformgithub.NewClient(platformgithub.ClientConfig{
		Host: platformHost, Read: httpClient, Write: writeHTTPClient,
		Notifications:  notificationHTTPClient,
		MarkdownImages: &http.Client{Transport: wrapPublicGitHubAPIGuard(http.DefaultTransport)},
		Clock:          time.Now, APIBase: apiBase, UploadBase: uploadBase, GraphQLEndpoint: graphQL,
		ReadRate: githubRateObserver(rateTracker), NotificationRate: githubRateObserver(options.notificationRateTracker),
		ViewerCacheTTL:  authenticatedViewerLoginTTL,
		ReadOnlyContext: IsArchiveSyncBudgetContext,
		GraphQLContext:  func(ctx context.Context) context.Context { return withQuotaResource(ctx, QuotaResourceGraphQL) },
		InvalidateETags: et.invalidateRepo,
		Warning:         slog.Warn,
		Progress: func(owner, repo, kind string) platformgithub.Progress {
			ref := RepoRef{Owner: owner, Name: repo, PlatformHost: platformHost}
			progress := newIssueListFetchProgressLogger(ref, "rest")
			if kind == "pulls" {
				progress = newMergeRequestListFetchProgressLogger(ref, "rest")
			}
			return platformgithub.Progress{Page: progress.recordPage, Done: progress.done}
		},
		Authentication: platformgithub.Authentication{
			Source: source,
			InstallationActive: func(owner string) bool {
				if source == nil {
					return false
				}
				if owner == "" {
					return source.Descriptor().HasActiveGitHubApp()
				}
				return source.Descriptor().HasActiveGitHubAppForOwner(owner)
			},
			CredentialKey: func() string {
				if source == nil {
					return ""
				}
				return source.Descriptor().CanonicalSourceString()
			},
			Context: func(ctx context.Context, owner string, mutation bool) context.Context {
				ctx = tokenauth.WithGitHubOwner(ctx, owner)
				if mutation {
					ctx = tokenauth.WithMutationAuth(ctx)
				}
				return ctx
			},
		},
	})
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

func githubCredentialContext(req *http.Request) context.Context {
	return tokenauth.WithGitHubOwner(req.Context(), githubOwnerFromRequest(req))
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
