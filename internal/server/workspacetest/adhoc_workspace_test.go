package workspacetest

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

// Starting new work needs no provider item: a tracked repository plus an
// optional branch name is enough to get a materialized worktree.
func TestCreateAdHocWorkspaceMaterializesRequestedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/rate-limits"

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.NotNil(resp.JSON202.Created)
	assert.True(*resp.JSON202.Created)
	assert.Equal("adhoc", resp.JSON202.ItemType)
	assert.EqualValues(0, resp.JSON202.ItemNumber)
	assert.Equal(branch, resp.JSON202.GitHeadRef)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	assert.Equal(branch, ready.GitHeadRef)
	assert.Nil(ready.MrTitle)

	head := gitfixture.SHA(t, ready.WorktreePath, "HEAD")
	assert.Equal(gitfixture.SHA(t, fixture.remote, "refs/heads/main"), head,
		"ad-hoc workspaces branch from the repository default branch")
	checkedOut, err := os.ReadFile(filepath.Join(ready.WorktreePath, "base.txt"))
	require.NoError(err)
	assert.Equal("base\n", string(checkedOut))
}

func TestCreateAdHocWorkspaceAfterRepositoryRouteReuse(t *testing.T) {
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	replacementBare, err := fixture.clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-current-occupant"),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	require.NotEqual(fixture.bare, replacementBare)
	gitfixture.Run(t, t.TempDir(), "clone", "--bare", fixture.remote, replacementBare)
	gitfixture.Run(
		t, replacementBare, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	gitfixture.Run(
		t, replacementBare, "config", "--add",
		"url."+fixture.remote+".insteadOf", "https://github.com/acme/widget.git",
	)
	current, _, err := fixture.database.ReconcileRepositoryObservation(
		t.Context(),
		db.RepoIdentity{
			Platform:       "github",
			PlatformHost:   "github.com",
			PlatformRepoID: "repo-current-occupant",
			Owner:          "acme",
			Name:           "widget",
		},
		time.Now().UTC().Add(time.Hour),
	)
	require.NoError(err)
	require.NotNil(current)

	branch := "spike/route-reuse"
	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	require.Equal(branch, ready.GitHeadRef)
	workspace, err := fixture.database.GetWorkspace(t.Context(), ready.Id)
	require.NoError(err)
	require.NotNil(workspace)
	require.Equal(current.Repository.ID, workspace.RepoID)
}

func TestCreateAdHocWorkspaceGeneratesBranchWhenOmitted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	assert.True(strings.HasPrefix(resp.JSON202.GitHeadRef, "kenn-forge/work-"),
		"generated branch %q should carry the work prefix", resp.JSON202.GitHeadRef)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	assert.Equal(resp.JSON202.GitHeadRef, ready.GitHeadRef)
}

// A repeat request for the same branch reopens the workspace that already owns
// it rather than creating a second worktree.
func TestCreateAdHocWorkspaceReusesWorkspaceForSameBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/rate-limits"
	body := generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch}

	first, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", body,
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, first.StatusCode(), string(first.Body))
	require.NotNil(first.JSON202)

	second, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget", body,
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, second.StatusCode(), string(second.Body))
	require.NotNil(second.JSON202)
	assert.Equal(first.JSON202.Id, second.JSON202.Id)
	assert.Nil(second.JSON202.Created)

	listResp, err := fixture.client.HTTP.ListWorkspacesWithResponse(t.Context())
	require.NoError(err)
	require.NotNil(listResp.JSON200)
	adhoc := 0
	for _, ws := range listResp.JSON200.Workspaces {
		if ws.ItemType == "adhoc" {
			adhoc++
		}
	}
	assert.Equal(1, adhoc)
}

func TestCreateAdHocWorkspaceRejectsInvalidBranch(t *testing.T) {
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "bad branch"

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch},
	)
	require.NoError(err)
	require.Equal(http.StatusBadRequest, resp.StatusCode(), string(resp.Body))
}

func TestCreateAdHocWorkspaceRejectsUntrackedRepo(t *testing.T) {
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/thing"

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "unknown",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch},
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, resp.StatusCode(), string(resp.Body))
}

