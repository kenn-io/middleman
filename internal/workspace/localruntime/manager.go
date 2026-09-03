package localruntime

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/procutil"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/ptysize"
)

type SessionStatus string

const (
	SessionStatusStarting SessionStatus = "starting"
	SessionStatusRunning  SessionStatus = "running"
	SessionStatusExited   SessionStatus = "exited"
	SessionStatusError    SessionStatus = "error"
)

const initialMessageWriteTimeout = 30 * time.Second

// initialMessageEnterDelay separates the bracketed paste from the Enter
// keystroke that submits it. Terminal UIs that collapse a multi-line paste
// treat bytes arriving in the same chunk as the paste-end marker as part of
// the paste, so a carriage return in that chunk never submits the prompt.
const initialMessageEnterDelay = 150 * time.Millisecond

var (
	errManagerShutdown    = errors.New("runtime manager is shut down")
	ErrSessionNotFound    = errors.New("runtime session not found")
	ErrSessionUnavailable = errors.New(
		"runtime session is temporarily unavailable",
	)
	ErrBracketedPasteInactive   = errors.New("bracketed paste mode is not active")
	ErrInitialMessageNotWritten = errors.New(
		"initial message was not written",
	)
	errWorkspaceStopping = errors.New(
		"workspace is being stopped",
	)
)

type SessionInfo struct {
	Key           string           `json:"key"`
	WorkspaceID   string           `json:"workspace_id"`
	TargetKey     string           `json:"target_key"`
	Label         string           `json:"label"`
	Kind          LaunchTargetKind `json:"kind"`
	Status        SessionStatus    `json:"status"`
	DisplayRegion string           `json:"display_region"`
	CreatedAt     time.Time        `json:"created_at"`
	ExitedAt      *time.Time       `json:"exited_at,omitempty"`
	ExitCode      *int             `json:"exit_code,omitempty"`
	TmuxSession   string           `json:"-"`
	// tmuxLaunchID identifies the concrete tmux backend generation created for
	// this launch. It is empty when the manager reattached to a pre-existing
	// backend instead of creating one.
	tmuxLaunchID string
	// Reused reports that ensure semantics returned an already-live attachment
	// or reattached to a pre-existing backend. Callers whose post-launch
	// bookkeeping fails must not stop a session they did not create.
	Reused bool `json:"-"`
}

func (s SessionInfo) Compare(other SessionInfo) int {
	return s.CreatedAt.Compare(other.CreatedAt)
}

// SameGeneration reports whether two snapshots identify the same reusable
// session generation. Keys can be reused after an exit, so the key alone is
// not a generation identity.
func (s SessionInfo) SameGeneration(other SessionInfo) bool {
	return s.Key == other.Key && s.CreatedAt.Equal(other.CreatedAt)
}

type RestoredRuntimeSession struct {
	WorkspaceID string
	SessionKey  string
	TargetKey   string
	Label       string
	Kind        LaunchTargetKind
	TmuxSession string
	CWD         string
	CreatedAt   time.Time
}

type Options struct {
	Targets      []LaunchTarget
	ShellCommand []string
	TmuxCommand  []string
	// TmuxOwnerMarker tags tmux-backed agent sessions so workspace startup
	// cleanup can identify kenn-forge-owned runtime sessions that were created
	// before their durable DB row was written.
	TmuxOwnerMarker string
	// WrapAgentSessionsInTmux starts agent targets under tmux when
	// the tmux launch target is available. Other sessions are started
	// through PtyOwnerRuntime.
	WrapAgentSessionsInTmux bool
	// HideTmuxStatus turns off tmux's status line for newly-created
	// tmux-backed runtime sessions.
	HideTmuxStatus bool
	// TmuxMouse enables tmux mouse handling for tmux-backed runtime sessions.
	TmuxMouse bool
	// TmuxGraphics enables image passthrough for tmux-backed runtime sessions.
	TmuxGraphics bool
	// StripEnvVars names additional env vars to strip beyond the
	// built-in credential prefixes (e.g. a configured token env).
	StripEnvVars []string
	// OnSessionExit is called after a launched runtime session exits naturally
	// and is removed from the manager's active session map.
	OnSessionExit func(SessionInfo)
	// PtyOwnerRuntime starts non-tmux runtime PTYs through the durable PTY
	// owner so server restarts can detach and reconnect later.
	PtyOwnerRuntime ptyownerruntime.Owner
	// KnownPtyOwnerSessionKeys returns durable session keys that may no longer
	// have an in-memory attachment after a server restart.
	KnownPtyOwnerSessionKeys func(context.Context, string) ([]string, error)
	// DetachSessionsForServerRestart marks shutdown-driven attachment loss as
	// recoverable so terminal bridges do not report a natural process exit.
	// Normal shutdown behavior is unchanged unless the caller opts in.
	DetachSessionsForServerRestart bool
}

type Manager struct {
	mu                sync.Mutex
	targets           map[string]LaunchTarget
	targetsList       []LaunchTarget
	sessions          map[string]*session
	labelReservations map[string]map[string]int
	shellCommand      []string
	tmuxCommand       []string
	tmuxOwnerMarker   string
	wrapAgentsInTmux  bool
	hideTmuxStatus    bool
	tmuxMouse         bool
	tmuxGraphics      bool
	stripEnvVars      []string
	onSessionExit     func(SessionInfo)
	ptyOwnerRuntime   ptyownerruntime.Owner
	knownSessionKeys  func(context.Context, string) ([]string, error)
	detachForRestart  bool
	startLocks        map[string]*sync.Mutex
	stoppingWS        map[string]int
	inflightWS        map[string]int
	inflightCh        map[string]chan struct{}
	startWG           sync.WaitGroup
	closed            bool
}

// maxSessionOutputReplay caps how many bytes of recent PTY output
// the session retains for replay when a new subscriber attaches.
// Sized to comfortably hold an agent boot banner plus the first
// prompt — without it, a fast subscribe-after-launch flow can miss
// startup output entirely.
const maxSessionOutputReplay = 64 * 1024

// postExitPTYDrainTimeout bounds how long a naturally exited session can keep
// the PTY master open after cmd.Wait returns. Normal exits should reach PTY EOF
// immediately after the kernel buffer is drained; the timeout exists for the
// daemonized-child case where a descendant keeps the slave side open forever.
const postExitPTYDrainTimeout = 250 * time.Millisecond

var (
	alternateScreenEnterSequences = [][]byte{
		[]byte("\x1b[?47h"),
		[]byte("\x1b[?1047h"),
		[]byte("\x1b[?1049h"),
	}
	alternateScreenExitSequences = [][]byte{
		[]byte("\x1b[?47l"),
		[]byte("\x1b[?1047l"),
		[]byte("\x1b[?1049l"),
	}
	maxAlternateScreenSequenceLen = maxByteSliceLen(
		append(
			slices.Clone(alternateScreenEnterSequences),
			alternateScreenExitSequences...,
		),
	)
)

type session struct {
	mu sync.Mutex
	// inputMu serializes terminal input so an initial-message handoff owns
	// the PTY from its paste through its Enter keystroke.
	inputMu                   sync.Mutex
	info                      SessionInfo
	cmd                       *exec.Cmd
	ptmx                      *os.File
	pty                       ptyownerruntime.PTY
	lifecycle                 sessionLifecycle
	tmuxSession               string
	done                      chan struct{}
	outputDone                chan struct{}
	subscribers               map[chan []byte]struct{}
	outputBuffer              []byte
	outputClosed              bool
	alternateScreenActive     bool
	alternateScreenTail       []byte
	terminalSequenceTail      []byte
	inputModes                terminalInputModeState
	lifecycleMu               sync.Mutex
	lifecycleClosed           bool
	stopRequested             bool
	recoverableDetach         bool
	nextAttachmentID          uint64
	nextResizeClaim           uint64
	resizeOwnerID             uint64
	resizeOwnerPriority       ResizePriority
	resizeGeneration          uint64
	resizeSettledGeneration   uint64
	resizeAttachments         map[uint64]resizeAttachment
	replayBoundarySubscribers map[chan []byte]struct{}
}

type ResizePriority int

const (
	ResizePriorityRemote ResizePriority = iota + 1
	ResizePriorityLocal
)

type AttachSessionOptions struct {
	ResizePriority ResizePriority
	ResizeActive   bool
	ReplayBoundary bool
}

type resizeAttachment struct {
	priority ResizePriority
	active   bool
	claim    uint64
	geometry ptysize.Geometry
	hasSize  bool
}

type Attachment struct {
	Output <-chan []byte
	Done   <-chan struct{}

	info                 func() SessionInfo
	write                func([]byte) error
	submitInitialMessage func(context.Context, string) error
	resize               func(ptysize.Geometry) error
	claimResize          func(ptysize.Geometry) (bool, error)
	resizeSettled        func()
	refresh              func(context.Context) error
	setResizeActive      func(active bool)
	close                func()

	// sessionOutputClosed reports whether the underlying session's
	// PTY EOF has been observed by drainOutput (s.outputClosed=true).
	// The Output channel can also close because broadcast dropped
	// this subscriber for falling behind; that is not a session exit
	// and bridges must not propagate it as one.
	sessionOutputClosed func() bool
	recoverableDetach   func() bool
}

