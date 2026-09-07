package gitealike

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestMergeableCaptureTransportOnlyCapturesPullRequestJSON(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	cache := NewMergeableCache()
	body := []byte(`[
		{
			"html_url":"https://gitea.test/owner/repo/pulls/1",
			"mergeable":false,
			"head":{"sha":"head-a"},
			"base":{"sha":"base-a"}
		}
	]`)
	transport := &MergeableCaptureTransport{
		Cache: cache,
		Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://gitea.test/api/v1/repos/owner/repo/pulls", nil)
	require.NoError(err)
	resp, err := transport.RoundTrip(req)
	require.NoError(err)
	copiedBody, err := io.ReadAll(resp.Body)
	require.NoError(err)

	assert.Equal(body, copiedBody)
	mergeable, ok := cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/1", "head-a", "base-a")
	require.True(ok)
	require.NotNil(mergeable)
	assert.False(*mergeable)
}

func TestMergeableCacheCapturesKnownAndUnknownValues(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	cache := NewMergeableCache()
	cache.CapturePullRequestJSON([]byte(`[
		{"html_url":"https://gitea.test/owner/repo/pulls/1","mergeable":false,"head":{"sha":"head-a"},"base":{"sha":"base-a"}},
		{"html_url":"https://gitea.test/owner/repo/pulls/2","mergeable":true,"head":{"sha":"head-b"},"base":{"sha":"base-a"}},
		{"html_url":"https://gitea.test/owner/repo/pulls/3","head":{"sha":"head-c"},"base":{"sha":"base-a"}}
	]`))

	mergeable, ok := cache.MergeableForPullRequest(
		"https://gitea.test/owner/repo/pulls/1",
		"head-a",
		"base-a",
	)
	require.True(ok)
	require.NotNil(mergeable)
	assert.False(*mergeable)

	mergeable, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/2", "head-b", "base-a")
	require.True(ok)
	require.NotNil(mergeable)
	assert.True(*mergeable)

	mergeable, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/3", "head-c", "base-a")
	assert.True(ok)
	assert.Nil(mergeable)

	_, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/1", "head-b", "base-a")
	assert.False(ok)
	_, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/1", "head-a", "base-b")
	assert.False(ok)
	_, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/1", "", "base-a")
	assert.False(ok)
	_, ok = cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/4", "head-d", "base-a")
	assert.False(ok)
}

func TestMergeableCachePreservesDiffMetricPresence(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	cache := NewMergeableCache()
	cache.CapturePullRequestJSON([]byte(`{
		"html_url":"https://gitea.test/owner/repo/pulls/1",
		"head":{"sha":"head-a"},"base":{"sha":"base-a"},
		"additions":0,"deletions":5
	}`))

	metrics, ok := cache.MetricsForPullRequest(
		"https://gitea.test/owner/repo/pulls/1", "head-a", "base-a",
	)
	require.True(ok)
	assert.True(metrics.AdditionsKnown)
	assert.Zero(metrics.Additions)
	assert.True(metrics.DeletionsKnown)
	assert.Equal(5, metrics.Deletions)
	assert.Nil(metrics.FilesChanged)
}

func TestShouldCaptureMergeable(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		status      int
		contentType string
		want        bool
	}{
		{
			name:        "list",
			method:      http.MethodGet,
			path:        "/api/v1/repos/owner/repo/pulls",
			status:      http.StatusOK,
			contentType: "application/json",
			want:        true,
		},
		{
			name:        "detail mutation",
			method:      http.MethodPatch,
			path:        "/api/v1/repos/owner/repo/pulls/1",
			status:      http.StatusOK,
			contentType: "application/json",
			want:        true,
		},
		{
			name:        "subpath before api route",
			method:      http.MethodGet,
			path:        "/repos/api/v1/repos/owner/repo/pulls",
			status:      http.StatusOK,
			contentType: "application/json",
			want:        true,
		},
		{
			name:        "issues",
			method:      http.MethodGet,
			path:        "/api/v1/repos/owner/repo/issues",
			status:      http.StatusOK,
			contentType: "application/json",
			want:        false,
		},
		{
			name:        "non json",
			method:      http.MethodGet,
			path:        "/api/v1/repos/owner/repo/pulls",
			status:      http.StatusOK,
			contentType: "text/plain",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, "https://gitea.test"+tt.path, nil)
			Require.NoError(t, err)
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
			}
			assert.Equal(t, tt.want, shouldCaptureMergeable(req, resp))
		})
	}
}

func TestMergeableCaptureTransportSkipsOversizedBodiesWithoutConsumingThem(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	cache := NewMergeableCache()
	body := []byte(`{"html_url":"https://gitea.test/owner/repo/pulls/1","mergeable":false,"padding":"` +
		strings.Repeat("x", mergeableCaptureMaxBodyBytes) + `"}`)
	transport := &MergeableCaptureTransport{
		Cache: cache,
		Base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://gitea.test/api/v1/repos/owner/repo/pulls/1", nil)
	require.NoError(err)
	resp, err := transport.RoundTrip(req)
	require.NoError(err)
	copiedBody, err := io.ReadAll(resp.Body)
	require.NoError(err)

	assert.Equal(body, copiedBody)
	_, ok := cache.MergeableForPullRequest("https://gitea.test/owner/repo/pulls/1", "", "")
	assert.False(ok)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
