package gitclone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/tokenauth"
	providerplatform "go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
	"golang.org/x/sync/singleflight"
)

// ensureCloneTimeout caps how long a single bare-clone create-or-fetch
// is allowed to run inside the singleflight slot. The slot is detached
// from caller cancellation so one canceled waiter cannot abort work for
// others; the timeout is what prevents a stuck git subprocess from
// holding the slot forever. Generous enough to cover large initial
// clones over slow links, short enough to recover from a wedged
// network connection inside one sync interval.
const ensureCloneTimeout = 15 * time.Minute

// ErrNotFound is returned when a git ref or object cannot be resolved.
var ErrNotFound = errors.New("git object not found")

// ErrCredentialUnavailable reports that a daemon-local networked Git operation
// has no exact credential route for its verified repository.
var ErrCredentialUnavailable = errors.New("git credential unavailable")

// RouteResolver selects mutation-capable credentials for managed Git.
type RouteResolver interface {
	SourceForRepo(platform, host, owner, name string) tokenauth.Source
	FallbackSource(host string) tokenauth.Source
}

// HostSources adapts host fallback sources for providers without repository
// credential routes.
type HostSources map[string]tokenauth.Source

func (s HostSources) SourceForRepo(_, host, _, _ string) tokenauth.Source {
	return s[host]
}

func (s HostSources) FallbackSource(host string) tokenauth.Source {
	return s[host]
}

// Manager manages bare git clones for diff computation.
type Manager struct {
	baseDir string
	routes  RouteResolver

	// ensureFlights deduplicates concurrent EnsureClone calls for the same
	// storage namespace and repository while retaining the slot until every
	// caller has run its own route validation.
	ensureMu      sync.Mutex
	ensureFlights map[string]*ensureCloneFlight

	repoBrowserRefreshSF singleflight.Group
	repoBrowserMu        sync.Mutex
	repoBrowserRepos     map[string]RepoBrowserRepoRef

	// ancestryVisitBudget overrides maxAncestryVisits when positive; tests
	// use it to exercise the budget without building enormous histories.
	ancestryVisitBudget  int
	repoBrowserBarrierMu sync.Mutex
	repoBrowserBarriers  map[string]*repoBrowserBarrier
	transportPolicyMu    sync.RWMutex
	allowInsecureHTTP    map[string]struct{}

	// Deterministic synchronization and failure-injection hooks for
	// repository-browser concurrency tests and clone cleanup tests. Tests set
	// these before starting goroutines.
	repoBrowserReadWaitingForTest      func(string)
	repoBrowserAfterReadLockForTest    func(string)
	repoBrowserAfterRefreshJoinForTest func(RepoBrowserRouteFence)
	repoBrowserFetchErrorForTest       func(RepoBrowserRouteFence) error
	removeRepoBrowserStagingForTest    func(string) error
	publishRepoBrowserStagingForTest   func(string, string) error
	removeCloneAsideForTest            func(string) error
}

type ensureCloneFlight struct {
	done     chan struct{}
	released chan struct{}
	err      error
	complete bool
	waiters  int
}

// cloneValidationError marks a slot whose fetch succeeded but whose
// starter's route validation failed, removing a new clone or rolling an
// existing clone back. Followers distinguish it from a fetch failure so a
// caller whose own route still owns the path can retry with a self-validated
// fetch. Unwrap keeps errors.Is checks working through the marker.
type cloneValidationError struct{ err error }

func (e *cloneValidationError) Error() string { return e.err.Error() }

func (e *cloneValidationError) Unwrap() error { return e.err }

type repositoryIdentityContextKey struct{}
type requiredCredentialContextKey struct{}

// WithRepositoryIdentity partitions clone-backed work by the provider's
// stable repository identity. Callers should set this after reconciling a
// mutable owner/name route so route reuse cannot share clone state or an
// in-flight fetch between distinct repositories.
func WithRepositoryIdentity(ctx context.Context, providerRepoID string) context.Context {
	providerRepoID = strings.TrimSpace(providerRepoID)
	return context.WithValue(ctx, repositoryIdentityContextKey{}, providerRepoID)
}

// WithRequiredCredential makes every networked Git command in ctx fail closed
// instead of falling back to anonymous access when an exact route disappears.
func WithRequiredCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, requiredCredentialContextKey{}, true)
}

// RequireCredentialRoute admits daemon-local clone work only when the executing
// daemon has an exact repository credential that resolves to a non-empty token.
func (m *Manager) RequireCredentialRoute(
	ctx context.Context, platform, host, owner, name string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source := m.sourceForRepo(platform, host, owner, name)
	if source == nil {
		return fmt.Errorf("%w for %s/%s", ErrCredentialUnavailable, owner, name)
	}
	token, err := source.Token(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrCredentialUnavailable, err)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w for %s/%s", ErrCredentialUnavailable, owner, name)
	}
	return nil
}

// New creates a Manager that stores bare clones under baseDir. A nil resolver
// means all operations proceed without auth.
func New(baseDir string, routes RouteResolver) *Manager {
	return &Manager{
		baseDir:             baseDir,
		routes:              routes,
		ensureFlights:       make(map[string]*ensureCloneFlight),
		repoBrowserRepos:    make(map[string]RepoBrowserRepoRef),
		repoBrowserBarriers: make(map[string]*repoBrowserBarrier),
		allowInsecureHTTP:   make(map[string]struct{}),
	}
}

// SetAllowInsecureHTTP records the explicit, host-scoped acknowledgement
// required before authenticated Git may use plain HTTP.
func (m *Manager) SetAllowInsecureHTTP(platform, host string, allowed bool) {
	key := insecureHTTPPolicyKey(platform, host)
	m.transportPolicyMu.Lock()
	defer m.transportPolicyMu.Unlock()
	if allowed {
		m.allowInsecureHTTP[key] = struct{}{}
		return
	}
	delete(m.allowInsecureHTTP, key)
}

