# GitHub Sync Invariants

Use this document for changes in `internal/github/`, GitHub adapter code,
sync-triggering server handlers, fixture clients, and tests that rely on
GitHub-derived freshness. For provider-neutral identity rules, package layout,
and provider capability rules, start with
[`context/platform-sync-invariants.md`](./platform-sync-invariants.md) and
[`context/provider-architecture.md`](./provider-architecture.md).

## Purpose

- Keep sync correctness rules explicit.
- Preserve the distinction between identity, freshness, and optional fallback
  data.
- Prevent review-only regressions around `platform_host`, head-SHA drift,
  timeline parity, and fallback fetch paths.

GitHub API clients, sync, quota tracking, and mutations exist only on a
hub; spokes retain lazy smart-HTTP Git credentials only. `--disable-sync`
suppresses hub refresh without removing foreground provider operations
(`cmd/kenn-forge/provider_startup.go::buildServeControlPlanes`).

## Identity Rules

GitHub entities in kenn-forge are not identified by owner/name/number alone.
The provider-neutral identity is `(platform, platform_host, owner, name)`;
this document focuses on GitHub-specific default-host behavior and GitHub-only
sync optimizations.

- Repository identity is `(github, platform_host, owner, name)`.
- PR and issue identity is `(github, platform_host, owner, name, number)`.
- Workspace association repair and list filtering must preserve that
  provider/host-aware identity.
- GitHub owner/name are case-folded lookup keys; do not apply that rule to
  providers whose metadata preserves nested or mixed-case paths.

Rules:

- Treat `platform_host` as part of every persisted GitHub object identity.
- When a caller explicitly supplies `platform_host`, honor it all the way
  through query, sync, and response shaping.
- Only fall back to the default host when the request truly omits host and the
  route semantics allow an implied GitHub host.
- New repo-scoped API work should use provider-aware routes and generated
  clients, not new `/repos/{owner}/{name}/pulls/{number}/...` GitHub-only
  compatibility paths.
- Do not constrain repo-scoped listing queries to one host unless the caller
  asked for that host.

## Freshness Rules

Bulk sync and detail sync have different jobs, but they must not disagree about
what "current" means.

- Bulk sync keeps tracked repos, open PRs/issues, and cheap derived state fresh.
- Detail sync populates comments, reviews, commits, and richer timeline data for
  one item.
- If a PR or issue is marked as detail-fetched, the persisted fields that power
  the user-visible detail view must match that claim.
- A partial GraphQL issue-comment page is observation-only: merge its visible
  and minimized states over stored moderation metadata before REST completion.
  (`internal/github/sync.go::refreshIssueTimeline`)
- PR conversation comments and review threads share one bulk GraphQL freshness
  boundary. Fetch their first pages together, paginate comment content and moderation
  through GraphQL, and keep detail stale while either family is incomplete.
  (`internal/github/graphql.go::gqlPR`, `internal/github/sync.go::syncOpenMRFromBulk`)
- A parent `304 Not Modified` is not a moderation freshness signal. When GraphQL
  admission permits, re-observe conversation and review-thread visibility under
  the parent revision guard (`internal/github/sync.go::markUnchangedMRDetailFetched`).
- Budgeted detail drain treats each queue item's worst-case cost as soft admission;
  provider pagination and child hydration may exceed it because the transport counts
  actual wire attempts (`internal/github/sync.go::drainDetailQueue`).

For pull requests, that means:

- Detail freshness must cover comments, reviews, review threads, commits, and
  stored PR system timeline events together.
- `last_activity_at` is the provider merge request's authoritative `updated_at`;
  child timestamps never redefine it or receive a local-clock fallback.
  (`internal/db/queries_snapshot_children.go::CommitMergeRequestChildSnapshot`)
- Background sync cooldowns are allowed, but user-initiated refreshes must still
  be able to promote a stronger sync intent over an in-flight background fetch.
- The hot set is the last 10 unique open PR details viewed and persists across
  restarts. Hot and explicit workspace-linked PRs use the configured watched cadence;
  terminal PRs are evicted immediately (`internal/db/queries_hot_merge_requests.go::RecordHotMergeRequestView`).
- Other recently active PRs are warm and detail-age gated at 10 minutes;
  never-fetched hot or warm PRs are immediately due
  (`internal/github/sync.go::hotAndWarmOpenMRs`).
- Linked PR notifications may advance fast-sync scheduling through
  `source_updated_at`, but that timestamp is only a staleness hint. Combine it
  with authoritative PR activity for warm admission; never persist it as
  `last_activity_at` (`internal/github/sync.go::hotAndWarmOpenMRs`).
- Scheduled and immediate watched-MR passes are serialized for the full pass so
  provider work and host cadence state have one owner
  (`internal/github/sync.go::syncWatchedMRs`).
- GitHub detail ETags reduce both payload work and kenn-forge's eager-refresh
  budget spend for unchanged PRs; the sync budget transport does not count
  `304 Not Modified` responses (`internal/github/budget_transport.go::budgetTransport`).
  Active watched-PR sync must use the same persisted pull-request ETag path as
  detail drain (`internal/github/sync.go::syncMRForRepo`,
  `internal/github/sync.go::getPullRequestForDetail`,
  `internal/github/sync.go::markUnchangedMRDetailFetched`). Manual/API PR
  refreshes must bypass that PR ETag gate so rerun checks, workflow approval,
  comments, reviews, and commits can refresh even when GitHub's PR resource is
  unchanged (`internal/github/sync.go::SyncMR`,
  `internal/server/huma_routes.go::syncPR`). Cadence control is still required
  because changed PRs correctly fall through to comments, reviews, commits, CI,
  and workflow approval refreshes.
- MR snapshot publication and workspace head-repository reclassification share
  one reconciliation barrier. Provider-ID collisions preserve the destination
  MR ID and merge its dependent review data before applying the newest provider
  snapshot. A snapshot moved from the old repository identity carries a durable
  stale-identity marker while its revision continues to advance; manual refresh
  preserves the workspace's prior trust classification until a post-move
  provider snapshot with authoritative head-repository data clears that marker
  (`internal/github/sync.go::CommitMergeRequestParentSnapshot`,
  `internal/db/queries.go::UpsertRepoByProviderID`,
  `internal/github/sync.go::reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead`).

