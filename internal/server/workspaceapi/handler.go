// Package workspaceapi owns the workspace and local-project HTTP boundary.
package workspaceapi

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/systemclipboard"
	"go.kenn.io/forge/internal/terminalpaste"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

// Event is the workspace domain's event-hub-neutral broadcast payload.
type Event struct {
	Type string
	Data any
}

// RecordedEvent is an Event with its event-hub generation.
type RecordedEvent struct {
	ID uint64
	Event
}

type eventHubAdapter struct {
	broadcast  func(Event) uint64
	subscribe  func(context.Context, bool) (<-chan RecordedEvent, <-chan struct{})
	generation func() uint64
}

func (h *eventHubAdapter) Broadcast(event Event) uint64 {
	if h == nil || h.broadcast == nil {
		return 0
	}
	return h.broadcast(event)
}

func (h *eventHubAdapter) Subscribe(
	ctx context.Context, injectCached bool,
) (<-chan RecordedEvent, <-chan struct{}) {
	if h != nil && h.subscribe != nil {
		return h.subscribe(ctx, injectCached)
	}
	events := make(chan RecordedEvent)
	done := make(chan struct{})
	close(events)
	close(done)
	return events, done
}

func (h *eventHubAdapter) Generation() uint64 {
	if h == nil || h.generation == nil {
		return 0
	}
	return h.generation()
}

type workspaceDiffEventData struct {
	WorkspaceID string `json:"workspace_id"`
	Revision    uint64 `json:"revision"`
	Version     string `json:"version"`
}

// Deps contains the state and root-owned integration hooks used by the
// workspace and project boundary. Hooks keep this package independent from
// the root server package while preserving the shared shutdown and event
// ordering owned by the composition root.
type Deps struct {
	DB                  *db.DB
	Resolver            *httpapi.RepositoryResolver
	Syncer              *ghclient.Syncer
	Config              ConfigSnapshot
	Workspaces          *workspace.Manager
	Runtime             *localruntime.Manager
	TerminalClipboard   systemclipboard.Writer
	TerminalPasteImages *terminalpaste.Store
	AgentActivity       *agentactivity.Store
	TmuxCommand         []string
	Now                 func() time.Time
	EnrichmentDisabled  bool
	Broadcast           func(Event) uint64
	Subscribe           func(context.Context, bool) (<-chan RecordedEvent, <-chan struct{})
	Generation          func() uint64

	RecomputeWorktreeLinks  func(context.Context)
	RefreshWorktreeStats    func(context.Context, string, string) error
	RefreshProjectInventory func(context.Context, string) error
	LookupRepo              func(context.Context, string, string, string, string) (*db.Repo, error)
	ResolveRepository       func(
		context.Context, providerplane.RepositoryRoute,
	) (*db.Repo, error)
	EnqueueDetailSync func(
		string, []any, func(context.Context) error, func(context.Context),
	) bool
	ProviderWriteGate           providerplane.WriteAdmitter
	LaunchSpecResolver          providerplane.WorkspaceLaunchSpecResolver
	PullCandidates              workspace.PullCandidateSource
	ProviderWorkspaceAutomation ProviderWorkspaceAutomation
	MergeRequestWorktreeSource  MergeRequestWorktreeSource
}

