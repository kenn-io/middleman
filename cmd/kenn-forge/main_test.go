package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/cli/serve"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

func TestMain(m *testing.M) {
	if os.Getenv("TELEMETRY_ENABLED") == "" {
		if err := os.Setenv("TELEMETRY_ENABLED", "0"); err != nil {
			panic(err)
		}
	}
	runtimeDir, err := os.MkdirTemp("", "kenn-forge-test-home-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("KENN_FORGE_HOME", runtimeDir); err != nil {
		panic(err)
	}
	code := m.Run()
	if err := os.RemoveAll(runtimeDir); err != nil {
		panic(err)
	}
	os.Exit(code)
}

func TestConfigureLoggingRedactsTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	var buf bytes.Buffer

	closeLog, err := configureLogging(&buf)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(closeLog()) })

	slog.Error(
		"request failed with ghp_message_secret",
		"err", errors.New("https://x-access-token:ghp_error_secret@github.com/acme/widgets.git failed"),
		"token", "plain-provider-secret",
	)

	out := buf.String()
	require.NotEmpty(out)
	for _, secret := range []string{
		"ghp_message_secret",
		"ghp_error_secret",
		"plain-provider-secret",
		"x-access-token",
	} {
		assert.NotContains(out, secret)
	}
	assert.Contains(out, "[REDACTED]")
}

func TestConfigureLoggingRedactsTokensInConfiguredLogFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	var stderr bytes.Buffer
	logFile := filepath.Join(t.TempDir(), "forge.log")
	t.Setenv("KENN_FORGE_LOG_FILE", logFile)

	closeLog, err := configureLogging(&stderr)
	require.NoError(err)

	slog.Error(
		"request failed with glpat-message-secret",
		"err", errors.New("https://oauth2:glpat_url_secret@gitlab.example.com/acme/widgets.git failed"),
		"authorization", "Bearer plain-provider-secret",
	)
	require.NoError(closeLog())

	fileOut, err := os.ReadFile(logFile)
	require.NoError(err)
	for _, out := range []string{stderr.String(), string(fileOut)} {
		require.NotEmpty(out)
		for _, secret := range []string{
			"glpat-message-secret",
			"glpat_url_secret",
			"plain-provider-secret",
			"oauth2",
		} {
			assert.NotContains(out, secret)
		}
		assert.Contains(out, "[REDACTED]")
	}
}

func mainTestTokenSource(
	t *testing.T,
	platformName, host, envName, token string,
) tokenauth.Source {
	t.Helper()
	t.Setenv(envName, token)
	return tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: platformName, Host: host},
		Candidates: []tokenauth.Candidate{{
			Kind:    tokenauth.SourceKindEnv,
			EnvName: envName,
		}},
	}, tokenauth.Options{})
}

func TestRunMainShutdownStopsSignalsBeforeLongCleanup(t *testing.T) {
	var order []string
	record := func(name string) {
		order = append(order, name)
	}

	errs := runMainShutdown(t.Context(), mainShutdownCallbacks{
		StopSignals: func() {
			record("signals")
		},
		StopBackgroundLoops: func(context.Context) error {
			record("background")
			return nil
		},
		ShutdownMCPHTTP: func(context.Context) error {
			record("mcp-http")
			return nil
		},
		ShutdownPrimaryHTTP: func(context.Context) error {
			record("primary-http")
			return nil
		},
		StopSyncer: func() {
			record("syncer")
		},
		ShutdownProfiler: func(context.Context) error {
			record("profiler")
			return nil
		},
		CloseTelemetry: func() error {
			record("telemetry")
			return nil
		},
		CloseMCP: func() error {
			record("mcp")
			return nil
		},
		CloseDatabase: func() error {
			record("database")
			return nil
		},
	})

	assert.Empty(t, errs)
	assert.Equal(t, []string{
		"signals",
		"background",
		"mcp-http",
		"primary-http",
		"syncer",
		"profiler",
		"telemetry",
		"mcp",
		"database",
	}, order)
}

func TestRunMainShutdownBoundsMCPStoreCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release := make(chan struct{})
	databaseClosed := make(chan struct{})
	done := make(chan []mainShutdownError, 1)
	go func() {
		done <- runMainShutdown(ctx, mainShutdownCallbacks{
			CloseMCP: func() error {
				<-release
				return nil
			},
			CloseDatabase: func() error {
				close(databaseClosed)
				return nil
			},
		})
	}()

	select {
	case errs := <-done:
		// The canceled parent context may also record a canceled database
		// bound even though the database callback ran; only the MCP entry is
		// required.
		require.NotEmpty(errs)
		assert.Equal("close MCP temp store", errs[0].message)
		require.ErrorIs(errs[0].err, context.Canceled)
		for _, shutdownErr := range errs[1:] {
			assert.Equal("close database", shutdownErr.message)
			require.ErrorIs(shutdownErr.err, context.Canceled)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		<-done
		require.Fail("MCP temp-store cleanup ignored the shutdown context")
	}

	// Later cleanup must still run after the MCP cleanup budget expires:
	// the database closes while the abandoned MCP cleanup stays blocked.
	select {
	case <-databaseClosed:
	case <-time.After(100 * time.Millisecond):
		require.Fail("database cleanup did not run after MCP cleanup timed out")
	}
	close(release)
}

func TestRunBoundedShutdownHonorsDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	err := runBoundedShutdown(t.Context(), 20*time.Millisecond, func() error {
		<-release
		return nil
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestMCPStartupHandlerStaysUnavailableUntilFullServerSwap(t *testing.T) {
	switcher := server.NewSwitchHandler(newMCPStartupHandler())
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8092/mcp", nil)
	startup := httptest.NewRecorder()

	switcher.ServeHTTP(startup, request)

	assert.Equal(t, http.StatusServiceUnavailable, startup.Code)
	switcher.Swap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ready := httptest.NewRecorder()
	switcher.ServeHTTP(ready, request)
	assert.Equal(t, http.StatusNoContent, ready.Code)
}

func TestBindDaemonListenersOwnsOptionalMCPPortAndClosesPrimaryOnFailure(t *testing.T) {
	primaryPort, defaultMCPPort := reserveAdjacentPorts(t)
	explicitMCPPort := reserveFreePort(t)
	for explicitMCPPort == primaryPort || explicitMCPPort == defaultMCPPort {
		explicitMCPPort = reserveFreePort(t)
	}

	tests := []struct {
		name        string
		mcp         config.MCP
		occupiedMCP bool
		wantMCPPort int
		wantErr     string
	}{
		{name: "disabled"},
		{
			name: "default next port", mcp: config.MCP{Enabled: true},
			wantMCPPort: defaultMCPPort,
		},
		{
			name: "explicit port", mcp: config.MCP{Enabled: true, Port: explicitMCPPort},
			wantMCPPort: explicitMCPPort,
		},
		{
			name: "occupied port", mcp: config.MCP{Enabled: true, Port: explicitMCPPort},
			occupiedMCP: true, wantErr: "MCP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var occupied net.Listener
			if tt.occupiedMCP {
				var err error
				occupied, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(explicitMCPPort)))
				require.NoError(err)
				defer occupied.Close()
			}
			cfg := &config.Config{Host: "127.0.0.1", Port: primaryPort, MCP: tt.mcp}

			primary, mcpListener, err := bindDaemonListeners(cfg)
			if tt.wantErr != "" {
				require.ErrorContains(err, tt.wantErr)
				probe, listenErr := net.Listen("tcp", cfg.ListenAddr())
				require.NoError(listenErr, "primary listener must close after MCP bind failure")
				require.NoError(probe.Close())
				return
			}
			require.NoError(err)
			t.Cleanup(func() {
				require.NoError(primary.Close())
				if mcpListener != nil {
					require.NoError(mcpListener.Close())
				}
			})
			if tt.wantMCPPort == 0 {
				assert.Nil(mcpListener)
				return
			}
			require.NotNil(mcpListener)
			_, portText, splitErr := net.SplitHostPort(mcpListener.Addr().String())
			require.NoError(splitErr)
			assert.Equal(strconv.Itoa(tt.wantMCPPort), portText)
		})
	}
}

func reserveAdjacentPorts(t *testing.T) (int, int) {
	t.Helper()
	for range 100 {
		primary := reserveFreePort(t)
		if primary >= 65535 {
			continue
		}
		next, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(primary+1)))
		if err != nil {
			continue
		}
		require.NoError(t, next.Close())
		return primary, primary + 1
	}
	require.FailNow(t, "could not reserve adjacent loopback ports")
	return 0, 0
}

func TestRunClosesPrimaryListenerWhenProfilerStartFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(os.MkdirAll(dataDir, 0o700))
	cfgPath := filepath.Join(root, "config.toml")
	appPort := reserveFreePort(t)
	writeMinimalConfig(t, cfgPath, dataDir, appPort)
	t.Setenv("KENN_FORGE_GITHUB_TOKEN_UNSET_FOR_LOCK_E2E", "")

	err := run(serve.Options{
		ConfigPath:   cfgPath,
		ProfilerAddr: "0.0.0.0:0",
	})
	require.Error(err)
	assert.Contains(err.Error(), "profiler address")

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort))
	assert.Eventually(func() bool {
		ln, listenErr := net.Listen("tcp", addr)
		if listenErr != nil {
			return false
		}
		return ln.Close() == nil
	}, 2*time.Second, 25*time.Millisecond)
}

func TestResolveStartupReposExpandsConfiguredGlobs(t *testing.T) {
	assert := assert.New(t)
	cfg := &config.Config{
		Repos: []config.Repo{{Owner: "roborev-dev", Name: "*"}},
	}
	client := &testutil.FixtureClient{
		ReposByOwner: map[string][]*gh.Repository{
			"roborev-dev": {
				{
					Name:     new("kenn-forge"),
					Archived: new(false),
				},
				{
					Name:     new("archived"),
					Archived: new(true),
				},
			},
		},
	}

	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		nil,
		nil,
	)

	assert.Equal([]ghclient.RepoRef{
		{
			Platform:     "github",
			Owner:        "roborev-dev",
			Name:         "kenn-forge",
			PlatformHost: "github.com",
			RepoPath:     "roborev-dev/kenn-forge",
		},
		{
			Platform:     "github",
			Owner:        "roborev-dev",
			Name:         "archived",
			PlatformHost: "github.com",
			RepoPath:     "roborev-dev/archived",
			Archived:     true,
		},
	}, repos)
}

type archiveContextRepositoryReader struct {
	mainTestRepositoryReader
	sawArchiveContext *bool
}

func (r archiveContextRepositoryReader) GetRepository(
	ctx context.Context, ref platform.RepoRef,
) (platform.Repository, error) {
	*r.sawArchiveContext = ghclient.IsArchiveSyncBudgetContext(ctx)
	return r.mainTestRepositoryReader.GetRepository(ctx, ref)
}

func TestResolveStartupReposUsesArchiveRouteWithoutOrdinaryPAT(t *testing.T) {
	require := require.New(t)
	sawArchiveContext := false
	cfg := &config.Config{
		Repos: []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 2, Role: config.GitHubAppRoleArchive,
			PrivateKeyPath: "/keys/archive.pem", InstallationID: 20,
			InstallationAccount: "acme", RepositorySelection: "all",
		}},
	}
	registry := mustProviderRegistry(t, nil, archiveContextRepositoryReader{
		kind: platform.KindGitHub, host: "github.com",
		sawArchiveContext: &sawArchiveContext,
	})

	repos := resolveStartupRepos(t.Context(), cfg, registry, nil, nil)

	require.Len(repos, 1)
	assert.True(t, sawArchiveContext,
		"archive-covered startup resolution must use the archive route")
}

type getRepoFailingClient struct {
	*testutil.FixtureClient
}

func (getRepoFailingClient) GetRepository(
	context.Context, string, string,
) (*gh.Repository, error) {
	return nil, errors.New("transient resolve failure")
}

