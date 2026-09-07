package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/configwatch"
	ghclient "go.kenn.io/forge/internal/github"
	katacatalog "go.kenn.io/forge/internal/kata"
	"go.kenn.io/forge/internal/server/docsapi"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

// configChangedEvent is the payload broadcast on the SSE "config.changed"
// channel. A late subscriber receives the most recently broadcast event so
// it can detect a stale-config state even if it connected after the parse
// error landed.
type configChangedEvent struct {
	// Valid reports whether the reloaded file parsed and validated. The
	// daemon keeps the previous in-memory config when Valid is false.
	Valid bool `json:"valid"`
	// Error is set when Valid is false; it contains a sanitized message
	// derived from the config parser/validator.
	Error string `json:"error,omitempty"`
	// RestartRequired is true when one or more startup-bound fields
	// (listener address, base path, sync interval, data dir, token env
	// names, platform registry, tmux/shell command, etc.) differ from
	// the boot-time snapshot. Hot-reloadable fields (repos, activity,
	// terminal, agents, docs) are applied regardless.
	RestartRequired bool `json:"restart_required"`
}

// startupConfigSnapshot is a deep copy of the fields the server binds at
// startup. It is taken once in newServer and compared in applyConfigChange
// to detect drift that the watcher cannot fix without a restart.
type startupConfigSnapshot struct {
	SyncInterval                    string
	NotificationSyncInterval        string
	NotificationPropagationInterval string
	NotificationBatchSize           int
	DefaultPlatformHost             string
	Host                            string
	Port                            int
	MCP                             config.MCP
	BasePath                        string
	DataDir                         string
	SyncBudgetPerHour               int
	AllowedHosts                    []config.HostKey
	TrustReverseProxy               bool
	ProviderHosts                   []tokenauth.Key
	PlatformTransports              []startupPlatformTransport
	// GitHubCredentialRoutes records the scoped client routes built at startup.
	// Adding, removing, or changing a route descriptor requires rebuilding the
	// bounded client pool and re-resolving authenticated identity. Token values
	// may still rotate underneath an unchanged env/file descriptor.
	GitHubCredentialRoutes []tokenauth.Descriptor
	// GitHubArchiveCredentialRoutes records the dedicated archive client routes
	// built at startup. Archive clients and their quota trackers are also
	// startup-bound, so archive App changes require a restart.
	GitHubArchiveCredentialRoutes []tokenauth.Descriptor
	// GitHubAppSplitHosts lists hosts whose effective credential chain
	// resolves sync reads through a GitHub App installation token.
	// Split topology is startup-bound: write rate trackers and the
	// write-credential clients are wired in buildProviderStartup, so a
	// reload that adds or removes an app for a host must flag a
	// restart instead of leaving mutation availability gating on the
	// wrong bucket.
	GitHubAppSplitHosts []string
	// TokenEnvNames is the boot-time baseline of provider token env
	// names used to accumulate runtime strip-env lists; it is not
	// compared for restart-required drift.
	TokenEnvNames   []string
	RoborevEndpoint string
	Tmux            config.Tmux
	Shell           config.Shell
	FleetSessions   config.FleetSessions
	FleetRole       config.FleetRole
	FleetBaseURL    string
	Hub             *fleetHubStartupBinding
	RequireAuth     bool
	TailscaleServe  config.TailscaleServeAPI
}

type fleetHubStartupBinding struct {
	NodeID  string
	BaseURL string
}

type startupPlatformTransport struct {
	Platform      string
	Host          string
	BaseURL       string
	AllowInsecure bool
}

