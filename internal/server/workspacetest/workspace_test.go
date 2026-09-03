package workspacetest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func TestWorkspaceFixtureUsesIsolatedTmuxServer(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not available")
	}

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		deleteWorkspaceForPtyOwnerTest(t, cleanupCtx, fixture, ws.Id)
	})

	err = procutil.Run(
		ctx,
		procutil.CommandContext(
			ctx, tmuxPath, "has-session", "-t", ws.TmuxSession,
		),
		"inspect default test tmux server",
	)
	assert.Error(t, err, "workspace session leaked into the default tmux server")
}

func TestWorkspaceRuntimeTargetsE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	resp, err := fixture.client.HTTP.GetWorkspaceRuntimeWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.LaunchTargets)
	require.NotNil(resp.JSON200.Sessions)
	assert.NotEmpty(resp.JSON200.LaunchTargets)
	assert.Empty(resp.JSON200.Sessions)
	assertWorkspaceRuntimeTarget(
		t, resp.JSON200.LaunchTargets, "plain_shell",
	)
	assertWorkspaceRuntimeTargetAbsent(t, resp.JSON200.LaunchTargets, "shell")
}

func TestWorkspaceRuntimeClaimFailureClosesBeforeFollowingInputE2E(t *testing.T) {
	if len(workspaceTestTmuxCommand) == 0 {
		t.Skip("tmux is required")
	}
	require := require.New(t)
	dir := t.TempDir()
	failMarker := filepath.Join(dir, "fail-refresh")
	inputMarker := filepath.Join(dir, "input-reached")
	wrapper := filepath.Join(dir, "tmux-wrapper")
	require.NoError(os.WriteFile(wrapper, []byte(`#!/bin/sh
fail_marker=$1
shift
for arg in "$@"; do
  if [ "$arg" = "refresh-client" ] && [ -f "$fail_marker" ]; then
    echo "forced refresh failure" >&2
    exit 42
  fi
done
exec "$@"
`), 0o755))
	tmuxCommand := append([]string{wrapper, failMarker}, workspaceTestTmuxCommand...)
	fixture := setupWorkspaceServerFixture(t, &config.Config{
		Tmux: config.Tmux{Command: tmuxCommand},
	}, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
	})
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launch, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{TargetKey: "plain_shell"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launch.StatusCode(), string(launch.Body))
	require.NotNil(launch.JSON200)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + launch.JSON200.Key +
		"/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	workspaceTerminalConnWriteRead(
		t, ctx, conn, "printf 'ready-for-claim\\n'\r", "ready-for-claim",
	)
	require.NoError(os.WriteFile(failMarker, []byte("fail\n"), 0o644))
	require.NoError(conn.Write(
		ctx, websocket.MessageText,
		[]byte(`{"type":"claim_resize","cols":132,"rows":43}`),
	))
	require.NoError(conn.Write(
		ctx, websocket.MessageBinary,
		fmt.Appendf(nil, "printf reached > %q\r", inputMarker),
	))

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		_, _, readErr := conn.Read(readCtx)
		if readErr == nil {
			continue
		}
		require.Equal(websocket.StatusNormalClosure, websocket.CloseStatus(readErr))
		break
	}
	_, err = os.Stat(inputMarker)
	require.ErrorIs(err, os.ErrNotExist)
}

func TestWorkspaceRuntimeTargetsHideInternalShellTargetE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "tmux-wrapper")
	require.NoError(os.WriteFile(
		tmuxPath,
		[]byte("#!/bin/sh\nexit 0\n"),
		0o755,
	))
	cfg := &config.Config{Tmux: config.Tmux{
		Command: []string{tmuxPath, "--scope", "tmux"},
	}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	resp, err := fixture.client.HTTP.GetWorkspaceRuntimeWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.LaunchTargets)

	foundPlainShell := false
	for _, target := range resp.JSON200.LaunchTargets {
		if target.Key == string(localruntime.LaunchTargetShell) {
			require.Fail("internal shell target should not be exposed")
		}
		if target.Key == string(localruntime.LaunchTargetPlainShell) {
			foundPlainShell = true
			assert.True(target.Available)
		}
	}
	assert.True(foundPlainShell, "plain shell target should be exposed")
}

func TestWorkspaceRuntimeLaunchUnavailableTargetE2E(t *testing.T) {
	disabled := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "disabled",
		Label:   "Disabled",
		Enabled: &disabled,
	}}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	resp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "disabled",
		},
	)

	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode())
	require.Contains(t, string(resp.Body), "not available")
}

func TestWorkspaceRuntimeLaunchPlainShellCreatesRuntimeSessionE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	resp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "plain_shell",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	shell := resp.JSON200
	assert.Equal("plain_shell", shell.TargetKey)
	assert.Equal(string(localruntime.LaunchTargetPlainShell), shell.Kind)
	assert.Equal(string(localruntime.SessionStatusRunning), shell.Status)
	assert.Equal("terminal", shell.DisplayRegion)

	getResp, err := fixture.client.HTTP.GetWorkspaceRuntimeWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, getResp.StatusCode())
	require.NotNil(getResp.JSON200)
	require.NotNil(getResp.JSON200.Sessions)
	require.Len(getResp.JSON200.Sessions, 1)
	assert.Equal(shell.Key, getResp.JSON200.Sessions[0].Key)
	assert.Equal("terminal", getResp.JSON200.Sessions[0].DisplayRegion)
}

