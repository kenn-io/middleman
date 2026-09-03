<script lang="ts">
  import type { DiffLineAnnotation, SelectedLineRange, Virtualizer } from "@pierre/diffs";
  import { Effect } from "effect";
  import { mount, onMount, unmount } from "svelte";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { nextMicrotask } from "../../browser/microtask.js";
  import { observeIntersection, observeMutation } from "../../browser/observers.js";
  import type { DiffFile as DiffFileType } from "../../api/types.js";
  import type { DiffReviewDraftComment } from "../../stores/diff-review-draft.svelte.js";
  import type { DiffReviewLineRange } from "../../stores/diff-review-draft.svelte.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import { STORES_KEY, getStores } from "../../context.js";
  import DiffInlineCommentComposer from "./DiffInlineCommentComposer.svelte";
  import DiffReviewDraftInlineComment from "./DiffReviewDraftInlineComment.svelte";
  import DiffReviewThreadInlineComment from "./DiffReviewThreadInlineComment.svelte";
  import DiffRichPreview from "./DiffRichPreview.svelte";
  import { CopyButton, DiffStats } from "@kenn-io/kit-ui";
  import {
    reviewThreadSnapshotState,
    reviewThreadTargetLine,
    reviewThreadTargetSide,
    type ReviewThread,
    type ReviewThreadCardPlacement,
  } from "./review-thread-context.js";
  import PierreFileDiff from "./PierreFileDiff.svelte";

  const stores = getStores();
  const diffStore = stores.diff;
  const diffReviewDraft = stores.diffReviewDraft;
  const runtime = getAppRuntime();

  interface Props {
    file: DiffFileType;
    contextPrefetchIdentity?: string | undefined;
    provider: string;
    platformHost?: string | undefined;
    owner: string;
    name: string;
    repoPath: string;
    number: number;
    richPreviewEnabled?: boolean;
    contextExpansionEnabled?: boolean;
    reviewEnabled?: boolean;
    canReplyToThreads?: boolean;
    diffHeadSHA?: string | undefined;
    nativeMultilineRanges?: boolean;
    reviewThreads?: ReviewThread[];
    virtualizer?: Virtualizer | undefined;
  }

  const {
    file,
    contextPrefetchIdentity = "",
    provider,
    platformHost,
    owner,
    name,
    repoPath,
    number,
    richPreviewEnabled = true,
    contextExpansionEnabled = true,
    reviewEnabled = false,
    canReplyToThreads = false,
    diffHeadSHA = undefined,
    nativeMultilineRanges = false,
    reviewThreads = [],
    virtualizer,
  }: Props = $props();

  const collapsed = $derived(diffStore.isFileCollapsed(owner, name, number, file.path));
  const richPreview = $derived(diffStore.getRichPreview());
  const wordWrap = $derived(diffStore.getWordWrap());
  const viewMode = $derived(diffStore.getViewMode());
  const tabWidth = $derived(diffStore.getTabWidth());
  const filePreviewGeneration = $derived(diffStore.getFilePreviewGeneration());
  const showRichPreview = $derived(
    richPreviewEnabled && richPreview && supportsRichPreview(file.path),
  );
  const richPreviewKey = $derived(`${file.path}:${filePreviewGeneration}`);
  const textDiffKey = $derived(`${file.path}:${file.old_path ?? ""}:${filePreviewGeneration}`);
  const fileDraftComments = $derived(
    diffReviewDraft.getComments().filter((comment) => comment.path === file.path),
  );
  const fileReviewThreads = $derived(
    reviewThreads.filter((thread) => threadMatchesFile(thread)),
  );
  const fileHunks = $derived(file.hunks ?? []);

  // Track viewport visibility so off-screen files skip expensive tokenization
  // on whitespace toggles and theme switches. Starts false so the initial
  // render on large diffs doesn't eagerly tokenize every file before the
  // IntersectionObserver reports visibility — the first observer callback
  // fires synchronously for on-screen files.
  let fileEl: HTMLDivElement | undefined = $state();
  let inViewport = $state(false);
  type MountedAnnotation = {
    component?: object;
    execution?: AppExecution<void, never>;
    released?: boolean;
    target: HTMLElement;
  };
  type ReviewSide = "left" | "right";
  type PierreSide = "deletions" | "additions";
  type ReviewLineRef = {
    side: ReviewSide;
    order: number;
    hunkIndex: number;
    line: number;
    oldLine?: number | undefined;
    newLine?: number | undefined;
    lineType: "context" | "add" | "delete";
  };
  type DiffAnnotation =
    | { kind: "draft"; id: string; comment: DiffReviewDraftComment }
    | { kind: "thread"; id: string; thread: ReviewThread; canReply: boolean }
    | { kind: "composer"; id: string; range: DiffReviewLineRange };
  const mountedAnnotations = new Set<MountedAnnotation>();
  let annotationMountsEnabled = false;

  let selectedRange = $state<SelectedLineRange | null>(null);
  let composerRange = $state<DiffReviewLineRange | null>(null);
  let composerBody = $state("");
  let suppressNextPierreSelection = false;
  const selectableLineRefs = $derived.by(() => ({
    left: selectableLines("left"),
    right: selectableLines("right"),
  }));
  const lineAnnotations = $derived.by<DiffLineAnnotation<DiffAnnotation>[]>(() => {
    const annotations: DiffLineAnnotation<DiffAnnotation>[] = [];
    if (reviewEnabled) {
      for (const comment of fileDraftComments) {
        annotations.push({
          side: pierreSide(commentSide(comment)),
          lineNumber: comment.line,
          metadata: { kind: "draft", id: comment.id, comment },
        });
      }
    }
    for (const thread of fileReviewThreads) {
      if (reviewThreadPlacement(thread) !== "inline") continue;
      annotations.push({
        side: pierreSide(reviewThreadTargetSide(thread)),
        lineNumber: reviewThreadTargetLine(thread),
        metadata: { kind: "thread", id: thread.id, thread, canReply: canReplyToThreads },
      });
    }
    return annotations;
  });
  const pierreLineAnnotations = $derived(lineAnnotations as DiffLineAnnotation<unknown>[]);
  const composerAnnotation = $derived.by<DiffLineAnnotation<DiffAnnotation> | null>(() => {
    if (!reviewEnabled || !composerRange) return null;
    return {
      side: pierreSide(reviewSideFromValue(composerRange.side)),
      lineNumber: composerRange.line,
      metadata: {
        kind: "composer",
        id: `composer:${rangeKey(composerRange)}`,
        range: composerRange,
      },
    };
  });
  const pierreComposerAnnotation = $derived(
    composerAnnotation as DiffLineAnnotation<unknown> | null,
  );
  const draftSelectedRanges = $derived.by<SelectedLineRange[]>(() => {
    if (!reviewEnabled) return [];
    const ranges: SelectedLineRange[] = [];
    for (const comment of fileDraftComments) {
      const range = selectedRangeForDraftComment(comment);
      if (range) ranges.push(range);
    }
    return ranges;
  });

  onMount(() => {
    annotationMountsEnabled = true;
    let visibilityExecution: AppExecution<void, never> | undefined;
    // Guard for jsdom / SSR-ish test environments where IntersectionObserver
    // is not provided — treat the file as visible so rendering still runs.
    if (typeof IntersectionObserver === "undefined") {
      inViewport = true;
    } else if (fileEl) {
      const root = fileEl.closest(".kit-scrollbox__viewport");
      visibilityExecution = runtime.runCommand(
        Effect.scoped(
          observeIntersection(
            fileEl,
            (entries) => { inViewport = entries[0]!.isIntersecting; },
            { root, rootMargin: "600px 0px" },
          ).pipe(Effect.andThen(Effect.never)),
        ),
        { operation: "observe diff file visibility", safeContext: { path: file.path }, onFailure: () => {} },
      );
    }

    return () => {
      annotationMountsEnabled = false;
      visibilityExecution?.interrupt();
      clearMountedAnnotations();
    };
  });

  function toggle(): void {
    diffStore.toggleFileCollapsed(owner, name, number, file.path);
  }

  function loadDiffContext(callbacks: {
    readonly onSuccess: (context: { readonly oldText: string; readonly newText: string }) => void;
    readonly onFailure: (message: string) => void;
    readonly onSettled: () => void;
  }): void {
    diffStore.loadFileContextPreviews(owner, name, number, file, {
      onSuccess: (previews) =>
        callbacks.onSuccess({
          oldText: previews.old === null ? "" : decodePreviewText(previews.old.content),
          newText: previews.new === null ? "" : decodePreviewText(previews.new.content),
        }),
      onFailure: callbacks.onFailure,
      onSettled: callbacks.onSettled,
    });
  }

  function decodePreviewText(content: string): string {
    const binary = atob(content);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
  }

  function displayPath(f: DiffFileType): string {
    if (f.status === "renamed" && f.old_path !== f.path) {
      return `${f.old_path} -> ${f.path}`;
    }
    return f.path;
  }

  function supportsRichPreview(path: string): boolean {
    const idx = path.lastIndexOf(".");
    const ext = idx >= 0 ? path.slice(idx).toLowerCase() : "";
    return [
      ".avif",
      ".gif",
      ".jpeg",
      ".jpg",
      ".markdown",
      ".md",
      ".mdown",
      ".mkd",
      ".pdf",
      ".png",
      ".svg",
      ".webp",
    ].includes(ext);
  }

  function threadMatchesFile(thread: ReviewThread): boolean {
    return thread.path === file.path ||
      thread.path === file.old_path ||
      (!!thread.old_path && !!file.old_path && thread.old_path === file.old_path);
  }

  function lineMatchesReviewThread(
    line: DiffFileType["hunks"][number]["lines"][number],
    thread: ReviewThread,
  ): boolean {
    const lineNumber = reviewThreadTargetSide(thread) === "left"
      ? line.old_num
      : line.new_num;
    return lineNumber != null && lineNumber === reviewThreadTargetLine(thread);
  }

  function hasRenderedReviewThread(thread: ReviewThread): boolean {
    if (file.is_binary) return false;
    return fileHunks.some((hunk) =>
      hunk.lines.some((line) => lineMatchesReviewThread(line, thread)),
    );
  }

  function reviewThreadPlacement(thread: ReviewThread): ReviewThreadCardPlacement {
    if (thread.line_type === "file") return "file";
    if (reviewThreadSnapshotState(thread, diffHeadSHA) === "stale") return "outdated";
    if (hasRenderedReviewThread(thread)) return "inline";
    return "unavailable";
  }

  const fileHeaderReviewThreads = $derived(
    fileReviewThreads.filter((thread) => reviewThreadPlacement(thread) === "file"),
  );
  const detachedReviewThreads = $derived(
    fileReviewThreads.filter((thread) => {
      const placement = reviewThreadPlacement(thread);
      return placement === "outdated" || placement === "unavailable";
    }),
  );

  function lineRef(
    line: DiffFileType["hunks"][number]["lines"][number],
    side: ReviewSide,
    order: number,
    hunkIndex: number,
  ): ReviewLineRef | null {
    const lineNumber = side === "right" ? line.new_num : line.old_num;
    if (lineNumber == null) return null;
    return {
      side,
      order,
      hunkIndex,
      line: lineNumber,
      oldLine: line.old_num,
      newLine: line.new_num,
      lineType: line.type,
    };
  }

  function selectableLines(side: ReviewSide): ReviewLineRef[] {
    const refs: ReviewLineRef[] = [];
    let order = 0;
    for (let hunkIndex = 0; hunkIndex < fileHunks.length; hunkIndex++) {
      const hunk = fileHunks[hunkIndex]!;
      for (const line of hunk.lines) {
        const ref = lineRef(line, side, order, hunkIndex);
        if (ref) refs.push(ref);
        order += 1;
      }
    }
    return refs;
  }

  function pierreSide(side: ReviewSide): PierreSide {
    return side === "left" ? "deletions" : "additions";
  }

  function reviewSide(side: PierreSide | undefined): ReviewSide {
    return side === "deletions" ? "left" : "right";
  }

  function refForSelection(line: number, side: ReviewSide): ReviewLineRef | null {
    return selectableLineRefs[side].find((ref) => ref.line === line) ?? null;
  }

  function rangeFor(start: ReviewLineRef, end: ReviewLineRef): DiffReviewLineRange {
    const [first, last] = start.order <= end.order ? [start, end] : [end, start];
    return {
      path: file.path,
      side: last.side,
      line: last.line,
      line_type: last.lineType,
      ...(file.old_path !== file.path && { old_path: file.old_path }),
      ...(first.order !== last.order && {
        start_side: first.side,
        start_line: first.line,
      }),
      ...(last.oldLine != null && { old_line: last.oldLine }),
      ...(last.newLine != null && { new_line: last.newLine }),
      ...(diffHeadSHA && { diff_head_sha: diffHeadSHA }),
    };
  }

  function rangeKey(range: DiffReviewLineRange): string {
    return [
      range.start_side ?? range.side,
      range.start_line ?? range.line,
      range.side,
      range.line,
    ].join(":");
  }

  function selectedLinesFor(start: ReviewLineRef, end: ReviewLineRef): SelectedLineRange {
    return {
      start: start.line,
      end: end.line,
      side: pierreSide(start.side),
      ...(start.side !== end.side && { endSide: pierreSide(end.side) }),
    };
  }

  function normalizedSelection(
    selection: SelectedLineRange,
  ): { selected: SelectedLineRange; range: DiffReviewLineRange } | null {
    if (!reviewEnabled || !diffHeadSHA) return null;
    const startSide = reviewSide(selection.side);
    const endSide = reviewSide(selection.endSide ?? selection.side);
    const start = refForSelection(selection.start, startSide);
    const end = refForSelection(selection.end, endSide);
    if (!start || !end) return null;
    if (
      !nativeMultilineRanges ||
      start.side !== end.side ||
      start.hunkIndex !== end.hunkIndex
    ) {
      return {
        selected: selectedLinesFor(end, end),
        range: rangeFor(end, end),
      };
    }
    return {
      selected: selectedLinesFor(start, end),
      range: rangeFor(start, end),
    };
  }

  function handlePierreSelection(selection: SelectedLineRange | null): void {
    if (suppressNextPierreSelection) {
      suppressNextPierreSelection = false;
      return;
    }
    if (!selection) {
      closeComposer();
      return;
    }
    const normalized = normalizedSelection(selection);
    if (!normalized) {
      closeComposer();
      return;
    }
    if (composerRange && rangeKey(composerRange) !== rangeKey(normalized.range)) {
      composerRange = null;
      composerBody = "";
    }
    selectedRange = normalized.selected;
  }

  function handlePierreGutterUtility(selection: SelectedLineRange): void {
    const normalized = normalizedSelection(selection);
    if (!normalized) {
      closeComposer();
      return;
    }
    if (composerRange && rangeKey(composerRange) === rangeKey(normalized.range)) {
      closeComposer();
      suppressNextPierreSelection = true;
      return;
    }
    selectedRange = normalized.selected;
    composerBody = "";
    composerRange = normalized.range;
  }

  function commentSide(comment: DiffReviewDraftComment): ReviewSide {
    return reviewSideFromValue(comment.side);
  }

  function selectedRangeForDraftComment(comment: DiffReviewDraftComment): SelectedLineRange | null {
    if (comment.line_type === "file") return null;
    if (reviewThreadSnapshotState(comment, diffHeadSHA) === "stale") return null;
    const endSide = commentSide(comment);
    const end = refForSelection(comment.line, endSide);
    if (!end) return null;
    const startLine = comment.start_line ?? comment.line;
    const startSide = comment.start_side ? reviewSideFromValue(comment.start_side) : endSide;
    const start = refForSelection(startLine, startSide);
    if (!start || start.hunkIndex !== end.hunkIndex || start.side !== end.side) {
      return selectedLinesFor(end, end);
    }
    return selectedLinesFor(start, end);
  }

  function reviewSideFromValue(side: string): ReviewSide {
    return side.toLowerCase() === "left" ? "left" : "right";
  }

  function renderAnnotation(annotation: DiffLineAnnotation<DiffAnnotation>): HTMLElement {
    const target = document.createElement("div");
    target.className = "pierre-annotation-host";
    const mounted: MountedAnnotation = { target };
    mountedAnnotations.add(mounted);
    mounted.execution = runtime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          yield* nextMicrotask;
          if (!annotationMountsEnabled || !target.isConnected) return;
          mounted.component = mountAnnotationComponent(target, annotation.metadata);
          yield* nextMicrotask;
          if (!target.isConnected || typeof MutationObserver === "undefined") return yield* Effect.never;
          const root = target.getRootNode();
          const observedRoot = root instanceof ShadowRoot || root instanceof Document
            ? root
            : document;
          yield* observeMutation(
            observedRoot,
            () => {
              if (!target.isConnected) mounted.execution?.interrupt();
            },
            { childList: true, subtree: true },
          );
          return yield* Effect.never;
        }).pipe(Effect.ensuring(Effect.suspend(() => releaseMountedAnnotation(mounted)))),
      ),
      { operation: "mount diff annotation", safeContext: { path: file.path }, onFailure: () => {} },
    );
    return target;
  }

  function mountAnnotationComponent(target: HTMLElement, metadata: DiffAnnotation): object {
    const context = new Map([[STORES_KEY, stores]]);
    return metadata.kind === "draft"
      ? mount(DiffReviewDraftInlineComment, {
        target,
        props: { runtime, comment: metadata.comment },
        context,
      })
      : metadata.kind === "thread"
        ? mount(DiffReviewThreadInlineComment, {
          target,
          props: {
            runtime,
            thread: metadata.thread,
            canReply: metadata.canReply,
            onreply: replyToThread,
          },
          context,
        })
        : mount(DiffInlineCommentComposer, {
          target,
          props: {
            runtime,
            range: metadata.range,
            body: composerBody,
            onbodychange: (body) => {
              if (composerRange && rangeKey(composerRange) === rangeKey(metadata.range)) {
                composerBody = body;
              }
            },
            onclose: closeComposer,
          },
          context,
        });
  }

  function renderUnknownAnnotation(annotation: DiffLineAnnotation<unknown>): HTMLElement {
    return renderAnnotation(annotation as DiffLineAnnotation<DiffAnnotation>);
  }

  function releaseMountedAnnotation(mounted: MountedAnnotation): Effect.Effect<void> {
    if (mounted.released) return Effect.void;
    mounted.released = true;
    mountedAnnotations.delete(mounted);
    const component = mounted.component;
    return component === undefined
      ? Effect.void
      : Effect.promise(() => unmount(component));
  }

  function clearMountedAnnotations(): void {
    for (const mounted of mountedAnnotations) {
      mounted.execution?.interrupt();
    }
  }

  function replyToThread(thread: ReviewThread, body: string, callbacks: MutationCallbacks): void {
    const detailStore = stores.detail;
    if (detailStore === undefined) {
      callbacks.onFailure?.("Pull request details are unavailable");
      callbacks.onSettled?.();
      return;
    }
    detailStore.replyToDiscussion(owner, name, number, thread.id, body, callbacks);
  }

  function closeComposer(): void {
    composerRange = null;
    composerBody = "";
    selectedRange = null;
  }

  let reviewContextKey = "";
  $effect(() => {
    const nextKey = reviewEnabled && diffHeadSHA
      ? `${file.path}:${file.old_path ?? ""}:${diffHeadSHA}`
      : "";
    if (nextKey !== reviewContextKey) {
      reviewContextKey = nextKey;
      composerRange = null;
      composerBody = "";
      selectedRange = null;
    }
  });

