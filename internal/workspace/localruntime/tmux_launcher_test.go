package localruntime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTmuxLauncherAgentOperationsKeepEnvValuesOutOfArgv(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("XDG_RUNTIME_DIR", "argv-visible-value")
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "secret-value")

	paneEnv := tmuxAgentEnvPolicy.paneEnvironment(
		os.Environ(), []string{"/bin/sh", "-lc", "sleep 10"}, nil,
	)
	launcher := tmuxLauncher{
		TmuxCommand: []string{"/usr/bin/tmux"},
		Session:     "kenn-forge-test",
		CWD:         "/tmp/work tree",
		Pane:        paneEnv,
		OwnerMarker: "kenn-forge:test-owner",
	}

	paneCommand, cleanup, err := launcher.newSessionPaneCommand()
	require.NoError(t, err)
	t.Cleanup(cleanup)
	scriptText := requireTmuxPaneScript(t, paneCommand)
	newSession := launcher.newSessionCommand(paneCommand)
	newSessionText := strings.Join(newSession, "\n")
	paneCommandArg := ""
	for i, arg := range newSession {
		if arg == ";" && i > 0 {
			paneCommandArg = newSession[i-1]
			break
		}
	}
	require.NotEmpty(t, paneCommandArg)

	assert.Equal("new-session", newSession[1])
	assert.Contains(newSession, "-E")
	assert.NotContains(newSession, "-e")
	assert.Equal(paneCommand, paneCommandArg)
	assert.Contains(newSession, "-c")
	assert.Contains(newSession, "/tmp/work tree")
	assert.Contains(scriptText, "exec env -i")
	assert.Contains(newSession, ";")
	assert.Contains(newSession, "set-option")
	assert.Contains(newSession, "@forge_owner")
	assert.Contains(newSession, "kenn-forge:test-owner")
	assert.Contains(scriptText, `XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR-}"`)
	assert.Contains(scriptText, "__kenn_forge_env_file=")
	assert.Contains(scriptText, "__kenn_forge_script_file=")
	assert.Contains(scriptText, "trap __kenn_forge_cleanup_tmux_files EXIT")
	assert.Contains(scriptText, "trap - EXIT")
	assert.NotContains(newSessionText, "argv-visible-value")
	assert.NotContains(newSessionText, "secret-value")
	assert.NotContains(scriptText, "argv-visible-value")
	assert.NotContains(scriptText, "secret-value")
}

func TestTmuxPaneEnvironmentExtraReplacesInheritedValue(t *testing.T) {
	pane := tmuxAgentEnvPolicy.paneEnvironmentWithExtra(
		[]string{"PATH=/usr/bin", "KENN_FORGE_RUNTIME_SESSION_KEY=parent"},
		[]string{"/bin/sh"}, nil,
		map[string]string{"KENN_FORGE_RUNTIME_SESSION_KEY": "child"},
	)

	assert.Contains(t, pane.commandEnv, "KENN_FORGE_RUNTIME_SESSION_KEY=child")
	assert.NotContains(t, pane.commandEnv, "KENN_FORGE_RUNTIME_SESSION_KEY=parent")
}

func TestTmuxLauncherCanHideStatusOnNewSessions(t *testing.T) {
	assert := assert.New(t)

	paneEnv := tmuxAgentEnvPolicy.paneEnvironment(
		os.Environ(), []string{"/bin/sh", "-lc", "sleep 10"}, nil,
	)
	launcher := tmuxLauncher{
		TmuxCommand: []string{"/usr/bin/tmux"},
		Session:     "kenn-forge-test",
		CWD:         "/tmp/work tree",
		Pane:        paneEnv,
		OwnerMarker: "kenn-forge:test-owner",
		HideStatus:  true,
	}

	paneCommand, cleanup, err := launcher.newSessionPaneCommand()
	require.NoError(t, err)
	t.Cleanup(cleanup)
	newSession := launcher.newSessionCommand(paneCommand)

	hideStatus := launcher.hideStatusCommand()

	assert.NotContains(newSession, "status")
	assert.NotContains(newSession, "off")
	assert.True(containsArgvSequence(hideStatus, []string{
		"set-option", "-q", "-t", "kenn-forge-test", "status", "off",
	}))
	assert.NotContains(launcher.attachSessionCommand(), "status")
}

