// Package testtmux owns private tmux servers started by Go tests.
package testtmux

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/procutil"
)

const (
	startTokenLength      = 12
	cleanupTimeout        = 2 * time.Second
	rootLockName          = ".owner.lock"
	admissionPrefix       = "admission."
	admissionLockName     = "lock"
	admissionClosedName   = "closed"
	wrapperExecutableName = "tmux-wrapper"
	wrapperMetadataName   = "wrapper.json"
)

var runNamePattern = regexp.MustCompile(
	`^run\.([1-9][0-9]*)\.([0-9a-f]{12})\.([A-Za-z0-9]{6,32})$`,
)

var rootLockMu sync.Mutex

type processIdentity struct {
	pid        int
	startToken string
}

type registeredServer struct {
	tmuxPath string
	socket   string
}

// Owner tracks private tmux servers for one test binary.
type Owner struct {
	root         string
	runDir       string
	admissionDir string
	reaper       *ownerReaper
	mu           sync.Mutex
	servers      map[string]registeredServer
	closed       bool
	cleanup      sync.Once
	cleanupErr   error
}

type ownerMarker struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start"`
	StartToken   string `json:"start_token"`
}

type wrapperMetadata struct {
	TmuxPath     string `json:"tmux_path"`
	AdmissionDir string `json:"admission_dir"`
}

// New creates an owner beneath a stable, per-user temporary root. It reaps
// stale owners before publishing the new run.
func New() (*Owner, error) {
	if !Supported() {
		return nil, errors.New("private test tmux servers are unsupported on Windows")
	}
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current user: %w", err)
	}
	uid := current.Uid
	if uid == "" || strings.ContainsAny(uid, `/\\`) {
		return nil, fmt.Errorf("invalid current user ID %q", uid)
	}
	return newAt(filepath.Join(tmuxRootBase(), "kenn-forge-tmux-"+uid))
}

func newAt(root string) (*Owner, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("tmux test root must be absolute: %s", root)
	}
	root = filepath.Clean(root)
	// Bootstrap the directory so it can contain its stable lock file. The
	// validated initialization is repeated while holding the lock below.
	if err := prepareRoot(root); err != nil {
		return nil, err
	}
	var owner *Owner
	err := withRootLock(root, func() error {
		if err := prepareRoot(root); err != nil {
			return err
		}
		if err := reapStale(root); err != nil {
			return err
		}
		start, err := processStart(os.Getpid())
		if err != nil {
			return fmt.Errorf("identify tmux test owner: %w", err)
		}
		identity := processIdentity{
			pid:        os.Getpid(),
			startToken: tokenForStart(start),
		}
		nonce, err := randomToken(6)
		if err != nil {
			return fmt.Errorf("create tmux test run nonce: %w", err)
		}
		runName := fmt.Sprintf(
			"run.%d.%s.%s", identity.pid, identity.startToken, nonce,
		)
		marker := ownerMarker{
			PID:          identity.pid,
			ProcessStart: start,
			StartToken:   identity.startToken,
		}
		runDir, admissionDir, err := publishOwnerState(
			root, runName, marker, os.Rename,
		)
		if err != nil {
			return err
		}
		owner = &Owner{
			root:         root,
			runDir:       runDir,
			admissionDir: admissionDir,
			servers:      make(map[string]registeredServer),
		}
		owner.reaper, err = startOwnerReaper(runDir)
		if err != nil {
			cleanupErr := errors.Join(
				fmt.Errorf("start private tmux owner reaper: %w", err),
				os.RemoveAll(runDir),
				os.RemoveAll(admissionDir),
			)
			owner = nil
			return cleanupErr
		}
		return nil
	})
	return owner, err
}

func withRootLock(root string, fn func() error) (err error) {
	// flock does not serialize fresh lock handles within every supported Unix
	// process, so pair it with a package mutex for goroutines in this binary.
	rootLockMu.Lock()
	defer rootLockMu.Unlock()

	lock := flock.New(filepath.Join(root, rootLockName))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("acquire tmux test root lock: %w", err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"release tmux test root lock: %w", unlockErr,
			))
		}
	}()
	return fn()
}

