# Platform Sync Invariants

Use this document for changes that touch provider-aware repository identity,
sync, import, server routes, settings, or API responses. For package layout,
provider interfaces, and the checklist for adding a new provider, read
[`context/provider-architecture.md`](./provider-architecture.md) first.

## Identity

Provider-verified repository identity is
`(platform, platform_host, platform_repo_id)`. Owner, name, and `repo_path` are
mutable routes; route reuse must create a distinct catalog entry rather than
combining repository-owned history
(`internal/db/repository_catalog.go::ReconcileRepositoryObservation`).

- Saved repository-filter presets resolve by stable identity and use `repo_path` only for display; reject unverified members and never fall back to a new occupant of the stored route (`internal/config/config.go::RepoPresetRepository`, `frontend/src/lib/stores/repo-presets.ts`).

- `platform` is the provider kind named in the canonical provider list in
  `CLAUDE.md`.
- `platform_host` is the normalized host for that provider. Preserve ports.
- Gitea `base_url` changes API transport only; `platform_host` remains identity, and repository reads plus every authenticated Git path reject mismatched or unacknowledged plain-HTTP clone URLs (`internal/platform/gitea/client.go::validateRepositoryCloneURL`, `internal/gitclone/clone.go::validateRemoteTransport`).
- `owner` and `name` are provider-canonical display/config fields.
- `repo_path` carries the full provider path when `owner/name` is not enough.
- `platform_repo_id` / provider external IDs are stable provider identities;
  preserve human-readable route history across renames and replacements.
- Exact configured repositories with a stable provider ID never adopt a new
  occupant of their saved route; startup recovers that ID's current catalog
  route or fails closed (`cmd/kenn-forge/main.go::fallbackExactFromDB`).
- Rows without a verified provider ID remain inactive legacy records. Never
  infer their identity from a matching route.
- Timestamp provider observations before the identity lookup starts; a delayed
  response must not supersede a newer route observation
  (`internal/github/sync.go::Syncer.syncRepoIdentity`).
- Asynchronous provider responses fetched through a mutable route must fence the
  exact ownership generation before snapshot, notification, or cache commits;
  same-identity freshness observations do not advance that generation
  (`internal/db/repository_catalog.go::RepositoryRouteFence`).
- Workflow reads and dispatch fence route ownership after live provider work;
  dispatch also re-reads definition state before its single write, so stale UI
  authority cannot cross revisions (`internal/server/workflowapi/routes.go::Handler.dispatch`).
- Hub descriptors are provider observations and use the same
  reconciliation path as sync, enrollment, and project discovery. Spokes must
  preserve stable provider identity and A-to-B-to-A route generations; they may
  not create a catalog row from owner/name alone. Descriptor metadata writes
  remain fenced to the descriptor's observation time
  (`internal/server/provider_sources.go::hubProviderSource.observeRepositoryDescriptor`).
- Parallel reads may receive same-identity, same-route descriptors out of order;
  superseded metadata is skipped but the read remains usable. Identity or route
  changes still fail closed (`internal/server/provider_sources.go::observeRepositoryDescriptor`).
- Repository provider metadata and merge settings have a single sync-path
  writer: commits go through the observation watermark so a delayed snapshot
  never overwrites a newer same-route observation, and reconciled direct item
  syncs persist their verified snapshot so a replacement repository row never
  serves permissive schema defaults. Snapshot fields a provider omits must not
  erase stored metadata — clone URLs seeded outside the provider resolve
  clones — and permission-sensitive merge settings are authoritative only when
  the provider returns the complete settings group; omission retains stored
  values, while a later complete observation repairs them. A failed settings
  write stops the item sync instead of
  populating a row that still advertises default merge availability
  (`internal/db/repository_catalog.go::UpdateRepoProviderObservation`).
  Newly verified or legacy-adopted rows fail closed when viewer merge permission
  is omitted; only an already verified row may preserve its stored permission
  (`internal/db/repository_catalog.go::ReconcileRepositoryObservation`).
  Concurrent identity-only observations (the archive lifecycle re-reconciles
  tracked repositories during route changes and first encounters) advance the
  watermark without writing settings, so a direct sync whose snapshot loses
  the watermark must re-resolve rather than proceed unsettled
  (`internal/github/sync.go::Syncer.reconcileRepoForDirectSync`). Periodic
  repository sync holds the same line: an uncommitted settings refresh aborts
  the sync before item indexing and records the aborted attempt on the
  repository row's sync health; only a provider that cannot report settings
  is a silent skip (`internal/github/sync.go::Syncer.refreshRepoSettings`,
  `internal/github/sync.go::Syncer.recordAbortedRepoSync`).
