package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/cli/serve"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/daemonruntime"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/githubapp"
	"go.kenn.io/forge/internal/mcpserver"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/profiler"
	"go.kenn.io/forge/internal/ptyowner"
	"go.kenn.io/forge/internal/runtimelock"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/fleetapi"
	"go.kenn.io/forge/internal/shutdownbudget"
	"go.kenn.io/forge/internal/stacks"
	"go.kenn.io/forge/internal/telemetry"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/web"
	"go.kenn.io/kit/daemon"
	oteltelemetry "go.kenn.io/kit/telemetry"
)

type splitLogHandler struct {
	handlers []slog.Handler
}

type serveReadyListener struct {
	net.Listener
	notifyReady func()
}

func (l serveReadyListener) Accept() (net.Conn, error) {
	l.notifyReady()
	return l.Listener.Accept()
}

func newMCPStartupHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "MCP is unavailable during startup", http.StatusServiceUnavailable)
	})
}

func bindDaemonListeners(cfg *config.Config) (net.Listener, net.Listener, error) {
	primaryAddr := cfg.ListenAddr()
	primary, err := net.Listen("tcp", primaryAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", primaryAddr, err)
	}
	if !cfg.MCP.Enabled {
		return primary, nil, nil
	}
	mcpAddr := cfg.MCPListenAddr()
	mcpListener, err := net.Listen("tcp", mcpAddr)
	if err != nil {
		_ = primary.Close()
		return nil, nil, fmt.Errorf("listen for MCP on %s: %w", mcpAddr, err)
	}
	return primary, mcpListener, nil
}

func (h splitLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h splitLogHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, r.Level) {
			continue
		}
		if err := handler.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (h splitLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithAttrs(attrs))
	}
	return splitLogHandler{handlers: handlers}
}

func (h splitLogHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithGroup(name))
	}
	return splitLogHandler{handlers: handlers}
}

var (
	version                = "dev"
	commit                 = "unknown"
	buildDate              = "unknown"
	runtimePublishGatePath string
	runtimeServeGatePath   string
	runtimeShutdownDelay   string
)

var runServer = run

type versionOutput struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

func main() {
	closeLog, err := configureLogging(os.Stderr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "configure logging: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := closeLog(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "close log file: %v\n", err)
		}
	}()

	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		if _, ok := errors.AsType[*apiVerbError](err); ok {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(exitCodeForAPIVerb(err))
			return
		}
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func configureLogging(stderr io.Writer) (func() error, error) {
	level, err := parseLogLevel(os.Getenv("KENN_FORGE_LOG_LEVEL"))
	if err != nil {
		return nil, err
	}

	var file *os.File
	logFile := strings.TrimSpace(os.Getenv("KENN_FORGE_LOG_FILE"))
	stderrLevel := level
	if logFile != "" {
		stderrLevel = slog.LevelInfo
	}
	if raw := os.Getenv("KENN_FORGE_LOG_STDERR_LEVEL"); strings.TrimSpace(raw) != "" {
		stderrLevel, err = parseLogLevel(raw)
		if err != nil {
			return nil, err
		}
	}

	handlers := []slog.Handler{
		tokenauth.NewRedactingHandler(slog.NewTextHandler(
			stderr,
			&slog.HandlerOptions{Level: stderrLevel},
		)),
	}
	if logFile != "" {
		if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		file, err = os.OpenFile(
			logFile,
			os.O_CREATE|os.O_WRONLY|os.O_APPEND,
			0o600,
		)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		handlers = append(
			handlers,
			tokenauth.NewRedactingHandler(slog.NewTextHandler(
				file,
				&slog.HandlerOptions{Level: level},
			)),
		)
	}

	slog.SetDefault(slog.New(splitLogHandler{handlers: handlers}))
	slog.Debug(
		"logging configured",
		"level", level.String(),
		"stderr_level", stderrLevel.String(),
		"file", logFile,
	)

	return func() error {
		if file == nil {
			return nil
		}
		return file.Close()
	}, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf(
			"unsupported KENN_FORGE_LOG_LEVEL %q", raw,
		)
	}
}

func writeVersion(stdout io.Writer, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Fprintf(
			stdout,
			"kenn-forge %s (%s) built %s\n",
			version, commit, buildDate,
		)
		return err
	}
	return json.NewEncoder(stdout).Encode(versionOutput{
		Name:      "kenn-forge",
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	})
}

