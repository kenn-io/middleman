package workspacetest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitsafe"
	"go.kenn.io/forge/internal/testutil/servertest"
)

// registerIdentifiedProject registers localPath as a project carrying the
// acme/widget GitHub identity and returns its id.
func registerIdentifiedProject(
	t *testing.T, ts *httptest.Server, localPath string,
) string {
	return registerPlatformProject(
		t, ts, localPath, "github", "github.com", "acme", "widget",
	)
}

func logUnexpectedResponse(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode == want {
		return
	}
	body, err := io.ReadAll(resp.Body)
	Require.NoError(t, err)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	t.Logf("unexpected response: %s", body)
}

func registerPlatformProject(
	t *testing.T, ts *httptest.Server, localPath, platform, host, owner, name string,
) string {
	t.Helper()
	body := mustMarshal(t, map[string]any{
		"local_path": localPath,
		"platform_identity": map[string]any{
			"platform":      platform,
			"platform_host": host,
			"owner":         owner,
			"name":          name,
		},
	})
	resp := httpDo(t, ts, http.MethodPost, "/api/v1/projects", body)
	Require.Equal(t, http.StatusCreated, resp.StatusCode)
	defer resp.Body.Close()
	var registered struct {
		ID string `json:"id"`
	}
	Require.NoError(t, json.NewDecoder(resp.Body).Decode(&registered))
	return registered.ID
}

func seedMergeRequest(
	t *testing.T, database *db.DB, number int, headBranch, headSHA, cloneURL string,
) {
	seedMergeRequestForRepo(t, database, db.GitHubRepoIdentity(
		"github.com", "acme", "widget",
	), number, headBranch, headSHA, cloneURL)
}

func seedMergeRequestForRepo(
	t *testing.T, database *db.DB, identity db.RepoIdentity,
	number int, headBranch, headSHA, cloneURL string,
) {
	t.Helper()
	ctx := t.Context()
	if identity.PlatformRepoID == "" {
		identity.PlatformRepoID = "repo-" + identity.Owner + "-" + identity.Name
	}
	repoID, err := database.UpsertRepo(ctx, identity)
	Require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       int64(90000 + number),
		Number:           number,
		URL:              "https://github.com/acme/widget/pull/42",
		Title:            "Add feature",
		Author:           "octocat",
		State:            "open",
		IsDraft:          true,
		HeadBranch:       headBranch,
		PlatformHeadSHA:  headSHA,
		BaseBranch:       "main",
		HeadRepoCloneURL: cloneURL,
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	})
	Require.NoError(t, err)
}