func snapshotStartupConfig(cfg *config.Config) startupConfigSnapshot {
	if cfg == nil {
		return startupConfigSnapshot{}
	}
	snap := startupConfigSnapshot{
		SyncInterval:                    cfg.SyncInterval,
		NotificationSyncInterval:        cfg.Notifications.SyncInterval,
		NotificationPropagationInterval: cfg.Notifications.PropagationInterval,
		NotificationBatchSize:           cfg.Notifications.BatchSize,
		DefaultPlatformHost:             cfg.DefaultPlatformHost,
		Host:                            cfg.Host,
		Port:                            cfg.Port,
		MCP:                             cfg.MCP,
		BasePath:                        cfg.BasePath,
		DataDir:                         cfg.DataDir,
		SyncBudgetPerHour:               cfg.SyncBudgetPerHour,
		AllowedHosts:                    startupAllowedHosts(cfg),
		TrustReverseProxy:               cfg.TrustReverseProxy,
		ProviderHosts:                   startupProviderHosts(cfg),
		PlatformTransports:              startupPlatformTransports(cfg),
		GitHubCredentialRoutes:          githubCredentialRoutes(cfg),
		GitHubArchiveCredentialRoutes:   githubArchiveCredentialRoutes(cfg),
		GitHubAppSplitHosts:             githubAppSplitHosts(cfg),
		RoborevEndpoint:                 cfg.RoborevEndpoint(),
	}
	snap.Tmux.Command = slices.Clone(cfg.Tmux.Command)
	if cfg.Tmux.AgentSessions != nil {
		v := *cfg.Tmux.AgentSessions
		snap.Tmux.AgentSessions = &v
	}
	snap.Shell.Command = slices.Clone(cfg.Shell.Command)
	snap.TokenEnvNames = startupBoundTokenEnvNames(cfg)
	// API auth, private-ingress policy, fleet identity, and session monitoring
	// are wired at startup, so edits require a restart.
	snap.FleetSessions = cfg.Fleet.Sessions
	snap.FleetRole = cfg.Fleet.RoleOrDefault()
	snap.FleetBaseURL = cfg.Fleet.BaseURL
	if cfg.Fleet.Hub != nil {
		snap.Hub = &fleetHubStartupBinding{
			NodeID:  cfg.Fleet.Hub.NodeID,
			BaseURL: cfg.Fleet.Hub.BaseURL,
		}
	}
	snap.RequireAuth = cfg.API.RequireAuth
	snap.TailscaleServe = cfg.API.TailscaleServe
	snap.TailscaleServe.AllowedUsers = slices.Clone(
		cfg.API.TailscaleServe.AllowedUsers,
	)
	return snap
}

func startupPlatformTransports(cfg *config.Config) []startupPlatformTransport {
	if cfg == nil {
		return nil
	}
	transports := make([]startupPlatformTransport, 0, len(cfg.Platforms))
	for _, configured := range cfg.Platforms {
		transports = append(transports, startupPlatformTransport{
			Platform:      configured.Type,
			Host:          configured.Host,
			BaseURL:       configured.BaseURL,
			AllowInsecure: configured.AllowInsecure,
		})
	}
	slices.SortFunc(transports, func(a, b startupPlatformTransport) int {
		if cmp := strings.Compare(a.Platform, b.Platform); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Host, b.Host)
	})
	return transports
}

func startupAllowedHosts(cfg *config.Config) []config.HostKey {
	if cfg == nil {
		return nil
	}
	allowed := cfg.ParsedAllowedHosts()
	slices.SortFunc(allowed, func(a, b config.HostKey) int {
		if cmp := strings.Compare(a.Host, b.Host); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Port, b.Port)
	})
	return allowed
}

func startupProviderHosts(cfg *config.Config) []tokenauth.Key {
	if cfg == nil {
		return nil
	}
	seen := make(map[tokenauth.Key]struct{}, len(cfg.Platforms)+len(cfg.Repos)+1)
	out := make([]tokenauth.Key, 0, len(cfg.Platforms)+len(cfg.Repos)+1)
	add := func(platformName, host string) {
		key := tokenauth.Key{Platform: platformName, Host: host}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	for _, p := range cfg.Platforms {
		add(p.Type, p.Host)
	}
	for _, r := range cfg.Repos {
		add(r.PlatformOrDefault(), r.PlatformHostOrDefault())
	}
	add(string(platform.KindGitHub), platform.DefaultGitHubHost)
	slices.SortFunc(out, func(a, b tokenauth.Key) int {
		if cmp := strings.Compare(a.Platform, b.Platform); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Host, b.Host)
	})
	return out
}

func githubCredentialRoutes(cfg *config.Config) []tokenauth.Descriptor {
	if cfg == nil {
		return nil
	}
	seen := make(map[tokenauth.Key]struct{})
	var routes []tokenauth.Descriptor
	for _, plan := range cfg.ProviderTokenSources() {
		key := plan.Descriptor.Key
		if key.Platform != string(platform.KindGitHub) || key.Scope == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		routes = append(routes, plan.Descriptor)
	}
	slices.SortFunc(routes, func(a, b tokenauth.Descriptor) int {
		if cmp := strings.Compare(a.Key.Host, b.Key.Host); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Key.Scope, b.Key.Scope)
	})
	return routes
}

