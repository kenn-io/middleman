package github

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/forge/platform"
)

func TestSyncBudgetBasics(t *testing.T) {
	assert := assert.New(t)
	b := NewSyncBudget(100)

	assert.Equal(100, b.Limit())
	assert.Equal(0, b.Spent())
	assert.Equal(100, b.Remaining())
	assert.True(b.CanSpend(6))

	b.Spend(50)
	assert.Equal(50, b.Spent())
	assert.Equal(50, b.Remaining())
	assert.True(b.CanSpend(6))
	assert.False(b.CanSpend(51))

	b.Reset()
	assert.Equal(0, b.Spent())
	assert.Equal(100, b.Remaining())
}

func TestSyncBudgetWorstCase(t *testing.T) {
	b := NewSyncBudget(10)
	b.Spend(5)
	assert.Equal(t, 10, PRDetailWorstCase)
	assert.False(t, b.CanSpend(PRDetailWorstCase))   // 10 > 5 remaining
	assert.True(t, b.CanSpend(IssueDetailWorstCase)) // 2 <= 5 remaining
}

func TestSyncBudgetEssentialReserve(t *testing.T) {
	assert := assert.New(t)

	// A tenth of the limit is held back for essential spend.
	b := NewSyncBudgetWithEssentialReserve(100)
	_, ok := b.TrySpend(90)
	assert.True(ok, "optional spend may use the limit minus the reserve")
	_, ok = b.TrySpend(1)
	assert.False(ok, "optional spend must stop at the essential reserve")

	_, ok = b.TrySpendEssential(10)
	assert.True(ok, "essential spend may consume the reserved headroom")
	_, ok = b.TrySpendEssential(1)
	assert.False(ok, "essential spend must stop at the full limit")
}

func TestSyncBudgetEssentialReserveBoundsArchiveSpend(t *testing.T) {
	assert := assert.New(t)

	b := NewSyncBudgetWithEssentialReserve(100)
	_, ok := b.TrySpendArchive(91)
	assert.False(ok, "archive spend must not consume the essential reserve")
	_, ok = b.TrySpendArchive(90)
	assert.True(ok)
}

func TestSyncBudgetDefaultConstructorHasNoReserve(t *testing.T) {
	assert := assert.New(t)

	b := NewSyncBudget(100)
	_, ok := b.TrySpend(100)
	assert.True(ok, "budgets without an essential reserve keep full-limit spend")
}

func TestArchiveLiveFloorReservesWorstCaseWireAttempts(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(24, archiveLiveFloor(platform.KindGitHub))
	assert.Equal(24, archiveLiveFloor(platform.KindGitLab))
	assert.Equal(22, archiveLiveFloor(platform.KindGitea))
	assert.Equal(22, archiveLiveFloor(platform.KindForgejo))
}

func TestLocalArchiveSpendAvailablePreservesLiveFloor(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(50)

	assert.Equal(28, budget.LocalArchiveSpendAvailable(22))
	budget.SpendArchive(27)
	assert.Equal(1, budget.LocalArchiveSpendAvailable(22))
	budget.SpendArchive(1)
	assert.Zero(budget.LocalArchiveSpendAvailable(22))
	assert.Equal(22, budget.Remaining())
}

func TestSyncBudgetArchiveAdmissionRampsTowardReset(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(100)
	reset := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	liveFloor := 20

	tests := []struct {
		name        string
		now         time.Time
		wantCeiling int
		wantSpend   int
	}{
		{name: "start", now: reset.Add(-time.Hour), wantCeiling: 0},
		{name: "half", now: reset.Add(-30 * time.Minute), wantCeiling: 20, wantSpend: 20},
		{name: "three quarters", now: reset.Add(-15 * time.Minute), wantCeiling: 45, wantSpend: 25},
		{name: "reset", now: reset, wantCeiling: 80, wantSpend: 35},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(test.wantCeiling, budget.ArchiveSpendCeiling(test.now, &reset, liveFloor))
			if test.wantSpend == 0 {
				assert.False(budget.CanSpendArchive(1, test.now, &reset, liveFloor))
				return
			}
			assert.True(budget.CanSpendArchive(test.wantSpend, test.now, &reset, liveFloor))
			budget.SpendArchive(test.wantSpend)
		})
	}
	assert.Equal(80, budget.ArchiveSpent())
	assert.Equal(20, budget.Remaining())
}

func TestSyncBudgetArchiveAdmissionPreservesLiveFloor(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(50)
	reset := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)

	assert.False(budget.CanSpendArchive(1, reset, &reset, 50))
	assert.False(budget.CanSpendArchive(1, reset, &reset, 60))
	assert.True(budget.CanSpendArchive(37, reset, &reset, 13))
	budget.SpendArchive(37)
	assert.Equal(13, budget.Remaining())
	assert.False(budget.CanSpendArchive(1, reset, &reset, 13))
}

