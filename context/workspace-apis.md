# Workspace APIs

Use this document for changes to workspace creation, reuse, identity, routes,
or item-to-workspace association.

These APIs manage **kenn-forge-owned workspaces**: durable local execution
contexts for tracked PRs, provider issues, mapped Kata tasks, and ad-hoc work in
a tracked repository. They are not a generic Git worktree browser and not an
embedder protocol for arbitrary host state.

## Purpose

- Persist a kenn-forge workspace entry for a tracked item.
- Materialize that entry as a local Git worktree plus tmux session.
- Let the UI reopen the same workspace from `/workspaces` or `/terminal/:id`.
- Carry enough item metadata to render the correct sidebar behavior.
- Persist provider workspaces by the internal repository catalog ID. Route
  requests resolve their current occupant before lookup or creation; a rename
  follows the same repository, while route reuse creates a separate workspace
  identity. Migration backfills every unambiguous catalog route, including
  route-only repositories. Provider lifecycle code retires legacy rows through
  dirty-aware deletion and leaves failures stable for explicit user action; it
  must neither discard uncommitted work nor resolve them through the current
  occupant
  (`internal/server/workspaceapi/handler.go::New`,
  `internal/workspace/launch_spec.go::Manager.RequireWorkspaceLaunchSpec`).
- A repository referenced by a workspace is a durable identity tombstone.
  Provider-host purge deactivates that repository and releases its current
  route instead of deleting it or nulling the workspace `repo_id`
  (`internal/db/queries.go::DB.PurgeOtherHosts`).
- Stable-identity migration keeps the newest workspace when pre-rename and
  post-rename routes contain the same item, then deletes the older workspace
  and its owned state instead of adding another legacy path (`internal/db/migrations/000055_workspace_repository_identity.up.sql:15`).
- Setup verifies the stable repository ID independently of its mutable route.
  Managed clones partition storage by that ID, and configured bases must match
  it; route reuse must not share checkout state. Network Git work also captures
  the route generation and fails closed if that route changes before setup
  completes, restoring refs and retargeted origins after a rejected fetch.
  Recovery and cleanup discover identity-scoped clones across every route
  owned by that repository; network reuse retargets a historical origin to the
  fenced current route
  (`internal/workspace/manager.go::Manager.workspaceSetupGitDir`,
  `internal/gitclone/clone.go::Manager.EnsureCloneValidated`,
  `internal/server/settings_handlers.go::Server.worktreeBasePathForRepo`,
  `internal/workspace/manager.go::Manager.workspaceManagedCloneCandidates`).
- A backfilled workspace may keep a route-keyed managed clone from any current
  or historical route that has one stable owner; route reuse excludes that path
  (`internal/workspace/manager.go::Manager.workspaceManagedClonePaths`).
- Keep Workspace and Projects request state below the root server composition
  boundary. The handler receives deep-copied committed config snapshots; it
  never retains the root mutable config pointer or mutex
  (`internal/server/workspaceapi/config.go::ConfigSnapshot`).
- Construct Workspace manager, runtime, tmux, clock, and enrichment policy
  before handler startup; production and test callers must not mutate
  dependencies or test controls after `Start` (`internal/server/workspaceapi/handler.go::Deps`).
- Workspace clock overrides remain scoped to the handler; replacing the root
  server clock also changes unrelated domain timestamps
  (`internal/server/server.go::newServer`).
- Fleet consumes Workspace-owned summary and runtime snapshots, never the
  Workspace manager or root server receiver
  (`internal/server/workspaceapi/fleet_snapshot.go::FleetSnapshot`).
- Managed clones keep `core.bare=true` in shared config and override it per
  linked worktree; repository tools use the shared value to identify this layout
  (`internal/workspace/manager.go::configureBareLinkedWorktree`).
- Managed clones and existing local base checkouts use the same exact
  provider-host cleartext acknowledgement when validating their canonical remote; local
  bases do not silently weaken or ignore the configured transport policy
  (`internal/workspace/manager.go::ValidateWorktreeBasePath`).
- Configured bases resolve and authenticate their read remote by repository identity,
  rejecting ambiguous matches or tracking-namespace overlap.
- Credentialed named-remote operations validate every URL and authenticate `origin`'s
  repository. A first push may create a missing fork branch but never force-update it
  (`internal/gitclone/clone.go::RunGitForNamedRemote`, `internal/workspace/branch_sync.go::pushWorktreeBranch`).

## Endpoint Intent

- `POST /workspaces`: create or reuse a PR-backed workspace.
- `POST /issues/{provider}/{owner}/{name}/{number}/workspace`, with
  `/host/{platform_host}/...` for non-default hosts: create or reuse an
  issue-backed workspace; these start from the repo's current `origin/HEAD`,
  not from a PR head branch.
  - Directory recovery accepts no path and applies only when the workspace row
    is absent; an occupied deterministic path conflicts with its actual branch,
    and choosing another branch cannot relocate it (`internal/workspace/manager.go::Manager.CreateIssue`).
  - Recovery validates repository provenance before persistence and again during
    setup; the managed clone's deterministic path is not identity without a
    matching origin (`internal/workspace/manager.go::Manager.existingWorktreeUsesManagedClone`).
  - Pending recovery uses a Git-invalid branch marker and must adopt that
    directory without create/cleanup fallback; retry/delete preserve it until setup
    publishes the real branch and ready status (`internal/workspace/manager.go::workspaceRequiresExistingDirectory`).
- `POST /repo/{provider}/{owner}/{name}/workspaces`: create or reuse an ad-hoc
  workspace for new work with no source item. Its branch is its identity: the
  item key is `adhoc:<branch>` and `item_number` stays 0, so item-key fallbacks
  derived from the number must exclude this type
  (`internal/db/queries.go::workspaceItemTypeKeysByNumber`). Requesting the same
  branch twice returns the existing workspace. Local branch and ref-namespace
  conflicts use a four-character random hash suffix as the persisted identity;
  database collisions retry with another suffix
  (`internal/workspace/manager.go::Manager.persistAdHocWorkspace`). Setup never
  rewrites that identity: a later Git ref collision fails explicitly.
  Once its branch is pushed the monitor links a PR into `associated_pr_number`
  (`internal/workspace/monitor.go::workspacePRMonitorEligible`); that number is
  the workspace's item identity for display, links, and search, so surfaces must
  never gate PR affordances on `item_type == "pull_request"` alone.
  A hub may route this creation to itself or a writable spoke; a spoke offers
  only itself as an execution target (`internal/server/fleetapi/fleet_proxy.go::Handler.registerFleetOperationRoutes`).
