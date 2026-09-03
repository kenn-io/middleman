import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect } from "effect";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";

// jsdom does not ship IntersectionObserver; install a stub that reports the
// observed element as visible immediately so the viewport-gated render effect
// actually runs under test. The original global (if any) is saved and restored after
// the suite so it does not leak into sibling test files.
type GlobalWithIO = { IntersectionObserver?: unknown };
type GlobalWithResizeObserver = { ResizeObserver?: unknown };
type GlobalWithCSSStyleSheet = {
  CSSStyleSheet?: {
    prototype: CSSStyleSheet & { replaceSync?: (text: string) => void };
  };
};
let originalIntersectionObserver: unknown;
let originalIntersectionObserverExisted = false;
let originalResizeObserver: unknown;
let originalResizeObserverExisted = false;
let resizeObserverInstances: ResizeObserverStub[] = [];
let originalReplaceSync: unknown;
let runtime: OwnedAppRuntime;

class ResizeObserverStub {
  readonly observed: Element[] = [];
  disconnected = false;

  constructor() {
    resizeObserverInstances.push(this);
  }

  observe(target: Element): void {
    this.observed.push(target);
  }

  unobserve(): void {}

  disconnect(): void {
    this.disconnected = true;
  }
}

beforeEach(() => {
  resizeObserverInstances = [];
  runtime = makeAppRuntime();
});

afterEach(async () => {
  await Effect.runPromise(runtime.disposeEffect);
});

