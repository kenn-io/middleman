package platform

import (
	"context"
	"time"
)

type Provider interface {
	Platform() Kind
	Host() string
	Capabilities() Capabilities
}

type RateLimitBucket string

const (
	RateLimitBucketREST    RateLimitBucket = "rest"
	RateLimitBucketGraphQL RateLimitBucket = "graphql"
)

type OperationName string

const (
	OperationApplyReviewSuggestion OperationName = "apply_review_suggestion"
)

// OperationRateLimitReporter lets a provider override the server's default
// operation-to-rate-bucket mapping when an implementation consumes a different
// API budget than the provider-neutral operation normally would.
type OperationRateLimitReporter interface {
	OperationRateLimitBuckets(operation OperationName) ([]RateLimitBucket, bool)
}

type RepositoryReader interface {
	GetRepository(ctx context.Context, ref RepoRef) (Repository, error)
	ListRepositories(
		ctx context.Context,
		owner string,
		opts RepositoryListOptions,
	) ([]Repository, error)
}

type MarkdownImage struct {
	Content     []byte
	ContentType string
	// Mutable marks a source whose bytes can change under the same URL, such
	// as a file read through a branch or tag ref. Callers must cache it
	// briefly instead of treating the URL as content-addressed.
	Mutable bool
}

// MarkdownImageReader fetches provider-hosted images embedded in repository
// Markdown. Providers should accept only their own trusted attachment URLs;
// callers must not be able to turn this into a general-purpose URL fetcher.
type MarkdownImageReader interface {
	GetMarkdownImage(ctx context.Context, ref RepoRef, sourceURL string) (MarkdownImage, error)
}

type MergeRequestReader interface {
	ListOpenMergeRequests(ctx context.Context, ref RepoRef) ([]MergeRequest, error)
	GetMergeRequest(ctx context.Context, ref RepoRef, number int) (MergeRequest, error)
	ListMergeRequestEvents(
		ctx context.Context,
		ref RepoRef,
		number int,
	) ([]MergeRequestEvent, error)
}

type MergeRequestViewerResolver interface {
	ViewerAuthoredMergeRequest(ctx context.Context, mr MergeRequest) (bool, error)
}

// AuthenticatedUserResolver returns the username attached to the provider
// credential used for a repository. The repository matters for providers
// whose credential routing can vary by owner or repository.
type AuthenticatedUserResolver interface {
	AuthenticatedUser(ctx context.Context, ref RepoRef) (string, error)
}

// AuthenticatedUserCacheKeyResolver identifies the effective credential used
// for a repository without exposing credential material. Repositories with the
// same non-empty key share one authenticated-user lookup for the process.
type AuthenticatedUserCacheKeyResolver interface {
	AuthenticatedUserCacheKey(ref RepoRef) string
}

type IssueReader interface {
	ListOpenIssues(ctx context.Context, ref RepoRef) ([]Issue, error)
	GetIssue(ctx context.Context, ref RepoRef, number int) (Issue, error)
	ListIssueEvents(ctx context.Context, ref RepoRef, number int) ([]IssueEvent, error)
}

// IssuePageReader is the archive inventory surface for issues.
type IssuePageReader interface {
	ListIssuesPage(context.Context, RepoRef, ItemPageQuery) (Page[Issue], error)
}

// MergeRequestPageReader is the archive inventory surface for merge requests.
type MergeRequestPageReader interface {
	ListMergeRequestsPage(context.Context, RepoRef, ItemPageQuery) (Page[MergeRequest], error)
}

type LabelCatalog struct {
	Labels      []Label
	NotModified bool
}

type LabelReader interface {
	ListLabels(ctx context.Context, ref RepoRef) (LabelCatalog, error)
}

type ReleaseReader interface {
	ListReleases(ctx context.Context, ref RepoRef) ([]Release, error)
}

type TagReader interface {
	ListTags(ctx context.Context, ref RepoRef) ([]Tag, error)
}

type CIReader interface {
	ListCIChecks(ctx context.Context, ref RepoRef, sha string) ([]CICheck, error)
}

type CommentMutator interface {
	CreateMergeRequestComment(
		ctx context.Context,
		ref RepoRef,
		number int,
		body string,
	) (MergeRequestEvent, error)
	EditMergeRequestComment(
		ctx context.Context,
		ref RepoRef,
		number int,
		commentID int64,
		body string,
	) (MergeRequestEvent, error)
	DeleteMergeRequestComment(ctx context.Context, ref RepoRef, number int, commentID int64) error
	CreateIssueComment(ctx context.Context, ref RepoRef, number int, body string) (IssueEvent, error)
	EditIssueComment(ctx context.Context, ref RepoRef, number int, commentID int64, body string) (IssueEvent, error)
	DeleteIssueComment(ctx context.Context, ref RepoRef, number int, commentID int64) error
}

type StateMutator interface {
	SetMergeRequestState(ctx context.Context, ref RepoRef, number int, state string) (MergeRequest, error)
	SetIssueState(ctx context.Context, ref RepoRef, number int, state string) (Issue, error)
}

type MergeMutator interface {
	// MergeMergeRequest merges the MR. expectedHeadSHA is the head commit
	// the caller reviewed; providers that support head binding must reject
	// the merge when the MR head has moved past it. An empty value skips
	// the check. Providers whose API cannot bind the merge to a head
	// commit treat it as advisory.
	MergeMergeRequest(
		ctx context.Context,
		ref RepoRef,
		number int,
		commitTitle string,
		commitMessage string,
		method string,
		expectedHeadSHA string,
	) (MergeResult, error)
}