- Assigning a historically occupied route to another verified repository clears
  route-scoped state even without an active occupant; migration 47 applies the same
  cleanup to preexisting reuse, while legacy adoption remains in-place (`internal/db/repository_catalog.go::ReconcileRepositoryObservation`, `internal/db/migrations/000047_repository_route_generation.up.sql:1`).
- Hidden-from-UI is an operator preference keyed by the stable internal catalog
  row, never by mutable config routes — renames keep it, route reuse must not
  inherit it, and only exact configured repositories can be hidden. Correlating
  hidden rows with configured entries and resolving visibility mutations must
  also use the stable provider id, not route keys: a displaced row keeps its
  old display route. Mutations resolve, validate lifecycle, and write in one
  critical section under the repository-reconciliation read lock, rejecting
  non-active rows — a tracked snapshot lagging reconciliation still names the
  displaced repository
  (`internal/server/settings_handlers.go::applyVisibilityUnderReconciliationRead`).
  Removing the last exact entry for a repository releases its hidden
  preference on every configuration-change path — startup, TOML hot reload,
  and the DELETE handler (detached from request cancellation): globs can keep
  the repository tracked but expose no visibility controls
  (`internal/server/settings_handlers.go::reconcileOrphanedRepoVisibility`).
  Visibility mutations serialize with that sweep on a shared lock and
  revalidate exact-entry membership inside it, so a mutation racing an
  exact-entry removal cannot recreate an orphaned preference
  (`internal/server/settings_handlers.go::updateConfiguredRepoUIVisibilityState`). Filtering is
  server-owned and scoped to interactive repository catalogs; item feeds, direct
  routes, and the settings surface stay unfiltered
  (`internal/server/helpers.go::filterHiddenRepos`).

GitLab nested namespaces make `repo_path` mandatory for reliable addressing:
`group/subgroup/project` has owner `group/subgroup` and name `project`.
GitHub repositories can continue to omit `repo_path` when the path is exactly
`owner/name`.

Forgejo and Gitea use GitHub-like two-segment repository paths. Preserve
provider-canonical repository-path casing unless provider metadata explicitly
opts into case folding (`internal/platform/metadata.go::LowercaseRepoNames`).
`repo_path` is normally `owner/name` and is primarily a canonicalization aid for
URL-parsed config or provider responses.

Scope repository-owned data by internal repository ID; route-facing requests
still carry the full provider ref. New provider workspaces persist that internal
ID, so renames follow the same repository and a replacement repository can use
the old route without inheriting workspaces. Unresolved legacy workspaces and
route-only Git operations still fail closed once a route has historical
occupants (`internal/db/queries.go::DB.workspaceRouteHasHistoricalOccupants`,
`internal/workspace/manager.go::Manager.verifyWorkspaceRepository`).

## Provider Hosts And Tokens

Each configured provider-host pair may have its own fallback token source;
providers sharing one hostname may carry different chains. When their chains
disagree, the ownerless host clone fallback is disabled rather than borrowing
one provider's credential, because an ownerless operation cannot select a
provider safely; providers with no credential chain of their own do not veto
the fallback, and runtime route resolution must honor the disabled state
instead of falling through to another provider's unscoped route
(`internal/config/config.go::Config.CloneTokenDescriptors`,
`cmd/kenn-forge/provider_startup.go::providerStartup.FallbackSource`).
Non-GitHub repositories on one (provider, host) must declare equivalent
effective chains, checked against each repository's own descriptor —
`ProviderTokenSources` deduplicates by key and would hide the conflict
(`internal/config/config.go::Config.ValidateRepoTokenSourceConsistency`).
GitHub may additionally define exact-repository and owner authorization routes.

- Self-hosted hosts are hostnames with optional ports, not URL paths.
- A missing token should fail only the provider host that needs it.
- Reload credential probes cover only provider hosts registered at startup, so
  a degraded host cannot block unrelated config changes
  (`internal/server/config_reload.go::Server.validateReloadProviderTokenSources`).
- Reloads retain boot-tracked exact and glob matches for degraded hosts; new
  unavailable hosts remain untracked until restart
  (`internal/server/config_reload.go::Server.resolveReposForReload`).
- Disabled startup registers credential descriptors without resolving them, so
  provider tokens stay lazy (`cmd/kenn-forge/provider_startup.go::registerProviderTokenSources`).
- Refresh access uses the gated registry; only explicit foreground provider
  operations use direct access (`internal/github/sync.go::Syncer.SyncRegistry`).
