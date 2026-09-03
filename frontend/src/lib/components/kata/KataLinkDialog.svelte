<script lang="ts">
  import { Button, Modal, SearchInput, SelectDropdown } from "@kenn-io/kit-ui";
  import { untrack } from "svelte";

  import type { KataDaemonResponse, KataIssueReference } from "../../api/generated/models/index.js";
  import * as runtimeClient from "../../api/generated/index.js";
  import type { GeneratedClient } from "../../api/generated-api.js";
  import {
    createKataLink,
    type KataEffectiveLinksResponse,
    type KataLinksSubject,
  } from "../../stores/kata-links.svelte.js";
  import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";

  type KataDaemon = KataDaemonResponse;
  type KataReference = KataIssueReference;

  interface Props {
    subject: KataLinksSubject;
    onlinked: (response: KataEffectiveLinksResponse) => void | Promise<void>;
    onclose: () => void;
    apiClient?: GeneratedClient;
  }

  let { subject, onlinked, onclose, apiClient = runtimeClient }: Props = $props();

  let daemons = $state.raw<KataDaemon[]>([]);
  let selectedDaemonID = $state("");
  let query = $state("");
  let references = $state.raw<KataReference[]>([]);
  let selectedReference = $state.raw<KataReference | null>(null);
  let rosterLoading = $state(true);
  let searching = $state(false);
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let searchTimer: ReturnType<typeof setTimeout> | null = null;
  let searchController: AbortController | null = null;
  let searchGeneration = 0;

  const selectedDaemon = $derived(
    daemons.find((daemon) => daemon.id === selectedDaemonID) ?? null,
  );
  const daemonOptions = $derived(
    daemons.map((daemon) => {
      const reason = daemon.health === "connected" ? "" : daemon.hint || `Health: ${daemon.health}`;
      return {
        value: daemon.id,
        label: daemon.id,
        disabled: reason !== "",
        ...(reason === ""
          ? { indicator: { tone: "success" as const, title: "Connected" } }
          : { indicator: { tone: "danger" as const, title: reason } }),
      };
    }),
  );
  const selectedDaemonUsable = $derived(selectedDaemon?.health === "connected");
  const selectedDaemonProblem = $derived.by(() => {
    if (!selectedDaemon || selectedDaemonUsable) return null;
    if (selectedDaemon.hint) return selectedDaemon.hint;
    return `Kata daemon health: ${selectedDaemon.health}.`;
  });

  function cancelScheduledSearch(): void {
    if (searchTimer !== null) {
      clearTimeout(searchTimer);
      searchTimer = null;
    }
    searchController?.abort();
    searchController = null;
  }

  function scheduleSearch(): void {
    cancelScheduledSearch();
    searchGeneration += 1;
    references = [];
    selectedReference = null;
    error = null;
    if (query.trim() === "" || !selectedDaemonUsable) {
      searching = false;
      return;
    }
    const generation = searchGeneration;
    searchTimer = setTimeout(() => {
      searchTimer = null;
      void runSearch(generation);
    }, 250);
  }

  async function runSearch(generation: number): Promise<void> {
    const daemonID = selectedDaemonID;
    const text = query.trim();
    if (daemonID === "" || text === "" || generation !== searchGeneration) return;
    const controller = new AbortController();
    searchController = controller;
    searching = true;
    try {
      const result = await apiClient.KataService.listKataReferences({ daemonId: daemonID }, { q: text, limit: 50 }, { signal: controller.signal });
      if (generation !== searchGeneration || controller.signal.aborted) return;
      references = result.issues;
    } catch (cause) {
      if (controller.signal.aborted || generation !== searchGeneration) return;
      error = cause instanceof Error ? cause.message : "Unable to search Kata issues.";
    } finally {
      if (generation === searchGeneration) searching = false;
      if (searchController === controller) searchController = null;
    }
  }

  function chooseDaemon(daemonID: string): void {
    selectedDaemonID = daemonID;
    scheduleSearch();
  }

  async function submit(): Promise<void> {
    if (!selectedReference || !selectedDaemonUsable || submitting) return;
    submitting = true;
    error = null;
    try {
      const result = await createKataLink(apiClient, subject, {
        daemon_id: selectedDaemonID,
        issue_uid: selectedReference.uid,
        project_uid: selectedReference.project_uid,
      });
      await onlinked(result);
      onclose();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Unable to link Kata issue.";
    } finally {
      submitting = false;
    }
  }

  $effect(() => {
    const controller = new AbortController();
    void (async () => {
      rosterLoading = true;
      error = null;
      try {
        const result = await apiClient.KataService.listKataDaemons({ signal: controller.signal });
        if (controller.signal.aborted) return;
        daemons = result.daemons ?? [];
        selectedDaemonID = daemons.find((daemon) => daemon.default)?.id ?? daemons[0]?.id ?? "";
      } catch (cause) {
        if (!controller.signal.aborted) {
          error = cause instanceof Error ? cause.message : "Unable to load Kata daemons.";
        }
      } finally {
        if (!controller.signal.aborted) rosterLoading = false;
      }
    })();
    return () => controller.abort();
  });

  $effect(() => untrack(() => pushModalFrame("kata-link-dialog", [])));

  $effect(() => () => {
    cancelScheduledSearch();
  });
