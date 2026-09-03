package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

// setupTestServerWithRoborev creates a server with the roborev
// proxy configured to point at the given endpoint URL.
func setupTestServerWithRoborev(
	t *testing.T, roborevEndpoint string,
) *Server {
	t.Helper()

	dir := t.TempDir()
	database := dbtest.Open(t)

	cfgContent := fmt.Sprintf(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget"

[roborev]
endpoint = %q
`, roborevEndpoint)

	cfgPath := filepath.Join(dir, "config.toml")
	err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644)
	require.NoError(t, err)

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	mock := &mockGH{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database, nil, nil, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	return NewWithConfig(
		database, syncer, nil, nil, cfg, cfgPath,
		ServerOptions{HostCheckAllowLoopbackAnyPort: true},
	)
}

func TestRoborevProxyForwarding(t *testing.T) {
	assert := assert.New(t)

	var mu sync.Mutex
	var receivedMethod, receivedPath string
	daemon := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			receivedMethod = r.Method
			receivedPath = r.URL.Path
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jobs":[{"id":1}]}`))
		},
	))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/roborev/jobs", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	mu.Lock()
	gotMethod := receivedMethod
	gotPath := receivedPath
	mu.Unlock()

	assert.Equal("GET", gotMethod)
	assert.Equal("/jobs", gotPath)
	assert.JSONEq(`{"jobs":[{"id":1}]}`, rr.Body.String())
}

func TestRoborevProxyRejectsDeclaredStreamsWithoutAccept(t *testing.T) {
	var upstreamRequests atomic.Int64
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)
	for _, target := range []string{
		"/api/roborev/api/stream/events",
		"/api/roborev/api/job/output?job_id=7&stream=1",
	} {
		t.Run(target, func(t *testing.T) {
			rr := testutil.DoJSON(t, srv, http.MethodGet, target, nil)

			assert.Equal(t, http.StatusNotAcceptable, rr.Code)
			assert.Contains(t, rr.Body.String(), "requires an explicit Accept header")
		})
	}
	assert.Zero(t, upstreamRequests.Load())
}

func TestRoborevProxyE2EForwardsSubpathAndNonGETMethod(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	var receivedMethod, receivedPath, receivedQuery, receivedBody string
	daemon := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			mu.Lock()
			receivedMethod = r.Method
			receivedPath = r.URL.Path
			receivedQuery = r.URL.RawQuery
			receivedBody = string(body)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)
	forge := httptest.NewServer(srv)
	defer forge.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		forge.URL+"/api/roborev/api/jobs/123/retry?force=1",
		strings.NewReader(`{"reason":"retry"}`),
	)
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode, string(respBody))

	mu.Lock()
	gotMethod := receivedMethod
	gotPath := receivedPath
	gotQuery := receivedQuery
	gotBody := receivedBody
	mu.Unlock()

	assert.Equal(http.MethodPost, gotMethod)
	assert.Equal("/api/jobs/123/retry", gotPath)
	assert.Equal("force=1", gotQuery)
	assert.JSONEq(`{"reason":"retry"}`, gotBody)
	assert.JSONEq(`{"ok":true}`, string(respBody))
}

func TestRoborevProxy502(t *testing.T) {
	assert := assert.New(t)

	srv := setupTestServerWithRoborev(t, "http://127.0.0.1:1")

	rr := testutil.DoJSON(
		t, srv, http.MethodGet, "/api/roborev/jobs", nil)

	require.Equal(t, http.StatusBadGateway, rr.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Contains(body["error"], "roborev daemon is not reachable")
}

func TestRoborevHealthProbeAvailable(t *testing.T) {
	assert := assert.New(t)

	daemon := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/status" {
				w.Header().Set(
					"Content-Type", "application/json",
				)
				_, _ = w.Write(
					[]byte(`{"version":"1.2.3"}`),
				)
				return
			}
			http.NotFound(w, r)
		},
	))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)

	rr := testutil.DoJSON(
		t, srv, http.MethodGet,
		"/api/v1/roborev/status", nil)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp roborevStatusResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(resp.Available)
	assert.Equal("1.2.3", resp.Version)
	assert.Equal(daemon.URL, resp.Endpoint)
}

func TestRoborevHealthProbeUnavailable(t *testing.T) {
	assert := assert.New(t)

	srv := setupTestServerWithRoborev(t, "http://127.0.0.1:1")

	rr := testutil.DoJSON(
		t, srv, http.MethodGet,
		"/api/v1/roborev/status", nil)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp roborevStatusResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.False(resp.Available)
	assert.Empty(resp.Version)
}

func TestRoborevNDJSONPassThrough(t *testing.T) {
	lines := []string{
		`{"event":"start","id":1}`,
		`{"event":"progress","pct":50}`,
		`{"event":"done","pct":100}`,
	}

	// Gate each line behind a channel so we can prove
	// streaming: lines arrive before the handler returns.
	gate := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(
				"Content-Type", "application/x-ndjson",
			)
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "not a Flusher", 500)
				return
			}
			for _, line := range lines {
				fmt.Fprintln(w, line)
				flusher.Flush()
				// Wait for reader to ack before next line.
				<-gate
			}
		},
	))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)

	// Wrap the kenn-forge server in its own httptest.Server
	// so we get a real TCP connection with streaming reads.
	forge := httptest.NewServer(srv)
	defer forge.Close()

	r := require.New(t)

	resp, err := http.Get(
		forge.URL + "/api/roborev/stream",
	)
	r.NoError(err)
	defer resp.Body.Close()

	r.Equal(http.StatusOK, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	var received []string
	for scanner.Scan() {
		received = append(received, scanner.Text())
		// Unblock the daemon to send the next line.
		gate <- struct{}{}
	}
	r.NoError(scanner.Err())
	r.Equal(lines, received)
}

func TestRoborevProxyCancelsIdleUpstreamBeforeReconnect(t *testing.T) {
	require := require.New(t)

	var started atomic.Int64
	var canceled atomic.Int64
	daemon := httptest.NewServer(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			requestNumber := started.Add(1)
			<-r.Context().Done()
			canceled.Store(requestNumber)
		},
	))
	defer daemon.Close()

	srv := setupTestServerWithRoborev(t, daemon.URL)
	forge := httptest.NewServer(srv)
	defer forge.Close()

	startRequest := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			forge.URL+"/api/roborev/api/stream/events",
			nil,
		)
		require.NoError(err)
		req.Header.Set("Accept", "application/x-ndjson")
		done := make(chan error, 1)
		go func() {
			resp, requestErr := http.DefaultClient.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			done <- requestErr
		}()
		return cancel, done
	}

	cancelFirst, firstDone := startRequest()
	require.Eventually(
		func() bool { return started.Load() == 1 },
		time.Second,
		10*time.Millisecond,
	)
	cancelFirst()
	require.Error(<-firstDone)
	require.Eventually(
		func() bool { return canceled.Load() == 1 },
		time.Second,
		10*time.Millisecond,
	)

	// Open the replacement only after the idle upstream request has closed.
	cancelSecond, secondDone := startRequest()
	require.Eventually(
		func() bool { return started.Load() == 2 },
		time.Second,
		10*time.Millisecond,
	)
	cancelSecond()
	require.Error(<-secondDone)
	require.Eventually(
		func() bool { return canceled.Load() == 2 },
		time.Second,
		10*time.Millisecond,
	)
}
