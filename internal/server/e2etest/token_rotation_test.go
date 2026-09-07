package e2etest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformforgejo "go.kenn.io/forge/platform/forgejo"
	platformgitea "go.kenn.io/forge/platform/gitea"
	platformgitlab "go.kenn.io/forge/platform/gitlab"
)

func TestTokenFileRotationE2EConfigStartupAndHTTPSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "gitlab-token")
	writeTokenFileAtomically(t, tokenPath, "opaque-startup-token-11111\n")

	var tokens []string
	gitlabAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Private-Token"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42":
			_, _ = fmt.Fprint(w, `{
				"id": 42,
				"path": "project",
				"path_with_namespace": "group/project",
				"web_url": "https://gitlab.example.com/group/project",
				"default_branch": "main"
			}`)
		case "/api/v4/projects/42/merge_requests/7":
			_, _ = fmt.Fprint(w, `{
				"id": 7001,
				"iid": 7,
				"project_id": 42,
				"title": "GitLab token-file rotation",
				"state": "opened",
				"web_url": "https://gitlab.example.com/group/project/-/merge_requests/7",
				"author": {"username": "ada", "name": "Ada Lovelace"},
				"source_branch": "feature/token-rotation",
				"target_branch": "main",
				"created_at": "2026-04-01T10:00:00Z",
				"updated_at": "2026-04-02T10:00:00Z"
			}`)
		case "/api/v4/projects/42/merge_requests/7/discussions",
			"/api/v4/projects/42/merge_requests/7/commits":
			_, _ = fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gitlabAPI.Close)

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, fmt.Appendf(nil, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com"
owner = "group"
name = "project"
repo_path = "group/project"
token_file = %q
`, tokenPath), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet, source := collectStartupTokenSource(
		t, cfg, tokenauth.Key{Platform: string(platform.KindGitLab), Host: "gitlab.example.com"},
	)
	client, err := platformgitlab.NewClient(
		"gitlab.example.com",
		source,
		platformgitlab.WithBaseURLForTesting(gitlabAPI.URL+"/api/v4"), platformgitlab.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := platform.NewRegistry(client)
	require.NoError(err)

	database := dbtest.Open(t)
	ref := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "group",
		Name:               "project",
		PlatformHost:       "gitlab.example.com",
		RepoPath:           "group/project",
		PlatformRepoID:     42,
		PlatformExternalID: "42",
		WebURL:             "https://gitlab.example.com/group/project",
		CloneURL:           "https://gitlab.example.com/group/project.git",
		DefaultBranch:      "main",
	}
	repoID, err := database.UpsertRepo(ctx, platformdb.DBRepoIdentity(platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         42,
		PlatformExternalID: "42",
		WebURL:             "https://gitlab.example.com/group/project",
		CloneURL:           "https://gitlab.example.com/group/project.git",
		DefaultBranch:      "main",
	}))
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{ref}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{TokenSources: sourceSet},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	firstResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/sync",
		nil,
	)
	defer firstResp.Body.Close()
	require.Equal(http.StatusOK, firstResp.StatusCode)
	firstCallCount := len(tokens)
	require.Positive(firstCallCount)
	for _, token := range tokens {
		assert.Equal("opaque-startup-token-11111", token)
	}

	writeTokenFileAtomically(t, tokenPath, "opaque-rotated-token-22222\n")
	secondResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/sync",
		nil,
	)
	defer secondResp.Body.Close()
	require.Equal(http.StatusOK, secondResp.StatusCode)
	require.Greater(len(tokens), firstCallCount)
	for _, token := range tokens[firstCallCount:] {
		assert.Equal("opaque-rotated-token-22222", token)
	}

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("GitLab token-file rotation", mr.Title)
	assert.Equal(
		"provider returned [REDACTED]",
		tokenauth.RedactKnownSecrets("provider returned opaque-rotated-token-22222"),
	)
}

func TestInvalidReloadKeepsLiveTokenSourceE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dir := t.TempDir()
	const liveTokenEnv = "KENN_FORGE_INVALID_RELOAD_E2E_REPO_TOKEN"
	const missingTokenEnv = "KENN_FORGE_INVALID_RELOAD_E2E_MISSING_REPO_TOKEN"
	t.Setenv(liveTokenEnv, "opaque-live-token-11111")
	t.Setenv(missingTokenEnv, "")

	var tokens []string
	gitlabAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Private-Token"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42":
			_, _ = fmt.Fprint(w, `{
				"id": 42,
				"path": "project",
				"path_with_namespace": "group/project",
				"web_url": "https://gitlab.example.com/group/project",
				"default_branch": "main"
			}`)
		case "/api/v4/projects/42/merge_requests/7":
			_, _ = fmt.Fprint(w, `{
				"id": 7001,
				"iid": 7,
				"project_id": 42,
				"title": "GitLab token reload fallback",
				"state": "opened",
				"web_url": "https://gitlab.example.com/group/project/-/merge_requests/7",
				"author": {"username": "ada", "name": "Ada Lovelace"},
				"source_branch": "feature/token-rotation",
				"target_branch": "main",
				"created_at": "2026-04-01T10:00:00Z",
				"updated_at": "2026-04-02T10:00:00Z"
			}`)
		case "/api/v4/projects/42/merge_requests/7/discussions",
			"/api/v4/projects/42/merge_requests/7/commits":
			_, _ = fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gitlabAPI.Close)

	cfgPath := filepath.Join(dir, "config.toml")
	initialConfig := gitLabTokenEnvConfig(liveTokenEnv)
	require.NoError(os.WriteFile(cfgPath, []byte(initialConfig), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet, source := collectStartupTokenSource(
		t, cfg, tokenauth.Key{Platform: string(platform.KindGitLab), Host: "gitlab.example.com"},
	)
	srv, httpServer, database, repoID := startGitLabTokenSyncServer(
		t, cfg, cfgPath, sourceSet, source, gitlabAPI.URL+"/api/v4",
	)
	stream := streamTokenRotationConfigEvents(t, srv, httpServer)
	defer stream.Close()

	firstResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/sync",
		nil,
	)
	defer firstResp.Body.Close()
	require.Equal(http.StatusOK, firstResp.StatusCode)
	firstCallCount := len(tokens)
	require.Positive(firstCallCount)
	for _, token := range tokens {
		assert.Equal("opaque-live-token-11111", token)
	}

	writeConfigTomlAtomically(
		t, cfgPath, gitLabTokenEnvConfig(missingTokenEnv),
	)
	ev := waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	assert.False(ev.Valid)
	assert.NotEmpty(ev.Error)

	secondResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/sync",
		nil,
	)
	defer secondResp.Body.Close()
	require.Equal(http.StatusOK, secondResp.StatusCode)
	require.Greater(len(tokens), firstCallCount)
	for _, token := range tokens[firstCallCount:] {
		assert.Equal("opaque-live-token-11111", token)
	}

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("GitLab token reload fallback", mr.Title)
}

// startMissingGitLabTokenServerE2E boots a server whose gitlab host is
// configured with a token file holding only whitespace — the state a
// token-file rotation passes through while the new credential is being
// written. Every provider call through the host's source fails with
// tokenauth.ErrMissingToken.
func startMissingGitLabTokenServerE2E(t *testing.T) *httptest.Server {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "missing-token")
	require.NoError(os.WriteFile(tokenPath, []byte("\n"), 0o600))

	gitlabAPI := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gitlabAPI.Close)

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, fmt.Appendf(nil, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com"
owner = "group"
name = "project"
repo_path = "group/project"
token_file = %q
`, tokenPath), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{})
	var source tokenauth.Source
	for _, plan := range cfg.ProviderTokenSources() {
		src := sourceSet.Upsert(plan.Descriptor)
		if plan.Descriptor.Key == (tokenauth.Key{
			Platform: string(platform.KindGitLab),
			Host:     "gitlab.example.com",
		}) {
			source = src
		}
	}
	require.NotNil(source)

	_, httpServer, _, _ := startGitLabTokenSyncServer(
		t, cfg, cfgPath, sourceSet, source, gitlabAPI.URL+"/api/v4",
	)
	return httpServer
}

