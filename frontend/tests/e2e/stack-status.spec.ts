import { expect, test, type Page, type Route } from "@playwright/test";
import { mockSettings as defaultSettings } from "./support/mockApi";

const capabilities = {
  comment_mutation: true,
  issue_mutation: true,
  merge_mutation: true,
  read_ci: true,
  read_comments: true,
  read_issues: true,
  read_labels: false,
  read_merge_requests: true,
  read_releases: true,
  read_repositories: true,
  ready_for_review: true,
  draft_mutation: true,
  review_mutation: true,
  state_mutation: true,
  workflow_approval: true,
};

function repoRef() {
  return {
    provider: "github",
    platform_host: "github.com",
    owner: "acme",
    name: "widgets",
    repo_path: "acme/widgets",
    capabilities,
  };
}

const checks = [
  {
    name: "frontend / vp check",
    status: "completed",
    conclusion: "failure",
    app: "GitHub Actions",
    url: "https://example.test/frontend",
  },
  {
    name: "e2e / chromium",
    status: "in_progress",
    conclusion: "",
    app: "GitHub Actions",
    url: "https://example.test/e2e",
  },
];

const pr = {
  ID: 102,
  RepoID: 1,
  GitHubID: 100102,
  PlatformID: 1,
  PlatformExternalID: "PR_kwDO_102",
  Number: 102,
  URL: "https://github.com/acme/widgets/pull/102",
  Title: "Add workspace session recovery",
  Author: "marius",
  AuthorDisplayName: "Marius",
  State: "open",
  IsDraft: false,
  IsLocked: false,
  Body: "Adds recovery behavior for interrupted workspace sessions.",
  HeadBranch: "feat/session-storage",
  BaseBranch: "main",
  HeadRepoCloneURL: "https://github.com/acme/widgets.git",
  Additions: 342,
  Deletions: 91,
  CommentCount: 3,
  ReviewDecision: "APPROVED",
  CIStatus: "pending",
  CIChecksJSON: JSON.stringify(checks),
  CIHadPending: true,
  CreatedAt: "2026-05-24T15:00:00Z",
  UpdatedAt: "2026-05-24T17:00:00Z",
  LastActivityAt: "2026-05-24T17:00:00Z",
  MergedAt: null,
  ClosedAt: null,
  MergeableState: "blocked",
  DetailFetchedAt: "2026-05-24T17:00:00Z",
  KanbanStatus: "reviewing",
  Starred: false,
  LabelsJSON: "[]",
  labels: [],
  repo_owner: "acme",
  repo_name: "widgets",
  platform_host: "github.com",
  repo: repoRef(),
  worktree_links: [],
};

function prForNumber(number: number, members = stackMembers) {
  const member = members.find((candidate) => candidate.number === number);
  return {
    ...pr,
    ID: number,
    GitHubID: 100000 + number,
    PlatformExternalID: `PR_kwDO_${number}`,
    Number: number,
    URL: `https://github.com/acme/widgets/pull/${number}`,
    Title: member ? member.title : pr.Title,
    HeadBranch:
      member?.base_branch === "main"
        ? "feat/base-schema"
        : (member?.base_branch.replace("feat/", "feat/child-") ?? pr.HeadBranch),
    CIStatus: member?.ci_status ?? pr.CIStatus,
    ReviewDecision: member?.review_decision ?? pr.ReviewDecision,
    MergeableState: member?.mergeable_state ?? pr.MergeableState,
  };
}

const stackMembers = [
  {
    number: 101,
    title: "base schema",
    state: "open",
    ci_status: "failure",
    review_decision: "APPROVED",
    mergeable_state: "",
    position: 1,
    is_draft: false,
    base_branch: "main",
    blocked_by: null,
  },
  {
    number: 102,
    title: "session storage",
    state: "open",
    ci_status: "pending",
    review_decision: "APPROVED",
    mergeable_state: "",
    position: 2,
    is_draft: false,
    base_branch: "feat/base-schema",
    blocked_by: 101,
  },
  {
    number: 103,
    title: "auth flow",
    state: "open",
    ci_status: "success",
    review_decision: "APPROVED",
    mergeable_state: "",
    position: 3,
    is_draft: false,
    base_branch: "feat/session-storage",
    blocked_by: 101,
  },
  {
    number: 104,
    title: "cache API",
    state: "open",
    ci_status: "success",
    review_decision: "APPROVED",
    mergeable_state: "",
    position: 4,
    is_draft: false,
    base_branch: "feat/auth-flow",
    blocked_by: 101,
  },
  {
    number: 105,
    title: "workspace logs",
    state: "open",
    ci_status: "success",
    review_decision: "APPROVED",
    mergeable_state: "",
    position: 5,
    is_draft: false,
    base_branch: "feat/cache-api",
    blocked_by: 101,
  },
  {
    number: 106,
    title: "agent retries",
    state: "open",
    ci_status: "success",
    review_decision: "",
    mergeable_state: "",
    position: 6,
    is_draft: false,
    base_branch: "feat/logs",
    blocked_by: 101,
  },
  {
    number: 107,
    title: "UI polish",
    state: "open",
    ci_status: "pending",
    review_decision: "",
    mergeable_state: "",
    position: 7,
    is_draft: false,
    base_branch: "feat/agent-retries",
    blocked_by: 101,
  },
];

