package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func projectRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindGitLab,
		Host:     "gitlab.example.com",
		Owner:    "group",
		Name:     "project",
		RepoPath: "group/project",
		WebURL:   "https://gitlab.example.com/group/project",
		// Non-zero so projectScopedArg skips the lookup round trip.
		PlatformID: 42,
	}
}

// decodeBody decodes a fake-API request body. It uses assert rather than
// require because it runs inside httptest handler goroutines.
func decodeBody(t *testing.T, r *http.Request, into any) bool {
	t.Helper()
	return assert.NoError(t, json.NewDecoder(r.Body).Decode(into))
}

func TestGitLabCommentMutations(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		method     string
		call       func(*Client) (author, body string, dedupeKey string, directURL string, err error)
		wantDedupe string
		wantURL    string
	}{
		{
			name:   "create merge request comment",
			path:   "/api/v4/projects/42/merge_requests/7/notes",
			method: http.MethodPost,
			call: func(client *Client) (string, string, string, string, error) {
				event, err := client.CreateMergeRequestComment(
					context.Background(), projectRef(), 7, "hello mr",
				)
				return event.Author, event.Body, event.DedupeKey, event.DirectURL, err
			},
			wantDedupe: "gitlab:gitlab.example.com:group/project:mr:7:note:9001",
			wantURL:    "https://gitlab.example.com/group/project/-/merge_requests/7#note_9001",
		},
		{
			name:   "edit merge request comment",
			path:   "/api/v4/projects/42/merge_requests/7/notes/9001",
			method: http.MethodPut,
			call: func(client *Client) (string, string, string, string, error) {
				event, err := client.EditMergeRequestComment(
					context.Background(), projectRef(), 7, 9001, "hello mr",
				)
				return event.Author, event.Body, event.DedupeKey, event.DirectURL, err
			},
			wantDedupe: "gitlab:gitlab.example.com:group/project:mr:7:note:9001",
			wantURL:    "https://gitlab.example.com/group/project/-/merge_requests/7#note_9001",
		},
		{
			name:   "reply to merge request discussion",
			path:   "/api/v4/projects/42/merge_requests/7/discussions/0123456789abcdef0123456789abcdef01234567/notes",
			method: http.MethodPost,
			call: func(client *Client) (string, string, string, string, error) {
				event, err := client.ReplyToThread(
					context.Background(),
					projectRef(),
					7,
					"0123456789abcdef0123456789abcdef01234567",
					"hello mr",
				)
				return event.Author, event.Body, event.DedupeKey, event.DirectURL, err
			},
			wantDedupe: "gitlab:gitlab.example.com:group/project:mr:7:note:9001",
			wantURL:    "https://gitlab.example.com/group/project/-/merge_requests/7#note_9001",
		},
		{
			name:   "create issue comment",
			path:   "/api/v4/projects/42/issues/11/notes",
			method: http.MethodPost,
			call: func(client *Client) (string, string, string, string, error) {
				event, err := client.CreateIssueComment(
					context.Background(), projectRef(), 11, "hello mr",
				)
				return event.Author, event.Body, event.DedupeKey, event.DirectURL, err
			},
			wantDedupe: "gitlab:gitlab.example.com:group/project:issue:11:note:9001",
			wantURL:    "https://gitlab.example.com/group/project/-/issues/11#note_9001",
		},
		{
			name:   "edit issue comment",
			path:   "/api/v4/projects/42/issues/11/notes/9001",
			method: http.MethodPut,
			call: func(client *Client) (string, string, string, string, error) {
				event, err := client.EditIssueComment(
					context.Background(), projectRef(), 11, 9001, "hello mr",
				)
				return event.Author, event.Body, event.DedupeKey, event.DirectURL, err
			},
			wantDedupe: "gitlab:gitlab.example.com:group/project:issue:11:note:9001",
			wantURL:    "https://gitlab.example.com/group/project/-/issues/11#note_9001",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			var sawRequest bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != tt.path {
					http.NotFound(w, r)
					return
				}
				sawRequest = true
				assert.Equal(tt.method, r.Method)
				var payload struct {
					Body string `json:"body"`
				}
				decodeBody(t, r, &payload)
				assert.Equal("hello mr", payload.Body)
				writeJSON(w, `{
					"id": 9001,
					"body": "hello mr",
					"author": {"username": "ada"},
					"created_at": "2026-06-01T10:00:00Z"
				}`)
			}))
			defer server.Close()

			author, body, dedupeKey, directURL, err := tt.call(newTestClient(t, server.URL))
			require.NoError(err)
			require.True(sawRequest)
			assert.Equal("ada", author)
			assert.Equal("hello mr", body)
			assert.Equal(tt.wantDedupe, dedupeKey)
			assert.Equal(tt.wantURL, directURL)
		})
	}
}

