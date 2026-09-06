package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/configwatch"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/docs"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	katacatalog "go.kenn.io/forge/internal/kata"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/projects"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/ptyowner"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/server/docsapi"
	"go.kenn.io/forge/internal/server/fleetapi"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/issueapi"
	"go.kenn.io/forge/internal/server/kata"
	"go.kenn.io/forge/internal/server/pullapi"
	"go.kenn.io/forge/internal/server/repobrowserapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/systemclipboard"
	"go.kenn.io/forge/internal/telemetry"
	"go.kenn.io/forge/internal/terminalpaste"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var crossOriginProtection http.CrossOriginProtection

type BuildInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

type versionOutputBody BuildInfo
type versionOutput = httpapi.BodyOutput[versionOutputBody]

type ServerOptions struct {
	DaemonAccess                       DaemonAccessOptions
	FederationCredentials              *federationauth.Store
	FederationEnrollments              *federation.Store
	FederationSpokeID                  string
	FederationSpokeActive              bool
	FederationSpokeUnavailableReason   string
	MaintainFederationSpokeActivation  func(context.Context)
	FederationHTTPClient               *http.Client
	ProviderWriteGate                  *providerplane.ProviderWriteGate
	MCPURL                             string
	Clones                             *gitclone.Manager // optional clone manager for diff view
	WorktreeDir                        string            // base dir for workspace worktrees
	DisableWorkspaceBackgroundMonitors bool
	DisableWorkspaceEnrichment         bool
	WorkspaceNow                       func() time.Time
	PtyOwnerDir                        string
	PtyOwnerExePath                    string
	PtyOwnerExeArgs                    []string
	PtyOwnerManagerPath                string
	PtyOwnerCommand                    []string
	PtyOwnerInProcess                  bool
	Telemetry                          telemetry.Client
	TokenSources                       *tokenauth.SourceSet
	Archive                            archive.Controller
	DocsRegistry                       *docs.Registry
	// TerminalClipboard overrides native clipboard integration in tests.
	TerminalClipboard systemclipboard.Writer
	// HostCheck overrides the Host validation middleware options.
	// When Valid(), the override wins over any cfg-derived options.
	// Used by wire-level tests that want to control the bind /
	// allowed_hosts / trust_reverse_proxy independently of a full
	// config.Config.
	HostCheck HostCheckOptions
	// HostCheckAllowLoopbackAnyPort relaxes literal loopback Host
	// port matching after HostCheck/cfg options have been selected.
	// Use this for httptest-style listeners on ephemeral ports.
	HostCheckAllowLoopbackAnyPort bool
	// DetachRuntimeSessionsForRestart makes shutdown-driven terminal
	// attachment loss reconnectable. Development reloaders use this because
	// the durable tmux/ptyowner process outlives the server process.
	DetachRuntimeSessionsForRestart bool
	deferredMergeMaxWait            time.Duration
}

type shutdownDeadline struct {
	mu       sync.RWMutex
	deadline time.Time
	set      bool
}

var (
	startupTmuxCleanupTimeout    = 2 * time.Second
	runtimeSessionCleanupTimeout = 2 * time.Second
)

func (d *shutdownDeadline) tighten(deadline time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.set || deadline.Before(d.deadline) {
		d.deadline = deadline
		d.set = true
	}
}

func (d *shutdownDeadline) get() (time.Time, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.deadline, d.set
}

type shutdownAwareContext struct {
	parent   context.Context
	deadline *shutdownDeadline
}

func (c shutdownAwareContext) Deadline() (time.Time, bool) {
	deadline, ok := c.deadline.get()
	if !ok {
		return c.parent.Deadline()
	}
	if parentDeadline, parentOK := c.parent.Deadline(); parentOK &&
		parentDeadline.Before(deadline) {
		return parentDeadline, true
	}
	return deadline, true
}

func (c shutdownAwareContext) Done() <-chan struct{} {
	return c.parent.Done()
}

func (c shutdownAwareContext) Err() error {
	return c.parent.Err()
}

func (c shutdownAwareContext) Value(key any) any {
	return c.parent.Value(key)
}

type pullLifecycle interface {
	Stop()
	Shutdown(context.Context) error
}

// Server holds the HTTP mux and its dependencies.
type Server struct {
	db             *db.DB
	repoResolver   *httpapi.RepositoryResolver
	syncer         *ghclient.Syncer
	archive        archive.Controller
	clones         *gitclone.Manager
	workspaces     *workspace.Manager
	fleetAPI       *fleetapi.Handler
	runtime        *localruntime.Manager
	tmuxCmd        []string
	telemetry      telemetry.Client
	cfg            *config.Config
	cfgPath        string
	tokenSources   *tokenauth.SourceSet
	cfgMu          sync.Mutex
	configReloadMu sync.Mutex
	// repoVisibilityMu serializes hidden-from-UI mutations with the orphan
	// sweep so a visibility write cannot interleave with a concurrent
	// exact-entry removal and recreate an orphaned preference.
	repoVisibilityMu sync.Mutex
	// bootCfgSnapshot freezes the subset of config fields that are
	// bound at startup (registry, listeners, clone manager, etc.) so a
	// config-file watcher reload can detect when those changed and
	// surface restart_required to the UI without ever mutating them.
	bootCfgSnapshot     startupConfigSnapshot
	fleetEnabledAtBoot  bool
	runtimeStripEnvVars []string
	ptyOwnerClient      *ptyowner.Client
	configWatcher       *configwatch.Watcher
	basePath            string
	options             ServerOptions
	allowedHostMu       sync.RWMutex
	allowedHosts        map[string]struct{}
	// hostOpts is atomic: Serve repoints an ephemeral (port-0) bind
	// at the kernel-assigned port while requests may already be
	// reading the options.
	hostOpts               atomic.Pointer[HostCheckOptions]
	buildInfo              BuildInfo
	now                    func() time.Time
	handler                http.Handler
	hub                    *EventHub
	federationStreamsMu    sync.Mutex
	federationStreamsNext  uint64
	federationStreams      map[string]map[uint64]context.CancelFunc
	activeWorktreeMu       sync.Mutex
	activeWorktreeKey      string
	activeWorktreeSet      bool
	labelCatalogRefreshMu  sync.Mutex
	labelCatalogRefreshIDs map[int64]struct{}
	detailSyncMu           sync.Mutex
	detailSyncInFlight     map[string]struct{}
	detailSyncPending      map[string]detailSyncJob
	writeCredProbeMu       sync.Mutex
	writeCredProbes        map[string]writeCredentialProbe
	writeCredProbeInFlight map[string]chan struct{}
	viewerLoginMu          sync.Mutex
	viewerLoginCache       map[string]viewerLoginCacheEntry
	viewerLoginInFlight    map[string]*viewerLoginCall
	docsAPI                *docsapi.Handler
	kataAPI                *kata.Handler
	repoBrowserAPI         *repobrowserapi.Handler
	pullAPI                *pullapi.Handler
	issueAPI               *issueapi.Handler
	pullLifecycle          pullLifecycle
	workspaceAPI           *workspaceapi.Handler
	providerSource         *hubProviderSource
	providerProxy          *providerProxy
	hubEvents              *hubEventLifecycle
	spokeActivationLease   *hubEventLifecycle
	providerRouteSpoke     bool
	providerWriteGate      *providerplane.ProviderWriteGate
	// activityAfterItemsForTest pauses Activity between its two identity reads
	// so tests can prove the request-wide repository reconciliation fence.
	activityAfterItemsForTest func()
	// providerDescriptorBeforeSnapshotForTest marks descriptor admission before
	// the reconciliation lease so tests can queue an identity writer first.
	providerDescriptorBeforeSnapshotForTest func()
	markdownImages                          *markdownImageCache
	roborevRepositories                     *roborevRepositoryProbe

	// toolingStatus caches the assembled CLI tooling probe;
	// toolingRun overrides the probe subprocess runner in tests.
	toolingStatus toolingStatusCache
	toolingRun    toolingRunner

	daemonRequests daemonRequestPolicy
	federationAuth *federationauth.Authenticator

	// bg tracks short-lived goroutines that HTTP handlers spawn
	// outside of the Syncer's own wait group (e.g. mergePR's
	// post-failure refresh). Shutdown waits on bg before the
	// caller tears down the DB.
	//
	// bgMu guards shuttingDown, drainDone, and httpSrv, and
	// serializes bg.Add against Shutdown's bg.Wait so the
	// WaitGroup cannot observe Add racing with Wait when the
	// counter transiently hits zero.
	bgMu         sync.Mutex
	bg           sync.WaitGroup
	bgCtx        context.Context
	bgCancel     context.CancelFunc
	bgDeadline   *shutdownDeadline
	shuttingDown bool
	// drainDone is created the first time Shutdown is called and
	// closed when bg.Wait returns. Every caller waits on it
	// subject to its own ctx, so a retry with a longer deadline
	// observes true drain after an earlier caller's ctx expired.
	drainDone chan struct{}
	httpSrv   *http.Server
	// connWG tracks per-connection goroutines spawned by Serve.
	// Incremented from ConnState(StateNew), decremented from
	// ConnState(StateClosed|StateHijacked). Shutdown waits on it
	// after http.Server.Shutdown so that the deferred setState in
	// (*conn).serve finishes before tests tear down dependencies.
	connWG sync.WaitGroup

	// workspaceDependents tracks Fleet and repository-browser loops started
	// after Workspace. Root shutdown drains this group before stopping the
	// Workspace domain they consume.
	workspaceDependentsCtx    context.Context
	workspaceDependentsCancel context.CancelFunc
	workspaceDependentsWG     sync.WaitGroup
	workspaceDependentsDone   chan struct{}
	workspaceDependentsOnce   sync.Once
	workspaceLifecycleCtx     context.Context
	workspaceLifecycleCancel  context.CancelFunc
	workspaceDependencyStop   *workspaceDependencyShutdown
}

