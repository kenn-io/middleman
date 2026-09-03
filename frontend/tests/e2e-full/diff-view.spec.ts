import { expect, test, type Locator, type Page, type Request } from "@playwright/test";
import type { DiffFile, DiffLine, DiffResult, FilesResult } from "../../src/lib/api/types.js";
import { startIsolatedE2EServer } from "./support/e2eServer";

type DiffFixtureFile = Omit<DiffFile, "patch"> & {
  patch?: string;
  preserveHunkCounts?: boolean;
};
type DiffFixture = Omit<DiffResult, "files"> & {
  files: DiffFixtureFile[];
};
type MergeRequestDetailForRoute = {
  diff_head_sha?: string;
  events?: unknown[] | null;
  merge_request?: { ID?: number };
};

const gitBackedDiffTestTimeoutMs = 120_000;

// --- Fixtures ---

// Small fixture: 4 files covering modified (multi-hunk), added, deleted, binary.
const smallDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "internal/server/handler.go",
      old_path: "internal/server/handler.go",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 5,
      deletions: 2,
      hunks: [
        {
          old_start: 10,
          old_count: 7,
          new_start: 10,
          new_count: 8,
          section: "func handleRequest",
          lines: [
            {
              type: "context",
              content: "func handleRequest(w http.ResponseWriter, r *http.Request) {",
              old_num: 10,
              new_num: 10,
            },
            {
              type: "context",
              content: "\tctx := r.Context()",
              old_num: 11,
              new_num: 11,
            },
            {
              type: "delete",
              content: '\tlog.Println("handling request")',
              old_num: 12,
            },
            {
              type: "add",
              content: '\tslog.Info("handling request", "method", r.Method)',
              new_num: 12,
            },
            {
              type: "add",
              content: '\tslog.Info("request path", "path", r.URL.Path)',
              new_num: 13,
            },
            {
              type: "context",
              content: "\tif err := process(ctx); err != nil {",
              old_num: 13,
              new_num: 14,
            },
            {
              type: "context",
              content: "\t\thttp.Error(w, err.Error(), 500)",
              old_num: 14,
              new_num: 15,
            },
          ],
        },
        {
          old_start: 30,
          old_count: 5,
          new_start: 31,
          new_count: 8,
          section: "func process",
          lines: [
            {
              type: "context",
              content: "func process(ctx context.Context) error {",
              old_num: 30,
              new_num: 31,
            },
            { type: "delete", content: "\treturn nil", old_num: 31 },
            {
              type: "add",
              content: "\tif err := validate(ctx); err != nil {",
              new_num: 32,
            },
            {
              type: "add",
              content: '\t\treturn fmt.Errorf("validation: %w", err)',
              new_num: 33,
            },
            { type: "add", content: "\t}", new_num: 34 },
            { type: "add", content: "\treturn nil", new_num: 35 },
            { type: "context", content: "}", old_num: 32, new_num: 36 },
          ],
        },
      ],
    },
    {
      path: "frontend/src/lib/utils/format.ts",
      old_path: "frontend/src/lib/utils/format.ts",
      status: "added",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 8,
      deletions: 0,
      hunks: [
        {
          old_start: 0,
          old_count: 0,
          new_start: 1,
          new_count: 8,
          lines: [
            {
              type: "add",
              content: "export function formatDate(d: Date): string {",
              new_num: 1,
            },
            {
              type: "add",
              content: "  const year = d.getFullYear();",
              new_num: 2,
            },
            {
              type: "add",
              content: "  const month = String(d.getMonth() + 1).padStart(2, '0');",
              new_num: 3,
            },
            {
              type: "add",
              content: "  const day = String(d.getDate()).padStart(2, '0');",
              new_num: 4,
            },
            {
              type: "add",
              content: "  return `${year}-${month}-${day}`;",
              new_num: 5,
            },
            { type: "add", content: "}", new_num: 6 },
            { type: "add", content: "", new_num: 7 },
            {
              type: "add",
              content: "export function formatNumber(n: number): string {",
              new_num: 8,
            },
          ],
        },
      ],
    },
    {
      path: "internal/legacy/old_handler.go",
      old_path: "internal/legacy/old_handler.go",
      status: "deleted",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 0,
      deletions: 12,
      hunks: [
        {
          old_start: 1,
          old_count: 12,
          new_start: 0,
          new_count: 0,
          lines: [
            { type: "delete", content: "package legacy", old_num: 1 },
            { type: "delete", content: "", old_num: 2 },
            {
              type: "delete",
              content: 'import "net/http"',
              old_num: 3,
            },
            { type: "delete", content: "", old_num: 4 },
            {
              type: "delete",
              content: "func OldHandler(w http.ResponseWriter, r *http.Request) {",
              old_num: 5,
            },
            {
              type: "delete",
              content: "\tw.WriteHeader(200)",
              old_num: 6,
            },
            {
              type: "delete",
              content: '\tw.Write([]byte("ok"))',
              old_num: 7,
            },
            { type: "delete", content: "}", old_num: 8 },
            { type: "delete", content: "", old_num: 9 },
            { type: "delete", content: "func init() {", old_num: 10 },
            {
              type: "delete",
              content: '\thttp.HandleFunc("/old", OldHandler)',
              old_num: 11,
            },
            { type: "delete", content: "}", old_num: 12 },
          ],
        },
      ],
    },
    {
      path: "assets/logo.png",
      old_path: "assets/logo.png",
      status: "modified",
      is_binary: true,
      is_generated: false,
      is_whitespace_only: false,
      additions: 0,
      deletions: 0,
      hunks: [],
    },
  ],
});

// Generate a large diff (50 files) for perf tests.
function makeLargeDiff(): DiffResult {
  const files: DiffFixtureFile[] = [];
  for (let i = 0; i < 50; i++) {
    const lines: DiffLine[] = [];
    for (let j = 1; j <= 20; j++) {
      if (j % 5 === 0) {
        lines.push({
          type: "delete" as const,
          content: `  old line ${j}`,
          old_num: j,
        });
        lines.push({
          type: "add" as const,
          content: `  new line ${j}`,
          new_num: j,
        });
      } else {
        lines.push({
          type: "context" as const,
          content: `  line ${j}`,
          old_num: j,
          new_num: j,
        });
      }
    }
    files.push({
      path: `src/pkg${Math.floor(i / 5)}/file_${i}.go`,
      old_path: `src/pkg${Math.floor(i / 5)}/file_${i}.go`,
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 4,
      deletions: 4,
      hunks: [
        {
          old_start: 1,
          old_count: 20,
          new_start: 1,
          new_count: 20,
          lines,
        },
      ],
    });
  }
  return withServerDiffData({
    stale: false,
    whitespace_only_count: 0,
    files,
  });
}

function makeSingleLineFixtureFile(path: string): DiffFixtureFile {
  return {
    path,
    old_path: path,
    status: "modified",
    is_binary: false,
    is_generated: false,
    is_whitespace_only: false,
    additions: 1,
    deletions: 0,
    hunks: [
      {
        old_start: 1,
        old_count: 1,
        new_start: 1,
        new_count: 2,
        lines: [
          {
            type: "context",
            content: "package server",
            old_num: 1,
            new_num: 1,
          },
          { type: "add", content: "// changed", new_num: 2 },
        ],
      },
    ],
  };
}

const largeDiff = makeLargeDiff();

// Stale fixture reuses small diff with stale flag.
const staleDiff = { ...smallDiff, stale: true };

const hunklessTextDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "internal/server/config.go",
      old_path: "internal/server/config.go",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 0,
      deletions: 0,
      hunks: [],
    },
  ],
});

const emptyAddedFileDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "fixtures/.gitkeep",
      old_path: "fixtures/.gitkeep",
      status: "added",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 0,
      deletions: 0,
      patch: [
        "diff --git a/fixtures/.gitkeep b/fixtures/.gitkeep",
        "new file mode 100644",
        "index 0000000..e69de29",
        "",
      ].join("\n"),
      hunks: [],
    },
  ],
});

const oversizedSparseDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "src/huge-context.ts",
      old_path: "src/huge-context.ts",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 0,
      hunks: [
        {
          old_start: 1_000_000,
          old_count: 1,
          new_start: 1_000_000,
          new_count: 2,
          lines: [
            {
              type: "context",
              content: "export const distant = true;",
              old_num: 1_000_000,
              new_num: 1_000_000,
            },
            {
              type: "add",
              content: "export const changedFarAway = true;",
              new_num: 1_000_001,
            },
          ],
        },
      ],
    },
  ],
});

const syntaxContextPath = "src/example.test.ts";
const syntaxContextOldText = [
  "import { vi } from 'vitest';",
  "",
  "function render() {",
  "  return null;",
  "}",
  "",
  "const setup = true;",
  "",
  "function mount() {",
  "const html = `",
  "  <div>",
  "  </div>",
  "  <main>",
  "    content",
  "  </main>",
  "  <footer>",
  "    footer",
  "  </footer>",
  "`;",
  "afterRender();",
  ...Array.from({ length: 59 }, (_, index) => `// unchanged ${index + 21}`),
  "function makeFile() {}",
  "export const done = true;",
].join("\n");
const syntaxContextNewText = [
  "import { vi } from 'vitest';",
  "",
  "function render() {",
  "  return null;",
  "}",
  "",
  "const setup = true;",
  "",
  "function mount() {",
  "const html = `",
  "  <span>new</span>",
  "  <div>",
  "  </div>",
  "  <main>",
  "    content",
  "  </main>",
  "  <footer>",
  "    footer",
  "  </footer>",
  "`;",
  "afterRender();",
  ...Array.from({ length: 59 }, (_, index) => `// unchanged ${index + 21}`),
  'vi.doMock("./worker", () => ({',
  "  run: () => undefined,",
  "}));",
  "function makeFile() {}",
  "export const done = true;",
].join("\n");
const syntaxContextDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: syntaxContextPath,
      old_path: syntaxContextPath,
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 4,
      deletions: 0,
      hunks: [
        {
          old_start: 10,
          old_count: 2,
          new_start: 10,
          new_count: 3,
          section: "function mount() {",
          lines: [
            { type: "context", content: "const html = `", old_num: 10, new_num: 10 },
            { type: "add", content: "  <span>new</span>", new_num: 11 },
            { type: "context", content: "  <div>", old_num: 11, new_num: 12 },
          ],
        },
        {
          old_start: 80,
          old_count: 2,
          new_start: 81,
          new_count: 5,
          section: "afterRender();",
          lines: [
            { type: "add", content: 'vi.doMock("./worker", () => ({', new_num: 81 },
            { type: "add", content: "  run: () => undefined,", new_num: 82 },
            { type: "add", content: "}));", new_num: 83 },
            { type: "context", content: "function makeFile() {}", old_num: 80, new_num: 84 },
            { type: "context", content: "export const done = true;", old_num: 81, new_num: 85 },
          ],
        },
      ],
    },
  ],
});

const previewDiff: DiffResult = withServerDiffData({
  stale: smallDiff.stale,
  whitespace_only_count: smallDiff.whitespace_only_count,
  files: [
    {
      path: "docs/preview.md",
      old_path: "docs/preview.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 6,
      deletions: 5,
      hunks: [
        {
          old_start: 1,
          old_count: 7,
          new_start: 1,
          new_count: 7,
          lines: [
            {
              type: "context",
              content: "# Rendered preview",
              old_num: 1,
              new_num: 1,
            },
            { type: "context", content: "", old_num: 2, new_num: 2 },
            {
              type: "delete",
              content: "Old paragraph that should be highlighted.",
              old_num: 3,
            },
            {
              type: "add",
              content: "New paragraph that should be highlighted.",
              new_num: 3,
            },
            { type: "context", content: "", old_num: 4, new_num: 4 },
            {
              type: "delete",
              content: "- [ ] Markdown task",
              old_num: 5,
            },
            { type: "add", content: "- [x] Markdown task", new_num: 5 },
            {
              type: "delete",
              content: "<em>alpha</em><strong>beta</strong><code>gamma</code>",
              old_num: 6,
            },
            {
              type: "add",
              content: '<a href="/link">link</a><strong>two</strong><code>three</code>',
              new_num: 6,
            },
            {
              type: "delete",
              content: "<table><tbody><tr><td>Keep</td></tr></tbody></table>",
              old_num: 7,
            },
            {
              type: "add",
              content: "<table><tbody><tr><td>Keep</td><td>Added cell</td></tr></tbody></table>",
              new_num: 7,
            },
          ],
        },
      ],
    },
    ...smallDiff.files,
  ],
});

const listReviewPreviewDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/list-review.md",
      old_path: "docs/list-review.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 4,
      deletions: 0,
      hunks: [
        {
          old_start: 1,
          old_count: 0,
          new_start: 1,
          new_count: 4,
          lines: [
            { type: "add", content: "- Issues", new_num: 1 },
            { type: "add", content: "- Actions", new_num: 2 },
            { type: "add", content: "- Statuses", new_num: 3 },
            { type: "add", content: "- Members", new_num: 4 },
          ],
        },
      ],
    },
    ...smallDiff.files,
  ],
});

const untargetedListReviewPreviewDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/untargeted-list-review.md",
      old_path: "docs/untargeted-list-review.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 0,
      hunks: [
        {
          old_start: 1,
          old_count: 4,
          new_start: 1,
          new_count: 5,
          lines: [
            { type: "context", content: "- Issues", old_num: 1, new_num: 1 },
            { type: "context", content: "- Actions", old_num: 2, new_num: 2 },
            { type: "context", content: "- Statuses", old_num: 3, new_num: 3 },
            { type: "context", content: "", old_num: 4, new_num: 4 },
            { type: "add", content: "Paragraph with review note.", new_num: 5 },
          ],
        },
      ],
    },
    ...smallDiff.files,
  ],
});

const changedListReviewPreviewDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/changed-list-review.md",
      old_path: "docs/changed-list-review.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 0,
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
    },
    ...smallDiff.files,
  ],
});

const edgeListReviewPreviewDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/edge-added-list-review.md",
      old_path: "docs/edge-added-list-review.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 3,
      deletions: 0,
      hunks: [
        {
          old_start: 1,
          old_count: 2,
          new_start: 1,
          new_count: 5,
          lines: [
            { type: "add", content: "- Prepended", new_num: 1 },
            { type: "add", content: "prepended details", new_num: 2 },
            { type: "context", content: "- Issues", old_num: 1, new_num: 3 },
            { type: "context", content: "- Statuses", old_num: 2, new_num: 4 },
            { type: "add", content: "- Appended", new_num: 5 },
          ],
        },
      ],
    },
    {
      path: "docs/edge-deleted-list-review.md",
      old_path: "docs/edge-deleted-list-review.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 0,
      deletions: 3,
      hunks: [
        {
          old_start: 1,
          old_count: 5,
          new_start: 1,
          new_count: 2,
          lines: [
            { type: "delete", content: "- Removed first", old_num: 1 },
            { type: "delete", content: "removed first details", old_num: 2 },
            { type: "context", content: "- Issues", old_num: 3, new_num: 1 },
            { type: "context", content: "- Statuses", old_num: 4, new_num: 2 },
            { type: "delete", content: "- Removed last", old_num: 5 },
          ],
        },
      ],
    },
    ...smallDiff.files,
  ],
});

const multiHunkMarkdownDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/multihunk.md",
      old_path: "docs/multihunk.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 0,
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
          new_count: 3,
          lines: [
            { type: "context", content: "second hunk code", old_num: 10, new_num: 10 },
            { type: "add", content: "added hunk code", new_num: 11 },
            { type: "context", content: "```", old_num: 11, new_num: 12 },
          ],
        },
      ],
    },
  ],
});

const blockFillMarkdownDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "docs/block-fill.md",
      old_path: "docs/block-fill.md",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 1,
      hunks: [
        {
          old_start: 1,
          old_count: 6,
          new_start: 1,
          new_count: 6,
          lines: [
            { type: "context", content: "# Block fills", old_num: 1, new_num: 1 },
            { type: "context", content: "", old_num: 2, new_num: 2 },
            {
              type: "delete",
              content: "Removed standalone block that should have a filled background.",
              old_num: 3,
            },
            { type: "context", content: "", old_num: 4, new_num: 3 },
            { type: "context", content: "Shared stable block", old_num: 5, new_num: 4 },
            { type: "context", content: "", old_num: 6, new_num: 5 },
            {
              type: "add",
              content: "Added standalone block that should have a filled background.",
              new_num: 6,
            },
          ],
        },
      ],
    },
  ],
});

const splitRichPreviewDiff: DiffResult = withServerDiffData({
  stale: false,
  whitespace_only_count: 0,
  files: [previewDiff.files[0]!, blockFillMarkdownDiff.files[0]!, ...smallDiff.files],
});

// --- Helpers ---

