package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/workspace/localruntime"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
)

// Manager owns kenn-forge's persisted workspace lifecycle.
//
// Its purpose is to turn tracked review items into durable local execution
// contexts backed by a database row, a Git worktree, and a tmux session. It is
// intentionally not a generic host worktree browser or arbitrary Git
// automation layer.
type Manager struct {
	db                        *db.DB
	worktreeDir               string
	clones                    *gitclone.Manager
	locks                     *FileLockManager
	tmuxCmd                   []string
	tmuxStripEnvMu            sync.RWMutex
	tmuxStripEnvVars          []string
	hideTmuxStatusMu          sync.RWMutex
	hideTmuxStatus            bool
	tmuxGraphicsMu            sync.RWMutex
	tmuxGraphics              bool
	tmuxMouseMu               sync.RWMutex
	tmuxMouse                 bool
	ptyOwner                  PtyOwnerClient
	preferPtyOwner            bool
	retryMu                   sync.Mutex
	retryQueued               map[string]bool
	runtimeTmuxMu             sync.Mutex
	issueBranchSlugEnabled    bool
	summaryCacheMu            sync.RWMutex
	summaryCache              []WorkspaceSummary
	deletedSummaryIDs         map[string]bool
	worktreeBaseResolver      WorktreeBasePathResolver
	launchSpecResolver        providerplane.WorkspaceLaunchSpecResolver
	requireProviderCredential bool
	now                       func() time.Time
	afterHeadRepoSnapshotRead func()
	// beforeExistingWorktreeRepoLock runs after reuse pre-validation and right
	// before acquiring the repository lock; tests use it to coordinate a path
	// replacement while lock acquisition is blocked.
	beforeExistingWorktreeRepoLock func()
	// beforeSetupRouteRevalidation runs right before setup's final route
	// re-validation; tests use it to interleave a route replacement.
	beforeSetupRouteRevalidation func()
	// beforeHeadRepoSnapshotRepoLookup runs right before the head-repo
	// refresh resolves the workspace repository; tests use it to queue a
	// reconciliation writer against the held read lock.
	beforeHeadRepoSnapshotRepoLookup func()
	roborevEndpoint                  string
	roborevRepositoryInvalidator     func()
}

// WorktreeBaseRepository identifies a tracked remote repository when resolving
// a user-configured checkout for new git worktrees.
type WorktreeBaseRepository struct {
	Platform       string
	PlatformHost   string
	PlatformRepoID string
	Owner          string
	Name           string
}

// WorktreeBasePathResolver resolves a tracked remote repository to a
// user-configured local repository that should own new git worktrees.
type WorktreeBasePathResolver func(
	ctx context.Context, repo WorktreeBaseRepository,
) (path string, ok bool, err error)

// CreateIssueOptions controls how issue-backed workspaces choose their branch.
//
// The default path creates kenn-forge's conventional issue branch. When a local
// branch with that name already exists, callers can either ask the manager to
// reuse it or supply a different GitHeadRef.
type CreateIssueOptions struct {
	Provider               string
	GitHeadRef             string
	ReuseExistingBranch    bool
	ReuseExistingDirectory bool
}

// CreateAdHocOptions controls how ad-hoc workspaces choose their branch.
//
// Ad-hoc workspaces have no source item, so the branch is their only identity.
// An empty BranchName generates one; a name that already exists locally is
// reused when requested or automatically suffixed with a short random hash.
type CreateAdHocOptions struct {
	BranchName          string
	ReuseExistingBranch bool
}

// WorkspaceBranchConflictError reports that the requested workspace branch
// already exists locally. ExistingDirectory means the deterministic workspace
// path is occupied, so choosing another branch cannot resolve the conflict.
type WorkspaceBranchConflictError struct {
	Branch            string
	SuggestedBranch   string
	ExistingDirectory bool
}

func (e *WorkspaceBranchConflictError) Error() string {
	return fmt.Sprintf(
		"workspace branch %q already exists; suggested alternative %q",
		e.Branch,
		e.SuggestedBranch,
	)
}

type WorkspaceDirectoryRecoveryReason string

const (
	WorkspaceDirectoryMissing            WorkspaceDirectoryRecoveryReason = "missing"
	WorkspaceDirectoryNotLinkedWorktree  WorkspaceDirectoryRecoveryReason = "not_linked_worktree"
	WorkspaceDirectoryRepositoryMismatch WorkspaceDirectoryRecoveryReason = "repository_mismatch"
	WorkspaceDirectoryBranchMismatch     WorkspaceDirectoryRecoveryReason = "branch_mismatch"
)

type WorkspaceDirectoryRecoveryError struct {
	Reason         WorkspaceDirectoryRecoveryReason
	ExpectedBranch string
	ActualBranch   string
}

func (e *WorkspaceDirectoryRecoveryError) Error() string {
	switch e.Reason {
	case WorkspaceDirectoryMissing:
		return "the expected Kenn Forge worktree directory does not exist"
	case WorkspaceDirectoryNotLinkedWorktree:
		return "the expected Kenn Forge directory is not a linked Git worktree"
	case WorkspaceDirectoryRepositoryMismatch:
		return "the expected Kenn Forge worktree does not belong to this repository"
	case WorkspaceDirectoryBranchMismatch:
		return fmt.Sprintf(
			"the expected Kenn Forge worktree checks out %q, not %q",
			e.ActualBranch, e.ExpectedBranch,
		)
	default:
		return "the expected Kenn Forge worktree cannot be reused"
	}
}

const (
	workspaceSetupStageSetup           = "setup"
	workspaceSetupStageClone           = "clone"
	workspaceSetupStageWorktree        = "worktree"
	workspaceSetupStageRepositoryHooks = "repository_hooks"
	workspaceSetupStageTmuxSession     = "tmux_session"
	workspaceBranchUnknown             = "__kenn_forge_unknown__"
	workspaceBranchRecoveryPending     = "__kenn_forge_recovery_pending__..state"
	workspaceOwnershipMarkerFile       = "kenn-forge-workspace-id"
	tmuxCaptureScrollbackLines         = 160
)

var workspacePersistTimeout = 5 * time.Second
var workspaceCleanupTimeout = 5 * time.Second

var (
	ErrWorkspaceNotFound             = errors.New("workspace not found")
	ErrWorkspaceNotSynced            = errors.New("workspace merge request not synced")
	ErrWorkspaceDuplicate            = errors.New("workspace already exists")
	ErrWorkspaceRepositoryUnresolved = errors.New("workspace repository identity is unresolved")
	ErrWorkspaceInvalidState         = errors.New("workspace invalid state")
	ErrWorkspaceOwnershipUnproven    = errors.New("workspace worktree ownership cannot be verified")
	errWorkspaceOwnershipMarker      = errors.New("workspace ownership marker failed")
	// ErrInvalidBranchName lets HTTP handlers map a rejected branch to a
	// validation response with errors.Is instead of matching on the git
	// message this error carries.
	ErrInvalidBranchName = errors.New("invalid branch name")
)

func workspaceUsesOriginHead(ws *Workspace) bool {
	return ws.ItemType == db.WorkspaceItemTypeIssue ||
		ws.ItemType == db.WorkspaceItemTypeKataTask ||
		ws.ItemType == db.WorkspaceItemTypeAdHoc
}

type TerminalPaneSnapshot struct {
	Title  string
	Output string
}

// NewManager creates a Manager that stores worktrees under
// worktreeDir.
func NewManager(
	database *db.DB, worktreeDir string,
) *Manager {
	if worktreeDir != "" {
		if abs, err := filepath.Abs(worktreeDir); err == nil {
			worktreeDir = abs
		}
	}
	return &Manager{
		db:                     database,
		worktreeDir:            worktreeDir,
		locks:                  NewFileLockManager(),
		retryQueued:            make(map[string]bool),
		issueBranchSlugEnabled: true,
		now:                    time.Now,
	}
}

// SetLaunchSpecResolver binds the hub-owned provider-fact authority
// used by provider-backed creation and lease renewal.
func (m *Manager) SetLaunchSpecResolver(resolver providerplane.WorkspaceLaunchSpecResolver) {
	m.launchSpecResolver = resolver
}

// SetNow overrides the clock used for launch-spec leases.
func (m *Manager) SetNow(now func() time.Time) {
	if now == nil {
		m.now = time.Now
		return
	}
	m.now = now
}

// SetIssueBranchSlugEnabled controls whether issue-workspace branch
// names include a slug derived from the issue title. When false, the
// hub issues the bare kenn-forge/issue-<n> form. Default is
// true, matching the configured default issue_workspace_branch_style.
func (m *Manager) SetIssueBranchSlugEnabled(enabled bool) {
	m.issueBranchSlugEnabled = enabled
}

// SetWorktreeBasePathResolver configures the optional local-repository
// resolver used when a tracked remote repo should create worktrees from a
// user-openable checkout instead of kenn-forge's managed bare clone.
func (m *Manager) SetWorktreeBasePathResolver(resolver WorktreeBasePathResolver) {
	m.worktreeBaseResolver = resolver
}

// SetClones sets the git clone manager used for bare clone
// operations. Called after the clone manager is initialized.
func (m *Manager) SetClones(clones *gitclone.Manager) {
	m.clones = clones
}

// SetRequireProviderCredential makes provider-backed workspace network Git
// fail closed when the executing federation spoke loses its credential route.
func (m *Manager) SetRequireProviderCredential(required bool) {
	m.requireProviderCredential = required
}

// SetRoborevEndpoint binds the startup-scoped daemon endpoint used for
// managed-clone initialization. Endpoint edits require a server restart.
func (m *Manager) SetRoborevEndpoint(endpoint string) {
	m.roborevEndpoint = endpoint
}

// SetRoborevRepositoryInvalidator configures the cache invalidation callback
// invoked after a managed workspace is confirmed in the daemon inventory.
func (m *Manager) SetRoborevRepositoryInvalidator(invalidate func()) {
	m.roborevRepositoryInvalidator = invalidate
}

// withRepoLock acquires a repository-scoped lock, executes the function, and
// releases the lock. The lock is released even if the function panics.
func (m *Manager) withRepoLock(ctx context.Context, lockRoot string, fn func() error) error {
	if err := os.MkdirAll(lockRoot, 0o755); err != nil {
		return fmt.Errorf("prepare worktree lock for %q: %w", lockRoot, err)
	}
	lock, err := m.locks.Acquire(ctx, lockRoot)
	if err != nil {
		return fmt.Errorf("acquire worktree lock for %q: %w", lockRoot, err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			slog.Warn("failed to release worktree lock",
				"path", lockRoot, "err", err)
		}
	}()
	return fn()
}

func (m *Manager) withRepoLockForGitDir(
	ctx context.Context, gitDir string, fn func() error,
) error {
	lockRoot, err := m.worktreeLockRoot(ctx, gitDir)
	if err != nil {
		return err
	}
	return m.withRepoLock(ctx, lockRoot, fn)
}

func (m *Manager) worktreeLockRoot(ctx context.Context, gitDir string) (string, error) {
	bare, err := gitIsBareRepository(ctx, gitDir)
	if err != nil {
		return "", err
	}
	if bare {
		return gitDir, nil
	}
	commonDir, err := worktreeCommonGitDir(ctx, gitDir)
	if err != nil {
		return "", err
	}
	return m.localWorktreeBaseLockRoot(commonDir), nil
}

// SetTmuxCommand sets the command + argv prefix for every tmux
// invocation the manager issues. When nil/empty, the manager uses
// config.DefaultTmuxCommand — the dedicated kenn-forge tmux server.
func (m *Manager) SetTmuxCommand(cmd []string) {
	m.tmuxCmd = slices.Clone(cmd)
}

// UpdateTmuxStripEnvVars merges names into the configured provider
// token env var names stripped from tmux client and base-pane
// environments; the client allowlist admits LC_ and XDG_ prefixes a
// configured token name could hide under. Names accumulate
// monotonically so reload ordering can never stop stripping a
// previously configured name.
func (m *Manager) UpdateTmuxStripEnvVars(names []string) {
	m.tmuxStripEnvMu.Lock()
	defer m.tmuxStripEnvMu.Unlock()
	merged := slices.Clone(m.tmuxStripEnvVars)
	for _, name := range names {
		// Never accumulate non-secret terminal variables: validation
		// rejects such token names, but a rejected candidate's names
		// are accumulated before rejection, and stripping PATH or
		// TMUX_TMPDIR would break terminals and socket routing.
		if name == "" || config.IsTmuxNonSecretEnvVar(name) ||
			slices.Contains(merged, name) {
			continue
		}
		merged = append(merged, name)
	}
	m.tmuxStripEnvVars = merged
}

func (m *Manager) currentTmuxStripEnvVars() []string {
	m.tmuxStripEnvMu.RLock()
	defer m.tmuxStripEnvMu.RUnlock()
	return m.tmuxStripEnvVars
}

// TmuxStripEnvVars returns the accumulated configured token env var
// names. The returned slice is a copy, safe for callers to retain.
func (m *Manager) TmuxStripEnvVars() []string {
	if m == nil {
		return nil
	}
	m.tmuxStripEnvMu.RLock()
	defer m.tmuxStripEnvMu.RUnlock()
	return slices.Clone(m.tmuxStripEnvVars)
}

// SetHideTmuxStatus controls whether newly-created tmux sessions hide
// tmux's own status line.
func (m *Manager) SetHideTmuxStatus(hide bool) {
	m.hideTmuxStatusMu.Lock()
	defer m.hideTmuxStatusMu.Unlock()
	m.hideTmuxStatus = hide
}

func (m *Manager) currentHideTmuxStatus() bool {
	m.hideTmuxStatusMu.RLock()
	defer m.hideTmuxStatusMu.RUnlock()
	return m.hideTmuxStatus
}

// SetTmuxGraphics controls graphics passthrough for managed workspace sessions.
func (m *Manager) SetTmuxGraphics(enabled bool) {
	m.tmuxGraphicsMu.Lock()
	defer m.tmuxGraphicsMu.Unlock()
	m.tmuxGraphics = enabled
}

func (m *Manager) currentTmuxGraphics() bool {
	m.tmuxGraphicsMu.RLock()
	defer m.tmuxGraphicsMu.RUnlock()
	return m.tmuxGraphics
}

// SetTmuxMouse controls tmux mouse handling for managed workspace sessions.
func (m *Manager) SetTmuxMouse(enabled bool) {
	m.tmuxMouseMu.Lock()
	defer m.tmuxMouseMu.Unlock()
	m.tmuxMouse = enabled
}

func (m *Manager) currentTmuxMouse() bool {
	m.tmuxMouseMu.RLock()
	defer m.tmuxMouseMu.RUnlock()
	return m.tmuxMouse
}

// tmuxExec builds an *exec.Cmd for a tmux invocation: the
// configured prefix + extra args. Defaults to
// config.DefaultTmuxCommand when unconfigured. Returning the
// *exec.Cmd directly (rather than a []string that callers index)
// keeps the first-element access inside this function where the
// branch structure makes it statically safe — NilAway cannot prove
// safety through an indexed slice return.
func (m *Manager) tmuxExec(
	ctx context.Context, extra ...string,
) *exec.Cmd {
	command := m.tmuxCmd
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	args := make([]string, 0, len(command)-1+len(extra))
	args = append(args, command[1:]...)
	args = append(args, extra...)
	cmd := procutil.CommandContext(ctx, command[0], args...)
	// Any invocation can be the one that spawns the tmux server, which
	// retains its spawn environment globally; give the client only the
	// non-secret allowlist so no credential can ever enter it.
	cmd.Env = localruntime.TmuxClientEnvironment(
		os.Environ(), m.currentTmuxStripEnvVars(),
	)
	return cmd
}

// Create persists a PR-backed kenn-forge workspace.
//
// The point of this row is to give a tracked pull request a stable local
// workspace entry that the UI can reopen later, rather than rediscovering local
// Git state on every load. The caller runs Setup in the background to
// materialize the worktree and tmux session.
func (m *Manager) Create(
	ctx context.Context,
	provider, platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	if m.launchSpecResolver == nil {
		return nil, ErrLaunchSpecResolverMissing
	}
	spec, err := m.launchSpecResolver.ResolveWorkspaceLaunchSpec(ctx, launchRequest(
		provider, platformHost, owner, name,
		db.WorkspaceItemTypePullRequest, mrNumber, "", false,
	))
	if err != nil {
		return nil, err
	}
	return m.CreateFromLaunchSpec(ctx, spec)
}

// CreateFromLaunchSpec atomically persists a PR-backed workspace and the
// hub-issued provider facts that every later lifecycle step consumes.
func (m *Manager) CreateFromLaunchSpec(
	ctx context.Context, spec WorkspaceLaunchSpec,
) (*Workspace, error) {
	if spec.ItemType != db.WorkspaceItemTypePullRequest {
		return nil, errors.New("pull-request workspace requires a pull launch specification")
	}
	if err := validateLaunchSpecForCreation(spec); err != nil {
		return nil, err
	}
	if err := spec.RequireVisible(m.launchSpecNow()); err != nil {
		return nil, err
	}
	if existing, err := m.GetByLaunchSpecIdentity(ctx, spec); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceDuplicate, existing.ID)
	}
	repo, err := m.repositoryForLaunchSpec(ctx, spec)
	if err != nil {
		return nil, err
	}
	id, err := newWorkspaceID()
	if err != nil {
		return nil, err
	}

	ws := &Workspace{
		ID:              id,
		Platform:        spec.Repository.Provider,
		PlatformHost:    spec.Repository.PlatformHost,
		RepoOwner:       spec.Repository.Owner,
		RepoName:        spec.Repository.Name,
		RepoID:          repo.ID,
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      spec.ItemNumber,
		ItemKey:         spec.ItemKey,
		GitHeadRef:      spec.GitHeadRef,
		MRHeadRepo:      workspaceHeadRepoFromLaunchSpec(spec),
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath: m.newWorkspacePath(
			workspaceRepoRef{
				ID: repo.ID, Platform: spec.Repository.Provider,
				PlatformHost: spec.Repository.PlatformHost,
				Owner:        spec.Repository.Owner, Name: spec.Repository.Name,
			},
			fmt.Sprintf("pr-%d", spec.ItemNumber),
		),
		TmuxSession:     "forge-" + id,
		TerminalBackend: m.PreferredTerminalBackend(),
		Status:          "creating",
	}

	if err := m.db.CreateWorkspaceWithLaunchSpec(ctx, ws, spec); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %v", ErrWorkspaceDuplicate, err)
		}
		return nil, fmt.Errorf("insert workspace and launch specification: %w", err)
	}
	return ws, nil
}

// CreateIssue persists an issue-backed kenn-forge workspace.
//
// Unlike PR workspaces, issue workspaces are not tied to a remote head branch.
// They exist to give an issue its own durable local execution context that
// starts from the repo's current origin/HEAD. The caller runs Setup in the
// background to materialize the worktree and tmux session.
func (m *Manager) CreateIssue(
	ctx context.Context,
	platformHost, owner, name string,
	issueNumber int,
	opts CreateIssueOptions,
) (*Workspace, error) {
	if m.launchSpecResolver == nil {
		return nil, ErrLaunchSpecResolverMissing
	}
	spec, err := m.launchSpecResolver.ResolveWorkspaceLaunchSpec(ctx, launchRequest(
		opts.Provider, platformHost, owner, name,
		db.WorkspaceItemTypeIssue, issueNumber, opts.GitHeadRef,
		m.issueBranchSlugEnabled,
	))
	if err != nil {
		return nil, err
	}
	return m.CreateIssueFromLaunchSpec(ctx, spec, opts)
}

// CreateIssueFromLaunchSpec atomically persists an issue-backed workspace and
// its hub-issued source facts while retaining spoke-local branch and
// directory reuse decisions.
func (m *Manager) CreateIssueFromLaunchSpec(
	ctx context.Context,
	spec WorkspaceLaunchSpec,
	opts CreateIssueOptions,
) (*Workspace, error) {
	if spec.ItemType != db.WorkspaceItemTypeIssue {
		return nil, errors.New("issue workspace requires an issue launch specification")
	}
	if err := validateLaunchSpecForCreation(spec); err != nil {
		return nil, err
	}
	if err := spec.RequireVisible(m.launchSpecNow()); err != nil {
		return nil, err
	}
	if existing, err := m.GetByLaunchSpecIdentity(ctx, spec); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceDuplicate, existing.ID)
	}
	repo, err := m.repositoryForLaunchSpec(ctx, spec)
	if err != nil {
		return nil, err
	}
	gitHeadRef := spec.GitHeadRef
	if err := validateLocalBranchName(ctx, "", gitHeadRef); err != nil {
		return nil, err
	}
	if opts.ReuseExistingBranch && opts.ReuseExistingDirectory {
		return nil, errors.New(
			"reuse existing branch and directory are mutually exclusive",
		)
	}

	id, err := newWorkspaceID()
	if err != nil {
		return nil, err
	}

	ws := &Workspace{
		ID:              id,
		Platform:        spec.Repository.Provider,
		PlatformHost:    spec.Repository.PlatformHost,
		RepoOwner:       spec.Repository.Owner,
		RepoName:        spec.Repository.Name,
		RepoID:          repo.ID,
		ItemType:        db.WorkspaceItemTypeIssue,
		ItemNumber:      spec.ItemNumber,
		ItemKey:         spec.ItemKey,
		GitHeadRef:      gitHeadRef,
		WorkspaceBranch: gitHeadRef,
		WorktreePath: m.newWorkspacePath(
			workspaceRepoRef{
				ID: repo.ID, Platform: spec.Repository.Provider,
				PlatformHost: spec.Repository.PlatformHost,
				Owner:        spec.Repository.Owner, Name: spec.Repository.Name,
			},
			fmt.Sprintf("issue-%d", spec.ItemNumber),
		),
		TmuxSession:     "forge-" + id,
		TerminalBackend: m.PreferredTerminalBackend(),
		Status:          "creating",
	}
	existingDirectoryBranch, directoryErr := m.inspectExistingWorkspaceDirectory(
		ctx, ws,
	)
	if directoryErr == nil {
		if !opts.ReuseExistingDirectory {
			suggested, err := nextAvailableBranchName(
				ctx, ws.WorktreePath, existingDirectoryBranch,
			)
			if err != nil {
				return nil, fmt.Errorf("suggest branch name: %w", err)
			}
			return nil, &WorkspaceBranchConflictError{
				Branch:            existingDirectoryBranch,
				SuggestedBranch:   suggested,
				ExistingDirectory: true,
			}
		}
		if existingDirectoryBranch != gitHeadRef {
			return nil, &WorkspaceDirectoryRecoveryError{
				Reason:         WorkspaceDirectoryBranchMismatch,
				ExpectedBranch: gitHeadRef,
				ActualBranch:   existingDirectoryBranch,
			}
		}
		ws.WorkspaceBranch = workspaceBranchRecoveryPending
	} else if opts.ReuseExistingDirectory {
		return nil, directoryErr
	} else {
		if _, ok := errors.AsType[*WorkspaceDirectoryRecoveryError](directoryErr); !ok {
			return nil, directoryErr
		}
	}

	if !opts.ReuseExistingDirectory {
		branchDir, ok, localBase, err := m.branchInspectionDir(ctx, workspaceRepoRef{
			ID: repo.ID, Platform: spec.Repository.Provider,
			PlatformHost: spec.Repository.PlatformHost,
			ProviderID:   spec.Repository.PlatformRepoID,
			Owner:        spec.Repository.Owner, Name: spec.Repository.Name,
			RemoteURL: spec.Repository.CloneURL,
		})
		if err != nil {
			return nil, err
		}
		if ok {
			branch, err := workspaceBranchForExistingLocalBranch(
				ctx, branchDir, gitHeadRef, opts.ReuseExistingBranch,
				localBase,
			)
			if err != nil {
				return nil, err
			}
			ws.WorkspaceBranch = branch
		}
	}

	if err := m.db.CreateWorkspaceWithLaunchSpec(ctx, ws, spec); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %v", ErrWorkspaceDuplicate, err)
		}
		return nil, fmt.Errorf("insert workspace and launch specification: %w", err)
	}
	return ws, nil
}

func (m *Manager) repositoryForLaunchSpec(
	ctx context.Context, spec WorkspaceLaunchSpec,
) (*db.Repo, error) {
	repo, err := m.workspaceRepo(
		ctx, spec.Repository.Provider, spec.Repository.PlatformHost,
		spec.Repository.Owner, spec.Repository.Name,
	)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		if err := m.verifyRepoRouteUnoccupied(
			ctx, spec.Repository.Provider, spec.Repository.PlatformHost,
			spec.Repository.Owner, spec.Repository.Name,
		); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: repository not tracked", ErrWorkspaceNotFound)
	}
	if strings.TrimSpace(repo.PlatformRepoID) !=
		strings.TrimSpace(spec.Repository.PlatformRepoID) {
		return nil, fmt.Errorf(
			"%w: workspace repository identity changed for route: %s/%s",
			db.ErrRepositoryRouteFenceChanged,
			spec.Repository.Owner, spec.Repository.Name,
		)
	}
	return repo, nil
}

func (m *Manager) validateExistingWorkspaceDirectory(
	ctx context.Context, ws *Workspace,
) error {
	actualBranch, err := m.inspectExistingWorkspaceDirectory(ctx, ws)
	if err != nil {
		return err
	}
	if actualBranch != ws.GitHeadRef {
		return &WorkspaceDirectoryRecoveryError{
			Reason:         WorkspaceDirectoryBranchMismatch,
			ExpectedBranch: ws.GitHeadRef,
			ActualBranch:   actualBranch,
		}
	}
	return nil
}

func (m *Manager) inspectExistingWorkspaceDirectory(
	ctx context.Context, ws *Workspace,
) (string, error) {
	info, err := os.Lstat(ws.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryMissing,
		}
	}
	if err != nil {
		return "", fmt.Errorf("stat expected workspace directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryNotLinkedWorktree,
		}
	}
	commonDir, err := worktreeCommonGitDir(ctx, ws.WorktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return "", &WorkspaceDirectoryRecoveryError{
				Reason: WorkspaceDirectoryNotLinkedWorktree,
			}
		}
		return "", fmt.Errorf("inspect expected workspace directory: %w", err)
	}
	owned, err := gitDirOwnsLinkedWorktree(ctx, commonDir, ws.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("inspect expected linked worktree: %w", err)
	}
	if !owned {
		return "", &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryNotLinkedWorktree,
		}
	}
	prov, err := m.existingWorkspaceWorktreeProvenance(
		ctx, commonDir, ws,
	)
	if err != nil {
		return "", fmt.Errorf("validate expected workspace provenance: %w", err)
	}
	if !prov.reusable {
		return "", &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryRepositoryMismatch,
		}
	}
	actualBranch, err := worktreeCurrentBranch(ctx, ws.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("inspect expected workspace branch: %w", err)
	}
	return actualBranch, nil
}

