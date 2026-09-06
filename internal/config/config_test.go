package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/tokenauth"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestWorkspaceDefaultSidebarView(t *testing.T) {
	t.Run("defaults to diff", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, ""))
		require.NoError(t, err)
		assert.Equal(t, "diff", cfg.Workspaces.DefaultSidebarView)
	})

	t.Run("round trips item", func(t *testing.T) {
		cfg, saved := roundTripConfigString(t, "[workspaces]\ndefault_sidebar_view = \"item\"\n")
		assert.Equal(t, "item", cfg.Workspaces.DefaultSidebarView)
		assert.Equal(t, "item", saved.Workspaces.DefaultSidebarView)
	})

	t.Run("rejects invalid value", func(t *testing.T) {
		_, err := Load(writeConfig(t, "[workspaces]\ndefault_sidebar_view = \"pr\"\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "workspaces.default_sidebar_view")
	})
}

func roundTripConfigString(t *testing.T, content string) (*Config, *Config) {
	t.Helper()
	cfg, err := Load(writeConfig(t, content))
	require.NoError(t, err)
	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))
	cfg2, err := Load(savePath)
	require.NoError(t, err)
	return cfg, cfg2
}

func TestMCPConfigDefaultsToBackendPortPlusOne(t *testing.T) {
	assert := assert.New(t)
	cfg, err := Load(writeConfig(t, "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\n"))
	require.NoError(t, err)
	assert.Equal(8092, cfg.MCPPort())
	assert.Equal("127.0.0.1:8092", cfg.MCPListenAddr())
	assert.Equal(int64(128<<20), cfg.MCPDiffCacheBytes())
}

func TestMCPConfigRoundTrip(t *testing.T) {
	require := require.New(t)
	cfg, err := Load(writeConfig(t, ""))
	require.NoError(err)
	cfg.MCP = MCP{Enabled: true, Port: 9192, DiffCacheMB: 256}
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(cfg.Save(path))
	reloaded, err := Load(path)
	require.NoError(err)
	assert.Equal(t, MCP{Enabled: true, Port: 9192, DiffCacheMB: 256}, reloaded.MCP)
}

func TestMCPConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "default port overflows",
			content: "host = \"127.0.0.1\"\nport = 65535\n[mcp]\nenabled = true\n",
			wantErr: "MCP port",
		},
		{
			name:    "matches backend port",
			content: "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\nport = 8091\n",
			wantErr: "MCP port",
		},
		{
			name:    "explicit port below range",
			content: "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\nport = -1\n",
			wantErr: "MCP port",
		},
		{
			name:    "explicit port above range",
			content: "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\nport = 65536\n",
			wantErr: "MCP port",
		},
		{
			name:    "non-loopback host",
			content: "host = \"192.0.2.10\"\nport = 8091\n[mcp]\nenabled = true\n",
			wantErr: "loopback",
		},
		{
			name:    "negative diff cache",
			content: "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\ndiff_cache_mb = -1\n",
			wantErr: "diff cache",
		},
		{
			name:    "diff cache byte conversion overflows",
			content: "host = \"127.0.0.1\"\nport = 8091\n[mcp]\nenabled = true\ndiff_cache_mb = 8796093022208\n",
			wantErr: "diff cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func setFakeGHCLI(t *testing.T, stdout string) {
	t.Helper()
	setFakeGHCLIScript(t, fakeGHCLIOptions{Stdout: stdout})
}

type fakeGHCLIOptions struct {
	// Stdout is echoed verbatim on success.
	Stdout string
	// Stderr is echoed to stderr regardless of exit code.
	Stderr string
	// ExitCode is the exit status of the fake gh. Default 0.
	ExitCode int
	// SleepSeconds, if >0, makes the fake sleep before exiting.
	SleepSeconds int
}

// setFakeGHCLIScript writes a fake `gh` to a temp dir and points PATH
// at it. The fake records its argv to <tempdir>/argv (one entry per
// invocation, newline-separated), then emits stdout/stderr/exit per
// opts. The argv-file path is returned and also exported via
// FAKE_GH_ARGV so the script can locate it.
//
// To keep PATH minimal (the fake gh should be the only resolvable
// `gh`), the helper embeds absolute paths for any external tools the
// script needs — currently just `sleep` when SleepSeconds > 0.
func setFakeGHCLIScript(t *testing.T, opts fakeGHCLIOptions) string {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	writeFakeGHCLI(t, dir, fakeGHCLIScript(t, opts))
	t.Setenv("FAKE_GH_ARGV", argvPath)
	return argvPath
}

func setFakeGHCLIWithScript(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	writeFakeGHCLI(t, dir, script)
	t.Setenv("FAKE_GH_ARGV", argvPath)
	return argvPath
}

func writeFakeGHCLI(t *testing.T, dir, script string) {
	t.Helper()
	require.NoError(t, os.WriteFile(fakeGHCLIPath(dir), []byte(script), 0o755))
	t.Setenv("PATH", dir)
	// The production 5s exec bound exists to catch a hung gh; under a
	// loaded parallel suite run it can instead kill the fake before it
	// records argv. Relax it so these tests assert flag/retry behavior,
	// not subprocess scheduling latency. Tests that exercise the timeout
	// itself pin their own value afterwards.
	setGHAuthExecTimeout(t, time.Minute)
}

// setGHAuthExecTimeout overrides the gh exec deadline for one test and
// restores it on cleanup. Safe without locking: this package has no
// parallel tests (the fake-gh helpers already rely on t.Setenv, which
// rejects t.Parallel).
func setGHAuthExecTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := ghAuthExecTimeout
	ghAuthExecTimeout = d
	t.Cleanup(func() { ghAuthExecTimeout = old })
}

// readFakeGHArgv returns the recorded argv strings, one per
// invocation, in call order. Returns nil when no calls were made.
func readFakeGHArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	raw := strings.TrimRight(string(data), "\r\n")
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

func TestLoadRejectsNonPositiveNotificationIntervals(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "sync interval zero",
			content: `
[notifications]
sync_interval = "0s"
`,
			wantErr: "notifications.sync_interval must be positive",
		},
		{
			name: "propagation interval negative",
			content: `
[notifications]
propagation_interval = "-1s"
`,
			wantErr: "notifications.propagation_interval must be positive",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadAppliesActivePRRefreshDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, ``))
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(defaultActivePRRefreshInterval, cfg.ActivePRRefreshInterval)
	assert.Equal(defaultActivePRWindow, cfg.ActivePRWindow)
	assert.Equal(2*time.Minute, cfg.ActivePRRefreshDuration())
	assert.Equal(4*time.Hour, cfg.ActivePRWindowDuration())
}

func TestLoadRejectsNonPositiveActivePRRefreshDurations(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "refresh interval zero",
			content: `
active_pr_refresh_interval = "0s"
`,
			wantErr: "active_pr_refresh_interval must be positive",
		},
		{
			name: "window negative",
			content: `
active_pr_window = "-1s"
`,
			wantErr: "active_pr_window must be positive",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestSaveAppliesNotificationDefaultsForInMemoryConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := &Config{
		SyncInterval:        "5m",
		GitHubTokenEnv:      "KENN_FORGE_GITHUB_TOKEN",
		DefaultPlatformHost: "github.com",
		Host:                "127.0.0.1",
		Port:                8091,
		BasePath:            "/",
		DataDir:             DefaultDataDir(),
		SyncBudgetPerHour:   defaultSyncBudgetPerHour,
		Repos: []Repo{{
			Owner: "acme",
			Name:  "widget",
		}},
		Activity: Activity{
			ViewMode:  defaultViewMode,
			TimeRange: defaultTimeRange,
		},
	}
	path := filepath.Join(t.TempDir(), "config.toml")

	require.NoError(cfg.Save(path))
	reloaded, err := Load(path)
	require.NoError(err)

	assert.True(reloaded.NotificationsEnabled())
	assert.Equal(defaultNotificationSyncInterval, reloaded.Notifications.SyncInterval)
	assert.Equal(defaultNotificationPropagationInterval, reloaded.Notifications.PropagationInterval)
	assert.Equal(25, reloaded.Notifications.BatchSize)
}

func TestLoadValid(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
sync_interval = "10m"
github_token_env = "MY_TOKEN"
host = "127.0.0.1"
port = 9000

[[repos]]
owner = "apache"
name = "arrow"

[[repos]]
owner = "ibis-project"
name = "ibis"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 2)
	assert.Equal("apache/arrow", cfg.Repos[0].FullName())
	assert.Equal("10m", cfg.SyncInterval)
	assert.Equal(9000, cfg.Port)
}

func TestLoadCasefoldsRepoOwnerAndName(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "Org"
name = "Foo"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("org", cfg.Repos[0].Owner)
	assert.Equal("foo", cfg.Repos[0].Name)
}

func TestLoadRejectsDuplicateReposAfterCasefolding(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "Org"
name = "Foo"

[[repos]]
owner = "org"
name = "foo"
`)

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate repo "org/foo"`)
}

func TestLoadDefaults(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "test"
name = "repo"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("5m", cfg.SyncInterval)
	assert.Equal("127.0.0.1", cfg.Host)
	assert.Equal(8091, cfg.Port)
	assert.Equal("github.com", cfg.DefaultPlatformHost)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("github", cfg.Repos[0].Platform)
	assert.Equal("github.com", cfg.Repos[0].PlatformHostOrDefault())
	assert.True(cfg.NotificationsEnabled())
}

func TestNotificationsAlwaysEnabled(t *testing.T) {
	// Notifications are a built-in capability with no enable/disable
	// setting. A legacy config that still carries the removed
	// [notifications] enabled key must load without error (the key is
	// ignored) and keep notifications on.
	cfg, err := Load(writeConfig(t, `
[[repos]]
owner = "test"
name = "repo"

[notifications]
enabled = false
`))
	require.NoError(t, err)
	assert.True(t, cfg.NotificationsEnabled())
}

func TestLoadDocFoldersRoundTrips(t *testing.T) {
	assert := assert.New(t)
	cfg, cfg2 := roundTripConfigString(t, `
[[doc_folders]]
id = "notes"
name = "Notes"
path = "/tmp/notes"
daemon = "work"
`)

	require.Len(t, cfg.DocFolders, 1)
	assert.Equal("notes", cfg.DocFolders[0].ID)
	assert.Equal("Notes", cfg.DocFolders[0].Name)
	assert.Equal("/tmp/notes", cfg.DocFolders[0].Path)
	assert.Equal("work", cfg.DocFolders[0].Daemon)
	require.Len(t, cfg2.DocFolders, 1)
	assert.Equal(cfg.DocFolders[0], cfg2.DocFolders[0])
}

func TestLoadDocFoldersRejectsDeprecatedKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "notebooks",
			body: `
[[notebooks]]
id = "notes"
path = "/tmp/notes"
`,
		},
		{
			name: "vaults",
			body: `
[[vaults]]
id = "notes"
path = "/tmp/notes"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "[[doc_folders]]")
		})
	}
}

func TestLoadDocFoldersCanonicalizesPathAndDefaultName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(os.MkdirAll(filepath.Join(home, "Notes"), 0o755))

	path := writeConfig(t, `
[[doc_folders]]
id = "notes"
path = "~/Notes"
daemon = "  work  "
`)

	cfg, err := Load(path)
	require.NoError(err)
	require.Len(cfg.DocFolders, 1)
	folder := cfg.DocFolders[0]
	assert.Equal("notes", folder.ID)
	assert.Equal("Notes", folder.Name)
	assert.True(filepath.IsAbs(folder.Path), "folder path should be absolute")
	assert.True(strings.HasSuffix(folder.Path, "Notes"))
	assert.Equal("work", folder.Daemon)
}

func TestLoadDocFoldersRejectsDuplicateIDs(t *testing.T) {
	path := writeConfig(t, `
[[doc_folders]]
id = "notes"
path = "/tmp/a"

[[doc_folders]]
id = "notes"
path = "/tmp/b"
`)

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), `doc_folders: duplicate id "notes"`)
}

func TestLoadDocFoldersRejectsNonSegmentSafeID(t *testing.T) {
	for _, id := range []string{"a/b", "with space", ".."} {
		t.Run(id, func(t *testing.T) {
			path := writeConfig(t, "[[doc_folders]]\nid = \""+id+"\"\npath = \"/tmp/a\"\n")
			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "may contain only")
		})
	}
}

func TestLoadDocFoldersRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "missing id",
			body: `[[doc_folders]]
path = "/tmp/notes"`,
		},
		{
			name: "missing path",
			body: `[[doc_folders]]
id = "notes"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), "doc_folders")
		})
	}
}

func TestLoadRoundTripsHostAuthorityConfig(t *testing.T) {
	assert := assert.New(t)

	cfg, cfg2 := roundTripConfigString(t, `
host = "127.0.0.1"
port = 8091
allowed_hosts = ["forge.local:8091", "studio.tailnet:8091"]
trust_reverse_proxy = true
`)

	assert.Equal(
		[]string{"forge.local:8091", "studio.tailnet:8091"},
		cfg.AllowedHosts,
	)
	assert.True(cfg.TrustReverseProxy)
	assert.Equal(cfg.AllowedHosts, cfg2.AllowedHosts)
	assert.Equal(cfg.TrustReverseProxy, cfg2.TrustReverseProxy)
}

func TestListenAddrBracketsIPv6Host(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, err := Load(writeConfig(t, `
host = "::1"
port = 8091
`))
	require.NoError(err)

	assert.Equal("[::1]:8091", cfg.ListenAddr())
	assert.Equal("127.0.0.1:8091", (&Config{
		Host: "127.0.0.1",
		Port: 8091,
	}).ListenAddr())
}

func TestLoadRoundTripsAPIAuthConfig(t *testing.T) {
	assert := assert.New(t)

	cfg, cfg2 := roundTripConfigString(t, `
[api]
require_auth = true
`)

	assert.True(cfg.API.RequireAuth)
	assert.True(cfg2.API.RequireAuth)
}

