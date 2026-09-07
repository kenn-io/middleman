package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/platform"
)

type QuotaResource string

const (
	QuotaResourceREST    QuotaResource = "rest"
	QuotaResourceGraphQL QuotaResource = "graphql"
)

// QuotaPool is one GitHub principal's live capacity for a single resource.
// GitHub meters REST and GraphQL separately per principal, so a route's App
// installation and the user's PAT hold independent pools on the same host.
type QuotaPool struct {
	Identity    IdentityKey
	Resource    QuotaResource
	Limit       int
	Remaining   int
	ResetAt     time.Time
	UpdatedAt   time.Time
	Requests    int
	AttemptCost int // largest observed Remaining decrease per response in this window
	Known       bool
}

type quotaKey struct {
	identity IdentityKey
	resource QuotaResource
}

type quotaReservationKey struct {
	quotaKey
	resetAt time.Time
}

type quotaReservation struct {
	key  quotaReservationKey
	cost int
	pool QuotaPool
}

type QuotaRegistry struct {
	mu           sync.RWMutex
	pools        map[quotaKey]QuotaPool
	reservations map[quotaReservationKey]int
	now          func() time.Time
}

func NewQuotaRegistry() *QuotaRegistry {
	return &QuotaRegistry{
		pools:        make(map[quotaKey]QuotaPool),
		reservations: make(map[quotaReservationKey]int),
		now:          time.Now,
	}
}

func (r *QuotaRegistry) ObserveHeaders(
	identity IdentityKey,
	resource QuotaResource,
	header http.Header,
) {
	if r == nil || identity.Principal == "" {
		return
	}
	key := newQuotaKey(identity, resource)
	r.mu.Lock()
	pool := r.pools[key]
	pool.Identity = key.identity
	pool.Resource = key.resource
	pool.Requests++
	if rate, ok := rateFromQuotaHeaders(header); ok {
		reset := rate.Reset.UTC()
		// Responses complete out of order, so a header issued earlier can
		// arrive after a later one. The reset timestamp orders them: an older
		// window says nothing about the current one and is dropped, an equal
		// window may only lower the count because the provider's remaining
		// only falls inside a window, and a later window is a fresh quota that
		// replaces the pool. /rate_limit snapshots still reconcile either way.
		switch {
		case pool.Known && reset.Before(pool.ResetAt):
			// Drop the observation, but the request itself still happened.
		case pool.Known && reset.Equal(pool.ResetAt):
			if rate.Remaining < pool.Remaining {
				pool.AttemptCost = max(pool.AttemptCost, pool.Remaining-rate.Remaining)
				pool.Remaining = rate.Remaining
			}
			pool.Limit = rate.Limit
			pool.UpdatedAt = r.now().UTC()
		default:
			r.clearReservationsBeforeLocked(key, reset)
			pool.Limit = rate.Limit
			pool.Remaining = rate.Remaining
			pool.ResetAt = reset
			pool.UpdatedAt = r.now().UTC()
			pool.AttemptCost = 1
			pool.Known = true
		}
	}
	r.pools[key] = pool
	r.mu.Unlock()
}

func (r *QuotaRegistry) UpdateSnapshot(
	identity IdentityKey,
	resource QuotaResource,
	rate Rate,
) {
	if r == nil || identity.Principal == "" {
		return
	}
	key := newQuotaKey(identity, resource)
	reset := rate.Reset.UTC()
	known := rate.Limit >= 0 && rate.Remaining >= 0 && !rate.Reset.IsZero()
	r.mu.Lock()
	pool := r.pools[key]
	pool.Identity = key.identity
	pool.Resource = key.resource
	// A delayed /rate_limit response can land after the window it describes has
	// closed. Rewinding ResetAt to it would make the pool read as expired, and
	// an expired pool reads as unobserved, which admits background work the
	// current window may have already ruled out.
	//
	// Within its own window a snapshot is authoritative in both directions,
	// unlike a response header. It is the endpoint whose whole job is to state
	// the truth, refreshes are claimed so only one is in flight per credential,
	// and it is how a pool that headers clamped too low recovers before the
	// window rolls.
	if pool.Known && known && reset.Before(pool.ResetAt) {
		r.pools[key] = pool
		r.mu.Unlock()
		return
	}
	if !pool.Known || !known || !reset.Equal(pool.ResetAt) {
		pool.AttemptCost = 1
	}
	if known && (!pool.Known || reset.After(pool.ResetAt)) {
		r.clearReservationsBeforeLocked(key, reset)
	}
	pool.Limit = rate.Limit
	pool.Remaining = rate.Remaining
	pool.ResetAt = reset
	pool.UpdatedAt = r.now().UTC()
	pool.Known = known
	r.pools[key] = pool
	r.mu.Unlock()
}

