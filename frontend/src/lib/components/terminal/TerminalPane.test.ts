import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { createSettingsStore, type SettingsStore } from "../../stores/settings.svelte.js";
import { beginTerminalGeometryIntent, extendTerminalGeometryIntent } from "./terminalGeometryIntent.js";
import TerminalPaneInputHarness from "./TerminalPaneInputHarness.svelte";

const {
  clipboardWriteText,
  clipboardWriterCancelAuthorization,
  clipboardWriterCancelPointerGesture,
  clipboardWriterConfirmPointerSelection,
  clipboardWriterDispose,
  clipboardWriterWrite,
  imageAddonCtor,
  imageAddons,
  ligaturesAddonCtor,
  mockItemResolvePost,
  mockNavigate,
  mockShowFlash,
  mockWebglCtor,
  mouseDragEndPointerGesture,
  mouseDragObserveTerminalData,
  mouseDragReset,
  resizeObserverCallbacks,
  webglAddons,
  webLinksAddonCtor,
  xtermFitAddons,
  xtermInstances,
  xtermCustomKeyEventHandlers,
  xtermOnDataHandlers,
  xtermOscHandlers,
  xtermTerminalCtor,
  xtermOpen,
} = vi.hoisted(() => ({
  clipboardWriteText: vi.fn(),
  clipboardWriterCancelAuthorization: vi.fn(),
  clipboardWriterCancelPointerGesture: vi.fn(),
  clipboardWriterConfirmPointerSelection: vi.fn(),
  clipboardWriterDispose: vi.fn(),
  clipboardWriterWrite: vi.fn(),
  imageAddonCtor: vi.fn(),
  imageAddons: [] as Array<{ dispose: ReturnType<typeof vi.fn> }>,
  ligaturesAddonCtor: vi.fn(),
  mockItemResolvePost: vi.fn(),
  mockNavigate: vi.fn(),
  mockShowFlash: vi.fn(),
  mockWebglCtor: vi.fn(),
  mouseDragEndPointerGesture: vi.fn(),
  mouseDragObserveTerminalData: vi.fn(),
  mouseDragReset: vi.fn(),
  resizeObserverCallbacks: [] as ResizeObserverCallback[],
  webglAddons: [] as Array<{ dispose: ReturnType<typeof vi.fn>; onContextLoss: ReturnType<typeof vi.fn> }>,
  webLinksAddonCtor: vi.fn(),
  xtermFitAddons: [] as Array<{ fit: ReturnType<typeof vi.fn>; proposeDimensions: ReturnType<typeof vi.fn> }>,
  xtermInstances: [] as Array<{
    buffer: { active: { baseY: number; type: "normal" | "alternate" } };
    clearTextureAtlas: ReturnType<typeof vi.fn>;
    cols: number;
    focus: ReturnType<typeof vi.fn>;
    modes: {
      applicationCursorKeysMode: boolean;
      bracketedPasteMode: boolean;
      mouseTrackingMode: "none" | "x10" | "vt200" | "drag" | "any";
    };
    refresh: ReturnType<typeof vi.fn>;
    rows: number;
    write: ReturnType<typeof vi.fn>;
  }>,
  xtermCustomKeyEventHandlers: [] as Array<(event: KeyboardEvent) => boolean>,
  xtermOnDataHandlers: [] as Array<(data: string) => void>,
  xtermOscHandlers: new Map<number, (data: string) => boolean | Promise<boolean>>(),
  xtermTerminalCtor: vi.fn(),
  xtermOpen: vi.fn(),
}));

let configuredFontFamily = "";
let configuredFontSize = 14;
let configuredScrollback = 1000;
let configuredLineHeight = 1;
let configuredLetterSpacing = 0;
let configuredCursorBlink = true;
let configuredFontLigatures = false;
let terminalSettingsStore: SettingsStore;
let configuredRepos: Array<{ provider: string; platform_host: string }> = [];
let mockSockets: MockWebSocket[] = [];
let mockSocketsStartOpen = true;
let initialTerminalDimensions = { cols: 80, rows: 24 };
// What the fit addon measures the region as. undefined models a container with
// no content box (a parked terminal), for which the real addon proposes nothing.
let fitDimensions: { cols: number; rows: number } | undefined = { cols: 80, rows: 24 };
const originalDocumentFonts = Object.getOwnPropertyDescriptor(document, "fonts");
const originalNavigatorClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
const originalIsSecureContext = Object.getOwnPropertyDescriptor(window, "isSecureContext");

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
} {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve;
    reject = promiseReject;
  });
  return { promise, resolve, reject };
}

function stubFontLoad(promise: Promise<FontFace[]>): ReturnType<typeof vi.fn> {
  void promise.catch(() => undefined);
  const load = vi.fn().mockReturnValue(promise);
  Object.defineProperty(document, "fonts", {
    configurable: true,
    value: {
      load,
      ready: new Promise<FontFaceSet>(() => undefined),
    },
  });
  return load;
}

class MockWebSocket extends EventTarget {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState: WebSocket["readyState"] = MockWebSocket.CONNECTING;
  binaryType = "arraybuffer";
  onopen = () => this.dispatchEvent(new Event("open"));
  onmessage = (event: MessageEvent) => this.dispatchEvent(event);
  onclose = () => this.dispatchEvent(new CloseEvent("close", { code: 1006 }));
  onerror = () => this.dispatchEvent(new Event("error"));
  sent: Array<string | ArrayBuffer | ArrayBufferView> = [];

  constructor(public url: string) {
    super();
    mockSockets.push(this);
    if (mockSocketsStartOpen) {
      queueMicrotask(() => {
        this.readyState = MockWebSocket.OPEN;
        this.onopen();
      });
    }
  }
  send(data: string | ArrayBuffer | ArrayBufferView): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = MockWebSocket.CLOSED;
  }
}

vi.mock("../../context.js", () => ({
  getStores: () => ({
    settings: {
      getTerminalFontFamily: () => configuredFontFamily,
      getTerminalFontSize: () => configuredFontSize,
      getTerminalScrollback: () => configuredScrollback,
      getTerminalLineHeight: () => configuredLineHeight,
      getTerminalLetterSpacing: () => configuredLetterSpacing,
      getTerminalCursorBlink: () => configuredCursorBlink,
      getTerminalFontLigatures: () => configuredFontLigatures,
      getTerminalGraphics: () => terminalSettingsStore.getTerminalGraphics(),
      getConfiguredRepos: () => configuredRepos,
    },
  }),
}));

vi.mock("../../app/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.js")>();
  const { makeGeneratedClient } = await import("../../testing/generated-client.js");
  return {
    ...actual,
    makeAppRuntime: () =>
      actual.makeAppRuntime(makeGeneratedClient({ RepositoriesService: { resolveRepoItem: mockItemResolvePost } })),
  };
});

vi.mock("../../stores/router.svelte.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../stores/router.svelte.js")>();
  return {
    ...actual,
    navigate: mockNavigate,
  };
});

vi.mock("../../stores/flash.svelte.js", () => ({
  showFlash: mockShowFlash,
}));

vi.mock("./terminalClipboardWriter.js", async () => {
  const { Effect } = await import("effect");
  return {
    createBrowserTerminalClipboardPort: vi.fn(() => ({})),
    makeTerminalClipboardWriter: vi.fn(() =>
      Effect.succeed({
        beginPointerGesture: vi.fn(),
        cancelAuthorization: clipboardWriterCancelAuthorization,
        cancelPointerGesture: clipboardWriterCancelPointerGesture,
        confirmPointerSelection: clipboardWriterConfirmPointerSelection,
        endPointerGesture: vi.fn(),
        authorizeKeyboardGesture: vi.fn(),
        write: clipboardWriterWrite,
        dispose: clipboardWriterDispose,
      }),
    ),
  };
});

