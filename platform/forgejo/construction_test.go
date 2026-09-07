package forgejo

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestConstructionDoesNotProbeProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	calls := 0
	transport := platform.RoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("fixture provider unavailable")
	})
	client, err := NewClient("code.example.org", testTokenSource("fixture-token"), WithTransport(transport))
	require.NoError(err)
	assert.Zero(calls)
	_, err = client.GetRepository(t.Context(), platform.RepoRef{Platform: platform.KindForgejo, Host: "code.example.org", Owner: "team-a", Name: "project-a"})
	require.Error(err)
	assert.Positive(calls)
}

func TestServerVersionRequiresExplicitRead(t *testing.T) {
	assert := assert.New(t)
	var paths []string
	transport := platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"version":"13.0.0+gitea-1.26.0"}`)), Request: req}, nil
	})
	client, err := NewClient("code.example.org", testTokenSource("fixture-token"), WithTransport(transport))
	require.NoError(t, err)
	assert.Empty(paths)
	version, err := client.ServerVersion(t.Context())
	require.NoError(t, err)
	assert.Equal("13.0.0+gitea-1.26.0", version)
	assert.Equal([]string{"/api/v1/version"}, paths)
	_, err = NewClient("code.example.org", testTokenSource("fixture-token"), WithTransport(transport), WithServerVersion(version))
	require.NoError(t, err)
	assert.Len(paths, 1)
}
