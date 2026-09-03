// Shared mock of the kenn-forge REST API: one set of fixtures and one
// /api/v1 route matcher consumed by two thin adapters so the suites cannot
// drift apart:
//
//   - createMockApiFetch: fetch adapter for jsdom component tests (tests
//     stub globalThis.fetch with it);
//   - createMockApiHandler: transport-neutral core, also wrapped by the
//     Playwright page.route adapter in tests/e2e/support/mockApi.ts.
//
// This module must stay free of @playwright/test imports. Unhandled paths
// get a JSON 404 so optional endpoints fail softly in both suites.

import type { SettingsResponse as GeneratedSettingsResponse } from "../lib/api/generated/models/index.js";

type SettingsResponse = GeneratedSettingsResponse;

const defaultProviderCapabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_issue_pr_references: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_workflows: true,
  read_workflow_runs: true,
  workflow_dispatch: true,
  read_labels: true,
  read_markdown_images: true,
  read_authenticated_user: true,
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
  review_suggestion_application: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: false,
  thread_reply: false,
  thread_resolve: false,
  supported_review_actions: [],
};

const defaultOperationAvailability = { available: true };

const defaultRepoOperations = {
  add_comment: defaultOperationAvailability,
  add_label: defaultOperationAvailability,
  apply_review_suggestion: defaultOperationAvailability,
  approve_workflow: defaultOperationAvailability,
  close_issue: defaultOperationAvailability,
  close_pr: defaultOperationAvailability,
  dispatch_workflow: defaultOperationAvailability,
  mark_draft: defaultOperationAvailability,
  mark_ready_for_review: defaultOperationAvailability,
  merge_pr: defaultOperationAvailability,
  remove_label: defaultOperationAvailability,
  reopen_issue: defaultOperationAvailability,
  reopen_pr: defaultOperationAvailability,
  submit_review: defaultOperationAvailability,
};

function repoRef(owner: string, name: string, platformHost: string) {
  return {
    provider: "github",
    platform_host: platformHost,
    repo_path: `${owner}/${name}`,
    owner,
    name,
    capabilities: defaultProviderCapabilities,
  };
}

const pulls = [
  {
    ID: 1,
    RepoID: 1,
    GitHubID: 101,
    Number: 42,
    URL: "https://github.com/acme/widgets/pull/42",
    Title: "Add browser regression coverage",
    Author: "marius",
    State: "open",
    IsDraft: false,
    Body: "Adds Playwright smoke tests for workspace panel.",
    HeadBranch: "feature/playwright",
    BaseBranch: "main",
    Additions: 120,
    Deletions: 12,
    CommentCount: 3,
    ReviewDecision: "APPROVED",
    CIStatus: "success",
    CIChecksJSON: "[]",
    CreatedAt: "2026-03-29T14:00:00Z",
    UpdatedAt: "2026-03-30T14:00:00Z",
    LastActivityAt: "2026-03-30T14:00:00Z",
    MergedAt: null,
    ClosedAt: null,
    KanbanStatus: "reviewing",
    Starred: false,
    repo_owner: "acme",
    repo_name: "widgets",
    platform_host: "github.com",
    platform_head_sha: "42aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa42",
    repo: repoRef("acme", "widgets", "github.com"),
    worktree_links: [],
  },
  {
    ID: 2,
    RepoID: 2,
    GitHubID: 201,
    Number: 84,
    URL: "https://example.com/acme/widgets/pull/84",
    Title: "Mirror host stub PR",
    Author: "marius",
    State: "open",
    IsDraft: false,
    Body: "",
    HeadBranch: "mirror/stub",
    BaseBranch: "main",
    Additions: 1,
    Deletions: 0,
    CommentCount: 0,
    ReviewDecision: "",
    CIStatus: "success",
    CIChecksJSON: "[]",
    CreatedAt: "2026-03-29T14:00:00Z",
    UpdatedAt: "2026-03-30T14:00:00Z",
    LastActivityAt: "2026-03-30T14:00:00Z",
    MergedAt: null,
    ClosedAt: null,
    KanbanStatus: "new",
    Starred: false,
    repo_owner: "acme",
    repo_name: "widgets",
    platform_host: "example.com",
    platform_head_sha: "84bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb84",
    repo: repoRef("acme", "widgets", "example.com"),
    worktree_links: [],
  },
  {
    ID: 3,
    RepoID: 1,
    GitHubID: 301,
    Number: 55,
    URL: "https://github.com/acme/widgets/pull/55",
    Title: "Refactor theme system",
    Author: "luisa",
    State: "open",
    IsDraft: false,
    Body: "Consolidates theme tokens.",
    HeadBranch: "refactor/theme",
    BaseBranch: "main",
    Additions: 80,
    Deletions: 40,
    CommentCount: 0,
    ReviewDecision: "",
    CIStatus: "pending",
    CIChecksJSON: "[]",
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
    platform_head_sha: "55cccccccccccccccccccccccccccccccccccc55",
    repo: repoRef("acme", "widgets", "github.com"),
    worktree_links: [
      {
        worktree_key: "projects/theme-rework",
        worktree_branch: "refactor/theme",
      },
    ],
  },
];