func NewManager(options Options) *Manager {
	targets := make(map[string]LaunchTarget, len(options.Targets))
	targetsList := make([]LaunchTarget, 0, len(options.Targets))
	for _, target := range options.Targets {
		cloned := cloneTarget(target)
		targets[target.Key] = cloned
		targetsList = append(targetsList, cloneTarget(cloned))
	}
	return &Manager{
		targets:           targets,
		targetsList:       targetsList,
		sessions:          make(map[string]*session),
		labelReservations: make(map[string]map[string]int),
		shellCommand:      slices.Clone(options.ShellCommand),
		tmuxCommand:       slices.Clone(options.TmuxCommand),
		tmuxOwnerMarker:   options.TmuxOwnerMarker,
		wrapAgentsInTmux:  options.WrapAgentSessionsInTmux,
		hideTmuxStatus:    options.HideTmuxStatus,
		tmuxMouse:         options.TmuxMouse,
		tmuxGraphics:      options.TmuxGraphics,
		stripEnvVars:      dedupeStrings(options.StripEnvVars),
		onSessionExit:     options.OnSessionExit,
		ptyOwnerRuntime:   options.PtyOwnerRuntime,
		knownSessionKeys:  options.KnownPtyOwnerSessionKeys,
		detachForRestart:  options.DetachSessionsForServerRestart,
		startLocks:        make(map[string]*sync.Mutex),
		stoppingWS:        make(map[string]int),
		inflightWS:        make(map[string]int),
		inflightCh:        make(map[string]chan struct{}),
	}
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func maxByteSliceLen(slices [][]byte) int {
	maxLen := 0
	for _, slice := range slices {
		if len(slice) > maxLen {
			maxLen = len(slice)
		}
	}
	return maxLen
}

func (m *Manager) Launch(
	ctx context.Context,
	workspaceID string,
	cwd string,
	targetKey string,
) (SessionInfo, error) {
	slog.Debug(
		"runtime launch requested",
		"workspace_id", workspaceID,
		"target_key", targetKey,
		"cwd", cwd,
	)
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}

	target, err := m.target(targetKey)
	if err != nil {
		return SessionInfo{}, err
	}
	if !target.Available {
		reason := target.DisabledReason
		if reason == "" {
			reason = "target not available"
		}
		return SessionInfo{}, fmt.Errorf(
			"target %q not available: %s", targetKey, reason,
		)
	}
	if target.Kind == LaunchTargetPlainShell && len(target.Command) == 0 {
		target.Command = slices.Clone(m.shellCommand)
		if len(target.Command) == 0 {
			target.Command = defaultShellCommand()
		}
	}
	if len(target.Command) == 0 || target.Command[0] == "" {
		return SessionInfo{}, fmt.Errorf(
			"target %q has no command", targetKey,
		)
	}

	if err := m.ensureOpen(); err != nil {
		return SessionInfo{}, err
	}
	if err := m.beginStart(); err != nil {
		return SessionInfo{}, err
	}
	defer m.finishStart()

	if err := m.claimInflight(workspaceID); err != nil {
		return SessionInfo{}, err
	}
	defer m.releaseInflight(workspaceID)

	key, err := m.newSessionKey(workspaceID)
	if err != nil {
		return SessionInfo{}, err
	}
	label, releaseLabel := m.reserveSessionLabel(
		workspaceID,
		fallbackSessionLabel(target.Label, targetKey),
	)
	defer releaseLabel()

	launch, err := m.launchCommand(ctx, target, workspaceID, key, cwd)
	if err != nil {
		slog.Debug(
			"runtime launch command failed",
			"workspace_id", workspaceID,
			"session_key", key,
			"target_key", targetKey,
			"err", err,
		)
		return SessionInfo{}, err
	}
	slog.Debug(
		"runtime launch starting session",
		"workspace_id", workspaceID,
		"session_key", key,
		"target_key", targetKey,
		"kind", target.Kind,
		"tmux_session", launch.TmuxSession,
	)

	started, err := m.startOwnedSession(ctx, SessionInfo{
		Key:          key,
		WorkspaceID:  workspaceID,
		TargetKey:    targetKey,
		Label:        label,
		Kind:         target.Kind,
		Status:       SessionStatusStarting,
		CreatedAt:    time.Now().UTC(),
		TmuxSession:  launch.TmuxSession,
		tmuxLaunchID: launch.TmuxLaunchID,
		Reused:       launch.TmuxSession != "" && !launch.TmuxCreated,
	}, launch.Command, cwd, m.currentStripEnvVars())
	if err != nil {
		if launch.TmuxCreated {
			cleanupCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), 5*time.Second,
			)
			_ = m.killTmuxSession(cleanupCtx, launch.TmuxSession)
			cancel()
		}
		slog.Debug(
			"runtime launch start failed",
			"workspace_id", workspaceID,
			"session_key", key,
			"target_key", targetKey,
			"err", err,
		)
		return SessionInfo{}, err
	}
	started.tmuxSession = launch.TmuxSession

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go m.watchSession(started)
		_ = m.stopSession(ctx, started)
		waitSessionDone(started)
		slog.Debug(
			"runtime launch discarded session after shutdown",
			"workspace_id", workspaceID,
			"session_key", key,
		)
		return SessionInfo{}, errManagerShutdown
	}
	m.sessions[key] = started
	m.mu.Unlock()
	go m.watchSession(started)
	slog.Debug(
		"runtime launch session stored",
		"workspace_id", workspaceID,
		"session_key", key,
		"target_key", targetKey,
	)

	return started.snapshot(), nil
}

func (m *Manager) RenameSession(
	workspaceID string,
	sessionKey string,
	label string,
) (SessionInfo, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return SessionInfo{}, errors.New("session label is required")
	}

	m.mu.Lock()
	s := m.sessions[sessionKey]
	m.mu.Unlock()
	if s == nil {
		return SessionInfo{}, ErrSessionNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.WorkspaceID != workspaceID {
		return SessionInfo{}, ErrSessionNotFound
	}
	s.info.Label = label
	info := s.info
	info.TmuxSession = s.tmuxSession
	if s.info.ExitedAt != nil {
		exitedAt := *s.info.ExitedAt
		info.ExitedAt = &exitedAt
	}
	if s.info.ExitCode != nil {
		exitCode := *s.info.ExitCode
		info.ExitCode = &exitCode
	}
	return info, nil
}

func (m *Manager) RestoreRuntimeSessions(
	ctx context.Context,
	sessions []RestoredRuntimeSession,
) error {
	for _, restored := range sessions {
		if err := m.restoreRuntimeSession(ctx, restored); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) restoreRuntimeSession(
	ctx context.Context,
	restored RestoredRuntimeSession,
) error {
	workspaceID := strings.TrimSpace(restored.WorkspaceID)
	targetKey := strings.TrimSpace(restored.TargetKey)
	tmuxSession := strings.TrimSpace(restored.TmuxSession)
	if workspaceID == "" || targetKey == "" {
		return nil
	}

	key := strings.TrimSpace(restored.SessionKey)
	if key == "" {
		return nil
	}
	startMu := m.startLock(key)
	startMu.Lock()
	defer startMu.Unlock()

	if err := m.ensureOpen(); err != nil {
		return err
	}
	if existing := m.runningSession(m.sessions, key); existing != nil {
		slog.Debug(
			"runtime tmux restore reused existing session",
			"workspace_id", workspaceID,
			"session_key", key,
			"target_key", targetKey,
			"tmux_session", tmuxSession,
		)
		return nil
	}
	if tmuxSession == "" && m.ptyOwnerRuntime == nil {
		return fmt.Errorf(
			"%w: %q: pty owner runtime unavailable",
			ErrSessionUnavailable, key,
		)
	} else if tmuxSession == "" && !m.ptyOwnerRuntime.HasState(key) {
		return fmt.Errorf(
			"%w: %q: pty owner state missing",
			ErrSessionUnavailable, key,
		)
	}

	target, err := m.target(targetKey)
	if err != nil {
		target = LaunchTarget{
			Key:    targetKey,
			Label:  targetKey,
			Kind:   LaunchTargetAgent,
			Source: "stored",
		}
	}
	if target.Label == "" {
		target.Label = targetKey
	}
	if target.Kind == "" {
		target.Kind = LaunchTargetAgent
	}
	if restored.Label != "" {
		target.Label = restored.Label
	}
	if restored.Kind != "" {
		target.Kind = restored.Kind
	}
	if targetKey == string(LaunchTargetPlainShell) || target.Kind == LaunchTargetPlainShell {
		label := restored.Label
		if label == "" {
			label = "Shell"
		}
		target = LaunchTarget{
			Key:    string(LaunchTargetPlainShell),
			Label:  label,
			Kind:   LaunchTargetPlainShell,
			Source: "stored",
		}
	}

	createdAt := restored.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	if err := m.beginStart(); err != nil {
		return err
	}
	defer m.finishStart()

	slog.Debug(
		"runtime tmux restore starting attach",
		"workspace_id", workspaceID,
		"session_key", key,
		"target_key", targetKey,
		"tmux_session", tmuxSession,
	)
	info := SessionInfo{
		Key:         key,
		WorkspaceID: workspaceID,
		TargetKey:   targetKey,
		Label:       target.Label,
		Kind:        target.Kind,
		Status:      SessionStatusStarting,
		CreatedAt:   createdAt,
		TmuxSession: tmuxSession,
	}
	var started *session
	if tmuxSession == "" {
		started, err = attachPtyOwnerSession(ctx, m.ptyOwnerRuntime, info)
	} else {
		command, commandErr := m.restoredRuntimeCommand(restored)
		if commandErr != nil {
			return commandErr
		}
		started, err = m.startOwnedSession(
			ctx, info, command, "", m.currentStripEnvVars(),
		)
	}
	if err != nil {
		if tmuxSession != "" && isTmuxCommandUnavailable(err) {
			return fmt.Errorf(
				"%w: restored tmux attach unavailable for %q: %v",
				ErrSessionUnavailable, key, err,
			)
		}
		return err
	}
	started.tmuxSession = tmuxSession

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go m.watchSession(started)
		_ = m.stopSession(ctx, started)
		waitSessionDone(started)
		return errManagerShutdown
	}
	m.sessions[key] = started
	m.mu.Unlock()
	// startSession already starts drainOutput; restored tmux attach
	// sessions only need the process watcher here.
	go m.watchSession(started)
	slog.Debug(
		"runtime tmux restore session stored",
		"workspace_id", workspaceID,
		"session_key", key,
		"target_key", targetKey,
		"tmux_session", tmuxSession,
	)
	return nil
}

func (m *Manager) restoredRuntimeCommand(
	restored RestoredRuntimeSession,
) ([]string, error) {
	if restored.TmuxSession == "" {
		return nil, errors.New("restored command requires a tmux session")
	}
	command := slices.Clone(m.tmuxCommand)
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	return tmuxAttachSessionCommand(command, restored.TmuxSession), nil
}

