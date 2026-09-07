package ratelimit

import (
	"sync"
	"testing"
	"time"

	platformpkg "go.kenn.io/forge/platform"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t)
}

func newGitHubRateTracker(d *db.DB, host string, apiType string) *RateTracker {
	return NewPlatformRateTracker(d, "github", host, "host", apiType)
}

func TestRateTrackerPrincipalIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	first := NewPlatformRateTracker(
		d, "github", "github.com", "user:123", "rest",
	)
	second := NewPlatformRateTracker(
		d, "github", "github.com", "user:456", "rest",
	)
	first.RecordRequest()
	second.RecordRequest()
	second.RecordRequest()

	firstRow, err := d.GetPlatformRateLimit(
		"github", "github.com", "user:123", "rest",
	)
	require.NoError(err)
	require.NotNil(firstRow)
	secondRow, err := d.GetPlatformRateLimit(
		"github", "github.com", "user:456", "rest",
	)
	require.NoError(err)
	require.NotNil(secondRow)
	assert.Equal(1, firstRow.RequestsHour)
	assert.Equal(2, secondRow.RequestsHour)
	assert.Equal("user:123", first.Principal())
	assert.NotEqual(first.BucketKey(), second.BucketKey())
}

func TestRateTrackerMissingResetStaysUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	host := "gitlab.example.com"
	rt := NewPlatformRateTracker(d, "gitlab", host, "host", "rest")

	// A provider response with an exhausted quota but no reset time must leave
	// the reset unknown (nil), not a non-nil zero timestamp.
	rt.UpdateFromRate(platformpkg.Rate{Limit: 600, Remaining: 0, Reset: time.Time{}})

	require.Nil(rt.ResetAt())

	// With reset unknown, an exhausted quota falls back to the documented 60s
	// backoff instead of treating a year-one reset as an expired window.
	backoff, wait := rt.ShouldBackoff()
	assert.True(backoff)
	assert.Equal(60*time.Second, wait)

	// The unknown reset is persisted as SQL NULL, so a reopened tracker still
	// reports nil rather than a fabricated timestamp.
	reopened := NewPlatformRateTracker(d, "gitlab", host, "host", "rest")
	assert.Nil(reopened.ResetAt())
}

func TestRateTrackerCounting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	rt := newGitHubRateTracker(d, "github.com", "rest")

	assert.Equal(0, rt.RequestsThisHour())
	assert.Equal(-1, rt.Remaining())

	rt.RecordRequest()
	rt.RecordRequest()
	rt.RecordRequest()

	assert.Equal(3, rt.RequestsThisHour())

	// Verify persisted to DB
	rl, err := d.GetRateLimit("github.com", "host", "rest")
	require.NoError(err)
	require.NotNil(rl)
	assert.Equal(3, rl.RequestsHour)
	assert.Equal(-1, rl.RateRemaining)
}

func TestRateTrackerScopesPersistenceByPlatform(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	host := "gitlab.example.com"
	ghRT := NewPlatformRateTracker(d, "github", host, "host", "rest")
	glRT := NewPlatformRateTracker(d, "gitlab", host, "host", "rest")

	ghRT.RecordRequest()
	glRT.RecordRequest()
	glRT.RecordRequest()

	ghRow, err := d.GetPlatformRateLimit("github", host, "host", "rest")
	require.NoError(err)
	require.NotNil(ghRow)
	assert.Equal("github", ghRow.Platform)
	assert.Equal(1, ghRow.RequestsHour)

	glRow, err := d.GetPlatformRateLimit("gitlab", host, "host", "rest")
	require.NoError(err)
	require.NotNil(glRow)
	assert.Equal("gitlab", glRow.Platform)
	assert.Equal(2, glRow.RequestsHour)

	reopenedGitLab := NewPlatformRateTracker(d, "gitlab", host, "host", "rest")
	assert.Equal(2, reopenedGitLab.RequestsThisHour())
	assert.Equal(RateBucketKey("gitlab", host, "host"), reopenedGitLab.BucketKey())
}

func TestRateTrackerBackoff(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)

	rt := newGitHubRateTracker(d, "github.com", "rest")

	// No backoff when remaining is -1 (unknown)
	backoff, wait := rt.ShouldBackoff()
	assert.False(backoff)
	assert.Zero(wait)

	// No backoff when remaining > 0
	futureReset := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Remaining: 100,
		Reset:     futureReset,
	})
	backoff, wait = rt.ShouldBackoff()
	assert.False(backoff)
	assert.Zero(wait)

	// Backoff when remaining == 0 with future reset
	rt.UpdateFromRate(platformpkg.Rate{
		Remaining: 0,
		Reset:     futureReset,
	})
	backoff, wait = rt.ShouldBackoff()
	assert.True(backoff)
	assert.Greater(wait, time.Duration(0))

	// Backoff with nil resetAt defaults to 60s
	rt.mu.Lock()
	rt.remaining = 0
	rt.resetAt = nil
	rt.mu.Unlock()
	backoff, wait = rt.ShouldBackoff()
	assert.True(backoff)
	assert.Equal(60*time.Second, wait)

	// No backoff when reset is in the past
	pastReset := time.Now().Add(-1 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Remaining: 0,
		Reset:     pastReset,
	})
	backoff, wait = rt.ShouldBackoff()
	assert.False(backoff)
	assert.Zero(wait)
}

