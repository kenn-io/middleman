package fleetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/terminalwebsocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestFleetRepositoryWorkspaceCreateRoutesToOwningHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	type receivedRequest struct {
		path string
		body string
		err  error
	}
	received := make(chan receivedRequest, 1)
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		received <- receivedRequest{path: r.URL.Path, body: string(body), err: err}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"remote-workspace"}`))
	}))
	t.Cleanup(peer.Close)

	srv, _ := setupTestServer(t)
	configureTestMembers(t, srv, testTLSClient(t, peer), config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: peer.URL,
	})
	hub := httptest.NewServer(srv.localHandler())
	t.Cleanup(hub.Close)

	resp := httpDo(
		t,
		hub,
		http.MethodPost,
		"/api/v1/fleet/hosts/"+testMemberNodeID+"/repo/github/acme/widgets/workspaces",
		[]byte(`{"branch":"fleet-ux"}`),
	)
	defer resp.Body.Close()
	require.Equal(http.StatusAccepted, resp.StatusCode)

	request := <-received
	require.NoError(request.err)
	assert.Equal("/api/v1/repo/github/acme/widgets/workspaces", request.path)
	assert.JSONEq(`{"branch":"fleet-ux"}`, request.body)
}

func TestFleetTerminalPasteImageProxyStreamsBinaryBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	imageBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 2<<20)...)
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/terminal/paste-image", r.URL.Path)
		assert.Equal("application/octet-stream", r.Header.Get("Content-Type"))
		got, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			http.Error(w, "read request body", http.StatusInternalServerError)
			return
		}
		assert.Equal(imageBytes, got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"path":"/var/lib/forge/paste-image.png"}`))
	}))
	t.Cleanup(peer.Close)

	srv, _ := setupTestServer(t)
	configureTestMembers(t, srv, testTLSClient(t, peer), config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: peer.URL,
	})
	hub := httptest.NewServer(srv.localHandler())
	t.Cleanup(hub.Close)
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		hub.URL+"/api/v1/fleet/hosts/"+testMemberNodeID+"/terminal/paste-image",
		bytes.NewReader(imageBytes),
	)
	require.NoError(err)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := hub.Client().Do(req)
	require.NoError(err)
	defer resp.Body.Close()

	assert.Equal(http.StatusCreated, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(err)
	assert.JSONEq(`{"path":"/var/lib/forge/paste-image.png"}`, string(body))
}

func TestFleetWebSocketProxyNegotiatesContextTakeoverOnBothLegs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	peerExtensions := make(chan string, 1)
	peerHeaders := make(chan http.Header, 1)
	peerPath := make(chan string, 1)
	peerErrors := make(chan error, 1)
	peer := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			peerPath <- r.URL.Path
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
				InsecureSkipVerify: true,
				CompressionMode:    websocket.CompressionContextTakeover,
			})
			if err != nil {
				peerErrors <- err
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")
			peerExtensions <- w.Header().Get("Sec-WebSocket-Extensions")
			peerHeaders <- r.Header.Clone()

			for {
				typ, payload, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if err := conn.Write(r.Context(), typ, payload); err != nil {
					peerErrors <- err
					return
				}
			}
		},
	))
	t.Cleanup(peer.Close)

	srv, _ := setupTestServer(t)
	tokens := configureTestMembers(t, srv, testTLSClient(t, peer), config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: peer.URL,
	})
	hub := httptest.NewServer(srv.localHandler())
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const sessionKey = "client:key one"
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http") +
		"/ws/v1/fleet/hosts/" + testMemberNodeID +
		"/workspaces/ws_1/runtime/sessions/" + url.PathEscape(sessionKey) + "/terminal"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer browser-secret"},
			"Cookie":        []string{"session=browser"},
			"Origin":        []string{"https://hub.example"},
		},
	})
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	require.NotNil(resp)

	clientExtensions := resp.Header.Get("Sec-WebSocket-Extensions")
	assert.Contains(clientExtensions, "permessage-deflate")
	assert.NotContains(clientExtensions, "client_no_context_takeover")
	assert.NotContains(clientExtensions, "server_no_context_takeover")

	select {
	case extensions := <-peerExtensions:
		assert.Contains(extensions, "permessage-deflate")
		assert.NotContains(extensions, "client_no_context_takeover")
		assert.NotContains(extensions, "server_no_context_takeover")
	case err := <-peerErrors:
		require.NoError(err)
	case <-ctx.Done():
		require.Fail("peer websocket handshake did not complete")
	}
	select {
	case headers := <-peerHeaders:
		assert.Equal("Bearer "+tokens[testMemberNodeID], headers.Get("Authorization"))
		assert.Equal(testHubNodeID, headers.Get("X-Kenn-Forge-Node-ID"))
		assert.Empty(headers.Get("Cookie"))
		assert.Empty(headers.Get("Origin"))
	case <-ctx.Done():
		require.Fail("peer websocket headers were not captured")
	}
	select {
	case path := <-peerPath:
		assert.Equal(
			"/ws/v1/workspaces/ws_1/runtime/sessions/"+sessionKey+"/terminal",
			path,
		)
	case <-ctx.Done():
		require.Fail("peer websocket path was not captured")
	}

	want := []byte("fleet-compression-round-trip")
	require.NoError(conn.Write(ctx, websocket.MessageBinary, want))
	typ, got, err := conn.Read(ctx)
	require.NoError(err)
	assert.Equal(websocket.MessageBinary, typ)
	assert.Equal(want, got)

	require.NoError(conn.Write(
		ctx, websocket.MessageText, []byte(terminalwebsocket.HeartbeatMessage),
	))
	typ, got, err = conn.Read(ctx)
	require.NoError(err)
	assert.Equal(websocket.MessageText, typ)
	assert.JSONEq(terminalwebsocket.HeartbeatMessage, string(got))
}