- Refresh interfaces that may be retained across policy setup must recheck the
  gate when invoked, not only when resolved
  (`internal/github/sync.go::Syncer.MergeRequestReviewThreadReader`).

Provider clients must be registered by `(platform, platform_host)`. GitHub rate
trackers and sync budgets are keyed by `(host, authenticated identity)`: PATs
resolve to `user:<numeric-id>` and App reads use `installation:<id>`. PAT routes
for the same user share trackers and one budget. Non-GitHub providers remain
host-scoped unless their provider model later proves otherwise. Startup also
builds clone routes, GitHub GraphQL fetchers where applicable, and a
`platform.Registry`. A third provider should add metadata, a factory, and an
implementation; it should not masquerade as another provider.

- Authenticated-viewer lookups run only for repositories selected by the request.
  Include only repositories with a resolved identity, cache successes for one hour by
  effective credential with stale-while-refresh, retry failures, and keep unkeyed GitHub routes repository-scoped
  (`internal/server/viewer_identity.go::Server.resolveAuthenticatedViewerLogins`).

Fallback token lookup is scoped by `(provider, platform_host)`. GitHub
authorization routes may be exact-repository, owner, or host fallback. Lookup
checks repo `token_file`, repo `token_env`, a covered App installation for
reads, owner PAT, platform token, public-host default, then GitHub CLI. GitLab
`gitlab.com` has no implicit default env var. Forgejo `codeberg.org` uses
`KENN_FORGE_FORGEJO_TOKEN`, and Gitea `gitea.com` uses `KENN_FORGE_GITEA_TOKEN`.
Token files are read lazily so atomic replacement rotates credentials without
rebuilding provider clients. Route descriptors and auth transports stay keyed
by full route, while GitHub App installation-token caches are shared by
canonical App credential across routes; reload probes must reuse that shared
cache (`internal/tokenauth/source.go::githubAppTokenStore`,
`internal/tokenauth/source.go::SourceSet.ProbeToken`).

GitHub CLI fallback is always requested with `gh auth token --hostname HOST`.
Only the default `github.com` host may retry bare `gh auth token` for an older
CLI that does not support `--hostname`; retrying bare for another host could
silently authenticate to the wrong account
(`internal/config/config.go::ghAuthTokenForHost`).

Managed Git authorization is selected by full `(platform, platform_host, owner,
name)` identity. GitHub smart HTTP uses mutation/user candidates and never an
App installation token. Exact repository routes beat owner routes, which beat
the unscoped host fallback. Non-GitHub providers use their provider-host
fallback. Clone storage and clone-coordination keys must partition providers sharing a host and
distinct stable repository IDs sharing mutable routes
(`internal/gitclone/clone.go::WithRepositoryIdentity`). Authenticated workspace operations
must validate the effective origin fetch and push destinations before resolving a credential.
Pre-stable-ID clone adoption stays offline and requires a catalog-verified stable owner with
no other route history plus a matching stored origin (`internal/db/repository_catalog.go::DB.AdoptLegacyClonesIfSafe`).
Stable main storage is an independent copy while the workspace path-scoped main clone remains
(`internal/gitclone/repo_browser.go::Manager.AdoptLegacyClones`).
Federation-spoke clone work first validates the descriptor remote against that
exact identity, then requires the executing spoke's exact credential route.
Credential-required contexts never fall back to anonymous Git if the route or
token disappears; public standalone clone behavior remains unchanged. Missing
spoke credentials return the typed `gitCredentialUnavailable` problem
(`internal/gitclone/clone.go::WithRequiredCredential`,
`internal/workspace/manager.go::Manager.SetRequireProviderCredential`,
`internal/server/httpapi/problems.go::GitCredentialUnavailable`).
Repository-browser refreshes fetch only into unpublished same-filesystem staging and publish with a
route-generation guard plus reader-exclusive rename swap; failures retain the prior clone. Current
waiters retry only after stale staging cleanup (`internal/gitclone/repo_browser.go::refreshRepoBrowserClone`).

Minimum read scope should cover repository metadata, merge requests or pull
requests, issues, comments, commits, tags, releases, and CI/status data. Write
scopes are only required for mutation capabilities: comments, issue creation,
issue or PR content/state changes, merge, review approval, workflow approval,
review suggestion application, or ready-for-review.

## Sync Capabilities

kenn-forge reads repositories, merge requests, issues, releases, tags, CI, and
timeline/comment-like events through provider capability interfaces in
`internal/platform`. Providers implement only supported optional interfaces;
registry helpers return typed errors for missing providers or capabilities.

- A PR reference is historical evidence that a provider observed a pull or merge
  request cross-reference to an issue. It survives upstream mention removal and
  does not depend on repository tracking, repository boundary, or PR lifecycle.
  (`internal/db/queries.go::upsertIssueEventsTx`)