func isTmuxCommandUnavailable(err error) bool {
	return procutil.IsResourceExhausted(err) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, os.ErrPermission)
}

func (m *Manager) LaunchTargets() []LaunchTarget {
	m.mu.Lock()
	defer m.mu.Unlock()

	targets := make([]LaunchTarget, 0, len(m.targetsList))
	for _, target := range m.targetsList {
		if target.Kind == LaunchTargetShell {
			continue
		}
		targets = append(targets, cloneTarget(target))
	}
	return targets
}

func (m *Manager) UpdateTargets(targets []LaunchTarget) {
	next, nextList := cloneLaunchTargetSet(targets)

	m.mu.Lock()
	m.targets = next
	m.targetsList = nextList
	m.mu.Unlock()
}

func (m *Manager) UpdateTargetsAndStripEnvVars(
	targets []LaunchTarget,
	names []string,
) {
	next, nextList := cloneLaunchTargetSet(targets)

	m.mu.Lock()
	m.targets = next
	m.targetsList = nextList
	m.stripEnvVars = dedupeStrings(append(slices.Clone(m.stripEnvVars), names...))
	m.mu.Unlock()
}

func (m *Manager) UpdateHideTmuxStatus(hide bool) {
	m.mu.Lock()
	m.hideTmuxStatus = hide
	m.mu.Unlock()
}

func (m *Manager) UpdateTmuxMouse(enabled bool) {
	m.mu.Lock()
	m.tmuxMouse = enabled
	m.mu.Unlock()
}

func (m *Manager) UpdateTmuxGraphics(enabled bool) {
	m.mu.Lock()
	m.tmuxGraphics = enabled
	m.mu.Unlock()
}

// ReattachTmuxClients replaces retained tmux attach clients so their browser
// connections restore them with the dedicated server's current terminal
// features. The tmux sessions and their pane processes remain running.
func (m *Manager) ReattachTmuxClients(ctx context.Context) error {
	m.mu.Lock()
	if !m.tmuxGraphics || !config.IsDefaultTmuxCommand(m.tmuxCommand) {
		m.mu.Unlock()
		return nil
	}
	targets := make(map[string]*session)
	for key, current := range m.sessions {
		if current.tmuxSession == "" {
			continue
		}
		targets[key] = current
	}
	m.mu.Unlock()

	if err := m.beginStart(); err != nil {
		return err
	}
	defer m.finishStart()

	var errs []error
	for key, current := range targets {
		startMu := m.startLock(key)
		startMu.Lock()

		m.mu.Lock()
		if m.sessions[key] != current {
			m.mu.Unlock()
			startMu.Unlock()
			continue
		}
		m.mu.Unlock()

		info := current.snapshot()
		command, err := m.restoredRuntimeCommand(RestoredRuntimeSession{
			TmuxSession: current.tmuxSession,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("reattach %q: %w", key, err))
			startMu.Unlock()
			continue
		}
		replacement, err := m.startOwnedSession(
			ctx, info, command, "", m.currentStripEnvVars(),
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("reattach %q: %w", key, err))
			startMu.Unlock()
			continue
		}
		replacement.tmuxSession = current.tmuxSession

		m.mu.Lock()
		if m.closed || m.sessions[key] != current {
			m.mu.Unlock()
			go m.watchSession(replacement)
			replacement.markRecoverableDetach()
			replacement.detach()
			startMu.Unlock()
			continue
		}
		m.sessions[key] = replacement
		m.mu.Unlock()
		go m.watchSession(replacement)
		startMu.Unlock()

		current.markRecoverableDetach()
		current.detach()
	}
	return errors.Join(errs...)
}

func cloneLaunchTargetSet(
	targets []LaunchTarget,
) (map[string]LaunchTarget, []LaunchTarget) {
	next := make(map[string]LaunchTarget, len(targets))
	nextList := make([]LaunchTarget, 0, len(targets))
	for _, target := range targets {
		cloned := cloneTarget(target)
		next[target.Key] = cloned
		nextList = append(nextList, cloneTarget(cloned))
	}
	return next, nextList
}

func (m *Manager) UpdateStripEnvVars(names []string) {
	// Non-secret terminal variables are never credentials; see
	// workspace.Manager.UpdateTmuxStripEnvVars.
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || config.IsTmuxNonSecretEnvVar(name) {
			continue
		}
		filtered = append(filtered, name)
	}
	m.mu.Lock()
	m.stripEnvVars = dedupeStrings(append(slices.Clone(m.stripEnvVars), filtered...))
	m.mu.Unlock()
}

func (m *Manager) currentStripEnvVars() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.stripEnvVars)
}

func (m *Manager) currentHideTmuxStatus() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hideTmuxStatus
}

func (m *Manager) currentTmuxMouse() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tmuxMouse
}

func (m *Manager) currentTmuxGraphics() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tmuxGraphics
}

func (m *Manager) ListSessions(workspaceID string) []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessions := make([]SessionInfo, 0)
	for _, s := range m.sessions {
		info := s.snapshot()
		if info.WorkspaceID == workspaceID {
			sessions = append(sessions, info)
		}
	}
	slices.SortFunc(sessions, SessionInfo.Compare)
	return sessions
}

// TmuxSessions returns runtime-owned tmux sessions associated with
// a workspace. These sessions are additional activity sources for
// the workspace sidebar; the persisted workspace tmux session remains
// owned by internal/workspace.Manager.
func (m *Manager) TmuxSessions(workspaceID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessions := make([]string, 0)
	for _, s := range m.sessions {
		info := s.snapshot()
		if info.WorkspaceID == workspaceID && s.tmuxSession != "" {
			sessions = append(sessions, s.tmuxSession)
		}
	}
	slices.Sort(sessions)
	return sessions
}

func (m *Manager) Stop(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
) error {
	s, ok := m.session(workspaceID, sessionKey)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionKey)
	}

	cleanupErr := m.stopSession(ctx, s)
	if cleanupErr != nil {
		if s.snapshot().TmuxSession != "" {
			m.removeIfSame(workspaceID, sessionKey, s)
		}
		return cleanupErr
	}
	m.removeIfSame(workspaceID, sessionKey, s)
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RollbackLaunch stops the owned backend created by a launch whose caller
// failed to persist its metadata. A non-tmux backend can be stopped only while
// its live manager entry proves ownership. A short-lived tmux attachment can
// disappear from the manager before persistence returns, so tmux rollback falls
// back to the generation marker retained in the launch result.
func (m *Manager) RollbackLaunch(ctx context.Context, info SessionInfo) error {
	if info.Reused {
		return nil
	}
	startMu := m.startLock(info.Key)
	startMu.Lock()
	defer startMu.Unlock()
	return m.rollbackLaunchLocked(ctx, info)
}

// rollbackLaunchLocked requires the caller to own startLock(info.Key) and the
// launch result to represent a newly owned backend.
func (m *Manager) rollbackLaunchLocked(ctx context.Context, info SessionInfo) error {
	if info.TmuxSession == "" {
		stopErr := m.Stop(ctx, info.WorkspaceID, info.Key)
		if errors.Is(stopErr, ErrSessionNotFound) {
			return nil
		}
		return stopErr
	}
	if info.tmuxLaunchID == "" {
		return nil
	}

	if current, ok := m.session(info.WorkspaceID, info.Key); ok &&
		current.snapshot().tmuxLaunchID != info.tmuxLaunchID {
		return nil
	}

	stopErr := m.Stop(ctx, info.WorkspaceID, info.Key)
	if errors.Is(stopErr, ErrSessionNotFound) {
		stopErr = nil
	}
	// Stop normally removes the tmux backend itself. Repeat cleanup only when
	// the retained launch marker still identifies this concrete generation, so
	// an older rollback cannot kill a newer replacement using the same name.
	tmuxErr := m.killTmuxSessionIfLaunchID(
		ctx, info.TmuxSession, info.tmuxLaunchID,
	)
	return errors.Join(stopErr, tmuxErr)
}

// Detach removes an in-memory runtime session attachment without stopping the
// owned backend.
func (m *Manager) Detach(workspaceID string, sessionKey string) error {
	s, ok := m.session(workspaceID, sessionKey)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionKey)
	}
	m.removeIfSame(workspaceID, sessionKey, s)
	s.detach()
	return nil
}

