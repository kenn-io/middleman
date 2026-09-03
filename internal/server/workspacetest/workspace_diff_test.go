package workspacetest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace"
)

var workspaceGitSlots = semaphore.NewWeighted(8)

func runParallelWorkspaceGitTest(t *testing.T) {
	t.Helper()
	t.Parallel()
	acquireWorkspaceGitSlot(t)
}

func acquireWorkspaceGitSlot(t *testing.T) {
	t.Helper()
	require.NoError(t, workspaceGitSlots.Acquire(t.Context(), 1))
	t.Cleanup(func() { workspaceGitSlots.Release(1) })
}

func TestWorkspaceDiffEndpointsReportHeadAndPushedE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")

	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "committed.go"),
		[]byte("package committed\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local workspace commit")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "dirty.go"),
		[]byte("package dirty\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, ".workspace-state.json"),
		[]byte("{}\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "z-blank.txt"),
		[]byte(" \t\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "z-empty.txt"),
		nil,
		0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "base.txt"),
		[]byte("base  \n"), 0o644,
	))

	headFiles := requestWorkspaceFiles(t, srv, ws.Id, "head")
	require.NotNil(headFiles.Files)
	assertWorkspaceDiffPaths(
		t,
		headFiles.Files,
		[]string{
			".workspace-state.json",
			"base.txt",
			"dirty.go",
			"z-blank.txt",
			"z-empty.txt",
		},
	)

	headFilesHideWhitespace := requestWorkspaceFiles(
		t, srv, ws.Id, "head", "hide",
	)
	require.NotNil(headFilesHideWhitespace.Files)
	assertWorkspaceDiffPaths(
		t,
		headFilesHideWhitespace.Files,
		[]string{".workspace-state.json", "dirty.go", "z-empty.txt"},
	)

	headDiffHideWhitespace := requestWorkspaceDiff(
		t, srv, ws.Id, "head", "hide",
	)
	require.NotNil(headDiffHideWhitespace.Files)
	assertWorkspaceDiffPaths(
		t,
		headDiffHideWhitespace.Files,
		[]string{".workspace-state.json", "dirty.go", "z-empty.txt"},
	)

	pushedDiff := requestWorkspaceDiff(t, srv, ws.Id, "pushed")
	require.NotNil(pushedDiff.Files)
	assertWorkspaceDiffPaths(
		t,
		pushedDiff.Files,
		[]string{
			".workspace-state.json",
			"base.txt",
			"committed.go",
			"dirty.go",
			"z-blank.txt",
			"z-empty.txt",
		},
	)
	assert.Equal(int64(1), pushedDiff.WhitespaceOnlyCount)
}

func TestWorkspaceFilePreviewEndpointReturnsRequestedDiffSideContentE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")

	path := "preview.go"
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, path),
		[]byte("package preview\n\nfunc value() string {\n\treturn \"base\"\n}\n"),
		0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "add preview fixture")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, path),
		[]byte("package preview\n\nfunc value() string {\n\treturn \"worktree\"\n}\n"),
		0o644,
	))

	oldPreview := requestWorkspaceFilePreview(t, srv, ws.Id, "head", path, "old")
	oldDecoded, err := base64.StdEncoding.DecodeString(oldPreview.Content)
	require.NoError(err)
	newPreview := requestWorkspaceFilePreview(t, srv, ws.Id, "head", path, "new")
	newDecoded, err := base64.StdEncoding.DecodeString(newPreview.Content)
	require.NoError(err)

	assert.Equal(path, oldPreview.Path)
	assert.Equal(path, newPreview.Path)
	assert.Contains(string(oldDecoded), `return "base"`)
	assert.NotContains(string(oldDecoded), `return "worktree"`)
	assert.Contains(string(newDecoded), `return "worktree"`)
	assert.NotContains(string(newDecoded), `return "base"`)
}

