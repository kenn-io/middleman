package gitealiketest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ghsync "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/ratelimit"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
)

type Client interface {
	platform.Provider
	platform.RepositoryReader
	platform.MergeRequestReader
	platform.IssueReader
	platform.IssuePageReader
	platform.MergeRequestPageReader
	platform.CommentMutator
	platform.StateMutator
	platform.MergeMutator
	platform.ReviewMutator
	platform.IssueMutator
	platform.MergeRequestContentMutator
	platform.LabelReader
	platform.LabelMutator
}

type TestClient struct {
	Client
	LookupRepository func(context.Context, string, string) (string, error)
}

type ClientOptions struct {
	ForegroundTimeout time.Duration
	RateTracker       *ratelimit.RateTracker
	SyncBudget        *ghsync.SyncBudget
}

type Adapter struct {
	Kind         platform.Kind
	Host         string
	Token        string
	Capabilities platform.Capabilities
	NewClient    func(*testing.T, string, string, ClientOptions) TestClient
}

func BaseCapabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories:      true,
		ReadMergeRequests:     true,
		ReadIssues:            true,
		ReadIssuePRReferences: true,
		ReadComments:          true,
		ReadReleases:          true,
		ReadCI:                true,
		ReadLabels:            true,
		ReadAuthenticatedUser: true,
		CommentMutation:       true,
		StateMutation:         true,
		MergeMutation:         true,
		ReviewMutation:        true,
		IssueMutation:         true,
		LabelMutation:         true,
		AssigneeMutation:      true,
		ReviewerMutation:      true,
		MutationHeadBinding:   true,
		SupportedReviewActions: []platform.ReviewAction{
			platform.ReviewActionComment,
			platform.ReviewActionApprove,
			platform.ReviewActionRequestChanges,
		},
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues: true, HistoricalMergeRequests: true,
			OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
		},
	}
}

