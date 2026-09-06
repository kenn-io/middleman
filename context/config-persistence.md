# Config Persistence Invariants

Use this document when adding or changing config fields that kenn-forge saves
back to TOML.

- `configFile` in `internal/config/config.go` is the hand-maintained subset of `Config` that `Save` writes to disk. A `Config` field absent from `configFile` (or from the `Save` initializer) loads from TOML fine but is silently dropped on the next save or restart.
- Every new persisted config field or section must be wired in three places — `Config`, `configFile`, and the `Save` initializer — and covered by a save/load round-trip test with a non-default value (see `TestPullRequestsConfigRoundTrip` in `internal/config/config_test.go`).
- Omitted `fleet.role` means `hub`; an explicit `spoke` role and its
  hub binding must survive whole-file saves together
  (`internal/config/config.go::Fleet.RoleOrDefault`, `internal/config/config.go::Config.Save`).
- `fleet.members` is hub-only active metadata keyed by stable node ID
  and canonical HTTPS origin. Enrollment does not change the startup-bound role;
  `fleet prepare-spoke` saves spoke role under the daemon's config mutation locks
  only after matching the durable seal; abort may reset an unactivated spoke
  (`internal/config/config.go::Fleet.Validate`,
  `internal/server/spoke_preparation.go::Server.persistPreparedSpokeRole`,
  `internal/server/settings_routes.go::Server.resetPreparedSpokeBinding`).
- Ordinary fleet mutations persist against the boot-pinned role, origin, and hub
  binding. Preparation keeps the boot role and origin but carries forward the
  enrollment-owned hub binding written by join; reload drift cannot redefine either
  path (`internal/server/settings_routes.go::Server.mutatePersistedFleetChecked`,
  `internal/server/settings_routes.go::Server.mutatePersistedEnrollmentFleetChecked`).
- Unsupported fleet fields fail config loading through the undecoded-key gate.
  Do not add migration parsing or derive spoke identity from removed display
  metadata; operators must remove unsupported fields and enroll spokes explicitly
  (`internal/config/config.go::rejectUnsupportedConfigKeys`).
- Enrollment records, opaque preparation seals, and federation bearers live in
  0600 atomic JSON stores under `data_dir`, not TOML; settings and CLI output
  must never expose bearers or seals
  (`internal/federation/enrollment_store.go::DefaultStorePath`,
  `internal/federationauth/store.go::DefaultStorePath`).
- A spoke's settings response composes hub-owned provider policy with
  spoke-owned execution policy. Each spoke settings write must target exactly one
  owner; mixed-owner requests fail before either side changes. Spoke-local writes
  fetch their hub projection before saving, so a failed response never
  hides a committed local mutation (`internal/server/settings_handlers.go::Server.updateSettings`,
  `internal/server/settings_handlers.go::Server.updateLocalSettings`).
- Fleet preference writes never accept role, hub, or member state;
  those fields change only through enrollment and preparation workflows
  (`internal/server/settings_routes.go::Server.updateFleetSettings`).
- `fleet setup` reloads under the shared config lifecycle lock and never moves
  `data_dir` or writes enrollment-owned spoke role/binding state. Failed setup
  restores the previous config before restarting the previous service
  (`internal/fleetsetup/setup.go::Runner.Apply`).
- `activity.use_workspace_activity_for_recency` is one global PR, Issue, and Activity opt-in. Its zero value is intentionally false; successful settings writes and file reloads publish the committed value to handler snapshots (`internal/server/server.go::pullConfigSnapshot`).
- `detail.initial_timeline_entry_limit` is a global PR/issue presentation preference.
  Omitted or zero values default to 50; explicit values must remain within 10-250
  in both config loading and settings writes (`internal/config/config.go::Detail`).
- `detail.collapse_single_line_breaks` is a false-by-default PR/issue presentation opt-in
  that renders markdown descriptions, comments, and markdown-rendered commit bodies with
  CommonMark soft breaks. It never changes plain text, Docs mode, or editor previews, and Detail settings writes replace the
  whole section, so the UI must send every Detail field
  (`internal/config/config.go::Detail`, `frontend/src/lib/utils/markdown.ts::getMarked`).
- `detail.render_commit_messages_as_markdown` is a false-by-default opt-in that routes
  timeline commit bodies through the comment markdown pipeline instead of plain text
  (`internal/config/config.go::Detail`).
- When zero is meaningful, represent the saved value as optional so TOML `omitempty` cannot turn explicit zero into an unset default; the round-trip test must cover zero (`internal/config/config.go::Terminal`).
- Whole-file settings mutations must hold `configReloadMu` before `cfgMu` while applying and saving changes, or the watcher can restore a stale snapshot between writes (`internal/server/settings_handlers.go::updateSettings`).
- Partial settings request objects must use pointer fields and merge only fields that were present; reusing persisted value structs collapses omission into zero values (`internal/server/settings_handlers.go::mcpSettingsUpdate`).
- `modes.actions` is false by default and controls presentation only; provider
  workflow HTTP access remains available (`internal/config/config.go::ModeVisibility`).
- Disabling Actions releases demand and removes both its top-level mode and PR
  workflow menu (`frontend/src/App.svelte::syncWorkflowActionsAvailability`).
- `roborev.init_managed_clones` is a hot-reloaded, false-by-default setup policy. It persists through the partial `roborev` settings object and the committed workspace API snapshot; only the effective Roborev endpoint remains in the startup-bound restart snapshot (`internal/config/config.go::Roborev`, `internal/server/config_reload.go::startupConfigSnapshot`, `internal/server/workspaceapi/config.go::ConfigSnapshot`).
- Repository preset config stores only named custom definitions; `Global` is a derived UI preset and must never be serialized to TOML. Each member persists provider, provider host, provider-verified repository ID, and a last-known display route; preset create/update/delete use dedicated atomic settings endpoints instead of replacing the collection through generic settings (`internal/config/config.go::RepoPreset`, `internal/server/settings_handlers.go::mutateRepoPresets`).