// TestCreateWorktreeFromMergeRequestRoute covers the happy path for a
// same-repo merge request: the head branch is fetched from the project's
// origin, materialized as a new worktree, and registered.
func TestCreateWorktreeFromMergeRequestRoute(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := Require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// origin carries the merge request head branch; clone is the project.
	origin := initLifecycleRouteRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleRouteGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	lifecycleRouteGit(t, origin, "checkout", "-q", "-b", "feature-x")
	lifecycleRouteGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleRouteGit(t, origin, "rev-parse", "feature-x")
	lifecycleRouteGit(t, origin, "checkout", "-q", "main")

	// Providers report the hosted clone URL even when this project uses a
	// local mirror as origin.
	seedMergeRequest(t, database, 42, "feature-x", headSHA,
		"https://github.com/acme/widget.git")
	projectID := registerIdentifiedProject(t, ts, clone)

	dest := filepath.Join(t.TempDir(), "wt")
	body := mustMarshal(t, map[string]any{
		"number": 42,
		"branch": "pr-42",
		"path":   dest,
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var created struct {
		ID           string `json:"id"`
		Branch       string `json:"branch"`
		Path         string `json:"path"`
		MergeRequest struct {
			Number  int    `json:"number"`
			URL     string `json:"url"`
			State   string `json:"state"`
			Title   string `json:"title"`
			IsDraft bool   `json:"is_draft"`
		} `json:"merge_request"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&created))
	resp.Body.Close()

	assert.Equal("pr-42", created.Branch)
	assert.Equal(dest, created.Path)
	assert.Equal(42, created.MergeRequest.Number,
		"the response echoes which merge request was materialized")
	assert.Equal("https://github.com/acme/widget/pull/42",
		created.MergeRequest.URL)
	assert.Equal("open", created.MergeRequest.State)
	assert.Equal("Add feature", created.MergeRequest.Title)
	assert.True(created.MergeRequest.IsDraft,
		"draft state must survive the import response so callers"+
			" can fold it into their own state vocabulary")
	assert.Equal(headSHA,
		lifecycleRouteGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the merge request head")
	assert.Equal("origin/feature-x",
		lifecycleRouteGit(t, dest, "rev-parse", "--abbrev-ref",
			"--symbolic-full-name", "@{upstream}"),
		"same-repository imports track the project origin branch")
	rows := listWorktreeRows(t, ts, projectID)
	require.Len(rows, 2, "root checkout row plus the imported worktree")
	require.NotNil(worktreeRowByBranch(rows, "pr-42"),
		"imported worktree is registered")
}

func TestCreateWorktreeFromMergeRequestRouteRejectsChangedHead(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	origin := initLifecycleRouteRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleRouteGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	staleSHA := lifecycleRouteGit(t, origin, "rev-parse", "HEAD")
	lifecycleRouteGit(t, origin, "checkout", "-q", "-b", "feature-moved")
	lifecycleRouteGit(t, origin, "commit", "--allow-empty", "-m", "new head")
	newHeadSHA := lifecycleRouteGit(t, origin, "rev-parse", "HEAD")
	lifecycleRouteGit(t, origin, "update-ref", "refs/pull/44/head", newHeadSHA)
	lifecycleRouteGit(t, origin, "checkout", "-q", "main")

	seedMergeRequest(t, database, 44, "feature-moved", staleSHA,
		"https://github.com/acme/widget.git")
	projectID := registerIdentifiedProject(t, ts, clone)
	destination := filepath.Join(t.TempDir(), "wt")
	body := mustMarshal(t, map[string]any{
		"number": 44,
		"branch": "pr-44",
		"path":   destination,
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	defer resp.Body.Close()

	logUnexpectedResponse(t, resp, http.StatusConflict)
	assert.Equal(http.StatusConflict, resp.StatusCode)
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(resp.Body).Decode(&problem))
	assert.Equal("stale_state", problem.Details["reason"])
	assert.NoDirExists(destination)
}

func TestCreateWorktreeFromGitLabMergeRequestRefRoute(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	origin := initLifecycleRouteRepo(t)
	lifecycleRouteGit(t, origin, "checkout", "-q", "-b", "gitlab-head")
	lifecycleRouteGit(t, origin, "commit", "--allow-empty", "-m", "gitlab mr")
	headSHA := lifecycleRouteGit(t, origin, "rev-parse", "HEAD")
	lifecycleRouteGit(t, origin, "update-ref", "refs/merge-requests/55/head", headSHA)
	lifecycleRouteGit(t, origin, "checkout", "-q", "main")
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleRouteGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)

	seedMergeRequestForRepo(t, database, db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, 55, "", headSHA, "")
	projectID := registerPlatformProject(
		t, ts, clone, "gitlab", "gitlab.example.com", "acme", "widget",
	)

	dest := filepath.Join(t.TempDir(), "wt")
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request",
		mustMarshal(t, map[string]any{"number": 55, "branch": "mr-55", "path": dest}))
	defer resp.Body.Close()

	logUnexpectedResponse(t, resp, http.StatusCreated)
	require.Equal(http.StatusCreated, resp.StatusCode)
	assert.Equal(headSHA, lifecycleRouteGit(t, dest, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfigForRoute(t, dest, "branch.mr-55.remote"))
	rows := listWorktreeRows(t, ts, projectID)
	require.Len(rows, 2)
	require.NotNil(worktreeRowByBranch(rows, "mr-55"))
}

func TestCreateWorktreeFromRelativeForkRoutePersistsAbsoluteTracking(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	origin := initLifecycleRouteRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleRouteGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	fork := filepath.Join(filepath.Dir(clone), "forks", "octocat", "widget")
	require.NoError(os.MkdirAll(filepath.Dir(fork), 0o755))
	lifecycleRouteGit(t, filepath.Dir(fork), "clone", "-q", origin, fork)
	lifecycleRouteGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleRouteGit(t, fork, "config", "user.name", "Tester")
	lifecycleRouteGit(t, fork, "checkout", "-q", "-b", "relative-head")
	lifecycleRouteGit(t, fork, "commit", "--allow-empty", "-m", "relative fork")
	headSHA := lifecycleRouteGit(t, fork, "rev-parse", "HEAD")
	lifecycleRouteGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/relative-head:refs/pull/45/head")
	relativeFork, err := filepath.Rel(clone, fork)
	require.NoError(err)

	seedMergeRequest(t, database, 45, "relative-head", headSHA, relativeFork)
	projectID := registerIdentifiedProject(t, ts, clone)
	dest := filepath.Join(t.TempDir(), "wt")
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request",
		mustMarshal(t, map[string]any{"number": 45, "branch": "pr-45", "path": dest}))
	defer resp.Body.Close()

	logUnexpectedResponse(t, resp, http.StatusCreated)
	require.Equal(http.StatusCreated, resp.StatusCode)
	remote := worktreeConfigForRoute(t, dest, "branch.pr-45.remote")
	require.NotEmpty(remote)
	assert.Equal(fork, lifecycleRouteGit(t, clone, "remote", "get-url", remote))
}

func worktreeConfigForRoute(t *testing.T, dir, key string) string {
	t.Helper()
	out, err := gitsafe.Runner().Output(t.Context(), dir, "config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestCreateWorktreeFromMergeRequestRouteUnknownNumber: an unsynced merge
// request is a 404 with the pullNotFound code, and nothing touches disk.
func TestCreateWorktreeFromMergeRequestRouteUnknownNumber(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := Require.New(t)
	assert := assert.New(t)

	srv, database := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := initLifecycleRouteRepo(t)
	_, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	projectID := registerIdentifiedProject(t, ts, repo)

	dest := filepath.Join(t.TempDir(), "wt")
	body := mustMarshal(t, map[string]any{
		"number": 99,
		"branch": "pr-99",
		"path":   dest,
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	assert.Equal("pullNotFound", decodeProblemCode(t, resp))
	resp.Body.Close()
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
}

// TestCreateWorktreeFromMergeRequestRouteNoIdentity: a local-only project
// cannot resolve merge requests.
func TestCreateWorktreeFromMergeRequestRouteNoIdentity(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := Require.New(t)

	srv, _ := setupProjectServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	repo := initLifecycleRouteRepo(t)
	projectID := registerProjectForTest(t, ts, repo)

	body := mustMarshal(t, map[string]any{
		"number": 1,
		"branch": "pr-1",
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	require.Equal(http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateWorktreeFromMergeRequestRouteSyncsOnDemand: a merge request
// the registry has not synced yet is fetched from the provider during
// import instead of failing with pullNotFound, so a caller (e.g. a fleet
// hub proxying into this host) does not need a separate sync step.
func TestCreateWorktreeFromMergeRequestRouteSyncsOnDemand(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := Require.New(t)
	assert := assert.New(t)

	origin := initLifecycleRouteRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleRouteGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	lifecycleRouteGit(t, origin, "checkout", "-q", "-b", "feature-y")
	lifecycleRouteGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleRouteGit(t, origin, "rev-parse", "feature-y")
	lifecycleRouteGit(t, origin, "update-ref", "refs/pull/43/head", headSHA)
	lifecycleRouteGit(t, origin, "checkout", "-q", "main")

	now := time.Now()
	prID := int64(9043)
	number := 43
	nodeID := "PR_kwDO9043"
	title := "on-demand sync"
	state := "open"
	url := "https://github.com/acme/widget/pull/43"
	author := "ada"
	headRef := "feature-y"
	baseRef := "main"
	cloneURL := "https://github.com/acme/widget.git"
	fullName := "acme/widget"
	mock := testutil.NewFixtureClient().(*testutil.FixtureClient)
	mock.PRs["acme/widget"] = []*gh.PullRequest{{
		ID:        &prID,
		NodeID:    &nodeID,
		Number:    &number,
		HTMLURL:   &url,
		Title:     &title,
		State:     &state,
		User:      &gh.User{Login: &author},
		CreatedAt: &gh.Timestamp{Time: now},
		UpdatedAt: &gh.Timestamp{Time: now},
		Head: &gh.PullRequestBranch{
			Ref: &headRef,
			SHA: &headSHA,
			Repo: &gh.Repository{
				CloneURL: &cloneURL,
				FullName: &fullName,
			},
		},
		Base: &gh.PullRequestBranch{Ref: &baseRef},
	}}
	database := dbtest.Open(t)
	ref := ghclient.RepoRef{
		Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "widget",
		PlatformExternalID: "repo-acme-widget",
	}
	_, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock}, database, nil,
		[]ghclient.RepoRef{ref},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	projectID := registerIdentifiedProject(t, ts, clone)

	dest := filepath.Join(t.TempDir(), "wt")
	body := mustMarshal(t, map[string]any{
		"number": 43,
		"branch": "pr-43",
		"path":   dest,
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	require.Equal(http.StatusCreated, resp.StatusCode)
	var created struct {
		Branch string `json:"branch"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&created))
	resp.Body.Close()
	assert.Equal("pr-43", created.Branch)
	assert.Equal(headSHA,
		lifecycleRouteGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the merge request head")
}

func TestCreateWorktreeFromMergeRequestRouteDoesNotSyncRemovedItem(t *testing.T) {
	runParallelWorkspaceGitTest(t)
	require := Require.New(t)

	repoPath := initLifecycleRouteRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	number := 43
	providerID := int64(9043)
	nodeID := "PR_kwDO9043"
	title := "removed pull"
	state := "open"
	url := "https://github.com/acme/widget/pull/43"
	author := "ada"
	headRef := "feature-y"
	baseRef := "main"
	cloneURL := "https://github.com/acme/widget.git"
	fullName := "acme/widget"
	mock := testutil.NewFixtureClient().(*testutil.FixtureClient)
	mock.PRs["acme/widget"] = []*gh.PullRequest{{
		ID: &providerID, NodeID: &nodeID, Number: &number,
		HTMLURL: &url, Title: &title, State: &state,
		User:      &gh.User{Login: &author},
		CreatedAt: &gh.Timestamp{Time: now}, UpdatedAt: &gh.Timestamp{Time: now},
		Head: &gh.PullRequestBranch{
			Ref: &headRef, SHA: new(string),
			Repo: &gh.Repository{CloneURL: &cloneURL, FullName: &fullName},
		},
		Base: &gh.PullRequestBranch{Ref: &baseRef},
	}}
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', ?, ?, ?, ?, 'removed_upstream')`,
		repoID, number, nodeID, now, now,
	)
	require.NoError(err)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock}, database, nil,
		[]ghclient.RepoRef{{
			Platform: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", PlatformExternalID: "repo-acme-widget",
		}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	projectID := registerIdentifiedProject(t, ts, repoPath)

	body := mustMarshal(t, map[string]any{
		"number": number, "branch": "pr-43", "path": filepath.Join(t.TempDir(), "wt"),
	})
	resp := httpDo(t, ts, http.MethodPost,
		"/api/v1/projects/"+projectID+"/worktrees/from-merge-request", body)
	logUnexpectedResponse(t, resp, http.StatusNotFound)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	require.Equal("pullNotFound", decodeProblemCode(t, resp))
	resp.Body.Close()

	stored, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, number)
	require.NoError(err)
	require.Nil(stored, "removed item must be rejected before on-demand provider sync")
}
