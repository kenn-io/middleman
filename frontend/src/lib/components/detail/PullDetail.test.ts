import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { Effect } from "effect";
import { tick, type ComponentProps } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { DiffResult, Label, PullDetail } from "../../api/types.js";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";
import { ACTIONS_KEY, NAVIGATE_KEY, STORES_KEY, UI_CONFIG_KEY } from "../../context.js";
import { createDetailActivityViewStore } from "../../stores/detail-activity-view.svelte.js";
import { createDetailStore } from "../../stores/detail.svelte.js";
import { makeTestAppRuntime } from "../../testing/effect-layers.js";
import { createSettingsStore } from "../../stores/settings.svelte.js";
import { createWorkflowActionsStore } from "../../stores/workflow-actions.svelte.js";
import { dismissFlash, getFlashes, showFlash } from "../../stores/flash.svelte.js";
import {
  discardWorkspaceLaunch,
  nextWorkspaceLifecycleTick,
  resetWorkspaceCreatePendingForTest,
} from "../../stores/workspace-create-pending.svelte.js";
import type { GeneratedClient } from "../../api/generated-api.js";
import type { ProblemBody } from "../../api/problems.js";
import type { ProviderRouteRef } from "../../api/provider-routes.js";
import type { DetailSyncCallbacks, MergePullCallbacks, ProviderActionCallbacks } from "../../stores/detail.svelte.js";
import type { InlineWorkspaceController, WorkspaceItemIdentity } from "../../workspace-inline.js";
import { openLabelPickerFor } from "./labelPickerCommand.js";
import { createTestController } from "../workspace/inlineWorkspaceTestController.svelte.js";
import * as workspaceHost from "../../stores/workspace-host.svelte.js";

const launchTargets = [
  {
    key: "codex",
    label: "Codex",
    kind: "agent",
    source: "builtin",
    command: ["codex"],
    available: true,
    disabled_reason: "",
  },
];

// The pending-create store is module-scoped so it can survive component
// remounts; tests that leave a deferred create unresolved must not leak
// that pending identity into later tests.
afterEach(resetWorkspaceCreatePendingForTest);

let detailRuntime: OwnedAppRuntime | null = null;

afterEach(async () => {
  if (detailRuntime === null) return;
  await Effect.runPromise(detailRuntime.disposeEffect);
  detailRuntime = null;
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const markdownMockState = vi.hoisted(() => ({
  pending: false,
  pendingPromise: new Promise<string>(() => undefined),
}));

const clipboardMockState = vi.hoisted(() => ({
  resolvers: [] as Array<(ok: boolean) => void>,
}));

vi.mock("@kenn-io/kit-ui", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@kenn-io/kit-ui")>();
  return {
    ...actual,
    copyToClipboard: vi.fn(() => new Promise<boolean>((resolve) => clipboardMockState.resolvers.push(resolve))),
  };
});

vi.mock("../../utils/markdown.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../utils/markdown.js")>();
  return {
    ...actual,
    renderMarkdown: vi.fn((raw: string, repo?: unknown, opts?: unknown) =>
      markdownMockState.pending
        ? markdownMockState.pendingPromise
        : actual.renderMarkdown(
            raw,
            repo as Parameters<typeof actual.renderMarkdown>[1],
            opts as Parameters<typeof actual.renderMarkdown>[2],
          ),
    ),
  };
});

import PullDetailComponent from "./PullDetail.svelte";
import PullDetailTestHarness from "./PullDetailTestHarness.svelte";

const capabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_workflows: false,
  read_workflow_runs: false,
  workflow_dispatch: false,
  read_labels: false,
  comment_mutation: false,
  state_mutation: true,
  merge_mutation: false,
  review_mutation: false,
  workflow_approval: false,
  ready_for_review: false,
  issue_mutation: false,
  label_mutation: false,
};

function reviewEvent(author: string, summary = "APPROVED", createdAt = "2026-05-01T12:00:00Z") {
  return {
    ID: Math.floor(Math.random() * 1_000_000),
    MergeRequestID: 1,
    PlatformID: 1,
    PlatformExternalID: "",
    EventType: "review",
    Author: author,
    Summary: summary,
    Body: "",
    MetadataJSON: "",
    CreatedAt: createdAt,
    DedupeKey: `review-${author}-${summary}-${createdAt}`,
  };
}

function pullDetail(): PullDetail {
  return {
    detail_loaded: true,
    detail_fetched_at: "2026-05-01T12:05:00Z",
    deferred_merge_pending: false,
    diff_head_sha: "head",
    merge_base_sha: "base",
    platform_base_sha: "base",
    platform_head_sha: "head",
    reviewed_head_sha: "head",
    head_repo_kind: "same_repo",
    platform_host: "github.com",
    repo_owner: "acme",
    repo_name: "widget",
    warnings: [],
    workflow_approval: {
      count: 0,
      required: false,
      runs: [],
    },
    workspace: undefined,
    worktree_links: [],
    repo: {
      ID: 1,
      Owner: "acme",
      Name: "widget",
      Host: "github.com",
      PlatformHost: "github.com",
      Platform: "github",
      URL: "https://github.com/acme/widget",
      DefaultBranch: "main",
      IsArchived: false,
      AllowSquashMerge: false,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      capabilities,
      provider: "github",
      platform_host: "github.com",
      owner: "acme",
      name: "widget",
      repo_path: "acme/widget",
    },
    merge_request: {
      ID: 1,
      RepoID: 1,
      PlatformID: 100,
      PlatformExternalID: "PR_1",
      Number: 1,
      URL: "https://github.com/acme/widget/pull/1",
      Title: "Make approval counts visible",
      Author: "octocat",
      AuthorDisplayName: "Octocat",
      State: "open",
      IsDraft: false,
      IsLocked: false,
      Body: "",
      HeadBranch: "feature",
      BaseBranch: "main",
      HeadRepoCloneURL: "https://github.com/acme/widget.git",
      Additions: 0,
      Deletions: 0,
      CommentCount: 0,
      ReviewDecision: "APPROVED",
      CIStatus: "",
      CIChecksJSON: "",
      CIHadPending: false,
      CreatedAt: "2026-05-01T11:00:00Z",
      UpdatedAt: "2026-05-01T12:00:00Z",
      LastActivityAt: "2026-05-01T12:00:00Z",
      MergedAt: null,
      ClosedAt: null,
      MergeableState: "clean",
      DetailFetchedAt: "2026-05-01T12:05:00Z",
      KanbanStatus: "new",
      Starred: false,
      labels: [],
    },
    events: [
      reviewEvent("alice", "APPROVED", "2026-05-01T12:00:00Z"),
      reviewEvent("bob", "APPROVED", "2026-05-01T11:59:00Z"),
    ],
  };
}