func TestWorkspaceDiffEndpointsReturnPierreTreeOrderE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	dir := t.TempDir()
	worktreePath := filepath.Join(dir, "worktree")
	gitfixture.Run(t, dir, "init", "--initial-branch=main", worktreePath)
	gitfixture.Run(t, worktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, worktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "base.txt"),
		[]byte("base\n"),
		0o644,
	))
	gitfixture.Run(t, worktreePath, "add", ".")
	gitfixture.Run(t, worktreePath, "commit", "-m", "base commit")

	database := dbtest.Open(t)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	_, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(err)
	srv := server.New(database, nil, nil, "/", nil, server.ServerOptions{
		WorktreeDir: filepath.Join(dir, "managed-worktrees"),
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	ctx := context.Background()
	require.NoError(database.InsertWorkspace(ctx, &workspace.Workspace{
		ID:              "ws-file-order",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      1,
		GitHeadRef:      "feature/file-order",
		WorkspaceBranch: "kenn-forge/pr-1",
		WorktreePath:    worktreePath,
		TmuxSession:     "kenn-forge-file-order",
		Status:          "ready",
	}))

	serverDir := filepath.Join(worktreePath, "internal", "server")
	require.NoError(os.MkdirAll(
		filepath.Join(serverDir, "e2etest"),
		0o755,
	))
	for _, path := range []string{
		filepath.Join(serverDir, "config_reload_test.go"),
		filepath.Join(serverDir, "e2etest", "settings_test.go"),
		filepath.Join(serverDir, "config_reload.go"),
		filepath.Join(serverDir, "api_types.go"),
	} {
		require.NoError(os.WriteFile(path, []byte("package server\n"), 0o644))
	}

	want := []string{
		"internal/server/e2etest/settings_test.go",
		"internal/server/api_types.go",
		"internal/server/config_reload.go",
		"internal/server/config_reload_test.go",
	}

	files := requestWorkspaceFiles(t, srv, "ws-file-order", "head")
	require.NotNil(files.Files)
	assertWorkspaceDiffPaths(t, files.Files, want)

	diff := requestWorkspaceDiff(t, srv, "ws-file-order", "head")
	require.NotNil(diff.Files)
	assertWorkspaceDiffPaths(t, diff.Files, want)
}

func TestWorkspaceCommitsEndpointListsBranchCommitsE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")

	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "local-one.go"),
		[]byte("package one\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local one")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "local-two.go"),
		[]byte("package two\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local two")

	commits := requestWorkspaceCommits(t, srv, ws.Id)
	require.NotNil(commits.Commits)
	require.Len(commits.Commits, 3)
	assert.Equal("local two", commits.Commits[0].Message)
	assert.Equal("local one", commits.Commits[1].Message)
	assert.Equal("feature commit", commits.Commits[2].Message)
}

func TestWorkspaceDiffEndpointsAcceptCommitAndRangeScopesE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")

	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "local-one.go"),
		[]byte("package one\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local one")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "local-two.go"),
		[]byte("package two\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local two")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "dirty.go"),
		[]byte("package dirty\n"), 0o644,
	))

	commits := requestWorkspaceCommits(t, srv, ws.Id)
	require.NotNil(commits.Commits)
	require.Len(commits.Commits, 3)
	newest := commits.Commits[0].Sha
	older := commits.Commits[1].Sha

	singleFiles := requestWorkspaceFilesQuery(
		t, srv, ws.Id, "base=head&commit="+url.QueryEscape(newest),
	)
	require.NotNil(singleFiles.Files)
	assertWorkspaceDiffPaths(t, singleFiles.Files, []string{"local-two.go"})

	singleDiff := requestWorkspaceDiffQuery(
		t, srv, ws.Id, "base=head&commit="+url.QueryEscape(newest),
	)
	require.NotNil(singleDiff.Files)
	assertWorkspaceDiffPaths(t, singleDiff.Files, []string{"local-two.go"})

	rangeFiles := requestWorkspaceFilesQuery(
		t,
		srv,
		ws.Id,
		"base=head&from="+url.QueryEscape(older)+"&to="+url.QueryEscape(newest),
	)
	require.NotNil(rangeFiles.Files)
	assertWorkspaceDiffPaths(
		t,
		rangeFiles.Files,
		[]string{"local-one.go", "local-two.go"},
	)

	rangeDiff := requestWorkspaceDiffQuery(
		t,
		srv,
		ws.Id,
		"base=head&from="+url.QueryEscape(older)+"&to="+url.QueryEscape(newest),
	)
	require.NotNil(rangeDiff.Files)
	assertWorkspaceDiffPaths(
		t,
		rangeDiff.Files,
		[]string{"local-one.go", "local-two.go"},
	)
}

