package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gh "github.com/google/go-github/v89/github"
	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/platform/gitealike"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/stacks"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func setupTestServerWithConfig(
	t *testing.T,
) (*Server, *db.DB, string) {
	return setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{})
}

func setupTestServerWithConfigContent(
	t *testing.T,
	cfgContent string,
	mock *mockGH,
) (*Server, *db.DB, string) {
	return setupTestServerWithConfigContentAndOptions(
		t, cfgContent, mock, ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
}

func setupTestServerWithConfigContentAndOptions(
	t *testing.T,
	cfgContent string,
	mock *mockGH,
	options ServerOptions,
) (*Server, *db.DB, string) {
	t.Helper()

	dir := t.TempDir()
	database := dbtest.Open(t)
	cfgPath := filepath.Join(dir, "config.toml")
	err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644)
	require.NoError(t, err)

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	clients := map[string]ghclient.Client{"github.com": mock}
	resolved := ghclient.ResolveConfiguredRepos(
		t.Context(), clients, cfg.Repos,
	)
	syncer := ghclient.NewSyncer(
		clients, database, nil, resolved.Expanded,
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		options,
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	return srv, database, cfgPath
}

func installSettingsTmuxRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "commands")
	tmuxPath := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + shellquote.Join(record) + "\n" +
		`case " $* " in *" list-sessions "*) printf 'sess-A:\n';; *" list-panes "*) printf 'pane-A\npane-B\n';; esac` + "\n"
	require.NoError(t, os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func readSettingsTmuxMouseCommands(t *testing.T, record string) []string {
	t.Helper()
	content, err := os.ReadFile(record)
	require.NoError(t, err)
	commands := make([]string, 0, 2)
	for command := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		if strings.Contains(command, " list-sessions -F #{session_name}:#{@forge_owner}") ||
			strings.Contains(command, " set-option -q -g mouse ") {
			commands = append(commands, command)
		}
	}
	return commands
}

func readSettingsTmuxGraphicsCommands(t *testing.T, record string) []string {
	t.Helper()
	content, err := os.ReadFile(record)
	require.NoError(t, err)
	commands := make([]string, 0, 3)
	for command := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		if strings.Contains(command, "allow-passthrough") ||
			strings.Contains(command, " terminal-features[100]") {
			commands = append(commands, command)
		}
	}
	return commands
}

func TestServerStartupAppliesTmuxSettingsToExistingDedicatedServer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	record := installSettingsTmuxRecorder(t)
	srv, _, _ := setupTestServerWithConfigContentAndOptions(t, `
[terminal]
graphics = true
tmux_mouse = false
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		WorktreeDir:                   t.TempDir(),
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	commands, err := os.ReadFile(record)
	require.NoError(err)
	text := string(commands)
	assert.Contains(text, "-L kenn-forge set-option -q -g allow-passthrough on")
	assert.Contains(text, "-L kenn-forge set-option -q -s terminal-features[100] xterm-256color:sixel")
	assert.Contains(text, "-L kenn-forge set-option -q -g mouse off")
}

func setupTestServerWithConfigProviders(
	t *testing.T,
	cfgContent string,
	mock *mockGH,
	providers ...platform.Provider,
) (*Server, *db.DB, string) {
	t.Helper()

	dir := t.TempDir()
	database := dbtest.Open(t)
	cfgPath := filepath.Join(dir, "config.toml")
	err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644)
	require.NoError(t, err)

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	clients := map[string]ghclient.Client{"github.com": mock}
	registry, err := ghclient.NewProviderRegistry(clients, providers...)
	require.NoError(t, err)
	resolved := ghclient.ResolveConfiguredReposWithRegistry(
		t.Context(), registry, cfg.Repos,
	)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, resolved.Expanded, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	return srv, database, cfgPath
}

type repoImportTestProvider struct {
	kind  platform.Kind
	host  string
	repos []platform.Repository
}

func (p repoImportTestProvider) Platform() platform.Kind { return p.kind }

func (p repoImportTestProvider) Host() string { return p.host }

func (p repoImportTestProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadRepositories: true}
}

func (p repoImportTestProvider) GetRepository(
	_ context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	for _, repo := range p.repos {
		repoPath := strings.TrimSpace(repo.Ref.RepoPath)
		if repoPath == "" {
			repoPath = repo.Ref.Owner + "/" + repo.Ref.Name
		}
		refPath := strings.TrimSpace(ref.RepoPath)
		if refPath == "" {
			refPath = ref.Owner + "/" + ref.Name
		}
		if strings.EqualFold(repoPath, refPath) ||
			(strings.EqualFold(repo.Ref.Owner, ref.Owner) &&
				strings.EqualFold(repo.Ref.Name, ref.Name)) {
			return repo, nil
		}
	}
	return platform.Repository{}, errors.New("not found")
}

func (p repoImportTestProvider) ListRepositories(
	_ context.Context,
	owner string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos := make([]platform.Repository, 0, len(p.repos))
	for _, repo := range p.repos {
		if strings.EqualFold(repo.Ref.Owner, owner) {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

type gitealikeImportTransport struct {
	userRepos     []gitealike.RepositoryDTO
	userReposErr  error
	orgRepos      []gitealike.RepositoryDTO
	orgReposErr   error
	repository    gitealike.RepositoryDTO
	repositoryErr error
}

func (t *gitealikeImportTransport) GetRepository(
	context.Context,
	string,
	string,
) (gitealike.RepositoryDTO, error) {
	return t.repository, t.repositoryErr
}

func (t *gitealikeImportTransport) ListUserRepositories(
	context.Context,
	string,
	gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	return t.userRepos, gitealike.Page{}, t.userReposErr
}

func (t *gitealikeImportTransport) ListOrgRepositories(
	context.Context,
	string,
	gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	return t.orgRepos, gitealike.Page{}, t.orgReposErr
}

func (t *gitealikeImportTransport) ListOpenPullRequests(
	context.Context,
	platform.RepoRef,
	gitealike.PageOptions,
) ([]gitealike.PullRequestDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListOpenPullRequests call")
}

func (t *gitealikeImportTransport) GetPullRequest(
	context.Context,
	platform.RepoRef,
	int,
) (gitealike.PullRequestDTO, error) {
	return gitealike.PullRequestDTO{}, errors.New("unexpected GetPullRequest call")
}

func (t *gitealikeImportTransport) ListPullRequestComments(
	context.Context,
	platform.RepoRef,
	int,
	gitealike.PageOptions,
) ([]gitealike.CommentDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListPullRequestComments call")
}

func (t *gitealikeImportTransport) ListPullRequestReviews(
	context.Context,
	platform.RepoRef,
	int,
	gitealike.PageOptions,
) ([]gitealike.ReviewDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListPullRequestReviews call")
}

func (t *gitealikeImportTransport) ListPullRequestCommits(
	context.Context,
	platform.RepoRef,
	int,
	gitealike.PageOptions,
) ([]gitealike.CommitDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListPullRequestCommits call")
}

func (t *gitealikeImportTransport) ListOpenIssues(
	context.Context,
	platform.RepoRef,
	gitealike.PageOptions,
) ([]gitealike.IssueDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListOpenIssues call")
}

func (t *gitealikeImportTransport) GetIssue(
	context.Context,
	platform.RepoRef,
	int,
) (gitealike.IssueDTO, error) {
	return gitealike.IssueDTO{}, errors.New("unexpected GetIssue call")
}

func (t *gitealikeImportTransport) ListIssueComments(
	context.Context,
	platform.RepoRef,
	int,
	gitealike.PageOptions,
) ([]gitealike.CommentDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListIssueComments call")
}

func (t *gitealikeImportTransport) ListReleases(
	context.Context,
	platform.RepoRef,
	gitealike.PageOptions,
) ([]gitealike.ReleaseDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListReleases call")
}

func (t *gitealikeImportTransport) ListTags(
	context.Context,
	platform.RepoRef,
	gitealike.PageOptions,
) ([]gitealike.TagDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListTags call")
}

func (t *gitealikeImportTransport) ListStatuses(
	context.Context,
	platform.RepoRef,
	string,
	gitealike.PageOptions,
) ([]gitealike.StatusDTO, gitealike.Page, error) {
	return nil, gitealike.Page{}, errors.New("unexpected ListStatuses call")
}

func TestHandleGetSettings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[agents]]
key = "codex"
label = "Codex"
command = ["codex", "--full-auto"]
`, &mockGH{})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(rr.Body.String(), `"default_agent"`)
	assert.Contains(rr.Body.String(), `"launch_targets"`)

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("acme", resp.Repos[0].Owner)
	assert.Equal("repo-acme-widget", resp.Repos[0].PlatformRepoID)
	assert.Equal(1, resp.Repos[0].MatchedRepoCount)
	assert.True(resp.Repos[0].IssuePRReferences)
	assert.Equal("threaded", resp.Activity.ViewMode)
	assert.True(resp.Notifications.Enabled)
	assert.Empty(resp.Terminal.FontFamily)
	assert.Equal(config.DefaultTerminalFontSize, resp.Terminal.FontSize)
	assert.Equal(config.DefaultTerminalScrollback, resp.Terminal.Scrollback)
	assert.InDelta(
		config.DefaultTerminalLineHeight,
		resp.Terminal.LineHeight,
		0.001,
	)
	require.NotNil(resp.Terminal.CursorBlink)
	assert.True(*resp.Terminal.CursorBlink)
	assert.False(resp.Terminal.FontLigatures)
	assert.False(resp.Terminal.HideTmuxStatus)
	require.NotNil(resp.Terminal.Graphics)
	assert.True(*resp.Terminal.Graphics)
	assertDefaultModeVisibility(t, resp.Modes)
	require.Len(resp.Agents, 1)
	assert.Equal("codex", resp.Agents[0].Key)
	assert.Equal([]string{"codex", "--full-auto"}, resp.Agents[0].Command)
}

func TestHandleGetSettingsReportsMCPDesiredAndActiveState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, _ := setupTestServerWithConfigContentAndOptions(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[mcp]
enabled = true
port = 9092
diff_cache_mb = 256
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		MCPURL:                        "http://127.0.0.1:9092/mcp",
	})
	srv.bootCfgSnapshot.RequireAuth = true

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(resp.MCP.Enabled)
	assert.Equal(9092, resp.MCP.Port)
	assert.Equal(256, resp.MCP.DiffCacheMB)
	assert.False(resp.MCP.RestartRequired)
	assert.Equal("http://127.0.0.1:9092/mcp", resp.MCP.ActiveURL)
	assert.True(resp.MCP.ActiveRequiresAuth)
}

func TestHandleUpdateSettingsPersistsMCPAndReportsRestartRequired(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	mcp := config.MCP{Enabled: true, Port: 9092, DiffCacheMB: 256}
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{MCP: &mcpSettingsUpdate{
		Enabled: new(true), Port: new(9092), DiffCacheMB: new(256),
	}})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(config.MCP{
		Enabled:     resp.MCP.Enabled,
		Port:        resp.MCP.Port,
		DiffCacheMB: resp.MCP.DiffCacheMB,
	}, mcp)
	assert.True(resp.MCP.RestartRequired)
	assert.Empty(resp.MCP.ActiveURL)

	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(mcp, reloaded.MCP)
}

func TestHandleUpdateSettingsMergesMCPFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	srv.cfg.MCP = config.MCP{Port: 9092, DiffCacheMB: 256}
	require.NoError(srv.cfg.Save(cfgPath))

	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", map[string]any{
		"mcp": map[string]any{"enabled": true},
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(config.MCP{Enabled: true, Port: 9092, DiffCacheMB: 256}, srv.cfg.MCP)

	rr = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", map[string]any{
		"mcp": map[string]any{"port": 0},
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(config.MCP{Enabled: true, DiffCacheMB: 256}, srv.cfg.MCP)

	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(config.MCP{Enabled: true, DiffCacheMB: 256}, reloaded.MCP)
}

func TestHandleUpdateSettingsPersistsRoborevManagedCloneInit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Roborev: &roborevSettingsUpdate{InitManagedClones: new(true)},
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(resp.Roborev.InitManagedClones)
	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(reloaded.Roborev.InitManagedClones)
}

func TestHandleUpdateSettingsRejectsInvalidMCPWithoutPublishing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{MCP: &mcpSettingsUpdate{
		Enabled: new(true), Port: new(8091),
	}})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Equal(config.MCP{}, srv.cfg.MCP)

	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(config.MCP{}, reloaded.MCP)
}

func TestHandleGetSettingsEncodesEmptyKataProjectsAsArray(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// No [[kata_projects]] configured, so cfg.KataProjects is nil.
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	// kata_projects is a required non-null array in the schema. Assert on the
	// raw wire value because decoding into a Go slice would hide a null/[]
	// difference.
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &raw))
	require.Contains(raw, "kata_projects")
	assert.JSONEq("[]", string(raw["kata_projects"]))
}

func TestHandleGetSettingsEncodesEmptyRepoPresetsAsArray(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, _ := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &raw))
	require.Contains(raw, "repo_presets")
	assert.JSONEq("[]", string(raw["repo_presets"]))
}

func TestRepoPresetMutationsAreAtomic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	first := config.RepoPreset{Name: "Review queue", Repos: []config.RepoPresetRepository{{
		Provider: "github", PlatformHost: "github.com", PlatformRepoID: "R_widgets", RepoPath: "acme/widgets",
	}}}
	second := config.RepoPreset{Name: "Docs", Repos: []config.RepoPresetRepository{{
		Provider: "gitlab", PlatformHost: "git.example.com", PlatformRepoID: "42", RepoPath: "group/docs",
	}}}

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/settings/repo-presets", first)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())
	rr = testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/settings/repo-presets", second)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	updatedRepos := []config.RepoPresetRepository{{
		Provider: "github", PlatformHost: "github.com", PlatformRepoID: "R_tools", RepoPath: "acme/tools",
	}}
	rr = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings/repo-presets/Review%20queue", struct {
		Repos []config.RepoPresetRepository `json:"repos"`
	}{Repos: updatedRepos})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	rr = testutil.DoJSON(t, srv, http.MethodDelete, "/api/v1/settings/repo-presets/Review%20queue", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal([]config.RepoPreset{second}, resp.RepoPresets)

	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal([]config.RepoPreset{second}, reloaded.RepoPresets)

	rr = testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/settings/repo-presets", second)
	assert.Equal(http.StatusConflict, rr.Code)
}