func (r *QuotaRegistry) Get(
	identity IdentityKey,
	resource QuotaResource,
) (QuotaPool, bool) {
	if r == nil {
		return QuotaPool{}, false
	}
	r.mu.RLock()
	pool, ok := r.pools[newQuotaKey(identity, resource)]
	r.mu.RUnlock()
	return pool, ok
}

func (r *QuotaRegistry) Snapshot() []QuotaPool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	pools := make([]QuotaPool, 0, len(r.pools))
	for _, pool := range r.pools {
		pools = append(pools, pool)
	}
	r.mu.RUnlock()
	slices.SortFunc(pools, func(a, b QuotaPool) int {
		if cmp := strings.Compare(a.Identity.Host, b.Identity.Host); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Identity.Principal, b.Identity.Principal); cmp != 0 {
			return cmp
		}
		return strings.Compare(string(a.Resource), string(b.Resource))
	})
	return pools
}

// QuotaAvailability reports one credential's headroom across the requested
// resources. Known and Exhausted are tracked independently: a resource whose
// pool has never been observed makes the answer unknown, but it must not hide
// a different resource that is known to be at its reserve. Callers that treat
// unknown as permission to proceed must therefore also check Exhausted.
type QuotaAvailability struct {
	Allowed   bool
	Known     bool
	Exhausted bool
	ResetAt   *time.Time
}

// QuotaPacingResource is one resource pool's view inside a pacing window.
type QuotaPacingResource struct {
	Limit     int
	Remaining int
	// Headroom is remaining minus this pool's own archive reserve
	// (`ArchiveProviderReserve` of this pool's limit). Negative when the
	// pool sits below its reserve.
	Headroom int
	ResetAt  time.Time
}

type QuotaPacingWindow struct {
	Limit     int
	Remaining int
	// ArchiveHeadroom is the archive spend this credential may admit: the
	// minimum across required resources of remaining minus that pool's own
	// archive reserve. Reserves are per pool — applying the smallest pool's
	// reserve to a larger pool would admit spend below the larger pool's
	// floor. Negative when the binding pool sits below its reserve.
	ArchiveHeadroom int
	ResetAt         time.Time
	Resources       map[QuotaResource]QuotaPacingResource
}

// ArchiveRetryAt returns the latest reset among pools whose headroom is below
// cost: the earliest time every deficient pool can have recovered. Waiting for
// the window-wide latest reset would leave archives paused after the exhausted
// pool has already reset. Zero when no pool is deficient.
func (w QuotaPacingWindow) ArchiveRetryAt(cost int) time.Time {
	var retry time.Time
	for _, resource := range w.Resources {
		if resource.Headroom < cost && resource.ResetAt.After(retry) {
			retry = resource.ResetAt
		}
	}
	return retry
}

// ArchiveBindingResource returns the pool with the least archive headroom —
// the constraint that currently limits archive admission — with a
// lexicographic tie-break for determinism.
func (w QuotaPacingWindow) ArchiveBindingResource() (QuotaResource, QuotaPacingResource) {
	var bindingName QuotaResource
	var binding QuotaPacingResource
	first := true
	for name, resource := range w.Resources {
		if first || resource.Headroom < binding.Headroom ||
			(resource.Headroom == binding.Headroom && name < bindingName) {
			bindingName = name
			binding = resource
			first = false
		}
	}
	return bindingName, binding
}

