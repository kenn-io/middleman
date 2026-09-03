package gitclone

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"go.kenn.io/forge/internal/procutil"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

const (
	RepoBrowserRefLimit            = 1000
	RepoBrowserTreeEntryLimit      = 20000
	RepoBrowserBlobSizeLimit       = 1 << 20
	RepoBrowserLastChangedBatchMax = 250
	RepoBrowserLastChangedLogLimit = 500
	RepoBrowserHistoryLimit        = 50
)

var (
	ErrUnsafePath                   = errors.New("unsafe repo browser path")
	ErrTooManyPaths                 = errors.New("too many repo browser paths")
	ErrTooLargeAsset                = errors.New("repo browser asset too large")
	ErrUnsupportedAsset             = errors.New("unsupported repo browser asset type")
	ErrCommitOutOfScope             = errors.New("repo browser commit outside selected file history")
	ErrRepoBrowserRouteFenceChanged = errors.New("repo browser repository route changed")
)

// RepoBrowserRouteFence is an opaque, comparable route-ownership token.
// Server callers create one from their catalog generation without exposing
// the database representation to gitclone.
type RepoBrowserRouteFence struct {
	parts [3]int64
}

func NewRepoBrowserRouteFence(part1, part2, part3 int64) RepoBrowserRouteFence {
	return RepoBrowserRouteFence{parts: [3]int64{part1, part2, part3}}
}

type RepoBrowserRouteFenceValidator func(
	context.Context, RepoBrowserRouteFence,
) (bool, error)

type RepoBrowserRouteFencePublishGuard func(
	context.Context,
	RepoBrowserRouteFence,
	func() error,
) (bool, error)

type repoBrowserCloneValidationError struct{ err error }

func (e *repoBrowserCloneValidationError) Error() string { return e.err.Error() }
func (e *repoBrowserCloneValidationError) Unwrap() error { return e.err }

const repoBrowserBarrierCapacity int64 = 1 << 30

// repoBrowserBarrier gives publications preference once queued so a steady
// stream of browser reads cannot starve a validated clone swap. The weighted
// semaphore also lets callers abandon admission when their context is canceled.
type repoBrowserBarrier struct {
	semaphore      *semaphore.Weighted
	mu             sync.Mutex
	waitingWriters int
	writer         bool
}

func newRepoBrowserBarrier() *repoBrowserBarrier {
	return &repoBrowserBarrier{semaphore: semaphore.NewWeighted(repoBrowserBarrierCapacity)}
}

func (b *repoBrowserBarrier) lockRead(ctx context.Context, waiting func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	queuedBehindWriter := b.writer || b.waitingWriters > 0
	b.mu.Unlock()
	if queuedBehindWriter && waiting != nil {
		waiting()
	}
	return b.semaphore.Acquire(ctx, 1)
}

func (b *repoBrowserBarrier) unlockRead() {
	b.semaphore.Release(1)
}

func (b *repoBrowserBarrier) lockWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.waitingWriters++
	b.mu.Unlock()
	err := b.semaphore.Acquire(ctx, repoBrowserBarrierCapacity)
	b.mu.Lock()
	b.waitingWriters--
	if err == nil {
		b.writer = true
	}
	b.mu.Unlock()
	return err
}

func (b *repoBrowserBarrier) unlockWrite() {
	b.mu.Lock()
	b.writer = false
	b.mu.Unlock()
	b.semaphore.Release(repoBrowserBarrierCapacity)
}

type RepoBrowserRefType string

const (
	RepoBrowserRefBranch RepoBrowserRefType = "branch"
	RepoBrowserRefTag    RepoBrowserRefType = "tag"
	RepoBrowserRefCommit RepoBrowserRefType = "commit"
)

type RepoBrowserRepoRef struct {
	Provider string
	Host     string
	Owner    string
	Name     string
	RepoPath string
	// ProviderRepoID is the provider's stable repository identity. Browser
	// clone storage partitions on it so a reused owner/name path never
	// serves the displaced repository's cached refs or objects. Empty for
	// repositories without a verified identity, which keep path-scoped
	// storage.
	ProviderRepoID string
	RemoteURL      string
	// RequireCredential marks federation-node clones whose network work must
	// never fall back to anonymous access if the exact route disappears.
	RequireCredential bool

	// The route-fence callbacks bind server-created browser work to the catalog
	// generation that supplied RemoteURL. Standalone callers may leave all
	// three unset for legacy unfenced behavior.
	RouteFence         RepoBrowserRouteFence
	ValidateRouteFence RepoBrowserRouteFenceValidator
	// PublishIfRouteFenceMatches must hold route ownership stable while it
	// invokes the supplied filesystem publication callback.
	PublishIfRouteFenceMatches RepoBrowserRouteFencePublishGuard
}

type RepoBrowserRef struct {
	Type         RepoBrowserRefType `json:"type" doc:"Selected ref type: branch, tag, or commit."`
	Name         string             `json:"name" doc:"Selected branch or tag name. Commit refs leave this empty."`
	SHA          string             `json:"sha" doc:"Resolved commit SHA used for the read."`
	RequestedSHA string             `json:"requested_sha,omitempty" doc:"Caller-supplied branch or tag SHA when it differs from the resolved SHA."`
	Stale        bool               `json:"stale" doc:"True when a caller-supplied branch or tag SHA no longer matches the current ref target."`
}

type RepoBrowserTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type RepoBrowserBlob struct {
	Path      string `json:"path"`
	SHA       string `json:"sha"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
	Binary    bool   `json:"binary"`
	TooLarge  bool   `json:"too_large"`
}

type RepoBrowserCommit struct {
	SHA         string    `json:"sha"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	AuthorName  string    `json:"author_name"`
	AuthorEmail string    `json:"author_email"`
	AuthoredAt  time.Time `json:"authored_at"`
}

func (m *Manager) ListRepoBrowserRefs(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	defaultBranch string,
) ([]RepoBrowserRef, RepoBrowserRef, bool, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return nil, RepoBrowserRef{}, false, err
	}
	defer unlock()
	dir, err := m.repoBrowserClonePath(repo)
	if err != nil {
		return nil, RepoBrowserRef{}, false, err
	}
	out, err := m.git(ctx, dir,
		"for-each-ref",
		"--count="+strconv.Itoa(RepoBrowserRefLimit+1),
		"--exclude=refs/remotes/origin/HEAD",
		"--sort=refname",
		"--format=%(refname)%00%(objectname)%00%(*objectname)",
		"refs/remotes/origin",
		"refs/tags",
	)
	if err != nil {
		return nil, RepoBrowserRef{}, false, fmt.Errorf("list repo browser refs: %w", err)
	}
	refs := parseRepoBrowserRefs(out)
	refs, truncated := capRepoBrowserRefs(refs)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Type != refs[j].Type {
			return refs[i].Type < refs[j].Type
		}
		return refs[i].Name < refs[j].Name
	})
	branch, sha, err := m.resolveRepoBrowserDefaultBranch(ctx, repo, defaultBranch)
	if err != nil {
		return refs, RepoBrowserRef{}, truncated, err
	}
	return refs, RepoBrowserRef{Type: RepoBrowserRefBranch, Name: branch, SHA: sha}, truncated, nil
}