func requireMissingTokenBadRequest(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&problem))
	assert.Equal(t, "badRequest", problem.Code)
}

func TestMissingRuntimeTokenSyncReturnsBadRequestE2E(t *testing.T) {
	httpServer := startMissingGitLabTokenServerE2E(t)

	resp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/sync",
		nil,
	)
	requireMissingTokenBadRequest(t, resp)
}

func TestMissingTokenRepoPreviewReturnsBadRequestE2E(t *testing.T) {
	httpServer := startMissingGitLabTokenServerE2E(t)

	resp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/repos/preview",
		map[string]string{
			"provider":      "gitlab",
			"platform_host": "gitlab.example.com",
			"owner":         "group",
			"pattern":       "*",
		},
	)
	requireMissingTokenBadRequest(t, resp)
}

func TestMissingTokenBulkAddReposReturnsBadRequestE2E(t *testing.T) {
	httpServer := startMissingGitLabTokenServerE2E(t)

	resp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/repos/bulk",
		map[string]any{
			"repos": []map[string]string{{
				"provider":      "gitlab",
				"platform_host": "gitlab.example.com",
				"owner":         "group",
				"name":          "other",
			}},
		},
	)
	requireMissingTokenBadRequest(t, resp)
}

func gitLabPlatformTokenConfig(tokenEnvLine string) string {
	return fmt.Sprintf(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[platforms]]
type = "gitlab"
host = "gitlab.example.com"
%s
`, tokenEnvLine)
}

func TestPlatformTokenRemovalAppliesWithoutRestartE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	t.Setenv("KENN_FORGE_E2E_PLATFORM_TOKEN", "opaque-platform-token-11111")

	var tokens []string
	gitlabAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Private-Token"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.EscapedPath() == "/api/v4/groups/group/projects" {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gitlabAPI.Close)

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(gitLabPlatformTokenConfig(
		`token_env = "KENN_FORGE_E2E_PLATFORM_TOKEN"`,
	)), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet, source := collectStartupTokenSource(
		t, cfg, tokenauth.Key{Platform: string(platform.KindGitLab), Host: "gitlab.example.com"},
	)
	srv, httpServer, _, _ := startGitLabTokenSyncServer(
		t, cfg, cfgPath, sourceSet, source, gitlabAPI.URL+"/api/v4",
	)
	stream := streamTokenRotationConfigEvents(t, srv, httpServer)
	defer stream.Close()

	previewBody := map[string]string{
		"provider":      "gitlab",
		"platform_host": "gitlab.example.com",
		"owner":         "group",
		"pattern":       "*",
	}
	firstResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/repos/preview", previewBody,
	)
	defer firstResp.Body.Close()
	require.Equal(http.StatusOK, firstResp.StatusCode)
	firstCallCount := len(tokens)
	require.Positive(firstCallCount)
	for _, token := range tokens {
		assert.Equal("opaque-platform-token-11111", token)
	}

	// Drop the platform's token_env. The reload must clear the live
	// source without demanding a restart, and the next provider call must
	// fail closed instead of reusing the removed credential.
	writeConfigTomlAtomically(t, cfgPath, gitLabPlatformTokenConfig(""))
	ev := waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	assert.True(ev.Valid, "reload error: %s", ev.Error)
	assert.False(ev.RestartRequired)

	secondResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/repos/preview", previewBody,
	)
	requireMissingTokenBadRequest(t, secondResp)
	assert.Len(tokens, firstCallCount,
		"no provider request may carry the removed credential")
}

func TestRuntimeLaunchStripsReloadedAndImplicitTokenEnvsE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "runtime-env.txt")
	workspacePath := filepath.Join(dir, "workspace")
	require.NoError(os.MkdirAll(workspacePath, 0o755))
	t.Setenv("KENN_FORGE_OLD_REPO_TOKEN", "old-secret")
	t.Setenv("KENN_FORGE_NEW_REPO_TOKEN", "new-secret")
	t.Setenv("KENN_FORGE_FORGEJO_TOKEN", "forgejo-secret")
	t.Setenv("KENN_FORGE_GITEA_TOKEN", "gitea-secret")
	t.Setenv("KENN_FORGE_VISIBLE_VALUE", "visible")

	cfgPath := filepath.Join(dir, "config.toml")
	initialConfig := runtimeStripConfig(outputPath, "KENN_FORGE_OLD_REPO_TOKEN")
	require.NoError(os.WriteFile(cfgPath, []byte(initialConfig), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{})
	for _, plan := range cfg.ProviderTokenSources() {
		sourceSet.Upsert(plan.Descriptor)
	}

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{
			WorktreeDir:                        filepath.Join(dir, "worktrees"),
			DisableWorkspaceBackgroundMonitors: true,
			PtyOwnerInProcess:                  true,
			TokenSources:                       sourceSet,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)
	seedReadyRuntimeWorkspace(t, database, workspacePath)

	stream := streamTokenRotationConfigEvents(t, srv, httpServer)
	defer stream.Close()
	writeConfigTomlAtomically(
		t, cfgPath, runtimeStripConfig(outputPath, "KENN_FORGE_NEW_REPO_TOKEN"),
	)
	ev := waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	require.True(ev.Valid, "reload error: %s", ev.Error)

	resp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/workspaces/ws-token-runtime/runtime/sessions",
		map[string]string{"target_key": "envdump"},
	)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode, string(responseBody))

	var data string
	require.Eventually(func() bool {
		raw, err := os.ReadFile(outputPath)
		if err != nil {
			return false
		}
		data = string(raw)
		return true
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal("unset\nunset\nunset\nunset\nvisible\n", data)
}

// equivalentCloneTokenChainReloadConfig points two providers at one
// self-hosted host. The forgejo repo's token_env repeats its platform fallback
// (env:SHARED -> env:SHARED) while gitlab resolves to a plain env:SHARED. Both
// name the same token, so the per-host clone-token reload check must compare
// canonical chains and keep the reload valid rather than flag a host conflict.
const equivalentCloneTokenChainReloadConfig = `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[platforms]]
type = "forgejo"
host = "code.example.com"
token_env = "SHARED"

