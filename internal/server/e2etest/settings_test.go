package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/fleet"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func doServerJSON(
	t *testing.T,
	client *http.Client,
	method, rawURL string,
	body any,
) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	var reader io.Reader = http.NoBody
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
		reader = &buf
	}
	req, err := http.NewRequestWithContext(
		t.Context(), method, rawURL, reader,
	)
	require.NoError(t, err)
	setAcceptedHostForE2ETest(req)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

func TestSettingsAPIE2EReadUpdateAndValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	getResp := doServerJSON(
		t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/settings", nil,
	)
	defer getResp.Body.Close()
	require.Equal(http.StatusOK, getResp.StatusCode)

	var settings generated.SettingsResponse
	require.NoError(json.NewDecoder(getResp.Body).Decode(&settings))
	require.Len(settings.Repos, 1)
	assert.Equal("acme", settings.Repos[0].Owner)
	assert.Equal(generated.Threaded, settings.Activity.ViewMode)
	assert.True(settings.Activity.CollapseThreads)
	require.NotNil(settings.LaunchTargets)
	assert.NotEmpty(*settings.LaunchTargets)
	plainShellTarget := findSettingsLaunchTarget(
		t, *settings.LaunchTargets, "plain_shell",
	)
	assert.Equal("Shell", plainShellTarget.Label)
	assert.Equal("plain_shell", plainShellTarget.Kind)
	assert.Equal("system", plainShellTarget.Source)
	assert.True(plainShellTarget.Available)

	invalidResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/settings",
		generated.UpdateSettingsRequest{
			Activity: &generated.Activity{
				ViewMode:  "kanban",
				TimeRange: "7d",
			},
		},
	)
	defer invalidResp.Body.Close()
	require.Equal(http.StatusUnprocessableEntity, invalidResp.StatusCode)

	cfgAfterInvalid, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal("threaded", cfgAfterInvalid.Activity.ViewMode)

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/settings",
		generated.UpdateSettingsRequest{
			Workspaces: &generated.WorkspaceSettingsUpdate{
				AutoAssignOnCreate: new(true),
				DefaultSidebarView: new(generated.WorkspaceSettingsUpdateDefaultSidebarViewItem),
			},
			Activity: &generated.Activity{
				ViewMode:        "flat",
				TimeRange:       "30d",
				HideClosed:      true,
				HideBots:        true,
				CollapseThreads: true,
			},
			Terminal: &generated.Terminal{
				FontFamily:     "\"Iosevka Term\", monospace",
				FontSize:       18,
				Scrollback:     5000,
				LineHeight:     1.15,
				CursorBlink:    true,
				FontLigatures:  true,
				HideTmuxStatus: true,
			},
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	var updated generated.SettingsResponse
	require.NoError(json.NewDecoder(updateResp.Body).Decode(&updated))
	assert.True(updated.Activity.CollapseThreads)
	assert.True(updated.Workspaces.AutoAssignOnCreate)
	assert.Equal(generated.WorkspacesDefaultSidebarViewItem, updated.Workspaces.DefaultSidebarView)

	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal("flat", cfgAfterUpdate.Activity.ViewMode)
	assert.Equal("30d", cfgAfterUpdate.Activity.TimeRange)
	assert.True(cfgAfterUpdate.Activity.HideClosed)
	assert.True(cfgAfterUpdate.Activity.HideBots)
	assert.True(cfgAfterUpdate.Activity.CollapseThreads)
	assert.True(cfgAfterUpdate.Workspaces.AutoAssignOnCreate)
	assert.Equal("item", cfgAfterUpdate.Workspaces.DefaultSidebarView)
	assert.Equal(
		"\"Iosevka Term\", monospace",
		cfgAfterUpdate.Terminal.FontFamily,
	)
	assert.Equal(18, cfgAfterUpdate.Terminal.FontSize)
	assert.Equal(5000, cfgAfterUpdate.Terminal.Scrollback)
	assert.InDelta(1.15, cfgAfterUpdate.Terminal.LineHeight, 0.001)
	assert.True(cfgAfterUpdate.Terminal.FontLigatures)
	assert.True(cfgAfterUpdate.Terminal.HideTmuxStatus)

	reGetResp := doServerJSON(
		t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/settings", nil,
	)
	defer reGetResp.Body.Close()
	require.Equal(http.StatusOK, reGetResp.StatusCode)
	var reGet generated.SettingsResponse
	require.NoError(json.NewDecoder(reGetResp.Body).Decode(&reGet))
	assert.True(reGet.Activity.CollapseThreads)
	assert.True(reGet.Workspaces.AutoAssignOnCreate)
	assert.Equal(generated.WorkspacesDefaultSidebarViewItem, reGet.Workspaces.DefaultSidebarView)
	assert.True(reGet.Terminal.HideTmuxStatus)
}

