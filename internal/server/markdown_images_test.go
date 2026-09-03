package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	platformgitlab "go.kenn.io/forge/internal/platform/gitlab"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func repoBrowserRequest(
	t *testing.T,
	srv *Server,
	method, target string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestMarkdownImageRouteFetchesThroughProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const source = "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	var gotOwner, gotSource string
	fetches := 0
	mock := &mockGH{getMarkdownImageFn: func(
		_ context.Context,
		owner, _, sourceURL string,
	) (platform.MarkdownImage, error) {
		fetches++
		gotOwner, gotSource = owner, sourceURL
		return platform.MarkdownImage{Content: []byte("png-bytes"), ContentType: "image/png"}, nil
	}}
	srv, database := setupTestServerWithMock(t, mock)
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	_, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	rr := repoBrowserRequest(t, srv, http.MethodGet,
		"/api/v1/repo/github/acme/widget/markdown-image?source="+url.QueryEscape(source),
	)

	require.Equal(http.StatusOK, rr.Code)
	assert.Equal("image/png", rr.Header().Get("Content-Type"))
	assert.Equal("private, max-age=31536000, immutable", rr.Header().Get("Cache-Control"))
	assert.Equal("nosniff", rr.Header().Get("X-Content-Type-Options"))
	assert.Equal("png-bytes", rr.Body.String())
	assert.Equal("acme", gotOwner)
	assert.Equal(source, gotSource)

	cached := repoBrowserRequest(t, srv, http.MethodGet,
		"/api/v1/repo/github/acme/widget/markdown-image?source="+url.QueryEscape(source),
	)
	require.Equal(http.StatusOK, cached.Code)
	assert.Equal("png-bytes", cached.Body.String())
	assert.Equal(1, fetches)
	entries, err := os.ReadDir(srv.markdownImages.root)
	require.NoError(err)
	assert.Len(entries, 1)
}

// Production GitHub hosts are served by a RoutedClient, not a bare client, so
// this drives the markdown-image route through one. The route here is
// repo-scoped with no host fallback: the credential that owns acme/widget must
// serve the fetch, and the capability probe must not report the whole host as
// unable to read markdown images.
func TestMarkdownImageRouteFetchesThroughRoutedRepositoryCredential(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const source = "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	var gotOwner, gotRepo string
	mock := &mockGH{getMarkdownImageFn: func(
		_ context.Context,
		owner, repo, _ string,
	) (platform.MarkdownImage, error) {
		gotOwner, gotRepo = owner, repo
		return platform.MarkdownImage{
			Content: []byte("routed-bytes"), ContentType: "image/png",
		}, nil
	}}
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key:    ghclient.RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
		Client: mock,
	})
	require.NoError(err)
	routed, err := ghclient.NewRoutedClient(router)
	require.NoError(err)

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": routed},
		database, nil, defaultTestRepos, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	_, err = database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)

	rr := repoBrowserRequest(t, srv, http.MethodGet,
		"/api/v1/repo/github/acme/widget/markdown-image?source="+url.QueryEscape(source),
	)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal("image/png", rr.Header().Get("Content-Type"))
	assert.Equal("routed-bytes", rr.Body.String())
	assert.Equal("acme", gotOwner)
	assert.Equal("widget", gotRepo)
}

func TestMarkdownImageRouteMapsProviderDeadlineToUpstreamError(t *testing.T) {
	mock := &mockGH{getMarkdownImageFn: func(
		context.Context,
		string,
		string,
		string,
	) (platform.MarkdownImage, error) {
		return platform.MarkdownImage{}, context.DeadlineExceeded
	}}
	srv, database := setupTestServerWithMock(t, mock)
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	_, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(t, err)

	rr := repoBrowserRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/repo/github/acme/widget/markdown-image?source="+
			url.QueryEscape("https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"),
	)

	require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
}

func TestMarkdownImageRouteMapsGitLabServerErrorToUpstreamError(t *testing.T) {
	require := require.New(t)
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v4/projects/42/uploads/secret/private.png", r.URL.EscapedPath())
		assert.Equal(t, "gitlab-token", r.Header.Get("PRIVATE-TOKEN"))
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(gitlabServer.Close)

	provider, err := platformgitlab.NewClient(
		"gitlab.example.com",
		testTokenSource("gitlab-token"),
		platformgitlab.WithBaseURLForTesting(gitlabServer.URL+"/api/v4"),
		platformgitlab.WithoutRetriesForTesting(),
	)
	require.NoError(err)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	database := dbtest.Open(t)
	_, err = database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "42",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	srv.markdownImages = newMarkdownImageCache(t.TempDir())

	rr := repoBrowserRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/host/gitlab.example.com/repo/gitlab/group/project/markdown-image?source="+
			url.QueryEscape(gitlabServer.URL+"/group/project/uploads/secret/private.png"),
	)

	require.Equal(http.StatusBadGateway, rr.Code, rr.Body.String())
}

