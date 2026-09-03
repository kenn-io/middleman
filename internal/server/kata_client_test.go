package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/kata"
)

func TestKataAPIClientExposesSnapshotEnrichmentMethods(t *testing.T) {
	runParallelServerTest(t)

	_ = kataAPIClient.ShowIssueByUIDWithResponse
	_ = kataAPIClient.PollEventsWithResponse
	_ = kataAPIClient.ReachableIssueGraphWithResponse
}

func TestNewKataAPIClientUsesResolvedTargetAuth(t *testing.T) {
	runParallelServerTest(t)
	require := require.New(t)

	var authorization string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		assert.Equal(t, "/api/v1/instance", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance_id":"test","version":"test"}`))
	}))
	t.Cleanup(daemon.Close)

	api, err := newKataAPIClient(t.Context(), kata.Daemon{
		ID:    "work",
		URL:   daemon.URL,
		Token: "secret-token",
	})
	require.NoError(err)

	response, err := api.InstanceWithResponse(t.Context())
	require.NoError(err)
	require.NotNil(response.JSON200)
	assert.Equal(t, "Bearer secret-token", authorization)
}

func TestKataAPIClientStreamEventsRawDoesNotBuffer(t *testing.T) {
	runParallelServerTest(t)
	require := require.New(t)

	requestHeaders := make(chan http.Header, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("id: 1\ndata: {}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(daemon.Close)

	api, err := newKataAPIClient(t.Context(), kata.Daemon{ID: "work", URL: daemon.URL, Token: "secret-token"})
	require.NoError(err)

	type streamResult struct {
		response *http.Response
		err      error
	}
	result := make(chan streamResult, 1)
	streamCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		response, streamErr := api.StreamEventsRaw(streamCtx, nil)
		result <- streamResult{response: response, err: streamErr}
	}()

	select {
	case streamed := <-result:
		require.NoError(streamed.err)
		require.NotNil(streamed.response)
		require.NotNil(streamed.response.Body)
		require.NoError(streamed.response.Body.Close())
	case <-time.After(time.Second):
		require.Fail("StreamEventsRaw buffered the live response body")
	}

	headers := <-requestHeaders
	require.Equal("text/event-stream", headers.Get("Accept"))
	require.Equal("Bearer secret-token", headers.Get("Authorization"))
}

func TestKataGeneratedHTTPDoerRejectsResponseBeyondEndpointBudget(t *testing.T) {
	runParallelServerTest(t)
	require := require.New(t)

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, strings.NewReader("123456789"))
	}))
	t.Cleanup(daemon.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, daemon.URL+"/api/v1/issues", nil)
	require.NoError(err)
	doer := kataGeneratedHTTPDoer{
		client: daemon.Client(),
		limitForRequest: func(*http.Request) int64 {
			return 8
		},
	}
	response, err := doer.Do(t.Context(), request)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(response.Body.Close()) })

	_, err = io.ReadAll(response.Body)
	var tooLarge *kataDaemonResponseTooLargeError
	require.ErrorAs(err, &tooLarge)
	require.Equal(int64(8), tooLarge.Limit)
	require.Equal("/api/v1/issues", tooLarge.Path)
}

func TestKataGeneratedResponseLimitLeavesRoomForCompleteAuthorities(t *testing.T) {
	runParallelServerTest(t)
	require := require.New(t)

	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/api/v1/issues"))
	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/api/v1/ready"))
	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/api/v1/projects/7/ready"))
	require.Equal(int64(32<<20), kataGeneratedResponseLimit("/api/v1/projects"))
	require.Equal(int64(32<<20), kataGeneratedResponseLimit("/api/v1/projects/7/events"))

	// Daemons served under a base-path prefix must keep the enlarged
	// authority budgets; detail reads must not inherit them.
	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/kata/api/v1/issues"))
	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/kata/api/v1/ready"))
	require.Equal(int64(128<<20), kataGeneratedResponseLimit("/kata/api/v1/projects/7/ready"))
	require.Equal(int64(32<<20), kataGeneratedResponseLimit("/api/v1/issues/issue-a"))
	require.Equal(int64(32<<20), kataGeneratedResponseLimit("/kata/api/v1/projects/7/events"))
}
