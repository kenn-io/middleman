# Retries, Backoff, and Single-Flight Dedup

Use this document for transient upstream retries, rate-limit gates, scheduling
cadence, or single-flight request deduplication.

kenn-forge has three distinct wait patterns. Keep them separate:

- **Transient retry** — short bounded retries for flaky upstream failures.
- **Rate-limit gate** — wait until provider quota resets.
- **Scheduling cadence** — steady periodic loops.

Issue #299 consolidates only transient retry under one shared package.
Rate-limit gates and cadence loops are intentionally separate.

## Transient retry: `internal/retry`

[`internal/retry/retry.go`](../internal/retry/retry.go). Canonical home for
`cenkalti/backoff/v5` orchestration. It wraps idempotent operations in a
bounded retry loop, retries only when the caller's classifier marks an error
transient, wraps all other errors with `backoff.Permanent`, and logs each retry
attempt at `DEBUG`.

Production schedule: 500ms → ~4s, jitter ±30%, 3 attempts.

**Transient retry sites:**

- [`internal/retry/retry.go`](../internal/retry/retry.go) — shared helper and
  production schedule.
- [`internal/gitclone/retry.go`](../internal/gitclone/retry.go) — git-specific
  transient matcher plus thin adapter into `internal/retry`.
- [`internal/gitclone/clone.go`](../internal/gitclone/clone.go) — retries
  `git clone --bare`, `git fetch`, and `git remote set-head`.

Use transient retry for idempotent work that hits flaky upstreams. Do not stack
it on top of paths that already retry (for example `internal/github` REST
client behavior). Test the shared helper against `DoWithBackOff` with a fast
injected backoff, and test package-specific callers for their own classifier and
budget choices.

Scope upstream timeout child contexts to the upstream call; subsequent local
database and filesystem work must retain the request context
(`internal/server/kata/workspace.go::getKataProjectMappings`).

To extend git transient matching, add a substring to the slice in
`internal/gitclone/retry.go` and a row to `internal/gitclone/retry_test.go`.
Keep the matcher conservative — false positives turn permanent failures into
multi-second hangs.

## Rate-limit gates

These paths are **not** transient retry. They represent provider quota state and
wait until the reset window.

- [`internal/ratelimit/rate.go`](../internal/ratelimit/rate.go) —
  `RateTracker.ShouldBackoff()` returns `(bool, time.Duration)` for exhausted
  quota windows.
- [`internal/github/graphql.go`](../internal/github/graphql.go) —
  `GraphQLFetcher.ShouldBackoff()` passthrough for GraphQL quota gating.
- [`internal/github/sync.go`](../internal/github/sync.go) — worker and GraphQL
  call sites that gate work on `ShouldBackoff()` before proceeding.

Do not wrap these paths in `backoff.Retry`, `RetryAfterError`, or any new retry
abstraction unless a separate design explicitly changes rate-limit policy.

## Scheduling cadence (out of scope)

Steady background cadence is scheduling policy, not transient retry. Do not
migrate ticker-driven sync or refresh loops into `backoff/v5`
(`internal/github/sync.go::Syncer.Start`).

The archive worker uses the backoff schedule type only as an idle delay calculator,
not as a retry wrapper: idle passes double up to a five-minute cap and any wake or
worked pass resets it (`internal/github/sync.go::runArchiveLoop`).

- Manual workflow dispatch is non-idempotent and never retried by either side.
  The server locates the created run by reading runs for about a minute, then
  watches that run at a fixed interval until it completes or thirty minutes pass;
  the browser never polls workflow state
  (`internal/server/workflowapi/dispatch_follow.go::Handler.followDispatch`).

## Long-lived stream recovery

Hub event-stream recovery is connection lifecycle policy, not a retry
budget for one idempotent provider request. A spoke reconnects indefinitely at
1s, 2s, then a jittered 4s ceiling. Opening a valid SSE response resets the
failure count; authorization, protocol, framing, and transport failures all use
the same bounded wait so none can hot-loop. Context cancellation interrupts
both an active read and the reconnect timer immediately
(`internal/providerplane/events.go::EventClient.Run`).

Every successful connection requests provider reconciliation and refreshes
sync status after the replay-complete barrier and before it is reported
healthy. A stale cursor or poison frame
updates only the private hub cursor and repeats that recovery; it never
copies a remote ID into the spoke's local browser cursor
(`internal/providerplane/events.go::EventClient.resynchronize`).

- Provider-index sync keeps at most one coalesced follow-up, atomically transfers
  its single-flight slot, and preserves nil-versus-empty scope; bursts neither
  drop accepted work nor create an unbounded queue. Scoped user bypasses stay
  bound to stable repository identity when coalesced with full work. Ad-hoc
  triggers return only after admission; provider execution stays asynchronous.
  (`internal/github/sync.go::triggerRunWithCadence`)
