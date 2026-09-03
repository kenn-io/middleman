package localruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/cenkalti/backoff/v7"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	internalretry "go.kenn.io/forge/internal/retry"
)

var newPtyOwnerAttachBackOff = func() backoff.BackOff {
	return internalretry.DefaultBackOff()
}

type ptyOwnerLifecycle struct {
	owner   ptyownerruntime.Owner
	session string
	pty     ptyownerruntime.PTY
}

func (l ptyOwnerLifecycle) Detach() {
	if l.pty != nil {
		l.pty.Close()
	}
}

func (l ptyOwnerLifecycle) Stop(ctx context.Context) error {
	if l.pty != nil {
		defer l.pty.Close()
	}
	if l.owner != nil {
		if err := l.owner.Stop(ctx, l.session); err != nil {
			return err
		}
	}
	return nil
}

func startPtyOwnerSession(
	ctx context.Context,
	owner ptyownerruntime.Owner,
	info SessionInfo,
	command []string,
	cwd string,
	extraStripVars []string,
) (*session, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, errors.New("session command is empty")
	}
	var extraEnv map[string]string
	if info.Kind == LaunchTargetPlainShell {
		if locale := shellCharacterLocaleDefault(os.Environ(), runtime.GOOS); locale != "" {
			extraEnv = map[string]string{"LC_CTYPE": locale}
		}
	}
	ptySession, err := owner.Start(ctx, info.Key, cwd, command, extraStripVars, extraEnv)
	if err != nil {
		return nil, fmt.Errorf("start pty owner: %w", err)
	}
	slog.Debug(
		"runtime session pty owner started",
		"workspace_id", info.WorkspaceID,
		"session_key", info.Key,
		"target_key", info.TargetKey,
	)
	return newPtyOwnerSession(owner, info, ptySession), nil
}

func attachPtyOwnerSession(
	ctx context.Context,
	owner ptyownerruntime.Owner,
	info SessionInfo,
) (*session, error) {
	ptySession, err := internalretry.Do(ctx, internalretry.Config[ptyownerruntime.PTY]{
		Label:    "runtime pty owner attach",
		BackOff:  newPtyOwnerAttachBackOff(),
		MaxTries: 4,
		IsTransient: func(error) bool {
			return true
		},
		Op: func() (ptyownerruntime.PTY, error) {
			return owner.Attach(ctx, info.Key)
		},
	})
	if err != nil {
		return nil, fmt.Errorf(
			"%w: %q: %v",
			ErrSessionUnavailable, info.Key, err,
		)
	}
	slog.Debug(
		"runtime session pty owner attached",
		"workspace_id", info.WorkspaceID,
		"session_key", info.Key,
		"target_key", info.TargetKey,
	)
	return newPtyOwnerSession(owner, info, ptySession), nil
}

func newPtyOwnerSession(
	owner ptyownerruntime.Owner,
	info SessionInfo,
	ptySession ptyownerruntime.PTY,
) *session {
	info.Status = SessionStatusRunning
	s := &session{
		info: info,
		pty:  ptySession,
		lifecycle: ptyOwnerLifecycle{
			owner:   owner,
			session: info.Key,
			pty:     ptySession,
		},
		done:        make(chan struct{}),
		outputDone:  make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	go s.drainOutput()
	return s
}
