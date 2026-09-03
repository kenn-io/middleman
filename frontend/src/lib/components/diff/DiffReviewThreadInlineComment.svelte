<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import EyeOffIcon from "@lucide/svelte/icons/eye-off";
  import MessageSquareReplyIcon from "@lucide/svelte/icons/message-square-reply";
  import SendIcon from "@lucide/svelte/icons/send";
  import XIcon from "@lucide/svelte/icons/x";
  import { Effect } from "effect";
  import { tick } from "svelte";
  import type { Attachment } from "svelte/attachments";
  import type { AppExecution, AppRuntime } from "../../app/runtime.js";
  import { makeAnimationFrameScheduler } from "../../browser/animation-frame.js";
  import { observeResize } from "../../browser/observers.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import type { ReviewThread, ReviewThreadCardPlacement } from "./review-thread-context.js";
  import { reviewThreadLineLabel, reviewThreadProviderHiddenState } from "./review-thread-context.js";

  interface Props {
    runtime: AppRuntime;
    thread: ReviewThread;
    placement?: ReviewThreadCardPlacement;
    canReply?: boolean;
    onreply?: ((thread: ReviewThread, body: string, callbacks: MutationCallbacks) => void) | undefined;
  }

  const {
    runtime,
    thread,
    placement = "inline",
    canReply = false,
    onreply,
  }: Props = $props();

  let replying = $state(false);
  let replyBody = $state("");
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let textareaEl: HTMLTextAreaElement | undefined = $state();
  let panelWidth = $state<string | undefined>();
  let focusExecution: AppExecution<void, never> | null = null;
  let hiddenExpanded = $state(false);
  const hiddenState = $derived(reviewThreadProviderHiddenState(thread));
  const bodyVisible = $derived(hiddenState === null || hiddenExpanded);

  function providerHiddenLabel(reason: string | null): string {
    if (!reason) return "Hidden on GitHub";
    const label = reason === "OFF_TOPIC" ? "off-topic" : reason.toLowerCase().replaceAll("_", " ");
    return `Hidden on GitHub: ${label.charAt(0).toUpperCase()}${label.slice(1)}`;
  }

  const setupThreadLayout: Attachment<HTMLDivElement> = (node) => {
    const execution = runtime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          const layoutScheduler = yield* makeAnimationFrameScheduler(
            Effect.sync(() => updatePanelWidth(node)),
          );
          const scheduleLayout = (): void => {
            layoutScheduler.schedule();
          };
          const container = layoutContainer(node);
          yield* Effect.acquireRelease(
            Effect.sync(() => {
              container?.addEventListener("scroll", scheduleLayout, { passive: true });
              window.addEventListener("resize", scheduleLayout);
              scheduleLayout();
            }),
            () => Effect.sync(() => {
              container?.removeEventListener("scroll", scheduleLayout);
              window.removeEventListener("resize", scheduleLayout);
            }),
          );
          if (typeof ResizeObserver !== "undefined") {
            if (container) yield* observeResize(container, scheduleLayout);
            yield* observeResize(node, scheduleLayout);
          }
          return yield* Effect.never;
        }),
      ),
      {
        operation: "own diff review thread layout",
        safeContext: { threadId: thread.id },
        onFailure: () => {},
      },
    );
    return execution.interrupt;
  };

  function updatePanelWidth(element: HTMLElement): void {
    const container = layoutContainer(element);
    if (!container) {
      panelWidth = undefined;
      return;
    }
    const containerRect = container.getBoundingClientRect();
    const threadRect = element.getBoundingClientRect();
    const available = Math.floor(containerRect.right - threadRect.left - 12);
    panelWidth = available > 0 ? `${available}px` : undefined;
  }

  function layoutContainer(element: HTMLElement): HTMLElement | null {
    const root = element.getRootNode();
    if (root instanceof ShadowRoot && root.host instanceof HTMLElement) {
      return closestLayoutContainer(root.host);
    }
    return closestLayoutContainer(element);
  }

  function closestLayoutContainer(element: HTMLElement): HTMLElement | null {
    return (
      element.closest(".markdown-rich-diff__pane") ??
      element.closest(".diff-rich-preview") ??
      element.closest(".preview-shell") ??
      element.closest(".file-content") ??
      element.closest(".diff-area")
    );
  }

  function startReply(): void {
    focusExecution?.interrupt();
    replying = true;
    error = null;
    focusExecution = runtime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(Effect.sync(() => textareaEl?.focus({ preventScroll: true }))),
      ),
      {
        operation: "focus inline review reply",
        safeContext: { threadId: thread.id },
        onFailure: () => {},
      },
    );
  }

  function cancelReply(): void {
    focusExecution?.interrupt();
    focusExecution = null;
    replying = false;
    replyBody = "";
    error = null;
  }

  function submitReply(): void {
    const body = replyBody.trim();
    if (!body) {
      error = "Reply body must not be empty";
      return;
    }
    if (!onreply) return;
    submitting = true;
    error = null;
    onreply(thread, body, {
      onSuccess: cancelReply,
      onSettled: () => {
        submitting = false;
      },
    });
  }
</script>

<div
  class="inline-review-thread"
  class:inline-review-thread--file-level={placement !== "inline"}
  class:inline-review-thread--idle-reply={bodyVisible && canReply && !thread.resolved && !replying}
  data-review-thread-id={thread.id}
  {@attach setupThreadLayout}
  style:--inline-review-thread-width={panelWidth}
  tabindex="-1"
