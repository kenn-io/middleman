package localruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/procutil"
)

type tmuxEnvPolicy struct {
	preserveShellEnv bool
}

type tmuxPaneEnvironment struct {
	keys        []string
	paneCommand string
	commandEnv  []string
	// clientEnv feeds tmux client processes. It is derived from the
	// policy environment before caller-supplied launch extras are
	// applied: extras come from API request bodies and reach only the
	// pane via the env-file handoff, never the tmux client, whose
	// environment can seed the server's retained global environment or
	// steer socket resolution.
	clientEnv []string
	// paneLocalKeys are captured from the pane's own environment before
	// the env file is sourced and re-applied after the env -i wipe, so
	// tmux's per-pane variables survive the sanitized handoff.
	paneLocalKeys []string
}

var (
	tmuxAgentEnvPolicy = tmuxEnvPolicy{}
	tmuxShellEnvPolicy = tmuxEnvPolicy{preserveShellEnv: true}
)

func (p tmuxEnvPolicy) paneEnvironment(
	baseEnv []string,
	command []string,
	extraStripVars []string,
) tmuxPaneEnvironment {
	return paneEnvironmentFromEnv(
		p.environment(baseEnv, extraStripVars), command,
	)
}

// paneEnvironmentWithExtra applies the policy to baseEnv and then appends
// caller-supplied variables, which always reach the pane even when the
// policy's allowlist would drop them. Keys must be shell identifiers; the
// caller validates them.
func (p tmuxEnvPolicy) paneEnvironmentWithExtra(
	baseEnv []string,
	command []string,
	extraStripVars []string,
	extraEnv map[string]string,
) tmuxPaneEnvironment {
	env := p.environment(baseEnv, extraStripVars)
	clientEnv := append(slices.Clone(env), "TERM=xterm-256color")
	filtered := env[:0]
	for _, value := range env {
		key, _, found := strings.Cut(value, "=")
		if found {
			if _, replaced := extraEnv[key]; replaced {
				continue
			}
		}
		filtered = append(filtered, value)
	}
	env = filtered
	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		env = append(env, key+"="+extraEnv[key])
	}
	pane := paneEnvironmentFromEnv(env, command)
	pane.clientEnv = clientEnv
	return pane
}

func paneEnvironmentFromEnv(
	env []string,
	command []string,
) tmuxPaneEnvironment {
	return paneEnvironmentFromEnvWithPaneLocals(env, command, nil)
}

func paneEnvironmentFromEnvWithPaneLocals(
	env []string,
	command []string,
	paneLocals []string,
) tmuxPaneEnvironment {
	envWithTerm := append(slices.Clone(env), "TERM=xterm-256color")
	keys := tmuxEnvironmentKeys(envWithTerm)
	parts := make([]string, 0, len(keys)+len(paneLocals)+4)
	parts = append(parts, "exec", "env", "-i")
	for _, key := range keys {
		parts = append(parts, key+"=\"${"+key+"-}\"")
	}
	// Pane-local re-applications come last so they win over the
	// file-sourced values; TERM falls back to the xterm.js default when
	// the pane somehow carries none.
	for _, key := range paneLocals {
		if key == "TERM" {
			parts = append(parts,
				"TERM=\"${"+paneLocalCaptureVar(key)+":-xterm-256color}\"")
			continue
		}
		parts = append(parts, key+"=\"${"+paneLocalCaptureVar(key)+"-}\"")
	}
	parts = append(parts, shellCommand(command))
	return tmuxPaneEnvironment{
		keys:          keys,
		paneCommand:   strings.Join(parts, " "),
		commandEnv:    envWithTerm,
		clientEnv:     envWithTerm,
		paneLocalKeys: slices.Clone(paneLocals),
	}
}

func paneLocalCaptureVar(key string) string {
	return "__kenn_forge_pane_" + key
}

func (p tmuxEnvPolicy) keys(extraStripVars []string) []string {
	return p.paneEnvironment(os.Environ(), nil, extraStripVars).keys
}