func TestFleetSettingsPublishesOnlyCommittedRuntimeSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const (
		hubID          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		memberID       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		token          = "cccccccccccccccccccccccccccccccccccccccccccccccc"
		peerReplyDelay = time.Second
	)

	var peerRequests atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerRequests.Add(1)
		select {
		case <-time.After(peerReplyDelay):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"protocolVersion":3,"nodeID":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","host":{"hostname":"remote","platform":"linux"}}`))
		case <-r.Context().Done():
		}
	}))
	defer peer.Close()

	credentials, err := federationauth.Open(filepath.Join(t.TempDir(), "credentials.json"))
	require.NoError(err)
	require.NoError(credentials.StoreOutbound(
		memberID, token, federationauth.HubToSpokeScopes(),
	))
	configText := fmt.Sprintf(`
host = "127.0.0.1"
port = 8091

[api]
require_auth = true

[fleet]
enabled = true
role = "hub"
base_url = "https://hub.example"
peer_timeout = "2s"

[[fleet.members]]
node_id = "%s"
name = "Remote"
base_url = "%s"
state = "active"
`, memberID, peer.URL)
	srv, _, cfgPath := setupTestServerWithConfigContentAndOptions(
		t, configText, &mockGH{}, server.ServerOptions{
			FederationCredentials: credentials,
			FederationEnrollments: activeHubEnrollmentsForTest(
				t, hubID, "https://hub.example", []config.FleetMember{{
					NodeID: memberID, Name: "Remote", BaseURL: peer.URL,
					State: federation.EnrollmentActive,
				}},
			),
			FederationSpokeID:    hubID,
			FederationHTTPClient: peer.Client(),
		},
	)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	putFleet := func(timeout string, enabled bool) *http.Response {
		return doServerJSON(t, ts.Client(), http.MethodPut,
			ts.URL+"/api/v1/settings/fleet", map[string]any{
				"enabled": enabled, "peer_timeout": timeout,
				"sessions": map[string]any{"include_unmanaged_details": false},
			})
	}
	readSnapshot := func() (fleet.Snapshot, time.Duration) {
		started := time.Now()
		resp := doServerJSON(t, ts.Client(), http.MethodGet,
			ts.URL+"/api/v1/snapshot?include_peers=true", nil)
		defer resp.Body.Close()
		require.Equal(http.StatusOK, resp.StatusCode)
		var snapshot fleet.Snapshot
		require.NoError(json.NewDecoder(resp.Body).Decode(&snapshot))
		return snapshot, time.Since(started)
	}
	hostByKey := func(snapshot fleet.Snapshot, key string) *fleet.HostSummary {
		for i := range snapshot.Hosts {
			if snapshot.Hosts[i].ConfigKey == key {
				return &snapshot.Hosts[i]
			}
		}
		return nil
	}

	update := putFleet("300ms", true)
	defer update.Body.Close()
	require.Equal(http.StatusOK, update.StatusCode)

	snapshot, elapsed := readSnapshot()
	require.NotNil(hostByKey(snapshot, hubID))
	remote := hostByKey(snapshot, memberID)
	require.NotNil(remote)
	assert.False(remote.Reachable, "the committed timeout must degrade the blocked member")
	awaitPeerRequest(t, &peerRequests, 0, "the committed member must be probed on the first read")
	assert.Less(elapsed, 1500*time.Millisecond)

	require.NoError(os.Remove(cfgPath))
	require.NoError(os.Mkdir(cfgPath, 0o700))
	failed := putFleet("3s", false)
	defer failed.Body.Close()
	require.Equal(http.StatusInternalServerError, failed.StatusCode)

	requestsBefore := peerRequests.Load()
	snapshot, elapsed = readSnapshot()
	require.NotNil(hostByKey(snapshot, hubID))
	remote = hostByKey(snapshot, memberID)
	require.NotNil(remote, "failed persistence must leave the committed member published")
	assert.False(remote.Reachable)
	awaitPeerRequest(t, &peerRequests, requestsBefore,
		"failed persistence must leave the committed member published")
	assert.Less(elapsed, 1500*time.Millisecond,
		"failed persistence must leave the committed timeout published")
}

// awaitPeerRequest waits for the peer to observe a request beyond the ones
// already counted. The hub abandons a peer probe the moment the committed
// peer timeout expires, so a snapshot read can return before the peer server
// has accepted and dispatched a request that was already put on the wire.
// Reading the counter synchronously races that dispatch.
func awaitPeerRequest(t *testing.T, requests *atomic.Int32, seen int32, msg string) {
	t.Helper()
	assert.Eventually(t, func() bool { return requests.Load() > seen },
		5*time.Second, 5*time.Millisecond, msg)
}

func findSettingsLaunchTarget(
	t *testing.T,
	targets []generated.LaunchTarget,
	key string,
) generated.LaunchTarget {
	t.Helper()
	for _, target := range targets {
		if target.Key == key {
			return target
		}
	}
	require.Failf(t, "target not found", "key %q", key)
	return generated.LaunchTarget{}
}

