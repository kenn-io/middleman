# Error Handling

Use this document for changes that touch HTTP API failure responses, platform
error translation, generated API clients, or frontend behavior that branches on
server errors. Retry and scheduling policy lives in
[`context/retries-and-backoffs.md`](./retries-and-backoffs.md).

## API Problem Envelope

API handlers should return RFC 9457 `application/problem+json` responses for
failure paths. The envelope keeps the standard fields and adds stable extension
members:

```json
{
  "type": "about:blank",
  "title": "Conflict",
  "status": 409,
  "detail": "provider does not support workflow approval",
  "code": "unsupportedCapability",
  "details": {
    "capability": "workflow_approval",
    "provider": "gitlab",
    "platformHost": "gitlab.com"
  }
}
```

`detail` and `title` are human-readable and may change. UI code, generated
clients, tests, and automation should branch on `code` and `details`, not on
prose.

## Code Taxonomy

Wire codes are camelCase. Keep internal platform error constants in their native
snake_case form and translate them at the `internal/server` boundary.
`internal/server/httpapi/problems.go::ProblemCode` is the complete wire-code
source of truth; the table below records selected domain and recovery contracts,
not an exhaustive enum.

| Wire code | Status | Use |
| --- | ---: | --- |
| `badRequest` | 400 | Generic malformed request fallback. |
| `validationError` | 400 | Input validation such as blank fields, invalid formats, or allowed-value checks. Include `details.field`; include `details.allowed` when useful. |
| `forbidden` | 403 | Authenticated caller or token lacks permission. |
| `notFound` | 404 | Generic not-found fallback. When a provider lookup reports the item moved to another repository, include `details.destinationProvider`, `details.destinationPlatformHost`, `details.destinationOwner`, and `details.destinationName` so clients can retarget the reference. |
| `repoNotFound` | 404 | Repository lookup miss by provider-aware identity. |
| `pullNotFound` | 404 | Pull or merge request lookup miss. |
| `issueNotFound` | 404 | Issue lookup miss. |
| `commentNotFound` | 404 | Comment lookup miss. |
| `projectNotFound` | 404 | Local project record lookup miss. |
| `workspaceNotFound` | 404 | Workspace lookup miss; it does not prove that the workspace was deleted. |
| `settingsUnavailable` | 404 | Settings store is unavailable in the current server mode. |
| `conflict` | 409 | Generic state conflict. Head-bound provider mutations include `details.reason` as a stable discriminator: `stale_state` (target moved past the reviewed commit; reload and re-review), `conflict` (provider refuses the current state), or `head_unknown` (no reviewed head synced locally). Review suggestion apply adds `not_open` (PR closed/merged, locally or upstream) and `head_repo_unknown` (head repo identity missing or inaccessible); both fail closed with no branch mutation. For providers with hard head binding only `stale_state` triggers a server-side MR resync — refreshing on other conflicts would persist a head nobody reviewed and arm stale retries; providers without hard binding also resync on generic merge conflicts to refresh the local mergeable view. Three head states have distinct contracts: (1) initial missing synced head — first-party clients disable head-bound actions preflight and fire no request; the head arrives via normal detail-load or periodic sync; (2) stale pinned head — the 409 `stale_state` makes the client reload the detail (a sync-enabled load) and prompt re-review; (3) server `head_unknown` — reachable only from clients without preflight gating; it never syncs server-side (a freshly persisted head would arm stale retries) and recovery is the same client-initiated reload with head-bound actions disabled until the response carries `platform_head_sha`. When a `stale_state` failure leaves a provider side effect behind — an approval that could not be revoked after the head moved, a review note already posted before the approval failed — `details.context` carries that context verbatim and the human-readable `detail` repeats it; clients still branch on `reason`, but should show `context` so the user knows a blind retry duplicates the side effect. Approval revocation outcomes additionally carry stable members, but only on post-submit staleness — when an approval was actually created upstream and then found to sit on a moved head: `details.revocation` (`succeeded` — the approval was dismissed/deleted upstream; `failed` — it may still stand and the user must verify or remove it manually, identified by `details.review_id`). Pre-check stale approvals omit both members because no approval was created. A post-submit verification read that fails outright is treated like a moved head — the approval is revoked and the response carries the revocation members — because an approval whose head cannot be proven must not stand; `details.cause` (`moved_head` or `head_unverifiable`) distinguishes the two so clients can prompt re-review versus a possibly-unchanged head after a transient read failure. When a review-draft publish both leaves published content behind and hits a stale approval, the 409 `stale_state` envelope wins: it carries the revocation members plus `details.partialPublish` and `details.publishedCommentCount`, and the server clears the published local draft copies before responding — `partially_published` success responses are reserved for non-stale partial failures. Clients must treat the members as optional and only branch on `revocation` when present. |
| `branchConflict` | 409 | Local workspace branch already exists. Include `details.branch`, `details.suggestedBranch`, and `details.existingDirectory`; when the discriminator is true, clients must not offer an alternate-branch action for the occupied deterministic path. |
| `workspaceDirectoryNotReusable` | 409 | The deterministic kenn-forge issue-worktree directory cannot be recovered. Include `details.reason` as `missing`, `not_linked_worktree`, `repository_mismatch`, or `branch_mismatch`; branch mismatches also include `expectedBranch` and `actualBranch`. |
| `unsupportedCapability` | 409 | Provider lacks the operation capability. Include `details.capability`, `details.provider`, and `details.platformHost`. |
| `resyncRequired` | 409 | A provider item lacks stable external identity required for a safe mutation. Include `details.subject_kind` and `details.item_number`; clients should request a successful resync before retrying. |
| `payloadTooLarge` | 413 | A bounded payload exceeds its accepted size. Request-body failures include `details.maxBytes` when known. Archive detailed-report failures use `details.reason = "reportTooLarge"` with integer `observedRecords`, `maxRecords`, `observedTextBytes`, and `maxTextBytes`; clients branch on the reason and camelCase fields, not prose. |
| `rateLimited` | 429 | Upstream provider quota is exhausted. Include `details.retryAfter` as a UTC RFC3339 timestamp when known. |
| `internalError` | 500 | Generic kenn-forge bug or unexpected local failure. |
| `mutationOutcomeUnknown` | 502 | A non-idempotent upstream mutation may have been applied despite transport failure. Clients fence related writes, reconcile fresh authority, and never replay the request automatically. |
| `upstreamError` | 502 | Provider API, auth, network, or upstream service failure. Include provider identity when known. |
| `hubUnavailable` | 503 | A spoke cannot reach the federation's provider owner. It is retryable; provider reads and writes must not fall back to spoke-local provider tables. |
| `serviceUnavailable` | 503 | Temporarily unavailable local service or health dependency. |

