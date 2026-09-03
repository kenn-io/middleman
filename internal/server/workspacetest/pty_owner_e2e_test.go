package workspacetest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/ptyowner"
	"go.kenn.io/forge/internal/ptysize"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

const workspaceRuntimeHelperMarker = "kenn-forge-workspace-runtime-helper"

var rustPtyManagerBuild struct {
	once sync.Once
	path string
	out  []byte
	err  error
}

func TestWorkspaceCreatesRustPtyManagerSessionE2E(t *testing.T) {
	requirePTYAvailable(t)

	require := require.New(t)
	assert := assert.New(t)

	managerPath := buildRustPtyManagerForTest(t)
	ptyOwnerDir := longRustPtyOwnerDirForTest(t)
	setLongUnixTempDirForTest(t)
	cfg := &config.Config{
		Tmux: config.Tmux{
			Command: []string{filepath.Join(t.TempDir(), "missing-tmux")},
		},
		Shell: config.Shell{Command: rustPtyManagerShellCommandForTest()},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerManagerPath:                managerPath,
	})
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(workspace.TerminalBackendPtyOwner, stored.TerminalBackend)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	conn, _, err := workspaceTerminalDialWithQuery(
		ctx, ts.URL, ws.Id, "cols=120&rows=30",
	)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	if runtime.GOOS == "windows" {
		workspaceTerminalConnWriteRead(
			t, ctx, conn, "echo rust-owner-one\r", "rust-owner-one",
		)
	} else {
		workspaceTerminalConnWriteRead(
			t, ctx, conn, "printf 'rust-owner-one\n'\r", "rust-owner-one",
		)
		require.NoError(conn.Write(
			ctx,
			websocket.MessageText,
			[]byte(`{"type":"resize","cols":133,"rows":37}`),
		))
		workspaceTerminalConnWriteRead(
			t, ctx, conn,
			"size=$(stty size); printf 'size:%s\\n' \"$size\"\r",
			"size:37 133",
		)
	}

	require.NoError(conn.Close(websocket.StatusNormalClosure, "done"))
	deleteWorkspaceForPtyOwnerTest(t, ctx, fixture, ws.Id)

	_, err = os.Stat(filepath.Join(ptyOwnerDir, ws.TmuxSession))
	assert.True(os.IsNotExist(err))
}

func TestWorkspaceRuntimeLaunchesRustPtyManagerSessionE2E(t *testing.T) {
	requirePTYAvailable(t)

	require := require.New(t)
	assert := assert.New(t)

	managerPath := buildRustPtyManagerForTest(t)
	ptyOwnerDir := longRustPtyOwnerDirForTest(t)
	setLongUnixTempDirForTest(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		Agents: []config.Agent{{
			Key:     "helper",
			Label:   "Helper",
			Command: workspaceRuntimeHelperCommand("echo"),
		}},
		Tmux: config.Tmux{
			Command:       []string{filepath.Join(t.TempDir(), "missing-tmux")},
			AgentSessions: &disableTmuxAgentSessions,
		},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerManagerPath:                managerPath,
	})
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{TargetKey: "helper"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, session.Key)
	assert.Equal("helper", session.TargetKey)
	assert.Equal(string(localruntime.SessionStatusRunning), session.Status)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(t, ctx, conn, "ping\r", "echo:ping")

	stopResp, err := fixture.client.HTTP.StopWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id, session.Key,
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, stopResp.StatusCode())
	deleteWorkspaceForPtyOwnerTest(t, ctx, fixture, ws.Id)
}