func TestSettingsAPIE2EHideTmuxStatusUpdateAffectsRuntimeSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
record=%q
printf '%%s\0' "$#" "$@" >> "$record"
if [ "$1" = "-u" ]; then shift; fi
case "$1" in
  has-session)
    echo "can't find session: $3" >&2
    exit 1
    ;;
  attach-session)
    trap 'exit 0' HUP INT TERM
    while :; do sleep 1; done
    ;;
esac
exit 0
`, record), 0o755))
	workspacePath := filepath.Join(dir, "workspace")
	require.NoError(os.MkdirAll(workspacePath, 0o755))
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, fmt.Appendf(nil, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[tmux]
command = [%q]

[[agents]]
key = "noop"
label = "Noop"
command = ["/bin/sh", "-c", "sleep 60"]

[[repos]]
owner = "acme"
name = "widget"
`, tmuxPath), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)
	database := dbtest.Open(t)
	seedReadyRuntimeWorkspace(t, database, workspacePath)
	syncer := github.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{
			WorktreeDir:                        filepath.Join(dir, "worktrees"),
			DisableWorkspaceBackgroundMonitors: true,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	updateResp := doServerJSON(
		t, httpServer.Client(), http.MethodPut,
		httpServer.URL+"/api/v1/settings",
		generated.UpdateSettingsRequest{
			Terminal: &generated.Terminal{
				FontSize:       14,
				Scrollback:     1000,
				LineHeight:     1,
				CursorBlink:    true,
				HideTmuxStatus: true,
			},
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	launchResp := doServerJSON(
		t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/v1/workspaces/ws-token-runtime/runtime/sessions",
		map[string]string{"target_key": "noop"},
	)
	defer launchResp.Body.Close()
	launchBody, err := io.ReadAll(launchResp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode, string(launchBody))

	require.Eventually(func() bool {
		raw, err := os.ReadFile(record)
		if err != nil {
			return false
		}
		data := string(raw)
		return strings.Contains(data, "set-option\x00-q\x00-t") &&
			strings.Contains(data, "status\x00off")
	}, 3*time.Second, 10*time.Millisecond)
	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(cfgAfterUpdate.Terminal.HideTmuxStatus)
}

func TestSettingsAPIE2EPreservesStartupConfigThroughSettingsSave(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "10m"
github_token_env = "KENN_FORGE_RELOADED_GITHUB_TOKEN"
host = "127.0.0.2"
port = 9191
base_path = "/kenn-forge"
allowed_hosts = ["forge.test:9191"]
trust_reverse_proxy = false

[[repos]]
owner = "acme"
name = "widget"

[api]
require_auth = true

[fleet.sessions]
include_unmanaged_details = true

[roborev]
endpoint = "http://127.0.0.1:7374"

[tmux]
command = ["systemd-run", "--user", "--scope", "tmux"]

[shell]
command = ["systemd-run", "--user", "--scope", "--pty", "bash"]
`, &mockGH{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/kenn-forge/api/v1/settings",
		generated.UpdateSettingsRequest{
			Activity: &generated.Activity{
				ViewMode:  "flat",
				TimeRange: "30d",
			},
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal("10m", cfgAfterUpdate.SyncInterval)
	assert.Equal("KENN_FORGE_RELOADED_GITHUB_TOKEN", cfgAfterUpdate.GitHubTokenEnv)
	assert.Equal("127.0.0.2", cfgAfterUpdate.Host)
	assert.Equal(9191, cfgAfterUpdate.Port)
	assert.Equal("/kenn-forge/", cfgAfterUpdate.BasePath)
	assert.Equal([]string{"forge.test:9191"}, cfgAfterUpdate.AllowedHosts)
	assert.False(cfgAfterUpdate.TrustReverseProxy)
	assert.True(cfgAfterUpdate.API.RequireAuth)
	assert.True(cfgAfterUpdate.Fleet.Sessions.IncludeUnmanagedDetails)
	assert.Equal("http://127.0.0.1:7374", cfgAfterUpdate.Roborev.Endpoint)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "tmux"},
		cfgAfterUpdate.Tmux.Command,
	)
	assert.Equal(
		[]string{"systemd-run", "--user", "--scope", "--pty", "bash"},
		cfgAfterUpdate.Shell.Command,
	)
	assert.Equal("flat", cfgAfterUpdate.Activity.ViewMode)
	assert.Equal("30d", cfgAfterUpdate.Activity.TimeRange)
}

func TestSettingsAPIE2EFleetReadUpdateAndValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const memberID = "fedcba9876543210fedcba9876543210"
	srv, _, cfgPath := setupTestServerWithConfigContent(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[api]
require_auth = true

[fleet]
enabled = true
role = "hub"
base_url = "https://hub.example"
peer_timeout = "3s"

[[fleet.members]]
node_id = "fedcba9876543210fedcba9876543210"
name = "Build spoke"
base_url = "https://spoke.example"
state = "active"
`, &mockGH{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	getResp := doServerJSON(
		t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/settings/fleet", nil,
	)
	defer getResp.Body.Close()
	require.Equal(http.StatusOK, getResp.StatusCode)

	var settings struct {
		Enabled     bool             `json:"enabled"`
		Role        config.FleetRole `json:"role"`
		PeerTimeout string           `json:"peer_timeout"`
		Members     []struct {
			NodeID  string `json:"node_id"`
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			State   string `json:"state"`
		} `json:"members"`
		RestartRequired bool `json:"restart_required"`
	}
	require.NoError(json.NewDecoder(getResp.Body).Decode(&settings))
	assert.True(settings.Enabled)
	assert.Equal(config.FleetRoleHub, settings.Role)
	assert.Equal("3s", settings.PeerTimeout)
	require.Len(settings.Members, 1)
	assert.Equal(memberID, settings.Members[0].NodeID)
	assert.Equal("Build spoke", settings.Members[0].Name)
	assert.False(settings.RestartRequired)

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/settings/fleet",
		map[string]any{
			"enabled": true, "peer_timeout": "4s",
			"sessions": map[string]any{"include_unmanaged_details": false},
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	assert.True(cfgAfterUpdate.Fleet.Enabled)
	assert.Equal(config.FleetRoleHub, cfgAfterUpdate.Fleet.Role)
	assert.Equal("4s", cfgAfterUpdate.Fleet.PeerTimeout)
	require.Len(cfgAfterUpdate.Fleet.Members, 1)
	assert.Equal(memberID, cfgAfterUpdate.Fleet.Members[0].NodeID)
	assert.Equal("Build spoke", cfgAfterUpdate.Fleet.Members[0].Name)

	for _, timeout := range []string{"0s", "-1s"} {
		invalidTimeout := doServerJSON(
			t, ts.Client(), http.MethodPut,
			ts.URL+"/api/v1/settings/fleet",
			map[string]any{
				"enabled": true, "peer_timeout": timeout,
				"sessions": map[string]any{"include_unmanaged_details": false},
			},
		)
		assert.Equal(http.StatusBadRequest, invalidTimeout.StatusCode)
		invalidTimeout.Body.Close()
	}

	invalidResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/settings/fleet",
		map[string]any{
			"enabled": true, "role": "spoke", "peer_timeout": "2s",
			"sessions": map[string]any{"include_unmanaged_details": false},
			"members":  []map[string]any{},
		},
	)
	defer invalidResp.Body.Close()
	require.Equal(http.StatusUnprocessableEntity, invalidResp.StatusCode)

	afterInvalid, err := config.Load(cfgPath)
	require.NoError(err)
	assert.Equal(config.FleetRoleHub, afterInvalid.Fleet.Role)
	assert.Equal("Build spoke", afterInvalid.Fleet.Members[0].Name)
}

func TestRepoConfigAPIE2EAddDeleteAndErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	duplicateResp := doServerJSON(
		t, ts.Client(), http.MethodPost,
		ts.URL+"/api/v1/repos",
		map[string]string{
			"provider": "github",
			"host":     "github.com",
			"owner":    "acme",
			"name":     "widget",
		},
	)
	defer duplicateResp.Body.Close()
	require.Equal(http.StatusBadRequest, duplicateResp.StatusCode)

	addResp := doServerJSON(
		t, ts.Client(), http.MethodPost,
		ts.URL+"/api/v1/repos",
		map[string]string{
			"provider": "github",
			"host":     "github.com",
			"owner":    "other-org",
			"name":     "other-repo",
		},
	)
	defer addResp.Body.Close()
	require.Equal(http.StatusCreated, addResp.StatusCode)

	cfgAfterAdd, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterAdd.Repos, 2)
	assert.Equal("other-org", cfgAfterAdd.Repos[1].Owner)
	assert.Equal("other-repo", cfgAfterAdd.Repos[1].Name)

	missingDeleteResp := doServerJSON(
		t, ts.Client(), http.MethodDelete,
		ts.URL+"/api/v1/repo/gh/nope/missing", nil,
	)
	defer missingDeleteResp.Body.Close()
	require.Equal(http.StatusNotFound, missingDeleteResp.StatusCode)

	deleteResp := doServerJSON(
		t, ts.Client(), http.MethodDelete,
		ts.URL+"/api/v1/repo/gh/acme/widget", nil,
	)
	defer deleteResp.Body.Close()
	require.Equal(http.StatusNoContent, deleteResp.StatusCode)

	cfgAfterDelete, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterDelete.Repos, 1)
	assert.Equal("other-org", cfgAfterDelete.Repos[0].Owner)
}

