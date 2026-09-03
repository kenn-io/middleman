package localruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/creack/pty/v2"
	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/ptyowner"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/ptysize"
	"go.kenn.io/forge/internal/testutil/processjob"
	"go.kenn.io/forge/internal/testutil/testsignal"
	"go.kenn.io/forge/internal/testutil/testtmux"
)

var privateTmuxOwner *testtmux.Owner

func TestMain(m *testing.M) {
	if code, ok := testtmux.CommandWrapperExitCode(); ok {
		os.Exit(code)
	}
	if os.Getenv("KENN_FORGE_LOCALRUNTIME_HELPER") == "1" {
		os.Exit(m.Run())
	}
	if err := processjob.ContainCurrentProcessTree(); err != nil {
		fmt.Fprintf(os.Stderr, "contain local runtime test process tree: %v\n", err)
		os.Exit(1)
	}
	if testtmux.Supported() {
		var ownerErr error
		privateTmuxOwner, ownerErr = testtmux.New()
		if ownerErr != nil {
			fmt.Fprintf(os.Stderr, "initialize private test tmux owner: %v\n", ownerErr)
			os.Exit(1)
		}
	}
	envDir, err := os.MkdirTemp("", "kenn-forge-localruntime-tmux-env-*")
	if err == nil {
		_ = os.Setenv("KENN_FORGE_TMUX_ENV_DIR", envDir)
	}
	runCleanup, stopSignalCleanup := testsignal.Install(
		func() error {
			if privateTmuxOwner == nil {
				return nil
			}
			return privateTmuxOwner.Cleanup()
		},
		func(cleanupErr error) {
			fmt.Fprintf(os.Stderr, "cleanup private test tmux servers: %v\n", cleanupErr)
		},
	)
	code := m.Run()
	if cleanupErr := runCleanup(); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "cleanup private test tmux servers: %v\n", cleanupErr)
		if code == 0 {
			code = 1
		}
	}
	stopSignalCleanup()
	if err == nil {
		_ = os.RemoveAll(envDir)
	}
	os.Exit(code)
}

func requirePTYAvailable(t *testing.T) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable in this test environment: %v", err)
	}
	_ = ptmx.Close()
	_ = tty.Close()
}

func withTestPtyOwnerRuntime(t *testing.T, options Options) Options {
	t.Helper()
	if options.PtyOwnerRuntime != nil {
		return options
	}
	options.PtyOwnerRuntime = ptyownerruntime.New(&ptyowner.Client{
		Root:      filepath.Join(t.TempDir(), "pty-owner"),
		InProcess: true,
	}, nil)
	return options
}

func TestManagerLaunchesIndependentSessionsPerWorkspaceTarget(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}}))
	t.Cleanup(mgr.Shutdown)

	session1, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(t, err)
	session2, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(t, err)

	sessions := mgr.ListSessions("ws-1")
	assert := assert.New(t)
	assert.NotEqual(session1.Key, session2.Key)
	assert.Equal("helper", session1.Label)
	assert.Equal("helper 2", session2.Label)
	assert.Equal(SessionStatusRunning, session1.Status)
	assert.Len(sessions, 2)
}

func TestManagerRenamesSessionMetadata(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}}))
	t.Cleanup(mgr.Shutdown)

	session, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(t, err)

	renamed, err := mgr.RenameSession("ws-1", session.Key, "Review helper")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(session.Key, renamed.Key)
	assert.Equal("Review helper", renamed.Label)
	sessions := mgr.ListSessions("ws-1")
	require.Len(t, sessions, 1)
	assert.Equal("Review helper", sessions[0].Label)
}

func TestManagerLaunchConcurrentStartsIndependentProcesses(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.Background()
	record := filepath.Join(t.TempDir(), "starts")
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		{
			Key: "helper", Label: "helper", Kind: LaunchTargetAgent,
			Source: "config", Command: helperRecordCommand(record),
			Available: true,
		},
	}}))
	t.Cleanup(mgr.Shutdown)

	const launches = 12
	var wg sync.WaitGroup
	errs := make(chan error, launches)
	infos := make(chan SessionInfo, launches)
	cwd := t.TempDir()
	for range launches {
		wg.Go(func() {
			info, err := mgr.Launch(ctx, "ws-1", cwd, "helper")
			errs <- err
			infos <- info
		})
	}
	wg.Wait()
	close(errs)
	close(infos)

	for err := range errs {
		require.NoError(err)
	}
	keys := make(map[string]bool)
	labels := make(map[string]bool)
	for info := range infos {
		keys[info.Key] = true
		labels[info.Label] = true
	}
	assert.Len(keys, launches)
	assert.Len(labels, launches)
	assert.True(labels["helper"])
	require.Eventually(func() bool {
		data, err := os.ReadFile(record)
		if err != nil {
			return false
		}
		return strings.Count(string(data), "\n") == launches
	}, 2*time.Second, 20*time.Millisecond)
	assert.Len(mgr.ListSessions("ws-1"), launches)
}

func TestNewSessionKeyUsesWorkspacePrefixAndRandomSuffix(t *testing.T) {
	first, err := NewSessionKey("ws-1")
	require.NoError(t, err)
	second, err := NewSessionKey("ws-1")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.True(strings.HasPrefix(first, "ws-1_"))
	assert.True(strings.HasPrefix(second, "ws-1_"))
	assert.NotEqual(first, second)
	assert.Len(strings.TrimPrefix(first, "ws-1_"), 16)
}

func TestSessionInfoSameGenerationRequiresKeyAndCreatedAt(t *testing.T) {
	createdAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	session := SessionInfo{Key: "surface:console", CreatedAt: createdAt}

	assert.True(t, session.SameGeneration(SessionInfo{
		Key:       session.Key,
		CreatedAt: createdAt,
	}))
	assert.False(t, session.SameGeneration(SessionInfo{
		Key:       session.Key,
		CreatedAt: createdAt.Add(time.Second),
	}))
	assert.False(t, session.SameGeneration(SessionInfo{
		Key:       "surface:replacement",
		CreatedAt: createdAt,
	}))
}

func TestManagerLaunchUnavailableTarget(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(Options{Targets: []LaunchTarget{{
		Key: "missing", Label: "Missing", Kind: LaunchTargetAgent,
		Available: false, DisabledReason: "not found",
	}}})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not available")
}

func TestManagerLaunchMissingTarget(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(Options{})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "target not found")
}

func TestManagerUpdateTargetsAffectsFutureLaunches(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	assert := assert.New(t)

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}}))
	t.Cleanup(mgr.Shutdown)

	mgr.UpdateTargets([]LaunchTarget{{
		Key: "custom", Label: "Custom", Kind: LaunchTargetAgent,
		Source: "config", Command: helperCommand("sleep"),
		Available: true,
	}})

	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.Error(t, err)
	assert.Contains(err.Error(), "target not found")

	session, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "custom")
	require.NoError(t, err)
	assert.Equal("custom", session.TargetKey)
	assert.Equal("Custom", session.Label)
}

func TestManagerTmuxSessionsReturnsWrappedAgentSessions(t *testing.T) {
	assert := assert.New(t)
	mgr := NewManager(Options{})
	mgr.sessions["ws-1:codex"] = &session{
		info: SessionInfo{
			Key:         "ws-1:codex",
			WorkspaceID: "ws-1",
			TargetKey:   "codex",
			Kind:        LaunchTargetAgent,
		},
		tmuxSession: "kenn-forge-ws-1-codex",
	}
	mgr.sessions["ws-1:other"] = &session{
		info: SessionInfo{
			Key:         "ws-1:other",
			WorkspaceID: "ws-1",
			TargetKey:   "other",
			Kind:        LaunchTargetAgent,
		},
	}
	mgr.sessions["ws-2:codex"] = &session{
		info: SessionInfo{
			Key:         "ws-2:codex",
			WorkspaceID: "ws-2",
			TargetKey:   "codex",
			Kind:        LaunchTargetAgent,
		},
		tmuxSession: "kenn-forge-ws-2-codex",
	}

	assert.Equal(
		[]string{"kenn-forge-ws-1-codex"},
		mgr.TmuxSessions("ws-1"),
	)
}

func TestStartTmuxAttachSessionKeepsBackingTmuxSession(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	info := SessionInfo{
		Key:         "session-1",
		WorkspaceID: "ws-1",
		TargetKey:   "codex",
		Kind:        LaunchTargetAgent,
		TmuxSession: "kenn-forge-ws-1-codex",
	}
	s, err := startTmuxAttachSession(info, helperCommand("sleep"), t.TempDir(), nil)
	require.NoError(err)
	go s.watch()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(s.stop(ctx))
		waitSessionDone(s)
	})

	assert.Equal("kenn-forge-ws-1-codex", s.tmuxSession)
	assert.Equal("kenn-forge-ws-1-codex", s.snapshot().TmuxSession)
}

func TestManagerLaunchCommandWrapsAgentsInTmuxWhenEnabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("XDG_RUNTIME_DIR", "argv-visible-value")
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
printf '%%s\0' "$#" "$@" >> %s
if [ "$1" = "has-session" ]; then
  echo "can't find session: $3" >&2
  exit 1
fi
exit 0
`, shellquote.Join(record)), 0o755))
	agent := helperTarget("codex", "sleep")
	agent.Label = "Codex"
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{tmuxPath},
				Available: true,
			},
		},
		TmuxCommand:             []string{tmuxPath},
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	launch, err := mgr.launchCommand(
		context.Background(), agent, "ws:alpha", "/tmp/work tree",
	)
	require.NoError(err)
	sessionName := tmuxSessionName("ws:alpha", "codex")

	assert.Equal(
		[]string{tmuxPath, "-u", "attach-session", "-E", "-t", sessionName},
		launch.Command,
	)
	assert.Equal(sessionName, launch.TmuxSession)
	records := readNullArgvRecord(t, record)
	assert.Contains(records, []string{"has-session", "-t", sessionName})
	var newSession []string
	for _, record := range records {
		if len(record) > 0 && record[0] == "new-session" {
			newSession = record
			break
		}
	}
	require.NotEmpty(newSession)
	newSessionText := strings.Join(newSession, "\n")
	scriptText := requireNewSessionPaneScript(t, newSession)
	assert.Contains(newSession, "-E")
	assert.NotContains(newSession, "-e")
	assert.Contains(newSession, "-c")
	assert.Contains(newSession, "/tmp/work tree")
	assert.Contains(scriptText, "__kenn_forge_env_file=")
	assert.Contains(scriptText, "exec env -i")
	assert.Contains(scriptText, `XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR-}"`)
	assert.Contains(scriptText, shellquote.Join(agent.Command[0]))
	assert.NotContains(newSessionText, "argv-visible-value")
}

func TestManagerLaunchCommandResolvesTmuxBeforeEmbeddingScript(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(os.MkdirAll(binDir, 0o755))
	tmuxPath := filepath.Join(binDir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte(`#!/bin/sh
if [ "$1" = "has-session" ]; then
  echo "can't find session: $3" >&2
  exit 1
fi
exit 0
`), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{"tmux"},
				Available: true,
			},
		},
		TmuxCommand:             []string{"tmux"},
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	launch, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())
	require.NoError(err)

	assert.Equal(tmuxPath, launch.Command[0])
	assert.Equal("-u", launch.Command[1])
	assert.Equal("attach-session", launch.Command[2])
}

func TestManagerLaunchCommandRejectsRelativeTmuxCommandWhenWrapped(t *testing.T) {
	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{"./tmux"},
				Available: true,
			},
		},
		TmuxCommand:             []string{"./tmux"},
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())

	require.Error(t, err)
	require.Contains(t, err.Error(), "resolve tmux command")
	require.Contains(t, err.Error(), "relative paths")
}

