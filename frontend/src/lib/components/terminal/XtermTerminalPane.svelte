<script lang="ts">
  import { Deferred, Effect, Queue } from "effect";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { makeAnimationFrameScheduler, nextAnimationFrame } from "../../browser/animation-frame.js";
  import { observeResize } from "../../browser/observers.js";
  import { getStores } from "../../context.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import { parseConfiguredProviderItemURL } from "../../utils/item-reference.js";
  import { resolveItemReference } from "../../utils/itemRefHandler.js";
  import { Terminal } from "@xterm/xterm";
  import type { ILinkHandler } from "@xterm/xterm";
  import { FitAddon } from "@xterm/addon-fit";
  import { ImageAddon } from "@xterm/addon-image";
  import { LigaturesAddon } from "@xterm/addon-ligatures/lib/addon-ligatures.mjs";
  import { WebLinksAddon } from "@xterm/addon-web-links";
  import { WebglAddon } from "@xterm/addon-webgl";
  import "@xterm/xterm/css/xterm.css";
  import { workspaceTmuxWebSocketPath } from "../../api/workspace-runtime.js";
  import { createWorkspaceSwitchPaneTimer } from "../../instrumentation/workspaceSwitchTiming.js";
  import { traceHeadersForRequest } from "../../instrumentation/traceContext.js";
  import { createTerminalPastePayload } from "./bracketedPaste.js";
  import { embeddedWebSocketUrl } from "./embeddedWebSocket.js";
  import { parseOsc52ClipboardWrite } from "./osc52Clipboard.js";
  import {
    createBrowserTerminalClipboardPort,
    makeTerminalClipboardWriter,
    type TerminalClipboardWriter,
  } from "./terminalClipboardWriter.js";
  import {
    buildTerminalFontFamily,
    primaryTerminalFontFamily,
  } from "./terminalFontFamily.js";
  import { createInitialFocusIntent } from "./terminal-focus.js";
  import { supportsReplayBoundary } from "./terminalReplayBoundary.js";
  import {
    clearSharedTerminalTextureAtlas,
    registerTerminalTextureAtlasParticipant,
  } from "./sharedTerminalTextureAtlas.js";
  import {
    makeTmuxMouseDragAutoscroll,
    type TmuxMouseDragAutoscroll,
  } from "./tmuxMouseDragAutoscroll.js";
  import {
    makeTerminalSessionController,
    type TerminalMessageDecision,
    type TerminalSessionController,
  } from "./terminal-session.js";
  import { terminalAttachment } from "./terminal-attachment.js";
  import type { TerminalKey } from "./terminal-key.js";
  import { currentTerminalGeometryIntent } from "./terminalGeometryIntent.js";
  import { decodeTerminalControlMessage } from "./terminal-control-message.js";

  interface TerminalPaneProps {
    workspaceId?: string | undefined;
    websocketPath?: string | undefined;
    reconnectOnExit?: boolean | undefined;
    active?: boolean | undefined;
    renderingEnabled?: boolean | undefined;
    autoFocus?: boolean | undefined;
    cursorWheelInput?: boolean;
    disabled?: boolean;
    onExit?: ((code: number) => void) | undefined;
    onConnectionChange?: ((connected: boolean) => void) | undefined;
    // When the session is not attachable at mount time, skip the
    // WebSocket connect — the server's attach endpoint returns 404
    // for non-running sessions, which would loop scheduleReconnect.
    initialStatus?: string | undefined;
  }

  interface TerminalGeometry {
    cols: number;
    rows: number;
    pixelWidth?: number;
    pixelHeight?: number;
  }

  let {
    workspaceId,
    websocketPath,
    reconnectOnExit = true,
    active = true,
    renderingEnabled = true,
    autoFocus = true,
    cursorWheelInput = false,
    disabled = false,
    onExit,
    onConnectionChange,
    initialStatus,
  }: TerminalPaneProps = $props();
  const runtime = getAppRuntime();
  const { settings: settingsStore } = getStores();

  const basePath = (window.__BASE_PATH__ ?? "/").replace(/\/$/, "");
  const terminalLinkUsesMetaKey = /Mac/.test(navigator.platform);
  const terminalLinkModifierLabel = terminalLinkUsesMetaKey ? "Cmd" : "Ctrl";

  let containerEl: HTMLElement;
  let terminal: Terminal | null = $state(null);
  let hoveredTerminalLink: string | null = $state(null);
  let itemLinkExecution: { interrupt: () => void } | null = null;
  const hoveredTerminalLinkIsItem = $derived(
    hoveredTerminalLink !== null && configuredItemReference(hoveredTerminalLink) !== null,
  );
  let fitAddon: FitAddon | null = null;
  let imageAddon: ImageAddon | null = null;
  let ligaturesAddon: LigaturesAddon | null = null;
  let webglAddon: WebglAddon | null = null;
  let rendererParked = false;
  let terminalSession: TerminalSessionController | null = null;
  let connectionGeneration = 0;
  let unregisterTextureAtlasParticipant: (() => void) | null = null;
  let requestTerminalRefresh = (): void => {};
  let requestTerminalResize = (): void => {};
  let requestFirstPaint = (): void => {};
  let publishClipboardWrite = (_text: string): void => {};
  // What the PTY was last told. Resends are skipped against this, so a
  // ResizeObserver burst costs one frame, not one per callback.
  let sentCols = 0;
  let sentRows = 0;
  let sentPixelWidth = 0;
  let sentPixelHeight = 0;
  let sentResizeActive: boolean | null = null;
  let claimedGeometryIntentGeneration = 0;
  let resizeReady = true;
  let appliedTerminalFontFamily = "";
  let appliedFontSize = 0;
  let appliedScrollback = 0;
  let appliedLineHeight = 0;
  let appliedLetterSpacing = 0;
  let appliedCursorBlink = true;
  let appliedFontLigatures = false;
  let disposed = false;
  let sawFirstBytes = false;
  let clipboardFailureReported = false;
  let activePointerId: number | null = null;
  let pointerOrigin: { clientX: number; clientY: number } | null = null;
  let lastTouchGesturePoint: { clientX: number; clientY: number } | null = null;
  let explicitFocusRequested = false;
  const encoder = new TextEncoder();
  let clipboardWriter: TerminalClipboardWriter | undefined;
  let mouseDragAutoscroll: TmuxMouseDragAutoscroll | undefined;
  // Binds this pane to the workspace switch that was live at creation;
  // panes surviving from a previous workspace record nothing.
  const switchTimer = createWorkspaceSwitchPaneTimer();
  // Captured at mount, before the async font-load race in start() below,
  // so a later focus steal is detected even though start() itself cannot
  // observe when focus moved during the wait.
  const focusIntent = createInitialFocusIntent();

  export function focus(): void {
    if (terminal !== null) {
      terminal.focus();
      return;
    }
    explicitFocusRequested = true;
  }

  export function sendInput(data: string): boolean {
    if (disabled || !terminal || !terminalSession?.isConnected()) return false;
    terminal.input(data, true);
    return true;
  }

  export function sendPastedInput(data: string, suffix = ""): boolean {
    if (disabled || !terminal || !terminalSession?.isConnected()) return false;
    claimTerminalResize();
    terminalSession.send(
      encoder.encode(`${createTerminalPastePayload(data, terminal.modes.bracketedPasteMode)}${suffix}`),
    );
    return true;
  }

  const terminalKeyEvents: Record<TerminalKey, { key: string; code: string; keyCode: number }> = {
    Escape: { key: "Escape", code: "Escape", keyCode: 27 },
    Tab: { key: "Tab", code: "Tab", keyCode: 9 },
    ArrowLeft: { key: "ArrowLeft", code: "ArrowLeft", keyCode: 37 },
    ArrowUp: { key: "ArrowUp", code: "ArrowUp", keyCode: 38 },
    ArrowDown: { key: "ArrowDown", code: "ArrowDown", keyCode: 40 },
    ArrowRight: { key: "ArrowRight", code: "ArrowRight", keyCode: 39 },
    Space: { key: " ", code: "Space", keyCode: 32 },
    Enter: { key: "Enter", code: "Enter", keyCode: 13 },
  };

  function terminalKeyEvent(type: "keydown" | "keypress" | "keyup", key: TerminalKey): KeyboardEvent {
    const definition = terminalKeyEvents[key];
    const event = new KeyboardEvent(type, {
      bubbles: true,
      cancelable: true,
      code: definition.code,
      key: definition.key,
    });
    Object.defineProperties(event, {
      charCode: { value: key === "Space" && type === "keypress" ? definition.keyCode : 0 },
      keyCode: { value: definition.keyCode },
      which: { value: definition.keyCode },
    });
    return event;
  }

  export function sendKey(key: TerminalKey): boolean {
    if (disabled || !terminal || !terminalSession?.isConnected()) return false;
    const input = containerEl.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    if (!input) return false;
    if (!claimTerminalResize()) return false;
    applyTerminalSoftwareKeyboardPolicy(input);
    const previousFocus = input.ownerDocument.activeElement;
    const keyDownAllowed = input.dispatchEvent(terminalKeyEvent("keydown", key));
    if (key === "Space" && keyDownAllowed) input.dispatchEvent(terminalKeyEvent("keypress", key));
    input.dispatchEvent(terminalKeyEvent("keyup", key));
    if (previousFocus !== input && input.ownerDocument.activeElement === input) {
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) {
        previousFocus.focus({ preventScroll: true });
      }
      if (input.ownerDocument.activeElement === input) input.blur();
    }
    return true;
  }

  const TERMINAL_SMOOTH_SCROLL_DURATION = 0;
  const TERMINAL_MINIMUM_CONTRAST_RATIO = 4.5;
  const TERMINAL_FONT_WAIT_MS = 300;
  const TERMINAL_FONT_LOAD_GLYPHS = "0MWim@#";
  const TERMINAL_SEQUENCE_CANCEL = new Uint8Array([0x18]);
  const POINTER_SELECTION_INTENT_MIN_PX = 4;

  function isAttachableInitialStatus(status: string | undefined): boolean {
    return status === undefined || status === "running" || status === "starting";
  }

  function reportClipboardFailure(): void {
    if (disposed || clipboardFailureReported) return;
    clipboardFailureReported = true;
    showFlash("Could not write the terminal selection to the clipboard.", {
      tone: "danger",
    });
  }

  function cancelPendingTerminalSequence(callback?: () => void): void {
    terminal?.write(TERMINAL_SEQUENCE_CANCEL, callback);
  }

  function handleOsc52Clipboard(data: string): boolean {
    const result = parseOsc52ClipboardWrite(data);
    if (result.status === "rejected") return true;
    if (disposed || disabled || !terminalSession?.isConnected()) return true;

    publishClipboardWrite(result.text);
    return true;
  }

  function openTerminalLink(event: MouseEvent, uri: string): void {
    if (!terminalLinkModifierPressed(event)) return;

    let url: URL;
    try {
      url = new URL(uri);
    } catch {
      return;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") return;

    // Pull request and issue links for a configured repository stay inside
    // the app: the resolve endpoint decides whether the repo is tracked and
    // falls back to opening the provider page when it is not.
    const itemRef = configuredItemReference(url.href);
    if (itemRef) {
      itemLinkExecution?.interrupt();
      itemLinkExecution = resolveItemReference(runtime, itemRef);
      return;
    }

    window.open(url.href, "_blank", "noopener,noreferrer");
  }

  function configuredItemReference(href: string) {
    return parseConfiguredProviderItemURL(href, settingsStore.getConfiguredRepos());
  }

  function terminalLinkModifierPressed(event: MouseEvent): boolean {
    return terminalLinkUsesMetaKey ? event.metaKey : event.ctrlKey;
  }

  function showTerminalLink(_event: MouseEvent, uri: string): void {
    hoveredTerminalLink = uri;
  }

  function hideTerminalLink(): void {
    hoveredTerminalLink = null;
  }

  const terminalLinkHandler: ILinkHandler = {
    activate: openTerminalLink,
    hover: showTerminalLink,
    leave: hideTerminalLink,
  };

  function handleTerminalPointerDown(event: PointerEvent): void {
    if (disposed || disabled) return;
    if (event.button !== 0) {
      if (
        (event.button === 1 || event.button === 2) &&
        event.isTrusted &&
        terminal?.modes.mouseTrackingMode !== "none"
      ) {
        claimTerminalResize();
      }
      return;
    }
    // xterm focuses its hidden textarea from mousedown, but its touch gesture
    // path only handles scrolling. Focus synchronously while the touch/pen
    // activation is live, applying the owning surface's software-keyboard
    // policy first.
    if (active && (event.pointerType === "touch" || event.pointerType === "pen")) {
      const input = containerEl.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
      if (input) applyTerminalSoftwareKeyboardPolicy(input);
      terminal?.focus();
    }
    if (!event.isTrusted) return;
    claimTerminalResize();
    if (activePointerId !== null) {
      cancelTerminalPointerGesture();
    }
    // Over an active link, pointer capture retargets mouseup to the container
    // and prevents xterm's linkifier from activating it. Modified non-link
    // gestures still need the normal clipboard and drag lifecycle below.
    if (hoveredTerminalLink !== null && terminalLinkModifierPressed(event)) return;
    activePointerId = event.pointerId;
    pointerOrigin = { clientX: event.clientX, clientY: event.clientY };
    clipboardWriter?.beginPointerGesture();
    try {
      containerEl.setPointerCapture(event.pointerId);
    } catch {
      // The timeout and global cancellation handlers still bound the gesture.
    }
  }

  function applyTerminalSoftwareKeyboardPolicy(input: HTMLTextAreaElement): void {
    if (containerEl.closest('[data-terminal-software-keyboard="manual"]')) {
      input.setAttribute("inputmode", "none");
    } else {
      input.removeAttribute("inputmode");
    }
  }

  function handleTerminalPointerEnd(event: PointerEvent): void {
    if (!event.isTrusted || activePointerId !== event.pointerId) return;
    activePointerId = null;
    pointerOrigin = null;
    releaseTerminalPointerCapture(event.pointerId);
    clipboardWriter?.endPointerGesture();
    mouseDragAutoscroll?.endPointerGesture();
  }

  function releaseTerminalPointerCapture(pointerId: number): void {
    try {
      if (containerEl.hasPointerCapture(pointerId)) {
        containerEl.releasePointerCapture(pointerId);
      }
    } catch {
      // Capture may already be gone after focus or visibility loss.
    }
  }

  function cancelTerminalPointerGesture(pointerId?: number): void {
    if (pointerId !== undefined && activePointerId !== pointerId) return;
    const capturedPointerId = activePointerId;
    activePointerId = null;
    pointerOrigin = null;
    if (capturedPointerId !== null) {
      releaseTerminalPointerCapture(capturedPointerId);
    }
    clipboardWriter?.cancelPointerGesture();
    mouseDragAutoscroll?.endPointerGesture();
  }

  function handleTerminalPointerCancel(event: PointerEvent): void {
    cancelTerminalPointerGesture(event.pointerId);
  }

  function handleTerminalLostPointerCapture(event: PointerEvent): void {
    if (activePointerId !== event.pointerId) return;
    activePointerId = null;
    pointerOrigin = null;
    clipboardWriter?.cancelPointerGesture();
    mouseDragAutoscroll?.endPointerGesture();
  }

  function cancelTerminalClipboardAuthorization(): void {
    cancelTerminalPointerGesture();
    clipboardWriter?.cancelAuthorization();
  }

  function handleTerminalFocusOut(event: FocusEvent): void {
    const nextTarget = event.relatedTarget;
    if (nextTarget instanceof Node && containerEl.contains(nextTarget)) return;
    // Pointer capture can briefly produce a focusout without a destination.
    // A concrete external target is an actual focus transfer and must revoke.
    if (nextTarget === null && activePointerId !== null) return;
    cancelTerminalClipboardAuthorization();
  }

  function handleDocumentPointerDown(event: PointerEvent): void {
    const target = event.target;
    if (target instanceof Node && containerEl.contains(target)) return;
    cancelTerminalClipboardAuthorization();
  }

  function handleWindowBlur(): void {
    cancelTerminalClipboardAuthorization();
  }

  function handleDocumentVisibilityChange(): void {
    if (document.visibilityState !== "visible") {
      cancelTerminalClipboardAuthorization();
    }
  }

  $effect(() => {
    if (active && !disabled) return;
    cancelTerminalClipboardAuthorization();
  });

  function handleTerminalKeyDown(event: KeyboardEvent): void {
    if (disposed || disabled || event.isComposing || !event.isTrusted) return;
    claimTerminalResize();
    clipboardWriter?.authorizeKeyboardGesture();
  }

  function handleTerminalCompositionStart(event: CompositionEvent): void {
    if (disposed || disabled || !event.isTrusted) return;
    claimTerminalResize();
  }

  function isBrowserPasteShortcut(event: KeyboardEvent): boolean {
    const pasteModifierPressed = terminalLinkUsesMetaKey ? event.metaKey : event.ctrlKey;
    return !event.altKey && pasteModifierPressed && event.key.toLowerCase() === "v";
  }

  // xterm turns touch flings into momentum "gesturechange" events after the
  // finger lifts. Those synthetic events carry a translation but no
  // clientX/clientY, so when an app has mouse tracking on (Codex, tmux, vim)
  // xterm reports the wheel at NaN;NaN and the app receives the tail of the
  // malformed sequence as typed text. Capture runs before xterm's own listener
  // on the screen element, so pin momentum reports to the last finger position.
  const XTERM_GESTURE_CHANGE_EVENT = "-xterm-gesturechange";

  function handleTerminalGestureChange(event: Event): void {
    const gesture = event as Event & { clientX?: number; clientY?: number };
    if (Number.isFinite(gesture.clientX) && Number.isFinite(gesture.clientY)) {
      lastTouchGesturePoint = { clientX: gesture.clientX!, clientY: gesture.clientY! };
      return;
    }
    if (lastTouchGesturePoint === null) return;
    gesture.clientX = lastTouchGesturePoint.clientX;
    gesture.clientY = lastTouchGesturePoint.clientY;
  }

  function handleInsecureTerminalRightMouse(event: MouseEvent): void {
    if (window.isSecureContext || event.button !== 2) return;
    event.stopPropagation();
  }

  function handleTerminalWheel(event: WheelEvent): void {
    if (
      disposed ||
      disabled ||
      event.deltaY === 0 ||
      event.ctrlKey ||
      event.shiftKey ||
      !terminal ||
      !terminalSession?.isConnected()
    ) return;

    const activeBuffer = terminal.buffer.active;
    if (terminal.modes.mouseTrackingMode !== "none") {
      claimTerminalResize();
      return;
    }
    if (!cursorWheelInput) return;
    if (
      activeBuffer.type !== "normal" ||
      activeBuffer.baseY > 0
    ) return;

    event.preventDefault();
    event.stopPropagation();
    claimTerminalResize();
    const cursorPrefix = terminal.modes.applicationCursorKeysMode ? "O" : "[";
    const cursorDirection = event.deltaY < 0 ? "A" : "B";
    terminalSession.send(encoder.encode(`\x1b${cursorPrefix}${cursorDirection}`));
  }

  function hasPointerSelectionIntent(event: PointerEvent): boolean {
    if (pointerOrigin === null) return false;
    const cell = containerEl.querySelector<HTMLElement>(".xterm-helper-textarea")?.getBoundingClientRect();
    if (!cell || cell.width <= 0 || cell.height <= 0) return false;

    const deltaX = event.clientX - pointerOrigin.clientX;
    const deltaY = event.clientY - pointerOrigin.clientY;
    const pixelsMovedSquared = deltaX * deltaX + deltaY * deltaY;
    const columnsMoved = deltaX / cell.width;
    const rowsMoved = deltaY / cell.height;
    return (
      pixelsMovedSquared >= POINTER_SELECTION_INTENT_MIN_PX ** 2 &&
      columnsMoved * columnsMoved + rowsMoved * rowsMoved >= 1
    );
  }

  function handleWindowPointerMove(event: PointerEvent): void {
    if (disposed || disabled || !terminal) return;
    if (activePointerId === event.pointerId && hasPointerSelectionIntent(event)) {
      clipboardWriter?.confirmPointerSelection();
      pointerOrigin = null;
    }
    const screen = containerEl.querySelector<HTMLElement>(".xterm-screen");
    const bounds = (screen ?? containerEl).getBoundingClientRect();
    mouseDragAutoscroll?.updatePointer({
      clientX: event.clientX,
      clientY: event.clientY,
      bounds,
      cols: terminal.cols,
      rows: terminal.rows,
    });
  }

  function initialStatusMessage(status: string | undefined): string {
    return status === "exited" ? "Process exited" : "Session unavailable";
  }

  function defaultTerminalFontFamily(): string {
    const rootFontFamily = getComputedStyle(
      document.documentElement,
    )
      .getPropertyValue("--font-mono")
      .trim();
    return rootFontFamily || "monospace";
  }

  const terminalFontFamily = $derived.by(() => {
    const configured = settingsStore
      .getTerminalFontFamily()
      .trim();
    return buildTerminalFontFamily(configured, defaultTerminalFontFamily());
  });
  const terminalFontSize = $derived(settingsStore.getTerminalFontSize());
  const terminalScrollback = $derived(settingsStore.getTerminalScrollback());
  const terminalLineHeight = $derived(settingsStore.getTerminalLineHeight());
  const terminalLetterSpacing = $derived(
    settingsStore.getTerminalLetterSpacing(),
  );
  const terminalCursorBlink = $derived(
    settingsStore.getTerminalCursorBlink(),
  );
  const terminalFontLigatures = $derived(
    settingsStore.getTerminalFontLigatures(),
  );
  const terminalGraphics = $derived(
    settingsStore.getTerminalGraphics(),
  );

  function defaultWebsocketPath(): string {
    if (!workspaceId) return "";
    return workspaceTmuxWebSocketPath(workspaceId);
  }

  function terminalSupportsReplayBoundary(): boolean {
    return supportsReplayBoundary(websocketPath ?? defaultWebsocketPath());
  }

  function appendConnectionParams(
    url: string,
    size: TerminalGeometry | null,
    replayBoundary: boolean,
  ): string {
    const sep = url.includes("?") ? "&" : "?";
    const resizeActive = resizeAuthorityRegionSize() !== null ? "1" : "0";
    const { traceparent, baggage } = traceHeadersForRequest();
    let sizeParams = size ? `cols=${size.cols}&rows=${size.rows}&` : "";
    if (size?.pixelWidth && size.pixelHeight) {
      sizeParams += `pixel_width=${size.pixelWidth}&pixel_height=${size.pixelHeight}&`;
    }
    const replayBoundaryParam = replayBoundary ? "replay_boundary=1&" : "";
    let result = `${url}${sep}${sizeParams}${replayBoundaryParam}resize_active=${resizeActive}&traceparent=${encodeURIComponent(traceparent)}`;
    if (baggage !== null) result += `&baggage=${encodeURIComponent(baggage)}`;
    return result;
  }

  function buildWsUrl(
    size: TerminalGeometry | null,
    replayBoundary: boolean,
  ): string | null {
    const path = websocketPath ?? defaultWebsocketPath();
    if (!path) return null;

    const withConnectionParams = appendConnectionParams(path, size, replayBoundary);
    if (/^wss?:\/\//.test(withConnectionParams)) {
      return withConnectionParams;
    }
    const embeddedUrl = embeddedWebSocketUrl(withBasePath(withConnectionParams));
    if (embeddedUrl) return embeddedUrl;
    const devUrl = buildDevApiWsUrl(withConnectionParams);
    if (devUrl) return devUrl;
    const proto = location.protocol === "https:" ? "wss" : "ws";
    return `${proto}://${location.host}${withBasePath(withConnectionParams)}`;
  }

  function withBasePath(path: string): string {
    const normalizedPath = path.startsWith("/") ? path : `/${path}`;
    if (!basePath) return normalizedPath;
    if (
      normalizedPath === basePath ||
      normalizedPath.startsWith(`${basePath}/`)
    ) {
      return normalizedPath;
    }
    return `${basePath}${normalizedPath}`;
  }

  function buildDevApiWsUrl(path: string): string | null {
    if (!import.meta.env.DEV) return null;
    const apiUrl = window.__KENN_FORGE_DEV_API_URL__?.trim();
    if (!apiUrl || !path.startsWith("/api/")) return null;

    try {
      const base = new URL(apiUrl);
      const requested = new URL(path, "http://forge.local");
      const basePath = base.pathname.replace(/\/$/, "");
      base.protocol = base.protocol === "https:" ? "wss:" : "ws:";
      base.pathname = `${basePath}${requested.pathname}`;
      base.search = requested.search;
      base.hash = "";
      return base.toString();
    } catch {
      return null;
    }
  }

  function sendResize(geometry: TerminalGeometry, claim: boolean = false): boolean {
    return sendControl(claim ? "claim_resize" : "resize", geometry);
  }

  function sendRefresh(geometry: TerminalGeometry): boolean {
    return sendControl("refresh", geometry);
  }

  function claimTerminalResize(refit: boolean = false): boolean {
    const size = resizeAuthorityRegionSize();
    if (!resizeReady || !size || !terminal || !terminalSession?.isConnected()) return false;

    if (refit || terminal.cols !== size.cols || terminal.rows !== size.rows) {
      fitAddon?.fit();
      terminal.refresh(0, Math.max(0, terminal.rows - 1));
    }
    const geometry = terminalGeometry(terminal.cols, terminal.rows);
    if (!sendControl("claim_resize", geometry)) return false;
    sentResizeActive = true;
    recordSentGeometry(geometry);
    return true;
  }

  function sendResizeActive(nextActive: boolean): boolean {
    if (sentResizeActive === nextActive) return false;
    // A geometry claim activates a pane and transfers resize ownership in one
    // server operation. Deactivation remains a separate message because it
    // has no geometry to apply.
    if (nextActive) return true;
    if (terminalSession?.isConnected()) {
      terminalSession.send(JSON.stringify({ type: "resize_active", active: nextActive }));
      sentResizeActive = nextActive;
      return true;
    }
    return false;
  }

  function sendControl(
    type: "claim_resize" | "resize" | "refresh",
    geometry: TerminalGeometry,
  ): boolean {
    if (!resizeReady || !terminalSession?.isConnected()) return false;
    terminalSession.send(JSON.stringify(terminalControlMessage(type, geometry)));
    return true;
  }

  function terminalControlMessage(
    type: "claim_resize" | "resize" | "refresh",
    geometry: TerminalGeometry,
  ): {
    type: "claim_resize" | "resize" | "refresh";
    cols: number;
    rows: number;
    pixel_width?: number;
    pixel_height?: number;
  } {
    const message: {
      type: "claim_resize" | "resize" | "refresh";
      cols: number;
      rows: number;
      pixel_width?: number;
      pixel_height?: number;
    } = {
      type,
      cols: geometry.cols,
      rows: geometry.rows,
    };
    if (geometry.pixelWidth && geometry.pixelHeight) {
      message.pixel_width = geometry.pixelWidth;
      message.pixel_height = geometry.pixelHeight;
    }
    return message;
  }

  function terminalGeometry(cols: number, rows: number): TerminalGeometry {
    const screen = containerEl.querySelector<HTMLElement>(".xterm-screen");
    const bounds = screen?.getBoundingClientRect();
    if (!bounds || bounds.width <= 0 || bounds.height <= 0) {
      return { cols, rows };
    }
    return {
      cols,
      rows,
      pixelWidth: Math.max(1, Math.round(bounds.width)),
      pixelHeight: Math.max(1, Math.round(bounds.height)),
    };
  }

  function recordSentGeometry(geometry: TerminalGeometry): void {
    sentCols = geometry.cols;
    sentRows = geometry.rows;
    sentPixelWidth = geometry.pixelWidth ?? 0;
    sentPixelHeight = geometry.pixelHeight ?? 0;
  }

  function refreshVisibleTerminal(): void {
    const size = resizeAuthorityRegionSize();
    if (!size || !terminal) return;

    fitAddon?.fit();
    const fittedGeometry = terminalGeometry(terminal.cols, terminal.rows);
    terminal.refresh(0, Math.max(0, fittedGeometry.rows - 1));
    // The server resizes on a refresh's dimensions too, so a delivered refresh
    // counts as the size the PTY now has.
    if (sendRefresh(fittedGeometry)) {
      recordSentGeometry(fittedGeometry);
    }
  }

  function redrawTerminalTextureAtlas(): void {
    if (!terminal) return;

    clearSharedTerminalTextureAtlas(terminal);
    terminal.refresh(0, Math.max(0, terminal.rows - 1));
  }

  function shouldUseBuiltinRenderer(): boolean {
    const userAgent = navigator.userAgent.toLowerCase();
    return userAgent.includes("firefox/") || userAgent.includes("android");
  }

  function recreateWebglAddon(): void {
    if (!terminal) return;
    webglAddon?.dispose();
    webglAddon = null;
    if (rendererParked) return;
    if (shouldUseBuiltinRenderer()) {
      scheduleTerminalResize();
      return;
    }
    try {
      const wgl = new WebglAddon({ customGlyphs: true });
      wgl.onContextLoss(() => {
        wgl.dispose();
        if (webglAddon === wgl) webglAddon = null;
        scheduleTerminalResize();
      });
      terminal.loadAddon(wgl);
      webglAddon = wgl;
      scheduleTerminalResize();
    } catch {
      // WebGL unavailable; xterm's built-in renderer remains active.
    }
  }

  function syncRendererState(): void {
    if (!terminal) return;
    const shouldPark = !renderingEnabled;
    if (rendererParked === shouldPark) return;
    rendererParked = shouldPark;
    if (shouldPark) {
      webglAddon?.dispose();
      webglAddon = null;
      return;
    }
    recreateWebglAddon();
    scheduleTerminalRefresh();
  }

  function syncLigaturesAddon(): void {
    if (!terminal) return;
    ligaturesAddon?.dispose();
    ligaturesAddon = null;
    if (terminalFontLigatures) {
      ligaturesAddon = new LigaturesAddon();
      terminal.loadAddon(ligaturesAddon);
    }
    recreateWebglAddon();
  }

  function syncImageAddon(enabled: boolean): void {
    if (!terminal) return;
    if (!enabled) {
      imageAddon?.dispose();
      imageAddon = null;
      return;
    }
    if (imageAddon) return;
    imageAddon = new ImageAddon({
      iipSupport: true,
      kittySupport: false,
      sixelSupport: true,
    });
    terminal.loadAddon(imageAddon);
  }

  function scheduleTerminalRefresh(): void {
    requestTerminalRefresh();
  }

  function scheduleTerminalResize(): void {
    requestTerminalResize();
  }

  /**
   * The size this terminal's region actually is, or null when it has none.
   *
   * A parked terminal sits in a `display:none` node, whose content box is zero,
   * so the fit addon proposes nothing usable for it — measuring that is what
   * used to resize a live tmux pane to one row. The measurement itself is the
   * geometry check. Painted state is applied separately because
   * `visibility:hidden` retains dimensions.
   */
  function terminalRegionSize(): { cols: number; rows: number } | null {
    if (!fitAddon || !terminal || !containerEl.isConnected) return null;

    const proposed = fitAddon.proposeDimensions();
    if (!proposed) return null;
    if (!Number.isFinite(proposed.cols) || !Number.isFinite(proposed.rows)) return null;
    if (proposed.cols < 1 || proposed.rows < 1) return null;

    return { cols: proposed.cols, rows: proposed.rows };
  }

  function resizeAuthorityRegionSize(): { cols: number; rows: number } | null {
    if (!active) return null;
    return terminalRegionSize();
  }

  /**
   * Push the region's size whenever it differs from what the PTY was last told.
   *
   * Painted state is not focus: every visible split leaf is active and owns its
   * own size. Requiring it here prevents a visibility-hidden tab, whose geometry
   * remains measurable, from resizing the PTY. Sending only on a real change
   * keeps a ResizeObserver burst from turning into a burst of resize frames.
   */
  function resizeVisibleTerminal(): void {
    const size = terminalRegionSize();
    const resizeActive = active && size !== null;
    const authorityChanged = sendResizeActive(resizeActive);
    if (!resizeActive || !size || !terminal) return;

    fitAddon?.fit();
    const fittedGeometry = terminalGeometry(terminal.cols, terminal.rows);
    const dimensionsChanged = fittedGeometry.cols !== sentCols ||
      fittedGeometry.rows !== sentRows ||
      (fittedGeometry.pixelWidth ?? 0) !== sentPixelWidth ||
      (fittedGeometry.pixelHeight ?? 0) !== sentPixelHeight;
    terminal.refresh(0, Math.max(0, fittedGeometry.rows - 1));
    // Report the dimensions fit() actually applied. It takes its own fresh
    // measurement, so the region can cross a row or column boundary after the
    // authority preflight above but before xterm is resized.
    // Re-send unchanged dimensions when reclaiming authority because another
    // attachment may have resized the PTY while this region had no geometry.
    if (!authorityChanged && !dimensionsChanged) return;
    // Recorded only once the socket carried it — a resize computed before the
    // socket opened would otherwise be suppressed forever as already sent.
    const geometryIntentGeneration = dimensionsChanged
      ? currentTerminalGeometryIntent()
      : null;
    const claim = authorityChanged || (
      geometryIntentGeneration !== null &&
      geometryIntentGeneration !== claimedGeometryIntentGeneration
    );
    if (sendResize(fittedGeometry, claim)) {
      if (authorityChanged) {
        sentResizeActive = true;
      }
      if (claim && geometryIntentGeneration !== null) {
        claimedGeometryIntentGeneration = geometryIntentGeneration;
      }
      recordSentGeometry(fittedGeometry);
    }
  }

  function handleTerminalPaste(event: ClipboardEvent): void {
    if (disabled) {
      event.preventDefault();
      event.stopImmediatePropagation();
      return;
    }
    if (!terminalSession?.isConnected()) return;

    const pastedText =
      event.clipboardData?.getData("text/plain") ||
      event.clipboardData?.getData("text") ||
      "";
    if (pastedText === "") return;

    if (!sendPastedInput(pastedText)) return;
    event.preventDefault();
    event.stopImmediatePropagation();
  }

  function handleTerminalMessage(data: string | Uint8Array): TerminalMessageDecision {
    if (!terminal) return "continue";
    if (data instanceof Uint8Array) {
      if (!sawFirstBytes) {
        sawFirstBytes = true;
        if (switchTimer.record("first-bytes", { byteLength: data.byteLength })) {
          terminal.write(data, () => {
            requestFirstPaint();
          });
        } else {
          terminal.write(data);
        }
      } else {
        terminal.write(data);
      }
      return "continue";
    }

    const message = decodeTerminalControlMessage(data);
    if (message === null) return "continue";
    if (message.type === "replay_ready" && terminalSupportsReplayBoundary()) {
      const generation = connectionGeneration;
      cancelPendingTerminalSequence(() => {
        if (disposed || generation !== connectionGeneration || !terminalSession?.isConnected()) return;
        resizeReady = true;
        // Replayed state can belong to an older browser attachment. Claim with
        // the geometry xterm has now so this pane becomes the resize owner
        // without first restoring the retained PTY's fallback size.
        claimTerminalResize(true);
      });
      return "continue";
    }
    if (message.type !== "exited") return "continue";

    cancelPendingTerminalSequence();
    onExit?.(message.code ?? 0);
    if (reconnectOnExit) {
      terminal.write("\r\n\x1b[90m[Process exited — reconnecting...]\x1b[0m\r\n");
      return "restart";
    }
    terminal.write("\r\n\x1b[90m[Process exited]\x1b[0m\r\n");
    return "stop";
  }

  function createTerminalSession(): TerminalSessionController {
    return makeTerminalSessionController({
      initialStatus,
      url: () => {
        if (disposed || !terminal) return undefined;
        mouseDragAutoscroll?.reset();
        const replayBoundary = terminalSupportsReplayBoundary();
        sentResizeActive = null;
        resizeReady = !replayBoundary;
        const claimedSize = resizeAuthorityRegionSize();
        sentResizeActive = claimedSize !== null;
        const geometry = claimedSize
          ? terminalGeometry(terminal.cols, terminal.rows)
          : null;
        return buildWsUrl(replayBoundary ? null : geometry, replayBoundary) ?? undefined;
      },
      onOpen: () => {
        connectionGeneration += 1;
        onConnectionChange?.(true);
        switchTimer.record("socket-open");
        const authorityChanged = sendResizeActive(resizeAuthorityRegionSize() !== null);
        if (authorityChanged) claimTerminalResize(true);
        const size = resizeAuthorityRegionSize();
        if (resizeReady && size && (size.cols !== sentCols || size.rows !== sentRows)) {
          scheduleTerminalRefresh();
        }
      },
      onMessage: handleTerminalMessage,
      onDisconnected: () => {
        onConnectionChange?.(false);
        cancelPendingTerminalSequence();
        clipboardWriter?.cancelAuthorization();
        mouseDragAutoscroll?.reset();
      },
    });
  }

  function cleanup(): void {
    disposed = true;
    pointerOrigin = null;
    itemLinkExecution?.interrupt();
    itemLinkExecution = null;
    clipboardWriter?.dispose();
    clipboardWriter = undefined;
    mouseDragAutoscroll?.dispose();
    mouseDragAutoscroll = undefined;
    terminalSession = null;
    requestTerminalRefresh = () => {};
    requestTerminalResize = () => {};
    requestFirstPaint = () => {};
    publishClipboardWrite = () => {};
    containerEl?.removeEventListener("paste", handleTerminalPaste, true);
    containerEl?.removeEventListener(XTERM_GESTURE_CHANGE_EVENT, handleTerminalGestureChange, true);
    lastTouchGesturePoint = null;
    if (terminal) {
      unregisterTextureAtlasParticipant?.();
      unregisterTextureAtlasParticipant = null;
      imageAddon?.dispose();
      imageAddon = null;
      ligaturesAddon?.dispose();
      ligaturesAddon = null;
      webglAddon?.dispose();
      webglAddon = null;
      terminal.dispose();
      terminal = null;
    }
  }

  $effect(() => {
    if (!terminal) return;
    if (
      terminalFontFamily === appliedTerminalFontFamily &&
      terminalFontSize === appliedFontSize &&
      terminalScrollback === appliedScrollback &&
      terminalLineHeight === appliedLineHeight &&
      terminalLetterSpacing === appliedLetterSpacing &&
      terminalCursorBlink === appliedCursorBlink &&
      terminalFontLigatures === appliedFontLigatures
    ) return;
    const ligaturesChanged = terminalFontLigatures !== appliedFontLigatures;
    appliedTerminalFontFamily = terminalFontFamily;
    appliedFontSize = terminalFontSize;
    appliedScrollback = terminalScrollback;
    appliedLineHeight = terminalLineHeight;
    appliedLetterSpacing = terminalLetterSpacing;
    appliedCursorBlink = terminalCursorBlink;
    appliedFontLigatures = terminalFontLigatures;
    terminal.options.fontFamily = terminalFontFamily;
    terminal.options.fontSize = terminalFontSize;
    terminal.options.scrollback = terminalScrollback;
    terminal.options.lineHeight = terminalLineHeight;
    terminal.options.letterSpacing = terminalLetterSpacing;
    terminal.options.cursorBlink = terminalCursorBlink;
    terminal.options.disableStdin = disabled;
    if (ligaturesChanged) {
      syncLigaturesAddon();
    }
    redrawTerminalTextureAtlas();
    fitAddon?.fit();
    if (active) scheduleTerminalResize();
  });

  $effect(syncRendererState);

  $effect(() => {
    syncImageAddon(terminalGraphics);
  });

  $effect(() => {
    if (!terminal) return;
    terminal.options.disableStdin = disabled;
  });

  $effect(() => {
    if (!terminal) return;
    // Authority requires painted state plus geometry, never focus: every
    // painted split leaf stays eligible, while hidden and parked terminals do
    // not dictate a size for the pane the user is actually looking at.
    sendResizeActive(resizeAuthorityRegionSize() !== null);
    if (active) scheduleTerminalResize();
  });

  const FONT_LOADED: "loaded" = "loaded";
  const FONT_FAILED: "failed" = "failed";
  const FONT_TIMED_OUT: "timed-out" = "timed-out";

  function openTerminalPane(node: HTMLElement) {
    return Effect.gen(function* () {
      containerEl = node;
      yield* Effect.addFinalizer(() => Effect.sync(cleanup));
      clipboardWriter = yield* makeTerminalClipboardWriter(
        createBrowserTerminalClipboardPort(),
        { onPointerGestureTimeout: cancelTerminalPointerGesture },
      );
      mouseDragAutoscroll = yield* makeTmuxMouseDragAutoscroll({
        send(data) {
          if (disabled || !terminalSession?.isConnected()) return;
          terminalSession.send(encoder.encode(data));
        },
      });
      const refreshScheduler = yield* makeAnimationFrameScheduler(Effect.sync(refreshVisibleTerminal));
      const resizeScheduler = yield* makeAnimationFrameScheduler(Effect.sync(resizeVisibleTerminal));
      const firstPaintScheduler = yield* makeAnimationFrameScheduler(
        nextAnimationFrame.pipe(
          Effect.andThen(Effect.sync(() => switchTimer.record("first-paint"))),
          Effect.asVoid,
        ),
      );
      requestTerminalRefresh = refreshScheduler.schedule;
      requestTerminalResize = resizeScheduler.schedule;
      requestFirstPaint = firstPaintScheduler.schedule;

      const writer = clipboardWriter;
      const clipboardWrites = yield* Queue.bounded<string>(16);
      yield* Effect.addFinalizer(() => Queue.shutdown(clipboardWrites));
      publishClipboardWrite = (text) => {
        if (!Queue.offerUnsafe(clipboardWrites, text)) reportClipboardFailure();
      };
      yield* Effect.forkScoped(
        Effect.forever(
          Queue.take(clipboardWrites).pipe(
            Effect.flatMap((text) =>
              Effect.tryPromise({
                try: () => writer.write(text),
                catch: (cause) => cause,
              }).pipe(
                Effect.match({
                  onFailure: () => reportClipboardFailure(),
                  onSuccess: (outcome) => {
                    if (outcome === "blocked") reportClipboardFailure();
                  },
                }),
              ),
            ),
          ),
        ),
      );

      function startTerminal(): void {
      const term = new Terminal({
        theme: {
          background: "#0d1117",
          foreground: "#c9d1d9",
          cursor: "#58a6ff",
        },
        // The ligatures addon registers a character joiner, which xterm
        // exposes as proposed API. This is constructor-only and must be on
        // before a user enables ligatures at runtime.
        allowProposedApi: true,
        allowTransparency: false,
        cursorBlink: terminalCursorBlink,
        drawBoldTextInBrightColors: true,
        fontFamily: terminalFontFamily,
        fontSize: terminalFontSize,
        scrollback: terminalScrollback,
        letterSpacing: terminalLetterSpacing,
        lineHeight: terminalLineHeight,
        linkHandler: terminalLinkHandler,
        minimumContrastRatio: TERMINAL_MINIMUM_CONTRAST_RATIO,
        rescaleOverlappingGlyphs: true,
        scrollOnEraseInDisplay: true,
        smoothScrollDuration: TERMINAL_SMOOTH_SCROLL_DURATION,
        vtExtensions: {
          kittyKeyboard: true,
        },
        windowOptions: {
          getCellSizePixels: true,
          getWinSizeChars: true,
          getWinSizePixels: true,
        },
        disableStdin: disabled,
      });
      terminal = term;
      unregisterTextureAtlasParticipant =
        registerTerminalTextureAtlasParticipant(terminal);

      term.open(containerEl);
      term.attachCustomKeyEventHandler((event) => !isBrowserPasteShortcut(event));
      term.parser.registerOscHandler(52, handleOsc52Clipboard);
      switchTimer.record("terminal-constructed");
      containerEl.addEventListener("paste", handleTerminalPaste, true);
      containerEl.addEventListener(XTERM_GESTURE_CHANGE_EVENT, handleTerminalGestureChange, true);

      const fit = new FitAddon();
      fitAddon = fit;
      term.loadAddon(fit);
      syncImageAddon(terminalGraphics);
      term.loadAddon(
        new WebLinksAddon(openTerminalLink, {
          hover: showTerminalLink,
          leave: hideTerminalLink,
        }),
      );

      if (terminalFontLigatures) {
        ligaturesAddon = new LigaturesAddon();
        term.loadAddon(ligaturesAddon);
      }
      rendererParked = !renderingEnabled;
      recreateWebglAddon();

      appliedTerminalFontFamily = terminalFontFamily;
      appliedFontSize = terminalFontSize;
      appliedScrollback = terminalScrollback;
      appliedLineHeight = terminalLineHeight;
      appliedLetterSpacing = terminalLetterSpacing;
      appliedCursorBlink = terminalCursorBlink;
      appliedFontLigatures = terminalFontLigatures;
      fit.fit();

      // Renderer autofocus runs only at terminal creation. Explicit pool/host
      // requests use focus() above, but reveal- or enable-driven re-runs of the
      // active effect must not steal focus from controls. The font-load wait is
      // async, so creation autofocus only moves focus when the mount-time context
      // is still current and isn't a dialog/menu/input.
      const focusExplicitly = explicitFocusRequested;
      explicitFocusRequested = false;
      if (active && !disabled && (focusExplicitly || (autoFocus && focusIntent.shouldFocus()))) term.focus();

      term.onData((data: string) => {
        if (disabled) return;
        if (terminalSession?.isConnected()) {
          terminalSession.send(encoder.encode(data));
          mouseDragAutoscroll?.observeTerminalData(data);
        }
      });

      term.onBinary((data: string) => {
        if (disabled) return;
        if (terminalSession?.isConnected()) {
          const buf = new Uint8Array(data.length);
          for (let i = 0; i < data.length; i++) {
            buf[i] = data.charCodeAt(i) & 0xff;
          }
          terminalSession.send(buf);
        }
      });
      }

      const startOwnedTerminal = Effect.gen(function* () {
        yield* Effect.sync(startTerminal);
        yield* observeResize(containerEl, () => scheduleTerminalResize());
        if (!isAttachableInitialStatus(initialStatus)) {
          terminal?.write(
            `\r\n\x1b[90m[${initialStatusMessage(initialStatus)}]\x1b[0m\r\n`,
          );
          return;
        }
        const session = createTerminalSession();
        terminalSession = session;
        yield* Effect.forkScoped(session.program);
      });

      const fontSet = document.fonts;
      if (typeof fontSet?.load !== "function") {
        switchTimer.record("fonts-ready", { unsupported: true });
        yield* startOwnedTerminal;
        return yield* Effect.never;
      }

      // Loading only the selected terminal face avoids making xterm wait
      // for unrelated page fonts. The bound keeps terminal construction
      // moving when the face is slow or unavailable; a late completion
      // then repairs the fallback metrics once, provided the pane lives.
      const fontDescriptor = `${terminalFontSize}px ${primaryTerminalFontFamily(terminalFontFamily)}`;
      const fontCompletion = yield* Deferred.make<"loaded" | "failed">();
      yield* Effect.forkScoped(
        Effect.tryPromise({
          try: () => fontSet.load(fontDescriptor, TERMINAL_FONT_LOAD_GLYPHS),
          catch: (cause) => cause,
        }).pipe(
          Effect.match({
            onFailure: () => FONT_FAILED,
            onSuccess: () => FONT_LOADED,
          }),
          Effect.flatMap((result) => Deferred.succeed(fontCompletion, result)),
        ),
      );
      const fontResult = yield* Effect.race(
        Deferred.await(fontCompletion),
        Effect.sleep(`${TERMINAL_FONT_WAIT_MS} millis`).pipe(Effect.as(FONT_TIMED_OUT)),
      );
      switchTimer.record(
        "fonts-ready",
        fontResult === FONT_FAILED
          ? { error: true }
          : fontResult === FONT_TIMED_OUT
            ? { timedOut: true }
            : undefined,
      );
      yield* startOwnedTerminal;

      if (fontResult === FONT_TIMED_OUT && (yield* Deferred.await(fontCompletion)) === FONT_LOADED) {
        yield* Effect.sync(() => {
          if (disposed || !terminal) return;
          clearSharedTerminalTextureAtlas(terminal);
          // New metrics mean a new measurement; resizeVisibleTerminal re-fits,
          // repaints, and pushes the size only if the region now works out
          // differently.
          resizeVisibleTerminal();
        });
      }
      return yield* Effect.never;
    });
  }

  const attachTerminalPane = terminalAttachment(runtime, {
    open: openTerminalPane,
    onFailure: () => {
      if (!disposed) showFlash("Could not connect to the terminal session.", { tone: "danger" });
    },
  });
</script>

<svelte:window
  onblur={handleWindowBlur}
  onpointermove={handleWindowPointerMove}
  onpointerup={handleTerminalPointerEnd}
  onpointercancel={handleTerminalPointerCancel}
/>
<svelte:document
  onpointerdowncapture={handleDocumentPointerDown}
  onvisibilitychange={handleDocumentVisibilityChange}
/>

<div
  class="terminal-container"
  bind:this={containerEl}
  {@attach attachTerminalPane}
  onpointerdowncapture={handleTerminalPointerDown}
  onlostpointercapture={handleTerminalLostPointerCapture}
  onkeydowncapture={handleTerminalKeyDown}
  oncompositionstartcapture={handleTerminalCompositionStart}
  onmousedowncapture={handleInsecureTerminalRightMouse}
  onmouseupcapture={handleInsecureTerminalRightMouse}
  onwheelcapture={handleTerminalWheel}
  onfocusout={handleTerminalFocusOut}
>
  {#if hoveredTerminalLink}
    <div class="terminal-link-tooltip">
      <span>{hoveredTerminalLink}</span>
      <small>{terminalLinkModifierLabel}+Click to {hoveredTerminalLinkIsItem ? "open in kenn-forge" : "open link"}</small>
    </div>
    {/if}
</div>

<style>
  .terminal-container {
    position: relative;
    width: 100%;
    height: 100%;
    background: var(--terminal-bg);
  }

  .terminal-link-tooltip {
    position: absolute;
    z-index: 5;
    bottom: var(--space-4);
    left: var(--space-4);
    display: flex;
    max-width: calc(100% - (2 * var(--space-4)));
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    overflow: hidden;
    color: var(--text-primary);
    font-family: var(--font-sans);
    font-size: var(--font-size-sm);
    line-height: 1.25;
    pointer-events: none;
    background: var(--bg-surface);
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-md);
  }

  .terminal-link-tooltip span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .terminal-link-tooltip small {
    color: var(--text-muted);
    font-size: inherit;
  }

  .terminal-container :global(.xterm),
  .terminal-container :global(.xterm-viewport),
  .terminal-container :global(.xterm-screen) {
    background: var(--terminal-bg);
  }
</style>