func (r *QuotaRegistry) PacingWindow(
	identity IdentityKey,
	resources []QuotaResource,
) (QuotaPacingWindow, bool) {
	if r == nil || len(resources) == 0 {
		return QuotaPacingWindow{}, false
	}
	now := r.now().UTC()
	r.mu.RLock()
	defer r.mu.RUnlock()
	window := QuotaPacingWindow{
		Resources: make(map[QuotaResource]QuotaPacingResource, len(resources)),
	}
	for index, resource := range resources {
		key := newQuotaKey(identity, resource)
		pool, ok := r.pools[key]
		if !ok || !pool.Known || pool.Limit <= 0 || pool.Remaining < 0 ||
			pool.ResetAt.IsZero() || !pool.ResetAt.After(now) {
			return QuotaPacingWindow{}, false
		}
		if window.Limit == 0 || pool.Limit < window.Limit {
			window.Limit = pool.Limit
		}
		remaining := r.effectiveRemainingLocked(key, pool)
		if index == 0 || remaining < window.Remaining {
			window.Remaining = remaining
		}
		headroom := remaining - ArchiveProviderReserve(pool.Limit)
		if index == 0 || headroom < window.ArchiveHeadroom {
			window.ArchiveHeadroom = headroom
		}
		if pool.ResetAt.After(window.ResetAt) {
			window.ResetAt = pool.ResetAt
		}
		window.Resources[resource] = QuotaPacingResource{
			Limit:     pool.Limit,
			Remaining: remaining,
			Headroom:  headroom,
			ResetAt:   pool.ResetAt,
		}
	}
	return window, true
}

// AllowedOrUnobserved reports whether background work may proceed. Unknown
// quota is permission to proceed — ordinary response headers are what populate
// the registry — but only while no observed resource sits at its reserve.
func (a QuotaAvailability) AllowedOrUnobserved() bool {
	return a.Allowed || (!a.Known && !a.Exhausted)
}

func (r *QuotaRegistry) CheckReserve(
	identity IdentityKey,
	resources []QuotaResource,
	cost int,
	reserve int,
) QuotaAvailability {
	availability := QuotaAvailability{Allowed: true, Known: true}
	now := r.now().UTC()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, resource := range resources {
		key := newQuotaKey(identity, resource)
		pool, ok := r.pools[key]
		if !ok || !pool.Known || pool.ResetAt.IsZero() || !pool.ResetAt.After(now) {
			availability.Allowed = false
			availability.Known = false
			continue
		}
		if r.effectiveRemainingLocked(key, pool)-cost < reserve {
			availability.Allowed = false
			availability.Exhausted = true
			if !pool.ResetAt.IsZero() &&
				(availability.ResetAt == nil || pool.ResetAt.After(*availability.ResetAt)) {
				reset := pool.ResetAt
				availability.ResetAt = &reset
			}
		}
	}
	return availability
}

// reserveArchiveAttempt reserves one wire attempt's cost against the target
// resource pool. Every required pool must stay above its own archive reserve
// (`ArchiveProviderReserve` of that pool's limit) — reserves are per pool, so
// a larger pool keeps its proportionally larger floor.
func (r *QuotaRegistry) reserveArchiveAttempt(
	identity IdentityKey,
	resources []QuotaResource,
	resource QuotaResource,
) (quotaReservation, bool) {
	if r == nil || identity.Principal == "" {
		return quotaReservation{}, false
	}
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()

	var targetKey quotaKey
	var target QuotaPool
	for _, required := range resources {
		key := newQuotaKey(identity, required)
		pool, ok := r.pools[key]
		if !ok || !pool.Known || pool.ResetAt.IsZero() || !pool.ResetAt.After(now) {
			return quotaReservation{}, false
		}
		remaining := r.effectiveRemainingLocked(key, pool)
		if required == resource {
			targetKey = key
			target = pool
			continue
		}
		if remaining <= ArchiveProviderReserve(pool.Limit) {
			return quotaReservation{}, false
		}
	}
	if target.Resource == "" {
		return quotaReservation{}, false
	}
	cost := max(target.AttemptCost, 1)
	if r.effectiveRemainingLocked(targetKey, target)-cost < ArchiveProviderReserve(target.Limit) {
		return quotaReservation{}, false
	}
	key := quotaReservationKey{quotaKey: targetKey, resetAt: target.ResetAt.UTC()}
	r.reservations[key] += cost
	return quotaReservation{key: key, cost: cost, pool: target}, true
}

func (r *QuotaRegistry) reconcileArchiveReservation(
	reservation quotaReservation,
	observedReset time.Time,
) {
	if r == nil || reservation.cost <= 0 || observedReset.IsZero() ||
		observedReset.Before(reservation.key.resetAt) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseArchiveReservationLocked(reservation)
}

func (r *QuotaRegistry) releaseArchiveReservation(reservation quotaReservation) {
	if r == nil || reservation.cost <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseArchiveReservationLocked(reservation)
}