// StopWorkspace stops every running runtime session that
// belongs to workspaceID. It is intended to be called when a
// workspace is deleted so launched processes do not survive the
// worktree they were started in. The marker it takes internally is
// released on return; callers that go on to delete the workspace's
// backing rows must hold their own BeginStopping/EndStopping pair
// across the whole destructive flow.
func (m *Manager) StopWorkspace(
	ctx context.Context,
	workspaceID string,
) {
	// 1. Mark the workspace as stopping under the manager mutex.
	//    New Launch calls that observe this marker bail
	//    out via claimInflight before spawning a process.
	m.BeginStopping(workspaceID)
	defer m.EndStopping(workspaceID)

	// 2. Drain any Launch calls that passed claimInflight
	//    before the marker was set. They are mid-startSession; once
	//    they finish, their processes are in m.sessions and
	//    will be picked up by the snapshot below. Without this drain
	//    a launch in flight at step 1 can insert after the snapshot
	//    and leave a process alive for the deleted worktree.
	if err := m.waitInflight(ctx, workspaceID); err != nil {
		return
	}

	// 3. Snapshot and remove all sessions for the workspace.
	m.mu.Lock()
	stopping := make([]*session, 0)
	for key, s := range m.sessions {
		if s.snapshot().WorkspaceID == workspaceID {
			delete(m.sessions, key)
			stopping = append(stopping, s)
		}
	}
	m.mu.Unlock()

	for _, s := range stopping {
		if err := m.stopSession(ctx, s); err != nil {
			slog.Warn(
				"stop workspace runtime session",
				"workspace_id", workspaceID,
				"session_key", s.snapshot().Key,
				"err", err,
			)
		}
	}
	m.stopKnownPtyOwnerSessions(ctx, workspaceID, stopping)
	for _, s := range stopping {
		select {
		case <-s.done:
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) stopSession(ctx context.Context, s *session) error {
	if s == nil {
		return nil
	}
	s.markStopRequested()
	if s.tmuxSession != "" {
		if err := m.killTmuxSession(ctx, s.tmuxSession); err != nil {
			_ = s.stop(ctx)
			return fmt.Errorf(
				"kill tmux session %q: %w", s.tmuxSession, err,
			)
		}
	}
	return s.stop(ctx)
}

func (m *Manager) stopKnownPtyOwnerSessions(
	ctx context.Context,
	workspaceID string,
	stopping []*session,
) {
	if m.ptyOwnerRuntime == nil {
		return
	}
	stopped := make(map[string]struct{}, len(stopping))
	for _, s := range stopping {
		if s == nil {
			continue
		}
		stopped[s.snapshot().Key] = struct{}{}
	}
	for _, key := range m.knownPtyOwnerSessionKeys(workspaceID) {
		if _, ok := stopped[key]; ok {
			continue
		}
		if err := m.ptyOwnerRuntime.Stop(ctx, key); err != nil {
			slog.Warn(
				"stop stored ptyowner runtime session",
				"workspace_id", workspaceID,
				"session_key", key,
				"err", err,
			)
		}
	}
}

func (m *Manager) knownPtyOwnerSessionKeys(workspaceID string) []string {
	m.mu.Lock()
	knownSessionKeys := m.knownSessionKeys
	m.mu.Unlock()

	var keys []string
	if knownSessionKeys != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		stored, err := knownSessionKeys(ctx, workspaceID)
		cancel()
		if err != nil {
			slog.Warn(
				"list stored ptyowner runtime sessions",
				"workspace_id", workspaceID,
				"err", err,
			)
		}
		keys = append(keys, stored...)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func (m *Manager) killTmuxSessionIfLaunchID(
	ctx context.Context,
	session string,
	launchID string,
) error {
	actual, exists, err := m.tmuxSessionLaunchID(ctx, session)
	if err != nil {
		return err
	}
	if !exists || actual != launchID {
		return nil
	}
	return m.killTmuxSession(ctx, session)
}

func (m *Manager) tmuxSessionLaunchID(
	ctx context.Context,
	session string,
) (string, bool, error) {
	if session == "" {
		return "", false, nil
	}
	command := slices.Clone(m.tmuxCommand)
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	if len(command) == 0 || command[0] == "" {
		return "", false, nil
	}
	var err error
	command, err = resolveTmuxCommand(command)
	if err != nil {
		return "", false, err
	}
	args := append(
		command[1:], "show-options", "-qv", "-t", session, "@forge_launch",
	)
	cmd := procutil.CommandContext(ctx, command[0], args...)
	cmd.Env = TmuxClientEnvironment(os.Environ(), m.currentStripEnvVars())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err == nil {
		return strings.TrimSpace(stdout.String()), true, nil
	}
	if isTmuxSessionAbsent(stderr.Bytes(), err) {
		return "", false, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		return "", false, err
	}
	return "", false, fmt.Errorf("%w: %s", err, msg)
}

func (m *Manager) killTmuxSession(
	ctx context.Context,
	session string,
) error {
	if session == "" {
		return nil
	}
	command := slices.Clone(m.tmuxCommand)
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	if len(command) == 0 || command[0] == "" {
		return nil
	}
	var err error
	command, err = resolveTmuxCommand(command)
	if err != nil {
		return err
	}
	args := append(command[1:], "kill-session", "-t", session)
	cmd := procutil.CommandContext(ctx, command[0], args...)
	cmd.Env = TmuxClientEnvironment(os.Environ(), m.currentStripEnvVars())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err == nil || isTmuxSessionAbsent(stderr.Bytes(), err) {
		return nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

func (m *Manager) refreshSession(
	ctx context.Context,
	s *session,
) error {
	if s == nil {
		return nil
	}
	info := s.snapshot()
	if info.TmuxSession == "" {
		return nil
	}
	return m.refreshTmuxSessionClients(ctx, info.TmuxSession)
}

func (m *Manager) refreshTmuxSessionClients(
	ctx context.Context,
	session string,
) error {
	if session == "" {
		return nil
	}
	command := slices.Clone(m.tmuxCommand)
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	if len(command) == 0 || command[0] == "" {
		return nil
	}

	listArgs := append(
		command[1:],
		"list-clients", "-t", session, "-F", "#{client_tty}",
	)
	listCmd := procutil.CommandContext(ctx, command[0], listArgs...)
	listCmd.Env = TmuxClientEnvironment(os.Environ(), m.currentStripEnvVars())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	listCmd.Stdout = &stdout
	listCmd.Stderr = &stderr
	if err := procutil.Run(ctx, listCmd, "tmux subprocess capacity"); err != nil {
		if isTmuxSessionAbsent(stderr.Bytes(), err) {
			return nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}

	var errs []error
	for client := range strings.FieldsSeq(stdout.String()) {
		refreshArgs := append(
			command[1:],
			"refresh-client", "-t", client,
		)
		refreshCmd := procutil.CommandContext(
			ctx, command[0], refreshArgs...,
		)
		refreshCmd.Env = TmuxClientEnvironment(os.Environ(), m.currentStripEnvVars())
		var refreshStderr bytes.Buffer
		refreshCmd.Stderr = &refreshStderr
		if err := procutil.Run(
			ctx, refreshCmd, "tmux subprocess capacity",
		); err != nil {
			msg := strings.TrimSpace(refreshStderr.String())
			if msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

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

// BeginStopping holds the stopping marker for workspaceID without
// running StopWorkspace. Use it to extend the marker's lifetime
// across higher-level operations — for example, the workspace
// deletion handler holds it from the start of Delete through the
// destructive cleanup and DB removal so a concurrent Launch cannot
// spawn a process into a worktree that is about to disappear. Must
// be paired with EndStopping.
func (m *Manager) BeginStopping(workspaceID string) {
	m.mu.Lock()
	m.stoppingWS[workspaceID]++
	m.mu.Unlock()
}

// EndStopping releases a marker held by BeginStopping. Decrementing
// to zero unblocks new launches for the workspace.
func (m *Manager) EndStopping(workspaceID string) {
	m.mu.Lock()
	m.stoppingWS[workspaceID]--
	if m.stoppingWS[workspaceID] <= 0 {
		delete(m.stoppingWS, workspaceID)
	}
	m.mu.Unlock()
}

// claimInflight registers a starting Launch so a
// concurrent StopWorkspace can wait for it to finish inserting
// before snapshotting. Rejects if the workspace is already being
// stopped.
func (m *Manager) claimInflight(workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stoppingWS[workspaceID] > 0 {
		return errWorkspaceStopping
	}
	m.inflightWS[workspaceID]++
	return nil
}

func (m *Manager) releaseInflight(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflightWS[workspaceID]--
	if m.inflightWS[workspaceID] <= 0 {
		delete(m.inflightWS, workspaceID)
		if ch, ok := m.inflightCh[workspaceID]; ok {
			close(ch)
			delete(m.inflightCh, workspaceID)
		}
	}
}

// waitInflight blocks until every claimInflight for workspaceID
// that completed before this call has been released. New claims
// are rejected by the stoppingWS marker the caller already holds.
func (m *Manager) waitInflight(
	ctx context.Context,
	workspaceID string,
) error {
	m.mu.Lock()
	if m.inflightWS[workspaceID] == 0 {
		m.mu.Unlock()
		return nil
	}
	ch, ok := m.inflightCh[workspaceID]
	if !ok {
		ch = make(chan struct{})
		m.inflightCh[workspaceID] = ch
	}
	m.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) AttachSession(
	workspaceID string,
	key string,
) (*Attachment, error) {
	return m.AttachSessionWithOptions(workspaceID, key, AttachSessionOptions{
		ResizePriority: ResizePriorityLocal,
		ResizeActive:   true,
	})
}

func (m *Manager) AttachSessionWithOptions(
	workspaceID string,
	key string,
	options AttachSessionOptions,
) (*Attachment, error) {
	slog.Debug(
		"runtime terminal attach requested",
		"workspace_id", workspaceID,
		"session_key", key,
	)
	m.mu.Lock()
	s := m.sessions[key]
	m.mu.Unlock()
	attachment, err := attachToSession(
		s, workspaceID, key, m.refreshSession, options,
	)
	if err != nil {
		slog.Debug(
			"runtime terminal attach rejected",
			"workspace_id", workspaceID,
			"session_key", key,
			"err", err,
		)
		return nil, err
	}
	slog.Debug(
		"runtime terminal attach accepted",
		"workspace_id", workspaceID,
		"session_key", key,
	)
	return attachment, nil
}

// SubmitInitialMessage writes one bounded, already-normalized initial prompt
// through a live agent runtime. It requires observed bracketed-paste mode and
// sends the complete paste frame and Enter in one terminal write.
func (m *Manager) SubmitInitialMessage(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	message string,
) error {
	return m.SubmitAgentMessage(ctx, workspaceID, sessionKey, message)
}

// SubmitAgentMessage writes one bounded, already-normalized prompt through a
// live agent runtime. It requires observed bracketed-paste mode and sends the
// complete paste frame and Enter in one serialized terminal operation.
func (m *Manager) SubmitAgentMessage(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	message string,
) error {
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrInitialMessageNotWritten, err)
	}
	attachment, err := m.AttachSession(workspaceID, sessionKey)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInitialMessageNotWritten, err)
	}
	defer attachment.Close()
	if attachment.Info().Kind != LaunchTargetAgent {
		return fmt.Errorf("%w: runtime session is not an agent", ErrInitialMessageNotWritten)
	}
	return attachment.submitInitialMessage(ctx, message)
}

func (a *Attachment) Write(data []byte) error {
	if a == nil || a.write == nil {
		return errors.New("attachment is closed")
	}
	return a.write(data)
}

func (a *Attachment) Resize(geometry ptysize.Geometry) error {
	if a == nil || a.resize == nil {
		return errors.New("attachment is closed")
	}
	return a.resize(geometry)
}

// ClaimResize records a deliberate interaction from this attachment and
// applies its dimensions when its priority permits it to own PTY sizing. The
// returned bool tells the caller that tmux must be synchronously refreshed
// before forwarding the input that followed the claim.
func (a *Attachment) ClaimResize(geometry ptysize.Geometry) (bool, error) {
	if a == nil || a.claimResize == nil {
		return false, errors.New("attachment is closed")
	}
	return a.claimResize(geometry)
}

// ResizeSettled records that the synchronous tmux refresh following a resize
// claim completed successfully. Failed refreshes deliberately leave the claim
// pending so a later attachment can retry before forwarding input.
func (a *Attachment) ResizeSettled() {
	if a != nil && a.resizeSettled != nil {
		a.resizeSettled()
	}
}

// clampWinsizeDim bounds a client-supplied terminal dimension into the
// uint16 range pty.Winsize requires, so an oversized cols/rows value is
// capped rather than silently truncated by the narrowing conversion.
func clampWinsizeDim(v int) uint16 {
	switch {
	case v < 1:
		return 1
	case v > math.MaxUint16:
		return math.MaxUint16
	default:
		return uint16(v)
	}
}

func (a *Attachment) Refresh(ctx context.Context) error {
	if a == nil || a.refresh == nil {
		return errors.New("attachment is closed")
	}
	return a.refresh(ctx)
}

func (a *Attachment) SetResizeActive(active bool) {
	if a != nil && a.setResizeActive != nil {
		a.setResizeActive(active)
	}
}

func (a *Attachment) Info() SessionInfo {
	if a == nil || a.info == nil {
		return SessionInfo{}
	}
	return a.info()
}

func (a *Attachment) Close() {
	if a != nil && a.close != nil {
		a.close()
	}
}

// SessionOutputClosed reports whether the session's drainOutput has
// observed PTY EOF — i.e. the session itself ended, not merely this
// attachment's per-subscriber channel. Bridges use it to tell a real
// session exit (send the "exited" frame) from a slow-subscriber drop
// (broadcast closed our channel because we fell behind; the session
// is still running and any reconnect should resubscribe).
func (a *Attachment) SessionOutputClosed() bool {
	if a == nil || a.sessionOutputClosed == nil {
		return false
	}
	return a.sessionOutputClosed()
}

// RecoverableDetach reports whether the server deliberately detached this
// session while leaving its backend available. That is a connection
// interruption, not a process exit.
func (a *Attachment) RecoverableDetach() bool {
	if a == nil || a.recoverableDetach == nil {
		return false
	}
	return a.recoverableDetach()
}

func fallbackSessionLabel(label string, fallback string) string {
	label = strings.TrimSpace(label)
	if label != "" {
		return label
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return "Session"
}

func (m *Manager) reserveSessionLabel(
	workspaceID string,
	baseLabel string,
) (string, func()) {
	baseLabel = fallbackSessionLabel(baseLabel, "Session")
	m.mu.Lock()
	defer m.mu.Unlock()

	label := m.nextSessionLabelLocked(workspaceID, baseLabel)
	if m.labelReservations[workspaceID] == nil {
		m.labelReservations[workspaceID] = make(map[string]int)
	}
	m.labelReservations[workspaceID][label]++

	var once sync.Once
	return label, func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			reservations := m.labelReservations[workspaceID]
			if reservations == nil {
				return
			}
			reservations[label]--
			if reservations[label] <= 0 {
				delete(reservations, label)
			}
			if len(reservations) == 0 {
				delete(m.labelReservations, workspaceID)
			}
		})
	}
}

func (m *Manager) nextSessionLabelLocked(workspaceID string, baseLabel string) string {
	used := map[string]bool{}
	for _, s := range m.sessions {
		info := s.snapshot()
		if info.WorkspaceID == workspaceID && strings.TrimSpace(info.Label) != "" {
			used[info.Label] = true
		}
	}
	for label, count := range m.labelReservations[workspaceID] {
		if count > 0 && strings.TrimSpace(label) != "" {
			used[label] = true
		}
	}
	if !used[baseLabel] {
		return baseLabel
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s %d", baseLabel, i)
		if !used[candidate] {
			return candidate
		}
	}
}

func (m *Manager) shellLaunchCommand(
	ctx context.Context,
	command []string,
	workspaceID string,
	sessionKey string,
	cwd string,
) (launchCommand, error) {
	if len(command) == 0 || command[0] == "" {
		return launchCommand{}, errors.New("session command is empty")
	}
	tmux, err := m.target(string(LaunchTargetShell))
	if err != nil || !tmux.Available {
		return launchCommand{Command: command}, nil
	}
	tmuxCommand := slices.Clone(m.tmuxCommand)
	if len(tmuxCommand) == 0 {
		tmuxCommand = slices.Clone(tmux.Command)
	}
	if len(tmuxCommand) == 0 {
		tmuxCommand = config.DefaultTmuxCommand()
	}
	configureServer := config.IsDefaultTmuxCommand(tmuxCommand)
	tmuxCommand, err = resolveTmuxCommand(tmuxCommand)
	if err != nil {
		return launchCommand{}, err
	}
	resolvedCommand := slices.Clone(command)
	resolvedPath, err := resolveExecutable(resolvedCommand[0])
	if err != nil {
		return launchCommand{}, err
	}
	resolvedCommand[0] = resolvedPath

	tmuxSession := tmuxSessionName(workspaceID, sessionKey)
	launchID, err := newTmuxLaunchID()
	if err != nil {
		return launchCommand{}, err
	}
	paneEnv := tmuxShellEnvPolicy.paneEnvironment(
		os.Environ(), resolvedCommand, m.currentStripEnvVars(),
	)
	prepared, err := tmuxLauncher{
		TmuxCommand:     tmuxCommand,
		Session:         tmuxSession,
		CWD:             cwd,
		Pane:            paneEnv,
		OwnerMarker:     m.tmuxOwnerMarker,
		LaunchID:        launchID,
		HideStatus:      m.currentHideTmuxStatus(),
		TmuxMouse:       m.currentTmuxMouse(),
		Graphics:        m.currentTmuxGraphics(),
		ConfigureServer: configureServer,
	}.prepare(ctx)
	if err != nil {
		return launchCommand{}, err
	}
	result := launchCommand{
		Command:     prepared.AttachCommand,
		TmuxSession: tmuxSession,
		TmuxCreated: prepared.Created,
	}
	if prepared.Created {
		result.TmuxLaunchID = launchID
	}
	return result, nil
}

func (m *Manager) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	m.startWG.Wait()

	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*session)
	m.mu.Unlock()

	for _, s := range sessions {
		if m.detachForRestart {
			s.markRecoverableDetach()
		}
		s.detach()
	}
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) ensureOpen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerShutdown
	}
	return nil
}