type StackMember = (typeof stackMembers)[number];

async function fulfillJson(route: Route, body: unknown, status = 200): Promise<void> {
  await route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

async function mockStackedPR(
  page: Page,
  options: {
    stackMembers?: () => StackMember[];
    stackResponseDelays?: Map<number, Promise<void>>;
  } = {},
): Promise<void> {
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    const { pathname } = url;
    const method = route.request().method();

    if (method === "GET" && pathname === "/api/v1/pulls") {
      const currentStackMembers = options.stackMembers?.() ?? stackMembers;
      const position = currentStackMembers.findIndex((member) => member.number === pr.Number) + 1;
      const stack =
        position > 0 && currentStackMembers.length > 1 ? { position, size: currentStackMembers.length } : undefined;
      await fulfillJson(route, [{ ...pr, stack }]);
      return;
    }

    const detailMatch = pathname.match(/^\/api\/v1\/pulls\/github\/acme\/widgets\/(\d+)$/);
    if (method === "GET" && detailMatch) {
      const number = Number(detailMatch[1]!);
      const currentStackMembers = options.stackMembers?.() ?? stackMembers;
      const detailPR = prForNumber(number, currentStackMembers);
      const member = currentStackMembers.find((candidate) => candidate.number === number);
      await fulfillJson(route, {
        merge_request: detailPR,
        repo: repoRef(),
        platform_host: "github.com",
        repo_owner: "acme",
        repo_name: "widgets",
        detail_loaded: true,
        detail_fetched_at: "2026-05-24T17:00:00Z",
        diff_head_sha: "head",
        merge_base_sha: "base",
        platform_base_sha: "base",
        platform_head_sha: "head",
        reviewed_head_sha: "head",
        events: [],
        warnings: [],
        worktree_links: [],
        workflow_approval: {
          checked: true,
          required: false,
          count: 0,
          runs: [],
        },
        stack: member
          ? {
              stack_id: 1,
              stack_name: "session-recovery",
              position: member.position,
              size: currentStackMembers.length,
              health: "blocked",
              members: currentStackMembers,
            }
          : undefined,
      });
      return;
    }

    const stackMatch = pathname.match(/^\/api\/v1\/pulls\/github\/acme\/widgets\/(\d+)\/stack$/);
    if (method === "GET" && stackMatch) {
      const number = Number(stackMatch[1]!);
      const currentStackMembers = options.stackMembers?.() ?? stackMembers;
      const member = currentStackMembers.find((candidate) => candidate.number === number);
      await options.stackResponseDelays?.get(number);
      if (!member) {
        await fulfillJson(route, { error: "PR is not part of a stack" }, 404);
        return;
      }
      await fulfillJson(route, {
        stack_id: 1,
        stack_name: "session-recovery",
        position: member.position,
        size: currentStackMembers.length,
        health: "blocked",
        members: currentStackMembers,
      });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/repo/github/acme/widgets") {
      await fulfillJson(route, {
        AllowSquashMerge: true,
        AllowMergeCommit: true,
        AllowRebaseMerge: true,
        ViewerCanMerge: true,
      });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/snapshot") {
      await fulfillJson(route, { hosts: [], workspaces: [] });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/settings") {
      await fulfillJson(route, {
        ...defaultSettings,
        repos: [
          {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
            is_glob: false,
            matched_repo_count: 1,
          },
        ],
        activity: {
          ...defaultSettings.activity,
          view_mode: "threaded",
        },
        terminal: {
          ...defaultSettings.terminal,
          font_size: 14,
        },
      });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/sync/status") {
      await fulfillJson(route, {
        running: false,
        last_run_at: "2026-05-24T17:00:00Z",
        last_error: "",
      });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/rate-limits") {
      await fulfillJson(route, { provider_pools: {}, local_ceilings: {} });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/events") {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ":\n\n",
      });
      return;
    }

    if (method === "GET" && pathname === "/api/v1/activity") {
      await fulfillJson(route, []);
      return;
    }

    await fulfillJson(route, { error: `Unhandled ${method} ${pathname}` }, 404);
  });
}