func TestRateTrackerHourRollover(t *testing.T) {
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	for range 5 {
		rt.RecordRequest()
	}
	assert.Equal(t, 5, rt.RequestsThisHour())

	// Simulate hour rollover by manipulating internal state.
	rt.mu.Lock()
	rt.hourStart = time.Now().Add(-2 * time.Hour)
	rt.mu.Unlock()

	rt.RecordRequest()
	assert.Equal(t, 1, rt.RequestsThisHour(),
		"counter should reset after hour boundary")
}

func TestRateTrackerConcurrentAccess(t *testing.T) {
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			rt.RecordRequest()
			rt.RequestsThisHour()
			rt.Remaining()
			rt.ShouldBackoff()
		})
	}
	wg.Wait()
	assert.GreaterOrEqual(t, rt.RequestsThisHour(), 1)
}

func TestRateTrackerThrottleFactor(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	// Unknown state (limit=-1): no throttling
	assert.Equal(1, rt.ThrottleFactor())
	assert.False(rt.IsPaused())

	// >50% remaining: factor 1
	futureReset := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 3000,
		Reset:     futureReset,
	})
	assert.Equal(1, rt.ThrottleFactor())
	assert.False(rt.IsPaused())
	assert.True(rt.Known())
	assert.Equal(5000, rt.RateLimit())

	// 25-50% remaining: factor 2
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 2000,
		Reset:     futureReset,
	})
	assert.Equal(2, rt.ThrottleFactor())

	// 10-25% remaining: factor 4
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 1000,
		Reset:     futureReset,
	})
	assert.Equal(4, rt.ThrottleFactor())

	// <10% remaining: factor 8
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 400,
		Reset:     futureReset,
	})
	assert.Equal(8, rt.ThrottleFactor())

	// At reserve buffer (200): paused
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 200,
		Reset:     futureReset,
	})
	assert.True(rt.IsPaused())
	assert.Equal(8, rt.ThrottleFactor())
}

func TestRateTrackerStaleQuota(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	pastReset := time.Now().Add(-1 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 100,
		Reset:     pastReset,
	})

	assert.Equal(1, rt.ThrottleFactor())
	assert.False(rt.IsPaused())
}

func TestRateTrackerHydrateFromDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	rt1 := newGitHubRateTracker(d, "github.com", "rest")
	futureReset := time.Now().Add(30 * time.Minute)
	rt1.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 2000,
		Reset:     futureReset,
	})
	for range 10 {
		rt1.RecordRequest()
	}

	rt2 := newGitHubRateTracker(d, "github.com", "rest")

	assert.Equal(10, rt2.RequestsThisHour())
	assert.Equal(2000, rt2.Remaining())
	assert.Equal(5000, rt2.RateLimit())
	assert.True(rt2.Known())
	require.Equal(2, rt2.ThrottleFactor())
}

func TestRateTrackerWindowRolloverResetsQuota(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	futureReset := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 100,
		Reset:     futureReset,
	})
	assert.True(rt.Known())

	// Simulate window expiry by setting resetAt to the past
	rt.mu.Lock()
	pastReset := time.Now().Add(-1 * time.Minute)
	rt.resetAt = &pastReset
	rt.mu.Unlock()

	// Trigger rollIfNeeded via RequestsThisHour
	rt.RequestsThisHour()

	// Known stays true — the account rate limit (5000) does not
	// change between windows. Only remaining/count reset.
	assert.True(rt.Known())
	assert.Equal(1, rt.ThrottleFactor())
	assert.False(rt.IsPaused())
}

func TestRateTrackerWindowResetResetsCounter(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	// Use a future resetAt so requests accumulate normally.
	reset1 := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 4000,
		Reset:     reset1,
	})
	for range 50 {
		rt.RecordRequest()
	}
	assert.Equal(50, rt.RequestsThisHour())

	// Simulate window expiry: move resetAt to the past.
	rt.mu.Lock()
	pastReset := time.Now().Add(-1 * time.Second)
	rt.resetAt = &pastReset
	// Keep remaining at a known value so UpdateFromRate can
	// detect the jump. In production, remaining would still
	// hold its last value until rollIfNeeded fires.
	rt.remaining = 100
	rt.mu.Unlock()

	// GitHub window resets: remaining jumps up AND old resetAt
	// has passed — both conditions met.
	reset2 := time.Now().Add(1 * time.Hour)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 4999,
		Reset:     reset2,
	})

	assert.Equal(1, rt.RequestsThisHour())
	assert.Equal(4999, rt.Remaining())
}

