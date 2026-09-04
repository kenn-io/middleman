# UI Interaction Contracts

Use this document for frontend behavior changes where the risk is not visual
style but stale identity, broken persistence, or surprising interaction
semantics.

## Purpose

- Make behavior-level UI contracts explicit.
- Keep route identity, persisted browser state, and keyboard/pointer semantics
  consistent across the app.
- Prevent narrow regressions that usually show up only after review or in e2e
  flows.

## Browser Runtime Constraints

- Do not require secure-context-only browser APIs: remote daemon access may use
  plain HTTP, so local identity tokens must use runtime-independent identity
  (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte:workspaceRefreshOwner`).

## Identity And Route State

Interactive surfaces must agree on which item is selected.

- Treat `platform_host` as part of PR and issue identity in route state, drawer
  state, and stale-detail guards.
- When host is omitted for a provider's default host (Activity URLs,
  provider-default routes), normalize comparisons and cache keys with
  `frontend/src/lib/api/provider-routes.ts::resolvedPlatformHost` so the
  concrete default host and an omitted host do not look like different items.
- Route segments and item references may carry provider aliases (gh/gl/fj)
  while store data uses canonical names: every identity comparison or cache
  key derived from `provider` must canonicalize it first
  (`frontend/src/lib/workspace-inline.ts::identityEquals`). This includes
  route-reset/generation effects that detect item changes — tracking raw
  props treats an alias-only re-expression of the same item as a new item
  and discards in-flight work.
- Workspace item identity includes the item type, canonicalized across caller
  vocabularies ("pull"/"pr"/"pull_request") by
  `frontend/src/lib/workspace-inline.ts::canonicalItemType`: a PR and an issue
  can share a repo and number, so repo+number alone must never key claims,
  overrides, or deletion tombstones.
- Use shared named route/item reference types from
  `frontend/src/lib/stores/router.svelte.ts` instead of repeating anonymous
  `{ owner, name, number }`-style shapes.
- Markdown cross-references stay in-app on every surface: `#N`, `owner/repo#N`,
  and pasted provider item URLs on the rendering repository's host all render as
  `item-ref` anchors that resolve through the shared click handler, with the
  provider URL kept as the untracked-repo fallback
  (`frontend/src/lib/utils/item-reference.ts::parseProviderItemURL`).
- Terminal item links use the same resolver: match provider hosts from the
  configured repos, never a hardcoded host list, and let the resolve endpoint
  decide tracked-vs-external (`frontend/src/lib/utils/item-reference.ts::parseConfiguredProviderItemURL`).
- When a view changes from item A to item B, reset transient action state that
  could otherwise submit or render against the wrong item.
- A response confirming a server-side outcome (a completed delete or create)
  must publish to identity-scoped global state — claims, tombstones, creation
  overrides, route memory — before any liveness guard: neither unmount nor a
  selection that moved on (an A→B→A round-trip bumps the request generation)
  may discard it, or the replacement UI re-offers the action for a duplicate
  submission. Gate only presentation — refetches, prompts, flashes,
  navigation — on the live component and current selection
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::handleDelete`,
  `frontend/src/lib/components/detail/PullDetail.svelte::createWorkspace`).
  The pending request is identity-scoped shared state too: component-local
  creating flags reset on route changes and remounts while the request is
  still in flight, re-enabling the action for a duplicate submission
  (`frontend/src/lib/stores/workspace-create-pending.svelte.ts`). The same
  store records confirmed creations for detail instances WITHOUT an inline
  controller (focus/mobile views), which otherwise only see
  the detail envelope; records reconcile away when an identity-matched
  envelope carries a workspace and clear on deletion by workspace ID. A
  workspace-absent envelope clears a created record or override only when
  its request STARTED after the confirmation (shared lifecycle tick,
  `nextWorkspaceLifecycleTick`): a stale pre-create fetch must not wipe a
  creation, but a post-create fetch reporting absence or a replacement
  workspace ID — or a 404 on the workspace itself, which also drops the
  cached envelope so liveness rendering shows the error state — means
  another client deleted it and the record must drop. Detail stores apply
  envelope payload and tick atomically (last-started-wins) so a stale
  response cannot pair with a newer tick, and tombstones mask only their
  own deleted ID — a fresh-ID created record supersedes them. Deleted
  workspace IDs persist for the session (`markWorkspaceIdDeleted`) and
  creation publications for a deleted ID are refused in both stores: a
  delayed create response that lost the race with its own deletion must
  not overwrite the tombstone or republish the record (IDs are never
  reused, so fresh-ID recreations pass the guard). The shared created
  record reconciles under the same rule as the host store's positive
  override — same-ID envelope or newer-tick request only; a stale
  different-ID envelope must not erase a recreation
  (`frontend/src/lib/stores/workspace-create-pending.svelte.ts::reconcileWorkspaceCreated`).
  Controller-less detail views (focus/mobile) AND the host
  store's `effectiveRef` (both its tombstone and no-override branches)
  resolve under one rule — `resolveControllerlessWorkspaceRef`, never
  bare `envelope ?? createdWorkspaceRef`: the created record wins over a
  different-ID envelope until reconciled (a stale pre-confirmation
  envelope must not shadow, or let the dock claim over, a confirmed
  recreation), a same-ID envelope wins for its fresher status, and
  session-deleted envelope IDs are masked — globally across identities
  and past tombstone reconciliation, since IDs are never reused. E2e mocks
  of the create POST must keep the detail envelope consistent (the real
  server inserts the row before returning 202). The
  host store's `effectiveRef` falls back to that record too — a create
  begun controller-less must surface on an inline surface after a layout
  switch, where no recordCreated override ever ran
  (`frontend/src/lib/stores/workspace-host.svelte.ts::effectiveRef`).
- Explicit create-and-launch intent starts under canonical item identity, promotes to `(workspaceHostKey, workspaceId)` before workspace publication or create cleanup, and gates automatic launcher rendering in either form; promotion queues the workspace form before clearing the item form so no render observes a gap
  (`frontend/src/lib/stores/workspace-create-pending.svelte.ts::promoteWorkspaceCreateLaunch`,
  `frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::automaticLauncherBlocked`).
  A successful claim stays pending until the exact session appears or bounded reconciliation expires;
  reconciliation runs on the application runtime, survives the initiating view's teardown, treats transient
  runtime reads as observations rather than terminal failures, and releases its read owner on exact-session
  evidence or timeout. The window ends 15 seconds after launch acknowledgement; expiry clears only the launch
  intent and reports that the session was not observed. A session that appears later remains authoritative and
  is discovered by subsequent runtime reads rather than stopped or relaunched. Sibling views suppress or retract
  their automatically opened empty fallback while the launch intent is pending and may discard only unclaimed intents
  (`frontend/src/lib/stores/workspace-create-pending.svelte.ts::acceptWorkspaceLaunch`,
  `frontend/src/lib/components/terminal/workspace-runtime-workflow.ts::reconcileAcceptedLaunch`).
- Inline surface claims come only from live selection effects (the list
  views' claim effects, which react to recorded overrides); async responses
  record overrides and tombstones but never claim a surface themselves, and
  the hosted workspace key follows the visible surface's claim
  (`frontend/src/lib/stores/workspace-host.svelte.ts::desiredKey`), falling
  back to the sticky key only while parked — otherwise a late response could
  expose the wrong terminal beneath another surface's detail.
- Deletion invalidation must not require a live inline claim: views release
  claims on unmount, so the workspace-host store keeps workspace-id → identity
  metadata past release and tombstones by remembered identity — and deletion
  callbacks carry the provider-aware identity themselves so workspaces never
  claimed inline (tab-only, sidebar deletes) still tombstone
  (`frontend/src/lib/stores/workspace-host.svelte.ts::notifyWorkspaceDeleted`).
  Tombstones carry the deleted workspace ID and suppress only envelopes still
  carrying that ID: an envelope with a different ID is a recreation that must
  surface immediately and reconcile the tombstone away — an ID-less tombstone
  would mask it forever, because the workspace-absent envelope it waits for
  never arrives once the item has a new workspace.
  Deleting the exact `(hostKey, workspaceId)` named by the active terminal route
  must replace that history entry with the Workspaces list; pushing a redirect lets
  Back rehost the dead workspace (`frontend/src/lib/stores/workspace-host.svelte.ts::notifyWorkspaceDeleted`).
- Merge success closes the modal after the cleanup-admission attempt, not teardown:
  `deleting`/`deletion_failed` remain workspace states, and only the provider-aware
  `workspace_deleted` event tombstones the row and reconciles visible item data
  (`frontend/src/lib/app-stores.svelte.ts::createAppStores`).
- Pull and issue detail actions route `deleting`/`deletion_failed` workspaces to
  Workspaces recovery; inline hosts must not reopen their terminals
  (`frontend/src/lib/components/detail/PullDetail.svelte::workspaceActionButton`).
- A `deletion_failed` workspace remains selectable so terminal recovery and the same confirmed force-delete action stay reachable on local and fleet rows; fleet requests
  forward `force=true` to the owning host (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::openWorkspace`,
  `frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::confirmDeleteWorkspaceFromList`).
