<script lang="ts">
  import { ImagePreview } from "@kenn-io/kit-ui";
  import type { AppRuntime } from "../../app/runtime.js";
  import type { DiffFile, FilePreview } from "../../api/types.js";
  import { getStores } from "../../context.js";
  import type { DiffViewMode } from "../../stores/diff.svelte.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import MarkdownHtml from "../shared/MarkdownHtml.svelte";
  import {
    buildMarkdownRichPreview,
    type MarkdownRichPreviewBlock,
  } from "../../utils/markdown-rich-preview.js";
  import DiffReviewThreadInlineComment from "./DiffReviewThreadInlineComment.svelte";
  import {
    reviewThreadSnapshotState,
    reviewThreadTargetLine,
    reviewThreadTargetSide,
    type ReviewThread,
    type ReviewThreadCardPlacement,
  } from "./review-thread-context.js";

  interface Props {
    runtime: AppRuntime;
    file: DiffFile;
    provider: string;
    platformHost?: string | undefined;
    owner: string;
    name: string;
    repoPath: string;
    number: number;
    active: boolean;
    viewMode?: DiffViewMode;
    reviewThreads?: ReviewThread[];
    canReplyToThreads?: boolean;
    diffHeadSHA?: string | undefined;
    onreply?: ((thread: ReviewThread, body: string, callbacks: MutationCallbacks) => void) | undefined;
  }

  const {
    runtime,
    file,
    provider,
    platformHost,
    owner,
    name,
    repoPath,
    number,
    active,
    viewMode = "unified",
    reviewThreads = [],
    canReplyToThreads = false,
    diffHeadSHA,
    onreply,
  }: Props = $props();
  const { diff: diffStore } = getStores();

  type ReviewThreadPlacement = {
    thread: ReviewThread;
    placement: ReviewThreadCardPlacement;
  };
  type DiffHunk = DiffFile["hunks"][number];
  type DiffHunkLine = DiffHunk["lines"][number];
  type ChangedListMarker = {
    index: number;
    indent: number;
  };
  type AnchoredMarkdownBlock = MarkdownRichPreviewBlock & {
    leftReviewThreads: ReviewThreadPlacement[];
    rightReviewThreads: ReviewThreadPlacement[];
    reviewThreads: ReviewThreadPlacement[];
  };
  interface AnchoredMarkdownPreview {
    blocks: AnchoredMarkdownBlock[];
    fallbackReviewThreads: ReviewThreadPlacement[];
  }

  let loading = $state(false);
  let error = $state<string | null>(null);
  let preview = $state<FilePreview | null>(null);
  let requestVersion = 0;

  const isMarkdownFile = $derived(isMarkdownPath(file.path));
  const markdownPreview = $derived.by(() =>
    active && isMarkdownFile ? buildAnchoredMarkdownPreview(file, reviewThreads) : null,
  );
  const text = $derived(preview ? decodeText(preview.content) : "");
  const dataURL = $derived(preview ? `data:${preview.media_type};base64,${preview.content}` : "");
  const kind = $derived(previewKind(file.path, preview?.media_type ?? ""));
  const displayText = $derived(formatText(file.path, text));
  const fallbackReviewThreads = $derived<ReviewThreadPlacement[]>(
    markdownPreview?.fallbackReviewThreads ??
      (isMarkdownFile
        ? []
        : reviewThreads.map((thread) => ({ thread, placement: reviewThreadFallbackPlacement(thread) }))),
  );
  const fileHeaderReviewThreads = $derived(
    fallbackReviewThreads.filter(({ placement }) => placement === "file"),
  );
  const detachedReviewThreads = $derived(
    fallbackReviewThreads.filter(({ placement }) => placement === "outdated" || placement === "unavailable"),
  );

  $effect(() => {
    const sourceFile = file;
    const version = ++requestVersion;
    if (!active || isMarkdownFile) {
      loading = false;
      error = null;
      preview = null;
      return;
    }
    loading = true;
    error = null;
    preview = null;
    diffStore.loadFilePreview(owner, name, number, sourceFile.path, undefined, {
      onSuccess: (result) => {
        if (version !== requestVersion) return;
        preview = result;
      },
      onFailure: (message) => {
        if (version !== requestVersion) return;
        error = message;
      },
      onSettled: () => {
        if (version === requestVersion) loading = false;
      },
    });
  });

  function decodeText(content: string): string {
    const binary = atob(content);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
  }

  function isMarkdownPath(path: string): boolean {
    return [".md", ".markdown", ".mdown", ".mkd"].includes(extension(path));
  }

  function extension(path: string): string {
    const idx = path.lastIndexOf(".");
    return idx >= 0 ? path.slice(idx).toLowerCase() : "";
  }

  function previewKind(
    path: string,
    mediaType: string,
  ): "markdown" | "image" | "pdf" | "text" | "unsupported" {
    const ext = extension(path);
    if (mediaType.startsWith("image/")) return "image";
    if (mediaType === "application/pdf") return "pdf";
    if (
      mediaType.includes("markdown") ||
      [".md", ".markdown", ".mdown", ".mkd"].includes(ext)
    ) return "markdown";
    if (
      mediaType.startsWith("text/") ||
      mediaType.includes("json") ||
      mediaType.includes("yaml") ||
      mediaType.includes("toml") ||
      [".css", ".csv", ".html", ".js", ".jsx", ".ts", ".tsx", ".xml"].includes(ext)
    ) return "text";
    return "unsupported";
  }

  function formatText(path: string, value: string): string {
    if (extension(path) !== ".json") return value;
    try {
      return `${JSON.stringify(JSON.parse(value), null, 2)}\n`;
    } catch {
      return value;
    }
  }

  function buildAnchoredMarkdownPreview(
    source: DiffFile,
    threads: ReviewThread[],
  ): AnchoredMarkdownPreview {
    const blocks: AnchoredMarkdownBlock[] = buildMarkdownRichPreview(source, {
      provider,
      platformHost,
      owner,
      name,
      repoPath,
    }, reviewThreadSplitLines(source, threads)).blocks.map((block) => ({
      ...block,
      leftReviewThreads: [],
      rightReviewThreads: [],
      reviewThreads: [],
    }));
    const fallbackReviewThreads: ReviewThreadPlacement[] = [];
    for (const thread of threads) {
      const snapshotState = reviewThreadSnapshotState(thread, diffHeadSHA);
      if (thread.line_type === "file") {
        fallbackReviewThreads.push({ thread, placement: "file" });
        continue;
      }
      if (snapshotState === "stale") {
        fallbackReviewThreads.push({ thread, placement: "outdated" });
        continue;
      }
      const block = blocks.find((candidate) => blockContainsReviewThread(candidate, thread));
      if (!block) {
        fallbackReviewThreads.push({ thread, placement: "unavailable" });
        continue;
      }
      const placement = { thread, placement: "inline" as const };
      block.reviewThreads.push(placement);
      if (reviewThreadTargetSide(thread) === "left") {
        block.leftReviewThreads.push(placement);
      } else {
        block.rightReviewThreads.push(placement);
      }
    }
    sortReviewThreadPlacements(fallbackReviewThreads);
    for (const block of blocks) {
      sortReviewThreadPlacements(block.reviewThreads);
      sortReviewThreadPlacements(block.leftReviewThreads);
      sortReviewThreadPlacements(block.rightReviewThreads);
    }
    return { blocks, fallbackReviewThreads };
  }

  function reviewThreadSplitLines(source: DiffFile, threads: ReviewThread[]): {
    splitOldLines: number[];
    splitNewLines: number[];
  } {
    const splitOldLines: number[] = [];
    const splitNewLines: number[] = [];
    for (const thread of threads) {
      if (thread.line_type === "file" || reviewThreadSnapshotState(thread, diffHeadSHA) === "stale") continue;
      const side = reviewThreadTargetSide(thread);
      const targetLine = reviewThreadTargetLine(thread);
      const match = findReviewThreadDiffLine(source, side, targetLine);
      if (!match && side === "left") {
        addUniqueLine(splitOldLines, targetLine);
        continue;
      } else if (!match) {
        addUniqueLine(splitNewLines, targetLine);
        continue;
      }
      addDiffLineNumbers(splitOldLines, splitNewLines, match.lines[match.index]!);
      addListSplitLinesForChangedLine(splitOldLines, splitNewLines, match.lines, match.index);
    }
    return {
      splitOldLines,
      splitNewLines,
    };
  }

  function addUniqueLine(lines: number[], line: number): void {
    if (!lines.includes(line)) lines.push(line);
  }

  function findReviewThreadDiffLine(
    source: DiffFile,
    side: "left" | "right",
    targetLine: number,
  ): { lines: DiffHunk["lines"]; index: number } | undefined {
    for (const hunk of source.hunks ?? []) {
      const lines = hunk.lines ?? [];
      const index = lines.findIndex((candidate) => {
        const lineNumber = side === "left" ? candidate.old_num : candidate.new_num;
        return lineNumber === targetLine;
      });
      if (index >= 0) return { lines, index };
    }
    return undefined;
  }

  function addDiffLineNumbers(oldLines: number[], newLines: number[], line: DiffHunkLine): void {
    if (line.old_num != null) addUniqueLine(oldLines, line.old_num);
    if (line.new_num != null) addUniqueLine(newLines, line.new_num);
  }

  function addListSplitLinesForChangedLine(
    oldLines: number[],
    newLines: number[],
    lines: DiffHunk["lines"],
    targetIndex: number,
  ): void {
    const target = lines[targetIndex]!;
    if (target.type !== "add" && target.type !== "delete") return;
    const marker = changedListMarker(lines, targetIndex);
    if (!marker) return;

    if (marker.index !== targetIndex) addDiffLineNumbers(oldLines, newLines, lines[marker.index]!);

    const adjacent = adjacentOppositeChangedListMarker(lines, marker);
    if (adjacent) addDiffLineNumbers(oldLines, newLines, adjacent);

    const boundary = comparableListMarkerLine(lines, targetIndex, marker.indent);
    if (boundary) addDiffLineNumbers(oldLines, newLines, boundary);
  }

  function changedListMarker(lines: DiffHunk["lines"], targetIndex: number): ChangedListMarker | null {
    const target = lines[targetIndex]!;
    const targetIndent = listMarkerIndent(target.content);
    if (targetIndent != null) return { index: targetIndex, indent: targetIndent };

    let crossedBlank = false;
    for (let index = targetIndex - 1; index >= 0; index--) {
      const line = lines[index]!;
      if (line.content.trim() === "") {
        crossedBlank = true;
        continue;
      }
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent == null) continue;
      return isListContinuation(target.content, lineIndent, crossedBlank)
        ? { index, indent: lineIndent }
        : null;
    }
    return null;
  }

  function adjacentOppositeChangedListMarker(
    lines: DiffHunk["lines"],
    marker: ChangedListMarker,
  ): DiffHunkLine | undefined {
    const target = lines[marker.index]!;
    if (target.type !== "add" && target.type !== "delete") return undefined;
    const oppositeType = target.type === "add" ? "delete" : "add";
    return (
      previousAdjacentOppositeChangedListMarker(lines, marker.index, marker.indent, oppositeType) ??
      nextAdjacentOppositeChangedListMarker(
        lines,
        changedListItemEndIndex(lines, marker.index, marker.indent),
        marker.indent,
        oppositeType,
      )
    );
  }

  function previousAdjacentOppositeChangedListMarker(
    lines: DiffHunk["lines"],
    startIndex: number,
    targetIndent: number,
    oppositeType: DiffHunkLine["type"],
  ): DiffHunkLine | undefined {
    let crossedBlank = false;
    for (let index = startIndex - 1; index >= 0; index--) {
      const line = lines[index]!;
      if (line.content.trim() === "") {
        crossedBlank = true;
        continue;
      }
      if (line.type !== oppositeType) return undefined;
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent == null) {
        if (isListContinuation(line.content, targetIndent, crossedBlank)) continue;
        return undefined;
      }
      if (lineIndent < targetIndent) return undefined;
      if (lineIndent === targetIndent) return line;
      crossedBlank = false;
    }
    return undefined;
  }

  function nextAdjacentOppositeChangedListMarker(
    lines: DiffHunk["lines"],
    startIndex: number,
    targetIndent: number,
    oppositeType: DiffHunkLine["type"],
  ): DiffHunkLine | undefined {
    let crossedBlank = false;
    for (let index = startIndex + 1; index < lines.length; index++) {
      const line = lines[index]!;
      if (line.content.trim() === "") {
        crossedBlank = true;
        continue;
      }
      if (line.type !== oppositeType) return undefined;
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent == null) {
        if (isListContinuation(line.content, targetIndent, crossedBlank)) continue;
        return undefined;
      }
      if (lineIndent < targetIndent) return undefined;
      if (lineIndent === targetIndent) return line;
      crossedBlank = false;
    }
    return undefined;
  }

  function changedListItemEndIndex(
    lines: DiffHunk["lines"],
    markerIndex: number,
    targetIndent: number,
  ): number {
    const target = lines[markerIndex]!;
    let endIndex = markerIndex;
    let crossedBlank = false;
    for (let index = markerIndex + 1; index < lines.length; index++) {
      const line = lines[index]!;
      if (line.type !== target.type) return endIndex;
      if (line.content.trim() === "") {
        endIndex = index;
        crossedBlank = true;
        continue;
      }
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent != null) {
        if (lineIndent <= targetIndent) return endIndex;
        endIndex = index;
        crossedBlank = false;
        continue;
      }
      if (!isListContinuation(line.content, targetIndent, crossedBlank)) return endIndex;
      endIndex = index;
      crossedBlank = false;
    }
    return endIndex;
  }

  function comparableListMarkerLine(
    lines: DiffHunk["lines"],
    targetIndex: number,
    targetIndent: number,
  ): DiffHunkLine | undefined {
    return (
      previousComparableListMarkerLine(lines, targetIndex, targetIndent) ??
      nextComparableListMarkerLine(lines, targetIndex, targetIndent)
    );
  }

  function previousComparableListMarkerLine(
    lines: DiffHunk["lines"],
    targetIndex: number,
    targetIndent: number,
  ): DiffHunkLine | undefined {
    let crossedBlank = false;
    for (let index = targetIndex - 1; index >= 0; index--) {
      const line = lines[index]!;
      if (line.content.trim() === "") {
        crossedBlank = true;
        continue;
      }
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent == null) {
        if (isListContinuation(line.content, targetIndent, crossedBlank)) continue;
        return undefined;
      }
      if (lineIndent < targetIndent) return undefined;
      if (lineIndent === targetIndent && line.old_num != null && line.new_num != null) return line;
      crossedBlank = false;
    }
    return undefined;
  }

  function nextComparableListMarkerLine(
    lines: DiffHunk["lines"],
    targetIndex: number,
    targetIndent: number,
  ): DiffHunkLine | undefined {
    let crossedBlank = false;
    for (let index = targetIndex + 1; index < lines.length; index++) {
      const line = lines[index]!;
      if (line.content.trim() === "") {
        crossedBlank = true;
        continue;
      }
      const lineIndent = listMarkerIndent(line.content);
      if (lineIndent == null) {
        if (isListContinuation(line.content, targetIndent, crossedBlank)) continue;
        return undefined;
      }
      if (lineIndent < targetIndent) return undefined;
      if (lineIndent === targetIndent && line.old_num != null && line.new_num != null) return line;
      crossedBlank = false;
    }
    return undefined;
  }

  function isListContinuation(line: string, targetIndent: number, crossedBlank: boolean): boolean {
    const match = line.match(/^(\s*)/);
    const lineIndent = indentationWidth(match?.[1] ?? "");
    return lineIndent > targetIndent || (!crossedBlank && lineIndent === targetIndent);
  }

  function listMarkerIndent(line: string): number | null {
    const match = line.match(/^(\s{0,12})(?:[-+*]|\d+[.)])\s+/);
    if (!match) return null;
    return indentationWidth(match[1]!);
  }

  function indentationWidth(value: string): number {
    return Array.from(value).reduce((width, char) => width + (char === "\t" ? 4 : 1), 0);
  }

  function sortReviewThreadPlacements(placements: ReviewThreadPlacement[]): void {
    placements.sort(compareReviewThreadPlacements);
  }

  function compareReviewThreadPlacements(
    left: ReviewThreadPlacement,
    right: ReviewThreadPlacement,
  ): number {
    const lineDelta = reviewThreadTargetLine(left.thread) - reviewThreadTargetLine(right.thread);
    if (lineDelta !== 0) return lineDelta;
    const sideDelta = sideSortValue(left.thread) - sideSortValue(right.thread);
    if (sideDelta !== 0) return sideDelta;
    const createdAtDelta = Date.parse(left.thread.created_at) - Date.parse(right.thread.created_at);
    if (!Number.isNaN(createdAtDelta) && createdAtDelta !== 0) return createdAtDelta;
    return left.thread.id.localeCompare(right.thread.id);
  }

  function sideSortValue(thread: ReviewThread): number {
    return reviewThreadTargetSide(thread) === "left" ? 0 : 1;
  }

  function blockContainsReviewThread(block: MarkdownRichPreviewBlock, thread: ReviewThread): boolean {
    if (thread.line_type === "file" || reviewThreadSnapshotState(thread, diffHeadSHA) === "stale") return false;
    const line = reviewThreadTargetLine(thread);
    if (reviewThreadTargetSide(thread) === "left") {
      return lineInMappedBlock(line, block.oldLines, block.oldStart, block.oldEnd);
    }
    return lineInMappedBlock(line, block.newLines, block.newStart, block.newEnd);
  }

  function lineInMappedBlock(
    line: number,
    mappedLines: number[] | undefined,
    start: number | undefined,
    end: number | undefined,
  ): boolean {
    if (mappedLines?.length) return mappedLines.includes(line);
    return lineInRange(line, start, end);
  }

  function lineInRange(
    line: number,
    start: number | undefined,
    end: number | undefined,
  ): boolean {
    return start != null && end != null && line >= start && line <= end;
  }

  function reviewThreadFallbackPlacement(thread: ReviewThread): ReviewThreadCardPlacement {
    if (thread.line_type === "file") return "file";
    return reviewThreadSnapshotState(thread, diffHeadSHA) === "stale" ? "outdated" : "unavailable";
  }