function renderPullDetail(
  detail: PullDetail,
  repoSettings = {
    AllowSquashMerge: false,
    AllowMergeCommit: false,
    AllowRebaseMerge: false,
    ViewerCanMerge: true,
  },
  apiClient = {
    GET: vi.fn(async () => ({
      data: repoSettings,
    })),
    POST: vi.fn(async () => ({
      data: {},
    })),
  },
  options: {
    hideWorkspaceAction?: boolean;
    phonePresentation?: boolean;
    onOpenWorkspace?: (workspaceId: string) => void;
    hideTabs?: boolean;
    actions?: { pull: unknown[] };
    detailLoading?: boolean;
    detailSyncing?: boolean;
    deferRefresh?: boolean;
    refreshFailure?: string;
    diff?: ReturnType<typeof makeReviewSuggestionDiffStore>;
    diffReviewDraft?: { setRouteContext: ReturnType<typeof vi.fn>; isSubmitting: () => boolean };
    inlineWorkspace?: InlineWorkspaceController | null;
    onDetailTabChange?: ((tab: "conversation" | "files") => void) | undefined;
    runtimeClient?: GeneratedClient;
    actionsModeVisible?: boolean;
    detailProps?: Partial<ComponentProps<typeof PullDetailComponent>>;
  } = {},
) {
  const actions = options.actions ?? { pull: [] };
  let envelopeTick = 0;
  let pendingRefreshCallbacks: DetailSyncCallbacks | null = null;
  const runProviderAction = (path: string, body: unknown, callbacks: ProviderActionCallbacks): void => {
    void apiClient.POST(path, { body }).then((result) => {
      const error = "error" in result ? (result.error as ProblemBody | undefined) : undefined;
      if (error !== undefined) {
        callbacks.onProblem?.(error);
        callbacks.onFailure?.(error.detail ?? error.title ?? "provider action failed");
      } else {
        const warning = result.data?.workspace_cleanup_warning;
        if (warning) {
          showFlash(`Pull request merged, but the workspace was not pruned: ${warning}`, { tone: "warning" });
        }
        callbacks.onSuccess?.();
      }
      callbacks.onSettled?.();
    });
  };
  const runMergeAction = (path: string, body: unknown, callbacks: MergePullCallbacks): void => {
    void apiClient.POST(path, { body }).then((result) => {
      const error = "error" in result ? (result.error as ProblemBody | undefined) : undefined;
      if (error !== undefined) {
        callbacks.onProblem?.(error);
        callbacks.onFailure?.(error.detail ?? error.title ?? "provider action failed");
      } else {
        const warning = result.data?.workspace_cleanup_warning;
        if (warning) {
          showFlash(`Pull request merged, but the workspace was not pruned: ${warning}`, { tone: "warning" });
        }
        callbacks.onSuccess?.(
          path.endsWith("/merge/deferred")
            ? { _tag: "Queued" }
            : warning === undefined
              ? { _tag: "Merged" }
              : { _tag: "Merged", cleanupWarning: warning },
        );
      }
      callbacks.onSettled?.();
    });
  };
  const detailStore = {
    loadDetail: vi.fn(async () => undefined),
    startDetailPolling: vi.fn(),
    stopDetailPolling: vi.fn(),
    getDetail: () => detail,
    getDetailEnvelopeTick: () => envelopeTick,
    isDetailLoading: () => options.detailLoading ?? false,
    getDetailError: () => null,
    isDetailSyncing: () => options.detailSyncing ?? false,
    getDetailLoaded: () => true,
    updateKanbanState: vi.fn(),
    setPullState: vi.fn(
      (_ref: ProviderRouteRef, _number: number, _state: string, callbacks: ProviderActionCallbacks) => {
        callbacks.onSuccess?.();
        callbacks.onSettled?.();
      },
    ),
    toggleDetailPRStar: vi.fn(),
    updatePRContent: vi.fn(),
    refreshPendingCI: vi.fn(async () => undefined),
    syncDetailNow: vi.fn(
      (_owner: string, _name: string, _number: number, _identity: unknown, callbacks: DetailSyncCallbacks) => {
        if (options.deferRefresh) {
          options.detailSyncing = true;
          pendingRefreshCallbacks = callbacks;
          return;
        }
        if (options.refreshFailure) callbacks.onFailure?.(options.refreshFailure);
        else callbacks.onSuccess?.(true);
        callbacks.onSettled?.();
      },
    ),
    refreshDetailOnly: vi.fn(
      (_owner: string, _name: string, _number: number, _identity: unknown, callbacks?: { onSettled?: () => void }) => {
        callbacks?.onSettled?.();
      },
    ),
    approvePull: vi.fn((_ref: ProviderRouteRef, _number: number, body: unknown, callbacks: ProviderActionCallbacks) =>
      runProviderAction("/approve", body, callbacks),
    ),
    requestPullChanges: vi.fn(
      (_ref: ProviderRouteRef, _number: number, body: unknown, callbacks: ProviderActionCallbacks) =>
        runProviderAction("/request-changes", body, callbacks),
    ),
    markPullReady: vi.fn((_ref: ProviderRouteRef, _number: number, callbacks: ProviderActionCallbacks) =>
      runProviderAction("/ready-for-review", undefined, callbacks),
    ),
    approvePullWorkflows: vi.fn((_ref: ProviderRouteRef, _number: number, callbacks: ProviderActionCallbacks) =>
      runProviderAction("/approve-workflows", undefined, callbacks),
    ),
    mergePull: vi.fn(
      (_ref: ProviderRouteRef, _number: number, body: unknown, deferred: boolean, callbacks: MergePullCallbacks) =>
        runMergeAction(deferred ? "/merge/deferred" : "/merge", body, callbacks),
    ),
    editComment: vi.fn(),
    savePRBodyInBackground: vi.fn(),
    setLocalPRBody: vi.fn(),
    applyReviewSuggestions: vi.fn(
      (
        _owner: string,
        _name: string,
        _number: number,
        _input: unknown,
        callbacks: { onResult?: (value: boolean) => void; onSettled?: () => void },
      ) => {
        callbacks.onResult?.(true);
        callbacks.onSettled?.();
      },
    ),
  };
  const navigate = vi.fn();

  const runtime =
    detailRuntime ?? makeTestAppRuntime(options.runtimeClient ?? (apiClient as unknown as GeneratedClient));
  detailRuntime = runtime;
  const settings = createSettingsStore();
  settings.setLaunchTargets(launchTargets);
  settings.setDetailSettings({ initial_timeline_entry_limit: 250 });
  settings.setModeVisibility({
    ...settings.getModeVisibility(),
    actions: options.actionsModeVisible ?? false,
  });
  const workflowActions = createWorkflowActionsStore({ runtime });
  let detailProps: ComponentProps<typeof PullDetailComponent> = {
    owner: "acme",
    name: "widget",
    number: detail.merge_request.Number,
    provider: "github",
    platformHost: "github.com",
    repoPath: "acme/widget",
    hideWorkspaceAction: options.hideWorkspaceAction ?? true,
    phonePresentation: options.phonePresentation ?? false,
    hideTabs: options.hideTabs ?? false,
    inlineWorkspace: options.inlineWorkspace ?? null,
    onDetailTabChange: options.onDetailTabChange,
    onOpenWorkspace: options.onOpenWorkspace,
    ...options.detailProps,
  };
  const rendered = render(PullDetailTestHarness, {
    props: {
      runtime,
      detailProps,
    },
    context: new Map<symbol, unknown>([
      [
        STORES_KEY,
        {
          detail: detailStore,
          ...(options.diff === undefined ? {} : { diff: options.diff }),
          ...(options.diffReviewDraft === undefined ? {} : { diffReviewDraft: options.diffReviewDraft }),
          pulls: { loadPulls: vi.fn() },
          activity: { loadActivity: vi.fn() },
          detailActivityView: createDetailActivityViewStore(),
          settings,
          workflowActions,
        },
      ],
      [ACTIONS_KEY, actions],
      [UI_CONFIG_KEY, { hideStar: true }],
      [NAVIGATE_KEY, navigate],
    ]),
  });
  return {
    ...rendered,
    rerender: (next: Partial<ComponentProps<typeof PullDetailComponent>>) => {
      detailProps = { ...detailProps, ...next };
      return rendered.rerender({ runtime, detailProps });
    },
    detailStore,
    navigate,
    settings,
    workflowActions,
    settleRefresh: () => {
      options.detailSyncing = false;
      pendingRefreshCallbacks?.onSuccess?.(true);
      pendingRefreshCallbacks?.onSettled?.();
      pendingRefreshCallbacks = null;
    },
    failRefresh: (message: string) => {
      options.detailSyncing = false;
      pendingRefreshCallbacks?.onFailure?.(message);
      pendingRefreshCallbacks?.onSettled?.();
      pendingRefreshCallbacks = null;
    },
    setEnvelopeTick: (tick: number) => {
      envelopeTick = tick;
    },
  };
}

function addReviewSuggestionToDetail(detail: PullDetail): void {
  detail.repo.capabilities.review_suggestion_application = true;
  detail.repo.operations = {
    apply_review_suggestion: {
      available: true,
    },
  } as unknown as PullDetail["repo"]["operations"];
  detail.events = [
    {
      ID: 501,
      MergeRequestID: detail.merge_request.ID,
      PlatformID: 501,
      PlatformExternalID: "review-comment-501",
      EventType: "review_comment",
      Author: "reviewer",
      Summary: "",
      Body: ["This can return directly.", "", "```suggestion", "return client.publishThreads();", "```"].join("\n"),
      MetadataJSON: "",
      CreatedAt: "2026-05-01T12:01:00Z",
      DedupeKey: "review-comment-501",
      ThreadID: null,
      Resolvable: true,
      Resolved: false,
      diff_thread: {
        id: "501",
        provider_comment_id: "review-comment-501",
        path: "src/review.ts",
        side: "right",
        start_side: "right",
        start_line: 10,
        line: 11,
        new_line: 11,
        line_type: "context",
        diff_head_sha: "head",
        commit_sha: "head",
        body: "This can return directly.",
        author_login: "reviewer",
        resolved: false,
        can_resolve: true,
        created_at: "2026-05-01T12:01:00Z",
        updated_at: "2026-05-01T12:01:00Z",
      },
    },
  ];
}

function makeReviewSuggestionDiffStore() {
  const diff: DiffResult = {
    stale: false,
    whitespace_only_count: 0,
    files: [
      {
        path: "src/review.ts",
        old_path: "src/review.ts",
        status: "modified",
        is_binary: false,
        is_whitespace_only: false,
        additions: 2,
        deletions: 0,
        hunks: [
          {
            old_start: 9,
            old_count: 1,
            new_start: 9,
            new_count: 3,
            lines: [
              {
                type: "context",
                old_num: 9,
                new_num: 9,
                content: "const client = setup();",
              },
              {
                type: "add",
                new_num: 10,
                content: "client.enableReviews();",
              },
              {
                type: "add",
                new_num: 11,
                content: "client.publishThreads();",
              },
              {
                type: "context",
                old_num: 10,
                new_num: 12,
                content: "return client;",
              },
            ],
          },
        ],
      },
    ],
  };
  return {
    getDiff: () => diff,
    isDiffLoading: () => false,
    getCurrentPR: () => ({
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 1,
    }),
    getTabWidth: () => 4,
    loadDiff: vi.fn(),
    requestScrollToLine: vi.fn(),
  };
}

function getActionMenuLabelsButton(): HTMLButtonElement {
  const button = document.querySelector<HTMLButtonElement>(".actions-menu-popover .btn--labels");
  if (button === null) {
    throw new Error("actions menu Labels button not found");
  }
  return button;
}

const releaseWorkflow = {
  available: true,
  definition_sha: "release-definition",
  id: "release.yml",
  inputs: [],
  name: "Release",
  path: ".github/workflows/release.yml",
  state: "active",
  web_url: "https://github.com/acme/widget/actions/workflows/release.yml",
} as const;

function enableWorkflowActions(detail: PullDetail): void {
  detail.repo.capabilities = {
    ...detail.repo.capabilities,
    read_workflows: true,
    read_workflow_runs: true,
    workflow_dispatch: true,
  };
  detail.repo.operations = {
    ...detail.repo.operations,
    dispatch_workflow: { available: true },
  };
}

function workflowClient(detail: PullDetail) {
  const repoSettings = {
    AllowSquashMerge: true,
    AllowMergeCommit: false,
    AllowRebaseMerge: false,
    ViewerCanMerge: true,
    operations: detail.repo.operations,
  };
  const GET = vi.fn(
    async (
      path: string,
      _options?: {
        params?: {
          path?: {
            provider?: string;
            platform_host?: string;
            owner?: string;
            name?: string;
          };
        };
      },
    ) => {
      if (path.endsWith("/workflows")) {
        return {
          data: {
            repo: detail.repo,
            environments: [],
            workflows: [releaseWorkflow],
          },
        };
      }
      if (path.endsWith("/runs")) {
        return {
          data: {
            repo: detail.repo,
            items: [],
            exhausted: true,
          },
        };
      }
      return { data: repoSettings };
    },
  );
  const POST = vi.fn(async () => ({ data: { accepted: true, dispatch_id: "dispatch-1" } }));
  return {
    client: { GET, POST } as unknown as GeneratedClient,
    GET,
    POST,
    repoSettings,
  };
}

