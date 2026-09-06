package httpapi

type ProviderCapabilitiesResponse struct {
	ReadRepositories            bool     `json:"read_repositories"`
	ReadMergeRequests           bool     `json:"read_merge_requests"`
	ReadIssues                  bool     `json:"read_issues"`
	ReadIssuePRReferences       bool     `json:"read_issue_pr_references"`
	ReadComments                bool     `json:"read_comments"`
	ReadReleases                bool     `json:"read_releases"`
	ReadCI                      bool     `json:"read_ci"`
	ReadWorkflows               bool     `json:"read_workflows"`
	ReadWorkflowRuns            bool     `json:"read_workflow_runs"`
	ReadLabels                  bool     `json:"read_labels"`
	ReadMarkdownImages          bool     `json:"read_markdown_images"`
	ReadAuthenticatedUser       bool     `json:"read_authenticated_user"`
	CommentMutation             bool     `json:"comment_mutation"`
	StateMutation               bool     `json:"state_mutation"`
	MergeMutation               bool     `json:"merge_mutation"`
	ReviewMutation              bool     `json:"review_mutation"`
	WorkflowApproval            bool     `json:"workflow_approval"`
	WorkflowDispatch            bool     `json:"workflow_dispatch"`
	ReadyForReview              bool     `json:"ready_for_review"`
	DraftMutation               bool     `json:"draft_mutation"`
	IssueMutation               bool     `json:"issue_mutation"`
	LabelMutation               bool     `json:"label_mutation"`
	AssigneeMutation            bool     `json:"assignee_mutation"`
	ReviewerMutation            bool     `json:"reviewer_mutation"`
	ThreadReply                 bool     `json:"thread_reply"`
	ThreadResolve               bool     `json:"thread_resolve"`
	ReviewDraftMutation         bool     `json:"review_draft_mutation"`
	ReviewThreadResolution      bool     `json:"review_thread_resolution"`
	ReviewSuggestionApplication bool     `json:"review_suggestion_application"`
	ReadReviewThreads           bool     `json:"read_review_threads"`
	NativeMultilineRanges       bool     `json:"native_multiline_ranges"`
	MutationHeadBinding         bool     `json:"mutation_head_binding"`
	SupportedReviewActions      []string `json:"supported_review_actions"`
}

type OperationAvailability struct {
	Available          bool   `json:"available"`
	Code               string `json:"code,omitempty"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
	RequiredCapability string `json:"required_capability,omitempty"`
	RetryAt            string `json:"retry_at,omitempty"`
}

type RepoOperations struct {
	MergePR               OperationAvailability `json:"merge_pr"`
	ClosePR               OperationAvailability `json:"close_pr"`
	ReopenPR              OperationAvailability `json:"reopen_pr"`
	MarkReadyForReview    OperationAvailability `json:"mark_ready_for_review"`
	MarkDraft             OperationAvailability `json:"mark_draft"`
	SubmitReview          OperationAvailability `json:"submit_review"`
	ReviewDraft           OperationAvailability `json:"review_draft"`
	AddComment            OperationAvailability `json:"add_comment"`
	EditComment           OperationAvailability `json:"edit_comment"`
	DeleteComment         OperationAvailability `json:"delete_comment"`
	AddLabel              OperationAvailability `json:"add_label"`
	RemoveLabel           OperationAvailability `json:"remove_label"`
	SetAssignees          OperationAvailability `json:"set_assignees"`
	SetReviewers          OperationAvailability `json:"set_reviewers"`
	CreateIssue           OperationAvailability `json:"create_issue"`
	CloseIssue            OperationAvailability `json:"close_issue"`
	ReopenIssue           OperationAvailability `json:"reopen_issue"`
	ApproveWorkflow       OperationAvailability `json:"approve_workflow"`
	DispatchWorkflow      OperationAvailability `json:"dispatch_workflow"`
	UpdateContent         OperationAvailability `json:"update_content"`
	ReplyReviewThread     OperationAvailability `json:"reply_review_thread"`
	ResolveReviewThread   OperationAvailability `json:"resolve_review_thread"`
	ApplyReviewSuggestion OperationAvailability `json:"apply_review_suggestion"`
}

type RepoRefResponse struct {
	Provider       string                       `json:"provider"`
	PlatformHost   string                       `json:"platform_host"`
	PlatformRepoID string                       `json:"platform_repo_id,omitempty"`
	RepoPath       string                       `json:"repo_path"`
	Owner          string                       `json:"owner"`
	Name           string                       `json:"name"`
	DefaultBranch  string                       `json:"default_branch,omitempty"`
	Capabilities   ProviderCapabilitiesResponse `json:"capabilities"`
	Operations     *RepoOperations              `json:"operations,omitempty"`
}