func TestWorkspaceRuntimeResizeOwnerFollowsLatestDeliberateClientE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stty-based terminal size probe requires a Unix PTY")
	}
	requirePTYAvailable(t)
	require := require.New(t)

	managerPath := buildRustPtyManagerForTest(t)
	ptyOwnerDir := longRustPtyOwnerDirForTest(t)
	setLongUnixTempDirForTest(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		Agents: []config.Agent{{
			Key:     "shell-size",
			Label:   "Shell size",
			Command: []string{"/bin/sh"},
		}},
		Tmux: config.Tmux{
			Command:       []string{filepath.Join(t.TempDir(), "missing-tmux")},
			AgentSessions: &disableTmuxAgentSessions,
		},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerManagerPath:                managerPath,
	})
	ctx := t.Context()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{TargetKey: "shell-size"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, session.Key)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal"
	first, _, err := websocket.Dial(
		ctx, wsURL+"?cols=80&rows=24&resize_active=1", nil,
	)
	require.NoError(err)
	defer first.Close(websocket.StatusNormalClosure, "done")
	second, _, err := websocket.Dial(
		ctx, wsURL+"?cols=100&rows=30&resize_active=1", nil,
	)
	require.NoError(err)
	defer second.Close(websocket.StatusNormalClosure, "done")

	require.NoError(second.Write(
		ctx,
		websocket.MessageText,
		[]byte(`{"type":"claim_resize","cols":100,"rows":30}`),
	))
	workspaceTerminalConnWriteRead(
		t, ctx, second,
		"size=$(stty size); printf 'second-size:%s\\n' \"$size\"\r",
		"second-size:30 100",
	)

	require.NoError(first.Write(
		ctx,
		websocket.MessageText,
		[]byte(`{"type":"resize","cols":120,"rows":40}`),
	))
	workspaceTerminalConnWriteRead(
		t, ctx, second,
		"size=$(stty size); printf 'still-second:%s\\n' \"$size\"\r",
		"still-second:30 100",
	)

	require.NoError(first.Write(
		ctx,
		websocket.MessageText,
		[]byte(`{"type":"claim_resize","cols":120,"rows":40}`),
	))
	workspaceTerminalConnWriteRead(
		t, ctx, first,
		"size=$(stty size); printf 'first-size:%s\\n' \"$size\"\r",
		"first-size:40 120",
	)

	stopResp, err := fixture.client.HTTP.StopWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id, session.Key,
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, stopResp.StatusCode())
	deleteWorkspaceForPtyOwnerTest(t, ctx, fixture, ws.Id)
}

func TestWorkspaceRuntimeSessionTerminalWebSocketE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("echo"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode())
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(t, ctx, conn, "ping\n", "echo:ping")
}

func TestWorkspaceRuntimeSessionTerminalWebSocketBasePathE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		BasePath: "/kenn-forge/",
		Agents: []config.Agent{{
			Key:     "helper",
			Label:   "Helper",
			Command: workspaceRuntimeHelperCommand("echo"),
		}},
		Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode())
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/kenn-forge/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(t, ctx, conn, "ping\n", "echo:ping")
}

func TestWorkspaceRuntimeSessionTerminalSkipsAltScreenReplayE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("altscreen"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode())
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal"

	primingConn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	workspaceTerminalConnWriteRead(t, ctx, primingConn, "prime\n", "codex screen")
	require.NoError(primingConn.Close(websocket.StatusNormalClosure, "primed"))

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	type terminalRead struct {
		typ  websocket.MessageType
		data []byte
		err  error
	}
	reads := make(chan terminalRead, 1)
	readOnce := func() {
		go func() {
			typ, data, readErr := conn.Read(context.Background())
			reads <- terminalRead{typ: typ, data: data, err: readErr}
		}()
	}
	readOnce()
	select {
	case read := <-reads:
		require.NoError(read.err)
		require.Empty(
			string(read.data),
			"late attach must not replay stale alternate-screen output",
		)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(conn.Write(
		ctx, websocket.MessageBinary, []byte("paint\n"),
	))
	var got strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case read := <-reads:
			require.NoError(read.err)
			if read.typ == websocket.MessageBinary {
				got.WriteString(string(read.data))
			}
			if strings.Contains(got.String(), "live:paint") {
				break
			}
			readOnce()
			continue
		case <-deadline:
			require.Contains(got.String(), "live:paint")
		}
		break
	}
	assert.NotContains(got.String(), "codex screen")
	require.Contains(got.String(), "live:paint")
}

func TestWorkspaceRuntimeSessionTerminalAppliesInitialSizeE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	// This intentionally goes through the generated HTTP client, the real
	// httptest server, and the terminal websocket rather than attaching to
	// localruntime directly. The helper exits quickly after printing the
	// observed PTY size, so receiving size:41:177 exercises the full path
	// that must preserve final terminal output before the exit frame wins.
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("size"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode())
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key +
		"/terminal?cols=177&rows=41"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(t, ctx, conn, "size\n", "size:41:177")
}