</script>

{#snippet footer()}
  <Button surface="outline" onclick={onclose}>Cancel</Button>
  <Button
    tone="info"
    disabled={!selectedReference || !selectedDaemonUsable || submitting}
    onclick={() => void submit()}
  >
    {submitting ? "Linking…" : "Link issue"}
  </Button>
{/snippet}

<Modal title="Link Kata issue" {onclose} {footer} width="min(560px, calc(100vw - 32px))">
  <div class="kata-link-dialog">
    <label class="field">
      <span>Kata daemon</span>
      {#if rosterLoading}
        <span class="muted">Loading daemons…</span>
      {:else if daemonOptions.length > 0}
        <SelectDropdown
          value={selectedDaemonID}
          options={daemonOptions}
          onchange={chooseDaemon}
          title="Kata daemon"
        />
      {:else}
        <span class="muted">No Kata daemons configured.</span>
      {/if}
    </label>

    <div class="field">
      <span>Issue</span>
      <SearchInput
        bind:value={query}
        block
        ariaLabel="Search Kata issues"
        placeholder="Search by reference or title"
        disabled={!selectedDaemonUsable}
        oninput={() => scheduleSearch()}
      />
    </div>

    {#if selectedDaemonProblem}
      <p class="error" role="alert">{selectedDaemonProblem}</p>
    {/if}

    {#if error}
      <p class="error" role="alert">{error}</p>
    {/if}

    {#if searching}
      <p class="muted" role="status">Searching…</p>
    {:else if query.trim() !== "" && references.length === 0 && !error}
      <p class="muted">No matching Kata issues.</p>
    {:else if references.length > 0}
      <div class="reference-list" aria-label="Kata issue results">
        {#each references as reference (reference.uid)}
          <button
            type="button"
            class={["reference-row", { "reference-row--selected": selectedReference?.uid === reference.uid }]}
            aria-pressed={selectedReference?.uid === reference.uid}
            onclick={() => (selectedReference = reference)}
          >
            <strong>{reference.qualified_id || reference.short_id}</strong>
            <span>{reference.title}</span>
            <small>{reference.project_name} · {reference.status}</small>
          </button>
        {/each}
      </div>
    {/if}
  </div>
</Modal>

<style>
  .kata-link-dialog,
  .field {
    display: grid;
    gap: var(--space-3);
  }

  .kata-link-dialog {
    gap: var(--space-5);
    min-width: 0;
  }

  .field > span:first-child {
    font-size: var(--font-size-sm);
    font-weight: 600;
    color: var(--text-secondary);
  }

  .reference-list {
    display: grid;
    gap: var(--space-2);
    max-height: 240px;
    overflow-y: auto;
  }

  .reference-row {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--space-1) var(--space-3);
    width: 100%;
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--surface-interactive);
    color: var(--text-primary);
    text-align: left;
    cursor: pointer;
  }

  .reference-row--selected {
    border-color: var(--accent-blue);
    background: var(--surface-selected);
  }

  .reference-row span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .reference-row small {
    grid-column: 1 / -1;
    color: var(--text-muted);
  }

  .muted {
    margin: 0;
    color: var(--text-muted);
  }

  .error {
    margin: 0;
    color: var(--accent-red);
  }
</style>