func TestFleetProxyUsesOnlyDestinationCredentialAndRefusesRedirects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const otherNodeID = "cccccccccccccccccccccccccccccccc"

	type capturedRequest struct {
		authorization string
		cookie        string
		origin        string
		forwarded     string
		nodeID        string
	}
	var destinationRequests []capturedRequest
	var otherRequests int
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(other.Close)
	redirect := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests = append(destinationRequests, capturedRequest{
			authorization: r.Header.Get("Authorization"),
			cookie:        r.Header.Get("Cookie"),
			origin:        r.Header.Get("Origin"),
			forwarded:     r.Header.Get("Forwarded"),
			nodeID:        r.Header.Get("X-Kenn-Forge-Node-ID"),
		})
		if redirect {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspaces":[]}`))
	}))
	t.Cleanup(destination.Close)

	handler := New(Deps{})
	tokens := configureTestMembers(
		t, handler, testTLSClient(t, destination, other),
		config.FleetMember{NodeID: testMemberNodeID, BaseURL: destination.URL},
		config.FleetMember{NodeID: otherNodeID, BaseURL: other.URL},
	)
	api := newFleetTestAPI()
	handler.Register(api)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodGet,
			"/fleet/hosts/"+testMemberNodeID+"/workspaces",
			nil,
		)
		req.Header.Set("Authorization", "Bearer browser-secret")
		req.Header.Set("Cookie", "session=browser")
		req.Header.Set("Origin", "https://hub.example")
		req.Header.Set("Forwarded", "host=hub.example;proto=https")
		recorder := httptest.NewRecorder()
		api.Adapter().ServeHTTP(recorder, req)
		return recorder
	}

	response := request()
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.Len(destinationRequests, 1)
	assert.Equal("Bearer "+tokens[testMemberNodeID], destinationRequests[0].authorization)
	assert.Equal(testHubNodeID, destinationRequests[0].nodeID)
	assert.Empty(destinationRequests[0].cookie)
	assert.Empty(destinationRequests[0].origin)
	assert.Empty(destinationRequests[0].forwarded)
	assert.Zero(otherRequests)

	redirect = true
	response = request()
	require.Equal(http.StatusTemporaryRedirect, response.Code, response.Body.String())
	assert.Zero(otherRequests, "the destination credential must never cross an enrolled origin")
	assert.NotEqual(tokens[otherNodeID], destinationRequests[1].authorization)
}