// AllowsInsecureHTTP reports whether plain HTTP was explicitly acknowledged
// for one provider identity. Loopback HTTP is handled separately.
func (m *Manager) AllowsInsecureHTTP(platform, host string) bool {
	key := insecureHTTPPolicyKey(platform, host)
	m.transportPolicyMu.RLock()
	defer m.transportPolicyMu.RUnlock()
	_, ok := m.allowInsecureHTTP[key]
	return ok
}

func insecureHTTPPolicyKey(platform, host string) string {
	return strings.ToLower(strings.TrimSpace(platform)) + "\x00" +
		strings.ToLower(strings.TrimSpace(host))
}

// ClonePath returns the filesystem path for a repo's bare clone.
// Path is partitioned by host: {baseDir}/{host}/{owner}/{name}.git
func (m *Manager) ClonePath(
	platform, host, owner, name string,
) (string, error) {
	return m.ClonePathInNamespace(
		cloneNamespaceForPlatform(platform), host, owner, name,
	)
}

func cloneNamespaceForPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" || platform == "github" {
		return ""
	}
	return platform
}

func cloneNamespaceForContext(ctx context.Context, platform string) string {
	namespace := cloneNamespaceForPlatform(platform)
	providerRepoID, _ := ctx.Value(repositoryIdentityContextKey{}).(string)
	providerRepoID = strings.TrimSpace(providerRepoID)
	if providerRepoID == "" {
		return namespace
	}
	digest := sha256.Sum256([]byte(providerRepoID))
	identityNamespace := fmt.Sprintf("repo-%x", digest[:16])
	if namespace == "" {
		return identityNamespace
	}
	return namespace + "-" + identityNamespace
}

// ClonePathForContext returns the clone path selected for ctx. Contexts
// carrying a provider repository identity use identity-partitioned storage.
func (m *Manager) ClonePathForContext(
	ctx context.Context, platform, host, owner, name string,
) (string, error) {
	return m.ClonePathInNamespace(
		cloneNamespaceForContext(ctx, platform), host, owner, name,
	)
}

func (m *Manager) clonePathForContext(
	ctx context.Context, platform, host, owner, name string,
) (string, error) {
	return m.ClonePathForContext(ctx, platform, host, owner, name)
}

// ClonePathInNamespace returns the filesystem path for a repo's bare clone
// inside an additional storage namespace. The namespace is only a local
// partition; host validation and git authentication still use host.
func (m *Manager) ClonePathInNamespace(
	namespace, host, owner, name string,
) (string, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace != "" {
		if err := validateCloneNamespace(namespace); err != nil {
			return "", err
		}
		return clonePath(filepath.Join(m.baseDir, namespace), host, owner, name)
	}
	return clonePath(m.baseDir, host, owner, name)
}

func clonePath(baseDir, host, owner, name string) (string, error) {
	if host == "" && owner == "" {
		// Preserve local fixture clones at {baseDir}/{name}.git while
		// still using kit's path validator for the repository name.
		if _, err := gitremote.ClonePath(baseDir, gitremote.Identity{
			Host:  "local",
			Owner: "fixture",
			Name:  name,
		}); err != nil {
			return "", err
		}
		return filepath.Join(baseDir, name+".git"), nil
	}
	return gitremote.ClonePath(baseDir, gitremote.Identity{
		Host:  host,
		Owner: owner,
		Name:  name,
	})
}

func validateCloneNamespace(namespace string) error {
	if namespace == "." || namespace == ".." {
		return fmt.Errorf("unsafe clone namespace %q", namespace)
	}
	for _, r := range namespace {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("unsafe clone namespace %q", namespace)
	}
	return nil
}

// EnsureClone creates or fetches a bare clone for the given repo.
// remoteURL is the HTTPS clone URL (e.g., https://github.com/owner/name.git).
// On first call, clones the repo. On subsequent calls, fetches updates.
//
// Concurrent callers for the same storage namespace and (host, owner, name)
// share a single underlying clone/fetch slot so PR detail syncs, the
// periodic syncer, and workspace setup do not stampede the same bare
// clone with duplicate git operations.
//
// The shared runner uses a context detached from any individual
// caller's cancellation, capped at ensureCloneTimeout, so one canceled
// waiter cannot abort the in-flight work but a stuck git subprocess
// cannot hold the slot forever either. Callers whose own context is
// already canceled on entry short-circuit without ever taking the
// slot.
func (m *Manager) EnsureClone(
	ctx context.Context, platform, host, owner, name, remoteURL string,
) error {
	return m.EnsureCloneInNamespace(
		ctx, cloneNamespaceForContext(ctx, platform),
		platform, host, owner, name, remoteURL,
	)
}

// EnsureCloneValidated creates or fetches a clone with route validation.
// When this caller starts the shared fetch, validate runs inside the slot
// after the fetch and before any waiter is released; a failure there removes
// the fetched clone, so data fetched across an A -> B -> A ownership change
// is never retained. Every caller additionally runs validate as a pure gate
// before joining and again before leaving the slot; a stale caller is
// rejected without touching the shared clone, and joined validation remains
// serialized against later clone work.
func (m *Manager) EnsureCloneValidated(
	ctx context.Context,
	platform, host, owner, name, remoteURL string,
	validate func(context.Context) error,
) error {
	return m.ensureCloneInNamespaceValidated(
		ctx, cloneNamespaceForContext(ctx, platform),
		platform, host, owner, name, remoteURL, validate,
	)
}

// EnsureCloneInNamespace creates or fetches a bare clone in a local storage
// namespace while still validating and authenticating against host.
func (m *Manager) EnsureCloneInNamespace(
	ctx context.Context, namespace, platform, host, owner, name, remoteURL string,
) error {
	return m.ensureCloneInNamespaceValidated(
		ctx, namespace, platform, host, owner, name, remoteURL, nil,
	)
}

