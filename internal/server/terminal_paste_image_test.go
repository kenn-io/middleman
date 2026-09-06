package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/terminalpaste"
)

func TestTerminalPasteImageStoresBrowserImageForRemoteTerminal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	srv := New(
		openTestDB(t), nil, nil, "/",
		&config.Config{DataDir: dataDir},
		ServerOptions{HostCheck: HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
		}},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	var imageBytes bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x44, G: 0x88, B: 0xcc, A: 0xff})
	require.NoError(png.Encode(&imageBytes, img))

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/terminal/paste-image",
		bytes.NewReader(imageBytes.Bytes()),
	)
	setAcceptedHostForServerTest(req, srv)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusCreated, rr.Code, rr.Body.String())
	var response struct {
		Path string `json:"path"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&response))
	assert.True(filepath.IsAbs(response.Path))
	assert.Equal(".png", filepath.Ext(response.Path))
	assert.NotContains(response.Path, " ")
	assert.True(strings.HasPrefix(
		response.Path,
		filepath.Join(dataDir, "cache", "terminal-paste-images")+string(os.PathSeparator),
	))
	stored, err := os.ReadFile(response.Path)
	require.NoError(err)
	assert.Equal(imageBytes.Bytes(), stored)
	info, err := os.Stat(response.Path)
	require.NoError(err)
	assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(response.Path))
	require.NoError(err)
	assert.Equal(os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestTerminalPasteImageAcceptsFleetPeerRelay(t *testing.T) {
	dataDir := t.TempDir()
	store, err := federationauth.Open(filepath.Join(t.TempDir(), "credentials.json"))
	require.NoError(t, err)
	token, err := store.MintInbound("fedcba9876543210fedcba9876543210", []federationauth.Scope{federationauth.ScopeTerminalAttach})
	require.NoError(t, err)
	srv := New(
		openTestDB(t), nil, nil, "/",
		&config.Config{DataDir: dataDir},
		ServerOptions{FederationCredentials: store,
			DaemonAccess: DaemonAccessOptions{Token: "local-secret", RequireAPIAuth: true},
			HostCheck: HostCheckOptions{
				Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
			}},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/terminal/paste-image",
		strings.NewReader("\x89PNG\r\n\x1a\n"),
	)
	setAcceptedHostForServerTest(req, srv)
	req.RemoteAddr = "203.0.113.8:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

func TestTerminalPasteImageRejectsUnsupportedAndOversizedPayloads(t *testing.T) {
	srv := New(
		openTestDB(t), nil, nil, "/",
		&config.Config{DataDir: t.TempDir()},
		ServerOptions{HostCheck: HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
		}},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	oversized := make([]byte, terminalpaste.MaxImageBytes+1)
	copy(oversized, []byte("\x89PNG\r\n\x1a\n"))

	for _, tt := range []struct {
		name string
		body []byte
		want int
	}{
		{name: "unsupported", body: []byte("not an image"), want: http.StatusBadRequest},
		{name: "oversized", body: oversized, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/terminal/paste-image",
				bytes.NewReader(tt.body),
			)
			setAcceptedHostForServerTest(req, srv)
			req.RemoteAddr = "203.0.113.7:54321"
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rr := httptest.NewRecorder()

			srv.ServeHTTP(rr, req)

			assert.Equal(t, tt.want, rr.Code, rr.Body.String())
		})
	}
}

func TestTerminalPasteImageFleetRouteAcceptsBrowserBinaryContentType(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/fleet/hosts/missing/terminal/paste-image",
		strings.NewReader("\x89PNG\r\n\x1a\n"),
	)
	setAcceptedHostForServerTest(req, srv)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

func TestTerminalPasteImageAcceptsAuthenticatedCLIRelay(t *testing.T) {
	srv := New(
		openTestDB(t), nil, nil, "/",
		&config.Config{DataDir: t.TempDir()},
		ServerOptions{HostCheck: HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
		}},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/terminal/paste-image",
		strings.NewReader("\x89PNG\r\n\x1a\n"),
	)
	setAcceptedHostForServerTest(req, srv)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer daemon-token")
	req.Header.Set("Content-Type", "application/octet-stream")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

func TestTerminalPasteImageRejectsSimpleRequestContentType(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/terminal/paste-image",
		strings.NewReader("image bytes"),
	)
	setAcceptedHostForServerTest(req, srv)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, rr.Code, rr.Body.String())
}
