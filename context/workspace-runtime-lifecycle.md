# Workspace Runtime Lifecycle

Use this document for changes in workspace delete flows, runtime session
management, tmux persistence, and workspace terminal UI behavior.

## Purpose

- Keep the lifecycle of kenn-forge-managed runtime state explicit.
- Preserve the distinction between the durable workspace, the base tmux
  terminal, and launched runtime sessions.
- Prevent review regressions around destructive ordering, stale tmux rows, and
  UI/runtime disagreement after exits.

## Runtime Model

kenn-forge manages three related but different things:

- The persisted workspace record and worktree.
- The base workspace `tmux` terminal, which is durable and reconnectable.
- Launched runtime sessions and the shell drawer. When tmux is available they
  are tmux-backed, recorded, and reconnectable across kenn-forge restarts; when
  tmux is unavailable they use ptyowner.

Rules:

- The base workspace tmux session remains durable backend state, but production
  workspace terminal panes are pooled runtime sessions; do not treat the primary
  terminal endpoint as a mounted SPA workflow (`frontend/src/lib/components/terminal/PooledSessionTerminal.svelte`).
- Launched agent sessions and shell sessions are not durable after natural exit.
- The shell drawer is a singleton per workspace, but a tmux-backed shell should
  survive kenn-forge server restarts until the shell exits or the workspace is
  deleted.
- Coding-agent session IDs are hook-authoritative and live-only: expose only fresh,
  supported reports joined by canonical worktree and runtime key to a live `agent`
  runtime (`internal/server/workspaceapi/agent_sessions.go::Handler.listWorkspaceAgentSessions`).
- Hook reports own workspace activity where they exist: a workspace with a
  hook report for a live agent runtime is never probed through tmux pane
  capture, its last-activity time is the report's timestamp, and the tmux
  probe returns only when the session ends or its runtime is removed. Reports
  have no time-based expiry; a launched agent keeps reporting until teardown
  (`internal/server/workspaceapi/routes_handlers.go::Handler.applyWorkspaceTmuxEnrichment`,
  `internal/agentactivity/store.go::Store.LiveReportsForWorkspace`).
- A completion keeps its first timestamp: `done` written over `done` for the
  same session preserves `UpdatedAt`, so the sidebar's acknowledged Done badge
  does not reappear when Claude Code's `idle_prompt` follows Stop
  (`internal/agentactivity/store.go::Store.HandleEvent`).
- Claude Code's `idle_prompt` notification fires after a minute of waiting
  whatever the agent waits for. It leaves a pending `input` or `approval`
  state untouched and otherwise maps to `done`; only `elicitation_dialog` and
  user-input tools put a session into `input`
  (`internal/agentactivity/store.go::Store.HandleEvent`).
- Reports are reconciled against persisted and live runtime session keys
  after startup restoration and after every missing-tmux prune, so a report
  whose runtime row was pruned does not outlive it
  (`internal/server/workspaceapi/lifecycle.go::Handler.reconcileAgentActivityReports`).

## Provider-Backed Lifecycle Facts

Provider-backed workspace execution is local, but its provider facts remain
hub-owned.

- Creation commits the workspace and its versioned launch specification in one
  transaction before starting asynchronous setup. The specification preserves
  the exact base clone, default branch, requested workspace branch, PR head
  kind, and fork clone URL selected at admission
  (`internal/db/queries_workspace_launch_specs.go::DB.CreateWorkspaceWithLaunchSpec`).
- Initial setup, retry, crash recovery, pull/push branch synchronization, PR
  association, and pushed-head monitoring pass through
  `internal/workspace/launch_spec.go::Manager.RequireWorkspaceLaunchSpec`.
  These paths must not reconstruct provider facts from repository, pull, or
  issue cache rows on a spoke.
- A source-visibility lease is valid only while `now < source_visible_until`.
  There is no grace period. An unexpired specification permits the lifecycle
  operation after local route and Git checks; expiry requires hub
  refresh before setup or provider-backed network Git. An unavailable
  hub yields a typed retryable `LaunchSpecRefreshError`, and an
  explicitly hidden source fails closed for those operations.