const issues = [
  {
    ID: 2,
    RepoID: 1,
    GitHubID: 202,
    Number: 7,
    URL: "https://github.com/acme/widgets/issues/7",
    Title: "Theme toggle does not stick",
    Author: "marius",
    State: "open",
    Body: "",
    CommentCount: 1,
    LabelsJSON: "[]",
    CreatedAt: "2026-03-28T14:00:00Z",
    UpdatedAt: "2026-03-30T14:00:00Z",
    LastActivityAt: "2026-03-30T14:00:00Z",
    ClosedAt: null,
    Starred: false,
    platform_host: "github.com",
    repo_owner: "acme",
    repo_name: "widgets",
    repo: repoRef("acme", "widgets", "github.com"),
  },
];

const repos = [
  {
    ID: 1,
    Owner: "acme",
    Name: "widgets",
    Platform: "github",
    PlatformHost: "github.com",
    AllowSquashMerge: true,
    AllowMergeCommit: true,
    AllowRebaseMerge: true,
    ViewerCanMerge: true,
    LastSyncStartedAt: "2026-03-30T14:00:00Z",
    LastSyncCompletedAt: "2026-03-30T14:00:30Z",
    LastSyncError: "",
    CreatedAt: "2026-03-01T00:00:00Z",
    capabilities: defaultProviderCapabilities,
    operations: defaultRepoOperations,
  },
];

// Twelve repos with open PRs on only a few of them, so switching the summary
// page filter between "All" (2-digit count) and "Has PRs" (1-digit count)
// exercises the results-label digit change.
const repoSummaries = Array.from({ length: 12 }, (_, i) => {
  const name = `repo-${String(i + 1).padStart(2, "0")}`;
  return {
    owner: "acme",
    name,
    platform_host: "github.com",
    repo: {
      provider: "github",
      platform_host: "github.com",
      owner: "acme",
      name,
      repo_path: `acme/${name}`,
    },
    cached_pr_count: i < 4 ? 3 : 0,
    open_pr_count: i < 4 ? 3 : 0,
    draft_pr_count: 0,
    cached_issue_count: i < 3 ? 2 : 0,
    open_issue_count: i < 3 ? 2 : 0,
    most_recent_activity_at: "2026-03-30T12:00:00Z",
    last_sync_completed_at: "2026-03-30T14:00:30Z",
    last_sync_started_at: "2026-03-30T14:00:00Z",
    last_sync_error: "",
    active_authors: [],
    recent_issues: [],
  };
});

const syncStatus = {
  running: false,
  last_run_at: "2026-03-30T14:00:30Z",
  last_error: "",
};