Add new codes only when the frontend or an API client needs a distinct recovery
branch. Keep the OpenAPI enum stable and regenerate API artifacts with
`make api-generate` after changing the taxonomy.

- After dispatching a non-idempotent provider write, preserve only typed errors that prove rejection;
  unverified provider failures, unreadable or oversized hub responses, and local persistence
  failures after provider success return `mutationOutcomeUnknown`
  (`internal/server/httpapi/problems.go::ProviderMutationProblem`, `internal/server/provider_proxy.go::providerProxy.ServeHTTP`).
- A stale manual-workflow definition is `conflict` with
  `details.reason = "workflow_definition_changed"` and expected/live SHAs, so
  clients can require a catalog reload (`internal/server/workflowapi/routes.go::Handler.dispatch`).

## Server Construction

`internal/server/httpapi` owns the shared HTTP problem contract. Server domain
packages must use its constructors instead of direct `huma.Error4xx` /
`huma.Error5xx` calls so status, wire code, and details stay consistent
(`internal/server/httpapi/problems.go::ProblemError`).

Huma uses Go's JSON v2 semantics for API bodies. Ordinary nil slices serialize
as `[]`, and their OpenAPI schemas are non-null arrays; use a pointer or an
explicit nullable wrapper only when `null` has domain meaning
(`internal/server/httpapi/problems.go::init`).