func TestLoadRoundTripsTailscaleServeUsers(t *testing.T) {
	assert := assert.New(t)

	cfg, cfg2 := roundTripConfigString(t, `
[api]
require_auth = true

[api.tailscale_serve]
enabled = true
allowed_users = [" User-A@Example.COM ", "user-b@example.com"]
`)

	want := TailscaleServeAPI{
		Enabled: true,
		AllowedUsers: []string{
			"user-a@example.com",
			"user-b@example.com",
		},
	}
	assert.Equal(want, cfg.API.TailscaleServe)
	assert.Equal(want, cfg2.API.TailscaleServe)
}

func TestTailscaleServeUsersRejectInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		name  string
		block string
	}{
		{name: "enabled without API auth", block: "enabled = true\nallowed_users = [\"user@example.com\"]"},
		{name: "enabled without users", block: "enabled = true"},
		{name: "empty user", block: "enabled = true\nallowed_users = [\"\"]"},
		{name: "display name", block: "enabled = true\nallowed_users = [\"User <user@example.com>\"]"},
		{name: "multiple users in one header", block: "enabled = true\nallowed_users = [\"a@example.com,b@example.com\"]"},
		{name: "multiple at signs", block: "enabled = true\nallowed_users = [\"a@b@example.com\"]"},
		{name: "case insensitive duplicate", block: "enabled = true\nallowed_users = [\"USER@example.com\", \"user@example.com\"]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "[api.tailscale_serve]\n"+test.block+"\n"))
			require.ErrorContains(t, err, "api.tailscale_serve")
		})
	}
}

func TestLoadNormalizesDefaultPlatformHost(t *testing.T) {
	assert := assert.New(t)
	cfg, cfg2 := roundTripConfigString(t, `
default_platform_host = "GHE.Example.COM"

[[repos]]
owner = "test"
name = "repo"
`)

	assert.Equal("ghe.example.com", cfg.DefaultPlatformHost)
	assert.Equal("ghe.example.com", cfg2.DefaultPlatformHost)
}

func TestLoadAppliesDefaultPlatformHostToLegacyGitHubRepos(t *testing.T) {
	assert := assert.New(t)
	// github_token_env is github.com-only, so a GHE-primary setup names
	// its host token through a [[platforms]] entry instead.
	path := writeConfig(t, `
default_platform_host = "ghe.example.com"

[[platforms]]
type = "github"
host = "ghe.example.com"
token_env = "GHE_TOKEN"

[[repos]]
owner = "Acme"
name = "Widgets"
`)
	t.Setenv("GHE_TOKEN", "ghe-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("github", cfg.Repos[0].Platform)
	assert.Equal("ghe.example.com", cfg.Repos[0].PlatformHost)
	assert.Equal("ghe.example.com", cfg.Repos[0].PlatformHostOrDefault())
	assert.Equal(
		"ghe-secret",
		cfg.TokenForPlatformHost("github", cfg.Repos[0].PlatformHost, ""),
	)
}

func TestLoadNoRepos(t *testing.T) {
	path := writeConfig(t, `host = "127.0.0.1"`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Repos)
}

func TestLoadInvalidSyncInterval(t *testing.T) {
	path := writeConfig(t, `
sync_interval = "not-a-duration"
[[repos]]
owner = "a"
name = "b"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadRejectsNonLoopback(t *testing.T) {
	path := writeConfig(t, `
host = "0.0.0.0"
[[repos]]
owner = "a"
name = "b"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadRepoMissingFields(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadRepoNameDotGitOnly(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = ".git"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadRejectsGlobInOwner(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "acme-*"
name = "widgets"
`)

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "glob syntax in owner")
}

func TestLoadRejectsGlobInOwnerBeforeGitHubRefNormalization(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "acme-*"
name = "https://github.com/acme/widgets"
`)

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "glob syntax in owner")
}

func TestLoadRejectsGitHubRepoCredentialOnNameGlob(t *testing.T) {
	for _, tc := range []struct {
		name       string
		credential string
	}{
		{name: "token env", credential: `token_env = "ACME_TOKEN"`},
		{name: "token file", credential: `token_file = "tokens/acme"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
[[repos]]
owner = "acme"
name = "widgets-*"
`+tc.credential)

			_, err := Load(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), "GitHub repo token override requires an exact repository name")
		})
	}
}

func TestRepoHasNameGlob(t *testing.T) {
	assert := assert.New(t)

	assert.False((&Repo{Owner: "acme", Name: "widgets"}).HasNameGlob())
	assert.True((&Repo{Owner: "acme", Name: "widgets-*"}).HasNameGlob())
	assert.True((&Repo{Owner: "acme", Name: "widgets-?"}).HasNameGlob())
}

func TestGitHubToken(t *testing.T) {
	t.Setenv("TEST_GH_TOKEN", "secret123")
	cfg := &Config{GitHubTokenEnv: "TEST_GH_TOKEN"}
	assert.Equal(t, "secret123", cfg.GitHubToken())
}

func TestGitHubTokenFallsBackToGHCli(t *testing.T) {
	setFakeGHCLI(t, "gh-secret")
	t.Setenv("TEST_GH_TOKEN", "")

	cfg := &Config{GitHubTokenEnv: "TEST_GH_TOKEN"}
	assert.Equal(t, "gh-secret", cfg.GitHubToken())
}

func TestGitHubTokenPrefersEnvVarOverGHCli(t *testing.T) {
	setFakeGHCLI(t, "gh-secret")
	t.Setenv("TEST_GH_TOKEN", "secret123")

	cfg := &Config{GitHubTokenEnv: "TEST_GH_TOKEN"}
	assert.Equal(t, "secret123", cfg.GitHubToken())
}

func TestBasePathValidation(t *testing.T) {
	base := `
[[repos]]
owner = "a"
name = "b"
`
	tests := []struct {
		name    string
		value   string
		wantErr bool
		wantBP  string
	}{
		{"default", "", false, "/"},
		{"root", "/", false, "/"},
		{"simple", "kenn-forge", false, "/kenn-forge/"},
		{"with slashes", "/kenn-forge/", false, "/kenn-forge/"},
		{"nested", "/apps/kenn-forge", false, "/apps/kenn-forge/"},
		{"dot segment", "/../evil", true, ""},
		{"single dot", "/./path", true, ""},
		{"special chars", "/mid<script>", true, ""},
		{"quotes", `/mid"man`, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := ""
			if tt.value != "" {
				extra = `base_path = "` + tt.value + `"`
			}
			path := writeConfig(t, extra+"\n"+base)
			cfg, err := Load(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBP, cfg.BasePath)
		})
	}
}

func TestGitHubTokenReturnsEmptyWhenGHCliUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TEST_GH_TOKEN", "")

	cfg := &Config{GitHubTokenEnv: "TEST_GH_TOKEN"}
	assert.Empty(t, cfg.GitHubToken())
}

func TestForgeHomeOverridesDefaultPaths(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("KENN_FORGE_HOME", "/tmp/kenn-forge-test")

	assert.Equal(
		filepath.FromSlash("/tmp/kenn-forge-test/config.toml"),
		DefaultConfigPath(),
	)
	assert.Equal("/tmp/kenn-forge-test", DefaultDataDir())
}

func TestDefaultPathsWithoutForgeHome(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("KENN_FORGE_HOME", "")
	t.Setenv("HOME", "/fakehome")

	assert.Equal(
		filepath.FromSlash("/fakehome/.kenn/forge/config.toml"),
		DefaultConfigPath(),
	)
	assert.Equal(filepath.FromSlash("/fakehome/.kenn/forge"), DefaultDataDir())
}

func TestDBPath(t *testing.T) {
	cfg := &Config{DataDir: "/tmp/kenn-forge-test"}
	expected := filepath.FromSlash("/tmp/kenn-forge-test/forge.db")
	assert.Equal(t, expected, cfg.DBPath())
}

func TestLoadActivityDefaults(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("threaded", cfg.Activity.ViewMode)
	assert.Equal("7d", cfg.Activity.TimeRange)
	assert.False(cfg.Activity.HideClosed)
	assert.False(cfg.Activity.HideBots)
	assert.True(cfg.Activity.CollapseThreads)
	assert.False(cfg.Activity.UseWorkspaceActivityForRecency)
}

func TestLoadActivityExplicit(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
view_mode = "threaded"
time_range = "30d"
hide_closed = true
hide_bots = true
collapse_threads = true
use_workspace_activity_for_recency = true
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("threaded", cfg.Activity.ViewMode)
	assert.Equal("30d", cfg.Activity.TimeRange)
	assert.True(cfg.Activity.HideClosed)
	assert.True(cfg.Activity.HideBots)
	assert.True(cfg.Activity.CollapseThreads)
	assert.True(cfg.Activity.UseWorkspaceActivityForRecency)
}

func TestLoadActivityExplicitCollapseThreadsFalse(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
collapse_threads = false
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(cfg.Activity.CollapseThreads)
}

