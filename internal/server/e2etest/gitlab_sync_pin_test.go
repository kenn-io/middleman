package e2etest

// Full-stack proof that a normal GitLab sync produces the reviewed
// diff snapshot head-bound mutations gate on: list sync discovers the
// MR, the detail sync (the same sync a detail view or post-conflict
// reload runs) fills base/head from diff_refs and records the
// snapshot against a real local clone, and the merge then succeeds
// with the sync-derived pin — no seeded SHAs anywhere.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
	platformgitlab "go.kenn.io/forge/platform/gitlab"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// setupGitLabCloneFixture builds a local git history (base commit on
// main, head commit on feature) plus a bare clone usable as the sync
// remote. gitcmd.New() strips inherited GIT_DIR/GIT_WORK_TREE so the
// fixture cannot touch the host repository under the pre-commit hook.
func setupGitLabCloneFixture(t *testing.T) (cloneURL, baseSHA, headSHA string) {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	require.NoError(os.MkdirAll(work, 0o755))
	run := func(args ...string) string {
		out, stderr, err := gitcmd.New().Run(t.Context(), work, nil, args...)
		require.NoError(err, "git %v: %s%s", args, out, stderr)
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "fixture@example.invalid")
	run("config", "user.name", "Fixture")
	require.NoError(os.WriteFile(filepath.Join(work, "a.txt"), []byte("base\n"), 0o644))
	run("add", "a.txt")
	run("commit", "-m", "base")
	baseSHA = run("rev-parse", "HEAD")
	run("checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(work, "b.txt"), []byte("head\n"), 0o644))
	run("add", "b.txt")
	run("commit", "-m", "head")
	headSHA = run("rev-parse", "HEAD")
	cloneURL = filepath.Join(dir, "origin.git")
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, "clone", "--bare", work, cloneURL)
	require.NoError(err, "%s%s", out, stderr)
	return cloneURL, baseSHA, headSHA
}
func TestGitLabNormalSyncEnablesHeadBoundMutations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	cloneURL, baseSHA, headSHA := setupGitLabCloneFixture(t)

	recorder := &gitlabAPIRecorder{}
	now := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	mrJSON := `{
		"id": 7001, "iid": 7, "title": "Sync pin MR", "state": "opened",
		"sha": "` + headSHA + `",
		"diff_refs": {"base_sha": "` + baseSHA + `", "head_sha": "` + headSHA + `", "start_sha": "` + baseSHA + `"},
		"source_branch": "feature", "target_branch": "main",
		"author": {"username": "author"},
		"created_at": "2026-05-01T09:00:00Z",
		"updated_at": "` + now + `"
	}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := recorder.record(r)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.EscapedPath()
		switch {
		case (path == "/api/v4/projects/acme%2Fwidget" || path == "/api/v4/projects/4242") &&
			r.Method == http.MethodGet:
			writeGitLabJSON(w, `{
				"id": 4242, "path": "widget", "path_with_namespace": "acme/widget",
				"web_url": "https://gitlab.com/acme/widget",
				"http_url_to_repo": `+strconv.Quote(cloneURL)+`,
				"default_branch": "main"
			}`)
		case path == "/api/v4/projects/4242/merge_requests" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[`+mrJSON+`]`)
		case path == "/api/v4/projects/4242/merge_requests/7" && r.Method == http.MethodGet:
			writeGitLabJSON(w, mrJSON)
		case path == "/api/v4/projects/4242/merge_requests/7/discussions" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/merge_requests/7/commits" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/pipelines" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/issues" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/labels" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/releases" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/repository/tags" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/merge_requests/7/merge" && r.Method == http.MethodPut:
			if !strings.Contains(request.Body, `"sha":"`+headSHA+`"`) {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "SHA does not match HEAD of source branch"}`)
				return
			}
			writeGitLabJSON(w, `{"id": 7001, "iid": 7, "state": "merged", "sha": "`+headSHA+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	client, err := platformgitlab.NewClient(
		"gitlab.com",
		staticGitLabTokenSource("token"),
		platformgitlab.WithBaseURLForTesting(api.URL+"/api/v4"), platformgitlab.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := platform.NewRegistry(client)
	require.NoError(err)

	database := dbtest.Open(t)
	clones := gitclone.New(t.TempDir(), nil)
	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     4242,
		PlatformExternalID: "4242",
		CloneURL:           cloneURL,
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, clones, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})

	// A normal periodic sync discovers the MR; the detail sync — the
	// same one a detail view or post-conflict reload performs — fills
	// diff_refs and records the reviewed snapshot. A SECOND list sync
	// then runs: the GitLab list payload omits diff_refs, so a naive
	// upsert would blank platform_base_sha and invalidate the snapshot
	// (the exact regression that re-armed 409 head_unknown).
	syncer.RunOnce(ctx)
	require.NoError(syncer.SyncMROnProvider(ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", 7))
	syncer.RunOnce(ctx)

	repoRow, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)
	require.NotNil(repoRow)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoRow.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(headSHA, mr.DiffHeadSHA,
		"a normal sync must record the reviewed diff snapshot for GitLab")
	assert.Equal(baseSHA, mr.PlatformBaseSHA,
		"a later list sync must not blank the detail-populated base SHA")

	var detail struct {
		ReviewedHeadSHA string `json:"reviewed_head_sha"`
	}
	rr := doGitLabJSON(t, srv, http.MethodGet, "/api/v1/pulls/gitlab/acme/widget/7", "")
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(json.NewDecoder(rr.Body).Decode(&detail))
	assert.Equal(headSHA, detail.ReviewedHeadSHA)

	rr = doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/merge",
		`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"`+headSHA+`"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	merge, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/merge")
	require.True(ok, "merge must reach the provider")
	assert.Contains(merge.Body, `"sha":"`+headSHA+`"`,
		"the sync-derived reviewed head must reach GitLab as the sha pin")
}
