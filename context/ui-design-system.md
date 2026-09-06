# UI Design System

Use this document as the intent-level guide for frontend UI work in `kenn-forge`. It should stay short, stable, and useful in model context.

## Purpose

- Keep the app visually coherent.
- Reuse shared primitives by default; one-off styling is a last resort.
- Extend semantic tokens and components instead of duplicating UI geometry.

## Design intent

`kenn-forge` is a dense maintainer tool, not a marketing surface.

- Layouts should feel compact, deliberate, and information-rich.
- Visual emphasis should come from hierarchy and semantic color, not oversized controls or decorative effects.
- Light and dark themes should express the same UI language through shared tokens.
- Detail background refresh uses the metadata-row `Syncing` indicator; a manual
  Activity refresh reports progress only in its initiating icon button. Do not
  add stale-data banners or other progress surfaces. Keep the manual control
  disabled while detail is loading, stale for the current route, or already
  syncing (`frontend/src/lib/components/detail/PullDetail.svelte::refreshDetail`,
  `frontend/src/lib/components/detail/IssueDetail.svelte::refreshDetail`).
- Inline conditional notices occupy layout only while active; do not reserve
  invisible rows for them (`frontend/src/lib/components/diff/DiffView.svelte`).

## Sources of truth

- Tokens: `@kenn-io/kit-ui/theme.css` and `@kenn-io/kit-ui/mermaid.css`
  (the `--mermaid-*`/`--viewer-scrim` tokens and diagram viewer chrome;
  both imported at the top of `frontend/src/app.css`) plus the
  kenn-forge-specific tokens `app.css` defines on top (chrome, budget,
  workflow status, review, verdict, diff, viewer glass controls)
- Shared primitives: `@kenn-io/kit-ui` first; app-specific compositions live
  in `frontend/src/lib/components/shared/`
- Diff/file-tree adapters: `frontend/src/lib/components/diff/PierreFileDiff.svelte`
  and `frontend/src/lib/components/diff/PierreFileTree.svelte`
- Routed item references and URL builders: `frontend/src/lib/routes.ts`
- SPA code and tests live in `frontend/src`; `packages/ui` is generated-schema output,
  not an `@kenn-forge/ui` application import surface (`frontend/src/main.ts`, `packages/ui/src/api/generated/schema.ts`).
- Svelte guidance: `skills/svelte-core-bestpractices/` (`svelte-core-bestpractices`) and `skills/svelte-code-writer/` (`svelte-code-writer`)
- Interaction contracts: `context/ui-interaction-contracts.md`
- Mobile UX principles: `context/mobile-ux.md`
- This guidance: `context/ui-design-system.md`

## kit-ui contract

kenn-forge consumes `@kenn-io/kit-ui` as source, pinned to one commit SHA in
`frontend/package.json` (never a `file:` path — bun's store keys by
name@version and goes stale). Its runtime deps are peers and its rune-module source cannot be
prebundled: keep it in vite `optimizeDeps.exclude` with transitive deps as
`"@kenn-io/kit-ui > <dep>"` includes. Bun links the pinned checkout from its
global cache outside the workspace root, so kit source that imports assets
(`?inline` SVGs) needs kit's resolved source root in vite `server.fs.allow`
(`frontend/vite.config.ts::kitUiSourceRoot`); a bump that adds such an import
otherwise fails only in the Vitest/Playwright transform tier, not in
`svelte-check`. See kit-ui's `docs/migration.md` and
`docs/theming.md`. Invariants kenn-forge relies on:

- Theme tokens come from kit `theme.css`; theming is `dark` /
  `high-contrast` classes on `<html>`. Frontend components additionally
  consume app tokens from `frontend/src/app.css` — style-asserting harnesses must load `app.css` like
  `browserAppHarness.ts`.
- Disabled styling: native controls that dim when disabled use
  `var(--opacity-disabled)` (kit's `hand-rolled-disabled-state` rule rejects a
  literal); a deliberate no-dim rule uses `opacity: unset`, and hover rules
  scoped to enabled buttons use `:enabled`, not `:not(:disabled)`.
- Type scale: rem tokens self-adjust on coarse pointers; `kit-type-touch`
  on `<html>` forces the touch scale. Never pin `html { font-size }`.
- Breakpoints are written in px (shared steps 640/760/900) — media-query
  `rem` resolves against the browser's 16px, not the app root.
- Spacing: `--space-1…8` = 2/4/6/8/12/16/24/32px. New or edited `gap`
  declarations use ladder tokens (both axes of shorthands); off-ladder px
  snaps to the nearest step, biased compact. On-ladder raw px in untouched
  code migrates opportunistically, not as churn.
- kit BEM classes are the sanctioned surface for parent `:global` styles
  and test selectors; a SHA bump that renames a class updates selectors in
  the same change.