func tmuxEnvironmentKeys(env []string) []string {
	keysByName := make(map[string]struct{}, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		if !isShellIdentifier(key) {
			continue
		}
		// Reserved handoff-internal names must never be sourced from
		// the env file: the cleanup trap removes the file paths they
		// hold, so an API-supplied override could aim rm at arbitrary
		// caller-selected paths.
		if strings.HasPrefix(key, "__kenn_forge") {
			continue
		}
		keysByName[key] = struct{}{}
	}

	keys := make([]string, 0, len(keysByName))
	for key := range keysByName {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (p tmuxEnvPolicy) environment(
	baseEnv []string,
	extraStripVars []string,
) []string {
	return p.environmentForOS(baseEnv, extraStripVars, runtime.GOOS)
}

func (p tmuxEnvPolicy) environmentForOS(
	baseEnv []string,
	extraStripVars []string,
	goos string,
) []string {
	if p.preserveShellEnv {
		env := sessionEnvironment(baseEnv, extraStripVars)
		if locale := shellCharacterLocaleDefault(env, goos); locale != "" {
			return append(env, "LC_CTYPE="+locale)
		}
		return env
	}
	return tmuxSessionEnvironment(baseEnv, extraStripVars)
}

type tmuxLauncher struct {
	TmuxCommand     []string
	Session         string
	CWD             string
	Pane            tmuxPaneEnvironment
	OwnerMarker     string
	LaunchID        string
	HideStatus      bool
	TmuxMouse       bool
	Graphics        bool
	ConfigureServer bool
}

type tmuxLaunchResult struct {
	AttachCommand []string
	Created       bool
}

func (l tmuxLauncher) prepare(ctx context.Context) (tmuxLaunchResult, error) {
	if l.Session == "" {
		return tmuxLaunchResult{}, fmt.Errorf("tmux session is empty")
	}
	exists, err := l.sessionExists(ctx)
	if err != nil {
		return tmuxLaunchResult{}, err
	}
	if exists {
		return l.prepareExisting(ctx)
	}

	paneCommand, cleanupEnvFile, err := l.newSessionPaneCommand()
	if err != nil {
		return tmuxLaunchResult{}, err
	}
	created := false
	defer func() {
		if !created {
			cleanupEnvFile()
		}
	}()
	if err := l.run(ctx, l.newSessionCommand(paneCommand)); err != nil {
		if retryErr := l.validateExistingAfterCreateRace(ctx); retryErr == nil {
			return l.prepareExisting(ctx)
		}
		return tmuxLaunchResult{}, fmt.Errorf("tmux new-session: %w", err)
	}
	if l.ConfigureServer {
		if err := l.run(ctx, l.globalPassthroughCommand()); err != nil {
			return tmuxLaunchResult{}, l.cleanupNewSessionAfterError(
				ctx, "configure global tmux passthrough", err,
			)
		}
		if err := l.run(ctx, l.sixelCommand()); err != nil {
			return tmuxLaunchResult{}, l.cleanupNewSessionAfterError(
				ctx, "configure tmux SIXEL", err,
			)
		}
		if err := l.run(ctx, l.tmuxMouseCommand()); err != nil {
			return tmuxLaunchResult{}, l.cleanupNewSessionAfterError(
				ctx, "configure tmux mouse", err,
			)
		}
	}
	if l.Graphics && !l.ConfigureServer {
		if err := l.run(ctx, l.passthroughCommand()); err != nil {
			return tmuxLaunchResult{}, l.cleanupNewSessionAfterError(
				ctx, "configure tmux passthrough", err,
			)
		}
	}
	if l.HideStatus {
		if err := l.run(ctx, l.hideStatusCommand()); err != nil {
			return tmuxLaunchResult{}, l.cleanupNewSessionAfterError(
				ctx, "hide tmux status", err,
			)
		}
	}
	created = true
	return tmuxLaunchResult{
		AttachCommand: l.attachSessionCommand(),
		Created:       true,
	}, nil
}

func (l tmuxLauncher) prepareExisting(ctx context.Context) (tmuxLaunchResult, error) {
	if err := l.validateOwner(ctx); err != nil {
		return tmuxLaunchResult{}, err
	}
	if err := l.replaceLaunchMarker(ctx); err != nil {
		return tmuxLaunchResult{}, err
	}
	if l.ConfigureServer {
		if err := l.run(ctx, l.globalPassthroughCommand()); err != nil {
			return tmuxLaunchResult{}, fmt.Errorf("configure global tmux passthrough: %w", err)
		}
		if err := l.run(ctx, l.sixelCommand()); err != nil {
			return tmuxLaunchResult{}, fmt.Errorf("configure tmux SIXEL: %w", err)
		}
		if err := l.run(ctx, l.tmuxMouseCommand()); err != nil {
			return tmuxLaunchResult{}, fmt.Errorf("configure tmux mouse: %w", err)
		}
	}
	if l.Graphics && !l.ConfigureServer {
		if err := l.run(ctx, l.passthroughCommand()); err != nil {
			return tmuxLaunchResult{}, fmt.Errorf("configure tmux passthrough: %w", err)
		}
	}
	return tmuxLaunchResult{AttachCommand: l.attachSessionCommand()}, nil
}

func (l tmuxLauncher) cleanupNewSessionAfterError(
	ctx context.Context,
	operation string,
	cause error,
) error {
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		5*time.Second,
	)
	defer cancel()
	if err := l.run(cleanupCtx, l.killSessionCommand()); err != nil {
		return fmt.Errorf(
			"%s: %w; cleanup new tmux session: %v",
			operation, cause, err,
		)
	}
	return fmt.Errorf("%s: %w", operation, cause)
}

// replaceLaunchMarker rewrites @forge_launch when this launch adopts an
// already-existing backend. The creator's rollback kills by launch marker as a
// fallback when no live manager entry remains, so an adopted backend must stop
// matching the creator's generation or a stale rollback after the adopting
// attachment detaches would kill work the adopter durably recorded.
func (l tmuxLauncher) replaceLaunchMarker(ctx context.Context) error {
	if l.LaunchID == "" {
		return nil
	}
	command := append(
		slices.Clone(l.TmuxCommand),
		"set-option", "-q", "-t", l.Session, "@forge_launch", l.LaunchID,
	)
	if err := l.run(ctx, command); err != nil {
		return fmt.Errorf("replace tmux launch marker: %w", err)
	}
	return nil
}

func (l tmuxLauncher) validateExistingAfterCreateRace(
	ctx context.Context,
) error {
	exists, err := l.sessionExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("tmux session %q still absent", l.Session)
	}
	return l.validateOwner(ctx)
}

