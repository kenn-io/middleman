package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/testutil/gitfake"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/tokenauth"
)

// branchSyncTestManager builds a Manager with no clone manager configured, so
// PushWorktreeBranch and PullWorktreeBranch fall back to plain git against the
// worktree's existing remote. The networked-runner path is exercised
// separately by TestPushWorktreeBranchUsesAuthenticatedRunnerAndMutationAuth.
func branchSyncTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(nil, t.TempDir())
}

func TestPushWorktreeBranchPushesAheadCommitsAndRunsHooks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	work := gitfixture.DivergenceWorktree(t)
	marker := filepath.Join(filepath.Dir(work), "pre-push-ran")
	hook := filepath.Join(work, ".git", "hooks", "pre-push")
	require.NoError(os.WriteFile(
		hook,
		[]byte("#!/bin/sh\nprintf ran > "+marker+"\n"),
		0o755,
	))
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("ahead\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "ahead")

	require.NoError(branchSyncTestManager(t).PushWorktreeBranch(t.Context(), "", "github", "", "", "", work))

	div, ok, err := WorktreeDivergence(t.Context(), work)
	require.NoError(err)
	require.True(ok)
	assert.Equal(Divergence{}, div)
	assert.FileExists(marker)
}

func TestPushWorktreeBranchCreatesMissingRemoteBranch(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	remote := filepath.Join(filepath.Dir(work), "remote.git")
	runWorkspaceTestGit(t, filepath.Dir(work), "--git-dir", remote, "branch", "-D", "feature")
	runWorkspaceTestGit(t, work, "update-ref", "-d", "refs/remotes/origin/feature")
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("first fork commit\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "first fork commit")
	realGit, err := exec.LookPath("git")
	require.NoError(err)
	fakeDir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(fakeDir, "git"), []byte(`#!/bin/sh
set -eu
if [ "${1:-}" = "ls-remote" ]; then
	echo "remote: informational diagnostic" >&2
fi
exec "${KENN_FORGE_TEST_REAL_GIT:?}" "$@"
`), 0o755))
	t.Setenv("KENN_FORGE_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	require.NoError(branchSyncTestManager(t).PushWorktreeBranch(
		t.Context(), "", "github", "", "", "", work,
	))

	remoteCommit := strings.TrimSpace(string(runWorkspaceTestGit(
		t, filepath.Dir(work), "--git-dir", remote, "rev-parse", "refs/heads/feature",
	)))
	localCommit := strings.TrimSpace(string(runWorkspaceTestGit(t, work, "rev-parse", "HEAD")))
	require.Equal(localCommit, remoteCommit)
}

func TestPushWorktreeBranchRecreatesRemoteBranchWithStaleTrackingRef(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	remote := filepath.Join(filepath.Dir(work), "remote.git")
	runWorkspaceTestGit(t, filepath.Dir(work), "--git-dir", remote, "branch", "-D", "feature")
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("recreated branch\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "recreated branch")

	require.NoError(branchSyncTestManager(t).PushWorktreeBranch(
		t.Context(), "", "github", "", "", "", work,
	))

	remoteCommit := strings.TrimSpace(string(runWorkspaceTestGit(
		t, filepath.Dir(work), "--git-dir", remote, "rev-parse", "refs/heads/feature",
	)))
	localCommit := strings.TrimSpace(string(runWorkspaceTestGit(t, work, "rev-parse", "HEAD")))
	require.Equal(localCommit, remoteCommit)
}

func TestPushWorktreeBranchDoesNotOverwriteUntrackedRemoteBranch(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	require.NoError(os.WriteFile(filepath.Join(work, "f.txt"), []byte("local\n"), 0o644))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "local")

	other := filepath.Join(filepath.Dir(work), "other")
	remote := filepath.Join(filepath.Dir(work), "remote.git")
	runWorkspaceTestGit(t, filepath.Dir(work), "clone", remote, other)
	runWorkspaceTestGit(t, other, "config", "user.email", "o@test.com")
	runWorkspaceTestGit(t, other, "config", "user.name", "Other")
	runWorkspaceTestGit(t, other, "checkout", "-b", "feature", "origin/feature")
	require.NoError(os.WriteFile(filepath.Join(other, "f.txt"), []byte("remote\n"), 0o644))
	runWorkspaceTestGit(t, other, "add", ".")
	runWorkspaceTestGit(t, other, "commit", "-m", "remote")
	runWorkspaceTestGit(t, other, "push", "origin", "feature")
	remoteCommit := runWorkspaceTestGit(
		t, filepath.Dir(work), "--git-dir", remote, "rev-parse", "refs/heads/feature",
	)
	runWorkspaceTestGit(t, work, "update-ref", "-d", "refs/remotes/origin/feature")

	err := branchSyncTestManager(t).PushWorktreeBranch(
		t.Context(), "", "github", "", "", "", work,
	)

	require.Error(err)
	require.Equal(remoteCommit, runWorkspaceTestGit(
		t, filepath.Dir(work), "--git-dir", remote, "rev-parse", "refs/heads/feature",
	))
}

