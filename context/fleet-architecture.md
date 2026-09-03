# Fleet Architecture

Use this document for federation settings, snapshot aggregation, spoke routing,
or remote workspace and session operations.

## Ownership And Topology

- Every data directory has one random 128-bit lowercase-hex node ID; hostnames,
  listener addresses, and mutable display names never establish fleet identity
  (`internal/runtimelock/node_id.go::EnsureNodeID`).
- Hub and spoke identities must be distinct; enrollment rejects the hub's own
  node ID before creating durable membership (`internal/federation/enrollment_store.go::Store.Begin`).
- `fleet.role` defaults to `hub`. A `spoke` requires a validated
  hub binding; enabling federation requires API authentication, the
  root base path, and a canonical HTTPS `fleet.base_url`
  (`internal/config/config.go::Fleet.Validate`, `internal/config/config.go::Config.validate`).
- A fleet is a one-hop execution view over independent kenn-forge daemons, not
  a replicated database. Each host remains authoritative for its local
  repositories, workspaces, runtimes, and execution mutations.
- Federation requires mutually reachable canonical HTTPS origins, not a
  private-network product; Tailscale Serve and operator-managed private ingress
  use the same protocol and peer credentials
  (`internal/fleetsetup/setup.go::Runner.Plan`).
- Provider repository settings come from the hub, but each spoke owns
  and persists its stable-identity `worktree_base_path`; mutable or reused routes
  cannot move the override to another repository (`internal/config/config.go::Repo`,
  `internal/server/settings_handlers.go::Server.updateConfiguredRepoWorktreeBasePath`).
- Settings-carried repository observations use hub catalog timestamps;
  spoke clocks never advance route freshness (`internal/server/federation_provider_settings.go::Server.buildProviderSettingsProjection`).
- Disabling federation must leave local settings readable so the operator can
  re-enable it; hub-owned settings may be absent while disabled
  (`internal/server/settings_handlers.go::Server.getSettings`).
- Raw snapshots contain only producer-local facts; they never contain fetched
  aggregates or observer permissions (`internal/server/fleetapi/fleet_adapter.go::Handler.buildLocalRaw`).
- Hub provider enrichment keys by stable repository identity and item
  number; local numeric repository IDs never cross the federation wire
  (`internal/fleet/provider_enrichment.go::EnrichProviderState`).
- Spokes replace their aggregate entries with fresh local authority. Raw endpoints
  never re-export aggregates, which keeps federation data flow acyclic
  (`internal/server/fleetapi/fleet_hub.go::Handler.buildFleetSnapshot`).
- A spoke may use hub routes only after its config, enrollment, and
  database seal match and remote identity, protocol, and activation validate.
  An active spoke booted disabled validates its sealed binding and repairs both
  active credential grants before building dormant clients without startup
  egress; it publishes an initial disconnected provider state (`cmd/kenn-forge/spoke_startup.go::activateFederationSpokeAtStartup`,
  `internal/server/server.go::newServer`).
- Fleet consumes detached Workspace-owned summaries and runtime snapshots, not
  Workspace managers or root mutable config
  (`internal/server/workspaceapi/fleet_snapshot.go::FleetSnapshot`,
  `internal/server/fleetapi/handler.go::ConfigSnapshot`).
- Workspace overlays keep the local shell but omit links and metadata for
  removed source or associated items; inaccessible items remain visible
  (`internal/server/fleetapi/fleet_adapter.go::worktreeFromWorkspace`).

## Snapshot And Routing Contracts

- Snapshots use the shared federation protocol version, not a separate schema
  version; protocol version and node ID must match exactly
  (`internal/server/fleetapi/fleet_hub.go::Handler.fetchRawSnapshot`).
- Every projected host publishes `configKey == nodeID`, its topology role, and its canonical HTTPS origin. Self uses validated `fleet.base_url`; remote origins come from enrollment, never self-reporting
  (`internal/fleet/enrich.go::buildHost`, `internal/server/fleetapi/fleet_hub.go::Handler.fetchMemberRaw`).
- Enrolled identity is immutable until revocation: reload keeps the boot role, hub binding, and origin active, while setup and restart reject an origin mismatch
  (`internal/server/server.go::Server.activeFleetConfigSnapshotLocked`, `cmd/kenn-forge/spoke_startup.go::validateFederationHubOrigin`).
- A failed, incompatible, or unauthenticated member degrades only that member to
  an unreachable summary; it must not fail local or other member results
  (`internal/server/fleetapi/fleet_hub.go::Handler.fetchMemberRaw`).