func (l tmuxLauncher) sessionExists(ctx context.Context) (bool, error) {
	err := l.run(ctx, l.hasSessionCommand())
	if err == nil {
		return true, nil
	}
	var tmuxErr tmuxCommandError
	if errors.As(err, &tmuxErr) && isTmuxSessionAbsent(tmuxErr.stderr, tmuxErr.err) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session: %w", err)
}

func (l tmuxLauncher) validateOwner(ctx context.Context) error {
	if l.OwnerMarker == "" {
		return nil
	}
	out, err := l.output(ctx, l.showOwnerCommand())
	if err != nil {
		return fmt.Errorf("tmux show owner: %w", err)
	}
	if strings.TrimSpace(string(out)) != l.OwnerMarker {
		return fmt.Errorf("tmux session %q is not owned by this manager", l.Session)
	}
	return nil
}

func (l tmuxLauncher) run(ctx context.Context, command []string) error {
	_, err := l.output(ctx, command)
	return err
}

func (l tmuxLauncher) output(
	ctx context.Context,
	command []string,
) ([]byte, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, fmt.Errorf("tmux command is empty")
	}
	cmd := procutil.CommandContext(ctx, command[0], command[1:]...)
	// The pane command receives its environment through the env-file
	// handoff; the tmux client itself gets only the non-secret
	// allowlist of the pre-extras policy environment, because a client
	// that spawns the server seeds the server's permanently retained
	// global environment and its TMUX_TMPDIR steers socket resolution.
	cmd.Env = tmuxSessionEnvironment(l.Pane.clientEnv, nil)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := procutil.Run(ctx, cmd, "tmux subprocess capacity")
	if err != nil {
		return nil, tmuxCommandError{
			err:    err,
			stderr: slices.Clone(stderr.Bytes()),
		}
	}
	return stdout.Bytes(), nil
}

type tmuxCommandError struct {
	err    error
	stderr []byte
}

func (e tmuxCommandError) Error() string {
	msg := strings.TrimSpace(string(e.stderr))
	if msg == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + msg
}

func (e tmuxCommandError) Unwrap() error {
	return e.err
}