- `GET /kata/daemons/{daemon_id}/references`: search canonical Kata issue references on an explicitly pinned daemon for link and workspace creation.
- `GET /kata/daemons/{daemon_id}/issues/{issue_uid}`: return one read-only issue detail envelope plus the daemon health schema version. Forge does not cache or persist the task payload.
- `GET /kata/daemons/{daemon_id}/issues/{issue_uid}/launch-target`: resolve Kata’s safe external browser target.
- `POST /kata/workspaces`: create or reuse a Kata-task-backed workspace. Kata
  tasks are not provider issues, so this path never resolves or syncs a
  provider issue row.
  - Reuse identity is daemon ID plus issue UID, independent of the task's current
    project; project moves must reopen the existing workspace
    (`internal/db/queries.go::DB.GetKataWorkspaceByIssue`).
  - Serialize lookup and persistence for that identity; concurrent creates across
    project mappings must not materialize duplicate workspaces
    (`internal/server/kata/workspace.go::Handler.createKataWorkspace`).
- `GET /workspaces`: list kenn-forge's persisted workspaces for the workspaces
  page and terminal picker.
- Terminal and mobile pickers read inline workspaces from the projected snapshot;
  they never fan out per-host list reads, and remote actions require advertised
  availability (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::loadWorkspaces`).
- A hub may proxy workspace operations to an active member by stable
  node ID. A spoke may operate on `self` only; other spokes remain summary-only.
  The same workspace therefore has local affordances on its owner, routed
  affordances on the hub, and no mutation affordances on another spoke
  (`internal/server/fleetapi/fleet_proxy.go::Handler.resolveFleetHostTarget`).
- Spoke raw workspaces carry stable repository identity but no provider overlay or
  local numeric repository ID; the hub owns enrichment
  (`internal/server/workspaceapi/fleet_snapshot.go::Handler.FleetSnapshot`).
- `GET /workspaces/{id}`: load one persisted workspace for terminal view.
- Provider-backed workspaces have a one-to-one, versioned launch specification
  containing stable repository identity, clone/default-branch facts, item
  identity, PR head-repository semantics, and a hub-issued source
  visibility lease. Workspace and specification creation is one transaction;
  reads never reconstruct a missing specification from provider cache tables
  (`internal/db/queries_workspace_launch_specs.go::DB.CreateWorkspaceWithLaunchSpec`).
- Preparation inventory must close its SQLite result cursor before per-workspace
  repository or launch-spec reads; the bounded read pool cannot nest these queries
  (`internal/db/queries_workspace_launch_specs.go::DB.ListUnpreparedProviderWorkspacesAt`).
- PR and issue create routes resolve that specification before persistence and
  validate its exact request, Git identity, credential route, and mutable route
  occupancy. The atomic workspace/specification commit precedes asynchronous
  setup. Issue branch overrides, existing-branch/directory reuse, worktree base
  paths, and automatic-assignment suppression remain spoke-local request policy
  and are not smuggled into hub provider facts
  (`internal/providerplane/client.go::ValidateWorkspaceLaunchSpecResponse`,
  `internal/server/workspaceapi/routes_handlers.go`).
- Source visibility leases last exactly 15 minutes. They are usable only while
  `now < source_visible_until`; expiry requires a hub refresh, and an
  explicitly hidden source remains unavailable. Branch-reuse and worktree-path
  request intent is local and is never persisted in the provider fact envelope
  (`internal/db/types.go::WorkspaceLaunchSpec.RequireVisible`).
- During hub outage, provider-backed creation and expired launch-spec
  work fail with `hubUnavailable`; the spoke must not fall back to stale
  provider replicas. Existing local execution plus ad-hoc and Kata creation
  remain available because their authority is spoke-local
  (`internal/server/provider_sources.go`, `internal/workspace/launch_spec.go`).
- Workspace creation is event-confirmed rather than response-owned. After accepting a purpose payload, the backend emits
  `workspace_created` with the persisted ID and a `created` boolean that distinguishes a new workspace from task-scoped
  reuse; clients load that ID through the generated workspace API to recover canonical identity.
- The created event precedes asynchronous setup status and duplicate delivery is idempotent. When replay is stale, list
  reconciliation recovers persisted identity and status without replaying launch or navigation presentation.
- List/detail reads return persisted plus last-known-good enrichment without
  foreground git or tmux probes; stale components reconcile through bounded
  background workers (`internal/server/workspaceapi/workspace_enrichment.go::toCachedWorkspaceResponse`).
- Background Git enrichment must not take optional repository locks; cached
  probes cannot interfere with a maintainer's foreground Git commands
  (`internal/server/workspaceapi/routes_handlers.go::readOnlyWorktreeIsDirty`).
- Activity and Issue/Pull Request list and detail responses share one cached
  subject snapshot: every resolvable subject keeps a workspace reference, while
  tmux activity remains optional and ephemeral (`internal/server/workspaceapi/subject_activity.go::Handler.WorkspaceSubjectSnapshot`).
- Spokes overlay their subject snapshot onto hub pull, issue, and Activity
  rows by stable provider identity, preserving membership, order, and cursors
  (`internal/server/pullapi/services.go::Handler.overlayLocalPullWorkspaces`).
- Assignment-dependent filters on spoke workspace overlays must use hub state;
  spoke provider rows are optional and cannot decide filter membership
  (`internal/server/provider_activity_subjects.go::Server.federationFilterUnassignedActivitySubjects`).
- Workspace enrichment is best-effort on Issue/PR detail: snapshot failures log
  and omit optional workspace metadata rather than hiding a valid item; list and
  Activity reads stay fail-fast because the snapshot affects ordering and identity (`internal/server/pullapi/routes.go::Handler.buildPullDetailResponse`, `internal/server/issueapi/routes.go::Handler.BuildDetail`).
- Only enabled workspace recency merges the snapshot into Threaded and Mobile
  Activity after provider-event filters, allowing eventless subjects; disabled
  mode leaves every Activity mode provider-event-only
  (`frontend/src/lib/views/MobileActivityView.svelte::visibleWorkspaceActivity`).
- In enabled mode, Activity number search uses the same `#number` shape for provider events,
  notifications, and eventless workspace subjects; matching provider events keep
  workspace recency across incremental polls (`internal/server/huma_routes.go::Server.workspaceActivityResponse`).
- Enabled workspace recency lets Activity author filters and candidates match
  eventless workspace subjects in the same repository and time scope; disabled
  mode returns provider authors unchanged
  (`internal/server/huma_routes.go::Server.activityAuthorsWithWorkspace`).
- Activity events and parent summaries key that snapshot by stable repo ID and
  canonical item type; normalize wire `"pr"` to workspace `"pull_request"`
  before lookup so route reuse stays fail-closed (`internal/server/helpers.go::workspaceItemTypeFromActivity`).
- The shared subject snapshot holds the repository-reconciliation read barrier
  across both its workspace-summary and subject-metadata reads, so a route move
  cannot split one response across repository identities
  (`internal/server/workspaceapi/subject_activity.go::Handler.WorkspaceSubjectSnapshot`).
- Hub and standalone Activity reads hold one reconciliation barrier
  across events and workspace subjects; spokes overlay a separate local snapshot
  by stable identity (`internal/server/huma_routes.go::Server.listActivity`).
- Subject metadata and opt-in Issue/PR activity ordering use JSON-backed SQLite
  relations, so retained workspaces cannot exhaust bind variables. Lists always
  expose `last_workspace_activity_at`; provider activity is authoritative by default
  (`internal/db/workspace_subjects.go::DB.ListWorkspaceSubjectMetadata`, `internal/db/queries.go::workspaceActivityCTE`).
- Association changes only activity identity: an issue keeps its own workspace
  affordance after its work resolves to a PR, while that PR receives the tmux recency
  candidate and uses it only when workspace recency is enabled
  (`internal/server/workspaceapi/subject_activity.go::Handler.WorkspaceSubjectSnapshot`).
- Tmux recency publishes immediately, then at most once per rolling minute per workspace;
  existing enrichment lazily publishes suppressed activity while live state stays
  responsive. Nil resets the throttle; older samples never regress it (`internal/server/workspaceapi/workspace_enrichment.go::recordCachedWorkspaceTmux`).
- Aggregate enrichment is fresh only after divergence and tmux both complete;
  partial results remain pending or stale
  (`internal/server/workspaceapi/workspace_enrichment.go::workspaceResponseFromEnrichmentCacheEntry`).
- Divergence and tmux freshness use only their own attempt and refresh times
  against separate cadences (30s divergence, 5s tmux); an unattempted
  component remains immediately due after unrelated component work
  (`internal/server/workspaceapi/workspace_enrichment.go::componentsDue`).
- Background divergence refreshes re-validate from a stat-only git-directory
  fingerprint and spawn git only when it moved or the forced interval elapsed;
  explicit refreshes and invalidation hooks always probe
  (`internal/server/workspaceapi/workspace_enrichment.go::workspaceEnrichmentCacheEntry.gitProbeSkippable`).
- Worktree-only edits never move the fingerprint; the first refresh a read
  requests after the forced interval runs git, and no daemon timer probes an
  unread workspace. Fingerprinting must never spawn git
  (`internal/workspace/worktree_fingerprint.go::WorktreeGitFingerprint`).
- Failed enrichment retains last-known-good values and component-owned errors;
  one component's success clears only its own error
  (`internal/server/workspaceapi/workspace_enrichment.go::recordWorkspaceEnrichmentResult`).
- Each workspace admits one background enrichment flight at a time; a full
  refresh upgrade waits behind active tmux work, and only the matching unique
  flight may release ownership (`internal/server/workspaceapi/workspace_enrichment.go::nextWorkspaceEnrichmentJob`).
- Overlapping tmux probes wait for the active sample within the caller budget;
  fallback carries an error only when waiting or sample production fails
  (`internal/server/workspaceapi/routes_handlers.go::probeOneTmuxSession`).
- Background completion emits `workspace_status` only for durable changes:
  first completion, divergence movement, or error-state change — never for
  tmux-activity-only movement, and tmux prune broadcasts only when it pruned.
  Unconditional broadcasts made every client refetch schedule the next
  enrichment, a permanent refresh loop
  (`internal/server/workspaceapi/workspace_enrichment.go::workspaceEnrichmentBroadcastWorthy`).
- Client detail stores mirror this: a background poll or sync whose payload is
  content-identical (ignoring fetch timestamps) must not replace displayed
  store state — equal-but-new objects re-render the whole panel every cycle
  (`frontend/src/lib/stores/detail.svelte.ts::applyRefreshedDetail`).
- `DELETE /workspaces/{id}`: tear down a kenn-forge-managed workspace and its
  local resources.
  - Every admitted deletion persists `deleting` before destructive work and
    `deletion_failed` on failure. Synchronous safe-delete rejection occurs
    before admission so a dirty workspace remains usable (`internal/server/workspaceapi/workspace_deletion.go::Handler.runWorkspaceDeletion`, `internal/server/workspaceapi/routes_handlers.go::Handler.DeleteWorkspace`).
  - Confirmed removal emits full provider/repository/item identity; merge and
    ordinary deletion share this lifecycle (`internal/server/workspaceapi/workspace_deletion.go::Handler.publishWorkspaceDeleted`).
  - Deletion admission is a compare-and-swap from stable `ready`, `error`, or
    `deletion_failed` state. A concurrent setup that wins the race and moves the
    row to `creating` receives a conflict instead of being overwritten; once
    deletion is admitted, retry requests also conflict until teardown succeeds
    or leaves an explicit `deletion_failed` row. Deletions remain
    reference-counted per workspace ID so one failed concurrent request cannot
    reopen setup while another owns teardown
    (`internal/db/queries.go::DB.BeginWorkspaceDeletion`, `internal/server/workspaceapi/routes_handlers.go::Handler.DeleteWorkspace`).
  - Delete conflicts use stable `worktreeDirty`,
    `workspaceSetupInProgress`, and `workspaceDeletionInProgress` problem
    codes. The server checks lifecycle state before and after dirty preflight;
    clients offer force deletion only for a dirty worktree and refresh into
    the authoritative lifecycle state for setup/deletion races
    (`internal/server/workspaceapi/routes_handlers.go::Handler.DeleteWorkspace`).
  - Setup rejects occupied destinations before clone/fetch and again under the
    repo lock before mutation. Branch creation and failed-add cleanup use ref
    compare-and-swap so changed branches survive (`internal/workspace/manager.go::createBranchAndAddWorktree`).
  - New registrations receive their workspace-ID marker before fallible post-add
    configuration; marker failure rolls back the exact registration and any unchanged
    created branch under the repo lock (`internal/workspace/manager.go::Manager.runOwnedGitWorktreeAdd`).
  - Existing-worktree reuse accepts only a non-symlink worktree root and revalidates
    its repository and provenance under the repo lock before refresh or ownership
    marking (`internal/workspace/manager.go::Manager.reuseExistingWorkspaceWorktree`).
  - Destructive worktree removal, including setup rollback, requires a matching
    persisted workspace-ID marker under the repo lock; preserve unmarked or mismatched
    roots (`internal/workspace/manager.go::Manager.rollbackWorktree`).
  - An unmarked live registration is ambiguous after upgrade: delete and retry return
    conflict and retain the workspace row; only registrations without a live worktree
    may be cleared as stale (`internal/workspace/manager.go::gitDirOwnsCleanupWorktree`).
  - Only exact Git roots whose `.git` matches their registration are live; symlinked
    roots may be live but are never owned, so they conflict instead of entering stale
    cleanup (`internal/workspace/manager.go::gitDirHasLiveWorktree`).
  - Pre-lock cleanup resolution is advisory. Revalidate marker identity and current
    live/stale state as the first locked action (`internal/workspace/manager.go::currentWorkspaceCleanupState`).
  - A force delete retains the workspace row when Git cannot remove a live owned
    worktree; only errors proving the worktree is already absent or corrupt may
    continue to branch and row cleanup (`internal/workspace/manager.go::Manager.cleanupWorkspaceArtifactsForDeleteLocked`).

## Data Model Intent

- `item_type`: whether the workspace belongs to a `pull_request`, provider
  `issue`, or `kata_task`.
- `item_key`: the canonical owner key within the repo/workspace namespace. PR
  and provider issue workspaces use the decimal item number as a string; Kata
  task workspaces use an opaque composite of Kata daemon ID, project UID, and
  issue UID so issue IDs from different Kata scopes cannot collide.
- `item_number`: the provider item number within the repo. For Kata task
  workspaces this is `0` and must not be used for owner identity.
- `git_head_ref`: the Git branch name kenn-forge opens in the worktree.
  Kata-task workspaces keep a readable slug from `short_id`, `qualified_id`, or
  issue UID, but the branch/worktree leaf must also include a short stable hash
  of daemon ID, project UID, and issue UID so project-scoped visible task IDs do
  not collide in the same watched repo.
- `item_last_activity_at`: the synced provider item activity timestamp for the
  owning PR or issue, when kenn-forge has that owner item row.
- Pull detail prefers a direct PR workspace, then the newest persisted issue,
  Kata, or ad-hoc PR association; status is not a selection input
  (`internal/db/queries.go::DB.GetWorkspaceLinkedToMRForProvider`).
- Merge-request sync limits head-repo trust writes to direct PR workspaces;
  association-only rows are presentation links, not sync write targets
  (`internal/github/sync.go::Syncer.reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead`).

These fields exist so PR-backed workspaces show PR/Reviews sidebars, while
issue-backed workspaces show the issue sidebar and disable the PR/reviews path.
Kata-backed workspaces expose their intrinsic Kata association in the contextual
Kata links panel; selected detail is read-only and rendered by the shared Kata UI
package.

Workspace summaries join the owning PR or issue row by full provider identity:
`platform`, `platform_host`, `repo_owner`, `repo_name`, `item_type`, and
`item_number`. A PR workspace uses `forge_merge_requests.last_activity_at`;
an issue workspace uses `forge_issues.last_activity_at`. Kata workspaces do
not join provider item tables and leave provider item activity absent. If the
owning provider item has not synced yet, the summary leaves
`item_last_activity_at` absent rather than inventing a value.

Kata task repository resolution is deliberately exact. Manual settings mappings
key by optional daemon ID plus Kata project UID and point to a known repository
identity, including registered kenn-forge Projects. Removing a watched repo does
not delete an override because a registered Project may still own that identity
(`internal/config/config.go::validateKataProjectRepoMappings`,
`internal/server/kata/workspace.go::Handler.kataManualWorkspaceTarget`). Automatic
resolution first uses watched exact repos with `worktree_base_path` whose clone
contains a matching `.kata.toml`. Matching first compares both explicit
identifiers, `project.uid` and `project.identity`, to the Kata project UID. If
either identifier matches exactly, that clone is a candidate; if more than one
clone matches, the result is ambiguous. Name fallback through `.kata.toml` is
only allowed per clone when that clone has no usable `project.uid` or
`project.identity`, and then exactly one case-insensitive `project.name` match
is required. If no `.kata.toml` mapping matches, the
resolver may fall back to a case-insensitive exact match between the Kata
project and exactly one non-stale registered kenn-forge Project with provider
identity; use `.kata.toml` before display/repository name. Distinct matching
registered checkout paths are ambiguous. A unique registered match carries its
checkout through workspace creation, while a configured clone carries its own
base path. Only then may one synced repo matched by exact
or globbed config and lacking readable project metadata resolve by name.
Ambiguous, mismatched, or missing matches hide the Create/Open workspace action;
the effective Kata link retains an actionable typed resolution reason
(`internal/server/kata/workspace.go::Handler.resolveKataWorkspaceRepoResolution`).

Settings lists each selected-daemon Kata project with the status and source from
the workspace resolver. Its selector lists repository identities known from
exact watched repositories, currently matched tracked repositories, or
non-stale registered Projects. It defaults only to an inferred identity match
and persists that repository identity
(`internal/server/kata/workspace.go::Handler.getKataProjectMappings`).

Persisted workspace `worktree_path` values should be absolute. Workspace setup
runs `git worktree add` from the managed clone or configured base checkout, so
relative paths would be interpreted relative to that Git directory while later
API reads interpret them relative to the kenn-forge server process.

Managed-clone hooks must carry the exact common Git directory and ownership flag
through new and reused worktrees; never infer ownership from paths or run them for
`worktree_base_path` repositories (`internal/workspace/manager.go::SetupWithOptions`).
After branch persistence, `repository_hooks` holds a fresh repository lock while
requiring Roborev to use common-directory hooks, resolving the trusted default only
from fetched remote refs, and configuring a worktree-specific snapshot exclude
without replacing explicit or implicit user exclusion rules. A custom effective
hooks directory is a setup error; shared hook changes roll back unless
registration succeeds.
Only regular root config files on that commit are trusted; a workspace Roborev
snapshot path must match it or use `.roborev`. Registration confirmation, final
route validation, and rollback stay under the repository lock; cache invalidation
follows lock release
(`internal/workspace/repository_hooks.go::Manager.setupManagedRepositoryHooks`).

Keep Git worktree and merge-request lifecycle semantics in
`go.kenn.io/kit/git/managed`; kenn-forge supplies application policy instead of
maintaining a local lifecycle fork (`internal/server/projects_handlers.go::createWorktreeOnDisk`).
Classify same-repository merge requests with the provider-hosted project
identity, not the effective origin URL: the origin may be a local mirror
(`internal/server/projects_handlers.go::createProjectWorktreeFromMergeRequest`).

All workspace API timestamps are emitted as UTC RFC3339 strings. Keep timestamp
normalization in the DB/server boundary; the Svelte UI can present local time
where needed.

## Agent Launch Context

Agent launch selects Codex, Pi, and Claude families by case-folded target-name prefix.
Codex and Pi receive generated workspace context followed by root `AGENTS.md` verbatim
in `AGENTS.override.md`; only a non-symlink regular file up to 1 MiB is appended,
otherwise the override is context-only (`internal/workspace/agent_context.go::readRepositoryAgentInstructions`).
Claude receives context-only `CLAUDE.local.md` because its local file is additive
(`internal/workspace/agent_context.go::agentContextRelPath`).
No instruction file is written during setup.

The first-line marker owns refreshes; ownership detection is root-confined and
reads only the bounded marker prefix. kenn-forge updates only marked files.
Unmarked `AGENTS.override.md`/`CLAUDE.local.md` files, symlinks, and root
`AGENTS.md`/`CLAUDE.md` stay untouched. The content carries source identity
(kind, repo, item number, URL) and PR push target facts agents cannot read from
the worktree. Source-system prose (titles, Kata project names) is XML-escaped
inside `<untrusted-source-text>` fences — the prompt-injection boundary.
External identifiers are only normalized to one line, which preserves Markdown
structure and is not a trust boundary; new free-prose fields must go through the
fence.

Before writing, kenn-forge ignores the generated path through the worktree's
private exclude file, not tracked `.gitignore`. If the path would remain
visible to Git, the write fails.

Head-repo trust for a provider-backed workspace is projected from its persisted
launch specification on every lifecycle admission. Setup, retry/recovery,
branch synchronization, agent launch, and hook-rendered worktree context fail
instead of continuing when that projection cannot be validated or persisted
(`internal/workspace/launch_spec.go::Manager.RequireWorkspaceLaunchSpec`). The
hub's provider-neutral sync engine still funnels accepted MR snapshots,
including action responses, through one choke point that reclassifies tracking
workspace rows from the post-upsert snapshot; an unknown-head snapshot cannot
downgrade an already-known fork classification. This cache projection is
best-effort, while the launch-specification lifecycle path is fail-closed
(`internal/github/sync.go::CommitMergeRequestParentSnapshot`,
`internal/github/sync.go::reclassifyWorkspaceHeadRepoTrustUnderRepositoryReconciliationRead`,
`internal/workspace/manager.go::WorkspaceHeadRepo`).

Head-repo classification reads and writes stay on the workspace repository ID;
persisted provider workspaces without one fail unresolved. Parent snapshot
commits use the per-MR snapshot lock; repository-ID reconciliation holds the
exclusive side of the stable barrier that every snapshot lock holds shared, so
moving an MR cannot change its lock identity during a snapshot commit
(`internal/workspace/manager.go::Manager.RefreshWorkspaceHeadRepoSnapshot`,
`internal/db/queries.go::UpdateWorkspaceMRHeadRepoForSnapshot`).
Launch-spec refresh preserves the workspace's stable repository and branch
identity while renewing hub-owned head and visibility facts. A changed
repository identity conflicts; an expired lease followed by a hub
outage is retryable, while removed or inaccessible PRs fail closed before generated
context can expose a branch or push target
(`internal/server/provider_state_handoff.go::Server.RefreshWorkspaceLaunchSpec`,
`internal/workspace/agent_context.go::PrepareAgentLaunchContext`).
Branch push and pull use the workspace route returned by launch-spec
validation, never a route captured before a lease refresh
(`internal/workspace/branch_sync.go::Manager.validateBranchSyncLaunchSpec`).
Persisted-workspace refreshes reload by workspace ID while holding that same
barrier through classification persistence, so repository renames cannot turn
a known head into `unknown` (`internal/workspace/manager.go::RefreshWorkspaceHeadRepoSnapshot`).
Head-trust refresh and generated-context rendering both recheck removed-upstream
visibility; removed PRs contribute no provider title, URL, branch, or push target
(`internal/workspace/agent_context.go::PrepareAgentLaunchContext`).
Workspace summaries resolve identified repositories by stored stable ID, so
renames retain provider projections while inactive repositories do not.
Unresolved legacy rows require an unambiguous active route
(`internal/db/queries.go::workspaceSummaryJoins`).

Workspace creation launches an agent only after an explicit target choice on
the create split button. The one-shot target is reactive session state keyed by
workspace ID; primary creation never launches, while an explicit fork-PR choice
shares the ordinary manual Launch-menu trust boundary
(`frontend/src/lib/stores/workspace-create-pending.svelte.ts::queueWorkspaceLaunch`).
The launch API accepts only the target and display region. Agent launches
validate the persisted launch specification and renew an expired visibility
lease while preparing generated context. A retryable hub outage or a
hidden source blocks that provider-backed launch; ad-hoc and Kata launches keep
their local behavior
(`internal/server/workspaceapi/routes_handlers.go::Handler.launchWorkspaceRuntimeSession`,
`internal/workspace/agent_context.go::Manager.PrepareAgentLaunchContext`).
Each recorded workspace agent runtime contributes one durable launch event per
session; runtime cleanup never erases this preference history
(`internal/db/workspace_agent_usage.go::DB.RecordWorkspaceRuntimeSession`).
Agent settings use the `agents` and `launch_targets` fields from `GET /settings`;
the shared create split control consumes agent targets from the hydrated store
(`frontend/src/lib/components/workspace/WorkspaceCreateSplitButton.svelte::agentTargets`).
A save replaces both fields from its response so command fixes update
availability without a reload
(`frontend/src/lib/components/settings/AgentSettings.svelte::save`).
Startup (`frontend/src/lib/utils/appStartup.ts`) and a
`config_changed` SSE hot reload (`frontend/src/lib/Provider.svelte::reloadSettingsAfterConfigChange`,
via `frontend/src/lib/stores/settings-hydration.ts::applySettingsHydration`)
both hydrate the same `GET /settings` fields into the settings/activity/issues
stores from two separate call sites — a field added to one path and not the
other silently goes stale after a config edit until the next full page load.

Workspace create endpoints may return 202 with a pre-existing workspace
(`internal/server/workspaceapi/routes_handlers.go::createIssueWorkspace`).

- Automatic self-assignment is opt-in and applies only after a new PR/issue
  workspace is persisted; preserve current assignees and never roll back the
  workspace on upstream failure (`internal/server/workspaceapi/auto_assign.go::Handler.autoAssignWorkspaceItem`).

## Agent Activity Hooks

- Kit owns hook profiles, config mutation, payload normalization, and native responses;
  thin clients relay normalized events and the daemon owns activity transitions
  (`cmd/kenn-forge/agent_hook.go::agentHookRelay`, `internal/server/workspaceapi/agent_hook.go::Handler.receiveAgentHook`).
- Claude `SessionStart` context is regenerated from persisted workspace metadata,
  never read from instruction files (`internal/workspace/agent_context.go::Manager.RenderAgentContextForWorktree`).
- User-level install and uninstall target all kit profiles by default or one
  profile with `--agent`; kit preserves unrelated handlers and never enables
  agent consent or auto-approval (`cmd/kenn-forge/agent_hook.go::installAgentHooks`).
- Matching live runtime/worktree reports prioritize approval, input, working, done, then idle.
  Agent status row activation preserves completion, its hook timestamp, and sort position. Outside that sort, activation acknowledges a versioned completion for the browser-tab session, while a new timestamp resurfaces (`internal/agentactivity/store.go::statePriority`, `frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::openWorkspace`).
- Hook installs require absolute data roots; kit preserves config symlinks, while
  report/worktree matching uses canonical paths (`cmd/kenn-forge/agent_hook.go::installAgentHooks`,
  `internal/agentactivity/store.go::canonicalWorkspacePath`).
- The active sidebar polls every five seconds, and hook receipt fails open (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::onMount`, `cmd/kenn-forge/agent_hook.go::receiveAgentHook`).

## Diff Scopes

Workspace diffs compare against local `HEAD`, the pushed branch, or a merge
target. The merge-target scope exists only when the server can resolve a real
merge target branch, not merely when the workspace carries a PR identity.
Resolution requires all of: a positive PR number (PR-backed workspaces use their
own `item_number`; issue-backed, Kata-backed, and ad-hoc workspaces use
`associated_pr_number`), a synced repo row, a synced merge request row, and a
non-empty base branch on that row. When any of those is missing the API returns
"workspace merge target branch not available" and treats it as the
non-actionable state.

The server is authoritative for availability. The sidebar hides the
merge-target-dependent controls (both the Target scope control and the commit
range picker) whenever the workspace has no PR identity, which is necessary but
not sufficient: a workspace whose PR identity is present but whose merge request
row is unsynced, removed, or has no base branch can still surface those controls
and then receive the unavailable response. Clients must treat the unavailable
response as expected rather than an error, and a future change should expose a
resolved-merge-target signal on the workspace summary so the UI gate matches the
server check exactly.

## Diff Snapshot Coherence

- Files and patches project from one immutable snapshot. Preview membership is
  revision-pinned too, but new-side bytes remain live and may move afterward
  (`internal/server/workspaceapi/workspace_diff_cache.go::workspaceDiffCache`).
- Cache entries are stale-while-revalidate with last-known-good fallback.
  `jellydator/ttlcache/v3` owns entry storage, TTL expiration, and inactive-entry
  cost pressure through separate protected and cost-limited pools; kenn-forge's
  wrapper owns snapshot coherence, bounded validation, selection leases, and
  publication. Only selected workspaces
  receive proactive refresh leases; ordinary entries validate on demand
  (`internal/server/server.go::streamEvents`).
- A workspace response is user-visibly stale only when a bounded, coalesced
  head-only probe confirms cached/current Git HEAD mismatch and queues refresh;
  cache age, probe timeout, and resolution failure do not warn (`internal/server/workspaceapi/workspace_diff_cache.go::workspaceDiffCache.Get`).
- Local workspace selection reuses the singleton provider SSE connection;
  workspace subscribers attach before `workspace_id` selection and release
  with it (`frontend/src/lib/stores/events.svelte.ts::createEventsStore`).
- Late workspace subscribers receive current connected state, so route mount
  does not depend on a future reconnect (`frontend/src/lib/stores/events.svelte.ts::subscribeWorkspaceEvents`).
- Workspace event fan-out is lossless while subscribed; an event burst must not
  terminate refresh lifecycles (`frontend/src/lib/components/terminal/workspace-event-stream.ts::workspaceEventStream`).
- The server subscribes that stream before acquiring the selection lease, then emits
  `workspace_diff_ready` only when cold/coalesced default-HEAD preparation
  completes; a warm cache hit needs no readiness event. This ordering prevents
  fast preparation from racing ahead of the selecting browser.
  `workspace_diff_ready` belongs to selection lifetime and never cancels an
  active read; only `workspace_diff_changed` advances the visible refresh token
  (`frontend/src/lib/components/terminal/workspace-event-stream.ts::WorkspaceEventSignal`).
- Fleet selection uses `/workspaces/{id}/diff/watch` through the fleet proxy to
  hold the remote lease, prewarm HEAD, and relay opaque versions. Switching
  aborts and replaces the watch, so only the selected remote workspace refreshes
  (`internal/server/workspaceapi/routes_handlers.go::watchWorkspaceDiff`). Empty or foreign
  tokens return `changed=true`; matches wait 25 seconds and timeout unchanged.
  A `409` while the workspace is still being created is transient and the client
  retries it with the normal watch backoff; unsupported watch responses remain
  terminal and fall back to request-driven diff loading.
- Workspace switching keeps runtime and shell reads on their own critical path.
  The previous sidebar is replaced immediately by a neutral placeholder. The
  new diff panel mounts after matching workspace metadata and either matching
  runtime state or a terminal runtime error, so a runtime API failure cannot
  leave workspace details hidden forever. Panel cancellation uses a per-load
  token in addition to workspace identity: cleanup from an older same-workspace
  load must not abort its replacement.
- Manual workspace refresh schedules asynchronous validation for every cached
  key belonging to that workspace, whether or not the workspace currently has
  a local selection lease, even when provider refresh later fails. Workspace
  responses and runtime readiness never wait on Git. Failure preserves the
  last-known-good snapshot; preserving browser refreshes retry with capped,
  cancelable backoff only when retained files and diff share a snapshot version,
  while cold loads expose blocking errors
  (`frontend/src/lib/stores/diff.svelte.ts::loadWorkspaceDiff`).
  A changed fingerprint publishes through `workspace_diff_changed` when ready.
  Watcher hints validate selected keys even inside the freshness interval;
  periodic validation makes stale snapshots eligible for refresh, while the
  bounded queue is not a hard completion deadline
  (`internal/server/fleetapi/fleet_worktree_links.go::Handler.notifyWorktreeStatsChanged`). One background worker
  serializes proactive validation; foreground cold reads bypass that queue.
  Entryless cold failures stay with selection prewarm's five-second retry;
  periodic validation handles only published entries, so its one-second cadence
  cannot bypass cold backoff
  (`internal/server/workspaceapi/workspace_diff_cache.go::validateSelected`).
- The 128 MiB inactive-cache budget evicts least-recently-used inactive
  entries, never active snapshots. A newly published snapshot has a one-minute
  files/diff revision lease so an oversized `/files` response cannot evict
  itself before its pinned `/diff` read. Selected keys stop being protected
  after 10 minutes without access. Selected and pair-retained snapshots have
  zero eviction cost and may temporarily put the total working set above the
  inactive-cache budget
  (`internal/server/workspaceapi/workspace_diff_cache.go::maintainLocked`).
- Publishing a dirty-worktree snapshot requires matching before/after resolved
  refs and fingerprints; repository-local attributes are fingerprint inputs.
  Commit/range generated-file checks use the resolved head commit as the Git
  attribute source, while live worktree snapshots intentionally use worktree
  attributes (`internal/workspace/diff_snapshot.go::PrepareDiffSnapshot`).
- Whitespace-only classification is post-processing over aggregate Git output;
  raw mode/type changes remain substantive even when content differs only in
  whitespace. Ambiguous multi-hunk files compare complete old/new record
  sequences in Go. One batch Git process streams old blobs into ordered
  whitespace-normalized record digests, keeping memory bounded without per-file
  subprocesses (`internal/workspace/diff_whitespace.go::readWhitespaceBlobDigests`).
- Untracked-file content is required for binary detection, line totals, and
  synthetic patches, but snapshot preparation does not read an unbounded path
  list serially. Tracked/untracked fingerprint hashing and untracked patch
  construction share one file-read budget sized from the Go runtime's host
  parallelism at process start, so cache validation cannot multiply I/O
  concurrency. Results retain path order and cancellation propagates between
  files and read chunks
  (`internal/workspace/diff.go::untrackedReadPool.run`).
- Snapshot versions are opaque equality tokens. Clients may compare only for
  equality; ordering and replay position come from SSE event IDs, not from
  parsing the version or revision fields. A typed `snapshot_changed` preview
  conflict reloads the coherent files/diff pair once only while the preview's
  captured load token and generation still own the store; stale preview work
  fails without mutation
  (`frontend/src/lib/stores/diff.svelte.ts::loadWorkspaceFilePreview`).
- Preserving refreshes publish only a fresh coherent files/diff pair; stale
  responses retain the visible pair and retry because same-fingerprint
  validation emits no change event
  (`frontend/src/lib/stores/diff.svelte.ts::loadWorkspaceDiff`).
- Workspace diff and preview paths identify the current path first. Old paths
  are fallback aliases only, since a rename source can coexist with a new file
  at that path (`internal/server/workspaceapi/routes_handlers.go::filterWorkspaceDiffSnapshotPath`).
- Live worktree reads use Go's `os.Root` containment. Final symlinks are read as
  links, regular files are identity-checked across the open, and intermediate
  symlinks may resolve only within the worktree. Untracked patch reads and
  fingerprints use the same rooted opens, reject non-regular files, and remain
  cancellable while hashing; cached diff membership never authorizes traversal
  (`internal/workspace/diff_snapshot.go::fingerprintWorktreePath`).

## Worktree Branch Names

An unavailable branch name must never fail workspace creation: an unusable PR
head branch degrades to `kenn-forge/pr-<n>`, then to a numbered variant of it,
then to a detached checkout with no managed branch
(`internal/workspace/manager.go::addFallbackWorktree`). kenn-forge owns the
synthetic name and its numbered variants and may delete them during cleanup;
any other pre-existing branch is user-owned and must keep pointing where it did.
Ad-hoc workspaces instead reserve their final hashed branch identity before
setup; a later external Git ref collision is an explicit setup error.
PR worktree creation and reuse fast-forward the fetched local base branch while
holding repository route identity stable; checked-out, diverged, or ref-namespace-
blocked base branches stay untouched and emit a warning
(`internal/workspace/manager.go::syncLocalBaseBranch`).

## Branch Upstream

The branch's git upstream config (`branch.<name>.remote`/`.merge`) is the
single source of truth for every sync-derived workspace surface:
`commits_ahead`/`commits_behind` in the list response, the sidebar
ahead/behind arrows, push, pull, and unpushed-commit flags. All of them
silently report nothing when the upstream is not configured. A configured upstream
whose local tracking ref is missing exposes `branch_upstream_missing`, so the UI can
offer the push that verifies or creates its remote branch. Every path that creates a
PR-owned branch should configure its upstream when repository identity is known.
Passive workspace enrichment never contacts remotes, so the UI offers Push only for a
missing local tracking ref or commits ahead. A zero-ahead branch deleted remotely remains
in-sync until local Git state changes (`internal/server/workspaceapi/routes_handlers.go::applyWorktreeDivergence`).
Issue, Kata, and ad-hoc workspaces create new untracked branches; a
same-named remote ref is not authority to adopt an upstream
(`internal/workspace/manager.go::configureFallbackBranchUpstream`). PR upstream
wiring requires a non-empty head-repository identity whose
provider, host, and full repository path match the base repository; matching
commit SHAs are not identity evidence because forks preserve commit IDs. Fork
heads stay untracked, while unknown heads fail closed before Git access. The
pushed-head observer may repair a missing upstream only when a current
hub candidate proves the head is in the base repository, the
checked-out branch is the PR head or synthetic branch, and the remote-tracking
ref exists.

Test fixtures exercising a provider-backed lifecycle must seed the matching
launch specification even when they also seed hub PR rows. A same-repo
specification requires the synthetic `refs/pull/<n>/head` on the fixture base
remote; an unknown-head specification fails closed before Git, while a fork
specification carries the exact fork clone URL and branch that setup must fetch.

## Pushed-Head Refresh Convergence

A tracking-ref/provider-head mismatch is not proof of a local push: when the
PR advanced from another checkout, the local tracking ref is the stale side
and a provider sync can never converge. The observer therefore enqueues a PR
sync on mismatch, retries on failure after `pushedHeadRefreshRetryInterval`,
but must stop once a refresh for the same observed SHA succeeded and the
provider head still differs (`LastRefreshSucceededAt >= LastRefreshEnqueuedAt`)
— otherwise the visible PR is re-synced and re-rendered forever. A tracking-ref
move restarts the cycle.

The observer's read path spawns no processes: it reads branch, upstream, and
remote-tracking state in process through go-git, and only upstream repair runs
git. Do not reintroduce per-pass git subprocesses there
(`internal/workspace/pushed_head_gitdir.go::gitdirRemoteHeadReader`).

## Sidebar Ordering

The workspace sidebar has two separate activity concepts:

- `Activity`: terminal/runtime activity, ordered by `tmux_last_output_at` with
  `created_at` as the fallback.
- `Item activity`: provider item activity, ordered by `item_last_activity_at`
  with `created_at` as the fallback.

- `Agent status` groups approval, input, working, done, idle, then unlabeled
  rows without a report; equal states use hook recency, then item activity, then creation
  (`frontend/src/lib/components/terminal/workspaceListSort.ts`).
- Each flat sort shows the timestamp that drives its order as an unlabeled relative time
  (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::workspaceRow`).

Keep these modes distinct. Do not relabel `Activity` to mean provider PR/issue
activity, and do not add compatibility aliases for old sort values without an
explicit migration reason.

`Org / repo` is the grouped ordering mode. Timestamp sorts are flat lists, with
ties broken deterministically by workspace ID so the visible order does not
shift between refreshes.

## Testing Expectations

Workspace API changes that alter summary fields or sorting inputs need coverage
at the boundary a client observes:

- DB summary tests should prove PR-backed, issue-backed, Kata-backed, and
  unsynced-owner workspaces expose the expected `item_last_activity_at` shape.
- Server/API tests should assert `/api/v1/workspaces` returns the generated JSON
  field for synced owner items and omits it for missing owner rows.
- Frontend sidebar tests should cover the relevant sort mode and fallback.
- Visible workspace sidebar changes need affected Playwright coverage before
  pushing.

## Non-Goals

- Represent arbitrary worktrees discovered on a host machine.
- Mirror an external workspace tree or host inventory.
- Serve as a generic Git automation API outside kenn-forge's workspace lifecycle.

## Related context

- [`context/workspace-runtime-lifecycle.md`](./workspace-runtime-lifecycle.md)
  documents runtime-session exit, tmux persistence, and destructive ordering
  rules that sit underneath these APIs.