</script>

<div class="preview-shell">
  {#if isMarkdownFile}
    {#each fileHeaderReviewThreads as placement (placement.thread.id)}
      <DiffReviewThreadInlineComment
        {runtime}
        thread={placement.thread}
        placement={placement.placement}
        canReply={canReplyToThreads}
        {onreply}
      />
    {/each}
    {#if markdownPreview}
      {#if viewMode === "split"}
        <div class="diff-rich-preview markdown-rich-diff markdown-rich-diff--split">
          <div class="markdown-rich-diff__split-header" aria-hidden="true">
            <div class="markdown-rich-diff__split-label markdown-rich-diff__label markdown-rich-diff__block--delete">
              Before
            </div>
            <div class="markdown-rich-diff__split-label markdown-rich-diff__label markdown-rich-diff__block--add">
              After
            </div>
          </div>
          <div class="markdown-rich-diff__split-rows">
            {#each markdownPreview.blocks as block (block.key)}
              <div class="markdown-rich-diff__split-row">
                <div
                  class="markdown-rich-diff__pane markdown-rich-diff__block--delete markdown-body"
                  aria-label="Before markdown preview"
                  data-markdown-rich-side="before"
                >
                  <div class="markdown-rich-diff__anchored-block markdown-rich-diff__anchored-block--spaced">
                    {@html block.beforeHtml ?? ""}
                  </div>
                  {#each block.leftReviewThreads as placement (placement.thread.id)}
                    <DiffReviewThreadInlineComment
                      {runtime}
                      thread={placement.thread}
                      placement={placement.placement}
                      canReply={canReplyToThreads}
                      {onreply}
                    />
                  {/each}
                </div>
                <div
                  class="markdown-rich-diff__pane markdown-rich-diff__block--add markdown-body"
                  aria-label="After markdown preview"
                  data-markdown-rich-side="after"
                >
                  <div class="markdown-rich-diff__anchored-block markdown-rich-diff__anchored-block--spaced">
                    {@html block.afterHtml ?? ""}
                  </div>
                  {#each block.rightReviewThreads as placement (placement.thread.id)}
                    <DiffReviewThreadInlineComment
                      {runtime}
                      thread={placement.thread}
                      placement={placement.placement}
                      canReply={canReplyToThreads}
                      {onreply}
                    />
                  {/each}
                </div>
              </div>
            {/each}
          </div>
        </div>
      {:else}
        <div class="diff-rich-preview markdown-rich-diff markdown-rich-diff--unified markdown-body">
          {#each markdownPreview.blocks as block (block.key)}
            <div class="markdown-rich-diff__anchored-block markdown-rich-diff__anchored-block--spaced">
              {@html block.unifiedHtml}
            </div>
            {#each block.reviewThreads as placement (placement.thread.id)}
              <DiffReviewThreadInlineComment
                {runtime}
                thread={placement.thread}
                placement={placement.placement}
                canReply={canReplyToThreads}
                {onreply}
              />
            {/each}
          {/each}
        </div>
      {/if}
      {#each detachedReviewThreads as placement (placement.thread.id)}
        <DiffReviewThreadInlineComment
          {runtime}
          thread={placement.thread}
          placement={placement.placement}
          canReply={canReplyToThreads}
          {onreply}
        />
      {/each}
    {:else}
      <div class="preview-state">Loading preview</div>
      {#each detachedReviewThreads as placement (placement.thread.id)}
        <DiffReviewThreadInlineComment
          {runtime}
          thread={placement.thread}
          placement={placement.placement}
          canReply={canReplyToThreads}
          {onreply}
        />
      {/each}
    {/if}
  {:else}
    {#each fileHeaderReviewThreads as placement (placement.thread.id)}
      <DiffReviewThreadInlineComment
        {runtime}
        thread={placement.thread}
        placement={placement.placement}
        canReply={canReplyToThreads}
        {onreply}
      />
    {/each}
    {#if loading}
      <div class="preview-state">Loading preview</div>
    {:else if error}
      <div class="preview-state preview-state--error">{error}</div>
    {:else if preview}
      {#if kind === "markdown"}
        <div class="diff-rich-preview markdown-body">
          <MarkdownHtml raw={text} repo={{ provider, platformHost, owner, name, repoPath }} />
        </div>
      {:else if kind === "image"}
        <ImagePreview src={dataURL} alt={file.path} />
      {:else if kind === "pdf"}
        <object
          class="diff-object-preview"
          data={dataURL}
          type={preview.media_type}
          aria-label={`${file.path} preview`}
        >
          <a href={dataURL}>Open PDF preview</a>
        </object>
      {:else if kind === "text"}
        <pre class="diff-text-preview">{displayText}</pre>
      {:else}
        <div class="preview-state">No rich preview for {preview.media_type}</div>
      {/if}
    {:else}
      <div class="preview-state">Loading preview</div>
    {/if}
    {#each detachedReviewThreads as placement (placement.thread.id)}
      <DiffReviewThreadInlineComment
        {runtime}
        thread={placement.thread}
        placement={placement.placement}
        canReply={canReplyToThreads}
        {onreply}
      />
    {/each}
  {/if}
</div>

<style>
  .preview-shell {
    min-height: 140px;
    background: var(--bg-surface);
  }

  .preview-state {
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 140px;
    padding: 20px;
    color: var(--text-muted);
    font-size: var(--font-size-md);
  }

  .preview-state--error {
    color: var(--accent-red);
  }

  .diff-rich-preview {
    box-sizing: border-box;
    max-width: 920px;
    padding: 24px 32px 36px;
    color: var(--text-primary);
  }

  .markdown-rich-diff--split {
    display: flex;
    flex-direction: column;
    gap: 12px;
    width: 100%;
    max-width: none;
  }

  .markdown-rich-diff__split-header,
  .markdown-rich-diff__split-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 12px;
  }

  .markdown-rich-diff__split-rows {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .markdown-rich-diff__pane {
    min-width: 0;
    padding: 12px 14px 18px;
    border: 1px solid var(--diff-border);
    border-radius: 6px;
  }

  .markdown-rich-diff__label {
    margin-bottom: 10px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 600;
    text-transform: uppercase;
  }

  .markdown-rich-diff__split-label {
    margin: 0;
    padding: 10px 14px;
    border: 1px solid var(--diff-border);
    border-radius: 6px;
  }

  .markdown-rich-diff__block--add {
    background: color-mix(in srgb, var(--diff-add-bg) 76%, transparent);
    border-color: color-mix(in srgb, var(--diff-add-text) 42%, var(--diff-border));
  }

  .markdown-rich-diff__block--delete {
    background: color-mix(in srgb, var(--diff-del-bg) 78%, transparent);
    border-color: color-mix(in srgb, var(--diff-del-text) 42%, var(--diff-border));
  }

  .markdown-rich-diff__block--add :global(*) {
    color: var(--text-primary);
  }

  .markdown-rich-diff__block--delete :global(*) {
    color: var(--text-primary);
  }

  .markdown-rich-diff--split :global(.markdown-diff__placeholder) {
    visibility: hidden;
    pointer-events: none;
  }

  .markdown-rich-diff--split :global(ins),
  .markdown-rich-diff--split :global(del) {
    color: var(--text-primary);
    text-decoration: none;
  }

  .markdown-rich-diff--split :global(.markdown-diff__block) {
    display: block;
  }

  .markdown-rich-diff--split :global(.markdown-diff__block),
  .markdown-rich-diff--split :global(.markdown-diff__block *) {
    text-decoration: none;
  }

  .markdown-rich-diff--split :global(ins:not(.markdown-diff__block)),
  .markdown-rich-diff--split :global(del:not(.markdown-diff__block)) {
    padding: 0 0.16em;
    border-radius: 3px;
  }

  .markdown-rich-diff--split :global(ins:not(.markdown-diff__block)) {
    background: color-mix(in srgb, var(--diff-add-bg) 78%, transparent);
  }

  .markdown-rich-diff--split :global(del:not(.markdown-diff__block)) {
    background: color-mix(in srgb, var(--diff-del-bg) 80%, transparent);
  }

  .markdown-rich-diff--split :global(ins.markdown-diff__block),
  .markdown-rich-diff--split :global(del.markdown-diff__block) {
    background: transparent;
  }

  .markdown-rich-diff--unified {
    max-width: 920px;
  }

  .markdown-rich-diff--unified :global(ins:not(.markdown-diff__block)),
  .markdown-rich-diff--unified :global(del:not(.markdown-diff__block)) {
    padding: 0 0.16em;
    border-radius: 3px;
    text-decoration-thickness: 1px;
    text-underline-offset: 0.12em;
  }

  .markdown-rich-diff--unified :global(ins:not(.markdown-diff__block)) {
    color: var(--text-primary);
    background: color-mix(in srgb, var(--diff-add-bg) 80%, transparent);
    text-decoration-color: color-mix(in srgb, var(--diff-add-text) 65%, transparent);
  }

  .markdown-rich-diff--unified :global(del:not(.markdown-diff__block)) {
    color: var(--text-primary);
    background: color-mix(in srgb, var(--diff-del-bg) 82%, transparent);
    text-decoration-color: color-mix(in srgb, var(--diff-del-text) 70%, transparent);
  }

  .markdown-rich-diff--unified :global(.markdown-diff__block) {
    display: block;
    margin: 7px 0;
    padding: 6px 8px;
    border: 1px solid transparent;
    text-decoration: none;
  }

  .markdown-rich-diff--unified :global(ins.markdown-diff__block) {
    background: var(--diff-add-bg);
    border-color: color-mix(in srgb, var(--diff-add-text) 32%, transparent);
  }

  .markdown-rich-diff--unified :global(del.markdown-diff__block) {
    background: var(--diff-del-bg);
    border-color: color-mix(in srgb, var(--diff-del-text) 36%, transparent);
  }

  .markdown-rich-diff--unified :global(.markdown-diff__structural[data-diff-kind="insert"]) {
    background: var(--diff-add-bg);
  }

  .markdown-rich-diff--unified :global(.markdown-diff__structural[data-diff-kind="delete"]) {
    background: var(--diff-del-bg);
  }

  .markdown-rich-diff :global(.markdown-diff__structural > ins:not(.markdown-diff__block)),
  .markdown-rich-diff :global(.markdown-diff__structural > del:not(.markdown-diff__block)) {
    padding: 0;
    background: transparent;
    text-decoration: none;
  }

  .markdown-rich-diff :global(.markdown-rich-diff__split-list) {
    margin: 0;
  }

  .markdown-rich-diff__anchored-block--spaced {
    margin-bottom: 13px;
  }

  .markdown-rich-diff__anchored-block--spaced:last-child {
    margin-bottom: 0;
  }

  .markdown-rich-diff__anchored-block--spaced:has(:global(.markdown-rich-diff__split-list)) {
    margin: 0;
  }

  .markdown-rich-diff--unified :global(.markdown-diff__block:has(.markdown-rich-diff__split-list)) {
    margin: 0;
    padding-block: 0;
  }

  .markdown-rich-diff :global(.markdown-rich-diff__split-list > li) {
    margin-block: 0;
  }

  @media (max-width: 760px) {
    .markdown-rich-diff__split-header,
    .markdown-rich-diff__split-row {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  .diff-object-preview {
    width: 100%;
    height: min(72vh, 900px);
    border: 0;
    background: var(--bg-surface);
  }

  .diff-text-preview {
    margin: 0;
    padding: 18px 22px 28px;
    color: var(--diff-text);
    background: var(--diff-bg);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 1.55;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