[[platforms]]
type = "gitlab"
host = "code.example.com"
token_env = "SHARED"

[[repos]]
owner = "acme"
name = "widget"
platform = "forgejo"
platform_host = "code.example.com"
token_env = "SHARED"
`

func TestEquivalentCloneTokenChainsReloadStaysValidE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()

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

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockGH{}},
		database, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath, server.ServerOptions{},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	stream := streamTokenRotationConfigEvents(t, srv, httpServer)
	defer stream.Close()

	writeConfigTomlAtomically(t, cfgPath, equivalentCloneTokenChainReloadConfig)

	ev := waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	assert.True(ev.Valid, "reload error: %s", ev.Error)
	assert.Empty(ev.Error)
}

// sharedHostCloneConfig points forgejo and gitea at one self-hosted host,
// each with its own token line ("" for credential-less).
func sharedHostCloneConfig(forgejoTokenLine, giteaTokenLine string) string {
	return fmt.Sprintf(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[platforms]]
type = "forgejo"
host = "code.example.com"
%s

[[platforms]]
type = "gitea"
host = "code.example.com"
%s
`, forgejoTokenLine, giteaTokenLine)
}

func TestSharedHostCloneAuthFollowsSurvivingProviderTokenE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	t.Setenv("KENN_FORGE_E2E_SHARED_TOKEN", "opaque-shared-token-11111")
	t.Setenv("KENN_FORGE_E2E_ROTATED_TOKEN", "opaque-rotated-token-22222")

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(sharedHostCloneConfig(
		`token_env = "KENN_FORGE_E2E_SHARED_TOKEN"`,
		`token_env = "KENN_FORGE_E2E_SHARED_TOKEN"`,
	)), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet, forgejoSource := collectStartupTokenSource(
		t, cfg, tokenauth.Key{
			Platform: string(platform.KindForgejo), Host: "code.example.com",
		},
	)
	giteaSource, ok := sourceSet.Get(tokenauth.Key{
		Platform: string(platform.KindGitea), Host: "code.example.com",
	})
	require.True(ok)
	// No provider API call happens in this test; the stub base URL only
	// keeps client construction off the real network.
	forgeAPI := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(forgeAPI.Close)
	forgejoClient, err := platformforgejo.NewClient(
		"code.example.com", forgejoSource,
		platformforgejo.WithBaseURLForTesting(forgeAPI.URL), platformforgejo.
			WithTransport(http.DefaultTransport),
	)
	require.NoError(err)
	giteaClient, err := platformgitea.NewClient(
		"code.example.com", giteaSource,
		platformgitea.WithBaseURL(forgeAPI.URL, true),
		platformgitea.WithServerVersion("1.26.0"), platformgitea.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := platform.NewRegistry(forgejoClient, giteaClient)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{TokenSources: sourceSet},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	cloneSrc, ok := sourceSet.Get(tokenauth.CloneKey("code.example.com"))
	require.True(ok)
	bootToken, err := cloneSrc.Token(t.Context())
	require.NoError(err)
	require.Equal("opaque-shared-token-11111", bootToken)

	stream := streamTokenRotationConfigEvents(t, srv, httpServer)
	defer stream.Close()

	// The forgejo entry that may have supplied clone auth goes
	// credential-less while gitea rotates to a new env var. Clone auth
	// must hot-follow the host's surviving effective chain; both hosts
	// keep live provider clients, so no restart may be demanded.
	writeConfigTomlAtomically(t, cfgPath, sharedHostCloneConfig(
		"",
		`token_env = "KENN_FORGE_E2E_ROTATED_TOKEN"`,
	))
	ev := waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	assert.True(ev.Valid, "reload error: %s", ev.Error)
	assert.False(ev.RestartRequired)
	rotatedToken, err := cloneSrc.Token(t.Context())
	require.NoError(err)
	assert.Equal("opaque-rotated-token-22222", rotatedToken)

	// Every provider on the host goes credential-less: clone auth fails
	// closed instead of keeping a removed credential.
	writeConfigTomlAtomically(t, cfgPath, sharedHostCloneConfig("", ""))
	ev = waitForTokenRotationConfigEvent(t, stream, 3*time.Second)
	assert.True(ev.Valid, "reload error: %s", ev.Error)
	assert.False(ev.RestartRequired)
	_, err = cloneSrc.Token(t.Context())
	require.ErrorIs(err, tokenauth.ErrMissingToken)
}