func TestManagerReattachTmuxClientsReplacesDedicatedTmuxSessions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte(`#!/bin/sh
if [ "$1" = "-L" ]; then shift 2; fi
if [ "$1" = "-u" ]; then shift; fi
if [ "$1" = "attach-session" ]; then
  trap 'exit 0' HUP INT TERM
  while :; do sleep 1; done
fi
exit 0
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mgr := NewManager(Options{
		TmuxCommand:  config.DefaultTmuxCommand(),
		TmuxGraphics: true,
	})
	t.Cleanup(mgr.Shutdown)
	restored := RestoredRuntimeSession{
		WorkspaceID: "ws-1",
		SessionKey:  "ws-1_shell-restored",
		TmuxSession: "kenn-forge-ws-1-shell",
		TargetKey:   string(LaunchTargetPlainShell),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(mgr.RestoreRuntimeSessions(t.Context(), []RestoredRuntimeSession{restored}))
	previous := mgr.sessions[restored.SessionKey]
	direct := &session{info: SessionInfo{
		WorkspaceID: "ws-1",
		Key:         "direct",
		Status:      SessionStatusRunning,
	}}
	mgr.sessions[direct.info.Key] = direct

	require.NoError(mgr.ReattachTmuxClients(t.Context()))

	replacement := mgr.sessions[restored.SessionKey]
	require.NotNil(replacement)
	assert.NotSame(previous, replacement)
	assert.True(previous.lifecycleClosed)
	assert.True(previous.recoverableDetach)
	assert.Same(direct, mgr.sessions[direct.info.Key])
	attachment, err := mgr.AttachSession(restored.WorkspaceID, restored.SessionKey)
	require.NoError(err)
	attachment.Close()
}

func TestManagerReattachTmuxClientsLeavesCustomTmuxSessionsAttached(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := NewManager(Options{
		TmuxCommand:  []string{"/usr/bin/tmux"},
		TmuxGraphics: true,
	})
	tmuxSession := &session{
		info: SessionInfo{
			WorkspaceID: "ws-1",
			Key:         "tmux",
			Status:      SessionStatusRunning,
		},
		tmuxSession: "forge-ws-1-test",
		lifecycle:   tmuxAttachLifecycle{},
	}
	mgr.sessions["tmux"] = tmuxSession

	require.NoError(mgr.ReattachTmuxClients(t.Context()))

	assert.Same(tmuxSession, mgr.sessions["tmux"])
	assert.False(tmuxSession.lifecycleClosed)
	assert.False(tmuxSession.recoverableDetach)
}

func TestManagerLaunchCommandMarksWrappedAgentTmuxSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
printf '%%s\0' "$#" "$@" >> %s
if [ "$1" = "has-session" ] || [ "$3" = "has-session" ]; then
  echo "can't find session: $3" >&2
  exit 1
fi
exit 0
`, shellquote.Join(record)), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: config.DefaultTmuxCommand(),
				Available: true,
			},
		},
		TmuxOwnerMarker:         "kenn-forge:test-owner",
		WrapAgentSessionsInTmux: true,
		TmuxGraphics:            true,
	})
	t.Cleanup(mgr.Shutdown)

	launch, err := mgr.launchCommand(context.Background(), agent, "ws-1", "/tmp/work tree")
	require.NoError(err)
	sessionName := tmuxSessionName("ws-1", "codex")

	assert.Equal(
		[]string{tmuxPath, "-L", "kenn-forge", "-u", "attach-session", "-E", "-t", sessionName},
		launch.Command,
	)
	assert.Equal(sessionName, launch.TmuxSession)
	records := readNullArgvRecord(t, record)
	require.Len(records, 5)
	newSession := records[1]
	assert.Contains(newSession, ";")
	assert.Contains(newSession, "set-option")
	assert.Contains(newSession, "-t")
	assert.Contains(newSession, sessionName)
	assert.Contains(newSession, "@forge_owner")
	assert.Contains(newSession, "kenn-forge:test-owner")
	assert.Equal([]string{
		"-L", "kenn-forge", "set-option", "-q", "-g", "allow-passthrough", "on",
	}, records[2])
	assert.Equal([]string{
		"-L", "kenn-forge", "set-option", "-q", "-s", "terminal-features[100]",
		"xterm-256color:sixel",
	}, records[3])
	assert.Equal([]string{
		"-L", "kenn-forge", "set-option", "-q", "-g", "mouse", "off",
	}, records[4])
}

func TestManagerLaunchPlainShellWrapsInTmuxWhenAvailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("XDG_RUNTIME_DIR", "argv-visible-value")
	t.Setenv("KENN_FORGE_TEST_CUSTOM_SHELL_ENV", "custom-visible-value")
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
printf '%%s\0' "$#" "$@" >> %s
if [ "$1" = "-u" ]; then shift; fi
if [ "$1" = "attach-session" ]; then
  trap 'exit 0' HUP INT TERM
  while :; do sleep 1; done
fi
if [ "$1" = "has-session" ]; then
  echo "can't find session: $3" >&2
  exit 1
fi
exit 0
`, shellquote.Join(record)), 0o755))
	mgr := NewManager(Options{
		Targets: []LaunchTarget{{
			Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
			Source: "system", Command: []string{tmuxPath},
			Available: true,
		}, plainShellTarget()},
		ShellCommand:    helperCommand("sleep"),
		TmuxCommand:     []string{tmuxPath},
		TmuxOwnerMarker: "kenn-forge:test-owner",
	})
	t.Cleanup(mgr.Shutdown)

	session, err := mgr.Launch(
		context.Background(), "ws:alpha", "/tmp/work tree",
		string(LaunchTargetPlainShell),
	)
	require.NoError(err)
	sessionName := tmuxSessionName("ws:alpha", session.Key)

	assert.Equal(sessionName, session.TmuxSession)
	assert.Equal(string(LaunchTargetPlainShell), session.TargetKey)
	assert.Equal("Shell", session.Label)
	records := readNullArgvRecord(t, record)
	var newSession []string
	for _, record := range records {
		if len(record) > 0 && record[0] == "new-session" {
			newSession = record
			break
		}
	}
	require.NotEmpty(newSession)
	newSessionText := strings.Join(newSession, "\n")
	scriptText := requireNewSessionPaneScript(t, newSession)
	assert.Contains(newSession, "-c")
	assert.Contains(newSession, "/tmp/work tree")
	assert.Contains(scriptText, "exec env -i")
	assert.Contains(scriptText, shellquote.Join(os.Args[0]))
	assert.Contains(
		scriptText,
		"XDG_RUNTIME_DIR=\"${XDG_RUNTIME_DIR-}\"",
	)
	assert.Contains(
		scriptText,
		"KENN_FORGE_TEST_CUSTOM_SHELL_ENV=\"${KENN_FORGE_TEST_CUSTOM_SHELL_ENV-}\"",
	)
	assert.Contains(newSession, "-E")
	assert.NotContains(newSession, "-e")
	assert.Contains(scriptText, "__kenn_forge_env_file=")
	assert.Contains(newSession, ";")
	assert.Contains(newSession, "set-option")
	assert.Contains(newSession, "-t")
	assert.Contains(newSession, sessionName)
	assert.Contains(newSession, "@forge_owner")
	assert.Contains(newSession, "kenn-forge:test-owner")
	assert.NotContains(newSessionText, "argv-visible-value")
	assert.NotContains(newSessionText, "custom-visible-value")
	assert.Len(mgr.ListSessions("ws:alpha"), 1)
}

func TestManagerRestoreTmuxSessionRestoresPlainShellRuntimeSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmuxPath := writeLongRunningAttachTmux(t)
	mgr := NewManager(Options{
		TmuxCommand: []string{tmuxPath},
	})
	t.Cleanup(mgr.Shutdown)
	createdAt := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  "ws-1_shell-restored",
		TmuxSession: "middleman-ws-1-shell",
		TargetKey:   string(LaunchTargetPlainShell),
		CreatedAt:   createdAt,
	}})
	require.NoError(err)

	sessions := mgr.ListSessions("ws-1")
	require.Len(sessions, 1)
	shell := sessions[0]
	assert.Equal("ws-1_shell-restored", shell.Key)
	assert.Equal(string(LaunchTargetPlainShell), shell.TargetKey)
	assert.Equal(LaunchTargetPlainShell, shell.Kind)
	assert.Equal("Shell", shell.Label)
	assert.Equal("middleman-ws-1-shell", shell.TmuxSession)
	assert.Equal(createdAt, shell.CreatedAt)
}

func TestManagerRestoredRuntimeCommandForcesUTF8(t *testing.T) {
	mgr := NewManager(Options{
		TmuxCommand: []string{"/usr/bin/tmux", "-L", "kenn-forge-test"},
	})
	t.Cleanup(mgr.Shutdown)

	command, err := mgr.restoredRuntimeCommand(RestoredRuntimeSession{
		TmuxSession: "kenn-forge-ws-1-shell",
	})

	require.NoError(t, err)
	assert.Equal(t,
		[]string{
			"/usr/bin/tmux", "-L", "kenn-forge-test",
			"-u", "attach-session", "-E", "-t", "kenn-forge-ws-1-shell",
		},
		command,
	)
}

func TestManagerRestoreTmuxSessionReusesExistingPlainShellRuntimeSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmuxPath := writeLongRunningAttachTmux(t)
	mgr := NewManager(Options{
		TmuxCommand: []string{tmuxPath},
	})
	t.Cleanup(mgr.Shutdown)
	restored := RestoredRuntimeSession{
		WorkspaceID: "ws-1",
		SessionKey:  "ws-1_shell-restored",
		TmuxSession: "kenn-forge-ws-1-shell",
		TargetKey:   string(LaunchTargetPlainShell),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}

	require.NoError(mgr.RestoreRuntimeSessions(
		context.Background(), []RestoredRuntimeSession{restored},
	))
	require.NoError(mgr.RestoreRuntimeSessions(
		context.Background(), []RestoredRuntimeSession{restored},
	))

	assert.Len(mgr.ListSessions("ws-1"), 1)
}

func TestManagerRestorePtyOwnerSessionIgnoresRemovedTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sessionKey := "ws-1_removedtarget"
	owner := newFakeRuntimePtyOwner()
	owner.startedSession = sessionKey
	owner.startedPTY = &fakeRuntimePTY{
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(mgr.Shutdown)
	createdAt := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  sessionKey,
		TargetKey:   "removed-agent",
		Label:       "Removed Agent",
		Kind:        LaunchTargetAgent,
		CWD:         t.TempDir(),
		CreatedAt:   createdAt,
	}})
	require.NoError(err)

	sessions := mgr.ListSessions("ws-1")
	require.Len(sessions, 1)
	assert.Equal(0, owner.starts)
	assert.Equal(1, owner.attaches)
	assert.Equal(sessionKey, sessions[0].Key)
	assert.Equal("removed-agent", sessions[0].TargetKey)
	assert.Equal("Removed Agent", sessions[0].Label)
	assert.Equal(LaunchTargetAgent, sessions[0].Kind)
	assert.Equal(createdAt, sessions[0].CreatedAt)
}

func TestManagerRestorePtyOwnerSessionRetriesAttach(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sessionKey := "ws-1_retry-attach"
	owner := newFakeRuntimePtyOwner()
	owner.startedSession = sessionKey
	owner.startedPTY = &fakeRuntimePTY{
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	owner.attachErrs = []error{
		errors.New("owner socket not ready"),
		errors.New("owner still starting"),
	}
	previousBackOff := newPtyOwnerAttachBackOff
	newPtyOwnerAttachBackOff = func() backoff.BackOff {
		expo := backoff.NewExponentialBackOff()
		expo.InitialInterval = time.Millisecond
		expo.MaxInterval = time.Millisecond
		expo.RandomizationFactor = 0
		return expo
	}
	t.Cleanup(func() { newPtyOwnerAttachBackOff = previousBackOff })
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(mgr.Shutdown)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  sessionKey,
		TargetKey:   "helper",
		Label:       "Helper",
		Kind:        LaunchTargetAgent,
		CWD:         t.TempDir(),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}})
	require.NoError(err)

	sessions := mgr.ListSessions("ws-1")
	require.Len(sessions, 1)
	assert.Equal(3, owner.attaches)
	assert.Equal(sessionKey, sessions[0].Key)
	assert.Equal(SessionStatusRunning, sessions[0].Status)
}

func TestManagerRestorePtyOwnerAttachFailureIsUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sessionKey := "ws-1_unavailable-attach"
	owner := newFakeRuntimePtyOwner()
	owner.startedSession = sessionKey
	owner.startedPTY = &fakeRuntimePTY{
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	owner.attachErrs = []error{
		errors.New("owner socket not ready"),
		errors.New("owner still starting"),
		errors.New("owner still absent"),
		errors.New("owner gone"),
	}
	previousBackOff := newPtyOwnerAttachBackOff
	newPtyOwnerAttachBackOff = func() backoff.BackOff {
		expo := backoff.NewExponentialBackOff()
		expo.InitialInterval = time.Millisecond
		expo.MaxInterval = time.Millisecond
		expo.RandomizationFactor = 0
		return expo
	}
	t.Cleanup(func() { newPtyOwnerAttachBackOff = previousBackOff })
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(mgr.Shutdown)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  sessionKey,
		TargetKey:   "helper",
		Label:       "Helper",
		Kind:        LaunchTargetAgent,
		CWD:         t.TempDir(),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}})
	require.ErrorIs(err, ErrSessionUnavailable)
	require.NotErrorIs(err, ErrSessionNotFound)
	assert.Equal(4, owner.attaches)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerRestoreTmuxSessionAttachesStoredSessionWithoutOwnerValidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte(`#!/bin/sh
if [ "$1" = "-u" ]; then shift; fi
if [ "$1" = "show-options" ]; then
  exit 99
fi
if [ "$1" = "attach-session" ]; then
  trap 'exit 0' HUP INT TERM
  while :; do sleep 1; done