- Starting a new local shell or agent session does not contact the hub.
  Generated agent context uses the last validated launch specification and may
  therefore contain stale provider labels while the hub is unavailable.
- Same-repository PRs fetch the hub-issued synthetic pull ref from the
  base clone. Fork PRs fetch the exact persisted fork clone URL and head branch;
  a spoke never derives a fork URL from provider cache state. Issue workspaces
  start from the persisted base clone/default branch. Ad-hoc and Kata workspaces
  continue through their local resolution paths.

## Terminal Transport

- Every terminal WebSocket leg negotiates RFC 7692 `permessage-deflate` with
  context takeover; Fleet relays apply the same policy to their separately
  terminated upstream leg. (`internal/terminalwebsocket/`)
- Use standard negotiation, not an application compression envelope: context
  takeover shares a dictionary across PTY messages, while unsupported peers
  continue with an ordinary uncompressed WebSocket.
- The browser pane that claims resize authority owns the complete character and
  pixel geometry; hidden or non-owning panes cannot overwrite it. Retained PTYs use
  an 8x16 fallback only until claimed browser geometry arrives (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::resizeVisibleTerminal`).
- A hub terminates the browser WebSocket and opens a separately
  authenticated WebSocket to the owning spoke. It forwards bytes and resize
  messages only; the spoke still owns tmux/ptyowner state, persistence, exit
  observation, and cleanup (`internal/server/fleetapi/fleet_proxy.go`).

## Natural Exit Rules

Natural process exit should collapse stale runtime state quickly.

- When a launched runtime session exits naturally, remove it from backend
  runtime state and from the workspace UI.
- If the exited session was active, return the UI to Home rather than leaving a
  dead terminal tab selected.
- If the session was tmux-backed, forget the persisted runtime tmux row once the
  backing tmux session is gone.
- When the shell drawer process exits, close or collapse the drawer, forget any
  persisted runtime tmux row once the backing tmux session is gone, and require
  a fresh launch on reopen.
- PTY-owner process exit may precede final output; keep subscribers alive for a
  bounded drain or loaded runners lose the last repaint
  (`internal/workspace/localruntime/manager.go::watchPtyOwner`).
- PTY output EOF may precede process wait; give wait a bounded chance to publish
  the real exit code without marking the owner fully exited before replay drains
  (`internal/ptyowner/owner.go::exitCodeAfterOutputClose`).

The base workspace `tmux` tab is the exception:

- Keep reconnect behavior for the base `tmux` tab.
- Do not auto-close that tab just because the websocket detached or the view
  remounted.

## Delete Ordering Rules

Workspace deletion is intentionally conservative.

- First decide whether deletion is allowed, including dirty-worktree checks.
- Persist deletion intent before destructive work; failures remain visible and
  retryable, while interrupted attempts become explicit failures after restart
  (`internal/server/workspaceapi/workspace_deletion.go::Handler.runWorkspaceDeletion`).
- Setup generations are process-local, so startup must change persisted
  `creating` rows to `error` and `deleting` rows to `deletion_failed`
  (`internal/server/workspaceapi/lifecycle.go::Handler.Start`).
- Lifecycle writers that act on a previously observed status must use a
  conditional transition. In particular, tmux pruning cannot replace an
  admitted `deleting` state with a stale `ready` to `error` write
  (`internal/db/queries.go::DB.MarkReadyWorkspaceError`).
- Only after a clean preflight may runtime sessions and shells be stopped.
- Only after runtime shutdown succeeds should destructive worktree and DB
  teardown continue.
- A live worktree registration at the persisted path in the resolved repository
  authorizes cleanup without an ownership marker; a same-repository replacement
  is the workspace, but a symlink to another worktree is not.
  (`internal/workspace/manager.go::gitDirOwnsCleanupWorktree`)
- A confirmed delete publishes workspace absence from the application workflow before presenter-specific navigation
  or failure UI; releasing the initiating presenter must not suppress tombstones, hosted-session cleanup, or route invalidation
  (`frontend/src/lib/components/terminal/workspace-runtime-workflow.ts::executeMutation`).
- Workspace `DELETE` and merge-cleanup admission are idempotent after
  authoritative absence; stale projections and retries succeed without
  publishing another deletion
  (`internal/server/workspaceapi/workspace_deletion.go::Handler.QueueWorkspaceDeletion`).

This ordering prevents a rejected delete from silently killing the user's live
workspace sessions.

## Server Shutdown Ordering

Workspace and Fleet own independent idempotent, context-bounded lifecycles;
Fleet starts after Workspace and shuts down its workers before Workspace stops
(`internal/server/fleetapi/handler.go::Handler.Shutdown`). Root closes
Pull admission and cancels its workers before HTTP drain, then waits for Pull before
Fleet in the post-drain dependency stage (`internal/server/server.go::Server.Shutdown`,
`internal/server/pullapi/handler.go::Handler.Stop`).
If any stage times out, shutdown must not advance; a later call resumes at the
blocked stage (`internal/server/workspace_dependency_shutdown.go::workspaceDependencyShutdown`).

Federated terminal sessions stay spoke-owned. The hub bridges the
browser WebSocket directly to the enrolled spoke's HTTPS origin and does not
create a local process, PTY, or durable transport session
(`internal/server/fleetapi/fleet_proxy.go::Handler.serveFleetWebSocketProxy`).

## Tmux Persistence Rules

Persisted tmux-backed runtime rows are only valid while the backing tmux session
still exists.

- Restore persisted runtime tmux sessions on startup only when the backing tmux
  session is still present.
- Treat "tmux session is no longer running" and equivalent dead-server cases as
  gone state to be cleaned up, not as a reason to preserve stale runtime rows.
- During explicit delete or stop flows, forgetting the persisted row is part of
  cleanup.
- Removal of a created runtime backend is attempted when launch or persistence
  fails; it is best-effort, not retried, and a backend that survives a failed
  compensation is unrecorded until startup reaping. Reaping must protect stored
  workspace, host, and project-worktree sessions while recognizing every owned
  scope-derived tmux name (`internal/workspace/manager.go::ReapOrphanTmuxSessions`).
  Caller-keyed command ensure uses one manager transaction, and per-worktree shell
  discovery uses one handler transaction, through persistence and compensation so rollback authority cannot race adoption (`internal/workspace/localruntime/command_session.go::EnsureCommandSessionAndPersist`, `internal/server/workspaceapi/projects_handlers.go::Handler.ensureProjectWorktreeRuntimeShell`).
- Persistence failures stay client-visible; compensation failures are logged.
  Ptyowner rollback needs a live owned entry, and tmux fallback cleanup must
  match the launch marker, which adoption rewrites so a stale creator rollback
  cannot kill an adopted backend (`internal/workspace/localruntime/manager.go::RollbackLaunch`).
- A runtime can exit while its metadata write is still in flight, so every
  launch path must reconcile after persistence: live means both key and
  `CreatedAt` match, and a replacement must not suppress deletion of the exited
  generation's row (`internal/server/host_runtime_handlers.go::Server.forgetHostRuntimeCommandSessionIfExited`, `internal/server/workspaceapi/projects_handlers.go::Handler.forgetProjectWorktreeRuntimeSessionIfExited`).
- During kenn-forge shutdown, detach/restart behavior is different: do not treat
  normal server shutdown as a natural user exit that should erase recoverable
  base runtime state.
- Development launchers and Forge-managed systemd services opt into
  restart-detach classification via `KENN_FORGE_DEV_RESTART`; managed systemd
  units also use `KillMode=process` so the supervisor does not kill durable
  tmux or ptyowner processes with the daemon. Terminal bridges close without an
  `exited` frame so the browser reconnects after restart. Genuine process exits
  and non-supervised shutdown behavior remain unchanged (`Makefile::dev`,
  `scripts/dev-stack-backend.sh`, `docker/backend-dev-entrypoint.sh`,
  `internal/fleetsetup/service.go`,
  `internal/workspace/localruntime/manager.go::Manager.Shutdown`).
- New tmux sessions use the `forge-` prefix; persisted `middleman-` session
  names remain valid and must not be rewritten (`internal/workspace/`).
- Only `DefaultTmuxCommand` selects Forge's dedicated tmux server. Custom tmux
  commands may address a shared user server, so Forge never changes their global
  options or applies graphics-off mutations (`internal/config/config.go::IsDefaultTmuxCommand`).
- The tmux server permanently retains its spawn environment for every pane to
  read via `show-environment -g`, and only `new-session` clients spawn it, so
  every Forge-issued tmux client runs with the non-secret allowlist
  environment, minus configured token names that could hide under the admitted
  `LC_`/`XDG_` prefixes — deny-by-default; never a bare strip list, which
  silently leaks any credential the configuration did not name. Pane processes
  (including the base workspace terminal, which keeps the credential-sanitized
  full daemon environment) get their environment only through the env-file
  handoff (`internal/workspace/localruntime/manager.go::TmuxClientEnvironment`,
  `internal/workspace/localruntime/manager.go::NewTmuxPaneHandoff`).
- The allowlist names exact variables only — never prefixes, which a secret
  such as `XDG_API_TOKEN` could hide under. It must keep `TMUX_TMPDIR`: tmux
  resolves `-L` sockets beneath it, so dropping it routes tmux clients to a
  different server than the manager owns. Attach commands returned to external
  callers always wrap in `env`, unsetting the caller's `TMUX` and setting or
  unsetting `TMUX_TMPDIR` to match the daemon. Fake tmux test fixtures bake
  their control paths into the script instead of smuggling them through the
  client environment
  (`internal/config/config.go::IsTmuxNonSecretEnvVar`).
- The non-secret terminal environment and credential names are disjoint by
  construction: config and Kata catalog validation reject `token_env` names
  that appear in the allowlist, because the tmux server's retained spawn
  environment can never be scrubbed after a name turns secret
  (`internal/config/config.go::Config.validateTokenEnvNamesNotTerminalVars`).
- Kata catalog `token_env` names feed every credential strip set on catalog
  load and at boot; catalogs load lazily per request, so the boot feed keeps
  earlier terminals covered (`internal/server/server.go::Server.updateCatalogStripEnvVars`).
- Every attach command passes `-E`: a pane can widen the server's
  update-environment, and without `-E` the next attach copies the attach
  client's variables into the session environment; external attach specs
  additionally run in the caller's unsanitized shell
  (`internal/workspace/localruntime/tmux_launcher.go::tmuxAttachSessionCommand`).
- Every tmux client attach must force UTF-8; service launchers may omit locale
  variables, causing tmux to replace non-ASCII output before WebSocket transport
  (`internal/workspace/localruntime/tmux_launcher.go::tmuxAttachSessionCommand`).
- On macOS, tmux and PTY-owner shells with no configured `LANG`, `LC_ALL`, or `LC_CTYPE` must
  default `LC_CTYPE` to UTF-8 without overriding an explicit locale; launchd can
  otherwise leave zsh with incorrect Unicode prompt widths (`internal/workspace/localruntime/shell_environment.go::shellCharacterLocaleDefault`).
- Forge's dedicated tmux server owns global passthrough, SIXEL, and mouse mode;
  live changes clear pane overrides, while custom servers receive passthrough
  only on Forge-owned panes and only while graphics are enabled
  (`internal/workspace/manager.go::Manager.ApplyTmuxGraphics`).
- Startup applies saved graphics and mouse settings before restoring retained
  clients on an existing dedicated tmux server
  (`internal/server/server.go::newServer`).
- Live graphics or mouse changes are best-effort; graphics updates try every
  managed pane before reporting combined failures, and setup reapplies the
  pane state
  (`internal/server/settings_handlers.go::Server.applyTmuxGraphics`).
- Enabling graphics installs each replacement retained client before detaching
  the old one; browser reconnects must always find an attachment while panes
  stay running
  (`internal/workspace/localruntime/manager.go::Manager.ReattachTmuxClients`).
- Terminal graphics are optional and default on. The browser supports SIXEL and
  iTerm images; Kitty graphics remain disabled while the xterm add-on labels
  that protocol alpha (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::syncImageAddon`).
