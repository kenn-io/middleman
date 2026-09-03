<script lang="ts">
  import {
    copyToClipboard,
    formatRelativeTime,
    formatTimestamp,
    IconButton,
    SearchInput,
    StatusDot,
    type StatusDotStatus,
  } from "@kenn-io/kit-ui";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import {
    openNewWorkspaceDialog,
    type NewWorkspaceRepoSeed,
  } from "../../stores/new-workspace.svelte.js";
  import { onMount, tick } from "svelte";
  import { Effect, Stream } from "effect";
  import { navigate } from "../../stores/router.svelte.ts";
  import GitBranchIcon from "@lucide/svelte/icons/git-branch";
  import ArrowUpIcon from "@lucide/svelte/icons/arrow-up";
  import ArrowDownIcon from "@lucide/svelte/icons/arrow-down";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { ApiProblemError } from "../../api/effect-errors.js";
  import {
    executeGeneratedApiRequest,
    executeOpaqueGeneratedApiRequest,
  } from "../../api/generated-api.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { getStores } from "../../context.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import type { HostSummary as GeneratedHostSummary } from "../../api/generated/models/index.js";
  import { DiffStats, FilterDropdown, ScrollBox, SidebarToggle } from "@kenn-io/kit-ui";
  import GroupedSidebarSection from "../shared/GroupedSidebarSection.svelte";
  import SidebarTitlePopover from "../sidebar/SidebarTitlePopover.svelte";
  import type { WorkspaceItemIdentity } from "../../workspace-inline.js";
  import {
    createRepoLabelFormatter,
    repoIdentityKey,
    type RepoLabelIdentity,
  } from "../../utils/repo-label.js";
  import ProviderIcon from "../provider/ProviderIcon.svelte";
  import ConfirmDialog from "../shared/ConfirmDialog.svelte";
  import {
    defaultWorkspaceListDisplayOptions,
    defaultWorkspaceListSort,
    loadWorkspaceListDisplayOptions,
    loadWorkspaceListSort,
    saveWorkspaceListDisplayOptions,
    saveWorkspaceListSort,
    workspaceAgentStatePriority,
    workspaceAgentStateSortTime,
    workspaceListSortTimestamp,
    workspaceListSortOptions,
    type WorkspaceListDisplayOptions,
    type WorkspaceListSort,
  } from "./workspaceListSort.ts";
  import {
    WorkspaceListWorkflow,
    makeWorkspaceRefreshHub,
    workspaceListLifecycle,
  } from "./workspace-list-workflow.js";
  import { workspaceEventStream } from "./workspace-event-stream.js";
  import {
    decodeWorkspaceList,
    retainDegradedHostWorkspaces,
    type WorkspaceListItem,
  } from "./workspace-list-schema.js";
  import { parseRepoFilterValue } from "../../stores/filter.svelte.js";
  import {
    canonicalRepoFilterValue,
    type RepoFilterIdentity,
  } from "../../utils/repo-filter-values.js";
  import { setWorkspaceRepoCatalog } from "../../stores/workspace-repo-catalog.svelte.js";
  import { loadFleetSnapshot } from "../../api/fleet-snapshot.js";

  type Workspace = WorkspaceListItem;

  type HostSummary = GeneratedHostSummary;
  type CatalogLoadStatus = "loading" | "loaded" | "failed";

  interface Props {
    selectedId: string;
    selectedRepos?: string | undefined;
    selectedHostKey?: string | undefined;
    onOpenItemSidebar?: (
      workspaceId: string,
      tab: "pr" | "issue" | "kata",
      hostKey?: string,
    ) => void;
    onWorkspaceListStateChange?: (
      state: { status: "loading" | "retrying" | "loaded"; total: number },
    ) => void;
    isWorkspaceActionDisabled?: (
      workspaceId: string,
      hostKey?: string,
    ) => boolean;
    onWorkspaceDeletePendingChange?: (
      workspaceId: string,
      hostKey: string | undefined,
      pending: boolean,
    ) => void;
    // Reports a successful delete from this list's own delete flow, so a
    // hosting shell (e.g. an inline claimant) can react even though this
    // sidebar isn't the one navigating away. Mirrors WorkspaceTerminalView's
    // own onWorkspaceDeleted contract.
    onWorkspaceDeleted?:
      | ((workspaceId: string, hostKey?: string, identity?: WorkspaceItemIdentity) => void)
      | undefined;
    isSidebarToggleEnabled?: boolean;
    onCollapseSidebar?: (() => void) | undefined;
    // False while this instance is parked in a hidden host: every dialog
    // unmounts (its state persists for reopen) the same way
    // WorkspaceTerminalView's own dialogs do. Defaults to true so
    // standalone/embedded usage (this component mounted outside the
    // reparenting host) is unaffected.
    hostVisible?: boolean;
  }

  const {
    selectedId,
    selectedRepos = undefined,
    selectedHostKey = undefined,
    onOpenItemSidebar,
    onWorkspaceListStateChange,
    isWorkspaceActionDisabled,
    onWorkspaceDeletePendingChange,
    onWorkspaceDeleted,
    isSidebarToggleEnabled = false,
    onCollapseSidebar,
    hostVisible = true,
  }: Props = $props();

  const runtime = getAppRuntime();
  const { events: eventsStore } = getStores();

  const doneAcknowledgementsStorageKey =
    "kenn-forge:workspace-agent-done-acknowledgements/v1";

  let workspaces = $state.raw<Workspace[]>([]);
  let localCatalogStatus = $state<CatalogLoadStatus>("loading");
  let fleetCatalogStatus = $state<CatalogLoadStatus>("loading");
  let peerCatalogStatus = $state<CatalogLoadStatus>("loading");
  const workspaceRepoCatalogComplete = $derived(
    localCatalogStatus === "loaded" &&
      fleetCatalogStatus === "loaded" &&
      peerCatalogStatus === "loaded",
  );

  $effect(() => {
    setWorkspaceRepoCatalog(
      workspaceListStatus === "loaded" ? workspaces : undefined,
      workspaceRepoCatalogComplete,
    );
  });
  let fleetHosts = $state.raw<HostSummary[]>([]);
  let fleetError = $state<string | null>(null);
  let fleetPeerErrors = $state.raw<Record<string, string>>({});
  let collapsedGroups = $state<string[]>([]);
  let searchQuery = $state("");
  let sortMode = $state<WorkspaceListSort>(loadWorkspaceListSort());
  let workspaceListStatus = $state<"loading" | "retrying" | "loaded">("loading");
  let contextMenu = $state<{
    ws: Workspace;
    x: number;
    y: number;
  } | null>(null);
  let deleteConfirmWorkspace = $state<Workspace | null>(null);
  let deleteConfirmForce = $state(false);
  let workspaceAction = $state<{
    workspaceKey: string;
    action: "push" | "pull" | "reveal" | "delete";
  } | null>(null);
  let contextMenuEl = $state<HTMLDivElement | null>(null);
  // Row elements keyed by workspaceRowKey so the snippet-rendered rows can
  // each anchor their own truncation popover.
  let rowEls = $state<Record<string, HTMLElement | null>>({});
  let contextMenuStyle = $state("");
  let acknowledgedDoneStates = $state.raw<Record<string, string>>(
    loadDoneAcknowledgements(),
  );

  const workspaceListLoadTimeoutMs = 10_000;
  let displayOptions = $state<WorkspaceListDisplayOptions>(
    loadWorkspaceListDisplayOptions(),
  );

  const refreshWorkspaces = makeWorkspaceRefreshHub(
    Effect.suspend(loadWorkspaces).pipe(
      Effect.catch(() =>
        Effect.sync(() => {
          localCatalogStatus = "failed";
          fleetCatalogStatus = "failed";
          peerCatalogStatus = "failed";
          if (workspaces.length === 0) workspaceListStatus = "retrying";
        }),
      ),
    ),
  );
  const refreshFleet = refreshWorkspaces;
  let requestApplicationWorkspaceRefresh = refreshWorkspaces.request;
  const workspaceRefreshOwner = $props.id();

  type WorkspaceGroup = {
    key: string;
    items: Workspace[];
  };

  const normalizedSearchQuery = $derived(
    searchQuery.trim().toLowerCase(),
  );
  const selectedRepoValues = $derived(new Set(parseRepoFilterValue(selectedRepos)));
  const deleteConfirmBusy = $derived(
    deleteConfirmWorkspace !== null &&
      workspaceActionMatches(deleteConfirmWorkspace, "delete"),
  );

  const visibleWorkspaces = $derived.by(() => {
    const repoIdentities = workspaces.map(workspaceRepoFilterIdentity);
    const scoped = selectedRepoValues.size === 0
      ? workspaces
      : workspaces.filter((workspace) => {
          const value = canonicalRepoFilterValue(
            workspaceRepoFilterIdentity(workspace),
            repoIdentities,
          );
          return value !== null && selectedRepoValues.has(value);
        });
    if (!normalizedSearchQuery) return scoped;
    return scoped.filter((ws) => workspaceMatchesSearch(ws, normalizedSearchQuery));
  });

  const sidebarCountLabel = $derived(
    normalizedSearchQuery || selectedRepoValues.size > 0
      ? `${visibleWorkspaces.length}/${workspaces.length}`
      : `${workspaces.length}`,
  );

  const grouped = $derived.by<WorkspaceGroup[]>(() => {
    const groups: WorkspaceGroup[] = [];
    for (const ws of visibleWorkspaces) {
      const key = repoIdentityKey(workspaceRepoIdentity(ws));
      const group = groups.find((candidate) => candidate.key === key);
      if (group) {
        group.items.push(ws);
      } else {
        groups.push({ key, items: [ws] });
      }
    }
    return groups;
  });

  const sortLabel = $derived(
    workspaceListSortOptions.find(
      (option) => option.value === sortMode,
    )?.label ?? "Org / repo",
  );

  const viewBadgeCount = $derived(
    Number(sortMode !== defaultWorkspaceListSort) +
      Number(
        displayOptions.showOrgNames !==
          defaultWorkspaceListDisplayOptions.showOrgNames,
      ) +
      Number(
        displayOptions.showDiffStats !==
          defaultWorkspaceListDisplayOptions.showDiffStats,
      ),
  );

  const viewSections = $derived.by(() => [
    {
      title: "Sorting",
      items: workspaceListSortOptions.map((option) => ({
        id: option.value,
        label: option.label,
        description: option.description,
        active: sortMode === option.value,
        closeOnSelect: true,
        onSelect: () => setSort(option.value),
      })),
    },
    {
      title: "Visibility",
      items: [
        {
          id: "hide-org-name",
          label: "Hide org name",
          description: "Hide owner or organization names in workspace repo labels.",
          active: !displayOptions.showOrgNames,
          onSelect: () =>
            setDisplayOption(
              "showOrgNames",
              !displayOptions.showOrgNames,
            ),
        },
        {
          id: "show-diff-stats",
          label: "Show PR diff stats",
          description: "Show additions and deletions for linked pull request workspaces.",
          active: displayOptions.showDiffStats,
          onSelect: () =>
            setDisplayOption(
              "showDiffStats",
              !displayOptions.showDiffStats,
            ),
        },
      ],
    },
  ]);

  const fleetDegraded = $derived(
    fleetError !== null || Object.keys(fleetPeerErrors).length > 0,
  );
  const showWorkspaceHostBadges = $derived(
    fleetHosts.length > 1 &&
      fleetHosts.find((host) => host.kind === "self")?.federationRole === "hub",
  );

  // Flat ordering for status and timestamp sorts. The org/repo mode keeps
  // the API order (created_at DESC) inside each repo group.
  // Agent status follows the same attention-first priority as the server's
  // aggregate hook state, then keeps newer workspaces first within a status.
  // "Activity" means terminal output only (tmux_last_output_at).
  // "Item activity" means the synced PR/issue last_activity_at.
  // Missing timestamps fall back to workspace creation time.
  const sortedFlat = $derived.by(() => {
    if (sortMode === "agent-status") {
      return [...visibleWorkspaces].sort(
        (a, b) =>
          workspaceAgentStatePriority(b.agent_state) - workspaceAgentStatePriority(a.agent_state) ||
          timeValue(workspaceAgentStateSortTime(b)) - timeValue(workspaceAgentStateSortTime(a)) ||
          a.id.localeCompare(b.id),
      );
    }
    const stamp = sortMode === "activity"
      ? (ws: Workspace) =>
          timeValue(ws.tmux_last_output_at) || timeValue(ws.created_at)
      : sortMode === "item-activity"
        ? (ws: Workspace) =>
            timeValue(ws.item_last_activity_at) ||
            timeValue(ws.created_at)
        : (ws: Workspace) => timeValue(ws.created_at);
    return [...visibleWorkspaces].sort(
      (a, b) => stamp(b) - stamp(a) || a.id.localeCompare(b.id),
    );
  });

  function setSort(sort: WorkspaceListSort): void {
    sortMode = sort;
    saveWorkspaceListSort(sort);
  }

  function setDisplayOption(
    key: keyof WorkspaceListDisplayOptions,
    value: boolean,
  ): void {
    displayOptions = { ...displayOptions, [key]: value };
    saveWorkspaceListDisplayOptions(displayOptions);
  }

  $effect(() => {
    if (!contextMenu) return;

    function closeForOutsideClick(event: MouseEvent): void {
      if (
        contextMenuEl &&
        event.target instanceof Node &&
        contextMenuEl.contains(event.target)
      ) {
        return;
      }
      closeContextMenu();
    }

    function closeForEscape(event: KeyboardEvent): void {
      if (event.key === "Escape") closeContextMenu();
    }

    function reposition(): void {
      positionContextMenu();
    }

    document.addEventListener("mousedown", closeForOutsideClick);
    document.addEventListener("keydown", closeForEscape);
    window.addEventListener("resize", reposition);
    window.addEventListener("scroll", closeContextMenu, true);
    return () => {
      document.removeEventListener("mousedown", closeForOutsideClick);
      document.removeEventListener("keydown", closeForEscape);
      window.removeEventListener("resize", reposition);
      window.removeEventListener("scroll", closeContextMenu, true);
    };
  });

  $effect(() => {
    onWorkspaceListStateChange?.({
      status: workspaceListStatus,
      total: workspaces.length,
    });
  });

  function timeValue(value: string | null | undefined): number {
    if (!value) return 0;
    const ms = Date.parse(value);
    return Number.isNaN(ms) ? 0 : ms;
  }

  function fleetHostName(hostKey: string): string {
    const name = fleetHosts.find((host) => host.configKey === hostKey)?.name.trim();
    return name || hostKey;
  }

  function workspaceHostName(ws: Workspace): string {
    return ws.fleet_host_name?.trim() || workspaceHost(ws)?.name.trim() || ws.fleet_host_key || "This machine";
  }

  const repoLabelFormatter = $derived.by(() =>
    createRepoLabelFormatter(
      workspaces.map(workspaceRepoIdentity),
      { showOrgNames: displayOptions.showOrgNames },
    ),
  );

  const showProviderIcons = $derived.by(() => {
    const providers: string[] = [];
    for (const ws of workspaces) {
      const provider = workspaceProvider(ws);
      const normalized = provider?.toLowerCase();
      if (normalized && !providers.includes(normalized)) {
        providers.push(normalized);
      }
    }
    return providers.length > 1;
  });

  function failureMessage(failure: unknown, fallback: string): string {
    if (failure instanceof ApiProblemError) return apiErrorMessage(failure.problem, fallback);
    return failure instanceof Error ? failure.message : fallback;
  }

  function loadWorkspaces() {
    return Effect.sync(() => {
      localCatalogStatus = "loading";
      fleetCatalogStatus = "loading";
      peerCatalogStatus = "loading";
    }).pipe(
      Effect.andThen(Effect.gen(function* () {
        const data = yield* loadFleetSnapshot().pipe(
          Effect.timeout(`${workspaceListLoadTimeoutMs} millis`),
        );
        const decoded = yield* decodeWorkspaceList({ workspaces: data.workspaces });
        const nextHosts = data.hosts ?? [];
        const aggregateComplete = !(data.aggregateIncomplete ?? false);
        const nextWorkspaces = retainDegradedHostWorkspaces(
          workspaces,
          decoded.filter((workspace) => workspace.visible !== false),
          nextHosts,
          data.aggregateIncomplete ?? false,
        );
        yield* Effect.sync(() => {
          reconcileDoneAcknowledgements(nextWorkspaces);
          workspaces = nextWorkspaces;
          fleetHosts = nextHosts;
          fleetPeerErrors = Object.fromEntries(
            fleetHosts
              .filter((host) => host.kind !== "self" && host.error)
              .map((host) => [host.configKey, host.error ?? "Host unavailable"]),
          );
          fleetError = null;
          workspaceListStatus = "loaded";
          localCatalogStatus = "loaded";
          fleetCatalogStatus = aggregateComplete ? "loaded" : "failed";
          peerCatalogStatus = aggregateComplete ? "loaded" : "failed";
        });
      })),
    );
  }

  function toggleGroup(key: string): void {
    collapsedGroups = collapsedGroups.includes(key)
      ? collapsedGroups.filter((candidate) => candidate !== key)
      : [...collapsedGroups, key];
  }

  function displayName(ws: Workspace): string {
    if (ws.item_type === "kata_task") {
      return ws.kata?.title?.trim() || ws.git_head_ref;
    }
    return ws.mr_title ?? ws.git_head_ref;
  }

  function kataIdentityLabel(ws: Workspace): string {
    return (
      ws.kata?.short_id?.trim() ||
      ws.kata?.qualified_id?.trim() ||
      "Kata"
    );
  }

  // An ad-hoc workspace has no provider item at creation, but the daemon links
  // one once its branch is pushed and a PR appears. That detected PR is the
  // workspace's item number for display, opening, and search.
  function adHocPRNumber(ws: Workspace): number | null {
    if (ws.item_type !== "adhoc") return null;
    const number = ws.associated_pr_number;
    return number && number > 0 ? number : null;
  }

  function itemBubbleNumber(ws: Workspace): number | null {
    if (
      (ws.item_type === "pull_request" || ws.item_type === "issue") &&
      !ws.source_item_visible
    ) {
      return null;
    }
    return ws.item_type === "adhoc" ? adHocPRNumber(ws) : ws.item_number;
  }

  function hasItemBubble(ws: Workspace): boolean {
    if (ws.item_type === "kata_task") return true;
    return itemBubbleNumber(ws) !== null;
  }

  function itemBubbleLabel(ws: Workspace): string {
    if (ws.item_type === "kata_task") return kataIdentityLabel(ws);
    return `#${itemBubbleNumber(ws)}`;
  }

  function itemBubbleTitle(ws: Workspace): string {
    if (ws.item_type === "kata_task") {
      const title = ws.kata?.title?.trim();
      const identity = kataIdentityLabel(ws);
      return title ? `Open Kata task ${identity}: ${title}` : `Open Kata task ${identity}`;
    }
    return ws.item_type === "issue"
      ? `Open issue #${itemBubbleNumber(ws)}`
      : `Open PR #${itemBubbleNumber(ws)}`;
  }

  function updateSearch(value: string): void {
    searchQuery = value;
  }

  function workspaceMatchesSearch(
    ws: Workspace,
    query: string,
  ): boolean {
      const haystack: Array<string | undefined> = [
      displayName(ws),
      ws.git_head_ref,
      shortBranch(ws.git_head_ref),
      ws.platform_host,
      ws.repo_owner,
      ws.repo_name,
      ws.repo?.repo_path,
      `${ws.repo_owner}/${ws.repo_name}`,
      `${ws.platform_host}/${ws.repo_owner}/${ws.repo_name}`,
    ];

    if (ws.item_type === "kata_task") {
      haystack.push(
        "kata",
        ws.kata?.short_id,
        ws.kata?.qualified_id,
        ws.kata?.title,
        ws.kata?.project_name,
        // Durable identifiers so a Kata workspace stays findable by its task
        // key even when it has no short/qualified ID to display.
        ws.kata?.daemon_id,
        ws.kata?.project_uid,
        ws.kata?.issue_uid,
        ws.item_key,
      );
    } else if (ws.item_type === "adhoc") {
      // Without a detected PR there is no number or item title to match on:
      // the branch (already in the haystack) plus the kind keywords are what
      // a user would type.
      haystack.push("adhoc", "new work");
      const prNumber = adHocPRNumber(ws);
      if (prNumber !== null) {
        haystack.push(
          String(prNumber),
          `#${prNumber}`,
          `pr ${prNumber}`,
          `pr #${prNumber}`,
        );
      }
    } else {
      const itemKind = ws.item_type === "issue" ? "issue" : "pr";
      const itemNumber = String(ws.item_number);
      haystack.push(
        itemNumber,
        `#${itemNumber}`,
        `${itemKind} ${itemNumber}`,
        `${itemKind} #${itemNumber}`,
      );
    }

    return haystack.some((value) =>
      value?.toLowerCase().includes(query),
    );
  }

  function workspaceStatus(ws: Workspace): StatusDotStatus {
    if (ws.status === "ready") return "idle";
    if (ws.status === "creating" || ws.status === "deleting") return "working";
    if (ws.status === "error" || ws.status === "deletion_failed") return "unclean";
    return "stale";
  }

  function workspaceStatusLabel(ws: Workspace): string {
    if (ws.status === "ready") return "Workspace ready";
    if (ws.status === "creating") return "Creating workspace";
    if (ws.status === "error") return "Workspace error";
    if (ws.status === "deleting") return "Deleting workspace";
    if (ws.status === "deletion_failed") return "Deletion failed";
    return `Workspace ${ws.status}`;
  }

  function workingTitle(ws: Workspace): string {
    const title = ws.tmux_pane_title?.trim();
    const source = ws.tmux_activity_source;
    if (source && source !== "unknown" && title) {
      return `Working (${source}): ${title}`;
    }
    if (source && source !== "unknown") {
      return `Working (${source})`;
    }
    return title || "Working";
  }

  function agentStatePresentation(ws: Workspace): {
    label: "Working" | "Approval" | "Input" | "Done" | "Idle";
    status: StatusDotStatus;
    tone: "working" | "approval" | "input" | "done" | "idle";
  } | null {
    switch (ws.agent_state) {
      case "working":
        return { label: "Working", status: "working", tone: "working" };
      case "approval":
        return { label: "Approval", status: "waiting", tone: "approval" };
      case "input":
        return { label: "Input", status: "waiting", tone: "input" };
      case "done": {
        const version = doneStateVersion(ws);
        if (sortMode !== "agent-status" && version !== null && acknowledgedDoneStates[workspaceRowKey(ws)] === version) {
          return null;
        }
        return { label: "Done", status: "idle", tone: "done" };
      }
      case "idle":
        return sortMode === "agent-status" ? { label: "Idle", status: "idle", tone: "idle" } : null;
      default:
        return null;
    }
  }

  function loadDoneAcknowledgements(): Record<string, string> {
    try {
      const stored = window.sessionStorage.getItem(doneAcknowledgementsStorageKey);
      if (!stored) return {};
      const parsed: unknown = JSON.parse(stored);
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
        return {};
      }
      const acknowledgements: Record<string, string> = {};
      for (const [workspaceKey, version] of Object.entries(parsed)) {
        if (typeof version === "string" && version.trim()) {
          acknowledgements[workspaceKey] = version;
        }
      }
      return acknowledgements;
    } catch {
      return {};
    }
  }

  function saveDoneAcknowledgements(value: Record<string, string>): void {
    try {
      if (Object.keys(value).length === 0) {
        window.sessionStorage.removeItem(doneAcknowledgementsStorageKey);
      } else {
        window.sessionStorage.setItem(doneAcknowledgementsStorageKey, JSON.stringify(value));
      }
    } catch {
      // Storage is optional; the in-memory acknowledgement still applies.
    }
  }

  function doneStateVersion(ws: Workspace): string | null {
    if (ws.agent_state !== "done") return null;
    return ws.agent_state_updated_at?.trim() || null;
  }

  function reconcileDoneAcknowledgements(items: Workspace[]): void {
    let next = acknowledgedDoneStates;
    for (const ws of items) {
      const workspaceKey = workspaceRowKey(ws);
      const acknowledgedVersion = next[workspaceKey];
      if (!acknowledgedVersion || acknowledgedVersion === doneStateVersion(ws)) continue;
      if (next === acknowledgedDoneStates) next = { ...acknowledgedDoneStates };
      delete next[workspaceKey];
    }
    if (next === acknowledgedDoneStates) return;
    acknowledgedDoneStates = next;
    saveDoneAcknowledgements(next);
  }

  function openWorkspace(ws: Workspace): void {
    if (
      ws.status === "deleting" ||
      !workspaceOperationAvailable(ws, "workspaceRead") ||
      !workspaceOperationAvailable(ws, "terminalAttach")
    ) return;
    const doneVersion = doneStateVersion(ws);
    if (sortMode !== "agent-status" && doneVersion !== null) {
      acknowledgedDoneStates = {
        ...acknowledgedDoneStates,
        [workspaceRowKey(ws)]: doneVersion,
      };
      saveDoneAcknowledgements(acknowledgedDoneStates);
    }
    navigate(workspaceRoute(ws));
  }

  function itemStateClass(ws: Workspace): string {
    if (ws.item_type === "kata_task") {
      return "kata";
    }
    if (ws.item_type === "issue") {
      return ws.mr_state === "closed" ? "closed" : "issue";
    }
    if (ws.mr_is_draft) return "draft";
    if (ws.mr_state === "merged") return "merged";
    if (ws.mr_state === "closed") return "closed";
    return "open";
  }

  function shortBranch(ref: string): string {
    return ref.replace(/^refs\/heads\//, "");
  }

  function workspaceRepoIdentity(ws: Workspace): RepoLabelIdentity {
    return {
      provider: ws.repo?.provider ?? "",
      platformHost: ws.platform_host,
      owner: ws.repo_owner,
      name: ws.repo_name,
      repoPath: ws.repo?.repo_path,
    };
  }

  function workspaceRepoFilterIdentity(ws: Workspace): RepoFilterIdentity {
    return {
      ...(ws.repo?.provider === undefined ? {} : { provider: ws.repo.provider }),
      platformHost: ws.platform_host,
      repoPath: ws.repo?.repo_path ?? `${ws.repo_owner}/${ws.repo_name}`,
      isGlob: false,
    };
  }

  function repoLabel(ws: Workspace): string {
    return repoLabelFormatter.format(workspaceRepoIdentity(ws));
  }

  function workspaceProvider(ws: Workspace): string | undefined {
    return ws.repo?.provider;
  }

  function workspaceHost(ws: Workspace): HostSummary | undefined {
    if (ws.fleet_host_key) {
      return fleetHosts.find(
        (host) => host.configKey === ws.fleet_host_key,
      );
    }
    return fleetHosts.find((host) => host.kind === "self");
  }

  function isRemoteWorkspace(ws: Workspace): boolean {
    return Boolean(ws.fleet_host_key);
  }

  function workspaceOperationAvailable(
    ws: Workspace,
    operation: "workspaceRead" | "workspaceWrite" | "terminalAttach",
  ): boolean {
    if (!isRemoteWorkspace(ws)) return true;
    return workspaceHost(ws)?.operationAvailability[operation]?.available === true;
  }

  function revealLabel(ws: Workspace): string {
    const platform = workspaceHost(ws)?.platform?.toLowerCase() ?? "";
    if (platform === "darwin" || platform === "macos") {
      return "Reveal in Finder";
    }
    if (platform.includes("win")) {
      return "Open containing folder";
    }
    return "Reveal in file manager";
  }

  function providerItemURL(ws: Workspace): string | null {
    // Kata workspaces are not backed by a provider item, and an ad-hoc
    // workspace only has one once a PR has been detected for its branch.
    if (ws.item_type === "kata_task") return null;
    const itemNumber = itemBubbleNumber(ws);
    if (itemNumber === null) return null;
    const provider = workspaceProvider(ws)?.toLowerCase();
    const repoPath = ws.repo?.repo_path ?? `${ws.repo_owner}/${ws.repo_name}`;
    const encodedPath = repoPath
      .split("/")
      .map((part) => encodeURIComponent(part))
      .join("/");
    const host = ws.platform_host;
    if (!host || !encodedPath) return null;
    if (provider === "github") {
      const kind = ws.item_type === "issue" ? "issues" : "pull";
      return `https://${host}/${encodedPath}/${kind}/${itemNumber}`;
    }
    if (provider === "gitlab") {
      const kind = ws.item_type === "issue" ? "issues" : "merge_requests";
      return `https://${host}/${encodedPath}/-/${kind}/${itemNumber}`;
    }
    if (provider === "gitea" || provider === "forgejo") {
      const kind = ws.item_type === "issue" ? "issues" : "pulls";
      return `https://${host}/${encodedPath}/${kind}/${itemNumber}`;
    }
    return null;
  }

  function providerLabel(ws: Workspace): string {
    const provider = workspaceProvider(ws);
    if (!provider) return "Open item on provider";
    return `Open item on ${provider}`;
  }

  function canPush(ws: Workspace): boolean {
    const ahead = ws.commits_ahead ?? 0;
    const behind = ws.commits_behind ?? 0;
    return ws.branch_upstream_missing === true || (ahead > 0 && behind === 0);
  }

  function canPull(ws: Workspace): boolean {
    const ahead = ws.commits_ahead ?? 0;
    const behind = ws.commits_behind ?? 0;
    return behind > 0 && ahead === 0;
  }

  function syncActionDetail(ws: Workspace): string {
    if (ws.branch_upstream_missing === true) {
      return "";
    }
    if (canPush(ws)) {
      return `${ws.commits_ahead ?? 0} ahead`;
    }
    if (canPull(ws)) {
      return `${ws.commits_behind ?? 0} behind`;
    }
    return "";
  }

  function openContextMenu(event: MouseEvent, ws: Workspace): void {
    event.preventDefault();
    event.stopPropagation();
    contextMenu = {
      ws,
      x: event.clientX,
      y: event.clientY,
    };
    runtime.runCommand(Effect.promise(() => tick()).pipe(Effect.andThen(Effect.sync(positionContextMenu))), {
      operation: "position workspace context menu",
      safeContext: { workspaceId: ws.id },
      onFailure: () => undefined,
    });
  }

  function positionContextMenu(): void {
    if (!contextMenu || !contextMenuEl) return;
    const margin = 8;
    const width = contextMenuEl.offsetWidth;
    const height = contextMenuEl.offsetHeight;
    const left = Math.min(
      Math.max(margin, contextMenu.x),
      Math.max(margin, window.innerWidth - width - margin),
    );
    const top = Math.min(
      Math.max(margin, contextMenu.y),
      Math.max(margin, window.innerHeight - height - margin),
    );
    contextMenuStyle = `left: ${left}px; top: ${top}px;`;
  }

  function closeContextMenu(): void {
    contextMenu = null;
    contextMenuStyle = "";
  }

  function copyMenuText(value: string, successMessage: string): void {
    closeContextMenu();
    runtime.runCommand(
      Effect.promise(() => copyToClipboard(value)).pipe(
        Effect.flatMap((copied) =>
          Effect.sync(() => {
            showFlash(copied ? successMessage : "Could not copy to clipboard.", copied ? undefined : { tone: "danger" });
          }),
        ),
      ),
      {
        operation: "copy workspace text",
        safeContext: {},
        onFailure: () => showFlash("Could not copy to clipboard.", { tone: "danger" }),
      },
    );
  }

  function refreshWorkspaceStatus(ws: Workspace): void {
    if (workspaceActionsDisabled(ws)) return;
    closeContextMenu();
    const hostKey = ws.fleet_host_key;
    const refresh = hostKey
      ? executeOpaqueGeneratedApiRequest("refresh remote workspace", (generatedClient, signal) =>
          generatedClient.FleetService.refreshFleetWorkspace({ hostKey, id: ws.id }, { signal }),
        ).pipe(Effect.asVoid)
      : executeGeneratedApiRequest("refresh workspace", (generatedClient, signal) =>
          generatedClient.WorkspacesService.refreshWorkspace({ id: ws.id }, { signal }),
        ).pipe(Effect.asVoid);
    runtime.runCommand(refresh.pipe(Effect.tap(() => Effect.sync(requestApplicationWorkspaceRefresh))), {
      operation: "refresh workspace status",
      safeContext: { workspaceId: ws.id, remote: Boolean(ws.fleet_host_key) },
      onFailure: (failure) => showFlash(failureMessage(failure, "Refresh failed."), { tone: "danger" }),
    });
  }

  function workspaceActionMatches(
    ws: Workspace,
    action?: "push" | "pull" | "reveal" | "delete",
  ): boolean {
    return (
      workspaceAction?.workspaceKey === workspaceRowKey(ws) &&
      (action === undefined || workspaceAction.action === action)
    );
  }

  function workspaceActionsDisabled(ws: Workspace): boolean {
    return ws.status === "deleting" || ws.status === "deletion_failed" ||
      !workspaceOperationAvailable(ws, "workspaceWrite") ||
      (isWorkspaceActionDisabled?.(ws.id, ws.fleet_host_key) ?? false);
  }

  function workspaceBusyLabel(ws: Workspace): string {
    if (!workspaceActionMatches(ws)) return "";
    if (workspaceAction?.action === "push") return "Pushing branch";
    if (workspaceAction?.action === "pull") return "Pulling branch";
    if (workspaceAction?.action === "reveal") return "Opening folder";
    if (workspaceAction?.action === "delete") return "Deleting workspace";
    return "";
  }

  function startWorkspaceAction(
    ws: Workspace,
    action: "push" | "pull" | "reveal" | "delete",
  ): boolean {
    const externallyDisabled = isWorkspaceActionDisabled?.(ws.id, ws.fleet_host_key) ?? false;
    const deletionDisabled = ws.status === "deleting" || externallyDisabled;
    if (
      workspaceAction !== null ||
      !workspaceOperationAvailable(ws, "workspaceWrite") ||
      (action === "delete" ? deletionDisabled : workspaceActionsDisabled(ws))
    ) return false;
    workspaceAction = { workspaceKey: workspaceRowKey(ws), action };
    return true;
  }

  function finishWorkspaceAction(ws: Workspace): void {
    if (workspaceActionMatches(ws)) {
      workspaceAction = null;
    }
  }

  function syncWorkspaceBranch(ws: Workspace, action: "push" | "pull"): void {
    if (!startWorkspaceAction(ws, action)) return;
    const label = action === "push" ? "Push branch" : "Pull remote changes";
    const hostKey = ws.fleet_host_key;
    const command = hostKey
      ? action === "push"
        ? executeOpaqueGeneratedApiRequest("push remote workspace branch", (generatedClient, signal) =>
            generatedClient.FleetService.pushFleetWorkspaceBranch({ hostKey, id: ws.id }, { signal }),
          ).pipe(Effect.asVoid)
        : executeOpaqueGeneratedApiRequest("pull remote workspace branch", (generatedClient, signal) =>
            generatedClient.FleetService.pullFleetWorkspaceBranch({ hostKey, id: ws.id }, { signal }),
          ).pipe(Effect.asVoid)
      : action === "push"
        ? executeGeneratedApiRequest("push workspace branch", (generatedClient, signal) =>
            generatedClient.WorkspacesService.pushWorkspaceBranch({ id: ws.id }, { signal }),
          ).pipe(Effect.asVoid)
        : executeGeneratedApiRequest("pull workspace branch", (generatedClient, signal) =>
            generatedClient.WorkspacesService.pullWorkspaceBranch({ id: ws.id }, { signal }),
          ).pipe(Effect.asVoid);
    runtime.runCommand(
      command.pipe(
        Effect.tap(() =>
          Effect.sync(() => {
            requestApplicationWorkspaceRefresh();
            closeContextMenu();
          }),
        ),
        Effect.ensuring(Effect.sync(() => finishWorkspaceAction(ws))),
      ),
      {
        operation: `${action} workspace branch`,
        safeContext: { workspaceId: ws.id, remote: Boolean(ws.fleet_host_key) },
        onFailure: (failure) => showFlash(failureMessage(failure, `${label} failed.`), { tone: "danger" }),
      },
    );
  }

  function revealWorkspacePath(ws: Workspace): void {
    if (!startWorkspaceAction(ws, "reveal")) return;
    const label = revealLabel(ws);
    const hostKey = ws.fleet_host_key;
    const command = hostKey
      ? executeOpaqueGeneratedApiRequest("reveal remote workspace path", (generatedClient, signal) =>
          generatedClient.FleetService.revealFleetWorkspace({ hostKey, id: ws.id }, { signal }),
        ).pipe(Effect.asVoid)
      : executeGeneratedApiRequest("reveal workspace path", (generatedClient, signal) =>
          generatedClient.WorkspacesService.revealWorkspace({ id: ws.id }, { signal }),
        ).pipe(Effect.asVoid);
    runtime.runCommand(
      command.pipe(
        Effect.tap(() => Effect.sync(closeContextMenu)),
        Effect.ensuring(Effect.sync(() => finishWorkspaceAction(ws))),
      ),
      {
        operation: "reveal workspace path",
        safeContext: { workspaceId: ws.id, remote: Boolean(ws.fleet_host_key) },
        onFailure: (failure) => showFlash(failureMessage(failure, `${label} failed.`), { tone: "danger" }),
      },
    );
  }

  function openDeleteWorkspaceDialog(ws: Workspace, force = false): void {
    deleteConfirmWorkspace = ws;
    deleteConfirmForce = force;
    closeContextMenu();
  }

  function closeDeleteWorkspaceDialog(): void {
    if (deleteConfirmWorkspace && workspaceActionMatches(deleteConfirmWorkspace, "delete")) return;
    deleteConfirmWorkspace = null;
    deleteConfirmForce = false;
  }

  function confirmDeleteWorkspaceFromList(): void {
    const ws = deleteConfirmWorkspace;
    if (!ws || !startWorkspaceAction(ws, "delete")) return;
    const force = deleteConfirmForce;
    onWorkspaceDeletePendingChange?.(ws.id, ws.fleet_host_key, true);
    const hostKey = ws.fleet_host_key;
    const command = hostKey
      ? executeOpaqueGeneratedApiRequest("delete remote workspace", (generatedClient, signal) =>
          generatedClient.FleetService.deleteFleetWorkspace(
            { hostKey, id: ws.id },
            force ? { force: true } : undefined,
            { signal },
          ),
        ).pipe(Effect.asVoid)
      : executeGeneratedApiRequest("delete workspace", (generatedClient, signal) =>
          generatedClient.WorkspacesService.deleteWorkspace(
            { id: ws.id },
            force ? { force: true } : undefined,
            { signal },
          ),
        ).pipe(Effect.asVoid);
    runtime.runCommand(
      command.pipe(
        Effect.tap(() =>
          Effect.sync(() => {
            // Report the deletion regardless of selection so a hosting shell's
            // inline claim cannot briefly reclaim a workspace this list destroyed.
            onWorkspaceDeleted?.(
              ws.id,
              hostKey,
              ws.repo
                ? {
                    provider: ws.repo.provider,
                    platformHost: ws.repo.platform_host,
                    owner: ws.repo.owner,
                    name: ws.repo.name,
                    repoPath: ws.repo.repo_path,
                    number: ws.item_number,
                    itemType: ws.item_type,
                  }
                : undefined,
            );
            requestApplicationWorkspaceRefresh();
            closeContextMenu();
            if (isSelectedWorkspace(ws)) navigate("/workspaces");
          }),
        ),
        Effect.ensuring(
          Effect.sync(() => {
            onWorkspaceDeletePendingChange?.(ws.id, hostKey, false);
            finishWorkspaceAction(ws);
            deleteConfirmWorkspace = null;
            deleteConfirmForce = false;
          }),
        ),
      ),
      {
        operation: "delete workspace",
        safeContext: { workspaceId: ws.id, remote: Boolean(hostKey) },
        onFailure: (failure) => {
          const problem = failure instanceof ApiProblemError ? failure.problem : undefined;
          const fallback = problem?.status === 409
            ? "Workspace has uncommitted changes. Open it to force delete."
            : "Delete failed.";
          showFlash(failureMessage(failure, fallback), { tone: "danger" });
        },
      },
    );
  }

  function openProviderItem(ws: Workspace): void {
    const url = providerItemURL(ws);
    closeContextMenu();
    if (!url) {
      showFlash("Provider URL is not available for this workspace.", { tone: "danger" });
      return;
    }
    window.open(url, "_blank", "noopener,noreferrer");
  }

  function copyProviderItemURL(ws: Workspace): void {
    const url = providerItemURL(ws);
    if (!url) {
      closeContextMenu();
      showFlash("Provider URL is not available for this workspace.", { tone: "danger" });
      return;
    }
    copyMenuText(url, "Copied item URL.");
  }

  function workspaceRoute(ws: Workspace): string {
    if (ws.fleet_host_key) {
      return `/terminal/fleet/${encodeURIComponent(ws.fleet_host_key)}/${encodeURIComponent(ws.id)}`;
    }
    return `/terminal/${encodeURIComponent(ws.id)}`;
  }

  function workspaceRowKey(ws: Workspace): string {
    return `${ws.fleet_host_key ?? "self"}:${ws.id}`;
  }

  // Preselects the repository of the workspace the user is already looking at,
  // which is nearly always the repo they want to start more work in.
  function newWorkspaceSeedRepo(): NewWorkspaceRepoSeed | undefined {
    const current = workspaces.find(isSelectedWorkspace);
    const provider = current?.repo?.provider;
    if (!current || !provider) return undefined;
    return {
      provider,
      platformHost: current.platform_host,
      owner: current.repo_owner,
      name: current.repo_name,
    };
  }

  function isSelectedWorkspace(ws: Workspace): boolean {
    return (
      ws.id === selectedId &&
      (ws.fleet_host_key ?? undefined) === selectedHostKey
    );
  }

  function handleItemBubbleClick(
    e: MouseEvent | KeyboardEvent,
    ws: Workspace,
  ): void {
    e.stopPropagation();
    e.preventDefault();
    if (!workspaceOperationAvailable(ws, "workspaceRead")) return;
    const tab =
      ws.item_type === "kata_task"
        ? "kata"
        : ws.item_type === "issue"
          ? "issue"
          : "pr";

    if (onOpenItemSidebar) {
      onOpenItemSidebar(ws.id, tab, ws.fleet_host_key);
      return;
    }
    navigate(workspaceRoute(ws));
  }

  onMount(() => {
    const workspaceEvents = workspaceEventStream(eventsStore.subscribeWorkspaceEvents).pipe(
      Stream.filter((signal) => signal._tag === "Status"),
    );
    const execution = runtime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          const workflow = yield* WorkspaceListWorkflow;
          requestApplicationWorkspaceRefresh = workflow.request;
          yield* workflow.claim(workspaceRefreshOwner, refreshWorkspaces.request);
          yield* workspaceListLifecycle({
            refreshWorkspaces,
            refreshFleet,
            workspaceEvents,
          });
        }),
      ),
      {
        operation: "run workspace list lifecycle",
        safeContext: {},
        onFailure: (failure) => {
          if (workspaces.length === 0) workspaceListStatus = "retrying";
          console.error("Workspace list lifecycle stopped", failure);
        },
      },
    );
    return execution.interrupt;
  });