- Chip: icons go in `children` (kit centers them), dropdown chevrons in
  `trailing`; no downstream `.kit-chip__label` overrides — repo chips
  depend on its ellipsis.
- Agent launch targets draw their icon as a kit `HarnessIcon` via `LaunchTargetName`
  (key → glyph by shared leading segments of the glyph id or its agent product
  names, then a bare prefix of at least four characters;
  `frontend/src/lib/components/terminal/agentHarness.ts::harnessForAgentKey`).
  The glyph only replaces the generic kind icon; the target's own label always stays.
- Theme resolution: kit's theme store owns dark/light/system resolution
  and persistence (`kenn-forge-theme` key); `theme.svelte.ts` adapts it. A
  host-forced mode applies classes directly and never persists via
  `setThemeMode`; an explicit user toggle persists even under a forced
  mode. Relative timestamps use kit `formatRelativeTime`;
  `parseAPITimestamp`/`localDate*Label` stay app-side.
- Dialogs: every dialog pushes a keyboard modal-stack frame. Background
  Escape surfaces cannot detect dialogs via `defaultPrevented` (kit's
  window listener registers late); they stand down when
  `getStackDepth() > 0`. kit `Modal`'s `closable` gates only the header X.
- Escape in overlay-hosted search: a non-empty kit `SearchInput` claims
  Escape to clear itself (stops propagation); an empty field lets it
  bubble so the hosting popover closes — every `SearchInput`-hosting
  popover must handle that bubbled Escape (`UserListEditor.test.ts` pins
  the flow).