func TestResolveStartupReposPrefersResolvedOverFallbackDuplicates(t *testing.T) {
	assert := assert.New(t)
	// The exact entry fails resolution and falls back to a synthetic ref;
	// the overlapping glob resolves the same repo as archived. The resolved
	// metadata must win or the archived repo would be polled as live.
	cfg := &config.Config{
		Repos: []config.Repo{
			{Owner: "acme", Name: "archived"},
			{Owner: "acme", Name: "*"},
		},
	}
	client := getRepoFailingClient{&testutil.FixtureClient{
		ReposByOwner: map[string][]*gh.Repository{
			"acme": {{
				NodeID:   new("repo-acme-archived"),
				Name:     new("archived"),
				Owner:    &gh.User{Login: new("acme")},
				Archived: new(true),
			}},
		},
	}}

	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		nil,
		nil,
	)

	assert.Equal([]ghclient.RepoRef{{
		Platform:           "github",
		Owner:              "acme",
		Name:               "archived",
		PlatformHost:       "github.com",
		RepoPath:           "acme/archived",
		PlatformExternalID: "repo-acme-archived",
		Archived:           true,
		ConfiguredRepoPath: "acme/archived",
	}}, repos)
}

func TestResolveStartupReposKeepsExactReposWhenResolutionFails(t *testing.T) {
	assert := assert.New(t)
	cfg := &config.Config{
		Repos: []config.Repo{{Owner: "roborev-dev", Name: "kenn-forge"}},
	}

	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, nil),
		nil,
		nil,
	)

	assert.Equal([]ghclient.RepoRef{{
		Platform:           "github",
		Owner:              "roborev-dev",
		Name:               "kenn-forge",
		PlatformHost:       "github.com",
		RepoPath:           "roborev-dev/kenn-forge",
		ConfiguredRepoPath: "roborev-dev/kenn-forge",
	}}, repos)
}

func TestResolveStartupReposRecoversRenamedExactEntryFromCatalog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Now().UTC()
	before := db.GitHubRepoIdentity("github.com", "acme", "tools")
	before.PlatformRepoID = "repo-acme-tools"
	_, _, err := database.ReconcileRepositoryObservation(
		t.Context(), before, now.Add(-time.Hour),
	)
	require.NoError(err)
	after := db.GitHubRepoIdentity("github.com", "acme", "tools-new")
	after.PlatformRepoID = "repo-acme-tools"
	_, _, err = database.ReconcileRepositoryObservation(t.Context(), after, now)
	require.NoError(err)

	// The renamed repository resolves through the glob; the exact entry
	// still lists the old path and fails transiently. Catalog route
	// history recovers the stable identity so the fallback deduplicates
	// instead of tracking an identity-less duplicate on the stale route.
	cfg := &config.Config{Repos: []config.Repo{
		{Owner: "acme", Name: "tools"},
		{Owner: "acme", Name: "*"},
	}}
	client := getRepoFailingClient{&testutil.FixtureClient{
		ReposByOwner: map[string][]*gh.Repository{
			"acme": {{
				NodeID:   new("repo-acme-tools"),
				Name:     new("tools-new"),
				Owner:    &gh.User{Login: new("acme")},
				Archived: new(true),
			}},
		},
	}}

	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		database,
		nil,
	)

	require.Len(repos, 1)
	assert.Equal("tools-new", repos[0].Name)
	assert.Equal("repo-acme-tools", repos[0].PlatformExternalID)
	assert.True(repos[0].Archived)
	assert.Equal("acme/tools", repos[0].ConfiguredRepoPath)
}

func TestResolveStartupReposUsesStableIdentityAfterRouteReuse(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Now().UTC()
	original := db.GitHubRepoIdentity("github.com", "acme", "tools")
	original.PlatformRepoID = "repo-original"
	_, _, err := database.ReconcileRepositoryObservation(
		t.Context(), original, now.Add(-2*time.Hour),
	)
	require.NoError(err)
	renamed := db.GitHubRepoIdentity("github.com", "acme", "tools-renamed")
	renamed.PlatformRepoID = original.PlatformRepoID
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), renamed, now.Add(-time.Hour),
	)
	require.NoError(err)
	replacement := db.GitHubRepoIdentity("github.com", "acme", "tools")
	replacement.PlatformRepoID = "repo-replacement"
	_, _, err = database.ReconcileRepositoryObservation(t.Context(), replacement, now)
	require.NoError(err)

	cfg := &config.Config{Repos: []config.Repo{{
		Owner: "acme", Name: "tools", PlatformRepoID: original.PlatformRepoID,
	}}}
	client := &testutil.FixtureClient{}
	repos := resolveStartupRepos(
		t.Context(), cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		database, nil,
	)

	require.Len(repos, 1)
	require.Equal(original.PlatformRepoID, repos[0].PlatformExternalID)
	require.Equal("acme/tools-renamed", repos[0].RepoPath)

	withoutCatalog := resolveStartupRepos(
		t.Context(), cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		nil, nil,
	)
	require.Empty(withoutCatalog, "a stable identity must not fall back to a reused route")
}

func TestResolveStartupReposRegistersCredentialAliasForCatalogFallback(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Now().UTC()
	before := db.GitHubRepoIdentity("github.com", "acme", "tools")
	before.PlatformRepoID = "repo-acme-tools"
	_, _, err := database.ReconcileRepositoryObservation(
		t.Context(), before, now.Add(-time.Hour),
	)
	require.NoError(err)
	after := db.GitHubRepoIdentity("github.com", "acme", "tools-new")
	after.PlatformRepoID = "repo-acme-tools"
	_, _, err = database.ReconcileRepositoryObservation(t.Context(), after, now)
	require.NoError(err)

	client := getRepoFailingClient{&testutil.FixtureClient{}}
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key:    ghclient.RouteKey{Host: "github.com", Owner: "acme", Name: "tools"},
		Client: client,
	})
	require.NoError(err)
	cfg := &config.Config{Repos: []config.Repo{{Owner: "acme", Name: "tools"}}}

	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		database,
		map[string]*ghclient.HostRouter{"github.com": router},
	)

	require.Len(repos, 1)
	require.Equal("tools-new", repos[0].Name)
	// The recovered renamed route must keep resolving to the exact entry's
	// repo-scoped credential instead of falling through to owner or host
	// routes.
	route, err := router.RouteForRepo("acme", "tools-new")
	require.NoError(err)
	require.Equal("tools", route.Key.Name)
}

