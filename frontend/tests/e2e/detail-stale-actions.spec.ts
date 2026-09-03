import { expect, test, type Page, type Route } from "@playwright/test";
import { mockApi, mockSettings as defaultSettings } from "./support/mockApi";

const prA = {
  ID: 11,
  RepoID: 1,
  GitHubID: 1101,
  Number: 100,
  URL: "https://github.com/acme/widgets/pull/100",
  Title: "PR A title",
  Author: "marius",
  State: "open",
  IsDraft: false,
  Body: "Body A",
  HeadBranch: "feature/a",
  BaseBranch: "main",
  Additions: 10,
  Deletions: 1,
  CommentCount: 0,
  ReviewDecision: "",
  CIStatus: "success",
  CIChecksJSON: "[]",
  CreatedAt: "2026-04-01T12:00:00Z",
  UpdatedAt: "2026-04-01T12:00:00Z",
  LastActivityAt: "2026-04-01T12:00:00Z",
  MergedAt: null,
  ClosedAt: null,
  KanbanStatus: "new",
  Starred: false,
  repo_owner: "acme",
  repo_name: "widgets",
  platform_host: "github.com",
  platform_head_sha: "",
  worktree_links: [],
  MergeableState: "",
};

const prB = {
  ...prA,
  ID: 22,
  GitHubID: 1102,
  Number: 200,
  URL: "https://github.com/acme/widgets/pull/200",
  Title: "PR B title",
  Body: "Body B",
  HeadBranch: "feature/b",
};

const prSquashOnly = {
  ...prA,
  ID: 23,
  RepoID: 2,
  GitHubID: 1103,
  Number: 300,
  URL: "https://github.com/acme/squash-only/pull/300",
  Title: "Squash-only PR title",
  Body: "Body C",
  HeadBranch: "feature/c",
  platform_head_sha: "fixture-head-sha-300",
  repo_name: "squash-only",
};

const providerCapabilities = {
  comment_mutation: true,
  issue_mutation: true,
  merge_mutation: true,
  read_ci: true,
  read_comments: true,
  read_issues: true,
  read_merge_requests: true,
  read_releases: true,
  read_repositories: true,
  ready_for_review: true,
  draft_mutation: true,
  review_mutation: true,
  state_mutation: true,
  workflow_approval: true,
};

function repoEnvelope(item: { repo_owner: string; repo_name: string; platform_host: string }) {
  return {
    provider: "github",
    platform_host: item.platform_host,
    owner: item.repo_owner,
    name: item.repo_name,
    repo_path: `${item.repo_owner}/${item.repo_name}`,
    capabilities: providerCapabilities,
  };
}

function pullDetailApiPath(pr: typeof prA): string {
  return `**/api/v1/pulls/github/${pr.repo_owner}/${pr.repo_name}/${pr.Number}`;
}

function issueDetailApiPath(issue: { repo_owner: string; repo_name: string; Number: number }): string {
  return `**/api/v1/issues/github/${issue.repo_owner}/${issue.repo_name}/${issue.Number}`;
}

function repoSettingsApiPath(pr: typeof prA): string {
  return `**/api/v1/repo/github/${pr.repo_owner}/${pr.repo_name}`;
}

const issueX = {
  ID: 31,
  RepoID: 1,
  GitHubID: 1201,
  Number: 300,
  URL: "https://github.com/acme/widgets/issues/300",
  Title: "Issue X title",
  Author: "marius",
  State: "open",
  Body: "Body X",
  CommentCount: 0,
  LabelsJSON: "[]",
  CreatedAt: "2026-04-01T12:00:00Z",
  UpdatedAt: "2026-04-01T12:00:00Z",
  LastActivityAt: "2026-04-01T12:00:00Z",
  ClosedAt: null,
  Starred: false,
  platform_host: "github.com",
  repo_owner: "acme",
  repo_name: "widgets",
};

const issueY = {
  ...issueX,
  ID: 32,
  GitHubID: 1202,
  Number: 400,
  URL: "https://github.com/acme/widgets/issues/400",
  Title: "Issue Y title",
  Body: "Body Y",
};

