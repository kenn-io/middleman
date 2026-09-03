<script lang="ts">
  import type { PullRequest } from "../../api/types.js";
  import type { Action } from "../../types.js";
  import { getStores, getHostState } from "../../context.js";
  import { formatRelativeTime } from "@kenn-io/kit-ui";
  import { hashColor } from "@kenn-io/kit-ui";
  import { parseCIChecks, bucketCIChecks, safeDiagnosticText } from "../../utils/ci-buckets.js";
  import {
    warnOnUnknownConclusions,
    warnOnMalformedCIChecksJSON,
  } from "../../utils/ci-buckets-warn.js";
  import CITokenCluster, { composeAriaLabel } from "../shared/CITokenCluster.svelte";
  import CircleAlertIcon from "@lucide/svelte/icons/circle-alert";
  import CircleCheckBigIcon from "@lucide/svelte/icons/circle-check-big";
  import Layers2Icon from "@lucide/svelte/icons/layers-2";
  import OctagonXIcon from "@lucide/svelte/icons/octagon-x";
  import LabelRow from "../shared/LabelRow.svelte";
  import WorkspaceIndicator from "../shared/WorkspaceIndicator.svelte";
  import SidebarTitlePopover from "./SidebarTitlePopover.svelte";
  import { repoIdentityKey } from "../../utils/repo-label.js";
  import { effectiveActivity } from "../../utils/effective-activity.js";

  const { pulls, activity } = getStores();
  const hostState = getHostState();

  interface Props {
    pr: PullRequest;
    selected: boolean;
    showRepo: boolean;
    repoLabel: string;
    onclick: () => void;
    importAction?: Action | undefined;
  }

  const {
    pr,
    selected,
    showRepo,
    repoLabel,
    onclick,
    importAction,
  }: Props = $props();

  function handleStarClick(e: MouseEvent): void {
    e.stopPropagation();
    pulls.togglePRStar(
      {
        provider: pr.repo.provider,
        platformHost: pr.repo.platform_host,
        owner: pr.repo.owner,
        name: pr.repo.name,
        repoPath: pr.repo.repo_path,
      },
      pr.Number,
      pr.Starred,
    );
  }

  let el = $state<HTMLButtonElement>();

  $effect(() => {
    if (selected && el) {
      el.scrollIntoView({ block: "nearest", behavior: "smooth" });
    }
  });

  const activityTime = $derived(effectiveActivity(
    pr.LastActivityAt,
    pr.last_workspace_activity_at,
    activity.getUseWorkspaceActivityForRecency(),
  ));
  const ago = $derived(formatRelativeTime(activityTime.at));
  const hasWorktree = $derived(
    (pr.worktree_links?.length ?? 0) > 0,
  );
  const isActiveWorktree = $derived.by(() => {
    const key = hostState.getActiveWorktreeKey?.();
    if (!key || !pr.worktree_links) return false;
    return pr.worktree_links.some(
      (l) => l.worktree_key === key,
    );
  });

  type PRState = "open" | "draft" | "closed" | "merged";
  const prState = $derived.by((): PRState => {
    if (pr.State === "merged") return "merged";
    if (pr.State === "closed") return "closed";
    if (pr.IsDraft) return "draft";
    return "open";
  });

  const stateColors: Record<PRState, string> = {
    open: "var(--accent-green)",
    draft: "var(--accent-amber)",
    closed: "var(--accent-red)",
    merged: "var(--accent-purple)",
  };

  const worktreeName = $derived(
    pr.worktree_links?.[0]?.worktree_branch ??
    pr.worktree_links?.[0]?.worktree_key,
  );

  const showImport = $derived(
    importAction &&
    !hasWorktree &&
    pr.State === "open",
  );
  const labels = $derived(pr.labels ?? []);
  const repoColorKey = $derived(repoIdentityKey({
    provider: pr.repo.provider,
    platformHost: pr.repo.platform_host,
    owner: pr.repo.owner,
    name: pr.repo.name,
    repoPath: pr.repo.repo_path,
  }));
  const reviewDecision = $derived(pr.ReviewDecision.trim().toUpperCase());
  const reviewIndicator = $derived.by(
    ():
      | { kind: "approved"; label: string }
      | { kind: "changes-requested"; label: string }
      | null => {
      if (reviewDecision === "APPROVED") {
        return { kind: "approved", label: "PR approved" };
      }
      if (reviewDecision === "CHANGES_REQUESTED") {
        return { kind: "changes-requested", label: "Changes requested" };
      }
      return null;
    },
  );

  function handleImportClick(e: MouseEvent): void {
    e.stopPropagation();
    importAction?.handler({
      surface: "pull-list",
      owner: pr.repo_owner ?? "",
      name: pr.repo_name ?? "",
      number: pr.Number,
    });
  }

  const parsed = $derived(parseCIChecks(pr.CIChecksJSON));
  const bucketed = $derived(bucketCIChecks(parsed.checks));

  $effect(() => {
    if (parsed.error !== null) {
      warnOnMalformedCIChecksJSON(pr.CIChecksJSON, parsed.error, {
        repo: `${pr.repo_owner}/${pr.repo_name}`,
        number: pr.Number,
      });
    }
  });

  $effect(() => {
    if (bucketed.unknown.length > 0) {
      warnOnUnknownConclusions(bucketed.unknown, {
        repo: `${pr.repo_owner}/${pr.repo_name}`,
        number: pr.Number,
      });
    }
  });