func TestWorkspaceRuntimeAttachSpecUsesStoredTmuxSessionE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tmuxPath := writeWorkspaceRuntimeTmuxProbe(t, "workspace-runtime-live", 0, "")
	cfg := &config.Config{Tmux: config.Tmux{
		Command: []string{tmuxPath, "--socket", "workspace"},
	}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	sessionKey := ws.Id + "_codex"
	require.NoError(fixture.database.UpsertWorkspaceRuntimeSession(
		ctx,
		&db.WorkspaceRuntimeSession{
			WorkspaceID: ws.Id,
			SessionKey:  sessionKey,
			TargetKey:   "codex",
			Label:       "codex",
			Kind:        "agent",
			Scope:       "session",
			TmuxSession: "workspace-runtime-live",
			CreatedAt:   time.Now().UTC(),
		},
	))

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/workspaces/"+ws.Id+"/runtime/sessions/"+
			sessionKey+"/attach-spec",
		nil,
	)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	fixture.server.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)
	var spec struct {
		Version           int      `json:"version"`
		Kind              string   `json:"kind"`
		SessionKey        string   `json:"session_key"`
		TargetKey         string   `json:"target_key"`
		TmuxSession       string   `json:"tmux_session"`
		Command           []string `json:"command"`
		RequiresLocalHost bool     `json:"requires_local_host"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&spec))
	assert.Equal(1, spec.Version)
	assert.Equal("tmux", spec.Kind)
	assert.Equal(sessionKey, spec.SessionKey)
	assert.Equal("codex", spec.TargetKey)
	assert.Equal("workspace-runtime-live", spec.TmuxSession)
	assert.Equal(
		[]string{
			"env", "-u", "TMUX", "-u", "TMUX_TMPDIR",
			tmuxPath, "--socket", "workspace",
			"-u", "attach-session", "-E", "-t", "workspace-runtime-live",
		},
		spec.Command,
	)
	assert.True(spec.RequiresLocalHost)
}

func TestWorkspaceCommitsFlagsUnpushedCommitsE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	require.NotEmpty(ws.WorktreePath)

	// Commit locally without pushing. A brand-new commit is unpushed no matter
	// how the worktree tracks its upstream, so the commits endpoint must flag
	// it - this proves the push status reaches the wire for a real workspace.
	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "ws@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Workspace")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "local-only.txt"),
		[]byte("local\n"), 0o644,
	))
	gitfixture.Run(t, ws.WorktreePath, "add", ".")
	gitfixture.Run(t, ws.WorktreePath, "commit", "-m", "local only commit")
	localSHA := gitfixture.SHA(t, ws.WorktreePath, "HEAD")

	resp, err := fixture.client.HTTP.GetWorkspaceCommitsWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Commits)
	require.NotEmpty(resp.JSON200.Commits)

	top := resp.JSON200.Commits[0]
	assert.Equal(localSHA, top.Sha, "newest commit should be the local-only commit")
	require.NotNil(top.Pushed, "workspace commits should carry push status")
	assert.False(*top.Pushed, "freshly committed local commit must be unpushed")
}

func TestWorkspaceCommitsOmitsPushStatusWithoutUpstreamE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	// A fork pull has a valid provider identity but its branch remains
	// untracked, so provider reconciliation cannot add a base-repository
	// upstream while this request runs.
	forkURL := "https://github.com/fork/widget.git"
	gitfixture.Run(t, fixture.bare, "config", "--add", "url."+fixture.remote+".insteadOf", forkURL)
	seedPRWithHeadRepo(t, fixture.database, "github.com", "acme", "widget", 2, forkURL)
	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx, generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 2,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode(), string(createResp.Body))
	require.NotNil(createResp.JSON202)
	ws := waitForWorkspaceReady(t, ctx, fixture.client, createResp.JSON202.Id)
	require.NotEmpty(ws.WorktreePath)

	resp, err := fixture.client.HTTP.GetWorkspaceCommitsWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Commits)
	require.NotEmpty(resp.JSON200.Commits)

	for _, c := range resp.JSON200.Commits {
		assert.Nil(c.Pushed,
			"push status must be omitted when the branch has no upstream")
	}
}

func writeWorkspaceRuntimeTmuxProbe(
	t *testing.T,
	expectedSession string,
	exitCode int,
	stderr string,
) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-tmux")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--socket\" ]; then shift 2; fi\n" +
		"if [ \"$1\" != \"has-session\" ]; then exit 0; fi\n" +
		"if [ \"$1\" != \"has-session\" ] || [ \"$2\" != \"-t\" ] || [ \"$3\" != \"" + expectedSession + "\" ]; then\n" +
		"  echo unexpected tmux argv: \"$@\" >&2\n" +
		"  exit 2\n" +
		"fi\n"
	if stderr != "" {
		body += "echo " + shellQuoteTest(stderr) + " >&2\n"
	}
	body += "exit " + strconv.Itoa(exitCode) + "\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	return script
}

func shellQuoteTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