- A terminal view whose loaded workspace is `deleting` or `deletion_failed`
  blocks its normal runtime and workspace actions. It renders deletion progress
  or confirmed force-delete recovery instead, retaining the exact workspace and
  fleet-host target through confirmation
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::openForceDeleteRecovery`).
- Catalog-backed routes must normalize missing selections even when the catalog
  is empty: select the first available item or `null`, and clear dependent route
  identity (`frontend/src/lib/components/docs/DocsWorkspace.svelte::loadFolders`).
- Commit user-initiated repository ref routes only after the selected tree
  loads; failed switches keep the picker query, prior ref identity, and last
  usable tree/path snapshot, remain retryable, and must not advance the URL
  to unloaded content
  (`frontend/src/lib/features/repo-browser/RepoBrowserFeature.svelte::selectRefFromPicker`).
- Treat an unresolved ref and its resolved-SHA route as equivalent only for the
  successful load that produced the alias; path/anchor changes reuse that load,
  while repository, ref, or resolved-SHA changes invalidate it
  (`frontend/src/lib/features/repo-browser/RepoBrowserFeature.svelte::loadRoute`).
- Contextual Kata panes keep association selection separate from task detail.
  Refreshing links may replace the selected identity only when it no longer
  exists; a detail response applies only to the daemon and issue UID that
  initiated it (`frontend/src/lib/stores/kata-links.svelte.ts`).
- Docs task references never route into Forge. They resolve a canonical issue
  and safe browser launch target through the folder's explicitly pinned daemon,
  then open Kata with `noopener,noreferrer`; missing, ambiguous, stale-daemon,
  and browser-unavailable outcomes remain in Forge as visible errors
  (`frontend/src/App.svelte::openKataReference`).
- Linking and unlinking update Forge-owned associations without mutating the
  Kata task. Open-in-Kata and create-workspace actions use the selected canonical
  daemon/issue identity, never display text or route-number fallbacks
  (`frontend/src/lib/components/kata/KataLinksPanel.svelte`).

Responsive layout changes must not change route identity.

- Resizing a canonical PR or issue route must not rewrite `/pulls/...`,
  `/pulls/.../files`, `/issues/...`, or `/host/{platform_host}/...` into
  `/focus/...` or `/m/...`.
- Responsive presentation decisions belong in the shell/rendering layer. Route
  builders still follow the active route family: canonical builders for
  canonical routes, focus builders for explicit `/focus` routes, and mobile
  builders for explicit `/m` flows.
- If a canonical list route renders with the focus presentation because the
  viewport is compact, selecting an item should still navigate to a canonical
  detail route.
- Distinguish compact desktop presentation from phone-like presentation in
  state names and tests. Compact desktop may hide sidebars or use the focus
  presentation; phone-like contexts may additionally use mobile typography,
  touch hit targets, and phone-specific action layouts.

Examples of transient state that should usually reset on identity change:

- inline edit drafts
- merge/close/reopen dialogs
- approve/review forms
- embedded detail-tab selection when the parent surface owns the item

## Persistence Scope

Persisted controls must state their scope clearly.

- Switching Forges is ordinary cross-origin link navigation. Do not keep a
  fleet-global selected host or retarget the link in JavaScript: every open tab
  and Forge origin owns its own route, filters, terminals, cursors, and browser
  storage (`frontend/src/lib/components/layout/ForgeSelector.svelte`).
- Browser-local preferences belong in `localStorage` only when the behavior is
  intentionally per-browser and not worth server settings.
- `Involves me` is three independent browser-local preferences for Pulls, Issues, and
  Activity; each enabled view sends the server query so filtering happens before limits,
  never through URL or config state (`frontend/src/lib/stores/involves-me-filter.ts`, `internal/db/queries_involvement.go`).
- `Unassigned` is three independent browser-local preferences for Pulls, Issues, and
  Activity. Each sends a pre-limit server query; only PR/issue subjects with an empty
  synchronized assignee set match (`frontend/src/lib/stores/unassigned-filter.ts`, `internal/db/queries_assignees.go::unassignedCondition`).
- `Referenced by PR` is a browser-local Issues preference. Every issue-filter
  presentation exposes the same control, and the server combines it with state,
  repository, search, starred, and involvement filters before limits.
  (`frontend/src/lib/stores/issues.svelte.ts::createIssuesStore`)
- `Referenced by PR` defaults off. Issue visibility reset clears it, and the
  compact `Reset view` action restores it with the other menu defaults.
  (`frontend/src/lib/components/sidebar/IssueList.svelte::resetVisibility`)
- `Referenced by PR` is available when any configured provider-host can supply
  reference edges. Active repository selections do not hide it. Issues with no
  edge do not match, and edges never cross provider-host boundaries. The graph
  has no separate badge or detail UI.
  (`frontend/src/lib/app-stores.svelte.ts::createAppStores`)
- Named repository preset definitions follow server settings, while the active
  repository selection and preset affinity remain browser-local; `Global` clears both
  (`frontend/src/lib/stores/filter.svelte.ts::setGlobalRepoPresetSelection`).
- Editing a preset-derived selection retains its overwrite target; an exact saved-set match supplies the label, and deleting the active preset keeps its repositories selected as an ad hoc scope (`frontend/src/lib/stores/repo-presets.ts::findMatchingRepoPreset`).
- Exact-set matching prefers browser-local preset affinity when multiple immutable preset names contain the same repositories. The selector catalog includes repositories from reachable fleet workspace responses and may prune a Workspaces-route selection only after local, fleet-discovery, and reachable-peer loads all succeed; failures preserve the selection (`frontend/src/lib/stores/workspace-repo-catalog.svelte.ts`, `frontend/src/lib/components/RepoTypeahead.svelte`).
- The workspace details tab is keyed by host-aware workspace identity; an unsupported
  tab may fall back only for the current live workspace, never rewrite another
  workspace's choice (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::sidebarTabStorageKey`).
- URL query state belongs in the route only when deep-linking or back/forward
  navigation is part of the feature contract.
- Activity detail selection uses one URL-backed slot for pull requests, issues,
  and default-branch commits so browser history restores one mutually exclusive pane
  (`frontend/src/App.svelte::updateDrawerURL`).
- Activity filters remain URL-backed and session-scoped. Missing filter params on a
  partial Activity URL inherit the last validated route before store hydration, while
  explicit URL values win (`frontend/src/lib/stores/router.svelte.ts::restoreMissingActivityFilters`).
- Activity URLs persist item scope (`item_types`) and event toggles (`event_types`)
  independently. Legacy `types` migrates both dimensions and owns them during session
  restoration; only exact `types=notification` means default item scope with no events,
  and `commit` alone controls the old Commits toggle (ignore stale `default_branch_commit`)
  (`frontend/src/lib/stores/activity.svelte.ts::readLegacyFilterSelections`).
- Activity's Author filter follows GitHub `author:` semantics: it matches the parent PR
  or issue author, never a child event actor; its URL/API key remains `author`
  (`internal/db/queries_activity.go::ListActivity`).
- Activity compact mode means a detail is active, not that its resizable feed pane is
  narrow; controls stay content-sized until their own container is narrow (`frontend/src/lib/components/ActivityFeed.svelte`).
- Activity's viewport-positioned filter panel leaves the feed's containment chain and
  owns focus while open; retain inline-size containment for compact control sizing
  (`frontend/src/lib/components/ActivityFilters.svelte::portalToBody`).
- Arrow/Home/End radio navigation selects and refocuses within that panel without
  honoring the pointer-only close-on-select behavior
  (`frontend/src/lib/components/ActivityFilters.svelte::handleRadioKeydown`).
- Activity author candidates follow only the current tracked-repository and time-range
  scope. Search text, activity type, and the selected author must not shrink the picker
  (`frontend/src/lib/stores/activity.svelte.ts::loadActivityAuthorsEffect`).
- That scope key deduplicates unchanged reads; it is not freshness authority. Activity
  remounts and feed reconciliation, polling changes, or full refetches must revalidate it
  (`frontend/src/lib/stores/activity.svelte.ts::loadActivityAuthorsEffect`).
- A foreground Activity load replaces a same-scope author read owned by supersedable
  reconciliation; joining that read can let its interruption strand stale candidates
  (`frontend/src/lib/stores/activity.svelte.ts::loadActivity`).
- Actions is standalone-only and stays inert until settings hydrate; disabling it releases
  workflow demand before replacing `/actions` with Activity (`frontend/src/App.svelte::syncWorkflowActionsAvailability`).
- Actions derives provider identity from globally filtered repository summaries and requires
  workflow catalog, run, and dispatch capabilities; unsupported repos stay visible without reads
  (`frontend/src/lib/components/actions/ActionsPage.svelte::supportsWorkflowActions`).
- Actions snapshot read failures never collapse into successful empty/current data: empty
  failures replace the empty state, while retained runs/jobs remain with a visible stale-data alert
  (`frontend/src/lib/components/actions/ActionsPage.svelte::workflowReadErrorMessage`).
- Run reads belong to the selected workflow: selection replaces the prior run
  projection and every generated request carries that workflow ID
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::selectWorkflow`).
- A dispatched run appears in the list from the dispatch response or the first
  `workflow_dispatch_progress` event, and later events update it in place; the
  list is otherwise refreshed only by user action
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::applyDispatchProgress`).
- PR Actions defaults open same-repository pulls to the head branch, but forks
  and non-open states to the target; workflows remain on merged pulls
  (`frontend/src/lib/components/detail/PullDetail.svelte::workflowInitialRef`).
- Server-backed settings belong in the API only when the preference should
  follow the user/config rather than one browser session.