func publishRun(
	root string,
	runName string,
	marker ownerMarker,
	rename func(string, string) error,
) (string, error) {
	stagingDir := filepath.Join(root, "stage."+runName)
	finalDir := filepath.Join(root, runName)
	if err := os.Mkdir(stagingDir, 0o700); err != nil {
		return "", fmt.Errorf("create staged tmux test run: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	content, err := json.Marshal(marker)
	if err != nil {
		return "", fmt.Errorf("encode tmux test owner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "owner.json"), content, 0o600); err != nil {
		return "", fmt.Errorf("write tmux test owner: %w", err)
	}
	if err := rename(stagingDir, finalDir); err != nil {
		return "", fmt.Errorf("publish tmux test run: %w", err)
	}
	removeStaging = false
	return finalDir, nil
}

func publishOwnerState(
	root string,
	runName string,
	marker ownerMarker,
	rename func(string, string) error,
) (string, string, error) {
	admissionDir := filepath.Join(root, admissionPrefix+runName)
	if err := os.Mkdir(admissionDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create private tmux admission directory: %w", err)
	}
	runDir, err := publishRun(root, runName, marker, rename)
	if err != nil {
		if removeErr := os.RemoveAll(admissionDir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"remove private tmux admission directory: %w", removeErr,
			))
		}
		return "", "", err
	}
	return runDir, admissionDir, nil
}

// Command registers a private socket before returning a tmux command prefix.
func (o *Owner) Command(t testing.TB, tmuxPath string) []string {
	t.Helper()
	command, server, err := o.register(tmuxPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, o.release(server))
	})
	return command
}

// CommandForRun registers a private socket for the lifetime of the owner.
func (o *Owner) CommandForRun(tmuxPath string) ([]string, error) {
	command, _, err := o.register(tmuxPath)
	return command, err
}

func (o *Owner) register(tmuxPath string) ([]string, registeredServer, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, registeredServer{}, errors.New("private tmux owner is cleaning up")
	}

	nonce, err := randomToken(6)
	if err != nil {
		return nil, registeredServer{}, err
	}
	serverDir := filepath.Join(o.runDir, "server-"+nonce)
	if err := os.Mkdir(serverDir, 0o700); err != nil {
		return nil, registeredServer{}, fmt.Errorf("create tmux test server directory: %w", err)
	}
	removeServerDir := true
	defer func() {
		if removeServerDir {
			_ = os.RemoveAll(serverDir)
		}
	}()
	socket := filepath.Join(serverDir, "tmux.sock")
	executable, err := os.Executable()
	if err != nil {
		return nil, registeredServer{}, fmt.Errorf("resolve tmux test wrapper executable: %w", err)
	}
	metadata, err := json.Marshal(wrapperMetadata{
		TmuxPath:     tmuxPath,
		AdmissionDir: o.admissionDir,
	})
	if err != nil {
		return nil, registeredServer{}, fmt.Errorf("encode tmux test wrapper metadata: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(serverDir, wrapperMetadataName), metadata, 0o600,
	); err != nil {
		return nil, registeredServer{}, fmt.Errorf("write tmux test wrapper metadata: %w", err)
	}
	wrapperPath := filepath.Join(serverDir, wrapperExecutableName)
	if err := os.Symlink(executable, wrapperPath); err != nil {
		return nil, registeredServer{}, fmt.Errorf("link tmux test wrapper: %w", err)
	}
	server := registeredServer{tmuxPath: tmuxPath, socket: socket}
	o.servers[socket] = server
	removeServerDir = false
	return []string{wrapperPath, "-f", "/dev/null", "-S", socket}, server, nil
}

// CommandWrapperExitCode runs a private tmux command when the current test
// binary was invoked through an Owner command wrapper.
func CommandWrapperExitCode() (int, bool) {
	if code, ok := ownerReaperExitCode(); ok {
		return code, true
	}
	if filepath.Base(os.Args[0]) != wrapperExecutableName {
		return 0, false
	}
	metadataPath := filepath.Join(filepath.Dir(os.Args[0]), wrapperMetadataName)
	content, err := os.ReadFile(metadataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read private tmux wrapper metadata: %v\n", err)
		return 1, true
	}
	var metadata wrapperMetadata
	if err := json.Unmarshal(content, &metadata); err != nil {
		fmt.Fprintf(os.Stderr, "decode private tmux wrapper metadata: %v\n", err)
		return 1, true
	}
	return runWrappedTmux(metadata, os.Args[1:]), true
}