export const mockSettings = {
  repos: [
    {
      provider: "github",
      platform_host: "github.com",
      issue_pr_references: true,
      owner: "acme",
      name: "widgets",
      repo_path: "acme/widgets",
      is_glob: false,
      matched_repo_count: 1,
      hidden_from_ui: false,
    },
  ],
  repo_presets: [],
  activity: {
    collapse_threads: false,
    default_branch_max_commits: 5000,
    use_workspace_activity_for_recency: false,
    default_branch_retention_days: 90,
    view_mode: "threaded",
    time_range: "7d",
    hide_closed: false,
    hide_bots: false,
  },
  detail: {
    initial_timeline_entry_limit: 50,
    collapse_single_line_breaks: false,
    render_commit_messages_as_markdown: false,
  },
  issues: {
    hide_bots: false,
  },
  pull_requests: {
    allow_mid_stack_merges: false,
    prefer_github_native_stacks: false,
  },
  workspaces: {
    auto_assign_on_create: false,
    default_sidebar_view: "diff",
  },
  modes: {
    activity: true,
    actions: false,
    docs: true,
    issues: true,
    pulls: true,
    repos: true,
    reviews: true,
    workspaces: true,
  },
  kata_projects: [],
  terminal: {
    font_family: "",
    font_size: 12,
    scrollback: 1000,
    line_height: 1,
    letter_spacing: 0,
    cursor_blink: true,
    font_ligatures: false,
    hide_tmux_status: false,
    graphics: true,
    tmux_mouse: true,
    retained_sessions: 10,
  },
  agents: [],
  notifications: {
    enabled: true,
  },
  fleet: {
    enabled: false,
    role: "hub",
    members: [],
    enrollments: [],
    peer_timeout: "2s",
    sessions: {
      include_unmanaged_details: false,
    },
    restart_required: false,
  },
  mcp: {
    enabled: false,
    restart_required: false,
    active_requires_auth: false,
  },
  roborev: {
    init_managed_clones: false,
  },
} satisfies SettingsResponse;

