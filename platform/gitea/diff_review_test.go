package gitea

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	giteasdk "code.gitea.io/sdk/gitea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ghsync "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
)

func TestGiteaReviewThreadPreservesContextCoordinates(t *testing.T) {
	assert := assert.New(t)
	thread := giteaReviewThread(
		&giteasdk.PullReview{ID: 99},
		&giteasdk.PullReviewComment{
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

func TestListMergeRequestReviewThreadsReadsBeyondTenthReviewPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	client, err := NewClient(
		"gitea.test", testTokenSource("token"), WithBaseURL(server.URL, true),
		WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
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

func TestListMergeRequestReviewThreadsReadsEveryReviewPageAndComment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	created := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/pulls/42/reviews":
			assert.Equal("100", r.URL.Query().Get("limit"))
			switch r.URL.Query().Get("page") {
			case "1":
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/repos/acme/widgets/pulls/42/reviews?page=2&limit=100>; rel="next"`, server.URL))
				assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{
					"id": 99, "state": "COMMENT", "submitted_at": created.Format(time.RFC3339),
					"user": map[string]any{"login": "reviewer-one"},
				}}))
			case "2":
				assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{
					"id": 100, "state": "COMMENT", "submitted_at": created.Add(time.Minute).Format(time.RFC3339),
					"user": map[string]any{"login": "reviewer-two"},
				}}))
			default:
				http.Error(w, "unexpected review page", http.StatusBadRequest)
			}
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 101, "body": "first inline note",
					"html_url": "https://gitea.test/acme/widgets/pulls/42#issuecomment-101",
					"user":     map[string]any{"login": "reviewer-one"}, "pull_request_review_id": 99,
					"path": "src/main.go", "commit_id": "head-sha", "position": 7,
					"created_at": created.Format(time.RFC3339), "updated_at": created.Add(time.Minute).Format(time.RFC3339),
				},
				{
					"id": 102, "body": "second inline note",
					"html_url": "https://gitea.test/acme/widgets/pulls/42#issuecomment-102",
					"user":     map[string]any{"login": "reviewer-one"}, "pull_request_review_id": 99,
					"path": "src/other.go", "commit_id": "head-sha", "position": 9,
					"created_at": created.Add(2 * time.Minute).Format(time.RFC3339), "updated_at": created.Add(3 * time.Minute).Format(time.RFC3339),
				},
			}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/100/comments":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{
				"id": 201, "body": "resolved old-side note",
				"html_url": "https://gitea.test/acme/widgets/pulls/42#issuecomment-201",
				"user":     map[string]any{"login": "reviewer-two"}, "resolver": map[string]any{"login": "maintainer"},
				"pull_request_review_id": 100, "path": "src/old.go", "commit_id": "older-sha", "original_position": 4,
				"created_at": created.Add(4 * time.Minute).Format(time.RFC3339), "updated_at": created.Add(5 * time.Minute).Format(time.RFC3339),
			}}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient("gitea.test", testTokenSource("token"), WithBaseURL(server.URL, true), WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Platform: platform.KindGitea, Host: "gitea.test", Owner: "acme", Name: "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 3)
	assert.Equal("101", threads[0].ProviderThreadID)
	assert.Equal("99", threads[0].ProviderReviewID)
	assert.Equal("101", threads[0].ProviderCommentID)
	assert.Equal("first inline note", threads[0].Body)
	assert.Equal("reviewer-one", threads[0].AuthorLogin)
	assert.Equal("https://gitea.test/acme/widgets/pulls/42#issuecomment-101", threads[0].DirectURL)
	assert.Equal("src/main.go", threads[0].Range.Path)
	assert.Equal("right", threads[0].Range.Side)
	assert.Equal(7, threads[0].Range.Line)
	assert.Equal(7, *threads[0].Range.NewLine)
	assert.Nil(threads[0].Range.OldLine)
	assert.Equal("add", threads[0].Range.LineType)
	assert.Equal("head-sha", threads[0].Range.DiffHeadSHA)
	assert.Equal("head-sha", threads[0].Range.CommitSHA)
	assert.Equal(created, threads[0].CreatedAt)
	assert.Equal(created.Add(time.Minute), threads[0].UpdatedAt)
	assert.Equal("102", threads[1].ProviderCommentID)
	assert.Equal("100", threads[2].ProviderReviewID)
	assert.Equal("left", threads[2].Range.Side)
	assert.Equal(4, threads[2].Range.Line)
	assert.Equal(4, *threads[2].Range.OldLine)
	assert.Nil(threads[2].Range.NewLine)
	assert.Equal("delete", threads[2].Range.LineType)
	assert.True(threads[2].Resolved)
	require.NotNil(threads[2].ResolvedAt)
	assert.Equal(created.Add(5*time.Minute), *threads[2].ResolvedAt)
}

func TestListMergeRequestReviewThreadsRejectsPartialDataset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/pulls/42/reviews":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 99}, {"id": 100}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "body": "partial"}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/100/comments":
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient("gitea.test", testTokenSource("token"), WithBaseURL(server.URL, true), WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.Error(err)
	assert.Nil(threads)
}

func TestListMergeRequestReviewThreadsMapsAuthenticationErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			defer server.Close()

			client, err := NewClient(
				"gitea.test", testTokenSource("token"),
				WithBaseURL(server.URL, true),
				WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
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
			require.Equal(platform.KindGitea, platformErr.Provider)
			require.Equal("gitea.test", platformErr.PlatformHost)
		})
	}
}

func TestListMergeRequestReviewThreadsReadsEveryLargeDatasetReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
		"gitea.test", testTokenSource("token"),
		WithBaseURL(server.URL, true),
		WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
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
	require := require.New(t)
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
		"gitea.test", testTokenSource("token"),
		WithBaseURL(server.URL, true),
		WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.NoError(err)
	require.Len(threads, 1001)
	assert.Equal("1", threads[0].ProviderCommentID)
	assert.Equal("1001", threads[1000].ProviderCommentID)
}

func TestListMergeRequestReviewThreadsRejectsReviewPageCycleBeforeFanout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/42/reviews" {
			assert.Fail("review comments must not be fetched from a cyclic review listing")
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		page := r.URL.Query().Get("page")
		nextPage := "2"
		reviewID := 1
		if page == "2" {
			nextPage = "1"
			reviewID = 2
		}
		w.Header().Set("Link", fmt.Sprintf(
			`<%s/api/v1/repos/acme/widgets/pulls/42/reviews?page=%s&limit=100>; rel="next"`,
			server.URL, nextPage,
		))
		assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": reviewID}}))
	}))
	defer server.Close()

	client, err := NewClient(
		"gitea.test", testTokenSource("token"),
		WithBaseURL(server.URL, true),
		WithServerVersion(testGiteaServerVersion), WithTransport(http.DefaultTransport))
	require.NoError(err)
	threads, err := client.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.ErrorIs(err, platform.ErrProviderContract)
	assert.Nil(threads)
	assert.Equal(int32(2), requests.Load())
}

func TestListMergeRequestReviewThreadsRejectsDatasetBeyondArchiveAttemptAllowance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/pulls/42/reviews":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 99}, {"id": 100}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/99/comments":
			assert.NoError(json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "body": "partial"}}))
		case "/api/v1/repos/acme/widgets/pulls/42/reviews/100/comments":
			assert.Fail("request exceeded admitted wire-attempt allowance")
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	budget := ghsync.NewSyncBudget(20)
	client, err := NewClient(
		"gitea.test", testTokenSource("token"),
		WithBaseURL(server.URL, true),
		WithServerVersion(testGiteaServerVersion),
		WithTransport(ghsync.WrapSyncBudgetTransport(http.DefaultTransport, budget)))
	require.NoError(err)
	ctx := ghsync.WithArchiveAttemptAllowance(ghsync.WithArchiveSyncBudget(t.Context()), 2)
	threads, err := client.ListMergeRequestReviewThreads(ctx, platform.RepoRef{
		Owner: "acme", Name: "widgets",
	}, 42)

	require.ErrorIs(err, platform.ErrArchiveAttemptBudget)
	assert.Nil(threads)
	assert.Equal(int32(2), requests.Load())
	assert.Equal(2, budget.Spent())
}