func parseRepoBrowserRefs(out []byte) []RepoBrowserRef {
	var refs []RepoBrowserRef
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), "\x00")
		if len(parts) < 2 {
			continue
		}
		refName := parts[0]
		sha := parts[1]
		if len(parts) > 2 && parts[2] != "" {
			sha = parts[2]
		}
		switch {
		case refName == "refs/remotes/origin/HEAD":
			continue
		case strings.HasPrefix(refName, "refs/remotes/origin/"):
			name := strings.TrimPrefix(refName, "refs/remotes/origin/")
			if name != "" {
				refs = append(refs, RepoBrowserRef{Type: RepoBrowserRefBranch, Name: name, SHA: sha})
			}
		case strings.HasPrefix(refName, "refs/tags/"):
			name := strings.TrimPrefix(refName, "refs/tags/")
			if name != "" {
				refs = append(refs, RepoBrowserRef{Type: RepoBrowserRefTag, Name: name, SHA: sha})
			}
		}
	}
	return refs
}

func capRepoBrowserRefs(refs []RepoBrowserRef) ([]RepoBrowserRef, bool) {
	if len(refs) <= RepoBrowserRefLimit {
		return refs, false
	}
	return refs[:RepoBrowserRefLimit], true
}

func (m *Manager) ListRepoBrowserTree(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
) ([]RepoBrowserTreeEntry, bool, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	dir, sha, _, err := m.resolveRepoBrowserRef(ctx, repo, ref)
	if err != nil {
		return nil, false, err
	}
	return m.listRepoBrowserTreeEntries(ctx, dir, sha)
}

func (m *Manager) listRepoBrowserTreeEntries(
	ctx context.Context,
	dir string,
	sha string,
) ([]RepoBrowserTreeEntry, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := newGitRunner().Command(ctx, dir,
		"ls-tree", "-r", "-z", "-l", "--full-tree", "--end-of-options", sha,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("list repo browser tree pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	release, err := procutil.TryAcquire(ctx, "git subprocess capacity")
	if err != nil {
		return nil, false, err
	}
	defer release()

	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("list repo browser tree start: %w", err)
	}

	entries, truncated, readErr := readRepoBrowserTreeEntries(stdout, cancel)
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, false, fmt.Errorf("list repo browser tree read: %w", readErr)
	}
	if waitErr != nil && !truncated {
		return nil, false, fmt.Errorf("list repo browser tree: %w", wrapGitError(waitErr, stderr.Bytes()))
	}
	SortRepoBrowserTree(entries)
	return entries, truncated, nil
}

