package workspacetest

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/procutil"
)

const abruptWorkspaceTestTmuxOwnerPID = "KENN_FORGE_WORKSPACETEST_ABRUPT_OWNER_PID"

func runAbruptWorkspaceTestTmuxOwnerHelper() {
	pidPath := os.Getenv(abruptWorkspaceTestTmuxOwnerPID)
	if pidPath == "" {
		return
	}
	args := append(append([]string(nil), workspaceTestTmuxCommand[1:]...),
		"new-session", "-d", "-s", "abrupt-owner", "sleep", "300",
	)
	if output, err := procutil.Command(
		workspaceTestTmuxCommand[0], args...,
	).CombinedOutput(); err != nil {
		_, _ = os.Stderr.Write(output)
		os.Exit(2)
	}
	args = append(append([]string(nil), workspaceTestTmuxCommand[1:]...),
		"display-message", "-p", "#{pid}",
	)
	output, err := procutil.Command(
		workspaceTestTmuxCommand[0], args...,
	).CombinedOutput()
	if err != nil {
		_, _ = os.Stderr.Write(output)
		os.Exit(2)
	}
	if err := os.WriteFile(pidPath, []byte(strings.TrimSpace(string(output))), 0o600); err != nil {
		os.Exit(2)
	}
	select {}
}

func TestWorkspaceTestTmuxStopsServerAfterAbruptOwnerExit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("private test tmux owners require Darwin or Linux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux is unavailable: %v", err)
	}
	require := require.New(t)
	pidPath := t.TempDir() + "/server.pid"
	owner := procutil.Command(os.Args[0], "-test.run=^$")
	owner.Env = append(os.Environ(), abruptWorkspaceTestTmuxOwnerPID+"="+pidPath)
	require.NoError(owner.Start())
	t.Cleanup(func() {
		if owner.ProcessState == nil {
			_ = owner.Process.Kill()
			_, _ = owner.Process.Wait()
		}
	})

	var serverPID int
	require.Eventually(func() bool {
		content, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		serverPID, err = strconv.Atoi(string(content))
		return err == nil
	}, 5*time.Second, 25*time.Millisecond)
	t.Cleanup(func() {
		if !processIsGone(serverPID) {
			_ = procutil.Command("/bin/kill", "-KILL", strconv.Itoa(serverPID)).Run()
		}
	})

	require.NoError(owner.Process.Kill())
	require.Error(owner.Wait())
	require.Eventually(
		func() bool { return processIsGone(serverPID) },
		5*time.Second,
		25*time.Millisecond,
	)
}

func processIsGone(pid int) bool {
	return procutil.Command("/bin/kill", "-0", strconv.Itoa(pid)).Run() != nil
}