func runWrappedTmux(metadata wrapperMetadata, args []string) int {
	var activePath string
	if slices.Contains(args, "new-session") {
		lock := flock.New(filepath.Join(metadata.AdmissionDir, admissionLockName))
		if err := lock.Lock(); err != nil {
			fmt.Fprintf(os.Stderr, "acquire private tmux admission lock: %v\n", err)
			return 1
		}
		if _, err := os.Stat(filepath.Join(metadata.AdmissionDir, admissionClosedName)); err == nil {
			_ = lock.Unlock()
			fmt.Fprintln(os.Stderr, "private tmux owner is cleaning up")
			return 1
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = lock.Unlock()
			fmt.Fprintf(os.Stderr, "inspect private tmux admission gate: %v\n", err)
			return 1
		}
		start, err := processStart(os.Getpid())
		if err != nil {
			_ = lock.Unlock()
			fmt.Fprintf(os.Stderr, "identify private tmux wrapper: %v\n", err)
			return 1
		}
		activePath = filepath.Join(metadata.AdmissionDir, fmt.Sprintf(
			"starting.%d.%s", os.Getpid(), tokenForStart(start),
		))
		if err := os.WriteFile(activePath, []byte(start), 0o600); err != nil {
			_ = lock.Unlock()
			fmt.Fprintf(os.Stderr, "record private tmux startup: %v\n", err)
			return 1
		}
		if err := lock.Unlock(); err != nil {
			_ = os.Remove(activePath)
			fmt.Fprintf(os.Stderr, "release private tmux admission lock: %v\n", err)
			return 1
		}
		defer func() { _ = os.Remove(activePath) }()
	}

	if err := replaceWithTmux(metadata.TmuxPath, args); err != nil {
		if activePath != "" {
			_ = os.Remove(activePath)
		}
		fmt.Fprintf(os.Stderr, "run private tmux command: %v\n", err)
		return 1
	}
	return 0
}

// Cleanup stops every registered server and removes this owner's run.
func (o *Owner) Cleanup() error {
	o.cleanup.Do(func() {
		defer func() {
			if o.reaper == nil {
				return
			}
			if o.cleanupErr != nil {
				o.cleanupErr = errors.Join(o.cleanupErr, o.reaper.cancel())
				return
			}
			o.cleanupErr = o.reaper.stop()
		}()

		o.mu.Lock()
		o.closed = true
		o.mu.Unlock()

		var cleanupErrors []error
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		gateErr := closeAdmission(ctx, o.admissionDir)
		if gateErr == nil {
			gateErr = waitForAdmittedCreations(ctx, []string{o.admissionDir})
		}
		if gateErr != nil {
			o.cleanupErr = gateErr
			return
		}

		o.mu.Lock()
		servers := make([]registeredServer, 0, len(o.servers))
		for _, server := range o.servers {
			servers = append(servers, server)
		}
		o.mu.Unlock()

		for _, server := range servers {
			if err := o.release(server); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		processErr := killRunProcesses(o.runDir)
		runRemoved := false
		if processErr != nil {
			cleanupErrors = append(cleanupErrors, processErr)
		} else if err := os.RemoveAll(o.runDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		} else {
			runRemoved = true
		}
		if runRemoved {
			if err := os.RemoveAll(o.admissionDir); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		o.cleanupErr = errors.Join(cleanupErrors...)
	})
	return o.cleanupErr
}

func closeAdmission(ctx context.Context, admissionDir string) (err error) {
	lock := flock.New(filepath.Join(admissionDir, admissionLockName))
	locked, lockErr := lock.TryLockContext(ctx, 10*time.Millisecond)
	if lockErr != nil {
		return fmt.Errorf("close private tmux admission gate: %w", lockErr)
	}
	if !locked {
		return errors.New("close private tmux admission gate: lock unavailable")
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"release private tmux admission gate: %w", unlockErr,
			))
		}
	}()
	if err := os.WriteFile(
		filepath.Join(admissionDir, admissionClosedName), []byte("closed\n"), 0o600,
	); err != nil {
		return fmt.Errorf("close private tmux admission gate: %w", err)
	}
	return nil
}