func readRepoBrowserTreeEntries(
	r io.Reader,
	cancel context.CancelFunc,
) ([]RepoBrowserTreeEntry, bool, error) {
	reader := bufio.NewReader(r)
	entries := make([]RepoBrowserTreeEntry, 0, RepoBrowserTreeEntryLimit)
	for {
		record, err := reader.ReadBytes(0)
		if len(record) > 0 {
			record = bytes.TrimSuffix(record, []byte{0})
			if entry, ok := parseRepoBrowserTreeRecord(record); ok {
				if len(entries) == RepoBrowserTreeEntryLimit {
					cancel()
					return entries, true, nil
				}
				entries = append(entries, entry)
			}
		}
		if errors.Is(err, io.EOF) {
			return entries, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

func parseRepoBrowserTreeRecord(record []byte) (RepoBrowserTreeEntry, bool) {
	if len(record) == 0 {
		return RepoBrowserTreeEntry{}, false
	}
	header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
	if !ok {
		return RepoBrowserTreeEntry{}, false
	}
	fields := strings.Fields(string(header))
	if len(fields) < 4 {
		return RepoBrowserTreeEntry{}, false
	}
	size := int64(0)
	if fields[3] != "-" {
		size, _ = strconv.ParseInt(fields[3], 10, 64)
	}
	return RepoBrowserTreeEntry{
		Path: string(pathBytes),
		Type: fields[1],
		Size: size,
	}, true
}

func SortRepoBrowserTree(entries []RepoBrowserTreeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return compareDiffFilePaths(entries[i].Path, entries[j].Path) < 0
	})
}

func (m *Manager) ReadRepoBrowserBlob(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
	pathName string,
) (RepoBrowserBlob, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	defer unlock()
	return m.readRepoBrowserBlob(ctx, repo, ref, pathName, false)
}

func (m *Manager) ReadRepoBrowserAsset(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
	pathName string,
) (RepoBrowserBlob, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	defer unlock()
	blob, err := m.readRepoBrowserBlob(ctx, repo, ref, pathName, true)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	if blob.TooLarge {
		return RepoBrowserBlob{}, fmt.Errorf("%w: %w", ErrTooLargeAsset, ErrTooLarge)
	}
	if !repoBrowserAssetMediaTypeAllowed(blob.MediaType) {
		return RepoBrowserBlob{}, fmt.Errorf("%w: %s", ErrUnsupportedAsset, blob.MediaType)
	}
	return blob, nil
}

func (m *Manager) readRepoBrowserBlob(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
	pathName string,
	asset bool,
) (RepoBrowserBlob, error) {
	cleanPath, err := cleanRepoBrowserPath(pathName)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	dir, sha, _, err := m.resolveRepoBrowserRef(ctx, repo, ref)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	entry, err := m.lookupRepoBrowserTreeEntry(ctx, dir, sha, cleanPath)
	if err != nil {
		return RepoBrowserBlob{}, err
	}
	if entry.Type != "blob" {
		return RepoBrowserBlob{}, fmt.Errorf("%w: %s", ErrNotFound, cleanPath)
	}
	blob := RepoBrowserBlob{
		Path:      cleanPath,
		SHA:       entry.SHA,
		Size:      entry.Size,
		MediaType: mediaTypeForRepoBrowserPath(cleanPath),
	}
	if blob.Size > RepoBrowserBlobSizeLimit {
		blob.TooLarge = true
		return blob, nil
	}
	data, err := m.git(ctx, dir, "cat-file", "blob", entry.SHA)
	if err != nil {
		return RepoBrowserBlob{}, fmt.Errorf("read repo browser blob %s: %w", cleanPath, err)
	}
	if blob.MediaType == "" {
		blob.MediaType = http.DetectContentType(data)
	}
	blob.Binary = bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
	if blob.Binary && !asset {
		return blob, nil
	}
	blob.Encoding = "utf-8"
	blob.Content = string(data)
	return blob, nil
}

type repoBrowserTreeEntryLookup struct {
	Path string
	Type string
	SHA  string
	Size int64
}

func (m *Manager) lookupRepoBrowserTreeEntry(
	ctx context.Context,
	dir, sha, pathName string,
) (repoBrowserTreeEntryLookup, error) {
	out, err := m.git(ctx, dir,
		"ls-tree", "-z", "-l", "--full-tree", "--end-of-options", sha, "--", literalRepoBrowserPathspec(pathName),
	)
	if err != nil {
		return repoBrowserTreeEntryLookup{}, fmt.Errorf("lookup repo browser path %s: %w", pathName, err)
	}
	for record := range bytes.SplitSeq(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		if !ok || string(pathBytes) != pathName {
			continue
		}
		fields := strings.Fields(string(header))
		if len(fields) < 4 {
			continue
		}
		size := int64(0)
		if fields[3] != "-" {
			size, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		return repoBrowserTreeEntryLookup{
			Path: pathName,
			Type: fields[1],
			SHA:  fields[2],
			Size: size,
		}, nil
	}
	return repoBrowserTreeEntryLookup{}, fmt.Errorf("%w: %s", ErrNotFound, pathName)
}

func (m *Manager) RepoBrowserLastChanged(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
	paths []string,
) (map[string]RepoBrowserCommit, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(paths) > RepoBrowserLastChangedBatchMax {
		return nil, ErrTooManyPaths
	}
	cleanPaths := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, pathName := range paths {
		cleanPath, err := cleanRepoBrowserPath(pathName)
		if err != nil {
			return nil, err
		}
		if seen[cleanPath] {
			continue
		}
		seen[cleanPath] = true
		cleanPaths = append(cleanPaths, cleanPath)
	}
	if len(cleanPaths) == 0 {
		return map[string]RepoBrowserCommit{}, nil
	}
	dir, sha, _, err := m.resolveRepoBrowserRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	args := []string{
		"log",
		"-z",
		"--max-count=" + strconv.Itoa(RepoBrowserLastChangedLogLimit),
		"--format=" + repoBrowserLastChangedCommitFormat,
		"--name-only",
		"--end-of-options",
		sha,
		"--",
	}
	for _, cleanPath := range cleanPaths {
		args = append(args, literalRepoBrowserPathspec(cleanPath))
	}
	out, err := m.git(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("repo browser last changed: %w", err)
	}
	changed, err := parseRepoBrowserLastChanged(out, seen)
	if err != nil {
		return nil, err
	}
	for _, cleanPath := range cleanPaths {
		if _, ok := changed[cleanPath]; ok {
			continue
		}
		commit, ok, err := m.repoBrowserLastChangedForPath(ctx, dir, sha, cleanPath)
		if err != nil {
			return nil, err
		}
		if ok {
			changed[cleanPath] = commit
		}
	}
	return changed, nil
}

func parseRepoBrowserLastChanged(out []byte, wanted map[string]bool) (map[string]RepoBrowserCommit, error) {
	changed := make(map[string]RepoBrowserCommit, len(wanted))
	var current RepoBrowserCommit
	var haveCurrent bool
	var nextTokenIsCommit bool
	for part := range bytes.SplitSeq(out, []byte{0}) {
		token := strings.TrimPrefix(string(part), "\n")
		if token == "" {
			nextTokenIsCommit = true
			continue
		}
		if nextTokenIsCommit {
			commit, err := parseRepoBrowserCommitLine(token)
			if err != nil {
				return nil, err
			}
			current = commit
			haveCurrent = true
			nextTokenIsCommit = false
			continue
		}
		if wanted[token] && haveCurrent {
			if _, exists := changed[token]; !exists {
				changed[token] = current
			}
		}
		if len(changed) == len(wanted) {
			break
		}
	}
	return changed, nil
}

func (m *Manager) repoBrowserLastChangedForPath(
	ctx context.Context,
	dir string,
	sha string,
	pathName string,
) (RepoBrowserCommit, bool, error) {
	out, err := m.git(ctx, dir,
		"log",
		"--max-count=1",
		"--format="+repoBrowserCommitFormat,
		"--end-of-options",
		sha,
		"--",
		literalRepoBrowserPathspec(pathName),
	)
	if err != nil {
		return RepoBrowserCommit{}, false, fmt.Errorf("repo browser last changed %s: %w", pathName, err)
	}
	commits, err := parseRepoBrowserCommitLines(out)
	if err != nil {
		return RepoBrowserCommit{}, false, err
	}
	if len(commits) == 0 {
		return RepoBrowserCommit{}, false, nil
	}
	return commits[0], true, nil
}

func (m *Manager) RepoBrowserFileHistory(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
	pathName string,
) ([]RepoBrowserCommit, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cleanPath, err := cleanRepoBrowserPath(pathName)
	if err != nil {
		return nil, err
	}
	dir, sha, _, err := m.resolveRepoBrowserRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	entry, err := m.lookupRepoBrowserTreeEntry(ctx, dir, sha, cleanPath)
	if err != nil {
		return nil, err
	}
	if entry.Type != "blob" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, cleanPath)
	}
	out, err := m.git(ctx, dir,
		"log",
		"--max-count="+strconv.Itoa(RepoBrowserHistoryLimit),
		"--format="+repoBrowserCommitFormat,
		"--end-of-options",
		sha,
		"--",
		literalRepoBrowserPathspec(cleanPath),
	)
	if err != nil {
		return nil, fmt.Errorf("repo browser file history %s: %w", cleanPath, err)
	}
	return parseRepoBrowserCommitLines(out)
}

func (m *Manager) RepoBrowserCommitDetail(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	root RepoBrowserRef,
	pathName string,
	sha string,
) (RepoBrowserCommit, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	defer unlock()
	cleanPath, err := cleanRepoBrowserPath(pathName)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	if !isFullHexSHA(sha) {
		return RepoBrowserCommit{}, fmt.Errorf("%w: %s", ErrNotFound, sha)
	}
	dir, rootSHA, _, err := m.resolveRepoBrowserRef(ctx, repo, root)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	commitSHA, err := m.resolveRefInDir(ctx, dir, sha)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	inScope, err := m.repoBrowserCommitTouchesPath(ctx, dir, rootSHA, cleanPath, commitSHA)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	if !inScope {
		return RepoBrowserCommit{}, fmt.Errorf("%w: %s", ErrCommitOutOfScope, sha)
	}
	out, err := m.git(ctx, dir,
		"show", "-s", "--format="+repoBrowserCommitDetailFormat, "--end-of-options", commitSHA,
	)
	if err != nil {
		return RepoBrowserCommit{}, fmt.Errorf("repo browser commit detail %s: %w", sha, err)
	}
	commit, err := parseRepoBrowserCommitDetail(out)
	if err != nil {
		return RepoBrowserCommit{}, err
	}
	if commit.SHA == "" {
		return RepoBrowserCommit{}, fmt.Errorf("%w: %s", ErrNotFound, sha)
	}
	return commit, nil
}