// trackHTTPConn is installed as http.Server.ConnState by Serve so
// Shutdown can wait for per-connection goroutines to fully unwind.
func (s *Server) trackHTTPConn(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.connWG.Add(1)
	case http.StateHijacked, http.StateClosed:
		s.connWG.Done()
	}
}

// Hub returns the server's SSE event hub. Callers should never
// retain the returned pointer beyond the server's lifetime.
func (s *Server) Hub() *EventHub { return s.hub }

// Fleet returns the composed Fleet service boundary.
func (s *Server) Fleet() *fleetapi.Handler { return s.fleetAPI }

// SubscriberCount returns the number of live SSE subscribers. Intended
// for tests that need to wait for a connection to register before
// broadcasting (broadcasts issued before subscription would otherwise
// race against the handler's Subscribe call).
func (s *Server) SubscriberCount() int { return s.hub.SubscriberCount() }

func (s *Server) subscribeWorkspaceEvents(
	ctx context.Context, injectCached bool,
) (<-chan workspaceapi.RecordedEvent, <-chan struct{}) {
	source, done := s.hub.Subscribe(ctx, injectCached)
	events := make(chan workspaceapi.RecordedEvent, cap(source))
	go func() {
		defer close(events)
		for event := range source {
			select {
			case events <- workspaceapi.RecordedEvent{
				ID:   event.ID,
				Type: event.Event.Type,
				Data: event.Event.Data,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, done
}

// SetBuildInfo sets the metadata returned by GET /api/v1/version.
func (s *Server) SetBuildInfo(info BuildInfo) { s.buildInfo = info }

// runBackground launches fn as a tracked goroutine. fn receives a
// context cancelled by Shutdown. If Shutdown has already started,
// runBackground drops the task: these goroutines are best-effort
// refreshes and starting one during drain would race with bg.Wait.
func (s *Server) runBackground(fn func(ctx context.Context)) bool {
	s.bgMu.Lock()
	if s.shuttingDown {
		s.bgMu.Unlock()
		return false
	}
	s.bg.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bg.Done()
		fn(s.bgCtx)
	}()
	return true
}

func (s *Server) runWorkspaceDependent(fn func(context.Context)) {
	if fn == nil {
		return
	}
	s.workspaceDependentsWG.Go(func() {
		fn(s.workspaceDependentsCtx)
	})
}

func (s *Server) stopWorkspaceDependents() <-chan struct{} {
	s.workspaceDependentsOnce.Do(func() {
		s.workspaceDependentsCancel()
		go func() {
			s.workspaceDependentsWG.Wait()
			close(s.workspaceDependentsDone)
		}()
	})
	return s.workspaceDependentsDone
}

// Shutdown stops the HTTP listener (if started via ListenAndServe
// or Serve), closes the SSE event hub so streaming handlers exit,
// drains later-started Workspace consumers, shuts Workspace down before its
// runtime dependency, cancels remaining background goroutines, and blocks
// until they finish or ctx expires. Safe to call concurrently and repeatedly.
// Every caller drives http.Server.Shutdown with its own ctx
// (stdlib polls idle-conn closure per call) and waits on a shared
// drain channel, so a retry with a longer deadline observes true
// drain for both HTTP handlers and the bg group. Only the first
// caller closes the hub and cancels bgCtx.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.pullLifecycle != nil {
		s.pullLifecycle.Stop()
	}
	s.bgMu.Lock()
	first := !s.shuttingDown
	if first {
		s.shuttingDown = true
		s.drainDone = make(chan struct{})
		if deadline, ok := ctx.Deadline(); ok {
			s.bgDeadline.tighten(deadline)
		}
	}
	drainDone := s.drainDone
	httpSrv := s.httpSrv
	s.bgMu.Unlock()

	// Close the hub first so handleSSE subscribers can exit on
	// their <-done select arm. Otherwise http.Server.Shutdown
	// below would wait on SSE handlers that never return until
	// client disconnect, hanging the shutdown until ctx expires.
	if first && s.hub != nil {
		s.hub.Close()
	}
	var httpErr error
	httpDrained := httpSrv == nil
	if httpSrv != nil {
		httpErr = httpSrv.Shutdown(ctx)
		// http.Server.Shutdown returns when active connections
		// become idle and are removed from its tracking map, but
		// the per-connection goroutine's deferred setState(Closed)
		// chain is still running on its way out. Wait for our
		// ConnState hook to observe the final state transition so
		// callers can safely tear down dependencies.
		connDone := make(chan struct{})
		go func() {
			s.connWG.Wait()
			close(connDone)
		}()
		select {
		case <-connDone:
		case <-ctx.Done():
			if httpErr == nil {
				httpErr = ctx.Err()
			}
		}
		httpDrained = httpErr == nil
	}

	if first {
		s.stopWorkspaceDependents()
		s.bgCancel()
		go func() {
			s.bg.Wait()
			close(drainDone)
		}()
	}
	if !httpDrained {
		return httpErr
	}
	return s.workspaceDependencyStop.Shutdown(ctx)
}

// SetActiveWorktreeKey sets the key of the currently
// focused worktree. Thread-safe.
func (s *Server) SetActiveWorktreeKey(key string) {
	s.activeWorktreeMu.Lock()
	s.activeWorktreeKey = key
	s.activeWorktreeSet = true
	s.activeWorktreeMu.Unlock()
}

// ActiveWorktreeKey returns the key of the currently
// focused worktree and whether it was explicitly set.
// Thread-safe.
func (s *Server) ActiveWorktreeKey() (string, bool) {
	s.activeWorktreeMu.Lock()
	defer s.activeWorktreeMu.Unlock()
	return s.activeWorktreeKey, s.activeWorktreeSet
}

// New creates a Server without config persistence.
// Pass cfg for repo filtering (can be nil for tests that
// don't need filtering).
func New(
	database *db.DB,
	syncer *ghclient.Syncer,
	frontend fs.FS,
	basePath string,
	cfg *config.Config,
	opts ServerOptions,
) *Server {
	return newServer(
		database, syncer, opts.Clones, frontend,
		basePath, cfg, "", opts,
	)
}

// NewWithConfig creates a Server with config persistence for
// settings/repo endpoints.
func NewWithConfig(
	database *db.DB,
	syncer *ghclient.Syncer,
	clones *gitclone.Manager,
	frontend fs.FS,
	cfg *config.Config,
	cfgPath string,
	opts ServerOptions,
) *Server {
	return newServer(
		database, syncer, clones, frontend,
		cfg.BasePath, cfg, cfgPath, opts,
	)
}

// hostCheckTestFallbackBindHost / Port define the bind used when
// server.New is called with cfg=nil AND no explicit
// ServerOptions.HostCheck. These match the defaults that come out
// of config.Load, so existing same-package tests work without
// per-test churn.
const (
	hostCheckTestFallbackBindHost = "127.0.0.1"
	hostCheckTestFallbackBindPort = "8091"
)

// testFallbackAllowedHosts is the allowlist applied alongside the
// fallback bind. httptest.NewRequest defaults the Host to
// "example.com" and the apitest helpers use "forge.test"; both
// must be accepted so the dozens of test helpers that pass
// cfg=nil work unchanged.
func testFallbackAllowedHosts() []config.HostKey {
	return []config.HostKey{
		{Host: "example.com", Port: ""},
		{Host: "forge.test", Port: ""},
	}
}

// allowUnvalidatedConfigHostCheckFallbackForTests is false in
// production. Same-package tests set it from _test.go so legacy
// partial config literals can exercise unrelated server behavior
// without manufacturing a full validated config.
var allowUnvalidatedConfigHostCheckFallbackForTests bool

// resolveHostCheckOptions applies the precedence rule:
// caller override > cfg-derived options > cfg=nil test-friendly
// fallback. For non-nil configs that bypassed config.Load, derive
// the bind and allowlist from the provided config fields so
// production callers do not silently inherit hard-coded host
// defaults.
func resolveHostCheckOptions(
	cfg *config.Config,
	override HostCheckOptions,
	allowLoopbackAnyPort bool,
) HostCheckOptions {
	opts, err := pickHostCheckOptions(cfg, override)
	if err != nil {
		panic(err)
	}
	if allowLoopbackAnyPort {
		opts.AllowLoopbackAnyPort = true
	}
	return opts
}

func pickHostCheckOptions(cfg *config.Config, override HostCheckOptions) (HostCheckOptions, error) {
	if override.Valid() {
		return override, nil
	}
	if cfg != nil {
		if k := cfg.BindHostKey(); k.Valid() {
			return HostCheckOptions{
				Bind:              k,
				Allowed:           cfg.ParsedAllowedHosts(),
				TrustReverseProxy: cfg.TrustReverseProxy,
			}, nil
		}
		opts, err := deriveHostCheckOptionsFromConfig(cfg)
		if err == nil {
			return opts, nil
		}
		if !allowUnvalidatedConfigHostCheckFallbackForTests {
			return HostCheckOptions{}, fmt.Errorf("server: config did not provide valid Host check options: %w", err)
		}
		return fallbackHostCheckOptions(), nil
	}
	slog.Warn(
		"server.New used without a cfg or explicit ServerOptions.HostCheck; using httptest-compatible Host defaults. Production callers must pass a validated config or explicit HostCheck options.",
	)
	return fallbackHostCheckOptions(), nil
}

func deriveHostCheckOptionsFromConfig(cfg *config.Config) (HostCheckOptions, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return HostCheckOptions{}, errors.New("host is empty")
	}
	if ip := net.ParseIP(cfg.Host); ip == nil {
		return HostCheckOptions{}, fmt.Errorf("config: invalid host %q", cfg.Host)
	} else if !ip.IsLoopback() {
		return HostCheckOptions{}, fmt.Errorf(
			"config: host %q is not loopback; only loopback addresses are supported",
			cfg.Host,
		)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return HostCheckOptions{}, fmt.Errorf("port %d is outside 1-65535", cfg.Port)
	}
	bind, err := config.ParseHostKey(net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)))
	if err != nil {
		return HostCheckOptions{}, fmt.Errorf("bind host %q: %w", cfg.ListenAddr(), err)
	}
	allowed := make([]config.HostKey, 0, len(cfg.AllowedHosts))
	for _, entry := range cfg.AllowedHosts {
		key, err := config.ParseHostKey(entry)
		if err != nil {
			return HostCheckOptions{}, fmt.Errorf("allowed_hosts entry %q: %w", entry, err)
		}
		allowed = append(allowed, key)
	}
	return HostCheckOptions{
		Bind:              bind,
		Allowed:           allowed,
		TrustReverseProxy: cfg.TrustReverseProxy,
	}, nil
}