- Palette, Cheatsheet, and the image lightbox keep hand-rolled focus
  traps and own their focus restore (the state stores' close functions;
  the lightbox's `restoreFocusTo`): close restores focus synchronously
  before the picked action runs, so an action's own focus move wins. kit
  `trapFocus` restores at unmount teardown, which would undo it.
- Focus signals: kit text fields show keyboard focus as the wrapper's
  accent border only; `app.css` drops the kit `TextInput` outline ring
  that would otherwise stack on the same wrapper (every tap or Tab into a
  text field is `:focus-visible`). Hover-only controls nested inside a
  list row button (`.star-btn`, `.import-btn`) carry `tabindex="-1"` so
  Tab moves row to row instead of stopping on an invisible target.
- Tab strips are one tab stop: only the selected tab has `tabindex="0"`,
  Left/Right/Home/End move focus and selection via
  `shared/tablist-keyboard.ts` (`TabbedPanelTree`, `MobileWorkspaceItem`,
  the `PullDetail` fallback strip), and Tab continues into the panel.
  In `TabbedPanelTree` a focused tab button reports its own tab (an
  arrow-key switch must not lag one render behind); any other focus in the
  leaf reports the active tab, so landing on a tab's "Hide" tool does not
  activate that pane.
- jsdom lacks `offsetParent` / `scrollIntoView` / `ResizeObserver`:
  `test/setup.ts` stubs the latter two, focus-trap tests install
  `stubOffsetParent.ts`, and synthetic Tab only exercises kit's trap at
  wrap boundaries.
- `CollapsibleSidebar`: kenn-forge relies on the `kit-sidebar-layout*` BEM
  classes, `data-collapsed`, `SplitResizeEvent`, and `SidebarToggle`
  modifiers; the narrow floating overlay is kit-owned via the `overlay`
  prop — no app-side copies of its CSS.
- `StatusBar`: relies on `kit-status-bar*` classes and
  `--status-bar-height`; `overflow="visible"` lets BudgetPopover use kit's
  popover recipe; the app owns keeping bar text short.
- `TopBar`: renders the app header as `.app-top-bar`; tabs collapse by
  measurement. The header must clip its x-axis (`overflow-x: clip` — kit's
  hidden probe row otherwise inflates scrollWidth) and side regions must
  stay content-sized (a flex-stretched region poisons the frozen
  `expandUsed` footprint and blocks re-expansion). Select tabs via
  `.kit-top-bar__tabs .kit-top-bar__tab`, never the bare class.
  Provider-mode repo selector visibility must not move the tab row; non-provider
  modes reserve its footprint unless embed config hides it
  (`frontend/src/lib/components/layout/AppHeader.svelte::reserveProviderRepoSelectorSlot`).
- `AdaptiveActionGrid`: the issue detail action row on every layout
  (`frontend/src/lib/components/detail/IssueDetail.svelte::issue-actions-grid`,
  adaptive layout, `frame="none"`, `padding={0}`) and the phone PR
  primary-action surface. Controls inside a grid never pass their own `size`:
  the grid item supplies the shared control height and type, and an explicit
  `sm` opts out and misaligns. Phone-like PR
  routes (`phonePresentation` threaded from
  `frontend/src/App.svelte::phoneDetailProps`) render approve, merge, close,
  reopen, ready, workflows, and the workspace action with kit's
  `layout="fill"`: each row is packed by the buttons' natural widths and then
  stretched to span the content width, rows are independent of each other,
  and no label is ever truncated. Gaps stay (`rowGap`/`columnGap` 2,
  `frame="none"`, `padding={0}`); `collapseBelow={0}` disables the
  disclosure. It reuses the same per-action snippets the desktop `FitStages`
  row composes, so each stateful control still renders exactly once. Kit
  fills only its own direct controls; compound wrappers (`approve-section`,
  `ready-section`, `workflow-approval-section`, `workspace-create-split`)
  are custom items and `PullDetail` fills their primary button itself. A
  wrapper around a kit control must not hardcode a width smaller than the
  phone hit target (`--focus-detail-hit-target`), or the control overflows
  its cell. Desktop-narrow focus presentation keeps `FitStages`.
- `WorkspaceCreateSplitButton` takes its control height from the kit `size` it
  is given (or the surrounding grid item), never a pinned `min-height`; the
  desktop PR `FitStages` row passes `sm` to match the buttons beside it
  (`frontend/src/lib/components/workspace/WorkspaceCreateSplitButton.svelte`).
- `FitStages`: how an action row degrades under pressure (richest labelled
  `Button` row to compact labelled or `IconButton` rows, then a measured menu
  trigger when needed), never a media query, Button-internal overrides, or a
  wrapping stage. Measurement probes must be stateless and hidden from the
  accessibility tree; stateful action controls render exactly once outside the
  probes so dialog drafts and pending state survive stage changes. Inside a `flex-wrap` parent
  the host needs `flex: 1 1 0` _and_ a `min-width` at the
  compact stage's intrinsic width: grow otherwise leaves the host narrower
  than that stage, and the icons paint over the sibling that should have
  wrapped instead. Every stage must expose the same accessible names
  (`frontend/src/lib/components/roborev/ReviewDrawer.svelte::.footer-actions-fit`,
  `frontend/src/lib/components/detail/PullDetail.svelte::measuredPrimaryActions`).
- Pull-request lifecycle decisions stay in the primary action row; workspace
  creation and workflow dispatch form a utility row, joining the measured
  `Actions` overflow only under pressure (`frontend/src/lib/components/detail/PullDetail.svelte::workflowActionsMenu`).
- Flash: one shared store (`frontend/src/lib/stores/flash.svelte.ts`); kit `FlashBanner`
  mounts once per shell in a page-level fixed layer below measured shell chrome
  and above modal backdrops, never inside feature containers; headerless shells
  use the viewport edge (`frontend/src/App.svelte:968`).
- Commit timeline rows keep type, author, SHA, and relative time together in the
  compact header; the SHA is metadata, not card action content
  (`frontend/src/lib/components/detail/EventTimeline.svelte`).
- `PullItem`/`IssueItem` sidebar rows have a Playwright-enforced uniform two-line
  height; any icon rendered inside the row (e.g. `CITokenCluster`'s compact
  `dotSize`) must fit the row's line-height or it silently grows that one row
  (`frontend/src/lib/components/shared/CITokenCluster.svelte`).

`kit-ui-check` gates at zero findings in both `make frontend-check` and the
Vite+ `frontend-check` task behind CI's `vp run -w check`. If a rule mistakes
application-owned UI for component debt, fix the rule rather than expanding
kit-ui or adding an ignore solely to silence the checker. New kit-ui behavior
requires an independently justified, reusable contract; checker cleanliness
alone is never justification.

## Shared primitives

### Chip

Use `Chip` and its semantic chip wrappers for compact status and metadata UI.

Intent:

- one shared geometry for small labeled UI
- consistent vertical alignment, spacing, casing, and density
- reusable across detail views, sidebars, and compact status surfaces
- semantic tone, dot, kind, and state semantics at the call site

Use it for:

- PR/issue state
- CI/review state
- repo and count badges
- other compact metadata markers

Use `Chip` directly when the caller already knows the semantic `tone`
(`success`, `warning`, `danger`, `info`, `merged`, `workspace`, `muted`,
or `neutral`) or needs the shared dotted status treatment. Use
`ItemKindChip` for PR/issue kind and `ItemStateChip` for PR/issue state
rather than repeating kind/state class maps in feature components.

Do not create new local `.badge`, `.pill`, `.tag`, or `.chip` geometry when
`Chip`, `ItemKindChip`, or `ItemStateChip` fits.

In this repo, the standard term is **chip**, not pill.

When a screen needs semantic chip color, extend `Chip` with a named tone class such as `chip--blue`, `chip--green`, or `chip--red` instead of redefining local badge geometry. Screens may keep legacy class names for test selectors during migration, but sizing, casing, and spacing should come from `Chip`.

### Tree Cells

Tree-like rows inside dense tables should preserve the table's scan line.
Keep IDs and primary row numbers in their normal column, and put disclosure
chevrons plus child indentation inside the content cell that owns the
hierarchy, such as a repo/ref or file-name cell. Do not use terminal/TUI
connector glyphs, branch-line borders, or extra ornamental strokes to draw the
tree. Indentation and a standard chevron are the affordance.

### ActionButton

Use `ActionButton` for repeated action styling.

Intent:

- one shared button model for tone, surface, and size
- semantic action styling instead of per-screen button CSS

If a new repeated button treatment is needed, extend `ActionButton` rather than creating another local button pattern.

### Comment composers

Every comment composer insets its submit button at the bottom-right _inside_ the input
and reserves that footprint as the input's bottom padding — never a button in a column
beside the field. Three surfaces repeat this shell today (`CommentBox.svelte`,
`IssueCommentBox.svelte`, `roborev/ResponseList.svelte`); extract it into a shared
component instead of adding a fourth copy.

### Modal primitives

- `Modal`
- `ConfirmDialog`
- `DialogButton`

### SidebarToggle

Use kit-ui's `SidebarToggle` directly for collapse and expand controls on left-side navigation rails.

Intent:

- one shared icon, size, hover, and accessible label contract for left-sidebar collapse affordances
- consistent expanded/collapsed direction across PR, issue, activity, and workspace sidebars
- avoid one-off SVG buttons or local `.sidebar-toggle` styling in each rail

Use it inside left sidebar headers and collapsed strips. Pass a specific label such as `Workspaces sidebar` when the generic `sidebar` label would be ambiguous. The resizable sidebar layout itself is kit-ui's `CollapsibleSidebar`; the container-width-driven floating overlay is requested through its `overlay` prop (hosts pass the container store's `isNarrow()` — kit's `overlayOnNarrow` media query is viewport-based, which is not the same signal).

### SidebarTitlePopover

Use `SidebarTitlePopover` on pull-request, issue, and workspace sidebar rows so the full
title and formatted repository remain readable; pull-request and workspace rows add the
full head branch on its own line. The required `truncationSelector` prop names the row
elements whose ellipsis truncation the popover reveals; it opens only while one of those
is actually truncated, measured at show time
(`frontend/src/lib/components/sidebar/SidebarTitlePopover.svelte`).

### GroupedSidebarSection

Use `GroupedSidebarSection` for collapsible groups in PR, issue, and workspace list rails. Keep group chrome and the `--sidebar-*` surface/row-state tokens shared; domain-specific row content stays with its owner. Wrap large always-visible vertical scroll panes (list rails, diff area, pull/issue detail, activity views) in `ScrollBox` for consistent flex sizing, native vertical scrolling, and a labelled focusable region; bind `viewport` when a host needs imperative scroll logic, and note the scrolling element is the viewport, not the host's content wrapper class. Give each scroll area a concise accessible label so keyboard users can identify and scroll the region. (`frontend/src/lib/components/shared/GroupedSidebarSection.svelte`, `ScrollBox` from `@kenn-io/kit-ui` — see kit-ui's `docs/components/scroll-box.md`, `frontend/src/app.css:39`)

