import { expect, test, type Page, type Route } from "@playwright/test";

import type { DiffResponse } from "../../src/lib/api/generated/models/index.js";
import { mockApi } from "./support/mockApi";
import { splitFocusedPane } from "./support/paneCommands";

const capabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_labels: true,
  comment_mutation: true,
  thread_reply: false,
  thread_resolve: false,
  label_mutation: true,
  state_mutation: true,
  merge_mutation: true,
  review_mutation: true,
  workflow_approval: true,
  ready_for_review: true,
  draft_mutation: true,
  issue_mutation: true,
  review_draft_mutation: false,
  review_thread_resolution: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: false,
  supported_review_actions: [],
};

const diffResponse = {
  stale: false,
  whitespace_only_count: 0,
  files: [
    {
      path: "src/split-view.ts",
      old_path: "src/split-view.ts",
      status: "modified",
      is_binary: false,
      is_generated: false,
      is_whitespace_only: false,
      additions: 1,
      deletions: 1,
      patch: "@@ -1,1 +1,1 @@\n-old\n+new\n",
      hunks: [
        {
          old_start: 1,
          old_count: 1,
          new_start: 1,
          new_count: 1,
          lines: [
            { type: "delete", content: "old", old_num: 1 },
            { type: "add", content: "new", new_num: 1 },
          ],
        },
      ],
    },
  ],
} satisfies DiffResponse;

async function fulfillJson(route: Route, body: unknown): Promise<void> {
  await route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

async function mockSplitViewPR(page: Page): Promise<void> {
  const detailPath = "**/api/v1/pulls/github/acme/widgets/42";
  const replacementDetailPath = "**/api/v1/pulls/github/acme/widgets/55";

  await page.route(detailPath, async (route) => {
    if (route.request().method() !== "GET") {
      await route.fallback();
      return;
    }
    await fulfillJson(route, {
      detail_loaded: true,
      detail_fetched_at: "2026-03-30T14:00:00Z",
      diff_head_sha: "head",
      platform_host: "github.com",
      repo_owner: "acme",
      repo_name: "widgets",
      warnings: [],
      workflow_approval: {
        count: 0,
        required: false,
        runs: [],
      },
      workspace: undefined,
      worktree_links: [],
      repo: {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        capabilities,
      },
      merge_request: {
        ID: 1,
        RepoID: 1,
        PlatformID: 101,
        PlatformExternalID: "PR_42",
        Number: 42,
        URL: "https://github.com/acme/widgets/pull/42",
        Title: "Add browser regression coverage",
        Author: "marius",
        AuthorDisplayName: "Marius",
        State: "open",
        IsDraft: false,
        IsLocked: false,
        Body: "Adds Playwright smoke tests for workspace panel.",
        HeadBranch: "feature/playwright",
        BaseBranch: "main",
        HeadRepoCloneURL: "https://github.com/acme/widgets.git",
        Additions: 120,
        Deletions: 12,
        CommentCount: 3,
        ReviewDecision: "APPROVED",
        CIStatus: "success",
        CIChecksJSON: "[]",
        CIHadPending: false,
        CreatedAt: "2026-03-29T14:00:00Z",
        UpdatedAt: "2026-03-30T14:00:00Z",
        LastActivityAt: "2026-03-30T14:00:00Z",
        MergedAt: null,
        ClosedAt: null,
        MergeableState: "clean",
        DetailFetchedAt: "2026-03-30T14:00:00Z",
        KanbanStatus: "reviewing",
        Starred: false,
        labels: [],
      },
      events: [],
    });
  });

  await page.route(`${detailPath}/files`, async (route) => {
    await fulfillJson(route, diffResponse);
  });
  await page.route(`${detailPath}/diff**`, async (route) => {
    await fulfillJson(route, diffResponse);
  });
  await page.route("**/api/v1/repo/github/acme/widgets/labels", async (route) => {
    await fulfillJson(route, {
      labels: [{ name: "bug", color: "d73a4a", description: "Something is not working" }],
      stale: false,
      syncing: false,
    });
  });
  await page.route(`${replacementDetailPath}/files`, async (route) => {
    await fulfillJson(route, diffResponse);
  });
  await page.route(`${replacementDetailPath}/diff**`, async (route) => {
    await fulfillJson(route, diffResponse);
  });
}

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await mockSplitViewPR(page);
});

