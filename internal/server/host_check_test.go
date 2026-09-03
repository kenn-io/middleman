package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

// setupHostCheckServer builds a Server with the given
// HostCheckOptions so each row in the wire-level table controls
// bind, allowed_hosts, and trust_reverse_proxy precisely. The
// override is passed via ServerOptions, which takes precedence in
// resolveHostCheckOptions over both the cfg=nil fallback and the
// test-friendly AllowLoopbackAnyPort relaxation.
func setupHostCheckServer(t *testing.T, opts HostCheckOptions) *Server {
	return setupHostCheckServerWithToken(t, opts, "")
}

func setupHostCheckServerWithToken(
	t *testing.T, opts HostCheckOptions, token string,
) *Server {
	t.Helper()
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	return New(database, syncer, emptyFrontend(), "/", nil, ServerOptions{
		HostCheck: opts,
		DaemonAccess: DaemonAccessOptions{
			Token: token, RequireAPIAuth: token != "",
		},
	})
}

func bindLoopback8091() config.HostKey {
	return config.HostKey{Host: "127.0.0.1", Port: "8091"}
}

func directDaemonRequest(bearer string, headers http.Header) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	req.Host = "127.0.0.1:8091"
	req.RemoteAddr = "127.0.0.1:1234"
	if headers != nil {
		req.Header = headers.Clone()
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func serveDirectDaemonRequest(srv *Server, bearer string, headers http.Header) int {
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, directDaemonRequest(bearer, headers))
	return rr.Code
}

