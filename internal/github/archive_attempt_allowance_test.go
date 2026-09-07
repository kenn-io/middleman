package github

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/platform"
)

// countingRoundTripper records wire attempts and returns a fixed status so a
// test can prove exactly how many attempts reached the underlying transport.
type countingRoundTripper struct {
	calls  atomic.Int64
	status int
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return &http.Response{
		StatusCode: c.status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestArchiveAttemptAllowanceRefusesBeyondAdmittedCeiling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := &countingRoundTripper{status: http.StatusInternalServerError}
	budget := NewSyncBudget(100)
	transport := WrapSyncBudgetTransport(base, budget)
	ctx := WithArchiveAttemptAllowance(WithArchiveSyncBudget(t.Context()), 2)

	for attempt := 1; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
		require.NoError(err)
		resp, err := transport.RoundTrip(req)
		require.NoError(err)
		assert.Equal(http.StatusInternalServerError, resp.StatusCode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
	require.NoError(err)
	resp, err := transport.RoundTrip(req)
	assert.Nil(resp)
	require.ErrorIs(err, platform.ErrArchiveAttemptBudget)

	assert.Equal(int64(2), base.calls.Load())
	assert.Equal(2, budget.ArchiveSpent())
	assert.Equal(2, budget.Spent())
}

func TestArchiveAttemptAllowanceBoundsAuthRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := &countingRoundTripper{status: http.StatusUnauthorized}
	budget := NewSyncBudget(100)
	// budgetTransport sits beneath AuthTransport, exactly as the production
	// clients layer it, so an authentication retry is a second wire attempt
	// that must draw from the same admitted allowance.
	authRT := platform.AuthTransport{
		Source:              newMutableRuntimeAuthTokenSource("first-token"),
		Base:                WrapSyncBudgetTransport(base, budget),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}
	ctx := WithArchiveAttemptAllowance(WithArchiveSyncBudget(t.Context()), 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
	require.NoError(err)

	resp, err := authRT.RoundTrip(req)
	assert.Nil(resp)
	require.ErrorIs(err, platform.ErrArchiveAttemptBudget)
	// The initial attempt spent the only admitted unit; the authentication
	// retry was refused locally without a second wire attempt.
	assert.Equal(int64(1), base.calls.Load())
	assert.Equal(1, budget.ArchiveSpent())
}

func TestArchiveAttemptAllowanceLeavesLiveContextsUnbounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := &countingRoundTripper{status: http.StatusInternalServerError}
	budget := NewSyncBudget(100)
	transport := WrapSyncBudgetTransport(base, budget)
	// A live sync context carries no attempt allowance, so its retries are
	// never refused by the archive ceiling.
	ctx := WithSyncBudget(t.Context())

	for range 5 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
		require.NoError(err)
		resp, err := transport.RoundTrip(req)
		require.NoError(err)
		assert.Equal(http.StatusInternalServerError, resp.StatusCode)
	}
	assert.Equal(int64(5), base.calls.Load())
	assert.Zero(budget.ArchiveSpent())
	assert.Equal(5, budget.Spent())
}

func TestArchiveProviderAttemptAllowanceUsesObservedQuotaCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	registry := NewQuotaRegistry()
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, resource := range []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL} {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: 1003, Reset: reset,
		})
	}
	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     quotaTestHeaders(5000, 1001, reset),
			Request:    req,
		}, nil
	})
	budget := NewSyncBudget(100)
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceREST,
	}
	ctx := WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()),
		3,
		identity,
		[]QuotaResource{QuotaResourceREST, QuotaResourceGraphQL},
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
	require.NoError(err)
	_, err = transport.RoundTrip(req)
	require.NoError(err)

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil)
	require.NoError(err)
	resp, err := transport.RoundTrip(req)
	assert.Nil(resp)
	require.ErrorIs(err, platform.ErrArchiveAttemptBudget)

	assert.Equal(int32(1), calls.Load())
	assert.Zero(budget.ArchiveSpent())
	pool, ok := registry.Get(identity, QuotaResourceREST)
	require.True(ok)
	assert.Equal(1001, pool.Remaining)
	assert.Equal(2, pool.AttemptCost)
	window, ok := registry.PacingWindow(
		identity, []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL},
	)
	require.True(ok)
	assert.Equal(1001, window.Remaining)
}

func TestArchiveProviderQuotaCostPersistsAcrossAdmissions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	registry := NewQuotaRegistry()
	registry.now = func() time.Time { return now }
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	resources := []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL}
	for _, resource := range resources {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: 5000, Reset: reset,
		})
	}
	window, ok := registry.PacingWindow(identity, resources)
	require.True(ok)
	budget := NewSyncBudget(100000)
	assert.Equal(5000, window.Remaining)

	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := http.Header{}
		if calls.Add(1) == 1 {
			header = quotaTestHeaders(5000, 4998, reset)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     header,
			Request:    req,
		}, nil
	})
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceREST,
	}
	ctx := WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()),
		1200,
		identity,
		resources,
	)
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil,
	)
	require.NoError(err)
	_, err = transport.RoundTrip(req)
	require.NoError(err)

	window, ok = registry.PacingWindow(identity, resources)
	require.True(ok)
	assert.Equal(4998, window.Remaining)

	// A later admission reserves the learned two-unit cost. Even if its
	// response omits quota headers, that reserved cost must persist beyond the
	// request-local allowance.
	ctx = WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()),
		1198,
		identity,
		resources,
	)
	req, err = http.NewRequestWithContext(
		ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil,
	)
	require.NoError(err)
	_, err = transport.RoundTrip(req)
	require.NoError(err)

	window, ok = registry.PacingWindow(identity, resources)
	require.True(ok)
	assert.Equal(4996, window.Remaining)
}

