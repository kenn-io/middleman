import { describe, expect, it } from "vite-plus/test";

import type { DiffResult, PREvent } from "../../api/types.js";
import {
  reviewThreadContext,
  reviewThreadSnapshotState,
  reviewThreadsFromEvents,
  type ReviewThread,
} from "./review-thread-context.js";

function makeThread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: "thread-1",
    provider_comment_id: "comment-1",
    path: "src/new-other.ts",
    old_path: "",
    side: "right",
    line: 2,
    new_line: 2,
    line_type: "add",
    body: "Published review note",
    author_login: "reviewer",
    resolved: false,
    can_resolve: false,
    created_at: "2026-03-30T14:01:00Z",
    updated_at: "2026-03-30T14:01:00Z",
    ...overrides,
  };
}

function makeDiff(): DiffResult {
  return {
    stale: false,
    whitespace_only_count: 0,
    files: [
      {
        path: "src/new.ts",
        old_path: "",
        status: "added",
        is_binary: false,
        is_whitespace_only: false,
        additions: 1,
        deletions: 0,
        hunks: [
          {
            old_start: 0,
            old_count: 0,
            new_start: 1,
            new_count: 1,
            lines: [{ type: "add", content: "new line", new_num: 2 }],
          },
        ],
      },
    ],
  };
}

describe("reviewThreadContext", () => {
  it.each([
    ["fresh", "thread-head", "thread-head"],
    ["stale", "thread-head", "other-head"],
    ["head-unknown", undefined, "thread-head"],
    ["head-unknown", "thread-head", undefined],
    ["head-unknown", "", "thread-head"],
    ["head-unknown", "thread-head", ""],
  ] as const)("classifies %s thread and diff heads", (expected, threadHead, diffHead) => {
    expect(reviewThreadSnapshotState(makeThread({ diff_head_sha: threadHead }), diffHead)).toBe(expected);
  });

  it("extracts review threads from generated client event casing", () => {
    const thread = makeThread({ id: "thread-pascal" });
    const event = { DiffThread: thread } as unknown as PREvent;

    expect(reviewThreadsFromEvents([event])).toEqual([thread]);
  });

  it("does not match added files only because old paths are empty", () => {
    const context = reviewThreadContext(makeDiff(), makeThread());

    expect(context.outdated).toBe(true);
    expect(context.lines).toHaveLength(0);
  });
});
