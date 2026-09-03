package workspacetest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/testutil/servertest"
	"golang.org/x/sync/semaphore"
)

var parallelWorkspaceTestSlots = semaphore.NewWeighted(4)

func runParallelWorkspacePTYTest(t *testing.T) {
	t.Helper()
	t.Parallel()
	require.NoError(t, parallelWorkspaceTestSlots.Acquire(t.Context(), 1))
	t.Cleanup(func() { parallelWorkspaceTestSlots.Release(1) })
	acquireWorkspaceGitSlot(t)
}

type workspaceServerFixture struct {
	server           *server.Server
	client           *apiclient.Client
	database         *db.DB
	clones           *gitclone.Manager
	bare             string
	remote           string
	repoID           int64
	agentActivityDir string
	worktreeDir      string
}

func setupWorkspaceServerFixture(
	t *testing.T,
	cfg *config.Config,
	serverOptions ...server.ServerOptions,
) workspaceServerFixture {
	t.Helper()
	return setupWorkspaceServerFixtureWithTmuxInjection(
		t, cfg, true, serverOptions...,
	)
}

// setupWorkspaceServerFixtureUnconfiguredTmux leaves [tmux] command unset so
// the server resolves config.DefaultTmuxCommand. The caller must point
// TMUX_TMPDIR at a private directory first so the kenn-forge socket resolves
// inside the test sandbox instead of the developer's real socket directory.
func setupWorkspaceServerFixtureUnconfiguredTmux(
	t *testing.T,
	cfg *config.Config,
	serverOptions ...server.ServerOptions,
) workspaceServerFixture {
	t.Helper()
	return setupWorkspaceServerFixtureWithTmuxInjection(
		t, cfg, false, serverOptions...,
	)
}

func setupWorkspaceServerFixtureWithTmuxInjection(
	t *testing.T,
	cfg *config.Config,
	injectTestTmux bool,
	serverOptions ...server.ServerOptions,
) workspaceServerFixture {
	t.Helper()

	if testing.Short() {
		t.Skip("workspace e2e tests skipped in short mode")
	}
	if cfg == nil {
		cfg = &config.Config{}
	} else {
		clone := *cfg
		cfg = &clone
	}
	if injectTestTmux && len(cfg.Tmux.Command) == 0 {
		cfg.Tmux.Command = append([]string(nil), workspaceTestTmuxCommand...)
	}

	dir := t.TempDir()
	database := dbtest.Open(t)

	remoteDir := filepath.Join(dir, "remote")
	require.NoError(t, os.MkdirAll(remoteDir, 0o755))
	remote := filepath.Join(remoteDir, "widget.git")
	gitfixture.Run(t, dir, "init", "--bare", "--initial-branch=main", remote)

	tmpWork := filepath.Join(dir, "work")
	gitfixture.Run(t, dir, "clone", remote, tmpWork)
	gitfixture.Run(t, tmpWork, "config", "user.email", "test@test.com")
	gitfixture.Run(t, tmpWork, "config", "user.name", "Test")

	require.NoError(t, os.WriteFile(
		filepath.Join(tmpWork, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	gitfixture.Run(t, tmpWork, "add", ".")
	gitfixture.Run(t, tmpWork, "commit", "-m", "base commit")
	gitfixture.Run(t, tmpWork, "push", "origin", "main")

	gitfixture.Run(t, tmpWork, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpWork, "new.txt"),
		[]byte("new\n"), 0o644,
	))
	gitfixture.Run(t, tmpWork, "add", ".")
	gitfixture.Run(t, tmpWork, "commit", "-m", "feature commit")
	gitfixture.Run(t, tmpWork, "push", "origin", "feature")

	bareDir := filepath.Join(dir, "clones")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	clones := gitclone.New(bareDir, nil)
	bare, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(t, err)
	gitfixture.Run(t, dir, "clone", "--bare", remote, bare)
	gitfixture.Run(
		t, bare, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	gitfixture.Run(
		t, bare, "config", "--add",
		"url."+remote+".insteadOf", "https://github.com/acme/widget.git",
	)

	worktreeDir := filepath.Join(dir, "worktrees")
	repos := []ghclient.RepoRef{
		{Owner: "acme", Name: "widget", PlatformHost: "github.com"},
	}
	syncer := ghclient.NewSyncer(nil, database, nil, repos, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	basePath := "/"
	if cfg != nil && cfg.BasePath != "" {
		basePath = cfg.BasePath
	}
	repoID := seedPROnHost(t, database, "github.com", "acme", "widget", 1)
	options := server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		PtyOwnerInProcess:                  true,
	}
	if len(serverOptions) > 0 {
		options = serverOptions[0]
	}
	options.Clones = clones
	options.WorktreeDir = worktreeDir
	options.HostCheck = server.HostCheckOptions{
		Bind:    config.HostKey{Host: "127.0.0.1", Port: "8091"},
		Allowed: []config.HostKey{{Host: "forge.test", Port: ""}},
	}
	srv := servertest.New(t, database, syncer, nil, basePath, cfg, options)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})

	clientBaseURL := "http://forge.test"
	if basePath != "/" {
		clientBaseURL += strings.TrimSuffix(basePath, "/")
	}
	client := setupTestClientWithBaseURL(t, srv, clientBaseURL)
	return workspaceServerFixture{
		server:           srv,
		client:           client,
		database:         database,
		clones:           clones,
		bare:             bare,
		remote:           remote,
		repoID:           repoID,
		agentActivityDir: filepath.Join(dir, "agent-activity"),
		worktreeDir:      worktreeDir,
	}
}

