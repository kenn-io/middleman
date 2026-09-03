# Mobile UX Principles

Use this document as the intent-level guide for mobile UI work in `kenn-forge`. Read it before designing, implementing, or reviewing anything under phone routes, narrow viewports, touch-focused layouts, or mobile-specific CSS.

## Core stance

Mobile is not the desktop app squeezed into a smaller viewport. It is a separate phone-first workflow for maintainers who need to triage, inspect, and act while holding a phone.

`kenn-forge` can stay dense and information-rich, but phone density must come from hierarchy and summarization, not from tiny desktop controls, compressed split panes, or table layouts.

Kata and Docs remain desktop-first modes. Do not add `/m` routes or phone-only
sizing until each mode has a deliberately designed phone workflow.

## Product model

Think about mobile work in this order:

1. **What is the maintainer trying to do on a phone?**
   - Scan what changed.
   - Triage what needs attention.
   - Open the right PR or issue quickly.
   - Read enough context to decide whether to defer to desktop.
   - Perform lightweight actions only when they are safe and obvious.

2. **What can be hidden, grouped, or deferred?**
   - Prefer summary cards, grouped events, focused detail routes, and progressive disclosure.
   - Do not expose every desktop control just because it exists.
   - Avoid sidebars, split panes, dense tables, drawer stacks, and multi-row toolbars on phone routes unless they are deliberately reimagined for touch.

3. **What should the thumb hit first?**
   - Primary actions need clear labels and comfortable hit targets.
   - Secondary filters should be compact but still readable.
   - Binary states can be toggles; mutually-exclusive choices usually belong in compact labeled dropdowns/selects rather than repeated chip rows.

## Design rules

- Build dedicated phone routes/components when the desktop interaction model does not fit. A `/m` route must not simply mount the desktop view inside a narrow wrapper.
- The phone top bar may expose the same direct Forge selector as desktop. Keep
  it within the phone viewport, retain the product name when only one Forge is
  available, and navigate with ordinary origin links so other tabs are not
  retargeted (`frontend/src/lib/components/layout/ForgeSelector.svelte`).