fi
exit 0
`), 0o755))
	mgr := NewManager(Options{
		TmuxCommand:     []string{tmuxPath},
		TmuxOwnerMarker: "kenn-forge:test-owner",
	})
	t.Cleanup(mgr.Shutdown)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  "ws-1_shell-restored",
		TmuxSession: "kenn-forge-ws-1-shell",
		TargetKey:   string(LaunchTargetPlainShell),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}})

	require.NoError(err)
	assert.Len(mgr.ListSessions("ws-1"), 1)
}

func TestManagerRestoreTmuxSessionUnavailableWhenCommandCannotResolve(
	t *testing.T,
) {
	require := require.New(t)

	mgr := NewManager(Options{
		TmuxCommand:     []string{"/missing/kenn-forge-test-tmux"},
		TmuxOwnerMarker: "kenn-forge:test-owner",
	})
	t.Cleanup(mgr.Shutdown)

	err := mgr.RestoreRuntimeSessions(context.Background(), []RestoredRuntimeSession{{
		WorkspaceID: "ws-1",
		SessionKey:  "ws-1_shell-restored",
		TmuxSession: "kenn-forge-ws-1-shell",
		TargetKey:   string(LaunchTargetPlainShell),
		CreatedAt:   time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
	}})

	require.ErrorIs(err, ErrSessionUnavailable)
}

func TestTmuxSessionNameUsesOpaqueTargetHash(t *testing.T) {
	assert := assert.New(t)

	fooSlash := tmuxSessionName("ws:alpha", "foo/bar")
	fooColon := tmuxSessionName("ws:alpha", "foo:bar")

	assert.NotEqual(fooSlash, fooColon)
	assert.NotContains(fooSlash, "foo")
	assert.NotContains(fooSlash, "/")
	assert.NotContains(fooColon, ":")
	assert.True(strings.HasPrefix(fooSlash, "forge-ws-alpha-"))
}

func TestManagerLaunchCommandFailsWhenOwnerMarkingFailsDuringCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux owner shell wrapper uses Unix shell semantics")
	}

	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux-fails-owner-create")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
printf '%%s\0' "$@" >> %s
case "$1" in
  has-session)
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session)
    for a in "$@"; do
      if [ "$a" = "@forge_owner" ]; then
        exit 42
      fi
    done
    exit 0
    ;;
  kill-session)
    exit 0
    ;;
esac
exit 0
`, shellquote.Join(record)), 0o755))

	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{tmuxPath},
				Available: true,
			},
		},
		TmuxCommand:             []string{tmuxPath},
		TmuxOwnerMarker:         "kenn-forge:test-owner",
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())
	require.Error(err)
	data, err := os.ReadFile(record)
	require.NoError(err)
	recorded := string(data)
	assert.Contains(recorded, "new-session")
	assert.Contains(recorded, "@forge_owner")
	assert.NotContains(recorded, "kill-session")
}

func TestManagerLaunchCommandDoesNotKillSessionWhenTmuxCreateFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux owner shell wrapper uses Unix shell semantics")
	}

	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux-fails-new-session")
	require.NoError(os.WriteFile(tmuxPath, fmt.Appendf(nil, `#!/bin/sh
printf '%%s\0' "$@" >> %s
case "$1" in
  has-session)
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session)
    exit 42
    ;;
  kill-session)
    exit 0
    ;;
esac
exit 0
`, shellquote.Join(record)), 0o755))

	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{tmuxPath},
				Available: true,
			},
		},
		TmuxCommand:             []string{tmuxPath},
		TmuxOwnerMarker:         "kenn-forge:test-owner",
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())
	require.Error(err)
	data, err := os.ReadFile(record)
	require.NoError(err)
	recorded := string(data)
	assert.Contains(recorded, "new-session")
	assert.NotContains(recorded, "kill-session")
}

func TestManagerLaunchCommandRejectsRelativeAgentCommandWhenWrapped(t *testing.T) {
	agent := helperTarget("codex", "sleep")
	agent.Command = []string{"./codex"}
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{"/usr/bin/tmux"},
				Available: true,
			},
		},
		TmuxCommand:             []string{"/usr/bin/tmux"},
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())

	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute path")
}

func TestManagerLaunchCommandDoesNotEmbedEnvForWrappedAgent(t *testing.T) {
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "secret-token")
	t.Setenv("CONTEXT7_API_KEY", "context7-secret")
	t.Setenv("XDG_RUNTIME_DIR", "not-carried")
	assert := assert.New(t)
	resolvedShell, err := resolveExecutable("sh")
	require.NoError(t, err)

	paneCommand := tmuxAgentEnvPolicy.paneEnvironment(
		os.Environ(), []string{resolvedShell, "-c", "echo ok"}, nil,
	).paneCommand
	assert.Contains(paneCommand, "exec ")
	assert.Contains(paneCommand, "env -i")
	assert.Contains(paneCommand, shellquote.Join(resolvedShell))
	assert.Contains(
		paneCommand,
		"XDG_RUNTIME_DIR=\"${XDG_RUNTIME_DIR-}\"",
	)
	assert.NotContains(paneCommand, "TERM=xterm-256color")
	assert.NotContains(paneCommand, "secret-token")
	assert.NotContains(paneCommand, "context7-secret")
	assert.NotContains(paneCommand, "not-carried")
}

func TestTmuxLauncherCopiesClientEnvWithoutGlobalUpdateEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux environment handoff uses Unix tmux")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux unavailable in this test environment: %v", err)
	}

	require := require.New(t)
	t.Setenv("XDG_RUNTIME_DIR", "client-visible-value")
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "client-secret")
	t.Setenv("KENN_FORGE_STRIPPED_ENV", "client-stripped")

	tmuxCommand := privateTmuxOwner.Command(t, tmuxPath)
	socket := tmuxCommand[len(tmuxCommand)-1]
	dir := filepath.Dir(socket)
	output := filepath.Join(dir, "env-output")
	seed := "kenn-forge-seed-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sessionName := "kenn-forge-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedCmd := procutil.Command(
		tmuxCommand[0], append(
			append([]string(nil), tmuxCommand[1:]...),
			"new-session", "-d", "-s", seed, "sleep 10",
		)...,
	)
	seedCmd.Env = append(
		sessionEnvironment(os.Environ(), []string{"KENN_FORGE_STRIPPED_ENV"}),
		"XDG_RUNTIME_DIR=server-visible-value",
		"KENN_FORGE_GITHUB_TOKEN=server-secret",
		"KENN_FORGE_STRIPPED_ENV=server-stripped",
		"TERM=xterm-256color",
	)
	runOutput, err := seedCmd.CombinedOutput()
	require.NoError(err, string(runOutput))

	printCommand := fmt.Sprintf(
		"printf '%%s\\n%%s\\n%%s\\n' "+
			"\"$XDG_RUNTIME_DIR\" "+
			"\"${KENN_FORGE_GITHUB_TOKEN-unset}\" "+
			"\"${KENN_FORGE_STRIPPED_ENV-unset}\" > %s",
		shellquote.Join(output),
	)
	paneEnv := tmuxAgentEnvPolicy.paneEnvironment(
		os.Environ(),
		[]string{"/bin/sh", "-c", printCommand},
		[]string{"KENN_FORGE_STRIPPED_ENV"},
	)
	paneCommand := paneEnv.paneCommand
	require.NotContains(paneCommand, "client-visible-value")
	require.NotContains(paneCommand, "client-secret")
	require.NotContains(paneCommand, "client-stripped")
	require.NotContains(paneCommand, "server-visible-value")
	require.NotContains(paneCommand, "server-secret")
	require.NotContains(paneCommand, "server-stripped")

	_, err = tmuxLauncher{
		TmuxCommand: tmuxCommand,
		Session:     sessionName,
		Pane:        paneEnv,
	}.prepare(context.Background())
	require.NoError(err)

	// Poll for content, not existence: the shell creates the file via
	// the > redirect before printf writes, so an existence check can
	// read the file empty under load.
	require.Eventually(func() bool {
		data, err := os.ReadFile(output)
		return err == nil && len(data) > 0
	}, 5*time.Second, 20*time.Millisecond)
	data, err := os.ReadFile(output)
	require.NoError(err)
	require.Equal("client-visible-value\nunset\nunset\n", string(data))

	cmd := procutil.Command(
		tmuxPath, "-f", "/dev/null", "-S", socket,
		"show-option", "-gqv", "update-environment",
	)
	globalEnv, err := cmd.CombinedOutput()
	require.NoError(err, string(globalEnv))
	require.NotContains(string(globalEnv), "XDG_RUNTIME_DIR")
}

func TestManagerLaunchCommandFallsBackWhenTmuxUnavailable(t *testing.T) {
	assert := assert.New(t)
	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{"tmux"},
				Available: false, DisabledReason: "tmux not found",
			},
		},
		TmuxCommand:             []string{"tmux"},
		WrapAgentSessionsInTmux: true,
	})
	t.Cleanup(mgr.Shutdown)

	launch, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())
	require.NoError(t, err)

	assert.Equal(agent.Command, launch.Command)
	assert.Empty(launch.TmuxSession)
}

func TestManagerLaunchUsesPtyOwnerWhenConfigured(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	backend := newFakeRuntimePtyOwner()
	agent := helperTarget("codex", "exit")
	mgr := NewManager(Options{
		Targets:         []LaunchTarget{agent},
		PtyOwnerRuntime: backend,
	})
	t.Cleanup(mgr.Shutdown)

	info, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "codex")
	require.NoError(err)

	assert.Equal(SessionStatusRunning, info.Status)
	assert.Equal(info.Key, backend.startedSession)
	assert.NotContains(backend.startedSession, ":")
	assert.Equal(agent.Command, backend.startedCommand)
	assert.Len(mgr.ListSessions("ws-1"), 1)
}

func TestManagerLaunchPassesStripEnvVarsToPtyOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	backend := newFakeRuntimePtyOwner()
	agent := helperTarget("codex", "exit")
	mgr := NewManager(Options{
		Targets:         []LaunchTarget{agent},
		PtyOwnerRuntime: backend,
		StripEnvVars:    []string{"WORKSPACE_TOKEN", "WORKSPACE_TOKEN"},
	})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "codex")
	require.NoError(err)

	assert.Equal([]string{"WORKSPACE_TOKEN"}, backend.startedStripEnvVars)
}

func TestManagerPtyOwnerShellCharacterLocale(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS shell locale default")
	}
	requirePTYAvailable(t)
	for _, tt := range []struct {
		name   string
		kind   LaunchTargetKind
		locale string
		want   string
	}{
		{name: "shell default", kind: LaunchTargetPlainShell, want: "UTF-8"},
		{name: "explicit shell locale", kind: LaunchTargetPlainShell, locale: "C", want: "C"},
		{name: "agent unchanged", kind: LaunchTargetAgent, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LANG", "")
			t.Setenv("LC_ALL", "")
			t.Setenv("LC_CTYPE", tt.locale)
			cwd := t.TempDir()
			mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
				Targets: []LaunchTarget{{
					Key: "test-shell", Kind: tt.kind, Available: true,
					Command: []string{"/bin/sh", "-c", `printf '%s' "$LC_CTYPE" > locale; read line`},
				}},
			}))
			t.Cleanup(mgr.Shutdown)
			info, err := mgr.Launch(t.Context(), "ws-locale", cwd, "test-shell")
			require.NoError(t, err)
			assert.Empty(t, info.TmuxSession)
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(cwd, "locale"))
				return err == nil && string(data) == tt.want
			}, 2*time.Second, 10*time.Millisecond)
		})
	}
}

func TestManagerUpdateStripEnvVarsPreservesPreviousNamesForFutureLaunches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	backend := newFakeRuntimePtyOwner()
	agent := helperTarget("codex", "exit")
	mgr := NewManager(Options{
		Targets:         []LaunchTarget{agent},
		PtyOwnerRuntime: backend,
		StripEnvVars:    []string{"OLD_TOKEN"},
	})
	t.Cleanup(mgr.Shutdown)

	mgr.UpdateStripEnvVars([]string{"NEW_TOKEN", "NEW_TOKEN"})
	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "codex")
	require.NoError(err)

	assert.Equal([]string{"OLD_TOKEN", "NEW_TOKEN"}, backend.startedStripEnvVars)
}

func TestManagerUpdateTargetsAndStripEnvVarsPreservesPreviousNames(t *testing.T) {
	assert := assert.New(t)
	oldAgent := helperTarget("old", "exit")
	newAgent := helperTarget("new", "exit")
	mgr := NewManager(Options{
		Targets:      []LaunchTarget{oldAgent},
		StripEnvVars: []string{"OLD_TOKEN"},
	})
	t.Cleanup(mgr.Shutdown)

	mgr.UpdateTargetsAndStripEnvVars(
		[]LaunchTarget{newAgent},
		[]string{"NEW_TOKEN", "NEW_TOKEN"},
	)

	targets := mgr.LaunchTargets()
	assert.Len(targets, 1)
	assert.Equal("new", targets[0].Key)
	assert.Equal([]string{"OLD_TOKEN", "NEW_TOKEN"}, mgr.currentStripEnvVars())
}

func TestManagerLaunchTargetsHideInternalShellTarget(t *testing.T) {
	mgr := NewManager(Options{Targets: []LaunchTarget{
		{
			Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
			Source: "system", Command: []string{"tmux"}, Available: true,
		},
		plainShellTarget(),
		helperTarget("codex", "sleep"),
	}})
	t.Cleanup(mgr.Shutdown)

	targets := mgr.LaunchTargets()
	assert := assert.New(t)
	assert.Len(targets, 2)
	assert.Equal(string(LaunchTargetPlainShell), targets[0].Key)
	assert.Equal("codex", targets[1].Key)
}

