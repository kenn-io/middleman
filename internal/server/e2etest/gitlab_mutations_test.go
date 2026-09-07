package e2etest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformgitlab "go.kenn.io/forge/platform/gitlab"
)

const gitlabMutationThreadID = "abc123def456789012345678901234567890abcd"

type staticGitLabTokenSource string

func (s staticGitLabTokenSource) Token(context.Context) (string, error) { return string(s), nil }
func (s staticGitLabTokenSource) Invalidate(string)                     {}
func (s staticGitLabTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "gitlab", Host: "gitlab.com"}}
}

type recordedGitLabRequest struct {
	Method string
	Path   string
	Body   string
}

type gitlabAPIRecorder struct {
	mu       sync.Mutex
	requests []recordedGitLabRequest
}

func (rec *gitlabAPIRecorder) record(r *http.Request) recordedGitLabRequest {
	body, _ := io.ReadAll(r.Body)
	entry := recordedGitLabRequest{
		Method: r.Method,
		Path:   r.URL.EscapedPath(),
		Body:   string(body),
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests = append(rec.requests, entry)
	return entry
}

func (rec *gitlabAPIRecorder) find(method, path string) (recordedGitLabRequest, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, request := range rec.requests {
		if request.Method == method && request.Path == path {
			return request, true
		}
	}
	return recordedGitLabRequest{}, false
}

// findEventually polls for a request issued from a background goroutine
// (e.g. the resync triggered after a stale mutation).
func (rec *gitlabAPIRecorder) findEventually(method, path string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := rec.find(method, path); ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// setupGitLabMutationServer wires the real GitLab provider client against a
// fake GitLab REST API and seeds a tracked repo with MR 7 (including one
// existing comment, platform id 9001) and issue 11.
func setupGitLabMutationServer(
	t *testing.T,
) (*server.Server, *db.DB, *gitlabAPIRecorder, int64) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	recorder := &gitlabAPIRecorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := recorder.record(r)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.EscapedPath()
		switch {
		case (path == "/api/v4/projects/acme%2Fwidget" || path == "/api/v4/projects/4242") &&
			r.Method == http.MethodGet:
			writeGitLabJSON(w, `{
				"id": 4242,
				"path": "widget",
				"path_with_namespace": "acme/widget",
				"web_url": "https://gitlab.com/acme/widget",
				"http_url_to_repo": "https://gitlab.com/acme/widget.git",
				"default_branch": "main"
			}`)
		case path == "/api/v4/projects/4242/merge_requests/7" && r.Method == http.MethodGet:
			// Reflect prior mutations the way the real provider does: a
			// successful merge or close changes the state later GETs
			// report, which the canonical post-mutation resync depends on.
			// updated_at must be current: UpsertMergeRequest discards
			// snapshots older than the stored row.
			state := "opened"
			terminalFields := ""
			if merge, ok := recorder.find(
				http.MethodPut, "/api/v4/projects/4242/merge_requests/7/merge",
			); ok && strings.Contains(merge.Body, `"sha":"head-sha"`) &&
				!strings.Contains(merge.Body, "force-generic-conflict") {
				state = "merged"
				terminalFields = `"merged_at": "2026-06-01T11:00:00Z",
					"merged_by": {"username": "ada"},`
			} else if update, ok := recorder.find(
				http.MethodPut, "/api/v4/projects/4242/merge_requests/7",
			); ok && strings.Contains(update.Body, `"state_event":"close"`) {
				state = "closed"
				terminalFields = `"closed_at": "2026-06-01T11:00:00Z",`
			}
			writeGitLabJSON(w, `{
				"id": 7001, "iid": 7, "title": "Test MR", "state": "`+state+`",
				"sha": "head-sha",
				"author": {"username": "author"},
				`+terminalFields+`
				"created_at": "2026-05-01T09:00:00Z",
				"updated_at": "`+time.Now().UTC().Add(time.Minute).Format(time.RFC3339)+`"
			}`)
		case path == "/api/v4/projects/4242/merge_requests/7/discussions" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[{
				"id": "`+gitlabMutationThreadID+`",
				"notes": [{
					"id": 9100,
					"body": "ship it",
					"author": {"username": "ada"},
					"created_at": "2026-06-01T10:00:00Z"
				}]
			}]`)
		case path == "/api/v4/projects/4242/merge_requests/7/commits" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case path == "/api/v4/projects/4242/pipelines" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `[]`)
		case (path == "/api/v4/projects/4242/merge_requests/7/notes" ||
			path == "/api/v4/projects/acme%2Fwidget/merge_requests/7/notes") &&
			r.Method == http.MethodPost:
			writeGitLabJSON(w, `{
				"id": 9100,
				"body": "from e2e",
				"author": {"username": "ada"},
				"created_at": "2026-06-01T10:00:00Z"
			}`)
		case (path == "/api/v4/projects/4242/merge_requests/7/draft_notes" ||
			path == "/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes") &&
			r.Method == http.MethodPost:
			writeGitLabJSON(w, `{
				"id": 55,
				"note": "draft note",
				"author": {"username": "ada"},
				"created_at": "2026-06-01T10:00:00Z"
			}`)
		case (path == "/api/v4/projects/4242/merge_requests/7/draft_notes/55/publish" ||
			path == "/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes/55/publish") &&
			r.Method == http.MethodPut:
			writeGitLabJSON(w, `{}`)
		case path == "/api/v4/projects/4242/merge_requests/7/notes/9001" && r.Method == http.MethodPut:
			writeGitLabJSON(w, `{
				"id": 9001,
				"body": "edited body",
				"author": {"username": "ada"},
				"created_at": "2026-05-01T10:00:00Z"
			}`)
		case path == "/api/v4/projects/4242/merge_requests/7/merge" && r.Method == http.MethodPut:
			// Magic marker sent via commit_message by tests that need a
			// non-stale provider conflict from this fake.
			if strings.Contains(request.Body, "force-generic-conflict") {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "merge request is not mergeable"}`)
				return
			}
			if !strings.Contains(request.Body, `"sha":"head-sha"`) {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "SHA does not match HEAD of source branch"}`)
				return
			}
			writeGitLabJSON(w, `{
				"id": 7001, "iid": 7, "state": "merged",
				"squash_commit_sha": "squash-sha", "sha": "head-sha"
			}`)
		case path == "/api/v4/projects/4242/merge_requests/7" && r.Method == http.MethodPut:
			if strings.Contains(request.Body, "state_event") {
				// The real provider returns the full updated MR with fresh
				// timestamps; the handler commits this snapshot, and a
				// zero updated_at would be rejected as stale.
				writeGitLabJSON(w, `{
					"id": 7001, "iid": 7, "title": "Test MR", "state": "closed",
					"sha": "head-sha",
					"author": {"username": "author"},
					"closed_at": "2026-06-01T11:00:00Z",
					"created_at": "2026-05-01T09:00:00Z",
					"updated_at": "`+time.Now().UTC().Add(time.Minute).Format(time.RFC3339)+`"
				}`)
				return
			}
			writeGitLabJSON(w, `{
				"id": 7001, "iid": 7, "title": "Test MR",
				"description": "Updated MR body", "state": "opened"
			}`)
		case (path == "/api/v4/projects/4242/merge_requests/7/approve" ||
			path == "/api/v4/projects/acme%2Fwidget/merge_requests/7/approve") &&
			r.Method == http.MethodPost:
			// Magic note marker for tests that need the approval to go
			// stale only after the note posted (a push landing between
			// the pre-check and the approvals call).
			note, noteOK := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
			if !noteOK {
				note, noteOK = recorder.find(http.MethodPost, "/api/v4/projects/acme%2Fwidget/merge_requests/7/notes")
			}
			if noteOK && strings.Contains(note.Body, "force-stale-after-note") {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "SHA does not match HEAD of source branch"}`)
				return
			}
			_, draftPublished := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/draft_notes/55/publish")
			if !draftPublished {
				_, draftPublished = recorder.find(
					http.MethodPut,
					"/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes/55/publish",
				)
			}
			if draftPublished {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "SHA does not match HEAD of source branch"}`)
				return
			}
			if !strings.Contains(request.Body, `"sha":"head-sha"`) {
				w.WriteHeader(http.StatusConflict)
				writeGitLabJSON(w, `{"message": "SHA does not match HEAD of source branch"}`)
				return
			}
			writeGitLabJSON(w, `{"approved": true, "updated_at": "2026-06-01T11:00:00Z"}`)
		case path == "/api/v4/user" && r.Method == http.MethodGet:
			writeGitLabJSON(w, `{"id": 1, "username": "ada"}`)
		case path == "/api/v4/projects/4242/merge_requests/7/discussions/"+gitlabMutationThreadID+"/notes" &&
			r.Method == http.MethodPost:
			writeGitLabJSON(w, `{
				"id": 9200,
				"body": "thread reply",
				"author": {"username": "ada"},
				"created_at": "2026-06-01T12:00:00Z",
				"resolvable": true
			}`)
		case path == "/api/v4/projects/4242/merge_requests/7/discussions/"+gitlabMutationThreadID &&
			r.Method == http.MethodPut:
			writeGitLabJSON(w, `{"id": "`+gitlabMutationThreadID+`", "notes": []}`)
		case path == "/api/v4/projects/4242/issues" && r.Method == http.MethodPost:
			writeGitLabJSON(w, `{
				"id": 8002, "iid": 12,
				"title": "Created issue", "description": "Issue body",
				"state": "opened",
				"web_url": "https://gitlab.com/acme/widget/-/issues/12"
			}`)
		case path == "/api/v4/projects/4242/issues/11/notes" && r.Method == http.MethodPost:
			writeGitLabJSON(w, `{
				"id": 9300,
				"body": "issue comment",
				"author": {"username": "ada"},
				"created_at": "2026-06-01T13:00:00Z"
			}`)
		case path == "/api/v4/projects/4242/issues/11" && r.Method == http.MethodPut:
			if strings.Contains(request.Body, "state_event") {
				writeGitLabJSON(w, `{"id": 8001, "iid": 11, "title": "Issue", "state": "closed"}`)
				return
			}
			writeGitLabJSON(w, `{"id": 8001, "iid": 11, "title": "Issue (edited)", "state": "opened"}`)
		case path == "/api/v4/projects/4242/issues/11/notes/9301" && r.Method == http.MethodPut:
			writeGitLabJSON(w, `{
				"id": 9301,
				"body": "edited issue comment",
				"author": {"username": "ada"},
				"created_at": "2026-05-01T13:00:00Z"
			}`)
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
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: "4242",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:           "Test MR",
		Author:          "author",
		State:           "open",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	require.NoError(database.UpdateDiffSHAs(ctx, repoID, 7, "head-sha", "base-sha", "merge-base"))

	existingNoteID := int64(9001)
	threadID := gitlabMutationThreadID
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &existingNoteID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "original body",
		CreatedAt:      now,
		DedupeKey:      "gitlab:gitlab.com:acme/widget:mr:7:note:9001",
		ThreadID:       &threadID,
		Resolvable:     true,
	}}))

	issueID, err := database.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     8001,
		Number:         11,
		URL:            "https://gitlab.com/acme/widget/-/issues/11",
		Title:          "Issue",
		Author:         "author",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	existingIssueNoteID := int64(9301)
	require.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{{
		IssueID:    issueID,
		PlatformID: &existingIssueNoteID,
		EventType:  "issue_comment",
		Author:     "reviewer",
		Body:       "original issue comment",
		CreatedAt:  now,
		DedupeKey:  "gitlab:gitlab.com:acme/widget:issue:11:note:9301",
	}}))

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     4242,
		PlatformExternalID: "4242",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	return srv, database, recorder, repoID
}

func writeGitLabJSON(w http.ResponseWriter, body string) {
	_, _ = io.WriteString(w, body)
}

// upsertGitLabTestMR rewrites MR 7's row with the given local head SHA,
// modelling rows reviewed at older heads ("stale-sha") or rows synced
// before head tracking existed ("").
func upsertGitLabTestMR(
	t *testing.T,
	ctx context.Context,
	database *db.DB,
	repoID int64,
	headSHA string,
) {
	t.Helper()
	_, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:           "Test MR",
		Author:          "author",
		State:           "open",
		PlatformHeadSHA: headSHA,
		PlatformBaseSHA: "base-sha",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
		LastActivityAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	if headSHA != "" {
		require.NoError(t, database.UpdateDiffSHAs(ctx, repoID, 7, headSHA, "base-sha", "merge-base"))
	} else {
		require.NoError(t, database.UpdateDiffSHAs(ctx, repoID, 7, "", "", ""))
	}
}

func doGitLabJSON(
	t *testing.T,
	srv *server.Server,
	method, path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func assertNoGitLabRepoPathLookup(t *testing.T, recorder *gitlabAPIRecorder) {
	t.Helper()
	_, lookedUp := recorder.find(http.MethodGet, "/api/v4/projects/acme%2Fwidget")
	assert.False(t, lookedUp, "GitLab mutations must use the stored project id without path lookup")
}

func TestGitLabMutationCommentPostAndEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/comments",
		`{"body":"from e2e"}`,
	)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	created, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
	require.True(ok, "fake GitLab API did not receive note creation")
	assert.Contains(created.Body, `"body":"from e2e"`)

	var postResult struct {
		Author string `json:"Author"`
		Body   string `json:"Body"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&postResult))
	assert.Equal("ada", postResult.Author)
	assert.Equal("from e2e", postResult.Body)

	rr = doGitLabJSON(t, srv, http.MethodPatch,
		"/api/v1/pulls/gitlab/acme/widget/7/comments/9001",
		`{"body":"edited body"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	edited, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/notes/9001")
	require.True(ok, "fake GitLab API did not receive note edit")
	assert.Contains(edited.Body, `"body":"edited body"`)

	var editResult struct {
		Body string `json:"Body"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&editResult))
	assert.Equal("edited body", editResult.Body)

	// GitLab note responses omit the discussion id; the edit must not
	// detach the stored comment from its thread.
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	threadPreserved := false
	for _, event := range events {
		if event.PlatformID != nil && *event.PlatformID == 9001 {
			assert.Equal("edited body", event.Body)
			require.NotNil(event.ThreadID, "thread_id must survive a comment edit")
			assert.Equal(gitlabMutationThreadID, *event.ThreadID)
			threadPreserved = true
		}
	}
	assert.True(threadPreserved, "edited comment event not found")
}