- Retained tmux attach PTYs must start with nonzero pixel geometry: tmux asks for
  pixels before a browser subscribes and otherwise never retries the unanswered
  query needed for native SIXEL (`internal/workspace/localruntime/tmux_runtime.go::startTmuxAttachSession`).
- Local-runtime reconnects restore browser-generated cursor-key, mouse, focus,
  and paste DEC modes from session-wide PTY state, not bounded screen replay
  (`internal/workspace/localruntime/manager.go::session.subscribe`).
- Initial agent handoff requires observed bracketed-paste mode, then sends the
  opening frame, prompt, and closing frame in one terminal write and, after a
  short fixed settle delay, Enter as a separate write. Agent TUIs treat bytes
  in the same chunk as the paste-end marker as part of the paste, so an Enter
  in that chunk leaves the prompt unsubmitted. The paste-delay-Enter sequence
  holds the session input lock that attachment writes also take, and it runs
  to completion even when the caller's deadline expires first. Do not gate
  Enter on terminal echo: Claude renders pasted input only after the Enter
  event (`internal/workspace/localruntime/manager.go::session.submitInitialMessage`).
- Mode transitions precede one session-wide UTF-8-aware VT tail even in the
  alternate screen; retain split-rune introducers and decoded C1 controls/ST
  (`internal/workspace/localruntime/terminal_sequence_tail.go::trailingIncompleteTerminalDataLen`).