func TestPullWorktreeBranchFastForwardsBehindBranch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	work := gitfixture.DivergenceWorktree(t)
	other := filepath.Join(filepath.Dir(work), "other")
	remote := filepath.Join(filepath.Dir(work), "remote.git")
	runWorkspaceTestGit(t, filepath.Dir(work), "clone", remote, other)
	runWorkspaceTestGit(t, other, "config", "user.email", "o@test.com")
	runWorkspaceTestGit(t, other, "config", "user.name", "Other")
	runWorkspaceTestGit(t, other, "checkout", "-b", "feature", "origin/feature")
	require.NoError(os.WriteFile(
		filepath.Join(other, "f.txt"), []byte("remote-extra\n"), 0o644,
	))
	runWorkspaceTestGit(t, other, "add", ".")
	runWorkspaceTestGit(t, other, "commit", "-m", "remote extra")
	runWorkspaceTestGit(t, other, "push", "origin", "feature")

	require.NoError(branchSyncTestManager(t).PullWorktreeBranch(t.Context(), "", "github", "", "", "", work))

	contents, err := os.ReadFile(filepath.Join(work, "f.txt"))
	require.NoError(err)
	assert.Equal("remote-extra\n", string(contents))
	div, ok, err := WorktreeDivergence(t.Context(), work)
	require.NoError(err)
	require.True(ok)
	assert.Equal(Divergence{}, div)
}

func TestPushWorktreeBranchRejectsDivergedBranch(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("local\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "local")

	other := filepath.Join(filepath.Dir(work), "other")
	remote := filepath.Join(filepath.Dir(work), "remote.git")
	runWorkspaceTestGit(t, filepath.Dir(work), "clone", remote, other)
	runWorkspaceTestGit(t, other, "config", "user.email", "o@test.com")
	runWorkspaceTestGit(t, other, "config", "user.name", "Other")
	runWorkspaceTestGit(t, other, "checkout", "-b", "feature", "origin/feature")
	require.NoError(os.WriteFile(
		filepath.Join(other, "f.txt"), []byte("remote\n"), 0o644,
	))
	runWorkspaceTestGit(t, other, "add", ".")
	runWorkspaceTestGit(t, other, "commit", "-m", "remote")
	runWorkspaceTestGit(t, other, "push", "origin", "feature")

	err := branchSyncTestManager(t).PushWorktreeBranch(t.Context(), "", "github", "", "", "", work)

	require.Error(err)
	assert.New(t).ErrorIs(err, ErrWorktreeDiverged)
}

func TestPushWorktreeBranchRejectsNonOriginUpstream(t *testing.T) {
	work := gitfixture.DivergenceWorktree(t)
	runWorkspaceTestGit(t, work, "remote", "add", "other", "https://github.com/other/repo.git")
	runWorkspaceTestGit(t, work, "config", "branch.feature.remote", "other")

	err := branchSyncTestManager(t).PushWorktreeBranch(
		t.Context(), "", "github", "github.com", "acme", "widgets", work,
	)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrWorktreeNoUpstream)
	assert.ErrorContains(t, err, "unsupported remote other")
}

