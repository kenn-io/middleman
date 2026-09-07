package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	platformgithub "go.kenn.io/forge/platform/github"
)

var _ Client = (*platformgithub.Client)(nil)

func newEnterpriseGHClient(hc *http.Client, baseURL, uploadURL string) (*gh.Client, error) {
	return gh.NewClient(gh.WithHTTPClient(hc), gh.WithEnterpriseURLs(baseURL, uploadURL))
}

func (m *mockClient) ListPullRequestTimelineEvents(
	_ context.Context, _, _ string, _ int,
) ([]platformgithub.PullRequestTimelineEvent, error) {
	m.trackCall()
	if m.timelineEventsErr != nil {
		return nil, m.timelineEventsErr
	}
	return m.timelineEvents, nil
}

func TestNewClientReturnsNonNil(t *testing.T) {
	c, err := NewClient(testTokenSource("fake-token"), "", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewClientEnterprise(t *testing.T) {
	c, err := NewClient(testTokenSource("test-token"), "github.mycompany.com", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewClientGitHubDotCom(t *testing.T) {
	c, err := NewClient(testTokenSource("test-token"), "github.com", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewClientEmptyHost(t *testing.T) {
	c, err := NewClient(testTokenSource("test-token"), "", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNativeStackClientDecodesPullHintsAndStackPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := "2026-07-24T12:00:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/repos/acme/widgets/pulls":
			assert.Equal("open", r.URL.Query().Get("state"))
			_, _ = io.WriteString(w, `[
  {"number":101,"title":"base","state":"open","stack":{"id":987,"number":42,"size":2,"position":1,"base":{"ref":"main","sha":"base"}}},
  {"number":102,"title":"standalone","state":"open","stack":null}
]`)
		case "/api/v3/repos/acme/widgets/stacks":
			assert.Equal("3", r.URL.Query().Get("page"))
			w.Header().Set("Link", "<https://api.github.com/repos/acme/widgets/stacks?per_page=100&page=4>; rel=\"next\"")
			_, _ = io.WriteString(w, `[{"id":987,"number":42,"base":{"ref":"main"},"open":true,"created_at":"`+createdAt+`","pull_requests":[{"number":101,"state":"open","draft":false,"merged_at":null,"head":{"ref":"feature/a","sha":"aaa"}},{"number":103,"state":"open","draft":true,"merged_at":null,"head":{"ref":"feature/b","sha":"bbb"}}]}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(testTokenSource("token"), "github.com", nil, nil, WithBaseURLForTesting(server.URL))
	require.NoError(err)
	nativeClient, ok := client.(platformgithub.NativeStackClient)
	require.True(ok)

	prs, hints, err := nativeClient.ListOpenPullRequestsWithNativeStackHints(t.Context(), "acme", "widgets")
	require.NoError(err)
	require.Len(prs, 2)
	require.NotNil(hints[101])
	assert.Equal(platformgithub.NativeStackHint{ID: 987, Number: 42, Size: 2, Position: 1, BaseRef: "main"}, *hints[101])
	assert.Contains(hints, 102)
	assert.Nil(hints[102])

	page, err := nativeClient.ListNativeStacksPage(t.Context(), "acme", "widgets", 3)
	require.NoError(err)
	assert.Equal(4, page.NextPage)
	require.Len(page.Stacks, 1)
	assert.Equal(42, page.Stacks[0].Number)
	assert.Equal("main", page.Stacks[0].BaseRef)
	require.Len(page.Stacks[0].Members, 2)
	assert.Equal(103, page.Stacks[0].Members[1].PullRequestNumber)
	assert.Equal(2, page.Stacks[0].Members[1].Position)
}

func TestGraphQLEndpointForHost(t *testing.T) {
	require.Equal(t, "https://api.github.com/graphql", graphQLEndpointForHost(""))
	require.Equal(t, "https://api.github.com/graphql", graphQLEndpointForHost("github.com"))
	require.Equal(t, "https://github.example.com/api/graphql", graphQLEndpointForHost("github.example.com"))
}

func TestClientInterfaceIncludesListForcePushEvents(t *testing.T) {
	_, ok := reflect.TypeFor[Client]().MethodByName("ListForcePushEvents")
	require.True(t, ok)
}

func TestClientInterfaceIncludesListPullRequestTimelineEvents(t *testing.T) {
	_, ok := reflect.TypeFor[Client]().MethodByName("ListPullRequestTimelineEvents")
	require.True(t, ok)
}

func TestClientInterfaceIncludesListPullRequestReviewThreads(t *testing.T) {
	_, ok := reflect.TypeFor[Client]().MethodByName("ListPullRequestReviewThreads")
	require.True(t, ok)
}

func TestListOpenIssuesLogsFetchProgressForPaginatedIssueSet(t *testing.T) {
	require := require.New(t)
	logs := captureDefaultLogs(t)

	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widgets/issues", func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page == 0 {
			page = 1
		}
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(
				`<%s/api/v3/repos/acme/widgets/issues?page=2&per_page=100>; rel="next"`, serverURL,
			))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testIssuePage(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	serverURL = srv.URL

	client, err := NewClient(testTokenSource("token"), "github.com", nil, nil, WithBaseURLForTesting(srv.URL))
	require.NoError(err)

	issues, err := client.ListOpenIssues(t.Context(), "acme", "widgets")
	require.NoError(err)
	require.Len(issues, 2)
	require.Contains(logs.String(), `msg="issue list fetch started"`)
	require.Contains(logs.String(), `msg="issue list fetch completed"`)
	require.Contains(logs.String(), "source=rest")
}

func testIssuePage(page int) []map[string]any {
	return []map[string]any{{
		"id":         page * 1000,
		"number":     page,
		"title":      fmt.Sprintf("Issue %d", page),
		"state":      "open",
		"html_url":   fmt.Sprintf("https://github.com/acme/widgets/issues/%d", page),
		"user":       map[string]any{"login": "alice"},
		"created_at": "2026-05-20T12:00:00Z",
		"updated_at": "2026-05-20T12:00:00Z",
	}}
}

func TestListOpenPullRequestsLogsFetchProgressForPaginatedPullRequestSet(t *testing.T) {
	require := require.New(t)
	logs := captureDefaultLogs(t)

	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/conda-forge/staged-recipes/pulls", func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page == 0 {
			page = 1
		}
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(
				`<%s/api/v3/repos/conda-forge/staged-recipes/pulls?page=2&per_page=100>; rel="next"`, serverURL,
			))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testPullRequestPage(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	serverURL = srv.URL

	client, err := NewClient(testTokenSource("token"), "github.com", nil, nil, WithBaseURLForTesting(srv.URL))
	require.NoError(err)

	prs, err := client.ListOpenPullRequests(t.Context(), "conda-forge", "staged-recipes")
	require.NoError(err)
	require.Len(prs, 2)
	require.Contains(logs.String(), `msg="merge request list fetch started"`)
	require.Contains(logs.String(), `msg="merge request list fetch completed"`)
	require.Contains(logs.String(), "source=rest")
}

func testPullRequestPage(page int) []map[string]any {
	return []map[string]any{{
		"id":         page * 1000,
		"number":     page,
		"title":      fmt.Sprintf("Pull request %d", page),
		"state":      "open",
		"html_url":   fmt.Sprintf("https://github.com/conda-forge/staged-recipes/pull/%d", page),
		"user":       map[string]any{"login": "alice"},
		"created_at": "2026-05-20T12:00:00Z",
		"updated_at": "2026-05-20T12:00:00Z",
		"head":       map[string]any{"ref": "recipe", "sha": "abc123"},
		"base":       map[string]any{"ref": "main", "sha": "def456"},
	}}
}

// Exercise cache installation through the application-built client's requests.
func TestNewClientWiresETagTransport(t *testing.T) {
	require := require.New(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			assert.Equal(t, "\"version-1\"", r.Header.Get("If-None-Match"))
			w.WriteHeader(http.StatusNotModified)
			return
		}
		assert.Empty(t, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", "\"version-1\"")
		_, _ = io.WriteString(w, "[]")
	}))
	defer server.Close()
	client, err := NewClient(testTokenSource("token"), "github.com", nil, nil, WithBaseURLForTesting(server.URL))
	require.NoError(err)
	_, err = client.ListOpenPullRequests(t.Context(), "example", "project")
	require.NoError(err)
	_, err = client.ListOpenPullRequests(t.Context(), "example", "project")
	require.True(platformgithub.IsNotModified(err))
	client.InvalidateListETagsForRepo("example", "project", "pulls")
	_, err = client.ListOpenPullRequests(t.Context(), "example", "project")
	require.NoError(err)
	require.Equal(3, calls)
}