func (m *Manager) startLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	startMu := m.startLocks[key]
	if startMu == nil {
		startMu = &sync.Mutex{}
		m.startLocks[key] = startMu
	}
	return startMu
}

// CommandSessionStartLockHeldForTest reports whether the keyed command-session
// transaction lock is held. It releases the lock immediately when probing finds
// it available.
func (m *Manager) CommandSessionStartLockHeldForTest(key string) bool {
	startMu := m.startLock(key)
	if startMu.TryLock() {
		startMu.Unlock()
		return false
	}
	return true
}

func (m *Manager) beginStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerShutdown
	}
	m.startWG.Add(1)
	return nil
}

func (m *Manager) finishStart() {
	m.startWG.Done()
}

func (m *Manager) target(key string) (LaunchTarget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	target, ok := m.targets[key]
	if !ok {
		return LaunchTarget{}, fmt.Errorf("target not found: %s", key)
	}
	return cloneTarget(target), nil
}

type launchCommand struct {
	Command      []string
	TmuxSession  string
	TmuxLaunchID string
	TmuxCreated  bool
}

func (m *Manager) launchCommand(
	ctx context.Context,
	target LaunchTarget,
	workspaceID string,
	sessionKeyOrCWD string,
	optionalCWD ...string,
) (launchCommand, error) {
	sessionKey := target.Key
	cwd := sessionKeyOrCWD
	if len(optionalCWD) > 0 {
		sessionKey = sessionKeyOrCWD
		cwd = optionalCWD[0]
	}
	command := slices.Clone(target.Command)
	if target.Kind == LaunchTargetPlainShell {
		if len(command) == 0 {
			command = slices.Clone(m.shellCommand)
		}
		if len(command) == 0 {
			command = defaultShellCommand()
		}
		return m.shellLaunchCommand(ctx, command, workspaceID, sessionKey, cwd)
	}
	if target.Kind != LaunchTargetAgent || !m.wrapAgentsInTmux {
		return launchCommand{Command: command}, nil
	}

	tmux, err := m.target(string(LaunchTargetShell))
	if err != nil || !tmux.Available {
		return launchCommand{Command: command}, nil
	}
	tmuxCommand := slices.Clone(m.tmuxCommand)
	if len(tmuxCommand) == 0 {
		tmuxCommand = slices.Clone(tmux.Command)
	}
	if len(tmuxCommand) == 0 {
		tmuxCommand = config.DefaultTmuxCommand()
	}
	configureServer := config.IsDefaultTmuxCommand(tmuxCommand)
	tmuxCommand, err = resolveTmuxCommand(tmuxCommand)
	if err != nil {
		return launchCommand{}, err
	}
	resolvedAgentCommand := slices.Clone(command)
	resolvedPath, err := resolveExecutable(resolvedAgentCommand[0])
	if err != nil {
		return launchCommand{}, err
	}
	resolvedAgentCommand[0] = resolvedPath

	tmuxSession := tmuxSessionName(workspaceID, sessionKey)
	launchID, err := newTmuxLaunchID()
	if err != nil {
		return launchCommand{}, err
	}

	paneEnv := tmuxAgentEnvPolicy.paneEnvironmentWithExtra(
		os.Environ(), resolvedAgentCommand, m.currentStripEnvVars(), map[string]string{
			agentactivity.RuntimeSessionKeyEnv: sessionKey,
		},
	)
	prepared, err := tmuxLauncher{
		TmuxCommand:     tmuxCommand,
		Session:         tmuxSession,
		CWD:             cwd,
		Pane:            paneEnv,
		OwnerMarker:     m.tmuxOwnerMarker,
		LaunchID:        launchID,
		HideStatus:      m.currentHideTmuxStatus(),
		TmuxMouse:       m.currentTmuxMouse(),
		Graphics:        m.currentTmuxGraphics(),
		ConfigureServer: configureServer,
	}.prepare(ctx)
	if err != nil {
		return launchCommand{}, err
	}
	result := launchCommand{
		Command:     prepared.AttachCommand,
		TmuxSession: tmuxSession,
		TmuxCreated: prepared.Created,
	}
	if prepared.Created {
		result.TmuxLaunchID = launchID
	}
	return result, nil
}