func collectStartupTokenSource(
	t *testing.T,
	cfg *config.Config,
	key tokenauth.Key,
) (*tokenauth.SourceSet, tokenauth.Source) {
	t.Helper()
	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{})
	var out tokenauth.Source
	resolvedHosts := make(map[string]struct{})
	for _, plan := range cfg.ProviderTokenSources() {
		source := sourceSet.Upsert(plan.Descriptor)
		_, err := source.Token(t.Context())
		if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
			continue
		}
		require.NoError(t, err)
		resolvedHosts[plan.Descriptor.Key.Host] = struct{}{}
		if plan.Descriptor.Key == key {
			out = source
		}
	}
	// Mirror buildProviderStartup: hosts with a resolved provider source
	// also get the host-level clone source under tokenauth.CloneKey.
	for _, desc := range cfg.CloneTokenDescriptors() {
		if _, ok := resolvedHosts[desc.Key.Host]; !ok {
			continue
		}
		sourceSet.Upsert(desc)
	}
	require.NotNil(t, out)
	return sourceSet, out
}

func writeTokenFileAtomically(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func startGitLabTokenSyncServer(
	t *testing.T,
	cfg *config.Config,
	cfgPath string,
	sourceSet *tokenauth.SourceSet,
	source tokenauth.Source,
	gitlabBaseURL string,
) (*server.Server, *httptest.Server, *db.DB, int64) {
	t.Helper()
	client, err := platformgitlab.NewClient(
		"gitlab.example.com",
		source,
		platformgitlab.WithBaseURLForTesting(gitlabBaseURL), platformgitlab.WithTransport(http.DefaultTransport))
	require.NoError(t, err)
	registry, err := platform.NewRegistry(client)
	require.NoError(t, err)
	database := dbtest.Open(t)
	ref := gitLabTokenRepoRef()
	repoID, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         42,
		PlatformExternalID: "42",
		WebURL:             "https://gitlab.example.com/group/project",
		CloneURL:           "https://gitlab.example.com/group/project.git",
		DefaultBranch:      "main",
	}))
	require.NoError(t, err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{ref}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{TokenSources: sourceSet},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)
	return srv, httpServer, database, repoID
}

