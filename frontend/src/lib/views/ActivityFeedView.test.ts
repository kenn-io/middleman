import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { createRawSnippet, tick, type Snippet } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { STORES_KEY } from "../context.js";
import { resetModalStack } from "../stores/keyboard/modal-stack.svelte.js";
import { getPaneLayoutStore, resetPaneLayoutStoresForTest } from "../stores/paneLayout.svelte.js";
import { sessionPaneKey } from "../stores/session-pane-key.js";
import { createClaimTestController, createReactiveValue } from "./viewWorkspaceTestDoubles.svelte.js";
import type { InlineWorkspaceController } from "../workspace-inline.js";

vi.mock("../components/ActivityFeed.svelte", async () => ({
  default: (await import("./ActivityFeedViewTestActivityFeed.svelte")).default,
}));
vi.mock("../components/detail/PullDetailPane.svelte", async () => ({
  default: (await import("./ActivityFeedViewTestPullDetailPane.svelte")).default,
}));
vi.mock("../components/detail/IssueDetail.svelte", async () => ({
  default: (await import("./IssueListViewTestIssueDetail.svelte")).default,
}));
vi.mock("../components/CommitDiffPanel.svelte", async () => ({
  default: (await import("./ActivityFeedViewTestCommitDiffPanel.svelte")).default,
}));

import ActivityFeedView from "./ActivityFeedViewRuntimeHarness.svelte";

const repo = {
  provider: "github",
  platformHost: "github.com",
  repoPath: "acme/widgets",
  owner: "acme",
  name: "widgets",
};

function prDrawer(number = 12) {
  return { ...repo, itemType: "pr" as const, number, detailTab: "conversation" as const };
}

function issueDrawer(number = 9) {
  return { ...repo, itemType: "issue" as const, number, detailTab: "conversation" as const };
}

function commitDrawer() {
  return {
    ...repo,
    itemType: "commit" as const,
    branchName: "main",
    commitSha: "abcdef1234567890abcdef1234567890abcdef12",
    title: "Fix the thing",
  };
}

function pullDetailFixture(number: number, workspace?: { id: string; status: string }) {
  return {
    repo_owner: repo.owner,
    repo_name: repo.name,
    merge_request: { Number: number },
    repo: { provider: repo.provider, platform_host: repo.platformHost, repo_path: repo.repoPath },
    workspace,
  };
}

function issueDetailFixture(number: number, workspace?: { id: string; status: string }) {
  return {
    repo_owner: repo.owner,
    repo_name: repo.name,
    issue: { Number: number },
    repo: { provider: repo.provider, platform_host: repo.platformHost, repo_path: repo.repoPath },
    workspace,
  };
}

interface RenderOptions {
  drawerItem?: unknown;
  commitItem?: unknown;
  onSelectCommit?: (item: unknown) => void;
  inlineWorkspace?: InlineWorkspaceController | null;
  pullDetail?: unknown;
  issueDetail?: unknown;
  workspacePaneControls?: Snippet | undefined;
}

/** Stands in for the frontend's workspace controls button. */
const controlsDouble: Snippet = createRawSnippet(() => ({
  render: () => `<button type="button" data-testid="workspace-pane-controls">Controls</button>`,
}));

function renderActivity(options: RenderOptions = {}) {
  const pullBox = createReactiveValue(options.pullDetail ?? null);
  const issueBox = createReactiveValue(options.issueDetail ?? null);
  const stores = {
    detail: { getDetail: pullBox.get, loadDetail: vi.fn(async () => undefined) },
    issues: { getIssueDetail: issueBox.get, loadIssueDetail: vi.fn(async () => undefined) },
  };

  return {
    stores,
    pullBox,
    issueBox,
    ...render(ActivityFeedView, {
      props: {
        drawerItem: options.drawerItem ?? null,
        ...(options.commitItem !== undefined ? { commitItem: options.commitItem } : {}),
        ...(options.onSelectCommit !== undefined ? { onSelectCommit: options.onSelectCommit } : {}),
        ...(options.inlineWorkspace !== undefined ? { inlineWorkspace: options.inlineWorkspace } : {}),
        ...(options.workspacePaneControls !== undefined
          ? { workspacePaneControls: options.workspacePaneControls }
          : {}),
      },
      context: new Map<symbol, unknown>([[STORES_KEY, stores]]),
    }),
  };
}

