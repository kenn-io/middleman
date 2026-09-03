package db

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Label struct {
	ID                 int64      `json:"-"`
	RepoID             int64      `json:"-"`
	PlatformID         int64      `json:"-"`
	PlatformExternalID string     `json:"-"`
	Name               string     `json:"name"`
	Description        string     `json:"description,omitempty"`
	Color              string     `json:"color"`
	IsDefault          bool       `json:"is_default"`
	UpdatedAt          time.Time  `json:"-"`
	CatalogPresent     bool       `json:"-"`
	CatalogSeenAt      *time.Time `json:"-"`
}

// LabelCatalogFreshness records provider catalog sync state for a repository.
type LabelCatalogFreshness struct {
	SyncedAt  *time.Time
	CheckedAt *time.Time
	SyncError string
}

type Repo struct {
	ID                    int64
	Platform              string
	PlatformHost          string
	PlatformRepoID        string `json:"-"`
	Owner                 string
	Name                  string
	RepoPath              string `json:"-"`
	OwnerKey              string `json:"-"`
	NameKey               string `json:"-"`
	RepoPathKey           string `json:"-"`
	WebURL                string `json:"-"`
	CloneURL              string `json:"-"`
	DefaultBranch         string `json:"-"`
	LastSyncStartedAt     *time.Time
	LastSyncCompletedAt   *time.Time
	LastSyncError         string
	AllowSquashMerge      bool
	AllowMergeCommit      bool
	AllowRebaseMerge      bool
	ViewerCanMerge        bool
	LabelCatalogSyncedAt  *time.Time
	LabelCatalogCheckedAt *time.Time
	LabelCatalogSyncError string
	CreatedAt             time.Time
}

func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
}

type RepoIdentity struct {
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	Owner          string
	Name           string
	RepoPath       string
	OwnerKey       string
	NameKey        string
	RepoPathKey    string
}

type RepoProviderMetadata struct {
	PlatformRepoID string
	WebURL         string
	CloneURL       string
	DefaultBranch  string
}

type RepoMergeSettings struct {
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
}

type ArchiveCollectionMode string

const (
	ArchiveCollectionModeDiscovery ArchiveCollectionMode = "discovery"
	ArchiveCollectionModeFull      ArchiveCollectionMode = "full"
)

type ArchiveOperatorState string

const (
	ArchiveOperatorStateActive ArchiveOperatorState = "active"
	ArchiveOperatorStatePaused ArchiveOperatorState = "paused"
)

type ArchiveItemType string

const (
	ArchiveItemTypeIssue        ArchiveItemType = "issue"
	ArchiveItemTypeMergeRequest ArchiveItemType = "merge_request"
)

type ArchiveLifecycleState string

const (
	ArchiveLifecycleStateRemovedUpstream ArchiveLifecycleState = "removed_upstream"
	ArchiveLifecycleStateInaccessible    ArchiveLifecycleState = "inaccessible"
)

type ArchiveDataset string

const (
	ArchiveDatasetLookup ArchiveDataset = "lookup"
)

// ArchiveScanKind names one repository-level scan with durable cursor state.
type ArchiveScanKind string

const (
	ArchiveScanIssueInventory           ArchiveScanKind = "issue_inventory"
	ArchiveScanMergeRequestInventory    ArchiveScanKind = "merge_request_inventory"
	ArchiveScanMaintenanceIssues        ArchiveScanKind = "maintenance_issues"
	ArchiveScanMaintenanceMergeRequests ArchiveScanKind = "maintenance_merge_requests"
)

// ArchiveScanStatus is the lifecycle of one repository-level scan row.
type ArchiveScanStatus string

const (
	ArchiveScanPending  ArchiveScanStatus = "pending"
	ArchiveScanRunning  ArchiveScanStatus = "running"
	ArchiveScanComplete ArchiveScanStatus = "complete"
	ArchiveScanBlocked  ArchiveScanStatus = "blocked"
)

// ArchiveScanState is the durable progress of one repository-level scan:
// historical inventory or maintenance, per item type. The scans table is the
// single authority for repository cursors.
type ArchiveScanState struct {
	Generation      int64
	NextCursor      *string
	LastInputCursor *string
	PageCount       int
	Status          ArchiveScanStatus
	LastErrorCode   *string
	LastErrorDetail *string
}

// Complete reports whether the scan reached explicit end-of-pagination in its
// current generation.
func (s ArchiveScanState) Complete() bool { return s.Status == ArchiveScanComplete }

// Blocked reports whether the scan durably stopped.
func (s ArchiveScanState) Blocked() bool { return s.Status == ArchiveScanBlocked }

// Cursor returns the durable next input cursor, empty for page one.
func (s ArchiveScanState) Cursor() string {
	if s.NextCursor == nil {
		return ""
	}
	return *s.NextCursor
}

// StaleArchiveScanError reports an inventory or maintenance page commit whose
// scan generation or input cursor no longer matches the durable scan row.
type StaleArchiveScanError struct {
	RepoID             int64
	Scan               ArchiveScanKind
	ExpectedGeneration int64
	GotGeneration      int64
	ExpectedCursor     string
	GotCursor          string
	// GotStatus is set when the scan's status alone rejected the commit (a
	// completed scan accepts nothing but an exact final-page replay).
	GotStatus ArchiveScanStatus
}

func (e *StaleArchiveScanError) Error() string {
	return fmt.Sprintf(
		"stale archive scan for repo %d %s: generation %d/%d, cursor %q/%q",
		e.RepoID, e.Scan,
		e.ExpectedGeneration, e.GotGeneration,
		e.ExpectedCursor, e.GotCursor,
	)
}

// ArchiveDatasetProgressStatus is the lifecycle of one dataset progress row.
type ArchiveDatasetProgressStatus string

const (
	ArchiveDatasetProgressPending     ArchiveDatasetProgressStatus = "pending"
	ArchiveDatasetProgressComplete    ArchiveDatasetProgressStatus = "complete"
	ArchiveDatasetProgressUnsupported ArchiveDatasetProgressStatus = "unsupported"
	ArchiveDatasetProgressBlocked     ArchiveDatasetProgressStatus = "blocked"
	ArchiveDatasetProgressFailed      ArchiveDatasetProgressStatus = "failed"
	ArchiveDatasetProgressTerminal    ArchiveDatasetProgressStatus = "terminal"
)