- Preserve human-facing product copy. Remove text that sounds like an implementation note or model instruction.
- Keep repository/provider identity visible enough to disambiguate similarly named repos, especially on activity cards and detail headers.
- Give focused PR/issue detail pages their own phone shell treatment even when they reuse desktop detail components internally. Phone-like focus presentation, whether reached from `/m` lists, the activity feed, `/focus/...`, or a narrow canonical URL, renders inside the same `.mobile-shell` top bar as every other phone view, and detail routes add a header with the item badge and a Back control. Desktop-narrow focus presentation stays chrome-free (`frontend/src/App.svelte::phoneDetailItem`).
- The phone PR header status row is one control family: state, CI, diff stat, review decision, labels, assignees, and reviewers all render at the action-grid height with the same corner radius, one size, one weight, and plain case. Tone tints still carry status; pills and uppercase labels are desktop density devices. Branch names keep the app mono face at the inline-code ratio so the mono line reads the same size as the sans repository line (`frontend/src/App.svelte` phone `.chips-row` rules).
- Phone-like PR and issue detail branches in `App.svelte` all receive the phone workspace callbacks (`phoneDetailProps` / `phoneIssueDetailProps`), and both detail components open a freshly created workspace through `onOpenWorkspace` when the host provides it; the desktop `/terminal` route is only the fallback for hosts without a callback (`frontend/src/lib/components/detail/IssueDetail.svelte`, `frontend/src/lib/components/detail/PullDetail.svelte`).
- Back on a phone detail returns to the list that opened it, at the same rows. Phone lists record their origin in history state when they open an item (through the navigate callback's `state` option, never as a bare second argument) and park their scroll offset and loaded chunk size before every path that leaves the list, timeline events included. Parking stamps the list's own history entry, so only a remount on that entry (header Back or browser Back) restores the offset; a fresh visit from the mode picker or a deep link discards it and starts at the top. A deep link with no origin falls back to the matching `/m` list. A detail that navigates again (a tab switch, a stack member) carries the origin onto the new entry with a back depth, so the header Back pops straight to the list while browser back still steps through the tabs (`frontend/src/lib/stores/mobile-list-return.ts::carryMobileListOrigin`, `frontend/src/lib/views/FocusListView.svelte::parkListPosition`, `frontend/src/App.svelte::leavePhoneDetail`).
- Mobile escape hatches to desktop views are allowed, but they must be intentional and not the default path.

## Responsive route and presentation model

Viewport size chooses presentation, not route identity.

- Canonical PR and issue routes such as `/pulls/...`, `/pulls/.../files`, and `/issues/...` must not be rewritten just because the viewport becomes narrow. A user who resizes a desktop detail page down and back up should stay on the same URL and naturally return to the desktop presentation when there is room again.
- Narrow canonical PR/issue routes may reuse the same focus presentation used by `/focus`, but they must keep canonical route builders for list selection, detail-tab changes, and back/forward behavior.
- `/focus` remains a valid explicit route family, but it should be a route over shared focus presentation components, not a separate implementation that canonical routes cannot enter or leave responsively.
- `/m` remains a phone-first route family. Use it for workflows that need a mobile shell, not as an automatic replacement for every narrow canonical route.
- Keep host-prefixed identity stable too. `/host/{platform_host}/pulls/...` and `/host/{platform_host}/issues/...` should not normalize back to default-host or `/focus` URLs during responsive presentation changes.

Do not collapse these concepts:

- **Compact/narrow presentation**: a desktop window, split pane, or embedded surface that is too narrow for sidebars or dense desktop chrome. It can use focus presentation, but it should retain desktop-scale typography and desktop action geometry.
- **Phone-like presentation**: a touch/mobile-user-agent context where larger mobile tokens, hit targets, and phone-specific action layouts are appropriate.

A phone stays phone-like in landscape: a coarse-pointer, mobile-user-agent device keeps phone presentation up to the handheld landscape bound, while wider or single-signal devices stay desktop-narrow (`frontend/src/lib/utils/phone-presentation.ts::isPhoneLikeViewport`).

In code and tests, name predicates so this distinction is visible. Avoid generic helpers such as `isPhoneViewport()` when the real question is either "should this route use the compact focus presentation?" or "should this surface use phone-only sizing?"

## Typography and sizing

- `frontend/src/app.css` owns the shared design tokens, including `--font-size-mobile-*`.
- Mobile typography, spacing, radii, and hit targets should be mostly `rem`-based and expressed through scoped tokens.
- The app intentionally keeps the global root font size small for desktop/terminal stability. Do not change the global `html` root just to make mobile readable.
- Compensate inside mobile shells with mobile-scoped tokens such as `--mobile-type-*`, pointing back to the app-level mobile font-size tokens where possible.
- Do not apply mobile-scoped tokens to every narrow desktop viewport. A desktop-narrow focus presentation should remain compact and desktop-scaled unless the environment is phone-like.
- Avoid raw `px` as the sizing model for mobile typography, spacing, or touch targets. Hairline borders and tiny decorative strokes are the main exceptions.
- Avoid device-DPI-specific scaling unless there is a proven, user-requested reason; it fights browser/user text scaling and makes the UI less predictable.

## Interaction patterns that usually fit phones

Prefer:

- Card lists over tables.
- Single-column flows over split panes.
- Focused detail routes over desktop drawers.
- Sticky or clearly placed primary actions over toolbar clusters.
- Compact labeled dropdowns/selects for mutually-exclusive filters.
- Horizontal chip scrollers only when the chips are truly glanceable and do not dominate vertical space.
- Progressive disclosure for metadata, timelines, and secondary actions.

Avoid by default:

- Desktop tables in phone wrappers.
- Nested sidebars or trailing panes.
- Multi-row chip/filter chrome that pushes content below the fold.
- Tiny icon-only actions without accessible names and visible context.
- Routing mobile taps into desktop drawer/query state with no visible phone result.

- Mobile Activity starts with 30 collapsed parents and autoloads one 30-parent chunk per
  end-of-list scroll gesture. The response limit counts distinct parents after event
  matching, while event scans remain separately bounded (`internal/server/huma_routes.go::Server.listActivityRouteCore`).
- Phone PR and issue lists start with 30 items and autoload one 30-item chunk per
  end-of-list scroll gesture. Mutation reloads retain the active chunk size
  (`frontend/src/lib/stores/pulls.svelte.ts::createPullsStore`, `frontend/src/lib/stores/issues.svelte.ts::createIssuesStore`).
- Phone startup and repository changes defer list loading to the current phone view
  (`frontend/src/App.svelte::shouldDeferInitialListsToActiveView`).
- Config and provider events refresh only the current list
  (`frontend/src/lib/app-stores.svelte.ts::refreshVisibleData`).
- Phone Activity, PR, and issue filters use the shared repository selector for Global,
  saved presets, and custom selections; preset management remains outside the phone
  workflow (`frontend/src/lib/components/RepoTypeahead.svelte`).
- Phone Activity, PR, and issue lists share one full-width search/filter trigger; each
  view owns its filter content. Repository selection comes first, hierarchical selectors
  use divider rows, and booleans use unframed kit switches (`frontend/src/lib/components/mobile/MobileTriageSearchBar.svelte`).

## Routing expectations

- Phone list/start routes should route to phone-appropriate focused detail routes when the user is already in a phone route family.
- Canonical list/detail routes should stay canonical when they are only changing presentation for a narrow viewport. Do not route desktop-narrow list clicks into `/focus` unless the current route family is already `/focus`.
- Focused detail tabs, such as PR files, must use route builders for the active route family: focus builders for `/focus/...`, canonical builders for `/pulls/...` and `/issues/...`.
- Automatic responsive redirects should be rare. Prefer presentation selection over URL replacement for canonical routes; when redirects are truly needed, preserve user intent for deep links and do not bounce focused/detail routes back to a landing page.
- Desktop opt-out links are acceptable, but they should be explicit and test-covered.

## Mobile workspace workflow

- Workspaces is a first-class phone mode selected from the shared mobile mode picker; `/m/workspaces` uses a dedicated card list and never mounts the desktop workspace layout.
- Keep search and workspace creation inline. Put the existing sort, grouping, organization-name, and diff-stat controls in a touch-sized View sheet backed by the same persisted settings as desktop.
- Phone workspace rows show hook-reported Working, Approval, Input, and Done states as visible compact badges; color-only status dots are insufficient for agent state (`frontend/src/lib/components/mobile/MobileWorkspaceList.svelte::agentStatePresentation`).
- Phone PR detail must not reflow while a background sync runs: the inline "Syncing" meta item wraps to its own row on a phone and pushes the page down, so phone presentation renders a 2px absolutely positioned progress bar at the top of `.pull-detail` instead (`role="status"`, reduced-motion safe). Desktop keeps the inline indicator.
- Phone PR detail hides the kanban/review-status `SelectDropdown` (header and below-chips instances); its purpose is not self-evident on a phone and it competes with the primary actions. Desktop-narrow presentation keeps it (`frontend/src/lib/components/detail/PullDetail.svelte`, gated on `phonePresentation`).
- Phone detail headers turn the title actions (edit, star, provider link) into bordered, equal hit-target squares; the inline `#123` copy button stays text-sized with only the WCAG 24px target floor and must not receive the 49px hit-target minimums, or the meta row grows to button height (`frontend/tests/e2e-full/mobile-routes.spec.ts::expectReadableDetail` pins the floor) (`frontend/src/App.svelte::.focus-layout--phone`).
- Phone-like PR detail routes have no inline workspace controller; creating or opening a workspace from them must land in the `/m/workspaces` shell, never the desktop `/terminal/{id}` route. The same phone signal renders the PR primary actions as one kit `AdaptiveActionGrid` in `layout="fill"` (rows packed by natural width and stretched edge to edge, labels never truncated) instead of the desktop fit stages and actions menu. Desktop-narrow focus presentation keeps desktop destinations and layout (`frontend/src/App.svelte::phoneDetailProps`, `tests/e2e/mobile-pr-actions.spec.ts`).
- Opening a workspace shows exactly one terminal. Repository, branch, and Fleet host identity get a full-width context row above the controls; the switcher selects base, agent, or shell sessions without destroying background sessions (`frontend/src/lib/components/mobile/MobileWorkspaceTerminal.svelte`).
- Phone terminals keep direct xterm interaction available for hardware keyboards and terminal-native controls, but touching xterm must not summon the software keyboard. The optional auto-growing composer owns software-keyboard input, stays above the keyboard, routes text through xterm's sanitized paste path, and exposes Escape, Tab, arrow, Enter, and Space keys through xterm's mode-aware keyboard path. Each special-key tap claims the visible pane's resize authority before sending input (`frontend/src/lib/components/mobile/MobileWorkspaceTerminal.svelte::sendComposedInput`, `frontend/src/lib/components/mobile/MobileWorkspaceTerminal.svelte::sendSpecialKey`, `frontend/src/lib/components/terminal/XtermTerminalPane.svelte::sendKey`).
- Keep xterm's built-in renderer on Android and Firefox; Android WebGL can accept terminal output while presenting a blank surface (`frontend/src/lib/components/terminal/XtermTerminalPane.svelte::shouldUseBuiltinRenderer`).
- Remember the selected session per workspace. A ready local workspace always offers its base terminal, even while runtime-session discovery is pending or unavailable; Fleet exposes only runtime-reported sessions. When a runtime session exits, fall back to another live session or the local base terminal (`frontend/src/lib/components/mobile/MobileWorkspaceTerminal.svelte`).
- An accepted queued launch reconciles the exact workspace, Fleet host, session key, and acceptance time for 15 seconds. Transient reads retry; the matching session completes the claim, while only that exact claim may expire and report a danger flash (`frontend/src/lib/components/terminal/workspace-runtime-workflow.ts::reconcileAcceptedLaunch`).
- A local linked item opened from the workspace list returns to the list without mounting terminal runtime work; one opened from a terminal keeps that exact workspace and selected session mounted. Direct item tabs replace their direct-origin history entry before Back falls through to the terminal. Fleet linked-item numbers stay passive until detail routing can retain Fleet host identity (`frontend/src/App.svelte::leaveMobileWorkspaceItem`).
- The phone shell has one linked-item slot per workspace. A visible associated PR wins over the issue or ad-hoc branch the workspace was created from, so the list badge, terminal pill, item route, and provider link all name the same PR (`frontend/src/lib/components/mobile/mobile-workspace-detail.ts::mobileWorkspaceLinkedItem`, `frontend/src/lib/components/mobile/mobile-workspace-list.ts::mobileWorkspaceLinkedItem`).
- Keep launch and Stop out of the phone terminal header. Its touch-sized ellipsis opens a bottom Terminal options tray backed by the existing persisted terminal settings; New terminal opens the launcher, while Stop remains immediately discoverable in the tray and requires explicit confirmation before the runtime mutation begins (`frontend/src/lib/components/mobile/MobileWorkspaceTerminal.svelte::stopSelectedSession`).
- Routes retain local or Fleet identity. Only authoritative deletion events invalidate shared workspace state. A `workspaceNotFound` response replaces matching phone routes with the list without publishing deletion or invalidating shared state. Unavailable Fleet hosts and generic connection failures remain in context with Retry or reconnect affordances (`frontend/src/lib/stores/workspace-host.svelte.ts::notifyWorkspaceDeleted`).
- Mobile workspace screens may share data, persistence, runtime, and focused-item primitives with desktop, but must not depend on its pane tree, dock, sidebar, or resize hub.
- Verify phone routing, overflow, filters, touch input, session switching and exit, Fleet failures, item round trips, retained workspace actions, and desktop workspace regressions.

## Verification expectations

For mobile-visible changes, verify behavior with a real phone profile, not only a resized desktop viewport.

Minimum expectations for meaningful mobile UI changes:

- Use a Playwright phone profile or explicit phone-like viewport/user-agent setup appropriate for the repo's browser matrix.
- Add a separate desktop-narrow regression when canonical PR/issue routes change responsive presentation. This should prove the URL stays canonical, the focus presentation appears when narrow, the phone-only class/tokens do not apply in desktop-narrow contexts, and the desktop shell returns after widening.
- Assert the phone route renders a phone shell/component and not the desktop layout.
- Assert no document-level horizontal overflow.
- Check key element bounds for cards, filters, tabs, branch names, and action buttons; element clipping can happen even when document width is fine.
- Keep the PR branch icon on the head branch's first line, with the unbroken arrow/base pair immediately following its final text (`frontend/src/lib/components/detail/PullDetail.svelte::.branch-target`).
- Assert source sizing remains token/rem-based for the changed mobile surface.
- Cover click/tap flows that move from mobile lists into focused detail routes.
- When testing through Tailscale Serve or another proxy, confirm the proxy target and server process so screenshots are not from stale embedded assets.

## Review checklist

Before shipping mobile UX work, ask:

- Is this a phone-first workflow, or did we just resize desktop?
- Is the primary task obvious without scanning desktop chrome?
- Are type, spacing, and hit targets driven by mobile tokens?
- Are mobile tokens limited to phone-like contexts rather than every narrow desktop viewport?
- Did we keep the global root font size stable?
- Are provider/repo identity and item numbers still clear?
- Do taps navigate to visible phone outcomes rather than desktop-only state?
- Did focused detail and tab routes use builders for their active route family?
- Did Playwright cover both a phone profile and any desktop-narrow canonical route behavior?

If the answer to any of these is no, fix the interaction model before tuning individual CSS values.