- Mode replay must mirror xterm.js 6 effective semantics: DEC private save/restore
  and ignored 1005/1015 encodings cannot re-enable modes or displace 1006/1016
  (`internal/workspace/localruntime/terminal_input_modes.go::terminalInputModeState.observe`).
- Recognize C1 CSI only as decoded U+009B; valid non-C1 UTF-8 invalidates pending
  controls, while invalid scalars, BOM, and standalone continuation bytes are
  decoder-discarded without reaching the VT parser
  (`internal/workspace/localruntime/terminal_input_modes.go::terminalInputModeState.observe`).
- Send reconnect cancellation to xterm as bytes, not text; only binary input
  clears its streaming UTF-8 decoder before replay
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::cancelPendingTerminalSequence`).
- Direct local-runtime sessions request a subscriber-only replay boundary and
  withhold resize/refresh controls until xterm parses it; the backend queues the
  boundary only after the retained VT/UTF-8 tail reaches ground. Fresh legacy
  and Fleet attachments still receive dimensions on every connection
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::connect`,
  `internal/workspace/localruntime/manager.go::session.subscribeInternal`).
- In xterm.js 6, DECSTR resets cursor keys, focus reporting, and bracketed paste,
  but leaves the mouse service's protocol and encoding unchanged
  (`internal/workspace/localruntime/terminal_input_modes.go::terminalInputModeState.softReset`).

