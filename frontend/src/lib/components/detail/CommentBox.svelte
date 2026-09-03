<script lang="ts">
  import { getStores } from "../../context.js";
  import CommentEditor from "./CommentEditor.svelte";
  import {
    beginCommentSubmit,
    clearCommentDraft,
    finishCommentSubmit,
    getCommentDraft,
    getCommentDraftKey,
    isCommentSubmitPending,
    setCommentDraft,
  } from "./comment-drafts.svelte.js";

  const { detail } = getStores();

  interface Props {
    owner: string;
    name: string;
    number: number;
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
    disabled?: boolean;
    /** Shown under the editor when the box is disabled for a reason
     * the user can act on (e.g. missing write credential). */
    disabledReason?: string | undefined;
  }

  const {
    owner,
    name,
    number,
    provider,
    platformHost,
    repoPath,
    disabled = false,
    disabledReason = undefined,
  }: Props = $props();

  const currentDraftKey = $derived(
    getCommentDraftKey("pull", owner, name, number, platformHost),
  );
  const body = $derived(
    getCommentDraft("pull", owner, name, number, platformHost),
  );

  const isEmpty = $derived(body.trim() === "");
  const isPostingCurrent = $derived(
    isCommentSubmitPending("pull", owner, name, number, platformHost),
  );

  function handleSubmit(): void {
    if (isEmpty || isPostingCurrent || disabled) return;
    const submittedOwner = owner;
    const submittedName = name;
    const submittedNumber = number;
    const submittedBody = body.trim();
    const submittedPlatformHost = platformHost;
    beginCommentSubmit(
      "pull",
      submittedOwner,
      submittedName,
      submittedNumber,
      submittedPlatformHost,
    );
    detail.submitComment(submittedOwner, submittedName, submittedNumber, submittedBody, {
      onSuccess: () => {
        clearCommentDraft(
          "pull",
          submittedOwner,
          submittedName,
          submittedNumber,
          submittedPlatformHost,
        );
      },
      onSettled: () => {
        finishCommentSubmit(
        "pull",
        submittedOwner,
        submittedName,
        submittedNumber,
        submittedPlatformHost,
        );
      },
    });
  }
</script>

<div class="comment-box">
  {#key `pull:${owner}/${name}/${number}`}
    <div class="comment-editor-shell">
      <CommentEditor
        {owner}
        {name}
        {provider}
        {platformHost}
        {repoPath}
        itemType="pull"
        itemNumber={number}
        value={body}
        disabled={isPostingCurrent || disabled}
        oninput={(nextBody) => {
          setCommentDraft("pull", owner, name, number, nextBody, platformHost);
        }}
        onsubmit={() => {
          handleSubmit();
        }}
      />
      <button
        class="submit-btn"
        onclick={handleSubmit}
        disabled={isEmpty || isPostingCurrent || disabled}
        title={disabled && disabledReason !== undefined ? disabledReason : undefined}
      >
        {isPostingCurrent ? "Posting…" : "Comment"}
      </button>
    </div>
  {/key}
  {#if disabled && disabledReason !== undefined}
    <p class="error-msg">{disabledReason}</p>
  {/if}
</div>

<style>
  .comment-box {
    display: flex;
    flex-direction: column;
    gap: var(--focus-detail-space-sm, 8px);
  }

  .error-msg {
    font-size: var(--font-size-sm);
    color: var(--accent-red);
  }

  .comment-editor-shell {
    position: relative;
  }

  .comment-editor-shell :global(.comment-editor-input) {
    min-height: 112px;
    max-height: 75dvh;
    padding-bottom: calc(var(--focus-detail-hit-target, 39.5px) + var(--focus-detail-space-sm, 7.5px));
  }

  .submit-btn {
    position: absolute;
    right: var(--focus-detail-space-sm, 8px);
    bottom: var(--focus-detail-space-sm, 8px);
    font-size: var(--font-size-root);
    font-weight: 500;
    padding: var(--focus-detail-space-xs, 6px) var(--focus-detail-space-md, 14px);
    background: var(--accent-blue);
    color: var(--text-on-accent);
    border-radius: var(--radius-sm);
    cursor: pointer;
    z-index: 1;
    transition: opacity 0.15s;
  }

  .submit-btn:hover:enabled {
    opacity: 0.85;
  }

  .submit-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }
</style>