func TestResolveStartupReposFallsBackToDBForOfflineGlobs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := dbtest.Open(t)

	ctx := t.Context()
	widgets := db.GitHubRepoIdentity("github.com", "acme", "widgets")
	widgets.PlatformRepoID = "R_widgets"
	_, err := database.UpsertRepoByProviderID(ctx, widgets)
	require.NoError(err)
	tools := db.GitHubRepoIdentity("github.com", "acme", "tools")
	tools.PlatformRepoID = "R_tools"
	_, err = database.UpsertRepoByProviderID(ctx, tools)
	require.NoError(err)

	cfg := &config.Config{
		Repos: []config.Repo{{Owner: "acme", Name: "*"}},
	}

	repos := resolveStartupRepos(
		ctx, cfg, mustProviderRegistry(t, nil), database, nil,
	)

	assert.ElementsMatch([]ghclient.RepoRef{
		{
			Platform:     platform.KindGitHub,
			Owner:        "acme",
			Name:         "widgets",
			PlatformHost: "github.com",
		},
		{
			Platform:     platform.KindGitHub,
			Owner:        "acme",
			Name:         "tools",
			PlatformHost: "github.com",
		},
	}, repos)
}

func TestResolveStartupReposUsesProviderRegistryForGitLab(t *testing.T) {
	assert := assert.New(t)
	cfg := &config.Config{
		Repos: []config.Repo{{
			Platform:     "gitlab",
			PlatformHost: "gitlab.com",
			Owner:        "group/subgroup",
			Name:         "project",
		}},
	}
	registry := mustProviderRegistry(t, nil, mainTestRepositoryReader{
		kind: platform.KindGitLab,
		host: "gitlab.com",
	})

	repos := resolveStartupRepos(t.Context(), cfg, registry, nil, nil)

	assert.Equal([]ghclient.RepoRef{{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "group/subgroup",
		Name:               "project",
		RepoPath:           "group/subgroup/project",
		ConfiguredRepoPath: "group/subgroup/project",
	}}, repos)
}

func TestDefaultProviderFactoriesRegisterForgejoAndGitea(t *testing.T) {
	factories := defaultProviderFactories()

	assert := assert.New(t)
	assert.Contains(factories, string(platform.KindForgejo))
	assert.Contains(factories, string(platform.KindGitea))
}

func TestConfigureCloneTransportPolicyUsesExactProviderIdentity(t *testing.T) {
	clones := gitclone.New(t.TempDir(), nil)
	configureCloneTransportPolicy(clones, &config.Config{
		Platforms: []config.PlatformConfig{
			{
				Type:          "gitea",
				Host:          "gitea.example.test:3000",
				AllowInsecure: true,
			},
			{
				Type: "gitea",
				Host: "secure-gitea.example.test",
			},
		},
	})

	assert.True(t, clones.AllowsInsecureHTTP("gitea", "gitea.example.test:3000"))
	assert.False(t, clones.AllowsInsecureHTTP("forgejo", "gitea.example.test:3000"))
	assert.False(t, clones.AllowsInsecureHTTP("gitea", "secure-gitea.example.test"))
}