## UI Contract Rules

The workspace UI should reflect runtime truth without leaving users stranded in
stale tabs.

- Runtime lists returned by `/workspaces/{id}/runtime` are the authoritative
  backend view of live launched sessions.
- Manual stop settlement must bound every awaited stage and publish confirmed local absence before any
  best-effort refresh; no stalled transport, authority read, or presenter may retain the pending control
  (`frontend/src/lib/components/terminal/workspace-runtime-workflow.ts::makeWorkspaceRuntimeWorkflow`).
- Publish confirmed workflow sessions before best-effort reloads, but keep those additions provisional: only a successful
  runtime read may tombstone absent peers. Reject only definite failures; reconciliation decides uncertain launches
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::presentRuntimeMutation`).
- Workspace terminals use xterm.js exclusively; there is no renderer setting
  or alternate renderer path (`frontend/src/lib/components/terminal/TerminalPane.svelte`).
- Terminal clients must replace silent half-open sockets without a page reload; every local terminal backend must answer the shared round-trip heartbeat, and Fleet bridges must pass it through so idle output is not required for recovery
  (`frontend/src/lib/components/terminal/terminal-session.ts::makeTerminalSessionController`,
  `internal/terminalwebsocket/terminalwebsocket.go::WriteHeartbeat`).
- Own every non-empty browser text paste at the terminal container boundary,
  sanitize and send it once, and do not delegate single-line paste to xterm;
  this event path must remain usable on insecure HTTP origins without the async
  Clipboard API (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalPaste`).