func TestCreateAdHocWorkspaceExistingBranchIsUniquified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/rate-limits"
	mainSHA := gitfixture.SHA(t, fixture.remote, "refs/heads/main")
	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/"+branch, mainSHA)

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &branch},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	assert.Regexp(`^spike/rate-limits-[0-9a-f]{4}$`, resp.JSON202.GitHeadRef)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	assert.Equal(resp.JSON202.GitHeadRef, ready.GitHeadRef)
	assert.Equal(resp.JSON202.GitHeadRef, workspaceGitOutput(
		t, ready.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD",
	))
}

func TestCreateAdHocWorkspaceReusesExistingBranchWhenAsked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/rate-limits"
	mainSHA := gitfixture.SHA(t, fixture.remote, "refs/heads/main")
	gitfixture.Run(t, fixture.bare, "update-ref", "refs/heads/"+branch, mainSHA)
	reuse := true

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{
			Branch: &branch, ReuseExistingBranch: &reuse,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	assert.Nil(resp.JSON202.Created)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	assert.Equal(branch, ready.GitHeadRef)
	checkedOut := workspaceGitOutput(
		t, ready.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD",
	)
	assert.Equal(branch, checkedOut)
}

func TestCreateAdHocWorkspaceReuseMissingBranchReportsCreated(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	branch := "spike/rate-limits"
	reuse := true

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{
			Branch: &branch, ReuseExistingBranch: &reuse,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.NotNil(resp.JSON202.Created)
	assert.True(*resp.JSON202.Created)
}

// Reuse is the one case where work does not start at origin/HEAD: the existing
// branch is adopted at its own tip, however far that has diverged.
func TestCreateAdHocWorkspaceReuseStartsFromDivergedBranchTip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	// The fixture's "feature" branch carries a commit that main does not.
	branch := "feature"
	featureSHA := gitfixture.SHA(t, fixture.remote, "refs/heads/"+branch)
	mainSHA := gitfixture.SHA(t, fixture.remote, "refs/heads/main")
	require.NotEqual(mainSHA, featureSHA)
	reuse := true

	resp, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{
			Branch: &branch, ReuseExistingBranch: &reuse,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)

	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, resp.JSON202.Id)
	assert.Equal(branch, ready.GitHeadRef)
	assert.Equal(featureSHA, gitfixture.SHA(t, ready.WorktreePath, "HEAD"),
		"reuse adopts the existing branch tip, not origin/HEAD")
	_, err = os.Stat(filepath.Join(ready.WorktreePath, "new.txt"))
	assert.NoError(err, "the diverged commit's file should be checked out")
}

// Renaming the branch from inside the worktree is a shell action kenn-forge does
// not observe, so the workspace keeps its creation-time identity. The old name
// still resolves to it, while a new workspace request for the renamed branch
// receives a unique branch.
func TestCreateAdHocWorkspaceAfterInWorktreeBranchRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	original := "spike/rate-limits"
	renamed := "spike/rate-limits-v2"

	created, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &original},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, created.StatusCode(), string(created.Body))
	require.NotNil(created.JSON202)
	ready := waitForWorkspaceReady(t, t.Context(), fixture.client, created.JSON202.Id)

	gitfixture.Run(t, ready.WorktreePath, "branch", "-m", original, renamed)

	// Old name: still this workspace, because item_key is the creation-time
	// branch and is never rewritten.
	again, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &original},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, again.StatusCode(), string(again.Body))
	require.NotNil(again.JSON202)
	assert.Equal(created.JSON202.Id, again.JSON202.Id)

	unique, err := fixture.client.HTTP.CreateRepoWorkspaceWithResponse(
		t.Context(), "gh", "acme", "widget",
		generated.CreateRepoWorkspaceJSONRequestBody{Branch: &renamed},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, unique.StatusCode(), string(unique.Body))
	require.NotNil(unique.JSON202)
	assert.Regexp(
		`^spike/rate-limits-v2-[0-9a-f]{4}$`,
		unique.JSON202.GitHeadRef,
	)
}