func fallbackHostCheckOptions() HostCheckOptions {
	return HostCheckOptions{
		Bind: config.HostKey{
			Host: hostCheckTestFallbackBindHost,
			Port: hostCheckTestFallbackBindPort,
		},
		Allowed:              testFallbackAllowedHosts(),
		TrustReverseProxy:    false,
		AllowLoopbackAnyPort: true,
	}
}

func workspaceConfigSnapshot(
	cfg *config.Config, tmuxCommand []string,
) workspaceapi.ConfigSnapshot {
	snapshot := workspaceapi.ConfigSnapshot{
		TmuxCommand: slices.Clone(tmuxCommand), IssueBranchSlug: true,
	}
	if cfg == nil {
		return snapshot
	}
	snapshot.Agents = cloneConfigAgents(cfg.Agents)
	snapshot.AutoAssignOnCreate = cfg.Workspaces.AutoAssignOnCreate
	snapshot.RoborevInitManagedClones = cfg.Roborev.InitManagedClones
	snapshot.IssueBranchSlug = cfg.IssueWorkspaceBranchSlugEnabled()
	snapshot.KnownPlatformHosts = make(
		[]projects.KnownPlatformHost, 0, len(cfg.Platforms)+len(cfg.Repos)+1,
	)
	snapshot.KnownPlatformHosts = append(snapshot.KnownPlatformHosts, projects.KnownPlatformHost{
		Platform: string(platform.KindGitHub),
		Host:     cfg.DefaultPlatformHost,
	})
	for _, configured := range cfg.Platforms {
		snapshot.KnownPlatformHosts = append(snapshot.KnownPlatformHosts, projects.KnownPlatformHost{
			Platform: configured.Type,
			Host:     configured.Host,
		})
	}
	for _, repo := range cfg.Repos {
		snapshot.KnownPlatformHosts = append(snapshot.KnownPlatformHosts, projects.KnownPlatformHost{
			Platform: repo.PlatformOrDefault(),
			Host:     repo.PlatformHostOrDefault(),
		})
	}
	return snapshot
}

func kataConfigSnapshot(cfg *config.Config) kata.ConfigSnapshot {
	if cfg == nil {
		return kata.ConfigSnapshot{}
	}
	return kata.ConfigSnapshot{
		Repos:        slices.Clone(cfg.Repos),
		KataProjects: slices.Clone(cfg.KataProjects),
	}
}

func pullConfigSnapshot(cfg *config.Config) pullapi.ConfigSnapshot {
	if cfg == nil {
		return pullapi.ConfigSnapshot{}
	}
	return pullapi.ConfigSnapshot{
		AllowMidStackMerges:            cfg.PullRequests.AllowMidStackMerges,
		UseWorkspaceActivityForRecency: cfg.Activity.UseWorkspaceActivityForRecency,
	}
}

func issueConfigSnapshot(cfg *config.Config) issueapi.ConfigSnapshot {
	if cfg == nil {
		return issueapi.ConfigSnapshot{}
	}
	return issueapi.ConfigSnapshot{
		UseWorkspaceActivityForRecency: cfg.Activity.UseWorkspaceActivityForRecency,
	}
}

func fleetConfigSnapshot(cfg *config.Config, tmuxCommand []string) fleetapi.ConfigSnapshot {
	if cfg == nil {
		return fleetapi.ConfigSnapshot{TmuxCommand: slices.Clone(tmuxCommand)}
	}
	platformAuth := config.Config{
		GitHubTokenEnv:      cfg.GitHubTokenEnv,
		DefaultPlatformHost: cfg.DefaultPlatformHost,
		Repos:               slices.Clone(cfg.Repos),
		Platforms:           slices.Clone(cfg.Platforms),
		// Owner PATs and App installations are credential routes in their
		// own right: without them a repository served only by an owner
		// token resolves to no credential and Fleet reports the platform
		// backend as unauthenticated while sync and mutations work.
		GitHubOwnerTokens: slices.Clone(cfg.GitHubOwnerTokens),
		GitHubApps:        slices.Clone(cfg.GitHubApps),
	}
	return fleetapi.ConfigSnapshot{
		Fleet:               cfg.Fleet,
		PlatformAuthConfig:  platformAuth,
		PlatformAuthEnabled: true,
		TmuxCommand:         slices.Clone(tmuxCommand),
	}
}

// updateCatalogStripEnvVars widens every credential strip set with
// externally cataloged token env names (Kata daemon catalogs). All
// consumers accumulate monotonically, so stale catalog names only
// over-strip.
func (s *Server) updateCatalogStripEnvVars(names []string) {
	if len(names) == 0 {
		return
	}
	if s.workspaces != nil {
		s.workspaces.UpdateTmuxStripEnvVars(names)
	}
	if s.runtime != nil {
		s.runtime.UpdateStripEnvVars(names)
	}
	if s.ptyOwnerClient != nil {
		s.ptyOwnerClient.UpdateStripEnvVars(names)
	}
}

func (s *Server) applyWorkspaceConfigLocked() {
	if s.workspaceAPI != nil {
		s.workspaceAPI.ApplyConfig(workspaceConfigSnapshot(s.cfg, s.tmuxCmd))
	}
}

func (s *Server) applyFleetConfigLocked() {
	active := s.activeFleetConfigSnapshotLocked()
	if s.fleetAPI != nil {
		s.fleetAPI.ApplyConfig(active)
	}
	if s.hubEvents != nil {
		s.hubEvents.SetEnabled(active.Fleet.Enabled)
	}
	if s.spokeActivationLease != nil {
		s.spokeActivationLease.SetEnabled(active.Fleet.Enabled)
	}
}

func (s *Server) activeFleetConfigSnapshotLocked() fleetapi.ConfigSnapshot {
	snapshot := fleetConfigSnapshot(s.cfg, s.tmuxCmd)
	// A daemon that booted outside a fleet may activate federation only when
	// its startup request policy already required API authentication.
	snapshot.Fleet.Enabled = snapshot.Fleet.Enabled &&
		(s.fleetEnabledAtBoot || s.daemonRequests.requireAPIAuth)
	snapshot.Fleet.Role = s.bootCfgSnapshot.FleetRole
	snapshot.Fleet.BaseURL = s.bootCfgSnapshot.FleetBaseURL
	if s.bootCfgSnapshot.Hub == nil {
		snapshot.Fleet.Hub = nil
	} else {
		name := ""
		if snapshot.Fleet.Hub != nil {
			name = snapshot.Fleet.Hub.Name
		}
		snapshot.Fleet.Hub = &config.FleetHub{
			NodeID: s.bootCfgSnapshot.Hub.NodeID,
			Name:   name, BaseURL: s.bootCfgSnapshot.Hub.BaseURL,
		}
	}
	return snapshot
}

func (s *Server) applyKataConfigLocked() {
	if s.kataAPI != nil {
		s.kataAPI.ApplyConfig(kataConfigSnapshot(s.cfg))
	}
}

func (s *Server) applyPullConfigLocked() {
	if s.pullAPI != nil {
		s.pullAPI.ApplyConfig(pullConfigSnapshot(s.cfg))
	}
}

func (s *Server) applyIssueConfigLocked() {
	if s.issueAPI != nil {
		s.issueAPI.ApplyConfig(issueConfigSnapshot(s.cfg))
	}
}

