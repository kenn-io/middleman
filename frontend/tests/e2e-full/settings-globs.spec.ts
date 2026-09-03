import { execFileSync } from "node:child_process";
import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, request as playwrightRequest, test, type APIRequestContext } from "@playwright/test";
import type { SettingsResponse } from "../../src/lib/api/generated/models/index.js";
import { startIsolatedE2EServer, type IsolatedE2EServer } from "./support/e2eServer";

type RepoSummary = {
  Platform: string;
  PlatformHost: string;
  Owner: string;
  Name: string;
};

let isolatedServer: IsolatedE2EServer | undefined;
let api: APIRequestContext | undefined;
let localRepo: string | undefined;

function git(dir: string, ...args: string[]): void {
  execFileSync("git", args, { cwd: dir, stdio: "ignore" });
}

test.beforeEach(async () => {
  isolatedServer = await startIsolatedE2EServer();
  api = await playwrightRequest.newContext({
    baseURL: isolatedServer.info.base_url,
  });
});

test.afterEach(async () => {
  await api?.dispose();
  await isolatedServer?.stop();
  api = undefined;
  isolatedServer = undefined;
  if (localRepo) {
    rmSync(localRepo, { recursive: true, force: true });
    localRepo = undefined;
  }
});

test("settings shows glob match counts and refresh updates tracked repos", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);

  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await expect(page.getByRole("button", { name: /^Select repository:/ })).not.toBeAttached();

  const row = page.locator(".repo-row", { hasText: "roborev-dev/*" });
  await expect(row).toContainText("roborev-dev/*");
  await expect(row).toContainText("(3)");
  await expect
    .poll(async () => {
      if (!api) {
        throw new Error("settings-globs API context not initialized");
      }
      const response = await api.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter((repo) => repo.Owner === "roborev-dev")
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("archived,kenn-forge,worker");

  await page.goto(`${isolatedServer!.info.base_url}/pulls`);
  const selector = page.getByRole("button", { name: /^Select repository:/ });
  await expect(selector).toBeVisible();
  await selector.click();
  await expect(page.getByRole("option", { name: /roborev-dev\/kenn-forge/ })).toBeVisible();
  await expect(page.getByRole("option", { name: /roborev-dev\/worker/ })).toBeVisible();
  await page.keyboard.press("Escape");

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await row.getByRole("button", { name: "Refresh" }).click();

  await expect(row).toContainText("(4)");
  await expect
    .poll(async () => {
      if (!api) {
        throw new Error("settings-globs API context not initialized");
      }
      const response = await api.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter((repo) => repo.Owner === "roborev-dev")
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("archived,kenn-forge,review-bot,worker");

  await page.screenshot({
    path: "test-results/settings-globs-pr.png",
    fullPage: true,
  });
});

test("settings imports a selected subset from a repository glob", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await expect(page.getByRole("button", { name: /^Select repository:/ })).not.toBeAttached();

  await page.getByRole("button", { name: "Add repositories…" }).click();
  await expect(page.getByRole("dialog", { name: "Add repositories" })).toBeVisible();
  await page.getByLabel("Repository pattern").fill("import-lab/*");
  await page.getByRole("button", { name: "Preview" }).click();

  await expect(page.getByText("import-lab/api")).toBeVisible();
  await expect(page.getByText("import-lab/worker")).toBeVisible();
  await expect(page.getByText("import-lab/archived")).toHaveCount(0);

  await page.getByLabel("Filter repositories").fill("worker");
  await page.getByRole("button", { name: "None" }).click();
  await page.getByLabel("Filter repositories").fill("");
  await page.getByRole("button", { name: "Add selected repositories" }).click();

  await expect(page.getByRole("dialog", { name: "Add repositories" })).toHaveCount(0);
  await expect(page.locator(".repo-row", { hasText: "import-lab/api" })).toBeVisible();
  await expect(page.locator(".repo-row", { hasText: "import-lab/worker" })).toHaveCount(0);

  await page.goto(`${isolatedServer!.info.base_url}/pulls`);
  const selector = page.getByRole("button", { name: /^Select repository:/ });
  await expect(selector).toBeVisible();
  await selector.click();
  const apiOption = page.getByRole("option", {
    name: /import-lab\/api/,
  });
  await expect(apiOption).toBeVisible();
  await expect(page.getByRole("option", { name: /import-lab\/worker/ })).toHaveCount(0);
  await apiOption.click();
  await expect(apiOption.locator("input[type='checkbox']")).toBeChecked();
  await page.keyboard.press("Escape");
  await expect(selector).toContainText("import-lab/api");
  await expect(selector).toHaveAttribute("aria-label", "Select repository: github/github.com/import-lab/api");

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  if (!api) throw new Error("settings-globs API context not initialized");
  const settingsResponse = await api.get("/api/v1/settings");
  const settings = (await settingsResponse.json()) as {
    repos: Array<{ owner: string; name: string; is_glob: boolean }>;
  };
  const exactNames = settings.repos
    .filter((repo) => repo.owner === "import-lab" && !repo.is_glob)
    .map((repo) => repo.name)
    .sort();
  expect(exactNames).toEqual(["api"]);
  await expect
    .poll(async () => {
      const response = await api!.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter((repo) => repo.Owner === "import-lab")
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("api");

  const repoRow = page.locator(".repo-row", {
    hasText: "import-lab/api",
  });
  await repoRow.getByTitle("Remove github/github.com/import-lab/api").click();
  await repoRow.getByRole("button", { name: "Yes" }).click();

  await expect(repoRow).toHaveCount(0);
  await expect
    .poll(async () => {
      const response = await api!.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter((repo) => repo.Owner === "import-lab")
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("");

  await page.goto(`${isolatedServer!.info.base_url}/pulls`);
  await expect(page.getByRole("button", { name: "Select repository: Global" })).toBeVisible();
});

test("repository import reconciles a bulk add when the committed response is lost", async ({ page }) => {
  let bulkAddCommitted = false;
  await page.route(
    "**/api/v1/repos/bulk",
    async (route) => {
      const response = await route.fetch();
      expect(response.status()).toBe(201);
      bulkAddCommitted = true;
      await route.abort("connectionfailed");
    },
    { times: 1 },
  );

  try {
    await page.goto(`${isolatedServer!.info.base_url}/settings`);
    await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
    await page.getByRole("button", { name: "Add repositories…" }).click();
    const dialog = page.getByRole("dialog", { name: "Add repositories" });
    await dialog.getByLabel("Repository pattern").fill("import-lab/*");
    await dialog.getByRole("button", { name: "Preview" }).click();
    await expect(dialog.getByText("import-lab/api")).toBeVisible();
    await expect(dialog.getByText("import-lab/worker")).toBeVisible();

    await dialog.getByRole("button", { name: "Add selected repositories" }).click();

    await expect.poll(() => bulkAddCommitted).toBe(true);
    await expect(dialog).toHaveCount(0);
    await expect(page.locator(".repo-row", { hasText: "import-lab/api" })).toBeVisible();
    await expect(page.locator(".repo-row", { hasText: "import-lab/worker" })).toBeVisible();

    if (!api) throw new Error("settings-globs API context not initialized");
    const response = await api.get("/api/v1/settings");
    expect(response.ok()).toBe(true);
    const settings: SettingsResponse = await response.json();
    const imported = settings.repos
      .filter((repo) => repo.owner === "import-lab" && !repo.is_glob)
      .map((repo) => repo.name)
      .sort();
    expect(imported).toEqual(["api", "worker"]);
  } finally {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  }
});

test("settings promotes a glob match to a persisted exact repo with a local clone", async ({ page }) => {
  localRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "mm-promote-clone-")));
  git(localRepo, "init");
  git(localRepo, "remote", "add", "origin", "https://github.com/roborev-dev/kenn-forge.git");

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });

  const globRow = page.locator(".repo-row", { hasText: "roborev-dev/*" });
  await globRow.getByRole("button", { name: "Promote glob repository roborev-dev/*" }).click();
  const dialog = page.getByRole("dialog", { name: "Promote wildcard repository" });
  await expect(dialog.getByLabel("Search matches")).toBeFocused();
  await expect(dialog.getByRole("radio", { name: /roborev-dev\/kenn-forge/ })).toBeChecked();

  const saveResponsePromise = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/repo/github/roborev-dev/kenn-forge/worktree-base") &&
      response.request().method() === "PUT",
  );
  await dialog.getByLabel("Local clone path for roborev-dev/kenn-forge", { exact: true }).fill(localRepo);
  await dialog.getByRole("button", { name: "Promote repository" }).click();
  const saveResponse = await saveResponsePromise;
  const saveBody = await saveResponse.text();
  expect(saveResponse.status(), `PUT worktree-base failed: ${saveBody}`).toBe(200);

  await expect(dialog).toHaveCount(0);
  const exactRow = page.locator(".repo-row", { hasText: "roborev-dev/kenn-forge" });
  await expect(exactRow).toBeVisible();
  await exactRow.getByRole("button", { name: "Configure roborev-dev/kenn-forge" }).click();
  await exactRow.getByRole("menuitem", { name: "Edit local clone path" }).click();
  await expect(exactRow.getByLabel("Local clone path for roborev-dev/kenn-forge", { exact: true })).toHaveValue(
    localRepo,
  );

  if (!api) throw new Error("settings-globs API context not initialized");
  const settingsResponse = await api.get("/api/v1/settings");
  const settingsBody = await settingsResponse.text();
  expect(settingsResponse.status(), `GET settings failed: ${settingsBody}`).toBe(200);
  const settings = JSON.parse(settingsBody) as {
    repos: Array<{
      owner: string;
      name: string;
      repo_path: string;
      is_glob: boolean;
      worktree_base_path?: string;
    }>;
  };
  expect(settings.repos).toContainEqual(
    expect.objectContaining({
      owner: "roborev-dev",
      name: "kenn-forge",
      repo_path: "roborev-dev/kenn-forge",
      is_glob: false,
      worktree_base_path: localRepo,
    }),
  );
});