- Settings controls persist on change. Do not add a Save button, a dirty draft,
  or a saving state that disables sibling controls; queue saves serially and
  build each payload from the latest persisted values
  (`frontend/src/lib/components/settings/DetailSettings.svelte::persist`).
- Bounded numeric settings take their limits from the generated
  `schema-constraints` module, never from hand-copied numbers, and show an inline
  invalid state instead of sending a request the server would reject
  (`scripts/generate-schema-constraints.mjs`, `frontend/src/lib/components/settings/DetailSettings.svelte::validateLimit`).
- Detail timelines apply the server-backed entry limit after filtering and grouping,
  then make the remainder explicit and mount it in bounded idle batches; harnesses
  that require every fixture row pass a large limit. An explicit full-timeline request
  survives event additions and edits for the same item, and resets only when item or
  presentation scope, including the exact active filter set, changes
  (`frontend/src/lib/components/detail/EventTimeline.svelte`).
- Concurrent controls for one server-backed settings object must share a
  serialized mutation path and reconcile only fields still owned by the settling
  mutation generation; value equality alone is ABA-prone, while stale full-object
  saves can erase unrelated preferences
  (`frontend/src/lib/stores/terminal-settings-persistence.ts::saveTerminalSettings`).
- A control may render in a neighboring panel without joining its persistence
  object: the managed-clone Roborev toggle appears under Workspaces but owns a
  separate optimistic queue, hydration generation, and `roborev` request payload
  (`frontend/src/lib/stores/roborev-settings-persistence.ts`).
- Server-backed settings with irreversible local effects must publish only after
  persistence succeeds, not during preview or optimistic save; lowering terminal
  retention destroys cache entries that rollback cannot reconstruct
  (`frontend/src/lib/stores/terminal-settings-persistence.ts::isDeferredSetting`).
- A settings form that snapshots its baseline must either merge sibling mutations
  or keep the form and those controls mutually gated while either save settles
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::terminalZoomSaving`).
- An idle settings queue must rebase from authoritative store values, excluding
  fields still owned by live preview; otherwise reloads are erased or drafts leak
  into unrelated saves (`frontend/src/lib/stores/terminal-settings-persistence.ts::settingsWithoutPreview`).
- Settings hydration must share the mutation hub; a stale read must
  preserve pending or newly confirmed fields and rebase active previews while
  retaining only generation-owned drafts
  (`frontend/src/lib/stores/terminal-settings-persistence.ts::hydrateTerminalSettings`).
- Server-backed settings forms must render and build mutations from the reactive
  authoritative store; a page-load snapshot can republish stale sibling fields
  after hydration (`frontend/src/lib/components/settings/WorkspaceSettings.svelte::workspaces`).
- Settings that select a runtime must hydrate before that runtime starts, but
  the gate must abort timed-out or superseded reads and expose retry rather than strand the surface
  (`frontend/src/lib/components/terminal/WorkspaceEmbedShell.svelte::loadTerminalSettings`).
- Concurrent startup and embedded-shell callers share the last successful settings snapshot;
  every accepted settings command invalidates that cache entry through the same acknowledged
  workflow, backend readiness is not part of the settings-request timeout, and an invalidated
  in-flight read cannot publish into the next generation
  (`frontend/src/lib/app/startup-workflow.ts::StartupWorkflowLive`, `frontend/src/lib/stores/settings-workflow.ts::SettingsWorkflowLive`).
- Backend readiness polling belongs to the active application-startup fiber: stopping the full app shell interrupts
  the poll and closes its scoped response, while a ready backend starts the separately bounded settings read. A
  settings failure invalidates the startup cache so the next startup attempt performs fresh readiness and settings
  work (`frontend/src/lib/utils/appStartup.ts::runAppStartup`, `frontend/src/lib/utils/backendReadiness.ts`).

Whenever a control persists, document and test:

- where it persists
- whether it is global, per-view, or per-item
- what happens after navigating away and back
- for layout dimensions, clamp stale values on restore; temporary constraints
  and resize input below a valid minimum must not replace the saved preference
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::renderedRightSidebarWidth`)

## Keyboard Scope Precedence

Keyboard handlers must have one clear owner for each key press.

- Input fields, textareas, and contenteditable elements own printable keys while
  focused. Global shortcuts must not reinterpret those keystrokes, though
  modified bindings still dispatch.
- A focused TERMINAL owns every key, modified ones included, and outranks even
  the modal stack: a TUI binds Escape, function keys, and Ctrl/Cmd chords
  (Cmd-K and Cmd-P included), so any key the app reserves is a key the terminal
  loses. Only `Ctrl/Cmd-Shift-K`, the documented command-palette escape hatch,
  crosses this boundary; otherwise the dispatcher runs no handler and does not
  preventDefault. Ownership
  is matched from the terminal surface, not from xterm's hidden textarea alone,
  because focus also rests on the session wrapper. Popovers close from their own
  window Escape listeners, not from the registry
  (`frontend/src/lib/utils/keyboardShortcuts.ts::isTerminalKeyboardTarget`).
- xterm must advertise the Kitty keyboard protocol so terminal applications can
  negotiate detailed key reports instead of misreading legacy cursor input
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::start`).
- Browser paste shortcuts remain browser-owned inside xterm (Ctrl+V on Windows
  and Linux, Cmd+V on macOS); macOS Ctrl+V remains terminal input. Forge
  consumes each non-empty paste event once at the terminal container
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::isBrowserPasteShortcut`).
- Forge-owned paste matches xterm: LF and CRLF become one carriage return
  before sanitizing and framing; a standalone carriage return stays unchanged
  (`frontend/src/lib/components/terminal/bracketedPaste.ts::sanitizeTerminalPasteText`).
- Plain-HTTP remote terminals keep copy usable through a gesture-authorized copy-event fallback, but
  revocation must be rechecked between async clipboard stages so a rejected stale write cannot reach
  copy-event or server fallback (`frontend/src/lib/components/terminal/terminalClipboardWriter.ts`).
- On insecure origins right-button mouse events stay out of tmux so the browser context menu remains usable
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleInsecureTerminalRightMouse`).
- Modal frames outrank page-level shortcuts. When a modal, drawer, popover, or
  command surface is active, route and list navigation should run only through
  actions explicitly registered for that active surface.
- Registry actions also back the command palette: remove only an accelerator by
  setting `binding: null`; deleting the action removes the palette command too
  (`frontend/src/lib/components/keyboard/Palette.svelte::visibleActions`).
- If two surfaces can expose the same binding, document the precedence in the
  action registration rather than relying on registration order.
- Shortcut labels and cheatsheet entries must match the actual key event
  contract, including required modifiers.
- Async shortcut handlers should report failures through the same user-visible
  error path as pointer-triggered actions, and must not leave the action marked
  in-flight forever.
- Components that stay mounted while hidden (anything reparented under the
  workspace host, which parks on every page) must gate window-level listeners
  and geometry persistence on `hostVisible`: a parked view must not consume
  shortcuts on unrelated pages or clamp and persist layout measured from
  `display:none` geometry
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::toggleRightSidebar`).
  Transient popovers (and any dialog nested inside them) close when
  `hostVisible` goes false; only dialog open-state designed to restore on
  reveal may persist the hidden window
  (`frontend/src/lib/components/terminal/TerminalOptionsMenu.svelte`).
- Panes hidden behind a maximized one stay mounted with live window-level
  command listeners: a command that opens detail UI must un-maximize first so it
  cannot build an invisible overlay
  (`frontend/src/lib/components/detail/PullDetail.svelte::onOpenLabelPickerCommand`).
- A simultaneous pane is active exactly while DOM focus is inside it. Only that
  focused pane consumes pane-scoped window keys. Pointer interaction changes the
  active pane only when normal browser behavior moves focus; wheel input never
  moves focus or changes the active pane. Mark actual focus with a subtle inset
  border without replacing control focus styling. Paint that marker as part of
  the pane itself, below descendants; a high-z generated overlay cuts through
  popovers trapped in descendant stacking contexts. Because it paints below
  descendants, the leaf must reserve the marker's pixels: a constant 1px leaf
  padding keeps every child inside the content box while the inset shadow
  paints in the outer ring of the padding box, so opaque children (tab strip,
  diff surfaces, terminal canvases) can never cover it and no z-index is
  involved (`frontend/src/lib/components/shared/TabbedPanelTree.svelte`).
  A dedicated Files route keeps global diff shortcuts only while no pane or
  external dock has live focus; this fallback never paints active-pane styling
  (`frontend/src/lib/views/PRListView.svelte::diffKeyboardActive`).
- Focus callbacks carry the exact rendered leaf ID; never infer it from the
  persisted tree because narrow layouts render one synthetic leaf
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::focusPane`).
- A focused tab header reports that exact tab, including an inactive one;
  reporting only the leaf's selected tab misroutes ownership after dragging the
  focused header (`frontend/src/lib/components/shared/TabbedPanelTree.svelte::handleLeafFocusIn`).
- When a focused tab moves between rendered leaves, move live pane ownership
  with that tab even if its old leaf still renders; pooled focus restoration
  must not race a layout-host fallback (`frontend/src/lib/components/shared/DetailPaneLayout.svelte`).
- If a same-leaf tab change removes focused content without `focusout`, reclaim
  the layout only when focus fell to `document.body`
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::reclaimFocus`).
- Same-tab content identity replacement must also release disconnected focus;
  renderer DOM removal is the signal because pane layout state does not change
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::observeMutation`).
- Activity commit diffs use live pane `inputActive` for global shortcuts, not
  pane visibility (`frontend/src/lib/components/CommitDiffPanel.svelte::Props`).
- Nested pane trees paint focus only while DOM focus is inside both the nested
  pane and its containing pane, and only the deepest focused pane paints;
  workspace Workflow, Details, and bottom Terminal regions are siblings
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::renderedWorkspaceInputRegion`).
- If focused Workspace Workflow, Details, or bottom Terminal becomes unready or disappears while its host stays visible,
  reclaim the Workspace root only when focus fell to `document.body`; parking never reclaims focus
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::workspaceRoot`).
- Standalone Workspace shortcuts accept unclaimed `document.body` focus, but a
  concrete focus owner outside the Workspace root (including app chrome) blocks
  them (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::onKeydown`).
