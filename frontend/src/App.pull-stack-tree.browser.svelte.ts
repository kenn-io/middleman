import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { page } from "vite-plus/test/browser";
import { mountBrowserApp, resetKeyboardModuleState, type MountedBrowserApp } from "./test/browserAppHarness.js";
import { createMockApiFetch, jsonResponse, type MockRouteOverride } from "./test/mockApiFetch.js";
import type { PullRequest } from "./lib/api/types.js";

let mounted: MountedBrowserApp | null = null;
let routes: MockRouteOverride;

beforeEach(async () => {
  localStorage.clear();
  await page.viewport(1280, 900);
  const api = createMockApiFetch();
  const response = await api.fetch("/api/v1/pulls");
  const defaults: PullRequest[] = await response.json();
  const base = defaults[0]!;
  const titles = [
    "Extract request validation",
    "Add validation to the API",
    "Show validation errors",
    "Update release notes",
  ];
  const members = titles.map((Title, index) => ({
    ...base,
    ID: 800 + index,
    Number: index + 1,
    Title,
    Author: "user-a",
    AuthorDisplayName: "User A",
    stack: index < 3 ? { stack_id: 71, position: index + 1, size: 3 } : undefined,
  }));
  routes = (req) => {
    if (req.method !== "GET") return null;
    if (req.url.pathname === "/api/v1/pulls") return jsonResponse([members[2], members[3], members[1], members[0]]);
    const number = Number(req.url.pathname.match(/\/pulls\/github\/[^/]+\/[^/]+\/(\d+)$/)?.[1]);
    const pr = members.find((member) => member.Number === number);
    if (!pr) return null;
    return jsonResponse({
      merge_request: pr,
      events: [],
      repo: pr.repo,
      repo_owner: pr.repo_owner,
      repo_name: pr.repo_name,
      platform_host: pr.platform_host,
      worktree_links: [],
      detail_loaded: true,
    });
  };
});

afterEach(async () => {
  mounted?.unmount();
  mounted = null;
  localStorage.clear();
  await resetKeyboardModuleState();
});

function rowTitles(): string[] {
  return [...document.querySelectorAll(".pull-list .list-body .pull-item .title-text")].map(
    (element) => element.textContent?.trim() ?? "",
  );
}

describe("PR sidebar stack tree", () => {
  it("toggles from the filter menu, expands without navigation, and remembers the view", async () => {
    mounted = await mountBrowserApp("/pulls", { overrides: [routes] });
    await vi.waitFor(() => expect(rowTitles()).toHaveLength(4));
    await page.elementLocator(document.querySelector(".pull-list .compact-filter-menu button")!).click();
    await page.getByRole("button", { name: "Stack tree", exact: true }).click();
    await vi.waitFor(() => expect(rowTitles()).toEqual(["Extract request validation", "Update release notes"]));
    const path = window.location.pathname;
    await page.getByRole("button", { name: "Expand stack at #1: 3 PRs in stack" }).click();
    await vi.waitFor(() =>
      expect(rowTitles()).toEqual([
        "Extract request validation",
        "Add validation to the API",
        "Show validation errors",
        "Update release notes",
      ]),
    );
    expect(window.location.pathname).toBe(path);
    const sidebar = document.querySelector<HTMLElement>(".pull-list .list-body")!;
    expect(sidebar.scrollWidth).toBeLessThanOrEqual(sidebar.clientWidth);
    await page.getByRole("button", { name: "Collapse stack at #1: 3 PRs in stack" }).click();
    await vi.waitFor(() => expect(rowTitles()).toHaveLength(2));
    mounted.unmount();
    mounted = await mountBrowserApp("/pulls", { overrides: [routes] });
    await expect.element(page.getByRole("button", { name: "Expand stack at #1: 3 PRs in stack" })).toBeVisible();
  });

  it("reveals a deep-linked member and lets the user collapse it", async () => {
    localStorage.setItem("kenn-forge:pullStackTree", "1");
    mounted = await mountBrowserApp("/pulls/github/acme/widgets/2", { overrides: [routes] });
    await expect.element(page.getByRole("button", { name: "Collapse stack at #1: 3 PRs in stack" })).toBeVisible();
    await vi.waitFor(() =>
      expect(document.querySelector(".pull-list .list-body .pull-item.selected .title-text")?.textContent).toBe(
        "Add validation to the API",
      ),
    );
    await page.getByRole("button", { name: "Collapse stack at #1: 3 PRs in stack" }).click();
    await vi.waitFor(() => expect(rowTitles()).toEqual(["Extract request validation", "Update release notes"]));
  });
});