func TestWorkspacePtyOwnerTitleMarksWorkspaceWorkingE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("workspace clone fixture uses Unix-style local remotes")
	}
	runParallelWorkspacePTYTest(t)
	requirePTYAvailable(t)

	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	cfg := &config.Config{
		Tmux:  config.Tmux{Command: []string{filepath.Join(dir, "missing-tmux")}},
		Shell: config.Shell{Command: []string{"/bin/sh"}},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)

	conn, _, err := workspaceTerminalDialWithQuery(ctx, ts.URL, ws.Id, "")
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	workspaceTerminalConnWriteRead(
		t, ctx, conn, "stty -echo\rprintf '%s\\n' $((40+2))\r", "42",
	)
	// The spinner glyph is sent as printf octal escapes (\342\240\264
	// is "⠴") so the typed line stays pure ASCII. Raw multibyte bytes
	// written to an interactive shell are interpreted by readline as
	// meta editing commands when the host has no UTF-8 locale set,
	// scrambling the command before it runs. The session still emits
	// the real multibyte title for the pty owner to track.
	workspaceTerminalConnWriteRead(
		t, ctx, conn,
		"printf 'title-sent\\n'; "+
			"printf '\\033]0;\\342\\240\\264 t3code-b5014b03\\007'\r",
		"t3code-b5014b03",
	)

	var got *generated.WorkspaceResponse
	require.Eventually(func() bool {
		resp, err := fixture.client.HTTP.GetWorkspaceWithResponse(ctx, ws.Id)
		if err != nil || resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return false
		}
		got = resp.JSON200
		return got.TmuxWorking &&
			got.TmuxActivitySource == workspaceapi.TmuxActivitySourceTitle &&
			got.TmuxPaneTitle != nil
	}, 6*time.Second, 50*time.Millisecond)
	require.NotNil(got)
	assert.True(got.TmuxWorking)
	assert.Equal(workspaceapi.TmuxActivitySourceTitle, got.TmuxActivitySource)
	require.NotNil(got.TmuxPaneTitle)
	assert.Equal("⠴ t3code-b5014b03", *got.TmuxPaneTitle)
}

func TestWorkspaceRuntimePlainShellUsesPtyOwnerWhenTmuxUnavailableE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses /bin/sh")
	}
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	cfg := &config.Config{
		Tmux:  config.Tmux{Command: []string{filepath.Join(dir, "missing-tmux")}},
		Shell: config.Shell{Command: []string{"/bin/sh"}},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: string(localruntime.LaunchTargetPlainShell),
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, session.Key)
	assert.Equal(string(localruntime.LaunchTargetPlainShell), session.TargetKey)
	assert.Equal(string(localruntime.SessionStatusRunning), session.Status)

	paths, err := ptyowner.NewSessionPaths(ptyOwnerDir, session.Key)
	require.NoError(err)
	_, err = os.Stat(paths.StatePath)
	require.NoError(err)

	storedTmux, err := fixture.database.ListWorkspaceRuntimeTmuxSessions(ctx, ws.Id)
	require.NoError(err)
	assert.Empty(storedTmux)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(
		t, ctx, conn, "printf 'pty-owner-shell\\n'\r", "pty-owner-shell",
	)
}

func TestRustPtyManagerRejectsConcurrentAttachmentsE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("concurrent attach coverage is exercised by the Rust owner tests on Windows")
	}
	requirePTYAvailable(t)
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)

	managerPath := buildRustPtyManagerForTest(t)
	ptyOwnerDir := longRustPtyOwnerDirForTest(t)
	session := "kenn-forge-rust-concurrent"
	command := []string{
		"sh", "-c",
		"printf ready; while IFS= read -r line; do echo got:$line; done",
	}
	client := ptyowner.Client{
		Root:        ptyOwnerDir,
		ManagerPath: managerPath,
		Command:     command,
	}
	require.NoError(client.Ensure(t.Context(), session, t.TempDir()))
	t.Cleanup(func() {
		_ = client.Stop(context.Background(), session)
	})

	first, err := client.Attach(
		context.Background(), session,
		ptysize.Geometry{Cols: 120, Rows: 30},
	)
	require.NoError(err)
	defer first.Close()
	require.Contains(
		readPtyOwnerOutputUntil(t, first.Output, "ready"),
		"ready",
	)
	require.NoError(first.Write([]byte("before-second\r")))
	require.Contains(
		readPtyOwnerOutputUntil(t, first.Output, "got:before-second"),
		"got:before-second",
	)

	second, err := client.Attach(
		context.Background(), session,
		ptysize.Geometry{Cols: 100, Rows: 20},
	)
	if second != nil {
		second.Close()
	}
	require.Error(err)
	assert.Contains(err.Error(), "already has an active attachment")

	first.Close()
	var third *ptyowner.Attachment
	require.Eventually(func() bool {
		var attachErr error
		third, attachErr = client.Attach(
			context.Background(), session,
			ptysize.Geometry{Cols: 80, Rows: 24},
		)
		return attachErr == nil
	}, 2*time.Second, 20*time.Millisecond)
	defer third.Close()
	require.NoError(third.Write([]byte("after-close\n")))
	require.Contains(
		readPtyOwnerOutputUntil(t, third.Output, "got:after-close"),
		"got:after-close",
	)
}

