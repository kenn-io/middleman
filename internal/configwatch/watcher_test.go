package configwatch

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWatcher constructs a watcher with a 25 ms debounce so tests stay
// fast while still verifying the coalescing behavior.
func newTestWatcher(t *testing.T, path string, onChange func()) *Watcher {
	t.Helper()
	w, err := New(Options{
		Path:     path,
		OnChange: onChange,
		Debounce: 25 * time.Millisecond,
	})
	require.NoError(t, err)
	return w
}

func waitForCount(t *testing.T, c *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Equalf(t, want, c.Load(), "callback count never reached %d", want)
}

func TestWatcher_NewValidatesInputs(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		err  string
	}{
		{name: "missing path", opts: Options{OnChange: func() {}}, err: "Path is required"},
		{name: "missing callback", opts: Options{Path: "/tmp/x"}, err: "OnChange is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.err)
		})
	}
}

func TestWatcher_FiresAfterDebounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("a = 1"), 0o600))

	var count atomic.Int32
	w := newTestWatcher(t, path, func() { count.Add(1) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.Start(ctx)
	require.NoError(t, w.WaitReady(ctx))

	require.NoError(t, os.WriteFile(path, []byte("a = 2"), 0o600))

	waitForCount(t, &count, 1, time.Second)
}

func TestWatcher_DebouncesBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		const debounce = 250 * time.Millisecond
		var count atomic.Int32
		w, err := New(Options{
			Path: "config.toml", Debounce: debounce,
			OnChange: func() { count.Add(1) },
		})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		events := make(chan fsnotify.Event)
		go w.runEvents(ctx, events, nil)

		// Real filesystem writes can straddle debounce windows under load.
		// Control event delivery and time while exercising the real event loop.
		for range 5 {
			events <- fsnotify.Event{Name: w.path, Op: fsnotify.Write}
			time.Sleep(debounce / 2)
			synctest.Wait()
			assert.Equal(int32(0), count.Load(), "each event must postpone the callback")
		}
		time.Sleep(debounce / 2)
		synctest.Wait()
		assert.Equal(int32(1), count.Load(), "one callback after the last event's debounce window")
		time.Sleep(2 * debounce)
		synctest.Wait()
		assert.Equal(int32(1), count.Load(), "no additional callback without another event")
	})
}

func TestWatcher_IgnoresUnrelatedFiles(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	req.NoError(os.WriteFile(path, []byte("a = 1"), 0o600))

	var count atomic.Int32
	w := newTestWatcher(t, path, func() { count.Add(1) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.Start(ctx)
	req.NoError(w.WaitReady(ctx))

	// Writes to a sibling file in the same directory must not fire.
	sibling := filepath.Join(dir, "other.toml")
	req.NoError(os.WriteFile(sibling, []byte("noise = true"), 0o600))

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), count.Load(),
		"sibling file should not trigger the watcher")
}

func TestWatcher_FiresOnAtomicRename(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	req.NoError(os.WriteFile(path, []byte("a = 1"), 0o600))

	var count atomic.Int32
	w := newTestWatcher(t, path, func() { count.Add(1) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.Start(ctx)
	req.NoError(w.WaitReady(ctx))

	// Atomic-rename save pattern: write a sibling, then rename over.
	// Vim's ":w" uses this when backupcopy=auto.
	tmp := filepath.Join(dir, ".config.toml.tmp")
	req.NoError(os.WriteFile(tmp, []byte("a = 2"), 0o600))
	req.NoError(os.Rename(tmp, path))

	waitForCount(t, &count, 1, time.Second)
}

func TestWatcher_StartFailureSurfacedThroughWaitReady(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "config.toml")
	w, err := New(Options{
		Path:     missing,
		OnChange: func() {},
		Debounce: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.Start(ctx)

	err = w.WaitReady(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add")
}

func TestWatcher_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("a = 1"), 0o600))

	w := newTestWatcher(t, path, func() {})

	ctx, cancel := context.WithCancel(t.Context())
	w.Start(ctx)
	require.NoError(t, w.WaitReady(ctx))

	cancel()

	select {
	case <-w.Done():
	case <-time.After(time.Second):
		require.FailNow(t, "watcher did not stop after context cancel")
	}
}

func TestWatcher_DoneWaitsForInFlightCallback(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("a = 1"), 0o600))

	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	w, err := New(Options{
		Path:     path,
		Debounce: 10 * time.Millisecond,
		OnChange: func() {
			close(callbackStarted)
			<-callbackRelease
		},
	})
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	w.Start(ctx)
	require.NoError(w.WaitReady(ctx))

	require.NoError(os.WriteFile(path, []byte("a = 2"), 0o600))
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		require.FailNow("callback did not start")
	}

	cancel()
	select {
	case <-w.Done():
		require.FailNow("watcher stopped before callback returned")
	case <-time.After(50 * time.Millisecond):
	}

	close(callbackRelease)
	select {
	case <-w.Done():
	case <-time.After(time.Second):
		require.FailNow("watcher did not stop after callback returned")
	}
}