func TestGitLabDeleteCommentMutations(t *testing.T) {
	tests := []struct {
		name string
		path string
		call func(*Client) error
	}{
		{
			name: "merge request comment",
			path: "/api/v4/projects/42/merge_requests/7/notes/9001",
			call: func(client *Client) error {
				return client.DeleteMergeRequestComment(context.Background(), projectRef(), 7, 9001)
			},
		},
		{
			name: "issue comment",
			path: "/api/v4/projects/42/issues/11/notes/9001",
			call: func(client *Client) error {
				return client.DeleteIssueComment(context.Background(), projectRef(), 11, 9001)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(http.MethodDelete, r.Method)
				assert.Equal(tt.path, r.URL.EscapedPath())
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			assert.NoError(tt.call(newTestClient(t, server.URL)))
		})
	}
}

func TestGitLabSetMergeRequestStateSendsStateEvent(t *testing.T) {
	tests := []struct {
		state          string
		wantStateEvent string
		responseState  string
		wantState      string
	}{
		{state: "closed", wantStateEvent: "close", responseState: "closed", wantState: "closed"},
		{state: "open", wantStateEvent: "reopen", responseState: "opened", wantState: "open"},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/api/v4/projects/42/merge_requests/7" {
					http.NotFound(w, r)
					return
				}
				assert.Equal(http.MethodPut, r.Method)
				var payload struct {
					StateEvent string `json:"state_event"`
				}
				decodeBody(t, r, &payload)
				assert.Equal(tt.wantStateEvent, payload.StateEvent)
				writeJSON(w, `{"id": 7001, "iid": 7, "title": "MR", "state": "`+tt.responseState+`"}`)
			}))
			defer server.Close()

			mr, err := newTestClient(t, server.URL).SetMergeRequestState(
				context.Background(), projectRef(), 7, tt.state,
			)
			require.NoError(err)
			assert.Equal(tt.wantState, mr.State)
			assert.Equal(7, mr.Number)
		})
	}
}

func TestGitLabSetIssueStateSendsStateEvent(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/42/issues/11" {
			http.NotFound(w, r)
			return
		}
		assert.Equal(http.MethodPut, r.Method)
		var payload struct {
			StateEvent string `json:"state_event"`
		}
		decodeBody(t, r, &payload)
		assert.Equal("close", payload.StateEvent)
		writeJSON(w, `{"id": 8001, "iid": 11, "title": "Bug", "state": "closed"}`)
	}))
	defer server.Close()

	issue, err := newTestClient(t, server.URL).SetIssueState(
		context.Background(), projectRef(), 11, "closed",
	)
	require.NoError(err)
	assert.Equal("closed", issue.State)
	assert.Equal(11, issue.Number)
}

func TestGitLabSetStateRejectsUnknownState(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	client, err := NewClient("gitlab.example.com", testTokenSource("token"), WithTransport(http.DefaultTransport))
	require.NoError(err)

	var platformErr *platform.Error
	_, err = client.SetMergeRequestState(context.Background(), projectRef(), 7, "merged")
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeInvalidRepoRef, platformErr.Code)
	assert.Equal("state", platformErr.Field)

	_, err = client.SetIssueState(context.Background(), projectRef(), 11, "locked")
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeInvalidRepoRef, platformErr.Code)
}

func TestGitLabMergeMergeRequestMapsMethods(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		wantSquash  bool
		wantMessage map[string]string
		response    string
		wantSHA     string
	}{
		{
			name:       "squash uses squash flag and squash message",
			method:     "squash",
			wantSquash: true,
			wantMessage: map[string]string{
				"squash_commit_message": "Squash title\n\nSquash body",
			},
			response: `{
				"id": 7001, "iid": 7, "state": "merged",
				"squash_commit_sha": "squash-sha", "sha": "head-sha"
			}`,
			wantSHA: "squash-sha",
		},
		{
			name:       "merge keeps squash off and sets merge message",
			method:     "merge",
			wantSquash: false,
			wantMessage: map[string]string{
				"merge_commit_message": "Squash title\n\nSquash body",
			},
			response: `{
				"id": 7001, "iid": 7, "state": "merged",
				"merge_commit_sha": "merge-sha", "sha": "head-sha"
			}`,
			wantSHA: "merge-sha",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/api/v4/projects/42/merge_requests/7/merge" {
					http.NotFound(w, r)
					return
				}
				assert.Equal(http.MethodPut, r.Method)
				var payload map[string]any
				decodeBody(t, r, &payload)
				assert.Equal(tt.wantSquash, payload["squash"])
				assert.Equal("reviewed-head", payload["sha"])
				for key, want := range tt.wantMessage {
					assert.Equal(want, payload[key])
				}
				writeJSON(w, tt.response)
			}))
			defer server.Close()

			result, err := newTestClient(t, server.URL).MergeMergeRequest(
				context.Background(), projectRef(), 7,
				"Squash title", "Squash body", tt.method, "reviewed-head",
			)
			require.NoError(err)
			assert.True(result.Merged)
			assert.Equal(tt.wantSHA, result.SHA)
		})
	}
}