func TestPullWorktreeBranchRejectsDirtyWorktree(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	require.NoError(os.WriteFile(
		filepath.Join(work, "dirty.txt"), []byte("dirty\n"), 0o644,
	))

	err := branchSyncTestManager(t).PullWorktreeBranch(t.Context(), "", "github", "", "", "", work)

	require.Error(err)
	assert.New(t).ErrorIs(err, ErrWorktreeDirty)
}

// TestPushWorktreeBranchUsesAuthenticatedRunnerAndMutationAuth proves that
// when clone management is configured the networked branch-sync steps route
// through the host's authenticated git runner so a provider credential is
// always injected, and that the push specifically resolves the user's own PAT
// chain (mutation auth) while the fetches resolve the read-preferred GitHub App
// installation token. A fake git on PATH stands in for an authenticated
// remote: it rejects any push or fetch that arrives without a credential and
// records which token each networked operation carried.
func TestPushWorktreeBranchUsesAuthenticatedRunnerAndMutationAuth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	work := gitfixture.DivergenceWorktree(t)
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("ahead\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "ahead")

	realGit, err := exec.LookPath("git")
	require.NoError(err)
	fakeDir := t.TempDir()
	capturePath := filepath.Join(fakeDir, "credentials.txt")
	require.NoError(os.WriteFile(filepath.Join(fakeDir, "git"), []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
real="${KENN_FORGE_TEST_REAL_GIT:?}"
capture="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
op="${1:-}"
case "$op" in
push|fetch)
	helper=""
	i=0
	count="${GIT_CONFIG_COUNT:-0}"
	while [ "$i" -lt "$count" ]; do
		eval "key=\${GIT_CONFIG_KEY_$i:-}"
		eval "value=\${GIT_CONFIG_VALUE_$i:-}"
		if [ "$key" = "credential.helper" ]; then
			helper="$value"
		fi
		i=$((i + 1))
	done
	if [ -z "$helper" ]; then
		echo "fatal: Authentication failed: no credential helper" >&2
		exit 128
	fi
	password="$(run_credential_helper "$helper" get | sed -n 's/^password=//p')"
	if [ -z "$password" ]; then
		echo "fatal: Authentication failed: empty credential" >&2
		exit 128
	fi
	printf '%s:%s\n' "$op" "$password" >> "$capture"
	;;
esac
exec "$real" "$@"
`), 0o755))
	t.Setenv("KENN_FORGE_TEST_REAL_GIT", realGit)
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const patEnv = "KENN_FORGE_TEST_BRANCH_SYNC_PAT"
	t.Setenv(patEnv, "pat-token")
	source := tokenauth.NewManagedSource(
		tokenauth.Descriptor{
			Key: tokenauth.Key{Platform: "github", Host: "github.com"},
			Candidates: []tokenauth.Candidate{
				{
					Kind:           tokenauth.SourceKindGitHubApp,
					Host:           "github.com",
					AppID:          1,
					InstallationID: 123,
				},
				{Kind: tokenauth.SourceKindEnv, EnvName: patEnv},
			},
		},
		tokenauth.Options{
			GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
				return "app-token", time.Now().Add(time.Hour), nil
			},
		},
	)
	mgr := NewManager(nil, t.TempDir())
	mgr.SetClones(gitclone.New(
		t.TempDir(), gitclone.HostSources{"github.com": source},
	))

	require.NoError(mgr.PushWorktreeBranch(t.Context(), "", "github", "github.com", "acme", "widgets", work))

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	ops := strings.Split(strings.TrimSpace(string(data)), "\n")
	// The push must carry the user's PAT, never the app installation token,
	// so the pushed commits stay attributed to the user rather than the bot.
	assert.Contains(ops, "push:pat-token")
	assert.NotContains(ops, "push:app-token")
	// Reads (the upstream refresh fetches) prefer the app installation token.
	assert.Contains(ops, "fetch:app-token")
	assert.NotContains(ops, "fetch:pat-token")

	div, ok, err := WorktreeDivergence(t.Context(), work)
	require.NoError(err)
	require.True(ok)
	assert.Equal(Divergence{}, div)
}

func seedAmbiguousBranchSyncRoute(t *testing.T, d *db.DB) {
	t.Helper()
	now := time.Now().UTC()
	_, _, err := d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-old",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now)
	require.NoError(t, err)
	_, _, err = d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-new",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, now.Add(time.Hour))
	require.NoError(t, err)
}

func TestPushWorktreeBranchRejectsAmbiguousRoute(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	seedAmbiguousBranchSyncRoute(t, d)
	mgr := NewManager(d, t.TempDir())

	err := mgr.PushWorktreeBranch(
		t.Context(), "", "github", "github.com", "acme", "widget", t.TempDir(),
	)
	require.ErrorContains(err, "historical occupants",
		"push must fail closed on a route with contested history")
}

func TestPullWorktreeBranchRejectsAmbiguousRoute(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	seedAmbiguousBranchSyncRoute(t, d)
	mgr := NewManager(d, t.TempDir())

	err := mgr.PullWorktreeBranch(
		t.Context(), "", "github", "github.com", "acme", "widget", t.TempDir(),
	)
	require.ErrorContains(err, "historical occupants",
		"pull must fail closed on a route with contested history")
}

func TestLaunchSpecBranchSyncRefreshesExpiredLeaseBeforeGit(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	spec := launchSpecForTest()
	seedLaunchSpecRepository(t, database, spec)
	workspace := &db.Workspace{
		ID: "ws-expired-branch-sync", Platform: spec.Repository.Provider,
		PlatformHost: spec.Repository.PlatformHost,
		RepoOwner:    spec.Repository.Owner, RepoName: spec.Repository.Name,
		ItemType: spec.ItemType, ItemNumber: spec.ItemNumber,
		ItemKey: spec.ItemKey, GitHeadRef: spec.GitHeadRef,
		WorkspaceBranch: "kenn-forge/pr-7", WorktreePath: t.TempDir(),
		TmuxSession: "forge-ws-expired-branch-sync", Status: "ready",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.NoError(database.PutWorkspaceLaunchSpec(t.Context(), workspace.ID, spec))
	manager := NewManager(database, t.TempDir())
	manager.SetNow(func() time.Time { return spec.SourceVisibleUntil })
	manager.SetLaunchSpecResolver(unavailableLaunchSpecResolver{})

	err := manager.PushWorktreeBranch(
		t.Context(), workspace.ID, workspace.Platform, workspace.PlatformHost,
		workspace.RepoOwner, workspace.RepoName, workspace.WorktreePath,
	)

	require.ErrorIs(err, ErrLaunchSpecRefreshRequired)
	assert.True(t, LaunchSpecErrorRetryable(err))
}

func TestLaunchSpecBranchSyncUsesRefreshedRepositoryRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	work := gitfixture.DivergenceWorktree(t)
	database := openTestDB(t)
	original := launchSpecForTest()
	_, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: original.Repository.Provider, PlatformHost: original.Repository.PlatformHost,
			PlatformRepoID: original.Repository.PlatformRepoID,
			Owner:          original.Repository.Owner, Name: original.Repository.Name,
		}, original.IssuedAt,
	)
	require.NoError(err)
	require.True(accepted)
	workspace := &db.Workspace{
		ID: "ws-renamed-branch-sync", Platform: original.Repository.Provider,
		PlatformHost: original.Repository.PlatformHost,
		RepoOwner:    original.Repository.Owner, RepoName: original.Repository.Name,
		ItemType: original.ItemType, ItemNumber: original.ItemNumber,
		ItemKey: original.ItemKey, GitHeadRef: original.GitHeadRef,
		WorkspaceBranch: "feature", WorktreePath: work,
		TmuxSession: "forge-ws-renamed-branch-sync", Status: "ready",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.NoError(database.PutWorkspaceLaunchSpec(t.Context(), workspace.ID, original))

	renamed := original
	renamed.Repository.Owner = "acme-renamed"
	renamed.Repository.Name = "widget-renamed"
	renamed.Repository.CloneURL = "https://github.com/acme-renamed/widget-renamed.git"
	renamed.IssuedAt = original.SourceVisibleUntil
	renamed.SourceVisibleUntil = renamed.IssuedAt.Add(WorkspaceLaunchSpecVisibilityLease)
	_, accepted, err = database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: renamed.Repository.Provider, PlatformHost: renamed.Repository.PlatformHost,
			PlatformRepoID: renamed.Repository.PlatformRepoID,
			Owner:          renamed.Repository.Owner, Name: renamed.Repository.Name,
		}, renamed.IssuedAt,
	)
	require.NoError(err)
	require.True(accepted)
	manager := NewManager(database, t.TempDir())
	manager.SetNow(func() time.Time { return renamed.IssuedAt })
	manager.SetLaunchSpecResolver(&staticLaunchSpecResolver{spec: renamed})

	err = manager.PushWorktreeBranch(
		t.Context(), workspace.ID, workspace.Platform, workspace.PlatformHost,
		workspace.RepoOwner, workspace.RepoName, workspace.WorktreePath,
	)

	require.ErrorIs(err, ErrWorktreeInSync)
	assert.NotContains(err.Error(), "historical occupants")
	persisted, readErr := database.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(readErr)
	require.NotNil(persisted)
	assert.Equal(renamed.Repository.Owner, persisted.RepoOwner)
	assert.Equal(renamed.Repository.Name, persisted.RepoName)
}

func TestProviderBackedBranchSyncDoesNotFallBackToAnonymousGit(t *testing.T) {
	require := require.New(t)
	work := gitfixture.DivergenceWorktree(t)
	require.NoError(os.WriteFile(
		filepath.Join(work, "f.txt"), []byte("ahead\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "ahead")
	database := openTestDB(t)
	spec := launchSpecForTest()
	seedLaunchSpecRepository(t, database, spec)
	workspace := &db.Workspace{
		ID: "ws-required-branch-credential", Platform: spec.Repository.Provider,
		PlatformHost: spec.Repository.PlatformHost,
		RepoOwner:    spec.Repository.Owner, RepoName: spec.Repository.Name,
		ItemType: spec.ItemType, ItemNumber: spec.ItemNumber,
		ItemKey: spec.ItemKey, GitHeadRef: spec.GitHeadRef,
		WorkspaceBranch: "feature", WorktreePath: work,
		TmuxSession: "forge-ws-required-branch-credential", Status: "ready",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.NoError(database.PutWorkspaceLaunchSpec(t.Context(), workspace.ID, spec))
	manager := NewManager(database, t.TempDir())
	manager.SetNow(func() time.Time { return spec.IssuedAt })
	manager.SetClones(gitclone.New(t.TempDir(), nil))
	manager.SetRequireProviderCredential(true)

	err := manager.PushWorktreeBranch(
		t.Context(), workspace.ID, workspace.Platform, workspace.PlatformHost,
		workspace.RepoOwner, workspace.RepoName, workspace.WorktreePath,
	)

	require.ErrorIs(err, gitclone.ErrCredentialUnavailable)
}