func runPtyOwner(root, session, cwd, commandJSON string) error {
	if session == "" {
		return fmt.Errorf("pty-owner session is required")
	}
	if root == "" {
		return fmt.Errorf("pty-owner root is required")
	}
	if cwd == "" {
		return fmt.Errorf("pty-owner cwd is required")
	}
	var command []string
	if commandJSON != "" {
		if err := json.Unmarshal([]byte(commandJSON), &command); err != nil {
			return fmt.Errorf("parse pty-owner command-json: %w", err)
		}
	}
	return ptyowner.RunOwner(context.Background(), ptyowner.Options{
		Root:    root,
		Session: session,
		Cwd:     cwd,
		Command: command,
	})
}

func readConfigValue(configPath, key string, stdout io.Writer) error {
	if err := config.EnsureDefault(configPath); err != nil {
		return fmt.Errorf("ensure config: %w", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	switch key {
	case "port":
		_, err := fmt.Fprintf(stdout, "%d\n", cfg.Port)
		return err
	default:
		return fmt.Errorf("unsupported config key %q", key)
	}
}

func writeRuntimeStatus(dataDir string, asJSON bool, stdout io.Writer) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf(
			"create data directory %s: %w", dataDir, err,
		)
	}

	st, err := runtimelock.Read(dataDir)
	if err != nil {
		return fmt.Errorf("read runtime status: %w", err)
	}

	return runtimelock.FormatStatus(stdout, st, asJSON)
}

