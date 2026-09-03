import { expect, test, type Page } from "@playwright/test";
import { startIsolatedE2EServer, startIsolatedWorkspaceE2EServer, type IsolatedE2EServer } from "./support/e2eServer";
import type { IssueDetailResponse } from "../../src/lib/api/generated/models/index.js";

// Seeded issues (6 total):
//   acme/widgets#10: open, eve, "Widget rendering broken on Safari"
//   acme/widgets#11: open, alice, "Add dark mode support"
//   acme/widgets#12: closed, bob, "Crash on empty input"
//   acme/widgets#13: open, dependabot[bot], "Security advisory: prototype pollution"
//   acme/tools#5: open, dave, "Support config file loading"
//   group/project#11: open, ada, "GitLab read-only issue"

async function waitForIssueList(page: Page): Promise<void> {
  await page.locator(".issue-item").first().waitFor({ state: "visible", timeout: 10_000 });
}

async function selectIssueState(page: Page, label: string): Promise<void> {
  const stateButton = page.locator(".state-btn", { hasText: label });
  if (await stateButton.isVisible()) {
    await stateButton.click();
    return;
  }

  await page.getByRole("button", { name: "Filters" }).click();
  await page.locator(".kit-filter-dropdown__panel .kit-filter-dropdown__item", { hasText: label }).first().click();
}

async function selectIssueGrouping(page: Page, label: string): Promise<void> {
  const groupButton = page.locator(".group-btn", { hasText: label });
  if (await groupButton.isVisible()) {
    await groupButton.click();
    return;
  }

  await page.getByRole("button", { name: "Filters" }).click();
  await page.locator(".kit-filter-dropdown__panel .kit-filter-dropdown__item", { hasText: label }).last().click();
}

const longRepoName = "widgets-with-an-extremely-long-repository-name";
const longRepoPath = `acme/${longRepoName}`;

type WorkspaceStatusResponse = {
  id: string;
  status: string;
  error_message?: string | null;
};

async function waitForWorkspaceReady(page: Page, baseURL: string, workspaceId: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const response = await page.request.get(`${baseURL}/api/v1/workspaces/${workspaceId}`);
        expect(response.ok()).toBe(true);
        const workspace = (await response.json()) as WorkspaceStatusResponse;
        if (workspace.status === "error") {
          throw new Error(workspace.error_message ?? `workspace ${workspaceId} failed to become ready`);
        }
        return workspace.status;
      },
      { timeout: 15_000 },
    )
    .toBe("ready");
}

async function mockLongIssueRepoSlug(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname.endsWith("/api/v1/issues"),
    async (route) => {
      const response = await route.fetch();
      const issues = (await response.json()) as Array<{
        repo?: { owner?: string; name?: string; repo_path?: string };
        repo_owner?: string;
        repo_name?: string;
      }>;
      const firstIssue = issues[0];
      if (firstIssue) {
        firstIssue.repo_owner = "acme";
        firstIssue.repo_name = longRepoName;
        if (firstIssue.repo) {
          firstIssue.repo.owner = "acme";
          firstIssue.repo.name = longRepoName;
          firstIssue.repo.repo_path = longRepoPath;
        }
      }
      await route.fulfill({ response, json: issues });
    },
  );
}

async function expectRepoNameToClipSafely(
  item: ReturnType<Page["locator"]>,
  repoName: ReturnType<Page["locator"]>,
  expectedRepoPath: string,
): Promise<void> {
  await item.evaluate((node) => {
    (node as HTMLElement).style.width = "180px";
  });

  await expect(repoName).toHaveText(expectedRepoPath);
  await expect(repoName).toHaveCSS("overflow", "hidden");
  await expect(repoName).toHaveCSS("text-overflow", "ellipsis");
  await expect(repoName).toHaveAttribute("title", expectedRepoPath);

  const nameBox = await repoName.boundingBox();
  const itemBox = await item.boundingBox();
  expect(nameBox).not.toBeNull();
  expect(itemBox).not.toBeNull();
  if (nameBox !== null && itemBox !== null) {
    expect(nameBox.x + nameBox.width).toBeLessThanOrEqual(itemBox.x + itemBox.width + 1);
  }

  const labelOverflow = await repoName.evaluate((node) => ({
    clientWidth: (node as HTMLElement).clientWidth,
    scrollWidth: (node as HTMLElement).scrollWidth,
  }));
  expect(labelOverflow.scrollWidth).toBeGreaterThanOrEqual(labelOverflow.clientWidth);
}