- Treat terminal processes as native-terminal-equivalent, but accept bounded, write-only OSC 52 writes only after one
  recent one-shot trusted DOM gesture; terminal data callbacks are not input provenance, and browser denial falls back
  through CSRF-protected loopback (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalKeyDown`).
- Keep OSC 52 validation synchronous and ahead of gesture consumption; replacing it with `@xterm/addon-clipboard`
  would require custom prefilters, nonblocking write separation, and handler-order coupling to preserve current
  rejection, parser-progress, and read-denial guarantees.
- Terminal clipboard authority is revoked on external pointerdown/focus transfer, pane inactivity/parking, disablement,
  or window focus/visibility loss; pointerdown capture closes the race before a competing browser copy starts, and
  revocation also stops in-flight browser-to-loopback fallback chains before their next stage
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleDocumentPointerDown`).
- Terminal pointerdown only prepares clipboard access; selection intent requires both a four-CSS-pixel dead zone and
  movement of roughly one rendered terminal cell, so physical focus-click jitter expires without reaching browser or
  loopback clipboard fallbacks at readable terminal geometry without suppressing deliberate drags.
  Confirmed captured drags retain authority only through internal or destinationless focus movement while the pane stays
  active; their watchdog also releases browser capture, so a missing release cannot shield later focus loss
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::hasPointerSelectionIntent`).
- Host clipboard writes require a local browser; trusted loopback proxies must report exactly one client IP assigned
  to the host, because the proxy's loopback `RemoteAddr` alone does not establish browser locality
  (`internal/server/terminal_clipboard_access.go::isLocalTerminalClipboardRequest`).
- During active tmux SGR drags outside xterm bounds, add only clamped edge wheel, drag, and release reports; forward
  all other mouse reports unchanged, and never retain unsent drag state across a WebSocket boundary
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::connect`).
- Agent TUIs in the normal buffer with no local scrollback receive vertical wheel gestures as cursor input; xterm/tmux
  keep ownership when scrollback, the alternate buffer, mouse tracking, or browser Ctrl-wheel zoom is active
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalWheel`).
- Touch-fling momentum must never reach mouse-tracking apps as unpositioned wheel reports: xterm's post-lift
  `-xterm-gesturechange` events omit `clientX`/`clientY`, so the pane pins them to the last finger position in a
  capture listener before xterm encodes `ESC[<65;NaN;NaNM`
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalGestureChange`).
- macOS loopback clipboard fallback must run `pbcopy` with `LC_ALL=en_US.UTF-8`; service launchers may omit
  a UTF-8 locale and make `pbcopy` reinterpret unchanged UTF-8 input
  (`internal/systemclipboard/systemclipboard.go::nativeWriter.WriteText`).
- Windows loopback clipboard fallback must send UTF-16LE to `clip.exe`; UTF-8 stdin is code-page-dependent and corrupts
  non-ASCII text (`internal/systemclipboard/systemclipboard.go::encodeUTF16LE`).