async function emitPRDetailRefreshed(page: Page, number: number): Promise<void> {
  await page.evaluate(
    (ref) => {
      const eventSources = (
        window as unknown as {
          __kenn_forgeEventSources?: EventTarget[];
        }
      ).__kenn_forgeEventSources;
      eventSources?.[0]?.dispatchEvent(
        new MessageEvent("pr_detail_refreshed", {
          data: JSON.stringify(ref),
        }),
      );
    },
    {
      provider: "github",
      platform_host: "github.com",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      number,
      head_sha: "head",
      synced_at: "2026-05-24T17:01:00Z",
      warnings: [],
    },
  );
}

async function installMockEventSource(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const eventSources: EventTarget[] = [];

    class MockEventSource extends EventTarget {
      url: string;
      readyState = 0;

      constructor(url: string) {
        super();
        this.url = url;
        eventSources.push(this);
        setTimeout(() => {
          this.readyState = 1;
          this.dispatchEvent(new Event("open"));
        }, 0);
      }

      close(): void {
        this.readyState = 2;
      }
    }

    (
      window as unknown as {
        EventSource: typeof EventSource;
        __kenn_forgeEventSources: EventTarget[];
      }
    ).EventSource = MockEventSource as unknown as typeof EventSource;
    (
      window as unknown as {
        __kenn_forgeEventSources: EventTarget[];
      }
    ).__kenn_forgeEventSources = eventSources;
  });
}

test("unstacked pull detail does not request stack context", async ({ page }) => {
  let stackRequests = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.endsWith("/stack")) {
      stackRequests += 1;
    }
  });
  await mockStackedPR(page);

  await page.goto("/pulls/github/acme/widgets/108");

  await expect(page.getByTestId("ci-chip")).toBeVisible();
  await expect(page.getByTestId("stack-chip")).toHaveCount(0);
  expect(stackRequests).toBe(0);
});

test("stack status shares the PR detail expandable slot with CI", async ({ page }) => {
  await page.setViewportSize({ width: 892, height: 998 });
  await mockStackedPR(page);

  await page.goto("/pulls/github/acme/widgets/102");

  const listStackIndicator = page.locator(".pull-item").getByLabel("Stacked: 2/7");
  await expect(listStackIndicator).toHaveText("2/7");
  await expect(page.getByTestId("stack-chip")).toContainText("2/7");
  await expect(page.getByTestId("stack-chip")).not.toContainText("Stacked");

  await page.getByTestId("ci-chip").click();
  await expect(page.getByText("frontend / vp check")).toBeVisible();

  await page.getByTestId("stack-chip").click();

  await expect(page.getByText("frontend / vp check")).toBeHidden();
  await expect(page.getByText("7 PRs · current 2/7 · downstack CI failure")).toBeVisible();
  await expect(page.getByText("blocked by #101")).toBeVisible();

  const currentRow = page.locator(".stack-row--current");
  const currentBadgesBox = await currentRow.locator(".stack-badges").boundingBox();
  const currentLinkBox = await currentRow.locator(".stack-member-link").boundingBox();
  expect(currentBadgesBox).not.toBeNull();
  expect(currentLinkBox).not.toBeNull();
  const badgeCenterY = currentBadgesBox!.y + currentBadgesBox!.height / 2;
  const linkCenterY = currentLinkBox!.y + currentLinkBox!.height / 2;
  expect(Math.abs(badgeCenterY - linkCenterY)).toBeLessThanOrEqual(4);
  await expect(page.locator(".stack-member-meta")).toHaveCount(0);
  await expect(page.locator(".stack-base-name")).toHaveText("main");
  await expect(page.locator(".stack-row--base .stack-member-link")).toHaveCount(0);

  const stackRows = page.locator(".stack-member-link");
  await expect(stackRows).toHaveText([
    "#107 UI polish",
    "#106 agent retries",
    "#105 workspace logs",
    "#104 cache API",
    "#103 auth flow",
    "#102 session storage",
    "#101 base schema",
  ]);

  await page.getByRole("button", { name: "#101 base schema" }).click();

  await expect(page).toHaveURL(/\/pulls\/github\/acme\/widgets\/101$/);
  await expect(page.getByText("7 PRs · current 1/7")).toBeVisible();
  await expect(page.locator(".stack-base-name")).toHaveText("main");
});