func TestManagerLaunchCommandDoesNotWrapWhenConfigDisabled(t *testing.T) {
	assert := assert.New(t)
	agent := helperTarget("codex", "sleep")
	mgr := NewManager(Options{
		Targets: []LaunchTarget{
			agent,
			{
				Key: "shell", Label: "Shell", Kind: LaunchTargetShell,
				Source: "system", Command: []string{"/usr/bin/tmux"},
				Available: true,
			},
		},
		TmuxCommand:             []string{"/usr/bin/tmux"},
		WrapAgentSessionsInTmux: false,
	})
	t.Cleanup(mgr.Shutdown)

	launch, err := mgr.launchCommand(context.Background(), agent, "ws-1", t.TempDir())
	require.NoError(t, err)

	assert.Equal(agent.Command, launch.Command)
	assert.Empty(launch.TmuxSession)
}

func TestManagerStopReportsTmuxCleanupFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmuxPath := filepath.Join(t.TempDir(), "tmux-fails")
	require.NoError(os.WriteFile(
		tmuxPath,
		[]byte("#!/bin/sh\nexit 42\n"),
		0o755,
	))
	done := make(chan struct{})
	close(done)
	mgr := NewManager(Options{TmuxCommand: []string{tmuxPath}})
	mgr.sessions["ws-1:codex"] = &session{
		info: SessionInfo{
			Key:         "ws-1:codex",
			WorkspaceID: "ws-1",
			TargetKey:   "codex",
			Kind:        LaunchTargetAgent,
		},
		cmd:         &exec.Cmd{},
		tmuxSession: "kenn-forge-ws-1-codex",
		done:        done,
	}

	err := mgr.Stop(context.Background(), "ws-1", "ws-1:codex")

	require.Error(err)
	require.Contains(err.Error(), "kill tmux session")
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerStopFailedTmuxCleanupDoesNotSuppressExitCleanup(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	tmuxPath := filepath.Join(t.TempDir(), "tmux-fails")
	require.NoError(os.WriteFile(
		tmuxPath,
		[]byte("#!/bin/sh\nexit 42\n"),
		0o755,
	))
	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
		Targets: []LaunchTarget{
			helperTarget("helper", "sleep"),
		},
		TmuxCommand: []string{tmuxPath},
	}))
	t.Cleanup(mgr.Shutdown)

	info, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(err)

	mgr.mu.Lock()
	mgr.sessions[info.Key].tmuxSession = "kenn-forge-ws-1-helper"
	mgr.mu.Unlock()

	err = mgr.Stop(ctx, "ws-1", info.Key)

	require.Error(err)
	require.Contains(err.Error(), "kill tmux session")
	assert.Eventually(func() bool {
		return len(mgr.ListSessions("ws-1")) == 0
	}, 2*time.Second, 20*time.Millisecond)
}

func TestManagerStopIgnoresAbsentTmuxSession(t *testing.T) {
	tmuxPath := filepath.Join(t.TempDir(), "tmux-absent")
	require.NoError(t, os.WriteFile(
		tmuxPath,
		[]byte("#!/bin/sh\necho \"can't find session: nope\" >&2\nexit 1\n"),
		0o755,
	))
	done := make(chan struct{})
	close(done)
	mgr := NewManager(Options{TmuxCommand: []string{tmuxPath}})
	mgr.sessions["ws-1:codex"] = &session{
		info: SessionInfo{
			Key:         "ws-1:codex",
			WorkspaceID: "ws-1",
			TargetKey:   "codex",
			Kind:        LaunchTargetAgent,
		},
		cmd:         &exec.Cmd{},
		tmuxSession: "kenn-forge-ws-1-codex",
		done:        done,
	}

	err := mgr.Stop(context.Background(), "ws-1", "ws-1:codex")

	require.NoError(t, err)
}

func TestManagerShutdownLeavesTmuxSessionsRunning(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux-records")
	require.NoError(os.WriteFile(
		tmuxPath,
		[]byte("#!/bin/sh\nTMUX_RECORD="+shellquote.Join(record)+
			"\nprintf '%s\\0' \"$@\" >> \"$TMUX_RECORD\"\n"),
		0o755,
	))
	mgr := NewManager(Options{
		TmuxCommand:                    []string{tmuxPath},
		DetachSessionsForServerRestart: true,
	})
	info := SessionInfo{
		Key:         "ws-1_codex",
		WorkspaceID: "ws-1",
		TargetKey:   "codex",
		Kind:        LaunchTargetAgent,
		TmuxSession: "kenn-forge-ws-1-codex",
	}
	s, err := startTmuxAttachSession(
		info, helperCommand("sleep"), t.TempDir(), nil,
	)
	require.NoError(err)
	go s.watch()

	var pid int
	s.mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	s.mu.Unlock()
	require.Positive(pid)
	require.True(processAlive(pid), "local attach client should be alive")

	mgr.mu.Lock()
	mgr.sessions[info.Key] = s
	mgr.mu.Unlock()
	attachment, err := mgr.AttachSession(info.WorkspaceID, info.Key)
	require.NoError(err)
	defer attachment.Close()

	mgr.Shutdown()

	assert.True(attachment.RecoverableDetach())
	_, statErr := os.Stat(record)
	assert.True(os.IsNotExist(statErr), "shutdown should not invoke tmux cleanup")
	assert.Eventually(func() bool {
		return !processAlive(pid)
	}, 5*time.Second, 25*time.Millisecond)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerRejectsUnownedRuntimeSessions(t *testing.T) {
	mgr := NewManager(Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}})
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.Launch(context.Background(), "ws-1", t.TempDir(), "helper")
	require.Error(t, err)
	require.Contains(t, err.Error(), "runtime sessions require tmux or ptyowner")
}

func TestManagerShutdownDetachesPtyOwnerSessions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.Background()
	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
		ShellCommand:    []string{"/bin/sh"},
		Targets:         []LaunchTarget{plainShellTarget()},
	})

	info, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(err)
	require.Equal(string(LaunchTargetPlainShell), info.TargetKey)

	mgr.Shutdown()

	assert.Empty(owner.stoppedSession)
	assert.Eventually(func() bool {
		select {
		case <-owner.startedPTY.Done():
			return true
		default:
			return false
		}
	}, 2*time.Second, 20*time.Millisecond)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerStopWorkspaceStopsKnownPtyOwnerSessionsAfterRestart(t *testing.T) {
	assert := assert.New(t)

	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
		KnownPtyOwnerSessionKeys: func(
			context.Context,
			string,
		) ([]string, error) {
			return []string{"ws-1_a", "ws-1_b"}, nil
		},
	})

	mgr.StopWorkspace(context.Background(), "ws-1")

	assert.ElementsMatch([]string{"ws-1_a", "ws-1_b"}, owner.stoppedSessions)
}

func TestPtyOwnerLifecycleStopClosesAttachmentAfterOwnerStopFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	owner := newFakeRuntimePtyOwner()
	owner.stopErr = errors.New("stop failed")
	ptySession := &fakeRuntimePTY{
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	lifecycle := ptyOwnerLifecycle{
		owner:   owner,
		session: "session-1",
		pty:     ptySession,
	}

	err := lifecycle.Stop(context.Background())
	require.Error(err)
	assert.Contains(err.Error(), "stop failed")
	select {
	case <-ptySession.Done():
	case <-time.After(2 * time.Second):
		require.Fail("pty attachment was not closed")
	}
}

func TestManagerStopKeepsPtyOwnerSessionRetryableAfterStopFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.Background()
	owner := newFakeRuntimePtyOwner()
	owner.stopErr = errors.New("stop failed")
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
		ShellCommand:    []string{"/bin/sh"},
		Targets:         []LaunchTarget{plainShellTarget()},
	})
	t.Cleanup(mgr.Shutdown)

	info, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(err)

	err = mgr.Stop(ctx, "ws-1", info.Key)
	require.Error(err)
	require.Contains(err.Error(), "stop failed")
	_, ok := mgr.session("ws-1", info.Key)
	require.True(ok, "failed ptyowner stop should keep retry handle")

	owner.stopErr = nil
	err = mgr.Stop(ctx, "ws-1", info.Key)
	require.NoError(err)
	_, ok = mgr.session("ws-1", info.Key)
	assert.False(ok)
	assert.Equal([]string{info.Key, info.Key}, owner.stoppedSessions)
}

func TestRollbackLaunchStopsOwnedNonTmuxSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
		ShellCommand:    []string{"/bin/sh"},
		Targets:         []LaunchTarget{plainShellTarget()},
	})
	t.Cleanup(mgr.Shutdown)

	info, err := mgr.Launch(
		context.Background(), "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(err)
	require.Empty(info.TmuxSession)

	require.NoError(mgr.RollbackLaunch(context.Background(), info))
	assert.Equal([]string{info.Key}, owner.stoppedSessions)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestRollbackLaunchPreservesReusedNonTmuxSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{
		PtyOwnerRuntime: owner,
		ShellCommand:    []string{"/bin/sh"},
		Targets:         []LaunchTarget{plainShellTarget()},
	})
	t.Cleanup(mgr.Shutdown)

	info, err := mgr.Launch(
		context.Background(), "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(err)
	info.Reused = true

	require.NoError(mgr.RollbackLaunch(context.Background(), info))
	assert.Empty(owner.stoppedSessions)
	assert.Len(mgr.ListSessions("ws-1"), 1)
}

func TestManagerStopRemovesSession(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}}))
	t.Cleanup(mgr.Shutdown)

	session, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(ctx, "ws-1", session.Key))

	assert := assert.New(t)
	assert.Empty(mgr.ListSessions("ws-1"))
	assert.Error(mgr.Stop(ctx, "ws-1", session.Key))
}

func TestManagerLaunchRejectsWhileWorkspaceStopping(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	record := filepath.Join(t.TempDir(), "pids")
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{{
		Key: "helper", Label: "helper", Kind: LaunchTargetAgent,
		Source: "config", Available: true,
		Command: helperRecordCommand(record),
	}}}))
	t.Cleanup(mgr.Shutdown)

	mgr.mu.Lock()
	mgr.stoppingWS["ws-1"] = 1
	mgr.mu.Unlock()

	_, err := mgr.Launch(context.Background(), "ws-1", t.TempDir(), "helper")
	require.ErrorIs(err, errWorkspaceStopping)
	assert.Empty(mgr.ListSessions("ws-1"))

	// Whatever PID the helper recorded before being killed must be
	// gone — no orphan from the rejected launch.
	assert.Eventually(func() bool {
		data, err := os.ReadFile(record)
		if err != nil || len(data) == 0 {
			return true // helper died before recording
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			pid, _ := strconv.Atoi(line)
			if processAlive(pid) {
				return false
			}
		}
		return true
	}, 5*time.Second, 25*time.Millisecond,
		"rejected launch's helper process must be reaped")

	// Launches succeed again once the marker clears.
	mgr.mu.Lock()
	delete(mgr.stoppingWS, "ws-1")
	mgr.mu.Unlock()
	_, err = mgr.Launch(context.Background(), "ws-1", t.TempDir(), "helper")
	require.NoError(err)
}

func TestBeginStoppingRejectsLaunchUntilEnd(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)

	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}}))
	t.Cleanup(mgr.Shutdown)

	mgr.BeginStopping("ws-1")
	_, err := mgr.Launch(context.Background(), "ws-1", t.TempDir(), "helper")
	require.ErrorIs(err, errWorkspaceStopping)

	// Other workspaces are unaffected.
	_, err = mgr.Launch(context.Background(), "ws-2", t.TempDir(), "helper")
	require.NoError(err)

	mgr.EndStopping("ws-1")
	_, err = mgr.Launch(context.Background(), "ws-1", t.TempDir(), "helper")
	require.NoError(err)
}

func TestStopWorkspaceWaitsForInflightLaunches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr := NewManager(Options{})
	t.Cleanup(mgr.Shutdown)

	// Simulate a Launch that already passed claimInflight but has
	// not yet returned (i.e. still inside startSession). Without
	// the drain, StopWorkspace would snapshot empty sessions and
	// finish; the in-flight launch would then insert a session
	// after the workspace was supposedly stopped.
	mgr.mu.Lock()
	mgr.inflightWS["ws-1"] = 1
	mgr.mu.Unlock()

	stopReturned := make(chan struct{})
	go func() {
		mgr.StopWorkspace(context.Background(), "ws-1")
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
		require.FailNow(
			"StopWorkspace returned before inflight launch drained",
		)
	case <-time.After(75 * time.Millisecond):
	}

	mgr.releaseInflight("ws-1")

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		require.FailNow(
			"StopWorkspace did not return after inflight drained",
		)
	}

	// And the marker is cleared, so subsequent launches are not
	// permanently rejected.
	mgr.mu.Lock()
	stopping := mgr.stoppingWS["ws-1"]
	mgr.mu.Unlock()
	assert.Equal(0, stopping)
}

func TestManagerStopKillsDescendantProcesses(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	record := filepath.Join(t.TempDir(), "pids")
	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{Targets: []LaunchTarget{{
		Key: "helper", Label: "helper", Kind: LaunchTargetAgent,
		Source: "config", Available: true,
		Command: []string{
			os.Args[0],
			"-test.run=TestHelperProcess",
			"--",
			"spawn-child",
			record,
		},
	}}}))
	t.Cleanup(mgr.Shutdown)

	session, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(err)

	var parentPID, childPID int
	require.Eventually(func() bool {
		data, err := os.ReadFile(record)
		if err != nil {
			return false
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) < 2 {
			return false
		}
		parentPID, _ = strconv.Atoi(lines[0])
		childPID, _ = strconv.Atoi(lines[1])
		return parentPID > 0 && childPID > 0
	}, 5*time.Second, 25*time.Millisecond, "helper should record both pids")

	require.True(processAlive(parentPID), "parent should be alive")
	require.True(processAlive(childPID), "child should be alive")

	require.NoError(mgr.Stop(ctx, "ws-1", session.Key))

	assert.Eventually(func() bool {
		return !processAlive(parentPID) && !processAlive(childPID)
	}, 5*time.Second, 25*time.Millisecond,
		"descendant child should die with the session leader")
}