func newServer(
	database *db.DB,
	syncer *ghclient.Syncer,
	clones *gitclone.Manager,
	frontend fs.FS,
	basePath string,
	cfg *config.Config,
	cfgPath string,
	options ServerOptions,
) *Server {
	mux := http.NewServeMux()

	bgBaseCtx, bgCancel := context.WithCancel(context.Background())
	bgDeadline := &shutdownDeadline{}
	hostOpts := resolveHostCheckOptions(
		cfg,
		options.HostCheck,
		options.HostCheckAllowLoopbackAnyPort,
	)
	deferredMergeMaxWait := options.deferredMergeMaxWait
	terminalClipboard := options.TerminalClipboard
	if terminalClipboard == nil {
		terminalClipboard = systemclipboard.NewWriter()
	}
	markdownImageDataDir := ""
	if cfg != nil {
		markdownImageDataDir = cfg.DataDir
	}
	var terminalPasteImages *terminalpaste.Store
	if markdownImageDataDir != "" {
		var err error
		terminalPasteImages, err = terminalpaste.NewStore(filepath.Join(
			markdownImageDataDir,
			"cache",
			"terminal-paste-images",
		))
		if err != nil {
			slog.Warn("initialize terminal paste image cache", "err", err)
		}
	}
	repoResolver := httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{
		DB: database,
		ProviderCapabilities: func(kind platform.Kind, host string) (platform.Capabilities, error) {
			if syncer == nil {
				return platform.Capabilities{}, errors.New("provider registry unavailable")
			}
			return syncer.ProviderCapabilities(kind, host)
		},
	})

	s := &Server{
		db:                     database,
		repoResolver:           repoResolver,
		basePath:               basePath,
		syncer:                 syncer,
		archive:                options.Archive,
		clones:                 clones,
		telemetry:              options.Telemetry,
		cfg:                    cfg,
		cfgPath:                cfgPath,
		tokenSources:           options.TokenSources,
		bootCfgSnapshot:        snapshotStartupConfig(cfg),
		fleetEnabledAtBoot:     cfg != nil && cfg.Fleet.Enabled,
		runtimeStripEnvVars:    initialRuntimeStripEnvNames(cfg),
		options:                options,
		daemonRequests:         newDaemonRequestPolicy(options.DaemonAccess),
		federationAuth:         federationauth.NewAuthenticator(options.FederationCredentials),
		now:                    time.Now,
		hub:                    NewEventHubWithCapacity(cfg.SSEBufferSizeOrDefault()),
		labelCatalogRefreshIDs: make(map[int64]struct{}),
		markdownImages:         newMarkdownImageCache(markdownImageCacheRoot(markdownImageDataDir)),
		bgCtx: shutdownAwareContext{
			parent:   bgBaseCtx,
			deadline: bgDeadline,
		},
		bgCancel:                bgCancel,
		bgDeadline:              bgDeadline,
		workspaceDependentsDone: make(chan struct{}),
	}
	s.providerWriteGate = options.ProviderWriteGate
	if s.providerWriteGate == nil {
		restoreDurableState := false
		if options.FederationEnrollments != nil {
			_, restoreDurableState = options.FederationEnrollments.Local()
		}
		s.providerWriteGate = providerplane.NewProviderWriteGate(database, restoreDurableState)
	}
	if cfg != nil && cfg.Fleet.RoleOrDefault() == config.FleetRoleSpoke {
		s.providerRouteSpoke = true
		s.providerSource = &hubProviderSource{
			db: database, clones: clones, enabled: s.federationEnabled,
		}
		if options.FederationSpokeActive &&
			options.MaintainFederationSpokeActivation != nil {
			s.spokeActivationLease = newHubEventLifecycleStoppingOnCleanReturn(
				cfg.Fleet.Enabled, options.MaintainFederationSpokeActivation,
			)
		}
		if options.FederationSpokeActive && cfg.Fleet.Hub != nil {
			client, err := providerplane.NewClient(providerplane.Options{
				LocalNodeID: options.FederationSpokeID,
				Hub: providerplane.Hub{
					NodeID:  cfg.Fleet.Hub.NodeID,
					BaseURL: cfg.Fleet.Hub.BaseURL,
				},
				Credentials: options.FederationCredentials,
				HTTPClient:  options.FederationHTTPClient,
			})
			if err != nil {
				slog.Error("configure hub provider client", "err", err)
			} else {
				s.providerSource.client = client
				s.providerProxy = newProviderProxy(client)
				events, eventsErr := providerplane.NewEventClient(providerplane.EventClientOptions{
					Client:              client,
					OnEvent:             s.receiveHubEvent,
					OnResync:            s.resynchronizeHubProviderState,
					OnConnectionChanged: s.broadcastHubConnection,
				})
				if eventsErr != nil {
					slog.Error("configure hub event client", "err", eventsErr)
				} else {
					s.hubEvents = newHubEventLifecycle(
						cfg.Fleet.Enabled, events.Run,
					)
				}
			}
		}
		if s.hubEvents == nil || !cfg.Fleet.Enabled {
			s.broadcastHubConnection(false)
		}
	}
	roborevConfig := cfg
	if roborevConfig == nil {
		roborevConfig = &config.Config{}
	}
	s.roborevRepositories = newRoborevRepositoryProbe(
		s.bgCtx,
		roborevConfig.RoborevEndpoint(),
		workspaceConfigSnapshot(cfg, nil).KnownPlatformHosts,
	)
	if syncer != nil {
		syncer.SetOnMergedActorRepaired(s.broadcastMergedActorDetailRefresh)
	}
	s.workspaceDependentsCtx, s.workspaceDependentsCancel = context.WithCancel(s.bgCtx)
	s.workspaceLifecycleCtx, s.workspaceLifecycleCancel = context.WithCancel(context.Background())
	workspaceNow := s.now
	if options.WorkspaceNow != nil {
		workspaceNow = options.WorkspaceNow
	}
	s.docsAPI = docsapi.New(docsapi.Deps{
		Config:   cfg,
		Registry: options.DocsRegistry,
		BeginConfigMutation: func() func() {
			s.configReloadMu.Lock()
			return s.configReloadMu.Unlock
		},
		SaveFolders: func(folders []config.DocFolder) error {
			if s.cfgPath == "" || s.cfg == nil {
				return docsapi.ErrSettingsUnavailable
			}
			s.cfgMu.Lock()
			defer s.cfgMu.Unlock()
			previous := slices.Clone(s.cfg.DocFolders)
			s.cfg.DocFolders = slices.Clone(folders)
			if err := s.cfg.Save(s.cfgPath); err != nil {
				s.cfg.DocFolders = previous
				return err
			}
			return nil
		},
	})
	if cfg != nil {
		docsapi.WarnDaemonBindings(cfg.DocFolders)
	}
	var repositoryDescriptorSource repobrowserapi.RepositoryDescriptorSource
	if s.providerSource != nil {
		repositoryDescriptorSource = s.providerSource
	}
	s.repoBrowserAPI = repobrowserapi.New(repobrowserapi.Deps{
		Resolver:         repoResolver,
		Clones:           clones,
		Config:           cfg,
		DescriptorSource: repositoryDescriptorSource,
	})
	s.hostOpts.Store(&hostOpts)
	if hostOpts.TrustReverseProxy && len(hostOpts.Allowed) == 0 {
		slog.Warn(
			"trust_reverse_proxy is enabled but allowed_hosts is empty; only loopback Hosts will be accepted",
		)
	}

	// (*Config).TmuxCommand handles a nil receiver and returns
	// config.DefaultTmuxCommand. Compute once so the workspace, runtime, and
	// terminal handler all share the same value and the nil-safety
	// of the call is explicit at this level.
	tmuxCmd := cfg.TmuxCommand()
	s.tmuxCmd = tmuxCmd
	hideTmuxStatus := false
	terminalGraphics := cfg.TerminalGraphicsEnabled()
	tmuxMouse := cfg.TerminalTmuxMouseEnabled()
	if cfg != nil {
		hideTmuxStatus = cfg.Terminal.HideTmuxStatus
	}
	tmuxAvailable := tmuxCommandAvailable(tmuxCmd)
	s.fleetAPI = fleetapi.New(fleetapi.Deps{
		DB:       database,
		Syncer:   syncer,
		Config:   fleetConfigSnapshot(cfg, tmuxCmd),
		BasePath: basePath,
		BuildVersion: func() string {
			return s.buildInfo.Version
		},
		Now: workspaceNow,
		LocalHandler: func() http.Handler {
			return s.handler
		},
		Broadcast: func(event fleetapi.Event) uint64 {
			return s.hub.Broadcast(Event{Type: event.Type, Data: event.Data})
		},
		Generation:      s.hub.Generation,
		SubscriberCount: s.hub.SubscriberCount,
		WorkspaceSnapshot: func(ctx context.Context) (workspaceapi.FleetSnapshot, error) {
			if s.workspaceAPI == nil {
				return workspaceapi.FleetSnapshot{}, nil
			}
			return s.workspaceAPI.FleetSnapshot(ctx)
		},
		WorkspaceStatsSnapshot: func(ctx context.Context) (workspaceapi.FleetSnapshot, error) {
			if s.workspaceAPI == nil {
				return workspaceapi.FleetSnapshot{}, nil
			}
			return s.workspaceAPI.FleetStatsSnapshot(ctx)
		},
		QueueWorkspaceDeletion: func(id string) error {
			if s.workspaceAPI == nil {
				return errors.New("workspace cleanup is unavailable")
			}
			return s.workspaceAPI.QueueWorkspaceDeletion(id)
		},
		RuntimeSnapshot: func(scope string) workspaceapi.RuntimeSnapshot {
			if s.workspaceAPI == nil {
				return nil
			}
			return s.workspaceAPI.RuntimeSnapshot(scope)
		},
		RevalidateDiffs: func() {
			if s.workspaceAPI != nil {
				s.workspaceAPI.RevalidateSelectedDiffs()
			}
		},
		NodeID:                      options.FederationSpokeID,
		FederationActive:            options.FederationSpokeActive,
		FederationUnavailableReason: options.FederationSpokeUnavailableReason,
		Credentials:                 options.FederationCredentials,
		Enrollments:                 options.FederationEnrollments,
		FederationHTTPClient:        options.FederationHTTPClient,
		PersistMember:               s.persistFleetMember,
		PersistHubBinding:           s.persistHubBinding,
		RemoveMember:                s.removeFleetMember,
		CancelEventStreams:          s.cancelFederationEventStreams,
	})
	var launchSpecResolver providerplane.WorkspaceLaunchSpecResolver = s
	var workspacePullCandidates workspace.PullCandidateSource
	if s.providerSource != nil {
		launchSpecResolver = s.providerSource
		workspacePullCandidates = s.providerSource
	}
	if options.WorktreeDir != "" {
		s.workspaces = workspace.NewManager(database, options.WorktreeDir)
		s.workspaces.SetNow(workspaceNow)
		s.workspaces.SetLaunchSpecResolver(launchSpecResolver)
		s.workspaces.SetRequireProviderCredential(s.providerRouteSpoke)
		s.workspaces.SetTmuxCommand(tmuxCmd)
		s.workspaces.UpdateTmuxStripEnvVars(s.runtimeStripEnvVars)
		s.workspaces.SetHideTmuxStatus(hideTmuxStatus)
		s.workspaces.SetTmuxGraphics(terminalGraphics)
		s.workspaces.SetTmuxMouse(tmuxMouse)
		s.workspaces.SetIssueBranchSlugEnabled(
			cfg.IssueWorkspaceBranchSlugEnabled(),
		)
		s.workspaces.SetRoborevEndpoint(roborevConfig.RoborevEndpoint())
		s.workspaces.SetRoborevRepositoryInvalidator(s.roborevRepositories.Invalidate)
		s.workspaces.SetWorktreeBasePathResolver(s.worktreeBasePathForRepo)
		ptyOwnerDir := options.PtyOwnerDir
		if ptyOwnerDir == "" {
			ptyOwnerDir = filepath.Join(
				filepath.Dir(options.WorktreeDir), "pty-owner",
			)
		}
		ptyOwnerClient := &ptyowner.Client{
			Root:        ptyOwnerDir,
			ExePath:     options.PtyOwnerExePath,
			ExeArgs:     append([]string(nil), options.PtyOwnerExeArgs...),
			ManagerPath: options.PtyOwnerManagerPath,
			Command:     append([]string(nil), options.PtyOwnerCommand...),
			// Configured token names must vanish from tmux-less base
			// terminals just like tmux-backed ones.
			StripEnvVars: slices.Clone(s.runtimeStripEnvVars),
			InProcess:    options.PtyOwnerInProcess,
		}
		s.ptyOwnerClient = ptyOwnerClient
		if preferPtyOwnerForWorkspaces(runtime.GOOS, tmuxAvailable, options) {
			s.workspaces.SetPtyOwnerClient(ptyOwnerClient)
		} else {
			s.workspaces.SetPtyOwnerFallbackClient(ptyOwnerClient)
		}
		if clones != nil {
			s.workspaces.SetClones(clones)
		}
		if tmuxAvailable {
			cleanupCtx, cleanupCancel := context.WithTimeout(
				context.Background(), startupTmuxCleanupTimeout,
			)
			if err := s.workspaces.ReapOrphanTmuxSessions(cleanupCtx); err != nil {
				slog.Warn("reap orphan tmux sessions", "err", err)
			}
			if _, err := s.workspaces.PruneMissingTmuxSessions(cleanupCtx); err != nil {
				slog.Warn("prune missing tmux sessions", "err", err)
			}
			cleanupCancel()
		}
		var agents []config.Agent
		if cfg != nil {
			agents = cfg.Agents
		}
		// Runtime sessions that are not tmux-backed must still be owned
		// outside the kenn-forge server process so restarts do not tear down
		// workspace terminal state. Tmux-backed sessions still attach via
		// tmux; the runtime manager only uses this owner for non-tmux starts.
		runtimePtyOwner := ptyownerruntime.New(ptyOwnerClient, nil)
		s.runtime = localruntime.NewManager(localruntime.Options{
			Targets: localruntime.ResolveLaunchTargets(
				agents, tmuxCmd, nil,
			),
			TmuxCommand:                    tmuxCmd,
			TmuxOwnerMarker:                s.workspaces.TmuxOwnerMarker(),
			WrapAgentSessionsInTmux:        cfg.TmuxAgentSessionsEnabled(),
			HideTmuxStatus:                 hideTmuxStatus,
			TmuxGraphics:                   terminalGraphics,
			TmuxMouse:                      tmuxMouse,
			StripEnvVars:                   s.runtimeStripEnvVars,
			ShellCommand:                   cfg.ShellCommand(),
			OnSessionExit:                  s.handleRuntimeSessionExit,
			PtyOwnerRuntime:                runtimePtyOwner,
			KnownPtyOwnerSessionKeys:       s.workspaces.RuntimeSessionKeysForWorkspace,
			DetachSessionsForServerRestart: options.DetachRuntimeSessionsForRestart,
		})
	}
	var providerWorkspaceAutomation workspaceapi.ProviderWorkspaceAutomation
	var mergeRequestWorktreeSource workspaceapi.MergeRequestWorktreeSource
	var resolveRepository func(
		context.Context, providerplane.RepositoryRoute,
	) (*db.Repo, error)
	if s.providerSource != nil {
		providerWorkspaceAutomation = s.providerSource
		mergeRequestWorktreeSource = s.providerSource
		if s.providerSource.client != nil {
			resolveRepository = s.providerSource.ResolveRepositoryRoute
		}
	}
	s.workspaceAPI = workspaceapi.New(workspaceapi.Deps{
		DB:                  database,
		Resolver:            repoResolver,
		Syncer:              syncer,
		Config:              workspaceConfigSnapshot(cfg, tmuxCmd),
		Workspaces:          s.workspaces,
		Runtime:             s.runtime,
		TerminalClipboard:   terminalClipboard,
		TerminalPasteImages: terminalPasteImages,
		AgentActivity: agentactivity.NewStore(filepath.Join(
			filepath.Dir(options.WorktreeDir), "agent-activity",
		)),
		TmuxCommand:        tmuxCmd,
		Now:                workspaceNow,
		EnrichmentDisabled: options.DisableWorkspaceEnrichment,
		Broadcast: func(event workspaceapi.Event) uint64 {
			return s.hub.Broadcast(Event{Type: event.Type, Data: event.Data})
		},
		Subscribe:                   s.subscribeWorkspaceEvents,
		Generation:                  s.hub.Generation,
		RecomputeWorktreeLinks:      s.fleetAPI.RecomputeWorktreeLinks,
		RefreshWorktreeStats:        s.fleetAPI.RefreshWorktreeStats,
		RefreshProjectInventory:     s.fleetAPI.RefreshProjectInventory,
		LookupRepo:                  repoResolver.LookupRoute,
		ResolveRepository:           resolveRepository,
		EnqueueDetailSync:           s.enqueueDetailSyncWithCompletion,
		ProviderWriteGate:           s.providerWriteGate,
		LaunchSpecResolver:          launchSpecResolver,
		PullCandidates:              workspacePullCandidates,
		ProviderWorkspaceAutomation: providerWorkspaceAutomation,
		MergeRequestWorktreeSource:  mergeRequestWorktreeSource,
	})
	s.kataAPI = kata.New(kata.Deps{
		DB:                     database,
		Resolver:               repoResolver,
		Config:                 kataConfigSnapshot(cfg),
		Workspaces:             s.workspaces,
		WorkspaceAPI:           s.workspaceAPI.Workspaces(),
		SamePlatformHost:       samePlatformHost,
		ConfigRepoPath:         configRepoPath,
		OnCatalogTokenEnvNames: s.updateCatalogStripEnvVars,
	})
	// Kata catalogs load lazily per request; feed their token env names
	// into stripping at boot too so terminals created before the first
	// Kata route never see cataloged credentials. Decoded-but-invalid
	// catalogs still carry their declared names, so apply them
	// regardless of the load error.
	bootCatalog, err := katacatalog.LoadCatalog()
	if err != nil {
		slog.Debug(
			"kata catalog boot load for credential stripping", "err", err,
		)
	}
	s.updateCatalogStripEnvVars(bootCatalog.TokenEnvNames())
	var pullProviderSource pullapi.ProviderSource
	var issueProviderSource issueapi.ProviderSource
	if s.providerSource != nil {
		pullProviderSource = s.providerSource
		issueProviderSource = s.providerSource
	}
	s.pullAPI = pullapi.New(pullapi.Deps{
		DB:                   database,
		Resolver:             repoResolver,
		Syncer:               syncer,
		Clones:               clones,
		Config:               pullConfigSnapshot(cfg),
		Now:                  func() time.Time { return s.now() },
		DeferredMergeMaxWait: deferredMergeMaxWait,
		QueueWorkspaceDeletion: func(
			ctx context.Context, hostKey, workspaceID string,
		) error {
			if hostKey == "" || hostKey == s.fleetAPI.SelfKey("") {
				return s.workspaceAPI.QueueWorkspaceDeletion(workspaceID)
			}
			return s.fleetAPI.RequestWorkspaceCleanup(ctx, hostKey, workspaceID)
		},
		WorkspaceSubjects: s.workspaceAPI.WorkspaceSubjectSnapshot,
		ViewerLogins:      s.resolveAuthenticatedViewerLogins,
		ProviderSource:    pullProviderSource,
		ProviderWriteGate: s.providerWriteGate,
		FleetSelfKey:      s.fleetAPI.SelfKey,
		FilterRepos: func(repos []db.Repo) []db.Repo {
			if s.cfg == nil {
				return repos
			}
			return s.filterConfiguredRepos(repos)
		},
		RepoOperations:                s.repoOperations,
		RepoOperationsForMergeRequest: s.repoOperationsForMergeRequest,
		EnqueueDetailSyncOrRerun:      s.enqueueDetailSyncOrRerun,
		Broadcast: func(event pullapi.Event) uint64 {
			return s.hub.Broadcast(Event{Type: event.Type, Data: event.Data})
		},
		MarkClosedLinkedNotificationsDone: s.markClosedLinkedNotificationsDone,
	})
	s.issueAPI = issueapi.New(issueapi.Deps{
		DB:                database,
		Resolver:          repoResolver,
		Syncer:            syncer,
		Now:               func() time.Time { return s.now() },
		Config:            issueConfigSnapshot(cfg),
		WorkspaceSubjects: s.workspaceAPI.WorkspaceSubjectSnapshot,
		ViewerLogins:      s.resolveAuthenticatedViewerLogins,
		ProviderSource:    issueProviderSource,
		FilterRepos: func(repos []db.Repo) []db.Repo {
			if s.cfg == nil {
				return repos
			}
			return s.filterConfiguredRepos(repos)
		},
		RepoOperations:                    s.repoOperations,
		MarkClosedLinkedNotificationsDone: s.markClosedLinkedNotificationsDone,
	})
	s.pullLifecycle = s.pullAPI
	s.workspaceDependencyStop = newWorkspaceDependencyShutdown(
		func(ctx context.Context) error {
			for _, done := range []<-chan struct{}{
				s.workspaceDependentsDone,
				s.drainDone,
			} {
				select {
				case <-done:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		func(ctx context.Context) error {
			if err := s.pullLifecycle.Shutdown(ctx); err != nil {
				return err
			}
			if err := s.fleetAPI.Shutdown(ctx); err != nil {
				return err
			}
			s.workspaceLifecycleCancel()
			return s.workspaceAPI.Shutdown(ctx)
		},
		func() {
			if s.runtime != nil {
				s.runtime.Shutdown()
			}
		},
	)
	if s.workspaces != nil {
		if err := s.workspaces.ApplyTmuxGraphics(context.Background()); err != nil {
			slog.Warn("apply startup tmux graphics setting", "err", err)
		}
		if err := s.workspaces.ApplyTmuxMouse(context.Background()); err != nil {
			slog.Warn("apply startup tmux mouse setting", "err", err)
		}
	}
	if err := s.workspaceAPI.RestoreRuntimeSessions(context.Background()); err != nil {
		slog.Warn("restore runtime tmux sessions", "err", err)
	}
	s.workspaceAPI.Start(s.workspaceLifecycleCtx, options.DisableWorkspaceBackgroundMonitors)
	s.fleetAPI.Start(
		s.workspaceLifecycleCtx,
		tmuxAvailable && s.workspaces != nil,
		options.DisableWorkspaceBackgroundMonitors,
	)
	if s.hubEvents != nil {
		s.runWorkspaceDependent(s.hubEvents.Run)
	}
	if s.spokeActivationLease != nil {
		s.runWorkspaceDependent(s.spokeActivationLease.Run)
	}
	if clones != nil {
		// Seed even when background refresh is disabled: startup also adopts
		// safe pre-stable-ID clone paths so cached reads survive an upgrade.
		s.repoBrowserAPI.SeedRefreshRepos(context.Background())
		if !options.DisableWorkspaceBackgroundMonitors {
			s.runWorkspaceDependent(s.repoBrowserAPI.RunRefreshLoop)
		}
	}

	// The syncer's native-stack preference is the transition authority for
	// later settings changes, so every server binds it to the boot config
	// rather than relying on the caller that assembled the syncer. This runs
	// before the config watcher so the boot value cannot race a reload that
	// swaps the preference and reconciles from its own snapshot.
	if syncer != nil && cfg != nil {
		syncer.SetPreferGitHubNativeStacks(cfg.PullRequests.PreferGitHubNativeStacks)
		if !cfg.PullRequests.PreferGitHubNativeStacks {
			// Boot is a transition point too: the setting may have been edited
			// while the daemon was stopped, or a previous run may have saved it and
			// exited before reconciling. Stored native ordering would otherwise
			// keep driving the merge safeguard until each repository next synced,
			// and forever for repositories no longer tracked.
			s.restoreBranchDerivedStackProjections()
		}
	}

	// Watch the config file so an external edit (vim, dotfiles deploy,
	// sd -i, etc.) is picked up without a restart. Watcher init failures
	// are logged inside startConfigWatcher; the server still serves.
	s.startConfigWatcher()

	healthAPI := humago.New(mux, healthAPIConfig())
	healthAPI.UseMiddleware(otelSpanMiddleware)
	s.registerHealthAPI(healthAPI)

	api := humago.NewWithPrefix(mux, "/api/v1", apiConfig(basePath))
	api.UseMiddleware(newResponseCompressionMiddleware(responseCompressionMinSize))
	api.UseMiddleware(otelSpanMiddleware)
	s.registerAPI(api)
	if s.workspaces != nil {
		s.registerTerminalAPI(api, tmuxCmd)
		wsAPI := humago.NewWithPrefix(mux, "/ws/v1", terminalAPIConfig())
		wsAPI.UseMiddleware(otelSpanMiddleware)
		s.registerTerminalAPI(wsAPI, tmuxCmd)
	}

	// Roborev proxy
	if cfg != nil {
		roborevAPI := humago.NewWithPrefix(
			mux, "/api", roborevProxyAPIConfig(),
		)
		s.registerRoborevProxyAPI(roborevAPI)
	}

	if frontend != nil {
		mux.Handle("/", newSPAAssetHandler(frontend, basePath, s.bootstrapScript))
	}

	// When serving under a base path, use an outer mux with
	// StripPrefix so the inner mux sees clean paths like /api/v1/...
	// Health endpoints stay at the root so external probes do not need
	// to know about the UI base path.
	var assembled http.Handler
	if basePath != "/" {
		outer := http.NewServeMux()
		prefix := strings.TrimSuffix(basePath, "/")
		outer.Handle("/healthz", mux)
		outer.Handle("/livez", mux)
		s.registerDaemonPing(outer)
		outer.Handle(basePath, stripPrefixPreservingPattern(prefix, mux))
		assembled = outer
	} else {
		s.registerDaemonPing(mux)
		assembled = mux
	}
	s.handler = otelhttp.NewHandler(assembled, "forge.http",
		otelhttp.WithFilter(otelTraceable(basePath)),
		otelhttp.WithSpanNameFormatter(otelSpanName))

	// Exact entries removed from the TOML file while the daemon was stopped
	// must release their hidden-from-UI preferences; boot restores tracked
	// refs from provider snapshots before the server is constructed, so
	// exact-owned preferences resolve and survive the sweep.
	if err := s.reconcileOrphanedRepoVisibility(s.bgCtx); err != nil {
		slog.Warn(
			"release orphaned hidden-from-UI preferences at startup",
			"err", err,
		)
	}

	return s
}

func (s *Server) handleRuntimeSessionExit(info localruntime.SessionInfo) {
	if info.WorkspaceID == hostRuntimeScope {
		if s.db == nil || info.TmuxSession == "" {
			return
		}
		s.runBackground(func(ctx context.Context) {
			cleanupCtx, cancel := context.WithTimeout(
				ctx, runtimeSessionCleanupTimeout,
			)
			defer cancel()
			// Generation-qualified: command session keys are reusable, so
			// this exit's cleanup must not delete the row of a newer live
			// session relaunched under the same key.
			if _, err := s.db.DeleteHostRuntimeTmuxSessionCreatedAt(
				cleanupCtx, info.Key, info.CreatedAt,
			); err != nil {
				slog.Warn(
					"forget host runtime tmux session",
					"session_key", info.Key,
					"tmux_session", info.TmuxSession,
					"err", err,
				)
			}
		})
		return
	}
	if worktreeID, ok := strings.CutPrefix(info.WorkspaceID, "project-worktree:"); ok {
		if worktreeID == "" || s.db == nil || info.TmuxSession == "" {
			return
		}
		s.runBackground(func(ctx context.Context) {
			cleanupCtx, cancel := context.WithTimeout(
				ctx, runtimeSessionCleanupTimeout,
			)
			defer cancel()
			// Generation-qualified: command session keys are reusable, so
			// this exit's cleanup must not delete the row of a newer live
			// session relaunched under the same key.
			if _, err := s.db.DeleteProjectWorktreeTmuxSessionCreatedAt(
				cleanupCtx, worktreeID, info.Key, info.CreatedAt,
			); err != nil {
				slog.Warn(
					"forget project worktree runtime tmux session",
					"worktree_id", worktreeID,
					"session_key", info.Key,
					"tmux_session", info.TmuxSession,
					"err", err,
				)
			}
		})
		return
	}
	if s.workspaceAPI != nil {
		s.workspaceAPI.HandleRuntimeSessionExit(info)
	}
}

func preferPtyOwnerForWorkspaces(
	runtimeGOOS string,
	tmuxAvailable bool,
	options ServerOptions,
) bool {
	if !tmuxAvailable {
		return true
	}
	return runtimeGOOS == "windows" &&
		(options.PtyOwnerManagerPath != "" || options.PtyOwnerExePath != "" ||
			options.PtyOwnerInProcess)
}

func tmuxCommandAvailable(command []string) bool {
	if len(command) == 0 || command[0] == "" {
		return false
	}
	_, err := exec.LookPath(command[0])
	return err == nil
}

func (s *Server) bootstrapScript() string {
	safeBase, _ := json.Marshal(s.basePath)
	var builder strings.Builder
	builder.WriteString(`window.__BASE_PATH__=`)
	builder.WriteString(scriptSafe(string(safeBase)))
	builder.WriteString(`;`)
	// The served config carries the daemon-side UI state thin
	// clients set over the API (PUT /api/v1/ui/active-worktree);
	// presentation preferences (embed mode, theming) are injected
	// client-side by whoever hosts the webview.
	if awKey, set := s.ActiveWorktreeKey(); set {
		configJSON, _ := json.Marshal(map[string]any{
			"ui": map[string]any{"activeWorktreeKey": awKey},
		})
		builder.WriteString(`window.__kenn_forge_config=`)
		builder.WriteString(scriptSafe(string(configJSON)))
		builder.WriteString(`;`)
	}
	return builder.String()
}

// scriptSafe escapes sequences that could break out of an inline
// <script> block. Replaces "</" with "<\/" so that payloads
// containing "</script>" cannot close the tag early.
func scriptSafe(s string) string {
	return strings.ReplaceAll(s, "</", `<\/`)
}

// ServeHTTP implements http.Handler so Server can be used directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logged := &statusLoggingResponseWriter{ResponseWriter: w}
	w = logged
	start := time.Now()
	slog.Debug(
		"http request started",
		"method", r.Method,
		"path", r.URL.Path,
		"query", redactedQuery(r.URL),
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)
	defer func() {
		status := logged.status
		if status == 0 {
			status = http.StatusOK
		}
		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"query", redactedQuery(r.URL),
			"status", status,
			"duration", time.Since(start).String(),
			"bytes", logged.bytes,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		}
		if status >= http.StatusBadRequest {
			slog.Warn("http request failed", args...)
		} else {
			slog.Debug("http request completed", args...)
		}
	}()
	hostOpts := *s.hostOpts.Load()
	admission := s.daemonRequests.admit(
		w, r, hostOpts, s.isGatedAPIRequest(r),
	)
	if admission.handled {
		return
	}
	if !admission.bypassProxyHostCheck && !checkHost(w, r, hostOpts) {
		return
	}
	if !s.checkHost(w, r) {
		return
	}
	if s.daemonRequests.requireAPIAuth {
		if s.handleAuthBootstrap(w, r) {
			return
		}
		if s.isGatedAPIRequest(r) && !s.authorizeAPIRequest(w, r) {
			return
		}
	}
	if r.Method != http.MethodGet && s.isMutatingAPIRequest(r) {
		if !checkCrossOrigin(w, r, hostOpts.TrustReverseProxy) {
			return
		}
		if s.isMutatingDocsAPIRequest(r) && !isLoopbackRemoteAddr(r.RemoteAddr) {
			writeProblemResponse(w, httpapi.NewProblem(
				http.StatusForbidden,
				httpapi.CodeForbidden,
				"docs mutations require a loopback client",
				map[string]any{"reason": "loopbackOnly"},
			))
			return
		}
		if s.isTerminalClipboardAPIRequest(r) &&
			!isLocalTerminalClipboardRequest(r, hostOpts.TrustReverseProxy) {
			writeProblemResponse(w, httpapi.NewProblem(
				http.StatusForbidden,
				httpapi.CodeForbidden,
				"terminal clipboard writes require a local client",
				map[string]any{"reason": "loopbackOnly"},
			))
			return
		}
	}
	if r.Method == http.MethodGet && s.isDocsBrowseAPIRequest(r) && !isLoopbackRemoteAddr(r.RemoteAddr) {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusForbidden,
			httpapi.CodeForbidden,
			"docs browse requires a loopback client",
			map[string]any{"reason": "loopbackOnly"},
		))
		return
	}
	if r.Method == http.MethodGet && s.isDocsReadAPIRequest(r) && !isLoopbackRemoteAddr(r.RemoteAddr) {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusForbidden,
			httpapi.CodeForbidden,
			"docs reads require a loopback client",
			map[string]any{"reason": "loopbackOnly"},
		))
		return
	}
	if release, handled := s.admitProviderWrite(w, r); handled {
		return
	} else if release != nil {
		defer release()
	}
	if s.serveProviderRoute(w, r) {
		return
	}
	s.handler.ServeHTTP(w, r)
}

