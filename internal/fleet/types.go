package fleet

// NodeID is one daemon's stable federation identity.
type NodeID string

// Role controls how a daemon projects a neutral fleet aggregate.
type Role string

const (
	RoleHub   Role = "hub"
	RoleSpoke Role = "spoke"
)

// RepositoryIdentity is the provider-verified cross-spoke repository key.
// Local numeric repository IDs never enter a federation payload.
type RepositoryIdentity struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platformHost"`
	PlatformRepoID string `json:"platformRepoID"`
	Owner          string `json:"owner,omitempty"`
	Name           string `json:"name,omitempty"`
}

// ---- raw layer (scoped keys, no UUIDs; spoke-to-hub wire shape) ----

type RawHost struct {
	Hostname         string            `json:"hostname"`
	Platform         string            `json:"platform"` // "linux" | "macos"
	Version          string            `json:"version,omitempty"`
	LastSeenAt       string            `json:"lastSeenAt,omitempty"`
	TmuxLastPolledAt string            `json:"tmuxLastPolledAt,omitempty"`
	TmuxProbeError   string            `json:"tmuxProbeError,omitempty"`
	TmuxMetricsError string            `json:"tmuxMetricsError,omitempty"`
	TmuxSessions     []TmuxSessionInfo `json:"tmuxSessions,omitempty"`
}

type TmuxWindowInfo struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Activity string `json:"activity,omitempty"`
}

type TmuxSessionInfo struct {
	Name             string           `json:"name"`
	Managed          bool             `json:"managed"`
	WorktreeKey      string           `json:"worktreeKey,omitempty"`
	SessionScopedKey string           `json:"sessionScopedKey,omitempty"`
	Windows          []TmuxWindowInfo `json:"windows"`
	WindowCount      int              `json:"windowCount"`
	CreatedAt        string           `json:"createdAt,omitempty"`
}

type RawSnapshot struct {
	ProtocolVersion       int            `json:"protocolVersion"`
	NodeID                NodeID         `json:"nodeID"`
	BaseURL               string         `json:"baseURL,omitempty"`
	Generation            uint64         `json:"generation"`
	Host                  RawHost        `json:"host"`
	PlatformAuthenticated *bool          `json:"platformAuthenticated,omitempty"`
	Capabilities          *Capabilities  `json:"capabilities,omitempty"`
	Projects              []RawProject   `json:"projects,omitempty"`
	Worktrees             []RawWorktree  `json:"worktrees,omitempty"`
	Sessions              []RawSession   `json:"sessions,omitempty"`
	Workspaces            []RawWorkspace `json:"workspaces,omitempty"`
}

