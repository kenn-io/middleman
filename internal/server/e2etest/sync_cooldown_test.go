package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func TestTriggerSyncE2EBypassesCooldown(t *testing.T) {
	require := require.New(t)

	var syncCalls atomic.Int32
	secondSync := make(chan struct{})
	var secondSyncClosed atomic.Bool
	mock := &mockGH{
		listOpenPullRequestsFn: func(
			_ context.Context, _, _ string,
		) ([]*gh.PullRequest, error) {
			if syncCalls.Add(1) == 2 && secondSyncClosed.CompareAndSwap(false, true) {
				close(secondSync)
			}
			return nil, nil
		},
	}
	baseURL, client, _, syncer := startSyncCooldownE2EServerWithSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)

	syncer.RunOnce(t.Context())
	require.Equal(int32(1), syncCalls.Load())

	status, body := postJSON(
		t, client, baseURL+"/api/v1/sync", nil,
	)
	require.Equal(http.StatusAccepted, status, body)

	select {
	case <-secondSync:
	case <-time.After(10 * time.Second):
		require.Fail("second explicit sync did not bypass cooldown")
	}
}

func TestTriggerSyncE2ERefreshesSnapshotBeforeRateBackoff(t *testing.T) {
	require := require.New(t)

	synced := make(chan struct{})
	var syncedClosed atomic.Bool
	mock := &mockGH{
		getRateLimitSnapshotFn: func(context.Context) (*platformgithub.RateLimitSnapshot, error) {
			resetAt := time.Now().UTC().Add(time.Hour)
			return &platformgithub.RateLimitSnapshot{
				Core: &ghclient.Rate{
					Limit:     5000,
					Remaining: 4800,
					Reset:     resetAt,
				},
			}, nil
		},
		listOpenPullRequestsFn: func(
			_ context.Context, _, _ string,
		) ([]*gh.PullRequest, error) {
			if syncedClosed.CompareAndSwap(false, true) {
				close(synced)
			}
			return nil, nil
		},
	}
	baseURL, client, _, syncer := startSyncCooldownE2EServerWithSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, mock)
	rt := syncer.RateTrackers()["github.com"]
	require.NotNil(rt)
	rt.UpdateFromRate(ghclient.Rate{
		Limit:     5000,
		Remaining: 0,
		Reset:     time.Now().UTC().Add(time.Hour),
	})

	status, body := postJSON(
		t, client, baseURL+"/api/v1/sync", nil,
	)
	require.Equal(http.StatusAccepted, status, body)

	select {
	case <-synced:
	case <-time.After(2 * time.Second):
		require.Fail("explicit sync did not refresh snapshot before rate backoff")
	}
}

func TestTriggerSyncE2EPrioritizesFilteredRepos(t *testing.T) {
	require := require.New(t)

	var mu sync.Mutex
	var calls []string
	done := make(chan struct{})
	var doneClosed atomic.Bool
	mock := &mockGH{
		listOpenPullRequestsFn: func(
			_ context.Context, owner, repo string,
		) ([]*gh.PullRequest, error) {
			mu.Lock()
			calls = append(calls, owner+"/"+repo)
			callCount := len(calls)
			mu.Unlock()
			if callCount == 3 && doneClosed.CompareAndSwap(false, true) {
				close(done)
			}
			return nil, nil
		},
	}
	baseURL, client, _, syncer := startSyncCooldownE2EServerWithSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "first"

[[repos]]
owner = "acme"
name = "second"

[[repos]]
owner = "acme"
name = "third"
`, mock)
	syncer.SetParallelism(1)

	status, body := postJSON(
		t, client,
		baseURL+"/api/v1/sync?priority_repo=github|github.com/acme/third,github|github.com/acme/second",
		nil,
	)
	require.Equal(http.StatusAccepted, status, body)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail("explicit sync did not process all repos")
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	require.Equal([]string{
		"acme/third",
		"acme/second",
		"acme/first",
	}, got)
}

func TestTriggerSyncE2EPrioritizesNonDefaultHostFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("sync prioritization e2e runs in the full Go test lane")
	}
	require := require.New(t)

	var mu sync.Mutex
	var calls []string
	done := make(chan struct{})
	var doneClosed atomic.Bool
	mock := &mockGH{
		listOpenPullRequestsFn: func(
			_ context.Context, owner, repo string,
		) ([]*gh.PullRequest, error) {
			mu.Lock()
			calls = append(calls, owner+"/"+repo)
			callCount := len(calls)
			mu.Unlock()
			if callCount == 3 && doneClosed.CompareAndSwap(false, true) {
				close(done)
			}
			return nil, nil
		},
	}
	baseURL, client, _, syncer := startSyncCooldownE2EServerWithSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "first"

[[repos]]
owner = "acme"
name = "second"
platform_host = "gitea"

[[repos]]
owner = "acme"
name = "third"
platform_host = "gitea"
`, mock)
	syncer.SetParallelism(1)

	status, body := postJSON(
		t, client,
		baseURL+"/api/v1/sync?priority_repo=github|gitea/acme/second",
		nil,
	)
	require.Equal(http.StatusAccepted, status, body)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail("explicit sync did not process all repos")
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	require.Equal([]string{
		"acme/second",
		"acme/first",
		"acme/third",
	}, got)
}