func TestWorkspaceDiffEndpointReportsMergeTargetE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, remote, srv := fixture.client, fixture.remote, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	targetWork := filepath.Join(t.TempDir(), "target")
	gitfixture.Run(t, filepath.Dir(targetWork), "clone", remote, targetWork)
	gitfixture.Run(t, targetWork, "config", "user.email", "test@test.com")
	gitfixture.Run(t, targetWork, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(targetWork, "target-only.txt"),
		[]byte("target\n"), 0o644,
	))
	gitfixture.Run(t, targetWork, "add", ".")
	gitfixture.Run(t, targetWork, "commit", "-m", "advance main")
	gitfixture.Run(t, targetWork, "push", "origin", "main")
	gitfixture.Run(t, ws.WorktreePath, "fetch", "origin", "main")

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "committed.go"),
		[]byte("package committed\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local workspace commit")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "dirty.go"),
		[]byte("package dirty\n"), 0o644,
	))

	mergeTargetFiles := requestWorkspaceFiles(t, srv, ws.Id, "merge-target")
	require.NotNil(mergeTargetFiles.Files)
	filePaths := workspaceDiffPaths(mergeTargetFiles.Files)
	assert.Contains(filePaths, "new.txt")
	assert.Contains(filePaths, "committed.go")
	assert.Contains(filePaths, "dirty.go")
	assert.NotContains(filePaths, "target-only.txt")

	mergeTargetDiff := requestWorkspaceDiff(t, srv, ws.Id, "merge-target")
	require.NotNil(mergeTargetDiff.Files)
	diffPaths := workspaceDiffPaths(mergeTargetDiff.Files)
	assert.Contains(diffPaths, "new.txt")
	assert.Contains(diffPaths, "committed.go")
	assert.Contains(diffPaths, "dirty.go")
	assert.NotContains(diffPaths, "target-only.txt")
}

func TestWorkspaceDiffEndpointReportsMergeTargetForAssociatedKataWorkspaceE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, database, remote, srv := fixture.client, fixture.database, fixture.remote, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	targetWork := filepath.Join(t.TempDir(), "target")
	gitfixture.Run(t, filepath.Dir(targetWork), "clone", remote, targetWork)
	gitfixture.Run(t, targetWork, "config", "user.email", "test@test.com")
	gitfixture.Run(t, targetWork, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(targetWork, "target-only.txt"),
		[]byte("target\n"), 0o644,
	))
	gitfixture.Run(t, targetWork, "add", ".")
	gitfixture.Run(t, targetWork, "commit", "-m", "advance main")
	gitfixture.Run(t, targetWork, "push", "origin", "main")
	gitfixture.Run(t, ws.WorktreePath, "fetch", "origin", "main")

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "kata-local.go"),
		[]byte("package kata\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "kata workspace commit")

	associatedPRNumber := 1
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID:                 "ws-kata-merge-target",
		Platform:           "github",
		PlatformHost:       "github.com",
		RepoOwner:          "acme",
		RepoName:           "widget",
		ItemType:           db.WorkspaceItemTypeKataTask,
		ItemKey:            "kata:desktop:project-kata:issue-kata-1",
		AssociatedPRNumber: &associatedPRNumber,
		GitHeadRef:         ws.GitHeadRef,
		WorkspaceBranch:    ws.GitHeadRef,
		WorktreePath:       ws.WorktreePath,
		TmuxSession:        "kenn-forge-ws-kata-merge-target",
		Status:             "ready",
		KataMetadata: &db.WorkspaceKataMetadata{
			DaemonID:   "desktop",
			ProjectUID: "project-kata",
			IssueUID:   "issue-kata-1",
			ShortID:    "task-123",
			Title:      "Fix widget",
		},
	}))

	mergeTargetFiles := requestWorkspaceFiles(t, srv, "ws-kata-merge-target", "merge-target")
	require.NotNil(mergeTargetFiles.Files)
	filePaths := workspaceDiffPaths(mergeTargetFiles.Files)
	assert.Contains(filePaths, "kata-local.go")
	assert.NotContains(filePaths, "target-only.txt")
}

func TestWorkspaceDiffEndpointRejectsOriginBaseE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	req := newWorkspaceFixtureRequest(
		http.MethodGet,
		"/api/v1/workspaces/"+ws.Id+"/diff?base=origin",
		nil,
	)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()

	require.Equal(http.StatusBadRequest, resp.StatusCode)

	var body httpapi.ProblemError
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	require.Contains(body.Detail, "base must be head, pushed, or merge-target")
}

