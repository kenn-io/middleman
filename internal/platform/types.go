package platform

import (
	"fmt"
	"time"
)

type Kind string

const (
	KindGitHub  Kind = "github"
	KindGitLab  Kind = "gitlab"
	KindForgejo Kind = "forgejo"
	KindGitea   Kind = "gitea"
)

const (
	DefaultGitHubHost  = "github.com"
	DefaultGitLabHost  = "gitlab.com"
	DefaultForgejoHost = "codeberg.org"
	DefaultGiteaHost   = "gitea.com"
)

type RepoRef struct {
	Platform           Kind
	Host               string
	Owner              string
	Name               string
	RepoPath           string
	PlatformID         int64
	PlatformExternalID string
	WebURL             string
	CloneURL           string
	DefaultBranch      string
}

func (r RepoRef) DisplayName() string {
	if r.RepoPath != "" {
		return r.RepoPath
	}
	if r.Owner == "" {
		return r.Name
	}
	if r.Name == "" {
		return r.Owner
	}
	return r.Owner + "/" + r.Name
}

type RepositoryFeatures struct {
	IssuesEnabled        *bool
	MergeRequestsEnabled *bool
}

type Repository struct {
	Ref                RepoRef
	PlatformID         int64
	PlatformExternalID string
	Description        string
	Private            bool
	Archived           bool
	Features           RepositoryFeatures
	MergeSettings      *RepositoryMergeSettings
	ViewerCanMerge     *bool
	DefaultBranch      string
	WebURL             string
	CloneURL           string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (r Repository) FeatureEnabled(feature string) (bool, bool) {
	var enabled *bool
	switch feature {
	case RepositoryFeatureIssues:
		enabled = r.Features.IssuesEnabled
	case RepositoryFeatureMergeRequests:
		enabled = r.Features.MergeRequestsEnabled
	default:
		return false, false
	}
	if enabled == nil {
		return false, false
	}
	return *enabled, true
}

type RepositoryMergeSettings struct {
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
}

type MergeRequest struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	Number             int
	URL                string
	Title              string
	Author             string
	AuthorDisplayName  string
	State              string
	IsDraft            bool
	IsLocked           bool
	Body               string
	HeadBranch         string
	BaseBranch         string
	HeadSHA            string
	BaseSHA            string
	// HeadRepoCloneURL is the clone URL of the head (source) repository. An
	// empty value with HeadRepoCloneURLUnknown unset is authoritative (the
	// provider confirmed there is no reachable head repository); setting
	// HeadRepoCloneURLUnknown marks this observation as undetermined (for
	// example a failed best-effort fork enrichment), and persistence then
	// preserves the previously stored value instead of clearing it.
	HeadRepoCloneURL        string
	HeadRepoCloneURLUnknown bool
	// AdditionsKnown and DeletionsKnown distinguish an explicit zero from a
	// provider response that omitted the corresponding diff metric.
	Additions      int
	AdditionsKnown bool
	Deletions      int
	DeletionsKnown bool
	FilesChanged   *int
	MergeCommitSHA string
	CommentCount   int
	ReviewDecision string
	CIStatus       string
	MergeableState string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastActivityAt time.Time
	MergedAt       *time.Time
	MergedBy       string
	ClosedAt       *time.Time
	Labels         []Label
	// Assignees and RequestedReviewers carry usernames. nil means the
	// provider response did not include the field (unknown), while an
	// empty non-nil slice means the provider reported none. Persistence
	// preserves the previously stored value for nil so partial provider
	// responses never wipe synced data.
	Assignees          []string
	RequestedReviewers []string
}

type Issue struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	Number             int
	URL                string
	Title              string
	Author             string
	State              string
	Body               string
	CommentCount       int
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastActivityAt     time.Time
	ClosedAt           *time.Time
	Labels             []Label
	Assignees          []string
}

type Label struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	Name               string
	Description        string
	Color              string
	IsDefault          bool
}