func (m *Manager) ensureCloneInNamespaceValidated(
	ctx context.Context,
	namespace, platform, host, owner, name, remoteURL string,
	validate func(context.Context) error,
) error {
	namespace = strings.TrimSpace(namespace)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Validate per-caller inputs before entering the shared clone
	// slot. remoteURL is not part of the slot key (we dedup by
	// repo identity, not URL spelling), so without an up-front
	// check a follower with a malformed URL could inherit the
	// leader's success — or a valid caller could inherit the
	// leader's validation error.
	if err := validateRemoteURLIdentity(host, owner, name, remoteURL); err != nil {
		return err
	}
	if err := m.validateRemoteTransport(platform, host, remoteURL); err != nil {
		return err
	}
	clonePath, err := m.ClonePathInNamespace(namespace, host, owner, name)
	if err != nil {
		return err
	}
	key := ensureCloneKey(namespace, host, owner, name)
	if err := validateEnsureCloneCaller(ctx, validate); err != nil {
		return err
	}
	run := func() error {
		opCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), ensureCloneTimeout,
		)
		defer cancel()
		var previous existingCloneState
		var existed bool
		if validate != nil {
			var err error
			previous, existed, err = m.snapshotExistingClone(opCtx, clonePath)
			if err != nil {
				return err
			}
		}
		if err := m.ensureCloneNowInNamespace(
			opCtx, namespace, platform, host, owner, name, remoteURL,
		); err != nil {
			return err
		}
		if validate == nil {
			return nil
		}
		// The slot starter's validation spans the whole fetch window, so a
		// route that changed ownership during the fetch fails here. A new
		// clone is removed; an existing clone is restored so linked worktrees
		// keep their repository while no fetched replacement refs remain.
		if err := validate(opCtx); err != nil {
			cleanupCtx := context.WithoutCancel(opCtx)
			var cleanupErr error
			if existed {
				cleanupErr = m.restoreExistingClone(cleanupCtx, clonePath, previous)
			} else {
				cleanupErr = m.removeCloneAside(clonePath)
			}
			if cleanupErr != nil {
				return errors.Join(err, fmt.Errorf(
					"clean clone after failed validation: %w", cleanupErr,
				))
			}
			return &cloneValidationError{err: err}
		}
		return nil
	}
	started, flight, err := m.awaitEnsureCloneFlight(ctx, key, run, validate)
	if err != nil {
		var invalidated *cloneValidationError
		if started || !errors.As(err, &invalidated) {
			return err
		}
		// The starter's route lost ownership and its failed validation
		// removed the fetched clone, but this caller's route may still own
		// the path. Wait for every joined caller to finish validation, then
		// retry once with a fresh fetch this caller validates.
		if err := waitEnsureCloneFlightReleased(ctx, flight); err != nil {
			return err
		}
		if err := validateEnsureCloneCaller(ctx, validate); err != nil {
			return err
		}
		if _, _, err := m.awaitEnsureCloneFlight(ctx, key, run, validate); err != nil {
			return err
		}
	}
	return nil
}

type cloneRefState struct {
	objectName string
	symref     string
}

type existingCloneState struct {
	refs          map[string]cloneRefState
	fetchRefspecs []string
}

func (m *Manager) snapshotExistingClone(
	ctx context.Context, clonePath string,
) (existingCloneState, bool, error) {
	if _, err := os.Stat(filepath.Join(clonePath, "HEAD")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return existingCloneState{}, false, nil
		}
		return existingCloneState{}, false, fmt.Errorf("inspect existing clone: %w", err)
	}
	refs, err := m.snapshotCloneRefs(ctx, clonePath)
	if err != nil {
		return existingCloneState{}, false, err
	}
	refspecs, err := m.cloneFetchRefspecs(ctx, clonePath)
	if err != nil {
		return existingCloneState{}, false, err
	}
	return existingCloneState{refs: refs, fetchRefspecs: refspecs}, true, nil
}

func (m *Manager) snapshotCloneRefs(
	ctx context.Context, clonePath string,
) (map[string]cloneRefState, error) {
	out, err := m.git(
		ctx, clonePath, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)",
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot clone refs: %w", err)
	}
	refs := make(map[string]cloneRefState)
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("snapshot clone refs: malformed ref record %q", line)
		}
		refs[parts[0]] = cloneRefState{objectName: parts[1], symref: parts[2]}
	}
	return refs, nil
}

func (m *Manager) cloneFetchRefspecs(
	ctx context.Context, clonePath string,
) ([]string, error) {
	out, err := m.git(ctx, clonePath, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot clone fetch refspecs: %w", err)
	}
	value := strings.TrimSuffix(string(out), "\n")
	if value == "" {
		return nil, nil
	}
	return strings.Split(value, "\n"), nil
}

