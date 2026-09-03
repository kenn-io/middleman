<script lang="ts">
  import { StatusBar as KitStatusBar, StatusDot } from "@kenn-io/kit-ui";
  import { Effect } from "effect";
  import { untrack } from "svelte";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import { getStores } from "../../context.js";
  import type { ActivityItem } from "../../api/types.js";
  import { isActivityItemTypeEnabled } from "../../stores/activity.svelte.js";
  import { repoIdentityKey } from "../../utils/repo-label.js";
  import BudgetBars from "./BudgetBars.svelte";
  import BudgetPopover from "./BudgetPopover.svelte";
  import { formatCompact } from "./budget-utils";
  import { getPage } from "../../stores/router.svelte.ts";

  const { activity, pulls, issues, sync, events } = getStores();
  const runtime = getAppRuntime();
  const liveUpdateState = $derived(events.getConnectionState());

  let appVersion = $state("");
  let tick = $state(0);

  $effect(() => {
    const loadVersion = executeGeneratedApiRequest("GET /version", (client, signal) =>
      client.SystemService.getVersion({ signal })
    ).pipe(
      Effect.tap((version) => Effect.sync(() => {
        appVersion = version.version;
      })),
      Effect.catch(() => Effect.void),
    );
    const updateRelativeTimes = Effect.sleep("10 seconds").pipe(
      Effect.andThen(Effect.sync(() => {
        tick += 1;
      })),
      Effect.forever,
    );
    const execution = untrack(() => runtime.runCommand(
      Effect.all([loadVersion, updateRelativeTimes], { concurrency: "unbounded", discard: true }),
      {
        operation: "load status bar version and update relative times",
        safeContext: {},
        onFailure: () => {},
      },
    ));
    return execution.interrupt;
  });

  function syncText(): string {
    void tick;
    const st = sync.getSyncState();
    if (st === null) return "";
    if (st.running) {
      if (st.progress) {
        return `syncing (${st.progress})`;
      }
      return "syncing\u2026";
    }
    if (!st.last_run_at) return "not synced";
    const diffMs = Date.now() - new Date(st.last_run_at).getTime();
    const mins = Math.floor(diffMs / 60_000);
    if (mins < 1) return "synced just now";
    if (mins < 60) return `synced ${mins}m ago`;
    return `synced ${Math.floor(mins / 60)}h ago`;
  }

  const openPulls = $derived(pulls.getPulls().filter((pr) => pr.State === "open"));
  const openIssues = $derived(issues.getIssues().filter((issue) => issue.State === "open"));

  interface RepoBackedItem {
    repo?: {
      provider?: string | undefined;
      platform_host?: string | undefined;
      platform_repo_id?: string | undefined;
      repo_path?: string | undefined;
      owner?: string | undefined;
      name?: string | undefined;
    } | undefined;
    platform_host?: string | undefined;
    repo_owner?: string | undefined;
    repo_name?: string | undefined;
  }

  interface ActivityCountSubject extends RepoBackedItem {
    item_number: number;
    item_state: string;
    item_type: string;
  }

  interface StatusCounts {
    pullRequests: number;
    issues: number;
    repos: number;
  }

  const BOT_SUFFIXES = ["[bot]", "-bot", "bot"];

  function repoKey(item: RepoBackedItem): string {
    const provider = item.repo?.provider ?? "";
    const platformHost = item.repo?.platform_host ?? item.platform_host ?? "";
    const repoPath = item.repo?.repo_path
      ?? [item.repo?.owner ?? item.repo_owner, item.repo?.name ?? item.repo_name]
        .filter(Boolean)
        .join("/");
    return repoIdentityKey({
      provider,
      platformHost,
      platformRepoId: item.repo?.platform_repo_id,
      owner: item.repo?.owner ?? item.repo_owner ?? "",
      name: item.repo?.name ?? item.repo_name ?? "",
      repoPath,
    });
  }

  function activityItemKey(item: ActivityCountSubject): string {
    return `${repoKey(item)}|${item.item_type}|${item.item_number}`;
  }

  function isBot(author: string): boolean {
    const lower = author.toLowerCase();
    return BOT_SUFFIXES.some((suffix) => lower.endsWith(suffix));
  }

  function activityLifecycleState(item: ActivityItem): string {
    if (item.activity_type === "notification") {
      return item.subject_state || item.item_state;
    }
    return item.item_state;
  }

  const globalCounts = $derived.by((): StatusCounts => {
    const repos = new Set<string>();
    for (const pr of openPulls) repos.add(repoKey(pr));
    for (const issue of openIssues) repos.add(repoKey(issue));
    return {
      pullRequests: openPulls.length,
      issues: openIssues.length,
      repos: repos.size,
    };
  });

  const activityCounts = $derived.by((): StatusCounts => {
    const subjects = new Map<string, { item: ActivityCountSubject; lifecycleState: string }>();
    const pullRequests = new Set<string>();
    const issueKeys = new Set<string>();
    const repos = new Set<string>();
    const enabledItemTypes = activity.getEnabledItemTypes();
    const hideBots = activity.getHideBots();

    for (const item of activity.getActivityItems()) {
      if (hideBots && isBot(item.author)) continue;
      if (!isActivityItemTypeEnabled(item.item_type, enabledItemTypes)) continue;

      subjects.set(activityItemKey(item), {
        item,
        lifecycleState: activityLifecycleState(item),
      });
    }

    // Parent summaries carry the authoritative lifecycle state and replace any
    // event-backed snapshot for the same item.
    for (const subject of activity.getItemActivity()) {
      if (hideBots && isBot(subject.item_author ?? "")) continue;
      if (!isActivityItemTypeEnabled(subject.item_type, enabledItemTypes)) continue;
      subjects.set(activityItemKey(subject), {
        item: subject,
        lifecycleState: subject.item_state,
      });
    }

    // Workspace-only subjects are visible threads too, but must not override
    // an authoritative parent or an existing event-backed lifecycle state.
    for (const subject of activity.getWorkspaceActivity()) {
      if (hideBots && isBot(subject.item_author ?? "")) continue;
      if (!isActivityItemTypeEnabled(subject.item_type, enabledItemTypes)) continue;
      const key = activityItemKey(subject);
      if (!subjects.has(key)) {
        subjects.set(key, {
          item: subject,
          lifecycleState: subject.item_state,
        });
      }
    }

    for (const [key, { item, lifecycleState }] of subjects) {
      const isOpenPullRequest = item.item_type === "pr"
        && lifecycleState === "open";
      const isOpenIssue = item.item_type === "issue"
        && lifecycleState === "open";

      if (isOpenPullRequest) {
        pullRequests.add(key);
        repos.add(repoKey(item));
      } else if (isOpenIssue) {
        issueKeys.add(key);
        repos.add(repoKey(item));
      }
    }

    return {
      pullRequests: pullRequests.size,
      issues: issueKeys.size,
      repos: repos.size,
    };
  });

  function isActivityStatusSurface(): boolean {
    const page = getPage();
    return page === "activity" || page === "mobile-activity";
  }

  const counts = $derived(isActivityStatusSurface() ? activityCounts : globalCounts);

  let popoverOpen = $state(false);

  function togglePopover() {
    popoverOpen = !popoverOpen;
  }

  function closePopover() {
    popoverOpen = false;
  }

  let rateLimits = $derived.by(() => {
	void tick;
	return sync.getRateLimits();
  });
  let hasRateLimits = $derived(
	Object.keys(rateLimits.provider_pools).length > 0
	|| Object.keys(rateLimits.local_ceilings).length > 0,
  );

  let hasLocalCeilingFailure = $derived(
    sync.getSyncState()?.last_error_code === "localSyncCeilingExhausted",
  );
  let localCeilingFailure = $derived.by(() => {
    const status = sync.getSyncState();
    if (!status || !hasLocalCeilingFailure) return null;
    const ceiling = status.last_error_ceiling_key
      ? rateLimits.local_ceilings[status.last_error_ceiling_key]
      : undefined;
    if (!ceiling) return null;
    if (
      !status.last_error_ceiling_reset_at
      || ceiling.reset_at !== status.last_error_ceiling_reset_at
    ) return null;
    return {
      spent: ceiling.spent,
      limit: ceiling.limit,
      resetAt: ceiling.reset_at,
    };
  });

  function localCeilingFailureText(): string {
    if (localCeilingFailure === null || localCeilingFailure.limit <= 0) {
      return "local sync ceiling reached";
    }
    return `local sync ceiling reached (${formatCompact(localCeilingFailure.spent)} / ${formatCompact(localCeilingFailure.limit)})`;
  }

  function localCeilingFailureTitle(): string {
    void tick;
    const error = sync.getSyncState()?.last_error ?? "Local sync ceiling reached";
    if (localCeilingFailure === null || localCeilingFailure.resetAt === "") return error;
    const resetMs = new Date(localCeilingFailure.resetAt).getTime() - Date.now();
    if (!Number.isFinite(resetMs) || resetMs <= 0) return error;
    return `${error}; local ceiling resets in ${Math.ceil(resetMs / 60_000)}m`;
  }