test("settings rolls back a promoted glob match when the local clone path is invalid", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });

  const globRow = page.locator(".repo-row", { hasText: "roborev-dev/*" });
  await globRow.getByRole("button", { name: "Promote glob repository roborev-dev/*" }).click();
  const dialog = page.getByRole("dialog", { name: "Promote wildcard repository" });
  await expect(dialog.getByRole("radio", { name: /roborev-dev\/kenn-forge/ })).toBeChecked();

  const addResponsePromise = page.waitForResponse(
    (response) => response.url().endsWith("/api/v1/repos/bulk") && response.request().method() === "POST",
  );
  const saveResponsePromise = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/repo/github/roborev-dev/kenn-forge/worktree-base") &&
      response.request().method() === "PUT",
  );
  const rollbackResponsePromise = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/repo/github/roborev-dev/kenn-forge") && response.request().method() === "DELETE",
  );

  await dialog
    .getByLabel("Local clone path for roborev-dev/kenn-forge", { exact: true })
    .fill("/missing/promoted/clone");
  await dialog.getByRole("button", { name: "Promote repository" }).click();
  const addResponse = await addResponsePromise;
  const saveResponse = await saveResponsePromise;
  const rollbackResponse = await rollbackResponsePromise;
  expect(addResponse.status(), `POST repos/bulk failed: ${await addResponse.text()}`).toBe(201);
  expect(saveResponse.status(), "invalid worktree path should fail validation").not.toBe(200);
  expect(rollbackResponse.ok(), `DELETE promoted repo returned ${rollbackResponse.status()}`).toBe(true);

  await expect(page.locator(".kit-flash-stack").getByRole("status")).toContainText("path does not exist");

  if (!api) throw new Error("settings-globs API context not initialized");
  await expect
    .poll(async () => {
      const settingsResponse = await api!.get("/api/v1/settings");
      expect(settingsResponse.ok()).toBe(true);
      const settings = (await settingsResponse.json()) as {
        repos: Array<{
          owner: string;
          name: string;
          repo_path: string;
          is_glob: boolean;
        }>;
      };
      return settings.repos.some((repo) => repo.owner === "roborev-dev" && repo.name === "kenn-forge" && !repo.is_glob);
    })
    .toBe(false);
});