func TestRepoConfigAPIE2EUpdatesWorktreeBasePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	localRepo := setupSettingsLocalGitRepoForDefaultHost(t)

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/worktree-base",
		generated.RepoWorktreeBaseRequest{
			WorktreeBasePath: localRepo,
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	var updated generated.SettingsResponse
	require.NoError(json.NewDecoder(updateResp.Body).Decode(&updated))
	require.Len(updated.Repos, 1)
	require.NotNil(updated.Repos[0].WorktreeBasePath)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	assert.Equal(canonicalLocalRepo, *updated.Repos[0].WorktreeBasePath)

	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterUpdate.Repos, 1)
	assert.Equal(canonicalLocalRepo, cfgAfterUpdate.Repos[0].WorktreeBasePath)

	clearResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/worktree-base",
		generated.RepoWorktreeBaseRequest{},
	)
	defer clearResp.Body.Close()
	require.Equal(http.StatusOK, clearResp.StatusCode)

	cfgAfterClear, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterClear.Repos, 1)
	assert.Empty(cfgAfterClear.Repos[0].WorktreeBasePath)
}

func TestRepoConfigAPIE2EAcceptsForkStyleWorktreeBase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	localRepo := setupSettingsLocalGitRepoForDefaultHost(t)
	runSettingsGit(t, localRepo, "remote", "rename", "origin", "upstream")
	runSettingsGit(
		t, localRepo, "remote", "add", "origin",
		"https://github.com/forker/widget.git",
	)

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/worktree-base",
		generated.RepoWorktreeBaseRequest{WorktreeBasePath: localRepo},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusOK, updateResp.StatusCode)

	var updated generated.SettingsResponse
	require.NoError(json.NewDecoder(updateResp.Body).Decode(&updated))
	require.Len(updated.Repos, 1)
	require.NotNil(updated.Repos[0].WorktreeBasePath)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	assert.Equal(canonicalLocalRepo, *updated.Repos[0].WorktreeBasePath)

	cfgAfterUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterUpdate.Repos, 1)
	assert.Equal(canonicalLocalRepo, cfgAfterUpdate.Repos[0].WorktreeBasePath)
}

