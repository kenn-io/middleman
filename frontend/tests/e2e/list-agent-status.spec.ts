import { expect, test, type Page } from "@playwright/test";
import type { PullRequest, Issue } from "../../src/lib/api/types.js";
import { createMockApiHandler } from "../../src/test/mockApiFetch.js";
import { mockApi } from "./support/mockApi";

async function expectAgentAtRight(page: Page): Promise<void> {
  const gap = await page
    .locator(".cell--title .agent-state")
    .first()
    .evaluate((label) => {
      const cell = label.parentElement!;
      return cell.getBoundingClientRect().right - label.getBoundingClientRect().right;
    });
  expect(gap).toBeLessThanOrEqual(1);
}

test("browser preference shows linked agent states across item lists", async ({ page }) => {
  test.setTimeout(60_000);
  await mockApi(page);
  const api = createMockApiHandler();
  const pulls: PullRequest[] = await api
    .handle({ method: "GET", url: new URL("http://localhost/api/v1/pulls"), bodyText: "" })
    .json();
  const issues: Issue[] = await api
    .handle({ method: "GET", url: new URL("http://localhost/api/v1/issues"), bodyText: "" })
    .json();
  const pull = pulls[0]!;
  const issue = issues[0]!;
  let pullAgentState = "working";
  await page.route("**/api/v1/pulls?*", (route) =>
    route.fulfill({
      json: pulls.map((item) => ({
        ...item,
        workspace: { id: `pr-${item.Number}`, status: "ready", agent_state: pullAgentState },
      })),
    }),
  );
  await page.route("**/api/v1/issues?*", (route) =>
    route.fulfill({
      json: issues.map((item) => ({
        ...item,
        workspace: { id: `issue-${item.Number}`, status: "ready", agent_state: "approval" },
      })),
    }),
  );
  await page.route("**/api/v1/activity?*", (route) =>
    route.fulfill({
      json: {
        capped: false,
        items: [
          {
            id: "agent-status-event",
            cursor: "agent-status-event",
            activity_type: "comment",
            author: "reviewer",
            body_preview: "Ready for review",
            created_at: new Date().toISOString(),
            item_number: pull.Number,
            item_state: "open",
            item_title: pull.Title,
            item_type: "pr",
            item_url: pull.URL,
            platform_host: pull.platform_host,
            repo_owner: pull.repo_owner,
            repo_name: pull.repo_name,
            repo: pull.repo,
            workspace: { id: `pr-${pull.Number}`, status: "ready", agent_state: "done" },
          },
        ],
      },
    }),
  );

  await page.goto("/pulls");
  await expect(page.locator(".pr-list-row").filter({ hasText: pull.Title })).toBeVisible();
  await expect(page.locator(".pr-list-row .agent-state")).toHaveCount(0);
  const originalHeight = await page
    .locator(".pr-list-row")
    .filter({ hasText: pull.Title })
    .evaluate((row) => row.getBoundingClientRect().height);
  await page.goto("/settings");
  await page
    .getByRole("navigation", { name: "Settings" })
    .getByRole("button", { name: /^Workspaces / })
    .click();
  const toggle = page.getByRole("button", { name: "Show agent status in lists" });
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-pressed", "true");
  await page.goto("/pulls");
  await expect(
    page.locator(".pr-list-row").filter({ hasText: pull.Title }).getByText("Working", { exact: true }),
  ).toBeVisible();
  expect(
    await page
      .locator(".pr-list-row")
      .filter({ hasText: pull.Title })
      .evaluate((row) => row.getBoundingClientRect().height),
  ).toBe(originalHeight);
  const numberGaps = await page.locator(".pr-list-row").evaluateAll((rows) =>
    rows.map((row) => {
      const agent = row.querySelector(".agent-state")!.getBoundingClientRect();
      const number = row.querySelector(".item-number")!.getBoundingClientRect();
      return {
        horizontal: number.left - agent.right,
        vertical: Math.abs((number.top + number.bottom) / 2 - (agent.top + agent.bottom) / 2),
      };
    }),
  );
  for (const gap of numberGaps) {
    expect(gap.horizontal).toBeLessThan(12);
    expect(gap.vertical).toBeLessThan(1);
  }
  await page.screenshot({ path: test.info().outputPath("pull-list-agent-status.png") });
  pullAgentState = "input";
  await expect(
    page.locator(".pr-list-row").filter({ hasText: pull.Title }).getByText("Input", { exact: true }),
  ).toBeVisible({ timeout: 12_000 });
  await page.goto("/issues");
  await expect(
    page.locator(".issue-item").filter({ hasText: issue.Title }).getByText("Approval", { exact: true }),
  ).toBeVisible();
  await page.goto("/?view=flat");
  await expect(page.locator(".agent-state").getByText("Done", { exact: true }).first()).toBeVisible();
  await expectAgentAtRight(page);
  await page.goto("/?view=threaded");
  await expect(page.locator(".agent-state").getByText("Done", { exact: true }).first()).toBeVisible();
  await expectAgentAtRight(page);
  await page.screenshot({ path: test.info().outputPath("list-agent-status.png") });
});