func TestLoadActivityInvalidViewMode(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
view_mode = "kanban"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadActivityInvalidTimeRange(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
time_range = "1y"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadNormalizesRepoNames(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		repoName  string
		wantOwner string
		wantName  string
	}{
		{
			name:      "strips .git suffix",
			owner:     "apache",
			repoName:  "arrow.git",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "HTTPS URL in name",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "HTTPS URL with .git in name",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow.git",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "SSH URL in name",
			owner:     "ignored",
			repoName:  "git@github.com:apache/arrow.git",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "SSH URL without .git in name",
			owner:     "ignored",
			repoName:  "git@github.com:apache/arrow",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "SSH URI-style URL",
			owner:     "ignored",
			repoName:  "ssh://git@github.com/apache/arrow.git",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "SSH URI-style with port",
			owner:     "ignored",
			repoName:  "ssh://git@github.com:22/apache/arrow.git",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "omitted platform GitLab SSH URL not parsed",
			owner:     "ignored",
			repoName:  "ssh://git@gitlab.com/apache/arrow.git",
			wantOwner: "ignored",
			wantName:  "ssh://git@gitlab.com/apache/arrow",
		},
		{
			name:      "bare github.com path in name",
			owner:     "ignored",
			repoName:  "github.com/apache/arrow",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "HTTPS URL in owner",
			owner:     "https://github.com/apache/arrow.git",
			repoName:  "ignored",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "plain owner and name unchanged",
			owner:     "apache",
			repoName:  "arrow",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "URL with query string",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow?tab=readme",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "URL with fragment",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow#readme",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "URL with trailing slash",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow/",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "URL with .git and trailing slash",
			owner:     "ignored",
			repoName:  "https://github.com/apache/arrow.git/",
			wantOwner: "apache",
			wantName:  "arrow",
		},
		{
			name:      "repo literally named github.com",
			owner:     "acme",
			repoName:  "github.com",
			wantOwner: "acme",
			wantName:  "github.com",
		},
		{
			name:      "non-github HTTPS host not parsed",
			owner:     "ignored",
			repoName:  "https://notgithub.com/apache/arrow",
			wantOwner: "ignored",
			wantName:  "https://notgithub.com/apache/arrow",
		},
		{
			name:      "non-github SSH host not parsed",
			owner:     "ignored",
			repoName:  "git@notgithub.com:apache/arrow.git",
			wantOwner: "ignored",
			wantName:  "git@notgithub.com:apache/arrow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			cfg := fmt.Sprintf(`
[[repos]]
owner = %q
name = %q
`, tt.owner, tt.repoName)
			path := writeConfig(t, cfg)
			got, err := Load(path)
			require.NoError(t, err)
			assert.Equal(tt.wantOwner, got.Repos[0].Owner)
			assert.Equal(tt.wantName, got.Repos[0].Name)
		})
	}
}

func TestLoadOmittedPlatformGitLabURLRemainsGitHub(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "Ignored"
name = "https://gitlab.com/MyGroup/SubGroup/MyProject.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("github", cfg.Repos[0].Platform)
	assert.Equal("github.com", cfg.Repos[0].PlatformHostOrDefault())
	assert.Equal("ignored", cfg.Repos[0].Owner)
	assert.Equal("https://gitlab.com/mygroup/subgroup/myproject", cfg.Repos[0].Name)
}

func TestLoadRejectsMalformedGitHubRef(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		repoName string
	}{
		{
			name:     "HTTPS URL missing repo",
			owner:    "ignored",
			repoName: "https://github.com/apache/",
		},
		{
			name:     "HTTPS URL owner only",
			owner:    "ignored",
			repoName: "https://github.com/apache",
		},
		{
			name:     "SSH URL missing repo",
			owner:    "ignored",
			repoName: "git@github.com:apache",
		},
		{
			name:     "bare HTTPS prefix",
			owner:    "ignored",
			repoName: "https://github.com/",
		},
		{
			name:     "bare github.com slash",
			owner:    "ignored",
			repoName: "github.com/",
		},
		{
			name:     "bare SSH prefix",
			owner:    "ignored",
			repoName: "git@github.com:",
		},
		{
			name:     "HTTPS host only no slash",
			owner:    "ignored",
			repoName: "https://github.com",
		},
		{
			name:     "SSH URI bare host",
			owner:    "ignored",
			repoName: "ssh://git@github.com",
		},
		{
			name:     "SSH URI bare host with port",
			owner:    "ignored",
			repoName: "ssh://git@github.com:22",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`
[[repos]]
owner = %q
name = %q
`, tt.owner, tt.repoName)
			path := writeConfig(t, cfg)
			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "incomplete GitHub reference")
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	assert := assert.New(t)
	cfg, cfg2 := roundTripConfigString(t, `
sync_interval = "10m"
github_token_env = "MY_TOKEN"
host = "127.0.0.1"
port = 9000
base_path = "/app/"

[[repos]]
owner = "apache"
name = "arrow"

[activity]
view_mode = "threaded"
time_range = "30d"
hide_closed = true
hide_bots = true
collapse_threads = true
use_workspace_activity_for_recency = true
`)
	assert.Equal("MY_TOKEN", cfg2.GitHubTokenEnv)
	assert.Equal(cfg.SyncInterval, cfg2.SyncInterval)
	assert.Equal(cfg.Host, cfg2.Host)
	assert.Equal(cfg.Port, cfg2.Port)
	assert.Equal(cfg.BasePath, cfg2.BasePath)
	assert.Len(cfg2.Repos, len(cfg.Repos))
	assert.Equal(cfg.Repos[0].FullName(), cfg2.Repos[0].FullName())
	assert.Equal(cfg.Activity.ViewMode, cfg2.Activity.ViewMode)
	assert.Equal(cfg.Activity.TimeRange, cfg2.Activity.TimeRange)
	assert.Equal(cfg.Activity.HideClosed, cfg2.Activity.HideClosed)
	assert.Equal(cfg.Activity.HideBots, cfg2.Activity.HideBots)
	assert.Equal(cfg.Activity.CollapseThreads, cfg2.Activity.CollapseThreads)
	assert.Equal(cfg.Activity.UseWorkspaceActivityForRecency, cfg2.Activity.UseWorkspaceActivityForRecency)
}

func TestSavePreservesDefaults(t *testing.T) {
	assert := assert.New(t)
	_, cfg2 := roundTripConfigString(t, `
[[repos]]
owner = "a"
name = "b"
`)
	assert.Equal("5m", cfg2.SyncInterval)
	assert.Equal("127.0.0.1", cfg2.Host)
	assert.Equal(8091, cfg2.Port)
	assert.Equal("threaded", cfg2.Activity.ViewMode)
	assert.Equal("7d", cfg2.Activity.TimeRange)
}

func TestEnsureDefaultCreatesFile(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")

	require.NoError(t, EnsureDefault(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(content, "sync_interval")
	assert.Contains(content, "github_token_env")
	assert.Contains(content, "[[repos]]")
	assert.Contains(content, "[notifications]")
}

func TestEnsureDefaultSkipsExisting(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
	require.NoError(t, EnsureDefault(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `owner = "a"`)
}

func TestEnsureDefaultIdempotent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	require.NoError(EnsureDefault(path))
	info1, err := os.Stat(path)
	require.NoError(err)

	require.NoError(EnsureDefault(path))
	info2, err := os.Stat(path)
	require.NoError(err)

	require.Equal(info1.ModTime(), info2.ModTime())
}

func TestLoadRepoPlatformHost(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
platform_host = "github.example.com"
token_env = "GHE_TOKEN"

[[repos]]
owner = "ibis-project"
name = "ibis"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 2)
	assert.Equal("github", cfg.Repos[0].Platform)
	assert.Equal("github.example.com", cfg.Repos[0].PlatformHost)
	assert.Equal("GHE_TOKEN", cfg.Repos[0].TokenEnv)
	assert.Equal("github", cfg.Repos[1].Platform)
	assert.Empty(cfg.Repos[1].PlatformHost)
	assert.Equal("github.com", cfg.Repos[1].PlatformHostOrDefault())
	assert.Empty(cfg.Repos[1].TokenEnv)
}

func TestLoadTokenFilePathsAreNormalized(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	require.NoError(os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(dir, "config", "config.toml")
	require.NoError(os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	require.NoError(os.WriteFile(cfgPath, []byte(`
github_token_env = "KENN_FORGE_GITHUB_TOKEN"

[[platforms]]
type = "gitlab"
host = "gitlab.com"
token_file = "tokens/gitlab"

[[repos]]
owner = "acme"
name = "widget"
platform = "github"
token_file = "~/tokens/github"
`), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(err)

	assert.Equal(filepath.Join(filepath.Dir(cfgPath), "tokens", "gitlab"), cfg.Platforms[0].TokenFile)
	assert.Equal(filepath.Join(home, "tokens", "github"), cfg.Repos[0].TokenFile)
}

func TestConfigTokenSourceDescriptorPrecedence(t *testing.T) {
	assert := assert.New(t)
	cfg := &Config{
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Platforms: []PlatformConfig{{
			Type: "gitlab", Host: "gitlab.com", TokenFile: "/platform/file", TokenEnv: "PLATFORM_TOKEN",
		}},
	}

	desc := cfg.TokenSourceForPlatformHost("gitlab", "gitlab.com", "REPO_TOKEN", "/repo/file")

	require.Len(t, desc.Candidates, 4)
	assert.Equal(tokenauth.SourceKindFile, desc.Candidates[0].Kind)
	assert.Equal("/repo/file", desc.Candidates[0].FilePath)
	assert.Equal(tokenauth.SourceKindEnv, desc.Candidates[1].Kind)
	assert.Equal("REPO_TOKEN", desc.Candidates[1].EnvName)
	assert.Equal(tokenauth.SourceKindFile, desc.Candidates[2].Kind)
	assert.Equal("/platform/file", desc.Candidates[2].FilePath)
	assert.Equal(tokenauth.SourceKindEnv, desc.Candidates[3].Kind)
	assert.Equal("PLATFORM_TOKEN", desc.Candidates[3].EnvName)
}

func TestTokenSourceForPlatformHostScopesGitHubTokenEnvToDefaultHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The env var being set must not matter: a non-default GitHub host's
	// candidate chain may only contain the host-scoped gh credential, so
	// the public-GitHub token can never be sent to an Enterprise or
	// self-hosted GitHub host that lacks an explicit token.
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "public-github-token")
	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}

	ghe := cfg.TokenSourceForPlatformHost("github", "ghe.example.com", "", "")
	require.Len(ghe.Candidates, 1)
	assert.Equal(tokenauth.SourceKindGitHubCLI, ghe.Candidates[0].Kind)
	assert.Equal("ghe.example.com", ghe.Candidates[0].Host)

	def := cfg.TokenSourceForPlatformHost("github", "github.com", "", "")
	require.Len(def.Candidates, 2)
	assert.Equal(tokenauth.SourceKindEnv, def.Candidates[0].Kind)
	assert.Equal("KENN_FORGE_GITHUB_TOKEN", def.Candidates[0].EnvName)
	assert.Equal(tokenauth.SourceKindGitHubCLI, def.Candidates[1].Kind)
	assert.Equal("github.com", def.Candidates[1].Host)
}

func TestConfigProviderTokenSourcesKeepsCredentiallessPlatformHosts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A platform host whose token config was removed must stay in the
	// plans with an empty candidate chain: config reload updates live
	// sources from this list, and dropping the host would leave its old
	// credential active until restart.
	cfg := &Config{
		Platforms: []PlatformConfig{{Type: "gitlab", Host: "gitlab.example.com"}},
	}

	plans := cfg.ProviderTokenSources()

	require.Len(plans, 2)
	assert.Equal(
		tokenauth.Key{Platform: "gitlab", Host: "gitlab.example.com"},
		plans[0].Descriptor.Key,
	)
	assert.False(plans[0].Required)
	assert.Empty(plans[0].Descriptor.Candidates)
	assert.Equal(
		tokenauth.Key{Platform: "github", Host: "github.com"},
		plans[1].Descriptor.Key,
	)
}

func TestConfigCloneTokenDescriptorsUseFirstNonEmptyHostChain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The ownerless host fallback carries the chain every tokened provider
	// on the host agrees on. The credential-less forgejo entry does not
	// conflict, so the host descriptor carries the only non-empty chain
	// (gitlab's); a host whose providers are all credential-less keeps an
	// empty chain so reload clears a previously tokened clone source
	// instead of leaving it active.
	cfg := &Config{
		Platforms: []PlatformConfig{
			{Type: "forgejo", Host: "code.example.com"},
			{Type: "gitlab", Host: "code.example.com", TokenEnv: "SHARED"},
			{Type: "gitea", Host: "tokenless.example.com"},
		},
	}

	descs := cfg.CloneTokenDescriptors()

	require.Len(descs, 3)
	assert.Equal(tokenauth.CloneKey("code.example.com"), descs[0].Key)
	assert.Equal(
		[]tokenauth.Candidate{{Kind: tokenauth.SourceKindEnv, EnvName: "SHARED"}},
		descs[0].Candidates,
	)
	assert.Equal(tokenauth.CloneKey("tokenless.example.com"), descs[1].Key)
	assert.Empty(descs[1].Candidates)
	assert.Equal(tokenauth.CloneKey("github.com"), descs[2].Key)
	assert.NotEmpty(descs[2].Candidates)
}

func TestConfigProviderTokenSourcesPlansEffectiveDescriptors(t *testing.T) {
	assert := assert.New(t)
	cfg := &Config{
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Platforms: []PlatformConfig{
			{Type: "github", Host: "github.com", TokenEnv: "PLATFORM_GITHUB_TOKEN"},
			{Type: "gitlab", Host: "gitlab.com"},
			{Type: "forgejo", Host: "codeberg.org", TokenEnv: "PLATFORM_FORGEJO_TOKEN"},
		},
		Repos: []Repo{
			{
				Owner:        "acme",
				Name:         "widget",
				Platform:     "github",
				PlatformHost: "github.com",
				TokenEnv:     "REPO_GITHUB_TOKEN",
			},
			{
				Owner:        "gitlab-org",
				Name:         "example",
				Platform:     "gitlab",
				PlatformHost: "gitlab.com",
			},
		},
	}

	plans := cfg.ProviderTokenSources()

	require.Len(t, plans, 4)
	assert.True(plans[0].Required)
	assert.Equal(tokenauth.Key{
		Platform: "github",
		Host:     "github.com",
		Scope:    "repo:acme/widget",
	}, plans[0].Descriptor.Key)
	assert.Equal("REPO_GITHUB_TOKEN", plans[0].Descriptor.Candidates[0].EnvName)
	assert.Equal("PLATFORM_GITHUB_TOKEN", plans[0].Descriptor.Candidates[1].EnvName)
	assert.True(plans[1].Required)
	assert.Equal(tokenauth.Key{Platform: "gitlab", Host: "gitlab.com"}, plans[1].Descriptor.Key)
	assert.Empty(plans[1].Descriptor.Candidates)
	assert.False(plans[2].Required)
	assert.Equal(tokenauth.Key{Platform: "github", Host: "github.com"}, plans[2].Descriptor.Key)
	assert.Equal("PLATFORM_GITHUB_TOKEN", plans[2].Descriptor.Candidates[0].EnvName)
	assert.False(plans[3].Required)
	assert.Equal(tokenauth.Key{Platform: "forgejo", Host: "codeberg.org"}, plans[3].Descriptor.Key)
	assert.Equal("PLATFORM_FORGEJO_TOKEN", plans[3].Descriptor.Candidates[0].EnvName)
}

func TestConfigProviderTokenSourcesIncludesOptionalDefaultGitHub(t *testing.T) {
	assert := assert.New(t)
	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}

	plans := cfg.ProviderTokenSources()

	require.Len(t, plans, 1)
	assert.False(plans[0].Required)
	assert.Equal(tokenauth.Key{Platform: "github", Host: "github.com"}, plans[0].Descriptor.Key)
	require.Len(t, plans[0].Descriptor.Candidates, 2)
	assert.Equal(tokenauth.SourceKindEnv, plans[0].Descriptor.Candidates[0].Kind)
	assert.Equal("KENN_FORGE_GITHUB_TOKEN", plans[0].Descriptor.Candidates[0].EnvName)
	assert.Equal(tokenauth.SourceKindGitHubCLI, plans[0].Descriptor.Candidates[1].Kind)
	assert.Equal("github.com", plans[0].Descriptor.Candidates[1].Host)
}

func TestValidateAllowsDistinctGitHubRepoTokenSources(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[repos]]
owner = "acme"
name = "one"
platform_host = "ghe.example.com"
token_file = "/tokens/a"

[[repos]]
owner = "acme"
name = "two"
platform_host = "ghe.example.com"
token_file = "/tokens/b"
`))

	require.NoError(t, err)
}

func TestValidateRejectsConflictingRepoTokenSourcesOnSharedHost(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[repos]]
platform = "gitlab"
owner = "group"
name = "one"
token_env = "GITLAB_TOKEN_A"

[[repos]]
platform = "gitlab"
owner = "group"
name = "two"
token_env = "GITLAB_TOKEN_B"
`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `conflicting token source for gitlab host "gitlab.com"`)
}

func TestValidateAllowsRepoTokenEnvMatchingPlatformFallback(t *testing.T) {
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "gitlab.com"
token_env = "GITLAB_SHARED_TOKEN"

[[repos]]
platform = "gitlab"
owner = "group"
name = "explicit"
token_env = "GITLAB_SHARED_TOKEN"

[[repos]]
platform = "gitlab"
owner = "group"
name = "fallback"
`)

	_, err := Load(path)
	require.NoError(t, err)
}

func TestSaveRoundTripTokenFile(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &Config{
		SyncInterval:        defaultSyncInterval,
		GitHubTokenEnv:      defaultGitHubTokenEnv,
		DefaultPlatformHost: defaultPlatformHost,
		Host:                defaultHost,
		Port:                defaultPort,
		Platforms: []PlatformConfig{{
			Type: "gitlab", Host: "gitlab.com", TokenFile: "/tokens/gitlab",
		}},
		Repos: []Repo{{
			Owner: "acme", Name: "widget", Platform: "gitlab", PlatformHost: "gitlab.com", TokenFile: "/tokens/repo",
		}},
	}

	require.NoError(t, cfg.Save(path))
	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal("/tokens/gitlab", loaded.Platforms[0].TokenFile)
	assert.Equal("/tokens/repo", loaded.Repos[0].TokenFile)
}