func TestRepoConfigAPIE2EUpdatesUIVisibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _ := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	entry, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform:       "github",
			PlatformHost:   "github.com",
			PlatformRepoID: "repo-acme-widget",
			Owner:          "acme",
			Name:           "widget",
		}, time.Now().UTC(),
	)
	require.NoError(err)
	require.NotNil(entry)

	summaryNames := func() []string {
		resp, err := ts.Client().Get(ts.URL + "/api/v1/repos/summary")
		require.NoError(err)
		defer resp.Body.Close()
		require.Equal(http.StatusOK, resp.StatusCode)
		var summaries []generated.RepoSummaryResponse
		require.NoError(json.NewDecoder(resp.Body).Decode(&summaries))
		names := make([]string, 0, len(summaries))
		for _, summary := range summaries {
			names = append(names, summary.Name)
		}
		return names
	}
	require.Equal([]string{"widget"}, summaryNames())

	hideResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/ui-visibility",
		generated.RepoUIVisibilityRequest{Hidden: true},
	)
	defer hideResp.Body.Close()
	require.Equal(http.StatusOK, hideResp.StatusCode)

	var hidden generated.SettingsResponse
	require.NoError(json.NewDecoder(hideResp.Body).Decode(&hidden))
	require.Len(hidden.Repos, 1)
	assert.True(hidden.Repos[0].HiddenFromUi)
	assert.Empty(summaryNames(),
		"hidden repo leaves the repository overview summaries")

	showResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/ui-visibility",
		generated.RepoUIVisibilityRequest{Hidden: false},
	)
	defer showResp.Body.Close()
	require.Equal(http.StatusOK, showResp.StatusCode)

	var shown generated.SettingsResponse
	require.NoError(json.NewDecoder(showResp.Body).Decode(&shown))
	require.Len(shown.Repos, 1)
	assert.False(shown.Repos[0].HiddenFromUi)
	assert.Equal([]string{"widget"}, summaryNames(),
		"showing restores the repo in the repository overview summaries")
}

func TestRepoConfigAPIE2ERejectsUnsafeWorktreeScopedBaseConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupTestServerWithConfig(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	localRepo := setupSettingsLocalGitRepoForDefaultHost(t)
	runSettingsGit(t, localRepo, "config", "extensions.worktreeConfig", "true")
	runSettingsGit(
		t, localRepo, "config", "--worktree",
		"filter.demo.clean", "demo-clean",
	)

	updateResp := doServerJSON(
		t, ts.Client(), http.MethodPut,
		ts.URL+"/api/v1/repo/github/acme/widget/worktree-base",
		generated.RepoWorktreeBaseRequest{
			WorktreeBasePath: localRepo,
		},
	)
	defer updateResp.Body.Close()
	require.Equal(http.StatusBadRequest, updateResp.StatusCode)
	body, err := io.ReadAll(updateResp.Body)
	require.NoError(err)
	assert.Contains(string(body), "filter.demo.clean")

	cfgAfterRejectedUpdate, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(cfgAfterRejectedUpdate.Repos, 1)
	assert.Empty(cfgAfterRejectedUpdate.Repos[0].WorktreeBasePath)
}