func TestCreateRepoPresetRejectsSaveFailureWithoutPublishing(t *testing.T) {
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[repo_presets]]
name = "Existing"
repos = [{ provider = "github", platform_host = "github.com", platform_repo_id = "R_widget", repo_path = "acme/widget" }]
`, &mockGH{})

	srv.configReloadMu.Lock()
	srv.cfgPath = t.TempDir()
	srv.configReloadMu.Unlock()
	replacement := config.RepoPreset{Name: "Replacement", Repos: []config.RepoPresetRepository{{
		Provider: "github", PlatformHost: "github.com", PlatformRepoID: "R_other", RepoPath: "acme/other",
	}}}
	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/settings/repo-presets", replacement)
	require.Equal(http.StatusInternalServerError, rr.Code, rr.Body.String())
	require.Equal([]config.RepoPreset{{Name: "Existing", Repos: []config.RepoPresetRepository{{
		Provider: "github", PlatformHost: "github.com", PlatformRepoID: "R_widget", RepoPath: "acme/widget",
	}}}}, srv.cfg.RepoPresets)
}

func TestHandleUpdateSettingsPersistsModes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	modes := config.DefaultModeVisibility()
	*modes.Docs = true
	*modes.Workspaces = false
	*modes.Actions = true

	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{Modes: &modes})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(*resp.Modes.Actions)
	assert.True(*resp.Modes.Docs)
	assert.False(*resp.Modes.Workspaces)
	assert.True(*resp.Modes.Activity)
	assert.True(*resp.Modes.Repos)
	assert.True(*resp.Modes.Pulls)
	assert.True(*resp.Modes.Issues)
	assert.True(*resp.Modes.Reviews)

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(*cfg2.Modes.Actions)
	assert.True(*cfg2.Modes.Docs)
	assert.False(*cfg2.Modes.Workspaces)
	assert.True(*cfg2.Modes.Activity)
	assert.True(*cfg2.Modes.Repos)
	assert.True(*cfg2.Modes.Pulls)
	assert.True(*cfg2.Modes.Issues)
	assert.True(*cfg2.Modes.Reviews)

	activity := srv.cfg.Activity
	activity.TimeRange = "30d"
	rr = doJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Activity: &activity,
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	cfg3, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(*cfg3.Modes.Actions)
}

func TestHandleUpdateSettingsPublishesPullConfigOnlyAfterPersistence(t *testing.T) {
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	require.False(srv.pullAPI.ConfigSnapshot().AllowMidStackMerges)
	require.False(srv.pullAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)
	require.False(srv.issueAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)

	enabled := config.PullRequests{AllowMidStackMerges: true}
	activityEnabled := srv.cfg.Activity
	activityEnabled.UseWorkspaceActivityForRecency = true
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		PullRequests: &enabled,
		Activity:     &activityEnabled,
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	require.True(srv.pullAPI.ConfigSnapshot().AllowMidStackMerges)
	require.True(srv.pullAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)
	require.True(srv.issueAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)

	// Swapped under the reload lock: the config watcher goroutine reads
	// cfgPath under configReloadMu.
	srv.configReloadMu.Lock()
	srv.cfgPath = t.TempDir()
	srv.configReloadMu.Unlock()
	disabled := config.PullRequests{AllowMidStackMerges: false}
	activityDisabled := activityEnabled
	activityDisabled.UseWorkspaceActivityForRecency = false
	rr = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		PullRequests: &disabled,
		Activity:     &activityDisabled,
	})

	require.Equal(http.StatusInternalServerError, rr.Code, rr.Body.String())
	require.True(
		srv.pullAPI.ConfigSnapshot().AllowMidStackMerges,
		"failed persistence published an uncommitted Pull config",
	)
	require.True(srv.pullAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)
	require.True(srv.issueAPI.ConfigSnapshot().UseWorkspaceActivityForRecency)
}

func TestHandleUpdateSettingsSerializesWithConfigReload(t *testing.T) {
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)

	srv.configReloadMu.Lock()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := srv.updateSettings(context.Background(), &updateSettingsInput{
			Body: updateSettingsRequest{
				Activity: &config.Activity{TimeRange: "30d", ViewMode: "threaded"},
			},
		})
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		srv.configReloadMu.Unlock()
		require.NoError(err)
		require.Fail("settings update completed while config reload lock was held")
	case <-time.After(100 * time.Millisecond):
	}

	srv.configReloadMu.Unlock()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(5 * time.Second):
		require.Fail("settings update did not complete after config reload lock was released")
	}
}

func TestHandleUpdateSettingsPersistsKataProjectMappings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	mappings := []config.KataProjectRepoMapping{
		{
			DaemonID:     "desktop",
			ProjectUID:   "project-kata",
			Provider:     "github",
			PlatformHost: "github.com",
			RepoPath:     "acme/widget",
		},
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{KataProjects: &mappings})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.KataProjects, 1)
	assert.Equal(mappings[0], resp.KataProjects[0])

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.KataProjects, 1)
	assert.Equal(mappings[0], cfg2.KataProjects[0])
}

func assertDefaultModeVisibility(t *testing.T, modes config.ModeVisibility) {
	t.Helper()
	assert := assert.New(t)
	assert.True(*modes.Activity)
	assert.True(*modes.Repos)
	assert.False(*modes.Docs)
	assert.False(*modes.Actions)
	assert.True(*modes.Pulls)
	assert.True(*modes.Issues)
	assert.True(*modes.Reviews)
	assert.True(*modes.Workspaces)
}

func TestHandleUpdateSettings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	activity := config.Activity{
		ViewMode:   "threaded",
		TimeRange:  "30d",
		HideClosed: true,
		HideBots:   true,
	}
	issues := config.Issues{HideBots: true}
	autoAssign := true
	defaultSidebarView := "item"
	workspaces := workspaceSettingsUpdate{
		AutoAssignOnCreate: &autoAssign,
		DefaultSidebarView: &defaultSidebarView,
	}
	terminal := config.Terminal{
		FontFamily:       "\"Fira Code\", monospace",
		FontSize:         16,
		Scrollback:       5000,
		LineHeight:       1.15,
		CursorBlink:      new(true),
		FontLigatures:    true,
		HideTmuxStatus:   true,
		Graphics:         new(true),
		TmuxMouse:        new(false),
		RetainedSessions: new(4),
	}
	body := updateSettingsRequest{
		Activity:   &activity,
		Issues:     &issues,
		Workspaces: &workspaces,
		Terminal:   &terminal,
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings", body)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	// Verify persisted to disk.
	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal("threaded", cfg2.Activity.ViewMode)
	assert.Equal("30d", cfg2.Activity.TimeRange)
	assert.True(cfg2.Issues.HideBots)
	assert.True(cfg2.Workspaces.AutoAssignOnCreate)
	assert.Equal("item", cfg2.Workspaces.DefaultSidebarView)
	assert.Equal("\"Fira Code\", monospace", cfg2.Terminal.FontFamily)
	assert.Equal(16, cfg2.Terminal.FontSize)
	assert.Equal(5000, cfg2.Terminal.Scrollback)
	assert.InDelta(1.15, cfg2.Terminal.LineHeight, 0.001)
	assert.True(cfg2.Terminal.FontLigatures)
	assert.True(cfg2.Terminal.HideTmuxStatus)
	require.NotNil(cfg2.Terminal.TmuxMouse)
	assert.False(*cfg2.Terminal.TmuxMouse)
	require.NotNil(cfg2.Terminal.Graphics)
	assert.True(*cfg2.Terminal.Graphics)
	require.NotNil(cfg2.Terminal.RetainedSessions)
	assert.Equal(4, *cfg2.Terminal.RetainedSessions)
}

func TestHandleUpdateSettingsMergesWorkspaceFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	srv.cfg.Workspaces.AutoAssignOnCreate = true
	require.NoError(srv.cfg.Save(cfgPath))

	defaultSidebarView := "item"
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Workspaces: &workspaceSettingsUpdate{DefaultSidebarView: &defaultSidebarView},
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(cfg2.Workspaces.AutoAssignOnCreate)
	assert.Equal("item", cfg2.Workspaces.DefaultSidebarView)
}

func TestHandleUpdateSettingsDisablesNativeStackProjectionImmediately(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[pull_requests]
prefer_github_native_stacks = true
`, &mockGH{})
	ctx := t.Context()
	seedStackedPR(t, database, "acme", "widget", 10, "feat/base", "main", db.MergeRequestStateOpen, "", "")
	seedStackedPR(t, database, "acme", "widget", 11, "feat/tip", "feat/base", db.MergeRequestStateOpen, "", "")
	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC()
	require.NoError(database.ReplaceGitHubNativeStack(ctx, db.GitHubNativeStack{
		RepoID: repo.ID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "native", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 11, State: "open", HeadRef: "feat/tip", HeadSHA: "sha11"},
			{Position: 2, PullRequestNumber: 10, State: "open", HeadRef: "feat/base", HeadSHA: "sha10"},
		},
	}))
	require.NoError(stacks.RunDetectionWithNativeStacks(ctx, database, repo.ID, []int{42}))
	client := setupTestClientWithBaseURL(t, srv, "http://127.0.0.1:8091")

	before, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(before.JSON200)
	require.NotNil(before.JSON200.Members)
	assert.Equal([]int64{11, 10}, stackMemberNumbers(before.JSON200.Members))

	disabled := config.PullRequests{}
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		PullRequests: &disabled,
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	after, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(after.JSON200)
	require.NotNil(after.JSON200.Members)
	assert.Equal([]int64{10, 11}, stackMemberNumbers(after.JSON200.Members))
}

func TestHandleUpdateTerminalSettingsPreservesActivity(t *testing.T) {
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[activity]
view_mode = "flat"
time_range = "30d"
hide_closed = true
hide_bots = true

[modes]
docs = false
`, &mockGH{})

	terminal := config.Terminal{
		FontFamily:       "\"Iosevka Term\", monospace",
		FontSize:         15,
		Scrollback:       2000,
		LetterSpacing:    1,
		CursorBlink:      new(true),
		Graphics:         new(true),
		TmuxMouse:        new(true),
		RetainedSessions: new(config.DefaultTerminalRetainedSessions),
	}
	body := updateSettingsRequest{
		Terminal: &terminal,
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings", body)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(t, err)
	assert.Equal("flat", cfg2.Activity.ViewMode)
	assert.Equal("30d", cfg2.Activity.TimeRange)
	assert.True(cfg2.Activity.HideClosed)
	assert.True(cfg2.Activity.HideBots)
	assert.Equal("\"Iosevka Term\", monospace", cfg2.Terminal.FontFamily)
	assert.Equal(15, cfg2.Terminal.FontSize)
	assert.Equal(2000, cfg2.Terminal.Scrollback)
	assert.Equal(1, cfg2.Terminal.LetterSpacing)
	assert.False(*cfg2.Modes.Docs)
}

func TestHandleUpdateTerminalSettingsAppliesMouseToDedicatedTmuxServer(t *testing.T) {
	require := require.New(t)
	record := installSettingsTmuxRecorder(t)
	srv, _, _ := setupTestServerWithConfigContentAndOptions(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		WorktreeDir:                   t.TempDir(),
	})
	require.NoError(os.WriteFile(record, nil, 0o600))

	srv.cfgMu.Lock()
	terminal := srv.cfg.Terminal
	srv.cfgMu.Unlock()
	terminal.TmuxMouse = new(false)
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Terminal: &terminal,
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{
		"-L kenn-forge list-sessions -F #{session_name}:#{@forge_owner}",
		"-L kenn-forge set-option -q -g mouse off",
	}, readSettingsTmuxMouseCommands(t, record))
}

func TestHandleUpdateTerminalSettingsAppliesGraphicsToDedicatedTmuxServer(t *testing.T) {
	require := require.New(t)
	record := installSettingsTmuxRecorder(t)
	srv, _, _ := setupTestServerWithConfigContentAndOptions(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		WorktreeDir:                   t.TempDir(),
	})
	require.NoError(os.WriteFile(record, nil, 0o600))

	srv.cfgMu.Lock()
	terminal := srv.cfg.Terminal
	srv.cfgMu.Unlock()
	terminal.Graphics = new(false)
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Terminal: &terminal,
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{
		"-L kenn-forge set-option -q -g allow-passthrough off",
		"-L kenn-forge set-option -q -s -u terminal-features[100]",
		"-L kenn-forge set-option -q -p -u -t pane-A allow-passthrough",
		"-L kenn-forge set-option -q -p -u -t pane-B allow-passthrough",
	}, readSettingsTmuxGraphicsCommands(t, record))
}

func TestHandleUpdateSettingsPersistsAgents(t *testing.T) {
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	disabled := false
	agents := []config.Agent{{
		Key:     "codex",
		Label:   "Codex with flags",
		Command: []string{"/opt/codex", "--full-auto", "--search"},
	}, {
		Key:     "notes",
		Label:   "Notes",
		Command: []string{"/usr/local/bin/notes-agent", "--draft"},
	}, {
		Key:     "claude",
		Label:   "Claude",
		Enabled: &disabled,
	}}

	body := updateSettingsRequest{Agents: &agents}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings", body)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(t, err)
	require.Len(t, cfg2.Agents, 3)
	assert.Equal("codex", cfg2.Agents[0].Key)
	assert.Equal(
		[]string{"/opt/codex", "--full-auto", "--search"},
		cfg2.Agents[0].Command,
	)
	assert.Equal("notes", cfg2.Agents[1].Key)
	assert.False(cfg2.Agents[2].EnabledOrDefault())
}

func TestHandleUpdateSettingsRefreshesRuntimeTargets(t *testing.T) {
	dir := t.TempDir()
	agentPath := filepath.Join(dir, "codex-custom")
	require.NoError(t, os.WriteFile(
		agentPath,
		[]byte("#!/bin/sh\nexit 0\n"),
		0o755,
	))
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{})
	srv.runtime = localruntime.NewManager(localruntime.Options{
		Targets: []localruntime.LaunchTarget{{
			Key: "codex", Label: "Codex", Kind: localruntime.LaunchTargetAgent,
			Source: "builtin", Command: []string{"codex"},
			Available: false, DisabledReason: "codex not found on PATH",
		}},
	})
	t.Cleanup(srv.runtime.Shutdown)

	agents := []config.Agent{{
		Key:     "codex",
		Label:   "Custom Codex",
		Command: []string{agentPath, "--full-auto"},
	}}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{Agents: &agents})

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	target := findRuntimeTargetForSettingsTest(
		t, srv.runtime.LaunchTargets(), "codex",
	)
	assert := assert.New(t)
	assert.Equal("Custom Codex", target.Label)
	assert.Equal([]string{agentPath, "--full-auto"}, target.Command)
	assert.True(target.Available)
}

func findRuntimeTargetForSettingsTest(
	t *testing.T,
	targets []localruntime.LaunchTarget,
	key string,
) localruntime.LaunchTarget {
	t.Helper()
	for _, target := range targets {
		if target.Key == key {
			return target
		}
	}
	require.Failf(t, "target not found", "key %q", key)
	return localruntime.LaunchTarget{}
}

func TestHandleUpdateSettingsInvalid(t *testing.T) {
	srv, _, cfgPath := setupTestServerWithConfig(t)

	activity := config.Activity{
		ViewMode:  "kanban",
		TimeRange: "7d",
	}
	body := updateSettingsRequest{
		Activity: &activity,
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings", body)

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

	// Verify config was NOT modified (rollback).
	cfg2, err := config.Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "threaded", cfg2.Activity.ViewMode)
}

func TestHandleAddRepo(t *testing.T) {
	srv, _, cfgPath := setupTestServerWithConfig(t)

	body := map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "other-org",
		"name":     "other-repo",
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos", body)

	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(t, err)
	require.Len(t, cfg2.Repos, 2)
}

func TestHandleAddRepoAcceptsArchivedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(true),
			}, nil
		},
	}
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "other-org",
		"name":     "frozen",
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 2)
	assert.True(srv.syncer.IsTrackedRepo("other-org", "frozen"),
		"archived repo is tracked archive-only after add")
}

func TestHandleAddRepoRefreshesArchivedStateForTrackedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	archivedNow := atomic.Bool{}
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(archivedNow.Load()),
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:     new("widget"),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(archivedNow.Load()),
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "*"
`, mock)
	require.True(srv.syncer.IsTrackedRepo("acme", "widget"))

	// The repo gets archived on the provider; adding an overlapping exact
	// entry must refresh the tracked ref, not keep the stale live one.
	archivedNow.Store(true)
	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "acme",
		"name":     "widget",
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	assert.True(trackedRepoArchived(srv, "acme", "widget"),
		"overlapping add must apply fresh archived state")
}