func TestLoadPlatformConfigGitLabToken(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "gitlab.com"
token_env = "KENN_FORGE_GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "acme"
name = "widgets"
`)
	t.Setenv("KENN_FORGE_GITLAB_TOKEN", "gitlab-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Platforms, 1)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Platforms[0].Type)
	assert.Equal("gitlab.com", cfg.Platforms[0].Host)
	assert.Equal("KENN_FORGE_GITLAB_TOKEN", cfg.Platforms[0].TokenEnv)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.com", cfg.Repos[0].PlatformHost)
	assert.Equal(
		"gitlab-secret",
		cfg.TokenForPlatformHost("gitlab", "gitlab.com", ""),
	)
}

func TestLoadPlatformConfigGiteaLikeToken(t *testing.T) {
	tests := []struct {
		kind   string
		host   string
		env    string
		secret string
		owner  string
		name   string
	}{
		{kind: "forgejo", host: "codeberg.org", env: "KENN_FORGE_FORGEJO_TOKEN", secret: "forgejo-secret", owner: "forgejo", name: "forgejo"},
		{kind: "gitea", host: "gitea.com", env: "KENN_FORGE_GITEA_TOKEN", secret: "gitea-secret", owner: "gitea", name: "tea"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			assert := assert.New(t)
			path := writeConfig(t, fmt.Sprintf(`
[[platforms]]
type = %q
host = %q
token_env = %q

[[repos]]
platform = %q
platform_host = %q
owner = %q
name = %q
`, tt.kind, tt.host, tt.env, tt.kind, tt.host, tt.owner, tt.name))
			t.Setenv(tt.env, tt.secret)

			cfg, err := Load(path)
			require.NoError(t, err)
			require.Len(t, cfg.Platforms, 1)
			require.Len(t, cfg.Repos, 1)
			assert.Equal(tt.kind, cfg.Platforms[0].Type)
			assert.Equal(tt.host, cfg.Platforms[0].Host)
			assert.Equal(tt.env, cfg.Platforms[0].TokenEnv)
			assert.Equal(tt.kind, cfg.Repos[0].Platform)
			assert.Equal(tt.host, cfg.Repos[0].PlatformHost)
			assert.Equal(tt.secret, cfg.TokenForPlatformHost(tt.kind, tt.host, ""))
		})
	}
}

func TestLoadGiteaExplicitBaseURL(t *testing.T) {
	path := writeConfig(t, `
[[platforms]]
type = "gitea"
host = "gitea.example.test:3000"
base_url = "http://gitea-api.example.test:3000/root/"
allow_insecure = true
token_env = "GITEA_PRIVATE_TOKEN"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Platforms, 1)
	assert.Equal(t, PlatformConfig{
		Type:          "gitea",
		Host:          "gitea.example.test:3000",
		BaseURL:       "http://gitea-api.example.test:3000/root",
		AllowInsecure: true,
		TokenEnv:      "GITEA_PRIVATE_TOKEN",
	}, cfg.Platforms[0])
}

func TestLoadRejectsInvalidGiteaExplicitBaseURL(t *testing.T) {
	tests := []struct {
		name          string
		baseURL       string
		allowInsecure bool
		want          string
	}{
		{name: "relative", baseURL: "gitea.example.test:3000", want: "absolute http(s) URL"},
		{name: "unsupported scheme", baseURL: "ssh://gitea.example.test", want: "scheme must be http or https"},
		{name: "missing host", baseURL: "https:///api", want: "absolute http(s) URL"},
		{name: "user info", baseURL: "https://user@gitea.example.test", want: "must not include user info"},
		{name: "query", baseURL: "https://gitea.example.test?x=1", want: "must not include a query string"},
		{name: "empty query", baseURL: "https://gitea.example.test?", want: "must not include a query string"},
		{name: "fragment", baseURL: "https://gitea.example.test#api", want: "must not include a fragment"},
		{name: "http without acknowledgement", baseURL: "http://gitea.example.test", want: "set allow_insecure = true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`
[[platforms]]
type = "gitea"
host = "gitea.example.test"
base_url = %q
allow_insecure = %t
`, tt.baseURL, tt.allowInsecure))

			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadRejectsGiteaTransportFieldsForOtherProviders(t *testing.T) {
	path := writeConfig(t, `
[[platforms]]
type = "forgejo"
host = "forgejo.example.test"
base_url = "https://api.example.test"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "supported only for gitea")
}

func TestSaveRoundTripGiteaExplicitBaseURL(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &Config{
		SyncInterval:        defaultSyncInterval,
		GitHubTokenEnv:      defaultGitHubTokenEnv,
		DefaultPlatformHost: defaultPlatformHost,
		Host:                defaultHost,
		Port:                defaultPort,
		Platforms: []PlatformConfig{{
			Type:          "gitea",
			Host:          "gitea.example.test:3000",
			BaseURL:       "http://gitea.example.test:3000/",
			AllowInsecure: true,
			TokenEnv:      "GITEA_PRIVATE_TOKEN",
		}},
	}

	require.NoError(cfg.Save(path))
	reloaded, err := Load(path)
	require.NoError(err)
	require.Len(reloaded.Platforms, 1)
	assert.Equal("http://gitea.example.test:3000", reloaded.Platforms[0].BaseURL)
	assert.True(reloaded.Platforms[0].AllowInsecure)
}

func TestLoadForgejoDefaultHostUsesDefaultTokenEnv(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "forgejo"
platform_host = "codeberg.org"
owner = "forgejo"
name = "forgejo"
`)
	t.Setenv("KENN_FORGE_FORGEJO_TOKEN", "codeberg-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "codeberg-secret", cfg.TokenForPlatformHost("forgejo", "codeberg.org", ""))
	assert.Empty(t, cfg.TokenForPlatformHost("forgejo", "forgejo.example.com", ""))
}

func TestLoadPlatformConfigGiteaLikeTokensAreHostScoped(t *testing.T) {
	tests := []struct {
		kind      string
		public    string
		private   string
		publicEnv string
		customEnv string
	}{
		{kind: "forgejo", public: "codeberg.org", private: "forgejo.example.com", publicEnv: "KENN_FORGE_FORGEJO_TOKEN", customEnv: "FORGEJO_EXAMPLE_TOKEN"},
		{kind: "gitea", public: "gitea.com", private: "gitea.internal.example", publicEnv: "KENN_FORGE_GITEA_TOKEN", customEnv: "GITEA_INTERNAL_TOKEN"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`
[[platforms]]
type = %q
host = %q
token_env = %q

[[platforms]]
type = %q
host = %q
token_env = %q

[[repos]]
platform = %q
platform_host = %q
owner = "public"
name = "service"

[[repos]]
platform = %q
platform_host = %q
owner = "private"
name = "service"
`, tt.kind, tt.public, tt.publicEnv, tt.kind, tt.private, tt.customEnv, tt.kind, tt.public, tt.kind, tt.private))
			t.Setenv(tt.publicEnv, "public-secret")
			t.Setenv(tt.customEnv, "private-secret")

			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, "public-secret", cfg.TokenForPlatformHost(tt.kind, tt.public, ""))
			assert.Equal(t, "private-secret", cfg.TokenForPlatformHost(tt.kind, tt.private, ""))
		})
	}
}

func TestLoadParsesForgejoCodebergURL(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
name = "https://codeberg.org/forgejo/forgejo.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("forgejo", cfg.Repos[0].Platform)
	assert.Equal("codeberg.org", cfg.Repos[0].PlatformHost)
	assert.Equal("forgejo", cfg.Repos[0].Owner)
	assert.Equal("forgejo", cfg.Repos[0].Name)
	assert.Equal("forgejo/forgejo", cfg.Repos[0].RepoPath)
}

func TestLoadGiteaDefaultHostUsesDefaultTokenEnv(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "gitea"
platform_host = "gitea.com"
owner = "gitea"
name = "tea"
`)
	t.Setenv("KENN_FORGE_GITEA_TOKEN", "gitea-public-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "gitea-public-secret", cfg.TokenForPlatformHost("gitea", "gitea.com", ""))
	assert.Empty(t, cfg.TokenForPlatformHost("gitea", "gitea.internal.example", ""))
}

func TestLoadParsesGiteaURL(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
name = "https://gitea.com/gitea/tea.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitea", cfg.Repos[0].Platform)
	assert.Equal("gitea.com", cfg.Repos[0].PlatformHost)
	assert.Equal("gitea", cfg.Repos[0].Owner)
	assert.Equal("tea", cfg.Repos[0].Name)
	assert.Equal("gitea/tea", cfg.Repos[0].RepoPath)
}

func TestLoadKeepsExistingGitHubURLInference(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
name = "https://github.com/wesm/kenn-forge.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("github", cfg.Repos[0].Platform)
	assert.Equal("github.com", cfg.Repos[0].PlatformHost)
	assert.Equal("wesm", cfg.Repos[0].Owner)
	assert.Equal("kenn-forge", cfg.Repos[0].Name)
	assert.Equal("wesm/kenn-forge", cfg.Repos[0].RepoPath)
}

func TestLoadKeepsExistingGitLabURLInference(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
name = "https://gitlab.com/gitlab-org/gitlab.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.com", cfg.Repos[0].PlatformHost)
	assert.Equal("gitlab-org", cfg.Repos[0].Owner)
	assert.Equal("gitlab", cfg.Repos[0].Name)
	assert.Equal("gitlab-org/gitlab", cfg.Repos[0].RepoPath)
}

func TestLoadRejectsDuplicatePlatformConfig(t *testing.T) {
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "https://gitlab.example.com/"
token_env = "GITLAB_TOKEN"

[[platforms]]
type = "gitlab"
host = "gitlab.example.com"
token_env = "GITLAB_TOKEN"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate platform "gitlab/gitlab.example.com"`)
}

func TestLoadRejectsConflictingPlatformTokenEnv(t *testing.T) {
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "gitlab.example.com"
token_env = "GITLAB_TOKEN_A"

[[platforms]]
type = "gitlab"
host = "https://gitlab.example.com/"
token_env = "GITLAB_TOKEN_B"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting token_env")
}

func TestLoadAllowsDifferentProviderFallbacksOnSharedHostAndDisablesOwnerlessFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, err := Load(writeConfig(t, `
[[platforms]]
type = "github"
host = "code.example.com"
token_env = "GITHUB_PAT"

[[platforms]]
type = "forgejo"
host = "code.example.com"
token_env = "FORGEJO_PAT"
`))
	require.NoError(err,
		"credentials are provider-scoped; providers sharing a hostname may differ")

	// Ownerless host operations cannot select a provider safely when the
	// providers' chains disagree, so the host clone fallback is disabled
	// instead of borrowing whichever provider came first.
	var hostChain *tokenauth.Descriptor
	for _, desc := range cfg.CloneTokenDescriptors() {
		if desc.Key == tokenauth.CloneKey("code.example.com") {
			d := desc
			hostChain = &d
		}
	}
	require.NotNil(hostChain)
	assert.Empty(hostChain.Candidates)
}

func TestLoadGitLabNestedNamespaceURL(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
owner = "ignored"
name = "https://gitlab.com/My-Group/SubGroup/My-Project.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.com", cfg.Repos[0].PlatformHost)
	assert.Equal("My-Group/SubGroup", cfg.Repos[0].Owner)
	assert.Equal("My-Project", cfg.Repos[0].Name)
}

func TestLoadGitLabMergeRequestURL(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
owner = "ignored"
name = "https://gitlab.com/group/project/-/merge_requests/1"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.com", cfg.Repos[0].PlatformHost)
	assert.Equal("group", cfg.Repos[0].Owner)
	assert.Equal("project", cfg.Repos[0].Name)
}

func TestLoadPreservesExplicitGitLabOwnerNameCase(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "My-Group/SubGroup"
name = "My-Project"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("My-Group/SubGroup", cfg.Repos[0].Owner)
	assert.Equal("My-Project", cfg.Repos[0].Name)
}

func TestLoadNormalizesSelfHostedGitLabPlatformHost(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "https://gitlab.example.com/"
token_env = "GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "https://gitlab.example.com/"
owner = "acme"
name = "widgets"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("gitlab.example.com", cfg.Platforms[0].Host)
	assert.Equal("gitlab.example.com", cfg.Repos[0].PlatformHost)
}

func TestLoadPreservesSelfHostedGitLabHostPort(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "https://gitlab.example.com:8443/"
token_env = "GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com:8443"
owner = "acme"
name = "widgets"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("gitlab.example.com:8443", cfg.Platforms[0].Host)
	assert.Equal("gitlab.example.com:8443", cfg.Repos[0].PlatformHost)
}

func TestLoadRejectsGitLabSubpathPlatformHost(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
platform_host = "https://example.com/gitlab"
owner = "acme"
name = "widgets"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_repo_ref")
}

func TestLoadRejectsUnsafePlatformHosts(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"url userinfo", "https://gitlab.com@attacker.example/"},
		{"bare userinfo", "gitlab.com@attacker.example"},
		{"malformed port", "gitlab.example.com:bad"},
		{"control character", "gitlab.example.com\nattacker.example"},
		{"whitespace", "gitlab.example.com attacker.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`
[[repos]]
platform = "gitlab"
platform_host = %q
owner = "acme"
name = "widgets"
`, tt.host))

			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid_repo_ref")
		})
	}
}

func TestLoadRejectsAmbiguousGitLabURL(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
owner = "ignored"
name = "https://gitlab.com/acme"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete GitLab reference")
}

func TestRepoPlatformHostOrDefault(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{"empty defaults to github.com", "", "github.com"},
		{"explicit host preserved", "github.example.com", "github.example.com"},
		{"github.com explicit", "github.com", "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Repo{
				Owner:        "a",
				Name:         "b",
				PlatformHost: tt.host,
			}
			assert.Equal(t, tt.want, r.PlatformHostOrDefault())
		})
	}
}

func TestRepoResolveToken(t *testing.T) {
	t.Run("token_env set and populated", func(t *testing.T) {
		t.Setenv("MY_GHE_TOKEN", "ghe-secret")
		r := Repo{Owner: "a", Name: "b", TokenEnv: "MY_GHE_TOKEN"}
		assert.Equal(t, "ghe-secret", r.ResolveToken("global-token"))
	})

	t.Run("token_env set but empty falls back to global", func(t *testing.T) {
		t.Setenv("MY_GHE_TOKEN", "")
		r := Repo{Owner: "a", Name: "b", TokenEnv: "MY_GHE_TOKEN"}
		assert.Equal(t, "global-token", r.ResolveToken("global-token"))
	})

	t.Run("token_env not set falls back to global", func(t *testing.T) {
		r := Repo{Owner: "a", Name: "b"}
		assert.Equal(t, "global-token", r.ResolveToken("global-token"))
	})
}

func TestConfigResolveRepoTokenUsesPlatformToken(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
github_token_env = "GH_TOKEN"

[[platforms]]
type = "gitlab"
host = "gitlab.com"
token_env = "GITLAB_TOKEN"

[[repos]]
owner = "acme"
name = "widgets"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "group"
name = "project"
`)
	t.Setenv("GH_TOKEN", "github-secret")
	t.Setenv("GITLAB_TOKEN", "gitlab-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 2)
	assert.Equal("github-secret", cfg.ResolveRepoToken(cfg.Repos[0]))
	assert.Equal("gitlab-secret", cfg.ResolveRepoToken(cfg.Repos[1]))
}

