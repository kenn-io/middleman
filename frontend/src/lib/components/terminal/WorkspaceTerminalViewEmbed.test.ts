// Pins the embed-only props on WorkspaceTerminalView so a refactor that
// loses the conditional rendering around the workspace list column or the
// right detail sidebar fails loudly rather than silently breaking
// embedders that mount the surface via /workspaces/embed/terminal.
//
// Lives in its own file because the broader WorkspaceTerminalView test
// suite stubs globalThis.fetch *after* the runtime client module has
// captured it; that's a pre-existing test-infrastructure issue
// (introduced in #182) which affects neither this branch nor the embed
// props themselves. Mocking the api/runtime module here avoids the
// captured-fetch problem entirely.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { Effect } from "effect";
import { afterAll, afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { ProblemBody } from "../../api/problems.js";
import { GeneratedProblemResponse } from "../../api/runtime.js";
import type { OwnedAppRuntime } from "../../app/runtime.js";
import { pushModalFrame, resetModalStack } from "../../stores/keyboard/modal-stack.svelte.js";

const mocks = vi.hoisted(() => ({
  runtimeClient: {
    getWorkspace: vi.fn(),
    refreshWorkspace: vi.fn(),
  },
  showFlash: vi.fn(),
  workspaceEventsSubscriber: undefined as ((event: unknown) => void) | undefined,
}));
const runtimeState = vi.hoisted<{ appRuntime?: OwnedAppRuntime }>(() => ({}));

function problem(status: number, code: ProblemBody["code"], detail: string): ProblemBody {
  return {
    type: "about:blank",
    title: "Workspace request failed",
    status,
    detail,
    code,
  };
}

function failedProblem(status: number, code: ProblemBody["code"], detail: string): GeneratedProblemResponse {
  return new GeneratedProblemResponse(problem(status, code, detail), new Response(null, { status }));
}

vi.mock("../../app/runtime-context.js", async () => {
  const { makeAppRuntime } = await import("../../app/runtime.js");
  const { makeGeneratedClient } = await import("../../testing/generated-client.js");
  runtimeState.appRuntime = makeAppRuntime(
    makeGeneratedClient({
      WorkspacesService: {
        getWorkspace: mocks.runtimeClient.getWorkspace,
        refreshWorkspace: mocks.runtimeClient.refreshWorkspace,
      },
    }),
  );
  return { getAppRuntime: () => runtimeState.appRuntime };
});

vi.mock("../../stores/flash.svelte.js", () => ({
  showFlash: mocks.showFlash,
}));

vi.mock("../../api/workspace-runtime.js", () => ({
  workspaceSessionWebSocketPath: () => "",
  workspaceTmuxWebSocketPath: () => "",
}));

// Stub xterm so the terminal panes don't try to render in jsdom.
vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn().mockImplementation(function () {
    return {
      cols: 80,
      rows: 24,
      attachCustomKeyEventHandler: vi.fn(),
      open: vi.fn(),
      focus: vi.fn(),
      loadAddon: vi.fn(),
      onData: vi.fn(),
      onBinary: vi.fn(),
      dispose: vi.fn(),
      write: vi.fn(),
      refresh: vi.fn(),
      clearTextureAtlas: vi.fn(),
      options: {},
    };
  }),
}));

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(function () {
    // The pane measures its own region through the addon; a real one proposes
    // nothing for a container with no content box.
    return { fit: vi.fn(), proposeDimensions: () => ({ cols: 80, rows: 24 }) };
  }),
}));

vi.mock("@xterm/addon-webgl", () => ({
  WebglAddon: vi.fn().mockImplementation(function () {
    return {};
  }),
}));

vi.mock("../../context.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../context.js")>();
  return {
    ...actual,
    getStores: () => ({
      events: {
        selectWorkspace: () => () => undefined,
        subscribeWorkspaceEvents: (subscriber: (event: unknown) => void) => {
          mocks.workspaceEventsSubscriber = subscriber;
          return () => {
            if (mocks.workspaceEventsSubscriber === subscriber) mocks.workspaceEventsSubscriber = undefined;
          };
        },
      },
      settings: {
        getTerminalSettings: () => ({
          font_family: "",
          font_size: 14,
          scrollback: 1000,
          line_height: 1,
          letter_spacing: 0,
          cursor_blink: true,
          font_ligatures: false,
        }),
        setTerminalSettings: vi.fn(),
        getModeVisibility: () => ({
          activity: true,
          repos: true,
          docs: false,
          pulls: true,
          issues: true,
          reviews: true,
          workspaces: true,
        }),
        setModeVisibility: vi.fn(),
        getTerminalFontFamily: () => "",
        getTerminalFontSize: () => 14,
        getTerminalScrollback: () => 1000,
        getTerminalLineHeight: () => 1,
        getTerminalLetterSpacing: () => 0,
        getTerminalCursorBlink: () => true,
        getTerminalFontLigatures: () => false,
        getWorkspaceSettings: () => ({
          auto_assign_on_create: false,
          default_sidebar_view: "diff",
        }),
      },
    }),
  };
});