function detailEnvelopePR(pr: typeof prA & { workspace?: { id: string } }): unknown {
  return {
    merge_request: pr,
    repo: repoEnvelope(pr),
    repo_owner: pr.repo_owner,
    repo_name: pr.repo_name,
    platform_head_sha: pr.platform_head_sha ?? "",
    reviewed_head_sha: pr.platform_head_sha ?? "",
    detail_loaded: true,
    detail_fetched_at: "2026-04-01T12:00:00Z",
    worktree_links: pr.worktree_links,
    workspace: pr.workspace,
  };
}

function detailEnvelopeIssue(issue: typeof issueX & { workspace?: { id: string } }): unknown {
  return {
    issue,
    events: [],
    repo: repoEnvelope(issue),
    platform_host: issue.platform_host,
    repo_owner: issue.repo_owner,
    repo_name: issue.repo_name,
    detail_loaded: true,
    detail_fetched_at: "2026-04-01T12:00:00Z",
    workspace: issue.workspace,
  };
}

// Endpoints triggered ONLY by user mutation actions (not the
// detail store's automatic /sync POST or list refreshes).
const USER_MUTATION_PATTERNS = [
  /\/pulls\/\d+\/github-state$/,
  /\/pulls\/\d+\/approve$/,
  /\/pulls\/\d+\/approve-workflows$/,
  /\/pulls\/\d+\/ready-for-review$/,
  /\/pulls\/\d+\/comments$/,
  /\/pulls\/\d+\/merge$/,
  /\/pulls\/\d+\/title$/,
  /\/pulls\/\d+\/body$/,
  /\/pulls\/\d+\/star$/,
  /\/pulls\/\d+\/kanban-status$/,
  /\/issues\/\d+\/github-state$/,
  /\/issues\/\d+\/comments$/,
  /\/issues\/\d+\/star$/,
  /\/api\/v1\/workspaces$/,
];

function recordUserMutations(page: Page): string[] {
  const seen: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "GET") return;
    const path = new URL(request.url()).pathname;
    if (USER_MUTATION_PATTERNS.some((p) => p.test(path))) {
      seen.push(`${request.method()} ${path}`);
    }
  });
  return seen;
}

async function mockSettings(page: Page): Promise<void> {
  await page.route("**/api/v1/settings", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ...defaultSettings,
        terminal: {
          ...defaultSettings.terminal,
          font_size: 14,
        },
      }),
    });
  });

  await page.route("**/api/v1/repo/github/**", async (route) => {
    if (route.request().method() !== "GET") {
      await route.fallback();
      return;
    }
    const parts = new URL(route.request().url()).pathname.split("/");
    const owner = parts.at(-2) ?? "acme";
    const name = parts.at(-1) ?? "widgets";
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ID: name === "squash-only" ? prSquashOnly.RepoID : prA.RepoID,
        Owner: owner,
        Name: name,
        AllowSquashMerge: true,
        AllowMergeCommit: true,
        AllowRebaseMerge: true,
        ViewerCanMerge: true,
        LastSyncStartedAt: "2026-04-01T12:00:00Z",
        LastSyncCompletedAt: "2026-04-01T12:00:30Z",
        LastSyncError: "",
        CreatedAt: "2026-03-01T00:00:00Z",
      }),
    });
  });
}

async function setupHeldPR(page: Page, fast: typeof prA, slow: typeof prB): Promise<{ release: () => void }> {
  await mockApi(page);
  await mockSettings(page);
  let release: () => void = () => {};
  const slowDelay = new Promise<void>((resolve) => {
    release = resolve;
  });

  // Fast PR: instant detail GET.
  await page.route(pullDetailApiPath(fast), async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(detailEnvelopePR(fast)),
      });
      return;
    }
    await route.fallback();
  });

  // Slow PR: held until release().
  await page.route(pullDetailApiPath(slow), async (route: Route) => {
    if (route.request().method() === "GET") {
      await slowDelay;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(detailEnvelopePR(slow)),
      });
      return;
    }
    await route.fallback();
  });

  return { release };
}