func (s *Server) serveProviderRoute(w http.ResponseWriter, r *http.Request) bool {
	if !s.providerRouteSpoke {
		return false
	}
	canonicalPath := s.canonicalAPIPath(r)
	rule, ok := providerRouteRuleForRequest(r.Method, canonicalPath)
	if !ok || rule.Owner != ProviderHubOnly {
		return false
	}
	if !s.federationEnabled() || s.providerProxy == nil {
		writeProblemResponse(w, httpapi.HubUnavailable(
			"provider data is unavailable because the federation hub cannot be reached",
		))
		return true
	}
	request := r.Clone(r.Context())
	requestURL := *r.URL
	requestURL.Path = r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		requestURL.Path = strings.TrimPrefix(requestURL.Path, prefix)
	}
	requestURL.RawPath = canonicalPath
	request.URL = &requestURL
	s.providerProxy.ServeHTTP(w, request, rule)
	return true
}

func (s *Server) federationEnabled() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg == nil ||
		s.cfg.Fleet.Enabled &&
			(s.fleetEnabledAtBoot || s.daemonRequests.requireAPIAuth)
}

func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) bool {
	s.allowedHostMu.RLock()
	allowedHosts := s.allowedHosts
	s.allowedHostMu.RUnlock()
	return checkListenerHost(w, r, allowedHosts)
}