func TestWorkspacePtyOwnerTerminalRejectsConcurrentAttachmentsE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	cfg := &config.Config{
		Tmux:  config.Tmux{Command: []string{filepath.Join(dir, "missing-tmux")}},
		Shell: config.Shell{Command: []string{"/bin/sh"}},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)

	first, _, err := workspaceTerminalDialWithQuery(ctx, ts.URL, ws.Id, "")
	require.NoError(err)
	defer first.Close(websocket.StatusNormalClosure, "done")

	second, resp, err := workspaceTerminalDialWithQuery(ctx, ts.URL, ws.Id, "")
	require.Error(err)
	if second != nil {
		second.Close(websocket.StatusNormalClosure, "done")
	}
	require.NotNil(resp)
	assert.Equal(http.StatusConflict, resp.StatusCode)
	if resp.Body != nil {
		resp.Body.Close()
	}

	require.NoError(first.Close(websocket.StatusNormalClosure, "done"))
	third := workspaceTerminalDialEventually(t, ctx, ts.URL, ws.Id)
	defer third.Close(websocket.StatusNormalClosure, "done")
	workspaceTerminalConnWriteRead(
		t, ctx, third, "printf 'owner-after-close\n'\n", "owner-after-close",
	)
}

func TestWorkspacePtyOwnerTerminalFlushesFinalOutputOnExitE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)

	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	cfg := &config.Config{
		Tmux:  config.Tmux{Command: []string{filepath.Join(dir, "missing-tmux")}},
		Shell: config.Shell{Command: []string{"/bin/sh"}},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, ws.TmuxSession)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)

	conn, _, err := workspaceTerminalDialWithQuery(ctx, ts.URL, ws.Id, "")
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	require.NoError(conn.Write(
		ctx, websocket.MessageBinary,
		[]byte("printf 'final-owner-output\n'; exit\n"),
	))
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var got strings.Builder
	for {
		typ, data, readErr := conn.Read(readCtx)
		if readErr != nil {
			break
		}
		if typ == websocket.MessageBinary {
			got.WriteString(string(data))
		}
		if strings.Contains(got.String(), "final-owner-output") {
			return
		}
	}
	require.Contains(got.String(), "final-owner-output")
}

func TestWorkspaceRuntimeLaunchMultipleAndStopOneE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("echo"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	client := fixture.client
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	firstResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, firstResp.StatusCode())
	require.NotNil(firstResp.JSON200)
	first := firstResp.JSON200

	secondResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, secondResp.StatusCode())
	require.NotNil(secondResp.JSON200)
	second := secondResp.JSON200
	assert.NotEqual(first.Key, second.Key)
	assert.True(isRuntimeSessionKeyForWorkspace(ws.Id, first.Key))
	assert.True(isRuntimeSessionKeyForWorkspace(ws.Id, second.Key))
	assert.Equal("Helper", first.Label)
	assert.Equal("Helper 2", second.Label)
	assert.Equal(string(localruntime.SessionStatusRunning), first.Status)

	renameResp, err := client.HTTP.RenameWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id, second.Key,
		generated.RenameWorkspaceRuntimeSessionInputBody{Label: "Review helper"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, renameResp.StatusCode(), string(renameResp.Body))
	require.NotNil(renameResp.JSON200)
	assert.Equal(second.Key, renameResp.JSON200.Key)
	assert.Equal("Review helper", renameResp.JSON200.Label)

	listResp, err := client.HTTP.GetWorkspaceRuntimeWithResponse(
		ctx, ws.Id,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp.StatusCode())
	require.NotNil(listResp.JSON200)
	require.NotNil(listResp.JSON200.Sessions)
	require.Len(listResp.JSON200.Sessions, 2)
	assert.Equal(first.Key, listResp.JSON200.Sessions[0].Key)
	assert.Equal("Helper", listResp.JSON200.Sessions[0].Label)
	assert.Equal("Review helper", listResp.JSON200.Sessions[1].Label)

	stopResp, err := client.HTTP.StopWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id, first.Key,
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, stopResp.StatusCode())

	afterStopResp, err := client.HTTP.GetWorkspaceRuntimeWithResponse(
		ctx, ws.Id,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, afterStopResp.StatusCode())
	require.NotNil(afterStopResp.JSON200)
	require.NotNil(afterStopResp.JSON200.Sessions)
	require.Len(afterStopResp.JSON200.Sessions, 1)
	assert.Equal(second.Key, afterStopResp.JSON200.Sessions[0].Key)
}

