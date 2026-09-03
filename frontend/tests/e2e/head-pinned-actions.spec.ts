import { expect, test, type Page, type Route } from "@playwright/test";
import type { MergePRBody } from "../../src/lib/api/generated/models/index.js";
import { mockApi } from "./support/mockApi";

// Wire-level coverage for the head-pinning contract
// (context/provider-architecture.md "Head binding"): the detail view
// echoes the rendered reviewed_head_sha as expected_head_sha on merge,
// sends the latest synced provider head on approve when available, and
// branches on the 409 conflict reasons.

// The default acme/widgets#42 fixture uses the same SHA for platform_head_sha
// and reviewed_head_sha; the PR #77 cases below cover divergent heads.
const REVIEWED_SHA = "42aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa42";
const SYNCED_SHA = "0123456789abcdef0123456789abcdef01234567";
const MERGE_SUCCESS = {
  merged: true,
  message: "merged",
  sha: REVIEWED_SHA,
} satisfies MergePRBody;

const MERGE_PATH = "**/api/v1/pulls/github/acme/widgets/42/merge";
const APPROVE_PATH = "**/api/v1/pulls/github/acme/widgets/42/approve";

const STALE_PROMPT = "The head commit changed since this pull request was reviewed.";

function conflictProblem(
  reason: string,
  detail: string,
  context?: string,
): { status: number; contentType: string; body: string } {
  return {
    status: 409,
    contentType: "application/problem+json",
    body: JSON.stringify({
      type: "about:blank",
      title: "Conflict",
      status: 409,
      detail,
      code: "conflict",
      details: { reason, ...(context !== undefined && { context }) },
    }),
  };
}

const providerCapabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_labels: true,
  comment_mutation: true,
  state_mutation: true,
  merge_mutation: true,
  label_mutation: true,
  review_mutation: true,
  workflow_approval: true,
  ready_for_review: true,
  draft_mutation: true,
  issue_mutation: true,
  review_draft_mutation: false,
  review_thread_resolution: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: true,
  supported_review_actions: [],
};

// A PR whose reviewed head has never been synced, for the head_unknown
// flow. Local fixture so the shared mockApi pulls (which now all carry a
// reviewed head SHA) stay untouched.
const unboundPR = {
  ID: 9,
  RepoID: 1,
  GitHubID: 901,
  Number: 77,
  URL: "https://github.com/acme/widgets/pull/77",
  Title: "Unbound head PR",
  Author: "marius",
  State: "open",
  IsDraft: false,
  Body: "No head synced yet.",
  HeadBranch: "feature/unbound",
  BaseBranch: "main",
  Additions: 5,
  Deletions: 1,
  CommentCount: 0,
  ReviewDecision: "",
  CIStatus: "success",
  CIChecksJSON: "[]",
  MergeableState: "clean",
  CreatedAt: "2026-03-29T14:00:00Z",
  UpdatedAt: "2026-03-30T14:00:00Z",
  LastActivityAt: "2026-03-30T14:00:00Z",
  MergedAt: null,
  ClosedAt: null,
  KanbanStatus: "new",
  Starred: false,
  repo_owner: "acme",
  repo_name: "widgets",
  platform_host: "github.com",
  worktree_links: [],
};

function unboundDetailEnvelope(platformHeadSha: string, reviewedHeadSha = platformHeadSha): unknown {
  return {
    merge_request: unboundPR,
    repo: {
      provider: "github",
      platform_host: "github.com",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      capabilities: providerCapabilities,
    },
    repo_owner: "acme",
    repo_name: "widgets",
    platform_host: "github.com",
    platform_head_sha: platformHeadSha,
    reviewed_head_sha: reviewedHeadSha,
    detail_loaded: true,
    detail_fetched_at: "2026-03-30T14:00:00Z",
    worktree_links: [],
  };
}