const LAYOUT_KEY = "kenn-forge-pane-layout-v1:prs";

async function firstPaneWidth(page: Page): Promise<number> {
  const box = await page.locator(".tabbed-panel-split-child.first").boundingBox();
  return box?.width ?? 0;
}

interface StoredNode {
  type: string;
  ratio?: number;
  first?: StoredNode;
  second?: StoredNode;
}

/**
 * Every split ratio in the stored tree.
 *
 * The stored tree keeps panes the current selection cannot offer (here the
 * workspace), so the dragged split is nested rather than the root.
 */
async function persistedRatios(page: Page): Promise<number[]> {
  const raw = await page.evaluate((key) => localStorage.getItem(key), LAYOUT_KEY);
  if (raw === null) return [];
  const parsed = JSON.parse(raw) as { tree?: StoredNode };
  const ratios: number[] = [];
  const walk = (node: StoredNode | undefined): void => {
    if (!node || node.type !== "split") return;
    if (typeof node.ratio === "number") ratios.push(node.ratio);
    walk(node.first);
    walk(node.second);
  };
  walk(parsed.tree);
  return ratios;
}

test("splits the PR detail panes apart and remembers the dragged ratio", async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/pulls/github/acme/widgets/42");

  await expect(page.locator(".detail-title")).toContainText("Add browser regression coverage");
  // Conversation and files start stacked in one leaf, so only one body shows.
  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(0);
  await expect(page.locator(".files-layout")).toHaveCount(0);

  await splitFocusedPane(page, "right");

  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(2);
  await expect(page.locator(".pull-detail")).toBeVisible();
  await expect(page.locator(".files-layout")).toBeVisible();
  await expect(page.getByText("src/split-view.ts")).toBeVisible();

  const resizeHandle = page.getByRole("separator", { name: "Resize detail panes" });
  await expect(resizeHandle).toBeVisible();
  await expect(resizeHandle).toHaveCSS("cursor", "col-resize");

  const before = await firstPaneWidth(page);
  const handleBox = await resizeHandle.boundingBox();
  expect(before).toBeGreaterThan(0);
  expect(handleBox).not.toBeNull();
  if (!handleBox) {
    throw new Error("Expected the pane divider to be measurable");
  }

  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + handleBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(handleBox.x + handleBox.width / 2 - 240, handleBox.y + handleBox.height / 2);
  await page.mouse.up();

  await expect.poll(async () => firstPaneWidth(page)).toBeLessThan(before - 150);
  // The ratio has to survive in storage: the layout is per top-level mode and is
  // restored on the next visit rather than recomputed from the viewport.
  const ratios = await persistedRatios(page);
  expect(ratios.some((ratio) => ratio < 0.4)).toBe(true);

  await page.reload();
  await expect(page.locator(".files-layout")).toBeVisible();
  await expect.poll(async () => firstPaneWidth(page)).toBeLessThan(before - 150);
});