function cssString(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

async function expectNonTransparentBackground(locator: Locator) {
  await expect
    .poll(async () => {
      const background = await locator.evaluate((element) => getComputedStyle(element).backgroundColor);
      return background !== "rgba(0, 0, 0, 0)" && background !== "transparent";
    })
    .toBe(true);
}

function treeFileItems(pageOrLocator: Page | ReturnType<Page["locator"]>) {
  return pageOrLocator.locator('.diff-file-tree [data-item-type="file"]');
}

async function treeFileItemPaths(pageOrLocator: Page | ReturnType<Page["locator"]>) {
  return await treeFileItems(pageOrLocator).evaluateAll((items) =>
    items.map((item) => item.getAttribute("data-item-path") ?? ""),
  );
}

async function renderedDiffFilePaths(pageOrLocator: Page | ReturnType<Page["locator"]>) {
  return await pageOrLocator
    .locator(".diff-file")
    .evaluateAll((files) => files.map((file) => file.getAttribute("data-file-path") ?? ""));
}

function treeFileItem(pageOrLocator: Page | ReturnType<Page["locator"]>, path: string) {
  return pageOrLocator.locator(`.diff-file-tree [data-item-path="${cssString(path)}"]`);
}

async function clickVisibleTarget(target: Locator): Promise<void> {
  await expect(async () => {
    await expect(target).toBeVisible({ timeout: 2_000 });
    await target.scrollIntoViewIfNeeded({ timeout: 2_000 });
    const box = await target.boundingBox();
    expect(box).not.toBeNull();
    await target.page().mouse.click(box!.x + box!.width / 2, box!.y + box!.height / 2);
  }).toPass({ timeout: 10_000 });
}

async function activateVisibleTarget(target: Locator): Promise<void> {
  await expect
    .poll(
      async () => {
        try {
          return await target.evaluate((element) => {
            if (!(element instanceof HTMLElement)) return false;
            const rect = element.getBoundingClientRect();
            const style = getComputedStyle(element);
            if (rect.width <= 0 || rect.height <= 0 || style.display === "none" || style.visibility === "hidden") {
              return false;
            }
            element.scrollIntoView({ block: "center", inline: "center" });
            element.click();
            return true;
          });
        } catch {
          return false;
        }
      },
      { timeout: 10_000 },
    )
    .toBe(true);
}

async function clickTreeFileItem(pageOrLocator: Page | ReturnType<Page["locator"]>, path: string): Promise<void> {
  const item = treeFileItem(pageOrLocator, path);
  await activateVisibleTarget(item);
  await expect(item).toHaveAttribute("aria-selected", "true");
}

async function jumpToFile(page: Page, path: string): Promise<void> {
  await page.getByRole("button", { name: "Jump to file" }).click();
  const dialog = page.getByRole("dialog", { name: "Jump to file" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("combobox", { name: "Jump to file" }).fill(path);
  await dialog.getByRole("option").first().click();
  await expect(dialog).toBeHidden();
}

async function scrollFileTreeToTop(page: Page): Promise<void> {
  await page.locator(".diff-file-tree").evaluate((host) => {
    host.scrollTop = 0;
    const root = host.shadowRoot;
    if (!root) return;
    for (const element of root.querySelectorAll<HTMLElement>("*")) {
      if (element.scrollHeight > element.clientHeight) {
        element.scrollTop = 0;
      }
    }
  });
}

const diffAdditionsSelector = '[data-content] [data-line-type="change-addition"]';
const diffDeletionsSelector = '[data-content] [data-line-type="change-deletion"]';
const diffContextSelector = '[data-content] [data-line-type="context"]';
const diffHunkSeparatorsSelector = '[data-content] [data-separator="line-info"]';

async function pierreDiffCount(file: ReturnType<Page["locator"]>, selector: string) {
  return await file.locator(".pierre-diff").evaluate((host, selector) => {
    return host.shadowRoot?.querySelectorAll(selector).length ?? 0;
  }, selector);
}

async function pierreDiffTexts(file: ReturnType<Page["locator"]>, selector: string) {
  return await file.locator(".pierre-diff").evaluate((host, selector) => {
    return Array.from(host.shadowRoot?.querySelectorAll(selector) ?? []).map(
      (element) => element.textContent?.trim() ?? "",
    );
  }, selector);
}

async function pierreRenderedDiffTextStats(
  file: ReturnType<Page["locator"]>,
  selector = "[data-content] [data-line-type]",
) {
  return await file.locator(".pierre-diff").evaluate((host, selector) => {
    const rows = Array.from(host.shadowRoot?.querySelectorAll(selector) ?? []).map(
      (element) => element.textContent?.trim() ?? "",
    );
    return {
      blank: rows.filter((text) => text.length === 0).length,
      nonBlank: rows.filter((text) => text.length > 0).length,
      texts: rows.filter((text) => text.length > 0),
    };
  }, selector);
}

async function expectPierreDiffCount(file: ReturnType<Page["locator"]>, selector: string, count: number) {
  await expect.poll(() => pierreDiffCount(file, selector)).toBe(count);
}

async function expectPierreDiffCountAtLeast(file: ReturnType<Page["locator"]>, selector: string, count: number) {
  await expect.poll(() => pierreDiffCount(file, selector)).toBeGreaterThanOrEqual(count);
}

async function expectPierreDiffFirstVisible(file: ReturnType<Page["locator"]>, selector: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host, selector) => {
        const element = host.shadowRoot?.querySelector(selector);
        if (!(element instanceof HTMLElement)) return false;
        const rect = element.getBoundingClientRect();
        return rect.width > 0 && rect.height > 0;
      }, selector);
    })
    .toBe(true);
}

async function expectPierreDiffFirstText(file: ReturnType<Page["locator"]>, selector: string, text: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host, selector) => {
        return host.shadowRoot?.querySelector(selector)?.textContent ?? "";
      }, selector);
    })
    .toContain(text);
}

async function expectPierreDiffVisibleText(file: ReturnType<Page["locator"]>, selector: string, text: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate(
        (host, { selector, text }) => {
          return Array.from(host.shadowRoot?.querySelectorAll(selector) ?? []).some((element) => {
            if (!(element instanceof HTMLElement)) return false;
            const rect = element.getBoundingClientRect();
            return rect.width > 0 && rect.height > 0 && element.textContent?.includes(text);
          });
        },
        { selector, text },
      );
    })
    .toBe(true);
}

async function expectPierreDiffVisibleExactText(file: ReturnType<Page["locator"]>, selector: string, text: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host, selector) => {
        return Array.from(host.shadowRoot?.querySelectorAll(selector) ?? [])
          .filter((element): element is HTMLElement => {
            if (!(element instanceof HTMLElement)) return false;
            const rect = element.getBoundingClientRect();
            return rect.width > 0 && rect.height > 0;
          })
          .map((element) => element.textContent ?? "");
      }, selector);
    })
    .toContain(text);
}

async function pierreAdditionTokenStats(file: ReturnType<Page["locator"]>, lineText: string) {
  return await file.locator(".pierre-diff").evaluate((host, lineText) => {
    const rows = Array.from(
      host.shadowRoot?.querySelectorAll('[data-content] [data-line-type="change-addition"]') ?? [],
    );
    const row = rows.find((candidate) => candidate.textContent?.includes(lineText));
    const spans = Array.from(row?.querySelectorAll("span") ?? []).map((span) => span.textContent ?? "");
    return {
      hasSeparateDoMock: spans.includes("doMock"),
      hasSeparateVi: spans.includes("vi"),
      singleLineSpan: spans.length === 1 && spans[0] === lineText,
    };
  }, lineText);
}

async function expectRenderedNonBlankRows(file: ReturnType<Page["locator"]>, textFragment: string) {
  await expect
    .poll(async () => {
      const stats = await pierreRenderedDiffTextStats(file);
      return {
        blank: stats.blank,
        hasText: stats.texts.some((text) => text.includes(textFragment)),
        nonBlankPositive: stats.nonBlank > 0,
      };
    })
    .toEqual({
      blank: 0,
      hasText: true,
      nonBlankPositive: true,
    });
}

async function expectVisibleExpandedRowContent(file: ReturnType<Page["locator"]>, expectedText: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host, expectedText) => {
        const root = host.shadowRoot;
        const gutters = Array.from(root?.querySelectorAll("[data-gutter] > [data-line-index]") ?? []);
        const contents = Array.from(root?.querySelectorAll("[data-content] > [data-line-index]") ?? []);
        const visibleExpanded = gutters.filter((gutter): gutter is HTMLElement => {
          if (!(gutter instanceof HTMLElement)) return false;
          if (gutter.getAttribute("data-line-type") !== "context-expanded") return false;
          const rect = gutter.getBoundingClientRect();
          return rect.bottom > 0 && rect.top < window.innerHeight;
        });

        let matched = false;
        let missing = 0;
        let blank = 0;
        for (const gutter of visibleExpanded) {
          const gutterRect = gutter.getBoundingClientRect();
          const index = gutter.getAttribute("data-line-index");
          const content = contents.find((candidate): candidate is HTMLElement => {
            if (!(candidate instanceof HTMLElement)) return false;
            if (candidate.getAttribute("data-line-index") !== index) return false;
            const contentRect = candidate.getBoundingClientRect();
            return Math.abs(contentRect.top - gutterRect.top) <= 1;
          });
          if (!content) {
            missing += 1;
            continue;
          }
          const text = content.textContent?.trim() ?? "";
          if (text.length === 0) blank += 1;
          if (text.includes(expectedText)) matched = true;
        }

        return {
          blank,
          matched,
          missing,
          visibleExpandedPositive: visibleExpanded.length > 0,
        };
      }, expectedText);
    })
    .toEqual({
      blank: 0,
      matched: true,
      missing: 0,
      visibleExpandedPositive: true,
    });
}

async function expectRenderedPierreContainmentDisabled(page: Page): Promise<void> {
  await expect
    .poll(
      async () => {
        return await page.locator(".diff-file .pierre-diff").evaluateAll((hosts) => {
          for (const host of hosts) {
            const root = host.shadowRoot;
            const code = root?.querySelector("code");
            if (!(root instanceof ShadowRoot) || !(code instanceof HTMLElement)) continue;

            const visibleTextRows = Array.from(root.querySelectorAll("[data-content] [data-line-type]")).filter(
              (element): element is HTMLElement => {
                if (!(element instanceof HTMLElement)) return false;
                const rect = element.getBoundingClientRect();
                return rect.width > 0 && rect.height > 0 && (element.textContent?.trim().length ?? 0) > 0;
              },
            ).length;
            if (visibleTextRows === 0) continue;

            const placeholderProbe = document.createElement("div");
            placeholderProbe.dataset.placeholder = "";
            root.append(placeholderProbe);
            const containment = {
              code: getComputedStyle(code).contain,
              placeholder: getComputedStyle(placeholderProbe).contain,
            };
            placeholderProbe.remove();
            return containment;
          }
          return null;
        });
      },
      { timeout: 10_000 },
    )
    .toEqual({ code: "none", placeholder: "none" });
}

async function scrollDiffAreaUntilPierreText(
  page: Page,
  diffArea: Locator,
  file: ReturnType<Page["locator"]>,
  selector: string,
  text: string,
  scrollDelta: number,
): Promise<void> {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const hasText = await file.locator(".pierre-diff").evaluate(
      (host, { selector, text }) => {
        return Array.from(host.shadowRoot?.querySelectorAll(selector) ?? []).some((element) =>
          element.textContent?.includes(text),
        );
      },
      { selector, text },
    );
    if (hasText) return;
    await diffArea.evaluate((area, delta) => {
      area.scrollTop += delta;
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    }, scrollDelta);
    await page.waitForTimeout(50);
  }
  const texts = await pierreDiffTexts(file, selector);
  expect(texts.join("\n")).toContain(text);
}

async function expectPierreCodeTabSize(file: ReturnType<Page["locator"]>, tabSize: string) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host) => {
        const code = host.shadowRoot?.querySelector("[data-code]");
        return code instanceof HTMLElement ? getComputedStyle(code).tabSize : "";
      });
    })
    .toBe(tabSize);
}

async function clickPierreContextExpander(
  page: Page,
  file: ReturnType<Page["locator"]>,
  separatorIndex = 0,
  buttonSelector = "[data-expand-button]",
  options: { shiftKey?: boolean } = {},
): Promise<void> {
  const separator = file
    .locator(".pierre-diff [data-separator][data-expand-index]")
    .filter({ visible: true })
    .nth(separatorIndex);
  const expander = separator.locator(buttonSelector).filter({ visible: true }).first();
  if (options.shiftKey) {
    await expect(expander).toBeVisible();
    await expander.evaluate((button: HTMLElement) => {
      button.dispatchEvent(
        new MouseEvent("click", {
          bubbles: true,
          cancelable: true,
          composed: true,
          shiftKey: true,
        }),
      );
    });
  } else {
    await activateVisibleTarget(expander);
  }
}

async function expectPierreDarkBackgroundMatchesAppSurface(file: ReturnType<Page["locator"]>) {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host) => {
        const sample = document.createElement("div");
        sample.style.cssText = [
          "position: fixed",
          "left: -9999px",
          "top: -9999px",
          "width: 1px",
          "height: 1px",
          "background: var(--bg-surface)",
        ].join(";");
        document.body.append(sample);
        const appSurface = getComputedStyle(sample).backgroundColor;
        sample.remove();

        const root = host.shadowRoot;
        const pre = root?.querySelector("pre");
        const contextLine = root?.querySelector("[data-content] [data-line-type='context']");
        return {
          host: getComputedStyle(host).backgroundColor === appSurface,
          pre: pre instanceof HTMLElement && getComputedStyle(pre).backgroundColor === appSurface,
          context: contextLine instanceof HTMLElement && getComputedStyle(contextLine).backgroundColor === appSurface,
        };
      });
    })
    .toEqual({ host: true, pre: true, context: true });
}

async function expectPierreChangeColorsMatchAppTokens(file: ReturnType<Page["locator"]>, themeType: "dark" | "light") {
  await expect
    .poll(async () => {
      return await file.locator(".pierre-diff").evaluate((host, themeType) => {
        const sample = document.createElement("div");
        sample.style.cssText = [
          "position: fixed",
          "left: -9999px",
          "top: -9999px",
          "width: 1px",
          "height: 1px",
          "color: var(--accent-green)",
        ].join(";");
        document.body.append(sample);
        const appGreen = getComputedStyle(sample).color;
        sample.style.background =
          themeType === "dark"
            ? "color-mix(in srgb, transparent 76%," + " color-mix(in srgb, var(--accent-green) 42%, black))"
            : "color-mix(in srgb, var(--accent-green) 22%, transparent)";
        const appGreenEmphasis = getComputedStyle(sample).backgroundColor;
        sample.style.background = "color-mix(in srgb, var(--accent-green) 55%, black)";
        const oldLightModeDarkGreenLine = getComputedStyle(sample).backgroundColor;
        sample.style.color = "var(--accent-red)";
        const appRed = getComputedStyle(sample).color;
        sample.style.background =
          themeType === "dark"
            ? "color-mix(in srgb, transparent 76%," + " color-mix(in srgb, var(--accent-red) 58%, black))"
            : "color-mix(in srgb, var(--accent-red) 24%, transparent)";
        const appRedEmphasis = getComputedStyle(sample).backgroundColor;
        sample.style.background = "color-mix(in srgb, var(--accent-red) 69%, black)";
        const oldLightModeDarkRedLine = getComputedStyle(sample).backgroundColor;
        sample.remove();

        const root = host.shadowRoot;
        const additionLine = root?.querySelector("[data-content] [data-line-type='change-addition']");
        const additionNumber = root?.querySelector("[data-column-number][data-line-type='change-addition']");
        const additionSpan = root?.querySelector("[data-line-type='change-addition'] [data-diff-span]");
        const deletionLine = root?.querySelector("[data-content] [data-line-type='change-deletion']");
        const deletionNumber = root?.querySelector("[data-column-number][data-line-type='change-deletion']");
        const deletionSpan = root?.querySelector("[data-line-type='change-deletion'] [data-diff-span]");

        const additionLineBackground =
          additionLine instanceof HTMLElement ? getComputedStyle(additionLine).backgroundColor : "";
        const deletionLineBackground =
          deletionLine instanceof HTMLElement ? getComputedStyle(deletionLine).backgroundColor : "";
        return {
          additionNumber: additionNumber instanceof HTMLElement && getComputedStyle(additionNumber).color === appGreen,
          additionBar:
            additionNumber instanceof HTMLElement &&
            getComputedStyle(additionNumber, "::before").backgroundColor === appGreen,
          additionSpan:
            additionSpan instanceof HTMLElement && getComputedStyle(additionSpan).backgroundColor === appGreenEmphasis,
          deletionNumber: deletionNumber instanceof HTMLElement && getComputedStyle(deletionNumber).color === appRed,
          deletionSpan:
            deletionSpan instanceof HTMLElement && getComputedStyle(deletionSpan).backgroundColor === appRedEmphasis,
          lightAdditionLineSafe:
            themeType === "dark" ||
            (additionLineBackground !== "" && additionLineBackground !== oldLightModeDarkGreenLine),
          lightDeletionLineSafe:
            themeType === "dark" ||
            (deletionLineBackground !== "" && deletionLineBackground !== oldLightModeDarkRedLine),
        };
      }, themeType);
    })
    .toEqual({
      additionNumber: true,
      additionBar: true,
      additionSpan: true,
      deletionNumber: true,
      deletionSpan: true,
      lightAdditionLineSafe: true,
      lightDeletionLineSafe: true,
    });
}

function patchLinePrefix(line: DiffLine): string {
  switch (line.type) {
    case "add":
      return "+";
    case "delete":
      return "-";
    default:
      return " ";
  }
}

function patchRange(start: number, count: number): string {
  return count === 1 ? `${start}` : `${start},${count}`;
}

function patchForFile(file: DiffFixtureFile): string {
  if (file.is_binary || file.hunks.length === 0) return "";
  const oldPath = file.status === "added" ? "/dev/null" : `a/${file.old_path || file.path}`;
  const newPath = file.status === "deleted" ? "/dev/null" : `b/${file.path}`;
  const lines = [`diff --git a/${file.old_path || file.path} b/${file.path}`, `--- ${oldPath}`, `+++ ${newPath}`];
  for (const hunk of file.hunks) {
    lines.push(
      `@@ -${patchRange(hunk.old_start, hunk.old_count)} +${patchRange(hunk.new_start, hunk.new_count)} @@${hunk.section ? ` ${hunk.section}` : ""}`,
    );
    for (const line of hunk.lines) {
      lines.push(`${patchLinePrefix(line)}${line.content}`);
    }
  }
  return `${lines.join("\n")}\n`;
}

function normalizeFixtureFile(file: DiffFixtureFile): DiffFixtureFile {
  if (file.preserveHunkCounts) return file;
  return {
    ...file,
    hunks: file.hunks.map((hunk) => ({
      ...hunk,
      old_count: hunk.lines.filter((line) => line.type !== "add").length,
      new_count: hunk.lines.filter((line) => line.type !== "delete").length,
    })),
  };
}

