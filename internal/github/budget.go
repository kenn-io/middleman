package github

import (
	"math"
	"sync"
	"time"

	"go.kenn.io/forge/platform"
)

// PRDetailWorstCase is the maximum API calls a PR detail
// fetch can make (detail + GetUser + comments + reviews +
// commits + force-push events + review threads + combined status +
// check runs + one workflow-run read).
const PRDetailWorstCase = 10

// IssueDetailWorstCase is the maximum API calls an issue
// detail fetch can make (detail + comments).
const IssueDetailWorstCase = 2

const wireAttemptsPerRequest = 2

func detailWorstCaseAttemptCost(kind platform.Kind, itemType QueueItemType) int {
	logicalRequests := IssueDetailWorstCase
	if itemType == QueueItemPR {
		logicalRequests = PRDetailWorstCase
	} else if kind != platform.KindGitHub {
		logicalRequests++ // provider-native reference or timeline read
	}
	if kind != platform.KindGitHub {
		logicalRequests++ // repository metadata confirmation after a candidate feature error
	}
	return logicalRequests * wireAttemptsPerRequest
}

// SyncBudget tracks hourly API call spend for background
// detail fetches on a single host.
//
// The hourly window rolls on the budget's own clock, independent of provider
// quota. Provider window resets still call Reset, but the local ceiling must
// never depend on them: once the ceiling is exhausted its transport refuses
// counted requests before any wire attempt, so no provider response — and
// therefore no provider-driven reset — can arrive to release it.
type SyncBudget struct {
	mu    sync.Mutex
	limit int
	// reserve is the slice of limit held back for essential spend (the
	// list fetches that discover new and closed items). Optional
	// background work — detail refreshes, fast-sync, archive — refuses
	// beyond limit-reserve so it can never starve discovery within the
	// configured ceiling. Zero unless constructed with
	// NewSyncBudgetWithEssentialReserve.
	reserve      int
	spent        int
	archiveSpent int
	windowStart  time.Time
	window       BudgetWindow
	now          func() time.Time
}

// BudgetWindow identifies the hourly window a reservation was made in. Refunds
// carry it so a response that arrives after a rollover cannot credit spend back
// into the new window: that spend was already cleared by the roll, and
// returning it again would let the new window exceed its ceiling.
type BudgetWindow uint64

func NewSyncBudget(limit int) *SyncBudget {
	return &SyncBudget{
		limit:       limit,
		windowStart: time.Now().UTC(),
		now:         time.Now,
	}
}

// archiveProviderReserveDenominator sizes the slice of a provider quota
// window held back from archive hydration: limit/5. Live and essential sync
// always keep at least that headroom; archive availability is whatever
// remains above it.
const archiveProviderReserveDenominator = 5

// ArchiveProviderReserve returns the provider quota archive hydration must
// leave untouched. It never drops below the global rate reserve buffer, so
// small or unknown limits still keep the hard floor that pauses all
// background work.
func ArchiveProviderReserve(limit int) int {
	return max(limit/archiveProviderReserveDenominator, RateReserveBuffer)
}

// essentialReserveDenominator sizes the essential reserve as a fraction of
// the configured hourly limit: limit/10. With steady-state list fetches
// riding warm ETags (304s are refunded), a tenth of the ceiling comfortably
// covers the lists that actually changed plus transient in-flight spend.
const essentialReserveDenominator = 10

// NewSyncBudgetWithEssentialReserve returns a budget that holds back a
// slice of the hourly limit for essential spend (list discovery). Optional
// background work refuses beyond limit minus the reserve; TrySpendEssential
// may spend up to the full limit.
func NewSyncBudgetWithEssentialReserve(limit int) *SyncBudget {
	b := NewSyncBudget(limit)
	b.reserve = limit / essentialReserveDenominator
	return b
}

// TrySpendEssential is TrySpend for essential requests: it may consume the
// reserved headroom that optional spend cannot touch.
func (b *SyncBudget) TrySpendEssential(n int) (BudgetWindow, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if n < 0 || b.spent+n > b.limit {
		return 0, false
	}
	b.spent += n
	return b.window, true
}

// optionalLimitLocked is the ceiling for non-essential spend. Must be
// called with mu held.
func (b *SyncBudget) optionalLimitLocked() int {
	return b.limit - b.reserve
}

// rollLocked clears spend once the local hourly window has elapsed.
// Must be called with mu held.
func (b *SyncBudget) rollLocked() {
	now := b.now().UTC()
	if now.Sub(b.windowStart) < time.Hour {
		return
	}
	b.spent = 0
	b.archiveSpent = 0
	b.windowStart = now
	b.window++
}

func (b *SyncBudget) CanSpend(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.spent+n <= b.optionalLimitLocked()
}

