package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestBudgetTransport_CountsSyncContext(t *testing.T) {
	assert := assert.New(t)

	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return httptest.NewRecorder().Result(), nil
		}),
		budget: budget,
	}

	ctx := WithSyncBudget(t.Context())
	req, _ := http.NewRequestWithContext(
		ctx, "GET", "https://api.github.com/repos/o/n/pulls", nil,
	)
	_, err := bt.RoundTrip(req)
	require.NoError(t, err)

	assert.Equal(1, budget.Spent())
	assert.Zero(budget.ArchiveSpent())
}

func TestBudgetTransportReservesBeforeConcurrentProviderIO(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	budget := NewSyncBudget(1)
	var providerCalls atomic.Int32
	transport := WrapSyncBudgetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}), budget)

	const attempts = 20
	start := make(chan struct{})
	errors := make(chan error, attempts)
	var workers sync.WaitGroup
	for range attempts {
		workers.Go(func() {
			<-start
			req, err := http.NewRequestWithContext(
				WithSyncBudget(t.Context()), http.MethodGet,
				"https://api.github.com/repos/acme/widget", nil,
			)
			if err != nil {
				errors <- err
				return
			}
			_, err = transport.RoundTrip(req)
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	close(errors)

	exhausted := 0
	for err := range errors {
		if err != nil {
			require.ErrorIs(err, platform.ErrSyncBudgetExhausted)
			exhausted++
		}
	}
	assert.Equal(attempts-1, exhausted)
	assert.Equal(int32(1), providerCalls.Load())
	assert.Equal(1, budget.Spent())
	assert.Zero(budget.Remaining())
}

func TestBudgetTransportArchiveReservationUpdatesBothCountersAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	budget := NewSyncBudget(1)
	transport := WrapSyncBudgetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}), budget)
	req, err := http.NewRequestWithContext(
		WithArchiveSyncBudget(t.Context()), http.MethodGet,
		"https://api.github.com/repos/acme/widget/issues/1", nil,
	)
	require.NoError(err)
	_, err = transport.RoundTrip(req)
	require.NoError(err)

	assert.Equal(1, budget.Spent())
	assert.Equal(1, budget.ArchiveSpent())
	assert.Zero(budget.Remaining())
}

func TestBudgetTransport_CountsArchiveContextSeparately(t *testing.T) {
	assert := assert.New(t)
	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return httptest.NewRecorder().Result(), nil
		}),
		budget: budget,
	}

	req, err := http.NewRequestWithContext(
		WithArchiveSyncBudget(t.Context()),
		http.MethodGet,
		"https://api.github.com/repos/o/n/issues/1/comments",
		nil,
	)
	require.NoError(t, err)
	_, err = bt.RoundTrip(req)
	require.NoError(t, err)

	assert.Equal(1, budget.Spent())
	assert.Equal(1, budget.ArchiveSpent())
	assert.True(IsArchiveSyncBudgetContext(req.Context()))
}

func TestBudgetTransportEssentialContextSpendsReserve(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	budget := NewSyncBudgetWithEssentialReserve(10) // reserve 1
	bt := WrapSyncBudgetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}), budget)

	send := func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet,
			"https://api.github.com/repos/acme/widget/pulls", nil,
		)
		require.NoError(err)
		_, err = bt.RoundTrip(req)
		return err
	}

	// Optional counted requests exhaust the optional ceiling (limit-reserve).
	optionalCtx := WithSyncBudget(t.Context())
	for range 9 {
		require.NoError(send(optionalCtx))
	}
	require.ErrorIs(send(optionalCtx), platform.ErrSyncBudgetExhausted)

	// An essential list fetch still goes through on the reserve.
	essentialCtx := WithEssentialSyncBudget(optionalCtx)
	require.NoError(send(essentialCtx))

	// The full limit still bounds essential spend.
	assert.ErrorIs(send(essentialCtx), platform.ErrSyncBudgetExhausted)
}

func TestBudgetTransport_SkipsNotModifiedResponses(t *testing.T) {
	assert := assert.New(t)

	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusNotModified)
			return rec.Result(), nil
		}),
		budget: budget,
	}

	ctx := WithSyncBudget(t.Context())
	req, _ := http.NewRequestWithContext(
		ctx, "GET", "https://api.github.com/repos/o/n/pulls", nil,
	)
	_, err := bt.RoundTrip(req)
	require.NoError(t, err)

	assert.Equal(0, budget.Spent(),
		"304 responses should not spend sync budget")
}