func TestGitLabMutationContentEdits(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPatch,
		"/api/v1/pulls/gitlab/acme/widget/7",
		`{"body":"Updated MR body"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	mrEdit, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7")
	require.True(ok, "fake GitLab API did not receive MR content edit")
	assert.Contains(mrEdit.Body, `"description":"Updated MR body"`)
	var prDetail struct {
		MergeRequest struct {
			Body string `json:"Body"`
		} `json:"merge_request"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&prDetail))
	assert.Equal("Updated MR body", prDetail.MergeRequest.Body)

	rr = doGitLabJSON(t, srv, http.MethodPatch,
		"/api/v1/issues/gitlab/acme/widget/11",
		`{"title":"Issue (edited)"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	issueEdit, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/issues/11")
	require.True(ok, "fake GitLab API did not receive issue content edit")
	assert.Contains(issueEdit.Body, `"title":"Issue (edited)"`)
}

func TestGitLabMutationIssueCommentEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPatch,
		"/api/v1/issues/gitlab/acme/widget/11/comments/9301",
		`{"body":"edited issue comment"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	edited, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/issues/11/notes/9301")
	require.True(ok, "fake GitLab API did not receive issue note edit")
	assert.Contains(edited.Body, `"body":"edited issue comment"`)
}

func TestGitLabMutationMergeSquash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/merge",
		`{"method":"squash","commit_title":"Squash title","commit_message":"Squash body","expected_head_sha":"head-sha"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var result struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&result))
	assert.True(result.Merged)
	assert.Equal("squash-sha", result.SHA)

	merge, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/merge")
	require.True(ok, "fake GitLab API did not receive merge")
	assert.Contains(merge.Body, `"squash":true`)
	assert.Contains(merge.Body, `"squash_commit_message":"Squash title\n\nSquash body"`)
	assertNoGitLabRepoPathLookup(t, recorder)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("merged", string(mr.State))
}

func TestGitLabMutationMergeRebaseReturnsTypedCapabilityError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/merge",
		`{"method":"rebase","commit_title":"t","commit_message":"m","expected_head_sha":"head-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())

	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("unsupportedCapability", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("merge_method_rebase", problem.Details["capability"])
	assert.Equal("gitlab", problem.Details["provider"])

	_, merged := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/merge")
	assert.False(merged, "rebase must not reach the GitLab merge API")
}

func TestGitLabMutationStateChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/github-state",
		`{"state":"closed"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	update, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7")
	require.True(ok, "fake GitLab API did not receive MR update")
	assert.Contains(update.Body, `"state_event":"close"`)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("closed", string(mr.State))
}

func TestGitLabMutationIssueStateChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/issues/gitlab/acme/widget/11/github-state",
		`{"state":"closed"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	update, ok := recorder.find(http.MethodPut, "/api/v4/projects/4242/issues/11")
	require.True(ok, "fake GitLab API did not receive issue update")
	assert.Contains(update.Body, `"state_event":"close"`)
}

func TestGitLabMutationApprove(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/approve",
		`{"body":"ship it","expected_head_sha":"head-sha"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var result struct {
		Status string `json:"status"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&result))
	assert.Equal("approved", result.Status)

	_, approved := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	assert.True(approved, "fake GitLab API did not receive approval")
	note, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
	require.True(ok, "approval body was not posted as a note")
	assert.Contains(note.Body, `"body":"ship it"`)
	assertNoGitLabRepoPathLookup(t, recorder)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	reviewSeen := false
	commentSeen := false
	for _, event := range events {
		if event.EventType == "review" {
			reviewSeen = true
			assert.Equal("ada", event.Author)
			assert.Equal("approved", event.Summary)
		}
		if event.EventType == "issue_comment" && event.Body == "ship it" {
			commentSeen = true
		}
	}
	assert.True(reviewSeen, "approval event was not persisted")
	// The approval body lives in an upstream note; the inline sync after
	// approve must make it visible locally right away.
	assert.True(commentSeen, "approval comment was not synced into local events")
}