func tmuxSessionName(workspaceID string, targetKey string) string {
	sum := sha256.Sum256([]byte(targetKey))
	return "forge-" + tmuxSessionSafeComponent(workspaceID) + "-" +
		hex.EncodeToString(sum[:8])
}

func tmuxSessionSafeComponent(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_' || r == '-':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (m *Manager) runningSession(
	sessions map[string]*session,
	key string,
) *session {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := sessions[key]
	if s == nil {
		return nil
	}
	info := s.snapshot()
	if info.Status != SessionStatusRunning &&
		info.Status != SessionStatusStarting {
		delete(sessions, key)
		return nil
	}
	// drainOutput closes outputClosed as soon as the PTY hits EOF,
	// but watchSession only marks Status=Exited once cmd.Wait
	// returns — which can lag noticeably for wrapped commands
	// (systemd-run --wait, etc.). Treating those output-dead
	// sessions as still "running" hands callers a zombie they can
	// attach to but never receive any bytes from. Reject the zombie
	// so Launch starts a fresh session.
	//
	// The caller will overwrite our map entry with the new session,
	// which means we are about to lose our only handle to the old
	// process. If its wrapper is still in cmd.Wait (zsh exited but
	// systemd-run is still tearing down the transient unit), or
	// worse, hung indefinitely, Manager.Shutdown can no longer
	// reach it. stop() it now: SIGKILL the process group and close
	// the master so cmd.Wait returns and the goroutine can finish.
	// removeExitedSession's identity check (current != s) guards
	// against double-cleanup once the new session is in place.
	s.mu.Lock()
	outputClosed := s.outputClosed
	s.mu.Unlock()
	if outputClosed {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.stop(ctx)
		cancel()
		return nil
	}
	return s
}

func (m *Manager) session(
	workspaceID string,
	key string,
) (*session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[key]; ok &&
		s.snapshot().WorkspaceID == workspaceID {
		return s, true
	}
	return nil, false
}

func (m *Manager) removeIfSame(
	workspaceID string,
	key string,
	s *session,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if current, ok := m.sessions[key]; ok &&
		current == s &&
		current.snapshot().WorkspaceID == workspaceID {
		delete(m.sessions, key)
	}
}

func (m *Manager) watchSession(
	s *session,
) {
	info := s.watch()
	if s.wasStopRequested() {
		return
	}
	if m.removeExitedSession(info, s) && m.onSessionExit != nil {
		m.onSessionExit(info)
	}
}

func (m *Manager) removeExitedSession(
	info SessionInfo,
	s *session,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.sessions[info.Key]
	if !ok || current != s {
		return false
	}
	delete(m.sessions, info.Key)
	return true
}

func (m *Manager) startOwnedSession(
	ctx context.Context,
	info SessionInfo,
	command []string,
	cwd string,
	extraStripVars []string,
) (*session, error) {
	if info.TmuxSession != "" {
		return startTmuxAttachSession(info, command, cwd, extraStripVars)
	}
	if m.ptyOwnerRuntime != nil {
		return startPtyOwnerSession(
			ctx, m.ptyOwnerRuntime, info, command, cwd, extraStripVars,
		)
	}
	return nil, errors.New("runtime sessions require tmux or ptyowner")
}

func (s *session) snapshot() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := s.info
	info.TmuxSession = s.tmuxSession
	if s.info.ExitedAt != nil {
		exitedAt := *s.info.ExitedAt
		info.ExitedAt = &exitedAt
	}
	if s.info.ExitCode != nil {
		exitCode := *s.info.ExitCode
		info.ExitCode = &exitCode
	}
	return info
}

func (s *session) watch() SessionInfo {
	if s.pty != nil {
		return s.watchPtyOwner()
	}
	exitCode := waitExitCode(s.cmd.Wait())
	now := time.Now().UTC()

	s.mu.Lock()
	s.info.Status = SessionStatusExited
	s.info.ExitedAt = &now
	s.info.ExitCode = &exitCode
	info := s.info
	info.TmuxSession = s.tmuxSession
	s.mu.Unlock()

	// Do not close ptmx immediately. cmd.Wait only tells us the session
	// leader exited; the PTY reader may still have kernel-buffered output to
	// broadcast, and closing the master here races that final output. The
	// natural EOF path belongs to drainOutput, but keep it bounded so a
	// daemonized child that holds the slave PTY open cannot leak this session's
	// reader goroutine and master descriptor forever.
	s.closePTYAfterPostExitDrainDeadline()
	close(s.done)
	slog.Debug(
		"runtime session exited",
		"workspace_id", info.WorkspaceID,
		"session_key", info.Key,
		"target_key", info.TargetKey,
		"exit_code", exitCode,
	)
	return info
}

func (s *session) watchPtyOwner() SessionInfo {
	<-s.pty.Done()
	exitCode := s.pty.ExitCode()
	now := time.Now().UTC()

	s.mu.Lock()
	s.info.Status = SessionStatusExited
	s.info.ExitedAt = &now
	s.info.ExitCode = &exitCode
	info := s.info
	info.TmuxSession = s.tmuxSession
	s.mu.Unlock()

	// The owner can report process exit before its output reader publishes the
	// final PTY chunk and closes Output. Keep subscribers alive for that bounded
	// drain; closing them immediately loses fast-exit command output.
	if s.outputDone != nil {
		select {
		case <-s.outputDone:
		case <-time.After(postExitPTYDrainTimeout):
		}
	}
	s.closeSubscribers()
	close(s.done)
	slog.Debug(
		"runtime session exited",
		"workspace_id", info.WorkspaceID,
		"session_key", info.Key,
		"target_key", info.TargetKey,
		"exit_code", exitCode,
		"pty_backend", "pty_owner",
	)
	return info
}

func (s *session) drainOutput() {
	if s.outputDone != nil {
		defer close(s.outputDone)
	}
	if s.pty != nil {
		for chunk := range s.pty.Output() {
			if len(chunk) > 0 {
				s.broadcast(chunk)
			}
		}
		// The PTY-owner client records the process exit code before closing
		// Output. Publish that code before closing runtime subscribers so an
		// HTTP terminal bridge that observes output EOF first does not emit -1
		// while watchPtyOwner is still waiting to update the session snapshot.
		if exitCode := s.pty.ExitCode(); exitCode != -1 {
			s.mu.Lock()
			if s.info.ExitCode == nil {
				s.info.ExitCode = &exitCode
			}
			s.mu.Unlock()
		}
		s.closeSubscribers()
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.broadcast(buf[:n])
		}
		if err != nil {
			_ = s.ptmx.Close()
			s.closeSubscribers()
			return
		}
	}
}

func (s *session) closePTYAfterPostExitDrainDeadline() {
	if s.ptmx == nil || s.outputDone == nil {
		return
	}
	go func() {
		select {
		case <-s.outputDone:
		case <-time.After(postExitPTYDrainTimeout):
			_ = s.ptmx.Close()
		}
	}()
}

func (s *session) broadcast(data []byte) {
	chunk := slices.Clone(data)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.inputModes.observe(chunk)
	s.appendReplayOutputLocked(chunk)
	replayReady := len(s.terminalSequenceTail) == 0

	for ch := range s.subscribers {
		select {
		case ch <- chunk:
		default:
			delete(s.subscribers, ch)
			delete(s.replayBoundarySubscribers, ch)
			close(ch)
			continue
		}
		if !replayReady {
			continue
		}
		if _, pending := s.replayBoundarySubscribers[ch]; !pending {
			continue
		}
		select {
		case ch <- nil:
			delete(s.replayBoundarySubscribers, ch)
		default:
			delete(s.subscribers, ch)
			delete(s.replayBoundarySubscribers, ch)
			close(ch)
		}
	}
}

type alternateScreenEvent struct {
	start  int
	end    int
	active bool
}

func (s *session) appendReplayOutputLocked(chunk []byte) {
	s.updateTerminalSequenceTailLocked(chunk)

	// Alternate-screen TUIs are stateful. Replaying a suffix of their
	// screen history into a fresh terminal can corrupt the attach, so
	// keep only normal-screen output for future subscribers.
	scan := append(slices.Clone(s.alternateScreenTail), chunk...)
	events := alternateScreenEvents(scan)
	tailLen := len(s.alternateScreenTail)
	active := s.alternateScreenActive
	normalStart := 0

	for _, event := range events {
		if event.end <= tailLen {
			continue
		}
		chunkStart := max(event.start-tailLen, 0)
		chunkEnd := min(event.end-tailLen, len(chunk))
		if !active && chunkStart > normalStart {
			s.appendOutputBufferLocked(chunk[normalStart:chunkStart])
		}
		if event.active {
			s.outputBuffer = nil
		}
		active = event.active
		normalStart = chunkEnd
	}

	if !active && normalStart < len(chunk) {
		s.appendOutputBufferLocked(chunk[normalStart:])
	}
	s.alternateScreenActive = active
	s.alternateScreenTail = trailingBytes(scan, maxAlternateScreenSequenceLen-1)
}

func (s *session) updateTerminalSequenceTailLocked(chunk []byte) {
	scan := append(slices.Clone(s.terminalSequenceTail), chunk...)
	tailLen := trailingIncompleteTerminalDataLen(scan)
	if tailLen == 0 || tailLen > maxSessionOutputReplay {
		s.terminalSequenceTail = nil
		return
	}
	s.terminalSequenceTail = slices.Clone(scan[len(scan)-tailLen:])
}