func TestRateTrackerResetAtJitterDoesNotResetCounter(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	reset1 := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 4000,
		Reset:     reset1,
	})
	for range 50 {
		rt.RecordRequest()
	}
	assert.Equal(50, rt.RequestsThisHour())

	// resetAt jitters forward by 1s but remaining goes DOWN
	// (normal within-window behavior). Counter must NOT reset.
	reset2 := reset1.Add(1 * time.Second)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 3990,
		Reset:     reset2,
	})

	assert.Equal(50, rt.RequestsThisHour())
}

func TestRateTrackerSnapshotResetDoesNotRequireRemainingIncrease(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")
	var resetCallbacks int
	rt.SetOnWindowReset(func() {
		resetCallbacks++
	})

	oldReset := time.Now().Add(30 * time.Minute)
	rt.UpdateFromRate(platformpkg.Rate{
		Limit:     5000,
		Remaining: 4999,
		Reset:     oldReset,
	})
	for range 50 {
		rt.RecordRequest()
	}

	pastReset := time.Now().Add(-1 * time.Minute)
	rt.SetResetAtForTesting(pastReset)
	newReset := time.Now().Add(time.Hour)
	rt.UpdateFromSnapshot(platformpkg.Rate{
		Limit:     5000,
		Remaining: 4990,
		Reset:     newReset,
	})

	assert.Equal(0, rt.RequestsThisHour())
	assert.Equal(4990, rt.Remaining())
	assert.Equal(newReset.UTC(), *rt.ResetAt())
	assert.Equal(1, resetCallbacks)
}

// TestRateTrackerProductionFlow simulates the exact production
// pattern: every API call does RecordRequest + UpdateFromRate
// (via trackRate), then the counter is read via RequestsThisHour
// (via the /rate-limits endpoint). Verifies the counter survives
// repeated reads and window expiry.
func TestRateTrackerProductionFlow(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := newGitHubRateTracker(d, "github.com", "rest")

	futureReset := time.Now().Add(30 * time.Minute)

	// Simulate 5 API calls (the trackRate pattern)
	for i := range 5 {
		rt.RecordRequest()
		rt.UpdateFromRate(platformpkg.Rate{
			Limit:     5000,
			Remaining: 4999 - i,
			Reset:     futureReset,
		})
	}

	// Counter should be 5, and survive repeated reads
	assert.Equal(5, rt.RequestsThisHour())
	assert.Equal(5, rt.RequestsThisHour())
	assert.Equal(5, rt.RequestsThisHour())

	// Simulate window expiry: resetAt moves to past
	rt.mu.Lock()
	pastReset := time.Now().Add(-1 * time.Minute)
	rt.resetAt = &pastReset
	rt.mu.Unlock()

	// First read after expiry: rollIfNeeded fires, count resets
	assert.Equal(0, rt.RequestsThisHour())
	// Second read: roll must NOT fire again (lastRolledAt prevents re-roll)
	assert.Equal(0, rt.RequestsThisHour())

	// Now simulate new API calls in the new window
	newReset := time.Now().Add(55 * time.Minute)
	for i := range 3 {
		rt.RecordRequest()
		rt.UpdateFromRate(platformpkg.Rate{
			Limit:     5000,
			Remaining: 4999 - i,
			Reset:     newReset,
		})
	}

	// Counter should be 3
	assert.Equal(3, rt.RequestsThisHour())
	assert.Equal(3, rt.RequestsThisHour())
}

func TestRateTrackerAPITypeIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	restRT := newGitHubRateTracker(d, "github.com", "rest")
	gqlRT := newGitHubRateTracker(d, "github.com", "graphql")

	// Record requests on each independently
	for range 5 {
		restRT.RecordRequest()
	}
	for range 3 {
		gqlRT.RecordRequest()
	}

	assert.Equal(5, restRT.RequestsThisHour())
	assert.Equal(3, gqlRT.RequestsThisHour())

	// Verify DB isolation
	restRow, err := d.GetRateLimit("github.com", "host", "rest")
	require.NoError(err)
	require.NotNil(restRow)
	assert.Equal(5, restRow.RequestsHour)

	gqlRow, err := d.GetRateLimit("github.com", "host", "graphql")
	require.NoError(err)
	require.NotNil(gqlRow)
	assert.Equal(3, gqlRow.RequestsHour)

	// Hydrate new trackers — they pick up correct state
	restRT2 := newGitHubRateTracker(d, "github.com", "rest")
	gqlRT2 := newGitHubRateTracker(d, "github.com", "graphql")
	assert.Equal(5, restRT2.RequestsThisHour())
	assert.Equal(3, gqlRT2.RequestsThisHour())
}
