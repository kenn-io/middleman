<script lang="ts">
  import { copyToClipboard, DiffStats, formatRelativeTime, formatTimestamp, Modal, SearchInput, Spinner, StatusDot, Toggle, type StatusDotStatus } from "@kenn-io/kit-ui";
  import MoreHorizontalIcon from "@lucide/svelte/icons/ellipsis";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import { Effect, Schedule, Stream } from "effect";
  import { onMount, untrack } from "svelte";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { configuredAPIPath } from "../../api/runtime-base.js";
  import { ApiProblemError } from "../../api/effect-errors.js";
  import { ProblemCodes } from "../../api/problems.js";
  import {
    executeGeneratedApiRequest,
    executeOpaqueGeneratedApiRequest,
  } from "../../api/generated-api.js";
  import { loadFleetSnapshot } from "../../api/fleet-snapshot.js";
  import type { HostSummary as GeneratedHostSummary } from "../../api/generated/models/index.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { eventSourceStream } from "../../browser/event-source.js";
  import { openNewWorkspaceDialog } from "../../stores/new-workspace.svelte.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";
  import { notifyWorkspaceDeleted } from "../../stores/workspace-host.svelte.js";
  import ConfirmDialog from "../shared/ConfirmDialog.svelte";
  import {
    WorkspaceListWorkflow,
    makeWorkspaceRefreshHub,
    workspaceListLifecycle,
  } from "../terminal/workspace-list-workflow.js";
  import {
    decodeWorkspaceList,
    retainDegradedHostWorkspaces,
    type WorkspaceListItem,
  } from "../terminal/workspace-list-schema.js";
  import {
    defaultWorkspaceListDisplayOptions,
    defaultWorkspaceListSort,
    loadWorkspaceListDisplayOptions,
    loadWorkspaceListSort,
    saveWorkspaceListDisplayOptions,
    saveWorkspaceListSort,
    workspaceListSortTimestamp,
    workspaceListSortOptions,
    type WorkspaceListDisplayOptions,
    type WorkspaceListSort,
  } from "../terminal/workspaceListSort.js";
  import {
    groupMobileWorkspaces,
    mobileWorkspaceDisplayName,
    mobileWorkspaceItemNumber,
    mobileWorkspaceLinkedItem,
    sortMobileWorkspaces,
    workspaceMatchesMobileSearch,
  } from "./mobile-workspace-list.js";

  type HostSummary = GeneratedHostSummary;

  interface Props {
    onOpen: (workspaceId: string, hostKey?: string) => void;
    onOpenItem: (workspaceId: string, hostKey?: string) => void;
  }

  let { onOpen, onOpenItem }: Props = $props();
  const appRuntime = getAppRuntime();
  const refreshOwner = $props.id();
  const loadTimeout = "10 seconds";

  let workspaces = $state.raw<WorkspaceListItem[]>([]);
  let fleetHosts = $state.raw<HostSummary[]>([]);
  let peerErrors = $state.raw<Record<string, string>>({});
  let fleetError = $state<string | null>(null);
  let listStatus = $state<"loading" | "retrying" | "loaded">("loading");
  let searchQuery = $state("");
  let sortMode = $state<WorkspaceListSort>(loadWorkspaceListSort());
  let displayOptions = $state<WorkspaceListDisplayOptions>(loadWorkspaceListDisplayOptions());
  let viewOpen = $state(false);
  let actionsWorkspace = $state<WorkspaceListItem | null>(null);
  let deleteWorkspace = $state<WorkspaceListItem | null>(null);
  let deleteForce = $state(false);
  let actionBusy = $state<string | null>(null);

  const filtered = $derived(
    workspaces.filter((workspace) => workspaceMatchesMobileSearch(workspace, searchQuery)),
  );
  const groups = $derived(groupMobileWorkspaces(filtered, displayOptions.showOrgNames));
  const flat = $derived(
    sortMode === "repo" ? [] : sortMobileWorkspaces(filtered, sortMode),
  );
  const viewBadgeCount = $derived(
    Number(sortMode !== defaultWorkspaceListSort) +
      Number(displayOptions.showOrgNames !== defaultWorkspaceListDisplayOptions.showOrgNames) +
      Number(displayOptions.showDiffStats !== defaultWorkspaceListDisplayOptions.showDiffStats),
  );
  const fleetDegraded = $derived(fleetError !== null || Object.keys(peerErrors).length > 0);
  const showFleet = $derived(
    fleetDegraded || fleetHosts.some((host) => host.kind !== "self"),
  );

  function failureMessage(failure: unknown, fallback: string): string {
    if (failure instanceof ApiProblemError) return apiErrorMessage(failure.problem, fallback);
    return failure instanceof Error ? failure.message : fallback;
  }

  function workspaceHost(workspace: WorkspaceListItem): HostSummary | undefined {
    if (workspace.fleet_host_key) {
      return fleetHosts.find((host) => host.configKey === workspace.fleet_host_key);
    }
    return fleetHosts.find((host) => host.kind === "self");
  }

  function workspaceOperationAvailable(
    workspace: WorkspaceListItem,
    operation: "workspaceRead" | "workspaceWrite" | "terminalAttach",
  ): boolean {
    if (!workspace.fleet_host_key) return true;
    return workspaceHost(workspace)?.operationAvailability[operation]?.available === true;
  }

  function openWorkspace(workspace: WorkspaceListItem): void {
    if (
      !workspaceOperationAvailable(workspace, "workspaceRead") ||
      !workspaceOperationAvailable(workspace, "terminalAttach")
    ) return;
    onOpen(workspace.id, workspace.fleet_host_key);
  }

  function openWorkspaceItem(workspace: WorkspaceListItem): void {
    if (!workspaceOperationAvailable(workspace, "workspaceRead")) return;
    onOpenItem(workspace.id, workspace.fleet_host_key);
  }

  function loadWorkspaces() {
    return Effect.gen(function* () {
      const payload = yield* loadFleetSnapshot().pipe(Effect.timeout(loadTimeout));
      const decoded = yield* decodeWorkspaceList({ workspaces: payload.workspaces });
      const nextHosts = payload.hosts ?? [];
      workspaces = retainDegradedHostWorkspaces(
        workspaces,
        decoded.filter((workspace) => workspace.visible !== false),
        nextHosts,
        payload.aggregateIncomplete ?? false,
      );
      fleetHosts = nextHosts;
      peerErrors = Object.fromEntries(
        fleetHosts
          .filter((host) => host.kind !== "self" && host.error)
          .map((host) => [host.configKey, host.error ?? "Host unavailable"]),
      );
      fleetError = null;
      listStatus = "loaded";
    });
  }

  const refreshWorkspaces = makeWorkspaceRefreshHub(
    Effect.suspend(loadWorkspaces).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          fleetError = failureMessage(failure, "Fleet unavailable");
          if (workspaces.length === 0) listStatus = "retrying";
        }),
      ),
    ),
  );

  const refreshFleet = refreshWorkspaces;

  function setSort(sort: WorkspaceListSort): void {
    sortMode = sort;
    saveWorkspaceListSort(sort);
  }

  function setDisplayOption(key: keyof WorkspaceListDisplayOptions, value: boolean): void {
    displayOptions = { ...displayOptions, [key]: value };
    saveWorkspaceListDisplayOptions(displayOptions);
  }

  function status(workspace: WorkspaceListItem): StatusDotStatus {
    if (workspace.agent_state === "working" || workspace.tmux_working || workspace.status === "creating") {
      return "working";
    }
    if (workspace.agent_state === "approval" || workspace.agent_state === "input") return "waiting";
    if (workspace.status === "error") return "unclean";
    if (workspace.status === "ready") return "idle";
    return "stale";
  }

  function statusLabel(workspace: WorkspaceListItem): string {
    if (workspace.agent_state === "approval") return "Agent waiting for approval";
    if (workspace.agent_state === "input") return "Agent waiting for input";
    if (workspace.agent_state === "working" || workspace.tmux_working) return "Workspace active";
    if (workspace.status === "ready") return "Workspace ready";
    if (workspace.status === "creating") return "Creating workspace";
    if (workspace.status === "error") return "Workspace error";
    return `Workspace ${workspace.status}`;
  }

  function agentStatePresentation(workspace: WorkspaceListItem): {
    label: "Working" | "Approval" | "Input" | "Done" | "Idle";
    tone: "working" | "approval" | "input" | "done" | "idle";
    announcement: "working" | "waiting for approval" | "waiting for input" | "done" | "idle";
  } | null {
    switch (workspace.agent_state) {
      case "working":
        return { label: "Working", tone: "working", announcement: "working" };
      case "approval":
        return { label: "Approval", tone: "approval", announcement: "waiting for approval" };
      case "input":
        return { label: "Input", tone: "input", announcement: "waiting for input" };
      case "done":
        return { label: "Done", tone: "done", announcement: "done" };
      case "idle":
        return sortMode === "agent-status" ? { label: "Idle", tone: "idle", announcement: "idle" } : null;
      default:
        return null;
    }
  }

  function itemLabel(workspace: WorkspaceListItem): string | null {
    const number = mobileWorkspaceItemNumber(workspace);
    return number === null ? null : `#${number}`;
  }

  function providerItemURL(workspace: WorkspaceListItem): string | null {
    const linked = mobileWorkspaceLinkedItem(workspace);
    const provider = workspace.repo?.provider.toLowerCase();
    const repoPath = workspace.repo?.repo_path ?? `${workspace.repo_owner}/${workspace.repo_name}`;
    if (linked === null || !provider || !workspace.platform_host || !repoPath) return null;
    const number = linked.number;
    const encodedPath = repoPath.split("/").map(encodeURIComponent).join("/");
    const kind = linked.itemType === "issue" ? "issues" : provider === "github" ? "pull" : provider === "gitlab" ? "merge_requests" : "pulls";
    const separator = provider === "gitlab" ? "/-/" : "/";
    return `https://${workspace.platform_host}/${encodedPath}${separator}${kind}/${number}`;
  }

  function copyText(value: string, message: string): void {
    actionsWorkspace = null;
    appRuntime.runCommand(
      Effect.promise(() => copyToClipboard(value)).pipe(
        Effect.tap((copied) =>
          Effect.sync(() => showFlash(copied ? message : "Could not copy to clipboard.", copied ? undefined : { tone: "danger" })),
        ),
        Effect.asVoid,
      ),
      {
        operation: "copy mobile workspace value",
        safeContext: {},
        onFailure: () => showFlash("Could not copy to clipboard.", { tone: "danger" }),
      },
    );
  }

  function runWorkspaceAction(
    workspace: WorkspaceListItem,
    action: "push" | "pull" | "refresh" | "reveal",
  ): void {
    if (actionBusy || workspaceActionsDisabled(workspace)) return;
    actionBusy = `${workspace.fleet_host_key ?? "local"}:${workspace.id}:${action}`;
    const hostKey = workspace.fleet_host_key;
    const command = hostKey
      ? executeOpaqueGeneratedApiRequest<unknown>(`${action} mobile Fleet workspace`, (client, signal) => {
          const params = { hostKey, id: workspace.id };
          switch (action) {
            case "push": return client.FleetService.pushFleetWorkspaceBranch(params, { signal });
            case "pull": return client.FleetService.pullFleetWorkspaceBranch(params, { signal });
            case "refresh": return client.FleetService.refreshFleetWorkspace(params, { signal });
            case "reveal": return client.FleetService.revealFleetWorkspace(params, { signal });
          }
        }).pipe(Effect.asVoid)
      : executeGeneratedApiRequest<unknown>(`${action} mobile workspace`, (client, signal) => {
          const params = { id: workspace.id };
          switch (action) {
            case "push": return client.WorkspacesService.pushWorkspaceBranch(params, { signal });
            case "pull": return client.WorkspacesService.pullWorkspaceBranch(params, { signal });
            case "refresh": return client.WorkspacesService.refreshWorkspace(params, { signal });
            case "reveal": return client.WorkspacesService.revealWorkspace(params, { signal });
          }
        }).pipe(Effect.asVoid);
    appRuntime.runCommand(
      command.pipe(
        Effect.tap(() => Effect.sync(refreshWorkspaces.request)),
        Effect.ensuring(
          Effect.sync(() => {
            actionBusy = null;
            actionsWorkspace = null;
          }),
        ),
      ),
      {
        operation: `${action} mobile workspace`,
        safeContext: { workspaceId: workspace.id, remote: Boolean(hostKey) },
        onFailure: (failure) => showFlash(failureMessage(failure, `${action} failed.`), { tone: "danger" }),
      },
    );
  }

  function confirmDelete(): void {
    const workspace = deleteWorkspace;
    if (!workspace || actionBusy || !workspaceOperationAvailable(workspace, "workspaceWrite")) return;
    const force = deleteForce;
    actionBusy = `${workspace.fleet_host_key ?? "local"}:${workspace.id}:delete`;
    const hostKey = workspace.fleet_host_key;
    const command = hostKey
      ? executeOpaqueGeneratedApiRequest("delete mobile Fleet workspace", (client, signal) =>
          client.FleetService.deleteFleetWorkspace(
            { hostKey, id: workspace.id },
            force ? { force: true } : undefined,
            { signal },
          ),
        ).pipe(Effect.asVoid)
      : executeGeneratedApiRequest("delete mobile workspace", (client, signal) =>
          client.WorkspacesService.deleteWorkspace(
            { id: workspace.id },
            force ? { force: true } : undefined,
            { signal },
          ),
        ).pipe(Effect.asVoid);
    appRuntime.runCommand(
      command.pipe(
        Effect.tap(() =>
          Effect.sync(() => {
            notifyWorkspaceDeleted(workspace.id, hostKey, {
              provider: workspace.repo?.provider ?? "",
              platformHost: workspace.platform_host,
              owner: workspace.repo_owner,
              name: workspace.repo_name,
              repoPath: workspace.repo?.repo_path ?? `${workspace.repo_owner}/${workspace.repo_name}`,
              number: workspace.item_number,
              itemType: workspace.item_type,
            });
            workspaces = workspaces.filter(
              (candidate) => candidate.id !== workspace.id || candidate.fleet_host_key !== hostKey,
            );
            deleteWorkspace = null;
            deleteForce = false;
            refreshWorkspaces.request();
          }),
        ),
        Effect.ensuring(
          Effect.sync(() => {
            actionBusy = null;
          }),
        ),
      ),
      {
        operation: "delete mobile workspace",
        safeContext: { workspaceId: workspace.id, remote: Boolean(hostKey) },
        onFailure: (failure) => {
          const problem = failure instanceof ApiProblemError ? failure.problem : undefined;
          if (!force && problem?.code === ProblemCodes.worktreeDirty) {
            deleteWorkspace = workspace;
            deleteForce = true;
            return;
          }
          deleteWorkspace = null;
          deleteForce = false;
          showFlash(failureMessage(failure, "Delete failed."), { tone: "danger" });
        },
      },
    );
  }

  function canPush(workspace: WorkspaceListItem): boolean {
    return workspace.branch_upstream_missing === true ||
      ((workspace.commits_ahead ?? 0) > 0 && (workspace.commits_behind ?? 0) === 0);
  }

  function canPull(workspace: WorkspaceListItem): boolean {
    return (workspace.commits_behind ?? 0) > 0 && (workspace.commits_ahead ?? 0) === 0;
  }

  function workspaceActionsDisabled(workspace: WorkspaceListItem): boolean {
    return workspace.status === "deleting" || workspace.status === "deletion_failed" ||
      !workspaceOperationAvailable(workspace, "workspaceWrite");
  }

  function runSheetAction(action: "push" | "pull" | "refresh" | "reveal"): void {
    if (actionsWorkspace) runWorkspaceAction(actionsWorkspace, action);
  }

  function copySheetValue(kind: "branch" | "path"): void {
    const workspace = actionsWorkspace;
    if (!workspace) return;
    if (kind === "branch") copyText(workspace.git_head_ref, "Copied branch name.");
    else copyText(workspace.worktree_path, "Copied worktree path.");
  }

  function openSheetProviderItem(url: string): void {
    window.open(url, "_blank", "noopener,noreferrer");
    actionsWorkspace = null;
  }

  function promptSheetDelete(force = false): void {
    deleteWorkspace = actionsWorkspace;
    deleteForce = force;
    actionsWorkspace = null;
  }

  function closeDeleteConfirmation(): void {
    if (actionBusy?.endsWith(":delete")) return;
    deleteWorkspace = null;
    deleteForce = false;
  }

  $effect(() => {
    if (!viewOpen) return;
    return untrack(() => pushModalFrame("mobile-workspace-view-options", []));
  });

  $effect(() => {
    if (!actionsWorkspace) return;
    return untrack(() => pushModalFrame("mobile-workspace-actions", []));
  });

  onMount(() => {
    const events = eventSourceStream(configuredAPIPath("/events"), "workspace_status").pipe(
      Stream.retry(Schedule.exponential("1 second").pipe(Schedule.jittered)),
      Stream.catch(() => Stream.empty),
    );
    const execution = appRuntime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          const workflow = yield* WorkspaceListWorkflow;
          yield* workflow.claim(refreshOwner, refreshWorkspaces.request);
          yield* workspaceListLifecycle({ refreshWorkspaces, refreshFleet, workspaceEvents: events });
        }),
      ),
      {
        operation: "run mobile workspace list",
        safeContext: {},
        onFailure: () => {
          if (workspaces.length === 0) listStatus = "retrying";
        },
      },
    );
    return execution.interrupt;
  });
