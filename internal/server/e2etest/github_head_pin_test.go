package e2etest

// Full-stack coverage for provider-enforced head pins on GitHub: the HTTP API
// must hand the reviewed head to merge mutations, map the provider's moved-head
// rejection to a 409 conflict with reason stale_state, and send approval
// reviews with the requested or stored provider head when one is available.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

// setupGitHubHeadPinServer boots the HTTP API with real SQLite against
// the recording GitHub mock and seeds repo acme/widget with PR 7 whose
// locally synced (reviewed) head is "reviewed-sha".
func setupGitHubHeadPinServer(
	t *testing.T, mock *mockGH,
) (*server.Server, *db.DB, int64) {
	return setupGitHubHeadPinServerWithDiff(t, mock, true)
}

func setupGitHubHeadPinServerWithoutReviewedDiff(
	t *testing.T, mock *mockGH,
) (*server.Server, *db.DB, int64) {
	return setupGitHubHeadPinServerWithDiff(t, mock, false)
}

func setupGitHubHeadPinServerWithDiff(
	t *testing.T, mock *mockGH, seedReviewedDiff bool,
) (*server.Server, *db.DB, int64) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "1",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://github.com/acme/widget/pull/7",
		Title:           "Test PR",
		Author:          "author",
		State:           "open",
		PlatformHeadSHA: "reviewed-sha",
		PlatformBaseSHA: "base-sha",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	if seedReviewedDiff {
		require.NoError(database.UpdateDiffSHAs(
			ctx, repoID, 7, "reviewed-sha", "base-sha", "merge-base",
		))
	}

	repo := ghclient.RepoRef{
		Platform:     platform.KindGitHub,
		Owner:        "acme",
		Name:         "widget",
		PlatformHost: "github.com",
		RepoPath:     "acme/widget",
	}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	return srv, database, repoID
}

type conflictProblemBody struct {
	Code    string         `json:"code"`
	Detail  string         `json:"detail"`
	Details map[string]any `json:"details"`
}

func decodeConflictProblem(t *testing.T, body *json.Decoder) (string, map[string]any) {
	t.Helper()
	problem := decodeConflictProblemBody(t, body)
	return problem.Code, problem.Details
}

func decodeConflictProblemBody(t *testing.T, body *json.Decoder) conflictProblemBody {
	t.Helper()
	var problem conflictProblemBody
	require.NoError(t, body.Decode(&problem))
	return problem
}

// mergedHeadPinPullRequest is the provider's view of PR 7 after its merge
// succeeded; the canonical post-merge resync commits this snapshot to record
// the transition. updated_at must be current so the monotonic snapshot guard
// accepts it.
func mergedHeadPinPullRequest() *gh.PullRequest {
	now := gh.Timestamp{Time: time.Now().UTC()}
	return &gh.PullRequest{
		ID:             new(int64(7001)),
		Number:         new(7),
		State:          new("closed"),
		Merged:         new(true),
		Title:          new("Test PR"),
		HTMLURL:        new("https://github.com/acme/widget/pull/7"),
		User:           &gh.User{Login: new("author")},
		MergedBy:       &gh.User{Login: new("merger")},
		MergeCommitSHA: new("merge-sha"),
		CreatedAt:      &gh.Timestamp{Time: now.Add(-time.Hour)},
		UpdatedAt:      &now,
		MergedAt:       &now,
		Head: &gh.PullRequestBranch{
			Ref: new("feature"), SHA: new("reviewed-sha"),
			Repo: &gh.Repository{ID: new(int64(1)), FullName: new("acme/widget")},
		},
		Base: &gh.PullRequestBranch{
			Ref: new("main"), SHA: new("base-sha"),
			Repo: &gh.Repository{ID: new(int64(1)), FullName: new("acme/widget")},
		},
	}
}