func TestFleetProxyRejectsMemberOriginOutsideEnrollment(t *testing.T) {
	require := require.New(t)
	original := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(original.Close)
	var substitutedRequests int
	substituted := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		substitutedRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspaces":[]}`))
	}))
	t.Cleanup(substituted.Close)

	handler := New(Deps{})
	configureTestMembers(
		t, handler, testTLSClient(t, original, substituted),
		config.FleetMember{NodeID: testMemberNodeID, BaseURL: original.URL},
	)
	snapshot := handler.configSnapshot()
	snapshot.Fleet.Members[0].BaseURL = substituted.URL
	handler.ApplyConfig(snapshot)
	api := newFleetTestAPI()
	handler.Register(api)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/fleet/hosts/"+testMemberNodeID+"/workspaces",
		nil,
	)

	api.Adapter().ServeHTTP(recorder, req)

	require.Equal(http.StatusNotFound, recorder.Code, recorder.Body.String())
	require.Zero(substitutedRequests, "credential must not reach an unenrolled origin")
}

func TestFederationMemberClientsBoundRequestsAndStreamingHandshakes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	baseTransport := &http.Transport{
		TLSHandshakeTimeout:    time.Minute,
		ResponseHeaderTimeout:  time.Minute,
		ExpectContinueTimeout:  time.Minute,
		MaxResponseHeaderBytes: 2 << 20,
	}
	base := &http.Client{Timeout: time.Minute, Transport: baseTransport}

	clients := newFederationMemberClients(base)

	assert.Equal(15*time.Second, clients.rest.Timeout)
	assert.Zero(clients.proxy.Timeout,
		"proxied operations own their request lifetime through the browser context")
	assert.Zero(clients.websocket.Timeout,
		"a websocket body may stream indefinitely after its bounded handshake")
	restTransport, ok := clients.rest.Transport.(*http.Transport)
	require.True(ok)
	assert.NotSame(baseTransport, restTransport)
	assert.Equal(5*time.Second, restTransport.TLSHandshakeTimeout)
	assert.Equal(10*time.Second, restTransport.ResponseHeaderTimeout)
	proxyTransport, ok := clients.proxy.Transport.(*http.Transport)
	require.True(ok)
	assert.Zero(proxyTransport.ResponseHeaderTimeout,
		"long polls and synchronous clones may take longer than snapshot fan-out")
	assert.Equal(time.Second, restTransport.ExpectContinueTimeout)
	assert.Equal(int64(1<<20), restTransport.MaxResponseHeaderBytes)
	assert.Equal(time.Minute, base.Timeout, "hardening must not mutate the shared client")
	assert.Equal(time.Minute, baseTransport.ResponseHeaderTimeout,
		"hardening must not mutate the shared transport")

	timed := hardenedFederationHTTPClient(&http.Client{
		Timeout: 25 * time.Millisecond,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	}, false)
	req, err := http.NewRequest(http.MethodGet, "https://member.example/api/v1/snapshot", nil)
	require.NoError(err)
	started := time.Now()
	_, err = timed.Do(req)
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Less(time.Since(started), time.Second)
}

func TestFleetProxyStripsPeerAuthorityResponseHeaders(t *testing.T) {
	destination := make(http.Header)
	source := http.Header{
		"Content-Type":     []string{"application/json"},
		"Connection":       []string{"X-Peer-Secret"},
		"X-Peer-Secret":    []string{"hidden"},
		"Set-Cookie":       []string{"forge_auth=attacker"},
		"Location":         []string{"https://spoke.example/elsewhere"},
		"Clear-Site-Data":  []string{`"cookies"`},
		"Www-Authenticate": []string{`Bearer realm="spoke"`},
	}

	copyProxyResponseHeaders(destination, source)

	assert.Equal(t, "application/json", destination.Get("Content-Type"))
	for _, key := range []string{
		"Connection", "X-Peer-Secret", "Set-Cookie", "Location",
		"Clear-Site-Data", "Www-Authenticate",
	} {
		assert.Empty(t, destination.Values(key), key)
	}
}

