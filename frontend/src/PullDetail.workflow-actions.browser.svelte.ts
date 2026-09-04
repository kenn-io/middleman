import { cleanup, render } from "vitest-browser-svelte";
import { Effect } from "effect";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";

import "./app.css";
import type { GeneratedClient } from "./lib/api/generated-api.js";
import type { PullDetail } from "./lib/api/types.js";
import { type OwnedAppRuntime } from "./lib/app/runtime.js";
import { ACTIONS_KEY, NAVIGATE_KEY, STORES_KEY, UI_CONFIG_KEY } from "./lib/context.js";
import PullDetailTestHarness from "./lib/components/detail/PullDetailTestHarness.svelte";
import { createDetailActivityViewStore } from "./lib/stores/detail-activity-view.svelte.js";
import { createSettingsStore } from "./lib/stores/settings.svelte.js";
import { makeTestAppRuntime } from "./lib/testing/effect-layers.js";

const WAIT = { timeout: 10_000, interval: 50 } as const;

const workflow = {
  available: true,
  definition_sha: "release-definition",
  id: "release.yml",
  inputs: [],
  name: "Release",
  path: ".github/workflows/release.yml",
  state: "active",
  web_url: "https://github.com/acme/widgets/actions/workflows/release.yml",
} as const;

function pullDetail(): PullDetail {
  const capabilities: PullDetail["repo"]["capabilities"] = {
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
    comment_mutation: false,
    state_mutation: true,
    merge_mutation: true,
    label_mutation: true,
    assignee_mutation: false,
    reviewer_mutation: false,
    review_mutation: true,
    workflow_approval: false,
    ready_for_review: false,
    draft_mutation: false,
    issue_mutation: false,
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
  const unavailable = { available: false } as const;
  const operations: NonNullable<PullDetail["repo"]["operations"]> = {
    add_comment: unavailable,
    add_label: { available: true },
    apply_review_suggestion: unavailable,
    approve_workflow: unavailable,
    close_issue: unavailable,
    close_pr: { available: true },
    create_issue: unavailable,
    delete_comment: unavailable,
    dispatch_workflow: { available: true },
    edit_comment: unavailable,
    mark_draft: unavailable,
    mark_ready_for_review: unavailable,
    merge_pr: { available: true },
    remove_label: { available: true },
    reopen_issue: unavailable,
    reopen_pr: unavailable,
    reply_review_thread: unavailable,
    resolve_review_thread: unavailable,
    review_draft: unavailable,
    set_assignees: unavailable,
    set_reviewers: unavailable,
    submit_review: { available: true },
    update_content: unavailable,
  };
  const repo: PullDetail["repo"] = {
    provider: "github",
    platform_host: "github.com",
    owner: "acme",
    name: "widgets",
    repo_path: "acme/widgets",
    default_branch: "main",
    capabilities,
    operations,
  };
  return {
    detail_loaded: true,
    detail_fetched_at: "2026-08-28T12:00:00Z",
    deferred_merge_pending: false,
    diff_head_sha: "head",
    head_repo_kind: "same_repo",
    merge_base_sha: "base",
    platform_base_sha: "base",
    platform_head_sha: "head",
    reviewed_head_sha: "head",
    platform_host: "github.com",
    repo_owner: "acme",
    repo_name: "widgets",
    warnings: [],
    workflow_approval: { checked: false, count: 0, required: false },
    worktree_links: [],
    repo,
    merge_request: {
      ID: 1,
      RepoID: 1,
      PlatformID: 42,
      PlatformExternalID: "PR_42",
      Number: 42,
      URL: "https://github.com/acme/widgets/pull/42",
      Title: "Add browser regression coverage",
      Author: "marius",
      AuthorDisplayName: "Marius",
      State: "open",
      IsDraft: false,
      IsLocked: false,
      Body: "Adds browser coverage for provider workflows.",
      HeadBranch: "feature/workflow-actions",
      BaseBranch: "main",
      HeadRepoCloneURL: "https://github.com/acme/widgets.git",
      Additions: 12,
      FilesChanged: 3,
      MergeCommitSHA: "",
      Deletions: 2,
      CommentCount: 0,
      ReviewDecision: "APPROVED",
      CIStatus: "success",
      CIChecksJSON: "[]",
      CIHadPending: false,
      CreatedAt: "2026-08-28T10:00:00Z",
      UpdatedAt: "2026-08-28T12:00:00Z",
      LastActivityAt: "2026-08-28T12:00:00Z",
      MergedAt: null,
      ClosedAt: null,
      MergeableState: "clean",
      DetailFetchedAt: "2026-08-28T12:00:00Z",
      KanbanStatus: "reviewing",
      Starred: false,
      labels: [],
    },
    events: [],
  };
}

let runtime: OwnedAppRuntime | null = null;
let catalogClaimed = $state(false);

function visibleActionsTriggers(): HTMLButtonElement[] {
  return Array.from(document.querySelectorAll<HTMLButtonElement>("button.actions-menu-trigger")).filter((button) => {
    if (button.closest("[aria-hidden='true']")) return false;
    return getComputedStyle(button).display !== "none";
  });
}

function visibleButton(name: string): HTMLButtonElement | null {
  return (
    Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) =>
        button.textContent?.trim() === name &&
        !button.closest("[aria-hidden='true']") &&
        getComputedStyle(button).display !== "none" &&
        button.offsetParent !== null,
    ) ?? null
  );
}

