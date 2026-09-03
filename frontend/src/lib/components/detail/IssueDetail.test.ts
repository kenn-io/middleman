import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect } from "effect";
import type { ComponentProps } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";
import type { GeneratedClient } from "../../api/generated-api.js";
import type { IssueDetail, Label } from "../../api/types.js";
import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
import { ACTIONS_KEY, NAVIGATE_KEY, STORES_KEY, UI_CONFIG_KEY } from "../../context.js";
import { createDetailActivityViewStore } from "../../stores/detail-activity-view.svelte.js";
import { createIssuesStore } from "../../stores/issues.svelte.js";
import { makeTestAppRuntime } from "../../testing/effect-layers.js";
import { dismissFlash, getFlashes } from "../../stores/flash.svelte.js";
import {
  discardWorkspaceLaunch,
  nextWorkspaceLifecycleTick,
  resetWorkspaceCreatePendingForTest,
} from "../../stores/workspace-create-pending.svelte.js";
import type { ActionRegistry } from "../../types.js";
import type { InlineWorkspaceController, WorkspaceItemIdentity } from "../../workspace-inline.js";
import { openLabelPickerFor } from "./labelPickerCommand.js";
import { createTestController } from "../workspace/inlineWorkspaceTestController.svelte.js";

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

import IssueDetailComponent from "./IssueDetail.svelte";
import IssueDetailTestHarness from "./IssueDetailTestHarness.svelte";

let issueRuntime: OwnedAppRuntime | null = null;

afterEach(async () => {
  if (issueRuntime === null) return;
  await Effect.runPromise(issueRuntime.disposeEffect);
  issueRuntime = null;
});

const capabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_labels: false,
  comment_mutation: false,
  state_mutation: true,
  merge_mutation: false,
  review_mutation: false,
  workflow_approval: false,
  ready_for_review: false,
  draft_mutation: false,
  issue_mutation: true,
  label_mutation: false,
  assignee_mutation: false,
  reviewer_mutation: false,
  thread_reply: false,
  thread_resolve: false,
  review_draft_mutation: false,
  review_thread_resolution: false,
  review_suggestion_application: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: false,
  supported_review_actions: [],
};

function issueDetail(): IssueDetail {
  return {
    detail_loaded: true,
    detail_fetched_at: "2026-05-01T12:05:00Z",
    platform_host: "github.com",
    repo_owner: "acme",
    repo_name: "widget",
    workspace: undefined,
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
    issue: {
      ID: 1,
      RepoID: 1,
      PlatformID: 100,
      PlatformExternalID: "ISSUE_1",
      Number: 7,
      URL: "https://github.com/acme/widget/issues/7",
      Title: "Track compact issue activity",
      Author: "octocat",
      Body: "",
      State: "open",
      CommentCount: 1,
      CreatedAt: "2026-05-01T11:00:00Z",
      UpdatedAt: "2026-05-01T12:00:00Z",
      LastActivityAt: "2026-05-01T12:00:00Z",
      ClosedAt: null,
      DetailFetchedAt: "2026-05-01T12:05:00Z",
      Starred: false,
      labels: [],
      assignees: [],
      detail_loaded: true,
      repo: {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widget",
        repo_path: "acme/widget",
      },
      repo_owner: "acme",
      repo_name: "widget",
      platform_host: "github.com",
    },
    events: [
      {
        ID: 20,
        IssueID: 1,
        PlatformID: 20,
        PlatformExternalID: "",
        EventType: "issue_comment",
        Author: "alice",
        Summary: "",
        Body: "Issue **activity** preview",
        MetadataJSON: "",
        CreatedAt: "2026-05-01T12:01:00Z",
        DedupeKey: "issue-comment-20",
        DirectURL: "",
        ThreadID: null,
      },
    ],
  };
}