func TestGitHubMergePassesReviewedHeadPinToProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var recordedPin atomic.Value
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, expectedHeadSHA string,
		) (*gh.PullRequestMergeResult, error) {
			recordedPin.Store(expectedHeadSHA)
			merged := true
			sha := "merge-sha"
			message := "merged"
			return &gh.PullRequestMergeResult{Merged: &merged, SHA: &sha, Message: &message}, nil
		},
		// The canonical post-merge resync reads the pull request back;
		// reflect the merge the way the real provider does.
		getPullRequestFn: func(
			context.Context, string, string, int,
		) (*gh.PullRequest, error) {
			return mergedHeadPinPullRequest(), nil
		},
	}
	srv, database, repoID := setupGitHubHeadPinServer(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"reviewed-sha"}`),
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal("reviewed-sha", recordedPin.Load(),
		"the reviewed head must reach the provider merge call as the sha pin")

	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("merged", string(mr.State))
}

func TestGitHubDetailExposesReviewedHeadOnlyWhenDiffIsCurrent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mock := &mockGH{}
	srv, database, repoID := setupGitHubHeadPinServer(t, mock)

	var body struct {
		PlatformHeadSHA string `json:"platform_head_sha"`
		ReviewedHeadSHA string `json:"reviewed_head_sha"`
	}
	rr := doJSONRequest(t, srv, http.MethodGet,
		"/api/v1/pulls/github/acme/widget/7", nil,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal("reviewed-sha", body.PlatformHeadSHA)
	assert.Equal("reviewed-sha", body.ReviewedHeadSHA)

	require.NoError(database.UpdatePlatformSHAs(t.Context(), repoID, 7, "new-head", "base-sha"))
	rr = doJSONRequest(t, srv, http.MethodGet,
		"/api/v1/pulls/github/acme/widget/7", nil,
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal("new-head", body.PlatformHeadSHA)
	assert.Empty(body.ReviewedHeadSHA,
		"a platform head without a matching diff snapshot must not be echoed as reviewed")
}

func TestGitHubMergeRejectsMissingReviewedDiff(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, _ string,
		) (*gh.PullRequestMergeResult, error) {
			providerCalled.Store(true)
			return &gh.PullRequestMergeResult{}, nil
		},
	}
	srv, _, _ := setupGitHubHeadPinServerWithoutReviewedDiff(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"reviewed-sha"}`),
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	code, details := decodeConflictProblem(t, json.NewDecoder(rr.Body))
	assert.Equal("conflict", code)
	require.NotNil(details)
	assert.Equal("head_unknown", details["reason"])
	assert.False(providerCalled.Load())
}

func TestGitHubMergeRejectsStaleReviewedDiff(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, _ string,
		) (*gh.PullRequestMergeResult, error) {
			providerCalled.Store(true)
			return &gh.PullRequestMergeResult{}, nil
		},
	}
	srv, database, repoID := setupGitHubHeadPinServer(t, mock)
	require.NoError(database.UpdatePlatformSHAs(t.Context(), repoID, 7, "new-head", "base-sha"))

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"new-head"}`),
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	code, details := decodeConflictProblem(t, json.NewDecoder(rr.Body))
	assert.Equal("conflict", code)
	require.NotNil(details)
	assert.Equal("stale_state", details["reason"])
	assert.False(providerCalled.Load())
}

func TestGitHubMergeRejectsMissingReviewedHeadPin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, _ string,
		) (*gh.PullRequestMergeResult, error) {
			providerCalled.Store(true)
			return &gh.PullRequestMergeResult{}, nil
		},
	}
	srv, _, _ := setupGitHubHeadPinServer(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m"}`),
	)
	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	var problem conflictProblemBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("validationError", problem.Code)
	require.NotNil(problem.Details)
	assert.Equal("body.expected_head_sha", problem.Details["field"])
	assert.False(providerCalled.Load())
}

