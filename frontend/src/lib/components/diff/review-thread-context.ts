import type { DiffReviewThreadResponse } from "../../api/generated/models/index.js";
import type { DiffLine, DiffResult, PREvent } from "../../api/types.js";

export type ReviewThread = DiffReviewThreadResponse;

export type ReviewThreadSnapshotState = "fresh" | "stale" | "head-unknown";

export type ReviewThreadCardPlacement = "inline" | "file" | "outdated" | "unavailable";

export function reviewThreadSnapshotState(
  thread: Pick<ReviewThread, "diff_head_sha">,
  diffHeadSHA: string | undefined,
): ReviewThreadSnapshotState {
  if (!thread.diff_head_sha || !diffHeadSHA) return "head-unknown";
  return thread.diff_head_sha === diffHeadSHA ? "fresh" : "stale";
}

export type ReviewThreadContextLine = {
  key: string;
  type: DiffLine["type"];
  oldNum?: number | undefined;
  newNum?: number | undefined;
  content: string;
  target: boolean;
};

export type ReviewThreadContext = {
  path: string;
  lineLabel: string;
  lines: ReviewThreadContextLine[];
  outdated: boolean;
};

export type ReviewThreadProviderHiddenState = {
  reason: string | null;
};

export function reviewThreadProviderHiddenState(thread: ReviewThread): ReviewThreadProviderHiddenState | null {
  if (!thread.metadata_json) return null;
  try {
    const metadata = JSON.parse(thread.metadata_json) as unknown;
    if (metadata === null || typeof metadata !== "object" || Array.isArray(metadata)) return null;
    const values = metadata as Record<string, unknown>;
    if (values.provider_hidden !== true) return null;
    return {
      reason:
        typeof values.provider_hidden_reason === "string" && values.provider_hidden_reason.length > 0
          ? values.provider_hidden_reason
          : null,
    };
  } catch {
    return null;
  }
}

export function reviewThreadsFromEvents(events: PREvent[] | null | undefined): ReviewThread[] {
  const threads: ReviewThread[] = [];
  const seen = new Set<string>();

  for (const event of events ?? []) {
    const thread = event.diff_thread ?? (event as PREvent & { DiffThread?: ReviewThread }).DiffThread;
    if (!thread || seen.has(thread.id)) continue;
    seen.add(thread.id);
    threads.push(thread);
  }

  return threads;
}

export function reviewThreadTargetSide(thread: ReviewThread): "left" | "right" {
  return thread.side.toLowerCase() === "left" ? "left" : "right";
}

export function reviewThreadStartSide(thread: ReviewThread): "left" | "right" {
  return thread.start_side?.toLowerCase() === "left" ? "left" : reviewThreadTargetSide(thread);
}

export function reviewThreadTargetLine(thread: ReviewThread): number {
  return reviewThreadTargetSide(thread) === "left"
    ? (thread.old_line ?? thread.line)
    : (thread.new_line ?? thread.line);
}

export function reviewThreadStartLine(thread: ReviewThread): number {
  return thread.start_line ?? reviewThreadTargetLine(thread);
}

export function reviewThreadLineLabel(thread: ReviewThread): string {
  const start = reviewThreadStartLine(thread);
  const end = reviewThreadTargetLine(thread);
  return start !== end ? `${thread.path}:${start}-${end}` : `${thread.path}:${end}`;
}

function lineNumberForSide(line: DiffLine, side: "left" | "right"): number | undefined {
  return side === "left" ? line.old_num : line.new_num;
}

function pathMatches(thread: ReviewThread, filePath: string, oldPath: string): boolean {
  return (
    thread.path === filePath ||
    thread.path === oldPath ||
    (!!thread.old_path && !!oldPath && thread.old_path === oldPath)
  );
}

export function reviewThreadContext(diff: DiffResult | null | undefined, thread: ReviewThread): ReviewThreadContext {
  const fallback: ReviewThreadContext = {
    path: thread.path,
    lineLabel: reviewThreadLineLabel(thread),
    lines: [],
    outdated: true,
  };
  if (!diff) return fallback;

  const file = diff.files.find((item) => pathMatches(thread, item.path, item.old_path));
  if (!file || file.is_binary) return fallback;

  const side = reviewThreadTargetSide(thread);
  const startLine = reviewThreadStartLine(thread);
  const endLine = reviewThreadTargetLine(thread);
  const minLine = Math.min(startLine, endLine);
  const maxLine = Math.max(startLine, endLine);

  for (const hunk of file.hunks ?? []) {
    const targetIndexes: number[] = [];
    for (let index = 0; index < hunk.lines.length; index++) {
      const lineNumber = lineNumberForSide(hunk.lines[index]!, side);
      if (lineNumber != null && lineNumber >= minLine && lineNumber <= maxLine) {
        targetIndexes.push(index);
      }
    }
    if (targetIndexes.length === 0) continue;

    const first = Math.max(0, targetIndexes[0]! - 1);
    const last = Math.min(hunk.lines.length - 1, targetIndexes.at(-1)! + 1);
    return {
      path: file.path,
      lineLabel: reviewThreadLineLabel(thread),
      outdated: false,
      lines: hunk.lines.slice(first, last + 1).map((line, offset) => {
        const lineNumber = lineNumberForSide(line, side);
        return {
          key: `${first + offset}:${line.old_num ?? ""}:${line.new_num ?? ""}`,
          type: line.type,
          oldNum: line.old_num,
          newNum: line.new_num,
          content: line.content,
          target: lineNumber != null && lineNumber >= minLine && lineNumber <= maxLine,
        };
      }),
    };
  }

  return fallback;
}