// Handler implements both the workspace and local-project services so their
// Git-heavy tests and process limits remain in one package and test binary.
type Handler struct {
	db            *db.DB
	resolver      *httpapi.RepositoryResolver
	syncer        *ghclient.Syncer
	configMu      sync.RWMutex
	config        ConfigSnapshot
	workspaces    *workspace.Manager
	runtime       *localruntime.Manager
	clipboard     systemclipboard.Writer
	pasteImages   *terminalpaste.Store
	agentActivity *agentactivity.Store
	tmuxCmd       []string
	now           func() time.Time
	broadcast     func(Event) uint64
	subscribe     func(context.Context, bool) (<-chan RecordedEvent, <-chan struct{})
	generation    func() uint64

	recomputeWorktreeLinks         func(context.Context)
	refreshWorktreeStats           func(context.Context, string, string) error
	refreshProjectInventory        func(context.Context, string) error
	lookupRepo                     func(context.Context, string, string, string, string) (*db.Repo, error)
	resolveRepository              func(context.Context, providerplane.RepositoryRoute) (*db.Repo, error)
	enqueueDetailSync              func(string, []any, func(context.Context) error, func(context.Context)) bool
	providerWriteGate              providerplane.WriteAdmitter
	launchSpecResolver             providerplane.WorkspaceLaunchSpecResolver
	pullCandidates                 workspace.PullCandidateSource
	providerWorkspaceAutomation    ProviderWorkspaceAutomation
	mergeRequestWorktreeSource     MergeRequestWorktreeSource
	hub                            *eventHubAdapter
	workspacePRMonitor             *workspace.PRMonitor
	workspacePushedHeadObserver    *workspace.PushedHeadObserver
	workspaceDiffCache             *workspaceDiffCache
	tmuxActivity                   *tmuxActivityTracker
	workspaceEnrichmentMu          sync.Mutex
	workspaceEnrichmentCache       map[string]workspaceEnrichmentCacheEntry
	workspaceEnrichmentInFlight    map[string]uint64
	workspaceEnrichmentFlightKinds map[string]workspaceEnrichmentKind
	workspaceEnrichmentFlightIDs   map[string]uint64
	workspaceEnrichmentNextFlight  uint64
	workspaceEnrichmentGenerations map[string]uint64
	workspaceEnrichmentPending     map[string]workspaceEnrichmentJob
	workspaceEnrichmentWorkers     int
	workspaceEnrichmentSlots       chan struct{}
	workspaceEnrichmentDisabled    bool
	workspaceSetupMu               sync.Mutex
	workspaceSetupDone             map[string]chan struct{}
	workspaceDeleting              map[string]*workspaceDeletion
	initialMessagesMu              sync.Mutex
	initialMessages                map[initialMessageKey]initialMessageAttempt
	worktreeShellTransactions      sync.Map
	workspaceTmuxPrunedAt          time.Time
	workspaceTmuxPrunePending      bool
	workspaceTmuxPruneInFlight     bool
	// workspaceSubjectAfterSummariesForTest pauses a snapshot between its two
	// repository-identity reads so tests can prove the reconciliation fence.
	workspaceSubjectAfterSummariesForTest func()
	lifecycleMu                           sync.Mutex
	lifecycleCtx                          context.Context
	lifecycleCancel                       context.CancelFunc
	lifecycleWG                           sync.WaitGroup
	lifecycleStarted                      bool
	lifecycleStopping                     bool
	lifecycleDone                         chan struct{}
}