func TestWorkspaceDiffEndpointHandlesUntrackedSymlinkAndLargeFileE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "secret.txt")
	require.NoError(os.WriteFile(secretPath, []byte("do not expose\n"), 0o644))
	require.NoError(os.Symlink(
		secretPath,
		filepath.Join(ws.WorktreePath, "secret-link"),
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "large.txt"),
		bytes.Repeat([]byte("x"), 2<<20),
		0o644,
	))

	diff := requestWorkspaceDiff(t, srv, ws.Id, "head")
	require.NotNil(diff.Files)

	symlink := testutil.RequireWorkspaceDiffFile(t, diff.Files, "secret-link")
	assert.Equal("added", symlink.Status)
	assert.False(symlink.IsBinary)
	assert.Equal(int64(1), symlink.Additions)
	require.NotNil(symlink.Hunks)
	require.Len(symlink.Hunks, 1)
	require.NotNil(symlink.Hunks[0].Lines)
	require.Len(symlink.Hunks[0].Lines, 1)
	line := (symlink.Hunks[0].Lines)[0]
	assert.Equal(secretPath, line.Content)
	assert.NotContains(line.Content, "do not expose")

	large := testutil.RequireWorkspaceDiffFile(t, diff.Files, "large.txt")
	assert.Equal("added", large.Status)
	assert.True(large.IsBinary)
	assert.Zero(large.Additions)
	require.NotNil(large.Hunks)
	assert.Empty(large.Hunks)
}

func TestWorkspaceDiffEndpointMarksGeneratedFilesE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	client, srv := fixture.client, fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, ".gitattributes"),
		[]byte("dist/** linguist-generated\nbun.lock -linguist-generated\n"), 0o644,
	))
	require.NoError(os.MkdirAll(filepath.Join(ws.WorktreePath, "dist"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "dist", "api.ts"),
		[]byte("export const generated = true;\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "bun.lock"),
		[]byte("# lock\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "src.ts"),
		[]byte("export const source = true;\n"), 0o644,
	))

	files := requestWorkspaceFiles(t, srv, ws.Id, "head")
	require.NotNil(files.Files)
	assert.True(testutil.RequireWorkspaceDiffFile(t, files.Files, "dist/api.ts").IsGenerated)
	assert.False(testutil.RequireWorkspaceDiffFile(t, files.Files, "bun.lock").IsGenerated)
	assert.False(testutil.RequireWorkspaceDiffFile(t, files.Files, "src.ts").IsGenerated)

	diff := requestWorkspaceDiff(t, srv, ws.Id, "head")
	require.NotNil(diff.Files)
	assert.True(testutil.RequireWorkspaceDiffFile(t, diff.Files, "dist/api.ts").IsGenerated)
	assert.False(testutil.RequireWorkspaceDiffFile(t, diff.Files, "bun.lock").IsGenerated)
	assert.False(testutil.RequireWorkspaceDiffFile(t, diff.Files, "src.ts").IsGenerated)
}

func TestWorkspaceDiffEndpointScopesPatchByPathE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ws := createReadyWorkspace(t, context.Background(), fixture.client)

	require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, "first.go"), []byte("package first\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, "second.go"), []byte("package second\n"), 0o644))

	diff := requestWorkspaceDiffForPath(t, fixture.server, ws.Id, "head", "first.go")
	require.NotNil(diff.Files)
	require.Len(diff.Files, 1)
	file := diff.Files[0]
	assert.Equal("first.go", file.Path)
	assert.Equal("added", file.Status)
	assert.Contains(file.Patch, "diff --git a/first.go b/first.go\n")
	assert.Contains(file.Patch, "new file mode 100644\n")
	require.NotNil(file.Hunks)
	require.Len(file.Hunks, 1)
	assert.NotContains(workspaceDiffPaths(diff.Files), "second.go")
}

