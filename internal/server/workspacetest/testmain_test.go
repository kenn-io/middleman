package workspacetest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"testing"

	"go.kenn.io/forge/internal/testutil/gitsafe"
	"go.kenn.io/forge/internal/testutil/processjob"
	"go.kenn.io/forge/internal/testutil/testsignal"
	"go.kenn.io/forge/internal/testutil/testtmux"
)

var workspaceTestTmuxCommand []string

func TestMain(m *testing.M) {
	if code, ok := testtmux.CommandWrapperExitCode(); ok {
		os.Exit(code)
	}
	if slices.Contains(os.Args, workspaceRuntimeHelperMarker) {
		os.Exit(m.Run())
	}
	if err := processjob.ContainCurrentProcessTree(); err != nil {
		fmt.Fprintf(os.Stderr, "contain workspace test process tree: %v\n", err)
		os.Exit(1)
	}
	cleanupTmux, err := configureWorkspaceTestTmux()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure isolated workspace test tmux: %v\n", err)
		os.Exit(1)
	}
	runAbruptWorkspaceTestTmuxOwnerHelper()
	runCleanup, stopSignalCleanup := testsignal.Install(cleanupTmux, func(err error) {
		fmt.Fprintf(os.Stderr, "cleanup isolated workspace test tmux: %v\n", err)
	})
	code := gitsafe.RunIsolatedMain(m)
	if err := runCleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup isolated workspace test tmux: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	stopSignalCleanup()
	os.Exit(code)
}

func configureWorkspaceTestTmux() (func() error, error) {
	tmuxPath, err := exec.LookPath("tmux")
	if errors.Is(err, exec.ErrNotFound) {
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find tmux: %w", err)
	}
	if !testtmux.Supported() {
		return func() error { return nil }, nil
	}
	owner, err := testtmux.New()
	if err != nil {
		return nil, fmt.Errorf("initialize private test tmux owner: %w", err)
	}
	workspaceTestTmuxCommand, err = owner.CommandForRun(tmuxPath)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("register workspace test tmux server: %w", err),
			owner.Cleanup(),
		)
	}
	return owner.Cleanup, nil
}