export function makeRateLimits() {
  const now = Date.now();
  return {
    provider_pools: {
      "github.com": {
        provider: "github",
        platform_host: "github.com",
        rate_principal: "host",
        principal_label: "Host credential",
        reserve_buffer: 200,
        sync_throttle_factor: 1,
        sync_paused: false,
        rest: {
          remaining: 4812,
          limit: 5000,
          reset_at: new Date(now + 42 * 60_000).toISOString(),
          known: true,
          requests: 188,
        },
        graphql: {
          remaining: 4950,
          limit: 5000,
          reset_at: new Date(now + 38 * 60_000).toISOString(),
          known: true,
          requests: 50,
        },
      },
    },
    local_ceilings: {
      "github.com": {
        provider: "github",
        platform_host: "github.com",
        rate_principal: "host",
        principal_label: "Host credential",
        limit: 50000,
        spent: 42,
        remaining: 49958,
      },
    },
  };
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function decodePathSegment(segment: string | undefined): string {
  return decodeURIComponent(segment ?? "");
}

function canonicalProvider(provider: string): string {
  const normalized = provider.toLowerCase();
  if (normalized === "gh") return "github";
  if (normalized === "gl") return "gitlab";
  if (normalized === "fj") return "forgejo";
  return normalized;
}

function defaultPlatformHost(provider: string): string {
  switch (canonicalProvider(provider)) {
    case "github":
      return "github.com";
    case "gitlab":
      return "gitlab.com";
    case "forgejo":
      return "codeberg.org";
    case "gitea":
      return "gitea.com";
    default:
      return "";
  }
}

function routePlatformHost(provider: string, hostSegment: string | undefined): string {
  const host = decodePathSegment(hostSegment).trim();
  return host || defaultPlatformHost(provider);
}

function matchesRouteIdentity(
  candidate: {
    repo?: { provider?: string | undefined } | undefined;
    platform_host: string;
    repo_owner: string;
    repo_name: string;
    Number: number;
  },
  ref: {
    provider: string;
    platformHost: string;
    owner: string;
    name: string;
    number: number;
  },
): boolean {
  return (
    canonicalProvider(candidate.repo?.provider ?? "") === ref.provider &&
    candidate.platform_host === ref.platformHost &&
    candidate.repo_owner === ref.owner &&
    candidate.repo_name === ref.name &&
    candidate.Number === ref.number
  );
}

function pullDetailResponse(pr: (typeof pulls)[number]) {
  return {
    merge_request: pr,
    repo: pr.repo,
    repo_owner: pr.repo_owner,
    repo_name: pr.repo_name,
    platform_host: pr.platform_host,
    platform_head_sha: pr.platform_head_sha ?? "",
    reviewed_head_sha: pr.platform_head_sha ?? "",
    detail_loaded: true,
    detail_fetched_at: "2026-03-30T14:00:00Z",
    deferred_merge_pending: false,
    worktree_links: pr.worktree_links,
  };
}

export interface MockRequest {
  method: string;
  url: URL;
  bodyText: string;
}

/**
 * Route override, equivalent to layering an extra page.route over mockApi
 * in the Playwright suite. Return a Response to handle the request, or
 * null/undefined to fall through to the default fixtures.
 */
export type MockRouteOverride = (req: MockRequest) => Response | null | undefined;

export interface MockApiHandler {
  /** Resolve one request against overrides, then the default fixtures. */
  handle: (req: MockRequest) => Response;
  /** Every request seen by this handler, in order. */
  requests: MockRequest[];
}

export interface MockApiHandle {
  fetch: typeof globalThis.fetch;
  /** Every request the app issued, in order. */
  requests: MockRequest[];
}

export function createMockApiHandler(overrides: MockRouteOverride[] = []): MockApiHandler {
  // Deep-clone so mutations (e.g. PATCH) don't leak between tests.
  const localPulls: typeof pulls = JSON.parse(JSON.stringify(pulls));
  const requests: MockRequest[] = [];

  function handle(req: MockRequest): Response {
    requests.push(req);
    for (const override of overrides) {
      const res = override(req);
      if (res) return res;
    }

    const { method } = req;
    const { pathname } = req.url;

    if (method === "GET" && pathname === "/api/v1/pulls") {
      return jsonResponse(localPulls);
    }

    const providerPrMatch = pathname.match(
      /^\/api\/v1(?:\/host\/([^/]+))?\/pulls\/([^/]+)\/([^/]+)\/([^/]+)\/(\d+)(?:\/(sync|sync\/async))?$/,
    );
    if (
      providerPrMatch &&
      ((method === "GET" && !providerPrMatch[6]) || (method === "POST" && providerPrMatch[6]?.startsWith("sync")))
    ) {
      const prProvider = canonicalProvider(decodePathSegment(providerPrMatch[2]));
      const platformHost = routePlatformHost(prProvider, providerPrMatch[1]);
      const prOwner = decodePathSegment(providerPrMatch[3]);
      const prName = decodePathSegment(providerPrMatch[4]);
      const prNumber = parseInt(providerPrMatch[5]!, 10);
      const pr = localPulls.find((p) =>
        matchesRouteIdentity(p, {
          provider: prProvider,
          platformHost,
          owner: prOwner,
          name: prName,
          number: prNumber,
        }),
      );
      if (pr) {
        return jsonResponse(pullDetailResponse(pr));
      }
      return jsonResponse({ error: "Not found" }, 404);
    }

    if (providerPrMatch && method === "PATCH" && !providerPrMatch[6]) {
      const prProvider = canonicalProvider(decodePathSegment(providerPrMatch[2]));
      const platformHost = routePlatformHost(prProvider, providerPrMatch[1]);
      const prOwner = decodePathSegment(providerPrMatch[3]);
      const prName = decodePathSegment(providerPrMatch[4]);
      const prNumber = parseInt(providerPrMatch[5]!, 10);
      const pr = localPulls.find((p) =>
        matchesRouteIdentity(p, {
          provider: prProvider,
          platformHost,
          owner: prOwner,
          name: prName,
          number: prNumber,
        }),
      );
      if (!pr) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      const reqBody = JSON.parse(req.bodyText || "{}");
      if (reqBody.title !== undefined) pr.Title = reqBody.title;
      if (reqBody.body !== undefined) pr.Body = reqBody.body;
      return jsonResponse(pullDetailResponse(pr));
    }

    const approvePrMatch = pathname.match(
      /^\/api\/v1(?:\/host\/([^/]+))?\/pulls\/([^/]+)\/([^/]+)\/([^/]+)\/(\d+)\/approve$/,
    );
    if (approvePrMatch && method === "POST") {
      const prProvider = canonicalProvider(decodePathSegment(approvePrMatch[2]));
      const platformHost = routePlatformHost(prProvider, approvePrMatch[1]);
      const prOwner = decodePathSegment(approvePrMatch[3]);
      const prName = decodePathSegment(approvePrMatch[4]);
      const prNumber = parseInt(approvePrMatch[5]!, 10);
      const pr = localPulls.find((p) =>
        matchesRouteIdentity(p, {
          provider: prProvider,
          platformHost,
          owner: prOwner,
          name: prName,
          number: prNumber,
        }),
      );
      if (!pr) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      return jsonResponse({ status: "approved" });
    }

    const singlePrMatch = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)$/);
    if (method === "GET" && singlePrMatch) {
      const prOwner = singlePrMatch[1];
      const prName = singlePrMatch[2];
      const prNumber = parseInt(singlePrMatch[3]!, 10);
      const pr = localPulls.find((p) => p.repo_owner === prOwner && p.repo_name === prName && p.Number === prNumber);
      if (pr) {
        return jsonResponse(pullDetailResponse(pr));
      }
      return jsonResponse({ error: "Not found" }, 404);
    }

    if (method === "GET" && pathname === "/api/v1/issues") {
      return jsonResponse(issues);
    }

    const providerIssueMatch = pathname.match(
      /^\/api\/v1(?:\/host\/([^/]+))?\/issues\/([^/]+)\/([^/]+)\/([^/]+)\/(\d+)(?:\/(sync|sync\/async))?$/,
    );
    if (
      providerIssueMatch &&
      ((method === "GET" && !providerIssueMatch[6]) || (method === "POST" && providerIssueMatch[6]?.startsWith("sync")))
    ) {
      const issueProvider = canonicalProvider(decodePathSegment(providerIssueMatch[2]));
      const platformHost = routePlatformHost(issueProvider, providerIssueMatch[1]);
      const issueOwner = decodePathSegment(providerIssueMatch[3]);
      const issueName = decodePathSegment(providerIssueMatch[4]);
      const issueNumber = parseInt(providerIssueMatch[5]!, 10);
      const issue = issues.find((candidate) =>
        matchesRouteIdentity(candidate, {
          provider: issueProvider,
          platformHost,
          owner: issueOwner,
          name: issueName,
          number: issueNumber,
        }),
      );
      if (!issue) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      return jsonResponse({
        issue,
        repo: issue.repo,
        events: [],
        platform_host: issue.platform_host,
        repo_owner: issue.repo_owner,
        repo_name: issue.repo_name,
        detail_loaded: true,
        detail_fetched_at: "2026-03-30T14:00:00Z",
      });
    }

    const singleIssueMatch = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)\/issues\/(\d+)$/);
    if (method === "GET" && singleIssueMatch) {
      const issueOwner = singleIssueMatch[1];
      const issueName = singleIssueMatch[2];
      const issueNumber = parseInt(singleIssueMatch[3]!, 10);
      const issue = issues.find(
        (candidate) =>
          candidate.repo_owner === issueOwner && candidate.repo_name === issueName && candidate.Number === issueNumber,
      );
      if (!issue) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      return jsonResponse({
        issue,
        repo: issue.repo,
        events: [],
        platform_host: issue.platform_host,
        repo_owner: issue.repo_owner,
        repo_name: issue.repo_name,
        detail_loaded: true,
        detail_fetched_at: "2026-03-30T14:00:00Z",
      });
    }

    const syncIssueMatch = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)\/issues\/(\d+)\/sync$/);
    if (method === "POST" && syncIssueMatch) {
      const issueOwner = syncIssueMatch[1];
      const issueName = syncIssueMatch[2];
      const issueNumber = parseInt(syncIssueMatch[3]!, 10);
      const issue = issues.find(
        (candidate) =>
          candidate.repo_owner === issueOwner && candidate.repo_name === issueName && candidate.Number === issueNumber,
      );
      if (!issue) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      return jsonResponse({
        issue,
        events: [],
        platform_host: issue.platform_host,
        repo_owner: issue.repo_owner,
        repo_name: issue.repo_name,
        detail_loaded: true,
        detail_fetched_at: "2026-03-30T14:00:00Z",
      });
    }

    if (method === "GET" && pathname === "/api/v1/repos") {
      return jsonResponse(repos);
    }

    if (method === "GET" && pathname === "/api/v1/repos/summary") {
      return jsonResponse(repoSummaries);
    }

    if (method === "GET" && pathname === "/api/v1/snapshot") {
      return jsonResponse({ hosts: [], workspaces: [] });
    }

    const singleRepoMatch = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)$/);
    if (method === "GET" && singleRepoMatch) {
      const repo = repos.find((r) => r.Owner === singleRepoMatch[1] && r.Name === singleRepoMatch[2]);
      if (!repo) {
        return jsonResponse({ error: "Not found" }, 404);
      }
      return jsonResponse(repo);
    }

    const providerRepoMatch = pathname.match(/^\/api\/v1(?:\/host\/([^/]+))?\/repo\/([^/]+)\/([^/]+)\/([^/]+)$/);
    if (method === "GET" && providerRepoMatch) {
      const repoProvider = canonicalProvider(decodePathSegment(providerRepoMatch[2]));
      const platformHost = routePlatformHost(repoProvider, providerRepoMatch[1]);
      const repoOwner = decodePathSegment(providerRepoMatch[3]);
      const repoName = decodePathSegment(providerRepoMatch[4]);
      const repo = repos.find(
        (r) =>
          canonicalProvider(r.Platform) === repoProvider &&
          r.PlatformHost === platformHost &&
          r.Owner === repoOwner &&
          r.Name === repoName,
      );
      if (!repo) {
        return jsonResponse({ error: "Not found" }, 404);
      }
      return jsonResponse(repo);
    }

    if (method === "GET" && pathname === "/api/v1/sync/status") {
      return jsonResponse(syncStatus);
    }

    if (method === "GET" && pathname === "/api/v1/settings") {
      return jsonResponse(mockSettings);
    }

    if (method === "GET" && pathname === "/api/v1/rate-limits") {
      return jsonResponse(makeRateLimits());
    }

    if (method === "POST" && pathname === "/api/v1/sync") {
      return jsonResponse(undefined, 202);
    }

    const patchPrMatch = pathname.match(/^\/api\/v1\/repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)$/);
    if (method === "PATCH" && patchPrMatch) {
      const prOwner = patchPrMatch[1];
      const prName = patchPrMatch[2];
      const prNumber = parseInt(patchPrMatch[3]!, 10);
      const pr = localPulls.find((p) => p.repo_owner === prOwner && p.repo_name === prName && p.Number === prNumber);
      if (!pr) {
        return jsonResponse({ title: "Not found" }, 404);
      }
      const reqBody = JSON.parse(req.bodyText || "{}");
      if (reqBody.title !== undefined) pr.Title = reqBody.title;
      if (reqBody.body !== undefined) pr.Body = reqBody.body;
      return jsonResponse(pullDetailResponse(pr));
    }

    return jsonResponse({ error: `Unhandled ${method} ${pathname}` }, 404);
  }

  return { handle, requests };
}