func run(opts serve.Options) error {
	configPath := opts.ConfigPath
	cfg, err := config.LoadOrCreate(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := validateBackgroundLaunchConfig(cfg); err != nil {
		return err
	}
	slog.Debug(
		"config loaded",
		"config_path", configPath,
		"data_dir", cfg.DataDir,
		"db_path", cfg.DBPath(),
		"listen_addr", cfg.ListenAddr(),
		"repo_count", len(cfg.Repos),
	)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf(
			"create data directory %s: %w", cfg.DataDir, err,
		)
	}

	lockHandle, err := runtimelock.Acquire(cfg.DataDir)
	if err != nil {
		if cerr, ok := errors.AsType[*runtimelock.CollisionError](err); ok {
			runtimelock.FormatCollisionBanner(
				os.Stderr, cerr, configPath, config.DefaultConfigPath(),
			)
			return fmt.Errorf(
				"another kenn-forge is already running on %s",
				cfg.DataDir,
			)
		}
		return fmt.Errorf("acquire runtime lock: %w", err)
	}
	defer func() {
		if err := lockHandle.Release(); err != nil {
			slog.Warn("release runtime lock", "err", err)
		}
	}()

	ctx, stopSignals := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	stopSignalsOnce := sync.OnceFunc(stopSignals)
	defer stopSignalsOnce()

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load frontend assets: %w", err)
	}

	// API auth: the token is always minted (thin clients read it from
	// the well-known data_dir path), but only enforced when
	// [api].require_auth is set.
	authToken, err := runtimelock.EnsureAuthToken(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("ensure auth token: %w", err)
	}
	federationCredentials, err := federationauth.Open(
		federationauth.DefaultStorePath(cfg.DataDir),
	)
	if err != nil {
		return fmt.Errorf("open federation credential store: %w", err)
	}
	federationEnrollments, err := federation.Open(
		federation.DefaultStorePath(cfg.DataDir), federation.StoreOptions{},
	)
	if err != nil {
		return fmt.Errorf("open federation enrollment store: %w", err)
	}
	if err := validateFederationHubOrigin(cfg, federationEnrollments); err != nil {
		return err
	}
	ln, mcpLn, err := bindDaemonListeners(cfg)
	if err != nil {
		return err
	}
	mcpListenAddr := ""
	mcpURL := ""
	if mcpLn != nil {
		mcpListenAddr = mcpLn.Addr().String()
		mcpURL = "http://" + mcpListenAddr + "/mcp"
	}
	closeListeners := func() {
		_ = ln.Close()
		if mcpLn != nil {
			_ = mcpLn.Close()
		}
	}
	if ip := net.ParseIP(cfg.Host); ip != nil && !ip.IsLoopback() {
		slog.Warn(
			"binding a non-loopback address: the API has no"+
				" authentication, so the bound network is the trust"+
				" boundary (e.g. a tailnet with ACLs)",
			"host", cfg.Host,
		)
	}

	runtimeIdentity, err := daemonruntime.NewIdentity(ln.Addr(), daemonruntime.IdentityOptions{
		Version: version, Commit: commit, DataDir: cfg.DataDir, ConfigPath: configPath,
		BasePath: cfg.BasePath, RequireAuth: cfg.API.RequireAuth,
		MCPListenAddr: mcpListenAddr,
	})
	if err != nil {
		closeListeners()
		return fmt.Errorf("build daemon runtime identity: %w", err)
	}
	if err := lockHandle.WriteMetadata(runtimeIdentity.LockMetadata); err != nil {
		slog.Warn("write runtime metadata", "err", err)
	}
	proof, err := daemon.NewProof([]byte(authToken))
	if err != nil {
		closeListeners()
		return fmt.Errorf("initialize daemon proof: %w", err)
	}
	daemonProofHandler, err := proof.NewPingHandler(runtimeIdentity.Record)
	if err != nil {
		closeListeners()
		return fmt.Errorf("initialize daemon ping: %w", err)
	}
	daemonAccess := server.DaemonAccessOptions{
		Token: authToken, RequireAPIAuth: cfg.API.RequireAuth,
		ProofHandler:          daemonProofHandler,
		TailscaleServeEnabled: cfg.API.TailscaleServe.Enabled,
		TailscaleServeUsers:   cfg.API.TailscaleServe.AllowedUsers,
	}

	startupOptions := server.ServerOptions{DaemonAccess: daemonAccess, MCPURL: mcpURL}
	startupHandler := server.NewStartupHandler(assets, cfg, startupOptions, ln)
	switcher := server.NewSwitchHandler(startupHandler)
	httpSrv := &http.Server{
		Handler:     switcher,
		ReadTimeout: 15 * time.Second,
		// WriteTimeout is 0 (disabled) because SSE and proxy
		// responses are long-lived by design.
		IdleTimeout: 60 * time.Second,
	}

	var mcpHTTPSrv *http.Server
	var mcpSwitcher *server.SwitchHandler
	mcpRequestsCtx, cancelMCPRequests := context.WithCancel(context.Background())
	defer cancelMCPRequests()
	if mcpLn != nil {
		bind, parseErr := config.ParseHostKey(mcpListenAddr)
		if parseErr != nil {
			closeListeners()
			return fmt.Errorf("parse MCP listener address %s: %w", mcpListenAddr, parseErr)
		}
		mcpSwitcher = server.NewSwitchHandler(newMCPStartupHandler())
		mcpHTTPSrv = &http.Server{
			Handler: server.NewMCPHTTPGuard(mcpSwitcher, server.MCPHTTPGuardOptions{
				Bind: bind, Token: authToken, RequireAuth: cfg.API.RequireAuth,
			}),
			ReadHeaderTimeout: 5 * time.Second,
			// Handlers inherit this context so shutdown can cancel
			// long-running MCP requests once the grace period expires.
			BaseContext: func(net.Listener) context.Context { return mcpRequestsCtx },
		}
	}

	readyCount := 1
	if mcpLn != nil {
		readyCount++
	}
	serveReady := make(chan struct{}, readyCount)
	errCh := make(chan error, readyCount)
	primaryReadyListener := serveReadyListener{
		Listener:    ln,
		notifyReady: sync.OnceFunc(func() { serveReady <- struct{}{} }),
	}
	go func() {
		if serveErr := httpSrv.Serve(primaryReadyListener); !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("primary HTTP server: %w", serveErr)
		}
	}()
	if mcpLn != nil {
		mcpReadyListener := serveReadyListener{
			Listener:    mcpLn,
			notifyReady: sync.OnceFunc(func() { serveReady <- struct{}{} }),
		}
		go func() {
			if serveErr := mcpHTTPSrv.Serve(mcpReadyListener); !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("MCP HTTP server: %w", serveErr)
			}
		}()
	}

	var database *db.DB
	var srv *server.Server
	var mcpSrv *mcpserver.Server
	var syncer *ghclient.Syncer
	var telemetryReporter *telemetry.Reporter
	var profilerSrv *profiler.Server
	var backgroundLoops *backgroundLoopHandle
	defer func() {
		if err := waitForRuntimeShutdownDelay(); err != nil {
			slog.Warn("test shutdown delay", "err", err)
		}
		for _, shutdownErr := range runMainShutdown(
			context.Background(),
			mainShutdownCallbacks{
				StopSignals: stopSignalsOnce,
				StopBackgroundLoops: func(ctx context.Context) error {
					if backgroundLoops == nil {
						return nil
					}
					return backgroundLoops.Stop(ctx)
				},
				ShutdownMCPHTTP: func(shutdownCtx context.Context) error {
					if mcpHTTPSrv == nil {
						return nil
					}
					// Stop admission and wait out the grace period, then
					// cancel still-running MCP handlers and force-close
					// their connections so later cleanup never closes
					// shared services beneath an in-flight handoff.
					err := mcpHTTPSrv.Shutdown(shutdownCtx)
					cancelMCPRequests()
					if closeErr := mcpHTTPSrv.Close(); err == nil {
						err = closeErr
					}
					return err
				},
				ShutdownPrimaryHTTP: func(shutdownCtx context.Context) error {
					if srv != nil {
						return srv.Shutdown(shutdownCtx)
					}
					return httpSrv.Shutdown(shutdownCtx)
				},
				StopSyncer: func() {
					if syncer != nil {
						syncer.Stop()
					}
				},
				ShutdownProfiler: func(ctx context.Context) error {
					if profilerSrv != nil {
						return profilerSrv.Shutdown(ctx)
					}
					return nil
				},
				CloseTelemetry: func() error {
					if telemetryReporter == nil {
						return nil
					}
					return telemetryReporter.Close()
				},
				CloseMCP: func() error {
					if mcpSrv == nil {
						return nil
					}
					return mcpSrv.Close()
				},
				CloseDatabase: func() error {
					if database == nil {
						return nil
					}
					return database.Close()
				},
			},
		) {
			slog.Warn(shutdownErr.message, "err", shutdownErr.err)
		}
	}()

	for range readyCount {
		select {
		case <-serveReady:
		case <-ctx.Done():
			return fmt.Errorf("wait for HTTP server readiness: %w", ctx.Err())
		case serveErr := <-errCh:
			return fmt.Errorf("start HTTP server: %w", serveErr)
		}
	}
	if err := waitForRuntimeGate(ctx, runtimePublishGatePath, "publish"); err != nil {
		return err
	}
	runtimePath, err := daemonruntime.Publish(runtimeIdentity.Record)
	if err != nil {
		return fmt.Errorf("write daemon runtime record: %w", err)
	}
	defer func() {
		if err := os.Remove(runtimePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove daemon runtime record", "err", err)
		}
	}()
	if err := waitForRuntimeGate(ctx, runtimeServeGatePath, "serve"); err != nil {
		return err
	}

	slog.Info(fmt.Sprintf("starting server at http://%s", ln.Addr().String()))
	if mcpListenAddr != "" {
		slog.Info("starting MCP listener", "url", mcpURL)
	}

	if ctx.Err() != nil {
		slog.Info("shutting down")
		return nil
	}

	profilerSrv, err = profiler.Start(opts.ProfilerAddr)
	if err != nil {
		return err
	}
	if profilerSrv != nil {
		profilerAddr := ""
		if addr := profilerSrv.Addr(); addr != nil {
			profilerAddr = addr.String()
		}
		slog.Info(
			"starting profiler listener",
			"addr", profilerAddr,
		)
	}

	database, err = db.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	backgroundLoops = startBackgroundLoops(ctx, database)
	tokenSources := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: config.GitHubCLITokenForHost,
		GitHubApp: func(
			ctx context.Context, candidate tokenauth.Candidate,
		) (string, time.Time, error) {
			return githubapp.MintInstallationToken(
				ctx, candidate.Host, candidate.AppID,
				candidate.FilePath, candidate.InstallationID,
			)
		},
	})
	spokeStartup := activateFederationSpokeAtStartup(
		ctx, database, cfg, runtimeIdentity.LockMetadata.NodeID,
		federationEnrollments, federationCredentials, nil,
	)
	if cfg.Fleet.RoleOrDefault() == config.FleetRoleSpoke && !spokeStartup.Active() {
		slog.Warn(
			"fleet spoke started with local execution only",
			"state", spokeStartup.State, "reason", spokeStartup.Reason,
		)
	}
	var identityResolver ghclient.IdentityResolver
	if !opts.DisableSync {
		identityResolver = ghclient.HTTPIdentityResolver{}
	}
	controlPlanes, err := buildServeControlPlanes(
		ctx, database, cfg, tokenSources, defaultProviderFactories(),
		identityResolver, opts.DisableSync,
	)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			slog.Info("shutting down")
			return nil
		}
		return err
	}
	if ctx.Err() != nil {
		slog.Info("shutting down")
		return nil
	}
	cloneMgr := gitclone.New(
		filepath.Join(cfg.DataDir, "clones"), &controlPlanes.Git,
	)
	configureCloneTransportPolicy(cloneMgr, cfg)

	var archiveService *archive.Service
	var repos []ghclient.RepoRef
	if controlPlanes.Provider != nil {
		controlPlane := controlPlanes.Provider
		syncer = ghclient.NewSyncerWithRegistry(
			controlPlane.registry, database, cloneMgr, nil,
			cfg.SyncDuration(), controlPlane.rateTrackers, controlPlane.budgets,
		)
		if opts.DisableSync {
			syncer.DisableSync()
		}
		repos = resolveStartupRepos(
			ctx, cfg, syncer.SyncRegistry(), database, controlPlane.githubRouters,
		)
		slog.Debug("startup repos resolved", "count", len(repos))
		syncer.SetBranchActivityLimits(
			cfg.BranchActivityRetention(), cfg.Activity.DefaultBranchMaxCommits,
		)
		syncer.SetWatchInterval(cfg.ActivePRRefreshDuration())
		syncer.SetActiveMRWindow(cfg.ActivePRWindowDuration())
		syncer.SetPreferGitHubNativeStacks(cfg.PullRequests.PreferGitHubNativeStacks)
		syncer.SetFetchers(controlPlane.fetchers)
		syncer.SetGitHubRouters(controlPlane.githubRouters)
		syncer.SetRatePrincipalLabels(controlPlane.ratePrincipalLabels)
		syncer.SetQuotaRegistry(controlPlane.quotaRegistry)
		syncer.SetWriteRateTrackers(controlPlane.writeRateTrackers)
		syncer.SetWriteGQLRateTrackers(controlPlane.writeGQLRateTrackers)
		archiveService, err = archive.NewService(
			database, controlPlane.registry, syncer, syncer, nil, nil,
		)
		if err != nil {
			return fmt.Errorf("create archive service: %w", err)
		}
		archiveService.SetMaintenanceInterval(cfg.SyncDuration())
		archiveService.SetWake(syncer.WakeArchive)
		syncer.SetArchiveService(archiveService)
		if err := syncer.SetReposWithContext(ctx, repos, false); err != nil {
			return fmt.Errorf("prepare archive repositories: %w", err)
		}
	}

	telemetryReporter = telemetry.NewReporterOrDisabled(telemetry.Options{
		Database: database,
		Version:  version,
		Commit:   commit,
	})
	if telemetryReporter.Enabled() {
		if err := telemetryReporter.Capture("daemon_active", map[string]any{
			"repo_count": len(repos),
		}); err != nil {
			slog.Warn("capture telemetry event", "err", err)
		}
	}

	srv = server.NewWithConfig(
		database, syncer, cloneMgr, assets,
		cfg, configPath, server.ServerOptions{
			DaemonAccess:                     daemonAccess,
			FederationCredentials:            federationCredentials,
			FederationEnrollments:            federationEnrollments,
			FederationSpokeID:                runtimeIdentity.LockMetadata.NodeID,
			FederationSpokeActive:            spokeStartup.Active(),
			FederationSpokeUnavailableReason: spokeStartup.Reason,
			MaintainFederationSpokeActivation: func(ctx context.Context) {
				maintainFederationSpokeActivation(
					ctx, federationEnrollments, federationCredentials, nil,
				)
			},
			MCPURL:                          mcpURL,
			WorktreeDir:                     filepath.Join(cfg.DataDir, "worktrees"),
			PtyOwnerManagerPath:             os.Getenv("KENN_FORGE_PTY_MANAGER"),
			Telemetry:                       telemetryReporter,
			TokenSources:                    tokenSources,
			Archive:                         archiveService,
			DetachRuntimeSessionsForRestart: os.Getenv("KENN_FORGE_DEV_RESTART") == "1",
		},
	)
	srv.AttachHTTPServer(httpSrv, ln)
	slog.Debug(
		"server initialized",
		"base_path", cfg.BasePath,
		"worktree_dir", filepath.Join(cfg.DataDir, "worktrees"),
	)

	if syncer != nil {
		// Wire status callbacks only when this process owns a provider plane.
		syncer.SetOnStatusChange(func(status *ghclient.SyncStatus) {
			srv.Hub().Broadcast(server.Event{
				Type: "sync_status", Data: status,
			})
			if !status.Running {
				srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
			}
		})
		srv.Hub().Broadcast(server.Event{
			Type: "sync_status", Data: syncer.Status(),
		})
		syncer.SetOnNotificationSyncComplete(func() {
			srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
		})
		syncer.SetOnWatchedMRSyncCompleted(func() {
			srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
		})
		syncer.SetOnSyncCompleted(
			fleetapi.WorktreeLinksSyncHook(
				ctx, database, syncer,
				srv.Fleet().NotifyWorktreeLinksChanged,
				stacks.SyncCompletedHook(ctx, database, nil),
			),
		)
		syncer.Start(ctx)
		if !opts.DisableSync && cfg.NotificationsEnabled() {
			startNotificationLoops(backgroundLoops, syncer, cfg)
		}
	}

	otelShutdown, err := oteltelemetry.Init(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), shutdownbudget.OpenTelemetry,
		)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Warn("telemetry shutdown failed", "err", err)
		}
	}()

	srv.SetBuildInfo(server.BuildInfo{
		Name:      "kenn-forge",
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	})
	if mcpSwitcher != nil {
		mcpSrv, err = mcpserver.New(mcpserver.Options{
			Backend: srv.MCPBackend(), Version: version,
			DiffCacheBytes: cfg.MCPDiffCacheBytes(),
		})
		if err != nil {
			return fmt.Errorf("initialize MCP server: %w", err)
		}
	}
	switcher.Swap(srv)
	if mcpSwitcher != nil {
		mcpSwitcher.Swap(mcpSrv.HTTPHandler())
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		return nil
	case err := <-profilerSrvDone(profilerSrv):
		return fmt.Errorf("profiler: %w", err)
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	}
}