### SplitResizeHandle and BottomDock

Use kit-ui `SplitResizeHandle` for horizontal and vertical pane dividers,
including handles inside application-owned recursive trees. The app retains
tree topology, ratio/size bounds, state, and persistence; the shared handle owns
pointer/keyboard interaction and separator semantics. Pass a specific label
such as `Resize Activity rail`.

Use kit-ui `BottomDock` for resizable inline bottom panels. The app owns whether
the dock is open plus its domain header/body/footer content; the shared dock
owns shell geometry, top-edge resizing, bounds, close control, and body
scrolling. It exposes no prop to hide its resize handle, so a mode that forces a
controlled `height` (for example a 100% expanded state) must hide the handle
itself via a scoped `:global(.kit-bottom-dock > .kit-split-resize-handle)` rule
under an app-owned class, or a live drag silently corrupts persisted height state
that isn't visibly changing. Keep the child combinator: dock bodies can host
content with its own kit split handles, and a descendant selector would disable
those nested handles too. Pin the computed `display` in a real browser rather
than asserting the class name, so a kit-ui SHA bump that renames or restructures
the handle out from under the override fails a test instead of only a manual
check. The inline workspace no longer uses this — it is a pane in the detail
tree — so no consumer currently needs the override.

### Styling shared components

Treat component props, documented CSS custom properties, and the shared root
class as the supported styling contract. Prefer wrapping a component with an
application layout element or setting a public custom property over reaching
into child markup.