test("settings keeps a surviving promoted repository visible when rollback fails", async ({ page }) => {
  await page.route(
    "**/api/v1/repo/github/roborev-dev/kenn-forge",
    async (route) => {
      if (route.request().method() !== "DELETE") {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 502,
        contentType: "application/problem+json",
        body: JSON.stringify({
          title: "Repository removal failed",
          status: 502,
          detail: "repository removal failed",
          code: "upstreamError",
          type: "about:blank",
        }),
      });
    },
    { times: 1 },
  );

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  const globRow = page.locator(".repo-row", { hasText: "roborev-dev/*" });
  await globRow.getByRole("button", { name: "Promote glob repository roborev-dev/*" }).click();
  const dialog = page.getByRole("dialog", { name: "Promote wildcard repository" });
  await expect(dialog.getByRole("radio", { name: /roborev-dev\/kenn-forge/ })).toBeChecked();
  await dialog
    .getByLabel("Local clone path for roborev-dev/kenn-forge", { exact: true })
    .fill("/missing/promoted/clone");
  await dialog.getByRole("button", { name: "Promote repository" }).click();

  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("alert")).toContainText("path does not exist");
  await expect(dialog.getByRole("alert")).toContainText("repository removal failed");
  await expect(page.locator(".repo-row", { hasText: "roborev-dev/kenn-forge" })).toBeVisible();

  if (!api) throw new Error("settings-globs API context not initialized");
  const response = await api.get("/api/v1/settings");
  expect(response.ok()).toBe(true);
  const settings: SettingsResponse = await response.json();
  expect(
    settings.repos.some((repo) => repo.owner === "roborev-dev" && repo.name === "kenn-forge" && !repo.is_glob),
  ).toBe(true);
});

