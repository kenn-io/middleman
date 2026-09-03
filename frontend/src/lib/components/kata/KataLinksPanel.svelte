<script lang="ts">
  import { Button, Chip, EmptyState, Spinner } from "@kenn-io/kit-ui";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import UnlinkIcon from "@lucide/svelte/icons/unlink";
  import { onDestroy, untrack } from "svelte";

  import * as runtimeClient from "../../api/generated/index.js";
  import type { GeneratedClient } from "../../api/generated-api.js";
  import { getNavigate } from "../../context.js";
  import {
    createKataLinksStore,
    deleteKataLink,
    type KataEffectiveLink,
    type KataLinksSubject,
  } from "../../stores/kata-links.svelte.js";
  import {
    beginKataWorkspaceCreate,
    endKataWorkspaceCreate,
    isKataWorkspaceCreatePending,
    recordKataWorkspaceCreated,
    resolveKataWorkspaceRef,
    type KataWorkspaceIdentity,
  } from "../../stores/kata-workspace-create.svelte.js";
  import { isSafeExternalHTTPURL } from "../../utils/safe-external-url.js";
  import KataIssueDetailView from "./KataIssueDetailView.svelte";
  import KataLinkDialog from "./KataLinkDialog.svelte";

  interface Props {
    subject: KataLinksSubject;
    active: boolean;
    disabled?: boolean;
    apiClient?: GeneratedClient;
  }

  let { subject, active, disabled = false, apiClient = runtimeClient }: Props = $props();

  const navigate = getNavigate();
  const store = createKataLinksStore({
    client: untrack(() => apiClient),
    subject: untrack(() => subject),
    visible: typeof document === "undefined" || document.visibilityState === "visible",
  });
  let loadedSubjectKey = "";
  let loadedAssociationSubjectKey = "";
  let linkDialogOpen = $state(false);
  let actionPending = $state<"open" | "workspace" | "unlink" | null>(null);
  let actionError = $state<string | null>(null);
  let actionGeneration = 0;
  let loadedSelectionKey = "";
  let componentDestroyed = false;

  onDestroy(() => {
    componentDestroyed = true;
  });

  const selected = $derived(store.selected());
  const detailEnvelope = $derived(store.detail());
  const selectedWorkspaceIdentity = $derived<KataWorkspaceIdentity | null>(
    selected ? { daemonID: selected.daemon_id, issueUID: selected.issue_uid } : null,
  );
  const effectiveWorkspace = $derived(
    selectedWorkspaceIdentity
      ? resolveKataWorkspaceRef(selectedWorkspaceIdentity, selected?.workspace?.existing_workspace)
      : null,
  );
  const workspaceCreatePending = $derived(
    selectedWorkspaceIdentity ? isKataWorkspaceCreatePending(selectedWorkspaceIdentity) : false,
  );

  function identityKey(value: KataLinksSubject): string {
    if (value.kind === "workspace") return `workspace:${value.workspaceID}`;
    return [
      value.kind,
      value.provider,
      value.platformHost ?? "",
      value.owner,
      value.name,
      String(value.number),
    ].join(":");
  }

  function referenceFor(link: KataEffectiveLink): string {
    return link.reference || link.issue_uid;
  }

  function selectionKey(link: KataEffectiveLink | null): string {
    return link ? `${link.daemon_id}:${link.issue_uid}` : "";
  }

  function loadActiveSubjectOnce(): void {
    const currentSubjectKey = identityKey(subject);
    if (!active || currentSubjectKey !== loadedSubjectKey || currentSubjectKey === loadedAssociationSubjectKey) {
      return;
    }
    loadedAssociationSubjectKey = currentSubjectKey;
    void store.loadLinks();
  }

  function actionIsCurrent(generation: number, requestSubjectKey: string, requestSelectionKey: string): boolean {
    return (
      !componentDestroyed &&
      generation === actionGeneration &&
      requestSubjectKey === identityKey(subject) &&
      requestSelectionKey === selectionKey(selected)
    );
  }

  function reserveKataPopup(): Window | null {
    const popup = window.open("about:blank", "_blank");
    if (!popup) return null;
    popup.opener = null;
    const popupDocument = popup.document;
    if (popupDocument?.head) {
      const referrerPolicy = popupDocument.createElement("meta");
      referrerPolicy.name = "referrer";
      referrerPolicy.content = "no-referrer";
      popupDocument.head.append(referrerPolicy);
    }
    return popup;
  }

  function navigateKataPopup(popup: Window, url: string): void {
    const popupDocument = popup.document;
    if (!popupDocument?.body) {
      popup.location.replace(url);
      return;
    }
    const link = popupDocument.createElement("a");
    link.href = url;
    link.referrerPolicy = "no-referrer";
    link.rel = "noreferrer";
    popupDocument.body.append(link);
    link.click();
  }

  function provenanceLabel(link: KataEffectiveLink): string {
    return (link.provenance ?? [])
      .map((value) => `${value.charAt(0).toUpperCase()}${value.slice(1)}`)
      .join(", ");
  }

  async function unlink(link: KataEffectiveLink): Promise<void> {
    if (link.direct_link_id === undefined || disabled || actionPending !== null) return;
    const requestSubjectKey = identityKey(subject);
    const requestSelectionKey = selectionKey(selected);
    const requestGeneration = ++actionGeneration;
    actionPending = "unlink";
    actionError = null;
    try {
      await deleteKataLink(apiClient, subject, link.direct_link_id);
      if (actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) {
        await store.loadLinks();
      }
    } catch (cause) {
      if (actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) {
        actionError = cause instanceof Error ? cause.message : "Unable to unlink Kata issue.";
      }
    } finally {
      if (actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) actionPending = null;
    }
  }

  async function openInKata(): Promise<void> {
    if (!selected || actionPending !== null) return;
    const popup = reserveKataPopup();
    if (!popup) {
      actionError = "Allow popups to open this issue in Kata.";
      return;
    }
    const requestSubjectKey = identityKey(subject);
    const requestSelectionKey = selectionKey(selected);
    const requestGeneration = ++actionGeneration;
    const requestDaemonID = selected.daemon_id;
    const requestIssueUID = selected.issue_uid;
    actionPending = "open";
    actionError = null;
    try {
      const result = await apiClient.KataService.getKataLaunchTarget({ daemonId: requestDaemonID, issueUid: requestIssueUID });
      if (!actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) {
        popup.close();
        return;
      }
      if (!result.available || !result.url) {
        popup.close();
        actionError = result.reason || "Kata issue cannot be opened.";
        return;
      }
      if (!isSafeExternalHTTPURL(result.url)) {
        popup.close();
        actionError = "Kata must provide a safe HTTP or HTTPS URL.";
        return;
      }
      navigateKataPopup(popup, result.url);
    } catch (cause) {
      popup.close();
      if (actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) {
        actionError = cause instanceof Error ? cause.message : "Kata issue cannot be opened.";
      }
    } finally {
      if (actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey)) actionPending = null;
    }
  }

  async function openOrCreateWorkspace(): Promise<void> {
    if (!selected || disabled || actionPending !== null) return;
    const existingID = effectiveWorkspace?.id;
    if (existingID) {
      navigate(`/terminal/${encodeURIComponent(existingID)}`);
      return;
    }
    if (!selected.workspace?.available) return;

    const requestIdentity = selectedWorkspaceIdentity;
    if (!requestIdentity || !beginKataWorkspaceCreate(requestIdentity)) return;
    const requestSubjectKey = identityKey(subject);
    const requestGeneration = ++actionGeneration;
    const requestDaemonID = selected.daemon_id;
    const requestIssueUID = selected.issue_uid;
    const requestSelectionKey = selectionKey(selected);
    const responseIsCurrent = () => actionIsCurrent(requestGeneration, requestSubjectKey, requestSelectionKey);

    actionPending = "workspace";
    actionError = null;
    try {
      const result = await apiClient.KataService.createKataWorkspace({
          daemon_id: selected.daemon_id,
          issue_uid: selected.issue_uid,
          project_uid: selected.project_uid,
          ...(selected.project_name ? { project_name: selected.project_name } : {}),
          ...(selected.reference ? { qualified_id: selected.reference } : {}),
          ...(selected.title ? { title: selected.title } : {}),
        });
      recordKataWorkspaceCreated(requestIdentity, {
        id: result.id,
        status: result.status ?? "provisioning",
      });
      if (responseIsCurrent()) navigate(`/terminal/${encodeURIComponent(result.id)}`);
    } catch (cause) {
      if (responseIsCurrent()) {
        actionError = cause instanceof Error ? cause.message : "Unable to create Kata workspace.";
      }
    } finally {
      endKataWorkspaceCreate(requestIdentity);
      if (responseIsCurrent()) actionPending = null;
    }
  }

  function handleFocus(): void {
    store.noteFocus();
  }

  function handleVisibilityChange(): void {
    store.noteVisibility(document.visibilityState === "visible");
  }

  $effect(() => {
    const nextKey = identityKey(subject);
    if (nextKey === loadedSubjectKey) return;
    loadedSubjectKey = nextKey;
    actionGeneration += 1;
    store.setSubject(subject);
    actionPending = null;
    actionError = null;
    linkDialogOpen = false;
    loadedAssociationSubjectKey = "";
    loadActiveSubjectOnce();
  });

  $effect(() => {
    const nextKey = selectionKey(selected);
    if (nextKey === loadedSelectionKey) return;
    loadedSelectionKey = nextKey;
    actionGeneration += 1;
    actionPending = null;
    actionError = null;
  });

  $effect(() => {
    loadActiveSubjectOnce();
    store.activate(active);
  });

  $effect(() => () => store.destroy());
