package github

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"go.kenn.io/forge/platform"
)

type syncBudgetKey struct{}
type archiveSyncBudgetKey struct{}
type archiveAttemptAllowanceKey struct{}
type archiveProviderAttemptReservationKey struct{}

// archiveAttemptAllowance is a shared, mutable counter of the provider capacity
// an admitted archive request is still allowed to consume. Headerless providers
// debit one unit per wire attempt. GitHub transports reserve the resource's
// largest observed same-window attempt cost and reconcile larger response
// deltas, so SDK and authentication retries cannot outrun live quota headroom.
type archiveAttemptAllowance struct {
	remaining atomic.Int64
	provider  *archiveProviderAttemptConfig
}

type archiveProviderAttemptConfig struct {
	identity  IdentityKey
	resources []QuotaResource
}

type archiveProviderAttemptReservation struct {
	allowance *archiveAttemptAllowance
	before    QuotaPool
	cost      int
	quota     quotaReservation
}

// WithArchiveAttemptAllowance attaches a per-request wire-attempt allowance to
// ctx alongside the archive-budget marker. Admission sets attempts to the
// admitted archive cost so the ceiling enforced at admission is also enforced
// atomically at every wire attempt.
func WithArchiveAttemptAllowance(ctx context.Context, attempts int) context.Context {
	allowance := &archiveAttemptAllowance{}
	allowance.remaining.Store(int64(attempts))
	return context.WithValue(ctx, archiveAttemptAllowanceKey{}, allowance)
}

// WithArchiveProviderAttemptAllowance adds live provider-pool enforcement to
// the admitted allowance. quotaTransport reserves capacity before every wire
// attempt and updates the effective cost from response headers.
func WithArchiveProviderAttemptAllowance(
	ctx context.Context,
	attempts int,
	identity IdentityKey,
	resources []QuotaResource,
) context.Context {
	allowance := &archiveAttemptAllowance{
		provider: &archiveProviderAttemptConfig{
			identity: identity, resources: slices.Clone(resources),
		},
	}
	allowance.remaining.Store(int64(attempts))
	return context.WithValue(ctx, archiveAttemptAllowanceKey{}, allowance)
}

// ConsumeArchiveAttemptAllowance reserves one wire attempt from the archive
// allowance in ctx. It returns true when the attempt is permitted and false
// once the allowance is exhausted, in which case the caller must refuse the
// attempt without any upstream I/O. Contexts without an allowance — every live
// (non-archive) request — always permit the attempt.
func ConsumeArchiveAttemptAllowance(ctx context.Context) bool {
	allowance, ok := ctx.Value(archiveAttemptAllowanceKey{}).(*archiveAttemptAllowance)
	if !ok {
		return true
	}
	if reserved, _ := ctx.Value(archiveProviderAttemptReservationKey{}).(*archiveAttemptAllowance); reserved == allowance {
		return true
	}
	return allowance.consume(1)
}

func (a *archiveAttemptAllowance) consume(cost int) bool {
	for {
		remaining := a.remaining.Load()
		if cost <= 0 || remaining < int64(cost) {
			return false
		}
		if a.remaining.CompareAndSwap(remaining, remaining-int64(cost)) {
			return true
		}
	}
}

func (a *archiveAttemptAllowance) debit(cost int) {
	for cost > 0 {
		remaining := a.remaining.Load()
		next := max(remaining-int64(cost), 0)
		if a.remaining.CompareAndSwap(remaining, next) {
			return
		}
	}
}

func reserveArchiveProviderAttempt(
	req *http.Request,
	registry *QuotaRegistry,
	identity IdentityKey,
	resource QuotaResource,
) (*http.Request, archiveProviderAttemptReservation, bool) {
	if registry == nil || identity.Principal == "" {
		return req, archiveProviderAttemptReservation{}, true
	}
	allowance, ok := req.Context().Value(archiveAttemptAllowanceKey{}).(*archiveAttemptAllowance)
	if !ok || allowance.provider == nil {
		return req, archiveProviderAttemptReservation{}, true
	}
	config := allowance.provider
	resources := []QuotaResource{resource}
	if identity == config.identity {
		resources = config.resources
	}
	quota, allowed := registry.reserveArchiveAttempt(
		identity, resources, resource,
	)
	if !allowed {
		return req, archiveProviderAttemptReservation{}, false
	}
	cost := quota.cost
	if !allowance.consume(cost) {
		registry.releaseArchiveReservation(quota)
		return req, archiveProviderAttemptReservation{}, false
	}
	reservation := archiveProviderAttemptReservation{
		allowance: allowance, before: quota.pool, cost: cost, quota: quota,
	}
	ctx := context.WithValue(
		req.Context(), archiveProviderAttemptReservationKey{}, allowance,
	)
	return req.Clone(ctx), reservation, true
}