func checkListenerHost(
	w http.ResponseWriter,
	r *http.Request,
	allowedHosts map[string]struct{},
) bool {
	if len(allowedHosts) == 0 {
		return true
	}
	if !authorityIsLoopbackHost(r.Host) || isLoopbackRemoteAddr(r.RemoteAddr) {
		return true
	}
	writeProblemResponse(w, httpapi.NewProblem(
		http.StatusForbidden,
		httpapi.CodeForbidden,
		"host is not allowed",
		map[string]any{"reason": "hostNotAllowed"},
	))
	return false
}

// isMutatingAPIRequest checks whether the request targets an API route,
// accounting for the configured basePath prefix.
func (s *Server) isMutatingAPIRequest(r *http.Request) bool {
	path := r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		path = strings.TrimPrefix(path, prefix)
	}
	return strings.HasPrefix(path, "/api/")
}

func (s *Server) isMutatingDocsAPIRequest(r *http.Request) bool {
	path := r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		path = strings.TrimPrefix(path, prefix)
	}
	return strings.HasPrefix(path, "/api/v1/docs/")
}

func (s *Server) isTerminalClipboardAPIRequest(r *http.Request) bool {
	path := r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		path = strings.TrimPrefix(path, prefix)
	}
	return path == "/api/v1/terminal/clipboard"
}