test("routes page keys by focus while wheel input remains focus-neutral", async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/pulls/github/acme/widgets/42");
  await expect(page.locator(".detail-title")).toContainText("Add browser regression coverage");

  await splitFocusedPane(page, "right");
  await expect(page.locator(".files-layout")).toBeVisible();

  const conversationPane = page.locator('[data-pane-key="conversation"]');
  const filesPane = page.locator('[data-pane-key="files"]');
  const conversationLeaf = conversationPane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");
  const filesLeaf = filesPane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");
  const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");

  await page.locator(".diff-content").evaluate((content) => {
    const spacer = document.createElement("div");
    spacer.style.height = "2000px";
    spacer.dataset.testScrollSpacer = "true";
    content.append(spacer);
  });
  await expect.poll(async () => diffArea.evaluate((area) => area.scrollHeight > area.clientHeight)).toBe(true);

  await conversationLeaf.getByRole("tab").focus();
  await expect(page.locator(".tabbed-panel-leaf.input-active")).toHaveCount(1);
  await expect(conversationLeaf).toHaveClass(/input-active/);
  await expect(filesLeaf).not.toHaveClass(/input-active/);

  await page.locator(".pull-detail .chips-row").getByRole("button", { name: "Labels", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Edit labels" })).toBeVisible();
  await expect(page.getByLabel("Filter labels")).toBeFocused();
  const activeBorder = await conversationLeaf.evaluate((leaf) => ({
    overlayContent: getComputedStyle(leaf, "::after").content,
    insetShadow: getComputedStyle(leaf).boxShadow,
  }));
  expect(activeBorder.overlayContent).toBe("none");
  expect(activeBorder.insetShadow).toContain("inset");
  await page.getByRole("button", { name: "Close label picker" }).click();
  await conversationLeaf.getByRole("tab").focus();

  await diffArea.evaluate((area) => {
    area.scrollTop = 0;
  });
  await page.keyboard.press("PageDown");
  await expect.poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop))).toBe(0);

  await diffArea.hover();
  await page.mouse.wheel(0, 360);
  await expect(page.locator(".tabbed-panel-leaf.input-active")).toHaveCount(1);
  await expect(conversationLeaf).toHaveClass(/input-active/);
  await expect(filesLeaf).not.toHaveClass(/input-active/);
  await expect.poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop))).toBeGreaterThan(0);

  const afterWheel = await diffArea.evaluate((area) => Math.round(area.scrollTop));
  await page.keyboard.press("PageDown");
  await expect.poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop))).toBe(afterWheel);

  await filesLeaf.getByRole("tab", { name: "Files changed" }).focus();
  await expect(filesLeaf).toHaveClass(/input-active/);
  await expect(conversationLeaf).not.toHaveClass(/input-active/);

  const filesPaneChrome = await filesLeaf.evaluate((leaf) => {
    const toolbar = leaf.querySelector<HTMLElement>(".diff-toolbar");
    const activeTab = leaf.querySelector<HTMLElement>(".tabbed-panel-tab.active");
    if (!toolbar || !activeTab) throw new Error("Files pane chrome is missing");
    const tabAccent = getComputedStyle(activeTab, "::before");
    return {
      activeBorder: getComputedStyle(leaf).boxShadow,
      overlayContent: getComputedStyle(leaf, "::after").content,
      tabAccentContent: tabAccent.content,
      toolbarZIndex: getComputedStyle(toolbar).zIndex,
    };
  });
  expect(filesPaneChrome.activeBorder).toContain("inset");
  expect(filesPaneChrome.overlayContent).toBe("none");
  expect(filesPaneChrome.tabAccentContent).toBe("none");
  expect(filesPaneChrome.toolbarZIndex).toBe("auto");

  // The focus marker is an inset shadow, which paints beneath descendants. It
  // stays visible only because the leaf reserves the outer 1px of its padding
  // box: every child must lay out strictly inside that ring, even while the
  // scrolled diff surface presses against the pane edges.
  const ringClearance = await filesLeaf.evaluate((leaf) => {
    const rect = leaf.getBoundingClientRect();
    const style = getComputedStyle(leaf);
    const inner = {
      left: rect.left + parseFloat(style.borderLeftWidth) + 1,
      top: rect.top + parseFloat(style.borderTopWidth) + 1,
      right: rect.right - parseFloat(style.borderRightWidth) - 1,
      bottom: rect.bottom - parseFloat(style.borderBottomWidth) - 1,
    };
    const epsilon = 0.01;
    return [...leaf.children].map((child) => {
      const box = child.getBoundingClientRect();
      return {
        className: child.className,
        insideRing:
          box.width === 0 ||
          box.height === 0 ||
          (box.left >= inner.left - epsilon &&
            box.top >= inner.top - epsilon &&
            box.right <= inner.right + epsilon &&
            box.bottom <= inner.bottom + epsilon),
      };
    });
  });
  for (const child of ringClearance) {
    expect(child.insideRing, `${child.className} must not cover the focus ring`).toBe(true);
  }

  const afterFocus = await diffArea.evaluate((area) => Math.round(area.scrollTop));
  await page.keyboard.press("PageDown");
  await expect.poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop))).toBeGreaterThan(afterFocus);
});

