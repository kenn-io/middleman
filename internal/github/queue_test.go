package github

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/forge/platform"
)

// Fixed reference time for deterministic tests.
var testNow = time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)

func TestQueueItemWorstCaseCost(t *testing.T) {
	assert := assert.New(t)
	tests := []struct {
		name string
		item QueueItem
		want int
	}{
		{name: "GitHub pull request", item: QueueItem{Type: QueueItemPR, Platform: platform.KindGitHub}, want: 20},
		{name: "GitLab pull request", item: QueueItem{Type: QueueItemPR, Platform: platform.KindGitLab}, want: 22},
		{name: "GitHub issue", item: QueueItem{Type: QueueItemIssue, Platform: platform.KindGitHub}, want: 4},
		{name: "Gitea issue", item: QueueItem{Type: QueueItemIssue, Platform: platform.KindGitea}, want: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(test.want, test.item.WorstCaseCost())
		})
	}
}

func TestBuildQueueNeverFetchedOpenPR(t *testing.T) {
	assert := assert.New(t)

	items := []QueueItem{{
		Type:      QueueItemPR,
		Number:    1,
		IsOpen:    true,
		UpdatedAt: testNow.Add(-1 * time.Hour),
		// DetailFetchedAt nil => never fetched
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
	// +500 (never fetched) +50 (open) +1000/(1+1)=500
	// = 1050. Spec says ~1500+, recency with 1h = 500.
	assert.Greater(q[0].Score, 1000.0)
}

func TestBuildQueueStarredUpdatedPR(t *testing.T) {
	assert := assert.New(t)

	fetched := testNow.Add(-20 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          2,
		IsOpen:          true,
		Starred:         true,
		UpdatedAt:       testNow.Add(-5 * time.Minute),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
	// +1000 (updated) +200 (starred) +50 (open)
	// +1000/(1+0.083)=~923 => ~2173
	assert.Greater(q[0].Score, 2100.0)
}

func TestBuildQueueRecentlyFetchedUnchangedExcluded(t *testing.T) {
	assert := assert.New(t)

	// Fetched 10min ago, updated_at before fetch, not
	// starred/watched, no pending CI, open.
	fetched := testNow.Add(-10 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          3,
		IsOpen:          true,
		UpdatedAt:       testNow.Add(-1 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Empty(q)
}

func TestBuildQueueStarredStalenessEligible(t *testing.T) {
	assert := assert.New(t)

	// Starred, fetched 20min ago (>15min threshold),
	// updated_at before fetch.
	fetched := testNow.Add(-20 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          4,
		IsOpen:          true,
		Starred:         true,
		UpdatedAt:       testNow.Add(-2 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
}

func TestBuildQueueClosedOldItemLowScore(t *testing.T) {
	assert := assert.New(t)

	sixMonthsAgo := testNow.Add(-180 * 24 * time.Hour)
	fetched := testNow.Add(-25 * time.Hour)
	items := []QueueItem{{
		Type:            QueueItemIssue,
		Number:          5,
		IsOpen:          false,
		UpdatedAt:       sixMonthsAgo,
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
	// No starred/watched/open/CI/never-fetched/updated
	// bonuses. Only recency: 1000/(1+4320) ~ 0.23.
	assert.Less(q[0].Score, 5.0)
}

func TestBuildQueueSortedByScoreDescending(t *testing.T) {
	assert := assert.New(t)

	fetched := testNow.Add(-1 * time.Hour)
	items := []QueueItem{
		{
			// Low score: closed, old, no bonuses.
			Type:            QueueItemIssue,
			Number:          10,
			IsOpen:          false,
			UpdatedAt:       testNow.Add(-90 * 24 * time.Hour),
			DetailFetchedAt: new(testNow.Add(-25 * time.Hour)),
		},
		{
			// High score: open, starred, updated.
			Type:            QueueItemPR,
			Number:          20,
			IsOpen:          true,
			Starred:         true,
			UpdatedAt:       testNow.Add(-10 * time.Minute),
			DetailFetchedAt: &fetched,
		},
		{
			// Mid score: never fetched, open.
			Type:      QueueItemPR,
			Number:    30,
			IsOpen:    true,
			UpdatedAt: testNow.Add(-3 * time.Hour),
		},
	}

	q := BuildQueue(items, testNow)
	assert.Len(q, 3)
	assert.Equal(20, q[0].Number) // highest
	assert.Equal(30, q[1].Number) // middle
	assert.Equal(10, q[2].Number) // lowest

	for i := 0; i < len(q)-1; i++ {
		assert.Greater(q[i].Score, q[i+1].Score)
	}
}

func TestBuildQueueCIHadPendingMakesEligible(t *testing.T) {
	assert := assert.New(t)

	// Fetched 10min ago, unchanged — normally ineligible.
	// But CIHadPending overrides.
	fetched := testNow.Add(-10 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          6,
		IsOpen:          true,
		CIHadPending:    true,
		UpdatedAt:       testNow.Add(-1 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
	// Verify CI bonus in score.
	assert.Greater(q[0].Score, 100.0)
}

func TestBuildQueueCIHadPendingBypassesLargeRepoGate(t *testing.T) {
	assert := assert.New(t)

	fetched := testNow.Add(-10 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          9,
		IsOpen:          true,
		CIHadPending:    true,
		LargeRepo:       true,
		UpdatedAt:       testNow.Add(-1 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
	assert.Equal(9, q[0].Number)
}

func TestBuildQueueClosedRecentlyFetchedExcluded(t *testing.T) {
	assert := assert.New(t)

	// Closed item fetched 12h ago (<24h) — should be
	// excluded.
	fetched := testNow.Add(-12 * time.Hour)
	items := []QueueItem{{
		Type:            QueueItemIssue,
		Number:          7,
		IsOpen:          false,
		UpdatedAt:       testNow.Add(-48 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Empty(q)
}

func TestBuildQueueWatchedStalenessEligible(t *testing.T) {
	assert := assert.New(t)

	fetched := testNow.Add(-20 * time.Minute)
	items := []QueueItem{{
		Type:            QueueItemPR,
		Number:          8,
		IsOpen:          true,
		Watched:         true,
		UpdatedAt:       testNow.Add(-2 * time.Hour),
		DetailFetchedAt: &fetched,
	}}

	q := BuildQueue(items, testNow)
	assert.Len(q, 1)
}

func TestBuildQueueEmptyInput(t *testing.T) {
	q := BuildQueue(nil, testNow)
	assert.Empty(t, q)
}