## Timeline Event Rules

PR timeline storage is intentionally selective.

- Merged-actor repair is independent of open-list freshness: run it after every successful
  MR index cycle, preserve the provider-matched route, revalidate stored provider identity
  before persistence, use parent timing, and cool down completed sweeps. (`internal/github/sync.go::reconcileMergedActorEvents`)
- Keep one canonical merged lifecycle row across dedupe keys: authored rows dominate
  actorless refreshes, and later authored snapshots update the retained row.
  (`internal/db/queries.go::upsertMREventsTx`)
- Actor repair uses transaction-current parent timing and rejects non-merged parents.
  (`internal/db/queries.go::UpsertMergedActorEvent`)
- Merged-actor provider reads are active-detail work and preempt archive leases.
  (`internal/github/sync.go::BackfillMergedActorEventOnProvider`)
- Successful immediate and scheduled actor repairs target `pr_detail_refreshed` with canonical
  repository identity; broad `data_changed` does not refresh an open detail. (`internal/server/`)
- Detail synthesis treats any authored merge as canonical regardless of timestamp, since
  stored rows may predate parent-time realignment. (`internal/server/pullapi/routes.go::withSyntheticMRLifecycleEvents`)
- Keep the existing event families stable: comments, reviews, commits, force
  pushes, and the currently supported PR system events.
- GitHub commit events use the committer login/name and committed date as their activity actor/time when present, preserve a distinct original author in commit metadata, and fall back independently to author identity/time when committer data is absent.
- Review comments are UI-aware but are not part of the stored sync model unless
  they can be fetched within the supported timeline path.
- If bulk sync persists PR system events, detail sync must persist the same
  family so filters and `detail_fetched_at` do not lie.
- Optional timeline fetch failures may degrade that event family, but should not
  drop the entire PR detail refresh when the rest of the detail payload is still
  usable.
- Optional child-dataset failures do not rewrite `last_activity_at`; only an
  accepted provider parent observation may change it.
  (`internal/db/queries_snapshot_children.go::CommitMergeRequestChildSnapshot`)

## SHA-Sensitive Rules

Some PR-derived state is only valid for one head commit.

- Never carry CI status, check runs, or similar head-derived summaries forward
  when the PR head SHA changed underneath the refresh.
- Commit-event `obsolete` metadata has exactly one transition rule: only
  clone-verified head ancestry may set the flag, a provider relisting on an
  unverifiable head may provisionally clear it (showing a commit is the safe
  direction), and events a round neither verifies nor relists keep their
  last-verified flag. The frontend never infers obsolescence. Liveness must ride
  the sync round's own revision-guarded snapshot write — never a separate
  post-persistence writer — so a round that loses the revision CAS writes no
  liveness state at all; the only cache allowed is a memo of the pure
  (head, candidate-set) reachability function, which can never substitute for a
  write. The open-to-terminal transition is detected and finalized inside the
  parent snapshot transaction itself, so every state change funnels through
  that one choke point and no terminal MR is ever recomputed. UI mutations
  never write local state eagerly: merge re-reads the provider through the
  periodic close-detection flow (a merge result is not an MR snapshot), while
  state edits commit the mutator's returned MR — provider adapters must return
  the complete updated MR with authoritative timestamps from an edit. Local
  ancestry reads run
  in-process via go-git with a bounded visit budget, never git subprocesses;
  SHA-256 (64-hex) repositories are out of scope and their commit events are
  never flagged
  (`internal/github/sync.go::computeCommitLiveness`,
  `internal/db/queries.go::UpsertMergeRequestSnapshotWithLabelsUnderRepositoryReconciliationRead`,
  `internal/gitclone/reachability.go::CommitsReachableFrom`).
- Mutation-triggered closed-MR refetches capture the route generation before
  provider I/O, carry stable clone identity, and fence all snapshot/diff writes
  (`internal/github/sync.go::SyncClosedMROnProvider`).
- Workflow-approval decisions must be tied to the correct PR identity, not just
  the head SHA. Shared SHAs across forks or sibling PRs must not leak approval
  state between items.
- When a refresh cannot prove the state belongs to the current head SHA, clear
  the stale derived state instead of preserving it.
- `MRReviewThread.Range.DiffHeadSHA` preserves the comment commit even for
  current threads and is distinct from `MergeRequest.DiffHeadSHA` (the synced
  diff snapshot); suggestion apply uses the stored thread head to reject stale
  suggestions (`internal/github/sync.go::githubReviewLineRange`).
- Suggestion apply commits to the PR head repo/branch bound by
  `createCommitOnBranch.expectedHeadOid`, never the base repo. Reject
  whitespace-padded paths (do not trim), preserve terminal blank replacement
  lines, and do not re-add a trailing newline when a suggestion deletes every
  line. Mutation-time `NOT_FOUND`/could-not-resolve failures are head repo or
  branch races and map to conflict `head_repo_unknown`, not `not_found`
  (`internal/github/client.go::ApplyReviewSuggestions`).
- Apply-suggestion consumes REST content reads plus the GraphQL
  `createCommitOnBranch` mutation; the provider reports both buckets via
  `OperationRateLimitBuckets(platform.OperationApplyReviewSuggestion)` so a
  paused GraphQL budget disables the operation without making GraphQL
  provider-neutral.

## Fallback Data Rules

GitHub data sources are intentionally layered and may remain GitHub-specific
behind the provider split.

- Repos without usable releases may fall back to tags for version-like timeline
  context.
- Repository import for the authenticated owner may need a different GitHub API
  path than generic org/user repo listing so private owned repos are included.
- Fallbacks must preserve the same response shape and user-visible semantics as
  the primary path whenever possible.

Use fallback paths to keep user-visible GitHub features working, not to silently
change what a field means. Provider-neutral persistence should receive the same
semantic shape regardless of whether data came from GraphQL, REST, tags, or
fallback repository listing.

## Native Stack Rules