func TestFleetRESTProxyClosesMemberResponseBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := &closeTrackingBody{Reader: strings.NewReader(`{"workspaces":[]}`)}
	client := &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		},
	)}
	handler := New(Deps{})
	configureTestMembers(t, handler, client, config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: "https://member.example",
	})
	api := newFleetTestAPI()
	handler.Register(api)
	req := httptest.NewRequest(
		http.MethodGet,
		"/fleet/hosts/"+testMemberNodeID+"/workspaces",
		nil,
	)
	recorder := httptest.NewRecorder()

	api.Adapter().ServeHTTP(recorder, req)

	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
	assert.True(body.closed, "the proxy must close every completed member response")
}

func TestFleetRESTProxyRejectsOversizedBodyBeforeDialingMember(t *testing.T) {
	require := require.New(t)
	peerRequests := 0
	client := &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			peerRequests++
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}, nil
		},
	)}
	handler := New(Deps{})
	configureTestMembers(t, handler, client, config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: "https://member.example",
	})
	api := newFleetTestAPI()
	handler.Register(api)
	body := strings.Repeat("x", int(fleetProxyMaxBodyBytes)+1)

	for _, tc := range []struct {
		name          string
		method        string
		path          string
		contentLength int64
	}{
		{
			name:          "known length workspace write",
			method:        http.MethodPost,
			path:          "/fleet/hosts/" + testMemberNodeID + "/workspaces",
			contentLength: int64(len(body)),
		},
		{
			name:          "chunked project write",
			method:        http.MethodPost,
			path:          "/fleet/hosts/" + testMemberNodeID + "/projects",
			contentLength: -1,
		},
		{
			name:          "chunked workspace read",
			method:        http.MethodGet,
			path:          "/fleet/hosts/" + testMemberNodeID + "/workspaces",
			contentLength: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
			request.ContentLength = tc.contentLength

			api.Adapter().ServeHTTP(recorder, request)

			require.Equal(http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
			var problem struct {
				Code string `json:"code"`
			}
			require.NoError(json.Unmarshal(recorder.Body.Bytes(), &problem))
			require.Equal(string(httpapi.CodePayloadTooLarge), problem.Code)
		})
	}
	require.Zero(peerRequests, "oversized request bodies must be rejected before peer dialing")
}

// TestCopyProxyRequestHeadersStripsBrowserHeaders verifies the hub does not
// forward a browser's Origin or Sec-Fetch-* metadata onto a server-to-server
// fleet proxy request. Forwarding them trips the peer's host-authority guard,
// which validates Origin against its own allowed hosts and rejects the
// fan-out because the origin is the hub, not the peer. It also verifies the
// caller's Authorization and Cookie are stripped: they authenticate the hub,
// not the peer, so forwarding them only leaks the hub credential.
func TestCopyProxyRequestHeadersStripsBrowserHeaders(t *testing.T) {
	assert := assert.New(t)
	src := http.Header{}
	src.Set("Origin", "http://hub.local:8091")
	src.Set("Sec-Fetch-Site", "same-origin")
	src.Set("Sec-Fetch-Mode", "cors")
	src.Set("Authorization", "Bearer token")
	src.Set("Content-Type", "application/json")
	src.Set("Cookie", "session=abc")
	src.Set("Connection", "keep-alive") // hop-by-hop
	src.Set("Forwarded", "host=hub.local:8091;proto=https")
	src.Set("X-Forwarded-Host", "hub.local:8091")
	src.Set("X-Forwarded-Proto", "https")

	dst := http.Header{}
	copyProxyRequestHeaders(dst, src)

	assert.Empty(dst.Get("Origin"), "browser Origin must not reach the peer")
	assert.Empty(dst.Get("Sec-Fetch-Site"), "Sec-Fetch-* must not reach the peer")
	assert.Empty(dst.Get("Sec-Fetch-Mode"), "Sec-Fetch-* must not reach the peer")
	assert.Empty(dst.Get("Connection"), "hop-by-hop headers are still dropped")
	assert.Empty(dst.Get("Forwarded"), "forwarded host metadata must not reach the peer")
	assert.Empty(dst.Get("X-Forwarded-Host"), "forwarded host metadata must not reach the peer")
	assert.Empty(dst.Get("X-Forwarded-Proto"), "forwarded proxy metadata must not reach the peer")
	assert.Empty(dst.Get("Authorization"), "the hub credential must not leak to the peer")
	assert.Empty(dst.Get("Cookie"), "the hub session cookie must not leak to the peer")
	assert.Equal("application/json", dst.Get("Content-Type"), "content type must pass through")
}