// TestDirectDaemonBearerClassification protects the native-client trust
// boundary without constructing the full application. If the classifier is
// widened, cookies, listener aliases, non-loopback peers, or forwarded
// requests can bypass reverse-proxy Host validation.
func TestDirectDaemonBearerClassification(t *testing.T) {
	tests := []struct {
		name, token, host, remoteAddr, bearer string
		headers                               http.Header
		cookie, wantBypass                    bool
		bind                                  config.HostKey
	}{
		{name: "valid", token: "secret", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1", bearer: "secret", wantBypass: true},
		{name: "missing bearer", token: "secret", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1"},
		{name: "invalid bearer", token: "secret", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1", bearer: "wrong"},
		{name: "cookie", token: "secret", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1", cookie: true},
		{name: "listener alias", token: "secret", host: "localhost:8091", remoteAddr: "127.0.0.1:1", bearer: "secret"},
		{name: "non-loopback peer", token: "secret", host: "127.0.0.1:8091", remoteAddr: "192.0.2.1:1", bearer: "secret"},
		{name: "public host", token: "secret", host: "mm.example.com", remoteAddr: "127.0.0.1:1", bearer: "secret"},
		{name: "forwarded", token: "secret", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1", bearer: "secret", headers: http.Header{"Forwarded": {"host=mm.example.com"}}},
		{name: "missing token", host: "127.0.0.1:8091", remoteAddr: "127.0.0.1:1", bearer: "secret"},
		{name: "IPv6", token: "secret", bind: config.HostKey{Host: "[::1]", Port: "8091"}, host: "[::1]:8091", remoteAddr: "[::1]:1", bearer: "secret", wantBypass: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bind := bindLoopback8091()
			if tt.bind.Host != "" {
				bind = tt.bind
			}
			req := directDaemonRequest(tt.bearer, tt.headers)
			req.Host, req.RemoteAddr = tt.host, tt.remoteAddr
			if tt.cookie {
				req.AddCookie(&http.Cookie{Name: authCookieName, Value: "secret"})
			}
			rr := httptest.NewRecorder()

			admission := (daemonRequestPolicy{token: tt.token}).admit(
				rr, req, HostCheckOptions{Bind: bind}, true,
			)

			assert.False(t, admission.handled)
			assert.Equal(t, tt.wantBypass, admission.bypassProxyHostCheck)
		})
	}
}

// TestDirectDaemonBearerHostIntegration protects middleware ordering. Direct
// bearer requests bypass proxy Host interpretation, while any forwarded shape
// remains subject to the existing proxy validation and rejection rules.
func TestDirectDaemonBearerHostIntegration(t *testing.T) {
	srv := setupHostCheckServerWithToken(t, HostCheckOptions{
		Bind:              bindLoopback8091(),
		TrustReverseProxy: true,
	}, "daemon-secret")
	assert.Equal(t, http.StatusOK, serveDirectDaemonRequest(srv, "daemon-secret", nil))
	assert.Equal(t, http.StatusForbidden, serveDirectDaemonRequest(srv, "", nil))
	assert.Equal(t, http.StatusForbidden, serveDirectDaemonRequest(srv, "wrong", nil))
}

// TestDirectDaemonBearerClassificationWithoutGeneralAPIAuth protects the
// distinction between the direct-listener credential and optional API auth.
// A native client must still bypass proxy Host interpretation without forcing
// proxied browser/API traffic to authenticate.
func TestDirectDaemonBearerClassificationWithoutGeneralAPIAuth(t *testing.T) {
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, emptyFrontend(), "/", nil, ServerOptions{
		HostCheck: HostCheckOptions{
			Bind:              bindLoopback8091(),
			Allowed:           []config.HostKey{{Host: "mm.example.com"}},
			TrustReverseProxy: true,
		},
		DaemonAccess: DaemonAccessOptions{Token: "daemon-secret"},
	})
	assert.Equal(t, http.StatusOK, serveDirectDaemonRequest(srv, "daemon-secret", nil))
	assert.Equal(t, http.StatusOK, serveDirectDaemonRequest(srv, "", http.Header{
		"X-Forwarded-Host": {"mm.example.com"},
	}))
}

// TestHostCheckBackendHost exercises Step 1+2 of the spec: parse
// the request Host and validate against bind, loopback synonyms at
// the bind port, and any configured allowlist entries.
//
// Bind for every case is 127.0.0.1:8091.
func TestHostCheckBackendHost(t *testing.T) {
	runParallelServerTest(t)

	cases := []struct {
		name    string
		allowed []config.HostKey
		host    string
		status  int
	}{
		// Loopback synonyms at the bind port — always accepted.
		{name: "direct loopback IP", allowed: nil, host: "127.0.0.1:8091", status: http.StatusOK},
		{name: "direct localhost", allowed: nil, host: "localhost:8091", status: http.StatusOK},
		{name: "direct IPv6 loopback", allowed: nil, host: "[::1]:8091", status: http.StatusOK},
		{name: "uppercase host accepted", allowed: nil, host: "LOCALHOST:8091", status: http.StatusOK},

		// Bind-derived rejections.
		{name: "wrong port", allowed: nil, host: "127.0.0.1:9999", status: http.StatusForbidden},
		{name: "attacker host (DNS rebinding)", allowed: nil, host: "attacker.example:8091", status: http.StatusForbidden},
		{name: "empty Host", allowed: nil, host: "", status: http.StatusForbidden},
		{name: "malformed Host", allowed: nil, host: "][", status: http.StatusForbidden},
		{name: "port-only Host", allowed: nil, host: ":8091", status: http.StatusForbidden},

		// allowed_hosts entries.
		{name: "allowed_hosts hit, exact port",
			allowed: []config.HostKey{{Host: "mm.local", Port: "8091"}},
			host:    "mm.local:8091", status: http.StatusOK},
		{name: "allowed_hosts miss, wrong port",
			allowed: []config.HostKey{{Host: "mm.local", Port: "8091"}},
			host:    "mm.local:9999", status: http.StatusForbidden},
		{name: "allowed_hosts bare entry hits bare Host",
			allowed: []config.HostKey{{Host: "mm.local", Port: ""}},
			host:    "mm.local", status: http.StatusOK},
		{name: "allowed_hosts bare entry rejects ported Host",
			allowed: []config.HostKey{{Host: "mm.local", Port: ""}},
			host:    "mm.local:8091", status: http.StatusForbidden},
		{name: "IPv6 allowed_hosts hit",
			allowed: []config.HostKey{{Host: "[::1]", Port: "8443"}},
			host:    "[::1]:8443", status: http.StatusOK},
		{name: "allowed_hosts attacker miss",
			allowed: []config.HostKey{{Host: "mm.local", Port: "8091"}},
			host:    "attacker.example:8091", status: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := setupHostCheckServer(t, HostCheckOptions{
				Bind:    bindLoopback8091(),
				Allowed: tc.allowed,
			})
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Host = tc.host
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			assert.Equal(t, tc.status, rr.Code, rr.Body.String())
		})
	}
}

// TestHostCheckForwardedHost exercises Step 3: when
// trust_reverse_proxy is true, validate the Public Host derived
// from X-Forwarded-Host / Forwarded against the same accept-set.
// Always also exercises that DNS-rebinding rejections from Step 2
// run before any forwarded header is read.
func TestHostCheckForwardedHost(t *testing.T) {
	runParallelServerTest(t)

	cases := []struct {
		name              string
		allowed           []config.HostKey
		trustReverseProxy bool
		host              string
		xfh               string
		forwarded         string
		status            int
	}{
		{name: "trust_reverse_proxy off, X-Forwarded-Host ignored",
			allowed: nil, trustReverseProxy: false,
			host: "attacker.example:8091", xfh: "127.0.0.1:8091",
			status: http.StatusForbidden},
		{name: "trust on, raw Host loopback, XFH in allowlist",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091", xfh: "mm.example.com",
			status: http.StatusOK},
		{name: "trust on, raw Host loopback, Forwarded in allowlist",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			forwarded:         "for=10.0.0.1;host=mm.example.com",
			status:            http.StatusOK},
		{name: "trust on, Forwarded quoted host",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			forwarded:         `host="mm.example.com"`,
			status:            http.StatusOK},
		{name: "trust on, multi-value XFH rejected",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com, other.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, multi-value XFH rejected even when later entry is allowed",
			allowed:           []config.HostKey{{Host: "other.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com, other.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, multi-value Forwarded rejected",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			forwarded:         "host=mm.example.com, host=other.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, both headers agree",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com",
			forwarded:         "host=mm.example.com",
			status:            http.StatusOK},
		{name: "trust on, headers disagree",
			allowed: []config.HostKey{
				{Host: "mm.example.com", Port: ""},
				{Host: "other.example.com", Port: ""},
			},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com",
			forwarded:         "host=other.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, neither forwarded header",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			status:            http.StatusForbidden},
		{name: "trust on, forwarded host NOT in allowlist",
			allowed:           nil,
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "attacker.example",
			status:            http.StatusForbidden},
		{name: "trust on, raw Host fails (DNS rebinding even with proxy on)",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "attacker.example:8091",
			xfh:               "mm.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, malformed Forwarded",
			allowed:           nil,
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			forwarded:         "wat",
			status:            http.StatusForbidden},
		{name: "trust on, Forwarded first entry lacks host=",
			allowed:           nil,
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			forwarded:         "for=10.0.0.1, host=mm.example.com",
			status:            http.StatusForbidden},
		{name: "trust on, present-but-malformed Forwarded with valid XFH",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: ""}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com",
			forwarded:         "wat",
			status:            http.StatusForbidden},
		{name: "trust on, forwarded port mismatch",
			allowed:           []config.HostKey{{Host: "mm.example.com", Port: "8443"}},
			trustReverseProxy: true,
			host:              "127.0.0.1:8091",
			xfh:               "mm.example.com:9999",
			status:            http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := setupHostCheckServer(t, HostCheckOptions{
				Bind:              bindLoopback8091(),
				Allowed:           tc.allowed,
				TrustReverseProxy: tc.trustReverseProxy,
			})
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Host = tc.host
			if tc.xfh != "" {
				req.Header.Set("X-Forwarded-Host", tc.xfh)
			}
			if tc.forwarded != "" {
				req.Header.Set("Forwarded", tc.forwarded)
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			assert.Equal(t, tc.status, rr.Code, rr.Body.String())
		})
	}
}

func TestCrossOriginProtectionUsesValidatedForwardedHost(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		value      string
		origin     string
		wantStatus int
	}{
		{
			name:       "X-Forwarded-Host matches Origin",
			header:     "X-Forwarded-Host",
			value:      "forge.example:8080",
			origin:     "http://forge.example:8080",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "Forwarded host matches Origin",
			header:     "Forwarded",
			value:      "for=192.0.2.10;host=forge.example:8080",
			origin:     "http://forge.example:8080",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "forwarded host does not match Origin",
			header:     "X-Forwarded-Host",
			value:      "forge.example:8080",
			origin:     "http://other.example:8080",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := setupHostCheckServer(t, HostCheckOptions{
				Bind: bindLoopback8091(),
				Allowed: []config.HostKey{
					{Host: "forge.example", Port: "8080"},
				},
				TrustReverseProxy: true,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
			req.Host = "127.0.0.1:8091"
			req.Header.Set(tc.header, tc.value)
			req.Header.Set("Origin", tc.origin)
			rr := httptest.NewRecorder()

			srv.ServeHTTP(rr, req)

			require.Equal(t, tc.wantStatus, rr.Code, rr.Body.String())
		})
	}
}

// TestHostCheck403BodyShape pins the 403 body shape used by the
// middleware. The body must be valid JSON of the form
// {"error":"..."} and the value must name both config knobs so an
// operator can debug a rejected request from curl output alone.
func TestHostCheck403BodyShape(t *testing.T) {
	srv := setupHostCheckServer(t, HostCheckOptions{
		Bind: bindLoopback8091(),
	})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "attacker.example:8091"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require := require.New(t)
	assert := assert.New(t)
	require.Equal(http.StatusForbidden, rr.Code)
	body, err := io.ReadAll(rr.Body)
	require.NoError(err)
	var payload struct {
		Error string `json:"error"`
	}
	require.NoError(json.Unmarshal(body, &payload))
	assert.Contains(payload.Error, "allowed_hosts")
	assert.Contains(payload.Error, "trust_reverse_proxy")
}

// TestParseForwardedHost exercises the unexported helpers
// directly, covering the zero-length-header and
// malformed-but-present cases that httptest.NewRequest cannot
// otherwise hand to the middleware (Go's http.Header normalises
// empty values away on set).
func TestParseForwardedHost(t *testing.T) {
	t.Run("Forwarded", func(t *testing.T) {
		cases := []struct {
			name   string
			input  string
			wantOK bool
			want   config.HostKey
		}{
			{name: "host param",
				input:  "host=mm.example.com",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "for and host",
				input:  "for=10.0.0.1;host=mm.example.com",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "quoted host",
				input:  `host="mm.example.com"`,
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "case-insensitive param",
				input:  "Host=mm.example.com",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "multiple entries rejected",
				input:  "host=mm.example.com, host=attacker.example",
				wantOK: false},
			{name: "first entry lacks host=",
				input:  "for=10.0.0.1, host=mm.example.com",
				wantOK: false},
			{name: "empty",
				input:  "",
				wantOK: false},
			{name: "garbage",
				input:  "wat",
				wantOK: false},
			{name: "empty quoted host",
				input:  `host=""`,
				wantOK: false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := parseForwardedHost(tc.input)
				if tc.wantOK {
					require.NoError(t, err)
					assert.Equal(t, tc.want, got)
				} else {
					assert.Error(t, err)
				}
			})
		}
	})

	t.Run("X-Forwarded-Host", func(t *testing.T) {
		cases := []struct {
			name   string
			input  string
			wantOK bool
			want   config.HostKey
		}{
			{name: "single host",
				input:  "mm.example.com",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "host with port",
				input:  "mm.example.com:8443",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: "8443"}},
			{name: "multiple values rejected",
				input:  "mm.example.com, attacker.example",
				wantOK: false},
			{name: "leading whitespace trimmed",
				input:  "  mm.example.com",
				wantOK: true,
				want:   config.HostKey{Host: "mm.example.com", Port: ""}},
			{name: "empty",
				input:  "",
				wantOK: false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := parseXForwardedHost(tc.input)
				if tc.wantOK {
					require.NoError(t, err)
					assert.Equal(t, tc.want, got)
				} else {
					assert.Error(t, err)
				}
			})
		}
	})
}

// TestHostCheckEphemeralPortFollowsListener pins the port-0 contract:
// a config bind of 127.0.0.1:0 means "kernel-assigned", so Serve must
// repoint the accept-set at the actual bound port — otherwise every
// request to an ephemeral-port daemon is rejected.
func TestHostCheckEphemeralPortFollowsListener(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupHostCheckServer(t, HostCheckOptions{
		Bind: config.HostKey{Host: "127.0.0.1", Port: "0"},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	require.NoError(err)
	defer resp.Body.Close()
	assert.Equal(http.StatusOK, resp.StatusCode)

	// The accept-set follows the real port — other ports still reject.
	req, err := http.NewRequest(
		http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil,
	)
	require.NoError(err)
	req.Host = "127.0.0.1:9"
	wrong, err := http.DefaultClient.Do(req)
	require.NoError(err)
	defer wrong.Body.Close()
	assert.Equal(http.StatusForbidden, wrong.StatusCode)
}