async function setupHeldIssue(page: Page, fast: typeof issueX, slow: typeof issueY): Promise<{ release: () => void }> {
  await mockApi(page);
  await mockSettings(page);
  let release: () => void = () => {};
  const slowDelay = new Promise<void>((resolve) => {
    release = resolve;
  });

  await page.route(issueDetailApiPath(fast), async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(detailEnvelopeIssue(fast)),
      });
      return;
    }
    await route.fallback();
  });

  await page.route(issueDetailApiPath(slow), async (route: Route) => {
    if (route.request().method() === "GET") {
      await slowDelay;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(detailEnvelopeIssue(slow)),
      });
      return;
    }
    await route.fallback();
  });

  return { release };
}

async function mockPullDetail(page: Page, pr: typeof prA): Promise<void> {
  await page.route(pullDetailApiPath(pr), async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(detailEnvelopePR(pr)),
      });
      return;
    }
    await route.fallback();
  });
}

async function gotoPullDetail(page: Page, pr: typeof prA): Promise<void> {
  await page.goto(`/pulls/github/${pr.repo_owner}/${pr.repo_name}/${pr.Number}`);
}

test.describe("PR detail merge modal route reset", () => {
  test("merge button is disabled when the PR has merge conflicts", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    const conflictedPR = {
      ...prA,
      MergeableState: "dirty",
    };

    await mockPullDetail(page, conflictedPR);

    await gotoPullDetail(page, conflictedPR);

    await expect(page.locator(".detail-title")).toContainText(conflictedPR.Title);
    await expect(page.getByTestId("merge-warnings-chip")).toContainText("Conflicts");

    const mergeButton = page.locator(".btn--merge").first();
    await expect(mergeButton).toBeDisabled();
    await mergeButton.click({ force: true });
    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
  });

  test("merge modal closes when the route changes and does not reopen for the new PR", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    for (const pr of [prA, prB]) {
      await page.route(pullDetailApiPath(pr), async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(detailEnvelopePR(pr)),
          });
          return;
        }
        await route.fallback();
      });
    }

    await page.goto(`/pulls/github/${prA.repo_owner}/${prA.repo_name}/${prA.Number}`);
    await expect(page.locator(".detail-title")).toContainText(prA.Title);

    // Open the merge modal for PR A.
    await page.locator(".btn--merge").first().click();
    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toBeVisible();

    // Navigate to PR B. The action-local reset must close the
    // modal as soon as the props change.
    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [prB.repo_owner, prB.repo_name, prB.Number] as const,
    );

    await expect(page.locator(".detail-title")).toContainText(prB.Title);
    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
  });

  test("hides merge actions when repo permissions disallow merging", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    await page.route(pullDetailApiPath(prA), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(detailEnvelopePR(prA)),
        });
        return;
      }
      await route.fallback();
    });
    await page.route(repoSettingsApiPath(prA), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            ID: prA.RepoID,
            Owner: prA.repo_owner,
            Name: prA.repo_name,
            AllowSquashMerge: true,
            AllowMergeCommit: true,
            AllowRebaseMerge: true,
            ViewerCanMerge: false,
            LastSyncStartedAt: "2026-04-01T12:00:00Z",
            LastSyncCompletedAt: "2026-04-01T12:00:30Z",
            LastSyncError: "",
            CreatedAt: "2026-03-01T00:00:00Z",
          }),
        });
        return;
      }
      await route.fallback();
    });

    await page.goto(`/pulls/github/${prA.repo_owner}/${prA.repo_name}/${prA.Number}`);

    await expect(page.locator(".detail-title")).toContainText(prA.Title);
    await expect(page.locator(".btn--merge")).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
  });

  test("hides merge actions when every repository merge method is disabled", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);
    await mockPullDetail(page, prA);

    await page.route(repoSettingsApiPath(prA), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            ID: prA.RepoID,
            Owner: prA.repo_owner,
            Name: prA.repo_name,
            AllowSquashMerge: false,
            AllowMergeCommit: false,
            AllowRebaseMerge: false,
            ViewerCanMerge: true,
            LastSyncStartedAt: "2026-04-01T12:00:00Z",
            LastSyncCompletedAt: "2026-04-01T12:00:30Z",
            LastSyncError: "",
            CreatedAt: "2026-03-01T00:00:00Z",
          }),
        });
        return;
      }
      await route.fallback();
    });

    await gotoPullDetail(page, prA);

    await expect(page.locator(".detail-title")).toContainText(prA.Title);
    await expect(page.locator(".btn--merge")).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toHaveCount(0);
  });

  test("merge actions wait for settings that match the selected repo", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    for (const pr of [prA, prSquashOnly]) {
      await page.route(pullDetailApiPath(pr), async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(detailEnvelopePR(pr)),
          });
          return;
        }
        await route.fallback();
      });
    }

    let releaseSquashSettings!: () => void;
    const squashSettingsReady = new Promise<void>((resolve) => {
      releaseSquashSettings = resolve;
    });

    await page.route(repoSettingsApiPath(prSquashOnly), async (route) => {
      if (route.request().method() === "GET") {
        await squashSettingsReady;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            ID: prSquashOnly.RepoID,
            Owner: prSquashOnly.repo_owner,
            Name: prSquashOnly.repo_name,
            AllowSquashMerge: true,
            AllowMergeCommit: false,
            AllowRebaseMerge: false,
            ViewerCanMerge: true,
            LastSyncStartedAt: "2026-04-01T12:00:00Z",
            LastSyncCompletedAt: "2026-04-01T12:00:30Z",
            LastSyncError: "",
            CreatedAt: "2026-03-01T00:00:00Z",
          }),
        });
        return;
      }
      await route.fallback();
    });

    await page.goto(`/pulls/github/${prA.repo_owner}/${prA.repo_name}/${prA.Number}`);
    await expect(page.locator(".detail-title")).toContainText(prA.Title);
    await expect(page.locator(".btn--merge")).toBeVisible();

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [prSquashOnly.repo_owner, prSquashOnly.repo_name, prSquashOnly.Number] as const,
    );

    await expect(page.locator(".detail-title")).toContainText(prSquashOnly.Title);
    await expect(page.locator(".btn--merge")).toHaveCount(0);

    releaseSquashSettings();

    const mergeButton = page.locator(".btn--merge").first();
    await expect(mergeButton).toBeVisible();
    await mergeButton.click();

    await expect(page.getByRole("dialog", { name: "Merge Pull Request" })).toBeVisible();
    await expect(page.locator(".method-option")).toHaveCount(0);
    await expect(
      page.locator(".kit-modal-footer").getByRole("button", {
        name: "Squash and merge",
      }),
    ).toBeVisible();
    await expect(page.getByText("Create a merge commit")).toHaveCount(0);
    await expect(page.getByText("Rebase and merge")).toHaveCount(0);

    // The merge must pin the head the detail view rendered so the server
    // can reject the action when the head moved after review.
    const mergeRequest = page.waitForRequest(
      (req) => req.method() === "POST" && /\/merge$/.test(new URL(req.url()).pathname),
    );
    await page.locator(".kit-modal-footer").getByRole("button", { name: "Squash and merge" }).click();
    const request = await mergeRequest;
    const body = request.postDataJSON() as { expected_head_sha?: string };
    expect(body.expected_head_sha).toBe("fixture-head-sha-300");
  });
});