func githubArchiveCredentialRoutes(cfg *config.Config) []tokenauth.Descriptor {
	if cfg == nil {
		return nil
	}
	seen := make(map[tokenauth.Key]struct{})
	var routes []tokenauth.Descriptor
	for _, plan := range cfg.ProviderTokenSources() {
		key := plan.ArchiveDescriptor.Key
		if key.Platform != string(platform.KindGitHub) || key.Scope == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		routes = append(routes, plan.ArchiveDescriptor)
	}
	slices.SortFunc(routes, func(a, b tokenauth.Descriptor) int {
		if cmp := strings.Compare(a.Key.Host, b.Key.Host); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Key.Scope, b.Key.Scope)
	})
	return routes
}

func startupBoundTokenEnvNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	names := cfg.TokenEnvNames()
	slices.Sort(names)
	return names
}

func initialRuntimeStripEnvNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	names := cfg.TokenEnvNames()
	slices.Sort(names)
	return names
}

func runtimeStripEnvNamesForConfig(
	boot startupConfigSnapshot,
	current []string,
	cfg *config.Config,
) []string {
	names := slices.Clone(boot.TokenEnvNames)
	for _, name := range current {
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	for _, name := range cfg.TokenEnvNames() {
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *Server) updateRuntimeStripEnvVarsLocked(cfg *config.Config) []string {
	s.runtimeStripEnvVars = runtimeStripEnvNamesForConfig(
		s.bootCfgSnapshot,
		s.runtimeStripEnvVars,
		cfg,
	)
	return slices.Clone(s.runtimeStripEnvVars)
}

// githubAppSplitHosts returns the sorted GitHub hosts whose effective
// provider credential chain carries an active github_app candidate,
// mirroring how startup resolves one source per (platform, host):
// the first plan per key wins, so a host fully covered by terminal
// repo overrides does not count as split.
func githubAppSplitHosts(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[tokenauth.Key]struct{})
	var hosts []string
	for _, plan := range cfg.ProviderTokenSources() {
		key := plan.Descriptor.Key
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if key.Platform == string(platform.KindGitHub) &&
			plan.Descriptor.HasActiveGitHubApp() {
			hosts = append(hosts, key.Host)
		}
	}
	slices.Sort(hosts)
	return hosts
}

func (s startupConfigSnapshot) restartRequiredFor(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	candidate := snapshotStartupConfig(cfg)
	// TokenEnvNames is the boot baseline for runtime strip-env
	// accumulation, not a startup binding: token env changes hot-reload
	// through the token sources, so they must not flag a restart.
	s.TokenEnvNames = nil
	candidate.TokenEnvNames = nil
	// Reflect-deep-equal handles slice and pointer comparison correctly
	// here; the snapshot owns its own slices so external mutation cannot
	// blur the comparison.
	return !reflect.DeepEqual(s, candidate)
}

// startConfigWatcher initializes the fsnotify-based watcher. It is a noop
// when cfgPath is empty (tests that build a Server without persistence) or
// when the parent directory of cfgPath does not exist on disk. The
// watcher goroutine is registered with runBackground so Shutdown waits
// for it to drain.
func (s *Server) startConfigWatcher() {
	if s.cfgPath == "" {
		return
	}
	w, err := configwatch.New(configwatch.Options{
		Path:     s.cfgPath,
		OnChange: s.handleConfigFileChanged,
	})
	if err != nil {
		slog.Warn("config watcher init failed", "err", err)
		return
	}
	s.configWatcher = w
	if !s.runBackground(func(ctx context.Context) {
		w.Start(ctx)
		<-w.Done()
	}) {
		// Shutdown started before we could schedule the watcher; the
		// daemon is on its way out, nothing more to do.
		s.configWatcher = nil
	}
}

// handleConfigFileChanged is invoked by the watcher after debouncing a
// burst of fsnotify events on the config file. It reloads the file,
// applies hot-reloadable fields, and broadcasts a config.changed SSE
// event. The daemon stays running on the previous in-memory config when
// the reload fails so an editor mid-save cannot crash the process.
func (s *Server) handleConfigFileChanged() {
	s.configReloadMu.Lock()
	defer s.configReloadMu.Unlock()
	// Checked under the reload lock: cfgPath is settled at construction,
	// but tests swap it to exercise persistence failures.
	if s.cfgPath == "" {
		return
	}

	event := s.applyConfigChange(s.bgCtx)
	s.hub.Broadcast(Event{
		Type: "config.changed",
		Data: event,
	})
}

// applyConfigChange reloads the config file, copies hot-reloadable fields
// onto the in-memory config, refreshes the syncer's repo set and runtime
// targets, and returns the payload to broadcast. Repository expansion can
// touch provider clients, so it happens before taking cfgMu. The lock is
// held only while applying the already-resolved result to in-memory state.
// The SSE broadcast is intentionally moved out of this function (to
// handleConfigFileChanged) so a slow subscriber cannot stall the daemon.
func (s *Server) applyConfigChange(ctx context.Context) configChangedEvent {
	newCfg, err := config.Load(s.cfgPath)
	// Accumulate the candidate's token env names from any parseable
	// candidate — config.Load returns the parsed config alongside
	// structural validation errors — before every failure path: a
	// rejected reload must still stop those names from reaching future
	// tmux panes, since the user just declared them credentials. The
	// lists are monotonic, so over-stripping from a rejected candidate
	// is safe.
	s.updateRuntimeStripEnvVars(newCfg)
	if err != nil {
		slog.Warn(
			"config reload failed; keeping last-known-good",
			"path", s.cfgPath,
			"err", err,
		)
		return configChangedEvent{
			Valid: false,
			Error: sanitizeConfigError(err, s.cfgPath),
		}
	}
	if err := validateReloadCloneTokenSources(newCfg); err != nil {
		slog.Warn(
			"config reload failed clone token validation; keeping last-known-good",
			"path", s.cfgPath,
			"err", err,
		)
		return configChangedEvent{
			Valid: false,
			Error: sanitizeConfigError(err, s.cfgPath),
		}
	}
	if err := s.validateReloadProviderTokenSources(ctx, newCfg); err != nil {
		slog.Warn(
			"config reload failed provider token validation; keeping last-known-good",
			"path", s.cfgPath,
			"err", err,
		)
		return configChangedEvent{
			Valid: false,
			Error: sanitizeConfigError(err, s.cfgPath),
		}
	}

	s.cfgMu.Lock()
	if s.cfg == nil {
		s.cfgMu.Unlock()
		// Defensive: a Server constructed without a cfg cannot be hot
		// reloaded; treat the change as a parse error so subscribers
		// learn nothing useful was applied.
		return configChangedEvent{
			Valid: false,
			Error: "config reload disabled: server has no in-memory config",
		}
	}
	s.cfgMu.Unlock()

	s.updateTokenSourcesForReload(newCfg)
	restartRequired := s.bootCfgSnapshot.restartRequiredFor(newCfg)
	if s.reloadCredentialNeedsClientRebuild(ctx, newCfg) {
		restartRequired = true
	}
	docsapi.WarnDaemonBindings(newCfg.DocFolders)
	// Kata catalogs load lazily on Kata routes; a catalog edited since
	// boot may declare new token names. Refresh the catalog-derived
	// strip names on every config reload so terminals never race a
	// catalog edit; rejected catalogs still carry declared names.
	if catalog, err := katacatalog.LoadCatalog(); err == nil || len(catalog.TokenEnvNames()) > 0 {
		s.updateCatalogStripEnvVars(catalog.TokenEnvNames())
	}

	// Resolve the new repo set against the boot-time registry. Repos
	// whose (platform, host) the registry never learned about cannot
	// reach a client without a restart; skip those for SetRepos but
	// keep them in s.cfg so the UI mirrors the file. A server built
	// without a syncer (embedded or test setups) still hot-reloads the
	// non-sync surfaces below, so the syncer is nil-guarded rather than
	// treated as a reload failure.
	var previous []ghclient.RepoRef
	if s.syncer != nil {
		previous = s.syncer.TrackedRepos()
	}
	resolved, skipped := s.resolveReposForReload(ctx, newCfg, previous)
	if len(skipped) > 0 {
		slog.Info(
			"config reload: skipping repos for unknown platform hosts",
			"path", s.cfgPath,
			"skipped", skipped,
		)
		restartRequired = true
	}
	if s.syncer != nil {
		if err := s.syncer.SetReposWithContext(ctx, resolved, true); err != nil {
			return configChangedEvent{
				Valid: false,
				Error: sanitizeConfigError(
					fmt.Errorf("apply archive repository lifecycle: %w", err), s.cfgPath,
				),
			}
		}
	}

	s.cfgMu.Lock()
	tmuxGraphicsChanged := s.cfg.TerminalGraphicsEnabled() !=
		newCfg.TerminalGraphicsEnabled()
	*s.cfg = cloneReloadedConfig(newCfg)
	nativeStacksPrevious := s.swapGitHubNativeStackPreferenceLocked(
		newCfg.PullRequests.PreferGitHubNativeStacks,
	)
	s.refreshRuntimeTargetsLocked()
	stripEnvVars := s.updateRuntimeStripEnvVarsLocked(newCfg)
	if s.workspaces != nil {
		s.workspaces.UpdateTmuxStripEnvVars(stripEnvVars)
	}
	if s.ptyOwnerClient != nil {
		s.ptyOwnerClient.UpdateStripEnvVars(stripEnvVars)
	}
	if s.runtime != nil {
		s.runtime.UpdateStripEnvVars(stripEnvVars)
	}
	s.applyWorkspaceConfigLocked()
	s.applyFleetConfigLocked()
	s.applyKataConfigLocked()
	s.applyPullConfigLocked()
	s.applyIssueConfigLocked()
	s.cfgMu.Unlock()
	if tmuxGraphicsChanged {
		s.applyTmuxGraphics(ctx)
	}
	s.applyTmuxMouse(ctx)

	if s.syncer != nil {
		s.syncer.SetBranchActivityLimits(
			newCfg.BranchActivityRetention(),
			newCfg.Activity.DefaultBranchMaxCommits,
		)
		s.syncer.SetWatchInterval(newCfg.ActivePRRefreshDuration())
		s.syncer.SetActiveMRWindow(newCfg.ActivePRWindowDuration())
	}

	if s.docsAPI != nil {
		s.docsAPI.ReplaceFolders(newCfg.DocFolders)
	}
	s.reconcileGitHubNativeStackProjection(
		nativeStacksPrevious, newCfg.PullRequests.PreferGitHubNativeStacks,
	)

	// Removing an exact entry from the TOML file must release its
	// hidden-from-UI preference just like the DELETE handler; otherwise a
	// glob can keep the repository tracked and filtered with no exact row
	// offering a "Show in UI" control.
	if err := s.reconcileOrphanedRepoVisibility(ctx); err != nil {
		slog.Warn(
			"release orphaned hidden-from-UI preferences after config reload",
			"err", err,
		)
	}

	slog.Info(
		"config reload applied",
		"path", s.cfgPath,
		"repo_count", len(resolved),
		"restart_required", restartRequired,
	)
	return configChangedEvent{Valid: true, RestartRequired: restartRequired}
}

// reloadCredentialNeedsClientRebuild reports whether the reloaded config
// resolves a token for a configured platform host that has no live provider
// client. Clients are constructed at startup from the sources that resolved
// then, so a credential added for a host that booted credential-less cannot
// serve sync, settings, or import requests until restart — surface that
// instead of reporting a clean hot reload. Hosts whose token still does not
// resolve are skipped: restarting would not make them usable either.
func (s *Server) reloadCredentialNeedsClientRebuild(
	ctx context.Context,
	cfg *config.Config,
) bool {
	if s.syncer == nil || s.tokenSources == nil || cfg == nil {
		return false
	}
	for _, pc := range cfg.Platforms {
		if _, err := s.syncer.RepositoryReader(
			platform.Kind(pc.Type), pc.Host,
		); err == nil {
			continue
		}
		src, ok := s.tokenSources.Get(tokenauth.Key{
			Platform: pc.Type,
			Host:     pc.Host,
		})
		if !ok || src == nil {
			continue
		}
		if _, err := src.Token(ctx); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) updateTokenSourcesForReload(cfg *config.Config) {
	if s.tokenSources == nil || cfg == nil {
		return
	}
	// Split-auth topology is startup-bound: write rate trackers and
	// the dedicated write clients are wired in buildProviderStartup.
	// If a reload adds or removes a GitHub App for a host, applying
	// the new chain here would flip sync reads to or from the app
	// token immediately while mutation gating still consults the
	// boot-time tracker topology. Those hosts keep their boot chain
	// until the restart the reload already flags as required.
	frozenHosts := s.splitTopologyChangedHosts(cfg)
	if len(frozenHosts) > 0 {
		slog.Info(
			"config reload: github app split topology changed; keeping boot credential chains until restart",
			"hosts", slices.Sorted(maps.Keys(frozenHosts)),
		)
	}
	// Clone credentials live under host-level keys (tokenauth.CloneKey)
	// rather than the provider platform, but they carry the same chain
	// — including the app candidate — so a frozen host's clone source
	// must stay on the boot chain too, keyed by host alone.
	updateIfKnown := func(desc tokenauth.Descriptor) {
		if desc.Key.Platform == string(platform.KindGitHub) ||
			desc.Key == tokenauth.CloneKey(desc.Key.Host) {
			if _, frozen := frozenHosts[desc.Key.Host]; frozen {
				return
			}
		}
		if _, ok := s.tokenSources.Get(desc.Key); !ok {
			return
		}
		s.tokenSources.Upsert(desc)
	}
	bootRoutes := make(map[tokenauth.Key]tokenauth.Descriptor,
		len(s.bootCfgSnapshot.GitHubCredentialRoutes))
	for _, desc := range s.bootCfgSnapshot.GitHubCredentialRoutes {
		bootRoutes[desc.Key] = desc
	}
	for _, plan := range cfg.ProviderTokenSources() {
		desc := plan.Descriptor
		if boot, bounded := bootRoutes[desc.Key]; bounded &&
			desc.Key.Platform == string(platform.KindGitHub) &&
			desc.Key.Scope != "" && !desc.EqualSource(boot) {
			continue
		}
		updateIfKnown(desc)
	}
	// When a provider entry on a shared host loses or changes its token
	// the clone source follows the host's surviving effective chain
	// instead of staying pinned to whichever provider source startup
	// picked.
	for _, desc := range cfg.CloneTokenDescriptors() {
		updateIfKnown(desc)
	}
}

// splitTopologyChangedHosts returns the GitHub hosts whose split-auth
// classification under cfg differs from the boot snapshot — hosts
// that would gain or lose an active GitHub App chain if the reload
// were applied.
func (s *Server) splitTopologyChangedHosts(cfg *config.Config) map[string]struct{} {
	boot := make(map[string]struct{}, len(s.bootCfgSnapshot.GitHubAppSplitHosts))
	for _, host := range s.bootCfgSnapshot.GitHubAppSplitHosts {
		boot[host] = struct{}{}
	}
	next := make(map[string]struct{})
	for _, host := range githubAppSplitHosts(cfg) {
		next[host] = struct{}{}
	}
	changed := make(map[string]struct{})
	for host := range next {
		if _, ok := boot[host]; !ok {
			changed[host] = struct{}{}
		}
	}
	for host := range boot {
		if _, ok := next[host]; !ok {
			changed[host] = struct{}{}
		}
	}
	return changed
}

func (s *Server) validateReloadProviderTokenSources(
	ctx context.Context,
	cfg *config.Config,
) error {
	if s.tokenSources == nil || cfg == nil {
		return nil
	}
	probes := s.tokenSources.NewProbeBatch()
	for _, plan := range cfg.ProviderTokenSources() {
		if !plan.Required {
			continue
		}
		desc := plan.Descriptor
		if plan.ArchiveOnly {
			// Archive-only routes deliberately have no ordinary PAT. Their
			// required credential is the independent archive App source.
			desc = plan.ArchiveDescriptor
		}
		if s.syncer != nil {
			registry := s.syncer.Registry()
			if registry == nil {
				continue
			}
			if _, err := registry.Provider(
				platform.Kind(desc.Key.Platform), desc.Key.Host,
			); err != nil {
				continue
			}
		}
		if _, ok := s.tokenSources.Get(desc.Key); !ok {
			continue
		}
		tokenCtx := ctx
		if plan.GitHubOwner != "" {
			tokenCtx = tokenauth.WithGitHubOwner(tokenCtx, plan.GitHubOwner)
		}
		if _, err := probes.ProbeToken(tokenCtx, desc); err != nil {
			label := fmt.Sprintf("%s host %s", desc.Key.Platform, desc.Key.Host)
			if plan.GitHubOwner != "" {
				label = fmt.Sprintf("%s owner %s", label, plan.GitHubOwner)
			}
			return fmt.Errorf(
				"no token for %s via %s: %w", label, desc.SafeString(), err,
			)
		}
	}
	return nil
}

func validateReloadCloneTokenSources(cfg *config.Config) error {
	return cfg.ValidateRepoTokenSourceConsistency()
}

func cloneReloadedConfig(in *config.Config) config.Config {
	if in == nil {
		return config.Config{}
	}
	out := *in
	out.AllowedHosts = slices.Clone(in.AllowedHosts)
	out.Repos = slices.Clone(in.Repos)
	out.Platforms = slices.Clone(in.Platforms)
	out.GitHubOwnerTokens = slices.Clone(in.GitHubOwnerTokens)
	out.GitHubApps = slices.Clone(in.GitHubApps)
	for i := range out.GitHubApps {
		out.GitHubApps[i].SelectedRepos = slices.Clone(
			in.GitHubApps[i].SelectedRepos,
		)
	}
	out.Modes = cloneModeVisibility(in.Modes)
	out.Agents = cloneConfigAgents(in.Agents)
	out.DocFolders = slices.Clone(in.DocFolders)
	out.Tmux.Command = slices.Clone(in.Tmux.Command)
	if in.Tmux.AgentSessions != nil {
		v := *in.Tmux.AgentSessions
		out.Tmux.AgentSessions = &v
	}
	out.Shell.Command = slices.Clone(in.Shell.Command)
	out.Fleet.Members = slices.Clone(in.Fleet.Members)
	if in.Fleet.Hub != nil {
		hub := *in.Fleet.Hub
		out.Fleet.Hub = &hub
	}
	return out
}

// resolveReposForReload walks the reloaded repo list and asks the syncer
// (via the boot-time platform registry) whether each (provider, host)
// pair has a client. Known hosts are resolved into RepoRefs; unknown
// hosts are returned in the skipped slice with a "owner/name@host"
// display string for logging.
func (s *Server) resolveReposForReload(
	ctx context.Context,
	cfg *config.Config,
	previous []ghclient.RepoRef,
) ([]ghclient.RepoRef, []string) {
	if s.syncer == nil {
		return nil, nil
	}
	set := ghclient.NewExpandedRepoSet()
	skipped := make([]string, 0)

	for _, raw := range cfg.Repos {
		host := raw.PlatformHostOrDefault()
		kind := platform.Kind(raw.PlatformOrDefault())
		if _, err := s.syncer.RepositoryReader(kind, host); err != nil {
			for _, repo := range ghclient.FallbackConfiguredRepoRefs(previous, raw) {
				if slices.Contains(previous, repo) {
					set.Add(repo, false)
				}
			}
			skipped = append(skipped, fmt.Sprintf(
				"%s/%s@%s/%s",
				string(kind), host, raw.Owner, raw.Name,
			))
			continue
		}
		resolveCtx := ctx
		if kind == platform.KindGitHub &&
			cfg.ResolveGitHubArchiveTokenSource(raw).Key.Host != "" {
			resolveCtx = ghclient.WithArchiveSyncBudget(ctx)
		}
		_, expanded, err := ghclient.ResolveConfiguredRepoWithRegistry(
			resolveCtx, s.syncer.SyncRegistry(), raw,
		)
		if err != nil {
			// Network failure or transient API error: fall back to a
			// synthetic RepoRef built from the configured fields so
			// the syncer still has a target to retry on its next
			// tick. This matches resolveStartupRepos's offline-
			// resilience behavior.
			slog.Warn(
				"config reload resolve repo failed; using fallback",
				"owner", raw.Owner,
				"name", raw.Name,
				"err", err,
			)
			expanded = ghclient.FallbackConfiguredRepoRefs(previous, raw)
		}
		for _, repo := range expanded {
			set.Add(repo, err == nil)
		}
	}
	return set.Refs(), skipped
}

// sanitizeConfigError trims internal path prefixes from the error so the
// frontend payload does not leak the absolute config path on the user's
// machine. The path is already known to the operator from logs.
func sanitizeConfigError(err error, cfgPath string) string {
	msg := err.Error()
	if cfgPath != "" {
		msg = strings.ReplaceAll(msg, cfgPath, "config.toml")
	}
	return tokenauth.RedactKnownSecrets(msg)
}
