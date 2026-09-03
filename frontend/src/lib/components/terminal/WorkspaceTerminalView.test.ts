import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { Effect } from "effect";
import { flushSync } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { RuntimeSession } from "../../api/types.js";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";
import { createDiffStore } from "../../stores/diff.svelte.js";
import { clearActiveTabbedPanelDrag, startTabbedPanelTabDrag } from "../shared/tabbed-panel-drag.js";
import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";
import {
  getPaneLayoutStore,
  promoteSessionBesideWorkspace,
  resetPaneLayoutStoresForTest,
} from "../../stores/paneLayout.svelte.js";
import { sessionPaneKey } from "../../stores/session-pane-key.js";
import {
  beginWorkspaceCreate,
  beginWorkspaceDeletion,
  discardWorkspaceLaunch,
  endWorkspaceDeletion,
  pendingWorkspaceLaunch,
  promoteWorkspaceCreateLaunch,
  queueWorkspaceLaunch,
  resetWorkspaceCreatePendingForTest,
} from "../../stores/workspace-create-pending.svelte.js";
import type { WorkspaceItemIdentity } from "../../workspace-inline.js";
import { STORES_KEY } from "../../context.js";

const mocks = vi.hoisted(() => ({
  getWorkspaceRuntime: vi.fn(),
  launchWorkspaceSession: vi.fn(),
  mockDispose: vi.fn(),
  mockFit: vi.fn(),
  mockLoadAddon: vi.fn(),
  mockOnData: vi.fn(),
  mockOpen: vi.fn(),
  mockSetTerminalSettings: vi.fn(),
  mockTerminalInstances: [] as Array<{
    buffer: { active: { baseY: number; type: "normal" | "alternate" } };
    focus: ReturnType<typeof vi.fn>;
    modes: {
      applicationCursorKeysMode: boolean;
      bracketedPasteMode: boolean;
      mouseTrackingMode: "none" | "x10" | "vt200" | "drag" | "any";
    };
    options: Record<string, unknown>;
  }>,
  mockUpdateSettings: vi.fn(),
  renameWorkspaceSession: vi.fn(),
  runtime: undefined as unknown as OwnedAppRuntime,
  selectWorkspace: vi.fn(),
  showFlash: vi.fn(),
  stopWorkspaceSession: vi.fn(),
  subscribeWorkspaceEvents: vi.fn(),
  terminalWrite: vi.fn(),
  diffStore: null as unknown as ReturnType<typeof createDiffStore>,
  workspaceSidebarPreference: "diff" as "diff" | "item",
}));

let sockets: MockWebSocket[] = [];

class MockWebSocket extends EventTarget {
  static OPEN = 1;
  readyState = 1;
  binaryType = "arraybuffer";
  runListenersAttached = false;
  onopen = () => this.dispatchEvent(new Event("open"));
  onmessage = (event: MessageEvent) => this.dispatchEvent(event);
  onclose = (event: CloseEvent) => this.dispatchEvent(event);
  onerror = () => this.dispatchEvent(new Event("error"));

  constructor(public url: string) {
    super();
    sockets.push(this);
  }

  addEventListener(
    type: string,
    callback: EventListenerOrEventListenerObject | null,
    options?: boolean | AddEventListenerOptions,
  ): void {
    if (type === "message") this.runListenersAttached = true;
    super.addEventListener(type, callback, options);
  }

  send = vi.fn();
  close = vi.fn();
}

vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn().mockImplementation(function (options) {
    const terminal = {
      buffer: { active: { baseY: 0, type: "normal" as const } },
      cols: 80,
      rows: 24,
      attachCustomKeyEventHandler: vi.fn(),
      clearTextureAtlas: vi.fn(),
      dispose: mocks.mockDispose,
      focus: vi.fn(),
      loadAddon: mocks.mockLoadAddon,
      modes: {
        applicationCursorKeysMode: false,
        bracketedPasteMode: false,
        mouseTrackingMode: "none" as const,
      },
      onBinary: vi.fn(),
      onData: mocks.mockOnData,
      open: mocks.mockOpen,
      parser: {
        registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })),
      },
      refresh: vi.fn(),
      write: mocks.terminalWrite,
      options: { ...options },
    };
    mocks.mockTerminalInstances.push(terminal);
    return terminal;
  }),
}));

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(function () {
    return {
      fit: mocks.mockFit,
      // The pane measures its own region through the addon; a real one
      // proposes nothing for a container with no content box.
      proposeDimensions: () => ({ cols: 80, rows: 24 }),
    };
  }),
}));

vi.mock("@xterm/addon-webgl", () => ({
  WebglAddon: vi.fn().mockImplementation(function () {
    return {
      dispose: vi.fn(),
      onContextLoss: vi.fn(),
    };
  }),
}));

vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

vi.mock("../../context.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../context.js")>();
  return {
    ...actual,
    getStores: () => ({
      settings: {
        getTerminalSettings: () => ({
          font_family: "",
          font_size: 14,
          scrollback: 1000,
          line_height: 1,
          letter_spacing: 0,
          cursor_blink: true,
          font_ligatures: false,
          hide_tmux_status: false,
          graphics: true,
          tmux_mouse: true,
          retained_sessions: 10,
        }),
        setTerminalSettings: mocks.mockSetTerminalSettings,
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
        getTerminalGraphics: () => true,
        getWorkspaceSettings: () => ({
          auto_assign_on_create: false,
          default_sidebar_view: mocks.workspaceSidebarPreference,
        }),
      },
      diff: mocks.diffStore,
      events: {
        selectWorkspace: mocks.selectWorkspace,
        subscribeWorkspaceEvents: mocks.subscribeWorkspaceEvents,
      },
    }),
  };
});

vi.mock("../../app/runtime-context.js", () => ({
  getAppRuntime: () => mocks.runtime,
}));

vi.mock("../../api/generated-api.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/generated-api.js")>();
  const { GeneratedProblemResponse } = await import("../../api/runtime.js");
  const { makeGeneratedClient } = await import("../../testing/generated-client.js");
  const getRuntime = async (workspaceId: string, hostKey?: string) => {
    const result = await mocks.getWorkspaceRuntime(workspaceId, hostKey);
    if (result instanceof Response) throw new GeneratedProblemResponse(await result.json(), result);
    return result;
  };
  const client = makeGeneratedClient({
    WorkspacesService: {
      getWorkspaceRuntime: ({ id }: { id: string }) => getRuntime(id),
      launchWorkspaceRuntimeSession: (
        { id }: { id: string },
        body: { target_key: string; display_region?: "workflow" | "terminal" },
      ) => mocks.launchWorkspaceSession(id, body.target_key, { region: body.display_region }),
      renameWorkspaceRuntimeSession: (
        { id, sessionKey }: { id: string; sessionKey: string },
        body: { label: string },
      ) => mocks.renameWorkspaceSession(id, sessionKey, body.label, undefined),
      stopWorkspaceRuntimeSession: ({ id, sessionKey }: { id: string; sessionKey: string }) =>
        mocks.stopWorkspaceSession(id, sessionKey, undefined),
    },
    FleetService: {
      getFleetWorkspaceRuntime: ({ hostKey, id }: { hostKey: string; id: string }) => getRuntime(id, hostKey),
      launchFleetWorkspaceRuntimeSession: (
        { hostKey, id }: { hostKey: string; id: string },
        body: { target_key: string; display_region?: "workflow" | "terminal" },
      ) => mocks.launchWorkspaceSession(id, body.target_key, { hostKey, region: body.display_region }),
      renameFleetWorkspaceRuntimeSession: (
        { hostKey, id, sessionKey }: { hostKey: string; id: string; sessionKey: string },
        body: { label: string },
      ) => mocks.renameWorkspaceSession(id, sessionKey, body.label, hostKey),
      stopFleetWorkspaceRuntimeSession: ({
        hostKey,
        id,
        sessionKey,
      }: {
        hostKey: string;
        id: string;
        sessionKey: string;
      }) => mocks.stopWorkspaceSession(id, sessionKey, hostKey),
    },
  });
  return {
    ...actual,
    GeneratedApiLive: actual.makeGeneratedApiLayer(client),
  };
});

vi.mock("../../api/workspace-runtime.js", () => ({
  workspaceSessionWebSocketPath: (workspaceId: string, sessionKey: string) =>
    `/ws/v1/workspaces/${workspaceId}/runtime/sessions/${sessionKey}/terminal`,
  workspaceTmuxWebSocketPath: (workspaceId: string) => `/ws/v1/workspaces/${workspaceId}/terminal`,
}));

vi.mock("../../stores/flash.svelte.js", () => ({
  showFlash: mocks.showFlash,
}));

vi.mock("../detail/PullDetail.svelte", async () => ({
  default: (await import("../../views/PRListViewTestPullDetail.svelte")).default,
}));

vi.mock("../detail/IssueDetail.svelte", async () => ({
  default: (await import("../../views/IssueListViewTestIssueDetail.svelte")).default,
}));

vi.mock("../kata/KataLinksPanel.svelte", async () => ({
  default: (await import("../../views/KataLinksPanelTestDouble.svelte")).default,
}));

// The harness pairs the view with the session terminal pool, which WorkspaceHost
// mounts in the app. Terminals live in the pool now, so the view on its own
// renders portal slots and no terminal would ever appear.
import WorkspaceTerminalView from "./WorkspaceTerminalViewTestHarness.svelte";
import WorkspacePaneControls from "./WorkspacePaneControls.svelte";
import {
  isSessionClaimed,
  mountedSessions,
  resetSessionHostForTest,
  sessionHostPrefix,
} from "../../stores/session-host.svelte.ts";
import {
  activeHostedSession,
  getInlineWorkspaceController,
  hostedWorkspaceLauncher,
  hostedWorkspaceControls,
  workspaceControlsBusy,
  resetWorkspaceHostForTest,
} from "../../stores/workspace-host.svelte.ts";
import { navigate } from "../../stores/router.svelte.ts";

const runningSession = {
  key: "ws-1:helper",
  workspace_id: "ws-1",
  target_key: "helper",
  label: "Helper",
  kind: "agent",
  status: "running",
  display_region: "workflow",
  created_at: "2026-04-29T00:00:00Z",
} satisfies RuntimeSession;

const reviewerSession = {
  ...runningSession,
  key: "ws-1:reviewer",
  target_key: "reviewer",
  label: "Reviewer",
  created_at: "2026-04-29T00:01:00Z",
} satisfies RuntimeSession;

const duplicateAgentSession = {
  ...runningSession,
  key: "ws-1:helper-b",
  target_key: "helper",
  label: "Helper 2",
  created_at: "2026-04-29T00:02:00Z",
};

const runningShellSession = {
  key: "ws-1_shell_a",
  workspace_id: "ws-1",
  target_key: "plain_shell",
  label: "Shell",
  kind: "plain_shell",
  status: "running",
  display_region: "terminal",
  created_at: "2026-04-29T00:00:00Z",
} satisfies RuntimeSession;

const relaunchedShellSession = {
  ...runningShellSession,
  key: "ws-1_shell_b",
  created_at: "2026-04-29T00:01:00Z",
};

const workspaceResponse = {
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
  git_head_ref: "feature/session-exit",
  worktree_path: "/tmp/worktree",
  tmux_session: "kenn-forge-ws-1",
  status: "ready",
  enrichment_status: "fresh",
  created_at: "2026-04-29T00:00:00Z",
  mr_head_repo_kind: "same_repo",
};

const workspaceItemIdentity: WorkspaceItemIdentity = {
  provider: workspaceResponse.repo.provider,
  platformHost: workspaceResponse.repo.platform_host,
  owner: workspaceResponse.repo.owner,
  name: workspaceResponse.repo.name,
  repoPath: workspaceResponse.repo.repo_path,
  number: workspaceResponse.item_number,
  itemType: workspaceResponse.item_type,
};

/**
 * Serve any workspace id, not just ws-1.
 *
 * The default stub answers for ws-1 alone, so a test that switches the view to
 * another workspace gets a body with no id, the view never reports that workspace
 * live, and nothing renders - which makes "the overlay is gone" pass for the wrong
 * reason.
 */
function serveAnyWorkspace(): void {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation((input: Request | URL | string) => {
      const url = input instanceof Request ? input.url : String(input);
      const { pathname } = new URL(url, "http://localhost");
      const match = /\/workspaces\/([^/]+)$/.exec(pathname);
      if (match) {
        return Promise.resolve(
          Response.json({ ...workspaceResponse, id: match[1], tmux_session: `kenn-forge-${match[1]}` }),
        );
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    }),
  );
}

function runtimeWithSession(createdAt: string) {
  return {
    launch_targets: [],
    sessions: [
      {
        ...runningSession,
        created_at: createdAt,
      },
    ],
  };
}

/** A workspace that can launch but is running nothing yet. */
function runtimeWithLaunchTargetsOnly() {
  return {
    launch_targets: [
      {
        key: "helper",
        label: "Helper",
        kind: "agent",
        source: "config",
        available: true,
      },
    ],
    sessions: [],
  };
}

function runtimeWithStaleSession() {
  return {
    launch_targets: [
      {
        key: "helper",
        label: "Helper",
        kind: "agent",
        source: "config",
        available: true,
      },
    ],
    sessions: [runningSession],
  };
}

function runtimeWithCodexTarget(available = true, sessions: Array<Record<string, unknown>> = []) {
  return {
    launch_targets: [
      {
        key: "codex",
        label: "Codex",
        kind: "agent",
        source: "builtin",
        available,
        ...(available ? {} : { disabled_reason: "Codex is not configured" }),
      },
    ],
    sessions,
  };
}

function runtimeWithTwoWorkflowSessions() {
  return {
    launch_targets: [],
    sessions: [runningSession, reviewerSession],
  };
}

function fetchPath(input: Request | URL | string): string {
  const url = input instanceof Request ? input.url : String(input);
  return new URL(url, "http://localhost").pathname;
}

function runtimeWithDuplicateWorkflowSessions() {
  return {
    launch_targets: [],
    sessions: [runningSession, duplicateAgentSession],
  };
}

function runtimeWithTerminalSession(session = runningShellSession) {
  return {
    launch_targets: [],
    sessions: [session],
  };
}

function runtimeWithTwoTerminalSessions() {
  return {
    launch_targets: [],
    sessions: [
      runningShellSession,
      {
        ...relaunchedShellSession,
        label: "Shell 2",
      },
    ],
  };
}

function persistedTerminalLayout(workflowMode: "tabs" | "grid") {
  return JSON.stringify({
    version: 1,
    open: false,
    dock: "bottom",
    height: 300,
    activeSessionKey: null,
    tree: null,
    sessionRegions: {},
    workflowMode,
    workflowTree: null,
    customSessionLabels: {},
  });
}

/** Home and one session tab in leaves of their own, so a demotion has a
 *  placement it could plausibly lose. */
function persistedSplitWorkflowLayout(sessionKey: string, region: "workflow" | "terminal" = "workflow") {
  return JSON.stringify({
    version: 1,
    open: region === "terminal",
    dock: "bottom",
    height: 300,
    activeSessionKey: region === "terminal" ? sessionKey : null,
    tree: region === "terminal" ? { type: "leaf", id: "dock-leaf", sessionKey } : null,
    sessionRegions: { [sessionKey]: region },
    workflowMode: "tabs",
    workflowTree: {
      type: "split",
      id: "wf-split",
      direction: "horizontal",
      ratio: 0.5,
      first: { type: "leaf", id: "wf-home", tabs: ["home"], activeTabKey: "home" },
      second:
        region === "workflow"
          ? { type: "leaf", id: "wf-session", tabs: [`session:${sessionKey}`], activeTabKey: `session:${sessionKey}` }
          : { type: "leaf", id: "wf-session", tabs: ["home"], activeTabKey: "home" },
    },
    customSessionLabels: {},
  });
}

/**
 * Two workflow sessions, each in its own leaf. A detail pane has no Home tab, so a
 * second session is what gives the strip something to render and a demotion
 * somewhere to land.
 */
function persistedTwoSessionWorkflowLayout(firstKey: string, secondKey: string) {
  return JSON.stringify({
    version: 1,
    open: false,
    dock: "bottom",
    height: 300,
    activeSessionKey: null,
    tree: null,
    sessionRegions: { [firstKey]: "workflow", [secondKey]: "workflow" },
    workflowMode: "tabs",
    workflowTree: {
      type: "split",
      id: "wf-split",
      direction: "horizontal",
      ratio: 0.5,
      first: {
        type: "leaf",
        id: "wf-first",
        tabs: [`session:${firstKey}`],
        activeTabKey: `session:${firstKey}`,
      },
      second: {
        type: "leaf",
        id: "wf-second",
        tabs: [`session:${secondKey}`],
        activeTabKey: `session:${secondKey}`,
      },
    },
    customSessionLabels: {},
  });
}

/**
 * Put the workspace on the PRs detail surface, the way the app does before an
 * embedded view exists: the session publication is surface-scoped, so a command
 * cannot reach a terminal rendered on a page the user is not looking at.
 */
/**
 * What the surface's container reports while the workspace pane is on screen.
 *
 * Promotion refuses without it, because a pane can hold a leaf in the stored tree
 * while rendering nothing (closed, tabbed behind a sibling, under another leaf's
 * zoom), and growing a split off screen looks to the user like the control failed.
 * The container notes this from its own render; a view rendered on its own here has
 * to stand in for it.
 */
function noteWorkspacePaneRendered(surface: "prs" | "issues" | "activity"): void {
  getPaneLayoutStore(surface).notePaneRender({
    activeInputTabKey: "workspace",
    editableTabs: ["conversation", "workspace"],
    onScreenTabs: ["conversation", "workspace"],
    flattened: false,
    soloChromeTabs: [],
  });
}

function claimForPrs(): void {
  navigate("/pulls");
  getInlineWorkspaceController("prs").claim(
    {
      provider: "github",
      platformHost: "github.com",
      owner: "octo",
      name: "repo",
      repoPath: "octo/repo",
      number: 1,
      itemType: "pull",
    },
    { id: "ws-1", status: "ready" },
  );
}