An inner selector such as `.kit-typeahead__trigger` or
`.kit-checkbox__label` is allowed only when the installed component has no
public hook for a required application layout, the dependency is pinned, and
the affected computed layout or interaction has browser coverage. Keep such
selectors scoped below an application-owned class; never use an unscoped
global override. Re-audit these selectors whenever the kit-ui revision moves.

### TabbedPanelTree

Use `TabbedPanelTree` for VS Code-like panel workspaces: tab groups that can
reorder tabs, drag tabs into another group, split a group horizontally or
vertically, and resize split panes.

Intent:

- one shared interaction model for draggable, tabbed, splittable panel groups
- let callers provide arbitrary panel content, tab icons, and tab action buttons
- keep dedicated sidebar resizing on `SplitResizeHandle` instead of forcing
  every two-pane layout into a tabbed workspace model

Use it when a surface needs multiple interchangeable panels or future panes
inside a draggable workspace. Do not use it for simple fixed sidebars,
single-purpose drawers, or file-tree/content splits where `SplitResizeHandle`
or a narrower layout primitive is enough.

PR, issue, and activity detail panes (conversation, diff, inline workspace) are
an intended home for this primitive, not an exception to the line above: they
are rearrangeable panes, so do not hand-roll another splitter for them. List
rails stay on `CollapsibleSidebar`/`SplitResizeHandle`. The saved arrangement,
rendered availability, history, and terminal-hosting contracts live in
[`ui-interaction-contracts.md`](./ui-interaction-contracts.md).

Use neutral `tabbed-panel-*` DOM classes/selectors for tests and consumers.
Do not add workflow-specific aliases or compatibility selectors when moving
this primitive into new surfaces.

Pass the mutation callbacks that match the interactions you expose:
`onMoveTabBefore`/`onAppendTabToLeaf` for tab sorting and cross-group moves,
`onSplitTab` for edge drops, and `onRatioChange` for divider resizing. Omitted
callbacks make that interaction read-only instead of rendering a visual drop
target that cannot apply.

A leaf renders every one of its tabs, showing only the active one, so the
`visible` flag passed to `renderTab` is the caller's only signal. Gate expensive
or side-effecting pane bodies (a diff fetch, the workspace portal slot) on it:
unconditional bodies mount for every selected item, and a portal slot that
lingers behind another tab stays the registered host and strands its content off
screen.

A pane body is stretched by its panel, which is a flex container: a pane body
must not size itself from its own content. Detail views end their chain at a
`ScrollBox` that expects a height-constrained flex parent, so a block panel
turns their internal scrolling into outer overflow with no visible error
(`frontend/src/lib/components/shared/TabbedPanelTree.svelte::.tabbed-panel-tab-panel`).

Zoom is transient focus state, not part of the saved arrangement. Drop it
whenever what was zoomed stops rendering (`DetailPaneLayout`'s reconciliation
effect covers availability, which the store cannot see) and on any successful
split, which mints a leaf the older zoom would hide.

The current accessibility scope is labeled tab groups, focusable tabs and tab
actions, and labeled pointer resize handles. Keyboard tab reordering, keyboard
splitting, and keyboard resizing are not implemented here; extend
`TabbedPanelTree` first if a consumer needs those interactions.

### SelectDropdown

Use `SelectDropdown` for single-value selection controls in the UI.

Intent:

- one custom dropdown visual language matching header controls
- avoid mixing browser-native select styling with custom app dropdowns
- keep selection affordances consistent across detail headers, filters, and compact command surfaces

Do not add native `<select>` controls for visible app UI; use `SelectDropdown` instead. This is enforced by `frontend/src/no-native-select.test.ts`, which scans the component source trees and fails when a native `<select>` element is reintroduced. There is no allowlist or per-component exemption: if `SelectDropdown` cannot express a case, extend the primitive rather than reaching for a native `<select>`.

### Repository selector

The shared repository selector presents `Global` first and named presets above the repository tree. Keep its search and preset regions fixed, make only the repository tree scroll, and keep the save footer visible at every scroll position (`frontend/src/lib/components/RepoTypeahead.svelte`).

Custom preset rows place their destructive action at the far right. Saving opens the create-or-overwrite dialog; preset names have no separate rename interaction (`frontend/src/lib/components/RepoPresetSaveDialog.svelte`).

### KataLinksPanel

Use `KataLinksPanel` for read-only linked Kata context in provider-item and
workspace detail panes. The panel owns Forge association controls, daemon and
provenance disambiguation, freshness, and workspace actions; issue-detail
projection and presentation come directly from the pinned `@kenn-io/kata-ui`
source component. Keep the Forge link list and unlink controls outside the
package error boundary so a Kata rendering failure cannot hide association
recovery actions.

### FilterDropdown menu rows