- Persist normalized PR references and query that graph for list filters. A missing
  edge means no reference; never parse Markdown during list reads.
  (`internal/db/queries.go::DB.ListIssues`)
- Each edge keeps provider-supplied source identity and URL without requiring a
  tracked source repository or local PR row.
  (`internal/db/migrations/000052_issue_pr_references.up.sql:1`)
- Ingest provider-native issue-event payloads and backfill stored events. GitHub
  uses cross-reference timeline events, GitLab uses related merge requests, and
  Gitea/Forgejo use `pull_ref` timeline events. Never scan Markdown or add reads
  while serving issue lists.
  (`internal/db/queries.go::issuePRReferenceFromEvent`)
- Historical references cannot prove a current issue-PR relationship. A current-link
  capability requires an authoritative replaceable provider snapshot.

- Missing optional capabilities should degrade that feature with a typed
  platform error, not break unrelated sync work.
- Never put foreground deadlines on a shared provider HTTP client; scope them to
  the operation context (`internal/platform/gitlab/client.go::NewClient`).
- Provider clients with a local sync budget must use the shared transport; duplicate
  wrappers can lose refusal-window identity and make sync status provider-dependent
  (`internal/github/budget_transport.go::WrapSyncBudgetTransport`).
- Resolve opaque provider repo IDs by `repo_path` before numeric-only operations
  (`internal/platform/gitlab/client.go::projectScopedArg`).
- `priority_repo` reorders a full run; `only_repo` restricts every repo-derived
  phase and must not delay full-run cadence. Resolve both by full identity, and
  never fall back from invalid exclusive scope. (`internal/github/sync.go::runOnce`)
- Every issue or merge-request read boundary that can receive a disabled-feature
  candidate must route through definitive classification: GitHub's disabled 410,
  or GitLab/Gitea/Forgejo repository metadata confirmation. (`internal/platform/gitlab/feature_disabled.go::Client.repositoryFeatureError`, `internal/platform/gitealike/feature_disabled.go::Provider.ClassifyRepositoryFeatureError`)
- Locked pull requests retain their upstream state for detail and sync, but list
  filters and repository counts classify them as closed because they cannot enter
  an open interaction workflow; detail presents only the Locked chip, not the
  upstream Open chip. (`internal/db/queries.go::DB.ListMergeRequests`, `frontend/src/lib/components/detail/PullDetail.svelte`)
- Mutation routes must check provider capabilities before posting comments,
  changing state, merging, requesting review, or approving workflows.
  Server handlers translate these typed platform errors into the stable problem
  envelope described in [`context/error-handling.md`](./error-handling.md).
- Review suggestion application is a provider capability
  (`review_suggestion_application` + `mutation_head_binding` +
  `read_review_threads`, via
  `internal/platform/client.go::ReviewSuggestionApplier`): an all-or-nothing
  batch apply against the expected head. Partial success is invalid.
- The server rebuilds each suggestion from persisted review-thread metadata and
  only accepts replacement text matching a stored suggestion fence (opaque
  non-suggestion fences skipped, fences may be indented up to three spaces,
  CRLF normalized to LF, no other rewriting; UI preview mirrors this). Never
  trust clients for ids, ranges, paths, or patch content
  (`internal/server/pullapi/diff_review_handlers.go::Handler.applyReviewSuggestions`).
- Providers report every upstream bucket the apply consumes through
  `OperationRateLimitReporter`; an empty or unknown report fails closed as
  `rate_limited`, and mutation handlers re-check buckets immediately before the
  provider call because UI availability goes stale.
- Suggestion apply mutates the source branch, is open-PR-only, and fails closed
  with stable reasons: non-open rows and upstream closed/merged races →
  `not_open`; missing, unparseable, or inaccessible head repo identity →
  `head_repo_unknown` (never fall back to the base repo as a write target;
  GitHub is currently the only provider needing an explicit source repo
  target); live branch or SHA movement → `stale_state`. The live re-check
  before mutation is best-effort — no provider offers commit-only-if-open — so
  expected-head binding is the final integrity check
  (`internal/server/pullapi/diff_review_handlers.go::Handler.applyReviewSuggestions`, `internal/github/client.go::ensureReviewSuggestionPullMutable`).
- Post-apply refresh goes through the detail-sync broadcaster and must rerun
  after any in-flight sync for the same PR — that sync may predate the commit
  (`internal/server/detail_sync.go::enqueueDetailSyncOrRerun`).