func (m *Manager) CreateKataTask(
	ctx context.Context,
	provider, platformHost, owner, name string,
	metadata db.WorkspaceKataMetadata,
) (*Workspace, error) {
	metadata.DaemonID = strings.TrimSpace(metadata.DaemonID)
	metadata.ProjectUID = strings.TrimSpace(metadata.ProjectUID)
	metadata.ProjectName = strings.TrimSpace(metadata.ProjectName)
	metadata.IssueUID = strings.TrimSpace(metadata.IssueUID)
	metadata.ShortID = strings.TrimSpace(metadata.ShortID)
	metadata.QualifiedID = strings.TrimSpace(metadata.QualifiedID)
	metadata.Title = strings.TrimSpace(metadata.Title)
	if metadata.DaemonID == "" {
		return nil, errors.New("kata daemon_id is required")
	}
	if metadata.ProjectUID == "" {
		return nil, errors.New("kata project_uid is required")
	}
	if metadata.IssueUID == "" {
		return nil, errors.New("kata issue_uid is required")
	}
	itemKey := db.KataWorkspaceItemKey(metadata)
	if itemKey == "" {
		return nil, errors.New("kata workspace item_key is required")
	}

	repo, err := m.workspaceRepo(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, fmt.Errorf("look up repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("repository not tracked")
	}

	branchID := kataTaskBranchID(metadata)
	gitHeadRef := kataWorkspaceBranch(branchID, metadata.Title)
	if err := validateLocalBranchName(ctx, "", gitHeadRef); err != nil {
		return nil, err
	}

	id, err := newWorkspaceID()
	if err != nil {
		return nil, err
	}

	ws := &Workspace{
		ID:              id,
		Platform:        repo.Platform,
		PlatformHost:    platformHost,
		RepoOwner:       owner,
		RepoName:        name,
		RepoID:          repo.ID,
		ItemType:        db.WorkspaceItemTypeKataTask,
		ItemKey:         itemKey,
		GitHeadRef:      gitHeadRef,
		WorkspaceBranch: gitHeadRef,
		WorktreePath: m.newWorkspacePath(
			workspaceRepoRef{
				ID: repo.ID, Platform: repo.Platform, PlatformHost: platformHost,
				Owner: owner, Name: name,
			},
			"kata-"+branchID,
		),
		TmuxSession:     "forge-" + id,
		TerminalBackend: m.PreferredTerminalBackend(),
		Status:          "creating",
		KataMetadata:    &metadata,
	}

	if err := m.db.InsertWorkspace(ctx, ws); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %v", ErrWorkspaceDuplicate, err)
		}
		return nil, fmt.Errorf("insert workspace: %w", err)
	}
	return ws, nil
}

// CreateAdHoc persists a workspace for new work in a tracked repository that
// has no pull request, provider issue, or Kata task behind it.
//
// The branch is the workspace's only identity: it becomes the item key, so a
// second create for the same branch in the same repository collides on the
// workspace unique index instead of producing a duplicate worktree. Like
// issue-backed workspaces these start from the repository's current
// origin/HEAD; the caller runs Setup in the background to materialize the
// worktree and tmux session.
func (m *Manager) CreateAdHoc(
	ctx context.Context,
	provider, platformHost, owner, name string,
	opts CreateAdHocOptions,
) (*Workspace, error) {
	repo, err := m.workspaceRepo(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, fmt.Errorf("look up repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("%w: repository not tracked", ErrWorkspaceNotFound)
	}

	id, err := newWorkspaceID()
	if err != nil {
		return nil, err
	}

	gitHeadRef := strings.TrimSpace(opts.BranchName)
	if gitHeadRef == "" {
		gitHeadRef = adHocWorkspaceBranch(id)
	}
	requestedBranch := gitHeadRef
	if err := validateLocalBranchName(ctx, "", gitHeadRef); err != nil {
		return nil, err
	}

	workspaceBranch := gitHeadRef
	nextHashAttempt := 0
	branchDir, ok, localBase, err := m.branchInspectionDir(ctx, workspaceRepoRef{
		ID: repo.ID, Platform: repo.Platform, PlatformHost: platformHost,
		ProviderID: repo.PlatformRepoID, Owner: owner, Name: name,
		RemoteURL: workspaceCloneRemoteURL(repo, platformHost, owner, name),
	})
	if err != nil {
		return nil, err
	}
	if ok {
		branch, err := workspaceBranchForExistingLocalBranch(
			ctx, branchDir, gitHeadRef, opts.ReuseExistingBranch, localBase,
		)
		if err != nil {
			if _, ok := errors.AsType[*WorkspaceBranchConflictError](err); !ok {
				return nil, err
			}
			branch, nextHashAttempt, err = nextAvailableAdHocBranchName(
				ctx, branchDir, requestedBranch, id, nextHashAttempt,
			)
			if err != nil {
				return nil, err
			}
		}
		workspaceBranch = branch
	}
	if workspaceBranch != "" {
		gitHeadRef = workspaceBranch
	}

	ws := &Workspace{
		ID:              id,
		Platform:        repo.Platform,
		PlatformHost:    platformHost,
		RepoOwner:       owner,
		RepoName:        name,
		RepoID:          repo.ID,
		ItemType:        db.WorkspaceItemTypeAdHoc,
		TmuxSession:     "forge-" + id,
		TerminalBackend: m.PreferredTerminalBackend(),
		Status:          "creating",
	}
	m.setAdHocWorkspaceIdentity(ws, gitHeadRef, workspaceBranch)

	if err := m.persistAdHocWorkspace(
		ctx, ws, branchDir, requestedBranch, nextHashAttempt,
	); err != nil {
		return nil, fmt.Errorf("insert workspace: %w", err)
	}
	return ws, nil
}

func (m *Manager) persistAdHocWorkspace(
	ctx context.Context,
	ws *Workspace,
	branchDir, requestedBranch string,
	nextHashAttempt int,
) error {
	for {
		err := m.db.InsertWorkspace(ctx, ws)
		if err == nil {
			return nil
		}
		if !isUniqueConstraintError(err) {
			return err
		}
		if nextHashAttempt == 0 {
			return fmt.Errorf("%w: %v", ErrWorkspaceDuplicate, err)
		}

		branch, nextAttempt, nameErr := nextAvailableAdHocBranchName(
			ctx, branchDir, requestedBranch, ws.ID, nextHashAttempt,
		)
		if nameErr != nil {
			return nameErr
		}
		m.setAdHocWorkspaceIdentity(ws, branch, branch)
		nextHashAttempt = nextAttempt
	}
}

func (m *Manager) setAdHocWorkspaceIdentity(
	ws *Workspace, identityBranch, managedBranch string,
) {
	ws.GitHeadRef = identityBranch
	ws.WorkspaceBranch = managedBranch
	ws.ItemKey = db.AdHocWorkspaceItemKey(identityBranch)
	ws.WorktreePath = m.newWorkspacePath(
		workspaceRepoRef{
			ID: ws.RepoID, Platform: ws.Platform, PlatformHost: ws.PlatformHost,
			Owner: ws.RepoOwner, Name: ws.RepoName,
		},
		adHocWorktreeDirName(identityBranch),
	)
}

func (m *Manager) newWorkspacePath(
	repo workspaceRepoRef, itemDir string,
) string {
	return filepath.Join(
		m.worktreeDir, repo.Platform, repo.PlatformHost, repo.Owner, repo.Name,
		fmt.Sprintf("repo-%d", repo.ID), itemDir,
	)
}

// adHocWorkspaceBranch names a branch for work the user did not name. The
// workspace ID is already unique, so its prefix keeps generated branches from
// colliding without needing a repository round-trip.
func adHocWorkspaceBranch(workspaceID string) string {
	suffix := workspaceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "kenn-forge/work-" + suffix
}

// adHocWorktreeDirName derives a filesystem-safe directory name from the
// branch. The slug alone is not injective (slashes and punctuation collapse),
// so a branch hash keeps two distinct branches in distinct worktrees.
func adHocWorktreeDirName(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	hash := hex.EncodeToString(sum[:])[:8]
	slug := truncateSlug(slugifyIssueTitle(branch), 48-len(hash)-1)
	if slug == "" {
		return "work-" + hash
	}
	return "work-" + slug + "-" + hash
}

func kataTaskBranchID(metadata db.WorkspaceKataMetadata) string {
	scopeHash := kataTaskScopeHash(metadata)
	for _, candidate := range []string{metadata.ShortID, metadata.QualifiedID, metadata.IssueUID} {
		if slug := slugifyIssueTitle(candidate); slug != "" {
			slug = truncateSlug(slug, 48-len(scopeHash)-1)
			if slug != "" {
				return slug + "-" + scopeHash
			}
		}
	}
	return "task-" + scopeHash
}

func kataTaskScopeHash(metadata db.WorkspaceKataMetadata) string {
	sum := sha256.Sum256([]byte(
		metadata.DaemonID + "\x00" + metadata.ProjectUID + "\x00" + metadata.IssueUID,
	))
	return hex.EncodeToString(sum[:])[:8]
}

func kataWorkspaceBranch(branchID, title string) string {
	bare := "kenn-forge/kata/" + branchID
	slug := slugifyIssueTitle(title)
	if slug == "" {
		return bare
	}
	budget := issueBranchMaxLen - issueBranchSlugBudget - len(bare) - len("-")
	if budget <= 0 {
		return bare
	}
	if len(slug) > budget {
		slug = truncateSlug(slug, budget)
		if slug == "" {
			return bare
		}
	}
	return bare + "-" + slug
}

func newWorkspaceID() (string, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("generate workspace id: %w", err)
	}
	return hex.EncodeToString(idBytes), nil
}

type workspaceRepoRef struct {
	ID           int64
	Platform     string
	PlatformHost string
	ProviderID   string
	Owner        string
	Name         string
	RemoteURL    string
}

func (m *Manager) branchInspectionDir(
	ctx context.Context, repo workspaceRepoRef,
) (dir string, ok bool, localBase bool, err error) {
	if base, ok, err := m.localWorktreeBaseDir(ctx, repo); err != nil || ok {
		return base.Path, ok, ok, err
	}
	if m.clones == nil {
		return "", false, false, nil
	}

	var validateRoute func(context.Context) error
	if repo.ID == 0 {
		validateRoute = func(validationCtx context.Context) error {
			return m.verifyRepoRouteUnoccupied(
				validationCtx, repo.Platform, repo.PlatformHost, repo.Owner, repo.Name,
			)
		}
	} else {
		identity := db.RepoIdentity{
			Platform: repo.Platform, PlatformHost: repo.PlatformHost,
			Owner: repo.Owner, Name: repo.Name,
		}
		fence, found, fenceErr := m.db.CurrentRepositoryRouteFence(
			ctx, identity, repo.ID,
		)
		if fenceErr != nil {
			return "", false, false, fenceErr
		}
		if !found {
			return "", false, false, fmt.Errorf(
				"%w: workspace repository route changed",
				db.ErrRepositoryRouteFenceChanged,
			)
		}
		validateRoute = m.workspaceCloneRouteValidator(identity, repo.ID, fence)
	}
	cloneCtx := gitclone.WithRepositoryIdentity(ctx, repo.ProviderID)
	if err := m.clones.EnsureCloneValidated(
		cloneCtx, repo.Platform, repo.PlatformHost, repo.Owner, repo.Name, repo.RemoteURL,
		validateRoute,
	); err != nil {
		return "", false, false, fmt.Errorf("ensure clone: %w", err)
	}

	cloneDir, err := m.clones.ClonePathForContext(
		cloneCtx, repo.Platform, repo.PlatformHost, repo.Owner, repo.Name,
	)
	if err != nil {
		return "", false, false, err
	}
	return cloneDir, true, false, nil
}

func workspaceCloneRemoteURL(
	repo *db.Repo, platformHost, owner, name string,
) string {
	if repo != nil {
		if cloneURL := strings.TrimSpace(repo.CloneURL); cloneURL != "" {
			return cloneURL
		}
	}
	return fmt.Sprintf("https://%s/%s/%s.git", platformHost, owner, name)
}

func workspaceBranchForExistingLocalBranch(
	ctx context.Context, dir, branch string, reuse, localBase bool,
) (string, error) {
	exists, err := localBranchExists(ctx, dir, branch)
	if err != nil {
		return "", fmt.Errorf("inspect local branch: %w", err)
	}
	if !exists {
		available, err := localBranchNameAvailable(ctx, dir, branch)
		if err != nil {
			return "", fmt.Errorf("inspect local branch namespace: %w", err)
		}
		if available {
			return branch, nil
		}
		return "", workspaceBranchConflict(ctx, dir, branch)
	}
	if reuse && !localBase {
		return "", nil
	}
	if reuse {
		checkedOut, err := localBranchCheckedOut(ctx, dir, branch)
		if err != nil {
			return "", fmt.Errorf("inspect checked out branch: %w", err)
		}
		if !checkedOut {
			return "", nil
		}
	}
	return "", workspaceBranchConflict(ctx, dir, branch)
}

func workspaceBranchConflict(
	ctx context.Context, dir, branch string,
) error {
	suggested, err := nextAvailableBranchName(ctx, dir, branch)
	if err != nil {
		return fmt.Errorf("suggest branch name: %w", err)
	}
	return &WorkspaceBranchConflictError{
		Branch:          branch,
		SuggestedBranch: suggested,
	}
}

// WorkspaceHeadRepo classifies a pull-request head repository against its
// base repository identity: nil means confirmed same-repo, a non-nil empty
// string means the head repository identity could not be determined from
// cloneURL, and a non-nil clone URL means the head lives in a fork. Exported
// so the sync engine can reclassify workspace trust rows when a later sync
// changes an MR's head_repo_clone_url, without duplicating this logic.
func WorkspaceHeadRepo(provider, platformHost, owner, name, cloneURL string) *string {
	// MRHeadRepo means "this PR head must be resolved through fork-safe refs"
	// in setup. GitHub also fills head.repo.clone_url for same-repo PRs, so
	// compare clone identities before treating a non-empty URL as fork metadata.
	headRepo := normalizeCloneRepoIdentity(provider, cloneURL)
	if headRepo == "" {
		unknown := ""
		return &unknown
	}
	baseRepo := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(provider),
		normalizePlatformHostIdentity(platformHost),
		strings.TrimSpace(owner),
		strings.TrimSpace(name),
	}, "/"))
	if headRepo == baseRepo {
		return nil
	}
	s := cloneURL
	return &s
}

// Setup clones/fetches the repo, creates the git worktree, starts
// a tmux session, and marks the workspace "ready". On failure it
// rolls back the worktree and sets status to "error".
func (m *Manager) Setup(
	ctx context.Context, ws *Workspace,
) error {
	return m.SetupWithOptions(ctx, ws, SetupOptions{})
}

// SetupOptions carries the committed per-attempt configuration and the exact
// user-selected worktree base path into workspace setup.
type SetupOptions struct {
	WorktreeBasePath         string
	RoborevInitManagedClones bool
}

func (m *Manager) verifyWorkspaceRepository(
	ctx context.Context, ws *Workspace,
) error {
	return m.verifyWorkspaceRepositoryCurrent(ctx, ws)
}

func (m *Manager) verifyWorkspaceRepositoryUnderReconciliationRead(
	ctx context.Context, ws *Workspace,
) error {
	return m.verifyWorkspaceRepositoryCurrent(ctx, ws)
}

func (m *Manager) verifyWorkspaceRepositoryCurrent(
	ctx context.Context, ws *Workspace,
) error {
	if ws == nil {
		return ErrWorkspaceNotFound
	}
	if m.db == nil {
		return nil
	}
	if ws.ID != "" {
		current, err := m.db.GetWorkspace(ctx, ws.ID)
		if err != nil {
			return fmt.Errorf("reload workspace repository identity: %w", err)
		}
		if current == nil && ws.RepoID != 0 {
			return ErrWorkspaceNotFound
		}
		if current != nil {
			*ws = *current
		}
	}
	if ws.RepoID == 0 {
		return m.verifyRepoRouteUnoccupied(
			ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		)
	}
	repo, err := m.db.GetActiveRepoByID(ctx, ws.RepoID)
	if err != nil {
		return fmt.Errorf("verify workspace repository identity: %w", err)
	}
	if repo == nil {
		return fmt.Errorf(
			"%w: workspace repository identity changed for route: %s/%s",
			db.ErrRepositoryRouteFenceChanged, ws.RepoOwner, ws.RepoName,
		)
	}
	ws.Platform = repo.Platform
	ws.PlatformHost = repo.PlatformHost
	ws.RepoOwner = repo.Owner
	ws.RepoName = repo.Name
	return nil
}

// verifyRepoRouteUnoccupied fails closed on routes with contested history so
// network git operations cannot exchange data with a route's new occupant.
// Managers without a database (unmanaged local checkouts) skip the check.
func (m *Manager) verifyRepoRouteUnoccupied(
	ctx context.Context, provider, platformHost, owner, name string,
) error {
	if m.db == nil {
		return nil
	}
	collision, err := m.db.WorkspaceRepoRouteHasHistoricalOccupants(
		ctx, provider, platformHost, owner, name,
	)
	if err != nil {
		return fmt.Errorf("verify workspace repository route: %w", err)
	}
	if collision {
		return fmt.Errorf(
			"workspace repository route has historical occupants: %s/%s",
			owner, name,
		)
	}
	return nil
}

// SetupWithWorktreeBasePath sets up a workspace from the exact checkout that
// justified a caller's repository resolution. An empty path uses the manager's
// normal repository-identity resolver.
func (m *Manager) SetupWithWorktreeBasePath(
	ctx context.Context, ws *Workspace, worktreeBasePath string,
) error {
	return m.SetupWithOptions(ctx, ws, SetupOptions{WorktreeBasePath: worktreeBasePath})
}

// SetupWithOptions sets up a workspace using a committed configuration
// snapshot captured for this attempt.
func (m *Manager) SetupWithOptions(
	ctx context.Context, ws *Workspace, options SetupOptions,
) error {
	worktreeBasePath := options.WorktreeBasePath
	recoveryPending := workspaceRequiresExistingDirectory(ws)
	m.recordSetupEvent(
		ctx,
		ws.ID, workspaceSetupStageSetup, "started",
		"starting workspace setup",
	)
	// Admission and execution are separate for initial setup, retries, and
	// recovery. Recheck the hub-issued visibility lease at execution
	// time before any network Git access.
	launchSpec, err := m.RequireWorkspaceLaunchSpec(ctx, ws)
	if err != nil {
		return m.failSetup(ctx, ws.ID, workspaceSetupStageSetup, err)
	}
	if launchSpec != nil && m.requireProviderCredential {
		ctx = gitclone.WithRequiredCredential(ctx)
	}
	// Setup is the chokepoint for every path that fetches code — initial
	// creation, retries, and recovery — so reload and verify the workspace's
	// stable repository identity before any Git or provider access.
	if err := m.verifyWorkspaceRepository(ctx, ws); err != nil {
		return m.failSetup(ctx, ws.ID, workspaceSetupStageSetup, err)
	}
	routeIdentity := db.RepoIdentity{
		Platform: ws.Platform, PlatformHost: ws.PlatformHost,
		Owner: ws.RepoOwner, Name: ws.RepoName,
	}
	var routeFence db.RepositoryRouteFence
	var validateCloneRoute func(context.Context) error
	if m.db != nil && ws.RepoID != 0 {
		var found bool
		routeFence, found, err = m.db.CurrentRepositoryRouteFence(
			ctx, routeIdentity, ws.RepoID,
		)
		if err != nil {
			return m.failSetup(ctx, ws.ID, workspaceSetupStageSetup, err)
		}
		if !found {
			return m.failSetup(
				ctx, ws.ID, workspaceSetupStageSetup,
				fmt.Errorf("%w: workspace repository route changed", db.ErrRepositoryRouteFenceChanged),
			)
		}
		validateCloneRoute = m.workspaceCloneRouteValidator(
			routeIdentity, ws.RepoID, routeFence,
		)
	} else {
		validateCloneRoute = func(validationCtx context.Context) error {
			return m.verifyRepoRouteUnoccupied(
				validationCtx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
			)
		}
	}
	if recoveryPending {
		if err := m.validateExistingWorkspaceDirectory(ctx, ws); err != nil {
			return m.failSetup(ctx, ws.ID, workspaceSetupStageWorktree, err)
		}
	}

	reuse, err := m.reuseExistingWorkspaceWorktreeDetails(
		ctx, ws, launchSpec, validateCloneRoute,
	)
	branch, reusedWorktree := reuse.branch, reuse.reused
	var gitDir string
	commonDir, managedClone := reuse.commonDir, reuse.managedClone
	if err != nil {
		if recoveryPending {
			if recoveryErr := m.validateExistingWorkspaceDirectory(ctx, ws); recoveryErr != nil {
				err = recoveryErr
			}
		}
		return m.failSetup(ctx, ws.ID, workspaceSetupStageWorktree, err)
	}
	if recoveryPending && !reusedWorktree {
		recoveryErr := m.validateExistingWorkspaceDirectory(ctx, ws)
		if recoveryErr == nil {
			recoveryErr = &WorkspaceDirectoryRecoveryError{
				Reason: WorkspaceDirectoryNotLinkedWorktree,
			}
		}
		return m.failSetup(
			ctx, ws.ID, workspaceSetupStageWorktree, recoveryErr,
		)
	}
	if !reusedWorktree {
		if err := m.ensureWorkspacePathAvailable(ctx, ws); err != nil {
			return m.failSetup(ctx, ws.ID, workspaceSetupStageWorktree, err)
		}
		var gitSetupDir workspaceGitDir
		gitSetupDir, err = m.workspaceSetupGitDir(
			ctx, ws, worktreeBasePath, launchSpec, validateCloneRoute,
		)
		if err != nil {
			return m.failSetup(
				ctx,
				ws.ID, workspaceSetupStageClone, err,
			)
		}

		gitDir = gitSetupDir.path
		branch, err = m.addWorktree(
			ctx, gitSetupDir, ws, workspaceGitFetchOptions{
				launchSpec: launchSpec, validateRoute: validateCloneRoute,
			},
		)
		if err != nil {
			return m.failSetup(
				ctx,
				ws.ID, workspaceSetupStageWorktree, err,
			)
		}
		commonDir = gitSetupDir.path
		managedClone = !gitSetupDir.localBase
	}
	if ws.ItemType == db.WorkspaceItemTypePullRequest && ws.MRHeadRepo != nil {
		currentBranch, branchErr := worktreeCurrentBranch(ctx, ws.WorktreePath)
		if branchErr == nil && currentBranch != "" {
			branchErr = clearBranchUpstream(ctx, ws.WorktreePath, currentBranch)
		}
		if branchErr != nil {
			if !reusedWorktree {
				m.rollbackWorktree(ctx, gitDir, ws, branch)
			}
			return m.failSetup(
				ctx, ws.ID, workspaceSetupStageWorktree,
				fmt.Errorf("clear untrusted branch upstream: %w", branchErr),
			)
		}
	}
	if m.beforeSetupRouteRevalidation != nil {
		m.beforeSetupRouteRevalidation()
	}
	// The route can be reconciled away while the clone runs (setup holds no
	// reconciliation lock — clones can take minutes), so re-check before
	// declaring the workspace ready.
	var routeErr error
	if routeFence.RepoID != 0 {
		currentFence, found, fenceErr := m.db.CurrentRepositoryRouteFence(
			ctx, routeIdentity, ws.RepoID,
		)
		if fenceErr != nil {
			routeErr = fenceErr
		} else if !found || currentFence != routeFence {
			routeErr = fmt.Errorf(
				"%w: workspace repository route changed during setup",
				db.ErrRepositoryRouteFenceChanged,
			)
		}
	}
	if routeErr == nil {
		routeErr = m.verifyWorkspaceRepository(ctx, ws)
	}
	if routeErr != nil {
		if !reusedWorktree {
			m.rollbackWorktree(ctx, gitDir, ws, branch)
		}
		return m.failSetup(ctx, ws.ID, workspaceSetupStageWorktree, routeErr)
	}
	persistedBranch := branch
	if workspaceUsesOriginHead(ws) && ws.WorkspaceBranch == "" {
		persistedBranch = ""
	}
	if !recoveryPending {
		ws.WorkspaceBranch = persistedBranch
		if err := m.updateWorkspaceBranch(
			ctx, ws.ID, persistedBranch,
		); err != nil {
			if !reusedWorktree {
				m.rollbackWorktree(ctx, gitDir, ws, branch)
			}
			return m.failSetup(
				ctx,
				ws.ID, workspaceSetupStageWorktree, err,
			)
		}
	}

	if managedClone && options.RoborevInitManagedClones {
		m.recordSetupEvent(
			ctx, ws.ID, workspaceSetupStageRepositoryHooks, "started",
			"setting up managed repository hooks",
		)
		if err := m.setupManagedRepositoryHooks(ctx, commonDir, ws); err != nil {
			return m.failSetup(ctx, ws.ID, workspaceSetupStageRepositoryHooks, err)
		}
		m.recordSetupEvent(
			ctx, ws.ID, workspaceSetupStageRepositoryHooks, "success",
			"managed repository hooks ready",
		)
	}

	terminalWorkspace := ws
	if recoveryPending {
		copy := *ws
		copy.WorkspaceBranch = persistedBranch
		terminalWorkspace = &copy
	}
	err = m.newTerminalSession(ctx, terminalWorkspace)
	if err != nil {
		if !reusedWorktree {
			m.rollbackWorktree(ctx, gitDir, ws, branch)
		}
		return m.failSetup(
			ctx,
			ws.ID, workspaceSetupStageTmuxSession, err,
		)
	}
	m.recordSetupEvent(
		ctx,
		ws.ID, workspaceSetupStageTmuxSession, "success",
		"terminal session started",
	)

	// Record the final setup event before flipping status: "ready" is
	// the externally visible completion signal, so observers that poll
	// status must never see "ready" while the event log is still
	// missing its last row. failSetup keeps the same event-then-status
	// order on the error path.
	m.recordSetupEvent(
		ctx,
		ws.ID, workspaceSetupStageSetup, "ready",
		"workspace ready",
	)
	if recoveryPending {
		err = m.completeRecoveredWorkspaceSetup(ctx, ws.ID, persistedBranch)
	} else {
		err = m.updateWorkspaceStatus(ctx, ws.ID, "ready", nil)
	}
	if err != nil {
		return m.failSetup(
			ctx,
			ws.ID, workspaceSetupStageSetup,
			fmt.Errorf("update status to ready: %w", err),
		)
	}
	ws.WorkspaceBranch = persistedBranch
	ws.Status = "ready"
	return nil
}

