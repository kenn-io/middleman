package gitlab

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// gitlabStatusError builds a *gitlab.ErrorResponse whose embedded request URL
// uses the given host. go-gitlab renders such errors as
// "GET <scheme>://<host><path>: <status> <message>", so host is where an
// ephemeral httptest port (or any digits) can leak into err.Error(). go-gitlab
// returns this typed shape for every error status except 404, which comes back
// as the gitlab.ErrNotFound sentinel instead.
func gitlabStatusError(host string, status int) *gitlab.ErrorResponse {
	return &gitlab.ErrorResponse{
		StatusCode: status,
		Message:    "boom",
		Response: &http.Response{
			StatusCode: status,
			Request: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Scheme: "http", Host: host, Path: "/api/v4/projects/77"},
			},
		},
	}
}

// Regression: a transient fork-head lookup failure must never be classified as a
// downgradeable (not-found/forbidden) error just because the request URL's
// host:port happens to contain "403"/"404". Before the fix, isGitLabStatus fell
// back to strings.Contains(err.Error(), "404"), so an ephemeral httptest port
// like 127.0.0.1:40404 turned a 429 into a silently-swallowed lookup, flaking
// TestClientListOpenMergeRequestsPropagatesTransientForkHeadRepoLookupFailures.
func TestUnavailableSourceProjectIgnoresStatusDigitsInErrorURL(t *testing.T) {
	assert := assert.New(t)
	cases := []struct {
		name          string
		err           error
		wantDowngrade bool
	}{
		// Transient failures must propagate even when the request URL's host:port
		// contains the digits "404"/"403" (e.g. an ephemeral httptest port).
		{"rate limited on 404-bearing port", gitlabStatusError("127.0.0.1:40404", http.StatusTooManyRequests), false},
		{"rate limited on 403-bearing port", gitlabStatusError("127.0.0.1:54033", http.StatusTooManyRequests), false},
		{"bad gateway on 404-bearing port", gitlabStatusError("127.0.0.1:40404", http.StatusBadGateway), false},
		{"server error on 403-bearing port", gitlabStatusError("127.0.0.1:40430", http.StatusInternalServerError), false},
		// Genuine not-found/forbidden still downgrade (fork identity is dropped).
		// 404 arrives as go-gitlab's sentinel, 403 as a typed *gitlab.ErrorResponse.
		{"genuine not found (sentinel)", gitlab.ErrNotFound, true},
		{"genuine forbidden", gitlabStatusError("127.0.0.1:51234", http.StatusForbidden), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the real call path: projectCloneURL wraps the SDK error
			// via mapGitLabError before it reaches isUnavailableSourceProjectError.
			wrapped := mapGitLabError("get_source_project", tc.err)
			assert.Equal(tc.wantDowngrade, isUnavailableSourceProjectError(wrapped),
				"raw err: %q", tc.err.Error())
		})
	}
}