type MergeRequestEvent struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	MergeRequestNumber int
	EventType          string
	Author             string
	Summary            string
	Body               string
	MetadataJSON       string
	CreatedAt          time.Time
	DedupeKey          string
	DirectURL          string
	// ThreadID groups root comments and replies that belong to the same
	// provider conversation. GitLab calls this a discussion ID.
	ThreadID     string
	PositionJSON string
	Resolvable   bool
	Resolved     bool
}

type IssueEvent struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	IssueNumber        int
	EventType          string
	Author             string
	Summary            string
	Body               string
	MetadataJSON       string
	CreatedAt          time.Time
	DedupeKey          string
	DirectURL          string
	// ThreadID groups root comments and replies that belong to the same
	// provider conversation. GitLab calls this a discussion ID.
	ThreadID string
}

type Release struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	TagName            string
	Name               string
	URL                string
	TargetCommitish    string
	Prerelease         bool
	PublishedAt        *time.Time
	CreatedAt          time.Time
}

type Tag struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	Name               string
	SHA                string
	URL                string
}

type NotificationListOptions struct {
	Since         *time.Time
	All           bool
	Participating bool
	Page          int
	RepoOwner     string
	RepoName      string
}

type NotificationThread struct {
	ID                      string
	RepoOwner               string
	RepoName                string
	SubjectType             string
	SubjectTitle            string
	SubjectURL              string
	SubjectLatestCommentURL string
	WebURL                  string
	ItemNumber              *int
	ItemType                string
	ItemAuthor              string
	Reason                  string
	Unread                  bool
	Participating           bool
	UpdatedAt               time.Time
	LastReadAt              *time.Time
}

type CICheck struct {
	Repo               RepoRef
	PlatformID         int64
	PlatformExternalID string
	Name               string
	Status             string
	Conclusion         string
	URL                string
	App                string
	StartedAt          *time.Time
	CompletedAt        *time.Time
}

type MergeResult struct {
	Merged  bool
	SHA     string
	Message string
}

type ReviewAction string

const (
	ReviewActionComment        ReviewAction = "comment"
	ReviewActionApprove        ReviewAction = "approve"
	ReviewActionRequestChanges ReviewAction = "request_changes"
)

type DiffReviewLineRange struct {
	Path         string
	OldPath      string
	Side         string
	StartSide    string
	StartLine    *int
	Line         int
	OldLine      *int
	NewLine      *int
	LineType     string
	DiffHeadSHA  string
	DiffBaseSHA  string
	MergeBaseSHA string
	CommitSHA    string
}