func TestWorkspaceRuntimeNaturalAgentExitRemovesSessionE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("exit"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	client := fixture.client
	database := fixture.database
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	launchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)

	require.Eventually(func() bool {
		runtimeResp, runtimeErr := client.HTTP.GetWorkspaceRuntimeWithResponse(
			ctx, ws.Id,
		)
		if runtimeErr != nil ||
			runtimeResp.StatusCode() != http.StatusOK ||
			runtimeResp.JSON200 == nil ||
			runtimeResp.JSON200.Sessions == nil {
			return false
		}
		return len(runtimeResp.JSON200.Sessions) == 0
	}, 2*time.Second, 20*time.Millisecond)
	stored, err := database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
	require.NoError(err)
	assert.Empty(stored)
	assert.NotEmpty(launchResp.JSON200.Key)
}

func TestWorkspaceRuntimePtyOwnerQuickExitLaunchSucceedsE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)
	disableTmuxAgentSessions := false
	cfg := &config.Config{Agents: []config.Agent{{
		Key:     "helper",
		Label:   "Helper",
		Command: workspaceRuntimeHelperCommand("print-exit"),
	}}, Tmux: config.Tmux{AgentSessions: &disableTmuxAgentSessions}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	client := fixture.client
	database := fixture.database
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	launchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	assert.NotEmpty(launchResp.JSON200.Key)

	require.Eventually(func() bool {
		runtimeResp, runtimeErr := client.HTTP.GetWorkspaceRuntimeWithResponse(
			ctx, ws.Id,
		)
		if runtimeErr != nil ||
			runtimeResp.StatusCode() != http.StatusOK ||
			runtimeResp.JSON200 == nil ||
			runtimeResp.JSON200.Sessions == nil {
			return false
		}
		return len(runtimeResp.JSON200.Sessions) == 0
	}, 2*time.Second, 20*time.Millisecond)
	stored, err := database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
	require.NoError(err)
	assert.Empty(stored)
}

// TestWorkspaceRuntimePlainShellTerminalWebSocketE2E exercises the runtime
// session websocket path end-to-end with a custom Shell.Command. Hardened
// deployments (e.g. systemd services with SystemCallFilter=~@privileged) need
// the override so that zsh's startup setresuid is not SIGSYS'd by the parent's
// seccomp filter; this test guards both the websocket route and the
// config.Shell.Command -> manager.Options.ShellCommand wiring.
func TestWorkspaceRuntimePlainShellTerminalWebSocketE2E(t *testing.T) {
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	cfg := &config.Config{
		Shell: config.Shell{
			Command: workspaceRuntimeHelperCommand("echo"),
		},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerInProcess:                  true,
	})
	client := fixture.client
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	launchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: string(localruntime.LaunchTargetPlainShell),
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	shell := launchResp.JSON200

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + shell.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	workspaceTerminalConnWriteRead(t, ctx, conn, "ping\n", "echo:ping")
}