async function mockPull77Detail(page: Page, platformHeadSha: string, reviewedHeadSha: string): Promise<void> {
  await page.route("**/api/v1/pulls/github/acme/widgets/77", async (route: Route) => {
    if (route.request().method() !== "GET") {
      await route.fallback();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(unboundDetailEnvelope(platformHeadSha, reviewedHeadSha)),
    });
  });

  await page.route("**/api/v1/pulls/github/acme/widgets/77/sync", async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(unboundDetailEnvelope(platformHeadSha, reviewedHeadSha)),
    });
  });

  await page.route("**/api/v1/pulls/github/acme/widgets/77/sync/async", async (route: Route) => {
    await route.fulfill({ status: 202, contentType: "application/json", body: "{}" });
  });
}

async function gotoPull42(page: Page): Promise<void> {
  await page.goto("/pulls/github/acme/widgets/42");
  await expect(page.locator(".detail-title")).toContainText("Add browser regression coverage");
}

async function openMergeModalAndConfirm(page: Page): Promise<void> {
  await page.locator(".btn--merge").first().click();
  const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
  await expect(modal).toBeVisible();
  const mergeResponse = page.waitForResponse((response) => {
    return response.request().method() === "POST" && /\/merge$/.test(new URL(response.url()).pathname);
  });
  await modal.getByRole("button", { name: "Squash and merge" }).click();
  await mergeResponse;
}

async function submitApproval(page: Page): Promise<void> {
  const approvalResponse = page.waitForResponse((response) => {
    return response.request().method() === "POST" && /\/approve$/.test(new URL(response.url()).pathname);
  });
  await page.locator(".btn--approve").first().click();
  const popover = page.locator(".approve-popover");
  await expect(popover).toBeVisible();
  await popover.getByRole("button", { name: "Approve", exact: true }).click();
  await approvalResponse;
}