func TestBuildProviderControlPlaneKeepsForgeProviderHostsDistinct(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	database := dbtest.Open(t)

	callsByProvider := map[string][]providerFactoryInput{}
	factories := map[string]providerFactory{
		string(platform.KindForgejo): func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			callsByProvider[string(platform.KindForgejo)] = append(
				callsByProvider[string(platform.KindForgejo)], input,
			)
			return providerFactoryOutput{provider: mainTestRepositoryReader{
				kind: platform.KindForgejo,
				host: input.host,
			}}, nil
		},
		string(platform.KindGitea): func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			callsByProvider[string(platform.KindGitea)] = append(
				callsByProvider[string(platform.KindGitea)], input,
			)
			return providerFactoryOutput{provider: mainTestRepositoryReader{
				kind: platform.KindGitea,
				host: input.host,
			}}, nil
		},
	}

	set := tokenauth.NewSourceSet(tokenauth.Options{})
	cfg := &config.Config{
		SyncBudgetPerHour: 200,
		Platforms: []config.PlatformConfig{
			{Type: string(platform.KindForgejo), Host: "codeberg.org", TokenEnv: "FORGEJO_TEST_TOKEN"},
			{
				Type:          string(platform.KindGitea),
				Host:          "gitea.example.com",
				BaseURL:       "http://gitea-api.example.com:3000",
				AllowInsecure: true,
				TokenEnv:      "GITEA_TEST_TOKEN",
			},
		},
	}
	startup, err := buildProviderControlPlane(
		t.Context(), database,
		cfg,
		set,
		map[string]tokenauth.Source{
			providerHostKey(string(platform.KindForgejo), "codeberg.org"): mainTestTokenSource(
				t, string(platform.KindForgejo), "codeberg.org", "FORGEJO_TEST_TOKEN", "codeberg-token",
			),
			providerHostKey(string(platform.KindGitea), "gitea.example.com"): mainTestTokenSource(
				t, string(platform.KindGitea), "gitea.example.com", "GITEA_TEST_TOKEN", "gitea-token",
			),
		},
		factories, nil,
	)
	require.NoError(err)
	gitRoutes, err := buildGitStartup(cfg, set)
	require.NoError(err)
	gitRoutes.ApplyProviderControlPlane(&startup)

	forgejoCalls := callsByProvider[string(platform.KindForgejo)]
	giteaCalls := callsByProvider[string(platform.KindGitea)]
	require.Len(forgejoCalls, 1)
	require.Len(giteaCalls, 1)
	assert.Equal("codeberg.org", forgejoCalls[0].host)
	forgejoFactoryToken, err := forgejoCalls[0].tokenSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("codeberg-token", forgejoFactoryToken)
	assert.Equal("gitea.example.com", giteaCalls[0].host)
	assert.Equal("http://gitea-api.example.com:3000", giteaCalls[0].baseURL)
	assert.True(giteaCalls[0].allowInsecure)
	giteaFactoryToken, err := giteaCalls[0].tokenSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("gitea-token", giteaFactoryToken)
	assert.NotSame(forgejoCalls[0].rateTracker, giteaCalls[0].rateTracker)
	assert.NotSame(forgejoCalls[0].budget, giteaCalls[0].budget)
	forgejoCloneSource := gitRoutes.cloneAuth["codeberg.org"]
	giteaCloneSource := gitRoutes.cloneAuth["gitea.example.com"]
	require.NotNil(forgejoCloneSource)
	require.NotNil(giteaCloneSource)
	forgejoToken, err := forgejoCloneSource.Token(t.Context())
	require.NoError(err)
	giteaToken, err := giteaCloneSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("codeberg-token", forgejoToken)
	assert.Equal("gitea-token", giteaToken)
	assert.NotNil(gitRoutes.cloneSources[tokenauth.Key{
		Platform: string(platform.KindForgejo), Host: "codeberg.org",
	}])
	assert.NotNil(gitRoutes.cloneSources[tokenauth.Key{
		Platform: string(platform.KindGitea), Host: "gitea.example.com",
	}])
	// Clone auth is the dedicated host-level source registered in the
	// SourceSet, not the provider's own source, so config reload can
	// re-point it via tokenauth.CloneKey.
	forgejoCloneManaged, ok := set.Get(tokenauth.CloneKey("codeberg.org"))
	require.True(ok)
	assert.Same(forgejoCloneManaged, forgejoCloneSource)
	giteaCloneManaged, ok := set.Get(tokenauth.CloneKey("gitea.example.com"))
	require.True(ok)
	assert.Same(giteaCloneManaged, giteaCloneSource)

	forgejoReader, err := startup.registry.RepositoryReader(platform.KindForgejo, "codeberg.org")
	require.NoError(err)
	giteaReader, err := startup.registry.RepositoryReader(platform.KindGitea, "gitea.example.com")
	require.NoError(err)
	assert.NotNil(forgejoReader)
	assert.NotNil(giteaReader)
}

func TestBuildProviderControlPlaneOrDegradedKeepsHealthyProviderWhenFactoryFails(t *testing.T) {
	require := require.New(t)

	set := tokenauth.NewSourceSet(tokenauth.Options{})
	startup, err := buildProviderControlPlaneOrDegraded(
		t.Context(), dbtest.Open(t), &config.Config{}, set,
		map[string]tokenauth.Source{
			providerHostKey(string(platform.KindForgejo), "forge.example.com"): mainTestTokenSource(
				t, string(platform.KindForgejo), "forge.example.com", "FORGEJO_TEST_TOKEN", "forgejo-token",
			),
			providerHostKey(string(platform.KindGitea), "gitea.example.com"): mainTestTokenSource(
				t, string(platform.KindGitea), "gitea.example.com", "GITEA_TEST_TOKEN", "gitea-token",
			),
		},
		map[string]providerFactory{
			string(platform.KindForgejo): func(context.Context, providerFactoryInput) (providerFactoryOutput, error) {
				return providerFactoryOutput{}, errors.New("provider API unavailable")
			},
			string(platform.KindGitea): func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
				return providerFactoryOutput{provider: mainTestRepositoryReader{
					kind: platform.KindGitea,
					host: input.host,
				}}, nil
			},
		},
		nil,
	)
	require.NoError(err)
	_, err = startup.registry.Provider(platform.KindGitea, "gitea.example.com")
	require.NoError(err)
}

func TestBuildProviderControlPlaneForServeReturnsFactoryErrorWhenSyncDisabled(t *testing.T) {
	require := require.New(t)

	set := tokenauth.NewSourceSet(tokenauth.Options{})
	_, err := buildProviderControlPlaneForServe(
		t.Context(), dbtest.Open(t), &config.Config{}, set,
		map[string]tokenauth.Source{
			providerHostKey(string(platform.KindForgejo), "forge.example.com"): mainTestTokenSource(
				t, string(platform.KindForgejo), "forge.example.com", "FORGEJO_TEST_TOKEN", "forgejo-token",
			),
		},
		map[string]providerFactory{
			string(platform.KindForgejo): func(context.Context, providerFactoryInput) (providerFactoryOutput, error) {
				return providerFactoryOutput{}, errors.New("provider API unavailable")
			},
		},
		nil, true,
	)
	require.ErrorContains(err, "create Forgejo client for forge.example.com: provider API unavailable")
}

func TestBuildProviderControlPlaneUsesRegisteredFactoryForFutureProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	database := dbtest.Open(t)

	called := false
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	codebergSource := mainTestTokenSource(
		t, "codeberg", "codeberg.org", "CODEBERG_TEST_TOKEN", "codeberg-token",
	)
	startup, err := buildProviderControlPlane(
		t.Context(), database,
		&config.Config{},
		set,
		map[string]tokenauth.Source{
			providerHostKey("codeberg", "codeberg.org"): codebergSource,
		},
		map[string]providerFactory{
			"codeberg": func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
				called = true
				assert.Equal("codeberg.org", input.host)
				token, err := input.tokenSource.Token(t.Context())
				require.NoError(err)
				assert.Equal("codeberg-token", token)
				return providerFactoryOutput{
					provider: mainTestRepositoryReader{
						kind: platform.Kind("codeberg"),
						host: input.host,
					},
				}, nil
			},
		}, nil,
	)
	require.NoError(err)
	assert.True(called)

	reader, err := startup.registry.RepositoryReader(platform.Kind("codeberg"), "codeberg.org")
	require.NoError(err)
	assert.NotNil(reader)
}

