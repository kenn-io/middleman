<script lang="ts">
  import type { Issue } from "../../api/types.js";
  import { getStores } from "../../context.js";
  import { formatRelativeTime } from "@kenn-io/kit-ui";
  import { hashColor } from "@kenn-io/kit-ui";
  import { Chip } from "@kenn-io/kit-ui";
  import LabelRow from "../shared/LabelRow.svelte";
  import WorkspaceIndicator from "../shared/WorkspaceIndicator.svelte";
  import AgentStatusIndicator from "../shared/AgentStatusIndicator.svelte";
  import SidebarTitlePopover from "./SidebarTitlePopover.svelte";
  import { repoIdentityKey } from "../../utils/repo-label.js";
  import { effectiveActivity } from "../../utils/effective-activity.js";

  const { issues, activity } = getStores();

  interface Props {
    issue: Issue;
    selected: boolean;
    showRepo: boolean;
    repoLabel: string;
    onclick: () => void;
  }

  const { issue, selected, showRepo, repoLabel, onclick }: Props = $props();

  let el = $state<HTMLButtonElement>();

  $effect(() => {
    if (selected && el) {
      el.scrollIntoView({ block: "nearest", behavior: "smooth" });
    }
  });

  function handleStarClick(e: MouseEvent): void {
    e.stopPropagation();
    void issues.toggleIssueStar(
      {
        provider: issue.repo.provider,
        platformHost: issue.repo.platform_host,
        owner: issue.repo.owner,
        name: issue.repo.name,
        repoPath: issue.repo.repo_path,
      },
      issue.Number,
      issue.Starred,
    );
  }

  const labels = $derived(issue.labels ?? []);
  const repoColorKey = $derived(repoIdentityKey({
    provider: issue.repo.provider,
    platformHost: issue.repo.platform_host,
    owner: issue.repo.owner,
    name: issue.repo.name,
    repoPath: issue.repo.repo_path,
  }));
  const activityTime = $derived(effectiveActivity(
    issue.LastActivityAt,
    issue.last_workspace_activity_at,
    activity.getUseWorkspaceActivityForRecency(),
  ));
  const ago = $derived(formatRelativeTime(activityTime.at));
</script>

<button class="issue-item" class:selected bind:this={el} onclick={onclick}>
  <p class="title">
    <span class="title-text">{issue.Title}</span>
    <LabelRow {labels} compact />
    <span class="title-status">
      <AgentStatusIndicator state={issue.workspace?.agent_state} />
      <span class="item-number">#{issue.Number}</span>
    </span>
  </p>
  <div class="meta-row">
    <span class="meta-left">
      {#if showRepo}
        <span class="repo-name" style={`color: ${hashColor(repoColorKey)}`} title={repoLabel}>{repoLabel}</span>
        <span class="meta-sep">·</span>
      {/if}
      <span class="meta-text">{issue.Author}</span>
    </span>
    <span class="meta-right">
      {#if issue.workspace}
        <WorkspaceIndicator status={issue.workspace.status} />
      {/if}
      <span
        class="star-btn"
        role="button"
        tabindex="-1"
        onclick={handleStarClick}
        onkeydown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); handleStarClick(e as unknown as MouseEvent); } }}
        title={issue.Starred ? "Unstar" : "Star"}
      >
        {#if issue.Starred}
          <svg class="star-icon star-icon--active" width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
            <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25z"/>
          </svg>
        {:else}
          <svg class="star-icon" width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
            <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25zm0 2.445L6.615 5.5a.75.75 0 01-.564.41l-3.097.45 2.24 2.184a.75.75 0 01.216.664l-.528 3.084 2.769-1.456a.75.75 0 01.698 0l2.77 1.456-.53-3.084a.75.75 0 01.216-.664l2.24-2.183-3.096-.45a.75.75 0 01-.564-.41L8 2.694z"/>
          </svg>
        {/if}
      </span>
      {#if issue.State !== "open"}
        <Chip size="xs" tone="merged" class="state-chip">Closed</Chip>
      {/if}
      <span
        class="time"
        title={activityTime.fromWorkspace ? "Recent workspace activity" : undefined}
        aria-label={activityTime.fromWorkspace ? `Recent workspace activity, ${ago}` : undefined}
      >{ago}</span>
    </span>
  </div>
</button>
<SidebarTitlePopover target={el} title={issue.Title} repository={repoLabel} truncationSelector=".title-text" />

<style>
  .issue-item {
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

  .issue-item:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .issue-item.selected {
    background: var(--bg-row-selected, var(--bg-inset));
    border-left-color: var(--accent-blue);
  }

  .issue-item.selected:hover {
    background: var(--sidebar-row-hover-bg, var(--bg-surface-hover));
  }

  .issue-item:focus-visible {
    outline: none;
    box-shadow: inset 0 0 0 1px var(--accent-blue);
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

  .title-status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    margin-left: auto;
    flex-shrink: 0;
  }

  .title-text {
    flex: 0 1 auto;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .issue-item.selected .title-text {
    font-weight: 600;
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

  :global(.state-chip) {
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

  .issue-item:hover .star-btn {
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

  :global(.mobile-main) .issue-item {
    min-height: calc(var(--focus-mobile-hit-target, 37px) * 1.95);
    font-size: var(--font-size-md);
    padding: var(--focus-mobile-space-sm, 10px) var(--focus-mobile-space-md, 13px);
    border-bottom: thin solid var(--border-muted);
    border-left-width: 3px;
  }

  :global(.mobile-main) .title {
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

  :global(.mobile-main) .meta-row {
    gap: var(--focus-mobile-space-sm, 8px);
  }

  :global(.mobile-main) .meta-text,
  :global(.mobile-main) .time,
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