// waitForRuntimeGate is enabled only by an ldflags-injected test build.
func waitForRuntimeGate(ctx context.Context, gatePath, phase string) error {
	if gatePath == "" {
		return nil
	}
	if err := os.WriteFile(gatePath+".ready", []byte("ready\n"), 0o600); err != nil {
		return fmt.Errorf("%s runtime gate readiness: %w", phase, err)
	}
	defer func() { _ = os.Remove(gatePath + ".ready") }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(gatePath); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect runtime %s gate: %w", phase, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for runtime %s gate: %w", phase, ctx.Err())
		case <-ticker.C:
		}
	}
}

type mainShutdownCallbacks struct {
	StopSignals         func()
	StopBackgroundLoops func(context.Context) error
	ShutdownMCPHTTP     func(context.Context) error
	ShutdownPrimaryHTTP func(context.Context) error
	StopSyncer          func()
	ShutdownProfiler    func(context.Context) error
	CloseTelemetry      func() error
	CloseMCP            func() error
	CloseDatabase       func() error
}

type mainShutdownError struct {
	message string
	err     error
}

func runMainShutdown(
	ctx context.Context,
	callbacks mainShutdownCallbacks,
) []mainShutdownError {
	var errs []mainShutdownError
	if callbacks.StopSignals != nil {
		callbacks.StopSignals()
	}
	if callbacks.StopBackgroundLoops != nil {
		if err := runContextShutdown(
			ctx, shutdownbudget.BackgroundLoops,
			callbacks.StopBackgroundLoops,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "background loops shutdown",
				err:     err,
			})
		}
	}
	if callbacks.ShutdownMCPHTTP != nil {
		if err := runContextShutdown(
			ctx, shutdownbudget.MCPHTTP, callbacks.ShutdownMCPHTTP,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "MCP HTTP shutdown",
				err:     err,
			})
		}
	}
	if callbacks.ShutdownPrimaryHTTP != nil {
		if err := runContextShutdown(
			ctx, shutdownbudget.PrimaryHTTP, callbacks.ShutdownPrimaryHTTP,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "server shutdown",
				err:     err,
			})
		}
	}
	if callbacks.StopSyncer != nil {
		callbacks.StopSyncer()
	}
	if callbacks.ShutdownProfiler != nil {
		if err := runContextShutdown(
			ctx, shutdownbudget.Profiler, callbacks.ShutdownProfiler,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "profiler shutdown",
				err:     err,
			})
		}
	}
	if callbacks.CloseTelemetry != nil {
		if err := runBoundedShutdown(
			ctx, shutdownbudget.TelemetryReporter, callbacks.CloseTelemetry,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "close telemetry",
				err:     err,
			})
		}
	}
	if callbacks.CloseMCP != nil {
		if err := runBoundedShutdown(
			ctx, shutdownbudget.MCPStore, callbacks.CloseMCP,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "close MCP temp store",
				err:     err,
			})
		}
	}
	if callbacks.CloseDatabase != nil {
		if err := runBoundedShutdown(
			ctx, shutdownbudget.Database, callbacks.CloseDatabase,
		); err != nil {
			errs = append(errs, mainShutdownError{
				message: "close database",
				err:     err,
			})
		}
	}
	return errs
}