func TestHandleRefreshRepoUpdatesArchivedStateForOverlappingEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	archivedNow := atomic.Bool{}
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(archivedNow.Load()),
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:     new("widget"),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(archivedNow.Load()),
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[repos]]
owner = "acme"
name = "*"
`, mock)
	require.True(srv.syncer.IsTrackedRepo("acme", "widget"))
	require.False(trackedRepoArchived(srv, "acme", "widget"))

	// widget matches both the exact entry and the glob; a refresh after the
	// provider archives it must update the tracked ref even though the
	// exact entry keeps it in the tracked set.
	archivedNow.Store(true)
	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/acme/*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	assert.True(trackedRepoArchived(srv, "acme", "widget"),
		"glob refresh must apply fresh archived state to overlapping repos")
	assert.Equal("acme/widget", trackedRepoProvenancePath(srv, "acme", "widget"),
		"glob refresh through the API must keep the exact entry's provenance")
}

func trackedRepoProvenancePath(srv *Server, owner, name string) string {
	for _, repo := range srv.syncer.TrackedRepos() {
		if strings.EqualFold(repo.Owner, owner) && strings.EqualFold(repo.Name, name) {
			return repo.ConfiguredRepoPath
		}
	}
	return ""
}

func TestHandleRefreshRepoStopsLiveLanesForArchivedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	archivedNow := atomic.Bool{}
	var listedRepos sync.Map
	var detailRepos sync.Map
	detailErr := errors.New("detail fetch short-circuited")
	mock := &mockGH{
		getPullRequestIfChangedFn: func(
			_ context.Context, _, repo string, _ int, _ string,
		) (*gh.PullRequest, string, bool, error) {
			detailRepos.Store(repo, true)
			return nil, "", false, detailErr
		},
		getPullRequestFn: func(
			_ context.Context, _, repo string, _ int,
		) (*gh.PullRequest, error) {
			detailRepos.Store(repo, true)
			return nil, detailErr
		},
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				NodeID:   new("repo-acme-" + repo),
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(repo == "widget" && archivedNow.Load()),
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					NodeID:   new("repo-acme-widget"),
					Name:     new("widget"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(archivedNow.Load()),
				},
				{
					NodeID:   new("repo-acme-tools"),
					Name:     new("tools"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
		listNotificationsFn: func(
			_ context.Context, opts ghclient.NotificationListOptions,
		) ([]ghclient.NotificationThread, bool, error) {
			if opts.RepoName != "" {
				listedRepos.Store(opts.RepoName, true)
			}
			return nil, false, nil
		},
	}
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[repos]]
owner = "acme"
name = "*"
`, mock)
	recentActivity := time.Now().UTC().Add(-10 * time.Minute)
	for i, name := range []string{"widget", "tools"} {
		repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-acme-" + name, Owner: "acme", Name: name,
		})
		require.NoError(err)
		_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(i + 1), Number: i + 1,
			Title: "PR", Author: "octo", State: db.MergeRequestStateOpen,
			HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: recentActivity.Add(-24 * time.Hour),
			UpdatedAt: recentActivity, LastActivityAt: recentActivity,
		})
		require.NoError(err)
	}
	srv.syncer.SetActiveMRWindow(4 * time.Hour)
	require.True(srv.syncer.IsTrackedRepo("acme", "widget"))
	// Stop background sync loops so the refresh-triggered async full sync
	// cannot populate the lane recorders; each lane runs synchronously below.
	srv.syncer.Stop()

	// The provider archives widget; the refresh applies the transition and
	// the live lanes must stop touching it while the live sibling keeps
	// syncing.
	archivedNow.Store(true)
	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/acme/*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	require.True(trackedRepoArchived(srv, "acme", "widget"))

	require.NoError(srv.syncer.SyncNotifications(t.Context()))
	_, listedTools := listedRepos.Load("tools")
	assert.True(listedTools, "live repo notifications should sync")
	_, listedWidget := listedRepos.Load("widget")
	assert.False(listedWidget,
		"archived repo must not receive notification polling after refresh")

	// Clear anything recorded by earlier phases so the assertions below
	// reflect the watched-MR lane exclusively.
	detailRepos.Range(func(key, _ any) bool {
		detailRepos.Delete(key)
		return true
	})
	srv.syncer.SyncWatchedMRs(t.Context())
	_, detailTools := detailRepos.Load("tools")
	assert.True(detailTools, "live repo open MR should fast-sync")
	_, detailWidget := detailRepos.Load("widget")
	assert.False(detailWidget,
		"archived repo open MRs must not enter fast sync after refresh")
}

func TestMergeTrackedReposReconcilesRenamedRouteByProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "old-name",
		PlatformHost: "github.com", RepoPath: "acme/old-name",
		PlatformExternalID: "repo-x",
	}})

	// The same stable provider id resolves under a renamed route: the
	// tracked set must reconcile to one entry, not sync both routes.
	srv.mergeTrackedRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "new-name",
		PlatformHost: "github.com", RepoPath: "acme/new-name",
		PlatformExternalID: "repo-x", Archived: true,
	}})

	tracked := srv.syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Equal("new-name", tracked[0].Name)
	assert.True(tracked[0].Archived)
}

func TestMergeTrackedReposPreservesExactEntryProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	// A settings-resolved duplicate (glob refresh, API add) carries no
	// config-entry provenance; replacing the tracked ref must not erase
	// the correlation the exact entry needs on the next failed reload.
	srv.mergeTrackedRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", Archived: true,
	}})

	tracked := srv.syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
	assert.Equal("acme/tools", tracked[0].ConfiguredRepoPath)
}

func TestMergeTrackedReposDoesNotTransferProvenanceAcrossProviderIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools",
		PlatformHost: "github.com", RepoPath: "acme/tools",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	// The tracked repo was renamed away and its old route reused by a
	// different repository. The renamed repo keeps its provenance through
	// stable identity; the route successor must not inherit it — two refs
	// claiming the same config entry would make a later fallback pick
	// whichever it sees first.
	srv.mergeTrackedRepos([]ghclient.RepoRef{
		{
			Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
			PlatformHost: "github.com", RepoPath: "acme/tools-new",
			PlatformExternalID: "repo-x",
		},
		{
			Platform: platform.KindGitHub, Owner: "acme", Name: "tools",
			PlatformHost: "github.com", RepoPath: "acme/tools",
			PlatformExternalID: "repo-y",
		},
	})

	tracked := srv.syncer.TrackedRepos()
	require.Len(tracked, 2)
	byName := make(map[string]ghclient.RepoRef, len(tracked))
	for _, repo := range tracked {
		byName[repo.Name] = repo
	}
	assert.Equal("acme/tools", byName["tools-new"].ConfiguredRepoPath,
		"stable identity carries provenance through the rename")
	assert.Empty(byName["tools"].ConfiguredRepoPath,
		"a different repository reusing the route must not inherit provenance")
}

func TestMergeTrackedReposTreatsCaseDifferingProviderIdsAsDistinct(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools",
		PlatformHost: "github.com", RepoPath: "acme/tools",
		PlatformExternalID: "repo-X", ConfiguredRepoPath: "acme/tools",
	}})

	// Provider ids are opaque, case-sensitive identities (identity keys
	// compare them exactly); a case-only difference is a different
	// repository and must not inherit route provenance.
	srv.mergeTrackedRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools",
		PlatformHost: "github.com", RepoPath: "acme/tools",
		PlatformExternalID: "repo-x",
	}})

	tracked := srv.syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.Empty(tracked[0].ConfiguredRepoPath)
}

func TestReplaceGlobReposPreservesExactEntryProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	glob := config.Repo{Owner: "acme", Name: "*"}
	srv.replaceGlobRepos(glob, []ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", Archived: true,
	}}, []config.Repo{{Owner: "acme", Name: "tools"}, glob})

	tracked := srv.syncer.TrackedRepos()
	require.Len(tracked, 1)
	assert.True(tracked[0].Archived)
	assert.Equal("acme/tools", tracked[0].ConfiguredRepoPath)
}

func trackedRepoArchived(srv *Server, owner, name string) bool {
	for _, repo := range srv.syncer.TrackedRepos() {
		if strings.EqualFold(repo.Owner, owner) && strings.EqualFold(repo.Name, name) {
			return repo.Archived
		}
	}
	return false
}

func TestHandleAddRepoTriggersImmediateSyncDuringCooldown(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	database := dbtest.Open(t)

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`), 0o644))

	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	mock := &mockGH{}
	trackers := map[string]*ghclient.RateTracker{
		"github.com": ghclient.NewRateTracker(
			database, "github.com", "host", "rest",
		),
	}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database,
		nil,
		[]ghclient.RepoRef{{
			Owner:        "acme",
			Name:         "widget",
			PlatformHost: "github.com",
		}},
		time.Minute,
		trackers,
		nil,
	)
	t.Cleanup(syncer.Stop)

	srv := NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{},
	)

	// Prime nextSyncAfter so the add-repo trigger exercises the same
	// cooldown path as a user clicking Sync right after a recent sync.
	syncer.RunOnce(t.Context())

	rr := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos",
		map[string]string{
			"provider": "github",
			"host":     "github.com",
			"owner":    "other-org",
			"name":     "other-repo",
		})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	require.Eventually(func() bool {
		repos, err := database.ListRepos(t.Context())
		if err != nil {
			return false
		}
		if len(repos) != 2 {
			return false
		}
		for _, repo := range repos {
			if repo.Owner == "other-org" &&
				repo.Name == "other-repo" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
}

func TestHandleAddRepoDuplicate(t *testing.T) {
	srv, _, _ := setupTestServerWithConfig(t)

	body := map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "acme",
		"name":     "widget",
	}
	rr := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos", body)

	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestHandleDeleteRepo(t *testing.T) {
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	// Add a second repo first so we can delete one.
	addBody := map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "other-org",
		"name":     "other-repo",
	}
	addRR := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos", addBody)

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())

	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/widget", nil)

	require.Equal(http.StatusNoContent, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal(t, "other-org", cfg2.Repos[0].Owner)
}

func TestHandleDeleteRepoPreservesKataProjectMappings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	addBody := map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "other-org",
		"name":     "other-repo",
	}
	addRR := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos", addBody)

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())

	mappings := []config.KataProjectRepoMapping{
		{
			DaemonID:     "desktop",
			ProjectUID:   "project-widget",
			Provider:     "github",
			PlatformHost: "github.com",
			RepoPath:     "acme/widget",
		},
		{
			DaemonID:     "desktop",
			ProjectUID:   "project-other",
			Provider:     "github",
			PlatformHost: "github.com",
			RepoPath:     "other-org/other-repo",
		},
	}
	updateRR := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{KataProjects: &mappings})

	require.Equal(http.StatusOK, updateRR.Code, updateRR.Body.String())

	deleteRR := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/widget", nil)

	require.Equal(http.StatusNoContent, deleteRR.Code, deleteRR.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal("other-org", cfg2.Repos[0].Owner)
	require.Len(cfg2.KataProjects, 2)
	assert.Equal("project-widget", cfg2.KataProjects[0].ProjectUID)
	assert.Equal("acme/widget", cfg2.KataProjects[0].RepoPath)
}

func TestGetSettingsWithoutPersistence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	database := dbtest.Open(t)

	cfg := &config.Config{
		SyncInterval:   "5m",
		GitHubTokenEnv: "UNUSED",
		Host:           "127.0.0.1",
		Port:           8091,
		BasePath:       "/",
		DataDir:        dir,
		Repos: []config.Repo{
			{Owner: "acme", Name: "widget"},
		},
		Activity: config.Activity{
			ViewMode:  "flat",
			TimeRange: "30d",
		},
	}
	mock := &mockGH{}
	syncer := ghclient.NewSyncer(map[string]ghclient.Client{"github.com": mock}, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", cfg, ServerOptions{})

	// GET /settings should work (read-only).
	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("acme", resp.Repos[0].Owner)
	assert.Equal("flat", resp.Activity.ViewMode)

	// Mutations should be rejected (no cfgPath).
	mutRR := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{Activity: &cfg.Activity})

	assert.Equal(http.StatusNotFound, mutRR.Code)

	addRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos",
		map[string]string{
			"provider": "github",
			"host":     "github.com",
			"owner":    "x",
			"name":     "y",
		})

	assert.Equal(http.StatusNotFound, addRR.Code)

	delRR := testutil.DoJSON(t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/widget", nil)

	assert.Equal(http.StatusNotFound, delRR.Code)
}

func TestDetailSettingsReadPersistAndRejectInvalidLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var initial settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&initial))
	assert.Equal(config.DefaultInitialTimelineEntryLimit, initial.Detail.InitialTimelineEntryLimit)

	updated := config.Detail{InitialTimelineEntryLimit: 80}
	rr = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{Detail: &updated})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var saved settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&saved))
	assert.Equal(80, saved.Detail.InitialTimelineEntryLimit)
	persisted, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(80, persisted.Detail.InitialTimelineEntryLimit)

	invalid := config.Detail{InitialTimelineEntryLimit: 9}
	rr = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{Detail: &invalid})
	require.Equal(http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	persisted, err = config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(80, persisted.Detail.InitialTimelineEntryLimit)
}

func TestHandleDeleteLastRepo(t *testing.T) {
	srv, _, cfgPath := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/widget", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg2.Repos)
}

func TestHandleGetSettingsIncludesGlobCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("kenn-forge"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("globber"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("archived"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(true),
				},
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("roborev-dev", resp.Repos[0].Owner)
	assert.Equal("*", resp.Repos[0].Name)
	assert.True(resp.Repos[0].IsGlob)
	assert.Equal(3, resp.Repos[0].MatchedRepoCount,
		"archived repos count as archive-only glob matches")
}

func TestHandleRefreshRepoRebuildsExpandedSyncSet(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("kenn-forge"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("globber"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("archived"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(true),
				},
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)

	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/roborev-dev/*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Equal(3, resp.Repos[0].MatchedRepoCount)
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "globber"))
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "archived"),
		"archived repos stay tracked as archive-only")
}

func TestHandleRefreshRepoPersistsExpandedReposBeforeAsyncSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	includeRefreshRepo := atomic.Bool{}
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			repos := []*gh.Repository{
				{
					ID:       new(int64(101)),
					NodeID:   new("repo-101"),
					Name:     new("kenn-forge"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					ID:       new(int64(102)),
					NodeID:   new("repo-102"),
					Name:     new("archived"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(true),
				},
			}
			if includeRefreshRepo.Load() {
				repos = append(repos, &gh.Repository{
					ID:       new(int64(103)),
					NodeID:   new("repo-103"),
					Name:     new("review-bot"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				})
			}
			return repos, nil
		},
	}
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)
	srv.syncer.Stop()
	includeRefreshRepo.Store(true)

	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/roborev-dev/*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	repos, err := database.ListRepos(t.Context())
	require.NoError(err)
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.Owner == "roborev-dev" {
			names = append(names, repo.Name)
		}
	}
	assert.ElementsMatch([]string{"kenn-forge", "archived", "review-bot"}, names)
}

func TestHandleRefreshRepoKeepsReposMatchedByOtherConfigEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("kenn-forge"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"

[[repos]]
owner = "roborev-dev"
name = "worker"
`, mock)

	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/roborev-dev/*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 2)
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "worker"))
}

func TestHandleDeleteRepoRebuildsExpandedSetFromRemainingPatterns(t *testing.T) {
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("kenn-forge"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"

[[repos]]
owner = "roborev-dev"
name = "tools"
`, mock)

	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/roborev-dev/*", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	assert.True(t, srv.syncer.IsTrackedRepo("roborev-dev", "tools"))
	assert.False(t, srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))
}

func TestHandleDeleteGlobKeepsRenamedExactEntryRepo(t *testing.T) {
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:  new(repo),
				Owner: &gh.User{Login: new(owner)},
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:  new("tools"),
				Owner: &gh.User{Login: new(owner)},
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "tools"

[[repos]]
owner = "acme"
name = "*"
`, mock)

	// The exact entry's repo was renamed provider-side; only provenance
	// still ties the tracked ref to the entry. A second repo matches only
	// the glob.
	srv.syncer.SetRepos([]ghclient.RepoRef{
		{
			Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
			PlatformHost: "github.com", RepoPath: "acme/tools-new",
			PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
		},
		{
			Platform: platform.KindGitHub, Owner: "acme", Name: "widgets",
			PlatformHost: "github.com", RepoPath: "acme/widgets",
			PlatformExternalID: "repo-w",
		},
	})

	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/*", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	assert.True(t, srv.syncer.IsTrackedRepo("acme", "tools-new"),
		"deleting the glob must keep the renamed repo its exact entry still claims")
	assert.False(t, srv.syncer.IsTrackedRepo("acme", "widgets"))
}

func TestHandleDeleteExactEntryClearsProvenanceOnGlobKeptRepo(t *testing.T) {
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:  new(repo),
				Owner: &gh.User{Login: new(owner)},
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:  new("tools-new"),
				Owner: &gh.User{Login: new(owner)},
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "tools"

[[repos]]
owner = "acme"
name = "*"
`, mock)

	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	// Removing the exact entry keeps the repo through the glob, but its
	// provenance now points at a config entry that no longer exists and
	// must not survive to claim a future entry with the same path.
	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/tools", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.True(t, srv.syncer.IsTrackedRepo("acme", "tools-new"))
	assert.Empty(t, trackedRepoProvenancePath(srv, "acme", "tools-new"),
		"provenance must clear when its exact entry is removed")
}

func TestHandleDeleteExactEntryIgnoresSamePathOnOtherHost(t *testing.T) {
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:  new(repo),
				Owner: &gh.User{Login: new(owner)},
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:  new("tools-new"),
				Owner: &gh.User{Login: new(owner)},
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "tools"

[[repos]]
owner = "acme"
name = "*"

[[repos]]
owner = "acme"
name = "tools"
platform_host = "ghe.example.com"
`, mock)

	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	// The remaining acme/tools entry lives on a different host; it cannot
	// keep the deleted github.com entry's provenance alive.
	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/tools", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.True(t, srv.syncer.IsTrackedRepo("acme", "tools-new"))
	assert.Empty(t, trackedRepoProvenancePath(srv, "acme", "tools-new"),
		"an entry with the same path on another host must not retain provenance")
}

func TestHandleDeleteExactEntryIgnoresSamePathOnOtherProvider(t *testing.T) {
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:  new(repo),
				Owner: &gh.User{Login: new(owner)},
			}, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:  new("tools-new"),
				Owner: &gh.User{Login: new(owner)},
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "tools"

[[repos]]
owner = "acme"
name = "*"

[[repos]]
platform = "gitlab"
platform_host = "github.com"
owner = "acme"
name = "tools"
`, mock)

	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-x", ConfiguredRepoPath: "acme/tools",
	}})

	// The remaining acme/tools entry shares the host but belongs to a
	// different provider; it cannot keep the deleted GitHub entry's
	// provenance alive.
	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/acme/tools", nil)

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.True(t, srv.syncer.IsTrackedRepo("acme", "tools-new"))
	assert.Empty(t, trackedRepoProvenancePath(srv, "acme", "tools-new"),
		"an entry with the same path on another provider must not retain provenance")
}

func TestHandleDeleteRepoUsesProviderHostQuery(t *testing.T) {
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[repos]]
platform = "gitlab"
platform_host = "gitlab.com"
owner = "acme"
name = "widget"
`, &mockGH{})

	rr := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/host/gitlab.com/repo/gl/acme/widget", nil)

	require.Equal(http.StatusNoContent, rr.Code, rr.Body.String())

	settingsRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, settingsRR.Code, settingsRR.Body.String())
	var resp settingsResponse
	require.NoError(json.NewDecoder(settingsRR.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal(t, "github", resp.Repos[0].Provider)
	assert.Equal(t, "github.com", resp.Repos[0].PlatformHost)
}

func TestRefreshRepoPreservesExistingWhenResolutionFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fail := true
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			if fail {
				return nil, errors.New("boom")
			}
			return []*gh.Repository{{
				Name:     new("kenn-forge"),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)
	// Prime the syncer with a previously resolved match so we can
	// verify it survives a failed refresh.
	srv.syncer.SetRepos([]ghclient.RepoRef{{
		Owner:        "roborev-dev",
		Name:         "kenn-forge",
		PlatformHost: "github.com",
	}})

	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/roborev-dev/*/refresh", nil)

	require.Equal(http.StatusBadGateway, rr.Code, rr.Body.String())
	assert.True(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))
}