- A focused surface-hosted dock outside the pane tree becomes the live focused
  surface until focus leaves it. Persisted last-focused state is restoration and
  command memory only; it never represents current focus. Workspace window
  shortcuts run only while the validated workspace container has DOM focus
  (`frontend/src/lib/stores/paneLayout.svelte.ts::PaneRenderReport`).
- Resetting pane arrangement preserves a live external dock claim; only dock focusout,
  unmount, or renderer teardown releases DOM input ownership
  (`frontend/src/lib/stores/paneLayout.svelte.ts::reset`).
- Pane keyboard ownership follows the exact rendered leaf and tab containing DOM focus;
  nested tab identity never bubbles into an outer tree
  (`frontend/src/lib/components/shared/TabbedPanelTree.svelte::handleLeafFocusIn`).
- Preserve a focused node through no-destination `focusout` only for the immediate Effect-managed replacement window;
  keyed replacement restores focus, while an ordinary blur or document hiding cannot affect later DOM removal
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::handleLayoutFocusOut`).
- When focused content leaves a detail layout without `focusout`, release that layout's ownership. Reclaim layout focus
  only for disconnected content; connected pooled terminals keep their own restoration path
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::observeMutation`).
- A terminal dock tracks the exact focused descendant across silent session removal; it releases ownership when that
  node leaves the dock, restores a surviving dock only for disconnected content, and leaves connected pooled moves alone
  (`frontend/src/lib/components/terminal/DockedTerminalPanel.svelte::handleFocusOut`).
- Focus Terminal reveals, it never maximizes: a closed workspace pane reopens
  alongside the detail and a visible one keeps its arrangement. Maximizing over
  the detail is only ever an explicit user action. Reopening also has to clear a
  zoom held by any other leaf, or the revealed pane sits behind it
  (`frontend/src/lib/stores/workspace-host.svelte.ts::focusTerminal`).
- Terminal renderers autofocus only once, at creation, and only when a
  mount-time focus-intent guard still holds; explicit pool/host requests may
  focus an existing renderer separately. Reveal or enable effects never focus,
  or they fight the opt-in contracts above (`pendingHostFocus`,
  `shouldReclaimFocus`)
  (`frontend/src/lib/components/terminal/terminal-focus.ts`,
  `frontend/src/lib/components/terminal/XtermTerminalPane.svelte::start`).
- Active terminals must synchronously focus xterm on primary touch or pen
  pointerdown; xterm's built-in focus path is mouse-only, so a delayed handoff
  leaves the software keyboard closed (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalPointerDown`).
- A maximized inline workspace reuses the live hosted shell and fills the pane
  edge-to-edge; never add outer chrome or mutate the shell's workflow/terminal
  layout state (`frontend/src/lib/components/terminal/WorkspaceHost.browser.svelte.ts`).
- History semantics for a URL-bound pane follow the ARRANGEMENT, never which
  control the user touched: while the route-bound panes share a leaf a change
  pushes, and once they are split apart — both on screen — it replaces, so
  walking between them does not fill the Back stack. Keying off "click pushes,
  focus replaces" is wrong because a pane split into its own leaf still renders a
  clickable tab header. The URL wins over stored layout state on load: it
  activates the pane it names and drops a zoom held elsewhere
  (`frontend/src/lib/views/PRListView.svelte::routePanesSplitApart`).
- The stored pane tree is intent, not what is on screen: below the flatten width
  one pane renders however the tree is split, hidden panes stay in the tree, and
  a zoom covers every other leaf. Anything acting on the arrangement — palette
  split/zoom/close commands, the push-vs-replace history rule — reads the
  renderer's report and is unavailable until the host has been measured. The
  report distinguishes EDITABLE tabs (rendered, a legitimate command target even
  behind a sibling tab) from ON-SCREEN tabs (one per rendered leaf); only the
  latter answers "are both route panes visible at once"
  (`frontend/src/lib/stores/paneLayout.svelte.ts::PaneRenderReport`).
- Tab drag scopes are namespaced `<kind>:<id>` and matched by string equality, so
  an un-namespaced scope silently lets two unrelated trees exchange tabs. The
  primitive that moves tabs rejects one rather than trusting call sites
  (`frontend/src/lib/components/shared/tabbed-panel-drag.ts::assertNamespacedDragScope`).
- Pane availability must be derived at render time, not read back from an effect's
  result: a claim made in an effect lags one tick, and one tick of an unavailable
  pane prunes it out, collapses a split into a bare leaf, and remounts the whole
  subtree — losing scroll state and reparenting the live terminal
  (`frontend/src/lib/item-workspace-claim.svelte.ts::useItemWorkspaceClaim`).
  For the same reason the release-on-teardown guard reads its controller
  untracked: reactive reads re-run the effect on a mere prop reassignment and its
  cleanup then clobbers the claim just made in the same flush.
- Rendered visibility is not dock mode. A workspace pane that is neither hidden
  nor maximized still renders nothing when it is tabbed behind a sibling or
  buried under another leaf's zoom, and its portal slot is unmounted in both
  cases — so revealing it means unhide, activate its tab, AND clear a zoom held
  by another leaf, while leaving its own zoom untouched
  (`frontend/src/lib/stores/workspace-host.svelte.ts::workspacePaneVisible`).
- The hosted terminal is one live DOM subtree reparented between registered
  portal slots, so exactly one slot may be mounted at a time. A host that
  embeds a view owning its own workspace pane must not also wrap it in a
  second slot: registration order alone decides which one gets the terminal,
  and the loser renders empty
  (`frontend/src/lib/components/terminal/WorkspaceHost.svelte`).
- Session terminals are one live subtree PER SESSION KEY, owned by the app-level
  pool: every container — workflow tabs, the terminal dock, a promoted detail
  pane — renders a `SessionTerminalSlot` and none renders a `TerminalPane` of its
  own, or one tmux session gets two sockets. A live view's desired set claims its
  sessions even behind hidden tabs; only sessions released by every view enter
  the bounded retention cache (`frontend/src/lib/stores/session-host.svelte.ts::isSessionClaimed`).
- xterm WebGL texture atlases are shared across matching live panes; every explicit
  atlas clear must refresh sibling renderers, or cached glyph coordinates display
  repurposed characters (`frontend/src/lib/components/terminal/sharedTerminalTextureAtlas.ts::clearSharedTerminalTextureAtlas`).
- A pooled terminal constructs immediately, even in parking, so every mounted
  session keeps its websocket; retained sessions disable WebGL until reclaimed,
  and the pool opts out of renderer autofocus. After
  attachment the pool honors queued focus requests — explicit ones always, soft
  navigation-driven ones (a detail surface switched items) only when current
  focus is not sacred — and restores focus-event-tracked keyboard ownership: a
  real pane move rips the focused textarea out of the DOM silently (slot
  teardown fires no focusout), so never sample activeElement at park time.
  Ownership is revoked by any other element's focus claim, and by a park that
  settles with no destination (the pane closed) — a cross-flush transfer's
  transient no-destination park keeps it. A restore fires only into unclaimed
  focus and after the attachment's `inert` removal commits, since browsers
  silently ignore focus inside inert subtrees
  (`frontend/src/lib/components/terminal/PooledSessionTerminal.svelte`).
- A slot's `visible` means PAINTED, never FOCUSED. It gates INTERACTIVITY — an
  invisible slot's terminal is `inert`, dead to pointer and keyboard — so a
  container that reports only its focused session makes the other halves of a
  split unclickable. Every leaf of a split shares the container's own visibility
  (`frontend/src/lib/components/terminal/TerminalSplitTree.svelte`).
- Terminal SIZE and resize authority require PAINTED state plus a valid fit
  measurement, never focus: `visibility:hidden` retains geometry, while focus
  gating strands unfocused split leaves. Every pooled slot boundary must fill
  its painted leaf in both axes; horizontal block stretch alone can leave the
  xterm at a stale intrinsic height, so no vertical ResizeObserver result ever
  reaches the PTY. A container with no content box (a
  parked terminal) measures nothing, which is what keeps it from resizing a live
  tmux pane to one row — the measurement IS the check. Record a size as sent
  only once the socket carried it, or a resize computed before the socket opened
  is suppressed forever and the PTY keeps its launch default. Synchronize
  resize eligibility on every measurement, but passive lifecycle or geometry
  changes never transfer ownership among equal-priority attachments. The latest
  deliberate terminal action wins within a priority; activating a higher-priority
  local attachment may preempt an HTTP Fleet attachment. Effective pointer or
  arrow-key split/dock divider changes are deliberate for terminals whose fitted
  cells change; each affected session claims only its first cell change per gesture,
  while later changes remain ordinary live resizes. Zero-motion and passive reflow
  are not deliberate. Claims apply fitted dimensions and
  direct/HTTP Fleet streams close rather than forward following input when tmux
  settlement fails. Single-attachment workspace terminals apply claims as resizes
  because they have no competing attachment. Ordinary
  resizes apply for the current owner, only retain fallback dimensions for
  non-owners, and owner loss restores the most-recent eligible claimant. The
  preflight measurement establishes eligibility only:
  send xterm's dimensions after
  `fit()`, which measures again and may cross a cell boundary
  (`frontend/src/lib/components/terminal/TerminalSplitTree.svelte::terminal-leaf-body`,
  `frontend/src/lib/components/terminal/XtermTerminalPane.svelte::resizeVisibleTerminal`,
  `internal/workspace/localruntime/manager.go::session.resizeAttachment`).
- Terminal input stays immediate during reconnect replay: resize readiness must
  never queue or suppress keystrokes, because trapped input is worse than the
  brief stale-geometry window (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalMessage`).