- Unexpected or ambiguous mutation outcomes trigger an immediate best-effort
  authoritative detail refresh; ordinary periodic sync is the eventual recovery
  path if that refresh fails or still observes stale provider state. kenn-forge
  must not persist a local mutation fence that can indefinitely block future
  actions. Provider snapshots use ordinary timestamp ordering, and expected-head
  binding remains the mutation integrity boundary.
- Background and watched GitHub detail syncs may use the persisted PR ETag; a
  normal 304 marks detail fetched and may refresh pending CI. Explicit
  `SyncMROnProvider` refreshes bypass the PR ETag.
- The UI only exposes apply actions when the thread head matches a known
  current PR head; stale or unknown heads disable actions, and stale batched
  suggestions must not reach batch submit while staying removable. A suggestion
  preview built from a cached diff whose `diff_head_sha` no longer matches the
  current head is treated as missing context (single apply and batch submit
  disabled) until reloaded. PR diff responses always carry `diff_head_sha` —
  the endpoint fails instead of serving a PR diff without a synced snapshot
  head — so clients only need to handle the mismatch case
  (`frontend/src/lib/components/detail/ReviewSuggestionBlock.svelte`, `frontend/src/lib/components/detail/EventTimeline.svelte`).
- Remaining edge cases (duplicate thread ids, overlapping ranges, missing
  reviewed heads, renamed/deleted/binary files, unsupported encodings,
  whitespace-padded paths) fail the entire batch with stable reasons.
- Forgejo and Gitea currently expose only SDK-proven mutations: comments,
  issue creation, issue and PR content/state edits, merge, review approval, and
  request changes. Their shared review operations must map SDK failures through
  the provider error mapper so reads and mutations retain typed platform errors.
  Workflow approval and ready-for-review must remain hidden or return typed
  `unsupported_capability` errors until proven per provider.
- Gitea 1.24 timeline responses encode `label` as one object while the SDK expects an array; normalize that field at the HTTP boundary so detail sync remains usable at the supported version floor. (`internal/platform/gitea/timeline_transport.go::timelineLabelTransport`)
- GitHub GraphQL bulk fetch, ETag recovery, and detailed diff behavior are
  GitHub-only optimizations. Keep them optional around the neutral persistence
  path.
- Provider-supplied web URLs, clone URLs, default branches, platform ids, and
  external ids should be persisted when available instead of reconstructed from
  host/owner/name. Every settings-refresh branch must write this metadata:
  repo resolution pre-fills the platform repo id, so the identity sync never
  re-resolves the repository and whichever refresh branch runs is the row's
  only metadata writer. A branch that skips the write leaves default_branch
  empty forever, which silently degrades the worktree diff sampler to a bare
  HEAD diff (0/0 sidebar stats).
- Child datasets and detail/CI/diff freshness writes are fenced to the parent snapshot revision. Complete comments and inline review sets replace; submitted reviews remain additive. (`internal/db/queries_snapshot_children.go::CommitMergeRequestChildSnapshot`)
- Merge-request assignee omission remains unknown; only a provider-confirmed empty set counts as unassigned, so incomplete snapshots cannot claim that an item has no owner. (`internal/platform/persist.go::MarshalUserNamesJSON`, `internal/db/queries_assignees.go::unassignedCondition`)

## Historical Archive

- Archive is a scheduling and progress mode over normal sync, not a second sync engine; completeness is repository and item progress scoped by full repository identity. (`internal/db/queries_archive.go::GetArchiveProgress`)
- Created-order inventory calls require the historical capability; updated-order maintenance traversal does not. Each returns one bounded identity page with an advancing opaque cursor or explicit exhaustion. (`internal/platform/reader_validation.go::pageReaderValidation.prepare`)
- Hydration admits one item and invokes canonical item sync; only a successful complete
  sync records an archive outcome, and provider finalizers run after that commit so they
  observe lifecycle reactivation. (`internal/archive/hydrate.go::hydrateItem`)
- Only parent lookups explicitly classified as removed, moved, or inaccessible are terminal. Generic and child-dataset not-found responses remain retries; a successful non-GitHub feature-metadata confirmation doubles as repository-accessibility evidence and must not be repeated before marking the parent absent. Canonical item content stays untouched. (`internal/archive/hydrate.go::archiveTerminalSyncOutcome`, `internal/platform/gitealike/feature_disabled.go::Provider.repositoryItemLookupError`,
  `internal/platform/gitlab/feature_disabled.go::Client.repositoryItemLookupError`)
- Removed-upstream parents stay stored for rediscovery but are excluded from public
  item list/detail reads and archive reports; inaccessible rows remain visible because
  authorization loss does not prove removal. (`internal/db/queries_archive_report.go::archiveReportActivityQuery`)