async function openReleaseWorkflow(detail: PullDetail, options: Parameters<typeof renderPullDetail>[3] = {}) {
  enableWorkflowActions(detail);
  const api = workflowClient(detail);
  const rendered = renderPullDetail(detail, api.repoSettings, api.client, {
    actionsModeVisible: true,
    runtimeClient: api.client,
    ...options,
  });
  await waitFor(() => {
    expect(api.GET.mock.calls.some(([path]) => String(path).endsWith("/workflows"))).toBe(true);
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Run workflow" }));
  const release = await screen.findByRole("button", { name: "Release" });
  await fireEvent.click(release);
  return { ...rendered, api };
}

describe("PullDetail provider workflow actions", () => {
  afterEach(() => {
    cleanup();
  });

  it.each(["open", "merged"] as const)(
    "does not demand or render workflows for a %s pull request when Actions mode is disabled",
    async (state) => {
      const detail = pullDetail();
      detail.merge_request.State = state;
      enableWorkflowActions(detail);
      const api = workflowClient(detail);

      renderPullDetail(detail, api.repoSettings, api.client, {
        actionsModeVisible: false,
        runtimeClient: api.client,
      });
      await tick();

      expect(api.GET.mock.calls.some(([path]) => String(path).includes("/actions/"))).toBe(false);
      expect(screen.queryByRole("button", { name: "Release" })).toBeNull();
      expect(screen.queryByText("GitHub Actions")).toBeNull();
    },
  );

  it.each(["read_workflows", "workflow_dispatch"] as const)(
    "does not demand workflows when %s is unavailable",
    async (capability) => {
      const detail = pullDetail();
      enableWorkflowActions(detail);
      detail.repo.capabilities[capability] = false;
      const api = workflowClient(detail);

      renderPullDetail(detail, api.repoSettings, api.client, {
        actionsModeVisible: true,
        runtimeClient: api.client,
      });
      await tick();

      expect(api.GET.mock.calls.some(([path]) => String(path).includes("/actions/"))).toBe(false);
      expect(screen.queryByText("GitHub Actions")).toBeNull();
    },
  );

  it("demands the exact provider repository and labels its workflow section from provider metadata", async () => {
    const detail = pullDetail();
    enableWorkflowActions(detail);
    detail.repo.provider = "gitlab";
    detail.repo.platform_host = "gitlab.example.com";
    detail.repo.repo_path = "group/widget";
    detail.repo.owner = "group";
    detail.repo.name = "widget";
    const api = workflowClient(detail);

    renderPullDetail(detail, api.repoSettings, api.client, {
      actionsModeVisible: true,
      runtimeClient: api.client,
      detailProps: {
        provider: "gitlab",
        platformHost: "gitlab.example.com",
        owner: "group",
        name: "widget",
        repoPath: "group/widget",
      },
    });

    await waitFor(() => {
      expect(api.GET).toHaveBeenCalledWith(
        "/host/{platform_host}/actions/{provider}/{owner}/{name}/workflows",
        expect.objectContaining({
          params: {
            path: {
              provider: "gitlab",
              platform_host: "gitlab.example.com",
              owner: "group",
              name: "widget",
            },
          },
        }),
      );
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Run workflow" }));

    expect(await screen.findByText("GitLab Actions")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release" })).toBeTruthy();
  });

  it("keeps workflow dispatch available in the phone action grid", async () => {
    const detail = pullDetail();
    enableWorkflowActions(detail);
    const api = workflowClient(detail);

    renderPullDetail(detail, api.repoSettings, api.client, {
      actionsModeVisible: true,
      runtimeClient: api.client,
      phonePresentation: true,
    });

    await waitFor(() => {
      expect(api.GET.mock.calls.some(([path]) => String(path).endsWith("/workflows"))).toBe(true);
    });
    const grid = screen.getByRole("group", { name: "Pull request actions" });
    await fireEvent.click(await within(grid).findByRole("button", { name: "Run workflow" }));

    expect(await screen.findByRole("region", { name: "GitHub Actions" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release" })).toBeTruthy();
  });

  it("opens the shared confirmation without dispatching when a workflow is selected", async () => {
    const detail = pullDetail();
    const { api } = await openReleaseWorkflow(detail);

    expect(screen.getByRole("dialog", { name: "Run workflow" })).toBeTruthy();
    expect(document.querySelector(".actions-menu-popover")).toBeNull();
    expect(api.POST).not.toHaveBeenCalled();
  });

  it.each([
    { state: "open", kind: "same_repo", expected: "feature" },
    { state: "open", kind: "fork", expected: "main" },
    { state: "open", kind: "unknown", expected: "main" },
    { state: "merged", kind: "same_repo", expected: "main" },
    { state: "closed", kind: "same_repo", expected: "main" },
  ] as const)("defaults a $state $kind pull request workflow to $expected", async ({ state, kind, expected }) => {
    const detail = pullDetail();
    detail.merge_request.State = state;
    detail.head_repo_kind = kind;

    await openReleaseWorkflow(detail);

    expect(screen.getByRole<HTMLInputElement>("textbox", { name: "Git ref" }).value).toBe(expected);
  });

  it("keeps the provider workflow ref editable", async () => {
    await openReleaseWorkflow(pullDetail());
    const ref = screen.getByRole<HTMLInputElement>("textbox", { name: "Git ref" });

    await fireEvent.input(ref, { target: { value: "release/2026-08" } });

    expect(ref.value).toBe("release/2026-08");
  });

  it("keeps only the workflow action surface on a merged pull request", async () => {
    const detail = pullDetail();
    detail.merge_request.State = "merged";
    detail.repo.capabilities.review_mutation = true;
    detail.repo.capabilities.merge_mutation = true;
    enableWorkflowActions(detail);
    const api = workflowClient(detail);

    renderPullDetail(detail, api.repoSettings, api.client, {
      actionsModeVisible: true,
      runtimeClient: api.client,
    });
    await waitFor(() => {
      expect(api.GET.mock.calls.some(([path]) => String(path).endsWith("/workflows"))).toBe(true);
    });

    expect(await screen.findByRole("button", { name: "Run workflow" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: /merge/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
  });

  it("routes definition-conflict recovery through a real catalog refresh without replaying POST", async () => {
    const rendered = await openReleaseWorkflow(pullDetail());
    const refreshCatalog = vi.spyOn(rendered.workflowActions, "refreshCatalog");
    rendered.api.POST.mockResolvedValue({
      error: {
        code: "conflict",
        detail: "Workflow definition changed.",
        details: { reason: "workflow_definition_changed" },
        status: 409,
        title: "Conflict",
        type: "about:blank",
      },
      response: new Response(null, { status: 409 }),
    });

    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Run workflow" })).getByRole("button", {
        name: "Run workflow",
      }),
    );
    const reload = await screen.findByRole("button", { name: "Reload workflows" });
    await fireEvent.click(reload);

    expect(refreshCatalog).toHaveBeenCalledWith(
      expect.objectContaining({
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
      }),
      "release.yml",
    );
    expect(rendered.api.POST).toHaveBeenCalledTimes(1);
  });

  it("closes the confirmation without dispatch when Actions mode is disabled", async () => {
    const detail = pullDetail();
    const rendered = await openReleaseWorkflow(detail);

    rendered.settings.setModeVisibility({
      ...rendered.settings.getModeVisibility(),
      actions: false,
    });
    await tick();

    expect(screen.queryByRole("dialog", { name: "Run workflow" })).toBeNull();
    expect(screen.queryByText("GitHub Actions")).toBeNull();
    expect(rendered.api.POST).not.toHaveBeenCalled();

    rendered.settings.setModeVisibility({
      ...rendered.settings.getModeVisibility(),
      actions: true,
    });
    await tick();
    expect(screen.queryByRole("dialog", { name: "Run workflow" })).toBeNull();
  });
});

describe("PullDetail activity refresh", () => {
  afterEach(() => {
    cleanup();
    for (const item of getFlashes()) dismissFlash(item.id);
  });

  it("refreshes the whole selected pull request from the Activity header", async () => {
    const { detailStore } = renderPullDetail(pullDetail());

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    expect(detailStore.syncDetailNow).toHaveBeenCalledWith(
      "acme",
      "widget",
      1,
      {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
      },
      expect.objectContaining({ onFailure: expect.any(Function) }),
    );
  });

  it("tracks manual refresh progress separately and reports pull-request sync failures", async () => {
    renderPullDetail(pullDetail(), undefined, undefined, {
      refreshFailure: "provider unavailable",
    });

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect(getFlashes()).toEqual([expect.objectContaining({ message: "provider unavailable", tone: "danger" })]);

    cleanup();
    const { settleRefresh } = renderPullDetail(pullDetail(), undefined, undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();
    expect(document.querySelector(".sync-indicator")).toBeNull();

    settleRefresh();
    await waitFor(() => {
      expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(false);
    });
  });

  it("does not present a background pull-request sync as a manual refresh", () => {
    renderPullDetail(pullDetail(), undefined, undefined, { detailSyncing: true });

    expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByLabelText("Refreshing detail")).toBeNull();
    expect(document.querySelector(".sync-indicator")?.textContent).toContain("Syncing");
  });

  it("blocks manual refresh while pull-request detail navigation is loading or stale", async () => {
    const loading = renderPullDetail(pullDetail(), undefined, undefined, { detailLoading: true });
    const loadingButton = screen.getByRole("button", { name: "Refresh detail" });

    expect((loadingButton as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(loadingButton);
    expect(loading.detailStore.syncDetailNow).not.toHaveBeenCalled();

    cleanup();
    const navigating = renderPullDetail(pullDetail());
    await navigating.rerender({ number: 2 });
    const staleButton = screen.getByRole("button", { name: "Refresh detail" });

    expect((staleButton as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(staleButton);
    expect(navigating.detailStore.syncDetailNow).not.toHaveBeenCalled();
  });

  it("drops pending manual refresh UI and late failures after a pull-request reroute", async () => {
    const { failRefresh, rerender } = renderPullDetail(pullDetail(), undefined, undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();

    await rerender({ number: 2 });

    await waitFor(() => expect(screen.queryByLabelText("Refreshing detail")).toBeNull());

    failRefresh("old pull request failed");
    expect(getFlashes()).toHaveLength(0);
  });

  it("drops a manual refresh failure after the pull-request detail unmounts", async () => {
    const { failRefresh, unmount } = renderPullDetail(pullDetail(), undefined, undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    unmount();
    failRefresh("unmounted pull request failed");

    expect(getFlashes()).toHaveLength(0);
  });

  it("keeps a pending pull-request refresh across an equivalent route alias", async () => {
    const { failRefresh, rerender } = renderPullDetail(pullDetail(), undefined, undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    await rerender({ provider: "gh", platformHost: undefined });

    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();
    failRefresh("current pull request failed");
    expect(getFlashes()).toEqual([expect.objectContaining({ message: "current pull request failed" })]);
  });

  it("preserves a real pull-request store refresh across an equivalent route alias", async () => {
    const cached = pullDetail();
    cached.merge_request.Title = "Cached pull request detail";
    const fresh = pullDetail();
    fresh.merge_request.Title = "Fresh pull request detail";
    const syncResponse = Promise.withResolvers<{ data: PullDetail; error: undefined }>();
    const get = vi.fn(async (path: string) =>
      path.startsWith("/pulls/")
        ? { data: cached, error: undefined }
        : {
            data: {
              AllowSquashMerge: false,
              AllowMergeCommit: false,
              AllowRebaseMerge: false,
              ViewerCanMerge: true,
            },
            error: undefined,
          },
    );
    const post = vi.fn(() => syncResponse.promise);
    detailRuntime = makeTestAppRuntime({ GET: get, POST: post } as unknown as GeneratedClient);
    const detailStore = createDetailStore({ runtime: detailRuntime });
    let detailProps: ComponentProps<typeof PullDetailComponent> = {
      owner: "acme",
      name: "widget",
      number: cached.merge_request.Number,
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      hideWorkspaceAction: true,
      autoSync: false,
    };
    const rendered = render(PullDetailTestHarness, {
      props: { runtime: detailRuntime, detailProps },
      context: new Map<symbol, unknown>([
        [
          STORES_KEY,
          {
            detail: detailStore,
            pulls: { loadPulls: vi.fn() },
            activity: { loadActivity: vi.fn() },
            detailActivityView: createDetailActivityViewStore(),
            settings: {
              getLaunchTargets: () => launchTargets,
              getDetailSettings: () => ({ initial_timeline_entry_limit: 250 }),
              isModeVisible: () => false,
            },
          },
        ],
        [ACTIONS_KEY, { pull: [] }],
        [UI_CONFIG_KEY, { hideStar: true }],
        [NAVIGATE_KEY, vi.fn()],
      ]),
    });
    const detailGets = () => get.mock.calls.filter(([path]) => path.startsWith("/pulls/"));
    await waitFor(() => expect(detailGets()).toHaveLength(1));
    await waitFor(() => expect(detailStore.isDetailLoading()).toBe(false));

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    await waitFor(() => expect(detailStore.isDetailSyncing()).toBe(true));
    detailProps = { ...detailProps, provider: "gh", platformHost: undefined };
    await rendered.rerender({ runtime: detailRuntime, detailProps });
    syncResponse.resolve({ data: fresh, error: undefined });

    await waitFor(() => expect(screen.queryByLabelText("Refreshing detail")).toBeNull());
    expect(detailGets()).toHaveLength(1);
    expect(detailStore.getDetail()?.merge_request.Title).toBe("Fresh pull request detail");
  });
});

describe("PullDetail approvals", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    markdownMockState.pending = false;
    markdownMockState.pendingPromise = new Promise<string>(() => undefined);
    cleanup();
    for (const item of getFlashes()) dismissFlash(item.id);
    vi.useRealTimers();
  });
  it("uses the standard syncing indicator without duplicate progress UI", () => {
    renderPullDetail(pullDetail(), undefined, undefined, {
      detailSyncing: true,
    });

    expect(document.querySelector(".sync-indicator")?.textContent).toContain("Syncing");
    expect(document.querySelector(".refresh-banner")).toBeNull();
    expect(screen.queryByText("Refreshing...")).toBeNull();
  });

  it("keeps task checkboxes disabled while highlighted markdown is pending", () => {
    markdownMockState.pending = true;
    const detail = pullDetail();
    detail.merge_request.Body = ["- [ ] pending task", "", "```toml", 'model_provider = "my-custom"', "```"].join("\n");

    const { container } = renderPullDetail(detail);

    const checkbox = container.querySelector<HTMLInputElement>(".markdown-body input[type='checkbox']");
    expect(checkbox).not.toBeNull();
    expect(checkbox?.disabled).toBe(true);
    expect(checkbox?.dataset.taskIndex).toBeUndefined();
  });

  it("explains that creating a workspace enables agent sessions", () => {
    renderPullDetail(pullDetail(), undefined, undefined, { hideWorkspaceAction: false });

    const buttons = screen.getAllByRole("button", { name: "Create Workspace" });
    expect(buttons.length).toBeGreaterThan(0);
    for (const button of buttons) {
      expect(button.getAttribute("aria-describedby")).toBe("pull-create-workspace-description");
      expect(button.getAttribute("title")).toContain("PR head worktree");
      expect(button.getAttribute("title")).toContain("launch agents");
      expect(button.getAttribute("title")).toContain("local review sessions");
      expect(document.getElementById("pull-create-workspace-description")?.textContent).toContain(
        button.getAttribute("title"),
      );
    }
  });

  it("forwards worktree link host key to navigate actions", async () => {
    const detail = pullDetail();
    detail.worktree_links = [
      {
        host_key: "hub",
        worktree_key: "worktree:/srv/widget-feature",
        worktree_path: "/srv/widget-feature",
        worktree_branch: "feature",
      },
    ];
    const navigate = vi.fn();

    renderPullDetail(detail, undefined, undefined, {
      actions: {
        pull: [
          {
            id: "navigate-worktree",
            label: "Open Worktree",
            handler: navigate,
          },
        ],
      },
    });

    await fireEvent.click(
      screen.getByRole("button", {
        name: "Open Worktree: worktree:/srv/widget-feature",
      }),
    );

    expect(navigate).toHaveBeenCalledWith({
      surface: "pull-detail",
      owner: "acme",
      name: "widget",
      number: 1,
      meta: {
        host_key: "hub",
        worktree_key: "worktree:/srv/widget-feature",
      },
    });
  });

  it("normalizes backend review decision casing before enabling approver popup", async () => {
    const detail = pullDetail();
    detail.merge_request.ReviewDecision = "approved";

    renderPullDetail(detail);

    const trigger = screen.getByRole("button", {
      name: "APPROVED (2)",
    });
    await fireEvent.click(trigger);

    const popup = document.querySelector(".approval-popup");
    expect(popup?.textContent).toContain("alice");
    expect(popup?.textContent).toContain("bob");
  });

  it("auto-refreshes pending CI checks while the CI panel is expanded", async () => {
    vi.useFakeTimers();
    const detail = pullDetail();
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/build",
        app: "GitHub Actions",
      },
    ]);

    const { detailStore } = renderPullDetail(detail);

    expect(detailStore.refreshPendingCI).not.toHaveBeenCalled();

    await fireEvent.click(
      screen.getByRole("button", {
        name: /CI:\s*1\s*pending\s*check/i,
      }),
    );

    expect(detailStore.refreshPendingCI).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(15_000);

    expect(detailStore.refreshPendingCI).toHaveBeenCalledTimes(2);
    expect(detailStore.refreshPendingCI).toHaveBeenCalledWith("acme", "widget", 1, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      workflowApprovalSync: true,
    });
  });

  it("stops pending CI refreshes when the detail unmounts", async () => {
    vi.useFakeTimers();
    const detail = pullDetail();
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/build",
        app: "GitHub Actions",
      },
    ]);
    const view = renderPullDetail(detail);
    await fireEvent.click(screen.getByRole("button", { name: /CI:\s*1\s*pending\s*check/i }));
    expect(view.detailStore.refreshPendingCI).toHaveBeenCalledTimes(1);

    view.unmount();
    await vi.advanceTimersByTimeAsync(30_000);

    expect(view.detailStore.refreshPendingCI).toHaveBeenCalledTimes(1);
  });

  it("cancels pending conversation scroll restoration when the detail unmounts", async () => {
    const callbacks: FrameRequestCallback[] = [];
    const cancelFrame = vi.fn();
    vi.stubGlobal(
      "requestAnimationFrame",
      vi.fn((callback: FrameRequestCallback) => {
        callbacks.push(callback);
        return callbacks.length;
      }),
    );
    vi.stubGlobal("cancelAnimationFrame", cancelFrame);
    const view = renderPullDetail(pullDetail());
    await waitFor(() => expect(callbacks.length).toBeGreaterThan(0));
    const viewport = view.container.querySelector<HTMLDivElement>(".kit-scrollbox__viewport");
    expect(viewport).not.toBeNull();
    let scrollWrites = 0;
    Object.defineProperty(viewport, "scrollTop", {
      configurable: true,
      get: () => 42,
      set: () => {
        scrollWrites += 1;
      },
    });

    view.unmount();
    for (const callback of callbacks) callback(performance.now());
    await Promise.resolve();
    await Promise.resolve();

    expect(cancelFrame).toHaveBeenCalled();
    expect(scrollWrites).toBe(0);
  });

  it("uses one shared expanded slot for CI and stack status", async () => {
    const detail = pullDetail();
    detail.merge_request.Number = 2;
    detail.merge_request.MergeableState = "dirty";
    detail.stack = {
      stack_id: 1,
      stack_name: "session-recovery",
      position: 2,
      size: 3,
      health: "blocked",
      members: [
        {
          number: 1,
          title: "base schema",
          state: "open",
          ci_status: "failure",
          review_decision: "APPROVED",
          position: 1,
          is_draft: false,
          base_branch: "main",
          blocked_by: null,
        },
        {
          number: 2,
          title: "session storage",
          state: "open",
          ci_status: "pending",
          review_decision: "APPROVED",
          position: 2,
          is_draft: false,
          base_branch: "feat/base-schema",
          blocked_by: 1,
        },
        {
          number: 3,
          title: "UI polish",
          state: "open",
          ci_status: "success",
          review_decision: "",
          position: 3,
          is_draft: false,
          base_branch: "feat/session-storage",
          blocked_by: 1,
        },
      ],
    };
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "frontend / vp check",
        status: "completed",
        conclusion: "failure",
        url: "https://example.com/frontend",
        app: "GitHub Actions",
      },
      {
        name: "e2e / chromium",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/e2e",
        app: "GitHub Actions",
      },
    ]);

    const apiClient = {
      GET: vi.fn(async () => {
        return {
          data: {
            AllowSquashMerge: false,
            AllowMergeCommit: false,
            AllowRebaseMerge: false,
            ViewerCanMerge: true,
          },
        };
      }),
    };

    renderPullDetail(detail, undefined, apiClient);

    await fireEvent.click(
      screen.getByRole("button", {
        name: /CI:\s*1 failed check,\s*1 pending check/i,
      }),
    );

    expect(screen.getByText("frontend / vp check")).toBeTruthy();

    await fireEvent.click(
      await screen.findByRole("button", {
        name: /Stacked: 2\/3, 1 downstack CI failure/i,
      }),
    );

    expect(screen.queryByText("frontend / vp check")).toBeNull();
    expect(screen.getByText("3 PRs · current 2/3 · downstack CI failure")).toBeTruthy();
    expect(document.querySelector(".stack-row--current .stack-dot--current")).toBeTruthy();
    expect(screen.getByText("blocked by #1")).toBeTruthy();

    const stackLinks = Array.from(document.querySelectorAll<HTMLButtonElement>(".stack-member-link")).map((button) =>
      button.textContent?.trim(),
    );
    expect(stackLinks).toEqual(["#3 UI polish", "#2 session storage", "#1 base schema"]);
    expect(document.querySelector(".stack-base-name")?.textContent?.trim()).toBe("main");

    await fireEvent.click(screen.getByTestId("merge-warnings-chip"));

    expect(screen.queryByText("3 PRs · current 2/3 · downstack CI failure")).toBeNull();
    expect(screen.getByText("This branch has conflicts that must be resolved before merging.")).toBeTruthy();
  });

  it("does not probe stack context for unstacked pull details", () => {
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: false,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
    };

    renderPullDetail(pullDetail(), undefined, apiClient);

    const paths = apiClient.GET.mock.calls.map(([path]) => String(path));
    expect(paths.some((path) => path.endsWith("/stack"))).toBe(false);
  });

  it("closes the label picker when the labels action is clicked twice", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };

    renderPullDetail(detail);

    const labelsAction = screen.getByRole("button", { name: "Labels" });
    await fireEvent.click(labelsAction);

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();

    await fireEvent.click(labelsAction);

    expect(screen.queryByRole("dialog", { name: "Edit labels" })).toBeNull();
  });

  it("loads the label catalog through the app runtime", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };
    const repoSettings = {
      AllowSquashMerge: false,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    };
    const contextClient = {
      GET: vi.fn(async (path: string) =>
        path.endsWith("/labels")
          ? { data: { labels: [{ name: "context-label", color: "ededed", description: "" }] } }
          : { data: repoSettings },
      ),
      POST: vi.fn(async () => ({ data: {} })),
    };
    const runtimeClient = {
      GET: vi.fn(async (path: string) =>
        path.endsWith("/labels")
          ? { data: { labels: [{ name: "runtime-label", color: "ededed", description: "" }] } }
          : { data: repoSettings },
      ),
    } as unknown as GeneratedClient;

    renderPullDetail(detail, repoSettings, contextClient, { runtimeClient });
    await fireEvent.click(screen.getByRole("button", { name: "Labels" }));

    expect(await screen.findByText("runtime-label")).toBeTruthy();
    expect(screen.queryByText("context-label")).toBeNull();
  });

  it("closes and invalidates the label catalog when the pull route changes", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };
    const oldCatalog = Promise.withResolvers<{ data: { labels: Label[] } }>();
    const apiClient = {
      GET: vi.fn((path: string) =>
        path.endsWith("/labels")
          ? oldCatalog.promise
          : Promise.resolve({
              data: {
                AllowSquashMerge: false,
                AllowMergeCommit: false,
                AllowRebaseMerge: false,
                ViewerCanMerge: true,
              },
            }),
      ),
      POST: vi.fn(async () => ({ data: {} })),
    };
    const { rerender } = renderPullDetail(detail, undefined, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Labels" }));
    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();

    await rerender({ name: "other", repoPath: "acme/other", number: 2 });
    expect(screen.queryByRole("dialog", { name: "Edit labels" })).toBeNull();

    oldCatalog.resolve({ data: { labels: [{ name: "old-route", color: "ededed", description: "" }] } });
    await oldCatalog.promise;
    expect(screen.queryByText("old-route")).toBeNull();
  });

  it("toggles the label picker from the visible Labels action", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };

    renderPullDetail(detail);

    const labelsTrigger = screen.getByRole("button", {
      name: "Labels",
    });
    await fireEvent.click(labelsTrigger);

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    expect(document.querySelector(".actions-menu-popover")).toBeNull();

    await fireEvent.mouseDown(labelsTrigger);
    await fireEvent.click(labelsTrigger);

    expect(screen.queryByRole("dialog", { name: "Edit labels" })).toBeNull();
  });

  it("opens the visible Labels action as a non-modal popover", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };

    renderPullDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "Labels" }));

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    expect(document.querySelector(".label-editor-backdrop")).toBeNull();

    await fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog", { name: "Edit labels" })).toBeNull();
  });

  it("keeps the visible Labels action on small button geometry", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      read_labels: true,
      label_mutation: true,
    };

    renderPullDetail(detail);

    const labelsAction = screen.getByRole("button", { name: "Labels" });
    const labelsIcon = labelsAction.querySelector("svg");

    expect(labelsAction.classList.contains("kit-button--sm")).toBe(true);
    expect(labelsIcon?.getAttribute("width")).toBe("16");
    expect(labelsIcon?.getAttribute("height")).toBe("16");
  });

  it("uses the shared View menu to persist compact activity rows", async () => {
    const detail = pullDetail();
    detail.events = [
      {
        ID: 30,
        MergeRequestID: 1,
        PlatformID: 30,
        PlatformExternalID: "",
        EventType: "issue_comment",
        Author: "alice",
        Summary: "",
        Body: "Compact **activity** preview",
        MetadataJSON: "",
        CreatedAt: "2026-05-01T12:03:00Z",
        DedupeKey: "comment-30",
        DirectURL: "",
        ThreadID: null,
        Resolvable: false,
        Resolved: false,
      },
    ];
    const { container } = renderPullDetail(detail);

    expect(screen.queryByRole("button", { name: /filters/i })).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: /view/i }));
    await fireEvent.click(screen.getByRole("button", { name: /compact/i }));

    expect(localStorage.getItem("kenn-forge-detail-activity-view")).toBe("compact");
    expect(container.querySelectorAll(".event-card--compact-row")).toHaveLength(1);
    expect(container.textContent).toContain("Compact activity preview");
  });

  it("does not describe GitHub unstable mergeability as required checks", () => {
    const detail = pullDetail();
    detail.merge_request.MergeableState = "unstable";
    detail.merge_request.CIStatus = "failure";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "e2e",
        status: "completed",
        conclusion: "failure",
        url: "https://example.com/e2e",
        app: "GitHub Actions",
      },
    ]);

    renderPullDetail(detail);

    expect(screen.queryByTestId("merge-warnings-chip")).toBeNull();
  });

  it("shows required CI and branch freshness warnings behind the merge warnings chip", async () => {
    const detail = pullDetail();
    detail.merge_request.MergeableState = "behind";
    detail.merge_request.CIStatus = "failure";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "completed",
        conclusion: "failure",
        url: "https://example.com/build",
        app: "GitHub Actions",
        required: true,
      },
    ]);

    renderPullDetail(detail);

    const chip = screen.getByTestId("merge-warnings-chip");
    expect(chip.textContent).toContain("2 merge warnings");
    expect(screen.queryByText("Required status checks have not passed.")).toBeNull();

    await fireEvent.click(chip);

    expect(screen.getByText("Required status checks have not passed.")).toBeTruthy();
    expect(screen.getByText("This branch is behind the base branch and may need to be updated.")).toBeTruthy();
  });

  it("labels the chip Conflicts and links to the provider when the branch is dirty", async () => {
    const detail = pullDetail();
    detail.merge_request.MergeableState = "dirty";

    renderPullDetail(detail);

    const chip = screen.getByTestId("merge-warnings-chip");
    expect(chip.textContent).toContain("Conflicts");

    await fireEvent.click(chip);

    expect(screen.getByText("This branch has conflicts that must be resolved before merging.")).toBeTruthy();
    const link = screen.getByRole("link", { name: "View on GitHub" });
    expect(link.getAttribute("href")).toBe(detail.merge_request.URL);
  });

  it("surfaces branch protection warnings through the merge warnings chip", async () => {
    const detail = pullDetail();
    detail.merge_request.MergeableState = "blocked";

    renderPullDetail(detail);

    const chip = screen.getByTestId("merge-warnings-chip");
    expect(chip.textContent).toContain("1 merge warning");

    await fireEvent.click(chip);

    expect(screen.getByText("Branch protection rules may prevent this merge.")).toBeTruthy();
  });

  it("shows only server warnings on the chip when the detail is stale", async () => {
    const detail = pullDetail();
    detail.repo_owner = "someone-else";
    detail.merge_request.MergeableState = "dirty";
    detail.warnings = ["Example sync warning"];

    renderPullDetail(detail);

    const chip = screen.getByTestId("merge-warnings-chip");
    expect(chip.textContent).toContain("1 merge warning");

    await fireEvent.click(chip);

    expect(screen.getByText("Example sync warning")).toBeTruthy();
    expect(screen.queryByText("This branch has conflicts that must be resolved before merging.")).toBeNull();
  });

  it("does not render the merge button when repo permissions disallow merging", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: true,
      AllowRebaseMerge: true,
      ViewerCanMerge: false,
    });

    await vi.waitFor(() => {
      expect(screen.queryByRole("button", { name: /merge/i })).toBeNull();
    });
  });

  it("renders the merge button as disabled with reason when operations.merge_pr.available is false", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    const retryAt = "2026-05-19T14:35:00Z";
    const localRetryTime = new Date(retryAt).toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
      operations: {
        merge_pr: {
          available: false,
          code: "rate_limited",
          unavailable_reason: "github.com rate-limited",
          retry_at: retryAt,
        },
      },
    });

    const button = await vi.waitFor(() => {
      const found = screen.queryByRole("button", { name: /merge/i });
      expect(found).not.toBeNull();
      return found as HTMLButtonElement;
    });
    expect(button.disabled).toBe(true);
    expect(button.title).toBe(`github.com rate-limited; retry at ${localRetryTime}`);
  });

  it("disables ready-for-review with reason when its operation is unavailable", async () => {
    // A GitHub App split host with no user write credential reports
    // missing_write_credential on every mutation; non-merge actions
    // must disable with the reason instead of failing at request time.
    const detail = pullDetail();
    detail.repo.capabilities.ready_for_review = true;
    detail.merge_request.IsDraft = true;

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
      operations: {
        mark_ready_for_review: {
          available: false,
          code: "missing_write_credential",
          unavailable_reason: "No user credential for writes on github.com",
        },
      },
    });

    const button = await vi.waitFor(() => {
      const found = screen.queryByRole("button", { name: /ready for review/i });
      expect(found).not.toBeNull();
      expect((found as HTMLButtonElement).disabled).toBe(true);
      return found as HTMLButtonElement;
    });
    expect(button.disabled).toBe(true);
    expect(button.title).toBe("No user credential for writes on github.com");
  });

  it("opens a state menu from the open chip and marks a pull request as draft", async () => {
    const detail = pullDetail();
    detail.repo.capabilities = {
      ...capabilities,
      draft_mutation: true,
    } as PullDetail["repo"]["capabilities"];
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: false,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn(async () => ({ data: { state: "draft" } })),
    };

    const { detailStore } = renderPullDetail(detail, undefined, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "State: Open" }));
    await fireEvent.click(screen.getByRole("menuitem", { name: "Draft" }));

    expect(detailStore.setPullState).toHaveBeenCalledWith(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      1,
      "draft",
      expect.objectContaining({ onSuccess: expect.any(Function), onSettled: expect.any(Function) }),
    );
  });

  it("gates actions from the detail payload before repo settings resolve", async () => {
    // The detail payload carries repo.operations as the primary
    // source; the separate /repo settings request is only a fallback.
    // Here the settings response has no operations at all, so only a
    // payload-sourced gate can disable the button.
    const detail = pullDetail();
    detail.repo.capabilities.ready_for_review = true;
    detail.merge_request.IsDraft = true;
    detail.repo.operations = {
      mark_ready_for_review: {
        available: false,
        code: "missing_write_credential",
        unavailable_reason: "No user credential for writes on github.com",
      },
    } as unknown as PullDetail["repo"]["operations"];

    renderPullDetail(detail);

    const button = await vi.waitFor(() => {
      const found = screen.queryByRole("button", { name: /ready for review/i });
      expect(found).not.toBeNull();
      return found as HTMLButtonElement;
    });
    expect(button.disabled).toBe(true);
    expect(button.title).toBe("No user credential for writes on github.com");
  });

  it("hides review suggestion apply actions when the pull request is not open", async () => {
    const detail = pullDetail();
    addReviewSuggestionToDetail(detail);
    detail.merge_request.State = "closed";

    renderPullDetail(detail);

    expect(screen.getByText("This can return directly.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Commit suggestion" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add suggestion to batch" })).toBeNull();
  });

  it("uses the controlled detail tab when a focused review thread jumps to its diff", async () => {
    const detail = pullDetail();
    addReviewSuggestionToDetail(detail);
    const onDetailTabChange = vi.fn();
    const { navigate } = renderPullDetail(detail, undefined, undefined, {
      hideTabs: true,
      diff: makeReviewSuggestionDiffStore(),
      diffReviewDraft: {
        setRouteContext: vi.fn(),
        isSubmitting: () => false,
      },
      onDetailTabChange,
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Jump to diff" }));

    expect(onDetailTabChange).toHaveBeenCalledWith("files");
    expect(navigate).not.toHaveBeenCalled();
  });

  it("routes a failed suggestion refresh through shared conflict recovery", async () => {
    const detail = pullDetail();
    addReviewSuggestionToDetail(detail);
    const repoSettings = {
      AllowSquashMerge: false,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
      operations: detail.repo.operations,
    };
    const apiClient = {
      GET: vi.fn(async (path: string) => {
        if (path.includes("/repos/")) {
          return { data: repoSettings };
        }
        return { data: detail };
      }),
      POST: vi.fn(async (path: string) => {
        if (path.endsWith("/review-suggestions/apply")) {
          return {
            error: {
              code: "conflict",
              type: "about:blank",
              detail: "target changed since it was reviewed; refresh and retry",
              details: { reason: "stale_state" },
            },
          };
        }
        if (path.endsWith("/sync")) {
          return {
            error: {
              code: "sync_failed",
              type: "about:blank",
              detail: "provider refresh failed",
            },
          };
        }
        return { data: {} };
      }),
    };
    detailRuntime = makeTestAppRuntime(apiClient as unknown as GeneratedClient);
    const detailStore = createDetailStore({ runtime: detailRuntime });
    detailStore.loadDetail("acme", "widget", 1, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    await waitFor(() => expect(detailStore.isDetailLoading()).toBe(false));

    render(PullDetailTestHarness, {
      props: {
        runtime: detailRuntime,
        detailProps: {
          owner: "acme",
          name: "widget",
          number: 1,
          provider: "github",
          platformHost: "github.com",
          repoPath: "acme/widget",
          hideWorkspaceAction: true,
          // Background sync would re-fetch the original fixture and race
          // the fail-closed state this test asserts.
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
            diff: makeReviewSuggestionDiffStore(),
            diffReviewDraft: {
              setRouteContext: vi.fn(),
              isSubmitting: () => false,
            },
            detailActivityView: createDetailActivityViewStore(),
            settings: {
              getLaunchTargets: () => [],
              getDetailSettings: () => ({ initial_timeline_entry_limit: 250 }),
              isModeVisible: () => false,
            },
          },
        ],
        [ACTIONS_KEY, { pull: [] }],
        [UI_CONFIG_KEY, { hideStar: true }],
        [NAVIGATE_KEY, vi.fn()],
      ]),
    });

    const commitButton = await vi.waitFor(() => {
      const found = screen.getByRole("button", { name: "Commit suggestion" }) as HTMLButtonElement;
      expect(found.disabled).toBe(false);
      return found;
    });
    await fireEvent.click(commitButton);

    await vi.waitFor(() => {
      expect(screen.queryByRole("button", { name: "Commit suggestion" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Add suggestion to batch" })).toBeNull();
      expect(screen.getByText(/head commit changed since this pull request was reviewed/i)).toBeTruthy();
      expect(screen.getByRole("alert").textContent).toContain("Could not refresh the pull request");
    });
  });

  it("disables approve with reason when submit_review is unavailable", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.review_mutation = true;

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
      operations: {
        submit_review: {
          available: false,
          code: "missing_write_credential",
          unavailable_reason: "No user credential for writes on github.com",
        },
      },
    });

    const button = await vi.waitFor(() => {
      const found = screen.queryByRole("button", { name: /^approve$/i });
      expect(found).not.toBeNull();
      expect((found as HTMLButtonElement).disabled).toBe(true);
      return found as HTMLButtonElement;
    });
    expect(button.disabled).toBe(true);
    expect(button.title).toBe("No user credential for writes on github.com");
  });

  it("opens the merge modal in deferred mode when CI is still pending", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/build",
        app: "GitHub Actions",
      },
    ]);

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));

    expect(screen.getByRole("dialog", { name: "Merge Pull Request" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Merge after CI is complete" })).toBeTruthy();
  });

  it("does not offer a merge action when no merge method is enabled", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.repo.operations = {
      merge_pr: { available: true },
    } as unknown as PullDetail["repo"]["operations"];

    const settings = {
      AllowSquashMerge: false,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    };
    const apiClient = {
      GET: vi.fn(async () => ({ data: settings })),
      POST: vi.fn(async () => ({ data: {} })),
    };
    renderPullDetail(detail, settings, apiClient);

    await waitFor(() => expect(apiClient.GET).toHaveBeenCalled());
    await tick();
    expect(screen.queryByRole("button", { name: /^merge$/i })).toBeNull();
  });

  it("warns after a successful merge when workspace cleanup fails", async () => {
    const notifyWorkspaceDeleted = vi.spyOn(workspaceHost, "notifyWorkspaceDeleted");
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.workspace = { id: "ws-1", status: "ready" };
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn(async () => ({
        data: {
          merged: true,
          sha: "merge-sha",
          message: "merged",
          workspace_cleanup_warning: "workspace has uncommitted changes",
        },
        error: undefined,
      })),
    };
    const { detailStore } = renderPullDetail(
      detail,
      {
        AllowSquashMerge: true,
        AllowMergeCommit: false,
        AllowRebaseMerge: false,
        ViewerCanMerge: true,
      },
      apiClient,
    );

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));
    expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Delete workspace after merge" }).checked).toBe(true);
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Merge Pull Request" })).getByRole("button", {
        name: "Squash and merge",
      }),
    );

    await waitFor(() => {
      expect(getFlashes()).toContainEqual(
        expect.objectContaining({
          message: expect.stringMatching(/merged.*workspace was not pruned.*uncommitted changes/i),
          tone: "warning",
        }),
      );
      expect(detailStore.loadDetail).toHaveBeenCalled();
    });
    expect(apiClient.POST.mock.calls.at(-1)?.[1]).toMatchObject({ body: { delete_workspace_id: "ws-1" } });
    expect(notifyWorkspaceDeleted).not.toHaveBeenCalled();
    notifyWorkspaceDeleted.mockRestore();
  });

  it("waits for the workspace deletion event instead of treating merge success as deletion", async () => {
    const notifyWorkspaceDeleted = vi.spyOn(workspaceHost, "notifyWorkspaceDeleted");
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.workspace = { id: "ws-1", status: "ready" };
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn(async () => ({
        data: {
          merged: true,
          sha: "merge-sha",
          message: "merged",
        },
        error: undefined,
      })),
    };
    renderPullDetail(
      detail,
      {
        AllowSquashMerge: true,
        AllowMergeCommit: false,
        AllowRebaseMerge: false,
        ViewerCanMerge: true,
      },
      apiClient,
    );

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Merge Pull Request" })).getByRole("button", {
        name: "Squash and merge",
      }),
    );

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Merge Pull Request" })).toBeNull());
    expect(notifyWorkspaceDeleted).not.toHaveBeenCalled();
    notifyWorkspaceDeleted.mockRestore();
  });

  it("keeps an in-flight merge modal mounted across transient eligibility refreshes", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    let resolveMerge!: (value: unknown) => void;
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn(
        () =>
          new Promise((resolve) => {
            resolveMerge = resolve;
          }),
      ),
    };
    const { rerender } = renderPullDetail(
      detail,
      {
        AllowSquashMerge: true,
        AllowMergeCommit: false,
        AllowRebaseMerge: false,
        ViewerCanMerge: true,
      },
      apiClient,
    );

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Merge Pull Request" })).getByRole("button", {
        name: "Squash and merge",
      }),
    );
    expect(screen.getByRole("button", { name: "Merging..." })).toBeTruthy();

    detail.repo.capabilities.merge_mutation = false;
    await rerender({ hideWorkspaceAction: false });
    expect(screen.getByRole("dialog", { name: "Merge Pull Request" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Merging..." })).toBeTruthy();

    resolveMerge({ data: { merged: true, sha: "merge-sha", message: "merged" } });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Merge Pull Request" })).toBeNull());
  });

  it("opens the merge modal in deferred mode when aggregate CI is pending without check rows", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = "";

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));

    expect(screen.getByRole("dialog", { name: "Merge Pull Request" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Merge after CI is complete" })).toBeTruthy();
  });

  it("shows the queued merge state and still allows an immediate merge while a deferred merge waits", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.deferred_merge_pending = true;
    detail.merge_request.CIStatus = "pending";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/build",
        app: "GitHub Actions",
      },
    ]);

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    });

    const queued = await vi.waitFor(() => {
      const found = screen.queryByRole("button", { name: "Merge queued" });
      expect(found).not.toBeNull();
      return found as HTMLButtonElement;
    });
    expect(queued.disabled).toBe(false);
    expect(queued.title).toContain("Click to merge immediately");

    // Clicking the queued action reopens the merge modal so the user can
    // force an immediate merge — but it must not offer queueing a second
    // deferred merge, which the server would reject.
    await fireEvent.click(queued);
    expect(screen.getByRole("dialog", { name: "Merge Pull Request" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Merge after CI is complete" })).toBeNull();
    expect(screen.getByRole("button", { name: "Squash and merge" })).toBeTruthy();
  });

  it("opens the merge modal in normal mode when aggregate CI has already failed", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.merge_request.CIStatus = "failure";
    detail.merge_request.CIChecksJSON = JSON.stringify([
      {
        name: "build",
        status: "completed",
        conclusion: "failure",
        url: "https://example.com/build",
        app: "GitHub Actions",
      },
      {
        name: "integration",
        status: "in_progress",
        conclusion: "",
        url: "https://example.com/integration",
        app: "GitHub Actions",
      },
    ]);

    renderPullDetail(detail, {
      AllowSquashMerge: true,
      AllowMergeCommit: false,
      AllowRebaseMerge: false,
      ViewerCanMerge: true,
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));

    expect(screen.getByRole("dialog", { name: "Merge Pull Request" })).toBeTruthy();
    // A failed aggregate with a still-running check must not route to deferred
    // merge, since the backend would reject that with a 409.
    expect(screen.queryByRole("button", { name: "Merge after CI is complete" })).toBeNull();
  });

  it("keeps a newer head conflict after an overlapping approval succeeds", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.repo.capabilities.review_mutation = true;
    let resolveApprove!: (value: unknown) => void;
    const approveRequest = new Promise((resolve) => {
      resolveApprove = resolve;
    });
    let resolveMerge!: (value: unknown) => void;
    const mergeRequest = new Promise((resolve) => {
      resolveMerge = resolve;
    });
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn((path: string) => {
        if (path.endsWith("/approve")) return approveRequest;
        if (path.endsWith("/merge")) return mergeRequest;
        return Promise.resolve({ data: {} });
      }),
    };
    const { detailStore } = renderPullDetail(
      detail,
      {
        AllowSquashMerge: true,
        AllowMergeCommit: false,
        AllowRebaseMerge: false,
        ViewerCanMerge: true,
      },
      apiClient,
    );

    await fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Submit pull request review" })).getByRole("button", {
        name: "Approve",
      }),
    );
    await vi.waitFor(() => {
      expect(apiClient.POST.mock.calls.some(([path]) => String(path).endsWith("/approve"))).toBe(true);
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Merge Pull Request" })).getByRole("button", {
        name: "Squash and merge",
      }),
    );

    resolveMerge({
      error: {
        code: "conflict",
        type: "about:blank",
        status: 409,
        detail: "head changed",
        details: { reason: "stale_state" },
      },
    });
    await vi.waitFor(() => {
      expect(screen.getByText(/head commit changed since this pull request was reviewed/i)).toBeTruthy();
    });

    resolveApprove({
      data: { status: "approved" },
      error: undefined,
      response: new Response("{}"),
    });
    await vi.waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Submit pull request review" })).toBeNull();
    });

    expect(screen.getByText(/head commit changed since this pull request was reviewed/i)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Approve" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("ignores a delayed merge conflict after an A-to-B-to-A route cycle", async () => {
    const detail = pullDetail();
    detail.repo.capabilities.merge_mutation = true;
    detail.repo.capabilities.review_mutation = true;
    let resolveMerge: ((value: unknown) => void) | undefined;
    const apiClient = {
      GET: vi.fn(async () => ({
        data: {
          AllowSquashMerge: true,
          AllowMergeCommit: false,
          AllowRebaseMerge: false,
          ViewerCanMerge: true,
        },
      })),
      POST: vi.fn(
        () =>
          new Promise((resolve) => {
            resolveMerge = resolve;
          }),
      ),
    };
    const { rerender, detailStore } = renderPullDetail(
      detail,
      {
        AllowSquashMerge: true,
        AllowMergeCommit: false,
        AllowRebaseMerge: false,
        ViewerCanMerge: true,
      },
      apiClient,
    );

    await fireEvent.click(await screen.findByRole("button", { name: "Squash and merge" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Merge Pull Request" })).getByRole("button", {
        name: "Squash and merge",
      }),
    );
    expect((screen.getByRole("button", { name: "Approve" }) as HTMLButtonElement).disabled).toBe(false);
    await rerender({
      owner: "acme",
      name: "other-widget",
      number: 2,
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      repoPath: "acme/other-widget",
      hideWorkspaceAction: true,
    });
    await rerender({
      owner: "acme",
      name: "widget",
      number: 1,
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      hideWorkspaceAction: true,
    });
    resolveMerge?.({
      error: {
        status: 409,
        detail: "head changed",
        details: { reason: "stale_state" },
      },
    });

    await vi.waitFor(() => expect(apiClient.POST).toHaveBeenCalledTimes(1));
    expect(detailStore.syncDetailNow).not.toHaveBeenCalled();
    expect(screen.queryByText(/head commit changed since/i)).toBeNull();
  });
});

