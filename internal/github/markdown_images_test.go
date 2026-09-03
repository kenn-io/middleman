package github

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/platform"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

type contentsRequest struct {
	Path   string
	Ref    string
	Accept string
}

// contentsServer serves the contents API for one repository file that lives on
// a branch whose name contains a slash, the case a web URL cannot delimit.
func contentsServer(t *testing.T, body []byte) (*httptest.Server, func() []contentsRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []contentsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, contentsRequest{
			Path: r.URL.Path, Ref: r.URL.Query().Get("ref"), Accept: r.Header.Get("Accept"),
		})
		mu.Unlock()
		if r.URL.Path != "/api/v3/repos/acme/widgets/contents/docs/images/search.png" ||
			r.URL.Query().Get("ref") != "feat/search-controls" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.raw; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, func() []contentsRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]contentsRequest(nil), requests...)
	}
}

func newMarkdownImageTestClient(t *testing.T, server *httptest.Server) *liveClient {
	t.Helper()
	client, err := NewClient(testTokenSource("token"), "github.com", nil, nil, WithBaseURLForTesting(server.URL))
	require.NoError(t, err)
	return client.(*liveClient)
}

func TestGetMarkdownImageResolvesRepositoryFileAcrossSlashedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := contentsServer(t, pngBytes)
	client := newMarkdownImageTestClient(t, server)

	image, err := client.GetMarkdownImage(t.Context(), "acme", "widgets",
		"https://github.com/acme/widgets/blob/feat/search-controls/docs/images/search.png?raw=true")
	require.NoError(err)
	assert.Equal("image/png", image.ContentType)
	assert.Equal(pngBytes, image.Content)
	assert.True(image.Mutable, "branch-addressed files change under the same URL")

	got := requests()
	require.Len(got, 2, "the shortest ref candidate misses before the slashed branch resolves")
	assert.Equal(contentsRequest{
		Path:   "/api/v3/repos/acme/widgets/contents/search-controls/docs/images/search.png",
		Ref:    "feat",
		Accept: "application/vnd.github.raw+json",
	}, got[0])
	assert.Equal(contentsRequest{
		Path:   "/api/v3/repos/acme/widgets/contents/docs/images/search.png",
		Ref:    "feat/search-controls",
		Accept: "application/vnd.github.raw+json",
	}, got[1])
}

func TestGetMarkdownImageAcceptsRawAndRawHostRepositoryURLs(t *testing.T) {
	server, _ := contentsServer(t, pngBytes)
	client := newMarkdownImageTestClient(t, server)

	for _, source := range []string{
		"https://github.com/acme/widgets/raw/feat/search-controls/docs/images/search.png",
		"https://raw.githubusercontent.com/acme/widgets/feat/search-controls/docs/images/search.png",
		"https://raw.githubusercontent.com/Acme/Widgets/feat/search-controls/docs/images/search.png",
	} {
		image, err := client.GetMarkdownImage(t.Context(), "acme", "widgets", source)
		require.NoError(t, err, source)
		assert.Equal(t, "image/png", image.ContentType, source)
	}
}

func TestGetMarkdownImageRejectsFilesOutsideRouteRepository(t *testing.T) {
	server, requests := contentsServer(t, pngBytes)
	client := newMarkdownImageTestClient(t, server)

	for _, source := range []string{
		"https://github.com/acme/other/blob/main/docs/images/search.png?raw=true",
		"https://raw.githubusercontent.com/acme/other/main/docs/images/search.png",
		"https://github.com/acme/widgets/tree/main/docs",
		"https://github.com/acme/widgets/blob/main",
		"https://github.com/acme/widgets/blob/main/../secret.png",
		"https://gist.githubusercontent.com/acme/widgets/main/docs/images/search.png",
	} {
		_, err := client.GetMarkdownImage(t.Context(), "acme", "widgets", source)
		var providerErr *platform.Error
		require.ErrorAs(t, err, &providerErr, source)
		assert.Equal(t, platform.ErrCodeInvalidArgument, providerErr.Code, source)
	}
	assert.Empty(t, requests(), "rejected sources must not reach the provider")
}

func TestGetMarkdownImageReportsMissingRepositoryFile(t *testing.T) {
	server, requests := contentsServer(t, pngBytes)
	client := newMarkdownImageTestClient(t, server)

	_, err := client.GetMarkdownImage(t.Context(), "acme", "widgets",
		"https://github.com/acme/widgets/blob/main/docs/images/missing.png?raw=true")
	var providerErr *platform.Error
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, platform.ErrCodeNotFound, providerErr.Code)
	assert.Len(t, requests(), 3, "every ref split is tried before reporting not found")
}

func TestGetMarkdownImageRejectsNonImageRepositoryFile(t *testing.T) {
	server, _ := contentsServer(t, []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"))
	client := newMarkdownImageTestClient(t, server)

	_, err := client.GetMarkdownImage(t.Context(), "acme", "widgets",
		"https://github.com/acme/widgets/blob/feat/search-controls/docs/images/search.png?raw=true")
	var providerErr *platform.Error
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, platform.ErrCodeInvalidArgument, providerErr.Code)
}

func TestGetMarkdownImageRejectsOversizedRepositoryFile(t *testing.T) {
	oversized := make([]byte, maxMarkdownImageBytes+1)
	copy(oversized, pngBytes)
	server, _ := contentsServer(t, oversized)
	client := newMarkdownImageTestClient(t, server)

	_, err := client.GetMarkdownImage(t.Context(), "acme", "widgets",
		"https://github.com/acme/widgets/blob/feat/search-controls/docs/images/search.png?raw=true")
	require.ErrorIs(t, err, errMarkdownImageTooLarge)
}
