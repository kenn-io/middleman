package pullapi

import (
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
)

type worktreeLinkResponse struct {
	HostKey        string `json:"host_key,omitempty"`
	WorktreeKey    string `json:"worktree_key"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
}

type MergeRequestResponse struct {
	db.MergeRequest
	Repo                    httpapi.RepoRefResponse    `json:"repo"`
	RepoOwner               string                     `json:"repo_owner"`
	RepoName                string                     `json:"repo_name"`
	PlatformHost            string                     `json:"platform_host"`
	WorktreeLinks           []worktreeLinkResponse     `json:"worktree_links"`
	Workspace               *workspaceapi.WorkspaceRef `json:"workspace,omitempty"`
	LastWorkspaceActivityAt string                     `json:"last_workspace_activity_at,omitempty" format:"date-time"`
	DetailLoaded            bool                       `json:"detail_loaded"`
	DetailFetchedAt         string                     `json:"detail_fetched_at,omitempty"`
	// Stack is present only when the pull request belongs to a stack with more
	// than one visible member, so list rows can show a stack indicator without
	// loading the full stack context.
	Stack *StackPlacementResponse `json:"stack,omitempty"`
}

// StackPlacementResponse is a pull request's position within its stack.
type StackPlacementResponse struct {
	Position int `json:"position" doc:"1-based position of this pull request in its stack"`
	Size     int `json:"size" doc:"Number of visible pull requests in the stack"`
}

type mergeRequestEventResponse struct {
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
	ThreadID           *string
	Resolvable         bool
	Resolved           bool
	DiffThread         *diffReviewThreadResponse `json:"diff_thread,omitempty"`
}

type workflowApprovalResponse struct {
	Checked  bool `json:"checked"`
	Required bool `json:"required"`
	Count    int  `json:"count"`
}

type HeadRepoKind string

const (
	HeadRepoKindSameRepo HeadRepoKind = "same_repo"
	HeadRepoKindFork     HeadRepoKind = "fork"
	HeadRepoKindUnknown  HeadRepoKind = "unknown"
)

type MergeRequestDetailResponse struct {
	MergeRequest     *db.MergeRequest            `json:"merge_request"`
	Events           []mergeRequestEventResponse `json:"events"`
	Repo             httpapi.RepoRefResponse     `json:"repo"`
	RepoOwner        string                      `json:"repo_owner"`
	RepoName         string                      `json:"repo_name"`
	PlatformHost     string                      `json:"platform_host"`
	PlatformHeadSHA  string                      `json:"platform_head_sha"`
	HeadRepoKind     HeadRepoKind                `json:"head_repo_kind" enum:"same_repo,fork,unknown"`
	PlatformBaseSHA  string                      `json:"platform_base_sha"`
	ReviewedHeadSHA  string                      `json:"reviewed_head_sha"`
	DiffHeadSHA      string                      `json:"diff_head_sha"`
	MergeBaseSHA     string                      `json:"merge_base_sha"`
	WorktreeLinks    []worktreeLinkResponse      `json:"worktree_links"`
	WorkflowApproval workflowApprovalResponse    `json:"workflow_approval"`
	Warnings         []string                    `json:"warnings,omitempty"`
	DetailLoaded     bool                        `json:"detail_loaded"`
	// DeferredMergePending reports whether a background "merge after CI"
	// worker is currently waiting on this pull request in this server
	// process, so the UI can show the queued state instead of a merge
	// action.
	DeferredMergePending bool                       `json:"deferred_merge_pending"`
	DetailFetchedAt      string                     `json:"detail_fetched_at,omitempty"`
	Workspace            *workspaceapi.WorkspaceRef `json:"workspace,omitempty"`
	Stack                *stackContextResponse      `json:"stack,omitempty"`
	// Checks is the merge request's CI checks decoded from its cached
	// ci_checks_json. Omitted when the merge request has no cached checks.
	Checks []db.CICheck `json:"checks,omitempty"`
}

var validKanbanStates = map[string]bool{
	"new":            true,
	"reviewing":      true,
	"waiting":        true,
	"awaiting_merge": true,
}

type diffResponse = httpapi.DiffResponse
type filesResponse = httpapi.FilesResponse

type diffReviewLineRange struct {
	Path        string `json:"path"`
	OldPath     string `json:"old_path,omitempty"`
	Side        string `json:"side"`
	StartSide   string `json:"start_side,omitempty"`
	StartLine   *int   `json:"start_line,omitempty"`
	Line        int    `json:"line"`
	OldLine     *int   `json:"old_line,omitempty"`
	NewLine     *int   `json:"new_line,omitempty"`
	LineType    string `json:"line_type"`
	DiffHeadSHA string `json:"diff_head_sha,omitempty"`
	CommitSHA   string `json:"commit_sha,omitempty"`
}

type diffReviewDraftComment struct {
	ID          string `json:"id"`
	Body        string `json:"body"`
	Path        string `json:"path"`
	OldPath     string `json:"old_path,omitempty"`
	Side        string `json:"side"`
	StartSide   string `json:"start_side,omitempty"`
	StartLine   *int   `json:"start_line,omitempty"`
	Line        int    `json:"line"`
	OldLine     *int   `json:"old_line,omitempty"`
	NewLine     *int   `json:"new_line,omitempty"`
	LineType    string `json:"line_type"`
	DiffHeadSHA string `json:"diff_head_sha,omitempty"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type diffReviewDraftResponse struct {
	DraftID               string                   `json:"draft_id,omitempty"`
	Comments              []diffReviewDraftComment `json:"comments"`
	SupportedActions      []string                 `json:"supported_actions"`
	NativeMultilineRanges bool                     `json:"native_multiline_ranges"`
}

type diffReviewThreadResponse struct {
	ID                string `json:"id"`
	ProviderCommentID string `json:"provider_comment_id,omitempty"`
	Path              string `json:"path"`
	OldPath           string `json:"old_path,omitempty"`
	Side              string `json:"side"`
	StartSide         string `json:"start_side,omitempty"`
	StartLine         *int   `json:"start_line,omitempty"`
	Line              int    `json:"line"`
	OldLine           *int   `json:"old_line,omitempty"`
	NewLine           *int   `json:"new_line,omitempty"`
	LineType          string `json:"line_type"`
	DiffHeadSHA       string `json:"diff_head_sha,omitempty"`
	CommitSHA         string `json:"commit_sha,omitempty"`
	Body              string `json:"body"`
	MetadataJSON      string `json:"metadata_json,omitempty"`
	AuthorLogin       string `json:"author_login,omitempty"`
	Resolved          bool   `json:"resolved"`
	CanResolve        bool   `json:"can_resolve"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type filePreviewResponse = httpapi.FilePreviewResponse

type mrImportMetadataResponse struct {
	Number           int    `json:"number"`
	HeadBranch       string `json:"head_branch"`
	PlatformHeadSHA  string `json:"platform_head_sha"`
	HeadRepoCloneURL string `json:"head_repo_clone_url"`
	State            string `json:"state"`
	IsDraft          bool   `json:"is_draft"`
	Title            string `json:"title"`
}

type commitResponse = httpapi.CommitResponse
type commitsResponse = httpapi.CommitsResponse

type stackMemberResponse struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	CIStatus       string `json:"ci_status"`
	ReviewDecision string `json:"review_decision"`
	MergeableState string `json:"mergeable_state"`
	Position       int    `json:"position"`
	IsDraft        bool   `json:"is_draft"`
	BaseBranch     string `json:"base_branch"`
	BlockedBy      *int   `json:"blocked_by"`
}

type stackResponse struct {
	ID        int64                 `json:"id"`
	Name      string                `json:"name"`
	RepoOwner string                `json:"repo_owner"`
	RepoName  string                `json:"repo_name"`
	Health    string                `json:"health"`
	Members   []stackMemberResponse `json:"members"`
}

type stackContextResponse struct {
	StackID   int64                 `json:"stack_id"`
	StackName string                `json:"stack_name"`
	Position  int                   `json:"position"`
	Size      int                   `json:"size"`
	Health    string                `json:"health"`
	Members   []stackMemberResponse `json:"members"`
}