func TestGetSettingsDoesNotCallGitHub(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		getRepositoryFn: func(
			_ context.Context, _, _ string,
		) (*gh.Repository, error) {
			require.FailNow("GET /settings must not call GetRepository")
			return nil, nil
		},
		listReposByOwnerFn: func(
			_ context.Context, _ string,
		) ([]*gh.Repository, error) {
			require.FailNow("GET /settings must not call ListRepositoriesByOwner")
			return nil, nil
		},
	}
	// Build the server directly (bypass setup helper) to avoid
	// its startup call to ResolveConfiguredRepos, which would
	// trip the failing mock during seeding.
	dir := t.TempDir()
	database := dbtest.Open(t)
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	clients := map[string]ghclient.Client{"github.com": mock}
	syncer := ghclient.NewSyncer(
		clients, database, nil,
		[]ghclient.RepoRef{{
			Owner: "acme", Name: "widget",
			PlatformHost: "github.com",
		}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{},
	)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal(1, resp.Repos[0].MatchedRepoCount)
}

func TestGlobMatchingIsCaseInsensitive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{{
				Name:     new("Widget-API"),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "Widget-*"
`, mock)

	rr := testutil.DoJSON(
		t, srv, http.MethodPost,
		"/api/v1/repo/gh/acme/Widget-*/refresh", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.True(srv.syncer.IsTrackedRepo("acme", "Widget-API"))
}

func TestAddRepoDoesNotDropConcurrentActivityChange(t *testing.T) {
	// Pre-check for the race fix: handleAddRepo must not
	// overwrite a concurrent handleUpdateSettings change.
	// The setup mutates s.cfg.Activity after the add's
	// pre-check but before its save, then verifies the
	// activity change survives in both memory and on disk.
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)

	// Change activity via the update handler.
	rr := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings",
		updateSettingsRequest{
			Activity: &config.Activity{
				ViewMode:  "threaded",
				TimeRange: "30d",
			},
		})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	// Add a new repo; handler should preserve activity.
	addRR := testutil.DoJSON(
		t, srv, http.MethodPost, "/api/v1/repos",
		map[string]string{
			"provider": "github", "host": "github.com",
			"owner": "other-org", "name": "other-repo",
		})

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal("30d", cfg2.Activity.TimeRange)
	assert.Len(cfg2.Repos, 2)
}

// TestConcurrentRefreshAndDeleteDoesNotResurrect exercises the
// race where a refresh of a glob is in-flight (blocked on the
// GitHub call) while a DELETE removes that glob. Before the fix
// the refresh would apply its stale expansion after the delete
// and re-add the removed repos to the syncer's tracked set.
func TestConcurrentRefreshAndDeleteDoesNotResurrect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls atomic.Int32
	ghBlocked := make(chan struct{}, 1)
	ghUnblock := make(chan struct{})
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			// The setup helper resolves the glob once at
			// server construction; block only on the second
			// call (the refresh request under test).
			if calls.Add(1) == 2 {
				ghBlocked <- struct{}{}
				<-ghUnblock
			}
			return []*gh.Repository{{
				Name:     new("kenn-forge"),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)
	require.True(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))

	refreshDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		// Inline the request (no testify helpers) so the
		// linter does not flag assertions inside the goroutine.
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/repo/gh/roborev-dev/*/refresh", nil,
		)
		req.Host = "127.0.0.1:8091"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		refreshDone <- rr
	}()

	select {
	case <-ghBlocked:
	case <-time.After(5 * time.Second):
		require.FailNow("refresh did not reach the GH mock")
	}

	delRR := testutil.DoJSON(
		t, srv, http.MethodDelete,
		"/api/v1/repo/gh/roborev-dev/*", nil)

	require.Equal(http.StatusNoContent, delRR.Code, delRR.Body.String())
	require.False(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"))

	close(ghUnblock)
	var refreshRR *httptest.ResponseRecorder
	select {
	case refreshRR = <-refreshDone:
	case <-time.After(5 * time.Second):
		require.FailNow("refresh did not complete")
	}
	// Refresh should observe that the glob no longer exists
	// and report 404 rather than applying its stale expansion.
	assert.Equal(http.StatusNotFound, refreshRR.Code, refreshRR.Body.String())

	// The deleted repo must not have reappeared after the
	// refresh completed.
	assert.False(srv.syncer.IsTrackedRepo("roborev-dev", "kenn-forge"),
		"deleted repo resurrected by concurrent refresh")
}

// TestHandleUpdateSettingsPreservesTmuxCommand drives a real
// settings-mutation HTTP call against a config that has a [tmux]
// section on disk, then reloads the config and asserts the Tmux
// command array survived the Save round-trip. This pins down the
// operator-visible contract: mutating activity settings (or any
// other field the UI touches) must not silently erase tmux.command.
func TestHandleUpdateSettingsPreservesTmuxCommand(t *testing.T) {
	assert := assert.New(t)
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[tmux]
command = ["systemd-run", "--user", "--scope", "tmux"]
`, &mockGH{})

	body := updateSettingsRequest{
		Activity: &config.Activity{
			ViewMode:  "threaded",
			TimeRange: "30d",
		},
	}
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	reloaded, err := config.Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "tmux"},
		reloaded.Tmux.Command,
	)
	// Sanity: the mutation actually took effect, so Save did write.
	assert.Equal("30d", reloaded.Activity.TimeRange)
}