- GitHub.com merges are versioned asynchronous direct operations polled to terminal
  `merged`; `pending`/`enqueued` never count as merged. GHES stays synchronous with no
  probe fallback. (`internal/github/merge_async.go::liveClient.MergePullRequest`)
- Preserve required-rebase failures verbatim; do not substitute private website routes
  or local force-pushes until GitHub exposes a token-authenticated stack-rebase API.
  (`internal/github/merge_async.go::mergeAsyncTerminalResult`)
- Confirmed native stacks claim and order their PRs first; branch inference always
  runs afterward on every unclaimed PR, including when the preview is disabled,
  incomplete, or failing. (`internal/stacks/detect.go::RunDetectionWithNativeStacks`)
- Compare current PR hints with cached stack rows; scan `/stacks` newest-first
  and stop once every target is found or passed.
  (`internal/github/native_stack_sync.go::refreshGitHubNativeStackCache`)
- Native projections use the bottom PR number as the neutral stack key; GitHub's
  independent stack number remains cache-only.
  (`internal/stacks/detect.go::persistStackChain`)
- Disabling the preference synchronously restores branch-derived projections;
  cached native rows remain dormant. The syncer's preference is the transition
  authority — every server binds it to the boot config and reconciles on the
  swap's own previous value, never on a separately read config snapshot, so
  concurrent writers cannot reconcile twice or not at all. The swap happens under
  cfgMu so the preference order matches the persisted order, and the
  reconciliation that follows the unlock is committed-state work: it runs on the
  server-lifecycle context, never the request's, and rechecks the current
  preference under the projection lock so a disable that lost to a later enable
  cannot replay over it. That recheck is only sound because the swap itself
  takes the projection lock. Reconciliation covers every repository holding
  cached native stacks, not just tracked ones: a repository dropped from config
  still serves its stored pull requests and no sync will revisit it. Boot with
  the preference off reconciles the same way, since the setting can change while
  the daemon is stopped.
  (`internal/server/native_stack_settings.go::reconcileGitHubNativeStackProjection`,
  `internal/github/sync.go::SetPreferGitHubNativeStacks`)
- The preview must not widen the blast radius of the list it rides on. The REST
  hint decodes separately from the pull request, so a changed field shape costs
  that hint and not the list, and hint-listing errors get the same
  feature-disabled classification as the plain list so a repository with pull
  requests off still enters the cooldown.
  (`internal/github/native_stacks.go::decodeNativeStackHint`,
  `internal/github/sync.go::ListOpenMergeRequestsWithNativeStackHints`)
- Preview-only GraphQL fields must be absent from disabled query shapes;
  `@include(false)` does not bypass schema validation on servers without those
  fields. Schema rejection drops the fields for that host instead of abandoning
  bulk fetch. (`internal/github/graphql.go::isNativeStackSchemaRejection`)
- Confirmation reconciles against currently observed open-PR hints, never cached
  or payload member state. Hints cannot attest to merged or closed members, so a
  stack holding one is refetched on a bounded schedule and its confirmation ages
  out rather than surviving every 304. The deadline is anchored to each stack's
  own observation time and the earliest one wins, so re-confirming an old stack
  during an unrelated refresh cannot extend its window.
  (`internal/github/native_stack_sync.go::cachedStackMatchesCurrentHints`,
  `internal/github/native_stack_sync.go::nativeStackObservationExpired`)
- A pull request may belong to at most one projected stack. Member eviction
  would silently shorten the stack written first and hide a preceding merge
  blocker, and projecting one side of an overlap does the same to the other, so
  an overlap makes the whole native projection ambiguous and branch inference
  owns the repository for that pass.
  (`internal/stacks/detect.go::RunDetectionWithNativeStacks`)
- Only a query that requested the preview fields may replace stack hints;
  a GraphQL shape that dropped them says nothing about membership and must leave
  REST-derived hints intact. (`internal/github/graphql.go::RepoBulkResult`)
- Only a refresh that resolved every target seeds the confirmation a later 304
  reuses; an incomplete refresh evicts the pull-request list ETag so the next
  sync retries. It also projects nothing for that pass, not the subset it did
  confirm: an unresolved stack is invisible to the overlap scan, so a confirmed
  stack could claim a pull request the unresolved one holds and hide its
  predecessor. A target dropped without being persisted -- fetch failure,
  malformed row, or disagreement with current hints -- makes the pass partial.
  (`internal/github/native_stack_sync.go::refreshGitHubNativeStackCache`)
- Native results carry the preference generation and project under the shared
  stack-projection lock, so a sync that began while the preview was enabled
  cannot reinstate it afterward.
  (`internal/github/sync.go::dropStaleNativeStackResults`)

## Historical Archive Rules

- The legacy closed-item backfill is retired; configured repositories seed durable archive discovery before sync cutover, with no cursor translation. (`internal/github/sync.go::SetReposWithContext`)
- Provider-archived repositories are configurable and archive-only: resolution
  accepts them (exact and glob) with `RepoRef.Archived` set, live sync skips
  them — the bulk sync pass, notification polling, and watched-MR fast sync
  alike — and archive discovery/hydration treat them like any configured repo.
  Settings add/refresh merges apply freshly resolved metadata to
  already-tracked refs, so an archived flip (either direction) takes effect
  without a daemon restart even when exact and glob entries overlap.
  (`internal/github/notifications_sync.go::SyncNotifications`,
  `internal/github/sync.go::watchedMRsForFastSync`,
  `internal/server/settings_handlers.go::mergeTrackedRepos`,
  `internal/server/settings_handlers.go::replaceGlobRepos`)
- Tracked-set deduplication reconciles by stable provider id when one is
  present, falling back to the route key: a renamed route must collapse onto
  the same tracked entry, never sync or archive-seed the repository twice.
  Provider-resolved refs replace fallback-derived duplicates; fallback refs
  never overwrite resolved ones. (`internal/github/repo_config_resolver.go::ExpandedRepoSet`,
  `internal/server/settings_handlers.go::trackedRepoIndex`)