test("stack status surfaces inherited downstack merge conflicts", async ({ page }) => {
  await mockStackedPR(page, {
    stackMembers: () => [
      {
        ...stackMembers[0]!,
        ci_status: "success",
        mergeable_state: "dirty",
      },
      {
        ...stackMembers[1]!,
        ci_status: "success",
        mergeable_state: "dirty",
      },
      ...stackMembers.slice(2),
    ],
  });

  await page.goto("/pulls/github/acme/widgets/102");

  await expect(page.getByTestId("merge-warnings-chip")).toContainText("Conflicts");
  await expect(
    page.getByRole("button", {
      name: /Stacked: 2\/7, 1 downstack merge conflict/i,
    }),
  ).toBeVisible();

  await page.getByTestId("stack-chip").click();
  await expect(page.getByText("7 PRs · current 2/7 · downstack conflict")).toBeVisible();
  await expect(page.getByText("× Conflicts")).toHaveCount(2);
});

test("stack status follows refreshed detail stack data", async ({ page }) => {
  await installMockEventSource(page);
  let currentStackMembers: StackMember[] = stackMembers;
  await mockStackedPR(page, {
    stackMembers: () => currentStackMembers,
  });

  await page.goto("/pulls/github/acme/widgets/102");
  await expect(
    page.getByRole("button", {
      name: /Stacked: 2\/7, 1 downstack CI failure/i,
    }),
  ).toBeVisible();

  currentStackMembers = [
    { ...stackMembers[0]!, ci_status: "success" },
    { ...stackMembers[1]!, ci_status: "success" },
  ];
  await emitPRDetailRefreshed(page, 102);

  // Scope to the chip: the sidebar row button also exposes "Stacked: n/m"
  // through its accessible name, so a page-wide role query would match it.
  await expect(page.getByTestId("stack-chip")).toHaveAccessibleName(/Stacked: 2\/2/i);

  currentStackMembers = [];
  await emitPRDetailRefreshed(page, 102);

  await expect(page.getByTestId("stack-chip")).toHaveCount(0);
});

test("phone pull rows fit the stack indicator beside the other indicators", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockStackedPR(page);

  await page.goto("/m/pulls");
  const row = page.locator(".mobile-shell .pull-item").first();
  const indicator = row.getByLabel("Stacked: 2/7");
  await expect(indicator).toHaveText("2/7");
  // The fixture also renders the approved review, CI cluster, and time
  // indicators, so the row exercises the full trailing cluster.
  await expect(row.locator(".review-indicator--approved")).toBeVisible();
  await expect(row.locator(".ci")).toBeVisible();

  const rowBox = await row.boundingBox();
  const indicatorBox = await indicator.boundingBox();
  expect(rowBox).not.toBeNull();
  expect(indicatorBox).not.toBeNull();
  if (rowBox && indicatorBox) {
    expect(rowBox.x + rowBox.width).toBeLessThanOrEqual(390);
    expect(indicatorBox.x).toBeGreaterThanOrEqual(rowBox.x);
    expect(indicatorBox.x + indicatorBox.width).toBeLessThanOrEqual(rowBox.x + rowBox.width);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});

test("stack status stays rendered while navigating to a stack member", async ({ page }) => {
  let releaseStackResponse: () => void = () => {};
  const delayedStackResponse = new Promise<void>((resolve) => {
    releaseStackResponse = resolve;
  });
  await mockStackedPR(page, {
    stackResponseDelays: new Map([[101, delayedStackResponse]]),
  });

  await page.goto("/pulls/github/acme/widgets/102");
  await page.getByTestId("stack-chip").click();
  await expect(page.getByText("7 PRs · current 2/7 · downstack CI failure")).toBeVisible();

  await page.getByRole("button", { name: "#101 base schema" }).click();

  await expect(page).toHaveURL(/\/pulls\/github\/acme\/widgets\/101$/);
  await expect(page.getByTestId("stack-chip")).toBeVisible();
  await expect(page.getByText("7 PRs · current 1/7")).toBeVisible();
  releaseStackResponse();
  await expect(page.getByText("7 PRs · current 1/7")).toBeVisible();
});