- The frontend may react immediately to terminal exit events, but should then
  reconcile with a runtime refresh.
- Only the active terminal pane may publish cell geometry; font-metric changes
  must refit and publish columns/rows through an ownership-aware geometry control because unchanged
  container pixels do not trigger resize observation (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::resizeVisibleTerminal`).
- Keyboard and pointer interactions inside workspace rows must not trigger
  unintended navigation when the user is targeting a nested control.
- Persisted "last active tab" state must be scoped per workspace.

## Released Terminal Retention

- A live view's desired set claims its sessions even when a tab is hidden; only unclaimed sessions enter the bounded, release-ordered LRU, and a zero limit disables retention (`frontend/src/lib/stores/session-host.svelte.ts::noteSessionReleased`).
- While a workspace switch awaits destination runtime reconciliation, cache trimming must protect that destination prefix; otherwise releasing the previous workspace can evict the pending cache hit at capacity (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::releaseOwnedSessions`).
- Retention keeps the parsed xterm subtree and connected socket but relinquishes interaction, resize, and WebGL resources; reclaim reparents the same subtree without reconnect or replay (`frontend/src/lib/components/terminal/PooledSessionTerminal.svelte`, `frontend/src/lib/components/terminal/XtermTerminalPane.svelte::syncRendererState`).
- Released sessions are browser presentation cache, never runtime authority: disconnect, exit, deletion, or stale generation causes app-level final discard rather than background reconnect (`frontend/src/lib/stores/session-host.svelte.ts::noteSessionExited`, `frontend/src/lib/stores/session-host.svelte.ts::discardSessionsWithPrefix`).

## Shell Command Override

When tmux is unavailable, the plain shell session is launched through ptyowner
rather than as a direct child of kenn-forge. This decouples shell ownership and
lifetime from the kenn-forge server process. Hardened deployments (systemd
services with `SystemCallFilter=~@privileged`, `LockPersonality=`,
`MemoryDenyWriteExecute=`, etc.) can still need a `[shell] command` wrapper or
external ptyowner manager path that starts the shell outside the restricted
service unit: zsh and bash both call `setresuid(uid, uid, uid)` during startup to
drop saved-uid privileges, and that syscall is in `@privileged`.

For these deployments, set `[shell] command = [...]` to wrap the launch
in something that escapes the parent unit's filter. On systemd hosts,
`systemd-run --user` spawns a fresh transient unit with its own
(unfiltered) policy:

```toml
[shell]
command = [
  "systemd-run", "--user", "--quiet", "--collect", "--wait", "--pipe",
  "--service-type=exec",
  "--property=KillMode=process",
  "--description=kenn-forge shell",
  "--",
  "zsh",  # absolute path or PATH-resolvable name; see below
]
```

Notes:

- `cwd` is propagated by the runtime via `cmd.Dir` — your wrapper must
  forward it to the actual shell. With `systemd-run`, that's
  `--working-directory=$PWD` (or a fixed path); without an explicit
  flag the transient unit does not inherit the launcher's working
  directory.
- The configured argv is invoked verbatim (no shell expansion). The
  first element must be an absolute path or a `PATH`-resolvable name;
  relative paths are rejected so a malicious worktree cannot drop a
  binary into itself and gain code execution.
- When unset, the runtime falls back to `$SHELL`, then `/bin/sh`. This
  is the safe default for unhardened single-user installs.

The `[tmux] command` setting follows the same wrap-it-in-systemd-run
pattern for similar reasons; the two are independent.

## Switch-Timing Instrumentation

The frontend emits one-shot `workspace-switch:<phase>` User Timing measures per
workspace switch (route selection through terminal first paint), recorded via
`frontend/src/lib/instrumentation/workspaceSwitchTiming.ts`. The phase names
are stable API for before/after performance comparisons — do not rename them,
and record new phases through that module so superseded-switch and duplicate
guards keep applying. `make profile-workspace-switch` captures a reproducible
profile; see `frontend/tests/profiling/README.md`. Each measure's `detail.traceId`
joins it to the same request's server-side OTel trace, whose export is opt-in
via `OTEL_TRACES_EXPORTER`.

