<script lang="ts">
  import { StatusDot, type StatusDotStatus } from "@kenn-io/kit-ui";
  import { getShowListAgentStatus } from "../../stores/list-agent-status.svelte.js";

  const { state }: { state?: string | undefined } = $props();

  const agent = $derived.by((): { label: string; status: StatusDotStatus; tone: string } | null => {
    if (!getShowListAgentStatus()) return null;
    switch (state) {
      case "working": return { label: "Working", status: "working", tone: "working" };
      case "approval": return { label: "Approval", status: "waiting", tone: "approval" };
      case "input": return { label: "Input", status: "waiting", tone: "input" };
      case "done": return { label: "Done", status: "idle", tone: "done" };
      default: return null;
    }
  });
</script>

{#if agent}
  <span class={["agent-state", `agent-state--${agent.tone}`]} title={`Agent ${agent.label.toLowerCase()}`}>
    <StatusDot animated status={agent.status} label={`Agent ${agent.label.toLowerCase()}`} size={6} />
    <span>{agent.label}</span>
  </span>
{/if}

<style>
  .agent-state {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
    margin-left: auto;
    font-size: var(--font-size-xs);
    font-weight: 600;
    line-height: 1;
    color: var(--accent-green);
  }
  .agent-state--approval {
    color: var(--accent-amber);
    --status-waiting: var(--accent-amber);
  }
  .agent-state--input {
    color: var(--accent-purple);
    --status-waiting: var(--accent-purple);
  }

</style>