test("fleet and activity settings stay ordered and persist through the real server", async ({ page }) => {
  let markFleetCommitted = () => {};
  const fleetCommitted = new Promise<void>((resolve) => {
    markFleetCommitted = resolve;
  });
  let releaseFleetResponse = () => {};
  const fleetResponseGate = new Promise<void>((resolve) => {
    releaseFleetResponse = resolve;
  });
  let activityWriteCount = 0;

  await page.route("**/api/v1/settings/fleet", async (route) => {
    if (route.request().method() !== "PUT") {
      await route.continue();
      return;
    }
    const response = await route.fetch();
    markFleetCommitted();
    await fleetResponseGate;
    await route.fulfill({ response });
  });
  await page.route("**/api/v1/settings", async (route) => {
    if (route.request().method() === "PUT") activityWriteCount += 1;
    await route.continue();
  });

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await page.getByRole("button", { name: /^Fleet federation / }).click();
  await page.getByLabel("Member request timeout").fill("4s");
  await page.getByRole("button", { name: "Save fleet federation" }).click();
  await fleetCommitted;

  await page.getByRole("button", { name: /^Activity / }).click();
  const hideBots = page.getByRole("button", { name: "Toggle hide bots" });
  const previousHideBots = await hideBots.getAttribute("aria-pressed");
  await hideBots.click();
  await expect(hideBots).toHaveAttribute("aria-pressed", previousHideBots === "true" ? "false" : "true");
  expect(activityWriteCount).toBe(0);

  releaseFleetResponse();
  await expect.poll(() => activityWriteCount).toBe(1);

  if (!api) throw new Error("settings-globs API context not initialized");
  await expect
    .poll(async () => {
      const response = await api!.get("/api/v1/settings");
      expect(response.ok()).toBe(true);
      const settings = (await response.json()) as {
        activity: { hide_bots: boolean };
        fleet: { peer_timeout?: string };
      };
      return `${settings.fleet.peer_timeout ?? ""}:${settings.activity.hide_bots}`;
    })
    .toBe(`4s:${previousHideBots !== "true"}`);

  await page.reload();
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await page.getByRole("button", { name: /^Fleet federation / }).click();
  await expect(page.getByLabel("Member request timeout")).toHaveValue("4s");
});

