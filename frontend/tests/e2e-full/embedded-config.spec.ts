import { execFileSync } from "node:child_process";
import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import type {
  ListProjectsOutputBody,
  ListWorktreesOutputBody,
  ProjectResponse as GeneratedProjectResponse,
  RegisterProjectInputBody,
  RegisterWorktreeInputBody,
} from "../../src/lib/api/generated/models/index.js";

import { startIsolatedE2EServerWithOptions } from "./support/e2eServer";

type ProjectResponse = GeneratedProjectResponse;
type ProjectListResponse = ListProjectsOutputBody;
type WorktreeListResponse = ListWorktreesOutputBody;
type RegisterProjectInput = RegisterProjectInputBody;
type RegisterWorktreeInput = RegisterWorktreeInputBody;

type SnapshotResponse = {
  hosts: Array<{
    configKey: string;
    kind: string;
    name: string;
  }>;
};

async function waitForPRList(page: Page): Promise<void> {
  await page.locator(".pull-item").first().waitFor({ state: "visible", timeout: 10_000 });
}

async function sidebarWidth(page: Page): Promise<number> {
  return Math.round(
    await page
      .locator(".kit-sidebar-layout__sidebar")
      .first()
      .evaluate((node) => node.getBoundingClientRect().width),
  );
}