test.describe("head-pinned merge and approve", () => {
  test.beforeEach(async ({ page }) => {
    await mockApi(page);
  });

  test("clicking outside the approval popover dismisses it", async ({ page }) => {
    await gotoPull42(page);
    await page.locator(".btn--approve").first().click();

    const popover = page.locator(".approve-popover");
    await expect(popover).toBeVisible();
    await page.locator(".detail-title").click();

    await expect(popover).toHaveCount(0);
  });

  test("merge echoes the rendered head as expected_head_sha", async ({ page }) => {
    let mergeBody: Record<string, unknown> | null = null;
    await page.route(MERGE_PATH, async (route: Route) => {
      mergeBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(MERGE_SUCCESS),
      });
    });

    await gotoPull42(page);
    await openMergeModalAndConfirm(page);

    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
    expect(mergeBody).not.toBeNull();
    expect(mergeBody!["expected_head_sha"]).toBe(REVIEWED_SHA);
  });

  test("approve sends the latest synced head as expected_head_sha", async ({ page }) => {
    let approveBody: Record<string, unknown> | null = null;
    await page.route(APPROVE_PATH, async (route: Route) => {
      approveBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "approved" }),
      });
    });

    await gotoPull42(page);
    await submitApproval(page);

    await expect(page.locator(".approve-popover")).toHaveCount(0);
    expect(approveBody).not.toBeNull();
    expect(approveBody!["expected_head_sha"]).toBe(REVIEWED_SHA);
  });

  test("merge stays on reviewed head while toolbar approve uses synced head", async ({ page }) => {
    const platformHead = SYNCED_SHA;
    const reviewedHead = REVIEWED_SHA;
    let mergeBody: Record<string, unknown> | null = null;
    let approveBody: Record<string, unknown> | null = null;

    await mockPull77Detail(page, platformHead, reviewedHead);
    await page.route("**/api/v1/pulls/github/acme/widgets/77/merge", async (route: Route) => {
      mergeBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(MERGE_SUCCESS),
      });
    });
    await page.route("**/api/v1/pulls/github/acme/widgets/77/approve", async (route: Route) => {
      approveBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "approved" }),
      });
    });

    await page.goto("/pulls/github/acme/widgets/77");
    await expect(page.locator(".detail-title")).toContainText("Unbound head PR");

    await openMergeModalAndConfirm(page);
    expect(mergeBody).not.toBeNull();
    expect(mergeBody!["expected_head_sha"]).toBe(reviewedHead);

    await submitApproval(page);
    expect(approveBody).not.toBeNull();
    expect(approveBody!["expected_head_sha"]).toBe(platformHead);
  });

  test("stale approval renders the provider side-effect context", async ({ page }) => {
    // A dismissal that failed leaves the approval standing on the
    // moved head; the banner must say so instead of presenting the
    // generic re-review prompt alone.
    const sideEffect = "approval 31 may stand on a moved head: dismissal failed";
    await page.route(APPROVE_PATH, async (route: Route) => {
      await route.fulfill(
        conflictProblem(
          "stale_state",
          `target changed since it was reviewed; refresh and retry; ${sideEffect}`,
          sideEffect,
        ),
      );
    });

    await gotoPull42(page);
    await submitApproval(page);

    await expect(page.getByText(STALE_PROMPT)).toBeVisible();
    await expect(page.getByText(sideEffect, { exact: false })).toBeVisible();
  });

  test("stale_state merge conflict closes the modal, refreshes, and prompts re-review", async ({ page }) => {
    await page.route(MERGE_PATH, async (route: Route) => {
      await route.fulfill(conflictProblem("stale_state", "target changed since it was reviewed; refresh and retry"));
    });

    await gotoPull42(page);

    // The conflict-path loadDetail runs with sync enabled, so a POST
    // to /sync (which background detail polling never issues) is the
    // unambiguous signal that the conflict refreshed the detail.
    const refresh = page.waitForRequest(
      (request) =>
        request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/pulls/github/acme/widgets/42/sync",
    );
    await openMergeModalAndConfirm(page);

    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
    await expect(page.getByText(STALE_PROMPT)).toBeVisible();
    await refresh;
    // The refreshed detail stays blocked until the user re-reviews the
    // moved head; rendering fresh data alone must not clear the conflict.
    await expect(page.locator(".btn--merge").first()).toBeDisabled();
    await expect(page.locator(".btn--merge").first()).toHaveAttribute(
      "title",
      "Refresh and re-review the pull request before merging",
    );
  });

  test("generic merge conflict keeps the modal open and shows the provider message", async ({ page }) => {
    await page.route(MERGE_PATH, async (route: Route) => {
      await route.fulfill(conflictProblem("conflict", "pull request is not mergeable"));
    });

    await gotoPull42(page);
    await openMergeModalAndConfirm(page);

    const modal = page.getByRole("dialog", { name: "Merge Pull Request" });
    await expect(modal).toBeVisible();
    await expect(modal.locator(".merge-error")).toHaveText("pull request is not mergeable");
    await expect(page.getByText(STALE_PROMPT)).toHaveCount(0);
  });
});