// TestCopyProxyWebSocketRequestHeadersStripsBrowserHeaders verifies the same
// browser-header stripping applies to fleet websocket dials, on top of the
// existing Sec-WebSocket-* exclusion the dialer sets itself.
func TestCopyProxyWebSocketRequestHeadersStripsBrowserHeaders(t *testing.T) {
	assert := assert.New(t)
	src := http.Header{}
	src.Set("Origin", "http://hub.local:8091")
	src.Set("Sec-Fetch-Dest", "websocket")
	src.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	src.Set("Authorization", "Bearer token")
	src.Set("Cookie", "forge_auth=abc")
	src.Set("Forwarded", "host=hub.local:8091")
	src.Set("X-Forwarded-Host", "hub.local:8091")

	dst := http.Header{}
	copyProxyWebSocketRequestHeaders(dst, src)

	assert.Empty(dst.Get("Origin"), "browser Origin must not reach the peer")
	assert.Empty(dst.Get("Sec-Fetch-Dest"), "Sec-Fetch-* must not reach the peer")
	assert.Empty(dst.Get("Sec-WebSocket-Key"), "Sec-WebSocket-* stays dialer-owned")
	assert.Empty(dst.Get("Forwarded"), "forwarded host metadata must not reach the peer")
	assert.Empty(dst.Get("X-Forwarded-Host"), "forwarded host metadata must not reach the peer")
	assert.Empty(dst.Get("Authorization"), "the hub credential must not leak to the peer")
	assert.Empty(dst.Get("Cookie"), "the hub session cookie must not leak to the peer")
}

func TestIsPeerProxyClientHeader(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"Origin", true},
		{"origin", true},
		{"Sec-Fetch-Site", true},
		{"sec-fetch-mode", true},
		{"Sec-Fetch-Dest", true},
		{"Forwarded", true},
		{"X-Forwarded-Host", true},
		{"x-forwarded-proto", true},
		{"X-Forwarded-For", true},
		{"Authorization", false},
		{"Content-Type", false},
		{"Sec-WebSocket-Key", false},
		{"X-Kenn-Forge-Fleet-Host", false},
	} {
		assert.Equal(t, tc.want, isPeerProxyClientHeader(tc.key), "header %q", tc.key)
	}
}

func TestIsPeerProxyCredentialHeader(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"Authorization", true},
		{"authorization", true},
		{"Cookie", true},
		{"cookie", true},
		{"Content-Type", false},
		{"Origin", false},
		{"X-Kenn-Forge-Fleet-Host", false},
	} {
		assert.Equal(t, tc.want, isPeerProxyCredentialHeader(tc.key), "header %q", tc.key)
	}
}

func TestResolveFleetHostTargetSkipsRemoteMembersWhenFederationDisabled(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		nodeID: testHubNodeID,
		config: ConfigSnapshot{
			Fleet: config.Fleet{
				Role: config.FleetRoleHub,
				Members: []config.FleetMember{
					{NodeID: testMemberNodeID, BaseURL: "https://member.test"},
				},
			},
		},
	}

	_, ok := srv.resolveFleetHostTarget(testMemberNodeID)
	assert.False(ok, "disabled federation must not resolve remote members")

	self, ok := srv.resolveFleetHostTarget(fleetSelfHostAlias)
	require.True(t, ok, "disabled federation must preserve self routing")
	assert.True(self.self)
}

func TestResolveFleetHostTargetUsesActiveMembersWhenFederationEnabled(t *testing.T) {
	assert := assert.New(t)
	srv := New(Deps{})
	configureTestMembers(t, srv, nil, config.FleetMember{
		NodeID: testMemberNodeID, BaseURL: "https://member.test",
	})

	target, ok := srv.resolveFleetHostTarget(testMemberNodeID)
	require.True(t, ok)
	assert.Equal(testMemberNodeID, target.member.NodeID)
}