func TestMarkdownImageRouteResolvesOpaqueGitLabProjectID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	var paths []string
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		assert.Equal("gitlab-token", r.Header.Get("PRIVATE-TOKEN"))
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fproject":
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"id": 42,
				"path": "project",
				"path_with_namespace": "group/project",
				"name": "Project"
			}`))
			assert.NoError(err)
		case "/api/v4/projects/42/uploads/secret/private.png":
			w.Header().Set("Content-Type", "image/png")
			_, err := w.Write(imageBytes)
			assert.NoError(err)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gitlabServer.Close)

	provider, err := platformgitlab.NewClient(
		"gitlab.example.com",
		testTokenSource("gitlab-token"),
		platformgitlab.WithBaseURLForTesting(gitlabServer.URL+"/api/v4"),
		platformgitlab.WithoutRetriesForTesting(),
	)
	require.NoError(err)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	database := dbtest.Open(t)
	_, err = database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gid://gitlab/Project/4242",
		Owner:          "group",
		Name:           "project",
		RepoPath:       "group/project",
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	source := gitlabServer.URL + "/group/project/uploads/secret/private.png"

	rr := repoBrowserRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/host/gitlab.example.com/repo/gitlab/group/project/markdown-image?source="+url.QueryEscape(source),
	)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal("image/png", rr.Header().Get("Content-Type"))
	assert.Equal(imageBytes, rr.Body.Bytes())
	assert.Equal([]string{
		"/api/v4/projects/group%2Fproject",
		"/api/v4/projects/42/uploads/secret/private.png",
	}, paths)
}

// Owner/name is a mutable route. When a different repository takes over the
// route, the cache must not hand it the previous occupant's private bytes.
func TestMarkdownImageCacheDoesNotFollowRouteReuse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const source = "https://github.com/acme/widget/blob/main/docs/images/search.png?raw=true"
	fetches := 0
	mock := &mockGH{getMarkdownImageFn: func(
		context.Context, string, string, string,
	) (platform.MarkdownImage, error) {
		fetches++
		return platform.MarkdownImage{
			Content: []byte(fmt.Sprintf("bytes-%d", fetches)), ContentType: "image/png",
		}, nil
	}}
	srv, database := setupTestServerWithMock(t, mock)
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	_, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	target := "/api/v1/repo/github/acme/widget/markdown-image?source=" + url.QueryEscape(source)

	first := repoBrowserRequest(t, srv, http.MethodGet, target)
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	assert.Equal("bytes-1", first.Body.String())

	replacement := db.GitHubRepoIdentity("github.com", "acme", "widget")
	replacement.PlatformRepoID = "repo-acme-widget-replacement"
	_, _, err = database.ReconcileRepositoryObservation(t.Context(), replacement, time.Now().UTC().Add(time.Second))
	require.NoError(err)

	second := repoBrowserRequest(t, srv, http.MethodGet, target)
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	assert.Equal("bytes-2", second.Body.String(), "the replacement repository must fetch its own image")
	assert.Equal(2, fetches)
}

// Branch-addressed files change under the same URL, so the browser and the
// disk cache must both revalidate them soon; attachments stay immutable.
func TestMarkdownImageRouteCachesMutableImagesBriefly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const mutableSource = "https://github.com/acme/widget/blob/main/docs/images/search.png?raw=true"
	const immutableSource = "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	fetches := map[string]int{}
	mock := &mockGH{getMarkdownImageFn: func(
		_ context.Context, _, _ string, sourceURL string,
	) (platform.MarkdownImage, error) {
		fetches[sourceURL]++
		return platform.MarkdownImage{
			Content:     []byte("png-bytes"),
			ContentType: "image/png",
			Mutable:     sourceURL == mutableSource,
		}, nil
	}}
	srv, database := setupTestServerWithMock(t, mock)
	srv.markdownImages = newMarkdownImageCache(t.TempDir())
	_, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	request := func(source string) *httptest.ResponseRecorder {
		return repoBrowserRequest(t, srv, http.MethodGet,
			"/api/v1/repo/github/acme/widget/markdown-image?source="+url.QueryEscape(source))
	}

	mutable := request(mutableSource)
	require.Equal(http.StatusOK, mutable.Code, mutable.Body.String())
	assert.Equal("private, max-age=300", mutable.Header().Get("Cache-Control"))
	immutable := request(immutableSource)
	require.Equal(http.StatusOK, immutable.Code, immutable.Body.String())
	assert.Equal("private, max-age=31536000, immutable", immutable.Header().Get("Cache-Control"))

	entries, err := os.ReadDir(srv.markdownImages.root)
	require.NoError(err)
	require.Len(entries, 2)
	aged := time.Now().Add(-markdownImageMutableTTL - time.Minute)
	for _, entry := range entries {
		require.NoError(os.Chtimes(filepath.Join(srv.markdownImages.root, entry.Name()), aged, aged))
	}

	require.Equal(http.StatusOK, request(mutableSource).Code)
	require.Equal(http.StatusOK, request(immutableSource).Code)
	assert.Equal(2, fetches[mutableSource], "a mutable entry past its short TTL is fetched again")
	assert.Equal(1, fetches[immutableSource], "an immutable entry is still served from disk")
}