func waitForAdmittedCreations(ctx context.Context, admissionDirs []string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		startups, err := admittedCreations(admissionDirs)
		if err != nil {
			return err
		}
		if len(startups) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return stopAdmittedCreations(startups)
		case <-ticker.C:
		}
	}
}

func admittedCreations(admissionDirs []string) ([]tmuxProcess, error) {
	return admittedCreationsWithLookup(admissionDirs, processStart)
}

func admittedCreationsWithLookup(
	admissionDirs []string,
	lookupStart processStartLookup,
) ([]tmuxProcess, error) {
	seen := make(map[tmuxProcess]bool)
	for _, admissionDir := range admissionDirs {
		markers, err := filepath.Glob(filepath.Join(admissionDir, "starting.*"))
		if err != nil {
			return nil, fmt.Errorf("list admitted private tmux startups: %w", err)
		}
		for _, marker := range markers {
			name := filepath.Base(marker)
			parts := strings.Split(name, ".")
			if len(parts) != 3 || parts[0] != "starting" {
				return nil, fmt.Errorf(
					"invalid admitted private tmux startup marker %s", marker,
				)
			}
			pid, parseErr := strconv.Atoi(parts[1])
			start, readErr := os.ReadFile(marker)
			if readErr != nil {
				_, statErr := os.Lstat(marker)
				if errors.Is(statErr, os.ErrNotExist) {
					continue
				}
				if statErr != nil {
					readErr = errors.Join(readErr, statErr)
				}
				return nil, fmt.Errorf(
					"read admitted private tmux startup marker %s: %w",
					marker, readErr,
				)
			}
			if parseErr != nil || pid <= 0 || len(start) == 0 ||
				tokenForStart(string(start)) != parts[2] {
				return nil, fmt.Errorf(
					"invalid admitted private tmux startup marker %s", marker,
				)
			}
			status, statusErr := exactProcessIdentityState(
				pid, string(start), lookupStart,
			)
			if statusErr != nil {
				return nil, fmt.Errorf(
					"inspect admitted private tmux startup %s: %w", marker, statusErr,
				)
			}
			if status == processIdentityAbsent {
				_ = os.Remove(marker)
				continue
			}
			seen[tmuxProcess{pid: pid, start: string(start)}] = true
		}
	}
	startups := make([]tmuxProcess, 0, len(seen))
	for startup := range seen {
		startups = append(startups, startup)
	}
	return startups, nil
}

func stopAdmittedCreations(startups []tmuxProcess) error {
	errorsByProcess := make(chan error, len(startups))
	var wait sync.WaitGroup
	for _, startup := range startups {
		wait.Go(func() {
			errorsByProcess <- stopProcess(startup.pid, startup.start)
		})
	}
	wait.Wait()
	close(errorsByProcess)
	var cleanupErrors []error
	for err := range errorsByProcess {
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func (o *Owner) release(server registeredServer) error {
	o.mu.Lock()
	_, registered := o.servers[server.socket]
	o.mu.Unlock()
	if !registered {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	command := procutil.CommandContext(
		ctx, server.tmuxPath, "-f", "/dev/null", "-S", server.socket,
		"kill-server",
	)
	_ = command.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("stop tmux test server %s: %w", server.socket, ctx.Err())
	}
	if err := os.RemoveAll(filepath.Dir(server.socket)); err != nil {
		return fmt.Errorf("remove tmux test server directory: %w", err)
	}
	o.mu.Lock()
	delete(o.servers, server.socket)
	o.mu.Unlock()
	return nil
}

func prepareRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create tmux test root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect tmux test root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("refusing insecure tmux test root: %s", root)
	}
	if err := validateDirectoryOwner(info); err != nil {
		return fmt.Errorf("refusing tmux test root ownership: %w", err)
	}
	return nil
}