func TestSessionWatchLeavesOutputOpenForDrain(t *testing.T) {
	require := require.New(t)

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(err)
	defer writeEnd.Close()
	defer readEnd.Close()

	_, err = writeEnd.WriteString("final output")
	require.NoError(err)

	cmd := procutil.Command("sh", "-c", "exit 0")
	require.NoError(cmd.Start())
	outputDone := make(chan struct{})
	s := &session{
		cmd:        cmd,
		ptmx:       readEnd,
		done:       make(chan struct{}),
		outputDone: outputDone,
	}

	s.watch()

	buf := make([]byte, len("final output"))
	_, err = readEnd.Read(buf)
	require.NoError(err)
	close(outputDone)
	require.Equal("final output", string(buf))
}

func TestSessionWatchClosesPTYAfterPostExitDrainTimeout(t *testing.T) {
	require := require.New(t)

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(err)
	defer readEnd.Close()
	defer writeEnd.Close()

	cmd := procutil.Command("sh", "-c", "exit 0")
	require.NoError(cmd.Start())
	outputDone := make(chan struct{})
	s := &session{
		cmd:        cmd,
		ptmx:       readEnd,
		done:       make(chan struct{}),
		outputDone: outputDone,
	}

	s.watch()
	defer close(outputDone)

	require.Eventually(func() bool {
		_, err := readEnd.Stat()
		return err != nil
	}, time.Second, 10*time.Millisecond)
}

func TestSessionWatchPtyOwnerWaitsForFinalOutputDrain(t *testing.T) {
	require := require.New(t)
	ownedPTY := &fakeRuntimePTY{
		output: make(chan []byte, 1),
		done:   make(chan struct{}),
	}
	s := &session{
		pty:         ownedPTY,
		done:        make(chan struct{}),
		outputDone:  make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	output, unsubscribe := s.subscribe()
	defer unsubscribe()

	go s.drainOutput()
	watchDone := make(chan struct{})
	go func() {
		s.watchPtyOwner()
		close(watchDone)
	}()

	close(ownedPTY.done)
	time.Sleep(50 * time.Millisecond)
	ownedPTY.output <- []byte("final output")
	close(ownedPTY.output)

	select {
	case got, ok := <-output:
		require.True(ok)
		require.Equal("final output", string(got))
	case <-time.After(time.Second):
		require.Fail("final PTY-owner output was not delivered")
	}
	select {
	case <-watchDone:
	case <-time.After(time.Second):
		require.Fail("PTY-owner watcher did not finish after output drained")
	}
}

func TestManagerRemovesNaturallyExitedSession(t *testing.T) {
	ctx := context.Background()
	exited := make(chan SessionInfo, 1)
	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}, OnSessionExit: func(info SessionInfo) {
		exited <- info
	}, PtyOwnerRuntime: owner})
	t.Cleanup(mgr.Shutdown)

	session, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "helper")
	require.NoError(t, err)
	owner.startedPTY.Close()

	var got SessionInfo
	require.Eventually(t, func() bool {
		select {
		case got = <-exited:
			return true
		default:
			return false
		}
	}, 2*time.Second, 20*time.Millisecond)

	assert := assert.New(t)
	assert.Equal(session.Key, got.Key)
	assert.Equal(SessionStatusExited, got.Status)
	assert.NotNil(got.ExitedAt)
	assert.NotNil(got.ExitCode)
	assert.Equal(0, *got.ExitCode)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerRemovesNaturallyExitedShell(t *testing.T) {
	ctx := context.Background()
	exited := make(chan SessionInfo, 1)
	owner := newFakeRuntimePtyOwner()
	mgr := NewManager(Options{
		ShellCommand: []string{"/bin/sh"},
		Targets:      []LaunchTarget{plainShellTarget()},
		OnSessionExit: func(info SessionInfo) {
			exited <- info
		},
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(mgr.Shutdown)

	shell, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(t, err)
	owner.startedPTY.Close()

	var got SessionInfo
	require.Eventually(t, func() bool {
		select {
		case got = <-exited:
			return true
		default:
			return false
		}
	}, 2*time.Second, 20*time.Millisecond)

	assert := assert.New(t)
	assert.Equal(shell.Key, got.Key)
	assert.Equal(SessionStatusExited, got.Status)
	assert.NotNil(got.ExitedAt)
	assert.NotNil(got.ExitCode)
	assert.Equal(0, *got.ExitCode)
	assert.Empty(mgr.ListSessions("ws-1"))
}

func TestManagerLaunchPlainShellCreatesIndependentSessions(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
		ShellCommand: helperCommand("sleep"),
		Targets:      []LaunchTarget{plainShellTarget()},
	}))
	t.Cleanup(mgr.Shutdown)

	shell1, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(t, err)
	shell2, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(t, err)

	assert := assert.New(t)
	assert.NotEqual(shell1.Key, shell2.Key)
	assert.Equal(SessionStatusRunning, shell1.Status)
	assert.Equal(SessionStatusRunning, shell2.Status)
	assert.Equal("Shell", shell1.Label)
	assert.Equal("Shell 2", shell2.Label)
	assert.Len(mgr.ListSessions("ws-1"), 2)
}

// TestAttachmentSessionOutputClosedDistinguishesSubscriberDrop covers
// the contract bridges rely on to tell a real session exit from a
// dropped subscriber: a closed Output channel can mean either, and
// auto-closing the drawer on the latter would hang the user out on a
// healthy shell.
//
// broadcast drops a subscriber when its 64-slot buffer can't accept
// another chunk (slow client / congested writer). drainOutput's PTY
// EOF, in contrast, runs closeSubscribers which flips s.outputClosed
// before closing every subscriber channel. SessionOutputClosed
// exposes that distinction to bridge code.
func TestAttachmentSessionOutputClosedDistinguishesSubscriberDrop(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
		ShellCommand: helperCommand("sleep"),
		Targets:      []LaunchTarget{plainShellTarget()},
	}))
	t.Cleanup(mgr.Shutdown)

	shell, err := mgr.Launch(
		ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell),
	)
	require.NoError(err)

	attach, err := mgr.AttachSession("ws-1", shell.Key)
	require.NoError(err)
	t.Cleanup(attach.Close)
	require.Equal(shell.Key, attach.Info().Key)

	// Healthy session: SessionOutputClosed must report false.
	assert.False(attach.SessionOutputClosed(),
		"freshly-attached session should not look output-closed")

	mgr.mu.Lock()
	s := mgr.sessions[shell.Key]
	mgr.mu.Unlock()
	require.NotNil(s)

	// Force the broadcast-drops-subscriber path: the channel buffer
	// is 64, so the 65th broadcast that can't enqueue takes the
	// `default` branch and closes the channel. Run the broadcasts
	// synchronously WITHOUT a concurrent consumer — a parallel
	// reader could drain the buffer faster than we fill it and the
	// drop would never trigger. Drain afterward to confirm closure.
	for range 200 {
		s.broadcast([]byte("x"))
	}
	drained := 0
drain:
	for {
		// Bound the receive: if broadcast regresses and never
		// closes the channel, the buffered messages drain and the
		// next receive would block forever, hanging the test
		// process instead of failing it.
		select {
		case _, ok := <-attach.Output:
			if !ok {
				break drain
			}
			drained++
			require.Less(drained, 200,
				"channel never closed; broadcast did not "+
					"drop the slow subscriber")
		case <-time.After(2 * time.Second):
			require.Fail(
				"timed out waiting for channel close; " +
					"broadcast did not drop the slow subscriber",
			)
		}
	}
	assert.LessOrEqual(drained, 64,
		"buffer is 64; drop should fire by the 65th broadcast")

	// Subscriber dropped, but the session itself is still healthy
	// (helperCommand("sleep") is still running and drainOutput has
	// not seen PTY EOF). SessionOutputClosed must NOT be true here —
	// otherwise the bridge would emit "exited" on a live shell.
	assert.False(attach.SessionOutputClosed(),
		"subscriber drop must not be misreported as session exit")

	// Now simulate the real session-exit path. closeSubscribers is
	// what drainOutput calls on PTY EOF; it flips outputClosed.
	s.closeSubscribers()
	assert.True(attach.SessionOutputClosed(),
		"after drainOutput's closeSubscribers, the bridge must see "+
			"the session as output-closed and emit the exit frame")
}

func TestAttachmentResizeOwnerPrefersActiveLocalUntilInactive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pty := &fakeRuntimePTY{
		output: make(chan []byte),
		done:   make(chan struct{}),
	}
	s := &session{
		info: SessionInfo{
			Key:         "session-1",
			WorkspaceID: "ws-1",
			Status:      SessionStatusRunning,
		},
		pty:         pty,
		done:        make(chan struct{}),
		outputDone:  make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}

	remote, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityRemote,
			ResizeActive:   true,
		},
	)
	require.NoError(err)
	defer remote.Close()
	require.NoError(remote.Resize(ptysize.Geometry{Cols: 80, Rows: 24}))

	local, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{ResizePriority: ResizePriorityLocal},
	)
	require.NoError(err)
	require.NoError(remote.Resize(ptysize.Geometry{Cols: 90, Rows: 25}))
	local.SetResizeActive(true)
	require.NoError(remote.Resize(ptysize.Geometry{Cols: 95, Rows: 26}))
	require.NoError(local.Resize(ptysize.Geometry{Cols: 100, Rows: 30}))

	local.SetResizeActive(false)
	require.NoError(remote.Resize(ptysize.Geometry{Cols: 120, Rows: 40}))

	assert.Equal([]terminalResize{
		{geometry: ptysize.Geometry{Cols: 80, Rows: 24}},
		{geometry: ptysize.Geometry{Cols: 90, Rows: 25}},
		{geometry: ptysize.Geometry{Cols: 100, Rows: 30}},
		{geometry: ptysize.Geometry{Cols: 95, Rows: 26}},
		{geometry: ptysize.Geometry{Cols: 120, Rows: 40}},
	}, pty.resizes())
}

func TestAttachmentResizeOwnerFollowsLatestDeliberateLocalClaim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pty := &fakeRuntimePTY{
		output: make(chan []byte),
		done:   make(chan struct{}),
	}
	s := &session{
		info: SessionInfo{
			Key:         "session-1",
			WorkspaceID: "ws-1",
			Status:      SessionStatusRunning,
		},
		pty:         pty,
		done:        make(chan struct{}),
		outputDone:  make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}

	first, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityLocal,
			ResizeActive:   true,
		},
	)
	require.NoError(err)
	defer first.Close()
	second, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityLocal,
		},
	)
	require.NoError(err)
	defer second.Close()

	firstGeometry := ptysize.Geometry{
		Cols: 100, Rows: 30, PixelWidth: 800, PixelHeight: 480,
	}
	settle, err := first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.True(settle)
	settle, err = first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.True(settle, "an unacknowledged claim must keep requesting settlement")
	first.ResizeSettled()
	settle, err = first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.False(settle)
	secondGeometry := ptysize.Geometry{
		Cols: 120, Rows: 40, PixelWidth: 1080, PixelHeight: 760,
	}
	secondNeedsSettle, err := second.ClaimResize(secondGeometry)
	require.NoError(err)
	require.True(secondNeedsSettle)
	firstGeometry = ptysize.Geometry{
		Cols: 101, Rows: 31, PixelWidth: 808, PixelHeight: 496,
	}
	require.NoError(first.Resize(firstGeometry))
	firstNeedsSettle, err := first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.True(firstNeedsSettle)
	second.ResizeSettled()
	firstNeedsSettle, err = first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.True(firstNeedsSettle, "an older acknowledgement must not settle a newer claim")
	first.ResizeSettled()
	firstNeedsSettle, err = first.ClaimResize(firstGeometry)
	require.NoError(err)
	require.False(firstNeedsSettle)

	assert.Equal([]terminalResize{
		{geometry: ptysize.Geometry{
			Cols: 100, Rows: 30, PixelWidth: 800, PixelHeight: 480,
		}},
		{geometry: ptysize.Geometry{
			Cols: 120, Rows: 40, PixelWidth: 1080, PixelHeight: 760,
		}},
		{geometry: ptysize.Geometry{
			Cols: 101, Rows: 31, PixelWidth: 808, PixelHeight: 496,
		}},
	}, pty.resizes())
}