func (m *Manager) restoreExistingClone(
	ctx context.Context, clonePath string, before existingCloneState,
) error {
	currentRefs, err := m.snapshotCloneRefs(ctx, clonePath)
	if err != nil {
		return err
	}
	var restoreErr error
	var deleteRefs []string
	for ref := range currentRefs {
		if _, ok := before.refs[ref]; !ok {
			deleteRefs = append(deleteRefs, ref)
		}
	}
	slices.Sort(deleteRefs)
	slices.Reverse(deleteRefs)
	for _, ref := range deleteRefs {
		_, err := m.git(ctx, clonePath, "update-ref", "--no-deref", "-d", ref)
		restoreErr = errors.Join(restoreErr, err)
	}
	refs := make([]string, 0, len(before.refs))
	for ref := range before.refs {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		state := before.refs[ref]
		if current, ok := currentRefs[ref]; ok && current == state {
			continue
		}
		var err error
		if state.symref != "" {
			_, err = m.git(ctx, clonePath, "symbolic-ref", ref, state.symref)
		} else {
			_, err = m.git(
				ctx, clonePath, "update-ref", "--no-deref", ref, state.objectName,
			)
		}
		restoreErr = errors.Join(restoreErr, err)
	}
	currentRefspecs, err := m.cloneFetchRefspecs(ctx, clonePath)
	if err != nil {
		return errors.Join(restoreErr, err)
	}
	if slices.Equal(currentRefspecs, before.fetchRefspecs) {
		return restoreErr
	}
	if len(currentRefspecs) > 0 {
		_, err = m.git(ctx, clonePath, "config", "--unset-all", "remote.origin.fetch")
		restoreErr = errors.Join(restoreErr, err)
	}
	for _, refspec := range before.fetchRefspecs {
		_, err = m.git(
			ctx, clonePath, "config", "--add", "remote.origin.fetch", refspec,
		)
		restoreErr = errors.Join(restoreErr, err)
	}
	return restoreErr
}

func (m *Manager) validateRemoteTransport(platform, host, remoteURL string) error {
	u, err := url.Parse(strings.TrimSpace(remoteURL))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return nil
	}
	if m.AllowsInsecureHTTP(platform, host) {
		return nil
	}
	hostname := strings.Trim(strings.ToLower(u.Hostname()), "[]")
	if !strings.EqualFold(strings.TrimSpace(platform), "gitea") && hostname == "localhost" {
		return nil
	}
	if ip := net.ParseIP(hostname); !strings.EqualFold(strings.TrimSpace(platform), "gitea") &&
		ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"plain HTTP clone transport for %s host %q requires allow_insecure = true",
		strings.ToLower(strings.TrimSpace(platform)), host,
	)
}

func validateEnsureCloneCaller(
	ctx context.Context, validate func(context.Context) error,
) error {
	if validate == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	validationCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), ensureCloneTimeout,
	)
	defer cancel()
	return validate(validationCtx)
}

// removeCloneAside renames an invalidated clone out of its published path
// before deleting it. Readers outside the clone flight (diff, commit, and
// file reads resolve the path directly) either finish on their already-open
// handles or fail cleanly on a vanished path; they never observe a
// half-deleted tree mid-RemoveAll.
func removeCloneAside(clonePath string) error {
	aside := clonePath + ".removing"
	if err := os.RemoveAll(aside); err != nil {
		return err
	}
	if err := os.Rename(clonePath, aside); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.RemoveAll(aside)
}

func (m *Manager) removeCloneAside(clonePath string) error {
	if remove := m.removeCloneAsideForTest; remove != nil {
		return remove(clonePath)
	}
	return removeCloneAside(clonePath)
}

// awaitEnsureCloneFlight joins (or starts) the shared clone flight for key,
// waits for it, and reports whether this caller started it alongside the
// flight and its result. Per-caller validation runs before leaving the
// flight, keeping the slot serialized until every joined route check ends.
func (m *Manager) awaitEnsureCloneFlight(
	ctx context.Context, key string, run func() error,
	validate func(context.Context) error,
) (bool, *ensureCloneFlight, error) {
	flight, started := m.joinEnsureCloneFlight(key, run)
	defer m.leaveEnsureCloneFlight(key, flight)
	select {
	case <-flight.done:
	case <-ctx.Done():
		return started, flight, ctx.Err()
	}
	err := flight.err
	if err == nil {
		err = validateEnsureCloneCaller(ctx, validate)
	}
	return started, flight, err
}

func (m *Manager) joinEnsureCloneFlight(
	key string,
	run func() error,
) (*ensureCloneFlight, bool) {
	m.ensureMu.Lock()
	if m.ensureFlights == nil {
		m.ensureFlights = make(map[string]*ensureCloneFlight)
	}
	// Completed work remains joinable until every registered caller finishes
	// its own route validation. A second flight must not mutate or remove the
	// clone while callers from the first flight are still checking ownership.
	if flight := m.ensureFlights[key]; flight != nil {
		flight.waiters++
		m.ensureMu.Unlock()
		return flight, false
	}
	flight := &ensureCloneFlight{
		done: make(chan struct{}), released: make(chan struct{}), waiters: 1,
	}
	m.ensureFlights[key] = flight
	m.ensureMu.Unlock()

	go func() {
		err := run()
		m.ensureMu.Lock()
		flight.err = err
		flight.complete = true
		close(flight.done)
		if flight.waiters == 0 && m.ensureFlights[key] == flight {
			delete(m.ensureFlights, key)
			close(flight.released)
		}
		m.ensureMu.Unlock()
	}()
	return flight, true
}

func (m *Manager) leaveEnsureCloneFlight(key string, flight *ensureCloneFlight) {
	m.ensureMu.Lock()
	flight.waiters--
	if flight.complete && flight.waiters == 0 && m.ensureFlights[key] == flight {
		delete(m.ensureFlights, key)
		close(flight.released)
	}
	m.ensureMu.Unlock()
}