func workspaceRequiresExistingDirectory(ws *Workspace) bool {
	return ws != nil && ws.WorkspaceBranch == workspaceBranchRecoveryPending
}

// WorkspaceHeadRepoSnapshot binds a workspace's head-repository trust
// classification to the merge-request snapshot from which it was derived.
type WorkspaceHeadRepoSnapshot struct {
	SnapshotRevision int64
	MRHeadRepo       *string
}

// RefreshWorkspaceHeadRepo recomputes a pull-request workspace's head-repo
// trust classification from the current merge request row and persists it
// when it differs from the stored value, mutating ws.MRHeadRepo in place.
func (m *Manager) RefreshWorkspaceHeadRepo(
	ctx context.Context, ws *Workspace,
) error {
	_, err := m.RefreshWorkspaceHeadRepoSnapshot(ctx, ws)
	return err
}

// RefreshWorkspaceHeadRepoSnapshot refreshes the persisted trust
// classification and returns the merge-request snapshot revision used to
// compute it. Non-pull-request workspaces do not have an MR snapshot.
func (m *Manager) RefreshWorkspaceHeadRepoSnapshot(
	ctx context.Context, ws *Workspace,
) (*WorkspaceHeadRepoSnapshot, error) {
	if ws == nil {
		return nil, ErrWorkspaceNotFound
	}
	releaseReconciliation, err :=
		m.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"lock repository reconciliation for head classification: %w",
			err,
		)
	}
	defer releaseReconciliation()
	if ws.ID != "" {
		current, err := m.db.GetWorkspace(ctx, ws.ID)
		if err != nil {
			return nil, fmt.Errorf(
				"reload workspace for head classification: %w", err,
			)
		}
		if current == nil {
			return nil, ErrWorkspaceNotFound
		}
		*ws = *current
	}
	if ws.ID != "" && ws.RepoID == 0 {
		return nil, ErrWorkspaceRepositoryUnresolved
	}
	if ws.ItemType != db.WorkspaceItemTypePullRequest {
		return nil, nil
	}
	for {
		if m.beforeHeadRepoSnapshotRepoLookup != nil {
			m.beforeHeadRepoSnapshotRepoLookup()
		}
		var repo *db.Repo
		if ws.RepoID != 0 {
			repo, err = m.db.GetActiveRepoByID(ctx, ws.RepoID)
		} else {
			repo, err = m.workspaceRepoUnderReconciliationRead(
				ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("look up workspace repo: %w", err)
		}
		if repo == nil {
			if ws.RepoID != 0 {
				return nil, ErrWorkspaceRepositoryUnresolved
			}
			refreshed := WorkspaceHeadRepo(
				ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName, "",
			)
			if m.afterHeadRepoSnapshotRead != nil {
				m.afterHeadRepoSnapshotRead()
			}
			ws.MRHeadRepo = refreshed
			return &WorkspaceHeadRepoSnapshot{
				MRHeadRepo: refreshed,
			}, nil
		}
		mr, lookupErr := m.db.GetMergeRequestByRepoIDAndNumber(
			ctx, repo.ID, ws.ItemNumber,
		)
		if lookupErr != nil {
			return nil, fmt.Errorf("look up workspace merge request: %w", lookupErr)
		}
		cloneURL := ""
		var snapshotRevision int64
		removed, visibilityErr := m.db.IsArchiveItemRemovedUpstream(
			ctx, repo.ID, db.ArchiveItemTypeMergeRequest, ws.ItemNumber,
		)
		if visibilityErr != nil {
			return nil, fmt.Errorf(
				"check workspace merge request visibility: %w", visibilityErr,
			)
		}
		if mr != nil {
			snapshotRevision = mr.SnapshotRevision
			if !removed {
				cloneURL = mr.HeadRepoCloneURL
			}
			if !removed && mr.HeadRepoIdentityStale {
				return &WorkspaceHeadRepoSnapshot{
					SnapshotRevision: snapshotRevision,
					MRHeadRepo:       ws.MRHeadRepo,
				}, nil
			}
		}
		refreshed := WorkspaceHeadRepo(
			ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName, cloneURL,
		)
		if m.afterHeadRepoSnapshotRead != nil {
			m.afterHeadRepoSnapshotRead()
		}
		if ws.ID == "" {
			ws.MRHeadRepo = refreshed
			return &WorkspaceHeadRepoSnapshot{
				SnapshotRevision: snapshotRevision,
				MRHeadRepo:       refreshed,
			}, nil
		}
		applied, updateErr := m.db.UpdateWorkspaceMRHeadRepoForSnapshot(
			ctx,
			ws.ID,
			repo.ID,
			ws.ItemNumber,
			snapshotRevision,
			removed,
			refreshed,
		)
		if updateErr != nil {
			return nil, fmt.Errorf(
				"persist refreshed head repository: %w", updateErr,
			)
		}
		if !applied {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		ws.MRHeadRepo = refreshed
		return &WorkspaceHeadRepoSnapshot{
			SnapshotRevision: snapshotRevision,
			MRHeadRepo:       refreshed,
		}, nil
	}
}

type existingWorkspaceWorktreeResult struct {
	branch       string
	commonDir    string
	managedClone bool
	reused       bool
}

func (m *Manager) reuseExistingWorkspaceWorktree(
	ctx context.Context, ws *Workspace, launchSpec *WorkspaceLaunchSpec,
) (string, bool, error) {
	result, err := m.reuseExistingWorkspaceWorktreeDetails(ctx, ws, launchSpec, nil)
	return result.branch, result.reused, err
}

func (m *Manager) reuseExistingWorkspaceWorktreeDetails(
	ctx context.Context,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
	validateCloneRoute func(context.Context) error,
) (existingWorkspaceWorktreeResult, error) {
	info, err := os.Lstat(ws.WorktreePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return existingWorkspaceWorktreeResult{}, nil
		}
		return existingWorkspaceWorktreeResult{}, fmt.Errorf("stat existing worktree: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return existingWorkspaceWorktreeResult{}, nil
	}
	commonDir, err := worktreeCommonGitDir(ctx, ws.WorktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return existingWorkspaceWorktreeResult{}, nil
		}
		return existingWorkspaceWorktreeResult{}, fmt.Errorf("inspect existing worktree: %w", err)
	}
	owned, err := gitDirOwnsLinkedWorktree(ctx, commonDir, ws.WorktreePath)
	if err != nil {
		return existingWorkspaceWorktreeResult{}, err
	}
	if !owned {
		return existingWorkspaceWorktreeResult{}, nil
	}
	prov, err := m.existingWorkspaceWorktreeProvenance(
		ctx, commonDir, ws,
	)
	if err != nil {
		return existingWorkspaceWorktreeResult{}, err
	}
	if !prov.reusable {
		return existingWorkspaceWorktreeResult{}, nil
	}
	if m.beforeExistingWorktreeRepoLock != nil {
		m.beforeExistingWorktreeRepoLock()
	}
	var branch string
	if err := m.withRepoLockForGitDir(ctx, commonDir, func() error {
		if err := m.revalidateExistingWorkspaceWorktree(
			ctx, commonDir, prov.localBase, ws,
		); err != nil {
			return err
		}
		var previousOriginURLs []string
		if !prov.localBase {
			var originErr error
			previousOriginURLs, originErr = gitConfigValues(
				ctx, commonDir, "remote.origin.url",
			)
			if originErr != nil {
				return fmt.Errorf("snapshot managed clone origin: %w", originErr)
			}
			if err := m.retargetManagedCloneOrigin(
				ctx, commonDir, ws, launchSpec, validateCloneRoute,
			); err != nil {
				restoreErr := restoreGitConfigValues(
					context.WithoutCancel(ctx), commonDir,
					"remote.origin.url", previousOriginURLs,
				)
				if restoreErr != nil {
					restoreErr = fmt.Errorf("restore managed clone origin: %w", restoreErr)
				}
				return errors.Join(err, restoreErr)
			}
		}
		useMergeRequestHeadRef, refreshErr := m.refreshExistingWorkspaceWorktree(
			ctx, commonDir, prov.remote, ws, launchSpec, validateCloneRoute,
		)
		if refreshErr != nil {
			if prov.localBase {
				return refreshErr
			}
			restoreErr := restoreGitConfigValues(
				context.WithoutCancel(ctx), commonDir,
				"remote.origin.url", previousOriginURLs,
			)
			if restoreErr != nil {
				restoreErr = fmt.Errorf("restore managed clone origin: %w", restoreErr)
			}
			return errors.Join(refreshErr, restoreErr)
		}
		currentBranch, branchErr := worktreeCurrentBranch(ctx, ws.WorktreePath)
		if branchErr != nil {
			return branchErr
		}
		var ok bool
		branch, ok, branchErr = existingWorkspacePersistedBranch(
			ctx, commonDir, prov.remote, ws, currentBranch, prov.localBase,
			useMergeRequestHeadRef,
		)
		if branchErr != nil {
			return branchErr
		}
		if !ok {
			return fmt.Errorf(
				"existing worktree branch %q does not match workspace-owned branch for %s #%d",
				currentBranch, ws.ItemType, ws.ItemNumber,
			)
		}
		if err := writeWorkspaceOwnershipMarker(ctx, commonDir, ws); err != nil {
			return fmt.Errorf("record workspace ownership: %w", err)
		}
		return nil
	}); err != nil {
		return existingWorkspaceWorktreeResult{}, err
	}
	return existingWorkspaceWorktreeResult{
		branch: branch, commonDir: commonDir, managedClone: !prov.localBase, reused: true,
	}, nil
}

func (m *Manager) revalidateExistingWorkspaceWorktree(
	ctx context.Context,
	expectedCommonDir string,
	expectedLocalBase bool,
	ws *Workspace,
) error {
	info, err := os.Lstat(ws.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryMissing,
		}
	}
	if err != nil {
		return fmt.Errorf("stat existing worktree under repository lock: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryNotLinkedWorktree,
		}
	}
	currentCommonDir, err := worktreeCommonGitDir(ctx, ws.WorktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return &WorkspaceDirectoryRecoveryError{
				Reason: WorkspaceDirectoryNotLinkedWorktree,
			}
		}
		return fmt.Errorf("inspect existing worktree under repository lock: %w", err)
	}
	current, err := canonicalFilesystemPath(currentCommonDir)
	if err != nil {
		return fmt.Errorf("resolve existing worktree repository: %w", err)
	}
	expected, err := canonicalFilesystemPath(expectedCommonDir)
	if err != nil {
		return fmt.Errorf("resolve expected worktree repository: %w", err)
	}
	if current != expected {
		return &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryRepositoryMismatch,
		}
	}
	owned, err := gitDirOwnsLinkedWorktree(ctx, currentCommonDir, ws.WorktreePath)
	if err != nil {
		return fmt.Errorf("revalidate existing linked worktree: %w", err)
	}
	if !owned {
		return &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryNotLinkedWorktree,
		}
	}
	prov, err := m.existingWorkspaceWorktreeProvenance(
		ctx, currentCommonDir, ws,
	)
	if err != nil {
		return fmt.Errorf("revalidate existing worktree provenance: %w", err)
	}
	if !prov.reusable || prov.localBase != expectedLocalBase {
		return &WorkspaceDirectoryRecoveryError{
			Reason: WorkspaceDirectoryRepositoryMismatch,
		}
	}
	return nil
}

func (m *Manager) ensureWorkspacePathAvailable(
	ctx context.Context, ws *Workspace,
) error {
	_, err := os.Lstat(ws.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat workspace destination: %w", err)
	}
	if _, err := m.inspectExistingWorkspaceDirectory(ctx, ws); err != nil {
		return err
	}
	return &WorkspaceDirectoryRecoveryError{
		Reason: WorkspaceDirectoryNotLinkedWorktree,
	}
}

// existingWorkspaceWorktreeProvenance decides whether the path already
// recorded for this workspace may be refreshed and reused. The ownership
// matrix is intentionally narrow:
//   - expected managed clone: reusable for synthetic PR branches, persisted
//     workspace branches, issue branches, and detached/unknown heads that later
//     pass branch validation;
//   - current configured local base: reusable only for same-repo workspaces
//     after validating the actual worktree config, hooks, refspecs, and origin;
//   - fork PR/MR: reusable only from the managed clone, never from a local base;
//   - matching origin from any other git dir, stale local-base config, or a
//     user-created checkout at the deterministic path: not reusable.
//
// If reuse fails after this point, Setup records an error and leaves the
// worktree in place. Cleanup later deletes only branches that the persisted
// workspace branch proves kenn-forge owns; an empty or unknown branch marker is
// deliberately treated as user-owned.
type workspaceWorktreeProvenance struct {
	remote    string
	localBase bool
	reusable  bool
}

func (m *Manager) existingWorkspaceWorktreeProvenance(
	ctx context.Context,
	commonDir string,
	ws *Workspace,
) (workspaceWorktreeProvenance, error) {
	usesManagedClone, err := m.existingWorktreeUsesManagedClone(ctx, commonDir, ws)
	if err != nil {
		return workspaceWorktreeProvenance{}, err
	}
	if usesManagedClone {
		return workspaceWorktreeProvenance{remote: originRemoteName, reusable: true}, nil
	}
	if ws.MRHeadRepo != nil {
		return workspaceWorktreeProvenance{}, nil
	}
	usesLocalBase, err := m.workspaceWorktreeUsesLocalBase(ctx, commonDir, ws)
	if err != nil || !usesLocalBase {
		return workspaceWorktreeProvenance{}, err
	}
	base, err := ValidateWorktreeBasePath(
		ctx, ws.WorktreePath, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		m.allowsInsecureHTTP(ws.Platform, ws.PlatformHost),
	)
	if err != nil {
		return workspaceWorktreeProvenance{}, err
	}
	return workspaceWorktreeProvenance{remote: base.Remote, localBase: true, reusable: true}, nil
}

func (m *Manager) existingWorktreeUsesManagedClone(
	ctx context.Context,
	commonDir string,
	ws *Workspace,
) (bool, error) {
	if m.clones == nil {
		return false, nil
	}
	candidates, err := m.workspaceManagedCloneCandidates(ctx, ws)
	if err != nil {
		return false, err
	}
	actualDir, err := canonicalFilesystemPath(commonDir)
	if err != nil {
		return false, nil
	}
	pathMatches := false
	for _, candidate := range candidates {
		ready, err := gitCloneDirReady(candidate.path)
		if err != nil || !ready {
			continue
		}
		expectedDir, err := canonicalFilesystemPath(candidate.path)
		if err == nil && actualDir == expectedDir {
			pathMatches = true
			break
		}
	}
	if !pathMatches {
		return false, nil
	}
	for _, candidate := range candidates {
		if validateBaseRemoteURLs(
			ctx, commonDir, originRemoteName, candidate.platformHost,
			candidate.owner, candidate.name,
			m.allowsInsecureHTTP(candidate.platform, candidate.platformHost),
		) == nil {
			return true, nil
		}
	}
	return false, nil
}

type managedCloneCandidate struct {
	path         string
	platform     string
	platformHost string
	owner        string
	name         string
}

func (m *Manager) workspaceManagedCloneCandidates(
	ctx context.Context, ws *Workspace,
) ([]managedCloneCandidate, error) {
	repo, err := m.workspaceRepositoryRef(ctx, ws)
	if err != nil {
		return nil, err
	}
	cloneCtx := gitclone.WithRepositoryIdentity(ctx, repo.ProviderID)
	candidates := make([]managedCloneCandidate, 0, 4)
	seen := make(map[string]struct{})
	appendCandidate := func(
		candidateCtx context.Context,
		platform, host, owner, name string,
	) error {
		path, err := m.clones.ClonePathForContext(
			candidateCtx, platform, host, owner, name,
		)
		if err != nil {
			return err
		}
		if _, ok := seen[path]; ok {
			return nil
		}
		seen[path] = struct{}{}
		candidates = append(candidates, managedCloneCandidate{
			path: path, platform: platform, platformHost: host,
			owner: owner, name: name,
		})
		return nil
	}
	if err := appendCandidate(
		cloneCtx,
		repo.Platform, repo.PlatformHost, repo.Owner, repo.Name,
	); err != nil {
		return nil, err
	}
	if repo.ProviderID != "" && m.db != nil {
		entry, err := m.db.GetRepositoryByProviderID(
			ctx, repo.Platform, repo.PlatformHost, repo.ProviderID,
		)
		if err != nil {
			return nil, fmt.Errorf("load managed clone repository routes: %w", err)
		}
		if entry == nil || entry.Repository.ID != repo.ID {
			return nil, fmt.Errorf(
				"%w: managed clone repository identity changed",
				ErrWorkspaceNotFound,
			)
		}
		for _, route := range entry.Routes {
			if err := appendCandidate(
				cloneCtx,
				route.Platform, route.PlatformHost, route.Owner, route.Name,
			); err != nil {
				return nil, err
			}
			collision, err := m.db.RepositoryRouteHasOtherRepository(
				ctx, db.RepoIdentity{
					Platform: route.Platform, PlatformHost: route.PlatformHost,
					Owner: route.Owner, Name: route.Name,
				}, repo.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("verify legacy managed clone route: %w", err)
			}
			if !collision {
				if err := appendCandidate(
					ctx, route.Platform, route.PlatformHost,
					route.Owner, route.Name,
				); err != nil {
					return nil, err
				}
			}
		}
	}
	return candidates, nil
}

// workspaceManagedClonePaths returns every route-keyed path within the stable
// provider-identity namespace plus pre-identity paths for current or historical
// routes that have never belonged to another repository. Existing linked
// worktrees cannot be moved between bare repositories, so safe historical
// locations remain valid until those workspaces are recreated.
func (m *Manager) workspaceManagedClonePaths(
	ctx context.Context, ws *Workspace,
) ([]string, error) {
	candidates, err := m.workspaceManagedCloneCandidates(ctx, ws)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(candidates)+1)
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	return paths, nil
}

func (m *Manager) retargetManagedCloneOrigin(
	ctx context.Context,
	commonDir string,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
	validateCloneRoute func(context.Context) error,
) error {
	if validateCloneRoute != nil {
		if err := validateCloneRoute(ctx); err != nil {
			return err
		}
	}
	remoteURL := ""
	if launchSpec != nil {
		remoteURL = launchSpec.Repository.CloneURL
	} else {
		var err error
		remoteURL, err = m.workspaceSetupRemoteURL(
			ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		)
		if err != nil {
			return err
		}
	}
	if err := validateBaseRemoteURL(
		remoteURL, originRemoteName, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		m.allowsInsecureHTTP(ws.Platform, ws.PlatformHost),
	); err != nil {
		return fmt.Errorf("validate managed clone origin after rename: %w", err)
	}
	if err := runGitWithoutHooks(
		ctx, commonDir, "remote", "set-url", "origin", remoteURL,
	); err != nil {
		return fmt.Errorf("update managed clone origin after rename: %w", err)
	}
	return nil
}

func (m *Manager) refreshExistingWorkspaceWorktree(
	ctx context.Context,
	commonDir string,
	remote string,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
	validateCloneRoute func(context.Context) error,
) (bool, error) {
	if err := runRouteValidatedFetch(
		ctx, commonDir, validateCloneRoute,
		func() error {
			return m.fetchWorkspaceBase(
				ctx, commonDir, ws.Platform, ws.PlatformHost,
				ws.RepoOwner, ws.RepoName, remote, false,
			)
		},
	); err != nil {
		return false, err
	}
	if err := m.syncWorkspaceBaseBranch(ctx, commonDir, remote, ws); err != nil {
		return false, err
	}
	if ws.ItemType != db.WorkspaceItemTypePullRequest {
		return false, nil
	}
	fetchErr := runRouteValidatedFetch(
		ctx, commonDir, validateCloneRoute,
		func() error {
			return m.fetchWorkspaceMergeRequestHeadRef(
				ctx, commonDir, remote, ws, launchSpec,
			)
		},
	)
	if fetchErr != nil {
		if ws.MRHeadRepo != nil {
			return false, fetchErr
		}
		return false, nil
	}
	return true, nil
}

func (m *Manager) workspaceWorktreeUsesLocalBase(
	ctx context.Context,
	commonDir string,
	ws *Workspace,
) (bool, error) {
	repo, err := m.workspaceRepositoryRef(ctx, ws)
	if err != nil {
		return false, err
	}
	base, ok, err := m.localWorktreeBaseDir(ctx, repo)
	if err != nil || !ok {
		return false, err
	}
	baseCommonDir, err := worktreeCommonGitDir(ctx, base.Path)
	if err != nil {
		return false, fmt.Errorf("inspect local worktree base: %w", err)
	}
	actualDir, err := canonicalFilesystemPath(commonDir)
	if err != nil {
		return false, fmt.Errorf("resolve existing worktree git dir: %w", err)
	}
	expectedDir, err := canonicalFilesystemPath(baseCommonDir)
	if err != nil {
		return false, fmt.Errorf("resolve local worktree base git dir: %w", err)
	}
	return actualDir == expectedDir, nil
}

func existingWorkspacePersistedBranch(
	ctx context.Context,
	gitDir string,
	remote string,
	ws *Workspace,
	currentBranch string,
	localBase bool,
	useMergeRequestHeadRef bool,
) (string, bool, error) {
	if workspaceRequiresExistingDirectory(ws) {
		return currentBranch, currentBranch == ws.GitHeadRef, nil
	}
	if ws.ItemType == db.WorkspaceItemTypePullRequest &&
		isSyntheticPRWorktreeBranch(ws.ItemNumber, currentBranch) {
		ok, err := existingWorkspaceHeadMatchesCurrentHead(
			ctx, gitDir, remote, ws, currentBranch, useMergeRequestHeadRef,
		)
		return currentBranch, ok, err
	}
	if ws.WorkspaceBranch != "" && ws.WorkspaceBranch != workspaceBranchUnknown {
		return ws.WorkspaceBranch, currentBranch == ws.WorkspaceBranch, nil
	}
	if currentBranch != "" && currentBranch == ws.GitHeadRef {
		if ws.ItemType == db.WorkspaceItemTypePullRequest {
			ok, err := existingWorkspaceHeadMatchesCurrentHead(
				ctx, gitDir, remote, ws, currentBranch, useMergeRequestHeadRef,
			)
			if err != nil || !ok {
				return "", false, err
			}
			if localBase {
				return "", true, nil
			}
			return currentBranch, true, nil
		}
		headSHA, ok, err := gitRefSHA(ctx, ws.WorktreePath, "HEAD")
		if err != nil || !ok {
			return "", false, err
		}
		startSHA, ok, err := gitRefSHA(ctx, ws.WorktreePath, workspaceStartRef(ws, remote))
		if err != nil || !ok {
			return "", false, err
		}
		if headSHA != startSHA {
			return "", false, nil
		}
		return currentBranch, true, nil
	}
	return "", false, nil
}

func existingWorkspaceHeadMatchesCurrentHead(
	ctx context.Context,
	gitDir, remote string,
	ws *Workspace,
	currentBranch string,
	useMergeRequestHeadRef bool,
) (bool, error) {
	headSHA, ok, err := gitRefSHA(ctx, ws.WorktreePath, "HEAD")
	if err != nil || !ok {
		return false, err
	}
	expectedRef, err := workspaceFallbackStartRef(
		ctx, gitDir, remote, ws, useMergeRequestHeadRef,
	)
	if err != nil {
		return false, err
	}
	expectedSHA, ok, err := gitRefSHA(ctx, gitDir, expectedRef)
	if err != nil || !ok {
		return false, err
	}
	if headSHA != expectedSHA {
		return false, fmt.Errorf(
			"existing worktree branch %q points at %s, not current workspace head %s",
			currentBranch, headSHA, expectedSHA,
		)
	}
	return true, nil
}

func worktreeCurrentBranch(ctx context.Context, path string) (string, error) {
	out, err := gitCombinedOutput(ctx, path, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("inspect existing worktree branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (m *Manager) workspaceSetupGitDir(
	ctx context.Context,
	ws *Workspace,
	worktreeBasePath string,
	launchSpec *WorkspaceLaunchSpec,
	validateCloneRoute func(context.Context) error,
) (workspaceGitDir, error) {
	repo, err := m.workspaceRepositoryRef(ctx, ws)
	if err != nil {
		return workspaceGitDir{}, err
	}
	if launchSpec != nil {
		repo.ProviderID = launchSpec.Repository.PlatformRepoID
	}
	if ws.MRHeadRepo == nil {
		if strings.TrimSpace(worktreeBasePath) != "" {
			base, err := ValidateWorktreeBasePath(
				ctx, worktreeBasePath, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
				m.allowsInsecureHTTP(ws.Platform, ws.PlatformHost),
			)
			return workspaceGitDir{path: base.Path, remote: base.Remote, localBase: err == nil}, err
		}
		if base, ok, err := m.localWorktreeBaseDir(ctx, repo); err != nil || ok {
			return workspaceGitDir{path: base.Path, remote: base.Remote, localBase: err == nil}, err
		}
	}

	if m.clones == nil {
		return workspaceGitDir{}, fmt.Errorf("clone manager not set")
	}

	remoteURL := ""
	if launchSpec != nil {
		remoteURL = launchSpec.Repository.CloneURL
	} else {
		var err error
		remoteURL, err = m.workspaceSetupRemoteURL(
			ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		)
		if err != nil {
			return workspaceGitDir{}, err
		}
	}
	cloneCtx := gitclone.WithRepositoryIdentity(ctx, repo.ProviderID)
	if err := m.clones.EnsureCloneValidated(
		cloneCtx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName, remoteURL,
		validateCloneRoute,
	); err != nil {
		return workspaceGitDir{}, err
	}

	cloneDir, err := m.clones.ClonePathForContext(
		cloneCtx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
	)
	if err != nil {
		return workspaceGitDir{}, err
	}
	return workspaceGitDir{path: cloneDir, remote: originRemoteName}, nil
}

func (m *Manager) workspaceCloneRouteValidator(
	identity db.RepoIdentity,
	repoID int64,
	fence db.RepositoryRouteFence,
) func(context.Context) error {
	return func(ctx context.Context) error {
		current, found, err := m.db.CurrentRepositoryRouteFence(ctx, identity, repoID)
		if err != nil {
			return err
		}
		if !found || current != fence {
			return fmt.Errorf(
				"%w: workspace repository route changed during clone",
				db.ErrRepositoryRouteFenceChanged,
			)
		}
		return nil
	}
}

func (m *Manager) workspaceSetupRemoteURL(
	ctx context.Context, platform, platformHost, owner, name string,
) (string, error) {
	repo, err := m.db.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     platform,
		PlatformHost: platformHost,
		Owner:        owner,
		Name:         name,
	})
	if err != nil {
		return "", fmt.Errorf("look up repo clone URL: %w", err)
	}
	return workspaceCloneRemoteURL(repo, platformHost, owner, name), nil
}