func TestGitLabMutationApproveAllowsOmittedHeadPin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/approve",
		`{"body":"ship it"}`,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	approve, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	require.True(ok, "fake GitLab API did not receive approval")
	assert.Contains(approve.Body, `"sha":"head-sha"`)
}

func TestGitLabMutationApproveStaleHeadReturnsConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	// The local head is behind the provider head served by the fake API,
	// mimicking a source-branch push after the user reviewed.
	upsertGitLabTestMR(t, ctx, database, repoID, "stale-sha")

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/approve",
		`{"body":"ship it","expected_head_sha":"stale-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"])

	_, noted := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
	assert.False(noted, "stale approval must not post the comment")
	_, approved := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	assert.False(approved, "stale approval must not reach the approvals API")
}

func TestGitLabMutationMergeStaleHeadReturnsConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	upsertGitLabTestMR(t, ctx, database, repoID, "stale-sha")

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/merge",
		`{"method":"squash","commit_title":"t","commit_message":"m","expected_head_sha":"stale-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"])

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("open", string(mr.State), "stale merge must not mark the MR merged locally")

	assert.True(
		recorder.findEventually(http.MethodGet, "/api/v4/projects/4242/merge_requests/7"),
		"stale merge must trigger an MR resync",
	)
}

// When the head goes stale only after the approval note posted (a push
// landing between the pre-check and the sha-bound approvals call), the
// 409 must tell the client the note side effect survived: a blind retry
// repeats the comment.
func TestGitLabMutationStaleApproveAfterNoteSurfacesContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/approve",
		`{"body":"force-stale-after-note","expected_head_sha":"head-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Detail  string         `json:"detail"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"])
	assert.Equal("the review comment was already posted; retrying will repeat it",
		problem.Details["context"],
		"the note side effect must survive problem mapping")
	assert.Contains(problem.Detail, "retrying will repeat it")

	_, noted := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
	assert.True(noted, "this scenario posts the note before the approval fails")
}

