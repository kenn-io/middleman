import { expect, test } from "@playwright/test";

test.describe("focus mode", () => {
  test("PR focus route renders detail without shell chrome", async ({ page }) => {
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".focus-layout").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.locator(".pull-detail")).toBeVisible();
    await expect(page.locator(".app-top-bar")).not.toBeAttached();
    await expect(page.locator(".kit-sidebar-layout__sidebar")).not.toBeAttached();
    await expect(page.locator(".kit-status-bar")).not.toBeAttached();
  });

  test("issue focus route renders detail without shell chrome", async ({ page }) => {
    await page.goto("/focus/issues/github/acme/widgets/10");
    await page.locator(".focus-layout").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.locator(".issue-detail")).toBeVisible();
    await expect(page.locator(".app-top-bar")).not.toBeAttached();
    await expect(page.locator(".kit-sidebar-layout__sidebar")).not.toBeAttached();
    await expect(page.locator(".kit-status-bar")).not.toBeAttached();
  });

  test("narrow PR focus route keeps actions available in the compact layout", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".focus-layout .pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Merge", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Close", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Create Workspace", exact: true })).toBeVisible();
    const labels = page.locator(".label-editor-anchor--inline").getByRole("button", { name: "Labels" });
    await expect(labels).toBeVisible();

    await labels.click();
    await expect(page.locator(".label-picker")).toBeVisible();
    await expect(page.locator(".label-editor-backdrop")).toHaveCount(0);
    await expect(page.locator(".focus-layout .pull-detail")).toBeVisible();

    const pickerRect = await page.locator(".label-picker").boundingBox();
    const viewport = page.viewportSize();
    expect(pickerRect).not.toBeNull();
    expect(viewport).not.toBeNull();
    if (pickerRect && viewport) {
      expect(pickerRect.y).toBeGreaterThanOrEqual(20);
      expect(pickerRect.y + pickerRect.height).toBeLessThanOrEqual(viewport.height - 20);
      expect(Math.abs(pickerRect.x + pickerRect.width / 2 - viewport.width / 2)).toBeLessThanOrEqual(2);
    }

    await page.mouse.click(4, 4);
    await expect(page.locator(".label-picker")).toBeHidden();
  });

  test("narrow PR focus route closes the label picker when Labels toggles it", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".focus-layout .pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    const labels = page.locator(".label-editor-anchor--inline").getByRole("button", { name: "Labels" });
    await labels.click();
    await expect(page.locator(".label-picker")).toBeVisible();
    await labels.click();
    await expect(page.locator(".label-picker")).toBeHidden();
  });

  test("PR focus route dismisses the inline label picker on outside click", async ({ page }) => {
    await page.setViewportSize({ width: 560, height: 720 });
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".focus-layout .pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    await page.locator(".label-editor-anchor--inline").getByRole("button", { name: "Labels" }).click();
    await expect(page.locator(".label-picker")).toBeVisible();

    await page.mouse.click(4, 4);
    await expect(page.locator(".label-picker")).toBeHidden();
  });

  test("PR focus route keeps workspace creation available in the mid-narrow layout", async ({ page }) => {
    await page.route("**/api/v1/workspaces", async (route) => {
      if (route.request().method() !== "POST") {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({
          title: "Workspace failed",
          status: 500,
          code: "internalError",
          detail: "workspace setup failed",
        }),
      });
    });

    await page.setViewportSize({ width: 560, height: 720 });
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".focus-layout .pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
    const createWorkspace = page.getByRole("button", { name: "Create Workspace", exact: true });
    await expect(createWorkspace).toBeVisible();
    await expect(page.locator(".actions-row--workspace")).toBeVisible();

    const copyNumberHeight = await page
      .locator(".meta-row .copy-number-btn")
      .evaluate((node) => node.getBoundingClientRect().height);
    expect(copyNumberHeight).toBeLessThan(28);
    await expect(page.locator(".meta-sep--branch")).toBeHidden();
    await expect(page.locator(".meta-sep--sync")).toBeHidden();

    await createWorkspace.click();
    await expect(page.locator(".kit-flash-stack").getByRole("status")).toContainText("workspace setup failed");
  });

  test("narrow merged PR focus route keeps labels available without workspace creation", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/focus/pulls/github/acme/widgets/3");
    await page.locator(".focus-layout .pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
    await expect(page.locator(".label-editor-anchor--inline").getByRole("button", { name: "Labels" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Create Workspace", exact: true })).toHaveCount(0);
  });

  test("browser back/forward works between focus routes", async ({ page }) => {
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await page.locator(".pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    // Navigate forward to an issue focus route.
    await page.goto("/focus/issues/github/acme/widgets/10");
    await page.locator(".issue-detail").waitFor({ state: "visible", timeout: 10_000 });
    await expect(page).toHaveURL(/\/focus\/issues\/github\/acme\/widgets\/10$/);

    // Go back to the PR focus route.
    await page.goBack();
    await page.locator(".pull-detail").waitFor({ state: "visible", timeout: 10_000 });
    await expect(page).toHaveURL(/\/focus\/pulls\/github\/acme\/widgets\/1$/);

    // Go forward to the issue focus route.
    await page.goForward();
    await page.locator(".issue-detail").waitFor({ state: "visible", timeout: 10_000 });
    await expect(page).toHaveURL(/\/focus\/issues\/github\/acme\/widgets\/10$/);
  });
});
