<script lang="ts">
  import { tick } from "svelte";
  import type { Attachment } from "svelte/attachments";
  import type { WorkflowDefinitionResponse } from "../../api/generated/models/workflowDefinitionResponse.js";
  import type { WorkflowEnvironmentResponse } from "../../api/generated/models/workflowEnvironmentResponse.js";
  import type { OperationAvailability } from "../../api/types.js";
  import DialogButton from "../shared/DialogButton.svelte";
  import Modal from "../shared/Modal.svelte";
  import WorkflowDispatchForm, { type WorkflowDispatchRequest } from "./WorkflowDispatchForm.svelte";
  import type { WorkflowDispatchPresentationState } from "./workflow-dispatch-presentation.js";

  type Workflow = WorkflowDefinitionResponse;
  type Environment = WorkflowEnvironmentResponse;

  interface Props {
    open: boolean;
    workflow: Workflow;
    environments: readonly Environment[];
    initialRef: string;
    operation: OperationAvailability | undefined;
    state: WorkflowDispatchPresentationState;
    trigger?: HTMLElement | null;
    onsubmit: (request: WorkflowDispatchRequest) => void;
    onclose: () => void;
    onreload: () => void;
    onnewcycle: () => void;
  }

  let { open, workflow, environments, initialRef, operation, state: presentation, trigger = null, onsubmit, onclose, onreload, onnewcycle }: Props = $props();
  let reloadRequested = $state(false);
  const canDismiss = $derived(
    presentation.kind === "idle"
      || presentation.kind === "succeeded"
      || presentation.kind === "failed"
      || (presentation.kind === "conflict" && presentation.reloadError !== undefined),
  );

  async function close(): Promise<void> {
    if (!canDismiss) return;
    if (presentation.kind === "succeeded" || presentation.kind === "failed") onnewcycle();
    onclose();
    await tick();
    trigger?.focus();
  }

  function beginNewCycle(): void {
    if (
      presentation.kind !== "succeeded"
      && presentation.kind !== "failed"
      && presentation.kind !== "uncertain"
    ) return;
    onnewcycle();
  }

  function reload(): void {
    if (reloadRequested || presentation.kind !== "conflict") return;
    reloadRequested = true;
    onreload();
  }

  const resetReloadOnUnmount: Attachment = () => () => {
    reloadRequested = false;
  };

  function observeReloadKind(kind: WorkflowDispatchPresentationState["kind"]): Attachment {
    return () => {
      if (kind !== "conflict") reloadRequested = false;
    };
  }
</script>

<Modal {open} title="Run workflow" width={520} frameId="workflow-dispatch" onClose={() => { void close(); }}>
  <div class="dispatch-dialog-body" {@attach resetReloadOnUnmount} {@attach observeReloadKind(presentation.kind)}>
    <WorkflowDispatchForm {workflow} {environments} {initialRef} {operation} state={presentation} {onsubmit} />
  </div>
  {#snippet footer()}
    {#if presentation.kind === "idle"}<DialogButton onclick={() => { void close(); }}>Cancel</DialogButton>{/if}
    {#if presentation.kind === "conflict"}
      {#if presentation.reloadError}<DialogButton onclick={() => { void close(); }}>Close</DialogButton>{/if}
      <DialogButton tone="primary" disabled={reloadRequested} onclick={reload}>Reload workflows</DialogButton>
    {/if}
    {#if presentation.kind === "succeeded" || presentation.kind === "failed"}
      <DialogButton onclick={() => { void close(); }}>Close</DialogButton>
      <DialogButton tone="primary" onclick={beginNewCycle}>Run again</DialogButton>
    {/if}
    {#if presentation.kind === "uncertain"}
      <DialogButton tone="primary" onclick={beginNewCycle}>Dispatch again</DialogButton>
    {/if}
  {/snippet}
</Modal>

<style>
  .dispatch-dialog-body { display: contents; }
</style>