func (m *Manager) localWorktreeBaseDir(
	ctx context.Context, repo workspaceRepoRef,
) (WorktreeBase, bool, error) {
	if m.worktreeBaseResolver == nil {
		return WorktreeBase{}, false, nil
	}
	raw, ok, err := m.worktreeBaseResolver(ctx, WorktreeBaseRepository{
		Platform:       repo.Platform,
		PlatformHost:   repo.PlatformHost,
		PlatformRepoID: repo.ProviderID,
		Owner:          repo.Owner,
		Name:           repo.Name,
	})
	if err != nil {
		return WorktreeBase{}, false, err
	}
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		return WorktreeBase{}, false, nil
	}
	base, err := ValidateWorktreeBasePath(
		ctx, raw, repo.PlatformHost, repo.Owner, repo.Name,
		m.allowsInsecureHTTP(repo.Platform, repo.PlatformHost),
	)
	if err != nil {
		return WorktreeBase{}, false, err
	}
	return base, true, nil
}

func (m *Manager) workspaceRepositoryRef(
	ctx context.Context, ws *Workspace,
) (workspaceRepoRef, error) {
	repoRef := workspaceRepoRef{
		ID:           ws.RepoID,
		Platform:     ws.Platform,
		PlatformHost: ws.PlatformHost,
		Owner:        ws.RepoOwner,
		Name:         ws.RepoName,
	}
	if ws.RepoID == 0 {
		return repoRef, nil
	}
	repo, err := m.db.GetRepoByID(ctx, ws.RepoID)
	if err != nil {
		return workspaceRepoRef{}, fmt.Errorf("look up workspace repository: %w", err)
	}
	if repo == nil {
		return workspaceRepoRef{}, fmt.Errorf(
			"%w: workspace repository not found", ErrWorkspaceNotFound,
		)
	}
	repoRef.ProviderID = repo.PlatformRepoID
	return repoRef, nil
}

func (m *Manager) localWorktreeBaseLockRoot(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(
		m.worktreeDir, ".kenn-forge-worktree-base-locks",
		hex.EncodeToString(sum[:]),
	)
}

// WorktreeBase is the validated local checkout and the remote resolved from its
// repository identity. Remote is resolved rather than assumed to be origin.
type WorktreeBase struct {
	// Path is the canonical filesystem path of the validated checkout.
	Path string
	// Remote is resolved from the checkout rather than assumed to be origin.
	Remote string
}

// ValidateWorktreeBasePath verifies that path is an existing local Git
// worktree whose resolved remote matches the tracked repository identity.
func ValidateWorktreeBasePath(
	ctx context.Context, path, platformHost, owner, name string,
	allowInsecureHTTP bool,
) (WorktreeBase, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return WorktreeBase{}, fmt.Errorf("resolve path: %w", err)
	}
	evaluated, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorktreeBase{}, fmt.Errorf("path does not exist: %s", abs)
		}
		return WorktreeBase{}, fmt.Errorf("resolve symbolic links: %w", err)
	}
	abs = evaluated
	stat, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorktreeBase{}, fmt.Errorf("path does not exist: %s", abs)
		}
		return WorktreeBase{}, fmt.Errorf("stat path: %w", err)
	}
	if !stat.IsDir() {
		return WorktreeBase{}, fmt.Errorf("path is not a directory: %s", abs)
	}
	insideWorkTree, err := gitOutput(ctx, abs, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return WorktreeBase{}, fmt.Errorf("path is not a git worktree: %w", err)
	}
	if strings.TrimSpace(insideWorkTree) != "true" {
		return WorktreeBase{}, fmt.Errorf("path is not a git worktree: %s", abs)
	}
	if err := validateNoExecutableLocalGitConfig(ctx, abs); err != nil {
		return WorktreeBase{}, err
	}
	remote, err := resolveWorktreeBaseRemote(
		ctx, abs, platformHost, owner, name, allowInsecureHTTP,
	)
	if err != nil {
		return WorktreeBase{}, err
	}
	if err := validateBaseRemoteURLs(
		ctx, abs, remote, platformHost, owner, name, allowInsecureHTTP,
	); err != nil {
		return WorktreeBase{}, err
	}
	if err := validateBaseFetchRefspec(ctx, abs, remote); err != nil {
		return WorktreeBase{}, err
	}
	return WorktreeBase{Path: abs, Remote: remote}, nil
}

func (m *Manager) allowsInsecureHTTP(platform, platformHost string) bool {
	return m.clones != nil && m.clones.AllowsInsecureHTTP(platform, platformHost)
}