function withServerDiffData(fixture: DiffFixture): DiffResult {
  const files = fixture.files.map((file) => {
    const normalized = normalizeFixtureFile(file);
    return {
      ...normalized,
      patch: normalized.patch ?? patchForFile(normalized),
    };
  });
  return {
    ...fixture,
    files,
  };
}

function filesFromDiff(fixture: DiffResult): FilesResult {
  const diff = withServerDiffData(fixture as DiffFixture);
  return {
    stale: diff.stale,
    whitespace_only_count: diff.whitespace_only_count,
    files: backendOrderedFiles(diff.files).map((f) => ({
      ...f,
      additions: 0,
      deletions: 0,
      hunks: [],
    })),
  };
}

function backendOrderedFiles<T extends { path: string; old_path?: string }>(files: readonly T[]): T[] {
  return [...files].sort((left, right) => {
    const pathOrder = compareBackendDiffPaths(left.path, right.path);
    if (pathOrder !== 0) return pathOrder;
    return compareBackendDiffPaths(left.old_path ?? "", right.old_path ?? "");
  });
}

function compareBackendDiffPaths(left: string, right: string): number {
  const leftParts = left.split("/");
  const rightParts = right.split("/");
  const partCount = Math.min(leftParts.length, rightParts.length);
  for (let i = 0; i < partCount; i++) {
    if (leftParts[i] === rightParts[i]) continue;
    const leftIsDir = leftParts.length > i + 1;
    const rightIsDir = rightParts.length > i + 1;
    if (leftIsDir && !rightIsDir) return -1;
    if (!leftIsDir && rightIsDir) return 1;
    return leftParts[i]! < rightParts[i]! ? -1 : 1;
  }
  return leftParts.length - rightParts.length;
}

function diffResponseFromFixture(fixture: DiffResult | DiffFixture): DiffResult {
  const response = withServerDiffData(fixture as DiffFixture);
  return {
    ...response,
    files: backendOrderedFiles(response.files),
  };
}

async function mockDiffApi(
  page: Page,
  fixture: DiffResult | DiffFixture | (() => DiffResult | DiffFixture),
): Promise<void> {
  const currentFixture = (): DiffResult => diffResponseFromFixture(typeof fixture === "function" ? fixture() : fixture);

  await page.route("**/api/v1/pulls/github/acme/widgets/1/files", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(filesFromDiff(currentFixture())),
    });
  });
  await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(currentFixture()),
    });
  });
}

async function mockFilePreviewApi(page: Page): Promise<void> {
  await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.searchParams.get("path");
    if (path === "docs/preview.md") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          path,
          media_type: "text/markdown; charset=utf-8",
          encoding: "base64",
          content: btoa(
            '# Rendered preview\n\nNew paragraph that should be highlighted.\n\n- [x] Markdown task\n<a href="/link">link</a><strong>two</strong><code>three</code>\n<table><tbody><tr><td>Keep</td><td>Added cell</td></tr></tbody></table>\n',
          ),
        }),
      });
      return;
    }
    if (path === "assets/logo.png") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          path,
          media_type: "image/png",
          encoding: "base64",
          content: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==",
        }),
      });
      return;
    }
    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({ detail: "preview unavailable" }),
    });
  });
}

async function mockReviewThreadOnPreviewMarkdown(
  page: Page,
  body: string,
  line = 3,
  path = "docs/preview.md",
): Promise<void> {
  await mockReviewThreadsOnPreviewMarkdown(page, [{ body, line, path }]);
}

interface PreviewMarkdownReviewThread {
  body: string;
  line?: number;
  lineType?: "add" | "delete";
  newLine?: number;
  oldLine?: number;
  path?: string;
  side?: "left" | "right";
}

async function mockReviewThreadsOnPreviewMarkdown(page: Page, threads: PreviewMarkdownReviewThread[]): Promise<void> {
  await page.route("**/api/v1/pulls/github/acme/widgets/1", async (route) => {
    const response = await route.fetch();
    const detail = (await response.json()) as MergeRequestDetailForRoute;
    const timestamp = "2026-06-17T15:00:00Z";

    await route.fulfill({
      response,
      json: {
        ...detail,
        events: [
          ...(detail.events ?? []),
          ...threads.map((thread, index) => {
            const line = thread.line ?? 3;
            const path = thread.path ?? "docs/preview.md";
            const side = thread.side ?? "right";
            const lineType = thread.lineType ?? (side === "left" ? "delete" : "add");
            const newLine = thread.newLine ?? (side === "right" ? line : undefined);
            const oldLine = thread.oldLine ?? (side === "left" ? line : undefined);
            const externalID = `e2e-rich-preview-${path}-${side}-${line}-${index}`;
            const threadID = `thread-rich-preview-${path}-${side}-${line}-${index}`;
            return {
              Author: "reviewer",
              Body: thread.body,
              CreatedAt: timestamp,
              DedupeKey: `review_comment:${externalID}`,
              DirectURL: "",
              EventType: "review_comment",
              ID: 900_001 + index,
              MergeRequestID: detail.merge_request?.ID ?? 1,
              MetadataJSON: "{}",
              PlatformExternalID: externalID,
              PlatformID: 900_001 + index,
              Resolvable: false,
              Resolved: false,
              Summary: thread.body,
              ThreadID: threadID,
              diff_thread: {
                author_login: "reviewer",
                body: thread.body,
                can_resolve: false,
                created_at: timestamp,
                diff_head_sha: detail.diff_head_sha,
                id: threadID,
                line,
                line_type: lineType,
                new_line: newLine,
                old_line: oldLine,
                path,
                provider_comment_id: externalID,
                resolved: false,
                side,
                updated_at: timestamp,
              },
            };
          }),
        ],
      },
    });
  });
}

async function mockDiffApiError(page: Page, status: number, detail: string): Promise<void> {
  await page.route("**/api/v1/pulls/github/acme/widgets/1/files", async (route) => {
    await route.fulfill({
      status,
      contentType: "application/problem+json",
      body: JSON.stringify({ status, code: "notFound", detail }),
    });
  });
  await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
    await route.fulfill({
      status,
      contentType: "application/problem+json",
      body: JSON.stringify({ status, code: "notFound", detail }),
    });
  });
}

async function navigateToDiff(page: Page): Promise<void> {
  await page.goto("/pulls/github/acme/widgets/1/files");
}

async function waitForDiffLoaded(page: Page): Promise<void> {
  await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });
}

async function waitForSidebarFilesLoaded(page: Page): Promise<void> {
  await treeFileItems(page).first().waitFor({ state: "visible", timeout: 10_000 });
}

async function visiblePierreLoadingCount(page: Page): Promise<number> {
  return await page.locator(".diff-file").evaluateAll((files) => {
    return files.filter((file) => {
      const rect = file.getBoundingClientRect();
      return rect.bottom > 0 && rect.top < window.innerHeight && file.querySelector(".pierre-diff-loading");
    }).length;
  });
}

async function renderedPierreDiffCount(page: Page): Promise<number> {
  return await page.locator(".diff-file .pierre-diff").evaluateAll((hosts) => {
    return hosts.filter((host) => {
      return host.shadowRoot?.querySelector("[data-content]") !== null;
    }).length;
  });
}

async function openDiffFilterMenu(page: Page): Promise<void> {
  await page.getByRole("button", { name: "More diff filters" }).click();
}

async function selectPierreReviewLine(file: Locator, line: number, side: "left" | "right"): Promise<void> {
  const type = side === "left" ? "change-deletion" : "change-addition";
  const fallback = side === "left" ? ["context"] : ["context", "context-expanded"];
  const selector = [
    `[data-column-number="${line}"][data-line-type="${type}"]`,
    ...fallback.map((lineType) => `[data-column-number="${line}"][data-line-type="${lineType}"]`),
  ].join(",");
  const target = file.locator(`.pierre-diff ${selector}`).first().locator("[data-line-number-content]");
  await expect(target).toBeVisible({ timeout: 10_000 });
  await target.press("Enter");
  await clickVisibleTarget(file.getByRole("button", { name: "Add comment to selected lines" }));
}

function inlineComposerFor(textarea: Locator): Locator {
  return textarea.locator(
    "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' inline-composer ')][1]",
  );
}

type BoundingBox = NonNullable<Awaited<ReturnType<Locator["boundingBox"]>>>;

async function visibleBoundingBox(target: Locator): Promise<BoundingBox> {
  let box: BoundingBox | null = null;
  await expect
    .poll(async () => {
      box = await target.boundingBox();
      return box !== null;
    })
    .toBe(true);
  return box!;
}

async function submitInlineComposer(textarea: Locator): Promise<void> {
  const composer = inlineComposerFor(textarea);
  const addButton = composer.getByRole("button", { name: "Add comment" });
  await expect(addButton).toBeEnabled();
  await addButton.click();
}

// --- Functional tests ---