func TestWorkspaceDiffPathPrefersCurrentPathOverEarlierRenameE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ws := createReadyWorkspace(t, context.Background(), fixture.client)
	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, "z.txt"), []byte("renamed content\n"), 0o644))
	gitfixture.Run(t, ws.WorktreePath, "add", "z.txt")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "add rename source")
	require.NoError(os.Rename(filepath.Join(ws.WorktreePath, "z.txt"), filepath.Join(ws.WorktreePath, "a.txt")))
	gitfixture.Run(t, ws.WorktreePath, "add", "-A")
	require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, "z.txt"), []byte("new current path\n"), 0o644))

	diff := requestWorkspaceDiffForPath(t, fixture.server, ws.Id, "head", "z.txt")
	require.NotNil(diff.Files)
	require.Len(diff.Files, 1)
	assert.Equal("z.txt", diff.Files[0].Path)
	assert.Equal("added", diff.Files[0].Status)

	preview := requestWorkspaceFilePreview(t, fixture.server, ws.Id, "head", "z.txt", "new")
	content, err := base64.StdEncoding.DecodeString(preview.Content)
	require.NoError(err)
	assert.Equal("z.txt", preview.Path)
	assert.Equal("new current path\n", string(content))
}

func TestWorkspaceDiffEndpointKeepsModifiedSourcePatchSeparateFromCopyE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ws := createReadyWorkspace(t, context.Background(), fixture.client)
	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")

	sourcePath := filepath.Join(ws.WorktreePath, "src", "a.txt")
	copiedPath := filepath.Join(ws.WorktreePath, "src", "z.txt")
	require.NoError(os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(os.WriteFile(sourcePath, []byte("base line\nshared line\n"), 0o644))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "add copy source fixture")
	require.NoError(os.WriteFile(copiedPath, []byte("base line\nshared line\n"), 0o644))
	require.NoError(os.WriteFile(sourcePath, []byte("changed line\nshared line\n"), 0o644))
	gitfixture.Run(t, ws.WorktreePath, "add", "src/z.txt")

	diff := requestWorkspaceDiff(t, fixture.server, ws.Id, "head")
	require.NotNil(diff.Files)
	source := testutil.RequireWorkspaceDiffFile(t, diff.Files, "src/a.txt")
	copied := testutil.RequireWorkspaceDiffFile(t, diff.Files, "src/z.txt")
	assert.Equal("modified", source.Status)
	assert.Equal(int64(1), source.Additions)
	assert.Equal(int64(1), source.Deletions)
	assert.Contains(source.Patch, "diff --git a/src/a.txt b/src/a.txt\n")
	assert.Contains(source.Patch, "+changed line\n")
	assert.NotContains(source.Patch, "copy to src/z.txt")
	require.NotNil(source.Hunks)
	require.Len(source.Hunks, 1)
	assert.Equal("copied", copied.Status)
	assert.Zero(copied.Additions)
	assert.Zero(copied.Deletions)
	assert.Empty(copied.Patch)
	require.NotNil(copied.Hunks)
	assert.Empty(copied.Hunks)
}

func TestWorkspaceDiffEndpointQuotesDangerousPathsE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	worktreePath := filepath.Join(dir, "worktree")
	gitfixture.Run(t, dir, "init", "--initial-branch=main", worktreePath)
	gitfixture.Run(t, worktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, worktreePath, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(worktreePath, "base.txt"), []byte("base\n"), 0o644))
	gitfixture.Run(t, worktreePath, "add", ".")
	gitfixture.Run(t, worktreePath, "commit", "-m", "base commit")

	maliciousPath := "src/evil\n--- forged\n+++ forged\n@@ -1,1 +1,1 @@"
	require.NoError(os.MkdirAll(filepath.Join(worktreePath, "src"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(worktreePath, maliciousPath), []byte("real content\n"), 0o644))
	unicodeSeparatorPath := "src/unicode\u2028separator\u2029file.go"
	require.NoError(os.WriteFile(filepath.Join(worktreePath, unicodeSeparatorPath), []byte("unicode separator content\n"), 0o644))

	database := dbtest.Open(t)
	srv := server.New(database, nil, nil, "/", nil, server.ServerOptions{WorktreeDir: filepath.Join(dir, "managed-worktrees")})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})
	require.NoError(database.InsertWorkspace(t.Context(), &workspace.Workspace{
		ID: "ws-control-paths", PlatformHost: "github.com", RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1, GitHeadRef: "feature/control-paths",
		WorkspaceBranch: "kenn-forge/pr-1", WorktreePath: worktreePath,
		TmuxSession: "kenn-forge-control-paths", Status: "ready",
	}))

	diff := requestWorkspaceDiff(t, srv, "ws-control-paths", "head")
	require.NotNil(diff.Files)
	file := testutil.RequireWorkspaceDiffFile(t, diff.Files, filepath.ToSlash(maliciousPath))
	assert.Contains(file.Patch, `diff --git "a/src/evil\n--- forged\n+++ forged\n@@ -1,1 +1,1 @@" "b/src/evil\n--- forged\n+++ forged\n@@ -1,1 +1,1 @@"`)
	assert.Contains(file.Patch, `+++ "b/src/evil\n--- forged\n+++ forged\n@@ -1,1 +1,1 @@"`)
	assert.NotContains(file.Patch, "\n--- forged\n")
	assert.NotContains(file.Patch, "\n+++ forged\n")
	assert.NotContains(file.Patch, "\n@@ -1,1 +1,1 @@\n")
	assert.Equal(1, strings.Count(file.Patch, "\n@@ "))

	unicodeFile := testutil.RequireWorkspaceDiffFile(t, diff.Files, filepath.ToSlash(unicodeSeparatorPath))
	assert.Contains(unicodeFile.Patch, `diff --git "a/src/unicode\u2028separator\u2029file.go" "b/src/unicode\u2028separator\u2029file.go"`)
	assert.Contains(unicodeFile.Patch, `+++ "b/src/unicode\u2028separator\u2029file.go"`)
	assert.NotContains(unicodeFile.Patch, "\u2028")
	assert.NotContains(unicodeFile.Patch, "\u2029")
}

