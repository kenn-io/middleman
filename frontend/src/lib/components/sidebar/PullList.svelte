<script lang="ts">
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import { Effect } from "effect";
  import { pollWhileVisible } from "../../effect/poll-while-visible.js";
  import { onDestroy, untrack } from "svelte";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getStores, getNavigate, getSidebar, getActions, getHostState } from "../../context.js";
  import { groupByWorkflow } from "../../stores/workflow.svelte.js";
  import {
    Chip,
    FilterDropdown,
    ScrollBox,
    SearchInput,
    SidebarToggle,
    StatusDot,
  } from "@kenn-io/kit-ui";
  import DiffSidebar from "../diff/DiffSidebar.svelte";
  import GroupedSidebarSection from "../shared/GroupedSidebarSection.svelte";
  import PullItem from "./PullItem.svelte";
  import type { KanbanStatus, PullRequest } from "../../api/types.js";
  import type { GroupingMode } from "../../stores/grouping.svelte.js";
  import type { PullAttributeFilter } from "../../stores/pulls.svelte.js";
  import { createRepoLabelFormatter } from "../../utils/repo-label.js";
  import {
    buildPullRequestFilesRoute,
    buildPullRequestRoute,
    type PullRequestRouteRef,
  } from "../../routes.js";

  const { pulls, sync, grouping, collapsedRepos, settings, activity } = getStores();
  const runtime = getAppRuntime();
  const navigate = getNavigate();
  const actions = getActions();
  const hostState = getHostState();
  const { isEmbedded, isSidebarToggleEnabled, toggleSidebar } = getSidebar();

  const importAction = $derived(
    (actions.pull ?? []).find(
      (a) => a.id === "import-worktree",
    ),
  );
  const activeWorktreeKey = $derived(
    hostState.getActiveWorktreeKey?.(),
  );
  const groupingMode = $derived(
    grouping.getGroupingMode(),
  );
  const workflowGroups = $derived(
    groupByWorkflow(pulls.getFilteredPulls(), activity.getUseWorkspaceActivityForRecency()),
  );
  const pullStateOptions = ["open", "closed", "all"] as const;
  const attributeFilterOptions: {
    value: PullAttributeFilter;
    label: string;
  }[] = [
    { value: "approved", label: "Approved" },
    { value: "draft", label: "Draft" },
    { value: "ready", label: "Ready for review" },
    { value: "merge_conflicts", label: "Merge conflicts" },
    { value: "failed_ci", label: "Failed CI" },
  ];
  const kanbanFilterOptions: {
    value: KanbanStatus;
    label: string;
  }[] = [
    { value: "new", label: "New" },
    { value: "reviewing", label: "Reviewing" },
    { value: "waiting", label: "Waiting" },
    { value: "awaiting_merge", label: "Awaiting merge" },
  ];
  const groupingOptions: {
    value: GroupingMode;
    label: string;
  }[] = [
    { value: "byRepo", label: "Repo" },
    { value: "byWorkflow", label: "Status" },
    { value: "flat", label: "All" },
  ];
  // Playwright-measured with the local PR filter icon and sidebar toggle:
  // the full PR filter row needs 423px, so collapse with a small buffer.
  const COMPACT_FILTER_MAX_WIDTH = 430;
  // Keep the full filter groups visible at medium widths, but collapse the
  // local PR filter trigger label before it crowds the sidebar toggle.
  const LOCAL_FILTER_ICON_ONLY_MAX_WIDTH = 520;

  interface Props {
    getDetailTab?: () => string;
    showSelectedDiffSidebar?: boolean;
    sidebarWidth?: number;
  }
  const {
    getDetailTab: _getDetailTab = () => "conversation",
    showSelectedDiffSidebar = true,
    sidebarWidth = 340,
  }: Props = $props();

  let searchInput = $state(pulls.getSearchQuery() ?? "");
  let searchExecution: AppExecution<void, never> | null = null;
  const visiblePulls = $derived(pulls.getFilteredPulls());
  const repoLabelFormatter = $derived(
    createRepoLabelFormatter(
      visiblePulls.map((pr) => ({
        provider: pr.repo.provider,
        platformHost: pr.repo.platform_host,
        owner: pr.repo.owner,
        name: pr.repo.name,
        repoPath: pr.repo.repo_path,
      })),
      { showOrgNames: !grouping.getHideOrgName() },
    ),
  );

  $effect(() => {
    const execution = untrack(() => runtime.runCommand(
      pollWhileVisible(Effect.sync(pulls.loadPulls), "15 seconds", { immediate: true }),
      { operation: "poll pull request sidebar", safeContext: {}, onFailure: () => {} },
    ));
    const unsubscribeSync = sync.subscribeSyncComplete(pulls.loadPulls);

    return () => {
      execution.interrupt();
      unsubscribeSync();
    };
  });

  onDestroy(() => searchExecution?.interrupt());

  function onSearchInput(value: string): void {
    searchInput = value;

    searchExecution?.interrupt();
    searchExecution = runtime.runCommand(
      Effect.sleep("300 millis").pipe(
        Effect.andThen(Effect.sync(() => {
          pulls.setSearchQuery(value.trim() === "" ? undefined : value.trim());
          pulls.loadPulls();
        })),
      ),
      { operation: "debounce pull request sidebar search", safeContext: {}, onFailure: () => {} },
    );
  }

  function pullStateLabel(state: string): string {
    if (state === "open") return "Open";
    if (state === "closed") return "Closed";
    return "All";
  }

  function pullStateDropdownLabel(state: string): string {
    if (state === "all") return "All states";
    return pullStateLabel(state);
  }

  function groupingDropdownLabel(mode: GroupingMode): string {
    if (mode === "byRepo") return "By repo";
    if (mode === "byWorkflow") return "By status";
    return "Flat list";
  }

  function setPullState(state: string): void {
    pulls.setFilterState(state);
    pulls.loadPulls();
  }

  function resetCompactView(): void {
    pulls.clearLocalFilters();
    grouping.setGroupingMode("byRepo");
    pulls.setStackTree(false);
    grouping.setHideOrgName(false);
    if (pulls.getFilterState() !== "open") {
      pulls.setFilterState("open");
    }
    pulls.loadPulls();
  }

  function clearLocalViewFilters(): void {
    pulls.clearLocalFilters();
    pulls.loadPulls();
    grouping.setHideOrgName(false);
  }

  const toolbarFilterSections = $derived.by(() => [
    {
      title: "State",
      items: pullStateOptions.map((state) => ({
        id: `state-${state}`,
        label: pullStateDropdownLabel(state),
        active: pulls.getFilterState() === state,
        onSelect: () => setPullState(state),
      })),
    },
    {
      title: "Group",
      items: groupingOptions.map((option) => ({
        id: `group-${option.value}`,
        label: groupingDropdownLabel(option.value),
        active: groupingMode === option.value,
        onSelect: () => grouping.setGroupingMode(option.value),
      })),
    },
  ]);

  const localFilterSections = $derived.by(() => [
    {
      title: "PR",
      items: [
        {
          id: "involves-me",
          label: "Involves me",
          active: pulls.getInvolvesMe(),
          onSelect: () => {
            pulls.setInvolvesMe(!pulls.getInvolvesMe());
            pulls.loadPulls();
          },
        },
        {
          id: "unassigned",
          label: "Unassigned",
          active: pulls.getUnassigned(),
          onSelect: () => {
            pulls.setUnassigned(!pulls.getUnassigned());
            pulls.loadPulls();
          },
        },
        ...attributeFilterOptions.map((option) => ({
          id: `pr-${option.value}`,
          label: option.label,
          active: pulls.getAttributeFilters().includes(option.value),
          onSelect: () => pulls.toggleAttributeFilter(option.value),
        })),
      ],
    },
    {
      title: "Status",
      items: [
        ...kanbanFilterOptions.map((option) => ({
          id: `kanban-${option.value}`,
          label: option.label,
          active: pulls.getKanbanStatusFilters().includes(option.value),
          onSelect: () => pulls.toggleKanbanStatusFilter(option.value),
        })),
        {
          id: "has-workspace",
          label: "Has workspace",
          active: pulls.getAttributeFilters().includes("has_workspace"),
          onSelect: () => pulls.toggleAttributeFilter("has_workspace"),
        },
      ],
    },
    {
      title: "View",
      items: [
        {
          id: "stack-tree",
          label: "Stack tree",
          active: pulls.getStackTree(),
          onSelect: () => pulls.setStackTree(!pulls.getStackTree()),
        },
        {
          id: "hide-org-name",
          label: "Hide org name",
          active: grouping.getHideOrgName(),
          onSelect: () => grouping.setHideOrgName(!grouping.getHideOrgName()),
        },
      ],
    },
  ]);
  const compactFilterSections = $derived.by(() => [
    ...toolbarFilterSections,
    ...localFilterSections,
  ]);
  const hasCompactFilterChanges = $derived(
    pulls.getFilterState() !== "open"
      || groupingMode !== "byRepo"
      || pulls.getLocalFilterCount() > 0
      || grouping.getHideOrgName()
      || pulls.getStackTree(),
  );
  const localViewFilterCount = $derived(
    pulls.getLocalFilterCount() + Number(grouping.getHideOrgName()),
  );
  const useCompactFilters = $derived(
    sidebarWidth <= COMPACT_FILTER_MAX_WIDTH,
  );
  const useIconOnlyLocalFilters = $derived(
    sidebarWidth <= LOCAL_FILTER_ICON_ONLY_MAX_WIDTH,
  );

  interface PullGroup {
    key: string;
    collapseKey: string;
    label: string;
    showRepo: boolean;
    items: PullRequest[];
  }

  const groupedPulls = $derived.by((): PullGroup[] | null => {
    if (groupingMode === "byRepo") {
      return [...pulls.pullsByRepo().entries()].map(([repo, prs]) => ({
        key: `repo:${repo}`,
        collapseKey: repo,
        label: prs[0] ? repoLabelFormatter.format({
          provider: prs[0].repo.provider,
          platformHost: prs[0].repo.platform_host,
          owner: prs[0].repo.owner,
          name: prs[0].repo.name,
          repoPath: prs[0].repo.repo_path,
        }) : repo,
        showRepo: false,
        items: prs,
      }));
    }
    if (groupingMode === "byWorkflow") {
      return workflowGroups.map((wg) => ({
        key: `status:${wg.group}`,
        collapseKey: `status:${wg.group}`,
        label: wg.label,
        showRepo: true,
        items: wg.items,
      }));
    }
    return null;
  });

  function routeRefForPull(pr: PullRequest): PullRequestRouteRef {
    return {
      provider: pr.repo.provider,
      platformHost: pr.repo.platform_host,
      owner: pr.repo.owner,
      name: pr.repo.name,
      repoPath: pr.repo.repo_path,
      number: pr.Number,
    };
  }

  function handleSelect(ref: PullRequestRouteRef): void {
    pulls.selectPR(
      ref.owner,
      ref.name,
      ref.number,
      ref.provider,
      ref.platformHost,
      ref.repoPath,
    );
    if (_getDetailTab() === "files") {
      navigate(buildPullRequestFilesRoute(ref));
    } else {
      navigate(buildPullRequestRoute(ref));
    }
  }

  function isSelected(ref: PullRequestRouteRef): boolean {
    const sel = pulls.getSelectedPR();
    return sel !== null
      && sel.owner === ref.owner
      && sel.name === ref.name
      && sel.number === ref.number
      && sel.provider === ref.provider
      && sel.platformHost === ref.platformHost;
  }

  function pullMatchesSelection(pr: PullRequest, sel: NonNullable<ReturnType<typeof pulls.getSelectedPR>>): boolean {
    return pr.Number === sel.number
      && pr.repo.provider === sel.provider
      && pr.repo.platform_host === sel.platformHost
      && pr.repo.repo_path === sel.repoPath
      && pr.repo.owner === sel.owner
      && pr.repo.name === sel.name;
  }

  const selectedPRGroup = $derived.by(() => {
    const sel = pulls.getSelectedPR();
    const groups = groupedPulls;
    if (sel === null || groups === null) return null;
    return groups.find((group) =>
      group.items.some((p) => pullMatchesSelection(p, sel)),
    ) ?? null;
  });

  const keepSelectedGroupExpanded = $derived(
    showSelectedDiffSidebar
      && _getDetailTab() === "files"
      && selectedPRGroup !== null,
  );

  const selectedVisiblePR = $derived.by(() => {
    const sel = pulls.getSelectedPR();
    if (sel === null) return null;
    const items = selectedPRGroup?.items ?? visiblePulls;
    const pr = pulls.getSidebarRows(items).find((row) => pullMatchesSelection(row.pr, sel))?.pr;
    if (!pr) return null;
    // Collapsed grouped modes hide the selected PR row, so the files tab
    // renders the fallback file list instead of losing the diff sidebar.
    if (
      selectedPRGroup !== null
      && !keepSelectedGroupExpanded
      && collapsedRepos.isCollapsed("pulls", selectedPRGroup.collapseKey)
    ) {
      return null;
    }
    return pr;
  });

  const isDiffFocus = $derived(
    showSelectedDiffSidebar
      && _getDetailTab() === "files"
      && selectedVisiblePR !== null,
  );

  // True when in files tab and selected PR isn't actually rendered in sidebar
  // (either filtered out of list, or in user-collapsed repo group).
  const needsFallbackFileList = $derived(
    showSelectedDiffSidebar
      && _getDetailTab() === "files"
      && pulls.getSelectedPR() !== null
      && selectedVisiblePR === null,
  );

  const isSelectedActiveWorktree = $derived.by(() => {
    const key = activeWorktreeKey;
    const pr = selectedVisiblePR;
    if (!key || !pr || !pr.worktree_links) return false;
    return pr.worktree_links.some((l) => l.worktree_key === key);
  });