func TestTmuxLauncherEnablesPassthroughPerPane(t *testing.T) {
	launcher := tmuxLauncher{
		TmuxCommand: []string{"/usr/bin/tmux", "-L", "kenn-forge-test"},
		Session:     "kenn-forge-test",
		Graphics:    true,
	}

	assert.Equal(t,
		[]string{
			"/usr/bin/tmux", "-L", "kenn-forge-test",
			"set-option", "-q", "-p", "-t", "kenn-forge-test",
			"allow-passthrough", "on",
		},
		launcher.passthroughCommand(),
	)
}

func TestTmuxLauncherDisablesGraphicsOnDedicatedServer(t *testing.T) {
	launcher := tmuxLauncher{
		TmuxCommand:     []string{"/usr/bin/tmux", "-L", "kenn-forge-test"},
		Session:         "kenn-forge-test",
		ConfigureServer: true,
	}

	assert.Equal(t, []string{
		"/usr/bin/tmux", "-L", "kenn-forge-test",
		"set-option", "-q", "-g", "allow-passthrough", "off",
	}, launcher.globalPassthroughCommand())
	assert.Equal(t, []string{
		"/usr/bin/tmux", "-L", "kenn-forge-test",
		"set-option", "-q", "-s", "-u", "terminal-features[100]",
	}, launcher.sixelCommand())
	assert.Equal(t, []string{
		"/usr/bin/tmux", "-L", "kenn-forge-test",
		"set-option", "-q", "-p", "-t", "kenn-forge-test",
		"allow-passthrough", "off",
	}, launcher.passthroughCommand())
}

func TestTmuxLauncherConfiguresGlobalMouse(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		value   string
	}{
		{name: "enabled", enabled: true, value: "on"},
		{name: "disabled", enabled: false, value: "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := tmuxLauncher{
				TmuxCommand: []string{"/usr/bin/tmux", "-L", "kenn-forge-test"},
				Session:     "kenn-forge-test",
				TmuxMouse:   test.enabled,
			}
			assert.Equal(t,
				[]string{
					"/usr/bin/tmux", "-L", "kenn-forge-test",
					"set-option", "-q", "-g",
					"mouse", test.value,
				},
				launcher.tmuxMouseCommand(),
			)
		})
	}
}

func TestTmuxLauncherAttachForcesUTF8(t *testing.T) {
	launcher := tmuxLauncher{
		TmuxCommand: []string{"/usr/bin/tmux", "-L", "kenn-forge-test"},
		Session:     "kenn-forge-test",
	}

	assert.Equal(t,
		[]string{
			"/usr/bin/tmux", "-L", "kenn-forge-test",
			"-u", "attach-session", "-E", "-t", "kenn-forge-test",
		},
		launcher.attachSessionCommand(),
	)
}

func TestTmuxLauncherCleansUpWhenHideStatusFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	created := filepath.Join(dir, "created")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		"TMUX_RECORD="+shellquote.Join(record)+"\n"+
		"TMUX_CREATED="+shellquote.Join(created)+"\n"+
		"TMUX_EXISTING_OWNER='kenn-forge:test-owner'\n"+
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"
case "$1" in
  has-session)
    if [ -f "$TMUX_CREATED" ]; then exit 0; fi
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session)
    : > "$TMUX_CREATED"
    previous=""
    for arg in "$@"; do
      if [ "$previous" = "status" ] && [ "$arg" = "off" ]; then
        echo "status update failed" >&2
        exit 1
      fi
      previous="$arg"
    done
    exit 0
    ;;
  show-options)
    printf '%s\n' "$TMUX_EXISTING_OWNER"
    exit 0
    ;;
  set-option)
    if [ "$5" = "status" ] && [ "$6" = "off" ]; then
      echo "status update failed" >&2
      exit 1
    fi
    exit 0
    ;;
  kill-session)
    rm -f "$TMUX_CREATED"
    exit 0
    ;;
  attach-session)
    exit 0
    ;;
