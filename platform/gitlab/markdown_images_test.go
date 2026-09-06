package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestGetMarkdownImageUsesAuthenticatedProjectUploadAPI(t *testing.T) {
	assert := assert.New(t)
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v4/projects/42/uploads/0123456789abcdef/private-image.png", r.URL.Path)
		assert.Equal("gitlab-token", r.Header.Get("PRIVATE-TOKEN"))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, err := w.Write(imageBytes)
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		server.Listener.Addr().String(),
		testTokenSource("gitlab-token"),
		WithBaseURLForTesting(server.URL+"/api/v4"), WithTransport(http.DefaultTransport))
	require.NoError(t, err)
	image, err := client.GetMarkdownImage(t.Context(), platform.RepoRef{
		Platform: platform.KindGitLab, Host: server.Listener.Addr().String(),
		RepoPath: "group/project", PlatformID: 42,
	}, server.URL+"/-/project/42/uploads/0123456789abcdef/private-image.png")
	require.NoError(t, err)
	assert.Equal("image/png", image.ContentType)
	assert.Equal(imageBytes, image.Content)
}

func TestGetMarkdownImageUsesForegroundTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(
		server.Listener.Addr().String(),
		testTokenSource("gitlab-token"),
		WithBaseURLForTesting(server.URL+"/api/v4"),
		WithForegroundTimeoutForTesting(time.Nanosecond), WithTransport(http.DefaultTransport))
	require.NoError(t, err)
	_, err = client.GetMarkdownImage(t.Context(), platform.RepoRef{
		Platform: platform.KindGitLab, RepoPath: "group/project", PlatformID: 42,
	}, server.URL+"/group/project/uploads/secret/private.png")

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestGetMarkdownImageRejectsUntrustedSourcesAndActiveContent(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		contentType string
	}{
		{name: "different repository", source: "/other/project/uploads/secret/image.png", contentType: "image/png"},
		{name: "active content", source: "/group/project/uploads/secret/image.svg", contentType: "image/svg+xml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, err := w.Write([]byte("<svg></svg>"))
				assert.NoError(err)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(
				server.Listener.Addr().String(), testTokenSource("token"),
				WithBaseURLForTesting(server.URL+"/api/v4"), WithTransport(http.DefaultTransport))
			require.NoError(err)
			_, err = client.GetMarkdownImage(context.Background(), platform.RepoRef{
				Platform: platform.KindGitLab, RepoPath: "group/project", PlatformID: 42,
			}, server.URL+tc.source)
			require.Error(err)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(platform.ErrCodeInvalidArgument, platformErr.Code)
		})
	}
}