- Structured peer rejections must retain an operator-actionable cause; do not
  collapse enrollment or identity failures to an HTTP status alone
  (`internal/server/fleetapi/fleet_hub.go::federationPeerResponseError`).
- A spoke supplies its aggregate member budget; the hub uses the smaller local
  or requested timeout, while the spoke reserves twice that budget for the full
  response (`internal/server/fleetapi/fleet_routes.go::Handler.getSnapshotAggregate`).
- Hubs show the fleet-wide workspace surface. Spokes retain the full host
  directory for navigation but project only their own actionable workspace data
  (`internal/fleet/enrich.go::ProjectForObserver`).
- Workspace lists consume inline projected summaries without per-host fan-out;
  remote actions require the owning host's projected operation availability.
  Explicitly incomplete aggregates retain absent-host rows; authoritative views
  retain only explicitly degraded hosts and remove absent membership
  (`frontend/src/lib/components/terminal/workspace-list-schema.ts::retainDegradedHostWorkspaces`).
- Hub workspace rows show a compact execution-host badge; spoke rows remain
  unlabeled because their actionable workspace surface is local-only. Fleet
  chrome remains reserved for actionable degradation (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte`).
- The reserved `self` alias and stable node ID address local. An active spoke routes
  only when config matches its enrollment; that enrollment supplies the credential-bound destination
  (`internal/server/fleetapi/fleet_proxy.go::Handler.resolveEnrolledSpoke`).
- Proxy operations preserve the owning host's status, problem envelope, and
  safe end-to-end headers; browser-facing responses must not carry peer cookies,
  redirects, authentication challenges, or connection-nominated headers
  (`internal/server/fleetapi/fleet_proxy.go::Handler.serveRemoteFleetRESTProxy`).

## Background Monitors

The local snapshot never does git or tmux I/O on the read path; three
Fleet-owned monitors keep the store and caches it reads fresh. They are
change-driven and idle-cheap:

- Every monitor pass is gated on demand: a live event-stream subscriber, or a
  snapshot read within the demand window. Hubs consume spokes through raw
  snapshot fetches without a spoke-local event stream, so the read itself is
  the demand signal. With none, a wake-up records a skip and re-checks on a
  short idle probe, so the first client after an idle stretch is served
  within seconds rather than a full interval; a nil subscriber source means
  always run (`internal/server/fleetapi/fleet_monitor_gate.go::runFleetMonitorLoop`,
  `internal/server/fleetapi/handler.go::Handler.noteSnapshotDemand`).
- Worktree discovery and worktree stats fingerprint the git directory (sizes
  and mtimes of `HEAD`, `index`, `config`, `packed-refs`, the `refs/` tree,
  and for discovery `worktrees/`) without spawning git, and re-run their git
  commands and database writes only when the fingerprint moved. The stats key
  also carries the default branch, so a discovery pass that moves it
  invalidates the sample. The fingerprint is a change hint, not proof: a
  matching fingerprint older than the max age is re-measured anyway, which
  bounds the staleness of anything it cannot see. A checkout whose fingerprint
  cannot be computed always takes the full path so it is marked stale; a
  failed pass drops the fingerprint so recovery is re-inspected
  (`internal/server/fleetapi/fleet_git_fingerprint.go::worktreeStatsFingerprint`,
  `internal/server/fleetapi/fleet_worktree_discovery.go::fleetWorktreeDiscoverer.refreshProjectIfChanged`,
  `internal/server/fleetapi/fleet_worktree_stats.go::fleetWorktreeStatsSampler.sampleTargets`).
- Unstaged edits and untracked files do not touch the git directory, so the
  background stats pass only sees them at the max-age resample. The on-demand
  refresh paths (`RefreshWorktreeStats`, `RefreshProjectInventory`, and the
  fleet-wide refresh route through `runOnceForced`) ignore the fingerprint and
  always measure; lifecycle mutations that change the working tree must keep
  calling them.
- Every discovery git spawn goes through `internal/procutil`; the stats probes
  already did.
- The tmux monitor runs one pass per interval: `list-sessions` always, then
  `list-windows` only when the session listing is not byte-identical to the
  previous pass or the window refresh bound elapsed, then `list-panes` plus
  process metrics only when managed sessions exist. Process metrics query the
  pane PIDs and their descendants (`ps -p`, `pgrep -P` per generation) rather
  than the whole host, descending until no children remain or the process
  table cap is hit. Exit status 1 from `ps` or `pgrep` is the no-match
  outcome; any other failure is a probe error, never an empty tree
  (`internal/server/fleetapi/fleet_tmux_monitor.go::probeFleetProcessTrees`).

## Transport Trust Boundary

- Supported fleets expose every Forge peer only through operator-controlled
  private ingress such as a tailnet. HTTPS and federation bearers still
  authenticate application peers; private reachability does not replace them.
- An activated hub and its spokes are one operator-controlled trust
  domain. A compromised active peer is equivalent to compromised fleet
  administration and is outside the federation isolation model. Route scopes
  enforce ownership and lifecycle boundaries; they are not a hostile-peer or
  multi-tenant sandbox (`internal/server/provider_route_policy.go::providerRouteDeclarations`).
- Pending spokes are not active peers. Pending hub bearers have only
  enrollment access; pending spoke bearers have preparation projections,
  handoff, and enrollment access. Generic provider reads require activation
  (`internal/server/api_auth.go::pendingProviderRouteAllowed`).
- A federation principal may revoke only its own enrollment; local daemon
  authentication remains the hub operator path for revoking any member
  (`internal/server/fleetapi/fleet_enrollment.go::Handler.revokeEnrollment`).
- Preparation access is operation-scoped, not repository-isolated: a one-time
  token approves eventual active membership, so transfer it only to a machine
  trusted with fleet provider data (`internal/server/api_auth.go::pendingProviderRouteAllowed`).
- A valid federation bearer takes precedence over optional ingress user
  identity so proxied peer requests retain their scoped spoke principal
  (`internal/server/api_auth.go::Server.authorizeAPIRequest`).
- Credentials require the current enrollment and configured binding.
  Activation upgrades both directions
  (`internal/federationauth/scope.go::HubToSpokeScopes`,
  `internal/server/api_auth.go::Server.federationPrincipalEnrollmentState`).
- Active peer bearers are accepted only after the versioned activation-lease
  handshake and while the hub and spoke copies of the renewable 24-hour lease
  are current and federation remains enabled. Lease-unaware or expired peers
  may reach only the identity preflight and activation handshake, or revocation;
  explicit hub rejection never falls back to a cached lease
  (`internal/server/api_auth.go::Server.federationPrincipalEnrollmentState`,
  `internal/server/api_auth.go::Server.allowsActivationLeaseHandshake`).
- The credential store persists inbound bearers only as SHA-256 digests and
  keeps outbound bearers readable for request construction. Atomic 0600 writes
  publish immutable in-memory snapshots only after persistence, so revocation
  takes effect on the next authentication attempt
  (`internal/federationauth/store.go::Store.Authenticate`,
  `internal/federationauth/store.go::Store.mutate`).
- Federation authorization is fail-closed: a valid bearer still receives
  a typed forbidden response when the method/path is not inventoried or its
  principal lacks the route's declared scope. Provider-owned operations join
  the closed inventory through their explicit read/write route rules
  (`internal/federationauth/authenticator.go::authorizedRoutes`,
  `internal/server/api_auth.go::Server.authorizeFederationRequest`).
- Provider calls require the exact federation protocol header after scope
  authorization; mismatch is explicit and never falls through to local state
  (`internal/server/api_auth.go::Server.authorizeFederationRequest`).
- Match authorization against the escaped request path; decoding `%2F` before
  scope lookup splits nested provider owners into extra route segments
  (`internal/server/api_auth.go::Server.canonicalAPIPath`).
- Enrollment reserves a digest-only inbound credential before the remote
  subject is known; it cannot authenticate until atomically bound to that
  subject
  (`internal/federationauth/store.go::Store.ReserveInbound`,
  `internal/federationauth/store.go::Store.BindInbound`).
- Issuing and transferring an enrollment token approves eventual active
  membership; the random preparation seal proves retry-safe handoff, not a
  second approval (`internal/server/fleetapi/fleet_enrollment.go::Handler.sealSpokePreparation`).
- Member configuration accepts only canonical HTTPS origins. REST and WebSocket
  requests strip browser credentials and browser/proxy provenance, resolve the
  destination member first, and then add only that member's outbound bearer.
  Per-origin clients refuse redirects and bound connection setup. Ordinary
  reads remain time-bounded; long REST proxies use request cancellation because
  clone and diff-watch handlers can legitimately exceed snapshot deadlines
  (`internal/server/fleetapi/fleet_enrollment.go::hardenedFederationProxyHTTPClient`).
- REST proxies buffer and bound any request body before selecting or dialing a
  member; raw Huma adapter handlers do not enforce operation body metadata
  (`internal/server/fleetapi/fleet_proxy.go::bufferFleetProxyRequestBody`).
- WebSocket attach tracing ends after bounded connection setup and before the
  long-lived bridge (`internal/server/fleetapi/fleet_proxy.go::startFleetAttachSpan`).
- Remote terminals are WebSocket-to-WebSocket bridges. The hub does not
  allocate a proxy PTY or rewrite the owning spoke's local session backend
  (`internal/server/fleetapi/fleet_proxy.go::Handler.serveFleetWebSocketProxy`).
- Provider Git traffic is a separate transport boundary. Federated clone URLs
  use HTTPS or SSH; HTTP is accepted only on loopback because Forge cannot prove
  an arbitrary remote route is tunnel-encrypted (`internal/providerplane/client.go::ValidateFederationWorkspaceLaunchSpecResponse`).
- Activation gives the hub workspace mutation and terminal attachment
  authority on a spoke; pending hubs have enrollment access only
  (`internal/federationauth/scope.go::HubToSpokeScopes`).
- Hub-supplied durable Git facts require spoke-side federation validation,
  including during preparation before the spoke role is saved
  (`internal/server/spoke_preparation.go::Server.refreshSpokePreparationLaunchSpecs`).

## Provider Facts For Local Execution

- The hub is the only authority for provider-backed workspace launch
  facts. A spoke resolves PR and issue launch specifications through the
  authenticated provider plane; it does not require local repository, pull, or
  issue replicas (`internal/server/provider_sources.go::hubProviderSource.ResolveWorkspaceLaunchSpec`).
- Manual provider refresh, automatic assignment, and merge-request worktree
  facts cross the same provider plane. The hub performs provider API
  work; the spoke persists validated launch facts and performs only spoke-local
  Git and workspace work
  (`internal/server/provider_state_handoff.go::Server.federationRefreshWorkspaceLaunchSpec`,
  `internal/server/federation_provider_workspace.go::Server.federationAutoAssignWorkspaceItem`,
  `internal/server/provider_sources.go::hubProviderSource.ResolveMergeRequestWorktreeFacts`).
- Clone-backed GitLab merge-request reads fetch only that merge request's
  provider-owned head ref before reading its descriptor SHA; the managed clone
  has no wildcard MR refspec (`internal/gitclone/clone.go::Manager.FetchMergeRequestHead`).
- Hub activity responses exclude its local workspace overlays and carry
  the hub-owned workspace-recency policy. Spokes apply that policy while
  rebuilding local activity and authors; provider markdown images remain
  hub-owned (`internal/server/huma_routes.go::Server.overlayLocalActivityWorkspaces`,
  `internal/server/provider_route_policy.go::providerRouteDeclarations`).
- A resolved specification is bound to the exact request and carries stable
  repository identity, base clone/default-branch facts, PR head-repository
  semantics, and source visibility. Stable identity permits owner/name refresh
  only within the requested provider and host. Before admitting the workspace,
  a spoke validates that binding and hosted clone URL, requires an exact Git
  credential route, rejects a historically occupied mutable route, and records
  only repository routing metadata in its spoke catalog (`internal/providerplane/client.go::ValidateFederationWorkspaceLaunchSpecResponse`,
  `internal/server/provider_sources.go::hubProviderSource.ResolveWorkspaceLaunchSpec`).
- Federated ad-hoc and MCP workspace creation resolve a hub repository descriptor
  before local lookup or route fencing, so newly discovered repositories do not
  require a settings refresh (`internal/server/provider_sources.go::hubProviderSource.ResolveRepositoryRoute`).
- Registered spoke projects resolve stable hub repository identity during
  preparation and future registration; raw fleet snapshots remain read-only
  (`internal/server/spoke_preparation.go::Server.reconcileSpokePreparationProjects`).
- Base and fork clone URLs must resolve to canonical provider repository routes;
  network URL syntax alone is not an authorization boundary
  (`internal/providerplane/client.go::federationRemoteRepositoryRoute`).
- The workspace row and launch specification commit atomically before
  asynchronous setup starts. Setup, retry and recovery, branch synchronization,
  PR monitoring, pushed-head observation, and generated agent context consume
  that persisted specification rather than spoke provider tables. Starting a
  local agent uses the last validated specification without renewing its
  provider-visibility lease
  (`internal/db/queries_workspace_launch_specs.go::DB.CreateWorkspaceWithLaunchSpec`,
  `internal/workspace/launch_spec.go::Manager.lifecycleSummary`).
- Launch-spec refresh follows stable provider identity across renames and
  commits the verified route with the specification; reused routes stay fenced
  (`internal/db/queries_workspace_launch_specs.go::DB.PutRefreshedWorkspaceLaunchSpec`).
- Source visibility is a strict 15-minute hub lease for setup and
  provider-backed Git work. Once it expires, those operations must refresh it;
  hub failure returns a typed retryable error and an explicitly hidden
  source remains unavailable. Existing local runtime sessions and new agent
  sessions do not renew the lease. Ad-hoc and Kata workspaces stay spoke-local
  (`internal/workspace/launch_spec.go::LaunchSpecRefreshError`).

## Provider Event Flow And Availability

- `fleet.enabled` is live. Disabling it stops hub-backed provider work
  and cancels the inbound event stream; enabling it opens a fresh stream whose
  replay barrier performs authoritative refresh
  (`internal/server/federation_events.go::hubEventLifecycle`).
- Spokes open one authenticated `events.read` SSE stream to the hub.
  The exact federation protocol header is required, redirects are refused, and
  the hub emits only its provider-owned event vocabulary
  (`internal/server/federation_events.go::Server.streamFederationEvents`,
  `internal/providerplane/events.go::EventClient`).
- Hub event IDs are private to that inbound stream. Each spoke
  re-stamps accepted events with its own `EventHub.Broadcast`, so browsers keep
  one spoke-local checkpoint and hub restart cannot move that checkpoint
  backward or forward (`internal/server/federation_events.go::Server.receiveHubEvent`).
- Stream open, replay staleness, and poison-frame recovery refresh provider
  projections and `sync_status` before the spoke announces the hub as
  connected. Initial connection does not announce an outage before its first
  attempt; a real connection failure does. While disconnected, the frontend
  retains already loaded provider projections behind an explicit stale-data
  banner and disables Sync and detail Refresh. A fresh browser load has no
  durable provider cache. The frontend refreshes its selected projection before
  removing the banner; an independent successful sync-status probe may restore
  hub availability while a projection-specific refresh error remains visible
  (`internal/server/federation_events.go::Server.resynchronizeHubProviderState`,
  `frontend/src/lib/app-stores.svelte.ts::createAppStores`).
- An initial pull-detail read that finds a tracked repository but no cached pull
  performs one synchronous item sync through the hub. This repairs a
  cold or incomplete hub projection without launching another full
  repository sync. A missing repository remains an operator action and directs
  the user to repository settings (`frontend/src/lib/stores/detail.svelte.ts::createDetailStore`).
- Spokes have no automatic provider failover. Provider credentials present on a
  spoke are Git routes only; the spoke role does not construct a provider control
  plane or an independent syncer. Hub loss therefore pauses provider
  reads, refreshes, and writes while local workspace execution continues
  (`cmd/kenn-forge/provider_startup.go::buildServeControlPlanes`).
- The inbound stream worker starts after Workspace and participates in the same
  dependent wait group as other Workspace consumers. Hub shutdown
  closes its event hub before draining HTTP so long-lived federation streams
  exit cleanly (`internal/server/server.go::Server.runWorkspaceDependent`,
  `internal/server/server.go::Server.Shutdown`).

## Configuration And Lifecycle

- Federation protocol version 4 requires an exact match; there is no
  translation or compatibility fallback
  (`internal/federation/protocol.go::ProtocolVersion`).
- Activation leases negotiate their own version 1 inside protocol 4. This
  keeps snapshot compatibility separate from the authorization handshake:
  upgraded hubs isolate lease-unaware active spokes until they upgrade and
  activate again, without rewriting enrollment storage or requiring
  re-enrollment (`internal/federation/protocol.go::ActivationLeaseVersion`,
  `internal/server/fleetapi/fleet_enrollment.go::Handler.activateEnrollment`).
- Enrollment accepts only canonical HTTPS origins. Each token is consumed by
  its first accepted request; retry or rekey requires a new token
  (`internal/federation/enrollment_store.go::Store.Begin`).
- A join with matching hub and spoke origins reuses its unbound provisional ID,
  so a new token recovers an ambiguously accepted request. Mismatched
  provisional records are replaced; bound or preparation-started enrollments
  are not, and a hub must revoke or migrate its members before becoming a spoke
  (`internal/server/fleetapi/fleet_enrollment.go::Handler.joinFederation`).
- Enrollment secrets are printed once and enter `fleet join` only through a
  hidden prompt, stdin, or `--token-file`. The sole pre-auth route is the exact
  enrollment POST (`cmd/kenn-forge/fleet_cli.go`,
  `internal/server/api_auth.go::Server.isPreEnrollmentRequest`).
- The token deadline governs only starting enrollment. Both peers enforce it for
  unstarted pending principals; preparation pins the enrollment until activation,
  abort, or revocation. Joining never changes `fleet.role` or restarts Forge
  (`internal/federation/enrollment_store.go::Store.CleanupExpired`,
  `internal/server/fleetapi/fleet_enrollment.go::Handler.activateEnrollment`).
- `fleet prepare-spoke` is resumable and talks only to the authenticated local
  daemon. Before sealing local writes it first proves the pending hub is
  reachable and pins that enrollment; hub failure before that point
  leaves the standalone provider plane open.
- A daemon without a local federation enrollment starts with provider writes
  open and does not restore fleet preparation state
  (`internal/server/server.go::newServer`).
- Once quiescing begins, the provider-write barrier survives restarts. Only
  `fleet abort-preparation` may reopen it before activation, after admitted work
  drains. A spoke-shaped process requires restart before standalone provider work;
  forced abort still reports the enrollment to revoke when its hub is unavailable
  and retains a revocation-only local tombstone so that hub cleanup remains possible
  (`internal/server/spoke_preparation.go::Server.abortFederationSpokePreparation`).
- Preparation waits for admitted provider writes and deferred merges to drain,
  then freezes notification acknowledgement admission before reading any state
  for handoff. It persists refreshed launch specifications only after the spoke's
  exact Git credential route resolves, then hands off review drafts and
  user-authored workflow rows; automatically seeded defaults carry no state and
  stay local
  (`internal/db/queries_provider_state_handoff.go::DB.ListProviderStateForHandoff`).
  Imports are content-addressed:
  absent state imports, identical state returns the same receipt, and different
  state reports both digests without overwriting either side
  (`internal/server/spoke_preparation.go::Server.prepareFederationSpoke`).
- The hub issues an opaque, retry-safe preparation seal bound to the
  enrollment, both node IDs, protocol and migration versions, handoff receipt
  digest, and drained acknowledgement generation. A changed binding conflicts.
  The spoke records it in preparation state and the 0600 enrollment store.
  The daemon then saves spoke role under its config mutation locks for that
  binding; it reports `restart_required` and never restarts itself
  (`internal/server/spoke_preparation.go::Server.persistPreparedSpokeRole`).
- Activation retries are bounded and reuse the sealed enrollment; the
  hub validates its issued seal, and already-active retries never
  duplicate membership. Renewal is conditional on the enrollment remaining
  active, so a response that races revocation cannot restore the lease.
  Retryable failures back off for one minute; definitive rejection clears the
  lease and stops renewal until restart. Invalid state is `action_required`,
  protocol mismatch is `incompatible`, and both preserve local execution while suppressing routes
  (`cmd/kenn-forge/spoke_startup.go::activateFederationSpokeAtStartup`,
  `internal/server/fleetapi/fleet_enrollment.go::Handler.activateEnrollment`).
- Hub-initiated revocation makes each side retry-safe. The spoke marks its local
  enrollment revoked, clears its preparation binding, removes its outbound
  credential, and retains the hub bearer only as an exact-revoke tombstone.
  After acknowledgement, the hub records revocation before fallible credential,
  membership, and stream cleanup, so retries cannot strand a half-revoked relationship
  (`internal/server/fleetapi/fleet_enrollment.go::Handler.revokeEnrollment`).
- Ordinary fleet settings update only operator preferences; enrollment owns
  role, hub binding, and membership, so a stale browser save cannot
  replace them (`internal/server/settings_routes.go::Server.updateFleetSettings`).
- Enrollment-managed membership and ordinary fleet settings can reload live.
  Role, local and hub origins, API authentication, and
  session-monitor policy report `restart_required` until the running process
  matches persisted config
  (`internal/server/config_reload.go::startupConfigSnapshot.restartRequiredFor`).
- A daemon that booted with fleet disabled cannot activate federation by
  reload unless API authentication was already active. Reload persists the
  intent, but federation waits for restart when both are enabled together
  (`internal/server/server.go::Server.activeFleetConfigSnapshotLocked`).
- Fleet workers start after Workspace and shut down before Workspace. Detailed
  shutdown rules live in
  [`workspace-runtime-lifecycle.md`](./workspace-runtime-lifecycle.md).
