import { expect, test, type Page } from "@playwright/test";
import { openSettingsPanel } from "./support/settingsPanel";
import { startIsolatedE2EServer, type IsolatedE2EServer } from "./support/e2eServer";

let server: IsolatedE2EServer | undefined;

test.beforeEach(async () => {
  server = await startIsolatedE2EServer();
});

test.afterEach(async () => {
  await server?.stop();
  server = undefined;
});

async function setActionsMode(page: Page, enabled: boolean): Promise<void> {
  await page.goto(`${server!.info.base_url}/settings`);
  await openSettingsPanel(page, "Visible modes");
  const toggle = page.getByLabel("Actions", { exact: true });
  if (enabled) {
    await expect(toggle).not.toBeChecked();
    await toggle.check();
  } else {
    await expect(toggle).toBeChecked();
    await toggle.uncheck();
  }
  const saveResponsePromise = page.waitForResponse(
    (response) => response.url().endsWith("/api/v1/settings") && response.request().method() === "PUT",
  );
  await page.getByRole("button", { name: "Save visible modes" }).click();
  const saveResponse = await saveResponsePromise;
  expect(saveResponse.status(), `saving visible modes failed: ${await saveResponse.text()}`).toBe(200);
}

async function openReleaseFromPull(page: Page, number: number) {
  await page.goto(`${server!.info.base_url}/pulls/github/acme/widgets/${number}`);
  await page.locator(".pull-detail").waitFor({ state: "visible", timeout: 10_000 });
  await page.getByLabel("Pull request conversation").getByRole("button", { name: "Run workflow", exact: true }).click();
  const workflowMenu = page.getByRole("region", { name: "GitHub Actions" });
  await expect(workflowMenu).toBeVisible();
  await workflowMenu.getByRole("button", { name: "Release", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Run workflow" });
  await expect(dialog).toBeVisible();
  return dialog;
}

async function chooseWorkflowInput(page: Page, name: string, option: string): Promise<void> {
  const combobox = page.getByRole("combobox", { name, exact: true });
  await combobox.click();
  const listbox = page.getByRole("listbox");
  await expect(listbox).toBeVisible();
  await listbox.getByRole("option", { name: option, exact: true }).click();
  await expect(combobox).toContainText(option);
}

test("runs typed Actions workflows and applies pull request ref defaults until the mode is disabled", async ({
  page,
}) => {
  test.setTimeout(60_000);
  const workflowReads: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" && url.pathname.includes("/api/v1/actions/")) {
      workflowReads.push(url.pathname);
    }
  });

  await page.goto(`${server!.info.base_url}/`);
  await expect(page.locator(".kit-top-bar__tabs")).toBeVisible();
  await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
  expect(workflowReads).toEqual([]);

  await setActionsMode(page, true);
  const actionsTab = page.getByRole("button", { name: "Actions", exact: true });
  await expect(actionsTab).toBeVisible();
  await actionsTab.click();
  await expect(page.getByRole("heading", { name: "Actions", exact: true })).toBeVisible();

  const repositoryRail = page.getByRole("navigation", { name: "Actions repositories" });
  await repositoryRail.getByRole("button", { name: /widgets/ }).click();
  await page.getByRole("button", { name: /Release.*release\.yml/ }).click();
  await expect(page.getByRole("button", { name: /Push checks/ })).toHaveCount(0);

  await page.getByRole("textbox", { name: "Git ref" }).fill("main");
  await page.getByRole("textbox", { name: "version" }).fill("v2.4.0");
  await page.getByRole("checkbox", { name: "dry_run" }).check();
  await chooseWorkflowInput(page, "channel", "beta");
  await chooseWorkflowInput(page, "target", "production");

  const dispatchResponsePromise = page.waitForResponse(
    (response) => response.url().includes("/workflows/8101/dispatch") && response.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Run workflow", exact: true }).click();
  const dispatchResponse = await dispatchResponsePromise;
  expect(dispatchResponse.status(), `workflow dispatch failed: ${await dispatchResponse.text()}`).toBe(202);
  const dispatchBody = (await dispatchResponse.json()) as { run?: { id?: string } };
  expect(dispatchBody.run?.id).toBe("82003");

  const dispatchedRun = page.getByRole("button", { name: "Run 43 Release" });
  await expect(dispatchedRun).toBeVisible();
  await expect(dispatchedRun).toContainText("#43");
  await expect(dispatchedRun).toContainText("main");
  await expect(dispatchedRun).toContainText("fixture-viewer");
  await expect(dispatchedRun).toContainText("in_progress");
  await dispatchedRun.click();

  const jobs = page.getByLabel("Jobs for run 43");
  await expect(jobs).toBeVisible();
  const publishJob = jobs.getByRole("button", { name: /publish-release/ });
  await expect(publishJob).toContainText("in_progress");
  await publishJob.click();
  const steps = jobs.getByRole("list", { name: "publish-release steps" });
  await expect(steps.getByText("Prepare")).toBeVisible();
  await expect(steps.getByText("completed · success")).toBeVisible();
  await expect(steps.getByText("Publish")).toBeVisible();
  await expect(steps.getByText("in_progress", { exact: true })).toBeVisible();

  const sameRepoDialog = await openReleaseFromPull(page, 1);
  await expect(sameRepoDialog.getByRole("textbox", { name: "Git ref" })).toHaveValue("feature/caching");
  await sameRepoDialog.getByRole("button", { name: "Cancel" }).click();

  await setActionsMode(page, false);
  await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
  const readsAfterDisable = workflowReads.length;
  await page.goto(`${server!.info.base_url}/pulls/github/acme/widgets/3`);
  await page.locator(".pull-detail").waitFor({ state: "visible", timeout: 10_000 });
  await expect(
    page.getByLabel("Pull request conversation").getByRole("button", { name: "Actions", exact: true }),
  ).toHaveCount(0);
  await page.waitForTimeout(6_000);
  expect(workflowReads).toHaveLength(readsAfterDisable);
});