func setupTestClientWithBaseURL(
	t *testing.T,
	srv *server.Server,
	baseURL string,
) *apiclient.Client {
	t.Helper()

	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body io.Reader = http.NoBody
			if req.Body != nil {
				payload, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				_ = req.Body.Close()
				body = strings.NewReader(string(payload))
			}

			serverReq := httptest.NewRequest(req.Method, req.URL.String(), body)
			serverReq.Header = req.Header.Clone()
			if req.Method != http.MethodGet && serverReq.Header.Get("Content-Type") == "" {
				serverReq.Header.Set("Content-Type", "application/json")
			}
			serverReq = serverReq.WithContext(req.Context())

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, serverReq)
			return rr.Result(), nil
		}),
	}

	client, err := apiclient.NewWithHTTPClient(baseURL, httpClient)
	require.NoError(t, err)
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func seedPROnHost(
	t *testing.T, database *db.DB,
	host, owner, name string, number int,
) int64 {
	t.Helper()
	// Same-repo head evidence: without it the workspace classifies as
	// unknown provenance and setup requires refs/pull/<n>/head from the
	// fixture remote.
	return seedPRWithHeadRepo(
		t, database, host, owner, name, number,
		fmt.Sprintf("https://%s/%s/%s.git", host, owner, name),
	)
}

// seedPRWithoutHeadRepo seeds a merge request whose head-repo identity is
// unknown. Workspace setup for such a row must fail closed before Git access.
func seedPRWithoutHeadRepo(
	t *testing.T, database *db.DB,
	host, owner, name string, number int,
) int64 {
	t.Helper()
	return seedPRWithHeadRepo(t, database, host, owner, name, number, "")
}