func (s *Server) isDocsBrowseAPIRequest(r *http.Request) bool {
	path := r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		path = strings.TrimPrefix(path, prefix)
	}
	return path == "/api/v1/docs/browse"
}

func (s *Server) isDocsReadAPIRequest(r *http.Request) bool {
	path := r.URL.Path
	if s.basePath != "/" {
		prefix := strings.TrimSuffix(s.basePath, "/")
		path = strings.TrimPrefix(path, prefix)
	}
	if path == "/api/v1/docs/folders" || path == "/api/v1/docs/search" {
		return true
	}
	if !strings.HasPrefix(path, "/api/v1/docs/folders/") {
		return false
	}
	return strings.HasSuffix(path, "/tree") ||
		strings.HasSuffix(path, "/git") ||
		strings.HasSuffix(path, "/git/changes") ||
		strings.HasSuffix(path, "/file") ||
		strings.HasSuffix(path, "/blob") ||
		strings.HasSuffix(path, "/search")
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authorityIsLoopbackHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkCrossOrigin rejects cross-origin browser requests. Returns true if
// the request is allowed, false if it was rejected (response written).
func checkCrossOrigin(w http.ResponseWriter, r *http.Request, trustReverseProxy bool) bool {
	request := r
	if trustReverseProxy {
		// Host validation has already accepted the forwarded public authority.
		// Use it for the Origin comparison instead of the proxy's backend Host.
		publicHost := ""
		if values := r.Header.Values("X-Forwarded-Host"); len(values) > 0 {
			if key, err := parseXForwardedHost(strings.Join(values, ",")); err == nil {
				publicHost = key.String()
			}
		} else if values := r.Header.Values("Forwarded"); len(values) > 0 {
			if key, err := parseForwardedHost(strings.Join(values, ",")); err == nil {
				publicHost = key.String()
			}
		}
		if publicHost != "" {
			request = r.Clone(r.Context())
			request.Host = publicHost
		}
	}
	if err := crossOriginProtection.Check(request); err != nil {
		writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
		return false
	}
	return true
}

// ListenAndServe starts the HTTP server on addr. Returns
// http.ErrServerClosed when stopped by Shutdown (matches net/http).
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts HTTP connections on the provided listener. Useful
// for tests and any caller that wants to own the listener lifetime.
// Returns http.ErrServerClosed when stopped by Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	s.setAllowedHostsForListener(ln)
	s.adoptListenerHostPort(ln)
	srv := &http.Server{
		Handler:     s,
		ReadTimeout: 15 * time.Second,
		// WriteTimeout is 0 (disabled) because the roborev
		// proxy streams SSE/NDJSON responses that are
		// long-lived by design. A non-zero value would kill
		// /api/roborev/api/stream/events and /api/job/log
		// after the deadline.
		IdleTimeout: 60 * time.Second,
		ConnState:   s.trackHTTPConn,
	}

	s.bgMu.Lock()
	if s.shuttingDown {
		s.bgMu.Unlock()
		_ = ln.Close()
		return http.ErrServerClosed
	}
	s.httpSrv = srv
	s.bgMu.Unlock()

	return srv.Serve(ln)
}