test("keeps page keys out of the files diff while a modal owns focus", async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto("/pulls/github/acme/widgets/42/files");
  const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");
  await page.locator(".diff-content").evaluate((content) => {
    const spacer = document.createElement("div");
    spacer.style.height = "2000px";
    content.append(spacer);
  });
  await diffArea.evaluate((area) => {
    area.scrollTop = 0;
  });

  await page.keyboard.press("Meta+K");
  const palette = page.getByRole("dialog", { name: "Command palette" });
  await expect(palette).toBeVisible();
  await expect(palette.getByRole("textbox", { name: "Search command palette" })).toBeFocused();

  await page.keyboard.press("PageDown");

  await expect.poll(async () => diffArea.evaluate((area) => Math.round(area.scrollTop))).toBe(0);
});

async function detailPaneHostWidth(page: Page): Promise<number> {
  return Math.round(await page.locator(".detail-pane-layout").evaluate((el) => el.getBoundingClientRect().width));
}

test("keeps the arrangement at an ordinary window width and flattens below 720px", async ({ page }) => {
  // The regression this pins: reusing the old 1280px split-view gate flattened
  // the layout at ordinary window sizes, because a 1280px window minus the list
  // rail leaves the pane host around 940px.
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto("/pulls/github/acme/widgets/42");
  await expect(page.locator(".detail-title")).toContainText("Add browser regression coverage");

  const ordinaryWidth = await detailPaneHostWidth(page);
  expect(ordinaryWidth).toBeLessThan(1280);
  expect(ordinaryWidth).toBeGreaterThanOrEqual(720);
  await expect(page.getByRole("button", { name: "Maximize pane" })).toBeVisible();

  await splitFocusedPane(page, "right");
  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(2);
  await expect(page.getByRole("separator", { name: "Resize detail panes" })).toBeVisible();

  // Below the threshold every structural control goes away and the panes collapse
  // into one flat tab strip, without discarding the stored arrangement.
  await page.setViewportSize({ width: 700, height: 900 });
  await expect.poll(async () => detailPaneHostWidth(page)).toBeLessThan(720);
  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Maximize pane" })).toHaveCount(0);
  await expect(page.getByRole("separator", { name: "Resize detail panes" })).toHaveCount(0);
  await expect(page.getByRole("tab", { name: "Files changed" })).toBeVisible();

  // Back above the threshold the split the user made is still there.
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(2);
});

test("restores layout focus when a focused pane is replaced across the flatten threshold", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto("/pulls/github/acme/widgets/42");
  await splitFocusedPane(page, "right");

  const layout = page.locator(".detail-pane-layout");
  const filesTab = page.getByRole("tab", { name: "Files changed" });
  await filesTab.focus();
  await expect(filesTab).toBeFocused();

  await page.setViewportSize({ width: 700, height: 900 });
  await expect.poll(async () => detailPaneHostWidth(page)).toBeLessThan(720);
  await expect(layout).toBeFocused();

  await filesTab.focus();
  await expect(filesTab).toBeFocused();

  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(page.locator(".tabbed-panel-split-child")).toHaveCount(2);
  await expect(layout).toBeFocused();
});

test("restores layout focus when history replaces focused same-tab content", async ({ page }) => {
  await page.goto("/pulls/github/acme/widgets/42/files");
  const layout = page.locator(".detail-pane-layout");
  const diffArea = page.locator(".diff-area .kit-scrollbox__viewport");
  await diffArea.focus();
  await expect(diffArea).toBeFocused();

  await page.evaluate(() => {
    window.history.pushState(null, "", "/pulls/github/acme/widgets/55/files");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });

  await expect(page.locator(".detail-title")).toContainText("Refactor theme system");
  await expect(layout).toBeFocused();
  await expect(page.locator(".tabbed-panel-leaf.input-active")).toHaveCount(0);
});