// settingsWorkspaceEnv is the shared scaffolding for worktree-base workspace
// e2e tests: a configured repo, seeded DB, running server, and API client.
type settingsWorkspaceEnv struct {
	ts           *httptest.Server
	client       *generated.ClientWithResponses
	database     *db.DB
	localRepo    string
	remote       string
	platformHost string
}

func setupSettingsWorkspaceEnv(
	t *testing.T, seed func(database *db.DB, repoID int64),
) settingsWorkspaceEnv {
	t.Helper()
	require := require.New(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	localRepo, remote, platformHost := setupSettingsLocalGitRepo(t)
	require.NoError(os.WriteFile(cfgPath, []byte(`
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[tmux]
command = ["sh", "-c", "exit 0"]

[[repos]]
platform_host = "`+platformHost+`"
owner = "acme"
name = "widget"
`), 0o644))
	cfg, err := config.Load(cfgPath)
	require.NoError(err)

	database := dbtest.Open(t)
	identity := db.GitHubRepoIdentity(platformHost, "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(
		t.Context(), identity,
	)
	require.NoError(err)
	cloneURL := strings.TrimSpace(string(runSettingsGitOutput(
		t, localRepo, "remote", "get-url", "origin",
	)))
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			PlatformRepoID: "repo-acme-widget",
			WebURL:         strings.TrimSuffix(cloneURL, ".git"),
			CloneURL:       cloneURL,
			DefaultBranch:  "main",
		},
	))
	seed(database, repoID)

	syncer := github.NewSyncer(
		map[string]github.Client{"github.com": &mockGH{}},
		database, nil,
		[]github.RepoRef{{
			Owner: "acme", Name: "widget", PlatformHost: platformHost,
			PlatformExternalID: "repo-acme-widget",
		}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.NewWithConfig(t,
		database, syncer, nil, nil, cfg, cfgPath,
		server.ServerOptions{WorktreeDir: filepath.Join(dir, "workspaces")},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// The server validates the request Host against its configured listen
	// address, so the generated client must present the configured host
	// rather than the random httptest port.
	client, err := generated.NewClientWithResponses(
		ts.URL+"/api/v1",
		generated.WithRequestEditorFn(
			func(_ context.Context, req *http.Request) error {
				setAcceptedHostForE2ETest(req)
				return nil
			},
		),
	)
	require.NoError(err)
	return settingsWorkspaceEnv{
		ts:           ts,
		client:       client,
		database:     database,
		localRepo:    localRepo,
		remote:       remote,
		platformHost: platformHost,
	}
}

// setWorktreeBase points the configured repo's worktree base at the env's
// local repo through the settings API. Call it after any git setup the
// validation needs to observe.
func (env settingsWorkspaceEnv) setWorktreeBase(t *testing.T) {
	t.Helper()
	resp, err := env.client.UpdateRepoWorktreeBaseOnHostWithResponse(
		t.Context(), env.platformHost, "github", "acme", "widget",
		generated.RepoWorktreeBaseRequest{WorktreeBasePath: env.localRepo},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode(), string(resp.Body))
}

func TestRepoConfigAPIE2EWorkspaceCreationUsesWorktreeBasePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupSettingsWorkspaceEnv(t, func(database *db.DB, repoID int64) {
		seedSettingsWorkspaceMR(t, database, repoID, 42, "feature/thing")
	})
	client, database := env.client, env.database
	localRepo, remote, platformHost := env.localRepo, env.remote, env.platformHost

	runSettingsGit(
		t, localRepo,
		"push", remote, "HEAD:refs/heads/feature/thing",
	)
	runSettingsGit(t, remote, "update-server-info")
	runSettingsGit(t, localRepo, "fetch", "--prune", "origin")

	env.setWorktreeBase(t)
	createResp, err := client.CreateWorkspaceWithResponse(
		t.Context(),
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: platformHost,
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     42,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode(), string(createResp.Body))
	require.NotNil(createResp.JSON202)

	ready := waitForSettingsWorkspaceReady(t, client, createResp.JSON202.Id)
	assert.Equal("ready", ready.Status)
	assert.Equal("feature/thing", ready.GitHeadRef)
	stored, err := database.GetWorkspace(t.Context(), ready.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("feature/thing", stored.WorkspaceBranch)

	canonicalWorktreePath, err := filepath.EvalSymlinks(ready.WorktreePath)
	require.NoError(err)
	listOutput := string(runSettingsGitOutput(
		t, localRepo, "worktree", "list", "--porcelain",
	))
	assert.Contains(listOutput, "worktree "+canonicalWorktreePath)
}

func TestRepoConfigAPIE2EWorkspaceCreationUsesFallbackBranchWhenPreferredCheckedOut(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupSettingsWorkspaceEnv(t, func(database *db.DB, repoID int64) {
		seedSettingsWorkspaceMR(t, database, repoID, 42, "feature/thing")
	})
	client, database := env.client, env.database
	localRepo, remote, platformHost := env.localRepo, env.remote, env.platformHost

	runSettingsGit(
		t, localRepo,
		"push", remote, "HEAD:refs/heads/feature/thing",
	)
	runSettingsGit(t, remote, "update-server-info")
	runSettingsGit(t, localRepo, "fetch", "--prune", "origin")
	runSettingsGit(
		t, localRepo, "checkout", "-B",
		"feature/thing", "refs/remotes/origin/feature/thing",
	)

	env.setWorktreeBase(t)
	createResp, err := client.CreateWorkspaceWithResponse(
		t.Context(),
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: platformHost,
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     42,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode(), string(createResp.Body))
	require.NotNil(createResp.JSON202)

	ready := waitForSettingsWorkspaceReady(t, client, createResp.JSON202.Id)
	assert.Equal("ready", ready.Status)
	assert.Equal("feature/thing", ready.GitHeadRef)
	stored, err := database.GetWorkspace(t.Context(), ready.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("kenn-forge/pr-42", stored.WorkspaceBranch)
	assert.Equal("kenn-forge/pr-42", strings.TrimSpace(string(runSettingsGitOutput(
		t, ready.WorktreePath, "branch", "--show-current",
	))))

	canonicalWorktreePath, err := filepath.EvalSymlinks(ready.WorktreePath)
	require.NoError(err)
	listOutput := string(runSettingsGitOutput(
		t, localRepo, "worktree", "list", "--porcelain",
	))
	assert.Contains(listOutput, "worktree "+canonicalWorktreePath)
}

func TestWorkspaceAPIE2ERejectsEmptyProviderForAmbiguousRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)
	syncer := github.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t,
		database, syncer, nil, "/", nil,
		server.ServerOptions{WorktreeDir: filepath.Join(t.TempDir(), "workspaces")},
	)
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, provider := range []string{"github", "gitlab"} {
		repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
			Platform:     provider,
			PlatformHost: "forge.example.com",
			Owner:        "acme",
			Name:         "widget",
		})
		require.NoError(err)
		seedSettingsWorkspaceMR(t, database, repoID, 7, "feature")
	}
	client, err := generated.NewClientWithResponses(ts.URL + "/api/v1")
	require.NoError(err)
	provider := ""

	resp, err := client.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     provider,
			PlatformHost: "forge.example.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     7,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusBadRequest, resp.StatusCode(), string(resp.Body))

	var problem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.Unmarshal(resp.Body, &problem))
	assert.Equal("validationError", problem.Code)
	assert.Equal("body.provider", problem.Details["field"])
}

