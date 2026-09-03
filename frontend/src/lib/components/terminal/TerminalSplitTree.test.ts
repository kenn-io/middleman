import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { clearActiveTabbedPanelDrag, readTabbedPanelTabDrag } from "../shared/tabbed-panel-drag.js";
import { sessionPaneKey } from "../../stores/session-pane-key.js";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { ComponentProps } from "svelte";
import AppRuntimeHarness from "../../../test/AppRuntimeHarness.svelte";
import TerminalSplitTree from "./TerminalSplitTree.svelte";
import type { PaneNode } from "./terminal-layout";
import { clearActiveTerminalDrag, readRuntimeSessionDrag } from "./terminal-drag";
import { currentTerminalGeometryIntent, hasTerminalGeometryIntent } from "./terminalGeometryIntent.js";
import { isSessionSlotVisible, resetSessionHostForTest, sessionHostKey } from "../../stores/session-host.svelte.ts";

vi.mock("./TerminalPane.svelte", async () => ({
  default: (await import("./TerminalSplitTreeTestPane.svelte")).default,
}));

const sessions = [
  {
    key: "ws-1:shell-a",
    workspace_id: "ws-1",
    target_key: "plain_shell",
    label: "Shell A",
    kind: "plain_shell" as const,
    status: "running" as const,
    display_region: "panel",
    created_at: "2026-07-15T00:00:00Z",
  },
  {
    key: "ws-1:shell-b",
    workspace_id: "ws-1",
    target_key: "plain_shell",
    label: "Shell B",
    kind: "plain_shell" as const,
    status: "running" as const,
    display_region: "panel",
    created_at: "2026-07-15T00:01:00Z",
  },
  {
    key: "ws-1:shell-c",
    workspace_id: "ws-1",
    target_key: "plain_shell",
    label: "Shell C",
    kind: "plain_shell" as const,
    status: "running" as const,
    display_region: "panel",
    created_at: "2026-07-15T00:02:00Z",
  },
];

function leaf(id: string, sessionKey: string): PaneNode {
  return { type: "leaf", id, sessionKey };
}

function split(direction: "horizontal" | "vertical" = "horizontal"): PaneNode {
  return {
    type: "split",
    id: "split-1",
    direction,
    ratio: 0.4,
    first: leaf("leaf-a", sessions[0]!.key),
    second: leaf("leaf-b", sessions[1]!.key),
  };
}

function renderTerminalSplitTree(props: ComponentProps<typeof TerminalSplitTree>) {
  return render(AppRuntimeHarness, { props: { component: TerminalSplitTree, ...props } });
}

function mockRect(width = 1000, height = 600): void {
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
    width,
    height,
    x: 0,
    y: 0,
    top: 0,
    right: width,
    bottom: height,
    left: 0,
    toJSON: () => ({}),
  });
}