test.describe("issue list mutations", () => {
  let server: IsolatedE2EServer | undefined;

  test.beforeAll(async () => {
    server = await startIsolatedE2EServer();
  });

  test.afterAll(async () => {
    await server?.stop();
  });

  test("sidebar star changes persist and remain visible after reload", async ({ page }) => {
    if (!server) throw new Error("issue list e2e server was not started");
    const baseURL = server.info.base_url;
    await page.goto(`${baseURL}/issues`);
    await waitForIssueList(page);
    const row = page.locator(".issue-item").filter({ hasText: "Widget rendering broken on Safari" }).first();
    const star = row.locator(".star-btn");
    const initialTitle = await star.getAttribute("title");
    const expectedTitle = initialTitle === "Star" ? "Unstar" : "Star";
    const method = initialTitle === "Star" ? "PUT" : "DELETE";
    const mutation = page.waitForResponse(
      (response) => response.request().method() === method && response.url() === `${baseURL}/api/v1/starred`,
    );

    await star.click();

    expect((await mutation).status()).toBe(200);
    await expect(star).toHaveAttribute("title", expectedTitle);
    await expect
      .poll(async () => {
        const response = await page.request.get(`${baseURL}/api/v1/issues/github/acme/widgets/10`);
        const detail = (await response.json()) as { issue: { Starred?: boolean } };
        return Boolean(detail.issue.Starred);
      })
      .toBe(expectedTitle === "Unstar");
    await page.reload();
    await waitForIssueList(page);
    await expect(
      page.locator(".issue-item").filter({ hasText: "Widget rendering broken on Safari" }).first().locator(".star-btn"),
    ).toHaveAttribute("title", expectedTitle);
  });

  test("closing and reopening an issue persist through refreshed detail", async ({ page }) => {
    if (!server) throw new Error("issue list e2e server was not started");
    const baseURL = server.info.base_url;
    const detailURL = `${baseURL}/api/v1/issues/github/acme/widgets/10`;
    const stateURL = `${detailURL}/github-state`;
    const persistedState = async (): Promise<string> => {
      const response = await page.request.get(detailURL);
      const detail = (await response.json()) as IssueDetailResponse;
      return detail.issue.State;
    };

    await page.goto(`${baseURL}/issues`);
    await waitForIssueList(page);
    await page.locator(".issue-item").filter({ hasText: "Widget rendering broken on Safari" }).first().click();
    await expect(page.locator(".issue-detail .issue-state-chip")).toHaveText("Open");

    const closed = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url() === stateURL,
    );
    await page.getByRole("button", { name: "Close issue" }).click();
    expect((await closed).status()).toBe(200);
    await expect(page.locator(".issue-detail .issue-state-chip")).toHaveText("Closed");
    await expect.poll(persistedState).toBe("closed");

    await page.reload();
    await expect(page.locator(".issue-detail .issue-state-chip")).toHaveText("Closed");
    const reopened = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url() === stateURL,
    );
    await page.getByRole("button", { name: "Reopen issue" }).click();
    expect((await reopened).status()).toBe(200);
    await expect(page.locator(".issue-detail .issue-state-chip")).toHaveText("Open");
    await expect.poll(persistedState).toBe("open");

    await page.reload();
    await expect(page.locator(".issue-detail .issue-state-chip")).toHaveText("Open");
  });
});

