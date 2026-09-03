import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { page } from "vite-plus/test/browser";

import {
  mountBrowserApp,
  pressKey,
  resetKeyboardModuleState,
  type MountedBrowserApp,
} from "./test/browserAppHarness.js";
import { jsonResponse, mockSettings, type MockRouteOverride } from "./test/mockApiFetch.js";

const WAIT = 10_000;

const repo = {
  provider: "github",
  platform_host: "github.com",
  owner: "acme",
  name: "widgets",
  repo_path: "acme/widgets",
  capabilities: {},
};

const activityItems = [
  {
    id: "a1",
    cursor: "a1",
    activity_type: "comment",
    author: "user-a",
    body_preview: "",
    created_at: "2026-03-30T14:00:00Z",
    item_number: 42,
    item_state: "open",
    item_title: "PR 42 title",
    item_type: "pr",
    item_url: "https://github.com/acme/widgets/pull/42",
    repo,
  },
  {
    id: "b1",
    cursor: "b1",
    activity_type: "comment",
    author: "user-a",
    body_preview: "",
    created_at: "2026-03-30T13:00:00Z",
    item_number: 55,
    item_state: "open",
    item_title: "Issue 55 title",
    item_type: "issue",
    item_url: "https://github.com/acme/widgets/issues/55",
    repo,
  },
  {
    id: "c1",
    cursor: "c1",
    activity_type: "default_branch_commit",
    author: "user-a",
    body_preview: "Bump dependency",
    created_at: "2026-03-30T12:00:00Z",
    item_number: 0,
    item_state: "",
    item_title: "",
    item_type: "pr",
    item_url: "https://github.com/acme/widgets/commit/abcdef1234567890abcdef1234567890abcdef12",
    branch_name: "main",
    commit_sha: "abcdef1234567890abcdef1234567890abcdef12",
    repo,
  },
];

function activityOverrides(): MockRouteOverride[] {
  return [
    (req) => {
      if (req.method !== "GET" || req.url.pathname !== "/api/v1/settings") return null;
      return jsonResponse({
        ...mockSettings,
        activity: {
          ...mockSettings.activity,
          view_mode: "flat",
          collapse_threads: false,
        },
      });
    },
    (req) => {
      if (req.method !== "GET" || req.url.pathname !== "/api/v1/activity") return null;
      return jsonResponse({ capped: false, items: activityItems });
    },
  ];
}

function activityRow(text: string): Element {
  return Array.from(document.querySelectorAll(".activity-row")).find((row) => (row.textContent ?? "").includes(text))!;
}

async function openSelection(text: string): Promise<string> {
  await vi.waitFor(() => expect(document.querySelector(".activity-row")).not.toBeNull(), WAIT);
  const row = activityRow(text);
  expect(row).not.toBeUndefined();
  await page.elementLocator(row).click();
  await vi.waitFor(() => expect(document.querySelector(".activity-detail")).not.toBeNull(), WAIT);
  return window.location.pathname + window.location.search;
}

async function leaveAndRestore(destination: "/pulls" | "/issues"): Promise<void> {
  const { navigate } = await import("./lib/stores/router.svelte.js");
  navigate(destination);
  await vi.waitFor(() => expect(document.querySelector(".activity-detail")).toBeNull(), WAIT);
  window.history.back();
  await vi.waitFor(() => expect(document.querySelector(".activity-detail")).not.toBeNull(), WAIT);
}

describe("Activity detail restoration after browser Back", () => {
  vi.setConfig({ testTimeout: 30_000 });

  let mounted: MountedBrowserApp | null = null;

  beforeEach(async () => {
    await page.viewport(1280, 900);
  });

  afterEach(async () => {
    mounted?.unmount();
    mounted = null;
    vi.restoreAllMocks();
    localStorage.clear();
    sessionStorage.clear();
    await resetKeyboardModuleState();
  });

  it("restores the commit detail pane after leaving Activity and pressing Back", async () => {
    mounted = await mountBrowserApp("/", { overrides: activityOverrides() });
    const activityUrl = await openSelection("Bump dependency");

    await leaveAndRestore("/pulls");

    expect(document.querySelector(".commit-diff-panel")).not.toBeNull();
    expect(document.querySelector(".activity-detail-header")?.textContent).toContain("acme/widgets");
    expect(document.querySelector(".activity-detail-header")?.textContent).toContain("main");
    expect(document.querySelector(".activity-detail-header")?.textContent).toContain("abcdef123456");
    const selected = new URL(activityUrl, window.location.origin).searchParams;
    expect(selected.get("selected")).toBe("commit:abcdef1234567890abcdef1234567890abcdef12");
    expect(selected.get("provider")).toBe("github");
    expect(selected.get("platform_host")).toBe("github.com");
    expect(selected.get("repo_path")).toBe("acme/widgets");
    expect(selected.get("branch")).toBe("main");
  });

  it("restores a PR selection after leaving Activity and pressing Back", async () => {
    mounted = await mountBrowserApp("/", { overrides: activityOverrides() });
    await openSelection("PR 42 title");

    await leaveAndRestore("/pulls");

    expect(document.querySelector(".activity-detail")).not.toBeNull();
    expect(document.querySelector(".activity-detail-header")?.textContent).toContain("acme/widgets#42");
  });

  it("restores an issue selection after leaving Activity and pressing Back", async () => {
    mounted = await mountBrowserApp("/", { overrides: activityOverrides() });
    await openSelection("Issue 55 title");

    await leaveAndRestore("/issues");

    expect(document.querySelector(".activity-detail")).not.toBeNull();
    expect(document.querySelector(".activity-detail-header")?.textContent).toContain("acme/widgets#55");
  });

  it("Escape closes a restored commit pane", async () => {
    mounted = await mountBrowserApp("/", { overrides: activityOverrides() });
    await openSelection("Bump dependency");
    await leaveAndRestore("/pulls");

    pressKey("Escape");
    await vi.waitFor(() => expect(document.querySelector(".activity-detail")).toBeNull(), WAIT);
    expect(new URL(window.location.href).searchParams.has("selected")).toBe(false);
  });

  it("replaces a commit selection when an item row is selected", async () => {
    mounted = await mountBrowserApp("/", { overrides: activityOverrides() });
    await openSelection("Bump dependency");

    await page.getByText("PR 42 title").click();
    await vi.waitFor(
      () => expect(document.querySelector(".activity-detail-header")?.textContent).toContain("acme/widgets#42"),
      WAIT,
    );
    expect(new URL(window.location.href).searchParams.get("selected")).toBe("pr:42");
    expect(document.querySelector(".commit-diff-panel")).toBeNull();
  });
});