describe("PullDetail inline workspace handoff", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    for (const item of getFlashes()) dismissFlash(item.id);
  });

  const identity: WorkspaceItemIdentity = {
    provider: "github",
    platformHost: "github.com",
    owner: "acme",
    name: "widget",
    repoPath: "acme/widget",
    number: 1,
    itemType: "pull",
  };

  function deferredWorkspaceApiClient() {
    let resolvePost!: (value: { data?: { id: string; status: string; created?: boolean } }) => void;
    const postPromise = new Promise<{ data?: { id: string; status: string; created?: boolean } }>((resolve) => {
      resolvePost = resolve;
    });
    const apiClient = {
      GET: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => postPromise),
    };
    return { apiClient, resolvePost };
  }

  it("creates a pull workspace through the app runtime", async () => {
    const controller = createTestController("split");
    const { apiClient: runtimeClient, resolvePost } = deferredWorkspaceApiClient();
    const contextClient = {
      GET: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => ({ error: { title: "legacy client used" } })),
    };
    renderPullDetail(pullDetail(), undefined, contextClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
      runtimeClient: runtimeClient as unknown as GeneratedClient,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await waitFor(() => expect(runtimeClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-runtime", status: "provisioning" } });

    await waitFor(() => expect(controller.recordCreated).toHaveBeenCalled());
    expect(contextClient.POST).not.toHaveBeenCalled();
  });

  it("create with inline controller records the override and does not navigate", async () => {
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { detailStore, navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
    expect(navigate).not.toHaveBeenCalled();
    await vi.waitFor(() => {
      expect(detailStore.refreshDetailOnly).toHaveBeenCalledWith("acme", "widget", 1, {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
      });
    });
  });

  it("records the override when a layout change unmounts the detail mid-create", async () => {
    // Tab switches, split-view toggles, and breakpoint crossings unmount
    // PullDetail with the same PR still selected; the successful response
    // must still land its store-level override instead of orphaning the
    // created workspace.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { detailStore, navigate, unmount } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    unmount();
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
    expect(navigate).not.toHaveBeenCalled();
    // No refetch from a destroyed component: its frozen identity cannot
    // see that the shared detail store may belong to a new selection.
    expect(detailStore.refreshDetailOnly).not.toHaveBeenCalled();
  });

  it("an alias-only route re-expression does not discard an in-flight create", async () => {
    // gh vs github and omitted vs concrete default host describe the same
    // PR; such a prop change mid-create must not bump the request
    // generation, or the success would be discarded and the button
    // re-enabled for a duplicate request.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { rerender } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await rerender({ provider: "gh", platformHost: undefined });
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
  });

  it("keeps mutation actions live when the identity omits the host or aliases the provider", async () => {
    // Activity URLs may omit platform_host and route segments may carry
    // gh/gl aliases while the payload is canonical and concrete; the
    // stale guard must not block mutations on a detail that is current.
    const controller = createTestController("split");
    const { apiClient } = deferredWorkspaceApiClient();
    const { rerender } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await rerender({ provider: "gh", platformHost: undefined });
    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);

    await vi.waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
  });

  it("keeps the primary workspace create action launch-free", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-new", status: "creating", created: true } });

    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/terminal/ws-new"));
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBeNull();
  });

  it("opens a created workspace through the host callback when one is provided", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const onOpenWorkspace = vi.fn();
    const { navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      onOpenWorkspace,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-new", status: "creating", created: true } });

    await waitFor(() => expect(onOpenWorkspace).toHaveBeenCalledWith("ws-new"));
    expect(navigate).not.toHaveBeenCalledWith("/terminal/ws-new");
  });

  it("queues the agent selected from Create Workspace options", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
    });

    await fireEvent.click(
      screen.getAllByRole("button", {
        name: "Create Workspace options",
      })[0]!,
    );
    await fireEvent.click(screen.getByRole("menuitem", { name: "Codex" }));
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-new", status: "creating" } });

    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/terminal/ws-new"));
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBe("codex");
  });

  it("publishes a confirmed creation even after the selection changed", async () => {
    // The workspace exists server-side the moment the response confirms
    // it. Discarding it because the selection moved on would leave the
    // next visit to this PR offering "Create Workspace" again — a
    // duplicate submission. Only presentation (refetch, navigation)
    // stays tied to the live selection.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { detailStore, navigate, rerender } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    // Navigate to a different PR while the create request is in flight.
    await rerender({ number: 2 });
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    expect(detailStore.refreshDetailOnly).not.toHaveBeenCalled();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("wires shared workspace creation state into the controller-less action", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate, rerender, setEnvelopeTick } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await rerender({ number: 2 });
    await rerender({ number: 1 });

    const pendingButton = screen.getAllByRole("button", { name: "Creating..." })[0]!;
    expect(pendingButton.hasAttribute("disabled")).toBe(true);
    await fireEvent.click(pendingButton);
    expect(apiClient.POST).toHaveBeenCalledTimes(1);

    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await vi.waitFor(() => {
      expect(screen.getAllByRole("button", { name: "Open Workspace" }).length).toBeGreaterThan(0);
    });
    expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
    expect(navigate).not.toHaveBeenCalled();

    setEnvelopeTick(nextWorkspaceLifecycleTick());
    await rerender({ number: 2 });
    await rerender({ number: 1 });

    await vi.waitFor(() => {
      expect(screen.getAllByRole("button", { name: "Create Workspace" }).length).toBeGreaterThan(0);
    });
    expect(screen.queryByRole("button", { name: "Open Workspace" })).toBeNull();
  });
  it("publishes a confirmed creation across a selection round-trip", async () => {
    // A→B→A: returning to the original PR restores an identity that
    // matches the request, but the round-trip bumped the request
    // generation. The confirmed creation must still land its override,
    // or the re-rendered detail shows "Create Workspace" until an
    // unrelated refetch.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { rerender } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    await rerender({ number: 2 });
    await rerender({ number: 1 });
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
  });

  it("discards a create failure that lands after the selection changed (no flash)", async () => {
    const controller = createTestController("split");
    let resolvePost!: (value: { error?: { detail?: string } }) => void;
    const postPromise = new Promise<{ error?: { detail?: string } }>((resolve) => {
      resolvePost = resolve;
    });
    const apiClient = {
      GET: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => postPromise),
    };
    const { rerender } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    // Navigate to a different PR while the create request is in flight.
    await rerender({ number: 2 });
    resolvePost({ error: { detail: "boom" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    // A failure toast about a PR the user already left is noise: without
    // the identity guard this would show "boom".
    expect(getFlashes()).toHaveLength(0);
  });

  it("without a controller create navigates to the terminal (today's behavior)", async () => {
    const apiClient = {
      GET: vi.fn(async () => ({ data: {} })),
      POST: vi.fn(async () => ({ data: { id: "ws-new", status: "provisioning" } })),
    };
    const { navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);

    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith("/terminal/ws-new"));
  });

  it("a late create response cannot navigate after unmount (no controller)", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate } = renderPullDetail(pullDetail(), undefined, apiClient, {
      hideWorkspaceAction: false,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Create Workspace" })[0]!);
    cleanup();
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(navigate).not.toHaveBeenCalled();
  });

  it("open action becomes focus-terminal with a secondary open-in-workspaces", async () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");
    // No override recorded: the controller passes the envelope ref through.
    controller.effectiveWorkspaceRef = vi.fn((_identity, envelopeRef) => envelopeRef ?? null);

    const { navigate } = renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    await fireEvent.click(screen.getAllByRole("button", { name: "Focus Terminal" })[0]!);
    expect(controller.focusTerminal).toHaveBeenCalled();

    await fireEvent.click(screen.getAllByRole("button", { name: "Open in Workspaces" })[0]!);
    expect(controller.openInWorkspaces).toHaveBeenCalledWith({ id: "ws-1", status: "ready" });
    expect(navigate).not.toHaveBeenCalled();
  });

  it.each(["deleting", "deletion_failed"])(
    "routes an inline %s workspace to recovery instead of opening its runtime",
    async (status) => {
      const detail = pullDetail();
      detail.workspace = { id: "ws-1", status };
      const controller = createTestController("split");
      controller.effectiveWorkspaceRef = vi.fn((_identity, envelopeRef) => envelopeRef ?? null);

      const { navigate } = renderPullDetail(detail, undefined, undefined, {
        hideWorkspaceAction: false,
        inlineWorkspace: controller,
      });

      await fireEvent.click(screen.getAllByRole("button", { name: "View in Workspaces" })[0]!);
      expect(navigate).toHaveBeenCalledWith("/workspaces");
      expect(controller.openInWorkspaces).not.toHaveBeenCalled();
    },
  );

  it("phone presentation renders the actions as one kit action grid instead of fit stages", async () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };

    const { navigate } = renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      phonePresentation: true,
    });

    const grid = screen.getByRole("group", { name: "Pull request actions" });
    expect(grid.classList.contains("kit-adaptive-action-grid")).toBe(true);
    expect(within(grid).getByRole("button", { name: "Approve" })).toBeTruthy();
    expect(within(grid).getByRole("button", { name: "Close" })).toBeTruthy();
    expect(document.querySelector(".actions-menu-trigger")).toBeNull();
    expect(document.querySelector(".actions-row--measure")).toBeNull();
    expect(document.querySelector(".kanban-select")).toBeNull();
    expect(screen.getAllByRole("button", { name: "Open Workspace" })).toHaveLength(1);

    await fireEvent.click(within(grid).getByRole("button", { name: "Open Workspace" }));
    expect(navigate).toHaveBeenCalledWith("/terminal/ws-1");
  });

  it("phone presentation shows sync as a top bar instead of an inline row", async () => {
    const detail = pullDetail();
    renderPullDetail(detail, undefined, undefined, { phonePresentation: true, detailSyncing: true });

    const bar = screen.getByRole("status", { name: "Syncing from GitHub" });
    expect(bar.classList.contains("sync-bar")).toBe(true);
    expect(document.querySelector(".sync-indicator")).toBeNull();
  });

  it("desktop presentation keeps the inline syncing indicator", async () => {
    const detail = pullDetail();
    renderPullDetail(detail, undefined, undefined, { detailSyncing: true });

    expect(document.querySelector(".sync-indicator")).not.toBeNull();
    expect(document.querySelector(".sync-bar")).toBeNull();
  });

  it("without a controller open renders a single Open Workspace button that navigates", async () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };

    const { navigate } = renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
    });

    expect(screen.queryByRole("button", { name: "Focus Terminal" })).toBeNull();
    await fireEvent.click(screen.getAllByRole("button", { name: "Open Workspace" })[0]!);
    expect(navigate).toHaveBeenCalledWith("/terminal/ws-1");
  });

  it.each(["deleting", "deletion_failed"])(
    "routes a %s workspace to recovery instead of opening its runtime",
    async (status) => {
      const detail = pullDetail();
      detail.workspace = { id: "ws-1", status };

      const { navigate } = renderPullDetail(detail, undefined, undefined, {
        hideWorkspaceAction: false,
      });

      expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
      await fireEvent.click(screen.getAllByRole("button", { name: "View in Workspaces" })[0]!);
      expect(navigate).toHaveBeenCalledWith("/workspaces");
    },
  );
  it("consults the override layer for button state", () => {
    const detail = pullDetail();
    detail.workspace = undefined;
    const controller = createTestController("split");
    controller.effectiveWorkspaceRef = vi.fn(() => ({ id: "ws-o", status: "ready" }));

    renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Focus Terminal" }).length).toBeGreaterThan(0);
  });

  it("consults the override layer for button state (tombstone hides an envelope workspace)", () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");
    controller.effectiveWorkspaceRef = vi.fn(() => null);

    renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    expect(screen.queryByRole("button", { name: "Focus Terminal" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Create Workspace" }).length).toBeGreaterThan(0);
  });

  it("reconciles the override on identity-matched detail load", () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });

    expect(controller.reconcile).toHaveBeenCalledWith(identity, { id: "ws-1", status: "ready" }, 0);
  });

  it("reconciles when the identity omits the host and the detail carries the provider default", async () => {
    // Activity URLs may omit platform_host; the loaded detail always
    // carries the concrete default host. The identity-match guard must
    // treat them as one item or the override never reconciles.
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    const { rerender } = renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });
    controller.reconcile.mockClear();

    await rerender({ platformHost: undefined });

    expect(controller.reconcile).toHaveBeenCalledWith(
      { ...identity, platformHost: undefined },
      { id: "ws-1", status: "ready" },
      0,
    );
  });

  it("does not reconcile the override for a detail belonging to a different identity", async () => {
    const detail = pullDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    const { rerender } = renderPullDetail(detail, undefined, undefined, {
      hideWorkspaceAction: false,
      inlineWorkspace: controller,
    });
    controller.reconcile.mockClear();

    // Same detail object (still describes PR #1), but props move to a
    // different number: detailMatchesIdentity must now return false so a
    // load for the stale identity can't reconcile an override recorded for
    // the newly-selected PR. Without the guard this would call reconcile
    // with the mismatched identity.
    await rerender({ number: 999 });

    expect(controller.reconcile).not.toHaveBeenCalled();
  });

  it("restores split before opening the label picker while the dock is expanded", async () => {
    // Expanded dock hides this detail (mounted, hidden+inert) but its
    // window-level command listener stays live; without the split reset the
    // picker would open invisibly and pop up on the next collapse.
    const controller = { ...createTestController("expanded"), isClaimedFor: () => true };
    const detail = pullDetail();
    detail.repo.capabilities = { ...capabilities, read_labels: true, label_mutation: true };
    renderPullDetail(detail, undefined, undefined, { inlineWorkspace: controller });

    openLabelPickerFor({
      itemType: "pull",
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 1,
    });

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    expect(controller.setDockMode).toHaveBeenCalledWith("split");
  });

  it("label picker command leaves the dock mode alone when this detail is not claimed", async () => {
    // getDockMode() can read "expanded" from a surface whose claim belongs
    // to nothing (panel inactive -> detail fully visible): the command must
    // not collapse a dock it is not hidden behind.
    const controller = createTestController("expanded");
    const detail = pullDetail();
    detail.repo.capabilities = { ...capabilities, read_labels: true, label_mutation: true };
    renderPullDetail(detail, undefined, undefined, { inlineWorkspace: controller });

    openLabelPickerFor({
      itemType: "pull",
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 1,
    });

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    expect(controller.setDockMode).not.toHaveBeenCalled();
  });
});