- Maintenance rediscovery reopens terminal item progress for hydration; unsupported and blocked items remain excluded. (`internal/db/queries_dataset_progress.go::reopenArchiveItemProgressTx`)
- Issue and merge-request inventory coverage is explicit and independent from child-dataset coverage. Exhausted supported scans record `supported`; declared or repository-specific feature absence records `unsupported` and completes only that stream. (`internal/archive/inventory.go::inventoryPage`, `internal/db/queries_archive.go::CommitArchiveInventoryPage`)
- Repository-specific feature absence also exhausts the current maintenance stream without replacing established historical coverage. (`internal/archive/maintenance.go::promptPages`)
- Capability reconciliation reopens an unsupported inventory with unknown
  coverage and requeues completed known-item lookups in a fresh generation when
  the provider advertises that stream again. (`internal/db/queries_archive.go::ReconcileArchiveCoverage`)
- A bare optional `DiffSyncError` does not block archive historical-activity completion; wrapped or joined hard failures still retry. (`internal/github/sync.go::SyncArchiveItem`)
- Every configured repository starts provider-neutral discovery; do not restore provider-specific closed-item cursors or translate legacy formats. (`internal/archive/service.go::EnsureConfigured`)
- Full-archive promotion keeps the discovery creation time as the first maintenance
  boundary; using the later promotion time can skip items updated after inventory
  completed. (`internal/db/queries_archive.go::StartFullArchives`)
- Configuration reconciliation pauses omitted repositories with a durable `configuration_removed` reason while retaining archive content and progress. Re-adding the same full identity clears only that automatic pause; an operator pause stays paused. (`internal/db/queries_archive.go::ReconcileDiscoveryArchives`, `internal/db/queries_archive.go::EnsureDiscoveryArchives`)
- Reconcile configured repositories only at startup or configuration reload; idle scheduler passes must remain read-only unless they claim actual work. (`internal/github/sync.go::SetReposWithContext`, `internal/archive/scheduler.go::RunPass`)
- The worker caches resolved repositories keyed on configured refs plus the store's repository reconciliation generation, which every catalog write advances, so an unchanged pass runs no resolution queries. (`internal/archive/service.go::workerRepositories`)
- Startup reconciliation reopens completed legacy known-item lookups once when
  lifecycle details are missing, independent of historical inventory coverage;
  a close actor is current only when the latest authored close event matches
  `closed_at`. (`internal/db/queries_archive.go::RequeueArchiveLifecycleDetails`)
- Successful canonical hydration durably marks lifecycle details checked only
  after provider-specific completeness gates pass; providers that define absent
  fields as unavailable may still be checked once.
  (`internal/archive/hydrate.go::hydrateItem`,
  `internal/db/queries_dataset_progress.go::CommitArchiveItemSync`)
- Authentication and repository-blocked errors defer every archive work class, including already-pending hydration, until an explicit retry clears the repository error. (`internal/archive/scheduler.go::archiveRepoDeferred`)
- Archive admission is provider-host scoped. Normal index, notification, and active-detail work outrank archive requests; live work registers first, cancels the active archive request context, and waits for that request lease to release before provider I/O. Archive leases are released before SQLite commits. (`internal/github/sync.go::beginProviderWork`, `internal/github/sync.go::tryBeginArchiveProviderRequest`, `internal/github/sync.go::Admit`, `internal/archive/scheduler.go::admit`)
- Split-auth live work holds every read/write credential identity it may spend, including identities selected after route reconciliation. Register work inside shared execution functions so no entry point bypasses admission. (`internal/github/notifications_sync.go::notificationProviderWork`)
- Archive admission requires the declared minimum cost and normally bounds wire attempts above the live floor. Gitealike merge-request reads remain preemptible but may exceed the estimate to complete their atomic dataset. (`internal/github/budget.go::LocalArchiveSpendAvailable`,
  `internal/archive/scheduler.go::archiveFeatureReadAttemptCost`, `internal/github/sync.go::Admit`)
- Detailed reports use schema `kenn-forge-archive-report/1` and half-open UTC windows. They expose opened items, current close/merge lifecycle projections, comments, reviews, and inline comments; a reopened issue has no close row. Issue close actors must match the current `closed_at`; merge metrics come from the normalized merge-request row. Daemon CLI JSON must reject any other schema before conversion and round-trip every recognized report kind and field. (`internal/db/queries_archive_report.go::archiveReportActivityQuery`, `cmd/kenn-forge/archive_cli.go::archiveReportFromAPI`)

## Label Catalogs And Mutations

