package workspacetest

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestIssueWorkspaceRecoversExpectedDirectory(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	seedIssue(t, fixture.database, "acme", "widget", 7, "open")

	branch := "kenn-forge/issue-7"
	expectedPath := filepath.Join(
		fixture.worktreeDir,
		"github", "github.com", "acme", "widget",
		fmt.Sprintf("repo-%d", fixture.repoID), "issue-7",
	)
	gitfixture.Run(
		t, fixture.bare,
		"worktree", "add", expectedPath, "-b", branch, "main",
	)
	wantHead := gitfixture.SHA(t, expectedPath, "HEAD")
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "base.txt"), []byte("dirty\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "untracked.txt"), []byte("keep\n"), 0o644,
	))
	wantStatus := workspaceGitOutput(t, expectedPath, "status", "--short")

	reuseDirectory := true
	resp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
		generated.CreateIssueWorkspaceInputBody{
			GitHeadRef:             &branch,
			ReuseExistingDirectory: &reuseDirectory,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)

	ready := waitForWorkspaceReady(
		t, t.Context(), fixture.client, resp.JSON202.Id,
	)
	assert.Equal(expectedPath, ready.WorktreePath)
	assert.Equal(
		branch,
		workspaceGitOutput(t, expectedPath, "branch", "--show-current"),
	)
	assert.Equal(wantHead, gitfixture.SHA(t, expectedPath, "HEAD"))
	assert.Equal(wantStatus, workspaceGitOutput(t, expectedPath, "status", "--short"))
	tracked, err := os.ReadFile(filepath.Join(expectedPath, "base.txt"))
	require.NoError(err)
	assert.Equal("dirty\n", string(tracked))
	assert.FileExists(filepath.Join(expectedPath, "untracked.txt"))
}

func TestIssueWorkspaceDirectoryRecoveryRejectsMissingPath(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	seedIssue(t, fixture.database, "acme", "widget", 7, "open")

	branch := "kenn-forge/issue-7"
	reuseDirectory := true
	resp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
		generated.CreateIssueWorkspaceInputBody{
			GitHeadRef:             &branch,
			ReuseExistingDirectory: &reuseDirectory,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusConflict, resp.StatusCode(), string(resp.Body))
	problem := resp.ApplicationproblemJSONDefault
	require.NotNil(problem)
	assert.Equal(generated.WorkspaceDirectoryNotReusable, problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("missing", (*problem.Details)["reason"])

	workspace, err := fixture.database.GetWorkspaceByIssueForProvider(
		t.Context(), "github", "github.com", "acme", "widget", 7,
	)
	require.NoError(err)
	assert.Nil(workspace)
}

func TestIssueWorkspaceDirectoryRecoveryReasons(t *testing.T) {
	tests := []struct {
		name          string
		prepare       func(t *testing.T, fixture workspaceServerFixture, path string)
		wantReason    string
		checkBranches bool
		wantExpected  string
		wantActual    string
	}{
		{
			name: "ordinary directory",
			prepare: func(t *testing.T, _ workspaceServerFixture, path string) {
				require.NoError(t, os.MkdirAll(path, 0o755))
			},
			wantReason: "not_linked_worktree",
		},
		{
			name: "wrong repository",
			prepare: func(t *testing.T, _ workspaceServerFixture, path string) {
				repo := filepath.Join(t.TempDir(), "other")
				gitfixture.Run(t, filepath.Dir(repo), "init", "--initial-branch=main", repo)
				gitfixture.Run(t, repo, "config", "user.email", "test@test.com")
				gitfixture.Run(t, repo, "config", "user.name", "Test")
				require.NoError(t, os.WriteFile(
					filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644,
				))
				gitfixture.Run(t, repo, "add", ".")
				gitfixture.Run(t, repo, "commit", "-m", "base")
				gitfixture.Run(t, repo, "worktree", "add", path, "-b", "kenn-forge/issue-7", "HEAD")
			},
			wantReason: "repository_mismatch",
		},
		{
			name: "wrong branch",
			prepare: func(t *testing.T, fixture workspaceServerFixture, path string) {
				gitfixture.Run(t, fixture.bare, "worktree", "add", path, "-b", "other/branch", "main")
			},
			wantReason:    "branch_mismatch",
			checkBranches: true,
			wantExpected:  "kenn-forge/issue-7",
			wantActual:    "other/branch",
		},
		{
			name: "detached head",
			prepare: func(t *testing.T, fixture workspaceServerFixture, path string) {
				gitfixture.Run(t, fixture.bare, "worktree", "add", "--detach", path, "main")
			},
			wantReason:    "branch_mismatch",
			checkBranches: true,
			wantExpected:  "kenn-forge/issue-7",
			wantActual:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runParallelWorkspaceGitTest(t)
			assert := assert.New(t)
			require := require.New(t)
			fixture := setupWorkspaceServerFixture(t, nil)
			seedIssue(t, fixture.database, "acme", "widget", 7, "open")
			expectedPath := filepath.Join(
				fixture.worktreeDir,
				"github", "github.com", "acme", "widget",
				fmt.Sprintf("repo-%d", fixture.repoID), "issue-7",
			)
			tt.prepare(t, fixture, expectedPath)

			branch := "kenn-forge/issue-7"
			reuseDirectory := true
			resp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
				t.Context(), "gh", "acme", "widget", 7,
				generated.CreateIssueWorkspaceInputBody{
					GitHeadRef:             &branch,
					ReuseExistingDirectory: &reuseDirectory,
				},
			)
			require.NoError(err)
			require.Equal(http.StatusConflict, resp.StatusCode(), string(resp.Body))
			problem := resp.ApplicationproblemJSONDefault
			require.NotNil(problem)
			assert.Equal(generated.WorkspaceDirectoryNotReusable, problem.Code)
			require.NotNil(problem.Details)
			details := *problem.Details
			assert.Equal(tt.wantReason, details["reason"])
			if tt.checkBranches {
				assert.Equal(tt.wantExpected, details["expectedBranch"])
				assert.Equal(tt.wantActual, details["actualBranch"])
			}
			workspace, getErr := fixture.database.GetWorkspaceByIssueForProvider(
				t.Context(), "github", "github.com", "acme", "widget", 7,
			)
			require.NoError(getErr)
			assert.Nil(workspace)
		})
	}
}