// TrySpend atomically checks and increments the budget. It reports whether the
// spend succeeded and, when it did, the window the reservation belongs to. Pass
// that window to Refund so a late refund cannot cross a rollover.
func (b *SyncBudget) TrySpend(n int) (BudgetWindow, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if n < 0 || b.spent+n > b.optionalLimitLocked() {
		return 0, false
	}
	b.spent += n
	return b.window, true
}

// trySpendForTransport reserves one wire attempt and returns the reset of the
// window examined by that reservation. Keeping the reset snapshot under the
// same lock as the refusal prevents a rollover between those two observations
// from assigning an exhausted request to the next window.
func (b *SyncBudget) trySpendForTransport(
	n int,
	archive bool,
	essential bool,
) (BudgetWindow, bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	resetAt := b.windowStart.Add(time.Hour)
	limit := b.optionalLimitLocked()
	if essential {
		limit = b.limit
	}
	if n < 0 || b.spent+n > limit {
		return 0, false, resetAt
	}
	b.spent += n
	if archive {
		b.archiveSpent += n
	}
	return b.window, true, resetAt
}

func (b *SyncBudget) Spend(n int) {
	b.TrySpend(n)
}

// Refund returns n calls back to the budget. A refund for an elapsed window is
// dropped: the roll already cleared that spend.
func (b *SyncBudget) Refund(window BudgetWindow, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if window != b.window {
		return
	}
	b.spent = max(b.spent-n, 0)
}

func (b *SyncBudget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent = 0
	b.archiveSpent = 0
	b.windowStart = b.now().UTC()
	b.window++
}

func (b *SyncBudget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return max(b.limit-b.spent, 0)
}

func (b *SyncBudget) Spent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.spent
}

func (b *SyncBudget) Limit() int {
	return b.limit
}

// BackgroundLimit returns the maximum total spend optional background work
// may consume in the current local window. Capacity above this boundary stays
// available to essential discovery.
func (b *SyncBudget) BackgroundLimit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.optionalLimitLocked()
}

// ResetAt returns the end of the budget's current local hourly window.
func (b *SyncBudget) ResetAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.windowStart.Add(time.Hour)
}

func (b *SyncBudget) ArchiveSpendCeiling(now time.Time, resetAt *time.Time, liveFloor int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.archiveSpendCeiling(now, resetAt, liveFloor)
}

func (b *SyncBudget) CanSpendArchive(n int, now time.Time, resetAt *time.Time, liveFloor int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return n > 0 && n <= b.archiveSpendAvailable(now, resetAt, liveFloor)
}

func (b *SyncBudget) ArchiveSpendAvailable(now time.Time, resetAt *time.Time, liveFloor int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.archiveSpendAvailable(now, resetAt, liveFloor)
}

// LocalArchiveSpendAvailable returns the unspent configured hourly budget
// above the live-work floor. It is used only by providers whose responses do
// not expose a usable quota window; it does not create provider quota state.
func (b *SyncBudget) LocalArchiveSpendAvailable(liveFloor int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.optionalLimitLocked()-max(liveFloor, 0)-b.spent, 0)
}

func (b *SyncBudget) SpendArchive(n int) {
	b.TrySpendArchive(n)
}

func (b *SyncBudget) TrySpendArchive(n int) (BudgetWindow, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if n < 0 || b.spent+n > b.optionalLimitLocked() {
		return 0, false
	}
	b.spent += n
	b.archiveSpent += n
	return b.window, true
}

func (b *SyncBudget) RefundArchive(window BudgetWindow, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if window != b.window {
		return
	}
	b.spent = max(b.spent-n, 0)
	b.archiveSpent = max(b.archiveSpent-n, 0)
}

func (b *SyncBudget) ArchiveSpent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.archiveSpent
}

func (b *SyncBudget) archiveSpendCeiling(now time.Time, resetAt *time.Time, liveFloor int) int {
	if resetAt == nil || liveFloor >= b.optionalLimitLocked() {
		return 0
	}
	remaining := resetAt.Sub(now)
	if remaining < 0 || remaining > time.Hour {
		return 0
	}
	elapsedFraction := 1 - float64(remaining)/float64(time.Hour)
	surplus := b.optionalLimitLocked() - max(liveFloor, 0)
	return int(math.Floor(float64(surplus) * elapsedFraction * elapsedFraction))
}

func (b *SyncBudget) archiveSpendAvailable(now time.Time, resetAt *time.Time, liveFloor int) int {
	ceilingRemaining := b.archiveSpendCeiling(now, resetAt, liveFloor) - b.archiveSpent
	liveRemaining := b.optionalLimitLocked() - max(liveFloor, 0) - b.spent
	return max(min(ceilingRemaining, liveRemaining), 0)
}
