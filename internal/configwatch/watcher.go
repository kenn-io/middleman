// Package configwatch watches a config file for in-place edits and invokes
// a callback after debouncing fsnotify events. It registers the watch on
// the parent directory so atomic-rename save patterns (e.g. vim ":w") and
// editor-style write-to-temp-then-rename flows are caught on both Linux
// inotify and macOS APFS FSEvents.
package configwatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is the default debounce window applied between the last
// observed filesystem event and the callback invocation. Editors that save
// via the temp-file + rename pattern (vim, sd -i, etc.) emit several events
// in quick succession; the debounce coalesces those into one callback.
const DefaultDebounce = 100 * time.Millisecond

// Watcher observes a single configuration file and invokes OnChange once
// per debounced burst of filesystem events. Zero value is not usable;
// construct via New.
type Watcher struct {
	path     string
	dir      string
	base     string
	debounce time.Duration
	onChange func()
	now      func() time.Time

	// readyCh is closed once the underlying fsnotify watcher has either
	// successfully registered the directory or recorded a startup error.
	readyCh chan struct{}
	readyMu sync.Mutex
	readyOK bool
	readyEr error

	// done is closed when the run goroutine exits.
	done chan struct{}
}

// Options configure a Watcher.
type Options struct {
	// Path is the absolute path to the config file. The parent directory
	// is registered with fsnotify; events whose basename matches Path are
	// forwarded through the debounce timer.
	Path string

	// OnChange is invoked once per debounced event burst. Required.
	// Callers should make OnChange idempotent because the watcher will
	// also fire for the daemon's own writes (e.g. UI Save).
	OnChange func()

	// Debounce overrides DefaultDebounce. Values <= 0 fall back to the
	// default.
	Debounce time.Duration

	// Now overrides time.Now for tests. nil falls back to time.Now.
	Now func() time.Time
}

// New constructs a Watcher. It validates the inputs but does not touch the
// filesystem; call Start to begin watching.
func New(opts Options) (*Watcher, error) {
	if opts.Path == "" {
		return nil, errors.New("configwatch: Path is required")
	}
	if opts.OnChange == nil {
		return nil, errors.New("configwatch: OnChange is required")
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("configwatch: resolve path: %w", err)
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Watcher{
		path:     path,
		dir:      filepath.Dir(path),
		base:     filepath.Base(path),
		debounce: debounce,
		onChange: opts.OnChange,
		now:      now,
		readyCh:  make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Start launches the watcher goroutine. It returns immediately; the
// goroutine exits when ctx is canceled. Start may only be called once.
func (w *Watcher) Start(ctx context.Context) {
	go w.run(ctx)
}

// WaitReady blocks until the watcher has registered the directory with
// fsnotify, the watcher's run loop exits, or ctx is canceled. It returns
// any startup error recorded by the run goroutine. Tests use it to
// synchronize on watcher registration before mutating the file.
func (w *Watcher) WaitReady(ctx context.Context) error {
	select {
	case <-w.readyCh:
		w.readyMu.Lock()
		defer w.readyMu.Unlock()
		return w.readyEr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a channel that is closed when the watcher goroutine has
// exited. Useful for tests that want to observe shutdown.
func (w *Watcher) Done() <-chan struct{} { return w.done }

func (w *Watcher) markReady(err error) {
	w.readyMu.Lock()
	defer w.readyMu.Unlock()
	if w.readyOK {
		return
	}
	w.readyOK = true
	w.readyEr = err
	close(w.readyCh)
}

func (w *Watcher) run(ctx context.Context) {
	defer close(w.done)

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.markReady(fmt.Errorf("configwatch: new watcher: %w", err))
		return
	}
	defer fsw.Close()

	if err := fsw.Add(w.dir); err != nil {
		w.markReady(fmt.Errorf("configwatch: add %s: %w", w.dir, err))
		return
	}
	w.markReady(nil)

	w.runEvents(ctx, fsw.Events, fsw.Errors)
}

func (w *Watcher) runEvents(ctx context.Context, events <-chan fsnotify.Event, errs <-chan error) {
	// debounceTimer is created lazily so we don't pay for a stopped
	// timer on watchers that never see an event. The callback runs in
	// this goroutine, not via time.AfterFunc, so Done only closes after
	// any in-flight callback has returned.
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounceTimer != nil {
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-debounceC:
			debounceC = nil
			// Skip empty defensive guard: if OnChange panics, propagate.
			// Callers should keep OnChange robust; this watcher must not
			// silently swallow callback errors.
			w.onChange()
		case ev, ok := <-events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != w.base {
				continue
			}
			// Watch RENAME and REMOVE too: atomic-rename editors
			// (vim's :w with backupcopy=no) may produce a RENAME
			// where the original inode is moved aside before the
			// new file is renamed into place. Some editors also
			// recreate the file; on macOS APFS the kqueue-backed
			// fsnotify implementation surfaces these as CREATE.
			// CHMOD-only events are still forwarded because some
			// editor save flows touch permissions and we'd rather
			// be conservative.
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(w.debounce)
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(w.debounce)
			}
			debounceC = debounceTimer.C
		case _, ok := <-errs:
			if !ok {
				return
			}
			// fsnotify errors are surfaced via the Errors channel;
			// log via the caller's debug stream is out of scope for
			// this leaf package. We swallow them so a single read
			// error doesn't tear down the watcher loop.
		}
	}
}