func (m *Manager) workspaceRepo(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (*db.Repo, error) {
	identity, err := workspaceRepoIdentity(provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	return m.db.GetRepoByIdentity(ctx, identity)
}

// workspaceRepoUnderReconciliationRead is workspaceRepo for callers already
// holding the repository reconciliation read lock.
func (m *Manager) workspaceRepoUnderReconciliationRead(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (*db.Repo, error) {
	identity, err := workspaceRepoIdentity(provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	return m.db.GetRepoByIdentityUnderRepositoryReconciliationRead(ctx, identity)
}

func workspaceRepoIdentity(
	provider, platformHost, owner, name string,
) (db.RepoIdentity, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return db.RepoIdentity{}, fmt.Errorf("provider is required")
	}
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return db.RepoIdentity{}, err
	}
	return db.RepoIdentity{
		Platform:     string(kind),
		PlatformHost: platformHost,
		RepoPath:     owner + "/" + name,
	}, nil
}

func validateNoExecutableLocalGitConfig(ctx context.Context, dir string) error {
	keys, err := localGitConfigKeys(ctx, dir)
	if err != nil {
		return fmt.Errorf("inspect executable local git config: %w", err)
	}
	for _, key := range keys {
		if localGitConfigKeyMayExecute(key) {
			return fmt.Errorf(
				"local git config %q may execute or rewrite git commands",
				key,
			)
		}
	}
	return nil
}

func localGitConfigKeyMayExecute(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "core.fsmonitor" ||
		key == "core.alternaterefscommand" ||
		key == "core.askpass" ||
		key == "core.gitproxy" ||
		key == "core.sshcommand" ||
		key == "credential.helper" ||
		key == "diff.external" ||
		key == "fetch.recursesubmodules" ||
		strings.HasPrefix(key, "http.") ||
		key == "submodule.recurse" ||
		(strings.HasPrefix(key, "credential.") &&
			strings.HasSuffix(key, ".helper")) ||
		(strings.HasPrefix(key, "diff.") &&
			(strings.HasSuffix(key, ".command") ||
				strings.HasSuffix(key, ".textconv"))) ||
		(strings.HasPrefix(key, "filter.") &&
			(strings.HasSuffix(key, ".process") ||
				strings.HasSuffix(key, ".clean") ||
				strings.HasSuffix(key, ".smudge"))) ||
		(strings.HasPrefix(key, "remote.") &&
			strings.HasSuffix(key, ".proxy")) ||
		(strings.HasPrefix(key, "url.") &&
			strings.HasSuffix(key, ".insteadof")) ||
		key == "include.path" ||
		(strings.HasPrefix(key, "includeif.") &&
			strings.HasSuffix(key, ".path")) ||
		(strings.HasPrefix(key, "protocol.") &&
			strings.HasSuffix(key, ".allow"))
}

// Managed-clone call sites pass the origin remote explicitly.
const originRemoteName = "origin"

func resolveWorktreeBaseRemote(
	ctx context.Context, dir, platformHost, owner, name string,
	allowInsecureHTTP bool,
) (string, error) {
	names, err := gitRemoteNames(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("list git remotes: %w", err)
	}
	var matches []string
	var unsafeMatches []string
	for _, remote := range names {
		remoteURLs, err := gitConfigValues(ctx, dir, "remote."+remote+".url")
		if err != nil {
			return "", fmt.Errorf("read %q remote: %w", remote, err)
		}
		identityMatch := false
		for _, remoteURL := range remoteURLs {
			if remoteMatchesRepositoryIdentity(remoteURL, platformHost, owner, name) {
				identityMatch = true
				break
			}
		}
		if !identityMatch {
			continue
		}
		if !validGitRemoteName(remote) {
			unsafeMatches = append(unsafeMatches, remote)
			continue
		}
		matches = append(matches, remote)
	}
	if len(unsafeMatches) > 0 {
		return "", fmt.Errorf("git remote name %q is unsafe", unsafeMatches[0])
	}
	if len(matches) == 0 {
		return "", fmt.Errorf(
			"no git remote matches configured repository %q on host %q; inspected remotes: %s",
			owner+"/"+name, platformHost, strings.Join(names, ", "),
		)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf(
			"git remotes %q and %q both match configured repository %q on host %q",
			matches[0], matches[1], owner+"/"+name, platformHost,
		)
	}
	return matches[0], nil
}

func remoteMatchesRepositoryIdentity(
	remoteURL, platformHost, owner, name string,
) bool {
	return gitremote.RemoteHost(remoteURL) != "" &&
		gitremote.RemoteRepoPath(remoteURL) != "" &&
		gitremote.ValidateRemoteIdentity(gitremote.Identity{
			Host: platformHost, Owner: owner, Name: name,
		}, remoteURL) == nil
}

func validGitRemoteName(remote string) bool {
	return remote != "" && remote[0] != '-' &&
		!strings.ContainsAny(remote, " \t\r\n")
}

func validateBaseRemoteURLs(
	ctx context.Context, dir, remote, platformHost, owner, name string,
	allowInsecureHTTP bool,
) error {
	remoteURLs, err := gitConfigValues(ctx, dir, "remote."+remote+".url")
	if err != nil {
		return fmt.Errorf("read %q remote: %w", remote, err)
	}
	if len(remoteURLs) == 0 {
		return fmt.Errorf("read %q remote: no URL configured", remote)
	}
	for _, remoteURL := range remoteURLs {
		if err := validateBaseRemoteURL(
			remoteURL, remote, platformHost, owner, name, allowInsecureHTTP,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateBaseRemoteURL(
	remoteURL, remote, platformHost, owner, name string,
	allowInsecureHTTP bool,
) error {
	if gitremote.RemoteHost(remoteURL) == "" ||
		gitremote.RemoteRepoPath(remoteURL) == "" {
		return fmt.Errorf(
			"%q remote must include a forge host and repository path", remote,
		)
	}
	if !originRemoteSchemeAllowed(remoteURL, allowInsecureHTTP) {
		// Never include the raw remote URL: it can embed credentials
		// (http://oauth2:token@host/...) and this error is persisted as
		// workspace error state and returned through the API.
		return fmt.Errorf(
			"%q remote scheme %q is not allowed (host %q)",
			remote,
			remoteURLScheme(remoteURL), gitremote.RemoteHost(remoteURL),
		)
	}
	if err := gitremote.ValidateRemoteIdentity(gitremote.Identity{
		Host:  platformHost,
		Owner: owner,
		Name:  name,
	}, remoteURL); err != nil {
		return fmt.Errorf("%q remote does not match repository: %w", remote, err)
	}
	return nil
}

// remoteURLScheme returns only the scheme prefix of a remote URL. The rest
// of the URL stays out of error messages because it can embed credentials.
func remoteURLScheme(remoteURL string) string {
	scheme, _, ok := strings.Cut(remoteURL, "://")
	if !ok {
		return ""
	}
	return strings.ToLower(scheme)
}

func originRemoteSchemeAllowed(remoteURL string, allowInsecureHTTP bool) bool {
	if !strings.Contains(remoteURL, "://") {
		return true
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "", "https", "ssh":
		return true
	case "http":
		return allowInsecureHTTP || hostIsLoopback(parsed.Host)
	default:
		return false
	}
}

func hostIsLoopback(hostport string) bool {
	host := hostport
	if parsedHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func remoteTrackingRef(remote, suffix string) string {
	if strings.TrimSpace(remote) == "" {
		return ""
	}
	return "refs/remotes/" + remote + "/" + suffix
}

func remoteTrackingGlob(remote string) string {
	return remoteTrackingRef(remote, "*")
}

func remoteBaseFetchRefspec(remote string) string {
	return "+refs/heads/*:" + remoteTrackingGlob(remote)
}

func remoteShorthandRef(remote, name string) string {
	return remote + "/" + name
}

func validateBaseFetchRefspec(ctx context.Context, dir, remote string) error {
	values, err := gitConfigValues(ctx, dir, "remote."+remote+".fetch")
	if err != nil {
		return fmt.Errorf("read %q fetch refspec: %w", remote, err)
	}
	for _, value := range values {
		if !fetchRefspecUpdatesRemote(value, remote) {
			return fmt.Errorf(
				"%q fetch refspec %q may update unsafe refs", remote, value,
			)
		}
	}
	remotes, err := gitRemoteNames(ctx, dir)
	if err != nil {
		return fmt.Errorf("list git remotes: %w", err)
	}
	for _, other := range remotes {
		if other == remote {
			continue
		}
		values, err := gitConfigValues(ctx, dir, "remote."+other+".fetch")
		if err != nil {
			return fmt.Errorf("read %q fetch refspec: %w", other, err)
		}
		for _, value := range values {
			destination, ok := fetchRefspecDestination(value)
			if ok && fetchRefspecOverlapsNamespace(
				destination, remoteTrackingRef(remote, ""),
			) {
				return fmt.Errorf(
					"remote %q fetch refspec %q writes into the %q tracking namespace",
					other, value, remote,
				)
			}
		}
	}
	return nil
}

func fetchRefspecUpdatesRemote(value, remote string) bool {
	source, destination, ok := fetchRefspecParts(value)
	return ok && strings.HasPrefix(source, "refs/heads/") &&
		remote != "" && strings.HasPrefix(destination, remoteTrackingRef(remote, ""))
}

func fetchRefspecDestination(value string) (string, bool) {
	_, destination, ok := fetchRefspecParts(value)
	return destination, ok
}

func fetchRefspecParts(value string) (string, string, bool) {
	refspec := strings.TrimSpace(value)
	if refspec == "" || strings.HasPrefix(refspec, "^") {
		return "", "", false
	}
	refspec = strings.TrimPrefix(refspec, "+")
	source, destination, ok := strings.Cut(refspec, ":")
	if !ok || destination == "" {
		return "", "", false
	}
	return source, destination, true
}

func fetchRefspecOverlapsNamespace(destination, namespace string) bool {
	if destination == "" || namespace == "" {
		return false
	}
	namespaceRoot := strings.TrimSuffix(namespace, "/")
	if prefix, _, wildcard := strings.Cut(destination, "*"); wildcard {
		prefix = strings.TrimSuffix(prefix, "/")
		return strings.HasPrefix(namespaceRoot, prefix) ||
			refPathPrefix(namespaceRoot, prefix)
	}
	return refPathPrefix(destination, namespaceRoot) ||
		refPathPrefix(namespaceRoot, destination)
}

func refPathPrefix(prefix, path string) bool {
	return prefix == path || strings.HasPrefix(path, prefix+"/")
}

func gitRemoteNames(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOutput(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func gitConfigValues(ctx context.Context, dir, key string) ([]string, error) {
	out, err := gitCombinedOutput(ctx, dir, "config", "--get-all", key)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var values []string
	for line := range strings.SplitSeq(out, "\n") {
		value := strings.TrimSpace(line)
		if value != "" {
			values = append(values, value)
		}
	}
	return values, nil
}

func restoreGitConfigValues(
	ctx context.Context, dir, key string, before []string,
) error {
	current, err := gitConfigValues(ctx, dir, key)
	if err != nil || slices.Equal(current, before) {
		return err
	}
	if len(current) > 0 {
		if err := runGitWithoutHooks(ctx, dir, "config", "--unset-all", key); err != nil {
			return err
		}
	}
	for _, value := range before {
		if err := runGitWithoutHooks(ctx, dir, "config", "--add", key, value); err != nil {
			return err
		}
	}
	return nil
}

func localGitConfigKeys(ctx context.Context, dir string) ([]string, error) {
	keys, err := localGitConfigKeysForScope(ctx, dir, "--local")
	if err != nil {
		return nil, err
	}
	worktreeKeys, err := localGitConfigKeysForScope(ctx, dir, "--worktree")
	if err != nil {
		return nil, err
	}
	keys = append(keys, worktreeKeys...)
	return keys, nil
}

func localGitConfigKeysForScope(
	ctx context.Context, dir, scope string,
) ([]string, error) {
	out, err := gitCombinedOutput(
		ctx, dir, "config", scope, "--name-only", "--list",
	)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		if scope == "--worktree" &&
			(strings.Contains(out, "extensions.worktreeConfig") ||
				strings.Contains(out, "extension worktreeConfig") ||
				strings.Contains(out, "config.worktree")) {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	for line := range strings.SplitSeq(out, "\n") {
		key := strings.TrimSpace(line)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// addWorktree creates the workspace's worktree and branch under the
// per-repo lock. The lock prevents concurrent worktree mutations on
// the same git repository from clobbering each other; see FileLockManager.
type workspaceGitFetchOptions struct {
	launchSpec    *WorkspaceLaunchSpec
	validateRoute func(context.Context) error
}

type workspaceGitDir struct {
	path      string
	remote    string
	localBase bool
}

func (m *Manager) addWorktree(
	ctx context.Context,
	gitDir workspaceGitDir,
	ws *Workspace,
	fetchOptions workspaceGitFetchOptions,
) (string, error) {
	var branch string
	err := m.withRepoLockForGitDir(ctx, gitDir.path, func() error {
		if gitDir.localBase {
			if err := runRouteValidatedFetch(
				ctx, gitDir.path, fetchOptions.validateRoute,
				func() error {
					return m.fetchWorkspaceBase(
						ctx, gitDir.path, ws.Platform, ws.PlatformHost,
						ws.RepoOwner, ws.RepoName,
						gitDir.remote, workspaceUsesOriginHead(ws),
					)
				},
			); err != nil {
				return err
			}
		}
		if err := m.syncWorkspaceBaseBranch(ctx, gitDir.path, gitDir.remote, ws); err != nil {
			return err
		}
		if err := m.ensureWorkspacePathAvailable(ctx, ws); err != nil {
			return err
		}
		var addErr error
		branch, addErr = m.addWorktreeLocked(
			ctx, gitDir, ws, fetchOptions,
		)
		if addErr != nil {
			return addErr
		}
		return nil
	})
	return branch, err
}

// addWorktreeLocked runs the worktree-add decision tree. Callers must
// hold the per-repo lock for cloneDir before invoking this function.
func (m *Manager) addWorktreeLocked(
	ctx context.Context,
	gitDir workspaceGitDir,
	ws *Workspace,
	fetchOptions workspaceGitFetchOptions,
) (string, error) {
	if workspaceUsesOriginHead(ws) {
		return m.addIssueWorktree(ctx, gitDir, ws)
	}
	mergeRequestHeadRefFetched := false
	if ws.MRHeadRepo != nil {
		if err := runRouteValidatedFetch(
			ctx, gitDir.path, fetchOptions.validateRoute,
			func() error {
				return m.fetchWorkspaceMergeRequestHeadRef(
					ctx, gitDir.path, gitDir.remote, ws, fetchOptions.launchSpec,
				)
			},
		); err != nil {
			return "", fmt.Errorf("fetch merge request head ref: %w", err)
		}
		mergeRequestHeadRefFetched = true
	}
	branch, err := m.addPreferredWorktree(ctx, gitDir, ws)
	if err == nil {
		return branch, nil
	}
	if errors.Is(err, errWorkspaceOwnershipMarker) {
		return "", err
	}
	fallbackBranch := syntheticPRWorktreeBranch(ws.ItemNumber)
	// Providers may not retain a synthetic MR head ref. Try to populate the
	// specific ref needed for this workspace, but do not trust a local stale
	// copy when the exact refresh fails.
	var fetchHeadErr error
	useMergeRequestHeadRef := mergeRequestHeadRefFetched
	if !useMergeRequestHeadRef {
		fetchHeadErr = runRouteValidatedFetch(
			ctx, gitDir.path, fetchOptions.validateRoute,
			func() error {
				return m.fetchWorkspaceMergeRequestHeadRef(
					ctx, gitDir.path, gitDir.remote, ws, fetchOptions.launchSpec,
				)
			},
		)
		useMergeRequestHeadRef = fetchHeadErr == nil
	}
	if !useMergeRequestHeadRef && ws.MRHeadRepo != nil {
		return "", fmt.Errorf(
			"preferred branch %q failed: %w; fallback branch %q failed: fetch merge request head ref: %w",
			ws.GitHeadRef, err, fallbackBranch, fetchHeadErr,
		)
	}
	startRef, startRefErr := workspaceFallbackStartRef(
		ctx, gitDir.path, gitDir.remote, ws, useMergeRequestHeadRef,
	)
	if startRefErr != nil {
		return "", fmt.Errorf(
			"preferred branch %q failed: %w; fallback branch %q failed: %w",
			ws.GitHeadRef, err, fallbackBranch, startRefErr,
		)
	}
	branch, fallbackErr := m.addFallbackWorktree(
		ctx, gitDir, ws, fallbackBranch, startRef,
	)
	if fallbackErr == nil {
		return branch, nil
	}
	return "", fmt.Errorf(
		"preferred branch %q failed: %w; fallback branch %q failed: %w",
		ws.GitHeadRef, err, fallbackBranch, fallbackErr,
	)
}

// addFallbackWorktree checks out the workspace head under a name other than the
// PR head branch, which is where setup lands whenever the preferred name is
// unusable (stale local branch, checked out elsewhere, or simply taken).
//
// Every failure reachable from here is a naming collision inside kenn-forge's own
// branch namespace, never a missing commit, so no collision may be terminal: a
// taken synthetic name is uniquified, and if no branch can be created at all the
// worktree is checked out detached. A workspace the maintainer can open and
// rename by hand always beats a setup error with nothing to retry into.
func (m *Manager) addFallbackWorktree(
	ctx context.Context,
	gitDir workspaceGitDir,
	ws *Workspace,
	fallbackBranch, startRef string,
) (string, error) {
	branch, err := m.addFallbackBranchWorktree(
		ctx, gitDir, ws, fallbackBranch, startRef,
	)
	if err == nil {
		return branch, nil
	}
	fallbackErr := err
	if errors.Is(err, errWorkspaceOwnershipMarker) {
		return "", err
	}

	// Uniquifying leaves the colliding branch untouched: it may be a live
	// workspace's checkout or a user branch kenn-forge never created.
	if uniqueBranch, nameErr := nextAvailableBranchName(
		ctx, gitDir.path, fallbackBranch,
	); nameErr == nil {
		branch, err = m.addFallbackBranchWorktree(
			ctx, gitDir, ws, uniqueBranch, startRef,
		)
		if err == nil {
			return branch, nil
		}
		if errors.Is(err, errWorkspaceOwnershipMarker) {
			return "", err
		}
	}

	if detachErr := m.runOwnedGitWorktreeAdd(
		ctx, gitDir.path, ws, "--detach", startRef,
	); detachErr != nil {
		return "", fmt.Errorf(
			"%w; detached checkout failed: %w", fallbackErr, detachErr,
		)
	}
	// An empty managed branch: kenn-forge owns no branch here, so rollback and
	// delete leave every existing branch in place.
	slog.Warn("workspace worktree checked out detached",
		"workspace_id", ws.ID, "path", ws.WorktreePath,
		"branch", fallbackBranch, "err", fallbackErr)
	return "", nil
}

func (m *Manager) addFallbackBranchWorktree(
	ctx context.Context,
	gitDir workspaceGitDir,
	ws *Workspace,
	branch, startRef string,
) (string, error) {
	branchSHA, err := m.runOwnedGitWorktreeAddCreatingBranch(
		ctx, gitDir.path, ws, branch, startRef,
	)
	if err != nil {
		return "", err
	}
	if err := configureFallbackBranchUpstream(
		ctx, gitDir, ws, branch,
	); err != nil {
		cleanupErr := cleanupOwnedWorktreeAddOnUpstreamFailure(
			ctx, gitDir.path, ws, branch, branchSHA,
		)
		return "", errors.Join(
			fmt.Errorf("configure branch upstream: %w", err), cleanupErr,
		)
	}
	return branch, nil
}

func (m *Manager) addIssueWorktree(
	ctx context.Context, gitDir workspaceGitDir, ws *Workspace,
) (string, error) {
	workspaceBranch := ws.WorkspaceBranch
	if workspaceBranch == workspaceBranchUnknown {
		workspaceBranch = ws.GitHeadRef
	}
	if workspaceBranch == "" {
		if err := m.runOwnedGitWorktreeAdd(
			ctx, gitDir.path, ws, ws.GitHeadRef,
		); err != nil {
			return "", err
		}
		return "", nil
	}
	startRef := workspaceStartRef(ws, gitDir.remote)
	if _, err := m.runOwnedGitWorktreeAddCreatingBranch(
		ctx, gitDir.path, ws, workspaceBranch, startRef,
	); err != nil {
		return "", err
	}
	return workspaceBranch, nil
}

func (m *Manager) addPreferredWorktree(
	ctx context.Context, gitDir workspaceGitDir, ws *Workspace,
) (string, error) {
	if err := validateLocalBranchName(
		ctx, gitDir.path, ws.GitHeadRef,
	); err != nil {
		return "", err
	}

	if ws.MRHeadRepo != nil {
		_, err := m.runOwnedGitWorktreeAddCreatingBranch(
			ctx, gitDir.path, ws,
			ws.GitHeadRef, workspaceStartRef(ws, gitDir.remote),
		)
		if err != nil {
			return "", err
		}
		return ws.GitHeadRef, nil
	}

	startRef := workspaceStartRef(ws, gitDir.remote)
	startSHA, ok, err := gitRefSHA(ctx, gitDir.path, startRef)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("start ref %q not found", startRef)
	}

	branchRef := "refs/heads/" + ws.GitHeadRef
	branchSHA, exists, err := gitRefSHA(ctx, gitDir.path, branchRef)
	if err != nil {
		return "", err
	}
	if !exists {
		branchSHA, err := m.runOwnedGitWorktreeAddCreatingBranch(
			ctx, gitDir.path, ws, ws.GitHeadRef, startRef,
		)
		if err != nil {
			return "", err
		}
		if err := setBranchUpstream(
			ctx, ws.WorktreePath, ws.GitHeadRef,
			originRemoteName, "refs/heads/"+ws.GitHeadRef,
		); err != nil {
			cleanupErr := cleanupOwnedWorktreeAddOnUpstreamFailure(
				ctx, gitDir.path, ws, ws.GitHeadRef, branchSHA,
			)
			return "", errors.Join(
				fmt.Errorf("configure branch upstream: %w", err), cleanupErr,
			)
		}
		return ws.GitHeadRef, nil
	}
	if branchSHA != startSHA {
		return "", fmt.Errorf(
			"preferred branch %q points at %s, not %s",
			ws.GitHeadRef, branchSHA, startSHA,
		)
	}
	if gitDir.localBase {
		checkedOut, err := localBranchCheckedOut(ctx, gitDir.path, ws.GitHeadRef)
		if err != nil {
			return "", fmt.Errorf("inspect checked out branch: %w", err)
		}
		if checkedOut {
			return "", fmt.Errorf(
				"preferred branch %q is already checked out",
				ws.GitHeadRef,
			)
		}
	}

	if err := m.runOwnedGitWorktreeAdd(
		ctx, gitDir.path, ws, ws.GitHeadRef,
	); err != nil {
		return "", err
	}

	if !gitDir.localBase {
		if err := setBranchUpstream(
			ctx, ws.WorktreePath, ws.GitHeadRef,
			originRemoteName, "refs/heads/"+ws.GitHeadRef,
		); err != nil {
			// Empty branch: the branch pre-existed this workspace and
			// stays in place; only the worktree is rolled back.
			cleanupErr := cleanupOwnedWorktreeAddOnUpstreamFailure(
				ctx, gitDir.path, ws, "", "",
			)
			return "", errors.Join(
				fmt.Errorf("configure branch upstream: %w", err), cleanupErr,
			)
		}
	}

	// The branch already existed before this workspace was materialized. Return
	// an empty managed branch so rollback, retry, and delete cleanup remove only
	// the worktree and never delete the user's pre-existing local branch.
	return "", nil
}

func workspaceStartRef(ws *Workspace, remote string) string {
	if workspaceUsesOriginHead(ws) {
		return remoteShorthandRef(remote, "HEAD")
	}
	if ws.MRHeadRepo != nil {
		return workspaceMergeRequestHeadRef(ws)
	}
	return remoteShorthandRef(remote, ws.GitHeadRef)
}

func workspaceFallbackStartRef(
	ctx context.Context, cloneDir, remote string, ws *Workspace, useMergeRequestHeadRef bool,
) (string, error) {
	if useMergeRequestHeadRef && ws.ItemType == db.WorkspaceItemTypePullRequest {
		ref := workspaceMergeRequestHeadRef(ws)
		_, exists, err := gitRefSHA(ctx, cloneDir, ref)
		if err != nil {
			return "", fmt.Errorf("inspect merge request head ref %q: %w", ref, err)
		}
		if exists {
			return ref, nil
		}
	}
	return workspaceStartRef(ws, remote), nil
}

func workspaceMergeRequestHeadRef(ws *Workspace) string {
	return platform.MergeRequestHeadRef(platform.Kind(ws.Platform), ws.ItemNumber)
}

func syntheticPRWorktreeBranch(mrNumber int) string {
	return fmt.Sprintf("kenn-forge/pr-%d", mrNumber)
}

// isSyntheticPRWorktreeBranch reports whether branch is the synthetic PR branch
// for this merge request or one of the numbered variants addFallbackWorktree
// creates when that name is taken. Both are kenn-forge-created names for the same
// workspace head, so reuse and upstream repair must recognize either.
func isSyntheticPRWorktreeBranch(mrNumber int, branch string) bool {
	base := syntheticPRWorktreeBranch(mrNumber)
	if branch == base {
		return true
	}
	suffix, ok := strings.CutPrefix(branch, base+"-")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}

// cleanupUnmarkedWorktreeAdd rolls back a worktree whose ownership marker
// could not be published. Callers hold the repository lock. The exact live
// registration must still be present and unmarked before it can be removed.
func cleanupUnmarkedWorktreeAdd(
	ctx context.Context,
	cloneDir string,
	ws *Workspace,
	branch, branchSHA string,
) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	live, err := gitDirHasLiveWorktree(
		cleanupCtx, cloneDir, ws.WorktreePath,
	)
	if err != nil {
		return fmt.Errorf("verify failed worktree registration: %w", err)
	}
	if !live {
		return fmt.Errorf(
			"%w: %s", ErrWorkspaceOwnershipUnproven, ws.WorktreePath,
		)
	}
	metadataDir, ok, err := worktreeRegistrationMetadataDir(
		cleanupCtx, cloneDir, ws.WorktreePath,
	)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("failed worktree registration metadata not found")
	}
	_, marked, err := readWorkspaceOwnershipMarker(
		filepath.Join(metadataDir, workspaceOwnershipMarkerFile),
	)
	if err != nil {
		return err
	}
	if marked {
		return fmt.Errorf(
			"%w: %s", ErrWorkspaceOwnershipUnproven, ws.WorktreePath,
		)
	}
	if err := runGitWithoutHooks(
		cleanupCtx, cloneDir,
		"worktree", "remove", "--force", ws.WorktreePath,
	); err != nil {
		return fmt.Errorf("remove failed worktree: %w", err)
	}
	if branch == "" {
		return nil
	}
	if err := deleteWorkspaceBranchIfMatches(
		cleanupCtx, cloneDir, branch, branchSHA,
	); err != nil {
		return fmt.Errorf("remove failed worktree branch: %w", err)
	}
	return nil
}

// cleanupOwnedWorktreeAddOnUpstreamFailure rolls back a marked worktree after
// post-add configuration fails. Callers hold the repository lock. An empty
// branch leaves a pre-existing user-owned branch in place.
func cleanupOwnedWorktreeAddOnUpstreamFailure(
	ctx context.Context,
	cloneDir string,
	ws *Workspace,
	branch, branchSHA string,
) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	owned, err := workspaceRegistrationMatches(
		cleanupCtx, cloneDir, ws.WorktreePath, ws.ID,
	)
	if err != nil {
		return fmt.Errorf("verify failed worktree ownership: %w", err)
	}
	if !owned {
		return fmt.Errorf(
			"%w: %s", ErrWorkspaceOwnershipUnproven, ws.WorktreePath,
		)
	}
	if err := runGitWithoutHooks(
		cleanupCtx, cloneDir,
		"worktree", "remove", "--force", ws.WorktreePath,
	); err != nil {
		return fmt.Errorf("remove failed worktree: %w", err)
	}
	if branch == "" {
		return nil
	}
	if err := deleteWorkspaceBranchIfMatches(
		cleanupCtx, cloneDir, branch, branchSHA,
	); err != nil {
		return fmt.Errorf("remove failed worktree branch: %w", err)
	}
	return nil
}

// configureFallbackBranchUpstream points the synthetic PR fallback branch at
// the PR's head branch on origin, so divergence counts, push, and pull treat
// the remote PR branch as the sync target exactly like a preferred-name
// checkout would. Setup classifies MRHeadRepo from the current merge-request
// row before reaching this path, so nil is explicit same-repository evidence.
// The SHA check is an additional checkout-consistency check, not repository
// identity evidence: forks preserve commit IDs. Fork and unknown heads take
// the merge-request-ref path and remain without an origin upstream.
func configureFallbackBranchUpstream(
	ctx context.Context,
	gitDir workspaceGitDir,
	ws *Workspace,
	fallbackBranch string,
) error {
	if ws.ItemType != db.WorkspaceItemTypePullRequest || ws.MRHeadRepo != nil {
		return nil
	}
	trackingSHA, ok, err := gitRefSHA(
		ctx, gitDir.path, remoteTrackingRef(gitDir.remote, ws.GitHeadRef),
	)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	headSHA, err := gitHeadSHA(ctx, ws.WorktreePath)
	if err != nil {
		return err
	}
	if trackingSHA != headSHA {
		return nil
	}
	return setBranchUpstream(
		ctx, ws.WorktreePath, fallbackBranch,
		originRemoteName, "refs/heads/"+ws.GitHeadRef,
	)
}

func setBranchUpstream(
	ctx context.Context,
	worktreePath, branch, remote, mergeRef string,
) error {
	if err := runGitWithoutHooks(
		ctx, worktreePath,
		"config", "branch."+branch+".remote", remote,
	); err != nil {
		return err
	}
	return runGitWithoutHooks(
		ctx, worktreePath,
		"config", "branch."+branch+".merge", mergeRef,
	)
}

func clearBranchUpstream(ctx context.Context, worktreePath, branch string) error {
	for _, suffix := range []string{"remote", "merge"} {
		key := "branch." + branch + "." + suffix
		if _, err := gitConfigValue(ctx, worktreePath, key); err != nil {
			continue
		}
		if err := runGitWithoutHooks(
			ctx, worktreePath, "config", "--unset-all", key,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateLocalBranchName(
	ctx context.Context, dir, branch string,
) error {
	cmd := procutil.CommandContext(
		ctx, "git", "check-ref-format", "--branch", branch,
	)
	if dir == "" {
		// `git check-ref-format --branch` consults cwd when it appears to be
		// inside a worktree. Run repo-independent validation somewhere neutral
		// so a broken launch cwd cannot make a valid branch look invalid.
		dir = os.TempDir()
	}
	cmd.Dir = dir
	out, err := procutil.CombinedOutput(
		ctx, cmd, "git subprocess capacity",
	)
	if err == nil {
		return nil
	}

	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%w %q: %s", ErrInvalidBranchName, branch, msg)
}

// Delete tears down a workspace: kills tmux, removes the git
// worktree and branch, and deletes the DB record.
// If force is false and the worktree has uncommitted changes,
// it returns the dirty file list without deleting.
//
// beforeDestructive is invoked after the dirty preflight passes
// (or is skipped because force=true) and before any destructive
// cleanup. It exists so callers can stop background processes
// that might still write to the worktree — e.g. agent shells
// launched into the workspace — without that cleanup running on
// a 409 dirty rejection. Returning an error stops deletion before
// cleanup starts. Pass nil if you have nothing to do between the
// preflight and the destructive part.
func (m *Manager) Delete(
	ctx context.Context, id string, force bool,
	beforeDestructive func(context.Context) error,
) (dirty []string, err error) {
	ws, err := m.db.GetWorkspace(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	if ws == nil {
		return nil, ErrWorkspaceNotFound
	}

	if !force {
		files, checkErr := dirtyFiles(ctx, ws.WorktreePath)
		if checkErr != nil {
			// Worktree may be missing/corrupt — surface as a
			// dirty-state response so the UI can offer force-delete.
			return []string{
				fmt.Sprintf("(dirty check failed: %v)", checkErr),
			}, nil
		}
		if len(files) > 0 {
			return files, nil
		}
	}

	if beforeDestructive != nil {
		if err := beforeDestructive(ctx); err != nil {
			return nil, err
		}
	}

	if err := m.cleanupWorkspaceArtifactsForDelete(ctx, ws); err != nil {
		return nil, err
	}

	if err := m.db.DeleteWorkspace(ctx, id); err != nil {
		return nil, fmt.Errorf("delete workspace record: %w", err)
	}
	m.removeWorkspaceSummaryFromCache(id)
	return nil, nil
}

// RequestRetry prepares an errored workspace for another setup
// attempt. If setup is already running, it queues one follow-up retry
// and returns startNow=false. If the workspace is not errored or
// creating, the request is discarded and startNow=false.
func (m *Manager) RequestRetry(
	ctx context.Context, id string,
) (*Workspace, bool, error) {
	ws, err := m.db.GetWorkspace(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("get workspace: %w", err)
	}
	if ws == nil {
		return nil, false, ErrWorkspaceNotFound
	}
	started, err := m.db.StartWorkspaceRetry(ctx, ws.ID)
	if err != nil {
		return nil, false, err
	}
	if !started {
		return m.queueRetryOrStartErrored(ctx, id)
	}

	if err := m.prepareWorkspaceRetry(ctx, ws); err != nil {
		m.consumeQueuedRetry(ws.ID)
		return nil, false, err
	}
	return ws, true, nil
}

// StartQueuedRetryIfErrored consumes one queued retry for id. It
// starts the retry only if the workspace is still in error status at
// the time the queue is consumed; otherwise the queued retry is
// discarded.
func (m *Manager) StartQueuedRetryIfErrored(
	ctx context.Context, id string,
) (*Workspace, bool, error) {
	if !m.consumeQueuedRetry(id) {
		return nil, false, nil
	}

	ws, err := m.db.GetWorkspace(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("get workspace: %w", err)
	}
	if ws == nil || ws.Status != "error" {
		return ws, false, nil
	}

	started, err := m.db.StartWorkspaceRetry(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !started {
		return ws, false, nil
	}

	if err := m.prepareWorkspaceRetry(ctx, ws); err != nil {
		m.consumeQueuedRetry(ws.ID)
		return nil, false, err
	}
	return ws, true, nil
}

func (m *Manager) queueRetryOrStartErrored(
	ctx context.Context, id string,
) (*Workspace, bool, error) {
	// Serialize the status re-check with queue consumption. If setup
	// already failed and the worker drained an empty queue, the retry
	// request must start the next setup attempt itself.
	m.retryMu.Lock()
	current, getErr := m.db.GetWorkspace(ctx, id)
	if getErr != nil {
		m.retryMu.Unlock()
		return nil, false, fmt.Errorf(
			"get workspace after retry conflict: %w", getErr,
		)
	}
	if current == nil {
		m.retryMu.Unlock()
		return nil, false, ErrWorkspaceNotFound
	}
	switch current.Status {
	case "creating":
		m.retryQueued[id] = true
		m.retryMu.Unlock()
		return current, false, nil
	case "error":
		delete(m.retryQueued, id)
		m.retryMu.Unlock()
		return m.startWorkspaceRetry(ctx, current)
	default:
		m.retryMu.Unlock()
		return nil, false, fmt.Errorf(
			"%w: workspace is not in error status",
			ErrWorkspaceInvalidState,
		)
	}
}

func (m *Manager) startWorkspaceRetry(
	ctx context.Context, ws *Workspace,
) (*Workspace, bool, error) {
	started, err := m.db.StartWorkspaceRetry(ctx, ws.ID)
	if err != nil {
		return nil, false, err
	}
	if !started {
		return m.queueRetryOrStartErrored(ctx, ws.ID)
	}

	if err := m.prepareWorkspaceRetry(ctx, ws); err != nil {
		m.consumeQueuedRetry(ws.ID)
		return nil, false, err
	}
	return ws, true, nil
}

func (m *Manager) prepareWorkspaceRetry(
	ctx context.Context, ws *Workspace,
) error {
	var cleanupErr error
	if workspaceRequiresExistingDirectory(ws) {
		cleanupErr = m.cleanupTmuxSession(ctx, ws)
	} else {
		cleanupErr = m.cleanupWorkspaceArtifactsForRetry(ctx, ws)
	}
	if cleanupErr != nil {
		return m.failSetup(
			ctx,
			ws.ID, workspaceSetupStageSetup,
			fmt.Errorf(
				"cleanup workspace artifacts before retry: %w", cleanupErr,
			),
		)
	}
	retryBranch := retryWorkspaceBranch(ws)
	if err := m.updateWorkspaceBranch(ctx, ws.ID, retryBranch); err != nil {
		return m.failSetup(
			ctx,
			ws.ID, workspaceSetupStageSetup,
			fmt.Errorf("reset workspace branch before retry: %w", err),
		)
	}
	m.markRetryStarted(ctx, ws, retryBranch)
	return nil
}

func retryWorkspaceBranch(ws *Workspace) string {
	if workspaceRequiresExistingDirectory(ws) {
		return workspaceBranchRecoveryPending
	}
	if workspaceUsesOriginHead(ws) && ws.WorkspaceBranch == "" {
		return ""
	}
	return workspaceBranchUnknown
}

func (m *Manager) consumeQueuedRetry(id string) bool {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	if !m.retryQueued[id] {
		return false
	}
	delete(m.retryQueued, id)
	return true
}

func (m *Manager) markRetryStarted(
	ctx context.Context, ws *Workspace, workspaceBranch string,
) {
	ws.WorkspaceBranch = workspaceBranch
	ws.Status = "creating"
	ws.ErrorMessage = nil
	m.recordSetupEvent(
		ctx,
		ws.ID, workspaceSetupStageSetup, "retrying",
		"retrying workspace setup",
	)
}

func (m *Manager) cleanupWorkspaceArtifactsForRetry(
	ctx context.Context, ws *Workspace,
) error {
	if err := m.cleanupTmuxSession(ctx, ws); err != nil {
		return err
	}

	gitDir, ok, err := m.workspaceCleanupGitDir(ctx, ws)
	if err != nil {
		return err
	}
	if !ok {
		gitDir, ok, err = m.workspaceRegisteredCleanupGitDir(ctx, ws)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	return m.withRepoLockForGitDir(ctx, gitDir, func() error {
		return m.cleanupWorkspaceArtifactsForRetryLocked(ctx, gitDir, ws)
	})
}

func (m *Manager) cleanupWorkspaceArtifactsForRetryLocked(
	ctx context.Context, gitDir string, ws *Workspace,
) error {
	state, err := currentWorkspaceCleanupState(ctx, gitDir, ws)
	if err != nil {
		return err
	}
	switch state {
	case workspaceCleanupOwned:
		if err := runGitWithoutHooks(
			ctx, gitDir,
			"worktree", "remove", "--force", ws.WorktreePath,
		); err != nil && !isGitWorktreeAbsent(err) {
			return fmt.Errorf("remove git worktree: %w", err)
		}
	case workspaceCleanupStaleRegistration:
		if err := removeStaleWorktreeRegistrationMetadata(
			ctx, gitDir, ws.WorktreePath,
		); err != nil {
			return err
		}
	}
	if ws.WorkspaceBranch == workspaceBranchUnknown {
		if err := deleteWorkspaceBranchStrict(
			ctx, gitDir, workspaceBranchUnknown,
		); err != nil {
			return err
		}
	}
	if err := m.deleteWorkspaceBranchesStrict(
		ctx, gitDir, ws, ws.WorkspaceBranch,
	); err != nil {
		return err
	}
	if err := runGitWithoutHooks(ctx, gitDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune git worktrees: %w", err)
	}
	return nil
}

func (m *Manager) cleanupWorkspaceArtifactsForDelete(
	ctx context.Context, ws *Workspace,
) error {
	if err := m.cleanupTmuxSession(ctx, ws); err != nil {
		return err
	}
	if workspaceRequiresExistingDirectory(ws) {
		return nil
	}

	gitDir, ok, err := m.workspaceCleanupGitDir(ctx, ws)
	if err != nil {
		return err
	}
	if !ok {
		if err := quarantineOrphanedWorkspacePath(ctx, ws.WorktreePath); err != nil {
			return err
		}
		gitDir, ok, err = m.workspaceCleanupGitDir(ctx, ws)
		if err != nil {
			return err
		}
		if !ok {
			gitDir, ok, err = m.workspaceRegisteredCleanupGitDir(ctx, ws)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
	}

	return m.withRepoLockForGitDir(ctx, gitDir, func() error {
		return m.cleanupWorkspaceArtifactsForDeleteLocked(ctx, gitDir, ws)
	})
}

func (m *Manager) cleanupWorkspaceArtifactsForDeleteLocked(
	ctx context.Context, gitDir string, ws *Workspace,
) error {
	state, err := currentWorkspaceCleanupState(ctx, gitDir, ws)
	if err != nil {
		return err
	}
	switch state {
	case workspaceCleanupOwned:
		if err := runGitWithoutHooks(
			ctx, gitDir,
			"worktree", "remove", "--force", ws.WorktreePath,
		); err != nil && !isGitWorktreeAbsent(err) {
			return fmt.Errorf("remove git worktree: %w", err)
		}
	case workspaceCleanupStaleRegistration:
		if err := removeStaleWorktreeRegistrationMetadata(
			ctx, gitDir, ws.WorktreePath,
		); err != nil {
			return err
		}
	}
	m.deleteWorkspaceBranches(ctx, gitDir, ws, ws.WorkspaceBranch)
	_ = runGitWithoutHooks(ctx, gitDir, "worktree", "prune")
	return nil
}

type workspaceCleanupState uint8

const (
	workspaceCleanupNone workspaceCleanupState = iota
	workspaceCleanupOwned
	workspaceCleanupStaleRegistration
)

func currentWorkspaceCleanupState(
	ctx context.Context, gitDir string, ws *Workspace,
) (workspaceCleanupState, error) {
	owned, err := gitDirOwnsCleanupWorktree(
		ctx, gitDir, ws.WorktreePath, ws.ID,
	)
	if err != nil {
		return workspaceCleanupNone, err
	}
	if owned {
		return workspaceCleanupOwned, nil
	}
	stale, err := gitDirHasStaleWorktreeRegistration(
		ctx, gitDir, ws.WorktreePath,
	)
	if err != nil {
		return workspaceCleanupNone, err
	}
	if stale {
		return workspaceCleanupStaleRegistration, nil
	}
	return workspaceCleanupNone, nil
}

func quarantineOrphanedWorkspacePath(
	ctx context.Context, worktreePath string,
) error {
	pathInfo, err := os.Lstat(worktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat orphaned workspace path: %w", err)
	}

	inspectAsDirectory := pathInfo.IsDir()
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		targetInfo, statErr := os.Stat(worktreePath)
		if statErr == nil {
			inspectAsDirectory = targetInfo.IsDir()
		} else if errors.Is(statErr, os.ErrNotExist) {
			inspectAsDirectory = false
		} else {
			return fmt.Errorf("stat orphaned workspace symlink target: %w", statErr)
		}
	}

	if inspectAsDirectory {
		if _, err := worktreeCommonGitDir(ctx, worktreePath); err == nil {
			isRoot, err := worktreePathIsRoot(ctx, worktreePath)
			if err != nil {
				return fmt.Errorf("inspect orphaned workspace root: %w", err)
			}
			if isRoot {
				return nil
			}
		} else if !isGitWorktreeAbsent(err) {
			return fmt.Errorf("inspect orphaned workspace path: %w", err)
		}
	}

	recoveryPath, err := nextWorkspaceRecoveryPath(worktreePath, time.Now())
	if err != nil {
		return err
	}
	if err := os.Rename(worktreePath, recoveryPath); err != nil {
		return fmt.Errorf("preserve orphaned workspace path: %w", err)
	}
	slog.Warn(
		"preserved orphaned workspace path",
		"path", worktreePath,
		"recovery_path", recoveryPath,
	)
	return nil
}

func nextWorkspaceRecoveryPath(
	worktreePath string, now time.Time,
) (string, error) {
	base := worktreePath + ".orphaned-" + now.UTC().Format("20060102T150405Z")
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		_, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat workspace recovery path: %w", err)
		}
	}
}

func (m *Manager) workspaceCleanupGitDir(
	ctx context.Context, ws *Workspace,
) (string, bool, error) {
	if commonDir, err := worktreeCommonGitDir(ctx, ws.WorktreePath); err == nil {
		owned, err := workspaceRegistrationMatches(
			ctx, commonDir, ws.WorktreePath, ws.ID,
		)
		if err != nil {
			return "", false, err
		}
		if owned {
			return commonDir, true, nil
		}
	}
	repo, err := m.workspaceRepositoryRef(ctx, ws)
	if err != nil {
		return "", false, err
	}
	if base, ok, err := m.localWorktreeBaseDir(ctx, repo); err != nil || ok {
		if err != nil {
			base, ok = WorktreeBase{}, false
		}
		if !ok && m.clones == nil {
			return base.Path, ok, nil
		}
		if ok {
			owned, err := gitDirOwnsCleanupWorktree(
				ctx, base.Path, ws.WorktreePath, ws.ID,
			)
			if err != nil {
				return "", false, err
			}
			if owned {
				return base.Path, true, nil
			}
		}
	}

	if m.clones != nil {
		cloneDirs, err := m.workspaceManagedClonePaths(ctx, ws)
		if err != nil {
			return "", false, err
		}
		for _, cloneDir := range cloneDirs {
			ready, err := gitCloneDirReady(cloneDir)
			if err != nil {
				return "", false, err
			}
			if !ready {
				continue
			}
			owned, err := gitDirOwnsCleanupWorktree(
				ctx, cloneDir, ws.WorktreePath, ws.ID,
			)
			if err != nil {
				return "", false, err
			}
			if owned {
				return cloneDir, true, nil
			}
		}
	}

	return "", false, nil
}

func (m *Manager) workspaceRegisteredCleanupGitDir(
	ctx context.Context, ws *Workspace,
) (string, bool, error) {
	repo, err := m.workspaceRepositoryRef(ctx, ws)
	if err != nil {
		return "", false, err
	}
	if base, ok, err := m.localWorktreeBaseDir(ctx, repo); err == nil && ok {
		stale, err := gitDirHasStaleWorktreeRegistration(
			ctx, base.Path, ws.WorktreePath,
		)
		if err != nil {
			return "", false, err
		}
		if stale {
			return base.Path, true, nil
		}
	}

	if m.clones == nil {
		return "", false, nil
	}
	cloneDirs, err := m.workspaceManagedClonePaths(ctx, ws)
	if err != nil {
		return "", false, err
	}
	for _, cloneDir := range cloneDirs {
		ready, err := gitCloneDirReady(cloneDir)
		if err != nil {
			return "", false, err
		}
		if !ready {
			continue
		}
		stale, err := gitDirHasStaleWorktreeRegistration(
			ctx, cloneDir, ws.WorktreePath,
		)
		if err != nil {
			return "", false, err
		}
		if stale {
			return cloneDir, true, nil
		}
	}
	return "", false, nil
}

// removeStaleWorktreeRegistrationMetadata clears only the linked-worktree
// administration entry whose gitdir names worktreePath. Git refuses
// `worktree remove` when a foreign repository now occupies that path, while
// `worktree prune` keeps the stale entry because the directory still exists.
// Callers hold the repository worktree lock and have already established that
// the checkout at worktreePath is not owned by gitDir.
func removeStaleWorktreeRegistrationMetadata(
	ctx context.Context, gitDir, worktreePath string,
) error {
	tracked, err := gitDirTracksWorktreePath(ctx, gitDir, worktreePath)
	if err != nil {
		return err
	}
	if !tracked {
		return nil
	}

	metadataDir, ok, err := worktreeRegistrationMetadataDir(
		ctx, gitDir, worktreePath,
	)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("remove stale worktree registration: metadata not found")
	}
	if err := os.RemoveAll(metadataDir); err != nil {
		return fmt.Errorf("remove stale worktree registration: %w", err)
	}
	return nil
}

func writeWorkspaceOwnershipMarker(
	ctx context.Context, gitDir string, ws *Workspace,
) error {
	if ws == nil || strings.TrimSpace(ws.ID) == "" {
		return errors.New("workspace ID is required")
	}
	metadataDir, ok, err := worktreeRegistrationMetadataDir(
		ctx, gitDir, ws.WorktreePath,
	)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("workspace worktree registration metadata not found")
	}
	markerPath := filepath.Join(metadataDir, workspaceOwnershipMarkerFile)
	marker, exists, err := readWorkspaceOwnershipMarker(markerPath)
	if err != nil {
		return err
	}
	if exists {
		if marker != ws.ID {
			return errors.New("workspace ownership marker belongs to another workspace")
		}
		return nil
	}
	file, err := os.OpenFile(
		markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return fmt.Errorf("create workspace ownership marker: %w", err)
	}
	_, writeErr := file.WriteString(ws.ID + "\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(markerPath)
		return fmt.Errorf("write workspace ownership marker: %w", err)
	}
	return nil
}

func workspaceRegistrationMatches(
	ctx context.Context, gitDir, worktreePath, workspaceID string,
) (bool, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if strings.TrimSpace(worktreePath) == "" || workspaceID == "" {
		return false, nil
	}
	metadataDir, ok, err := worktreeRegistrationMetadataDir(
		ctx, gitDir, worktreePath,
	)
	if err != nil || !ok {
		return false, err
	}
	marker, exists, err := readWorkspaceOwnershipMarker(
		filepath.Join(metadataDir, workspaceOwnershipMarkerFile),
	)
	if err != nil || !exists || marker != workspaceID {
		return false, err
	}

	info, err := os.Lstat(worktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat workspace path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	currentGitDir, err := worktreeGitDir(ctx, worktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect workspace git dir: %w", err)
	}
	current, err := canonicalFilesystemPath(currentGitDir)
	if err != nil {
		return false, fmt.Errorf("resolve workspace git dir: %w", err)
	}
	want, err := canonicalFilesystemPath(metadataDir)
	if err != nil {
		return false, fmt.Errorf("resolve workspace registration: %w", err)
	}
	return current == want, nil
}

func readWorkspaceOwnershipMarker(markerPath string) (string, bool, error) {
	info, err := os.Lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat workspace ownership marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 256 {
		return "", true, nil
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return "", false, fmt.Errorf("read workspace ownership marker: %w", err)
	}
	return strings.TrimSpace(string(contents)), true, nil
}

func worktreeRegistrationMetadataDir(
	ctx context.Context, gitDir, worktreePath string,
) (string, bool, error) {
	out, err := gitCombinedOutput(
		ctx, gitDir,
		"rev-parse", "--path-format=absolute", "--git-path", "worktrees",
	)
	if err != nil {
		return "", false, fmt.Errorf("resolve git worktree metadata: %w", err)
	}
	metadataRoot := strings.TrimSpace(out)
	entries, err := os.ReadDir(metadataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read git worktree metadata: %w", err)
	}
	wantGitFile, err := canonicalWorktreeListPath(
		filepath.Join(worktreePath, ".git"),
	)
	if err != nil {
		return "", false, fmt.Errorf("resolve worktree gitfile: %w", err)
	}
	var found string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		metadataDir := filepath.Join(metadataRoot, entry.Name())
		gitFile, err := os.ReadFile(filepath.Join(metadataDir, "gitdir"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("read worktree registration: %w", err)
		}
		registeredGitFile := strings.TrimSpace(string(gitFile))
		if !filepath.IsAbs(registeredGitFile) {
			registeredGitFile = filepath.Join(metadataDir, registeredGitFile)
		}
		gotGitFile, err := canonicalWorktreeListPath(registeredGitFile)
		if err != nil {
			return "", false, fmt.Errorf("resolve registered worktree gitfile: %w", err)
		}
		if gotGitFile != wantGitFile {
			continue
		}
		if found != "" {
			return "", false, errors.New("multiple worktree registrations match path")
		}
		found = metadataDir
	}
	return found, found != "", nil
}

func gitDirHasStaleWorktreeRegistration(
	ctx context.Context, gitDir, worktreePath string,
) (bool, error) {
	info, err := os.Lstat(worktreePath)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat workspace path: %w", err)
	}
	tracked, err := gitDirTracksWorktreePath(ctx, gitDir, worktreePath)
	if err != nil || !tracked {
		return false, err
	}
	live, err := gitDirHasLiveWorktree(ctx, gitDir, worktreePath)
	if err != nil {
		return false, err
	}
	return !live, nil
}

func gitDirHasLiveWorktree(
	ctx context.Context, gitDir, worktreePath string,
) (bool, error) {
	info, err := os.Lstat(worktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat workspace path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	isRoot, err := worktreePathIsRoot(ctx, worktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect workspace worktree root: %w", err)
	}
	if !isRoot {
		return false, nil
	}
	commonDir, err := worktreeCommonGitDir(ctx, worktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return false, nil
		}
		return false, err
	}
	candidateCommonDir, err := worktreeCommonGitDir(ctx, gitDir)
	if err != nil {
		return false, fmt.Errorf("inspect cleanup git dir: %w", err)
	}
	candidate, err := canonicalFilesystemPath(candidateCommonDir)
	if err != nil {
		return false, fmt.Errorf("resolve cleanup git dir: %w", err)
	}
	current, err := canonicalFilesystemPath(commonDir)
	if err != nil {
		return false, fmt.Errorf("resolve workspace git common dir: %w", err)
	}
	if current != candidate {
		return false, nil
	}
	metadataDir, ok, err := worktreeRegistrationMetadataDir(
		ctx, candidateCommonDir, worktreePath,
	)
	if err != nil || !ok {
		return false, err
	}
	currentGitDir, err := worktreeGitDir(ctx, worktreePath)
	if err != nil {
		if isGitWorktreeAbsent(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect workspace git dir: %w", err)
	}
	currentRegistration, err := canonicalFilesystemPath(currentGitDir)
	if err != nil {
		return false, fmt.Errorf("resolve workspace git dir: %w", err)
	}
	expectedRegistration, err := canonicalFilesystemPath(metadataDir)
	if err != nil {
		return false, fmt.Errorf("resolve workspace registration: %w", err)
	}
	return currentRegistration == expectedRegistration, nil
}

func worktreeGitDir(ctx context.Context, worktreePath string) (string, error) {
	out, err := gitCombinedOutput(
		ctx, worktreePath,
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func gitDirOwnsCleanupWorktree(
	ctx context.Context, gitDir, worktreePath, workspaceID string,
) (bool, error) {
	owned, err := workspaceRegistrationMatches(
		ctx, gitDir, worktreePath, workspaceID,
	)
	if err != nil || owned {
		return owned, err
	}
	return gitDirHasLiveWorktree(ctx, gitDir, worktreePath)
}

func gitDirOwnsLinkedWorktree(
	ctx context.Context, gitDir, worktreePath string,
) (bool, error) {
	commonDir, err := canonicalFilesystemPath(gitDir)
	if err != nil {
		return false, fmt.Errorf("resolve git common dir: %w", err)
	}
	worktreeDir, err := canonicalWorktreeListPath(worktreePath)
	if err != nil {
		return false, fmt.Errorf("resolve workspace path: %w", err)
	}
	if pathContains(worktreeDir, commonDir) {
		return false, nil
	}
	return gitDirTracksWorktreePath(ctx, gitDir, worktreePath)
}

func canonicalFilesystemPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
		return evaluated, nil
	}
	return abs, nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func (m *Manager) cleanupTmuxSession(
	ctx context.Context, ws *Workspace,
) error {
	usesPtyOwner := m.UsesPtyOwnerForWorkspace(ws)
	if usesPtyOwner {
		if m.ptyOwner == nil {
			return fmt.Errorf("pty owner backend unavailable")
		}
		if err := m.ptyOwner.Stop(ctx, ws.TmuxSession); err != nil {
			return fmt.Errorf(
				"stop pty owner session %q: %w", ws.TmuxSession, err,
			)
		}
	}

	type cleanupTarget struct {
		session string
		main    bool
	}
	var sessions []cleanupTarget
	if !usesPtyOwner {
		sessions = append(sessions, cleanupTarget{
			session: ws.TmuxSession,
			main:    true,
		})
	}
	stored, err := m.db.ListWorkspaceRuntimeTmuxSessions(ctx, ws.ID)
	if err != nil {
		return err
	}
	for _, storedSession := range stored {
		sessions = append(sessions, cleanupTarget{
			session: storedSession.TmuxSession,
		})
	}

	var cleanupErrs []error
	for _, target := range sessions {
		if target.session == "" {
			continue
		}
		err := m.killTmuxSession(ctx, target.session)
		if err == nil || isTmuxKillSessionGone(err) {
			continue
		}
		if target.main {
			hasSession, checkErr := m.workspaceHasCreatedTmuxSession(ctx, ws)
			if checkErr != nil {
				cleanupErrs = append(cleanupErrs, checkErr)
				continue
			}
			if !hasSession {
				continue
			}
		}
		cleanupErrs = append(
			cleanupErrs,
			fmt.Errorf("kill tmux session %q: %w", target.session, err),
		)
	}
	if err := errors.Join(cleanupErrs...); err != nil {
		return err
	}
	if err := m.db.DeleteWorkspaceRuntimeSessions(ctx, ws.ID); err != nil {
		return err
	}
	return nil
}

// Get returns a workspace by ID, or nil if not found.
func (m *Manager) Get(
	ctx context.Context, id string,
) (*Workspace, error) {
	return m.db.GetWorkspace(ctx, id)
}

// GetByMRForProvider returns the workspace represented on a provider-scoped
// MR detail surface, or nil.
func (m *Manager) GetByMRForProvider(
	ctx context.Context,
	provider, platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return nil, err
	}
	return m.db.GetWorkspaceLinkedToMRForProvider(
		ctx, string(kind), platformHost, owner, name, mrNumber,
	)
}

// GetByIssueForProvider returns the workspace for a specific provider-scoped
// issue, or nil.
func (m *Manager) GetByIssueForProvider(
	ctx context.Context,
	provider, platformHost, owner, name string,
	issueNumber int,
) (*Workspace, error) {
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return nil, err
	}
	return m.db.GetWorkspaceByIssueForProvider(
		ctx, string(kind), platformHost, owner, name, issueNumber,
	)
}

// GetByLaunchSpecIdentity finds an existing provider-backed workspace by its
// stable repository and item identity. When the repository has been renamed,
// it adopts the verified current route before returning the workspace.
func (m *Manager) GetByLaunchSpecIdentity(
	ctx context.Context, spec WorkspaceLaunchSpec,
) (*Workspace, error) {
	existing, err := m.db.GetWorkspaceByLaunchSpecIdentity(
		ctx, spec.Repository.Provider, spec.Repository.PlatformHost,
		spec.Repository.PlatformRepoID, spec.ItemType, spec.ItemKey,
	)
	if err != nil || existing == nil {
		return existing, err
	}
	refreshed := spec
	refreshed.GitHeadRef = existing.GitHeadRef
	return m.db.PutRefreshedWorkspaceLaunchSpec(ctx, existing.ID, refreshed)
}

func (m *Manager) GetByItemKeyForProvider(
	ctx context.Context,
	provider, platformHost, owner, name, itemType, itemKey string,
) (*Workspace, error) {
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return nil, err
	}
	return m.db.GetWorkspaceByItemKeyForProvider(
		ctx, string(kind), platformHost, owner, name, itemType, itemKey,
	)
}

// GetSummary returns a workspace with joined MR metadata.
func (m *Manager) GetSummary(
	ctx context.Context, id string,
) (*WorkspaceSummary, error) {
	summary, err := m.db.GetWorkspaceSummary(ctx, id)
	if err != nil {
		return nil, err
	}
	if summary != nil {
		m.upsertWorkspaceSummaryCache(*summary)
	}
	return summary, nil
}

// ListSummaries returns all workspaces with joined MR metadata.
func (m *Manager) ListSummaries(
	ctx context.Context,
) ([]WorkspaceSummary, error) {
	summaries, err := m.db.ListWorkspaceSummaries(ctx)
	if err != nil {
		return nil, err
	}
	if len(summaries) == 0 {
		return m.cachedWorkspaceSummaries(), nil
	}
	return m.setWorkspaceSummaryCache(summaries), nil
}

func (m *Manager) cachedWorkspaceSummaries() []WorkspaceSummary {
	m.summaryCacheMu.RLock()
	defer m.summaryCacheMu.RUnlock()
	return slices.Clone(m.summaryCache)
}

func (m *Manager) setWorkspaceSummaryCache(
	summaries []WorkspaceSummary,
) []WorkspaceSummary {
	m.summaryCacheMu.Lock()
	defer m.summaryCacheMu.Unlock()
	m.summaryCache = filterDeletedWorkspaceSummaries(
		summaries,
		m.deletedSummaryIDs,
	)
	return slices.Clone(m.summaryCache)
}

func (m *Manager) upsertWorkspaceSummaryCache(summary WorkspaceSummary) {
	m.summaryCacheMu.Lock()
	defer m.summaryCacheMu.Unlock()
	if m.deletedSummaryIDs[summary.ID] {
		return
	}
	for i := range m.summaryCache {
		if m.summaryCache[i].ID == summary.ID {
			m.summaryCache[i] = summary
			return
		}
	}
	m.summaryCache = append(m.summaryCache, summary)
}

func (m *Manager) removeWorkspaceSummaryFromCache(id string) {
	m.summaryCacheMu.Lock()
	defer m.summaryCacheMu.Unlock()
	if m.deletedSummaryIDs == nil {
		m.deletedSummaryIDs = make(map[string]bool)
	}
	m.deletedSummaryIDs[id] = true
	m.summaryCache = slices.DeleteFunc(
		m.summaryCache,
		func(summary WorkspaceSummary) bool {
			return summary.ID == id
		},
	)
}

func filterDeletedWorkspaceSummaries(
	summaries []WorkspaceSummary,
	deleted map[string]bool,
) []WorkspaceSummary {
	if len(deleted) == 0 {
		return slices.Clone(summaries)
	}
	out := make([]WorkspaceSummary, 0, len(summaries))
	for _, summary := range summaries {
		if !deleted[summary.ID] {
			out = append(out, summary)
		}
	}
	return out
}

// ReapOrphanTmuxSessions kills kenn-forge-managed tmux sessions that no longer
// correspond to any durable workspace, host, or project-worktree row. This is
// a conservative startup cleanup for stale sessions left behind by crashes or
// previous bugs.
func (m *Manager) ReapOrphanTmuxSessions(ctx context.Context) error {
	workspaces, err := m.db.ListWorkspaces(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	live := make(map[string]bool, len(workspaces))
	for _, ws := range workspaces {
		if ws.TmuxSession == "" {
			continue
		}
		live[ws.TmuxSession] = true
	}
	storedSessions, err := m.db.ListAllWorkspaceRuntimeTmuxSessions(ctx)
	if err != nil {
		return err
	}
	for _, stored := range storedSessions {
		if stored.TmuxSession != "" {
			live[stored.TmuxSession] = true
		}
	}
	hostSessions, err := m.db.ListHostRuntimeTmuxSessions(ctx)
	if err != nil {
		return err
	}
	for _, stored := range hostSessions {
		if stored.SessionName != "" {
			live[stored.SessionName] = true
		}
	}
	projectSessions, err := m.db.ListAllProjectWorktreeTmuxSessions(ctx)
	if err != nil {
		return err
	}
	for _, stored := range projectSessions {
		if stored.SessionName != "" {
			live[stored.SessionName] = true
		}
	}

	sessions, err := m.listTmuxSessionInfos(ctx)
	if err != nil {
		if isTmuxCommandUnavailable(err) {
			return nil
		}
		return err
	}
	for _, session := range sessions {
		if !isForgeOwnedTmuxSessionName(session.name) {
			continue
		}
		if live[session.name] {
			continue
		}
		if session.owner != m.tmuxOwnerMarker() {
			continue
		}
		if err := m.killTmuxSession(ctx, session.name); err != nil &&
			!isTmuxKillSessionGone(err) {
			return fmt.Errorf(
				"kill orphan tmux session %q: %w", session.name, err,
			)
		}
	}
	return nil
}

// PruneMissingTmuxSessions reconciles persisted tmux ownership state against
// the host tmux server. Runtime-session rows whose tmux session was killed
// outside kenn-forge are removed. Ready workspaces whose primary tmux session is
// missing are marked errored so list responses stop probing dead session names
// and the UI can offer retry/delete. It reports whether anything was pruned so
// callers only notify clients about passes that changed state.
func (m *Manager) PruneMissingTmuxSessions(ctx context.Context) (bool, error) {
	changed := false
	sessions, err := m.listTmuxSessions(ctx)
	if err != nil {
		return false, err
	}
	live := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		live[session] = true
	}

	storedSessions, err := m.db.ListAllWorkspaceRuntimeTmuxSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, stored := range storedSessions {
		if stored.TmuxSession == "" {
			continue
		}
		if live[stored.TmuxSession] {
			continue
		}
		slog.Debug(
			"prune missing runtime tmux session",
			"workspace_id", stored.WorkspaceID,
			"target_key", stored.TargetKey,
			"tmux_session", stored.TmuxSession,
		)
		deleted, err := m.db.DeleteWorkspaceRuntimeSessionCreatedAt(
			ctx, stored.WorkspaceID, stored.SessionKey, stored.CreatedAt,
		)
		if err != nil {
			return changed, err
		}
		changed = changed || deleted
	}

	// A server with zero live sessions is the bulk-gone case: machine
	// reboot, or first boot after the dedicated-socket upgrade while
	// old sessions remain on the previous server. Base sessions are
	// recreated lazily on terminal attach, so ready workspaces stay
	// ready and self-heal; marking them all errored would invite retry,
	// whose cleanup force-removes worktrees. Individual missing
	// sessions on a live server remain real anomalies and are errored
	// below.
	if len(live) == 0 {
		return changed, nil
	}
	workspaces, err := m.db.ListWorkspaces(ctx)
	if err != nil {
		return changed, fmt.Errorf("list workspaces: %w", err)
	}
	for _, ws := range workspaces {
		if ws.Status != "ready" ||
			ws.TmuxSession == "" ||
			live[ws.TmuxSession] {
			continue
		}
		if m.usesPtyOwnerForWorkspace(&ws) {
			continue
		}
		msg := fmt.Sprintf(
			"tmux session is no longer running: %s",
			ws.TmuxSession,
		)
		slog.Debug(
			"mark workspace missing tmux session",
			"workspace_id", ws.ID,
			"tmux_session", ws.TmuxSession,
		)
		updated, err := m.db.MarkReadyWorkspaceError(ctx, ws.ID, msg)
		if err != nil {
			return changed, err
		}
		changed = changed || updated
	}
	return changed, nil
}

func isWorkspaceTmuxSessionName(session string) bool {
	for _, prefix := range []string{"forge-", "middleman-"} {
		if len(session) == len(prefix)+16 &&
			strings.HasPrefix(session, prefix) &&
			isLowerHex(session[len(prefix):]) {
			return true
		}
	}
	return false
}

func isForgeOwnedTmuxSessionName(session string) bool {
	if isWorkspaceTmuxSessionName(session) {
		return true
	}
	for _, prefix := range []string{"forge-", "middleman-"} {
		if !strings.HasPrefix(session, prefix) {
			continue
		}
		scopeAndKey := session[len(prefix):]
		separator := len(scopeAndKey) - 17
		if separator > 0 && scopeAndKey[separator] == '-' &&
			isLowerHex(scopeAndKey[separator+1:]) {
			return true
		}
	}
	return false
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (m *Manager) tmuxOwnerMarker() string {
	abs, err := filepath.Abs(m.worktreeDir)
	if err != nil {
		abs = m.worktreeDir
	}
	sum := sha256.Sum256([]byte(abs))
	return "kenn-forge:" + hex.EncodeToString(sum[:8])
}

// TmuxOwnerMarker returns the marker used to tag tmux sessions owned by this
// workspace manager.
func (m *Manager) TmuxOwnerMarker() string {
	return m.tmuxOwnerMarker()
}

func (m *Manager) workspaceHasCreatedTmuxSession(
	ctx context.Context, ws *Workspace,
) (bool, error) {
	if ws.Status == "ready" {
		return true, nil
	}

	events, err := m.db.ListWorkspaceSetupEvents(ctx, ws.ID)
	if err != nil {
		return false, fmt.Errorf("list workspace setup events: %w", err)
	}
	for _, event := range events {
		if event.Stage == workspaceSetupStageTmuxSession &&
			event.Outcome == "success" {
			return true, nil
		}
		if event.Stage == workspaceSetupStageSetup &&
			event.Outcome == "ready" {
			return true, nil
		}
	}
	return false, nil
}

// EnsureTmux creates a tmux session if it does not already exist,
// using the manager's configured tmux command prefix.
func (m *Manager) EnsureTmux(
	ctx context.Context, session, cwd string,
) error {
	exists, err := m.tmuxSessionExists(ctx, session)
	if err != nil {
		return fmt.Errorf("tmux has-session: %w", err)
	}
	if exists {
		return m.configureTmuxSession(ctx, session)
	}
	return m.newTmuxSession(ctx, session, cwd)
}

func isTmuxCommandUnavailable(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

type tmuxSessionInfo struct {
	name  string
	owner string
}

// tmuxSessionListFormat joins the session name and owner marker with a
// colon. tmux 3.6+ sanitizes control characters in -F output (a literal
// tab prints as "_"), so the separator must be printable. A colon is
// unambiguous because tmux replaces ":" in session names with "_", so no
// live session name can contain one; the owner marker ("kenn-forge:<hex>")
// does, which is why parsing cuts at the first colon only.
const tmuxSessionListFormat = "#{session_name}:#{@forge_owner}"

func (m *Manager) listTmuxSessionInfos(
	ctx context.Context,
) ([]tmuxSessionInfo, error) {
	cmd := m.tmuxExec(
		ctx,
		"list-sessions", "-F", tmuxSessionListFormat,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err != nil {
		if isTmuxSessionAbsent(stderr.Bytes(), err) {
			return nil, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("tmux list-sessions: %w: %s", err, msg)
	}
	var sessions []tmuxSessionInfo
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		name, owner, _ := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		sessions = append(sessions, tmuxSessionInfo{
			name:  name,
			owner: strings.TrimSpace(owner),
		})
	}
	return sessions, nil
}

func (m *Manager) listTmuxSessions(
	ctx context.Context,
) ([]string, error) {
	infos, err := m.listTmuxSessionInfos(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]string, 0, len(infos))
	for _, info := range infos {
		sessions = append(sessions, info.name)
	}
	return sessions, nil
}

// RecordRuntimeSession stores a durable runtime session identity and its
// metadata. Normal launched terminals and agents use the session scope.
func (m *Manager) RecordRuntimeSession(
	ctx context.Context,
	session db.WorkspaceRuntimeSession,
) error {
	if session.SessionKey == "" {
		return nil
	}
	return m.db.RecordWorkspaceRuntimeSession(ctx, &session)
}

func (m *Manager) PreferredWorkspaceAgentTarget(
	ctx context.Context,
	since time.Time,
	targetKeys []string,
) (string, bool, error) {
	return m.db.PreferredWorkspaceAgentTarget(ctx, since, targetKeys)
}

func (m *Manager) UpdateRuntimeSessionLabel(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	label string,
) (bool, error) {
	if sessionKey == "" {
		return false, nil
	}
	return m.db.UpdateWorkspaceRuntimeSessionLabel(
		ctx, workspaceID, sessionKey, label,
	)
}

func (m *Manager) ForgetRuntimeSession(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
) error {
	if sessionKey == "" {
		return nil
	}
	return m.db.DeleteWorkspaceRuntimeSession(ctx, workspaceID, sessionKey)
}

func (m *Manager) ForgetRuntimeSessionCreatedAt(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	createdAt time.Time,
) (bool, error) {
	if sessionKey == "" {
		return false, nil
	}
	return m.db.DeleteWorkspaceRuntimeSessionCreatedAt(
		ctx, workspaceID, sessionKey, createdAt,
	)
}

func (m *Manager) ForgetRuntimeSessionAfterExit(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	createdAt time.Time,
	tmuxSession string,
) (bool, error) {
	if sessionKey == "" {
		return false, nil
	}
	tmuxSession = strings.TrimSpace(tmuxSession)
	if tmuxSession != "" {
		exists, err := m.tmuxSessionExists(ctx, tmuxSession)
		if err != nil {
			return false, fmt.Errorf(
				"check exited runtime tmux session %q: %w",
				tmuxSession, err,
			)
		}
		if exists {
			return false, nil
		}
	}
	return m.db.DeleteWorkspaceRuntimeSessionCreatedAt(
		ctx, workspaceID, sessionKey, createdAt,
	)
}

func (m *Manager) RuntimeSessionsForWorkspace(
	ctx context.Context,
	workspaceID string,
) ([]db.WorkspaceRuntimeSession, error) {
	return m.db.ListWorkspaceRuntimeSessions(ctx, workspaceID)
}

func (m *Manager) AllRuntimeSessions(
	ctx context.Context,
) ([]db.WorkspaceRuntimeSession, error) {
	return m.db.ListAllWorkspaceRuntimeSessions(ctx)
}

func (m *Manager) RuntimeSessionKeysForWorkspace(
	ctx context.Context,
	workspaceID string,
) ([]string, error) {
	sessions, err := m.db.ListWorkspaceRuntimeSessions(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionKey != "" {
			keys = append(keys, session.SessionKey)
		}
	}
	return keys, nil
}

// StopStoredRuntimeSessionByKey cleans up a persisted runtime session even
// when the in-memory runtime manager no longer knows about it.
func (m *Manager) StopStoredRuntimeSessionByKey(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
) (bool, error) {
	if sessionKey == "" {
		return false, nil
	}
	stored, err := m.db.ListWorkspaceRuntimeSessions(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	for _, storedSession := range stored {
		if storedSession.SessionKey == sessionKey {
			return m.stopStoredRuntimeSession(ctx, workspaceID, storedSession)
		}
	}
	return false, nil
}

func (m *Manager) stopStoredRuntimeSession(
	ctx context.Context,
	workspaceID string,
	storedSession db.WorkspaceRuntimeSession,
) (bool, error) {
	m.runtimeTmuxMu.Lock()
	defer m.runtimeTmuxMu.Unlock()
	if storedSession.TmuxSession != "" {
		if err := m.killTmuxSession(ctx, storedSession.TmuxSession); err != nil &&
			!isTmuxKillSessionGone(err) {
			return true, fmt.Errorf(
				"kill tmux session %q: %w",
				storedSession.TmuxSession, err,
			)
		}
	} else if m.ptyOwner != nil {
		if err := m.ptyOwner.Stop(ctx, storedSession.SessionKey); err != nil {
			return true, fmt.Errorf(
				"stop pty owner session %q: %w",
				storedSession.SessionKey, err,
			)
		}
	}
	if err := m.db.DeleteWorkspaceRuntimeSession(
		ctx, workspaceID, storedSession.SessionKey,
	); err != nil {
		return true, err
	}
	return true, nil
}

// TmuxSessionsForWorkspace returns the persisted workspace tmux
// session plus stored per-agent sessions. Runtime tmux sessions are
// stored rather than discovered by naming convention so restart
// recovery follows explicit ownership state.
func (m *Manager) TmuxSessionsForWorkspace(
	ctx context.Context,
	workspaceID string,
	baseSession string,
) ([]string, error) {
	stored, err := m.db.ListWorkspaceRuntimeTmuxSessions(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(stored)+1)
	if baseSession != "" {
		seen[baseSession] = true
		out = append(out, baseSession)
	}
	for _, storedSession := range stored {
		session := storedSession.TmuxSession
		if session == "" || seen[session] {
			continue
		}
		seen[session] = true
		out = append(out, session)
	}
	return out, nil
}

// TmuxPaneTitle returns the active pane title for a session. Agents
// can update this via terminal title escape sequences, which tmux
// exposes through the pane_title format.
func (m *Manager) TmuxPaneTitle(
	ctx context.Context, session string,
) (string, error) {
	return m.tmuxPaneTitle(ctx, session)
}

// TerminalPaneSnapshot returns recent terminal output for the backend
// that owns the workspace's primary terminal.
func (m *Manager) TerminalPaneSnapshot(
	ctx context.Context, ws *db.Workspace,
	session string,
) (TerminalPaneSnapshot, error) {
	if ws != nil && session == ws.TmuxSession && m.UsesPtyOwnerForWorkspace(ws) {
		if m.ptyOwner == nil {
			return TerminalPaneSnapshot{}, fmt.Errorf("pty owner backend unavailable")
		}
		status, err := m.ptyOwner.Snapshot(ctx, session)
		if err != nil {
			return TerminalPaneSnapshot{}, err
		}
		return TerminalPaneSnapshot{
			Title:  status.Title,
			Output: string(status.Output),
		}, nil
	}
	return m.tmuxPaneSnapshot(ctx, session)
}

// tmuxPaneSnapshot returns the active pane title and recent pane
// output for passive activity detection.
func (m *Manager) tmuxPaneSnapshot(
	ctx context.Context, session string,
) (TerminalPaneSnapshot, error) {
	title, err := m.tmuxPaneTitle(ctx, session)
	if err != nil {
		return TerminalPaneSnapshot{}, err
	}

	cmd := m.tmuxExec(
		ctx,
		"capture-pane", "-p",
		"-t", session,
		"-S", fmt.Sprintf("-%d", tmuxCaptureScrollbackLines),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return TerminalPaneSnapshot{}, fmt.Errorf(
			"tmux capture-pane: %w: %s", err, msg,
		)
	}
	return TerminalPaneSnapshot{
		Title:  title,
		Output: stdout.String(),
	}, nil
}

func (m *Manager) tmuxPaneTitle(
	ctx context.Context, session string,
) (string, error) {
	cmd := m.tmuxExec(
		ctx,
		"display-message", "-p",
		"-t", session,
		"#{pane_title}",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("tmux display-message: %w: %s", err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (m *Manager) newTmuxSession(
	ctx context.Context, session, cwd string,
) error {
	shell := userLoginShell()
	// The pane receives the credential-sanitized full daemon
	// environment through the env-file handoff, matching runtime shell
	// sessions: benign daemon variables stay usable in the base
	// terminal while the tmux client itself stays allowlisted.
	paneCommand, cleanupHandoff, err := localruntime.NewTmuxPaneHandoff(
		localruntime.SessionEnvironment(
			os.Environ(), m.currentTmuxStripEnvVars(),
		),
		[]string{shell, "-l"},
	)
	if err != nil {
		return fmt.Errorf("prepare base tmux pane environment: %w", err)
	}
	created := false
	defer func() {
		if !created {
			cleanupHandoff()
		}
	}()
	cmd := m.tmuxExec(
		ctx,
		"new-session", "-d",
		"-s", session,
		"-c", cwd,
		paneCommand,
	)
	if err := runBuiltCmd(ctx, cmd); err != nil {
		return err
	}
	created = true
	if err := m.setTmuxOwnerMarker(ctx, session); err != nil {
		if killErr := m.killTmuxSession(ctx, session); killErr != nil &&
			!isTmuxKillSessionGone(killErr) {
			return fmt.Errorf(
				"set tmux owner marker: %w; cleanup new tmux session: %v",
				err, killErr,
			)
		}
		return fmt.Errorf("set tmux owner marker: %w", err)
	}
	if err := m.configureTmuxSession(ctx, session); err != nil {
		if killErr := m.killTmuxSession(ctx, session); killErr != nil &&
			!isTmuxKillSessionGone(killErr) {
			return fmt.Errorf(
				"configure tmux session: %w; cleanup new tmux session: %v",
				err, killErr,
			)
		}
		return fmt.Errorf("configure tmux session: %w", err)
	}
	if m.currentHideTmuxStatus() {
		if err := m.setTmuxStatus(ctx, session, false); err != nil {
			if killErr := m.killTmuxSession(ctx, session); killErr != nil &&
				!isTmuxKillSessionGone(killErr) {
				return fmt.Errorf(
					"hide tmux status: %w; cleanup new tmux session: %v",
					err, killErr,
				)
			}
			return fmt.Errorf("hide tmux status: %w", err)
		}
	}
	return nil
}

func (m *Manager) configureTmuxSession(
	ctx context.Context,
	session string,
) error {
	graphics := m.currentTmuxGraphics()
	dedicatedServer := config.IsDefaultTmuxCommand(m.tmuxCmd)
	if dedicatedServer {
		if err := m.applyTmuxServerGraphics(ctx, graphics); err != nil {
			return err
		}
		if err := m.applyTmuxMouse(ctx); err != nil {
			return err
		}
	}
	if !graphics && !dedicatedServer {
		return nil
	}
	return m.setTmuxPassthrough(ctx, session, graphics)
}

func (m *Manager) applyTmuxServerGraphics(ctx context.Context, enabled bool) error {
	value := "off"
	if enabled {
		value = "on"
	}
	if err := runBuiltCmd(
		ctx,
		m.tmuxExec(ctx, "set-option", "-q", "-g", "allow-passthrough", value),
	); err != nil {
		return fmt.Errorf("configure global tmux passthrough: %w", err)
	}
	args := []string{
		"set-option", "-q", "-s", "terminal-features[100]",
		"xterm-256color:sixel",
	}
	if !enabled {
		args = []string{
			"set-option", "-q", "-s", "-u", "terminal-features[100]",
		}
	}
	if err := runBuiltCmd(ctx, m.tmuxExec(ctx, args...)); err != nil {
		return fmt.Errorf("configure tmux SIXEL: %w", err)
	}
	return nil
}

func (m *Manager) setTmuxPassthrough(
	ctx context.Context,
	target string,
	enabled bool,
) error {
	value := "off"
	if enabled {
		value = "on"
	}
	if err := runBuiltCmd(
		ctx,
		m.tmuxExec(
			ctx,
			"set-option", "-q", "-p", "-t", target,
			"allow-passthrough", value,
		),
	); err != nil {
		return fmt.Errorf("configure tmux passthrough: %w", err)
	}
	return nil
}

func (m *Manager) unsetTmuxPassthrough(
	ctx context.Context,
	target string,
) error {
	if err := runBuiltCmd(
		ctx,
		m.tmuxExec(
			ctx,
			"set-option", "-q", "-p", "-u", "-t", target,
			"allow-passthrough",
		),
	); err != nil {
		return fmt.Errorf("clear tmux passthrough override: %w", err)
	}
	return nil
}

const tmuxPaneListFormat = "#{pane_id}"

func (m *Manager) listTmuxPanes(
	ctx context.Context,
	session string,
) ([]string, error) {
	cmd := m.tmuxExec(
		ctx,
		"list-panes", "-s", "-t", session, "-F", tmuxPaneListFormat,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("tmux list-panes: %w: %s", err, msg)
	}
	var panes []string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		pane := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if pane != "" {
			panes = append(panes, pane)
		}
	}
	return panes, nil
}

func (m *Manager) applyTmuxSessionGraphics(
	ctx context.Context,
	session string,
	dedicatedServer bool,
) error {
	panes, err := m.listTmuxPanes(ctx, session)
	if err != nil {
		return err
	}
	var errs []error
	for _, pane := range panes {
		if dedicatedServer {
			err = m.unsetTmuxPassthrough(ctx, pane)
		} else {
			err = m.setTmuxPassthrough(ctx, pane, true)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pane %q: %w", pane, err))
		}
	}
	return errors.Join(errs...)
}

// ApplyTmuxGraphics updates the Forge-owned tmux server and every managed pane.
// Custom commands may target a user's shared server, so Forge changes only
// panes in marked Forge sessions, and only while graphics are enabled.
func (m *Manager) ApplyTmuxGraphics(ctx context.Context) error {
	dedicatedServer := config.IsDefaultTmuxCommand(m.tmuxCmd)
	enabled := m.currentTmuxGraphics()
	if !dedicatedServer && !enabled {
		return nil
	}
	infos, err := m.listTmuxSessionInfos(ctx)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return nil
	}
	var errs []error
	if dedicatedServer {
		if err := m.applyTmuxServerGraphics(ctx, enabled); err != nil {
			errs = append(errs, err)
		}
	}
	for _, info := range infos {
		if !dedicatedServer && info.owner != m.tmuxOwnerMarker() {
			continue
		}
		if err := m.applyTmuxSessionGraphics(ctx, info.name, dedicatedServer); err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", info.name, err))
		}
	}
	return errors.Join(errs...)
}

// ApplyTmuxMouse updates a running Forge-owned tmux server. Custom commands
// may target a user's shared server, so Forge never changes their global
// options. A missing dedicated server has no state to update.
func (m *Manager) ApplyTmuxMouse(ctx context.Context) error {
	if !config.IsDefaultTmuxCommand(m.tmuxCmd) {
		return nil
	}
	sessions, err := m.listTmuxSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	return m.applyTmuxMouse(ctx)
}

func (m *Manager) applyTmuxMouse(ctx context.Context) error {
	value := "off"
	if m.currentTmuxMouse() {
		value = "on"
	}
	if err := runBuiltCmd(
		ctx,
		m.tmuxExec(ctx, "set-option", "-q", "-g", "mouse", value),
	); err != nil {
		return fmt.Errorf("configure tmux mouse: %w", err)
	}
	return nil
}

func (m *Manager) setTmuxOwnerMarker(
	ctx context.Context, session string,
) error {
	return runBuiltCmd(
		ctx,
		m.tmuxExec(
			ctx,
			"set-option", "-t", session,
			"@forge_owner", m.tmuxOwnerMarker(),
		),
	)
}

func (m *Manager) setTmuxStatus(
	ctx context.Context,
	session string,
	enabled bool,
) error {
	value := "off"
	if enabled {
		value = "on"
	}
	return runBuiltCmd(
		ctx,
		m.tmuxExec(ctx, "set-option", "-q", "-t", session, "status", value),
	)
}

// tmuxSessionExists runs `tmux has-session` and distinguishes a
// genuine "session absent" signal from a wrapper/binary failure.
// tmux reports session-absent by exiting 1 with one of two
// well-known stderr messages:
//
//	can't find session: <name>
//	no server running on <socket>
//
// Stdout and stderr are captured separately so a wrapper that
// happens to emit those phrases on stdout for unrelated reasons
// cannot masquerade as session-absent. Any other failure — missing
// binary (non-ExitError), wrapper exit codes other than 1, or
// exit-1 without the canonical stderr — propagates so
// misconfiguration surfaces instead of silently falling through to
// new-session through the same broken wrapper.
func (m *Manager) tmuxSessionExists(
	ctx context.Context, session string,
) (bool, error) {
	cmd := m.tmuxExec(ctx, "has-session", "-t", session)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err == nil {
		return true, nil
	}
	if isTmuxSessionAbsent(stderr.Bytes(), err) {
		return false, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	return false, fmt.Errorf("%w: %s", err, msg)
}

// isTmuxSessionAbsent reports whether a has-session failure is
// tmux's documented "session does not exist" signal. Must be both
// exit code 1 AND one of tmux's specific stderr phrases. Plain
// exit 1 is a common generic wrapper/shell failure code, and
// stdout content is not load-bearing — a wrapper could emit
// anything there for unrelated reasons.
func isTmuxSessionAbsent(stderr []byte, err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	msg := string(stderr)
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no server running") ||
		(strings.Contains(msg, "error connecting to") &&
			strings.Contains(msg, "No such file or directory"))
}

func isTmuxKillSessionGone(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return isTmuxSessionAbsent([]byte(msg), err) ||
		strings.Contains(msg, "server exited unexpectedly")
}

// killTmuxSession kills a tmux session via the manager's prefix.
// Errors are returned rather than logged — callers decide whether
// to ignore them (Delete ignores; tests assert).
func (m *Manager) killTmuxSession(
	ctx context.Context, session string,
) error {
	return runBuiltCmd(
		ctx, m.tmuxExec(ctx, "kill-session", "-t", session),
	)
}

// userLoginShell resolves the current user's login shell from
// the OS user database (passwd), falling back to $SHELL, then
// /bin/sh.
func userLoginShell() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		if shell := lookupPasswdShell(u.Username); shell != "" {
			return shell
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func lookupPasswdShell(username string) string {
	cmd := procutil.Command("getent", "passwd", username)
	out, err := procutil.Output(
		context.Background(), cmd, "shell lookup subprocess capacity",
	)
	if err == nil {
		return shellFromPasswdLine(string(out))
	}
	// Fallback: read /etc/passwd directly with exact field match
	// (no grep — avoids regex injection from metacharacters in
	// usernames).
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	prefix := username + ":"
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return shellFromPasswdLine(line)
		}
	}
	return ""
}

func shellFromPasswdLine(line string) string {
	line = strings.TrimSpace(line)
	fields := strings.Split(line, ":")
	if len(fields) < 7 {
		return ""
	}
	shell := strings.TrimSpace(fields[len(fields)-1])
	if shell == "" || shell == "/usr/bin/false" ||
		shell == "/bin/false" || shell == "/sbin/nologin" {
		return ""
	}
	return shell
}

// runGitWithoutHooks executes a git mutation in dir and returns combined
// output on error. Internal workspace setup and cleanup paths must not run
// repo-local hooks from user-owned worktree bases; user-triggered foreground
// actions such as branch push/pull may call gitCombinedOutput directly when
// hooks are expected to run.
func runGitWithoutHooks(ctx context.Context, dir string, args ...string) error {
	_, err := gitCombinedOutput(ctx, dir, gitArgsWithoutHooks(args...)...)
	return err
}

// gitCombinedOutput runs git in dir, returning combined output. On failure
// the returned error includes the trimmed output, and the raw output is
// still returned so callers can inspect it.
func gitCombinedOutput(
	ctx context.Context, dir string, args ...string,
) (string, error) {
	cmd := workspaceGitCommand(ctx, dir, args...)
	out, err := procutil.CombinedOutput(
		ctx, cmd, "git subprocess capacity",
	)
	if err != nil {
		return string(out), fmt.Errorf(
			"%w: %s", err, strings.TrimSpace(string(out)),
		)
	}
	return string(out), nil
}

type gitRefState struct {
	objectName string
	symref     string
}

func snapshotGitRefs(ctx context.Context, dir string) (map[string]gitRefState, error) {
	out, err := gitCombinedOutput(
		ctx, dir, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)",
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot git refs: %w", err)
	}
	refs := make(map[string]gitRefState)
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("snapshot git refs: malformed ref record %q", line)
		}
		refs[parts[0]] = gitRefState{objectName: parts[1], symref: parts[2]}
	}
	return refs, nil
}

func restoreGitRefs(ctx context.Context, dir string, before map[string]gitRefState) error {
	current, err := snapshotGitRefs(ctx, dir)
	if err != nil {
		return err
	}
	var deleteRefs []string
	for ref := range current {
		if _, ok := before[ref]; !ok {
			deleteRefs = append(deleteRefs, ref)
		}
	}
	slices.Sort(deleteRefs)
	slices.Reverse(deleteRefs)
	var restoreErr error
	for _, ref := range deleteRefs {
		restoreErr = errors.Join(
			restoreErr,
			runGitWithoutHooks(ctx, dir, "update-ref", "--no-deref", "-d", ref),
		)
	}
	var refs []string
	for ref := range before {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		state := before[ref]
		if currentState, ok := current[ref]; ok && currentState == state {
			continue
		}
		if state.symref != "" {
			restoreErr = errors.Join(
				restoreErr,
				runGitWithoutHooks(ctx, dir, "symbolic-ref", ref, state.symref),
			)
			continue
		}
		restoreErr = errors.Join(
			restoreErr,
			runGitWithoutHooks(ctx, dir, "update-ref", "--no-deref", ref, state.objectName),
		)
	}
	return restoreErr
}

func runRouteValidatedFetch(
	ctx context.Context,
	dir string,
	validateRoute func(context.Context) error,
	fetch func() error,
) error {
	if validateRoute == nil {
		return fetch()
	}
	if err := validateRoute(ctx); err != nil {
		return err
	}
	refs, err := snapshotGitRefs(ctx, dir)
	if err != nil {
		return err
	}
	fetchErr := fetch()
	validationErr := validateRoute(ctx)
	if fetchErr == nil && validationErr == nil {
		return nil
	}
	restoreErr := restoreGitRefs(context.WithoutCancel(ctx), dir, refs)
	if restoreErr != nil {
		restoreErr = fmt.Errorf("restore git refs after rejected fetch: %w", restoreErr)
	}
	return errors.Join(fetchErr, validationErr, restoreErr)
}

func gitArgsWithoutHooks(args ...string) []string {
	gitArgs := make([]string, 0, len(args)+2)
	gitArgs = append(gitArgs, "-c", "core.hooksPath=/dev/null")
	return append(gitArgs, args...)
}

func (m *Manager) fetchWorkspaceBase(
	ctx context.Context,
	dir, platformName, platformHost, owner, name string,
	remote string,
	requireRemoteHead bool,
) error {
	run := runGitWithoutHooks
	if m.clones != nil {
		run = func(ctx context.Context, dir string, args ...string) error {
			out, err := m.clones.RunGitForRepoRemote(
				ctx, platformName, platformHost, owner, name, remote, dir, args...,
			)
			if err != nil {
				return fmt.Errorf(
					"%w: %s", err, strings.TrimSpace(string(out)),
				)
			}
			return nil
		}
	}
	return fetchWorkspaceBaseWithGit(ctx, run, dir, remote, requireRemoteHead)
}

func (m *Manager) fetchWorkspaceMergeRequestHeadRef(
	ctx context.Context,
	dir string,
	remote string,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
) error {
	run := runGitWithoutHooks
	runFork := run
	if m.clones != nil {
		run = func(ctx context.Context, dir string, args ...string) error {
			out, err := m.clones.RunGitForRepoRemote(
				ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
				remote, dir, args...,
			)
			if err != nil {
				return fmt.Errorf(
					"%w: %s", err, strings.TrimSpace(string(out)),
				)
			}
			return nil
		}
		runFork = func(ctx context.Context, dir string, args ...string) error {
			out, err := m.clones.RunGitForRemote(
				ctx, ws.Platform, ws.PlatformHost,
				launchSpec.Pull.HeadRepoCloneURL, dir, args...,
			)
			if err != nil {
				return fmt.Errorf(
					"%w: %s", err, strings.TrimSpace(string(out)),
				)
			}
			return nil
		}
	}
	return fetchWorkspaceMergeRequestHeadRefWithGit(
		ctx, run, runFork, dir, remote, ws, launchSpec,
	)
}

func fetchWorkspaceMergeRequestHeadRefWithGit(
	ctx context.Context,
	runBase func(context.Context, string, ...string) error,
	runFork func(context.Context, string, ...string) error,
	dir string,
	remote string,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
) error {
	if launchSpec == nil || launchSpec.Pull == nil {
		return errors.New("pull-request launch specification is required for head fetch")
	}
	targetRef := workspaceMergeRequestHeadRef(ws)
	switch launchSpec.Pull.HeadRepoKind {
	case "same_repo":
		return runBase(
			ctx, dir, gitArgsWithoutHooks(
				"fetch", "--no-tags", "--recurse-submodules=no",
				remote, "+"+targetRef+":"+targetRef,
			)...,
		)
	case "fork":
		// Provider-owned pull refs survive branch and fork deletion. Prefer the
		// base repository's synthetic ref, then fall back to the live fork.
		if err := runBase(
			ctx, dir, gitArgsWithoutHooks(
				"fetch", "--no-tags", "--recurse-submodules=no",
				remote, "+"+targetRef+":"+targetRef,
			)...,
		); err == nil {
			return nil
		}
		return runFork(
			ctx, dir, gitArgsWithoutHooks(
				"fetch", "--no-tags", "--recurse-submodules=no",
				launchSpec.Pull.HeadRepoCloneURL,
				"+refs/heads/"+launchSpec.Pull.HeadBranch+":"+targetRef,
			)...,
		)
	case "unknown":
		return errors.New("pull-request head repository identity is unavailable")
	default:
		return errors.New("pull-request head repository classification is invalid")
	}
}

func fetchWorkspaceBaseWithGit(
	ctx context.Context,
	run func(context.Context, string, ...string) error,
	dir string,
	remote string,
	requireRemoteHead bool,
) error {
	// The clones-backed run bypasses runGitWithoutHooks, so hook
	// suppression must be applied here as well; on the runGitWithoutHooks
	// path the flag is duplicated, which git tolerates.
	runWithoutHooks := func(ctx context.Context, dir string, args ...string) error {
		return run(ctx, dir, gitArgsWithoutHooks(args...)...)
	}
	if err := runWithoutHooks(
		ctx, dir,
		"fetch", "--prune", "--no-tags", "--recurse-submodules=no",
		"--negotiation-tip="+remoteTrackingGlob(remote), remote,
		remoteBaseFetchRefspec(remote),
	); err != nil {
		return fmt.Errorf("fetch configured worktree base: %w", err)
	}
	if err := refreshWorkspaceBaseRemoteHeadWithGit(
		ctx, runWithoutHooks, dir, remote,
	); err != nil {
		if !requireRemoteHead {
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) syncWorkspaceBaseBranch(
	ctx context.Context, dir, remote string, ws *Workspace,
) error {
	if ws == nil || ws.ItemType != db.WorkspaceItemTypePullRequest ||
		ws.Platform == "" || ws.PlatformHost == "" ||
		ws.RepoOwner == "" || ws.RepoName == "" {
		return nil
	}
	releaseReconciliation, err := m.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return fmt.Errorf("lock repository reconciliation for base branch sync: %w", err)
	}
	defer releaseReconciliation()
	if err := m.verifyWorkspaceRepositoryUnderReconciliationRead(ctx, ws); err != nil {
		return err
	}
	repo, err := m.workspaceRepoUnderReconciliationRead(
		ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
	)
	if err != nil {
		return fmt.Errorf("look up workspace repo for base branch sync: %w", err)
	}
	if repo == nil {
		return nil
	}
	mr, err := m.db.GetMergeRequestByRepoIDAndNumber(
		ctx, repo.ID, ws.ItemNumber,
	)
	if err != nil {
		return fmt.Errorf("look up workspace merge request for base branch sync: %w", err)
	}
	if mr == nil || strings.TrimSpace(mr.BaseBranch) == "" {
		return nil
	}
	return syncLocalBaseBranch(ctx, dir, remote, ws.ID, strings.TrimSpace(mr.BaseBranch))
}

func syncLocalBaseBranch(
	ctx context.Context, dir, remote, workspaceID, branch string,
) error {
	localRef := "refs/heads/" + branch
	remoteRef := remoteTrackingRef(remote, branch)
	remoteSHA, exists, err := gitRefSHA(ctx, dir, remoteRef)
	if err != nil {
		return fmt.Errorf("inspect remote base branch %q: %w", branch, err)
	}
	if !exists {
		slog.Warn("workspace base branch sync skipped",
			"workspace_id", workspaceID, "branch", branch,
			"reason", "remote-tracking branch is missing")
		return nil
	}
	localSHA, exists, err := gitRefSHA(ctx, dir, localRef)
	if err != nil {
		return fmt.Errorf("inspect local base branch %q: %w", branch, err)
	}
	if exists {
		if localSHA == remoteSHA {
			return nil
		}
		checkedOut, err := localBranchCheckedOut(ctx, dir, branch)
		if err != nil {
			return fmt.Errorf("inspect base branch worktrees: %w", err)
		}
		if checkedOut {
			slog.Warn("workspace base branch sync skipped",
				"workspace_id", workspaceID, "branch", branch,
				"reason", "local branch is checked out")
			return nil
		}
		ancestor, err := gitCommitIsAncestor(ctx, dir, localSHA, remoteSHA)
		if err != nil {
			return fmt.Errorf("check base branch fast-forward: %w", err)
		}
		if !ancestor {
			slog.Warn("workspace base branch sync skipped",
				"workspace_id", workspaceID, "branch", branch,
				"local_sha", localSHA, "remote_sha", remoteSHA,
				"reason", "update is not a fast-forward")
			return nil
		}
	}
	if err := runGitWithoutHooks(
		ctx, dir, "branch", "--force", "--", branch, remoteSHA,
	); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else {
		checkedOut, checkErr := localBranchCheckedOut(ctx, dir, branch)
		if checkErr == nil && checkedOut {
			slog.Warn("workspace base branch sync skipped",
				"workspace_id", workspaceID, "branch", branch,
				"reason", "local branch became checked out")
			return nil
		}
		if !exists {
			occupied, checkErr := localBranchRefNamespaceOccupied(ctx, dir, branch)
			if checkErr == nil && occupied {
				slog.Warn("workspace base branch sync skipped",
					"workspace_id", workspaceID, "branch", branch,
					"reason", "local branch ref namespace is occupied")
				return nil
			}
		}
		return fmt.Errorf("update local base branch %q: %w", branch, err)
	}
}

func localBranchRefNamespaceOccupied(
	ctx context.Context, dir, branch string,
) (bool, error) {
	parts := strings.Split(branch, "/")
	for i := 1; i < len(parts); i++ {
		_, exists, err := gitRefSHA(ctx, dir, "refs/heads/"+strings.Join(parts[:i], "/"))
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	out, err := gitCombinedOutput(
		ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch+"/",
	)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func gitCommitIsAncestor(
	ctx context.Context, dir, ancestor, descendant string,
) (bool, error) {
	_, err := gitCombinedOutput(
		ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant,
	)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func refreshWorkspaceBaseRemoteHeadWithGit(
	ctx context.Context,
	run func(context.Context, string, ...string) error,
	dir, remote string,
) error {
	setHeadErr := run(ctx, dir, "remote", "set-head", remote, "-a")
	if setHeadErr == nil {
		return nil
	}
	if remoteHeadRefReady(ctx, dir, remote) {
		return nil
	}
	for _, branch := range []string{"main", "master"} {
		ref := remoteTrackingRef(remote, branch)
		if gitRefExists(ctx, dir, ref) {
			if err := run(
				ctx, dir, "symbolic-ref",
				remoteTrackingRef(remote, "HEAD"), ref,
			); err != nil {
				return fmt.Errorf(
					"set configured worktree base %s/HEAD: %w", remote, err,
				)
			}
			return nil
		}
	}
	return fmt.Errorf(
		"refresh configured worktree base %s/HEAD: %w", remote, setHeadErr,
	)
}

func remoteHeadRefReady(ctx context.Context, dir, remote string) bool {
	out, err := gitOutput(
		ctx, dir, "symbolic-ref", "--quiet", remoteTrackingRef(remote, "HEAD"),
	)
	if err != nil {
		return false
	}
	return gitRefExists(ctx, dir, strings.TrimSpace(out))
}

func gitRefExists(ctx context.Context, dir, ref string) bool {
	cmd := workspaceGitCommand(
		ctx, dir, "show-ref", "--verify", "--quiet", ref,
	)
	err := cmd.Run()
	return err == nil
}

func runGitWorktreeAdd(
	ctx context.Context, dir, worktreePath string, args ...string,
) error {
	gitArgs := make([]string, 0, len(args)+3)
	gitArgs = append(gitArgs, "worktree", "add", worktreePath)
	gitArgs = append(gitArgs, args...)
	if err := runGitWithoutHooks(ctx, dir, gitArgs...); err != nil {
		return err
	}
	if err := configureBareLinkedWorktree(ctx, dir, worktreePath); err != nil {
		cleanupErr := runGitWithoutHooks(
			ctx, dir, "worktree", "remove", "--force", worktreePath,
		)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// Bare clones stay bare in shared config so repository tools can identify the
// layout. Each linked checkout overrides that value once worktree config is on.
func configureBareLinkedWorktree(
	ctx context.Context, commonDir, worktreePath string,
) error {
	bare, err := gitOutput(ctx, commonDir, "config", "--bool", "core.bare")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect shared core.bare: %w", err)
	}
	if strings.TrimSpace(bare) != "true" {
		return nil
	}
	worktreeConfig, err := gitOutput(
		ctx, commonDir, "config", "--bool", "extensions.worktreeConfig",
	)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect shared worktree config: %w", err)
	}
	if strings.TrimSpace(worktreeConfig) != "true" {
		return nil
	}
	gitDir, err := gitOutput(
		ctx, worktreePath, "-c", "core.bare=false",
		"rev-parse", "--path-format=absolute", "--git-dir",
	)
	if err != nil {
		return fmt.Errorf("resolve linked worktree Git directory: %w", err)
	}
	if err := runGitWithoutHooks(
		ctx, commonDir, "config", "--file",
		filepath.Join(strings.TrimSpace(gitDir), "config.worktree"),
		"core.bare", "false",
	); err != nil {
		return fmt.Errorf("configure linked worktree: %w", err)
	}
	return nil
}

func (m *Manager) runOwnedGitWorktreeAdd(
	ctx context.Context, dir string, ws *Workspace, args ...string,
) error {
	if err := runGitWorktreeAdd(ctx, dir, ws.WorktreePath, args...); err != nil {
		return err
	}
	if err := writeWorkspaceOwnershipMarker(ctx, dir, ws); err != nil {
		markerErr := fmt.Errorf("%w: %w", errWorkspaceOwnershipMarker, err)
		cleanupErr := cleanupUnmarkedWorktreeAdd(ctx, dir, ws, "", "")
		if cleanupErr != nil {
			return errors.Join(
				markerErr,
				fmt.Errorf("roll back unmarked worktree: %w", cleanupErr),
			)
		}
		return markerErr
	}
	return nil
}

func runGitWorktreeAddCreatingBranch(
	ctx context.Context, dir, worktreePath, branch, startRef string,
) error {
	_, err := createBranchAndAddWorktree(
		ctx, dir, worktreePath, branch, startRef,
	)
	return err
}

func (m *Manager) runOwnedGitWorktreeAddCreatingBranch(
	ctx context.Context,
	dir string,
	ws *Workspace,
	branch, startRef string,
) (string, error) {
	branchSHA, err := createBranchAndAddWorktree(
		ctx, dir, ws.WorktreePath, branch, startRef,
	)
	if err != nil {
		return "", err
	}
	if err := writeWorkspaceOwnershipMarker(ctx, dir, ws); err != nil {
		markerErr := fmt.Errorf("%w: %w", errWorkspaceOwnershipMarker, err)
		cleanupErr := cleanupUnmarkedWorktreeAdd(
			ctx, dir, ws, branch, branchSHA,
		)
		if cleanupErr != nil {
			return "", errors.Join(
				markerErr,
				fmt.Errorf("roll back unmarked worktree: %w", cleanupErr),
			)
		}
		return "", markerErr
	}
	return branchSHA, nil
}

func createBranchAndAddWorktree(
	ctx context.Context, dir, worktreePath, branch, startRef string,
) (string, error) {
	if err := validateLocalBranchName(ctx, dir, branch); err != nil {
		return "", err
	}
	startSHA, ok, err := gitRefSHA(ctx, dir, startRef)
	if err != nil {
		return "", fmt.Errorf("resolve worktree start ref %q: %w", startRef, err)
	}
	if !ok {
		return "", fmt.Errorf("worktree start ref %q not found", startRef)
	}
	branchRef := "refs/heads/" + branch
	zeroOID := strings.Repeat("0", len(startSHA))
	if err := runGitWithoutHooks(
		ctx, dir, "update-ref", branchRef, startSHA, zeroOID,
	); err != nil {
		return "", fmt.Errorf("create worktree branch %q: %w", branch, err)
	}
	addErr := runGitWorktreeAdd(
		ctx, dir, worktreePath, branch,
	)
	if addErr == nil {
		return startSHA, nil
	}
	if cleanupErr := deleteWorkspaceBranchIfMatches(
		ctx, dir, branch, startSHA,
	); cleanupErr != nil {
		return "", errors.Join(
			addErr,
			fmt.Errorf("clean up failed worktree branch: %w", cleanupErr),
		)
	}
	return "", addErr
}

func deleteWorkspaceBranchIfMatches(
	ctx context.Context, dir, branch, expectedSHA string,
) error {
	if strings.TrimSpace(expectedSHA) == "" {
		return errors.New("expected branch SHA is required")
	}
	if err := validateLocalBranchName(ctx, dir, branch); err != nil {
		return err
	}
	if err := runGitWithoutHooks(
		ctx, dir,
		"update-ref", "-d", "refs/heads/"+branch, expectedSHA,
	); err != nil {
		return fmt.Errorf("delete git branch %q if unchanged: %w", branch, err)
	}
	return nil
}

// runBuiltCmd runs a pre-built exec.Cmd and wraps any failure with
// the combined output. Used for tmux invocations whose *exec.Cmd is
// assembled by tmuxExec so argv[0] access stays inside that helper.
func runBuiltCmd(ctx context.Context, cmd *exec.Cmd) error {
	out, err := procutil.CombinedOutput(
		ctx, cmd, "tmux subprocess capacity",
	)
	if err != nil {
		return fmt.Errorf(
			"%w: %s", err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// dirtyFiles returns the list of uncommitted files in a worktree.
func dirtyFiles(
	ctx context.Context, worktreePath string,
) ([]string, error) {
	cmd := workspaceGitCommand(
		ctx, "", "-C", worktreePath, "status", "--porcelain",
	)
	out, err := procutil.Output(
		ctx, cmd, "git subprocess capacity",
	)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var files []string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			// porcelain format: "XY filename"
			if len(line) > 3 {
				files = append(files, line[3:])
			} else {
				files = append(files, line)
			}
		}
	}
	return files, nil
}

func (m *Manager) setErrorWithContext(
	ctx context.Context, id string, origErr error,
) {
	msg := origErr.Error()
	if err := m.updateWorkspaceStatusWithContext(
		ctx, id, "error", &msg,
	); err != nil {
		slog.Error("failed to set workspace error status",
			"workspace_id", id, "err", err)
	}
}

func (m *Manager) recordSetupEvent(
	ctx context.Context,
	workspaceID, stage, outcome, message string,
) {
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	m.recordSetupEventWithContext(
		persistCtx, workspaceID, stage, outcome, message,
	)
}

func (m *Manager) recordSetupEventWithContext(
	ctx context.Context,
	workspaceID, stage, outcome, message string,
) {
	err := m.db.InsertWorkspaceSetupEvent(
		ctx,
		&db.WorkspaceSetupEvent{
			WorkspaceID: workspaceID,
			Stage:       stage,
			Outcome:     outcome,
			Message:     message,
		},
	)
	if err != nil {
		slog.Warn("workspace setup audit insert failed",
			"workspace_id", workspaceID,
			"stage", stage,
			"outcome", outcome,
			"err", err,
		)
	}
}

func (m *Manager) failSetup(
	ctx context.Context,
	workspaceID, stage string, origErr error,
) error {
	wrapped := wrapWorkspaceSetupError(stage, origErr)
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	m.recordSetupEventWithContext(
		persistCtx, workspaceID, stage, "failure", wrapped.Error(),
	)
	slog.Error("workspace setup failed",
		"workspace_id", workspaceID,
		"stage", stage,
		"err", wrapped,
	)
	m.setErrorWithContext(persistCtx, workspaceID, wrapped)
	return wrapped
}

func wrapWorkspaceSetupError(stage string, err error) error {
	if procutil.IsResourceExhausted(err) {
		switch stage {
		case workspaceSetupStageClone:
			return fmt.Errorf(
				"ensure clone: host process limit reached while starting git or helper processes: %w",
				err,
			)
		case workspaceSetupStageWorktree:
			return fmt.Errorf(
				"add git worktree: host process limit reached while starting git or helper processes: %w",
				err,
			)
		case workspaceSetupStageTmuxSession:
			return fmt.Errorf(
				"tmux new-session: host process limit reached while starting tmux or shell: %w",
				err,
			)
		}
	}
	switch stage {
	case workspaceSetupStageClone:
		return fmt.Errorf("ensure clone: %w", err)
	case workspaceSetupStageWorktree:
		return fmt.Errorf("add git worktree: %w", err)
	case workspaceSetupStageTmuxSession:
		return fmt.Errorf("tmux new-session: %w", err)
	default:
		return err
	}
}

// rollbackWorktree removes a partially created worktree and its
// branch under the per-repo lock.
func (m *Manager) rollbackWorktree(
	ctx context.Context, cloneDir string, ws *Workspace,
	branch string,
) {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	err := m.withRepoLockForGitDir(cleanupCtx, cloneDir, func() error {
		owned, err := workspaceRegistrationMatches(
			cleanupCtx, cloneDir, ws.WorktreePath, ws.ID,
		)
		if err != nil {
			return fmt.Errorf("verify rollback worktree ownership: %w", err)
		}
		if !owned {
			slog.Warn("rollback: preserved worktree without matching ownership marker",
				"workspace_id", ws.ID, "path", ws.WorktreePath)
			return nil
		}
		if err := runGitWithoutHooks(
			cleanupCtx, cloneDir,
			"worktree", "remove", "--force", ws.WorktreePath,
		); err != nil {
			if isGitWorktreeAbsent(err) {
				if staleErr := removeStaleWorktreeRegistrationMetadata(
					cleanupCtx, cloneDir, ws.WorktreePath,
				); staleErr == nil {
					m.deleteWorkspaceBranches(cleanupCtx, cloneDir, ws, branch)
					return nil
				}
			}
			slog.Warn("rollback: worktree remove failed",
				"path", ws.WorktreePath, "err", err)
			return nil
		}
		m.deleteWorkspaceBranches(cleanupCtx, cloneDir, ws, branch)
		return nil
	})
	if err != nil {
		slog.Warn("rollback: acquire worktree lock failed",
			"path", cloneDir, "err", err)
	}
}

func (m *Manager) deleteWorkspaceBranches(
	ctx context.Context, cloneDir string, ws *Workspace,
	managedBranch string,
) {
	for _, branch := range workspaceBranchCandidates(ws, managedBranch) {
		if err := validateLocalBranchName(
			ctx, cloneDir, branch,
		); err != nil {
			slog.Warn("workspace branch delete skipped",
				"branch", branch, "err", err)
			continue
		}
		if err := runGitWithoutHooks(
			ctx, cloneDir, "branch", "-D", "--", branch,
		); err != nil {
			slog.Warn("workspace branch delete failed",
				"branch", branch, "err", err)
		}
	}
}

func (m *Manager) deleteWorkspaceBranchesStrict(
	ctx context.Context, cloneDir string, ws *Workspace,
	managedBranch string,
) error {
	for _, branch := range workspaceBranchCandidates(ws, managedBranch) {
		if err := deleteWorkspaceBranchStrict(
			ctx, cloneDir, branch,
		); err != nil {
			return err
		}
	}
	return nil
}

func deleteWorkspaceBranchStrict(
	ctx context.Context, cloneDir string, branch string,
) error {
	if err := validateLocalBranchName(
		ctx, cloneDir, branch,
	); err != nil {
		return err
	}
	if err := runGitWithoutHooks(
		ctx, cloneDir, "branch", "-D", "--", branch,
	); err != nil && !isGitBranchAbsent(err) {
		return fmt.Errorf("delete git branch %q: %w", branch, err)
	}
	return nil
}

func isGitWorktreeAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not a working tree") ||
		strings.Contains(msg, "is not a worktree") ||
		strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "no such file or directory") ||
		// A worktree whose .git gitfile was left empty or partial by
		// an interrupted "git worktree add" is unusable: rev-parse
		// reports "invalid gitfile format" and "worktree remove"
		// reports "is not a .git file". Treat both as absent so
		// cleanup skips the dead worktree instead of failing.
		strings.Contains(msg, "invalid gitfile format") ||
		strings.Contains(msg, "is not a .git file") ||
		// A linked worktree can disappear between Git opening its
		// metadata directory and reading commondir. Git reports this
		// race with a misleading "Success" suffix.
		(strings.Contains(msg, "failed to read worktrees/") &&
			strings.Contains(msg, "/commondir: success"))
}

func isGitBranchAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "branch") &&
		strings.Contains(msg, "not found")
}

func gitCloneDirReady(cloneDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(cloneDir, "HEAD"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat git clone dir: %w", err)
}

func isUniqueConstraintError(err error) bool {
	type sqliteCoder interface {
		Code() int
	}
	var coder sqliteCoder
	if !errors.As(err, &coder) {
		return false
	}
	const sqliteConstraintUnique = 2067
	return coder.Code() == sqliteConstraintUnique
}

func workspaceBranchCandidates(
	ws *Workspace, managedBranch string,
) []string {
	if managedBranch == workspaceBranchUnknown {
		if workspaceUsesOriginHead(ws) {
			// Trust the persisted branch. The bare-form fallback
			// only applies when GitHeadRef is empty (pre-feature
			// workspaces); a slug-style workspace's bare-form
			// branch may be a user-owned local branch that
			// kenn-forge never created, so cleanup must not delete
			// it as a candidate.
			if ws.GitHeadRef != "" {
				return []string{ws.GitHeadRef}
			}
			if ws.ItemType == db.WorkspaceItemTypeKataTask {
				return nil
			}
			return []string{issueWorkspaceBranch(ws.ItemNumber)}
		}
		return []string{syntheticPRWorktreeBranch(ws.ItemNumber)}
	}
	if managedBranch == "" {
		return nil
	}
	return []string{managedBranch}
}

func (m *Manager) persistenceContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	return boundedDetachedContext(ctx, workspacePersistTimeout)
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundedDetachedContext(ctx, workspaceCleanupTimeout)
}

func boundedDetachedContext(
	ctx context.Context, timeout time.Duration,
) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) <= timeout {
			return context.WithDeadline(base, deadline)
		}
	}
	return context.WithTimeout(base, timeout)
}

func (m *Manager) updateWorkspaceStatus(
	ctx context.Context, id, status string, errMsg *string,
) error {
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	return m.updateWorkspaceStatusWithContext(
		persistCtx, id, status, errMsg,
	)
}

func (m *Manager) updateWorkspaceStatusWithContext(
	ctx context.Context, id, status string, errMsg *string,
) error {
	return m.db.UpdateWorkspaceStatus(
		ctx, id, status, errMsg,
	)
}

func (m *Manager) updateWorkspaceBranch(
	ctx context.Context, id, branch string,
) error {
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	return m.db.UpdateWorkspaceBranch(
		persistCtx, id, branch,
	)
}

func (m *Manager) completeRecoveredWorkspaceSetup(
	ctx context.Context, id, branch string,
) error {
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	return m.db.CompleteRecoveredWorkspaceSetup(persistCtx, id, branch)
}

func gitRefSHA(
	ctx context.Context, dir, ref string,
) (string, bool, error) {
	out, err := gitCombinedOutput(
		ctx, dir, "rev-parse", "--verify", "--quiet",
		ref+"^{commit}",
	)
	if err == nil {
		return strings.TrimSpace(out), true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", false, nil
	}
	return "", false, err
}

func worktreeCommonGitDir(
	ctx context.Context, worktreePath string,
) (string, error) {
	out, err := gitCombinedOutput(
		ctx, worktreePath,
		"rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func worktreePathIsRoot(
	ctx context.Context, worktreePath string,
) (bool, error) {
	insideWorktree, err := gitCombinedOutput(
		ctx, worktreePath, "rev-parse", "--is-inside-work-tree",
	)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(insideWorktree) != "true" {
		return false, nil
	}
	out, err := gitCombinedOutput(
		ctx, worktreePath,
		"rev-parse", "--path-format=absolute", "--show-toplevel",
	)
	if err != nil {
		return false, err
	}
	root, err := canonicalFilesystemPath(strings.TrimSpace(out))
	if err != nil {
		return false, err
	}
	candidate, err := canonicalFilesystemPath(worktreePath)
	if err != nil {
		return false, err
	}
	return root == candidate, nil
}

func gitDirTracksWorktreePath(
	ctx context.Context, gitDir, worktreePath string,
) (bool, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return false, nil
	}
	want, err := canonicalWorktreeListPath(worktreePath)
	if err != nil {
		return false, fmt.Errorf("resolve workspace path: %w", err)
	}
	out, err := gitCombinedOutput(
		ctx, gitDir, "worktree", "list", "--porcelain",
	)
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		got, err := canonicalWorktreeListPath(strings.TrimSpace(path))
		if err != nil {
			return false, fmt.Errorf("resolve tracked worktree path: %w", err)
		}
		if got == want {
			return true, nil
		}
	}
	return false, nil
}

func canonicalWorktreeListPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	var suffix []string
	for {
		if evaluated, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{evaluated}, suffix...)...), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func gitIsBareRepository(ctx context.Context, dir string) (bool, error) {
	out, err := gitCombinedOutput(
		ctx, dir, "rev-parse", "--is-bare-repository",
	)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func localBranchExists(
	ctx context.Context, dir, branch string,
) (bool, error) {
	cmd := workspaceGitCommand(
		ctx,
		dir,
		"show-ref",
		"--verify",
		"--quiet",
		"refs/heads/"+branch,
	)
	err := procutil.Run(ctx, cmd, "git subprocess capacity")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func localBranchNameAvailable(
	ctx context.Context, dir, branch string,
) (bool, error) {
	available, _, err := localBranchNameStatus(ctx, dir, branch)
	return available, err
}

func localBranchNameStatus(
	ctx context.Context, dir, branch string,
) (available, ancestorConflict bool, err error) {
	out, err := gitCombinedOutput(
		ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads",
	)
	if err != nil {
		return false, false, err
	}
	want := "refs/heads/" + branch
	for ref := range strings.SplitSeq(out, "\n") {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(want, ref+"/") {
			return false, true, nil
		}
		if ref == want || strings.HasPrefix(ref, want+"/") {
			return false, false, nil
		}
	}
	return true, false, nil
}

func localBranchCheckedOut(
	ctx context.Context, dir, branch string,
) (bool, error) {
	out, err := gitCombinedOutput(
		ctx, dir, "worktree", "list", "--porcelain",
	)
	if err != nil {
		return false, err
	}
	want := "refs/heads/" + branch
	for line := range strings.SplitSeq(out, "\n") {
		got, ok := strings.CutPrefix(line, "branch ")
		if ok && strings.TrimSpace(got) == want {
			return true, nil
		}
	}
	return false, nil
}

func workspaceGitCommand(
	ctx context.Context, dir string, args ...string,
) *exec.Cmd {
	// Keep git process construction centralized so workspace mutations share
	// kit's automation defaults: no inherited GIT_* hook state, no global or
	// system config, and no terminal prompts. Callers remain responsible for
	// wrapping commands in procutil when they need the shared capacity guard.
	return gitcmd.New().Command(ctx, dir, args...)
}

func nextAvailableBranchName(
	ctx context.Context, dir, branch string,
) (string, error) {
	_, ancestorConflict, err := localBranchNameStatus(ctx, dir, branch)
	if err != nil {
		return "", err
	}
	for i := 2; i < 1000; i++ {
		for _, candidate := range numberedBranchCandidates(
			branch, i, ancestorConflict,
		) {
			available, err := localBranchNameAvailable(ctx, dir, candidate)
			if err != nil {
				return "", err
			}
			if available {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf(
		"could not find an available branch name derived from %q",
		branch,
	)
}

func nextAvailableAdHocBranchName(
	ctx context.Context,
	dir, branch, workspaceID string,
	startAttempt int,
) (string, int, error) {
	_, ancestorConflict, err := localBranchNameStatus(ctx, dir, branch)
	if err != nil {
		return "", startAttempt, err
	}
	for attempt := startAttempt; attempt < 1000; attempt++ {
		suffix := adHocBranchHash(workspaceID, attempt)
		for _, candidate := range suffixedBranchCandidates(
			branch, suffix, ancestorConflict,
		) {
			available, err := localBranchNameAvailable(ctx, dir, candidate)
			if err != nil {
				return "", attempt, err
			}
			if available {
				return candidate, attempt + 1, nil
			}
		}
	}
	return "", startAttempt, fmt.Errorf(
		"could not find an available branch name derived from %q",
		branch,
	)
}

func adHocBranchHash(workspaceID string, attempt int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d", workspaceID, attempt))
	return hex.EncodeToString(sum[:2])
}

func numberedBranchCandidates(
	branch string, number int, escapeAncestors bool,
) []string {
	return suffixedBranchCandidates(
		branch, strconv.Itoa(number), escapeAncestors,
	)
}

func suffixedBranchCandidates(
	branch, suffix string, escapeAncestors bool,
) []string {
	parts := strings.Split(branch, "/")
	candidates := make([]string, 0, len(parts))
	candidates = append(candidates, branch+"-"+suffix)
	if !escapeAncestors {
		return candidates
	}
	for i := len(parts) - 2; i >= 0; i-- {
		candidate := slices.Clone(parts)
		candidate[i] += "-" + suffix
		candidates = append(candidates, strings.Join(candidate, "/"))
	}
	return candidates
}