func TestIssueWorkspaceConflictRejectsAlternateBranchForExistingDirectory(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	seedIssue(t, fixture.database, "acme", "widget", 7, "open")

	branch := "kenn-forge/issue-7-original-title"
	expectedPath := filepath.Join(
		fixture.worktreeDir,
		"github", "github.com", "acme", "widget",
		fmt.Sprintf("repo-%d", fixture.repoID), "issue-7",
	)
	gitfixture.Run(t, fixture.bare, "worktree", "add", expectedPath, "-b", branch, "main")

	resp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
		generated.CreateIssueWorkspaceInputBody{},
	)
	require.NoError(err)
	require.Equal(http.StatusConflict, resp.StatusCode(), string(resp.Body))
	problem := resp.ApplicationproblemJSONDefault
	require.NotNil(problem)
	assert.Equal(generated.BranchConflict, problem.Code)
	require.NotNil(problem.Details)
	assert.Equal(true, (*problem.Details)["existingDirectory"])
	require.NotNil(problem.Errors)
	locations := map[string]any{}
	for _, detail := range *problem.Errors {
		if detail.Location != nil {
			locations[*detail.Location] = detail.Value
		}
	}
	assert.Equal(branch, locations["body.git_head_ref"])

	alternateBranch := branch + "-2"
	resp, err = fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
		generated.CreateIssueWorkspaceInputBody{GitHeadRef: &alternateBranch},
	)
	require.NoError(err)
	require.Equal(http.StatusConflict, resp.StatusCode(), string(resp.Body))
	problem = resp.ApplicationproblemJSONDefault
	require.NotNil(problem)
	assert.Equal(generated.BranchConflict, problem.Code)
	require.NotNil(problem.Details)
	assert.Equal(true, (*problem.Details)["existingDirectory"])

	workspace, getErr := fixture.database.GetWorkspaceByIssueForProvider(
		t.Context(), "github", "github.com", "acme", "widget", 7,
	)
	require.NoError(getErr)
	assert.Nil(workspace)
}

func TestIssueWorkspaceRejectsConflictingReuseOptions(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	seedIssue(t, fixture.database, "acme", "widget", 7, "open")

	reuse := true
	resp, err := fixture.client.HTTP.CreateIssueWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
		generated.CreateIssueWorkspaceInputBody{
			ReuseExistingBranch:    &reuse,
			ReuseExistingDirectory: &reuse,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusBadRequest, resp.StatusCode(), string(resp.Body))
	problem := resp.ApplicationproblemJSONDefault
	require.NotNil(problem)
	assert.Equal(generated.ValidationError, problem.Code)
}
