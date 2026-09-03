package e2etest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/kata"
)

func TestKataLocalDaemonChallengeIsReportedDownE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	authorizations := []string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"upstream"`)
		w.Header().Set("WWW-Authenticate", `Bearer realm="kata"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Authentication required"}`))
	}))
	defer daemon.Close()

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_DB", "")
	writeKataE2ECatalog(t, home, `
active_daemon = "local"

[[daemon]]
name = "local"
local = true
`)
	writeKataE2ERuntimeRecord(t, daemon.URL)
	srv, _ := setupTestServer(t)
	forge := httptest.NewServer(srv)
	defer forge.Close()

	client, err := apiclient.New(forge.URL)
	require.NoError(err)
	roster, err := client.HTTP.ListKataDaemonsWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, roster.StatusCode(), string(roster.Body))
	require.NotNil(roster.JSON200)
	require.NotNil(roster.JSON200.Daemons)
	require.Len(roster.JSON200.Daemons, 1)
	localDaemon := roster.JSON200.Daemons[0]
	assert.Equal("local", localDaemon.Id)
	assert.Equal("none", localDaemon.Auth)
	assert.Equal("down", localDaemon.Health)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal([]string{""}, authorizations)
}

func TestKataLocalDaemonTokenEnvIsNotUsedForNarrowReadsE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	authorizations := []string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/api/v1/ui/references" {
			_, _ = w.Write([]byte(`{"issues":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"api_schema_version":"0.10.0"}`))
	}))
	defer daemon.Close()

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_DB", "")
	t.Setenv("KENN_FORGE_KATA_MISSING_TOKEN", "")
	writeKataE2ECatalog(t, home, `
active_daemon = "local"

[[daemon]]
name = "local"
local = true
token_env = "KENN_FORGE_KATA_MISSING_TOKEN"
`)
	writeKataE2ERuntimeRecord(t, daemon.URL)
	srv, _ := setupTestServer(t)
	forge := httptest.NewServer(srv)
	defer forge.Close()

	client, err := apiclient.New(forge.URL)
	require.NoError(err)
	roster, err := client.HTTP.ListKataDaemonsWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, roster.StatusCode(), string(roster.Body))
	require.NotNil(roster.JSON200)
	require.NotNil(roster.JSON200.Daemons)
	require.Len(roster.JSON200.Daemons, 1)
	localDaemon := roster.JSON200.Daemons[0]
	assert.Equal("local", localDaemon.Id)
	assert.Equal("none", localDaemon.Auth)
	assert.Equal("connected", localDaemon.Health)

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		forge.URL+"/api/v1/kata/daemons/local/references?q=task",
		http.NoBody,
	)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer caller-secret")
	resp, err := forge.Client().Do(req)
	require.NoError(err)
	defer resp.Body.Close()
	assert.Equal(http.StatusOK, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal([]string{"", ""}, authorizations)
}

func writeKataE2ECatalog(t *testing.T, home string, body string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
}

func writeKataE2ERuntimeRecord(t *testing.T, address string) {
	t.Helper()

	runtimeDir, err := kata.RuntimeDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	rec := kata.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   address,
		StartedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(rec)
	require.NoError(t, err)
	path := filepath.Join(runtimeDir, "daemon."+strconv.Itoa(rec.PID)+".json")
	require.NoError(t, os.WriteFile(path, body, 0o600))
}
