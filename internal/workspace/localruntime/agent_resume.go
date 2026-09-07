package localruntime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/procutil"
)

// agentResumeCommand preserves configured options and uses the hook's agent
// identity, since configured target names need not identify an agent family.
func agentResumeCommand(command []string, agent, sessionID string) ([]string, error) {
	if len(command) == 0 || strings.TrimSpace(sessionID) == "" || strings.HasPrefix(sessionID, "-") {
		return nil, fmt.Errorf("agent resume requires a command and session ID")
	}
	result := slices.Clone(command)
	switch agent {
	case "codex":
		result = append(result, "resume", sessionID)
	case "claude":
		result = append(result, "--resume", sessionID)
	case "pi":
		result = append(result, "--session", sessionID)
	default:
		return nil, fmt.Errorf("session resume is not supported for agent %q", agent)
	}
	return result, nil
}

// A successful PTY spawn does not mean tmux attached. Probe before spawning so
// a missing server cannot erase the recovery report through an asynchronous exit.
func (m *Manager) requireTmuxSession(ctx context.Context, session string) error {
	command := slices.Clone(m.tmuxCommand)
	if len(command) == 0 {
		command = config.DefaultTmuxCommand()
	}
	command, err := resolveTmuxCommand(command)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionUnavailable, err)
	}
	args := append(command[1:], "has-session", "-t", session)
	cmd := procutil.CommandContext(ctx, command[0], args...)
	cmd.Env = TmuxClientEnvironment(os.Environ(), m.currentStripEnvVars())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := procutil.Run(ctx, cmd, "tmux subprocess capacity"); err != nil {
		if isTmuxSessionAbsent(stderr.Bytes(), err) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
		}
		return fmt.Errorf("%w: check tmux session: %v: %s", ErrSessionUnavailable, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