Optional numeric and boolean fields whose absence carries meaning need `omitzero`;
JSON v2's `omitempty` retains zero and false. Zero diff-side line numbers disable
context expansion (`internal/gitclone/types.go::Line`).

Rules for handler code:

- Validation failures use `validationError` and should name the request field.
- Provider capability gates use `unsupportedCapability`; do not hide unsupported
  mutations behind GitHub-only behavior.
- Rate-limit responses use `rateLimited` and carry a retry timestamp when a
  provider tracker or platform error exposes one.
- Not-found errors should use the most specific domain code available instead
  of the generic `notFound` fallback.
- Branch-conflict payloads put branch names in top-level `details`; do not rely
  on nested `errors[].value` payloads.
- Mid-stack merges fail closed for immediate and deferred paths unless explicitly
  enabled; return `conflict` with reason `mid_stack_merge_disallowed` and `blocking_number` (`internal/server/huma_routes.go::requireMidStackMergeAllowed`).
- Huma's `errors[]` field is reserved for Huma validation compatibility. Do not
  add new machine-readable contracts there.
- Map domain failures to statuses from typed or sentinel errors with
  `errors.Is`/`errors.As`. Matching on `err.Error()` text makes a documented
  `404` or `400` collapse into `500` the moment wording changes.

## Platform Error Translation

Translate `internal/platform` typed errors at the server boundary:

| Platform code | Wire result |
| --- | --- |
| `unsupported_capability` | `409 unsupportedCapability` |
| `stale_state` | `409 conflict` |
| `conflict` | `409 conflict` |
| `rate_limited` | `429 rateLimited` |
| `permission_denied` | `403 forbidden` |
| `not_found` | `404 notFound`, or a more specific not-found code when the caller knows the resource type |
| `provider_not_configured`, `missing_token`, `invalid_repo_ref`, `invalid_argument` | `400 badRequest` |
| Unknown provider/platform failures | `502 upstreamError` |

Cancellation and deadline errors pass through only when the request context is
done; a provider child-context deadline while the request remains active is a
`502 upstreamError` (`internal/server/markdown_images.go::markdownImageError`).

## Frontend Handling

Generated TypeScript schemas should expose the problem `code` enum. Shared UI
helpers should provide:

- an `isProblem(value)` type guard;
- typed accessors for common `details` members such as `capability` and
  `retryAfter`.

Components may still display `detail` as user-facing text, but behavior must use
the typed code. Examples: disable or explain unavailable provider operations
from `unsupportedCapability`, and show retry timing from `rateLimited`.

On a federation spoke, hub reachability is state carried on the existing
local browser event stream as `hub_connection_changed`; it is not a
second EventSource and is distinct from local live-update connectivity. A
disconnect marks provider data unavailable immediately. Reconnection refreshes
pull, issue, activity, selected-detail, and sync-status projections before
clearing that state. If a projection refresh fails but an independent
sync-status probe succeeds, the hub is available and the projection error is
reported separately; a failed probe keeps provider data unavailable. A stale
replay cursor follows the same rule; cached hub state suppresses refetch during
a known outage.
Provider pages must not present stale cached state as current during the outage,
while local workspace navigation remains available
(`frontend/src/lib/app-stores.svelte.ts::reconcileProviderState`,
`frontend/src/App.svelte::providerUnavailable`).

## Tests

Use wire-level server tests with real SQLite for API error contracts. Coverage
should assert status, content type, top-level `code`, and relevant `details`.
At minimum, protect:

- `unsupportedCapability` through a provider capability-gated mutation;
- `rateLimited` through a fake provider/platform error with a reset time;
- `validationError` through a request with an invalid enum or blank required
  field.

Tests should not assert on human-readable prose unless the prose itself is the
feature under test. Run Go tests with `-shuffle=on`; use generated clients for
integration-style API tests when practical.