func waitEnsureCloneFlightReleased(
	ctx context.Context, flight *ensureCloneFlight,
) error {
	if flight == nil {
		return errors.New("wait for clone flight release: missing flight")
	}
	select {
	case <-flight.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ensureCloneKey(namespace, host, owner, name string) string {
	return namespace + "\x00" + host + "\x00" + owner + "\x00" + name
}

// ensureCloneNow is the unshared inner: it decides whether to create a
// fresh bare clone or refresh an existing one. Always called from
// inside the shared clone slot opened by EnsureClone, which has
// already validated the caller's remoteURL.
func (m *Manager) ensureCloneNowInNamespace(
	ctx context.Context, namespace, platform, host, owner, name, remoteURL string,
) error {
	clonePath, err := m.ClonePathInNamespace(namespace, host, owner, name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(clonePath, "HEAD")); os.IsNotExist(err) {
		return m.cloneBare(
			ctx, platform, host, owner, name, clonePath, remoteURL,
		)
	}
	// On an existing clone, also re-verify the stored origin URL
	// belongs to the expected host: catches a clone whose config
	// was rewritten after creation.
	if out, err := m.git(ctx, clonePath, "config", "--get", "remote.origin.url"); err == nil {
		if err := validateRemoteURLIdentity(host, owner, name, strings.TrimSpace(string(out))); err != nil {
			return err
		}
	}
	m.ensureRefspecs(ctx, clonePath)
	return m.fetch(ctx, platform, host, owner, name, clonePath)
}

// Fetch refspecs configured on every bare clone.
//
//   - remoteTrackingRefspec stores origin branches under
//     refs/remotes/origin/* so bare-clone fetches never try to update a local
//     branch that a workspace has checked out.
//   - pullRefspec makes refs/pull/<N>/head available, which is how we resolve
//     PR heads that live on forks.
//   - gitlabMergeRequestRefspec is intentionally not a default refspec. GitLab
//     MR heads are fetched one at a time by explicit workspace operations.
const (
	legacyBranchRefspec       = "+refs/heads/*:refs/heads/*"
	remoteTrackingRefspec     = "+refs/heads/*:refs/remotes/origin/*"
	pullRefspec               = "+refs/pull/*/head:refs/pull/*/head"
	gitlabMergeRequestRefspec = "+refs/merge-requests/*/head:refs/merge-requests/*/head"
)

// defaultRefspecs returns the full list of fetch refspecs every clone should
// have. Used by both cloneBare (fresh clones) and ensureRefspecs (migration).
func defaultRefspecs() []string {
	return []string{
		remoteTrackingRefspec,
		pullRefspec,
	}
}

// ensureRefspecs idempotently adds any missing fetch refspecs to an
// existing clone. This upgrades clones created before branch/pull ref
// support was in place, including vanilla `git clone --bare` output with
// no configured fetch refspec at all.
func (m *Manager) ensureRefspecs(
	ctx context.Context, clonePath string,
) {
	// `git config --get-all` exits 1 with no output when the key is unset.
	// Treat any read failure as "no existing refspecs" and fall through to
	// the add loop, which is idempotent on its own and will log its own
	// warnings if the add commands fail for a real reason.
	out, _ := m.git(ctx, clonePath,
		"config", "--get-all", "remote.origin.fetch")
	existing := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			existing[line] = true
		}
	}
	if existing[legacyBranchRefspec] {
		if _, err := m.git(
			ctx, clonePath,
			"config", "--fixed-value", "--unset-all",
			"remote.origin.fetch", legacyBranchRefspec,
		); err != nil {
			slog.Warn("failed to remove legacy refspec from existing clone",
				"path", clonePath, "refspec", legacyBranchRefspec, "err", err)
		} else {
			delete(existing, legacyBranchRefspec)
		}
	}
	if existing[gitlabMergeRequestRefspec] {
		if _, err := m.git(
			ctx, clonePath,
			"config", "--fixed-value", "--unset-all",
			"remote.origin.fetch", gitlabMergeRequestRefspec,
		); err != nil {
			slog.Warn("failed to remove unbounded GitLab MR refspec from existing clone",
				"path", clonePath, "refspec", gitlabMergeRequestRefspec, "err", err)
		} else {
			delete(existing, gitlabMergeRequestRefspec)
		}
	}
	for _, refspec := range defaultRefspecs() {
		if existing[refspec] {
			continue
		}
		if _, err := m.git(ctx, clonePath,
			"config", "--add", "remote.origin.fetch", refspec); err != nil {
			slog.Warn("failed to add refspec to existing clone",
				"path", clonePath, "refspec", refspec, "err", err)
		}
	}
}

func (m *Manager) cloneBare(
	ctx context.Context, platform, host, owner, name, clonePath, remoteURL string,
) error {
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return fmt.Errorf("mkdir for clone: %w", err)
	}
	slog.Info("cloning bare repo", "path", clonePath)
	// Initial clones hit the same flaky smart-HTTP /info/refs that
	// fetches do, so wrap the clone command in the same retry helper.
	// git clone refuses to write into a non-empty destination, so a
	// partial directory from a previous failed attempt would poison
	// every retry — sweep it out before re-running.
	_, err := retryTransient(ctx, "git clone --bare", func() ([]byte, error) {
		if err := os.RemoveAll(clonePath); err != nil {
			return nil, fmt.Errorf("cleanup partial clone: %w", err)
		}
		return m.gitCloneBare(
			ctx, platform, host, owner, name, clonePath, remoteURL,
		)
	})
	if err != nil {
		return fmt.Errorf("git clone --bare: %w", err)
	}

	// Install fetch refspecs so future fetches pull both branch heads and
	// pull refs. git clone --bare does not install a default refspec.
	// On failure, remove the partial clone so the next call retries.
	for _, refspec := range defaultRefspecs() {
		if _, err := m.git(ctx, clonePath,
			"config", "--add", "remote.origin.fetch", refspec); err != nil {
			os.RemoveAll(clonePath)
			return fmt.Errorf("add fetch refspec %q: %w", refspec, err)
		}
	}

	// Fetch immediately after clone so pull refs are available before
	// merge-base computation runs in the same sync cycle.
	return m.fetch(ctx, platform, host, owner, name, clonePath)
}