func TestBuildProviderControlPlaneSharedHostCloneAuthUsesHostLevelSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	database := dbtest.Open(t)
	t.Setenv("SHARED_FORGE_TOKEN", "shared-token")
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")

	// Two providers on one host with the same credential chain, mirroring
	// the only multi-provider-per-host layout startup validation accepts.
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{
			{
				Type:     string(platform.KindForgejo),
				Host:     "code.example.com",
				TokenEnv: "SHARED_FORGE_TOKEN",
			},
			{
				Type:     string(platform.KindGitea),
				Host:     "code.example.com",
				TokenEnv: "SHARED_FORGE_TOKEN",
			},
		},
	}
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	providerSources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	require.Len(providerSources, 2)

	factories := map[string]providerFactory{
		string(platform.KindForgejo): func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			return providerFactoryOutput{provider: mainTestRepositoryReader{
				kind: platform.KindForgejo,
				host: input.host,
			}}, nil
		},
		string(platform.KindGitea): func(_ context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			return providerFactoryOutput{provider: mainTestRepositoryReader{
				kind: platform.KindGitea,
				host: input.host,
			}}, nil
		},
	}
	startup, err := buildProviderControlPlane(t.Context(), database, cfg, set, providerSources, factories, nil)
	require.NoError(err)
	gitRoutes, err := buildGitStartup(cfg, set)
	require.NoError(err)
	gitRoutes.ApplyProviderControlPlane(&startup)

	// Clone auth must be the host-level source under tokenauth.CloneKey,
	// not whichever provider source map iteration yielded first: reload
	// updates the host key from the config's effective per-host chain, so
	// pointing git at a provider source would detach clone auth from
	// reload whenever that provider entry is removed or loses its token.
	cloneSource := gitRoutes.cloneAuth["code.example.com"]
	require.NotNil(cloneSource)
	cloneManaged, ok := set.Get(tokenauth.CloneKey("code.example.com"))
	require.True(ok)
	assert.Same(cloneManaged, cloneSource)
	forgejoKey := providerHostKey(string(platform.KindForgejo), "code.example.com")
	giteaKey := providerHostKey(string(platform.KindGitea), "code.example.com")
	assert.NotSame(providerSources[forgejoKey], cloneSource)
	assert.NotSame(providerSources[giteaKey], cloneSource)
	token, err := cloneSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("shared-token", token)
}

func TestStartupFallbackKeepsPersistedGlobMatchesInAPIs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	database := dbtest.Open(t)

	forge := db.GitHubRepoIdentity("github.com", "roborev-dev", "kenn-forge")
	forge.PlatformRepoID = "R_kenn_forge"
	_, err := database.UpsertRepoByProviderID(t.Context(), forge)
	require.NoError(err)
	worker := db.GitHubRepoIdentity("github.com", "roborev-dev", "worker")
	worker.PlatformRepoID = "R_worker"
	_, err = database.UpsertRepoByProviderID(t.Context(), worker)
	require.NoError(err)

	cfgPath := filepath.Join(dir, "config.toml")
	cfg := &config.Config{
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		BasePath:       "/",
		DataDir:        dir,
		Repos: []config.Repo{
			{Owner: "roborev-dev", Name: "*"},
		},
		Activity: config.Activity{
			ViewMode:  "flat",
			TimeRange: "7d",
		},
	}
	require.NoError(cfg.Save(cfgPath))

	client := &testutil.FixtureClient{
		ListRepositoriesByOwnerFn: func(
			context.Context, string,
		) ([]*gh.Repository, error) {
			return nil, errors.New("offline")
		},
	}
	repos := resolveStartupRepos(
		t.Context(),
		cfg,
		mustProviderRegistry(t, map[string]ghclient.Client{"github.com": client}),
		database,
		nil,
	)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": client},
		database, nil, repos, 0, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := server.NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{},
	)

	reposReq := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
	reposReq.Host = "127.0.0.1:8091"
	reposRR := httptest.NewRecorder()
	srv.ServeHTTP(reposRR, reposReq)
	require.Equal(http.StatusOK, reposRR.Code, reposRR.Body.String())

	var listed []struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	require.NoError(json.NewDecoder(reposRR.Body).Decode(&listed))
	require.Len(listed, 2)
	assert.ElementsMatch([]string{"kenn-forge", "worker"}, []string{
		listed[0].Name,
		listed[1].Name,
	})

	settingsReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	settingsReq.Host = "127.0.0.1:8091"
	settingsRR := httptest.NewRecorder()
	srv.ServeHTTP(settingsRR, settingsReq)
	require.Equal(http.StatusOK, settingsRR.Code, settingsRR.Body.String())

	var settings struct {
		Repos []struct {
			Owner            string `json:"owner"`
			Name             string `json:"name"`
			MatchedRepoCount int    `json:"matched_repo_count"`
		} `json:"repos"`
	}
	require.NoError(json.NewDecoder(settingsRR.Body).Decode(&settings))
	require.Len(settings.Repos, 1)
	assert.Equal("roborev-dev", settings.Repos[0].Owner)
	assert.Equal("*", settings.Repos[0].Name)
	assert.Equal(2, settings.Repos[0].MatchedRepoCount)
}

func mustProviderRegistry(
	t *testing.T,
	clients map[string]ghclient.Client,
	providers ...platform.Provider,
) *platform.Registry {
	t.Helper()
	registry, err := ghclient.NewProviderRegistry(clients, providers...)
	require.NoError(t, err)
	return registry
}

type mainTestRepositoryReader struct {
	kind platform.Kind
	host string
}

func (r mainTestRepositoryReader) Platform() platform.Kind {
	return r.kind
}

func (r mainTestRepositoryReader) Host() string {
	return r.host
}

func (r mainTestRepositoryReader) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadRepositories: true}
}

func (r mainTestRepositoryReader) GetRepository(
	_ context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	return platform.Repository{Ref: ref}, nil
}

