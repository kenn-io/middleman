package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
)

func TestRetryPreservesWorkspaceCommitsAndUntrackedFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	mgr := newTestManager(t, database, t.TempDir())
	mgr.SetClones(gitclone.New(t.TempDir(), nil))
	clone, err := mgr.clones.ClonePath("github", "github.com", "acme", "widget")
	require.NoError(err)
	seedWorkspaceBareCloneAt(t, clone)
	path := filepath.Join(t.TempDir(), "workspace")
	branch := "feature/saved-work"
	runWorkspaceTestGit(t, clone, "worktree", "add", path, "-b", branch, "HEAD")
	runWorkspaceTestGit(t, path, "commit", "--allow-empty", "-m", "saved work")
	before, ok, err := gitRefSHA(ctx, path, "HEAD")
	require.NoError(err)
	require.True(ok)
	require.NoError(os.WriteFile(filepath.Join(path, "notes.txt"), []byte("unfinished work"), 0o600))
	ws := &Workspace{ID: "saved-workspace", Platform: "github", PlatformHost: "github.com", RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeIssue, ItemNumber: 7, GitHeadRef: branch, WorkspaceBranch: branch, WorktreePath: path, Status: "error"}
	require.NoError(database.InsertWorkspace(ctx, ws))
	require.NoError(writeWorkspaceOwnershipMarker(ctx, clone, ws))
	require.NoError(database.UpsertWorkspaceRuntimeSession(ctx, &db.WorkspaceRuntimeSession{
		WorkspaceID: ws.ID, SessionKey: "saved-agent", TargetKey: "codex", Kind: "agent", Scope: "session", TmuxSession: "saved-tmux",
	}))
	retried, started, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.True(started)
	assert.Equal(branch, retried.WorkspaceBranch)
	sessions, err := database.ListWorkspaceRuntimeSessions(ctx, ws.ID)
	require.NoError(err)
	require.Len(sessions, 1)
	assert.Equal("saved-agent", sessions[0].SessionKey)
	assert.FileExists(filepath.Join(path, "notes.txt"))
	after, ok, err := gitRefSHA(ctx, clone, "refs/heads/"+branch)
	require.NoError(err)
	assert.True(ok)
	assert.Equal(before, after)
}

func TestIssueRecoveryChecksOutExistingBranchWithSavedCommits(t *testing.T) {
	require := require.New(t)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	clone := filepath.Join(t.TempDir(), "clone.git")
	seedWorkspaceBareCloneAt(t, clone)
	configureOriginHeadForIssueWorkspace(t, clone)
	branch := "feature/saved-work"
	old := filepath.Join(t.TempDir(), "old")
	runWorkspaceTestGit(t, clone, "worktree", "add", old, "-b", branch, "HEAD")
	runWorkspaceTestGit(t, old, "commit", "--allow-empty", "-m", "saved work")
	before, _, err := gitRefSHA(t.Context(), old, "HEAD")
	require.NoError(err)
	runWorkspaceTestGit(t, clone, "worktree", "remove", old)
	ws := &Workspace{ID: "recover-branch", ItemType: db.WorkspaceItemTypeIssue, ItemNumber: 7, GitHeadRef: branch, WorkspaceBranch: branch, WorktreePath: filepath.Join(t.TempDir(), "restored")}
	_, err = mgr.addIssueWorktree(t.Context(), workspaceGitDir{path: clone, remote: originRemoteName}, ws)
	require.NoError(err)
	after, _, err := gitRefSHA(t.Context(), ws.WorktreePath, "HEAD")
	require.NoError(err)
	assert.Equal(t, before, after)
}