// New creates the workspace and project handler.
func New(deps Deps) *Handler {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	if deps.Workspaces != nil {
		deps.Workspaces.SetNow(now)
		if deps.LaunchSpecResolver != nil {
			deps.Workspaces.SetLaunchSpecResolver(deps.LaunchSpecResolver)
		}
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	h := &Handler{
		db:                             deps.DB,
		resolver:                       deps.Resolver,
		syncer:                         deps.Syncer,
		config:                         cloneConfigSnapshot(deps.Config),
		workspaces:                     deps.Workspaces,
		runtime:                        deps.Runtime,
		clipboard:                      deps.TerminalClipboard,
		pasteImages:                    deps.TerminalPasteImages,
		agentActivity:                  deps.AgentActivity,
		tmuxCmd:                        slices.Clone(deps.TmuxCommand),
		now:                            now,
		broadcast:                      deps.Broadcast,
		subscribe:                      deps.Subscribe,
		generation:                     deps.Generation,
		recomputeWorktreeLinks:         deps.RecomputeWorktreeLinks,
		refreshWorktreeStats:           deps.RefreshWorktreeStats,
		refreshProjectInventory:        deps.RefreshProjectInventory,
		lookupRepo:                     deps.LookupRepo,
		resolveRepository:              deps.ResolveRepository,
		enqueueDetailSync:              deps.EnqueueDetailSync,
		providerWriteGate:              deps.ProviderWriteGate,
		launchSpecResolver:             deps.LaunchSpecResolver,
		pullCandidates:                 deps.PullCandidates,
		providerWorkspaceAutomation:    deps.ProviderWorkspaceAutomation,
		mergeRequestWorktreeSource:     deps.MergeRequestWorktreeSource,
		hub:                            &eventHubAdapter{broadcast: deps.Broadcast, subscribe: deps.Subscribe, generation: deps.Generation},
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentInFlight:    make(map[string]uint64),
		workspaceEnrichmentFlightKinds: make(map[string]workspaceEnrichmentKind),
		workspaceEnrichmentFlightIDs:   make(map[string]uint64),
		workspaceEnrichmentGenerations: make(map[string]uint64),
		workspaceEnrichmentPending:     make(map[string]workspaceEnrichmentJob),
		workspaceEnrichmentSlots:       make(chan struct{}, tmuxProbeMaxConcurrency),
		workspaceEnrichmentDisabled:    deps.EnrichmentDisabled,
		workspaceSetupDone:             make(map[string]chan struct{}),
		workspaceDeleting:              make(map[string]*workspaceDeletion),
		initialMessages:                make(map[initialMessageKey]initialMessageAttempt),
		tmuxActivity:                   newTmuxActivityTracker(now),
		lifecycleCtx:                   lifecycleCtx,
		lifecycleCancel:                lifecycleCancel,
		lifecycleDone:                  make(chan struct{}),
	}
	if deps.DB != nil && deps.Workspaces != nil {
		monitorOptions := workspace.PRMonitorOptions{
			LaunchSpecs: deps.Workspaces, PullCandidates: deps.PullCandidates,
			RetireUnresolvedWorkspace: func(_ context.Context, workspaceID string) error {
				return h.queueWorkspaceRetirement(workspaceID)
			},
		}
		h.workspacePRMonitor = workspace.NewPRMonitor(deps.DB, monitorOptions)
		h.workspacePushedHeadObserver = workspace.NewPushedHeadObserver(
			deps.DB, monitorOptions,
		)
	}
	h.workspaceDiffCache = newWorkspaceDiffCache(lifecycleCtx, workspaceDiffCacheDeps{
		onReady: func(workspaceID string, revision uint64, version string) {
			h.hub.Broadcast(Event{Type: "workspace_diff_ready", Data: workspaceDiffEventData{WorkspaceID: workspaceID, Revision: revision, Version: version}})
		},
		onChanged: func(workspaceID string, revision uint64, version string) {
			h.hub.Broadcast(Event{Type: "workspace_diff_changed", Data: workspaceDiffEventData{WorkspaceID: workspaceID, Revision: revision, Version: version}})
		},
	})
	return h
}

// Workspaces returns the shared workspace service boundary.
func (h *Handler) Workspaces() *Handler { return h }

// Projects returns the shared project service boundary.
func (h *Handler) Projects() *Handler { return h }

// RevalidateSelectedDiffs schedules validation for active workspace diff
// leases after worktree stats change.
func (h *Handler) RevalidateSelectedDiffs() {
	if h != nil && h.workspaceDiffCache != nil {
		h.workspaceDiffCache.RevalidateSelected()
	}
}

func (h *Handler) enqueueDetailSyncWithCompletion(
	key string,
	attrs []any,
	run func(context.Context) error,
	onSuccess func(context.Context),
) bool {
	if h.enqueueDetailSync == nil {
		return false
	}
	return h.enqueueDetailSync(key, attrs, run, onSuccess)
}

// Register registers workspace and local-project REST operations.
func (s *Handler) Register(api huma.API) {
	s.registerTerminalClipboard(api)
	s.registerTerminalPasteImage(api)
	huma.Register(api, huma.Operation{
		OperationID: "receive-agent-hook",
		Method:      http.MethodPost,
		Path:        "/agent-hooks/{agent}",
		Summary:     "Receive agent lifecycle hook",
		Tags:        []string{"Activity"},
	}, s.receiveAgentHook)
	huma.Register(api, huma.Operation{
		OperationID:   "create-workspace",
		Method:        http.MethodPost,
		Path:          "/workspaces",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Create workspace",
		Tags:          []string{"Workspaces"},
	}, s.createWorkspace)
	huma.Get(api, "/workspaces", s.listWorkspaces,
		httpapi.DocumentOperation("list-workspaces", "List workspaces", "Workspaces"))
	huma.Get(api, "/workspaces/{id}", s.getWorkspace,
		httpapi.DocumentOperation("get-workspace", "Get workspace", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/agent-sessions", s.listWorkspaceAgentSessions,
		httpapi.DocumentOperation("list-workspace-agent-sessions", "List live coding sessions", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/commits", s.getWorkspaceCommits,
		httpapi.DocumentOperation("get-workspace-commits", "Get workspace commits", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/diff", s.getWorkspaceDiff,
		httpapi.DocumentOperation("get-workspace-diff", "Get workspace diff", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/file-preview", s.getWorkspaceFilePreview,
		httpapi.DocumentOperation("get-workspace-file-preview", "Get workspace file preview", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/files", s.getWorkspaceFiles,
		httpapi.DocumentOperation("get-workspace-files", "Get workspace files", "Workspaces"))
	huma.Get(api, "/workspaces/{id}/diff/watch", s.watchWorkspaceDiff,
		httpapi.DocumentOperation("watch-workspace-diff", "Watch selected workspace diff", "Workspaces"))
	huma.Register(api, huma.Operation{
		OperationID:   "retry-workspace",
		Method:        http.MethodPost,
		Path:          "/workspaces/{id}/retry",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Retry workspace",
		Tags:          []string{"Workspaces"},
	}, s.retryWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "refresh-workspace",
		Method:      http.MethodPost,
		Path:        "/workspaces/{id}/refresh",
		Summary:     "Refresh workspace",
		Tags:        []string{"Workspaces"},
	}, s.refreshWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "push-workspace-branch",
		Method:      http.MethodPost,
		Path:        "/workspaces/{id}/push",
		Summary:     "Push workspace branch",
		Tags:        []string{"Workspaces"},
	}, s.pushWorkspaceBranch)
	huma.Register(api, huma.Operation{
		OperationID: "pull-workspace-branch",
		Method:      http.MethodPost,
		Path:        "/workspaces/{id}/pull",
		Summary:     "Pull workspace branch",
		Tags:        []string{"Workspaces"},
	}, s.pullWorkspaceBranch)
	huma.Register(api, huma.Operation{
		OperationID:   "reveal-workspace",
		Method:        http.MethodPost,
		Path:          "/workspaces/{id}/reveal",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Reveal workspace folder",
		Tags:          []string{"Workspaces"},
	}, s.revealWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "get-workspace-runtime",
		Method:      http.MethodGet,
		Path:        "/workspaces/{id}/runtime",
		Summary:     "Get workspace runtime",
		Tags:        []string{"Workspaces"},
	}, s.getWorkspaceRuntime)
	huma.Register(api, huma.Operation{
		OperationID: "launch-workspace-runtime-session",
		Method:      http.MethodPost,
		Path:        "/workspaces/{id}/runtime/sessions",
		Summary:     "Launch workspace runtime session",
		Tags:        []string{"Workspaces"},
	}, s.launchWorkspaceRuntimeSession)
	huma.Register(api, huma.Operation{
		OperationID:   "stop-workspace-runtime-session",
		Method:        http.MethodDelete,
		Path:          "/workspaces/{id}/runtime/sessions/{session_key}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Stop workspace runtime session",
		Tags:          []string{"Workspaces"},
	}, s.stopWorkspaceRuntimeSession)
	huma.Register(api, huma.Operation{
		OperationID: "get-workspace-runtime-session-attach-spec",
		Method:      http.MethodGet,
		Path:        "/workspaces/{id}/runtime/sessions/{session_key}/attach-spec",
		Summary:     "Get workspace runtime session attach spec",
		Tags:        []string{"Workspaces"},
	}, s.getWorkspaceRuntimeSessionAttachSpec)
	huma.Get(api, "/workspaces/{id}/runtime/sessions/{session_key}/initial-message",
		s.getInitialMessageStatus,
		httpapi.DocumentOperation("get-workspace-runtime-session-initial-message", "Get initial agent message status", "Workspaces"))
	huma.Register(api, huma.Operation{
		OperationID: "submit-workspace-runtime-session-initial-message",
		Method:      http.MethodPost,
		Path:        "/workspaces/{id}/runtime/sessions/{session_key}/initial-message",
		Summary:     "Submit initial agent message",
		Tags:        []string{"Workspaces"},
	}, s.submitInitialMessage)
	huma.Register(api, huma.Operation{
		OperationID: "rename-workspace-runtime-session",
		Method:      http.MethodPatch,
		Path:        "/workspaces/{id}/runtime/sessions/{session_key}",
		Summary:     "Rename workspace runtime session",
		Tags:        []string{"Workspaces"},
	}, s.renameWorkspaceRuntimeSession)
	huma.Register(api, huma.Operation{
		OperationID:   "delete-workspace",
		Method:        http.MethodDelete,
		Path:          "/workspaces/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete workspace",
		Tags:          []string{"Workspaces"},
	}, s.DeleteWorkspace)

	huma.Register(api, huma.Operation{
		OperationID:   "register-project",
		Method:        http.MethodPost,
		Path:          "/projects",
		DefaultStatus: http.StatusCreated,
		Summary:       "Register project",
		Tags:          []string{"Projects"},
	}, s.registerProject)
	huma.Register(api, huma.Operation{
		OperationID: "list-projects",
		Method:      http.MethodGet,
		Path:        "/projects",
		Summary:     "List projects",
		Tags:        []string{"Projects"},
	}, s.listProjects)
	huma.Register(api, huma.Operation{
		OperationID: "get-project",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}",
		Summary:     "Get project",
		Tags:        []string{"Projects"},
	}, s.getProject)
	huma.Register(api, huma.Operation{
		OperationID:   "delete-project",
		Method:        http.MethodDelete,
		Path:          "/projects/{project_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete project",
		Tags:          []string{"Projects"},
	}, s.deleteProject)
	huma.Register(api, huma.Operation{
		OperationID:   "register-worktree",
		Method:        http.MethodPost,
		Path:          "/projects/{project_id}/worktrees",
		DefaultStatus: http.StatusCreated,
		Summary:       "Register worktree",
		Tags:          []string{"Projects"},
	}, s.registerWorktree)
	huma.Register(api, huma.Operation{
		OperationID:   "delete-worktree",
		Method:        http.MethodDelete,
		Path:          "/projects/{project_id}/worktrees/{worktree_id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete worktree",
		Tags:          []string{"Projects"},
	}, s.deleteProjectWorktree)
	huma.Register(api, huma.Operation{
		OperationID:   "remove-worktree",
		Method:        http.MethodPost,
		Path:          "/projects/{project_id}/worktrees/{worktree_id}/delete",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Remove worktree (optionally from disk)",
		Tags:          []string{"Projects"},
	}, s.removeProjectWorktree)
	huma.Register(api, huma.Operation{
		OperationID:   "create-worktree-from-merge-request",
		Method:        http.MethodPost,
		Path:          "/projects/{project_id}/worktrees/from-merge-request",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create worktree from a merge request head",
		Tags:          []string{"Projects"},
	}, s.createProjectWorktreeFromMergeRequest)
	huma.Register(api, huma.Operation{
		OperationID:   "remove-stale-worktree",
		Method:        http.MethodPost,
		Path:          "/worktrees/remove-stale",
		DefaultStatus: http.StatusOK,
		Summary:       "Remove a stale worktree",
		Tags:          []string{"Projects"},
	}, s.removeStaleWorktree)
	huma.Register(api, huma.Operation{
		OperationID: "set-worktree-hidden",
		Method:      http.MethodPut,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/hidden",
		Summary:     "Set worktree hidden",
		Tags:        []string{"Projects"},
	}, s.setProjectWorktreeHidden)
	huma.Register(api, huma.Operation{
		OperationID: "set-worktree-session-backend",
		Method:      http.MethodPut,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/session-backend",
		Summary:     "Set worktree session backend",
		Tags:        []string{"Projects"},
	}, s.setProjectWorktreeSessionBackend)
	// OperationID stays generic ("links", not "linked-issues"): the project
	// registry's operation surface must not bake issue/PR-tracker terms into
	// op IDs (enforced by TestW1SliceAGate). The path and body field keep the
	// precise "linked issue numbers" naming; the op ID does not.
	huma.Register(api, huma.Operation{
		OperationID: "set-worktree-links",
		Method:      http.MethodPut,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/linked-issues",
		Summary:     "Set worktree linked issues",
		Tags:        []string{"Projects"},
	}, s.setProjectWorktreeLinkedIssues)
	huma.Register(api, huma.Operation{
		OperationID: "refresh-worktree-stats",
		Method:      http.MethodPost,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/refresh-stats",
		Summary:     "Refresh worktree git stats",
		Tags:        []string{"Projects"},
	}, s.refreshProjectWorktreeStats)
	huma.Register(api, huma.Operation{
		OperationID: "list-worktrees",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/worktrees",
		Summary:     "List worktrees",
		Tags:        []string{"Projects"},
	}, s.listWorktrees)
	huma.Register(api, huma.Operation{
		OperationID: "list-launch-targets",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/launch-targets",
		Summary:     "List launch targets",
		Tags:        []string{"Projects"},
	}, s.listLaunchTargets)
	huma.Register(api, huma.Operation{
		OperationID:   "clone-project",
		Method:        http.MethodPost,
		Path:          "/projects/clone",
		DefaultStatus: http.StatusCreated,
		Summary:       "Clone a repository and register it as a project",
		Tags:          []string{"Projects"},
	}, s.cloneProject)
	huma.Register(api, huma.Operation{
		OperationID: "list-project-branches",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/branches",
		Summary:     "List project repository branches",
		Tags:        []string{"Projects"},
	}, s.listProjectBranches)
	huma.Register(api, huma.Operation{
		OperationID: "inspect-project-worktree",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/inspect",
		Summary:     "Inspect project worktree",
		Tags:        []string{"Projects"},
	}, s.inspectProjectWorktree)
	huma.Register(api, huma.Operation{
		OperationID: "get-project-worktree-runtime",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/runtime",
		Summary:     "Get project worktree runtime",
		Tags:        []string{"Projects"},
	}, s.getProjectWorktreeRuntime)
	huma.Register(api, huma.Operation{
		OperationID: "ensure-project-worktree-runtime-shell",
		Method:      http.MethodPost,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/runtime/shell",
		Summary:     "Ensure project worktree shell",
		Tags:        []string{"Projects"},
	}, s.ensureProjectWorktreeRuntimeShell)
	huma.Register(api, huma.Operation{
		OperationID: "launch-project-worktree-runtime-session",
		Method:      http.MethodPost,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions",
		Summary:     "Launch project worktree runtime session",
		Tags:        []string{"Projects"},
	}, s.launchProjectWorktreeRuntimeSession)
	huma.Register(api, huma.Operation{
		OperationID:   "stop-project-worktree-runtime-session",
		Method:        http.MethodDelete,
		Path:          "/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Stop project worktree runtime session",
		Tags:          []string{"Projects"},
	}, s.stopProjectWorktreeRuntimeSession)
	huma.Register(api, huma.Operation{
		OperationID: "get-project-worktree-runtime-session-attach-spec",
		Method:      http.MethodGet,
		Path:        "/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}/attach-spec",
		Summary:     "Get project worktree runtime session attach spec",
		Tags:        []string{"Projects"},
	}, s.getProjectWorktreeRuntimeSessionAttachSpec)
}