</script>

<div class="diff-file" data-file-path={file.path} bind:this={fileEl}>
  <div class="file-header">
    <button
      type="button"
      class="file-collapse-toggle"
      onclick={toggle}
      aria-label={collapsed ? "Expand file" : "Collapse file"}
      aria-expanded={!collapsed}
      title={collapsed ? "Expand file" : "Collapse file"}
    >
      <svg class="collapse-chevron" class:collapse-chevron--collapsed={collapsed} width="12" height="12" viewBox="0 0 12 12" fill="none">
        <path d="M3 4.5L6 7.5L9 4.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    </button>
    <span
      class="file-path"
      class:file-path--deleted={file.status === "deleted"}
    >
      {displayPath(file)}
    </span>
    <!--
      Copying repository metadata is not code execution or a security boundary.
      Preserve the provider's filename exactly; interpretation after paste belongs
      to the destination explicitly chosen by the user. Do not filter, quote,
      escape, or add shell-specific confirmation here.
    -->
    <CopyButton
      class="file-path-copy"
      text={file.path}
      ariaLabel={`Copy file path ${file.path}`}
      copiedAriaLabel={`Copied file path ${file.path}`}
      title="Copy file path"
      copiedTitle="Copied!"
    />
    <span class="file-stats">
      <DiffStats
        additions={file.additions}
        deletions={file.deletions}
        dimZeros
      />
    </span>
  </div>
  {#if !collapsed}
    <div class="file-content">
      {#if showRichPreview}
        {#key richPreviewKey}
          <DiffRichPreview
            {runtime}
            {file}
            {provider}
            {platformHost}
            {owner}
            {name}
            {repoPath}
            {number}
            active={inViewport}
            {viewMode}
            reviewThreads={fileReviewThreads}
            {canReplyToThreads}
            {diffHeadSHA}
            onreply={replyToThread}
          />
        {/key}
      {:else}
        {#each fileHeaderReviewThreads as thread (thread.id)}
          <DiffReviewThreadInlineComment {runtime} {thread} placement="file" />
        {/each}
        {#if file.is_binary}
          <div class="binary-notice">Binary file changed</div>
        {:else}
          {#key textDiffKey}
            <PierreFileDiff
              {file}
              active={inViewport}
              {contextPrefetchIdentity}
              {viewMode}
              {wordWrap}
              {tabWidth}
              loadFileContext={contextExpansionEnabled ? loadDiffContext : undefined}
              lineAnnotations={pierreLineAnnotations}
              transientLineAnnotation={pierreComposerAnnotation}
              selectedRange={selectedRange}
              selectedRanges={draftSelectedRanges}
              enableLineSelection={reviewEnabled && !!diffHeadSHA}
              onLineSelected={handlePierreSelection}
              onGutterUtilityClick={handlePierreGutterUtility}
              renderAnnotation={renderUnknownAnnotation}
              {virtualizer}
            />
          {/key}
        {/if}
        {#each detachedReviewThreads as thread (thread.id)}
          <DiffReviewThreadInlineComment
            {runtime}
            {thread}
            placement={reviewThreadPlacement(thread)}
          />
        {/each}
      {/if}
    </div>
  {/if}
</div>

<style>
  .diff-file {
    border-top: 2px solid var(--diff-border);
  }

  .file-header {
    position: sticky;
    top: 0;
    z-index: 2;
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 6px 12px;
    background: var(--diff-header-bg);
    border-bottom: 1px solid var(--diff-border);
    font-size: var(--font-size-sm);
    text-align: left;
    color: var(--diff-text);
  }

  .file-header:hover {
    background: var(--bg-surface-hover);
  }

  .file-collapse-toggle {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    margin: -4px -6px;
    flex: 0 0 24px;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    cursor: pointer;
  }

  .file-collapse-toggle:focus-visible {
    outline: 1px solid var(--border-strong);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  .collapse-chevron {
    transition: transform 0.15s ease-out;
    flex-shrink: 0;
  }

  .collapse-chevron--collapsed {
    transform: rotate(-90deg);
  }

  .file-path {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    color: var(--diff-text);
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .file-path--deleted {
    text-decoration: line-through;
  }

  .file-stats {
    display: flex;
    flex-shrink: 0;
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .file-content {
    overflow-x: auto;
    container-type: inline-size;
    background: var(--diff-bg);
  }

  :global(.diff-area--word-wrap) .file-content {
    overflow-x: hidden;
  }

  .binary-notice {
    padding: 20px;
    text-align: center;
    color: var(--diff-line-num);
    font-size: var(--font-size-md);
    font-style: italic;
  }

</style>
