package gitealike

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/forge/platform"
)

const defaultPageSize = 100

type Transport interface {
	GetRepository(ctx context.Context, owner, repo string) (RepositoryDTO, error)
	ListUserRepositories(ctx context.Context, owner string, opts PageOptions) ([]RepositoryDTO, Page, error)
	ListOrgRepositories(ctx context.Context, owner string, opts PageOptions) ([]RepositoryDTO, Page, error)
	ListOpenPullRequests(ctx context.Context, ref platform.RepoRef, opts PageOptions) ([]PullRequestDTO, Page, error)
	GetPullRequest(ctx context.Context, ref platform.RepoRef, number int) (PullRequestDTO, error)
	ListPullRequestComments(ctx context.Context, ref platform.RepoRef, number int, opts PageOptions) ([]CommentDTO, Page, error)
	ListPullRequestReviews(ctx context.Context, ref platform.RepoRef, number int, opts PageOptions) ([]ReviewDTO, Page, error)
	ListPullRequestCommits(ctx context.Context, ref platform.RepoRef, number int, opts PageOptions) ([]CommitDTO, Page, error)
	ListOpenIssues(ctx context.Context, ref platform.RepoRef, opts PageOptions) ([]IssueDTO, Page, error)
	GetIssue(ctx context.Context, ref platform.RepoRef, number int) (IssueDTO, error)
	ListIssueComments(ctx context.Context, ref platform.RepoRef, number int, opts PageOptions) ([]CommentDTO, Page, error)
	ListReleases(ctx context.Context, ref platform.RepoRef, opts PageOptions) ([]ReleaseDTO, Page, error)
	ListTags(ctx context.Context, ref platform.RepoRef, opts PageOptions) ([]TagDTO, Page, error)
	ListStatuses(ctx context.Context, ref platform.RepoRef, sha string, opts PageOptions) ([]StatusDTO, Page, error)
}

type TimelineTransport interface {
	ListIssueTimeline(ctx context.Context, ref platform.RepoRef, number int, opts PageOptions) ([]TimelineEventDTO, Page, error)
}

type AuthenticatedUserTransport interface {
	GetAuthenticatedUser(ctx context.Context) (UserDTO, error)
}

// ArchiveTransport is the bounded, one-round-trip transport surface used by
// historical archive enumeration. ListIssuesPage exposes the API's
// updated-time filters; ListPullRequestsPage exposes its stable sort modes.
type ArchiveTransport interface {
	ListIssuesPage(context.Context, platform.RepoRef, ArchiveListOptions) ([]IssueDTO, Page, error)
	ListPullRequestsPage(context.Context, platform.RepoRef, ArchiveListOptions) ([]PullRequestDTO, Page, error)
}

type ArchiveListOptions struct {
	PageOptions
	Since  time.Time
	Before time.Time
	Sort   string
}

// LabelTransport is an optional transport extension for repository
// label reads and issue-like label assignment. Forgejo and Gitea use the
// same endpoints and treat pull requests as issues for label purposes,
// so a single ReplaceIssueLabels covers both.
type LabelTransport interface {
	ListRepoLabels(ctx context.Context, ref platform.RepoRef, opts PageOptions) ([]LabelDTO, Page, error)
	ReplaceIssueLabels(ctx context.Context, ref platform.RepoRef, number int, labelIDs []int64) ([]LabelDTO, error)
}

type MutationTransport interface {
	CreateIssueComment(ctx context.Context, ref platform.RepoRef, number int, body string) (CommentDTO, error)
	EditIssueComment(ctx context.Context, ref platform.RepoRef, commentID int64, body string) (CommentDTO, error)
	DeleteIssueComment(ctx context.Context, ref platform.RepoRef, commentID int64) error
	CreateIssue(ctx context.Context, ref platform.RepoRef, title string, body string) (IssueDTO, error)
	EditIssue(ctx context.Context, ref platform.RepoRef, number int, opts IssueMutationOptions) (IssueDTO, error)
	EditPullRequest(ctx context.Context, ref platform.RepoRef, number int, opts PullRequestMutationOptions) (PullRequestDTO, error)
	MergePullRequest(ctx context.Context, ref platform.RepoRef, number int, opts MergeOptions) (MergeResultDTO, error)
}

// ReviewRequestTransport is the optional transport surface for adding
// and removing pull request review requests. The endpoints return no
// useful body, so callers re-read the pull request (or its reviews) for
// the updated requested-reviewer set.
type ReviewRequestTransport interface {
	CreateReviewRequests(ctx context.Context, ref platform.RepoRef, number int, reviewers []string) error
	DeleteReviewRequests(ctx context.Context, ref platform.RepoRef, number int, reviewers []string) error
}

type ReviewMutationTransport interface {
	CreatePullReview(ctx context.Context, ref platform.RepoRef, number int, opts ReviewOptions) (ReviewDTO, error)
}

type ActionsTransport interface {
	ListActionRuns(ctx context.Context, ref platform.RepoRef, sha string, opts PageOptions) ([]ActionRunDTO, Page, error)
}

type PageOptions struct {
	Page     int
	PageSize int
}

type Page struct {
	Next int
	Last int
}

type UserDTO struct {
	ID       int64
	UserName string
	FullName string
}

