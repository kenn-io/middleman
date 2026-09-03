<script lang="ts">
  import { untrack, type ComponentProps } from "svelte";
  import type { AppRuntime } from "../../app/runtime.js";
  import { setAppRuntime } from "../../app/runtime-context.js";
  import XtermTerminalPane from "./XtermTerminalPane.svelte";
  import type { TerminalKey } from "./terminal-key.js";

  type Props = ComponentProps<typeof XtermTerminalPane> & { runtime: AppRuntime };

  const { runtime, ...props }: Props = $props();
  setAppRuntime(untrack(() => runtime));
  let pane = $state<XtermTerminalPane | null>(null);

  export function sendKey(key: TerminalKey): boolean {
    return pane?.sendKey(key) ?? false;
  }
</script>

<XtermTerminalPane bind:this={pane} {...props} />