func TestGitLabMutationReviewDraftPartialStaleApproveCleansPublishedComment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	draft, err := database.GetOrCreateMRReviewDraft(ctx, mr.ID)
	require.NoError(err)
	line := 42
	_, err = database.CreateMRReviewDraftComment(ctx, draft.ID, db.MRReviewDraftCommentInput{
		Body: "ready to approve",
		Range: db.ReviewLineRange{
			Path:        "internal/server/e2etest/gitlab_mutations_test.go",
			Side:        "right",
			Line:        42,
			NewLine:     &line,
			LineType:    "add",
			DiffHeadSHA: "head-sha",
		},
	})
	require.NoError(err)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/review-draft/publish",
		`{"action":"approve","body":"summary note"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"])
	assert.Equal(true, problem.Details["partialPublish"])
	assert.EqualValues(1, problem.Details["publishedCommentCount"])
	assert.Equal("the review summary was already posted; retrying will repeat it", problem.Details["context"])

	_, created := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/draft_notes")
	if !created {
		_, created = recorder.find(http.MethodPost, "/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes")
	}
	assert.True(created, "draft comment was not sent to GitLab")
	_, published := recorder.find(http.MethodPut, "/api/v4/projects/4242/merge_requests/7/draft_notes/55/publish")
	if !published {
		_, published = recorder.find(http.MethodPut, "/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes/55/publish")
	}
	assert.True(published, "draft comment was not published before the stale approval")
	_, approved := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	if !approved {
		_, approved = recorder.find(http.MethodPost, "/api/v4/projects/acme%2Fwidget/merge_requests/7/approve")
	}
	assert.True(approved, "stale approval path should still reach the sha-bound approvals API after publishing")
	storedDraft, err := database.GetMRReviewDraft(ctx, mr.ID)
	require.NoError(err)
	assert.Nil(storedDraft, "published local draft comment should be removed after partial publish")
}

// A review-draft APPROVE publish is head-bound on GitLab: when the
// reviewed diff snapshot is stale because the base SHA moved while the
// head stayed the same, the per-comment head check still matches, so
// the publish must instead clear the same reviewedHeadSHA gate as
// /approve and /merge and fail closed before any provider draft note
// or approval is sent.
func TestGitLabReviewDraftApproveRejectsStaleBaseSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	draft, err := database.GetOrCreateMRReviewDraft(ctx, mr.ID)
	require.NoError(err)
	line := 42
	_, err = database.CreateMRReviewDraftComment(ctx, draft.ID, db.MRReviewDraftCommentInput{
		Body: "ready to approve",
		Range: db.ReviewLineRange{
			Path:        "internal/server/e2etest/gitlab_mutations_test.go",
			Side:        "right",
			Line:        42,
			NewLine:     &line,
			LineType:    "add",
			DiffHeadSHA: "head-sha",
		},
	})
	require.NoError(err)

	// Move the platform base SHA past the reviewed snapshot while the
	// head stays "head-sha": diffSnapshotStale is now true even though
	// every draft comment's DiffHeadSHA still equals the head.
	require.NoError(database.UpdatePlatformSHAs(ctx, repoID, 7, "head-sha", "base-sha-moved"))

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/review-draft/publish",
		`{"action":"approve","body":"summary note"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"],
		"a moved base SHA must reject the approve publish as stale")

	// Nothing should have reached the provider: no draft note created,
	// no approval submitted.
	_, draftAt4242 := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/draft_notes")
	_, draftAtPath := recorder.find(http.MethodPost, "/api/v4/projects/acme%2Fwidget/merge_requests/7/draft_notes")
	assert.False(draftAt4242 || draftAtPath, "stale approve publish must not create draft notes")
	_, approveAt4242 := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	_, approveAtPath := recorder.find(http.MethodPost, "/api/v4/projects/acme%2Fwidget/merge_requests/7/approve")
	assert.False(approveAt4242 || approveAtPath, "stale approve publish must not approve")
}

