package mcpserver

import (
	"context"
	"time"
)

// ProviderBackend owns provider data shared by every Forge node.
type ProviderBackend interface {
	ListRepositories(context.Context) ([]RepositorySummary, error)
	ListActivity(context.Context, ActivityQuery) (ActivityPage, error)
	ListPulls(context.Context, ItemListQuery) ([]Pull, error)
	ListIssues(context.Context, ItemListQuery) ([]Issue, error)
	GetPull(context.Context, ItemIdentity) (PullDetail, error)
	GetIssue(context.Context, ItemIdentity) (IssueDetail, error)
	GetPullStack(context.Context, ItemIdentity) (Stack, error)
	ListWorkflowStates(context.Context, WorkflowQuery) (WorkflowPage, error)
	SetWorkflowState(context.Context, ItemIdentity, WorkflowUpdate) (WorkflowMutation, error)
}

// LocalBackend owns data and execution that belong to the connected node.
type LocalBackend interface {
	GetPullDiff(context.Context, ItemIdentity, bool) (Diff, error)
	ListLaunchTargets(context.Context) ([]LaunchTarget, error)
	PreferredWorkspaceAgentTarget(context.Context, time.Time, []string) (string, bool, error)
	ListWorkspaceAgentSessions(context.Context, string) ([]WorkspaceAgentSession, error)
	GetWorkspace(context.Context, string) (Workspace, error)
	CreatePullWorkspace(context.Context, ItemIdentity, bool) (Workspace, error)
	CreateIssueWorkspace(context.Context, ItemIdentity, bool) (Workspace, error)
	CreateAdHocWorkspace(context.Context, RepositoryIdentity, string) (Workspace, error)
	LaunchWorkspaceRuntime(context.Context, string, string) (RuntimeSession, error)
	GetWorkspaceRuntime(context.Context, string) (WorkspaceRuntime, error)
	SubmitAgentMessage(context.Context, AgentMessageRequest) (AgentMessageResult, error)
	SubmitInitialMessage(context.Context, InitialMessageRequest) (InitialMessageStatus, error)
	GetInitialMessage(context.Context, string, string) (InitialMessageStatus, error)
}

// Backend is the complete in-process Forge service boundary used by MCP tools.
type Backend interface {
	ProviderBackend
	LocalBackend
}

type federatedBackend struct {
	ProviderBackend
	LocalBackend
}

// NewFederatedBackend combines the provider owner with the connected node's
// local execution backend. The interfaces have disjoint method sets so MCP
// tools cannot accidentally select the wrong owner at runtime.
func NewFederatedBackend(provider ProviderBackend, local LocalBackend) Backend {
	return federatedBackend{ProviderBackend: provider, LocalBackend: local}
}

