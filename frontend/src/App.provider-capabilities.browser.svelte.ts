// Browser-tier reimplementation of frontend/tests/e2e-full/provider-capabilities.spec.ts.
// Provider capabilities drive two pieces of detail-view rendering: a locked PR
// shows the "Locked" chip only on a provider that supports locking (GitHub),
// and a GitLab issue whose repo reports comment_mutation:false hides the
// timeline edit control while keeping the read-only copy affordance. The app is
// mounted for real through the browser harness with fetch mocked at the network
// boundary, so the rendered DOM reflects exactly what the capability data
// produced.
//
// A real Chromium page provides matchMedia/ResizeObserver/IntersectionObserver/
// canvas natively, so the jsdom installAppDomGlobals() shim is gone; the browser
// harness stubs only EventSource. The chip/button assertions are about exact DOM
// shape, which the page locator API (getByText/getByRole/getByTitle/getByTestId)
// does not fully expose, so they stay as scoped querySelector against the real
// DOM, wrapped in vi.waitFor for the genuine async render/network chain.
//
// The original Playwright spec also fetched the raw /api/v1 capabilities JSON
// and asserted its shape. That sub-assertion is dropped here: the backend
// capability contract is owned by the Go API tests, not by this UI-render test.

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { page } from "vite-plus/test/browser";

import { mountBrowserApp, resetKeyboardModuleState, type MountedBrowserApp } from "./test/browserAppHarness.js";
import { jsonResponse, type MockRouteOverride } from "./test/mockApiFetch.js";

// Real Chromium drives the genuine async render/network chain, which is slower
// than jsdom's synchronous fixtures, so each poll gets a generous window.
const WAIT = 10_000;

const githubCapabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_workflows: true,
  read_workflow_runs: true,
  workflow_dispatch: true,
  read_labels: true,
  comment_mutation: true,
  state_mutation: true,
  merge_mutation: true,
  label_mutation: true,
  review_mutation: true,
  workflow_approval: true,
  ready_for_review: true,
  issue_mutation: true,
};

// A locked GitHub PR detail (acme/widgets#1). Mirrors the shape pullDetailResponse
// builds in mockApiFetch, inlined so the fixture stays self-contained.
const lockedPullDetail = {
  merge_request: {
    ID: 9001,
    RepoID: 1,
    GitHubID: 9101,
    Number: 1,
    URL: "https://github.com/acme/widgets/pull/1",
    Title: "Locked pull request",
    Author: "marius",
    State: "open",
    IsDraft: false,
    IsLocked: true,
    Body: "A pull request locked on a provider that supports locking.",
    HeadBranch: "feature/locked",
    BaseBranch: "main",
    Additions: 5,
    Deletions: 1,
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
    platform_host: "github.com",
    platform_head_sha: "01aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
    repo: {
      provider: "github",
      platform_host: "github.com",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      capabilities: githubCapabilities,
    },
    worktree_links: [],
  },
  repo: {
    provider: "github",
    platform_host: "github.com",
    repo_path: "acme/widgets",
    owner: "acme",
    name: "widgets",
    capabilities: githubCapabilities,
  },
  repo_owner: "acme",
  repo_name: "widgets",
  platform_host: "github.com",
  platform_head_sha: "01aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
  reviewed_head_sha: "01aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
  detail_loaded: true,
  detail_fetched_at: "2026-03-30T14:00:00Z",
  worktree_links: [],
};

function lockedPullRoute(): MockRouteOverride {
  return (req) => {
    if (req.method !== "GET") return null;
    if (req.url.pathname !== "/api/v1/pulls/github/acme/widgets/1") return null;
    return jsonResponse(lockedPullDetail);
  };
}

// A GitLab issue (group/project#11) whose repo reports comment_mutation:false.
// Mirrors the gitLabReadOnlyIssueFixture seed in cmd/e2e-server/main.go.
function gitlabReadOnlyCapabilities(commentMutation: boolean) {
  return {
    read_repositories: false,
    read_merge_requests: false,
    read_issues: true,
    read_comments: true,
    read_releases: false,
    read_ci: false,
    read_workflows: false,
    read_workflow_runs: false,
    workflow_dispatch: false,
    read_labels: false,
    comment_mutation: commentMutation,
    state_mutation: false,
    merge_mutation: false,
    label_mutation: false,
    review_mutation: false,
    workflow_approval: false,
    ready_for_review: false,
    issue_mutation: false,
  };
}