afterEach(async () => {
  cleanup();
  if (runtime) await Effect.runPromise(runtime.disposeEffect);
  runtime = null;
  catalogClaimed = false;
});

describe("PullDetail provider workflow action geometry", () => {
  it("places workflow dispatch beside workspace tools while primary actions collapse only under pressure", async () => {
    const detail = pullDetail();
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
          operations: detail.repo.operations,
        },
      })),
      POST: vi.fn(async () => ({ data: {} })),
    } as unknown as GeneratedClient;
    runtime = makeTestAppRuntime(apiClient);
    const settings = createSettingsStore();
    settings.setModeVisibility({ ...settings.getModeVisibility(), actions: true });
    settings.setDetailSettings({ ...settings.getDetailSettings(), initial_timeline_entry_limit: 250 });
    const workflowActions = {
      loadCatalog: vi.fn(() => {
        catalogClaimed = true;
      }),
      refreshCatalog: vi.fn(),
      clearCatalogRefreshError: vi.fn(),
      selectWorkflow: vi.fn(),
      loadMoreRuns: vi.fn(),
      loadJobs: vi.fn(),
      dispatch: vi.fn(),
      newDispatchCycle: vi.fn(),
      applyDispatchProgress: vi.fn(),
      setEnabled: vi.fn(),
      getSnapshot: vi.fn(() => null),
      getCatalog: () => (catalogClaimed ? { repo: detail.repo, environments: [], workflows: [workflow] } : null),
      getEnvironments: () => [],
      getSelectedWorkflow: vi.fn(() => null),
      getRuns: vi.fn(() => []),
      getJobs: vi.fn(() => []),
      getLoading: vi.fn(() => ({ catalog: false, runs: false, jobs: [] })),
      getDispatch: () => null,
    };
    const detailStore = {
      loadDetail: vi.fn(),
      startDetailPolling: vi.fn(),
      stopDetailPolling: vi.fn(),
      getDetail: () => detail,
      getDetailEnvelopeTick: () => 0,
      isDetailLoading: () => false,
      getDetailError: () => null,
      isDetailSyncing: () => false,
      getDetailLoaded: () => true,
      updateKanbanState: vi.fn(),
      setPullState: vi.fn(),
      toggleDetailPRStar: vi.fn(),
      updatePRContent: vi.fn(),
      refreshPendingCI: vi.fn(),
      syncDetailNow: vi.fn(),
      refreshDetailOnly: vi.fn(),
      approvePull: vi.fn(),
      requestPullChanges: vi.fn(),
      markPullReady: vi.fn(),
      approvePullWorkflows: vi.fn(),
      mergePull: vi.fn(),
      editComment: vi.fn(),
      savePRBodyInBackground: vi.fn(),
      setLocalPRBody: vi.fn(),
      applyReviewSuggestions: vi.fn(),
    };
    const wrapper = document.createElement("div");
    wrapper.style.width = "900px";
    document.body.appendChild(wrapper);

    render(PullDetailTestHarness, {
      target: wrapper,
      props: {
        runtime,
        detailProps: {
          owner: "acme",
          name: "widgets",
          number: 42,
          provider: "github",
          platformHost: "github.com",
          repoPath: "acme/widgets",
          hideTabs: true,
          hideWorkspaceAction: false,
          autoSync: false,
        },
      },
      context: new Map<symbol, unknown>([
        [
          STORES_KEY,
          {
            detail: detailStore,
            pulls: { loadPulls: vi.fn() },
            activity: { loadActivity: vi.fn() },
            detailActivityView: createDetailActivityViewStore(),
            settings,
            workflowActions,
          },
        ],
        [ACTIONS_KEY, { pull: [] }],
        [UI_CONFIG_KEY, { hideStar: true }],
        [NAVIGATE_KEY, vi.fn()],
      ]),
    });

    let workflowTrigger: HTMLButtonElement | null = null;
    let workspaceTrigger: HTMLButtonElement | null = null;
    await vi.waitFor(() => {
      expect(visibleActionsTriggers()).toHaveLength(1);
      workflowTrigger = visibleButton("Run workflow");
      workspaceTrigger = visibleButton("Create Workspace");
      expect(workflowTrigger).not.toBeNull();
      expect(workspaceTrigger).not.toBeNull();
      expect(workflowTrigger!.closest(".actions-row--utility")).toBe(
        workspaceTrigger!.closest(".actions-row--utility"),
      );
      expect(visibleButton("Actions")).toBeNull();
      expect(visibleButton("Approve")).not.toBeNull();
      expect(visibleButton("Squash and merge")).not.toBeNull();
      expect(visibleButton("Close")).not.toBeNull();
    }, WAIT);

    workflowTrigger!.click();
    await vi.waitFor(() => {
      const workflowMenu = document.querySelector<HTMLElement>(".workflow-actions-menu--floating");
      expect(workflowMenu).not.toBeNull();
      expect(workflowMenu!.textContent).toContain("GitHub Actions");
      expect(visibleButton("Release")).not.toBeNull();
      const triggerRect = workflowTrigger!.getBoundingClientRect();
      const menuRect = workflowMenu!.getBoundingClientRect();
      expect(Math.abs(menuRect.left - triggerRect.left)).toBeLessThanOrEqual(1);
      expect(menuRect.top).toBeGreaterThanOrEqual(triggerRect.bottom);
      expect(menuRect.width).toBeGreaterThanOrEqual(220);
    }, WAIT);

    workflowTrigger!.click();
    wrapper.style.width = "180px";
    await vi.waitFor(() => {
      expect(document.querySelector(".pull-detail-content--actions-menu")).not.toBeNull();
      expect(visibleActionsTriggers()).toHaveLength(1);
      expect(visibleButton("Actions")).not.toBeNull();
      expect(visibleButton("Run workflow")).toBeNull();
      expect(visibleButton("Create Workspace")).toBeNull();
      expect(visibleButton("Approve")).toBeNull();
      expect(visibleButton("Squash and merge")).toBeNull();
      expect(visibleButton("Close")).toBeNull();
    }, WAIT);

    visibleButton("Actions")!.click();
    await vi.waitFor(() => {
      const menu = document.querySelector<HTMLElement>(".actions-menu-popover");
      expect(menu).not.toBeNull();
      const labels = Array.from(
        menu!.querySelectorAll("button"),
        (button) => button.getAttribute("aria-label") ?? button.textContent?.trim(),
      );
      expect(labels).toEqual(
        expect.arrayContaining(["Approve", "Squash and merge", "Close", "Labels", "Create Workspace", "Release"]),
      );
      expect(menu!.textContent).toContain("GitHub Actions");
      expect(visibleActionsTriggers()).toHaveLength(1);
    }, WAIT);

    wrapper.remove();
  });
});