func (s *session) appendOutputBufferLocked(chunk []byte) {
	s.outputBuffer = append(s.outputBuffer, chunk...)
	if extra := len(s.outputBuffer) - maxSessionOutputReplay; extra > 0 {
		start := replaySuffixStart(s.outputBuffer, extra)
		s.outputBuffer = slices.Clone(s.outputBuffer[start:])
	}
}

// replaySuffixStart advances a size-based cutoff only when it demonstrably
// splits a valid UTF-8 rune. Invalid continuation bytes are preserved because
// terminals may use them as raw C1 controls.
func replaySuffixStart(data []byte, start int) int {
	if start <= 0 || start >= len(data) || utf8.RuneStart(data[start]) {
		return start
	}
	for candidate := start - 1; candidate >= max(0, start-utf8.UTFMax+1); candidate-- {
		if !utf8.RuneStart(data[candidate]) {
			continue
		}
		_, size := utf8.DecodeRune(data[candidate:])
		if size > 1 && candidate+size > start {
			return candidate + size
		}
		return start
	}
	return start
}

func alternateScreenEvents(data []byte) []alternateScreenEvent {
	events := make([]alternateScreenEvent, 0, 2)
	for i := range data {
		if seq, ok := matchingSequence(data[i:], alternateScreenEnterSequences); ok {
			events = append(events, alternateScreenEvent{
				start:  i,
				end:    i + len(seq),
				active: true,
			})
			continue
		}
		if seq, ok := matchingSequence(data[i:], alternateScreenExitSequences); ok {
			events = append(events, alternateScreenEvent{
				start:  i,
				end:    i + len(seq),
				active: false,
			})
		}
	}
	return events
}

func matchingSequence(data []byte, sequences [][]byte) ([]byte, bool) {
	for _, seq := range sequences {
		if bytes.HasPrefix(data, seq) {
			return seq, true
		}
	}
	return nil, false
}

func trailingBytes(data []byte, maxLen int) []byte {
	if maxLen <= 0 || len(data) == 0 {
		return nil
	}
	if len(data) > maxLen {
		data = data[len(data)-maxLen:]
	}
	return slices.Clone(data)
}

func (s *session) subscribe() (<-chan []byte, func()) {
	return s.subscribeInternal(true, false)
}

func (s *session) subscribeWithReplayBoundary() (<-chan []byte, func()) {
	return s.subscribeInternal(true, true)
}

func (s *session) subscribeInternal(
	includeReplay bool,
	replayBoundary bool,
) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)

	s.mu.Lock()
	info := s.info
	if includeReplay {
		replay := make([]byte, 0)
		if !s.alternateScreenActive {
			replay = append(replay, s.outputBuffer...)
		}
		var replayModes terminalInputModeState
		replayModes.observe(replay)
		if pendingLen := trailingIncompleteTerminalDataLen(replay); pendingLen > 0 {
			stableLen := len(replay) - pendingLen
			pending := slices.Clone(replay[stableLen:])
			replay = slices.Clone(replay[:stableLen])
			replay = s.inputModes.appendTransitions(replay, replayModes)
			replay = append(replay, pending...)
		} else {
			replay = s.inputModes.appendTransitions(replay, replayModes)
		}
		if s.alternateScreenActive {
			replay = append(replay, s.terminalSequenceTail...)
		}
		if len(replay) > 0 {
			ch <- replay
			slog.Debug(
				"runtime terminal replay queued",
				"workspace_id", info.WorkspaceID,
				"session_key", info.Key,
				"bytes", len(replay),
			)
		}
	}
	if replayBoundary {
		if s.outputClosed || len(s.terminalSequenceTail) == 0 {
			ch <- nil
		} else {
			if s.replayBoundarySubscribers == nil {
				s.replayBoundarySubscribers = make(map[chan []byte]struct{})
			}
			s.replayBoundarySubscribers[ch] = struct{}{}
		}
	}
	if s.outputClosed {
		close(ch)
		s.mu.Unlock()
		return ch, func() {}
	}
	s.subscribers[ch] = struct{}{}
	subscriberCount := len(s.subscribers)
	s.mu.Unlock()
	slog.Debug(
		"runtime terminal subscriber added",
		"workspace_id", info.WorkspaceID,
		"session_key", info.Key,
		"subscribers", subscriberCount,
	)

	return ch, func() {
		s.mu.Lock()
		info := s.info
		if _, ok := s.subscribers[ch]; ok {
			delete(s.subscribers, ch)
			delete(s.replayBoundarySubscribers, ch)
			close(ch)
		}
		subscriberCount := len(s.subscribers)
		s.mu.Unlock()
		slog.Debug(
			"runtime terminal subscriber removed",
			"workspace_id", info.WorkspaceID,
			"session_key", info.Key,
			"subscribers", subscriberCount,
		)
	}
}

func (s *session) registerResizeAttachment(
	priority ResizePriority,
	active bool,
) uint64 {
	if priority == 0 {
		priority = ResizePriorityRemote
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextAttachmentID++
	id := s.nextAttachmentID
	if s.resizeAttachments == nil {
		s.resizeAttachments = make(map[uint64]resizeAttachment)
	}
	s.resizeAttachments[id] = resizeAttachment{
		priority: priority,
		active:   active,
	}
	if active && (s.resizeOwnerID == 0 || priority > s.resizeOwnerPriority) {
		s.resizeOwnerID = id
		s.resizeOwnerPriority = priority
	}
	return id
}

func (s *session) unregisterResizeAttachment(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.resizeAttachments != nil {
		delete(s.resizeAttachments, id)
	}
	if s.resizeOwnerID != id {
		return
	}
	s.resizeOwnerID = 0
	s.resizeOwnerPriority = 0
	s.selectResizeOwnerLocked()
	s.applyResizeOwnerSizeLocked(s.resizeAttachments[s.resizeOwnerID])
}

func (s *session) resizeAttachment(
	id uint64,
	geometry ptysize.Geometry,
	claim bool,
) (uint64, error) {
	if geometry.Cols <= 0 || geometry.Rows <= 0 {
		return 0, nil
	}
	geometry = ptysize.Normalize(geometry)

	s.mu.Lock()
	defer s.mu.Unlock()

	attachment, ok := s.resizeAttachments[id]
	if !ok || (!attachment.active && !claim) {
		return 0, nil
	}
	if claim {
		attachment.active = true
	}
	sizeChanged := !attachment.hasSize ||
		attachment.geometry != geometry
	attachment.geometry = geometry
	attachment.hasSize = true
	if claim {
		s.nextResizeClaim++
		attachment.claim = s.nextResizeClaim
	}
	s.resizeAttachments[id] = attachment

	ownerChanged := false
	if claim && (s.resizeOwnerID == 0 ||
		attachment.priority >= s.resizeOwnerPriority) &&
		s.resizeOwnerID != id {
		s.resizeOwnerID = id
		s.resizeOwnerPriority = attachment.priority
		ownerChanged = true
	}
	if s.resizeOwnerID != id {
		return 0, nil
	}
	if !claim || ownerChanged || sizeChanged {
		if err := s.resizePTYAndMarkLocked(geometry); err != nil {
			return 0, err
		}
	}
	if !claim || s.resizeGeneration <= s.resizeSettledGeneration {
		return 0, nil
	}
	return s.resizeGeneration, nil
}

func (s *session) markResizeSettled(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation > s.resizeSettledGeneration {
		s.resizeSettledGeneration = generation
	}
}

func (s *session) setResizeAttachmentActive(id uint64, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	attachment, ok := s.resizeAttachments[id]
	if !ok {
		return
	}
	attachment.active = active
	s.resizeAttachments[id] = attachment
	if active {
		if s.resizeOwnerID == 0 ||
			attachment.priority > s.resizeOwnerPriority {
			s.resizeOwnerID = id
			s.resizeOwnerPriority = attachment.priority
			s.applyResizeOwnerSizeLocked(attachment)
		}
		return
	}
	if s.resizeOwnerID == id {
		s.resizeOwnerID = 0
		s.resizeOwnerPriority = 0
		s.selectResizeOwnerLocked()
		s.applyResizeOwnerSizeLocked(s.resizeAttachments[s.resizeOwnerID])
	}
}

func (s *session) selectResizeOwnerLocked() {
	for attachmentID, attachment := range s.resizeAttachments {
		if !attachment.active {
			continue
		}
		owner := s.resizeAttachments[s.resizeOwnerID]
		if s.resizeOwnerID == 0 ||
			attachment.priority > s.resizeOwnerPriority ||
			(attachment.priority == s.resizeOwnerPriority &&
				(attachment.claim > owner.claim ||
					(attachment.claim == owner.claim && attachmentID < s.resizeOwnerID))) {
			s.resizeOwnerID = attachmentID
			s.resizeOwnerPriority = attachment.priority
		}
	}
}

func (s *session) applyResizeOwnerSizeLocked(attachment resizeAttachment) {
	if !attachment.hasSize {
		return
	}
	_ = s.resizePTYAndMarkLocked(attachment.geometry)
}

func (s *session) resizePTYAndMarkLocked(geometry ptysize.Geometry) error {
	if err := s.resizePTYLocked(geometry); err != nil {
		return err
	}
	s.resizeGeneration++
	return nil
}

func (s *session) resizePTYLocked(geometry ptysize.Geometry) error {
	if s.pty != nil {
		return s.pty.Resize(geometry)
	}
	return ptysize.Resize(s.ptmx, geometry)
}

func (s *session) closeSubscribers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputClosed {
		return
	}
	s.outputClosed = true
	for ch := range s.subscribers {
		if _, pending := s.replayBoundarySubscribers[ch]; pending {
			select {
			case ch <- nil:
			default:
			}
			delete(s.replayBoundarySubscribers, ch)
		}
		delete(s.subscribers, ch)
		close(ch)
	}
}

func (s *session) stop(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	if s.lifecycleClosed {
		s.mu.Unlock()
		return nil
	}
	lifecycle := s.lifecycle
	s.mu.Unlock()

	if lifecycle != nil {
		if err := lifecycle.Stop(ctx); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.lifecycleClosed = true
	s.mu.Unlock()
	return nil
}

func (s *session) markStopRequested() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopRequested = true
}

func (s *session) wasStopRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopRequested
}