func TestConfigResolveRepoTokenPrefersRepoTokenEnv(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "gitlab.com"
token_env = "GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "group"
name = "project"
token_env = "REPO_GITLAB_TOKEN"
`)
	t.Setenv("GITLAB_TOKEN", "platform-secret")
	t.Setenv("REPO_GITLAB_TOKEN", "repo-secret")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("repo-secret", cfg.ResolveRepoToken(cfg.Repos[0]))
}

func TestValidateRejectsDuplicateOwnerName(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"

[[repos]]
owner = "apache"
name = "arrow"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate repo")
}

func TestValidateAllowsSameOwnerNameAcrossPlatformHosts(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"

[[repos]]
platform = "github"
platform_host = "github.example.com"
owner = "apache"
name = "arrow"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "apache"
name = "arrow"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 3)
}

func TestValidateRejectsDuplicateRepoWithinSamePlatformHost(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "Apache"
name = "Arrow"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "Apache"
name = "Arrow"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate repo "gitlab/gitlab.com/Apache/Arrow"`)
}

func TestValidateRejectsGitLabDuplicateRepoByCaseWithinSamePlatformHost(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "Apache"
name = "Arrow"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "apache"
name = "arrow"
`)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate repo "gitlab/gitlab.com/Apache/Arrow"`)
}

func TestLoadGitLabSSHURIWithPortDoesNotUseSSHPortAsPlatformHost(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
platform = "gitlab"
owner = "ignored"
name = "ssh://git@gitlab.example.com:2222/group/project.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.example.com", cfg.Repos[0].PlatformHost)
	assert.Equal("group", cfg.Repos[0].Owner)
	assert.Equal("project", cfg.Repos[0].Name)
}

func TestLoadGitLabSSHURIWithPortKeepsExplicitPlatformHost(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[platforms]]
type = "gitlab"
host = "gitlab.example.com:8443"
token_env = "GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com:8443"
owner = "ignored"
name = "ssh://git@gitlab.example.com:2222/group/project.git"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 1)
	assert.Equal("gitlab", cfg.Repos[0].Platform)
	assert.Equal("gitlab.example.com:8443", cfg.Repos[0].PlatformHost)
	assert.Equal("group", cfg.Repos[0].Owner)
	assert.Equal("project", cfg.Repos[0].Name)
}

func TestValidateAllowsGitHubTokenEnvPerRepo(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "org1"
name = "repo1"
platform_host = "github.example.com"
token_env = "GHE_TOKEN_A"

[[repos]]
owner = "org2"
name = "repo2"
platform_host = "github.example.com"
token_env = "GHE_TOKEN_B"
`)
	_, err := Load(path)
	require.NoError(t, err)
}

func TestValidateScopesTokenEnvConflictsByPlatformHost(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
platform = "github"
platform_host = "example.com"
owner = "org1"
name = "repo1"
token_env = "GITHUB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "example.com"
owner = "org2"
name = "repo2"
token_env = "GITLAB_TOKEN"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com"
owner = "org3"
name = "repo3"
token_env = "OTHER_GITLAB_TOKEN"
`)

	_, err := Load(path)
	require.NoError(t, err)
}

func TestSaveRoundTripPlatformHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
platform_host = "github.example.com"
token_env = "GHE_TOKEN"

[[repos]]
owner = "ibis-project"
name = "ibis"
`)
	cfg, err := Load(path)
	require.NoError(err)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(err)
	require.Len(cfg2.Repos, 2)
	assert.Equal("github.example.com", cfg2.Repos[0].PlatformHost)
	assert.Equal("GHE_TOKEN", cfg2.Repos[0].TokenEnv)
	assert.Empty(cfg2.Repos[1].PlatformHost)
	assert.Empty(cfg2.Repos[1].TokenEnv)
}

func TestSaveRoundTripHostCheckSettings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
allowed_hosts = ["proxy.local:8091", "forge.example"]
trust_reverse_proxy = true

[[repos]]
owner = "apache"
name = "arrow"
`)
	cfg, err := Load(path)
	require.NoError(err)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(err)
	assert.Equal([]string{"proxy.local:8091", "forge.example"}, cfg2.AllowedHosts)
	assert.True(cfg2.TrustReverseProxy)
	assert.Equal(
		[]HostKey{{Host: "proxy.local", Port: "8091"}, {Host: "forge.example", Port: ""}},
		cfg2.ParsedAllowedHosts(),
	)
}

func TestSaveWritesPrivateFileMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("file permission semantics differ on Windows")
	}
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
`)
	cfg, err := Load(path)
	require.NoError(err)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))

	info, err := os.Stat(savePath)
	require.NoError(err)
	assert.Equal(fs.FileMode(0o600), info.Mode().Perm())
}

func TestSaveCreatesParentDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
`)
	cfg, err := Load(path)
	require.NoError(err)

	savePath := filepath.Join(t.TempDir(), "nested", "deeper", "saved.toml")
	require.NoError(cfg.Save(savePath))

	info, err := os.Stat(filepath.Dir(savePath))
	require.NoError(err)
	require.True(info.IsDir())
	if runtime.GOOS != "windows" {
		assert.Equal(fs.FileMode(0o700), info.Mode().Perm())
	}
}

func TestSaveFollowsExistingSymlink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions require elevated privileges on Windows")
	}
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
`)
	cfg, err := Load(path)
	require.NoError(err)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "dotfiles", "config.toml")
	require.NoError(os.MkdirAll(filepath.Dir(targetPath), 0o700))
	require.NoError(os.WriteFile(targetPath, []byte("stale = true\n"), 0o600))
	linkPath := filepath.Join(dir, "config.toml")
	require.NoError(os.Symlink(targetPath, linkPath))

	require.NoError(cfg.Save(linkPath))

	info, err := os.Lstat(linkPath)
	require.NoError(err)
	assert.NotZero(info.Mode()&fs.ModeSymlink, "config path should remain a symlink")
	cfg2, err := Load(targetPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal("arrow", cfg2.Repos[0].Name)
}

func TestSaveRejectsInvalidConfigWithoutWriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "0.0.0.0",
		Port:           8091,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
	}
	savePath := filepath.Join(t.TempDir(), "config.toml")

	err := cfg.Save(savePath)

	require.Error(err)
	assert.Contains(err.Error(), "host")
	_, statErr := os.Stat(savePath)
	require.True(os.IsNotExist(statErr), "config should not be written on validation failure: %v", statErr)
}

func TestSaveDoesNotInheritStaleTmpPermissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("file permission semantics differ on Windows")
	}
	path := writeConfig(t, `
[[repos]]
owner = "apache"
name = "arrow"
`)
	cfg, err := Load(path)
	require.NoError(err)

	dir := t.TempDir()
	savePath := filepath.Join(dir, "saved.toml")
	stale := savePath + ".tmp"
	require.NoError(os.WriteFile(stale, []byte("stale"), 0o644))

	require.NoError(cfg.Save(savePath))

	info, err := os.Stat(savePath)
	require.NoError(err)
	assert.Equal(fs.FileMode(0o600), info.Mode().Perm())
}

func TestSaveRoundTripEmptyGitHubTokenEnv(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
github_token_env = ""

[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(cfg.GitHubTokenEnv)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(t, err)
	assert.Empty(cfg2.GitHubTokenEnv)
}

func TestRoborevConfigRoundTrip(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[roborev]
endpoint = "http://custom:9999"
init_managed_clones = true
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal("http://custom:9999", cfg.RoborevEndpoint())
	assert.True(cfg.Roborev.InitManagedClones)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(t, err)
	assert.Equal("http://custom:9999", cfg2.RoborevEndpoint())
	assert.True(cfg2.Roborev.InitManagedClones)
}

func TestPullRequestsConfigRoundTrip(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[pull_requests]
allow_mid_stack_merges = true
prefer_github_native_stacks = true
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(cfg.PullRequests.AllowMidStackMerges)
	assert.True(cfg.PullRequests.PreferGitHubNativeStacks)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(t, err)
	assert.True(cfg2.PullRequests.AllowMidStackMerges)
	assert.True(cfg2.PullRequests.PreferGitHubNativeStacks)
}

func TestDetailInitialTimelineEntryLimitDefaultsAndValidates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
	`)
	cfg, err := Load(path)
	require.NoError(err)
	assert.Equal(50, cfg.Detail.InitialTimelineEntryLimit)

	for _, limit := range []int{10, 250} {
		candidate := *cfg
		candidate.Detail.InitialTimelineEntryLimit = limit
		require.NoError(candidate.Validate())
	}
	for _, limit := range []int{9, 251} {
		candidate := *cfg
		candidate.Detail.InitialTimelineEntryLimit = limit
		err := candidate.Validate()
		require.Error(err)
		assert.Contains(err.Error(), "detail.initial_timeline_entry_limit must be between 10 and 250 or omitted")
	}
}

func TestDetailConfigRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[detail]
initial_timeline_entry_limit = 80
collapse_single_line_breaks = true
render_commit_messages_as_markdown = true
	`)
	cfg, err := Load(path)
	require.NoError(err)
	assert.Equal(80, cfg.Detail.InitialTimelineEntryLimit)
	assert.True(cfg.Detail.CollapseSingleLineBreaks)
	assert.True(cfg.Detail.RenderCommitMessagesAsMarkdown)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(err)
	assert.Equal(80, cfg2.Detail.InitialTimelineEntryLimit)
	assert.True(cfg2.Detail.CollapseSingleLineBreaks)
	assert.True(cfg2.Detail.RenderCommitMessagesAsMarkdown)
}

func TestRepositoryStableIdentitySurvivesConfigSave(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "acme"
name = "widget"
platform_repo_id = "repo-widget"
	`)
	cfg, err := Load(path)
	require.NoError(err)

	savedPath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savedPath))
	saved, err := os.ReadFile(savedPath)
	require.NoError(err)
	assert.Contains(string(saved), `platform_repo_id = "repo-widget"`)
}

func TestRepoPresetsConfigRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, saved := roundTripConfigString(t, `
[[repos]]
owner = "a"
name = "b"

[[repo_presets]]
name = "  Review queue  "
repos = [
  { provider = "github", platform_host = " github.com ", platform_repo_id = " R_widgets ", repo_path = " acme/widgets " },
  { provider = "gitlab", platform_host = "git.example.com", platform_repo_id = "42", repo_path = "group/project" },
  { provider = "github", platform_host = "github.com", platform_repo_id = "R_widgets", repo_path = "acme/old-widgets" },
]
`)

	want := []RepoPreset{{
		Name: "Review queue",
		Repos: []RepoPresetRepository{
			{Provider: "github", PlatformHost: "github.com", PlatformRepoID: "R_widgets", RepoPath: "acme/widgets"},
			{Provider: "gitlab", PlatformHost: "git.example.com", PlatformRepoID: "42", RepoPath: "group/project"},
		},
	}}
	assert.Equal(want, cfg.RepoPresets)
	assert.Equal(want, saved.RepoPresets)
	require.Len(saved.RepoPresets, 1)
}

func TestRepoPresetValidation(t *testing.T) {
	tests := []struct {
		name    string
		presets string
		wantErr string
	}{
		{
			name: "reserved global name",
			presets: `
[[repo_presets]]
name = "GLOBAL"
repos = [{ provider = "github", platform_host = "github.com", platform_repo_id = "R_widgets", repo_path = "acme/widgets" }]
`,
			wantErr: `repo_presets[0]: name "GLOBAL" is reserved`,
		},
		{
			name: "names must be unique ignoring case",
			presets: `
[[repo_presets]]
name = "Review Queue"
repos = [{ provider = "github", platform_host = "github.com", platform_repo_id = "R_widgets", repo_path = "acme/widgets" }]

[[repo_presets]]
name = "review queue"
repos = [{ provider = "github", platform_host = "github.com", platform_repo_id = "R_docs", repo_path = "acme/docs" }]
`,
			wantErr: `duplicate repo preset name "review queue"`,
		},
		{
			name: "name is required",
			presets: `
[[repo_presets]]
name = "   "
repos = [{ provider = "github", platform_host = "github.com", platform_repo_id = "R_widgets", repo_path = "acme/widgets" }]
`,
			wantErr: "repo_presets[0]: name is required",
		},
		{
			name: "repositories are required",
			presets: `
[[repo_presets]]
name = "Review queue"
repos = []
`,
			wantErr: "repo_presets[0]: at least one repository is required",
		},
		{
			name: "provider must be canonical",
			presets: `
[[repo_presets]]
name = "Review queue"
repos = [{ provider = "GitHub", platform_host = "github.com", platform_repo_id = "R_widgets", repo_path = "acme/widgets" }]
`,
			wantErr: `repo_presets[0].repos[0]: repository identity must use a canonical provider`,
		},
		{
			name: "provider must be supported",
			presets: `
[[repo_presets]]
name = "Review queue"
repos = [{ provider = "bitbucket", platform_host = "bitbucket.org", platform_repo_id = "R_widgets", repo_path = "acme/widgets" }]
`,
			wantErr: `repo_presets[0].repos[0]: unsupported provider "bitbucket"`,
		},
		{
			name: "stable repository id is required",
			presets: `
[[repo_presets]]
name = "Review queue"
repos = [{ provider = "github", platform_host = "github.com", repo_path = "acme/widgets" }]
`,
			wantErr: `repo_presets[0].repos[0]: platform_repo_id is required`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`+tt.presets))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestTerminalConfigRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[terminal]
font_family = '  "Iosevka Term", monospace  '
font_size = 16
scrollback = 5000
line_height = 1.2
letter_spacing = 1
cursor_blink = false
font_ligatures = true
graphics = false
tmux_mouse = false
retained_sessions = 0
`)
	cfg, err := Load(path)
	require.NoError(err)
	assert.Equal("\"Iosevka Term\", monospace", cfg.Terminal.FontFamily)
	assert.Equal(16, cfg.Terminal.FontSize)
	assert.Equal(5000, cfg.Terminal.Scrollback)
	assert.InDelta(1.2, cfg.Terminal.LineHeight, 0.001)
	assert.Equal(1, cfg.Terminal.LetterSpacing)
	require.NotNil(cfg.Terminal.CursorBlink)
	assert.False(*cfg.Terminal.CursorBlink)
	assert.True(cfg.Terminal.FontLigatures)
	require.NotNil(cfg.Terminal.Graphics)
	assert.False(*cfg.Terminal.Graphics)
	require.NotNil(cfg.Terminal.TmuxMouse)
	assert.False(*cfg.Terminal.TmuxMouse)
	require.NotNil(cfg.Terminal.RetainedSessions)
	assert.Equal(0, *cfg.Terminal.RetainedSessions)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(err)
	assert.Equal("\"Iosevka Term\", monospace", cfg2.Terminal.FontFamily)
	assert.Equal(16, cfg2.Terminal.FontSize)
	assert.Equal(5000, cfg2.Terminal.Scrollback)
	assert.InDelta(1.2, cfg2.Terminal.LineHeight, 0.001)
	assert.Equal(1, cfg2.Terminal.LetterSpacing)
	require.NotNil(cfg2.Terminal.CursorBlink)
	assert.False(*cfg2.Terminal.CursorBlink)
	assert.True(cfg2.Terminal.FontLigatures)
	require.NotNil(cfg2.Terminal.Graphics)
	assert.False(*cfg2.Terminal.Graphics)
	require.NotNil(cfg2.Terminal.TmuxMouse)
	assert.False(*cfg2.Terminal.TmuxMouse)
	require.NotNil(cfg2.Terminal.RetainedSessions)
	assert.Equal(0, *cfg2.Terminal.RetainedSessions)
}

func TestTerminalSettingsDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(err)

	assert.Equal(12, cfg.Terminal.FontSize)
	assert.Equal(1000, cfg.Terminal.Scrollback)
	assert.InDelta(1.0, cfg.Terminal.LineHeight, 0.001)
	assert.Equal(0, cfg.Terminal.LetterSpacing)
	require.NotNil(cfg.Terminal.CursorBlink)
	assert.True(*cfg.Terminal.CursorBlink)
	assert.False(cfg.Terminal.FontLigatures)
	require.NotNil(cfg.Terminal.Graphics)
	assert.True(*cfg.Terminal.Graphics)
	require.NotNil(cfg.Terminal.TmuxMouse)
	assert.True(*cfg.Terminal.TmuxMouse)
	require.NotNil(cfg.Terminal.RetainedSessions)
	assert.Equal(DefaultTerminalRetainedSessions, *cfg.Terminal.RetainedSessions)
}

