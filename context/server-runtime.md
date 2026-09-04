# Server Runtime

Use this document for daemon startup, discovery, request-origin validation,
and the root event stream.

## Startup Contracts

- Bare `kenn-forge` is help-only, `serve` is foreground, and background
  lifecycle management is under `daemon start|status|stop|restart`
  (`cmd/kenn-forge/cli.go::newRootCommand`).
- `daemon start` is idempotent: reuse requires verified identity for the same
  resolved `data_dir`; incompatible versions require `daemon restart`
  (`internal/daemonruntime/lifecycle.go::NewManager`).
- Lifecycle startup mints the API token under the authoritative data-directory
  lock; atomic serialized publication makes concurrent paths retain one
  credential (`internal/runtimelock/token.go::EnsureAuthToken`).
- Each data directory owns one random 128-bit node ID. Startup serializes its
  first write, rejects malformed existing identity, and validates it before a
  daemon replacement can stop the old process (`internal/runtimelock/node_id.go::EnsureNodeID`,
  `cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.prepareConfigMutation`).
- Lifecycle commands retain caller-spelled paths for initial default migration,
  then freeze canonical identity for locks, reloads, runtime records, and
  comparisons (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.lockMutationConfig`).
- Default-home upgrades copy the legacy config and its referenced credential
  files before marking completion and relocating the database; explicit config
  paths and `KENN_FORGE_HOME` never relocate config
  (`internal/config/legacy_migration.go::migrateLegacyConfig`).
- Sync-enabled startup keeps the local UI available when provider credentials or
  identity initialization is unavailable. Failures attributable to one provider
  host drop only that host while healthy hosts keep syncing; unattributable
  failures degrade to no provider sync. Dropped hosts serve cached data until a
  later restart (`cmd/kenn-forge/provider_startup.go::buildProviderControlPlaneOrDegraded`).
- A fully sealed active spoke never requires hub reachability at process start;
  its event lifecycle owns reconnection after transient network or DNS outages
  (`cmd/kenn-forge/spoke_startup.go::activateFederationSpokeAtStartup`).
- Provider API origins and cleartext acknowledgements are startup-bound. Config
  reload reports `restart_required` when either changes instead of claiming the
  boot-time client and clone policy were updated
  (`internal/server/config_reload.go::startupPlatformTransports`).
- Every role gets lazy Git routing; only hubs construct provider API,
  accounting, sync, archive, notification, and deferred-work machinery.
  `--disable-sync` suppresses hub refresh and never weakens spoke absence
  (`cmd/kenn-forge/provider_startup.go::buildServeControlPlanes`,
  `cmd/kenn-forge/main.go::run`).

## Startup Lock

- Config identity must converge across relative, symlinked, and filesystem-equivalent
  name aliases before hashing or comparison
  (`internal/daemonruntime/runtime.go::CanonicalConfigPath`).
- Data-directory identity must converge across symlinked and filesystem-equivalent
  name aliases before hashing, comparison, or runtime publication
  (`internal/config/data_dir.go::CanonicalDataDir`).
- Linux without procfs retains the already symlink-resolved spelling instead of
  making all config-backed commands fail
  (`internal/pathidentity/canonical_linux.go::CanonicalExisting`).
- Background lifecycle mutations lock canonical config identity before sorted
  resolved `data_dir` identities; release but never unlink the stable
  cross-process paths (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.prepareConfigMutation`).
- Mutations reload config after locking and retry if `data_dir` changed;
  detached children reject a config that differs from the locked identity
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.prepareConfigMutation`, `cmd/kenn-forge/start_background.go::validateBackgroundLaunchConfig`).
- Start, stop, and restart use canonical config identity to lock both prior and
  current `data_dir` before reusing, replacing, or stopping a moved daemon
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.prepareConfigMutation`).
- Lifecycle mutations proof-authenticate config-attributed candidates individually;
  multiple records require one authoritative lock-metadata match before shadows
  are removed (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.runtimeForConfig`).
- An unauthenticated record is removable only while its authoritative lock is
  free or its former `data_dir` no longer exists
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.readCleanupRuntimeStatus`).
- Config-identified records take precedence over legacy records; a live legacy
  record remains attributable only while its `data_dir` matches current config
  (`internal/daemonruntime/runtime.go::ConfigRuntimes`).