func (r mainTestRepositoryReader) ListRepositories(
	_ context.Context,
	owner string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return []platform.Repository{{
		Ref: platform.RepoRef{
			Platform: r.kind,
			Host:     r.host,
			Owner:    owner,
			Name:     "project",
			RepoPath: owner + "/project",
		},
	}}, nil
}

func TestRunCLIConfigReadPort(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("port = 9123\n"), 0o644))

	var stdout bytes.Buffer
	err := runCLI([]string{"config", "read", "-config", cfgPath, "port"}, &stdout)
	require.NoError(err)
	assert.Equal("9123\n", stdout.String())
}

func TestRootHelpListsEveryPublicCommandWithoutStartingServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var stdout bytes.Buffer
	started := false
	cmd := newRootCommand(cliOptions{
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: io.Discard,
		RunServer: func(serve.Options) error {
			started = true
			return nil
		},
	})
	cmd.SetArgs([]string{"--help"})

	require.NoError(cmd.Execute())

	for _, name := range []string{
		"activity", "agent-hook", "api", "archive", "config", "docs",
		"daemon", "issues", "mcp", "pulls", "quickstart", "rate-limits", "repo-summaries",
		"repos", "serve", "stacks", "sync", "version", "workspaces",
	} {
		assert.Contains(stdout.String(), name)
	}
	assert.NotContains(stdout.String(), "pty-owner")
	assert.NotContains(stdout.String(), "completion")
	assert.False(started)
}

func TestRootUnknownCommandDoesNotStartServer(t *testing.T) {
	require := require.New(t)
	started := false
	cmd := newRootCommand(cliOptions{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
		RunServer: func(serve.Options) error {
			started = true
			return nil
		},
	})
	cmd.SetArgs([]string{"not-a-command"})

	err := cmd.Execute()

	require.Error(err)
	require.False(started)
}

func TestRootNestedHelpExposesCommandFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "version", args: []string{"version", "--help"}, want: []string{"--json"}},
		{name: "config read", args: []string{"config", "read", "--help"}, want: []string{"--config"}},
		{name: "docs add", args: []string{"docs", "add-folder", "--help"}, want: []string{"--config", "--id", "--name", "--daemon"}},
		{name: "archive report", args: []string{"archive", "report", "--help"}, want: []string{"--days", "--start", "--end", "--format", "--repo"}},
		{name: "agent hook run", args: []string{"agent-hook", "run", "--help"}, want: []string{"--agent", "--config", "--source"}},
		{name: "daemon start", args: []string{"daemon", "start", "--help"}, want: []string{"--config"}},
		{name: "daemon status", args: []string{"daemon", "status", "--help"}, want: []string{"--config", "--json"}},
		{name: "daemon stop", args: []string{"daemon", "stop", "--help"}, want: []string{"--config"}},
		{name: "daemon restart", args: []string{"daemon", "restart", "--help"}, want: []string{"--config"}},
		{name: "mcp quickstart", args: []string{"mcp", "quickstart", "--help"}, want: []string{"--config", "--json", "--timeout"}},
		{name: "serve", args: []string{"serve", "--help"}, want: []string{"--config", "--pprof-addr"}},
		{name: "api", args: []string{"api", "--help"}, want: []string{"list", "--config", "-d", "-i", "--timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var stdout bytes.Buffer
			cmd := newRootCommand(cliOptions{
				Stdin:     strings.NewReader(""),
				Stdout:    &stdout,
				Stderr:    io.Discard,
				RunServer: func(serve.Options) error { return errors.New("serve should not start") },
			})
			cmd.SetArgs(tt.args)

			require.NoError(cmd.Execute())
			for _, value := range tt.want {
				assert.Contains(stdout.String(), value)
			}
		})
	}
}

func TestRootAPIUsesControlModeForExplicitServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var receivedMethod, receivedPath string
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		assert.NoError(err)
		w.Header().Set("Content-Type", "application/json")
		_, err = io.WriteString(w, `{"ok":true}`)
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	var stdout bytes.Buffer
	cmd := newRootCommand(cliOptions{
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: io.Discard,
		RunServer: func(serve.Options) error {
			return errors.New("serve should not start")
		},
	})
	cmd.SetArgs([]string{
		"--server", server.URL,
		"--output", "yaml",
		"api", "POST", "/widgets", "name: sample",
	})

	require.NoError(cmd.Execute())

	assert.Equal(http.MethodPost, receivedMethod)
	assert.Equal("/api/v1/widgets", receivedPath)
	assert.JSONEq(`{"name":"sample"}`, string(receivedBody))
	assert.Contains(stdout.String(), "ok: true")
}

func TestRootAPIRejectsRelayFlagsInControlMode(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	tests := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{
			name:     "data",
			args:     []string{"--server", server.URL, "api", "-d", `{"x":1}`, "POST", "/widgets"},
			wantFlag: "--data",
		},
		{
			name:     "include",
			args:     []string{"--server", server.URL, "api", "-i", "POST", "/widgets"},
			wantFlag: "--include",
		},
		{
			name:     "config",
			args:     []string{"--server", server.URL, "api", "--config", filepath.Join(t.TempDir(), "config.toml"), "POST", "/widgets"},
			wantFlag: "--config",
		},
		{
			name:     "local timeout",
			args:     []string{"--server", server.URL, "api", "--timeout", "1s", "POST", "/widgets", "name: sample"},
			wantFlag: "--timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests = 0
			cmd := newRootCommand(cliOptions{
				Stdin:     strings.NewReader(""),
				Stdout:    io.Discard,
				Stderr:    io.Discard,
				RunServer: func(serve.Options) error { return errors.New("serve should not start") },
			})
			cmd.SetArgs(tt.args)

			err := cmd.Execute()

			require.Error(err)
			assert.Contains(err.Error(), tt.wantFlag)
			assert.Zero(requests)
		})
	}
}