Label picker data comes from a cached repo catalog, not from labels currently
assigned to visible items. Catalog refresh marks which labels are currently
selectable while preserving historical assigned labels until item sync removes
them. Stale `GET repo labels` responses should return cached labels immediately
and enqueue a deduped background refresh with `syncing=true`; catalog errors are
repo metadata and must not fail normal PR/issue sync.

Label mutations replace the full desired label name set. Server handlers must
check `read_labels` and `label_mutation`, reject missing/null/empty/duplicate or
non-catalog names, call the provider mutator first, then persist the returned
provider labels to SQLite so the next sync does not revert the edit.

## Provider Event Mutations

Comment deletion is provider-authoritative: validate the parent identity, call
the provider first, and retain the SQLite event until authoritative detail sync
removes it. Provider rejection must leave local state intact
(`internal/server/pullapi/routes.go::Handler.deleteComment`,
`internal/server/issueapi/mutation_handlers.go::Handler.deleteIssueComment`).

Direct event URLs are provider data. Preserve a stored URL when a partial
response omits it. GitLab Notes are the explicit exception: derive
`parent URL + #note_<id>` because Notes do not expose a browser URL
(`internal/db/queries.go::DB.UpsertMREvents`,
`internal/db/queries.go::DB.UpsertIssueEvents`,
`internal/platform/gitlab/normalize.go::noteDirectURL`).

## GitLab Shape

GitLab note IDs identify comments; discussion IDs identify reply and resolution
targets. Reads supporting threaded mutations must preserve Discussions
grouping rather than flattening it into Notes
(`internal/platform/gitlab/pages.go::Client.listMergeRequestDiscussionsPage`,
`internal/platform/gitlab/normalize.go::NormalizeMergeRequestDiscussions`).

GitLab API calls address projects by numeric id or URL-escaped path with
slashes. kenn-forge should prefer the stored provider id after resolution and
preserve `path_with_namespace` as `repo_path`.

GitLab private Markdown upload web URLs do not accept API-token authentication.
Translate only repo-scoped upload URLs to the authenticated project-upload API;
never proxy arbitrary provider URLs. (`internal/platform/gitlab/markdown_images.go::GetMarkdownImage`)

The markdown image cache is keyed by stable repository identity, never the owner/name
route, so a replacement occupant of a reused route cannot receive the previous
repository's bytes; providers mark ref-addressed sources `Mutable` and the server then
caches them for minutes instead of a year (`internal/server/markdown_images.go::markdownImageCacheKey`).

GitLab merge request and issue `iid` values are repo-scoped numbers. Persist
provider object ids separately from user-visible numbers, and scope events by
provider identity so equal GitHub/GitLab ids do not collide.

GitLab archive discussions normalize as ordinary or inline comments. Do not synthesize submitted reviews from notes or current approvals; without stable historical actions, that dataset stays unsupported and coverage stays partial. (`internal/platform/gitlab/client.go::Capabilities`)

GitLab issue event hydration preserves authored close and reopen system notes, while ordinary-comment reads exclude system notes. (`internal/platform/gitlab/client.go::Client.ListIssueEvents`, `internal/platform/gitlab/normalize.go::normalizeIssueSystemNote`)

GitLab historical merge-request inventory is unsupported because project merge requests expose only offset pagination and cannot guarantee completeness across equal-`created_at` ties. Coverage remains partial while supported issue history and discussion datasets continue. (`internal/platform/gitlab/client.go::Capabilities`, `internal/platform/gitlab/pages.go::ListMergeRequestsPage`)

GitLab maintenance inventories walk mutable `updated_at` results newest-first. Updates then move toward the consumed prefix; rows that move ahead before consumption remain eligible under the next scan's inclusive watermark. (`internal/platform/gitlab/pages.go::listInventoryIssuesPage`, `internal/platform/gitlab/pages.go::listInventoryMergeRequestsPage`)

GitLab merge reports use `merge_commit_sha`, falling back to `squash_commit_sha` when no merge commit exists. (`internal/platform/gitlab/normalize.go::normalizeMergeRequest`)

## Forgejo And Gitea Shape

Forgejo and Gitea use owner/name repository addressing in the REST and SDK
surfaces. kenn-forge should still persist provider repo IDs and external object
IDs when available, but route and config identity should remain
`(provider, host, owner, name)` with optional `repo_path` for canonical display.

Codeberg is Forgejo's public host. gitea.com is Gitea's public host.
Self-hosted Forgejo and Gitea instances are separate provider-host entries even
when they have the same owner/name pairs as public repos.