describe("PullDetail description collapse", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
  });

  it("offers an expanded collapse control for a long pull description", () => {
    const detail = pullDetail();
    detail.merge_request.Body = "x".repeat(1_501);
    renderPullDetail(detail);

    const collapse = screen.getByRole("button", { name: "Collapse description" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");
  });

  it("does not offer collapse at the long-description threshold", () => {
    const detail = pullDetail();
    detail.merge_request.Body = "x".repeat(1_500);
    renderPullDetail(detail);

    expect(screen.queryByRole("button", { name: "Collapse description" })).toBeNull();
  });

  it("expands a collapsed description after pull navigation", async () => {
    const detail = pullDetail();
    detail.merge_request.Body = "x".repeat(1_501);
    const { rerender } = renderPullDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "Collapse description" }));
    expect(screen.getByRole("button", { name: "Expand description" }).getAttribute("aria-expanded")).toBe("false");

    detail.merge_request.Number = 2;
    await rerender({ number: 2 });

    expect(screen.getByRole("button", { name: "Collapse description" }).getAttribute("aria-expanded")).toBe("true");

    detail.merge_request.Number = 1;
    await rerender({ number: 1 });

    expect(screen.getByRole("button", { name: "Collapse description" }).getAttribute("aria-expanded")).toBe("true");
  });
});