func seedPRWithHeadRepo(
	t *testing.T, database *db.DB,
	host, owner, name string, number int,
	headRepoCloneURL string,
) int64 {
	t.Helper()
	ctx := t.Context()

	repoID, err := database.UpsertRepo(ctx, verifiedGitHubRepoIdentity(host, owner, name))
	require.NoError(t, err)
	require.NoError(t, database.UpdateRepoProviderMetadata(ctx, repoID, db.RepoProviderMetadata{
		PlatformRepoID: "repo-" + owner + "-" + name,
		WebURL:         fmt.Sprintf("https://%s/%s/%s", host, owner, name),
		CloneURL:       fmt.Sprintf("https://%s/%s/%s.git", host, owner, name),
		DefaultBranch:  "main",
	}))

	now := time.Now().UTC().Truncate(time.Second)
	pr := &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       int64(number) * 1000,
		Number:           number,
		URL:              fmt.Sprintf("https://%s/%s/%s/pull/%d", host, owner, name, number),
		Title:            fmt.Sprintf("Test PR #%d", number),
		Author:           "testuser",
		State:            "open",
		IsDraft:          false,
		Body:             "test body",
		HeadBranch:       "feature",
		HeadRepoCloneURL: headRepoCloneURL,
		BaseBranch:       "main",
		Additions:        5,
		Deletions:        2,
		CommentCount:     0,
		ReviewDecision:   "",
		CIStatus:         "",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}

	prID, err := database.UpsertMergeRequest(ctx, pr)
	require.NoError(t, err)
	require.NoError(t, database.EnsureKanbanState(ctx, prID))

	return prID
}

func seedIssue(
	t *testing.T, database *db.DB,
	owner, name string, number int, state string,
) int64 {
	t.Helper()
	ctx := t.Context()

	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", owner, name),
	)
	require.NoError(t, err)
	require.NoError(t, database.UpdateRepoProviderMetadata(ctx, repoID, db.RepoProviderMetadata{
		PlatformRepoID: "repo-" + owner + "-" + name,
		WebURL:         fmt.Sprintf("https://github.com/%s/%s", owner, name),
		CloneURL:       fmt.Sprintf("https://github.com/%s/%s.git", owner, name),
		DefaultBranch:  "main",
	}))

	now := time.Now().UTC().Truncate(time.Second)
	issue := &db.Issue{
		RepoID:         repoID,
		PlatformID:     int64(number) * 1000,
		Number:         number,
		URL:            fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, name, number),
		Title:          fmt.Sprintf("Test Issue #%d", number),
		Author:         "testuser",
		State:          state,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}
	if state == "closed" {
		issue.ClosedAt = &now
	}
	issueID, err := database.UpsertIssue(ctx, issue)
	require.NoError(t, err)
	return issueID
}

func verifiedGitHubRepoIdentity(host, owner, name string) db.RepoIdentity {
	identity := db.GitHubRepoIdentity(host, owner, name)
	identity.PlatformRepoID = "repo-" + owner + "-" + name
	return identity
}

func createReadyWorkspace(
	t *testing.T,
	ctx context.Context,
	client *apiclient.Client,
) *generated.WorkspaceResponse {
	t.Helper()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, createResp.StatusCode())
	require.NotNil(t, createResp.JSON202)
	return waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)
}

func waitForWorkspaceReady(
	t *testing.T,
	ctx context.Context,
	client *apiclient.Client,
	wsID string,
) *generated.WorkspaceResponse {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		getResp, err := client.HTTP.GetWorkspaceWithResponse(waitCtx, wsID)
		require.NoError(t, err, "polling workspace readiness: %s", wsID)
		if getResp.StatusCode() == http.StatusOK &&
			getResp.JSON200 != nil &&
			getResp.JSON200.Status == "ready" {
			return getResp.JSON200
		}

		select {
		case <-waitCtx.Done():
			require.NoError(t, waitCtx.Err(), "workspace never became ready: %s", wsID)
		case <-ticker.C:
		}
	}
}

func assertWorkspaceRuntimeTarget(
	t *testing.T,
	targets []generated.LaunchTarget,
	key string,
) {
	t.Helper()

	for _, target := range targets {
		if target.Key == key {
			return
		}
	}
	require.Failf(t, "runtime target not found", "key %q", key)
}

func assertWorkspaceRuntimeTargetAbsent(
	t *testing.T,
	targets []generated.LaunchTarget,
	key string,
) {
	t.Helper()

	for _, target := range targets {
		if target.Key == key {
			require.Failf(t, "runtime target should be hidden", "key %q", key)
		}
	}
}