type RawProject struct {
	HostKey   string `json:"hostKey,omitempty"`
	ScopedKey string `json:"scopedKey"`
	// RegistryID is the producer's local registry id for this project (empty
	// for a synthesized project, which has no registry row). A client mutates
	// the project by this id rather than by scoped key.
	RegistryID    string `json:"registryId,omitempty"`
	Name          string `json:"name"`
	RootPath      string `json:"rootPath"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// Platform is the provider kind ("github", "gitlab", "forgejo",
	// "gitea") of the project's platform identity. Together with
	// PlatformHost and PlatformRepo it lets a client build provider-aware
	// routes and distinguish same-host identities across providers.
	Platform       string             `json:"platform,omitempty"`
	PlatformRepo   string             `json:"platformRepo,omitempty"`
	PlatformHost   string             `json:"platformHost,omitempty"`
	Repository     RepositoryIdentity `json:"repository,omitzero"`
	IsStale        bool               `json:"isStale,omitempty"`
	RepositoryKind string             `json:"repositoryKind,omitempty"`
	BackendReady   *bool              `json:"backendReady,omitempty"`
	// IsSynthesized marks a project with no registered local checkout —
	// synthesized only to anchor an orphan workspace's worktree. Such a
	// project has no rootPath or repositoryKind; consumers must treat it as
	// read-only (no worktree creation) rather than a registered project.
	IsSynthesized bool `json:"isSynthesized,omitempty"`
}

type RawWorktree struct {
	HostKey    string `json:"hostKey,omitempty"`
	ScopedKey  string `json:"scopedKey"`
	ProjectKey string `json:"projectKey"`
	// RegistryID is the producer's local registry id for this worktree (empty
	// for a synthesized primary root worktree or a workspace-only overlay,
	// neither of which has a registry row). A client mutates the worktree by
	// this id rather than by scoped key.
	RegistryID     string  `json:"registryId,omitempty"`
	Name           string  `json:"name"`
	Path           string  `json:"path"`
	Branch         string  `json:"branch,omitempty"`
	IsPrimary      bool    `json:"isPrimary,omitempty"`
	IsStale        bool    `json:"isStale,omitempty"`
	DiffAdded      *int    `json:"diffAdded,omitempty"`
	DiffRemoved    *int    `json:"diffRemoved,omitempty"`
	SyncAhead      *int    `json:"syncAhead,omitempty"`
	SyncBehind     *int    `json:"syncBehind,omitempty"`
	LinkedPRNumber *int    `json:"linkedPRNumber,omitempty"`
	PRState        *string `json:"prState,omitempty"`
	PRTitle        *string `json:"prTitle,omitempty"`
	ChecksStatus   *string `json:"checksStatus,omitempty"`

	// Pull-request enrichment carried straight from the linked merge
	// request's cached metadata. ReviewDecision and Mergeable are the
	// platform's own lowercase vocabularies (e.g. "approved",
	// "changes_requested"; "clean", "dirty"). Additions/Deletions are the
	// merge request's own diff size as reported by the platform, distinct
	// from DiffAdded/DiffRemoved (the live working-tree stats). All are
	// omitted when zero/empty so an unenriched or undetailed worktree
	// carries no misleading zeros.
	PRReviewDecision *string `json:"prReviewDecision,omitempty"`
	PRMergeable      *string `json:"prMergeable,omitempty"`
	PRAdditions      *int    `json:"prAdditions,omitempty"`
	PRDeletions      *int    `json:"prDeletions,omitempty"`
	PRCommentCount   *int    `json:"prCommentCount,omitempty"`

	IsHidden           bool          `json:"isHidden,omitempty"`
	PRURL              *string       `json:"prURL,omitempty"`
	PRUpdatedAt        *string       `json:"prUpdatedAt,omitempty"`
	ChecksDetail       []CheckDetail `json:"checksDetail,omitempty"`
	LastPolledAt       *string       `json:"lastPolledAt,omitempty"`
	SessionBackend     string        `json:"sessionBackend,omitempty"`
	LinkedIssueNumbers []int         `json:"linkedIssueNumbers,omitempty"`
}

// Session backend vocabulary for RawWorktree.SessionBackend and
// WorktreeSummary.SessionBackend. These are generic terminal-backend
// descriptors: a local PTY-owner-managed terminal or a local tmux session.
// Federation preserves the owning spoke's backend because terminal traffic is
// bridged over WebSocket without changing the spoke-local session model.
const (
	SessionBackendLocalPTY  = "localPTY"
	SessionBackendLocalTmux = "localTmux"
)

type RawSession struct {
	HostKey        string   `json:"hostKey,omitempty"`
	ScopedKey      string   `json:"scopedKey"`
	WorktreeKey    string   `json:"worktreeKey,omitempty"`
	Status         string   `json:"status"`
	RuntimeKind    string   `json:"runtimeKind,omitempty"`
	SessionKind    string   `json:"sessionKind,omitempty"`
	Role           string   `json:"role,omitempty"`
	Label          string   `json:"label,omitempty"`
	ExecutableName string   `json:"executableName,omitempty"`
	AgentKind      string   `json:"agentKind,omitempty"`
	CPUPercent     *float64 `json:"cpuPercent,omitempty"`
	ResidentMB     *int     `json:"residentMB,omitempty"`
	ProcessCount   *int     `json:"processCount,omitempty"`
	LastOutputAt   *string  `json:"lastOutputAt,omitempty"`
	LastActiveAt   *string  `json:"lastActiveAt,omitempty"`
}

// RawWorkspace is a detached, spoke-owned workspace summary. The raw producer
// fills execution fields and source-link visibility; provider display fields
// are populated by the hub after aggregate construction.
type RawWorkspace struct {
	HostKey               string             `json:"hostKey,omitempty"`
	ID                    string             `json:"id"`
	Repository            RepositoryIdentity `json:"repository"`
	ItemType              string             `json:"itemType"`
	ItemNumber            int                `json:"itemNumber"`
	SourceItemVisible     bool               `json:"sourceItemVisible"`
	ItemKey               string             `json:"itemKey,omitempty"`
	GitHeadRef            string             `json:"gitHeadRef"`
	WorktreePath          string             `json:"worktreePath"`
	TmuxSession           string             `json:"tmuxSession,omitempty"`
	SessionBackend        string             `json:"sessionBackend,omitempty"`
	TmuxPaneTitle         *string            `json:"tmuxPaneTitle,omitempty"`
	TmuxWorking           bool               `json:"tmuxWorking"`
	TmuxActivitySource    string             `json:"tmuxActivitySource,omitempty"`
	TmuxLastOutputAt      *string            `json:"tmuxLastOutputAt,omitempty"`
	AgentState            *string            `json:"agentState,omitempty"`
	AgentStateUpdatedAt   *string            `json:"agentStateUpdatedAt,omitempty"`
	Status                string             `json:"status"`
	ErrorMessage          *string            `json:"errorMessage,omitempty"`
	CreatedAt             string             `json:"createdAt"`
	CommitsAhead          *int               `json:"commitsAhead,omitempty"`
	CommitsBehind         *int               `json:"commitsBehind,omitempty"`
	BranchUpstreamMissing *bool              `json:"branchUpstreamMissing,omitempty"`
	WorktreeDirty         *bool              `json:"worktreeDirty,omitempty"`
	EnrichmentStatus      string             `json:"enrichmentStatus,omitempty"`
	EnrichmentRefreshedAt *string            `json:"enrichmentRefreshedAt,omitempty"`
	EnrichmentError       *string            `json:"enrichmentError,omitempty"`
	AssociatedPRNumber    *int               `json:"associatedPRNumber,omitempty"`
	Kata                  *RawWorkspaceKata  `json:"kata,omitempty"`
	ItemLastActivityAt    *string            `json:"itemLastActivityAt,omitempty"`
	MRTitle               *string            `json:"mrTitle,omitempty"`
	MRState               *string            `json:"mrState,omitempty"`
	MRIsDraft             *bool              `json:"mrIsDraft,omitempty"`
	MRCIStatus            *string            `json:"mrCIStatus,omitempty"`
	MRReviewDecision      *string            `json:"mrReviewDecision,omitempty"`
	MRAdditions           *int               `json:"mrAdditions,omitempty"`
	MRDeletions           *int               `json:"mrDeletions,omitempty"`
}

type RawWorkspaceKata struct {
	DaemonID    string `json:"daemonID"`
	ProjectUID  string `json:"projectUID"`
	ProjectName string `json:"projectName,omitempty"`
	IssueUID    string `json:"issueUID"`
	ShortID     string `json:"shortID,omitempty"`
	QualifiedID string `json:"qualifiedID,omitempty"`
	Title       string `json:"title,omitempty"`
}

// NeutralHost is one host record in the hub's observer-independent
// aggregate. It carries source facts, never projected kind or permissions.
type NeutralHost struct {
	NodeID                NodeID            `json:"nodeID"`
	FederationRole        Role              `json:"federationRole"`
	Name                  string            `json:"name"`
	Hostname              string            `json:"hostname,omitempty"`
	BaseURL               string            `json:"baseURL,omitempty"`
	Platform              string            `json:"platform,omitempty"`
	Reachable             bool              `json:"reachable"`
	PlatformAuthenticated *bool             `json:"platformAuthenticated,omitempty"`
	Generation            uint64            `json:"generation,omitempty"`
	Version               string            `json:"version,omitempty"`
	LastSeenAt            string            `json:"lastSeenAt,omitempty"`
	TmuxLastPolledAt      string            `json:"tmuxLastPolledAt,omitempty"`
	TmuxProbeError        string            `json:"tmuxProbeError,omitempty"`
	TmuxMetricsError      string            `json:"tmuxMetricsError,omitempty"`
	Error                 *string           `json:"error,omitempty"`
	Capabilities          *Capabilities     `json:"capabilities,omitempty"`
	TmuxSessions          []TmuxSessionInfo `json:"tmuxSessions,omitempty"`
}

// NeutralSnapshot is the hub-owned aggregate before a serving daemon
// projects self/remote kind and operation availability for its own observer.
type NeutralSnapshot struct {
	ProtocolVersion       int            `json:"protocolVersion"`
	Generation            uint64         `json:"generation"`
	PlatformAuthenticated *bool          `json:"platformAuthenticated,omitempty"`
	Hosts                 []NeutralHost  `json:"hosts"`
	Projects              []RawProject   `json:"projects,omitempty"`
	Worktrees             []RawWorktree  `json:"worktrees,omitempty"`
	Sessions              []RawSession   `json:"sessions,omitempty"`
	Workspaces            []RawWorkspace `json:"workspaces,omitempty"`
}

// Observer identifies the daemon serving a projected fleet snapshot.
type Observer struct {
	NodeID NodeID
	Role   Role
}

// ---- capabilities + diagnostics ----

type CommandCapabilities struct {
	WorktreeCreate   bool `json:"worktreeCreate"`
	WorktreeImportPR bool `json:"worktreeImportPullRequest"`
	WorktreeDelete   bool `json:"worktreeDelete"`
	SessionEnsure    bool `json:"sessionEnsure"`
	SessionKill      bool `json:"sessionKill"`
	RepositoryClone  bool `json:"repositoryClone"`
	ProjectAdd       bool `json:"projectAdd"`
	ProjectRemove    bool `json:"projectRemove"`
}

type DependencyCapabilities struct {
	Git  bool `json:"git"`
	Gh   bool `json:"gh"`
	Tmux bool `json:"tmux"`
}

type FeatureCapabilities struct {
	ResourceMetrics bool   `json:"resourceMetrics"`
	SetupHook       bool   `json:"setupHook"`
	TeardownHook    bool   `json:"teardownHook"`
	MoshAttach      bool   `json:"moshAttach"`
	TmuxVersion     string `json:"tmuxVersion,omitempty"`
}

// Capabilities groups command, dependency, and feature availability for a host.
type Capabilities struct {
	Commands     CommandCapabilities    `json:"commands"`
	Dependencies DependencyCapabilities `json:"dependencies"`
	Features     FeatureCapabilities    `json:"features"`
}

type HostDiagnostic struct {
	Code               string   `json:"code"`
	Severity           string   `json:"severity"`
	Summary            string   `json:"summary"`
	RecoverySuggestion string   `json:"recoverySuggestion"`
	BlocksOperations   []string `json:"blocksOperations"`
}

type HostOperationAvailability struct {
	Available         bool    `json:"available"`
	UnavailableReason *string `json:"unavailableReason,omitempty"`
}

// ---- enriched layer (UUIDs, client-ready) ----

// Snapshot is the enriched client-ready snapshot envelope.
// Populated by the snapshot builder.
type Snapshot struct {
	ProtocolVersion       int                `json:"protocolVersion"`
	Generation            uint64             `json:"generation"`
	AggregateIncomplete   bool               `json:"aggregateIncomplete,omitempty"`
	PlatformAuthenticated *bool              `json:"platformAuthenticated,omitempty"`
	ActivePlatformHost    *string            `json:"activePlatformHost,omitempty"`
	Hosts                 []HostSummary      `json:"hosts"`
	Projects              []ProjectSummary   `json:"projects"`
	Worktrees             []WorktreeSummary  `json:"worktrees"`
	Sessions              []SessionSummary   `json:"sessions"`
	Workspaces            []WorkspaceSummary `json:"workspaces"`
	ProjectMap            map[string]string  `json:"projectMap,omitempty"`
}

type HostSummary struct {
	ID                    string                               `json:"id"`
	ConfigKey             string                               `json:"configKey"`
	NodeID                string                               `json:"nodeID"`
	Name                  string                               `json:"name"`
	Kind                  string                               `json:"kind"`
	FederationRole        Role                                 `json:"federationRole" enum:"hub,spoke"`
	BaseURL               string                               `json:"baseURL,omitempty" format:"uri"`
	Platform              string                               `json:"platform"`
	PreferredTransport    string                               `json:"preferredTransport"`
	Reachable             bool                                 `json:"reachable"`
	LastSeenAt            *string                              `json:"lastSeenAt,omitempty"`
	Hostname              *string                              `json:"hostname,omitempty"`
	Version               *string                              `json:"version,omitempty"`
	TmuxLastPolledAt      *string                              `json:"tmuxLastPolledAt,omitempty"`
	TmuxProbeError        string                               `json:"tmuxProbeError,omitempty"`
	TmuxMetricsError      string                               `json:"tmuxMetricsError,omitempty"`
	Capabilities          *Capabilities                        `json:"capabilities,omitempty"`
	Diagnostics           []HostDiagnostic                     `json:"diagnostics"`
	OperationAvailability map[string]HostOperationAvailability `json:"operationAvailability"`
	Error                 *string                              `json:"error,omitempty"`
	ConnectionState       *string                              `json:"connectionState,omitempty"`
	TmuxSessions          []TmuxSessionInfo                    `json:"tmuxSessions"`
}

type ProjectSummary struct {
	ID        string `json:"id"`
	HostID    string `json:"hostID"`
	ScopedKey string `json:"scopedKey"`
	// RegistryID is the producer's local registry id (see RawProject.RegistryID):
	// the id a client mutates the project by. Empty for a synthesized project.
	RegistryID     string `json:"registryID,omitempty"`
	Name           string `json:"name"`
	RootPath       string `json:"rootPath"`
	RepositoryKind string `json:"repositoryKind"`
	DefaultBranch  string `json:"defaultBranch"`
	// Platform is the provider kind of the project's platform identity
	// (see RawProject.Platform).
	Platform         string  `json:"platform,omitempty"`
	PlatformURL      *string `json:"platformURL,omitempty"`
	PlatformCoverage *string `json:"platformCoverage,omitempty"`
	IsStale          bool    `json:"isStale,omitempty"`
	// IsSynthesized marks a project with no registered local checkout (see
	// RawProject.IsSynthesized): rootPath/repositoryKind are empty and the
	// consumer must treat it as read-only.
	IsSynthesized bool `json:"isSynthesized,omitempty"`
}

type WorktreeSummary struct {
	ID        string `json:"id"`
	HostID    string `json:"hostID"`
	ProjectID string `json:"projectID"`
	ScopedKey string `json:"scopedKey"`
	// RegistryID is the producer's local registry id (see RawWorktree.RegistryID):
	// the id a client mutates the worktree by. Empty for a synthesized primary
	// root worktree or a workspace-only overlay.
	RegistryID     string  `json:"registryID,omitempty"`
	Name           string  `json:"name"`
	Path           string  `json:"path"`
	Branch         string  `json:"branch"`
	IsPrimary      bool    `json:"isPrimary,omitempty"`
	IsHidden       bool    `json:"isHidden,omitempty"`
	IsStale        bool    `json:"isStale,omitempty"`
	DiffAdded      *int    `json:"diffAdded,omitempty"`
	DiffRemoved    *int    `json:"diffRemoved,omitempty"`
	SyncAhead      *int    `json:"syncAhead,omitempty"`
	SyncBehind     *int    `json:"syncBehind,omitempty"`
	LinkedPRNumber *int    `json:"linkedPRNumber,omitempty"`
	PRState        *string `json:"prState,omitempty"`
	PRURL          *string `json:"prURL,omitempty"`
	PRTitle        *string `json:"prTitle,omitempty"`
	PRUpdatedAt    *string `json:"prUpdatedAt,omitempty"`
	// Pull-request enrichment carried through from the linked merge request's
	// cached metadata (see RawWorktree). PRReviewDecision and PRMergeable are
	// the platform's lowercase vocabularies; PRAdditions/PRDeletions are the
	// merge request's own platform-reported diff size, distinct from
	// DiffAdded/DiffRemoved (the live working-tree stats). All are omitted when
	// zero/empty so an unenriched or undetailed worktree carries no misleading
	// review state or "+0 -0".
	PRReviewDecision   *string       `json:"prReviewDecision,omitempty"`
	PRMergeable        *string       `json:"prMergeable,omitempty"`
	PRAdditions        *int          `json:"prAdditions,omitempty"`
	PRDeletions        *int          `json:"prDeletions,omitempty"`
	PRCommentCount     *int          `json:"prCommentCount,omitempty"`
	ChecksStatus       *string       `json:"checksStatus,omitempty"`
	ChecksDetail       []CheckDetail `json:"checksDetail,omitempty"`
	LastPolledAt       *string       `json:"lastPolledAt,omitempty"`
	SessionBackend     string        `json:"sessionBackend"`
	LinkedIssueNumbers []int         `json:"linkedIssueNumbers"`
}

type CheckDetail struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	URL        string `json:"url,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

type WorkspaceRepositorySummary struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id,omitempty"`
	RepoPath       string `json:"repo_path"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
}

type WorkspaceKataSummary struct {
	DaemonID    string `json:"daemon_id"`
	ProjectUID  string `json:"project_uid"`
	ProjectName string `json:"project_name,omitempty"`
	IssueUID    string `json:"issue_uid"`
	ShortID     string `json:"short_id,omitempty"`
	QualifiedID string `json:"qualified_id,omitempty"`
	Title       string `json:"title,omitempty"`
}

// WorkspaceSummary is the projected workspace-list contract. Remote host
// attribution is omitted for the observer's own local workspaces.
type WorkspaceSummary struct {
	ID                    string                     `json:"id"`
	Repo                  WorkspaceRepositorySummary `json:"repo"`
	PlatformHost          string                     `json:"platform_host"`
	RepoOwner             string                     `json:"repo_owner"`
	RepoName              string                     `json:"repo_name"`
	ItemType              string                     `json:"item_type"`
	ItemNumber            int                        `json:"item_number"`
	SourceItemVisible     bool                       `json:"source_item_visible"`
	ItemKey               string                     `json:"item_key,omitempty"`
	GitHeadRef            string                     `json:"git_head_ref"`
	WorktreePath          string                     `json:"worktree_path"`
	TmuxSession           string                     `json:"tmux_session,omitempty"`
	TmuxPaneTitle         *string                    `json:"tmux_pane_title,omitempty"`
	TmuxWorking           bool                       `json:"tmux_working"`
	TmuxActivitySource    string                     `json:"tmux_activity_source"`
	TmuxLastOutputAt      *string                    `json:"tmux_last_output_at"`
	AgentState            *string                    `json:"agent_state,omitempty"`
	AgentStateUpdatedAt   *string                    `json:"agent_state_updated_at,omitempty"`
	Status                string                     `json:"status"`
	ErrorMessage          *string                    `json:"error_message,omitempty"`
	CreatedAt             string                     `json:"created_at"`
	ItemLastActivityAt    *string                    `json:"item_last_activity_at,omitempty"`
	MRTitle               *string                    `json:"mr_title,omitempty"`
	MRState               *string                    `json:"mr_state,omitempty"`
	MRIsDraft             *bool                      `json:"mr_is_draft,omitempty"`
	MRCIStatus            *string                    `json:"mr_ci_status,omitempty"`
	MRReviewDecision      *string                    `json:"mr_review_decision,omitempty"`
	MRAdditions           *int                       `json:"mr_additions,omitempty"`
	MRDeletions           *int                       `json:"mr_deletions,omitempty"`
	CommitsAhead          *int                       `json:"commits_ahead,omitempty"`
	CommitsBehind         *int                       `json:"commits_behind,omitempty"`
	BranchUpstreamMissing *bool                      `json:"branch_upstream_missing,omitempty"`
	WorktreeDirty         *bool                      `json:"worktree_dirty,omitempty"`
	EnrichmentStatus      string                     `json:"enrichment_status,omitempty"`
	EnrichmentRefreshedAt *string                    `json:"enrichment_refreshed_at,omitempty"`
	EnrichmentError       *string                    `json:"enrichment_error,omitempty"`
	AssociatedPRNumber    *int                       `json:"associated_pr_number,omitempty"`
	Kata                  *WorkspaceKataSummary      `json:"kata,omitempty"`
	FleetHostKey          string                     `json:"fleet_host_key,omitempty"`
	FleetHostName         string                     `json:"fleet_host_name,omitempty"`
	Visible               bool                       `json:"visible"`
}

type SessionSummary struct {
	ID             string   `json:"id"`
	HostID         string   `json:"hostID"`
	WorktreeID     *string  `json:"worktreeID,omitempty"`
	ScopedKey      string   `json:"scopedKey"`
	RuntimeKind    string   `json:"runtimeKind"`
	Status         string   `json:"status"`
	SessionKind    string   `json:"sessionKind,omitempty"`
	Role           string   `json:"role,omitempty"`
	ExecutableName string   `json:"executableName,omitempty"`
	AgentKind      string   `json:"agentKind,omitempty"`
	CPUPercent     *float64 `json:"cpuPercent,omitempty"`
	ResidentMB     *int     `json:"residentMB,omitempty"`
	ProcessCount   *int     `json:"processCount,omitempty"`
	LastOutputAt   *string  `json:"lastOutputAt,omitempty"`
	LastActiveAt   *string  `json:"lastActiveAt,omitempty"`
}