beforeAll(() => {
  originalIntersectionObserverExisted = "IntersectionObserver" in globalThis;
  originalIntersectionObserver = (globalThis as GlobalWithIO).IntersectionObserver;
  class IntersectionObserverStub {
    private readonly callback: IntersectionObserverCallback;
    root: Element | null = null;
    rootMargin = "";
    thresholds: readonly number[] = [];
    constructor(callback: IntersectionObserverCallback) {
      this.callback = callback;
    }
    observe(target: Element): void {
      // Report the element as visible immediately so viewport-gated work
      // (like tokenization in DiffFile) actually executes under test.
      const entry = {
        isIntersecting: true,
        intersectionRatio: 1,
        target,
        boundingClientRect: {} as DOMRectReadOnly,
        intersectionRect: {} as DOMRectReadOnly,
        rootBounds: null,
        time: 0,
      } as IntersectionObserverEntry;
      this.callback([entry], this as unknown as IntersectionObserver);
    }
    unobserve(): void {}
    disconnect(): void {}
    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }
  }
  (globalThis as GlobalWithIO).IntersectionObserver = IntersectionObserverStub;

  originalResizeObserverExisted = "ResizeObserver" in globalThis;
  originalResizeObserver = (globalThis as GlobalWithResizeObserver).ResizeObserver;
  (globalThis as GlobalWithResizeObserver).ResizeObserver = ResizeObserverStub;

  originalReplaceSync = (globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet?.prototype.replaceSync;
  if ((globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet?.prototype) {
    (globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet.prototype.replaceSync ??= function replaceSync(): void {};
  }
});

afterAll(() => {
  if (originalIntersectionObserverExisted) {
    (globalThis as GlobalWithIO).IntersectionObserver = originalIntersectionObserver;
  } else {
    delete (globalThis as GlobalWithIO).IntersectionObserver;
  }
  if (originalResizeObserverExisted) {
    (globalThis as GlobalWithResizeObserver).ResizeObserver = originalResizeObserver;
  } else {
    delete (globalThis as GlobalWithResizeObserver).ResizeObserver;
  }
  if ((globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet?.prototype) {
    if (originalReplaceSync) {
      (globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet.prototype.replaceSync = originalReplaceSync as (
        text: string,
      ) => void;
    } else {
      delete (globalThis as GlobalWithCSSStyleSheet).CSSStyleSheet.prototype.replaceSync;
    }
  }
});

import DiffFile from "./DiffFileRuntimeHarness.svelte";
import diffRichPreviewSource from "./DiffRichPreview.svelte?raw";
import type { DiffFile as DiffFileType, FilePreview } from "../../api/types.js";
import { STORES_KEY } from "../../context.js";
import type { DiffReviewDraftComment, DiffReviewLineRange } from "../../stores/diff-review-draft.svelte.js";
import type { FilePreviewCallbacks } from "../../stores/diff.svelte.js";
import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
import { createDiffStore } from "../../stores/diff.svelte.js";
import { renderedCodeSide } from "./pierre-dom.js";
import type { ReviewThread } from "./review-thread-context.js";

type LoadFilePreview = (
  owner: string,
  name: string,
  number: number,
  path: string,
  side?: "old" | "new",
  callbacks?: FilePreviewCallbacks,
) => void;

type FilePreviewPairCallbacks = {
  readonly onSuccess?: (previews: { readonly old: FilePreview | null; readonly new: FilePreview | null }) => void;
  readonly onSettled?: () => void;
};

function makeFile(overrides: Partial<DiffFileType> = {}): DiffFileType {
  return {
    path: "src/foo.ts",
    old_path: "src/foo.ts",
    status: "modified",
    is_binary: false,
    is_whitespace_only: false,
    additions: 3,
    deletions: 1,
    patch: `diff --git a/src/foo.ts b/src/foo.ts
--- a/src/foo.ts
+++ b/src/foo.ts
@@ -1,3 +1,5 @@
 line 1
-old line
+new line
`,
    hunks: [
      {
        old_start: 1,
        old_count: 3,
        new_start: 1,
        new_count: 5,
        lines: [
          {
            type: "context",
            content: "line 1",
            old_num: 1,
            new_num: 1,
          },
          { type: "delete", content: "old line", old_num: 2 },
          { type: "add", content: "new line", new_num: 2 },
        ],
      },
    ],
    ...overrides,
  };
}

function makeTwoAdditionFile(): DiffFileType {
  return makeFile({
    additions: 2,
    deletions: 1,
    patch: `diff --git a/src/foo.ts b/src/foo.ts
--- a/src/foo.ts
+++ b/src/foo.ts
@@ -1,2 +1,3 @@
 line 1
-old line
+new line
+newer line
`,
    hunks: [
      {
        old_start: 1,
        old_count: 2,
        new_start: 1,
        new_count: 3,
        lines: [
          {
            type: "context",
            content: "line 1",
            old_num: 1,
            new_num: 1,
          },
          { type: "delete", content: "old line", old_num: 2 },
          { type: "add", content: "new line", new_num: 2 },
          { type: "add", content: "newer line", new_num: 3 },
        ],
      },
    ],
  });
}

function makeAddedConfigFile(): DiffFileType {
  return makeFile({
    path: ".github/ISSUE_TEMPLATE/config.yml",
    old_path: ".github/ISSUE_TEMPLATE/config.yml",
    status: "added",
    additions: 4,
    deletions: 0,
    patch: `diff --git a/.github/ISSUE_TEMPLATE/config.yml b/.github/ISSUE_TEMPLATE/config.yml
--- /dev/null
+++ b/.github/ISSUE_TEMPLATE/config.yml
@@ -0,0 +1,4 @@
+# Route every new issue through one of the templates above.
+# Discussions is enabled for this repo, add a contact_links entry pointing
+# usage/"how do I" questions there instead of the issue tracker.
+blank_issues_enabled: false
`,
    hunks: [
      {
        old_start: 0,
        old_count: 0,
        new_start: 1,
        new_count: 4,
        lines: [
          { type: "add", content: "# Route every new issue through one of the templates above.", new_num: 1 },
          {
            type: "add",
            content: "# Discussions is enabled for this repo, add a contact_links entry pointing",
            new_num: 2,
          },
          {
            type: "add",
            content: '# usage/"how do I" questions there instead of the issue tracker.',
            new_num: 3,
          },
          { type: "add", content: "blank_issues_enabled: false", new_num: 4 },
        ],
      },
    ],
  });
}

function makeReviewThread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: "thread-1",
    provider_comment_id: "comment-1",
    path: "src/foo.ts",
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

// Use unique owner per test so module-level collapsed state doesn't leak.
let testCounter = 0;
function uniqueOwner(): string {
  return `test-owner-${++testCounter}`;
}

function renderDiffFile(
  file: DiffFileType,
  options: {
    richPreview?: boolean;
    richPreviewEnabled?: boolean;
    contextExpansionEnabled?: boolean;
    reviewEnabled?: boolean;
    diffHeadSHA?: string;
    nativeMultilineRanges?: boolean;
    owner?: string;
    draftComments?: DiffReviewDraftComment[];
    reviewThreads?: ReviewThread[];
    createComment?: (body: string, range: DiffReviewLineRange) => Promise<boolean>;
    canReplyToThreads?: boolean;
    viewMode?: "unified" | "split";
    loadFilePreview?: LoadFilePreview;
    replyToDiscussion?: (
      owner: string,
      name: string,
      number: number,
      discussionID: string,
      body: string,
      callbacks: MutationCallbacks,
    ) => void;
  } = {},
) {
  const diff = createDiffStore({ runtime });
  if (options.richPreview) diff.setRichPreview(true);
  if (options.viewMode) diff.setViewMode(options.viewMode);
  if (options.loadFilePreview) vi.spyOn(diff, "loadFilePreview").mockImplementation(options.loadFilePreview);
  const diffReviewDraft = {
    getComments: () => options.draftComments ?? [],
    isSubmitting: () => false,
    getError: () => null,
    createComment: options.createComment ?? (() => Promise.resolve(true)),
    deleteComment: () => Promise.resolve(true),
    editComment: () => Promise.resolve(true),
    setCommentEditState: vi.fn(),
  };
  const owner = options.owner ?? uniqueOwner();
  const result = render(DiffFile, {
    props: {
      runtime,
      file,
      provider: "github",
      platformHost: "github.com",
      owner,
      name: "n",
      repoPath: `${owner}/n`,
      number: 1,
      ...(options.richPreviewEnabled !== undefined && {
        richPreviewEnabled: options.richPreviewEnabled,
      }),
      ...(options.contextExpansionEnabled !== undefined && {
        contextExpansionEnabled: options.contextExpansionEnabled,
      }),
      ...(options.reviewEnabled !== undefined && {
        reviewEnabled: options.reviewEnabled,
      }),
      ...(options.canReplyToThreads !== undefined && {
        canReplyToThreads: options.canReplyToThreads,
      }),
      ...(options.diffHeadSHA !== undefined && {
        diffHeadSHA: options.diffHeadSHA,
      }),
      ...(options.nativeMultilineRanges !== undefined && {
        nativeMultilineRanges: options.nativeMultilineRanges,
      }),
      ...(options.reviewThreads !== undefined && {
        reviewThreads: options.reviewThreads,
      }),
    },
    context: new Map([
      [
        STORES_KEY,
        {
          diff,
          diffReviewDraft,
          detail: {
            replyToDiscussion:
              options.replyToDiscussion ??
              ((_owner, _name, _number, _discussionID, _body, callbacks) => {
                callbacks.onSuccess?.();
                callbacks.onSettled?.();
              }),
          },
        },
      ],
    ]),
  });
  return { ...result, diff };
}

function textPreview(path: string, text: string): FilePreview {
  return {
    path,
    media_type: "text/plain; charset=utf-8",
    encoding: "base64",
    content: Buffer.from(text, "utf8").toString("base64"),
    size: text.length,
  };
}

describe("DiffFile", () => {
  afterEach(() => {
    cleanup();
    localStorage.removeItem("diff-rich-preview");
    localStorage.removeItem("diff-view-mode");
  });

  async function expectPierreDiffText(pattern: RegExp): Promise<void> {
    await waitFor(() => {
      const host = document.querySelector(".pierre-diff");
      expect(host?.shadowRoot?.textContent).toMatch(pattern);
    });
  }

  it("renders file content when not collapsed", async () => {
    renderDiffFile(makeFile());

    expect(screen.getByText("src/foo.ts")).toBeTruthy();
    await expectPierreDiffText(/old linenew line/);
  });

  it("copies the exact title-bar file path from a dedicated copy button", async () => {
    const path = "PROMPT_COMMAND+=src/payload;$(hidden-command).ts";
    const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });

    try {
      renderDiffFile(makeFile({ path, old_path: path }));

      const pathLabel = screen.getByText(path);
      expect(pathLabel.tagName).toBe("SPAN");
      await fireEvent.click(screen.getByRole("button", { name: `Copy file path ${path}` }));

      expect(writeText).toHaveBeenCalledWith(path);
      expect(document.querySelector(".file-content")).toBeTruthy();
    } finally {
      if (originalClipboard) {
        Object.defineProperty(navigator, "clipboard", originalClipboard);
      } else {
        delete (navigator as Navigator & { clipboard?: Clipboard }).clipboard;
      }
    }
  });

  it("exposes stable line targets inside the Pierre shadow root", async () => {
    renderDiffFile(makeFile());

    await waitFor(() => {
      const root = document.querySelector(".pierre-diff")?.shadowRoot;
      expect(root?.querySelector('[data-diff-path="src/foo.ts"][data-diff-old-line="2"]')).toBeTruthy();
      expect(root?.querySelector('[data-diff-path="src/foo.ts"][data-diff-new-line="2"]')).toBeTruthy();
    });
  });

  it("shows a loading state before viewport-gated Pierre rendering starts", async () => {
    const visibleObserver = (globalThis as GlobalWithIO).IntersectionObserver;
    class PendingIntersectionObserverStub {
      root: Element | null = null;
      rootMargin = "";
      thresholds: readonly number[] = [];
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
      takeRecords(): IntersectionObserverEntry[] {
        return [];
      }
    }
    (globalThis as GlobalWithIO).IntersectionObserver = PendingIntersectionObserverStub;

    try {
      renderDiffFile(makeFile());

      expect(screen.getByRole("status", { name: "Loading diff" })).toBeTruthy();
      expect(document.querySelector(".pierre-diff--pending")).toBeTruthy();
    } finally {
      (globalThis as GlobalWithIO).IntersectionObserver = visibleObserver;
    }
  });

  it("renders an empty textual diff state without staying stuck loading", async () => {
    renderDiffFile(
      makeFile({
        additions: 0,
        deletions: 0,
        patch: "",
        hunks: [],
      }),
    );

    await waitFor(() => {
      expect(screen.queryByRole("status")).toBeNull();
      expect(screen.getByText("No textual changes")).toBeTruthy();
    });
  });

  it("treats nullable hunk payloads as hunkless diffs", async () => {
    renderDiffFile(
      makeFile({
        additions: 0,
        deletions: 0,
        patch: "",
        hunks: null as unknown as DiffFileType["hunks"],
      }),
    );

    await waitFor(() => {
      expect(screen.queryByRole("status")).toBeNull();
      expect(screen.getByText("No textual changes")).toBeTruthy();
    });
  });

  it("shows unified diff content when rich preview is disabled", async () => {
    renderDiffFile(makeFile({ path: "README.md", old_path: "README.md" }), {
      richPreview: true,
      richPreviewEnabled: false,
    });

    expect(screen.queryByLabelText("Before markdown preview")).toBeNull();
    await expectPierreDiffText(/old linenew line/);
  });

  it("renders markdown rich preview as a unified annotated document by default", async () => {
    renderDiffFile(makeFile({ path: "README.md", old_path: "README.md" }), {
      richPreview: true,
    });

    await waitFor(() => {
      expect(screen.queryByLabelText("Before markdown preview")).toBeNull();
      expect(screen.queryByLabelText("After markdown preview")).toBeNull();
      expect(document.querySelector(".markdown-rich-diff--unified del")?.textContent).toBe("old");
      expect(document.querySelector(".markdown-rich-diff--unified ins")?.textContent).toBe("new");
    });
  });

  it("does not classify inactive markdown review threads as unavailable", async () => {
    const visibleObserver = (globalThis as GlobalWithIO).IntersectionObserver;
    class InactiveIntersectionObserverStub {
      root: Element | null = null;
      rootMargin = "";
      thresholds: readonly number[] = [];
      constructor(private readonly callback: IntersectionObserverCallback) {}
      observe(target: Element): void {
        const entry = {
          isIntersecting: false,
          intersectionRatio: 0,
          target,
          boundingClientRect: {} as DOMRectReadOnly,
          intersectionRect: {} as DOMRectReadOnly,
          rootBounds: null,
          time: 0,
        } as IntersectionObserverEntry;
        this.callback([entry], this as unknown as IntersectionObserver);
      }
      unobserve(): void {}
      disconnect(): void {}
      takeRecords(): IntersectionObserverEntry[] {
        return [];
      }
    }
    (globalThis as GlobalWithIO).IntersectionObserver = InactiveIntersectionObserverStub;

    try {
      renderDiffFile(makeFile({ path: "README.md", old_path: "README.md" }), {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [makeReviewThread({ path: "README.md", diff_head_sha: "diff-head" })],
      });

      await waitFor(() => {
        expect(screen.getByText("Loading preview")).toBeTruthy();
      });
      expect(screen.queryByText("Published review note")).toBeNull();
      expect(screen.queryByText("Line unavailable")).toBeNull();
    } finally {
      (globalThis as GlobalWithIO).IntersectionObserver = visibleObserver;
    }
  });

  it("keeps structural markdown rich preview changes visibly highlighted", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 2,
            new_start: 1,
            new_count: 2,
            lines: [
              { type: "delete", content: "- Removed", old_num: 1 },
              { type: "context", content: "- Keep", old_num: 2, new_num: 1 },
              { type: "add", content: "- Added", new_num: 2 },
            ],
          },
        ],
      }),
      { richPreview: true },
    );

    await waitFor(() => {
      const preview = document.querySelector(".markdown-rich-diff--unified");
      expect(preview?.querySelector('li.markdown-diff__structural[data-diff-kind="delete"] > del')?.textContent).toBe(
        "Removed",
      );
      expect(preview?.querySelector('li.markdown-diff__structural[data-diff-kind="insert"] > ins')?.textContent).toBe(
        "Added",
      );
    });
    expect(diffRichPreviewSource).toMatch(
      /\.markdown-diff__structural\[data-diff-kind="delete"\][\s\S]*background: var\(--diff-del-bg\);/,
    );
    expect(diffRichPreviewSource).toMatch(
      /\.markdown-diff__structural\[data-diff-kind="insert"\][\s\S]*background: var\(--diff-add-bg\);/,
    );
  });

  it("keeps markdown rich preview side by side when split diff mode is enabled", async () => {
    renderDiffFile(makeFile({ path: "README.md", old_path: "README.md" }), {
      richPreview: true,
      viewMode: "split",
    });

    await waitFor(() => {
      const beforePreview = document.querySelector('[data-markdown-rich-side="before"]');
      const afterPreview = document.querySelector('[data-markdown-rich-side="after"]');

      expect(beforePreview?.querySelector("del")?.textContent).toBe("old");
      expect(beforePreview?.querySelector("ins")).toBeNull();
      expect(afterPreview?.querySelector("del")).toBeNull();
      expect(afterPreview?.querySelector("ins")?.textContent).toBe("new");
    });
    expect(diffRichPreviewSource).toMatch(
      /@media \(max-width: 760px\)[\s\S]*\.markdown-rich-diff__split-header,\s*\.markdown-rich-diff__split-row[\s\S]*grid-template-columns: minmax\(0, 1fr\);/,
    );
  });

  it("keeps non-Markdown rich preview review cards visible while loading", async () => {
    let resolvePreview: (preview: FilePreview) => void = () => {};
    renderDiffFile(makeFile({ path: "assets/chart.png", old_path: "assets/chart.png" }), {
      richPreview: true,
      diffHeadSHA: "diff-head",
      loadFilePreview: (_owner, _name, _number, _path, _side, callbacks) => {
        resolvePreview = (preview) => {
          callbacks?.onSuccess?.(preview);
          callbacks?.onSettled?.();
        };
      },
      reviewThreads: [
        makeReviewThread({
          path: "assets/chart.png",
          old_path: "assets/chart.png",
          body: "Loading non-markdown review note",
          diff_head_sha: "diff-head",
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Loading preview")).toBeTruthy();
    });
    expect(screen.getByText("Loading non-markdown review note")).toBeTruthy();

    resolvePreview(textPreview("assets/chart.png", "preview content"));
    await waitFor(() => {
      expect(screen.getByText("preview content")).toBeTruthy();
    });
  });

  it("keeps non-Markdown rich preview review cards visible after preview errors", async () => {
    renderDiffFile(makeFile({ path: "assets/chart.png", old_path: "assets/chart.png" }), {
      richPreview: true,
      diffHeadSHA: "diff-head",
      loadFilePreview: (_owner, _name, _number, _path, _side, callbacks) => {
        callbacks?.onFailure?.("preview failed");
        callbacks?.onSettled?.();
      },
      reviewThreads: [
        makeReviewThread({
          path: "assets/chart.png",
          old_path: "assets/chart.png",
          body: "Error non-markdown review note",
          diff_head_sha: "diff-head",
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("preview failed")).toBeTruthy();
    });
    expect(screen.getByText("Error non-markdown review note")).toBeTruthy();
  });

  it("hides content after clicking the header to collapse", async () => {
    renderDiffFile(makeFile());

    const header = screen.getByTitle("Collapse file");
    expect(header.getAttribute("aria-expanded")).toBe("true");
    await fireEvent.click(header);

    expect(document.querySelector(".file-content")).toBeNull();
    expect(header.getAttribute("aria-expanded")).toBe("false");
  });

  it("shows content again after toggling collapse twice", async () => {
    renderDiffFile(makeFile());

    const header = screen.getByTitle("Collapse file");
    await fireEvent.click(header);

    const expandHeader = screen.getByTitle("Expand file");
    await fireEvent.click(expandHeader);

    const content = document.querySelector(".file-content");
    expect(content?.classList.contains("file-content--collapsed")).toBe(false);
  });

  async function selectPierreLine(
    line: number,
    side: "left" | "right",
    options: { shiftKey?: boolean } = {},
  ): Promise<void> {
    const lineNumber = await findPierreLineNumber(line, side);
    await fireEvent.pointerDown(lineNumber, {
      button: 0,
      pointerId: 1,
      pointerType: "mouse",
      shiftKey: options.shiftKey,
    });
    await fireEvent.pointerUp(lineNumber, {
      button: 0,
      pointerId: 1,
      pointerType: "mouse",
      shiftKey: options.shiftKey,
    });
  }

  async function clickLineCommentButton(
    line: number,
    side: "left" | "right",
    options: { shiftKey?: boolean } = {},
  ): Promise<void> {
    let button = findGutterUtilityButton();
    const utilityLine = button?.closest<HTMLElement>("[data-column-number]");
    if (
      !utilityLine ||
      utilityLine.getAttribute("data-column-number") !== String(line) ||
      !utilityLine.hasAttribute(side === "left" ? "data-diff-old-line" : "data-diff-new-line")
    ) {
      await selectPierreLine(line, side, options);
      button = await waitFor(() => {
        const element = findGutterUtilityButton();
        expect(element).toBeTruthy();
        return element!;
      });
    }
    await fireEvent.pointerDown(button!, {
      button: 0,
      pointerId: 2,
      pointerType: "mouse",
      shiftKey: options.shiftKey,
    });
    await fireEvent.pointerUp(button!, {
      button: 0,
      pointerId: 2,
      pointerType: "mouse",
      shiftKey: options.shiftKey,
    });
  }

  async function keyboardActivateLineCommentButton(line: number, side: "left" | "right"): Promise<void> {
    const button = await findLineCommentButton(line, side);
    button.focus();
    expect(await fireEvent.keyDown(button, { key: "Enter", code: "Enter" })).toBe(true);
    await fireEvent.click(button, { detail: 0 });
    await fireEvent.keyUp(button, { key: "Enter", code: "Enter" });
  }

  async function findLineCommentButton(line: number, side: "left" | "right"): Promise<HTMLButtonElement> {
    return await waitFor(() => {
      const element = findGutterUtilityButton();
      const utilityLine = element?.closest<HTMLElement>("[data-column-number]");
      expect(element).toBeTruthy();
      expect(utilityLine?.getAttribute("data-column-number")).toBe(String(line));
      expect(utilityLine?.hasAttribute(side === "left" ? "data-diff-old-line" : "data-diff-new-line")).toBe(true);
      return element!;
    });
  }

  function findGutterUtilityButton(): HTMLButtonElement | null | undefined {
    return document
      .querySelector(".pierre-diff")
      ?.shadowRoot?.querySelector<HTMLButtonElement>("[data-utility-button]");
  }

  async function findPierreLineNumber(line: number, side: "left" | "right"): Promise<HTMLElement> {
    return await waitFor(() => {
      const sideAttribute = side === "left" ? "data-diff-old-line" : "data-diff-new-line";
      const elements =
        document
          .querySelector(".pierre-diff")
          ?.shadowRoot?.querySelectorAll<HTMLElement>(`[data-column-number="${line}"][${sideAttribute}="${line}"]`) ??
        [];
      const element = elements[0];
      expect(element).toBeTruthy();
      return element!;
    });
  }

  async function findPierreLineKeyboardTarget(line: number, side: "left" | "right"): Promise<HTMLElement> {
    const lineNumber = await findPierreLineNumber(line, side);
    const target = lineNumber.querySelector<HTMLElement>("[data-line-number-content]");
    expect(target).toBeTruthy();
    return target!;
  }

  async function keyboardSelectPierreLine(
    line: number,
    side: "left" | "right",
    options: { shiftKey?: boolean } = {},
  ): Promise<void> {
    const target = await findPierreLineKeyboardTarget(line, side);
    target.focus();
    expect(
      await fireEvent.keyDown(target, {
        key: "Enter",
        code: "Enter",
        shiftKey: options.shiftKey,
      }),
    ).toBe(false);
    await fireEvent.keyUp(target, {
      key: "Enter",
      code: "Enter",
      shiftKey: options.shiftKey,
    });
  }

  function selectedPierreLines(): NodeListOf<Element> | undefined {
    return document.querySelector(".pierre-diff")?.shadowRoot?.querySelectorAll("[data-selected-line]");
  }

  function splitColumnSide(element: Element | null | undefined): "additions" | "deletions" | null {
    const code = element?.closest("code");
    return code ? (renderedCodeSide(code) ?? null) : null;
  }

  function expandedContextLineTexts(): string[] {
    return Array.from(
      document
        .querySelector(".pierre-diff")
        ?.shadowRoot?.querySelectorAll<HTMLElement>("[data-content] [data-line-type='context-expanded']") ?? [],
    ).map((line) => line.textContent?.trim() ?? "");
  }

  it("allows shift-selecting ranges only when native multiline ranges are supported", async () => {
    const { unmount } = renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      nativeMultilineRanges: true,
    });

    await selectPierreLine(1, "right");
    await selectPierreLine(2, "right", { shiftKey: true });

    await waitFor(() => {
      expect(selectedPierreLines()?.length).toBeGreaterThanOrEqual(2);
    });
    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();

    await clickLineCommentButton(2, "right");
    expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();

    unmount();
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      nativeMultilineRanges: false,
    });

    await selectPierreLine(1, "right");
    await selectPierreLine(2, "right", { shiftKey: true });

    await waitFor(() => {
      expect(selectedPierreLines()).toHaveLength(2);
      expect(
        Array.from(selectedPierreLines() ?? []).every((line) => line.getAttribute("data-diff-new-line") === "2"),
      ).toBe(true);
    });
  });

  it("toggles an empty inline composer from the line comment button", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
    });

    await clickLineCommentButton(2, "right");
    const textarea = screen.getByPlaceholderText("Leave a comment");
    expect(textarea).toBeTruthy();
    await waitFor(() => {
      expect(textarea).toBe(document.activeElement);
    });
    await waitFor(() => {
      expect(selectedPierreLines()).toHaveLength(4);
    });

    await clickLineCommentButton(2, "right");

    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(selectedPierreLines()).toHaveLength(0);
  });

  it("keeps the final line visible when opening an inline composer", async () => {
    renderDiffFile(makeAddedConfigFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
    });

    await clickLineCommentButton(4, "right");
    await waitFor(() => expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy());

    const root = document.querySelector(".pierre-diff")?.shadowRoot;
    const lineTargets = Array.from(root?.querySelectorAll<HTMLElement>('[data-diff-new-line="4"]') ?? []);
    expect(lineTargets.some((line) => line.textContent?.includes("blank_issues_enabled: false"))).toBe(true);

    await waitFor(() => {
      const slot = root?.querySelector<HTMLSlotElement>('slot[name="annotation-additions-4"]');
      expect(slot).toBeTruthy();
      expect(slot?.closest('[data-diff-new-line="4"]')).toBeNull();
    });
  });

  it("keeps right-side inline composers in the additions column in split mode", async () => {
    renderDiffFile(makeTwoAdditionFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      viewMode: "split",
    });

    async function assertRightSideComposer(line: number): Promise<void> {
      await clickLineCommentButton(line, "right");

      await waitFor(() => {
        expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
        const button = findGutterUtilityButton();
        expect(splitColumnSide(button)).toBe("additions");
        const slot = document
          .querySelector(".pierre-diff")
          ?.shadowRoot?.querySelector<HTMLSlotElement>(`slot[name="annotation-additions-${line}"]`);
        expect(splitColumnSide(slot)).toBe("additions");
      });

      await fireEvent.click(screen.getByText("Cancel"));
      await waitFor(() => expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull());
    }

    await assertRightSideComposer(2);
    await assertRightSideComposer(3);
  });

  it("infers split column side from code-column order when non-code siblings are present", () => {
    const pre = document.createElement("pre");
    const deletions = document.createElement("code");
    const separator = document.createElement("span");
    const additions = document.createElement("code");
    pre.append(deletions, separator, additions);

    expect(renderedCodeSide(deletions)).toBe("deletions");
    expect(renderedCodeSide(additions)).toBe("additions");
  });

  it("focuses the inline composer textarea exactly once after opening", async () => {
    // Regression guard for issues #445/#446: repeated focus retries (extra
    // frames or timers) made the composer's visible focus indicator flicker.
    // The composer must mutate textarea focus a single time.
    const focusSpy = vi.spyOn(HTMLTextAreaElement.prototype, "focus");
    try {
      renderDiffFile(makeFile(), {
        reviewEnabled: true,
        diffHeadSHA: "diff-head",
      });

      await clickLineCommentButton(2, "right");
      const textarea = screen.getByPlaceholderText("Leave a comment");
      await waitFor(() => {
        expect(textarea).toBe(document.activeElement);
      });

      // Cover the windows the old retry implementation used: two animation
      // frames plus a 50ms timer after the initial focus.
      await new Promise((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve(undefined)));
      });
      await new Promise((resolve) => setTimeout(resolve, 80));

      expect(focusSpy).toHaveBeenCalledTimes(1);
      expect(textarea).toBe(document.activeElement);
    } finally {
      focusSpy.mockRestore();
    }
  });

  it("toggles an empty inline composer from keyboard line comment button activation", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
    });

    await clickLineCommentButton(2, "right");
    expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
    await waitFor(() => {
      expect(selectedPierreLines()).toHaveLength(4);
    });

    await keyboardActivateLineCommentButton(2, "right");

    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(selectedPierreLines()).toHaveLength(0);
  });

  it("opens a multiline inline composer without pointer interaction", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      nativeMultilineRanges: true,
    });

    const lineNumber = await findPierreLineNumber(1, "right");
    const lineNumberTarget = await findPierreLineKeyboardTarget(1, "right");
    expect(lineNumber.tabIndex).toBe(-1);
    expect(lineNumber.getAttribute("role")).toBeNull();
    expect(lineNumberTarget.tabIndex).toBe(0);
    expect(lineNumberTarget.getAttribute("role")).toBe("button");

    await keyboardSelectPierreLine(1, "right");
    await keyboardSelectPierreLine(2, "right", { shiftKey: true });

    const selectedNewLineNumbers = () =>
      new Set(
        Array.from(selectedPierreLines() ?? [], (line) => line.getAttribute("data-diff-new-line")).filter(
          (line): line is string => line !== null,
        ),
      );

    await findLineCommentButton(2, "right");
    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(selectedNewLineNumbers()).toEqual(new Set(["1", "2"]));

    await keyboardActivateLineCommentButton(2, "right");

    expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
    expect(selectedNewLineNumbers()).toEqual(new Set(["1", "2"]));
  });

  it("toggles an active multiline composer from keyboard line comment button activation", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      nativeMultilineRanges: true,
    });

    await selectPierreLine(1, "right");
    await selectPierreLine(2, "right", { shiftKey: true });
    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();

    await clickLineCommentButton(2, "right");

    await waitFor(() => {
      expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
      expect(selectedPierreLines()?.length).toBeGreaterThanOrEqual(2);
    });

    await keyboardActivateLineCommentButton(2, "right");

    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(selectedPierreLines()).toHaveLength(0);
  });

  it("does not create multiline review ranges across separate hunks", async () => {
    renderDiffFile(
      makeFile({
        additions: 2,
        deletions: 0,
        patch: `diff --git a/src/foo.ts b/src/foo.ts
--- a/src/foo.ts
+++ b/src/foo.ts
@@ -1,0 +1,1 @@
+first hunk
@@ -20,0 +20,1 @@
+second hunk
`,
        hunks: [
          {
            old_start: 1,
            old_count: 1,
            new_start: 1,
            new_count: 1,
            lines: [{ type: "add", content: "first hunk", new_num: 1 }],
          },
          {
            old_start: 20,
            old_count: 1,
            new_start: 20,
            new_count: 1,
            lines: [{ type: "add", content: "second hunk", new_num: 20 }],
          },
        ],
      }),
      {
        reviewEnabled: true,
        diffHeadSHA: "diff-head",
        nativeMultilineRanges: true,
      },
    );

    await selectPierreLine(1, "right");
    await selectPierreLine(20, "right", { shiftKey: true });
    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();

    await clickLineCommentButton(20, "right");

    await waitFor(() => {
      expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
    });
    expect(selectedPierreLines()).toHaveLength(4);
  });

  it("renders saved draft comments inline at their selected range", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      draftComments: [
        {
          id: "draft-1",
          body: "Follow up here",
          path: "src/foo.ts",
          side: "right",
          start_side: "right",
          start_line: 1,
          line: 2,
          new_line: 2,
          line_type: "add",
          diff_head_sha: "diff-head",
          created_at: "2026-03-30T14:01:00Z",
          updated_at: "2026-03-30T14:01:00Z",
        },
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Follow up here")).toBeTruthy();
      expect(selectedPierreLines()?.length).toBeGreaterThanOrEqual(4);
    });
  });

  it("does not restore a saved draft range from an older diff snapshot", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "current-diff-head",
      draftComments: [
        {
          id: "draft-stale",
          body: "Stale draft range",
          path: "src/foo.ts",
          side: "right",
          line: 2,
          new_line: 2,
          line_type: "add",
          diff_head_sha: "older-diff-head",
          created_at: "2026-03-30T14:01:00Z",
          updated_at: "2026-03-30T14:01:00Z",
        },
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Stale draft range")).toBeTruthy();
    });
    expect(selectedPierreLines()).toHaveLength(0);
  });

  it("opens a new inline composer on a line that already has a saved draft", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      draftComments: [
        {
          id: "draft-existing",
          body: "Existing draft on this line",
          path: "src/foo.ts",
          side: "right",
          line: 2,
          new_line: 2,
          line_type: "add",
          diff_head_sha: "diff-head",
          created_at: "2026-03-30T14:01:00Z",
          updated_at: "2026-03-30T14:01:00Z",
        },
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Existing draft on this line")).toBeTruthy();
      const selectedDraftLine = document
        .querySelector(".pierre-diff")
        ?.shadowRoot?.querySelector('[data-selected-line][data-diff-new-line="2"]');
      expect(selectedDraftLine).toBeTruthy();
    });

    await clickLineCommentButton(2, "right");

    await waitFor(() => {
      expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
    });
    expect(screen.getByText("Existing draft on this line")).toBeTruthy();
  });

  it("renders published review threads under their matching diff line", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      reviewThreads: [makeReviewThread()],
    });

    await waitFor(() => {
      expect(screen.getByText("Published review note")).toBeTruthy();
    });
    const host = document
      .querySelector("[data-review-thread-id='thread-1']")
      ?.closest("[slot='annotation-additions-2']");
    expect(host).toBeTruthy();
  });

  it("releases a mounted review thread when the diff surface unmounts", async () => {
    const view = renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      reviewThreads: [makeReviewThread()],
    });

    await waitFor(() => {
      expect(screen.getByText("Published review note")).toBeTruthy();
      expect(
        resizeObserverInstances.some((observer) =>
          observer.observed.some((target) => target.classList.contains("inline-review-thread")),
        ),
      ).toBe(true);
    });
    const threadObservers = resizeObserverInstances.filter((observer) =>
      observer.observed.some((target) => target.classList.contains("inline-review-thread")),
    );
    expect(threadObservers.every((observer) => !observer.disconnected)).toBe(true);

    view.unmount();

    await waitFor(() => {
      expect(threadObservers.every((observer) => observer.disconnected)).toBe(true);
    });
  });

  it("renders published review thread cards in markdown rich preview", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        canReplyToThreads: true,
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            body: "Published review note in rich preview",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(document.querySelector(".markdown-rich-diff--unified")).toBeTruthy();
    });
    expect(screen.getByText("Published review note in rich preview")).toBeTruthy();
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.closest("[slot^='annotation-']")).toBeNull();
    expect(comment?.closest(".markdown-rich-diff--unified")).toBeTruthy();
  });

  it("keeps markdown rich preview review threads visible when hunk lines are null", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        additions: 0,
        deletions: 0,
        patch: "",
        hunks: [
          {
            old_start: 1,
            old_count: 0,
            new_start: 1,
            new_count: 0,
            lines: null as unknown as DiffFileType["hunks"][number]["lines"],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 1,
            new_line: 1,
            body: "Nullable rich preview note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Nullable rich preview note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.closest(".markdown-rich-diff--unified")).toBeNull();
    expect(comment?.classList.contains("inline-review-thread--file-level")).toBe(true);
  });

  it("orders markdown rich preview review cards by their target source line", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 4,
            new_start: 1,
            new_count: 4,
            lines: [
              { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
              { type: "context", content: "- Actions", old_num: 2, new_num: 2 },
              { type: "context", content: "- Statuses", old_num: 3, new_num: 3 },
              { type: "context", content: "- Members", old_num: 4, new_num: 4 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-actions",
            provider_comment_id: "comment-actions",
            path: "README.md",
            old_path: "README.md",
            line: 2,
            new_line: 2,
            body: "Actions review note",
            diff_head_sha: "diff-head",
          }),
          makeReviewThread({
            id: "thread-issues",
            provider_comment_id: "comment-issues",
            path: "README.md",
            old_path: "README.md",
            line: 1,
            new_line: 1,
            body: "Issues review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Actions review note")).toBeTruthy();
      expect(screen.getByText("Issues review note")).toBeTruthy();
    });
    const reviewBodies = Array.from(
      document.querySelectorAll<HTMLElement>(".markdown-rich-diff--unified .review-thread-body"),
    ).map((element) => element.textContent?.trim());
    expect(reviewBodies).toEqual(["Issues review note", "Actions review note"]);
    const issuesComment = document.querySelector("[data-review-thread-id='thread-issues']");
    const actionsComment = document.querySelector("[data-review-thread-id='thread-actions']");
    const splitLists = Array.from(document.querySelectorAll<HTMLElement>("ul.markdown-rich-diff__split-list"));
    expect(splitLists).toHaveLength(3);
    expect(splitLists.at(-1)?.textContent).toContain("Statuses");
    expect(splitLists.at(-1)?.textContent).toContain("Members");
    expect(issuesComment?.previousElementSibling?.textContent).toContain("Issues");
    expect(issuesComment?.previousElementSibling?.textContent).not.toContain("Actions");
    expect(actionsComment?.previousElementSibling?.textContent).toContain("Actions");
    expect(actionsComment?.previousElementSibling?.textContent).not.toContain("Statuses");
  });

  it("keeps unchanged list siblings aligned when a review targets an added list item", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 2,
            new_start: 1,
            new_count: 3,
            lines: [
              { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
              { type: "add", content: "- Actions", new_num: 2 },
              { type: "context", content: "- Statuses", old_num: 2, new_num: 3 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-actions",
            provider_comment_id: "comment-actions",
            path: "README.md",
            old_path: "README.md",
            line: 2,
            new_line: 2,
            body: "Actions review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Actions review note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(preview?.querySelector("ins")?.textContent).toContain("Actions");
    expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Statuses");
    expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Issues");
    const actionsComment = document.querySelector("[data-review-thread-id='thread-actions']");
    expect(actionsComment?.previousElementSibling?.textContent).toContain("Actions");
    expect(actionsComment?.previousElementSibling?.textContent).not.toContain("Statuses");
  });

  it("keeps unchanged list siblings aligned when added list item review targets an edge", async () => {
    for (const scenario of [
      {
        name: "prepended",
        targetLine: 1,
        oldCount: 2,
        newCount: 3,
        lines: [
          { type: "add" as const, content: "- Actions", new_num: 1 },
          { type: "context" as const, content: "- Issues", old_num: 1, new_num: 2 },
          { type: "context" as const, content: "- Statuses", old_num: 2, new_num: 3 },
        ],
      },
      {
        name: "prepended continuation",
        targetLine: 2,
        oldCount: 2,
        newCount: 4,
        lines: [
          { type: "add" as const, content: "- Actions", new_num: 1 },
          { type: "add" as const, content: "still needs details", new_num: 2 },
          { type: "context" as const, content: "- Issues", old_num: 1, new_num: 3 },
          { type: "context" as const, content: "- Statuses", old_num: 2, new_num: 4 },
        ],
      },
      {
        name: "appended",
        targetLine: 3,
        oldCount: 2,
        newCount: 3,
        lines: [
          { type: "context" as const, content: "- Issues", old_num: 1, new_num: 1 },
          { type: "context" as const, content: "- Statuses", old_num: 2, new_num: 2 },
          { type: "add" as const, content: "- Actions", new_num: 3 },
        ],
      },
    ]) {
      cleanup();
      renderDiffFile(
        makeFile({
          path: "README.md",
          old_path: "README.md",
          hunks: [
            {
              old_start: 1,
              old_count: scenario.oldCount,
              new_start: 1,
              new_count: scenario.newCount,
              lines: scenario.lines,
            },
          ],
        }),
        {
          richPreview: true,
          diffHeadSHA: "diff-head",
          reviewThreads: [
            makeReviewThread({
              id: `thread-actions-${scenario.name}`,
              provider_comment_id: `comment-actions-${scenario.name}`,
              path: "README.md",
              old_path: "README.md",
              line: scenario.targetLine,
              new_line: scenario.targetLine,
              body: `Actions ${scenario.name} review note`,
              diff_head_sha: "diff-head",
            }),
          ],
        },
      );

      await waitFor(() => {
        expect(screen.getByText(`Actions ${scenario.name} review note`)).toBeTruthy();
      });
      const preview = document.querySelector(".markdown-rich-diff--unified");
      expect(preview?.querySelector("ins")?.textContent).toContain("Actions");
      expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Issues");
      expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Statuses");
      expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Issues");
      expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Statuses");
      const actionsComment = document.querySelector(`[data-review-thread-id='thread-actions-${scenario.name}']`);
      expect(actionsComment?.previousElementSibling?.textContent).toContain("Actions");
    }
  });

  it("keeps unchanged list siblings aligned when a review targets a deleted list item", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 3,
            new_start: 1,
            new_count: 2,
            lines: [
              { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
              { type: "delete", content: "- Actions", old_num: 2 },
              { type: "context", content: "- Statuses", old_num: 3, new_num: 2 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-actions",
            provider_comment_id: "comment-actions",
            path: "README.md",
            old_path: "README.md",
            side: "left",
            line: 2,
            old_line: 2,
            new_line: undefined,
            line_type: "delete",
            body: "Actions review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Actions review note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(preview?.querySelector("del")?.textContent).toContain("Actions");
    expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Statuses");
    expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Issues");
    const actionsComment = document.querySelector("[data-review-thread-id='thread-actions']");
    expect(actionsComment?.previousElementSibling?.textContent).toContain("Actions");
    expect(actionsComment?.previousElementSibling?.textContent).not.toContain("Statuses");
  });

  it("keeps unchanged list siblings aligned when deleted list item review targets an edge", async () => {
    for (const scenario of [
      {
        name: "first",
        targetLine: 1,
        oldCount: 3,
        newCount: 2,
        lines: [
          { type: "delete" as const, content: "- Actions", old_num: 1 },
          { type: "context" as const, content: "- Issues", old_num: 2, new_num: 1 },
          { type: "context" as const, content: "- Statuses", old_num: 3, new_num: 2 },
        ],
      },
      {
        name: "first continuation",
        targetLine: 2,
        oldCount: 4,
        newCount: 2,
        lines: [
          { type: "delete" as const, content: "- Actions", old_num: 1 },
          { type: "delete" as const, content: "still needs details", old_num: 2 },
          { type: "context" as const, content: "- Issues", old_num: 3, new_num: 1 },
          { type: "context" as const, content: "- Statuses", old_num: 4, new_num: 2 },
        ],
      },
      {
        name: "last",
        targetLine: 3,
        oldCount: 3,
        newCount: 2,
        lines: [
          { type: "context" as const, content: "- Issues", old_num: 1, new_num: 1 },
          { type: "context" as const, content: "- Statuses", old_num: 2, new_num: 2 },
          { type: "delete" as const, content: "- Actions", old_num: 3 },
        ],
      },
    ]) {
      cleanup();
      renderDiffFile(
        makeFile({
          path: "README.md",
          old_path: "README.md",
          hunks: [
            {
              old_start: 1,
              old_count: scenario.oldCount,
              new_start: 1,
              new_count: scenario.newCount,
              lines: scenario.lines,
            },
          ],
        }),
        {
          richPreview: true,
          diffHeadSHA: "diff-head",
          reviewThreads: [
            makeReviewThread({
              id: `thread-actions-${scenario.name}`,
              provider_comment_id: `comment-actions-${scenario.name}`,
              path: "README.md",
              old_path: "README.md",
              side: "left",
              line: scenario.targetLine,
              old_line: scenario.targetLine,
              new_line: undefined,
              line_type: "delete",
              body: `Actions ${scenario.name} review note`,
              diff_head_sha: "diff-head",
            }),
          ],
        },
      );

      await waitFor(() => {
        expect(screen.getByText(`Actions ${scenario.name} review note`)).toBeTruthy();
      });
      const preview = document.querySelector(".markdown-rich-diff--unified");
      expect(preview?.querySelector("del")?.textContent).toContain("Actions");
      expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Issues");
      expect(preview?.querySelector("del")?.textContent ?? "").not.toContain("Statuses");
      expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Issues");
      expect(preview?.querySelector("ins")?.textContent ?? "").not.toContain("Statuses");
      const actionsComment = document.querySelector(`[data-review-thread-id='thread-actions-${scenario.name}']`);
      expect(actionsComment?.previousElementSibling?.textContent).toContain("Actions");
    }
  });

  it("keeps modified list item pairs aligned when a review targets the added item", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 3,
            new_start: 1,
            new_count: 3,
            lines: [
              { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
              { type: "delete", content: "- Legacy action", old_num: 2 },
              { type: "add", content: "- Updated action", new_num: 2 },
              { type: "context", content: "- Statuses", old_num: 3, new_num: 3 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-new-action",
            provider_comment_id: "comment-new-action",
            path: "README.md",
            old_path: "README.md",
            line: 2,
            new_line: 2,
            body: "Updated action review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Updated action review note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    const deletedText = Array.from(preview?.querySelectorAll("del") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    const insertedText = Array.from(preview?.querySelectorAll("ins") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    expect(deletedText).toContain("Legacy");
    expect(deletedText).not.toContain("Issues");
    expect(deletedText).not.toContain("Statuses");
    expect(insertedText).toContain("Updated");
    expect(insertedText).not.toContain("Issues");
    expect(insertedText).not.toContain("Statuses");
    const actionComment = document.querySelector("[data-review-thread-id='thread-new-action']");
    expect(actionComment?.previousElementSibling?.textContent).toContain("Updated action");
  });

  it("keeps modified list item pairs aligned when a review targets an added continuation", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 4,
            new_start: 1,
            new_count: 4,
            lines: [
              { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
              { type: "delete", content: "- Action", old_num: 2 },
              { type: "delete", content: "legacy details", old_num: 3 },
              { type: "add", content: "- Action", new_num: 2 },
              { type: "add", content: "updated details", new_num: 3 },
              { type: "context", content: "- Statuses", old_num: 4, new_num: 4 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-new-details",
            provider_comment_id: "comment-new-details",
            path: "README.md",
            old_path: "README.md",
            line: 3,
            new_line: 3,
            body: "Updated details review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Updated details review note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    const deletedText = Array.from(preview?.querySelectorAll("del") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    const insertedText = Array.from(preview?.querySelectorAll("ins") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    expect(deletedText).toContain("legacy");
    expect(deletedText).not.toContain("Issues");
    expect(deletedText).not.toContain("Statuses");
    expect(insertedText).toContain("updated");
    expect(insertedText).not.toContain("Issues");
    expect(insertedText).not.toContain("Statuses");
    const detailsComment = document.querySelector("[data-review-thread-id='thread-new-details']");
    expect(detailsComment?.previousElementSibling?.textContent).toContain("updated details");
  });

  it("keeps context-owned list items aligned when a review targets a changed detail line", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 3,
            new_start: 1,
            new_count: 3,
            lines: [
              { type: "context", content: "- Action", old_num: 1, new_num: 1 },
              { type: "delete", content: "legacy details", old_num: 2 },
              { type: "add", content: "updated details", new_num: 2 },
              { type: "context", content: "- Statuses", old_num: 3, new_num: 3 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            id: "thread-context-details",
            provider_comment_id: "comment-context-details",
            path: "README.md",
            old_path: "README.md",
            line: 2,
            new_line: 2,
            body: "Context-owned details review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Context-owned details review note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    const deletedText = Array.from(preview?.querySelectorAll("del") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    const insertedText = Array.from(preview?.querySelectorAll("ins") ?? [])
      .map((element) => element.textContent ?? "")
      .join("\n");
    expect(deletedText).toContain("legacy");
    expect(deletedText).not.toContain("Statuses");
    expect(insertedText).toContain("updated");
    expect(insertedText).not.toContain("Statuses");
    const detailsComment = document.querySelector("[data-review-thread-id='thread-context-details']");
    expect(detailsComment?.previousElementSibling?.textContent).toContain("updated details");
  });

  it("preserves spacing between consecutive markdown rich preview paragraph blocks", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 3,
            new_start: 1,
            new_count: 3,
            lines: [
              { type: "context", content: "First paragraph", old_num: 1, new_num: 1 },
              { type: "context", content: "", old_num: 2, new_num: 2 },
              { type: "context", content: "Second paragraph", old_num: 3, new_num: 3 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
      },
    );

    await waitFor(() => {
      expect(document.querySelector(".markdown-rich-diff--unified")).toBeTruthy();
    });
    const blocks = Array.from(document.querySelectorAll<HTMLElement>(".markdown-rich-diff__anchored-block"));
    expect(blocks).toHaveLength(2);
    expect(blocks[0]!.classList.contains("markdown-rich-diff__anchored-block--spaced")).toBe(true);
  });

  it("keeps markdown document semantics when review cards are anchored in rich preview", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 6,
            new_start: 1,
            new_count: 7,
            lines: [
              { type: "context", content: "- first", old_num: 1, new_num: 1 },
              { type: "context", content: "", old_num: 2, new_num: 2 },
              { type: "context", content: "- second", old_num: 3, new_num: 3 },
              { type: "context", content: "", old_num: 4, new_num: 4 },
              { type: "context", content: "[ref]: https://example.com", old_num: 5, new_num: 5 },
              { type: "context", content: "", old_num: 6, new_num: 6 },
              { type: "add", content: "See [the ref][ref]", new_num: 7 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 7,
            new_line: 7,
            body: "Anchored semantic note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Anchored semantic note")).toBeTruthy();
    });
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(preview?.querySelectorAll("ul")).toHaveLength(1);
    expect(preview?.querySelectorAll("ul.markdown-rich-diff__split-list")).toHaveLength(0);
    expect(preview?.querySelector('a[href="https://example.com"]')?.textContent).toBe("the ref");
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.previousElementSibling?.classList.contains("markdown-rich-diff__anchored-block")).toBe(true);
  });

  it("anchors markdown rich preview review cards in the matching split pane", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 2,
            new_start: 1,
            new_count: 3,
            lines: [
              { type: "context", content: "## Title", old_num: 1, new_num: 1 },
              { type: "context", content: "", old_num: 2, new_num: 2 },
              { type: "add", content: "Added split-pane paragraph", new_num: 3 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        viewMode: "split",
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 3,
            new_line: 3,
            body: "Split pane review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Split pane review note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.closest('[data-markdown-rich-side="before"]')).toBeNull();
    expect(comment?.closest('[data-markdown-rich-side="after"]')).toBeTruthy();
    expect(comment?.previousElementSibling?.classList.contains("markdown-rich-diff__anchored-block")).toBe(true);
    expect(document.querySelector(".preview-shell > .inline-review-thread")).toBeNull();
  });

  it("keeps split rich preview blocks paired after a side-specific review card", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 3,
            new_start: 1,
            new_count: 5,
            lines: [
              { type: "context", content: "Opening paragraph", old_num: 1, new_num: 1 },
              { type: "context", content: "", old_num: 2, new_num: 2 },
              { type: "add", content: "Added reviewed paragraph", new_num: 3 },
              { type: "context", content: "", old_num: 3, new_num: 4 },
              { type: "context", content: "Following paragraph", old_num: 4, new_num: 5 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        viewMode: "split",
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 3,
            new_line: 3,
            body: "Split row review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Split row review note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    const commentRow = comment?.closest(".markdown-rich-diff__split-row");
    expect(commentRow).toBeTruthy();
    expect(commentRow?.querySelector('[data-markdown-rich-side="before"]')).toBeTruthy();
    expect(commentRow?.querySelector('[data-markdown-rich-side="after"]')?.textContent).toContain(
      "Added reviewed paragraph",
    );
    const followingRow = Array.from(document.querySelectorAll(".markdown-rich-diff__split-row")).find((row) =>
      row.textContent?.includes("Following paragraph"),
    );
    expect(followingRow?.querySelector('[data-markdown-rich-side="before"]')?.textContent).toContain(
      "Following paragraph",
    );
    expect(followingRow?.querySelector('[data-markdown-rich-side="after"]')?.textContent).toContain(
      "Following paragraph",
    );
  });

  it("keeps unmapped markdown rich preview threads in line-unavailable fallback cards", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 99,
            new_line: 99,
            body: "Unmapped rich preview note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Unmapped rich preview note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.closest(".markdown-rich-diff--unified")).toBeNull();
    expect(comment?.classList.contains("inline-review-thread--file-level")).toBe(true);
    expect(comment?.textContent).toContain("Line unavailable");
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(preview?.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("does not anchor markdown rich preview threads to hidden source gaps inside spanning blocks", async () => {
    renderDiffFile(
      makeFile({
        path: "README.md",
        old_path: "README.md",
        hunks: [
          {
            old_start: 1,
            old_count: 2,
            new_start: 1,
            new_count: 2,
            lines: [
              { type: "context", content: "```text", old_num: 1, new_num: 1 },
              { type: "context", content: "first hunk code", old_num: 2, new_num: 2 },
            ],
          },
          {
            old_start: 10,
            old_count: 2,
            new_start: 10,
            new_count: 2,
            lines: [
              { type: "context", content: "second hunk code", old_num: 10, new_num: 10 },
              { type: "context", content: "```", old_num: 11, new_num: 11 },
            ],
          },
        ],
      }),
      {
        richPreview: true,
        diffHeadSHA: "diff-head",
        reviewThreads: [
          makeReviewThread({
            path: "README.md",
            old_path: "README.md",
            line: 6,
            new_line: 6,
            body: "Gap line review note",
            diff_head_sha: "diff-head",
          }),
        ],
      },
    );

    await waitFor(() => {
      expect(screen.getByText("Gap line review note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(comment?.closest(".markdown-rich-diff--unified")).toBeNull();
    expect(comment?.classList.contains("inline-review-thread--file-level")).toBe(true);
    expect(comment?.textContent).toContain("Line unavailable");
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(preview?.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps stale markdown rich preview threads after the rendered preview", async () => {
    renderDiffFile(makeFile({ path: "README.md", old_path: "README.md" }), {
      richPreview: true,
      diffHeadSHA: "current-head",
      reviewThreads: [
        makeReviewThread({
          path: "README.md",
          old_path: "README.md",
          diff_head_sha: "stale-head",
          body: "Stale rich preview note",
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Stale rich preview note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    const preview = document.querySelector(".markdown-rich-diff--unified");
    expect(screen.getByText("Outdated")).toBeTruthy();
    expect(comment?.closest(".markdown-rich-diff--unified")).toBeNull();
    expect(preview?.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("lets published inline review threads be replied to", async () => {
    const replyToDiscussion = vi.fn((_owner, _name, _number, _discussionID, _body, callbacks) => {
      callbacks.onSuccess?.();
      callbacks.onSettled?.();
    });
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
      canReplyToThreads: true,
      reviewThreads: [makeReviewThread()],
      replyToDiscussion,
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Reply" }));
    const textarea = screen.getByPlaceholderText("Reply to thread");
    expect(textarea).toBe(document.activeElement);

    await fireEvent.input(textarea, {
      target: { value: "Follow-up reply" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Reply" }));

    await waitFor(() => {
      expect(replyToDiscussion).toHaveBeenCalledWith(
        expect.any(String),
        "n",
        1,
        "thread-1",
        "Follow-up reply",
        expect.objectContaining({ onSettled: expect.any(Function), onSuccess: expect.any(Function) }),
      );
    });
  });

  it("places the reported outdated review comment after the rendered file", async () => {
    const reviewBody =
      "I'm wondering if we should rename this to CompactionJob and make it a union, and the field a list so we can extend it if we need to support different state for different types of compactions, or multiple jobs per compaction. On the other hand we could just keep it simple for now and bump the version when we need/want to handle those things.";
    renderDiffFile(makeFile({ path: "schemas/compactor.fbs", old_path: "schemas/compactor.fbs" }), {
      reviewEnabled: true,
      diffHeadSHA: "3a6aab8fea50618d913847928c49dc4ece5e3cf0",
      reviewThreads: [
        makeReviewThread({
          path: "schemas/compactor.fbs",
          line: 97,
          new_line: 97,
          body: reviewBody,
          diff_head_sha: "0e1da4256897ac5ccf479b3ab7dd410244149f0c",
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText(reviewBody)).toBeTruthy();
    });
    expect(screen.getByText("Outdated")).toBeTruthy();
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    const diff = document.querySelector(".pierre-diff");
    expect(comment?.parentElement?.classList.contains("file-content")).toBe(true);
    expect(comment?.closest("[slot^='annotation-']")).toBeNull();
    expect(diff?.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps a stale review thread detached when its numeric line still exists", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      diffHeadSHA: "current-head",
      reviewThreads: [makeReviewThread({ diff_head_sha: "stale-head" })],
    });

    await waitFor(() => {
      expect(screen.getByText("Published review note")).toBeTruthy();
    });
    expect(screen.getByText("Outdated")).toBeTruthy();
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    const diff = document.querySelector(".pierre-diff");
    expect(comment?.closest("[slot^='annotation-']")).toBeNull();
    expect(diff?.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it.each([
    ["fresh unavailable", "diff-head", "diff-head", 99, "Line unavailable"],
    ["head-unknown thread inline", undefined, "diff-head", 2, ""],
    ["head-unknown diff inline", "diff-head", undefined, 2, ""],
  ] as const)("keeps %s", async (_label, threadHead, diffHead, line, expectedLabel) => {
    renderDiffFile(makeFile(), {
      reviewEnabled: true,
      ...(diffHead === undefined ? {} : { diffHeadSHA: diffHead }),
      reviewThreads: [
        makeReviewThread({
          line,
          new_line: line,
          ...(threadHead === undefined ? {} : { diff_head_sha: threadHead }),
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Published review note")).toBeTruthy();
    });
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    if (expectedLabel) {
      expect(screen.getByText(expectedLabel)).toBeTruthy();
      expect(comment?.closest("[slot^='annotation-']")).toBeNull();
    } else {
      expect(comment?.closest("[slot^='annotation-']")).toBeTruthy();
      expect(screen.queryByText("Line unavailable")).toBeNull();
    }
  });

  it("does not match added-file threads only because old paths are empty", () => {
    renderDiffFile(
      makeFile({
        path: "src/new.ts",
        old_path: "",
        status: "added",
      }),
      {
        reviewEnabled: true,
        reviewThreads: [
          makeReviewThread({
            id: "thread-other-added-file",
            path: "src/other-new.ts",
            old_path: "",
            body: "Wrong added file note",
          }),
        ],
      },
    );

    expect(screen.queryByText("Wrong added file note")).toBeNull();
  });

  it.each([
    ["fresh", "diff-head", "diff-head"],
    ["stale", "diff-head", "other-head"],
    ["head-unknown", undefined, "diff-head"],
  ] as const)("renders %s file-level review threads at the file header", (_label, diffHead, threadHead) => {
    renderDiffFile(
      makeFile({
        patch: `diff --git a/src/foo.ts b/src/foo.ts
--- a/src/foo.ts
+++ b/src/foo.ts
@@ -60,1 +60,1 @@
 visible context
`,
        hunks: [
          {
            old_start: 60,
            old_count: 1,
            new_start: 60,
            new_count: 1,
            lines: [
              {
                type: "context",
                content: "visible context",
                old_num: 60,
                new_num: 60,
              },
            ],
          },
        ],
      }),
      {
        ...(diffHead === undefined ? {} : { diffHeadSHA: diffHead }),
        reviewThreads: [
          makeReviewThread({
            id: "thread-file",
            line: 1,
            new_line: 1,
            line_type: "file",
            body: "File-level note",
            diff_head_sha: threadHead,
          }),
        ],
      },
    );

    expect(screen.getByText("File-level note")).toBeTruthy();
    expect(screen.getByText("File")).toBeTruthy();
    const comment = document.querySelector("[data-review-thread-id='thread-file']");
    const diff = document.querySelector(".pierre-diff");
    expect(comment?.parentElement?.classList.contains("file-content")).toBe(true);
    expect(comment?.compareDocumentPosition(diff!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it.each([
    ["fresh", "diff-head", "diff-head", "Line unavailable"],
    ["stale", "diff-head", "other-head", "Outdated"],
    ["head-unknown", undefined, "diff-head", "Line unavailable"],
  ] as const)("keeps binary review threads visible for %s snapshots", async (_label, diffHead, threadHead, status) => {
    renderDiffFile(makeFile({ is_binary: true, hunks: [] }), {
      ...(diffHead === undefined ? {} : { diffHeadSHA: diffHead }),
      reviewThreads: [
        makeReviewThread({
          diff_head_sha: threadHead,
          line: 99,
          new_line: 99,
        }),
      ],
    });

    await waitFor(() => {
      expect(screen.getByText("Published review note")).toBeTruthy();
    });
    const notice = screen.getByText("Binary file changed");
    const comment = document.querySelector("[data-review-thread-id='thread-1']");
    expect(screen.getByText(status)).toBeTruthy();
    expect(notice.compareDocumentPosition(comment!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps workspace review authoring disabled", async () => {
    renderDiffFile(makeFile(), {
      reviewEnabled: false,
      diffHeadSHA: "diff-head",
    });

    await expectPierreDiffText(/old linenew line/);
    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(screen.queryByRole("button", { name: "Publish review" })).toBeNull();
  });

  it("clears an open inline composer when review context changes", async () => {
    const file = makeFile();
    const owner = uniqueOwner();
    const { rerender } = renderDiffFile(file, {
      owner,
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
    });

    await clickLineCommentButton(1, "right");
    expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();
    expect(selectedPierreLines()).toHaveLength(4);

    await rerender({
      file,
      provider: "github",
      owner,
      name: "n",
      repoPath: "o/n",
      number: 1,
      reviewEnabled: false,
      diffHeadSHA: "diff-head",
    });

    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();

    await rerender({
      file,
      provider: "github",
      owner,
      name: "n",
      repoPath: "o/n",
      number: 1,
      reviewEnabled: true,
      diffHeadSHA: "new-diff-head",
    });
    await clickLineCommentButton(1, "right");
    expect(screen.getByPlaceholderText("Leave a comment")).toBeTruthy();

    await rerender({
      file,
      provider: "github",
      owner,
      name: "n",
      repoPath: "o/n",
      number: 1,
      reviewEnabled: true,
      diffHeadSHA: "another-diff-head",
    });

    expect(screen.queryByPlaceholderText("Leave a comment")).toBeNull();
    expect(selectedPierreLines()).toHaveLength(0);
  });

  it("loads and expands hidden context without clearing an inline composer draft", async () => {
    const oldText = Array.from({ length: 90 }, (_, index) => `shared ${index + 1}`);
    const newText = [...oldText];
    oldText[1] = "old early";
    newText[1] = "new early";
    oldText[77] = "old late";
    newText[77] = "new late";

    const file = makeFile({
      path: "src/context.txt",
      old_path: "src/context.txt",
      patch: `diff --git a/src/context.txt b/src/context.txt
--- a/src/context.txt
+++ b/src/context.txt
@@ -1,3 +1,3 @@
 shared 1
-old early
+new early
 shared 3
@@ -77,3 +77,3 @@ lateContext
 shared 77
-old late
+new late
 shared 79
`,
      hunks: [
        {
          old_start: 1,
          old_count: 3,
          new_start: 1,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 1",
              old_num: 1,
              new_num: 1,
            },
            { type: "delete", content: "old early", old_num: 2 },
            { type: "add", content: "new early", new_num: 2 },
            {
              type: "context",
              content: "shared 3",
              old_num: 3,
              new_num: 3,
            },
          ],
        },
        {
          old_start: 77,
          old_count: 3,
          new_start: 77,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 77",
              old_num: 77,
              new_num: 77,
            },
            { type: "delete", content: "old late", old_num: 78 },
            { type: "add", content: "new late", new_num: 78 },
            {
              type: "context",
              content: "shared 79",
              old_num: 79,
              new_num: 79,
            },
          ],
        },
      ],
    });
    const { diff } = renderDiffFile(file, {
      reviewEnabled: true,
      diffHeadSHA: "diff-head",
    });
    const loadFileContextPreviews = vi
      .spyOn(diff, "loadFileContextPreviews")
      .mockImplementation((_owner, _name, _number, source, callbacks: FilePreviewPairCallbacks) => {
        callbacks.onSuccess?.({
          old: textPreview(source.old_path, oldText.join("\n")),
          new: textPreview(source.path, newText.join("\n")),
        });
        callbacks.onSettled?.();
      });

    const expandButton = await waitFor(() => {
      const button = document
        .querySelector(".pierre-diff")
        ?.shadowRoot?.querySelector<HTMLElement>("[data-expand-button]");
      expect(button).toBeTruthy();
      return button!;
    });

    await clickLineCommentButton(2, "right");
    await fireEvent.input(screen.getByPlaceholderText("Leave a comment"), {
      target: { value: "Keep this draft while expanding context." },
    });

    await fireEvent.click(expandButton);

    await waitFor(() => {
      const text = document.querySelector(".pierre-diff")?.shadowRoot?.textContent ?? "";
      expect(text).toContain("shared 10");
      expect(text).toContain("@@ -77,3 +77,3 @@ lateContext");
      const expandedLines = expandedContextLineTexts();
      expect(expandedLines.length).toBeGreaterThan(0);
      expect(expandedLines.every((line) => line.length > 0)).toBe(true);
      expect(expandedLines.some((line) => line.includes("shared 10"))).toBe(true);
    });
    expect((screen.getByPlaceholderText("Leave a comment") as HTMLTextAreaElement).value).toBe(
      "Keep this draft while expanding context.",
    );
    expect(loadFileContextPreviews).toHaveBeenCalledTimes(1);
  });

  it("continues expanding context after full file text is loaded", async () => {
    const oldText = Array.from({ length: 90 }, (_, index) => `shared ${index + 1}`);
    const newText = [...oldText];
    oldText[1] = "old early";
    newText[1] = "new early";
    oldText[77] = "old late";
    newText[77] = "new late";

    const file = makeFile({
      path: "src/context.txt",
      old_path: "src/context.txt",
      patch: `diff --git a/src/context.txt b/src/context.txt
--- a/src/context.txt
+++ b/src/context.txt
@@ -1,3 +1,3 @@
 shared 1
-old early
+new early
 shared 3
@@ -77,3 +77,3 @@ lateContext
 shared 77
-old late
+new late
 shared 79
`,
      hunks: [
        {
          old_start: 1,
          old_count: 3,
          new_start: 1,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 1",
              old_num: 1,
              new_num: 1,
            },
            { type: "delete", content: "old early", old_num: 2 },
            { type: "add", content: "new early", new_num: 2 },
            {
              type: "context",
              content: "shared 3",
              old_num: 3,
              new_num: 3,
            },
          ],
        },
        {
          old_start: 77,
          old_count: 3,
          new_start: 77,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 77",
              old_num: 77,
              new_num: 77,
            },
            { type: "delete", content: "old late", old_num: 78 },
            { type: "add", content: "new late", new_num: 78 },
            {
              type: "context",
              content: "shared 79",
              old_num: 79,
              new_num: 79,
            },
          ],
        },
      ],
    });
    const { diff } = renderDiffFile(file);
    const loadFileContextPreviews = vi
      .spyOn(diff, "loadFileContextPreviews")
      .mockImplementation((_owner, _name, _number, source, callbacks: FilePreviewPairCallbacks) => {
        callbacks.onSuccess?.({
          old: textPreview(source.old_path, oldText.join("\n")),
          new: textPreview(source.path, newText.join("\n")),
        });
        callbacks.onSettled?.();
      });

    const firstExpandButton = await waitFor(() => {
      const button = document
        .querySelector(".pierre-diff")
        ?.shadowRoot?.querySelector<HTMLElement>("[data-expand-button]");
      expect(button).toBeTruthy();
      return button!;
    });
    await fireEvent.click(firstExpandButton);

    await waitFor(() => {
      const text = document.querySelector(".pierre-diff")?.shadowRoot?.textContent ?? "";
      expect(text).toContain("shared 10");
      expect(text).not.toContain("shared 50");
      const expandedLines = expandedContextLineTexts();
      expect(expandedLines.length).toBeGreaterThan(0);
      expect(expandedLines.every((line) => line.length > 0)).toBe(true);
      expect(expandedLines.some((line) => line.includes("shared 10"))).toBe(true);
    });

    const nextExpandButton = await waitFor(() => {
      const buttons = Array.from(
        document.querySelector(".pierre-diff")?.shadowRoot?.querySelectorAll<HTMLElement>("[data-expand-button]") ?? [],
      );
      const button = buttons.find((candidate) => candidate !== firstExpandButton);
      expect(button).toBeTruthy();
      return button!;
    });
    await fireEvent.click(nextExpandButton);

    await waitFor(() => {
      const text = document.querySelector(".pierre-diff")?.shadowRoot?.textContent ?? "";
      expect(text).toContain("shared 50");
      const expandedLines = expandedContextLineTexts();
      expect(expandedLines.every((line) => line.length > 0)).toBe(true);
      expect(expandedLines.some((line) => line.includes("shared 50"))).toBe(true);
    });
    expect(loadFileContextPreviews).toHaveBeenCalledTimes(1);
  });

  it("hides Pierre context expansion when file text loading is disabled", async () => {
    const file = makeFile({
      path: "src/context.ts",
      old_path: "src/context.ts",
      patch: `diff --git a/src/context.ts b/src/context.ts
--- a/src/context.ts
+++ b/src/context.ts
@@ -1,3 +1,3 @@
 shared 1
-old early
+new early
 shared 3
@@ -77,3 +77,3 @@ lateContext
 shared 77
-old late
+new late
 shared 79
`,
      hunks: [
        {
          old_start: 1,
          old_count: 3,
          new_start: 1,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 1",
              old_num: 1,
              new_num: 1,
            },
            { type: "delete", content: "old early", old_num: 2 },
            { type: "add", content: "new early", new_num: 2 },
            {
              type: "context",
              content: "shared 3",
              old_num: 3,
              new_num: 3,
            },
          ],
        },
        {
          old_start: 77,
          old_count: 3,
          new_start: 77,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 77",
              old_num: 77,
              new_num: 77,
            },
            { type: "delete", content: "old late", old_num: 78 },
            { type: "add", content: "new late", new_num: 78 },
            {
              type: "context",
              content: "shared 79",
              old_num: 79,
              new_num: 79,
            },
          ],
        },
      ],
    });
    const { diff } = renderDiffFile(file, {
      contextExpansionEnabled: false,
    });
    const loadFileContextPreviews = vi.spyOn(diff, "loadFileContextPreviews");

    await waitFor(() => {
      const root = document.querySelector(".pierre-diff")?.shadowRoot;
      expect(root?.textContent).toContain("@@ -77,3 +77,3 @@ lateContext");
      expect(root?.querySelector("[data-expand-button]")).toBeNull();
    });
    expect(loadFileContextPreviews).not.toHaveBeenCalled();
  });

  it("keeps raw hunk headers on Pierre context separators", async () => {
    const file = makeFile({
      path: "src/context.ts",
      old_path: "src/context.ts",
      patch: `diff --git a/src/context.ts b/src/context.ts
--- a/src/context.ts
+++ b/src/context.ts
@@ -1,3 +1,3 @@
 shared 1
-old early
+new early
 shared 3
@@ -17,3 +17,3 @@ usefulContext
 shared 17
-old late
+new late
 shared 19
`,
      hunks: [
        {
          old_start: 1,
          old_count: 3,
          new_start: 1,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 1",
              old_num: 1,
              new_num: 1,
            },
            { type: "delete", content: "old early", old_num: 2 },
            { type: "add", content: "new early", new_num: 2 },
            {
              type: "context",
              content: "shared 3",
              old_num: 3,
              new_num: 3,
            },
          ],
        },
        {
          old_start: 17,
          old_count: 3,
          new_start: 17,
          new_count: 3,
          lines: [
            {
              type: "context",
              content: "shared 17",
              old_num: 17,
              new_num: 17,
            },
            { type: "delete", content: "old late", old_num: 18 },
            { type: "add", content: "new late", new_num: 18 },
            {
              type: "context",
              content: "shared 19",
              old_num: 19,
              new_num: 19,
            },
          ],
        },
      ],
    });

    renderDiffFile(file);

    await expectPierreDiffText(/@@ -17,3 \+17,3 @@ usefulContext/);
  });
});