func TestAttachmentResizeOwnerFallbackRestoresLatestRemainingClaim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pty := &fakeRuntimePTY{
		output: make(chan []byte),
		done:   make(chan struct{}),
	}
	s := &session{
		info: SessionInfo{
			Key:         "session-1",
			WorkspaceID: "ws-1",
			Status:      SessionStatusRunning,
		},
		pty:         pty,
		done:        make(chan struct{}),
		outputDone:  make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}

	first, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityLocal,
			ResizeActive:   true,
		},
	)
	require.NoError(err)
	defer first.Close()
	_, err = first.ClaimResize(ptysize.Geometry{Cols: 80, Rows: 24})
	require.NoError(err)

	second, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityLocal,
			ResizeActive:   true,
		},
	)
	require.NoError(err)
	defer second.Close()
	_, err = second.ClaimResize(ptysize.Geometry{Cols: 90, Rows: 25})
	require.NoError(err)

	third, err := attachToSession(
		s, "ws-1", "session-1", nil,
		AttachSessionOptions{
			ResizePriority: ResizePriorityLocal,
			ResizeActive:   true,
		},
	)
	require.NoError(err)
	_, err = third.ClaimResize(ptysize.Geometry{Cols: 100, Rows: 30})
	require.NoError(err)
	require.NoError(first.Resize(ptysize.Geometry{Cols: 81, Rows: 24}))
	third.Close()

	assert.Equal([]terminalResize{
		{geometry: ptysize.Geometry{Cols: 80, Rows: 24}},
		{geometry: ptysize.Geometry{Cols: 90, Rows: 25}},
		{geometry: ptysize.Geometry{Cols: 100, Rows: 30}},
		{geometry: ptysize.Geometry{Cols: 90, Rows: 25}},
	}, pty.resizes())
}

func TestManagerSubmitInitialMessageWritesEnterAsSeparateKeystrokeAfterPaste(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	pty := &fakeRuntimePTY{
		output: make(chan []byte), done: make(chan struct{}),
	}
	s := &session{
		info: SessionInfo{
			Key: "agent-1", WorkspaceID: "ws-1", Kind: LaunchTargetAgent,
			Status: SessionStatusRunning,
		},
		pty: pty, done: make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	mgr := NewManager(Options{})
	mgr.sessions[s.info.Key] = s

	err := mgr.SubmitInitialMessage(t.Context(), "ws-1", "agent-1", "review this")
	require.ErrorIs(err, ErrBracketedPasteInactive)
	assert.Empty(pty.written())

	s.broadcast([]byte("\x1b[?2004h"))
	submit := func(message, framed string) {
		pty.resetWrites()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		require.NoError(mgr.SubmitInitialMessage(ctx, "ws-1", "agent-1", message))
		pty.mu.Lock()
		writeCalls := slices.Clone(pty.writeCalls)
		pty.mu.Unlock()
		// A terminal UI that collapses a multi-line paste treats bytes arriving
		// in the same chunk as the paste-end marker as part of the paste, so
		// the Enter keystroke must arrive as its own later write.
		assert.Equal([][]byte{[]byte(framed), []byte("\r")}, writeCalls)
	}

	submit("review this", "\x1b[200~review this\x1b[201~")
	submit("first\nsecond", "\x1b[200~first\nsecond\x1b[201~")
}

func TestManagerSubmitInitialMessageClassifiesMissingSessionAsNotWritten(t *testing.T) {
	mgr := NewManager(Options{})

	err := mgr.SubmitInitialMessage(t.Context(), "ws-1", "missing-agent", "review this")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInitialMessageNotWritten)
}