- Exact-resolved refs record the config-entry path they came from
  (`RepoRef.ConfiguredRepoPath`); glob refs carry none — a pattern identifies
  no single entry, and stamping it would displace exact provenance on
  deduplicated overlaps. Tracked-set deduplication merges provenance across
  duplicates in both directions, catalog republication preserves it, and
  publication never overwrites the currently tracked value: config resolution
  is the only author. When an exact entry cannot resolve, the fallback matches
  previously tracked refs by that provenance before the route path: a
  provider-side rename moves the tracked route away from the configured path,
  and without the provenance match the fallback would synthesize an
  identity-less live ref — dropping the archived flag and duplicating the
  repository next to a resolved overlapping entry. Settings merges preserve
  tracked provenance the same way (settings-resolved refs never author it),
  but route-keyed recovery refuses to cross conflicting stable provider ids:
  a route reused by a different repository must not inherit the displaced
  repository's provenance, or two refs would claim the same config entry.
  Startup — which has no tracked set — recovers a failed exact entry's
  stable identity through catalog route history before synthesizing, and
  registers the repo credential alias for the recovered route so repo-scoped
  credentials do not fall through to owner or host routes. Config-entry
  matching (glob refresh, entry removal) honors provenance the same way — a
  renamed repo still belongs to its exact entry, scoped to the entry's
  provider and host since the same path can be configured on several — and
  removal clears provenance whose entry no longer exists, so a stale claim
  cannot bind a future entry with the same path to the wrong repository.
  Publications locate their tracked slot by stable identity first, keyed on
  the resolved id: the provider response says whose data this is, so a
  lookup through a reused route lands on the tracked successor, never on
  the repository the snapshot named. The route fallback may displace the
  snapshot's own entry — configured-route reuse replaces the occupant and
  the archive lifecycle pauses the old repository — but never an entry
  whose id conflicts with the snapshot. When the resolved id differs from
  the snapshot's — or from the landed slot's, which matters when the
  snapshot carries no id — the data belongs to a different repository than
  the flags describe, so the snapshot's archived flag is meaningless:
  authoritative resolved metadata applies, and only a non-authoritative
  publication preserves tracked state.
  (`internal/github/repo_config_resolver.go::FallbackConfiguredRepoRefs`,
  `internal/github/repo_config_resolver.go::ExpandedRepoSet`,
  `internal/github/sync.go::repoRefFromCatalog`,
  `internal/github/sync.go::publishResolvedRepository`,
  `internal/server/settings_handlers.go::trackedRepoProvenance`,
  `cmd/kenn-forge/main.go::fallbackExactFromDB`)
- Catalog republication without fresh provider metadata preserves the
  currently tracked archived flag: a sync that began before a newer archived
  flip must not clear it when its own snapshot predates the flip. When the
  tracked flag differs from the operation's snapshot, a concurrent resolution
  flipped it mid-flight — ordering against the in-flight provider response is
  unknowable, so the newer tracked value stands even over fresh provider
  metadata. (`internal/github/sync.go::publishResolvedRepository`)
  Archived state refreshes wherever resolution already happens (startup,
  config reload, settings add/refresh) and must survive catalog
  republication, which cannot read it from the store. Archived inclusion in
  repository listings is an explicit request
  (`RepositoryListOptions.IncludeArchived`): configuration expansion sets it
  so GitLab globs match archived projects like GitHub globs do, while
  default listings — import previews and the repo-import handler — keep
  GitLab's server-side `archived=false` filter, which runs before any
  listing limit so archived projects cannot crowd live ones out of a
  bounded preview. (`internal/github/repo_config_resolver.go::resolveConfiguredRepo`,
  `internal/platform/gitlab/client.go::ListRepositories`,
  `internal/github/sync.go::repoRefFromCatalog`)
- Archived state transitions are observed during normal sync passes, not
  only at resolution sites: each pass reconciles archived tracked refs with
  metadata-only identity resolution, so an upstream unarchive returns the
  repository to live syncing without a restart or reload, while refs still
  archived (or whose refresh fails) stay excluded. The refresh honors the
  same credential-bucket eligibility that gates dispatch — a throttled,
  reserve-exhausted, or next-sync-deferred bucket defers its archived
  refreshes without a provider call, so they cannot spend essential sync
  budget a live repository's dispatch would be denied. An attempted archived
  refresh also advances the bucket's next-sync cadence gate — including for
  buckets holding only archived repositories, which drop out of the pass
  before dispatch eligibility is computed — so the refresh honors the
  bucket's throttle factor instead of rerunning every base interval. The
  refresh registers provider work like a live repo sync, so an admitted
  archive request on the same credential is preempted rather than
  overlapping it. In the other direction, a live repository whose in-pass
  identity resolution reports archived stops before any clone, overview,
  label, or item syncing — the publication has already flipped the tracked
  flag, and the pass must not sync an archived repository's content on the
  way out. Identity resolution returns the ref the publication actually
  stored, not its own snapshot: when the publication kept a newer tracked
  archived flip over the operation's metadata, the caller deciding whether
  to keep syncing must see the published value. Follow-on detail work
  honors the flip too: the detail drain skips queue items whose tracked or
  freshly resolved ref is archived — the queue was built before the pass
  observed the transition — and per-item MR/issue syncing stops on an
  archived resolve except under the archive sync budget, which is exactly
  the hydration path archived repositories rely on.
  (`internal/github/sync.go::reconcileArchivedRepos`,
  `internal/github/sync.go::syncRepo`,
  `internal/github/sync.go::reconcileRepoIdentity`,
  `internal/github/sync.go::drainDetailQueue`,
  `internal/github/sync.go::syncMRForRepo`,
  `internal/github/sync.go::syncIssueForRepo`)
- Archive seeding degrades per repository: a ref that fails validation,
  provider resolution, or catalog reconciliation is logged with its identity
  and skipped, never fatal — one bad configured entry must not crash-loop the
  daemon at startup. Only batch reconciliation errors (a broken store)
  propagate. Refs skipped by seeding are excluded from authentication retry
  so they cannot fail a config reload.
  (`internal/archive/service.go::EnsureConfigured`,
  `internal/github/sync.go::SetReposWithContext`)
