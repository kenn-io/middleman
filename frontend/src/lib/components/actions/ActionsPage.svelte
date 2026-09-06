<script lang="ts">
  import { ScrollBox } from "@kenn-io/kit-ui";
  import { Effect } from "effect";
  import { onMount, untrack } from "svelte";
  import type { Attachment } from "svelte/attachments";

  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppExecution } from "../../app/runtime.js";
  import type { ProviderRouteRef } from "../../api/provider-routes.js";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { getStores } from "../../context.js";
  import {
    normalizeSummaries,
    repoKey,
    repoStateKey,
    type RepoSummaryCard,
  } from "../repositories/repoSummary.js";
  import {
    RepoSummaryWorkflow,
    type RepoSummaryReadError,
  } from "../repositories/repo-summary-workflow.js";
  import { getGlobalRepo, parseRepoFilterValue } from "../../stores/filter.svelte.js";
  import WorkflowDispatchForm, { type WorkflowDispatchRequest } from "./WorkflowDispatchForm.svelte";
  import {
    workflowActionsErrorMessage,
    workflowDispatchPresentation,
  } from "./workflow-dispatch-presentation.js";
  import WorkflowRunList from "./WorkflowRunList.svelte";

  const runtime = getAppRuntime();
  const { workflowActions } = getStores();

  let summaries = $state.raw<RepoSummaryCard[]>([]);
  let summariesLoading = $state(true);
  let summariesError = $state<string | null>(null);
  let summaryExecution: AppExecution<void, never> | null = null;
  let selectedRepositoryKey = $state<string | null>(null);

  const filteredSummaries = $derived.by(() => {
    const selected = parseRepoFilterValue(getGlobalRepo());
    if (selected.length === 0) return summaries;
    const selectedKeys = new Set(selected);
    return summaries.filter((summary) => selectedKeys.has(repoStateKey(summary)));
  });
  const capableSummaries = $derived(filteredSummaries.filter(supportsWorkflowActions));
  const unsupportedSummaries = $derived(filteredSummaries.filter((summary) => !supportsWorkflowActions(summary)));
  const selectedSummary = $derived(
    capableSummaries.find((summary) => repoStateKey(summary) === selectedRepositoryKey)
      ?? capableSummaries[0]
      ?? null,
  );
  const selectedRef = $derived(selectedSummary ? workflowRef(selectedSummary) : null);
  const snapshot = $derived(selectedRef ? workflowActions.getSnapshot(selectedRef) : null);
  const catalog = $derived(snapshot?.catalog ?? null);
  const selectedWorkflow = $derived(snapshot?.selectedWorkflow ?? null);
  const showRepositoryRail = $derived(capableSummaries.length > 1 || unsupportedSummaries.length > 0);

  function supportsWorkflowActions(summary: RepoSummaryCard): boolean {
    const capabilities = summary.repo.capabilities;
    return capabilities.read_workflows
      && capabilities.read_workflow_runs
      && capabilities.workflow_dispatch;
  }

  function workflowRef(summary: RepoSummaryCard): ProviderRouteRef {
    return {
      provider: summary.repo.provider,
      platformHost: summary.repo.platform_host,
      owner: summary.repo.owner,
      name: summary.repo.name,
      repoPath: summary.repo.repo_path,
    };
  }

  function summaryFailureMessage(failure: RepoSummaryReadError): string {
    if (failure._tag === "ApiProblemError") {
      return apiErrorMessage(failure.problem, "Could not load repositories for Actions.");
    }
    return failure.cause instanceof Error
      ? failure.cause.message
      : "Could not load repositories for Actions.";
  }

  function loadSummaries(): void {
    summaryExecution?.interrupt();
    summariesLoading = true;
    summariesError = null;
    summaryExecution = runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* RepoSummaryWorkflow;
        return normalizeSummaries(yield* workflow.read);
      }).pipe(
        Effect.matchEffect({
          onFailure: (failure) => Effect.sync(() => {
            summariesError = summaryFailureMessage(failure);
            summariesLoading = false;
          }),
          onSuccess: (loaded) => Effect.sync(() => {
            summaries = loaded;
            summariesLoading = false;
          }),
        }),
      ),
      {
        operation: "actions.repositories.read",
        safeContext: { surface: "actions" },
        onFailure: () => undefined,
      },
    );
  }

  function selectRepository(summary: RepoSummaryCard): void {
    selectedRepositoryKey = repoStateKey(summary);
  }

  function selectWorkflow(workflowId: string): void {
    if (!selectedRef) return;
    workflowActions.selectWorkflow(selectedRef, workflowId);
  }

  function submitWorkflow(request: WorkflowDispatchRequest): void {
    if (!selectedRef || !selectedWorkflow) return;
    workflowActions.dispatch({
      ref: selectedRef,
      workflowId: selectedWorkflow.id,
      expectedDefinitionSha: selectedWorkflow.definition_sha,
      dispatchRef: request.ref,
      inputs: request.inputs,
    });
  }

  function reloadWorkflowCatalog(): void {
    if (!selectedRef || !selectedWorkflow) return;
    workflowActions.refreshCatalog(selectedRef, selectedWorkflow.id);
  }

  function newDispatchCycle(): void {
    if (!selectedRef || !selectedWorkflow) return;
    workflowActions.newDispatchCycle(selectedRef, selectedWorkflow.id);
  }

  function expandRun(runId: string): void {
    if (!selectedRef) return;
    workflowActions.loadJobs(selectedRef, runId);
  }

  onMount(() => {
    loadSummaries();
    return () => {
      summaryExecution?.interrupt();
    };
  });

  function loadSelectedCatalog(ref: ProviderRouteRef | null): Attachment {
    return () => {
      if (!ref) return;
      untrack(() => workflowActions.loadCatalog(ref));
    };
  }

