package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestGitLabPublishDiffReviewDraftCreatesDraftNotesAndApproves(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var createdDrafts int
	var published bool
	var summaryCreated bool
	var approved bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			createdDrafts++
			var body struct {
				Note     string `json:"note"`
				CommitID string `json:"commit_id"`
				Position struct {
					PositionType string `json:"position_type"`
					NewPath      string `json:"new_path"`
					NewLine      int64  `json:"new_line"`
					BaseSHA      string `json:"base_sha"`
					StartSHA     string `json:"start_sha"`
					HeadSHA      string `json:"head_sha"`
				} `json:"position"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("range note", body.Note)
			assert.Equal("head-sha", body.CommitID)
			assert.Equal("text", body.Position.PositionType)
			assert.Equal("src/main.go", body.Position.NewPath)
			assert.Equal(int64(5), body.Position.NewLine)
			assert.Equal("base-sha", body.Position.BaseSHA)
			assert.Equal("merge-base-sha", body.Position.StartSHA)
			assert.Equal("head-sha", body.Position.HeadSHA)
			writeJSON(w, `{"id": 55, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			published = true
			writeJSON(w, `{}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/notes":
			assert.Equal(http.MethodPost, r.Method)
			summaryCreated = true
			var body struct {
				Body string `json:"body"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("review summary", body.Body)
			writeJSON(w, `{"id": 77, "body": "review summary"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			approved = true
			var body struct {
				SHA string `json:"sha"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("head-sha", body.SHA)
			writeJSON(w, `{"approved": true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
		Body:   " review summary ",
		Comments: []platform.LocalDiffReviewDraftComment{{
			Body: "range note",
			Range: platform.DiffReviewLineRange{
				Path:         "src/main.go",
				Side:         "right",
				Line:         5,
				DiffHeadSHA:  "head-sha",
				DiffBaseSHA:  "base-sha",
				MergeBaseSHA: "merge-base-sha",
				CommitSHA:    "head-sha",
			},
		}},
	})

	require.NoError(err)
	require.NotNil(result)
	assert.Equal(1, createdDrafts)
	assert.True(published)
	assert.True(summaryCreated)
	assert.True(approved)
	assert.False(result.SubmittedAt.IsZero())
}

func TestGitLabPublishDiffReviewDraftReturnsPartialErrorWhenApproveFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var published bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			writeJSON(w, `{"id": 55, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			published = true
			writeJSON(w, `{}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			// 422 keeps the failure non-retryable; the SDK retries 5xx with backoff.
			http.Error(w, `{"message": "approval failed"}`, http.StatusUnprocessableEntity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
		Comments: []platform.LocalDiffReviewDraftComment{{
			ID:   123,
			Body: "range note",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				Line:        5,
				DiffHeadSHA: "head-sha",
			},
		}},
	})

	require.Error(err)
	var partialErr *platform.DiffReviewPublishPartialError
	require.ErrorAs(err, &partialErr)
	assert.Equal([]int64{123}, partialErr.PublishedCommentIDs)
	assert.True(published)
}

func TestGitLabPublishDiffReviewDraftReturnsPartialStaleErrorWhenApproveHeadMoves(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var published bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			writeJSON(w, `{"id": 55, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			published = true
			writeJSON(w, `{}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			var body struct {
				SHA string `json:"sha"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("old-head", body.SHA)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message": "SHA does not match HEAD of source branch"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
		Comments: []platform.LocalDiffReviewDraftComment{{
			ID:   123,
			Body: "range note",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				Line:        5,
				DiffHeadSHA: "old-head",
			},
		}},
	})

	require.Error(err)
	require.NotNil(result)
	var partialErr *platform.DiffReviewPublishPartialError
	require.ErrorAs(err, &partialErr)
	assert.Equal([]int64{123}, partialErr.PublishedCommentIDs)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeStaleState, platformErr.Code)
	assert.Equal("approve_merge_request", platformErr.Capability)
	assert.True(published)
	assert.False(result.SubmittedAt.IsZero())
}

func TestGitLabPublishDiffReviewDraftReportsSummaryPostedWhenApproveHeadMoves(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var summaryCreated bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/notes":
			assert.Equal(http.MethodPost, r.Method)
			summaryCreated = true
			writeJSON(w, `{"id": 77, "body": "summary"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			var body struct {
				SHA string `json:"sha"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("old-head", body.SHA)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message": "SHA does not match HEAD of source branch"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action:  platform.ReviewActionApprove,
		Body:    "summary",
		HeadSHA: "old-head",
	})

	require.Error(err)
	require.NotNil(result)
	var partialErr *platform.DiffReviewPublishPartialError
	require.ErrorAs(err, &partialErr)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeStaleState, platformErr.Code)
	assert.Equal("the review summary was already posted; retrying will repeat it", platformErr.Hint)
	assert.True(summaryCreated)
}

func TestGitLabPublishDiffReviewDraftUsesHeadSHAForSummaryApproval(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var approved bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/notes":
			assert.Equal(http.MethodPost, r.Method)
			writeJSON(w, `{"id": 77, "body": "summary"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			approved = true
			var body struct {
				SHA string `json:"sha"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.Equal("review-head", body.SHA)
			writeJSON(w, `{"approved": true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action:  platform.ReviewActionApprove,
		Body:    "summary",
		HeadSHA: "review-head",
	})

	require.NoError(err)
	require.NotNil(result)
	assert.True(approved)
}

func TestGitLabPublishDiffReviewDraftRejectsApprovalWithoutHeadSHA(t *testing.T) {
	require := Require.New(t)
	var calledProvider bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledProvider = true
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
	})

	require.Error(err)
	require.Nil(result)
	assert.New(t).False(calledProvider)
}

func TestGitLabPublishDiffReviewDraftDoesNotApproveWhenPublishFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var approved bool
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			writeJSON(w, `{"id": 55, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			assert.Equal(http.MethodPost, r.Method)
			approved = true
			writeJSON(w, `{"approved": true}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			// 422 keeps the failure non-retryable; the SDK retries 5xx with backoff.
			http.Error(w, `{"message": "publish failed"}`, http.StatusUnprocessableEntity)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55":
			assert.Equal(http.MethodDelete, r.Method)
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
		Comments: []platform.LocalDiffReviewDraftComment{{
			Body: "range note",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				Line:        5,
				DiffHeadSHA: "head-sha",
			},
		}},
	})

	require.Error(err)
	assert.Nil(result)
	assert.False(approved)
	assert.True(deleted)
}

func TestGitLabPublishDiffReviewDraftReturnsNormalErrorWhenFirstPublishCleanupFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			writeJSON(w, `{"id": 55, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			// 422 keeps the failure non-retryable; the SDK retries 5xx with backoff.
			http.Error(w, `{"message": "publish failed"}`, http.StatusUnprocessableEntity)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55":
			assert.Equal(http.MethodDelete, r.Method)
			http.Error(w, `{"message": "delete failed"}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionComment,
		Comments: []platform.LocalDiffReviewDraftComment{{
			Body: "range note",
			Range: platform.DiffReviewLineRange{
				Path:        "src/main.go",
				Side:        "right",
				Line:        5,
				DiffHeadSHA: "head-sha",
			},
		}},
	})

	require.Error(err)
	assert.Nil(result)
	var partialErr *platform.DiffReviewPublishPartialError
	assert.NotErrorAs(err, &partialErr)
	assert.Contains(err.Error(), "cleanup failed")
}

func TestGitLabPublishDiffReviewDraftReturnsPartialErrorAfterSomeDraftsPublish(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var createdDrafts int
	var approved bool
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			createdDrafts++
			writeJSON(w, `{"id": `+strconv.Itoa(54+createdDrafts)+`, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			writeJSON(w, `{}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/56/publish":
			assert.Equal(http.MethodPut, r.Method)
			// 422 keeps the failure non-retryable; the SDK retries 5xx with backoff.
			http.Error(w, `{"message": "publish failed"}`, http.StatusUnprocessableEntity)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/56":
			assert.Equal(http.MethodDelete, r.Method)
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/approve":
			approved = true
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionApprove,
		Comments: []platform.LocalDiffReviewDraftComment{
			{
				Body: "first note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        5,
					DiffHeadSHA: "head-sha",
				},
			},
			{
				Body: "second note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        6,
					DiffHeadSHA: "head-sha",
				},
			},
		},
	})

	require.Error(err)
	require.NotNil(result)
	var partialErr *platform.DiffReviewPublishPartialError
	require.ErrorAs(err, &partialErr)
	assert.Equal(2, createdDrafts)
	assert.False(approved)
	assert.True(deleted)
}

func TestGitLabPublishDiffReviewDraftKeepsPartialErrorWhenRemainingCleanupFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var createdDrafts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			createdDrafts++
			writeJSON(w, `{"id": `+strconv.Itoa(54+createdDrafts)+`, "note": "range note"}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55/publish":
			assert.Equal(http.MethodPut, r.Method)
			writeJSON(w, `{}`)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/56/publish":
			assert.Equal(http.MethodPut, r.Method)
			// 422 keeps the failure non-retryable; the SDK retries 5xx with backoff.
			http.Error(w, `{"message": "publish failed"}`, http.StatusUnprocessableEntity)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/56":
			assert.Equal(http.MethodDelete, r.Method)
			http.Error(w, `{"message": "delete failed"}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionComment,
		Comments: []platform.LocalDiffReviewDraftComment{
			{
				Body: "first note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        5,
					DiffHeadSHA: "head-sha",
				},
			},
			{
				Body: "second note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        6,
					DiffHeadSHA: "head-sha",
				},
			},
		},
	})

	require.Error(err)
	require.NotNil(result)
	var partialErr *platform.DiffReviewPublishPartialError
	require.ErrorAs(err, &partialErr)
	assert.Contains(err.Error(), "cleanup failed")
}

func TestGitLabPublishDiffReviewDraftDeletesCreatedDraftsWhenLaterCreateFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var createAttempts int
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes":
			assert.Equal(http.MethodPost, r.Method)
			createAttempts++
			if createAttempts == 1 {
				writeJSON(w, `{"id": 55, "note": "first note"}`)
				return
			}
			http.Error(w, `{"message": "create failed"}`, http.StatusBadRequest)
		case "/api/v4/projects/group%2Fproject/merge_requests/7/draft_notes/55":
			assert.Equal(http.MethodDelete, r.Method)
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, platform.PublishDiffReviewDraftInput{
		Action: platform.ReviewActionComment,
		Comments: []platform.LocalDiffReviewDraftComment{
			{
				Body: "first note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        5,
					DiffHeadSHA: "head-sha",
				},
			},
			{
				Body: "second note",
				Range: platform.DiffReviewLineRange{
					Path:        "src/main.go",
					Side:        "right",
					Line:        6,
					DiffHeadSHA: "head-sha",
				},
			},
		},
	})

	require.Error(err)
	assert.Nil(result)
	assert.Equal(2, createAttempts)
	assert.True(deleted)
}

func TestGitLabListMergeRequestReviewThreadsReadsDiscussions(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	updated := created.Add(time.Minute)
	// Path-only refs hydrate through the project lookup before the
	// discussions read.
	webURL := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		if r.URL.EscapedPath() == "/api/v4/projects/group%2Fproject" {
			writeJSON(w, `{"id":42,"path_with_namespace":"group/project","web_url":"`+webURL+`"}`)
			return
		}
		assert.Equal("/api/v4/projects/42/merge_requests/7/discussions", r.URL.EscapedPath())
		writeJSON(w, `[
			{
				"id": "discussion-1",
				"individual_note": false,
				"notes": [{
					"id": 101,
					"type": "DiscussionNote",
					"body": "inline note",
					"author": {"username": "reviewer"},
					"system": false,
					"resolvable": true,
					"resolved": true,
					"created_at": "`+created.Format(time.RFC3339)+`",
					"updated_at": "`+updated.Format(time.RFC3339)+`",
					"position": {
						"base_sha": "base",
						"start_sha": "base",
						"head_sha": "head",
						"position_type": "text",
						"new_path": "src/main.go",
						"new_line": 9
					}
				}]
			}
		]`)
	}))
	defer server.Close()
	webURL = server.URL + "/group/project"

	client := newTestClient(t, server.URL)
	threads, err := client.ListMergeRequestReviewThreads(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
		WebURL:   server.URL + "/group/project",
	}, 7)

	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("discussion-1", threads[0].ProviderThreadID)
	assert.Equal("101", threads[0].ProviderCommentID)
	assert.Equal("inline note", threads[0].Body)
	assert.Equal("reviewer", threads[0].AuthorLogin)
	assert.Equal(server.URL+"/group/project/-/merge_requests/7#note_101", threads[0].DirectURL)
	assert.True(threads[0].Resolved)
	assert.Equal("src/main.go", threads[0].Range.Path)
	assert.Equal("right", threads[0].Range.Side)
	assert.Equal(9, threads[0].Range.Line)
	assert.Equal("head", threads[0].Range.DiffHeadSHA)
	assert.Equal("base", threads[0].Range.DiffBaseSHA)
	assert.Equal("base", threads[0].Range.MergeBaseSHA)
	assert.Equal(created, threads[0].CreatedAt)
	assert.Equal(updated, threads[0].UpdatedAt)
	require.NotNil(threads[0].ResolvedAt)
	assert.Equal(updated, *threads[0].ResolvedAt)
}

func TestGitLabListMergeRequestReviewThreadsReadsContextLinePositions(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		if r.URL.EscapedPath() == "/api/v4/projects/group%2Fproject" {
			writeJSON(w, `{"id":42,"path_with_namespace":"group/project"}`)
			return
		}
		assert.Equal("/api/v4/projects/42/merge_requests/7/discussions", r.URL.EscapedPath())
		writeJSON(w, `[
			{
				"id": "discussion-1",
				"individual_note": false,
				"notes": [{
					"id": 101,
					"type": "DiscussionNote",
					"body": "context note",
					"author": {"username": "reviewer"},
					"system": false,
					"resolvable": true,
					"resolved": false,
					"created_at": "`+created.Format(time.RFC3339)+`",
					"updated_at": "`+created.Format(time.RFC3339)+`",
					"position": {
						"base_sha": "base",
						"start_sha": "merge-base",
						"head_sha": "head",
						"position_type": "text",
						"old_path": "src/main.go",
						"new_path": "src/main.go",
						"old_line": 8,
						"new_line": 9
					}
				}]
			}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	threads, err := client.ListMergeRequestReviewThreads(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7)

	require.NoError(err)
	require.Len(threads, 1)
	lineRange := threads[0].Range
	assert.Equal("right", lineRange.Side)
	assert.Equal(9, lineRange.Line)
	assert.Equal("context", lineRange.LineType)
	require.NotNil(lineRange.OldLine)
	require.NotNil(lineRange.NewLine)
	assert.Equal(8, *lineRange.OldLine)
	assert.Equal(9, *lineRange.NewLine)
}

func TestGitLabListMergeRequestReviewThreadsCollapsesDiscussionReplies(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		if r.URL.EscapedPath() == "/api/v4/projects/group%2Fproject" {
			writeJSON(w, `{"id":42,"path_with_namespace":"group/project"}`)
			return
		}
		assert.Equal("/api/v4/projects/42/merge_requests/7/discussions", r.URL.EscapedPath())
		writeJSON(w, `[
			{
				"id": "discussion-1",
				"individual_note": false,
				"notes": [
					{
						"id": 101,
						"type": "DiscussionNote",
						"body": "original inline note",
						"author": {"username": "reviewer"},
						"system": false,
						"resolvable": true,
						"resolved": false,
						"created_at": "`+created.Format(time.RFC3339)+`",
						"updated_at": "`+created.Format(time.RFC3339)+`",
						"position": {
							"base_sha": "base",
							"start_sha": "base",
							"head_sha": "head",
							"position_type": "text",
							"new_path": "src/main.go",
							"new_line": 9
						}
					},
					{
						"id": 102,
						"type": "DiscussionNote",
						"body": "reply should not replace original",
						"author": {"username": "other-reviewer"},
						"system": false,
						"resolvable": true,
						"resolved": false,
						"created_at": "`+created.Add(time.Minute).Format(time.RFC3339)+`",
						"updated_at": "`+created.Add(time.Minute).Format(time.RFC3339)+`",
						"position": {
							"base_sha": "base",
							"start_sha": "base",
							"head_sha": "head",
							"position_type": "text",
							"new_path": "src/main.go",
							"new_line": 10
						}
					}
				]
			}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	threads, err := client.ListMergeRequestReviewThreads(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7)

	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("discussion-1", threads[0].ProviderThreadID)
	assert.Equal("101", threads[0].ProviderCommentID)
	assert.Equal("original inline note", threads[0].Body)
	assert.Equal("reviewer", threads[0].AuthorLogin)
	assert.Equal(9, threads[0].Range.Line)
}

func TestGitLabResolveDiffReviewThreadUpdatesDiscussion(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	discussionID := "0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject":
			assert.Equal(http.MethodGet, r.Method)
			writeJSON(w, `{
				"id": 42,
				"path": "project",
				"path_with_namespace": "group/project",
				"name": "Project"
			}`)
		case "/api/v4/projects/42/merge_requests/7/discussions/" + discussionID:
			assert.Equal(http.MethodPut, r.Method)
			var body struct {
				Resolved bool `json:"resolved"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			assert.True(body.Resolved)
			writeJSON(w, `{"id": "`+discussionID+`", "notes": []}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	err := client.ResolveDiffReviewThread(context.Background(), platform.RepoRef{
		RepoPath: "group/project",
	}, 7, discussionID)

	require.NoError(err)
}