func requestWorkspaceFiles(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	base string,
	whitespace ...string,
) generated.FilesResponse {
	t.Helper()
	query := "/api/v1/workspaces/" + workspaceID + "/files?base=" + base
	if len(whitespace) > 0 {
		query += "&whitespace=" + whitespace[0]
	}
	return requestWorkspaceFilesPath(t, srv, query)
}

func requestWorkspaceFilesQuery(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	query string,
) generated.FilesResponse {
	t.Helper()
	return requestWorkspaceFilesPath(t, srv, "/api/v1/workspaces/"+workspaceID+"/files?"+query)
}

func requestWorkspaceFilesPath(t *testing.T, srv *server.Server, query string) generated.FilesResponse {
	t.Helper()
	req := newWorkspaceFixtureRequest(http.MethodGet, query, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body generated.FilesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func requestWorkspaceCommits(t *testing.T, srv *server.Server, workspaceID string) generated.CommitsResponse {
	t.Helper()
	req := newWorkspaceFixtureRequest(http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/commits", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body generated.CommitsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func requestWorkspaceDiff(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	base string,
	whitespace ...string,
) generated.DiffResponse {
	t.Helper()
	query := "/api/v1/workspaces/" + workspaceID + "/diff?base=" + base
	if len(whitespace) > 0 {
		query += "&whitespace=" + whitespace[0]
	}
	return requestWorkspaceDiffPath(t, srv, query)
}

func requestWorkspaceDiffQuery(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	query string,
) generated.DiffResponse {
	t.Helper()
	return requestWorkspaceDiffPath(t, srv, "/api/v1/workspaces/"+workspaceID+"/diff?"+query)
}

func requestWorkspaceDiffForPath(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	base string,
	path string,
) generated.DiffResponse {
	t.Helper()
	return requestWorkspaceDiffQuery(
		t, srv, workspaceID,
		"base="+url.QueryEscape(base)+"&path="+url.QueryEscape(path),
	)
}

func requestWorkspaceDiffPath(t *testing.T, srv *server.Server, query string) generated.DiffResponse {
	t.Helper()
	req := newWorkspaceFixtureRequest(http.MethodGet, query, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body generated.DiffResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func requestWorkspaceFilePreview(
	t *testing.T,
	srv *server.Server,
	workspaceID string,
	base string,
	path string,
	side string,
) generated.FilePreviewResponse {
	t.Helper()
	query := "/api/v1/workspaces/" + workspaceID +
		"/file-preview?base=" + url.QueryEscape(base) +
		"&path=" + url.QueryEscape(path)
	if side != "" {
		query += "&side=" + url.QueryEscape(side)
	}
	req := newWorkspaceFixtureRequest(http.MethodGet, query, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body generated.FilePreviewResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func newWorkspaceFixtureRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "forge.test"
	return req
}

func assertWorkspaceDiffPaths(t *testing.T, files []generated.DiffFile, want []string) {
	t.Helper()
	assert.Equal(t, want, workspaceDiffPaths(files))
}

func workspaceDiffPaths(files []generated.DiffFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

func gracefulShutdown(t *testing.T, srv *server.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
}
