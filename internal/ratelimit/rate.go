package ratelimit

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	platformpkg "go.kenn.io/forge/platform"

	"go.kenn.io/forge/internal/db"
)

const RateReserveBuffer = 200

type throttleStep struct {
	floor  float64
	factor int
}

var throttleSteps = []throttleStep{
	{0.50, 1},
	{0.25, 2},
	{0.10, 4},
	{0.00, 8},
}

// RateTracker records per-provider/host API request counts and rate limit
// state, persisting to SQLite for cross-restart visibility.
type RateTracker struct {
	mu            sync.Mutex
	db            *db.DB
	platform      string
	platformHost  string
	ratePrincipal string
	apiType       string
	count         int
	hourStart     time.Time
	remaining     int
	limit         int
	resetAt       *time.Time
	lastRolledAt  time.Time // prevents repeated rolls
	onWindowReset func()
}

// NewPlatformRateTracker creates a tracker for the given provider, host, and API type.
// It hydrates from DB if a row exists for the current hour.
func NewPlatformRateTracker(
	database *db.DB,
	platformName, platformHost, ratePrincipal, apiType string,
) *RateTracker {
	platformName = normalizedRatePlatform(platformName)
	rt := &RateTracker{
		db:            database,
		platform:      platformName,
		platformHost:  platformHost,
		ratePrincipal: strings.TrimSpace(ratePrincipal),
		apiType:       apiType,
		remaining:     -1,
		limit:         -1,
		hourStart:     truncateHour(time.Now().UTC()),
	}
	rt.hydrate()
	return rt
}

func (rt *RateTracker) hydrate() {
	row, err := rt.db.GetPlatformRateLimit(
		rt.platform, rt.platformHost, rt.ratePrincipal, rt.apiType,
	)
	if err != nil || row == nil {
		return
	}
	if row.RateResetAt != nil {
		if !time.Now().Before(*row.RateResetAt) {
			return // provider window expired
		}
	} else {
		now := truncateHour(time.Now().UTC())
		if row.HourStart.Before(now) {
			return // clock-hour expired, no provider data
		}
	}
	rt.count = row.RequestsHour
	rt.hourStart = row.HourStart
	rt.remaining = row.RateRemaining
	rt.limit = row.RateLimit
	rt.resetAt = row.RateResetAt
}

// Provider returns the provider name this tracker is scoped to.
func (rt *RateTracker) Provider() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.platform
}

// PlatformHost returns the provider host this tracker is scoped to.
func (rt *RateTracker) PlatformHost() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.platformHost
}

// Principal returns the identity whose capacity this tracker represents.
func (rt *RateTracker) Principal() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.ratePrincipal
}

// APIType returns the API bucket type this tracker is scoped to.
func (rt *RateTracker) APIType() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.apiType
}

// BucketKey returns the process-local map key for this provider/host bucket.
func (rt *RateTracker) BucketKey() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return RateBucketKey(rt.platform, rt.platformHost, rt.ratePrincipal)
}

// RecordRequest increments the hourly request counter and
// persists to DB.
func (rt *RateTracker) RecordRequest() {
	rt.mu.Lock()
	resetFn := rt.rollIfNeeded()
	rt.count++
	rt.persist()
	rt.mu.Unlock()

	if resetFn != nil {
		resetFn()
	}
}

// SetOnWindowReset registers a callback invoked when a provider
// rate limit window reset is detected. The callback runs with
// the tracker's mutex released.
func (rt *RateTracker) SetOnWindowReset(fn func()) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.onWindowReset = fn
}

// UpdateFromRate updates remaining/limit/reset from provider response data. If
// the reset time moved forward, the provider started a new rate window and the
// request counter resets to stay aligned with that window.
func (rt *RateTracker) UpdateFromRate(rate platformpkg.Rate) {
	rt.updateFromRate(rate, 1, false)
}

// UpdateFromSnapshot updates remaining/limit/reset from provider snapshot data
// that does not itself represent a counted API request.
func (rt *RateTracker) UpdateFromSnapshot(rate platformpkg.Rate) {
	rt.updateFromRate(rate, 0, true)
}

func (rt *RateTracker) updateFromRate(
	rate platformpkg.Rate,
	resetWindowCount int,
	resetExpiredWindow bool,
) {
	rt.mu.Lock()
	now := time.Now().UTC()
	// A zero reset means the provider response carried no reset time. Store it
	// as nil (unknown), never as a non-nil zero timestamp: the tracker's
	// nil-for-unknown contract is what makes ShouldBackoff fall back to its
	// documented delay and keeps ArchiveSpendCeiling at zero. A non-nil year-one
	// reset would instead look like an already-expired window and release
	// archive surplus that no provider ever justified.
	var resetPtr *time.Time
	if !rate.Reset.IsZero() {
		resetTime := rate.Reset.UTC()
		resetPtr = &resetTime
	}
	// Detect provider window reset: remaining increases AND the
	// previous resetAt has passed (proving the old window ended).
	// Both conditions are required to avoid false resets from
	// out-of-order responses within the same window.
	var windowReset bool
	expiredWindow := rt.resetAt != nil && !now.Before(*rt.resetAt)
	if expiredWindow && (resetExpiredWindow ||
		rt.remaining >= 0 && rate.Remaining > rt.remaining) {
		rt.count = resetWindowCount
		rt.hourStart = now
		windowReset = true
	}
	fn := rt.onWindowReset
	rt.remaining = rate.Remaining
	rt.limit = rate.Limit
	rt.resetAt = resetPtr
	rt.persist()
	rt.mu.Unlock()

	if windowReset && fn != nil {
		fn()
	}
}

