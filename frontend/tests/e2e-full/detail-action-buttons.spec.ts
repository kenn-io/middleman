import { execFileSync } from "node:child_process";
import { access, writeFile } from "node:fs/promises";
import { expect, request as playwrightRequest, test, type APIRequestContext, type Page } from "@playwright/test";
import {
  startIsolatedE2EServer,
  startIsolatedE2EServerWithOptions,
  startIsolatedWorkspaceE2EServer,
  type IsolatedE2EServer,
} from "./support/e2eServer";

type WorkspaceStatusResponse = {
  id: string;
  platform_host: string;
  repo_owner: string;
  repo_name: string;
  item_type: string;
  item_number: number;
  git_head_ref: string;
  worktree_path: string;
  tmux_session: string;
  status: string;
  error_message?: string | null;
};

const lockedWorkspaceTestTimeoutMs = 120_000;

function hasCommand(command: string, args: string[] = ["--version"]): boolean {
  try {
    execFileSync(command, args, { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

function gitOutput(dir: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd: dir,
    encoding: "utf8",
  }).trim();
}

function activePullAction(page: Page, selector: string) {
  return page.locator(".primary-actions-live > .actions-row--primary").locator(selector);
}

async function waitForWorkspaceReady(api: APIRequestContext, workspaceId: string): Promise<WorkspaceStatusResponse> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const response = await api.get(`/api/v1/workspaces/${workspaceId}`);
    expect(response.ok()).toBe(true);
    const workspace = (await response.json()) as WorkspaceStatusResponse;
    if (workspace.status === "ready") {
      return workspace;
    }
    if (workspace.status === "error") {
      throw new Error(workspace.error_message ?? `workspace ${workspaceId} failed to become ready`);
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }

  throw new Error(`workspace ${workspaceId} did not become ready`);
}

test.describe("detail action buttons", () => {
  test.describe.configure({ timeout: lockedWorkspaceTestTimeoutMs });

  test("issue detail creates a kenn-forge workspace in its inline workspace pane", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({
        baseURL: isolatedServer.info.base_url,
      });

      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/issues/github/acme/widgets/10`);
      await expect(page.locator(".issue-detail")).toBeVisible();

      const createResponsePromise = page.waitForResponse((response) => {
        const url = response.url();
        return (
          response.request().method() === "POST" &&
          url === `${server.info.base_url}/api/v1/issues/github/acme/widgets/10/workspace`
        );
      });

      await page.getByRole("button", { name: "Create Workspace", exact: true }).click();

      const createResponse = await createResponsePromise;
      expect(createResponse.status()).toBe(202);

      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      expect(createdWorkspace.platform_host).toBe("github.com");
      expect(createdWorkspace.item_type).toBe("issue");
      expect(createdWorkspace.item_number).toBe(10);
      expect(createdWorkspace.git_head_ref).toBe("kenn-forge/issue-10-widget-rendering-broken-on-safari");

      // Creation stays on the issue: the workspace claims the inline pane
      // instead of navigating away.
      await expect(page).toHaveURL(/\/issues\/github\/acme\/widgets\/10$/);
      // The pane tree renders before any claim; the workspace slot (and the
      // reparented workspace host inside it) exists only once the created
      // workspace actually claims and hosts the pane.
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();

      const readyWorkspace = await waitForWorkspaceReady(apiContext, createdWorkspace.id);
      await access(readyWorkspace.worktree_path);
      expect(gitOutput(readyWorkspace.worktree_path, ["branch", "--show-current"])).toBe(
        "kenn-forge/issue-10-widget-rendering-broken-on-safari",
      );

      // A workspace with nothing running opens its launcher overlay in the pane,
      // and nothing behind a modal is clickable.
      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(launcher).toBeHidden();

      // The secondary action still navigates to the full Workspaces view.
      await page.getByRole("button", { name: "Open in Workspaces" }).click();
      await expect(page).toHaveURL(new RegExp(`/terminal/${createdWorkspace.id}$`));
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("PR detail creates a kenn-forge workspace in its inline workspace pane", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({
        baseURL: isolatedServer.info.base_url,
      });

      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      // PR creation uses its own endpoint (POST /workspaces with an
      // mr_number body), separate from the issue path covered above.
      const createResponsePromise = page.waitForResponse((response) => {
        return response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`;
      });

      // PullDetail renders the workspace action in both its wide and
      // narrow action layouts; only one is visible at a time.
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();

      const createResponse = await createResponsePromise;
      expect(createResponse.status()).toBe(202);

      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      expect(createdWorkspace.platform_host).toBe("github.com");
      expect(createdWorkspace.item_type).toBe("pull_request");
      expect(createdWorkspace.item_number).toBe(1);

      // Creation stays on the pull request: the workspace claims the
      // inline pane instead of navigating away. The pane tree renders
      // before any claim; the slot (and the reparented workspace host
      // inside it) exists only once the created workspace actually claims
      // and hosts the pane.
      await expect(page).toHaveURL(/\/pulls\/github\/acme\/widgets\/1$/);
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();

      const readyWorkspace = await waitForWorkspaceReady(apiContext, createdWorkspace.id);
      await access(readyWorkspace.worktree_path);
      expect(readyWorkspace.git_head_ref).toBeTruthy();
      expect(gitOutput(readyWorkspace.worktree_path, ["branch", "--show-current"])).toBe(readyWorkspace.git_head_ref);

      // A workspace with nothing running opens its launcher overlay in the pane,
      // and nothing behind a modal is clickable.
      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(launcher).toBeHidden();

      // The secondary action still navigates to the full Workspaces view.
      await page.getByRole("button", { name: "Open in Workspaces" }).click();
      await expect(page).toHaveURL(new RegExp(`/terminal/${createdWorkspace.id}$`));
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("merge cleanup deletes the active workspace and replaces its terminal history entry", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({
        baseURL: isolatedServer.info.base_url,
      });
      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      const createResponsePromise = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`,
      );
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createResponse = await createResponsePromise;
      expect(createResponse.status()).toBe(202);
      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      await waitForWorkspaceReady(apiContext, createdWorkspace.id);

      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "Open in Workspaces" }).click();
      await expect(page).toHaveURL(new RegExp(`/terminal/${createdWorkspace.id}$`));

      await page.locator(".terminal-view .panel-toggle-btn", { hasText: "PR" }).click();
      const sidebar = page.locator(".right-sidebar");
      await expect(sidebar.locator(".pull-detail")).toBeVisible();
      await sidebar.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal.getByRole("checkbox", { name: "Delete workspace after merge" })).toBeChecked();

      const mergeResponse = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === "/api/v1/pulls/github/acme/widgets/1/merge",
      );
      await modal.getByRole("button", { name: "Merge Anyway" }).click();
      expect((await mergeResponse).status()).toBe(200);

      await expect(page).toHaveURL(/\/workspaces$/);
      expect((await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`)).status()).toBe(404);
      await page.goBack();
      await expect(page).toHaveURL(/\/pulls\/github\/acme\/widgets\/1$/);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("merging a pull request deletes its linked workspace by default", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({ baseURL: isolatedServer.info.base_url });
      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      const createResponsePromise = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`,
      );
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createResponse = await createResponsePromise;
      expect(createResponse.status()).toBe(202);
      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      await waitForWorkspaceReady(apiContext, createdWorkspace.id);

      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await launcher.getByRole("button", { name: "Close" }).click();
      await expect(launcher).toBeHidden();

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();
      await expect(modal.getByRole("checkbox", { name: "Delete workspace after merge" })).toBeChecked();
      const mergeRequestPromise = page.waitForRequest((request) => {
        const url = new URL(request.url());
        return request.method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      const mergeResponsePromise = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      const immediateMerge = modal.getByRole("button", { name: "Merge Anyway" });
      await expect(immediateMerge).toBeVisible();
      await immediateMerge.click();

      const mergeRequest = await mergeRequestPromise;
      expect(mergeRequest.postDataJSON()).toMatchObject({ delete_workspace_id: createdWorkspace.id });
      expect((await mergeResponsePromise).status()).toBe(200);
      await expect
        .poll(async () => (await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`)).status())
        .toBe(404);
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toHaveCount(0);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("deferred merge completion clears the workspace identity after confirmed cleanup", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({ baseURL: isolatedServer.info.base_url });
      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      const createResponsePromise = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`,
      );
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createdWorkspace = (await (await createResponsePromise).json()) as WorkspaceStatusResponse;
      await waitForWorkspaceReady(apiContext, createdWorkspace.id);
      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await launcher.getByRole("button", { name: "Close" }).click();

      const pendingCI = await apiContext.post("/__e2e/pr-ci-state/pending");
      expect(pendingCI.ok(), await pendingCI.text()).toBe(true);
      await page.reload();
      await expect(page.locator(".pull-detail")).toBeVisible();
      const reloadedLauncher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(reloadedLauncher).toBeVisible();
      await reloadedLauncher.getByRole("button", { name: "Close" }).click();
      await expect(reloadedLauncher).toBeHidden();
      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal.getByRole("checkbox", { name: "Delete workspace after merge" })).toBeChecked();
      const deferredRequest = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === "/api/v1/pulls/github/acme/widgets/1/merge/deferred",
      );
      const deferredResponse = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === "/api/v1/pulls/github/acme/widgets/1/merge/deferred",
      );
      await modal.getByRole("button", { name: "Merge after CI is complete" }).click();
      expect((await deferredRequest).postDataJSON()).toMatchObject({ delete_workspace_id: createdWorkspace.id });
      expect((await deferredResponse).status()).toBe(202);

      const successfulCI = await apiContext.post("/__e2e/pr-ci-state/success");
      expect(successfulCI.ok(), await successfulCI.text()).toBe(true);
      await expect
        .poll(async () => (await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`)).status(), {
          // The production deferred-merge worker polls once per minute. If
          // its first refresh wins the race with the fixture's CI update,
          // the next authoritative completion is intentionally one tick away.
          timeout: 90_000,
        })
        .toBe(404);
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toHaveCount(0);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("a merge cleanup failure opens and supports force-delete recovery", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({ baseURL: isolatedServer.info.base_url });
      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      const createResponsePromise = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`,
      );
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createResponse = await createResponsePromise;
      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      const readyWorkspace = await waitForWorkspaceReady(apiContext, createdWorkspace.id);
      await writeFile(`${readyWorkspace.worktree_path}/uncommitted-review-note.txt`, "keep this workspace\n", "utf8");

      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await launcher.getByRole("button", { name: "Close" }).click();
      await expect(launcher).toBeHidden();

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal.getByRole("checkbox", { name: "Delete workspace after merge" })).toBeChecked();
      const mergeResponsePromise = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();

      const mergeResponse = await mergeResponsePromise;
      expect(mergeResponse.status()).toBe(200);
      expect(await mergeResponse.json()).toMatchObject({
        workspace_cleanup_pending: true,
      });
      await expect(modal).toBeHidden();
      await expect
        .poll(async () => {
          const response = await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`);
          if (!response.ok()) return null;
          return ((await response.json()) as WorkspaceStatusResponse).status;
        })
        .toBe("deletion_failed");

      await page.goto(`${server.info.base_url}/workspaces`);
      await expect(page.locator(".workspace-lifecycle-state--deletion_failed")).toHaveText("Deletion failed");

      const failedWorkspaceRow = page.locator(".ws-row").filter({
        has: page.locator(".workspace-lifecycle-state--deletion_failed"),
      });
      await failedWorkspaceRow.click();
      await expect(page).toHaveURL(new RegExp(`/terminal/${createdWorkspace.id}$`));
      await expect(page.getByRole("img", { name: "Workspace deletion failed" })).toBeVisible();
      await expect(page.getByRole("button", { name: "Force delete workspace" })).toBeVisible();

      await page.goto(`${server.info.base_url}/workspaces`);
      await failedWorkspaceRow.click({ button: "right" });
      await page.getByRole("menuitem", { name: "Force delete workspace..." }).click();

      const forceDeleteDialog = page.getByRole("dialog", { name: "Force delete workspace?" });
      await expect(forceDeleteDialog).toBeVisible();
      const forceDeleteResponsePromise = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          response.request().method() === "DELETE" &&
          url.pathname === `/api/v1/workspaces/${createdWorkspace.id}` &&
          url.searchParams.get("force") === "true"
        );
      });
      await forceDeleteDialog.getByRole("button", { name: "Force delete workspace" }).click();
      expect((await forceDeleteResponsePromise).status()).toBe(204);

      await expect
        .poll(async () => (await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`)).status())
        .toBe(404);
      await expect
        .poll(async () => {
          try {
            await access(readyWorkspace.worktree_path);
            return true;
          } catch {
            return false;
          }
        })
        .toBe(false);
      await expect(failedWorkspaceRow).toHaveCount(0);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("a provider response that did not merge keeps the linked workspace available", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({ baseURL: isolatedServer.info.base_url });
      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      const createResponsePromise = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`,
      );
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createdWorkspace: WorkspaceStatusResponse = await (await createResponsePromise).json();
      await waitForWorkspaceReady(apiContext, createdWorkspace.id);

      const launcher = page.getByRole("dialog", { name: "Launch a session" });
      await expect(launcher).toBeVisible();
      await launcher.getByRole("button", { name: "Close" }).click();

      const configure = await page.request.post(`${server.info.base_url}/__e2e/merge/not-merged`);
      expect(configure.status()).toBe(204);

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      const mergeResponsePromise = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();

      const mergeResponse = await mergeResponsePromise;
      expect(mergeResponse.status()).toBe(200);
      expect(await mergeResponse.json()).toMatchObject({
        merged: false,
        message: "provider did not merge the pull request",
      });
      await expect(modal).toBeVisible();
      await expect(modal).toContainText("provider did not merge the pull request");
      expect((await apiContext.get(`/api/v1/workspaces/${createdWorkspace.id}`)).status()).toBe(200);
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("activity feed hosts a created workspace in its workspace pane", async ({ page }) => {
    test.skip(
      !hasCommand("git") || !hasCommand("tmux", ["-V"]),
      "git and tmux are required for the real workspace flow",
    );

    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedWorkspaceE2EServer();
      api = await playwrightRequest.newContext({
        baseURL: isolatedServer.info.base_url,
      });

      const server = isolatedServer;
      const apiContext = api;

      // The Activity surface's claim path runs through the embedded list
      // views inside the split shell: selection -> detail load -> claim ->
      // outer dock -> hosted terminal. Component tests stub the claim-owning
      // children, so this is the only place the real chain is exercised.
      await page.goto(`${server.info.base_url}/activity`);
      const prRow = page
        .locator(".activity-row")
        .filter({ has: page.locator(".badge", { hasText: "PR" }) })
        .filter({ hasText: "Add widget caching layer" })
        .first();
      await prRow.click();
      await expect(page.locator(".activity-shell.activity-shell--split")).toBeVisible();

      const createResponsePromise = page.waitForResponse((response) => {
        return response.request().method() === "POST" && response.url() === `${server.info.base_url}/api/v1/workspaces`;
      });
      await page.getByRole("button", { name: "Create Workspace", exact: true }).filter({ visible: true }).click();
      const createResponse = await createResponsePromise;
      expect(createResponse.status()).toBe(202);
      const createdWorkspace = (await createResponse.json()) as WorkspaceStatusResponse;
      expect(createdWorkspace.item_type).toBe("pull_request");

      // The workspace claims Activity's own outer dock: the host reparents
      // into its slot and the terminal view stays live on the activity page
      // (the selection URL, not /terminal).
      await expect(page).toHaveURL(/\?selected=pr%3A1/);
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();
      await expect(page.locator(".detail-pane-workspace-slot .terminal-view")).toBeVisible();

      await waitForWorkspaceReady(apiContext, createdWorkspace.id);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });

  test("PR detail shows approve workflows after real pending workflow sync", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const seedResponse = await page.request.post(`${server.info.base_url}/__e2e/pr-workflow-approval/required`);
      expect(seedResponse.ok()).toBe(true);

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);

      await expect(page.getByRole("button", { name: "Approve workflows" })).toBeVisible({
        timeout: 10_000,
      });

      const detail = page.locator(".pull-detail-content");
      await detail.evaluate((element) => {
        element.style.width = "350px";
        element.style.flex = "0 0 350px";
      });

      const actions = detail.getByRole("button", { name: "Actions" });
      await expect(actions).toBeVisible();
      await actions.click();
      await expect(detail.getByRole("button", { name: "Approve workflows" })).toBeVisible();
    } finally {
      await server.stop();
    }
  });

  test("issue workspace button still creates a kenn-forge workspace after detail sync refresh", async ({ page }) => {
    const createdWorkspace = {
      id: "ws-issue-10",
      platform_host: "github.com",
      repo_owner: "acme",
      repo_name: "widgets",
      item_type: "issue",
      item_number: 10,
      git_head_ref: "kenn-forge/issue-10",
      worktree_path: "/tmp/workspaces/issue-10",
      tmux_session: "kenn-forge-ws-issue-10",
      status: "ready",
      created_at: "2026-04-20T12:00:00Z",
      mr_title: "Add keyboard shortcut docs",
      mr_state: "open",
    };
    let createCalls = 0;
    await page.route("**/api/v1/issues/github/acme/widgets/10/workspace", async (route) => {
      createCalls += 1;
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify(createdWorkspace),
      });
    });
    // The real create inserts the workspace row before returning 202, so a
    // real detail fetch after creation always carries the workspace ref.
    // The mocked create must keep the detail envelope consistent: the
    // client rightly treats a post-create envelope WITHOUT the workspace
    // as authoritative absence (deleted elsewhere) and drops the ref.
    await page.route(
      (url) => url.pathname === "/api/v1/issues/github/acme/widgets/10",
      async (route) => {
        if (route.request().method() !== "GET" || createCalls === 0) {
          await route.fallback();
          return;
        }
        try {
          const response = await route.fetch();
          const body = (await response.json()) as Record<string, unknown>;
          body.workspace = { id: createdWorkspace.id, status: createdWorkspace.status };
          await route.fulfill({ response, body: JSON.stringify(body) });
        } catch {
          // Page teardown can dispose the fetched response while a
          // polling refetch is mid-handler; the request no longer
          // matters then.
          await route.continue().catch(() => {});
        }
      },
    );
    await page.route("**/api/v1/workspaces/ws-issue-10", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(createdWorkspace),
      });
    });
    await page.route("**/api/v1/workspaces", async (route) => {
      if (route.request().method() !== "GET") {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaces: [createdWorkspace] }),
      });
    });
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });

    const syncResponsePromise = page.waitForResponse((response) => {
      const url = response.url();
      return response.request().method() === "POST" && url.endsWith("/api/v1/issues/github/acme/widgets/10/sync/async");
    });
    const createResponsePromise = page.waitForResponse((response) => {
      const url = response.url();
      return response.request().method() === "POST" && url.endsWith("/api/v1/issues/github/acme/widgets/10/workspace");
    });

    await page.goto("/issues/github/acme/widgets/10");
    await expect(page.locator(".issue-detail")).toBeVisible();

    // Background sync enqueues asynchronously; the server returns 202
    // with no body. The platform host defaults to github.com server-side
    // when the URL has no platform_host query parameter.
    const syncResponse = await syncResponsePromise;
    expect(syncResponse.status()).toBe(202);
    expect(new URL(syncResponse.url()).searchParams.has("platform_host")).toBe(false);

    await page.getByRole("button", { name: "Create Workspace", exact: true }).click();
    const createResponse = await createResponsePromise;
    expect(createResponse.status()).toBe(202);
    expect(createCalls).toBe(1);
    // The workspace lands in its inline workspace pane; the issue stays selected.
    await expect(page).toHaveURL(/\/issues\/github\/acme\/widgets\/10$/);
    // The pane tree renders before any claim; the workspace slot (and the
    // reparented workspace host inside it) exists only once the created
    // workspace actually claims and hosts the pane.
    await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();
  });

  for (const scenario of [
    {
      name: "reuse the existing branch",
      branch: "kenn-forge/issue-10",
      button: "Use Existing Branch",
      reusePayload: { reuse_existing_branch: true },
      existingDirectory: false,
    },
    {
      name: "recover the existing Kenn Forge directory",
      branch: "kenn-forge/issue-10-original-title",
      button: "Use Existing Directory",
      reusePayload: { reuse_existing_directory: true },
      existingDirectory: true,
    },
  ] as const) {
    test(`issue workspace conflict dialog can ${scenario.name}`, async ({ page }) => {
      const createdWorkspace = {
        id: "ws-issue-10",
        platform_host: "github.com",
        repo_owner: "acme",
        repo_name: "widgets",
        item_type: "issue",
        item_number: 10,
        git_head_ref: scenario.branch,
        worktree_path: "/tmp/workspaces/issue-10",
        tmux_session: "kenn-forge-ws-issue-10",
        status: "ready",
        created_at: "2026-04-20T12:00:00Z",
        mr_title: "Add keyboard shortcut docs",
        mr_state: "open",
      };
      const conflict = {
        type: "urn:kenn-forge:error:issue-workspace-branch-conflict",
        title: "Issue workspace branch conflict",
        status: 409,
        code: "branchConflict",
        detail: "A local branch with the requested name already exists.",
        details: {
          existingDirectory: scenario.existingDirectory,
          branch: scenario.branch,
          suggestedBranch: `${scenario.branch}-2`,
        },
        errors: [
          { message: "Requested branch already exists", location: "body.git_head_ref", value: scenario.branch },
          {
            message: "Suggested alternative branch name",
            location: "body.suggested_git_head_ref",
            value: `${scenario.branch}-2`,
          },
        ],
      };

      const payloads: Record<string, unknown>[] = [];
      let workspaceCreated = false;
      await page.route("**/api/v1/issues/github/acme/widgets/10/workspace", async (route) => {
        payloads.push(JSON.parse(route.request().postData() ?? "{}") as Record<string, unknown>);
        if (payloads.length === 1) {
          await route.fulfill({
            status: 409,
            contentType: "application/problem+json",
            body: JSON.stringify(conflict),
          });
          return;
        }
        workspaceCreated = true;
        await route.fulfill({
          status: 202,
          contentType: "application/json",
          body: JSON.stringify(createdWorkspace),
        });
      });
      // The real create inserts the workspace row before returning 202, so a
      // real detail fetch after creation always carries the workspace ref.
      // The mocked create must keep the detail envelope consistent: the
      // client rightly treats a post-create envelope WITHOUT the workspace
      // as authoritative absence (deleted elsewhere) and drops the ref.
      await page.route(
        (url) => url.pathname === "/api/v1/issues/github/acme/widgets/10",
        async (route) => {
          if (route.request().method() !== "GET" || !workspaceCreated) {
            await route.fallback();
            return;
          }
          try {
            const response = await route.fetch();
            const body = (await response.json()) as Record<string, unknown>;
            body.workspace = { id: createdWorkspace.id, status: createdWorkspace.status };
            await route.fulfill({ response, body: JSON.stringify(body) });
          } catch {
            // Page teardown can dispose the fetched response while a
            // polling refetch is mid-handler; the request no longer
            // matters then.
            await route.continue().catch(() => {});
          }
        },
      );
      await page.route("**/api/v1/workspaces/ws-issue-10", async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(createdWorkspace),
        });
      });
      await page.route("**/api/v1/workspaces", async (route) => {
        if (route.request().method() !== "GET") {
          await route.continue();
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [createdWorkspace] }),
        });
      });
      await page.route("**/api/v1/events", async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "text/event-stream",
          body: "",
        });
      });

      await page.goto("/issues/github/acme/widgets/10");
      await expect(page.locator(".issue-detail")).toBeVisible();

      await page.getByRole("button", { name: "Create Workspace", exact: true }).click();

      const dialog = page.getByRole("dialog", {
        name: scenario.existingDirectory ? "Existing Workspace Directory" : "Branch Name Conflict",
      });
      await expect(dialog).toBeVisible();
      await expect(dialog).toContainText(scenario.branch);
      if (scenario.existingDirectory) {
        await expect(dialog.getByRole("button", { name: "Use Existing Branch" })).toHaveCount(0);
        await expect(dialog.getByRole("button", { name: "Create New Branch" })).toHaveCount(0);
        await expect(dialog.locator("#issue-workspace-branch-name")).toHaveCount(0);
      } else {
        await expect(dialog.locator("#issue-workspace-branch-name")).toHaveValue(`${scenario.branch}-2`);
      }

      await dialog.getByRole("button", { name: scenario.button }).click();

      await expect
        .poll(() => payloads)
        .toEqual([
          {},
          {
            git_head_ref: scenario.branch,
            ...scenario.reusePayload,
          },
        ]);
      // The workspace lands in its inline workspace pane; the issue stays selected.
      await expect(page).toHaveURL(/\/issues\/github\/acme\/widgets\/10$/);
      // The pane tree renders before any claim; the workspace slot (and the
      // reparented workspace host inside it) exists only once the created
      // workspace actually claims and hosts the pane.
      await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();
    });
  }

  test("issue workspace conflict dialog can create a new suggested branch", async ({ page }) => {
    const createdWorkspace = {
      id: "ws-issue-10",
      platform_host: "github.com",
      repo_owner: "acme",
      repo_name: "widgets",
      item_type: "issue",
      item_number: 10,
      git_head_ref: "kenn-forge/issue-10-2",
      worktree_path: "/tmp/workspaces/issue-10-2",
      tmux_session: "kenn-forge-ws-issue-10",
      status: "ready",
      created_at: "2026-04-20T12:00:00Z",
      mr_title: "Add keyboard shortcut docs",
      mr_state: "open",
    };
    const conflict = {
      type: "urn:kenn-forge:error:issue-workspace-branch-conflict",
      title: "Issue workspace branch conflict",
      status: 409,
      code: "branchConflict",
      detail: "A local branch with the requested name already exists.",
      details: {
        branch: "kenn-forge/issue-10",
        suggestedBranch: "kenn-forge/issue-10-2",
        existingDirectory: false,
      },
      errors: [
        { message: "Requested branch already exists", location: "body.git_head_ref", value: "kenn-forge/issue-10" },
        {
          message: "Suggested alternative branch name",
          location: "body.suggested_git_head_ref",
          value: "kenn-forge/issue-10-2",
        },
      ],
    };

    const payloads: Record<string, unknown>[] = [];
    let workspaceCreated = false;
    await page.route("**/api/v1/issues/github/acme/widgets/10/workspace", async (route) => {
      payloads.push(JSON.parse(route.request().postData() ?? "{}") as Record<string, unknown>);
      if (payloads.length === 1) {
        await route.fulfill({
          status: 409,
          contentType: "application/problem+json",
          body: JSON.stringify(conflict),
        });
        return;
      }
      workspaceCreated = true;
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify(createdWorkspace),
      });
    });
    // The real create inserts the workspace row before returning 202, so a
    // real detail fetch after creation always carries the workspace ref.
    // The mocked create must keep the detail envelope consistent: the
    // client rightly treats a post-create envelope WITHOUT the workspace
    // as authoritative absence (deleted elsewhere) and drops the ref.
    await page.route(
      (url) => url.pathname === "/api/v1/issues/github/acme/widgets/10",
      async (route) => {
        if (route.request().method() !== "GET" || !workspaceCreated) {
          await route.fallback();
          return;
        }
        try {
          const response = await route.fetch();
          const body = (await response.json()) as Record<string, unknown>;
          body.workspace = { id: createdWorkspace.id, status: createdWorkspace.status };
          await route.fulfill({ response, body: JSON.stringify(body) });
        } catch {
          // Page teardown can dispose the fetched response while a
          // polling refetch is mid-handler; the request no longer
          // matters then.
          await route.continue().catch(() => {});
        }
      },
    );
    await page.route("**/api/v1/workspaces/ws-issue-10", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(createdWorkspace),
      });
    });
    await page.route("**/api/v1/workspaces", async (route) => {
      if (route.request().method() !== "GET") {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaces: [createdWorkspace] }),
      });
    });
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });

    await page.goto("/issues/github/acme/widgets/10");
    await expect(page.locator(".issue-detail")).toBeVisible();

    await page.getByRole("button", { name: "Create Workspace", exact: true }).click();

    const dialog = page.getByRole("dialog", {
      name: "Branch Name Conflict",
    });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Create New Branch" }).click();

    await expect
      .poll(() => payloads)
      .toEqual([
        {},
        {
          git_head_ref: "kenn-forge/issue-10-2",
        },
      ]);
    // The workspace lands in its inline workspace pane; the issue stays selected.
    await expect(page).toHaveURL(/\/issues\/github\/acme\/widgets\/10$/);
    // The pane tree renders before any claim; the workspace slot (and the
    // reparented workspace host inside it) exists only once the created
    // workspace actually claims and hosts the pane.
    await expect(page.locator(".detail-pane-workspace-slot .workspace-host-wrapper")).toBeVisible();
  });

  test("supported pull request actions use shared ActionButton component", async ({ page }) => {
    await page.goto("/pulls");
    await page.locator(".pull-item").first().waitFor({ state: "visible", timeout: 10_000 });
    await page.locator(".pull-item").filter({ hasText: "Add widget caching layer" }).first().click();
    await expect(page.locator(".pull-detail")).toBeVisible();

    const approve = activePullAction(page, ".btn--approve");
    const merge = activePullAction(page, ".btn--merge");
    const close = activePullAction(page, ".btn--close");

    await expect(approve).toBeVisible();
    await expect(merge).toBeVisible();
    await expect(close).toBeVisible();

    // All action buttons use the shared kit-button base class
    for (const btn of [approve, merge, close]) {
      const classes = await btn.getAttribute("class");
      expect(classes).toContain("kit-button");
    }
  });

  test("pending CI merge action enqueues deferred merge and closes the modal", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();

      const deferResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          response.request().method() === "POST" &&
          url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge/deferred"
        );
      });
      await modal.getByRole("button", { name: "Merge after CI is complete" }).click();
      expect((await deferResponse).status()).toBe(202);

      await expect(modal).toHaveCount(0);
      await expect(page.locator(".pull-detail")).toBeVisible();

      // While the background merge waits on CI, the merge action reads as
      // queued. It stays clickable so the user can force an immediate
      // merge, but a second deferred queue is not offered (it would 409).
      const queued = page.getByRole("button", { name: "Merge queued" });
      await expect(queued).toBeVisible();
      await expect(queued).toBeEnabled();
      await queued.click();
      const reopened = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(reopened).toBeVisible();
      await expect(reopened.getByRole("button", { name: "Merge after CI is complete" })).toHaveCount(0);

      // Completing the immediate merge from the queued state must go
      // through the real /merge endpoint, supersede the queued worker,
      // and land the merged state in the UI.
      const mergeResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await reopened.getByRole("button", { name: "Squash and merge" }).click();
      expect((await mergeResponse).status()).toBe(200);
      await expect(reopened).toHaveCount(0);

      // The merged PR has no merge action at all — queued or otherwise —
      // and the detail header reflects the merged state.
      await expect(page.locator(".btn--merge")).toHaveCount(0);
      await expect(page.locator(".pull-detail").getByText("Merged", { exact: true }).first()).toBeVisible();
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("pending CI merge override sends the immediate merge request", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;
      const mergeRequestPaths: string[] = [];

      page.on("request", (request) => {
        if (request.method() !== "POST") return;
        const url = new URL(request.url());
        if (url.pathname.includes("/api/v1/pulls/github/acme/widgets/1/merge")) {
          mergeRequestPaths.push(url.pathname);
        }
      });

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();
      await expect(modal.getByRole("button", { name: "Merge after CI is complete" })).toBeVisible();

      const mergeResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();
      expect((await mergeResponse).status()).toBe(200);

      expect(mergeRequestPaths).toEqual(["/api/v1/pulls/github/acme/widgets/1/merge"]);
      await expect(modal).toHaveCount(0);
      await expect(page.locator(".pull-detail")).toBeVisible();
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("aggregate pending CI merge action enqueues deferred merge when check rows are empty", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;
      const seedResponse = await page.request.post(`${baseURL}/__e2e/pr-ci-state/pending-status-only`);
      expect(seedResponse.ok()).toBe(true);

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      await page.locator(".btn--merge").first().click();
      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();

      const deferResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          response.request().method() === "POST" &&
          url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge/deferred"
        );
      });
      await modal.getByRole("button", { name: "Merge after CI is complete" }).click();
      expect((await deferResponse).status()).toBe(202);

      await expect(modal).toHaveCount(0);
      await expect(page.locator(".pull-detail")).toBeVisible();
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("rejected merge keeps the edited modal retryable and pull request open", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const failure = await page.request.post(`${server.info.base_url}/__e2e/merge/fail`);
      expect(failure.status()).toBe(204);

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await page.locator(".btn--merge").first().click();

      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();
      await modal.getByLabel("Commit title").fill("Preserve this title");
      await modal.getByLabel("Commit message").fill("Preserve this message");

      const mergeResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();
      expect((await mergeResponse).status()).toBe(502);

      await expect(page.locator(".kit-flash-stack").getByRole("status")).toContainText("provider rejected merge");
      await expect(modal).toBeVisible();
      await expect(modal.getByLabel("Commit title")).toHaveValue("Preserve this title");
      await expect(modal.getByLabel("Commit message")).toHaveValue("Preserve this message");

      await page.reload();
      await expect(page.locator(".pull-detail")).toBeVisible();
      await expect(page.getByRole("button", { name: "State: Open" })).toBeVisible();
    } finally {
      await server.stop();
    }
  });

  test("typed not-open merge conflict closes stale fields and blocks retry", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await page.locator(".btn--merge").first().click();

      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();
      await modal.getByLabel("Commit title").fill("Stale title must not retry");

      const conflict = await page.request.post(`${server.info.base_url}/__e2e/merge/conflict/not-open`);
      expect(conflict.status()).toBe(204);

      let mergeRequests = 0;
      page.on("request", (request) => {
        if (
          request.method() === "POST" &&
          new URL(request.url()).pathname === "/api/v1/pulls/github/acme/widgets/1/merge"
        ) {
          mergeRequests += 1;
        }
      });
      const mergeResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();
      const response = await mergeResponse;
      expect(response.status()).toBe(409);
      expect(await response.json()).toMatchObject({ details: { reason: "not_open" } });

      await expect(modal).toHaveCount(0);
      await expect(page.locator(".action-error--state")).toContainText("no longer open");
      await expect(page.locator(".btn--merge")).toHaveCount(0);
      expect(mergeRequests).toBe(1);

      await page.keyboard.press("m");
      await expect.poll(() => mergeRequests).toBe(1);
      await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
    } finally {
      await server.stop();
    }
  });

  test("typed stale-head conflict blocks retry until reviewed state is refreshed", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await page.locator(".btn--merge").first().click();

      const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
      await expect(modal).toBeVisible();
      const conflict = await page.request.post(`${server.info.base_url}/__e2e/merge/conflict/stale-head`);
      expect(conflict.status()).toBe(204);
      let blockConflictSync = true;
      await page.route("**/api/v1/pulls/github/acme/widgets/1/sync", async (route) => {
        if (blockConflictSync) {
          await route.abort("connectionfailed");
        } else {
          await route.continue();
        }
      });

      let mergeRequests = 0;
      page.on("request", (request) => {
        if (
          request.method() === "POST" &&
          new URL(request.url()).pathname === "/api/v1/pulls/github/acme/widgets/1/merge"
        ) {
          mergeRequests += 1;
        }
      });
      const mergeResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === "/api/v1/pulls/github/acme/widgets/1/merge";
      });
      await modal.getByRole("button", { name: "Merge Anyway" }).click();
      const response = await mergeResponse;
      expect(response.status()).toBe(409);
      expect(await response.json()).toMatchObject({ details: { reason: "stale_state" } });

      await expect(modal).toHaveCount(0);
      await expect(page.locator(".action-error--state")).toContainText("head commit changed");
      await expect(page.locator(".btn--merge").first()).toBeDisabled();
      await expect(page.getByRole("button", { name: /^approve$/i }).first()).toBeDisabled();

      await page.keyboard.press("m");
      await expect.poll(() => mergeRequests).toBe(1);
      await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);

      const refresh = page.getByRole("button", { name: "Refresh reviewed state" });
      await expect(refresh).toBeEnabled();
      await refresh.click();
      await expect(page.getByRole("alert")).toContainText("Could not refresh the pull request");
      await expect(page.locator(".btn--merge").first()).toBeDisabled();
      await expect(page.getByRole("button", { name: /^approve$/i }).first()).toBeDisabled();

      blockConflictSync = false;
      await refresh.click();

      await expect(page.locator(".action-error--state")).toHaveCount(0);
      await expect(page.locator(".btn--merge").first()).toBeEnabled();
      await expect(page.getByRole("button", { name: /^approve$/i }).first()).toBeEnabled();
      expect(mergeRequests).toBe(1);

      const detailResponse = await page.request.get(`${server.info.base_url}/api/v1/pulls/github/acme/widgets/1`);
      expect(detailResponse.ok()).toBe(true);
      expect(await detailResponse.json()).toMatchObject({ merge_request: { State: "open" } });
    } finally {
      await server.stop();
    }
  });

  test("repo merge permission disables merge action with reason end-to-end", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;

      const seedResponse = await page.request.post(`${baseURL}/__e2e/repo-settings/viewer-can-merge/deny`);
      expect(seedResponse.ok()).toBe(true);

      const settingsResponse = await page.request.get(`${baseURL}/api/v1/repo/github/acme/widgets`);
      expect(settingsResponse.ok()).toBe(true);
      const settings = (await settingsResponse.json()) as {
        ViewerCanMerge: boolean;
        operations: {
          merge_pr: { available: boolean; code?: string };
        };
      };
      expect(settings.ViewerCanMerge).toBe(false);
      expect(settings.operations.merge_pr.available).toBe(false);
      expect(settings.operations.merge_pr.code).toBe("viewer_cannot_merge");

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await expect(page.locator(".detail-title")).toContainText("Add widget caching layer");
      const merge = page.locator(".btn--merge").first();
      await expect(merge).toBeVisible();
      await expect(merge).toBeDisabled();
      await expect(merge).toHaveAttribute("title", "You do not have permission to merge in this repository");
      // Clicking a disabled merge button must not open the merge modal.
      await merge.click({ force: true });
      await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("conflicted pull request disables the merge action", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;

      await page.goto(`${baseURL}/pulls/github/acme/widgets/2`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await expect(page.locator(".detail-title")).toContainText("Fix race condition in event loop");
      await expect(page.getByTestId("merge-warnings-chip")).toContainText("Conflicts");

      const merge = page.locator(".btn--merge").first();
      await expect(merge).toBeDisabled();
      await merge.click({ force: true });
      await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("typed suggestion conflict blocks stale actions through the real server", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServerWithOptions({ freshProcess: true });
      const baseURL = isolatedServer.info.base_url;

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      const commitSuggestion = page.getByRole("button", { name: "Commit suggestion" });
      await expect(commitSuggestion).toBeVisible();

      const closeResponse = await page.request.post(`${baseURL}/__e2e/merge/conflict/not-open`);
      expect(closeResponse.ok()).toBe(true);
      await page.route("**/api/v1/pulls/github/acme/widgets/1/sync", async (route) => {
        await route.fulfill({
          status: 503,
          contentType: "application/problem+json",
          body: JSON.stringify({ detail: "fixture refresh unavailable" }),
        });
      });

      const [conflictResponse] = await Promise.all([
        page.waitForResponse(
          (response) => response.url().endsWith("/review-suggestions/apply") && response.request().method() === "POST",
        ),
        commitSuggestion.click(),
      ]);
      expect(conflictResponse.status()).toBe(409);
      await expect(conflictResponse.json()).resolves.toMatchObject({
        code: "conflict",
        details: { reason: "not_open" },
      });
      await expect(
        page.getByText(
          "This pull request is no longer open. Its current state is being refreshed before any further action.",
        ),
      ).toBeVisible();
      await expect(page.getByText("Could not refresh the pull request. Try again.")).toBeVisible();
      await expect(activePullAction(page, ".btn--approve")).toBeDisabled();
      await expect(activePullAction(page, ".btn--merge")).toBeDisabled();

      await page.unroute("**/api/v1/pulls/github/acme/widgets/1/sync");
      const reopenResponse = await page.request.post(`${baseURL}/__e2e/merge/conflict/open`);
      expect(reopenResponse.ok()).toBe(true);
      await page.getByRole("button", { name: "Refresh reviewed state" }).click();
      await expect(
        page.getByText(
          "This pull request is no longer open. Its current state is being refreshed before any further action.",
        ),
      ).toHaveCount(0);
      await expect(activePullAction(page, ".btn--approve")).toBeEnabled();
      await expect(activePullAction(page, ".btn--merge")).toBeEnabled();
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("delayed successful suggestion reconciles after an A-to-B-to-A route cycle", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServerWithOptions({ freshProcess: true });
      const baseURL = isolatedServer.info.base_url;
      const detailURL = `${baseURL}/api/v1/pulls/github/acme/widgets/1`;
      const initialDetail = (await (await page.request.get(detailURL)).json()) as { platform_head_sha: string };

      let releaseResponse!: () => void;
      const release = new Promise<void>((resolve) => {
        releaseResponse = resolve;
      });
      let markApplied!: () => void;
      const providerApplied = new Promise<void>((resolve) => {
        markApplied = resolve;
      });
      await page.route("**/api/v1/pulls/github/acme/widgets/1/review-suggestions/apply", async (route) => {
        const response = await route.fetch();
        const responseBody = await response.body();
        expect(response.status(), responseBody.toString()).toBe(200);
        markApplied();
        await release;
        await route.fulfill({ response, body: responseBody });
      });

      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      const commitSuggestion = page.getByRole("button", { name: "Commit suggestion" });
      await expect(commitSuggestion).toBeEnabled();
      const configure = await page.request.post(`${baseURL}/__e2e/review-suggestion/succeed`);
      expect(configure.ok()).toBe(true);
      const applying = commitSuggestion.click();
      await providerApplied;

      await page.getByRole("button", { name: /Fix race condition in event loop #2/ }).click();
      await expect(page.locator(".detail-title")).toContainText("Fix race condition in event loop");
      await page.getByRole("button", { name: /Add widget caching layer.*#1/ }).click();
      await expect(page.locator(".detail-title")).toContainText("Add widget caching layer");

      releaseResponse();
      await applying;
      await expect
        .poll(async () => {
          const detail = (await (await page.request.get(detailURL)).json()) as { platform_head_sha: string };
          return detail.platform_head_sha;
        })
        .not.toBe(initialDetail.platform_head_sha);
      await expect(commitSuggestion).toBeDisabled();
      await expect(activePullAction(page, ".btn--approve")).toBeEnabled();
      await expect(activePullAction(page, ".btn--merge")).toBeEnabled();

      await page.reload();
      await expect(page.getByRole("button", { name: "Commit suggestion" })).toBeDisabled();
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("narrow actions menu closes when clicking outside", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/pulls/github/acme/widgets/6");
    await expect(page.locator(".pull-detail")).toBeVisible();

    const actionsTrigger = page.getByRole("button", { name: "Actions", exact: true });
    const actionsMenu = page.locator(".actions-menu-popover");
    await actionsTrigger.click();
    await expect(actionsMenu).toBeVisible();

    await page.locator(".detail-title").click();
    await expect(actionsMenu).toBeHidden();
    await expect(actionsTrigger).toHaveAttribute("aria-expanded", "false");
  });

  test("narrow actions menu shows state change failures after closing", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.route("**/api/v1/pulls/github/acme/widgets/6/github-state", async (route) => {
      await route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ status: 500, code: "internalError", detail: "backend down" }),
      });
    });

    await page.goto("/pulls/github/acme/widgets/6");
    await expect(page.locator(".pull-detail")).toBeVisible();

    const actionsTrigger = page.getByRole("button", { name: "Actions", exact: true });
    const actionsMenu = page.locator(".actions-menu-popover");
    await actionsTrigger.click();
    await page.locator(".actions-menu-popover .btn--close").click();

    await expect(actionsMenu).toBeHidden();
    await expect(actionsTrigger).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(".kit-flash-stack").getByRole("status")).toContainText("backend down");
  });

  test("closing a pull request persists and reconciles detail plus the open list", async ({ page }) => {
    const server = await startIsolatedE2EServer();
    try {
      const baseURL = server.info.base_url;
      await page.goto(`${baseURL}/pulls/github/acme/widgets/6`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      const closeResponse = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          response.url() === `${baseURL}/api/v1/pulls/github/acme/widgets/6/github-state`,
      );

      await activePullAction(page, ".btn--close").click();

      expect((await closeResponse).status()).toBe(200);
      await expect
        .poll(async () => {
          const response = await page.request.get(`${baseURL}/api/v1/pulls/github/acme/widgets/6`);
          const detail = (await response.json()) as { merge_request: { State: string } };
          return detail.merge_request.State;
        })
        .toBe("closed");
      await page.goto(`${baseURL}/pulls`);
      await expect(page.locator(".pull-item").filter({ hasText: "Improve mobile navigation" })).toHaveCount(0);
    } finally {
      await server.stop();
    }
  });

  test("narrow actions menu includes supported approve action", async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto("/pulls/github/acme/widgets/6");
    await expect(page.locator(".pull-detail")).toBeVisible();

    await page.getByRole("button", { name: "Actions", exact: true }).click();
    const menu = page.locator(".actions-menu-popover");
    await expect(menu).toBeVisible();

    await expect(menu.locator(".btn--approve")).toBeVisible();
    await expect(menu.locator(".btn--merge")).toBeVisible();
    await expect(menu.locator(".btn--close")).toBeVisible();
  });

  test("GitHub approve action submits from the review popover", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();

      const baseURL = isolatedServer.info.base_url;
      await page.goto(`${baseURL}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".pull-detail")).toBeVisible();
      await activePullAction(page, ".btn--approve").click();

      const popover = page.getByRole("dialog", { name: "Submit pull request review" });
      await expect(popover).toBeVisible();
      await popover.getByRole("textbox").fill("LGTM from approve e2e");

      const approvalResponse = page.waitForResponse((response) => {
        return (
          response.request().method() === "POST" &&
          response.url() === `${baseURL}/api/v1/pulls/github/acme/widgets/1/approve`
        );
      });
      await popover.getByRole("button", { name: "Approve", exact: true }).click();

      const response = await approvalResponse;
      expect(response.status()).toBe(200);
      expect((await response.json()) as { status?: string }).toMatchObject({
        status: "approved",
      });
      await expect(popover).toHaveCount(0);
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("self-contained actions close the narrow actions menu after success", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      const baseURL = isolatedServer.info.base_url;

      await page.setViewportSize({ width: 320, height: 720 });
      await page.goto(`${baseURL}/pulls/github/acme/widgets/6`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      await page.getByRole("button", { name: "Actions", exact: true }).click();
      await expect(page.locator(".actions-menu-popover")).toBeVisible();

      const readyResponse = page.waitForResponse((response) => {
        return (
          response.request().method() === "POST" &&
          response.url() === `${baseURL}/api/v1/pulls/github/acme/widgets/6/ready-for-review`
        );
      });
      await page.locator(".actions-menu-popover .btn--ready").click();
      expect((await readyResponse).status()).toBe(200);
      await expect(page.locator(".actions-menu-popover")).toHaveCount(0);
    } finally {
      await isolatedServer?.stop();
    }
  });

  test("draft pull request actions keep exactly the same height", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/6");
    await expect(page.locator(".pull-detail")).toBeVisible();

    const ready = activePullAction(page, ".btn--ready");
    const approve = activePullAction(page, ".btn--approve");
    const merge = activePullAction(page, ".btn--merge");
    const close = activePullAction(page, ".btn--close");

    for (const btn of [ready, approve, merge, close]) {
      await expect(btn).toBeVisible();
    }

    const metrics = await page.evaluate(() => {
      const selectors = [".btn--ready", ".btn--approve", ".btn--merge", ".btn--close"];
      return selectors.map((selector) => {
        const element = document.querySelector(selector);
        if (!(element instanceof HTMLElement)) {
          throw new Error(`missing action button: ${selector}`);
        }
        const rect = element.getBoundingClientRect();
        return {
          selector,
          height: element.offsetHeight,
          top: Math.round(rect.top),
          left: Math.round(rect.left),
          right: Math.round(rect.right),
        };
      });
    });

    expect(metrics.map((metric) => metric.height)).toEqual(Array(metrics.length).fill(metrics[0]?.height));
    expect(new Set(metrics.map((metric) => metric.top)).size).toBe(1);
    expect(metrics.slice(0, -1).map((metric, index) => metrics[index + 1]!.left - metric.right)).toEqual(
      Array(metrics.length - 1).fill(metrics[1] ? metrics[1].left - metrics[0]!.right : 0),
    );
  });

  test("primary and workspace action rows keep their vertical gap", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1");
    const liveActions = page.locator(".primary-actions-live");
    const primaryRow = liveActions.locator(".actions-row--primary");
    const workspaceRow = liveActions.locator(".actions-row--workspace");
    await expect(primaryRow).toBeVisible();
    await expect(workspaceRow).toBeVisible();

    const [primaryBox, workspaceBox] = await Promise.all([primaryRow.boundingBox(), workspaceRow.boundingBox()]);
    expect(primaryBox).not.toBeNull();
    expect(workspaceBox).not.toBeNull();
    expect(workspaceBox!.y - (primaryBox!.y + primaryBox!.height)).toBeGreaterThanOrEqual(8);
  });

  test("medium pull detail uses the compact Kit UI fit stage", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/6");
    const detail = page.locator(".pull-detail-content");
    await expect(detail).toBeVisible();
    await detail.evaluate((element) => {
      element.style.width = "400px";
      element.style.flex = "0 0 400px";
    });

    const fitStages = detail.locator(".kit-fit-stages");
    await expect(fitStages).toHaveCount(1);
    const activeRow = detail.locator(".primary-actions-live > .actions-row--primary");
    await expect(activeRow.locator(".btn--ready .kit-button__label-text")).toHaveText("Ready");
    await expect(activeRow.locator(".kit-button__short-label")).toHaveCount(0);
    await expect(detail.locator(".actions-menu-wrap > .actions-menu-trigger")).toBeHidden();

    const metrics = await activeRow.evaluate((row) => {
      const selectors = [".btn--ready", ".btn--approve", ".btn--merge", ".btn--close"];
      return selectors.map((selector) => {
        const button = row.querySelector<HTMLElement>(selector);
        const label = button?.querySelector<HTMLElement>(".kit-button__label");
        const labelText = label?.querySelector<HTMLElement>(".kit-button__label-text");
        const icon = button?.querySelector<SVGElement>("svg");
        if (!button || !label || !labelText || !icon) {
          throw new Error(`incomplete Kit UI action button: ${selector}`);
        }
        const buttonRect = button.getBoundingClientRect();
        const textRect = labelText.getBoundingClientRect();
        return {
          selector,
          labelDisplay: getComputedStyle(label).display,
          iconDisplay: getComputedStyle(icon).display,
          centerDelta: Math.abs(textRect.top + textRect.height / 2 - (buttonRect.top + buttonRect.height / 2)),
        };
      });
    });

    for (const metric of metrics) {
      expect(metric.labelDisplay, metric.selector).not.toBe("none");
      expect(metric.iconDisplay, metric.selector).not.toBe("none");
      expect(metric.centerDelta, metric.selector).toBeLessThan(0.5);
    }
  });

  test("pull action state survives a measured stage change", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1");
    const detail = page.locator(".pull-detail-content");
    await expect(detail).toBeVisible();
    await detail.evaluate((element) => {
      element.style.width = "800px";
      element.style.flex = "0 0 800px";
    });

    await expect(detail.locator(".kit-fit-stages .approve-section")).toHaveCount(0);
    await activePullAction(page, ".btn--approve").click();
    const dialog = page.getByRole("dialog", { name: "Submit pull request review" });
    const comment = dialog.getByPlaceholder("Leave an optional comment…");
    await comment.fill("Keep this review draft while the pane resizes.");

    await detail.evaluate((element) => {
      element.style.width = "400px";
      element.style.flex = "0 0 400px";
    });

    await expect(dialog).toBeVisible();
    await expect(comment).toHaveValue("Keep this review draft while the pane resizes.");
    await expect(activePullAction(page, ".btn--approve")).toHaveCount(1);
  });

  test("ready for review updates API state and removes the draft action", async ({ page }) => {
    let isolatedServer: IsolatedE2EServer | null = null;
    let api: APIRequestContext | null = null;
    try {
      isolatedServer = await startIsolatedE2EServer();
      api = await playwrightRequest.newContext({
        baseURL: isolatedServer.info.base_url,
      });

      const server = isolatedServer;
      const apiContext = api;

      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/6`);
      await expect(page.locator(".pull-detail")).toBeVisible();

      const readyResponsePromise = page.waitForResponse((response) => {
        const url = response.url();
        return (
          response.request().method() === "POST" &&
          url === `${server.info.base_url}/api/v1/pulls/github/acme/widgets/6/ready-for-review`
        );
      });

      await activePullAction(page, ".btn--ready").click();

      const readyResponse = await readyResponsePromise;
      expect(readyResponse.status()).toBe(200);
      expect((await readyResponse.json()).status).toBe("ready_for_review");

      await expect(page.locator(".btn--ready")).toHaveCount(0);
      await expect(activePullAction(page, ".btn--approve")).toBeVisible();
      await expect(activePullAction(page, ".btn--merge")).toBeVisible();
      await expect(activePullAction(page, ".btn--close")).toBeVisible();

      await expect
        .poll(async () => {
          const response = await apiContext.get("/api/v1/pulls/github/acme/widgets/6");
          const detail = await response.json();
          return detail.merge_request.IsDraft;
        })
        .toBe(false);
    } finally {
      await api?.dispose();
      await isolatedServer?.stop();
    }
  });
});