func TestTerminalRetainedSessionsValidation(t *testing.T) {
	for _, retainedSessions := range []int{-1, 21} {
		t.Run(fmt.Sprintf("value_%d", retainedSessions), func(t *testing.T) {
			_, err := Load(writeConfig(t, fmt.Sprintf(`
[[repos]]
owner = "a"
name = "b"

[terminal]
retained_sessions = %d
`, retainedSessions)))

			require.ErrorContains(t, err, "terminal.retained_sessions")
		})
	}
}

func TestIssueWorkspaceBranchStyleDefaultsToSlug(t *testing.T) {
	path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, IssueWorkspaceBranchStyleSlug, cfg.IssueWorkspaceBranchStyle)
	assert.True(t, cfg.IssueWorkspaceBranchSlugEnabled())
}

func TestIssueWorkspaceBranchStyleAcceptsBare(t *testing.T) {
	path := writeConfig(t, `
issue_workspace_branch_style = "bare"

[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, IssueWorkspaceBranchStyleBare, cfg.IssueWorkspaceBranchStyle)
	assert.False(t, cfg.IssueWorkspaceBranchSlugEnabled())
}

func TestIssueWorkspaceBranchStyleRejectsInvalidValue(t *testing.T) {
	path := writeConfig(t, `
issue_workspace_branch_style = "fancy"

[[repos]]
owner = "a"
name = "b"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid issue_workspace_branch_style")
}

func TestIssueWorkspaceBranchStyleRoundTrip(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
issue_workspace_branch_style = "bare"

[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(IssueWorkspaceBranchStyleBare, cfg.IssueWorkspaceBranchStyle)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))

	cfg2, err := Load(savePath)
	require.NoError(t, err)
	assert.Equal(IssueWorkspaceBranchStyleBare, cfg2.IssueWorkspaceBranchStyle)
	assert.False(cfg2.IssueWorkspaceBranchSlugEnabled())
}

func TestIssueWorkspaceBranchStyleSlugIsOmittedFromSavedConfig(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
issue_workspace_branch_style = "slug"

[[repos]]
owner = "a"
name = "b"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(IssueWorkspaceBranchStyleSlug, cfg.IssueWorkspaceBranchStyle)

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(t, cfg.Save(savePath))

	data, err := os.ReadFile(savePath)
	require.NoError(t, err)
	// The default style should not be written back to disk; the
	// field is treated as opt-out only.
	assert.NotContains(string(data), "issue_workspace_branch_style")
}

func TestKataProjectMappingsRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, cfg2 := roundTripConfigString(t, `
[[repos]]
owner = "Acme"
name = "Widget"

[[kata_projects]]
daemon_id = "desktop"
project_uid = "project-kata"
provider = "github"
platform_host = "github.com"
repo_path = "Acme/Widget"

[[kata_projects]]
project_uid = "project-global"
provider = "github"
platform_host = "github.com"
repo_path = "Acme/Widget"
`)

	require.Len(cfg.KataProjects, 2)
	assert.Equal("desktop", cfg.KataProjects[0].DaemonID)
	assert.Equal("project-kata", cfg.KataProjects[0].ProjectUID)
	assert.Equal("github", cfg.KataProjects[0].Provider)
	assert.Equal("github.com", cfg.KataProjects[0].PlatformHost)
	assert.Equal("acme/widget", cfg.KataProjects[0].RepoPath)
	assert.Empty(cfg.KataProjects[1].DaemonID)

	require.Len(cfg2.KataProjects, 2)
	assert.Equal(cfg.KataProjects, cfg2.KataProjects)
}

func TestKataProjectMappingsRejectDuplicates(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[repos]]
owner = "acme"
name = "widget"

[[kata_projects]]
daemon_id = "desktop"
project_uid = "project-kata"
provider = "github"
platform_host = "github.com"
repo_path = "acme/widget"

[[kata_projects]]
daemon_id = "desktop"
project_uid = "project-kata"
provider = "github"
platform_host = "github.com"
repo_path = "acme/widget"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate kata project mapping")
}

func TestKataProjectMappingsAllowRegisteredProjectTargets(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[[repos]]
owner = "acme"
name = "widget"

[[kata_projects]]
project_uid = "project-kata"
provider = "github"
platform_host = "github.com"
repo_path = "acme/other"
`))
	require.NoError(t, err)
	require.Len(t, cfg.KataProjects, 1)
	assert.Equal(t, "acme/other", cfg.KataProjects[0].RepoPath)
}

func TestSyncBudgetPerHour(t *testing.T) {
	t.Run("default is 500 when not set", func(t *testing.T) {
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 500, cfg.BudgetPerHour())
	})

	t.Run("rejects value below 50", func(t *testing.T) {
		path := writeConfig(t, `
sync_budget_per_hour = 49
[[repos]]
owner = "a"
name = "b"
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sync_budget_per_hour must be >= 50 or omitted")
	})

	t.Run("configured value preserved", func(t *testing.T) {
		path := writeConfig(t, `
sync_budget_per_hour = 1000
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 1000, cfg.BudgetPerHour())
	})

	t.Run("round-trips through Save", func(t *testing.T) {
		require := require.New(t)
		path := writeConfig(t, `
sync_budget_per_hour = 750
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(err)

		savePath := filepath.Join(t.TempDir(), "saved.toml")
		require.NoError(cfg.Save(savePath))

		cfg2, err := Load(savePath)
		require.NoError(err)
		assert.Equal(t, 750, cfg2.BudgetPerHour())
	})
}

