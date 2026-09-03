<script lang="ts">
  import type { Snippet } from "svelte";
  import type { ActivityItem } from "../api/types.js";
  import { getStackDepth } from "../stores/keyboard/modal-stack.svelte.js";
  import ActivityFeed from "../components/ActivityFeed.svelte";
  import CommitDiffPanel from "../components/CommitDiffPanel.svelte";
  import { IconButton, SidebarToggle, SplitResizeHandle } from "@kenn-io/kit-ui";
  import type { SplitResizeEvent } from "@kenn-io/kit-ui";
  import type { PullRequestRouteRef } from "../routes.js";
  import IssueDetail from "../components/detail/IssueDetail.svelte";
  import PullDetailPane from "../components/detail/PullDetailPane.svelte";
  import { issueDetailMatchesRef, pullDetailMatchesRef } from "../components/detail/detail-match.js";
  import DetailPaneLayout from "../components/shared/DetailPaneLayout.svelte";
  import type { TabbedPanelLeaf } from "../components/shared/tabbed-panel-layout.js";
  import { getPaneLayoutStore, type PaneTabSpec } from "../stores/paneLayout.svelte.js";
  import { isSessionPaneKey } from "../stores/session-pane-key.js";
  import { getStores } from "../context.js";
  import { useItemWorkspaceClaim } from "../item-workspace-claim.svelte.js";
  import type { ActivityCommitSelection } from "../utils/activitySelection.js";
  import type { InlineWorkspaceController, WorkspaceItemIdentity } from "../workspace-inline.js";

  type ActivityDetailTab = "conversation" | "files";

  type DrawerPRItem = PullRequestRouteRef & {
    itemType: "pr";
    detailTab: ActivityDetailTab;
  };

  type DrawerItem = {
    itemType: "pr" | "issue";
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
    owner: string;
    name: string;
    number: number;
    detailTab?: ActivityDetailTab;
  };

  interface Props {
    drawerItem?: DrawerItem | null;
    detailTab?: ActivityDetailTab;
    onSelectItem?: (item: ActivityItem) => void;
    onCloseDrawer?: () => void;
    onDetailTabChange?: (tab: ActivityDetailTab, options?: { replace?: boolean }) => void;
    onDrawerItemChange?: (item: DrawerPRItem) => void;
    commitItem?: ActivityCommitSelection | null;
    onSelectCommit?: (item: ActivityCommitSelection) => void;
    phone?: boolean;
    inlineWorkspace?: InlineWorkspaceController | null;
    /**
     * The workspace's own controls, rendered in the tab strip of the leaf holding
     * the workspace pane or one of its promoted sessions. Supplied by the app
     * shell: the controls live in `frontend/`, next to the state they act on.
     */
    workspacePaneControls?: Snippet<[boolean]> | undefined;
  }

  let {
    drawerItem: controlledDrawer,
    detailTab = "conversation",
    onSelectItem,
    onCloseDrawer,
    onDetailTabChange,
    onDrawerItemChange,
    commitItem: controlledCommitItem,
    onSelectCommit,
    phone = false,
    inlineWorkspace = null,
    workspacePaneControls = undefined,
  }: Props = $props();

  const { detail: detailStore, issues: issuesStore } = getStores();
  const paneLayout = getPaneLayoutStore("activity");

  const ACTIVITY_PANE_WIDTH_KEY = "kenn-forge-activity-pane-width";
  const DEFAULT_ACTIVITY_PANE_WIDTH = 360;

  function loadActivityPaneWidth(): number {
    try {
      const raw = localStorage.getItem(ACTIVITY_PANE_WIDTH_KEY);
      if (raw) {
        const parsed = Number(raw);
        if (Number.isFinite(parsed) && parsed > 0) {
          return parsed;
        }
      }
    } catch {
      // Storage blocked (private mode / embedded host); use the default.
    }
    return DEFAULT_ACTIVITY_PANE_WIDTH;
  }

  function persistActivityPaneWidth(value: number): void {
    try {
      localStorage.setItem(ACTIVITY_PANE_WIDTH_KEY, String(Math.round(value)));
    } catch {
      // Storage blocked; the rail width just won't survive a reload.
    }
  }

  // Internal state used when no controlled props are
  // provided (standalone usage).
  let internalDrawer = $state<DrawerItem | null>(null);
  let internalCommitDrawer = $state<ActivityCommitSelection | null>(null);
  let internalDetailTab = $state<ActivityDetailTab>(
    "conversation",
  );
  // The width the user has dragged the rail to, restored from storage so it
  // survives reloads. The effective width below re-clamps it reactively so it
  // also survives viewport changes.
  let requestedActivityPaneWidth = $state(loadActivityPaneWidth());
  let activityPaneCollapsed = $state(false);
  // Measured width of the whole split shell so the rail's upper bound
  // scales with the viewport rather than a fixed pixel cap.
  let activityShellWidth = $state(0);

  const minActivityPaneWidth = 280;
  // Space always kept for the detail pane so the rail can never be
  // dragged wide enough to squeeze it to nothing.
  const minDetailPaneWidth = 360;
  let activityResizeStartWidth = 0;

  // No fixed ceiling: on a wide monitor the rail grows until only
  // minDetailPaneWidth remains for the detail pane. Before the shell is
  // measured the bound is open so the initial width is never clamped.
  const maxActivityPaneWidth = $derived(
    activityShellWidth > 0
      ? Math.max(minActivityPaneWidth, activityShellWidth - minDetailPaneWidth)
      : Number.POSITIVE_INFINITY,
  );

  function clampActivityPaneWidth(width: number): number {
    return Math.max(
      minActivityPaneWidth,
      Math.min(maxActivityPaneWidth, width),
    );
  }

  // Effective rail width: the requested width re-clamped to the current
  // maximum, so narrowing the viewport keeps the detail pane visible and
  // widening it restores the rail toward the requested width.
  const activityPaneWidth = $derived(
    clampActivityPaneWidth(requestedActivityPaneWidth),
  );
  const activityPaneAriaMax = $derived(
    Number.isFinite(maxActivityPaneWidth)
      ? maxActivityPaneWidth
      : Math.max(minActivityPaneWidth, activityPaneWidth),
  );

  const controlled = $derived(
    controlledDrawer !== undefined || onCloseDrawer !== undefined,
  );
  const activeDrawer = $derived(
    controlled ? (controlledDrawer ?? null) : internalDrawer,
  );
  // Same seam shape as the item drawer above: a host that supplies either the
  // value or the callback owns the commit selection and its URL round trip;
  // standalone usage keeps it local.
  const commitControlled = $derived(
    controlledCommitItem !== undefined || onSelectCommit !== undefined,
  );
  const commitDrawer = $derived(
    commitControlled ? (controlledCommitItem ?? null) : internalCommitDrawer,
  );
  const hasActiveDetail = $derived(
    activeDrawer !== null || commitDrawer !== null,
  );
  const effectiveDetailTab = $derived(
    controlled ? detailTab : internalDetailTab,
  );
  // Guarded snapshots for the drawer detail panes. As inline prop
  // object literals these would compile into deriveds that can
  // re-evaluate while the {#if} branch below is tearing down — after
  // activeDrawer has already flipped to null — and crash reading
  // activeDrawer.owner. Hoisting them behind the null check makes a
  // teardown-time re-read return null instead of throwing.
  const drawerPRSelection = $derived(
    activeDrawer?.itemType === "pr"
      ? {
          owner: activeDrawer.owner,
          name: activeDrawer.name,
          number: activeDrawer.number,
          provider: activeDrawer.provider,
          platformHost: activeDrawer.platformHost,
          repoPath: activeDrawer.repoPath,
        }
      : null,
  );
  const drawerIssueSelection = $derived(
    activeDrawer && activeDrawer.itemType !== "pr"
      ? {
          owner: activeDrawer.owner,
          name: activeDrawer.name,
          number: activeDrawer.number,
          provider: activeDrawer.provider,
          platformHost: activeDrawer.platformHost,
          repoPath: activeDrawer.repoPath,
        }
      : null,
  );
  const isPRSelection = $derived(drawerPRSelection !== null);

  /** The selected item, whichever kind, for the claim lifecycle. */
  const claimIdentity = $derived<WorkspaceItemIdentity | null>(
    activeDrawer
      ? {
          provider: activeDrawer.provider,
          platformHost: activeDrawer.platformHost,
          owner: activeDrawer.owner,
          name: activeDrawer.name,
          repoPath: activeDrawer.repoPath,
          number: activeDrawer.number,
          // Drawer vocabulary ("pr"/"issue"); canonicalItemType maps it.
          itemType: activeDrawer.itemType,
        }
      : null,
  );

  /** Detail for the selected PR, or null while it is stale or absent. */
  const selectedPullDetail = $derived.by(() => {
    const detail = detailStore.getDetail();
    return pullDetailMatchesRef(detail, drawerPRSelection) ? detail : null;
  });

  function loadedDetailMatchesSelection(): boolean {
    if (drawerPRSelection) return pullDetailMatchesRef(detailStore.getDetail(), drawerPRSelection);
    if (drawerIssueSelection) return issueDetailMatchesRef(issuesStore.getIssueDetail(), drawerIssueSelection);
    return false;
  }

  function selectionEnvelopeRef() {
    if (drawerPRSelection) return detailStore.getDetail()?.workspace ?? null;
    if (drawerIssueSelection) return issuesStore.getIssueDetail()?.workspace ?? null;
    return null;
  }

  function refreshSelectionDetail(): void {
    if (drawerPRSelection) {
      const ref = drawerPRSelection;
      detailStore.loadDetail(ref.owner, ref.name, ref.number, {
        sync: false,
        provider: ref.provider,
        platformHost: ref.platformHost,
        repoPath: ref.repoPath,
      });
      return;
    }
    if (drawerIssueSelection) {
      const ref = drawerIssueSelection;
      issuesStore.loadIssueDetail(ref.owner, ref.name, ref.number, {
        sync: false,
        provider: ref.provider,
        platformHost: ref.platformHost,
        repoPath: ref.repoPath,
      });
    }
  }

  const workspaceClaim = useItemWorkspaceClaim({
    controller: () => inlineWorkspace,
    identity: () => claimIdentity,
    detailMatches: loadedDetailMatchesSelection,
    envelopeRef: selectionEnvelopeRef,
    refresh: refreshSelectionDetail,
  });

  // One pane vocabulary for every Activity selection kind, with availability
  // doing the switching: a PR contributes a diff pane, a commit contributes its
  // own, and an issue contributes neither. Keeping them in one tree is what lets
  // an arrangement survive moving between selections.
  // One entry per session the surface's stored tree already holds. `available`
  // never conjures a pane: a session pane exists only because the user promoted
  // it, so a workspace whose sessions were never promoted adds nothing here.
  const sessionTabs = $derived<PaneTabSpec[]>(
    (inlineWorkspace?.promotableSessions() ?? []).map((session) => ({
      key: session.paneKey,
      label: session.label,
      available: paneLayout.hasTab(session.paneKey),
      hideable: true,
    })),
  );

  const paneTabs = $derived<PaneTabSpec[]>([
    { key: "conversation", label: "Conversation", available: activeDrawer !== null },
    { key: "files", label: "Files changed", available: isPRSelection },
    { key: "commit", label: "Commit", available: commitDrawer !== null },
    {
      key: "workspace",
      label: inlineWorkspace?.workspacePaneLabel() ?? "Workspace",
      // Retire an empty workflow container behind the surface-hosted dock. A
      // promoted session then fills the branch beside it without a blank stage.
      available:
        workspaceClaim.ref() !== null &&
        inlineWorkspace?.workspacePaneEmpty() !== true &&
        inlineWorkspace?.workspacePaneRowOnly() !== true,
      hideable: true,
    },
    ...sessionTabs,
  ]);

  // Whether the drawer's two route-bound panes are on screen at once, which is
  // what decides that moving between them is a focus change rather than a
  // navigation. Same arrangement rule as PRs mode.
  const routePanesSplitApart = $derived.by(() => {
    // Straight from the renderer, never inferred from the stored tree: flatten,
    // a zoom over the other leaf, and a pane tabbed behind a sibling all show
    // one pane while the tree still reads as split.
    const render = paneLayout.paneRender();
    if (render === null || render.flattened) return false;
    return render.onScreenTabs.includes("conversation") && render.onScreenTabs.includes("files");
  });

  function handlePaneSelect(tabKey: string): void {
    // Only the PR's two panes are bound to the drawer's tab state; commit and
    // workspace have no equivalent.
    if (tabKey === "conversation" || tabKey === "files") handleDetailTabChange(tabKey);
  }

  /**
   * Follow the user between the two route-bound panes when both are visible.
   *
   * Without this the drawer's `detailTab` — and so the pane the layout treats as
   * route-bound — stays on whichever pane was last clicked, even after the user
   * moves into the other one and starts working in it. Only while split: sharing
   * a leaf means the invisible sibling can still take focus programmatically.
   */
  function handlePaneFocus(tabKey: string): void {
    // Every pane, before the detail-tab filter below: the host decides which keys
    // name a workspace, and it needs the container and the promoted session panes
    // that this branch would otherwise drop.
    inlineWorkspace?.notePaneFocused(tabKey);
    if (tabKey !== "conversation" && tabKey !== "files") return;
    if (!isPRSelection || tabKey === effectiveDetailTab) return;
    if (!routePanesSplitApart) return;
    // Activity's selection lives in the query string and is always written with
    // replaceUrl, so this only keeps the drawer's own tab state in step.
    handleDetailTabChange(tabKey, { replace: true });
  }

  function handleDetailTabChange(
    tab: ActivityDetailTab,
    options?: { replace?: boolean },
  ): void {
    if (controlled) {
      onDetailTabChange?.(tab, options);
      return;
    }
    internalDetailTab = tab;
  }

  function handleStackMemberNavigate(ref: PullRequestRouteRef): boolean {
    const nextDrawer: DrawerPRItem = {
      ...ref,
      itemType: "pr",
      detailTab: effectiveDetailTab,
    };
    if (!controlled) {
      internalDrawer = nextDrawer;
    }
    onDrawerItemChange?.(nextDrawer);
    return true;
  }

  function handleSelect(item: ActivityItem): void {
    if (!commitControlled) internalCommitDrawer = null;
    if (!item.repo) {
      throw new Error("activity item missing provider repo identity");
    }
    const itemType =
      item.item_type === "issue" ? "issue" : "pr";
    const entry: DrawerItem = {
      itemType,
      provider: item.repo.provider,
      platformHost: item.repo.platform_host,
      repoPath: item.repo.repo_path,
      owner: item.repo.owner,
      name: item.repo.name,
      number: item.item_number,
      detailTab: "conversation",
    };
    if (!controlled) {
      internalDrawer = entry;
      internalDetailTab = "conversation";
    }
    onSelectItem?.(item);
  }

  function handleSelectBranchCommit(item: ActivityItem): void {
    if (!item.repo) {
      throw new Error("branch activity item missing provider repo identity");
    }
    if (!item.commit_sha) return;

    const entry: ActivityCommitSelection = {
      itemType: "commit",
      provider: item.repo.provider,
      platformHost: item.repo.platform_host,
      repoPath: item.repo.repo_path,
      owner: item.repo.owner,
      name: item.repo.name,
      branchName: item.branch_name || "default branch",
      commitSha: item.commit_sha,
      title: item.body_preview || item.commit_sha.slice(0, 12),
    };
    if (commitControlled) {
      onSelectCommit?.(entry);
    } else {
      internalCommitDrawer = entry;
    }
    if (!controlled) {
      internalDrawer = null;
    } else if (!commitControlled && activeDrawer !== null) {
      onCloseDrawer?.();
    }
  }

  function handleClose(): void {
    activityPaneCollapsed = false;
    if (!commitControlled) internalCommitDrawer = null;
    if (!controlled) {
      internalDrawer = null;
    }
    onCloseDrawer?.();
  }

  function handleActivityPaneResizeStart(): void {
    activityResizeStartWidth = activityPaneWidth;
  }

  function handleActivityPaneResize(
    event: SplitResizeEvent,
  ): void {
    requestedActivityPaneWidth = clampActivityPaneWidth(
      activityResizeStartWidth + event.delta,
    );
    persistActivityPaneWidth(requestedActivityPaneWidth);
  }

  function collapseActivityPane(): void {
    activityPaneCollapsed = true;
  }

  function expandActivityPane(): void {
    activityPaneCollapsed = false;
  }

  // Escape closes the active drawer when one is open. Mirrors the
  // behavior of the previous DetailDrawer the split view replaced.
  // Dialogs register on the modal stack and their kit-ui Escape handler
  // runs after this one (later window listener), so defaultPrevented can't
  // signal "a dialog owns this Escape" here — check the stack instead.
  $effect(() => {
    if (!hasActiveDetail) return;
    function onKey(event: KeyboardEvent): void {
      if (event.key !== "Escape") return;
      if (event.defaultPrevented) return;
      if (getStackDepth() > 0) return;
      const target = event.target as HTMLElement | null;
      const tag = target?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA") return;
      if (target?.isContentEditable) return;
      handleClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
</script>

<div
  class="activity-shell"
  class:activity-shell--split={hasActiveDetail}
  class:activity-shell--full={!hasActiveDetail}
  class:activity-shell--phone={phone}
  bind:clientWidth={activityShellWidth}
>
  <section
    class="activity-pane"
    class:activity-pane--collapsed={hasActiveDetail && activityPaneCollapsed}
    style:--activity-pane-width={`${activityPaneWidth}px`}
  >
    {#if hasActiveDetail && activityPaneCollapsed}
      <div class="activity-collapsed-strip">
        <SidebarToggle
          state="collapsed"
          label="Activity sidebar"
          onclick={expandActivityPane}
          class="kit-sidebar-toggle--compact"
        />
      </div>
    {:else if hasActiveDetail}
      <div class="activity-rail-header">
        <span>Activity</span>
        <SidebarToggle
          state="expanded"
          label="Activity sidebar"
          onclick={collapseActivityPane}
          class="kit-sidebar-toggle--compact"
        />
      </div>
    {/if}
    <div class="activity-feed-wrap">
      <ActivityFeed
        compact={phone || hasActiveDetail}
        selectedItem={activeDrawer}
        selectedBranchCommit={commitDrawer}
        onSelectItem={handleSelect}
        onSelectBranchCommit={handleSelectBranchCommit}
      />
    </div>
  </section>

  {#if hasActiveDetail && !activityPaneCollapsed}
    <SplitResizeHandle
      class="activity-split-resize-handle"
      ariaLabel="Resize Activity rail"
      orientation="horizontal"
      ariaValueMin={minActivityPaneWidth}
      ariaValueMax={activityPaneAriaMax}
      ariaValueNow={activityPaneWidth}
      onResizeStart={handleActivityPaneResizeStart}
      onResize={handleActivityPaneResize}
    />
  {/if}

  {#if activeDrawer || commitDrawer}
    <section class="activity-detail">
      <div class="activity-detail-header">
        <span>
          {#if commitDrawer}
            Commit {commitDrawer.repoPath} {commitDrawer.branchName} {commitDrawer.title}
          {:else if activeDrawer}
            {activeDrawer.owner}/{activeDrawer.name}#{activeDrawer.number}
          {/if}
        </span>
        <IconButton
          size="sm"
          onclick={handleClose}
          ariaLabel="Close Activity selection"
        >
          <span aria-hidden="true">&times;</span>
        </IconButton>
      </div>

      <DetailPaneLayout
        layout={paneLayout}
        tabs={paneTabs}
        tablistLabel="Activity detail panes"
        leafLabel="Activity detail pane group"
        routeTabKey={isPRSelection ? effectiveDetailTab : undefined}
        onSelectTab={handlePaneSelect}
        onFocusPane={handlePaneFocus}
        paneLeafExtras={workspacePaneControls ? workspaceLeafExtras : undefined}
      >
        {#snippet renderPane(tabKey, visible, inputActive)}
          {#if tabKey === "commit" && commitDrawer && visible}
            {#key commitDrawer.commitSha}
              <CommitDiffPanel
                provider={commitDrawer.provider}
                platformHost={commitDrawer.platformHost}
                owner={commitDrawer.owner}
                name={commitDrawer.name}
                repoPath={commitDrawer.repoPath}
                commitSha={commitDrawer.commitSha}
                {inputActive}
              />
            {/key}
          {:else if tabKey === "workspace" && inlineWorkspace && visible}
            <!-- Portal target for the single live terminal subtree. Mounted only
                 while visible: a slot left behind another tab or a zoom would stay
                 the registered host and strand the terminal off screen. -->
            <div class="detail-pane-workspace-slot" {@attach inlineWorkspace.slotAttachment}></div>
          {:else if isSessionPaneKey(tabKey)}
            {@const sessionPane = inlineWorkspace?.sessionPane() ?? null}
            {#if sessionPane}
              <!-- The frontend supplies the body: it owns the session registry, and
                   the visibility argument travels with it so a pane tabbed behind a
                   sibling leaves its terminal inert rather than off screen and live. -->
              {@render sessionPane({ paneKey: tabKey, visible })}
            {/if}
          {:else if drawerPRSelection}
            <PullDetailPane
              {tabKey}
              {visible}
              keyboardActive={inputActive}
              pr={drawerPRSelection}
              detail={selectedPullDetail}
              autoSync="background"
              hideStaleWhileLoading={true}
              workflowApprovalSync={false}
              onStackMemberNavigate={handleStackMemberNavigate}
              {inlineWorkspace}
            />
          {:else if tabKey === "conversation" && drawerIssueSelection}
            <IssueDetail
              owner={drawerIssueSelection.owner}
              name={drawerIssueSelection.name}
              number={drawerIssueSelection.number}
              provider={drawerIssueSelection.provider}
              platformHost={drawerIssueSelection.platformHost}
              repoPath={drawerIssueSelection.repoPath}
              autoSync="background"
              hideStaleWhileLoading={true}
              {inlineWorkspace}
            />
          {/if}
        {/snippet}
      </DetailPaneLayout>
      <!-- The terminal dock, anchored at this surface's bottom edge while the
           container pane has retired because it is empty or row-only. The dock
           normally lives inside that pane, and must remain reachable outside it. -->
      {@render inlineWorkspace?.dockRow()?.()}
    </section>
  {/if}
</div>

<!-- Only the leaf actually holding the workspace or one of its promoted sessions:
     the controls act on that workspace, so offering them from a leaf of unrelated
     panes would be a control with no subject. -->
{#snippet workspaceLeafExtras(leaf: TabbedPanelLeaf)}
  {#if leaf.tabs.some((tabKey) => tabKey === "workspace" || isSessionPaneKey(tabKey))}
    {@render workspacePaneControls?.(leaf.tabs.includes("workspace"))}
  {/if}
{/snippet}

<style>
  .activity-shell {
    flex: 1;
    overflow: hidden;
    display: flex;
    min-height: 0;
    container-type: inline-size;
  }

  .activity-pane {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .activity-shell--split .activity-pane {
    width: var(--activity-pane-width, 360px);
    flex: 0 0 var(--activity-pane-width, 360px);
    border-right: 1px solid var(--border-default);
  }

  .activity-shell--split .activity-pane--collapsed {
    width: 28px;
    flex-basis: 28px;
  }

  .activity-feed-wrap {
    min-height: 0;
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .activity-shell--split .activity-pane--collapsed .activity-feed-wrap {
    display: none;
  }

  .activity-rail-header,
  .activity-detail-header {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    min-height: 34px;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }

  .activity-collapsed-strip {
    width: 28px;
    flex: 1;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding-top: 6px;
    background: var(--bg-surface);
  }

  .activity-detail-header span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .activity-detail {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .detail-pane-workspace-slot {
    display: flex;
    flex: 1;
    min-width: 0;
    min-height: 0;
    height: 100%;
  }

  @container (max-width: 760px) {
    .activity-shell--split .activity-pane {
      display: none;
    }

    .activity-shell--split :global(.activity-split-resize-handle) {
      display: none;
    }
  }

  .activity-shell--phone .activity-pane {
    width: 100%;
  }

  .activity-shell--phone .activity-feed-wrap {
    min-width: 0;
  }
</style>