// TestWorkspaceRuntimePlainShellTerminalDeliversActualExitCodeE2E pins the
// websocket "exited" text frame contract through the real runtime stack. The
// helper closes its PTY shortly before exiting so this exercises the window
// where PTY EOF can arrive before process wait publishes the exit code.
func TestWorkspaceRuntimePlainShellTerminalDeliversActualExitCodeE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses /bin/sh")
	}
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	cfg := &config.Config{
		Tmux: config.Tmux{
			Command: []string{filepath.Join(dir, "missing-tmux")},
		},
		Shell: config.Shell{
			Command: workspaceRuntimeHelperCommand("pty-close-on-input-then-exit"),
		},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	client := fixture.client
	srv := fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	launchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: string(localruntime.LaunchTargetPlainShell),
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	shell := launchResp.JSON200
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, shell.Key)
	stored, err := fixture.database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
	require.NoError(err)
	require.Len(stored, 1)
	require.Equal(shell.Key, stored[0].SessionKey)
	require.Empty(stored[0].TmuxSession)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + shell.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Trigger after attach so the session cannot exit before the websocket
	// connects. The helper's short delay makes PTY EOF reliably precede process
	// exit while remaining inside the owner's bounded exit-code grace period.
	require.NoError(conn.Write(ctx, websocket.MessageBinary, []byte("close\n")))
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		typ, data, readErr := conn.Read(readCtx)
		if readErr != nil {
			require.Failf(
				"never received exit frame",
				"read err before exit frame: %v", readErr,
			)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		}
		require.NoError(json.Unmarshal(data, &msg))
		require.Equal("exited", msg.Type)
		require.Equal(7, msg.Code)
		break
	}

	require.Eventually(func() bool {
		runtimeResp, runtimeErr := client.HTTP.GetWorkspaceRuntimeWithResponse(
			ctx, ws.Id,
		)
		if runtimeErr != nil || runtimeResp.StatusCode() != http.StatusOK ||
			runtimeResp.JSON200 == nil {
			return false
		}
		if runtimeResp.JSON200.Sessions != nil {
			for _, session := range runtimeResp.JSON200.Sessions {
				if session.Key == shell.Key {
					return false
				}
			}
		}
		rows, rowsErr := fixture.database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
		return rowsErr == nil && len(rows) == 0
	}, 5*time.Second, 20*time.Millisecond)
}

func TestWorkspaceRuntimePtyOwnerQuickExitReportsExactStatusE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses Unix PTY exit semantics")
	}
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	dir := t.TempDir()
	ptyOwnerDir := filepath.Join(dir, "pty-owner")
	disableTmuxAgentSessions := false
	cfg := &config.Config{
		Agents: []config.Agent{{
			Key:   "helper",
			Label: "Helper",
			// Keep the PTY open briefly after the shell exits so the owner can
			// publish the exact status before output EOF reaches the bridge.
			// Without this ordering, a loaded race runner can legitimately hit
			// the owner's bounded unknown-status fallback instead.
			Command: []string{
				"sh", "-c", "IFS= read -r line; sleep 1 & exit 9",
			},
		}},
		Tmux: config.Tmux{
			Command:       []string{filepath.Join(dir, "missing-tmux")},
			AgentSessions: &disableTmuxAgentSessions,
		},
	}
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, fixture.client)

	launchResp, err := fixture.client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{TargetKey: "helper"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode(), string(launchResp.Body))
	require.NotNil(launchResp.JSON200)
	session := launchResp.JSON200
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, session.Key)

	stored, err := fixture.database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
	require.NoError(err)
	require.Len(stored, 1)
	require.Equal(session.Key, stored[0].SessionKey)

	ts := httptest.NewServer(fixture.server)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + session.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	require.NoError(conn.Write(ctx, websocket.MessageBinary, []byte("exit\n")))

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		typ, data, readErr := conn.Read(readCtx)
		if readErr != nil {
			require.Failf(
				"never received exact exit frame",
				"read err before exit frame: %v", readErr,
			)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		}
		require.NoError(json.Unmarshal(data, &msg))
		require.Equal("exited", msg.Type)
		require.Equal(9, msg.Code)
		break
	}

	require.Eventually(func() bool {
		runtimeResp, runtimeErr := fixture.client.HTTP.GetWorkspaceRuntimeWithResponse(
			ctx, ws.Id,
		)
		if runtimeErr != nil || runtimeResp.StatusCode() != http.StatusOK ||
			runtimeResp.JSON200 == nil || runtimeResp.JSON200.Sessions == nil ||
			len(runtimeResp.JSON200.Sessions) != 0 {
			return false
		}
		stored, storedErr := fixture.database.ListWorkspaceRuntimeSessions(ctx, ws.Id)
		return storedErr == nil && len(stored) == 0
	}, 2*time.Second, 20*time.Millisecond)
}