</script>

<div class="workspace-list-sidebar">
  <div class="sidebar-header">
    <span class="sidebar-header-label">Workspaces</span>
    <span class="sidebar-header-count">{sidebarCountLabel}</span>
    <IconButton
      class="workspace-new-button"
      size="sm"
      ariaLabel="New workspace"
      title="New workspace"
      onclick={() => openNewWorkspaceDialog(newWorkspaceSeedRepo())}
    >
      <PlusIcon size="13" strokeWidth="2.2" aria-hidden="true" />
    </IconButton>
    <div class="workspace-sort">
      <FilterDropdown
        label="View"
        detail={sortLabel}
        active={viewBadgeCount > 0}
        badgeCount={viewBadgeCount}
        sections={viewSections}
        title="View workspace options"
        minWidth="220px"
        align="end"
      />
    </div>
    {#if isSidebarToggleEnabled && onCollapseSidebar}
      <!-- No --push here: .workspace-sort's auto margin already
           claims the free space, and a second auto margin would
           split it and strand the sort trigger mid-header. -->
      <SidebarToggle
        state="expanded"
        label="Workspaces sidebar"
        onclick={onCollapseSidebar}
        class="kit-sidebar-toggle--compact"
      />
    {/if}
  </div>
  <div class="workspace-filter">
    <SearchInput
      value={searchQuery}
      size="sm"
      block
      placeholder="Filter workspaces"
      ariaLabel="Filter workspaces"
      oninput={updateSearch}
    />
  </div>
  {#if fleetDegraded}
    <section class="fleet-status" aria-label="Fleet hosts">
      <div class="fleet-status-heading">
        <span class="fleet-status-title">Fleet</span>
        <span class="fleet-status-count">degraded</span>
      </div>
      {#if fleetError}
        <p class="fleet-status-error">{fleetError}</p>
      {:else}
        {#each Object.entries(fleetPeerErrors) as [hostKey, message] (hostKey)}
          <p class="fleet-status-error">
            <span>{fleetHostName(hostKey)} degraded</span>: {message}
          </p>
        {/each}
      {/if}
    </section>
  {/if}
  <ScrollBox class="sidebar-list" label="Workspaces">
    {#snippet children()}
    {#if sortMode === "repo"}
    {#each grouped as { key: repoKey, items } (repoKey)}
      {@const collapsed =
        !normalizedSearchQuery && collapsedGroups.includes(repoKey)}
      <GroupedSidebarSection
        label={repoLabel(items[0]!)}
        count={items.length}
        {collapsed}
        onclick={() => toggleGroup(repoKey)}
      >
        {#snippet leading()}
          {#if showProviderIcons && workspaceProvider(items[0]!)}
            <ProviderIcon
              provider={workspaceProvider(items[0]!)!}
              size={14}
              class="group-provider-icon"
            />
          {/if}
        {/snippet}
        {#each items as ws (workspaceRowKey(ws))}
          {@render workspaceRow(ws, false)}
        {/each}
      </GroupedSidebarSection>
    {/each}
    {:else}
      {#each sortedFlat as ws (workspaceRowKey(ws))}
        {@render workspaceRow(ws, true)}
      {/each}
    {/if}

    {#snippet worktreeDirtyIndicator()}
      <span
        class="worktree-dirty"
        title="Dirty worktree"
        role="img"
        aria-label="Dirty worktree"
      >
        <PencilIcon size={10} strokeWidth={2.2} aria-hidden="true" />
      </span>
    {/snippet}

    {#snippet workspaceRow(ws: Workspace, showRepo: boolean)}
          {@const adds = ws.mr_additions}
          {@const dels = ws.mr_deletions}
          {@const ahead = ws.commits_ahead ?? 0}
          {@const behind = ws.commits_behind ?? 0}
          {@const showPush = ahead > 0 || behind > 0}
          {@const agentState = agentStatePresentation(ws)}
          {@const sortTimestamp = workspaceListSortTimestamp(ws, sortMode)}
          {@const workspaceReadable = workspaceOperationAvailable(ws, "workspaceRead") && workspaceOperationAvailable(ws, "terminalAttach")}
          <div
            class={["ws-row", { selected: isSelectedWorkspace(ws) }]}
            bind:this={rowEls[workspaceRowKey(ws)]}
            onclick={(e) => {
              // The PR/issue bubble is a focusable child button; let
              // its own click handler run without the row also
              // navigating to the terminal route.
              if (e.target !== e.currentTarget &&
                e.target instanceof Element &&
                e.target.closest(".item-bubble")) {
                return;
              }
              openWorkspace(ws);
            }}
            onkeydown={(e) => {
              // Ignore keydowns that originate inside a nested
              // interactive element (e.g. the PR bubble button).
              // Without this guard, pressing Enter on the bubble
              // would navigate to the workspace before the bubble's
              // own click handler could open the sidebar tab.
              if (e.target !== e.currentTarget) return;
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                openWorkspace(ws);
              }
            }}
            oncontextmenu={(e) => {
              void openContextMenu(e, ws);
            }}
            aria-disabled={!workspaceReadable}
            tabindex={workspaceReadable ? 0 : -1}
            role="button"
          >
            <div class="ws-row-text">
              <div class="ws-row-title">
                <StatusDot
                  animated
                  status={workspaceStatus(ws)}
                  label={workspaceStatusLabel(ws)}
                  size={6}
                />
                <span class="ws-name">{displayName(ws)}</span>
                {#if showWorkspaceHostBadges}
                  <span
                    class="workspace-host-badge"
                    title={`Runs on ${workspaceHostName(ws)}`}
                  >{workspaceHostName(ws)}</span>
                {/if}
                {#if ws.status === "deleting" || ws.status === "deletion_failed"}
                  <span
                    class={["workspace-lifecycle-state", `workspace-lifecycle-state--${ws.status}`]}
                    title={ws.error_message ?? workspaceStatusLabel(ws)}
                  >{workspaceStatusLabel(ws)}</span>
                {/if}
                {#if agentState}
                  <span
                    class={["agent-state", `agent-state--${agentState.tone}`]}
                    title={`Agent ${agentState.label.toLowerCase()}`}
                  >
                    <StatusDot
                      animated
                      status={agentState.status}
                      label={`Agent ${agentState.label.toLowerCase()}`}
                      size={6}
                    />
                    <span>{agentState.label}</span>
                  </span>
                {:else if workspaceActionMatches(ws)}
                  <StatusDot status="working" label={workspaceBusyLabel(ws)} size={6} animated />
                {:else if ws.agent_state == null && ws.tmux_working}
                  <StatusDot status="working" label={workingTitle(ws)} size={6} animated />
                {/if}
              </div>
              <div class="ws-row-meta">
                {#if showRepo}
                  <span
                    class="repo-context"
                    title={`${ws.platform_host}/${ws.repo_owner}/${ws.repo_name}`}
                  >
                    {#if showProviderIcons && workspaceProvider(ws)}
                      <ProviderIcon
                        provider={workspaceProvider(ws)!}
                        size={10}
                        class="repo-context-icon"
                      />
                    {/if}
                    <span class="repo-context-name">{repoLabel(ws)}</span>
                  </span>
                {/if}
                <span class="branch-chip" title={ws.git_head_ref}>
                  <GitBranchIcon
                    class="branch-icon"
                    size="10"
                    strokeWidth="2"
                    aria-hidden="true"
                  />
                  <span class="branch-name">
                    {shortBranch(ws.git_head_ref)}
                  </span>
                </span>
                {#if showPush}
                  <span
                    class="push-state"
                    title={`${ahead} ahead, ${behind} behind upstream`}
                  >
                    {#if ahead > 0}
                      <span class="push-ahead">
                        <ArrowUpIcon
                          size="9"
                          strokeWidth="2.5"
                          aria-hidden="true"
                        />{ahead}
                      </span>
                    {/if}
                    {#if behind > 0}
                      <span class="push-behind">
                        <ArrowDownIcon
                          size="9"
                          strokeWidth="2.5"
                          aria-hidden="true"
                        />{behind}
                      </span>
                    {/if}
                  </span>
                {/if}
                {#if displayOptions.showDiffStats &&
                  ws.item_type === "pull_request" &&
                  ((adds ?? 0) > 0 || (dels ?? 0) > 0)}
                  <span class="workspace-diff-stats">
                    <DiffStats
                      additions={adds ?? 0}
                      deletions={dels ?? 0}
                    />
                  </span>
                {/if}
              </div>
            </div>
            <div class="ws-row-aside">
              <!-- Keep the item column stable when a workspace has no linked
                   provider item. The empty slot is layout only, never a
                   disabled or misleading #0 control. -->
              {#if hasItemBubble(ws)}
                <button
                  class={[
                    "item-bubble",
                    itemStateClass(ws),
                    ws.item_type === "kata_task" && "item-bubble--kata",
                  ]}
                  onclick={(e) => handleItemBubbleClick(e, ws)}
                  disabled={!workspaceOperationAvailable(ws, "workspaceRead")}
                  onkeydown={(e) => {
                    // Stop Enter/Space from bubbling to the row,
                    // since the row's keyboard handler also navigates.
                    if (e.key === "Enter" || e.key === " ") {
                      e.stopPropagation();
                    }
                  }}
                  title={itemBubbleTitle(ws)}
                >
                  {itemBubbleLabel(ws)}
                </button>
              {:else}
                <span class="item-bubble-slot" aria-hidden="true"></span>
              {/if}
              {#if sortTimestamp}
                <time
                  class="workspace-sort-time"
                  datetime={sortTimestamp.at}
                  title={`${sortTimestamp.label}: ${formatTimestamp(sortTimestamp.at)}`}
                  aria-label={`${sortTimestamp.label}: ${formatRelativeTime(sortTimestamp.at)}`}
                >{formatRelativeTime(sortTimestamp.at)}</time>
              {/if}
              {#if ws.worktree_dirty}
                {@render worktreeDirtyIndicator()}
              {/if}
            </div>
          </div>
          <SidebarTitlePopover
            target={rowEls[workspaceRowKey(ws)] ?? undefined}
            title={displayName(ws)}
            repository={repoLabel(ws)}
            branch={shortBranch(ws.git_head_ref)}
            truncationSelector=".ws-name"
          />
    {/snippet}
    {#if workspaceListStatus === "loading" && workspaces.length === 0}
      <p class="filter-empty">Loading workspaces...</p>
    {:else if workspaceListStatus === "retrying" && workspaces.length === 0}
      <p class="filter-empty">Still loading workspaces. Retrying...</p>
    {:else if visibleWorkspaces.length === 0 && normalizedSearchQuery}
      <p class="filter-empty">No workspaces match.</p>
    {:else if visibleWorkspaces.length === 0}
      <p class="filter-empty">No workspaces yet.</p>
    {/if}
    {/snippet}
  </ScrollBox>

</div>

{#if contextMenu}
  {@const menuWorkspace = contextMenu.ws}
  {@const localWorkspace = !isRemoteWorkspace(menuWorkspace)}
  {@const itemURL = providerItemURL(menuWorkspace)}
  {@const actionBusy = workspaceAction !== null}
  {@const actionDisabled = actionBusy || workspaceActionsDisabled(menuWorkspace)}
  <div
    class="workspace-context-menu kit-filter-dropdown__panel"
    bind:this={contextMenuEl}
    style={contextMenuStyle}
    role="menu"
    aria-label="Workspace actions"
    tabindex="-1"
    oncontextmenu={(e) => e.preventDefault()}
  >
    <div class="workspace-context-heading">
      <div class="workspace-context-title">
        {displayName(menuWorkspace)}
      </div>
      <div class="workspace-context-meta">
        {repoLabel(menuWorkspace)} · {shortBranch(menuWorkspace.git_head_ref)}
      </div>
    </div>

    <div class="kit-filter-dropdown__section-title">Sync branch</div>
    {#if canPush(menuWorkspace)}
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionDisabled}
        onclick={() => {
          void syncWorkspaceBranch(menuWorkspace, "push");
        }}
      >
        <span class="kit-filter-dropdown__dot filter-dot--success"></span>
        <span class="kit-filter-dropdown__label">{workspaceActionMatches(menuWorkspace, "push") ? "Pushing..." : "Push branch"}</span>
        <span class="workspace-context-detail">{syncActionDetail(menuWorkspace)}</span>
      </button>
    {:else if canPull(menuWorkspace)}
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionDisabled}
        onclick={() => {
          void syncWorkspaceBranch(menuWorkspace, "pull");
        }}
      >
        <span class="kit-filter-dropdown__dot filter-dot--warning"></span>
        <span class="kit-filter-dropdown__label">{workspaceActionMatches(menuWorkspace, "pull") ? "Pulling..." : "Pull remote changes"}</span>
        <span class="workspace-context-detail">{syncActionDetail(menuWorkspace)}</span>
      </button>
    {/if}
    <button
      class="kit-filter-dropdown__item active"
      role="menuitem"
      type="button"
      disabled={actionDisabled}
      onclick={() => {
        void refreshWorkspaceStatus(menuWorkspace);
      }}
    >
      <span class="kit-filter-dropdown__dot"></span>
      <span class="kit-filter-dropdown__label">Refresh git status</span>
    </button>

    <div class="kit-filter-dropdown__divider"></div>
    <button
      class="kit-filter-dropdown__item active"
      role="menuitem"
      type="button"
      disabled={actionBusy}
      onclick={() => {
        void copyMenuText(
          shortBranch(menuWorkspace.git_head_ref),
          "Copied branch name.",
        );
      }}
    >
      <span class="kit-filter-dropdown__dot"></span>
      <span class="kit-filter-dropdown__label">Copy branch name</span>
    </button>
    {#if localWorkspace}
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionBusy}
        onclick={() => {
          void copyMenuText(
            menuWorkspace.worktree_path,
            "Copied worktree path.",
          );
        }}
      >
        <span class="kit-filter-dropdown__dot"></span>
        <span class="kit-filter-dropdown__label">Copy worktree path</span>
      </button>
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionDisabled}
        onclick={() => {
          void revealWorkspacePath(menuWorkspace);
        }}
      >
        <span class="kit-filter-dropdown__dot"></span>
        <span class="kit-filter-dropdown__label">{workspaceActionMatches(menuWorkspace, "reveal") ? "Opening..." : revealLabel(menuWorkspace)}</span>
      </button>
    {/if}

    {#if itemURL}
      <div class="kit-filter-dropdown__divider"></div>
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionBusy}
        onclick={() => openProviderItem(menuWorkspace)}
      >
        <span class="kit-filter-dropdown__dot"></span>
        <span class="kit-filter-dropdown__label">{providerLabel(menuWorkspace)}</span>
      </button>
      <button
        class="kit-filter-dropdown__item active"
        role="menuitem"
        type="button"
        disabled={actionBusy}
        onclick={() => copyProviderItemURL(menuWorkspace)}
      >
        <span class="kit-filter-dropdown__dot"></span>
        <span class="kit-filter-dropdown__label">Copy item URL</span>
      </button>
    {/if}

    <div class="kit-filter-dropdown__divider"></div>
    <button
      class="kit-filter-dropdown__item active workspace-context-danger"
      role="menuitem"
      type="button"
      disabled={actionBusy || !workspaceOperationAvailable(menuWorkspace, "workspaceWrite") || menuWorkspace.status === "deleting" || (isWorkspaceActionDisabled?.(menuWorkspace.id, menuWorkspace.fleet_host_key) ?? false)}
      onclick={() => {
        openDeleteWorkspaceDialog(menuWorkspace);
      }}
    >
      <span class="kit-filter-dropdown__dot filter-dot--danger"></span>
      <span class="kit-filter-dropdown__label">{workspaceActionMatches(menuWorkspace, "delete") ? "Deleting..." : menuWorkspace.status === "deletion_failed" ? "Retry deletion..." : "Delete workspace..."}</span>
    </button>
    {#if menuWorkspace.status === "deletion_failed"}
      <button
        class="kit-filter-dropdown__item active workspace-context-danger"
        role="menuitem"
        type="button"
        disabled={actionBusy || !workspaceOperationAvailable(menuWorkspace, "workspaceWrite") || (isWorkspaceActionDisabled?.(menuWorkspace.id, menuWorkspace.fleet_host_key) ?? false)}
        onclick={() => openDeleteWorkspaceDialog(menuWorkspace, true)}
      >
        <span class="kit-filter-dropdown__dot filter-dot--danger"></span>
        <span class="kit-filter-dropdown__label">Force delete workspace...</span>
      </button>
    {/if}
  </div>
{/if}

<ConfirmDialog
  open={deleteConfirmWorkspace !== null && hostVisible}
  title={deleteConfirmForce ? "Force delete workspace?" : deleteConfirmWorkspace?.status === "deletion_failed" ? "Retry workspace deletion?" : "Delete workspace?"}
  message={deleteConfirmWorkspace
    ? `Delete workspace "${displayName(deleteConfirmWorkspace)}"?`
    : ""}
  hint={deleteConfirmForce
    ? "This discards uncommitted changes and removes the managed worktree and runtime sessions."
    : deleteConfirmWorkspace?.error_message ?? "This removes its managed worktree and runtime sessions."}
  confirmLabel={deleteConfirmForce ? "Force delete workspace" : deleteConfirmWorkspace?.status === "deletion_failed" ? "Retry deletion" : "Delete workspace"}
  pendingLabel="Deleting…"
  busy={deleteConfirmBusy}
  tone="danger"
  frameId="workspace-sidebar-delete"
  onCancel={closeDeleteWorkspaceDialog}
  onConfirm={() => void confirmDeleteWorkspaceFromList()}
/>

<style>
  .workspace-list-sidebar {
    width: 100%;
    height: 100%;
    background: var(--sidebar-list-bg, var(--bg-surface));
    display: flex;
    flex-direction: column;
    overflow: hidden;
    /* Establish a tighter type rhythm independent of the document
     * default, so the rail reads as a tool window rather than a
     * loosely-styled page section. */
    font-feature-settings: "tnum" 1, "calt" 1;
    /* Drive width-aware hiding (diff stats first, then push counts)
     * off the rail's own width rather than the viewport. The rail
     * is user-resizable, so a viewport media query would lie. */
    container-type: inline-size;
    container-name: workspace-rail;
  }

  .sidebar-header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-height: 40px;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    flex-shrink: 0;
  }

  .sidebar-header-label {
    font-size: var(--font-size-xs);
    font-weight: 700;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--text-muted);
  }

  .sidebar-header-count {
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    color: var(--text-muted);
    opacity: 0.7;
  }

  .workspace-filter {
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    flex-shrink: 0;
  }

  .workspace-sort {
    /* Claims the header's free space so the sort trigger (and the
     * collapse toggle after it) sit flush right. */
    margin-left: auto;
    flex-shrink: 0;
  }

  .workspace-sort :global(.kit-filter-dropdown__btn) {
    /* Borderless inside the 28px header rail; the dropdown trigger
     * reads as header chrome rather than a standalone button. */
    min-height: 22px;
    padding: 2px 6px;
    border-color: transparent;
    background: transparent;
  }

  .workspace-sort :global(.kit-filter-dropdown__btn:hover:not(:disabled)) {
    border-color: var(--border-muted);
  }

  .fleet-status {
    flex-shrink: 0;
    margin: 4px 8px 6px;
    padding: 7px 8px 8px;
    border-top: 1px solid var(--border-muted);
    border-bottom: 1px solid var(--border-muted);
    background: color-mix(in srgb, var(--bg-surface) 72%, transparent);
  }

  .fleet-status-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 6px;
  }

  .fleet-status-title {
    font-size: var(--font-size-2xs);
    font-weight: 700;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--text-muted);
  }

  .fleet-status-count {
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    color: var(--accent-amber);
  }

  .fleet-status-error {
    margin: 0;
    color: var(--accent-amber);
    font-size: var(--font-size-xs);
    line-height: 1.35;
  }

  .filter-empty {
    margin: 14px 12px;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    line-height: 1.4;
  }

  :global(.group-provider-icon) {
    color: var(--text-secondary);
  }

  .ws-row {
    /* Two columns: a flex-shrinking text region on the left (which
     * holds two lines — title + meta) and a fixed-width bubble
     * pinned to the right. The bubble lives outside .ws-row-text,
     * so push counts or diff stats in the meta line can never
     * shift it left or off-screen — its X is anchored to the rail's
     * right edge for every row. */
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    padding: var(--sidebar-row-padding, 10px 12px);
    border-bottom: 1px solid var(--sidebar-list-border-muted, var(--border-muted));
    border-left: 3px solid transparent;
    background: var(--sidebar-row-bg, var(--bg-surface));
    cursor: pointer;
    position: relative;
    outline: none;
  }

  .ws-row:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .ws-row:focus-visible {
    background: var(--bg-surface-hover);
    box-shadow: inset 0 0 0 1px var(--accent-blue);
  }

  .ws-row.selected {
    background: var(--bg-row-selected, var(--bg-surface));
    border-left-color: var(--accent-blue);
  }

  .ws-row.selected:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .ws-row-text {
    /* Stacks the title and meta lines inside the left column. Has
     * to set min-width:0 so its own content can shrink rather than
     * pushing the bubble off-screen. */
    flex: 1 1 auto;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .ws-row-title {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .ws-row-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .ws-name {
    flex: 1;
    min-width: 0;
    font-size: var(--font-size-md);
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    letter-spacing: 0.005em;
    line-height: 1.35;
  }

  .ws-row.selected .ws-name {
    font-weight: 600;
  }

  .workspace-host-badge {
    flex: 0 1 auto;
    min-width: 0;
    max-width: 38%;
    overflow: hidden;
    padding: 1px 5px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-subtle);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    line-height: 1.35;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .workspace-lifecycle-state {
    flex-shrink: 0;
    font-size: var(--font-size-xs);
    font-weight: 600;
    line-height: 1;
  }

  .workspace-lifecycle-state--deleting {
    color: var(--accent-blue);
  }

  .workspace-lifecycle-state--deletion_failed {
    color: var(--accent-red);
  }

  .agent-state {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    flex-shrink: 0;
    font-size: var(--font-size-xs);
    font-weight: 600;
    line-height: 1;
  }

  .agent-state--working {
    color: var(--accent-green);
  }

  .agent-state--approval {
    color: var(--accent-amber);
    --status-waiting: var(--accent-amber);
  }

  .agent-state--input {
    color: var(--accent-purple);
    --status-waiting: var(--accent-purple);
  }

  .agent-state--done {
    color: var(--accent-green);
  }

  .agent-state--idle {
    color: var(--text-muted);
  }


  .repo-context {
    /* Flat sorts drop the per-repo group headers, so each row
     * carries its own repo context on the meta line. Caps at half
     * the line so the branch chip always keeps some room. */
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    flex: 0 1 auto;
    max-width: 50%;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }

  .repo-context-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  :global(.repo-context-icon) {
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .branch-chip {
    /* Lives on the meta line; takes whatever width is left after
     * push state and diff stats and truncates with ellipsis. */
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    font-weight: 500;
    color: var(--text-secondary);
    letter-spacing: 0;
    /* Tabular numerals + slightly tighter tracking turn the branch
     * line into a JetBrains-style "ref chip" rather than soft prose. */
    font-variant-numeric: tabular-nums;
  }

  .branch-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  :global(.branch-icon) {
    color: var(--text-muted);
    flex-shrink: 0;
    margin-right: 1px;
  }

  .worktree-dirty {
    display: inline-flex;
    align-items: center;
    flex-shrink: 0;
    color: var(--text-muted);
    opacity: 0.5;
    line-height: 1;
    margin-top: 1px;
  }

  .ws-row-aside {
    flex-shrink: 0;
    align-self: flex-start;
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: var(--space-1);
    min-width: 44px;
  }

  .item-bubble-slot {
    display: block;
    width: 100%;
    height: 16px;
    margin-top: 1px;
  }

  .workspace-sort-time {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .item-bubble {
    /* GitHub-style state pill: a soft solid pastel fill with a
     * near-black foreground for legibility. The bg is mostly the
     * accent color but blended toward white so the swatch reads as
     * "soft solid"; the fg is the same accent darkened toward black
     * so the number always has high contrast against the bg. The
     * literal white/black anchors keep the look identical across
     * light and dark themes (matching GitHub label semantics).
     * Sits in its own flex column with align-self:flex-start so
     * it pins to the row's top edge regardless of the meta line's
     * height. */
    flex-shrink: 0;
    margin-top: 1px;
    height: 16px;
    padding: 0 6px;
    border: 1px solid transparent;
    border-radius: 8px;
    background: var(--bubble-bg);
    color: var(--bubble-fg);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-weight: 700;
    line-height: 1;
    letter-spacing: 0.01em;
    cursor: pointer;
    transition: background-color 80ms ease, border-color 80ms ease,
      color 80ms ease;
  }

  /* Bubble fills mix each accent toward pure white, and the ink toward
     --bubble-ink, in both themes. kit-ui-check deliberately permits pure
     black/white shade constants inside color-mix — white here is a shade
     anchor, not a palette color. */
  .item-bubble.open {
    --bubble-bg: color-mix(in srgb, var(--accent-green) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-green) 25%, var(--bubble-ink));
  }

  .item-bubble.merged {
    --bubble-bg: color-mix(in srgb, var(--accent-purple) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-purple) 25%, var(--bubble-ink));
  }

  .item-bubble.closed {
    --bubble-bg: color-mix(in srgb, var(--accent-red) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-red) 25%, var(--bubble-ink));
  }

  .item-bubble.draft {
    /* Amber matches the app-wide draft treatment (PR list, detail state
     * chip, design system) so a draft PR reads as draft here too, rather
     * than as a muted/neutral pill indistinguishable from other states. */
    --bubble-bg: color-mix(in srgb, var(--accent-amber) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-amber) 25%, var(--bubble-ink));
  }

  /* Open issues use blue instead of the open/green PR treatment so an
   * issue-backed workspace is distinguishable from a PR-backed one. */
  .item-bubble.issue {
    --bubble-bg: color-mix(in srgb, var(--accent-blue) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-blue) 25%, var(--bubble-ink));
  }

  .item-bubble.kata {
    --bubble-bg: color-mix(in srgb, var(--accent-blue) 70%, #ffffff);
    --bubble-fg: color-mix(in srgb, var(--accent-blue) 25%, var(--bubble-ink));
  }

  /* Kata identity labels are short slugs/qualified IDs rather than a
   * fixed-width number, so cap the width and ellipsize to keep the row
   * layout stable. */
  .item-bubble--kata {
    max-width: 117px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .item-bubble:hover {
    border-color: color-mix(in srgb, var(--bubble-fg) 50%, transparent);
  }

  .item-bubble:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
  }

  .push-state {
    flex-shrink: 0;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-variant-numeric: tabular-nums;
    color: var(--text-secondary);
  }

  .push-ahead,
  .push-behind {
    display: inline-flex;
    align-items: center;
    gap: 1px;
  }

  .push-ahead {
    color: var(--accent-green);
  }

  .push-behind {
    color: var(--accent-amber);
  }

  .workspace-diff-stats {
    flex-shrink: 0;
    display: inline-flex;
    font-size: var(--font-size-2xs);
  }

  .workspace-context-menu {
    position: fixed;
    min-width: 224px;
    max-width: min(320px, calc(100vw - 16px));
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    z-index: var(--z-popover);
    padding: 4px 0;
  }

  .workspace-context-heading {
    padding: 6px 12px 7px;
    border-bottom: 1px solid var(--border-muted);
    margin-bottom: 4px;
  }

  .workspace-context-title {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .workspace-context-meta {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    margin-top: 2px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
  }

  .kit-filter-dropdown__section-title {
    padding: 4px 12px;
    font-size: 0.9em;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .kit-filter-dropdown__divider {
    height: 1px;
    background: var(--border-muted);
    margin: 4px 8px;
  }

  .kit-filter-dropdown__item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 4px 12px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    text-align: left;
    cursor: pointer;
    transition: background 0.08s;
    background: transparent;
    border: 0;
  }

  .kit-filter-dropdown__item:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .kit-filter-dropdown__item:not(.active) {
    opacity: 0.5;
  }

  .kit-filter-dropdown__item:disabled {
    cursor: default;
  }

  .kit-filter-dropdown__dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
    background: var(--border-muted);
  }

  .filter-dot--success {
    background: var(--accent-green);
  }

  .filter-dot--warning {
    background: var(--accent-amber);
  }

  .filter-dot--danger {
    background: var(--accent-red);
  }

  .kit-filter-dropdown__label {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .workspace-context-detail {
    flex-shrink: 0;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
  }

  .workspace-context-danger {
    color: var(--accent-red);
  }

  /* Width-aware hiding: shed least-critical chrome first as the
   * rail narrows. Push state outranks diff stats because branch
   * hygiene matters more for "should I open this workspace?" than
   * line counts. */
  @container workspace-rail (max-width: 260px) {
    .workspace-diff-stats {
      display: none;
    }
  }

  @container workspace-rail (max-width: 220px) {
    .push-state {
      display: none;
    }
  }

  /* The sort trigger collapses to its icon before the filter input
   * is squeezed into uselessness. */
  @container workspace-rail (max-width: 240px) {
    .workspace-sort :global(.kit-filter-dropdown__trigger-label) {
      display: none;
    }
  }
</style>