func (m *Manager) repoBrowserCommitTouchesPath(
	ctx context.Context,
	dir string,
	rootSHA string,
	pathName string,
	sha string,
) (bool, error) {
	if _, err := m.git(ctx, dir, "merge-base", "--is-ancestor", sha, rootSHA); err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("repo browser commit ancestry %s: %w", sha, err)
	}
	out, err := m.git(ctx, dir,
		"diff-tree",
		"--no-commit-id",
		"--name-only",
		"-r",
		"-m",
		"-z",
		"--root",
		sha,
		"--",
		literalRepoBrowserPathspec(pathName),
	)
	if err != nil {
		return false, fmt.Errorf("repo browser commit scope %s: %w", pathName, err)
	}
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if string(entry) == pathName {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) ResolveRepoBrowserRef(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
) (RepoBrowserRef, error) {
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return RepoBrowserRef{}, err
	}
	defer unlock()
	_, sha, stale, err := m.resolveRepoBrowserRef(ctx, repo, ref)
	if err != nil {
		return RepoBrowserRef{}, err
	}
	resolved := ref
	resolved.SHA = sha
	resolved.Stale = stale
	if stale {
		resolved.RequestedSHA = strings.TrimSpace(ref.SHA)
	}
	return resolved, nil
}

const (
	repoBrowserCommitFormat            = "%H%x1f%an%x1f%ae%x1f%aI%x1f%s"
	repoBrowserCommitDetailFormat      = "%H%x00%an%x00%ae%x00%aI%x00%s%x00%b"
	repoBrowserLastChangedCommitFormat = "%x00" + repoBrowserCommitFormat
)

func parseRepoBrowserCommitLines(out []byte) ([]RepoBrowserCommit, error) {
	var commits []RepoBrowserCommit
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		commit, err := parseRepoBrowserCommitLine(line)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	return commits, scanner.Err()
}

func parseRepoBrowserCommitLine(line string) (RepoBrowserCommit, error) {
	parts := strings.SplitN(line, "\x1f", 5)
	if len(parts) != 5 {
		return RepoBrowserCommit{}, fmt.Errorf("unexpected repo browser commit line: %q", line)
	}
	authoredAt, err := time.Parse(time.RFC3339, parts[3])
	if err != nil {
		return RepoBrowserCommit{}, fmt.Errorf("parse repo browser commit time %q: %w", parts[3], err)
	}
	return RepoBrowserCommit{
		SHA:         parts[0],
		AuthorName:  truncateCommitText(parts[1], commitIdentityMaxBytes),
		AuthorEmail: truncateCommitText(parts[2], commitIdentityMaxBytes),
		AuthoredAt:  authoredAt.UTC(),
		Subject:     truncateCommitText(parts[4], commitMessageMaxBytes),
	}, nil
}

func parseRepoBrowserCommitDetail(out []byte) (RepoBrowserCommit, error) {
	raw := strings.TrimSuffix(string(out), "\n")
	if raw == "" {
		return RepoBrowserCommit{}, nil
	}
	parts := strings.SplitN(raw, "\x00", 6)
	if len(parts) != 6 {
		return RepoBrowserCommit{}, fmt.Errorf("unexpected repo browser commit detail: %q", raw)
	}
	authoredAt, err := time.Parse(time.RFC3339, parts[3])
	if err != nil {
		return RepoBrowserCommit{}, fmt.Errorf("parse repo browser commit time %q: %w", parts[3], err)
	}
	return RepoBrowserCommit{
		SHA:         parts[0],
		AuthorName:  truncateCommitText(parts[1], commitIdentityMaxBytes),
		AuthorEmail: truncateCommitText(parts[2], commitIdentityMaxBytes),
		AuthoredAt:  authoredAt.UTC(),
		Subject:     truncateCommitText(parts[4], commitMessageMaxBytes),
		Body:        truncateCommitText(strings.TrimSuffix(parts[5], "\n"), commitMessageMaxBytes),
	}, nil
}

func (m *Manager) resolveRepoBrowserRef(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	ref RepoBrowserRef,
) (dir string, sha string, stale bool, err error) {
	dir, err = m.repoBrowserClonePath(repo)
	if err != nil {
		return "", "", false, err
	}
	switch ref.Type {
	case RepoBrowserRefBranch:
		if strings.TrimSpace(ref.Name) == "" {
			return "", "", false, fmt.Errorf("%w: empty branch", ErrNotFound)
		}
		sha, err = m.resolveExactRefInDir(ctx, dir, remoteBranchRef(ref.Name))
	case RepoBrowserRefTag:
		if strings.TrimSpace(ref.Name) == "" {
			return "", "", false, fmt.Errorf("%w: empty tag", ErrNotFound)
		}
		sha, err = m.resolveExactRefInDir(ctx, dir, "refs/tags/"+ref.Name)
	case RepoBrowserRefCommit:
		if !isFullHexSHA(ref.SHA) {
			return "", "", false, fmt.Errorf("%w: %s", ErrNotFound, ref.SHA)
		}
		sha, err = m.resolveRefInDir(ctx, dir, ref.SHA)
	default:
		err = fmt.Errorf("%w: unsupported ref type %q", ErrNotFound, ref.Type)
	}
	if err != nil {
		return "", "", false, err
	}
	return dir, sha, ref.SHA != "" && ref.SHA != sha, nil
}

func (m *Manager) repoBrowserClonePath(repo RepoBrowserRepoRef) (string, error) {
	return m.ClonePathInNamespace(repoBrowserCloneNamespace(repo), repo.Host, repo.Owner, repo.Name)
}

func (m *Manager) repoBrowserCloneBarrier(repo RepoBrowserRepoRef) *repoBrowserBarrier {
	namespace := repoBrowserCloneNamespace(repo)
	m.repoBrowserBarrierMu.Lock()
	defer m.repoBrowserBarrierMu.Unlock()
	if m.repoBrowserBarriers == nil {
		m.repoBrowserBarriers = make(map[string]*repoBrowserBarrier)
	}
	barrier := m.repoBrowserBarriers[namespace]
	if barrier == nil {
		barrier = newRepoBrowserBarrier()
		m.repoBrowserBarriers[namespace] = barrier
	}
	return barrier
}