func (m *Manager) fetch(
	ctx context.Context, platform, host, owner, name, clonePath string,
) error {
	// GitHub's smart-HTTP endpoint sporadically returns 5xx on /info/refs.
	// Retry inline so a transient blip does not drop the entire sync cycle.
	_, err := retryTransient(ctx, "git fetch", func() ([]byte, error) {
		return m.gitNetworked(
			ctx, m.sourceForRepo(platform, host, owner, name), host, clonePath, nil,
			"fetch", "--prune", "--no-tags", "origin",
		)
	})
	if err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	// set-head -a is networked (it consults the remote's HEAD via
	// /info/refs) and so subject to the same transient 5xx as fetch.
	// Failure is non-fatal — bare clone still works — but retrying
	// reduces stale-HEAD noise across sync cycles.
	_, setHeadErr := retryTransient(ctx, "git remote set-head", func() ([]byte, error) {
		return m.gitNetworked(
			ctx, m.sourceForRepo(platform, host, owner, name), host, clonePath, nil,
			"remote", "set-head", "origin", "-a",
		)
	})
	if setHeadErr != nil {
		slog.Warn("failed to repair origin HEAD",
			"path", clonePath, "err", setHeadErr)
	}
	return nil
}

// RevParse resolves a git ref to its SHA. Returns an empty string if the ref
// does not exist.
func (m *Manager) RevParse(
	ctx context.Context, platform, host, owner, name, ref string,
) (string, error) {
	clonePath, err := m.clonePathForContext(ctx, platform, host, owner, name)
	if err != nil {
		return "", err
	}
	out, err := m.git(ctx, clonePath, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchMergeRequestHead fetches only the provider-owned head ref for one
// merge request. GitLab merge-request refs are intentionally excluded from
// the clone's default refspec to keep ordinary refreshes bounded.
func (m *Manager) FetchMergeRequestHead(
	ctx context.Context,
	platform, host, owner, name string,
	number int,
) error {
	if number <= 0 {
		return errors.New("merge request number must be positive")
	}
	clonePath, err := m.clonePathForContext(ctx, platform, host, owner, name)
	if err != nil {
		return err
	}
	ref := providerplatform.MergeRequestHeadRef(providerplatform.Kind(platform), number)
	_, err = retryTransient(ctx, "git fetch merge request head", func() ([]byte, error) {
		return m.RunGitForRepo(
			ctx, platform, host, owner, name, clonePath,
			"fetch", "--no-tags", "--recurse-submodules=no",
			"origin", "+"+ref+":"+ref,
		)
	})
	if err != nil {
		return fmt.Errorf("fetch merge request head %d: %w", number, err)
	}
	return nil
}

// MergeBase computes the merge base between two commits.
func (m *Manager) MergeBase(
	ctx context.Context, platform, host, owner, name, sha1, sha2 string,
) (string, error) {
	clonePath, err := m.clonePathForContext(ctx, platform, host, owner, name)
	if err != nil {
		return "", err
	}
	out, err := m.git(ctx, clonePath, "merge-base", sha1, sha2)
	if err != nil {
		return "", fmt.Errorf("git merge-base %s %s: %w", sha1, sha2, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func validateRemoteURLHost(expectedHost, remoteURL string) error {
	return gitremote.ValidateRemoteHost(expectedHost, remoteURL)
}

func validateRemoteURLIdentity(expectedHost, owner, name, remoteURL string) error {
	return gitremote.ValidateRemoteIdentity(gitremote.Identity{
		Host:  expectedHost,
		Owner: owner,
		Name:  name,
	}, remoteURL)
}

// git runs a local git command against an already-cloned bare repo and
// returns its stdout. Local reads (diff, log, rev-parse, merge-base,
// cat-file, config) never contact the remote, so they run without resolving
// or attaching a credential. Decoupling them from the token source keeps
// commit and diff views working during token rotation, when a token file can
// be briefly missing and resolving it would otherwise error. Networked
// operations go through gitNetworked instead.
func (m *Manager) git(
	ctx context.Context, dir string, args ...string,
) ([]byte, error) {
	return m.gitWithInput(ctx, dir, nil, args...)
}

// RunGitForRepo runs a networked Git command with the repository route's
// mutation-capable credential.
func (m *Manager) RunGitForRepo(
	ctx context.Context, platform, host, owner, name, dir string, args ...string,
) ([]byte, error) {
	return m.RunGitForRepoRemote(
		ctx, platform, host, owner, name, "origin", dir, args...,
	)
}

// RunGitForRepoRemote runs a networked Git command against a named remote
// that must match the supplied repository route.
func (m *Manager) RunGitForRepoRemote(
	ctx context.Context,
	platform, host, owner, name, remote, dir string,
	args ...string,
) ([]byte, error) {
	source := m.sourceForRepo(platform, host, owner, name)
	if source != nil {
		if err := m.validateRemoteIdentity(ctx, dir, remote, host, owner, name); err != nil {
			return nil, err
		}
	}
	return m.gitNetworked(ctx, source, host, dir, nil, args...)
}

// RunGitForNamedRemote runs a networked Git command with credentials selected
// from the named remote's validated repository identity.
func (m *Manager) RunGitForNamedRemote(
	ctx context.Context,
	platform, host, routeOwner, routeName, remote, dir string,
	args ...string,
) ([]byte, error) {
	owner, name, err := m.namedRemoteRepository(
		ctx, platform, host, routeOwner, routeName, remote, dir,
	)
	if err != nil {
		return nil, err
	}
	return m.RunGitForRepoRemote(
		ctx, platform, host, owner, name, remote, dir, args...,
	)
}

// RunGitForRemote runs a networked Git command against an explicit hosted
// repository URL using credentials resolved for that URL's repository owner,
// rather than credentials for the checkout's origin.
func (m *Manager) RunGitForRemote(
	ctx context.Context, platform, host, remoteURL, dir string, args ...string,
) ([]byte, error) {
	if err := validateRemoteURLHost(host, remoteURL); err != nil {
		return nil, err
	}
	if err := m.validateRemoteTransport(platform, host, remoteURL); err != nil {
		return nil, err
	}
	repoPath := gitremote.RemoteRepoPath(remoteURL)
	owner, name, ok := strings.Cut(repoPath, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return nil, errors.New("remote repository owner and name are required")
	}
	if index := strings.LastIndex(repoPath, "/"); index >= 0 {
		owner, name = repoPath[:index], repoPath[index+1:]
	}
	return m.gitNetworked(
		ctx, m.sourceForRepo(platform, host, owner, name), host, dir, nil, args...,
	)
}

// RunGitForHost runs a genuinely ownerless networked Git command with the
// host fallback credential.
func (m *Manager) RunGitForHost(
	ctx context.Context, host, dir string, args ...string,
) ([]byte, error) {
	return m.gitNetworked(ctx, m.fallbackSource(host), host, dir, nil, args...)
}

func (m *Manager) validateRemoteIdentity(
	ctx context.Context, dir, remote, host, owner, name string,
) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if remote == "" || remote[0] == '-' || strings.ContainsAny(remote, " \t\r\n") {
		return fmt.Errorf("authenticated git rejects unsafe remote name %q", remote)
	}
	rewrites, err := m.git(ctx, dir, "config", "--local", "--get-regexp", `^url\..*\.(insteadOf|pushInsteadOf)$`)
	if err == nil && strings.TrimSpace(string(rewrites)) != "" {
		return fmt.Errorf("authenticated git rejects repository-local URL rewrites")
	}
	for _, key := range []string{"remote." + remote + ".url", "remote." + remote + ".pushurl"} {
		out, err := m.git(ctx, dir, "config", "--get-all", key)
		if err != nil {
			if strings.HasSuffix(key, ".pushurl") {
				continue
			}
			return fmt.Errorf("read %s before authenticated git: %w", key, err)
		}
		urls := strings.Fields(string(out))
		if len(urls) == 0 && strings.HasSuffix(key, ".pushurl") {
			continue
		}
		for _, url := range urls {
			if err := validateRemoteURLIdentity(host, owner, name, url); err != nil {
				return fmt.Errorf("validate %s before authenticated git: %w", key, err)
			}
		}
	}
	return nil
}

func (m *Manager) namedRemoteRepository(
	ctx context.Context,
	platform, host, routeOwner, routeName, remote, dir string,
) (string, string, error) {
	if remote == "" || remote[0] == '-' || strings.ContainsAny(remote, " \t\r\n") {
		return "", "", fmt.Errorf("authenticated git rejects unsafe remote name %q", remote)
	}
	var remoteURLs []string
	for _, key := range []string{"remote." + remote + ".url", "remote." + remote + ".pushurl"} {
		out, err := m.git(ctx, dir, "config", "--get-all", key)
		if err != nil {
			if strings.HasSuffix(key, ".pushurl") {
				continue
			}
			return "", "", fmt.Errorf("read %s before authenticated git: %w", key, err)
		}
		remoteURLs = append(remoteURLs, strings.Fields(string(out))...)
	}
	if len(remoteURLs) == 0 {
		return "", "", fmt.Errorf("read remote.%s.url before authenticated git: no URL configured", remote)
	}
	repoPath := gitremote.RemoteRepoPath(remoteURLs[0])
	index := strings.LastIndex(repoPath, "/")
	if index <= 0 || index == len(repoPath)-1 {
		owner, name := strings.TrimSpace(routeOwner), strings.TrimSpace(routeName)
		if owner == "" || name == "" {
			return "", "", errors.New("remote repository owner and name are required")
		}
		for _, remoteURL := range remoteURLs {
			if err := m.validateRemoteTransport(platform, host, remoteURL); err != nil {
				return "", "", err
			}
			if err := validateRemoteURLIdentity(host, owner, name, remoteURL); err != nil {
				return "", "", fmt.Errorf(
					"validate %q remote before authenticated git: %w", remote, err,
				)
			}
		}
		return owner, name, nil
	}
	owner, name := repoPath[:index], repoPath[index+1:]
	for _, remoteURL := range remoteURLs {
		if err := m.validateRemoteTransport(platform, host, remoteURL); err != nil {
			return "", "", err
		}
		if err := validateRemoteURLIdentity(host, owner, name, remoteURL); err != nil {
			return "", "", fmt.Errorf(
				"validate %q remote before authenticated git: %w", remote, err,
			)
		}
	}
	return owner, name, nil
}

func (m *Manager) gitWithInput(
	ctx context.Context, dir string, input []byte, args ...string,
) ([]byte, error) {
	out, stderr, err := runGitCommand(ctx, newGitRunner(), dir, input, args...)
	if err != nil {
		return nil, wrapGitError(err, stderr)
	}
	return out, nil
}

func (m *Manager) gitCloneBare(
	ctx context.Context, platform, host, owner, name, clonePath, remoteURL string,
) ([]byte, error) {
	// Local-path clones copy the source object directory and can race source
	// maintenance. Use transport semantics consistently for every remote.
	return m.gitNetworked(
		ctx, m.sourceForRepo(platform, host, owner, name), host, "",
		func() error {
			if err := os.RemoveAll(clonePath); err != nil {
				return fmt.Errorf("cleanup partial clone before auth retry: %w", err)
			}
			return nil
		},
		"clone", "--bare", "--no-local", remoteURL, clonePath,
	)
}

// gitNetworked runs a git command that contacts the remote (clone, fetch,
// remote set-head). It resolves the host credential and attaches it, then on
// an authentication failure invalidates the source and retries once — the
// recovery path when a token rotates or expires mid-operation.
// cleanupBeforeAuthRetry, when set, runs between attempts; clone uses it to
// sweep the partial destination git refuses to overwrite.
func (m *Manager) gitNetworked(
	ctx context.Context,
	source tokenauth.Source,
	host, dir string,
	cleanupBeforeAuthRetry func() error,
	args ...string,
) ([]byte, error) {
	required, _ := ctx.Value(requiredCredentialContextKey{}).(bool)
	if required && source == nil {
		return nil, ErrCredentialUnavailable
	}
	out, stderr, rejectedToken, err := m.runGitAuthed(
		ctx, source, host, dir, required, args...,
	)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, ErrCredentialUnavailable) {
		return nil, err
	}
	wrapped := wrapGitError(err, stderr)
	if isAuthGitError(wrapped) && invalidateTokenSource(source, rejectedToken) {
		if cleanupBeforeAuthRetry != nil {
			if err := cleanupBeforeAuthRetry(); err != nil {
				return nil, err
			}
		}
		out, stderr, _, err = m.runGitAuthed(
			ctx, source, host, dir, required, args...,
		)
		if err == nil {
			return out, nil
		}
		wrapped = wrapGitError(err, stderr)
	}
	if required && errors.Is(wrapped, tokenauth.ErrMissingToken) {
		return nil, fmt.Errorf("%w: %v", ErrCredentialUnavailable, wrapped)
	}
	return nil, wrapped
}

// runGitAuthed builds a runner with the host credential attached and runs the
// command. Networked git has no stdin, so it takes no input.
func (m *Manager) runGitAuthed(
	ctx context.Context,
	source tokenauth.Source,
	host, dir string,
	required bool,
	args ...string,
) ([]byte, []byte, string, error) {
	runner, token, err := m.gitRunnerAuthed(ctx, source, host, required)
	if err != nil {
		return nil, nil, "", err
	}
	out, stderr, err := runGitCommand(ctx, runner, dir, nil, args...)
	return out, stderr, token, err
}

// runGitCommand runs git in dir with the given runner, bounded by the shared
// subprocess limiter. The limiter covers every git invocation — local reads
// and networked clone/fetch alike — because they all draw on the same process
// capacity as the rest of the app.
func runGitCommand(
	ctx context.Context, runner gitcmd.Runner, dir string, input []byte, args ...string,
) ([]byte, []byte, error) {
	var stdin io.Reader
	if input != nil {
		stdin = bytes.NewReader(input)
	}
	release, err := procutil.TryAcquire(ctx, "git subprocess capacity")
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return runner.Run(ctx, dir, stdin, args...)
}

func wrapGitError(err error, stderr []byte) error {
	msg := tokenauth.RedactKnownSecrets(string(stderr))
	if isNotFoundError(msg) {
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	errMsg := tokenauth.RedactKnownSecrets(err.Error())
	wrapped := gitCommandError{
		message: errMsg + ": " + msg,
		cause:   safeGitErrorCause(err),
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		wrapped.exitCode = exitErr.ExitCode()
		wrapped.hasExitCode = true
	}
	return wrapped
}

type gitCommandError struct {
	message     string
	cause       error
	exitCode    int
	hasExitCode bool
}

func (e gitCommandError) Error() string {
	return e.message
}

func (e gitCommandError) Unwrap() error {
	return e.cause
}

func (e gitCommandError) ExitCode() (int, bool) {
	return e.exitCode, e.hasExitCode
}

func safeGitErrorCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, tokenauth.ErrMissingToken):
		return tokenauth.ErrMissingToken
	default:
		return nil
	}
}

func gitExitCode(err error) (int, bool) {
	var exitErr interface {
		ExitCode() (int, bool)
	}
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 0, false
}

func invalidateTokenSource(source tokenauth.Source, rejectedToken string) bool {
	if source == nil {
		return false
	}
	source.Invalidate(rejectedToken)
	return true
}

// newGitRunner returns a runner with kit's automation defaults: inherited
// GIT_* variables are stripped, global/system config is ignored, and terminal
// prompts are disabled.
func newGitRunner() gitcmd.Runner {
	return gitcmd.New()
}

func (m *Manager) sourceForRepo(
	platform, host, owner, name string,
) tokenauth.Source {
	if m.routes == nil {
		return nil
	}
	return m.routes.SourceForRepo(platform, host, owner, name)
}

func (m *Manager) fallbackSource(host string) tokenauth.Source {
	if m.routes == nil {
		return nil
	}
	return m.routes.FallbackSource(host)
}

// gitRunnerAuthed returns a runner with the selected token attached for
// networked operations. With no source configured it returns the plain runner.
func (m *Manager) gitRunnerAuthed(
	ctx context.Context, source tokenauth.Source, host string, required bool,
) (gitcmd.Runner, string, error) {
	runner := newGitRunner()
	if source == nil {
		if required {
			return runner, "", ErrCredentialUnavailable
		}
		return runner, "", nil
	}
	token, err := source.Token(ctx)
	if err != nil {
		if required && !errors.Is(err, context.Canceled) &&
			!errors.Is(err, context.DeadlineExceeded) {
			return runner, "", fmt.Errorf("%w: %v", ErrCredentialUnavailable, err)
		}
		return runner, "", fmt.Errorf("resolve git token for host %s: %w", host, err)
	}
	if required && strings.TrimSpace(token) == "" {
		return runner, "", ErrCredentialUnavailable
	}
	if token != "" {
		// GitHub's smart HTTP endpoint expects Basic auth credentials.
		runner = runner.WithBasicAuth("x-access-token", token)
	}
	return runner, token, nil
}

// isNotFoundError checks if git stderr indicates a missing object or ref.
func isNotFoundError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "unknown revision") ||
		strings.Contains(s, "bad object") ||
		strings.Contains(s, "not a valid object name") ||
		strings.Contains(s, "not a valid commit name") ||
		strings.Contains(s, "does not exist")
}

func isAuthGitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "could not read username") ||
		strings.Contains(msg, "terminal prompts disabled")
}