describe("ActivityFeedView detail panes", () => {
  beforeEach(() => {
    localStorage.clear();
    resetModalStack();
    resetPaneLayoutStoresForTest();
    vi.stubGlobal(
      "MutationObserver",
      class {
        observe(): void {}
        disconnect(): void {}
        takeRecords(): MutationRecord[] {
          return [];
        }
      },
    );
    vi.stubGlobal("requestAnimationFrame", () => 1);
    vi.stubGlobal("cancelAnimationFrame", () => undefined);
  });

  afterEach(() => {
    cleanup();
    resetModalStack();
    resetPaneLayoutStoresForTest();
    localStorage.clear();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("offers the diff pane for a PR selection", () => {
    renderActivity({ drawerItem: prDrawer(), pullDetail: pullDetailFixture(12) });

    expect(screen.getByRole("tab", { name: "Conversation" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Files changed" })).toBeTruthy();
    // One body per tab in the leaf; only the active one is on screen.
    const bodies = screen.getAllByTestId("pull-detail-pane");
    expect(bodies.map((el) => el.getAttribute("data-tab-key")).sort()).toEqual(["conversation", "files"]);
    expect(bodies.find((el) => el.getAttribute("data-visible") === "true")?.getAttribute("data-tab-key")).toBe(
      "conversation",
    );
    expect(bodies.find((el) => el.getAttribute("data-tab-key") === "conversation")?.dataset.keyboardActive).toBe(
      "false",
    );
    expect(bodies.find((el) => el.getAttribute("data-tab-key") === "files")?.dataset.keyboardActive).toBe("false");
  });

  it("offers no diff pane for an issue selection", () => {
    // An issue has no diff, so the pane is unavailable and prunes out of the
    // tree rather than rendering an empty body.
    renderActivity({ drawerItem: issueDrawer(), issueDetail: issueDetailFixture(9) });

    expect(screen.getByRole("tab", { name: "Conversation" })).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Files changed" })).toBeNull();
    expect(screen.getByTestId("issue-detail")).toBeTruthy();
    expect(screen.queryByTestId("pull-detail-pane")).toBeNull();
  });

  it("moves diff keyboard routing only with live activity focus", async () => {
    renderActivity({ drawerItem: prDrawer(), pullDetail: pullDetailFixture(12) });
    const layout = getPaneLayoutStore("activity");
    layout.splitTab("files", layout.leafIDForTab("files")!, "horizontal", "after");
    await tick();

    const bodies = screen.getAllByTestId("pull-detail-pane");
    const conversation = bodies.find((body) => body.dataset.tabKey === "conversation")!;
    const files = bodies.find((body) => body.dataset.tabKey === "files")!;
    expect(conversation.dataset.keyboardActive).toBe("false");
    expect(files.dataset.keyboardActive).toBe("false");

    await fireEvent.pointerDown(files);
    await fireEvent.wheel(conversation);
    expect(conversation.dataset.keyboardActive).toBe("false");
    expect(files.dataset.keyboardActive).toBe("false");

    await fireEvent.focusIn(files);
    expect(conversation.dataset.keyboardActive).toBe("false");
    expect(files.dataset.keyboardActive).toBe("true");

    await fireEvent.focusIn(conversation);
    expect(conversation.dataset.keyboardActive).toBe("true");
    expect(files.dataset.keyboardActive).toBe("false");
  });

  it("offers the commit pane for a branch commit selection", async () => {
    const { component } = renderActivity({ drawerItem: null });
    // The feed double exposes the branch-commit callback the real feed fires.
    screen.getByTestId("select-branch-commit").click();
    await Promise.resolve();
    expect(component).toBeTruthy();

    expect(screen.getByRole("tab", { name: "Commit" })).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Conversation" })).toBeNull();
    const commitPanel = screen.getByTestId("commit-diff-panel");
    expect(commitPanel.dataset.inputActive).toBe("false");

    await fireEvent.focusIn(commitPanel);
    expect(commitPanel.dataset.inputActive).toBe("true");
  });

  it("renders a controlled commit pane", () => {
    renderActivity({ commitItem: commitDrawer() });

    expect(screen.getByRole("tab", { name: "Commit" })).toBeTruthy();
    expect(screen.getByTestId("commit-diff-panel")).toBeTruthy();
  });

  it("sends a branch-commit click to the controlled callback", async () => {
    const onSelectCommit = vi.fn();
    renderActivity({ onSelectCommit });

    screen.getByTestId("select-branch-commit").click();
    await tick();

    expect(onSelectCommit).toHaveBeenCalledWith({
      itemType: "commit",
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      branchName: "main",
      commitSha: "abcdef1234567890abcdef1234567890abcdef12",
      title: "Fix the thing",
    });
    expect(screen.queryByRole("tab", { name: "Commit" })).toBeNull();
  });

  it("renders a promoted session's pane in the activity drawer", () => {
    const layout = getPaneLayoutStore("activity");
    const paneKey = sessionPaneKey("ws-1", undefined, "ws-1:helper");
    layout.promoteTab(paneKey, { kind: "tab", leafID: layout.leafIDForTab("workspace")! });
    const { controller } = createClaimTestController("activity", {
      sessions: [{ paneKey, label: "Helper" }],
    });

    renderActivity({
      drawerItem: prDrawer(),
      inlineWorkspace: controller,
      pullDetail: pullDetailFixture(12, { id: "ws-1", status: "ready" }),
    });

    expect(document.querySelector(`[data-session-pane="${paneKey}"]`)).not.toBeNull();
    expect(screen.getByRole("tab", { name: "Helper" })).toBeTruthy();
  });

  it("reports a focused pane to the workspace host", () => {
    const layout = getPaneLayoutStore("activity");
    const paneKey = sessionPaneKey("ws-1", undefined, "ws-1:helper");
    layout.promoteTab(paneKey, { kind: "tab", leafID: layout.leafIDForTab("workspace")! });
    const { controller } = createClaimTestController("activity", {
      sessions: [{ paneKey, label: "Helper" }],
    });

    renderActivity({
      drawerItem: prDrawer(),
      inlineWorkspace: controller,
      pullDetail: pullDetailFixture(12, { id: "ws-1", status: "ready" }),
    });

    document.querySelector(`[data-pane-key="${paneKey}"]`)?.dispatchEvent(new FocusEvent("focusin", { bubbles: true }));

    // The drawer's own handler only tracks its conversation and files tabs, so the
    // report has to happen before that filter.
    expect(controller.notePaneFocused).toHaveBeenCalledWith(paneKey);
  });

  it("offers the workspace controls in the leaf holding a promoted session", () => {
    const layout = getPaneLayoutStore("activity");
    const paneKey = sessionPaneKey("ws-1", undefined, "ws-1:helper");
    // Placed directly rather than through promoteSessionBesideWorkspace: that
    // helper refuses until a container has reported the workspace pane on screen,
    // and this test is about what the view renders once the pane exists.
    layout.promoteTab(paneKey, {
      kind: "split",
      leafID: layout.leafIDForTab("workspace")!,
      direction: "horizontal",
      placement: "after",
    });
    const { controller } = createClaimTestController("activity", {
      sessions: [{ paneKey, label: "Helper" }],
    });

    renderActivity({
      drawerItem: prDrawer(),
      inlineWorkspace: controller,
      pullDetail: pullDetailFixture(12, { id: "ws-1", status: "ready" }),
      workspacePaneControls: controlsDouble,
    });

    // Each surface wires this separately, so each needs its own proof: the
    // workspace leaf and the promoted session's leaf get the button, the leaf of
    // route panes does not.
    expect(screen.getAllByTestId("workspace-pane-controls")).toHaveLength(2);
    expect(
      document
        .querySelector('[data-pane-key="conversation"]')
        ?.closest(".tabbed-panel-leaf")
        ?.querySelector('[data-testid="workspace-pane-controls"]'),
    ).toBeNull();
  });
  it("claims through the issue store when the selection is an issue", () => {
    const { controller } = createClaimTestController("activity");
    renderActivity({
      drawerItem: issueDrawer(),
      inlineWorkspace: controller,
      // A PR detail for the same repo and number must not satisfy an issue
      // selection: a PR and an issue can share both and own unrelated
      // workspaces.
      pullDetail: pullDetailFixture(9, { id: "ws-pr", status: "ready" }),
      issueDetail: issueDetailFixture(9, { id: "ws-issue", status: "ready" }),
    });

    expect(controller.claim).toHaveBeenCalledWith(expect.objectContaining({ number: 9, itemType: "issue" }), {
      id: "ws-issue",
      status: "ready",
    });
  });

  it("keeps the workspace slot mounted across a PR to issue selection change", async () => {
    const { controller } = createClaimTestController("activity");
    const { rerender } = renderActivity({
      drawerItem: prDrawer(12),
      inlineWorkspace: controller,
      pullDetail: pullDetailFixture(12, { id: "ws-1", status: "ready" }),
      issueDetail: issueDetailFixture(9, { id: "ws-1", status: "ready" }),
    });

    const slot = document.querySelector(".detail-pane-workspace-slot");
    expect(slot).toBeTruthy();

    // One pane tree spans both selection kinds, so the slot element survives the
    // switch and the live terminal is never reparented for it.
    await rerender({ drawerItem: issueDrawer(9), inlineWorkspace: controller });

    expect(document.querySelector(".detail-pane-workspace-slot")).toBe(slot);
  });

  it("restores a customized arrangement after a selection kind removed one of its panes", async () => {
    // The stored tree keeps unavailable panes; only the render prunes them. That
    // is what lets an arrangement made against a PR come back after an issue,
    // which contributes no diff pane at all, has been selected in between.
    const layout = getPaneLayoutStore("activity");
    const { rerender } = renderActivity({
      drawerItem: prDrawer(12),
      pullDetail: pullDetailFixture(12),
      issueDetail: issueDetailFixture(9),
    });

    const detailLeaf = layout.leafIDForTab("conversation");
    layout.splitTab("files", detailLeaf!, "horizontal", "after");
    const filesLeaf = layout.leafIDForTab("files");
    expect(filesLeaf).not.toBe(detailLeaf);
    layout.toggleZoom(filesLeaf!);

    await rerender({ drawerItem: issueDrawer(9) });

    // The diff has no place in an issue, so it renders nothing and its zoom is
    // dropped rather than blanking the surface.
    expect(screen.queryByRole("tab", { name: "Files changed" })).toBeNull();
    expect(layout.effectiveZoomedLeafID(["conversation"])).toBeNull();

    await rerender({ drawerItem: prDrawer(12) });

    expect(screen.getByRole("tab", { name: "Files changed" })).toBeTruthy();
    expect(layout.leafIDForTab("files")).toBe(filesLeaf);
    expect(layout.leafIDForTab("conversation")).toBe(detailLeaf);
  });
});
