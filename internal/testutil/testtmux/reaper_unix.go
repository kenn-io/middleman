//go:build !windows

package testtmux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.kenn.io/forge/internal/procutil"
)

const ownerReaperExecutableName = "tmux-owner-reaper"

type ownerReaper struct {
	signal  *os.File
	command *exec.Cmd
}

func startOwnerReaper(runDir string) (*ownerReaper, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve test executable: %w", err)
	}
	reaperPath := filepath.Join(runDir, ownerReaperExecutableName)
	if err := os.Symlink(executable, reaperPath); err != nil {
		return nil, fmt.Errorf("link owner reaper: %w", err)
	}
	watch, signal, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create owner reaper signal: %w", err)
	}
	command := procutil.Command(reaperPath)
	command.ExtraFiles = []*os.File{watch}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = watch.Close()
		_ = signal.Close()
		return nil, fmt.Errorf("launch owner reaper: %w", err)
	}
	if err := watch.Close(); err != nil {
		_ = signal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("close owner reaper read pipe: %w", err)
	}
	return &ownerReaper{signal: signal, command: command}, nil
}

func ownerReaperExitCode() (int, bool) {
	if filepath.Base(os.Args[0]) != ownerReaperExecutableName {
		return 0, false
	}
	signal := os.NewFile(3, "tmux-owner-reaper-signal")
	if signal == nil {
		fmt.Fprintln(os.Stderr, "open private tmux owner reaper signal: unavailable")
		return 1, true
	}
	_, readErr := io.Copy(io.Discard, signal)
	closeErr := signal.Close()
	if readErr != nil || closeErr != nil {
		fmt.Fprintf(os.Stderr, "wait for private tmux owner exit: %v\n", errors.Join(readErr, closeErr))
		return 1, true
	}
	runDir := filepath.Dir(os.Args[0])
	root := filepath.Dir(runDir)
	deadline := time.Now().Add(2 * cleanupTimeout)
	var cleanupErr error
	for {
		cleanupErr = withRootLock(root, func() error {
			if _, err := os.Lstat(runDir); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			return reapStale(root)
		})
		if cleanupErr == nil {
			if _, err := os.Lstat(runDir); errors.Is(err, os.ErrNotExist) {
				return 0, true
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "reap abandoned private tmux owner: %v\n", cleanupErr)
			return 1, true
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (w *ownerReaper) stop() error {
	if err := w.signal.Close(); err != nil {
		return fmt.Errorf("stop private tmux owner reaper: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- w.command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("wait for private tmux owner reaper: %w", err)
		}
		return nil
	case <-time.After(cleanupTimeout):
		_ = w.command.Process.Kill()
		<-done
		return errors.New("wait for private tmux owner reaper: timed out")
	}
}

func (w *ownerReaper) cancel() error {
	killErr := w.command.Process.Kill()
	waitErr := w.command.Wait()
	closeErr := w.signal.Close()
	if killErr == nil {
		waitErr = nil
	} else if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	return errors.Join(killErr, waitErr, closeErr)
}