import WorkspaceTerminalView from "./WorkspaceTerminalView.svelte";

const readyWorkspaceData = {
  id: "ws-1",
  platform_host: "github.com",
  repo_owner: "acme",
  repo_name: "widget",
  repo: {
    provider: "github",
    platform_host: "github.com",
    owner: "acme",
    name: "widget",
    repo_path: "acme/widget",
  },
  item_type: "pull_request",
  item_number: 7,
  git_head_ref: "feature/embed-props",
  worktree_path: "/tmp/worktree",
  tmux_session: "kenn-forge-ws-1",
  status: "ready",
  enrichment_status: "fresh",
  created_at: "2026-04-29T00:00:00Z",
};

const readyIssueWorkspaceData = {
  ...readyWorkspaceData,
  item_type: "issue",
  item_number: 9,
  associated_pr_number: null,
};

describe("WorkspaceTerminalView embed props", () => {
  afterAll(async () => {
    if (runtimeState.appRuntime !== undefined) {
      await Effect.runPromise(runtimeState.appRuntime.disposeEffect);
    }
  });

  beforeEach(() => {
    mocks.runtimeClient.getWorkspace.mockReset();
    mocks.runtimeClient.refreshWorkspace.mockReset();
    mocks.showFlash.mockReset();
    mocks.workspaceEventsSubscriber = undefined;
    mocks.runtimeClient.getWorkspace.mockResolvedValue(readyWorkspaceData);

    vi.stubGlobal(
      "EventSource",
      class {
        addEventListener(): void {}
        close(): void {}
      },
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    );
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      callback(0);
      return 1;
    });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("hides the workspace list column when hideWorkspaceList is true", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        hideWorkspaceList: true,
      },
    });

    // Wait for the header branch element that only renders once the
    // workspace payload resolves; this confirms the component reached
    // steady state rather than failing the load early.
    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    // The workspace-list column header reads "Workspaces"; with
    // hideWorkspaceList the entire column is skipped so the heading
    // must not be in the DOM.
    expect(screen.queryByText("Workspaces")).toBeNull();
  });

  it("renders the workspace list column by default", async () => {
    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
    });

    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    expect(screen.queryByText("Workspaces")).not.toBeNull();
  });

  it("hides the PR/Reviews segmented control when hideRightSidebar is true", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        hideWorkspaceList: true,
        hideRightSidebar: true,
      },
    });

    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    expect(screen.queryByRole("button", { name: "PR" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reviews" })).toBeNull();
  });

  it("renders the PR/Reviews segmented control by default", async () => {
    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
    });

    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    expect(screen.getByRole("button", { name: "PR" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reviews" })).toBeTruthy();
  });

  it("refreshes workspace details and reveals a newly associated PR", async () => {
    mocks.runtimeClient.getWorkspace.mockResolvedValue(readyIssueWorkspaceData);
    mocks.runtimeClient.refreshWorkspace.mockResolvedValue({ ...readyIssueWorkspaceData, associated_pr_number: 42 });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        hideWorkspaceList: true,
      },
    });

    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));
    expect(screen.queryByRole("button", { name: "PR" })).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: "Refresh workspace details" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "PR" })).toBeTruthy());
    expect(mocks.runtimeClient.refreshWorkspace).toHaveBeenCalledWith(
      { id: "ws-1" },
      { signal: expect.any(AbortSignal) },
    );
  });

  it("shows a flash when workspace detail refresh fails", async () => {
    mocks.runtimeClient.getWorkspace.mockResolvedValue(readyIssueWorkspaceData);
    mocks.runtimeClient.refreshWorkspace.mockRejectedValue(
      failedProblem(503, "serviceUnavailable", "temporarily unavailable"),
    );

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        hideWorkspaceList: true,
      },
    });

    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    await fireEvent.click(screen.getByRole("button", { name: "Refresh workspace details" }));

    await waitFor(() => {
      expect(mocks.showFlash).toHaveBeenCalledWith("temporarily unavailable", {
        tone: "danger",
      });
    });
  });

  it("reports a 404 workspace load as a deletion so cached refs clear", async () => {
    // A 404 is authoritative absence: the workspace was deleted by
    // another client. Without reporting it, created-records and
    // overrides keep advertising the dead ID indefinitely.
    mocks.runtimeClient.getWorkspace.mockRejectedValue(failedProblem(404, "workspaceNotFound", "workspace not found"));
    const onWorkspaceDeleted = vi.fn();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", hideWorkspaceList: true, onWorkspaceDeleted },
    });

    await waitFor(() => {
      // No cached envelope yet, so no identity snapshot to report.
      expect(onWorkspaceDeleted).toHaveBeenCalledWith("ws-1", undefined, undefined);
    });
  });

  it("a 404 after a successful load clears the cached workspace and reports the identity", async () => {
    // The workspace was deleted by another client mid-session. Liveness
    // rendering keys off the cached envelope, so without clearing it the
    // route would keep showing the deleted workspace; and the deletion
    // callback needs the identity snapshot to tombstone controller-less
    // cached detail.
    const workspaceWithRepo = {
      ...readyWorkspaceData,
      repo: {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widget",
        repo_path: "acme/widget",
      },
    };
    let gone = false;
    mocks.runtimeClient.getWorkspace.mockImplementation(async () => {
      if (gone) {
        throw failedProblem(404, "workspaceNotFound", "workspace not found");
      }
      return workspaceWithRepo;
    });
    const onWorkspaceDeleted = vi.fn();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", hideWorkspaceList: true, onWorkspaceDeleted },
    });
    await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

    gone = true;
    await waitFor(() => expect(mocks.workspaceEventsSubscriber).toBeTypeOf("function"));
    mocks.workspaceEventsSubscriber?.({ type: "workspace_status", payload: { id: "ws-1" } });

    await waitFor(() => {
      expect(onWorkspaceDeleted).toHaveBeenCalledWith(
        "ws-1",
        undefined,
        expect.objectContaining({ provider: "github", owner: "acme", name: "widget", number: 7 }),
      );
    });
    // The dead cached envelope must not keep rendering as live.
    await waitFor(() => {
      expect(screen.queryAllByText("feature/embed-props")).toHaveLength(0);
      expect(screen.getAllByText("workspace not found").length).toBeGreaterThan(0);
    });
  });

  it("a transient workspace load failure is not treated as a deletion", async () => {
    mocks.runtimeClient.getWorkspace.mockRejectedValue(failedProblem(500, "upstreamError", "upstream boom"));
    const onWorkspaceDeleted = vi.fn();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", hideWorkspaceList: true, onWorkspaceDeleted },
    });

    await waitFor(() => {
      expect(screen.getAllByText("upstream boom").length).toBeGreaterThan(0);
    });
    expect(onWorkspaceDeleted).not.toHaveBeenCalled();
  });

  describe("inlineDock toolbar controls", () => {
    afterEach(() => {
      resetModalStack();
    });

    it("renders no inline dock buttons without an inlineDock prop", async () => {
      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", hideWorkspaceList: true },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      expect(screen.queryByRole("button", { name: "Expand Terminal" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Collapse Terminal" })).toBeNull();
    });

    it("renders the toggle and collapse buttons in the same toolbar container as Delete", async () => {
      const inlineDock = { getMode: () => "split" as const, setMode: vi.fn() };
      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", hideWorkspaceList: true, inlineDock },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      const deleteButton = screen.getByRole("button", { name: "Delete" });
      const container = deleteButton.closest(".header-end");
      expect(container).toBeTruthy();
      const scoped = within(container as HTMLElement);
      expect(scoped.getByRole("button", { name: "Expand Terminal" })).toBeTruthy();
      expect(scoped.getByRole("button", { name: "Collapse Terminal" })).toBeTruthy();
    });

    it("flips the toggle label with mode and drives setMode through it", async () => {
      const setMode = vi.fn();
      const { rerender } = render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      await fireEvent.click(screen.getByRole("button", { name: "Expand Terminal" }));
      expect(setMode).toHaveBeenCalledWith("expanded");

      await rerender({
        workspaceId: "ws-1",
        hideWorkspaceList: true,
        inlineDock: { getMode: () => "expanded" as const, setMode },
      });

      expect(screen.queryByRole("button", { name: "Expand Terminal" })).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "Show Details" }));
      expect(setMode).toHaveBeenCalledWith("split");
    });

    it("collapses via the inline dock collapse button", async () => {
      const setMode = vi.fn();
      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      await fireEvent.click(screen.getByRole("button", { name: "Collapse Terminal" }));
      expect(setMode).toHaveBeenCalledWith("collapsed");
    });

    it("keeps a collapse control reachable while the workspace is still creating", async () => {
      // The toolbar that carries the dock controls only renders once the
      // workspace is ready; without a state-level control a slow setup
      // would leave the inline dock impossible to close.
      mocks.runtimeClient.getWorkspace.mockResolvedValue({ ...readyWorkspaceData, status: "creating" });
      const setMode = vi.fn();
      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getByText("Setting up workspace...")).toBeTruthy());

      await fireEvent.click(screen.getByRole("button", { name: "Collapse Terminal" }));
      expect(setMode).toHaveBeenCalledWith("collapsed");
    });

    it("keeps a collapse control reachable after workspace setup fails", async () => {
      mocks.runtimeClient.getWorkspace.mockResolvedValue({
        ...readyWorkspaceData,
        status: "error",
        error_message: "clone failed",
      });
      const setMode = vi.fn();
      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getByText("clone failed")).toBeTruthy());

      await fireEvent.click(screen.getByRole("button", { name: "Collapse Terminal" }));
      expect(setMode).toHaveBeenCalledWith("collapsed");
    });

    it("keeps a collapse control reachable when the workspace fetch fails", async () => {
      mocks.runtimeClient.getWorkspace.mockRejectedValue(failedProblem(500, "upstreamError", "boom"));
      const setMode = vi.fn();
      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getByText("boom")).toBeTruthy());

      await fireEvent.click(screen.getByRole("button", { name: "Collapse Terminal" }));
      expect(setMode).toHaveBeenCalledWith("collapsed");
    });

    it("a failed in-place workspace switch shows the error state, not the stale toolbar", async () => {
      // Switching the inline dock from A to B keeps A cached while B
      // loads. When B's fetch fails, the error state (with its retry and
      // collapse controls) must render instead of A's ready toolbar,
      // which would be a stale header whose action guards leave the dock
      // uncollapsible.
      mocks.runtimeClient.getWorkspace.mockImplementation(async ({ id }: { id: string }) => {
        if (id === "ws-2") {
          throw failedProblem(500, "upstreamError", "boom");
        }
        return readyWorkspaceData;
      });
      const setMode = vi.fn();
      const inlineDock = { getMode: () => "split" as const, setMode };
      const { rerender } = render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", hideWorkspaceList: true, inlineDock },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      await rerender({ workspaceId: "ws-2", hideWorkspaceList: true, inlineDock });

      await waitFor(() => expect(screen.getByText("boom")).toBeTruthy());
      expect(screen.queryByText("feature/embed-props")).toBeNull();

      const collapse = screen.getByRole("button", { name: "Collapse Terminal" });
      expect(collapse.hasAttribute("disabled")).toBe(false);
      await fireEvent.click(collapse);
      expect(setMode).toHaveBeenCalledWith("collapsed");
    });

    it("a slow in-place workspace switch shows the loading state, not the stale toolbar", async () => {
      mocks.runtimeClient.getWorkspace.mockImplementation(({ id }: { id: string }) => {
        if (id === "ws-2") {
          return new Promise(() => {});
        }
        return Promise.resolve(readyWorkspaceData);
      });
      const inlineDock = { getMode: () => "split" as const, setMode: vi.fn() };
      const { rerender } = render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", hideWorkspaceList: true, inlineDock },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      await rerender({ workspaceId: "ws-2", hideWorkspaceList: true, inlineDock });

      await waitFor(() => expect(screen.getByText("Setting up workspace...")).toBeTruthy());
      expect(screen.queryByText("feature/embed-props")).toBeNull();
      expect(screen.getByRole("button", { name: "Collapse Terminal" })).toBeTruthy();
    });

    it("shows no collapse control in setup states without an inlineDock prop", async () => {
      mocks.runtimeClient.getWorkspace.mockResolvedValue({ ...readyWorkspaceData, status: "creating" });
      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", hideWorkspaceList: true },
      });

      await waitFor(() => expect(screen.getByText("Setting up workspace...")).toBeTruthy());

      expect(screen.queryByRole("button", { name: "Collapse Terminal" })).toBeNull();
    });

    it("disables the expand direction while a modal frame is open", async () => {
      const setMode = vi.fn();
      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          hideWorkspaceList: true,
          inlineDock: { getMode: () => "split" as const, setMode },
        },
      });

      await waitFor(() => expect(screen.getAllByText("feature/embed-props").length).toBeGreaterThan(0));

      const expandButton = screen.getByRole("button", { name: "Expand Terminal" });
      expect(expandButton.hasAttribute("disabled")).toBe(false);

      const pop = pushModalFrame("test-modal", []);
      await waitFor(() =>
        expect(screen.getByRole("button", { name: "Expand Terminal" }).hasAttribute("disabled")).toBe(true),
      );

      pop();
      await waitFor(() =>
        expect(screen.getByRole("button", { name: "Expand Terminal" }).hasAttribute("disabled")).toBe(false),
      );
    });
  });
});
