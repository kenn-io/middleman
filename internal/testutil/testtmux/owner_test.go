package testtmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/testutil/testsignal"
)

func TestMain(m *testing.M) {
	if code, ok := CommandWrapperExitCode(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func requireTmux(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("real tmux tests require Unix")
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux is unavailable: %v", err)
	}
	return path
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "kf-tmux-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func startTmuxServer(
	t *testing.T,
	tmuxPath string,
	command []string,
) int {
	t.Helper()
	args := append(append([]string(nil), command[1:]...),
		"new-session", "-d", "-s", "fixture", "sleep 30",
	)
	output, err := procutil.Command(command[0], args...).CombinedOutput()
	require.NoError(t, err, string(output))
	pidArgs := append(append([]string(nil), command[1:]...),
		"display-message", "-p", "#{pid}",
	)
	output, err = procutil.Command(tmuxPath, pidArgs...).CombinedOutput()
	require.NoError(t, err, string(output))
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	return pid
}

func processGone(pid int) bool {
	command := procutil.Command("/bin/kill", "-0", strconv.Itoa(pid))
	return command.Run() != nil
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Fail(t, "process survived cleanup", "pid=%d", pid)
}

func TestParseRunName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		want      processIdentity
		wantValid bool
	}{
		{
			name:      "run.31415.0123456789ab.abcdef",
			want:      processIdentity{pid: 31415, startToken: "0123456789ab"},
			wantValid: true,
		},
		{name: "run.0.0123456789ab.abcdef"},
		{name: "run.31415.short.abcdef"},
		{name: "run.31415.0123456789ab.bad/slash"},
		{name: "not-a-run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRunName(tt.name)
			assert.Equal(t, tt.wantValid, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSupportedMatchesPlatform(t *testing.T) {
	t.Parallel()

	assert.Equal(t, runtime.GOOS == "darwin" || runtime.GOOS == "linux", Supported())
}

func TestPublishRunDoesNotExposeUnmarkedFinalDirectory(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	runName := "run.31415.0123456789ab.abcdef"
	marker := ownerMarker{
		PID:          31415,
		ProcessStart: "start",
		StartToken:   "0123456789ab",
	}
	renameErr := errors.New("injected rename failure")
	_, err := publishRun(root, runName, marker, func(staging, final string) error {
		_, matchesFinalPattern := parseRunName(filepath.Base(staging))
		assert.False(matchesFinalPattern)
		_, markerErr := os.Stat(filepath.Join(staging, "owner.json"))
		require.NoError(markerErr)
		_, finalErr := os.Stat(final)
		require.ErrorIs(finalErr, os.ErrNotExist)
		return renameErr
	})
	require.ErrorIs(err, renameErr)
	_, err = os.Stat(filepath.Join(root, runName))
	require.ErrorIs(err, os.ErrNotExist)
}

func TestPublishOwnerStateCreatesAdmissionBeforeRun(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	runName := "run.31415.0123456789ab.abcdef"
	marker := ownerMarker{
		PID:          31415,
		ProcessStart: "start",
		StartToken:   "0123456789ab",
	}

	runDir, admissionDir, err := publishOwnerState(
		root,
		runName,
		marker,
		func(staging, final string) error {
			require.DirExists(filepath.Join(root, admissionPrefix+runName))
			return os.Rename(staging, final)
		},
	)
	require.NoError(err)
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestRootLockSerializesAcrossProcesses(t *testing.T) {
	require := require.New(t)
	root := os.Getenv("KENN_FORGE_TEST_TMUX_LOCK_ROOT")
	if root != "" {
		ready := os.Getenv("KENN_FORGE_TEST_TMUX_LOCK_READY")
		release := os.Getenv("KENN_FORGE_TEST_TMUX_LOCK_RELEASE")
		require.NoError(withRootLock(root, func() error {
			require.NoError(os.WriteFile(ready, []byte("ready\n"), 0o600))
			require.Eventually(func() bool {
				_, err := os.Stat(release)
				return err == nil
			}, 5*time.Second, 10*time.Millisecond)
			return nil
		}))
		return
	}

	directory := t.TempDir()
	root = filepath.Join(directory, "root")
	require.NoError(prepareRoot(root))
	ready := filepath.Join(directory, "ready")
	release := filepath.Join(directory, "release")
	command := procutil.Command(
		os.Args[0], "-test.run=^TestRootLockSerializesAcrossProcesses$",
	)
	command.Env = append(os.Environ(),
		"KENN_FORGE_TEST_TMUX_LOCK_ROOT="+root,
		"KENN_FORGE_TEST_TMUX_LOCK_READY="+ready,
		"KENN_FORGE_TEST_TMUX_LOCK_RELEASE="+release,
	)
	require.NoError(command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	require.Eventually(func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	acquired := make(chan error, 1)
	go func() {
		acquired <- withRootLock(root, func() error { return nil })
	}()
	select {
	case err := <-acquired:
		require.NoError(err)
		require.Fail("second process entered the root critical section")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
	select {
	case err := <-acquired:
		require.NoError(err)
	case <-time.After(5 * time.Second):
		require.Fail("second process did not acquire the released root lock")
	}
	require.NoError(command.Wait())
}

func TestOwnerCleanupRetainsSharedRoot(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	owner, err := newAt(root)
	require.NoError(err)

	require.NoError(owner.Cleanup())
	info, err := os.Stat(root)
	require.NoError(err)
	require.True(info.IsDir())
}

func TestHeldAdmissionLockHelperProcess(t *testing.T) {
	require := require.New(t)
	admissionDir := os.Getenv("KENN_FORGE_TEST_TMUX_HELD_ADMISSION")
	if admissionDir == "" {
		return
	}
	lock := flock.New(filepath.Join(admissionDir, admissionLockName))
	require.NoError(lock.Lock())
	defer func() { require.NoError(lock.Unlock()) }()
	require.NoError(os.WriteFile(
		os.Getenv("KENN_FORGE_TEST_TMUX_HELD_ADMISSION_READY"),
		[]byte("ready\n"),
		0o600,
	))
	require.Eventually(func() bool {
		_, err := os.Stat(os.Getenv(
			"KENN_FORGE_TEST_TMUX_HELD_ADMISSION_RELEASE",
		))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}

func startHeldAdmissionLockHelper(t *testing.T, admissionDir string) func() {
	t.Helper()
	require := require.New(t)
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	release := filepath.Join(directory, "release")
	command := procutil.Command(
		os.Args[0],
		"-test.run=^TestHeldAdmissionLockHelperProcess$",
	)
	command.Env = append(os.Environ(),
		"KENN_FORGE_TEST_TMUX_HELD_ADMISSION="+admissionDir,
		"KENN_FORGE_TEST_TMUX_HELD_ADMISSION_READY="+ready,
		"KENN_FORGE_TEST_TMUX_HELD_ADMISSION_RELEASE="+release,
	)
	require.NoError(command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	require.Eventually(func() bool {
		_, statErr := os.Stat(ready)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	markers, err := filepath.Glob(filepath.Join(admissionDir, "starting.*"))
	require.NoError(err)
	require.Empty(markers)
	return func() {
		require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
		require.NoError(command.Wait())
	}
}

func runWithHeldAdmissionLock(
	t *testing.T,
	admissionDir string,
	operation func() error,
) error {
	t.Helper()
	require := require.New(t)
	releaseLock := startHeldAdmissionLockHelper(t, admissionDir)
	done := make(chan error, 1)
	go func() { done <- operation() }()
	var operationErr error
	timedOut := false
	select {
	case operationErr = <-done:
	case <-time.After(cleanupTimeout + 2*time.Second):
		timedOut = true
	}
	releaseLock()
	if timedOut {
		operationErr = <-done
		require.Fail("operation did not time out while the admission lock was held")
	}
	return operationErr
}

func TestOwnerCleanupTimesOutWhenAdmissionLockIsHeld(t *testing.T) {
	require := require.New(t)
	directory := t.TempDir()
	owner, err := newAt(filepath.Join(directory, "root"))
	require.NoError(err)

	cleanupErr := runWithHeldAdmissionLock(
		t, owner.admissionDir, owner.Cleanup,
	)
	require.ErrorIs(cleanupErr, context.DeadlineExceeded)
	require.DirExists(owner.runDir)
	require.DirExists(owner.admissionDir)
}

func TestReapStaleTimesOutWhenAdmissionLockIsHeld(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	start := "dead-owner"
	identity := processIdentity{
		pid:        999_999,
		startToken: tokenForStart(start),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	runDir, admissionDir, err := publishOwnerState(
		root,
		runName,
		ownerMarker{
			PID:          identity.pid,
			ProcessStart: start,
			StartToken:   identity.startToken,
		},
		os.Rename,
	)
	require.NoError(err)
	reapErr := runWithHeldAdmissionLock(t, admissionDir, func() error {
		return reapStaleWithLookup(root, func(int) (string, error) {
			return "", errProcessAbsent
		})
	})
	require.ErrorIs(reapErr, context.DeadlineExceeded)
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestOwnerCleanupPreservesStateWhenAdmissionCannotClose(t *testing.T) {
	require := require.New(t)
	owner, err := newAt(filepath.Join(t.TempDir(), "root"))
	require.NoError(err)
	require.NoError(os.Mkdir(
		filepath.Join(owner.admissionDir, admissionClosedName),
		0o700,
	))

	require.Error(owner.Cleanup())
	require.DirExists(owner.runDir)
	require.DirExists(owner.admissionDir)
}

func TestOwnerCleanupPreservesStateWhenAdmissionDrainFails(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	admissionDir := filepath.Join(root, "admission.[")
	require.NoError(os.Mkdir(runDir, 0o700))
	require.NoError(os.Mkdir(admissionDir, 0o700))
	owner := &Owner{
		root:         root,
		runDir:       runDir,
		admissionDir: admissionDir,
		servers:      make(map[string]registeredServer),
	}

	require.Error(owner.Cleanup())
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestOwnerCleanupPreservesAdmissionWhenRunRemovalFails(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	admissionDir := filepath.Join(root, "admission")
	require.NoError(os.Mkdir(admissionDir, 0o700))
	owner := &Owner{
		root:         root,
		runDir:       filepath.Join(root, "invalid\x00run"),
		admissionDir: admissionDir,
		servers:      make(map[string]registeredServer),
	}

	require.Error(owner.Cleanup())
	require.DirExists(admissionDir)
}

func TestAdmittedCreationsPreservesMarkerWhenLookupIsIndeterminate(t *testing.T) {
	require := require.New(t)
	admissionDir := t.TempDir()
	start := "startup-identity"
	marker := filepath.Join(admissionDir, fmt.Sprintf(
		"starting.%d.%s", 31415, tokenForStart(start),
	))
	require.NoError(os.WriteFile(marker, []byte(start), 0o600))

	_, err := admittedCreationsWithLookup(
		[]string{admissionDir},
		func(int) (string, error) {
			return "", errors.New("transient identity lookup failure")
		},
	)
	require.Error(err)
	require.FileExists(marker)
}

func TestAdmittedCreationsPreservesInvalidMarker(t *testing.T) {
	require := require.New(t)
	admissionDir := t.TempDir()
	marker := filepath.Join(admissionDir, "starting.invalid.token")
	require.NoError(os.WriteFile(marker, []byte("startup-identity"), 0o600))

	_, err := admittedCreationsWithLookup(
		[]string{admissionDir},
		func(int) (string, error) {
			return "", errProcessAbsent
		},
	)
	require.Error(err)
	require.FileExists(marker)
}

func TestAdmittedCreationsPreservesUnreadableMarker(t *testing.T) {
	require := require.New(t)
	admissionDir := t.TempDir()
	marker := filepath.Join(admissionDir, "starting.31415.token")
	require.NoError(os.Symlink(
		filepath.Join(admissionDir, "missing-target"), marker,
	))

	_, err := admittedCreationsWithLookup(
		[]string{admissionDir},
		func(int) (string, error) {
			return "", errProcessAbsent
		},
	)
	require.Error(err)
	_, statErr := os.Lstat(marker)
	require.NoError(statErr)
}

func TestRunProcessIdentityStateRequiresMatchingStart(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	lookup := func(pid int) (string, error) {
		switch pid {
		case 101:
			return "same-start", nil
		case 202:
			return "reused-start", nil
		default:
			return "", errProcessAbsent
		}
	}

	status, err := runProcessIdentityState(
		processIdentity{pid: 101, startToken: tokenForStart("same-start")},
		lookup,
	)
	require.NoError(err)
	require.Equal(processIdentityLive, status)

	status, err = runProcessIdentityState(
		processIdentity{pid: 202, startToken: tokenForStart("original-start")},
		lookup,
	)
	require.NoError(err)
	require.Equal(processIdentityAbsent, status, "a reused PID must not pin a dead run")

	status, err = runProcessIdentityState(
		processIdentity{pid: 303, startToken: tokenForStart("gone")},
		lookup,
	)
	require.NoError(err)
	require.Equal(processIdentityAbsent, status)
}

func TestProcessStartIsStableForLiveProcess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if !Supported() {
		t.Skip("high-resolution process identity is unavailable")
	}
	first, err := processStart(os.Getpid())
	require.NoError(err)
	time.Sleep(10 * time.Millisecond)
	second, err := processStart(os.Getpid())
	require.NoError(err)
	assert.NotEmpty(first)
	assert.Equal(first, second)
}

func TestProcessStartReportsDefiniteAbsence(t *testing.T) {
	if !Supported() {
		t.Skip("high-resolution process identity is unavailable")
	}

	_, err := processStart(2_000_000_000)
	require.ErrorIs(t, err, errProcessAbsent)
}

func TestExactProcessIdentityStateRequiresOriginalStart(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	lookup := func(pid int) (string, error) {
		if pid == 101 {
			return "current-start", nil
		}
		return "", errProcessAbsent
	}
	status, err := exactProcessIdentityState(101, "current-start", lookup)
	require.NoError(err)
	require.Equal(processIdentityLive, status)
	status, err = exactProcessIdentityState(101, "prior-start", lookup)
	require.NoError(err)
	require.Equal(processIdentityAbsent, status)
	status, err = exactProcessIdentityState(202, "gone", lookup)
	require.NoError(err)
	require.Equal(processIdentityAbsent, status)
}

func TestIdentityForSocketRequiresContainedRunPath(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	root := t.TempDir()
	run := filepath.Join(root, "run.31415.0123456789ab.abcdef")
	inside := filepath.Join(run, "server-1", "tmux.sock")
	identity, ok := identityForSocket(root, inside)
	require.True(t, ok)
	assert.Equal(processIdentity{
		pid:        31415,
		startToken: "0123456789ab",
	}, identity)

	_, ok = identityForSocket(root, filepath.Join(root, "..", "escape", "tmux.sock"))
	assert.False(ok)
	_, ok = identityForSocket(root, filepath.Join(root, "unexpected", "tmux.sock"))
	assert.False(ok)
	_, ok = identityForSocket(root, "relative/tmux.sock")
	assert.False(ok)
}

func TestExplicitSocketRecognizesTmuxServerTitle(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	socket, ok := explicitSocket("tmux: server (/tmp/owned/run.1/token/tmux.sock)")
	assert.True(ok)
	assert.Equal("/tmp/owned/run.1/token/tmux.sock", socket)

	invalid := []string{
		"tmux: server (relative/tmux.sock)",
		"tmux: server (/tmp/owned/tmux.sock) trailing",
		"tmux: client (/tmp/owned/tmux.sock)",
		"other: server (/tmp/owned/tmux.sock)",
	}
	for _, command := range invalid {
		_, ok := explicitSocket(command)
		assert.False(ok, command)
	}
}

func TestOwnerCleanupStopsRegisteredServerAndPreservesControl(t *testing.T) {
	tmuxPath := requireTmux(t)
	root := filepath.Join(shortTempDir(t), "owned")
	owner, err := newAt(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Cleanup() })

	ownedCommand := owner.Command(t, tmuxPath)
	ownedPID := startTmuxServer(t, tmuxPath, ownedCommand)

	controlDir := shortTempDir(t)
	controlSocket := filepath.Join(controlDir, "control.sock")
	controlCommand := []string{
		tmuxPath, "-f", "/dev/null", "-S", controlSocket,
	}
	controlPID := startTmuxServer(t, tmuxPath, controlCommand)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = procutil.CommandContext(
			ctx, tmuxPath, "-S", controlSocket, "kill-server",
		).Run()
	})

	require.NoError(t, owner.Cleanup())
	requireProcessGone(t, ownedPID)
	assert.False(t, processGone(controlPID), "cleanup touched an unrelated server")
}

func TestOwnerCleanupStopsRegisteredServerAfterDirectoryDisappears(t *testing.T) {
	tmuxPath := requireTmux(t)
	owner, err := newAt(filepath.Join(shortTempDir(t), "owned"))
	require.NoError(t, err)
	command := owner.Command(t, tmuxPath)
	serverPID := startTmuxServer(t, tmuxPath, command)
	t.Cleanup(func() {
		if !processGone(serverPID) {
			_ = procutil.Command("/bin/kill", "-KILL", strconv.Itoa(serverPID)).Run()
		}
	})
	require.NoError(t, os.RemoveAll(owner.runDir))

	require.NoError(t, owner.Cleanup())
	requireProcessGone(t, serverPID)
}

func TestOwnerCleanupWaitsForAdmittedStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux fixture uses Unix shell semantics")
	}
	require := require.New(t)
	directory := t.TempDir()
	root := filepath.Join(directory, "root")
	owner, err := newAt(root)
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Cleanup() })

	entered := filepath.Join(directory, "entered")
	release := filepath.Join(directory, "release")
	record := filepath.Join(directory, "record")
	tmuxPath := filepath.Join(directory, "fake-tmux")
	body := `#!/bin/sh
case "$*" in
  *new-session*)
    : > "$TEST_TMUX_ENTERED"
    while [ ! -f "$TEST_TMUX_RELEASE" ]; do sleep 0.01; done
    printf 'created\n' >> "$TEST_TMUX_RECORD"
    ;;
  *kill-server*)
    printf 'killed\n' >> "$TEST_TMUX_RECORD"
    ;;
esac
`
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("TEST_TMUX_ENTERED", entered)
	t.Setenv("TEST_TMUX_RELEASE", release)
	t.Setenv("TEST_TMUX_RECORD", record)

	tmuxCommand := owner.Command(t, tmuxPath)
	startupDone := make(chan error, 1)
	go func() {
		args := append(slices.Clone(tmuxCommand[1:]), "new-session", "-d")
		startupDone <- procutil.Command(tmuxCommand[0], args...).Run()
	}()
	require.Eventually(func() bool {
		_, statErr := os.Stat(entered)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(os.RemoveAll(owner.runDir))

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- owner.Cleanup() }()
	select {
	case cleanupErr := <-cleanupDone:
		require.NoError(cleanupErr)
		require.Fail("cleanup returned while admitted tmux startup was blocked")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
	require.NoError(<-startupDone)
	require.NoError(<-cleanupDone)
	content, err := os.ReadFile(record)
	require.NoError(err)
	require.Equal("created\nkilled\n", string(content))
}

func TestOwnerCleanupTerminatesStalledAdmittedStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux fixture uses Unix shell semantics")
	}
	require := require.New(t)
	directory := t.TempDir()
	owner, err := newAt(filepath.Join(directory, "root"))
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Cleanup() })

	entered := filepath.Join(directory, "entered")
	tmuxPath := filepath.Join(directory, "fake-tmux")
	body := `#!/bin/sh
case "$*" in
  *new-session*)
	trap '' TERM
	: > "$TEST_TMUX_ENTERED"
	while :; do sleep 1; done
	;;
esac
`
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("TEST_TMUX_ENTERED", entered)

	tmuxCommand := owner.Command(t, tmuxPath)
	args := append(slices.Clone(tmuxCommand[1:]), "new-session", "-d")
	startup := procutil.Command(tmuxCommand[0], args...)
	require.NoError(startup.Start())
	startupDone := make(chan error, 1)
	go func() { startupDone <- startup.Wait() }()
	t.Cleanup(func() {
		if !processGone(startup.Process.Pid) {
			_ = startup.Process.Kill()
		}
		select {
		case <-startupDone:
		default:
		}
	})
	require.Eventually(func() bool {
		_, statErr := os.Stat(entered)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- owner.Cleanup() }()
	select {
	case cleanupErr := <-cleanupDone:
		require.NoError(cleanupErr)
	case <-time.After(2*cleanupTimeout + 2*time.Second):
		require.Fail("cleanup did not terminate a stalled admitted tmux startup")
		_ = startup.Process.Kill()
		require.NoError(<-cleanupDone)
	}
	require.Error(<-startupDone)
	requireProcessGone(t, startup.Process.Pid)
}

func TestNewAtDrainsMultipleStaleAdmissionsWithinOneDeadline(t *testing.T) {
	if os.Getenv("KENN_FORGE_TEST_TMUX_STALLED_STARTUP") == "1" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		require.NoError(t, os.WriteFile(
			os.Getenv("KENN_FORGE_TEST_TMUX_STALLED_READY"),
			[]byte("ready\n"),
			0o600,
		))
		for range term {
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("stale startup recovery requires Unix signals")
	}
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))

	type startupProcess struct {
		command *exec.Cmd
		done    chan error
	}
	startups := make([]startupProcess, 0, 2)
	for index := range 2 {
		ready := filepath.Join(root, fmt.Sprintf("ready-%d", index))
		command := procutil.Command(
			os.Args[0],
			"-test.run=^TestNewAtDrainsMultipleStaleAdmissionsWithinOneDeadline$",
		)
		command.Env = append(os.Environ(),
			"KENN_FORGE_TEST_TMUX_STALLED_STARTUP=1",
			"KENN_FORGE_TEST_TMUX_STALLED_READY="+ready,
		)
		require.NoError(command.Start())
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		startups = append(startups, startupProcess{command: command, done: done})
		t.Cleanup(func() {
			if !processGone(command.Process.Pid) {
				_ = command.Process.Kill()
			}
			select {
			case <-done:
			default:
			}
		})
		require.Eventually(func() bool {
			_, statErr := os.Stat(ready)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)

		start, err := processStart(command.Process.Pid)
		require.NoError(err)
		runName := fmt.Sprintf(
			"run.%d.%s.%06d",
			999_999-index,
			tokenForStart(fmt.Sprintf("dead-owner-%d", index)),
			index,
		)
		admissionDir := filepath.Join(root, admissionPrefix+runName)
		require.NoError(os.Mkdir(admissionDir, 0o700))
		marker := filepath.Join(admissionDir, fmt.Sprintf(
			"starting.%d.%s", command.Process.Pid, tokenForStart(start),
		))
		require.NoError(os.WriteFile(marker, []byte(start), 0o600))
	}

	type newResult struct {
		owner *Owner
		err   error
	}
	result := make(chan newResult, 1)
	go func() {
		owner, err := newAt(root)
		result <- newResult{owner: owner, err: err}
	}()
	select {
	case created := <-result:
		require.NoError(created.err)
		t.Cleanup(func() { _ = created.owner.Cleanup() })
	case <-time.After(2*cleanupTimeout + time.Second):
		for _, startup := range startups {
			_ = startup.command.Process.Kill()
		}
		created := <-result
		if created.owner != nil {
			t.Cleanup(func() { _ = created.owner.Cleanup() })
		}
		require.Fail("stale admissions did not share one cleanup deadline")
	}
	for _, startup := range startups {
		require.Error(<-startup.done)
		requireProcessGone(t, startup.command.Process.Pid)
	}
}

func TestReapStalePreservesRunWhenAdmissionCannotClose(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	start := "dead-owner"
	identity := processIdentity{
		pid:        999_999,
		startToken: tokenForStart(start),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	runDir, err := publishRun(root, runName, ownerMarker{
		PID:          identity.pid,
		ProcessStart: start,
		StartToken:   identity.startToken,
	}, os.Rename)
	require.NoError(err)
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	require.NoError(os.Mkdir(admissionDir, 0o700))
	require.NoError(os.Mkdir(
		filepath.Join(admissionDir, admissionClosedName),
		0o700,
	))

	require.Error(reapStale(root))
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestReapStaleRemovesAdmissionWithoutRun(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	identity := processIdentity{
		pid:        999_999,
		startToken: tokenForStart("dead-owner"),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	require.NoError(os.Mkdir(admissionDir, 0o700))

	require.NoError(reapStaleWithLookup(root, func(int) (string, error) {
		return "", errProcessAbsent
	}))
	require.NoDirExists(admissionDir)
}

func TestReapStalePreservesAdmissionWhenRunValidationFails(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	identity := processIdentity{
		pid:        999_999,
		startToken: tokenForStart("dead-owner"),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	runDir, err := publishRun(root, runName, ownerMarker{
		PID:          identity.pid,
		ProcessStart: "different-owner",
		StartToken:   identity.startToken,
	}, os.Rename)
	require.NoError(err)
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	require.NoError(os.Mkdir(admissionDir, 0o700))

	require.Error(reapStaleWithLookup(root, func(int) (string, error) {
		return "", errProcessAbsent
	}))
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestReapStaleDefersOwnerThatDiesAfterAdmissionSnapshot(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	start := "live-owner"
	identity := processIdentity{
		pid:        31415,
		startToken: tokenForStart(start),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	runDir, err := publishRun(root, runName, ownerMarker{
		PID:          identity.pid,
		ProcessStart: start,
		StartToken:   identity.startToken,
	}, os.Rename)
	require.NoError(err)
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	require.NoError(os.Mkdir(admissionDir, 0o700))

	lookups := 0
	lookupStart := func(pid int) (string, error) {
		require.Equal(identity.pid, pid)
		lookups++
		if lookups == 1 {
			return start, nil
		}
		return "", errProcessAbsent
	}

	require.NoError(reapStaleWithLookup(root, lookupStart))
	require.Equal(1, lookups)
	require.DirExists(runDir)
	require.DirExists(admissionDir)
}

func TestReapStalePreservesOwnerWhenLookupIsIndeterminate(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(prepareRoot(root))
	start := "live-owner"
	identity := processIdentity{
		pid:        31415,
		startToken: tokenForStart(start),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef", identity.pid, identity.startToken,
	)
	runDir, err := publishRun(root, runName, ownerMarker{
		PID:          identity.pid,
		ProcessStart: start,
		StartToken:   identity.startToken,
	}, os.Rename)
	require.NoError(err)
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	require.NoError(os.Mkdir(admissionDir, 0o700))

	require.Error(reapStaleWithLookup(root, func(int) (string, error) {
		return "", errors.New("transient identity lookup failure")
	}))
	require.DirExists(runDir)
	require.DirExists(admissionDir)
	require.NoFileExists(filepath.Join(admissionDir, admissionClosedName))
}

func TestNewAtReapsServerAfterRunDirectoryDisappears(t *testing.T) {
	require := require.New(t)
	tmuxPath := requireTmux(t)
	root := filepath.Join(shortTempDir(t), "owned")
	require.NoError(os.MkdirAll(root, 0o700))
	staleIdentity := processIdentity{
		pid:        999999,
		startToken: tokenForStart("dead-owner"),
	}
	runName := fmt.Sprintf(
		"run.%d.%s.abcdef",
		staleIdentity.pid,
		staleIdentity.startToken,
	)
	runDir := filepath.Join(root, runName)
	serverDir := filepath.Join(runDir, "server-abcdef")
	require.NoError(os.MkdirAll(serverDir, 0o700))
	socket := filepath.Join(serverDir, "tmux.sock")
	command := []string{tmuxPath, "-f", "/dev/null", "-S", socket}
	serverPID := startTmuxServer(t, tmuxPath, command)
	t.Cleanup(func() {
		if !processGone(serverPID) {
			_ = procutil.Command("/bin/kill", "-KILL", strconv.Itoa(serverPID)).Run()
		}
	})

	require.NoError(os.RemoveAll(runDir))
	owner, err := newAt(root)
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Cleanup() })
	requireProcessGone(t, serverPID)
}

func TestNewAtRefusesUnmarkedStaleRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmuxPath := requireTmux(t)
	root := filepath.Join(shortTempDir(t), "owned")
	runDir := filepath.Join(
		root,
		"run.999999."+tokenForStart("dead-owner")+".abcdef",
	)
	serverDir := filepath.Join(runDir, "server-abcdef")
	require.NoError(os.MkdirAll(serverDir, 0o700))
	socket := filepath.Join(serverDir, "tmux.sock")
	command := []string{tmuxPath, "-f", "/dev/null", "-S", socket}
	serverPID := startTmuxServer(t, tmuxPath, command)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = procutil.CommandContext(ctx, tmuxPath, "-S", socket, "kill-server").Run()
	})

	owner, err := newAt(root)
	if owner != nil {
		t.Cleanup(func() { _ = owner.Cleanup() })
	}
	require.Error(err)
	assert.False(processGone(serverPID), "ambiguous stale ownership must fail closed")
	_, statErr := os.Stat(runDir)
	assert.NoError(statErr)
}

func TestSignalCleanupStopsRegisteredServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if os.Getenv("KENN_FORGE_TEST_TMUX_SIGNAL_HELPER") == "1" {
		owner, err := newAt(os.Getenv("KENN_FORGE_TEST_TMUX_SIGNAL_ROOT"))
		require.NoError(err)
		command := owner.Command(t, os.Getenv("KENN_FORGE_TEST_TMUX_BINARY"))
		pid := startTmuxServer(t, command[0], command)
		require.NoError(os.WriteFile(
			os.Getenv("KENN_FORGE_TEST_TMUX_SIGNAL_PID"),
			[]byte(strconv.Itoa(pid)),
			0o600,
		))
		testsignal.Install(owner.Cleanup, nil)
		require.NoError(os.WriteFile(
			os.Getenv("KENN_FORGE_TEST_TMUX_SIGNAL_READY"),
			[]byte("ready\n"),
			0o600,
		))
		select {}
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cleanup requires Unix")
	}

	tmuxPath := requireTmux(t)
	directory := shortTempDir(t)
	ready := filepath.Join(directory, "ready")
	pidFile := filepath.Join(directory, "server-pid")
	command := procutil.Command(
		os.Args[0], "-test.run=^TestSignalCleanupStopsRegisteredServer$",
	)
	command.Env = append(os.Environ(),
		"KENN_FORGE_TEST_TMUX_SIGNAL_HELPER=1",
		"KENN_FORGE_TEST_TMUX_SIGNAL_ROOT="+filepath.Join(directory, "root"),
		"KENN_FORGE_TEST_TMUX_SIGNAL_READY="+ready,
		"KENN_FORGE_TEST_TMUX_SIGNAL_PID="+pidFile,
		"KENN_FORGE_TEST_TMUX_BINARY="+tmuxPath,
	)
	require.NoError(command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	require.Eventually(func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 25*time.Millisecond)
	content, err := os.ReadFile(pidFile)
	require.NoError(err)
	serverPID, err := strconv.Atoi(string(content))
	require.NoError(err)

	require.NoError(command.Process.Signal(syscall.SIGTERM))
	err = command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(err, &exitErr)
	assert.Equal(143, exitErr.ExitCode())
	requireProcessGone(t, serverPID)
}
