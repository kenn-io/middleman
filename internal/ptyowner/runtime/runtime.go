package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"path/filepath"
	"strings"

	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/ptyowner"
	"go.kenn.io/forge/internal/ptysize"
)

type Owner interface {
	HasState(session string) bool
	Attach(ctx context.Context, session string) (PTY, error)
	Start(
		ctx context.Context,
		session string,
		cwd string,
		command []string,
		stripEnvVars []string,
		extraEnv map[string]string,
	) (PTY, error)
	Stop(ctx context.Context, session string) error
}

type PTY interface {
	Output() <-chan []byte
	Done() <-chan struct{}
	Write([]byte) error
	Resize(ptysize.Geometry) error
	ExitCode() int
	Close()
}

type ExecutableResolver func(string) (string, error)

type owner struct {
	client  *ptyowner.Client
	resolve ExecutableResolver
}

type ownedPTY struct {
	attachment *ptyowner.Attachment
}

func New(client *ptyowner.Client, resolve ExecutableResolver) Owner {
	if client == nil {
		return nil
	}
	if resolve == nil {
		resolve = ResolveExecutable
	}
	return owner{client: client, resolve: resolve}
}

func (o owner) HasState(session string) bool {
	if o.client == nil {
		return false
	}
	return o.client.HasState(session)
}

func (o owner) Attach(ctx context.Context, session string) (PTY, error) {
	if o.client == nil {
		return nil, errors.New("pty owner runtime is unavailable")
	}
	attachment, err := o.client.Attach(
		context.WithoutCancel(ctx), session,
		ptysize.FallbackGeometry(120, 30),
	)
	if err != nil {
		return nil, err
	}
	return ownedPTY{attachment: attachment}, nil
}

func (o owner) Start(
	ctx context.Context,
	session string,
	cwd string,
	command []string,
	stripEnvVars []string,
	extraEnv map[string]string,
) (PTY, error) {
	if o.client == nil {
		return nil, errors.New("pty owner runtime is unavailable")
	}
	if len(command) == 0 || command[0] == "" {
		return nil, errors.New("session command is empty")
	}
	resolvedPath, err := o.resolve(command[0])
	if err != nil {
		return nil, err
	}
	resolvedCommand := append([]string{resolvedPath}, command[1:]...)
	slog.Debug(
		"runtime session resolving command",
		"session_key", session,
		"program", resolvedPath,
		"argc", len(command),
		"cwd", cwd,
		"pty_backend", "pty_owner",
	)
	// Build a fresh per-launch client rather than copying o.client: the
	// shared client carries a mutex guarding its reloadable strip set.
	env := map[string]string{}
	maps.Copy(env, extraEnv)
	env[agentactivity.RuntimeSessionKeyEnv] = session
	client := ptyowner.Client{
		Root:        o.client.Root,
		ExePath:     o.client.ExePath,
		ExeArgs:     append([]string(nil), o.client.ExeArgs...),
		ManagerPath: o.client.ManagerPath,
		InProcess:   o.client.InProcess,
		Command:     resolvedCommand,
		ExtraEnv:    env,
		StripEnvVars: append(
			o.client.StripEnvVarsSnapshot(), stripEnvVars...,
		),
	}
	if err := client.Ensure(ctx, session, cwd); err != nil {
		return nil, err
	}
	// The caller's launch context may end while the runtime session continues;
	// this attachment is the long-lived drain for the owner-managed PTY.
	attachment, err := client.Attach(
		context.WithoutCancel(ctx), session,
		ptysize.FallbackGeometry(120, 30),
	)
	if err != nil {
		_ = client.Stop(ctx, session)
		return nil, err
	}
	return ownedPTY{attachment: attachment}, nil
}

func (o owner) Stop(ctx context.Context, session string) error {
	if o.client == nil {
		return nil
	}
	return o.client.Stop(ctx, session)
}

func (p ownedPTY) Output() <-chan []byte {
	if p.attachment == nil {
		ch := make(chan []byte)
		close(ch)
		return ch
	}
	return p.attachment.Output
}

func (p ownedPTY) Done() <-chan struct{} {
	if p.attachment == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return p.attachment.Done
}

func (p ownedPTY) Write(data []byte) error {
	if p.attachment == nil {
		return errors.New("pty owner attachment is closed")
	}
	return p.attachment.Write(data)
}

func (p ownedPTY) Resize(geometry ptysize.Geometry) error {
	if p.attachment == nil {
		return errors.New("pty owner attachment is closed")
	}
	return p.attachment.Resize(geometry)
}

func (p ownedPTY) ExitCode() int {
	if p.attachment == nil {
		return -1
	}
	return p.attachment.ExitCode()
}

func (p ownedPTY) Close() {
	if p.attachment != nil {
		p.attachment.Close()
	}
}

// ResolveExecutable returns an absolute path for name. Names that are already
// absolute are accepted as-is; names without a path separator are looked up via
// PATH; relative names with separators are rejected because the PTY cwd may be
// an untrusted worktree.
func ResolveExecutable(name string) (string, error) {
	if name == "" {
		return "", errors.New("session command is empty")
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	if !hasRelativePathSyntax(name) {
		path, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf(
				"resolve session command %q via PATH: %w",
				name, err,
			)
		}
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return "", fmt.Errorf(
					"resolve session command %q via PATH: %w",
					name, err,
				)
			}
			path = abs
		}
		return path, nil
	}
	return "", fmt.Errorf(
		"session command %q must be an absolute path or a "+
			"PATH-resolvable name; relative paths resolve inside "+
			"the PTY working directory, which may be untrusted",
		name,
	)
}

func hasRelativePathSyntax(name string) bool {
	if strings.ContainsAny(name, `/\\`) {
		return true
	}
	return len(name) >= 2 && name[1] == ':'
}