func TestActivityDefaultBranchBounds(t *testing.T) {
	t.Run("defaults retention and max commits when unset", func(t *testing.T) {
		assert := assert.New(t)
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(90, cfg.Activity.DefaultBranchRetentionDays)
		assert.Equal(5000, cfg.Activity.DefaultBranchMaxCommits)
		assert.Equal(90*24*time.Hour, cfg.BranchActivityRetention())
	})

	t.Run("configured values are preserved", func(t *testing.T) {
		assert := assert.New(t)
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
default_branch_retention_days = 14
default_branch_max_commits = 250
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(14, cfg.Activity.DefaultBranchRetentionDays)
		assert.Equal(250, cfg.Activity.DefaultBranchMaxCommits)
		assert.Equal(14*24*time.Hour, cfg.BranchActivityRetention())
	})

	t.Run("rejects negative values", func(t *testing.T) {
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
default_branch_retention_days = -1
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default_branch_retention_days")
	})

	t.Run("round-trips through Save", func(t *testing.T) {
		require := require.New(t)
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[activity]
default_branch_retention_days = 30
default_branch_max_commits = 1000
`)
		cfg, err := Load(path)
		require.NoError(err)

		savePath := filepath.Join(t.TempDir(), "saved.toml")
		require.NoError(cfg.Save(savePath))

		cfg2, err := Load(savePath)
		require.NoError(err)
		assert.Equal(t, 30, cfg2.Activity.DefaultBranchRetentionDays)
		assert.Equal(t, 1000, cfg2.Activity.DefaultBranchMaxCommits)
	})
}

func TestModeVisibilityDefaultsAndRoundTrip(t *testing.T) {
	t.Run("defaults imported modes hidden when unset", func(t *testing.T) {
		assert := assert.New(t)
		cfg, err := Load(writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`))
		require.NoError(t, err)

		assert.True(*cfg.Modes.Activity)
		assert.True(*cfg.Modes.Repos)
		assert.False(*cfg.Modes.Docs)
		assert.False(*cfg.Modes.Actions)
		assert.True(*cfg.Modes.Pulls)
		assert.True(*cfg.Modes.Issues)
		assert.True(*cfg.Modes.Reviews)
		assert.True(*cfg.Modes.Workspaces)
	})

	t.Run("preserves configured false values through save", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		cfg, err := Load(writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[modes]
actions = true
activity = false
repos = false
docs = false
pulls = false
issues = false
reviews = false
workspaces = false
`))
		require.NoError(err)

		savePath := filepath.Join(t.TempDir(), "saved.toml")
		require.NoError(cfg.Save(savePath))
		saved, err := os.ReadFile(savePath)
		require.NoError(err)
		assert.NotContains(string(saved), "board =")
		cfg2, err := Load(savePath)
		require.NoError(err)

		assert.True(*cfg2.Modes.Actions)
		assert.False(*cfg2.Modes.Activity)
		assert.False(*cfg2.Modes.Repos)
		assert.False(*cfg2.Modes.Docs)
		assert.False(*cfg2.Modes.Pulls)
		assert.False(*cfg2.Modes.Issues)
		assert.False(*cfg2.Modes.Reviews)
		assert.False(*cfg2.Modes.Workspaces)
	})
}

func TestKataModeRemoved(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	cfg, err := Load(writeConfig(t, `
[[repos]]
owner = "a"
name = "b"

[modes]
kata = true
docs = true
`))
	require.NoError(err)

	jsonModes, err := json.Marshal(cfg.Modes)
	require.NoError(err)
	assert.NotContains(t, string(jsonModes), "kata")

	savePath := filepath.Join(t.TempDir(), "saved.toml")
	require.NoError(cfg.Save(savePath))
	saved, err := os.ReadFile(savePath)
	require.NoError(err)
	assert.NotContains(t, string(saved), "kata =")
}

func TestSSEBufferSize(t *testing.T) {
	t.Run("default is 256 when unset", func(t *testing.T) {
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 256, cfg.SSEBufferSize)
		assert.Equal(t, 256, cfg.SSEBufferSizeOrDefault())
	})

	t.Run("nil receiver returns default", func(t *testing.T) {
		var cfg *Config
		assert.Equal(t, 256, cfg.SSEBufferSizeOrDefault())
	})

	t.Run("rejects below minimum", func(t *testing.T) {
		path := writeConfig(t, `
sse_buffer_size = 8
[[repos]]
owner = "a"
name = "b"
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sse_buffer_size must be between 16 and 16384")
	})

	t.Run("rejects above maximum", func(t *testing.T) {
		path := writeConfig(t, `
sse_buffer_size = 20000
[[repos]]
owner = "a"
name = "b"
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sse_buffer_size must be between 16 and 16384")
	})

	t.Run("accepts valid value in range", func(t *testing.T) {
		path := writeConfig(t, `
sse_buffer_size = 1024
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 1024, cfg.SSEBufferSize)
		assert.Equal(t, 1024, cfg.SSEBufferSizeOrDefault())
	})

	t.Run("accepts boundary minimum 16", func(t *testing.T) {
		path := writeConfig(t, `
sse_buffer_size = 16
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 16, cfg.SSEBufferSize)
	})

	t.Run("accepts boundary maximum 16384", func(t *testing.T) {
		path := writeConfig(t, `
sse_buffer_size = 16384
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 16384, cfg.SSEBufferSize)
	})

	t.Run("round-trips through Save", func(t *testing.T) {
		require := require.New(t)
		path := writeConfig(t, `
sse_buffer_size = 1024
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(err)

		savePath := filepath.Join(t.TempDir(), "saved.toml")
		require.NoError(cfg.Save(savePath))

		cfg2, err := Load(savePath)
		require.NoError(err)
		assert.Equal(t, 1024, cfg2.SSEBufferSize)
	})

	t.Run("default value is omitted from Save output", func(t *testing.T) {
		require := require.New(t)
		path := writeConfig(t, `
[[repos]]
owner = "a"
name = "b"
`)
		cfg, err := Load(path)
		require.NoError(err)

		savePath := filepath.Join(t.TempDir(), "saved.toml")
		require.NoError(cfg.Save(savePath))

		// Reload should still produce the default.
		cfg2, err := Load(savePath)
		require.NoError(err)
		assert.Equal(t, 256, cfg2.SSEBufferSize)
	})
}

func TestRoborevEndpointDefault(t *testing.T) {
	cfg := &Config{}
	assert.Equal(
		t, "http://127.0.0.1:7373", cfg.RoborevEndpoint(),
	)
}

func TestLoadTmuxCommand(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[tmux]
command = ["systemd-run", "--user", "--scope", "tmux"]
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "tmux"},
		cfg.Tmux.Command,
	)
}

func TestLoadTmuxCommandOmitted(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, ``)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(cfg.Tmux.Command)
	assert.Equal([]string{"tmux", "-L", "kenn-forge"}, cfg.TmuxCommand())
	assert.True(cfg.TmuxAgentSessionsEnabled())
}

func TestLoadTmuxCommandEmptyArray(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[tmux]
command = []
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal([]string{"tmux", "-L", "kenn-forge"}, cfg.TmuxCommand())
}

func TestLoadTmuxAgentSessionsDisabled(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[tmux]
agent_sessions = false
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(cfg.TmuxAgentSessionsEnabled())
}

func TestSavePreservesTmuxAgentSessionsDisabled(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	disabled := false

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
		Tmux: Tmux{
			AgentSessions: &disabled,
		},
	}
	require.NoError(t, cfg.Save(path))

	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.False(reloaded.TmuxAgentSessionsEnabled())
}

func TestTmuxCommandDefensiveCopy(t *testing.T) {
	assert := assert.New(t)
	cfg := &Config{Tmux: Tmux{
		Command: []string{"tmux"},
	}}
	first := cfg.TmuxCommand()
	first[0] = "hacked"
	second := cfg.TmuxCommand()
	assert.Equal([]string{"tmux"}, second)
}

func TestTmuxCommandNilReceiver(t *testing.T) {
	assert := assert.New(t)
	var cfg *Config
	assert.Equal([]string{"tmux", "-L", "kenn-forge"}, cfg.TmuxCommand())
}

func TestDefaultTmuxCommandDefensiveCopy(t *testing.T) {
	assert := assert.New(t)
	first := DefaultTmuxCommand()
	first[0] = "hacked"
	assert.Equal([]string{"tmux", "-L", "kenn-forge"}, DefaultTmuxCommand())
}

func TestIsDefaultTmuxCommand(t *testing.T) {
	assert.True(t, IsDefaultTmuxCommand(nil))
	assert.True(t, IsDefaultTmuxCommand([]string{"tmux", "-L", "kenn-forge"}))
	assert.False(t, IsDefaultTmuxCommand([]string{"tmux"}))
}

func TestLoadTmuxCommandRejectsEmptyFirstElement(t *testing.T) {
	path := writeConfig(t, `
[tmux]
command = ["", "extra"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		`config: invalid tmux.command`,
	)
}

// TestLoadTmuxCommandRejectsWhitespaceFirstElement covers the
// whitespace-only case: "   " would sneak past a plain == "" check
// and exec("   ") fails with a confusing shell-level error rather
// than the config-load validation message operators actually want.
func TestLoadTmuxCommandRejectsWhitespaceFirstElement(t *testing.T) {
	path := writeConfig(t, `
[tmux]
command = ["   ", "extra"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		`config: invalid tmux.command`,
	)
}

func TestLoadShellCommand(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[shell]
command = ["systemd-run", "--user", "--scope", "zsh"]
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "zsh"},
		cfg.Shell.Command,
	)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "zsh"},
		cfg.ShellCommand(),
	)
}

func TestLoadShellCommandOmitted(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, ``)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(cfg.Shell.Command)
	// Unset returns nil, signalling the runtime to fall back to $SHELL.
	assert.Nil(cfg.ShellCommand())
}

func TestLoadShellCommandEmptyArray(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[shell]
command = []
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Nil(cfg.ShellCommand())
}

func TestShellCommandDefensiveCopy(t *testing.T) {
	assert := assert.New(t)
	cfg := &Config{Shell: Shell{Command: []string{"zsh"}}}
	first := cfg.ShellCommand()
	first[0] = "hacked"
	second := cfg.ShellCommand()
	assert.Equal([]string{"zsh"}, second)
}

func TestShellCommandNilReceiver(t *testing.T) {
	assert := assert.New(t)
	var cfg *Config
	assert.Nil(cfg.ShellCommand())
}

func TestLoadShellCommandRejectsEmptyFirstElement(t *testing.T) {
	path := writeConfig(t, `
[shell]
command = ["", "zsh"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		`config: invalid shell.command`,
	)
}

// Whitespace-only first element sneaks past a plain == "" check and
// exec("   ") fails with a confusing shell-level error rather than
// the config-load validation message operators actually want.
func TestLoadShellCommandRejectsWhitespaceFirstElement(t *testing.T) {
	path := writeConfig(t, `
[shell]
command = ["   ", "zsh"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		`config: invalid shell.command`,
	)
}

func TestSavePreservesShellCommand(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
		Shell: Shell{
			Command: []string{"systemd-run", "--user", "zsh"},
		},
	}
	require.NoError(t, cfg.Save(path))

	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(
		[]string{"systemd-run", "--user", "zsh"},
		reloaded.Shell.Command,
	)
}

func TestSavePreservesTmuxCommand(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
		Tmux: Tmux{
			Command: []string{"systemd-run", "--user", "--scope", "tmux"},
		},
	}
	require.NoError(t, cfg.Save(path))

	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "tmux"},
		reloaded.Tmux.Command,
	)
}

// TestSaveRoundTripsFleet guards against Save silently dropping the
// spoke binding, peer timeout, or fleet session policy.
func TestSaveRoundTripsFleet(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
		API:            API{RequireAuth: true},
		Fleet: Fleet{
			Enabled: true,
			Role:    FleetRoleSpoke,
			BaseURL: "https://spoke.test:8443",
			Hub: &FleetHub{
				NodeID:  "0123456789abcdef0123456789abcdef",
				Name:    "Studio",
				BaseURL: "https://studio.test",
			},
			PeerTimeout: "3s",
			Sessions:    FleetSessions{IncludeUnmanagedDetails: true},
		},
	}
	require.NoError(t, cfg.Save(path))

	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.True(reloaded.Fleet.Enabled)
	assert.Equal(FleetRoleSpoke, reloaded.Fleet.RoleOrDefault())
	assert.Equal("https://spoke.test:8443", reloaded.Fleet.BaseURL)
	assert.Equal(cfg.Fleet.Hub, reloaded.Fleet.Hub)
	assert.Equal("3s", reloaded.Fleet.PeerTimeout)
	assert.True(reloaded.Fleet.Sessions.IncludeUnmanagedDetails)
}

// TestSaveOmitsEmptyFleet keeps default configs clean: a daemon with no
// fleet federation must not gain a [fleet] section on save.
func TestSaveOmitsEmptyFleet(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
	}
	require.NoError(t, cfg.Save(path))

	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(string(saved), "[fleet]")
}

func TestLoadAgents(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[agents]]
key = "codex"
label = "Codex"
command = ["codex", "--full-auto"]

[[agents]]
key = "claude"
label = "Claude"
command = ["claude"]
enabled = false
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 2)
	assert.Equal("codex", cfg.Agents[0].Key)
	assert.Equal("Codex", cfg.Agents[0].Label)
	assert.Equal(
		[]string{"codex", "--full-auto"},
		cfg.Agents[0].Command,
	)
	assert.True(cfg.Agents[0].EnabledOrDefault())
	assert.False(cfg.Agents[1].EnabledOrDefault())
}

func TestLoadAgentDefaultsLabelAndNormalizesKey(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, `
[[agents]]
key = "  Codex  "
command = ["codex"]
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 1)
	assert.Equal("codex", cfg.Agents[0].Key)
	assert.Equal("codex", cfg.Agents[0].Label)
}

func TestLoadAgentRejectsMissingKey(t *testing.T) {
	path := writeConfig(t, `
[[agents]]
label = "Codex"
command = ["codex"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "config: agents[0]: key")
}

func TestLoadAgentRejectsEnabledMissingCommand(t *testing.T) {
	path := writeConfig(t, `
[[agents]]
key = "codex"
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"config: agents[0]: command",
	)
}

func TestLoadAgentAllowsDisabledMissingCommand(t *testing.T) {
	path := writeConfig(t, `
[[agents]]
key = "codex"
enabled = false
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 1)
	assert.False(t, cfg.Agents[0].EnabledOrDefault())
}

func TestLoadAgentRejectsEmptyCommandFirstElement(t *testing.T) {
	path := writeConfig(t, `
[[agents]]
key = "codex"
command = ["   ", "extra"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"config: agents[0]: command first element must be non-empty",
	)
}

func TestLoadAgentRejectsDuplicateKeys(t *testing.T) {
	path := writeConfig(t, `
[[agents]]
key = "codex"
command = ["codex"]

[[agents]]
key = " CODEX "
command = ["codex-custom"]
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), `config: duplicate agent "codex"`)
}

func TestLoadAgentRejectsReservedSystemKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "tmux", key: "tmux"},
		{name: "plain shell", key: " plain_shell "},
		{name: "shell", key: " shell "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`
[[agents]]
key = %q
command = ["codex"]
`, tt.key))

			_, err := Load(path)

			require.Error(t, err)
			require.Contains(
				t, err.Error(),
				"reserved system launch target",
			)
		})
	}
}

func TestSavePreservesAgents(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	disabled := false

	cfg := &Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Host:           "127.0.0.1",
		Port:           8091,
		DataDir:        dir,
		Activity:       Activity{ViewMode: "threaded", TimeRange: "7d"},
		Agents: []Agent{{
			Key:     "codex",
			Label:   "Codex",
			Command: []string{"codex", "--full-auto"},
		}, {
			Key:     "claude",
			Label:   "Claude",
			Enabled: &disabled,
		}},
	}
	require.NoError(t, cfg.Save(path))

	reloaded, err := Load(path)
	require.NoError(t, err)
	require.Len(t, reloaded.Agents, 2)
	assert.Equal("codex", reloaded.Agents[0].Key)
	assert.Equal(
		[]string{"codex", "--full-auto"},
		reloaded.Agents[0].Command,
	)
	assert.False(reloaded.Agents[1].EnabledOrDefault())
}

func TestTokenEnvNamesIncludesGlobalPlatformAndPerRepo(t *testing.T) {
	var nilCfg *Config
	require.Nil(t, nilCfg.TokenEnvNames())

	cfg := &Config{
		GitHubTokenEnv: "WORK_GH_BOT_TOKEN",
		Platforms: []PlatformConfig{
			{Type: "forgejo", Host: "codeberg.org", TokenEnv: "KENN_FORGE_FORGEJO_TOKEN"},
			{Type: "forgejo", Host: "forgejo.example.com", TokenEnv: "FORGEJO_EXAMPLE_TOKEN"},
			{Type: "gitea", Host: "gitea.internal.example", TokenEnv: "GITEA_INTERNAL_TOKEN"},
		},
		Repos: []Repo{
			{Owner: "acme", Name: "widget", TokenEnv: "ACME_TOKEN"},
			{Owner: "other", Name: "thing"},
			{Owner: "third", Name: "x", TokenEnv: "THIRD_TOKEN"},
		},
	}
	assert.Equal(
		t,
		[]string{
			"WORK_GH_BOT_TOKEN",
			"KENN_FORGE_FORGEJO_TOKEN",
			"FORGEJO_EXAMPLE_TOKEN",
			"GITEA_INTERNAL_TOKEN",
			"ACME_TOKEN",
			"THIRD_TOKEN",
		},
		cfg.TokenEnvNames(),
	)
}

func TestTokenEnvNamesIncludesImplicitPublicForgeTokenEnvs(t *testing.T) {
	cfg := &Config{
		GitHubTokenEnv: "WORK_GH_BOT_TOKEN",
		Repos: []Repo{
			{
				Platform:     "forgejo",
				PlatformHost: "codeberg.org",
				Owner:        "forgejo",
				Name:         "forgejo",
			},
			{
				Platform:     "gitea",
				PlatformHost: "gitea.com",
				Owner:        "gitea",
				Name:         "tea",
			},
		},
	}

	assert.Equal(
		t,
		[]string{
			"WORK_GH_BOT_TOKEN",
			"KENN_FORGE_FORGEJO_TOKEN",
			"KENN_FORGE_GITEA_TOKEN",
		},
		cfg.TokenEnvNames(),
	)
}

func TestTokenEnvNamesIncludesImplicitPublicForgeTokenEnvsFromPlatformOnly(t *testing.T) {
	cfg := &Config{
		GitHubTokenEnv: "WORK_GH_BOT_TOKEN",
		Platforms: []PlatformConfig{
			{Type: "forgejo", Host: "codeberg.org"},
			{Type: "gitea", Host: "gitea.com"},
			{Type: "forgejo", Host: "forgejo.example.com"},
			{Type: "gitea", Host: "gitea.internal.example"},
		},
	}

	assert.Equal(
		t,
		[]string{
			"WORK_GH_BOT_TOKEN",
			"KENN_FORGE_FORGEJO_TOKEN",
			"KENN_FORGE_GITEA_TOKEN",
		},
		cfg.TokenEnvNames(),
	)
}

func TestTokenEnvNamesIncludesFallbackProviderDefaultsForRepoTokenEnv(t *testing.T) {
	cfg := &Config{
		GitHubTokenEnv: "WORK_GH_BOT_TOKEN",
		Repos: []Repo{
			{
				Platform:     "forgejo",
				PlatformHost: "codeberg.org",
				Owner:        "forgejo",
				Name:         "forgejo",
				TokenEnv:     "REPO_FORGEJO_TOKEN",
			},
			{
				Platform:     "gitea",
				PlatformHost: "gitea.com",
				Owner:        "gitea",
				Name:         "tea",
				TokenEnv:     "REPO_GITEA_TOKEN",
			},
		},
	}

	assert.Equal(
		t,
		[]string{
			"WORK_GH_BOT_TOKEN",
			"KENN_FORGE_FORGEJO_TOKEN",
			"KENN_FORGE_GITEA_TOKEN",
			"REPO_FORGEJO_TOKEN",
			"REPO_GITEA_TOKEN",
		},
		cfg.TokenEnvNames(),
	)
}

func TestGhAuthTokenForHostPassesHostnameFlag(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "gh-secret-github",
	})

	got := ghAuthTokenForHost("github.com")
	assert.Equal(t, "gh-secret-github", got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(t, argv, 1)
	assert.Equal(t, "auth token --hostname github.com", argv[0])
}

func TestGhAuthTokenForHostRetriesBareWhenOldGHRejectsHostnameFlag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	argvPath := setFakeGHCLIWithScript(
		t, fakeGHCLIRejectHostnameThenBareScript(),
	)

	got := ghAuthTokenForHost("github.com")
	assert.Equal("gh-secret-bare", got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(argv, 2)
	assert.Equal("auth token --hostname github.com", argv[0])
	assert.Equal("auth token", argv[1])
}

func TestGhAuthTokenForHostDoesNotRetryBareOnAuthFailure(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stderr:   "no oauth token",
		ExitCode: 1,
	})

	got := ghAuthTokenForHost("github.com")
	assert.Empty(t, got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(t, argv, 1, "should not retry bare on non-flag-rejection failure")
	assert.Equal(t, "auth token --hostname github.com", argv[0])
}

func TestGhAuthTokenForHostDoesNotRetryBareOnGHEFlagRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	argvPath := setFakeGHCLIWithScript(t, fakeGHCLIRejectHostnameScript())

	got := ghAuthTokenForHost("ghe.example.com")
	assert.Empty(got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(argv, 1, "non-github.com host must not retry bare")
	assert.Equal("auth token --hostname ghe.example.com", argv[0])
}

func TestGhAuthTokenForHostReturnsEmptyWhenBinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	assert.Empty(t, ghAuthTokenForHost("github.com"))
}

func TestGitHubCLITokenForHostTimesOutWithoutCallerDeadline(t *testing.T) {
	// Fake gh sleeps longer than the timeout, so the helper must
	// return "" once the context expires.
	setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout:       "never-reached",
		SleepSeconds: 10,
	})
	setGHAuthExecTimeout(t, time.Second)

	start := time.Now()
	got, err := GitHubCLITokenForHost(context.Background(), "github.com")
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Less(
		t, elapsed, ghAuthExecTimeout+2*time.Second,
		"helper should return shortly after timeout, took %s", elapsed,
	)
}

func TestTokenForPlatformHostUsesGHWithHostnameForGHE(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "ghe-secret",
	})
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")

	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}
	got := cfg.TokenForPlatformHost("github", "ghe.example.com", "")
	assert.Equal(t, "ghe-secret", got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(t, argv, 1)
	assert.Equal(t, "auth token --hostname ghe.example.com", argv[0])
}

func TestTokenForPlatformHostIgnoresGitHubTokenEnvForGHE(t *testing.T) {
	// github_token_env holds the public-GitHub token. A non-default
	// GitHub host must never receive it, even when the env var is set;
	// the host-scoped gh credential is the only implicit fallback.
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "ghe-from-gh",
	})
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "public-github-token")

	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}
	got := cfg.TokenForPlatformHost("github", "ghe.example.com", "")
	assert.Equal(t, "ghe-from-gh", got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(t, argv, 1)
	assert.Equal(t, "auth token --hostname ghe.example.com", argv[0])
}

func TestTokenForPlatformHostPrefersPlatformsEntryOverGHForGHE(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "ghe-from-gh",
	})
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")
	t.Setenv("PLATFORMS_GHE_TOKEN", "ghe-from-platforms")

	cfg := &Config{
		GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN",
		Platforms: []PlatformConfig{
			{Type: "github", Host: "ghe.example.com", TokenEnv: "PLATFORMS_GHE_TOKEN"},
		},
	}
	got := cfg.TokenForPlatformHost("github", "ghe.example.com", "")
	assert.Equal(t, "ghe-from-platforms", got)

	assert.Empty(t, readFakeGHArgv(t, argvPath), "[[platforms]] should short-circuit gh")
}

func TestTokenForPlatformHostPrefersRepoTokenEnvOverGHForGHE(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "ghe-from-gh",
	})
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")
	t.Setenv("REPO_GHE_TOKEN", "ghe-from-repo")

	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}
	got := cfg.TokenForPlatformHost("github", "ghe.example.com", "REPO_GHE_TOKEN")
	assert.Equal(t, "ghe-from-repo", got)

	assert.Empty(t, readFakeGHArgv(t, argvPath), "repo token_env should short-circuit gh")
}

func TestGitHubTokenInvokesGHWithGithubComHostname(t *testing.T) {
	argvPath := setFakeGHCLIScript(t, fakeGHCLIOptions{
		Stdout: "default-host-secret",
	})
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")

	cfg := &Config{GitHubTokenEnv: "KENN_FORGE_GITHUB_TOKEN"}
	got := cfg.GitHubToken()
	assert.Equal(t, "default-host-secret", got)

	argv := readFakeGHArgv(t, argvPath)
	require.Len(t, argv, 1)
	assert.Equal(t, "auth token --hostname github.com", argv[0])
}

func TestLoadAllowedHostsDefault(t *testing.T) {
	assert := assert.New(t)
	cfg, err := Load(writeConfig(t, `host = "127.0.0.1"
port = 8091
`))
	require.NoError(t, err)
	assert.Empty(cfg.AllowedHosts)
	assert.Empty(cfg.ParsedAllowedHosts())
	assert.False(cfg.TrustReverseProxy)
	assert.Equal(
		HostKey{Host: "127.0.0.1", Port: "8091"},
		cfg.BindHostKey(),
	)
}

func TestLoadRejectsZeroPort(t *testing.T) {
	_, err := Load(writeConfig(t, `host = "127.0.0.1"
port = 0
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid port 0")
}

func TestLoadAllowedHostsParsesAndCanonicalises(t *testing.T) {
	assert := assert.New(t)
	cfg, err := Load(writeConfig(t, `host = "127.0.0.1"
port = 8091
allowed_hosts = ["mm.local:8091", "MM.Example.Com", "[::1]:8443"]
trust_reverse_proxy = true
`))
	require.NoError(t, err)
	assert.Equal(
		[]HostKey{
			{Host: "mm.local", Port: "8091"},
			{Host: "mm.example.com", Port: ""},
			{Host: "[::1]", Port: "8443"},
		},
		cfg.ParsedAllowedHosts(),
	)
	assert.True(cfg.TrustReverseProxy)
}

func TestLoadAllowedHostsRejectsBadEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{name: "unbracketed ipv6", entry: "::1:8091"},
		{name: "port only", entry: ":8091"},
		{name: "empty", entry: ""},
		{name: "port out of range", entry: "mm.local:99999"},
		{name: "bracketed ipv4", entry: "[127.0.0.1]:8091"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`host = "127.0.0.1"
port = 8091
allowed_hosts = [%q]
`, tc.entry))
			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "allowed_hosts")
		})
	}
}