func reconcileArchiveProviderAttempt(
	reservation archiveProviderAttemptReservation,
	registry *QuotaRegistry,
	header http.Header,
) {
	if reservation.allowance == nil {
		return
	}
	actualCost := reservation.cost
	if observed, ok := rateFromQuotaHeaders(header); ok {
		observedReset := observed.Reset.UTC()
		if observedReset.After(reservation.before.ResetAt) ||
			(observedReset.Equal(reservation.before.ResetAt) &&
				observed.Remaining <= reservation.before.Remaining) {
			registry.reconcileArchiveReservation(reservation.quota, observedReset)
		}
		switch {
		case observedReset.Equal(reservation.before.ResetAt) &&
			observed.Remaining < reservation.before.Remaining:
			actualCost = max(
				actualCost,
				reservation.before.Remaining-observed.Remaining,
			)
		case observedReset.After(reservation.before.ResetAt):
			actualCost = max(
				actualCost,
				observed.Limit-observed.Remaining,
			)
			registry.raiseAttemptCost(
				reservation.quota.key.quotaKey,
				observedReset,
				actualCost,
			)
		}
	}
	if actualCost > reservation.cost {
		reservation.allowance.debit(actualCost - reservation.cost)
	}
}

// WithSyncBudget marks a context so that HTTP calls made with
// it count against the sync budget. Background sync entry points
// (RunOnce, syncWatchedMRs) inject this; user-initiated server
// handler paths do not.
func WithSyncBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, syncBudgetKey{}, true)
}

func IsSyncBudgetContext(ctx context.Context) bool {
	_, ok := ctx.Value(syncBudgetKey{}).(bool)
	return ok
}

type essentialSyncBudgetKey struct{}

// WithEssentialSyncBudget marks a context whose counted requests may spend
// the budget's essential reserve. Only the list fetches that discover new
// and closed items use it: they are the sync's source of truth, and optional
// work (detail refreshes, fast-sync, archive) must not be able to starve
// them within the configured ceiling.
func WithEssentialSyncBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, essentialSyncBudgetKey{}, true)
}

// WithoutEssentialSyncBudget demotes a context back to optional spend. List
// implementations use it for per-item enrichment lookups nested inside an
// essential list fetch, so enrichment cannot drain the discovery reserve.
func WithoutEssentialSyncBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, essentialSyncBudgetKey{}, false)
}

func IsEssentialSyncBudgetContext(ctx context.Context) bool {
	essential, ok := ctx.Value(essentialSyncBudgetKey{}).(bool)
	return ok && essential
}

func WithArchiveSyncBudget(ctx context.Context) context.Context {
	return context.WithValue(WithSyncBudget(ctx), archiveSyncBudgetKey{}, true)
}

func IsArchiveSyncBudgetContext(ctx context.Context) bool {
	_, ok := ctx.Value(archiveSyncBudgetKey{}).(bool)
	return ok
}

// budgetTransport wraps an http.RoundTripper and increments a
// SyncBudget for every RoundTrip invocation whose request
// context carries the sync-budget key. This captures paginated
// pages and GraphQL calls made during background sync without
// counting user-initiated server actions or GitHub REST 304
// responses that do not spend primary provider quota.
//
// Transparent retries inside net/http.Transport are not visible
// to RoundTripper wrappers and are not counted.
type budgetTransport struct {
	base   http.RoundTripper
	budget *SyncBudget
}

type syncBudgetExhaustedError struct {
	resetAt time.Time
}

func newSyncBudgetExhaustedError(resetAt time.Time) error {
	return &syncBudgetExhaustedError{resetAt: resetAt.UTC()}
}

func (e *syncBudgetExhaustedError) Error() string {
	return platform.ErrSyncBudgetExhausted.Error()
}

func (e *syncBudgetExhaustedError) Unwrap() error {
	return platform.ErrSyncBudgetExhausted
}

func WrapSyncBudgetTransport(base http.RoundTripper, budget *SyncBudget) http.RoundTripper {
	if budget == nil {
		return base
	}
	return &budgetTransport{base: base, budget: budget}
}

// archiveAttemptProviderReserved reports whether the attempt in ctx is
// covered by a provider quota reservation. Reserved attempts are metered by
// the quota registry; the local sync budget meters live sync only and must
// not double-count them. Attempts whose chain took no reservation (no quota
// transport for the identity) stay on the local-debit fallback.
func archiveAttemptProviderReserved(ctx context.Context) bool {
	allowance, ok := ctx.Value(archiveAttemptAllowanceKey{}).(*archiveAttemptAllowance)
	if !ok {
		return false
	}
	reserved, _ := ctx.Value(archiveProviderAttemptReservationKey{}).(*archiveAttemptAllowance)
	return reserved == allowance
}

func (t *budgetTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	if !ConsumeArchiveAttemptAllowance(req.Context()) {
		return nil, platform.ErrArchiveAttemptBudget
	}
	counted := IsSyncBudgetContext(req.Context())
	archive := IsArchiveSyncBudgetContext(req.Context())
	if archive && archiveAttemptProviderReserved(req.Context()) {
		counted = false
	}
	var window BudgetWindow
	if counted {
		var reserved bool
		var resetAt time.Time
		window, reserved, resetAt = t.budget.trySpendForTransport(
			1, archive, IsEssentialSyncBudgetContext(req.Context()),
		)
		if !reserved {
			return nil, newSyncBudgetExhaustedError(resetAt)
		}
	}
	resp, err := t.base.RoundTrip(req)
	if counted && resp != nil && resp.StatusCode == http.StatusNotModified {
		if archive {
			t.budget.RefundArchive(window, 1)
		} else {
			t.budget.Refund(window, 1)
		}
	}
	return resp, err
}