A binary view mode in a `FilterDropdown` menu is one toggle row named for the
non-default state ("Strict date order", "Hide bot activity"), checked when
active. Do not add a row for the default state, an exclusive radio pair, or an
opaque single-word label for default behavior.

### Overlays

Use shared overlay primitives for dropdowns, popovers, menus, tooltips, and similar floating controls.

Intent:

- overlays should float above panes, sidebars, drawers, resize handles, and scroll containers
- overflow-constrained parents must not clip menus or hide available choices
- repeated positioning, collision, z-index, and outside-click behavior belongs in the shared primitive, not local screen CSS

Before placing an overlay inside a split view, compact sidebar, drawer, or scrollable region, verify that it can extend past its trigger container without being cut off. An overlay opened from a modal must escape the modal's scrollable body while remaining inside the modal panel so the modal focus trap still owns it (`packages/ui/src/components/workspace/WorkspaceCreateSplitButton.svelte::portalMenu`).

Popover surface chrome (background, border, radius, shadow) comes from `kit-popover-card`; do not re-declare it in component-scoped styles. Scoped rules outrank the kit class, and a `var()` referencing an undefined token (there is no `--bg-elevated`) computes to transparent with no build-time error.

A popover that lowers its own min-content width (`overflow-wrap: anywhere`, so an unbreakable branch name cannot stretch `WorkspacePaneControls` past its max-width) leaks that to every surface nested inside it, where flex rows then shrink buttons below their labels and break them mid-word. Reset `overflow-wrap`/`word-break` at the nested popover's root instead of hardening each child (`frontend/src/lib/components/terminal/TerminalOptionsMenu.svelte`).

### GitHubLabels

Use `GitHubLabels` for actual GitHub labels.

Intent:

- keep repository labels distinct from generic status chips
- preserve GitHub-label semantics without collapsing them into a generic badge system

### Pierre Diff And File Tree

Use the local Pierre wrappers for changed-file UI:

- `PierreFileDiff.svelte` wraps `@pierre/diffs`
- `PierreFileTree.svelte` wraps `@pierre/trees`

Intent:

- keep Pierre lifecycle, Shadow DOM styling, theme selection, and selection/context
  behavior in one place
- let consumers pass app-level data such as `DiffFile[]`, selected path,
  word-wrap state, and demand-loaded file text callbacks
- avoid reimplementing direct `FileDiff` or `FileTree` setup in each files view

Reference the upstream docs before changing wrapper options or behavior:

- `@pierre/diffs`: <https://diffs.com/>
- `@pierre/trees`: <https://trees.software/>

Do not import `@pierre/diffs` or `@pierre/trees` directly in feature
components unless the existing wrappers cannot express the use case. Prefer
extending the wrappers with a small app-level prop over copying Pierre setup,
theme overrides, or Shadow DOM CSS into another component.

## Tokens and semantics

Use semantic variables instead of hard-coded values whenever possible.

- Surfaces and borders come from the app token set in `frontend/src/app.css`
- Text uses the shared primary / secondary / muted hierarchy
- Accent colors carry meaning, not decoration

Default color intent:

- green: success, open, ready
- amber: pending, draft, warning
- purple: merged, waiting, workflow-secondary status
- red: failure, conflict, destructive status
- blue: focus, active controls, informational emphasis
- teal: workspace/worktree-linked state

## Serialized payload fields

Never render a serialized payload string (JSON blob, opaque struct dump) as UI text.
Parse it in a named util and show labeled human-readable stats, with the unabbreviated
values in a `title`. Abbreviate the hover values by significant digits, not fixed
decimals — a fixed scale rounds small magnitudes to a meaningless zero
(`frontend/src/lib/utils/roborev-usage.ts` for roborev `token_usage`).

## Implementation guidance

When editing Svelte components, use the Svelte skills `skills/svelte-core-bestpractices/` (`svelte-core-bestpractices`) and `skills/svelte-code-writer/` (`svelte-code-writer`) alongside this document.

Effect-owned frontend work shares the single main `ManagedRuntime` and reaches it through Svelte context; do not create per-feature runtimes or detach async work from its scope (`frontend/src/lib/app/runtime.ts::makeAppRuntime`, `frontend/src/lib/app/mount.ts::mountApplication`).

Promise-required library callbacks may observe `AppExecution.exit`, but the command must start through `runCommand` so interoperability never becomes a second runtime or detached owner (`frontend/src/lib/app/runtime.ts::AppExecution`).

When an `$effect` launches an Effect fiber, wrap `runCommand` itself in `untrack`; fibers begin synchronously, so untracking only program construction can subscribe the outer Svelte effect to the fiber's rune transitions (`frontend/src/App.svelte:542`).

App-wide health polling belongs to the root runtime lifetime, not the full-shell lifetime, because embedded routes still depend on daemon availability (`frontend/src/App.svelte::roborevPollingExecution`).