func gitLabTokenRepoRef() ghclient.RepoRef {
	return ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "group",
		Name:               "project",
		PlatformHost:       "gitlab.example.com",
		RepoPath:           "group/project",
		PlatformRepoID:     42,
		PlatformExternalID: "42",
		WebURL:             "https://gitlab.example.com/group/project",
		CloneURL:           "https://gitlab.example.com/group/project.git",
		DefaultBranch:      "main",
	}
}

func gitLabTokenEnvConfig(tokenEnv string) string {
	return fmt.Sprintf(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
platform = "gitlab"
platform_host = "gitlab.example.com"
owner = "group"
name = "project"
repo_path = "group/project"
token_env = %q
`, tokenEnv)
}

func runtimeStripConfig(outputPath, tokenEnv string) string {
	script := strings.Join([]string{
		`printf '%s\n%s\n%s\n%s\n%s\n'`,
		`"${KENN_FORGE_OLD_REPO_TOKEN-unset}"`,
		`"${KENN_FORGE_NEW_REPO_TOKEN-unset}"`,
		`"${KENN_FORGE_FORGEJO_TOKEN-unset}"`,
		`"${KENN_FORGE_GITEA_TOKEN-unset}"`,
		`"${KENN_FORGE_VISIBLE_VALUE-unset}"`,
		`> "$1"`,
	}, " ")
	return fmt.Sprintf(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[tmux]
command = ["/definitely/missing-kenn-forge-test-tmux"]

[[platforms]]
type = "forgejo"
host = "codeberg.org"

[[platforms]]
type = "gitea"
host = "gitea.com"

[[agents]]
key = "envdump"
label = "Env dump"
command = ["/bin/sh", "-c", %q, "envdump", %q]

[[repos]]
owner = "acme"
name = "widget"
token_env = %q
`, script, outputPath, tokenEnv)
}