// ShouldBackoff returns true and the wait duration if the rate
// limit is exhausted (remaining==0). If resetAt is nil, defaults
// to 60s. Returns false if remaining is >0 or unknown (-1).
func (rt *RateTracker) ShouldBackoff() (bool, time.Duration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.remaining != 0 {
		return false, 0
	}
	if rt.resetAt == nil {
		return true, 60 * time.Second
	}
	wait := time.Until(*rt.resetAt)
	if wait <= 0 {
		return false, 0
	}
	return true, wait
}

// ThrottleFactor returns a multiplier (1, 2, 4, or 8) based on
// how much remaining quota is left relative to the limit.
func (rt *RateTracker) ThrottleFactor() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.isQuotaStale() {
		return 1
	}
	pct := float64(rt.remaining) / float64(rt.limit)
	for _, s := range throttleSteps {
		if pct > s.floor {
			return s.factor
		}
	}
	return throttleSteps[len(throttleSteps)-1].factor
}

// IsPaused returns true when remaining quota is at or below
// the reserve buffer and quota info is fresh.
func (rt *RateTracker) IsPaused() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.isQuotaStale() {
		return false
	}
	return rt.remaining <= RateReserveBuffer
}

// Remaining returns the last known remaining request count.
func (rt *RateTracker) Remaining() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.remaining
}

// RateLimit returns the last known rate limit.
func (rt *RateTracker) RateLimit() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.limit
}

// Known returns true if we have received at least one rate
// limit response with a positive limit value.
func (rt *RateTracker) Known() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.limit > 0
}

// RequestsThisHour returns the number of requests recorded in
// the current hour window.
func (rt *RateTracker) RequestsThisHour() int {
	rt.mu.Lock()
	resetFn := rt.rollIfNeeded()
	count := rt.count
	rt.mu.Unlock()

	if resetFn != nil {
		resetFn()
	}
	return count
}

// ResetAt returns a copy of the reset time, or nil if unknown.
func (rt *RateTracker) ResetAt() *time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.resetAt == nil {
		return nil
	}
	t := *rt.resetAt
	return &t
}

// SetResetAtForTesting overrides the reset time for tests that need to exercise
// window rollover behavior without sleeping.
func (rt *RateTracker) SetResetAtForTesting(resetAt time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.resetAt = &resetAt
}

// HourStart returns the start of the current tracking hour.
func (rt *RateTracker) HourStart() time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.hourStart
}

// isQuotaStale returns true when quota data is unknown or
// expired. Must be called with mu held.
func (rt *RateTracker) isQuotaStale() bool {
	if rt.remaining < 0 || rt.limit <= 0 {
		return true
	}
	if rt.resetAt != nil && !time.Now().Before(*rt.resetAt) {
		return true
	}
	return false
}

// rollIfNeeded resets the counter and quota state when the
// rate window has expired. When the provider's resetAt is known, it
// defines the window; otherwise falls back to clock-hour
// boundaries. Must be called with mu held.
func (rt *RateTracker) rollIfNeeded() func() {
	if rt.resetAt != nil {
		if !time.Now().Before(*rt.resetAt) && !rt.lastRolledAt.Equal(*rt.resetAt) {
			rt.lastRolledAt = *rt.resetAt
			rt.count = 0
			rt.hourStart = time.Now().UTC()
			rt.remaining = -1
			// Keep rt.limit — the account rate cap (e.g. 5000)
			// does not change between windows.
			rt.resetAt = nil
			rt.persist()
			return rt.onWindowReset
		}
		return nil
	}
	now := truncateHour(time.Now().UTC())
	if now.After(rt.hourStart) {
		rt.count = 0
		rt.hourStart = now
		rt.remaining = -1
		// Keep rt.limit — same reasoning as above.
		rt.persist()
		return rt.onWindowReset
	}
	return nil
}

// persist writes current state to DB. Must be called with mu held.
func (rt *RateTracker) persist() {
	err := rt.db.UpsertPlatformRateLimit(
		rt.platform,
		rt.platformHost,
		rt.ratePrincipal,
		rt.apiType,
		rt.count,
		rt.hourStart,
		rt.remaining,
		rt.limit,
		rt.resetAt,
	)
	if err != nil {
		slog.Warn("persist rate limit failed",
			"host", rt.platformHost, "err", err,
		)
	}
}

// truncateHour returns t truncated to the start of its hour.
func truncateHour(t time.Time) time.Time {
	return t.Truncate(time.Hour)
}

func normalizedRatePlatform(platformName string) string {
	platformName = strings.ToLower(strings.TrimSpace(platformName))
	if platformName == "" {
		return "github"
	}
	return platformName
}

// RateBucketKey returns the process-local key for a provider identity bucket.
func RateBucketKey(platformName, platformHost, ratePrincipal string) string {
	platformName = normalizedRatePlatform(platformName)
	platformHost = strings.ToLower(strings.TrimSpace(platformHost))
	ratePrincipal = strings.TrimSpace(ratePrincipal)
	if ratePrincipal == "host" {
		if platformName == "github" {
			return platformHost
		}
		return platformName + ":" + platformHost
	}
	return strings.Join([]string{
		platformName, platformHost, ratePrincipal,
	}, "\x00")
}