function gitlabIssueDetail(commentMutation: boolean) {
  const repo = {
    provider: "gitlab",
    platform_host: "gitlab.example.com",
    repo_path: "group/project",
    owner: "group",
    name: "project",
    capabilities: gitlabReadOnlyCapabilities(commentMutation),
  };
  return {
    issue: {
      ID: 9011,
      RepoID: 9,
      GitHubID: 7101,
      Number: 11,
      URL: "https://gitlab.example.com/group/project/-/issues/11",
      Title: "GitLab read-only issue",
      Author: "ada",
      State: "open",
      Body: "GitLab issue body",
      CommentCount: 1,
      LabelsJSON: "[]",
      CreatedAt: "2026-03-28T14:00:00Z",
      UpdatedAt: "2026-03-30T14:00:00Z",
      LastActivityAt: "2026-03-30T14:00:00Z",
      ClosedAt: null,
      Starred: false,
      platform_host: "gitlab.example.com",
      repo_owner: "group",
      repo_name: "project",
      repo,
    },
    repo,
    events: [
      {
        ID: 7201,
        IssueNumber: 11,
        PlatformID: 7201,
        EventType: "issue_comment",
        Author: "ada",
        Body: "GitLab read-only timeline comment",
        Summary: "",
        MetadataJSON: "",
        DedupeKey: "gitlab-read-only-issue-comment",
        CreatedAt: "2026-03-30T14:00:00Z",
        ThreadID: null,
        Resolvable: false,
        Resolved: false,
      },
    ],
    platform_host: "gitlab.example.com",
    repo_owner: "group",
    repo_name: "project",
    detail_loaded: true,
    detail_fetched_at: "2026-03-30T14:00:00Z",
  };
}

// The app abbreviates the gitlab provider in API paths (the Playwright spec
// fetched it as /issues/gl/...), while the in-app route uses the full provider
// token. Match either so the override is robust to the path the app emits.
function gitlabReadOnlyIssueRoute(commentMutation: boolean): MockRouteOverride {
  const detail = gitlabIssueDetail(commentMutation);
  return (req) => {
    if (req.method !== "GET") return null;
    if (!/^\/api\/v1\/host\/gitlab\.example\.com\/issues\/(gl|gitlab)\/group\/project\/11$/.test(req.url.pathname)) {
      return null;
    }
    return jsonResponse(detail);
  };
}

function chipsRowText(): string {
  return document.querySelector(".pull-detail .chips-row")?.textContent ?? "";
}

function issueDetailText(): string {
  return document.querySelector(".issue-detail")?.textContent ?? "";
}

function issueButtonCount(label: string): number {
  return document.querySelectorAll(`.issue-detail [aria-label="${label}"]`).length;
}

describe("provider capabilities: locked PR chip", () => {
  vi.setConfig({ testTimeout: 30_000 });

  let mounted: MountedBrowserApp | null = null;

  beforeEach(async () => {
    // The container store classifies layout by #app's clientWidth; a real
    // desktop viewport keeps the desktop detail DOM rendering here.
    await page.viewport(1280, 900);
  });

  afterEach(async () => {
    mounted?.unmount();
    mounted = null;
    localStorage.clear();
    await resetKeyboardModuleState();
  });

  it("shows the Locked chip for a locked PR on a provider that supports locking", async () => {
    mounted = await mountBrowserApp("/pulls/github/acme/widgets/1", {
      overrides: [lockedPullRoute()],
    });

    await expect.element(page.getByText("Locked pull request")).toBeVisible();
    await vi.waitFor(() => {
      expect(chipsRowText()).toContain("Locked");
      expect(chipsRowText()).not.toContain("Open");
    }, WAIT);
  });
});

describe("provider capabilities: GitLab read-only issue timeline", () => {
  vi.setConfig({ testTimeout: 30_000 });

  let mounted: MountedBrowserApp | null = null;

  beforeEach(async () => {
    await page.viewport(1280, 900);
  });

  afterEach(async () => {
    mounted?.unmount();
    mounted = null;
    localStorage.clear();
    await resetKeyboardModuleState();
  });

  it("hides timeline edit controls when comments are read-only", async () => {
    mounted = await mountBrowserApp("/host/gitlab.example.com/issues/gitlab/group/project/11", {
      overrides: [gitlabReadOnlyIssueRoute(false)],
    });

    await expect.element(page.getByText("GitLab read-only issue")).toBeVisible();
    await vi.waitFor(() => {
      expect(issueDetailText()).toContain("GitLab read-only timeline comment");
    }, WAIT);
    expect(issueButtonCount("Edit comment")).toBe(0);
    expect(issueButtonCount("Copy comment")).toBe(1);
  });

  it("offers timeline edit controls when the provider allows comment mutation", async () => {
    // Contrast case proving the read-only branch is capability-driven, not a
    // hardcoded absence of the edit control.
    mounted = await mountBrowserApp("/host/gitlab.example.com/issues/gitlab/group/project/11", {
      overrides: [gitlabReadOnlyIssueRoute(true)],
    });

    await expect.element(page.getByText("GitLab read-only timeline comment")).toBeVisible();
    await vi.waitFor(() => expect(issueButtonCount("Edit comment")).toBe(1), WAIT);
  });
});