</script>

<!-- overflow="visible": the budget popover anchors inside the right section;
     the app owns keeping bar text short in exchange (kit's default section
     truncation is off). -->
<KitStatusBar overflow="visible">
  {#snippet left()}
    <span class="status-item">{counts.pullRequests} PRs</span>
    <span class="status-sep">&middot;</span>
    <span class="status-item">{counts.issues} issues</span>
    <span class="status-sep">&middot;</span>
    <span class="status-item">{counts.repos} repos</span>
  {/snippet}
  {#snippet right()}
    {#if hasRateLimits}
      <span class="budget-wrapper">
        <BudgetBars providerPools={rateLimits.provider_pools} onclick={togglePopover} expanded={popoverOpen} />
        {#if popoverOpen}
          <BudgetPopover
			providerPools={rateLimits.provider_pools}
			localCeilings={rateLimits.local_ceilings}
			onclose={closePopover}
		  />
        {/if}
      </span>
      <span class="status-sep">&middot;</span>
    {/if}
    {#if !sync.getProviderAvailable()}
      <span
        class="status-item status-item--error status-item--provider-unavailable"
        title="This Forge spoke cannot reach its federation hub"
      >
        <StatusDot status="stale" label="Provider unavailable" size={5} animated />
        provider unavailable
      </span>
      <span class="status-sep">&middot;</span>
    {:else if hasLocalCeilingFailure}
      <span
        class="status-item status-item--error status-item--local-ceiling"
        title={localCeilingFailureTitle()}
      >{localCeilingFailureText()}</span>
      <span class="status-sep">&middot;</span>
    {:else if sync.getSyncState()?.last_error}
      <span class="status-item status-item--error" title={sync.getSyncState()?.last_error}>sync error</span>
      <span class="status-sep">&middot;</span>
    {/if}
    {#if liveUpdateState === "reconnecting" || (liveUpdateState === "disconnected" && events.getLastError())}
      {#if liveUpdateState === "disconnected"}
        <button
          type="button"
          class="status-item status-item--error status-item--live-updates live-updates-button"
          aria-label="Reconnect live updates"
          title={events.getLastError() ?? "Reconnect live updates"}
          onclick={events.reconnect}
        >
          <StatusDot status="stale" label="Live updates disconnected" size={5} animated />
          live updates disconnected
        </button>
      {:else}
        <span class="status-item status-item--live-updates">
          <StatusDot status="working" label="Reconnecting live updates" size={5} animated />
          live updates reconnecting
        </span>
      {/if}
      <span class="status-sep">&middot;</span>
    {/if}
    {#if sync.getProviderAvailable()}
      <span class="status-item" class:status-item--active={sync.getSyncState()?.running}>
        {#if sync.getSyncState()?.running}
          <StatusDot status="working" label="Syncing" size={5} animated />
        {/if}
        {syncText()}
      </span>
    {/if}
    {#if appVersion}
      <span class="status-sep">&middot;</span>
      <span class="status-item status-item--version">{appVersion}</span>
    {/if}
  {/snippet}
</KitStatusBar>

<style>
  .status-sep {
    color: var(--border-default);
  }
  .status-item--error {
    color: var(--accent-red);
  }
  .status-item--active {
    color: var(--accent-green);
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .status-item--live-updates {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .status-item--provider-unavailable {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .live-updates-button {
    padding: 0;
    border: 0;
    background: transparent;
    font: inherit;
    cursor: pointer;
  }
  .budget-wrapper {
    position: relative;
    display: flex;
    align-items: center;
  }
</style>