test.describe("detail load-error banner", () => {
  test("PR: failing route shows banner over the previous PR", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    await page.route(pullDetailApiPath(prA), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(detailEnvelopePR(prA)),
        });
        return;
      }
      await route.fallback();
    });
    await page.route(pullDetailApiPath(prB), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 500,
          contentType: "application/problem+json",
          body: JSON.stringify({
            type: "about:blank",
            title: "Internal Server Error",
            status: 500,
            detail: "upstream offline",
            code: "internalError",
          }),
        });
        return;
      }
      await route.fallback();
    });

    await page.goto(`/pulls/github/${prA.repo_owner}/${prA.repo_name}/${prA.Number}`);
    await expect(page.locator(".detail-title")).toContainText(prA.Title);

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [prB.repo_owner, prB.repo_name, prB.Number] as const,
    );

    const banner = page.getByTestId("detail-load-error");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("upstream offline");
    await expect(page.locator(".detail-title")).toContainText(prA.Title);
  });

  test("issue: failing route shows banner over the previous issue", async ({ page }) => {
    await mockApi(page);
    await mockSettings(page);

    await page.route(issueDetailApiPath(issueX), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(detailEnvelopeIssue(issueX)),
        });
        return;
      }
      await route.fallback();
    });
    await page.route(issueDetailApiPath(issueY), async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 500,
          contentType: "application/problem+json",
          body: JSON.stringify({
            type: "about:blank",
            title: "Internal Server Error",
            status: 500,
            detail: "upstream offline",
            code: "internalError",
          }),
        });
        return;
      }
      await route.fallback();
    });

    await page.goto(`/issues/github/${issueX.repo_owner}/${issueX.repo_name}/${issueX.Number}`);
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueX.Title);

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/issues/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [issueY.repo_owner, issueY.repo_name, issueY.Number] as const,
    );

    const banner = page.getByTestId("detail-load-error");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("upstream offline");
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueX.Title);
  });
});