- Only trusted user gestures, including xterm mouse-tracking button/wheel input
  and input-method composition, transfer terminal resize ownership; automatic
  protocol replies are transport traffic and must not claim for passive views
  (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::handleTerminalWheel`).
- A promoted session is recorded ONCE, in the detail surface's stored pane tree.
  Containers mask it out of what they render (derived, not an effect) and never
  prune their own stored trees, so demoting restores the tab order, split, and
  group the user chose. The pane body crosses the view/workspace boundary as an
  `InlineWorkspaceController` snippet: views get `{paneKey, label}` and pass
  their own `visible` back, and the generation-carrying registry key stays in
  `frontend/` (`frontend/src/lib/stores/workspace-host.svelte.ts`).
- A workspace pane holding exactly ONE session renders no chrome of its own: no
  header bar, no one-tab workflow strip. The pane's own tab takes that session's name
  (supplied through `InlineWorkspaceController`, since only the frontend knows the
  sessions), and its reopen strip follows. Two sessions, none, or a promoted sole
  session bring the chrome and the "Workspace" label back. A flattened surface keeps
  the chrome: it suppresses per-leaf strips, so the toolbar is the only thing left to
  carry the controls
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::soleEmbeddedSession`).
  The bare render also requires the surface's strip to actually name the session:
  in a solo-chrome leaf that strip is gone, so a sole WORKFLOW session keeps its
  inner one-tab strip (the only bar naming the agent), read from the render
  report's `soloChromeTabs`. A sole DOCKED session stays bare either way.
- A leaf holding ONLY the workspace pane renders no outer tab strip at all
  (`TabbedPanelTree`'s `soloChromeTabKeys`, wired in `DetailPaneLayout`): the pane
  draws its own strip inside, and an outer row saying "Workspace" named the same
  thing twice. Its remaining actions float top-right of the leaf instead: the hide
  X, caller extras, and Maximize. A solo-chrome pane has no pointer drag source;
  pane commands remain available, and a second tab or flattened surface brings the
  draggable strip back. The floating cluster must stack ABOVE
  xterm's internal layers (its overlay scrollbar slider is z-index 11 and hugs the
  same right edge; nothing between the leaf and xterm's internals is a stacking
  context) - below that, the scrollbar silently swallows clicks on the rightmost
  button while everything looks fine, and only Playwright's hit-target check
  ("intercepts pointer events") names the interceptor.
- The workspace container pane's tab goes away once EVERY session of that workspace
  sits in a promoted pane: its body would render nothing, and a pane with an empty
  body is a hole in the surface, not a pane. The workspace stays claimed and its
  controls stay hosted, so Launch is one click away from the promoted pane, and
  demoting any session brings the tab back
  (`frontend/src/lib/stores/workspace-host.svelte.ts::workspacePaneEmptyFor`).
- The bottom dock is NOT part of that chrome and stays, collapsed to its row, in a
  chrome-free pane: it is the only route to a second session, so dropping it made a
  one-session workspace a dead end. The one exception is a sole session that lives in
  the dock itself -- the stage is already showing it, and a dock underneath would aim
  a second slot at the same terminal host.
- An empty workflow container retires behind its surface-hosted bottom dock instead
  of resizing the recursive tree. Promoted panes then fill the stored branch, and
  demotion restores the untouched arrangement (`frontend/src/lib/stores/workspace-host.svelte.ts::workspacePaneRowOnlyFor`).
- A detail pane NEVER shows the workspace's own header bar (name, branch, Expand and
  Collapse Terminal, Delete). The pane's tab strip already names the workspace and
  carries its controls. A flattened surface keeps the chrome, since it has no
  per-leaf strip to carry any of it
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte`).
- A pane's tab strip carries ONE structural control, Maximize. Split right and Split
  down were removed: a single-tab leaf cannot split, so on the panes that most need
  it they were permanently greyed, and elsewhere they duplicated the two routes that
  remain -- dragging a tab to a pane edge, and the `pane.splitRight`/`pane.splitDown`
  palette commands. Tests split through the palette or the layout store, never a
  button.
- Deleting a workspace is a strip action, not a popover one: it is registered as
  `HostedWorkspaceControls.stripActions` and renders beside the controls trigger. It
  lives in exactly one place -- two Deletes with independent disabled and pending
  states is worse than one behind a menu -- and the surface passes
  `showStripActions` true only for the leaf holding the workspace pane itself, so a
  workspace split across leaves cannot grow one Delete per leaf.
- Every Delete entry point (strip icon, header bar, error panel) opens the same
  ConfirmDialog before any request is issued; the 409 force-delete prompt is a
  second, separate gate. Delete removes a worktree whose unpushed commits go with
  it, one click from a strip
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::handleDelete`).
- Buttons that share a row with a kit `IconButton --sm` (the solo cluster, the strip
  actions, the controls trigger) share its 24x24 box and 13px glyph. Three
  near-miss geometries in one row read as three unrelated controls.
- The dock modes (Expand Terminal / Show Details / Collapse Terminal) move into the
  pane's controls popover wherever the header bar is hidden, gated on exactly the
  complement of the header's own condition so neither state shows two copies. The
  leaf's close button is NOT a substitute for Collapse: it hides one pane, while
  collapse reaches the container and every session promoted out of it.
- A session the workflow tree is SHOWING mounts its terminal without a click: one per
  rendered leaf's active tab. Mounting only from the tab strip's select handler left
  every workspace opened with an agent already running showing an empty pane, which
  reads as broken rather than as one click away.
- Workflow presets are a standalone-Workspaces-tab surface only. A PR or issue pane
  hosts one workspace beside the thing being reviewed, so composing multi-session
  layouts there is chrome that pane was never asked for.
- Drag state is cleared by a drag-END broadcast, not only by the dragged element's
  own `dragend`: a drop that moves a tab into another leaf destroys that element
  first, so the strip it left keeps the gap and the dragging styling. The strip that
  accepted the drop adopts the dragged key to preview an insertion, so "this leaf no
  longer holds it" cannot tell a leftover from a live preview
  (`frontend/src/lib/components/shared/tabbed-panel-drag.ts::onTabbedPanelDragEnd`,
  `frontend/src/lib/components/terminal/terminal-drag.ts::onTerminalDragEnd`).
  The broadcast also hides body drop previews: trees nest (workflow tree inside a
  detail leaf) and a dragover bubbles through both, so both preview the same drag,
  but only the inner one consumes the drop - the outer's own drop handler reads
  the already-cleared payload as null and would leave its preview painted.
- Route authority over pane layout (`DetailPaneLayout`'s `routeTabKey` effect) is a
  TRANSITION, not an invariant: it activates the route's pane and drops foreign
  zooms only when the route names a different pane than last applied. The effect
  also tracks `tabs`, whose identity changes as a consequence of a zoom itself;
  re-asserting on every change silently undid Expand Terminal and Maximize.
- The pane controls popover is portalled to `<body>`. The leaf's action container is
  a stacking context (`position: relative; z-index: 2`), so a popover parented inside
  it is clamped under xterm's canvas layers, which compete one level up - every click
  lands on the terminal instead
  (`frontend/src/lib/components/terminal/WorkspacePaneControls.svelte::portalToBody`).
- A portalled popover whose own actions open modals sits BELOW the modal layer
  (`calc(var(--z-overlay) - 1)`), not level with it. Portalling puts it after every
  in-tree modal in document order, so an equal z-index paints it over the dialog it
  just opened.
- A renderer publishing what it shows must not clear that report from the
  publishing effect's cleanup. Cleanup runs before every re-run, so a consumer that
  feeds the report back into the renderer's inputs (the pane tab named from it) sees
  a null on each republish, changes the inputs, and the effect never settles. Clear
  it from a separate dependency-free effect, which only runs at unmount
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte`).
- A slot key computed from a session that can disappear is derived, and nullable.
  Child props are their own deriveds: they re-run on the flush that clears the
  session, before the `{#if}` guarding them is torn down, and a throw there aborts
  the app's whole render - the host stayed parked and its tab came up empty. Only the
  real-backend lane reproduces it; jsdom's flush ordering does not
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::soleEmbeddedSessionHostKey`).
- One embedded workspace view serves every selection on its surface, so anything
  it hands to a detail pane - the controls snippet, a mid-save busy flag, the
  launcher overlay's open state - is keyed by `(workspaceId, hostKey)`. An unkeyed
  flag survives the switch and acts on the next workspace: a popover pinned open by
  a write that will never report done, or a launcher covering a live terminal. Keyed
  by WORKSPACE WORK only: terminal font size and terminal options write app
  settings through one single-flight controller, so those are in flight for every
  workspace at once and keying them reports a control enabled that the controller
  is still refusing. Workspace-scoped writes are tracked as a SET of owners, since
  two workspaces can have one in flight at once
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte`).
- Promoting a session into a pane requires the workspace pane to be ON SCREEN, and
  that is enforced in `promoteSessionBesideWorkspace`, not per caller: holding a
  leaf in the stored tree says nothing about being visible, and the view keeps
  publishing its sessions from a parked host
  (`frontend/src/lib/stores/paneLayout.svelte.ts`).
- The inline dock mode covers EVERY pane of the hosted workspace - the container
  plus the sessions promoted out of it - so a container hidden while a promoted
  terminal is on screen is "split", not "collapsed", and collapsing hides exactly
  the panes it later restores. Which pane an expand or a Focus Terminal acts on is
  the workspace's last-focused one, recorded per WORKSPACE (views forward every
  focused pane; the host keeps the ones whose key names a workspace) so it survives
  promotion, demotion, and a visit to another item
  (`frontend/src/lib/stores/workspace-host.svelte.ts`).