func TestGitLabMergeMergeRequestClassifies409s(t *testing.T) {
	tests := []struct {
		name            string
		expectedHeadSHA string
		message         string
		wantCode        platform.PlatformErrorCode
	}{
		{
			name:            "sha-bound head mismatch is stale",
			expectedHeadSHA: "old-head",
			message:         "SHA does not match HEAD of source branch",
			wantCode:        platform.ErrCodeStaleState,
		},
		{
			name:            "sha-bound but unrelated conflict stays a conflict",
			expectedHeadSHA: "old-head",
			message:         "merge request is not mergeable",
			wantCode:        platform.ErrCodeConflict,
		},
		{
			name:            "sha mentioned without head mismatch stays a conflict",
			expectedHeadSHA: "old-head",
			message:         "commit sha was rejected by a push rule",
			wantCode:        platform.ErrCodeConflict,
		},
		{
			name:            "unbound 409 stays a conflict",
			expectedHeadSHA: "",
			message:         "SHA does not match HEAD of source branch",
			wantCode:        platform.ErrCodeConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/api/v4/projects/42/merge_requests/7/merge" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message": "` + tt.message + `"}`))
			}))
			defer server.Close()

			_, err := newTestClient(t, server.URL).MergeMergeRequest(
				context.Background(), projectRef(), 7,
				"title", "message", "squash", tt.expectedHeadSHA,
			)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(tt.wantCode, platformErr.Code)
			assert.Equal("merge_merge_request", platformErr.Capability)
			assert.Equal("gitlab.example.com", platformErr.PlatformHost)
		})
	}
}

func TestGitLabMergeMergeRequestRejectsRebaseWithTypedError(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	// No fake server: rebase must fail before any API call because GitLab
	// selects rebase/fast-forward behavior via project settings, not per merge.
	client, err := NewClient("gitlab.example.com", testTokenSource("token"), WithTransport(http.DefaultTransport))
	require.NoError(err)

	_, err = client.MergeMergeRequest(
		context.Background(), projectRef(), 7, "title", "message", "rebase", "",
	)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal(platform.KindGitLab, platformErr.Provider)
	assert.Equal("merge_method_rebase", platformErr.Capability)
}

func TestGitLabCreateIssueAndEditContent(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var createSeen, editIssueSeen, editMRSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == "/api/v4/projects/42/issues" && r.Method == http.MethodPost:
			createSeen = true
			var payload struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			}
			decodeBody(t, r, &payload)
			assert.Equal("New issue", payload.Title)
			assert.Equal("Issue body", payload.Description)
			writeJSON(w, `{"id": 8002, "iid": 12, "title": "New issue", "description": "Issue body", "state": "opened"}`)
		case r.URL.EscapedPath() == "/api/v4/projects/42/issues/12" && r.Method == http.MethodPut:
			editIssueSeen = true
			var payload struct {
				Title       *string `json:"title"`
				Description *string `json:"description"`
			}
			decodeBody(t, r, &payload)
			if assert.NotNil(payload.Title) {
				assert.Equal("Edited title", *payload.Title)
			}
			assert.Nil(payload.Description)
			writeJSON(w, `{"id": 8002, "iid": 12, "title": "Edited title", "description": "Issue body", "state": "opened"}`)
		case r.URL.EscapedPath() == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodPut:
			editMRSeen = true
			var payload struct {
				Title       *string `json:"title"`
				Description *string `json:"description"`
			}
			decodeBody(t, r, &payload)
			assert.Nil(payload.Title)
			if assert.NotNil(payload.Description) {
				assert.Equal("Edited MR body", *payload.Description)
			}
			writeJSON(w, `{"id": 7001, "iid": 7, "title": "MR", "description": "Edited MR body", "state": "opened"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)

	issue, err := client.CreateIssue(context.Background(), projectRef(), "New issue", "Issue body")
	require.NoError(err)
	assert.Equal(12, issue.Number)
	assert.Equal("New issue", issue.Title)

	title := "Edited title"
	edited, err := client.EditIssueContent(context.Background(), projectRef(), 12, &title, nil)
	require.NoError(err)
	assert.Equal("Edited title", edited.Title)

	mrBody := "Edited MR body"
	mr, err := client.EditMergeRequestContent(context.Background(), projectRef(), 7, nil, &mrBody)
	require.NoError(err)
	assert.Equal("Edited MR body", mr.Body)

	assert.True(createSeen)
	assert.True(editIssueSeen)
	assert.True(editMRSeen)
}

func TestGitLabApproveMergeRequestPostsNoteAndApproves(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var noteSeen, approveSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/merge_requests/7":
			writeJSON(w, `{"id": 7001, "iid": 7, "state": "opened", "sha": "reviewed-head"}`)
		case "/api/v4/projects/42/merge_requests/7/notes":
			noteSeen = true
			assert.Equal(http.MethodPost, r.Method)
			var payload struct {
				Body string `json:"body"`
			}
			decodeBody(t, r, &payload)
			assert.Equal("ship it", payload.Body)
			writeJSON(w, `{"id": 9002, "body": "ship it", "author": {"username": "ada"}}`)
		case "/api/v4/projects/42/merge_requests/7/approve":
			approveSeen = true
			assert.Equal(http.MethodPost, r.Method)
			var payload struct {
				SHA string `json:"sha"`
			}
			decodeBody(t, r, &payload)
			assert.Equal("reviewed-head", payload.SHA)
			writeJSON(w, `{"approved": true, "updated_at": "2026-06-01T10:00:00Z"}`)
		case "/api/v4/user":
			writeJSON(w, `{"id": 1, "username": "ada"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	event, err := newTestClient(t, server.URL).ApproveMergeRequest(
		context.Background(), projectRef(), 7, " ship it ", "reviewed-head",
	)
	require.NoError(err)
	assert.True(noteSeen)
	assert.True(approveSeen)
	assert.Equal("review", event.EventType)
	assert.Equal("approved", event.Summary)
	assert.Equal("ada", event.Author)
	// The body lives on the posted note, which sync imports as its own
	// comment; keeping it off the approval event avoids duplicate text.
	assert.Empty(event.Body)
	assert.Equal(
		"gitlab:gitlab.example.com:group/project:mr:7:approval:ada",
		event.DedupeKey,
	)
}

func TestGitLabApproveMergeRequestReportsNotePostedWhenApprovalFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var noteSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/merge_requests/7/notes":
			noteSeen = true
			writeJSON(w, `{"id": 9002, "body": "ship it", "author": {"username": "ada"}}`)
		case "/api/v4/projects/42/merge_requests/7/approve":
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := newTestClient(t, server.URL).ApproveMergeRequest(
		context.Background(), projectRef(), 7, "ship it", "",
	)
	require.Error(err)
	assert.True(noteSeen)
	assert.Contains(err.Error(), "review comment was posted but the approval failed")
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodePermissionDenied, platformErr.Code)
	assert.Equal("gitlab.example.com", platformErr.PlatformHost)
}

func TestGitLabApproveMergeRequestSkipsNoteForEmptyBody(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var noteSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/merge_requests/7/notes":
			noteSeen = true
			writeJSON(w, `{"id": 9002}`)
		case "/api/v4/projects/42/merge_requests/7/approve":
			writeJSON(w, `{"approved": true}`)
		case "/api/v4/user":
			// Attribution lookup failures must not fail the approval. A 403
			// keeps the SDK from retrying with backoff the way a 5xx would.
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	event, err := newTestClient(t, server.URL).ApproveMergeRequest(
		context.Background(), projectRef(), 7, "   ", "",
	)
	require.NoError(err)
	assert.False(noteSeen)
	assert.Equal("review", event.EventType)
	assert.Empty(event.Author)
}

func TestGitLabApproveMergeRequestRejectsStaleHeadBeforePostingNote(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var noteSeen, approveSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/merge_requests/7":
			writeJSON(w, `{"id": 7001, "iid": 7, "state": "opened", "sha": "new-head"}`)
		case "/api/v4/projects/42/merge_requests/7/notes":
			noteSeen = true
			writeJSON(w, `{"id": 9002}`)
		case "/api/v4/projects/42/merge_requests/7/approve":
			approveSeen = true
			writeJSON(w, `{"approved": true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := newTestClient(t, server.URL).ApproveMergeRequest(
		context.Background(), projectRef(), 7, "ship it", "reviewed-head",
	)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeStaleState, platformErr.Code)
	assert.Equal("gitlab.example.com", platformErr.PlatformHost)
	assert.False(noteSeen, "stale review must not post the comment")
	assert.False(approveSeen, "stale review must not approve")
}

func TestCombineCommitMessage(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		message string
		want    string
	}{
		{name: "both", title: "Title", message: "Body", want: "Title\n\nBody"},
		{name: "title only", title: " Title ", message: "", want: "Title"},
		{name: "message only", title: "", message: "Body", want: "Body"},
		{name: "empty", title: " ", message: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, combineCommitMessage(tt.title, tt.message))
		})
	}
}