func TestHandlePreviewReposFiltersAndMarksAlreadyConfigured(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	pushedNewer := gh.Timestamp{Time: time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)}
	pushedOlder := gh.Timestamp{Time: time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)}
	privateRepo := true
	publicRepo := false
	regularRepo := false
	forkRepo := true
	mock := &mockGH{
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:        new("widget"),
					Owner:       &gh.User{Login: new(owner)},
					Description: new("already configured widget"),
					Private:     &privateRepo,
					Fork:        &regularRepo,
					Archived:    new(false),
					PushedAt:    &pushedOlder,
				},
				{
					Name:        new("widget-api"),
					Owner:       &gh.User{Login: new(owner)},
					Description: new("api service"),
					Private:     &publicRepo,
					Fork:        &forkRepo,
					Archived:    new(false),
					PushedAt:    &pushedNewer,
				},
				{
					Name:     new("widget-archive"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(true),
					PushedAt: &pushedNewer,
				},
				{
					Name:     new("other"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[[repos]]
owner = "acme"
name = "widget-*"
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    " ACME ",
		"pattern":  " Widget* ",
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp repoPreviewResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 2)
	assert.Equal("ACME", resp.Owner)
	assert.Equal("Widget*", resp.Pattern)
	assert.Equal("acme", resp.Repos[0].Owner)
	assert.Equal("widget", resp.Repos[0].Name)
	assert.Equal("already configured widget", *resp.Repos[0].Description)
	assert.True(resp.Repos[0].Private)
	assert.True(resp.Repos[0].AlreadyConfigured)
	require.NotNil(resp.Repos[0].PushedAt)
	assert.Equal(pushedOlder.Time.UTC().Format(time.RFC3339), *resp.Repos[0].PushedAt)
	assert.Equal("widget-api", resp.Repos[1].Name)
	assert.False(resp.Repos[1].Private)
	assert.True(resp.Repos[1].Fork)
	assert.False(resp.Repos[1].AlreadyConfigured)
	assert.NotContains(rr.Body.String(), "widget-archive")
	assert.NotContains(rr.Body.String(), "other")
}

func TestHandlePreviewReposRoutesGitHubByOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ownerClient := func(expected, repoName string) *mockGH {
		return &mockGH{listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			if !strings.EqualFold(owner, expected) {
				return nil, fmt.Errorf("wrong owner route: %s", owner)
			}
			return []*gh.Repository{{
				Name: new(repoName), Owner: &gh.User{Login: new(expected)},
				Archived: new(false),
			}}, nil
		}}
	}
	router, err := ghclient.NewHostRouter(
		"github.com",
		&ghclient.Route{Key: ghclient.RouteKey{Host: "github.com"}, Client: &mockGH{}},
		&ghclient.Route{Key: ghclient.RouteKey{Host: "github.com", Owner: "org-a"}, Client: ownerClient("org-a", "repo-a")},
		&ghclient.Route{Key: ghclient.RouteKey{Host: "github.com", Owner: "org-b"}, Client: ownerClient("org-b", "repo-b")},
	)
	require.NoError(err)
	routed, err := ghclient.NewRoutedClient(router)
	require.NoError(err)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
host = "127.0.0.1"
port = 8091
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": routed}, database, nil,
		nil, time.Minute, nil, nil,
	)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true})

	preview := func(owner string) repoPreviewResponse {
		rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
			"provider": "github", "host": "github.com",
			"owner": owner, "pattern": "*",
		})

		require.Equal(http.StatusOK, rr.Code, rr.Body.String())
		var resp repoPreviewResponse
		require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
		return resp
	}

	orgA := preview("org-a")
	require.Len(orgA.Repos, 1)
	assert.Equal("repo-a", orgA.Repos[0].Name)
	orgB := preview("org-b")
	require.Len(orgB.Repos, 1)
	assert.Equal("repo-b", orgB.Repos[0].Name)
}

func TestHandlePreviewReposReportsUnconfiguredGitHubProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MIDDLEMAN_GITHUB_TOKEN", "")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "MIDDLEMAN_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github", "host": "github.com",
		"owner": "acme", "pattern": "widget",
	})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.True(strings.HasPrefix(
		rr.Header().Get("Content-Type"), "application/problem+json",
	))
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeBadRequest, problem.Code)
	assert.Contains(problem.Detail, "provider_not_configured")
	assert.Contains(problem.Detail, "github.com")
}

func TestHandlePreviewReposReportsMissingOwnerRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	router, err := ghclient.NewHostRouter(
		"github.com",
		&ghclient.Route{
			Key:    ghclient.RouteKey{Host: "github.com", Owner: "org-a"},
			Client: &mockGH{},
		},
	)
	require.NoError(err)
	routed, err := ghclient.NewRoutedClient(router)
	require.NoError(err)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
host = "127.0.0.1"
port = 8091
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": routed}, database, nil,
		nil, time.Minute, nil, nil,
	)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true})

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github", "host": "github.com",
		"owner": "org-b", "pattern": "*",
	})

	require.Equal(http.StatusBadGateway, rr.Code, rr.Body.String())
	assert.Contains(rr.Body.String(), "org-b")
	assert.Contains(rr.Body.String(), "github.com")
	assert.NotContains(rr.Body.String(), "org-a")
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeUpstreamError, problem.Code)
}

func TestHandlePreviewReposFallsBackToListWhenExactLookupFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	privateRepo := true
	forkRepo := false
	pushedAt := gh.Timestamp{Time: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	name := "dotfiles2026"
	ownerLogin := "mariusvniekerk"
	description := "personal dotfiles"
	var listCalls atomic.Int32
	var getCalls atomic.Int32
	mock := &mockGH{
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			listCalls.Add(1)
			assert.Equal("mariusvniekerk", owner)
			return []*gh.Repository{
				{
					Name:        &name,
					Owner:       &gh.User{Login: &ownerLogin},
					Description: &description,
					Private:     &privateRepo,
					Fork:        &forkRepo,
					Archived:    new(false),
					PushedAt:    &pushedAt,
				},
			}, nil
		},
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			getCalls.Add(1)
			assert.Equal("mariusvniekerk", owner)
			assert.Equal("dotfiles2026", repo)
			return nil, errors.New("not found")
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "mariusvniekerk",
		"pattern":  "dotfiles2026",
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp repoPreviewResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal(int32(1), getCalls.Load())
	assert.Equal(int32(1), listCalls.Load())
	assert.Equal("mariusvniekerk", resp.Repos[0].Owner)
	assert.Equal("dotfiles2026", resp.Repos[0].Name)
	assert.Equal("personal dotfiles", *resp.Repos[0].Description)
	assert.True(resp.Repos[0].Private)
	assert.False(resp.Repos[0].Fork)
	assert.False(resp.Repos[0].AlreadyConfigured)
	require.NotNil(resp.Repos[0].PushedAt)
	assert.Equal(pushedAt.Time.UTC().Format(time.RFC3339), *resp.Repos[0].PushedAt)
}

func TestHandlePreviewReposUsesExactLookupForConcreteRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	privateRepo := true
	forkRepo := false
	pushedAt := gh.Timestamp{Time: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	name := "tesseract-feedstock"
	ownerLogin := "anacondarecipes"
	description := "A conda-smithy repository for tesseract"
	var listCalls atomic.Int32
	var getCalls atomic.Int32
	mock := &mockGH{
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			listCalls.Add(1)
			assert.Fail("concrete repo preview should not list repositories", "owner: %s", owner)
			return []*gh.Repository{}, nil
		},
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			getCalls.Add(1)
			assert.Equal("anacondarecipes", owner)
			assert.Equal("tesseract-feedstock", repo)
			return &gh.Repository{
				Name:        &name,
				Owner:       &gh.User{Login: &ownerLogin},
				Description: &description,
				Private:     &privateRepo,
				Fork:        &forkRepo,
				Archived:    new(false),
				PushedAt:    &pushedAt,
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "anacondarecipes",
		"pattern":  "tesseract-feedstock",
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp repoPreviewResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal(int32(1), getCalls.Load())
	assert.Equal(int32(0), listCalls.Load())
	assert.Equal("anacondarecipes", resp.Repos[0].Owner)
	assert.Equal("tesseract-feedstock", resp.Repos[0].Name)
	assert.Equal("A conda-smithy repository for tesseract", *resp.Repos[0].Description)
	assert.True(resp.Repos[0].Private)
	assert.False(resp.Repos[0].Fork)
	assert.False(resp.Repos[0].AlreadyConfigured)
	require.NotNil(resp.Repos[0].PushedAt)
	assert.Equal(pushedAt.Time.UTC().Format(time.RFC3339), *resp.Repos[0].PushedAt)
}

func TestHandlePreviewReposRejectsInvalidPattern(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "acme*",
		"pattern":  "widget",
	})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(rr.Body.String(), "glob syntax in owner is not supported")

	rr = testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "acme",
		"pattern":  "widget[",
	})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(rr.Body.String(), "invalid glob pattern")
}

func TestHandlePreviewReposSupportsGitLabNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedAt := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	provider := repoImportTestProvider{
		kind: platform.KindGitLab,
		host: "gitlab.example.com",
		repos: []platform.Repository{
			{
				Ref: platform.RepoRef{
					Platform: platform.KindGitLab,
					Host:     "gitlab.example.com",
					Owner:    "Group/Subgroup",
					Name:     "Project",
					RepoPath: "Group/Subgroup/Project",
				},
				Description: "gitlab project",
				Private:     true,
				UpdatedAt:   updatedAt,
			},
			{
				Ref: platform.RepoRef{
					Platform: platform.KindGitLab,
					Host:     "gitlab.example.com",
					Owner:    "Group/Subgroup",
					Name:     "Project-Archived",
					RepoPath: "Group/Subgroup/Project-Archived",
				},
				Archived: true,
			},
			{
				Ref: platform.RepoRef{
					Platform: platform.KindGitLab,
					Host:     "gitlab.example.com",
					Owner:    "Group/Subgroup",
					Name:     "Other",
					RepoPath: "Group/Subgroup/Other",
				},
			},
		},
	}
	srv, _, _ := setupTestServerWithConfigProviders(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com"
owner = "Group/Subgroup"
name = "Project"
`, &mockGH{}, provider)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "gitlab",
		"host":     "gitlab.example.com",
		"owner":    "Group/Subgroup",
		"pattern":  "Project*",
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp repoPreviewResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("gitlab", resp.Provider)
	assert.Equal("gitlab.example.com", resp.PlatformHost)
	assert.Equal("Group/Subgroup", resp.Owner)
	assert.Equal("Project*", resp.Pattern)
	assert.Equal("gitlab", resp.Repos[0].Provider)
	assert.Equal("gitlab.example.com", resp.Repos[0].PlatformHost)
	assert.Equal("Group/Subgroup", resp.Repos[0].Owner)
	assert.Equal("Project", resp.Repos[0].Name)
	assert.Equal("Group/Subgroup/Project", resp.Repos[0].RepoPath)
	assert.Equal("gitlab project", *resp.Repos[0].Description)
	assert.True(resp.Repos[0].Private)
	assert.True(resp.Repos[0].AlreadyConfigured)
	require.NotNil(resp.Repos[0].PushedAt)
	assert.Equal(updatedAt.Format(time.RFC3339), *resp.Repos[0].PushedAt)
	assert.NotContains(rr.Body.String(), "Project-Archived")
	assert.NotContains(rr.Body.String(), "Other")
}

func TestHandlePreviewReposSupportsForgejoOrgFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedAt := time.Date(2026, 5, 2, 14, 0, 0, 0, time.UTC)
	transport := &gitealikeImportTransport{
		userReposErr: platform.ErrNotFound,
		orgRepos: []gitealike.RepositoryDTO{
			{
				ID:          101,
				Owner:       gitealike.UserDTO{UserName: "ForgeOrg"},
				Name:        "Widget",
				FullName:    "ForgeOrg/Widget",
				Description: "forgejo widget",
				Private:     true,
				Updated:     updatedAt,
			},
			{
				ID:       102,
				Owner:    gitealike.UserDTO{UserName: "ForgeOrg"},
				Name:     "Widget-Archived",
				FullName: "ForgeOrg/Widget-Archived",
				Archived: true,
			},
			{
				ID:       103,
				Owner:    gitealike.UserDTO{UserName: "ForgeOrg"},
				Name:     "Other",
				FullName: "ForgeOrg/Other",
			},
		},
	}
	provider := gitealike.NewProvider(
		platform.KindForgejo, "codeberg.example.com", transport,
	)
	srv, _, _ := setupTestServerWithConfigProviders(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
platform = "forgejo"
platform_host = "codeberg.example.com"
owner = "ForgeOrg"
name = "Widget"
repo_path = "ForgeOrg/Widget"
`, &mockGH{}, provider)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/preview", map[string]string{
		"provider": "forgejo",
		"host":     "codeberg.example.com",
		"owner":    "ForgeOrg",
		"pattern":  "Widget*",
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	var resp repoPreviewResponse
	require.NoError(json.NewDecoder(strings.NewReader(body)).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("forgejo", resp.Provider)
	assert.Equal("codeberg.example.com", resp.PlatformHost)
	assert.Equal("ForgeOrg", resp.Owner)
	assert.Equal("forgejo", resp.Repos[0].Provider)
	assert.Equal("codeberg.example.com", resp.Repos[0].PlatformHost)
	assert.Equal("ForgeOrg", resp.Repos[0].Owner)
	assert.Equal("Widget", resp.Repos[0].Name)
	assert.Equal("ForgeOrg/Widget", resp.Repos[0].RepoPath)
	assert.Equal("forgejo widget", *resp.Repos[0].Description)
	assert.True(resp.Repos[0].Private)
	assert.True(resp.Repos[0].AlreadyConfigured)
	require.NotNil(resp.Repos[0].PushedAt)
	assert.Equal(updatedAt.Format(time.RFC3339), *resp.Repos[0].PushedAt)
	assert.NotContains(body, "Widget-Archived")
	assert.NotContains(body, "Other")
}

func TestHandleBulkAddReposPersistsExactRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var getCalls atomic.Int32
	mock := &mockGH{
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			getCalls.Add(1)
			return &gh.Repository{
				Name:     new(strings.ToUpper(repo)),
				Owner:    &gh.User{Login: new(strings.ToUpper(owner))},
				Archived: new(false),
			}, nil
		},
	}
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)
	callsAfterSetup := getCalls.Load()

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{"provider": "github", "host": "github.com", "owner": " acme ", "name": " api ", "repo_path": " acme/api "},
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "worker", "repo_path": "acme/worker"},
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "api", "repo_path": "acme/api"},
		},
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())
	assert.GreaterOrEqual(getCalls.Load(), callsAfterSetup+2)

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 3)
	assert.Equal("acme", resp.Repos[1].Owner)
	assert.Equal("api", resp.Repos[1].Name)
	assert.Equal("worker", resp.Repos[2].Name)
	assert.True(srv.syncer.IsTrackedRepo("acme", "api"))
	assert.True(srv.syncer.IsTrackedRepo("acme", "worker"))

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 3)
	assert.Equal("api", cfg2.Repos[1].Name)
	assert.Equal("worker", cfg2.Repos[2].Name)
}

func TestHandleBulkAddReposPersistsGitLabProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "Group/Subgroup",
		Name:               "Project",
		RepoPath:           "Group/Subgroup/Project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		WebURL:             "https://gitlab.example.com/Group/Subgroup/Project",
		CloneURL:           "https://gitlab.example.com/Group/Subgroup/Project.git",
		DefaultBranch:      "main",
	}
	provider := repoImportTestProvider{
		kind: platform.KindGitLab,
		host: "gitlab.example.com",
		repos: []platform.Repository{{
			Ref:                ref,
			PlatformID:         ref.PlatformID,
			PlatformExternalID: ref.PlatformExternalID,
			WebURL:             ref.WebURL,
			CloneURL:           ref.CloneURL,
			DefaultBranch:      ref.DefaultBranch,
		}},
	}
	srv, database, cfgPath := setupTestServerWithConfigProviders(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091
`, &mockGH{}, provider)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{
				"provider":  "gitlab",
				"host":      "gitlab.example.com",
				"repo_path": "Group/Subgroup/Project",
			},
		},
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("gitlab", resp.Repos[0].Provider)
	assert.Equal("gitlab.example.com", resp.Repos[0].PlatformHost)
	assert.Equal("Group/Subgroup", resp.Repos[0].Owner)
	assert.Equal("Project", resp.Repos[0].Name)
	assert.Equal("Group/Subgroup/Project", resp.Repos[0].RepoPath)

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal("gitlab", cfg2.Repos[0].Platform)
	assert.Equal("gitlab.example.com", cfg2.Repos[0].PlatformHost)
	assert.Equal("Group/Subgroup", cfg2.Repos[0].Owner)
	assert.Equal("Project", cfg2.Repos[0].Name)
	assert.Equal("Group/Subgroup/Project", cfg2.Repos[0].RepoPath)
	assert.True(srv.syncer.IsTrackedRepoOnHost("Group/Subgroup", "Project", "gitlab.example.com"))

	dbRepo, err := database.GetRepoByIdentity(t.Context(), platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(dbRepo)
	assert.Equal("gitlab", dbRepo.Platform)
	assert.Equal("gitlab.example.com", dbRepo.PlatformHost)
	assert.Equal("Group/Subgroup/Project", dbRepo.RepoPath)
}

func TestWorktreeBasePathResolverMatchesProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := &Server{cfg: &config.Config{Repos: []config.Repo{
		{
			Platform:         "github",
			PlatformHost:     "forge.example.com",
			PlatformRepoID:   "github-widget",
			Owner:            "acme",
			Name:             "widget",
			WorktreeBasePath: "/tmp/github-widget",
		},
		{
			Platform:         "gitlab",
			PlatformHost:     "forge.example.com",
			PlatformRepoID:   "gitlab-widget",
			Owner:            "acme",
			Name:             "widget",
			WorktreeBasePath: "/tmp/gitlab-widget",
		},
	}}}

	got, ok, err := srv.worktreeBasePathForRepo(
		t.Context(), workspace.WorktreeBaseRepository{
			Platform: "gitlab", PlatformHost: "forge.example.com",
			PlatformRepoID: "gitlab-widget", Owner: "acme", Name: "widget",
		},
	)

	require.NoError(err)
	require.True(ok)
	assert.Equal("/tmp/gitlab-widget", got)

	_, ok, err = srv.worktreeBasePathForRepo(
		t.Context(), workspace.WorktreeBaseRepository{
			Platform: "gitlab", PlatformHost: "forge.example.com",
			PlatformRepoID: "replacement-widget", Owner: "acme", Name: "widget",
		},
	)
	require.NoError(err)
	assert.False(ok)
}

func TestWorktreeBasePathResolverMatchesRegisteredProjectIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "provider-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	_, err = database.CreateProject(t.Context(), db.CreateProjectInput{
		DisplayName: "widget", LocalPath: "/work/widget",
		RepoID: sql.NullInt64{Int64: repoID, Valid: true},
	})
	require.NoError(err)
	srv := &Server{db: database}

	got, ok, err := srv.worktreeBasePathForRepo(
		t.Context(), workspace.WorktreeBaseRepository{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "provider-widget", Owner: "acme", Name: "widget",
		},
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal("/work/widget", got)

	_, ok, err = srv.worktreeBasePathForRepo(
		t.Context(), workspace.WorktreeBaseRepository{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "replacement-widget", Owner: "acme", Name: "widget",
		},
	)
	require.NoError(err)
	assert.False(ok)
}

func TestApplyProviderSettingsMatchesWorktreePathByStableIdentity(t *testing.T) {
	local := settingsResponse{Repos: []ghclient.ConfiguredRepoStatus{
		{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
			WorktreeBasePath: "/work/widget",
		},
		{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-old", Owner: "acme", Name: "reused",
			WorktreeBasePath: "/work/old",
		},
	}}
	provider := settingsResponse{Repos: []ghclient.ConfiguredRepoStatus{
		{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme-renamed", Name: "widget-renamed",
		},
		{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-new", Owner: "acme", Name: "reused",
		},
	}}

	local.applyProviderSettings(provider)

	require.Len(t, local.Repos, 2)
	assert.Equal(t, "/work/widget", local.Repos[0].WorktreeBasePath)
	assert.Empty(t, local.Repos[1].WorktreeBasePath)
}

func TestProviderSettingsRepositoryObservationUsesHubTime(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	hubObservedAt := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		db:  database,
		now: func() time.Time { return hubObservedAt.Add(24 * time.Hour) },
	}
	provider := providerSettingsProjection{
		Settings: settingsResponse{Repos: []ghclient.ConfiguredRepoStatus{{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget", TrackedRepoPath: "acme/widget",
		}}},
		RepositoryObservations: []providerRepositoryObservation{{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget", ObservedAt: hubObservedAt,
		}},
	}

	changed, err := srv.observeProviderSettingsRepositories(
		t.Context(), provider.RepositoryObservations,
	)
	require.NoError(err)
	require.True(changed)

	source := hubProviderSource{db: database}
	require.NoError(source.observeRepositoryDescriptor(t.Context(), providerplane.RepositoryDescriptor{
		ProtocolVersion: federation.ProtocolVersion,
		Provider:        "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
		Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git",
		DefaultBranch: "main", ObservedAt: hubObservedAt.Add(time.Minute),
	}))
}

func TestRepositoryDescriptorAcceptsSupersededSameRouteObservation(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	newer := time.Date(2026, time.August, 24, 12, 1, 0, 0, time.UTC)
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
	}
	_, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), identity, newer,
	)
	require.NoError(err)
	require.True(accepted)

	source := hubProviderSource{db: database}
	require.NoError(source.observeRepositoryDescriptor(t.Context(), providerplane.RepositoryDescriptor{
		ProtocolVersion: federation.ProtocolVersion,
		Provider:        "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
		Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git",
		DefaultBranch: "main", SnapshotRevision: 1, ObservedAt: newer.Add(-time.Second),
	}))

	identity.Owner = "acme-renamed"
	_, accepted, err = database.ReconcileRepositoryObservation(
		t.Context(), identity, newer.Add(time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	require.Error(source.observeRepositoryDescriptor(t.Context(), providerplane.RepositoryDescriptor{
		ProtocolVersion: federation.ProtocolVersion,
		Provider:        "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
		Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git",
		DefaultBranch: "main", SnapshotRevision: 1, ObservedAt: newer,
	}))
}

func TestProviderSettingsProjectionCarriesCatalogObservationTime(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	observedAt := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	_, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget",
		}, observedAt,
	)
	require.NoError(err)
	require.True(accepted)
	srv := &Server{db: database}

	projection, err := srv.buildProviderSettingsProjection(
		t.Context(), settingsResponse{Repos: []ghclient.ConfiguredRepoStatus{{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
			RepoPath: "acme/widget", TrackedRepoPath: "acme/widget",
		}}},
	)

	require.NoError(err)
	require.Len(projection.RepositoryObservations, 1)
	assert.Equal(t, observedAt, projection.RepositoryObservations[0].ObservedAt)
}

func TestLocalSettingsCorrelateRenamedRepositoryThroughCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, _ := setupTestServerWithConfig(t)
	srv.cfg.Repos[0].WorktreeBasePath = "/work/widget"
	observedAt := time.Now().UTC()
	seedVerifiedRepo(t, database, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-widget", Owner: "acme", Name: "widget",
	})
	_, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-widget", Owner: "acme-renamed", Name: "widget-renamed",
		}, observedAt.Add(time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	srv.syncer = nil

	settings, err := srv.buildLocalSettingsResponse(t.Context())
	require.NoError(err)
	require.Len(settings.Repos, 1)
	assert.Equal("repo-widget", settings.Repos[0].PlatformRepoID)
	assert.Equal("acme-renamed/widget-renamed", settings.Repos[0].TrackedRepoPath)
}

func TestLocalSettingsDoNotCorrelateReusedRepositoryRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, _ := setupTestServerWithConfig(t)
	seedVerifiedRepo(t, database, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-old", Owner: "acme", Name: "widget",
	})
	_, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-new", Owner: "acme", Name: "widget",
		}, time.Now().UTC().Add(time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	srv.syncer = nil

	settings, err := srv.buildLocalSettingsResponse(t.Context())
	require.NoError(err)
	require.Len(settings.Repos, 1)
	assert.Empty(settings.Repos[0].PlatformRepoID)
	assert.Empty(settings.Repos[0].TrackedRepoPath)
}

func TestHandleBulkAddReposPersistsGiteaProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	transport := &gitealikeImportTransport{
		repository: gitealike.RepositoryDTO{
			ID:            6262,
			Owner:         gitealike.UserDTO{UserName: "Team"},
			Name:          "Service",
			FullName:      "Team/Service",
			HTMLURL:       "https://gitea.example.com/Team/Service",
			CloneURL:      "https://gitea.example.com/Team/Service.git",
			DefaultBranch: "main",
		},
	}
	provider := gitealike.NewProvider(
		platform.KindGitea, "gitea.example.com", transport,
	)
	srv, database, cfgPath := setupTestServerWithConfigProviders(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091
`, &mockGH{}, provider)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{
				"provider":  "gitea",
				"host":      "gitea.example.com",
				"repo_path": "Team/Service",
			},
		},
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(resp.Repos, 1)
	assert.Equal("gitea", resp.Repos[0].Provider)
	assert.Equal("gitea.example.com", resp.Repos[0].PlatformHost)
	assert.Equal("Team", resp.Repos[0].Owner)
	assert.Equal("Service", resp.Repos[0].Name)
	assert.Equal("Team/Service", resp.Repos[0].RepoPath)

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal("gitea", cfg2.Repos[0].Platform)
	assert.Equal("gitea.example.com", cfg2.Repos[0].PlatformHost)
	assert.Equal("Team", cfg2.Repos[0].Owner)
	assert.Equal("Service", cfg2.Repos[0].Name)
	assert.Equal("Team/Service", cfg2.Repos[0].RepoPath)
	assert.True(srv.syncer.IsTrackedRepoOnHost("Team", "Service", "gitea.example.com"))

	ref := platform.RepoRef{
		Platform:           platform.KindGitea,
		Host:               "gitea.example.com",
		Owner:              "Team",
		Name:               "Service",
		RepoPath:           "Team/Service",
		PlatformID:         6262,
		PlatformExternalID: "6262",
	}
	dbRepo, err := database.GetRepoByIdentity(t.Context(), platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(dbRepo)
	assert.Equal("gitea", dbRepo.Platform)
	assert.Equal("gitea.example.com", dbRepo.PlatformHost)
	assert.Equal("Team/Service", dbRepo.RepoPath)
}

func TestHandleBulkAddReposValidationFailureChangesNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mock := &mockGH{
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			if repo == "missing" {
				return nil, errors.New("not found")
			}
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
	}
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "api", "repo_path": "acme/api"},
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "missing", "repo_path": "acme/missing"},
		},
	})

	require.Equal(http.StatusBadGateway, rr.Code, rr.Body.String())
	assert.False(srv.syncer.IsTrackedRepo("acme", "api"))

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 1)
	assert.Equal("widget", cfg2.Repos[0].Name)
}

func TestHandleBulkAddReposSkipsAlreadyConfiguredBeforeValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var apiCalls atomic.Int32
	mock := &mockGH{
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			if repo == "api" {
				apiCalls.Add(1)
				return nil, errors.New("stale configured repo should not be validated")
			}
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
	}
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "api"
`, mock)
	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "api", "repo_path": "acme/api"},
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "worker", "repo_path": "acme/worker"},
		},
	})

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())
	assert.True(srv.syncer.IsTrackedRepo("acme", "worker"))

	cfg2, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfg2.Repos, 2)
	assert.Equal("worker", cfg2.Repos[1].Name)
}

func TestHandleBulkAddReposReturnsAlreadyConfiguredWhenAllSkippedBeforeValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var apiCalls atomic.Int32
	mock := &mockGH{
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			if repo == "api" {
				apiCalls.Add(1)
			}
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "api"
`, mock)
	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos/bulk", map[string]any{
		"repos": []map[string]string{
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "api", "repo_path": "acme/api"},
		},
	})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(rr.Body.String(), "all selected repositories are already configured")
}