Provider list, activity, and sync controllers expose synchronous launchers; their Effect workflows own cancellation, shared demand, bounded reads, and sequential cadence so Svelte callers never rebuild Promise generations or timer overlap guards (`frontend/src/lib/stores/`).

Diff context prefetch is app-scoped, foreground-prioritized, and concurrency-bounded; generation cancellation must not release an active slot until its shared read settles (`frontend/src/lib/components/diff/diff-context-prefetch.ts::DiffContextPrefetch`).

Detail and diff selection reads are latest-wins; identical full-provider keys share only active reads, while selection changes interrupt the underlying transport (`frontend/src/lib/effect/latest-shared-read.ts::makeLatestSharedRead`).

The standalone GitHub App setup entrypoint owns one scoped Effect program rather than another managed SPA runtime; its Svelte component only projects callbacks and publishes the synchronous Continue command (`packages/github-app-ui/src/main.ts`, `packages/github-app-ui/src/setup-program.ts::makeSetupController`).

Effect root lifetimes survive browser back-forward cache restores: the main SPA stays alive for the JavaScript realm, and standalone entrypoints ignore persisted page hides when deciding teardown (`frontend/src/main.ts`, `packages/github-app-ui/src/main.ts`).

Effect callback streams use explicit bounded buffers: pullable producers suspend for backpressure, while callback-only sources fail with a typed transient overflow so reconnect/replay can recover (`frontend/src/lib/browser/streaming-fetch.ts::byteStreamFromReader`, `frontend/src/lib/browser/event-source.ts::eventSourceStream`).

Provider live updates checkpoint only after their handler succeeds and preserve that cursor across owner handoffs; transient failures stay visibly reconnecting under capped backoff, while decode failures stop visibly until an explicit reconnect (`frontend/src/lib/stores/provider-events-workflow.ts::providerEventsProgram`).

Provider mutations share one app-scoped acknowledged queue: order commands by submission, key rollback versions by full item identity plus mutation family, never retry writes, and turn `stale_state` into refresh-and-review without replay (`frontend/src/lib/stores/ordered-mutations.ts::ProviderMutations`).

Component lifetime owns polling and live-event subscriptions; teardown interrupts that owner. Accepted queued mutations remain application-runtime-owned, and post-teardown hub demand stays pending for a remounted owner (`frontend/src/lib/features/kata/kata-workflow.ts::KataWorkflow`, `frontend/src/lib/components/terminal/workspace-list-workflow.ts::WorkspaceListWorkflow`).

A `$state` record written by full-object reassignment (`x = { ...x, k: v }`) that is also read inside the same reactive scope — an `$effect`, or a `{@attach ...}` callback, which Svelte runs as one — is a self-referential dependency: Svelte detects it as `effect_update_depth_exceeded` and the attachment tears itself down and reattaches forever. Mutate the specific key instead (`x[k] = v`) (`frontend/src/lib/stores/workspace-host.svelte.ts::registerSlotElement`).

For TypeScript/Svelte state and routing contracts, avoid anonymous object type literals when the shape represents a domain concept that is reused or exposed across modules. Name shared item identity shapes, route payloads, embed callbacks, and API view models near the module that owns the concept, then import those types at call sites. PR/issue/file/focus route identity and URL construction belongs in the shared route item module at `frontend/src/lib/routes.ts`; the frontend router remains the browser-location adapter over those builders. New routed item callers should use those named refs and builders instead of repeating `{ owner; name; number; platformHost }` shapes or hand-building `/pulls`, `/issues`, or `/focus` URLs.

When TypeScript complains, prefer making the owning type more precise over adding call-site assertions. Generated OpenAPI types, named domain unions, and shared option arrays should carry their real values so components can consume them directly. Good cleanups look like `handleCommandResult(result: void | Promise<void>, ...)` or a typed dropdown option returning `TimeRange`; they remove runtime probing and casts by tightening the contract. Bad cleanups add `as unknown as`, broad `as any`, defensive `instanceof` branches, or response-normalization functions around data that is already typed by the API schema.

Use assertions only at real boundaries: DOM event targets, `JSON.parse`, third-party libraries with incomplete types, test fixtures, and browser globals. Keep those assertions local and obvious. Do not turn a simple input handler into a defensive branch when the markup already owns the element type; likewise, do not add runtime validation around a generated API response unless the schema is wrong. If the schema is wrong, fix the Go/Huma/OpenAPI source and regenerate clients.

For repeated async or event patterns, prefer a small typed helper over repeated structural checks. Never check promise shape with `typeof result.then === "function"`, `then?: unknown`, or similar maybe-thenable probes. If the value may be async, make the contract `void | Promise<void>` and use the promise methods through that type; if the value is a browser API promise such as `document.fonts.ready`, use the typed API directly. Do not duplicate browser API feature checks across components if a shared helper can express the actual browser boundary.