func TestRepoConfigAPIE2EDeleteReusedIssueBranchKeepsLocalBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupSettingsWorkspaceEnv(t, func(database *db.DB, repoID int64) {
		seedSettingsWorkspaceIssue(t, database, repoID, 7, "")
	})
	client, database := env.client, env.database
	localRepo, platformHost := env.localRepo, env.platformHost
	ts := env.ts

	const branch = "kenn-forge/issue-7"
	runSettingsGit(t, localRepo, "branch", branch, "HEAD")

	env.setWorktreeBase(t)
	reuse := true
	createResp, err := client.CreateIssueWorkspaceOnHostWithResponse(
		t.Context(), platformHost, "github", "acme", "widget", 7,
		generated.CreateIssueWorkspaceOnHostJSONRequestBody{
			ReuseExistingBranch: &reuse,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode(), string(createResp.Body))
	require.NotNil(createResp.JSON202)

	ready := waitForSettingsWorkspaceReady(t, client, createResp.JSON202.Id)
	assert.Equal("ready", ready.Status)
	assert.Equal(branch, strings.TrimSpace(string(runSettingsGitOutput(
		t, ready.WorktreePath, "branch", "--show-current",
	))))
	stored, err := database.GetWorkspace(t.Context(), ready.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Empty(stored.WorkspaceBranch)

	deleteResp := doServerJSON(
		t, ts.Client(), http.MethodDelete,
		ts.URL+"/api/v1/workspaces/"+ready.Id+"?force=true",
		map[string]any{},
	)
	defer deleteResp.Body.Close()
	require.Equal(http.StatusNoContent, deleteResp.StatusCode)
	assert.Contains(
		string(runSettingsGitOutput(t, localRepo, "branch", "--list", branch)),
		branch,
	)
}

func TestRepoConfigAPIE2ERefreshGlobAndErrors(t *testing.T) {
	assert := assert.New(t)
	mock := &mockGH{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:  new("widget-one"),
					Owner: &gh.User{Login: new(owner)},
				},
				{
					Name:  new("tooling"),
					Owner: &gh.User{Login: new(owner)},
				},
			}, nil
		},
	}
	srv, _, _, syncer := setupTestServerWithConfigContentAndSyncer(t, `
sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "acme"
name = "widget-*"
`, mock)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	refreshPath := "/api/v1/repo/gh/acme/" +
		url.PathEscape("widget-*") + "/refresh"
	refreshResp := doServerJSON(
		t, ts.Client(), http.MethodPost,
		ts.URL+refreshPath, nil,
	)
	defer refreshResp.Body.Close()
	require.Equal(t, http.StatusOK, refreshResp.StatusCode)
	assert.True(syncer.IsTrackedRepo("acme", "widget-one"))
	assert.False(syncer.IsTrackedRepo("acme", "tooling"))

	nonGlob, _, _ := setupTestServerWithConfig(t)
	nonGlobTS := httptest.NewServer(nonGlob)
	defer nonGlobTS.Close()
	nonGlobResp := doServerJSON(
		t, nonGlobTS.Client(), http.MethodPost,
		nonGlobTS.URL+"/api/v1/repo/gh/acme/widget/refresh", nil,
	)
	defer nonGlobResp.Body.Close()
	require.Equal(t, http.StatusBadRequest, nonGlobResp.StatusCode)
}