type RepositoryDTO struct {
	ID                   int64
	Owner                UserDTO
	Name                 string
	FullName             string
	HTMLURL              string
	CloneURL             string
	DefaultBranch        string
	Private              bool
	Archived             bool
	Description          string
	AllowSquash          bool
	AllowMerge           bool
	AllowRebase          bool
	CanPush              *bool
	CanAdmin             *bool
	IssuesEnabled        *bool
	MergeRequestsEnabled *bool
	Created              time.Time
	Updated              time.Time
}

type BranchDTO struct {
	Ref          string
	SHA          string
	RepoCloneURL string
}

type LabelDTO struct {
	ID          int64
	Name        string
	Description string
	Color       string
	IsDefault   bool
}

type PullRequestDTO struct {
	ID        int64
	Index     int
	HTMLURL   string
	Title     string
	User      UserDTO
	State     string
	Draft     bool
	IsLocked  bool
	Body      string
	Head      BranchDTO
	Base      BranchDTO
	Labels    []LabelDTO
	Comments  int
	Mergeable *bool
	Additions int
	// AdditionsKnown and DeletionsKnown distinguish explicit zero values from
	// fields omitted by the provider response.
	AdditionsKnown bool
	Deletions      int
	DeletionsKnown bool
	// FilesChanged is nil when the provider response did not expose the
	// count and non-nil when the provider reported it, including zero.
	FilesChanged *int
	Created      time.Time
	Updated      time.Time
	Merged       bool
	MergedAt     *time.Time
	MergedBy     UserDTO
	Closed       *time.Time
	// Assignees and RequestedReviewers are nil when the transport's SDK
	// does not expose the field (unknown) and an empty non-nil slice
	// when the provider reported none. The Forgejo SDK has no
	// requested-reviewers field on pull requests, so Forgejo transports
	// leave RequestedReviewers nil.
	Assignees          []UserDTO
	RequestedReviewers []UserDTO
}

type IssueDTO struct {
	ID            int64
	Index         int
	HTMLURL       string
	Title         string
	User          UserDTO
	State         string
	Body          string
	Comments      int
	Labels        []LabelDTO
	Assignees     []UserDTO
	Created       time.Time
	Updated       time.Time
	Closed        *time.Time
	IsPullRequest bool
}

type CommentDTO struct {
	ID      int64
	HTMLURL string
	User    UserDTO
	Body    string
	Created time.Time
	Updated time.Time
}

type TimelineEventDTO struct {
	ID            int64
	HTMLURL       string
	User          UserDTO
	Type          string
	Body          string
	Assignee      UserDTO
	PreviousTitle string
	CurrentTitle  string
	Reference     *IssueReferenceDTO
	RefAction     string
	Created       time.Time
	Updated       time.Time
}

type IssueReferenceDTO struct {
	Owner         string
	Repo          string
	Number        int
	Title         string
	HTMLURL       string
	IsPullRequest bool
}

type ReviewDTO struct {
	ID        int64
	User      UserDTO
	State     string
	Body      string
	Submitted time.Time
}

type CommitDTO struct {
	SHA        string
	AuthorName string
	Message    string
	URL        string
	Created    time.Time
}

type ReleaseDTO struct {
	ID          int64
	TagName     string
	Title       string
	HTMLURL     string
	Target      string
	Prerelease  bool
	PublishedAt *time.Time
	CreatedAt   time.Time
}

type TagDTO struct {
	Name   string
	Commit CommitDTO
	URL    string
}

type StatusDTO struct {
	ID          int64
	Context     string
	State       string
	TargetURL   string
	Description string
	Created     time.Time
	Updated     time.Time
}

type IssueMutationOptions struct {
	Title *string
	Body  *string
	State *string
	// Assignees replaces the full assignee username set when non-nil.
	Assignees *[]string
}

type PullRequestMutationOptions struct {
	Title *string
	Body  *string
	State *string
	// Assignees replaces the full assignee username set when non-nil.
	Assignees *[]string
}

type MergeOptions struct {
	CommitTitle   string
	CommitMessage string
	Method        string
	// ExpectedHeadSHA, when set, is sent as head_commit_id so the
	// provider rejects the merge if the PR head moved past the
	// reviewed commit.
	ExpectedHeadSHA string
}

type ReviewOptions struct {
	State    string
	Body     string
	CommitID string
}

type MergeResultDTO struct {
	Merged  bool
	SHA     string
	Message string
}

type ActionRunDTO struct {
	ID           int64
	RunNumber    int64
	WorkflowID   string
	Title        string
	Status       string
	Conclusion   string
	CommitSHA    string
	HTMLURL      string
	Created      time.Time
	Updated      time.Time
	Started      *time.Time
	Stopped      *time.Time
	NeedApproval bool
}

type HTTPError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Message
	if message == "" {
		message = httpStatusMessage(e.StatusCode)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", message, e.Err)
	}
	return message
}

func (e *HTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func mapTransportError(kind platform.Kind, host string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	var code platform.PlatformErrorCode
	switch httpErr.StatusCode {
	case 401, 403:
		code = platform.ErrCodePermissionDenied
	case 404:
		code = platform.ErrCodeNotFound
	case http.StatusTooManyRequests:
		code = platform.ErrCodeRateLimited
	default:
		return err
	}
	return &platform.Error{
		Code:         code,
		Provider:     kind,
		PlatformHost: host,
		Err:          err,
	}
}

func httpStatusMessage(statusCode int) string {
	switch statusCode {
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not found"
	default:
		return fmt.Sprintf("http status %d", statusCode)
	}
}