func TestTriggerSyncE2EPrioritizesProviderQualifiedFilter(t *testing.T) {
	require := require.New(t)

	var mu sync.Mutex
	var calls []string
	done := make(chan struct{})
	var doneClosed atomic.Bool
	mock := &mockGH{
		listOpenPullRequestsFn: func(
			_ context.Context, owner, repo string,
		) ([]*gh.PullRequest, error) {
			mu.Lock()
			calls = append(calls, owner+"/"+repo)
			callCount := len(calls)
			mu.Unlock()
			if callCount == 2 && doneClosed.CompareAndSwap(false, true) {
				close(done)
			}
			return nil, nil
		},
	}
	baseURL, client, _, syncer := startSyncCooldownE2EServerWithSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "third"
`, mock)
	syncer.SetParallelism(1)
	syncer.SetRepos([]ghclient.RepoRef{
		{
			Platform:     platform.KindGitHub,
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "third",
			RepoPath:     "acme/third",
		},
		{
			Platform:     platform.KindGitea,
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			RepoPath:     "acme/widget",
		},
		{
			Platform:     platform.KindGitHub,
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			RepoPath:     "acme/widget",
		},
	})

	status, body := postJSON(
		t, client,
		baseURL+"/api/v1/sync?priority_repo=github|github.com/acme/widget",
		nil,
	)
	require.Equal(http.StatusAccepted, status, body)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail("explicit sync did not process all repos")
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	require.Equal([]string{
		"acme/widget",
		"acme/third",
	}, got)
}

func TestAddRepoE2ETriggersImmediateSyncDuringCooldown(t *testing.T) {
	require := require.New(t)

	baseURL, client, database := startSyncCooldownE2EServer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"
`, &mockGH{})

	status, body := postJSON(
		t, client, baseURL+"/api/v1/sync", nil,
	)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "acme", "widget", nil)
	waitForSyncIdle(t, client, baseURL)

	status, body = postJSON(t, client, baseURL+"/api/v1/repos", map[string]string{
		"provider": "github",
		"host":     "github.com",
		"owner":    "other-org",
		"name":     "other-repo",
	})
	require.Equal(http.StatusCreated, status, body)

	added := waitForRepoSynced(
		t, database, "other-org", "other-repo", nil,
	)
	require.NotNil(added.LastSyncCompletedAt)
}

func TestRefreshRepoE2ETriggersImmediateSyncDuringCooldown(t *testing.T) {
	require := require.New(t)

	var includeRefreshRepo atomic.Bool
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			kennForgeID := "repo-" + owner + "-kenn-forge"
			repos := []*gh.Repository{{
				Name:     new("kenn-forge"),
				NodeID:   &kennForgeID,
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}}
			if includeRefreshRepo.Load() {
				reviewBotID := "repo-" + owner + "-review-bot"
				repos = append(repos, &gh.Repository{
					Name:     new("review-bot"),
					NodeID:   &reviewBotID,
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				})
			}
			return repos, nil
		},
	}

	baseURL, client, database := startSyncCooldownE2EServer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "roborev-dev"