func runContextShutdown(
	parent context.Context,
	timeout time.Duration,
	shutdown func(context.Context) error,
) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return shutdown(ctx)
}

func runBoundedShutdown(
	parent context.Context,
	timeout time.Duration,
	shutdown func() error,
) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- shutdown() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForRuntimeShutdownDelay() error {
	if runtimeShutdownDelay == "" {
		return nil
	}
	delay, err := time.ParseDuration(runtimeShutdownDelay)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", runtimeShutdownDelay, err)
	}
	time.Sleep(delay)
	return nil
}

func profilerSrvDone(srv *profiler.Server) <-chan error {
	if srv == nil {
		return nil
	}
	return srv.Done()
}

func configureCloneTransportPolicy(clones *gitclone.Manager, cfg *config.Config) {
	if clones == nil || cfg == nil {
		return
	}
	for _, configured := range cfg.Platforms {
		if configured.AllowInsecure {
			clones.SetAllowInsecureHTTP(
				configured.Type, configured.Host, true,
			)
		}
	}
}

func resolveStartupRepos(
	ctx context.Context,
	cfg *config.Config,
	registry *platform.Registry,
	database *db.DB,
	githubRouters map[string]*ghclient.HostRouter,
) []ghclient.RepoRef {
	set := ghclient.NewExpandedRepoSet()
	for _, raw := range cfg.Repos {
		resolveCtx := ctx
		if raw.PlatformOrDefault() == string(platform.KindGitHub) &&
			cfg.ResolveGitHubArchiveTokenSource(raw).Key.Host != "" {
			// A repository with no ordinary PAT may still be discoverable
			// through its dedicated archive App on a fresh startup.
			resolveCtx = ghclient.WithArchiveSyncBudget(ctx)
		}
		configuredProviderID := strings.TrimSpace(raw.PlatformRepoID)
		_, expanded, err := ghclient.ResolveConfiguredRepoWithRegistry(
			resolveCtx, registry, raw,
		)
		if err != nil {
			slog.Warn("resolve configured repo", "err", err)
			if raw.HasNameGlob() {
				expanded = fallbackGlobFromDB(
					ctx, database, raw,
				)
			} else {
				expanded = fallbackExactFromDB(ctx, database, raw)
				if len(expanded) == 0 {
					if configuredProviderID == "" {
						expanded = ghclient.FallbackConfiguredRepoRefs(nil, raw)
					}
				} else {
					// The catalog recovered a stable identity on a renamed
					// route; without the alias, repo-scoped credentials for
					// the configured route would fall through to owner or
					// host credentials.
					ghclient.RegisterConfiguredRepoCredentialAliases(
						githubRouters, raw, expanded,
					)
				}
			}
		} else {
			ghclient.RegisterConfiguredRepoCredentialAliases(
				githubRouters, raw, expanded,
			)
		}
		for _, repo := range expanded {
			set.Add(repo, err == nil)
		}
	}
	return set.Refs()
}