- Authenticate moved legacy candidates before rejection; discard an unauthenticated
  stale record only when its authoritative data-directory lock is free
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.runtimeForConfig`).
- An authenticated pre-config-identity daemon matches authoritative status only
  when both discovery and status omit `config_path`; current-directory attribution
  above remains the boundary (`cmd/kenn-forge/daemon_lifecycle.go::runtimeStatusMatches`).
- Stop and restart prove the exact candidate record before signaling; moved
  candidates must also match authoritative config path and PID, while version
  differences remain stoppable
  (`internal/daemonruntime/lifecycle.go::FindVerifiedRecord`).
- Stop prefers authenticated config-identified runtime state under lifecycle locks;
  malformed TOML is consulted only when no attributable daemon exists
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.loadMutationConfigOrRuntime`).
- Daemon stop waits through the sum of every bounded shutdown phase plus process
  exit margin (`internal/shutdownbudget/budget.go::Total`).
- Unix sends one SIGTERM before requiring manual recovery; force-kill escalation
  cannot rule out PID reuse
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.stopLocked`).
- `kenn-forge.lock` is a stable lock target, not a disposable liveness sentinel.
  Never delete it: unlinking a held file lets another daemon lock a different
  inode and use the same data directory concurrently
  (`internal/runtimelock/lock.go::Acquire`).
- Release the OS lock while leaving the file in place. A file's existence says
  nothing about daemon liveness; only lock acquisition does
  (`internal/runtimelock/lock.go::Handle.Release`).

## Discovery And Readiness

- Every ready server publishes the standard `daemon.<pid>.json` record in
  config home, even when `config.toml` changes `data_dir`; the record is a
  discovery surface and does not replace the authoritative data-directory
  lock/status (`internal/daemonruntime/runtime.go::Publish`).
- Publish a runtime record only after the startup handler is installed and the
  HTTP server enters its listener accept loop; lifecycle discovery still proves
  the published loopback identity exactly before reuse (`cmd/kenn-forge/main.go::serveReadyListener.Accept`).
- Early identity proof establishes the startup owner, not application readiness;
  background lifecycle success requires `/healthz` and then re-proves the exact
  runtime record (`cmd/kenn-forge/start_background.go::waitForBackgroundReadiness`).
- Status uses authenticated config identity before TOML, including for moved runtimes;
  invalid config cannot strand an attributable daemon, while proof-unavailable state
  may report only the configured `data_dir` lock
  (`cmd/kenn-forge/daemon_lifecycle.go::daemonLifecycle.Status`).
- Record metadata is string-valued `node_id`, `host`, `port`, `read_only=false`,
  `require_auth`, `data_dir`, canonical `config_path`, and canonical
  `base_path`; `auth_token_path` is present only when auth is enabled, and
  `mcp_listen_addr` only when MCP is enabled. One typed owner builds and
  validates it;
  discovery still requires a live PID and a token-derived proof bound to the
  record's service, version, PID, network, and address
  (`internal/daemonruntime/runtime.go::Compatible`).
- The ready handler exposes standard identity at `GET /api/ping`; its private
  proof path binds the same identity to the published record and only accepts
  direct loopback requests
  (`internal/server/daemon_access.go::daemonRequestPolicy.admit`).
- Spoke activation is part of startup readiness for federation, not for local
  execution. A sealed spoke that cannot activate still serves local workspaces,
  logs `action_required` or `incompatible` with a reason, and supplies that
  reason to its local-only fleet projection. It must never construct a local
  provider control plane as a fallback (`cmd/kenn-forge/spoke_startup.go`,
  `cmd/kenn-forge/main.go`).

## Transport Trust Boundary

- Mutation identity must not depend on DELETE request bodies: the native desktop
  transport drops them before requests reach the daemon. Use path or query identity
  instead (`internal/server/huma_routes.go::unsetStarred`).
- Loopback TCP is the required cross-platform transport; Unix sockets and
  named pipes are not lifecycle requirements. Background startup rejects
  non-loopback listeners before launching
  (`cmd/kenn-forge/start_background.go::validateBackgroundConfig`).
- `[api.tailscale_serve]` requires API authentication and one allowlisted identity
  header; browser WebSocket origins must use HTTPS and match the requested Forge
  authority.
  The startup-bound mode trusts local processes and suits only trusted hosts;
  policy edits require restart and never change federation credentials
  (`internal/server/api_auth.go::tailscaleWebSocketOriginAllowed`,
  `internal/config/config.go::Config.validate`,
  `internal/server/daemon_access.go::daemonRequestPolicy.acceptsTailscaleServeUser`).
- `fleet setup` keeps Forge on loopback with API auth; `--tailscale` owns one
  Serve mapping, while `--origin` leaves ingress and browser auth to the operator
  (`internal/fleetsetup/setup.go::configureCandidate`).
- Setup services run as the selected current user and have no network-vendor or
  shell dependency (`internal/fleetsetup/service.go`).
- The daemon bearer and browser cookie remain full local-user credentials.
  Federation bearers authenticate through a separate branch, attach an exact
  node-ID principal to the request, and can dispatch only method/path pairs in
  the closed peer-route inventory
  (`internal/server/api_auth.go::Server.authorizeFederationRequest`,
  `internal/federationauth/authenticator.go::Authenticator.RequiredScope`).
- An optional `X-Kenn-Forge-Node-ID` header is diagnostic only: the bearer
  establishes identity, and a supplied header that differs from its subject is
  rejected (`internal/server/api_auth.go::Server.authorizeFederationRequest`).
- Only the startup-bound bearer with exact loopback authority/peer and no
  forwarding headers bypasses proxy Host interpretation; cookies never qualify,
  and the bearer remains available when general API auth is off
  (`internal/server/daemon_access.go::daemonRequestPolicy.admit`).
- Discovery sends only a random challenge until the endpoint proves the daemon
  token and full runtime identity; the proof route requires the exact direct
  loopback authority without forwarding headers
  (`internal/daemonruntime/lifecycle.go::discovery.probe`).

## Host And Origin Boundary

- Host validation is the DNS-rebinding boundary and runs before API auth,
  cross-origin protection,
  and route handling. Do not move it behind middleware that trusts request
  credentials first (`internal/server/server.go::Server.ServeHTTP`).
- Mutation CSRF protection uses `http.CrossOriginProtection`; requests without browser
  origin metadata remain available to native and generated API clients, while Huma
  operations enforce their own body media types (`internal/server/server.go::checkCrossOrigin`).
- Trusted forwarded-host support adds validation of the canonical forwarded
  authority; it never replaces validation of the raw backend `Host`
  (`internal/server/host_check.go::checkHost`).
- After trusted forwarded-host validation, mutation origin checks compare with
  that public authority rather than the reverse proxy's backend `Host`
  (`internal/server/server.go::checkCrossOrigin`).

## Event Replay

- Workflow dispatch follow-through is server-owned background work: after a
  provider accepts a dispatch, a tracked goroutine locates the run and watches it
  to completion, publishing `workflow_dispatch_progress` events keyed by the
  response's `dispatch_id`. Clients never poll workflow runs
  (`internal/server/workflowapi/dispatch_follow.go::Handler.followDispatch`).
- SSE event IDs are process-scoped replay cursors, not durable sequence
  numbers. Reconnects may replay only IDs retained by the current process's
  ring (`internal/server/event_hub.go::EventHub.ReplaySnapshotSince`).
- A cursor older than the ring or ahead of the current process head emits
  `reconnect.stale`; the client must discard incremental assumptions and perform
  an authoritative refetch (`internal/server/server.go::Server.handleSSE`).
- The frontend checkpoint advances only after an event's Effect consequences succeed;
  overlapping owners must keep it monotonic, and buffer pressure reconnects from the
  last accepted ID (`frontend/src/lib/stores/provider-events-workflow.ts::providerEventsProgram`).
- The hub's authenticated `/api/v1/federation/events` stream reuses the
  same replay ring but emits only provider-owned event types. Workspace,
  runtime, Docs, Kata, config, and spoke-connection events never cross that
  boundary (`internal/server/federation_events.go::Server.streamFederationEvents`).
- A spoke keeps the hub cursor private and republishes decoded provider
  events through ordinary local `EventHub.Broadcast`. Remote IDs never advance
  the browser's local replay floor; stale or undecodable remote state triggers
  an authoritative provider refresh instead
  (`internal/providerplane/events.go::EventClient`,
  `internal/server/federation_events.go::Server.receiveHubEvent`).
- A federation-only SSE comment marks the replay/live boundary. Spokes remain
  provider-unavailable while replay drains, then reconcile authoritative state
  before announcing the hub connection; replayed status cannot
  overwrite that recovery snapshot
  (`internal/server/federation_events.go::writeFederationReplayComplete`).
- `sync_status`, `config.changed`, and `hub_connection_changed` are
  latest-value events cached in local ID order for fresh browser subscribers
  (`internal/server/event_hub.go::EventHub.enqueueCachedLocked`).

## Long-Lived Transport Inventory

- Long-lived HTTP and WebSocket contracts derive from the Huma registrations;
  catch-all proxies declare finite streaming variants on their operation, and
  tracing consumes the same inventory (`internal/server/transport_inventory.go::NewTransportInventory`).
- Federation SSE clears the whole-request client timeout but retains bounded
  dialing, TLS negotiation, response headers, header bytes, origin pinning, and
  redirect refusal. Its request context owns the response lifetime
  (`internal/providerplane/client.go::hardenedStreamingClient`).
- An annotated proxy stream is served only when the request explicitly accepts
  its declared media type (`internal/server/httpapi/transport.go::ValidateTransportAccept`).