func TestHandleBulkAddReposSkipsAlreadyConfiguredAtApplyTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	unblockGet := make(chan struct{})
	getStarted := make(chan struct{}, 1)
	var apiCalls atomic.Int32
	mock := &mockGH{
		getRepositoryFn: func(_ context.Context, owner, repo string) (*gh.Repository, error) {
			if repo == "api" && apiCalls.Add(1) == 1 {
				getStarted <- struct{}{}
				<-unblockGet
			}
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
	}
	srv, _, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)

	var bulkBody bytes.Buffer
	require.NoError(json.NewEncoder(&bulkBody).Encode(map[string]any{
		"repos": []map[string]string{
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "api", "repo_path": "acme/api"},
			{"provider": "github", "host": "github.com", "owner": "acme", "name": "worker", "repo_path": "acme/worker"},
		},
	}))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		// Inline request avoids testify assertions inside this goroutine.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/bulk", bytes.NewReader(bulkBody.Bytes()))
		req.Host = "127.0.0.1:8091"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		done <- rr
	}()

	select {
	case <-getStarted:
	case <-time.After(5 * time.Second):
		require.FailNow("bulk validation did not start")
	}
	addRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/repos", map[string]string{
		"provider": "github", "host": "github.com", "owner": "acme", "name": "api",
	})

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())
	close(unblockGet)

	var rr *httptest.ResponseRecorder
	select {
	case rr = <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("bulk add did not finish")
	}
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())
	var resp settingsResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal([]string{"widget", "api", "worker"}, []string{resp.Repos[0].Name, resp.Repos[1].Name, resp.Repos[2].Name})
}

// TestSetActiveWorktreeRoute pins the UI focus contract thin clients
// use: PUT /api/v1/ui/active-worktree records the focused worktree
// key, the served SPA config carries it, and an empty key clears it.
func TestSetActiveWorktreeRoute(t *testing.T) {
	require := require.New(t)
	srv, _ := setupTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	put := func(body string) *http.Response {
		req, err := http.NewRequest(
			http.MethodPut,
			ts.URL+"/api/v1/ui/active-worktree",
			strings.NewReader(body),
		)
		require.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(err)
		return resp
	}

	resp := put(`{"key":"local:wt-alpha"}`)
	resp.Body.Close()
	require.Equal(http.StatusNoContent, resp.StatusCode)
	key, set := srv.ActiveWorktreeKey()
	require.True(set)
	require.Equal("local:wt-alpha", key)

	// Empty key clears the focus.
	resp = put(`{"key":""}`)
	resp.Body.Close()
	require.Equal(http.StatusNoContent, resp.StatusCode)
	key, set = srv.ActiveWorktreeKey()
	require.True(set)
	require.Empty(key)
}

func TestFleetSettingsPreserveEnrollmentOwnedRoleAndMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
host = "127.0.0.1"
port = 8091

[api]
require_auth = true

[fleet]
enabled = true
role = "hub"
base_url = "https://hub.example"

[[fleet.members]]
node_id = "fedcba9876543210fedcba9876543210"
name = "Build Box"
base_url = "https://spoke.example"
state = "active"
	`, &mockGH{})

	get := func() fleetSettingsResponse {
		response := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings/fleet", nil)
		require.Equal(http.StatusOK, response.Code)
		var result fleetSettingsResponse
		require.NoError(json.NewDecoder(response.Body).Decode(&result))
		return result
	}
	initial := get()
	assert.Equal(config.FleetRoleHub, initial.Role)
	require.Len(initial.Members, 1)
	assert.Equal("Build Box", initial.Members[0].Name)
	assert.Empty(initial.Enrollments)

	forbiddenLifecyclePayload := map[string]any{
		"enabled": true, "role": "spoke",
		"sessions": map[string]any{"include_unmanaged_details": false},
		"members": []map[string]any{{
			"node_id": "fedcba9876543210fedcba9876543210",
			"name":    "Renamed Build Box", "base_url": "https://spoke.example",
			"state": "active",
		}},
	}
	response := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/settings/fleet", forbiddenLifecyclePayload)

	require.Equal(http.StatusUnprocessableEntity, response.Code)

	payload := map[string]any{
		"enabled":  true,
		"sessions": map[string]any{"include_unmanaged_details": false},
	}
	response = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings/fleet", payload)
	require.Equal(http.StatusOK, response.Code)
	var updated fleetSettingsResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&updated))
	assert.Equal(config.FleetRoleHub, updated.Role)
	assert.Equal("Build Box", updated.Members[0].Name)
	assert.False(updated.RestartRequired)

	raw, err := os.ReadFile(cfgPath)
	require.NoError(err)
	assert.Contains(string(raw), "Build Box")
	assert.NotContains(string(raw), "Renamed Build Box")
	assert.NotContains(string(raw), `role = "spoke"`)
}

func TestFleetSettingsExposePendingEnrollmentWithoutCredentialMaterial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	enrollments, err := federation.Open(
		filepath.Join(dataDir, "enrollments.json"), federation.StoreOptions{},
	)
	require.NoError(err)
	credentials, err := federationauth.Open(filepath.Join(dataDir, "credentials.json"))
	require.NoError(err)
	const (
		hubID        = "0123456789abcdef0123456789abcdef"
		nodeID       = "fedcba9876543210fedcba9876543210"
		enrollmentID = "11111111111111111111111111111111"
		peerSecret   = "hub-calls-spoke-secret"
	)
	token, err := enrollments.CreateOneTimeToken(federation.Identity{
		NodeID: hubID, BaseURL: "https://hub.example",
	}, time.Now().Add(time.Minute))
	require.NoError(err)
	_, err = enrollments.Begin(t.Context(), token.Token, federation.JoinRequest{
		EnrollmentID: enrollmentID, NodeID: nodeID, Platform: "linux",
		BaseURL: "https://spoke.example", ProtocolVersion: federation.ProtocolVersion,
		HubCredential: peerSecret,
	})
	require.NoError(err)

	srv, _, _ := setupTestServerWithConfigContentAndOptions(t, `
host = "127.0.0.1"
port = 8091
[api]
require_auth = true
[fleet]
enabled = true
role = "hub"
base_url = "https://hub.example"
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		FederationSpokeID:             hubID,
		FederationEnrollments:         enrollments,
		FederationCredentials:         credentials,
	})
	response := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings/fleet", nil)
	require.Equal(http.StatusOK, response.Code)
	assert.Contains(response.Body.String(), enrollmentID)
	assert.NotContains(response.Body.String(), token.Token)
	assert.NotContains(response.Body.String(), peerSecret)
}

func TestRoleAwareSettingsRequireOneOwnerPerNodeWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hub, hubDB, _ := setupTestServerWithConfigContentAndOptions(t, `
host = "127.0.0.1"
port = 8091

[detail]
initial_timeline_entry_limit = 50

[workspaces]
auto_assign_on_create = false
default_sidebar_view = "diff"

[roborev]
init_managed_clones = false

[[repos]]
platform = "github"
platform_host = "github.com"
owner = "acme"
name = "widget"

	[fleet]
	role = "hub"
	`, &mockGH{}, ServerOptions{HostCheckAllowLoopbackAnyPort: true})
	seedVerifiedRepo(t, hubDB, verifiedGitHubRepoIdentity(
		"github.com", "acme", "widget",
	))
	hubHTTP := httptest.NewTLSServer(hub)
	t.Cleanup(hubHTTP.Close)

	credentials, err := federationauth.Open(filepath.Join(t.TempDir(), "credentials.json"))
	require.NoError(err)
	require.NoError(credentials.StoreOutbound(
		proxyTestHubID, "settings-spoke-secret", federationauth.SpokeToHubScopes(),
	))
	nodeConfigDir := t.TempDir()
	nodeConfigPath := filepath.Join(nodeConfigDir, "config.toml")
	nodeConfigBody := fmt.Sprintf(`
host = "127.0.0.1"
port = 8091

[api]
require_auth = true

[detail]
initial_timeline_entry_limit = 25

[workspaces]
auto_assign_on_create = false
default_sidebar_view = "diff"

[[repos]]
platform = "github"
platform_host = "github.com"
owner = "acme"
name = "widget"

[fleet]
enabled = true
role = "spoke"
base_url = "https://spoke.example"

[fleet.hub]
node_id = %q
base_url = %q
`, proxyTestHubID, hubHTTP.URL)
	require.NoError(os.WriteFile(nodeConfigPath, []byte(nodeConfigBody), 0o600))
	nodeConfig, err := config.Load(nodeConfigPath)
	require.NoError(err)
	spoke := NewWithConfig(
		dbtest.Open(t), nil, nil, nil, nodeConfig, nodeConfigPath,
		ServerOptions{
			HostCheckAllowLoopbackAnyPort:      true,
			FederationSpokeID:                  proxyTestNodeID,
			FederationSpokeActive:              true,
			FederationCredentials:              credentials,
			FederationHTTPClient:               hubHTTP.Client(),
			DisableWorkspaceBackgroundMonitors: true,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, spoke) })

	autoAssign := true
	response := testutil.DoJSON(t, spoke, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Detail:     &config.Detail{InitialTimelineEntryLimit: 75},
		Workspaces: &workspaceSettingsUpdate{AutoAssignOnCreate: &autoAssign},
	})

	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(httpapi.CodeValidationError, problem.Code)
	assert.Equal("mixedSettingsOwnership", problem.Details["reason"])
	hub.cfgMu.Lock()
	assert.Equal(50, hub.cfg.Detail.InitialTimelineEntryLimit)
	assert.False(hub.cfg.Workspaces.AutoAssignOnCreate)
	hub.cfgMu.Unlock()
	spoke.cfgMu.Lock()
	assert.Equal(25, spoke.cfg.Detail.InitialTimelineEntryLimit)
	assert.False(spoke.cfg.Workspaces.AutoAssignOnCreate)
	spoke.cfgMu.Unlock()

	response = testutil.DoJSON(t, spoke, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Detail: &config.Detail{InitialTimelineEntryLimit: 75},
	})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	response = testutil.DoJSON(t, spoke, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Workspaces: &workspaceSettingsUpdate{AutoAssignOnCreate: &autoAssign},
	})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	response = testutil.DoJSON(t, spoke, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Roborev: &roborevSettingsUpdate{InitManagedClones: new(true)},
	})

	require.Equal(http.StatusOK, response.Code, response.Body.String())

	hub.cfgMu.Lock()
	assert.Equal(75, hub.cfg.Detail.InitialTimelineEntryLimit)
	assert.False(hub.cfg.Workspaces.AutoAssignOnCreate)
	hub.cfgMu.Unlock()
	spoke.cfgMu.Lock()
	assert.Equal(25, spoke.cfg.Detail.InitialTimelineEntryLimit)
	assert.True(spoke.cfg.Workspaces.AutoAssignOnCreate)
	assert.True(spoke.cfg.Roborev.InitManagedClones)
	spoke.cfgMu.Unlock()

	worktreeBase := t.TempDir()
	canonicalWorktreeBase, err := filepath.EvalSymlinks(worktreeBase)
	require.NoError(err)
	gitfixture.Run(t, worktreeBase, "init", "--initial-branch=main")
	gitfixture.Run(t, worktreeBase, "remote", "add", "origin", "https://github.com/acme/widget.git")
	response = testutil.DoJSON(
		t, spoke, http.MethodPut,
		"/api/v1/repo/github/acme/widget/worktree-base",
		repoWorktreeBaseRequest{WorktreeBasePath: worktreeBase})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(canonicalWorktreeBase, spoke.cfg.Repos[0].WorktreeBasePath)
	assert.Empty(hub.cfg.Repos[0].WorktreeBasePath)

	var settings settingsResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&settings))
	require.Len(settings.Repos, 1)
	assert.Equal(canonicalWorktreeBase, settings.Repos[0].WorktreeBasePath)
	assert.Equal(75, settings.Detail.InitialTimelineEntryLimit)
	assert.True(settings.Workspaces.AutoAssignOnCreate)
	assert.Equal(config.FleetRoleSpoke, settings.Fleet.Role)

	response = testutil.DoJSON(t, spoke, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.NewDecoder(response.Body).Decode(&settings))
	require.Len(settings.Repos, 1)
	assert.Equal(canonicalWorktreeBase, settings.Repos[0].WorktreeBasePath)
}

