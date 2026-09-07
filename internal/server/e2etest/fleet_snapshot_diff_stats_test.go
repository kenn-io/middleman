package e2etest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/forge/internal/config"
	dbpkg "go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/fleet"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

// repoMetadataClient wraps the fixture client so GetRepository reports the
// provider metadata a real GitHub response carries. Local to this test so the
// shared fixture client's seeded e2e behavior is unchanged.
type repoMetadataClient struct {
	ghclient.Client
}

func (c repoMetadataClient) GetRepository(
	ctx context.Context, owner, repo string,
) (*gh.Repository, error) {
	r, err := c.Client.GetRepository(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	r.DefaultBranch = new("main")
	r.HTMLURL = new("https://github.com/" + owner + "/" + repo)
	r.CloneURL = new("https://github.com/" + owner + "/" + repo + ".git")
	return r, nil
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runner := gitcmd.New().WithConfig("init.defaultBranch", "main")
	out, stderr, err := runner.Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
}

// TestFleetSnapshotDetachedWorktreeDiffForSyncedRepoE2E covers the full chain
// behind the workspace sidebar's +/- chips: a GitHub sync with a pre-resolved
// repo identity persists the provider default branch, the worktree stats
// sampler resolves an orphan workspace's diff base from that synced row, and
// the snapshot API reports the diff even when HEAD is detached. A regression
// anywhere in that wiring either loses the sample on the expected unavailable-
// upstream result or measures committed work against the wrong base.
func TestFleetSnapshotDetachedWorktreeDiffForSyncedRepoE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	require := require.New(t)
	ctx := context.Background()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q")
	runGit(t, repoDir, "config", "user.email", "t@e.st")
	runGit(t, repoDir, "config", "user.name", "Tester")
	require.NoError(os.WriteFile(
		filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644,
	))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "base")

	featDir := filepath.Join(t.TempDir(), "feat")
	runGit(t, repoDir, "worktree", "add", "-b", "feature", featDir)
	require.NoError(os.WriteFile(
		filepath.Join(featDir, "feature.txt"), []byte("x\ny\n"), 0o644,
	))
	runGit(t, featDir, "add", ".")
	runGit(t, featDir, "commit", "-m", "feature work")
	runGit(t, featDir, "checkout", "--detach")

	database := dbtest.Open(t)
	// The pre-filled external id reproduces the modern resolution shape:
	// syncRepoIdentity short-circuits and the settings refresh is the only
	// path that can persist the default branch.
	repoRef := ghclient.RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "widgets",
		RepoPath:           "acme/widgets",
		PlatformExternalID: "repo-acme-widgets",
	}
	client := repoMetadataClient{Client: testutil.NewFixtureClient()}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": client}, database, nil,
		[]ghclient.RepoRef{repoRef}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	syncer.RunOnce(ctx)

	repoRow, err := database.GetRepoByIdentity(
		ctx, dbpkg.GitHubRepoIdentity("github.com", "acme", "widgets"),
	)
	require.NoError(err)
	require.NotNil(repoRow)
	require.Equal("main", repoRow.DefaultBranch,
		"sync must persist the provider default branch for a pre-resolved repo")

	// Orphan workspace: no registered project, so the sampler's diff base
	// comes from the synced repo row alone.
	require.NoError(database.InsertWorkspace(ctx, &dbpkg.Workspace{
		ID: "ws-diff", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widgets",
		ItemType: dbpkg.WorkspaceItemTypePullRequest, ItemNumber: 7,
		GitHeadRef: "feature", WorktreePath: featDir, Status: "ready",
	}))

	cfg := &config.Config{BasePath: "/"}
	cfg.Tmux.Command = []string{"kenn-forge-no-such-tmux"}
	srv := servertest.New(t, database, syncer, nil, "/", cfg, server.ServerOptions{
		WorktreeDir:                        t.TempDir(),
		DisableWorkspaceBackgroundMonitors: true,
		HostCheck: server.HostCheckOptions{
			Bind:                 config.HostKey{Host: "127.0.0.1", Port: "8091"},
			AllowLoopbackAnyPort: true,
		},
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	status, body := postJSON(
		t, ts.Client(), ts.URL+"/api/v1/snapshot/refresh-stats", nil,
	)
	require.Equal(http.StatusOK, status, "refresh-stats: %s", body)

	var snap fleet.Snapshot
	getJSON(t, ts, "/api/v1/snapshot", &snap)
	var wt *fleet.WorktreeSummary
	for i := range snap.Worktrees {
		if snap.Worktrees[i].LinkedPRNumber != nil &&
			*snap.Worktrees[i].LinkedPRNumber == 7 {
			wt = &snap.Worktrees[i]
			break
		}
	}
	require.NotNil(wt, "orphan workspace worktree present in snapshot")
	require.NotNil(wt.DiffAdded, "sampled worktree surfaces diff counts")
	require.Equal(2, *wt.DiffAdded,
		"diff must be measured against the synced default branch, not HEAD")
	require.NotNil(wt.DiffRemoved)
	require.Equal(0, *wt.DiffRemoved)
}
