package e2etest

import (
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
	"sync"
	"sync/atomic"
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
)

// TestGitHubAppSplitAuthE2E pins the credential split through the
// full stack: a real HTTP server, SQLite, the production token
// collector reading a [[github_apps]] config entry, and the real
// GitHub client transports against a fake GitHub. Sync reads must
// carry the minted installation token while a user-facing mutation
// (posting a PR comment) must carry the user's PAT so GitHub
// attributes it to the user instead of the app bot.
func TestGitHubAppSplitAuthE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "user-pat-e2e")

	var mu sync.Mutex
	authByCall := map[string]string{}
	var userMergeSettingsComplete atomic.Bool
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		// Keep the first credential seen per call; retries must not
		// silently overwrite what the upstream actually received.
		if _, ok := authByCall[name]; !ok {
			authByCall[name] = r.Header.Get("Authorization")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/kenn-io/kenn-forge",
		func(w http.ResponseWriter, r *http.Request) {
			// The repo settings refresh fetches metadata with the app
			// token and overlays viewer permissions from the PAT; the
			// permissions block is viewer-specific (only the PAT can
			// push), so viewer_can_merge in the DB proves the overlay
			// happened.
			permissions := `"permissions": {"push": false}`
			mergeSettings := ""
			if r.Header.Get("Authorization") == "Bearer user-pat-e2e" {
				record("write:repo-viewer-overlay", r)
				permissions = `"permissions": {"push": true}`
				if userMergeSettingsComplete.Load() {
					mergeSettings = `,
					"allow_squash_merge": true,
					"allow_merge_commit": false,
					"allow_rebase_merge": false`
				}
			} else {
				record("read:repo-metadata", r)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"id": 4242001,
				"node_id": "R_kenn_forge",
				"name": "kenn-forge",
				"full_name": "kenn-io/kenn-forge",
				"owner": {"login": "kenn-io"},
				"default_branch": "main",
				"html_url": "https://github.com/kenn-io/kenn-forge",
				"clone_url": "https://github.com/kenn-io/forge.git",
				`+permissions+mergeSettings+`
			}`)
		})
	mux.HandleFunc("GET /api/v3/repos/kenn-io/kenn-forge/pulls",
		func(w http.ResponseWriter, r *http.Request) {
			record("read:list-pulls", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		})
	mux.HandleFunc("GET /api/v3/repos/kenn-io/kenn-forge/releases",
		func(w http.ResponseWriter, r *http.Request) {
			record("read:releases", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		})
	mux.HandleFunc("POST /api/v3/repos/kenn-io/kenn-forge/issues/7/comments",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:comment", r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{
				"id": 99001,
				"body": "from kenn-forge",
				"user": {"login": "mariusvniekerk"},
				"created_at": "2026-06-11T17:00:00Z",
				"html_url": "https://github.com/kenn-io/kenn-forge/pull/7#issuecomment-99001"
			}`)
		})
	mux.HandleFunc("PATCH /api/v3/repos/kenn-io/kenn-forge/issues/7",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:assignees", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"number": 7,
				"assignees": [{"login": "octocat"}]
			}`)
		})
	// The reviewer diff reads the current set with the read client and
	// then adds/removes with the write client; the fake reports one
	// pre-existing reviewer so a single set call exercises both paths.
	mux.HandleFunc("GET /api/v3/repos/kenn-io/kenn-forge/pulls/7",
		func(w http.ResponseWriter, r *http.Request) {
			record("read:pull", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"number": 7,
				"state": "open",
				"requested_reviewers": [{"login": "old-reviewer"}]
			}`)
		})
	mux.HandleFunc("POST /api/v3/repos/kenn-io/kenn-forge/pulls/7/requested_reviewers",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:reviewers-add", r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{
				"number": 7,
				"requested_reviewers": [{"login": "old-reviewer"}, {"login": "new-reviewer"}]
			}`)
		})
	mux.HandleFunc("DELETE /api/v3/repos/kenn-io/kenn-forge/pulls/7/requested_reviewers",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:reviewers-remove", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"number": 7,
				"requested_reviewers": [{"login": "new-reviewer"}]
			}`)
		})
	mux.HandleFunc("GET /api/v3/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":2000000000}}}`)
	})
	// Remaining sync reads (issues, labels, tags, ...) are empty lists.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record("read:other "+r.Method+" "+r.URL.Path, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	})
	fakeGitHub := httptest.NewServer(mux)
	t.Cleanup(fakeGitHub.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "kenn-io"
name = "kenn-forge"

[[github_apps]]
host = "github.com"
app_id = 4242
private_key_path = "app.pem"
installation_id = 11
installation_account = "kenn-io"
repository_selection = "all"
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	// Same SourceSet shape main() builds, with the minter stubbed: the
	// JWT-signing exchange itself is pinned by the cmd/kenn-forge
	// startup e2e; this test pins which credential each server code
	// path resolves.
	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_app_token_e2e", time.Now().Add(time.Hour), nil
		},
	})
	var source tokenauth.Source
	for _, plan := range cfg.ProviderTokenSources() {
		src := sourceSet.Upsert(plan.Descriptor)
		tokenCtx := t.Context()
		if plan.GitHubOwner != "" {
			tokenCtx = tokenauth.WithGitHubOwner(tokenCtx, plan.GitHubOwner)
		}
		if _, err := src.Token(tokenCtx); err != nil {
			if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
				continue
			}
			require.NoError(err)
		}
		if plan.Descriptor.Key == (tokenauth.Key{
			Platform: "github", Host: "github.com", Scope: "owner:kenn-io",
		}) {
			source = src
		}
	}
	require.NotNil(source)

	// Production always wires a quota registry, so the split-auth path must be
	// exercised with one: it is what attributes each wire attempt to the
	// credential that authorized it.
	quotaRegistry := ghclient.NewQuotaRegistry()
	appIdentity := ghclient.IdentityKey{Host: "github.com", Principal: "installation:11"}
	userIdentity := ghclient.IdentityKey{Host: "github.com", Principal: "user:4242"}
	client, err := ghclient.NewClient(
		source, "github.com", nil, nil,
		ghclient.WithBaseURLForTesting(fakeGitHub.URL),
		ghclient.WithQuotaAccounting(quotaRegistry, appIdentity, userIdentity),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(
		map[string]ghclient.Client{"github.com": client},
	)
	require.NoError(err)

	database := dbtest.Open(t)
	ref := ghclient.RepoRef{
		Platform:           platform.KindGitHub,
		Owner:              "kenn-io",
		Name:               "kenn-forge",
		RepoPath:           "kenn-io/kenn-forge",
		PlatformHost:       "github.com",
		PlatformRepoID:     4242001,
		PlatformExternalID: "R_kenn_forge",
		DefaultBranch:      "main",
	}
	repoID, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(platform.RepoRef{
		Platform:           platform.KindGitHub,
		Host:               "github.com",
		Owner:              "kenn-io",
		Name:               "kenn-forge",
		RepoPath:           "kenn-io/kenn-forge",
		PlatformID:         4242001,
		PlatformExternalID: "R_kenn_forge",
		DefaultBranch:      "main",
	}))
	require.NoError(err)
	// Reproduce a row damaged by an older App-only refresh. The first sync's
	// incomplete App and PAT payloads must retain these values; a later complete
	// PAT overlay must repair them through the same SQLite and HTTP API path.
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_repos
		SET allow_squash_merge = 0,
		    allow_merge_commit = 0,
		    allow_rebase_merge = 0
		WHERE id = ?`, repoID)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     7001,
		Number:         7,
		URL:            "https://github.com/kenn-io/kenn-forge/pull/7",
		Title:          "Split auth test PR",
		Author:         "ada",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{ref}, time.Minute, nil, nil,
	)
	syncer.SetQuotaRegistry(quotaRegistry)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{
			TokenSources: sourceSet,
			// These tests drive a real httptest server whose port is
			// ephemeral; relax loopback port matching like the shared
			// fixtures do.
			HostCheckAllowLoopbackAnyPort: true,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	// Read half: a full repo sync through the API must reach upstream
	// with the app installation token.
	status, body := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sync", nil)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "kenn-io", "kenn-forge", nil)

	assertRepoMergeSettings := func(squash, mergeCommit, rebase bool) {
		t.Helper()
		resp := doServerJSON(
			t, httpServer.Client(), http.MethodGet,
			httpServer.URL+"/api/v1/repo/github/kenn-io/kenn-forge", nil,
		)
		defer resp.Body.Close()
		require.Equal(http.StatusOK, resp.StatusCode)
		var repo struct {
			AllowSquashMerge bool
			AllowMergeCommit bool
			AllowRebaseMerge bool
		}
		require.NoError(json.NewDecoder(resp.Body).Decode(&repo))
		assert.Equal(squash, repo.AllowSquashMerge)
		assert.Equal(mergeCommit, repo.AllowMergeCommit)
		assert.Equal(rebase, repo.AllowRebaseMerge)
	}

	// Neither credential reported a complete field group, so omission retains
	// the stored all-false row rather than inventing an authoritative snapshot.
	assertRepoMergeSettings(false, false, false)

	beforeRepair, err := database.GetRepoByIdentity(
		t.Context(), db.GitHubRepoIdentity("github.com", "kenn-io", "kenn-forge"),
	)
	require.NoError(err)
	require.NotNil(beforeRepair.LastSyncCompletedAt)
	userMergeSettingsComplete.Store(true)
	status, body = postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sync", nil)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "kenn-io", "kenn-forge", beforeRepair.LastSyncCompletedAt)

	// A complete PAT overlay repairs SQLite, and the repository API exposes the
	// squash-only configuration consumed by the detail UI.
	assertRepoMergeSettings(true, false, false)

	// Write half: posting a PR comment through the API must reach
	// upstream with the user's PAT.
	commentResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/pulls/gh/kenn-io/kenn-forge/7/comments",
		map[string]string{"body": "from kenn-forge"},
	)
	defer commentResp.Body.Close()
	commentBody, err := io.ReadAll(commentResp.Body)
	require.NoError(err)
	require.Equal(http.StatusCreated, commentResp.StatusCode, string(commentBody))

	// Assignee mutations ride the same write credential: setting
	// assignees must reach upstream with the PAT, never the app token.
	assigneeResp := doServerJSON(
		t, httpServer.Client(), http.MethodPut,
		httpServer.URL+"/api/v1/pulls/gh/kenn-io/kenn-forge/7/assignees",
		map[string][]string{"assignees": {"octocat"}},
	)
	defer assigneeResp.Body.Close()
	assigneeBody, err := io.ReadAll(assigneeResp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, assigneeResp.StatusCode, string(assigneeBody))

	// Reviewer mutations diff against the provider's current set, so
	// one request exercises both the add (POST) and remove (DELETE)
	// write paths.
	reviewerResp := doServerJSON(
		t, httpServer.Client(), http.MethodPut,
		httpServer.URL+"/api/v1/pulls/gh/kenn-io/kenn-forge/7/reviewers",
		map[string][]string{"reviewers": {"new-reviewer"}},
	)
	defer reviewerResp.Body.Close()
	reviewerBody, err := io.ReadAll(reviewerResp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, reviewerResp.StatusCode, string(reviewerBody))

	// The repo settings refresh resolved with the PAT, so the stored
	// merge permission must reflect the user, not the read-only app.
	dbRepo, err := database.GetRepoByIdentity(t.Context(), db.GitHubRepoIdentity("github.com", "kenn-io", "kenn-forge"))
	require.NoError(err)
	assert.True(dbRepo.ViewerCanMerge,
		"viewer_can_merge must come from the PAT-visible permissions, not the app's")

	// The PAT is present, so mutation availability must not flag a
	// missing write credential.
	opsResp := doServerJSON(
		t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/repo/github/kenn-io/kenn-forge", nil,
	)
	defer opsResp.Body.Close()
	require.Equal(http.StatusOK, opsResp.StatusCode)
	ops := decodeRepoOperations(t, opsResp.Body)
	assert.True(ops["add_comment"].Available,
		"a split host with a PAT behind the app keeps writes available")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal("Bearer ghs_app_token_e2e", authByCall["read:list-pulls"],
		"sync reads must use the minted installation token")
	assert.Equal("Bearer user-pat-e2e", authByCall["write:comment"],
		"mutations must use the user's PAT for attribution")
	assert.Equal("Bearer user-pat-e2e", authByCall["write:assignees"],
		"assignee mutations must use the user's PAT, not the app token")
	assert.Equal("Bearer user-pat-e2e", authByCall["write:reviewers-add"],
		"reviewer requests must use the user's PAT, not the app token")
	assert.Equal("Bearer user-pat-e2e", authByCall["write:reviewers-remove"],
		"reviewer removals must use the user's PAT, not the app token")
	assert.Equal("Bearer user-pat-e2e", authByCall["write:repo-viewer-overlay"],
		"the viewer permission overlay must use the user's PAT")
	assert.Equal("Bearer ghs_app_token_e2e", authByCall["read:repo-metadata"],
		"repository metadata must stay on the app token")
	notificationCall := "read:other GET /api/v3/repos/kenn-io/kenn-forge/notifications"
	assert.Equal("Bearer user-pat-e2e", authByCall[notificationCall],
		"notification APIs are user-scoped and must use the user's PAT")
	for name, auth := range authByCall {
		if strings.HasPrefix(name, "write:") {
			continue
		}
		if name == notificationCall {
			continue
		}
		assert.Equal("Bearer ghs_app_token_e2e", auth, "call %s", name)
	}

	// The same traffic must land in two separate credential pools. This is the
	// wire-visible half of split auth: app-token reads and PAT writes share one
	// host, so a single merged pool would misreport both.
	poolsResp := doServerJSON(
		t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/rate-limits", nil,
	)
	defer poolsResp.Body.Close()
	require.Equal(http.StatusOK, poolsResp.StatusCode)
	var limits struct {
		ProviderPools map[string]struct {
			Provider      string `json:"provider"`
			PlatformHost  string `json:"platform_host"`
			RatePrincipal string `json:"rate_principal"`
			REST          struct {
				Requests int `json:"requests"`
			} `json:"rest"`
		} `json:"provider_pools"`
		LocalCeilings map[string]struct {
			Limit int `json:"limit"`
		} `json:"local_ceilings"`
	}
	require.NoError(json.NewDecoder(poolsResp.Body).Decode(&limits))
	appPool, ok := limits.ProviderPools["github:github.com:installation:11"]
	require.True(ok,
		"app installation pool must be reported separately: %+v", limits.ProviderPools)
	userPool, ok := limits.ProviderPools["github:github.com:user:4242"]
	require.True(ok,
		"user pool must be reported separately: %+v", limits.ProviderPools)
	assert.Positive(appPool.REST.Requests,
		"installation-token reads must be attributed to the app pool")
	assert.Positive(userPool.REST.Requests,
		"PAT writes and notification reads must be attributed to the user pool")
}