func Run(t *testing.T, adapter Adapter) {
	t.Helper()
	require.NotEmpty(t, adapter.Kind)
	require.NotEmpty(t, adapter.Host)
	require.NotEmpty(t, adapter.Token)
	require.NotNil(t, adapter.NewClient)

	t.Run("rate limit headers", func(t *testing.T) {
		reset := time.Now().UTC().Add(30 * time.Minute).Unix()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			_, _ = w.Write(repositoryJSON())
		}))
		defer server.Close()

		database := dbtest.Open(t)
		tracker := ratelimit.NewPlatformRateTracker(
			database, string(adapter.Kind), adapter.Host, "host", "rest",
		)
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{RateTracker: tracker})

		_, err := client.GetRepository(t.Context(), repoRef(adapter, "owner", "repo"))
		require.NoError(t, err)
		row, err := database.GetPlatformRateLimit(string(adapter.Kind), adapter.Host, "host", "rest")
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.Equal(t, 1, row.RequestsHour)
		assert.Equal(t, 5000, row.RateLimit)
		assert.Equal(t, 4999, row.RateRemaining)
		require.NotNil(t, row.RateResetAt)
		assert.Equal(t, reset, row.RateResetAt.Unix())
	})

	t.Run("disabled issues classification", func(t *testing.T) {
		issueRequests := 0
		repositoryRequests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/repos/owner/repo/issues":
				issueRequests++
				http.Error(w, "issues disabled", http.StatusNotFound)
			case "/api/v1/repos/owner/repo":
				repositoryRequests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id":1,"name":"repo","full_name":"owner/repo",
					"owner":{"id":2,"login":"owner"},
					"has_issues":false,"has_pull_requests":true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		_, err := client.ListOpenIssues(t.Context(), repoRef(adapter, "owner", "repo"))

		require.ErrorIs(t, err, platform.ErrRepositoryFeatureDisabled)
		var platformErr *platform.Error
		require.ErrorAs(t, err, &platformErr)
		assert.Equal(t, adapter.Kind, platformErr.Provider)
		assert.Equal(t, adapter.Host, platformErr.PlatformHost)
		assert.Equal(t, platform.RepositoryFeatureIssues, platformErr.Capability)
		assert.Equal(t, 1, issueRequests)
		assert.Equal(t, 1, repositoryRequests)
	})

	t.Run("archive inventory", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/repos/owner/repo/issues":
				assert.Equal(t, "all", r.URL.Query().Get("state"))
				assert.Equal(t, "issues", r.URL.Query().Get("type"))
				assert.NotEmpty(t, r.URL.Query().Get("before"))
				_, _ = w.Write([]byte(`[{"id":1,"number":1,"state":"closed","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]`))
			case "/api/v1/repos/owner/repo/pulls":
				assert.Equal(t, "all", r.URL.Query().Get("state"))
				assert.Equal(t, "oldest", r.URL.Query().Get("sort"))
				_, _ = w.Write([]byte(`[{"id":2,"number":2,"state":"closed","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		budget := ghsync.NewSyncBudget(20)
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{SyncBudget: budget})
		ref := repoRef(adapter, "owner", "repo")
		ctx := ghsync.WithArchiveSyncBudget(context.Background())

		issues, err := client.ListIssuesPage(ctx, ref, platform.ItemPageQuery{Order: platform.ItemOrderCreated})
		require.NoError(t, err)
		pulls, err := client.ListMergeRequestsPage(ctx, ref, platform.ItemPageQuery{Order: platform.ItemOrderCreated})
		require.NoError(t, err)

		assert.Len(t, issues.Items, 1)
		assert.Len(t, pulls.Items, 1)
		assert.Equal(t, 2, requests)
		assert.Equal(t, 2, budget.Spent())
		assert.Equal(t, 2, budget.ArchiveSpent())
	})

	t.Run("repository lookup authentication", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/repos/owner/repo", r.URL.Path)
			assert.Equal(t, "token "+adapter.Token, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(repositoryJSON())
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		name, err := client.LookupRepository(context.Background(), "owner", "repo")
		require.NoError(t, err)
		assert.Equal(t, "repo", name)
	})

	t.Run("foreground timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{
			ForegroundTimeout: 20 * time.Millisecond,
		})

		_, err := client.LookupRepository(context.Background(), "owner", "repo")
		require.Error(t, err)
	})

	t.Run("in-flight cancellation", func(t *testing.T) {
		requestStarted := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/repos/owner/repo", r.URL.Path)
			close(requestStarted)
			<-r.Context().Done()
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{
			ForegroundTimeout: time.Minute,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := client.LookupRepository(ctx, "owner", "repo")
			done <- err
		}()

		select {
		case <-requestStarted:
		case <-time.After(time.Second):
			require.FailNow(t, "request did not start")
		}
		cancel()

		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			require.FailNow(t, "request was not canceled")
		}
	})

	t.Run("waiting request cancellation", func(t *testing.T) {
		requestStarted := make(chan struct{})
		releaseRequest := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/repos/owner/repo", r.URL.Path)
			close(requestStarted)
			<-releaseRequest
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(repositoryJSON())
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{
			ForegroundTimeout: time.Minute,
		})
		firstDone := make(chan error, 1)
		go func() {
			_, err := client.LookupRepository(context.Background(), "owner", "repo")
			firstDone <- err
		}()

		select {
		case <-requestStarted:
		case <-time.After(time.Second):
			require.FailNow(t, "request did not start")
		}

		waitingCtx, cancelWaiting := context.WithCancel(context.Background())
		waitingDone := make(chan error, 1)
		go func() {
			_, err := client.LookupRepository(waitingCtx, "owner", "repo")
			waitingDone <- err
		}()
		cancelWaiting()

		select {
		case err := <-waitingDone:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			require.FailNow(t, "waiting request was not canceled")
		}

		close(releaseRequest)
		select {
		case err := <-firstDone:
			require.NoError(t, err)
		case <-time.After(time.Second):
			require.FailNow(t, "first request did not finish")
		}
	})

	t.Run("sync budget", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(repositoryJSON())
		}))
		defer server.Close()
		budget := ghsync.NewSyncBudget(20)
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{SyncBudget: budget})

		_, err := client.LookupRepository(
			ghsync.WithSyncBudget(context.Background()), "owner", "repo",
		)
		require.NoError(t, err)
		assert.Equal(t, 1, budget.Spent())
	})

	t.Run("identity and capabilities", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		assert.Equal(t, adapter.Kind, client.Platform())
		assert.Equal(t, adapter.Host, client.Host())
		assert.Equal(t, adapter.Capabilities, client.Capabilities())
	})

	t.Run("open merge requests and issues", func(t *testing.T) {
		var sawPulls, sawIssues bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "token "+adapter.Token, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/repos/owner/repo/pulls":
				sawPulls = true
				assert.Equal(t, "open", r.URL.Query().Get("state"))
				assert.Equal(t, "1", r.URL.Query().Get("page"))
				assert.Equal(t, "100", r.URL.Query().Get("limit"))
				_, _ = w.Write([]byte(`[{"id":11,"number":3,"html_url":"https://provider.test/owner/repo/pulls/3","mergeable":false,"title":"review me","state":"open","user":{"login":"alice"},"head":{"ref":"feature","sha":"abc"},"base":{"ref":"main","sha":"def"}}]`))
			case "/api/v1/repos/owner/repo/issues":
				sawIssues = true
				assert.Equal(t, "open", r.URL.Query().Get("state"))
				_, _ = w.Write([]byte(`[{"id":21,"number":4,"html_url":"https://provider.test/owner/repo/issues/4","title":"bug","state":"open","user":{"login":"bob"}}]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})
		ref := repoRef(adapter, "owner", "repo")

		mergeRequests, err := client.ListOpenMergeRequests(context.Background(), ref)
		require.NoError(t, err)
		issues, err := client.ListOpenIssues(context.Background(), ref)
		require.NoError(t, err)

		assert.True(t, sawPulls)
		assert.True(t, sawIssues)
		require.Len(t, mergeRequests, 1)
		assert.Equal(t, "dirty", mergeRequests[0].MergeableState)
		assert.Len(t, issues, 1)
	})

	t.Run("mutations", func(t *testing.T) {
		var seen []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "token "+adapter.Token, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			seen = append(seen, r.Method+" "+r.URL.Path)
			switch r.Method + " " + r.URL.Path {
			case "POST /api/v1/repos/owner/repo/issues/7/comments",
				"PATCH /api/v1/repos/owner/repo/issues/comments/10":
				_, _ = w.Write([]byte(`{"id":10,"body":"comment","user":{"login":"alice"}}`))
			case "DELETE /api/v1/repos/owner/repo/issues/comments/10":
				w.WriteHeader(http.StatusNoContent)
			case "POST /api/v1/repos/owner/repo/issues",
				"PATCH /api/v1/repos/owner/repo/issues/8":
				_, _ = w.Write([]byte(`{"id":20,"number":8,"title":"issue","state":"closed","user":{"login":"bob"}}`))
			case "PATCH /api/v1/repos/owner/repo/pulls/7":
				_, _ = w.Write([]byte(`{"id":30,"number":7,"title":"pr","state":"closed","user":{"login":"carol"},"head":{"ref":"feature","sha":"abc"},"base":{"ref":"main","sha":"def"}}`))
			case "POST /api/v1/repos/owner/repo/pulls/7/merge":
				_, _ = w.Write([]byte(`{}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})
		ref := repoRef(adapter, "owner", "repo")

		_, err := client.CreateMergeRequestComment(context.Background(), ref, 7, "comment")
		require.NoError(t, err)
		_, err = client.EditIssueComment(context.Background(), ref, 8, 10, "comment")
		require.NoError(t, err)
		require.NoError(t, client.DeleteMergeRequestComment(context.Background(), ref, 7, 10))
		require.NoError(t, client.DeleteIssueComment(context.Background(), ref, 8, 10))
		_, err = client.CreateIssue(context.Background(), ref, "issue", "body")
		require.NoError(t, err)
		_, err = client.SetIssueState(context.Background(), ref, 8, "closed")
		require.NoError(t, err)
		_, err = client.SetMergeRequestState(context.Background(), ref, 7, "closed")
		require.NoError(t, err)
		prTitle := "pr"
		prBody := "body"
		_, err = client.EditMergeRequestContent(context.Background(), ref, 7, &prTitle, &prBody)
		require.NoError(t, err)
		_, err = client.MergeMergeRequest(context.Background(), ref, 7, "title", "message", "squash", "")
		require.NoError(t, err)

		assert.Equal(t, []string{
			"POST /api/v1/repos/owner/repo/issues/7/comments",
			"PATCH /api/v1/repos/owner/repo/issues/comments/10",
			"DELETE /api/v1/repos/owner/repo/issues/comments/10",
			"DELETE /api/v1/repos/owner/repo/issues/comments/10",
			"POST /api/v1/repos/owner/repo/issues",
			"PATCH /api/v1/repos/owner/repo/issues/8",
			"PATCH /api/v1/repos/owner/repo/pulls/7",
			"PATCH /api/v1/repos/owner/repo/pulls/7",
			"POST /api/v1/repos/owner/repo/pulls/7/merge",
		}, seen)
	})

	t.Run("approval", func(t *testing.T) {
		sawRequest := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawRequest = true
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/repos/owner/repo/pulls/7/reviews", r.URL.Path)
			var body struct {
				Event    string `json:"event"`
				Body     string `json:"body"`
				CommitID string `json:"commit_id"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, "APPROVED", body.Event)
			assert.Equal(t, "ship it", body.Body)
			assert.Equal(t, "reviewed-head", body.CommitID)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":40,"state":"APPROVED","body":"ship it","user":{"login":"dana"},"submitted_at":"2026-05-01T12:00:00Z"}`))
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		event, err := client.ApproveMergeRequest(
			context.Background(), repoRef(adapter, "owner", "repo"), 7, "ship it", "reviewed-head",
		)
		require.NoError(t, err)
		assert.True(t, sawRequest)
		assert.Equal(t, "review", event.EventType)
		assert.Equal(t, "APPROVED", event.Summary)
	})

	t.Run("not found mapping", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/repos/owner/repo/pulls/99":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
			case "/api/v1/repos/owner/repo":
				_, _ = w.Write([]byte(`{"id":1,"name":"repo","full_name":"owner/repo","owner":{"login":"owner"},"has_issues":true,"has_pull_requests":true}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		_, err := client.GetMergeRequest(context.Background(), repoRef(adapter, "owner", "repo"), 99)
		require.Error(t, err)
		assert.ErrorIs(t, err, platform.ErrNotFound)
	})

	t.Run("merge rejection", func(t *testing.T) {
		message := "head out of date"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/owner/repo/pulls/7/merge" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": message})
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})
		ref := repoRef(adapter, "owner", "repo")

		_, err := client.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "merge", "reviewed-head")
		require.Error(t, err)
		var platformErr *platform.Error
		require.ErrorAs(t, err, &platformErr)
		assert.Equal(t, platform.ErrCodeStaleState, platformErr.Code)

		message = "merge conflict detected"
		_, err = client.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "merge", "reviewed-head")
		require.Error(t, err)
		require.NotErrorIs(t, err, platform.ErrStaleState)
		var httpErr *gitealike.HTTPError
		require.ErrorAs(t, err, &httpErr)
		assert.Equal(t, http.StatusConflict, httpErr.StatusCode)
		assert.Contains(t, httpErr.Error(), "merge conflict detected")
	})

	t.Run("concurrent merge rejections", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/repos/owner/repo/pulls/7/merge":
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"head out of date"}`))
			case "/api/v1/repos/owner/repo/pulls/8/merge":
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"merge conflict on 8"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})
		ref := repoRef(adapter, "owner", "repo")

		var wg sync.WaitGroup
		var errStale, errGeneric error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errStale = client.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "merge", "reviewed-head")
		}()
		go func() {
			defer wg.Done()
			_, errGeneric = client.MergeMergeRequest(context.Background(), ref, 8, "t", "m", "merge", "reviewed-head")
		}()
		wg.Wait()

		require.ErrorIs(t, errStale, platform.ErrStaleState)
		require.NotErrorIs(t, errGeneric, platform.ErrStaleState)
		var httpErr *gitealike.HTTPError
		require.ErrorAs(t, errGeneric, &httpErr)
		assert.Equal(t, http.StatusConflict, httpErr.StatusCode)
		assert.Contains(t, httpErr.Error(), "merge conflict on 8")
	})

	t.Run("label catalog", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/repos/acme/widget/labels", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":11,"name":"bug","color":"d73a4a","description":"Something is broken"},
				{"id":12,"name":"triage","color":"fbca04","description":""}
			]`))
		}))
		defer server.Close()
		client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

		catalog, err := client.ListLabels(context.Background(), repoRef(adapter, "acme", "widget"))
		require.NoError(t, err)
		require.Len(t, catalog.Labels, 2)
		assert.Equal(t, "bug", catalog.Labels[0].Name)
		assert.Equal(t, "d73a4a", catalog.Labels[0].Color)
		assert.Equal(t, "Something is broken", catalog.Labels[0].Description)
		assert.Equal(t, int64(11), catalog.Labels[0].PlatformID)
		assert.Equal(t, "triage", catalog.Labels[1].Name)
	})

	for _, labelTarget := range []struct {
		name string
		set  func(Client, context.Context, platform.RepoRef) ([]platform.Label, error)
	}{
		{"merge request labels", func(client Client, ctx context.Context, ref platform.RepoRef) ([]platform.Label, error) {
			return client.SetMergeRequestLabels(ctx, ref, 7, []string{"triage"})
		}},
		{"issue labels", func(client Client, ctx context.Context, ref platform.RepoRef) ([]platform.Label, error) {
			return client.SetIssueLabels(ctx, ref, 7, []string{"triage"})
		}},
	} {
		t.Run(labelTarget.name, func(t *testing.T) {
			var putBody map[string][]int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widget/labels":
					_, _ = w.Write([]byte(`[
						{"id":11,"name":"bug","color":"d73a4a"},
						{"id":12,"name":"triage","color":"fbca04"}
					]`))
				case r.Method == http.MethodPut && r.URL.Path == "/api/v1/repos/acme/widget/issues/7/labels":
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&putBody))
					_, _ = w.Write([]byte(`[{"id":12,"name":"triage","color":"fbca04"}]`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := adapter.NewClient(t, server.URL, adapter.Token, ClientOptions{})

			labels, err := labelTarget.set(client, context.Background(), repoRef(adapter, "acme", "widget"))
			require.NoError(t, err)
			assert.Equal(t, []int64{12}, putBody["labels"])
			require.Len(t, labels, 1)
			assert.Equal(t, "triage", labels[0].Name)
			assert.Equal(t, "fbca04", labels[0].Color)
		})
	}
}

func repoRef(adapter Adapter, owner, name string) platform.RepoRef {
	return platform.RepoRef{
		Platform: adapter.Kind,
		Host:     adapter.Host,
		Owner:    owner,
		Name:     name,
		RepoPath: owner + "/" + name,
	}
}

func repositoryJSON() []byte {
	return []byte(`{
		"id":1,
		"name":"repo",
		"full_name":"owner/repo",
		"owner":{"id":2,"login":"owner","full_name":"Owner"}
	}`)
}