name = "*"
`, mock)

	status, body := postJSON(
		t, client, baseURL+"/api/v1/sync", nil,
	)
	require.Equal(http.StatusAccepted, status, body)
	waitForRepoSynced(t, database, "roborev-dev", "kenn-forge", nil)
	waitForSyncIdle(t, client, baseURL)

	includeRefreshRepo.Store(true)

	status, body = postJSON(
		t, client,
		baseURL+"/api/v1/repo/gh/roborev-dev/%2A/refresh",
		nil,
	)
	require.Equal(http.StatusOK, status, body)

	refreshed := waitForRepoSynced(
		t, database, "roborev-dev", "review-bot", nil,
	)
	require.NotNil(refreshed.LastSyncCompletedAt)
}

func startSyncCooldownE2EServer(
	t *testing.T,
	cfgContent string,
	mock *mockGH,
) (string, *http.Client, *db.DB) {
	baseURL, client, database, _ := startSyncCooldownE2EServerWithSyncer(
		t, cfgContent, mock,
	)
	return baseURL, client, database
}

func startSyncCooldownE2EServerWithSyncer(
	t *testing.T,
	cfgContent string,
	mock *mockGH,
) (string, *http.Client, *db.DB, *ghclient.Syncer) {
	t.Helper()
	require := require.New(t)

	dir := t.TempDir()
	database := dbtest.Open(t)

	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	clients := map[string]ghclient.Client{"github.com": mock}
	for _, repo := range cfg.Repos {
		clients[repo.PlatformHostOrDefault()] = mock
	}
	resolved := ghclient.ResolveConfiguredRepos(
		t.Context(), clients, cfg.Repos,
	)
	trackers := make(map[string]*ghclient.RateTracker, len(clients))
	for host := range clients {
		trackers[host] = ghclient.NewRateTracker(
			database, host, "host", "rest",
		)
	}
	syncer := ghclient.NewSyncer(
		clients, database, nil, resolved.Expanded,
		time.Minute, trackers, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	baseURL := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	require.Eventually(func() bool {
		resp, err := client.Get(baseURL + "/api/v1/version")
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond)

	t.Cleanup(func() {
		gracefulShutdown(t, srv)
		select {
		case err := <-serveErr:
			require.ErrorIs(err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			require.FailNow("server did not stop")
		}
	})

	return baseURL, client, database, syncer
}

func postJSON(
	t *testing.T,
	client *http.Client,
	url string,
	body any,
) (int, string) {
	t.Helper()
	require := require.New(t)

	var payload io.Reader = http.NoBody
	if body != nil {
		buf, err := json.Marshal(body)
		require.NoError(err)
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(http.MethodPost, url, payload)
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(err)
	return resp.StatusCode, string(respBody)
}

func waitForRepoSynced(
	t *testing.T,
	database *db.DB,
	owner, name string,
	after *time.Time,
) *db.Repo {
	t.Helper()
	require := require.New(t)

	var repo *db.Repo
	require.Eventually(func() bool {
		got, err := database.GetRepoByIdentity(
			t.Context(), db.GitHubRepoIdentity("github.com", owner, name),
		)
		if err != nil || got == nil || got.LastSyncCompletedAt == nil {
			return false
		}
		if after != nil &&
			!got.LastSyncCompletedAt.After(*after) {
			return false
		}
		repo = got
		return true
	}, 5*time.Second, 10*time.Millisecond)

	return repo
}

func waitForSyncIdle(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	require := require.New(t)

	var status struct {
		Running   bool       `json:"running"`
		LastRunAt *time.Time `json:"last_run_at"`
	}
	require.Eventually(func() bool {
		resp, err := client.Get(baseURL + "/api/v1/sync/status")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			return false
		}
		status = struct {
			Running   bool       `json:"running"`
			LastRunAt *time.Time `json:"last_run_at"`
		}{}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return false
		}
		return !status.Running && status.LastRunAt != nil
	}, 5*time.Second, 10*time.Millisecond)
}