- A promoted session pane is dropped from a surface's stored tree only on an
  authoritative deletion of its workspace. A stopped, exited, or reconnecting
  session is absent from the runtime in exactly the same way, and keeping the
  placement is what lets a relaunch reappear where the user put it. There is no
  session-deletion signal to react to either: the API's only session mutation is
  `stop-host-runtime-session`, a DELETE that means stop.
- Getting back to a collapsed dock restores the whole collapsed set, not the
  remembered pane alone: a container masks the sessions its workspace promoted, so
  revealing it by itself hands back an empty pane while the terminal the user asked
  for stays hidden. A ledger on record is what says a workspace is put away - not
  the derived dock mode, since the container tab is shared and another workspace's
  expand unhides it (`frontend/src/lib/stores/workspace-host.svelte.ts::restoreCollapsedPanes`).
- A deferred session focus is cancelled when its session unmounts and when the
  surface's claim changes. Left armed it waits for anything to mount under that key
  and steals the keyboard for a Focus Terminal pressed in a workspace the user has
  since left (`frontend/src/lib/stores/session-host.svelte.ts`).
- A pane tree node that leaves the on-screen tree takes its children with it, and
  those children read the removed node's `first`/`second` for the rest of that
  flush - `undefined`, while still mounted, including from a `ResizeObserver`
  batch. Every read of that prop is guarded: throwing there unmounts the whole
  surface, leaving a detail with no panes and no way back
  (`frontend/src/lib/components/shared/TabbedPanelTree.svelte`).
- The desktop `.app-main` clips overflow but must never become a scroll
  container; focus-driven scrolling there shifts every mode rail and creates
  matching chrome gaps (`frontend/src/App.svelte::.app-main`).
- The inline dock mode is DERIVED from the surface's pane layout — maximized is
  the workspace pane's leaf holding the zoom, collapsed is that pane hidden —
  never stored alongside it, or the two disagree when a leaf's own controls
  maximize a pane. A maximized workspace must not outlive its claim: the store
  un-zooms both when a claim is replaced by a different identity (`setClaim`)
  and synchronously on release (`clearClaim`), because a release-and-reclaim
  within one update gives the layout host no availability gap to notice and
  leaves setClaim no previous claim to compare against. Same-identity
  re-asserts (a ref status change) must NOT un-zoom
  (`frontend/src/lib/stores/workspace-host.svelte.ts::dockModeFor`).
- A collapse control must be reachable in every inline workspace state, not only
  from the ready toolbar: the creating, fetch-failure, and setup-error branches
  render their own collapse button, since the pane's own close control is the
  only other way out short of deleting the workspace
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::inlineCollapseControl`).
  Dock mode changes are pure local UI — never disable them behind mutation
  guards like `actionsBlocked`; only the modal-stack guard applies, and only
  to the expand direction.
- Every pane belonging to a workspace keeps direct non-destructive actions such
  as session launch; only destructive workspace actions are limited to the
  workspace-owner leaf (`frontend/src/lib/components/terminal/WorkspacePaneControls.svelte`).
- The workspace view renders by liveness, not cached presence: the previous
  workspace stays cached across an in-place A→B switch, and branching on
  `workspace` alone shows A's stale ready toolbar (with action guards
  engaged) while B is slow or failing. Gate the ready branch on
  `workspaceLive` so the loading/error states own the switch window
  (`frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte::workspaceLive`).

## Modal Ownership

Any surface that blocks background interaction must own both focus and
shortcuts while it is open.

- Opening a modal-like surface should push a frame before focus moves inside
  the surface; closing it should pop only that frame.
- Close behavior must be local to the active surface. Escape should not also
  close a parent drawer or trigger route navigation unless the child declined
  the key.
- Unmounting a subtree that holds focus (dock close, claim release) must
  reclaim focus for a deliberate target after the DOM update — and only when
  focus fell to `<body>` or was still inside the closing subtree, so a
  transition triggered in the background (e.g. a selection change un-zooming a
  maximized pane) never steals focus from a control the user moved to
  (`frontend/src/lib/components/shared/DetailPaneLayout.svelte::shouldReclaimFocus`).
- Background actions that are still visible should be disabled or skipped when
  their `when` predicate no longer matches the active modal state.
- Outside-click, focus-leave, and Escape close paths should converge on the same
  cleanup so stale frames, listeners, and highlighted rows are not left behind.
- Custom focus traps must cycle controls in rendered DOM order. If the trap
  builds the focusable list from a mixed selector list (`button, input, select,
