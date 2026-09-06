<script lang="ts">
  import {
    autoReposition,
    dismissable,
    floatingPopoverStyle,
    TopBar,
    type TopBarTab,
  } from "@kenn-io/kit-ui";
  import { getStores } from "../../context.js";
  import { Effect } from "effect";
  import KbdBadge from "../keyboard/KbdBadge.svelte";
  import type { ModeVisibility } from "../../api/types.js";
  import { tick, untrack } from "svelte";
  import { SvelteMap } from "svelte/reactivity";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { observeResize } from "../../browser/observers.js";
  import {
    getBasePath,
    getLastActivityRoute,
    getLastWorkspaceRoute,
    getPage,
    getRoute,
    navigate,
  } from "../../stores/router.svelte.ts";
  import {
    activitySelectionToRoute,
    parseActivitySelection,
  } from "../../utils/activitySelection.js";
  import RepoTypeahead from "../RepoTypeahead.svelte";
  import HeaderIconButton from "./HeaderIconButton.svelte";
  import ForgeSelector from "./ForgeSelector.svelte";
  import ThemeToggle from "./ThemeToggle.svelte";
  import {
    ChevronDownIcon,
    SearchIcon,
    SettingsIcon,
    SidebarToggleIcon,
    SpinnerIcon,
    SyncIcon,
  } from "../../icons.ts";
  import {
    getGlobalRepo,
    parseRepoFilterValue,
    setGlobalRepo,
  } from "../../stores/filter.svelte.js";
  import { isEmbedded, getUIConfig } from "../../stores/embed-config.svelte.js";
  import { isThemeToggleVisible } from "../../stores/theme.svelte.js";
  import {
    isSidebarCollapsed,
    toggleSidebar,
    isSidebarToggleEnabled,
  } from "../../stores/sidebar.svelte.js";
  import { openPalette } from "../../stores/keyboard/palette-state.svelte.js";
  import { syncRepoForRoute } from "../../utils/repoSelectionSync.js";

  interface Props {
    onheightchange?: ((height: number) => void) | undefined;
  }

  let { onheightchange = undefined }: Props = $props();
  const runtime = getAppRuntime();
  let headerFrame: HTMLDivElement | null = $state(null);
  let syncMenuExecution: AppExecution<void, never> | null = null;

  $effect(() => {
    const node = headerFrame;
    const notify = onheightchange;
    if (!node || !notify) return;

    const update = () => notify(node.getBoundingClientRect().height);
    const execution = untrack(() => runtime.runCommand(
      Effect.scoped(
        Effect.sync(update).pipe(
          Effect.andThen(observeResize(node, update)),
          Effect.andThen(Effect.never),
        ),
      ),
      {
        operation: "observe application header",
        safeContext: {},
        onFailure: () => {},
      },
    ));
    return () => {
      execution.interrupt();
      notify(0);
    };
  });

  const appIconSrc = `${getBasePath().replace(/\/$/, "")}/favicon.svg`;

  const hasSidebarStrip = $derived(
    getPage() === "issues"
    || getPage() === "pulls"
    || getPage() === "workspaces"
    || getPage() === "terminal",
  );

  const stores = getStores();
  const { settings, sync } = stores;

  type ModeKey = keyof ModeVisibility;
  type NavDestination =
    | "activity"
    | "repos"
    | "docs"
    | "actions"
    | "pulls"
    | "issues"
    | "reviews"
    | "workspaces";
  type NavValue = NavDestination | "settings" | "design-system";

  const modeNavOptions: { value: NavDestination; label: string; mode: ModeKey }[] = [
    { value: "activity", label: "Activity", mode: "activity" },
    { value: "repos", label: "Repos", mode: "repos" },
    { value: "docs", label: "Docs", mode: "docs" },
    { value: "actions", label: "Actions", mode: "actions" },
    { value: "pulls", label: "PRs", mode: "pulls" },
    { value: "issues", label: "Issues", mode: "issues" },
    { value: "reviews", label: "Reviews", mode: "reviews" },
    { value: "workspaces", label: "Workspaces", mode: "workspaces" },
  ];

  function handleSync(): void {
    if (sync.getSyncState()?.running || sync.getProviderAvailable() === false) return;
    sync.triggerSync();
  }

  const syncing = $derived(sync.getSyncState()?.running ?? false);
  const providerAvailable = $derived(sync.getProviderAvailable());
  const currentSyncRepo = $derived.by(() => {
    const routeRepo = syncRepoForRoute(getRoute());
    if (routeRepo) return routeRepo;

    const selectedRepos = parseRepoFilterValue(getGlobalRepo());
    return selectedRepos.length === 1 ? selectedRepos[0] : undefined;
  });
  let syncMenuOpen = $state(false);
  let syncControlEl = $state<HTMLDivElement>();
  let syncMenuTriggerEl = $state<HTMLButtonElement>();
  let syncMenuEl = $state<HTMLUListElement>();
  let syncMenuItemEl = $state<HTMLButtonElement>();
  let syncMenuStyle = $state("");

  $effect(() => {
    if (!syncMenuOpen) return;
    const cleanups = [
      dismissable({
        owners: () => [syncControlEl],
        dismiss: closeSyncMenu,
        escapeFocus: () => syncMenuTriggerEl,
      }),
      autoReposition(() => [syncMenuEl], positionSyncMenu),
    ];
    return () => cleanups.forEach((cleanup) => cleanup());
  });

  function positionSyncMenu(): void {
    if (!syncMenuTriggerEl || !syncMenuEl) return;
    syncMenuStyle = floatingPopoverStyle({
      trigger: syncMenuTriggerEl.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: syncMenuEl.offsetWidth,
      popoverHeight: syncMenuEl.offsetHeight,
      align: "end",
      triggerGap: 2,
    });
  }

  function closeSyncMenu(): void {
    syncMenuExecution?.interrupt();
    syncMenuExecution = null;
    syncMenuOpen = false;
  }

  function openSyncMenu(): void {
    if (syncing) return;
    syncMenuExecution?.interrupt();
    syncMenuOpen = true;
    syncMenuExecution = runtime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(Effect.sync(() => {
          positionSyncMenu();
          if (currentSyncRepo) syncMenuItemEl?.focus();
        })),
      ),
      {
        operation: "open repository sync menu",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function toggleSyncMenu(): void {
    if (syncMenuOpen) {
      closeSyncMenu();
      return;
    }
    openSyncMenu();
  }

  function handleSyncMenuTriggerKeydown(event: KeyboardEvent): void {
    if (event.key !== "ArrowDown") return;
    event.preventDefault();
    openSyncMenu();
  }

  function handleSyncMenuItemKeydown(event: KeyboardEvent): void {
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    syncMenuItemEl?.focus();
  }

  function handleCurrentRepoSync(): void {
    const repo = currentSyncRepo;
    if (!repo || syncing) return;
    closeSyncMenu();
    sync.triggerRepoSync(repo);
  }

  const hideProviderRepoSelector = $derived(getUIConfig().hideRepoSelector);
  const isProviderRepoSelectorPage = $derived(
    getPage() === "activity" ||
      getPage() === "repos" ||
      getPage() === "actions" ||
      getPage() === "pulls" ||
      getPage() === "issues" ||
      getPage() === "workspaces" ||
      getPage() === "terminal",
  );
  const showProviderRepoSelector = $derived(!hideProviderRepoSelector && isProviderRepoSelectorPage);
  const reserveProviderRepoSelectorSlot = $derived(!hideProviderRepoSelector && !isProviderRepoSelectorPage);
  let settingsReturnPath = "/";

  function currentAppPath(): string {
    const base = getBasePath();
    const basePrefix = base === "/" ? "" : base.replace(/\/$/, "");
    const fullPath = window.location.pathname + window.location.search;
    if (basePrefix && fullPath.startsWith(basePrefix)) {
      return fullPath.slice(basePrefix.length) || "/";
    }
    return fullPath;
  }

  function toggleSettings(): void {
    if (getPage() === "settings") {
      navigate(settingsReturnPath);
      return;
    }
    settingsReturnPath = currentAppPath();
    navigate("/settings");
  }

  // Settings and the design-system gallery are not modes, but while one of
  // those pages is current it needs a tab entry: the collapsed dropdown
  // otherwise presents the first mode as the current page.
  const reviewsDaemonUnavailable = $derived(
    settings.isModeVisible("reviews")
      && stores.roborevDaemon !== undefined
      && !stores.roborevDaemon.isAvailable(),
  );

  const tabs: TopBarTab[] = $derived.by(() => {
    const entries: TopBarTab[] = modeNavOptions
      .filter((option) => settings.isModeVisible(option.mode)
        && (option.value !== "actions" || !isEmbedded()))
      .map(({ value, label }) => {
        const tab: TopBarTab = { id: value, label };
        if (value === "reviews" && reviewsDaemonUnavailable) {
          tab.indicator = { tone: "danger", title: "Reviews daemon unavailable" };
        }
        return tab;
      });

    if (getPage() === "design-system") {
      entries.push({ id: "design-system", label: "Design system" });
    }
    if (!isEmbedded() && getPage() === "settings") {
      entries.push({ id: "settings", label: "Settings" });
    }

    return entries;
  });

  const routeTabId = $derived(
    getPage() === "terminal"
        ? "workspaces"
        : getPage() === "repo-browser"
          ? "repos"
      : getPage(),
  );
  // The route owns the active tab: TopBar writes a click into the binding,
  // navigation happens through onchange, and this sync settles the binding
  // on whatever page the router actually landed on.
  let activeTab = $state("");
  let tabsCollapsed = $state(false);
  $effect(() => {
    activeTab = routeTabId;
  });

  type StickyMode = "docs";
  const stickyModeDefaults: Record<StickyMode, string> = {
    docs: "/docs",
  };
  const lastStickyModeRoutes = new SvelteMap<StickyMode, string>();

  function stickyModeForPage(page: ReturnType<typeof getPage>): StickyMode | null {
    return page === "docs" ? page : null;
  }

  function rememberCurrentStickyModeRoute(): void {
    const currentMode = stickyModeForPage(getPage());
    if (!currentMode) return;
    lastStickyModeRoutes.set(currentMode, currentAppPath());
  }

  function routeForTab(
    destination: "pulls" | "issues",
  ): string {
    const selected = getPage() === "activity"
      ? parseActivitySelection(window.location.search)
      : null;
    return activitySelectionToRoute(selected, destination)
      ?? `/${destination}`;
  }

  function navigateTab(destination: NavValue): void {
    const currentMode = stickyModeForPage(getPage());
    rememberCurrentStickyModeRoute();
    if (destination === "activity") {
      if (getPage() !== "activity") navigate(getLastActivityRoute());
    }
    else if (destination === "repos") navigate("/repos");
    else if (destination === "actions") navigate("/actions");
    else if (destination === "docs") {
      if (currentMode === destination) {
        lastStickyModeRoutes.set(destination, stickyModeDefaults[destination]);
        navigate(stickyModeDefaults[destination]);
        return;
      }
      navigate(lastStickyModeRoutes.get(destination) ?? stickyModeDefaults[destination]);
    }
    else if (destination === "pulls" || destination === "issues") {
      navigate(routeForTab(destination));
    } else if (destination === "reviews") navigate("/reviews");
    else if (destination === "workspaces") {
      if (getPage() !== "workspaces" && getPage() !== "terminal") {
        navigate(getLastWorkspaceRoute());
      }
    }
    else if (destination === "settings") navigate("/settings");
    else if (destination === "design-system") navigate("/design-system");
  }

  function handleTabChange(value: string): void {
    if (value === "activity") navigateTab("activity");
    else if (value === "repos") navigateTab("repos");
    else if (value === "docs") navigateTab("docs");
    else if (value === "actions") navigateTab("actions");
    else if (value === "pulls") navigateTab("pulls");
    else if (value === "issues") navigateTab("issues");
    else if (value === "reviews") navigateTab("reviews");
    else if (value === "workspaces") navigateTab("workspaces");
    else if (value === "settings") navigateTab("settings");
    else if (value === "design-system") navigateTab("design-system");
  }
</script>

<!-- The app header renders through kit TopBar; app-top-bar is the app-owned
     selector alias (the kit element also carries .kit-top-bar) used by the
     app-startup/focus/embedded/routing specs to assert header presence. -->
<div class="top-bar-frame" bind:this={headerFrame}>
  <TopBar
    class="app-top-bar"
    {tabs}
    bind:active={activeTab}
    bind:collapsed={tabsCollapsed}
    centerTabs
    ariaLabel="Page"
    onchange={handleTabChange}
  >
  {#snippet left()}
    {#if isSidebarCollapsed() && isSidebarToggleEnabled() && !hasSidebarStrip}
      <HeaderIconButton
        onclick={toggleSidebar}
        title="Expand sidebar"
      >
        <SidebarToggleIcon
          size="14"
          strokeWidth="1.5"
          aria-hidden="true"
        />
      </HeaderIconButton>
    {/if}
    <span class="brand">
      <img class="app-icon" src={appIconSrc} alt="" aria-hidden="true" />
      <span class="logo">kenn-forge</span>
    </span>
    <div class="header-selectors">
      <ForgeSelector />
      {#if showProviderRepoSelector}
        <RepoTypeahead
          selected={getGlobalRepo()}
          onchange={setGlobalRepo}
        />
      {:else if reserveProviderRepoSelectorSlot}
        <div
          class="typeahead repo-selector-placeholder"
          aria-hidden="true"
        ></div>
      {/if}
    </div>
  {/snippet}

  {#snippet right()}
    <HeaderIconButton onclick={openPalette} title="Open command palette">
      <SearchIcon size="14" strokeWidth="1.75" aria-hidden="true" />
      <span class="command-palette-shortcut">
        <KbdBadge binding={{ key: "K", ctrlOrMeta: true }} />
      </span>
    </HeaderIconButton>
    {#if !getUIConfig().hideSync}
      <div class="sync-split" bind:this={syncControlEl}>
        <button
          type="button"
          class="action-btn sync-btn sync-primary"
          aria-label={syncing ? "Syncing" : providerAvailable ? "Sync" : "Sync unavailable"}
          title={syncing ? "Syncing" : providerAvailable ? "Sync" : "Hub unavailable"}
          onclick={handleSync}
          disabled={syncing || !providerAvailable}
        >
          {#if syncing}
            <span class="sync-icon sync-icon--spinning" aria-hidden="true">
              <SpinnerIcon
                size="14"
                strokeWidth="2"
              />
            </span>
          {:else}
            <span class="sync-icon" aria-hidden="true">
              <SyncIcon
                size="14"
                strokeWidth="1.75"
              />
            </span>
          {/if}
          {#if !tabsCollapsed}
            <span class="sync-label">{syncing ? "Syncing..." : "Sync"}</span>
          {/if}
        </button>
        <button
          bind:this={syncMenuTriggerEl}
          type="button"
          class="action-btn sync-menu-trigger"
          aria-label="Sync options"
          title={providerAvailable ? "Sync options" : "Hub unavailable"}
          aria-haspopup="menu"
          aria-expanded={syncMenuOpen}
          onclick={toggleSyncMenu}
          onkeydown={handleSyncMenuTriggerKeydown}
          disabled={syncing || !providerAvailable}
        >
          <ChevronDownIcon size="12" strokeWidth="1.75" aria-hidden="true" />
        </button>
        {#if syncMenuOpen}
          <ul
            bind:this={syncMenuEl}
            class="sync-menu kit-popover-card"
            role="menu"
            aria-label="Sync options"
            style={syncMenuStyle}
          >
            <li>
              <button
                bind:this={syncMenuItemEl}
                type="button"
                role="menuitem"
                title={currentSyncRepo ? "Sync current repo" : "Select one repository to sync"}
                disabled={!currentSyncRepo || syncing}
                onclick={handleCurrentRepoSync}
                onkeydown={handleSyncMenuItemKeydown}
              >
                Sync current repo
              </button>
            </li>
          </ul>
        {/if}
      </div>
    {/if}
    {#if isThemeToggleVisible()}
      <ThemeToggle />
    {/if}
    {#if !isEmbedded()}
      <HeaderIconButton
        active={getPage() === "settings"}
        onclick={toggleSettings}
        title="Settings"
      >
        <SettingsIcon size="14" strokeWidth="1.75" aria-hidden="true" />
      </HeaderIconButton>
    {/if}
  {/snippet}
  </TopBar>
</div>

<style>
  .top-bar-frame {
    flex: 0 0 auto;
    min-width: 0;
  }

  .brand {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    flex-shrink: 0;
  }

  .header-selectors {
    display: flex;
    flex: 0 1 auto;
    min-width: 0;
    align-items: center;
    gap: 8px;
  }

  .app-icon {
    display: block;
    width: 22px;
    height: 22px;
  }

  .logo {
    font-weight: 600;
    font-size: var(--font-size-lg);
    color: var(--text-primary);
    letter-spacing: -0.01em;
  }

  .action-btn {
    box-sizing: border-box;
    height: 28px;
    padding: 5px 12px;
    border-radius: var(--radius-sm);
    font-size: var(--font-size-md);
    font-weight: 500;
    color: var(--text-secondary);
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    transition: background 0.15s, color 0.15s, border-color 0.15s;
  }

  .command-palette-shortcut {
    display: contents;
  }

  .command-palette-shortcut :global(.kit-kbd-badge) {
    border-color: transparent;
    background: transparent;
  }

  .sync-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    min-width: 34px;
    min-height: 28px;
    line-height: 0;
  }

  .sync-split {
    position: relative;
    display: inline-flex;
    flex-shrink: 0;
  }

  .sync-primary {
    border-radius: var(--radius-sm) 0 0 var(--radius-sm);
  }

  .sync-menu-trigger {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 24px;
    margin-left: -1px;
    padding-inline: 6px;
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    line-height: 0;
  }

  .sync-menu {
    position: fixed;
    z-index: var(--z-popover);
    min-width: 168px;
    margin: 0;
    padding: var(--space-1);
    list-style: none;
  }

  .sync-menu button {
    width: 100%;
    padding: var(--space-2) var(--space-3);
    border: 0;
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    background: transparent;
    font: inherit;
    font-size: var(--font-size-md);
    text-align: left;
    white-space: nowrap;
  }

  .sync-menu button:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .sync-menu button:disabled {
    color: var(--text-muted);
    cursor: not-allowed;
  }

  .sync-icon {
    display: inline-flex;
    flex-shrink: 0;
  }

  .sync-icon--spinning {
    animation: kit-spin 0.9s linear infinite;
  }

  .sync-label {
    line-height: 1;
  }

  .repo-selector-placeholder {
    display: block;
    height: 26px;
    pointer-events: none;
    visibility: hidden;
  }

  .action-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    border-color: var(--border-muted);
  }

  .action-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  /* kit's collapse probe is a position:absolute row that always renders the
     full (uncollapsed) tab labels to measure their natural width. With no
     clip it extends past the bar and inflates body scrollWidth, producing
     horizontal page overflow at narrow widths. Clip the x-axis only: the
     nav and typeahead dropdowns open downward and must stay visible, and the
     probe's own offsetWidth (what kit measures) is unaffected by the clip. */
  :global(.app-top-bar) {
    overflow-x: clip;
  }

  /* Region sizing on kit's bar: the side regions never shrink (kit collapses
     the tabs first), so the repo typeahead gets an app-side width cap to keep
     the left region honest in tighter containers. */
  :global(.header-selectors > .forge-selector) {
    flex: 0 1 180px;
    max-width: 180px;
  }

  :global(.header-selectors > .typeahead) {
    flex: 1 1 150px;
    min-width: 128px;
    max-width: 220px;
  }

  :global(#app.container-medium .kit-top-bar) {
    gap: 8px;
    padding-inline: 10px;
  }

  :global(.kit-top-bar .kit-top-bar__nav-select .kit-select-dropdown__trigger) {
    border-color: var(--border-muted);
    background: var(--bg-inset);
  }

  /* Narrow containers (embedded or split panes under 500px) keep the
     two-row header: the left region wraps onto the first row and the
     collapsed nav dropdown shares the second row with the action buttons.
     kit's measurement keeps the tabs collapsed here — the wrap only reorders
     the regions it renders.

     The left region is content-sized (flex: 0 1 auto) rather than stretched
     to a full row: a stretched left inflated the side-region footprint kit
     freezes into expandUsed at collapse time, which then blocked the tabs
     from ever re-expanding when the container widened. Content-sizing keeps
     kit's collapse math honest across the narrow->wide transition. */
  :global(#app.container-narrow .kit-top-bar) {
    height: auto;
    min-height: 82px;
    align-items: center;
    flex-wrap: wrap;
    gap: 6px 8px;
    padding: 6px 10px;
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__left) {
    flex: 0 1 auto;
    order: 1;
    gap: 8px;
  }

  :global(#app.container-narrow .kit-top-bar) .header-selectors {
    flex: 0 1 220px;
  }

  :global(#app.container-narrow .kit-top-bar) .brand {
    gap: 6px;
  }

  :global(#app.container-narrow .kit-top-bar) .app-icon {
    width: 20px;
    height: 20px;
  }

  :global(#app.container-narrow .kit-top-bar .header-selectors > .forge-selector),
  :global(#app.container-narrow .kit-top-bar .header-selectors > .typeahead) {
    flex: 1 1 auto;
    min-width: 0;
    max-width: none;
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__left .typeahead-trigger),
  :global(#app.container-narrow .kit-top-bar .kit-top-bar__left .typeahead-input) {
    height: 30px;
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__nav) {
    flex: 1 1 min(190px, 100%);
    min-width: 0;
    order: 2;
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__nav-select) {
    width: 100%;
    min-width: 0;
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__nav-select .kit-select-dropdown__trigger) {
    min-height: 32px;
    font-size: var(--font-size-md);
  }

  :global(#app.container-narrow .kit-top-bar .kit-top-bar__right) {
    flex: 0 0 auto;
    order: 3;
    margin-left: 0;
    gap: 6px;
  }

  /* Every right-region control gets the same narrow-height bump, not just the
     sync .action-btn: the hand-rolled sync button and the HeaderIconButton
     controls (palette, theme, settings) sit side by side, so bumping one
     alone leaves them misaligned — and mismatched mid-transition, since kit
     re-expands the tabs immediately while the container class drops on a
     debounce. Governing all of them by the same class keeps their heights
     equal in every state. */
  :global(#app.container-narrow .kit-top-bar .kit-top-bar__right button) {
    height: 32px;
    padding-inline: 10px;
  }
</style>