// AttachHTTPServer records an externally-started HTTP server so Shutdown can
// close the listener after a startup handler has been swapped to this Server.
func (s *Server) AttachHTTPServer(srv *http.Server, ln net.Listener) {
	s.setAllowedHostsForListener(ln)
	s.adoptListenerHostPort(ln)
	s.bgMu.Lock()
	s.httpSrv = srv
	s.bgMu.Unlock()
}

// adoptListenerHostPort repoints the Host-check bind at the listener's actual
// authority. Besides kernel-assigned ports, this normalizes IP literals to the
// form net/http places in direct request Host headers.
func (s *Server) adoptListenerHostPort(ln net.Listener) {
	opts := *s.hostOpts.Load()
	bind, ok := listenerHostKey(ln)
	if !ok {
		return
	}
	opts.Bind = bind
	s.hostOpts.Store(&opts)
}

func (s *Server) setAllowedHostsForListener(ln net.Listener) {
	allowed := allowedHostsForListener(ln)
	s.allowedHostMu.Lock()
	s.allowedHosts = allowed
	s.allowedHostMu.Unlock()
}

func allowedHostsForListener(ln net.Listener) map[string]struct{} {
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil
	}
	out := map[string]struct{}{}
	for _, h := range []string{host, "127.0.0.1", "localhost", "::1"} {
		out[strings.ToLower(net.JoinHostPort(h, port))] = struct{}{}
	}
	return out
}

// handleSSE streams server events to a client. The handler subscribes
// to the EventHub and forwards each broadcast as an SSE frame. It exits
// when the client disconnects, when the hub closes, when the subscriber
// is evicted (slow consumer), or when context is canceled.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	// Clear server-wide WriteTimeout for this SSE response
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	cursor, hasCursor := parseLastEventID(r)
	s.serveSSE(r.Context(), w, rc, cursor, hasCursor)
}

func (s *Server) streamEvents(
	_ context.Context, input *streamEventsInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{
		Body: func(ctx huma.Context) {
			ctx.SetHeader("Content-Type", "text/event-stream")
			ctx.SetHeader("Cache-Control", "no-cache")
			ctx.SetHeader("Connection", "keep-alive")

			r, w := humago.Unwrap(ctx)
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Time{})
			cursor, hasCursor := parseLastEventID(r)
			ch, done := s.hub.Subscribe(ctx.Context(), !hasCursor)
			releaseSelection := func() {}
			if input.WorkspaceID != "" && s.workspaceAPI != nil {
				releaseSelection = s.workspaceAPI.SelectWorkspaceDiff(input.WorkspaceID)
			}
			defer releaseSelection()
			s.serveSSESubscribed(ctx.Context(), w, rc, cursor, hasCursor, ch, done)
		},
	}, nil
}

type sseController interface {
	SetWriteDeadline(time.Time) error
	Flush() error
}

// parseLastEventID inspects an incoming SSE request for a reconnect
// cursor. The Last-Event-ID header takes priority (HTML5 EventSource
// emits it automatically on reconnect); the since= query parameter is
// the fallback for non-browser callers and explicit first-connect
// resumption. Returns (0, false) when no usable cursor is present, so
// the handler can fall back to the no-cursor path (live + cached
// sync_status) without further branching.
func parseLastEventID(r *http.Request) (uint64, bool) {
	candidates := []string{r.Header.Get("Last-Event-ID")}
	if q := r.URL.Query().Get("since"); q != "" {
		candidates = append(candidates, q)
	}
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			slog.Debug("sse: ignoring unparseable cursor", "value", raw, "err", err)
			continue
		}
		return n, true
	}
	return 0, false
}

func (s *Server) serveSSE(
	ctx context.Context,
	w io.Writer,
	rc sseController,
	cursor uint64,
	hasCursor bool,
) {
	// Subscribe BEFORE the first flush so any broadcast issued between
	// the headers landing on the wire and the subscriber being registered
	// is delivered to this client instead of dropped. When a cursor is
	// supplied the handler replays the ring directly, so cached
	// sync_status injection by Subscribe would duplicate; pass false.
	ch, done := s.hub.Subscribe(ctx, !hasCursor)
	s.serveSSESubscribed(ctx, w, rc, cursor, hasCursor, ch, done)
}

func (s *Server) serveSSESubscribed(
	ctx context.Context,
	w io.Writer,
	rc sseController,
	cursor uint64,
	hasCursor bool,
	ch <-chan RecordedEvent,
	done <-chan struct{},
) {
	serveSSESubscribedFromHub(
		ctx,
		w,
		rc,
		s.hub,
		cursor,
		hasCursor,
		ch,
		done,
		func(uint64) Event {
			return s.reconnectStaleEvent()
		},
	)
}

func serveSSESubscribedFromHub(
	ctx context.Context,
	w io.Writer,
	rc sseController,
	hub *EventHub,
	cursor uint64,
	hasCursor bool,
	ch <-chan RecordedEvent,
	done <-chan struct{},
	staleEvent func(uint64) Event,
) {
	serveSSESubscribedFromHubTransformed(
		ctx, w, rc, hub, cursor, hasCursor, ch, done, staleEvent,
		func(rec RecordedEvent) (RecordedEvent, bool) { return rec, true },
		nil,
		nil,
	)
}

type sseReplaySnapshot struct {
	records []RecordedEvent
	staleID uint64
	stale   bool
}

func serveSSESubscribedFromHubTransformed(
	ctx context.Context,
	w io.Writer,
	rc sseController,
	hub *EventHub,
	cursor uint64,
	hasCursor bool,
	ch <-chan RecordedEvent,
	done <-chan struct{},
	staleEvent func(uint64) Event,
	transform func(RecordedEvent) (RecordedEvent, bool),
	afterReplay func(io.Writer, sseController) bool,
	preparedReplay *sseReplaySnapshot,
) {

	if err := rc.Flush(); err != nil {
		return
	}

	// Resolve the replay path before entering the live loop so the
	// client sees missed events (or a stale signal) before any new
	// live broadcasts and never out of order with them.
	deliveredThrough := cursor
	if hasCursor {
		replay, synID, stale := []RecordedEvent(nil), uint64(0), false
		if preparedReplay == nil {
			replay, synID, stale = hub.ReplaySnapshotSince(cursor)
		} else {
			replay = preparedReplay.records
			synID = preparedReplay.staleID
			stale = preparedReplay.stale
		}
		if stale {
			if !writeSSERecorded(w, rc, RecordedEvent{ID: synID, Event: staleEvent(synID)}) {
				return
			}
			deliveredThrough = synID
		} else {
			for _, rec := range replay {
				deliveredThrough = rec.ID
				transformed, ok := transform(rec)
				if !ok {
					continue
				}
				if !writeSSERecorded(w, rc, transformed) {
					return
				}
			}
		}
	}
	if afterReplay != nil && !afterReplay(w, rc) {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		// Non-blocking done check
		select {
		case <-done:
			return
		default:
		}

		select {
		case <-done:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if hasCursor && ev.ID <= deliveredThrough {
				// Already replayed; skip the duplicate that arrived
				// via the cached-status pre-load or a race between
				// the snapshot read and a fresh broadcast.
				continue
			}
			deliveredThrough = ev.ID
			transformed, include := transform(ev)
			if !include {
				continue
			}
			if !writeSSERecorded(w, rc, transformed) {
				return
			}
		case <-ticker.C:
			if err := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			if err := rc.SetWriteDeadline(time.Time{}); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// writeSSERecorded serializes a recorded event and writes it as a
// framed SSE frame. Returns true on success, false if any write or
// flush failed and the handler should exit.
func writeSSERecorded(w io.Writer, rc sseController, rec RecordedEvent) bool {
	data, err := json.Marshal(rec.Event.Data)
	if err != nil {
		slog.Error("sse: marshal event", "type", rec.Event.Type, "err", err)
		// Skip the unmarshalable event but keep streaming.
		return true
	}
	return writeSSEFrame(w, rc, rec.ID, rec.Event.Type, data)
}

func writeSSEFrame(
	w io.Writer, rc sseController, id uint64, eventType string, data []byte,
) bool {
	if err := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return false
	}
	if _, err := fmt.Fprintf(
		w, "id: %d\nevent: %s\ndata: %s\n\n", id, eventType, data,
	); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil {
		return false
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		return false
	}
	return true
}

func (s *Server) getVersion(
	_ context.Context, _ *struct{},
) (*versionOutput, error) {
	resp := &versionOutput{}
	resp.Body = versionOutputBody(s.buildInfo)
	return resp, nil
}

// writeJSON encodes v as JSON and writes it with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