test.describe("PR detail stale-action gating", () => {
  test("conflict warning hides while a clean PR is loading", async ({ page }) => {
    const conflictedPR = {
      ...prA,
      MergeableState: "dirty",
    };
    const cleanPR = {
      ...prB,
      MergeableState: "clean",
    };
    const { release } = await setupHeldPR(page, conflictedPR, cleanPR);

    await page.goto(`/pulls/github/${conflictedPR.repo_owner}/${conflictedPR.repo_name}/${conflictedPR.Number}`);
    await expect(page.locator(".detail-title")).toContainText(conflictedPR.Title);
    await expect(page.getByTestId("merge-warnings-chip")).toContainText("Conflicts");

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [cleanPR.repo_owner, cleanPR.repo_name, cleanPR.Number] as const,
    );

    await expect(page.locator(".detail-title")).toContainText(conflictedPR.Title);
    await expect(page.getByTestId("merge-warnings-chip")).toHaveCount(0);

    release();
    await expect(page.locator(".detail-title")).toContainText(cleanPR.Title);
    await expect(page.getByTestId("merge-warnings-chip")).toHaveCount(0);
  });

  test("close, comment, and create-workspace are inert while the new PR is loading", async ({ page }) => {
    const userMutations = recordUserMutations(page);
    const { release } = await setupHeldPR(page, prA, prB);

    await page.goto(`/pulls/github/${prA.repo_owner}/${prA.repo_name}/${prA.Number}`);
    await expect(page.locator(".detail-title")).toContainText(prA.Title);

    // Trigger an in-place navigation to the slow PR via popstate.
    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [prB.repo_owner, prB.repo_name, prB.Number] as const,
    );

    // PR A is still on screen because B is held.
    await expect(page.locator(".detail-title")).toContainText(prA.Title);

    const starBtn = page.locator(".pull-detail button.star-btn");
    await expect(starBtn).toHaveCount(0);

    // Close button must be hidden — clicking stale controls must not fire
    // POST /github-state for PR B.
    const closeBtn = page.locator(".btn--close").first();
    await expect(closeBtn).toHaveCount(0);

    // Workspace creation is withheld until the newly routed detail has loaded.
    const createWs = page.getByRole("button", { name: "Create Workspace", exact: true });
    const createWsOptions = page.getByRole("button", { name: "Create Workspace options" });
    await expect(createWs).toHaveCount(0);
    await expect(createWsOptions).toHaveCount(0);
    await expect(page.getByText("Refresh details before creating a workspace.")).toBeVisible();

    // Comment submit: disabled.
    const commentSubmit = page.locator(".comment-box .submit-btn").first();
    await expect(commentSubmit).toBeDisabled();

    // Release the slow load. PR B now displays and the controls
    // re-enable.
    release();
    await expect(page.locator(".detail-title")).toContainText(prB.Title);
    await expect(page.locator(".btn--close").first()).toBeEnabled();

    // No user-mutation request was sent during the stale window.
    expect(userMutations).toEqual([]);
  });

  test("the stale PR workspace is hidden while the new PR is loading", async ({ page }) => {
    const prWithWorkspace = {
      ...prA,
      workspace: { id: "ws-pr-a" },
    };
    const { release } = await setupHeldPR(page, prWithWorkspace, prB);

    await page.goto(
      `/pulls/github/${prWithWorkspace.repo_owner}/${prWithWorkspace.repo_name}/${prWithWorkspace.Number}`,
    );
    await expect(page.locator(".detail-title")).toContainText(prWithWorkspace.Title);

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/pulls/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [prB.repo_owner, prB.repo_name, prB.Number] as const,
    );

    await expect(page.locator(".detail-title")).toContainText(prWithWorkspace.Title);

    // Workspace resolution follows the newly routed identity, not the stale
    // detail envelope that is still painted. Never expose A's workspace or
    // offer B's create controls until B has loaded.
    await expect(page.getByRole("button", { name: "Open Workspace", exact: true })).toHaveCount(0);
    const createWs = page.getByRole("button", { name: "Create Workspace", exact: true });
    const createWsOptions = page.getByRole("button", { name: "Create Workspace options" });
    await expect(createWs).toHaveCount(0);
    await expect(createWsOptions).toHaveCount(0);
    await expect(page.getByText("Refresh details before creating a workspace.")).toBeVisible();
    await expect(page).not.toHaveURL(/\/terminal\/ws-pr-a/);

    release();
    await expect(page.locator(".detail-title")).toContainText(prB.Title);
  });
});