// A body-less stale approval has no note to protect, so it relies solely
// on GitLab's sha-bound rejection: the approval request must carry the
// stale stored head, surface as a stale_state conflict, persist nothing
// locally, and trigger the re-review resync.
func TestGitLabMutationBodylessStaleApproveReliesOnShaBinding(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	upsertGitLabTestMR(t, ctx, database, repoID, "stale-sha")

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/approve",
		`{"body":"","expected_head_sha":"stale-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("stale_state", problem.Details["reason"])

	approve, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/approve")
	require.True(ok, "body-less approval must reach the sha-bound approvals API")
	assert.Contains(approve.Body, `"sha":"stale-sha"`, "approval must be bound to the stored head")
	_, noted := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
	assert.False(noted)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	for _, event := range events {
		assert.NotEqual("review", event.EventType, "stale approval must not persist a review event")
	}

	assert.True(
		recorder.findEventually(http.MethodGet, "/api/v4/projects/4242/merge_requests/7"),
		"stale approval must trigger an MR resync",
	)
}

// GitLab projects with squash_option=always must stop offering the
// non-squash accept path all the way through sync, SQLite, and the repo
// settings API the merge modal reads.
func TestGitLabSquashAlwaysProjectDisallowsMergeCommitE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.EscapedPath() == "/api/v4/projects/acme%2Fsquashy" && r.Method == http.MethodGet {
			writeGitLabJSON(w, `{
				"id": 5151,
				"path": "squashy",
				"path_with_namespace": "acme/squashy",
				"web_url": "https://gitlab.com/acme/squashy",
				"http_url_to_repo": "https://gitlab.com/acme/squashy.git",
				"default_branch": "main",
				"squash_option": "always"
			}`)
			return
		}
		// Every list resource the sync touches is empty.
		writeGitLabJSON(w, `[]`)
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
	repo := ghclient.RepoRef{
		Platform:     platform.KindGitLab,
		Owner:        "acme",
		Name:         "squashy",
		PlatformHost: "gitlab.com",
		RepoPath:     "acme/squashy",
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})

	syncer.RunOnce(ctx)

	rr := doGitLabJSON(t, srv, http.MethodGet, "/api/v1/repo/gitlab/acme/squashy", "")
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var settings struct {
		AllowSquashMerge bool
		AllowMergeCommit bool
		AllowRebaseMerge bool
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&settings))
	assert.True(settings.AllowSquashMerge)
	assert.False(settings.AllowMergeCommit, "squash_option=always must disallow non-squash accepts")
	assert.False(settings.AllowRebaseMerge)
}

// Head-binding providers reject merge requests that omit the client's head
// pin: an omitted pin would silently bind to whatever the cache holds now,
// which may be newer than what the user reviewed.
func TestGitLabMergeOmittedHeadPinRejected(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		mutationPath string
	}{
		{
			name:         "merge",
			path:         "/api/v1/pulls/gitlab/acme/widget/7/merge",
			body:         `{"method":"squash","commit_title":"t","commit_message":"m"}`,
			mutationPath: "/api/v4/projects/4242/merge_requests/7/merge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, _, recorder, _ := setupGitLabMutationServer(t)

			rr := doGitLabJSON(t, srv, http.MethodPost, tt.path, tt.body)
			require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
			var problem struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal("validationError", problem.Code)
			require.NotNil(problem.Details)
			assert.Equal("body.expected_head_sha", problem.Details["field"])

			_, mutated := recorder.find(http.MethodPut, tt.mutationPath)
			if !mutated {
				_, mutated = recorder.find(http.MethodPost, tt.mutationPath)
			}
			assert.False(mutated, "omitted pin must not reach the provider")
		})
	}
}

// Clients can only echo expected_head_sha if the responses they render
// actually carry the head: both the list row and the detail object must
// expose platform_head_sha.
func TestGitLabMutationResponsesExposeHeadSHA(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, _, _ := setupGitLabMutationServer(t)

	detailRR := doGitLabJSON(t, srv, http.MethodGet, "/api/v1/pulls/gitlab/acme/widget/7", "")
	require.Equal(http.StatusOK, detailRR.Code, detailRR.Body.String())
	var detail struct {
		MergeRequest struct {
			PlatformHeadSHA string `json:"platform_head_sha"`
		} `json:"merge_request"`
	}
	require.NoError(json.NewDecoder(detailRR.Body).Decode(&detail))
	assert.Equal("head-sha", detail.MergeRequest.PlatformHeadSHA)

	listRR := doGitLabJSON(t, srv, http.MethodGet, "/api/v1/pulls", "")
	require.Equal(http.StatusOK, listRR.Code, listRR.Body.String())
	var list []struct {
		PlatformHeadSHA string `json:"platform_head_sha"`
	}
	require.NoError(json.NewDecoder(listRR.Body).Decode(&list))
	require.Len(list, 1)
	assert.Equal("head-sha", list[0].PlatformHeadSHA)
}

// The stored head is only a cache: when a sync moves it between the
// client's render and click, the client's expected_head_sha assertion
// must reject merge before any provider call.
func TestGitLabMergeClientExpectedHeadMismatchFailsClosed(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		mutationPath string
	}{
		{
			name:         "merge",
			path:         "/api/v1/pulls/gitlab/acme/widget/7/merge",
			body:         `{"method":"squash","commit_title":"t","commit_message":"m","expected_head_sha":"reviewed-old-head"}`,
			mutationPath: "/api/v4/projects/4242/merge_requests/7/merge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, _, recorder, _ := setupGitLabMutationServer(t)

			// The stored head ("head-sha", seeded by the fixture) advanced
			// past what the client rendered ("reviewed-old-head").
			rr := doGitLabJSON(t, srv, http.MethodPost, tt.path, tt.body)
			require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
			var problem struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal("conflict", problem.Code)
			require.NotNil(problem.Details)
			assert.Equal("stale_state", problem.Details["reason"])

			_, mutated := recorder.find(http.MethodPut, tt.mutationPath)
			if !mutated {
				_, mutated = recorder.find(http.MethodPost, tt.mutationPath)
			}
			assert.False(mutated, "client head mismatch must not reach the provider")
			_, noted := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
			assert.False(noted)
		})
	}
}

// A generic provider conflict on a head-binding provider must not resync:
// persisting a newer head would let a retry from the same stale UI merge
// a commit nobody reviewed.
func TestGitLabMutationGenericMergeConflictDoesNotResync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/merge",
		`{"method":"merge","commit_title":"t","commit_message":"force-generic-conflict","expected_head_sha":"head-sha"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("conflict", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("conflict", problem.Details["reason"])

	// Give a wrongly-scheduled background sync time to surface.
	time.Sleep(300 * time.Millisecond)
	_, synced := recorder.find(http.MethodGet, "/api/v4/projects/4242/merge_requests/7")
	assert.False(synced, "generic conflicts must not resync on head-binding providers")
}

// A legacy or partially synced row with no head SHA fails closed for merge:
// the user never reviewed any particular commit, so merge may not proceed.
// The path must not sync either — persisting a fresh head would let an
// immediate retry from the same stale UI mutate a commit nobody reviewed —
// so consecutive requests keep failing identically.
func TestGitLabMergeMissingHeadSHAFailsClosed(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		mutationPath string
	}{
		{
			name:         "merge",
			path:         "/api/v1/pulls/gitlab/acme/widget/7/merge",
			body:         `{"method":"squash","commit_title":"t","commit_message":"m"}`,
			mutationPath: "/api/v4/projects/4242/merge_requests/7/merge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, recorder, repoID := setupGitLabMutationServer(t)
			ctx := t.Context()

			upsertGitLabTestMR(t, ctx, database, repoID, "")

			// Two consecutive requests model a user re-clicking from the
			// same stale UI; both must fail closed identically.
			for attempt := range 2 {
				rr := doGitLabJSON(t, srv, http.MethodPost, tt.path, tt.body)
				require.Equal(http.StatusConflict, rr.Code,
					"attempt %d: %s", attempt+1, rr.Body.String())
				var problem struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				}
				require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
				assert.Equal("conflict", problem.Code)
				require.NotNil(problem.Details)
				assert.Equal("head_unknown", problem.Details["reason"])

				_, synced := recorder.find(http.MethodGet, "/api/v4/projects/4242/merge_requests/7")
				assert.False(synced,
					"missing head must not sync: a persisted fresh head would arm the retry")
				_, mutated := recorder.find(http.MethodPut, tt.mutationPath)
				if !mutated {
					_, mutated = recorder.find(http.MethodPost, tt.mutationPath)
				}
				assert.False(mutated, "missing head SHA must not reach the provider mutation")
				_, noted := recorder.find(http.MethodPost, "/api/v4/projects/4242/merge_requests/7/notes")
				assert.False(noted, "missing head SHA must not post the approval comment")
			}
		})
	}
}