test("repository import can hide forks and private repositories before adding", async ({ page }) => {
  let bulkRepos:
    | Array<{
        provider: string;
        host: string;
        owner: string;
        name: string;
        repo_path: string;
      }>
    | undefined;
  await page.route("**/api/v1/repos/preview", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        owner: "import-lab",
        pattern: "*",
        repos: [
          {
            provider: "github",
            platform_host: "github.com",
            owner: "import-lab",
            name: "public-source",
            repo_path: "import-lab/public-source",
            description: "Source repository",
            private: false,
            fork: false,
            pushed_at: "2026-04-22T10:00:00Z",
            already_configured: false,
          },
          {
            provider: "github",
            platform_host: "github.com",
            owner: "import-lab",
            name: "private-source",
            repo_path: "import-lab/private-source",
            description: "Private repository",
            private: true,
            fork: false,
            pushed_at: "2026-04-23T10:00:00Z",
            already_configured: false,
          },
          {
            provider: "github",
            platform_host: "github.com",
            owner: "import-lab",
            name: "public-fork",
            repo_path: "import-lab/public-fork",
            description: "Forked repository",
            private: false,
            fork: true,
            pushed_at: "2026-04-24T10:00:00Z",
            already_configured: false,
          },
        ],
      }),
    });
  });
  await page.route("**/api/v1/repos/bulk", async (route) => {
    const body = route.request().postDataJSON() as {
      repos: Array<{
        provider: string;
        host: string;
        owner: string;
        name: string;
        repo_path: string;
      }>;
    };
    bulkRepos = body.repos;
    await route.fulfill({
      contentType: "application/json",
      status: 201,
      body: JSON.stringify({
        repos: [
          {
            owner: "import-lab",
            name: "public-source",
            is_glob: false,
            matched_repo_count: 1,
          },
        ],
        activity: {
          view_mode: "threaded",
          time_range: "7d",
          hide_closed: false,
          hide_bots: false,
        },
        terminal: {
          font_family: "",
          font_size: 14,
          scrollback: 1000,
          line_height: 1,
          letter_spacing: 0,
          cursor_blink: true,
          font_ligatures: false,
        },
      }),
    });
  });

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });

  await page.getByRole("button", { name: "Add repositories…" }).click();
  const dialog = page.getByRole("dialog", { name: "Add repositories" });
  await dialog.getByLabel("Repository pattern").fill("import-lab/*");
  await dialog.getByRole("button", { name: "Preview" }).click();

  await expect(dialog.getByText("import-lab/public-source")).toBeVisible();
  await expect(dialog.getByText("import-lab/private-source")).toBeVisible();
  await expect(dialog.getByText("import-lab/public-fork")).toBeVisible();

  await dialog.getByLabel("Hide private").check();
  await dialog.getByLabel("Hide forks").check();
  await expect(dialog.getByText("import-lab/public-source")).toBeVisible();
  await expect(dialog.getByText("import-lab/private-source")).toHaveCount(0);
  await expect(dialog.getByText("import-lab/public-fork")).toHaveCount(0);
  await expect(dialog.getByText("Selected 1 of 1")).toBeVisible();

  await dialog.getByRole("button", { name: "Add selected repositories" }).click();

  await expect
    .poll(() => bulkRepos)
    .toEqual([
      {
        provider: "github",
        host: "github.com",
        owner: "import-lab",
        name: "public-source",
        repo_path: "import-lab/public-source",
      },
    ]);
});