func reapStale(root string) error {
	return reapStaleWithLookup(root, processStart)
}

func reapStaleWithLookup(
	root string,
	lookupStart processStartLookup,
) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list tmux test root: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	type staleAdmission struct {
		runName string
		dir     string
	}
	type gateState uint8
	const (
		gateDeferred gateState = iota + 1
		gateFailed
		gateSafe
	)
	var staleAdmissions []staleAdmission
	gateStates := make(map[string]gateState)
	for _, entry := range entries {
		identity, ok := parseAdmissionName(entry.Name())
		if !ok {
			continue
		}
		runName := strings.TrimPrefix(entry.Name(), admissionPrefix)
		ownerStatus, ownerErr := runProcessIdentityState(identity, lookupStart)
		if ownerErr != nil {
			gateStates[runName] = gateFailed
			cleanupErrors = append(cleanupErrors, ownerErr)
			continue
		}
		if ownerStatus == processIdentityLive {
			gateStates[runName] = gateDeferred
			continue
		}
		gateStates[runName] = gateFailed
		admissionDir := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(admissionDir)
		if statErr != nil {
			if !errors.Is(statErr, fs.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, statErr)
			}
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm() != 0o700 || validateDirectoryOwner(info) != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"refusing insecure stale tmux admission directory: %s",
				admissionDir,
			))
			continue
		}
		if err := closeAdmission(ctx, admissionDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		staleAdmissions = append(staleAdmissions, staleAdmission{
			runName: runName,
			dir:     admissionDir,
		})
	}
	if len(staleAdmissions) > 0 {
		admissionDirs := make([]string, 0, len(staleAdmissions))
		for _, admission := range staleAdmissions {
			admissionDirs = append(admissionDirs, admission.dir)
		}
		if err := waitForAdmittedCreations(ctx, admissionDirs); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			staleAdmissions = nil
		} else {
			for _, admission := range staleAdmissions {
				gateStates[admission.runName] = gateSafe
			}
		}
	}
	for _, entry := range entries {
		identity, ok := parseRunName(entry.Name())
		if !ok {
			continue
		}
		switch gateStates[entry.Name()] {
		case gateDeferred, gateFailed:
			continue
		case gateSafe:
		default:
			ownerStatus, ownerErr := runProcessIdentityState(identity, lookupStart)
			if ownerErr != nil {
				cleanupErrors = append(cleanupErrors, ownerErr)
				continue
			}
			if ownerStatus == processIdentityLive {
				continue
			}
			runDir := filepath.Join(root, entry.Name())
			if err := validateRunMarker(runDir, identity); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			} else {
				cleanupErrors = append(cleanupErrors, fmt.Errorf(
					"refusing stale tmux test run without admission gate: %s",
					runDir,
				))
			}
			continue
		}
		runDir := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(runDir)
		if statErr != nil {
			if !errors.Is(statErr, fs.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, statErr)
			}
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm() != 0o700 || validateDirectoryOwner(info) != nil {
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("refusing insecure stale tmux test run: %s", runDir),
			)
			continue
		}
		if err := validateRunMarker(runDir, identity); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if err := killSocketsIn(runDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if err := killRunProcesses(runDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if err := os.RemoveAll(runDir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
	}
	if err := reapStaleProcesses(root); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	for _, admission := range staleAdmissions {
		_, statErr := os.Lstat(filepath.Join(root, admission.runName))
		if statErr == nil {
			continue
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, statErr)
			continue
		}
		if err := os.RemoveAll(admission.dir); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func validateRunMarker(runDir string, identity processIdentity) error {
	path := filepath.Join(runDir, "owner.json")
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect stale tmux test owner %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		validateDirectoryOwner(info) != nil {
		return fmt.Errorf("refusing insecure stale tmux test owner: %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read stale tmux test owner %s: %w", path, err)
	}
	var marker ownerMarker
	if err := json.Unmarshal(content, &marker); err != nil {
		return fmt.Errorf("decode stale tmux test owner %s: %w", path, err)
	}
	if marker.PID != identity.pid || marker.StartToken != identity.startToken ||
		tokenForStart(marker.ProcessStart) != identity.startToken {
		return fmt.Errorf("refusing mismatched stale tmux test owner: %s", path)
	}
	return nil
}

func killSocketsIn(runDir string) error {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return nil
	}
	return filepath.WalkDir(runDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = procutil.CommandContext(ctx, tmuxPath, "-S", path, "kill-server").Run()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("stop stale tmux test server %s: %w", path, ctx.Err())
		}
		return nil
	})
}