func TestBudgetTransport_SkipsNonSyncContext(t *testing.T) {
	assert := assert.New(t)

	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return httptest.NewRecorder().Result(), nil
		}),
		budget: budget,
	}

	req, _ := http.NewRequestWithContext(
		t.Context(), "GET",
		"https://api.github.com/repos/o/n/pulls", nil,
	)
	_, err := bt.RoundTrip(req)
	require.NoError(t, err)

	assert.Equal(0, budget.Spent(),
		"non-sync context should not increment budget")
}

func TestBudgetTransport_CountsMultipleRequests(t *testing.T) {
	assert := assert.New(t)

	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return httptest.NewRecorder().Result(), nil
		}),
		budget: budget,
	}

	ctx := WithSyncBudget(t.Context())
	for range 5 {
		req, _ := http.NewRequestWithContext(
			ctx, "GET",
			"https://api.github.com/repos/o/n/pulls", nil,
		)
		_, err := bt.RoundTrip(req)
		require.NoError(t, err)
	}

	assert.Equal(5, budget.Spent())
}

func TestBudgetTransport_CountsEvenOnError(t *testing.T) {
	assert := assert.New(t)

	budget := NewSyncBudget(100)
	bt := &budgetTransport{
		base: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, http.ErrHandlerTimeout
		}),
		budget: budget,
	}

	ctx := WithSyncBudget(t.Context())
	req, _ := http.NewRequestWithContext(
		ctx, "GET",
		"https://api.github.com/repos/o/n/pulls", nil,
	)
	_, _ = bt.RoundTrip(req)

	assert.Equal(1, budget.Spent(),
		"budget should count even when base transport errors")
}

func TestWithSyncBudget_PreservesExistingValues(t *testing.T) {
	type customKey struct{}
	base := context.WithValue(
		t.Context(), customKey{}, "hello",
	)
	ctx := WithSyncBudget(base)

	assert.Equal(t, "hello", ctx.Value(customKey{}))
	_, ok := ctx.Value(syncBudgetKey{}).(bool)
	assert.True(t, ok)
}

// The local ceiling refuses counted requests before any wire attempt, so a
// provider response can never arrive to release an exhausted budget. Recovery
// must therefore come from the budget's own window rollover.
func TestBudgetTransportRecoversAfterWindowWithoutProviderResponse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	budget := NewSyncBudget(1)
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	budget.now = func() time.Time { return clock }
	budget.windowStart = clock

	var providerCalls atomic.Int32
	transport := WrapSyncBudgetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}), budget)

	send := func() error {
		req, err := http.NewRequestWithContext(
			WithSyncBudget(t.Context()), http.MethodGet,
			"https://api.github.com/repos/acme/widget", nil,
		)
		require.NoError(err)
		_, err = transport.RoundTrip(req)
		return err
	}

	require.NoError(send())
	require.ErrorIs(send(), platform.ErrSyncBudgetExhausted)
	assert.Equal(int32(1), providerCalls.Load())

	clock = clock.Add(time.Hour)

	require.NoError(send())
	assert.Equal(int32(2), providerCalls.Load())
}

// An archive attempt covered by a provider quota reservation is metered by
// the quota registry alone: the local sync budget keeps metering live sync
// and must not debit for it.
func TestBudgetTransportSkipsLocalDebitForProviderReservedAttempts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	budget := NewSyncBudget(100)
	transport := WrapSyncBudgetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}), budget)
	ctx := WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()), 10,
		IdentityKey{Host: "github.test", Principal: "user:7"},
		[]QuotaResource{QuotaResourceREST},
	)
	allowance, ok := ctx.Value(archiveAttemptAllowanceKey{}).(*archiveAttemptAllowance)
	require.True(ok)
	reserved := context.WithValue(ctx, archiveProviderAttemptReservationKey{}, allowance)

	req, err := http.NewRequestWithContext(
		reserved, http.MethodGet,
		"https://api.github.com/repos/acme/widget/issues/1", nil,
	)
	require.NoError(err)
	_, err = transport.RoundTrip(req)
	require.NoError(err)
	assert.Zero(budget.Spent())
	assert.Zero(budget.ArchiveSpent())

	// Without the reservation marker — a chain that has no quota transport —
	// the same provider-configured context stays on the local-debit fallback.
	unreserved, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		"https://api.github.com/repos/acme/widget/issues/2", nil,
	)
	require.NoError(err)
	_, err = transport.RoundTrip(unreserved)
	require.NoError(err)
	assert.Equal(1, budget.Spent())
	assert.Equal(1, budget.ArchiveSpent())
}