func TestManagerSubmitInitialMessageHonorsContextWithoutHoldingSessionLock(t *testing.T) {
	require := require.New(t)
	writeStarted := make(chan struct{})
	writeRelease := make(chan struct{})
	writeFinished := make(chan struct{})
	pty := &fakeRuntimePTY{
		output: make(chan []byte), done: make(chan struct{}),
		writeStarted: writeStarted, writeRelease: writeRelease, writeFinished: writeFinished,
	}
	s := &session{
		info: SessionInfo{
			Key: "agent-1", WorkspaceID: "ws-1", Kind: LaunchTargetAgent,
			Status: SessionStatusRunning,
		},
		pty: pty, done: make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	mgr := NewManager(Options{})
	mgr.sessions[s.info.Key] = s
	s.broadcast([]byte("\x1b[?2004h"))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- mgr.SubmitInitialMessage(ctx, "ws-1", "agent-1", "review this")
	}()

	<-writeStarted
	snapshotDone := make(chan struct{})
	go func() {
		_ = s.snapshot()
		close(snapshotDone)
	}()
	select {
	case <-snapshotDone:
	case <-time.After(time.Second):
		require.FailNow("session lock remained held during terminal write")
	}
	select {
	case err := <-result:
		require.ErrorIs(err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		require.FailNow("initial message write ignored context deadline")
	}
	close(writeRelease)
	select {
	case <-writeFinished:
	case <-time.After(time.Second):
		require.FailNow("blocked test write did not finish")
	}
}

func TestManagerSubmitInitialMessageStillSendsEnterWhenCallerStopsWaiting(t *testing.T) {
	require := require.New(t)
	writeRelease := make(chan struct{})
	writeObserved := make(chan []byte)
	pty := &fakeRuntimePTY{
		output: make(chan []byte), done: make(chan struct{}),
		writeRelease: writeRelease, writeObserved: writeObserved,
	}
	s := &session{
		info: SessionInfo{
			Key: "agent-1", WorkspaceID: "ws-1", Kind: LaunchTargetAgent,
			Status: SessionStatusRunning,
		},
		pty: pty, done: make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	mgr := NewManager(Options{})
	mgr.sessions[s.info.Key] = s
	s.broadcast([]byte("\x1b[?2004h"))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- mgr.SubmitInitialMessage(ctx, "ws-1", "agent-1", "review this")
	}()
	select {
	case err := <-result:
		require.ErrorIs(err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		require.FailNow("initial message write ignored context deadline")
	}

	// The paste completes after the caller gave up. A paste without its
	// Enter leaves the prompt sitting in the agent's input box, so the
	// keystroke must still follow.
	close(writeRelease)
	observe := func() []byte {
		select {
		case data := <-writeObserved:
			return data
		case <-time.After(2 * time.Second):
			require.FailNow("expected a terminal write after the caller stopped waiting")
			return nil
		}
	}
	require.Equal([]byte("\x1b[200~review this\x1b[201~"), observe())
	require.Equal([]byte("\r"), observe())
}

func TestManagerSubmitInitialMessageSerializesAttachmentInputUntilEnter(t *testing.T) {
	require := require.New(t)
	writeObserved := make(chan []byte)
	pty := &fakeRuntimePTY{
		output: make(chan []byte), done: make(chan struct{}),
		writeObserved: writeObserved,
	}
	s := &session{
		info: SessionInfo{
			Key: "agent-1", WorkspaceID: "ws-1", Kind: LaunchTargetAgent,
			Status: SessionStatusRunning,
		},
		pty: pty, done: make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	mgr := NewManager(Options{})
	mgr.sessions[s.info.Key] = s
	s.broadcast([]byte("\x1b[?2004h"))
	attachment, err := attachToSession(s, "ws-1", "agent-1", nil, AttachSessionOptions{})
	require.NoError(err)
	defer attachment.Close()

	go func() {
		_ = mgr.SubmitInitialMessage(t.Context(), "ws-1", "agent-1", "review this")
	}()
	observe := func() []byte {
		select {
		case data := <-writeObserved:
			return data
		case <-time.After(2 * time.Second):
			require.FailNow("expected a terminal write")
			return nil
		}
	}
	require.Equal([]byte("\x1b[200~review this\x1b[201~"), observe())
	// Typed input arriving during the settle window must not land between
	// the paste and its Enter, where it would alter or submit the prompt.
	go func() { _ = attachment.Write([]byte("x")) }()
	require.Equal([]byte("\r"), observe())
	require.Equal([]byte("x"), observe())
}

func TestManagerSubmitInitialMessageRejectsMultilineWithoutBracketedPaste(t *testing.T) {
	require := require.New(t)
	pty := &fakeRuntimePTY{output: make(chan []byte), done: make(chan struct{})}
	s := &session{
		info: SessionInfo{
			Key: "agent-1", WorkspaceID: "ws-1", Kind: LaunchTargetAgent,
			Status: SessionStatusRunning,
		},
		pty: pty, done: make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	mgr := NewManager(Options{})
	mgr.sessions[s.info.Key] = s

	err := mgr.SubmitInitialMessage(t.Context(), "ws-1", "agent-1", "first\nsecond")
	require.ErrorIs(err, ErrBracketedPasteInactive)
	assert.Empty(t, pty.written())
}

type fakeRuntimePtyOwner struct {
	startedSession      string
	startedCwd          string
	startedCommand      []string
	startedStripEnvVars []string
	startedPTY          *fakeRuntimePTY
	stoppedSession      string
	stoppedSessions     []string
	stopErr             error
	attachErrs          []error
	starts              int
	attaches            int
}

func newFakeRuntimePtyOwner() *fakeRuntimePtyOwner {
	return &fakeRuntimePtyOwner{}
}

func (f *fakeRuntimePtyOwner) HasState(session string) bool {
	return f.startedSession == session
}

func writeLongRunningAttachTmux(t *testing.T) string {
	t.Helper()
	tmuxPath := filepath.Join(t.TempDir(), "tmux")
	require.NoError(t, os.WriteFile(tmuxPath, []byte(`#!/bin/sh
if [ "$1" = "-u" ]; then shift; fi
if [ "$1" = "attach-session" ]; then
  trap 'exit 0' HUP INT TERM
  while :; do sleep 1; done
fi
exit 0
`), 0o755))
	return tmuxPath
}

func (f *fakeRuntimePtyOwner) Start(
	_ context.Context,
	session string,
	cwd string,
	command []string,
	stripEnvVars []string,
	_ map[string]string,
) (ptyownerruntime.PTY, error) {
	f.starts++
	f.startedSession = session
	f.startedCwd = cwd
	f.startedCommand = slices.Clone(command)
	f.startedStripEnvVars = slices.Clone(stripEnvVars)
	f.startedPTY = &fakeRuntimePTY{
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	return f.startedPTY, nil
}

func (f *fakeRuntimePtyOwner) Attach(
	_ context.Context,
	session string,
) (ptyownerruntime.PTY, error) {
	f.attaches++
	if len(f.attachErrs) > 0 {
		err := f.attachErrs[0]
		f.attachErrs = f.attachErrs[1:]
		return nil, err
	}
	if !f.HasState(session) || f.startedPTY == nil {
		return nil, errors.New("missing pty owner state")
	}
	return f.startedPTY, nil
}

func (f *fakeRuntimePtyOwner) Stop(_ context.Context, session string) error {
	f.stoppedSession = session
	f.stoppedSessions = append(f.stoppedSessions, session)
	if f.stopErr != nil {
		return f.stopErr
	}
	if f.startedPTY != nil {
		f.startedPTY.Close()
	}
	return nil
}

type fakeRuntimePTY struct {
	mu            sync.Mutex
	output        chan []byte
	done          chan struct{}
	resizeCalls   []terminalResize
	writes        []byte
	writeCalls    [][]byte
	writeObserved chan []byte
	writeErr      error
	writeStarted  chan struct{}
	writeRelease  chan struct{}
	writeFinished chan struct{}
	writeOnce     sync.Once
}

type terminalResize struct {
	geometry ptysize.Geometry
}

func (f *fakeRuntimePTY) Output() <-chan []byte { return f.output }

func (f *fakeRuntimePTY) Done() <-chan struct{} { return f.done }

func (f *fakeRuntimePTY) Write(data []byte) error {
	f.mu.Lock()
	writeErr := f.writeErr
	writeStarted := f.writeStarted
	writeRelease := f.writeRelease
	writeFinished := f.writeFinished
	f.mu.Unlock()
	if writeErr != nil {
		return writeErr
	}
	if writeStarted != nil {
		f.writeOnce.Do(func() { close(writeStarted) })
	}
	if writeRelease != nil {
		<-writeRelease
	}
	f.mu.Lock()
	f.writes = append(f.writes, data...)
	f.writeCalls = append(f.writeCalls, slices.Clone(data))
	writeObserved := f.writeObserved
	f.mu.Unlock()
	if writeObserved != nil {
		writeObserved <- slices.Clone(data)
	}
	if writeFinished != nil {
		// A handoff may write again after the caller stopped waiting, so
		// signal completion of the blocked write only once.
		f.mu.Lock()
		if f.writeFinished == writeFinished {
			f.writeFinished = nil
			close(writeFinished)
		}
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeRuntimePTY) written() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeRuntimePTY) resetWrites() {
	f.mu.Lock()
	f.writes = nil
	f.writeCalls = nil
	f.mu.Unlock()
}

func (f *fakeRuntimePTY) Resize(geometry ptysize.Geometry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeCalls = append(f.resizeCalls, terminalResize{geometry: geometry})
	return nil
}

func (f *fakeRuntimePTY) resizes() []terminalResize {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resizeCalls)
}

func (f *fakeRuntimePTY) ExitCode() int { return 0 }

func (f *fakeRuntimePTY) Close() {
	select {
	case <-f.done:
	default:
		close(f.output)
		close(f.done)
	}
}

func TestManagerPlainShellConcurrentLaunchesStartIndependentProcesses(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.Background()
	record := filepath.Join(t.TempDir(), "shell-starts")
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
		ShellCommand: helperRecordCommand(record),
		Targets:      []LaunchTarget{plainShellTarget()},
	}))
	t.Cleanup(mgr.Shutdown)

	const launches = 12
	var wg sync.WaitGroup
	errs := make(chan error, launches)
	infos := make(chan SessionInfo, launches)
	cwd := t.TempDir()
	for range launches {
		wg.Go(func() {
			info, err := mgr.Launch(
				ctx, "ws-1", cwd, string(LaunchTargetPlainShell),
			)
			errs <- err
			infos <- info
		})
	}
	wg.Wait()
	close(errs)
	close(infos)

	for err := range errs {
		require.NoError(err)
	}
	keys := make(map[string]bool, launches)
	for info := range infos {
		keys[info.Key] = true
	}
	assert.Len(keys, launches)
	require.Eventually(func() bool {
		data, err := os.ReadFile(record)
		if err != nil {
			return false
		}
		return strings.Count(string(data), "\n") == launches
	}, 2*time.Second, 20*time.Millisecond)
	assert.Len(mgr.ListSessions("ws-1"), launches)
}

func TestManagerShutdownRejectsNewLaunches(t *testing.T) {
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	mgr := NewManager(Options{Targets: []LaunchTarget{
		helperTarget("helper", "sleep"),
	}})
	t.Cleanup(mgr.Shutdown)

	mgr.Shutdown()

	_, err := mgr.Launch(
		context.Background(), "ws-1", t.TempDir(), "helper",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "runtime manager is shut down")
}

func TestSessionBroadcastClosesSlowSubscriber(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	ch := make(chan []byte, 1)
	ch <- []byte("queued")
	s.subscribers[ch] = struct{}{}

	s.broadcast([]byte("new"))

	got := <-ch
	assert := assert.New(t)
	assert.Equal([]byte("queued"), got)
	select {
	case _, ok := <-ch:
		assert.False(ok)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("slow subscriber was not closed")
	}
	s.mu.Lock()
	_, subscribed := s.subscribers[ch]
	s.mu.Unlock()
	assert.False(subscribed)
}

func TestSessionSubscribeReplaysBufferedOutput(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	s.broadcast([]byte("startup-banner\r\n"))
	s.broadcast([]byte("$ "))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Equal("startup-banner\r\n$ ", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive replay")
	}

	s.broadcast([]byte("ls\r\n"))
	select {
	case data := <-ch:
		assert.Equal("ls\r\n", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive new output after replay")
	}
}

func TestSessionSubscribeReplayTruncationPreservesUTF8Boundary(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	// U+201B ends in 0x9b. If truncation retains only that continuation byte,
	// the replay scanner mistakes it for a raw C1 CSI and treats the following
	// text as a bracketed-paste mode change.
	prefix := []byte("\xe2\x80\x9b?2004h")
	output := append(prefix, bytes.Repeat([]byte("x"), maxSessionOutputReplay+2-len(prefix))...)
	s.broadcast(output)

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert := assert.New(t)
		assert.Equal(byte('?'), data[0])
		assert.NotContains(string(data), "\x1b[?2004l")
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribeReplayTruncationPreservesRawC1(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	prefix := []byte("xx\x9b?2004h")
	output := append(prefix, bytes.Repeat([]byte("x"), maxSessionOutputReplay+2-len(prefix))...)
	s.broadcast(output)

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal(t, byte(0x9b), data[0])
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribeRestoresInputModesAfterReplayTruncation(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	modeSequence := "\x1b[?1000;1006;2004h"
	s.broadcast([]byte(modeSequence))
	s.broadcast(bytes.Repeat([]byte("x"), maxSessionOutputReplay+1))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert := assert.New(t)
		assert.Len(data, maxSessionOutputReplay+len(modeSequence))
		assert.Equal(modeSequence, string(data[maxSessionOutputReplay:]))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribeRestoresDisabledInputModesAfterReplayTruncation(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("\x1b[?1003;1004;1016;2004h"))
	s.broadcast([]byte("\x1b[?1000;1004;1006;2004l"))
	s.broadcast(bytes.Repeat([]byte("x"), maxSessionOutputReplay+1))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		resetSequence := "\x1b[?1000;1004;1006;2004l"
		assert := assert.New(t)
		assert.Len(data, maxSessionOutputReplay+len(resetSequence))
		assert.Equal(resetSequence, string(data[maxSessionOutputReplay:]))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribeOverridesStaleReplayInputModes(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("\x1b[?1000l"))
	s.mu.Lock()
	s.outputBuffer = []byte("screen\x1b[?1000h")
	s.mu.Unlock()

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal(t, "screen\x1b[?1000h\x1b[?1000l", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribeDoesNotSynthesizeDuplicateRetainedFocusMode(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("screen\x1b[?1004h\x1b[?1004h"))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal(t, 2, bytes.Count(data, []byte("\x1b[?1004h")))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionSubscribePreservesSplitTerminalData(t *testing.T) {
	tests := []struct {
		name         string
		candidate    string
		continuation string
	}{
		{
			name:         "tracked private mode CSI",
			candidate:    "\x1b[?1004",
			continuation: "h",
		},
		{
			name:         "ordinary CSI",
			candidate:    "\x1b[31",
			continuation: "m",
		},
		{
			name:         "ordinary CSI with executable C0",
			candidate:    "\x1b[31\x07",
			continuation: "m",
		},
		{
			name:         "ordinary CSI with DEL",
			candidate:    "\x1b[31\x7f",
			continuation: "m",
		},
		{
			name:         "Unicode split inside CSI",
			candidate:    "\x1b[?2004\xe2",
			continuation: "\x98\x83h",
		},
		{
			name:         "discarded BOM inside CSI",
			candidate:    "\x1b[?2004\xef\xbb\xbf",
			continuation: "h",
		},
		{
			name:         "discarded invalid scalar inside CSI",
			candidate:    "\x1b[?2004\xed\xa0\x80",
			continuation: "h",
		},
		{
			name:         "OSC",
			candidate:    "\x1b]0;partial title",
			continuation: "\x07",
		},
		{
			name:         "OSC terminated by ST",
			candidate:    "\x1b]0;partial title",
			continuation: "\x1b\\",
		},
		{
			name:         "OSC split inside ST",
			candidate:    "\x1b]0;partial title\x1b",
			continuation: "\\",
		},
		{
			name:         "DCS",
			candidate:    "\x1bP1;2|partial data",
			continuation: "\x1b\\",
		},
		{
			name:         "DCS split inside ST",
			candidate:    "\x1bP1;2|partial data\x1b",
			continuation: "\\",
		},
		{
			name:         "ESC with intermediate",
			candidate:    "\x1b(",
			continuation: "B",
		},
		{
			name:         "UTF-8 C1 CSI",
			candidate:    "\xc2\x9b31",
			continuation: "m",
		},
		{
			name:         "UTF-8 C1 OSC",
			candidate:    "\xc2\x9d0;partial title",
			continuation: "\xc2\x9c",
		},
		{
			name:         "UTF-8 C1 DCS",
			candidate:    "\xc2\x901;2|partial data",
			continuation: "\xc2\x9c",
		},
		{
			name:         "UTF-8 code point",
			candidate:    "\xe2\x82",
			continuation: "\xac",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{subscribers: make(map[chan []byte]struct{})}
			s.broadcast([]byte("\x1b[?1000h"))
			s.broadcast(bytes.Repeat([]byte("x"), maxSessionOutputReplay+1))
			s.broadcast([]byte(tt.candidate))

			ch, cancel := s.subscribeWithReplayBoundary()
			t.Cleanup(cancel)

			assert := assert.New(t)
			select {
			case data := <-ch:
				assert.True(bytes.HasSuffix(data, []byte("\x1b[?1000h"+tt.candidate)))
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive replay")
			}

			s.broadcast([]byte(tt.continuation))
			select {
			case data := <-ch:
				assert.Equal(tt.continuation, string(data))
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive split sequence continuation")
			}
			select {
			case data := <-ch:
				assert.Nil(data)
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive replay boundary")
			}
		})
	}
}

func TestSessionSubscribeReplayBoundaryReadyWithoutPendingTail(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("complete replay"))

	ch, cancel := s.subscribeWithReplayBoundary()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Equal("complete replay", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive replay")
	}
	select {
	case data := <-ch:
		assert.Nil(data)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive replay boundary")
	}
}

func TestSessionSubscribePlacesTransitionsAfterCompleteOrDiscardedC1Data(t *testing.T) {
	tests := []struct {
		name          string
		controlString string
	}{
		{
			name:          "UTF-8 C1 controls",
			controlString: "\xc2\x9d0;complete title\xc2\x9c",
		},
		{
			name:          "raw C1 controls",
			controlString: "\x9d0;complete title\x9c",
		},
		{
			name:          "raw C1 CSI prefix",
			controlString: "\x9b31",
		},
		{
			name:          "raw C1 OSC prefix",
			controlString: "\x9d0;partial title",
		},
		{
			name:          "raw C1 DCS prefix",
			controlString: "\x901;2|partial data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{subscribers: make(map[chan []byte]struct{})}
			s.broadcast([]byte("\x1b[?1000h"))
			s.broadcast(bytes.Repeat([]byte("x"), maxSessionOutputReplay+1))
			s.broadcast([]byte(tt.controlString))

			ch, cancel := s.subscribe()
			t.Cleanup(cancel)

			select {
			case data := <-ch:
				assert.True(t, bytes.HasSuffix(data, []byte(tt.controlString+"\x1b[?1000h")))
			case <-time.After(100 * time.Millisecond):
				assert.Fail(t, "subscriber did not receive replay")
			}
		})
	}
}

func TestSessionSubscribeRestoresInputModesWhileAlternateScreenActive(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("\x1b[?1000;1006h"))
	s.broadcast([]byte("\x1b[?1049h\x1b[Hcodex screen"))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal(t, "\x1b[?1000;1006h", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive input mode replay")
	}
}

func TestSessionSubscribeIgnoresUTF8ContinuationWhileAlternateScreenActive(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("\x1b[?1049h"))
	s.broadcast([]byte("\xd8"))
	s.broadcast([]byte("\x9b"))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Failf(
			t,
			"subscriber received a phantom C1 sequence",
			"unexpected replay: %q",
			string(data),
		)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestSessionSubscribePreservesSplitUTF8C1WhileAlternateScreenActive(t *testing.T) {
	assert := assert.New(t)
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("\x1b[?1049h"))
	s.broadcast([]byte("\xc2"))

	ch, cancel := s.subscribeWithReplayBoundary()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal("\xc2", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive split UTF-8 C1 prefix")
	}
	s.broadcast([]byte("\x9b?1h"))
	select {
	case data := <-ch:
		assert.Equal("\x9b?1h", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive split UTF-8 C1 continuation")
	}
	select {
	case data := <-ch:
		assert.Nil(data)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive replay boundary")
	}
}

func TestSessionSubscribePreservesSplitTerminalDataWhileAlternateScreenActive(t *testing.T) {
	tests := []struct {
		name         string
		candidate    string
		continuation string
	}{
		{
			name:         "tracked input mode",
			candidate:    "\x1b[?2004",
			continuation: "h",
		},
		{
			name:         "ordinary CSI",
			candidate:    "\x1b[31",
			continuation: "m",
		},
		{
			name:         "Unicode split inside CSI",
			candidate:    "\x1b[?2004\xe2",
			continuation: "\x98\x83h",
		},
		{
			name:         "discarded BOM inside CSI",
			candidate:    "\x1b[?2004\xef\xbb\xbf",
			continuation: "h",
		},
		{
			name:         "discarded invalid scalar inside CSI",
			candidate:    "\x1b[?2004\xed\xa0\x80",
			continuation: "h",
		},
		{
			name:         "OSC",
			candidate:    "\x1b]0;partial title",
			continuation: "\x07",
		},
		{
			name:         "OSC terminated by ST",
			candidate:    "\x1b]0;partial title",
			continuation: "\x1b\\",
		},
		{
			name:         "DCS",
			candidate:    "\x1bP1;2|partial data",
			continuation: "\x1b\\",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{subscribers: make(map[chan []byte]struct{})}
			s.broadcast([]byte("\x1b[?1000h"))
			s.broadcast([]byte("\x1b[?1049h"))
			s.broadcast([]byte(tt.candidate))

			ch, cancel := s.subscribeWithReplayBoundary()
			t.Cleanup(cancel)

			assert := assert.New(t)
			select {
			case data := <-ch:
				assert.Equal("\x1b[?1000h"+tt.candidate, string(data))
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive terminal replay")
			}

			s.broadcast([]byte(tt.continuation))
			select {
			case data := <-ch:
				assert.Equal(tt.continuation, string(data))
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive split sequence continuation")
			}
			select {
			case data := <-ch:
				assert.Nil(data)
			case <-time.After(100 * time.Millisecond):
				assert.Fail("subscriber did not receive replay boundary")
			}
		})
	}
}

func TestSessionSubscribeDropsRawC1TailWhileAlternateScreenActive(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
	}{
		{name: "CSI", candidate: "\x9b31"},
		{name: "OSC", candidate: "\x9d0;partial title"},
		{name: "DCS", candidate: "\x901;2|partial data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{subscribers: make(map[chan []byte]struct{})}
			s.broadcast([]byte("\x1b[?1000h"))
			s.broadcast([]byte("\x1b[?1049h"))
			s.broadcast([]byte(tt.candidate))

			ch, cancel := s.subscribe()
			t.Cleanup(cancel)

			select {
			case data := <-ch:
				assert.Equal(t, "\x1b[?1000h", string(data))
			case <-time.After(100 * time.Millisecond):
				assert.Fail(t, "subscriber did not receive terminal replay")
			}
		})
	}
}