describe("TerminalSplitTree", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    );
  });

  afterEach(async () => {
    if (vi.isFakeTimers()) {
      vi.runOnlyPendingTimers();
      vi.useRealTimers();
    } else if (hasTerminalGeometryIntent()) {
      await new Promise((resolve) => setTimeout(resolve, 251));
    }
    cleanup();
    clearActiveTerminalDrag();
    clearActiveTabbedPanelDrag();
    resetSessionHostForTest();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("keeps every leaf of a split on screen, not only the focused one", async () => {
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: split(),
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
    });

    // Both leaves of a split are painted side by side, so both terminals have
    // to size themselves to their own pane. Only the focused one reporting as
    // visible leaves the other inert at whatever size it last held.
    for (const session of sessions.slice(0, 2)) {
      const key = sessionHostKey("ws-1", undefined, session.key, session.created_at);
      expect(isSessionSlotVisible(key), session.label).toBe(true);
    }
  });

  it("uses the launch target harness icon in an agent pane header", () => {
    const agentSessions = [
      {
        ...sessions[0]!,
        key: "ws-1:codex-review",
        target_key: "codex-review",
        label: "Review Agent",
        kind: "agent" as const,
      },
      sessions[1]!,
    ];
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: {
        ...split(),
        first: leaf("leaf-a", agentSessions[0]!.key),
      },
      sessions: agentSessions,
      displayLabels: {},
      activeSessionKey: agentSessions[0]!.key,
    });

    const header = screen.getByRole("group", { name: "Review Agent terminal pane" });
    expect(header.querySelector(".kit-harness-icon--openai")).not.toBeNull();
  });

  it("hides every leaf while the host is parked", async () => {
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: split(),
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      hostVisible: false,
    });

    for (const session of sessions.slice(0, 2)) {
      const key = sessionHostKey("ws-1", undefined, session.key, session.created_at);
      expect(isSessionSlotVisible(key), session.label).toBe(false);
    }
  });

  it("publishes and clears a detail-pane payload from a terminal leaf drag", async () => {
    const paneKey = sessionPaneKey("ws-1", undefined, sessions[0]!.key);
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: leaf("leaf-a", sessions[0]!.key),
      sessions: sessions.slice(0, 2),
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      dragScope: "detail:prs",
      paneKeyForSession: (sessionKey: string) => (sessionKey === sessions[0]!.key ? paneKey : null),
    });
    const data = new Map<string, string>();
    const dataTransfer = {
      effectAllowed: "none",
      setData: (type: string, value: string) => data.set(type, value),
      getData: (type: string) => data.get(type) ?? "",
    };
    const header = screen.getByRole("group", { name: "Shell A terminal pane" });

    await fireEvent.dragStart(header, { dataTransfer });
    expect(readRuntimeSessionDrag({ dataTransfer } as unknown as DragEvent, "ws-1")).toBe(sessions[0]!.key);
    expect(readTabbedPanelTabDrag({ dataTransfer } as unknown as DragEvent, "detail:prs")).toBe(paneKey);

    await fireEvent.dragEnd(header, { dataTransfer });
    expect(readRuntimeSessionDrag({ dataTransfer } as unknown as DragEvent, "ws-1")).toBeNull();
    expect(readTabbedPanelTabDrag({ dataTransfer } as unknown as DragEvent, "detail:prs")).toBeNull();
  });

  it("resizes a horizontal split with truthful pixel ARIA values", async () => {
    mockRect();
    const onRatioChange = vi.fn();
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: split(),
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      onRatioChange,
    });

    const handle = screen.getByRole("separator", { name: "Resize split" });
    expect(handle.getAttribute("aria-orientation")).toBe("vertical");
    expect(handle.getAttribute("aria-valuemin")).toBe("120");
    expect(handle.getAttribute("aria-valuemax")).toBe("880");
    expect(handle.getAttribute("aria-valuenow")).toBe("400");

    await fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(onRatioChange).toHaveBeenCalledWith("split-1", expect.closeTo(0.424));

    onRatioChange.mockClear();
    await fireEvent.keyDown(handle, { key: "ArrowDown" });
    expect(onRatioChange).not.toHaveBeenCalled();
  });

  it("marks effective pointer and keyboard split changes as deliberate geometry", async () => {
    mockRect();
    Object.defineProperties(HTMLElement.prototype, {
      setPointerCapture: { configurable: true, value: vi.fn() },
      hasPointerCapture: { configurable: true, value: vi.fn(() => true) },
      releasePointerCapture: { configurable: true, value: vi.fn() },
    });
    vi.useFakeTimers();
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node: split(),
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      onRatioChange: vi.fn(),
    });
    const handle = screen.getByRole("separator", { name: "Resize split" });

    await fireEvent.pointerDown(handle, { clientX: 400, pointerId: 1, button: 0 });
    await fireEvent.pointerUp(handle, { clientX: 400, pointerId: 1 });
    expect(hasTerminalGeometryIntent()).toBe(false);

    await fireEvent.pointerDown(handle, { clientX: 400, pointerId: 2, button: 0 });
    await fireEvent.pointerMove(handle, { clientX: 424, pointerId: 2 });
    expect(hasTerminalGeometryIntent()).toBe(true);
    const pointerGeneration = currentTerminalGeometryIntent();
    await fireEvent.pointerMove(handle, { clientX: 448, pointerId: 2 });
    expect(currentTerminalGeometryIntent()).toBe(pointerGeneration);
    await fireEvent.pointerUp(handle, { clientX: 424, pointerId: 2 });

    vi.runOnlyPendingTimers();
    expect(hasTerminalGeometryIntent()).toBe(false);
    await fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(hasTerminalGeometryIntent()).toBe(true);
    expect(currentTerminalGeometryIntent()).not.toBe(pointerGeneration);
  });

  it("uses the vertical extent and clamps split ratios", async () => {
    mockRect();
    const onRatioChange = vi.fn();
    const node = split("vertical");
    if (node.type === "split") node.ratio = 0.87;
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node,
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      onRatioChange,
    });

    const handle = screen.getByRole("separator", { name: "Resize split" });
    expect(handle.getAttribute("aria-orientation")).toBe("horizontal");
    expect(handle.getAttribute("aria-valuenow")).toBe("522");
    await fireEvent.keyDown(handle, { key: "ArrowDown" });
    expect(onRatioChange).toHaveBeenCalledWith("split-1", 0.88);
  });

  it("updates only the targeted nested split", async () => {
    mockRect();
    const onRatioChange = vi.fn();
    const node: PaneNode = {
      type: "split",
      id: "outer",
      direction: "horizontal",
      ratio: 0.5,
      first: leaf("leaf-a", sessions[0]!.key),
      second: {
        type: "split",
        id: "inner",
        direction: "vertical",
        ratio: 0.4,
        first: leaf("leaf-b", sessions[1]!.key),
        second: leaf("leaf-c", sessions[2]!.key),
      },
    };
    renderTerminalSplitTree({
      workspaceId: "ws-1",
      node,
      sessions,
      displayLabels: {},
      activeSessionKey: sessions[0]!.key,
      onRatioChange,
    });

    const handles = screen.getAllByRole("separator", { name: "Resize split" });
    await fireEvent.keyDown(handles[1]!, { key: "ArrowDown" });
    expect(onRatioChange).toHaveBeenCalledTimes(1);
    expect(onRatioChange).toHaveBeenCalledWith("inner", 0.44);
  });
});