test.describe("diff view", () => {
  test.beforeEach(async ({ page }) => {
    // Clear any persisted diff preferences so tests start clean.
    await page.addInitScript(() => {
      localStorage.removeItem("diff-tab-width");
      localStorage.removeItem("diff-hide-whitespace");
      localStorage.removeItem("diff-view-mode");
      localStorage.removeItem("diff-word-wrap");
      localStorage.removeItem("diff-rich-preview");
      localStorage.removeItem("diff-collapsed-files");
      localStorage.removeItem("diff-file-tree-width");
    });
  });

  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("renders diff with sidebar file list, toolbar, and file diffs", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // Sidebar inline file list shows all 4 files under the selected PR.
    await expect(treeFileItems(page)).toHaveCount(4);

    // Toolbar is visible.
    await expect(page.locator(".diff-toolbar")).toBeVisible();

    // All 4 diff file sections are rendered in the detail area.
    await expect(page.locator(".diff-file")).toHaveCount(4);
  });

  test("keeps repository and commit scope controls in the compact button family", async ({ page }) => {
    await navigateToDiff(page);

    const repository = page.getByRole("button", { name: /^Select repository:/ });
    const commitScope = page.getByRole("button", { name: /^Select commit range:/ });
    await expect(repository).toBeVisible();
    await expect(commitScope).toBeVisible();
    await expect(repository).toHaveClass(/\bkit-button\b/);
    await expect(commitScope).toHaveClass(/\bkit-button\b/);
    await expect(commitScope).toHaveText("HEAD");
    await expect(commitScope.locator(".diff-scope-picker__chevron")).toBeVisible();
    await expect(commitScope.locator("svg")).toHaveCount(2);

    const metrics = await page.evaluate(() => {
      const inspect = (selector: string) => {
        const element = document.querySelector<HTMLElement>(selector);
        const style = element ? getComputedStyle(element) : null;
        const rect = element?.getBoundingClientRect();
        return {
          height: rect?.height ?? 0,
          background: style?.backgroundColor ?? "",
          border: style?.border ?? "",
          radius: style?.borderRadius ?? "",
        };
      };

      return {
        repository: inspect(".typeahead-trigger"),
        commitScope: inspect(".diff-scope-picker__trigger"),
      };
    });

    expect(metrics.repository).toEqual(metrics.commitScope);
    expect(metrics.repository.height).toBe(26);
  });

  test("resizes and remembers the changed-file tree width", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const fileTree = page.getByRole("complementary", {
      name: "Changed files",
    });
    const resizeHandle = page.getByRole("separator", {
      name: "Resize file tree",
    });
    await expect(fileTree).toBeVisible();
    await expect(resizeHandle).toBeVisible();

    const beforeWidth = await fileTree.evaluate((el) => Number.parseInt(getComputedStyle(el).width, 10));
    const handleBox = await resizeHandle.boundingBox();
    expect(handleBox).not.toBeNull();

    await page.mouse.move(handleBox!.x + handleBox!.width / 2, handleBox!.y + handleBox!.height / 2);
    await page.mouse.down();
    await page.mouse.move(handleBox!.x + handleBox!.width / 2 + 80, handleBox!.y + handleBox!.height / 2);
    await page.mouse.up();

    await expect
      .poll(async () => fileTree.evaluate((el) => Number.parseInt(getComputedStyle(el).width, 10)))
      .toBeGreaterThanOrEqual(beforeWidth + 75);

    await expect
      .poll(async () => page.evaluate(() => localStorage.getItem("diff-file-tree-width")))
      .toBe(String(beforeWidth + 80));

    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect(page.locator(".diff-view")).toHaveCount(0);

    await page.getByRole("tab", { name: "Files changed" }).click();
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect
      .poll(async () => fileTree.evaluate((el) => Number.parseInt(getComputedStyle(el).width, 10)))
      .toBe(beforeWidth + 80);
  });

  test("collapses and restores the changed-file tree", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const fileTree = page.getByRole("complementary", {
      name: "Changed files",
    });
    const resizeHandle = page.getByRole("separator", {
      name: "Resize file tree",
    });
    const fileTreeControls = fileTree.locator(".diff-files-controls");
    await expect(fileTreeControls.getByRole("searchbox", { name: "Filter files" })).toBeVisible();
    const initialWidth = await fileTree.evaluate((element) => element.getBoundingClientRect().width);

    await fileTreeControls.getByRole("button", { name: "Collapse file tree" }).click();

    await expect(fileTree).toBeHidden();
    await expect(resizeHandle).toBeHidden();

    await page.getByRole("button", { name: "Expand file tree" }).click();

    await expect(fileTree).toBeVisible();
    await expect(resizeHandle).toBeVisible();
    await expect
      .poll(async () => fileTree.evaluate((element) => element.getBoundingClientRect().width))
      .toBe(initialWidth);
  });

  test("clamps persisted file tree width inside narrow split panes", async ({ page }) => {
    await page.setViewportSize({ width: 780, height: 720 });
    await page.addInitScript(() => {
      localStorage.setItem("diff-file-tree-width", "520");
    });
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const diffPane = page.locator(".files-main");

    await expect
      .poll(async () => {
        const box = await diffPane.boundingBox();
        return Math.floor(box?.width ?? 0);
      })
      .toBeGreaterThanOrEqual(320);

    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect.poll(async () => page.evaluate(() => localStorage.getItem("diff-file-tree-width"))).toBe("520");
  });

  test("sidebar file list shows status indicators", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect(treeFileItem(page, "internal/server/handler.go")).toHaveAttribute("data-item-git-status", "modified");
    await expect(treeFileItem(page, "frontend/src/lib/utils/format.ts")).toHaveAttribute(
      "data-item-git-status",
      "added",
    );
    await expect(treeFileItem(page, "internal/legacy/old_handler.go")).toHaveAttribute(
      "data-item-git-status",
      "deleted",
    );
    await expect(treeFileItem(page, "assets/logo.png")).toHaveAttribute("data-item-git-status", "modified");
    const statusLanes = await treeFileItem(page, "internal/server/handler.go").evaluate((node) =>
      Array.from(node.querySelectorAll("[data-item-section='git'], [data-item-section='decoration']"))
        .map((section) => section.textContent?.trim())
        .filter(Boolean),
    );
    expect(statusLanes).toEqual(["M"]);
  });

  test("hunk-only added-file patches render added file content", async ({ page }) => {
    const hunkOnlyLines = [
      "export function formatDate(d: Date): string {",
      "  const year = d.getFullYear();",
      "  const month = String(d.getMonth() + 1).padStart(2, '0');",
      "  const day = String(d.getDate()).padStart(2, '0');",
      "  return `${year}-${month}-${day}`;",
      "}",
      "",
      "export function formatNumber(n: number): string {",
      "export const padded = true \t",
    ];
    const hunkOnlyAddedDiff: DiffResult = {
      ...smallDiff,
      files: smallDiff.files.map((file) =>
        file.path === "frontend/src/lib/utils/format.ts"
          ? {
              ...file,
              additions: hunkOnlyLines.length,
              patch: [`@@ -0,0 +1,${hunkOnlyLines.length} @@`, ...hunkOnlyLines.map((line) => `+${line}`), ""].join(
                "\n",
              ),
              hunks: [
                {
                  old_start: 0,
                  old_count: 0,
                  new_start: 1,
                  new_count: hunkOnlyLines.length,
                  lines: hunkOnlyLines.map((content, index) => ({
                    type: "add" as const,
                    content,
                    new_num: index + 1,
                  })),
                },
              ],
            }
          : file,
      ),
    };
    await mockDiffApi(page, hunkOnlyAddedDiff);

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const addedFile = page.locator('[data-file-path="frontend/src/lib/utils/format.ts"]');
    await clickTreeFileItem(page, "frontend/src/lib/utils/format.ts");
    await addedFile.scrollIntoViewIfNeeded();
    await expectPierreDiffFirstText(addedFile, diffAdditionsSelector, "export function");
    await expectPierreDiffVisibleExactText(addedFile, diffAdditionsSelector, "export const padded = true \t");
    await expectPierreDiffCount(addedFile, diffAdditionsSelector, hunkOnlyLines.length);
    await expectPierreDiffCount(addedFile, diffDeletionsSelector, 0);
    await expectPierreDiffCount(addedFile, diffContextSelector, 0);
  });

  test("hunk-only added-file patches render when structured hunks are absent", async ({ page }) => {
    const hunkOnlyLines = [
      "export function fromPatchOnly(): string {",
      "  return 'rendered from patch';",
      "}",
      "export const padded = true \t",
    ];
    const patchOnlyAddedDiff: DiffResult = {
      ...smallDiff,
      files: smallDiff.files.map((file) =>
        file.path === "frontend/src/lib/utils/format.ts"
          ? {
              ...file,
              additions: hunkOnlyLines.length,
              patch: [`@@ -0,0 +1,${hunkOnlyLines.length} @@`, ...hunkOnlyLines.map((line) => `+${line}`), ""].join(
                "\n",
              ),
              hunks: [],
            }
          : file,
      ),
    };
    await mockDiffApi(page, patchOnlyAddedDiff);

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const addedFile = page.locator('[data-file-path="frontend/src/lib/utils/format.ts"]');
    await clickTreeFileItem(page, "frontend/src/lib/utils/format.ts");
    await addedFile.scrollIntoViewIfNeeded();
    await expect(addedFile.getByText("No textual changes")).toHaveCount(0);
    await expectPierreDiffFirstText(addedFile, diffAdditionsSelector, "fromPatchOnly");
    await expectPierreDiffVisibleExactText(addedFile, diffAdditionsSelector, "export const padded = true \t");
    await expectPierreDiffCount(addedFile, diffAdditionsSelector, hunkOnlyLines.length);
    await expectPierreDiffCount(addedFile, diffDeletionsSelector, 0);
    await expectPierreDiffCount(addedFile, diffContextSelector, 0);
  });

  test("complete added Go file patches render through the syntax-enabled browser path", async ({ page }) => {
    await page.addInitScript(() => {
      (globalThis as { __kenn_forgeForceSyntaxHighlight?: boolean }).__kenn_forgeForceSyntaxHighlight = true;
      const nativeWorker = window.Worker;
      (window as typeof window & { __kenn_forgeWorkerUrls?: string[] }).__kenn_forgeWorkerUrls = [];
      window.Worker = class extends nativeWorker {
        constructor(scriptURL: string | URL, options?: WorkerOptions) {
          (
            window as typeof window & {
              __kenn_forgeWorkerUrls: string[];
            }
          ).__kenn_forgeWorkerUrls.push(String(scriptURL));
          super(scriptURL, options);
        }
      } as typeof Worker;
    });

    const path = "internal/hosted/roborev/webhook_secret_resolver_test.go";
    const lines = [
      "package roborev_test",
      "",
      "import (",
      '\t"context"',
      '\t"testing"',
      '\t"time"',
      "",
      '\t"github.com/google/uuid"',
      '\t"github.com/stretchr/testify/require"',
      "",
      '\t"go.kenn.io/platform/internal/hosted/roborev"',
      '\t"go.kenn.io/platform/internal/hosted/roborev/secrets"',
      ")",
      "",
      "func TestResolveWebhookSecretsForVerificationReturnsMaterialForActiveConnection(t *testing.T) {",
      "\tctx := context.Background()",
      "\torgID := uuid.New()",
      "\tnow := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)",
      "\tprepare := func(ctx context.Context, store *roborev.DBStore, secretStore *secrets.LocalStore, orgID uuid.UUID, repoID string, connectionID string, secretRef string, now time.Time) roborev.SetRepoWebhookSecretRefParams {",
      "\t\treturn roborev.SetRepoWebhookSecretRefParams{",
      "\t\t\tOrgID: orgID,",
      "\t\t}",
      "\t}",
      "\trequire.NotNil(t, prepare)",
      "}",
    ];
    const addedGoDiff = withServerDiffData({
      ...smallDiff,
      files: [
        {
          path,
          old_path: path,
          status: "added",
          is_binary: false,
          is_generated: false,
          is_whitespace_only: false,
          additions: lines.length,
          deletions: 0,
          patch: [
            `diff --git a/${path} b/${path}`,
            "new file mode 100644",
            "--- /dev/null",
            `+++ b/${path}`,
            `@@ -0,0 +1,${lines.length} @@`,
            ...lines.map((line) => `+${line}`),
            "",
          ].join("\n"),
          hunks: [
            {
              old_start: 0,
              old_count: 0,
              new_start: 1,
              new_count: lines.length,
              lines: lines.map((content, index) => ({
                type: "add" as const,
                content,
                new_num: index + 1,
              })),
            },
          ],
        },
      ],
    });

    await mockDiffApi(page, addedGoDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const addedFile = page.locator(`[data-file-path="${path}"]`);
    await addedFile.scrollIntoViewIfNeeded();
    await expect
      .poll(
        async () => {
          return await page.evaluate(() => {
            return (
              (window as typeof window & { __kenn_forgeWorkerUrls?: string[] }).__kenn_forgeWorkerUrls?.length ?? 0
            );
          });
        },
        { timeout: 10_000 },
      )
      .toBeGreaterThan(0);
    await expectPierreDiffVisibleText(addedFile, diffAdditionsSelector, "package roborev_test");
    await expectPierreDiffVisibleText(addedFile, diffAdditionsSelector, "prepare := func");
    await expectPierreDiffCount(addedFile, diffAdditionsSelector, lines.length);
    await expectPierreDiffCount(addedFile, diffDeletionsSelector, 0);
    await expectPierreDiffCount(addedFile, diffContextSelector, 0);
  });

  test("sidebar shows folders for grouped files", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect(page.locator('.diff-file-tree [data-item-path="internal/server/"]')).toBeVisible();
    await expect(page.locator('.diff-file-tree [data-item-path="frontend/src/lib/utils/"]')).toBeVisible();
    await expect(page.locator('.diff-file-tree [data-item-path="internal/legacy/"]')).toBeVisible();
    await expect(page.locator('.diff-file-tree [data-item-path="assets/"]')).toBeVisible();
  });

  test("clicking a sidebar file row highlights it as active", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const secondRow = treeFileItem(page, "frontend/src/lib/utils/format.ts");
    await secondRow.click();

    await expect(secondRow).toHaveAttribute("aria-selected", "true");
  });

  test("sidebar file jumps keep the outer detail frame pinned", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const mainArea = page.locator(".kit-sidebar-layout__main").filter({ visible: true });
    const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");
    await expect.poll(() => mainArea.evaluate((el) => Math.round(el.scrollTop))).toBe(0);

    await clickTreeFileItem(page, "src/pkg9/file_45.go");

    await expect(page.locator('[data-file-path="src/pkg9/file_45.go"]')).toBeVisible();
    await expect.poll(() => diffArea.evaluate((el) => Math.round(el.scrollTop))).toBeGreaterThan(0);
    await expect.poll(() => mainArea.evaluate((el) => Math.round(el.scrollTop))).toBe(0);
  });

  test("diff pane boundary wheel events do not scroll the outer detail frame", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const mainArea = page.locator(".kit-sidebar-layout__main").filter({ visible: true });
    const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");
    await page.evaluate(() => {
      document.documentElement.style.overflow = "hidden";
      document.body.style.overflow = "hidden";
    });
    await mainArea.evaluate((area) => {
      area.style.overflowY = "auto";
      area.style.overflowAnchor = "none";
      const filesView = area.firstElementChild;
      if (!(filesView instanceof HTMLElement)) throw new Error("missing files view");
      const frameHeight = Math.round(area.getBoundingClientRect().height);
      filesView.style.flex = `0 0 ${frameHeight}px`;
      filesView.style.minHeight = `${frameHeight}px`;
      const topSpacer = document.createElement("div");
      topSpacer.dataset.testid = "overscroll-top-spacer";
      topSpacer.setAttribute("aria-hidden", "true");
      topSpacer.style.flexShrink = "0";
      topSpacer.style.height = "180px";
      area.insertBefore(topSpacer, area.firstElementChild);
      const bottomSpacer = document.createElement("div");
      bottomSpacer.dataset.testid = "overscroll-bottom-spacer";
      bottomSpacer.setAttribute("aria-hidden", "true");
      bottomSpacer.style.flexShrink = "0";
      bottomSpacer.style.height = "1200px";
      area.appendChild(bottomSpacer);
    });
    await expect.poll(async () => mainArea.evaluate((area) => area.scrollHeight > area.clientHeight)).toBe(true);
    const pinnedFrameScrollTop = await mainArea.evaluate((area) => {
      area.scrollTop = 180;
      return Math.round(area.scrollTop);
    });
    expect(pinnedFrameScrollTop).toBeGreaterThan(0);
    await expect(diffArea).toBeVisible();

    await diffArea.evaluate((area) => {
      area.scrollTop = 0;
    });
    await diffArea.hover();
    await page.mouse.wheel(0, -900);
    await expect.poll(async () => mainArea.evaluate((area) => Math.round(area.scrollTop))).toBe(pinnedFrameScrollTop);

    const bottomScrollTop = await diffArea.evaluate((area) => {
      area.scrollTop = area.scrollHeight - area.clientHeight;
      return Math.round(area.scrollTop);
    });
    expect(bottomScrollTop).toBeGreaterThan(0);
    await diffArea.hover();
    await page.mouse.wheel(0, 900);
    await expect.poll(async () => mainArea.evaluate((area) => Math.round(area.scrollTop))).toBe(pinnedFrameScrollTop);
  });

  test("sidebar jump to the last file preserves expanded body space above it", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await clickTreeFileItem(page, "src/pkg9/file_49.go");
    await expect(page.locator('[data-file-path="src/pkg9/file_49.go"]')).toBeVisible();

    const earlierFile = page.locator('[data-file-path="src/pkg5/file_25.go"]');
    await expect(earlierFile.locator(".file-collapse-toggle")).toHaveAttribute("title", "Collapse file");
    await expect(earlierFile.locator(".file-content")).toBeAttached();
    await expect
      .poll(async () =>
        earlierFile.locator(".pierre-diff-shell").evaluate((el) => Math.round(el.getBoundingClientRect().height)),
      )
      .toBeGreaterThan(300);
  });

  test("manual paging and wheel scrolling override sidebar file jumps", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");

    await clickTreeFileItem(page, "src/pkg8/file_40.go");
    await expect(page.locator('[data-file-path="src/pkg8/file_40.go"]')).toBeVisible();
    const firstJumpTop = await diffArea.evaluate((area) => Math.round(area.scrollTop));

    await page.keyboard.press("PageDown");
    await expect
      .poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop)))
      .toBeGreaterThan(firstJumpTop + 100);
    await expect(page.locator(".diff-file-tree [data-item-type='file'][aria-selected='true']")).toBeInViewport();
    const afterPageDownTop = await diffArea.evaluate((area) => Math.round(area.scrollTop));
    await page.waitForTimeout(400);
    await expect
      .poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop)))
      .toBeGreaterThan(afterPageDownTop - 10);

    await clickTreeFileItem(page, "src/pkg8/file_41.go");
    await expect(page.locator('[data-file-path="src/pkg8/file_41.go"]')).toBeVisible();
    const secondJumpTop = await diffArea.evaluate((area) => Math.round(area.scrollTop));

    await diffArea.hover();
    await page.mouse.wheel(0, 900);
    await expect
      .poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop)))
      .toBeGreaterThan(secondJumpTop + 100);
    await expect(page.locator(".diff-file-tree [data-item-type='file'][aria-selected='true']")).toBeInViewport();
    const afterWheelTop = await diffArea.evaluate((area) => Math.round(area.scrollTop));
    await page.waitForTimeout(400);
    await expect
      .poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop)))
      .toBeGreaterThan(afterWheelTop - 10);
  });

  test("deleted file name has strikethrough in sidebar", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect(treeFileItem(page, "internal/legacy/old_handler.go")).toHaveAttribute(
      "data-item-git-status",
      "deleted",
    );
  });

  test("detail tabs switch between conversation and files views", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    // On the /files route the "Files changed" tab is active.
    const filesTab = page.getByRole("tab", { name: "Files changed" });
    await expect(filesTab).toHaveAttribute("aria-selected", "true");

    // Clicking "Conversation" navigates back to the PR detail.
    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect(page).toHaveURL(/\/pulls\/github\/acme\/widgets\/1$/);
  });

  test("clicking a file collapse control hides and restores its content", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator(".diff-file").first();
    const header = firstFile.locator(".file-header");
    const collapseToggle = firstFile.locator(".file-collapse-toggle");
    const content = firstFile.locator(".file-content");

    await expect(header.locator(".kit-diff-stats")).toHaveCount(1);
    const collapseBounds = await collapseToggle.boundingBox();
    expect(collapseBounds?.width).toBeGreaterThanOrEqual(24);
    expect(collapseBounds?.height).toBeGreaterThanOrEqual(24);

    // Content is initially visible.
    await expect(content).toBeVisible();

    // Collapse.
    await collapseToggle.click();
    await expect(content).not.toBeAttached();

    // Expand.
    await collapseToggle.click();
    await expect(content).toBeVisible();
  });

  test("clicking a file path copy icon writes the exact path to the clipboard", async ({
    page,
    context,
    browserName,
  }) => {
    test.skip(browserName !== "chromium", "Clipboard read assertions require Chromium permissions");
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const path = "internal/server/handler.go";
    const copyButton = page.locator(`.diff-file[data-file-path="${path}"] .file-path-copy`);
    await copyButton.click();

    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(path);
    await expect(copyButton).toHaveAttribute("title", "Copied!");
  });

  test("deleted file paths retain line-through", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const pathLabel = page.locator('.diff-file[data-file-path="internal/legacy/old_handler.go"] .file-path');
    const decorations = () => pathLabel.evaluate((element) => getComputedStyle(element).textDecorationLine.split(" "));

    await expect.poll(decorations).toContain("line-through");
  });

  test("more menu collapses and expands all visible diffs", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await expect(page.locator(".diff-file .file-content")).toHaveCount(4);

    await openDiffFilterMenu(page);
    await page.getByRole("button", { name: "Collapse all diffs" }).click();

    await expect(page.locator(".diff-file .file-content")).toHaveCount(0);
    await expect(page.locator(".diff-file .file-collapse-toggle[title='Expand file']")).toHaveCount(4);
    await expect(page.getByRole("button", { name: "Expand all diffs" })).toBeVisible();

    await page.getByRole("button", { name: "Expand all diffs" }).click();

    await expect(page.locator(".diff-file .file-content")).toHaveCount(4);
    await expect(page.locator(".diff-file .file-collapse-toggle[title='Collapse file']")).toHaveCount(4);
  });

  test("toolbar keeps file filters inline and moves display settings into the menu", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await expect(page.getByRole("group", { name: "Filter changed files" })).toBeVisible();
    await expect(page.getByRole("button", { name: "More diff filters" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Word wrap" })).toHaveCount(0);

    await openDiffFilterMenu(page);

    // Default tab width is 4.
    const handlerFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await expectPierreCodeTabSize(handlerFile, "4");
    const tabWidth = page.getByRole("group", { name: "Tab width" });
    await expect(tabWidth.getByRole("button", { name: "4" })).toHaveAttribute("aria-pressed", "true");

    // Click tab width 2.
    await tabWidth.getByRole("button", { name: "2" }).click();
    await expect(tabWidth.getByRole("button", { name: "2" })).toHaveAttribute("aria-pressed", "true");
    await expect(tabWidth.getByRole("button", { name: "4" })).toHaveAttribute("aria-pressed", "false");
    await expectPierreCodeTabSize(handlerFile, "2");
  });

  test("word wrap toggle changes diff line wrapping", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    // Scoped to the diff area: the conversation pane stays mounted behind the
    // files pane, and a review-thread snippet there renders its own always
    // wrapped pierre diff earlier in the DOM.
    const firstCodeLine = page.locator(".diff-area .pierre-diff [data-line]").first();

    await openDiffFilterMenu(page);
    const wrapToggle = page.getByRole("switch", { name: "Word wrap" });

    await expect(wrapToggle).not.toBeChecked();
    await expect(firstCodeLine).toHaveCSS("white-space", "pre");

    await wrapToggle.click();

    await expect(wrapToggle).toBeChecked();
    await expect(firstCodeLine).toHaveCSS("white-space", "pre-wrap");
  });

  test("scrolled-in Pierre files do not get stuck on loading placeholders", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await page.locator(".diff-area .kit-scrollbox__viewport").evaluate((area) => {
      area.scrollTop = area.scrollHeight * 0.55;
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    });

    await expect
      .poll(() => visiblePierreLoadingCount(page), {
        timeout: 10_000,
      })
      .toBe(0);
    await expectRenderedPierreContainmentDisabled(page);
  });

  test("rich preview toggle renders markdown and browser images", async ({ page }) => {
    await mockDiffApi(page, previewDiff);
    await mockFilePreviewApi(page);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await openDiffFilterMenu(page);
    const previewToggle = page.getByRole("switch", {
      name: "Rich preview",
    });
    await expect(previewToggle).not.toBeChecked();

    await previewToggle.click();
    await expect(previewToggle).toBeChecked();
    const markdownPreview = page.locator(".markdown-rich-diff--unified");
    await expect(markdownPreview.getByRole("heading", { name: "Rendered preview" })).toBeVisible();
    await expect(markdownPreview).toContainText("Markdown task");
    await expect(markdownPreview).toContainText("paragraph that should be highlighted.");
    await expect(markdownPreview.locator("del", { hasText: "Old" })).toBeVisible();
    await expect(markdownPreview.locator("ins", { hasText: "New" })).toBeVisible();
    await expect(markdownPreview.locator("del em", { hasText: "alpha" })).toBeVisible();
    await expect(markdownPreview.locator("ins a", { hasText: "link" })).toBeVisible();
    await expect(markdownPreview.locator("strong del", { hasText: "beta" })).toBeVisible();
    await expect(markdownPreview.locator("strong ins", { hasText: "two" })).toBeVisible();
    await expect(markdownPreview.locator("code del", { hasText: "gamma" })).toBeVisible();
    await expect(markdownPreview.locator("code ins", { hasText: "three" })).toBeVisible();
    await expect(markdownPreview.locator("tr > ins")).toHaveCount(0);
    await expect(markdownPreview.locator('td[data-diff-kind="insert"] ins', { hasText: "Added cell" })).toBeVisible();

    const handlerFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await handlerFile.scrollIntoViewIfNeeded();
    await expectPierreDiffFirstVisible(handlerFile, diffDeletionsSelector);
    await expectPierreDiffFirstVisible(handlerFile, diffAdditionsSelector);
    await expect(handlerFile.locator(".diff-text-preview")).toHaveCount(0);

    await clickTreeFileItem(page, "assets/logo.png");
    await expect(page.locator(".kit-image-preview img[alt='assets/logo.png']")).toBeVisible();
  });

  test("rich preview side-by-side panes do not underline changed text", async ({ page }) => {
    await page.setViewportSize({ width: 2200, height: 1000 });
    await mockDiffApi(page, splitRichPreviewDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();
    await page.getByRole("switch", { name: "Side-by-side diffs" }).click();

    const markdownPreview = page.locator('[data-file-path="docs/preview.md"] .markdown-rich-diff--split');
    await expect(markdownPreview).toBeVisible();
    const beforeChange = markdownPreview
      .locator('[aria-label="Before markdown preview"] del')
      .filter({ hasText: "Old" })
      .first();
    const afterChange = markdownPreview
      .locator('[aria-label="After markdown preview"] ins')
      .filter({ hasText: "New" })
      .first();
    await expect(beforeChange).toBeVisible();
    await expect(afterChange).toBeVisible();
    await expect
      .poll(() => beforeChange.evaluate((element) => getComputedStyle(element).textDecorationLine))
      .toBe("none");
    await expect
      .poll(() => afterChange.evaluate((element) => getComputedStyle(element).textDecorationLine))
      .toBe("none");
    const blockFillPreview = page.locator('[data-file-path="docs/block-fill.md"] .markdown-rich-diff--split');
    await expect(blockFillPreview).toBeVisible();
    const deletedBlock = blockFillPreview
      .locator('[aria-label="Before markdown preview"] del.markdown-diff__block')
      .filter({ hasText: "Removed standalone block" })
      .first();
    await expect(deletedBlock).toBeVisible();
    await expect
      .poll(() =>
        deletedBlock.evaluate((element) => {
          const textElement = element.querySelector("*") ?? element;
          return getComputedStyle(textElement).textDecorationLine;
        }),
      )
      .toBe("none");
    const inlineBeforeChange = markdownPreview
      .locator('[aria-label="Before markdown preview"] strong del')
      .filter({ hasText: "beta" })
      .first();
    const inlineAfterChange = markdownPreview
      .locator('[aria-label="After markdown preview"] strong ins')
      .filter({ hasText: "two" })
      .first();
    await expect(inlineBeforeChange).toBeVisible();
    await expect(inlineAfterChange).toBeVisible();
    await expect
      .poll(() => inlineBeforeChange.evaluate((element) => getComputedStyle(element).backgroundColor))
      .not.toBe("rgba(0, 0, 0, 0)");
    await expect
      .poll(() => inlineAfterChange.evaluate((element) => getComputedStyle(element).backgroundColor))
      .not.toBe("rgba(0, 0, 0, 0)");
    await expect
      .poll(() => markdownPreview.evaluate((element) => element.getBoundingClientRect().width))
      .toBeGreaterThan(1300);
  });

  test("rich preview gives standalone block additions and deletions visible fills", async ({ page }) => {
    await mockDiffApi(page, blockFillMarkdownDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownPreview = page.locator('[data-file-path="docs/block-fill.md"] .markdown-rich-diff--unified');
    await expect(markdownPreview).toBeVisible();
    await expectNonTransparentBackground(
      markdownPreview.locator("del.markdown-diff__block").filter({
        hasText: "Removed standalone block that should have a filled background.",
      }),
    );
    await expectNonTransparentBackground(
      markdownPreview.locator("ins.markdown-diff__block").filter({
        hasText: "Added standalone block that should have a filled background.",
      }),
    );
  });

  test("rich preview shows review thread cards", async ({ page }) => {
    const reviewBody = "Review note should show in rich preview";
    await mockDiffApi(page, previewDiff);
    await mockFilePreviewApi(page);
    await mockReviewThreadOnPreviewMarkdown(page, reviewBody);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownFile = page.locator('[data-file-path="docs/preview.md"]');
    await expect(markdownFile.locator(".markdown-rich-diff--unified")).toBeVisible();
    const reviewCard = markdownFile
      .locator(".markdown-rich-diff--unified .inline-review-thread")
      .filter({ hasText: reviewBody });
    await expect(reviewCard).toBeVisible();
    await expect
      .poll(() =>
        reviewCard.evaluate(
          (element) =>
            element.previousElementSibling?.classList.contains("markdown-rich-diff__anchored-block") ?? false,
        ),
      )
      .toBe(true);
    await expect
      .poll(() =>
        reviewCard.evaluate((element) => {
          const preview = element.closest(".markdown-rich-diff--unified");
          if (!preview) return false;
          const cardRect = element.getBoundingClientRect();
          const previewRect = preview.getBoundingClientRect();
          return cardRect.right <= previewRect.right + 1;
        }),
      )
      .toBe(true);
  });

  test("rich preview anchors multiple list review cards to their source items", async ({ page }) => {
    await mockDiffApi(page, listReviewPreviewDiff);
    await mockReviewThreadsOnPreviewMarkdown(page, [
      { body: "Actions review note", line: 2, path: "docs/list-review.md" },
      { body: "Issues review note", line: 1, path: "docs/list-review.md" },
    ]);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownFile = page.locator('[data-file-path="docs/list-review.md"]');
    await expect(markdownFile.locator(".markdown-rich-diff--unified")).toBeVisible();
    await expect(markdownFile.locator(".markdown-rich-diff__split-list")).toHaveCount(3);
    await expect
      .poll(() =>
        markdownFile.locator(".markdown-rich-diff__split-list").evaluateAll((lists) =>
          lists.every((list) => {
            const style = getComputedStyle(list);
            return style.marginTop === "0px" && style.marginBottom === "0px";
          }),
        ),
      )
      .toBe(true);
    await expect
      .poll(() =>
        markdownFile
          .locator(".markdown-diff__block")
          .evaluateAll(
            (blocks) => blocks.filter((block) => block.querySelector(".markdown-rich-diff__split-list")).length,
          ),
      )
      .toBeGreaterThan(0);
    await expect
      .poll(() =>
        markdownFile.locator(".markdown-diff__block").evaluateAll((blocks) =>
          blocks
            .filter((block) => block.querySelector(".markdown-rich-diff__split-list"))
            .every((block) => {
              const style = getComputedStyle(block);
              return (
                style.marginTop === "0px" &&
                style.marginBottom === "0px" &&
                style.paddingTop === "0px" &&
                style.paddingBottom === "0px"
              );
            }),
        ),
      )
      .toBe(true);
    await expect
      .poll(() => markdownFile.locator(".markdown-rich-diff--unified .review-thread-body").allTextContents())
      .toEqual(["Issues review note", "Actions review note"]);

    const issuesCard = markdownFile
      .locator(".markdown-rich-diff--unified .inline-review-thread")
      .filter({ hasText: "Issues review note" });
    const actionsCard = markdownFile
      .locator(".markdown-rich-diff--unified .inline-review-thread")
      .filter({ hasText: "Actions review note" });
    await expect
      .poll(() => issuesCard.evaluate((element) => element.previousElementSibling?.textContent?.trim() ?? ""))
      .toContain("Issues");
    await expect
      .poll(() => actionsCard.evaluate((element) => element.previousElementSibling?.textContent?.trim() ?? ""))
      .toContain("Actions");
    await expect
      .poll(() => markdownFile.locator(".markdown-rich-diff__split-list").last().textContent())
      .toContain("Statuses");
    await expect
      .poll(() => markdownFile.locator(".markdown-rich-diff__split-list").last().textContent())
      .toContain("Members");
  });

  test("rich preview keeps untargeted lists intact when a later block has a review card", async ({ page }) => {
    const reviewBody = "Paragraph review note";
    await mockDiffApi(page, untargetedListReviewPreviewDiff);
    await mockReviewThreadOnPreviewMarkdown(page, reviewBody, 5, "docs/untargeted-list-review.md");
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownPreview = page.locator(
      '[data-file-path="docs/untargeted-list-review.md"] .markdown-rich-diff--unified',
    );
    await expect(markdownPreview).toBeVisible();
    await expect(markdownPreview.locator("ul")).toHaveCount(1);
    await expect(markdownPreview.locator("ul.markdown-rich-diff__split-list")).toHaveCount(0);

    const reviewCard = markdownPreview.locator(".inline-review-thread").filter({ hasText: reviewBody });
    await expect(reviewCard).toBeVisible();
    await expect
      .poll(() => reviewCard.evaluate((element) => element.previousElementSibling?.textContent?.trim() ?? ""))
      .toContain("Paragraph with review note.");
  });

  test("rich preview keeps unchanged list siblings aligned around an added item review card", async ({ page }) => {
    const reviewBody = "Added list item review note";
    await mockDiffApi(page, changedListReviewPreviewDiff);
    await mockReviewThreadOnPreviewMarkdown(page, reviewBody, 2, "docs/changed-list-review.md");
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownPreview = page.locator('[data-file-path="docs/changed-list-review.md"] .markdown-rich-diff--unified');
    await expect(markdownPreview).toBeVisible();
    await expect(markdownPreview.locator("ins", { hasText: "Actions" })).toBeVisible();
    await expect(markdownPreview.locator("ins", { hasText: "Issues" })).toHaveCount(0);
    await expect(markdownPreview.locator("del", { hasText: "Statuses" })).toHaveCount(0);

    const reviewCard = markdownPreview.locator(".inline-review-thread").filter({ hasText: reviewBody });
    await expect(reviewCard).toBeVisible();
    await expect
      .poll(() => reviewCard.evaluate((element) => element.previousElementSibling?.textContent?.trim() ?? ""))
      .toContain("Actions");
  });

  test("rich preview keeps unchanged list siblings aligned for edge added and deleted item comments", async ({
    page,
  }) => {
    await mockDiffApi(page, edgeListReviewPreviewDiff);
    await mockReviewThreadsOnPreviewMarkdown(page, [
      {
        body: "Prepended item review note",
        line: 1,
        path: "docs/edge-added-list-review.md",
      },
      {
        body: "Appended item review note",
        line: 5,
        path: "docs/edge-added-list-review.md",
      },
      {
        body: "Deleted first item review note",
        line: 1,
        oldLine: 1,
        path: "docs/edge-deleted-list-review.md",
        side: "left",
      },
      {
        body: "Deleted last item review note",
        line: 5,
        oldLine: 5,
        path: "docs/edge-deleted-list-review.md",
        side: "left",
      },
    ]);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const addedPreview = page.locator('[data-file-path="docs/edge-added-list-review.md"] .markdown-rich-diff--unified');
    await expect(addedPreview).toBeVisible();
    await expect(addedPreview.locator("ins", { hasText: "Prepended" })).toBeVisible();
    await expect(addedPreview.locator("ins", { hasText: "Appended" })).toBeVisible();
    await expect(addedPreview.locator("ins", { hasText: "Issues" })).toHaveCount(0);
    await expect(addedPreview.locator("ins", { hasText: "Statuses" })).toHaveCount(0);
    await expect(addedPreview.locator("del", { hasText: "Issues" })).toHaveCount(0);
    await expect(addedPreview.locator("del", { hasText: "Statuses" })).toHaveCount(0);
    await expect(addedPreview.locator(".inline-review-thread").filter({ hasText: "Prepended item" })).toBeVisible();
    await expect(addedPreview.locator(".inline-review-thread").filter({ hasText: "Appended item" })).toBeVisible();

    const deletedPreview = page.locator(
      '[data-file-path="docs/edge-deleted-list-review.md"] .markdown-rich-diff--unified',
    );
    await expect(deletedPreview).toBeVisible();
    await expect(deletedPreview.locator("del", { hasText: "Removed first" })).toBeVisible();
    await expect(deletedPreview.locator("del", { hasText: "Removed last" })).toBeVisible();
    await expect(deletedPreview.locator("del", { hasText: "Issues" })).toHaveCount(0);
    await expect(deletedPreview.locator("del", { hasText: "Statuses" })).toHaveCount(0);
    await expect(deletedPreview.locator("ins", { hasText: "Issues" })).toHaveCount(0);
    await expect(deletedPreview.locator("ins", { hasText: "Statuses" })).toHaveCount(0);
    await expect(deletedPreview.locator(".inline-review-thread").filter({ hasText: "Deleted first" })).toBeVisible();
    await expect(deletedPreview.locator(".inline-review-thread").filter({ hasText: "Deleted last" })).toBeVisible();
  });

  test("rich preview keeps unmapped review thread cards visible as line-unavailable fallback", async ({ page }) => {
    const reviewBody = "Unmapped rich preview note should stay visible";
    await mockDiffApi(page, previewDiff);
    await mockFilePreviewApi(page);
    await mockReviewThreadOnPreviewMarkdown(page, reviewBody, 99);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownFile = page.locator('[data-file-path="docs/preview.md"]');
    await expect(markdownFile.locator(".markdown-rich-diff--unified")).toBeVisible();
    const fallbackCard = markdownFile.locator(".preview-shell > .inline-review-thread").filter({
      hasText: reviewBody,
    });
    await expect(fallbackCard).toBeVisible();
    await expect(fallbackCard).toHaveClass(/inline-review-thread--file-level/);
    await expect(fallbackCard).toContainText("Line unavailable");
    await expect(
      markdownFile.locator(".markdown-rich-diff--unified .inline-review-thread").filter({ hasText: reviewBody }),
    ).toHaveCount(0);
    await expect(
      markdownFile.locator(".markdown-rich-diff--unified ~ .inline-review-thread").filter({ hasText: reviewBody }),
    ).toHaveCount(1);
  });

  test("rich preview hides synthetic hunk separators inside spanning markdown blocks", async ({ page }) => {
    await mockDiffApi(page, multiHunkMarkdownDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownFile = page.locator('[data-file-path="docs/multihunk.md"]');
    const markdownPreview = markdownFile.locator(".markdown-rich-diff--unified");
    await expect(markdownPreview).toBeVisible();
    const codeBlock = markdownPreview.locator("code").filter({ hasText: "first hunk code" });
    await expect(codeBlock).toBeVisible();
    await expect(codeBlock).toContainText("second hunk code");
    await expect(codeBlock).toContainText("added hunk code");
    await expect(codeBlock).not.toContainText("---");
    await expect(markdownPreview.locator("hr")).toHaveCount(0);
  });

  test("rich preview keeps hidden hunk-gap review threads as line-unavailable fallback", async ({ page }) => {
    const reviewBody = "Hidden hunk gap review note should stay fallback";
    await mockDiffApi(page, multiHunkMarkdownDiff);
    await mockReviewThreadOnPreviewMarkdown(page, reviewBody, 6, "docs/multihunk.md");
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();

    const markdownFile = page.locator('[data-file-path="docs/multihunk.md"]');
    const fallbackCard = markdownFile.locator(".preview-shell > .inline-review-thread").filter({
      hasText: reviewBody,
    });
    await expect(fallbackCard).toBeVisible();
    await expect(fallbackCard).toHaveClass(/inline-review-thread--file-level/);
    await expect(fallbackCard).toContainText("Line unavailable");
    await expect(
      markdownFile.locator(".markdown-rich-diff--unified .inline-review-thread").filter({ hasText: reviewBody }),
    ).toHaveCount(0);
    await expect(
      markdownFile.locator(".markdown-rich-diff--unified ~ .inline-review-thread").filter({ hasText: reviewBody }),
    ).toHaveCount(1);
  });

  test("rich preview refetches blob content after a same-PR diff reload", async ({ page }) => {
    const firstLogo =
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==";
    const secondLogo =
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYPj/HwADAgH/5ncLrgAAAABJRU5ErkJggg==";
    const logoResponses = [firstLogo, secondLogo];
    let diffFetchCount = 0;
    let previewFetchCount = 0;

    await page.route("**/api/v1/pulls/github/acme/widgets/1/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(smallDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
      diffFetchCount++;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(smallDiff as DiffFixture)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
      const content = logoResponses[Math.min(previewFetchCount, logoResponses.length - 1)]!;
      previewFetchCount++;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          path: "assets/logo.png",
          media_type: "image/png",
          encoding: "base64",
          content,
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Rich preview" }).click();
    await clickTreeFileItem(page, "assets/logo.png");

    const image = page.locator(".kit-image-preview img[alt='assets/logo.png']");
    await expect(image).toHaveAttribute("src", `data:image/png;base64,${firstLogo}`);
    expect(previewFetchCount).toBe(1);

    const initialDiffFetchCount = diffFetchCount;
    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Hide whitespace changes" }).click();
    await expect.poll(() => diffFetchCount).toBeGreaterThan(initialDiffFetchCount);
    await clickTreeFileItem(page, "assets/logo.png");

    await expect.poll(() => previewFetchCount).toBe(2);
    await expect(image).toHaveAttribute("src", `data:image/png;base64,${secondLogo}`);
  });

  test("changed file category filter narrows the sidebar and rendered diff", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect(page.locator(".files-view > .diff-toolbar")).toBeVisible();

    const categoryFilter = page.getByRole("group", {
      name: "Filter changed files",
    });
    await expect(categoryFilter.getByRole("button", { name: "Plans/docs (0)" })).toBeVisible();
    await expect(categoryFilter.getByRole("button", { name: "Code (3)" })).toBeVisible();
    await expect(categoryFilter.getByRole("button", { name: "Tests (0)" })).toBeVisible();
    await expect(categoryFilter.getByRole("button", { name: "Other (1)" })).toBeVisible();
    await expect(categoryFilter.getByRole("button", { name: "All (4)" })).toHaveAttribute("aria-pressed", "true");

    await categoryFilter.getByRole("button", { name: "Code (3)" }).click();

    await expect(page.locator(".diff-file")).toHaveCount(3);
    await expect(treeFileItems(page)).toHaveCount(3);
    await expect(page.locator(".diff-file", { hasText: "assets/logo.png" })).toHaveCount(0);
    await expect(treeFileItem(page, "assets/logo.png")).toHaveCount(0);

    await expect(categoryFilter.getByRole("button", { name: "Code (3)" })).toHaveAttribute("aria-pressed", "true");
    await categoryFilter.getByRole("button", { name: "All (4)" }).click();

    await expect(page.locator(".diff-file")).toHaveCount(4);
    await expect(treeFileItems(page)).toHaveCount(4);
  });

  test("hide whitespace toggle triggers re-fetch", async ({ page }) => {
    let fetchCount = 0;
    await page.route("**/api/v1/pulls/github/acme/widgets/1/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(smallDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
      fetchCount++;
      const url = new URL(route.request().url());
      const fixture =
        url.searchParams.get("whitespace") === "hide" ? { ...smallDiff, whitespace_only_count: 1 } : smallDiff;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(fixture as DiffFixture)),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    const initialCount = fetchCount;

    // Toggle hide whitespace on.
    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Hide whitespace changes" }).click();

    // Wait for the re-fetch to land and assert it actually happened.
    await expect.poll(() => fetchCount).toBeGreaterThan(initialCount);
  });

  test("side-by-side toggle renders Pierre split diffs", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await firstFile.scrollIntoViewIfNeeded();

    await openDiffFilterMenu(page);
    const sideBySide = page.getByRole("switch", {
      name: "Side-by-side diffs",
    });
    await expect(sideBySide).not.toBeChecked();
    await sideBySide.click();
    await expect(sideBySide).toBeChecked();
    await expect
      .poll(async () => {
        return await firstFile.locator(".pierre-diff").evaluate((host) => {
          return host.shadowRoot?.querySelector("pre")?.getAttribute("data-diff-type");
        });
      })
      .toBe("split");
  });

  test("j/k keyboard navigation moves between files", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const expectedFileOrder = [
      "assets/logo.png",
      "frontend/src/lib/utils/format.ts",
      "internal/legacy/old_handler.go",
      "internal/server/handler.go",
    ];
    await expect.poll(() => treeFileItemPaths(page)).toEqual(expectedFileOrder);
    await expect.poll(() => renderedDiffFilePaths(page)).toEqual(expectedFileOrder);

    await expect(treeFileItem(page, "assets/logo.png")).toHaveAttribute("aria-selected", "true");

    // Press j to move to next file.
    await page.keyboard.press("j");
    await expect(treeFileItem(page, "frontend/src/lib/utils/format.ts")).toHaveAttribute("aria-selected", "true", {
      timeout: 2_000,
    });

    // Press j again.
    await page.keyboard.press("j");
    await expect(treeFileItem(page, "internal/legacy/old_handler.go")).toHaveAttribute("aria-selected", "true", {
      timeout: 2_000,
    });

    // Press k to move back.
    await page.keyboard.press("k");
    await expect(treeFileItem(page, "frontend/src/lib/utils/format.ts")).toHaveAttribute("aria-selected", "true", {
      timeout: 2_000,
    });
  });

  test("rendered diff order matches backend file order and the tree", async ({ page }) => {
    const orderedDiff: DiffResult = withServerDiffData({
      stale: false,
      whitespace_only_count: 0,
      files: [
        makeSingleLineFixtureFile("internal/server/config_reload_test.go"),
        makeSingleLineFixtureFile("internal/server/e2etest/settings_test.go"),
        makeSingleLineFixtureFile("internal/server/config_reload.go"),
        makeSingleLineFixtureFile("internal/server/api_types.go"),
      ],
    });
    await mockDiffApi(page, orderedDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const expectedFileOrder = [
      "internal/server/e2etest/settings_test.go",
      "internal/server/api_types.go",
      "internal/server/config_reload.go",
      "internal/server/config_reload_test.go",
    ];
    await expect.poll(() => treeFileItemPaths(page)).toEqual(expectedFileOrder);
    await expect.poll(() => renderedDiffFilePaths(page)).toEqual(expectedFileOrder);
  });

  test("stale diff warning occupies space only while visible", async ({ page }) => {
    let fixture = staleDiff;
    await mockDiffApi(page, () => fixture);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const banner = page.locator(".stale-banner");
    const diffBody = page.locator(".diff-body");
    const firstFile = page.locator(".diff-file").first();
    const firstHeader = firstFile.locator(".file-header");
    const firstContent = firstFile.locator(".file-content");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("outdated");
    const staleBounds = await diffBody.boundingBox();
    expect(staleBounds).not.toBeNull();

    const bannerBounds = await visibleBoundingBox(banner);
    const headerBounds = await visibleBoundingBox(firstHeader);
    expect(headerBounds.y).toBeGreaterThanOrEqual(bannerBounds.y + bannerBounds.height);

    await expect(firstContent).toBeAttached();
    await firstFile.locator(".file-collapse-toggle").click();
    await expect(firstContent).not.toBeAttached();

    fixture = smallDiff;
    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Hide whitespace changes" }).click();
    await expect(banner).toHaveCount(0);

    const freshBounds = await diffBody.boundingBox();
    expect(freshBounds).not.toBeNull();
    expect(freshBounds!.y).toBeLessThan(staleBounds!.y);
  });

  test("stale diff warning enters the layout when the initial stale diff loads", async ({ page }) => {
    let releaseDiffResponse: () => void = () => {};
    const diffResponseReleased = new Promise<void>((resolve) => {
      releaseDiffResponse = resolve;
    });
    await mockDiffApi(page, staleDiff);
    await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
      await diffResponseReleased;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(staleDiff)),
      });
    });
    await navigateToDiff(page);

    const diffBody = page.locator(".diff-body");
    await expect(page.locator(".diff-state-msg")).toHaveText("Loading diff");
    const loadingBounds = await diffBody.boundingBox();
    expect(loadingBounds).not.toBeNull();

    releaseDiffResponse();
    await waitForDiffLoaded(page);
    await expect(page.locator(".stale-banner")).toBeVisible();
    const staleBounds = await diffBody.boundingBox();
    expect(staleBounds).not.toBeNull();
    expect(staleBounds!.y).toBeGreaterThan(loadingBounds!.y);
  });

  test("error state shown when diff API fails", async ({ page }) => {
    await mockDiffApiError(page, 404, "diff not available for this pull request");
    await navigateToDiff(page);

    await expect(page.locator(".diff-state-msg--error")).toHaveText("diff not available for this pull request", {
      timeout: 10_000,
    });
  });

  test("diff content shows hunk headers and line types", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await firstFile.scrollIntoViewIfNeeded();

    // Hunk headers.
    await expectPierreDiffCount(firstFile, diffHunkSeparatorsSelector, 2);
    await expectPierreDiffFirstText(firstFile, diffHunkSeparatorsSelector, "@@ -10,5 +10,6 @@ func handleRequest");

    // Added lines (+ marker).
    await expectPierreDiffFirstVisible(firstFile, diffAdditionsSelector);

    // Deleted lines (- marker).
    await expectPierreDiffFirstVisible(firstFile, diffDeletionsSelector);
  });

  test("binary file shows notice instead of diff content", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const binaryFile = page.locator('[data-file-path="assets/logo.png"]');
    await expect(binaryFile.locator(".binary-notice")).toHaveText("Binary file changed");
  });

  test("hunkless textual file shows empty state instead of loading", async ({ page }) => {
    await mockDiffApi(page, hunklessTextDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const file = page.locator('[data-file-path="internal/server/config.go"]');
    await expect(file.getByText("No textual changes")).toBeVisible();
    await expect(file.getByRole("status")).toHaveCount(0);
  });

  test("empty added files with no hunk lines show empty state", async ({ page }) => {
    await mockDiffApi(page, emptyAddedFileDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    await expect(treeFileItem(page, "fixtures/.gitkeep")).toHaveAttribute("data-item-git-status", "added");
    const file = page.locator('[data-file-path="fixtures/.gitkeep"]');
    await expect(file.getByText("No textual changes")).toBeVisible();
    await expect(file.getByRole("status")).toHaveCount(0);
    await expectPierreDiffCount(file, diffAdditionsSelector, 0);
    await expectPierreDiffCount(file, diffDeletionsSelector, 0);
    await expectPierreDiffCount(file, diffContextSelector, 0);
  });

  test("oversized sparse hunks render without demand context expansion", async ({ page }) => {
    let previewRequests = 0;
    await mockDiffApi(page, oversizedSparseDiff);
    await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
      previewRequests++;
      await route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({
          detail: "unexpected preview request",
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const file = page.locator('[data-file-path="src/huge-context.ts"]');
    await expect(file).toBeVisible();
    await expectPierreDiffCount(file, diffAdditionsSelector, 1);
    await expectPierreDiffFirstText(file, diffHunkSeparatorsSelector, "999999 unmodified lines");
    await expectPierreDiffCount(file, "[data-expand-button]", 0);
    await expect.poll(() => previewRequests).toBe(0);
  });

  test("sparse diffs with syntax state gaps preload full context before rendering", async ({ page }) => {
    await page.addInitScript(() => {
      (globalThis as { __kenn_forgeForceSyntaxHighlight?: boolean }).__kenn_forgeForceSyntaxHighlight = true;
    });

    const previewSides: string[] = [];
    await mockDiffApi(page, syntaxContextDiff);
    await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get("path") !== syntaxContextPath) {
        await route.fulfill({
          status: 404,
          contentType: "application/json",
          body: JSON.stringify({ detail: "not found" }),
        });
        return;
      }

      const side = url.searchParams.get("side");
      previewSides.push(side ?? "");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          path: syntaxContextPath,
          media_type: "text/plain; charset=utf-8",
          encoding: "base64",
          content: Buffer.from(side === "old" ? syntaxContextOldText : syntaxContextNewText).toString("base64"),
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const file = page.locator(`[data-file-path="${syntaxContextPath}"]`);
    await expect.poll(() => [...new Set(previewSides)].sort()).toEqual(["new", "old"]);
    await expectPierreDiffVisibleText(file, diffAdditionsSelector, "vi.doMock");
    await expect
      .poll(() => pierreAdditionTokenStats(file, 'vi.doMock("./worker", () => ({'))
      .toEqual({
        hasSeparateDoMock: true,
        hasSeparateVi: true,
        singleLineSpan: false,
      });
  });

  test("sparse diffs with syntax state gaps fall back when full context fails", async ({ page }) => {
    let previewRequests = 0;
    await mockDiffApi(page, syntaxContextDiff);
    await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
      previewRequests++;
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ detail: "preview unavailable" }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const file = page.locator(`[data-file-path="${syntaxContextPath}"]`);
    await expect.poll(() => previewRequests).toBeGreaterThan(0);
    await expect(file.getByText("Could not load more context")).toBeVisible();
    await expectPierreDiffVisibleText(file, diffAdditionsSelector, "vi.doMock");
  });

  test("context expansion keeps earlier virtualized file rows rendered", async ({ page }) => {
    await page.setViewportSize({ width: 1852, height: 918 });

    const schemaLines: DiffLine[] = Array.from({ length: 140 }, (_, index) => ({
      type: "context" as const,
      content: `schema response row ${index + 1}`,
      old_num: index + 1,
      new_num: index + 1,
    }));
    schemaLines.splice(118, 0, {
      type: "add",
      content: "schema inserted response row",
      new_num: 119,
    });

    const oldDetailText = Array.from({ length: 1_000 }, (_, index) => `detail filler row ${index + 1}`);
    oldDetailText[909] = "function onActionMenuKeydown(e: KeyboardEvent): void {";
    oldDetailText[910] = '  if (actionMenuOpen && e.key === "Escape") {';
    oldDetailText[911] = "    actionMenuOpen = false;";
    oldDetailText[948] = '"/workspaces",';
    oldDetailText[949] = "{";
    oldDetailText[950] = "  body: {";
    oldDetailText[951] = "    platform_host: detail.platform_host,";
    oldDetailText[952] = "    owner: detail.repo_owner,";
    oldDetailText[953] = "    name: detail.repo_name,";
    const newDetailText = [...oldDetailText];
    newDetailText.splice(951, 0, "    provider,");

    const expandableDiff = withServerDiffData({
      stale: false,
      whitespace_only_count: 0,
      files: [
        {
          path: "src/api/generated/schema.ts",
          old_path: "src/api/generated/schema.ts",
          status: "modified",
          is_binary: false,
          is_generated: false,
          is_whitespace_only: false,
          additions: 1,
          deletions: 0,
          hunks: [
            {
              old_start: 1,
              old_count: 140,
              new_start: 1,
              new_count: 141,
              lines: schemaLines,
            },
          ],
        },
        {
          path: "src/api/provider-routes.ts",
          old_path: "src/api/provider-routes.ts",
          status: "modified",
          is_binary: false,
          is_generated: false,
          is_whitespace_only: false,
          additions: 1,
          deletions: 0,
          hunks: [
            {
              old_start: 1,
              old_count: 4,
              new_start: 1,
              new_count: 5,
              lines: [
                {
                  type: "context",
                  content: "export const routes = [",
                  old_num: 1,
                  new_num: 1,
                },
                {
                  type: "context",
                  content: '  "/commits/{sha}/diff",',
                  old_num: 2,
                  new_num: 2,
                },
                {
                  type: "add",
                  content: '  "/worktree-base",',
                  new_num: 3,
                },
                {
                  type: "context",
                  content: '  "/resolve/{number}",',
                  old_num: 3,
                  new_num: 4,
                },
                {
                  type: "context",
                  content: "];",
                  old_num: 4,
                  new_num: 5,
                },
              ],
            },
          ],
        },
        {
          path: "src/components/detail/PullDetail.svelte",
          old_path: "src/components/detail/PullDetail.svelte",
          preserveHunkCounts: true,
          status: "modified",
          is_binary: false,
          is_generated: false,
          is_whitespace_only: false,
          additions: 1,
          deletions: 0,
          hunks: [
            {
              old_start: 949,
              old_count: 48,
              new_start: 949,
              new_count: 49,
              lines: [
                {
                  type: "context",
                  content: '"/workspaces",',
                  old_num: 949,
                  new_num: 949,
                },
                {
                  type: "context",
                  content: "{",
                  old_num: 950,
                  new_num: 950,
                },
                {
                  type: "context",
                  content: "  body: {",
                  old_num: 951,
                  new_num: 951,
                },
                { type: "add", content: "    provider,", new_num: 952 },
                {
                  type: "context",
                  content: "    platform_host: detail.platform_host,",
                  old_num: 952,
                  new_num: 953,
                },
                {
                  type: "context",
                  content: "    owner: detail.repo_owner,",
                  old_num: 953,
                  new_num: 954,
                },
                {
                  type: "context",
                  content: "    name: detail.repo_name,",
                  old_num: 954,
                  new_num: 955,
                },
              ],
            },
          ],
        },
      ],
    });

    await mockDiffApi(page, expandableDiff);
    const previewSides: string[] = [];
    await page.route("**/api/v1/pulls/github/acme/widgets/1/file-preview**", async (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get("path") !== "src/components/detail/PullDetail.svelte") {
        await route.fulfill({
          status: 404,
          body: JSON.stringify({ detail: "not found" }),
        });
        return;
      }
      const side = url.searchParams.get("side");
      previewSides.push(side ?? "");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          path: "src/components/detail/PullDetail.svelte",
          media_type: "text/plain; charset=utf-8",
          encoding: "base64",
          content: Buffer.from(side === "old" ? oldDetailText.join("\n") : newDetailText.join("\n")).toString("base64"),
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");
    const schemaFile = page.locator('[data-file-path="src/api/generated/schema.ts"]');
    const detailFile = page.locator('[data-file-path="src/components/detail/PullDetail.svelte"]');

    await clickTreeFileItem(page, "src/components/detail/PullDetail.svelte");
    await expect(detailFile).toBeInViewport();
    await diffArea.evaluate((area) => {
      area.scrollTop = Math.max(0, area.scrollTop - 220);
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    });

    await expect
      .poll(async () =>
        detailFile.evaluate((detail) => {
          const rect = detail.getBoundingClientRect();
          return rect.bottom > 0 && rect.top < window.innerHeight;
        }),
      )
      .toBe(true);
    await expectPierreDiffCountAtLeast(detailFile, "[data-line-type]", 1);
    await expectPierreDiffCountAtLeast(detailFile, "[data-expand-button]", 1);
    await expectRenderedNonBlankRows(schemaFile, "schema response row");

    const beforeExpansionScrollTop = await diffArea.evaluate((area) => area.scrollTop);
    await clickPierreContextExpander(page, detailFile, 0, "[data-expand-down]");
    await expect.poll(() => [...new Set(previewSides)].sort()).toEqual(["new", "old"]);
    await expectRenderedNonBlankRows(schemaFile, "schema response row");
    await expectVisibleExpandedRowContent(detailFile, "function onActionMenuKeydown(e: KeyboardEvent): void {");
    await scrollDiffAreaUntilPierreText(
      page,
      diffArea,
      detailFile,
      "[data-content] [data-line-type='context-expanded']",
      "function onActionMenuKeydown(e: KeyboardEvent): void {",
      90,
    );

    await diffArea.evaluate((area, scrollTop) => {
      area.scrollTop = scrollTop;
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    }, beforeExpansionScrollTop);
    await expectRenderedNonBlankRows(schemaFile, "schema response row");
    await clickPierreContextExpander(page, detailFile);
    await expectRenderedNonBlankRows(schemaFile, "schema response row");
    await scrollDiffAreaUntilPierreText(
      page,
      diffArea,
      detailFile,
      "[data-content] [data-line-type='context-expanded']",
      "detail filler row 870",
      90,
    );
    await expectRenderedNonBlankRows(schemaFile, "schema response row");
  });

  test("deleted file path has strikethrough styling in diff header", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    // Deleted file is the 3rd file.
    const deletedHeader = page.locator(".diff-file").nth(2).locator(".file-path");
    await expect(deletedHeader).toHaveClass(/file-path--deleted/);
  });

  test("Pierre diff renders the gutter and content as aligned grid columns", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await firstFile.scrollIntoViewIfNeeded();
    await expectPierreDiffCount(firstFile, diffHunkSeparatorsSelector, 2);
    const layout = await firstFile.locator(".pierre-diff").evaluate((host) => {
      const root = host.shadowRoot;
      const code = root?.querySelector("code[data-unified]");
      const gutter = root?.querySelector("[data-gutter]");
      const content = root?.querySelector("[data-content]");
      if (!code || !gutter || !content) return null;
      return {
        codeDisplay: getComputedStyle(code).display,
        gutterRight: gutter.getBoundingClientRect().right,
        contentLeft: content.getBoundingClientRect().left,
      };
    });
    expect(layout?.codeDisplay).toBe("grid");
    expect(Math.round(layout!.gutterRight)).toBe(Math.round(layout!.contentLeft));
  });

  test("Pierre dark diff background follows the app surface token", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-theme", "dark");
    });
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await firstFile.scrollIntoViewIfNeeded();
    await expectPierreDarkBackgroundMatchesAppSurface(firstFile);
    await expectPierreChangeColorsMatchAppTokens(firstFile, "dark");
  });

  test("Pierre light diff change backgrounds stay light", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-theme", "light");
    });
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator('[data-file-path="internal/server/handler.go"]');
    await firstFile.scrollIntoViewIfNeeded();
    await expectPierreChangeColorsMatchAppTokens(firstFile, "light");
  });

  test("fallback file list renders when selected PR is filtered out of sidebar", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // PR 1 "Add widget caching layer" is selected and its file list renders
    // inside the PR detail files pane.
    const pr1 = page.locator(".pull-item").filter({ hasText: "caching layer" });
    await expect(pr1).toHaveCount(1);
    const detailFiles = page.locator(".files-layout > .files-sidebar");
    await expect(treeFileItems(detailFiles)).toHaveCount(4);

    // Filter the sidebar to exclude PR 1 by searching for a different PR.
    await page.locator(".search-wrap input").fill("race");
    await expect(page.locator(".list-count-chip")).toHaveText(/^1 PRs?$/, {
      timeout: 5_000,
    });
    await expect(page.locator(".pull-item").filter({ hasText: "caching layer" })).toHaveCount(0);

    // The selected PR can disappear from the outer list, but the detail
    // Files pane keeps its file list available.
    await expect(page.locator(".pull-list > .diff-files-wrap")).toHaveCount(0);
    await expect(treeFileItems(detailFiles)).toHaveCount(4);
  });

  test("inline file filter appears for large diffs and narrows list", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // Filter input is visible because diff has 50 files (>= 10 threshold).
    const filterInput = page.getByRole("searchbox", { name: "Filter files" }).first();
    await expect(filterInput).toBeVisible();
    await expect(treeFileItems(page)).toHaveCount(50);

    // Narrow the list — "file_1" matches file_1, file_10..file_19 = 11 files.
    await filterInput.fill("file_1");
    await expect(treeFileItems(page)).toHaveCount(11);

    // Clearing filter restores full list.
    await filterInput.fill("");
    await expect(treeFileItems(page)).toHaveCount(50);
  });

  test("filtered file tree consumes hidden explicit file jumps without revealing stale rows", async ({ page }) => {
    const targetPath = "src/pkg8/file_40.go";
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    const filterInput = page.getByRole("searchbox", { name: "Filter files" }).first();
    await filterInput.fill("file_1");
    await expect(treeFileItems(page)).toHaveCount(11);
    await expect(treeFileItem(page, targetPath)).toHaveCount(0);

    await jumpToFile(page, targetPath);
    await expect(page.locator(`[data-file-path="${targetPath}"]`)).toBeVisible();
    await expect(treeFileItem(page, targetPath)).toHaveCount(0);
    await expect(page.locator(".diff-file-tree [data-item-type='file'][aria-selected='true']")).toHaveCount(0);

    await filterInput.fill("");
    await expect(treeFileItems(page)).toHaveCount(50);
    const targetTreeItem = treeFileItem(page, targetPath);
    await expect(targetTreeItem).toHaveAttribute("aria-selected", "true");
    await scrollFileTreeToTop(page);
    expect(
      await targetTreeItem.evaluate((item) => {
        const rect = item.getBoundingClientRect();
        return rect.bottom > 0 && rect.top < window.innerHeight;
      }),
    ).toBe(false);

    await jumpToFile(page, targetPath);
    await expect(targetTreeItem).toHaveAttribute("aria-selected", "true");
    await expect(targetTreeItem).toBeInViewport();
  });

  test("inline file filter is hidden for small diffs", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // smallDiff has 4 files; filter input should not be rendered.
    await expect(page.getByRole("searchbox", { name: "Filter files" })).toHaveCount(0);
  });

  test("file filter resets when switching PRs (large -> large)", async ({ page }) => {
    // PR 1: large diff with filter shown.
    await mockDiffApi(page, largeDiff);
    // PR 2: also large so filter UI stays visible after switch.
    await page.route("**/api/v1/pulls/github/acme/widgets/2/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(largeDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/2/diff*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(largeDiff as DiffFixture)),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // Type into filter on PR 1.
    const filterInput = page.getByRole("searchbox", { name: "Filter files" }).first();
    await filterInput.fill("file_1");
    await expect(treeFileItems(page)).toHaveCount(11);

    // Switch to PR 2.
    await page.goto("/pulls/github/acme/widgets/2/files");
    await waitForSidebarFilesLoaded(page);

    // Filter input is empty and full list shows.
    const filterOnPR2 = page.getByRole("searchbox", { name: "Filter files" }).first();
    await expect(filterOnPR2).toHaveValue("");
    await expect(treeFileItems(page)).toHaveCount(50);
  });

  test("file filter doesn't silently hide files when switching to small-diff PR", async ({ page }) => {
    // PR 1: large diff with filter shown.
    await mockDiffApi(page, largeDiff);
    // PR 2: small diff (filter input hidden).
    await page.route("**/api/v1/pulls/github/acme/widgets/2/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(smallDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/2/diff*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(smallDiff as DiffFixture)),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // Type into filter on PR 1.
    await page.getByRole("searchbox", { name: "Filter files" }).first().fill("nomatch");
    await expect(treeFileItems(page)).toHaveCount(0);

    // Switch to PR 2 (small diff — filter input hidden).
    await page.goto("/pulls/github/acme/widgets/2/files");
    await waitForSidebarFilesLoaded(page);

    // Filter input is hidden and all 4 files show (stale query doesn't apply).
    await expect(page.getByRole("searchbox", { name: "Filter files" })).toHaveCount(0);
    await expect(treeFileItems(page)).toHaveCount(4);
  });

  test("fallback file list renders when selected PR's repo group is collapsed", async ({ page }) => {
    await mockDiffApi(page, smallDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // File list renders inside the PR detail Files pane.
    const pr1 = page.locator(".pull-item").filter({ hasText: "caching layer" });
    await expect(pr1).toHaveCount(1);
    const detailFiles = page.locator(".files-layout > .files-sidebar");
    await expect(treeFileItems(detailFiles)).toHaveCount(4);

    // Collapse the acme/widgets repo group (containing the selected PR).
    await page.locator(".sidebar-group-header", { hasText: "acme/widgets" }).click();
    await expect(page.locator(".sidebar-group-header", { hasText: "acme/widgets" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );

    // PR row hidden, but the detail Files pane keeps the file list available.
    await expect(page.locator(".pull-item").filter({ hasText: "caching layer" })).toHaveCount(0);
    await expect(page.locator(".pull-list > .diff-files-wrap")).toHaveCount(0);
    await expect(treeFileItems(detailFiles)).toHaveCount(4);
  });

  test("commit scope resets when switching PRs", async ({ page }) => {
    // Mock diff for PR 1 and PR 2 (same fixture is fine — we care about scope state).
    await mockDiffApi(page, smallDiff);
    await page.route("**/api/v1/pulls/github/acme/widgets/2/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(smallDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/2/diff*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(smallDiff as DiffFixture)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/*/commits", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          commits: [
            {
              sha: "abc1234567890123456789012345678901234567",
              message: "commit one",
              authored_at: "2026-04-01T00:00:00Z",
              author_name: "alice",
            },
          ],
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await waitForSidebarFilesLoaded(page);

    // Open the shared commit picker under PR 1 and select a scoped commit.
    await page.getByRole("button", { name: "Select commit range" }).click();
    await expect(page.locator(".commit-item").first()).toBeVisible();
    await page.locator(".commit-item").first().click();
    await expect(page.locator(".diff-scope-picker .diff-scope-label")).toHaveText("abc1234");

    // Switch to PR 2.
    await page.goto("/pulls/github/acme/widgets/2/files");
    await waitForSidebarFilesLoaded(page);

    // The selected commit scope should reset on the new PR.
    await expect(page.locator(".diff-scope-picker .diff-scope-label")).toHaveText("HEAD");
  });

  test("commit range picker scopes single commits and ranges", async ({ page }) => {
    const commitSHA = "abc1234567890123456789012345678901234567";
    const olderSHA = "def1234567890123456789012345678901234567";
    const scopedDiff: DiffResult = {
      ...smallDiff,
      files: [
        {
          ...smallDiff.files[1]!,
          path: "frontend/src/scoped.ts",
          old_path: "frontend/src/scoped.ts",
        },
      ],
    };
    const rangedDiff: DiffResult = {
      ...smallDiff,
      files: [
        {
          ...smallDiff.files[1]!,
          path: "frontend/src/scoped.ts",
          old_path: "frontend/src/scoped.ts",
        },
        {
          ...smallDiff.files[2]!,
          path: "frontend/src/ranged.ts",
          old_path: "frontend/src/ranged.ts",
        },
      ],
    };
    const scopedDiffRequests: string[] = [];
    const fixtureFor = (url: URL): DiffResult => {
      if (url.searchParams.has("from") && url.searchParams.has("to")) {
        return rangedDiff;
      }
      if (url.searchParams.has("commit")) {
        return scopedDiff;
      }
      return smallDiff;
    };

    await page.route("**/api/v1/pulls/github/acme/widgets/1/files**", async (route) => {
      const url = new URL(route.request().url());
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(fixtureFor(url))),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
      const url = new URL(route.request().url());
      scopedDiffRequests.push(url.toString());
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(fixtureFor(url) as DiffFixture)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/commits", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          commits: [
            {
              sha: commitSHA,
              message: "scoped commit",
              authored_at: "2026-04-01T00:00:00Z",
              author_name: "alice",
            },
            {
              sha: olderSHA,
              message: "base commit",
              authored_at: "2026-03-31T00:00:00Z",
              author_name: "alice",
            },
          ],
        }),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);
    await expect(page.locator(".diff-file")).toHaveCount(4);

    await page.getByRole("button", { name: "Select commit range" }).click();
    const commitItems = page.locator(".commit-item");
    await commitItems.first().click();

    await expect(page.locator(".diff-scope-picker .diff-scope-label")).toHaveText("abc1234");
    await expect(page.locator(".diff-scope-picker__trigger .scope-pill")).toHaveCount(0);
    await expect(page.locator(".diff-file")).toHaveCount(1);
    await expect(page.locator(".diff-file").first()).toHaveAttribute("data-file-path", "frontend/src/scoped.ts");

    await commitItems.nth(1).click({ modifiers: ["Shift"] });

    await expect(page.locator(".diff-scope-picker .diff-scope-label")).toHaveText("def1234..abc1234");
    await expect(page.locator(".diff-file")).toHaveCount(2);
    await expect(page.locator('[data-file-path="frontend/src/ranged.ts"]')).toBeVisible();
    expect(
      scopedDiffRequests.some((requestURL) => {
        const url = new URL(requestURL);
        return url.searchParams.get("from") === olderSHA && url.searchParams.get("to") === commitSHA;
      }),
    ).toBe(true);
  });
});

// --- Perf tests ---

test.describe("diff view performance", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.removeItem("diff-tab-width");
      localStorage.removeItem("diff-hide-whitespace");
      localStorage.removeItem("diff-word-wrap");
      localStorage.removeItem("diff-collapsed-files");
    });
  });

  test("fast scrolling large diffs keeps the UI interactive", async ({ page }) => {
    const pageErrors: string[] = [];
    page.on("pageerror", (error) => {
      pageErrors.push(error.message);
    });
    await page.addInitScript(() => {
      // This test asserts the highlight worker pool spins up during
      // fast scrolling, so opt back into syntax highlighting (it is
      // disabled by default under automation; see pierre-worker-pool.ts).
      (globalThis as { __kenn_forgeForceSyntaxHighlight?: boolean }).__kenn_forgeForceSyntaxHighlight = true;
      const nativeWorker = window.Worker;
      (window as typeof window & { __kenn_forgeWorkerUrls?: string[] }).__kenn_forgeWorkerUrls = [];
      window.Worker = class extends nativeWorker {
        constructor(scriptURL: string | URL, options?: WorkerOptions) {
          (
            window as typeof window & {
              __kenn_forgeWorkerUrls: string[];
            }
          ).__kenn_forgeWorkerUrls.push(String(scriptURL));
          super(scriptURL, options);
        }
      } as typeof Worker;
    });

    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await page.locator(".diff-area .kit-scrollbox__viewport").evaluate((area) => {
      for (let i = 0; i < 16; i++) {
        area.scrollTop = area.scrollHeight * ((i + 1) / 16);
        area.dispatchEvent(new Event("scroll", { bubbles: true }));
      }
      area.scrollTop = area.scrollHeight;
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    });

    await expect
      .poll(() => visiblePierreLoadingCount(page), {
        timeout: 10_000,
      })
      .toBe(0);
    await expect
      .poll(async () => {
        return await page.evaluate(
          () =>
            (
              window as typeof window & {
                __kenn_forgeWorkerUrls?: string[];
              }
            ).__kenn_forgeWorkerUrls?.length ?? 0,
        );
      })
      .toBeGreaterThan(0);

    const jumpTrigger = page.getByRole("button", { name: "Jump to file" });
    await jumpTrigger.click();
    const jumpDialog = page.getByRole("dialog", { name: "Jump to file" });
    const jumpInput = jumpDialog.getByRole("combobox", { name: "Jump to file" });
    await expect(jumpDialog).toBeVisible();
    await expect(jumpInput).toBeFocused();
    for (let i = 0; i < 20; i++) {
      await jumpInput.press("ArrowDown");
    }
    const highlightedOptionID = await jumpInput.getAttribute("aria-activedescendant");
    expect(highlightedOptionID).not.toBeNull();
    await expect(jumpDialog.locator(`#${highlightedOptionID}`)).toBeInViewport();
    await jumpInput.press("Escape");
    await expect(jumpDialog).toBeHidden();
    await expect(jumpTrigger).toBeFocused();
    expect(pageErrors).toEqual([]);
  });

  test("large diffs do not eagerly render every offscreen file", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await expect
      .poll(() => renderedPierreDiffCount(page), {
        timeout: 5_000,
      })
      .toBeGreaterThan(0);
    await page.waitForTimeout(500);
    expect(await renderedPierreDiffCount(page)).toBeLessThan(largeDiff.files.length);

    await page.locator(".diff-area .kit-scrollbox__viewport").evaluate((area) => {
      area.scrollTop = area.scrollHeight;
      area.dispatchEvent(new Event("scroll", { bubbles: true }));
    });

    await expect
      .poll(() => visiblePierreLoadingCount(page), {
        timeout: 10_000,
      })
      .toBe(0);
    expect(await renderedPierreDiffCount(page)).toBeLessThan(largeDiff.files.length);
  });

  test("pre-renders files within the observer margin below the fold", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    // DiffFile's IntersectionObserver uses the ScrollBox viewport as its
    // root with a 600px margin. Files below the fold but inside that
    // margin must render before they are scrolled into view; if the
    // observer root regresses to a non-scrolling ancestor, the viewport
    // clip zeroes the intersection and the pre-render buffer disappears.
    const bufferState = () =>
      page.evaluate(() => {
        const viewport = document.querySelector(".diff-area .kit-scrollbox__viewport");
        if (!viewport) return { inMargin: 0, rendered: 0 };
        const viewportBottom = viewport.getBoundingClientRect().bottom;
        const inMargin = Array.from(document.querySelectorAll(".diff-file")).filter((file) => {
          const rect = file.getBoundingClientRect();
          return rect.top > viewportBottom && rect.top < viewportBottom + 600;
        });
        return {
          inMargin: inMargin.length,
          rendered: inMargin.filter(
            (file) => file.querySelector(".pierre-diff")?.shadowRoot?.querySelector("[data-content]") != null,
          ).length,
        };
      });

    await expect
      .poll(
        async () => {
          const state = await bufferState();
          return state.inMargin > 0 && state.rendered === state.inMargin;
        },
        { timeout: 10_000 },
      )
      .toBe(true);
  });

  test("large diff (50 files) renders all file headers within timeout", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    await expect(page.getByRole("button", { name: "Jump to file" })).toBeVisible();

    // All 50 file headers should be in the DOM.
    await expect(page.locator(".diff-file .file-header")).toHaveCount(50, {
      timeout: 15_000,
    });

    // Sidebar inline file list should list all 50 files.
    await expect(treeFileItems(page)).toHaveCount(50);
  });

  test("collapsing a file removes its content from the DOM", async ({ page }) => {
    await mockDiffApi(page, largeDiff);
    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    const firstFile = page.locator(".diff-file").first();

    // Content present before collapse.
    await expect(firstFile.locator(".file-content")).toBeAttached();

    // Collapse.
    await firstFile.locator(".file-collapse-toggle").click();
    await expect(firstFile.locator(".file-content")).not.toBeAttached();

    // Other files still have their content.
    await expect(page.locator(".diff-file").nth(1).locator(".file-content")).toBeAttached();
  });

  test("whitespace toggle on large diff completes re-render", async ({ page }) => {
    // Return fewer files when whitespace=hide so we can distinguish
    // the post-toggle render from the initial one.
    const hiddenDiff = {
      ...largeDiff,
      files: largeDiff.files.slice(0, 45),
    };
    await page.route("**/api/v1/pulls/github/acme/widgets/1/files", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(filesFromDiff(largeDiff)),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/1/diff*", async (route) => {
      const url = new URL(route.request().url());
      const fixture = url.searchParams.get("whitespace") === "hide" ? hiddenDiff : largeDiff;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(diffResponseFromFixture(fixture as DiffFixture)),
      });
    });

    await navigateToDiff(page);
    await waitForDiffLoaded(page);

    // All 50 files present initially.
    await expect(page.locator(".diff-file .file-header")).toHaveCount(50, {
      timeout: 15_000,
    });

    // Toggle whitespace -- triggers a re-fetch with ?whitespace=hide
    // which returns fewer files.
    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Hide whitespace changes" }).click();

    // Count should drop to 45, proving the re-fetch and re-render completed.
    await expect(page.locator(".diff-file .file-header")).toHaveCount(45, {
      timeout: 15_000,
    });
  });
});