</script>

<svelte:window onfocus={handleFocus} />
<svelte:document onvisibilitychange={handleVisibilityChange} />

<section class="kata-links-panel" aria-label="Linked Kata issues">
  <header class="panel-header">
    <div>
      <h3>Kata</h3>
      {#if store.state() === "unavailable"}
        <Chip tone="danger">Unavailable</Chip>
      {:else if store.state() === "partial"}
        <Chip tone="warning">Partial</Chip>
      {/if}
    </div>
    <div class="header-actions">
      <Button
        size="sm"
        surface="soft"
        ariaLabel="Refresh Kata issue"
        title="Refresh Kata issue"
        disabled={store.loading() || disabled}
        onclick={() => void store.loadLinks()}
      >
        <RefreshCwIcon size="14" strokeWidth="2" aria-hidden="true" />
        Refresh
      </Button>
      <Button size="sm" tone="info" disabled={disabled} onclick={() => (linkDialogOpen = true)}>
        Link Kata issue
      </Button>
    </div>
  </header>

  {#if store.diagnostics().length > 0}
    <div class="diagnostics" role="status">
      {#each store.diagnostics() as diagnostic (`${diagnostic.daemon_id}:${diagnostic.reason}`)}
        <span>{diagnostic.daemon_id}: {diagnostic.reason}</span>
      {/each}
    </div>
  {/if}

  {#if store.error()}
    <p class="error" role="alert">{store.error()}</p>
  {/if}
  {#if actionError}
    <p class="error" role="alert">{actionError}</p>
  {/if}

  {#if store.loading() && store.links().length === 0}
    <div class="loading"><Spinner size={14} /> Loading linked issues…</div>
  {:else if store.links().length === 0}
    <EmptyState
      title="No Kata issues linked yet."
      description="Link a task to keep its current detail alongside this Forge item."
    />
  {:else}
    <div class="panel-grid">
      <div class="link-list" aria-label="Linked Kata issue list">
        {#each store.links() as link (`${link.daemon_id}:${link.issue_uid}`)}
          <div class={["link-row", { "link-row--selected": selected?.daemon_id === link.daemon_id && selected?.issue_uid === link.issue_uid }]}>
            <button
              type="button"
              class="link-select"
              onclick={() => void store.select(link.daemon_id, link.issue_uid)}
            >
              <span class="link-title">
                <strong>{referenceFor(link)}</strong>
                {#if link.title}<span>{link.title}</span>{/if}
              </span>
              <span class="link-meta">
                <span>{link.daemon_id}</span>
                {#if provenanceLabel(link)}<span>{provenanceLabel(link)}</span>{/if}
              </span>
              {#if link.unavailable_reason}
                <span class="unavailable">{link.unavailable_reason}</span>
              {/if}
            </button>
            {#if link.direct_link_id !== undefined}
              <Button
                size="sm"
                surface="soft"
                tone="danger"
                ariaLabel={`Unlink ${referenceFor(link)}`}
                title={`Unlink ${referenceFor(link)}`}
                disabled={disabled || actionPending !== null}
                onclick={() => void unlink(link)}
              >
                <UnlinkIcon size="14" strokeWidth="2" aria-hidden="true" />
              </Button>
            {/if}
          </div>
        {/each}
      </div>

      <div class="detail-shell">
        {#if selected}
          <div class="detail-actions">
            <Button
              size="sm"
              surface="soft"
              disabled={actionPending !== null}
              onclick={() => void openInKata()}
            >
              {actionPending === "open" ? "Opening…" : "Open in Kata"}
            </Button>
            {#if effectiveWorkspace || selected.workspace?.available}
              <Button
                size="sm"
                tone="info"
                disabled={disabled || actionPending !== null || workspaceCreatePending}
                onclick={() => void openOrCreateWorkspace()}
              >
                {#if effectiveWorkspace}
                  Open workspace
                {:else if actionPending === "workspace" || workspaceCreatePending}
                  Creating…
                {:else}
                  Create workspace
                {/if}
              </Button>
            {/if}
          </div>

          {#if !effectiveWorkspace && selected.workspace && !selected.workspace.available && selected.workspace.unavailable_reason}
            <p class="detail-message">{selected.workspace.unavailable_reason}</p>
          {/if}

          {#if selected.unavailable_reason}
            <p class="detail-message">{selected.unavailable_reason}</p>
          {:else if store.refreshingDetail() && detailEnvelope === null}
            <div class="loading"><Spinner size={14} /> Loading issue detail…</div>
          {:else if detailEnvelope === null}
            <p class="detail-message">Kata issue detail is unavailable.</p>
          {:else}
            {#key detailEnvelope}
              <svelte:boundary>
                <KataIssueDetailView detail={detailEnvelope.detail} />

                {#snippet failed()}
                  <p class="detail-message">Kata issue detail is unavailable.</p>
                {/snippet}
              </svelte:boundary>
            {/key}
          {/if}
        {/if}
      </div>
    </div>
  {/if}
</section>

{#if linkDialogOpen}
  <KataLinkDialog
    {subject}
    {apiClient}
    onlinked={async () => store.loadLinks()}
    onclose={() => (linkDialogOpen = false)}
  />
{/if}

<style>
  .kata-links-panel {
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
    height: 100%;
    color: var(--text-primary);
  }

  .panel-header,
  .panel-header > div,
  .header-actions,
  .detail-actions,
  .link-meta,
  .loading {
    display: flex;
    align-items: center;
  }

  .panel-header {
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-4);
    border-bottom: 1px solid var(--border-muted);
  }

  .panel-header > div,
  .header-actions,
  .detail-actions,
  .loading {
    gap: var(--space-3);
  }

  h3,
  p {
    margin: 0;
  }

  h3 {
    font-size: var(--font-size-md);
  }

  .diagnostics,
  .error {
    padding: var(--space-3) var(--space-4);
  }

  .diagnostics {
    display: grid;
    gap: var(--space-1);
    background: var(--surface-warning-subtle);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .error,
  .unavailable {
    color: var(--accent-red);
  }

  .panel-grid {
    display: grid;
    grid-template-columns: minmax(180px, 30%) minmax(0, 1fr);
    min-height: 0;
    flex: 1 1 auto;
  }

  .link-list {
    overflow-y: auto;
    border-right: 1px solid var(--border-muted);
  }

  .link-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) max-content;
    align-items: center;
    border-bottom: 1px solid var(--border-muted);
  }

  .link-row--selected {
    background: var(--surface-selected);
  }

  .link-select {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    padding: var(--space-4);
    border: 0;
    background: transparent;
    color: inherit;
    text-align: left;
    cursor: pointer;
  }

  .link-title {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
  }

  .link-title span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .link-meta {
    flex-wrap: wrap;
    gap: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .unavailable {
    font-size: var(--font-size-xs);
  }

  .detail-shell {
    min-width: 0;
    overflow-y: auto;
    padding: var(--space-5);
  }

  .detail-actions {
    justify-content: flex-end;
    margin-bottom: var(--space-5);
  }

  .detail-message,
  .loading {
    color: var(--text-muted);
  }

  .loading {
    padding: var(--space-6);
  }

  @media (max-width: 760px) {
    .panel-grid {
      grid-template-columns: 1fr;
    }

    .link-list {
      max-height: 220px;
      border-right: 0;
      border-bottom: 1px solid var(--border-muted);
    }
  }
</style>
