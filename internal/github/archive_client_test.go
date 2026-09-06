package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func readHistoricalIssuePage(
	provider *platformgithub.Provider,
	ctx context.Context,
	ref platform.RepoRef,
	cursor string,
) (platform.Page[platform.Issue], error) {
	return provider.ListIssuesPage(ctx, ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated, Cursor: cursor,
	})
}

func readUpdatedIssuePage(
	provider *platformgithub.Provider,
	ctx context.Context,
	ref platform.RepoRef,
	since time.Time,
	cursor string,
) (platform.Page[platform.Issue], error) {
	return provider.ListIssuesPage(ctx, ref, platform.ItemPageQuery{
		Order:        platform.ItemOrderUpdated,
		UpdatedSince: &since, Cursor: cursor,
	})
}

func readHistoricalMergeRequestPage(
	provider *platformgithub.Provider,
	ctx context.Context,
	ref platform.RepoRef,
	cursor string,
) (platform.Page[platform.MergeRequest], error) {
	return provider.ListMergeRequestsPage(ctx, ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated, Cursor: cursor,
	})
}

func TestGitHubArchiveDiscoveryParityUsesOldestFirstIssueOnlyConnection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/api/graphql", r.URL.Path)
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		assert.NoError(json.NewDecoder(r.Body).Decode(&request))
		assert.Contains(request.Query, "issues(first: 100")
		assert.NotContains(request.Query, "pullRequests")
		assert.Equal("CREATED_AT", request.Variables["orderField"])
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			assert.Nil(request.Variables["cursor"])
			_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[
				{"id":"I_kw1","databaseId":101,"number":1,"title":"old issue","state":"CLOSED","body":"body","url":"https://github.com/acme/widget/issues/1","author":{"login":"author"},"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2025-01-02T00:00:00Z","closedAt":"2025-01-02T00:00:00Z","comments":{"totalCount":2},"labels":{"nodes":[]},"assignees":{"nodes":[]}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"issue-1"}}}}}`))
			return
		}
		assert.Equal("issue-1", request.Variables["cursor"])
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[
			{"id":"I_kw3","databaseId":103,"number":3,"title":"newer issue","state":"OPEN","body":"","url":"https://github.com/acme/widget/issues/3","author":{"login":"author"},"createdAt":"2025-01-03T00:00:00Z","updatedAt":"2025-01-04T00:00:00Z","closedAt":null,"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
	}))
	defer srv.Close()

	provider := newArchiveTestGitHubProvider(t, srv.URL)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	first, err := readHistoricalIssuePage(provider, t.Context(), ref, "")
	require.NoError(err)
	require.Len(first.Items, 1)
	assert.Equal(1, first.Items[0].Number)
	assert.NotEmpty(first.NextCursor)
	assert.False(first.Exhausted)

	second, err := readHistoricalIssuePage(provider, t.Context(), ref, first.NextCursor)
	require.NoError(err)
	require.Len(second.Items, 1)
	assert.Equal(3, second.Items[0].Number)
	assert.Equal("I_kw3", second.Items[0].PlatformExternalID)
	assert.Empty(second.NextCursor)
	assert.True(second.Exhausted)
	assert.Equal(2, requests)
}

func TestGitHubArchiveUpdatedIssuesUseInclusiveWatermarkAndStableContinuation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		assert.NoError(json.NewDecoder(r.Body).Decode(&request))
		assert.Equal("UPDATED_AT", request.Variables["orderField"])
		assert.Contains(request.Query, "filterBy: {since: $since}")
		assert.Equal(watermark.Add(-time.Second).Format(time.RFC3339Nano), request.Variables["since"])
		w.Header().Set("Content-Type", "application/json")
		cursor := any(nil)
		more := requests == 1
		if requests == 1 {
			cursor = "updated-1"
		}
		cursorJSON, err := json.Marshal(cursor)
		assert.NoError(err)
		_, _ = fmt.Fprintf(w, `{"data":{"repository":{"issues":{"nodes":[{"id":"I_%d","databaseId":%d,"number":%d,"title":"item","state":"OPEN","body":"","url":"https://github.com/acme/widget/issues/%d","author":{"login":"author"},"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2026-07-01T02:03:04Z","closedAt":null,"comments":{"totalCount":0},"labels":{"nodes":[]},"assignees":{"nodes":[]}}],"pageInfo":{"hasNextPage":%t,"endCursor":%s}}}}}`, requests, requests, requests, requests, more, cursorJSON)
	}))
	defer srv.Close()

	provider := newArchiveTestGitHubProvider(t, srv.URL)
	ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.com", Owner: "acme", Name: "widget", RepoPath: "acme/widget"}
	first, err := readUpdatedIssuePage(provider, t.Context(), ref, watermark, "")
	require.NoError(err)
	_, err = readUpdatedIssuePage(provider, t.Context(), ref, watermark.Add(time.Second), first.NextCursor)
	require.ErrorIs(err, platform.ErrProviderContract)
	otherHost := ref
	otherHost.Host = "github.example.com"
	_, err = readUpdatedIssuePage(provider, t.Context(), otherHost, watermark, first.NextCursor)
	require.ErrorIs(err, platform.ErrProviderContract)
	second, err := readUpdatedIssuePage(provider, t.Context(), ref, watermark, first.NextCursor)
	require.NoError(err)
	assert.Equal([]int{1}, []int{first.Items[0].Number})
	assert.Equal([]int{2}, []int{second.Items[0].Number})
	assert.True(second.Exhausted)
	assert.Equal(2, requests)
}
func TestGitHubArchiveRESTErrorsCarryProviderClassification(t *testing.T) {
	t.Parallel()
	resetAt := time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    int
		headers   http.Header
		want      error
		wantReset *time.Time
	}{
		{name: "authentication", status: http.StatusUnauthorized, want: platform.ErrPermissionDenied},
		{name: "rate limit", status: http.StatusForbidden, headers: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Limit":     []string{"5000"},
			"X-Ratelimit-Reset":     []string{fmt.Sprint(resetAt.Unix())},
		}, want: platform.ErrRateLimited, wantReset: &resetAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, values := range tc.headers {
					for _, value := range values {
						w.Header().Add(name, value)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"denied"}`))
			}))
			t.Cleanup(srv.Close)
			provider := newArchiveTestGitHubProvider(t, srv.URL)
			ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.com", Owner: "acme", Name: "widget", RepoPath: "acme/widget"}

			_, err := readHistoricalMergeRequestPage(provider, t.Context(), ref, "")
			require.ErrorIs(err, tc.want)
			var providerErr *platform.Error
			require.ErrorAs(err, &providerErr)
			assert.Equal(platform.KindGitHub, providerErr.Provider)
			assert.Equal("github.com", providerErr.PlatformHost)
			assert.Equal(tc.wantReset, providerErr.ResetAt)
		})
	}
}