</script>

<section class="mobile-workspace-list" aria-label="Workspaces">
  <div class="mobile-workspace-list__controls">
    <div class="mobile-workspace-list__actions">
      <button
        class="mobile-workspace-list__view"
        type="button"
        aria-label="View workspace options"
        onclick={() => (viewOpen = true)}
      >
        View
        {#if viewBadgeCount > 0}<span>{viewBadgeCount}</span>{/if}
      </button>
      <button
        class="mobile-workspace-list__new"
        type="button"
        aria-label="New workspace"
        onclick={() => openNewWorkspaceDialog()}
      >
        <PlusIcon size="18" strokeWidth="2" aria-hidden="true" />
        New
      </button>
    </div>
    <SearchInput
      value={searchQuery}
      block
      placeholder="Filter workspaces"
      ariaLabel="Filter workspaces"
      oninput={(value) => (searchQuery = value)}
    />
  </div>

  {#if showFleet}
    <div class={["mobile-workspace-list__fleet", { degraded: fleetDegraded }]}>
      <span>Fleet</span>
      {#if fleetDegraded}
        <strong>Degraded</strong>
      {:else}
        <strong>{fleetHosts.filter((host) => host.reachable).length}/{fleetHosts.length}</strong>
      {/if}
    </div>
  {/if}

  <div class="mobile-workspace-list__scroll">
    {#if listStatus !== "loaded" && workspaces.length === 0}
      <div class="mobile-workspace-list__state">
        <Spinner size={18} />
        <span>{listStatus === "retrying" ? "Retrying workspace list…" : "Loading workspaces…"}</span>
      </div>
    {:else if filtered.length === 0}
      <div class="mobile-workspace-list__state">
        <strong>{searchQuery.trim() ? "No matching workspaces" : "No workspaces yet"}</strong>
        <span>{searchQuery.trim() ? "Try a repository, branch, title, or item number." : "Create one to start a terminal session."}</span>
      </div>
    {:else if sortMode === "repo"}
      {#each groups as group (group.key)}
        <section class="mobile-workspace-group">
          <h2>{group.label}<span>{group.items.length}</span></h2>
          {#each group.items as workspace (`${workspace.fleet_host_key ?? "local"}:${workspace.id}`)}
            {@render workspaceRow(workspace, false)}
          {/each}
        </section>
      {/each}
    {:else}
      {#each flat as workspace (`${workspace.fleet_host_key ?? "local"}:${workspace.id}`)}
        {@render workspaceRow(workspace, true)}
      {/each}
    {/if}
  </div>
</section>

{#snippet workspaceRow(workspace: WorkspaceListItem, showRepository: boolean)}
  {@const label = itemLabel(workspace)}
  {@const agentState = agentStatePresentation(workspace)}
  {@const sortTimestamp = workspaceListSortTimestamp(workspace, sortMode)}
  <article class="mobile-workspace-row">
    <button
      class="mobile-workspace-row__main"
      type="button"
      aria-label={`Open workspace ${mobileWorkspaceDisplayName(workspace)}${agentState ? `, agent ${agentState.announcement}` : ""}`}
      disabled={!workspaceOperationAvailable(workspace, "workspaceRead") || !workspaceOperationAvailable(workspace, "terminalAttach")}
      onclick={() => openWorkspace(workspace)}
    >
      <span class="mobile-workspace-row__title">
        <StatusDot status={status(workspace)} label={statusLabel(workspace)} size={7} animated />
        <strong>{mobileWorkspaceDisplayName(workspace)}</strong>
        {#if agentState}
          <span
            class={["mobile-workspace-row__agent-state", `mobile-workspace-row__agent-state--${agentState.tone}`]}
            aria-hidden="true"
          >{agentState.label}</span>
        {/if}
      </span>
      <span class="mobile-workspace-row__meta">
        {#if workspace.fleet_host_key}<em>{workspace.fleet_host_key}</em>{/if}
        {#if showRepository}
          <span>{displayOptions.showOrgNames ? `${workspace.repo_owner}/${workspace.repo_name}` : workspace.repo_name}</span>
        {/if}
        <code>{workspace.git_head_ref.replace(/^refs\/heads\//, "")}</code>
        {#if displayOptions.showDiffStats && workspace.mr_additions !== null && workspace.mr_additions !== undefined}
          <DiffStats additions={workspace.mr_additions} deletions={workspace.mr_deletions ?? 0} />
        {/if}
      </span>
    </button>
    {#if label || sortTimestamp}
      <span class="mobile-workspace-row__item-stack">
        {#if label}
          <button
            class="mobile-workspace-row__item"
            type="button"
            aria-label={`Open linked item ${label}`}
            onclick={() => openWorkspaceItem(workspace)}
            disabled={!workspaceOperationAvailable(workspace, "workspaceRead")}
            title={!workspaceOperationAvailable(workspace, "workspaceRead") ? "Linked item details are unavailable from this Forge" : undefined}
          >{label}</button>
        {/if}
        {#if sortTimestamp}
          <time
            class="mobile-workspace-row__sort-time"
            datetime={sortTimestamp.at}
            title={`${sortTimestamp.label}: ${formatTimestamp(sortTimestamp.at)}`}
            aria-label={`${sortTimestamp.label}: ${formatRelativeTime(sortTimestamp.at)}`}
          >{formatRelativeTime(sortTimestamp.at)}</time>
        {/if}
      </span>
    {/if}
    <button
      class="mobile-workspace-row__more"
      type="button"
      aria-label={`Workspace actions for ${mobileWorkspaceDisplayName(workspace)}`}
      onclick={() => (actionsWorkspace = workspace)}
    >
      <MoreHorizontalIcon size="20" strokeWidth="2" aria-hidden="true" />
    </button>
  </article>
{/snippet}

{#if viewOpen}
  <Modal
    title="View workspaces"
    ariaLabel="View workspace options"
    closeLabel="Close View options"
    width="min(100%, 38rem)"
    maxWidth="100%"
    onclose={() => (viewOpen = false)}
  >
    <div class="mobile-sheet-content">
      <fieldset>
        <legend>Sort and group</legend>
        {#each workspaceListSortOptions as option (option.value)}
          <label>
            <input type="radio" name="workspace-sort" value={option.value} checked={sortMode === option.value} onchange={() => setSort(option.value)} />
            <span><strong>{option.value === "activity" ? "Terminal activity" : option.label}</strong><small>{option.description}</small></span>
          </label>
        {/each}
      </fieldset>
      <div class="mobile-sheet__switches">
        <Toggle checked={displayOptions.showOrgNames} ariaLabel="Show organization names" onchange={(checked) => setDisplayOption("showOrgNames", checked)}>
          <span><strong>Show organization names</strong><small>Keep similarly named repositories distinct.</small></span>
        </Toggle>
        <Toggle checked={displayOptions.showDiffStats} ariaLabel="Show PR diff stats" onchange={(checked) => setDisplayOption("showDiffStats", checked)}>
          <span><strong>Show PR diff stats</strong><small>Show additions and deletions on linked pull requests.</small></span>
        </Toggle>
      </div>
    </div>
  </Modal>
{/if}

{#if actionsWorkspace}
  {@const itemURL = providerItemURL(actionsWorkspace)}
  <Modal
    title={mobileWorkspaceDisplayName(actionsWorkspace)}
    ariaLabel="Workspace actions"
    closeLabel="Close workspace actions"
    width="min(100%, 38rem)"
    maxWidth="100%"
    onclose={() => (actionsWorkspace = null)}
  >
    <div class="mobile-sheet-content mobile-sheet--actions">
      <small class="mobile-sheet__branch">{actionsWorkspace.git_head_ref}</small>
      <div class="mobile-sheet__action-list">
        {#if canPush(actionsWorkspace)}<button type="button" disabled={actionBusy !== null || workspaceActionsDisabled(actionsWorkspace)} onclick={() => runSheetAction("push")}>Push branch</button>{/if}
        {#if canPull(actionsWorkspace)}<button type="button" disabled={actionBusy !== null || workspaceActionsDisabled(actionsWorkspace)} onclick={() => runSheetAction("pull")}>Pull remote changes</button>{/if}
        <button type="button" disabled={actionBusy !== null || workspaceActionsDisabled(actionsWorkspace)} onclick={() => runSheetAction("refresh")}>Refresh workspace</button>
        <button type="button" disabled={actionBusy !== null || workspaceActionsDisabled(actionsWorkspace)} onclick={() => runSheetAction("reveal")}>Reveal worktree</button>
        <button type="button" onclick={() => copySheetValue("branch")}>Copy branch name</button>
        <button type="button" onclick={() => copySheetValue("path")}>Copy worktree path</button>
        {#if itemURL}
          <button type="button" onclick={() => openSheetProviderItem(itemURL)}>Open item on provider</button>
          <button type="button" onclick={() => copyText(itemURL, "Copied item URL.")}>Copy item URL</button>
        {/if}
        <button class="danger" type="button" disabled={actionBusy !== null || !workspaceOperationAvailable(actionsWorkspace, "workspaceWrite") || actionsWorkspace.status === "deleting"} onclick={() => promptSheetDelete(false)}>{actionsWorkspace.status === "deletion_failed" ? "Retry deletion…" : "Delete workspace…"}</button>
        {#if actionsWorkspace.status === "deletion_failed"}
          <button class="danger" type="button" disabled={actionBusy !== null || !workspaceOperationAvailable(actionsWorkspace, "workspaceWrite")} onclick={() => promptSheetDelete(true)}>Force delete workspace…</button>
        {/if}
      </div>
    </div>
  </Modal>
{/if}

<ConfirmDialog
  open={deleteWorkspace !== null}
  title={deleteForce ? "Force delete workspace?" : deleteWorkspace?.status === "deletion_failed" ? "Retry workspace deletion?" : "Delete workspace?"}
  message={deleteWorkspace ? `Delete workspace "${mobileWorkspaceDisplayName(deleteWorkspace)}"?` : ""}
  hint={deleteForce
    ? "This discards uncommitted changes and removes the managed worktree and runtime sessions."
    : deleteWorkspace?.error_message ?? "This removes its managed worktree and runtime sessions."}
  confirmLabel={deleteForce ? "Force delete workspace" : deleteWorkspace?.status === "deletion_failed" ? "Retry deletion" : "Delete workspace"}
  pendingLabel="Deleting…"
  busy={actionBusy?.endsWith(":delete") ?? false}
  tone="danger"
  frameId="mobile-workspace-delete"
  onCancel={closeDeleteConfirmation}
  onConfirm={confirmDelete}
/>

<style>
  .mobile-workspace-list { flex: 1; min-height: 0; display: flex; flex-direction: column; background: var(--bg-primary); }
  .mobile-workspace-list__controls { display: grid; gap: 0.625rem; padding: 0.75rem; border-bottom: thin solid var(--border-default); background: var(--bg-surface); }
  .mobile-workspace-list__actions { display: flex; justify-content: flex-end; gap: 0.5rem; }
  .mobile-workspace-list__view, .mobile-workspace-list__new { min-height: 2.75rem; display: inline-flex; align-items: center; justify-content: center; gap: 0.375rem; padding: 0 0.875rem; border: thin solid var(--border-default); border-radius: var(--radius-md); color: var(--text-primary); background: var(--bg-inset); font: inherit; font-size: var(--font-size-md); font-weight: 650; }
  .mobile-workspace-list__view span { min-width: 1.25rem; padding: 0.0625rem 0.375rem; border-radius: 999px; color: var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 14%, transparent); font-size: var(--font-size-sm); }
  .mobile-workspace-list__new { color: var(--text-on-accent); border-color: var(--accent-blue); background: var(--accent-blue); }
  .mobile-workspace-list__fleet { min-height: 2rem; display: flex; align-items: center; justify-content: space-between; padding: 0.375rem 0.75rem; color: var(--text-secondary); border-bottom: thin solid var(--border-muted); font-size: var(--font-size-sm); }
  .mobile-workspace-list__fleet strong { color: var(--accent-green); }
  .mobile-workspace-list__fleet.degraded strong { color: var(--accent-amber); }
  .mobile-workspace-list__scroll { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; padding-bottom: max(1rem, env(safe-area-inset-bottom)); }
  .mobile-workspace-list__state { min-height: 12rem; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 0.5rem; padding: 2rem; color: var(--text-muted); text-align: center; font-size: var(--font-size-md); }
  .mobile-workspace-list__state strong { color: var(--text-primary); }
  .mobile-workspace-group h2 { min-height: 2.5rem; display: flex; align-items: center; justify-content: space-between; margin: 0; padding: 0.5rem 0.875rem; color: var(--text-muted); border-bottom: thin solid var(--border-muted); background: var(--bg-inset); font-size: var(--font-size-sm); font-weight: 700; letter-spacing: 0.055em; text-transform: uppercase; }
  .mobile-workspace-group h2 span { font-family: var(--font-mono); font-weight: 500; }
  .mobile-workspace-row { display: grid; grid-template-columns: minmax(0, 1fr) auto auto; align-items: stretch; border-bottom: thin solid var(--border-default); background: var(--bg-surface); }
  .mobile-workspace-row:focus-within { box-shadow: inset 0 0 0 1px var(--accent-blue); }
  .mobile-workspace-row button { border: 0; color: inherit; background: transparent; font: inherit; }
  .mobile-workspace-row__main { min-width: 0; min-height: 5.5rem; display: flex; flex-direction: column; justify-content: center; gap: 0.5rem; padding: 0.75rem 0.25rem 0.75rem 0.875rem; text-align: left; }
  .mobile-workspace-row__title { min-width: 0; display: flex; align-items: center; gap: 0.5rem; }
  .mobile-workspace-row__title strong { min-width: 0; flex: 1; overflow: hidden; color: var(--text-primary); font-size: var(--font-size-md); font-weight: 650; line-height: 1.25; text-overflow: ellipsis; white-space: nowrap; }
  .mobile-workspace-row__agent-state { flex-shrink: 0; padding: 0.1875rem 0.375rem; border: thin solid currentColor; border-radius: 999px; background: var(--bg-inset); font-size: var(--font-size-xs); font-weight: 700; line-height: 1; }
  .mobile-workspace-row__agent-state--working, .mobile-workspace-row__agent-state--done { color: var(--accent-green); }
  .mobile-workspace-row__agent-state--approval { color: var(--accent-amber); }
  .mobile-workspace-row__agent-state--input { color: var(--accent-purple); }
  .mobile-workspace-row__agent-state--idle { color: var(--text-muted); }
  .mobile-workspace-row__meta { min-width: 0; display: flex; align-items: center; gap: 0.5rem; overflow: hidden; color: var(--text-muted); font-size: var(--font-size-sm); }
  .mobile-workspace-row__meta > span, .mobile-workspace-row__meta code, .mobile-workspace-row__meta em { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mobile-workspace-row__meta code { flex: 1; color: var(--text-secondary); font-family: var(--font-mono); }
  .mobile-workspace-row__meta em { color: var(--accent-blue); font-style: normal; font-weight: 650; }
  .mobile-workspace-row__item-stack { align-self: center; display: flex; flex-direction: column; align-items: center; gap: 0.125rem; margin: 0.25rem; }
  .mobile-workspace-row__item, .mobile-workspace-row__more { align-self: center; min-width: 2.75rem; min-height: 2.75rem; margin: 0.25rem; border-radius: var(--radius-md) !important; }
  .mobile-workspace-row__item { height: 2rem; min-width: auto; min-height: 2rem; margin: 0; padding: 0 0.625rem !important; color: var(--text-on-accent) !important; background: var(--accent-green) !important; font-family: var(--font-mono) !important; font-weight: 700 !important; }
  .mobile-workspace-row__sort-time { color: var(--text-muted); font-size: var(--font-size-sm); line-height: 1.35; white-space: nowrap; }
  .mobile-workspace-row__item:disabled { cursor: not-allowed; opacity: var(--opacity-disabled); }
  .mobile-workspace-row__more { display: inline-flex; align-items: center; justify-content: center; color: var(--text-muted) !important; }
  .mobile-workspace-row button:focus-visible, .mobile-sheet-content button:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: -2px; }
  :global(.kit-modal-overlay:has(.mobile-sheet-content)) { align-items: flex-end; }
  :global(.kit-modal-panel:has(.mobile-sheet-content)) { max-height: min(82vh, 44rem); border-bottom: 0; border-radius: var(--radius-lg) var(--radius-lg) 0 0; }
  :global(.kit-modal-body:has(> .mobile-sheet-content)) { padding: 0 0 max(1rem, env(safe-area-inset-bottom)); }
  .mobile-sheet-content fieldset { margin: 0; padding: 0.875rem; border: 0; }
  .mobile-sheet-content legend { padding: 0; color: var(--text-muted); font-size: var(--font-size-sm); font-weight: 700; letter-spacing: 0.055em; text-transform: uppercase; }
  .mobile-sheet-content fieldset label { min-height: 3.75rem; display: flex; align-items: center; gap: 0.75rem; border-bottom: thin solid var(--border-muted); }
  .mobile-sheet-content fieldset input { width: 1.25rem; height: 1.25rem; accent-color: var(--accent-blue); }
  .mobile-sheet-content label span, .mobile-sheet__switches :global(.kit-toggle__label > span) { min-width: 0; display: flex; flex: 1; flex-direction: column; gap: 0.125rem; text-align: left; }
  .mobile-sheet-content label strong, .mobile-sheet__switches strong { color: var(--text-primary); font-size: var(--font-size-md); }
  .mobile-sheet-content label small, .mobile-sheet__switches small { color: var(--text-muted); font-size: var(--font-size-sm); line-height: 1.35; }
  .mobile-sheet__switches { padding: 0 0.875rem; }
  .mobile-sheet__switches :global(.kit-toggle) { width: 100%; min-height: 4rem; border-top: thin solid var(--border-muted); }
  .mobile-sheet__switches :global(.kit-toggle__label) { min-width: 0; flex: 1; }
  .mobile-sheet__branch { display: block; overflow: hidden; padding: 0.625rem 0.875rem; color: var(--text-muted); border-bottom: thin solid var(--border-muted); font-family: var(--font-mono); font-size: var(--font-size-sm); text-overflow: ellipsis; white-space: nowrap; }
  .mobile-sheet__action-list { display: grid; }
  .mobile-sheet__action-list button { min-height: 3.25rem; padding: 0 1rem; border: 0; border-bottom: thin solid var(--border-muted); color: var(--text-primary); background: transparent; font: inherit; font-size: var(--font-size-md); text-align: left; }
  .mobile-sheet__action-list button.danger { color: var(--accent-red); }
  .mobile-sheet__action-list button:disabled { color: var(--text-muted); }
</style>