func TestGitHubAppGlobDiscoveryUsesInstallationRepositoriesE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "user-pat-e2e")

	var mu sync.Mutex
	authByCall := map[string]string{}
	var unexpected []string
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(name, "unexpected:") {
			unexpected = append(unexpected, r.Method+" "+r.URL.Path)
			return
		}
		if _, ok := authByCall[name]; !ok {
			authByCall[name] = r.Header.Get("Authorization")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		record("unexpected:user", r)
		http.Error(w, "installation token path must not identify a viewer", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v3/orgs/mariusvniekerk/repos", func(w http.ResponseWriter, r *http.Request) {
		record("unexpected:org-repos", r)
		http.Error(w, "installation token path must not probe org repositories", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v3/users/mariusvniekerk/repos", func(w http.ResponseWriter, r *http.Request) {
		record("unexpected:user-repos", r)
		http.Error(w, "installation token path must not fall back to public user repositories", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		record("read:installation-repos", r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"repositories": [
				{
					"id": 99001,
					"node_id": "R_private",
					"name": "private-repo",
					"full_name": "mariusvniekerk/private-repo",
					"private": true,
					"owner": {"login": "mariusvniekerk"},
					"default_branch": "main",
					"html_url": "https://github.com/mariusvniekerk/private-repo",
					"clone_url": "https://github.com/mariusvniekerk/private-repo.git"
				},
				{
					"id": 99002,
					"node_id": "R_org",
					"name": "private-repo",
					"full_name": "kenn-io/private-repo",
					"private": true,
					"owner": {"login": "kenn-io"},
					"default_branch": "main"
				}
			]
		}`)
	})
	mux.HandleFunc("GET /api/v3/repos/mariusvniekerk/private-repo",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer user-pat-e2e" {
				record("write:repo-viewer-overlay", r)
			} else {
				record("read:repo-metadata", r)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"id": 99001,
				"node_id": "R_private",
				"name": "private-repo",
				"full_name": "mariusvniekerk/private-repo",
				"private": true,
				"owner": {"login": "mariusvniekerk"},
				"default_branch": "main",
				"html_url": "https://github.com/mariusvniekerk/private-repo",
				"clone_url": "https://github.com/mariusvniekerk/private-repo.git",
				"permissions": {"push": true}
			}`)
		})
	mux.HandleFunc("GET /api/v3/repos/mariusvniekerk/private-repo/pulls",
		func(w http.ResponseWriter, r *http.Request) {
			record("read:list-pulls", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[{
				"id": 88001,
				"node_id": "PR_private",
				"number": 7,
				"title": "Private app-discovered PR",
				"state": "open",
				"html_url": "https://github.com/mariusvniekerk/private-repo/pull/7",
				"user": {"login": "ada"},
				"created_at": "2026-06-14T17:00:00Z",
				"updated_at": "2026-06-14T17:01:00Z",
				"head": {"ref": "feature", "sha": "abc123", "repo": {"full_name": "mariusvniekerk/private-repo"}},
				"base": {"ref": "main", "sha": "def456", "repo": {"full_name": "mariusvniekerk/private-repo"}}
			}]`)
		})
	mux.HandleFunc("GET /api/v3/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":2000000000}}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record("read:other "+r.Method+" "+r.URL.Path, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	})
	fakeGitHub := httptest.NewServer(mux)
	t.Cleanup(fakeGitHub.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "mariusvniekerk"
name = "private-*"

[[github_apps]]
host = "github.com"
app_id = 4242
private_key_path = "app.pem"
installation_id = 11
installation_account = "mariusvniekerk"
repository_selection = "all"
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_app_token_e2e", time.Now().Add(time.Hour), nil
		},
	})
	var source tokenauth.Source
	for _, plan := range cfg.ProviderTokenSources() {
		src := sourceSet.Upsert(plan.Descriptor)
		tokenCtx := t.Context()
		if plan.GitHubOwner != "" {
			tokenCtx = tokenauth.WithGitHubOwner(tokenCtx, plan.GitHubOwner)
		}
		if _, err := src.Token(tokenCtx); err != nil {
			if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
				continue
			}
			require.NoError(err)
		}
		if plan.Descriptor.Key == (tokenauth.Key{
			Platform: "github", Host: "github.com", Scope: "owner:mariusvniekerk",
		}) {
			source = src
		}
	}
	require.NotNil(source)

	client, err := ghclient.NewClient(
		source, "github.com", nil, nil,
		ghclient.WithBaseURLForTesting(fakeGitHub.URL),
	)
	require.NoError(err)
	clients := map[string]ghclient.Client{"github.com": client}
	resolved := ghclient.ResolveConfiguredRepos(t.Context(), clients, cfg.Repos)
	require.Empty(resolved.Warnings)
	require.Len(resolved.Expanded, 1)
	require.Equal("mariusvniekerk", resolved.Expanded[0].Owner)
	require.Equal("private-repo", resolved.Expanded[0].Name)

	registry, err := ghclient.NewProviderRegistry(clients)
	require.NoError(err)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, resolved.Expanded, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	status, body := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sync", nil)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "mariusvniekerk", "private-repo", nil)

	reposResp := doServerJSON(
		t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/repos", nil,
	)
	defer reposResp.Body.Close()
	require.Equal(http.StatusOK, reposResp.StatusCode)
	var repos []struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	require.NoError(json.NewDecoder(reposResp.Body).Decode(&repos))
	require.Len(repos, 1)
	assert.Equal("mariusvniekerk", repos[0].Owner)
	assert.Equal("private-repo", repos[0].Name)

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(unexpected)
	assert.Equal("Bearer ghs_app_token_e2e", authByCall["read:installation-repos"])
	assert.Equal("Bearer ghs_app_token_e2e", authByCall["read:list-pulls"])
}

// repoOperationWire is the client-observed availability shape for one
// operation in the repo response.
type repoOperationWire struct {
	Available         bool   `json:"available"`
	Code              string `json:"code"`
	UnavailableReason string `json:"unavailable_reason"`
}

func decodeRepoOperations(t *testing.T, body io.Reader) map[string]repoOperationWire {
	t.Helper()
	var resp struct {
		Operations map[string]repoOperationWire `json:"operations"`
	}
	require.NoError(t, json.NewDecoder(body).Decode(&resp))
	return resp.Operations
}

// TestGitHubAppNoUserCredentialGatesWritesE2E pins the failure mode
// the split design creates: a host can sync with only the GitHub App
// installed, but mutations skip the app candidate (writes stay
// attributed to the user), so with no PAT or gh credential behind the
// app every write must be reported unavailable up front and the
// mutation endpoint must refuse rather than write as the app bot.
func TestGitHubAppNoUserCredentialGatesWritesE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The configured PAT env var is present but empty: only the app
	// candidate can resolve a token.
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "")

	var writeMu sync.Mutex
	var upstreamWrites []string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/repos/kenn-io/kenn-forge/issues/7/comments",
		func(w http.ResponseWriter, r *http.Request) {
			writeMu.Lock()
			upstreamWrites = append(upstreamWrites, r.Method+" "+r.URL.Path)
			writeMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id": 99001, "body": "from kenn-forge"}`)
		})
	mux.HandleFunc("/api/v3/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"resources":{"core":{"limit":12500,"remaining":12000,"reset":2000000000}}}`)
	})
	mux.HandleFunc("GET /api/v3/repos/kenn-io/kenn-forge",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"id": 4242001,
				"node_id": "R_kenn_forge",
				"name": "kenn-forge",
				"full_name": "kenn-io/kenn-forge",
				"owner": {"login": "kenn-io"},
				"default_branch": "main",
				"html_url": "https://github.com/kenn-io/kenn-forge",
				"clone_url": "https://github.com/kenn-io/forge.git"
			}`)
		})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	})
	fakeGitHub := httptest.NewServer(mux)
	t.Cleanup(fakeGitHub.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "kenn-io"
name = "kenn-forge"

[[github_apps]]
host = "github.com"
app_id = 4242
private_key_path = "app.pem"
installation_id = 11
installation_account = "kenn-io"
repository_selection = "all"
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	sourceSet := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_app_only_e2e", time.Now().Add(time.Hour), nil
		},
	})
	var source tokenauth.Source
	for _, plan := range cfg.ProviderTokenSources() {
		src := sourceSet.Upsert(plan.Descriptor)
		tokenCtx := t.Context()
		if plan.GitHubOwner != "" {
			tokenCtx = tokenauth.WithGitHubOwner(tokenCtx, plan.GitHubOwner)
		}
		if _, err := src.Token(tokenCtx); err != nil {
			if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
				continue
			}
			require.NoError(err)
		}
		if plan.Descriptor.Key == (tokenauth.Key{
			Platform: "github",
			Host:     "github.com",
			Scope:    "owner:kenn-io",
		}) {
			source = src
		}
	}
	require.NotNil(source, "the app candidate alone must keep the owner's read chain alive")

	client, err := ghclient.NewClient(
		source, "github.com", nil, nil,
		ghclient.WithBaseURLForTesting(fakeGitHub.URL),
	)
	require.NoError(err)
	registry, err := ghclient.NewProviderRegistry(
		map[string]ghclient.Client{"github.com": client},
	)
	require.NoError(err)

	database := dbtest.Open(t)
	ref := ghclient.RepoRef{
		Platform:           platform.KindGitHub,
		Owner:              "kenn-io",
		Name:               "kenn-forge",
		RepoPath:           "kenn-io/kenn-forge",
		PlatformHost:       "github.com",
		PlatformRepoID:     4242001,
		PlatformExternalID: "R_kenn_forge",
		DefaultBranch:      "main",
	}
	repoID, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(platform.RepoRef{
		Platform:           platform.KindGitHub,
		Host:               "github.com",
		Owner:              "kenn-io",
		Name:               "kenn-forge",
		RepoPath:           "kenn-io/kenn-forge",
		PlatformID:         4242001,
		PlatformExternalID: "R_kenn_forge",
		DefaultBranch:      "main",
	}))
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     7001,
		Number:         7,
		URL:            "https://github.com/kenn-io/kenn-forge/pull/7",
		Title:          "App-only host PR",
		Author:         "ada",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{ref}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key:          ghclient.RouteKey{Host: "github.com", Owner: "kenn-io"},
		Client:       client,
		ReadIdentity: ghclient.IdentityKey{Host: "github.com", Principal: "installation:11"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{
			TokenSources: sourceSet,
			// These tests drive a real httptest server whose port is
			// ephemeral; relax loopback port matching like the shared
			// fixtures do.
			HostCheckAllowLoopbackAnyPort: true,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	// Reads still work: sync completes on the app token alone.
	status, body := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sync", nil)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "kenn-io", "kenn-forge", nil)

	// Availability must say up front that writes cannot authenticate.
	opsResp := doServerJSON(
		t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/v1/repo/github/kenn-io/kenn-forge", nil,
	)
	defer opsResp.Body.Close()
	require.Equal(http.StatusOK, opsResp.StatusCode)
	ops := decodeRepoOperations(t, opsResp.Body)
	for _, name := range []string{"merge_pr", "add_comment", "mark_ready_for_review"} {
		op, ok := ops[name]
		require.True(ok, "operation %s missing from repo response", name)
		assert.False(op.Available, "operation %s must gate without a write credential", name)
		assert.Equal("missing_write_credential", op.Code, "operation %s", name)
		assert.Contains(op.UnavailableReason, "github.com", "operation %s", name)
	}

	// The mutation endpoint must refuse instead of writing as the bot.
	commentResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/pulls/gh/kenn-io/kenn-forge/7/comments",
		map[string]string{"body": "from kenn-forge"},
	)
	defer commentResp.Body.Close()
	commentBody, err := io.ReadAll(commentResp.Body)
	require.NoError(err)
	assert.GreaterOrEqual(commentResp.StatusCode, http.StatusBadRequest,
		"mutation without a user credential must fail: %s", string(commentBody))

	// Refusing means refusing before the wire: no write request may
	// have reached GitHub, on any credential.
	writeMu.Lock()
	defer writeMu.Unlock()
	assert.Empty(upstreamWrites,
		"no write request may reach GitHub when the user credential is absent")
}