func TestNodeWorktreeBaseOverrideFollowsHubRepositoryIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, configPath := setupTestServerWithConfigContent(t, `
host = "127.0.0.1"
port = 8091
`, &mockGH{})
	srv.syncer = nil
	observedAt := time.Now().UTC().Truncate(time.Second)
	projection := providerSettingsResponse{
		Repos: []ghclient.ConfiguredRepoStatus{{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-late", Owner: "acme", Name: "late",
			RepoPath: "acme/late", TrackedRepoPath: "acme/late",
		}},
		RepositoryObservations: []providerRepositoryObservation{{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-late", Owner: "acme", Name: "late",
			RepoPath: "acme/late", ObservedAt: observedAt,
		}},
		RepoPresets: []config.RepoPreset{},
	}
	var reads atomic.Int32
	srv.providerSource = &hubProviderSource{
		db: database,
		client: providerPlaneClientFunc(func(
			_ context.Context, scope federationauth.Scope, request *http.Request,
		) (*http.Response, error) {
			require.Equal(federationauth.ScopeProviderRead, scope)
			require.Equal("/api/v1/federation/provider/settings", request.URL.Path)
			reads.Add(1)
			encoded, err := json.Marshal(projection)
			require.NoError(err)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(encoded)),
				Request:    request,
			}, nil
		}),
	}
	worktreeBase := t.TempDir()
	canonicalWorktreeBase, err := filepath.EvalSymlinks(worktreeBase)
	require.NoError(err)
	gitfixture.Run(t, worktreeBase, "init", "--initial-branch=main")
	gitfixture.Run(t, worktreeBase, "remote", "add", "origin", "https://github.com/acme/late.git")

	response := testutil.DoJSON(
		t, srv, http.MethodPut, "/api/v1/repo/github/acme/late/worktree-base",
		repoWorktreeBaseRequest{WorktreeBasePath: worktreeBase})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	srv.cfgMu.Lock()
	configuredRepos := cloneReloadedConfig(srv.cfg).Repos
	srv.cfgMu.Unlock()
	require.Len(configuredRepos, 1)
	assert.Equal(canonicalWorktreeBase, configuredRepos[0].WorktreeBasePath)

	projection.Repos[0].Owner = "renamed"
	projection.Repos[0].Name = "late-renamed"
	projection.Repos[0].RepoPath = "renamed/late-renamed"
	projection.Repos[0].TrackedRepoPath = "renamed/late-renamed"
	projection.RepositoryObservations[0].Owner = "renamed"
	projection.RepositoryObservations[0].Name = "late-renamed"
	projection.RepositoryObservations[0].RepoPath = "renamed/late-renamed"
	projection.RepositoryObservations[0].ObservedAt = observedAt.Add(time.Minute)
	response = testutil.DoJSON(
		t, srv, http.MethodPut,
		"/api/v1/repo/github/renamed/late-renamed/worktree-base",
		repoWorktreeBaseRequest{})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(int32(2), reads.Load(), "each mutation uses one pre-commit hub snapshot")
	srv.cfgMu.Lock()
	configuredRepos = cloneReloadedConfig(srv.cfg).Repos
	srv.cfgMu.Unlock()
	require.Len(configuredRepos, 1)
	assert.Equal("renamed", configuredRepos[0].Owner)
	assert.Equal("late-renamed", configuredRepos[0].Name)
	assert.Empty(configuredRepos[0].WorktreeBasePath)
	contents, err := os.ReadFile(configPath)
	require.NoError(err)
	assert.Contains(string(contents), `platform_repo_id = "repo-late"`)
	var settings settingsResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&settings))
	require.Len(settings.Repos, 1)
	assert.Equal("repo-late", settings.Repos[0].PlatformRepoID)
	assert.Empty(settings.Repos[0].WorktreeBasePath)
}

func TestNodeLocalSettingsDoNotCommitWhenHubSnapshotIsUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, configPath := setupTestServerWithConfigContent(t, `
host = "127.0.0.1"
port = 8091

[workspaces]
auto_assign_on_create = false
`, &mockGH{})
	srv.providerSource = &hubProviderSource{
		client: providerPlaneClientFunc(func(
			context.Context, federationauth.Scope, *http.Request,
		) (*http.Response, error) {
			return nil, providerplane.ErrHubUnavailable
		}),
	}
	autoAssign := true

	response := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Workspaces: &workspaceSettingsUpdate{AutoAssignOnCreate: &autoAssign},
	})

	require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.False(srv.cfg.Workspaces.AutoAssignOnCreate)
	persisted, err := config.Load(configPath)
	require.NoError(err)
	assert.False(persisted.Workspaces.AutoAssignOnCreate)
}

func TestNodeSettingsLoadWhileFederationIsDisabled(t *testing.T) {
	require := require.New(t)
	srv, _, _ := setupTestServerWithConfig(t)
	srv.cfg.Fleet.Enabled = false
	srv.cfg.Fleet.Role = config.FleetRoleSpoke
	srv.providerSource = &hubProviderSource{
		client: providerPlaneClientFunc(func(
			context.Context, federationauth.Scope, *http.Request,
		) (*http.Response, error) {
			require.Fail("disabled federation must not request hub settings")
			return nil, nil
		}),
		enabled: srv.federationEnabled,
	}

	response := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var settings settingsResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&settings))
	require.False(settings.Fleet.Enabled)
}

func TestInactiveSpokeSettingsStayLocal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, configPath := setupTestServerWithConfigContentAndOptions(t, `
host = "127.0.0.1"
port = 8091

[api]
require_auth = true

[workspaces]
auto_assign_on_create = false

[fleet]
enabled = true
role = "spoke"
base_url = "https://spoke.example"

[fleet.hub]
node_id = "0123456789abcdef0123456789abcdef"
base_url = "https://hub.example"
`, &mockGH{}, ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
		FederationSpokeID:             proxyTestNodeID,
	})
	require.NotNil(srv.providerSource)
	require.Nil(srv.providerSource.client)

	response := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/settings", nil)
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	autoAssign := true
	response = testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		Workspaces: &workspaceSettingsUpdate{AutoAssignOnCreate: &autoAssign},
	})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.True(srv.cfg.Workspaces.AutoAssignOnCreate)
	persisted, err := config.Load(configPath)
	require.NoError(err)
	assert.True(persisted.Workspaces.AutoAssignOnCreate)
}

func TestRoleAwareSettingsRejectFederationWriteToHubLocalPolicy(t *testing.T) {
	srv, _, _ := setupTestServerWithConfig(t)
	autoAssign := true
	ctx := federationauth.WithPrincipal(t.Context(), federationauth.Principal{
		NodeID: proxyTestNodeID,
		Scopes: map[federationauth.Scope]struct{}{
			federationauth.ScopeProviderWrite: {},
		},
	})
	_, err := srv.updateSettings(ctx, &updateSettingsInput{Body: updateSettingsRequest{
		Workspaces: &workspaceSettingsUpdate{AutoAssignOnCreate: &autoAssign},
	}})
	var statusErr huma.StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusForbidden, statusErr.GetStatus())
	assert.False(t, srv.cfg.Workspaces.AutoAssignOnCreate)
}

// TestHandleUpdateSettingsRestoresProjectionAfterRequestCancellation covers the
// window after the setting is persisted and the syncer has already switched: the
// committed state must be reconciled even if the client is gone, or native
// ordering would keep driving the UI and the merge safeguard until some later
// sync happened to re-detect.
func TestHandleUpdateSettingsRestoresProjectionAfterRequestCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[pull_requests]
prefer_github_native_stacks = true
`, &mockGH{})
	ctx := t.Context()
	seedStackedPR(t, database, "acme", "widget", 10, "feat/base", "main", db.MergeRequestStateOpen, "", "")
	seedStackedPR(t, database, "acme", "widget", 11, "feat/tip", "feat/base", db.MergeRequestStateOpen, "", "")
	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC()
	require.NoError(database.ReplaceGitHubNativeStack(ctx, db.GitHubNativeStack{
		RepoID: repo.ID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "native", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 11, State: "open", HeadRef: "feat/tip", HeadSHA: "sha11"},
			{Position: 2, PullRequestNumber: 10, State: "open", HeadRef: "feat/base", HeadSHA: "sha10"},
		},
	}))
	require.NoError(stacks.RunDetectionWithNativeStacks(ctx, database, repo.ID, []int{42}))
	client := setupTestClientWithBaseURL(t, srv, "http://127.0.0.1:8091")
	before, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(before.JSON200)
	require.NotNil(before.JSON200.Members)
	require.Equal([]int64{11, 10}, stackMemberNumbers(before.JSON200.Members))

	var buf bytes.Buffer
	require.NoError(json.NewEncoder(&buf).Encode(updateSettingsRequest{
		PullRequests: &config.PullRequests{},
	}))
	reqCtx, cancel := context.WithCancel(ctx)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", &buf).WithContext(reqCtx)
	req.Host = "127.0.0.1:8091"
	req.Header.Set("Content-Type", "application/json")
	// The client disconnects while the request is being served.
	cancel()
	srv.ServeHTTP(httptest.NewRecorder(), req)

	after, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(after.JSON200)
	require.NotNil(after.JSON200.Members)
	assert.Equal([]int64{10, 11}, stackMemberNumbers(after.JSON200.Members),
		"committed-state reconciliation must not depend on the request context")
}

// TestReconcileNativeStackProjectionSkipsSupersededDisable covers the window
// between the swap and the projection lock. A disable that lost the race to a
// later enable must not replay: the enable has already published native
// ordering, and replaying branch inference over it would leave the projection
// disagreeing with the preference until another sync.
func TestReconcileNativeStackProjectionSkipsSupersededDisable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[pull_requests]
prefer_github_native_stacks = true
`, &mockGH{})
	ctx := t.Context()
	seedStackedPR(t, database, "acme", "widget", 10, "feat/base", "main", db.MergeRequestStateOpen, "", "")
	seedStackedPR(t, database, "acme", "widget", 11, "feat/tip", "feat/base", db.MergeRequestStateOpen, "", "")
	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC()
	require.NoError(database.ReplaceGitHubNativeStack(ctx, db.GitHubNativeStack{
		RepoID: repo.ID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "native", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 11, State: "open", HeadRef: "feat/tip", HeadSHA: "sha11"},
			{Position: 2, PullRequestNumber: 10, State: "open", HeadRef: "feat/base", HeadSHA: "sha10"},
		},
	}))
	require.NoError(stacks.RunDetectionWithNativeStacks(ctx, database, repo.ID, []int{42}))
	client := setupTestClientWithBaseURL(t, srv, "http://127.0.0.1:8091")
	require.True(srv.syncer.PrefersGitHubNativeStacks())

	// A disable observed the enabled value, but by the time it reaches
	// reconciliation a later enable has already won the swap.
	srv.reconcileGitHubNativeStackProjection(true, false)

	after, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(after.JSON200)
	require.NotNil(after.JSON200.Members)
	assert.Equal([]int64{11, 10}, stackMemberNumbers(after.JSON200.Members),
		"a superseded disable must not overwrite the projection the current preference produced")
}

// TestHandleUpdateSettingsRestoresProjectionForUntrackedRepo covers a
// repository dropped from config before the preview is disabled. Nothing will
// sync it again, so if reconciliation only walked the tracked set its stored
// pull requests would keep serving native ordering forever.
func TestHandleUpdateSettingsRestoresProjectionForUntrackedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _ := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[pull_requests]
prefer_github_native_stacks = true
`, &mockGH{})
	ctx := t.Context()
	// "removed" is absent from config, so the syncer never tracked it.
	seedStackedPR(t, database, "acme", "removed", 10, "feat/base", "main", db.MergeRequestStateOpen, "", "")
	seedStackedPR(t, database, "acme", "removed", 11, "feat/tip", "feat/base", db.MergeRequestStateOpen, "", "")
	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "removed"))
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC()
	require.NoError(database.ReplaceGitHubNativeStack(ctx, db.GitHubNativeStack{
		RepoID: repo.ID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "native", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 11, State: "open", HeadRef: "feat/tip", HeadSHA: "sha11"},
			{Position: 2, PullRequestNumber: 10, State: "open", HeadRef: "feat/base", HeadSHA: "sha10"},
		},
	}))
	require.NoError(stacks.RunDetectionWithNativeStacks(ctx, database, repo.ID, []int{42}))
	// The cache row is gone but the projection it produced remains, so the
	// native ordering cannot be found by looking for native rows.
	require.NoError(database.DeleteGitHubNativeStacks(ctx, repo.ID, []int{42}))
	client := setupTestClientWithBaseURL(t, srv, "http://127.0.0.1:8091")
	before, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "removed", 10)
	require.NoError(err)
	require.NotNil(before.JSON200)
	require.NotNil(before.JSON200.Members)
	require.Equal([]int64{11, 10}, stackMemberNumbers(before.JSON200.Members))

	disabled := config.PullRequests{}
	rr := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/settings", updateSettingsRequest{
		PullRequests: &disabled,
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	after, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "removed", 10)
	require.NoError(err)
	require.NotNil(after.JSON200)
	require.NotNil(after.JSON200.Members)
	assert.Equal([]int64{10, 11}, stackMemberNumbers(after.JSON200.Members),
		"a repository no longer tracked must still lose native ordering when the preview is disabled")
}

// TestNewServerRestoresProjectionWhenNativeStacksBootDisabled covers a daemon
// that starts with the preview already off. The setting can be edited while the
// daemon is stopped, or a previous run can save it and exit before reconciling,
// so binding the syncer preference is not enough: stored native ordering would
// drive the merge safeguard until each repository next synced, and forever for
// repositories no longer tracked.
func TestNewServerRestoresProjectionWhenNativeStacksBootDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dir := t.TempDir()
	database := dbtest.Open(t)
	seedStackedPR(t, database, "acme", "widget", 10, "feat/base", "main", db.MergeRequestStateOpen, "", "")
	seedStackedPR(t, database, "acme", "widget", 11, "feat/tip", "feat/base", db.MergeRequestStateOpen, "", "")
	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC()
	require.NoError(database.ReplaceGitHubNativeStack(ctx, db.GitHubNativeStack{
		RepoID: repo.ID, GitHubID: 9001, Number: 42, Size: 2,
		BaseRef: "main", IsOpen: true, GitHubCreatedAt: now,
		ContentFingerprint: "native", LastObservedAt: now,
		Members: []db.GitHubNativeStackMember{
			{Position: 1, PullRequestNumber: 11, State: "open", HeadRef: "feat/tip", HeadSHA: "sha11"},
			{Position: 2, PullRequestNumber: 10, State: "open", HeadRef: "feat/base", HeadSHA: "sha10"},
		},
	}))
	// The last run left native ordering behind.
	require.NoError(stacks.RunDetectionWithNativeStacks(ctx, database, repo.ID, []int{42}))

	// This run boots with the preview off.
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[pull_requests]
prefer_github_native_stacks = false
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	clients := map[string]ghclient.Client{"github.com": &mockGH{}}
	syncer := ghclient.NewSyncer(clients, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := NewWithConfig(database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	client := setupTestClientWithBaseURL(t, srv, "http://127.0.0.1:8091")

	// No sync has run, and the repository is not even tracked.
	resp, err := client.HTTP.GetPullStackWithResponse(ctx, "gh", "acme", "widget", 10)
	require.NoError(err)
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Members)
	assert.Equal([]int64{10, 11}, stackMemberNumbers(resp.JSON200.Members),
		"a server booting with the preview disabled must not serve native ordering")
}
