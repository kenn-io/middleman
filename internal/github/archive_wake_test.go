package github

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/archive"
)

// pacedArchiveRunner records when each pass ran and answers with a
// configurable "attempted work" result.
type pacedArchiveRunner struct {
	worked atomic.Bool
	fail   atomic.Bool
	mu     sync.Mutex
	passes []time.Time
}

func (r *pacedArchiveRunner) RunPass(context.Context) (bool, error) {
	r.mu.Lock()
	r.passes = append(r.passes, time.Now())
	r.mu.Unlock()
	if r.fail.Load() {
		return false, errors.New("store unavailable")
	}
	return r.worked.Load(), nil
}

// reset clears the recorded passes and returns the new origin for offsets.
func (r *pacedArchiveRunner) reset() time.Time {
	r.mu.Lock()
	r.passes = nil
	r.mu.Unlock()
	return time.Now()
}

func (r *pacedArchiveRunner) offsetsFrom(origin time.Time) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	offsets := make([]time.Duration, 0, len(r.passes))
	for _, at := range r.passes {
		offsets = append(offsets, at.Sub(origin))
	}
	return offsets
}

func (r *pacedArchiveRunner) recorded() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.passes...)
}

// startPacedArchiveLoop runs the worker loop inside the current synctest
// bubble so the syncer's channels and the loop's timers are all virtual.
func startPacedArchiveLoop(t *testing.T, runner *pacedArchiveRunner) (*Syncer, func()) {
	t.Helper()
	syncer := NewSyncerWithRegistry(nil, nil, nil, nil, time.Hour, nil, nil)
	syncer.SetArchiveService(runner)
	syncer.SetArchivePollIntervalForTesting(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncer.runArchiveLoop(ctx, ready)
	}()
	return syncer, func() {
		cancel()
		<-done
	}
}

func TestArchiveLoopBacksOffWhileIdleAndResetsOnWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		start := time.Now()
		syncer, stop := startPacedArchiveLoop(t, runner)
		defer stop()

		// Idle passes double their spacing: 1s, 2s, 4s, 8s, ...
		time.Sleep(16 * time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{
			0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second,
		}, runner.offsetsFrom(start))

		// A wake runs a pass immediately and restarts the backoff from the
		// pacing interval.
		wakeAt := runner.reset()
		syncer.WakeArchive()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{0, time.Second, 3 * time.Second}, runner.offsetsFrom(wakeAt))
	})
}

func TestArchiveLoopKeepsPacingIntervalWhileWorkFlows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		runner.worked.Store(true)
		start := time.Now()
		_, stop := startPacedArchiveLoop(t, runner)
		defer stop()

		time.Sleep(5 * time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{
			0, time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second,
		}, runner.offsetsFrom(start))

		// Once work dries up the loop backs off again: 1s, then 2s.
		runner.worked.Store(false)
		last := runner.reset()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{time.Second, 3 * time.Second}, runner.offsetsFrom(last))
	})
}

func TestArchiveLoopIdleBackoffIsCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		_, stop := startPacedArchiveLoop(t, runner)
		defer stop()

		time.Sleep(time.Hour)
		synctest.Wait()
		passes := runner.recorded()
		require.GreaterOrEqual(len(passes), 3)
		last := passes[len(passes)-1]
		previous := passes[len(passes)-2]
		require.Equal(archiveIdleWait, last.Sub(previous))
	})
}

func TestArchiveLoopKeepsPacingIntervalWhilePassesFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		runner.fail.Store(true)
		start := time.Now()
		_, stop := startPacedArchiveLoop(t, runner)
		defer stop()

		// A failing store must keep retrying at the pacing interval so
		// recovery is prompt, not exponentially delayed.
		time.Sleep(4 * time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{
			0, time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second,
		}, runner.offsetsFrom(start))
	})
}

func TestArchiveLoopWakesWhenSyncRunCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		syncer, stop := startPacedArchiveLoop(t, runner)
		defer stop()

		// Let the idle backoff grow well past the pacing interval.
		time.Sleep(16 * time.Second)
		synctest.Wait()
		backedOff := runner.reset()

		// A completed sync run (here with no repositories) must wake the
		// worker immediately and restart the backoff from the pacing interval.
		syncer.RunOnce(context.Background())
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{0, time.Second}, runner.offsetsFrom(backedOff))
	})
}

func TestArchiveLoopWakesOnlyHostsThatDeniedArchiveWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		runner := &pacedArchiveRunner{}
		syncer, stop := startPacedArchiveLoop(t, runner)
		defer stop()
		const key = "github\x00github.test"

		time.Sleep(16 * time.Second)
		synctest.Wait()
		backedOff := runner.reset()

		// A normal stream of live work on a host that never turned archive
		// work away must not wake the worker, or every sync would trigger a
		// denied pass and a deferral write per release.
		release := syncer.beginProviderWork(key, archive.PriorityNormalIndex)
		release()
		synctest.Wait()
		require.Empty(runner.offsetsFrom(backedOff), "releasing a host that denied nothing must stay quiet")

		// Live work preempting an admitted archive request marks the host.
		// Only the release that frees the host wakes the worker, immediately.
		_, releaseArchive, ok := syncer.tryBeginArchiveProviderRequest(context.Background(), key)
		require.True(ok)
		started := make(chan struct{})
		var releaseFirst func()
		go func() {
			releaseFirst = syncer.beginProviderWork(key, archive.PriorityActiveDetail)
			close(started)
		}()
		synctest.Wait()
		releaseArchive()
		<-started
		releaseSecond := syncer.beginProviderWork(key, archive.PriorityNotificationRefresh)
		releaseFirst()
		synctest.Wait()
		require.Empty(runner.offsetsFrom(backedOff), "a host still busy must not wake the worker")
		releaseSecond()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal([]time.Duration{0, time.Second}, runner.offsetsFrom(backedOff))

		// The mark is consumed by the wake: the next quiet release stays quiet.
		quiet := runner.reset()
		release = syncer.beginProviderWork(key, archive.PriorityNormalIndex)
		release()
		synctest.Wait()
		require.Empty(runner.offsetsFrom(quiet))
	})
}