- Removal pausing (`configuration_removed`) requires a complete picture: when
  any configured ref fails seeding without a known repository row, the pass
  ensures discovery archives but defers the pausing side entirely — an
  incomplete protection list could pause the wrong repository's archive
  (renamed owners resolve to rows under other identities). A ref that failed
  after its row was identified stays protected without deferring the pass.
  While an unresolvable ref persists in config, genuinely removed repos keep
  collecting; the recurring deferral warning is the operator signal.
  (`internal/archive/service.go::EnsureConfigured`)
- The archive worker pass resolves configured repositories tolerantly: a ref
  that seeding skipped stays in the syncer's tracked set, so an all-or-nothing
  resolve would fail every pass and starve archive work for all
  healthy repositories. Only provider-classified failures (invalid ref,
  provider not configured, missing capability) are dropped as
  repository-scoped (debug-logged; seeding already warned); a broken store or
  any other infrastructure error still surfaces — an empty pass reported as
  success would hide a dead worker. (`internal/archive/scheduler.go::RunPass`,
  `internal/archive/service.go::resolveRepositoriesTolerant`)
- The archive worker backs off while idle instead of ticking every second: after a pass
  that attempted no work the wait doubles from the pacing interval to a five-minute cap,
  and a pass that worked or failed, or a wake, returns it to the pacing interval. Every
  completed sync run wakes it, since a sync can clear a feature cooldown and make archive
  work eligible. (`internal/github/sync.go::runArchiveLoop`, `internal/github/sync.go::runOnceWithSlot`)
- A pass "worked" when a unit reached the provider, including a provider-answered feature
  deferral or a preempted request, or failed; only an admission denial before any provider
  request is idle, so a long live sync backs the worker off instead of writing a one-second
  deferral every second. (`internal/archive/scheduler.go::finishWork`)
- Releasing the last live provider operation on a host wakes the archive worker only when
  that host denied or preempted an archive request; a normal sync's stream of releases
  must not trigger denied passes and deferral writes. (`internal/github/sync.go::beginProviderWork`)
- Archive scheduling is eventually consistent by maintainer decision: a missed or late wake
  that only delays eligible archive work until the next backoff pass (five minutes at most)
  is accepted behavior, not a defect. Do not add wake bookkeeping or atomicity for it.
  (`internal/github/sync.go::runArchiveLoop`)
- Initial issue and pull-request inventory includes all states in stable created-time ascending order; issue enumeration excludes PR-shaped rows. (`internal/github/pages.go::ListIssuesPage`, `internal/github/pages.go::ListMergeRequestsPage`)
- GitHub issue-only repositories return pulls API 404; normal and archive paths
  classify it as feature-disabled only for explicit `has_pull_requests=false`;
  ambiguous probes preserve the failure. (`internal/github/sync.go::mergeRequestsDisabledByRepository`)
- GitHub pull inventory admission reserves the possible metadata probe and its
  authentication retry. (`internal/archive/scheduler.go::archiveFeatureReadAttemptCost`)
- Every issue-only GitHub lookup rejects a PR-shaped Issues API response before normalization; `SyncItemByNumber` is the kind-dispatching exception. (`internal/github/pages.go::gitHubClientProvider.issuePullRequestOutcomeError`, `internal/github/sync.go::SyncItemByNumber`)
- Updated issue scans query one second before the durable watermark while keeping cursor identity bound to the original boundary. Updated pull-request scans run newest-first across the same overlap. (`internal/github/pages.go::ListIssuesPage`, `internal/github/pages.go::ListMergeRequestsPage`)
- Durable pull-request inventory bypasses the process-local list ETag cache. Archive cursors require response bodies, so a bodyless `304 Not Modified` must not turn an unchanged maintenance scan into a retryable failure. (`internal/github/pages.go::liveClient.ListInventoryPullRequestsPage`)
- Repository probes classify only authentication/access/not-found responses; transient probe failures remain retryable and non-destructive. Issue and pull-request lookups compare the response repository with the requested source identity so transfers become moved outcomes instead of source-owned snapshots. (`internal/github/pages.go::archiveRepositoryProbeError`, `internal/github/pages.go::githubArchiveDestination`)
- After repository-wide issue disablement is classified, a 410 from GitHub's
  single-issue endpoint means deleted; map it to `removed_upstream` only at
  that lookup boundary, never across GitHub endpoints.
  (`internal/github/pages.go::gitHubClientProvider.classifyIssueLookup`)
- A previously-open issue whose GitHub-classified lookup is a true removal
  (not_found, no destination) is tombstoned closed locally; otherwise it would
  fail every cycle forever. Transfers and provider-neutral bare 404s (GitLab
  hides inaccessible items behind 404) keep failing the cycle so maintainers
  see them in repo sync health
  (`internal/github/sync.go::tombstoneRemovedIssue`).
- Archive REST and GraphQL failures must preserve typed authentication and reset-aware rate-limit errors so scheduling defers rather than hot-looping generic retries. (`internal/github/pages.go::archiveTransportError`)
- GitHub archive code owns historical identity inventory only; hydration must invoke ordinary item sync instead of adding archive-specific lookup, normalization, or persistence. (`internal/github/pages.go::ListIssuesPage`, `internal/github/sync.go::SyncArchiveItem`)
- Archive item hydration bypasses persisted parent-detail ETags; an unchanged parent representation does not prove that legacy lifecycle timelines are complete. (`internal/github/sync.go::SyncArchiveItem`)
- Every older-generation active merged GitHub lookup is requeued once; its new
  generation proves canonical merged detail passed the current verification.
  (`internal/db/queries_archive.go::RequeueArchiveLifecycleDetails`)
- A GitHub archive lookup requires fetched and stored merge state to agree;
  merged lookups also require completed lifecycle persistence, merge time, and
  matching canonical SHAs and file count. (`internal/github/sync.go::requireGitHubArchiveMergedMRMetrics`)
- Archive completion atomically revalidates fetched merge evidence against the
  transaction-current row; a mismatch remains retryable, never complete.
  (`internal/db/queries_dataset_progress.go::validateArchiveMergeRequestEvidenceTx`)