func TestArchiveProviderHeaderlessReservationsProtectReserveAcrossAdmissions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	registry := NewQuotaRegistry()
	registry.now = func() time.Time { return now }
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	resources := []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL}
	for _, resource := range resources {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: ArchiveProviderReserve(5000) + 1, Reset: reset,
		})
	}
	window, ok := registry.PacingWindow(identity, resources)
	require.True(ok)
	budget := NewSyncBudget(100000)
	assert.Equal(ArchiveProviderReserve(5000)+1, window.Remaining)

	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceREST,
	}
	request := func() error {
		ctx := WithArchiveProviderAttemptAllowance(
			WithArchiveSyncBudget(t.Context()),
			1,
			identity,
			resources,
		)
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil,
		)
		require.NoError(err)
		_, err = transport.RoundTrip(req)
		return err
	}

	require.NoError(request())
	window, ok = registry.PacingWindow(identity, resources)
	require.True(ok)
	assert.Equal(ArchiveProviderReserve(5000), window.Remaining)
	require.ErrorIs(request(), platform.ErrArchiveAttemptBudget)
	assert.Equal(int32(1), calls.Load())
}

func TestArchiveProviderAttemptAllowanceResetsObservedCostWithQuotaWindow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	registry := NewQuotaRegistry()
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	firstReset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	nextReset := firstReset.Add(time.Hour)
	for _, resource := range []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL} {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: 1010, Reset: firstReset,
		})
	}
	currentReset := firstReset
	currentRemaining := 1008
	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     quotaTestHeaders(5000, currentRemaining, currentReset),
			Request:    req,
		}, nil
	})
	budget := NewSyncBudget(100)
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceREST,
	}
	ctx := WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()),
		3,
		identity,
		[]QuotaResource{QuotaResourceREST, QuotaResourceGraphQL},
	)
	request := func() error {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil,
		)
		if err != nil {
			return err
		}
		_, err = transport.RoundTrip(req)
		return err
	}

	require.NoError(request())
	pool, ok := registry.Get(identity, QuotaResourceREST)
	require.True(ok)
	assert.Equal(2, pool.AttemptCost)

	registry.UpdateSnapshot(identity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: 1010, Reset: nextReset,
	})
	currentReset = nextReset
	currentRemaining = 1009
	require.NoError(request())

	pool, ok = registry.Get(identity, QuotaResourceREST)
	require.True(ok)
	assert.Equal(1, pool.AttemptCost)
	assert.Equal(int32(2), calls.Load())
}

func TestArchiveProviderAttemptAllowanceSeedsCostAcrossQuotaWindowReset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 7, 28, 18, 30, 0, 0, time.UTC)
	firstReset := now.Add(5 * time.Minute)
	nextReset := now.Add(time.Hour)
	registry := NewQuotaRegistry()
	registry.now = func() time.Time { return now }
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	resources := []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL}
	for _, resource := range resources {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: 5000, Reset: firstReset,
		})
	}

	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header: quotaTestHeaders(
				RateReserveBuffer+5,
				RateReserveBuffer+1,
				nextReset,
			),
			Request: req,
		}, nil
	})
	budget := NewSyncBudget(100)
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceGraphQL,
	}
	request := func() error {
		ctx := WithArchiveProviderAttemptAllowance(
			WithArchiveSyncBudget(t.Context()),
			10,
			identity,
			resources,
		)
		req, err := http.NewRequestWithContext(
			ctx, http.MethodPost, "https://api.github.test/graphql", nil,
		)
		require.NoError(err)
		_, err = transport.RoundTrip(req)
		return err
	}

	require.NoError(request())
	pool, ok := registry.Get(identity, QuotaResourceGraphQL)
	require.True(ok)
	assert.Equal(nextReset, pool.ResetAt)
	assert.Equal(4, pool.AttemptCost)

	require.ErrorIs(request(), platform.ErrArchiveAttemptBudget)
	assert.Equal(int32(1), calls.Load())
}

func TestArchiveProviderAttemptAllowanceRechecksEveryRequiredPool(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	registry := NewQuotaRegistry()
	identity := IdentityKey{Host: "github.com", Principal: "user:7"}
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, resource := range []QuotaResource{QuotaResourceREST, QuotaResourceGraphQL} {
		registry.UpdateSnapshot(identity, resource, Rate{
			Limit: 5000, Remaining: 210, Reset: reset,
		})
	}
	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     quotaTestHeaders(5000, 209, reset),
			Request:    req,
		}, nil
	})
	budget := NewSyncBudget(100)
	transport := &quotaTransport{
		base: WrapSyncBudgetTransport(base, budget), registry: registry,
		identity: identity, resource: QuotaResourceREST,
	}
	ctx := WithArchiveProviderAttemptAllowance(
		WithArchiveSyncBudget(t.Context()),
		10,
		identity,
		[]QuotaResource{QuotaResourceREST, QuotaResourceGraphQL},
	)

	// Concurrent GraphQL work reaches the reserve after admission but before
	// this REST attempt.
	registry.UpdateSnapshot(identity, QuotaResourceGraphQL, Rate{
		Limit: 5000, Remaining: RateReserveBuffer, Reset: reset,
	})
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "https://api.github.test/repos/o/r", nil,
	)
	require.NoError(err)
	resp, err := transport.RoundTrip(req)
	assert.Nil(resp)
	require.ErrorIs(err, platform.ErrArchiveAttemptBudget)
	assert.Zero(calls.Load())
	assert.Zero(budget.ArchiveSpent())
}