test.describe("issue detail stale-action gating", () => {
  test("star, close, comment, and create-workspace are inert while the new issue is loading", async ({ page }) => {
    const userMutations = recordUserMutations(page);
    const { release } = await setupHeldIssue(page, issueX, issueY);

    await page.goto(`/issues/github/${issueX.repo_owner}/${issueX.repo_name}/${issueX.Number}`);
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueX.Title);

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/issues/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [issueY.repo_owner, issueY.repo_name, issueY.Number] as const,
    );

    // Issue X is still on screen because Y is held.
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueX.Title);

    const starBtn = page.locator(".issue-detail .star-btn");
    await expect(starBtn).toHaveCount(0);

    const closeBtn = page.locator(".issue-detail .btn--close");
    await expect(closeBtn).toBeDisabled();
    await closeBtn.click({ force: true }).catch(() => {});

    const createWs = page.locator(".issue-detail").getByRole("button", { name: "Create Workspace", exact: true });
    const createWsOptions = page.locator(".issue-detail").getByRole("button", { name: "Create Workspace options" });
    await expect(createWs).toBeDisabled();
    await expect(createWsOptions).toBeDisabled();
    await createWs.click({ force: true }).catch(() => {});
    await createWsOptions.click({ force: true }).catch(() => {});

    const commentSubmit = page.locator(".issue-detail .comment-box .submit-btn").first();
    await expect(commentSubmit).toBeDisabled();

    release();
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueY.Title);
    await expect(closeBtn).toBeEnabled();

    expect(userMutations).toEqual([]);
  });

  test("the stale issue workspace is hidden while the new issue is loading", async ({ page }) => {
    const issueWithWorkspace = {
      ...issueX,
      workspace: { id: "ws-issue-x" },
    };
    const { release } = await setupHeldIssue(page, issueWithWorkspace, issueY);

    await page.goto(
      `/issues/github/${issueWithWorkspace.repo_owner}/${issueWithWorkspace.repo_name}/${issueWithWorkspace.Number}`,
    );
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueWithWorkspace.Title);

    await page.evaluate(
      ([owner, name, number]) => {
        window.history.pushState(null, "", `/issues/github/${owner}/${name}/${number}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
      [issueY.repo_owner, issueY.repo_name, issueY.Number] as const,
    );

    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueWithWorkspace.Title);

    // Workspace resolution follows the newly routed identity, not the stale
    // detail envelope that is still painted. Never expose X's workspace while
    // Y is loading; offer only Y's disabled create controls.
    await expect(
      page.locator(".issue-detail").getByRole("button", { name: "Open Workspace", exact: true }),
    ).toHaveCount(0);
    const createWs = page.locator(".issue-detail").getByRole("button", { name: "Create Workspace", exact: true });
    const createWsOptions = page.locator(".issue-detail").getByRole("button", { name: "Create Workspace options" });
    await expect(createWs).toBeDisabled();
    await expect(createWsOptions).toBeDisabled();
    await createWs.click({ force: true }).catch(() => {});
    await createWsOptions.click({ force: true }).catch(() => {});
    await expect(page).not.toHaveURL(/\/terminal\/ws-issue-x/);

    release();
    await expect(page.locator(".issue-detail .detail-title")).toContainText(issueY.Title);
  });
});