- An accepted merged snapshot preserves the stored immutable merge time when
  the provider omits it; partial detail must not erase durable lifecycle data.
  (`internal/github/sync.go::preserveMergedAtIfOmitted`)
- Under repository, number, merged-row, and head-SHA guards, canonical merged
  detail replaces stored merge SHA and file count while filling missing merge
  time without weakening snapshot ordering. (`internal/db/queries_merge_lifecycle.go::FillMissingMergedMRMetrics`)
- Rejected canonical merged snapshots repair each available lifecycle field and
  the merger event under one route-fence lease; stored parent timing remains
  authoritative when GitHub omits it, and a rejected fence stays retryable. (`internal/github/sync.go::syncMRForRepoResolved`)
- Post-hydration completeness queries the repository ID resolved by that sync;
  never re-resolve the caller's mutable route after reconciliation.
  (`internal/github/sync.go::SyncArchiveItem`)
- Archive issue hydration treats timeline failures as hard errors; ordinary issue refresh remains best-effort for that optional dataset. (`internal/github/sync.go::refreshIssueTimeline`)
- GitHub archive admission with a known registry pacing window paces off
  provider quota alone: availability is the minimum across required resources
  of remaining minus that pool's own archive reserve
  (`max(limit/5, RateReserveBuffer)` of that pool's limit), enforced at
  admission and again at every wire attempt. Reserves are per pool — the
  smallest pool's reserve must not be applied to a larger pool. Attempts
  covered by a registry reservation do not debit the local ceiling. An active registry whose combined window is
  incomplete or expired defers admission as provider-quota-unknown; it never
  falls through to local pacing. Every other archive path — headerless
  Gitealike hosts, GitHub without registry-based pacing (nil registry or
  unresolved identity), and any attempt whose chain takes no reservation —
  spends configured local hourly surplus above `archiveLiveFloor`. Live work
  preempts the archive lease.
  (`internal/github/budget.go::ArchiveProviderReserve`,
  `internal/github/budget.go::LocalArchiveSpendAvailable`,
  `internal/github/sync.go::Admit`,
  `internal/github/budget_transport.go::archiveAttemptProviderReserved`)
- A GitHub issue without `updated_at` uses `created_at` as both its freshness and initial activity boundary; zero timestamps must not bypass monotonic snapshot acceptance. (`internal/platform/github/normalize.go::NormalizeIssue`)

## Owner Routes And Identity Accounting

GitHub authorization is selected by `(host, repository owner)`, with exact
repository overrides ahead of owner mappings and the host fallback. Rate state,
sync budgets, cadence, and snapshots are selected separately by `(host,
authenticated identity)`. Different PATs resolving to the same GitHub user ID
must share one runtime; App reads use their installation identity.

- Startup PAT identity discovery must use a bounded per-request context
  (`internal/github/identity.go::HTTPIdentityResolver.ResolvePAT`).
- When required scoped routes cover configured repositories, the implicit
  ownerless fallback is probed best-effort: a resolvable token keeps ownerless
  APIs routed, while a missing or invalid one is skipped with a warning instead
  of failing startup. Only an explicitly configured host fallback fails hard; a
  `github_token_env` equal to the built-in default does not count as explicit
  because Load, Save, and the sample config all materialize that name
  (`internal/config/config.go::Config.HasExplicitGitHubTokenEnv`,
  `cmd/kenn-forge/provider_startup.go::buildGitHubIdentityRuntimes`).
- A configured router with no exact, owner, or fallback route is a routing
  failure; operation availability must fail closed instead of treating it as an
  unrouted legacy host (`internal/github/auth_router.go::MissingRouteError`,
  `internal/server/operation_availability.go::writeCredentialGateForRepo`).
- Background requests on the write credential (viewer-permission overlay,
  notifications, queued read propagation) charge the write identity's sync
  budget — the transport's context gate keeps foreground mutations uncharged —
  and live work registers provider work for every principal it will touch,
  read and write, so a shared-PAT archive is preempted
  (`internal/github/client.go::NewClient`, `internal/github/sync.go::syncRepo`,
  `internal/github/notifications_sync.go::ProcessQueuedNotificationReads`).
- Reload probes share one fresh installation-token cache per validation batch:
  per-route caches would multiply minting, while reusing the live cache lets a
  revoked installation or replaced private key pass validation until the cached
  token expires (`internal/tokenauth/source.go::SourceSet.NewProbeBatch`).

Repository `token_file` and `token_env` overrides are exact-only; reject them on
name globs rather than creating a literal route that discovered repositories
cannot select (`internal/config/config.go::Config.validate`).

Repository preview must select the entered owner's route even before that owner
has a tracked repository. Ownerless APIs may use only the host fallback; never
borrow an arbitrary owner PAT. Repository notifications use the user/write
identity. App-only routes may read, but notifications and mutations remain
disabled until restart establishes a stable user identity.

Notification sync watermarks are per repository identity, never host-wide: a
repository whose credential route is unavailable or exhausted reports its error
without holding back watermark advancement for healthy repositories on the same
host (`internal/github/notifications_sync.go::Syncer.syncNotificationsForRepo`).

Queued read-acknowledgement backoff is scoped the same way. A rate limit belongs
to the credential that hit it — on either refetch leg or the mark-read — so
defer only that identity's repositories and keep propagating the batch's other
identities; the pass still returns the rate limit so the host records its error
(`internal/github/notifications_sync.go::Syncer.ProcessQueuedNotificationReads`,
`internal/db/queries_notifications.go::DB.DeferQueuedNotificationAcksForRepos`).

Selected-repository App routes may expose installation-repository listing as an
owner-scoped discovery route, but that route must never become a fallback for
other repository operations. Owner discovery unions the PAT route's listing
with the selected-App listing and dedupes by repository ID — a PAT lists
everything it can access but misses selection-only grants, while the App client
lists only its selection — and fails closed if either configured source fails
rather than silently narrowing coverage
(`internal/github/auth_router.go::RoutedClient.listRepositoriesByOwnerAcrossRoutes`).

`RoutedClient` embeds the `Client` interface, so any optional capability
interface it does not re-declare disappears from behind the wrapper and
`gitHubClientProvider.Capabilities()` silently reports that capability as
unsupported on every routed host. When adding an optional GitHub client
interface, give `RoutedClient` a repository-routed method and add its
`_ iface = (*RoutedClient)(nil)` assertion; carry owner and repository name in
the interface so exact `repo:` routes pick their own credential
(`internal/github/auth_router.go::RoutedClient.GetMarkdownImage`). List each
optional interface in the routing guard so an unrouted owner-bearing method
fails a test instead of silently disabling the feature
(`internal/github/public_api_guard_test.go::TestRoutedClientExplicitlyImplementsOwnerBearingClientMethods`).

A wire call issued during repository sync routes by repository even when the
endpoint itself is host-scoped (`/users/{login}` for author display names).
Owner-only and App-only configurations have no host fallback route, so a
fallback-only lookup fails for every repository and, where a fallback does
exist, spends the wrong identity's budget. Such a call must also pass
`tokenauth.WithGitHubOwner`: the transport derives the owner from the request
path, so an ownerless path silently skips the App candidate and pays with the
PAT for a read the route's tracker bills to the installation
(`internal/github/auth_router.go::RoutedClient.GetUserForRepo`).

Managed Git uses exact-repository or owner PAT routes with mutation context and
must never expose an App installation token to smart HTTP. Thread full provider,
host, owner, and repository identity through clone/fetch and local reads, passing
the normalized platform (`repoPlatform(repo)`) so an unqualified GitHub ref still
picks its credential route instead of none. Partition sync, diff, and repository
browser clone storage by stable provider repository identity
(`internal/gitclone/repo_browser.go::repoBrowserCloneNamespace`). Only the shared fetch's starter discards
a clone when its captured route generation no longer owns the path; later
callers re-validate their own fences as pure gates so a stale caller never
deletes a clone current-route callers are reading, and a follower rejected only
by the starter's stale route retries with its own validated fetch. Workspace clones remain
path-scoped, so shared full-stack fixtures must seed both namespaces
(`internal/testutil/diff_repo.go::SetupDiffRepo`). Before injecting
a PAT into workspace fetch or push, require the branch upstream to be `origin`,
reject repository-local URL rewrites, and validate every origin fetch/push URL.

A nil `tokenauth.Source` is not fail-closed: `gitclone` reads it as permission to
run git with no credential, which succeeds against any public repository and
spends no identity's budget. A route resolver that cannot serve a repository must
return a source whose `Token` reports the missing route
(`cmd/kenn-forge/provider_startup.go::missingRouteTokenSource`).

Token-file rotation within the same GitHub user is hot-reloadable. Changing the
authenticated user, adding a write identity to an App-only route, or adding or
removing a bounded route requires restart. Added, removed, or descriptor-changed scoped routes require restart. The live
bounded router keeps its boot descriptor until restart so it cannot lose auth or
move to a different identity while retaining the old trackers and budget.

## Credential-Aware Quota

Provider quota is keyed by `IdentityKey` and REST/GraphQL resource, so a user
response never overwrites an App installation pool
(`internal/github/quota.go::QuotaRegistry`).

- Each client transport chain carries a fixed identity: reads spend the route's
  read identity, mutations and notifications its write identity
  (`internal/github/client.go::WithQuotaAccounting`).
- Background admission gates on the routed credential's own reserve; the local
  `sync_budget_per_hour` ceiling is separate and is reported apart from provider
  quota (`internal/github/sync.go::backgroundQuotaAvailability`).
- Local-ceiling failures retain the code, credential key, and refusal-window reset through
  item, hard-return, and parallel-run aggregation; UI details require both the key and reset
  to match the live row (`internal/github/sync.go::SyncStatus`, `internal/github/budget_transport.go::syncBudgetExhaustedError`).
- List discovery (open PR/issue lists, repo identity resolve) spends the local
  ceiling's essential reserve; optional spend (details, fast-sync, archive
  attempts without a registry reservation)
  stops at limit minus reserve so it can never starve discovery
  (`internal/github/budget.go::TrySpendEssential`). Per-item enrichment nested
  inside an essential list fetch is demoted back to optional and degrades on
  refusal instead of failing the list
  (`internal/github/budget_transport.go::WithoutEssentialSyncBudget`).