func (s *session) markRecoverableDetach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverableDetach = true
}

func (s *session) detach() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	if s.lifecycleClosed {
		s.mu.Unlock()
		return
	}
	lifecycle := s.lifecycle
	s.lifecycleClosed = true
	s.mu.Unlock()

	if lifecycle != nil {
		lifecycle.Detach()
	}
}

func waitSessionDone(s *session) {
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

func (m *Manager) newSessionKey(workspaceID string) (string, error) {
	for range 16 {
		key, err := NewSessionKey(workspaceID)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		_, sessionExists := m.sessions[key]
		m.mu.Unlock()
		if !sessionExists {
			return key, nil
		}
	}
	return "", errors.New("generate unique runtime session key")
}

func NewSessionKey(workspaceID string) (string, error) {
	var data [8]byte
	if _, err := cryptorand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate runtime session id: %w", err)
	}
	return workspaceID + "_" + hex.EncodeToString(data[:]), nil
}

func newTmuxLaunchID() (string, error) {
	var data [16]byte
	if _, err := cryptorand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate tmux launch id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

// resolveExecutable returns an absolute path for name. Names that
// are already absolute are accepted as-is; names without a path
// separator are looked up via PATH; relative names with separators
// (e.g. "./agent", "scripts/codex") are rejected because cmd.Dir
// is set to the workspace worktree, which is PR-controlled content.
func resolveExecutable(name string) (string, error) {
	if name == "" {
		return "", errors.New("session command is empty")
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	if !strings.ContainsAny(name, `/\`) {
		path := procutil.ResolveBinary(name)
		if path == name {
			var err error
			path, err = exec.LookPath(name)
			if err != nil {
				return "", fmt.Errorf(
					"resolve session command %q via PATH: %w",
					name, err,
				)
			}
		}
		if path == name {
			return "", fmt.Errorf(
				"resolve session command %q via PATH: not found",
				name,
			)
		}
		// LookPath joins the matched PATH entry with name; a
		// relative entry like "bin" or "." yields a relative
		// result, which would re-resolve inside cmd.Dir (the
		// worktree). Bind it to an absolute path now, while we
		// are still in kenn-forge's working directory.
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return "", fmt.Errorf(
					"resolve session command %q via PATH: %w",
					name, err,
				)
			}
			path = abs
		}
		return path, nil
	}
	return "", fmt.Errorf(
		"session command %q must be an absolute path or a "+
			"PATH-resolvable name; relative paths resolve inside "+
			"the workspace worktree, which is untrusted",
		name,
	)
}

func resolveTmuxCommand(command []string) ([]string, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, errors.New("tmux command is empty")
	}
	resolved := slices.Clone(command)
	path, err := resolveExecutable(resolved[0])
	if err != nil {
		return nil, fmt.Errorf("resolve tmux command: %w", err)
	}
	resolved[0] = path
	return resolved, nil
}

// sessionVarPrefixes name prefixes whose env vars are stripped from
// launched runtime sessions. These tend to carry server credentials
// or API keys that an agent process running inside an untrusted
// workspace must not be able to read.
var sessionVarPrefixes = []string{
	"KENN_FORGE_GITHUB_TOKEN",
	"KENN_FORGE_GITLAB_TOKEN",
	"KENN_FORGE_FORGEJO_TOKEN",
	"KENN_FORGE_GITEA_TOKEN",
	"MIDDLEMAN_GITHUB_TOKEN",
	"MIDDLEMAN_GITLAB_TOKEN",
	"MIDDLEMAN_FORGEJO_TOKEN",
	"MIDDLEMAN_GITEA_TOKEN",
	"GITHUB_TOKEN",
	"GITLAB_TOKEN",
	"FORGEJO_TOKEN",
	"GITEA_TOKEN",
	"GH_TOKEN",
	"GITHUB_PAT",
	"GH_PAT",
	"GITHUB_ENTERPRISE_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"KATA_AUTH_TOKEN",
}

// TmuxClientEnvironment reduces env to the non-secret tmux session
// allowlist, additionally stripping the configured token names in
// extraStrip: the allowlist admits LC_ and XDG_ prefixes, which a
// configured token name could hide under. Every tmux client invocation
// that can spawn the tmux server must use it: the server retains its
// spawn environment for any pane to read via `show-environment -g`,
// and an allowlist cannot leak a credential the configuration never
// named.
func TmuxClientEnvironment(env []string, extraStrip []string) []string {
	return tmuxSessionEnvironment(env, extraStrip)
}

// SessionEnvironment returns a copy of env with credential-shaped
// variables removed (matched by the built-in prefix list and any names
// in extraStrip). Other variables are preserved; pane processes use it
// so benign daemon variables stay usable in terminals.
func SessionEnvironment(env []string, extraStrip []string) []string {
	return sessionEnvironment(env, extraStrip)
}

// sessionEnvironment returns a copy of env with credential-shaped
// variables removed (matched by the built-in prefix list and any
// names in extraStrip). Other variables are preserved.
func sessionEnvironment(env []string, extraStrip []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		if shouldStripSessionVar(key, extraStrip) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func shouldStripSessionVar(key string, extraStrip []string) bool {
	return shouldStripSessionVarFold(
		key, extraStrip, runtime.GOOS == "windows",
	)
}

// shouldStripSessionVarFold matches case-insensitively when fold is
// set: Windows resolves environment variables case-insensitively, so a
// token stored as github_token is the same credential as GITHUB_TOKEN.
func shouldStripSessionVarFold(
	key string, extraStrip []string, fold bool,
) bool {
	match := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	hasPrefix := func(value, prefix string) bool {
		if len(value) < len(prefix) {
			return false
		}
		return match(value[:len(prefix)], prefix)
	}
	for _, prefix := range sessionVarPrefixes {
		if match(key, prefix) || hasPrefix(key, prefix+"_") {
			return true
		}
	}
	return slices.ContainsFunc(extraStrip, func(name string) bool {
		return match(key, name)
	})
}

func defaultShellCommand() []string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return []string{shell}
	}
	return []string{"/bin/sh"}
}

func waitExitCode(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func attachToSession(
	s *session,
	workspaceID string,
	key string,
	refresh func(context.Context, *session) error,
	options AttachSessionOptions,
) (*Attachment, error) {
	if s == nil {
		return nil, fmt.Errorf("session %q not found", key)
	}
	info := s.snapshot()
	if info.WorkspaceID != workspaceID {
		return nil, fmt.Errorf("session %q not found", key)
	}
	if info.Status != SessionStatusRunning &&
		info.Status != SessionStatusStarting {
		return nil, fmt.Errorf("session %q is not running", key)
	}

	var output <-chan []byte
	var unsubscribe func()
	if options.ReplayBoundary {
		output, unsubscribe = s.subscribeWithReplayBoundary()
	} else {
		output, unsubscribe = s.subscribe()
	}
	resizeAttachmentID := s.registerResizeAttachment(
		options.ResizePriority,
		options.ResizeActive,
	)
	var resizeSettlementMu sync.Mutex
	var resizeSettlementGeneration uint64
	return &Attachment{
		Output: output,
		Done:   s.done,
		info:   s.snapshot,
		write: func(data []byte) error {
			s.inputMu.Lock()
			defer s.inputMu.Unlock()
			return s.writeInput(data)
		},
		submitInitialMessage: s.submitInitialMessage,
		resize: func(geometry ptysize.Geometry) error {
			_, err := s.resizeAttachment(resizeAttachmentID, geometry, false)
			return err
		},
		claimResize: func(geometry ptysize.Geometry) (bool, error) {
			generation, err := s.resizeAttachment(
				resizeAttachmentID, geometry, true,
			)
			resizeSettlementMu.Lock()
			resizeSettlementGeneration = generation
			resizeSettlementMu.Unlock()
			return generation > 0, err
		},
		resizeSettled: func() {
			resizeSettlementMu.Lock()
			generation := resizeSettlementGeneration
			resizeSettlementGeneration = 0
			resizeSettlementMu.Unlock()
			s.markResizeSettled(generation)
		},
		refresh: func(ctx context.Context) error {
			if refresh == nil {
				return nil
			}
			return refresh(ctx, s)
		},
		setResizeActive: func(active bool) {
			s.setResizeAttachmentActive(resizeAttachmentID, active)
		},
		close: func() {
			s.unregisterResizeAttachment(resizeAttachmentID)
			unsubscribe()
		},
		sessionOutputClosed: func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.outputClosed
		},
		recoverableDetach: func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.recoverableDetach
		},
	}, nil
}

func (s *session) submitInitialMessage(ctx context.Context, message string) error {
	s.mu.Lock()
	if !s.inputModes.observed[2004] {
		s.mu.Unlock()
		return ErrBracketedPasteInactive
	}
	paste := make([]byte, 0, len(message)+12)
	paste = append(paste, "\x1b[200~"...)
	paste = append(paste, message...)
	paste = append(paste, "\x1b[201~"...)
	s.mu.Unlock()

	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrInitialMessageNotWritten, err)
	}
	writeTimeout := initialMessageWriteTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < writeTimeout {
			writeTimeout = remaining
		}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := context.Cause(writeCtx); err != nil {
		return fmt.Errorf("%w: %w", ErrInitialMessageNotWritten, err)
	}
	// The paste, settle delay, and Enter run as one operation that outlives
	// a caller which stops waiting: a paste that lands after the deadline
	// without its Enter would leave the prompt sitting in the input box.
	result := make(chan error, 1)
	go func() {
		s.inputMu.Lock()
		defer s.inputMu.Unlock()
		if err := s.writeInput(paste); err != nil {
			result <- err
			return
		}
		time.Sleep(initialMessageEnterDelay)
		result <- s.writeInput([]byte("\r"))
	}()
	select {
	case err := <-result:
		return err
	case <-writeCtx.Done():
		return context.Cause(writeCtx)
	}
}

// writeInput writes terminal input to the session PTY. Callers hold inputMu.
func (s *session) writeInput(data []byte) error {
	if s.pty != nil {
		return s.pty.Write(data)
	}
	_, err := s.ptmx.Write(data)
	return err
}