func TestGitHubArchiveGraphQLErrorsCarryProviderClassification(t *testing.T) {
	t.Parallel()
	resetAt := time.Date(2026, 7, 14, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		headers   http.Header
		want      error
		wantReset *time.Time
	}{
		{name: "authentication", status: http.StatusUnauthorized, body: `{"message":"bad credentials"}`, want: platform.ErrPermissionDenied},
		{name: "rate limit response", status: http.StatusOK, body: `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, headers: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Limit":     []string{"5000"},
			"X-Ratelimit-Reset":     []string{fmt.Sprint(resetAt.Unix())},
		}, want: platform.ErrRateLimited, wantReset: &resetAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, values := range tc.headers {
					for _, value := range values {
						w.Header().Add(name, value)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			provider := newArchiveTestGitHubProvider(t, srv.URL)
			ref := platform.RepoRef{Platform: platform.KindGitHub, Host: "github.com", Owner: "acme", Name: "widget", RepoPath: "acme/widget"}

			_, err := readHistoricalIssuePage(provider, t.Context(), ref, "")
			require.ErrorIs(err, tc.want)
			var providerErr *platform.Error
			require.ErrorAs(err, &providerErr)
			assert.Equal(platform.KindGitHub, providerErr.Provider)
			assert.Equal("github.com", providerErr.PlatformHost)
			assert.Equal(tc.wantReset, providerErr.ResetAt)
		})
	}
}

func TestGitHubArchiveCapabilitiesRequireBoundedClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	live := newArchiveTestGitHubProvider(t, newEmptyArchiveServer(t).URL)
	assert.Equal(platform.ArchiveCapabilities{
		HistoricalIssues: true, HistoricalMergeRequests: true,
		OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
	}, live.Capabilities().Archive)
	assert.Equal(platform.ArchiveCapabilities{}, (newTestGitHubProvider(t, "github.com", &mockClient{})).Capabilities().Archive)
	registry, err := platform.NewRegistry(live)
	require.NoError(err)
	issueReader, err := registry.IssuePageReader(platform.KindGitHub, "github.com")
	require.NoError(err)
	assert.NotNil(issueReader)
	mergeRequestReader, err := registry.MergeRequestPageReader(platform.KindGitHub, "github.com")
	require.NoError(err)
	assert.NotNil(mergeRequestReader)
	_, err = registry.IssuePageReader(platform.KindGitHub, "github.example.com")
	require.Error(err)
}

func newEmptyArchiveServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	return server
}

func newArchiveTestGitHubProvider(t *testing.T, baseURL string) *platformgithub.Provider {
	t.Helper()
	client, err := NewClient(
		testTokenSource("archive-token"), "github.com", nil, nil,
		WithBaseURLForTesting(baseURL),
	)
	require.NoError(t, err)
	return newTestGitHubProvider(t, "github.com", client)
}

func srvURL(r *http.Request) string {
	return "http://" + r.Host
}