func TestParsedAllowedHostsDefensiveCopy(t *testing.T) {
	assert := assert.New(t)
	cfg, err := Load(writeConfig(t, `host = "127.0.0.1"
port = 8091
allowed_hosts = ["mm.local:8091"]
`))
	require.NoError(t, err)
	got := cfg.ParsedAllowedHosts()
	got[0] = HostKey{Host: "tampered", Port: "1"}
	again := cfg.ParsedAllowedHosts()
	assert.Equal(
		[]HostKey{{Host: "mm.local", Port: "8091"}},
		again,
	)
}

func TestFleetConfigParsesAndValidates(t *testing.T) {
	require := require.New(t)
	path := writeConfig(t, `
[fleet]
enabled = true
role = "hub"
base_url = "HTTPS://HUB.Example:443/"
peer_timeout = "2s"
[api]
require_auth = true
[fleet.sessions]
include_unmanaged_details = true
[[fleet.members]]
node_id = "fedcba9876543210fedcba9876543210"
name = "Build Box"
base_url = "https://spoke.example"
state = "active"
`)
	cfg, err := Load(path)
	require.NoError(err)
	require.True(cfg.Fleet.Enabled)
	require.Equal("https://hub.example", cfg.Fleet.BaseURL)
	require.Equal(FleetRoleHub, cfg.Fleet.RoleOrDefault())
	require.Len(cfg.Fleet.Members, 1)
	require.Equal("fedcba9876543210fedcba9876543210", cfg.Fleet.Members[0].NodeID)
	require.Equal("https://spoke.example", cfg.Fleet.Members[0].BaseURL)
	require.Equal("2s", cfg.Fleet.PeerTimeoutOrDefault().String())
	require.True(cfg.Fleet.Sessions.IncludeUnmanagedDetails)

	_, err = Load(writeConfig(t, `
base_path = "/forge/"
[fleet]
enabled = true
base_url = "https://hub.example"
[api]
require_auth = true
`))
	require.ErrorContains(err, `fleet.enabled requires base_path = "/"`)
}

func TestFleetRoleDefaultsAndSpokeRequirements(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, err := Load(writeConfig(t, ""))
	require.NoError(err)
	assert.Equal(FleetRoleHub, cfg.Fleet.RoleOrDefault())

	for _, role := range []FleetRole{FleetRoleHub, FleetRoleSpoke} {
		t.Run(string(role), func(t *testing.T) {
			hub := ""
			if role == FleetRoleSpoke {
				hub = `
[fleet.hub]
node_id = "0123456789abcdef0123456789abcdef"
base_url = "https://hub.test"
`
			}
			loaded, loadErr := Load(writeConfig(t, fmt.Sprintf(`
[fleet]
role = %q
%s
`, role, hub)))
			require.NoError(loadErr)
			assert.Equal(role, loaded.Fleet.RoleOrDefault())
		})
	}

	_, err = Load(writeConfig(t, "[fleet]\nrole = \"worker\"\n"))
	require.ErrorContains(err, "fleet.role")

	_, err = Load(writeConfig(t, "[fleet]\nrole = \"spoke\"\n"))
	require.ErrorContains(err, "fleet.hub")
}

func TestFleetHubAndMembersUseCanonicalHTTPSOrigins(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, err := Load(writeConfig(t, `
[fleet]
role = "hub"

[[fleet.members]]
node_id = "fedcba9876543210fedcba9876543210"
name = "Build Box"
base_url = "HTTPS://SPOKE.Example:443/"
state = "active"
`))
	require.NoError(err)
	require.Len(cfg.Fleet.Members, 1)
	assert.Equal("https://spoke.example", cfg.Fleet.Members[0].BaseURL)

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(cfg.Save(path))
	reloaded, err := Load(path)
	require.NoError(err)
	assert.Equal(cfg.Fleet.Members, reloaded.Fleet.Members)

	_, err = Load(writeConfig(t, `
[fleet]
role = "spoke"
[fleet.hub]
node_id = "0123456789abcdef0123456789abcdef"
base_url = "http://hub.example"
`))
	assert.ErrorContains(err, "HTTPS origin")
}

func TestFleetBaseURLIsRequiredAndCanonicalWhenEnabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, err := Load(writeConfig(t, `
[fleet]
enabled = true
[api]
require_auth = true
`))
	require.ErrorContains(err, "fleet.base_url")

	_, err = Load(writeConfig(t, `
[fleet]
enabled = true
base_url = "http://spoke.example"
[api]
require_auth = true
`))
	require.ErrorContains(err, "HTTPS origin")

	cfg, err := Load(writeConfig(t, `
[fleet]
enabled = true
base_url = "HTTPS://SPOKE.Example:443/"
[api]
require_auth = true
`))
	require.NoError(err)
	assert.Equal("https://spoke.example", cfg.Fleet.BaseURL)

	cfg, err = Load(writeConfig(t, `
[fleet]
enabled = true
base_url = "https://spoke.example:8443"
[api]
require_auth = true
`))
	require.NoError(err)
	assert.Equal("https://spoke.example:8443", cfg.Fleet.BaseURL)
}

func TestUnsupportedFleetConfigurationFailsClosed(t *testing.T) {
	_, err := Load(writeConfig(t, "[fleet]\nremoved_option = 'old'\n"))
	require.ErrorContains(t, err, "unsupported fleet configuration")
}

func TestFleetEnabledRequiresAPIAuth(t *testing.T) {
	_, err := Load(writeConfig(t, "[fleet]\nenabled = true\nbase_url = \"https://spoke.example\"\n"))
	require.ErrorContains(t, err, "api.require_auth")

	cfg, err := Load(writeConfig(t, `
[fleet]
enabled = true
base_url = "https://spoke.example"

[api]
require_auth = true
`))
	require.NoError(t, err)
	assert.True(t, cfg.Fleet.Enabled)
}

func TestFleetSessionsConfigDefaultsToRedactedUnmanagedDetails(t *testing.T) {
	path := writeConfig(t, "[fleet]\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.False(t, cfg.Fleet.Sessions.IncludeUnmanagedDetails)
}

func TestFleetMemberTimeoutDefaults(t *testing.T) {
	var f Fleet
	require.Equal(t, 2*time.Second, f.PeerTimeoutOrDefault(), "empty peer timeout must default to 2s")
}

func TestFleetPeerTimeoutMustBePositive(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			_, err := Load(writeConfig(t, fmt.Sprintf(
				"[fleet]\npeer_timeout = %q\n", timeout,
			)))
			require.ErrorContains(t, err, "fleet.peer_timeout must be positive")
		})
	}
}

func TestValidateHostBinding(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr string
	}{
		{name: "loopback v4", host: "127.0.0.1"},
		{name: "loopback v6", host: "::1"},
		{name: "specific non-loopback", host: "100.100.1.2"},
		{name: "specific non-loopback v6", host: "fd7a:115c:a1e0::1"},
		{
			name:    "wildcard v4",
			host:    "0.0.0.0",
			wantErr: "unspecified",
		},
		{
			name:    "wildcard v6",
			host:    "::",
			wantErr: "unspecified",
		},
		{
			name:    "hostname",
			host:    "studio.tailnet",
			wantErr: "invalid host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := writeConfig(t, "host = \""+tc.host+"\"\n")
			_, err := Load(path)
			if tc.wantErr == "" {
				require.NoError(err)
				return
			}
			require.Error(err)
			require.Contains(err.Error(), tc.wantErr)
		})
	}
}