test.describe("palette approve head conflict", () => {
  test("uses the synced head when it differs from the reviewed head", async ({ page }) => {
    await mockApi(page);

    const platformHead = SYNCED_SHA;
    const reviewedHead = REVIEWED_SHA;
    await mockPull77Detail(page, platformHead, reviewedHead);
    await page.route("**/api/v1/pulls/github/acme/widgets/77/approve", async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "approved" }),
      });
    });

    const approveRequest = page.waitForRequest(
      (req) =>
        req.method() === "POST" && /\/pulls\/github\/acme\/widgets\/77\/approve$/.test(new URL(req.url()).pathname),
    );
    await page.goto("/pulls/github/acme/widgets/77");
    await expect(page.locator(".detail-title")).toContainText("Unbound head PR");
    await page.keyboard.press("Meta+K");
    await page.getByRole("textbox", { name: "Search command palette" }).fill("approve pr");
    await page.keyboard.press("Enter");

    const request = await approveRequest;
    const body = request.postDataJSON() as { expected_head_sha?: string };
    expect(body.expected_head_sha).toBe(platformHead);
  });

  test("stale_state from a palette approval reloads the detail before retry", async ({ page }) => {
    await mockApi(page);

    // The conflict-owned reload is a sync-enabled loadDetail, which is
    // the only caller of the synchronous POST .../sync endpoint —
    // background auto-refresh uses /sync/async — so counting /sync
    // POSTs cannot be satisfied by an unrelated background refresh.
    let syncPosts = 0;
    await page.route("**/api/v1/pulls/github/acme/widgets/42/sync", async (route: Route) => {
      if (route.request().method() !== "POST") {
        await route.fallback();
        return;
      }
      syncPosts += 1;
      await route.fallback();
    });
    await page.route(APPROVE_PATH, async (route: Route) => {
      await route.fulfill(conflictProblem("stale_state", "target changed since it was reviewed; refresh and retry"));
    });

    await page.goto("/pulls/github/acme/widgets/42");
    await expect(page.locator(".detail-title")).toContainText("Add browser regression coverage");
    // Route-driven loads use background sync mode and never hit this
    // endpoint, so the count stays zero until the conflict-owned reload.
    expect(syncPosts).toBe(0);

    const approveRequest = page.waitForRequest(
      (req) =>
        req.method() === "POST" && /\/pulls\/github\/acme\/widgets\/42\/approve$/.test(new URL(req.url()).pathname),
    );
    await page.keyboard.press("Meta+K");
    await page.getByRole("textbox", { name: "Search command palette" }).fill("approve pr");
    await page.keyboard.press("Enter");

    // The approval pin must be the latest synced provider head; in this
    // fixture it matches the rendered reviewed head. The conflict must
    // trigger a detail reload so the user re-reviews current data before retrying.
    const request = await approveRequest;
    const body = request.postDataJSON() as { expected_head_sha?: string };
    expect(body.expected_head_sha).toBe(REVIEWED_SHA);

    await expect.poll(() => syncPosts, { timeout: 5000 }).toBeGreaterThan(0);
  });
});

test.describe("head_unknown action gating", () => {
  test("approve can proceed without a reviewed head while merge waits for diff sync", async ({ page }) => {
    await mockApi(page);

    // Approval is safe without a locally verified reviewed_head_sha: the
    // client still sends the latest synced provider head when it has one,
    // and providers may approve the current head otherwise. Merge remains
    // blocked until reviewed_head_sha proves the rendered diff snapshot
    // is current.
    const platformHead = SYNCED_SHA;
    let reviewedHead = "";
    let approveRequested = false;
    let approveBody: Record<string, unknown> | null = null;

    await page.route("**/api/v1/pulls/github/acme/widgets/77", async (route: Route) => {
      if (route.request().method() !== "GET") {
        await route.fallback();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(unboundDetailEnvelope(platformHead, reviewedHead)),
      });
    });

    await page.route("**/api/v1/pulls/github/acme/widgets/77/sync", async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(unboundDetailEnvelope(platformHead, reviewedHead)),
      });
    });

    await page.route("**/api/v1/pulls/github/acme/widgets/77/sync/async", async (route: Route) => {
      await route.fulfill({ status: 202, contentType: "application/json", body: "{}" });
    });

    await page.route("**/api/v1/pulls/github/acme/widgets/77/approve", async (route: Route) => {
      approveRequested = true;
      approveBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "approved" }),
      });
    });

    await page.goto("/pulls/github/acme/widgets/77");
    await expect(page.locator(".detail-title")).toContainText("Unbound head PR");

    await expect(page.locator(".btn--approve").first()).toBeEnabled();
    await expect(page.locator(".btn--merge").first()).toBeDisabled();
    await submitApproval(page);
    expect(approveRequested).toBe(true);
    expect(approveBody).not.toBeNull();
    expect(approveBody!["expected_head_sha"]).toBe(platformHead);

    // A diff sync verifies the reviewed head; the reloaded detail
    // re-enables merge, while approval remains available.
    reviewedHead = SYNCED_SHA;
    await page.reload();
    await expect(page.locator(".detail-title")).toContainText("Unbound head PR");
    await expect(page.locator(".btn--approve").first()).toBeEnabled();
    await expect(page.locator(".btn--merge").first()).toBeEnabled();
  });
});