- Public sync status stays running across retained follow-up handoffs; terminal
  publication is ordered with slot release so later runs cannot be overwritten
  by a stale idle snapshot.
  (`internal/github/sync.go::runOnceWithSlot`)

Repository-disabled issue and merge-request scopes use a 24-hour in-memory
background probe gate. Expiry admits one reserved background probe across all
lanes; any pre-provider exit abandons only the reservation, while completed provider
work either renews or releases the observed generation. (`internal/github/feature_cooldown.go::repositoryFeatureProbe.abandon`)
Explicit sync bypasses only cooldown generations present when the run begins; any
non-disabled attempt clears that observed generation even when retry bits remain, while
newer disabled results gate follow-on lanes. (`internal/github/feature_cooldown.go::repositoryFeatureProbe.release`)
Cooldown keys use provider-canonical repository identity; GitHub host, owner/name case,
and derived-path aliases must converge. (`internal/github/feature_cooldown.go::repositoryFeatureKey`)
Completion clears only the observed generation, so a concurrent disabled renewal wins.
(`internal/github/feature_cooldown.go::repositoryFeatureProbe.clear`)
Follow-on lanes after index completion must acquire a fresh probe; clearing the index
generation does not authorize an unchanged-list comment refresh. (`internal/github/sync.go::refreshRepoPRComments`)
Classify repository-disabled reads before generic fallback or detail handling. GitHub
uses its definitive disabled 410; other providers confirm candidate 403/404/410 failures
against repository metadata and preserve unconfirmed errors. (`internal/platform/gitealike/feature_disabled.go::Provider.repositoryFeatureError`)
Custom GitHub GraphQL timeline transports must preserve structured non-2xx errors before
closing the body so disabled-feature classification can inspect 410 responses.
(`internal/github/client.go::ListPullRequestTimelineEvents`)
Recording a disabled result must preserve any earlier same-scope item failure so retry
and ETag invalidation state survives the cooldown. (`internal/github/sync.go::indexSyncRepo`)
Apply the gate before shared budget exhaustion so suppressed work cannot starve
unaffected scopes. (`internal/github/sync.go::drainDetailQueue`)
Archive inventory and maintenance bypass cached feature cooldowns and commit empty streams only
after a live disabled response; a live recovery reopens an unsupported historical stream only when
the provider declares that inventory capability, while hydration keeps honoring the cooldown.
(`internal/github/sync.go::Admit`, `internal/archive/`)

## Shared clone slot: `Manager.EnsureClone`

[`clone.go`](../internal/gitclone/clone.go). `EnsureClone` opens a shared slot
keyed on storage namespace plus `(host, owner, name)` so concurrent callers
share one clone/fetch instead of stampeding GitHub. The slot remains occupied
until every joined caller completes its own route validation gate.

Invariants to preserve:

- **Pre-check `ctx.Err()`**. A caller whose ctx is already canceled
  must not enter the slot, or the runner does work for nobody.
- **Key shape** `namespace \x00 host \x00 owner \x00 name`. The null separator
  prevents `foo/barbaz` colliding with `foobar/baz`.
- **Detached, bounded runner ctx**. The slot runs with
  `context.WithTimeout(context.WithoutCancel(ctx), ensureCloneTimeout)`.
  Detached so one canceled waiter cannot abort work for others;
  bounded so a stuck git subprocess cannot hold the slot forever.
- **Only the slot starter deletes**. The starter's validation runs inside the
  slot after the fetch and spans the whole fetch window; its failure attempts
  to remove the clone before releasing waiters. Callers preflight before joining
  and remain registered through their final route gate; completed flights stay
  joinable until all gates finish. Followers retry only after successful
  quarantine and full drain; cleanup failure is terminal. Invalid clones are
  renamed aside so external readers never observe partial deletion
  (`internal/gitclone/clone.go::ensureCloneInNamespaceValidated`).

Reach for a shared slot whenever multiple in-process call sites
hit the same upstream resource. Prefer dedup over retry — it removes
the cause, retry just absorbs the effect.

GitHub App token mints share one in-flight result per credential. Only errors matching the minting
caller's context failure bypass caching and waiters; internal timeouts cool down normally. Headerless
failures cool down for five seconds; server-directed retry deadlines stop at one hour.
(`internal/tokenauth/source.go::githubAppTokenStore.resolve`)
Unauthorized retries evict only the exact rejected token; in-flight mints, active failure cooldowns,
and newer completed tokens survive stale responses.
(`internal/tokenauth/source.go::githubAppTokenStore.invalidateToken`)

Roborev discovery retains definitive results until managed-clone initialization
confirms a registration. Invalidation clears all inventory state and fences out
older refreshes by generation; transient failures still retry after cooldown and
each waiter can cancel independently (`internal/server/roborev_repositories.go::roborevRepositoryProbe`).

## Tests

Test the policy decisions, not the library. For retry that means the
matcher, the `backoff.Permanent` wrap, and the budget constant. For
dedup that means the key shape, per-caller validation, and integration paths
that exercise the real cloneBare/fetch.