func (l tmuxLauncher) newSessionPaneCommand() (string, func(), error) {
	return paneHandoffCommand(l.Pane)
}

// NewTmuxPaneHandoff builds the pane shell-command that delivers env to
// the pane's process through a self-deleting env file, keeping values
// out of tmux argv and out of the tmux server's retained environment.
// tmux's per-pane variables (TERM, TMUX, TMUX_PANE) are preserved from
// the pane's own environment so the base terminal's shell keeps nesting
// detection and tmux's terminal capabilities. The returned cleanup
// removes the handoff files; call it when the pane was never started.
func NewTmuxPaneHandoff(
	env []string, command []string,
) (string, func(), error) {
	return paneHandoffCommand(paneEnvironmentFromEnvWithPaneLocals(
		env, command, []string{"TERM", "TMUX", "TMUX_PANE"},
	))
}

func paneHandoffCommand(pane tmuxPaneEnvironment) (string, func(), error) {
	path, err := writeTmuxPaneEnvironment(pane.commandEnv, pane.keys)
	if err != nil {
		return "", nil, fmt.Errorf("write tmux pane environment: %w", err)
	}
	// tmux parses shell-command with its default shell, which may not be
	// POSIX-compatible. Keep the POSIX handoff in a script run by /bin/sh.
	scriptPath, err := writeTmuxPaneScript(
		path, pane.paneCommand, pane.paneLocalKeys,
	)
	if err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("write tmux pane script: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(path)
		_ = os.Remove(scriptPath)
	}
	return shellCommand([]string{"/bin/sh", scriptPath}), cleanup, nil
}