func TestGitLabMutationDiscussionReplyThroughRealClient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, recorder, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+gitlabMutationThreadID+"/reply",
		`{"body":"thread reply"}`,
	)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	reply, ok := recorder.find(
		http.MethodPost,
		"/api/v4/projects/4242/merge_requests/7/discussions/"+gitlabMutationThreadID+"/notes",
	)
	require.True(ok, "fake GitLab API did not receive discussion reply")
	assert.Contains(reply.Body, `"body":"thread reply"`)

	var result struct {
		Body     string  `json:"Body"`
		ThreadID *string `json:"ThreadID"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&result))
	assert.Equal("thread reply", result.Body)
	require.NotNil(result.ThreadID)
	assert.Equal(gitlabMutationThreadID, *result.ThreadID)
}

func TestGitLabMutationDiscussionResolveAndUnresolve(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	resolvePath := "/api/v1/pulls/gitlab/acme/widget/7/discussions/" +
		gitlabMutationThreadID + "/resolve"

	rr := doGitLabJSON(t, srv, http.MethodPost, resolvePath, `{"resolved":true}`)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	update, ok := recorder.find(
		http.MethodPut,
		"/api/v4/projects/4242/merge_requests/7/discussions/"+gitlabMutationThreadID,
	)
	require.True(ok, "fake GitLab API did not receive discussion resolve")
	assert.Contains(update.Body, `"resolved":true`)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	events, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	require.NotEmpty(events)
	resolvedSeen := false
	for _, event := range events {
		if event.ThreadID != nil && *event.ThreadID == gitlabMutationThreadID {
			resolvedSeen = true
			assert.True(event.Resolved)
		}
	}
	assert.True(resolvedSeen, "local discussion events were not marked resolved")

	rr = doGitLabJSON(t, srv, http.MethodPost, resolvePath, `{"resolved":false}`)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	events, err = database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	for _, event := range events {
		if event.ThreadID != nil && *event.ThreadID == gitlabMutationThreadID {
			assert.False(event.Resolved)
		}
	}
}

func TestGitLabMutationRequestChangesUnsupported(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, _, _ := setupGitLabMutationServer(t)

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/review-draft/publish",
		`{"action":"request_changes","body":"needs work"}`,
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())

	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("unsupportedCapability", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("review_action_request_changes", problem.Details["capability"])
	assert.Equal("gitlab", problem.Details["provider"])
	assert.Equal("gitlab.com", problem.Details["platformHost"])
}

func TestGitLabMutationCreateIssueAndComment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, recorder, repoID := setupGitLabMutationServer(t)
	ctx := t.Context()

	rr := doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/issues/gitlab/acme/widget",
		`{"title":"Created issue","body":"Issue body"}`,
	)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	created, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/issues")
	require.True(ok, "fake GitLab API did not receive issue creation")
	assert.Contains(created.Body, `"title":"Created issue"`)
	assert.Contains(created.Body, `"description":"Issue body"`)

	issue, err := database.GetIssueByRepoIDAndNumber(ctx, repoID, 12)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("Created issue", issue.Title)

	rr = doGitLabJSON(t, srv, http.MethodPost,
		"/api/v1/issues/gitlab/acme/widget/11/comments",
		`{"body":"issue comment"}`,
	)
	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())

	comment, ok := recorder.find(http.MethodPost, "/api/v4/projects/4242/issues/11/notes")
	require.True(ok, "fake GitLab API did not receive issue comment")
	assert.Contains(comment.Body, `"body":"issue comment"`)
}