Shared markdown rendering has an async highlighted path and plain synchronous fallbacks. Use `renderMarkdown` for normal rendered descriptions and comments so fenced code blocks can be highlighted by Shiki with the declared fence language. Keep `renderMarkdownSync` and `renderMarkdownBlocks` independent of highlighter state; they intentionally render plain code fences for pending UI and rich-preview slicing. Shiki work is bounded per render; once the fence, language, or code-size budget is exceeded, additional fences render as escaped plain code. Shiki inline styles are trusted only when generated by the renderer during the current sanitization pass. Raw markdown HTML, even if it uses Shiki class names, must not retain style attributes.

Rich Markdown diff preview parses each reconstructed hunk side as one document.
Review cards anchor only at confidently mapped valid container boundaries;
hidden-gap or uncertain threads stay visible in fallback rather than using a
guessed inline position
(`frontend/src/lib/utils/markdown-rich-preview.ts::buildMarkdownRichPreview`).

The markdown pipelines deliberately stay app-side rather than moving to kit-ui `createMarkdownRenderer`: interactive task lists and docs link/image rewriting need marked renderer overrides, docs external-image blocking needs an element-level DOMPurify hook, and the drag handle needs the non-data `draggable` attribute — all beyond kit's extensions/codeFence/data-\* hook surface. This applies to the two renderers (`frontend/src/lib/utils/markdown.ts` and the docs renderer) plus the markdown DOM-diff surface (`markdown-diff.ts`), which diffs already-rendered HTML and owns no render or escaping invariants of its own. The fence primitives that do fit (`escapeHtml`, `codeFenceLanguage`, `codeHighlightPlan` and its budgets, `shikiStyleIsAllowed`) are imported from `@kenn-io/kit-ui/utils/markdown` in both renderers so highlight budgets and escaping stay in parity by construction; do not reintroduce local copies. Mermaid is fully kit-owned: both renderers route fences through `mermaidCodeFence`, and `frontend/src/main.ts` wires kit's `initMarkdownMermaidRendering` (from `@kenn-io/kit-ui/utils/markdown-mermaid`, viewer classes `kit-mermaid-*`) into the app modal stack via `onLightboxOpen`; the diff image panel is kit's `ImagePreview`. New deps reached through the excluded kit-ui source barrel (mermaid, new lucide icons) must be added to `optimizeDeps.include` in `frontend/vite.config.ts`, or the cold optimizer re-bundles mid-run and breaks the browser test tier. `escapeHtml`'s double-quote escaping is a load-bearing contract for double-quoted attribute interpolation in both renderers, pinned by the docs suite's attribute-escaping tests. The invariants the boundary protects — task index stability, style stripping, external-image blocking, mermaid bypass, highlight budgets — are covered by `frontend/src/lib/utils/markdown.test.ts`, `frontend/tests/e2e-full/description-task-list.spec.ts`, and the docs markdown suite.

Responsive layout work should separate presentation mode from sizing mode.

- Use compact/focus presentation to remove sidebars, split panes, or dense chrome when the available width is too small.
- Use phone/mobile sizing only for phone-like contexts, such as coarse pointer, mobile user agent, or explicit force-mobile test paths.
- Do not use one broad "phone viewport" predicate for both decisions. That makes desktop-narrow windows inherit oversized mobile typography, action grids, and touch-only geometry.
- When a compact canonical route reuses focus presentation, keep desktop-scale tokens unless the environment is phone-like.
- Shared breakpoints are defaults, not overflow overrides: max-content toolbars
  must stack at the first width that contains their controls, with the boundary
  pinned by browser geometry (`frontend/src/lib/components/repositories/RepoSummaryPage.svelte::.repo-page__toolbar`).

Before adding UI styling:

1. Check whether an existing shared primitive already expresses the pattern.
2. If yes, extend that primitive with a semantic variant rather than duplicating layout CSS.
3. If no, add a shared component only when the pattern is clearly reusable.

Local CSS is acceptable for context-specific color or placement. Local CSS should not re-define repeated geometry that belongs in a shared primitive.

## When to add a shared component

Add or promote a shared component when:

- the same UI geometry appears in multiple places
- the same semantic control exists in both list and detail surfaces
- future work would otherwise copy and paste the same styling

Do not create a shared primitive for a one-off visual detail.

## Maintenance rule

If you add a new shared UI component, or materially change the intent of an existing one, you must update `context/ui-design-system.md` in the same turn.

The document should describe:

- what the component is for
- when to use it
- what UI duplication it is meant to prevent

It should not turn into implementation notes or a style dump.

## Testing expectation

When UI work changes shared primitives or visible interaction patterns, add or update regression coverage, preferably at the user-visible flow where the duplication or inconsistency previously appeared.