- A list fetch refused by the local ceiling must not evict list ETags: nothing
  reached the wire, so eviction would only turn recovery cycles into
  unconditional refetches that deepen the exhaustion
  (`internal/github/sync.go::indexSyncRepo`).
- Archive pacing on the provider path is the routed credential's minimum
  per-pool headroom above each resource's own archive reserve; the local ceiling
  neither enlarges nor shrinks the provider envelope. Each archive attempt
  atomically reserves live headroom; an unobserved reservation remains
  deducted until a current header accounts for it or that resource window
  rolls. The first header in a newer window uses total reported usage as its
  conservative request-cost bound
  (`internal/github/sync.go::Admit`,
  `internal/github/quota.go::quotaTransport`).
- Outside the archive attempt guard above, there is one background reserve
  check, and it runs on the snapshot cadence
  (`internal/github/sync.go::backgroundReserveExhausted`). The verdict is cached
  per credential, resource, and foreground/background mode, recomputed at most
  once per `rateLimitSnapshotRefreshInterval`, and dropped when a `/rate_limit`
  refresh replaces the numbers it was derived from. Repository admission,
  workers, both drains, bulk GraphQL, and notification acknowledgements all read
  that one verdict. Do not add per-repository, per-queue-item, or per-page
  reserve checks: ordinary provider quota only moves when the snapshot refresh
  moves it, so re-deriving more often mostly re-reads the same numbers, and the
  divergent costs and fallbacks that grew up around those sites were the bug.
- Outside archive hydration, a credential that crosses its reserve inside a
  cadence window keeps spending until the window turns. That reserve is a soft
  foreground buffer; the local hourly ceiling remains the hard per-wire guard.
