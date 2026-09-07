package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	platformgithub "go.kenn.io/forge/platform/github"
)

func TestGitHubComMergeUsesAsyncAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPut, r.Method)
		assert.Equal("/api/v3/repos/acme/widget/pulls/7/merge-async", r.URL.Path)
		assert.Equal("2026-03-10", r.Header.Get("X-GitHub-Api-Version"))
		var body map[string]string
		assert.NoError(json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(map[string]string{
			"commit_title":   "Merge feature",
			"commit_message": "Reviewed and ready",
			"sha":            "reviewed-head",
			"merge_method":   "squash",
			"merge_action":   "direct_merge",
		}, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"merged",
			"details":{"message":"Pull request merged.","sha":"merge-sha"}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7,
		"Merge feature", "Reviewed and ready", "squash", "reviewed-head",
	)
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.GetMerged())
	assert.Equal("merge-sha", result.GetSHA())
	assert.Equal("Pull request merged.", result.GetMessage())
}

func TestGitHubComMergePollsPendingOperation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		assert.Equal("2026-03-10", r.Header.Get("X-GitHub-Api-Version"))
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			assert.Equal(http.MethodPut, r.Method)
			assert.Equal("/api/v3/repos/acme/widget/pulls/7/merge-async", r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{
				"status":"pending",
				"details":{"message":"Merge request is in progress.","uuid":"operation-id"}
			}`))
		case 2:
			assert.Equal(http.MethodGet, r.Method)
			assert.Equal("/api/v3/repos/acme/widget/pulls/7/merge-async/operation-id", r.URL.Path)
			_, _ = w.Write([]byte(`{
				"status":"merged",
				"details":{"message":"Pull request merged.","sha":"merge-sha"}
			}`))
		default:
			assert.Fail("unexpected request", "call %d", call)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.GetMerged())
	assert.Equal("merge-sha", result.GetSHA())
	assert.Equal(int32(2), calls.Load())
}

func TestGitHubComMergePreservesTerminalFailure(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status":"failed",
			"details":{"message":"The stack must be rebased before it can be merged."}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.Nil(result)
	require.Error(err)
	assert.Contains(t, err.Error(), "The stack must be rebased before it can be merged.")
	var githubError *gh.ErrorResponse
	require.ErrorAs(err, &githubError)
	require.NotNil(githubError.Response)
	assert.Equal(t, http.StatusConflict, githubError.Response.StatusCode)
}

func TestGitHubComMergeHeadMoveIsConflict(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"failed",
			"details":{"message":"Head branch was modified. Review and try the merge again."}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	_, err = client.MergePullRequest(
		t.Context(), "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.Error(err)
	var githubError *gh.ErrorResponse
	require.ErrorAs(err, &githubError)
	require.NotNil(githubError.Response)
	assert.Equal(t, http.StatusConflict, githubError.Response.StatusCode)
	assert.Equal(t, "Head branch was modified. Review and try the merge again.", githubError.Message)
	assert.True(t, platformgithub.IsGitHubHeadModified(err))
}

func TestGitHubComMergeDoesNotAdoptExistingOperation(t *testing.T) {
	require := require.New(t)
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{
			"status":"pending",
			"details":{"message":"An asynchronous merge is already in progress.","uuid":"other-options"}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.Nil(result)
	require.Error(err)
	var githubError *gh.ErrorResponse
	require.ErrorAs(err, &githubError)
	require.NotNil(githubError.Response)
	assert.Equal(t, http.StatusConflict, githubError.Response.StatusCode)
	assert.Equal(t, "an asynchronous merge request is already in progress", githubError.Message)
	assert.Equal(t, int32(1), calls.Load(), "must not poll the existing operation")
}

func TestGitHubComMergeHonorsCancellationWhilePolling(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{
			"status":"pending",
			"details":{"message":"Merge request is in progress.","uuid":"operation-id"}
		}`))
		cancel()
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		ctx, "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.Nil(result)
	require.Error(err)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(t, int32(1), calls.Load())
}

func TestGitHubComMergeDoesNotTreatEnqueuedAsMerged(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"enqueued",
			"details":{"message":"Pull request was added to the merge queue."}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7, "title", "message", "merge", "head",
	)
	require.Nil(result)
	require.Error(err)
	assert.Contains(t, err.Error(), "Pull request was added to the merge queue.")
}

func TestGitHubEnterpriseMergeKeepsSynchronousAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPut, r.Method)
		assert.Equal("/api/v3/repos/acme/widget/pulls/7/merge", r.URL.Path)
		var body map[string]string
		assert.NoError(json.NewDecoder(r.Body).Decode(&body))
		assert.Equal("reviewed-head", body["sha"])
		assert.Equal("squash", body["merge_method"])
		assert.Equal("Merge feature", body["commit_title"])
		assert.Equal("Reviewed and ready", body["commit_message"])
		assert.NotContains(body, "merge_action")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"merged":true,"sha":"merge-sha","message":"Pull request merged."
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		testTokenSource("token"), "github.example.com", nil, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)

	result, err := client.MergePullRequest(
		t.Context(), "acme", "widget", 7,
		"Merge feature", "Reviewed and ready", "squash", "reviewed-head",
	)
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.GetMerged())
	assert.Equal("merge-sha", result.GetSHA())
}