// Root-level liveness endpoints the app shell polls during startup
// (waitUntilBackendReady) before it mounts any view. The Playwright lane
// gets these from the Vite dev server's healthcheck plugin and never routes
// them through the mock (page.route only intercepts /api/v1); the jsdom lane
// stubs fetch wholesale, so the adapter must answer them or startup spins on
// "Loading" forever.
const READINESS_PATHS = new Set(["/healthz", "/livez"]);

export function createMockApiFetch(overrides: MockRouteOverride[] = []): MockApiHandle {
  const handler = createMockApiHandler(overrides);

  const mockFetch: typeof globalThis.fetch = async (input, init) => {
    // jsdom runs on undici, where `new Request("/relative")` throws because
    // there is no document base — but the app shell fetches readiness and
    // other paths relatively. Resolve against window.location the way a
    // browser would before building the Request.
    const request = input instanceof Request ? input : new Request(new URL(String(input), window.location.href), init);
    const url = new URL(request.url);
    if (READINESS_PATHS.has(url.pathname)) {
      // Overrides get first crack so startup-ordering specs can hold the
      // readiness poll unready; every other spec falls through to ok.
      const readinessReq: MockRequest = { method: request.method.toUpperCase(), url, bodyText: "" };
      for (const override of overrides) {
        const overridden = override(readinessReq);
        if (overridden) return overridden;
      }
      return jsonResponse({ status: "ok" });
    }
    return handler.handle({
      method: request.method.toUpperCase(),
      url,
      bodyText: request.method === "GET" || request.method === "HEAD" ? "" : await request.text(),
    });
  };

  return { fetch: mockFetch, requests: handler.requests };
}