esac
exit 0
`), 0o755))

	launcher := tmuxLauncher{
		TmuxCommand: []string{tmuxPath},
		Session:     "kenn-forge-test",
		Pane: tmuxPaneEnvironment{
			paneCommand: "exec /bin/sh",
			keys:        []string{"PATH", "TERM"},
			commandEnv:  os.Environ(),
		},
		OwnerMarker: "kenn-forge:test-owner",
		HideStatus:  true,
	}

	_, err := launcher.prepare(context.Background())

	require.Error(err)
	assert.Contains(err.Error(), "hide tmux status")
	records := readNullArgvRecord(t, record)
	assert.Contains(records, []string{
		"kill-session", "-t", "kenn-forge-test",
	})
	assert.NotContains(records, []string{
		"-u", "attach-session", "-E", "-t", "kenn-forge-test",
	})
	assert.NoFileExists(created)
}

func TestTmuxLauncherCleansUpCreatedSessionAfterLaunchContextCancellation(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	createdSessionState := filepath.Join(dir, "created-session")
	statusStarted := filepath.Join(dir, "status-started")
	tmuxPath := filepath.Join(dir, "tmux")
	statusRelease := filepath.Join(dir, "status-release")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		"TMUX_RECORD="+shellquote.Join(record)+"\n"+
		"TMUX_CREATED="+shellquote.Join(createdSessionState)+"\n"+
		"TMUX_STATUS_STARTED="+shellquote.Join(statusStarted)+"\n"+
		"TMUX_STATUS_RELEASE="+shellquote.Join(statusRelease)+"\n"+
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"
case "$1" in
  has-session)
    if [ -f "$TMUX_CREATED" ]; then exit 0; fi
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session)
    : > "$TMUX_CREATED"
    exit 0
    ;;
  set-option)
    if [ "$5" = "status" ] && [ "$6" = "off" ]; then
      : > "$TMUX_STATUS_STARTED"
      while [ ! -f "$TMUX_STATUS_RELEASE" ]; do
        sleep 0.01
      done
    fi
    exit 0
    ;;
  kill-session)
    rm -f "$TMUX_CREATED"
    exit 0
    ;;
esac
exit 0
`), 0o755))

	launcher := tmuxLauncher{
		TmuxCommand: []string{tmuxPath},
		Session:     "kenn-forge-test",
		Pane: tmuxPaneEnvironment{
			paneCommand: "exec /bin/sh",
			keys:        []string{"PATH", "TERM"},
			commandEnv: []string{
				"PATH=" + os.Getenv("PATH"),
				"TERM=xterm-256color",
			},
		},
		HideStatus: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := launcher.prepare(ctx)
		errCh <- err
	}()

	require.Eventually(func() bool {
		_, err := os.Stat(statusStarted)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		require.FailNow("tmux launcher did not return after cancellation")
	}

	require.Error(err)
	assert.Contains(err.Error(), "hide tmux status")
	assert.NoFileExists(createdSessionState)
	assert.Contains(readNullArgvRecord(t, record), []string{
		"kill-session", "-t", launcher.Session,
	})
}

func TestTmuxLauncherDoesNotKillSessionAfterNewSessionError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		"TMUX_RECORD="+shellquote.Join(record)+"\n"+
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"
case "$1" in
  has-session)
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session)
    echo "new session failed" >&2
    exit 1
    ;;
  kill-session)
    exit 0
    ;;