function renderIssueDetail(
  detail: IssueDetail,
  deleteIssueComment = vi.fn(
    (_owner: string, _name: string, _number: number, _commentID: number, callbacks: MutationCallbacks): void => {
      callbacks.onSuccess?.();
      callbacks.onSettled?.();
    },
  ),
  options: {
    staleRefreshing?: boolean;
    detailLoading?: boolean;
    detailSyncing?: boolean;
    deferRefresh?: boolean;
    refreshFailure?: string;
    inlineWorkspace?: InlineWorkspaceController | null;
    onOpenWorkspace?: (workspaceId: string) => void;
    actions?: ActionRegistry;
    runtimeClient?: GeneratedClient;
  } = {},
  apiClient: { GET: ReturnType<typeof vi.fn>; POST: ReturnType<typeof vi.fn> } = {
    GET: vi.fn(),
    POST: vi.fn(),
  },
) {
  let envelopeTick = 0;
  let pendingRefreshCallbacks: MutationCallbacks | null = null;
  const issuesStore = {
    loadIssueDetail: vi.fn(),
    startIssueDetailPolling: vi.fn(),
    stopIssueDetailPolling: vi.fn(),
    getIssueDetail: () => detail,
    getIssueDetailEnvelopeTick: () => envelopeTick,
    isIssueDetailLoading: () => options.detailLoading ?? false,
    getIssueDetailError: () => null,
    isIssueStaleRefreshing: () => options.staleRefreshing ?? false,
    isIssueDetailSyncing: () => options.detailSyncing ?? false,
    getIssueDetailLoaded: () => true,
    syncIssueDetailNow: vi.fn(
      (_owner: string, _name: string, _number: number, _identity: unknown, callbacks: MutationCallbacks) => {
        if (options.deferRefresh) {
          options.detailSyncing = true;
          pendingRefreshCallbacks = callbacks;
          return;
        }
        if (options.refreshFailure) callbacks.onFailure?.(options.refreshFailure);
        else callbacks.onSuccess?.();
        callbacks.onSettled?.();
      },
    ),
    loadIssues: vi.fn(),
    updateIssueKanbanState: vi.fn(),
    toggleIssueStar: vi.fn(),
    setIssueState: vi.fn(),
    editIssueComment: vi.fn(),
    deleteIssueComment,
    setIssueLabels: vi.fn(),
    setIssueAssignees: vi.fn(),
    saveIssueBodyInBackground: vi.fn(),
    setLocalIssueBody: vi.fn(),
  };
  const navigate = vi.fn();

  const runtime =
    issueRuntime ?? makeTestAppRuntime(options.runtimeClient ?? (apiClient as unknown as GeneratedClient));
  issueRuntime = runtime;
  let detailProps: ComponentProps<typeof IssueDetailComponent> = {
    owner: "acme",
    name: "widget",
    number: detail.issue.Number,
    provider: "github",
    platformHost: "github.com",
    repoPath: "acme/widget",
    inlineWorkspace: options.inlineWorkspace ?? null,
    onOpenWorkspace: options.onOpenWorkspace,
  };
  const result = render(IssueDetailTestHarness, {
    props: {
      runtime,
      detailProps,
    },
    context: new Map<symbol, unknown>([
      [
        STORES_KEY,
        {
          issues: issuesStore,
          activity: { loadActivity: vi.fn() },
          detailActivityView: createDetailActivityViewStore(),
          settings: {
            getLaunchTargets: () => launchTargets,
            getDetailSettings: () => ({ initial_timeline_entry_limit: 250 }),
          },
        },
      ],
      [ACTIONS_KEY, options.actions ?? { issue: [] }],
      [UI_CONFIG_KEY, { hideStar: true }],
      [NAVIGATE_KEY, navigate],
    ]),
  });
  return {
    ...result,
    rerender: (next: Partial<ComponentProps<typeof IssueDetailComponent>>) => {
      detailProps = { ...detailProps, ...next };
      return result.rerender({ runtime, detailProps });
    },
    deleteIssueComment,
    issuesStore,
    navigate,
    settleRefresh: () => {
      options.detailSyncing = false;
      pendingRefreshCallbacks?.onSuccess?.();
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

describe("IssueDetail activity view", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    for (const item of getFlashes()) dismissFlash(item.id);
  });

  it("offers compact activity rows from the shared View menu without PR filters", async () => {
    const { container } = renderIssueDetail(issueDetail());

    await fireEvent.click(screen.getByRole("button", { name: /view/i }));

    expect(screen.getByRole("button", { name: /normal/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /compact/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /messages/i })).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: /compact/i }));

    expect(localStorage.getItem("kenn-forge-detail-activity-view")).toBe("compact");
    expect(container.querySelectorAll(".event-card--compact-row")).toHaveLength(1);
    expect(container.textContent).toContain("Issue activity preview");
  });

  it("refreshes the whole selected issue from the Activity header", async () => {
    const { issuesStore } = renderIssueDetail(issueDetail());

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    expect(issuesStore.syncIssueDetailNow).toHaveBeenCalledWith(
      "acme",
      "widget",
      7,
      {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
      },
      expect.objectContaining({ onFailure: expect.any(Function) }),
    );
  });

  it("tracks manual refresh progress separately and reports issue sync failures", async () => {
    renderIssueDetail(issueDetail(), undefined, { refreshFailure: "provider unavailable" });

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect(getFlashes()).toEqual([expect.objectContaining({ message: "provider unavailable", tone: "danger" })]);

    cleanup();
    const { settleRefresh } = renderIssueDetail(issueDetail(), undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();
    expect(document.querySelector(".sync-indicator")).toBeNull();

    settleRefresh();
    await waitFor(() => {
      expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(false);
    });
  });

  it("does not present a background issue sync as a manual refresh", () => {
    renderIssueDetail(issueDetail(), undefined, { detailSyncing: true });

    expect((screen.getByRole("button", { name: "Refresh detail" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByLabelText("Refreshing detail")).toBeNull();
    expect(document.querySelector(".sync-indicator")?.textContent).toContain("Syncing");
  });

  it("blocks manual refresh while issue detail navigation is loading or stale", async () => {
    const loading = renderIssueDetail(issueDetail(), undefined, { detailLoading: true });
    const loadingButton = screen.getByRole("button", { name: "Refresh detail" });

    expect((loadingButton as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(loadingButton);
    expect(loading.issuesStore.syncIssueDetailNow).not.toHaveBeenCalled();

    cleanup();
    const navigating = renderIssueDetail(issueDetail());
    await navigating.rerender({ number: 8 });
    const staleButton = screen.getByRole("button", { name: "Refresh detail" });

    expect((staleButton as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(staleButton);
    expect(navigating.issuesStore.syncIssueDetailNow).not.toHaveBeenCalled();
  });

  it("drops pending manual refresh UI and late failures after an issue reroute", async () => {
    const { failRefresh, rerender } = renderIssueDetail(issueDetail(), undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();

    await rerender({ number: 8 });

    await waitFor(() => expect(screen.queryByLabelText("Refreshing detail")).toBeNull());

    failRefresh("old issue failed");
    expect(getFlashes()).toHaveLength(0);
  });

  it("drops a manual refresh failure after the issue detail unmounts", async () => {
    const { failRefresh, unmount } = renderIssueDetail(issueDetail(), undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    unmount();
    failRefresh("unmounted issue failed");

    expect(getFlashes()).toHaveLength(0);
  });

  it("keeps a pending issue refresh across an equivalent route alias", async () => {
    const { failRefresh, rerender } = renderIssueDetail(issueDetail(), undefined, { deferRefresh: true });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));

    await rerender({ provider: "gh", platformHost: undefined });

    expect(screen.getByLabelText("Refreshing detail")).toBeTruthy();
    failRefresh("current issue failed");
    expect(getFlashes()).toEqual([expect.objectContaining({ message: "current issue failed" })]);
  });

  it("preserves a real issue-store refresh across an equivalent route alias", async () => {
    const cached = issueDetail();
    cached.issue.Title = "Cached issue detail";
    const fresh = issueDetail();
    fresh.issue.Title = "Fresh issue detail";
    const syncResponse = Promise.withResolvers<{ data: IssueDetail; error: undefined }>();
    const get = vi.fn(async () => ({ data: cached, error: undefined }));
    const post = vi.fn(() => syncResponse.promise);
    issueRuntime = makeTestAppRuntime({ GET: get, POST: post } as unknown as GeneratedClient);
    const issuesStore = createIssuesStore({ runtime: issueRuntime });
    let detailProps: ComponentProps<typeof IssueDetailComponent> = {
      owner: "acme",
      name: "widget",
      number: 7,
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      autoSync: false,
    };
    const rendered = render(IssueDetailTestHarness, {
      props: { runtime: issueRuntime, detailProps },
      context: new Map<symbol, unknown>([
        [
          STORES_KEY,
          {
            issues: issuesStore,
            activity: { loadActivity: vi.fn() },
            detailActivityView: createDetailActivityViewStore(),
            settings: {
              getLaunchTargets: () => launchTargets,
              getDetailSettings: () => ({ initial_timeline_entry_limit: 250 }),
            },
          },
        ],
        [ACTIONS_KEY, { issue: [] }],
        [UI_CONFIG_KEY, { hideStar: true }],
        [NAVIGATE_KEY, vi.fn()],
      ]),
    });
    await waitFor(() => expect(get).toHaveBeenCalledOnce());
    await waitFor(() => expect(issuesStore.isIssueDetailLoading()).toBe(false));

    await fireEvent.click(screen.getByRole("button", { name: "Refresh detail" }));
    await waitFor(() => expect(issuesStore.isIssueDetailSyncing()).toBe(true));
    detailProps = { ...detailProps, provider: "gh", platformHost: undefined };
    await rendered.rerender({ runtime: issueRuntime, detailProps });
    syncResponse.resolve({ data: fresh, error: undefined });

    await waitFor(() => expect(screen.queryByLabelText("Refreshing detail")).toBeNull());
    expect(get).toHaveBeenCalledOnce();
    expect(issuesStore.getIssueDetail()?.issue.Title).toBe("Fresh issue detail");
  });

  it("explains that creating a workspace enables agent sessions", () => {
    renderIssueDetail(issueDetail());

    const button = screen.getByRole("button", { name: "Create Workspace" });
    expect(button.getAttribute("aria-describedby")).toBe("issue-create-workspace-description");
    expect(button.getAttribute("title")).toContain("issue worktree");
    expect(button.getAttribute("title")).toContain("launch agents");
    expect(button.getAttribute("title")).toContain("shells");
    expect(document.getElementById("issue-create-workspace-description")?.textContent).toContain(
      button.getAttribute("title"),
    );
  });

  it("places all issue actions before the description", () => {
    const detail = issueDetail();
    detail.issue.Body = "Action placement marker";

    renderIssueDetail(detail, undefined, {
      actions: {
        issue: [
          {
            id: "extension-action",
            label: "Extension action",
            handler: vi.fn(),
          },
        ],
      },
    });

    const description = screen.getByText("Description");
    for (const action of [
      screen.getByRole("button", { name: "Create Workspace" }),
      screen.getByRole("button", { name: "Close issue" }),
      screen.getByRole("button", { name: "Extension action" }),
    ]) {
      expect(action.compareDocumentPosition(description) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("renders every issue action inside one kit action grid at the grid's size", () => {
    renderIssueDetail(issueDetail());

    const grid = screen.getByRole("group", { name: "Issue actions" });
    expect(grid.classList.contains("kit-adaptive-action-grid")).toBe(true);

    // The grid owns control geometry through its item context. A button that
    // asks for its own size (the old size="sm" row, the split button's pinned
    // height) opts out of that and renders taller or shorter than its
    // neighbours, which is the misalignment this row shipped with.
    for (const name of ["Create Workspace", "Create Workspace options", "Close issue"]) {
      const button = screen.getByRole("button", { name });
      expect(grid.contains(button)).toBe(true);
      expect(button.classList.contains("kit-button--md")).toBe(true);
    }
  });

  it("labels an active stale-detail refresh", () => {
    renderIssueDetail(issueDetail(), undefined, { staleRefreshing: true });

    expect(screen.getByLabelText("Refreshing issue details")).toBeTruthy();
    expect(screen.getByText("Refreshing...")).toBeTruthy();
  });

  it("deletes an ordinary issue comment through the issues store", async () => {
    const detail = issueDetail();
    detail.repo.capabilities.comment_mutation = true;
    detail.repo.operations = {
      delete_comment: { available: true },
    } as IssueDetail["repo"]["operations"];
    const { deleteIssueComment } = renderIssueDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "Delete comment" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    await waitFor(() => {
      expect(deleteIssueComment).toHaveBeenCalledWith(
        "acme",
        "widget",
        7,
        20,
        expect.objectContaining({
          onSuccess: expect.any(Function),
          onFailure: expect.any(Function),
          onSettled: expect.any(Function),
        }),
      );
      expect(screen.queryByRole("dialog", { name: "Delete comment?" })).toBeNull();
    });
  });

  it("hides issue comment deletion when the operation is unavailable", () => {
    const detail = issueDetail();
    detail.repo.capabilities.comment_mutation = true;
    detail.repo.operations = {
      delete_comment: {
        available: false,
        code: "missing_write_credential",
        unavailable_reason: "No user credential for writes on github.com",
      },
    } as IssueDetail["repo"]["operations"];

    renderIssueDetail(detail);

    expect(screen.queryByRole("button", { name: "Delete comment" })).toBeNull();
  });
});

describe("IssueDetail inline workspace handoff", () => {
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
    number: 7,
    itemType: "issue",
  };

  function deferredWorkspaceApiClient() {
    let resolvePost!: (value: { data?: { id: string; status: string; created?: boolean } }) => void;
    const postPromise = new Promise<{ data?: { id: string; status: string; created?: boolean } }>((resolve) => {
      resolvePost = resolve;
    });
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => postPromise),
    };
    return { apiClient, resolvePost };
  }

  function workspaceBranchConflict(existingBranch = "kenn-forge/issue-7-original-title", existingDirectory = false) {
    return {
      type: "urn:kenn-forge:error:issue-workspace-branch-conflict",
      title: "Issue workspace branch conflict",
      detail: "A local branch with the requested name already exists.",
      details: { existingDirectory },
      errors: [
        {
          message: "Requested branch already exists",
          location: "body.git_head_ref",
          value: existingBranch,
        },
        {
          message: "Suggested alternative branch name",
          location: "body.suggested_git_head_ref",
          value: `${existingBranch}-2`,
        },
      ],
    };
  }

  it("creates an issue workspace through the app runtime", async () => {
    const controller = createTestController("split");
    const { apiClient: runtimeClient, resolvePost } = deferredWorkspaceApiClient();
    const contextClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => ({ error: { title: "legacy client used" } })),
    };
    renderIssueDetail(
      issueDetail(),
      undefined,
      {
        inlineWorkspace: controller,
        runtimeClient: runtimeClient as unknown as GeneratedClient,
      },
      contextClient,
    );

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await waitFor(() => expect(runtimeClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-runtime", status: "provisioning" } });

    await waitFor(() => expect(controller.recordCreated).toHaveBeenCalled());
    expect(contextClient.POST).not.toHaveBeenCalled();
  });

  it("recovers the expected existing workspace directory", async () => {
    const controller = createTestController("split");
    const conflict = workspaceBranchConflict("kenn-forge/issue-7-original-title", true);
    const apiClient = {
      GET: vi.fn(),
      POST: vi
        .fn()
        .mockResolvedValueOnce({ error: conflict })
        .mockResolvedValueOnce({ data: { id: "ws-recovered", status: "provisioning" } }),
    };
    renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    const useExistingDirectory = await screen.findByRole("button", { name: "Use Existing Directory" });
    expect(screen.queryByRole("button", { name: "Use Existing Branch" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Create New Branch" })).toBeNull();
    expect(screen.queryByLabelText("New branch name")).toBeNull();
    await fireEvent.click(useExistingDirectory);

    await vi.waitFor(() =>
      expect(apiClient.POST.mock.calls[1]?.[1]).toMatchObject({
        body: {
          git_head_ref: "kenn-forge/issue-7-original-title",
          reuse_existing_directory: true,
        },
      }),
    );
    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, {
        id: "ws-recovered",
        status: "provisioning",
      });
    });
  });

  it("keeps directory recovery errors in the conflict dialog", async () => {
    const conflict = workspaceBranchConflict("kenn-forge/issue-7-original-title", true);
    const apiClient = {
      GET: vi.fn(),
      POST: vi
        .fn()
        .mockResolvedValueOnce({ error: conflict })
        .mockResolvedValueOnce({
          error: {
            code: "workspaceDirectoryNotReusable",
            detail: "the expected Kenn Forge worktree directory does not exist",
            details: { reason: "missing" },
          },
        }),
    };
    renderIssueDetail(issueDetail(), undefined, {}, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Use Existing Directory" }));

    expect(await screen.findByRole("dialog", { name: "Existing Workspace Directory" })).toBeTruthy();
    expect(await screen.findByText("the expected Kenn Forge worktree directory does not exist")).toBeTruthy();
  });

  it("explains when the existing branch is checked out elsewhere", async () => {
    const conflict = workspaceBranchConflict();
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn().mockResolvedValueOnce({ error: conflict }).mockResolvedValueOnce({ error: conflict }),
    };
    renderIssueDetail(issueDetail(), undefined, {}, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Use Existing Branch" }));

    expect(
      await screen.findByText("This branch is already checked out in another worktree. Create a new branch instead."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Use Existing Directory" })).toBeNull();
  });

  it("create with inline controller records the override and does not navigate", async () => {
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { issuesStore, navigate } = renderIssueDetail(
      issueDetail(),
      undefined,
      { inlineWorkspace: controller },
      apiClient,
    );

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
    expect(navigate).not.toHaveBeenCalled();
    await vi.waitFor(() => {
      expect(issuesStore.loadIssueDetail).toHaveBeenCalledWith("acme", "widget", 7, {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
      });
    });
  });

  it("records the override when a layout change unmounts the detail mid-create", async () => {
    // Drawer tab and layout changes unmount IssueDetail with the same
    // issue still selected; the successful response must still land its
    // store-level override instead of orphaning the created workspace.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { issuesStore, navigate, unmount } = renderIssueDetail(
      issueDetail(),
      undefined,
      { inlineWorkspace: controller },
      apiClient,
    );

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    const loadCallsBeforeUnmount = issuesStore.loadIssueDetail.mock.calls.length;
    unmount();
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });

    await vi.waitFor(() => {
      expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    });
    expect(navigate).not.toHaveBeenCalled();
    // No refetch from a destroyed component: its frozen identity cannot
    // see that the shared issue store may belong to a new selection.
    expect(issuesStore.loadIssueDetail.mock.calls.length).toBe(loadCallsBeforeUnmount);
  });

  it("an alias-only route re-expression does not discard an in-flight create", async () => {
    // gh vs github and omitted vs concrete default host describe the same
    // issue; such a prop change mid-create must not bump the request
    // generation, or the success would be discarded and the button
    // re-enabled for a duplicate request.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { rerender } = renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
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
    const { rerender } = renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await rerender({ provider: "gh", platformHost: undefined });
    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));

    await vi.waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
  });

  it("keeps the primary workspace create action launch-free", async () => {
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-new", status: "creating", created: true } });

    await waitFor(() => expect(controller.recordCreated).toHaveBeenCalled());
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBeNull();
  });

  it("queues the agent selected from Create Workspace options", async () => {
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(
      screen.getByRole("button", {
        name: "Create Workspace options",
      }),
    );
    await fireEvent.click(screen.getByRole("menuitem", { name: "Codex" }));
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalled());
    resolvePost({ data: { id: "ws-new", status: "creating" } });

    await waitFor(() => expect(controller.recordCreated).toHaveBeenCalled());
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBe("codex");
  });

  it("retains the selected agent when reusing an existing branch", async () => {
    const controller = createTestController("split");
    const apiClient = {
      GET: vi.fn(),
      POST: vi
        .fn()
        .mockResolvedValueOnce({ error: workspaceBranchConflict() })
        .mockResolvedValueOnce({ data: { id: "ws-existing", status: "ready" } }),
    };
    renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(
      screen.getByRole("button", {
        name: "Create Workspace options",
      }),
    );
    await fireEvent.click(screen.getByRole("menuitem", { name: "Codex" }));
    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Use Existing Branch",
      }),
    );
    await waitFor(() => expect(apiClient.POST).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(controller.recordCreated).toHaveBeenCalled());
    expect(discardWorkspaceLaunch("ws-existing", undefined)).toBe("codex");
  });

  it("publishes a confirmed creation even after the selection changed", async () => {
    // The workspace exists server-side the moment the response confirms
    // it. Discarding it because the selection moved on would leave the
    // next visit to this issue offering "Create Workspace" again — a
    // duplicate submission. Only presentation (refetch, navigation)
    // stays tied to the live selection.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { issuesStore, navigate, rerender } = renderIssueDetail(
      issueDetail(),
      undefined,
      { inlineWorkspace: controller },
      apiClient,
    );

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    // Navigate to a different issue while the create request is in flight.
    // The route change legitimately triggers its own loadIssueDetail call,
    // so the assertion below checks the call count doesn't grow further
    // once the stale create response lands, not that it's zero.
    await rerender({ number: 8 });
    const loadCallsAfterRerender = issuesStore.loadIssueDetail.mock.calls.length;
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(controller.recordCreated).toHaveBeenCalledWith(identity, { id: "ws-new", status: "provisioning" });
    expect(issuesStore.loadIssueDetail.mock.calls.length).toBe(loadCallsAfterRerender);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("wires shared workspace creation state into the controller-less action", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate, rerender, setEnvelopeTick } = renderIssueDetail(issueDetail(), undefined, {}, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await rerender({ number: 8 });
    await rerender({ number: 7 });

    const pendingButton = screen.getByRole("button", { name: "Creating..." });
    expect(pendingButton.hasAttribute("disabled")).toBe(true);
    await fireEvent.click(pendingButton);
    expect(apiClient.POST).toHaveBeenCalledTimes(1);

    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await vi.waitFor(() => {
      expect(screen.getByRole("button", { name: "Open Workspace" })).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
    expect(navigate).not.toHaveBeenCalled();

    setEnvelopeTick(nextWorkspaceLifecycleTick());
    await rerender({ number: 8 });
    await rerender({ number: 7 });

    await vi.waitFor(() => {
      expect(screen.getByRole("button", { name: "Create Workspace" })).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Open Workspace" })).toBeNull();
  });
  it("publishes a confirmed creation across a selection round-trip", async () => {
    // A→B→A: returning to the original issue restores an identity that
    // matches the request, but the round-trip bumped the request
    // generation. The confirmed creation must still land its override,
    // or the re-rendered detail shows "Create Workspace" until an
    // unrelated refetch.
    const controller = createTestController("split");
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { rerender } = renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    await rerender({ number: 8 });
    await rerender({ number: 7 });
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
      GET: vi.fn(),
      POST: vi.fn(async () => postPromise),
    };
    const { rerender } = renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    // Navigate to a different issue while the create request is in flight.
    await rerender({ number: 8 });
    resolvePost({ error: { detail: "boom" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    // A failure toast about an issue the user already left is noise:
    // without the identity guard this would show "boom".
    expect(getFlashes()).toHaveLength(0);
  });

  it("without a controller create navigates to the terminal (today's behavior)", async () => {
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => ({ data: { id: "ws-new", status: "provisioning" } })),
    };
    const { navigate } = renderIssueDetail(issueDetail(), undefined, {}, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));

    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith("/terminal/ws-new"));
  });

  it("opens a created workspace through the host callback when one is provided", async () => {
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => ({ data: { id: "ws-new", status: "provisioning" } })),
    };
    const onOpenWorkspace = vi.fn();
    const { navigate } = renderIssueDetail(issueDetail(), undefined, { onOpenWorkspace }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));

    // Phone-like issue routes hand the host the new workspace; the desktop
    // terminal route is unusable from the phone shell.
    await vi.waitFor(() => expect(onOpenWorkspace).toHaveBeenCalledWith("ws-new"));
    expect(navigate).not.toHaveBeenCalledWith("/terminal/ws-new");
  });

  it("discards a create failure that lands after the component unmounted (no flash)", async () => {
    const controller = createTestController("split");
    let resolvePost!: (value: { error?: { detail?: string } }) => void;
    const postPromise = new Promise<{ error?: { detail?: string } }>((resolve) => {
      resolvePost = resolve;
    });
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => postPromise),
    };
    renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    cleanup();
    resolvePost({ error: { detail: "boom" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(getFlashes()).toHaveLength(0);
  });

  it("a late create response cannot navigate after unmount (no controller)", async () => {
    const { apiClient, resolvePost } = deferredWorkspaceApiClient();
    const { navigate } = renderIssueDetail(issueDetail(), undefined, {}, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    cleanup();
    resolvePost({ data: { id: "ws-new", status: "provisioning" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(navigate).not.toHaveBeenCalled();
  });

  it("discards a branch-conflict response that lands after the selection changed", async () => {
    const conflictError = {
      type: "urn:kenn-forge:error:issue-workspace-branch-conflict",
      title: "Issue workspace branch conflict",
      detail: "A local branch with the requested name already exists.",
      errors: [
        { message: "Requested branch already exists", location: "body.git_head_ref", value: "kenn-forge/issue-7" },
        {
          message: "Suggested alternative branch name",
          location: "body.suggested_git_head_ref",
          value: "kenn-forge/issue-7-2",
        },
      ],
    };
    let resolvePost!: (value: { error?: typeof conflictError }) => void;
    const postPromise = new Promise<{ error?: typeof conflictError }>((resolve) => {
      resolvePost = resolve;
    });
    const apiClient = {
      GET: vi.fn(),
      POST: vi.fn(async () => postPromise),
    };
    const controller = createTestController("split");
    const { rerender } = renderIssueDetail(issueDetail(), undefined, { inlineWorkspace: controller }, apiClient);

    await fireEvent.click(screen.getByRole("button", { name: "Create Workspace" }));
    // Move to a different issue while the request is in flight, then let
    // the conflict land: the dialog must not pop up over the new issue.
    await rerender({ number: 8 });
    resolvePost({ error: conflictError });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(screen.queryByText("Branch Name Conflict")).toBeNull();
  });

  it("open action becomes focus-terminal with a secondary open-in-workspaces", async () => {
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");
    // No override recorded: the controller passes the envelope ref through.
    controller.effectiveWorkspaceRef = vi.fn((_identity, envelopeRef) => envelopeRef ?? null);

    const { navigate } = renderIssueDetail(detail, undefined, { inlineWorkspace: controller });

    await fireEvent.click(screen.getByRole("button", { name: "Focus Terminal" }));
    expect(controller.focusTerminal).toHaveBeenCalled();

    await fireEvent.click(screen.getByRole("button", { name: "Open in Workspaces" }));
    expect(controller.openInWorkspaces).toHaveBeenCalledWith({ id: "ws-1", status: "ready" });
    expect(navigate).not.toHaveBeenCalled();
  });

  it.each(["deleting", "deletion_failed"])(
    "routes an inline %s workspace to recovery instead of opening its runtime",
    async (status) => {
      const detail = issueDetail();
      detail.workspace = { id: "ws-1", status };
      const controller = createTestController("split");
      controller.effectiveWorkspaceRef = vi.fn((_identity, envelopeRef) => envelopeRef ?? null);

      const { navigate } = renderIssueDetail(detail, undefined, { inlineWorkspace: controller });

      await fireEvent.click(screen.getByRole("button", { name: "View in Workspaces" }));
      expect(navigate).toHaveBeenCalledWith("/workspaces");
      expect(controller.openInWorkspaces).not.toHaveBeenCalled();
    },
  );

  it("without a controller open renders a single Open Workspace button that navigates", async () => {
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };

    const { navigate } = renderIssueDetail(detail);

    expect(screen.queryByRole("button", { name: "Focus Terminal" })).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Open Workspace" }));
    expect(navigate).toHaveBeenCalledWith("/terminal/ws-1");
  });

  it.each(["deleting", "deletion_failed"])(
    "routes a %s workspace to recovery instead of opening its runtime",
    async (status) => {
      const detail = issueDetail();
      detail.workspace = { id: "ws-1", status };

      const { navigate } = renderIssueDetail(detail);

      expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "View in Workspaces" }));
      expect(navigate).toHaveBeenCalledWith("/workspaces");
    },
  );
  it("consults the override layer for button state", () => {
    const detail = issueDetail();
    detail.workspace = undefined;
    const controller = createTestController("split");
    controller.effectiveWorkspaceRef = vi.fn(() => ({ id: "ws-o", status: "ready" }));

    renderIssueDetail(detail, undefined, { inlineWorkspace: controller });

    expect(screen.queryByRole("button", { name: "Create Workspace" })).toBeNull();
    expect(screen.getByRole("button", { name: "Focus Terminal" })).toBeTruthy();
  });

  it("consults the override layer for button state (tombstone hides an envelope workspace)", () => {
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");
    controller.effectiveWorkspaceRef = vi.fn(() => null);

    renderIssueDetail(detail, undefined, { inlineWorkspace: controller });

    expect(screen.queryByRole("button", { name: "Focus Terminal" })).toBeNull();
    expect(screen.getByRole("button", { name: "Create Workspace" })).toBeTruthy();
  });

  it("reconciles the override on identity-matched detail load", () => {
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    renderIssueDetail(detail, undefined, { inlineWorkspace: controller });

    expect(controller.reconcile).toHaveBeenCalledWith(identity, { id: "ws-1", status: "ready" }, 0);
  });

  it("reconciles when the identity omits the host and the detail carries the provider default", async () => {
    // Activity URLs may omit platform_host; the loaded detail always
    // carries the concrete default host. The identity-match guard must
    // treat them as one item or the override never reconciles.
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    const { rerender } = renderIssueDetail(detail, undefined, { inlineWorkspace: controller });
    controller.reconcile.mockClear();

    await rerender({ platformHost: undefined });

    expect(controller.reconcile).toHaveBeenCalledWith(
      { ...identity, platformHost: undefined },
      { id: "ws-1", status: "ready" },
      0,
    );
  });

  it("does not reconcile the override for a detail belonging to a different identity", async () => {
    const detail = issueDetail();
    detail.workspace = { id: "ws-1", status: "ready" };
    const controller = createTestController("split");

    const { rerender } = renderIssueDetail(detail, undefined, { inlineWorkspace: controller });
    controller.reconcile.mockClear();

    // Same detail object (still describes issue #7), but props move to a
    // different number: detailMatchesIdentity must now return false so a
    // load for the stale identity can't reconcile an override recorded for
    // the newly-selected issue. Without the guard this would call
    // reconcile with the mismatched identity.
    await rerender({ number: 999 });

    expect(controller.reconcile).not.toHaveBeenCalled();
  });

  it("restores split before opening the label picker while the dock is expanded", async () => {
    // Expanded dock hides this detail (mounted, hidden+inert) but its
    // window-level command listener stays live; without the split reset the
    // picker would open invisibly and pop up on the next collapse.
    const controller = { ...createTestController("expanded"), isClaimedFor: () => true };
    const detail = issueDetail();
    detail.repo.capabilities = { ...capabilities, read_labels: true, label_mutation: true };
    renderIssueDetail(
      detail,
      undefined,
      { inlineWorkspace: controller },
      {
        GET: vi.fn(async () => ({ data: { labels: [] } })),
        POST: vi.fn(),
      },
    );

    openLabelPickerFor({
      itemType: "issue",
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 7,
    });

    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    expect(controller.setDockMode).toHaveBeenCalledWith("split");
  });

  it("loads the issue label catalog through the app runtime", async () => {
    const detail = issueDetail();
    detail.repo.capabilities = { ...capabilities, read_labels: true, label_mutation: true };
    const contextClient = {
      GET: vi.fn(async () => ({
        data: { labels: [{ name: "context-label", color: "ededed", description: "" }] },
      })),
      POST: vi.fn(),
    };
    const runtimeClient = {
      GET: vi.fn(async () => ({
        data: { labels: [{ name: "runtime-label", color: "ededed", description: "" }] },
      })),
    } as unknown as GeneratedClient;

    renderIssueDetail(detail, undefined, { runtimeClient }, contextClient);
    openLabelPickerFor({
      itemType: "issue",
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 7,
    });

    expect(await screen.findByText("runtime-label")).toBeTruthy();
    expect(screen.queryByText("context-label")).toBeNull();
  });

  it("does not apply a closed picker catalog to a reopened issue picker", async () => {
    const detail = issueDetail();
    detail.repo.capabilities = { ...capabilities, read_labels: true, label_mutation: true };
    const oldCatalog = Promise.withResolvers<{ data: { labels: Label[] } }>();
    const freshCatalog = Promise.withResolvers<{ data: { labels: Label[] } }>();
    const get = vi.fn().mockReturnValueOnce(oldCatalog.promise).mockReturnValueOnce(freshCatalog.promise);
    renderIssueDetail(detail, undefined, {}, { GET: get, POST: vi.fn() });
    const command = {
      itemType: "issue" as const,
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 7,
    };

    openLabelPickerFor(command);
    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Close label picker" }));
    openLabelPickerFor(command);
    expect(await screen.findByRole("dialog", { name: "Edit labels" })).toBeTruthy();

    oldCatalog.resolve({ data: { labels: [{ name: "old-open", color: "ededed", description: "" }] } });
    await oldCatalog.promise;
    expect(screen.queryByText("old-open")).toBeNull();

    freshCatalog.resolve({ data: { labels: [{ name: "fresh-open", color: "ededed", description: "" }] } });
    expect(await screen.findByText("fresh-open")).toBeTruthy();
  });
});

describe("IssueDetail description collapse", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
  });

  it("collapses and expands a long issue description", async () => {
    const detail = issueDetail();
    detail.issue.Body = "x".repeat(1_501);
    renderIssueDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "Collapse description" }));
    const expand = screen.getByRole("button", { name: "Expand description" });
    expect(expand.getAttribute("aria-expanded")).toBe("false");

    await fireEvent.click(expand);
    expect(screen.getByRole("button", { name: "Collapse description" }).getAttribute("aria-expanded")).toBe("true");
  });

  it("expands a collapsed description after issue navigation", async () => {
    const detail = issueDetail();
    detail.issue.Body = "x".repeat(1_501);
    const { rerender } = renderIssueDetail(detail);

    await fireEvent.click(screen.getByRole("button", { name: "Collapse description" }));

    detail.issue.Number = 8;
    await rerender({ number: 8 });
    expect(screen.getByRole("button", { name: "Collapse description" }).getAttribute("aria-expanded")).toBe("true");

    detail.issue.Number = 7;
    await rerender({ number: 7 });
    expect(screen.getByRole("button", { name: "Collapse description" }).getAttribute("aria-expanded")).toBe("true");
  });
});