func providerHostKey(platformName, host string) string {
	return strings.ToLower(platformName) + "\x00" + strings.ToLower(host)
}

func splitProviderHostKey(key string) (string, string) {
	platformName, host, ok := strings.Cut(key, "\x00")
	if !ok {
		return key, ""
	}
	return platformName, host
}

// fallbackExactFromDB recovers the stored identity for an exact config
// entry whose provider resolution failed at startup. Catalog route history
// follows a provider-side rename, so the fallback keeps the stable provider
// id — deduplicating against overlapping resolved entries — instead of
// synthesizing an identity-less ref under the stale configured route.
func fallbackExactFromDB(
	ctx context.Context,
	database *db.DB,
	raw config.Repo,
) []ghclient.RepoRef {
	if database == nil {
		return nil
	}
	repoPath := strings.TrimSpace(raw.RepoPath)
	if repoPath == "" {
		repoPath = raw.Owner + "/" + raw.Name
	}
	filter := db.RepositoryCatalogFilter{
		Platform:     raw.PlatformOrDefault(),
		PlatformHost: raw.PlatformHostOrDefault(),
		Lifecycle:    db.RepositoryLifecycleActive,
	}
	if strings.TrimSpace(raw.PlatformRepoID) != "" {
		filter.PlatformRepoID = raw.PlatformRepoID
	} else {
		filter.RepoPath = repoPath
	}
	entries, err := database.ListRepositoryCatalog(ctx, filter)
	if err != nil {
		slog.Warn("fallback exact from db", "err", err)
		return nil
	}
	var entry *db.RepositoryCatalogEntry
	if filter.PlatformRepoID != "" {
		if len(entries) == 1 {
			entry = &entries[0]
		}
	} else {
		entry = catalogEntryForConfiguredRoute(entries, repoPath)
	}
	if entry == nil {
		return nil
	}
	return []ghclient.RepoRef{{
		Platform:           platform.Kind(raw.PlatformOrDefault()),
		Owner:              entry.Repository.Owner,
		Name:               entry.Repository.Name,
		PlatformHost:       entry.Repository.PlatformHost,
		RepoPath:           entry.Repository.RepoPath,
		PlatformExternalID: entry.Repository.PlatformRepoID,
		WebURL:             entry.Repository.WebURL,
		CloneURL:           entry.Repository.CloneURL,
		DefaultBranch:      entry.Repository.DefaultBranch,
		ConfiguredRepoPath: repoPath,
	}}
}