func (r *QuotaRegistry) releaseArchiveReservationLocked(reservation quotaReservation) {
	remaining := r.reservations[reservation.key] - reservation.cost
	if remaining > 0 {
		r.reservations[reservation.key] = remaining
	} else {
		delete(r.reservations, reservation.key)
	}
}

func (r *QuotaRegistry) effectiveRemainingLocked(key quotaKey, pool QuotaPool) int {
	reserved := r.reservations[quotaReservationKey{
		quotaKey: key,
		resetAt:  pool.ResetAt.UTC(),
	}]
	return max(pool.Remaining-reserved, 0)
}

func (r *QuotaRegistry) clearReservationsBeforeLocked(key quotaKey, reset time.Time) {
	for reservation := range r.reservations {
		if reservation.quotaKey == key && reservation.resetAt.Before(reset) {
			delete(r.reservations, reservation)
		}
	}
}

func (r *QuotaRegistry) raiseAttemptCost(
	key quotaKey,
	resetAt time.Time,
	cost int,
) {
	if r == nil || cost <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pool, ok := r.pools[key]
	if !ok || !pool.Known || !pool.ResetAt.Equal(resetAt) ||
		cost <= pool.AttemptCost {
		return
	}
	pool.AttemptCost = cost
	r.pools[key] = pool
}

func (r *QuotaRegistry) EarliestReset(
	identity IdentityKey,
	resources []QuotaResource,
) *time.Time {
	var earliest *time.Time
	for _, resource := range resources {
		pool, ok := r.Get(identity, resource)
		if !ok || !pool.Known || pool.ResetAt.IsZero() {
			continue
		}
		if earliest == nil || pool.ResetAt.Before(*earliest) {
			reset := pool.ResetAt
			earliest = &reset
		}
	}
	return earliest
}

func newQuotaKey(identity IdentityKey, resource QuotaResource) quotaKey {
	return quotaKey{
		identity: IdentityKey{
			Host:      canonicalRepoHost(identity.Host),
			Principal: identity.Principal,
		},
		resource: resource,
	}
}

func rateFromQuotaHeaders(header http.Header) (Rate, bool) {
	if header == nil {
		return Rate{}, false
	}
	limit, err := strconv.Atoi(header.Get("X-RateLimit-Limit"))
	if err != nil {
		return Rate{}, false
	}
	remaining, err := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	if err != nil {
		return Rate{}, false
	}
	resetUnix, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return Rate{}, false
	}
	return Rate{
		Limit: limit, Remaining: remaining, Reset: time.Unix(resetUnix, 0).UTC(),
	}, true
}

type quotaResourceContextKey struct{}

func withQuotaResource(ctx context.Context, resource QuotaResource) context.Context {
	return context.WithValue(ctx, quotaResourceContextKey{}, resource)
}

func quotaResourceFromContext(ctx context.Context, fallback QuotaResource) QuotaResource {
	resource, ok := ctx.Value(quotaResourceContextKey{}).(QuotaResource)
	if !ok || resource == "" {
		return fallback
	}
	return resource
}

// quotaTransport attributes response rate-limit headers to the principal that
// authenticated the request. Each of a route's transport chains (read,
// mutation, notification) is built with a fixed identity, so attribution comes
// from the chain rather than from inspecting the resolved token.
type quotaTransport struct {
	base     http.RoundTripper
	registry *QuotaRegistry
	identity IdentityKey
	resource QuotaResource
}

func (t *quotaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resource := quotaResourceFromContext(req.Context(), t.resource)
	guardedReq, reservation, allowed := reserveArchiveProviderAttempt(
		req, t.registry, t.identity, resource,
	)
	if !allowed {
		return nil, platform.ErrArchiveAttemptBudget
	}
	resp, err := t.base.RoundTrip(guardedReq)
	if errors.Is(err, platform.ErrSyncBudgetExhausted) ||
		errors.Is(err, platform.ErrArchiveAttemptBudget) {
		t.registry.releaseArchiveReservation(reservation.quota)
		return resp, err
	}
	var header http.Header
	if resp == nil || t.registry == nil || t.identity.Principal == "" {
		reconcileArchiveProviderAttempt(reservation, t.registry, header)
		return resp, err
	}
	header = resp.Header
	t.registry.ObserveHeaders(
		t.identity,
		resource,
		header,
	)
	reconcileArchiveProviderAttempt(reservation, t.registry, header)
	return resp, err
}