Forgejo pull-request JSON is authoritative for merge metrics: its SDK drops
those fields, so raw-response capture must preserve values and field presence
with head/base binding before neutral normalization; omitted counters preserve
stored values while explicit zero replaces them
(`internal/platform/gitealike/mergeable_capture.go::MetricsForPullRequest`).

Actions/CI parity is provider-specific. Forgejo reads Actions runs through the
shared gitealike provider. Gitea reads repository workflow runs only when the
pinned SDK and server version expose the Gitea 1.26+ `/actions/runs` API; older
Gitea hosts stay status-only. Gitealike CI normalization must merge commit
statuses and Actions runs without duplicating a check already represented by
the status endpoint. Neither Forgejo nor Gitea should claim workflow approval or
ready-for-review support unless the provider interface and server/UI capability
tests prove those exact operations.

Archive access probes preserve transient and rate-limit failures for retry. Only
authoritative repository permission or not-found responses may become permanent
inaccessibility. (`internal/platform/gitealike/pages.go::classifyLookupOutcome`)

Inline review hydration is complete-or-error: drain every review page and every
per-review comment before revision-fenced dataset replacement. Never publish a
partial review dataset or report an incomplete explicit sync as successful
(`internal/github/sync.go::syncProviderMRReviewThreads`).

## Import And Routes

Repository import requests and route/query shapes should carry
`provider`, `platform_host`, and either `repo_path` or exact `owner/name`.

- Provider-aware item routes use `/pulls/{provider}/{owner}/{name}/{number}`,
  `/issues/{provider}/{owner}/{name}/{number}`, and `/repo/{provider}/{owner}/{name}`.
- Non-default hosts use the `/host/{platform_host}/...` route prefix.
- Do not add new `/repos/{owner}/{name}/pulls/{number}/...` compatibility
  routes for diff, files, commits, file preview, or other repo-scoped provider
  work. Route through the provider-aware generated clients instead.
- Frontend route state must encode slashes, host ports, mixed case, and special
  characters exactly once, via shared provider route helpers.
- New provider-aware routes should not require ad hoc URL construction in
  stores/components.
- Archive items marked `removed_upstream` remain stored for sync repair but are
  hidden from public item reads, resolution, autocomplete, stacks, activity,
  workspace creation and subject metadata, project worktree imports, Kata
  links, repository summaries, archive reports, and provider mutations;
  synchronous and queued public item sync rejects known tombstones before
  provider access, with queued work rechecking visibility when it executes.
  Watched fast-sync and priority detail-drain work perform that recheck after
  stable repository resolution; archive-budget hydration bypasses it so a
  terminal item can be repaired when it reappears upstream. Deferred PR and
  issue comment drains recheck their parent before provider access, and
  workspace setup rechecks its source before any Git or provider access.
  Periodic repository sync excludes tombstones from closed-item and
  merged-actor repair candidates, then rechecks before each provider fetch.
  Secondary workflows such as label validation, automatic assignment,
  pushed-head refresh, manual workspace refresh, and on-demand worktree sync
  must check item visibility before item-specific provider access. Workspace and
  Fleet projections retain local records but omit removed parent metadata;
  visible stack members are renumbered contiguously after filtering, and the
  pull list's per-row stack placement must report the same position and size
  as the detail stack context.
  `inaccessible` items remain visible.
  (`internal/server/pullapi/helpers.go::visibleMergeRequest`,
  `internal/server/issueapi/mutation_handlers.go::requireVisibleIssue`,
  `internal/db/queries_stacks.go::ListStackPlacementsForMRs`)
- Embedded navigation events for repo-bound routes must publish identity from
  parsed route state, not from global embed config. When a route carries repo
  identity, event payloads should include `provider`, `platform_host`, and
  `repo_path` and may keep `repo` as the display/canonical path. Global
  `ui.repo` config is only a fallback for non-repo-bound pages.

## Testing

Provider work should be covered at the boundary where a regression would show:

- provider package tests for API normalization, pagination, auth/header shape,
  and capability errors;
- config tests for provider defaults, host normalization, duplicate detection,
  and token/env selection;
- sync tests for full provider refs, optional capability behavior, and DB
  identity scoping;
- server e2e tests with real SQLite for API payloads, route shape,
  capability-gated actions, and settings/import flows;
- frontend store/component tests for provider route helpers and provider refs;
- optional live/container tests for provider API compatibility when fakes are
  too weak to catch endpoint or auth drift.

Run Go tests with `-shuffle=on`. Use the GitLab CE container fixture for
changes that need real GitLab REST behavior. Use the optional Forgejo/Gitea
container fixtures when fake transports are too weak to prove gitealike REST
behavior.