- Gate background eligibility on REST only. Optional GraphQL reads are optimizations with
  a REST fallback, so requiring GraphQL capacity stops repositories that could
  still sync (`internal/github/sync.go::repoEligibility`). `graphQLReadAllowed`
  applies the GraphQL reserve where it is spent, and answers from the credential
  verdict whenever that pool is known — falling through to the fetcher's tracker
  would consult a host-wide signal both credentials on a split-auth host feed.
- The quota registry is in-memory, so the check falls back to the rate tracker's
  SQLite-backed state when a credential is unobserved; otherwise a restart
  admits background work against a reserve the persisted state says is spent
  (`internal/github/sync.go::persistedReserve`). An elapsed persisted reset
  window says nothing and must not gate.
- The local ceiling rolls its hourly window on its own clock and must never need
  a provider response to reset: an exhausted ceiling refuses counted requests
  before any wire attempt, so a reset driven only by response headers or
  rate-tracker rollover wedges background sync for good
  (`internal/github/budget.go::rollLocked`).
- Reservations carry the window they were made in and refunds are dropped once
  that window elapses, so a 304 arriving after a roll cannot raise the new
  window's ceiling (`internal/github/budget.go::Refund`).
- Notification reads and queued ack propagation resolve to the write identity,
  so they gate on that credential's REST pool even when repository reads run on
  an App token. Gate on the operation's worst-case request count, not one
  request (`internal/github/notifications_sync.go::ensureNotificationBudget`).
- A scheduling decision must read the pool of the credential that will perform
  the work; a host-wide tracker lets one credential's exhaustion suppress
  another's (`internal/github/sync.go::graphQLReadAllowed`).

## GitHub App Manifest Flow

`kenn-forge-github-app create` uses GitHub's App Manifest flow so sync can read
with installation tokens. Even though kenn-forge disables webhooks and polls,
the manifest must still include a syntactically valid `hook_attributes.url`;
GitHub's live manifest validator can report the missing hook URL as a generic
`"url" wasn't supplied` error. Do not remove that hook URL from
`internal/githubapp/manifest.go::NewManifest`; keep
`cmd/kenn-forge-github-app/e2e_test.go::TestCreateFlowEndToEnd` asserting the
serialized manifest shape so the fake cannot accept a payload GitHub rejects.

A covering App installation leads every read chain, including on a repository
that configures its own `token_env`/`token_file`: installation tokens carry
their own rate-limit budget, so reads always prefer them and a repository
override must never displace one. That override is still the first PAT, so it
signs that repository's writes — mutation resolution skips App candidates
(`internal/config/config.go::Config.ResolveGitHubRepoTokenSource`,
`internal/tokenauth/source.go::WithMutationAuth`). Dropping the App candidate
also costs the owner its only selection-only discovery credential.

GitHub App installation tokens are account-scoped, not host-scoped. An app
installation for one owner must not authenticate reads for another owner just
because both repos share the same host. Repo-scoped GitHub reads must resolve app
tokens with the repository owner in context, and ownerless contexts such as
clone auth must fall through to PAT/`gh` credentials. This owner scoping governs
endpoint selection, not just token resolution: choose an installation-token-only
read endpoint (such as installation-repositories listing) only when the requested
owner actually resolves to an app installation. Gating it on whether the host has
any active app sends a PAT-backed owner that shares the host with another owner's
app to an endpoint its credential cannot use, which fails even though the token
chain "correctly" falls back to the PAT.
- Private `user-attachments` reads are the exception to app-token-first reads:
  GitHub returns 404 to installation tokens, so the repo-scoped image proxy must
  use the user's PAT/`gh` chain (`internal/github/markdown_images.go::GetMarkdownImage`).
- Repository-file markdown images (`blob`/`raw` web URLs, `raw.githubusercontent.com`)
  are proxied only for the route's own repository, use the normal read chain, and are
  type-sniffed because the contents raw media type hides the file type; web URLs do not
  delimit ref from path, so ref splits are tried shortest-first past 404s. They are
  marked mutable because the ref is usually a branch. The frontend must apply the
  same URL rules (`internal/github/markdown_images.go::getRepositoryFileImage`).
Config may carry multiple `[[github_apps]]` rows for one host, but those rows
represent distinct app credentials. Management commands must target one row by
app owner/installation account or app id, and duplicate installation accounts on
the same host are invalid. Selected-repository coverage applies only to repos
owned by that row's `installation_account`, and the install CLI must not warn
that an installation on one account "cannot reach" repos owned by another
account. The recorded selected-repository list is a startup routing snapshot:
expanded access remains on the PAT route and narrowed access may return 404
until `kenn-forge-github-app install` refreshes the snapshot and kenn-forge is
restarted. Do not retry PAT credentials after an App-backed repository 404;
GitHub uses the same response for absent, private, and inaccessible repositories,
so automatic fallback would hide stale or revoked App access.

Re-running `install` after a coverage failure (or against a restored
config) reconfigures the existing installation instead of minting a new
installation id, so on a clean install-poll timeout the flow adopts an
already-present installation rather than only ever waiting for a newly created
one. Adoption runs only after a clean poll deadline and is bounded by intent:
adopt only the app's sole installation when its account is the recorded
installation account or owns a configured repo that resolves to the app.
Multiple installations or a lone installation on an unrelated account leave the
deadline as a timeout instead of recording the wrong account. A transient probe
error or a user interrupt is not a clean deadline: it surfaces the original
error or cancellation unchanged and never adopts.

## Testing Expectations

Changes in this area should usually add or update tests at the boundary where
the regression would show up.

- `internal/github/*_test.go` and `internal/platform/github/*_test.go` for
  GraphQL parsing, normalization, adapter compatibility, optional failure
  handling, and sync sequencing.
- `internal/server/api_test.go` when the bug would surface through HTTP payloads
  or sync-triggering handlers.
- Fixture-client coverage when a fake GitHub path needs to model private repos,
  edited comments, or timeline families consistently.

For notification sync specifics, see [`context/notifications-in-activity.md`](./notifications-in-activity.md).

Also see [`context/testing.md`](./testing.md):

- Run the normal Go tests with `-shuffle=on`.
- If you change GraphQL query shape in `internal/github/graphql.go`, run the
  gated live GitHub validation as well.