vi.mock("./tmuxMouseDragAutoscroll.js", async () => {
  const { Effect } = await import("effect");
  return {
    makeTmuxMouseDragAutoscroll: vi.fn(() =>
      Effect.succeed({
        observeTerminalData: mouseDragObserveTerminalData,
        updatePointer: vi.fn(),
        endPointerGesture: mouseDragEndPointerGesture,
        reset: mouseDragReset,
        dispose: vi.fn(),
      }),
    ),
  };
});

vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn().mockImplementation(function (options) {
    xtermTerminalCtor(options);
    // The real xterm Terminal is a class instance, which Svelte leaves opaque.
    // Keep the double equally opaque so fit updates the same object the pane reads.
    class MockTerminal {}
    const bufferType: "normal" = "normal";
    const mouseTrackingMode: "none" = "none";
    const terminal = Object.assign(new MockTerminal(), {
      buffer: { active: { baseY: 0, type: bufferType } },
      cols: initialTerminalDimensions.cols,
      rows: initialTerminalDimensions.rows,
      modes: {
        applicationCursorKeysMode: false,
        bracketedPasteMode: false,
        mouseTrackingMode,
      },
      options: { ...options },
      clearTextureAtlas: vi.fn(),
      attachCustomKeyEventHandler: vi.fn((handler: (event: KeyboardEvent) => boolean) => {
        xtermCustomKeyEventHandlers.push(handler);
      }),
      dispose: vi.fn(),
      focus: vi.fn(),
      loadAddon: vi.fn(),
      onBinary: vi.fn(),
      onData: vi.fn((handler: (data: string) => void) => {
        xtermOnDataHandlers.push(handler);
        return { dispose: vi.fn() };
      }),
      open: xtermOpen,
      parser: {
        registerOscHandler: vi.fn((identifier: number, handler: (data: string) => boolean | Promise<boolean>) => {
          xtermOscHandlers.set(identifier, handler);
          return { dispose: vi.fn() };
        }),
      },
      refresh: vi.fn(),
      write: vi.fn(),
    });
    xtermInstances.push(terminal);
    return terminal;
  }),
}));

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(function () {
    // proposeDimensions is the pane's measurement of its own region: the real
    // addon returns undefined, or a zero, for a container with no content box
    // (a parked terminal), and the pane only pushes a size when it gets one.
    const terminal = xtermInstances.at(-1);
    const addon = {
      fit: vi.fn(() => {
        const fitted = addon.proposeDimensions();
        if (!terminal || !fitted) return;
        terminal.cols = fitted.cols;
        terminal.rows = fitted.rows;
      }),
      proposeDimensions: vi.fn(() => fitDimensions),
    };
    xtermFitAddons.push(addon);
    return addon;
  }),
}));

vi.mock("@xterm/addon-image", () => ({
  ImageAddon: vi.fn().mockImplementation(function (options) {
    imageAddonCtor(options);
    const addon = { dispose: vi.fn() };
    imageAddons.push(addon);
    return addon;
  }),
}));

vi.mock("@xterm/addon-ligatures/lib/addon-ligatures.mjs", () => ({
  LigaturesAddon: vi.fn().mockImplementation(function () {
    ligaturesAddonCtor();
    return { dispose: vi.fn() };
  }),
}));

vi.mock("@xterm/addon-web-links", () => ({
  WebLinksAddon: vi.fn().mockImplementation(function (handler, options) {
    webLinksAddonCtor(handler, options);
    return { dispose: vi.fn() };
  }),
}));

vi.mock("@xterm/addon-webgl", () => ({
  WebglAddon: vi.fn().mockImplementation(function (options) {
    mockWebglCtor(options);
    const addon = {
      dispose: vi.fn(),
      onContextLoss: vi.fn(),
    };
    webglAddons.push(addon);
    return addon;
  }),
}));

vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

import TerminalPane from "./TerminalPaneTestHarness.svelte";

function resizeFramesOf(socket: MockWebSocket): string[] {
  return socket.sent.map(String).filter((frame) => frame.includes('"type":"resize"'));
}

async function waitForSocketConnected(socket: MockWebSocket): Promise<void> {
  await waitFor(() => expect(socket.readyState).toBe(MockWebSocket.OPEN));
}

async function waitForInitialGeometry(socket: MockWebSocket): Promise<void> {
  await waitFor(() => expect(socketFramesOfType(socket, "refresh").length).toBeGreaterThan(0));
}