test.describe("embedded config", () => {
  test("hides sync button when hideSync is true", async ({ page }) => {
    await page.addInitScript(() => {
      window.__kenn_forge_config = { ui: { hideSync: true } };
    });
    await page.goto("/pulls");
    await waitForPRList(page);

    await expect(page.locator(".action-btn", { hasText: "Sync" })).not.toBeVisible();
  });

  test("hides repo selector when hideRepoSelector is true", async ({ page }) => {
    await page.addInitScript(() => {
      window.__kenn_forge_config = { ui: { hideRepoSelector: true } };
    });
    await page.goto("/pulls");
    await waitForPRList(page);

    await expect(page.locator(".typeahead")).not.toBeAttached();
  });

  test("hides star button when hideStar is true", async ({ page }) => {
    await page.addInitScript(() => {
      window.__kenn_forge_config = { ui: { hideStar: true } };
    });
    await page.goto("/pulls");
    await waitForPRList(page);

    // Open a PR detail.
    await page.locator(".pull-item").first().click();
    await page.locator(".pull-detail").waitFor({ state: "visible", timeout: 10_000 });

    await expect(page.locator(".pull-detail .star-btn")).not.toBeAttached();
  });

  test("hides theme toggle when theme.mode is set", async ({ page }) => {
    await page.addInitScript(() => {
      window.__kenn_forge_config = { theme: { mode: "dark" } };
    });
    await page.goto("/pulls");
    await waitForPRList(page);

    await expect(page.locator("button[title='Toggle theme']")).not.toBeAttached();
  });

  test("host sidebarWidth overrides persisted width on pulls", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-sidebar-width", "520");
      window.__kenn_forge_config = { embed: { sidebarWidth: 410 } };
    });
    await page.goto("/pulls");
    await waitForPRList(page);

    await expect.poll(async () => sidebarWidth(page)).toBe(410);

    await page.reload();
    await waitForPRList(page);

    await expect.poll(async () => sidebarWidth(page)).toBe(410);
  });

  test("settings page is blocked in embedded mode", async ({ page }) => {
    await page.addInitScript(() => {
      window.__kenn_forge_config = { embed: {} };
    });
    await page.goto("/settings");

    // When embedded, /settings is not a valid route and falls
    // through to the activity page. The URL may still say /settings
    // but the activity feed should render instead.
    await page.locator(".activity-feed").waitFor({ state: "visible", timeout: 10_000 });
    await expect(page.locator(".settings-page")).not.toBeAttached();
  });

  test("daemon ui-only config does not block standalone settings", async ({ page }) => {
    // The daemon serves window.__kenn_forge_config carrying only its
    // UI focus state (ui.activeWorktreeKey, set via the API). That
    // must not flip the SPA into embedded mode and hide the settings
    // page, which a standalone client needs.
    await page.addInitScript(() => {
      window.__kenn_forge_config = { ui: { activeWorktreeKey: "wt-1" } };
    });
    await page.goto("/settings");

    await page.locator(".settings-page").waitFor({ state: "visible", timeout: 10_000 });
  });

  test("project intake uses snapshot host metadata and host-scoped registration", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    const hostKey = server.info.node_id;
    const localRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "kenn-forge-hosted-intake-")));
    try {
      execFileSync("git", ["init", "-b", "main"], { cwd: localRepo, stdio: "ignore" });
      execFileSync("git", ["config", "user.email", "e2e@example.com"], {
        cwd: localRepo,
        stdio: "ignore",
      });
      execFileSync("git", ["config", "user.name", "E2E Fixture"], {
        cwd: localRepo,
        stdio: "ignore",
      });
      execFileSync("git", ["commit", "--allow-empty", "-m", "fixture: seed project"], {
        cwd: localRepo,
        stdio: "ignore",
      });

      const snapshotResponse = await page.request.get(`${server.info.base_url}/api/v1/snapshot?include_peers=true`);
      expect(snapshotResponse.status(), await snapshotResponse.text()).toBe(200);
      const snapshot = (await snapshotResponse.json()) as SnapshotResponse;
      const hubHost = snapshot.hosts.find((host) => host.configKey === hostKey);
      expect(hubHost).toBeDefined();
      expect(hubHost?.kind).toBe("self");

      const snapshotLoaded = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return url.pathname === "/api/v1/snapshot" && url.searchParams.get("include_peers") === "true";
      });
      await page.goto(`${server.info.base_url}/project-intake?host=${encodeURIComponent(hostKey)}`);
      await snapshotLoaded;
      await expect(page.getByText(`Host: ${hubHost?.name ?? "hub"}`)).toBeVisible();

      await page.getByRole("button", { name: /Add an existing repository/ }).click();
      await page.getByLabel("Repository path").fill(localRepo);

      const registerFinished = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return response.request().method() === "POST" && url.pathname === `/api/v1/fleet/hosts/${hostKey}/projects`;
      });
      await page.getByRole("button", { name: "Add repository" }).click();
      const registerResponse = await registerFinished;
      expect(registerResponse.status(), await registerResponse.text()).toBe(201);
      const created = (await registerResponse.json()) as ProjectResponse;
      expect(created.id).not.toBe("");

      await expect(page).toHaveURL(/\/workspaces$/);
      const listResponse = await page.request.get(`${server.info.base_url}/api/v1/projects`);
      expect(listResponse.status(), await listResponse.text()).toBe(200);
      const list = (await listResponse.json()) as ProjectListResponse;
      expect(list.projects).toContainEqual(
        expect.objectContaining({
          id: created.id,
          local_path: localRepo,
        }),
      );
    } finally {
      rmSync(localRepo, { recursive: true, force: true });
      await server.stop();
    }
  });

  test("accepted project registration survives host replacement without a duplicate", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    const hostKey = server.info.node_id;
    const localRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "kenn-forge-retained-intake-")));
    try {
      execFileSync("git", ["init", "-b", "main"], { cwd: localRepo, stdio: "ignore" });
      execFileSync("git", ["config", "user.email", "e2e@example.com"], {
        cwd: localRepo,
        stdio: "ignore",
      });
      execFileSync("git", ["config", "user.name", "E2E Fixture"], {
        cwd: localRepo,
        stdio: "ignore",
      });
      execFileSync("git", ["commit", "--allow-empty", "-m", "fixture: seed project"], {
        cwd: localRepo,
        stdio: "ignore",
      });

      let registrationRequests = 0;
      let noteCommitted: () => void = () => undefined;
      let releaseResponse: () => void = () => undefined;
      const committed = new Promise<void>((resolve) => {
        noteCommitted = resolve;
      });
      const responseGate = new Promise<void>((resolve) => {
        releaseResponse = resolve;
      });
      await page.route(`**/api/v1/fleet/hosts/${hostKey}/projects`, async (route) => {
        if (route.request().method() !== "POST") {
          await route.fallback();
          return;
        }
        registrationRequests += 1;
        const response = await route.fetch();
        noteCommitted();
        await responseGate;
        await route.fulfill({ response });
      });

      await page.goto(`${server.info.base_url}/project-intake?host=${encodeURIComponent(hostKey)}`);
      await page.getByRole("button", { name: /Add an existing repository/ }).click();
      await page.getByLabel("Repository path").fill(localRepo);
      await page.getByRole("button", { name: "Add repository" }).click();
      await committed;

      await page.evaluate(() => {
        history.pushState({}, "", "/project-intake");
        window.dispatchEvent(new PopStateEvent("popstate"));
      });
      await expect(page.getByText("Add an existing local repository")).toBeVisible();
      releaseResponse();
      await expect(page).toHaveURL(/\/project-intake$/);

      await page.evaluate((key) => {
        history.pushState({}, "", `/project-intake?host=${encodeURIComponent(key)}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, hostKey);
      await page.getByRole("button", { name: /Add an existing repository/ }).click();
      await page.getByLabel("Repository path").fill(localRepo);
      await page.getByRole("button", { name: "Add repository" }).click();

      await expect(page).toHaveURL(/\/workspaces$/);
      expect(registrationRequests).toBe(1);
      const listResponse = await page.request.get(`${server.info.base_url}/api/v1/projects`);
      expect(listResponse.status(), await listResponse.text()).toBe(200);
      const list = (await listResponse.json()) as ProjectListResponse;
      expect((list.projects ?? []).filter((project) => project.local_path === localRepo)).toHaveLength(1);
    } finally {
      rmSync(localRepo, { recursive: true, force: true });
      await server.stop();
    }
  });

  test("embed project card preserves host key in project actions", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    const hostKey = server.info.node_id;
    const localRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "kenn-forge-hosted-card-")));
    try {
      execFileSync("git", ["init"], { cwd: localRepo, stdio: "ignore" });
      const registerResponse = await page.request.post(`${server.info.base_url}/api/v1/projects`, {
        data: {
          local_path: localRepo,
          display_name: "Fleet Project",
          default_branch: "main",
        },
      });
      expect(registerResponse.status(), await registerResponse.text()).toBe(201);
      const project = (await registerResponse.json()) as ProjectResponse;
      expect(project.id).not.toBe("");

      await page.addInitScript(() => {
        const win = window as unknown as {
          __kenn_forge_config?: ForgeConfig;
          __kenn_forge_project_action_context?: unknown;
        };
        win.__kenn_forge_config = {
          actions: {
            project: [
              {
                id: "new-worktree",
                label: "New Worktree",
                handler: (context) => {
                  win.__kenn_forge_project_action_context = context;
                  return { ok: true };
                },
              },
            ],
          },
        };
      });

      await page.goto(
        `${server.info.base_url}/workspaces/embed/project/${encodeURIComponent(project.id)}?host=${encodeURIComponent(hostKey)}`,
      );
      await expect(page.locator("header.app-top-bar")).toHaveCount(0);
      await expect(page.getByText("Fleet Project")).toBeVisible();

      await page
        .getByRole("button", {
          name: /Create (your first|another) worktree/i,
        })
        .click();

      await expect
        .poll(() =>
          page.evaluate(() => {
            const win = window as unknown as {
              __kenn_forge_project_action_context?: unknown;
            };
            return win.__kenn_forge_project_action_context;
          }),
        )
        .toEqual({
          surface: "project-card",
          projectId: project.id,
          hostKey,
        });
    } finally {
      rmSync(localRepo, { recursive: true, force: true });
      await server.stop();
    }
  });

  test("committed worktree creation reconciles after navigation before accepting another intent", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    const firstRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "kenn-forge-retained-worktree-first-")));
    const secondRepo = realpathSync(mkdtempSync(path.join(os.tmpdir(), "kenn-forge-retained-worktree-second-")));
    const worktreePaths = [
      path.join(os.tmpdir(), `kenn-forge-retained-worktree-${process.pid}-one`),
      path.join(os.tmpdir(), `kenn-forge-retained-worktree-${process.pid}-two`),
    ];
    try {
      for (const repo of [firstRepo, secondRepo]) {
        execFileSync("git", ["init", "-b", "main"], { cwd: repo, stdio: "ignore" });
      }

      const firstProjectInput = {
        local_path: firstRepo,
        display_name: "Retained Worktree Project",
        default_branch: "main",
      } satisfies RegisterProjectInput;
      const firstProjectResponse = await page.request.post(`${server.info.base_url}/api/v1/projects`, {
        data: firstProjectInput,
      });
      expect(firstProjectResponse.status(), await firstProjectResponse.text()).toBe(201);
      const firstProject: ProjectResponse = await firstProjectResponse.json();

      const secondProjectInput = {
        local_path: secondRepo,
        display_name: "Navigation Target Project",
        default_branch: "main",
      } satisfies RegisterProjectInput;
      const secondProjectResponse = await page.request.post(`${server.info.base_url}/api/v1/projects`, {
        data: secondProjectInput,
      });
      expect(secondProjectResponse.status(), await secondProjectResponse.text()).toBe(201);
      const secondProject: ProjectResponse = await secondProjectResponse.json();

      await page.addInitScript(
        ({ paths }) => {
          window.__kenn_forge_config = {
            actions: {
              project: [
                {
                  id: "new-worktree",
                  label: "New Worktree",
                  handler: async (context) => {
                    const callNumber = Number.parseInt(localStorage.getItem("worktree-action-calls") ?? "0", 10) + 1;
                    localStorage.setItem("worktree-action-calls", String(callNumber));
                    const projectId = context.projectId;
                    const worktreePath = paths[callNumber - 1];
                    if (!projectId || !worktreePath) return { ok: false, message: "Missing worktree input." };
                    const branch = callNumber === 1 ? "feature-retained" : "feature-second-intent";
                    const body = { branch, path: worktreePath } satisfies RegisterWorktreeInput;
                    const response = await fetch(`/api/v1/projects/${encodeURIComponent(projectId)}/worktrees`, {
                      method: "POST",
                      headers: { "Content-Type": "application/json" },
                      body: JSON.stringify(body),
                    });
                    if (!response.ok) return { ok: false, message: await response.text() };
                    localStorage.setItem("worktree-action-committed", String(callNumber));
                    await new Promise<void>((resolve) => {
                      const release = (event: MessageEvent<unknown>) => {
                        if (event.data !== `release-retained-worktree-${callNumber}`) return;
                        window.removeEventListener("message", release);
                        resolve();
                      };
                      window.addEventListener("message", release);
                    });
                    localStorage.setItem("worktree-action-acknowledged", String(callNumber));
                    return { ok: true };
                  },
                },
              ],
            },
          };
        },
        { paths: worktreePaths },
      );

      const firstRoute = `/workspaces/embed/project/${encodeURIComponent(firstProject.id)}`;
      const secondRoute = `/workspaces/embed/project/${encodeURIComponent(secondProject.id)}`;
      await page.goto(`${server.info.base_url}${firstRoute}`);
      await expect(page.getByText("Retained Worktree Project")).toBeVisible();
      await page.getByRole("button", { name: /Create (your first|another) worktree/i }).click();
      await expect.poll(() => page.evaluate(() => localStorage.getItem("worktree-action-committed"))).toBe("1");

      await page.evaluate((route) => {
        history.pushState({}, "", route);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, secondRoute);
      await expect(page.getByText("Navigation Target Project")).toBeVisible();

      let holdProjectRefreshes = true;
      let firstProjectReadCount = 0;
      let deliveredRefreshes = 0;
      const releaseRefreshes: Array<() => void> = [];
      await page.route(`**/api/v1/projects/${encodeURIComponent(firstProject.id)}`, async (route) => {
        firstProjectReadCount += 1;
        if (!holdProjectRefreshes || firstProjectReadCount === 1) {
          await route.continue();
          return;
        }
        const response = await route.fetch();
        await new Promise<void>((resolve) => {
          releaseRefreshes.push(resolve);
        });
        await route.fulfill({ response });
        deliveredRefreshes += 1;
      });

      await page.evaluate((route) => {
        history.pushState({}, "", route);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, firstRoute);
      await expect(page.getByText("Retained Worktree Project")).toBeVisible();
      await expect(page.getByRole("button", { name: /Create (your first|another) worktree/i })).toBeDisabled();

      await page.evaluate(() => window.postMessage("release-retained-worktree-1", "*"));
      await expect.poll(() => releaseRefreshes.length).toBe(2);
      releaseRefreshes[1]?.();
      await expect.poll(() => deliveredRefreshes).toBe(1);
      await expect(page.getByRole("button", { name: "Create another worktree" })).toBeEnabled();
      expect(await page.evaluate(() => localStorage.getItem("worktree-action-calls"))).toBe("1");

      const retainedListResponse = await page.request.get(
        `${server.info.base_url}/api/v1/projects/${encodeURIComponent(firstProject.id)}/worktrees`,
      );
      expect(retainedListResponse.status(), await retainedListResponse.text()).toBe(200);
      const retainedList: WorktreeListResponse = await retainedListResponse.json();
      expect((retainedList.worktrees ?? []).filter((worktree) => !worktree.is_primary)).toEqual([
        expect.objectContaining({ branch: "feature-retained" }),
      ]);

      await page.getByRole("button", { name: "Create another worktree" }).click();
      await expect.poll(() => page.evaluate(() => localStorage.getItem("worktree-action-committed"))).toBe("2");

      releaseRefreshes[0]?.();
      await expect.poll(() => deliveredRefreshes).toBe(2);
      holdProjectRefreshes = false;
      await page.evaluate((route) => {
        history.pushState({}, "", route);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, secondRoute);
      await expect(page.getByText("Navigation Target Project")).toBeVisible();
      await page.evaluate((route) => {
        history.pushState({}, "", route);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, firstRoute);
      await expect(page.getByRole("button", { name: "Create another worktree" })).toBeDisabled();
      expect(await page.evaluate(() => localStorage.getItem("worktree-action-calls"))).toBe("2");

      await page.evaluate(() => window.postMessage("release-retained-worktree-2", "*"));
      await expect.poll(() => page.evaluate(() => localStorage.getItem("worktree-action-acknowledged"))).toBe("2");
      await expect(page.getByText("feature-second-intent")).toBeVisible();

      const finalListResponse = await page.request.get(
        `${server.info.base_url}/api/v1/projects/${encodeURIComponent(firstProject.id)}/worktrees`,
      );
      expect(finalListResponse.status(), await finalListResponse.text()).toBe(200);
      const finalList: WorktreeListResponse = await finalListResponse.json();
      expect(
        (finalList.worktrees ?? [])
          .filter((worktree) => !worktree.is_primary)
          .map((worktree) => worktree.branch)
          .sort(),
      ).toEqual(["feature-retained", "feature-second-intent"]);
    } finally {
      rmSync(firstRepo, { recursive: true, force: true });
      rmSync(secondRepo, { recursive: true, force: true });
      await server.stop();
    }
  });
});