type LocalDiffReviewDraftComment struct {
	ID        int64
	Body      string
	Range     DiffReviewLineRange
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PublishDiffReviewDraftInput struct {
	Body     string
	Action   ReviewAction
	HeadSHA  string
	Comments []LocalDiffReviewDraftComment
}

type PublishedDiffReview struct {
	ProviderReviewID string
	SubmittedAt      time.Time
}

type DiffReviewPublishPartialError struct {
	Err                 error
	PublishedCommentIDs []int64
}

func (e *DiffReviewPublishPartialError) Error() string {
	if e == nil || e.Err == nil {
		return "diff review partially published"
	}
	return e.Err.Error()
}

func (e *DiffReviewPublishPartialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type MergeRequestReviewThread struct {
	Repo               RepoRef
	MergeRequestNumber int
	ProviderThreadID   string
	ProviderReviewID   string
	ProviderCommentID  string
	Body               string
	AuthorLogin        string
	DirectURL          string
	Range              DiffReviewLineRange
	Resolved           bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ResolvedAt         *time.Time
	MetadataJSON       string
}

type ReviewSuggestion struct {
	ProviderThreadID  string
	ProviderCommentID string
	Range             DiffReviewLineRange
	Replacement       string
}

type ApplyReviewSuggestionsInput struct {
	HeadBranch       string
	HeadRepoCloneURL string
	ExpectedHeadSHA  string
	Message          string
	Suggestions      []ReviewSuggestion
}

type AppliedReviewSuggestions struct {
	CommitSHA string
	CommitURL string
}

type ArchiveCapability string

const (
	ArchiveCapabilityHistoricalIssues        ArchiveCapability = "archive_historical_issues"
	ArchiveCapabilityHistoricalMergeRequests ArchiveCapability = "archive_historical_merge_requests"
	ArchiveCapabilityOrdinaryComments        ArchiveCapability = "archive_ordinary_comments"
	ArchiveCapabilitySubmittedReviews        ArchiveCapability = "archive_submitted_reviews"
	ArchiveCapabilityInlineReviewComments    ArchiveCapability = "archive_inline_review_comments"
)

type ArchiveCapabilitySupport string

const (
	ArchiveCapabilitySupported   ArchiveCapabilitySupport = "supported"
	ArchiveCapabilityUnsupported ArchiveCapabilitySupport = "unsupported"
)

type ArchiveCapabilities struct {
	HistoricalIssues        bool
	HistoricalMergeRequests bool
	OrdinaryComments        bool
	SubmittedReviews        bool
	InlineReviewComments    bool
}

func (c ArchiveCapabilities) HasHistoricalInventory() bool {
	return c.HistoricalIssues || c.HistoricalMergeRequests
}

func (c ArchiveCapabilities) Support(capability ArchiveCapability) (ArchiveCapabilitySupport, error) {
	supported := false
	switch capability {
	case ArchiveCapabilityHistoricalIssues:
		supported = c.HistoricalIssues
	case ArchiveCapabilityHistoricalMergeRequests:
		supported = c.HistoricalMergeRequests
	case ArchiveCapabilityOrdinaryComments:
		supported = c.OrdinaryComments
	case ArchiveCapabilitySubmittedReviews:
		supported = c.SubmittedReviews
	case ArchiveCapabilityInlineReviewComments:
		supported = c.InlineReviewComments
	default:
		return "", invalidArchiveCapability("", "", capability)
	}
	if supported {
		return ArchiveCapabilitySupported, nil
	}
	return ArchiveCapabilityUnsupported, nil
}

func (c ArchiveCapabilities) Require(kind Kind, host string, capability ArchiveCapability) error {
	support, err := c.Support(capability)
	if err != nil {
		return invalidArchiveCapability(kind, host, capability)
	}
	if support == ArchiveCapabilitySupported {
		return nil
	}
	return UnsupportedCapability(kind, host, string(capability))
}

func invalidArchiveCapability(kind Kind, host string, capability ArchiveCapability) error {
	return &Error{
		Code:         ErrCodeInvalidArgument,
		Provider:     kind,
		PlatformHost: host,
		Field:        "archive_capability",
		Err:          fmt.Errorf("unknown archive capability %q", capability),
	}
}

type Page[T any] struct {
	Items      []T
	NextCursor string
	Exhausted  bool
	// ProgressOnly explicitly records an upstream page consumed while
	// provider-side filtering yielded no items for this dataset.
	ProgressOnly bool
}

func ValidatePage[T any](kind Kind, host, inputCursor string, page Page[T]) error {
	hasCursor := page.NextCursor != ""
	if hasCursor == page.Exhausted {
		return archiveContractError(
			kind,
			host,
			"archive_page",
			"archive page must return either a next cursor or exhaustion",
		)
	}
	if !page.Exhausted && len(page.Items) == 0 {
		if !page.ProgressOnly {
			return archiveContractError(
				kind,
				host,
				"archive_page",
				"non-exhausted empty archive page must declare filtered progress",
			)
		}
	}
	if page.ProgressOnly && (page.Exhausted || len(page.Items) != 0) {
		return archiveContractError(
			kind,
			host,
			"archive_page",
			"filtered progress requires an empty non-exhausted archive page",
		)
	}
	if hasCursor && page.NextCursor == inputCursor {
		return archiveContractError(
			kind,
			host,
			"archive_page_cursor",
			"archive page repeated its nonempty input cursor",
		)
	}
	return nil
}

func archiveContractError(kind Kind, host, field, format string, args ...any) error {
	return ProviderContract(kind, host, field, fmt.Errorf(format, args...))
}

type WorkflowInputType string

const (
	WorkflowInputString      WorkflowInputType = "string"
	WorkflowInputNumber      WorkflowInputType = "number"
	WorkflowInputBoolean     WorkflowInputType = "boolean"
	WorkflowInputChoice      WorkflowInputType = "choice"
	WorkflowInputEnvironment WorkflowInputType = "environment"
)

type WorkflowInput struct {
	Name        string
	Description string
	Required    bool
	Type        WorkflowInputType
	Default     any
	HasDefault  bool
	Options     []string
}

type WorkflowDefinition struct {
	ID                string
	Name              string
	Path              string
	State             string
	WebURL            string
	DefinitionSHA     string
	Inputs            []WorkflowInput
	Available         bool
	UnavailableReason string
}

type WorkflowEnvironment struct {
	Name string
}

type WorkflowRunQuery struct {
	WorkflowID string
	Event      string
	Branch     string
	Cursor     string
	PerPage    int
}

type WorkflowRunStep struct {
	Number      int
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

type WorkflowRunJob struct {
	ID          string
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
	WebURL      string
	Steps       []WorkflowRunStep
}

type WorkflowRun struct {
	ID         string
	WorkflowID string
	RunNumber  int64
	Name       string
	Event      string
	Ref        string
	HeadSHA    string
	Actor      string
	Status     string
	Conclusion string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	WebURL     string
}

type WorkflowDispatchRequest struct {
	WorkflowID            string
	Ref                   string
	Inputs                map[string]any
	ExpectedDefinitionSHA string
}

// WorkflowDispatchResult reports an accepted dispatch. Run is set only when the
// provider names the created run; otherwise callers locate it by reading runs.
type WorkflowDispatchResult struct {
	Accepted bool
	Run      *WorkflowRun
	Actor    string
}

type Capabilities struct {
	ReadRepositories      bool
	ReadMergeRequests     bool
	ReadIssues            bool
	ReadIssuePRReferences bool
	ReadComments          bool
	ReadReleases          bool
	ReadCI                bool
	ReadLabels            bool
	ReadMarkdownImages    bool
	ReadAuthenticatedUser bool
	ReadNotifications     bool
	ReadWorkflows         bool
	ReadWorkflowRuns      bool
	CommentMutation       bool
	// StateMutation means the provider can PATCH the item itself:
	// open/close state transitions AND title/body/content updates.
	// Every provider implements both through the same mutator, and
	// the UI gates both kinds of edit affordance on this flag. A
	// provider that could change state but not content (or vice
	// versa) must split this capability rather than half-implement
	// it.
	StateMutation               bool
	MergeMutation               bool
	ReviewMutation              bool
	WorkflowApproval            bool
	WorkflowDispatch            bool
	ReadyForReview              bool
	DraftMutation               bool
	IssueMutation               bool
	LabelMutation               bool
	AssigneeMutation            bool
	ReviewerMutation            bool
	NotificationMutation        bool
	ThreadReply                 bool
	ThreadResolve               bool
	ReviewDraftMutation         bool
	ReviewThreadResolution      bool
	ReviewSuggestionApplication bool
	ReadReviewThreads           bool
	NativeMultilineRanges       bool
	// MutationHeadBinding is true when mutations can be hard-bound to an
	// expected head SHA and the provider rejects the mutation when the MR
	// head moved past it. Merge uses the reviewed diff head as that pin;
	// direct approval uses the caller's target provider head. Providers
	// without it treat the expected head SHA as advisory.
	MutationHeadBinding    bool
	SupportedReviewActions []ReviewAction
	Archive                ArchiveCapabilities
}

type RepositoryListOptions struct {
	Limit  int
	Offset int
	// IncludeArchived asks providers that filter archived repositories
	// out of listings (GitLab) to keep them. Configuration expansion
	// sets it so archived repositories can match configured globs;
	// import previews leave it unset so archived repositories never
	// crowd live ones out of a limited listing. Providers that never
	// filter archived repositories (GitHub, gitea-like) ignore it.
	IncludeArchived bool
}
