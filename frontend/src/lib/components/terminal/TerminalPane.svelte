<script lang="ts">
  import XtermTerminalPane from "./XtermTerminalPane.svelte";
  import type { TerminalKey } from "./terminal-key.js";

  interface TerminalPaneProps {
    workspaceId?: string | undefined;
    websocketPath?: string | undefined;
    fleetHostKey?: string | undefined;
    reconnectOnExit?: boolean | undefined;
    active?: boolean | undefined;
    renderingEnabled?: boolean | undefined;
    autoFocus?: boolean | undefined;
    cursorWheelInput?: boolean;
    disabled?: boolean;
    onExit?: ((code: number) => void) | undefined;
    onConnectionChange?: ((connected: boolean) => void) | undefined;
    // When the session is already exited at mount time, skip the
    // WebSocket connect — the server's attach endpoint returns 404
    // for non-running sessions, which would loop scheduleReconnect.
    initialStatus?: string | undefined;
  }

  let {
    workspaceId = undefined,
    websocketPath = undefined,
    fleetHostKey = undefined,
    reconnectOnExit = undefined,
    active = undefined,
    renderingEnabled = undefined,
    autoFocus = undefined,
    cursorWheelInput = false,
    disabled = false,
    onExit = undefined,
    onConnectionChange = undefined,
    initialStatus = undefined,
  }: TerminalPaneProps = $props();

  let xtermPane = $state<XtermTerminalPane | null>(null);

  export function focus(): void {
    xtermPane?.focus();
  }

  export function sendInput(data: string): boolean {
    return xtermPane?.sendInput(data) ?? false;
  }

  export function sendPastedInput(data: string, suffix = ""): boolean {
    return xtermPane?.sendPastedInput(data, suffix) ?? false;
  }

  export function sendKey(key: TerminalKey): boolean {
    return xtermPane?.sendKey(key) ?? false;
  }
</script>

<XtermTerminalPane
  bind:this={xtermPane}
  {workspaceId}
  {websocketPath}
  {fleetHostKey}
  {reconnectOnExit}
  {active}
  {renderingEnabled}
  {autoFocus}
  {cursorWheelInput}
  {disabled}
  {onExit}
  {onConnectionChange}
  {initialStatus}
/>