func writeTmuxPaneScript(
	envPath string, paneCommand string, paneLocals []string,
) (string, error) {
	file, err := os.CreateTemp(tmuxPaneEnvironmentTempDir(), "kenn-forge-tmux-pane-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}

	// Capture pane-local values before the env file overwrites them.
	captures := make([]string, 0, len(paneLocals))
	for _, key := range paneLocals {
		captures = append(
			captures, paneLocalCaptureVar(key)+"=\"${"+key+"-}\"",
		)
	}
	content := strings.Join(append(captures, []string{
		"__kenn_forge_env_file=" + shellCommand([]string{envPath}),
		"__kenn_forge_script_file=" + shellCommand([]string{path}),
		`__kenn_forge_cleanup_tmux_files() { /bin/rm -f "$__kenn_forge_env_file" "$__kenn_forge_script_file"; }`,
		`trap __kenn_forge_cleanup_tmux_files EXIT`,
		`if [ ! -r "$__kenn_forge_env_file" ]; then exit 127; fi`,
		`. "$__kenn_forge_env_file"`,
		`__kenn_forge_cleanup_tmux_files`,
		`trap - EXIT`,
		`unset -f __kenn_forge_cleanup_tmux_files`,
		`unset __kenn_forge_env_file`,
		`unset __kenn_forge_script_file`,
		paneCommand,
	}...), "\n")
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.WriteString("\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func writeTmuxPaneEnvironment(env []string, keys []string) (string, error) {
	values := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		values[kv[:eq]] = kv[eq+1:]
	}

	var content strings.Builder
	for _, key := range keys {
		if !isShellIdentifier(key) {
			continue
		}
		content.WriteString("export ")
		content.WriteString(key)
		content.WriteByte('=')
		content.WriteString(shellCommand([]string{values[key]}))
		content.WriteByte('\n')
	}

	// This short-lived handoff keeps preserved values out of tmux argv. The
	// file is 0600 and cleaned on tmux launch failure and pane shell exit, but
	// it is not intended to be a same-user sandbox boundary.
	file, err := os.CreateTemp(tmuxPaneEnvironmentTempDir(), "kenn-forge-tmux-env-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.WriteString(content.String()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func tmuxPaneEnvironmentTempDir() string {
	return os.Getenv("KENN_FORGE_TMUX_ENV_DIR")
}

func (l tmuxLauncher) hasSessionCommand() []string {
	return append(slices.Clone(l.TmuxCommand), "has-session", "-t", l.Session)
}

func (l tmuxLauncher) showOwnerCommand() []string {
	return append(
		slices.Clone(l.TmuxCommand),
		"show-options", "-qv", "-t", l.Session, "@forge_owner",
	)
}

func (l tmuxLauncher) newSessionCommand(paneCommand string) []string {
	command := append(slices.Clone(l.TmuxCommand), "new-session")
	command = append(command, "-E", "-d", "-s", l.Session)
	if l.CWD != "" {
		command = append(command, "-c", l.CWD)
	}
	command = append(command, paneCommand)
	if l.OwnerMarker != "" {
		command = append(
			command,
			";", "set-option", "-q", "-t", l.Session,
			"@forge_owner", l.OwnerMarker,
		)
	}
	if l.LaunchID != "" {
		command = append(
			command,
			";", "set-option", "-q", "-t", l.Session,
			"@forge_launch", l.LaunchID,
		)
	}
	return command
}

func (l tmuxLauncher) hideStatusCommand() []string {
	return append(
		slices.Clone(l.TmuxCommand),
		"set-option", "-q", "-t", l.Session, "status", "off",
	)
}

func (l tmuxLauncher) passthroughCommand() []string {
	value := "off"
	if l.Graphics {
		value = "on"
	}
	return append(
		slices.Clone(l.TmuxCommand),
		"set-option", "-q", "-p", "-t", l.Session,
		"allow-passthrough", value,
	)
}

func (l tmuxLauncher) globalPassthroughCommand() []string {
	value := "off"
	if l.Graphics {
		value = "on"
	}
	return append(
		slices.Clone(l.TmuxCommand),
		"set-option", "-q", "-g", "allow-passthrough", value,
	)
}

func (l tmuxLauncher) sixelCommand() []string {
	command := append(
		slices.Clone(l.TmuxCommand), "set-option", "-q", "-s",
	)
	if !l.Graphics {
		return append(command, "-u", "terminal-features[100]")
	}
	return append(command, "terminal-features[100]", "xterm-256color:sixel")
}

func (l tmuxLauncher) tmuxMouseCommand() []string {
	value := "off"
	if l.TmuxMouse {
		value = "on"
	}
	return append(
		slices.Clone(l.TmuxCommand),
		"set-option", "-q", "-g", "mouse", value,
	)
}

func (l tmuxLauncher) killSessionCommand() []string {
	return append(
		slices.Clone(l.TmuxCommand), "kill-session", "-t", l.Session,
	)
}

func (l tmuxLauncher) attachSessionCommand() []string {
	return tmuxAttachSessionCommand(l.TmuxCommand, l.Session)
}

func tmuxAttachSessionCommand(command []string, session string) []string {
	// Kenn Forge may run as a service without locale variables. Force UTF-8 so
	// tmux does not replace non-ASCII terminal output with underscores.
	// -E disables update-environment: a pane can widen that server
	// option, and without -E the next attach would copy the attach
	// client's variables into the session environment.
	return append(
		slices.Clone(command),
		"-u", "attach-session", "-E", "-t", session,
	)
}

func shellCommand(command []string) string {
	return shellquote.Join(command...)
}

func tmuxSessionEnvironment(env []string, extraStrip []string) []string {
	sanitized := sessionEnvironment(env, extraStrip)
	out := make([]string, 0, len(sanitized))
	for _, kv := range sanitized {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		if shouldAllowTmuxSessionVar(key) {
			out = append(out, kv)
		}
	}
	return out
}

// IsShellIdentifier reports whether value is a valid POSIX shell variable
// name, the requirement for env keys passed to command sessions.
func IsShellIdentifier(value string) bool {
	return isShellIdentifier(value)
}

func isShellIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '_':
			continue
		case i > 0 && r >= '0' && r <= '9':
			continue
		default:
			return false
		}
	}
	return true
}

func shouldAllowTmuxSessionVar(key string) bool {
	return shouldAllowTmuxSessionVarFold(key, runtime.GOOS == "windows")
}

// shouldAllowTmuxSessionVarFold admits by exact name on case-sensitive
// platforms; on Windows, where environment names resolve
// case-insensitively, "Path" is PATH and folding is correct. Admission
// must stay exact elsewhere or unrelated variables such as a Unix
// "editor" would enter tmux's retained environment.
func shouldAllowTmuxSessionVarFold(key string, fold bool) bool {
	if fold {
		return config.IsTmuxNonSecretEnvVar(key)
	}
	return config.IsTmuxNonSecretEnvVarExact(key)
}