func TestSyncBudgetArchiveAdmissionRequiresPlausibleReset(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(100)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	tooFar := now.Add(time.Hour + time.Second)

	assert.Zero(budget.ArchiveSpendCeiling(now, nil, 20))
	assert.Zero(budget.ArchiveSpendCeiling(now, &past, 20))
	assert.Zero(budget.ArchiveSpendCeiling(now, &tooFar, 20))
	assert.False(budget.CanSpendArchive(1, now, nil, 20))
}

func TestSyncBudgetResetClearsArchiveSpend(t *testing.T) {
	budget := NewSyncBudget(100)
	budget.SpendArchive(12)
	budget.Reset()

	assert.Zero(t, budget.Spent())
	assert.Zero(t, budget.ArchiveSpent())
}

func TestSyncBudgetRollsWindowWithoutProviderResponse(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(2)
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock
	spend := func(n int) bool {
		_, ok := budget.TrySpend(n)
		return ok
	}

	assert.True(spend(2))
	assert.False(spend(1))
	assert.Zero(budget.Remaining())

	// Still inside the window: the ceiling must hold.
	clock = clock.Add(59 * time.Minute)
	assert.False(spend(1))
	assert.Zero(budget.Remaining())

	// The window elapsed. Nothing observed a provider response in between, so
	// only the budget's own clock can release the ceiling.
	clock = clock.Add(time.Minute)
	assert.Equal(2, budget.Remaining())
	assert.True(spend(1))
	assert.Equal(1, budget.Spent())
}

func TestSyncBudgetReportsOwnResetAt(t *testing.T) {
	budget := NewSyncBudget(2)
	clock := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock

	assert.Equal(t, clock.Add(time.Hour), budget.ResetAt())

	clock = clock.Add(time.Hour)
	assert.Equal(t, clock.Add(time.Hour), budget.ResetAt(),
		"reading the reset time must roll an elapsed local window")
}

func TestSyncBudgetRollsArchiveSpendWithWindow(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(4)
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock
	spendArchive := func(n int) bool {
		_, ok := budget.TrySpendArchive(n)
		return ok
	}

	assert.True(spendArchive(4))
	assert.Equal(4, budget.ArchiveSpent())
	assert.False(spendArchive(1))

	clock = clock.Add(time.Hour)
	assert.Zero(budget.ArchiveSpent())
	assert.Zero(budget.Spent())
	assert.True(spendArchive(1))
}

// A reservation made before a rollover must not be credited back into the new
// window: the roll already cleared it, so refunding again would let the new
// window spend past its ceiling.
func TestSyncBudgetDropsRefundFromElapsedWindow(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(2)
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock

	stale, ok := budget.TrySpend(1)
	assert.True(ok)

	clock = clock.Add(time.Hour)
	// Fill the new window to its ceiling.
	_, ok = budget.TrySpend(2)
	assert.True(ok)
	assert.Equal(2, budget.Spent())

	// The old window's 304 lands now. It must be ignored.
	budget.Refund(stale, 1)
	assert.Equal(2, budget.Spent())
	_, ok = budget.TrySpend(1)
	assert.False(ok)

	// A refund from the current window still applies.
	current, ok := budget.TrySpend(0)
	assert.True(ok)
	budget.Refund(current, 1)
	assert.Equal(1, budget.Spent())
}

func TestSyncBudgetDropsArchiveRefundFromElapsedWindow(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(4)
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock

	stale, ok := budget.TrySpendArchive(1)
	assert.True(ok)

	clock = clock.Add(time.Hour)
	_, ok = budget.TrySpendArchive(2)
	assert.True(ok)

	budget.RefundArchive(stale, 1)
	assert.Equal(2, budget.ArchiveSpent())
	assert.Equal(2, budget.Spent())
}

// Reset starts a fresh window too, so an in-flight reservation from before a
// provider-driven reset cannot credit the cleared window either.
func TestSyncBudgetResetDropsInFlightRefund(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(2)

	stale, ok := budget.TrySpend(1)
	assert.True(ok)
	budget.Reset()
	_, ok = budget.TrySpend(2)
	assert.True(ok)

	budget.Refund(stale, 1)
	assert.Equal(2, budget.Spent())
}

// The archive reserve holds back a fifth of the provider limit for live and
// essential work, never dropping below the global rate reserve buffer.
func TestArchiveProviderReserveHoldsBackFifthOfLimit(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(1000, ArchiveProviderReserve(5000))
	assert.Equal(300, ArchiveProviderReserve(1500))
	assert.Equal(RateReserveBuffer, ArchiveProviderReserve(500))
	assert.Equal(RateReserveBuffer, ArchiveProviderReserve(0))
	assert.Equal(RateReserveBuffer, ArchiveProviderReserve(-100))
}
