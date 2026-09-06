package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestForgejoReviewThreadPreservesContextCoordinates(t *testing.T) {
	assert := assert.New(t)
	thread := forgejoReviewThread(
		&forgejosdk.PullReview{ID: 99},
		&forgejosdk.PullReviewComment{
			ID:         101,
			Path:       "src/main.go",
			LineNum:    7,
			OldLineNum: 5,
		},
	)

	assert.Equal("right", thread.Range.Side)
	assert.Equal(7, thread.Range.Line)
	if assert.NotNil(thread.Range.OldLine) {
		assert.Equal(5, *thread.Range.OldLine)
	}
	if assert.NotNil(thread.Range.NewLine) {
		assert.Equal(7, *thread.Range.NewLine)
	}
	assert.Equal("context", thread.Range.LineType)
}

func TestPublishDiffReviewDraftCreatesForgejoReview(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	submitted := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/repos/acme/widgets/pulls/42/reviews", r.URL.Path)
		var body struct {
			Event    string `json:"event"`
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
			Comments []struct {
				Path        string `json:"path"`
				Body        string `json:"body"`
				NewPosition int64  `json:"new_position"`
				OldPosition int64  `json:"old_position"`
			} `json:"comments"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal("REQUEST_CHANGES", body.Event)
		assert.Equal("summary", body.Body)
		assert.Equal("head-sha", body.CommitID)
		if assert.Len(body.Comments, 1) {
			assert.Equal(int64(5), body.Comments[0].NewPosition)
			assert.Zero(body.Comments[0].OldPosition)
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id":           99,
			"state":        "REQUEST_CHANGES",
			"body":         "summary",
			"submitted_at": submitted.Format(time.RFC3339),
			"user":         map[string]any{"login": "reviewer"},
		}))
	}))
	defer server.Close()

	client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		Owner: "acme",
		Name:  "widgets",
	}, 42, platform.PublishDiffReviewDraftInput{
		Body:   "summary",
		Action: platform.ReviewActionRequestChanges,
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

	require.NoError(err)
	require.NotNil(result)
	assert.Equal("99", result.ProviderReviewID)
	assert.Equal(submitted, result.SubmittedAt)
}

func TestPublishDiffReviewDraftApproveSubmitsReview(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	submitted := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/repos/acme/widgets/pulls/42/reviews", r.URL.Path)
		var body struct {
			Event    string `json:"event"`
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal("APPROVED", body.Event)
		assert.Equal("ship it", body.Body)
		assert.Equal("reviewed-head", body.CommitID)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id":           100,
			"state":        "APPROVED",
			"body":         "ship it",
			"submitted_at": submitted.Format(time.RFC3339),
			"user":         map[string]any{"login": "reviewer"},
		}))
	}))
	defer server.Close()

	client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	result, err := client.PublishDiffReviewDraft(context.Background(), platform.RepoRef{
		Owner: "acme",
		Name:  "widgets",
	}, 42, platform.PublishDiffReviewDraftInput{
		Body:    "ship it",
		Action:  platform.ReviewActionApprove,
		HeadSHA: "reviewed-head",
	})

	require.NoError(err)
	require.NotNil(result)
	assert.Equal("100", result.ProviderReviewID)
	assert.Equal(submitted, result.SubmittedAt)
}

func TestRequestChangesMapsNotFoundResponse(t *testing.T) {
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	err = client.RequestChanges(
		context.Background(), platform.RepoRef{Owner: "acme", Name: "widgets"},
		42, "needs work", "reviewed-head",
	)

	require.Error(err)
	require.ErrorIs(err, platform.ErrNotFound)
}

func TestListMergeRequestReviewThreadsReadsForgejoReviewComments(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/pulls/42/reviews":
			assert.Equal(http.MethodGet, r.Method)
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{
				"id":           99,
				"state":        "COMMENT",
				"submitted_at": created.Format(time.RFC3339),
				"user":         map[string]any{"login": "reviewer"},
			}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments":
			assert.Equal(http.MethodGet, r.Method)
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{
				"id":                     101,
				"body":                   "inline note",
				"html_url":               "https://codeberg.test/acme/widgets/pulls/42#issuecomment-101",
				"user":                   map[string]any{"login": "reviewer"},
				"pull_request_review_id": 99,
				"path":                   "src/main.go",
				"commit_id":              "head-sha",
				"position":               7,
				"created_at":             created.Format(time.RFC3339),
				"updated_at":             created.Add(time.Minute).Format(time.RFC3339),
			}}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(context.Background(), platform.RepoRef{
		Owner: "acme",
		Name:  "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("101", threads[0].ProviderThreadID)
	assert.Equal("99", threads[0].ProviderReviewID)
	assert.Equal("inline note", threads[0].Body)
	assert.Equal("reviewer", threads[0].AuthorLogin)
	assert.Equal("https://codeberg.test/acme/widgets/pulls/42#issuecomment-101", threads[0].DirectURL)
	assert.Equal("right", threads[0].Range.Side)
	assert.Equal(7, threads[0].Range.Line)
	assert.Equal("head-sha", threads[0].Range.CommitSHA)
}

func TestListMergeRequestReviewThreadsReadsBeyondTenthReviewPage(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var reviewRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/42/reviews" {
			reviewPath := strings.TrimSuffix(r.URL.Path, "/comments")
			reviewID, err := strconv.Atoi(reviewPath[strings.LastIndexByte(reviewPath, '/')+1:])
			assert.NoError(err)
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 1000 + reviewID}}))
			return
		}
		reviewRequests.Add(1)
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		assert.NoError(err)
		if page < 11 {
			w.Header().Set("Link", fmt.Sprintf(
				`<%s/api/v1/repos/acme/widgets/pulls/42/reviews?page=%d&limit=100>; rel="next"`,
				server.URL, page+1,
			))
		}
		assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": page}}))
	}))
	defer server.Close()
	client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)

	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 11)
	assert.Equal("1", threads[0].ProviderReviewID)
	assert.Equal("11", threads[10].ProviderReviewID)
	assert.Equal(int32(11), reviewRequests.Load())
}

func TestListMergeRequestReviewThreadsClassifiesDisabledMergeRequests(t *testing.T) {
	tests := []struct {
		name       string
		failedPath string
		status     int
	}{
		{
			name:       "list reviews",
			failedPath: "/api/v1/repos/acme/widgets/pulls/42/reviews",
			status:     http.StatusForbidden,
		},
		{
			name:       "list review comments",
			failedPath: "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments",
			status:     http.StatusGone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			metadataRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == tt.failedPath {
					http.Error(w, "pull requests disabled", tt.status)
					return
				}
				switch r.URL.Path {
				case "/api/v1/repos/acme/widgets/pulls/42/reviews":
					assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 99}}))
				case "/api/v1/repos/acme/widgets":
					metadataRequests++
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{
						"id":                1,
						"name":              "widgets",
						"full_name":         "acme/widgets",
						"owner":             map[string]any{"id": 2, "login": "acme"},
						"has_issues":        true,
						"has_pull_requests": false,
					}))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client, err := NewClient("codeberg.test", testTokenSource("token"), WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
			require.NoError(err)
			_, err = client.ListMergeRequestReviewThreads(context.Background(), platform.RepoRef{
				Platform: platform.KindForgejo,
				Host:     "codeberg.test",
				Owner:    "acme",
				Name:     "widgets",
			}, 42)

			require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(platform.RepositoryFeatureMergeRequests, platformErr.Capability)
			assert.Equal(1, metadataRequests)
		})
	}
}

func TestListMergeRequestReviewThreadsMapsAuthenticationErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			require := Require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			defer server.Close()

			client, err := NewClient(
				"codeberg.test", testTokenSource("token"),
				WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
			require.NoError(err)
			_, err = client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
				Owner: "acme", Name: "widgets",
			}, 42)

			if status == http.StatusUnauthorized {
				require.ErrorIs(err, platform.ErrPermissionDenied)
			} else {
				require.ErrorIs(err, platform.ErrPermissionDenied)
			}
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			require.Equal(platform.KindForgejo, platformErr.Provider)
			require.Equal("codeberg.test", platformErr.PlatformHost)
		})
	}
}

func TestListMergeRequestReviewThreadsReadsEveryLargeDatasetReview(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	var commentRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/repos/acme/widgets/pulls/42/reviews" {
			reviews := make([]map[string]any, 101)
			for i := range reviews {
				reviews[i] = map[string]any{"id": i + 1}
			}
			assert.NoError(json.NewEncoder(w).Encode(reviews))
			return
		}
		commentRequests.Add(1)
		reviewPath := strings.TrimSuffix(r.URL.Path, "/comments")
		reviewID, err := strconv.Atoi(reviewPath[strings.LastIndexByte(reviewPath, '/')+1:])
		assert.NoError(err)
		assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 1000 + reviewID}}))
	}))
	defer server.Close()

	client, err := NewClient(
		"codeberg.test", testTokenSource("token"),
		WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 101)
	assert.Equal("1", threads[0].ProviderReviewID)
	assert.Equal("101", threads[100].ProviderReviewID)
	assert.Equal(int32(101), commentRequests.Load())
}

func TestListMergeRequestReviewThreadsReadsEveryLargeDatasetComment(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/pulls/42/reviews":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 99}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments":
			comments := make([]map[string]any, 1001)
			for i := range comments {
				comments[i] = map[string]any{"id": i + 1}
			}
			assert.NoError(json.NewEncoder(w).Encode(comments))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(
		"codeberg.test", testTokenSource("token"),
		WithBaseURLForTesting(server.URL), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 1001)
	assert.Equal("1", threads[0].ProviderCommentID)
	assert.Equal("1001", threads[1000].ProviderCommentID)
}