func TestRootRejectsControlFlagsForNonControlCommands(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{name: "bare server", args: []string{"--server", "http://forge.test"}, wantFlag: "--server"},
		{name: "version output", args: []string{"--output", "yaml", "version"}, wantFlag: "--output"},
		{name: "version timeout", args: []string{"--timeout", "1s", "version"}, wantFlag: "--timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			started := false
			cmd := newRootCommand(cliOptions{
				Stdin:  strings.NewReader(""),
				Stdout: io.Discard,
				Stderr: io.Discard,
				RunServer: func(serve.Options) error {
					started = true
					return nil
				},
			})
			cmd.SetArgs(tt.args)

			err := cmd.Execute()

			require.Error(err)
			assert.Contains(err.Error(), tt.wantFlag)
			assert.False(started)
		})
	}
}

func TestRunCLIVersionPreservesHumanOutput(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate := version, commit, buildDate
	version, commit, buildDate = "1.2.3", "abc1234", "2026-07-12T12:00:00Z"
	t.Cleanup(func() {
		version, commit, buildDate = originalVersion, originalCommit, originalBuildDate
	})

	var stdout bytes.Buffer
	err := runCLI([]string{"version"}, &stdout)

	require.NoError(t, err)
	assert.Equal(t, "kenn-forge 1.2.3 (abc1234) built 2026-07-12T12:00:00Z\n", stdout.String())
}

func TestRunCLIVersionJSONReturnsStableBuildMetadata(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate := version, commit, buildDate
	version, commit, buildDate = "1.2.3", "abc1234", "2026-07-12T12:00:00Z"
	t.Cleanup(func() {
		version, commit, buildDate = originalVersion, originalCommit, originalBuildDate
	})

	var stdout bytes.Buffer
	err := runCLI([]string{"version", "--json"}, &stdout)

	require.NoError(t, err)
	assert.JSONEq(t, `{
		"name": "kenn-forge",
		"version": "1.2.3",
		"commit": "abc1234",
		"buildDate": "2026-07-12T12:00:00Z"
	}`, stdout.String())
}

func TestRunCLIConfigReadPortCreatesDefaultConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	var stdout bytes.Buffer
	err := runCLI([]string{"config", "read", "-config", cfgPath, "port"}, &stdout)
	require.NoError(err)
	assert.Equal("8091\n", stdout.String())

	content, err := os.ReadFile(cfgPath)
	require.NoError(err)
	assert.Contains(string(content), "port = 8091")
}

func TestRunCLIBareInvocationShowsHelpWithoutStartingServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	original := runServer
	t.Cleanup(func() { runServer = original })
	runServer = func(serve.Options) error {
		return errors.New("serve should not start")
	}

	var stdout bytes.Buffer
	err := runCLI(nil, &stdout)

	require.NoError(err)
	assert.Contains(stdout.String(), "Usage:")
	assert.Contains(stdout.String(), "daemon")
	assert.Contains(stdout.String(), "serve")
}

func TestRunCLIServeSubcommandUsesServerRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	original := runServer
	t.Cleanup(func() { runServer = original })
	var got serve.Options
	runServer = func(opts serve.Options) error {
		got = opts
		return nil
	}

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var stdout bytes.Buffer
	err := runCLI([]string{"serve", "-config", cfgPath}, &stdout)

	require.NoError(err)
	assert.Equal(cfgPath, got.ConfigPath)
	assert.Empty(got.ProfilerAddr)
	assert.Empty(stdout.String())
}

func TestRunCLIServePassesProfilerAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	original := runServer
	t.Cleanup(func() { runServer = original })
	t.Setenv("KENN_FORGE_PPROF_ADDR", "127.0.0.1:6060")
	var got serve.Options
	runServer = func(opts serve.Options) error {
		got = opts
		return nil
	}

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var stdout bytes.Buffer
	err := runCLI([]string{
		"serve",
		"-config", cfgPath,
		"-pprof-addr", "127.0.0.1:7070",
	}, &stdout)

	require.NoError(err)
	assert.Equal(cfgPath, got.ConfigPath)
	assert.Equal("127.0.0.1:7070", got.ProfilerAddr)
	assert.Empty(stdout.String())
}

func TestRunCLIServeDefaultsProfilerAddressFromEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	original := runServer
	t.Cleanup(func() { runServer = original })
	t.Setenv("KENN_FORGE_PPROF_ADDR", "127.0.0.1:6060")
	var got serve.Options
	runServer = func(opts serve.Options) error {
		got = opts
		return nil
	}

	var stdout bytes.Buffer
	err := runCLI([]string{"serve"}, &stdout)

	require.NoError(err)
	assert.Equal(config.DefaultConfigPath(), got.ConfigPath)
	assert.Equal("127.0.0.1:6060", got.ProfilerAddr)
	assert.Empty(stdout.String())
}

func TestRunCLIControlCommandsDoNotStartServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	original := runServer
	t.Cleanup(func() { runServer = original })
	runServer = func(serve.Options) error {
		return errors.New("serve should not start")
	}

	var stdout bytes.Buffer
	err := runCLI([]string{"--server", "http://forge.test", "quickstart"}, &stdout)

	require.NoError(err)
	assert.Contains(stdout.String(), `"api_base_url": "http://forge.test/api/v1"`)
	assert.Contains(stdout.String(), "kenn-forge api GET /pulls")
}

func TestRunCLIPtyOwnerRejectsMissingRequiredFlags(t *testing.T) {
	var stdout bytes.Buffer

	err := runCLI([]string{"pty-owner"}, &stdout)

	require.Error(t, err)
	require.Contains(t, err.Error(), "session")
}

func TestRunCLIPtyOwnerParsesBeforeServerStartup(t *testing.T) {
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")
	var stdout bytes.Buffer

	err := runCLI([]string{
		"pty-owner",
		"-root", t.TempDir(),
		"-session", "bad/session",
		"-cwd", t.TempDir(),
		"-command-json", `["sh","-c","exit 0"]`,
	}, &stdout)

	require.Error(t, err)
	require.Contains(t, err.Error(), "unsafe pty owner session")
}