// ArchiveLookupOutcome mirrors platform.ArchiveLookupOutcome values.
// internal/db cannot import internal/platform (platform imports db), so the
// outcome is re-declared here with identical string values.
type ArchiveLookupOutcome string

const (
	ArchiveLookupPresent      ArchiveLookupOutcome = "present"
	ArchiveLookupRemoved      ArchiveLookupOutcome = "removed"
	ArchiveLookupMoved        ArchiveLookupOutcome = "moved"
	ArchiveLookupInaccessible ArchiveLookupOutcome = "inaccessible"
)

// ArchiveMergeRequestEvidence is canonical provider evidence that must still
// match storage when archive progress is committed.
type ArchiveMergeRequestEvidence struct {
	Merged         bool
	HeadSHA        string
	MergeCommitSHA string
	FilesChanged   *int
}

// ArchiveItemSyncCommit records the outcome of running the existing full item
// sync for archive hydration. Provider content is already persisted by the
// syncer; this commit advances only archive progress and terminal lifecycle.
type ArchiveItemSyncCommit struct {
	RepoID               int64
	ItemType             ArchiveItemType
	ItemNumber           int
	ScanGeneration       int64
	Outcome              ArchiveLookupOutcome
	Destination          *RepoIdentity
	ErrorCode            string
	ErrorDetail          string
	MergeRequestEvidence *ArchiveMergeRequestEvidence
	Now                  time.Time
}

// ArchiveDatasetProgressKey addresses one dataset progress row.
type ArchiveDatasetProgressKey struct {
	RepoID     int64
	ItemType   ArchiveItemType
	ItemNumber int
	Dataset    ArchiveDataset
}

// ArchiveDatasetProgress is one durable dataset progress row. It carries
// progress metadata only, never provider content.
type ArchiveDatasetProgress struct {
	RepoID          int64
	ItemType        ArchiveItemType
	ItemNumber      int
	Dataset         ArchiveDataset
	ParentRevision  int64
	ScanGeneration  int64
	NextCursor      *string
	LastInputCursor *string
	PageCount       int
	Status          ArchiveDatasetProgressStatus
	ObservedCount   int
	AttemptCount    int
	NextRetryAt     *time.Time
	LastErrorCode   *string
	LastErrorDetail *string
	StartedAt       *time.Time
	CompletedAt     *time.Time
	UpdatedAt       time.Time
}

// StaleDatasetProgressError reports a page or lookup commit whose revision,
// generation, or cursor no longer matches durable dataset progress.
type StaleDatasetProgressError struct {
	RepoID             int64
	ItemType           ArchiveItemType
	ItemNumber         int
	Dataset            ArchiveDataset
	ExpectedRevision   int64
	GotRevision        int64
	ExpectedGeneration int64
	GotGeneration      int64
	ExpectedCursor     string
	GotCursor          string
	// GotStatus is set when the row's status alone rejected the commit (a
	// completed, unsupported, or terminal dataset accepts no page advances).
	GotStatus ArchiveDatasetProgressStatus
}

func (e *StaleDatasetProgressError) Error() string {
	return fmt.Sprintf(
		"stale dataset progress for repo %d %s %d %s: revision %d/%d, generation %d/%d, cursor %q/%q",
		e.RepoID, e.ItemType, e.ItemNumber, e.Dataset,
		e.ExpectedRevision, e.GotRevision,
		e.ExpectedGeneration, e.GotGeneration,
		e.ExpectedCursor, e.GotCursor,
	)
}

// ScanBlockedError reports a scan that stopped spending provider requests.
type ScanBlockedError struct {
	Scope  string
	Reason string
}

func (e *ScanBlockedError) Error() string {
	return fmt.Sprintf("scan blocked for %s: %s", e.Scope, e.Reason)
}

type ArchiveErrorCode string

const (
	ArchiveErrorCodeBudgetExhausted      ArchiveErrorCode = "budget_exhausted"
	ArchiveErrorCodeAuthentication       ArchiveErrorCode = "authentication_failed"
	ArchiveErrorCodeRepoBlocked          ArchiveErrorCode = "repository_blocked"
	ArchiveErrorCodeTransient            ArchiveErrorCode = "transient"
	ArchiveErrorCodeConfigurationRemoved ArchiveErrorCode = "configuration_removed"
)

type ArchiveCoverage string

const (
	ArchiveCoverageUnknown     ArchiveCoverage = "unknown"
	ArchiveCoverageSupported   ArchiveCoverage = "supported"
	ArchiveCoverageUnsupported ArchiveCoverage = "unsupported"
)

type ArchiveCoverageSet struct {
	Issues         ArchiveCoverage
	MergeRequests  ArchiveCoverage
	Comments       ArchiveCoverage
	Reviews        ArchiveCoverage
	InlineComments ArchiveCoverage
}

type ArchiveRefreshReason string

const (
	ArchiveRefreshReasonInitial ArchiveRefreshReason = "initial"
	ArchiveRefreshReasonPrompt  ArchiveRefreshReason = "prompt"
)

