<script lang="ts">
  import { Effect } from "effect";
  import { onDestroy, untrack } from "svelte";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import Layers2Icon from "@lucide/svelte/icons/layers-2";
  import XIcon from "@lucide/svelte/icons/x";
  import type { ApiProblemError, TransientTransportError } from "../../api/effect-errors.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import type { StackContextResponse, StackMemberResponse } from "../../api/generated/models/index.js";
  import { providerItemPath, providerRouteParams, providerHostRouteParams, providerUsesHostRoute } from "../../api/provider-routes.js";
  import { retryIdempotentRead } from "../../api/retry-policy.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { getNavigate, getStores } from "../../context.js";
  import {
    buildPullRequestRoute,
    type PullRequestRouteRef,
  } from "../../routes.js";
  import { Chip } from "@kenn-io/kit-ui";
  import type { StoreInstances } from "../../types.js";

  const runtime = getAppRuntime();
  const navigate = getNavigate();
  const stores = getStores() as Partial<StoreInstances>;
  const syncStore = stores.sync;

  type Props = PullRequestRouteRef & {
    initialStack?: StackContext | null;
    expanded?: boolean;
    showButton?: boolean;
    showPanel?: boolean;
    ontoggle?: ((expanded: boolean) => void) | undefined;
    onmembernavigate?: ((ref: PullRequestRouteRef) => boolean | void) | undefined;
  };

  let {
    owner,
    name,
    number,
    provider,
    platformHost,
    repoPath,
    initialStack,
    expanded = $bindable(false),
    showButton = true,
    showPanel = true,
    ontoggle,
    onmembernavigate,
  }: Props = $props();

  type StackMember = StackMemberResponse;
  type StackContext = StackContextResponse;

  let data = $state<StackContext | null>(null);
  let visible = $state(false);
  let dataRefKey = $state("");
  let stackExecution: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;

  const currentRefKey = $derived(JSON.stringify([
    provider,
    platformHost ?? "",
    owner,
    name,
    repoPath,
  ]));
  const members = $derived(data?.members ?? []);
  const displayMembers = $derived(
    [...members].sort((a, b) => b.position - a.position),
  );
  const stackBaseBranch = $derived(
    members.find((member) => member.position === 1)?.base_branch || "base",
  );
  const downstackFailures = $derived(
    members.filter((member) =>
      member.position < (data?.position ?? 0) &&
      member.ci_status === "failure" &&
      member.state !== "merged"
    ),
  );
  const downstackConflicts = $derived(
    members.filter((member) =>
      member.position < (data?.position ?? 0) &&
      member.mergeable_state === "dirty" &&
      member.state !== "merged"
    ),
  );
  const downstackBlockerCount = $derived.by(() => {
    return members.filter((member) =>
      member.position < (data?.position ?? 0) &&
      member.state !== "merged" &&
      (member.ci_status === "failure" || member.mergeable_state === "dirty")
    ).length;
  });
  const summary = $derived.by(() => {
    if (!data) return "";
    const failureText = downstackFailures.length > 0
      ? ` · downstack CI ${downstackFailures.length === 1 ? "failure" : "failures"}`
      : "";
    const conflictText = downstackConflicts.length > 0
      ? ` · downstack ${downstackConflicts.length === 1 ? "conflict" : "conflicts"}`
      : "";
    return `${data.size} PRs · current ${data.position}/${data.size}${failureText}${conflictText}`;
  });
  const chipAriaLabel = $derived.by(() => {
    if (!data) return "Stacked";
    const parts = [`Stacked: ${data.position}/${data.size}`];
    if (downstackFailures.length > 0) {
      parts.push(`${downstackFailures.length} downstack CI failure${downstackFailures.length === 1 ? "" : "s"}`);
    }
    if (downstackConflicts.length > 0) {
      parts.push(`${downstackConflicts.length} downstack merge conflict${downstackConflicts.length === 1 ? "" : "s"}`);
    }
    return parts.join(", ");
  });

  function stackWithPosition(stack: StackContext, num: number): StackContext | null {
    const member = stack.members?.find((candidate) => candidate.number === num);
    if (!member) return null;
    if (stack.position === member.position) return stack;
    return { ...stack, position: member.position };
  }

  function fetchStack(o: string, n: string, num: number, refKey = currentRefKey): void {
    const ref = { provider, platformHost, owner: o, name: n, repoPath };
    stackExecution?.interrupt();
    const program = executeGeneratedApiRequest("GET pull request stack", (client, signal) =>
      providerUsesHostRoute(ref) ? client.PullRequestsService.getPullStackOnHost({ ...providerHostRouteParams(ref), number: num }, { signal }) : client.PullRequestsService.getPullStack({ ...providerRouteParams(ref), number: num }, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.flatMap((stack) =>
        Effect.sync(() => {
          if (!stack.members || stack.members.length <= 1) {
            visible = false;
            data = null;
            dataRefKey = refKey;
            return;
          }
          data = stack;
          dataRefKey = refKey;
          visible = true;
        }),
      ),
    );
    stackExecution = runtime.runCommand(program, {
      operation: "load pull request stack",
      safeContext: { provider: ref.provider, owner: ref.owner, name: ref.name, number: num },
      onFailure: () => {
        visible = false;
        data = null;
        dataRefKey = "";
      },
    });
  }

  $effect(() => {
    const num = number;
    const refKey = currentRefKey;
    const seededStack = initialStack ? stackWithPosition(initialStack, num) : null;
    if (seededStack) {
      data = seededStack;
      dataRefKey = refKey;
      visible = true;
    } else if (initialStack !== undefined) {
      visible = false;
      data = null;
      dataRefKey = refKey;
    } else {
      const cachedStack = untrack(() =>
        dataRefKey === refKey && data ? stackWithPosition(data, num) : null
      );
      if (cachedStack) {
        data = cachedStack;
        visible = true;
      } else {
        visible = false;
        data = null;
        dataRefKey = refKey;
      }
    }
    stackExecution?.interrupt();
  });

  const unsubSync = syncStore?.subscribeSyncComplete?.(() =>
    initialStack || data ? fetchStack(owner, name, number) : undefined
  );
  onDestroy(() => {
    unsubSync?.();
    stackExecution?.interrupt();
  });

  function toggleExpanded(): void {
    const next = !expanded;
    if (ontoggle) {
      ontoggle(next);
      return;
    }
    expanded = next;
  }

  function statusLabel(member: StackMember): { text: string; className: string } | null {
    if (!member.ci_status || member.state === "merged") return null;
    if (member.ci_status === "success") {
      return { text: "✓ CI", className: "stack-status-label--green" };
    }
    if (member.ci_status === "failure") {
      return { text: "× CI", className: "stack-status-label--red" };
    }
    if (member.ci_status === "pending") {
      return { text: "○ CI", className: "stack-status-label--amber" };
    }
    return null;
  }

  function reviewLabel(member: StackMember): { text: string; className: string } | null {
    if (!member.review_decision || member.state === "merged") return null;
    if (member.review_decision === "APPROVED") {
      return { text: "✓ Approved", className: "stack-status-label--green" };
    }
    if (member.review_decision === "CHANGES_REQUESTED") {
      return { text: "× Changes", className: "stack-status-label--red" };
    }
    return { text: "○ Review", className: "stack-status-label--muted" };
  }

  function mergeableLabel(member: StackMember): { text: string; className: string } | null {
    if (member.state === "merged" || member.mergeable_state !== "dirty") return null;
    return { text: "× Conflicts", className: "stack-status-label--red" };
  }

  function dotClass(member: StackMember, isCurrent: boolean): string {
    if (isCurrent) return "stack-dot stack-dot--current";
    if (member.state === "merged") return "stack-dot stack-dot--merged";
    if (member.mergeable_state === "dirty" || member.ci_status === "failure") {
      return "stack-dot stack-dot--red";
    }
    if (member.ci_status === "pending" || member.review_decision === "CHANGES_REQUESTED") {
      return "stack-dot stack-dot--amber";
    }
    if (
      member.state === "open" &&
      !member.is_draft &&
      member.ci_status === "success" &&
      member.review_decision === "APPROVED"
    ) {
      return "stack-dot stack-dot--green";
    }
    return "stack-dot stack-dot--outline";
  }

  function navigateToMember(memberNumber: number): void {
    const ref = {
      provider,
      platformHost,
      owner,
      name,
      repoPath,
      number: memberNumber,
    };
    if (onmembernavigate?.(ref) === true) return;
    navigate(buildPullRequestRoute(ref));
  }
</script>

{#if visible && data}
  <div class="stack-status">
    {#if showButton}
      <Chip size="sm"
        interactive={true}
        tone="neutral"
        uppercase={false}
        ariaLabel={chipAriaLabel}
        dataTestid="stack-chip"
        onclick={toggleExpanded}
        title={expanded ? "Collapse stack" : "Expand stack"}
        {expanded}
      >
        <Layers2Icon size={12} strokeWidth={2.3} aria-hidden="true" />
        <span class="stack-chip-label">{data.position}/{data.size}</span>
        {#if downstackBlockerCount > 0}
          <span class="stack-chip-failure" aria-hidden="true">
            <XIcon size={14} strokeWidth={2.8} /><span class="stack-chip-failure-count">{downstackBlockerCount}</span>
          </span>
        {/if}
        {#snippet trailing()}
          <ChevronDownIcon
            class={["chip-chevron", expanded && "chip-chevron--open"].filter(Boolean).join(" ")}
            size={12}
            strokeWidth={2.4}
            aria-hidden="true"
          />
        {/snippet}
      </Chip>
    {/if}

    {#if showPanel && expanded}
      <div class="stack-collapse">
        <div class="stack-panel">
          <div class="stack-summary">{summary}</div>
          <div class="stack-rows">
            {#each displayMembers as member, i (member.number)}
              {@const isCurrent = member.number === number}
              {@const ci = statusLabel(member)}
              {@const review = reviewLabel(member)}
              {@const mergeable = mergeableLabel(member)}
              <div
                class="stack-row"
                class:stack-row--current={isCurrent}
                class:stack-row--blocked={member.blocked_by != null && !isCurrent}
              >
                <span
                  class={["stack-rail", i === 0 && "stack-rail--first"]
                    .filter(Boolean).join(" ")}
                  aria-hidden="true"
                >
                  <span class="stack-line"></span>
                  <span class={dotClass(member, isCurrent)}></span>
                </span>
                <span class="stack-row-body">
                  <button
                    class="stack-member-link"
                    onclick={() => navigateToMember(member.number)}
                  >
                    #{member.number} {member.title}
                  </button>
                </span>
                <span class="stack-badges">
                  {#if mergeable}<span class={`stack-status-label ${mergeable.className}`}>{mergeable.text}</span>{/if}
                  {#if ci}<span class={`stack-status-label ${ci.className}`}>{ci.text}</span>{/if}
                  {#if review}<span class={`stack-status-label ${review.className}`}>{review.text}</span>{/if}
                  {#if member.blocked_by != null && isCurrent}
                    <span class="stack-blocked-label">blocked by #{member.blocked_by}</span>
                  {/if}
                </span>
              </div>
            {/each}
            <div class="stack-row stack-row--base" aria-label={`Stack base ${stackBaseBranch}`}>
              <span class="stack-rail stack-rail--last" aria-hidden="true">
                <span class="stack-line"></span>
                <span class="stack-dot stack-dot--outline"></span>
              </span>
              <span class="stack-base-name">{stackBaseBranch}</span>
            </div>
          </div>
        </div>
      </div>
    {/if}
  </div>
{/if}

<style>
  .stack-status {
    display: contents;
  }

  .stack-chip-label {
    line-height: 1;
  }

  .stack-chip-failure {
    display: inline-flex;
    align-items: center;
    vertical-align: middle;
    gap: var(--space-1);
    color: var(--accent-red);
    font-variant-numeric: tabular-nums;
    font-weight: 700;
    line-height: 1;
  }

  .stack-chip-failure :global(svg) {
    display: block;
  }

  .stack-chip-failure-count {
    font-size: 0.9em;
    line-height: 1;
  }

  :global(.chip-chevron) {
    flex-shrink: 0;
    vertical-align: middle;
    transition: transform 0.15s;
  }

  :global(.chip-chevron--open) {
    transform: rotate(180deg);
  }

  .stack-collapse {
    order: 999;
    flex-basis: 100%;
    width: 100%;
    min-width: 0;
    margin-top: 4px;
  }

  .stack-panel {
    --stack-rail-color: color-mix(in srgb, var(--text-muted) 58%, var(--border-default));

    display: flex;
    flex-direction: column;
    width: 100%;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    overflow: auto;
    flex-shrink: 0;
    max-height: min(430px, 60vh);
  }

  .stack-summary {
    font-size: var(--font-size-2xs);
    font-weight: 600;
    color: var(--text-muted);
    padding: 8px 12px 6px;
    border-bottom: 1px solid var(--border-muted);
    font-variant-numeric: tabular-nums;
  }

  .stack-rows {
    display: flex;
    flex-direction: column;
  }

  .stack-row {
    display: grid;
    grid-template-columns: 18px minmax(0, 1fr) auto;
    align-items: stretch;
    gap: 8px;
    min-height: 38px;
    padding: 5px 12px;
    font-size: var(--font-size-sm);
    color: var(--text-primary);
  }

  .stack-row--current {
    background: color-mix(in srgb, var(--accent-purple) 13%, transparent);
    border-left: 2px solid var(--accent-purple);
    padding-left: 10px;
  }

  .stack-row--blocked:not(.stack-row--current) {
    opacity: 0.68;
  }

  .stack-row--base {
    min-height: 30px;
    color: var(--text-muted);
    cursor: default;
    user-select: text;
  }

  .stack-rail {
    position: relative;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .stack-dot {
    position: relative;
    z-index: 1;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    border: 1px solid transparent;
    flex-shrink: 0;
  }

  .stack-dot--current {
    width: 10px;
    height: 10px;
    background: var(--accent-purple);
  }

  .stack-dot--merged {
    background: var(--text-muted);
  }

  .stack-dot--red {
    background: var(--accent-red);
  }

  .stack-dot--amber {
    background: var(--accent-amber);
  }

  .stack-dot--green {
    background: var(--accent-green);
  }

  .stack-dot--outline {
    background: var(--bg-inset);
    border-color: var(--stack-rail-color);
  }

  .stack-line {
    position: absolute;
    top: -5px;
    bottom: -5px;
    width: 2px;
    background: var(--stack-rail-color);
  }

  .stack-rail--first .stack-line {
    top: 50%;
  }

  .stack-rail--last .stack-line {
    bottom: 50%;
  }

  .stack-row-body {
    align-self: center;
    min-width: 0;
    display: grid;
    gap: 2px;
  }

  .stack-member-link {
    min-width: 0;
    color: var(--accent-blue);
    cursor: pointer;
    background: none;
    border: none;
    padding: 0;
    font-size: var(--font-size-sm);
    line-height: 1.25;
    text-align: left;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stack-member-link:hover {
    text-decoration: underline;
  }

  .stack-base-name {
    align-self: center;
    min-width: 0;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-family: var(--font-mono);
    line-height: 1.2;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stack-badges {
    align-self: center;
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: 6px;
    min-width: 0;
    font-size: var(--font-size-2xs);
    white-space: nowrap;
  }

  .stack-status-label {
    font-weight: 600;
    line-height: 1;
  }

  .stack-status-label--green {
    color: var(--accent-green);
  }

  .stack-status-label--red,
  .stack-blocked-label {
    color: var(--accent-red);
  }

  .stack-status-label--amber {
    color: var(--accent-amber);
  }

  .stack-status-label--muted {
    color: var(--text-muted);
  }

  .stack-blocked-label {
    font-style: italic;
    line-height: 1;
  }

  @container pull-detail (max-width: 640px) {
    .stack-row {
      grid-template-columns: 18px minmax(0, 1fr) max-content;
    }
  }

  @container pull-detail (max-width: 440px) {
    .stack-row {
      grid-template-columns: 18px minmax(0, 1fr);
    }

    .stack-rail {
      grid-row: 1 / 3;
    }

    .stack-row--base .stack-rail {
      grid-row: 1;
    }

    .stack-badges {
      grid-column: 2;
      grid-row: 2;
      justify-content: flex-start;
      flex-wrap: wrap;
      white-space: normal;
    }
  }
</style>