type WorkflowApprovalMutator interface {
	ApproveWorkflow(ctx context.Context, ref RepoRef, runID string) error
}

type ReadyForReviewMutator interface {
	MarkReadyForReview(ctx context.Context, ref RepoRef, number int) (MergeRequest, error)
}

type DraftMutator interface {
	ConvertMergeRequestToDraft(ctx context.Context, ref RepoRef, number int) (time.Time, error)
}

type IssueMutator interface {
	CreateIssue(ctx context.Context, ref RepoRef, title string, body string) (Issue, error)
}

type LabelMutator interface {
	SetMergeRequestLabels(ctx context.Context, ref RepoRef, number int, names []string) ([]Label, error)
	SetIssueLabels(ctx context.Context, ref RepoRef, number int, names []string) ([]Label, error)
}

// AssigneeMutator replaces the full assignee username set on a merge
// request or issue and returns the provider-normalized assignee list.
type AssigneeMutator interface {
	SetMergeRequestAssignees(ctx context.Context, ref RepoRef, number int, usernames []string) ([]string, error)
	SetIssueAssignees(ctx context.Context, ref RepoRef, number int, usernames []string) ([]string, error)
}

// ReviewerMutator requests reviews from users on a merge request and
// removes pending review requests. Both calls return the full updated
// requested-reviewer username list after the mutation. Requesting an
// empty username list is a read: it mutates nothing and returns the
// provider's current requested-reviewer set, so callers can diff a
// desired set against live provider state instead of cached state.
// All usernames, in both directions, are individual user logins;
// implementations must exclude team or group reviewer identities so a
// caller-computed removal diff never targets an unsupported identity.
type ReviewerMutator interface {
	RequestMergeRequestReviewers(ctx context.Context, ref RepoRef, number int, usernames []string) ([]string, error)
	RemoveMergeRequestReviewers(ctx context.Context, ref RepoRef, number int, usernames []string) ([]string, error)
}

type ReviewMutator interface {
	// ApproveMergeRequest approves the MR. expectedHeadSHA is the caller's
	// target provider head for the approval; providers that support head
	// binding must reject the approval when the MR head has moved past it.
	// An empty value skips the check. Providers whose API cannot bind the
	// approval to a head commit treat it as advisory.
	ApproveMergeRequest(
		ctx context.Context,
		ref RepoRef,
		number int,
		body string,
		expectedHeadSHA string,
	) (MergeRequestEvent, error)
}

type RequestChangesMutator interface {
	// RequestChanges submits a blocking review pinned to expectedHeadSHA
	// with the same head-binding semantics as
	// ReviewMutator.ApproveMergeRequest: the pin is forwarded to the
	// provider, which attaches the review to that commit or rejects a
	// stale head where it can natively. Implementations must not layer
	// extra client-side head verification or post-submit revocation on
	// top — request changes and approve share one submission contract.
	RequestChanges(
		ctx context.Context,
		ref RepoRef,
		number int,
		body string,
		expectedHeadSHA string,
	) error
}

type ThreadReplier interface {
	ReplyToThread(
		ctx context.Context,
		ref RepoRef,
		number int,
		threadID string,
		body string,
	) (MergeRequestEvent, error)
}

type ThreadResolver interface {
	ResolveThread(
		ctx context.Context,
		ref RepoRef,
		number int,
		threadID string,
		resolved bool,
	) error
}

type DiffReviewDraftMutator interface {
	PublishDiffReviewDraft(
		ctx context.Context,
		ref RepoRef,
		number int,
		input PublishDiffReviewDraftInput,
	) (*PublishedDiffReview, error)
}

type ReviewSuggestionApplier interface {
	ApplyReviewSuggestions(
		ctx context.Context,
		ref RepoRef,
		number int,
		input ApplyReviewSuggestionsInput,
	) (*AppliedReviewSuggestions, error)
}

type DiffReviewThreadResolver interface {
	ResolveDiffReviewThread(ctx context.Context, ref RepoRef, number int, providerThreadID string) error
	UnresolveDiffReviewThread(ctx context.Context, ref RepoRef, number int, providerThreadID string) error
}

type MergeRequestReviewThreadReader interface {
	ListMergeRequestReviewThreads(
		ctx context.Context,
		ref RepoRef,
		number int,
	) ([]MergeRequestReviewThread, error)
}

type MergeRequestContentMutator interface {
	EditMergeRequestContent(
		ctx context.Context,
		ref RepoRef,
		number int,
		title *string,
		body *string,
	) (MergeRequest, error)
}

type IssueContentMutator interface {
	EditIssueContent(
		ctx context.Context,
		ref RepoRef,
		number int,
		title *string,
		body *string,
	) (Issue, error)
}

// NotificationReader lists the authenticated user's notification
// threads for a host. The second return reports whether more pages
// remain.
type NotificationReader interface {
	ListNotifications(
		ctx context.Context,
		opts NotificationListOptions,
	) ([]NotificationThread, bool, error)
}

// NotificationMutator acknowledges notification threads upstream
// (mark-as-read propagation from the local inbox).
type NotificationMutator interface {
	MarkNotificationThreadRead(ctx context.Context, threadID string) error
}