test("repository import previews and adds Forgejo and Gitea repositories through the API", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await page.getByRole("button", { name: "Add repositories…" }).click();
  const dialog = page.getByRole("dialog", { name: "Add repositories" });

  await dialog.getByRole("combobox", { name: /Provider/ }).click();
  await dialog.getByRole("option", { name: "Forgejo" }).click();
  await expect(dialog.getByLabel("Host")).toHaveValue("codeberg.org");
  await dialog.getByLabel("Repository pattern").fill("team/subgroup/service-*");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect(dialog.getByRole("alert")).toContainText("Format: owner/pattern");

  await dialog.getByLabel("Repository pattern").fill("forge-lab/*");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect(dialog.getByText("forge-lab/service")).toBeVisible();
  await expect(dialog.getByText("forge-lab/archived")).toHaveCount(0);
  await dialog.getByRole("button", { name: "Add selected repositories" }).click();
  await expect(page.getByRole("dialog", { name: "Add repositories" })).toHaveCount(0);
  await expect(page.locator(".repo-row", { hasText: "forge-lab/service" })).toBeVisible();

  if (!api) throw new Error("settings-globs API context not initialized");
  let settingsResponse = await api.get("/api/v1/settings");
  let settings = (await settingsResponse.json()) as {
    repos: Array<{
      provider: string;
      platform_host: string;
      owner: string;
      name: string;
      repo_path: string;
      is_glob: boolean;
    }>;
  };
  expect(settings.repos).toContainEqual(
    expect.objectContaining({
      provider: "forgejo",
      platform_host: "codeberg.org",
      owner: "forge-lab",
      name: "service",
      repo_path: "forge-lab/service",
      is_glob: false,
    }),
  );
  await expect
    .poll(async () => {
      const response = await api!.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter(
          (repo) => repo.Platform === "forgejo" && repo.PlatformHost === "codeberg.org" && repo.Owner === "forge-lab",
        )
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("service");

  await page.getByRole("button", { name: "Add repositories…" }).click();
  const giteaDialog = page.getByRole("dialog", {
    name: "Add repositories",
  });
  await giteaDialog.getByRole("combobox", { name: /Provider/ }).click();
  await giteaDialog.getByRole("option", { name: "Gitea" }).click();
  await expect(giteaDialog.getByLabel("Host")).toHaveValue("gitea.com");
  await giteaDialog.getByLabel("Repository pattern").fill("gitea-team/*");
  await giteaDialog.getByRole("button", { name: "Preview" }).click();
  await expect(giteaDialog.getByText("gitea-team/service")).toBeVisible();
  await expect(giteaDialog.getByText("gitea-team/private-service")).toBeVisible();
  await expect(giteaDialog.getByText("gitea-team/archived")).toHaveCount(0);

  await giteaDialog.getByLabel("Hide private").check();
  await expect(giteaDialog.getByText("gitea-team/service")).toBeVisible();
  await expect(giteaDialog.getByText("gitea-team/private-service")).toHaveCount(0);
  await giteaDialog.getByRole("button", { name: "Add selected repositories" }).click();

  await expect(page.getByRole("dialog", { name: "Add repositories" })).toHaveCount(0);
  await expect(page.locator(".repo-row", { hasText: "gitea-team/service" })).toBeVisible();

  settingsResponse = await api.get("/api/v1/settings");
  settings = (await settingsResponse.json()) as typeof settings;
  expect(settings.repos).toContainEqual(
    expect.objectContaining({
      provider: "gitea",
      platform_host: "gitea.com",
      owner: "gitea-team",
      name: "service",
      repo_path: "gitea-team/service",
      is_glob: false,
    }),
  );

  await expect
    .poll(async () => {
      const response = await api!.get("/api/v1/repos");
      const repos = (await response.json()) as RepoSummary[];
      return repos
        .filter((repo) => repo.Owner === "gitea-team")
        .map((repo) => repo.Name)
        .sort()
        .join(",");
    })
    .toBe("service");
});

test("repository import traps keyboard focus inside the dialog", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });

  await page.getByRole("button", { name: "Add repositories…" }).click();
  const dialog = page.getByRole("dialog", { name: "Add repositories" });
  await expect(dialog.getByLabel("Repository pattern")).toBeFocused();

  await dialog.getByRole("button", { name: "Close" }).focus();
  await page.keyboard.press("Shift+Tab");
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeFocused();

  await page.keyboard.press("Tab");
  await expect(dialog.getByRole("button", { name: "Close" })).toBeFocused();
});