describe("IssueDetail body copy feedback", () => {
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

  it("shows copied feedback when the clipboard write resolves on the same issue", async () => {
    const detail = issueDetail();
    detail.issue.Body = "body text";
    renderIssueDetail(detail);

    await fireEvent.click(copyButton());
    expect(clipboardMockState.resolvers).toHaveLength(1);
    clipboardMockState.resolvers[0]!(true);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(document.querySelector(".body-copy--copied")).not.toBeNull();
  });

  it("drops a clipboard write that resolves after navigating to another issue", async () => {
    const detail = issueDetail();
    detail.issue.Body = "body text";
    const { rerender } = renderIssueDetail(detail);

    await fireEvent.click(copyButton());
    expect(clipboardMockState.resolvers).toHaveLength(1);

    // Navigate to a different issue while the clipboard promise is pending.
    await rerender({ number: detail.issue.Number + 1 });

    clipboardMockState.resolvers[0]!(true);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(document.querySelector(".body-copy--copied")).toBeNull();
  });
});

describe("IssueDetail task body saves", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("flushes a pending task checkbox save when the detail unmounts", async () => {
    const detail = issueDetail();
    detail.issue.Body = "- [ ] ship the change";
    const view = renderIssueDetail(detail);
    let checkbox: HTMLInputElement | null = null;
    await waitFor(() => {
      checkbox = view.container.querySelector<HTMLInputElement>(".markdown-body input[type='checkbox']");
      expect(checkbox?.dataset.taskIndex).toBe("0");
    });
    vi.useFakeTimers();

    await fireEvent.click(checkbox as HTMLInputElement);
    expect(view.issuesStore.saveIssueBodyInBackground).not.toHaveBeenCalled();
    view.unmount();

    expect(view.issuesStore.saveIssueBodyInBackground).toHaveBeenCalledWith(
      "acme",
      "widget",
      7,
      "- [x] ship the change",
      {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
      },
    );
  });
});