test.describe("issue detail pane focus", () => {
  let server: IsolatedE2EServer | undefined;

  test.beforeAll(async () => {
    server = await startIsolatedWorkspaceE2EServer();
  });

  test.afterAll(async () => {
    await server?.stop();
  });

  test("keeps one focused border across issue, workspace, and terminal panes", async ({ page }) => {
    if (!server) throw new Error("issue workspace e2e server was not started");
    const baseURL = server.info.base_url;
    const created = await page.request.post(`${baseURL}/api/v1/issues/github/acme/widgets/10/workspace`, {
      data: {},
    });
    expect(created.status()).toBe(202);
    const workspace = (await created.json()) as WorkspaceStatusResponse;
    await waitForWorkspaceReady(page, baseURL, workspace.id);
    const launched = await page.request.post(`${baseURL}/api/v1/workspaces/${workspace.id}/runtime/sessions`, {
      data: { target_key: "plain_shell", display_region: "workflow" },
    });
    expect(launched.status(), await launched.text()).toBe(200);
    const docked = await page.request.post(`${baseURL}/api/v1/workspaces/${workspace.id}/runtime/sessions`, {
      data: { target_key: "plain_shell", display_region: "terminal" },
    });
    expect(docked.status(), await docked.text()).toBe(200);

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${baseURL}/issues/github/acme/widgets/10`);

    const conversationPane = page.locator('[data-pane-key="conversation"]');
    const workspacePane = page.locator('[data-pane-key="workspace"]');
    const conversationLeaf = conversationPane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");
    const workspaceLeaf = workspacePane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");
    await expect(workspaceLeaf.locator("canvas, .xterm-screen").first()).toBeVisible();
    const conversationTab = conversationLeaf.getByRole("tab", { name: "Conversation" });
    await conversationTab.focus();
    await expect(conversationTab).toBeFocused();
    await expect(conversationLeaf).toHaveClass(/input-active/);
    await expect(workspaceLeaf).not.toHaveClass(/input-active/);

    const terminalToggle = workspaceLeaf.getByRole("button", { name: "Open terminal panel" });
    await terminalToggle.focus();
    await expect(workspaceLeaf).toHaveClass(/input-active/);
    await expect(conversationLeaf).not.toHaveClass(/input-active/);

    await terminalToggle.click();
    const terminalPanel = workspaceLeaf.locator(".terminal-panel.bottom.open");
    await expect(terminalPanel).toBeVisible();
    await terminalPanel.locator(".panel-title").focus();
    await expect(terminalPanel).toHaveClass(/input-active/);
    const focusChrome = await workspaceLeaf.evaluate((leaf) => {
      const terminal = leaf.querySelector<HTMLElement>(".terminal-panel.input-active");
      if (!terminal) throw new Error("Active terminal panel is missing");
      const outer = getComputedStyle(leaf, "::after");
      const inner = getComputedStyle(terminal, "::after");
      return {
        innerWidths: [inner.borderTopWidth, inner.borderRightWidth, inner.borderBottomWidth, inner.borderLeftWidth],
        outerContent: outer.content,
      };
    });
    expect(focusChrome.innerWidths).toEqual(["1px", "1px", "1px", "1px"]);
    expect(focusChrome.outerContent).toBe("none");

    await conversationPane.hover();
    await page.mouse.wheel(0, 120);
    await expect(terminalPanel).toHaveClass(/input-active/);
    await expect(conversationLeaf).not.toHaveClass(/input-active/);
  });
});

test.describe("issue list view", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/issues");
    await waitForIssueList(page);
  });

  test("sidebar issue pills use the shared chip component", async ({ page }) => {
    try {
      await expect(page.locator(".filter-bar .list-count-chip")).toHaveText(/^5 issues$/);

      await mockLongIssueRepoSlug(page);
      await page.goto("/issues");
      await waitForIssueList(page);

      await selectIssueGrouping(page, "All");
      const firstItem = page.locator(".issue-item").first();
      const repoName = firstItem.locator(".repo-name");
      await expect(repoName).toBeVisible();
      await expectRepoNameToClipSafely(firstItem, repoName, longRepoPath);
      // The default list view shows open issues, whose state chip is
      // silent by design; only non-default (closed) rows show a chip.
      await expect(firstItem.locator(".state-chip")).toHaveCount(0);

      await expect(firstItem.locator(".meta-row .repo-name")).toBeVisible();
      await expect(page.locator(".issue-item .repo-row")).toHaveCount(0);
      await expect(page.locator(".issue-item:has(.kit-color-label)").first()).toBeVisible();
      const rowHeights = await page
        .locator(".issue-item")
        .evaluateAll((nodes) => nodes.slice(0, 6).map((node) => node.getBoundingClientRect().height));
      expect(rowHeights.length).toBeGreaterThan(1);
      for (const height of rowHeights) {
        expect(height).toBeLessThanOrEqual(60);
        expect(Math.abs(height - (rowHeights[0] ?? 0))).toBeLessThanOrEqual(1);
      }
    } finally {
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  });

  test("closed state shows closed issues", async ({ page }) => {
    await selectIssueState(page, "Closed");

    await expect(page.locator(".state-note")).toBeVisible();
    // Closed is the non-default state, so its chip stays visible (open
    // rows render no chip at all; see "sidebar issue pills...").
    await waitForIssueList(page);
    await expect(page.locator(".issue-item .state-chip").first()).toHaveText("Closed");
  });

  test("search filters by title", async ({ page }) => {
    const input = page.locator(".search-wrap input");
    await input.fill("Safari");

    // Wait for the filtered result to appear (replaces fixed sleep).
    await expect(page.locator(".filter-bar .list-count-chip")).toHaveText(/^1 issues?$/, {
      timeout: 5_000,
    });

    const items = page.locator(".issue-item");
    const count = await items.count();
    expect(count).toBe(1);

    for (let i = 0; i < count; i++) {
      const title = await items.nth(i).locator(".title").textContent();
      expect(title).toContain("Safari");
    }
  });

  test("issue detail state chip preserves shared chip layout", async ({ page }) => {
    await page.locator(".issue-item").filter({ hasText: "Safari" }).first().click();

    const stateChip = page.locator(".issue-detail .issue-state-chip");
    await expect(stateChip).toBeVisible();
    await expect(stateChip).toHaveText("Open");

    const stateChipStyles = await stateChip.evaluate((node) => {
      const styles = getComputedStyle(node);
      return {
        minHeight: styles.minHeight,
        fontSize: styles.fontSize,
        backgroundColor: styles.backgroundColor,
      };
    });

    expect(stateChipStyles.minHeight).toBe("16px");
    expect(stateChipStyles.fontSize).toBe("10px");
    expect(stateChipStyles.backgroundColor).not.toBe("rgba(0, 0, 0, 0)");
  });

  test("issue detail keeps the scrollbar on the pane edge", async ({ page }) => {
    // Open the Safari issue specifically. Matches widgets#10 on the
    // seeded fixture (max-width 800px centered layout).
    await page.locator(".issue-item").filter({ hasText: "Safari" }).first().click();

    // IssueListView renders IssueDetail into .kit-sidebar-layout__main;
    // .issue-detail is the content wrapper and the ScrollBox viewport is
    // the designated internal scroll container.
    const issueDetail = page.locator(".issue-detail");
    await expect(issueDetail).toBeVisible();
    const scroller = page.getByRole("region", { name: "Issue conversation" });

    // Inject a tall filler so overflow is guaranteed even with the
    // short seeded body. flex-shrink: 0 is required because
    // .issue-detail is a flex column; without it, the child would be
    // shrunk to fit.
    await issueDetail.evaluate((el) => {
      const filler = document.createElement("div");
      filler.style.height = "3000px";
      filler.style.flexShrink = "0";
      filler.style.background = "transparent";
      filler.setAttribute("data-test-filler", "issue-scroll");
      el.appendChild(filler);
    });

    // The ScrollBox viewport owns vertical scroll.
    const overflowY = await scroller.evaluate((el) => getComputedStyle(el).overflowY);
    expect(["auto", "scroll"]).toContain(overflowY);

    const before = await scroller.evaluate((el) => ({
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
      scrollTop: el.scrollTop,
    }));
    expect(before.scrollHeight).toBeGreaterThan(before.clientHeight);
    expect(before.scrollTop).toBe(0);

    await scroller.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });

    const finalScroll = await scroller.evaluate((el) => el.scrollTop);
    expect(finalScroll).toBeGreaterThan(0);

    // The scroll container should span the detail pane so the native
    // scrollbar is flush with the pane edge, not the centered content
    // column. The header remains in the capped content column.
    const detailArea = page.locator(".kit-sidebar-layout__main").filter({ visible: true });
    const contentHeader = page.locator(".issue-detail .detail-header");
    const areaBox = await detailArea.boundingBox();
    const detailBox = await scroller.boundingBox();
    const headerBox = await contentHeader.boundingBox();
    expect(areaBox).not.toBeNull();
    expect(detailBox).not.toBeNull();
    expect(headerBox).not.toBeNull();
    if (areaBox !== null && detailBox !== null && headerBox !== null) {
      const scrollportWidth = await scroller.evaluate((el) => el.clientWidth);
      const scrollportCenter = detailBox.x + scrollportWidth / 2;
      const headerCenter = headerBox.x + headerBox.width / 2;
      // Pane chrome allowance: leaf border plus the 1px ring the leaf
      // reserves for the pane focus marker (see TabbedPanelTree), plus
      // sub-pixel slack. Anything larger means the scroller shrank to the
      // centered content column instead of spanning the pane.
      expect(Math.abs(detailBox.x + detailBox.width - (areaBox.x + areaBox.width))).toBeLessThan(3);
      expect(Math.abs(headerCenter - scrollportCenter)).toBeLessThan(2);
      expect(headerBox.width).toBeLessThanOrEqual(800);
    }
  });
});