// TestWorkspaceRuntimePlainShellAfterExitStartsFreshE2E pins the user-visible
// no-tmux behavior that launching a shell after the terminal has reported exit
// returns a fresh ptyowner-backed session, never the dead shell record the
// frontend just auto-closed.
func TestWorkspaceRuntimePlainShellAfterExitStartsFreshE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses /bin/sh")
	}
	runParallelWorkspacePTYTest(t)

	require := require.New(t)
	assert := assert.New(t)
	cfg := &config.Config{
		Tmux: config.Tmux{
			Command: []string{filepath.Join(t.TempDir(), "missing-tmux")},
		},
		Shell: config.Shell{
			Command: workspaceRuntimeHelperCommand("pty-close-on-input-then-sleep"),
		},
	}
	ptyOwnerDir := filepath.Join(t.TempDir(), "pty-owner")
	fixture := setupWorkspaceServerFixture(t, cfg, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		HostCheckAllowLoopbackAnyPort:      true,
		PtyOwnerDir:                        ptyOwnerDir,
		PtyOwnerInProcess:                  true,
	})
	client := fixture.client
	srv := fixture.server
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	firstLaunchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: string(localruntime.LaunchTargetPlainShell),
		},
	)
	require.NoError(err)
	require.Equal(
		http.StatusOK, firstLaunchResp.StatusCode(), string(firstLaunchResp.Body),
	)
	require.NotNil(firstLaunchResp.JSON200)
	first := firstLaunchResp.JSON200
	firstCreatedAt := first.CreatedAt
	cleanupPtyOwnerWorkspace(t, ptyOwnerDir, first.Key)

	// Attach + drain to drive the helper through exit, then ensure
	// the next shell open does not reuse the session that just
	// reported an exit frame to the frontend.
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/v1/workspaces/" + ws.Id +
		"/runtime/sessions/" + first.Key + "/terminal?cols=80&rows=24"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(err)
	require.NoError(conn.Write(ctx, websocket.MessageBinary, []byte("exit\n")))
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		typ, data, readErr := conn.Read(readCtx)
		if readErr != nil {
			require.Failf(
				"never received exit frame",
				"read err before exit frame: %v", readErr,
			)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type string `json:"type"`
		}
		require.NoError(json.Unmarshal(data, &msg))
		require.Equal("exited", msg.Type)
		break
	}
	conn.Close(websocket.StatusNormalClosure, "done")

	// Inside the zombie window: helper still sleeping, so cmd.Wait
	// hasn't returned and watchSession hasn't run.
	secondLaunchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: string(localruntime.LaunchTargetPlainShell),
		},
	)
	require.NoError(err)
	require.Equal(
		http.StatusOK, secondLaunchResp.StatusCode(), string(secondLaunchResp.Body),
	)
	require.NotNil(secondLaunchResp.JSON200)
	second := secondLaunchResp.JSON200
	assert.NotEqual(
		firstCreatedAt, second.CreatedAt,
		"second shell launch must return a fresh session, not the zombie",
	)
	assert.Equal(string(localruntime.SessionStatusRunning), second.Status)
}

func readPtyOwnerOutputUntil(
	t *testing.T,
	output <-chan []byte,
	needle string,
) string {
	t.Helper()

	deadline := time.After(2 * time.Second)
	var builder strings.Builder
	for {
		select {
		case chunk, ok := <-output:
			if !ok {
				return builder.String()
			}
			builder.Write(chunk)
			if strings.Contains(builder.String(), needle) {
				return builder.String()
			}
		case <-deadline:
			require.New(t).Failf(
				"timed out waiting for output",
				"wanted %q in %q", needle, builder.String(),
			)
		}
	}
}

func isRuntimeSessionKeyForWorkspace(workspaceID string, key string) bool {
	suffix := strings.TrimPrefix(key, workspaceID+"_")
	return suffix != key &&
		len(suffix) == 16 &&
		strings.IndexFunc(suffix, func(r rune) bool {
			return !strings.ContainsRune("0123456789abcdef", r)
		}) == -1
}

func deleteWorkspaceForPtyOwnerTest(
	t *testing.T,
	ctx context.Context,
	fixture workspaceServerFixture,
	workspaceID string,
) {
	t.Helper()

	force := true
	resp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, workspaceID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode())
}

func requirePTYAvailable(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable in this test environment: %v", err)
	}
	_ = ptmx.Close()
	_ = tty.Close()
}

func rustPtyManagerShellCommandForTest() []string {
	if runtime.GOOS == "windows" {
		return workspaceRuntimeHelperCommand("echo")
	}
	return []string{"/bin/sh"}
}

func buildRustPtyManagerForTest(t *testing.T) string {
	t.Helper()

	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo not available")
	}
	if err := procutil.Command(cargo, "--version").Run(); err != nil {
		t.Skipf("cargo not usable: %v", err)
	}
	rustPtyManagerBuild.once.Do(func() {
		root := repoRootForPtyOwnerTest(t)
		cmd := procutil.Command(cargo, "build", "-p", "kenn-forge-pty-manager")
		cmd.Dir = root
		rustPtyManagerBuild.out, rustPtyManagerBuild.err = cmd.CombinedOutput()
		rustPtyManagerBuild.path = filepath.Join(
			root, "target", "debug", "kenn-forge-pty-manager",
		)
		if runtime.GOOS == "windows" {
			rustPtyManagerBuild.path += ".exe"
		}
	})
	require.NoError(t, rustPtyManagerBuild.err, string(rustPtyManagerBuild.out))
	return rustPtyManagerBuild.path
}