</script>

<button
  class="pull-item pr-list-row"
  class:selected
  class:active-worktree={isActiveWorktree}
  bind:this={el}
  onclick={onclick}
>
  <p class="title">
    <span class="state-dot" style="background: {stateColors[prState]}"></span>
    <span class="title-text">{pr.Title}</span>
    <LabelRow {labels} compact />
    <span class="item-number">#{pr.Number}</span>
  </p>
  <div class="meta-row">
    <span class="meta-left">
      {#if showRepo}
        <span class="repo-name" style={`color: ${hashColor(repoColorKey)}`} title={repoLabel}>{repoLabel}</span>
        <span class="meta-sep">·</span>
      {/if}
      <span class="meta-text">{pr.Author}</span>
    </span>
    <span class="meta-right">
      {#if showImport}
        <span
          class="import-btn"
          role="button"
          tabindex="-1"
          onclick={handleImportClick}
          onkeydown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); handleImportClick(e as unknown as MouseEvent); } }}
          title="Import to worktree"
        >
          <svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor">
            <path d="M8 1a.75.75 0 01.75.75v6.19l1.72-1.72a.75.75 0 111.06 1.06l-3 3a.75.75 0 01-1.06 0l-3-3a.75.75 0 011.06-1.06l1.72 1.72V1.75A.75.75 0 018 1zM3.5 10a.75.75 0 01.75.75v1.5c0 .138.112.25.25.25h7a.25.25 0 00.25-.25v-1.5a.75.75 0 011.5 0v1.5A1.75 1.75 0 0111.5 14h-7A1.75 1.75 0 012.75 12.25v-1.5A.75.75 0 013.5 10z"/>
          </svg>
        </span>
      {/if}
      {#if pr.workspace}
        <WorkspaceIndicator status={pr.workspace.status} />
      {/if}
      {#if reviewIndicator}
        <span
          class={["review-indicator", `review-indicator--${reviewIndicator.kind}`]}
          aria-label={reviewIndicator.label}
          title={reviewIndicator.label}
        >
          {#if reviewIndicator.kind === "approved"}
            <CircleCheckBigIcon size={13} strokeWidth={2.2} aria-hidden="true" />
          {:else}
            <OctagonXIcon size={13} strokeWidth={2.2} aria-hidden="true" />
          {/if}
        </span>
      {/if}
      {#if hasWorktree && worktreeName}
        <span class="worktree-name" title="Linked to {worktreeName}">{worktreeName}</span>
      {:else if hasWorktree}
        <span class="worktree-badge" title="Linked to worktree">
          <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor">
            <path d="M5 3.25a.75.75 0 11-1.5 0 .75.75 0 011.5 0zm0 2.122a2.25 2.25 0 10-1.5 0v.878A2.25 2.25 0 005.75 8.5h1.5v2.128a2.251 2.251 0 101.5 0V8.5h1.5a2.25 2.25 0 002.25-2.25v-.878a2.25 2.25 0 10-1.5 0v.878a.75.75 0 01-.75.75h-5.5a.75.75 0 01-.75-.75v-.878zM8 12.25a.75.75 0 11-1.5 0 .75.75 0 011.5 0zm3.25-9.75a.75.75 0 100 1.5.75.75 0 000-1.5z"/>
          </svg>
        </span>
      {/if}
      {#if pr.stack}
        <span
          class="stack-indicator"
          aria-label={`Stacked: ${pr.stack.position}/${pr.stack.size}`}
          title={`Stacked: ${pr.stack.position}/${pr.stack.size}`}
        >
          <Layers2Icon size={13} strokeWidth={2.2} aria-hidden="true" />
          <span class="stack-indicator-count" aria-hidden="true">{pr.stack.position}/{pr.stack.size}</span>
        </span>
      {/if}
      {#if parsed.error !== null}
        <span
          class="ci ci-unavailable"
          data-testid="ci-token-unavailable"
          title={`CI unavailable: ${safeDiagnosticText(parsed.error)}`}
          aria-hidden="true"
        >
          <CircleAlertIcon size={10} strokeWidth={2.5} />
        </span>
        <span class="kit-sr-only">CI unavailable: {safeDiagnosticText(parsed.error)}</span>
      {:else if bucketed.all.length > 0}
        <span class="ci" aria-hidden="true">
          <CITokenCluster {bucketed} size="compact" pendingStyle="static" />
        </span>
        <span class="kit-sr-only">{composeAriaLabel(bucketed)}</span>
      {/if}
      {#if pr.MergeableState === "dirty"}
        <span class="conflict-icon" title="Has merge conflicts">
          <!-- git-merge-conflict icon, ISC License, Copyright (c) Lucide Icons and Contributors -->
          <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M12 6h4a2 2 0 0 1 2 2v7" />
            <path d="M6 12v9" />
            <path d="M9 3 3 9" />
            <path d="M9 9 3 3" />
            <circle cx="18" cy="18" r="3" />
          </svg>
        </span>
      {/if}
      <span
        class="star-btn"
        role="button"
        tabindex="-1"
        onclick={handleStarClick}
        onkeydown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); handleStarClick(e as unknown as MouseEvent); } }}
        title={pr.Starred ? "Unstar" : "Star"}
      >
        {#if pr.Starred}
          <svg class="star-icon star-icon--active" width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
            <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25z"/>
          </svg>
        {:else}
          <svg class="star-icon" width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
            <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25zm0 2.445L6.615 5.5a.75.75 0 01-.564.41l-3.097.45 2.24 2.184a.75.75 0 01.216.664l-.528 3.084 2.769-1.456a.75.75 0 01.698 0l2.77 1.456-.53-3.084a.75.75 0 01.216-.664l2.24-2.183-3.096-.45a.75.75 0 01-.564-.41L8 2.694z"/>
          </svg>
        {/if}
      </span>
      <span
        class="time"
        title={activityTime.fromWorkspace ? "Recent workspace activity" : undefined}
        aria-label={activityTime.fromWorkspace ? `Recent workspace activity, ${ago}` : undefined}
      >{ago}</span>
    </span>
  </div>
</button>
<SidebarTitlePopover target={el} title={pr.Title} repository={repoLabel} branch={pr.HeadBranch} truncationSelector=".title-text" />

<style>
  .pull-item {
    display: block;
    width: 100%;
    text-align: left;
    padding: 6px 10px;
    border-bottom: 1px solid var(--sidebar-list-border-muted, var(--border-muted));
    background: var(--sidebar-row-bg, var(--bg-surface));
    cursor: pointer;
    transition: background 0.1s;
    border-left: 3px solid transparent;
  }

  .pull-item:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .pull-item.selected {
    background: var(--bg-row-selected, var(--bg-inset));
    border-left-color: var(--accent-blue);
  }

  .pull-item.selected:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .pull-item:focus-visible {
    outline: none;
    box-shadow: inset 0 0 0 1px var(--accent-blue);
  }

  .pull-item.active-worktree {
    border-left-color: var(--accent-teal, var(--accent-green));
  }

  .pull-item.selected.active-worktree {
    border-left-color: var(--accent-blue);
  }

  .title {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: var(--font-size-md);
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden;
    margin-bottom: 2px;
  }

  .title-text {
    flex: 0 1 auto;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .pull-item.selected .title-text {
    font-weight: 600;
  }

  .state-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .item-number {
    margin-left: auto;
    flex-shrink: 0;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }

  .meta-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .meta-left {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: 1 1 auto;
    min-width: 0;
  }

  .meta-text {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex: 0 4 auto;
    min-width: 0;
  }

  .repo-name {
    font-size: var(--font-size-xs);
    font-weight: 500;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex: 0 1 auto;
    min-width: 0;
  }

  .meta-sep {
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .meta-right {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-shrink: 0;
  }

  .time {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }

  .worktree-badge {
    display: flex;
    align-items: center;
    color: var(--accent-teal, var(--accent-green));
    flex-shrink: 0;
  }

  .worktree-name {
    font-size: var(--font-size-2xs);
    font-weight: 500;
    color: var(--accent-teal, var(--accent-green));
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 80px;
    flex-shrink: 1;
    min-width: 0;
  }

  .review-indicator {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex: 0 0 auto;
  }

  .review-indicator--approved {
    color: var(--accent-green);
  }

  .review-indicator--changes-requested {
    color: var(--accent-red);
  }

  .stack-indicator {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    flex: 0 0 auto;
    color: var(--text-muted);
  }

  .stack-indicator-count {
    font-size: var(--font-size-2xs);
    font-weight: 500;
    font-variant-numeric: tabular-nums;
    line-height: 1;
  }

  .ci-unavailable {
    color: var(--state-warn, var(--accent-amber, #c08a2a));
    opacity: 0.85;
  }

  .pull-item .ci {
    display: inline-flex;
    align-items: center;
    flex-shrink: 0;
    gap: var(--space-2);
  }

  :global(.mobile-main) .pull-item .ci {
    gap: var(--space-1);
  }

  .import-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    opacity: 0;
    transition: opacity 0.15s;
    cursor: pointer;
    color: var(--text-muted);
  }

  .pull-item:hover .import-btn {
    opacity: 0.6;
  }

  .import-btn:hover {
    opacity: 1 !important;
    color: var(--accent-blue);
  }

  .conflict-icon {
    display: flex;
    align-items: center;
    color: var(--accent-amber);
    flex-shrink: 0;
  }

  .star-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    opacity: 0;
    transition: opacity 0.15s;
    cursor: pointer;
  }

  .pull-item:hover .star-btn {
    opacity: 0.6;
  }

  .star-btn:hover {
    opacity: 1 !important;
  }

  .star-btn:has(.star-icon--active) {
    opacity: 1;
  }

  .star-icon {
    color: var(--text-muted);
    transition: color 0.1s;
  }

  .star-btn:hover .star-icon {
    color: var(--accent-amber);
  }

  .star-icon--active {
    color: var(--accent-amber);
  }

  :global(.mobile-main) .pull-item {
    min-height: calc(var(--focus-mobile-hit-target, 37px) * 1.95);
    font-size: var(--font-size-md);
    padding: var(--focus-mobile-space-sm, 10px) var(--focus-mobile-space-md, 13px);
    border-bottom: thin solid var(--border-muted);
    border-left-width: 3px;
  }

  :global(.mobile-main) .title {
    gap: var(--focus-mobile-space-xs, 6px);
    margin-bottom: var(--focus-mobile-space-xs, 6.5px);
    font-size: var(--font-size-xl);
    line-height: 1.3;
  }

  :global(.mobile-main) .title-text {
    white-space: normal;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
  }

  :global(.mobile-main) .state-dot {
    width: 10px;
    height: 10px;
  }

  :global(.mobile-main) .meta-row {
    gap: var(--focus-mobile-space-sm, 8px);
  }

  :global(.mobile-main) .meta-text,
  :global(.mobile-main) .time,
  :global(.mobile-main) .worktree-name,
  :global(.mobile-main) .stack-indicator-count,
  :global(.mobile-main) .item-number,
  :global(.mobile-main) .repo-name {
    font-size: var(--font-size-sm);
    line-height: 1.35;
  }

  :global(.mobile-main) .meta-right {
    gap: var(--focus-mobile-space-xs, 6px);
  }

  :global(.mobile-main) :global(.kit-chip),
  :global(.mobile-main) :global(.state-chip) {
    min-height: calc(var(--focus-mobile-hit-target, 37px) * 0.65);
    padding: 2.5px var(--focus-mobile-space-xs, 6.5px);
    border-radius: 999px;
    font-size: var(--font-size-xs);
    line-height: 1.25;
  }
</style>
