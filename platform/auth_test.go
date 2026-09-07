package platform_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

type sequenceSource struct {
	tokens            []string
	invalidated       int
	invalidatedTokens []string
}

func (s *sequenceSource) Token(context.Context) (string, error) {
	token := s.tokens[0]
	if len(s.tokens) > 1 {
		s.tokens = s.tokens[1:]
	}
	return token, nil
}

func (s *sequenceSource) Invalidate(token string) {
	s.invalidated++
	s.invalidatedTokens = append(s.invalidatedTokens, token)
}

func TestAuthTransportReadsTokenEachRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"first", "second"}}
	var auth []string
	rt := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			auth = append(auth, req.Header.Get("Authorization"))
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader: platform.BearerAuthHeader,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://api.example.test", nil,
	)
	require.NoError(err)
	_, err = rt.RoundTrip(req)
	require.NoError(err)
	_, err = rt.RoundTrip(req)
	require.NoError(err)

	assert.Equal([]string{"Bearer first", "Bearer second"}, auth)
}

func TestRetryOnUnauthorizedInvalidatesAndRetriesOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"old", "new"}}
	var auth []string
	calls := 0
	rt := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			auth = append(auth, req.Header.Get("Authorization"))
			status := http.StatusUnauthorized
			if calls == 2 {
				status = http.StatusOK
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://api.example.test", nil,
	)
	require.NoError(err)
	resp, err := rt.RoundTrip(req)
	require.NoError(err)

	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal(1, src.invalidated)
	assert.Equal([]string{"old"}, src.invalidatedTokens)
	assert.Equal([]string{"Bearer old", "Bearer new"}, auth)
}

func TestRetryOnUnauthorizedDoesNotRetryForbidden(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"old", "new"}}
	calls := 0
	rt := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://api.example.test", nil,
	)
	require.NoError(err)
	resp, err := rt.RoundTrip(req)
	require.NoError(err)

	assert.Equal(http.StatusForbidden, resp.StatusCode)
	assert.Equal(1, calls)
	assert.Equal(0, src.invalidated)
}

func TestRetryOnUnauthorizedReplaysGetBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"old", "new"}}
	var bodies []string
	calls := 0
	rt := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(req.Body)
			require.NoError(err)
			bodies = append(bodies, string(body))
			status := http.StatusUnauthorized
			if calls == 2 {
				status = http.StatusOK
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://api.example.test",
		strings.NewReader("payload"),
	)
	require.NoError(err)
	resp, err := rt.RoundTrip(req)
	require.NoError(err)

	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal([]string{"payload", "payload"}, bodies)
	assert.Equal(1, src.invalidated)
}

func TestRetryOnUnauthorizedDoesNotRetryUnrewindableBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"old", "new"}}
	calls := 0
	rt := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://api.example.test",
		io.NopCloser(strings.NewReader("payload")),
	)
	require.NoError(err)
	resp, err := rt.RoundTrip(req)
	require.NoError(err)

	assert.Equal(http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(1, calls)
	assert.Equal(0, src.invalidated)
}

func TestAuthTransportRejectsRequestOutsideAllowedOrigin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := &sequenceSource{tokens: []string{"secret"}}
	called := false
	rt := platform.AuthTransport{
		Source:        src,
		AllowedOrigin: "https://api.example.test/base/path",
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader: platform.BearerAuthHeader,
	}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://evil.example.test", nil,
	)
	require.NoError(err)
	resp, err := rt.RoundTrip(req)

	require.Error(err)
	assert.Nil(resp)
	assert.False(called)
	assert.Equal([]string{"secret"}, src.tokens)
}

func TestAuthTransportRejectsCrossOriginRedirectBeforeAuth(t *testing.T) {
	src := &sequenceSource{tokens: []string{"first", "second"}}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer first", r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL+"/redirected", http.StatusFound)
	}))
	defer origin.Close()

	client := &http.Client{Transport: platform.AuthTransport{
		Source:        src,
		AllowedOrigin: origin.URL,
		Base:          http.DefaultTransport,
		SetHeader:     platform.BearerAuthHeader,
	}}

	resp, err := client.Get(origin.URL + "/start")

	require.Error(t, err)
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.False(t, redirected)
	assert.Equal(t, []string{"second"}, src.tokens)
}