// catalogEntryForConfiguredRoute prefers the repository currently occupying
// the configured route; a reused route may also historically match the
// renamed repository that held it before. Multiple historical matches with
// no current occupant cannot be attributed safely.
func catalogEntryForConfiguredRoute(
	entries []db.RepositoryCatalogEntry, repoPath string,
) *db.RepositoryCatalogEntry {
	for i := range entries {
		for _, route := range entries[i].Routes {
			if route.Current && strings.EqualFold(route.RepoPath, repoPath) {
				return &entries[i]
			}
		}
	}
	if len(entries) == 1 {
		return &entries[0]
	}
	return nil
}

// fallbackGlobFromDB returns repos from the database that match
// the glob config entry, preserving previously tracked matches
// when GitHub is unreachable at startup.
func fallbackGlobFromDB(
	ctx context.Context,
	database *db.DB,
	raw config.Repo,
) []ghclient.RepoRef {
	if database == nil {
		return nil
	}
	dbRepos, err := database.ListRepos(ctx)
	if err != nil {
		slog.Warn("fallback glob from db", "err", err)
		return nil
	}
	rawPlatform := platform.Kind(raw.PlatformOrDefault())
	host := raw.PlatformHostOrDefault()
	var matches []ghclient.RepoRef
	for _, r := range dbRepos {
		dbPlatform := platform.Kind(r.Platform)
		if dbPlatform == "" {
			dbPlatform = platform.KindGitHub
		}
		dbHost := r.PlatformHost
		if dbHost == "" {
			dbHost = platform.DefaultGitHubHost
		}
		if dbPlatform != rawPlatform ||
			!strings.EqualFold(dbHost, host) ||
			!strings.EqualFold(r.Owner, raw.Owner) {
			continue
		}
		matched, _ := path.Match(
			strings.ToLower(raw.Name),
			strings.ToLower(r.Name),
		)
		if matched {
			repo := ghclient.RepoRef{
				Platform:     rawPlatform,
				Owner:        r.Owner,
				Name:         r.Name,
				PlatformHost: dbHost,
			}
			matches = append(matches, repo)
		}
	}
	if len(matches) > 0 {
		slog.Info(
			"using DB-persisted repos for offline glob",
			"pattern", raw.Owner+"/"+raw.Name,
			"count", len(matches),
		)
	}
	return matches
}