// --- Git-backed tests (real diff pipeline, no route mocking) ---
// These use a real git repo created by testutil.SetupDiffRepo for
// acme/widgets PR #1. The diff contains:
//   - internal/handler.go: modified (2 hunks, log->slog + added line)
//   - internal/cache.go: added
//   - config.yaml: deleted
//   - README.md: whitespace-only change

// These run fully parallel: the state-mutating tests below lease
// their own pooled isolated server, and the rest only read the
// shared server's diff for acme/widgets PR #1 (concurrent git reads
// are safe). The old machine-wide "git-backed-diff" lock predates
// per-test isolated servers and only serialized wall-clock time.
test.describe("diff view (git-backed)", () => {
  test.describe.configure({
    timeout: gitBackedDiffTestTimeoutMs,
  });

  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.removeItem("diff-tab-width");
      localStorage.removeItem("diff-hide-whitespace");
      localStorage.removeItem("diff-rich-preview");
      localStorage.removeItem("diff-collapsed-files");
    });
  });

  test("diff is not marked as stale", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });

    const banner = page.locator(".stale-banner");
    await expect(banner).toHaveCount(0);
  });

  test("real diff loads and renders all changed files", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });
    await treeFileItems(page).first().waitFor({ state: "visible", timeout: 10_000 });

    // Should have 4 changed files from the test repo.
    await expect(page.locator(".diff-file")).toHaveCount(4);
    await expect(treeFileItems(page)).toHaveCount(4);
  });

  test("bounds proactive syntax context across refreshes and prepares distant files", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    let releasePreviewRequests = (): void => {};
    const previewGate = new Promise<void>((resolve) => {
      releasePreviewRequests = resolve;
    });
    let activePreviewRequests = 0;
    let maxActivePreviewRequests = 0;
    const previewRequests: Array<{ path: string; side: string }> = [];
    const previewPattern = "**/api/v1/pulls/github/acme/widgets/1/file-preview**";

    try {
      const seeded = await page.request.post(`${server.info.base_url}/__e2e/pr-diff-context/large-head`);
      expect(seeded.ok()).toBe(true);

      await page.route(previewPattern, async (route) => {
        const url = new URL(route.request().url());
        previewRequests.push({
          path: url.searchParams.get("path") ?? "",
          side: url.searchParams.get("side") ?? "",
        });
        activePreviewRequests += 1;
        maxActivePreviewRequests = Math.max(maxActivePreviewRequests, activePreviewRequests);
        try {
          await previewGate;
          const response = await route.fetch();
          await route.fulfill({ response });
        } finally {
          activePreviewRequests -= 1;
        }
      });

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await expect.poll(() => activePreviewRequests, { timeout: 15_000 }).toBe(8);

      const refreshed = page.waitForRequest((request) => {
        const url = new URL(request.url());
        return url.pathname.endsWith("/diff") && url.searchParams.get("whitespace") === "hide";
      });
      await openDiffFilterMenu(page);
      await page.getByRole("switch", { name: "Hide whitespace changes" }).click();
      await refreshed;
      await page.waitForTimeout(750);

      expect(previewRequests).toHaveLength(8);
      expect(maxActivePreviewRequests).toBe(8);

      releasePreviewRequests();
      const distantPath = "src/context/file_07.ts";
      await expect
        .poll(
          () =>
            new Set(previewRequests.filter((request) => request.path === distantPath).map((request) => request.side))
              .size,
          { timeout: 15_000 },
        )
        .toBe(2);

      const distantFile = page.locator(`[data-file-path="${distantPath}"]`);
      await distantFile.scrollIntoViewIfNeeded();
      await expect(distantFile.locator(".pierre-diff-loading")).toHaveCount(0);
      await expect.poll(() => previewRequests.length, { timeout: 15_000 }).toBe(24);
      await expect.poll(() => activePreviewRequests, { timeout: 15_000 }).toBe(0);
      expect(maxActivePreviewRequests).toBeLessThanOrEqual(8);
    } finally {
      releasePreviewRequests();
      await page.unrouteAll({ behavior: "ignoreErrors" });
      await server.stop();
    }
  });

  test("category filter counts and filtering come from the real diff API", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const response = await page.request.post(`${server.info.base_url}/__e2e/pr-diff-summary/advance-head`);
      expect(response.ok()).toBe(true);

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await waitForSidebarFilesLoaded(page);

      const categoryFilter = page.getByRole("group", {
        name: "Filter changed files",
      });
      await expect(categoryFilter.getByRole("button", { name: "Plans/docs (2)" })).toBeVisible();
      await expect(categoryFilter.getByRole("button", { name: "Code (3)" })).toBeVisible();
      await expect(categoryFilter.getByRole("button", { name: "Tests (1)" })).toBeVisible();
      await expect(categoryFilter.getByRole("button", { name: "Other (0)" })).toBeVisible();
      await expect(categoryFilter.getByRole("button", { name: "All (6)" })).toHaveAttribute("aria-pressed", "true");

      await categoryFilter.getByRole("button", { name: "Tests (1)" }).click();

      await expect(page.locator(".diff-file")).toHaveCount(1);
      await expect(treeFileItems(page)).toHaveCount(1);
      await expect(page.locator('[data-file-path="internal/cache_test.go"]')).toBeVisible();
      await expect(treeFileItem(page, "internal/cache_test.go")).toBeVisible();
      await expect(page.locator('[data-file-path="internal/cache.go"]')).toHaveCount(0);
      await expect(treeFileItem(page, "internal/cache.go")).toHaveCount(0);

      await categoryFilter.getByRole("button", { name: "Plans/docs (2)" }).click();

      await expect(page.locator(".diff-file")).toHaveCount(2);
      await expect(treeFileItems(page)).toHaveCount(2);
      await expect(page.locator('[data-file-path="docs/cache-plan.md"]')).toBeVisible();
      await expect(page.locator('[data-file-path="README.md"]')).toBeVisible();
      await expect(treeFileItem(page, "docs/cache-plan.md")).toBeVisible();
      await expect(treeFileItem(page, "README.md")).toBeVisible();
      await expect(page.locator('[data-file-path="internal/cache_test.go"]')).toHaveCount(0);
    } finally {
      await server.stop();
    }
  });

  test("rich preview uses real diff data for markdown and keeps source diffs", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const advanceResponse = await page.request.post(`${server.info.base_url}/__e2e/pr-diff-summary/advance-head`);
      expect(advanceResponse.ok()).toBe(true);

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await waitForSidebarFilesLoaded(page);

      await openDiffFilterMenu(page);
      await page.getByRole("switch", { name: "Rich preview" }).click();

      const handlerFile = page.locator('[data-file-path="internal/handler.go"]');
      await handlerFile.scrollIntoViewIfNeeded();
      await expectPierreDiffFirstVisible(handlerFile, diffDeletionsSelector);
      await expectPierreDiffFirstVisible(handlerFile, diffAdditionsSelector);
      await expect(handlerFile.locator(".diff-text-preview")).toHaveCount(0);

      const categoryFilter = page.getByRole("group", {
        name: "Filter changed files",
      });
      await categoryFilter.getByRole("button", { name: "Plans/docs (2)" }).click();

      const planFile = page.locator('[data-file-path="docs/cache-plan.md"]');
      const planPreview = planFile.locator(".markdown-rich-diff--unified");
      await expect(planPreview.locator("ins", { hasText: "Cache refresh plan" })).toBeVisible();
      await expect(planPreview.locator("ins", { hasText: "Verify changed-file summaries refresh" })).toBeVisible();

      const previewResponse = await page.request.get(
        `${server.info.base_url}/api/v1/pulls/github/acme/widgets/1/file-preview?path=internal/cache.go`,
      );
      expect(previewResponse.ok()).toBe(true);
      const previewBody = await previewResponse.json();
      expect(previewBody.media_type).toContain("text/");
      expect(previewBody.content.length).toBeGreaterThan(0);
    } finally {
      await server.stop();
    }
  });

  test("context expansion fetches both sides from the real file preview API", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await waitForSidebarFilesLoaded(page);

      const handlerFile = page.locator('[data-file-path="internal/handler.go"]');
      await handlerFile.scrollIntoViewIfNeeded();
      await expectPierreDiffCountAtLeast(handlerFile, "[data-expand-button]", 1);

      const previewSides: string[] = [];
      const capturePreviewRequest = (request: Request) => {
        const url = new URL(request.url());
        if (!url.pathname.endsWith("/file-preview")) return;
        if (url.searchParams.get("path") !== "internal/handler.go") return;
        const side = url.searchParams.get("side");
        if (side) previewSides.push(side);
      };
      page.on("request", capturePreviewRequest);
      try {
        await clickPierreContextExpander(page, handlerFile, 0, "[data-expand-button]", { shiftKey: true });

        await expect
          .poll(
            async () => {
              const texts = await pierreDiffTexts(handlerFile, "[data-content] [data-line-type='context-expanded']");
              return texts.filter((text) => text.length > 0).length;
            },
            { timeout: 20_000 },
          )
          .toBeGreaterThan(0);
      } finally {
        page.off("request", capturePreviewRequest);
      }
      await expectPierreDiffVisibleText(handlerFile, "[data-content] [data-line-type='context-expanded']", "// line 1");
      expect(new Set(previewSides)).toEqual(new Set(["old", "new"]));
    } finally {
      await server.stop();
    }
  });

  test("inline review composer only opens from the gutter utility", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const baseURL = server.info.base_url;
      await page.goto(`${baseURL}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await waitForSidebarFilesLoaded(page);

      const cacheFile = page.locator('[data-file-path="internal/cache.go"]');
      await cacheFile.scrollIntoViewIfNeeded();
      const lineContent = cacheFile.locator('.pierre-diff [data-line][data-diff-new-line="1"]').first();
      await expect(lineContent).toBeVisible();
      await lineContent.click();
      await expect(page.getByPlaceholder("Leave a comment")).toHaveCount(0);

      await selectPierreReviewLine(cacheFile, 1, "right");
      await expect(page.getByPlaceholder("Leave a comment")).toBeVisible();
    } finally {
      await server.stop();
    }
  });

  test("inline review composer opens and persists selected left and right diff targets", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const baseURL = server.info.base_url;
      const detailResponse = await page.request.get(`${baseURL}/api/v1/pulls/github/acme/widgets/1`);
      expect(detailResponse.ok()).toBe(true);
      const detail = (await detailResponse.json()) as {
        diff_head_sha: string;
      };
      expect(detail.diff_head_sha).toBeTruthy();

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1/files`);
      await waitForDiffLoaded(page);
      await waitForSidebarFilesLoaded(page);

      // Let the route-load background sync settle (fast on a fresh
      // isolated server: the sync advances detail_fetched_at, so the
      // store's refetch loop exits on its first poll). Without this,
      // the refetch re-render can steal focus from the composer
      // between toBeFocused and fill below.
      await expect(page.locator(".pull-detail .sync-indicator")).toHaveCount(0, { timeout: 15_000 });

      const cacheFile = page.locator('[data-file-path="internal/cache.go"]');
      await cacheFile.scrollIntoViewIfNeeded();
      await selectPierreReviewLine(cacheFile, 1, "right");
      const rightComposer = page.getByPlaceholder("Leave a comment");
      await expect(rightComposer).toBeVisible();
      await expect(rightComposer).toBeEditable();
      await expectPierreDiffFirstVisible(cacheFile, diffAdditionsSelector);
      const cacheContentBox = await visibleBoundingBox(cacheFile.locator(".file-content"));
      const composerBox = await visibleBoundingBox(inlineComposerFor(rightComposer));
      expect(composerBox.x + composerBox.width).toBeLessThanOrEqual(cacheContentBox.x + cacheContentBox.width + 1);
      await rightComposer.fill("Right-side cache note");
      await submitInlineComposer(rightComposer);
      await expect(
        page.locator(".inline-draft-comment", {
          hasText: "Right-side cache note",
        }),
      ).toBeVisible();

      const configFile = page.locator('[data-file-path="config.yaml"]');
      await configFile.scrollIntoViewIfNeeded();
      await selectPierreReviewLine(configFile, 1, "left");
      const leftComposer = page.getByPlaceholder("Leave a comment");
      await expect(leftComposer).toBeVisible();
      await expect(leftComposer).toBeEditable();
      await leftComposer.fill("Left-side config note");
      await submitInlineComposer(leftComposer);
      await expect(
        page.locator(".inline-draft-comment", {
          hasText: "Left-side config note",
        }),
      ).toBeVisible();

      const draftResponse = await page.request.get(`${baseURL}/api/v1/pulls/github/acme/widgets/1/review-draft`);
      expect(draftResponse.ok()).toBe(true);
      const draft = (await draftResponse.json()) as {
        comments: Array<Record<string, unknown>>;
      };
      expect(draft.comments).toEqual(
        expect.arrayContaining([
          expect.objectContaining({
            body: "Right-side cache note",
            path: "internal/cache.go",
            side: "right",
            line: 1,
            new_line: 1,
            line_type: "add",
            diff_head_sha: detail.diff_head_sha,
          }),
          expect.objectContaining({
            body: "Left-side config note",
            path: "config.yaml",
            side: "left",
            line: 1,
            old_line: 1,
            line_type: "delete",
            diff_head_sha: detail.diff_head_sha,
          }),
        ]),
      );
    } finally {
      await server.stop();
    }
  });

  test("modified file has multiple hunks with correct content", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });

    // Find the handler.go file by its data attribute.
    const handlerFile = page.locator('[data-file-path="internal/handler.go"]');
    await expect(handlerFile).toBeVisible();

    // Should have 2 hunks (two separate modified regions).
    await expectPierreDiffCountAtLeast(handlerFile, diffHunkSeparatorsSelector, 1);

    // Deleted line: old log.Println call.
    await expectPierreDiffFirstVisible(handlerFile, diffDeletionsSelector);

    // Added line: new slog.Info call.
    await expectPierreDiffFirstVisible(handlerFile, diffAdditionsSelector);

    // Verify actual diff content -- the old log import was replaced.
    await expectPierreDiffFirstText(handlerFile, diffDeletionsSelector, "log");
    await expectPierreDiffFirstText(handlerFile, diffAdditionsSelector, "slog");
  });

  test("added file shows A status in sidebar and only addition lines", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });
    await treeFileItems(page).first().waitFor({ state: "visible", timeout: 10_000 });

    const cacheFile = page.locator('[data-file-path="internal/cache.go"]');
    await expect(cacheFile).toBeVisible();

    // Only addition lines -- no deletions or context.
    await expectPierreDiffFirstVisible(cacheFile, diffAdditionsSelector);
    await expectPierreDiffCount(cacheFile, diffDeletionsSelector, 0);
    // No context lines in a pure-add file.
    await expectPierreDiffCount(cacheFile, diffContextSelector, 0);

    // Sidebar status should be "A".
    await expect(treeFileItem(page, "internal/cache.go")).toHaveAttribute("data-item-git-status", "added");
  });

  test("deleted file shows D status in sidebar and only deletion lines", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });
    await treeFileItems(page).first().waitFor({ state: "visible", timeout: 10_000 });

    const configFile = page.locator('[data-file-path="config.yaml"]');
    await configFile.scrollIntoViewIfNeeded();
    await expect(configFile).toBeVisible();

    // Only deletion lines -- no additions or context.
    await expectPierreDiffFirstVisible(configFile, diffDeletionsSelector);
    await expectPierreDiffCount(configFile, diffAdditionsSelector, 0);
    await expectPierreDiffCount(configFile, diffContextSelector, 0);

    // Sidebar status should be "D".
    await expect(treeFileItem(page, "config.yaml")).toHaveAttribute("data-item-git-status", "deleted");
  });

  test("hide whitespace toggle filters whitespace-only files", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });

    // Initially 4 files visible.
    await expect(page.locator(".diff-file")).toHaveCount(4);

    // Toggle hide whitespace.
    await openDiffFilterMenu(page);
    await page.getByRole("switch", { name: "Hide whitespace changes" }).click();

    // README.md is whitespace-only and should be hidden.
    await expect(page.locator(".diff-file")).toHaveCount(3, {
      timeout: 10_000,
    });
  });

  test("modified file renders multiple Pierre hunk separators", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.locator(".diff-file").first().waitFor({ state: "visible", timeout: 10_000 });

    const handlerFile = page.locator('[data-file-path="internal/handler.go"]');

    await expectPierreDiffCountAtLeast(handlerFile, diffHunkSeparatorsSelector, 1);
  });

  test("commit list uses UTC API values and local date rendering", async ({ page }) => {
    await page.addInitScript(
      (offsetMs) => {
        const originalNow = Date.now.bind(Date);
        Date.now = () => originalNow() + offsetMs;
      },
      20 * 24 * 60 * 60 * 1000,
    );

    await page.goto("/pulls/github/acme/widgets/1/files");
    await page.getByRole("button", { name: "Select commit range" }).click();
    await page.locator(".commit-item").first().waitFor({ state: "visible", timeout: 10_000 });

    const payload = await page.evaluate(async () => {
      const response = await fetch("/api/v1/pulls/github/acme/widgets/1/commits");
      return response.json();
    });

    expect(payload.commits[0].authored_at).toMatch(/Z$/);

    const expectedLabel = await page.evaluate(
      (iso: string) => new Date(iso).toLocaleDateString(),
      payload.commits[0].authored_at,
    );

    await expect(page.locator(".commit-item__date").first()).toHaveText(expectedLabel);
    expect(expectedLabel).not.toContain("T");
    expect(expectedLabel).not.toContain("Z");
  });
});