func (m *Manager) lockRepoBrowserRead(
	ctx context.Context, repo RepoBrowserRepoRef,
) (func(), error) {
	namespace := repoBrowserCloneNamespace(repo)
	barrier := m.repoBrowserCloneBarrier(repo)
	if err := barrier.lockRead(ctx, func() {
		if hook := m.repoBrowserReadWaitingForTest; hook != nil {
			hook(namespace)
		}
	}); err != nil {
		return nil, err
	}
	if hook := m.repoBrowserAfterReadLockForTest; hook != nil {
		hook(namespace)
	}
	return barrier.unlockRead, nil
}

// AdoptLegacyClones copies the pre-stable-ID main clone and moves the legacy
// repository-browser clone into identity-partitioned storage without
// contacting the provider. The path-scoped main clone remains in place for
// workspace flows that intentionally continue to use it. Callers must invoke
// this only after the repository catalog has verified that the current route
// has no other historical owner.
func (m *Manager) AdoptLegacyClones(ctx context.Context, repo RepoBrowserRepoRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(repo.ProviderRepoID) == "" {
		return errors.New("adopt legacy clones requires a stable provider repository id")
	}
	if err := m.validateRepoBrowserRemote(repo, repo.RemoteURL); err != nil {
		return err
	}

	legacyMain, err := m.ClonePath(repo.Provider, repo.Host, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	stableMain, err := m.ClonePathForContext(
		WithRepositoryIdentity(ctx, repo.ProviderRepoID),
		repo.Provider, repo.Host, repo.Owner, repo.Name,
	)
	if err != nil {
		return err
	}
	legacyBrowserRepo := repo
	legacyBrowserRepo.ProviderRepoID = ""
	legacyBrowser, err := m.repoBrowserClonePath(legacyBrowserRepo)
	if err != nil {
		return err
	}
	stableBrowser, err := m.repoBrowserClonePath(repo)
	if err != nil {
		return err
	}

	if err := m.copyLegacyMainClone(
		ctx, legacyMain, stableMain, repo.Host, repo.Owner, repo.Name, repo.RemoteURL,
	); err != nil {
		return fmt.Errorf("adopt legacy main clone: %w", err)
	}
	if err := m.adoptLegacyClone(
		ctx, legacyBrowser, stableBrowser, repo.Host, repo.Owner, repo.Name,
	); err != nil {
		return fmt.Errorf("adopt legacy repository browser clone: %w", err)
	}
	return nil
}

func (m *Manager) copyLegacyMainClone(
	ctx context.Context,
	legacyPath, stablePath, host, owner, name, remoteURL string,
) error {
	if legacyPath == stablePath {
		return nil
	}
	stableExists, err := m.validateStableClone(ctx, stablePath)
	if err != nil || stableExists {
		return err
	}
	legacyExists, err := m.validateLegacyClone(
		ctx, legacyPath, host, owner, name,
	)
	if err != nil || !legacyExists {
		return err
	}

	stableParent := filepath.Dir(stablePath)
	if err := os.MkdirAll(stableParent, 0o755); err != nil {
		return err
	}
	// Keep the lock file after unlock: removing a flock path can let a third
	// process lock a new inode while another adopter still holds the old one.
	adoptionLock := flock.New(stablePath + ".adopt.lock")
	locked, err := adoptionLock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire stable clone adoption lock: %w", err)
	}
	if !locked {
		return errors.New("stable clone adoption lock not acquired")
	}
	defer func() {
		_ = adoptionLock.Unlock()
		_ = adoptionLock.Close()
	}()

	stableExists, err = m.validateStableClone(ctx, stablePath)
	if err != nil || stableExists {
		return err
	}
	staging, err := os.MkdirTemp(
		stableParent, "."+filepath.Base(stablePath)+".adopting-",
	)
	if err != nil {
		return fmt.Errorf("create stable clone staging path: %w", err)
	}
	if err := os.Remove(staging); err != nil {
		return fmt.Errorf("prepare stable clone staging path: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if _, err := m.git(
		ctx, stableParent,
		"clone", "--mirror", "--no-hardlinks", legacyPath, staging,
	); err != nil {
		return fmt.Errorf("copy legacy main clone: %w", err)
	}
	if _, err := m.git(
		ctx, staging, "config", "remote.origin.url", remoteURL,
	); err != nil {
		return fmt.Errorf("set copied main clone origin: %w", err)
	}
	if _, err := m.git(
		ctx, staging, "config", "--unset-all", "remote.origin.fetch",
	); err != nil {
		return fmt.Errorf("reset copied main clone refspecs: %w", err)
	}
	if _, err := m.git(
		ctx, staging, "config", "--unset", "remote.origin.mirror",
	); err != nil {
		return fmt.Errorf("reset copied main clone mirror mode: %w", err)
	}
	for _, refspec := range defaultRefspecs() {
		if _, err := m.git(
			ctx, staging, "config", "--add", "remote.origin.fetch", refspec,
		); err != nil {
			return fmt.Errorf("add copied main clone refspec %q: %w", refspec, err)
		}
	}
	if complete, err := m.validateStableClone(ctx, staging); err != nil {
		return fmt.Errorf("validate copied main clone: %w", err)
	} else if !complete {
		return errors.New("validate copied main clone: staging clone is missing")
	}

	stableExists, err = m.validateStableClone(ctx, stablePath)
	if err != nil {
		return err
	}
	if stableExists {
		return nil
	}
	if err := os.Rename(staging, stablePath); err != nil {
		if exists, validationErr := m.validateStableClone(ctx, stablePath); exists && validationErr == nil {
			return nil
		} else if validationErr != nil {
			return errors.Join(err, validationErr)
		}
		return err
	}
	return nil
}

func (m *Manager) validateStableClone(
	ctx context.Context, stablePath string,
) (bool, error) {
	info, err := os.Lstat(stablePath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("stable clone path is not a directory")
	}
	headInfo, err := os.Lstat(filepath.Join(stablePath, "HEAD"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, errors.New("stable clone path is incomplete: missing HEAD")
		}
		return false, err
	}
	if !headInfo.Mode().IsRegular() {
		return false, errors.New("stable clone path is incomplete: HEAD is not a regular file")
	}
	out, err := m.git(ctx, stablePath, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		if err == nil {
			err = errors.New("repository is not bare")
		}
		return false, fmt.Errorf("stable clone path is incomplete: %w", err)
	}
	return true, nil
}

func (m *Manager) validateLegacyClone(
	ctx context.Context, legacyPath, host, owner, name string,
) (bool, error) {
	info, err := os.Lstat(legacyPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("legacy clone path is not a directory")
	}
	if _, err := os.Stat(filepath.Join(legacyPath, "HEAD")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := m.validateRemoteIdentity(ctx, legacyPath, "origin", host, owner, name); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) adoptLegacyClone(
	ctx context.Context,
	legacyPath, stablePath, host, owner, name string,
) error {
	if legacyPath == stablePath {
		return nil
	}
	stableExists, err := m.validateStableClone(ctx, stablePath)
	if err != nil || stableExists {
		return err
	}
	legacyExists, err := m.validateLegacyClone(ctx, legacyPath, host, owner, name)
	if err != nil || !legacyExists {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(stablePath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(legacyPath, stablePath); err != nil {
		if _, statErr := os.Stat(filepath.Join(stablePath, "HEAD")); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) EnsureRepoBrowserClone(ctx context.Context, repo RepoBrowserRepoRef) error {
	if err := m.ensureRepoBrowserCloneLocal(ctx, repo); err != nil {
		return err
	}
	m.registerRepoBrowserRepo(repo)
	return nil
}

func (m *Manager) RefreshRepoBrowserClone(ctx context.Context, repo RepoBrowserRepoRef) error {
	return m.refreshRepoBrowserClone(ctx, repo, repoBrowserRefreshRespectCaller)
}

type repoBrowserRefreshWorkMode uint8

const (
	repoBrowserRefreshRespectCaller repoBrowserRefreshWorkMode = iota
	repoBrowserRefreshDetachCaller
)

func repoBrowserRefreshWorkParent(ctx context.Context, mode repoBrowserRefreshWorkMode) context.Context {
	if mode == repoBrowserRefreshDetachCaller {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

func (m *Manager) refreshRepoBrowserClone(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	mode repoBrowserRefreshWorkMode,
) error {
	if repo.RequireCredential {
		ctx = WithRequiredCredential(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validateRepoBrowserRouteFence(ctx, repo); err != nil {
		m.evictStaleRepoBrowserRegistration(repo, err)
		return err
	}
	if repo.ValidateRouteFence != nil && repo.PublishIfRouteFenceMatches == nil {
		return errors.New("repo browser fenced refresh requires guarded publication")
	}
	namespace := repoBrowserCloneNamespace(repo)
	if err := m.validateRepoBrowserRemote(repo, repo.RemoteURL); err != nil {
		return err
	}
	dir, err := m.ClonePathInNamespace(namespace, repo.Host, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	key := ensureCloneKey(namespace, repo.Host, repo.Owner, repo.Name)
	run := func() (any, error) {
		opCtx, cancel := context.WithTimeout(
			repoBrowserRefreshWorkParent(ctx, mode),
			ensureCloneTimeout,
		)
		defer cancel()
		staging, seeded, err := m.prepareRepoBrowserStaging(opCtx, repo, dir)
		if err != nil {
			return nil, err
		}
		if !seeded {
			err = m.cloneBare(
				opCtx, repo.Provider, repo.Host, repo.Owner, repo.Name,
				staging, repo.RemoteURL,
			)
		} else {
			if inject := m.repoBrowserFetchErrorForTest; inject != nil {
				err = inject(repo.RouteFence)
			}
			if err == nil {
				err = m.fetch(
					opCtx, repo.Provider, repo.Host, repo.Owner, repo.Name, staging,
				)
			}
		}
		if err != nil {
			return nil, m.finishRepoBrowserStagingFailure(opCtx, repo, staging, err)
		}
		if err := m.fetchRepoBrowserTags(
			opCtx, repo.Provider, repo.Host, repo.Owner, repo.Name, staging,
		); err != nil {
			return nil, m.finishRepoBrowserStagingFailure(opCtx, repo, staging, err)
		}
		if err := m.validateRepoBrowserRouteFence(opCtx, repo); err != nil {
			return nil, m.discardRepoBrowserStaging(
				staging, err, errors.Is(err, ErrRepoBrowserRouteFenceChanged),
			)
		}
		moved, err := m.publishRepoBrowserStagingGuarded(opCtx, repo, staging, dir)
		if err != nil {
			if moved {
				return nil, err
			}
			return nil, m.discardRepoBrowserStaging(
				staging, err, errors.Is(err, ErrRepoBrowserRouteFenceChanged),
			)
		}
		return nil, nil
	}
	start := func() <-chan singleflight.Result {
		ch := m.repoBrowserRefreshSF.DoChan(key, run)
		if hook := m.repoBrowserAfterRefreshJoinForTest; hook != nil {
			hook(repo.RouteFence)
		}
		return ch
	}
	wait := func(ch <-chan singleflight.Result) (singleflight.Result, error) {
		select {
		case res := <-ch:
			return res, nil
		case <-ctx.Done():
			return singleflight.Result{}, ctx.Err()
		}
	}
	res, err := wait(start())
	if err != nil {
		return err
	}
	if res.Err != nil {
		if _, ok := errors.AsType[*repoBrowserCloneValidationError](res.Err); !ok {
			if errors.Is(res.Err, ErrRepoBrowserRouteFenceChanged) {
				if ownErr := m.validateRepoBrowserRouteFence(ctx, repo); ownErr != nil {
					m.evictStaleRepoBrowserRegistration(repo, ownErr)
				}
			}
			return res.Err
		}
		if ownErr := m.validateRepoBrowserRouteFence(ctx, repo); ownErr != nil {
			m.evictStaleRepoBrowserRegistration(repo, ownErr)
			return ownErr
		}
		res, err = wait(start())
		if err != nil {
			return err
		}
		if res.Err != nil {
			return res.Err
		}
	}
	if err := m.validateRepoBrowserRouteFence(ctx, repo); err != nil {
		m.evictStaleRepoBrowserRegistration(repo, err)
		return err
	}
	m.registerRepoBrowserRepo(repo)
	return nil
}

func (m *Manager) finishRepoBrowserStagingFailure(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	staging string,
	mutationErr error,
) error {
	validationErr := m.validateRepoBrowserRouteFence(ctx, repo)
	reason := errors.Join(mutationErr, validationErr)
	return m.discardRepoBrowserStaging(
		staging,
		reason,
		errors.Is(validationErr, ErrRepoBrowserRouteFenceChanged),
	)
}

func (m *Manager) prepareRepoBrowserStaging(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	published string,
) (string, bool, error) {
	if err := os.MkdirAll(filepath.Dir(published), 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir repo browser staging parent: %w", err)
	}
	staging, err := os.MkdirTemp(
		filepath.Dir(published), "."+filepath.Base(published)+".staging-",
	)
	if err != nil {
		return "", false, fmt.Errorf("create repo browser staging path: %w", err)
	}
	if err := os.Remove(staging); err != nil {
		return "", false, fmt.Errorf("prepare repo browser staging path: %w", err)
	}
	fail := func(reason error) (string, bool, error) {
		if cleanupErr := m.removeRepoBrowserStaging(staging); cleanupErr != nil {
			return "", false, errors.Join(
				reason,
				fmt.Errorf("remove unpublished repo browser staging clone: %w", cleanupErr),
			)
		}
		return "", false, reason
	}

	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return fail(err)
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(published, "HEAD")); err != nil {
		if os.IsNotExist(err) {
			return staging, false, nil
		}
		return fail(err)
	}
	if out, err := m.git(ctx, published, "config", "--get", "remote.origin.url"); err == nil {
		if err := m.validateRepoBrowserRemote(repo, strings.TrimSpace(string(out))); err != nil {
			return fail(err)
		}
	}
	if _, err := m.git(
		ctx, filepath.Dir(published),
		"clone", "--mirror", "--no-hardlinks", published, staging,
	); err != nil {
		return fail(fmt.Errorf("seed repo browser staging clone: %w", err))
	}
	if _, err := m.git(ctx, staging, "config", "remote.origin.url", repo.RemoteURL); err != nil {
		return fail(fmt.Errorf("set repo browser staging origin: %w", err))
	}
	_, _ = m.git(ctx, staging, "config", "--unset-all", "remote.origin.fetch")
	_, _ = m.git(ctx, staging, "config", "--unset", "remote.origin.mirror")
	for _, refspec := range defaultRefspecs() {
		if _, err := m.git(
			ctx, staging, "config", "--add", "remote.origin.fetch", refspec,
		); err != nil {
			return fail(fmt.Errorf("add repo browser staging refspec %q: %w", refspec, err))
		}
	}
	return staging, true, nil
}

func (m *Manager) discardRepoBrowserStaging(
	staging string,
	reason error,
	retryCurrentFence bool,
) error {
	if cleanupErr := m.removeRepoBrowserStaging(staging); cleanupErr != nil {
		return errors.Join(
			reason,
			fmt.Errorf("remove unpublished repo browser staging clone: %w", cleanupErr),
		)
	}
	if retryCurrentFence {
		return &repoBrowserCloneValidationError{err: reason}
	}
	return reason
}

func (m *Manager) removeRepoBrowserStaging(path string) error {
	if remove := m.removeRepoBrowserStagingForTest; remove != nil {
		return remove(path)
	}
	return os.RemoveAll(path)
}

func (m *Manager) publishRepoBrowserStagingGuarded(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	staging string,
	published string,
) (bool, error) {
	moved := false
	publish := func() error {
		var err error
		moved, err = m.publishRepoBrowserStaging(ctx, repo, staging, published)
		return err
	}
	if repo.PublishIfRouteFenceMatches == nil {
		return moved, publish()
	}
	matches, err := repo.PublishIfRouteFenceMatches(ctx, repo.RouteFence, publish)
	if err != nil {
		return moved, err
	}
	if !matches {
		return moved, ErrRepoBrowserRouteFenceChanged
	}
	return moved, nil
}

func (m *Manager) publishRepoBrowserStaging(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	staging string,
	published string,
) (bool, error) {
	barrier := m.repoBrowserCloneBarrier(repo)
	if err := barrier.lockWrite(ctx); err != nil {
		return false, err
	}
	previous := staging + ".previous"
	hadPrevious := false
	if err := os.Rename(published, previous); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			barrier.unlockWrite()
			return false, fmt.Errorf("move published repo browser clone aside: %w", err)
		}
	} else {
		hadPrevious = true
	}
	publish := os.Rename
	if injected := m.publishRepoBrowserStagingForTest; injected != nil {
		publish = injected
	}
	if err := publish(staging, published); err != nil {
		var restoreErr error
		if hadPrevious {
			restoreErr = os.Rename(previous, published)
		}
		barrier.unlockWrite()
		return false, errors.Join(
			fmt.Errorf("publish repo browser staging clone: %w", err),
			wrapOptionalError("restore published repo browser clone", restoreErr),
		)
	}
	barrier.unlockWrite()
	if !hadPrevious {
		return true, nil
	}
	if err := os.RemoveAll(previous); err != nil {
		return true, fmt.Errorf("remove previous repo browser clone: %w", err)
	}
	return true, nil
}

func wrapOptionalError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func (m *Manager) RefreshRepoBrowserClones(ctx context.Context) {
	for _, repo := range m.repoBrowserReposSnapshot() {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := m.refreshRepoBrowserClone(ctx, repo, repoBrowserRefreshRespectCaller); err != nil {
			slog.Warn("repo browser clone refresh failed",
				"provider", repo.Provider,
				"host", repo.Host,
				"repo", repo.RepoPath,
				"err", err)
		}
	}
}

func (m *Manager) RegisterExistingRepoBrowserClone(ctx context.Context, repo RepoBrowserRepoRef) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := m.validateRepoBrowserRouteFence(ctx, repo); err != nil {
		m.evictStaleRepoBrowserRegistration(repo, err)
		return false, err
	}
	if err := m.validateRepoBrowserRemote(repo, repo.RemoteURL); err != nil {
		return false, err
	}
	dir, err := m.repoBrowserClonePath(repo)
	if err != nil {
		return false, err
	}
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if out, err := m.git(ctx, dir, "config", "--get", "remote.origin.url"); err == nil {
		if err := m.validateRepoBrowserRemote(repo, strings.TrimSpace(string(out))); err != nil {
			return false, err
		}
	}
	m.ensureRefspecs(ctx, dir)
	m.registerRepoBrowserRepo(repo)
	return true, nil
}

func (m *Manager) ensureRepoBrowserCloneLocal(ctx context.Context, repo RepoBrowserRepoRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validateRepoBrowserRouteFence(ctx, repo); err != nil {
		m.evictStaleRepoBrowserRegistration(repo, err)
		return err
	}
	namespace := repoBrowserCloneNamespace(repo)
	if err := m.validateRepoBrowserRemote(repo, repo.RemoteURL); err != nil {
		return err
	}
	dir, err := m.ClonePathInNamespace(namespace, repo.Host, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	unlock, err := m.lockRepoBrowserRead(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(err) {
		unlock()
		return m.refreshRepoBrowserClone(ctx, repo, repoBrowserRefreshDetachCaller)
	} else if err != nil {
		unlock()
		return err
	}
	if out, err := m.git(ctx, dir, "config", "--get", "remote.origin.url"); err == nil {
		if err := m.validateRepoBrowserRemote(repo, strings.TrimSpace(string(out))); err != nil {
			unlock()
			return err
		}
	}
	m.ensureRefspecs(ctx, dir)
	// Route reconciliation may queue behind guarded publication. Release the
	// clone read barrier before re-entering that DB guard so publication never
	// waits on a reader that is itself waiting on reconciliation.
	unlock()
	if err := m.validateRepoBrowserRouteFence(ctx, repo); err != nil {
		m.evictStaleRepoBrowserRegistration(repo, err)
		return err
	}
	return nil
}

func (m *Manager) registerRepoBrowserRepo(repo RepoBrowserRepoRef) {
	m.repoBrowserMu.Lock()
	defer m.repoBrowserMu.Unlock()
	m.repoBrowserRepos[repoBrowserCloneNamespace(repo)] = repo
}

func (m *Manager) validateRepoBrowserRouteFence(
	ctx context.Context, repo RepoBrowserRepoRef,
) error {
	if repo.ValidateRouteFence == nil {
		return nil
	}
	matches, err := repo.ValidateRouteFence(ctx, repo.RouteFence)
	if err != nil {
		return err
	}
	if !matches {
		return ErrRepoBrowserRouteFenceChanged
	}
	return nil
}

func (m *Manager) evictStaleRepoBrowserRegistration(
	repo RepoBrowserRepoRef, err error,
) {
	if !errors.Is(err, ErrRepoBrowserRouteFenceChanged) {
		return
	}
	namespace := repoBrowserCloneNamespace(repo)
	m.repoBrowserMu.Lock()
	defer m.repoBrowserMu.Unlock()
	current, ok := m.repoBrowserRepos[namespace]
	if ok && current.RouteFence == repo.RouteFence {
		delete(m.repoBrowserRepos, namespace)
	}
}

func (m *Manager) repoBrowserReposSnapshot() []RepoBrowserRepoRef {
	m.repoBrowserMu.Lock()
	defer m.repoBrowserMu.Unlock()
	repos := make([]RepoBrowserRepoRef, 0, len(m.repoBrowserRepos))
	for _, repo := range m.repoBrowserRepos {
		repos = append(repos, repo)
	}
	return repos
}

func (m *Manager) fetchRepoBrowserTags(
	ctx context.Context, platform, host, owner, name, clonePath string,
) error {
	// Repo browser refs need current tag targets, but general clone refreshes
	// deliberately fetch with --no-tags so sync/diff hot paths are not coupled
	// to tag namespace size or moved-tag failures.
	_, err := retryTransient(ctx, "git fetch repo browser tags", func() ([]byte, error) {
		return m.gitNetworked(
			ctx, m.sourceForRepo(platform, host, owner, name), host, clonePath, nil,
			"fetch", "origin", "+refs/tags/*:refs/tags/*",
		)
	})
	if err != nil {
		return fmt.Errorf("git fetch repo browser tags: %w", err)
	}
	return nil
}

func (m *Manager) validateRepoBrowserRemote(repo RepoBrowserRepoRef, remoteURL string) error {
	if err := validateRemoteURLIdentity(repo.Host, repo.Owner, repo.Name, remoteURL); err != nil {
		return err
	}
	return m.validateRemoteTransport(repo.Provider, repo.Host, remoteURL)
}

func repoBrowserCloneNamespace(repo RepoBrowserRepoRef) string {
	parts := []string{
		strings.TrimSpace(repo.Provider),
		strings.TrimSpace(repo.Host),
		strings.Trim(strings.TrimSpace(repo.RepoPath), "/"),
	}
	// The stable provider identity partitions browser storage across route
	// reuse: a replacement repository on a reused path must not serve the
	// displaced repository's cached refs or SHA-addressable objects.
	// Repositories without a verified identity keep path-scoped storage.
	if id := strings.TrimSpace(repo.ProviderRepoID); id != "" {
		parts = append(parts, id)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "repo-browser-" + hex.EncodeToString(sum[:8])
}

func (m *Manager) resolveRepoBrowserDefaultBranch(
	ctx context.Context,
	repo RepoBrowserRepoRef,
	preferred string,
) (branch string, ref string, err error) {
	dir, err := m.repoBrowserClonePath(repo)
	if err != nil {
		return "", "", err
	}

	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		for _, candidate := range branchActivityRefCandidates(preferred) {
			if sha, err := m.resolveExactRefInDir(ctx, dir, candidate); err == nil {
				return defaultBranchNameForResolvedCandidate(preferred, candidate), sha, nil
			} else if !isMissingRefError(err) {
				return "", "", fmt.Errorf("resolve preferred default branch %s: %w", preferred, err)
			}
		}
	}

	out, err := m.git(ctx, dir,
		"symbolic-ref", "--quiet", "refs/remotes/origin/HEAD",
	)
	if err != nil {
		return "", "", fmt.Errorf("resolve origin HEAD: %w", err)
	}
	remoteRef := strings.TrimSpace(string(out))
	branch, ok := strings.CutPrefix(remoteRef, "refs/remotes/origin/")
	if !ok || branch == "" || branch == "HEAD" {
		return "", "", fmt.Errorf("resolve origin HEAD: %w", ErrNotFound)
	}
	sha, err := m.resolveExactRefInDir(ctx, dir, remoteRef)
	if err != nil {
		return "", "", fmt.Errorf("resolve origin HEAD target %s: %w", remoteRef, err)
	}
	return branch, sha, nil
}

func (m *Manager) resolveExactRefInDir(
	ctx context.Context,
	dir string,
	ref string,
) (string, error) {
	out, err := m.git(ctx, dir,
		"show-ref", "--verify", "--hash", ref,
	)
	if err != nil {
		if isShowRefMissingError(err) {
			return "", fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return "", err
	}
	objectID := strings.TrimSpace(string(out))
	if objectID == "" {
		return "", fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return m.resolveRefInDir(ctx, dir, objectID)
}

func isShowRefMissingError(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if strings.Contains(strings.ToLower(err.Error()), "not a valid ref") {
		return true
	}
	var exitErr interface {
		ExitCode() (int, bool)
	}
	if errors.As(err, &exitErr) {
		code, ok := exitErr.ExitCode()
		return ok && code == 1
	}
	return false
}

func cleanRepoBrowserPath(pathName string) (string, error) {
	if pathName == "" || strings.ContainsRune(pathName, 0) || path.IsAbs(pathName) {
		return "", ErrUnsafePath
	}
	cleaned := path.Clean(strings.ReplaceAll(pathName, "\\", "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrUnsafePath
	}
	return cleaned, nil
}

func literalRepoBrowserPathspec(pathName string) string {
	return ":(literal)" + pathName
}

func mediaTypeForRepoBrowserPath(pathName string) string {
	ext := strings.ToLower(filepath.Ext(pathName))
	if ext == ".svg" {
		return "image/svg+xml"
	}
	if typ := mime.TypeByExtension(ext); typ != "" {
		return typ
	}
	return ""
}

func repoBrowserAssetMediaTypeAllowed(mediaType string) bool {
	typ, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		typ = mediaType
	}
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "image/avif", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func isFullHexSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