type ArchiveRepoState struct {
	RepoID                   int64
	CollectionMode           ArchiveCollectionMode
	OperatorState            ArchiveOperatorState
	IssueInventory           ArchiveScanState
	MergeRequestInventory    ArchiveScanState
	MaintenanceIssues        ArchiveScanState
	MaintenanceMergeRequests ArchiveScanState
	InitialStartedAt         *time.Time
	InitialCompletedAt       *time.Time
	MaintenanceWatermark     *time.Time
	MaintenanceSucceededAt   *time.Time
	PromptScanStartedAt      *time.Time
	PromptSince              *time.Time
	IssuesCoverage           ArchiveCoverage
	MergeRequestsCoverage    ArchiveCoverage
	CommentsCoverage         ArchiveCoverage
	ReviewsCoverage          ArchiveCoverage
	InlineCommentsCoverage   ArchiveCoverage
	LastErrorCode            *string
	LastErrorDetail          *string
	NextRetryAt              *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// Scan returns the durable state of one repository-level scan.
func (s ArchiveRepoState) Scan(kind ArchiveScanKind) ArchiveScanState {
	switch kind {
	case ArchiveScanIssueInventory:
		return s.IssueInventory
	case ArchiveScanMergeRequestInventory:
		return s.MergeRequestInventory
	case ArchiveScanMaintenanceIssues:
		return s.MaintenanceIssues
	case ArchiveScanMaintenanceMergeRequests:
		return s.MaintenanceMergeRequests
	default:
		return ArchiveScanState{}
	}
}

// ArchiveRepoStateNotFoundError reports repository IDs that do not satisfy an
// archive lifecycle operation's durable precondition.
type ArchiveRepoStateNotFoundError struct {
	RepoIDs []int64
}

func (e *ArchiveRepoStateNotFoundError) Error() string {
	return fmt.Sprintf("archive repository state not found for repo IDs %v", e.RepoIDs)
}

type ClaimArchiveItemOpts struct {
	RepoIDs        []int64
	Now            time.Time
	ExcludedScopes []ArchiveItemScope
}

type ArchiveItemScope struct {
	RepoID   int64
	ItemType ArchiveItemType
}

type ArchiveInventoryItem struct {
	Number            int
	ProviderItemID    string
	ProviderCreatedAt time.Time
	ProviderUpdatedAt time.Time
}

type ArchiveInventoryCommit struct {
	RepoID        int64
	ItemType      ArchiveItemType
	RefreshReason ArchiveRefreshReason
	Items         []ArchiveInventoryItem
	// ScanGeneration and InputCursor bind the page to the durable scan row
	// with compare-and-swap semantics: a response from before an explicit
	// reset cannot advance the new generation, and a duplicate delivery of
	// the already-committed page is an idempotent no-op.
	ScanGeneration int64
	InputCursor    string
	NextCursor     string
	Exhausted      bool
	Coverage       ArchiveCoverage
	// InventoryAvailable records that a successful prompt-maintenance read
	// proved a previously unsupported historical stream is available. The
	// page commit reconciles that state under the same operator and scan fence.
	InventoryAvailable bool
	Now                time.Time
}

// ArchiveItemWork is one claimable archive hydration operation.
type ArchiveItemWork struct {
	RepoID            int64
	ItemType          ArchiveItemType
	ItemNumber        int
	ProviderCreatedAt time.Time
	ScanGeneration    int64
	AttemptCount      int
}

type ArchiveStatus string

const (
	ArchiveStatusRunning          ArchiveStatus = "running"
	ArchiveStatusWaitingForBudget ArchiveStatus = "waiting_for_budget"
	ArchiveStatusCurrent          ArchiveStatus = "current"
	ArchiveStatusPartial          ArchiveStatus = "partial"
	ArchiveStatusPaused           ArchiveStatus = "paused"
	ArchiveStatusBlocked          ArchiveStatus = "blocked"
)

type ArchivePhase string

const (
	ArchivePhaseIssueInventory        ArchivePhase = "issue_inventory"
	ArchivePhaseMergeRequestInventory ArchivePhase = "merge_request_inventory"
	ArchivePhaseHydration             ArchivePhase = "hydration"
	ArchivePhasePromptMaintenance     ArchivePhase = "prompt_maintenance"
)

type ArchiveProgressOpts struct {
	RepoIDs []int64
	Now     time.Time
}

type ArchiveProgressCounts struct {
	ItemCount         int
	CompleteItemCount int
	PendingItemCount  int
	FailedItemCount   int
	// UnsupportedItemCount counts items with at least one unsupported dataset.
	UnsupportedItemCount  int
	InaccessibleItemCount int
	DueItemCount          int
	// BlockedItemCount counts active items with at least one blocked dataset scan.
	BlockedItemCount int
}

// ArchiveRepoProgress is derived from durable repository and item state. It is
// intentionally separate from ArchiveRepoState, which represents stored data.
type ArchiveRepoProgress struct {
	RepoID          int64
	Status          ArchiveStatus
	ActivePhases    []ArchivePhase
	Counts          ArchiveProgressCounts
	BudgetWaitUntil *time.Time
}

type RepoSummary struct {
	Repo                 Repo
	CachedPRCount        int
	OpenPRCount          int
	DraftPRCount         int
	CachedIssueCount     int
	OpenIssueCount       int
	MostRecentActivityAt *time.Time
	Overview             RepoOverview
	ActiveAuthors        []RepoActivityAuthor
	RecentIssues         []RepoIssueHeadline
}

type RepoOverview struct {
	LatestRelease       *RepoRelease
	Releases            []RepoRelease
	CommitsSinceRelease *int
	CommitTimeline      []RepoCommitTimelinePoint
	TimelineUpdatedAt   *time.Time
}

type RepoRelease struct {
	TagName         string
	Name            string
	URL             string
	TargetCommitish string
	Prerelease      bool
	PublishedAt     *time.Time
}

type RepoCommitTimelinePoint struct {
	SHA         string
	Message     string
	CommittedAt time.Time
}

type BranchCommit struct {
	RepoID         int64
	BranchName     string
	CommitSHA      string
	AuthorName     string
	AuthorEmail    string
	AuthoredAt     time.Time
	CommitterName  string
	CommitterEmail string
	CommittedAt    time.Time
	Subject        string
	ObservedOrder  int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type BranchTip struct {
	RepoID     int64
	BranchName string
	TipSHA     string
	ObservedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type BranchForcePush struct {
	RepoID           int64
	BranchName       string
	BeforeSHA        string
	AfterSHA         string
	BeforeObservedAt time.Time
	DetectedAt       time.Time
	CreatedAt        time.Time
}

type RepoActivityAuthor struct {
	Login     string
	ItemCount int
}

type RepoIssueHeadline struct {
	Number         int
	Title          string
	Author         string
	State          string
	URL            string
	LastActivityAt time.Time
}

type MergeRequest struct {
	ID                 int64
	SnapshotRevision   int64 `json:"-"`
	RepoID             int64
	PlatformID         int64
	PlatformExternalID string
	Number             int
	URL                string
	Title              string
	Author             string
	AuthorDisplayName  string
	State              MergeRequestState `enum:"open,closed,merged"`
	IsDraft            bool
	IsLocked           bool
	Body               string
	HeadBranch         string
	BaseBranch         string
	// PlatformHeadSHA is exposed so clients can echo the head they
	// rendered back as expected_head_sha on head-bound mutations.
	PlatformHeadSHA  string `json:"platform_head_sha,omitempty"`
	PlatformBaseSHA  string `json:"-"`
	DiffHeadSHA      string `json:"-"`
	DiffBaseSHA      string `json:"-"`
	MergeBaseSHA     string `json:"-"`
	HeadRepoCloneURL string
	// HeadRepoCloneURLUnknown is not a column: it marks HeadRepoCloneURL as
	// undetermined for this snapshot (a failed best-effort enrichment), so
	// the upsert preserves the previously stored value instead of clearing
	// it. An authoritative empty HeadRepoCloneURL leaves it false.
	HeadRepoCloneURLUnknown bool `json:"-"`
	// HeadRepoIdentityStale marks a persisted clone URL observed before its
	// repository row moved to a new provider identity. Best-effort snapshots
	// that cannot observe the head repository leave it set.
	HeadRepoIdentityStale bool `json:"-"`
	Additions             int
	AdditionsKnown        bool `json:"-"`
	Deletions             int
	DeletionsKnown        bool `json:"-"`
	FilesChanged          *int
	MergeCommitSHA        string
	CommentCount          int
	ReviewDecision        string
	CIStatus              string
	CIChecksJSON          string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	LastActivityAt        time.Time
	MergedAt              *time.Time
	ClosedAt              *time.Time
	MergeableState        string
	DetailFetchedAt       *time.Time
	CIHadPending          bool
	// WorkflowApprovalCheckedAt is when kenn-forge last reconciled the
	// workflow-approval state for this merge request. Nil means never
	// checked; the GET path treats persisted state as authoritative
	// only when WorkflowApprovalHeadSHA matches PlatformHeadSHA. Only
	// providers that surface a workflow-approval concept populate
	// these columns; others leave them zero.
	WorkflowApprovalCheckedAt *time.Time   `json:"-"`
	WorkflowApprovalHeadSHA   string       `json:"-"`
	WorkflowApprovalRequired  bool         `json:"-"`
	WorkflowApprovalCount     int          `json:"-"`
	KanbanStatus              KanbanStatus `enum:"new,reviewing,waiting,awaiting_merge"`
	Starred                   bool
	Labels                    []Label `json:"labels,omitempty"`
	// AssigneesJSON and ReviewersJSON store usernames as JSON arrays.
	// Empty string means the provider never reported the field; '[]'
	// records a provider-confirmed empty set.
	AssigneesJSON      string   `json:"-"`
	ReviewersJSON      string   `json:"-"`
	Assignees          []string `json:"assignees,omitempty"`
	RequestedReviewers []string `json:"requested_reviewers,omitempty"`
}

type MergeRequestState string

const (
	MergeRequestStateOpen   MergeRequestState = "open"
	MergeRequestStateClosed MergeRequestState = "closed"
	MergeRequestStateMerged MergeRequestState = "merged"
)

type KanbanStatus string

const (
	KanbanStatusNew           KanbanStatus = "new"
	KanbanStatusReviewing     KanbanStatus = "reviewing"
	KanbanStatusWaiting       KanbanStatus = "waiting"
	KanbanStatusAwaitingMerge KanbanStatus = "awaiting_merge"
)

func (mr MergeRequest) Compare(other MergeRequest) int {
	return cmp.Compare(mr.Number, other.Number)
}

// CICheck represents a single CI check run.
type CICheck struct {
	Name            string `json:"name"`
	Status          string `json:"status"`     // queued, in_progress, completed
	Conclusion      string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required, or empty
	URL             string `json:"url"`        // link to the check run details page
	App             string `json:"app"`        // app name (e.g., "GitHub Actions")
	DurationSeconds *int64 `json:"duration_seconds,omitempty"`
}

func (c CICheck) Compare(other CICheck) int {
	leftFolded := strings.ToLower(c.Name)
	rightFolded := strings.ToLower(other.Name)
	if leftFolded != rightFolded {
		return cmp.Compare(leftFolded, rightFolded)
	}
	return cmp.Compare(c.Name, other.Name)
}

type MREvent struct {
	ID                 int64
	MergeRequestID     int64
	PlatformID         *int64
	PlatformExternalID string
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
	ThreadID     *string
	PositionJSON string
	Resolvable   bool
	Resolved     bool
}

type ReviewLineRange struct {
	Path        string
	OldPath     string
	Side        string
	StartSide   string
	StartLine   *int
	Line        int
	OldLine     *int
	NewLine     *int
	LineType    string
	DiffHeadSHA string
	CommitSHA   string
}

type MRReviewDraft struct {
	ID             int64
	MergeRequestID int64
	Body           string
	Action         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Comments       []MRReviewDraftComment
}

type MRReviewDraftComment struct {
	ID        int64
	DraftID   int64
	Body      string
	Range     ReviewLineRange
	CreatedAt time.Time
	UpdatedAt time.Time
}

type MRReviewDraftCommentInput struct {
	Body  string
	Range ReviewLineRange
}

type MRReviewThread struct {
	ID                int64
	MergeRequestID    int64
	ProviderThreadID  string
	ProviderReviewID  string
	ProviderCommentID string
	Body              string
	AuthorLogin       string
	Range             ReviewLineRange
	Resolved          bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ResolvedAt        *time.Time
	MetadataJSON      string
}

type KanbanState struct {
	MergeRequestID int64
	Status         string
	UpdatedAt      time.Time
}

type ItemWorkflowState struct {
	RepoID        int64
	ItemType      string
	ItemNumber    int
	Status        string
	UpdatedAt     time.Time
	UpdatedSource string
	UpdatedActor  string
	UpdatedReason string
}

type SetItemWorkflowStateResult struct {
	PreviousStatus string
	State          ItemWorkflowState
}

// WorkflowStateConflictError reports an expected-status mismatch on a
// conditional workflow-state write. Current is the effective status at
// write time; a missing row reads as "new".
type WorkflowStateConflictError struct {
	Expected string
	Current  string
}

func (e *WorkflowStateConflictError) Error() string {
	return fmt.Sprintf("workflow state is %q, expected %q", e.Current, e.Expected)
}

type SetItemWorkflowStateParams struct {
	RepoID         int64
	ItemType       string
	ItemNumber     int
	Status         string
	ExpectedStatus string
	Source         string
	Actor          string
	Reason         string
}

type ListWorkflowStatesOpts struct {
	RepoFilters            []RepoFilter
	ItemTypes              []string
	States                 []string
	IncludeClosed          bool
	ExcludeRemovedUpstream bool
	Limit                  int
	Cursor                 string
}

type WorkflowStateListRow struct {
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	Owner          string
	Name           string
	RepoPath       string
	ItemType       string
	Number         int
	Title          string
	State          string
	URL            string
	Author         string
	IsDraft        bool
	LastActivityAt time.Time
	Status         string
	HasRow         bool
	UpdatedAt      *time.Time
	UpdatedSource  string
	UpdatedActor   string
	UpdatedReason  string
}

type ListMergeRequestsOpts struct {
	RepoID            int64
	PlatformHost      string
	RepoOwner         string
	RepoName          string
	RepoPath          string
	RepoFilters       []RepoFilter
	State             string
	KanbanState       string
	Starred           bool
	Unassigned        bool
	Search            string
	Limit             int
	Offset            int
	WorkspaceActivity []ItemActivityOverride
	// ViewerLogins limits results to subjects involving the authenticated
	// viewer in each repository. nil disables the filter; an empty non-nil
	// slice matches no subjects.
	ViewerLogins []RepoViewerLogin
}

type ItemActivityOverride struct {
	RepoID     int64
	ItemNumber int
	ActivityAt time.Time
}

type WorkspaceSubjectKey struct {
	RepoID     int64  `json:"repo_id"`
	ItemType   string `json:"item_type"`
	ItemNumber int    `json:"item_number"`
}

type WorkspaceSubjectMetadata struct {
	Key            WorkspaceSubjectKey
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	RepoOwner      string
	RepoName       string
	RepoPath       string
	Title          string
	State          string
	URL            string
	Author         string
}

// RepoViewerLogin binds a provider-authenticated login to the stable local
// repository identity whose credentials produced it.
type RepoViewerLogin struct {
	RepoID int64
	Login  string
}

type RepoFilter struct {
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	RepoOwner      string
	RepoName       string
	RepoPath       string
}

type Issue struct {
	ID                 int64
	SnapshotRevision   int64 `json:"-"`
	RepoID             int64
	PlatformID         int64
	PlatformExternalID string
	Number             int
	URL                string
	Title              string
	Author             string
	State              string
	Body               string
	CommentCount       int
	LabelsJSON         string `json:"-"`
	AssigneesJSON      string `json:"-"` // JSON array of assignee usernames
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastActivityAt     time.Time
	ClosedAt           *time.Time
	DetailFetchedAt    *time.Time
	Starred            bool
	WorkflowStatus     KanbanStatus `enum:"new,reviewing,waiting,awaiting_merge"`
	Labels             []Label      `json:"labels,omitempty"`
	Assignees          []string     `json:"assignees,omitempty"` // Parsed assignees
}

type IssueEvent struct {
	ID                 int64
	IssueID            int64
	PlatformID         *int64
	PlatformExternalID string
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
	ThreadID *string
}

type CommentAutocompleteReference struct {
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

type ListIssuesOpts struct {
	PlatformHost      string
	RepoOwner         string
	RepoName          string
	RepoPath          string
	RepoFilters       []RepoFilter
	State             string
	Starred           bool
	Unassigned        bool
	Search            string
	Assignee          string
	Limit             int
	Offset            int
	WorkspaceActivity []ItemActivityOverride
	ViewerLogins      []RepoViewerLogin
	ReferencedByPR    bool
}

type Notification struct {
	ID                       int64
	Platform                 string
	PlatformHost             string
	PlatformNotificationID   string
	RepoID                   *int64
	RepoOwner                string
	RepoName                 string
	SubjectType              string
	SubjectTitle             string
	SubjectURL               string
	SubjectLatestCommentURL  string
	WebURL                   string
	ItemNumber               *int
	ItemType                 string
	ItemAuthor               string
	Reason                   string
	Unread                   bool
	Participating            bool
	SourceUpdatedAt          time.Time
	SourceLastAcknowledgedAt *time.Time
	SyncedAt                 time.Time
	DoneAt                   *time.Time
	DoneReason               string
	SourceAckQueuedAt        *time.Time
	SourceAckSyncedAt        *time.Time
	SourceAckGenerationAt    *time.Time
	SourceAckError           string
	SourceAckAttempts        int
	SourceAckLastAttemptAt   *time.Time
	SourceAckNextAttemptAt   *time.Time
}

// MergeRequestNotificationActivity is the newest notification timestamp
// linked to one open merge request. It is a scheduling hint, not authoritative
// merge-request activity.
type MergeRequestNotificationActivity struct {
	MergeRequestID  int64
	SourceUpdatedAt time.Time
}

type ListNotificationsOpts struct {
	Platform     string
	PlatformHost string
	RepoOwner    string
	RepoName     string
	Repos        []NotificationRepoFilter
	State        string
	Reasons      []string
	ItemTypes    []string
	Search       string
	Sort         string
	Limit        int
	Offset       int
}

type NotificationRepoFilter struct {
	Platform     string
	PlatformHost string
	RepoOwner    string
	RepoName     string
}

type NotificationSummary struct {
	TotalActive int
	Unread      int
	Done        int
	ByReason    map[string]int
	ByRepo      map[string]int
}

// NotificationSyncWatermark tracks notification sync progress per repository
// identity so one unavailable credential route cannot block watermark
// advancement for healthy repositories sharing the host.
type NotificationSyncWatermark struct {
	Platform             string
	PlatformHost         string
	RepoOwner            string
	RepoName             string
	LastSuccessfulSyncAt time.Time
	LastFullSyncAt       *time.Time
}

// WorktreeLink associates a merge request with an external worktree.
type WorktreeLink struct {
	ID             int64
	MergeRequestID int64
	WorktreeKey    string
	WorktreePath   string
	WorktreeBranch string
	LinkedAt       time.Time
}

// RateLimit tracks per-host API rate limit state.
type RateLimit struct {
	ID            int64
	Platform      string
	PlatformHost  string
	RatePrincipal string
	APIType       string
	RequestsHour  int
	HourStart     time.Time
	RateRemaining int
	RateLimit     int
	RateResetAt   *time.Time
	UpdatedAt     time.Time
}

// ActivityItem represents one row in the unified activity feed.
type ActivityItem struct {
	ActivityType   string // new_pr, new_issue, comment, review, commit, default_branch_*
	Source         string // pr, issue, pre, ise, bc, bfp
	SourceID       int64  // PK from the source table
	RepoID         int64
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	RepoOwner      string
	RepoName       string
	RepoPath       string
	ItemType       string // pr, issue, or empty for repo-level activity
	ItemNumber     int
	ItemTitle      string
	ItemURL        string
	ItemState      string // open, merged, closed
	Author         string
	// ItemAuthor is the author of the parent PR/issue, carried on every
	// PR/issue row (open, comment, review, commit, force_push) so the
	// threaded feed can show the item's author rather than the latest
	// actor. Empty for repo-level rows (branch commits/force pushes).
	ItemAuthor     string
	CreatedAt      time.Time
	BodyPreview    string
	BranchName     string
	CommitSHA      string
	BeforeSHA      string
	AfterSHA       string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	AuthoredAt     *time.Time
	CommittedAt    *time.Time
	ActivityURL    string
	// ItemLastActivityAt is the parent PR or issue's Activity recency, derived
	// from the event ledger the feed can render rather than provider
	// updated_at. It is independent of Activity filters, which may hide the
	// event responsible for that timestamp.
	ItemLastActivityAt *time.Time
	// SubjectState is the open/closed/merged state of a notification row's
	// linked PR/issue. Empty for non-notification rows, which carry their
	// own state in ItemState. Lets the feed apply Hide closed/merged to
	// notifications, whose ItemState holds unread/read instead.
	SubjectState string
}

// ActivitySubject is the authoritative parent projection for one pull request
// or issue in the Activity time range. It is independent of event visibility.
type ActivitySubject struct {
	Subject             WorkspaceSubjectMetadata
	ActivityAt          time.Time
	EventLedgerRevision string
}

// Stack represents a detected chain of dependent PRs.
type Stack struct {
	ID         int64
	RepoID     int64
	BaseNumber int
	Name       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// StackMember links a merge request to a stack with a position.
type StackMember struct {
	StackID        int64
	MergeRequestID int64
	Position       int
}

// StackWithRepo extends Stack with resolved repo owner/name.
type StackWithRepo struct {
	Stack
	RepoOwner string
	RepoName  string
}

// StackMemberWithPR combines stack membership with PR fields needed for display.
type StackMemberWithPR struct {
	StackID        int64
	MergeRequestID int64
	Position       int
	Number         int
	Title          string
	State          string
	CIStatus       string
	ReviewDecision string
	IsDraft        bool
	BaseBranch     string
	MergeableState string
}

// StackPlacement is a merge request's contiguous position within its stack
// after hidden members are filtered, plus the visible stack size.
type StackPlacement struct {
	Position int
	Size     int
}

// GitHubNativeStack is a cached GitHub stack resource. It remains separate
// from Stack, which is the provider-neutral projection served to the UI.
type GitHubNativeStack struct {
	ID                 int64
	RepoID             int64
	GitHubID           int64
	Number             int
	Size               int
	BaseRef            string
	IsOpen             bool
	GitHubCreatedAt    time.Time
	ContentFingerprint string
	LastObservedAt     time.Time
	Members            []GitHubNativeStackMember
}

// GitHubNativeStackMember is one bottom-to-top member of a cached native stack.
type GitHubNativeStackMember struct {
	StackID           int64
	Position          int
	PullRequestNumber int
	State             string
	Draft             bool
	MergedAt          *time.Time
	HeadRef           string
	HeadSHA           string
}

const (
	WorkspaceItemTypePullRequest = "pull_request"
	WorkspaceItemTypeIssue       = "issue"
	WorkspaceItemTypeKataTask    = "kata_task"
	// WorkspaceItemTypeAdHoc is new work started from a tracked repository
	// with no provider item and no Kata task behind it. Its identity is the
	// branch, carried in item_key; item_number is always 0.
	WorkspaceItemTypeAdHoc = "adhoc"
)

// AdHocWorkspaceItemKey returns the workspace item key that makes an ad-hoc
// workspace unique within a repository. The branch is the only identity such a
// workspace has, so it is the key.
func AdHocWorkspaceItemKey(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return "adhoc:" + branch
}

type WorkspaceKataMetadata struct {
	DaemonID    string `json:"daemon_id"`
	ProjectUID  string `json:"project_uid"`
	ProjectName string `json:"project_name,omitempty"`
	IssueUID    string `json:"issue_uid"`
	ShortID     string `json:"short_id,omitempty"`
	QualifiedID string `json:"qualified_id,omitempty"`
	Title       string `json:"title,omitempty"`
}

func KataWorkspaceItemKey(metadata WorkspaceKataMetadata) string {
	daemonID := strings.TrimSpace(metadata.DaemonID)
	projectUID := strings.TrimSpace(metadata.ProjectUID)
	issueUID := strings.TrimSpace(metadata.IssueUID)
	if daemonID == "" || projectUID == "" || issueUID == "" {
		return ""
	}
	return strings.Join([]string{
		"kata",
		kataWorkspaceItemKeyPart(daemonID),
		kataWorkspaceItemKeyPart(projectUID),
		kataWorkspaceItemKeyPart(issueUID),
	}, ":")
}

func kataWorkspaceItemKeyPart(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// Workspace represents a kenn-forge-managed git worktree linked to a
// pull request, provider issue, or external Kata task.
type Workspace struct {
	ID                 string
	Platform           string
	PlatformHost       string
	RepoOwner          string
	RepoName           string
	RepoID             int64
	ItemType           string
	ItemNumber         int
	ItemKey            string
	AssociatedPRNumber *int
	GitHeadRef         string
	// MRHeadRepo is nil for confirmed same-repo PRs, non-nil empty when
	// repository identity is unknown, and non-nil with a clone URL for forks.
	MRHeadRepo *string
	// WorkspaceBranch is the exact branch name checked out in the
	// worktree after setup. Before setup completes it may contain the
	// requested branch name or workspaceBranchUnknown.
	WorkspaceBranch string
	WorktreePath    string
	TmuxSession     string
	TerminalBackend string
	Status          string // "creating", "ready", "error", "deleting", "deletion_failed"
	ErrorMessage    *string
	CreatedAt       time.Time
	KataMetadata    *WorkspaceKataMetadata
}

const (
	WorkspaceLaunchSpecVersion          = 1
	WorkspaceLaunchSpecMigrationVersion = 54
	WorkspaceLaunchSpecVisibilityLease  = 15 * time.Minute
)

var (
	ErrLaunchSpecRefreshRequired = errors.New("workspace launch specification requires hub refresh")
	ErrLaunchSpecSourceHidden    = errors.New("workspace launch source is not visible")
)

type WorkspaceLaunchRepository struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	CloneURL       string `json:"clone_url"`
	DefaultBranch  string `json:"default_branch"`
}

type WorkspaceLaunchPull struct {
	HeadBranch       string `json:"head_branch"`
	HeadRepoKind     string `json:"head_repo_kind" enum:"same_repo,fork,unknown"`
	HeadRepoCloneURL string `json:"head_repo_clone_url"`
	SnapshotRevision int64  `json:"snapshot_revision" minimum:"1"`
}

// WorkspaceLaunchSpec is the immutable provider fact set used by every
// provider-backed workspace lifecycle operation. Request intent such as branch
// reuse and worktree placement deliberately does not belong here.
type WorkspaceLaunchSpec struct {
	Version            int                       `json:"version"`
	Repository         WorkspaceLaunchRepository `json:"repository"`
	ItemType           string                    `json:"item_type" enum:"pull_request,issue"`
	ItemNumber         int                       `json:"item_number" minimum:"1"`
	ItemKey            string                    `json:"item_key"`
	GitHeadRef         string                    `json:"git_head_ref"`
	SourceTitle        string                    `json:"source_title"`
	SourceURL          string                    `json:"source_url"`
	Pull               *WorkspaceLaunchPull      `json:"pull,omitempty"`
	SourceVisible      bool                      `json:"source_visible"`
	SourceVisibleUntil time.Time                 `json:"source_visible_until"`
	IssuedAt           time.Time                 `json:"issued_at"`
}

func (spec WorkspaceLaunchSpec) Validate() error {
	if spec.Version != WorkspaceLaunchSpecVersion {
		return fmt.Errorf("workspace launch specification version must be %d", WorkspaceLaunchSpecVersion)
	}
	repositoryFields := []struct {
		name  string
		value string
	}{
		{name: "provider", value: spec.Repository.Provider},
		{name: "platform_host", value: spec.Repository.PlatformHost},
		{name: "platform_repo_id", value: spec.Repository.PlatformRepoID},
		{name: "owner", value: spec.Repository.Owner},
		{name: "name", value: spec.Repository.Name},
		{name: "clone_url", value: spec.Repository.CloneURL},
		{name: "default_branch", value: spec.Repository.DefaultBranch},
	}
	for _, field := range repositoryFields {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("workspace launch repository %s is required", field.name)
		}
	}
	if spec.ItemType != WorkspaceItemTypePullRequest &&
		spec.ItemType != WorkspaceItemTypeIssue {
		return fmt.Errorf("workspace launch item type must be %q or %q", WorkspaceItemTypePullRequest, WorkspaceItemTypeIssue)
	}
	if spec.ItemNumber <= 0 {
		return errors.New("workspace launch item number must be positive")
	}
	if strings.TrimSpace(spec.ItemKey) == "" {
		return errors.New("workspace launch item key is required")
	}
	if spec.ItemKey != strconv.Itoa(spec.ItemNumber) {
		return errors.New("workspace launch item key must match the item number")
	}
	if strings.TrimSpace(spec.GitHeadRef) == "" {
		return errors.New("workspace launch git head ref is required")
	}
	if spec.ItemType == WorkspaceItemTypeIssue {
		if spec.Pull != nil {
			return errors.New("issue workspace launch specification cannot contain pull metadata")
		}
	} else {
		if spec.Pull == nil {
			return errors.New("pull-request workspace launch specification requires pull metadata")
		}
		if strings.TrimSpace(spec.Pull.HeadBranch) == "" {
			return errors.New("workspace launch pull head branch is required")
		}
		if spec.Pull.SnapshotRevision <= 0 {
			return errors.New("workspace launch pull snapshot revision must be positive")
		}
		switch spec.Pull.HeadRepoKind {
		case "same_repo", "unknown":
			if strings.TrimSpace(spec.Pull.HeadRepoCloneURL) != "" {
				return fmt.Errorf("%s pull head cannot include a clone URL", spec.Pull.HeadRepoKind)
			}
		case "fork":
			if strings.TrimSpace(spec.Pull.HeadRepoCloneURL) == "" {
				return errors.New("fork pull head clone URL is required")
			}
		default:
			return errors.New("workspace launch pull head repository kind is invalid")
		}
	}
	if spec.IssuedAt.IsZero() || spec.SourceVisibleUntil.IsZero() {
		return errors.New("workspace launch visibility lease timestamps are required")
	}
	issuedAt := spec.IssuedAt.UTC()
	visibleUntil := spec.SourceVisibleUntil.UTC()
	if !visibleUntil.Equal(issuedAt.Add(WorkspaceLaunchSpecVisibilityLease)) {
		return fmt.Errorf("workspace launch visibility lease must be %s", WorkspaceLaunchSpecVisibilityLease)
	}
	return nil
}

func (spec WorkspaceLaunchSpec) ValidateWorkspace(workspace Workspace) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if workspace.ID == "" {
		return errors.New("workspace ID is required")
	}
	routeChanged := !strings.EqualFold(workspace.RepoOwner, spec.Repository.Owner) ||
		!strings.EqualFold(workspace.RepoName, spec.Repository.Name)
	if !strings.EqualFold(canonicalWorkspacePlatform(workspace.Platform), strings.TrimSpace(spec.Repository.Provider)) ||
		!strings.EqualFold(strings.TrimSpace(workspace.PlatformHost), strings.TrimSpace(spec.Repository.PlatformHost)) ||
		(workspace.RepoID == 0 && routeChanged) ||
		workspace.ItemType != spec.ItemType || workspace.ItemNumber != spec.ItemNumber ||
		workspaceItemKeyForComparison(workspace) != spec.ItemKey ||
		strings.TrimSpace(workspace.GitHeadRef) != strings.TrimSpace(spec.GitHeadRef) {
		return errors.New("workspace launch specification does not match workspace identity")
	}
	return nil
}

func workspaceItemKeyForComparison(workspace Workspace) string {
	if key := strings.TrimSpace(workspace.ItemKey); key != "" {
		return key
	}
	return strconv.Itoa(workspace.ItemNumber)
}

func (spec WorkspaceLaunchSpec) RequireVisible(now time.Time) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if !spec.SourceVisible {
		return ErrLaunchSpecSourceHidden
	}
	if !now.UTC().Before(spec.SourceVisibleUntil.UTC()) {
		return ErrLaunchSpecRefreshRequired
	}
	return nil
}

type UnpreparedWorkspace struct {
	Workspace      Workspace `json:"workspace"`
	Reason         string    `json:"reason"`
	PlatformRepoID string    `json:"-"`
}

// WorkspaceSummary extends Workspace with joined source-item metadata.
type WorkspaceSummary struct {
	Workspace
	RepoID         int64
	RepoPlatformID string
	// SourceItemVisible and AssociatedPRVisible are false only at the public
	// removed-upstream boundary. Inaccessible and not-yet-synced items remain
	// visible by number, matching the rest of the public read contract.
	SourceItemVisible   bool
	AssociatedPRVisible bool
	SourceTitle         *string
	SourceState         *string
	SourceURL           *string
	MRTitle             *string
	MRState             *string
	MRIsDraft           *bool
	MRCIStatus          *string
	MRReviewDecision    *string
	MRAdditions         *int
	MRDeletions         *int
	MRCommentCount      *int
	MRMergeableState    *string
	// MRHeadBranch is the currently synced PR head branch, which can move
	// after workspace creation (branch rename); prefer it over the
	// creation-time GitHeadRef snapshot for push-target context.
	MRHeadBranch       *string
	ItemLastActivityAt *time.Time
}

type WorkspaceSetupEvent struct {
	ID          int64
	WorkspaceID string
	Stage       string
	Outcome     string
	Message     string
	CreatedAt   time.Time
}

type WorkspaceRuntimeSession struct {
	WorkspaceID   string
	SessionKey    string
	TargetKey     string
	Label         string
	Kind          string
	DisplayRegion string
	Scope         string
	TmuxSession   string
	CreatedAt     time.Time
}

// ListActivityOpts holds filters and pagination for the activity feed.
type ListActivityOpts struct {
	Repo           string       // "owner/name" filter
	RepoFilters    []RepoFilter // one or more repository filters
	AllowedRepoIDs []int64      // caller-visible stable repository scope
	Types          []string     // activity type filter
	// ItemTypes filters item-scoped rows before ordering and limiting.
	// "repo" selects rows without a PR or issue parent.
	ItemTypes []string
	Search    string // title/body search
	Author    string // exact, case-insensitive PR or issue author filter
	// Unassigned limits rows to PR and issue subjects with no assignees.
	// Repository-only rows never match.
	Unassigned bool
	// ViewerLogins limits rows to PR and issue subjects involving the
	// authenticated viewer. Repository-only rows never match.
	ViewerLogins []RepoViewerLogin
	// ExcludeNotifications drops notification rows from the union before
	// ordering/limit. Notifications are always enabled in normal operation;
	// the server only sets this when no config is loaded (nil-config
	// nil-safety), so the safety-capped window is never spent on rows the
	// caller will not serve.
	ExcludeNotifications bool
	HideClosedMerged     bool
	HideBots             bool
	HideDefaultBranch    bool
	ParentRepoID         int64
	ParentItemType       string
	ParentItemNumber     int
	// DirectProjectionOnly selects rows that cannot be represented by the
	// filtered parent projection, including repository/unparented rows and
	// visible human events whose bot-authored parent is hidden.
	DirectProjectionOnly bool
	// NotificationRepoFilters limits notification rows to the caller's
	// current monitored repo set before ordering/limit. nil means no
	// additional notification scope; an empty/non-matching set means no
	// notification rows.
	NotificationRepoFilters []NotificationRepoFilter
	Limit                   int        // page size (default 50, max 200)
	Since                   *time.Time // only return events created at or after this time
	// Cursor fields -- decoded from opaque token by the handler.
	BeforeTime         *time.Time
	BeforeSource       string
	BeforeSourceID     int64
	AfterTime          *time.Time
	AfterSource        string
	AfterSourceID      int64
	AtOrBeforeTime     *time.Time
	AtOrBeforeSource   string
	AtOrBeforeSourceID int64
}

// ListActivitySubjectsOpts holds parent-level filters for the Activity feed.
// Event-type filters and cursors do not apply to this authoritative snapshot.
type ListActivitySubjectsOpts struct {
	Repo           string
	RepoFilters    []RepoFilter
	AllowedRepoIDs []int64
	ItemTypes      []string
	// ExcludeNotificationRecency prevents hidden notification rows from
	// admitting or reordering otherwise-old parents.
	ExcludeNotificationRecency bool
	Search                     string
	// SearchMatchedSubjectKeys identifies parents whose provider events matched
	// Search. The parent title and author do not necessarily contain that term.
	SearchMatchedSubjectKeys []WorkspaceSubjectKey
	Author                   string
	Unassigned               bool
	ViewerLogins             []RepoViewerLogin
	HideClosedMerged         bool
	HideBots                 bool
	Limit                    int
	Since                    *time.Time
}

type ActivityProjection struct {
	DirectRows  []ActivityItem
	Subjects    []ActivitySubject
	EventCursor string
}

type ListActivityProjectionOpts struct {
	ListActivityOpts
	SubjectLimit     int
	SearchEventLimit int
}

// ListActivityAuthorsOpts holds the repository and time scopes used to build
// the activity author's typeahead candidates. Feed-only filters such as search
// and activity type intentionally do not participate.
type ListActivityAuthorsOpts struct {
	RepoFilters             []RepoFilter
	AllowedRepoIDs          []int64
	ExcludeNotifications    bool
	NotificationRepoFilters []NotificationRepoFilter
	Since                   *time.Time
}
