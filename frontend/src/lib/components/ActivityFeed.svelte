<script lang="ts">
  import {
    EmptyState,
    SearchInput,
    Spinner,
    Toggle,
    type TypeaheadOption,
  } from "@kenn-io/kit-ui";
  import { Effect } from "effect";
  import { onMount, onDestroy } from "svelte";
  import type { ActivityItem, ActivitySubject, WorkspaceActivitySubject } from "../api/types.js";
  import type { AppExecution } from "../app/runtime.js";
  import { getAppRuntime } from "../app/runtime-context.js";
  import {
    buildActivityFilterTypes,
    DEFAULT_EVENT_TYPES,
    isActivityItemTypeEnabled,
    type ActivityItemType,
    type TimeRange,
    type ViewMode,
  } from "../stores/activity.svelte.js";
  import { getStores, getNavigate, getSidebar } from "../context.js";
  import ActivityThreaded from "./ActivityThreaded.svelte";
  import ActivityFilters from "./ActivityFilters.svelte";
  import { ScrollBox } from "@kenn-io/kit-ui";
  import {
    isDefaultBranchActivity,
    isDefaultBranchCommitActivity,
    isDefaultBranchForcePushActivity,
    isCollapsedActivityRow,
    isClosedOrMergedActivity,
    collapseActivityRuns,
    notificationReasonLabel,
    shortSha,
  } from "./activityRows.js";
  import {
    localDateLabel,
    parseAPITimestamp,
  } from "../utils/time.js";
  import {
    createRepoLabelFormatter,
    type RepoLabelIdentity,
  } from "../utils/repo-label.js";
  import { hashColor } from "@kenn-io/kit-ui";
  import { Chip, type ChipTone } from "@kenn-io/kit-ui";
  import ItemKindChip from "./shared/ItemKindChip.svelte";
  import ItemStateChip from "./shared/ItemStateChip.svelte";
  import WorkspaceIndicator from "./shared/WorkspaceIndicator.svelte";
  import AgentStatusIndicator from "./shared/AgentStatusIndicator.svelte";
  import ArrowUpRightIcon from "@lucide/svelte/icons/arrow-up-right";
  import CheckIcon from "@lucide/svelte/icons/check";
  import ChevronsDownUpIcon from "@lucide/svelte/icons/chevrons-down-up";
  import ChevronsUpDownIcon from "@lucide/svelte/icons/chevrons-up-down";

  const { activity, settings, sync, grouping } = getStores();
  const runtime = getAppRuntime();
  const navigate = getNavigate();
  const { isEmbedded } = getSidebar();

  interface Props {
    onSelectItem?: (item: ActivityItem) => void;
    onSelectBranchCommit?: (item: ActivityItem) => void;
    compact?: boolean;
    selectedItem?: SelectedActivityRef | null;
    selectedBranchCommit?: SelectedBranchCommitRef | null;
  }

  type SelectedActivityRef = {
    itemType: "pr" | "issue";
    owner: string;
    name: string;
    number: number;
    provider?: string | undefined;
    platformHost?: string | undefined;
    repoPath?: string | undefined;
  };

  type SelectedBranchCommitRef = {
    owner: string;
    name: string;
    commitSha: string;
    provider?: string | undefined;
    platformHost?: string | undefined;
    repoPath?: string | undefined;
  };

  let {
    onSelectItem,
    onSelectBranchCommit,
    compact = false,
    selectedItem = null,
    selectedBranchCommit = null,
  }: Props = $props();

  let searchInput = $state("");
  let debounceExecution: AppExecution<void, never> | null = null;

  const EVENT_TYPES = DEFAULT_EVENT_TYPES;
  type EventType = (typeof EVENT_TYPES)[number];

  const EVENT_LABELS: Record<EventType, string> = {
    comment: "Comments",
    review: "Reviews",
    commit: "Commits",
    force_push: "Force pushes",
  };

  const EVENT_COLORS: Record<EventType, string> = {
    comment: "var(--accent-amber)",
    review: "var(--accent-green)",
    commit: "var(--accent-teal)",
    force_push: "var(--accent-red)",
  };

  const BOT_SUFFIXES = ["[bot]", "-bot", "bot"];

  function isBot(author: string): boolean {
    const lower = author.toLowerCase();
    return BOT_SUFFIXES.some((s) => lower.endsWith(s));
  }

  const popoverFilterCount = $derived(
    (activity.getActivityAuthor() ? 1 : 0)
    + (EVENT_TYPES.length - activity.getEnabledEvents().size)
    + (activity.getShowNotifications() ? 0 : 1)
    + (activity.getHideClosedMerged() ? 1 : 0)
    + (activity.getHideBots() ? 1 : 0)
    + (activity.getHideDefaultBranchActivity() ? 1 : 0)
    + (grouping.getHideOrgName() ? 1 : 0)
    + (activity.getInvolvesMe() ? 1 : 0)
    + (activity.getUnassigned() ? 1 : 0),
  );

  let unsubSync: (() => void) | undefined;

  onMount(() => {
    activity.initializeFromMount();
    searchInput = activity.getActivitySearch() ?? "";
    activity.loadActivity(true);
    activity.startActivityPolling();
    unsubSync = sync.subscribeSyncComplete(() => {
      runtime.runCommand(activity.reconcileActivityEffect(), {
        operation: "reconcile activity after sync",
        safeContext: {},
        onFailure: () => {},
      });
    });
  });

  onDestroy(() => {
    activity.stopActivityPolling();
    unsubSync?.();
    debounceExecution?.interrupt();
  });

  function applyFilters(): void {
    activity.setActivityFilterTypes(buildActivityFilterTypes(
      activity.getEnabledItemTypes(),
      activity.getEnabledEvents(),
      activity.getHideDefaultBranchActivity(),
      activity.getShowNotifications(),
    ));
    activity.syncToURL();
    activity.loadActivity();
  }

  function toggleItemType(itemType: ActivityItemType): void {
    const next = new Set(activity.getEnabledItemTypes());
    if (next.has(itemType)) next.delete(itemType);
    else next.add(itemType);
    activity.setEnabledItemTypes(next);
    applyFilters();
  }

  function toggleEvent(evt: EventType): void {
    const current = activity.getEnabledEvents();
    const next = new Set(current);
    // Deselecting the last event type is valid: it hides every filterable
    // event row while PR timeline commits and the separate Notifications
    // toggle remain independent. buildActivityFilterTypes encodes the empty
    // set explicitly so the state round-trips through the URL rather than
    // collapsing back to "show everything".
    if (next.has(evt)) next.delete(evt);
    else next.add(evt);
    activity.setEnabledEvents(next);
    applyFilters();
  }

  function handleTimeRangeChange(range: TimeRange): void {
    activity.setTimeRange(range);
    activity.syncToURL();
    activity.loadActivity();
  }

  function handleViewModeChange(mode: ViewMode): void {
    activity.setViewMode(mode);
    activity.syncToURL();
    activity.loadActivity();
  }

  function handleRollUpCommitsChange(value: boolean): void {
    activity.setRollUpCommits(value);
    activity.syncToURL();
  }

  const TIME_RANGES: { value: TimeRange; label: string }[] = [
    { value: "24h", label: "24h" },
    { value: "7d", label: "7d" },
    { value: "30d", label: "30d" },
    { value: "90d", label: "90d" },
  ];

  function handleSearchInput(val: string): void {
    searchInput = val;
    debounceExecution?.interrupt();
    debounceExecution = runtime.runCommand(
      Effect.sleep("300 millis").pipe(
        Effect.andThen(
          Effect.sync(() => {
            debounceExecution = null;
            activity.setActivitySearch(val || undefined);
            activity.syncToURL();
            activity.loadActivity();
          }),
        ),
      ),
      {
        operation: "debounce activity search",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  const authorOptions = $derived.by<TypeaheadOption[]>(() =>
    activity.getActivityAuthors().map((author) => ({
      name: author,
      label: author,
    })),
  );

  function handleAuthorSelect(author: string): void {
    activity.setActivityAuthor(author || undefined);
    activity.syncToURL();
    activity.loadActivity();
  }

  function eventLabel(item: ActivityItem): string {
    switch (item.activity_type) {
      case "new_pr": return "Opened";
      case "new_issue": return "Opened";
      case "comment": return "Comment";
      case "review": return "Review";
      case "commit": return "Commit";
      case "force_push": return "Force-pushed";
      case "default_branch_commit": return "Commit";
      case "default_branch_force_push": return "Force-pushed";
      case "notification": return notificationReasonLabel(item.body_preview);
      default: return item.activity_type;
    }
  }

  function hasStateChip(item: ActivityItem): boolean {
    if (isDefaultBranchActivity(item)) return false;
    return item.item_state === "merged" || item.item_state === "closed";
  }

  function branchName(item: ActivityItem): string {
    return item.branch_name || "default branch";
  }

  function activityAuthor(item: ActivityItem): string {
    return item.author_name || item.author;
  }

  function branchActivityTitle(item: ActivityItem): string {
    if (isDefaultBranchForcePushActivity(item)) {
      const before = shortSha(item.before_sha);
      const after = shortSha(item.after_sha);
      if (before && after) return `${before} -> ${after}`;
    }
    return item.body_preview || shortSha(item.commit_sha) || "Commit";
  }

  function branchActivityDetail(item: ActivityItem): string {
    if (isDefaultBranchCommitActivity(item)) {
      return shortSha(item.commit_sha);
    }
    return branchActivityTitle(item);
  }

  function activityLink(item: ActivityItem): string {
    return item.activity_url || item.item_url;
  }

  function handleLinkClick(e: Event, url: string): void {
    e.stopPropagation();
    if (url) window.open(url, "_blank", "noopener");
  }

  const displayItems = $derived.by(() => {
    let result = activity.getActivityItems().filter((item) =>
      isActivityItemTypeEnabled(item.item_type, activity.getEnabledItemTypes())
    );
    if (activity.getHideClosedMerged()) {
      result = result.filter((it) => !isClosedOrMergedActivity(it));
    }
    if (activity.getHideBots()) {
      result = result.filter((it) => !isBot(it.author));
    }
    if (activity.getHideDefaultBranchActivity()) {
      result = result.filter((it) => !isDefaultBranchActivity(it));
    }
    return result;
  });

  const visibleWorkspaceActivity = $derived.by(() => {
    if (!activity.getUseWorkspaceActivityForRecency()) return [];
    let result: WorkspaceActivitySubject[] = activity.getWorkspaceActivity().filter((subject) =>
      isActivityItemTypeEnabled(subject.item_type, activity.getEnabledItemTypes())
    );
    if (activity.getHideClosedMerged()) {
      result = result.filter((subject) => subject.item_state !== "closed" && subject.item_state !== "merged");
    }
    if (activity.getHideBots()) {
      result = result.filter((subject) => !isBot(subject.item_author ?? ""));
    }
    return result;
  });

  const visibleItemActivity = $derived.by(() => {
    let result: ActivitySubject[] = activity.getItemActivity().filter((subject) =>
      isActivityItemTypeEnabled(subject.item_type, activity.getEnabledItemTypes())
    );
    if (activity.getHideClosedMerged()) {
      result = result.filter((subject) => subject.item_state !== "closed" && subject.item_state !== "merged");
    }
    if (activity.getHideBots()) {
      result = result.filter((subject) => !isBot(subject.item_author ?? ""));
    }
    return result;
  });

  const flatRows = $derived.by(() => collapseActivityRuns(displayItems, {
    rollUpCommits: activity.getRollUpCommits(),
    rollUpNonCommitActivity: false,
  }));

  const repoLabelFormatter = $derived.by(() =>
    createRepoLabelFormatter(
      displayItems.map(activityRepoIdentity),
      { showOrgNames: !grouping.getHideOrgName() },
    ),
  );

  function activityRepoIdentity(item: ActivityItem): RepoLabelIdentity {
    return {
      provider: item.repo?.provider ?? "",
      platformHost: item.repo?.platform_host ?? item.platform_host,
      owner: item.repo?.owner ?? item.repo_owner,
      name: item.repo?.name ?? item.repo_name,
      repoPath: item.repo?.repo_path,
    };
  }

  function repoLabel(item: ActivityItem): string {
    return repoLabelFormatter.format(activityRepoIdentity(item));
  }

  function resetFilters(): void {
    activity.setActivityAuthor(undefined);
    activity.setEnabledEvents(new Set(EVENT_TYPES));
    activity.setShowNotifications(true);
    activity.setHideClosedMerged(false);
    activity.setHideBots(false);
    activity.setHideDefaultBranchActivity(false);
    grouping.setHideOrgName(false);
    activity.setInvolvesMe(false);
    activity.setUnassigned(false);
    applyFilters();
  }

  const activityFilterSections = $derived.by(() => [
    {
      title: "Event types",
      items: [
        ...EVENT_TYPES.map((evt) => ({
          id: evt,
          label: EVENT_LABELS[evt],
          active: activity.getEnabledEvents().has(evt),
          color: EVENT_COLORS[evt],
          onSelect: () => toggleEvent(evt),
        })),
        {
          id: "notification",
          label: "Notifications",
          active: activity.getShowNotifications(),
          color: "var(--accent-blue)",
          onSelect: () => {
            activity.setShowNotifications(!activity.getShowNotifications());
            applyFilters();
          },
        },
      ],
    },
    {
      title: "Visibility",
      items: [
        {
          id: "involves-me",
          label: "Involves me",
          active: activity.getInvolvesMe(),
          color: "var(--accent-blue)",
          onSelect: () => {
            activity.setInvolvesMe(!activity.getInvolvesMe());
            activity.loadActivity();
          },
        },
        {
          id: "unassigned",
          label: "Unassigned",
          active: activity.getUnassigned(),
          color: "var(--accent-blue)",
          onSelect: () => {
            activity.setUnassigned(!activity.getUnassigned());
            activity.loadActivity();
          },
        },
        {
          id: "hide-default-branch",
          label: "Hide default-branch activity",
          active: activity.getHideDefaultBranchActivity(),
          color: "var(--accent-teal)",
          onSelect: () => {
            activity.setHideDefaultBranchActivity(
              !activity.getHideDefaultBranchActivity(),
            );
            applyFilters();
          },
        },
        {
          id: "hide-closed-merged",
          label: "Hide closed/merged",
          active: activity.getHideClosedMerged(),
          color: "var(--accent-red)",
          onSelect: () => {
            activity.setHideClosedMerged(
              !activity.getHideClosedMerged(),
            );
            activity.loadActivity();
          },
        },
        {
          id: "hide-bots",
          label: "Hide bots",
          active: activity.getHideBots(),
          color: "var(--accent-purple)",
          onSelect: () => {
            activity.setHideBots(!activity.getHideBots());
            activity.loadActivity();
          },
        },
        {
          id: "hide-org-name",
          label: "Hide org name",
          active: grouping.getHideOrgName(),
          color: "var(--accent-blue)",
          onSelect: () => {
            grouping.setHideOrgName(!grouping.getHideOrgName());
          },
        },
      ],
    },
  ]);

  const currentViewDetail = $derived.by(() => {
    const mode = activity.getViewMode() === "flat" ? "Flat" : "Threaded";
    return `${mode} · ${activity.getTimeRange()}`;
  });

  const collapseThreads = $derived(activity.getCollapseThreads());

  const collapseAllLabel = $derived(
    collapseThreads ? "Expand all" : "Collapse all",
  );

  const filterSections = $derived.by(() => [
    {
      title: "View",
      selectionMode: "single" as const,
      items: [
        {
          id: "view-flat",
          label: "Flat",
          active: activity.getViewMode() === "flat",
          closeOnSelect: true,
          onSelect: () => handleViewModeChange("flat"),
        },
        {
          id: "view-threaded",
          label: "Threaded",
          active: activity.getViewMode() === "threaded",
          closeOnSelect: true,
          onSelect: () => handleViewModeChange("threaded"),
        },
      ],
    },
    {
      title: "Time range",
      selectionMode: "single" as const,
      items: TIME_RANGES.map((range) => ({
        id: `range-${range.value}`,
        label: range.label,
        active: activity.getTimeRange() === range.value,
        closeOnSelect: true,
        onSelect: () => handleTimeRangeChange(range.value),
      })),
    },
    {
      title: "Commits",
      items: [
        {
          id: "roll-up-commits",
          label: "Roll up commits",
          active: activity.getRollUpCommits(),
          onSelect: () => handleRollUpCommitsChange(!activity.getRollUpCommits()),
        },
      ],
    },
    ...(activity.getViewMode() === "threaded"
      ? [
          {
            title: "Grouping",
            selectionMode: "single" as const,
            items: [
              {
                id: "group-by-repo",
                label: "By repo",
                active: grouping.getGroupByRepo(),
                closeOnSelect: true,
                onSelect: () => grouping.setGroupByRepo(true),
              },
              {
                id: "group-all",
                label: "All",
                active: !grouping.getGroupByRepo(),
                closeOnSelect: true,
                onSelect: () => grouping.setGroupByRepo(false),
              },
            ],
          },
        ]
      : []),
    ...activityFilterSections,
  ]);

  function eventClass(type: string): string {
    switch (type) {
      case "comment": return "evt-comment";
      case "review": return "evt-review";
      case "commit": return "evt-commit";
      case "default_branch_commit": return "evt-commit";
      case "force_push": return "evt-force-push";
      case "default_branch_force_push": return "evt-force-push";
      case "notification": return "evt-notification";
      default: return "";
    }
  }

  function eventChipTone(type: string): ChipTone {
    return type === "comment" ? "warning"
      : type === "review" ? "success"
      : type === "commit" || type === "default_branch_commit" ? "workspace"
      : type === "force_push" || type === "default_branch_force_push" ? "danger"
      : type === "notification" ? "info"
      : "muted";
  }

  function eventChipClass(type: string): string {
    return `evt-label ${eventClass(type)}`;
  }

  function relativeTime(iso: string): string {
    const diff = Date.now() - parseAPITimestamp(iso).getTime();
    const mins = Math.floor(diff / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    if (days < 7) return `${days}d ago`;
    return localDateLabel(iso);
  }

  function handleRowClick(item: ActivityItem): void {
    if (isDefaultBranchActivity(item)) {
      if (isDefaultBranchCommitActivity(item)) {
        onSelectBranchCommit?.(item);
        return;
      }
      const url = activityLink(item);
      if (url) window.open(url, "_blank", "noopener");
      return;
    }
    // Notifications that point at a tracked PR/issue open in the detail
    // pane like any other row; those for other subjects (discussions,
    // releases, CI) have no in-app detail, so follow their web URL.
    if (item.activity_type === "notification" && !opensInDetailPane(item)) {
      const url = activityLink(item);
      if (url) window.open(url, "_blank", "noopener");
      return;
    }
    onSelectItem?.(item);
  }

  function opensInDetailPane(item: ActivityItem): boolean {
    return (item.item_type === "pr" || item.item_type === "issue") && item.item_number > 0;
  }

  function isUnreadNotification(item: ActivityItem): boolean {
    return item.activity_type === "notification" && item.item_state === "unread";
  }

  function handleMarkSeen(e: Event, item: ActivityItem): void {
    e.stopPropagation();
    activity.markNotificationSeen(item);
  }

  function isSelectedActivityItem(item: ActivityItem): boolean {
    if (isDefaultBranchActivity(item)) {
      if (!isDefaultBranchCommitActivity(item)) return false;
      const selected = selectedBranchCommit;
      if (!selected) return false;
      return selected.commitSha === item.commit_sha
        && selected.owner === item.repo_owner
        && selected.name === item.repo_name
        && (!selected.provider
          || selected.provider === item.repo?.provider)
        && (!selected.repoPath
          || selected.repoPath === item.repo?.repo_path)
        && (!selected.platformHost
          || selected.platformHost === item.platform_host);
    }
    return selectedItem?.itemType === item.item_type
      && selectedItem.owner === item.repo_owner
      && selectedItem.name === item.repo_name
      && selectedItem.number === item.item_number
      && (!selectedItem.provider
        || selectedItem.provider === item.repo?.provider)
      && (!selectedItem.repoPath
        || selectedItem.repoPath === item.repo?.repo_path)
      && (!selectedItem.platformHost
        || selectedItem.platformHost === item.platform_host);
  }

</script>

<div
  class="activity-feed"
  class:activity-feed--compact={compact}
  data-selected-item={selectedItem
    ? `${selectedItem.itemType}:${selectedItem.owner}/${selectedItem.name}/${selectedItem.number}`
    : undefined}
>
  <div class="controls-bar">
    <div class="filter-group" aria-label="Item types">
      <Toggle
        checked={activity.getEnabledItemTypes().has("pr")}
        label="PRs"
        onchange={() => toggleItemType("pr")}
      />
      <Toggle
        checked={activity.getEnabledItemTypes().has("issue")}
        label="Issues"
        onchange={() => toggleItemType("issue")}
      />
    </div>

    <div class="filters-wrap">
      <ActivityFilters
        author={activity.getActivityAuthor() ?? ""}
        {authorOptions}
        authorLoading={activity.isActivityAuthorsLoading()}
        authorError={activity.getActivityAuthorsError() ?? ""}
        detail={currentViewDetail}
        {compact}
        badgeCount={popoverFilterCount}
        sections={filterSections}
        onAuthorSelect={handleAuthorSelect}
        onReset={resetFilters}
      />
    </div>

    {#if !compact && activity.getActivityAuthor()}
      <div class="author-chip-wrap">
        <Chip
          size="sm"
          tone="info"
          uppercase={false}
          interactive
          ariaLabel={`Clear author filter ${activity.getActivityAuthor()}`}
          onclick={() => handleAuthorSelect("")}
        >
          {activity.getActivityAuthor()}
          {#snippet trailing()}<span aria-hidden="true">×</span>{/snippet}
        </Chip>
      </div>
    {/if}

    {#if activity.getViewMode() === "threaded"}
      <button
        class="collapse-all-btn"
        type="button"
        aria-label={collapseAllLabel}
        title={collapseAllLabel}
        onclick={() =>
          collapseThreads
            ? activity.expandAllThreads()
            : activity.collapseAllThreads()}
      >
        {#if collapseThreads}
          <ChevronsUpDownIcon size="14" strokeWidth="2" aria-hidden="true" />
        {:else}
          <ChevronsDownUpIcon size="14" strokeWidth="2" aria-hidden="true" />
        {/if}
        <span class="collapse-all-label">{collapseAllLabel}</span>
      </button>
    {/if}

    <div class="search-wrap">
      <SearchInput
        bind:value={searchInput}
        size="sm"
        block
        placeholder="Search..."
        ariaLabel="Search activity"
        oninput={handleSearchInput}
      />
    </div>
  </div>

  {#if activity.getActivityError()}
    <div class="error-banner">{activity.getActivityError()}</div>
  {/if}
  {#if activity.getThreadLoadError()}
    <div class="error-banner error-banner--action" role="alert">
      <span>{activity.getThreadLoadError()}</span>
      <button type="button" onclick={activity.retryFailedThreadLoads}>Retry thread history</button>
    </div>
  {/if}

  {#if settings.isSettingsLoaded() && !settings.hasConfiguredRepos()}
    <ScrollBox label="Activity feed">
    <div class="table-container">
      <EmptyState title="No repositories configured.">
        {#if !isEmbedded()}<button class="settings-link" onclick={() => navigate("/settings")}>Add one in Settings</button>{/if}
      </EmptyState>
    </div>
    </ScrollBox>
  {:else if activity.getViewMode() === "threaded"}
    {#if displayItems.length === 0 && visibleItemActivity.length === 0 && visibleWorkspaceActivity.length === 0 && activity.isActivityLoading()}
      <ScrollBox label="Activity feed">
      <div class="table-container">
        <div class="loading-placeholder">
          <Spinner size={14} label="Loading activity" />
          Loading...
        </div>
      </div>
      </ScrollBox>
    {:else}
      <ActivityThreaded
        items={displayItems}
        itemActivity={visibleItemActivity}
        workspaceActivity={visibleWorkspaceActivity}
        {onSelectItem}
        {onSelectBranchCommit}
        {compact}
        {selectedItem}
        {selectedBranchCommit}
      />
    {/if}
  {:else}
    <ScrollBox label="Activity feed">
    <div class="table-container">
      {#if compact}
        <div class="activity-compact-list">
          {#each flatRows as row (row.id)}
            {#if isCollapsedActivityRow(row)}
              <button
                class="activity-compact-row collapsed-row"
                class:selected={isSelectedActivityItem(row.representative)}
                onclick={() => handleRowClick(row.representative)}
                type="button"
              >
                <span class="compact-row-top">
                  {#if isDefaultBranchActivity(row.representative)}
                    <Chip size="xs" tone="muted" uppercase={false} class="branch-chip">Branch</Chip>
                    <span class="branch-name">{branchName(row.representative)}</span>
                  {:else}
                    <ItemKindChip kind={row.representative.item_type} />
                    <span class="item-number">#{row.representative.item_number}</span>
                    {#if row.representative.workspace}
                      <WorkspaceIndicator
                        status={row.representative.workspace.status}
                        size={12}
                      />
                    {/if}
                  {/if}
                  <span class="compact-status">
                    <AgentStatusIndicator state={row.representative.workspace?.agent_state} />
                    <span class="compact-time">{relativeTime(row.latest)}</span>
                  </span>
                </span>
                <span class="compact-title">
                  {#if isDefaultBranchActivity(row.representative)}
                    {row.count} commits on {branchName(row.representative)}
                  {:else}
                    {row.representative.item_title}
                  {/if}
                </span>
                <span class="compact-meta">
                  <span>{repoLabel(row.representative)}</span>
                  <Chip
                    size="xs"
                    uppercase={false}
                    tone="workspace" class="evt-label evt-commit"
                  >{row.count} commits</Chip>
                  <span>{row.author}</span>
                </span>
              </button>
            {:else}
              {@const unread = isUnreadNotification(row)}
              <div class="compact-row-slot">
                <button
                  class="activity-compact-row"
                  class:selected={isSelectedActivityItem(row)}
                  class:has-seen-action={unread}
                  onclick={() => handleRowClick(row)}
                  type="button"
                >
                  <span class="compact-row-top">
                    {#if isDefaultBranchActivity(row)}
                      <Chip size="xs" tone="muted" uppercase={false} class="branch-chip">Branch</Chip>
                      <span class="branch-name">{branchName(row)}</span>
                    {:else}
                      <ItemKindChip kind={row.item_type} />
                      <span class="item-number">#{row.item_number}</span>
                      {#if row.workspace}
                        <WorkspaceIndicator status={row.workspace.status} size={12} />
                      {/if}
                      {#if hasStateChip(row)}
                        <ItemStateChip state={row.item_state} />
                      {/if}
                    {/if}
                    <span class="compact-status">
                      <AgentStatusIndicator state={row.workspace?.agent_state} />
                      <span class="compact-time">{relativeTime(row.created_at)}</span>
                    </span>
                  </span>
                  <span class="compact-title">
                    {isDefaultBranchActivity(row) ? branchActivityTitle(row) : row.item_title}
                  </span>
                  <span class="compact-meta">
                    <span>{repoLabel(row)}</span>
                    {#if isDefaultBranchActivity(row) && branchActivityDetail(row)}
                      <span class="sha">{branchActivityDetail(row)}</span>
                    {/if}
                    <Chip
                      size="xs"
                      uppercase={false}
                      tone={eventChipTone(row.activity_type)}
                      class={eventChipClass(row.activity_type)}
                    >{eventLabel(row)}</Chip>
                    <span>{activityAuthor(row)}</span>
                  </span>
                </button>
                {#if unread}
                  <button
                    class="link-btn mark-seen-btn compact-mark-seen"
                    type="button"
                    aria-label="Mark notification seen"
                    title="Mark seen"
                    onclick={(e) => handleMarkSeen(e, row)}
                  >
                    <CheckIcon size="14" strokeWidth="2" aria-hidden="true" />
                  </button>
                {/if}
              </div>
            {/if}
          {/each}
        </div>
      {:else}
        <div class="activity-table" aria-label="Activity events">
          <div class="activity-column-headers">
            <span class="cell cell--caret-spacer" aria-hidden="true"></span>
            <span class="cell cell--type">Type</span>
            <span class="cell cell--repo col-repo">Repo</span>
            <span class="cell cell--author col-author">Author</span>
            <span class="cell cell--title">Item</span>
            <span class="cell cell--time col-when">When</span>
            <span class="cell cell--link" aria-hidden="true"></span>
          </div>
          {#each flatRows as row (row.id)}
            {@const rep = isCollapsedActivityRow(row) ? row.representative : row}
            {@const repoStyle =
              `color: ${hashColor(`${rep.repo_owner}/${rep.repo_name}`)}; `
              + `background: color-mix(in srgb, `
              + `${hashColor(`${rep.repo_owner}/${rep.repo_name}`)} 15%, transparent);`}
            <!-- svelte-ignore a11y_click_events_have_key_events -->
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
              class="activity-row"
              class:collapsed-row={isCollapsedActivityRow(row)}
              onclick={() =>
                handleRowClick(isCollapsedActivityRow(row) ? row.representative : row)}
            >
              <span class="cell cell--caret-spacer"></span>
              {#if isCollapsedActivityRow(row)}
                <span class="cell cell--type">
                  {#if isDefaultBranchActivity(row.representative)}
                    <Chip size="sm" tone="muted" uppercase={false} class="branch-chip">Branch</Chip>
                  {:else}
                    <ItemKindChip kind={row.representative.item_type} />
                  {/if}
                  <Chip
                    size="sm"
                    uppercase={false}
                    tone="workspace" class="evt-label evt-commit"
                  >{row.count} commits</Chip>
                </span>
                <span class="cell cell--repo col-repo">
                  <Chip
                    size="sm"
                    uppercase={false}
                    class="repo-chip repo-tag"
                    style={repoStyle}
                  >
                    <span class="repo-chip__label"
                      >{repoLabel(row.representative)}</span>
                  </Chip>
                </span>
                <span class="cell cell--author col-author">{row.author}</span>
                <span class="cell cell--title">
                  {#if isDefaultBranchActivity(row.representative)}
                    <span class="item-ref">{branchName(row.representative)}</span>
                    <span class="item-title">{row.count} commits</span>
                  {:else}
                    <span class="item-ref">#{row.representative.item_number}</span>
                    {#if row.representative.workspace}
                      <WorkspaceIndicator
                        status={row.representative.workspace.status}
                        size={12}
                      />
                    {/if}
                    <span class="item-title">{row.representative.item_title}</span>
                    <AgentStatusIndicator state={row.representative.workspace?.agent_state} />
                  {/if}
                </span>
                <span class="cell cell--time col-when"
                  >{relativeTime(row.earliest)} – {relativeTime(row.latest)}</span>
                <span class="cell cell--link">
                  {#if activityLink(row.representative)}
                    <button
                      class="link-btn"
                      type="button"
                      aria-label="Open activity in provider"
                      title="Open activity"
                      onclick={(e) => handleLinkClick(e, activityLink(row.representative))}
                    >
                      <ArrowUpRightIcon size="14" strokeWidth="2" aria-hidden="true" />
                    </button>
                  {/if}
                </span>
              {:else}
                <span class="cell cell--type">
                  {#if isDefaultBranchActivity(row)}
                    <Chip size="sm" tone="muted" uppercase={false} class="branch-chip">Branch</Chip>
                  {:else}
                    <ItemKindChip kind={row.item_type} />
                  {/if}
                  <Chip
                    size="sm"
                    uppercase={false}
                    tone={eventChipTone(row.activity_type)}
                    class={eventChipClass(row.activity_type)}
                  >{eventLabel(row)}</Chip>
                </span>
                <span class="cell cell--repo col-repo">
                  <Chip
                    size="sm"
                    uppercase={false}
                    class="repo-chip repo-tag"
                    style={repoStyle}
                  >
                    <span class="repo-chip__label"
                      >{repoLabel(row)}</span>
                  </Chip>
                </span>
                <span class="cell cell--author col-author">{activityAuthor(row)}</span>
                <span class="cell cell--title">
                  {#if isDefaultBranchActivity(row)}
                    <span class="item-ref">{branchName(row)}</span>
                    {#if branchActivityDetail(row)}
                      <span class="sha">{branchActivityDetail(row)}</span>
                    {/if}
                    <span class="item-title">{branchActivityTitle(row)}</span>
                  {:else}
                    {#if hasStateChip(row)}
                      <ItemStateChip state={row.item_state} />
                    {/if}
                    <span class="item-ref">#{row.item_number}</span>
                    {#if row.workspace}
                      <WorkspaceIndicator status={row.workspace.status} size={12} />
                    {/if}
                    <span class="item-title">{row.item_title}</span>
                    <AgentStatusIndicator state={row.workspace?.agent_state} />
                  {/if}
                </span>
                <span class="cell cell--time col-when">{relativeTime(row.created_at)}</span>
                <span class="cell cell--link">
                  {#if isUnreadNotification(row)}
                    <button
                      class="link-btn mark-seen-btn"
                      type="button"
                      aria-label="Mark notification seen"
                      title="Mark seen"
                      onclick={(e) => handleMarkSeen(e, row)}
                    >
                      <CheckIcon size="14" strokeWidth="2" aria-hidden="true" />
                    </button>
                  {/if}
                  {#if activityLink(row)}
                    <button
                      class="link-btn"
                      type="button"
                      aria-label="Open activity in provider"
                      title="Open activity"
                      onclick={(e) => handleLinkClick(e, activityLink(row))}
                    >
                      <ArrowUpRightIcon size="14" strokeWidth="2" aria-hidden="true" />
                    </button>
                  {/if}
                </span>
              {/if}
            </div>
          {/each}
        </div>
      {/if}

      {#if flatRows.length === 0 && !activity.isActivityLoading()}
        <EmptyState title="No activity found" />
      {/if}
    </div>
    </ScrollBox>
  {/if}

  {#if activity.isActivityCapped()}
    <div class="capped-notice event-capped-notice">
      Showing most recent 5,000 events. Narrow the time range or use filters to see more.
    </div>
  {/if}

  {#if activity.isItemActivityCapped()}
    <div class="capped-notice item-activity-capped-notice">
      Showing the 5,000 most recently active pull requests and issues. Item totals may be incomplete; narrow the time
      range or item filters to see fewer results.
    </div>
  {/if}

</div>

<style>
  .activity-feed {
    display: flex;
    flex-direction: column;
    height: 100%;
    overflow: hidden;
    container-type: inline-size;
  }

  .loading-placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    padding: var(--space-8) var(--space-6);
    color: var(--text-muted);
    font-size: var(--font-size-md);
  }

  .controls-bar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    flex-shrink: 0;
  }

  .filter-group {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .search-wrap {
    margin-left: auto;
    width: 180px;
  }

  .author-chip-wrap {
    min-width: 0;
  }

  .collapse-all-btn {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 3px 8px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    cursor: pointer;
  }

  .collapse-all-btn:hover {
    color: var(--text-primary);
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .collapse-all-btn:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
  }

  .activity-feed--compact .controls-bar {
    align-items: stretch;
    flex-wrap: wrap;
    gap: 8px;
    padding: 8px;
  }

  .activity-feed--compact .filter-group {
    order: 2;
    flex: 0 0 auto;
  }

  .activity-feed--compact .search-wrap {
    order: 1;
    flex: 1 0 100%;
    width: 100%;
    margin-left: 0;
  }

  /* In the narrow side pane the labeled button wraps to its own row and
     stacks awkwardly, so collapse to an icon-only control there. The
     aria-label/title keep the accessible name intact. */
  .activity-feed--compact .collapse-all-label {
    display: none;
  }

  .activity-feed--compact .filters-wrap {
    order: 3;
    min-width: 0;
    max-width: min(320px, 100%);
    flex: 0 1 auto;
  }

  .activity-feed--compact .collapse-all-btn {
    order: 4;
    flex: 0 0 auto;
  }

  @container (max-width: 480px) {
    .activity-feed--compact .filters-wrap {
      flex: 1 1 0;
    }
  }

  .table-container {
    padding: 0 16px;
  }

  .activity-feed--compact .table-container {
    padding: 0;
  }

  .activity-compact-list {
    display: flex;
    flex-direction: column;
  }

  /* The mark-seen action sits beside the row rather than inside it: a
     <button> cannot nest another button, so the row body stays a real
     <button> (keyboard focus + Enter/Space) and the action is an absolutely
     positioned sibling in this relative slot. The row reserves a right
     gutter via .has-seen-action so the control never overlaps content. */
  .compact-row-slot {
    position: relative;
    display: flex;
  }

  .compact-row-slot > .activity-compact-row {
    flex: 1;
    min-width: 0;
  }

  .compact-mark-seen {
    position: absolute;
    top: 50%;
    right: 8px;
    transform: translateY(-50%);
  }

  .activity-compact-row {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: var(--space-1);
    width: 100%;
    min-height: 62px;
    padding: 8px 10px;
    border-bottom: 1px solid var(--border-muted);
    text-align: left;
    color: inherit;
    background: transparent;
  }

  .activity-compact-row.has-seen-action {
    padding-right: 36px;
  }

  .activity-compact-row:hover {
    background: var(--bg-surface-hover);
  }

  .activity-compact-row.selected {
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
    box-shadow: inset 3px 0 0 var(--accent-blue);
  }

  .compact-row-top,
  .compact-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .compact-title {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 500;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .compact-status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    margin-left: auto;
  }

  .compact-time {
    margin-left: auto;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    flex-shrink: 0;
  }

  .compact-meta {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .compact-meta > span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* The flat view shares the threaded view's grid layout so toggling between
   * modes doesn't shift the columns. The first column is an 18px spacer that
   * lines up with the threaded view's chevron caret so the type chip starts
   * at the same x-coordinate in both layouts. Widths come from the same CSS
   * custom properties so the column caps stay in lockstep. */
  .activity-table {
    display: grid;
    grid-template-columns:
      18px
      fit-content(140px)
      fit-content(var(--threaded-col-repo-max, 220px))
      fit-content(var(--threaded-col-author-max, 140px))
      minmax(0, 1fr)
      auto
      24px;
    column-gap: 6px;
  }

  .cell--caret-spacer {
    width: 18px;
  }

  .activity-column-headers {
    display: grid;
    grid-template-columns: subgrid;
    grid-column: 1 / -1;
    align-items: center;
    padding: 6px 0 4px;
    font-size: var(--font-size-2xs);
    font-weight: 500;
    text-transform: uppercase;
    letter-spacing: 0.5px;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-default);
    position: sticky;
    top: 0;
    background: var(--bg-primary);
    z-index: 1;
  }

  .activity-row {
    display: grid;
    grid-template-columns: subgrid;
    grid-column: 1 / -1;
    align-items: center;
    padding: 5px 0;
    cursor: pointer;
    border-bottom: 1px solid var(--border-muted);
    transition: background 0.1s;
  }

  .activity-row:hover {
    background: var(--bg-surface-hover);
  }

  .collapsed-row {
    background: var(--bg-inset);
  }

  .cell {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell--type {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    overflow: visible;
  }

  .cell--repo {
    display: inline-flex;
    align-items: center;
    min-width: 0;
    font-size: var(--font-size-sm);
  }

  .cell--repo :global(.repo-chip) {
    min-width: 0;
    max-width: 100%;
  }

  .cell--repo :global(.repo-chip .repo-chip__label) {
    display: block;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell--author {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
  }

  .cell--title {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .item-ref {
    font-size: var(--font-size-sm);
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .item-title {
    font-size: var(--font-size-sm);
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }

  .cell--time {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    text-align: right;
  }

  .cell--link {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    overflow: visible;
  }

  .link-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
    background: none;
    border: 0;
    padding: 2px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: color 0.1s, background 0.1s;
  }

  .link-btn:hover {
    color: var(--accent-blue);
    background: var(--bg-surface-hover);
  }

  .link-btn:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
  }

  .mark-seen-btn {
    color: var(--accent-blue);
  }

  :global(.evt-label) {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
  }

  :global(.evt-label.evt-comment) { color: var(--accent-amber); }
  :global(.evt-label.evt-review) { color: var(--accent-green); }
  :global(.evt-label.evt-commit) { color: var(--accent-teal); }
  :global(.evt-label.evt-force-push) { color: var(--accent-red); }
  :global(.evt-label.evt-notification) { color: var(--accent-blue); }

  .sha {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  /* Compact-list-only labels (rendered by the sidebar card layout). The
   * table layout uses .item-ref instead. */
  .branch-name,
  .item-number {
    color: var(--text-muted);
    margin-right: 4px;
  }

  :global(.branch-chip) {
    flex-shrink: 0;
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

  .error-banner {
    padding: 8px 16px;
    background: color-mix(in srgb, var(--accent-red) 10%, transparent);
    color: var(--accent-red);
    font-size: var(--font-size-sm);
    border-bottom: 1px solid var(--border-default);
  }

  .error-banner--action {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .error-banner--action button {
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    background: transparent;
    color: inherit;
    cursor: pointer;
    font: inherit;
    padding: 2px 8px;
  }

  .error-banner--action button:hover {
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
  }

  .capped-notice {
    padding: 6px 16px;
    font-size: var(--font-size-xs);
    color: var(--accent-amber);
    background: color-mix(in srgb, var(--accent-amber) 8%, transparent);
    border-top: 1px solid var(--border-default);
    text-align: center;
    flex-shrink: 0;
  }

</style>