function promoteSession(surface: "prs" | "issues" | "activity", sessionKey: string): string {
  const layout = getPaneLayoutStore(surface);
  const paneKey = sessionPaneKey("ws-1", undefined, sessionKey);
  const leafID = layout.leafIDForTab("conversation");
  if (leafID === null) throw new Error("surface default tree has no conversation leaf");
  if (!layout.promoteTab(paneKey, { kind: "tab", leafID })) throw new Error("promotion refused");
  return paneKey;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

type RecordedEventListener = (event?: MessageEvent) => void;
type RecordedEventSource = {
  readonly url: string;
  readonly listeners: Record<string, RecordedEventListener>;
};

function installEventSourceRecorder(): RecordedEventSource[] {
  const sources: RecordedEventSource[] = [];
  mocks.subscribeWorkspaceEvents.mockImplementation((subscriber: (event: unknown) => void) => {
    const record: RecordedEventSource = {
      url: "shared-provider-events",
      listeners: {
        open: () => subscriber({ type: "open" }),
        "reconnect.stale": () => subscriber({ type: "reconnect.stale", payload: {} }),
        workspace_status: (event) => subscriber({ type: "workspace_status", payload: JSON.parse(event?.data ?? "{}") }),
        workspace_pr_associated: (event) =>
          subscriber({ type: "workspace_pr_associated", payload: JSON.parse(event?.data ?? "{}") }),
        workspace_diff_ready: (event) =>
          subscriber({ type: "workspace_diff_ready", payload: JSON.parse(event?.data ?? "{}") }),
        workspace_diff_changed: (event) =>
          subscriber({ type: "workspace_diff_changed", payload: JSON.parse(event?.data ?? "{}") }),
      },
    };
    sources.push(record);
    return () => {
      for (const type of Object.keys(record.listeners)) delete record.listeners[type];
    };
  });
  return sources;
}

function latestWorkspaceEventListeners(sources: RecordedEventSource[]): Record<string, RecordedEventListener> {
  return sources.findLast((source) => source.listeners.workspace_status !== undefined)?.listeners ?? {};
}

function capturePollingIntervals(callbacks: Array<{ callback: () => void; delay: number | undefined }>) {
  const setTimeoutLive = globalThis.setTimeout;
  return vi.spyOn(globalThis, "setTimeout").mockImplementation((callback: TimerHandler, delay?: number) => {
    if (delay !== 3000) return setTimeoutLive(callback, delay);
    const scheduled = () => {
      if (typeof callback === "function") callback();
    };
    const current = callbacks[0];
    if (current === undefined) {
      callbacks.push({ callback: scheduled, delay });
    } else {
      current.callback = scheduled;
      current.delay = delay;
    }
    return setTimeoutLive(() => undefined, 2_147_483_647);
  });
}

// Every Delete entry point opens the same confirmation dialog before any
// request goes out; "Delete workspace" is that dialog's confirm button.
async function clickDeleteAndConfirm(trigger?: HTMLElement): Promise<void> {
  await fireEvent.click(trigger ?? screen.getByRole("button", { name: "Delete" }));
  const confirmButton = await screen.findByRole("button", { name: "Delete workspace" });
  await fireEvent.click(confirmButton);
}

function fakeDataTransfer(): DataTransfer {
  const data = new Map<string, string>();
  return {
    dropEffect: "none",
    effectAllowed: "none",
    getData: (type: string) => data.get(type) ?? "",
    setData: (type: string, value: string) => {
      data.set(type, value);
    },
    setDragImage: vi.fn(),
  } as unknown as DataTransfer;
}

describe("WorkspaceTerminalView", () => {
  beforeEach(() => {
    mocks.runtime = makeAppRuntime();
    delete window.__BASE_PATH__;
    localStorage.clear();
    resetSessionHostForTest();
    resetPaneLayoutStoresForTest();
    resetWorkspaceHostForTest();
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    sockets = [];
    resetWorkspaceCreatePendingForTest();
    mocks.diffStore = createDiffStore({ runtime: mocks.runtime });
    mocks.workspaceSidebarPreference = "diff";
    mocks.getWorkspaceRuntime.mockReset();
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
    mocks.launchWorkspaceSession.mockReset();
    mocks.renameWorkspaceSession.mockReset();
    mocks.selectWorkspace.mockReset();
    mocks.selectWorkspace.mockReturnValue(() => undefined);
    mocks.showFlash.mockReset();
    mocks.renameWorkspaceSession.mockImplementation(
      async (_workspaceId: string, sessionKey: string, label: string) => ({
        ...(sessionKey === duplicateAgentSession.key ? duplicateAgentSession : runningSession),
        key: sessionKey,
        label,
      }),
    );
    mocks.stopWorkspaceSession.mockReset();
    mocks.subscribeWorkspaceEvents.mockReset();
    mocks.subscribeWorkspaceEvents.mockReturnValue(() => undefined);
    mocks.terminalWrite.mockReset();
    mocks.mockTerminalInstances.length = 0;
    mocks.mockSetTerminalSettings.mockReset();
    mocks.mockUpdateSettings.mockReset();
    mocks.mockUpdateSettings.mockImplementation(async ({ terminal }) => ({
      terminal,
    }));

    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const url = request.url;
        const { pathname } = new URL(url, "http://localhost");
        if (request.method === "PUT" && pathname.endsWith("/api/v1/settings")) {
          return request
            .clone()
            .json()
            .then((body) => mocks.mockUpdateSettings(body).then((settings) => Response.json(settings)));
        }
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(workspaceResponse));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(
            Response.json({
              workspaces: [workspaceResponse],
            }),
          );
        }
        return Promise.resolve(Response.json({}));
      }),
    );
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
    vi.stubGlobal("WebSocket", MockWebSocket);
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      callback(0);
      return 1;
    });
    vi.stubGlobal("cancelAnimationFrame", () => undefined);
  });

  afterEach(async () => {
    cleanup();
    await Effect.runPromise(mocks.runtime.disposeEffect);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("explains workspace creation in the main pane when no workspaces exist", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const url = input instanceof Request ? input.url : String(input);
        const { pathname } = new URL(url, "http://localhost");
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [] }));
        }
        if (pathname.endsWith("/api/v1/snapshot")) {
          return Promise.resolve(
            Response.json({
              workspaces: [],
              hosts: [
                {
                  configKey: "local",
                  diagnostics: [],
                  id: "local",
                  kind: "self",
                  name: "local",
                  operationAvailability: {},
                  platform: "darwin",
                  preferredTransport: "local",
                  reachable: true,
                  tmuxSessions: [],
                },
              ],
            }),
          );
        }
        if (pathname.endsWith("/api/v1/settings")) {
          return Promise.resolve(
            Response.json({
              launch_targets: [
                {
                  key: "configured-agent",
                  label: "Configured Agent",
                  kind: "agent",
                  source: "config",
                  available: true,
                },
                {
                  key: "plain_shell",
                  label: "Shell",
                  kind: "plain_shell",
                  source: "system",
                  available: true,
                },
              ],
            }),
          );
        }
        return Promise.resolve(Response.json({}));
      }),
    );

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "",
      },
    });

    expect(await screen.findByText("Create a workspace to run agents on a branch")).toBeTruthy();
    expect(screen.getByText(/a PR workspace checks out the PR head/i)).toBeTruthy();
    expect(screen.getByText(/From a PR or issue, use the/i)).toBeTruthy();
    expect(screen.getByText(/use New workspace in the sidebar/i)).toBeTruthy();
    const exampleCard = screen.getByLabelText("Workspace workflow example");
    expect(exampleCard).toBeTruthy();
    expect(screen.queryByText("Example workflow")).toBeNull();
    const createWorkspaceButton = screen.getByRole("button", {
      name: "Create Workspace",
    }) as HTMLButtonElement;
    expect(createWorkspaceButton.disabled).toBe(true);
    expect(createWorkspaceButton.getAttribute("title")).toContain("launch agents");
    const capabilityCopy = screen.getByText(/start agents, local review sessions, or a shell/i);
    expect(screen.getByText("You can then launch configured agents via the buttons provided")).toBeTruthy();
    const exampleHeading = await screen.findByText("Launch");
    expect(capabilityCopy.compareDocumentPosition(exampleHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText("New session")).toBeNull();
    expect(screen.queryByRole("button", { name: /Codex review agent/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /Claude review agent/i })).toBeNull();
    expect((screen.getByRole("button", { name: /Configured Agent/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: /Shell/i }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("uses an idle status for a live workflow session without changing the tab name", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    expect(await screen.findByRole("tab", { name: "Helper, Helper running" })).toBeTruthy();
    expect(screen.getByLabelText("Helper running").classList.contains("kit-status-dot--idle")).toBe(true);
  });

  it("uses the launch target harness icon for a workflow session tab", async () => {
    const codexSession = {
      ...runningSession,
      target_key: "codex-review",
      label: "Review Agent",
    } satisfies RuntimeSession;
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(true, [codexSession]));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    const tab = await screen.findByRole("tab", { name: "Review Agent, Review Agent running" });
    expect(tab.querySelector(".kit-harness-icon--openai")).not.toBeNull();
  });

  it("persists toolbar font zoom through shared settings", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Increase terminal font size",
      }),
    );
    await waitFor(() => {
      expect(mocks.mockUpdateSettings).toHaveBeenCalledWith({
        terminal: expect.objectContaining({ font_size: 15 }),
      });
    });
    expect(mocks.mockSetTerminalSettings).toHaveBeenCalledWith(expect.objectContaining({ font_size: 15 }));
  });

  it.each([
    ["Meta+=", { key: "=", metaKey: true }],
    ["Ctrl+-", { key: "-", ctrlKey: true }],
    ["Meta+0", { key: "0", metaKey: true }],
  ] as const)("leaves %s untouched while a terminal is focused", async (_label, init) => {
    const { container } = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Increase terminal font size" });
    const terminalInput = document.createElement("textarea");
    container.querySelector(".terminal-container")?.append(terminalInput);
    terminalInput.focus();
    const event = new KeyboardEvent("keydown", {
      ...init,
      bubbles: true,
      cancelable: true,
    });
    terminalInput.dispatchEvent(event);
    await Promise.resolve();

    expect(event.defaultPrevented).toBe(false);
    expect(mocks.mockUpdateSettings).not.toHaveBeenCalled();
  });

  it.each([
    ["starting", "Helper starting", "kit-status-dot--stale"],
    ["error", "Helper unavailable", "kit-status-dot--unclean"],
  ] as const)("maps a %s workflow session to a semantic tab status", async (status, label, className) => {
    mocks.getWorkspaceRuntime.mockResolvedValue({
      launch_targets: [],
      sessions: [{ ...runningSession, status }],
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    expect(await screen.findByRole("tab", { name: `Helper, ${label}` })).toBeTruthy();
    expect(screen.getByLabelText(label).classList.contains(className)).toBe(true);
  });

  it("closes an agent tab immediately when its terminal exits", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("region", { name: "Workflow panes" });
    await waitFor(() => expect(sockets).toHaveLength(1));

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );

    await waitFor(() => expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull());
    expect(screen.getByRole("tab", { name: /Home/ }).getAttribute("aria-selected")).toBe("true");
    expect(localStorage.getItem("kenn-forge-workspace-active-tab:ws-1")).toBe("home");
  });

  it("restores workspace focus when the focused workflow content exits", async () => {
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      JSON.stringify({
        version: 1,
        open: false,
        dock: "bottom",
        height: 300,
        activeSessionKey: null,
        tree: null,
        sessionRegions: { "ws-1:helper": "workflow", "ws-1:reviewer": "workflow" },
        workflowMode: "tabs",
        workflowTree: {
          type: "leaf",
          id: "wf-sessions",
          tabs: ["session:ws-1:helper", "session:ws-1:reviewer"],
          activeTabKey: "session:ws-1:helper",
        },
        customSessionLabels: {},
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    claimForPrs();
    noteWorkspacePaneRendered("prs");
    const runCommand = mocks.runtime.runCommand.bind(mocks.runtime);
    const recoveryStarted = deferred<void>();
    const releaseRecovery = deferred<void>();
    vi.spyOn(mocks.runtime, "runCommand").mockImplementation((program, options) =>
      runCommand(
        options.operation === "workspace.restore.focus"
          ? Effect.sync(() => recoveryStarted.resolve()).pipe(
              Effect.andThen(Effect.promise(() => releaseRecovery.promise)),
              Effect.andThen(program),
            )
          : program,
        options,
      ),
    );
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    const focusTarget = await screen.findByRole("button", { name: "Move Helper to terminal" });
    focusTarget.focus();
    expect(document.activeElement).toBe(focusTarget);
    const workflowStage = screen.getByRole("region", { name: "Workflow panes" });
    await waitFor(() =>
      expect(document.querySelectorAll(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveLength(1),
    );
    await waitFor(() => expect(sockets).toHaveLength(1));

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );
    await recoveryStarted.promise;
    workflowStage.dispatchEvent(new FocusEvent("focusout", { bubbles: true, relatedTarget: document.body }));
    flushSync();
    releaseRecovery.resolve();

    await waitFor(() => expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull());
    expect(focusTarget.isConnected).toBe(false);
    await waitFor(() => expect(document.activeElement).toBe(document.querySelector(".terminal-view")));
  });

  it("starts the runtime request before workspace metadata resolves without fetching it twice", async () => {
    const workspaceRequest = deferred<Response>();
    const runtimeRequest = deferred<ReturnType<typeof runtimeWithStaleSession>>();
    let workspaceRequestStarted = false;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const url = input instanceof Request ? input.url : String(input);
        const { pathname } = new URL(url, "http://localhost");
        if (pathname.endsWith("/workspaces/ws-1")) {
          workspaceRequestStarted = true;
          return workspaceRequest.promise;
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    mocks.getWorkspaceRuntime.mockReturnValue(runtimeRequest.promise);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await waitFor(() => expect(workspaceRequestStarted).toBe(true));
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", undefined);

    runtimeRequest.resolve(runtimeWithStaleSession());
    workspaceRequest.resolve(Response.json(workspaceResponse));

    await screen.findByText("acme/widget");
    await screen.findByRole("tab", { name: /Helper/ });
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(1);
  });

  it("polls local workspace runtime so peer-spawned sessions appear", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    const setIntervalSpy = capturePollingIntervals(intervalCallbacks);
    const initialRuntime = deferred<{ launch_targets: never[]; sessions: never[] }>();
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(initialRuntime.promise)
      .mockResolvedValueOnce(runtimeWithSession("2026-04-29T00:03:00Z"));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", undefined));
    await waitFor(() => expect(intervalCallbacks.some((interval) => interval.delay === 3000)).toBe(true));
    initialRuntime.resolve({ launch_targets: [], sessions: [] });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull();
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();

    runtimePoll!.callback();

    await waitFor(() => expect(screen.getByRole("tab", { name: /Helper/ })).toBeTruthy());
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2);
    setIntervalSpy.mockRestore();
  });

  it("does not reapply identical runtime polls to an active terminal", async () => {
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    capturePollingIntervals(intervalCallbacks);
    const requestAnimationFrameSpy = vi.fn((callback: FrameRequestCallback) => {
      callback(0);
      return 1;
    });
    vi.stubGlobal("requestAnimationFrame", requestAnimationFrameSpy);
    const layoutStorageSpy = vi.spyOn(Storage.prototype, "setItem");
    const runtimePayload = runtimeWithStaleSession();
    mocks.getWorkspaceRuntime.mockImplementation(async () => structuredClone(runtimePayload));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Helper/ });
    await waitFor(() => expect(mocks.mockTerminalInstances).toHaveLength(1));
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();
    const runtimeRequestCount = mocks.getWorkspaceRuntime.mock.calls.length;
    const terminalCount = mocks.mockTerminalInstances.length;
    mocks.mockFit.mockClear();
    requestAnimationFrameSpy.mockClear();
    layoutStorageSpy.mockClear();

    runtimePoll!.callback();

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(runtimeRequestCount + 1));
    await Promise.resolve();
    expect(mocks.mockTerminalInstances).toHaveLength(terminalCount);
    expect(mocks.mockFit).not.toHaveBeenCalled();
    expect(requestAnimationFrameSpy).not.toHaveBeenCalled();
    expect(layoutStorageSpy).not.toHaveBeenCalled();
  });

  it("reapplies an authoritative runtime response after a local rename", async () => {
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    capturePollingIntervals(intervalCallbacks);
    const serverRuntime = runtimeWithStaleSession();
    mocks.getWorkspaceRuntime.mockImplementation(async () => structuredClone(serverRuntime));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Helper/ });
    await fireEvent.click(screen.getByRole("button", { name: "Rename Helper" }));
    const input = await screen.findByRole("textbox", { name: "Name" });
    await fireEvent.input(input, { target: { value: "Review helper" } });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("tab", { name: /Review helper/ });

    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();
    runtimePoll!.callback();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Helper, Helper running" })).toBeTruthy());
    expect(screen.queryByRole("tab", { name: /Review helper/ })).toBeNull();
  });

  it("ignores a runtime poll that started before a local rename", async () => {
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    capturePollingIntervals(intervalCallbacks);
    const stalePoll = deferred<ReturnType<typeof runtimeWithStaleSession>>();
    mocks.getWorkspaceRuntime
      .mockResolvedValueOnce(runtimeWithStaleSession())
      .mockReturnValueOnce(stalePoll.promise)
      .mockResolvedValueOnce({
        ...runtimeWithStaleSession(),
        sessions: [{ ...runningSession, label: "Review helper" }],
      });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: "Helper, Helper running" });
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();
    runtimePoll!.callback();
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2));

    await fireEvent.click(screen.getByRole("button", { name: "Rename Helper" }));
    const input = await screen.findByRole("textbox", { name: "Name" });
    await fireEvent.input(input, { target: { value: "Review helper" } });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("tab", { name: /Review helper/ });

    stalePoll.resolve(runtimeWithStaleSession());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.getByRole("tab", { name: /Review helper/ })).toBeTruthy();

    runtimePoll!.callback();
    await waitFor(() => expect(mocks.getWorkspaceRuntime.mock.calls.length).toBeGreaterThanOrEqual(3));
  });

  it("polls remote workspace runtime so peer-spawned sessions appear", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:fleet:member:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    const setIntervalSpy = capturePollingIntervals(intervalCallbacks);
    const initialRuntime = deferred<{ launch_targets: never[]; sessions: never[] }>();
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(initialRuntime.promise)
      .mockResolvedValueOnce(runtimeWithSession("2026-04-29T00:03:00Z"));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        workspaceHostKey: "member",
      },
    });

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", "member"));
    await waitFor(() => expect(intervalCallbacks.some((interval) => interval.delay === 3000)).toBe(true));
    initialRuntime.resolve({ launch_targets: [], sessions: [] });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Kata" })).toBeNull();
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();

    runtimePoll!.callback();

    await waitFor(() => expect(screen.getByRole("tab", { name: /Helper/ })).toBeTruthy());
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2);
    setIntervalSpy.mockRestore();
  });

  it("persists remote terminal layout under the fleet-scoped workspace key", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:fleet:member:ws-1", "home");
    localStorage.removeItem("kenn-forge-workspace-terminal-layout:ws-1");
    mocks.getWorkspaceRuntime.mockResolvedValue({ launch_targets: [], sessions: [] });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        workspaceHostKey: "member",
      },
    });

    await screen.findByRole("tab", { name: "Home" });
    await waitFor(() =>
      expect(localStorage.getItem("kenn-forge-workspace-terminal-layout:fleet:member:ws-1")).toContain(
        '"workflowMode":"tabs"',
      ),
    );
    expect(localStorage.getItem("kenn-forge-workspace-terminal-layout:ws-1")).toBeNull();
  });

  it("does not show remote runtime while same-id local workspace data is still cached", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem("kenn-forge-workspace-active-tab:fleet:member:ws-1", "home");
    const remoteWorkspace = deferred<typeof workspaceResponse>();
    const eventSources = installEventSourceRecorder();

    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const url = input instanceof Request ? input.url : String(input);
        const { pathname } = new URL(url, "http://localhost");
        if (pathname === "/api/v1/workspaces/ws-1") {
          return Promise.resolve(Response.json(workspaceResponse));
        }
        if (pathname === "/api/v1/fleet/hosts/member/workspaces/ws-1") {
          return remoteWorkspace.promise.then((workspace) => Response.json({ ...workspace, fleet_host_key: "member" }));
        }
        if (pathname === "/api/v1/workspaces") {
          return Promise.resolve(
            Response.json({
              workspaces: [workspaceResponse],
            }),
          );
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
    const { rerender } = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Helper/ });

    mocks.getWorkspaceRuntime.mockClear();
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    await rerender({
      workspaceId: "ws-1",
      workspaceHostKey: "member",
    });

    await waitFor(() => expect(latestWorkspaceEventListeners(eventSources)["reconnect.stale"]).toBeTypeOf("function"));
    latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", "member"));
    expect(screen.queryByRole("tab", { name: /Reviewer/ })).toBeNull();

    remoteWorkspace.resolve(workspaceResponse);

    await waitFor(() => expect(screen.getByRole("tab", { name: /Reviewer/ })).toBeTruthy());
  });

  it("recovers pending enrichment that completed before the event stream opened", async () => {
    const eventSources = installEventSourceRecorder();
    const pendingWorkspace = {
      ...workspaceResponse,
      enrichment_status: "pending",
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string) => {
      const url = input instanceof Request ? input.url : String(input);
      const { pathname } = new URL(url, "http://localhost");
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(pendingWorkspace));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [pendingWorkspace] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => fetchPath(input as Request | URL | string).endsWith("/workspaces/ws-1")),
      ).toBe(true);
    });
    const beforeOpen = fetchMock.mock.calls.filter(([input]) =>
      fetchPath(input as Request | URL | string).endsWith("/workspaces/ws-1"),
    ).length;

    await waitFor(() => expect(latestWorkspaceEventListeners(eventSources).open).toBeTypeOf("function"));
    latestWorkspaceEventListeners(eventSources).open?.();

    await waitFor(() => {
      const afterOpen = fetchMock.mock.calls.filter(([input]) =>
        fetchPath(input as Request | URL | string).endsWith("/workspaces/ws-1"),
      ).length;
      expect(afterOpen).toBe(beforeOpen + 1);
    });
  });

  it("scopes only local workspace event streams for diff prewarming", async () => {
    const releaseSelection = vi.fn();
    mocks.selectWorkspace.mockReturnValue(releaseSelection);
    const { rerender } = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => expect(mocks.selectWorkspace).toHaveBeenCalledWith("ws-1"));

    await rerender({ workspaceId: "ws-1", workspaceHostKey: "member" });
    await waitFor(() => expect(releaseSelection).toHaveBeenCalledOnce());
    expect(mocks.selectWorkspace).toHaveBeenCalledTimes(1);
  });

  it("keeps an active diff load when selected prewarming becomes ready", async () => {
    window.__BASE_PATH__ = window.location.origin;
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    localStorage.setItem("kenn-forge-workspace-sidebar-tab:ws-1", "diff");
    const eventSources = installEventSourceRecorder();
    const loadWorkspaceDiff = vi.spyOn(mocks.diffStore, "loadWorkspaceDiff").mockResolvedValue();
    const cancelWorkspaceDiff = vi.spyOn(mocks.diffStore, "cancelWorkspaceDiff");

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
      context: new Map([[STORES_KEY, { diff: mocks.diffStore }]]),
    });

    await waitFor(() => expect(loadWorkspaceDiff).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(latestWorkspaceEventListeners(eventSources).workspace_diff_ready).toBeTypeOf("function"),
    );
    const listeners = latestWorkspaceEventListeners(eventSources);
    listeners.workspace_diff_ready?.(
      new MessageEvent("workspace_diff_ready", {
        data: JSON.stringify({ workspace_id: "ws-1", version: "generation:ready" }),
      }),
    );
    const fetchMock = vi.mocked(globalThis.fetch);
    fetchMock.mockClear();
    listeners.workspace_status?.(
      new MessageEvent("workspace_status", {
        data: JSON.stringify({ id: "ws-1" }),
      }),
    );
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([input]) => fetchPath(input).endsWith("/workspaces/ws-1"))).toBe(true),
    );
    expect(loadWorkspaceDiff).toHaveBeenCalledTimes(1);
    expect(cancelWorkspaceDiff).not.toHaveBeenCalled();

    listeners.workspace_diff_changed?.(
      new MessageEvent("workspace_diff_changed", {
        data: JSON.stringify({ workspace_id: "ws-1", version: "generation:changed" }),
      }),
    );

    await waitFor(() => expect(loadWorkspaceDiff.mock.calls.length).toBeGreaterThan(1));
    expect(loadWorkspaceDiff).toHaveBeenCalledTimes(2);
    expect(cancelWorkspaceDiff).toHaveBeenCalledTimes(1);
    expect(loadWorkspaceDiff).toHaveBeenLastCalledWith(
      "ws-1",
      "head",
      false,
      expect.objectContaining({ preserveVisible: true }),
    );
  });

  it("prewarms a selected fleet diff and reloads it when the remote watch advances", async () => {
    window.__BASE_PATH__ = window.location.origin;
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    localStorage.setItem("kenn-forge-workspace-sidebar-tab:fleet:member:ws-1", "diff");
    const changed = deferred<Response>();
    let watchCalls = 0;
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string) => {
      const raw = input instanceof Request ? input.url : String(input);
      const url = new URL(raw, "http://localhost");
      if (url.pathname.endsWith("/fleet/hosts/member/workspaces/ws-1/diff/watch")) {
        watchCalls += 1;
        if (watchCalls === 1) {
          return Promise.resolve(Response.json({ changed: true, version: "fleet:1" }));
        }
        if (watchCalls === 2) return changed.promise;
        return new Promise<Response>(() => {});
      }
      if (url.pathname.endsWith("/fleet/hosts/member/workspaces/ws-1")) {
        return Promise.resolve(Response.json({ ...workspaceResponse, fleet_host_key: "member" }));
      }
      if (url.pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const loadWorkspaceDiff = vi.spyOn(mocks.diffStore, "loadWorkspaceDiff").mockResolvedValue();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", workspaceHostKey: "member" },
      context: new Map([[STORES_KEY, { diff: mocks.diffStore }]]),
    });

    await waitFor(() => expect(watchCalls).toBe(2));
    await waitFor(() => expect(loadWorkspaceDiff).toHaveBeenCalled());
    const beforeChange = loadWorkspaceDiff.mock.calls.length;

    changed.resolve(Response.json({ changed: true, version: "fleet:2" }));

    await waitFor(() => expect(loadWorkspaceDiff.mock.calls.length).toBeGreaterThan(beforeChange));
    expect(loadWorkspaceDiff).toHaveBeenLastCalledWith(
      "ws-1",
      "head",
      false,
      expect.objectContaining({ workspaceHostKey: "member", preserveVisible: true }),
    );
  });

  it("retries a fleet diff watch while the workspace transitions from creating to ready", async () => {
    window.__BASE_PATH__ = window.location.origin;
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    localStorage.setItem("kenn-forge-workspace-sidebar-tab:fleet:member:ws-1", "diff");
    vi.spyOn(Math, "random").mockReturnValue(0);
    let watchCalls = 0;
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string) => {
      const raw = input instanceof Request ? input.url : String(input);
      const url = new URL(raw, "http://localhost");
      if (url.pathname.endsWith("/fleet/hosts/member/workspaces/ws-1/diff/watch")) {
        watchCalls += 1;
        if (watchCalls === 1) {
          return Promise.resolve(Response.json({ detail: "workspace is not ready" }, { status: 409 }));
        }
        if (watchCalls === 2) {
          return Promise.resolve(Response.json({ changed: true, version: "fleet:ready" }));
        }
        return new Promise<Response>(() => {});
      }
      if (url.pathname.endsWith("/fleet/hosts/member/workspaces/ws-1")) {
        return Promise.resolve(Response.json({ ...workspaceResponse, fleet_host_key: "member" }));
      }
      if (url.pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const loadWorkspaceDiff = vi.spyOn(mocks.diffStore, "loadWorkspaceDiff").mockResolvedValue();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", workspaceHostKey: "member" },
      context: new Map([[STORES_KEY, { diff: mocks.diffStore }]]),
    });

    await waitFor(() => expect(watchCalls).toBe(3), { timeout: 2_000 });
    expect(
      fetchMock.mock.calls.some(([input]) => {
        const raw = input instanceof Request ? input.url : String(input);
        return new URL(raw, "http://localhost").searchParams.get("version") === "fleet:ready";
      }),
    ).toBe(true);
    await waitFor(() => {
      expect(loadWorkspaceDiff).toHaveBeenLastCalledWith(
        "ws-1",
        "head",
        false,
        expect.objectContaining({ workspaceHostKey: "member", preserveVisible: true }),
      );
    });
  });

  it.each(["details", "workflow"] as const)(
    "releases %s focus while waiting for matching workspace state",
    async (focusedRegion) => {
      window.__BASE_PATH__ = window.location.origin;
      localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
      localStorage.setItem("kenn-forge-workspace-sidebar-tab:ws-1", "diff");
      const workspaceB = { ...workspaceResponse, id: "ws-2", git_head_ref: "feature/two" };
      const workspaceBGate = deferred<typeof workspaceB>();
      const runtimeBGate = deferred<ReturnType<typeof runtimeWithStaleSession>>();
      const eventListeners: Array<Record<string, (event: MessageEvent) => void>> = [];
      const loadWorkspaceDiff = vi.spyOn(mocks.diffStore, "loadWorkspaceDiff").mockResolvedValue();

      vi.stubGlobal(
        "fetch",
        vi.fn().mockImplementation((input: Request | URL | string) => {
          const path = fetchPath(input);
          if (path.endsWith("/workspaces/ws-1")) return Promise.resolve(Response.json(workspaceResponse));
          if (path.endsWith("/workspaces/ws-2")) {
            return workspaceBGate.promise.then((workspace) => Response.json(workspace));
          }
          if (path.endsWith("/api/v1/workspaces")) {
            return Promise.resolve(Response.json({ workspaces: [workspaceResponse, workspaceB] }));
          }
          return Promise.resolve(Response.json({}));
        }),
      );
      vi.stubGlobal(
        "EventSource",
        class {
          private listeners: Record<string, (event: MessageEvent) => void> = {};
          constructor() {
            eventListeners.push(this.listeners);
          }
          addEventListener(type: string, callback: (event: MessageEvent) => void): void {
            this.listeners[type] = callback;
          }
          close(): void {}
        },
      );
      mocks.getWorkspaceRuntime
        .mockResolvedValueOnce(runtimeWithStaleSession())
        .mockReturnValueOnce(runtimeBGate.promise);

      const { rerender } = render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1" },
        context: new Map([[STORES_KEY, { diff: mocks.diffStore }]]),
      });
      await waitFor(() => expect(loadWorkspaceDiff).toHaveBeenCalledWith("ws-1", "head", false, expect.anything()));

      const inputOwner =
        focusedRegion === "details"
          ? await screen.findByRole("region", { name: "Workspace details pane" })
          : await waitFor(() => {
              const pane = document.querySelector<HTMLElement>('[data-pane-key="session:ws-1:helper"]');
              expect(pane).not.toBeNull();
              return pane!;
            });
      const focusTarget = document.createElement("button");
      inputOwner.append(focusTarget);
      focusTarget.focus();
      expect(document.activeElement).toBe(focusTarget);
      if (focusedRegion === "details") {
        await waitFor(() => expect(inputOwner.classList.contains("input-active")).toBe(true));
      } else {
        await waitFor(() =>
          expect(document.querySelectorAll(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveLength(1),
        );
      }

      await rerender({ workspaceId: "ws-2" });

      // Liveness gating unmounts the stale ws-1 view entirely while ws-2
      // loads: the old toolbar and sidebar are gone, not lingering behind
      // action guards.
      expect(await screen.findByText("Setting up workspace...")).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Workspace Diff" })).toBeNull();
      expect(loadWorkspaceDiff).toHaveBeenCalledTimes(1);
      await waitFor(() => expect(document.activeElement).toBe(document.querySelector(".terminal-view")));
      expect(document.querySelector(".right-sidebar.input-active")).toBeNull();
      expect(document.querySelector(".workspace-stage .tabbed-panel-leaf.input-active")).toBeNull();

      eventListeners
        .findLast((listeners) => listeners.workspace_diff_ready !== undefined)
        ?.workspace_diff_ready?.(
          new MessageEvent("workspace_diff_ready", {
            data: JSON.stringify({ workspace_id: "ws-2", version: "generation:2" }),
          }),
        );
      workspaceBGate.resolve(workspaceB);
      // ws-2's payload landed but its runtime is still pending: the ready
      // view mounts with the details-loading sub-state and the diff still
      // waits for the matching runtime.
      expect(await screen.findByText("Loading workspace details...")).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Workspace Diff" })).toBeNull();
      expect(loadWorkspaceDiff).toHaveBeenCalledTimes(1);

      runtimeBGate.resolve(runtimeWithStaleSession());
      await waitFor(() => expect(loadWorkspaceDiff).toHaveBeenCalledWith("ws-2", "head", false, expect.anything()));
      expect(screen.queryByText("Loading workspace details...")).toBeNull();
    },
  );

  it("renders matching workspace details when runtime loading fails", async () => {
    window.__BASE_PATH__ = window.location.origin;
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    localStorage.setItem("kenn-forge-workspace-sidebar-tab:ws-1", "diff");
    const loadWorkspaceDiff = vi.spyOn(mocks.diffStore, "loadWorkspaceDiff").mockResolvedValue();

    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const path = fetchPath(input);
        if (path.endsWith("/workspaces/ws-1")) return Promise.resolve(Response.json(workspaceResponse));
        if (path.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    vi.stubGlobal(
      "EventSource",
      class {
        addEventListener(): void {}
        close(): void {}
      },
    );
    mocks.getWorkspaceRuntime.mockRejectedValue(new Error("runtime unavailable"));

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
      context: new Map([[STORES_KEY, { diff: mocks.diffStore }]]),
    });

    expect(await screen.findByText("runtime unavailable")).toBeTruthy();
    await waitFor(() => expect(loadWorkspaceDiff).toHaveBeenCalledWith("ws-1", "head", false, expect.anything()));
    expect(screen.queryByText("Loading workspace details...")).toBeNull();
  });

  it("treats id-less workspace status events as global invalidation", async () => {
    const eventSources = installEventSourceRecorder();
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string) => {
      const url = input instanceof Request ? input.url : String(input);
      const { pathname } = new URL(url, "http://localhost");
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    fetchMock.mockClear();

    await waitFor(() => expect(latestWorkspaceEventListeners(eventSources).workspace_status).toBeTypeOf("function"));
    latestWorkspaceEventListeners(eventSources).workspace_status?.(
      new MessageEvent("workspace_status", { data: "{}" }),
    );

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => fetchPath(input as Request | URL | string).endsWith("/workspaces/ws-1")),
      ).toBe(true);
    });
  });

  it("does not overlap runtime polling while a slow fetch is in flight", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:fleet:member:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    const setIntervalSpy = capturePollingIntervals(intervalCallbacks);
    let resolveFirst: (value: ReturnType<typeof runtimeWithStaleSession>) => void = () => undefined;
    const firstFetch = new Promise<ReturnType<typeof runtimeWithStaleSession>>((resolve) => {
      resolveFirst = resolve;
    });
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(firstFetch)
      .mockResolvedValueOnce(runtimeWithSession("2026-04-29T00:03:00Z"));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        workspaceHostKey: "member",
      },
    });

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", "member"));
    await waitFor(() => expect(intervalCallbacks.some((interval) => interval.delay === 3000)).toBe(true));
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();

    runtimePoll!.callback();
    await Promise.resolve();
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(1);

    resolveFirst({ launch_targets: [], sessions: [] });
    await waitFor(() => expect(screen.getByRole("tab", { name: /Home/ })).toBeTruthy());

    runtimePoll!.callback();
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2));
    setIntervalSpy.mockRestore();
  });

  it("forces post-launch runtime refresh past an older in-flight poll", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    const setIntervalSpy = capturePollingIntervals(intervalCallbacks);
    const stalePoll = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    mocks.getWorkspaceRuntime
      .mockResolvedValueOnce(runtimeWithTerminalSession())
      .mockReturnValueOnce(stalePoll.promise)
      .mockResolvedValueOnce(runtimeWithTerminalSession())
      .mockResolvedValueOnce(runtimeWithTerminalSession(relaunchedShellSession));
    mocks.launchWorkspaceSession.mockResolvedValue(relaunchedShellSession);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Home/ });
    const terminalButton = screen.getByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_a"))).toBe(true));
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();

    runtimePoll!.callback();
    await Promise.resolve();
    expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2);

    await fireEvent.click(screen.getAllByRole("button", { name: "New terminal" })[0]!);

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(true));

    stalePoll.resolve(runtimeWithTerminalSession());
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(sockets.filter((socket) => socket.url.includes("ws-1_shell_a"))).toHaveLength(1);
    expect(sockets.filter((socket) => socket.url.includes("ws-1_shell_b"))).toHaveLength(1);
    setIntervalSpy.mockRestore();
  });

  it("collapses a dock persisted open when no terminal session is left in it", async () => {
    // The last docked session exiting leaves open=true behind; an open dock with
    // nothing in it is a saved-height hole in the stage.
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      JSON.stringify({
        version: 1,
        open: true,
        dock: "bottom",
        height: 300,
        activeSessionKey: null,
        tree: null,
        sessionRegions: {},
        workflowMode: "tabs",
        workflowTree: null,
        customSessionLabels: {},
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    // The toggle's label flips with `open`, so waiting for "Open terminal
    // panel" is waiting for the reconcile to have closed the dock.
    await screen.findByRole("button", { name: "Open terminal panel" });
    expect(document.querySelector(".terminal-panel.open")).toBeNull();
  });

  it("releases the previous workspace's pooled terminals for bounded retention", async () => {
    const { rerender } = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
    });

    await screen.findByRole("tab", { name: /Helper/ });
    await waitFor(() => expect(mountedSessions()).toHaveLength(1));
    await waitFor(() => expect(sockets).toHaveLength(1));
    sockets[0]!.onopen();
    const firstPrefix = sessionHostPrefix("ws-1", undefined);
    expect(mountedSessions()[0]?.hostKey.startsWith(firstPrefix)).toBe(true);
    const firstHostKey = mountedSessions()[0]!.hostKey;
    const firstWrapper = document.querySelector(`[data-session-host="${firstHostKey}"]`);
    const firstSocket = sockets[0];

    await rerender({ workspaceId: "ws-2" });

    await waitFor(() => {
      const retained = mountedSessions().find((session) => session.hostKey.startsWith(firstPrefix));
      expect(retained).toBeDefined();
      expect(isSessionClaimed(retained!.hostKey)).toBe(false);
      expect(document.querySelector(`[data-session-host="${firstHostKey}"]`)).toBe(firstWrapper);
      expect(sockets.filter((socket) => socket.url.includes("/workspaces/ws-1/"))).toEqual([firstSocket]);
    });

    await rerender({ workspaceId: "ws-1" });

    await waitFor(() => {
      expect(isSessionClaimed(firstHostKey)).toBe(true);
      expect(sockets.filter((socket) => socket.url.includes("/workspaces/ws-1/"))).toEqual([firstSocket]);
      expect(document.querySelector(`[data-session-host="${firstHostKey}"]`)).toBe(firstWrapper);
    });
  });

  it("releases its pooled terminals when the view itself goes away", async () => {
    const { rerender } = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await screen.findByRole("tab", { name: /Helper/ });
    await waitFor(() => expect(mountedSessions()).toHaveLength(1));
    await waitFor(() => expect(sockets).toHaveLength(1));
    sockets[0]!.onopen();
    const hostKey = mountedSessions()[0]!.hostKey;

    await rerender({ workspaceId: "ws-1", showView: false });

    await waitFor(() => {
      expect(mountedSessions()).toHaveLength(1);
      expect(isSessionClaimed(hostKey)).toBe(false);
    });
  });

  it("shows a relaunched agent with the same key and a new generation", async () => {
    const relaunchedAt = "2026-04-29T00:01:00Z";
    const initialRuntime = deferred<ReturnType<typeof runtimeWithStaleSession>>();
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(initialRuntime.promise)
      .mockResolvedValueOnce(runtimeWithSession(relaunchedAt));

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Refresh workspace details" });
    initialRuntime.resolve(runtimeWithStaleSession());
    await screen.findByRole("tab", { name: /Helper/ });
    await waitFor(() => expect(sockets).toHaveLength(1));

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByRole("tab", { name: /Helper/ })).toBeTruthy());
  });

  it("restores a selected workflow tab without keeping the tiled grid view", async () => {
    localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-1", persistedTerminalLayout("grid"));

    const { container } = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    const helperTab = await screen.findByRole("tab", {
      name: /Helper/,
    });

    expect(helperTab.getAttribute("aria-selected")).toBe("true");
    expect(container.querySelector(".workspace-stage.grid")).toBeNull();
  });

  it("drops a restored legacy Shell tab after runtime tabs are normalized", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "shell");

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    const homeTab = await screen.findByRole("tab", { name: "Home" });

    expect(homeTab.getAttribute("aria-selected")).toBe("true");
    expect(screen.queryByRole("tab", { name: /Shell/ })).toBeNull();
    expect(sockets).toHaveLength(0);
  });

  it("closes a terminal-panel shell when its terminal exits", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Home/ });
    const terminalButton = screen.getByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets).toHaveLength(1));
    expect(screen.queryByLabelText("Terminal selector")).toBeNull();

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );

    // The dock does not sit open and empty behind the exit: the reconcile
    // collapses it, and the toggle's label is the collapse signal.
    await screen.findByRole("button", { name: "Open terminal panel" });
    expect(document.querySelector(".terminal-panel.open")).toBeNull();
  });

  it("uses an in-app modal when stopping a running shell", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());
    const confirm = vi.fn();
    vi.stubGlobal("confirm", confirm);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Open terminal panel",
      }),
    );
    await waitFor(() => expect(sockets).toHaveLength(1));

    await fireEvent.click(screen.getByRole("button", { name: "Close Shell" }));

    expect(confirm).not.toHaveBeenCalled();
    expect(mocks.stopWorkspaceSession).not.toHaveBeenCalled();
    expect(await screen.findByRole("dialog", { name: "Stop Shell?" })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Stop Shell?" })).toBeNull());

    await fireEvent.click(screen.getByRole("button", { name: "Close Shell" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Stop session" }));

    await waitFor(() => expect(mocks.stopWorkspaceSession).toHaveBeenCalledWith("ws-1", "ws-1_shell_a", undefined));
  });

  it("settles a stopped session before its forced runtime refresh returns", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());
    mocks.stopWorkspaceSession.mockResolvedValue(undefined);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Open terminal panel" }));
    await waitFor(() => expect(sockets).toHaveLength(1));
    await fireEvent.click(screen.getByRole("button", { name: "Close Shell" }));
    const confirm = await screen.findByRole("dialog", { name: "Stop Shell?" });
    const stalledRefresh = deferred<ReturnType<typeof runtimeWithTwoTerminalSessions>>();
    mocks.getWorkspaceRuntime.mockReturnValue(stalledRefresh.promise);

    await fireEvent.click(within(confirm).getByRole("button", { name: "Stop session" }));
    await waitFor(() => expect(mocks.stopWorkspaceSession).toHaveBeenCalledWith("ws-1", "ws-1_shell_a", undefined));

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Stop Shell?" })).toBeNull());
    expect(screen.queryByRole("button", { name: "Close Shell" })).toBeNull();
    expect(screen.getByRole("button", { name: "Shell 2 Running" })).toBeTruthy();
  });

  it("uses an in-app modal when renaming a tab", async () => {
    const prompt = vi.fn();
    vi.stubGlobal("prompt", prompt);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Helper/ });
    await fireEvent.click(screen.getByRole("button", { name: "Rename Helper" }));

    expect(prompt).not.toHaveBeenCalled();
    expect(await screen.findByRole("dialog", { name: "Rename tab" })).toBeTruthy();
    const input = screen.getByRole("textbox", { name: "Name" });
    expect((input as HTMLInputElement).value).toBe("Helper");

    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Rename tab" })).toBeNull());
    expect(screen.getByRole("tab", { name: /Helper/ })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Rename Helper" }));
    const reopenedInput = await screen.findByRole("textbox", {
      name: "Name",
    });
    await fireEvent.input(reopenedInput, {
      target: { value: "Review helper" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(screen.getByRole("tab", { name: /Review helper/ })).toBeTruthy());
    expect(mocks.renameWorkspaceSession).toHaveBeenCalledWith("ws-1", "ws-1:helper", "Review helper", undefined);
  });

  it("renders duplicate runtime labels literally instead of synthesizing names", async () => {
    mocks.getWorkspaceRuntime.mockResolvedValue({
      launch_targets: [],
      sessions: [
        runningSession,
        {
          ...duplicateAgentSession,
          label: runningSession.label,
        },
      ],
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await waitFor(() => expect(screen.getAllByRole("tab", { name: "Helper, Helper running" })).toHaveLength(2));
    expect(screen.queryByRole("tab", { name: /Helper 2/ })).toBeNull();
  });

  it("renames a workflow tab by its opaque session key", async () => {
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithDuplicateWorkflowSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: "Helper, Helper running" });
    await screen.findByRole("tab", { name: /Helper 2/ });

    await fireEvent.click(screen.getByRole("button", { name: "Rename Helper" }));
    const input = await screen.findByRole("textbox", { name: "Name" });
    expect((input as HTMLInputElement).value).toBe("Helper");

    await fireEvent.input(input, {
      target: { value: "Plan review" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(screen.getByRole("tab", { name: /Plan review/ })).toBeTruthy());
    expect(screen.getByRole("tab", { name: /Helper 2/ })).toBeTruthy();
    expect(mocks.renameWorkspaceSession).toHaveBeenCalledWith("ws-1", "ws-1:helper", "Plan review", undefined);
  });
  it("does not reopen the just-exited terminal from stale runtime data", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Home/ });
    const terminalButton = screen.getByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets).toHaveLength(1));

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );
    await screen.findByRole("button", { name: "Open terminal panel" });
    expect(sockets).toHaveLength(1);
  });

  it("reconnects terminal panes when selecting another shell", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    const terminalButton = await screen.findByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_a"))).toBe(true));

    await fireEvent.click(screen.getByRole("button", { name: "Shell 2" }));

    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(true));
  });

  it("renders a split terminal immediately after launching its session", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
    mocks.launchWorkspaceSession.mockResolvedValue({
      ...relaunchedShellSession,
      label: "Shell 2",
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    const terminalButton = await screen.findByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_a"))).toBe(true));

    await fireEvent.click(screen.getByRole("button", { name: "Split terminal right" }));

    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(true));
    expect(screen.getByRole("button", { name: "Shell 2" })).toBeTruthy();
  });

  it("keeps a split terminal when an older runtime poll resolves", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    capturePollingIntervals(intervalCallbacks);
    const stalePoll = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    mocks.getWorkspaceRuntime
      .mockResolvedValueOnce(runtimeWithTerminalSession())
      .mockReturnValueOnce(stalePoll.promise)
      .mockResolvedValueOnce(runtimeWithTerminalSession())
      .mockResolvedValueOnce(runtimeWithTwoTerminalSessions());
    mocks.launchWorkspaceSession.mockResolvedValue({
      ...relaunchedShellSession,
      label: "Shell 2",
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Home/ });
    const terminalButton = screen.getByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_a"))).toBe(true));
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();
    runtimePoll!.callback();
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2));

    await fireEvent.click(screen.getByRole("button", { name: "Split terminal right" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Shell 2" })).toBeTruthy());

    stalePoll.resolve(runtimeWithTerminalSession());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.getByRole("button", { name: "Shell 2" })).toBeTruthy();

    runtimePoll!.callback();
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(4));
  });

  it("shows newly discovered terminal sessions without auto-splitting them", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const intervalCallbacks: Array<{ callback: () => void; delay: number | undefined }> = [];
    const setIntervalSpy = capturePollingIntervals(intervalCallbacks);
    const initialRuntime = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(initialRuntime.promise)
      .mockResolvedValueOnce(runtimeWithTwoTerminalSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Refresh workspace details" });
    initialRuntime.resolve(runtimeWithTerminalSession());
    await screen.findByRole("tab", { name: /Home/ });
    const terminalButton = screen.getByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_a"))).toBe(true));
    const runtimePoll = intervalCallbacks.find((interval) => interval.delay === 3000);
    expect(runtimePoll).toBeTruthy();

    runtimePoll!.callback();
    await waitFor(() => expect(screen.getByRole("button", { name: "Shell 2" })).toBeTruthy());
    expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(false);

    await fireEvent.click(screen.getByRole("button", { name: "Shell 2" }));

    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(true));
    setIntervalSpy.mockRestore();
  });

  it("ignores older runtime responses after terminal cleanup refreshes", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const initialRuntime = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    const staleRefresh = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    const freshRefresh = deferred<ReturnType<typeof runtimeWithTerminalSession>>();
    mocks.getWorkspaceRuntime
      .mockReturnValueOnce(initialRuntime.promise)
      .mockReturnValueOnce(staleRefresh.promise)
      .mockReturnValueOnce(freshRefresh.promise);
    mocks.launchWorkspaceSession.mockResolvedValue(relaunchedShellSession);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Refresh workspace details" });
    initialRuntime.resolve(runtimeWithTerminalSession());
    const terminalButton = await screen.findByRole("button", {
      name: "Open terminal panel",
    });
    await fireEvent.click(terminalButton);
    await waitFor(() => expect(sockets).toHaveLength(1));

    sockets[0]!.onmessage?.(
      new MessageEvent("message", {
        data: JSON.stringify({ type: "exited", code: 0 }),
      }),
    );
    await screen.findByRole("button", { name: "Open terminal panel" });

    await fireEvent.click(screen.getAllByRole("button", { name: "New terminal" })[0]!);
    freshRefresh.resolve(runtimeWithTerminalSession(relaunchedShellSession));
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes("ws-1_shell_b"))).toBe(true));

    staleRefresh.resolve(runtimeWithTerminalSession());
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(sockets.filter((socket) => socket.url.includes("ws-1_shell_a"))).toHaveLength(1);
    expect(sockets.filter((socket) => socket.url.includes("ws-1_shell_b"))).toHaveLength(1);
  });

  it("moves a workflow shell back into the terminal panel", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Open terminal panel",
      }),
    );
    await waitFor(() => expect(sockets).toHaveLength(1));
    expect(screen.getByRole("button", { name: "Focus Shell" }).getAttribute("draggable")).toBe("true");
    await fireEvent.click(screen.getByRole("button", { name: "Move Shell to workflow" }));

    await screen.findByRole("tab", { name: /Shell/ });
    await fireEvent.click(screen.getByRole("button", { name: "Move Shell to terminal" }));

    await waitFor(() => expect(screen.queryByRole("tab", { name: /Shell/ })).toBeNull());
    expect(screen.getByRole("button", { name: "Focus Shell" })).toBeTruthy();
  });

  it("keeps one live terminal while a shell moves between the terminal panel and the workflow area", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Open terminal panel",
      }),
    );
    await waitFor(() => expect(sockets).toHaveLength(1));
    const socket = sockets[0]!;
    expect(socket.url).toContain("ws-1_shell_a");

    await fireEvent.click(screen.getByRole("button", { name: "Move Shell to workflow" }));
    await screen.findByRole("tab", { name: /Shell/ });
    await fireEvent.click(screen.getByRole("button", { name: "Move Shell to terminal" }));
    await waitFor(() => expect(screen.queryByRole("tab", { name: /Shell/ })).toBeNull());

    // One tmux attachment from start to finish. Both regions render the same
    // pooled terminal, so moving between them reparents it instead of tearing
    // the shell down and reattaching to a scrollback-less new one.
    expect(sockets.filter((candidate) => candidate.url.includes("ws-1_shell_a"))).toHaveLength(1);
    expect(socket.close).not.toHaveBeenCalled();
  });

  it("releases a connected docked terminal when the terminal panel closes", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await fireEvent.click(
      await screen.findByRole("button", {
        name: "Open terminal panel",
      }),
    );
    await waitFor(() => expect(sockets).toHaveLength(1));
    sockets[0]!.onopen();
    const hostKey = mountedSessions()[0]!.hostKey;

    await fireEvent.click(screen.getAllByRole("button", { name: "Close terminal panel" })[0]!);

    await waitFor(() => expect(isSessionClaimed(hostKey)).toBe(false));
    expect(mountedSessions().some((session) => session.hostKey === hostKey)).toBe(true);
    expect(sockets[0]!.close).not.toHaveBeenCalled();
  });

  it("shows a workspace sidebar collapse button", async () => {
    const onToggleSidebar = vi.fn();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        isSidebarToggleEnabled: true,
        onToggleSidebar,
      },
    });

    const collapseButton = await screen.findByRole("button", {
      name: "Collapse Workspaces sidebar",
    });

    await fireEvent.click(collapseButton);

    expect(onToggleSidebar).toHaveBeenCalledTimes(1);
  });

  it.each(["pull_request", "issue", "kata_task", "adhoc"] as const)(
    "offers Kata links for a %s workspace",
    async (itemType) => {
      window.__BASE_PATH__ = window.location.origin;
      const selectedWorkspace = {
        ...workspaceResponse,
        item_type: itemType,
        item_number: itemType === "adhoc" ? 0 : 7,
        associated_pr_number: null,
      };
      vi.stubGlobal(
        "fetch",
        vi.fn().mockImplementation((input: Request | URL | string) => {
          const url = input instanceof Request ? input.url : String(input);
          const { pathname } = new URL(url, "http://localhost");
          if (pathname.endsWith("/workspaces/ws-1")) {
            return Promise.resolve(Response.json(selectedWorkspace));
          }
          if (pathname.endsWith("/api/v1/workspaces")) {
            return Promise.resolve(Response.json({ workspaces: [selectedWorkspace] }));
          }
          return Promise.resolve(Response.json({}));
        }),
      );

      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1" },
        context: new Map([
          [
            STORES_KEY,
            {
              diff: mocks.diffStore,
              roborevDaemon: { isAvailable: () => false },
            },
          ],
        ]),
      });

      const kataButton = (await screen.findAllByRole("button", { name: "Kata" })).find((button) =>
        button.classList.contains("panel-toggle-btn"),
      );
      if (!kataButton) throw new Error("Kata details control was not rendered");
      await fireEvent.click(kataButton);

      expect(kataButton.classList.contains("active")).toBe(true);
      const panel = await screen.findByTestId("kata-links-panel");
      expect(JSON.parse(panel.getAttribute("data-subject") ?? "null")).toEqual({
        kind: "workspace",
        workspaceID: "ws-1",
      });
    },
  );

  it("remembers the selected details tab for each workspace", async () => {
    window.__BASE_PATH__ = window.location.origin;
    const adhocWorkspace = {
      ...workspaceResponse,
      id: "ws-2",
      item_type: "adhoc",
      item_number: 0,
      associated_pr_number: null,
      git_head_ref: "feature/adhoc",
      tmux_session: "kenn-forge-ws-2",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const url = input instanceof Request ? input.url : String(input);
        const { pathname } = new URL(url, "http://localhost");
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(workspaceResponse));
        }
        if (pathname.endsWith("/workspaces/ws-2")) {
          return Promise.resolve(Response.json(adhocWorkspace));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse, adhocWorkspace] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );

    const view = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
      context: new Map([
        [
          STORES_KEY,
          {
            diff: mocks.diffStore,
            roborevDaemon: { isAvailable: () => false },
          },
        ],
      ]),
    });

    const prButton = await screen.findByRole("button", { name: "PR" });
    await fireEvent.click(prButton);
    expect(prButton.classList.contains("active")).toBe(true);

    await view.rerender({ workspaceId: "ws-2" });
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "PR" })).toBeNull();
      expect(screen.getByRole("button", { name: "Diff" }).classList.contains("active")).toBe(true);
    });

    await view.rerender({ workspaceId: "ws-1" });
    await waitFor(() => expect(screen.getByRole("button", { name: "PR" }).classList.contains("active")).toBe(true));
  });

  it.each([
    ["pull_request", "PR"],
    ["issue", "Issue"],
  ] as const)("defaults a source-backed %s workspace to its item", async (itemType, label) => {
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    mocks.workspaceSidebarPreference = "item";
    const sourceWorkspace = { ...workspaceResponse, item_type: itemType };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const pathname = new URL(input instanceof Request ? input.url : String(input), "http://localhost").pathname;
        if (pathname.endsWith("/workspaces/ws-1")) return Promise.resolve(Response.json(sourceWorkspace));
        if (pathname.endsWith("/api/v1/workspaces"))
          return Promise.resolve(Response.json({ workspaces: [sourceWorkspace] }));
        return Promise.resolve(Response.json({}));
      }),
    );

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await waitFor(() => expect(screen.getByRole("button", { name: label }).classList.contains("active")).toBe(true));
  });

  it("keeps a saved workspace tab ahead of the configured item default", async () => {
    localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
    localStorage.setItem("kenn-forge-workspace-sidebar-tab:ws-1", "diff");
    mocks.workspaceSidebarPreference = "item";

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await waitFor(() => expect(screen.getByRole("button", { name: "Diff" }).classList.contains("active")).toBe(true));
  });

  it("disables middle-pane workspace controls while the selected workspace is deleting", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());
    const deleteRequest = deferred<Response>();
    const otherDeleteRequest = deferred<Response>();
    const otherWorkspaceResponse = {
      ...workspaceResponse,
      id: "ws-2",
      item_number: 8,
      worktree_path: "/tmp/worktree-2",
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-2")) {
        return otherDeleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/workspaces/ws-2")) {
        return Promise.resolve(Response.json(otherWorkspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse, otherWorkspaceResponse] }));
      }
      if (pathname.endsWith("/workspaces/ws-1/files") || pathname.endsWith("/workspaces/ws-2/files")) {
        return Promise.resolve(Response.json({ stale: false, whitespace_only_count: 0, files: [] }));
      }
      if (pathname.endsWith("/workspaces/ws-1/diff") || pathname.endsWith("/workspaces/ws-2/diff")) {
        return Promise.resolve(Response.json({ stale: false, whitespace_only_count: 0, files: [] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal(
      "confirm",
      vi.fn(() => true),
    );
    window.history.pushState({}, "", "/terminal/ws-1");

    const view = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Launch" });
    await fireEvent.click(screen.getByRole("button", { name: "Open terminal panel" }));
    const shellPaneButton = await screen.findByRole("button", { name: "Focus Shell" });
    expect(shellPaneButton.getAttribute("draggable")).toBe("true");

    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => {
          if (!(input instanceof Request)) return false;
          const { pathname } = new URL(input.url);
          return input.method === "DELETE" && pathname === "/api/v1/workspaces/ws-1";
        }),
      ).toBe(true);
    });

    expect(screen.getAllByRole("button", { name: "Launch" }).every((button) => button.hasAttribute("disabled"))).toBe(
      true,
    );
    expect(screen.getByRole("button", { name: "Diff" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "PR" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Reviews" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Terminal options" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Focus Shell" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Focus Shell" }).getAttribute("draggable")).toBe("false");
    expect(screen.getByRole("button", { name: "Rename Shell" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Move Shell to workflow" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Close Shell" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getAllByRole("button", { name: "Shell" }).every((button) => button.hasAttribute("disabled"))).toBe(
      true,
    );
    expect(
      screen.getAllByRole("button", { name: "Shell" }).every((button) => button.getAttribute("draggable") === "false"),
    ).toBe(true);

    window.history.pushState({}, "", "/terminal/ws-2");
    await view.rerender({ workspaceId: "ws-2" });
    // Wait for ws-2's data to be applied (Delete re-enables), not
    // merely for its request to be issued: handleDelete intentionally
    // ignores clicks during the in-place transition window, so
    // clicking earlier races the metadata response's microtasks.
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(false);
    });
    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => {
          if (!(input instanceof Request)) return false;
          const { pathname } = new URL(input.url);
          return input.method === "DELETE" && pathname === "/api/v1/workspaces/ws-2";
        }),
      ).toBe(true);
    });
    expect(screen.getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(true);

    window.history.pushState({}, "", "/terminal/ws-1");
    await view.rerender({ workspaceId: "ws-1" });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Focus Shell" }).hasAttribute("disabled")).toBe(true);
    });
    expect(screen.getByRole("button", { name: "Focus Shell" }).getAttribute("draggable")).toBe("false");
    expect(screen.getByRole("button", { name: "Rename Shell" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Move Shell to workflow" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Close Shell" }).hasAttribute("disabled")).toBe(true);

    otherDeleteRequest.resolve(new Response(null, { status: 204 }));
    deleteRequest.resolve(new Response(null, { status: 204 }));
    await waitFor(() => expect(window.location.pathname).toBe("/workspaces"));
  });

  it("renders persisted workspace deletion as progress instead of an interactive terminal", async () => {
    const deletingWorkspace = {
      ...workspaceResponse,
      status: "deleting",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const request = input instanceof Request ? input : new Request(input);
        const { pathname } = new URL(request.url);
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(deletingWorkspace));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [deletingWorkspace] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    expect(await screen.findByText("Deleting workspace...")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Launch" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Refresh workspace details" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("offers confirmed force-delete recovery for a persisted deletion failure", async () => {
    const failedWorkspace = {
      ...workspaceResponse,
      status: "deletion_failed",
      error_message: "Workspace deletion failed after stopping its runtime.",
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      const { pathname, searchParams } = new URL(request.url);
      if (request.method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        expect(searchParams.get("force")).toBe("true");
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(failedWorkspace));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [failedWorkspace] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");
    const onWorkspaceDeleted = vi.fn();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        workspaceHostKey: "member",
        onWorkspaceDeleted,
      },
    });

    expect(await screen.findByText(failedWorkspace.error_message)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Launch" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: "Force delete workspace" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Force delete" }));

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input, init]) => {
          const request = input instanceof Request ? input : new Request(input, init);
          const { pathname, searchParams } = new URL(request.url);
          return (
            request.method === "DELETE" &&
            pathname === "/api/v1/fleet/hosts/member/workspaces/ws-1" &&
            searchParams.get("force") === "true"
          );
        }),
      ).toBe(true);
    });
    await waitFor(() => expect(onWorkspaceDeleted).toHaveBeenCalledWith("ws-1", "member", workspaceItemIdentity));
  });

  it.each([
    ["workspaceSetupInProgress", "creating", "Setting up workspace..."],
    ["workspaceDeletionInProgress", "deleting", "Deleting workspace..."],
  ])("refreshes lifecycle state instead of offering force delete for %s", async (code, status, lifecycleMessage) => {
    let deletionAttempted = false;
    const lifecycleWorkspace = {
      ...workspaceResponse,
      status,
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      const { pathname } = new URL(request.url);
      if (request.method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        deletionAttempted = true;
        return Promise.resolve(
          Response.json(
            {
              code,
              detail:
                status === "creating"
                  ? "workspace setup is still in progress"
                  : "workspace deletion is already in progress",
              status: 409,
            },
            { status: 409 },
          ),
        );
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(deletionAttempted ? lifecycleWorkspace : workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(
          Response.json({
            workspaces: [deletionAttempted ? lifecycleWorkspace : workspaceResponse],
          }),
        );
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();

    expect(await screen.findByText(lifecycleMessage)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Force delete" })).toBeNull();
    expect(screen.queryByText("Workspace has uncommitted changes.")).toBeNull();
  });

  it("issues no delete until the confirmation is accepted, and none on cancel", async () => {
    // Delete removes a worktree whose unpushed commits go with it, from a
    // one-click strip button — so every entry point confirms first.
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string) => {
      const pathname = fetchPath(input);
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await screen.findByRole("button", { name: "Delete" });
    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    await screen.findByRole("button", { name: "Delete workspace" });
    const deleteIssued = () =>
      fetchMock.mock.calls.some(([input]) => input instanceof Request && input.method === "DELETE");
    expect(deleteIssued()).toBe(false);

    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "Delete workspace" })).toBeNull();
    });
    expect(deleteIssued()).toBe(false);
    expect(screen.getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(false);
  });

  it("drops a failed delete response after unmounting and remounting the workspace", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const deleteRequest = deferred<Response>();
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    const firstVisit = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input]) => input instanceof Request && input.method === "DELETE")).toBe(true);
    });

    firstVisit.unmount();
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });
    await screen.findByRole("button", { name: "Delete" });

    deleteRequest.resolve(Response.json({ detail: "Old workspace delete failed." }, { status: 500 }));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mocks.showFlash).not.toHaveBeenCalled();
  });

  it("surfaces uncertain delete feedback through the replacement workspace presenter", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const deleteRequest = Promise.withResolvers<Response>();
    let readsFail = false;
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return readsFail
          ? Promise.reject(new TypeError("workspace read unavailable"))
          : Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    const firstVisit = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input]) => input instanceof Request && input.method === "DELETE")).toBe(true);
    });

    firstVisit.unmount();
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("button", { name: "Delete" });
    readsFail = true;
    deleteRequest.reject(new TypeError("delete response unavailable"));

    await waitFor(() => {
      expect(mocks.showFlash).toHaveBeenCalledWith(
        "Could not confirm whether the delete completed. Retry will check workspace state before sending anything.",
        { tone: "danger" },
      );
    });
  });

  it("surfaces uncertain delete feedback after switching away and returning to the workspace", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const deleteRequest = Promise.withResolvers<Response>();
    const otherWorkspaceResponse = {
      ...workspaceResponse,
      id: "ws-2",
      item_number: 8,
      worktree_path: "/tmp/worktree-2",
    };
    let readsFail = false;
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return readsFail
          ? Promise.reject(new TypeError("workspace read unavailable"))
          : Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/workspaces/ws-2")) {
        return Promise.resolve(Response.json(otherWorkspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse, otherWorkspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    const view = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input]) => input instanceof Request && input.method === "DELETE")).toBe(true);
    });

    window.history.pushState({}, "", "/terminal/ws-2");
    await view.rerender({ workspaceId: "ws-2" });
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input]) => fetchPath(input).endsWith("/workspaces/ws-2"))).toBe(true);
    });
    readsFail = true;
    deleteRequest.reject(new TypeError("delete response unavailable"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.showFlash).not.toHaveBeenCalled();

    readsFail = false;
    window.history.pushState({}, "", "/terminal/ws-1");
    await view.rerender({ workspaceId: "ws-1" });

    await waitFor(() => {
      expect(mocks.showFlash).toHaveBeenCalledWith(
        "Could not confirm whether the delete completed. Retry will check workspace state before sending anything.",
        { tone: "danger" },
      );
    });
  });

  it("reports a successful delete even after switching to another workspace", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const deleteRequest = deferred<Response>();
    const otherWorkspaceResponse = {
      ...workspaceResponse,
      id: "ws-2",
      item_number: 8,
      worktree_path: "/tmp/worktree-2",
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/workspaces/ws-2")) {
        return Promise.resolve(Response.json(otherWorkspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse, otherWorkspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    const onWorkspaceDeleted = vi.fn();
    const view = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        onWorkspaceDeleted,
      },
    });

    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => {
          if (!(input instanceof Request)) return false;
          const { pathname } = new URL(input.url);
          return input.method === "DELETE" && pathname === "/api/v1/workspaces/ws-1";
        }),
      ).toBe(true);
    });

    // Switch to another workspace while the delete is still in flight.
    window.history.pushState({}, "", "/terminal/ws-2");
    await view.rerender({ workspaceId: "ws-2" });

    deleteRequest.resolve(new Response(null, { status: 204 }));

    // The server destroyed ws-1 regardless of the current selection:
    // inline claimants, tombstones, and route memory must still hear
    // about it, while navigation stays put on ws-2.
    await waitFor(() =>
      expect(onWorkspaceDeleted).toHaveBeenCalledWith("ws-1", undefined, {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
        number: 7,
        itemType: "pull_request",
      }),
    );
    expect(window.location.pathname).toBe("/terminal/ws-2");
  });

  it("reports a successful force delete even after switching to another workspace", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const forceDeleteRequest = deferred<Response>();
    const otherWorkspaceResponse = {
      ...workspaceResponse,
      id: "ws-2",
      item_number: 8,
      worktree_path: "/tmp/worktree-2",
    };
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname, searchParams } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        if (searchParams.get("force") === "true") {
          return forceDeleteRequest.promise;
        }
        return Promise.resolve(
          Response.json(
            {
              code: "worktreeDirty",
              detail: "Workspace has uncommitted changes.",
              status: 409,
            },
            { status: 409 },
          ),
        );
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/workspaces/ws-2")) {
        return Promise.resolve(Response.json(otherWorkspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse, otherWorkspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.pushState({}, "", "/terminal/ws-1");

    const onWorkspaceDeleted = vi.fn();
    const view = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        onWorkspaceDeleted,
      },
    });

    await screen.findByRole("button", { name: "Delete" });
    await clickDeleteAndConfirm();

    // The 409 opens the force-delete confirmation; confirming issues the
    // forced DELETE that stays in flight while the user switches away.
    const forceButton = await screen.findByRole("button", { name: "Force delete" });
    await fireEvent.click(forceButton);
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => {
          if (!(input instanceof Request)) return false;
          const { pathname, searchParams } = new URL(input.url);
          return (
            input.method === "DELETE" && pathname === "/api/v1/workspaces/ws-1" && searchParams.get("force") === "true"
          );
        }),
      ).toBe(true);
    });

    window.history.pushState({}, "", "/terminal/ws-2");
    await view.rerender({ workspaceId: "ws-2" });

    forceDeleteRequest.resolve(new Response(null, { status: 204 }));

    await waitFor(() =>
      expect(onWorkspaceDeleted).toHaveBeenCalledWith("ws-1", undefined, {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
        number: 7,
        itemType: "pull_request",
      }),
    );
    expect(window.location.pathname).toBe("/terminal/ws-2");
  });

  it("disables active workflow terminal input while the selected workspace is deleting", async () => {
    const deleteRequest = deferred<Response>();
    const fetchMock = vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : "GET");
      const { pathname } = new URL(url, "http://localhost");
      if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) {
        return deleteRequest.promise;
      }
      if (pathname.endsWith("/workspaces/ws-1")) {
        return Promise.resolve(Response.json(workspaceResponse));
      }
      if (pathname.endsWith("/api/v1/workspaces")) {
        return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
      }
      return Promise.resolve(Response.json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal(
      "confirm",
      vi.fn(() => true),
    );
    window.history.pushState({}, "", "/terminal/ws-1");

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Helper/ });
    await waitFor(() => expect(mocks.mockTerminalInstances.length).toBeGreaterThanOrEqual(1));
    expect(mocks.mockTerminalInstances.some((terminal) => terminal.options.disableStdin === true)).toBe(false);
    const terminalDataHandler = mocks.mockOnData.mock.calls.at(-1)?.[0] as ((data: string) => void) | undefined;
    expect(terminalDataHandler).toBeTypeOf("function");

    await clickDeleteAndConfirm();
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([input]) => {
          if (!(input instanceof Request)) return false;
          const { pathname } = new URL(input.url);
          return input.method === "DELETE" && pathname === "/api/v1/workspaces/ws-1";
        }),
      ).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(true);
    });
    sockets.forEach((socket) => socket.send.mockClear());
    terminalDataHandler?.("echo blocked");
    expect(sockets.every((socket) => socket.send.mock.calls.length === 0)).toBe(true);

    deleteRequest.resolve(new Response(null, { status: 204 }));
    await waitFor(() => expect(window.location.pathname).toBe("/workspaces"));
  });
  it("launches an explicitly queued target without a confirmation modal", async () => {
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime
      .mockResolvedValueOnce(runtimeWithCodexTarget())
      .mockResolvedValue(runtimeWithCodexTarget(true, [runningSession]));
    mocks.launchWorkspaceSession.mockResolvedValue(runningSession);

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await waitFor(() => {
      expect(mocks.launchWorkspaceSession).toHaveBeenCalledWith("ws-1", "codex", {
        hostKey: undefined,
        region: "workflow",
      });
    });
    expect(
      screen.queryByRole("dialog", {
        name: /Launch default agent/,
      }),
    ).toBeNull();
  });

  it("routes an agent session wheel gesture through the workspace terminal", async () => {
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(true, [runningSession]));

    const { container } = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    await waitFor(() => expect(sockets.length).toBeGreaterThan(0));
    const socket = sockets.find((candidate) => candidate.url.includes(runningSession.key));
    expect(socket).toBeDefined();
    socket!.send.mockClear();
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).not.toBeNull();

    const defaultAllowed = terminalContainer!.dispatchEvent(
      new WheelEvent("wheel", { bubbles: true, cancelable: true, deltaY: -120 }),
    );

    expect(defaultAllowed).toBe(false);
    await waitFor(() => expect(socket!.send).toHaveBeenCalled());
    const payload = socket!.send.mock.calls.at(-1)?.[0];
    expect(new TextDecoder().decode(payload)).toBe("\x1b[A");
  });

  it("keeps the empty-workspace launcher closed while an explicit launch starts", async () => {
    const launchRequest = deferred<typeof runningSession>();
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });

    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();
  });

  it("keeps an accepted create-and-launch intent across an empty refresh and remount", async () => {
    const eventSources = installEventSourceRecorder();
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockResolvedValue(runningSession);
    claimForPrs();

    const firstView = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });

    await waitFor(() => {
      expect(pendingWorkspaceLaunch("ws-1", undefined)).toMatchObject({
        phase: "awaiting_session",
        sessionKey: runningSession.key,
      });
    });
    expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();

    firstView.unmount();
    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(mocks.getWorkspaceRuntime.mock.calls.length).toBeGreaterThanOrEqual(3));
    expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();

    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(true, [runningSession]));
    await waitFor(() => {
      expect(eventSources.length).toBeGreaterThanOrEqual(2);
      expect(latestWorkspaceEventListeners(eventSources)["reconnect.stale"]).toBeTypeOf("function");
    });
    latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();

    await waitFor(() => expect(pendingWorkspaceLaunch("ws-1", undefined)).toBeNull());
    await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
    expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1);
  });

  it("continues accepted-launch reconciliation after the initiating view unmounts", async () => {
    const launchRequest = deferred<typeof runningSession>();
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);
    claimForPrs();

    const firstView = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1));

    vi.useFakeTimers();
    try {
      launchRequest.resolve(runningSession);
      await vi.advanceTimersByTimeAsync(0);
      expect(pendingWorkspaceLaunch("ws-1", undefined)).toMatchObject({
        phase: "awaiting_session",
        sessionKey: runningSession.key,
      });

      firstView.unmount();
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(true, [runningSession]));
      await vi.advanceTimersByTimeAsync(500);

      expect(pendingWorkspaceLaunch("ws-1", undefined)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the accepted-launch deadline active across transient runtime read failures", async () => {
    const launchRequest = deferred<typeof runningSession>();
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1));

    vi.useFakeTimers();
    try {
      mocks.getWorkspaceRuntime
        .mockRejectedValueOnce(new TypeError("runtime temporarily unavailable"))
        .mockResolvedValue(runtimeWithCodexTarget());
      launchRequest.resolve(runningSession);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(pendingWorkspaceLaunch("ws-1", undefined)).toBeNull();
      expect(mocks.showFlash).toHaveBeenCalledWith("Codex launched, but its session did not become available", {
        tone: "danger",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("settles an accepted launch that never appears after the reconciliation window", async () => {
    const launchRequest = deferred<typeof runningSession>();
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });

    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1));
    vi.useFakeTimers();
    try {
      launchRequest.resolve(runningSession);
      await vi.advanceTimersByTimeAsync(0);
      expect(pendingWorkspaceLaunch("ws-1", undefined)).toMatchObject({ phase: "awaiting_session" });

      await vi.advanceTimersByTimeAsync(15_000);
      expect(pendingWorkspaceLaunch("ws-1", undefined)).toBeNull();
      expect(mocks.showFlash).toHaveBeenCalledWith("Codex launched, but its session did not become available", {
        tone: "danger",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not apply a local launch intent to a same-ID fleet workspace", async () => {
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const path = fetchPath(input);
        if (path.endsWith("/fleet/hosts/member/workspaces/ws-1")) {
          return Promise.resolve(Response.json({ ...workspaceResponse, fleet_host_key: "member" }));
        }
        if (path.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", workspaceHostKey: "member", paneSurface: "prs" as const },
    });

    expect(await screen.findByRole("dialog", { name: "Launch a session" })).toBeTruthy();
    expect(mocks.launchWorkspaceSession).not.toHaveBeenCalled();
    expect(pendingWorkspaceLaunch("ws-1", undefined)?.targetKey).toBe("codex");
  });

  it("publishes an accepted launch for its workspace after navigating during launch", async () => {
    const launchRequest = deferred<typeof runningSession>();
    const workspaceB = { ...workspaceResponse, id: "ws-2", status: "provisioning" };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const path = fetchPath(input);
        if (path.endsWith("/workspaces/ws-1")) return Promise.resolve(Response.json(workspaceResponse));
        if (path.endsWith("/workspaces/ws-2")) return Promise.resolve(Response.json(workspaceB));
        if (path.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse, workspaceB] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);

    const view = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledWith("ws-1", "codex", expect.anything()));

    await view.rerender({ workspaceId: "ws-2" });
    await screen.findByText("Setting up workspace...");
    queueWorkspaceLaunch("ws-2", "codex", undefined);
    launchRequest.resolve(runningSession);

    await waitFor(() => {
      expect(pendingWorkspaceLaunch("ws-1", undefined)).toMatchObject({
        phase: "awaiting_session",
        sessionKey: runningSession.key,
      });
    });
    expect(pendingWorkspaceLaunch("ws-2", undefined)?.targetKey).toBe("codex");
  });

  it("selects an accepted launch after navigating away and back before settlement", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    const launchRequest = deferred<typeof runningSession>();
    const workspaceAReload = deferred<Response>();
    const runtimeReload = Promise.withResolvers<ReturnType<typeof runtimeWithCodexTarget>>();
    const workspaceB = { ...workspaceResponse, id: "ws-2", git_head_ref: "feature/two" };
    let holdWorkspaceA = false;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const path = fetchPath(input);
        if (path.endsWith("/workspaces/ws-1")) {
          return holdWorkspaceA ? workspaceAReload.promise : Promise.resolve(Response.json(workspaceResponse));
        }
        if (path.endsWith("/workspaces/ws-2")) return Promise.resolve(Response.json(workspaceB));
        if (path.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse, workspaceB] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);

    const view = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledWith("ws-1", "codex", expect.anything()));

    await view.rerender({ workspaceId: "ws-2" });
    await waitFor(() => expect(document.querySelector(".header-branch")?.textContent).toBe("feature/two"));

    holdWorkspaceA = true;
    mocks.getWorkspaceRuntime.mockClear();
    mocks.getWorkspaceRuntime.mockReturnValue(runtimeReload.promise);
    await view.rerender({ workspaceId: "ws-1" });
    await screen.findByText("Setting up workspace...");
    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledWith("ws-1", undefined));

    launchRequest.resolve(runningSession);
    await new Promise((resolve) => setTimeout(resolve, 0));
    workspaceAReload.resolve(Response.json(workspaceResponse));

    const sessionTab = await screen.findByRole("tab", { name: /Helper/ });
    expect(sessionTab.getAttribute("aria-selected")).toBe("true");
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes(runningSession.key))).toBe(true));

    runtimeReload.reject(new Error("runtime unavailable"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(sessionTab.getAttribute("aria-selected")).toBe("true");
    expect(mocks.showFlash).not.toHaveBeenCalled();
  });

  it("reacts when intent is queued after an already-ready workspace renders", async () => {
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockResolvedValue(runningSession);
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("tab", { name: "Home" });

    queueWorkspaceLaunch("ws-1", "codex", undefined);

    await waitFor(() => {
      expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1);
    });
  });

  it("allows an explicit fork-workspace launch", async () => {
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockResolvedValue(runningSession);
    const forkWorkspaceResponse = { ...workspaceResponse, mr_head_repo_kind: "fork" };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const url = input instanceof Request ? input.url : String(input);
        const { pathname } = new URL(url, "http://localhost");
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(forkWorkspaceResponse));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [forkWorkspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalled());
    expect(mocks.launchWorkspaceSession.mock.calls[0]?.[2]).toEqual({
      hostKey: undefined,
      region: "workflow",
    });
  });

  it("launches manually with only workspace display options", async () => {
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget());
    mocks.launchWorkspaceSession.mockResolvedValue({
      ...runningSession,
      key: "ws-1:codex",
      target_key: "codex",
      label: "Codex",
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: "Home" });
    await fireEvent.click(screen.getByRole("button", { name: "Launch" }));
    const popover = document.querySelector(".launch-popover");
    if (!popover) throw new Error("expected launch popover to open");
    await fireEvent.click(within(popover as HTMLElement).getByRole("button", { name: "Codex" }));

    await waitFor(() => {
      expect(mocks.launchWorkspaceSession).toHaveBeenCalledWith("ws-1", "codex", {
        hostKey: undefined,
        region: "workflow",
      });
    });
  });

  it("consumes unavailable explicit intent and flashes its reason", async () => {
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(false));
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("tab", { name: "Home" });
    expect(mocks.launchWorkspaceSession).not.toHaveBeenCalled();
    expect(mocks.showFlash).toHaveBeenCalledWith(expect.stringContaining("Codex is not configured"), {
      tone: "danger",
    });
    expect(discardWorkspaceLaunch("ws-1", undefined)).toBeNull();
  });

  it("consumes missing explicit intent and flashes a generic reason", async () => {
    queueWorkspaceLaunch("ws-1", "missing", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue({ launch_targets: [], sessions: [] });
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("tab", { name: "Home" });
    expect(mocks.launchWorkspaceSession).not.toHaveBeenCalled();
    expect(mocks.showFlash).toHaveBeenCalledWith(expect.stringContaining("is not available in this workspace"), {
      tone: "danger",
    });
    expect(discardWorkspaceLaunch("ws-1", undefined)).toBeNull();
  });

  it("consumes explicit intent without launching when a session exists", async () => {
    queueWorkspaceLaunch("ws-1", "codex", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithCodexTarget(true, [runningSession]));
    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    await screen.findByRole("tab", { name: /Helper/ });
    expect(mocks.launchWorkspaceSession).not.toHaveBeenCalled();
    expect(mocks.showFlash).not.toHaveBeenCalled();
    expect(discardWorkspaceLaunch("ws-1", undefined)).toBeNull();
  });

  it("publishes the session the pane is showing, so a keyboard command can promote it", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // The active workflow tab holds this session, so it is the one on screen.
    // Only the view can decide that; a palette command sees stores alone.
    await waitFor(() =>
      expect(activeHostedSession("prs")?.paneKey).toBe(sessionPaneKey("ws-1", undefined, "ws-1:helper")),
    );

    await fireEvent.click(await screen.findByRole("tab", { name: /Reviewer/ }));

    // Republished as the user moves around: the other session fills the pane now,
    // so a promote command must act on that one instead.
    await waitFor(() =>
      expect(activeHostedSession("prs")?.paneKey).toBe(sessionPaneKey("ws-1", undefined, "ws-1:reviewer")),
    );
  });

  it("hands its controls to the pane instead of a toolbar while embedded", async () => {
    claimForPrs();

    const { unmount } = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // One bar, not three: the pane's tab strip renders these controls, so a
    // toolbar here would be a second copy of them above the terminal.
    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    expect(document.querySelector(".workspace-toolbar")).toBeNull();

    // Unregistered on the way out, or a pane could open the controls of a
    // workspace no longer hosted there.
    unmount();
    await waitFor(() => expect(hostedWorkspaceControls()).toBeNull());
  });

  it("renders a sole embedded session without workspace or workflow chrome, but keeps its dock", async () => {
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(activeHostedSession("prs")?.label).toBe("Helper"));
    expect(document.querySelector(".header-bar")).toBeNull();
    expect(screen.queryByRole("tablist", { name: "Workflow group tabs" })).toBeNull();

    // The header bar and the one-tab strip only restated what the pane's own tab
    // already said. The dock is not chrome: collapsed to a row it is the only route
    // to a shell beside the agent, and without it a one-session workspace is a dead
    // end -- the user cannot get a second terminal at all.
    expect(screen.getByRole("region", { name: "Terminal panel" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open terminal panel" })).toBeTruthy();
  });

  it("keeps the inner session strip when the pane's own strip is dropped", async () => {
    claimForPrs();
    // The surface reports the workspace leaf as solo-chrome: its strip is gone,
    // so no tab above the terminal names the agent. Rendering the session bare
    // here leaves nothing on screen naming it - the inner strip is the one bar
    // that pane has.
    getPaneLayoutStore("prs").notePaneRender({
      activeInputTabKey: "workspace",
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["conversation", "workspace"],
      flattened: false,
      soloChromeTabs: ["workspace"],
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    expect(await screen.findAllByRole("tablist", { name: "Workflow group tabs" })).not.toHaveLength(0);
    expect(screen.getByRole("tab", { name: /Helper/ })).toBeTruthy();
    expect(document.querySelector(".sole-embedded-session")).toBeNull();
  });

  it("keeps the workflow strip for two embedded sessions", async () => {
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    expect(await screen.findAllByRole("tablist", { name: "Workflow group tabs" })).not.toHaveLength(0);
  });

  it("moves active input ownership only when standalone workflow focus moves", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await screen.findByRole("tab", { name: /Reviewer/ });
    const helperPane = document.querySelector<HTMLElement>('[data-pane-key="session:ws-1:helper"]')!;
    const reviewerPane = document.querySelector<HTMLElement>('[data-pane-key="session:ws-1:reviewer"]')!;
    const activePaneKey = () =>
      document.querySelector(".tabbed-panel-leaf.input-active [data-pane-key]")?.getAttribute("data-pane-key");
    expect(activePaneKey()).toBeUndefined();

    await fireEvent.pointerDown(reviewerPane);
    await fireEvent.wheel(reviewerPane);
    expect(activePaneKey()).toBeUndefined();

    await fireEvent.focusIn(reviewerPane);
    expect(activePaneKey()).toBe("session:ws-1:reviewer");

    await fireEvent.wheel(helperPane);
    await fireEvent.pointerDown(helperPane);
    expect(activePaneKey()).toBe("session:ws-1:reviewer");

    await fireEvent.focusIn(helperPane);
    expect(activePaneKey()).toBe("session:ws-1:helper");

    await fireEvent.focusOut(helperPane, { relatedTarget: document.body });
    expect(activePaneKey()).toBeUndefined();
  });

  it("moves workflow focus ownership while workspace actions are blocked", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });
    const reviewerPane = await waitFor(() => {
      const pane = document.querySelector<HTMLElement>('[data-pane-key="session:ws-1:reviewer"]');
      expect(pane).not.toBeNull();
      return pane!;
    });
    const activePaneKey = () =>
      document.querySelector(".tabbed-panel-leaf.input-active [data-pane-key]")?.getAttribute("data-pane-key");

    beginWorkspaceDeletion("ws-1", undefined);
    await fireEvent.focusIn(reviewerPane);

    expect(activePaneKey()).toBe("session:ws-1:reviewer");
    endWorkspaceDeletion("ws-1", undefined);
  });

  it("clears workflow focus ownership when the host is parked", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());

    const view = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1", hostVisible: true } });
    const reviewerPane = await waitFor(() => {
      const pane = document.querySelector<HTMLElement>('[data-pane-key="session:ws-1:reviewer"]');
      expect(pane).not.toBeNull();
      return pane!;
    });

    await fireEvent.focusIn(reviewerPane);
    expect(document.querySelectorAll(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveLength(1);

    await view.rerender({ workspaceId: "ws-1", hostVisible: false });

    expect(document.querySelectorAll(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveLength(0);
  });

  it("keeps a focused bottom dock active while its header opens the panel", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());

    render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    const toggle = await screen.findByRole("button", { name: "Open terminal panel" });
    const panel = toggle.closest(".terminal-panel");
    expect(panel).not.toBeNull();

    await fireEvent.focusIn(toggle);
    expect(panel?.classList.contains("input-active")).toBe(true);

    await fireEvent.click(toggle);
    expect(panel?.classList.contains("open")).toBe(true);
    expect(panel?.classList.contains("input-active")).toBe(true);
  });

  it("uses the renderer-validated outer owner for nested workflow chrome", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    const surfaceLayout = getPaneLayoutStore("prs");
    surfaceLayout.noteFocused("conversation");
    surfaceLayout.notePaneRender({
      activeInputTabKey: "conversation",
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["conversation", "workspace"],
      flattened: false,
      soloChromeTabs: [],
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await screen.findByRole("tab", { name: /Reviewer/ });
    const activeWorkflowLeaves = () => document.querySelectorAll(".workspace-stage .tabbed-panel-leaf.input-active");
    expect(activeWorkflowLeaves()).toHaveLength(0);

    // The renderer can select workspace as a fallback while persisted focus still
    // names a hidden or zoom-covered pane. Nested ownership follows this report,
    // not the stale focus history.
    surfaceLayout.notePaneRender({
      activeInputTabKey: "workspace",
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["workspace"],
      flattened: false,
      soloChromeTabs: ["workspace"],
    });
    await waitFor(() => expect(activeWorkflowLeaves()).toHaveLength(1));
  });

  it("gates the workspace sidebar shortcut on the renderer-validated owner", async () => {
    claimForPrs();
    const surfaceLayout = getPaneLayoutStore("prs");
    surfaceLayout.notePaneRender({
      activeInputTabKey: "conversation",
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["conversation", "workspace"],
      flattened: false,
      soloChromeTabs: [],
    });

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });
    await screen.findByRole("region", { name: "Workflow panes" });

    await fireEvent.keyDown(window, { key: "]", ctrlKey: true });
    expect(document.querySelector(".right-sidebar")).toBeNull();

    surfaceLayout.notePaneRender({
      activeInputTabKey: "workspace",
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["conversation", "workspace"],
      flattened: false,
      soloChromeTabs: ["workspace"],
    });
    await waitFor(() =>
      expect(document.querySelector(".workspace-stage .tabbed-panel-leaf.input-active")).not.toBeNull(),
    );
    await fireEvent.keyDown(window, { key: "]", ctrlKey: true });
    expect(document.querySelector(".right-sidebar")).not.toBeNull();

    await fireEvent.keyDown(window, { key: "]", ctrlKey: true });
    expect(document.querySelector(".right-sidebar")).toBeNull();

    const releaseModal = pushModalFrame("workspace-sidebar-shortcut-test", []);
    try {
      await fireEvent.keyDown(window, { key: "]", ctrlKey: true });
      expect(document.querySelector(".right-sidebar")).toBeNull();
    } finally {
      releaseModal();
    }
  });

  it("keeps the workspace header for a sole standalone session", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    await waitFor(() => expect(mocks.mockTerminalInstances.length).toBeGreaterThanOrEqual(1));
    expect(document.querySelector(".header-bar")).not.toBeNull();
  });

  it("offers the sole session's own rename and stop from the pane controls", async () => {
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));

    // Dropping the chrome took the session's own tab actions with it, so without
    // these a single-session workspace has no route to rename or stop the one thing
    // it is running.
    const controls = within(screen.getByRole("dialog", { name: "Workspace controls" }));
    await fireEvent.click(controls.getByRole("button", { name: "Rename session" }));
    expect(await screen.findByRole("dialog", { name: /Rename/ })).toBeTruthy();
  });

  it("leaves session and workspace actions to the chrome that already owns them", async () => {
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));

    // Two sessions means the header bar and the session strip are both on screen
    // with their own Delete and per-session actions. A second copy in here would be
    // a destructive action with two owners whose disabled and pending states drift.
    const controls = within(screen.getByRole("dialog", { name: "Workspace controls" }));
    expect(controls.queryByRole("button", { name: "Delete" })).toBeNull();
    expect(controls.queryByRole("button", { name: "Rename session" })).toBeNull();
  });

  it("names the branch in the popover and puts delete one click away in the strip", async () => {
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);

    // Deleting the worktree is what a maintainer reaches for once a PR is done, and
    // behind the popover it cost a click to open, a target to find, and a second
    // click. It sits in the strip beside the trigger now, and only there: two
    // Deletes with their own disabled and pending states is worse than one.
    const strip = await screen.findByRole("button", { name: /^Delete workspace / });
    expect(strip.closest("[role='dialog']")).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));
    const controls = within(screen.getByRole("dialog", { name: "Workspace controls" }));
    expect(controls.getByText("feature/session-exit").closest("code")).not.toBeNull();
    expect(controls.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("opens the session launcher directly from the workspace pane header", async () => {
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);

    expect(screen.queryByRole("dialog", { name: "Workspace controls" })).toBeNull();
    const launch = await screen.findByRole("button", { name: "Launch session" });
    const deleteWorkspace = screen.getByRole("button", { name: /^Delete workspace / });
    expect(launch.getAttribute("title")).toBe("Launch session");
    expect(launch.compareDocumentPosition(deleteWorkspace) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);

    await fireEvent.click(launch);
    expect(await screen.findByRole("dialog", { name: "Launch a session" })).toBeTruthy();
  });

  it("disables the header session launcher while workspace deletion is pending", async () => {
    const deleteRequest = deferred<Response>();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
        const method = init?.method ?? (input instanceof Request ? input.method : "GET");
        const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
        if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) return deleteRequest.promise;
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(workspaceResponse));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    const launch = await screen.findByRole("button", { name: "Launch session" });
    expect(launch.hasAttribute("disabled")).toBe(false);

    await clickDeleteAndConfirm(screen.getByRole("button", { name: /^Delete workspace / }));
    await waitFor(() => expect(launch.hasAttribute("disabled")).toBe(true));
  });

  it("does not auto-open the session launcher while workspace deletion empties the runtime", async () => {
    const deleteRequest = deferred<Response>();
    const eventSources = installEventSourceRecorder();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string, init?: RequestInit) => {
        const method = init?.method ?? (input instanceof Request ? input.method : "GET");
        const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
        if (method === "DELETE" && pathname.endsWith("/workspaces/ws-1")) return deleteRequest.promise;
        if (pathname.endsWith("/workspaces/ws-1")) {
          return Promise.resolve(Response.json(workspaceResponse));
        }
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    await screen.findByRole("button", { name: "Launch session" });

    await clickDeleteAndConfirm(screen.getByRole("button", { name: /^Delete workspace / }));
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
    await waitFor(() => expect(latestWorkspaceEventListeners(eventSources)["reconnect.stale"]).toBeTypeOf("function"));
    latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();

    await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();
  });

  it("blocks workspace actions while merge-triggered deletion is pending", async () => {
    beginWorkspaceDeletion("ws-1", undefined);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);

    const launch = await screen.findByRole("button", { name: "Launch session" });
    expect(launch.hasAttribute("disabled")).toBe(true);
    expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();
    endWorkspaceDeletion("ws-1", undefined);
  });

  it("leaves workspace strip actions off a broken workspace, whose error panel owns delete", async () => {
    // The strip actions only apply while the workspace is ready. A failed setup
    // cannot launch a session and renders its own Delete beside the Retry the user
    // is already looking at, so the header must offer neither shortcut.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
        if (/\/workspaces\/[^/]+$/.test(pathname)) {
          return Promise.resolve(
            Response.json({
              ...workspaceResponse,
              status: "error",
              error_message: "tmux session is no longer running: kenn-forge-ws-1",
            }),
          );
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });
    render(WorkspacePaneControls);

    await waitFor(() => expect(screen.getByText(/tmux session is no longer running/)).toBeTruthy());
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Delete workspace / })).toBeNull();
    expect(screen.queryByRole("button", { name: "Launch session" })).toBeNull();
  });

  it("carries the dock modes into the pane controls, since the header that held them is gone", async () => {
    // Collapse is the only control that puts the whole workspace away: the pane's own
    // close button hides one pane and leaves a promoted session on screen. Hiding the
    // header bar in a pane took collapse with it, so the popover has to carry it.
    const setMode = vi.fn();
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
        inlineDock: { getMode: () => "split" as const, setMode },
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    expect(document.querySelector(".header-bar")).toBeNull();
    render(WorkspacePaneControls);
    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));

    const controls = within(screen.getByRole("dialog", { name: "Workspace controls" }));
    await fireEvent.click(controls.getByRole("button", { name: "Expand Terminal" }));
    expect(setMode).toHaveBeenCalledWith("expanded");

    await fireEvent.click(controls.getByRole("button", { name: "Collapse Terminal" }));
    expect(setMode).toHaveBeenCalledWith("collapsed");
  });

  it("keeps each workspace's own preset apply pending while another one runs", async () => {
    // Two applies in flight at once. With a single owner slot, B's apply overwrote
    // A's and B finishing re-enabled A's control while A's sessions were still being
    // launched - which is an invitation to launch the whole preset twice. Driven on
    // the standalone tab, which is the only place presets are offered.
    serveAnyWorkspace();
    localStorage.setItem(
      "kenn-forge-workspace-layout-presets",
      JSON.stringify([
        {
          id: "preset-1",
          name: "Pair",
          createdAt: "2026-04-29T00:00:00Z",
          updatedAt: "2026-04-29T00:00:00Z",
          sessions: [{ sourceKey: "s1", targetKey: "helper", region: "workflow", label: "Helper" }],
          layout: JSON.parse(persistedSplitWorkflowLayout("s1")),
        },
      ]),
    );
    const launchA = deferred<typeof runningSession>();
    const launchB = deferred<typeof runningSession>();
    mocks.launchWorkspaceSession.mockReturnValueOnce(launchA.promise).mockReturnValueOnce(launchB.promise);
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());

    const { rerender } = render(WorkspaceTerminalView, { props: { workspaceId: "ws-1" } });

    const presetTrigger = () => screen.getByRole("button", { name: "Workflow presets" });
    async function applyPreset(): Promise<void> {
      await fireEvent.click(presetTrigger());
      await fireEvent.click(screen.getAllByRole("button", { name: /Pair/ })[0]!);
    }

    await waitFor(() => expect(presetTrigger()).toBeTruthy());
    await applyPreset();
    await waitFor(() => expect(presetTrigger().hasAttribute("disabled")).toBe(true));

    await rerender({ workspaceId: "ws-2" });
    await waitFor(() => expect(presetTrigger().hasAttribute("disabled")).toBe(false));
    await applyPreset();
    await waitFor(() => expect(presetTrigger().hasAttribute("disabled")).toBe(true));

    // B finishes first.
    launchB.resolve(runningSession);
    await waitFor(() => expect(presetTrigger().hasAttribute("disabled")).toBe(false));

    await rerender({ workspaceId: "ws-1" });
    await waitFor(() => expect(presetTrigger().hasAttribute("disabled")).toBe(true));

    launchA.resolve(runningSession);
  });

  it("leaves workflow presets out of an embedded workspace's controls", async () => {
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));

    // Presets compose a whole multi-session workflow, which is the standalone tab's
    // job; a pane hosts one workspace beside the thing being reviewed.
    const controls = within(screen.getByRole("dialog", { name: "Workspace controls" }));
    expect(controls.queryByRole("button", { name: "Workflow presets" })).toBeNull();
    expect(controls.getByRole("button", { name: "Launch session" })).toBeTruthy();
  });

  it("keeps a terminal settings save busy across a workspace switch", async () => {
    // Terminal font size is an app setting written through one single-flight
    // controller, so a save is in flight for every workspace at once. Keying the busy
    // flag by workspace reported the next one's controls free while the controller was
    // still refusing input - an enabled button that does nothing.
    serveAnyWorkspace();
    const save = deferred<{ terminal: { font_size: number } }>();
    mocks.mockUpdateSettings.mockReturnValueOnce(save.promise);
    claimForPrs();

    const { rerender } = render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });
    await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
    render(WorkspacePaneControls);
    await fireEvent.click(screen.getByRole("button", { name: "Workspace controls" }));
    await fireEvent.click(screen.getByRole("button", { name: "Increase terminal font size" }));
    await waitFor(() => expect(workspaceControlsBusy()).toBe(true));

    await rerender({ workspaceId: "ws-2", paneSurface: "prs" as const });
    await waitFor(() => expect(hostedWorkspaceControls()?.workspaceKey).toContain("ws-2"));
    expect(workspaceControlsBusy()).toBe(true);

    save.resolve({ terminal: { font_size: 15 } });
    await waitFor(() => expect(workspaceControlsBusy()).toBe(false));
  });

  it("keeps its own toolbar on the standalone Workspaces tab", async () => {
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
      },
    });

    // That tab's panes have no tab strip to hold the controls, so the bar stays
    // and nothing is published for a detail pane to render.
    await waitFor(() => expect(document.querySelector(".workspace-toolbar")).not.toBeNull());
    expect(hostedWorkspaceControls()).toBeNull();
  });

  describe("launcher overlay", () => {
    it("drops the Home tab in a pane and opens the launcher when nothing is running", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // The pane's one slot goes to a terminal, not to a surface only used to start
      // one; with nothing to show, the launcher is what opens instead of an empty
      // strip.
      await waitFor(() => expect(screen.getByRole("dialog", { name: /Launch a session/ })).toBeTruthy());
      expect(screen.queryByRole("tab", { name: "Home" })).toBeNull();
    });

    it("removes the automatic launcher at item intent and never remounts it during promotion", async () => {
      const launchRequest = deferred<typeof runningSession>();
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      mocks.launchWorkspaceSession.mockReturnValue(launchRequest.promise);
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", paneSurface: "prs" as const },
      });

      await screen.findByRole("dialog", { name: "Launch a session" });
      beginWorkspaceCreate(workspaceItemIdentity, "helper");

      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull());

      const launcherAppearances: Element[] = [];
      const selector = '[role="dialog"][aria-label="Launch a session"]';
      const observer = new MutationObserver((records) => {
        for (const record of records) {
          for (const node of record.addedNodes) {
            if (!(node instanceof Element)) continue;
            if (node.matches(selector)) launcherAppearances.push(node);
            launcherAppearances.push(...node.querySelectorAll(selector));
          }
        }
      });
      observer.observe(document.body, { childList: true, subtree: true });

      try {
        promoteWorkspaceCreateLaunch(workspaceItemIdentity, "ws-1", undefined);

        await waitFor(() => expect(mocks.launchWorkspaceSession).toHaveBeenCalledTimes(1));
        expect(screen.queryByRole("dialog", { name: "Launch a session" })).toBeNull();
        expect(launcherAppearances).toHaveLength(0);
      } finally {
        observer.disconnect();
      }
    });

    it("leaves a broken workspace's error on screen instead of covering it with the launcher", async () => {
      // A worktree whose setup failed, or whose tmux server dropped its session,
      // reports zero sessions for the same reason it reports an error. Auto-opening
      // the launcher there covered the error message - and its Retry and Delete, the
      // only two useful actions - with an invitation to start an agent inside
      // something that cannot run one.
      vi.stubGlobal(
        "fetch",
        vi.fn().mockImplementation((input: Request | URL | string) => {
          const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
          if (/\/workspaces\/[^/]+$/.test(pathname)) {
            return Promise.resolve(
              Response.json({
                ...workspaceResponse,
                status: "error",
                error_message: "tmux session is no longer running: kenn-forge-ws-1",
              }),
            );
          }
          return Promise.resolve(Response.json({}));
        }),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      await waitFor(() => expect(screen.getByText(/tmux session is no longer running/)).toBeTruthy());
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
    });

    it("stays away while a workspace is still being created", async () => {
      // Retry leaves the workspace "creating" while the runtime the view still holds
      // -- zero sessions, from before the failure -- says it has nothing to show.
      // Refusing only "error" would put the launcher over a half-built worktree.
      vi.stubGlobal(
        "fetch",
        vi.fn().mockImplementation((input: Request | URL | string) => {
          const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
          if (/\/workspaces\/[^/]+$/.test(pathname)) {
            return Promise.resolve(Response.json({ ...workspaceResponse, status: "creating" }));
          }
          return Promise.resolve(Response.json({}));
        }),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", paneSurface: "prs" as const },
      });
      render(WorkspacePaneControls);

      await waitFor(() => expect(mocks.getWorkspaceRuntime).toHaveBeenCalled());
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
      expect(screen.queryByRole("button", { name: "Launch session" })).toBeNull();
    });

    it("gives a recovered workspace its launcher back", async () => {
      // The view withdrew the launcher itself when the workspace broke under it, so
      // the once-per-workspace marker has to come off with it. Kept, it would spend
      // the workspace's one automatic launcher on a state that refused to show it,
      // and the workspace that recovers -- setup finished, still nothing running --
      // would come back with nothing on screen and no way to know why.
      const eventSources = installEventSourceRecorder();
      let status = "ready";
      vi.stubGlobal(
        "fetch",
        vi.fn().mockImplementation((input: Request | URL | string) => {
          const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
          if (/\/workspaces\/[^/]+$/.test(pathname)) {
            return Promise.resolve(
              Response.json({
                ...workspaceResponse,
                status,
                error_message: status === "error" ? "tmux session is no longer running: kenn-forge-ws-1" : null,
              }),
            );
          }
          return Promise.resolve(Response.json({}));
        }),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: { workspaceId: "ws-1", paneSurface: "prs" as const },
      });
      await waitFor(() => expect(screen.getByRole("dialog", { name: /Launch a session/ })).toBeTruthy());

      // The tmux server drops the session out from under a workspace the view is
      // already showing a launcher for.
      status = "error";
      await waitFor(() =>
        expect(latestWorkspaceEventListeners(eventSources)["reconnect.stale"]).toBeTypeOf("function"),
      );
      latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();
      await waitFor(() => expect(screen.getByText(/tmux session is no longer running/)).toBeTruthy());
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();

      status = "ready";
      latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();
      await waitFor(() => expect(screen.getByRole("dialog", { name: /Launch a session/ })).toBeTruthy());
    });

    it("leaves a docked terminal alone instead of covering it with the launcher", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // The sole-session surface replaces the dock panel without changing the
      // launcher's rule: the terminal is on screen, so the overlay stays away.
      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
    });

    it("keeps the Home tab on the standalone Workspaces tab", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
        },
      });

      // That tab has room for it, and its chrome is out of scope here.
      expect(await screen.findByRole("tab", { name: "Home" })).toBeTruthy();
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
    });

    it("leaves a running workspace's sessions on screen instead of the launcher", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-1", persistedSplitWorkflowLayout("ws-1:helper"));
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // A remembered Home tab names a tab that does not exist here, so the session
      // takes its place directly rather than the overlay covering a live terminal.
      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
    });

    it("closes on a successful launch and stays open when one fails", async () => {
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      mocks.launchWorkspaceSession.mockRejectedValueOnce(new Error("helper not on PATH"));
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const dialog = await screen.findByRole("dialog", { name: /Launch a session/ });
      await fireEvent.click(within(dialog).getByRole("button", { name: /Helper/ }));

      // A failed launch leaves nothing to show, so closing the overlay would strand
      // the user on an empty pane with the error out of sight.
      await waitFor(() => expect(mocks.showFlash).toHaveBeenCalled());
      expect(screen.getByRole("dialog", { name: /Launch a session/ })).toBeTruthy();

      mocks.launchWorkspaceSession.mockResolvedValueOnce(runningSession);
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
      await fireEvent.click(within(dialog).getByRole("button", { name: /Helper/ }));

      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
    });

    it("takes back an auto-opened launcher once a session shows up", async () => {
      const eventSources = installEventSourceRecorder();
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      await screen.findByRole("dialog", { name: /Launch a session/ });

      // A reconnect (or a first runtime load that lands before its sessions do)
      // reports zero sessions for a moment. The launcher opened over that gap is
      // ours to take back, or it sits on top of the terminal it stood in for.
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
      await waitFor(() =>
        expect(latestWorkspaceEventListeners(eventSources)["reconnect.stale"]).toBeTypeOf("function"),
      );
      latestWorkspaceEventListeners(eventSources)["reconnect.stale"]?.();

      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
    });

    it("keeps a successful launch when the follow-up runtime reload fails", async () => {
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const dialog = await screen.findByRole("dialog", { name: /Launch a session/ });
      const launchRequest = deferred<typeof runningSession>();
      const launchStarted = deferred<void>();
      mocks.launchWorkspaceSession.mockImplementationOnce(() => {
        launchStarted.resolve();
        return launchRequest.promise;
      });
      await fireEvent.click(within(dialog).getByRole("button", { name: /Helper/ }));
      await launchStarted.promise;
      const runtimeReload = Promise.withResolvers<ReturnType<typeof runtimeWithLaunchTargetsOnly>>();
      mocks.getWorkspaceRuntime.mockReturnValueOnce(runtimeReload.promise);
      launchRequest.resolve(runningSession);

      // The launch response already identifies the running session. A redundant
      // read must not delay the confirmed session or keep the picker open.
      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
      runtimeReload.reject(new Error("runtime unavailable"));
      await new Promise((resolve) => setTimeout(resolve, 0));

      expect(document.querySelector(".sole-embedded-session")).not.toBeNull();
      expect(mocks.showFlash).not.toHaveBeenCalled();
    });

    it("replays a successful launch after remount without discarding a retained peer", async () => {
      mocks.getWorkspaceRuntime.mockResolvedValue({
        ...runtimeWithLaunchTargetsOnly(),
        sessions: [reviewerSession],
      });
      claimForPrs();

      const firstView = render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });
      await waitFor(() => expect(hostedWorkspaceControls()).not.toBeNull());
      await waitFor(() => expect(mountedSessions()).toHaveLength(1));
      await waitFor(() => expect(sockets).toHaveLength(1));
      const peerHostKey = mountedSessions()[0]!.hostKey;
      const peerSocket = sockets[0]!;
      await waitFor(() => expect(peerSocket.runListenersAttached).toBe(true));
      render(WorkspacePaneControls);

      await fireEvent.click(screen.getByRole("button", { name: "Launch session" }));
      const dialog = await screen.findByRole("dialog", { name: /Launch a session/ });
      const launchRequest = deferred<typeof runningSession>();
      const launchStarted = deferred<void>();
      mocks.launchWorkspaceSession.mockImplementationOnce(() => {
        launchStarted.resolve();
        return launchRequest.promise;
      });
      await fireEvent.click(within(dialog).getByRole("button", { name: /Helper/ }));
      await launchStarted.promise;

      const runtimeReload = Promise.withResolvers<ReturnType<typeof runtimeWithLaunchTargetsOnly>>();
      mocks.getWorkspaceRuntime.mockReturnValue(runtimeReload.promise);
      await firstView.rerender({
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
        showView: false,
      });
      await waitFor(() => expect(isSessionClaimed(peerHostKey)).toBe(false));
      expect(peerSocket.close).not.toHaveBeenCalled();
      launchRequest.resolve(runningSession);
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(peerSocket.close).not.toHaveBeenCalled();

      await firstView.rerender({
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
        showView: true,
      });

      await waitFor(() => expect(activeHostedSession("prs")?.label).toBe("Helper"));
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
      expect(mountedSessions().some((session) => session.hostKey === peerHostKey)).toBe(true);
      expect(peerSocket.close).not.toHaveBeenCalled();

      runtimeReload.reject(new Error("runtime unavailable"));
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(activeHostedSession("prs")?.label).toBe("Helper");
      expect(mountedSessions().some((session) => session.hostKey === peerHostKey)).toBe(true);
      expect(peerSocket.close).not.toHaveBeenCalled();
    });

    it("keeps a successful launch when the follow-up reload returns an API problem", async () => {
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const dialog = await screen.findByRole("dialog", { name: /Launch a session/ });
      const launchRequest = deferred<typeof runningSession>();
      const launchStarted = deferred<void>();
      mocks.launchWorkspaceSession.mockImplementationOnce(() => {
        launchStarted.resolve();
        return launchRequest.promise;
      });
      await fireEvent.click(within(dialog).getByRole("button", { name: /Helper/ }));
      await launchStarted.promise;
      mocks.getWorkspaceRuntime.mockResolvedValueOnce(
        Response.json({ code: "serviceUnavailable", detail: "Runtime authority is unavailable" }, { status: 503 }),
      );
      launchRequest.resolve(runningSession);

      await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
      expect(mocks.showFlash).not.toHaveBeenCalled();
    });

    it("does not carry an open launcher into the next workspace", async () => {
      serveAnyWorkspace();
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      const { rerender } = render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      await screen.findByRole("dialog", { name: /Launch a session/ });

      // One embedded view serves every selection on the surface, so the overlay
      // has to belong to the workspace it was opened for - otherwise it covers the
      // next one's live terminal, and the once-per-workspace guard would refuse to
      // open the launcher that workspace actually needs.
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
      await rerender({ workspaceId: "ws-2", paneSurface: "prs" as const });

      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());
    });

    it("auto-opens again for a workspace visited after another one", async () => {
      serveAnyWorkspace();
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());
      claimForPrs();

      const { rerender } = render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // Dismissed for ws-1, so ws-1 must not get another one...
      await screen.findByRole("dialog", { name: /Launch a session/ });
      await fireEvent.keyDown(window, { key: "Escape" });
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());

      // ...but ws-2 has never been offered one.
      await rerender({ workspaceId: "ws-2", paneSurface: "prs" as const });
      await screen.findByRole("dialog", { name: /Launch a session/ });
      await fireEvent.keyDown(window, { key: "Escape" });
      await waitFor(() => expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull());

      await rerender({ workspaceId: "ws-1", paneSurface: "prs" as const });

      // Back on ws-1, whose launcher the user already dismissed. A single-slot
      // memory forgets ws-1 the moment ws-2 is offered one, and reopening here
      // traps the user in an overlay they closed.
      // Settled: the runtime for ws-1 has been applied again and nothing reopened.
      await waitFor(() => expect(document.querySelector(".terminal-view")).not.toBeNull());
      expect(screen.queryByRole("dialog", { name: /Launch a session/ })).toBeNull();
    });

    it("hands the palette an opener only while a pane is hosting the workspace", async () => {
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithStaleSession());
      claimForPrs();

      const { unmount } = render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // A palette command sees stores, not components, and the overlay state lives
      // in the view.
      await waitFor(() => expect(hostedWorkspaceLauncher("prs")).not.toBeNull());
      expect(hostedWorkspaceLauncher("issues")).toBeNull();

      unmount();
      await waitFor(() => expect(hostedWorkspaceLauncher("prs")).toBeNull());
    });
  });

  it("selects a replayed launch on the standalone workspace before runtime hydration", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithLaunchTargetsOnly());

    const firstView = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
    });

    await screen.findByRole("tab", { name: "Home" });
    const launchRequest = deferred<typeof runningSession>();
    const launchStarted = deferred<void>();
    mocks.launchWorkspaceSession.mockImplementationOnce(() => {
      launchStarted.resolve();
      return launchRequest.promise;
    });
    const launchButton = screen.getByRole("button", { name: "Launch" });
    const launchMenu = launchButton.closest(".launch-menu");
    expect(launchMenu).not.toBeNull();
    await fireEvent.click(launchButton);
    await fireEvent.click(within(launchMenu as HTMLElement).getByRole("button", { name: "Helper" }));
    await launchStarted.promise;

    firstView.unmount();
    launchRequest.resolve(runningSession);
    await new Promise((resolve) => setTimeout(resolve, 0));

    const workspaceReload = Promise.withResolvers<Response>();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: Request | URL | string) => {
        const { pathname } = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
        if (pathname.endsWith("/workspaces/ws-1")) return workspaceReload.promise;
        if (pathname.endsWith("/api/v1/workspaces")) {
          return Promise.resolve(Response.json({ workspaces: [workspaceResponse] }));
        }
        return Promise.resolve(Response.json({}));
      }),
    );
    const runtimeReload = Promise.withResolvers<ReturnType<typeof runtimeWithLaunchTargetsOnly>>();
    mocks.getWorkspaceRuntime.mockReturnValue(runtimeReload.promise);
    render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1" },
    });

    await new Promise((resolve) => setTimeout(resolve, 0));
    workspaceReload.resolve(Response.json(workspaceResponse));

    const sessionTab = await screen.findByRole("tab", { name: /Helper/ });
    expect(sessionTab.getAttribute("aria-selected")).toBe("true");
    await waitFor(() => expect(sockets.some((socket) => socket.url.includes(runningSession.key))).toBe(true));

    runtimeReload.reject(new Error("runtime unavailable"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(sessionTab.getAttribute("aria-selected")).toBe("true");
    expect(mocks.showFlash).not.toHaveBeenCalled();
  });

  it("keeps its toolbar when the detail surface is flattened", async () => {
    claimForPrs();
    // What a narrow detail surface reports: one strip for every pane, per-leaf
    // chrome suppressed, so the pane has nowhere to hang a controls button and no
    // tab of its own to name the session.
    getPaneLayoutStore("prs").notePaneRender({
      activeInputTabKey: "workspace",
      flattened: true,
      editableTabs: [],
      onScreenTabs: ["workspace"],
      soloChromeTabs: [],
    });
    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // Even with a single session, dropping the chrome here would strip the only
    // route to presets, zoom, terminal options, launch and delete.
    await waitFor(() => expect(document.querySelector(".workspace-toolbar")).not.toBeNull());
    expect(document.querySelector(".header-bar")).not.toBeNull();
  });

  it("publishes the dock's session while the dock is open", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    await waitFor(() =>
      expect(activeHostedSession("prs")?.paneKey).toBe(sessionPaneKey("ws-1", undefined, "ws-1_shell_a")),
    );
  });

  it("shows a sole session whose dock was collapsed, and treats it as current", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      JSON.stringify({
        ...JSON.parse(persistedSplitWorkflowLayout("ws-1_shell_a", "terminal")),
        open: false,
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // A pane with one session shows it whatever region it was parked in: the
    // alternative is a pane rendering nothing but a collapsed dock bar.
    await waitFor(() => expect(document.querySelector(".sole-embedded-session")).not.toBeNull());
    // And it must be the current one, or the keyboard commands report no session to
    // promote while the user is looking straight at it.
    await waitFor(() => expect(activeHostedSession("prs")?.label).toBe("Shell"));
  });

  it("mounts a session the workflow tree is already showing, with no click first", async () => {
    // Opening a PR whose workspace already has an agent running: its session is the
    // active tab of a workflow leaf before the user touches anything. Mounting used
    // to happen only in the tab strip's select handler, so that pane came up empty
    // and stayed empty until something re-selected the tab -- which read as a broken
    // pane rather than one that needed a click.
    //
    // TWO sessions on purpose. With one, the pane renders it through the sole-session
    // path, which goes nowhere near the workflow tree - so a single session would pass
    // this whether the mounting effect exists or not.
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      JSON.stringify({
        version: 1,
        open: false,
        dock: "bottom",
        height: 300,
        activeSessionKey: null,
        tree: null,
        sessionRegions: { "ws-1:helper": "workflow", "ws-1:reviewer": "workflow" },
        workflowMode: "tabs",
        workflowTree: {
          type: "split",
          id: "wf-split",
          direction: "horizontal",
          ratio: 0.5,
          first: {
            type: "leaf",
            id: "wf-helper",
            tabs: ["session:ws-1:helper"],
            activeTabKey: "session:ws-1:helper",
          },
          second: {
            type: "leaf",
            id: "wf-reviewer",
            tabs: ["session:ws-1:reviewer"],
            activeTabKey: "session:ws-1:reviewer",
          },
        },
        terminalGroups: [],
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // One per rendered leaf: what the tree shows is what has a terminal.
    await waitFor(() => expect(document.querySelectorAll(".session-terminal-slot").length).toBe(2));
    expect(document.querySelector(".sole-embedded-session")).toBeNull();
  });

  it("publishes a collapsed dock's session without making it current", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      JSON.stringify({
        version: 1,
        open: false,
        dock: "bottom",
        height: 300,
        activeSessionKey: "ws-1_shell_a",
        tree: { type: "leaf", id: "dock-leaf", sessionKey: "ws-1_shell_a" },
        sessionRegions: { "ws-1:helper": "workflow", "ws-1_shell_a": "terminal" },
        workflowMode: "tabs",
        workflowTree: { type: "leaf", id: "wf-leaf", tabKey: "session:ws-1:helper" },
      }),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue({
      launch_targets: [],
      sessions: [runningSession, runningShellSession],
    });
    claimForPrs();

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // Two sessions, so the pane keeps its chrome and the collapsed dock really does
    // render nothing: promoting its session by keyboard would move a terminal the
    // user cannot see. Its pane is still offered, since promoting from the strip is
    // a different, deliberate act.
    await waitFor(() => expect(getInlineWorkspaceController("prs").promotableSessions()).toHaveLength(2));
    expect(activeHostedSession("prs")?.label).toBe("Helper");
  });

  it("releases external input ownership when the hosted view leaves a surface", async () => {
    const layout = getPaneLayoutStore("prs");
    const { rerender } = render(WorkspaceTerminalView, {
      props: { workspaceId: "ws-1", paneSurface: "prs" as const },
    });
    layout.setExternalInputActive(true);
    expect(layout.externalInputActive()).toBe(true);

    await rerender({ workspaceId: "ws-1", paneSurface: undefined });

    expect(layout.externalInputActive()).toBe(false);
  });

  describe("promoted sessions", () => {
    it("leaves a connected focused workflow terminal to the pool during promotion", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:helper");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedTwoSessionWorkflowLayout("ws-1:helper", "ws-1:reviewer"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
      claimForPrs();
      noteWorkspacePaneRendered("prs");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const terminal = await waitFor(() => {
        const element = document.querySelector<HTMLElement>(
          '[data-pane-key="session:ws-1:helper"] .terminal-container',
        );
        expect(element).not.toBeNull();
        return element!;
      });
      const focusTarget = document.createElement("textarea");
      terminal.append(focusTarget);
      focusTarget.focus();
      await waitFor(() =>
        expect(document.querySelector(".workspace-stage .tabbed-panel-leaf.input-active")).not.toBeNull(),
      );

      const layout = getPaneLayoutStore("prs");
      const paneKey = sessionPaneKey("ws-1", undefined, "ws-1:helper");
      expect(promoteSessionBesideWorkspace(layout, paneKey)).toBe(true);

      await waitFor(() => expect(layout.hasTab(paneKey)).toBe(true));
      await waitFor(() => expect(terminal.closest(".workspace-stage")).toBeNull());
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(document.activeElement).not.toBe(document.querySelector(".terminal-view"));
      expect(focusTarget.isConnected).toBe(true);
    });

    it("gives a parked row-only dock the sole workspace actions and live dialogs", async () => {
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        JSON.stringify({
          version: 1,
          open: true,
          dock: "bottom",
          height: 300,
          activeSessionKey: "ws-1_shell_a",
          tree: { type: "leaf", id: "dock-leaf", sessionKey: "ws-1_shell_a" },
          sessionRegions: {
            "ws-1:helper": "workflow",
            "ws-1_shell_a": "terminal",
          },
          workflowMode: "tabs",
          workflowTree: {
            type: "leaf",
            id: "wf-session",
            tabs: ["session:ws-1:helper"],
            activeTabKey: "session:ws-1:helper",
          },
          customSessionLabels: {},
        }),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue({
        launch_targets: [],
        sessions: [runningSession, runningShellSession],
      });
      claimForPrs();
      noteWorkspacePaneRendered("prs");
      const paneKey = promoteSession("prs", "ws-1:helper");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
          hostVisible: false,
        },
      });

      const controller = getInlineWorkspaceController("prs");
      await waitFor(() => expect(controller.workspacePaneRowOnly()).toBe(true));
      expect(controller.dockRow()).not.toBeNull();

      const externalDock = await screen.findByRole("region", { name: "Terminal panel" });
      const layout = getPaneLayoutStore("prs");
      expect(layout.externalInputActive()).toBe(false);
      await fireEvent.wheel(externalDock);
      await fireEvent.pointerDown(externalDock);
      expect(layout.externalInputActive()).toBe(false);
      expect(externalDock.classList.contains("input-active")).toBe(false);

      await fireEvent.focusIn(externalDock);
      expect(layout.externalInputActive()).toBe(true);
      expect(externalDock.classList.contains("input-active")).toBe(true);

      layout.noteFocused(paneKey);
      expect(layout.externalInputActive()).toBe(true);

      await fireEvent.focusOut(externalDock, { relatedTarget: document.body });
      expect(layout.externalInputActive()).toBe(false);
      expect(externalDock.classList.contains("input-active")).toBe(false);

      render(WorkspacePaneControls, { props: { showStripActions: false } });
      expect(screen.getAllByRole("button", { name: /^Delete workspace / })).toHaveLength(1);
      expect(screen.getAllByRole("button", { name: "Workspace controls" })).toHaveLength(2);

      await fireEvent.click(within(externalDock).getByRole("button", { name: /^Delete workspace / }));
      const deleteDialog = await screen.findByRole("dialog", { name: "Delete workspace?" });
      await fireEvent.click(within(deleteDialog).getByRole("button", { name: "Cancel" }));

      await fireEvent.click(within(externalDock).getByRole("button", { name: "Workspace controls" }));
      const controls = await screen.findByRole("dialog", { name: "Workspace controls" });
      await fireEvent.click(within(controls).getByRole("button", { name: "Launch session" }));
      const launcher = await screen.findByRole("dialog", { name: "Launch a session" });
      await fireEvent.click(within(launcher).getByRole("button", { name: "Close" }));

      await fireEvent.click(within(externalDock).getByRole("button", { name: "Move Shell to a pane" }));
      const shellPaneKey = sessionPaneKey("ws-1", undefined, "ws-1_shell_a");
      await waitFor(() => expect(layout.hasTab(shellPaneKey)).toBe(true));

      layout.demoteTab(shellPaneKey);
      layout.demoteTab(paneKey);
      await waitFor(() => expect(controller.workspacePaneRowOnly()).toBe(false));
      expect(controller.dockRow()).toBeNull();
    });

    it("masks a promoted session out of the workflow strip and gives back its placement on demote", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:reviewer");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedTwoSessionWorkflowLayout("ws-1:reviewer", "ws-1:helper"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
      const paneKey = promoteSession("prs", "ws-1:helper");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const reviewerTab = await screen.findByRole("tab", { name: /Reviewer/ });
      // The detail pane is showing this session, so the container must not show it
      // too: two slots for one terminal race for it and one renders empty.
      expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull();

      getPaneLayoutStore("prs").demoteTab(paneKey);

      const helperTab = await screen.findByRole("tab", { name: /Helper/ });
      // Its own leaf, not merged into the other session's. Masking must not prune
      // the stored tree, or a demotion returns the session to the region and loses
      // the place the user put it in.
      expect(helperTab.closest('[role="tablist"]')).not.toBe(reviewerTab.closest('[role="tablist"]'));
    });

    it("keeps a promoted session's terminal live without a tab of its own", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-1", persistedSplitWorkflowLayout("ws-1:helper"));
      promoteSession("prs", "ws-1:helper");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // Nothing in the container renders it, so only the pool can: a promoted
      // session that is not mounted leaves the detail pane's slot empty.
      await waitFor(() =>
        expect(sockets.some((socket) => socket.url.includes("/sessions/ws-1:helper/terminal"))).toBe(true),
      );
    });

    it("masks a promoted session out of the terminal dock too", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
      const paneKey = promoteSession("prs", "ws-1_shell_a");
      claimForPrs();
      noteWorkspacePaneRendered("prs");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      // A masked leaf must be pruned, not left rendering the dock's
      // session-unavailable placeholder for a session that is alive elsewhere -
      // and an emptied dock collapses rather than sitting open on nothing.
      await screen.findByRole("button", { name: "Open terminal panel" });
      expect(screen.queryByText("Session unavailable")).toBeNull();

      getPaneLayoutStore("prs").demoteTab(paneKey);
      // Back in the dock as the embedded pane's sole session, which renders
      // chrome-free: no dock row at all, and still no placeholder.
      await waitFor(() => {
        expect(document.querySelector(".terminal-panel")).toBeNull();
      });
      expect(screen.queryByText("Session unavailable")).toBeNull();
    });

    it("promotes a docked session from its own control", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
      );
      // Two, because the per-session header carrying this control only renders
      // once the dock holds more than one session.
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());
      claimForPrs();
      noteWorkspacePaneRendered("prs");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const promote = await screen.findByRole("button", { name: "Move Shell to a pane" });
      await fireEvent.click(promote);

      const layout = getPaneLayoutStore("prs");
      const paneKey = sessionPaneKey("ws-1", undefined, "ws-1_shell_a");
      expect(layout.hasTab(paneKey)).toBe(true);
      // Its own leaf beside the workspace pane, the same placement the palette
      // command uses: a tab stacked behind the workspace pane would look like the
      // control did nothing.
      expect(layout.leafIDForTab(paneKey)).not.toBe(layout.leafIDForTab("workspace"));
      // And masked out of the dock it came from; where the dock puts what is left
      // is the masking tests' subject, not this one's.
      await waitFor(() => expect(screen.queryByRole("button", { name: "Move Shell to a pane" })).toBeNull());
    });

    it("keeps the only docked session available to pane commands without dock chrome", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTerminalSession());
      claimForPrs();
      noteWorkspacePaneRendered("prs");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
          paneSurface: "prs" as const,
        },
      });

      const paneKey = sessionPaneKey("ws-1", undefined, "ws-1_shell_a");
      await waitFor(() =>
        expect(getInlineWorkspaceController("prs").promotableSessions()).toEqual([{ paneKey, label: "Shell" }]),
      );
      expect(screen.queryByRole("button", { name: "Move Shell to a pane" })).toBeNull();
      // The dock stays on screen in a chrome-free pane, except here: the sole session
      // IS the dock's, so the stage is already showing it. A dock underneath would
      // aim a second slot at the same registry key, and one terminal host cannot be
      // in two places at once.
      expect(screen.queryByRole("region", { name: "Terminal panel" })).toBeNull();
    });

    it("offers no promote control on the standalone Workspaces tab", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-1",
        persistedSplitWorkflowLayout("ws-1_shell_a", "terminal"),
      );
      mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoTerminalSessions());

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
        },
      });

      // The session's other controls are there, so the header rendered; only the
      // promote one is absent. No detail surface is hosting this workspace, so
      // there is no tree to promote into and the control would lead nowhere.
      await screen.findByRole("button", { name: "Move Shell to workflow" });
      expect(screen.queryByRole("button", { name: "Move Shell to a pane" })).toBeNull();
    });

    it("masks nothing on the standalone Workspaces tab, which has no detail panes", async () => {
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "home");
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-1", persistedSplitWorkflowLayout("ws-1:helper"));
      promoteSession("prs", "ws-1:helper");

      render(WorkspaceTerminalView, {
        props: {
          workspaceId: "ws-1",
        },
      });

      // Promotion is per surface. A session promoted in the PRs surface is still
      // at home here, and hiding it would leave it unreachable.
      expect(await screen.findByRole("tab", { name: /Helper/ })).toBeTruthy();
    });
  });
  it("demotes a promoted session dropped back on the workflow strip", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:reviewer");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:reviewer", "ws-1:helper"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());
    const paneKey = promoteSession("prs", "ws-1:helper");

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    // A pane has no Home tab, so the workspace's other session is the strip the
    // promoted one can be dropped back onto.
    const reviewerTab = await screen.findByRole("tab", { name: /Reviewer/ });
    expect(screen.queryByRole("tab", { name: /Helper/ })).toBeNull();
    await waitFor(() =>
      expect(sockets.some((socket) => socket.url.includes("/sessions/ws-1:helper/terminal"))).toBe(true),
    );
    const socket = sockets.find((candidate) => candidate.url.includes("/sessions/ws-1:helper/terminal"))!;

    // The pane's own drag, arriving from the surface's tree: same scope, and a
    // key in the canonical form the workspace tab does not use.
    const dataTransfer = fakeDataTransfer();
    startTabbedPanelTabDrag({ dataTransfer } as unknown as DragEvent, { scope: "detail:prs", tabKey: paneKey });
    const reviewerStrip = reviewerTab.closest('[role="tablist"]')!;
    await fireEvent.dragOver(reviewerStrip, { dataTransfer, clientX: 5 });
    await fireEvent.drop(reviewerStrip, { dataTransfer, clientX: 5 });
    clearActiveTabbedPanelDrag();

    // Demoted, and placed where it was dropped rather than back in the leaf it
    // came from: the drop names a target, so honoring the stored placement here
    // would ignore the gesture.
    const helperTab = await screen.findByRole("tab", { name: /Helper/ });
    expect(getPaneLayoutStore("prs").hasTab(paneKey)).toBe(false);
    expect(helperTab.closest('[role="tablist"]')).toBe(reviewerStrip);
    // Same shell, still attached: the drop reparents the pooled terminal into the
    // workflow slot rather than tearing it down and reattaching.
    expect(sockets.filter((candidate) => candidate.url.includes("/sessions/ws-1:helper/terminal"))).toHaveLength(1);
    expect(socket.close).not.toHaveBeenCalled();
  });

  it("demotes a promoted session dropped into a terminal split", async () => {
    const persisted = JSON.parse(persistedTwoSessionWorkflowLayout("ws-1:reviewer", "ws-1:helper")) as Record<
      string,
      unknown
    >;
    persisted.open = true;
    persisted.activeSessionKey = "ws-1_shell_a";
    persisted.tree = {
      type: "leaf",
      id: "dock-leaf",
      sessionKey: "ws-1_shell_a",
    };
    persisted.sessionRegions = {
      "ws-1:reviewer": "workflow",
      "ws-1:helper": "workflow",
      "ws-1_shell_a": "terminal",
    };
    localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-1", JSON.stringify(persisted));
    mocks.getWorkspaceRuntime.mockResolvedValue({
      launch_targets: [],
      sessions: [reviewerSession, runningSession, runningShellSession],
    });
    claimForPrs();
    noteWorkspacePaneRendered("prs");
    const paneKey = promoteSession("prs", "ws-1:helper");

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    const terminalTarget = await screen.findByRole("group", {
      name: "Shell split drop targets",
    });
    vi.spyOn(terminalTarget, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: 400,
      bottom: 300,
      width: 400,
      height: 300,
      toJSON: () => ({}),
    });

    const dataTransfer = fakeDataTransfer();
    startTabbedPanelTabDrag({ dataTransfer } as unknown as DragEvent, { scope: "detail:prs", tabKey: paneKey });
    await fireEvent.dragOver(terminalTarget, {
      dataTransfer,
      clientX: 200,
      clientY: 150,
    });
    await fireEvent.drop(terminalTarget, {
      dataTransfer,
      clientX: 200,
      clientY: 150,
    });
    clearActiveTabbedPanelDrag();

    expect(getPaneLayoutStore("prs").hasTab(paneKey)).toBe(false);
    expect(await screen.findByRole("button", { name: "Move Helper to workflow" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: /Reviewer/ })).toBeTruthy();
  });

  it("refuses a session pane dropped from another workspace", async () => {
    localStorage.setItem("kenn-forge-workspace-active-tab:ws-1", "session:ws-1:reviewer");
    localStorage.setItem(
      "kenn-forge-workspace-terminal-layout:ws-1",
      persistedTwoSessionWorkflowLayout("ws-1:reviewer", "ws-1:helper"),
    );
    mocks.getWorkspaceRuntime.mockResolvedValue(runtimeWithTwoWorkflowSessions());

    render(WorkspaceTerminalView, {
      props: {
        workspaceId: "ws-1",
        paneSurface: "prs" as const,
      },
    });

    const reviewerTab = await screen.findByRole("tab", { name: /Reviewer/ });
    const helperTab = await screen.findByRole("tab", { name: /Helper/ });
    const helperStrip = helperTab.closest('[role="tablist"]');
    // Same session key, another workspace. A session key is unique only within
    // its own workspace, so this must not move the local session of that name.
    const dataTransfer = fakeDataTransfer();
    startTabbedPanelTabDrag({ dataTransfer } as unknown as DragEvent, {
      scope: "detail:prs",
      tabKey: sessionPaneKey("ws-2", undefined, "ws-1:helper"),
    });
    const reviewerStrip = reviewerTab.closest('[role="tablist"]')!;
    await fireEvent.dragOver(reviewerStrip, { dataTransfer, clientX: 5 });
    await fireEvent.drop(reviewerStrip, { dataTransfer, clientX: 5 });
    clearActiveTabbedPanelDrag();

    expect(screen.getByRole("tab", { name: /Helper/ }).closest('[role="tablist"]')).toBe(helperStrip);
    expect(helperStrip).not.toBe(reviewerStrip);
  });
});