>
  <div class="review-thread-header">
    <span class="review-thread-state">Review Comment</span>
    <span class="review-thread-location">{reviewThreadLineLabel(thread)}</span>
    {#if thread.resolved}
      <span class="review-thread-status">Resolved</span>
    {/if}
    {#if placement === "file"}
      <span class="review-thread-status review-thread-status--outdated">File</span>
    {:else if placement === "outdated"}
      <span class="review-thread-status review-thread-status--outdated">Outdated</span>
    {:else if placement === "unavailable"}
      <span class="review-thread-status review-thread-status--outdated">Line unavailable</span>
    {/if}
  </div>
  {#if thread.author_login}
    <div class="review-thread-author">{thread.author_login}</div>
  {/if}
  {#if hiddenState}
    <div class="provider-hidden-notice">
      <EyeOffIcon size={14} aria-hidden="true" />
      <span>{providerHiddenLabel(hiddenState.reason)}</span>
      <button
        class="provider-hidden-toggle"
        type="button"
        onclick={() => (hiddenExpanded = !hiddenExpanded)}
        aria-expanded={hiddenExpanded}
      >
        {hiddenExpanded ? "Collapse" : "Show comment"}
      </button>
    </div>
  {/if}
  {#if bodyVisible}
    <p
      class="review-thread-body"
      class:review-thread-body--with-idle-reply={canReply && !thread.resolved && !replying}
    >
      {thread.body}
    </p>
  {/if}
  {#if bodyVisible && canReply && !thread.resolved}
    {#if replying}
      <div class="review-thread-reply">
        <textarea
          bind:this={textareaEl}
          bind:value={replyBody}
          placeholder="Reply to thread"
          disabled={submitting}
          rows="3"
        ></textarea>
        {#if error}
          <p class="review-thread-error">{error}</p>
        {/if}
        <div class="review-thread-actions">
          <Button
            class="review-thread-btn"
            size="sm"
            onclick={cancelReply}
            disabled={submitting}
          >
            <XIcon size={14} />
            Cancel
          </Button>
          <Button
            class="review-thread-btn review-thread-btn--primary"
            tone="info"
            surface="solid"
            size="sm"
            onclick={() => void submitReply()}
            disabled={submitting || replyBody.trim() === ""}
          >
            <SendIcon size={14} />
            {submitting ? "Replying..." : "Reply"}
          </Button>
        </div>
      </div>
    {:else}
      <div class="review-thread-actions review-thread-actions--idle">
        <Button
          class="review-thread-btn"
          size="sm"
          surface="soft"
          tone="neutral"
          onclick={startReply}
        >
          <MessageSquareReplyIcon size={14} />
          Reply
        </Button>
      </div>
    {/if}
  {/if}
</div>

<style>
  .inline-review-thread {
    position: sticky;
    left: 12px;
    box-sizing: border-box;
    margin: 6px 0 8px;
    padding: 8px;
    border: 1px solid color-mix(in srgb, var(--accent-purple) 44%, var(--border-muted));
    border-radius: 6px;
    background: color-mix(in srgb, var(--accent-purple) 9%, var(--bg-surface));
    width: var(--inline-review-thread-width, calc(100% - 24px));
    max-width: var(--inline-review-thread-width, calc(100% - 24px));
    min-width: 0;
    scroll-margin-block: 96px;
  }

  .inline-review-thread--idle-reply {
    min-height: 78px;
  }

  .inline-review-thread--file-level {
    margin-top: 8px;
  }

  .inline-review-thread:focus {
    outline: 2px solid var(--accent-purple);
    outline-offset: 2px;
  }

  @container (max-width: 520px) {
    .inline-review-thread {
      left: 8px;
      margin: 6px 0 8px;
    }
  }

  .review-thread-header {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .review-thread-state {
    flex-shrink: 0;
    padding: 1px 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent-purple) 16%, var(--bg-inset));
    color: var(--accent-purple);
    font-size: var(--font-size-2xs);
    font-weight: 700;
    text-transform: uppercase;
  }

  .review-thread-location {
    min-width: 0;
    overflow: hidden;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .review-thread-status {
    flex-shrink: 0;
    padding: 1px 5px;
    border-radius: 999px;
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .review-thread-status--outdated {
    color: var(--accent-orange);
  }

  .review-thread-author {
    margin-top: 6px;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .review-thread-body {
    margin: 6px 0 0;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .review-thread-body--with-idle-reply {
    padding-right: 118px;
  }

  .provider-hidden-notice {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: 6px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .provider-hidden-toggle {
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
  }

  .review-thread-reply {
    margin-top: 8px;
  }

  textarea {
    box-sizing: border-box;
    width: 100%;
    min-height: 72px;
    resize: vertical;
    padding: 8px;
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    background: var(--bg-inset);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
  }

  .review-thread-error {
    margin: 6px 0 0;
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .review-thread-actions {
    display: flex;
    justify-content: flex-end;
    flex-wrap: wrap;
    gap: 6px;
    margin-top: 8px;
  }

  .review-thread-actions--idle {
    position: absolute;
    right: 12px;
    bottom: 14px;
    margin-top: 0;
  }

  @container (max-width: 420px) {
    .review-thread-actions--idle {
      position: static;
      margin-top: 8px;
    }

    .review-thread-body--with-idle-reply {
      padding-right: 0;
    }
  }

  :global(.review-thread-btn.kit-button) {
    min-height: 28px;
  }

  :global(.review-thread-btn--primary.kit-button) {
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: var(--text-on-accent);
  }
</style>