</script>

{#snippet pullRows(items: PullRequest[], showRepo: boolean)}
  {#each pulls.getSidebarRows(items) as row (row.pr.ID)}
    {@const pr = row.pr}
    {@const prRef = routeRefForPull(pr)}
    {@const prSelected = isSelected(prRef)}
    <div
      class="stack-row"
      class:stack-row--member={row.depth > 0}
      class:stack-row--root={row.memberCount > 0}
      style:--stack-depth={Math.min(row.depth, 3)}
    >
      {#if row.memberCount > 0}
        {@const countLabel = row.memberCount === pr.stack?.size
          ? `${row.memberCount} PRs in stack`
          : `${row.memberCount} of ${pr.stack?.size} PRs in this view`}
        <div class="stack-control">
          {#if row.memberCount > 1}
            <button
              type="button"
              class="stack-toggle"
              aria-expanded={row.expanded}
              aria-label={`${row.expanded ? "Collapse" : "Expand"} stack at #${pr.Number}: ${countLabel}`}
              title={countLabel}
              onclick={() => pulls.toggleStack(row.stackKey, row.expanded)}
            >
              <ChevronDownIcon size={12} class={row.expanded ? "" : "stack-chevron--collapsed"} aria-hidden="true" />
              <span>{row.memberCount}</span>
            </button>
          {:else}
            <span class="stack-partial" title={countLabel} aria-label={countLabel}>1/{pr.stack?.size}</span>
          {/if}
        </div>
      {/if}
      <div class="stack-row-content">
        {#if row.memberCount > 0 && pr.stack && (pr.stack.position > 1 || row.memberCount < pr.stack.size)}
          <span class="stack-context">{row.memberCount} of {pr.stack.size} PRs in this view{pr.stack.position > 1 ? ` · starts at ${pr.stack.position}/${pr.stack.size}` : ""}</span>
        {/if}
        <PullItem
          {pr}
          repoLabel={repoLabelFormatter.format({
            provider: pr.repo.provider,
            platformHost: pr.repo.platform_host,
            owner: pr.repo.owner,
            name: pr.repo.name,
            repoPath: pr.repo.repo_path,
          })}
          {showRepo}
          selected={prSelected}
          {importAction}
          onclick={() => handleSelect(prRef)}
        />
        {#if showSelectedDiffSidebar && prSelected && _getDetailTab() === "files"}
          <div class="diff-files-wrap">
            <DiffSidebar showCommits={false} />
          </div>
        {/if}
      </div>
    </div>
  {/each}
{/snippet}

<div class="pull-list">
  <div class="filter-bar" class:filter-bar--compact={useCompactFilters}>
    <Chip size="xs" tone="muted" uppercase={false} class="list-count-chip">
      {visiblePulls.length} PRs
    </Chip>
    <div class="state-toggle">
      {#each pullStateOptions as s (s)}
        <button
          class="state-btn"
          class:state-btn--active={pulls.getFilterState() === s}
          onclick={() => setPullState(s)}
        >{pullStateLabel(s)}</button>
      {/each}
    </div>
    <div class="group-toggle">
      {#each groupingOptions as option (option.value)}
        <button
          class="group-btn"
          class:group-btn--active={groupingMode === option.value}
          onclick={() => grouping.setGroupingMode(option.value)}
        >{option.label}</button>
      {/each}
    </div>
    <div class="compact-filter-menu">
      <FilterDropdown
        label="Filters"
        title="Filters"
        active={hasCompactFilterChanges}
        badgeCount={localViewFilterCount}
        sections={compactFilterSections}
        resetLabel="Reset view"
        onReset={resetCompactView}
        minWidth="190px"
      />
    </div>
    <div
      class="local-filter-menu"
      class:local-filter-menu--icon-only={useIconOnlyLocalFilters}
    >
      <FilterDropdown
        label="PR filters"
        title="PR filters"
        active={localViewFilterCount > 0 || pulls.getStackTree()}
        badgeCount={localViewFilterCount}
        sections={localFilterSections}
        resetLabel="Clear filters"
        onReset={clearLocalViewFilters}
        minWidth="190px"
      />
    </div>
    {#if isSidebarToggleEnabled()}
      <SidebarToggle
        state="expanded"
        label="sidebar"
        onclick={toggleSidebar}
        class="kit-sidebar-toggle--push"
      />
    {/if}
  </div>
  <div class="search-bar">
    <div class="search-wrap">
      <SearchInput
        bind:value={searchInput}
        size="sm"
        block
        placeholder="Search PRs..."
        ariaLabel="Search PRs"
        oninput={onSearchInput}
      />
    </div>
    <button
      class="star-filter-btn"
      class:star-filter-btn--active={pulls.getFilterStarred()}
      onclick={() => { pulls.setFilterStarred(!pulls.getFilterStarred()); pulls.loadPulls(); }}
      title={pulls.getFilterStarred() ? "Show all" : "Show starred only"}
    >
      {#if pulls.getFilterStarred()}
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
          <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25z"/>
        </svg>
      {:else}
        <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
          <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25zm0 2.445L6.615 5.5a.75.75 0 01-.564.41l-3.097.45 2.24 2.184a.75.75 0 01.216.664l-.528 3.084 2.769-1.456a.75.75 0 01.698 0l2.77 1.456-.53-3.084a.75.75 0 01.216-.664l2.24-2.183-3.096-.45a.75.75 0 01-.564-.41L8 2.694z"/>
        </svg>
      {/if}
    </button>
  </div>

  {#if pulls.getFilterState() !== "open"}
    <p class="state-note">Showing items closed after kenn-forge began tracking them</p>
  {/if}
  <ScrollBox
    dataTest="pr-list"
    label="Pull requests"
    class={[
      "list-body",
      {
        "list-body--diff-focus": isDiffFocus,
        "list-body--diff-focus-worktree": isDiffFocus && isSelectedActiveWorktree,
      },
    ]}
  >
    {#if settings.isSettingsLoaded() && !settings.hasConfiguredRepos()}
      <p class="state-message">No repositories configured.<br />
        {#if !isEmbedded()}<button class="settings-link" onclick={() => navigate("/settings")}>Add one in Settings</button>{/if}</p>
    {:else if pulls.isLoading() && pulls.getPulls().length === 0}
      <p class="state-message">Loading…</p>
    {:else if pulls.getError() !== null && pulls.getPulls().length === 0}
      <p class="state-message state-message--error">Error: {pulls.getError()}</p>
    {:else if visiblePulls.length === 0 && sync.getSyncState()?.running && pulls.getPulls().length === 0}
      <div class="state-message sync-message">
        <StatusDot status="working" label="Syncing pull requests from GitHub" size={6} animated />
        <span aria-hidden="true">Syncing from GitHub…</span>
      </div>
    {:else if visiblePulls.length === 0 && !sync.getSyncState()?.last_run_at && pulls.getPulls().length === 0}
      <p class="state-message">Waiting for first sync…</p>
    {:else if visiblePulls.length === 0 && pulls.getLocalFilterCount() > 0}
      <p class="state-message">No pull requests match these filters.</p>
    {:else if visiblePulls.length === 0}
      <p class="state-message">No pull requests found.</p>
    {:else}
      {#if groupedPulls !== null}
        {#each groupedPulls as group (group.key)}
          {@const userCollapsed = collapsedRepos.isCollapsed("pulls", group.collapseKey)}
          {@const hasSelectedPR = keepSelectedGroupExpanded && selectedPRGroup?.key === group.key}
          {@const collapsed = userCollapsed && !hasSelectedPR}
          <GroupedSidebarSection
            label={group.label}
            count={group.items.length}
            {collapsed}
            onclick={() => collapsedRepos.toggle("pulls", group.collapseKey)}
          >
            {@render pullRows(group.items, group.showRepo)}
          </GroupedSidebarSection>
        {/each}
      {:else}
        {@render pullRows(visiblePulls, true)}
      {/if}
    {/if}
  </ScrollBox>
  {#if needsFallbackFileList}
    <div class="diff-files-wrap">
                  <DiffSidebar showCommits={false} />
                </div>
  {/if}
  <div class="sidebar-footer">
    {#if !isEmbedded()}
      <button class="add-repo-link" onclick={() => navigate("/settings")}>
        + Add repository
      </button>
    {/if}
  </div>
</div>

<style>
  .stack-row {
    display: flex;
    min-width: 0;
  }

  .stack-row-content {
    flex: 1;
    min-width: 0;
  }

  .stack-row--member {
    margin-left: calc(16px + var(--stack-depth) * 12px);
    border-left: 1px solid var(--border-default);
  }

  .stack-control {
    display: flex;
    align-items: flex-start;
    justify-content: center;
    width: 28px;
    flex-shrink: 0;
    padding-top: 8px;
  }

  .stack-toggle {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-1);
    width: 28px;
    min-height: 40px;
    color: var(--text-secondary);
    font-size: var(--font-size-2xs);
    font-variant-numeric: tabular-nums;
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .stack-toggle:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .stack-toggle:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .stack-toggle :global(.stack-chevron--collapsed) {
    transform: rotate(-90deg);
  }

  .stack-partial {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .stack-context {
    display: block;
    padding: 6px 12px 0;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .pull-list {
    display: flex;
    flex-direction: column;
    height: 100%;
    width: 100%;
  }

  .filter-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    background: var(--bg-surface);
    overflow: hidden;
  }

  .search-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-default);
    flex-shrink: 0;
    background: var(--bg-surface);
  }

  .search-wrap {
    flex: 1;
    min-width: 0;
  }

  .star-filter-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    flex-shrink: 0;
    transition: color 0.1s, background 0.1s;
  }

  .star-filter-btn:hover {
    color: var(--accent-amber);
    background: var(--bg-surface-hover);
  }

  .star-filter-btn--active {
    color: var(--accent-amber);
  }

  :global(.list-count-chip) {
    flex-shrink: 0;
  }

  /* Diff focus: combine typographic mute on siblings + a continuous
     accent rail that extends from the selected card through the inline
     file list, binding them as one visual unit. */
  .pull-list :global(.list-body--diff-focus .pull-item:not(.selected) .title) {
    color: var(--text-muted);
    font-weight: 400;
    transition: color 0.15s ease;
  }

  .pull-list :global(.list-body--diff-focus .pull-item:not(.selected) .state-dot) {
    opacity: 0.45;
  }

  .pull-list :global(.list-body--diff-focus .pull-item:not(.selected):hover .title) {
    color: var(--text-secondary);
  }

  .pull-list :global(.list-body--diff-focus .diff-files-wrap) {
    border-left: 3px solid var(--accent-blue);
  }

  .pull-list :global(.list-body--diff-focus-worktree .diff-files-wrap) {
    border-left-color: var(--accent-teal, var(--accent-green));
  }

  .state-message {
    padding: 24px 16px;
    font-size: var(--font-size-md);
    color: var(--text-muted);
    text-align: center;
  }

  .state-message--error {
    color: var(--accent-red);
  }

  .settings-link {
    color: var(--accent-blue);
    cursor: pointer;
    font-size: var(--font-size-md);
    margin-top: 4px;
    display: inline-block;
  }

  .settings-link:hover {
    text-decoration: underline;
  }

  .sync-message {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
  }


  .sidebar-footer {
    padding: 8px 12px;
    border-top: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .add-repo-link {
    font-size: var(--font-size-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: color 0.1s;
    padding: 0;
  }

  .add-repo-link:hover {
    color: var(--accent-blue);
  }

  .state-toggle {
    display: flex;
    gap: var(--space-1);
    background: var(--bg-inset);
    border-radius: 6px;
    padding: 2px;
    animation: sidebar-filter-pop-out 120ms ease-out;
    transform-origin: right center;
  }

  .compact-filter-menu {
    display: none;
    flex-shrink: 0;
    transform-origin: left center;
  }

  .local-filter-menu {
    flex-shrink: 0;
  }

  .local-filter-menu--icon-only :global(.kit-filter-dropdown__btn) {
    width: 34px;
    justify-content: center;
    gap: 0;
    padding-inline: 0;
  }

  .local-filter-menu--icon-only :global(.kit-filter-dropdown__trigger-label) {
    display: none;
  }

  .local-filter-menu--icon-only :global(.kit-filter-dropdown__badge) {
    position: absolute;
    top: -5px;
    right: -5px;
  }

  .compact-filter-menu :global(.kit-filter-dropdown__btn) {
    width: 34px;
    justify-content: center;
    gap: 0;
    padding-inline: 0;
  }

  .compact-filter-menu :global(.kit-filter-dropdown__trigger-label),
  .compact-filter-menu :global(.kit-filter-dropdown__trigger-detail) {
    display: none;
  }

  .compact-filter-menu :global(.kit-filter-dropdown__badge) {
    position: absolute;
    top: -5px;
    right: -5px;
  }

  .state-btn {
    font-size: var(--font-size-xs);
    padding: 2px 8px;
    border: none;
    border-radius: 4px;
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    white-space: nowrap;
  }
  .state-btn--active {
    background: var(--bg-surface);
    color: var(--text-primary);
    box-shadow: var(--shadow-sm);
  }
  .state-note {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    padding: 4px 10px;
    margin: 0;
    border-bottom: 1px solid var(--border-muted);
  }
  .group-toggle {
    display: flex;
    gap: var(--space-1);
    background: var(--bg-inset);
    border-radius: 6px;
    padding: 2px;
    animation: sidebar-filter-pop-out 120ms ease-out;
    transform-origin: right center;
  }
  .group-btn {
    font-size: var(--font-size-xs);
    padding: 2px 8px;
    border: none;
    border-radius: 4px;
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    white-space: nowrap;
  }
  .group-btn--active {
    background: var(--bg-surface);
    color: var(--text-primary);
    box-shadow: var(--shadow-sm);
  }

  .filter-bar--compact .state-toggle,
  .filter-bar--compact .group-toggle {
    display: none;
  }

  .filter-bar--compact .local-filter-menu {
    display: none;
  }

  .filter-bar--compact .compact-filter-menu {
    display: block;
    animation: sidebar-filter-collapse-in 120ms ease-out;
  }

  @keyframes sidebar-filter-collapse-in {
    from {
      opacity: 0.2;
      transform: translateX(-10px) scale(0.82);
    }
    to {
      opacity: 1;
      transform: translateX(0) scale(1);
    }
  }

  @keyframes sidebar-filter-pop-out {
    from {
      opacity: 0;
      transform: translateX(8px) scale(0.92);
    }
    to {
      opacity: 1;
      transform: translateX(0) scale(1);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .state-toggle,
    .group-toggle,
    .filter-bar--compact .compact-filter-menu {
      animation: none;
    }
  }

  .diff-files-wrap {
    max-height: 40vh;
    overflow-y: auto;
  }
</style>