test("stack member navigation preserves focus routes", async ({ page }) => {
  await mockStackedPR(page);

  await page.goto("/focus/pulls/github/acme/widgets/102");
  await page.getByTestId("stack-chip").click();
  await page.getByRole("button", { name: "#101 base schema" }).click();

  await expect(page).toHaveURL(/\/focus\/pulls\/github\/acme\/widgets\/101$/);
  await expect(page.getByText("7 PRs · current 1/7")).toBeVisible();
});

test("stack member navigation updates the activity drawer selection", async ({ page }) => {
  await mockStackedPR(page);

  await page.goto("/?selected=pr:102&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets");
  await page.locator(".activity-detail").getByTestId("stack-chip").click();
  await page.getByRole("button", { name: "#101 base schema" }).click();

  await expect(page).toHaveURL(/selected=pr%3A101/);
  await expect(page).toHaveURL(/repo_path=acme%2Fwidgets/);
  await expect(page.locator(".activity-detail")).toContainText("acme/widgets#101");
  await expect(page.getByText("7 PRs · current 1/7")).toBeVisible();
});

test("stack rail spans wrapped CI badges at narrow widths", async ({ page }) => {
  await page.setViewportSize({ width: 319, height: 998 });
  await mockStackedPR(page);

  await page.goto("/pulls/github/acme/widgets/102");
  await page.getByTestId("stack-chip").click();

  const currentRow = page.locator(".stack-row--current");
  const currentRowBox = await currentRow.boundingBox();
  const currentDotBox = await currentRow.locator(".stack-dot--current").boundingBox();
  const currentLineBox = await currentRow.locator(".stack-line").boundingBox();
  const currentBadgesBox = await currentRow.locator(".stack-badges").boundingBox();
  expect(currentRowBox).not.toBeNull();
  expect(currentDotBox).not.toBeNull();
  expect(currentLineBox).not.toBeNull();
  expect(currentBadgesBox).not.toBeNull();
  const dotCenterY = currentDotBox!.y + currentDotBox!.height / 2;
  const rowCenterY = currentRowBox!.y + currentRowBox!.height / 2;
  expect(Math.abs(dotCenterY - rowCenterY)).toBeLessThanOrEqual(4);
  expect(currentLineBox!.y).toBeLessThanOrEqual(currentRowBox!.y + 1);
  expect(currentLineBox!.y + currentLineBox!.height).toBeGreaterThanOrEqual(
    currentBadgesBox!.y + currentBadgesBox!.height - 1,
  );
  const containerQueryEvidence = await page.evaluate(() => {
    function collectRules(ruleList: CSSRuleList): string[] {
      return Array.from(ruleList).flatMap((rule) => {
        const nested = "cssRules" in rule ? collectRules((rule as CSSGroupingRule).cssRules) : [];
        return [rule.cssText, ...nested];
      });
    }
    const rules = Array.from(document.styleSheets).flatMap((sheet) => {
      try {
        return collectRules(sheet.cssRules);
      } catch {
        return [];
      }
    });
    return {
      hasExpectedContainerRule: rules.some(
        (rule) =>
          rule.includes("@container pull-detail") && rule.includes("max-width: 440px") && rule.includes(".stack-row"),
      ),
      hasMalformedRule: rules.some((rule) => rule.includes("@frontend/src/lib/stores/container.svelte.ts")),
    };
  });
  expect(containerQueryEvidence).toEqual({
    hasExpectedContainerRule: true,
    hasMalformedRule: false,
  });
  const narrowStyles = await currentRow.evaluate((row) => {
    const badges = row.querySelector(".stack-badges");
    if (!badges) return null;
    const rowStyle = getComputedStyle(row);
    const badgeStyle = getComputedStyle(badges);
    return {
      rowGridColumns: rowStyle.gridTemplateColumns,
      badgesGridColumnStart: badgeStyle.gridColumnStart,
      badgesGridRowStart: badgeStyle.gridRowStart,
    };
  });
  expect(narrowStyles).not.toBeNull();
  expect(narrowStyles!.rowGridColumns.trim().split(/\s+/)).toHaveLength(2);
  expect(narrowStyles!.badgesGridColumnStart).toBe("2");
  expect(narrowStyles!.badgesGridRowStart).toBe("2");
  const railColors = await currentRow.evaluate((row) => {
    const line = row.querySelector(".stack-line");
    const panel = row.closest(".stack-panel");
    if (!line || !panel) return null;
    return {
      line: getComputedStyle(line).backgroundColor,
      panel: getComputedStyle(panel).backgroundColor,
    };
  });
  expect(railColors).not.toBeNull();
  expect(railColors!.line).not.toEqual(railColors!.panel);
});