esac
exit 0
`), 0o755))

	launcher := tmuxLauncher{
		TmuxCommand: []string{tmuxPath},
		Session:     "kenn-forge-test",
		Pane: tmuxPaneEnvironment{
			paneCommand: "exec /bin/sh",
			keys:        []string{"PATH", "TERM"},
			commandEnv: []string{
				"PATH=" + os.Getenv("PATH"),
				"TERM=xterm-256color",
			},
		},
	}

	_, err := launcher.prepare(context.Background())

	require.Error(err)
	assert.Contains(err.Error(), "tmux new-session")
	assert.NotContains(readNullArgvRecord(t, record), []string{
		"kill-session", "-t", launcher.Session,
	})
}

func TestTmuxLauncherShellPolicyPreservesCustomEnvByKey(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("KENN_FORGE_TEST_CUSTOM_SHELL_ENV", "custom-visible-value")

	shellKeys := tmuxShellEnvPolicy.keys(nil)
	agentKeys := tmuxAgentEnvPolicy.keys(nil)

	assert.Contains(shellKeys, "KENN_FORGE_TEST_CUSTOM_SHELL_ENV")
	assert.NotContains(agentKeys, "KENN_FORGE_TEST_CUSTOM_SHELL_ENV")

	paneCommand := tmuxShellEnvPolicy.paneEnvironment(
		os.Environ(), []string{"/bin/sh"}, nil,
	).paneCommand
	assert.Contains(
		paneCommand,
		`KENN_FORGE_TEST_CUSTOM_SHELL_ENV="${KENN_FORGE_TEST_CUSTOM_SHELL_ENV-}"`,
	)
	assert.NotContains(paneCommand, "custom-visible-value")
}

func TestTmuxLauncherShellPolicyDefaultsMacOSCharacterLocale(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		env     []string
		want    string
		wantSet bool
	}{
		{
			name:    "macOS without a locale",
			goos:    "darwin",
			env:     []string{"PATH=/usr/bin"},
			want:    "LC_CTYPE=UTF-8",
			wantSet: true,
		},
		{
			name: "macOS with empty locale variables",
			goos: "darwin",
			env: []string{
				"PATH=/usr/bin", "LANG=", "LC_ALL=", "LC_CTYPE=",
			},
			want:    "LC_CTYPE=UTF-8",
			wantSet: true,
		},
		{
			name:    "macOS with LANG",
			goos:    "darwin",
			env:     []string{"PATH=/usr/bin", "LANG=en_US.UTF-8"},
			want:    "LC_CTYPE=UTF-8",
			wantSet: false,
		},
		{
			name:    "macOS with LC_ALL",
			goos:    "darwin",
			env:     []string{"PATH=/usr/bin", "LC_ALL=C"},
			want:    "LC_CTYPE=UTF-8",
			wantSet: false,
		},
		{
			name:    "macOS with LC_CTYPE",
			goos:    "darwin",
			env:     []string{"PATH=/usr/bin", "LC_CTYPE=C"},
			want:    "LC_CTYPE=UTF-8",
			wantSet: false,
		},
		{
			name:    "Linux without a locale",
			goos:    "linux",
			env:     []string{"PATH=/usr/bin"},
			want:    "LC_CTYPE=UTF-8",
			wantSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tmuxShellEnvPolicy.environmentForOS(tt.env, nil, tt.goos)

			if tt.wantSet {
				assert.Contains(t, env, tt.want)
				return
			}
			assert.NotContains(t, env, tt.want)
		})
	}
}

func TestTmuxLauncherRejectsUnownedExistingSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "tmux-record")
	tmuxPath := filepath.Join(dir, "tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		"TMUX_RECORD="+shellquote.Join(record)+"\n"+
		"TMUX_EXISTING_OWNER='other-owner'\n"+
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"
case "$1" in
  has-session)
    exit 0
    ;;
  show-options)
    printf '%s\n' "$TMUX_EXISTING_OWNER"
    exit 0
    ;;
  attach-session)
    exit 0
    ;;
esac
exit 0
`), 0o755))

	launcher := tmuxLauncher{
		TmuxCommand: []string{tmuxPath},
		Session:     "kenn-forge-test",
		Pane: tmuxPaneEnvironment{
			paneCommand: "exec /bin/sh",
			keys:        []string{"PATH", "TERM"},
			commandEnv:  os.Environ(),
		},
		OwnerMarker: "kenn-forge:test-owner",
	}

	_, err := launcher.prepare(context.Background())

	require.Error(err)
	records := readNullArgvRecord(t, record)
	assert.Contains(records, []string{
		"has-session", "-t", "kenn-forge-test",
	})
	assert.Contains(records, []string{
		"show-options", "-qv", "-t", "kenn-forge-test", "@forge_owner",
	})
	assert.NotContains(records, []string{
		"-u", "attach-session", "-E", "-t", "kenn-forge-test",
	})
	assert.NotContains(records, []string{
		"new-session", "-e", "PATH", "-e", "TERM",
		"-d", "-s", "kenn-forge-test", "exec /bin/sh",
	})
}