func repoRootForPtyOwnerTest(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "Cargo.toml"))
	require.NoError(t, err)
	return root
}

func longRustPtyOwnerDirForTest(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), strings.Repeat("long-owner-root-", 8))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

func setLongUnixTempDirForTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	dir := filepath.Join(t.TempDir(), strings.Repeat("long-temp-root-", 8))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	t.Setenv("TMPDIR", dir)
}

func cleanupPtyOwnerWorkspace(
	t *testing.T,
	ptyOwnerDir string,
	session string,
) {
	t.Helper()
	t.Cleanup(func() {
		_ = (&ptyowner.Client{Root: ptyOwnerDir}).Stop(
			context.Background(), session,
		)
	})
}

func workspaceTerminalConnWriteRead(
	t *testing.T,
	ctx context.Context,
	conn *websocket.Conn,
	input string,
	needle string,
) {
	t.Helper()

	require.NoError(t, conn.Write(
		ctx, websocket.MessageBinary, []byte(input),
	))
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var got strings.Builder
	for {
		typ, data, readErr := conn.Read(readCtx)
		if readErr != nil {
			break
		}
		if typ != websocket.MessageBinary {
			continue
		}
		got.WriteString(string(data))
		if strings.Contains(got.String(), needle) {
			return
		}
	}
	require.Contains(t, got.String(), needle)
}

func workspaceTerminalDialWithQuery(
	ctx context.Context,
	serverURL string,
	workspaceID string,
	query string,
) (*websocket.Conn, *http.Response, error) {
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") +
		"/api/v1/workspaces/" + workspaceID + "/terminal"
	if query != "" {
		wsURL += "?" + query
	}
	return websocket.Dial(ctx, wsURL, nil)
}

func workspaceTerminalDialEventually(
	t *testing.T,
	ctx context.Context,
	serverURL string,
	workspaceID string,
) *websocket.Conn {
	t.Helper()

	var conn *websocket.Conn
	require.Eventually(t, func() bool {
		var resp *http.Response
		var err error
		conn, resp, err = workspaceTerminalDialWithQuery(
			ctx, serverURL, workspaceID, "",
		)
		if err != nil && conn != nil {
			conn.Close(websocket.StatusNormalClosure, "done")
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)
	return conn
}

func workspaceRuntimeHelperCommand(mode string) []string {
	return []string{
		os.Args[0],
		"-test.run=TestWorkspaceRuntimeHelperProcess",
		"--",
		workspaceRuntimeHelperMarker,
		mode,
	}
}

func TestWorkspaceRuntimeHelperProcess(t *testing.T) {
	args := os.Args
	sep := slices.Index(args, "--")
	if sep < 0 || len(args) <= sep+2 || args[sep+1] != workspaceRuntimeHelperMarker {
		return
	}
	switch args[sep+2] {
	case "echo":
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err == nil {
			fmt.Print("echo:" + line)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "altscreen":
		reader := bufio.NewReader(os.Stdin)
		_, err := reader.ReadString('\n')
		if err != nil {
			for {
				time.Sleep(time.Hour)
			}
		}
		fmt.Print("\x1b[?1049h\x1b[Hcodex screen")
		line, err := reader.ReadString('\n')
		if err == nil {
			fmt.Print("\x1b[Hlive:" + line)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "size":
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err == nil {
			rows, cols, sizeErr := pty.Getsize(os.Stdin)
			if sizeErr == nil {
				fmt.Printf("size:%d:%d:%s", rows, cols, line)
			}
		}
		return
	case "exit":
		os.Exit(3)
	case "print-exit":
		fmt.Print("quick-api-output")
		os.Exit(7)
	case "pty-close-on-input-then-sleep":
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			os.Exit(2)
		}
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
		_ = os.Stdin.Close()
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		time.Sleep(2 * time.Second)
		os.Exit(7)
	case "pty-close-on-input-then-exit":
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			os.Exit(2)
		}
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
		_ = os.Stdin.Close()
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		time.Sleep(50 * time.Millisecond)
		os.Exit(7)
	default:
		require.Failf(t, "unknown helper mode", "mode %q", args[sep+2])
	}
}