func TestSessionSubscribeAfterCloseCombinesReplayAndInputModes(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	modeSequence := "\x1b[?1000;1006h"
	s.broadcast([]byte(modeSequence))
	s.broadcast(bytes.Repeat([]byte("x"), maxSessionOutputReplay+1))
	s.closeSubscribers()

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data, ok := <-ch:
		assert.True(ok)
		assert.Len(data, maxSessionOutputReplay+len(modeSequence))
		assert.Equal(modeSequence, string(data[maxSessionOutputReplay:]))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("expected replay before channel close")
	}
	select {
	case _, ok := <-ch:
		assert.False(ok)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("expected channel to close after replay")
	}
}

func TestSessionSubscribeWithoutInputModesPreservesReplay(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	s.broadcast([]byte("startup-banner\r\n$ "))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	select {
	case data := <-ch:
		assert.Equal(t, "startup-banner\r\n$ ", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "subscriber did not receive replay")
	}
}

func TestSessionInputModeReplayIsSubscriberOnly(t *testing.T) {
	s := &session{subscribers: make(map[chan []byte]struct{})}
	ch, cancel := s.subscribe()
	t.Cleanup(cancel)
	raw := []byte("screen\x1b[?1000;1006h")

	s.broadcast(raw)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Equal(raw, data)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive live output")
	}
	select {
	case data := <-ch:
		assert.Failf("subscriber received synthesized live output", "output: %q", data)
	case <-time.After(25 * time.Millisecond):
	}
	s.mu.Lock()
	replay := bytes.Clone(s.outputBuffer)
	s.mu.Unlock()
	assert.Equal(raw, replay)
}

func TestSessionSubscribeSkipsReplayWhileAlternateScreenActive(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	s.broadcast([]byte("startup-banner\r\n$ "))
	s.broadcast([]byte("\x1b[?1049h\x1b[Hcodex screen"))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Failf(
			"subscriber received alternate screen replay",
			"unexpected replay: %q",
			string(data),
		)
	case <-time.After(25 * time.Millisecond):
	}

	s.broadcast([]byte("\x1b[Hupdated screen"))
	select {
	case data := <-ch:
		assert.Equal("\x1b[Hupdated screen", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive live output")
	}
}

func TestSessionSubscribeReplaysNormalOutputAfterAlternateScreenExit(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	s.broadcast([]byte("startup-banner\r\n$ "))
	s.broadcast([]byte("\x1b[?1049h\x1b[Hcodex screen"))
	s.broadcast([]byte("\x1b[?1049l\r\n$ "))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Equal("\r\n$ ", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive normal replay after exit")
	}
}

func TestSessionAlternateScreenTrackingHandlesSplitEscapeSequences(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	s.broadcast([]byte("startup-banner\r\n$ \x1b[?104"))
	s.broadcast([]byte("9h\x1b[Hcodex screen"))

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data := <-ch:
		assert.Failf(
			"subscriber received split alternate screen replay",
			"unexpected replay: %q",
			string(data),
		)
	case <-time.After(25 * time.Millisecond):
	}

	s.broadcast([]byte("\x1b[?104"))
	s.broadcast([]byte("9l\r\n$ "))
	var live strings.Builder
	select {
	case data := <-ch:
		live.Write(data)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive live split exit prefix")
	}
	select {
	case data := <-ch:
		live.Write(data)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive live split exit suffix")
	}
	assert.Equal("\x1b[?1049l\r\n$ ", live.String())

	ch2, cancel2 := s.subscribe()
	t.Cleanup(cancel2)
	select {
	case data := <-ch2:
		assert.Equal("\r\n$ ", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("subscriber did not receive replay after split exit")
	}
}

func TestSessionSubscribeAfterCloseStillReplays(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	s.broadcast([]byte("hello\r\nbye\r\n"))
	s.closeSubscribers()

	ch, cancel := s.subscribe()
	t.Cleanup(cancel)

	assert := assert.New(t)
	select {
	case data, ok := <-ch:
		assert.True(ok)
		assert.Equal("hello\r\nbye\r\n", string(data))
	case <-time.After(100 * time.Millisecond):
		assert.Fail("expected replay before channel close")
	}
	select {
	case _, ok := <-ch:
		assert.False(ok)
	case <-time.After(100 * time.Millisecond):
		assert.Fail("expected channel to close after replay")
	}
}

func TestSessionOutputBufferIsBounded(t *testing.T) {
	s := &session{
		subscribers: make(map[chan []byte]struct{}),
	}
	chunk := make([]byte, 8*1024)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for range 12 {
		s.broadcast(chunk)
	}

	s.mu.Lock()
	bufLen := len(s.outputBuffer)
	s.mu.Unlock()
	assert.New(t).LessOrEqual(bufLen, maxSessionOutputReplay)
}

func TestManagerStopWorkspaceStopsAllSessions(t *testing.T) {
	requirePTYAvailable(t)
	t.Setenv("KENN_FORGE_LOCALRUNTIME_HELPER", "1")

	require := require.New(t)
	assert := assert.New(t)

	ctx := context.Background()
	mgr := NewManager(withTestPtyOwnerRuntime(t, Options{
		Targets: []LaunchTarget{
			helperTarget("agent-a", "sleep"),
			helperTarget("agent-b", "sleep"),
			plainShellTarget(),
		},
		ShellCommand: helperCommand("sleep"),
	}))
	t.Cleanup(mgr.Shutdown)

	_, err := mgr.Launch(ctx, "ws-1", t.TempDir(), "agent-a")
	require.NoError(err)
	_, err = mgr.Launch(ctx, "ws-1", t.TempDir(), "agent-b")
	require.NoError(err)
	_, err = mgr.Launch(ctx, "ws-1", t.TempDir(), string(LaunchTargetPlainShell))
	require.NoError(err)

	// A second workspace's sessions must survive.
	_, err = mgr.Launch(ctx, "ws-2", t.TempDir(), "agent-a")
	require.NoError(err)

	mgr.StopWorkspace(ctx, "ws-1")

	assert.Empty(mgr.ListSessions("ws-1"))
	assert.Len(mgr.ListSessions("ws-2"), 1)
}

func plainShellTarget() LaunchTarget {
	return LaunchTarget{
		Key:       string(LaunchTargetPlainShell),
		Label:     "Shell",
		Kind:      LaunchTargetPlainShell,
		Source:    "system",
		Available: true,
	}
}

func helperTarget(key, mode string) LaunchTarget {
	return LaunchTarget{
		Key: key, Label: key, Kind: LaunchTargetAgent,
		Source: "config", Command: helperCommand(mode),
		Available: true,
	}
}

func helperRecordCommand(record string) []string {
	return []string{
		os.Args[0],
		"-test.run=TestHelperProcess",
		"--",
		"sleep-record",
		record,
	}
}

func helperCommand(mode string) []string {
	return []string{
		os.Args[0],
		"-test.run=TestHelperProcess",
		"--",
		mode,
	}
}

// TestResolveExecutableRejectsRelativePaths ensures startSession
// refuses commands that would resolve inside the workspace worktree
// (PR-controlled content). Absolute paths and PATH-resolvable
// names are accepted; relative names with separators are rejected.
func TestResolveExecutableRejectsRelativePaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Absolute path: pass through unchanged.
	absCommand := "/usr/local/bin/codex"
	if runtime.GOOS == "windows" {
		absCommand = `C:\tools\codex.exe`
	}
	got, err := resolveExecutable(absCommand)
	require.NoError(err)
	assert.Equal(absCommand, got)

	// PATH-resolvable: returns the full path.
	exeName, exePath := writeFakeRuntimeTool(t, t.TempDir(), "fake-runtime-tool")
	t.Setenv("PATH", filepath.Dir(exePath))
	got, err = resolveExecutable(exeName)
	require.NoError(err)
	assert.True(filepath.IsAbs(got), "expected absolute path, got %q", got)
	if runtime.GOOS == "windows" {
		assert.True(
			strings.EqualFold(exePath, got),
			"expected %q, got %q",
			exePath,
			got,
		)
	} else {
		assert.Equal(exePath, got)
	}

	// Relative paths must be rejected.
	for _, rel := range []string{
		"./agent",
		"../scripts/codex",
		"scripts/codex",
		"a/b",
	} {
		_, err := resolveExecutable(rel)
		require.Error(err, "expected error for %q", rel)
		assert.Contains(err.Error(), "absolute path")
	}

	// Empty name.
	_, err = resolveExecutable("")
	require.Error(err)

	// Bare name not on PATH should surface a LookPath error.
	_, err = resolveExecutable(
		"kenn-forge-localruntime-bogus-name-zzz",
	)
	require.Error(err)
}

func TestResolveExecutableForcesAbsoluteFromRelativePATH(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(os.MkdirAll(binDir, 0o755))
	exeName, exe := writeFakeRuntimeTool(t, binDir, "fake-runtime-tool")

	t.Chdir(dir)
	t.Setenv("PATH", "bin")
	// Recent Go versions wrap LookPath results from relative PATH
	// entries with ErrDot. With execerrdot=0 they're returned with
	// no error — that's exactly the case where the worktree-cwd
	// rebinding is dangerous, so verify the absolute fallback runs.
	t.Setenv("GODEBUG", "execerrdot=0")

	got, err := resolveExecutable(exeName)
	require.NoError(err)
	assert.True(
		filepath.IsAbs(got),
		"expected absolute path, got %q (relative would resolve "+
			"inside cmd.Dir = the workspace worktree)",
		got,
	)
	if runtime.GOOS == "windows" {
		assert.True(
			strings.EqualFold(exe, got),
			"expected %q, got %q",
			exe,
			got,
		)
	} else {
		assert.Equal(exe, got)
	}
}

func writeFakeRuntimeTool(
	t *testing.T,
	dir string,
	name string,
) (string, string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
		path := filepath.Join(dir, name+".cmd")
		require.NoError(t, os.WriteFile(
			path,
			[]byte("@echo off\r\nexit /b 0\r\n"),
			0o755,
		))
		return name, path
	}
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(
		path,
		[]byte("#!/bin/sh\nexit 0\n"),
		0o755,
	))
	return name, path
}

// TestSessionEnvironmentStripsCredentials verifies that the
// environment passed to runtime sessions has GitHub-token-shaped
// variables removed so that launched agents cannot exfiltrate
// the maintainer's credentials.
func TestSessionEnvironmentStripsCredentials(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"KENN_FORGE_GITHUB_TOKEN=secret-1",
		"GITHUB_TOKEN=secret-2",
		"GH_TOKEN=secret-3",
		"GITHUB_PAT=secret-4",
		"GH_PAT=secret-5",
		"GITHUB_ENTERPRISE_TOKEN=secret-6",
		"GH_ENTERPRISE_TOKEN=secret-7",
		"GITHUB_TOKEN_GHE=secret-8",
		"MAINTAINER_PERSONAL_GH_PAT=secret-9",
		"NOTSECRET=ok",
	}
	out := sessionEnvironment(in, []string{
		"MAINTAINER_PERSONAL_GH_PAT",
	})

	require.Contains(out, "PATH=/usr/bin")
	require.Contains(out, "HOME=/home/me")
	require.Contains(out, "NOTSECRET=ok")

	for _, kv := range out {
		assert.NotContains(
			kv, "secret-",
			"credential leaked through sessionEnvironment: %q", kv,
		)
	}
}

func TestSessionEnvironmentStripsConfiguredTokenEnv(t *testing.T) {
	require := require.New(t)
	in := []string{
		"PATH=/usr/bin",
		"WORK_GH_BOT_TOKEN=top-secret",
	}
	out := sessionEnvironment(in, []string{"WORK_GH_BOT_TOKEN"})
	require.Contains(out, "PATH=/usr/bin")
	for _, kv := range out {
		require.NotContains(
			kv, "top-secret",
			"configured token env leaked: %q", kv,
		)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("KENN_FORGE_LOCALRUNTIME_HELPER") != "1" {
		return
	}
	args := os.Args
	helperArgs := args[len(args)-1:]
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			helperArgs = args[i+1:]
			break
		}
	}
	mode := helperArgs[0]
	switch mode {
	case "sleep":
		time.Sleep(time.Hour)
	case "sleep-record":
		if len(helperArgs) < 2 {
			os.Exit(2)
		}
		f, err := os.OpenFile(
			helperArgs[1],
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0o644,
		)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Close()
		time.Sleep(time.Hour)
	case "spawn-child":
		if len(helperArgs) < 2 {
			os.Exit(2)
		}
		child := procutil.Command(
			os.Args[0], "-test.run=TestHelperProcess", "--", "sleep",
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		f, err := os.OpenFile(
			helperArgs[1],
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0o644,
		)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(
			f, "%d\n%d\n", os.Getpid(), child.Process.Pid,
		)
		_ = f.Close()
		time.Sleep(time.Hour)
	case "exit":
		os.Exit(3)
	default:
		os.Exit(2)
	}
}