func reapStaleProcesses(root string) error {
	processes, err := tmuxProcesses()
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, process := range processes {
		socket, ok := explicitSocket(process.command)
		if !ok {
			continue
		}
		identity, runDir, ok := runForSocket(root, socket)
		if !ok {
			continue
		}
		ownerStatus, ownerErr := runProcessIdentityState(identity, processStart)
		if ownerErr != nil {
			cleanupErrors = append(cleanupErrors, ownerErr)
			continue
		}
		if ownerStatus == processIdentityLive {
			continue
		}
		if _, statErr := os.Lstat(runDir); statErr == nil ||
			!errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if err := stopProcess(process.pid, process.start); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func killRunProcesses(runDir string) error {
	processes, err := tmuxProcesses()
	if err != nil {
		return err
	}
	runPrefix := filepath.Clean(runDir) + string(filepath.Separator)
	var cleanupErrors []error
	for _, process := range processes {
		socket, ok := explicitSocket(process.command)
		if !ok || !strings.HasPrefix(filepath.Clean(socket), runPrefix) {
			continue
		}
		if err := stopProcess(process.pid, process.start); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func explicitSocket(command string) (string, bool) {
	if socket, ok := tmuxServerTitleSocket(command); ok {
		return socket, true
	}
	fields := strings.Fields(command)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "-S" && filepath.IsAbs(fields[index+1]) {
			return filepath.Clean(fields[index+1]), true
		}
	}
	return "", false
}

func tmuxServerTitleSocket(command string) (string, bool) {
	const prefix = "tmux: server ("
	if !strings.HasPrefix(command, prefix) || !strings.HasSuffix(command, ")") {
		return "", false
	}
	socket := strings.TrimSuffix(strings.TrimPrefix(command, prefix), ")")
	if !filepath.IsAbs(socket) || strings.ContainsAny(socket, " \t\r\n()") {
		return "", false
	}
	return filepath.Clean(socket), true
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func tokenForStart(start string) string {
	sum := sha256.Sum256([]byte(start))
	return hex.EncodeToString(sum[:])[:startTokenLength]
}

func parseRunName(name string) (processIdentity, bool) {
	match := runNamePattern.FindStringSubmatch(name)
	if match == nil {
		return processIdentity{}, false
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil || pid <= 0 {
		return processIdentity{}, false
	}
	return processIdentity{pid: pid, startToken: match[2]}, true
}

func parseAdmissionName(name string) (processIdentity, bool) {
	if !strings.HasPrefix(name, admissionPrefix) {
		return processIdentity{}, false
	}
	return parseRunName(strings.TrimPrefix(name, admissionPrefix))
}

func identityForSocket(root, socket string) (processIdentity, bool) {
	identity, _, ok := runForSocket(root, socket)
	return identity, ok
}

func runForSocket(root, socket string) (processIdentity, string, bool) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(socket) {
		return processIdentity{}, "", false
	}
	root = filepath.Clean(root)
	socket = filepath.Clean(socket)
	relative, err := filepath.Rel(root, socket)
	if err != nil || relative == "." || relative == ".." ||
		filepath.IsAbs(relative) {
		return processIdentity{}, "", false
	}
	parts := splitPath(relative)
	if len(parts) < 3 || parts[len(parts)-1] != "tmux.sock" {
		return processIdentity{}, "", false
	}
	identity, ok := parseRunName(parts[0])
	return identity, filepath.Join(root, parts[0]), ok
}

func splitPath(path string) []string {
	var parts []string
	for path != "." && path != string(filepath.Separator) && path != "" {
		dir, base := filepath.Split(path)
		if base == "" || base == "." || base == ".." {
			return nil
		}
		parts = append([]string{base}, parts...)
		path = filepath.Clean(dir)
	}
	return parts
}