</script>

<section
  class="actions-page"
  aria-labelledby="actions-title"
  {@attach loadSelectedCatalog(selectedRef)}
>
  <header class="actions-page__header">
    <div>
      <h1 id="actions-title">Actions</h1>
      <p>Run manual provider workflows and inspect recent runs.</p>
    </div>
    {#if selectedSummary}
      <span class="actions-page__selection" title={selectedSummary.repo.repo_path}>
        {repoKey(selectedSummary)}
      </span>
    {/if}
  </header>

  {#if summariesLoading}
    <div class="actions-page__state" role="status">Loading workflow repositories…</div>
  {:else if summariesError}
    <div class="actions-page__state actions-page__state--error" role="alert">
      <span>{summariesError}</span>
      <button type="button" onclick={loadSummaries}>Retry</button>
    </div>
  {:else if filteredSummaries.length === 0}
    <div class="actions-page__state">No repositories match the current repository filter.</div>
  {:else if capableSummaries.length === 0}
    <div class="actions-page__state">
      <strong>Workflow Actions unavailable</strong>
      <span>No repositories in the current filter support workflow catalogs, runs, and dispatch.</span>
      <ul class="unsupported-list">
        {#each unsupportedSummaries as summary (repoStateKey(summary))}
          <li><strong>{repoKey(summary)}</strong> does not support workflow Actions.</li>
        {/each}
      </ul>
    </div>
  {:else}
    <div class:actions-layout--with-rail={showRepositoryRail} class="actions-layout">
      {#if showRepositoryRail}
        <nav class="repository-rail" aria-label="Actions repositories">
          <ScrollBox label="Actions repositories">
            <div class="repository-list">
              {#each capableSummaries as summary (repoStateKey(summary))}
                {@const selected = summary === selectedSummary}
                <button
                  type="button"
                  class:selected
                  aria-current={selected ? "true" : undefined}
                  onclick={() => selectRepository(summary)}
                >
                  <span>{summary.name}</span>
                  <small>{summary.owner}</small>
                </button>
              {/each}
              {#each unsupportedSummaries as summary (repoStateKey(summary))}
                <div
                  class="repository-unsupported"
                  aria-label={`${repoKey(summary)} does not support workflow Actions`}
                  role="note"
                >
                  <strong>{summary.name}</strong>
                  <span>{summary.owner}</span>
                  <small>does not support workflow Actions</small>
                </div>
              {/each}
            </div>
          </ScrollBox>
        </nav>
      {/if}

      <section class="workflow-catalog" aria-labelledby="workflow-list-title">
        <header class="pane-heading">
          <h2 id="workflow-list-title">Workflows</h2>
          {#if catalog}<span>{catalog.workflows?.length ?? 0}</span>{/if}
        </header>
        <ScrollBox label="Manual workflows">
          {#if snapshot?.loading.catalog || !snapshot}
            <p class="pane-state" role="status">Loading workflows…</p>
          {:else if snapshot.error && !catalog}
            <p class="pane-state pane-state--error" role="alert">Could not load workflows.</p>
          {:else if (catalog?.workflows?.length ?? 0) === 0}
            <p class="pane-state">No manual workflows are available.</p>
          {:else}
            <ul class="workflow-list">
              {#each catalog?.workflows ?? [] as workflow (workflow.id)}
                <li>
                  <button
                    type="button"
                    class:selected={selectedWorkflow?.id === workflow.id}
                    aria-current={selectedWorkflow?.id === workflow.id ? "true" : undefined}
                    onclick={() => selectWorkflow(workflow.id)}
                  >
                    <strong>{workflow.name}</strong>
                    <span>{workflow.path}</span>
                    {#if !workflow.available}<small>{workflow.unavailable_reason || "Unavailable"}</small>{/if}
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        </ScrollBox>
      </section>

      <div class="workflow-workspace">
        <section class="dispatch-pane" aria-labelledby="dispatch-title">
          <header class="pane-heading"><h2 id="dispatch-title">Dispatch</h2></header>
          <ScrollBox label="Workflow dispatch form">
            <div class="pane-content">
              {#if selectedWorkflow && selectedRef}
                <WorkflowDispatchForm
                  workflow={selectedWorkflow}
                  environments={snapshot?.catalog?.environments ?? []}
                  initialRef={snapshot?.catalog?.repo.default_branch ?? ""}
                  operation={snapshot?.catalog?.repo.operations?.dispatch_workflow}
                  state={workflowDispatchPresentation(snapshot, selectedWorkflow.id)}
                  onsubmit={submitWorkflow}
                  onreload={reloadWorkflowCatalog}
                  onnewcycle={newDispatchCycle}
                />
              {:else}
                <p class="pane-state">Select a workflow to configure a manual run.</p>
              {/if}
            </div>
          </ScrollBox>
        </section>

        <section class="runs-pane" aria-labelledby="runs-title">
          <header class="pane-heading">
            <h2 id="runs-title">Recent runs</h2>
            {#if snapshot}<span>{snapshot.runs.length}</span>{/if}
          </header>
          <ScrollBox label="Recent workflow runs">
            {#if snapshot?.error}
              <div class="workflow-data-error" role="alert">
                <strong>Workflow data may be stale.</strong>
                <span>{workflowActionsErrorMessage(snapshot.error, "Workflow data could not be refreshed.")}</span>
              </div>
            {/if}
            {#if snapshot?.loading.runs && snapshot.runs.length === 0}
              <p class="pane-state" role="status">Loading runs…</p>
            {:else if !snapshot?.error && (snapshot?.runs.length ?? 0) === 0}
              <p class="pane-state">No recent workflow runs.</p>
            {:else if selectedRef && snapshot}
              <WorkflowRunList
                runs={snapshot.runs}
                jobs={snapshot.jobs}
                loadingJobs={snapshot.loading.jobs}
                onexpand={expandRun}
              />
              {#if snapshot.runsPage.nextCursor && !snapshot.runsPage.exhausted}
                <div class="runs-pagination">
                  <button
                    type="button"
                    disabled={snapshot.runsPage.loadingMore}
                    onclick={() => workflowActions.loadMoreRuns(selectedRef)}
                  >
                    {snapshot.runsPage.loadingMore ? "Loading more runs…" : "Load more runs"}
                  </button>
                </div>
              {/if}
            {/if}
          </ScrollBox>
        </section>
      </div>
    </div>
  {/if}
</section>

<style>
  .actions-page {
    flex: 1;
    min-height: 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
  }

  .actions-page__header {
    min-height: 54px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-5);
    border-block-end: 1px solid var(--border-default);
    background: var(--bg-surface);
  }

  h1,
  h2,
  p {
    margin: 0;
  }

  h1 {
    font-size: var(--font-size-lg);
    line-height: 1.25;
  }

  .actions-page__header p,
  .actions-page__selection,
  .pane-heading span {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .actions-page__selection {
    max-width: min(40vw, 420px);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-family: var(--font-mono);
  }

  .actions-page__state {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    padding: var(--space-6);
    color: var(--text-secondary);
    text-align: center;
  }

  .actions-page__state button {
    min-height: 30px;
    padding: 0 var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  .actions-page__state--error,
  .pane-state--error {
    color: var(--status-danger-text, var(--text-danger));
  }

  .unsupported-list {
    display: grid;
    gap: var(--space-2);
    padding: 0;
    list-style: none;
    font-size: var(--font-size-sm);
  }

  .actions-layout {
    flex: 1;
    min-height: 0;
    min-width: 0;
    display: grid;
    grid-template-columns: var(--actions-workflow-list-width) minmax(0, 1fr);
  }

  .actions-layout--with-rail {
    grid-template-columns: var(--actions-repository-rail-width) var(--actions-workflow-list-width) minmax(0, 1fr);
  }

  .repository-rail,
  .workflow-catalog,
  .dispatch-pane,
  .runs-pane {
    min-width: 0;
    min-height: 0;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
  }

  .repository-rail,
  .workflow-catalog {
    border-inline-end: 1px solid var(--border-default);
  }

  .repository-list,
  .workflow-list {
    display: grid;
  }

  .workflow-list {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .workflow-list li {
    min-width: 0;
  }

  .repository-list button,
  .repository-unsupported,
  .workflow-list button {
    min-width: 0;
    display: grid;
    gap: var(--space-1);
    padding: var(--space-3) var(--space-4);
    border-block-end: 1px solid var(--border-subtle);
    text-align: start;
  }

  .workflow-list button {
    width: 100%;
  }

  .repository-list button:hover,
  .workflow-list button:hover {
    background: var(--bg-surface-hover);
  }

  .repository-list button.selected,
  .workflow-list button.selected {
    background: var(--bg-row-selected);
    box-shadow: inset var(--chrome-active-accent-width) 0 0 var(--accent-blue);
  }

  .repository-list span,
  .repository-list small,
  .workflow-list span,
  .workflow-list small {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .repository-list small,
  .repository-unsupported,
  .workflow-list span,
  .workflow-list small {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .repository-unsupported {
    background: var(--bg-inset);
  }

  .repository-unsupported small {
    white-space: normal;
  }

  .pane-heading {
    min-height: 37px;
    flex: 0 0 auto;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: 0 var(--space-4);
    border-block-end: 1px solid var(--border-default);
    background: var(--bg-inset);
  }

  .pane-heading h2 {
    font-size: var(--font-size-sm);
    font-weight: 650;
  }

  .workflow-workspace {
    min-height: 0;
    min-width: 0;
    display: grid;
    grid-template-columns: minmax(260px, 0.8fr) minmax(360px, 1.2fr);
  }

  .dispatch-pane {
    border-inline-end: 1px solid var(--border-default);
  }

  .pane-content {
    padding: var(--space-4);
  }

  .pane-state {
    padding: var(--space-5) var(--space-4);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .workflow-data-error {
    display: grid;
    gap: var(--space-1);
    margin: var(--space-3) var(--space-4) 0;
    padding: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent-red) 45%, var(--border-default));
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 8%, var(--bg-surface));
    color: var(--status-danger-text, var(--text-danger));
    font-size: var(--font-size-sm);
  }

  @media (max-width: 900px) {
    .actions-layout {
      grid-template-columns: 180px minmax(0, 1fr);
      grid-template-rows: minmax(0, 1fr);
    }

    .actions-layout--with-rail {
      grid-template-columns: 180px minmax(0, 1fr);
      grid-template-rows: minmax(150px, 0.7fr) minmax(0, 1.3fr);
    }

    .actions-layout--with-rail .repository-rail {
      grid-row: 1 / 3;
    }

    .actions-layout--with-rail .workflow-catalog {
      border-inline-end: 0;
      border-block-end: 1px solid var(--border-default);
    }

    .workflow-workspace {
      grid-template-columns: minmax(0, 1fr);
    }

    .dispatch-pane {
      border-inline-end: 0;
      border-block-end: 1px solid var(--border-default);
    }
  }

  @media (max-width: 640px) {
    .actions-page__header {
      align-items: flex-start;
      padding: var(--space-3) var(--space-4);
    }

    .actions-page__header p {
      display: none;
    }

    .actions-layout,
    .actions-layout--with-rail {
      display: flex;
      flex-direction: column;
      overflow: auto;
    }

    .repository-rail,
    .workflow-catalog,
    .dispatch-pane,
    .runs-pane {
      flex: 0 0 auto;
      min-height: 160px;
      border-inline-end: 0;
      border-block-end: 1px solid var(--border-default);
    }

    .repository-rail {
      min-height: 132px;
    }

    .repository-list {
      grid-auto-flow: column;
      grid-auto-columns: minmax(148px, 1fr);
      overflow-x: auto;
    }

    .repository-list button,
    .repository-unsupported {
      border-inline-end: 1px solid var(--border-subtle);
    }

    .workflow-workspace {
      display: contents;
    }

    .dispatch-pane,
    .runs-pane {
      min-height: 260px;
    }
  }
</style>