func writeConfigTomlAtomically(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o644))
	require.NoError(t, os.Rename(tmp, path))
}

type tokenRotationConfigEvent struct {
	Valid           bool   `json:"valid"`
	Error           string `json:"error,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
}

type tokenRotationConfigEventStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	events chan tokenRotationConfigEvent
}

func (s *tokenRotationConfigEventStream) Close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

func streamTokenRotationConfigEvents(
	t *testing.T,
	srv *server.Server,
	httpServer *httptest.Server,
) *tokenRotationConfigEventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, httpServer.URL+"/api/v1/events", http.NoBody,
	)
	require.NoError(t, err)
	setAcceptedHostForE2ETest(req)
	resp, err := httpServer.Client().Do(req)
	require.NoError(t, err)
	stream := &tokenRotationConfigEventStream{
		resp:   resp,
		cancel: cancel,
		events: make(chan tokenRotationConfigEvent, 8),
	}
	waitForSubscribe(t, srv, 1)
	go func() {
		defer close(stream.events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
		for scanner.Scan() {
			frame := scanSSEFrame(t, scanner, scanner.Text())
			if frame.Event != "config.changed" {
				continue
			}
			var ev tokenRotationConfigEvent
			if err := json.Unmarshal([]byte(frame.Data), &ev); err != nil {
				continue
			}
			select {
			case stream.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return stream
}

func scanSSEFrame(t *testing.T, scanner *bufio.Scanner, firstLine string) sseFrame {
	t.Helper()
	var frame sseFrame
	consumeSSELine(&frame, firstLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return frame
		}
		consumeSSELine(&frame, line)
	}
	return frame
}

func consumeSSELine(frame *sseFrame, line string) {
	switch {
	case strings.HasPrefix(line, "id: "):
		frame.ID = strings.TrimPrefix(line, "id: ")
	case strings.HasPrefix(line, "event: "):
		frame.Event = strings.TrimPrefix(line, "event: ")
	case strings.HasPrefix(line, "data: "):
		frame.Data = strings.TrimPrefix(line, "data: ")
	}
}

func waitForTokenRotationConfigEvent(
	t *testing.T,
	stream *tokenRotationConfigEventStream,
	timeout time.Duration,
) tokenRotationConfigEvent {
	t.Helper()
	select {
	case ev, ok := <-stream.events:
		require.True(t, ok, "events channel closed before config.changed arrived")
		return ev
	case <-time.After(timeout):
		require.FailNow(t, "timed out waiting for config.changed event")
		return tokenRotationConfigEvent{}
	}
}

func seedReadyRuntimeWorkspace(t *testing.T, database *db.DB, worktreePath string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	runSettingsGit(t, worktreePath, "init", "--initial-branch=main")
	runSettingsGit(t, worktreePath, "config", "user.email", "test@example.test")
	runSettingsGit(t, worktreePath, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "README.md"), []byte("# Test\n"), 0o644))
	runSettingsGit(t, worktreePath, "add", "README.md")
	runSettingsGit(t, worktreePath, "commit", "-m", "initial")
	workspace := &db.Workspace{
		ID:              "ws-token-runtime",
		Platform:        string(platform.KindGitHub),
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      1,
		GitHeadRef:      "feature",
		WorkspaceBranch: "feature",
		WorktreePath:    worktreePath,
		Status:          "ready",
		CreatedAt:       now,
	}
	require.NoError(t, database.CreateWorkspaceWithLaunchSpec(
		t.Context(), workspace, db.WorkspaceLaunchSpec{
			Version: db.WorkspaceLaunchSpecVersion,
			Repository: db.WorkspaceLaunchRepository{
				Provider: "github", PlatformHost: "github.com",
				PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
				CloneURL: "https://github.com/acme/widget.git", DefaultBranch: "main",
			},
			ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1,
			ItemKey: "1", GitHeadRef: "feature",
			Pull: &db.WorkspaceLaunchPull{
				HeadBranch: "feature", HeadRepoKind: "same_repo", SnapshotRevision: 1,
			},
			SourceVisible: true, IssuedAt: now,
			SourceVisibleUntil: now.Add(db.WorkspaceLaunchSpecVisibilityLease),
		},
	))
}