test("repository import clears stale preview results after failed preview", async ({ page }) => {
  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });

  await page.getByRole("button", { name: "Add repositories…" }).click();
  const dialog = page.getByRole("dialog", { name: "Add repositories" });
  await dialog.getByLabel("Repository pattern").fill("roborev-dev/*");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect(dialog.getByText("roborev-dev/kenn-forge")).toBeVisible();

  await dialog.getByLabel("Repository pattern").fill("bad-owner/[invalid");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect(dialog.getByText(/invalid glob pattern|GitHub API error|glob syntax/)).toBeVisible();
  await expect(dialog.getByText("roborev-dev/kenn-forge")).toHaveCount(0);
});

test("repository import ignores older preview responses", async ({ page }) => {
  let firstPreviewRelease: (() => void) | undefined;
  let previewCalls = 0;
  await page.route("**/api/v1/repos/preview", async (route) => {
    previewCalls += 1;
    const request = route.request().postDataJSON() as {
      owner: string;
      pattern: string;
    };
    if (previewCalls === 1) {
      await new Promise<void>((resolve) => {
        firstPreviewRelease = resolve;
      });
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({
          owner: request.owner,
          pattern: request.pattern,
          repos: [
            {
              provider: "github",
              platform_host: "github.com",
              owner: "roborev-dev",
              name: "kenn-forge",
              repo_path: "roborev-dev/kenn-forge",
              description: "Main dashboard",
              private: false,
              fork: false,
              pushed_at: "2026-04-22T10:00:00Z",
              already_configured: false,
            },
          ],
        }),
      });
      return;
    }
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        owner: request.owner,
        pattern: request.pattern,
        repos: [
          {
            provider: "github",
            platform_host: "github.com",
            owner: "roborev-dev",
            name: "review-bot",
            repo_path: "roborev-dev/review-bot",
            description: "Review automation",
            private: false,
            fork: false,
            pushed_at: "2026-04-24T09:15:00Z",
            already_configured: false,
          },
        ],
      }),
    });
  });

  await page.goto(`${isolatedServer!.info.base_url}/settings`);
  await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  await page.getByRole("button", { name: "Add repositories…" }).click();
  const dialog = page.getByRole("dialog", { name: "Add repositories" });

  await dialog.getByLabel("Repository pattern").fill("roborev-dev/*");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect.poll(() => previewCalls).toBe(1);

  await dialog.getByLabel("Repository pattern").fill("roborev-dev/review-*");
  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect.poll(() => previewCalls).toBe(2);
  await expect(dialog.getByText("roborev-dev/review-bot")).toBeVisible();

  firstPreviewRelease?.();
  await expect(dialog.getByText("roborev-dev/review-bot")).toBeVisible();
  await expect(dialog.getByText("roborev-dev/kenn-forge")).toHaveCount(0);
});