func readNullArgvRecord(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	if len(data) == 0 {
		return nil
	}
	fields := strings.Split(string(data), "\x00")
	var records [][]string
	for i := 0; i < len(fields); {
		if fields[i] == "" && i == len(fields)-1 {
			break
		}
		count, err := strconv.Atoi(fields[i])
		require.NoError(t, err)
		i++
		require.LessOrEqual(t, i+count, len(fields))
		records = append(records, fields[i:i+count])
		i += count
	}
	return records
}

func requireNewSessionPaneScript(t *testing.T, newSession []string) string {
	t.Helper()
	require.NotEmpty(t, newSession)
	command := newSession[len(newSession)-1]
	for i, arg := range newSession {
		if arg == ";" && i > 0 {
			command = newSession[i-1]
			break
		}
	}
	return requireTmuxPaneScript(t, command)
}

func requireTmuxPaneScript(t *testing.T, command string) string {
	t.Helper()
	words, err := shellquote.Split(command)
	require.NoError(t, err)
	require.Len(t, words, 2)
	require.Equal(t, "/bin/sh", words[0])
	data, err := os.ReadFile(words[1])
	require.NoError(t, err)
	return string(data)
}

func containsArgvSequence(argv []string, sequence []string) bool {
	if len(sequence) == 0 {
		return true
	}
	for i := 0; i+len(sequence) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

// TestTmuxAgentPolicyPreservesTmuxTmpdirInClientEnv pins socket routing:
// tmux resolves -L sockets under TMUX_TMPDIR, so the launcher's tmux
// client must see the daemon's value or agent sessions land on a
// different tmux server than the manager-owned base sessions, breaking
// attachment and orphan reaping.
func TestTmuxAgentPolicyPreservesTmuxTmpdirInClientEnv(t *testing.T) {
	assert := assert.New(t)
	env := []string{
		"PATH=/usr/bin",
		"TMUX_TMPDIR=/private/socket-dir",
		"GITHUB_TOKEN=agent-secret",
	}
	pane := tmuxAgentEnvPolicy.paneEnvironment(env, []string{"agent"}, nil)
	joined := strings.Join(pane.commandEnv, "\n")
	assert.Contains(joined, "TMUX_TMPDIR=/private/socket-dir",
		"tmux client must resolve the same socket directory as the daemon")
	assert.NotContains(joined, "GITHUB_TOKEN",
		"credential stripping must survive the socket-routing fix")
	client := TmuxClientEnvironment(
		append(env,
			"TMUX_GITHUB_TOKEN=sneaky",
			"XDG_CONFIGURED_TOKEN=xdg-sneaky",
			"XDG_RUNTIME_DIR=/run/user/1000",
		),
		[]string{"XDG_CONFIGURED_TOKEN"},
	)
	clientJoined := strings.Join(client, "\n")
	assert.Contains(clientJoined, "TMUX_TMPDIR=/private/socket-dir")
	assert.Contains(clientJoined, "XDG_RUNTIME_DIR=/run/user/1000")
	assert.NotContains(clientJoined, "TMUX_GITHUB_TOKEN",
		"the allowlist must name exact variables, not a TMUX_ wildcard a secret could hide under")
	assert.NotContains(clientJoined, "XDG_CONFIGURED_TOKEN",
		"configured token names must be stripped even under allowlisted prefixes")
	undeclared := TmuxClientEnvironment(
		append(env,
			"XDG_API_TOKEN=undeclared-xdg-secret",
			"LC_SECRET_TOKEN=undeclared-lc-secret",
			"XDG_RUNTIME_DIR=/run/user/1000",
			"LC_ALL=en_US.UTF-8",
		),
		nil,
	)
	undeclaredJoined := strings.Join(undeclared, "\n")
	assert.Contains(undeclaredJoined, "XDG_RUNTIME_DIR=/run/user/1000")
	assert.Contains(undeclaredJoined, "LC_ALL=en_US.UTF-8")
	assert.NotContains(undeclaredJoined, "XDG_API_TOKEN",
		"the allowlist must name exact XDG variables, not a prefix a secret could hide under")
	assert.NotContains(undeclaredJoined, "LC_SECRET_TOKEN",
		"the allowlist must name exact locale variables, not a prefix a secret could hide under")
}

// TestTmuxAttachSessionCommandDisablesUpdateEnvironment pins -E on every
// daemon-side attach: a pane can widen the server's update-environment,
// and without -E the next attach would copy the attach client's
// variables into the session environment where panes read them back.
func TestTmuxAttachSessionCommandDisablesUpdateEnvironment(t *testing.T) {
	assert.Equal(t,
		[]string{"tmux", "-L", "sock", "-u", "attach-session", "-E", "-t", "kenn-forge-test"},
		tmuxAttachSessionCommand(
			[]string{"tmux", "-L", "sock"}, "kenn-forge-test",
		),
	)
}

// TestTmuxPaneEnvironmentExtraNeverReachesClientEnv pins the client/pane
// split: launch extras come from API request bodies, so they may only
// reach the pane via the env-file handoff. The tmux client environment
// must derive from the pre-extras policy env, or an extra such as
// TMUX_TMPDIR could steer new-session to a different socket than the
// daemon's own management clients.
func TestTmuxPaneEnvironmentExtraNeverReachesClientEnv(t *testing.T) {
	assert := assert.New(t)
	pane := tmuxAgentEnvPolicy.paneEnvironmentWithExtra(
		[]string{"PATH=/usr/bin", "TMUX_TMPDIR=/daemon/sockets"},
		[]string{"/bin/sh"}, nil,
		map[string]string{
			"TMUX_TMPDIR": "/attacker/sockets",
			"HOME":        "/attacker/home",
		},
	)

	clientJoined := strings.Join(pane.clientEnv, "\n")
	assert.Contains(clientJoined, "TMUX_TMPDIR=/daemon/sockets")
	assert.NotContains(clientJoined, "/attacker/sockets")
	assert.NotContains(clientJoined, "/attacker/home")
	// The pane still receives the extras through the handoff env.
	commandJoined := strings.Join(pane.commandEnv, "\n")
	assert.Contains(commandJoined, "TMUX_TMPDIR=/attacker/sockets")
	assert.Contains(commandJoined, "HOME=/attacker/home")
}

// TestShouldStripSessionVarFold pins Windows semantics: environment
// variable names resolve case-insensitively there, so github_token is
// the same credential as GITHUB_TOKEN.
func TestShouldStripSessionVarFold(t *testing.T) {
	assert := assert.New(t)
	assert.True(shouldStripSessionVarFold("github_token", nil, true))
	assert.True(shouldStripSessionVarFold("Kata_Auth_Token", nil, true))
	assert.True(shouldStripSessionVarFold("my_token", []string{"MY_TOKEN"}, true))
	assert.False(shouldStripSessionVarFold("github_token", nil, false))
	assert.True(shouldStripSessionVarFold("GITHUB_TOKEN", nil, false))
}

// TestTmuxEnvironmentKeysRejectsReservedHandoffNames pins that
// handoff-internal variables can never be sourced from the env file:
// the cleanup trap removes the paths they hold, so an API-supplied
// override could aim rm at caller-selected files.
func TestTmuxEnvironmentKeysRejectsReservedHandoffNames(t *testing.T) {
	pane := tmuxAgentEnvPolicy.paneEnvironmentWithExtra(
		[]string{"PATH=/usr/bin"},
		[]string{"/bin/sh"}, nil,
		map[string]string{
			"__kenn_forge_env_file":    "/victim/path",
			"__kenn_forge_script_file": "/victim/script",
		},
	)
	assert := assert.New(t)
	assert.NotContains(pane.keys, "__kenn_forge_env_file")
	assert.NotContains(pane.keys, "__kenn_forge_script_file")
	assert.NotContains(pane.paneCommand, "__kenn_forge_env_file=")
}

// TestShouldAllowTmuxSessionVarFold pins admission casing: exact names
// on case-sensitive platforms, folded on Windows where environment
// names resolve case-insensitively.
func TestShouldAllowTmuxSessionVarFold(t *testing.T) {
	assert := assert.New(t)
	assert.True(shouldAllowTmuxSessionVarFold("EDITOR", false))
	assert.False(shouldAllowTmuxSessionVarFold("editor", false),
		"a Unix variable named editor is unrelated to EDITOR")
	assert.False(shouldAllowTmuxSessionVarFold("path", false))
	assert.True(shouldAllowTmuxSessionVarFold("Path", true),
		"Windows resolves Path as PATH")
	assert.True(shouldAllowTmuxSessionVarFold("editor", true))
}