func setupSettingsLocalGitRepo(t *testing.T) (repo, remote, platformHost string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "acme", "widget.git")
	repo = filepath.Join(root, "widget")
	require.NoError(t, os.MkdirAll(filepath.Dir(remote), 0o755))
	runSettingsGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	gitServer := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(gitServer.Close)
	remoteURL := gitServer.URL + "/acme/widget.git"
	parsed, err := url.Parse(gitServer.URL)
	require.NoError(t, err)
	platformHost = parsed.Host

	runSettingsGit(t, root, "init", "--initial-branch=main", repo)
	runSettingsGit(t, repo, "config", "user.email", "test@example.com")
	runSettingsGit(t, repo, "config", "user.name", "Test")
	runSettingsGit(t, repo, "remote", "add", "origin", remote)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "README.md"), []byte("test\n"), 0o644,
	))
	runSettingsGit(t, repo, "add", ".")
	runSettingsGit(t, repo, "commit", "-m", "initial commit")
	runSettingsGit(t, repo, "push", "origin", "HEAD:refs/heads/main")
	runSettingsGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runSettingsGit(t, remote, "update-server-info")
	runSettingsGit(t, repo, "remote", "set-url", "origin", remoteURL)
	runSettingsGit(t, repo, "fetch", "--prune", "origin")
	runSettingsGit(
		t, repo, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main",
	)
	return repo, remote, platformHost
}

func setupSettingsLocalGitRepoForDefaultHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "widget")
	runSettingsGit(t, root, "init", "--initial-branch=main", repo)
	runSettingsGit(t, repo, "config", "user.email", "test@example.com")
	runSettingsGit(t, repo, "config", "user.name", "Test")
	runSettingsGit(
		t, repo, "remote", "add", "origin",
		"https://github.com/acme/widget.git",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "README.md"), []byte("test\n"), 0o644,
	))
	runSettingsGit(t, repo, "add", ".")
	runSettingsGit(t, repo, "commit", "-m", "initial commit")
	return repo
}

func runSettingsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
}

func runSettingsGitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
	return out
}

func seedSettingsWorkspaceMR(
	t *testing.T, database *db.DB,
	repoID int64, number int, headBranch string,
) {
	t.Helper()
	repo, err := database.GetRepoByID(t.Context(), repoID)
	require.NoError(t, err)
	require.NotNil(t, repo)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       repoID*10000 + int64(number),
		Number:           number,
		Title:            "Test PR",
		Author:           "author",
		State:            db.MergeRequestStateOpen,
		HeadBranch:       headBranch,
		HeadRepoCloneURL: "https://" + repo.PlatformHost + "/" + repo.Owner + "/" + repo.Name + ".git",
		BaseBranch:       "main",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	})
	require.NoError(t, err)
}

func seedSettingsWorkspaceIssue(
	t *testing.T, database *db.DB,
	repoID int64, number int, title string,
) {
	t.Helper()
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	_, err := database.UpsertIssue(t.Context(), &db.Issue{
		RepoID:         repoID,
		PlatformID:     repoID*10000 + int64(number),
		Number:         number,
		Title:          title,
		Author:         "author",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(t, err)
}

func waitForSettingsWorkspaceReady(
	t *testing.T,
	client *generated.ClientWithResponses,
	workspaceID string,
) *generated.WorkspaceResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := client.GetWorkspaceWithResponse(ctx, workspaceID)
		require.NoError(t, err)
		if resp.StatusCode() == http.StatusOK &&
			resp.JSON200 != nil &&
			resp.JSON200.Status == "ready" {
			return resp.JSON200
		}

		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "workspace %s never reached ready", workspaceID)
		case <-ticker.C:
		}
	}
}