type RepositoryIdentity struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id"`
	RepoPath       string `json:"repo_path"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
}

type RepositorySummary struct {
	Repository          RepositoryIdentity
	OpenPRCount         int
	OpenIssueCount      int
	LastSyncCompletedAt string
	LastSyncError       string
}

type ActivityQuery struct {
	Since         string
	Repository    RepositoryIdentity
	ActivityTypes []string
	ItemTypes     []string
	Search        string
	After         string
}

type ActivityPage struct {
	Items  []ActivityItem
	Capped bool
}

type ActivityItem struct {
	ID             string
	Cursor         string
	ActivityType   string
	Repository     RepositoryIdentity
	ItemType       string
	ItemNumber     int
	ItemTitle      string
	ItemURL        string
	ItemState      string
	Workspace      *WorkspaceRef
	Author         string
	ItemAuthor     string
	CreatedAt      string
	BodyPreview    string
	BranchName     string
	CommitSHA      string
	BeforeSHA      string
	AfterSHA       string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	AuthoredAt     string
	CommittedAt    string
	ActivityURL    string
	SubjectState   string
}

type ItemListQuery struct {
	Repository RepositoryIdentity
	State      string
	Text       string
	Limit      int
	Offset     int
}

type WorkspaceRef struct {
	ID     string
	Status string
}

type Pull struct {
	Number          int
	Title           string
	State           string
	Author          string
	URL             string
	IsDraft         bool
	Body            string
	WorkflowStatus  string
	LastActivityAt  time.Time
	Repository      RepositoryIdentity
	Workspace       *WorkspaceRef
	DetailLoaded    bool
	DetailFetchedAt string
}

type Issue struct {
	Number          int
	Title           string
	State           string
	Author          string
	URL             string
	Body            string
	WorkflowStatus  string
	LastActivityAt  time.Time
	Repository      RepositoryIdentity
	Workspace       *WorkspaceRef
	DetailLoaded    bool
	DetailFetchedAt string
}

type ItemIdentity struct {
	Type           string `json:"type"`
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	Number         int    `json:"number"`
}

type DetailEvent struct {
	EventType string
	Author    string
	Summary   string
	Body      string
	CreatedAt time.Time
}

type Check struct {
	Name            string
	Status          string
	Conclusion      string
	URL             string
	App             string
	DurationSeconds *int64
}

type PullDetail struct {
	Pull            *Pull
	Events          []DetailEvent
	DetailLoaded    bool
	DetailFetchedAt string
	Workspace       *WorkspaceRef
	Stack           *Stack
	Checks          []Check
}

type IssueDetail struct {
	Issue           *Issue
	Events          []DetailEvent
	DetailLoaded    bool
	DetailFetchedAt string
	Workspace       *WorkspaceRef
	Workflow        *WorkflowState
}

type Diff struct {
	Stale bool
	Files []DiffFile
}

type DiffFile struct {
	Path        string
	OldPath     string
	Status      string
	IsBinary    bool
	IsGenerated bool
	Additions   int
	Deletions   int
	Patch       string
}

type Stack struct {
	Position int
	Size     int
	Health   string
	Members  []StackMember
}

type StackMember struct {
	Number   int
	Title    string
	State    string
	Position int
	IsDraft  bool
}

type WorkflowQuery struct {
	Repository    RepositoryIdentity `json:"repository"`
	ItemTypes     []string           `json:"item_types" nullable:"false"`
	States        []string           `json:"states" nullable:"false"`
	IncludeClosed bool               `json:"include_closed"`
	Limit         int                `json:"limit"`
	Cursor        string             `json:"cursor"`
}

type WorkflowPage struct {
	Items      []WorkflowItem `json:"items" nullable:"false"`
	NextCursor string         `json:"next_cursor"`
}

type WorkflowItem struct {
	Identity       ItemIdentity       `json:"identity"`
	Repository     RepositoryIdentity `json:"repository"`
	Title          string             `json:"title"`
	State          string             `json:"state"`
	URL            string             `json:"url"`
	Author         string             `json:"author"`
	IsDraft        bool               `json:"is_draft"`
	LastActivityAt string             `json:"last_activity_at"`
	Workflow       WorkflowState      `json:"workflow"`
}

type WorkflowState struct {
	Status        string `json:"status"`
	UpdatedAt     string `json:"updated_at"`
	UpdatedSource string `json:"updated_source"`
	UpdatedActor  string `json:"updated_actor"`
	UpdatedReason string `json:"updated_reason"`
}

type WorkflowUpdate struct {
	Status         string `json:"status"`
	ExpectedStatus string `json:"expected_status"`
	Force          bool   `json:"force"`
	Source         string `json:"source"`
	Actor          string `json:"actor"`
	Reason         string `json:"reason"`
}

type WorkflowMutation struct {
	PreviousStatus string        `json:"previous_status"`
	State          WorkflowState `json:"state"`
}

type LaunchTarget struct {
	Key            string
	Label          string
	Kind           string
	Source         string
	Available      bool
	DisabledReason string
}

type InitialMessageStatus struct {
	State        string
	MessageBytes int
	DeliveredAt  *time.Time
}

type WorkspaceAgentSession struct {
	Agent             string
	SessionID         string
	RuntimeSessionKey string
	TargetKey         string
	State             string
	UpdatedAt         time.Time
	InitialMessage    *InitialMessageStatus
}

type Workspace struct {
	ID           string
	Status       string
	Created      bool
	GitHeadRef   string
	ErrorMessage *string
}

type RuntimeSession struct {
	Key       string
	TargetKey string
	Kind      string
	Status    string
	CreatedAt time.Time
}

type WorkspaceRuntime struct {
	Sessions []RuntimeSession
}

type AgentMessageRequest struct {
	WorkspaceID       string
	RuntimeSessionKey string
	Message           string
}

type AgentMessageResult struct {
	TargetKey    string
	MessageBytes int
	SubmittedAt  time.Time
}

type InitialMessageRequest struct {
	WorkspaceID       string
	RuntimeSessionKey string
	TargetKey         string
	Message           string
}

const (
	ErrorCodeWorkspaceAlreadyExists          = "workspaceAlreadyExists"
	ErrorCodeInitialMessageInputModeNotReady = "initialMessageInputModeNotReady"
)

// Error is the transport-neutral MCP service error.
type Error struct {
	Kind      string
	Code      string
	Message   string
	Retryable bool
	Ambiguous bool
	Details   map[string]any
}

func (e *Error) Error() string { return e.Message }