describe("TerminalPane", () => {
  beforeEach(() => {
    configuredFontFamily = "";
    configuredFontSize = 14;
    configuredScrollback = 1000;
    configuredLineHeight = 1;
    configuredLetterSpacing = 0;
    configuredCursorBlink = true;
    configuredFontLigatures = false;
    terminalSettingsStore = createSettingsStore();
    configuredRepos = [];
    mockItemResolvePost.mockReset();
    mockNavigate.mockReset();
    initialTerminalDimensions = { cols: 80, rows: 24 };
    fitDimensions = { cols: 80, rows: 24 };
    ligaturesAddonCtor.mockReset();
    imageAddonCtor.mockReset();
    imageAddons.length = 0;
    clipboardWriteText.mockReset();
    clipboardWriterCancelAuthorization.mockReset();
    clipboardWriterCancelPointerGesture.mockReset();
    clipboardWriterConfirmPointerSelection.mockReset();
    clipboardWriterDispose.mockReset();
    clipboardWriterWrite.mockReset().mockResolvedValue("unauthorized");
    mockShowFlash.mockReset();
    mockWebglCtor.mockReset();
    mouseDragEndPointerGesture.mockReset();
    mouseDragObserveTerminalData.mockReset();
    mouseDragReset.mockReset();
    resizeObserverCallbacks.length = 0;
    webglAddons.length = 0;
    webLinksAddonCtor.mockReset();
    xtermFitAddons.length = 0;
    xtermInstances.length = 0;
    xtermCustomKeyEventHandlers.length = 0;
    xtermTerminalCtor.mockReset();
    xtermOpen.mockReset();
    xtermOnDataHandlers.length = 0;
    xtermOscHandlers.clear();
    mockSockets = [];
    mockSocketsStartOpen = true;

    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: clipboardWriteText },
    });

    vi.stubGlobal(
      "ResizeObserver",
      class {
        constructor(callback: ResizeObserverCallback) {
          resizeObserverCallbacks.push(callback);
        }
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

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    if (originalDocumentFonts) {
      Object.defineProperty(document, "fonts", originalDocumentFonts);
    } else {
      Reflect.deleteProperty(document, "fonts");
    }
    if (originalNavigatorClipboard) {
      Object.defineProperty(navigator, "clipboard", originalNavigatorClipboard);
    } else {
      Reflect.deleteProperty(navigator, "clipboard");
    }
    if (originalIsSecureContext) {
      Object.defineProperty(window, "isSecureContext", originalIsSecureContext);
    } else {
      Reflect.deleteProperty(window, "isSecureContext");
    }
  });

  it("uses xterm.js", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
  });

  it("loads SIXEL and iTerm images without enabling Kitty graphics", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => {
      expect(imageAddonCtor).toHaveBeenCalledWith({
        iipSupport: true,
        kittySupport: false,
        sixelSupport: true,
      });
    });
  });

  it("does not load the image decoder when terminal graphics are disabled", async () => {
    terminalSettingsStore.setTerminalSettings({
      ...terminalSettingsStore.getTerminalSettings(),
      graphics: false,
    });

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    expect(imageAddonCtor).not.toHaveBeenCalled();
  });

  it("reloads the image decoder when terminal graphics change", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(imageAddons).toHaveLength(1));
    const initialAddon = imageAddons[0]!;

    terminalSettingsStore.setTerminalSettings({
      ...terminalSettingsStore.getTerminalSettings(),
      graphics: false,
    });
    await waitFor(() => expect(initialAddon.dispose).toHaveBeenCalledTimes(1));

    terminalSettingsStore.setTerminalSettings({
      ...terminalSettingsStore.getTerminalSettings(),
      graphics: true,
    });
    await waitFor(() => expect(imageAddons).toHaveLength(2));
    expect(imageAddonCtor).toHaveBeenCalledTimes(2);
  });

  it("leaves Ctrl+V browser-owned on Windows and Linux", async () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("Win32");
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermCustomKeyEventHandlers).toHaveLength(1));
    const handler = xtermCustomKeyEventHandlers[0]!;

    expect(handler(new KeyboardEvent("keydown", { key: "v", ctrlKey: true }))).toBe(false);
    expect(handler(new KeyboardEvent("keydown", { key: "V", ctrlKey: true, shiftKey: true }))).toBe(false);
    expect(handler(new KeyboardEvent("keydown", { key: "v", metaKey: true }))).toBe(true);
    expect(handler(new KeyboardEvent("keydown", { key: "c", ctrlKey: true }))).toBe(true);
  });

  it("leaves Cmd+V browser-owned on macOS without consuming Ctrl+V", async () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("MacIntel");
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermCustomKeyEventHandlers).toHaveLength(1));
    const handler = xtermCustomKeyEventHandlers[0]!;

    expect(handler(new KeyboardEvent("keydown", { key: "v", metaKey: true }))).toBe(false);
    expect(handler(new KeyboardEvent("keydown", { key: "V", metaKey: true, shiftKey: true }))).toBe(false);
    expect(handler(new KeyboardEvent("keydown", { key: "v", ctrlKey: true }))).toBe(true);
    expect(handler(new KeyboardEvent("keydown", { key: "V", ctrlKey: true, shiftKey: true }))).toBe(true);
  });

  it("keeps insecure-origin right clicks out of tmux without blocking Chrome's context menu", async () => {
    Object.defineProperty(window, "isSecureContext", { configurable: true, value: false });
    const { container } = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const terminalContainer = container.querySelector<HTMLElement>(".terminal-container")!;
    const xtermChild = document.createElement("div");
    terminalContainer.append(xtermChild);
    const mouseDown = vi.fn();
    const mouseUp = vi.fn();
    xtermChild.addEventListener("mousedown", mouseDown);
    xtermChild.addEventListener("mouseup", mouseUp);

    const downAllowed = xtermChild.dispatchEvent(
      new MouseEvent("mousedown", { bubbles: true, cancelable: true, button: 2 }),
    );
    const upAllowed = xtermChild.dispatchEvent(
      new MouseEvent("mouseup", { bubbles: true, cancelable: true, button: 2 }),
    );

    expect(mouseDown).not.toHaveBeenCalled();
    expect(mouseUp).not.toHaveBeenCalled();
    expect(downAllowed).toBe(true);
    expect(upAllowed).toBe(true);
  });

  it("uses the same safe opener for detected URLs and OSC 8 links", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());

    const linkHandler = xtermTerminalCtor.mock.calls[0]![0].linkHandler;

    expect(webLinksAddonCtor.mock.calls[0]![0]).toBe(linkHandler.activate);
    expect(webLinksAddonCtor.mock.calls[0]![1]).toEqual({
      hover: linkHandler.hover,
      leave: linkHandler.leave,
    });
  });

  it("discloses a hovered link target and its activation modifier", async () => {
    const view = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const linkHandler = xtermTerminalCtor.mock.calls[0]![0].linkHandler;

    linkHandler.hover(new MouseEvent("mouseover"), "https://example.com/hidden");
    await tick();

    expect(view.getByText("https://example.com/hidden")).toBeTruthy();
    expect(view.getByText(`${/Mac/.test(navigator.platform) ? "Cmd" : "Ctrl"}+Click to open link`)).toBeTruthy();
  });

  it("opens only modified HTTP links in a new isolated tab", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const activate = xtermTerminalCtor.mock.calls[0]![0].linkHandler.activate;
    const modifier = /Mac/.test(navigator.platform) ? { metaKey: true } : { ctrlKey: true };

    activate(new MouseEvent("click"), "https://example.com/no-modifier");
    activate(new MouseEvent("click", modifier), "javascript:alert(document.domain)");
    activate(new MouseEvent("click", modifier), "https://example.com/docs");

    expect(open).toHaveBeenCalledTimes(1);
    expect(open).toHaveBeenCalledWith("https://example.com/docs", "_blank", "noopener,noreferrer");
  });

  it("routes tracked repository item links through the app instead of a new tab", async () => {
    configuredRepos = [{ provider: "github", platform_host: "github.com" }];
    mockItemResolvePost.mockResolvedValue({ repo_tracked: true, item_type: "pr" });
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const activate = xtermTerminalCtor.mock.calls[0]![0].linkHandler.activate;
    const modifier = /Mac/.test(navigator.platform) ? { metaKey: true } : { ctrlKey: true };

    activate(new MouseEvent("click", modifier), "https://github.com/acme/widgets/pull/1028");

    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith("/pulls/github/acme/widgets/1028"));
    expect(mockItemResolvePost).toHaveBeenCalledWith(
      { provider: "github", owner: "acme", name: "widgets", number: 1028 },
      undefined,
      { signal: expect.any(AbortSignal) },
    );
    expect(open).not.toHaveBeenCalled();
  });

  it("opens item links for untracked repositories externally after resolving", async () => {
    configuredRepos = [{ provider: "github", platform_host: "github.com" }];
    mockItemResolvePost.mockResolvedValue({ repo_tracked: false });
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const activate = xtermTerminalCtor.mock.calls[0]![0].linkHandler.activate;
    const modifier = /Mac/.test(navigator.platform) ? { metaKey: true } : { ctrlKey: true };

    activate(new MouseEvent("click", modifier), "https://github.com/other/repo/issues/7");

    await waitFor(() =>
      expect(open).toHaveBeenCalledWith("https://github.com/other/repo/issues/7", "_blank", "noopener,noreferrer"),
    );
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("keeps opening links on unconfigured hosts externally without resolving", async () => {
    configuredRepos = [{ provider: "gitlab", platform_host: "gitlab.example.com" }];
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const activate = xtermTerminalCtor.mock.calls[0]![0].linkHandler.activate;
    const modifier = /Mac/.test(navigator.platform) ? { metaKey: true } : { ctrlKey: true };

    activate(new MouseEvent("click", modifier), "https://github.com/acme/widgets/pull/1028");

    expect(open).toHaveBeenCalledWith("https://github.com/acme/widgets/pull/1028", "_blank", "noopener,noreferrer");
    expect(mockItemResolvePost).not.toHaveBeenCalled();
  });

  it("tells the user a hovered item link opens inside the app", async () => {
    configuredRepos = [{ provider: "github", platform_host: "github.com" }];
    const view = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const linkHandler = xtermTerminalCtor.mock.calls[0]![0].linkHandler;
    const modifier = /Mac/.test(navigator.platform) ? "Cmd" : "Ctrl";

    linkHandler.hover(new MouseEvent("mouseover"), "https://github.com/acme/widgets/issues/3");
    await tick();
    expect(view.getByText(`${modifier}+Click to open in kenn-forge`)).toBeTruthy();

    linkHandler.hover(new MouseEvent("mouseover"), "https://github.com/acme/widgets/commit/abc");
    await tick();
    expect(view.getByText(`${modifier}+Click to open link`)).toBeTruthy();
  });

  it("forwards accepted tmux OSC 52 text to the authorized clipboard writer", async () => {
    clipboardWriterWrite.mockResolvedValue("written");
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);

    const handled = await xtermOscHandlers.get(52)!("c;Y29waWVkIHRleHQ=");

    expect(handled).toBe(true);
    await waitFor(() => expect(clipboardWriterWrite).toHaveBeenCalledWith("copied text"));
  });

  it("consumes OSC 52 writes synchronously while the clipboard write is pending", async () => {
    const clipboardWrite = deferred<"written">();
    clipboardWriterWrite.mockReturnValue(clipboardWrite.promise);
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);

    const handled = xtermOscHandlers.get(52)!("c;Y29waWVkIHRleHQ=");

    expect(handled).toBe(true);
    await waitFor(() => expect(clipboardWriterWrite).toHaveBeenCalledWith("copied text"));
    clipboardWrite.resolve("written");
    await clipboardWrite.promise;
  });

  it("consumes OSC 52 reads without exposing the browser clipboard", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));

    const handled = await xtermOscHandlers.get(52)!("c;?");

    expect(handled).toBe(true);
    expect(clipboardWriterWrite).not.toHaveBeenCalled();
  });

  it("reports blocked terminal clipboard writes once per pane", async () => {
    clipboardWriterWrite.mockResolvedValue("blocked");
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    const handler = xtermOscHandlers.get(52)!;

    await handler("c;b25l");
    await handler("c;dHdv");

    await waitFor(() => expect(mockShowFlash).toHaveBeenCalledTimes(1));
    expect(mockShowFlash).toHaveBeenCalledWith("Could not write the terminal selection to the clipboard.", {
      tone: "danger",
    });
  });

  it("does not report a pending clipboard failure after pane disposal", async () => {
    const clipboardWrite = deferred<"blocked">();
    clipboardWriterWrite.mockReturnValue(clipboardWrite.promise);
    const view = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));

    xtermOscHandlers.get(52)!("c;Y29waWVkIHRleHQ=");
    view.unmount();
    clipboardWrite.resolve("blocked");
    await clipboardWrite.promise;

    expect(mockShowFlash).not.toHaveBeenCalled();
  });

  it("does not write OSC 52 text from a disconnected pane", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mockSockets[0]!.onclose();
    await waitFor(() => expect(mouseDragReset).toHaveBeenCalled());

    const handled = xtermOscHandlers.get(52)!("c;Y29waWVkIHRleHQ=");

    expect(handled).toBe(true);
    expect(clipboardWriterWrite).not.toHaveBeenCalled();
  });

  it("does not write OSC 52 text through a retained handler after unmount", async () => {
    const view = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));
    const handler = xtermOscHandlers.get(52)!;
    view.unmount();

    const handled = handler("c;Y29waWVkIHRleHQ=");

    expect(handled).toBe(true);
    expect(clipboardWriterWrite).not.toHaveBeenCalled();
  });

  it("does not write OSC 52 text from a disabled pane", async () => {
    render(TerminalPane, {
      props: { workspaceId: "ws-123", disabled: true },
    });
    await waitFor(() => expect(xtermOscHandlers.has(52)).toBe(true));

    await xtermOscHandlers.get(52)!("c;Y29waWVkIHRleHQ=");

    expect(clipboardWriterWrite).not.toHaveBeenCalled();
  });

  it("matches VS Code's stable xterm rendering defaults", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());

    expect(xtermTerminalCtor).toHaveBeenCalledWith(
      expect.objectContaining({
        allowProposedApi: true,
        allowTransparency: false,
        cursorBlink: true,
        fontSize: 14,
        scrollback: 1000,
        letterSpacing: 0,
        lineHeight: 1,
        minimumContrastRatio: 4.5,
        rescaleOverlappingGlyphs: true,
        scrollOnEraseInDisplay: true,
        smoothScrollDuration: 0,
        vtExtensions: {
          kittyKeyboard: true,
        },
      }),
    );
    expect(mockWebglCtor).toHaveBeenCalledWith({ customGlyphs: true });
  });

  it("keeps xterm's built-in renderer on Android", async () => {
    vi.spyOn(navigator, "userAgent", "get").mockReturnValue(
      "Mozilla/5.0 (Linux; Android 16) AppleWebKit/537.36 Chrome/133.0 Mobile Safari/537.36",
    );

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    expect(mockWebglCtor).not.toHaveBeenCalled();
  });

  it("releases WebGL while retained and recreates it when claimed", async () => {
    const { rerender, unmount } = render(TerminalPane, {
      props: { workspaceId: "ws-123", renderingEnabled: true },
    });
    await waitFor(() => expect(webglAddons).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const initialWebgl = webglAddons[0]!;
    const closeSocket = vi.spyOn(mockSockets[0]!, "close");

    await rerender({ workspaceId: "ws-123", renderingEnabled: false });

    expect(initialWebgl.dispose).toHaveBeenCalledTimes(1);
    expect(terminal.dispose).not.toHaveBeenCalled();

    await rerender({ workspaceId: "ws-123", renderingEnabled: true });
    await waitFor(() => expect(webglAddons).toHaveLength(2));
    expect(xtermInstances).toHaveLength(1);

    const replacementWebgl = webglAddons[1]!;
    unmount();

    expect(replacementWebgl.dispose).toHaveBeenCalledTimes(1);
    expect(terminal.dispose).toHaveBeenCalledTimes(1);
    expect(closeSocket).toHaveBeenCalledTimes(1);
  });

  it("reports socket connection changes", async () => {
    const onConnectionChange = vi.fn();
    render(TerminalPane, { props: { workspaceId: "ws-123", onConnectionChange } });
    await waitFor(() => expect(onConnectionChange).toHaveBeenCalledWith(true));

    mockSockets[0]!.onclose();

    await waitFor(() => expect(onConnectionChange).toHaveBeenCalledWith(false));
  });

  it("uses configured terminal metrics for xterm.js", async () => {
    configuredFontSize = 17;
    configuredScrollback = 5000;
    configuredLineHeight = 1.2;
    configuredLetterSpacing = 1;
    configuredCursorBlink = false;

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());

    expect(xtermTerminalCtor).toHaveBeenCalledWith(
      expect.objectContaining({
        cursorBlink: false,
        fontSize: 17,
        scrollback: 5000,
        lineHeight: 1.2,
        letterSpacing: 1,
      }),
    );
  });

  it("constructs xterm when the selected font resolves", async () => {
    configuredFontFamily = '"MesloLGS NF", monospace';
    const fontLoad = deferred<FontFace[]>();
    const load = stubFontLoad(fontLoad.promise);

    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(load).toHaveBeenCalled());

    expect(load).toHaveBeenCalledWith('14px "MesloLGS NF"', "0MWim@#");
    expect(xtermTerminalCtor).not.toHaveBeenCalled();

    fontLoad.resolve([]);
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalledTimes(1));
  });

  it("constructs xterm after the selected-font wait reaches 300 ms", async () => {
    vi.useFakeTimers();
    const fontLoad = deferred<FontFace[]>();
    const load = stubFontLoad(fontLoad.promise);

    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await vi.waitFor(() => expect(load).toHaveBeenCalled());
    await vi.advanceTimersByTimeAsync(300);

    expect(xtermTerminalCtor).toHaveBeenCalledTimes(1);
  });

  it("constructs xterm when the selected font descriptor is rejected synchronously", async () => {
    Object.defineProperty(document, "fonts", {
      configurable: true,
      value: {
        load: vi.fn(() => {
          throw new DOMException("Invalid font shorthand", "SyntaxError");
        }),
        ready: new Promise<FontFaceSet>(() => undefined),
      },
    });

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalledTimes(1));
  });

  it("constructs xterm when the selected font load rejects asynchronously", async () => {
    stubFontLoad(Promise.reject(new DOMException("Font load failed", "NetworkError")));

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalledTimes(1));
  });

  it("rebuilds the xterm atlas once when the selected font resolves after the bound", async () => {
    vi.useFakeTimers();
    mockSocketsStartOpen = false;
    const fontLoad = deferred<FontFace[]>();
    const load = stubFontLoad(fontLoad.promise);

    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await vi.waitFor(() => expect(load).toHaveBeenCalled());
    await vi.advanceTimersByTimeAsync(300);
    vi.useRealTimers();
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mockSockets[0]!.readyState = MockWebSocket.OPEN;
    mockSockets[0]!.onopen();
    await waitForSocketConnected(mockSockets[0]!);

    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    terminal.clearTextureAtlas.mockClear();
    terminal.refresh.mockClear();
    fitAddon.fit.mockClear();
    mockSockets[0]!.sent = [];

    fontLoad.resolve([]);

    await waitFor(() => expect(terminal.clearTextureAtlas).toHaveBeenCalledTimes(1));
    expect(fitAddon.fit).toHaveBeenCalledTimes(1);
    expect(terminal.refresh).toHaveBeenCalledTimes(1);
  });

  it("pushes the re-measured size when a font resolves late in a painted pane", async () => {
    vi.useFakeTimers();
    mockSocketsStartOpen = false;
    const fontLoad = deferred<FontFace[]>();
    const load = stubFontLoad(fontLoad.promise);

    render(TerminalPane, { props: { workspaceId: "ws-123", active: true } });
    await vi.waitFor(() => expect(load).toHaveBeenCalled());
    await vi.advanceTimersByTimeAsync(300);
    vi.useRealTimers();
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mockSockets[0]!.readyState = MockWebSocket.OPEN;
    mockSockets[0]!.onopen();
    await waitForSocketConnected(mockSockets[0]!);

    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    terminal.clearTextureAtlas.mockClear();
    fitAddon.fit.mockClear();
    mockSockets[0]!.sent = [];
    // Different metrics, so the region works out to a different size.
    fitDimensions = { cols: 70, rows: 20 };

    fontLoad.resolve([]);

    await waitFor(() => expect(terminal.clearTextureAtlas).toHaveBeenCalledTimes(1));
    expect(fitAddon.fit).toHaveBeenCalledTimes(1);
    await waitFor(() =>
      expect(resizeFramesOf(mockSockets[0]!)).toEqual([JSON.stringify({ type: "resize", cols: 70, rows: 20 })]),
    );
  });

  it("does not rebuild a disposed xterm when the selected font resolves late", async () => {
    vi.useFakeTimers();
    const fontLoad = deferred<FontFace[]>();
    stubFontLoad(fontLoad.promise);

    const { unmount } = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await tick();
    await vi.advanceTimersByTimeAsync(300);

    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    terminal.clearTextureAtlas.mockClear();
    terminal.refresh.mockClear();
    fitAddon.fit.mockClear();
    unmount();

    fontLoad.resolve([]);
    await vi.advanceTimersByTimeAsync(0);

    expect(terminal.clearTextureAtlas).not.toHaveBeenCalled();
    expect(fitAddon.fit).not.toHaveBeenCalled();
    expect(terminal.refresh).not.toHaveBeenCalled();
  });

  it("releases terminal gesture resources when detached before the selected font is ready", async () => {
    const fontLoad = deferred<FontFace[]>();
    stubFontLoad(fontLoad.promise);

    const { unmount } = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await tick();
    unmount();

    await waitFor(() => expect(clipboardWriterDispose).toHaveBeenCalledTimes(1));
  });

  it("loads the ligatures addon for xterm.js when enabled", async () => {
    configuredFontLigatures = true;

    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());

    expect(ligaturesAddonCtor).toHaveBeenCalledTimes(1);
  });

  it("does not rebuild the WebGL atlas during initial mount refresh", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermInstances).toHaveLength(1));

    expect(xtermInstances[0]!.clearTextureAtlas).not.toHaveBeenCalled();
  });

  it("claims resize authority for a painted measurable region", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123", active: true } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    expect(mockSockets[0]!.url).toContain("resize_active=1");

    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];
    fitDimensions = { cols: 100, rows: 40 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() =>
      expect(resizeFramesOf(mockSockets[0]!)).toEqual([JSON.stringify({ type: "resize", cols: 100, rows: 40 })]),
    );
  });

  it("does not claim resize authority for a hidden but measurable region", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123", active: false } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    expect(mockSockets[0]!.url).toContain("resize_active=0");

    await waitForSocketConnected(mockSockets[0]!);

    mockSockets[0]!.sent = [];
    fitDimensions = { cols: 100, rows: 40 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    expect(mockSockets[0]!.sent).toHaveLength(0);
  });

  it("revokes authority and ignores later measurements when its tab becomes hidden", async () => {
    const { rerender } = render(TerminalPane, {
      props: { workspaceId: "ws-123", active: true },
    });
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];

    await rerender({ workspaceId: "ws-123", active: false });
    await waitFor(() =>
      expect(mockSockets[0]!.sent).toContain(JSON.stringify({ type: "resize_active", active: false })),
    );

    mockSockets[0]!.sent = [];
    fitDimensions = { cols: 100, rows: 40 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    expect(mockSockets[0]!.sent).toHaveLength(0);
  });

  it("neither claims authority nor pushes a size for an unmeasurable region", async () => {
    // A parked terminal sits in a display:none node: the fit addon proposes
    // nothing for it, and measuring it anyway is what used to resize a live
    // tmux pane to one row.
    fitDimensions = undefined;
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    expect(mockSockets[0]!.url).toContain("resize_active=0");

    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    expect(resizeFramesOf(mockSockets[0]!)).toHaveLength(0);
  });

  it("claims authority before resizing when an active region gains geometry", async () => {
    fitDimensions = undefined;
    const { container } = render(TerminalPane, { props: { workspaceId: "ws-123", active: true } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    expect(mockSockets[0]!.url).toContain("resize_active=0");
    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];

    const screen = document.createElement("div");
    screen.className = "xterm-screen";
    vi.spyOn(screen, "getBoundingClientRect").mockReturnValue(DOMRect.fromRect({ width: 875, height: 740 }));
    container.querySelector(".terminal-container")?.append(screen);

    fitDimensions = { cols: 100, rows: 40 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() =>
      expect(mockSockets[0]!.sent.map(String)).toEqual([
        JSON.stringify({
          type: "claim_resize",
          cols: 100,
          rows: 40,
          pixel_width: 875,
          pixel_height: 740,
        }),
      ]),
    );
  });

  it("revokes and reclaims authority as active region geometry changes", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123", active: true } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];

    fitDimensions = undefined;
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    await waitFor(() =>
      expect(mockSockets[0]!.sent.map(String)).toContain(JSON.stringify({ type: "resize_active", active: false })),
    );

    fitDimensions = { cols: 80, rows: 24 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() =>
      expect(mockSockets[0]!.sent.map(String)).toEqual([
        JSON.stringify({ type: "resize_active", active: false }),
        JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }),
      ]),
    );
  });

  it("sends nothing more for a burst that measures the same size", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    await waitForInitialGeometry(mockSockets[0]!);

    mockSockets[0]!.sent = [];
    fitDimensions = { cols: 120, rows: 50 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() =>
      expect(resizeFramesOf(mockSockets[0]!)).toEqual([JSON.stringify({ type: "resize", cols: 120, rows: 50 })]),
    );
  });

  it("reports the dimensions that fit actually applies when the region changes between measurements", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    await waitForSocketConnected(mockSockets[0]!);
    mockSockets[0]!.sent = [];
    fitDimensions = undefined;
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    fitAddon.proposeDimensions.mockReturnValueOnce({ cols: 80, rows: 24 }).mockReturnValue({ cols: 80, rows: 25 });

    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() => expect(terminal.rows).toBe(25));
    await waitFor(() =>
      expect(socketFramesOfType(mockSockets[0]!, "claim_resize")).toEqual([
        JSON.stringify({ type: "claim_resize", cols: 80, rows: 25 }),
      ]),
    );
  });

  it("sends a size measured before socket open once the connection opens", async () => {
    // The first measurement lands before the socket opens. Recording it as sent
    // anyway would let the dedupe suppress it forever, leaving the PTY at the
    // size it launched with.
    mockSocketsStartOpen = false;
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    fitDimensions = { cols: 90, rows: 30 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    expect(resizeFramesOf(mockSockets[0]!)).toHaveLength(0);

    mockSockets[0]!.readyState = MockWebSocket.OPEN;
    mockSockets[0]!.onopen();

    await waitFor(() =>
      expect(mockSockets[0]!.sent).toContain(JSON.stringify({ type: "refresh", cols: 90, rows: 30 })),
    );
  });

  it("claims resize authority when its tab becomes active while the socket connects", async () => {
    mockSocketsStartOpen = false;
    const { rerender } = render(TerminalPane, {
      props: { workspaceId: "ws-123", active: false },
    });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    expect(mockSockets[0]!.url).toContain("resize_active=0");

    await rerender({ workspaceId: "ws-123", active: true });
    mockSockets[0]!.readyState = MockWebSocket.OPEN;
    mockSockets[0]!.onopen();

    await waitFor(() =>
      expect(socketFramesOfType(mockSockets[0]!, "claim_resize")).toEqual([
        JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }),
      ]),
    );
  });

  it("focuses the xterm terminal once it initializes while active", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermInstances.length).toBe(1));
    expect(xtermInstances[0]!.focus).toHaveBeenCalled();
  });

  it("does not steal focus when an existing terminal becomes active", async () => {
    const { rerender } = render(TerminalPane, {
      props: { workspaceId: "ws-123", active: false },
    });

    await waitFor(() => expect(xtermInstances.length).toBe(1));
    expect(xtermInstances[0]!.focus).not.toHaveBeenCalled();

    await rerender({ workspaceId: "ws-123", active: true });
    await tick();
    expect(xtermInstances[0]!.focus).not.toHaveBeenCalled();
  });

  it("does not focus a disabled terminal", async () => {
    render(TerminalPane, {
      props: { workspaceId: "ws-123", active: true, disabled: true },
    });

    await waitFor(() => expect(xtermInstances.length).toBe(1));
    expect(xtermInstances[0]!.focus).not.toHaveBeenCalled();
  });

  it("focuses the terminal input when its active surface is touched", async () => {
    render(TerminalPane, {
      props: { workspaceId: "ws-123", active: true, autoFocus: false },
    });

    await waitFor(() => expect(xtermInstances.length).toBe(1));
    const terminal = xtermInstances[0]!;
    const container = document.querySelector<HTMLElement>(".terminal-container");
    expect(container).not.toBeNull();

    container!.dispatchEvent(
      new PointerEvent("pointerdown", {
        bubbles: true,
        button: 0,
        pointerType: "touch",
      }),
    );

    expect(terminal.focus).toHaveBeenCalledOnce();
  });

  it("suppresses the software keyboard when a manual-keyboard terminal surface is touched", async () => {
    render(TerminalPane, {
      props: { workspaceId: "ws-123", active: true, autoFocus: false },
    });

    await waitFor(() => expect(xtermInstances.length).toBe(1));
    const container = document.querySelector<HTMLElement>(".terminal-container");
    expect(container).not.toBeNull();
    container!.dataset.terminalSoftwareKeyboard = "manual";
    const input = document.createElement("textarea");
    input.className = "xterm-helper-textarea";
    container!.append(input);

    container!.dispatchEvent(
      new PointerEvent("pointerdown", {
        bubbles: true,
        button: 0,
        pointerType: "touch",
      }),
    );

    expect(input.inputMode).toBe("none");
  });

  it("repaints after container resize without rebuilding the WebGL atlas", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    terminal.clearTextureAtlas.mockClear();
    terminal.refresh.mockClear();
    fitAddon.fit.mockClear();
    mockSockets[0]!.sent = [];

    fitDimensions = { cols: 80, rows: 24 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);

    await waitFor(() => expect(fitAddon.fit).toHaveBeenCalled());
    expect(terminal.clearTextureAtlas).not.toHaveBeenCalled();
    expect(terminal.refresh).toHaveBeenCalledWith(0, 23);
    expect(socketFramesOfType(mockSockets[0]!, "claim_resize")).toHaveLength(0);
  });

  it("claims a changed fitted size during deliberate layout resizing", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    await waitForInitialGeometry(mockSockets[0]!);
    const socket = mockSockets[0]!;
    socket.sent = [];
    beginTerminalGeometryIntent();
    fitDimensions = { cols: 101, rows: 33 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    await waitFor(() => expect(socket.sent).toHaveLength(1));

    expect(socketFramesOfType(socket, "claim_resize")).toEqual([
      JSON.stringify({ type: "claim_resize", cols: 101, rows: 33 }),
    ]);
    expect(socketFramesOfType(socket, "resize")).toHaveLength(0);
    extendTerminalGeometryIntent();
    fitDimensions = { cols: 102, rows: 34 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    await waitFor(() => expect(socket.sent).toHaveLength(2));

    expect(socketFramesOfType(socket, "claim_resize")).toHaveLength(1);
    expect(socketFramesOfType(socket, "resize")).toEqual([JSON.stringify({ type: "resize", cols: 102, rows: 34 })]);
    await new Promise((resolve) => setTimeout(resolve, 250));
  });

  it("does not claim when deliberate layout movement crosses no terminal cell boundary", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(resizeObserverCallbacks).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitForSocketConnected(mockSockets[0]!);
    await waitForInitialGeometry(mockSockets[0]!);
    const socket = mockSockets[0]!;
    socket.sent = [];
    beginTerminalGeometryIntent();
    fitDimensions = { cols: 80, rows: 24 };
    resizeObserverCallbacks[0]!([], {} as ResizeObserver);
    await new Promise((resolve) => setTimeout(resolve, 250));

    expect(socketFramesOfType(socket, "claim_resize")).toHaveLength(0);
    expect(socketFramesOfType(socket, "resize")).toHaveLength(0);
  });

  it("forwards xterm protocol replies without claiming terminal size", async () => {
    fitDimensions = { cols: 101, rows: 33 };
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const socket = mockSockets[0]!;
    await waitForSocketConnected(socket);
    await waitForInitialGeometry(socket);
    socket.sent = [];

    xtermOnDataHandlers[0]!("\x1b[24;80R");

    await waitFor(() => expect(socket.sent).toHaveLength(1));
    expect(socketFramesOfType(socket, "claim_resize")).toHaveLength(0);
    expect(sentText(socket, 0)).toBe("\x1b[24;80R");
  });

  it("forwards complete tmux mouse drags without a local threshold", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const drag = "\x1b[<0;10;5M" + "\x1b[<32;12;5M" + "\x1b[<32;13;5M" + "\x1b[<0;13;5m";

    xtermOnDataHandlers[0]!(drag);

    const socket = mockSockets[0]!;
    await waitFor(() => expect(sentText(socket, socket.sent.length - 1)).toBe(drag));
  });

  it("forwards wheel input to an agent TUI that owns no terminal scrollback", async () => {
    const { container } = render(TerminalPane, {
      props: { workspaceId: "ws-123", cursorWheelInput: true },
    });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitFor(() => expect(xtermInstances).toHaveLength(1));
    const socket = mockSockets[0]!;
    socket.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).not.toBeNull();

    const defaultAllowed = terminalContainer!.dispatchEvent(
      new WheelEvent("wheel", { bubbles: true, cancelable: true, deltaY: -120 }),
    );

    expect(defaultAllowed).toBe(false);
    await waitFor(() => expect(socket.sent).toHaveLength(2));
    expect(socket.sent.map((_, index) => sentText(socket, index))).toEqual([
      JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }),
      "\x1b[A",
    ]);
  });

  it("claims terminal size before xterm handles mouse-tracking wheel input", async () => {
    const { container } = render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    await waitFor(() => expect(xtermInstances).toHaveLength(1));
    const socket = mockSockets[0]!;
    const terminal = xtermInstances[0]!;
    terminal.modes.mouseTrackingMode = "any";
    socket.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).not.toBeNull();

    const defaultAllowed = terminalContainer!.dispatchEvent(
      new WheelEvent("wheel", { bubbles: true, cancelable: true, deltaY: -120 }),
    );

    expect(defaultAllowed).toBe(true);
    await waitFor(() => expect(socket.sent).toHaveLength(1));
    expect(sentText(socket, 0)).toBe(JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }));
  });

  it("leaves Ctrl-wheel gestures with the browser", async () => {
    const { container } = render(TerminalPane, {
      props: { workspaceId: "ws-123", cursorWheelInput: true },
    });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const socket = mockSockets[0]!;
    socket.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).not.toBeNull();

    const defaultAllowed = terminalContainer!.dispatchEvent(
      new WheelEvent("wheel", {
        bubbles: true,
        cancelable: true,
        ctrlKey: true,
        deltaY: -120,
      }),
    );

    expect(defaultAllowed).toBe(true);
    expect(socket.sent).toHaveLength(0);
  });

  it("does not replay input received while disconnected", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const socket = mockSockets[0]!;
    mouseDragReset.mockClear();
    socket.onclose();
    await waitFor(() => expect(mouseDragReset).toHaveBeenCalled());
    socket.sent = [];
    mouseDragObserveTerminalData.mockClear();

    xtermOnDataHandlers[0]!("\x1b[<0;10;5M");
    expect(mouseDragObserveTerminalData).not.toHaveBeenCalled();
    expect(socket.sent).toHaveLength(0);
  });

  it("resets tmux drag state when the terminal socket closes", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mouseDragReset.mockClear();

    mockSockets[0]!.onclose();

    await waitFor(() => expect(mouseDragReset).toHaveBeenCalledTimes(1));
  });

  it("revokes clipboard authorization when the terminal socket closes", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    clipboardWriterCancelAuthorization.mockClear();

    mockSockets[0]!.onclose();

    await waitFor(() => expect(clipboardWriterCancelAuthorization).toHaveBeenCalledTimes(1));
  });

  it.each([
    {
      name: "legacy workspace terminal",
      props: { workspaceId: "ws-123", active: true },
    },
    {
      name: "Fleet session",
      props: {
        websocketPath: "/ws/v1/fleet/hosts/peer/workspaces/ws-123/runtime/sessions/ws-123%3Ashell/terminal",
        active: true,
      },
    },
  ])("resends dimensions without a client refresh when a $name reconnects", async ({ props }) => {
    initialTerminalDimensions = { cols: 177, rows: 41 };
    fitDimensions = initialTerminalDimensions;
    render(TerminalPane, { props });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const firstSocket = mockSockets[0]!;
    await waitForSocketConnected(firstSocket);
    firstSocket.onclose();
    await waitFor(() => expect(mockSockets).toHaveLength(2), { timeout: 2_000 });
    const reconnectedSocket = mockSockets[1]!;

    const reconnectURL = new URL(reconnectedSocket.url);
    expect(reconnectURL.searchParams.get("cols")).toBe("177");
    expect(reconnectURL.searchParams.get("rows")).toBe("41");
    await waitForSocketConnected(reconnectedSocket);
    expect(reconnectedSocket.sent.map(String)).not.toContainEqual(expect.stringContaining('"type":"refresh"'));
  });

  it("waits for replay parsing before resizing a reconnected local runtime session", async () => {
    initialTerminalDimensions = { cols: 177, rows: 41 };
    fitDimensions = initialTerminalDimensions;
    render(TerminalPane, {
      props: {
        websocketPath: "/ws/v1/workspaces/ws-123/runtime/sessions/ws-123%3Ashell/terminal",
        active: true,
      },
    });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const firstSocket = mockSockets[0]!;
    expect(new URL(firstSocket.url).searchParams.get("replay_boundary")).toBe("1");
    expect(new URL(firstSocket.url).searchParams.has("cols")).toBe(false);
    await waitForSocketConnected(firstSocket);
    expect(firstSocket.sent.map(String)).not.toContainEqual(expect.stringContaining('"type":"refresh"'));

    firstSocket.onmessage(new MessageEvent("message", { data: JSON.stringify({ type: "replay_ready" }) }));
    await waitFor(() => expect(xtermInstances[0]!.write).toHaveBeenCalled());
    const firstBoundaryCallback = xtermInstances[0]!.write.mock.calls.at(-1)?.[1] as (() => void) | undefined;
    expect(firstBoundaryCallback).toBeTypeOf("function");
    firstBoundaryCallback?.();
    await waitFor(() =>
      expect(firstSocket.sent.map(String)).toContain(JSON.stringify({ type: "claim_resize", cols: 177, rows: 41 })),
    );

    firstSocket.onclose();
    await waitFor(() => expect(mockSockets).toHaveLength(2), { timeout: 2_000 });
    const reconnectedSocket = mockSockets[1]!;
    const reconnectURL = new URL(reconnectedSocket.url);
    expect(reconnectURL.searchParams.get("replay_boundary")).toBe("1");
    expect(reconnectURL.searchParams.has("cols")).toBe(false);
    expect(reconnectURL.searchParams.has("rows")).toBe(false);
  });

  it("refreshes tmux with the dimensions fit applies after replay", async () => {
    initialTerminalDimensions = { cols: 177, rows: 41 };
    fitDimensions = initialTerminalDimensions;
    render(TerminalPane, {
      props: {
        websocketPath: "/ws/v1/workspaces/ws-123/runtime/sessions/ws-123%3Ashell/terminal",
        active: true,
      },
    });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const fitAddon = xtermFitAddons[0]!;
    const socket = mockSockets[0]!;
    await waitForSocketConnected(socket);
    socket.sent = [];
    fitAddon.proposeDimensions.mockReturnValueOnce({ cols: 177, rows: 41 }).mockReturnValue({ cols: 177, rows: 42 });

    socket.onmessage(new MessageEvent("message", { data: JSON.stringify({ type: "replay_ready" }) }));
    await waitFor(() => expect(terminal.write).toHaveBeenCalled());
    const boundaryCallback = terminal.write.mock.calls.at(-1)?.[1] as (() => void) | undefined;
    boundaryCallback?.();

    await waitFor(() => expect(terminal.rows).toBe(42));
    await waitFor(() =>
      expect(socket.sent.map(String)).toContain(JSON.stringify({ type: "claim_resize", cols: 177, rows: 42 })),
    );
  });

  it("aborts a partial OSC sequence before writing output from a reconnected socket", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const firstSocket = mockSockets[0]!;
    await waitForSocketConnected(firstSocket);
    terminal.write.mockClear();
    const binaryMessage = (text: string): MessageEvent => {
      const encoded = new TextEncoder().encode(text);
      const data = new Uint8Array(new ArrayBuffer(encoded.byteLength));
      data.set(encoded);
      return new MessageEvent("message", { data });
    };

    firstSocket.onmessage(binaryMessage("\x1b]52;c;cGFydGlhbA=="));
    await waitFor(() => expect(terminal.write).toHaveBeenCalled());
    firstSocket.onclose();
    await waitFor(() => expect(mockSockets).toHaveLength(2), { timeout: 2_000 });

    mockSockets[1]!.onmessage(binaryMessage("fresh session output"));
    await waitFor(() => expect(terminal.write).toHaveBeenCalledTimes(3));

    const writtenChunks = terminal.write.mock.calls.map(([data]) =>
      typeof data === "string" ? data : new TextDecoder().decode(data),
    );
    expect(writtenChunks).toEqual(["\x1b]52;c;cGFydGlhbA==", "\x18", "fresh session output"]);
    expect(terminal.write.mock.calls[1]![0]).toEqual(new Uint8Array([0x18]));
  });

  it("clears an incomplete UTF-8 byte before replaying it into the same terminal", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });

    await waitFor(() => expect(mockSockets).toHaveLength(1));
    const terminal = xtermInstances[0]!;
    const firstSocket = mockSockets[0]!;
    await waitForSocketConnected(firstSocket);
    terminal.write.mockClear();
    const binaryMessage = (bytes: number[]): MessageEvent => {
      const data = new Uint8Array(new ArrayBuffer(bytes.length));
      data.set(bytes);
      return new MessageEvent("message", { data });
    };

    firstSocket.onmessage(binaryMessage([0xe2]));
    await waitFor(() => expect(terminal.write).toHaveBeenCalledTimes(1));
    firstSocket.onclose();
    await waitFor(() => expect(mockSockets).toHaveLength(2), { timeout: 2_000 });

    // The new subscriber replays the prefix before live output completes the
    // rune. The byte CAN between them must clear xterm's streaming decoder.
    mockSockets[1]!.onmessage(binaryMessage([0xe2]));
    mockSockets[1]!.onmessage(binaryMessage([0x98, 0x83]));
    await waitFor(() => expect(terminal.write).toHaveBeenCalledTimes(4));

    expect(terminal.write.mock.calls.map(([data]) => Array.from(data as Uint8Array))).toEqual([
      [0xe2],
      [0x18],
      [0xe2],
      [0x98, 0x83],
    ]);
  });

  it("revokes pointer clipboard authorization when the window loses focus", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    clipboardWriterCancelPointerGesture.mockClear();

    window.dispatchEvent(new Event("blur"));

    expect(clipboardWriterCancelPointerGesture).toHaveBeenCalledTimes(1);
  });

  it("revokes pointer clipboard authorization when the document is hidden", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    clipboardWriterCancelPointerGesture.mockClear();
    const visibilityState = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");

    document.dispatchEvent(new Event("visibilitychange"));

    expect(clipboardWriterCancelPointerGesture).toHaveBeenCalledTimes(1);
    visibilityState.mockRestore();
  });

  it("revokes pending terminal clipboard writes when focus leaves the terminal", async () => {
    const { container } = render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    const terminalContainer = container.querySelector<HTMLElement>(".terminal-container");
    const outsideButton = document.createElement("button");
    container.append(outsideButton);

    terminalContainer!.dispatchEvent(new FocusEvent("focusout", { bubbles: true, relatedTarget: outsideButton }));

    expect(clipboardWriterCancelAuthorization).toHaveBeenCalledTimes(1);
  });

  it("revokes pending terminal clipboard writes before an outside click copies text", async () => {
    render(TerminalPane, { props: { workspaceId: "ws-123" } });
    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());
    clipboardWriterCancelAuthorization.mockClear();

    const outsideButton = document.createElement("button");
    document.body.append(outsideButton);
    outsideButton.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));

    expect(clipboardWriterCancelAuthorization).toHaveBeenCalledTimes(1);
    outsideButton.remove();
  });

  it("does not attach xterm sessions with unavailable initial status", async () => {
    render(TerminalPane, {
      props: {
        websocketPath: "/api/v1/workspaces/ws-123/runtime/sessions/ws-123%3Ahelper/terminal",
        reconnectOnExit: false,
        initialStatus: "error",
      },
    });

    await waitFor(() => expect(xtermTerminalCtor).toHaveBeenCalled());

    expect(mockSockets).toHaveLength(0);
    expect(xtermInstances[0]!.write).toHaveBeenCalledWith(expect.stringContaining("[Session unavailable]"));
  });

  it("sends browser multiline paste as one bracketed paste payload", async () => {
    const { container } = render(TerminalPane, {
      props: { workspaceId: "ws-123" },
    });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    xtermInstances[0]!.modes.bracketedPasteMode = true;
    mockSockets[0]!.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).toBeDefined();
    const laterPasteListener = vi.fn();
    terminalContainer!.addEventListener("paste", laterPasteListener, true);

    const event = new Event("paste", {
      bubbles: true,
      cancelable: true,
    }) as ClipboardEvent;
    Object.defineProperty(event, "clipboardData", {
      value: {
        getData: vi.fn((type: string) => (type === "text/plain" ? "first\x1b[201~\r\nsecond\r\nthird" : "")),
      },
    });

    const defaultAllowed = terminalContainer!.dispatchEvent(event);

    expect(defaultAllowed).toBe(false);
    expect(laterPasteListener).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(mockSockets[0]!.sent.map((_, index) => sentText(mockSockets[0]!, index))).toContain(
        "\x1b[200~first[201~\rsecond\rthird\x1b[201~",
      ),
    );
  });

  it.each([
    [true, "\x1b[200~first[201~\rsecond\x1b[201~\r"],
    [false, "first[201~\rsecond\r"],
  ])("sends composed input through the sanitized paste path with bracketed mode %s", async (bracketed, expected) => {
    const onSend = vi.fn();
    const { getByRole } = render(TerminalPaneInputHarness, {
      props: { data: "first\x1b[201~\r\nsecond\x07", suffix: "\r", onSend },
    });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    xtermInstances[0]!.modes.bracketedPasteMode = bracketed;
    mockSockets[0]!.sent = [];

    await fireEvent.click(getByRole("button", { name: "Send pasted input" }));

    expect(onSend).toHaveBeenCalledWith(true);
    await waitFor(() => expect(mockSockets[0]!.sent).toHaveLength(2));
    expect(mockSockets[0]!.sent.map((_, index) => sentText(mockSockets[0]!, index))).toEqual([
      JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }),
      expected,
    ]);
  });

  it("sends browser multiline paste raw when bracketed paste is disabled", async () => {
    const { container } = render(TerminalPane, {
      props: { workspaceId: "ws-123" },
    });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mockSockets[0]!.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).toBeDefined();

    const event = new Event("paste", {
      bubbles: true,
      cancelable: true,
    }) as ClipboardEvent;
    Object.defineProperty(event, "clipboardData", {
      value: {
        getData: vi.fn((type: string) => (type === "text/plain" ? "first\r\nsecond\r\nthird" : "")),
      },
    });

    const defaultAllowed = terminalContainer!.dispatchEvent(event);

    expect(defaultAllowed).toBe(false);
    await waitFor(() =>
      expect(mockSockets[0]!.sent.map((_, index) => sentText(mockSockets[0]!, index))).toContain(
        "first\rsecond\rthird",
      ),
    );
  });

  it("sends single-line browser paste through the terminal session once", async () => {
    const { container } = render(TerminalPane, {
      props: { workspaceId: "ws-123" },
    });

    await waitFor(() => expect(xtermOnDataHandlers).toHaveLength(1));
    await waitFor(() => expect(mockSockets).toHaveLength(1));
    mockSockets[0]!.sent = [];
    const terminalContainer = container.querySelector(".terminal-container");
    expect(terminalContainer).toBeDefined();
    const laterPasteListener = vi.fn();
    terminalContainer!.addEventListener("paste", laterPasteListener, true);

    const event = new Event("paste", {
      bubbles: true,
      cancelable: true,
    }) as ClipboardEvent;
    Object.defineProperty(event, "clipboardData", {
      value: {
        getData: vi.fn((type: string) => (type === "text/plain" ? "single\x1b[201~ line" : "")),
      },
    });

    const defaultAllowed = terminalContainer!.dispatchEvent(event);

    expect(defaultAllowed).toBe(false);
    expect(laterPasteListener).not.toHaveBeenCalled();
    await waitFor(() => expect(mockSockets[0]!.sent).toHaveLength(2));
    expect(mockSockets[0]!.sent.map((_, index) => sentText(mockSockets[0]!, index))).toEqual([
      JSON.stringify({ type: "claim_resize", cols: 80, rows: 24 }),
      "single[201~ line",
    ]);
  });
});

function socketFramesOfType(socket: MockWebSocket, type: string): string[] {
  return socket.sent
    .map((_, index) => sentText(socket, index))
    .filter((frame) => {
      try {
        const decoded: unknown = JSON.parse(frame);
        return typeof decoded === "object" && decoded !== null && "type" in decoded && decoded.type === type;
      } catch {
        return false;
      }
    });
}

function sentText(socket: MockWebSocket, index: number): string {
  const value = socket.sent[index];
  if (typeof value === "string") return value;
  if (value instanceof ArrayBuffer) {
    return new TextDecoder().decode(value);
  }
  return new TextDecoder().decode(value);
}