- Every frontend HTTP path, including hand-written runtime requests, must use
  the shared traced fetch boundary so `traceparent` and `baggage` are not lost
  when code bypasses the generated client (`frontend/src/lib/api/runtime.ts::tracedFetch`).
- Base-path routing must preserve the inner Huma route pattern for outer OTel
  middleware; otherwise prefixed API spans collapse to the base-path pattern
  (`internal/server/otel_middleware.go::stripPrefixPreservingPattern`).
- A workspace-switch trace ends at terminal first paint or after 30 seconds;
  cancellation and supersession must clear the matching fallback timer
  (`frontend/src/lib/instrumentation/workspaceSwitchTiming.ts::endSwitchTrace`).
- Automatic HTTP tracing excludes only exact long-lived stream routes/modes;
  short endpoints such as telemetry event capture remain traced
  (`internal/server/otel_middleware.go::otelTraceable`).
- Federated terminal WebSockets need their own bounded attach span, ending
  after setup and before the long-lived spoke-to-browser bridge
  (`internal/server/fleetapi/fleet_proxy.go::startFleetAttachSpan`).

## Testing Expectations

Prefer full-stack coverage when the bug crosses backend lifecycle and frontend
behavior.

- Use real SQLite-backed server tests for delete ordering, tmux cleanup, and
  runtime-session API behavior.
- Workspace/Projects handler and Git-heavy wire tests belong to
  `internal/server/workspaceapi` or `internal/server/workspacetest`; Git and
  worktree cases in the public wire lane must acquire its weighted semaphore.
- Root-retained Git tests must cross a root composition boundary and acquire
  the root Git semaphore before expensive setup; `t.Parallel` alone is never
  a Git-work concurrency bound (`internal/server/api_test.go::acquireRootWorkspaceGitSlot`).
- A server test that creates a workspace must wait for setup to reach a terminal
  state (`waitForWorkspaceReady`) before it returns. The `202 Accepted` create
  runs clone/setup in a background goroutine; if the test returns first, that
  goroutine can keep writing into the test's `t.TempDir` clone path and race
  `RemoveAll` teardown, failing intermittently with "directory not empty".
- Workspace `ready` can precede the post-setup pushed-head observer pass. Tests
  that mutate upstream state after readiness must use a state that observer
  cannot repair (`internal/server/workspaceapi/routes_handlers.go::Handler.runWorkspaceSetupWithBasePath`).
- Spoke lifecycle tests for provider-backed workspaces seed a launch specification
  and leave provider-item tables empty. Lease-expiry tests use the handler clock
  so admission, setup, and HTTP error mapping observe the same instant.
- Kata API fixtures must use their package-private tmux server and force-delete
  created workspaces; shutdown preserves durable base sessions, so temp-dir
  cleanup alone leaks (`internal/server/kata/testmain_test.go::TestMain`).
- Use tmux wrappers/fakes for missing-session and dead-server cases.
- Tests exercising the unconfigured tmux default must sandbox `TMUX_TMPDIR`
  and probe the `kenn-forge` socket name literally; deriving the probe from
  the default command would follow a regressed default and pass
  (`internal/server/workspacetest/default_tmux_socket_test.go`).
- Add frontend or Playwright coverage when the regression is visible in tab
  selection, shell drawer state, or workspace navigation.
- Federation terminal tests must cross an authenticated TLS boundary; a
  handler-only test cannot prove credential forwarding, WebSocket proxying, or
  spoke-owned runtime behavior.

Related intent docs:

- [`context/workspace-apis.md`](./workspace-apis.md) for workspace API scope and
  non-goals.
- [`context/ui-interaction-contracts.md`](./ui-interaction-contracts.md) for
  row/button, tab, and keyboard interaction expectations in the UI.