func TestGitHubMergeMovedHeadRejectionMapsToStaleState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, _ string,
		) (*gh.PullRequestMergeResult, error) {
			return nil, &gh.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusMethodNotAllowed},
				Message:  "Head branch was modified. Review and try the merge again.",
			}
		},
	}
	srv, _, _ := setupGitHubHeadPinServer(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"reviewed-sha"}`),
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	code, details := decodeConflictProblem(t, json.NewDecoder(rr.Body))
	assert.Equal("conflict", code)
	require.NotNil(details)
	assert.Equal("stale_state", details["reason"],
		"the provider's moved-head rejection must surface as stale_state")
}

func TestGitHubMergeGenericConflictKeepsConflictReason(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mock := &mockGH{
		mergePullRequestFn: func(
			_ context.Context, _, _ string, _ int, _, _, _, _ string,
		) (*gh.PullRequestMergeResult, error) {
			return nil, &gh.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusMethodNotAllowed},
				Message:  "Pull Request is not mergeable",
			}
		},
	}
	srv, _, _ := setupGitHubHeadPinServer(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/merge",
		json.RawMessage(`{"method":"merge","commit_title":"t","commit_message":"m","expected_head_sha":"reviewed-sha"}`),
	)
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	code, details := decodeConflictProblem(t, json.NewDecoder(rr.Body))
	assert.Equal("conflict", code)
	require.NotNil(details)
	assert.Equal("conflict", details["reason"],
		"an unrelated provider conflict must not present as staleness")
}

func TestGitHubApproveSubmitsReview(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	var reviewCommitID string
	mock := &mockGH{
		createReviewWithCommentsFn: func(
			_ context.Context, _, _ string, _ int, event, body, commitID string,
			_ []*gh.DraftReviewComment,
		) (*gh.PullRequestReview, error) {
			providerCalled.Store(true)
			reviewCommitID = commitID
			id := int64(77)
			state := event
			now := gh.Timestamp{Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
			return &gh.PullRequestReview{
				ID:          &id,
				State:       &state,
				Body:        &body,
				SubmittedAt: &now,
				User:        &gh.User{Login: new("reviewer")},
			}, nil
		},
	}
	srv, _, _ := setupGitHubHeadPinServer(t, mock)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/approve",
		json.RawMessage(`{"body":"lgtm","expected_head_sha":"reviewed-sha"}`),
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.True(providerCalled.Load())
	assert.Equal("reviewed-sha", reviewCommitID)
}

func TestGitHubApproveOmittedHeadPinUsesStoredPlatformHead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	var reviewCommitID string
	mock := &mockGH{
		createReviewWithCommentsFn: func(
			_ context.Context, _, _ string, _ int, event, body, commitID string,
			_ []*gh.DraftReviewComment,
		) (*gh.PullRequestReview, error) {
			providerCalled.Store(true)
			reviewCommitID = commitID
			id := int64(79)
			state := event
			now := gh.Timestamp{Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
			return &gh.PullRequestReview{
				ID:          &id,
				State:       &state,
				Body:        &body,
				SubmittedAt: &now,
				User:        &gh.User{Login: new("reviewer")},
			}, nil
		},
	}
	srv, database, repoID := setupGitHubHeadPinServerWithoutReviewedDiff(t, mock)
	require.NoError(database.UpdatePlatformSHAs(t.Context(), repoID, 7, "platform-sha", "base-sha"))

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/approve",
		json.RawMessage(`{"body":"lgtm"}`),
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.True(providerCalled.Load())
	assert.Equal("platform-sha", reviewCommitID)
}

func TestGitHubReviewDraftApprovePublishesReview(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var providerCalled atomic.Bool
	var reviewCommitID string
	mock := &mockGH{
		createReviewWithCommentsFn: func(
			_ context.Context, _, _ string, _ int, event, body, commitID string,
			comments []*gh.DraftReviewComment,
		) (*gh.PullRequestReview, error) {
			providerCalled.Store(true)
			reviewCommitID = commitID
			assert.Equal("APPROVE", event)
			assert.Equal("summary note", body)
			assert.Len(comments, 1)
			id := int64(78)
			state := event
			now := gh.Timestamp{Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
			return &gh.PullRequestReview{
				ID:          &id,
				State:       &state,
				Body:        &body,
				SubmittedAt: &now,
				User:        &gh.User{Login: new("reviewer")},
			}, nil
		},
	}
	srv, database, repoID := setupGitHubHeadPinServer(t, mock)
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
			Path:        "internal/server/e2etest/github_head_pin_test.go",
			Side:        "right",
			Line:        42,
			NewLine:     &line,
			LineType:    "add",
			DiffHeadSHA: "reviewed-sha",
		},
	})
	require.NoError(err)

	rr := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/review-draft/publish",
		json.RawMessage(`{"action":"approve","body":"summary note"}`),
	)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.True(providerCalled.Load())
	assert.Equal("reviewed-sha", reviewCommitID)
	storedDraft, err := database.GetMRReviewDraft(ctx, mr.ID)
	require.NoError(err)
	require.Nil(storedDraft)
}