...`), normalize the result by document position before wrapping Tab /
  Shift+Tab so selector-engine grouping cannot change keyboard order.

## Palette Persistence

Command palette state is browser-local unless a feature explicitly needs a
shareable URL or server-backed preference.

- Recent commands should store stable action references, not route-specific
  labels that become invalid after navigation.
- Stored recents must tolerate malformed JSON, unknown actions, and stale item
  references by pruning or ignoring bad entries without blocking the palette.
- Palette search, highlighted row, preview content, and command enablement
  should be derived from the current route context each time the palette opens.
- When palette content can overflow, keyboard navigation must scroll the
  highlighted result into view without moving focus out of the search field.

## Mobile Route Constraints

Mobile layouts may redirect between list and detail surfaces, but must preserve
the user's current item identity and deep link.

- Redirects should keep `platform_host`, owner, repo, number, and item kind
  together. Repo labels alone are ambiguous in multi-provider views.
- Desktop-only layout specs should opt out of mobile redirects explicitly so
  viewport changes do not make assertions pass against the wrong surface.
- Mobile detail routes should reset transient action state when switching items,
  the same way desktop split-detail routes do.
- Any mobile-specific back/forward behavior should be tested with direct links
  and with in-app navigation, not only from the default landing route.

## Nested Interaction Rules

Rows that contain buttons, links, or toggles need clear event ownership.

- Activating a nested control inside a clickable row must not also trigger the
  row's navigation or selection behavior.
- Escape should close drawers, split-detail panels, menus, or modals when that
  surface is currently active.
- Focus-visible states matter for controls that are visually subtle, such as tab
  close buttons or compact action affordances.
- Explicit clipboard controls copy provider/repository values exactly; copying is
  not code execution or a shell security boundary, so source UI must not filter,
  quote, escape, or add shell confirmation (`frontend/src/lib/components/diff/DiffFile.svelte`).
- If a component claims menu-like behavior, it must honor the keyboard and focus
  contract of that role. Otherwise, use simpler semantics honestly.
- Keep contextual Kata association selection available while detail refreshes.
  Background freshness work is not a navigation transaction and must not block
  link selection or Forge-owned actions
  (`frontend/src/lib/components/kata/KataLinksPanel.svelte`).
- An explicitly seeded target (for example the repository a workspace dialog
  was opened for) must never silently fall back to a last-used or first option
  when the seed cannot be resolved — leave the selection empty and require a
  choice (`frontend/src/lib/components/terminal/NewWorkspaceDialog.svelte`).
- Repository selectors treat the server's `hidden_from_ui` flag as authoritative:
  once a configured repository is hidden, every matching configured, fetched, or
  workspace-catalog entry stays out of the selector. Consumers must not reimplement
  visibility matching client-side. Selection normalization must drop hidden
  selections explicitly — general normalization preserves unknown values for
  glob-resolved repositories, so a hidden repo would otherwise keep filtering —
  and must clear against both the configured path and the server-provided
  `tracked_repo_path`, because selections created from catalog rows use the
  current route, which diverges after a provider-side rename
  (`frontend/src/lib/utils/repo-filter-values.ts::normalizeInteractiveRepoFilterSelection`).
  A host-pinned scope (`ui.hideRepoSelector`) is exempt: with no picker to
  rescope, dropping it would unscope every request, so pinned selections pass
  through normalization untouched
  (`frontend/src/lib/utils/repo-filter-values.ts::normalizeGlobalRepoSelection`).
- Roborev has no event replay cursor: reconnect after authoritative job-list reconciliation; a lost
  mutation response retains and fences its original target until authoritative observation, never
  replays the write. A confirmed POST stays acknowledged when its follow-up refresh fails; report
  refresh degradation separately. Cancel only the exact owner lease on teardown
  (`frontend/src/lib/stores/roborev/roborev-workflow.ts::RoborevWorkflowService`).
- Catalog reads use consumer-local owners: picker teardown or route replacement may cancel only that
  consumer, never review-run state or sibling repository resolution
  (`frontend/src/lib/components/roborev/RepoTreePicker.svelte::owner`).
- Workflow Actions seeds top-level refs from repository authority and reads jobs lazily once per
  expanded run; collapsing a run keeps its jobs and never refetches
  (`frontend/src/lib/components/actions/ActionsPage.svelte::expandRun`).
- Docs publish commands snapshot folder and message and remain application-owned after replacement;
  same-folder surfaces adopt pending or unacknowledged failure state, while completed success is never
  replayed into a later session (`frontend/src/lib/stores/docs-workflow.ts::DocsWorkflowService`).
- Repository issue creation remains application-owned after acceptance; retained provider-aware state survives
  page replacement, and only initial presenter adoption may replay it; ordinary summary refreshes must not reopen
  dismissed state. Replacing a presenter interrupts its in-progress delivery before the replacement adopts the
  state. Fence retry only for transport failure or stable `mutationOutcomeUnknown`; the bounded single-browser
  command queue applies backpressure outside the registry lock instead of rejecting an admitted mutation
  (`frontend/src/lib/components/repositories/repo-summary-workflow.ts::RepoSummaryWorkflow`).
- Every Docs resource has one read key across ordinary loads and mutation reconciliation; owner-local lanes
  cancel obsolete route reads without canceling another owner's accepted reconciliation. Owner generations
  protect replacements, while presenter leases retain refreshes until a current surface claims them
  (`frontend/src/lib/stores/docs-workflow.ts::DocsWorkflowService`).
- When a settings or Docs write may have committed before failing, reconcile through its application workflow:
  matching state is recovered success only when every requested field or section matches;
  contradictory state preserves the failure, and an inconclusive read fences duplicate submission. Retain pre-mutation
  absence evidence. Repository evidence
  includes canonical provider, resolved host, owner, and name
  (`frontend/src/lib/stores/settings-workflow.ts::SettingsWorkflowLive`,
  `frontend/src/lib/stores/docs-workflow.ts::DocsWorkflow`).
- Frontend uncertainty fences live for one browser application runtime. A deliberate reload clears unresolved
  evidence, so the user must verify fresh authoritative state before attempting that mutation again
  (`frontend/src/lib/app/runtime.ts::makeAppRuntime`).
- Project registration, clone, and new-worktree commands capture host/project identity and remain
  application-owned after acceptance; retained worktree acknowledgements are generation-owned, so an
  older reconciler cannot clear a replacement command or presentation fence
  (`frontend/src/lib/components/terminal/project-mutation-workflow.ts::ProjectMutationWorkflow`).
- Workspace runtime commands remain application-owned after acceptance and retain presentation by
  `(hostKey, workspaceId)` across surface replacement; one-shot delete presenters may shadow the route presenter,
  but retained uncertainty transfers when that presenter leaves. Failures from an abandoned visit must not surface
  in its replacement. Presenter replacement interrupts
  stale asynchronous delivery before it can publish. A transport failure retains a retry fence until fresh runtime
  authority proves applied or not applied; presets retain per-session progress so recovery never relaunches a
  completed step. A known API or payload failure is a definite mutation failure, while a failed refresh after an
  acknowledged success is presentation degradation and cannot reopen the mutation. These fences live for the one
  browser application runtime; a deliberate page reload clears them, so a user who reloads during an unresolved
  outcome must verify authoritative workspace state before attempting the action again
  (`frontend/src/lib/components/terminal/workspace-runtime-workflow.ts::makeWorkspaceRuntimeWorkflow`).
- Embedding host callbacks settle only after mutations are durably visible to the next authoritative
  snapshot; negative or malformed acknowledgements reconcile before the command is offered again
  (`frontend/src/lib/components/terminal/project-mutation-workflow.ts::ProjectMutationWorkflow`).
- Repository-browser commands use a mount-bound facade and fence every state publication;
  automatic README-first selection yields to user selection, and stale teardown cannot affect a successor
  (`frontend/src/lib/stores/repo-browser.svelte.ts::RepoBrowserMount`).

## Controlled Form Controls

A native form control driven from app state must not also run its own default
action, or the two fight and the control renders the inverse of its real state.

- A native `<input type="checkbox">` with `checked={state}` still toggles itself
  on a real click (the click default action). When app state owns the value,
  cancel that default action (`onclick` `preventDefault`) so the box only ever
  reflects `state`. The repo selector's tri-state checkbox
  (`frontend/src/lib/components/TreeCheckbox.svelte`) is the reference: a
  controlled custom control that suppresses the native toggle and drives
  selection from `onmousedown`, not click.
- Keep an underlying real `<input>` for accessibility and tests even when the
  visuals are custom: set `indeterminate` imperatively for the partial state, and
  expose `aria-checked` (`"mixed"` for partial) on the owning row.
- Tri-state selection that cascades to descendants must keep parent and child
  visuals consistent: a parent is checked only when all leaves are, partial when
  some are, unchecked when none are. A parent disagreeing with all its children
  is a desync bug.

## Filtering And Visibility Rules

Not every visibility control means "remove this entity entirely."

- Controls that toggle detail visibility should preserve the parent row unless
  the feature explicitly removes that category from the result set.
- Activity's Commits filter controls top-level default-branch commits only; it
  neither selects PR or issue opening rows nor hides PR timeline commits while
  PRs are enabled. Opening rows follow only the timeline events that can occur
  on that item kind (issues: comments), so Reviews or Force pushes never toggle
  issue opening rows
  (`frontend/src/lib/stores/activity.svelte.ts::buildActivityFilterTypes`).
- Activity recency for a PR or issue parent includes the event ledger the feed can
  render, visible notification timestamps, and lifecycle transitions (opening, comments,
  reviews, commits, force pushes, reopen, merge, close), never provider
  `updated_at`/`last_activity_at`: GitHub bumps those for mergeability recomputes
  and branch deletion, which read as phantom activity
  (`internal/db/queries_activity.go::prActivitySubjectAtExpr`).
- An in-window notification dates and retains its parent only while Notifications are visible;
  hiding it cannot admit or reorder an otherwise-old parent. Other event visibility filters do
  not redefine parent recency (`internal/db/queries_activity.go::listActivitySubjectsWithQueryer`).
- "Use workspace activity for recency" hydrates one global PR, Issue, and Activity opt-in. Disabled mode keeps provider sorting/display and hides cached workspace-only subjects while retaining `last_workspace_activity_at` as metadata (`frontend/src/lib/utils/effective-activity.ts::effectiveActivity`, `frontend/src/lib/components/ActivityFeed.svelte::visibleWorkspaceActivity`).
- "Hide bots" tests event actors on events and item authors on parent/workspace subjects;
  collapsed Activity keeps a visible event direct when this hides its parent summary
  (`internal/db/queries_activity_projection.go::DB.ListCollapsedActivityProjection`).
- Visibility controls backed by server filtering, including "Hide closed/merged" and
  "Hide bots", reload Activity so turning a filter off can restore rows omitted from
  the bounded server window (`frontend/src/lib/components/ActivityFeed.svelte`).
- Collapsed Activity cursors come from the same snapshot as the visible summaries and
  cover hidden children; clients must not derive forward-poll authority from a visible
  row (`internal/db/queries_activity_projection.go::DB.ListCollapsedActivityProjection`).
- Opaque Activity cursors preserve provider-controlled timestamps outside the Unix
  nanosecond range (`internal/db/queries_activity.go::EncodeCursor`).
- Empty Activity deltas must not rebuild or replace parent/workspace summaries
  (`frontend/src/lib/stores/activity.svelte.ts::pollNewItems`).
- Single-thread Activity expansion uses stable repository identity, a frozen upper bound,
  and the active event, search, and visibility filters (`internal/server/huma_routes.go::listActivityThreadEvents`).
- Globally expanded threaded Activity fences older foreground snapshots, then pages the
  bulk event read instead of fanning out per thread, including when settings or URL state
  initializes it expanded (`frontend/src/lib/stores/activity.svelte.ts::loadBulkActivity`).
- Progressive Activity mounting must retain its already-visible row window across routine
  projection updates; collapsing it during refresh makes the side pane flash
  (`frontend/src/lib/components/ActivityThreaded.svelte::mountedEntryCount`).
- Thread and bulk event pages belong to the repository, range, filter, and projection
  generation that started them. Scope changes discard late pages and prevent older
  requests from clearing newer loading or error ownership
  (`frontend/src/lib/stores/activity.svelte.ts::invalidatePagedActivityRequests`).
- Parent recency or child-ledger revision invalidates stale and in-flight thread pages
  before restarting; if bulk expansion is active, restart the bulk read instead. The
  ledger revision must advance for edits, deletes, and older backfills that do not change
  display recency. Changed parents discard cached children
  before reloading even when the authoritative parent snapshot is capped. New expanded
  parents load complete history.
  Otherwise authoritative reconciliation preserves loaded children and merges deltas; mobile and status
  totals consume the same summary authority (`frontend/src/lib/stores/activity.svelte.ts::projectAuthoritativeActivitySnapshot`).
- Any collapsed snapshot accepted after scope or filter invalidation clears child-page
  authority, then reloads each expanded parent or restarts bulk expansion; stale children
  never cross scopes (`frontend/src/lib/stores/activity.svelte.ts::projectAuthoritativeActivitySnapshot`).
- Thread and bulk pages publish only after complete success; failures preserve the
  pre-request projection. Thread retries restart the frozen read, and only the owning
  token may publish an error (`frontend/src/lib/stores/activity.svelte.ts::retryFailedThreadLoads`).
- Uncapped snapshots evict absent-parent children and their loaded-thread authority, so
  reappearing expanded parents reload history. Capped snapshots retain absent caches only
  without an active parent filter (`frontend/src/lib/stores/activity.svelte.ts::projectAuthoritativeActivitySnapshot`).
- Activity `capped` reports event overflow; only it triggers event reloads and the
  event warning. `item_activity_capped` drives only the independent parent-truncation
  notice; a search whose event matches overflow the event page also reports it, because
  event-matched parents are derived from that bounded page
  (`frontend/src/lib/stores/activity.svelte.ts::createActivityStore`, `internal/server/huma_routes.go::Server.listActivity`).
- Activity status totals deduplicate open subjects across event, parent, and workspace
  snapshots; authoritative parent lifecycle state overrides event state
  (`frontend/src/lib/components/layout/StatusBar.svelte::activityCounts`).
- Authoritative Activity parents rewrite retained event parent metadata and notification provider
  targets from the current parent URL; event-specific deep links stay event-owned. Key by stable
  identity so mutable routes cannot split renames or route reuse (`frontend/src/lib/stores/activity.svelte.ts::reconcileItemsWithParentSubjects`).
- When two data sources race, prefer the source that matches the user's current
  filter/scope rather than a stale but faster preview.
- Successful initial detail reads and detail sync convergence must directly
  reconcile visible PR, issue, and Activity lists; Activity's forward cursor
  cannot surface newly persisted events older than unrelated leading rows.
  Selection generations gate only detail installation, not successful-read or
  successful-sync invalidation (`frontend/src/lib/stores/detail.svelte.ts::reconcileListsAfterDetailSync`, `frontend/src/lib/stores/issues.svelte.ts::reconcileListsAfterDetailSync`).
- Detail poll paths — scheduled cycles and sync-completion refreshes — skip while provider
  synchronization is active; overlap lets cached data supersede the authoritative response
  (`frontend/src/lib/stores/detail.svelte.ts::startDetailPolling`, `frontend/src/lib/stores/issues.svelte.ts::startIssueDetailPolling`).
- Activity full-snapshot projections are latest-successful-request-wins across
  foreground and convergence reads. Incremental polling neither claims snapshot
  authority nor overwrites a later claimed snapshot; replacement polling does
  claim snapshot authority. Failed reads do not claim
  projection: convergence failure must not invalidate an in-flight foreground
  load or suppress an earlier successful convergence read, and foreground failure
  must not suppress same-scope convergence. Request-start order decides between
  successful snapshots, and a logical filter-scope change still fences the old result.
  A later successful snapshot fences older foreground failures; without one, a fenced
  foreground owner still settles loading (`frontend/src/lib/stores/activity-workflow.ts::ActivityWorkflowLive`).
- Projection is part of Activity snapshot scope, so collapsed and full responses cannot
  replace each other (`frontend/src/lib/stores/activity.svelte.ts::activityProjectionScope`).
  The `events` projection is delta-only; searched polling must not reconstruct full
  parent or workspace inputs that its response discards (`internal/server/huma_routes.go::Server.listActivity`).
  Every fourth scheduled Activity poll is an authoritative collapsed-snapshot
  replacement in collapsed threaded mode, so events hidden behind the forward
  cursor self-heal even without detail navigation or SSE without reloading the
  complete event ledger (`frontend/src/lib/stores/activity.svelte.ts::refreshActivityProgram`).
  Every authoritative snapshot projection clears an earlier error banner
  (`frontend/src/lib/stores/activity.svelte.ts::createActivityStore`).
- After a sync trigger is accepted, retain optimistic running state through the
  pre-trigger idle snapshot; accept completion only after running or a newer `last_run_at`
  (`frontend/src/lib/stores/sync.svelte.ts::applySyncStatus`).
- A trigger with no local status reads its baseline before POST, and invalidates any
  refresh that began before the trigger (`frontend/src/lib/stores/sync.svelte.ts::runTriggeredSync`).
- Empty states should make it clear when filters, not missing data, are hiding
  results.
- Workspaces applies the shared repository scope to local and fleet rows by full provider/host/path identity before its text search (`frontend/src/lib/components/terminal/WorkspaceListSidebar.svelte::visibleWorkspaces`).

## Threaded Comments

Threaded comment rendering must preserve both timeline recency and reply
context.

- In reverse-chronological timelines, a thread is positioned where its newest
  visible event would have appeared.
- Inside that thread, render the main/root comment first, then threaded replies
  underneath in reverse-chronological order: newest reply, then the reply before
  that, and so on.
- Do not flatten same-`thread_id` comments into separate top-level timeline
  items when the surrounding UI is meant to show comment conversations.
- This contract should also guide future diff-comment UI: inline diff threads
  can anchor to a file/line position, but their compact timeline summaries
  should still use root-comment context plus newest-first replies.

## Inline Review Drafts

Inline diff review draft comments are local staged review state until publish.
Direct detail-form review actions must leave that staged state untouched; do not
load or publish saved draft comments (`internal/server/huma_routes.go::requestChangesPR`).
Direct approve and request-changes share one submission contract end to end —
the same head pin, the same provider-side binding, the same post-success refresh
handling. Do not give either action stronger client-side verification, staleness
checks, or revocation behavior than the other
(`internal/server/huma_routes.go::approvalReviewHeadSHA`).
PR head mutations must not share an in-flight lock. Approve, request-changes,
merge, and suggestion application keep local submission guards; only durable
head-conflict state blocks the other actions (`frontend/src/lib/components/detail/PullDetail.svelte::headActionsBlocked`).
Once either provider mutation succeeds, close and clear its form before the
follow-up refresh. A refresh failure may show a warning, but must not leave the
successful mutation available for an accidental duplicate submission.
Editing a saved draft comment should change only the body and preserve the
original diff range, so the PATCH path must rebuild the range from the stored
comment rather than from whichever line is currently selected
(`frontend/src/lib/stores/diff-review-draft.svelte.ts::editComment`).

An open saved-draft comment editor is also pending local state, even before its
body differs from the saved text. Review-level publish and discard must stay
unavailable until every draft comment editor is saved or canceled; otherwise the
provider mutation can submit the old saved body while the UI still shows an
unsaved edit. Track that state in the draft-review store and have both tray and
inline editors clear it on save, cancel, and unmount
(`frontend/src/lib/stores/diff-review-draft.svelte.ts::hasPendingCommentEdits`,
`frontend/src/lib/components/diff/DiffReviewDraftTray.svelte::publish`,
`frontend/src/lib/components/diff/DiffReviewDraftInlineComment.svelte::reportEditState`).

Draft authoring and the sticky publish tray are gated by the repo operation
`review_draft`, not `submit_review`. `submit_review` gates submitted review
actions in the detail header, while Files-tab draft authoring must disappear
when `review_draft` is unavailable and show that operation's reason instead
(`frontend/src/lib/components/diff/DiffFilesLayout.svelte:44`,
`frontend/tests/e2e/inline-review.spec.ts:655`).

## Optional Metadata Controls

Optional metadata must not reserve empty rows or placeholders when absent. Put
compact edit controls beside the metadata's normal display location, and keep
empty states for places where missing data itself is useful information.

Async detail mutations must be scoped to the currently visible item. Compare the
full provider route identity before opening transient UI or applying mutation
responses, and discard stale responses instead of patching another item.

- Acknowledging a provider comment POST clears and unlocks its keyed draft; follow-up
  reconciliation failure is reported separately and must never offer to replay the POST
  (`frontend/src/lib/stores/detail.svelte.ts::submitComment`).
- Onboarding repository setup owns its initial sync through `triggerSyncEffect`: a rejected trigger returns the flow
  to a retryable repository step with the failure visible, while an accepted trigger advances only after the ordered
  sync command settles (`frontend/src/lib/components/onboarding/OnboardingFlow.svelte::startSync`).

## Testing Expectations

Behavior contracts should usually be tested where the user would notice the
breakage.

- Component tests for local state transitions, event propagation, and route/item
  identity helpers.
- Store tests for persistence scope and normalization logic.
- Playwright/e2e tests for navigation away/back, Escape behavior, nested button
  activation, and other multi-surface flows.
- For controlled native form controls, assert behavior under a real
  `fireEvent.click`, not only `fireEvent.mouseDown`. A mousedown-only test skips
  the native default action (e.g. a checkbox's own toggle) and will pass while a
  real click desyncs the control. A real-browser visual check catches this class
  of bug when the suite is green.
- Keyboard e2e tests should cover conflicting scopes, modal frame ownership,
  async action failure, overflow scroll-into-view, and mobile redirect cases
  when those behaviors are part of the feature.

Related docs:

- [`docs/adr/0003-provider-ready-first-run.md`](../docs/adr/0003-provider-ready-first-run.md)
  for first-run trigger, dismissal, identity, and completion policy.
- [`context/ui-design-system.md`](./ui-design-system.md) for visual primitives
  and styling guidance.
- [`context/notifications-in-activity.md`](./notifications-in-activity.md) for
  notification feed rows, state, and sync behavior.
- [`context/workspace-runtime-lifecycle.md`](./workspace-runtime-lifecycle.md)
  for runtime-specific workspace tab and shell behavior.