describe("PullDetail body copy feedback", () => {
  beforeEach(() => {
    localStorage.clear();
    clipboardMockState.resolvers.length = 0;
  });

  afterEach(() => {
    cleanup();
  });

  function copyButton(): HTMLButtonElement {
    const button = document.querySelector<HTMLButtonElement>(".kit-copy-btn.body-copy");
    if (button === null) {
      throw new Error("body copy button not found");
    }
    return button;
  }

  it("shows copied feedback when the clipboard write resolves on the same pull", async () => {
    const detail = pullDetail();
    detail.merge_request.Body = "body text";
    renderPullDetail(detail);

    await fireEvent.click(copyButton());
    expect(clipboardMockState.resolvers).toHaveLength(1);
    clipboardMockState.resolvers[0]!(true);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(document.querySelector(".body-copy--copied")).not.toBeNull();
  });

  it("drops a clipboard write that resolves after navigating to another pull", async () => {
    const detail = pullDetail();
    detail.merge_request.Body = "body text";
    const { rerender } = renderPullDetail(detail);

    await fireEvent.click(copyButton());
    expect(clipboardMockState.resolvers).toHaveLength(1);

    // Navigate to a different pull while the clipboard promise is pending.
    await rerender({ number: detail.merge_request.Number + 1 });

    clipboardMockState.resolvers[0]!(true);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(document.querySelector(".body-copy--copied")).toBeNull();
  });

  it("drops branch-copy feedback that resolves after navigating to another pull", async () => {
    const detail = pullDetail();
    const { rerender } = renderPullDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "main" }));
    expect(clipboardMockState.resolvers).toHaveLength(1);
    detail.merge_request.Number += 1;
    await rerender({ number: detail.merge_request.Number });

    clipboardMockState.resolvers[0]!(true);
    await Promise.resolve();
    await Promise.resolve();

    expect(screen.getByRole("button", { name: "main" }).getAttribute("title")).toBe("Click to copy");
  });
});

describe("PullDetail task body saves", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("flushes a pending task checkbox save when the detail unmounts", async () => {
    const detail = pullDetail();
    detail.merge_request.Body = "- [ ] ship the change";
    const view = renderPullDetail(detail);
    let checkbox: HTMLInputElement | null = null;
    await waitFor(() => {
      checkbox = view.container.querySelector<HTMLInputElement>(".markdown-body input[type='checkbox']");
      expect(checkbox?.dataset.taskIndex).toBe("0");
    });
    vi.useFakeTimers();

    await fireEvent.click(checkbox as HTMLInputElement);
    expect(view.detailStore.savePRBodyInBackground).not.toHaveBeenCalled();
    view.unmount();

    expect(view.detailStore.savePRBodyInBackground).toHaveBeenCalledWith(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      1,
      "- [x] ship the change",
    );
  });
});
