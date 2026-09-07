package main

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"go.kenn.io/forge/internal/platformdb"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/runtimelock"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestArchiveCommandE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	bin := buildForge(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(os.MkdirAll(dataDir, 0o700))
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		projectPath := "/api/v4/projects/owner%2Farchive"
		if r.Method == http.MethodGet &&
			(r.URL.EscapedPath() == projectPath || r.URL.Path == "/api/v4/projects/owner/archive") {
			_, _ = fmt.Fprint(w, `{
				"id":1,"path":"archive","path_with_namespace":"owner/archive",
				"name":"archive","default_branch":"main",
				"web_url":"https://example.invalid/owner/archive",
				"http_url_to_repo":"https://example.invalid/owner/archive.git"
			}`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(api.Close)
	host := api.Listener.Addr().String()
	certPath := filepath.Join(root, "provider-cert.pem")
	require.NoError(os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: api.Certificate().Raw,
	}), 0o600))
	cfgPath := filepath.Join(root, "config.toml")
	port := reserveFreePort(t)
	writeMinimalConfig(t, cfgPath, dataDir, port)
	existing, err := os.ReadFile(cfgPath)
	require.NoError(err)
	require.NoError(os.WriteFile(cfgPath, append(
		[]byte("base_path = \"/archive-e2e\"\ntrust_reverse_proxy = true\n"),
		append(existing, fmt.Appendf(nil, `
[api]
require_auth = true

[[repos]]
platform = "gitlab"
platform_host = %q
owner = "owner"
name = "archive"
token_env = "KENN_FORGE_ARCHIVE_E2E_TOKEN"
`, host)...)...,
	), 0o600))

	now := time.Now().UTC().Truncate(time.Second)
	database := dbtest.OpenAt(t, filepath.Join(dataDir, "forge.db"))
	ref := platform.RepoRef{
		Platform: platform.KindGitLab, Host: host, Owner: "owner",
		Name: "archive", RepoPath: "owner/archive", PlatformExternalID: "1",
	}
	repoID, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NoError(database.EnsureDiscoveryArchives(t.Context(), []int64{repoID}, now))
	require.NoError(database.ReconcileArchiveCoverage(t.Context(), repoID, db.ArchiveCoverageSet{
		Issues: db.ArchiveCoverageSupported, MergeRequests: db.ArchiveCoverageSupported,
		Comments: db.ArchiveCoverageSupported, Reviews: db.ArchiveCoverageSupported,
		InlineComments: db.ArchiveCoverageSupported,
	}, now))
	_, err = database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_issues (
			repo_id, platform_id, platform_external_id, number, url, title, author,
			state, body, created_at, updated_at, last_activity_at
		) VALUES (?, 1, 'issue-e2e', 1, 'https://github.test/owner/archive/issues/1',
			'Archive E2E issue', 'alice', 'closed', 'body', ?, ?, ?)`,
		repoID, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Hour))
	require.NoError(err)
	require.NoError(database.Close())

	daemon := procutil.Command(bin, "serve", "--config", cfgPath)
	daemon.Stdout = os.Stderr
	daemon.Stderr = os.Stderr
	daemon.Env = append(os.Environ(),
		"KENN_FORGE_LOG_LEVEL=warn",
		"KENN_FORGE_ARCHIVE_E2E_TOKEN=archive-e2e-token",
		"SSL_CERT_FILE="+certPath,
	)
	require.NoError(daemon.Start())
	daemonStopped := false
	t.Cleanup(func() {
		if !daemonStopped && daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			_ = daemon.Wait()
		}
	})
	waitForFile(t, runtimelock.MetadataPath(dataDir), 10*time.Second)
	waitForFile(t, runtimelock.AuthTokenPath(dataDir), 10*time.Second)
	require.Eventually(func() bool {
		request, requestErr := http.NewRequest(
			http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/archive-e2e/api/v1/health", port),
			nil,
		)
		if requestErr != nil {
			return false
		}
		request.Header.Set("X-Forwarded-Host", fmt.Sprintf("127.0.0.1:%d", port))
		resp, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	}, 10*time.Second, 100*time.Millisecond)

	run := func(args ...string) (string, string, int) {
		cmd := procutil.Command(bin, append([]string{"archive"}, args...)...)
		cmd.Env = append(os.Environ(), "KENN_FORGE_LOG_LEVEL=warn")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if runErr != nil {
			require.ErrorAs(runErr, &exitErr)
			code = exitErr.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	}

	stdout, stderr, code := run("status", "--json", "--config", cfgPath)
	assert.Equal(0, code)
	assert.Empty(stderr)
	assert.Contains(stdout, `"repo_path": "owner/archive"`)

	configAfterStartup, err := os.ReadFile(cfgPath)
	require.NoError(err)
	configAfterStartup = bytes.Replace(
		configAfterStartup,
		[]byte("trust_reverse_proxy = true"),
		[]byte("trust_reverse_proxy = false"),
		1,
	)
	require.NoError(os.WriteFile(cfgPath, configAfterStartup, 0o600))

	stdout, stderr, code = run("status", "--json", "--config", cfgPath)
	assert.Equal(0, code)
	assert.Empty(stderr)
	assert.Contains(stdout, `"repo_path": "owner/archive"`)

	stdout, stderr, code = run("start", "--config", cfgPath, "--all")
	assert.Equal(0, code)
	assert.Empty(stderr)
	assert.Contains(stdout, `"repo_path": "owner/archive"`)
	assert.Contains(stdout, `"collection_mode": "full"`)

	stdout, stderr, code = run("pause", "--config", cfgPath, "--all")
	assert.Equal(0, code)
	assert.Empty(stderr)
	assert.Contains(stdout, `"operator_state": "paused"`)

	output := filepath.Join(root, "archive.md")
	stdout, stderr, code = run(
		"report", "--config", cfgPath, "--days", "1", "--output", output,
	)
	assert.Equal(0, code)
	assert.Empty(stdout)
	assert.Empty(stderr)
	contents, err := os.ReadFile(output)
	require.NoError(err)
	assert.Contains(string(contents), "# Activity archive")
	assert.Contains(string(contents), "Issues opened: 1")

	stdout, stderr, code = run(
		"report", "--config", cfgPath, "--days", "1",
		"--repo", "gitlab|"+host+"/owner/missing",
	)
	assert.Equal(1, code)
	assert.Empty(stdout)
	assert.Contains(stderr, "badRequest")
	assert.NotContains(stderr, "repository is not configured")

	require.NoError(daemon.Process.Signal(syscall.SIGTERM))
	_ = daemon.Wait()
	daemonStopped = true
	require.Eventually(func() bool {
		status, readErr := runtimelock.Read(dataDir)
		return readErr == nil && !status.Running
	}, 10*time.Second, 100*time.Millisecond)
	stdout, stderr, code = run("status", "--config", cfgPath)
	assert.Equal(1, code)
	assert.Empty(stdout)
	assert.Contains(stderr, "no kenn-forge daemon is running", stderr)
}
