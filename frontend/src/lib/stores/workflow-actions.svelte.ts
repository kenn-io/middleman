import { Effect } from "effect";

import type { components } from "../api/generated/schema.js";
import { GeneratedApi, type GeneratedClient, type executeGeneratedRequest } from "../api/generated-api.js";
import type { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import {
  canonicalProvider,
  providerActionsPath,
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
} from "../api/provider-routes.js";
import type { AppExecution, AppRuntime } from "../app/runtime.js";
import type { WorkflowDispatchProgressEvent } from "./provider-events-workflow.js";

export type WorkflowCatalog = components["schemas"]["WorkflowCatalogResponse"];
export type WorkflowDefinition = components["schemas"]["WorkflowDefinitionResponse"];
export type WorkflowEnvironment = components["schemas"]["WorkflowEnvironmentResponse"];
export type WorkflowRun = components["schemas"]["WorkflowRunResponse"];
export type WorkflowRunJob = components["schemas"]["WorkflowRunJobResponse"];
export type WorkflowActionsError = ApiProblemError | TransientTransportError;

export interface WorkflowDispatchInput {
  readonly ref: ProviderRouteRef;
  readonly workflowId: string;
  readonly expectedDefinitionSha: string;
  readonly dispatchRef: string;
  readonly inputs: Readonly<Record<string, unknown>>;
}

/**
 * One dispatch cycle per workflow. The POST moves pending to locating or
 * succeeded; server workflow_dispatch_progress events move locating to
 * succeeded or unresolved and refresh the run. Only newDispatchCycle clears it.
 */
export type WorkflowDispatchState =
  | { readonly kind: "pending" }
  | { readonly kind: "locating"; readonly dispatchId: string }
  | { readonly kind: "succeeded"; readonly dispatchId: string; readonly run?: WorkflowRun }
  | { readonly kind: "unresolved"; readonly dispatchId: string }
  | { readonly kind: "failed"; readonly error: WorkflowActionsError }
  | { readonly kind: "uncertain"; readonly error: WorkflowActionsError };

export interface WorkflowActionsLoading {
  readonly catalog: boolean;
  readonly runs: boolean;
  readonly jobs: readonly string[];
}

export interface WorkflowRunsPageState {
  readonly nextCursor: string | null;
  readonly exhausted: boolean;
  readonly loadingMore: boolean;
}

export interface WorkflowActionsSnapshot {
  readonly ref: ProviderRouteRef;
  readonly catalog: WorkflowCatalog | null;
  readonly selectedWorkflow: WorkflowDefinition | null;
  readonly runs: readonly WorkflowRun[];
  readonly runsPage: WorkflowRunsPageState;
  readonly jobs: Readonly<Record<string, readonly WorkflowRunJob[]>>;
  readonly loading: WorkflowActionsLoading;
  readonly dispatches: Readonly<Record<string, WorkflowDispatchState>>;
  readonly catalogRefreshErrors: Readonly<Record<string, WorkflowActionsError>>;
  readonly error: WorkflowActionsError | null;
}

export interface WorkflowActionsStoreOptions {
  readonly runtime: AppRuntime;
}

export interface WorkflowActionsStore {
  /** Reads the catalog once per repository; later calls are no-ops until refreshCatalog. */
  readonly loadCatalog: (ref: ProviderRouteRef) => void;
  readonly refreshCatalog: (ref: ProviderRouteRef, workflowId: string) => void;
  readonly clearCatalogRefreshError: (ref: ProviderRouteRef, workflowId: string) => void;
  readonly selectWorkflow: (ref: ProviderRouteRef, workflowId: string | null) => void;
  readonly loadMoreRuns: (ref: ProviderRouteRef) => void;
  readonly loadJobs: (ref: ProviderRouteRef, runId: string) => void;
  readonly dispatch: (input: WorkflowDispatchInput) => void;
  readonly newDispatchCycle: (ref: ProviderRouteRef, workflowId: string) => void;
  readonly applyDispatchProgress: (event: WorkflowDispatchProgressEvent) => void;
  readonly setEnabled: (enabled: boolean) => void;
  readonly getSnapshot: (ref: ProviderRouteRef) => WorkflowActionsSnapshot | null;
  readonly getCatalog: (ref: ProviderRouteRef) => WorkflowCatalog | null;
  readonly getEnvironments: (ref: ProviderRouteRef) => readonly WorkflowEnvironment[];
  readonly getSelectedWorkflow: (ref: ProviderRouteRef) => WorkflowDefinition | null;
  readonly getRuns: (ref: ProviderRouteRef) => readonly WorkflowRun[];
  readonly getJobs: (ref: ProviderRouteRef, runId: string) => readonly WorkflowRunJob[];
  readonly getLoading: (ref: ProviderRouteRef) => WorkflowActionsLoading;
  readonly getDispatch: (ref: ProviderRouteRef, workflowId: string) => WorkflowDispatchState | null;
}

type GeneratedApiService = {
  readonly client: GeneratedClient;
  readonly execute: typeof executeGeneratedRequest;
};

const runsPageSize = 50;
const notLoading: WorkflowActionsLoading = { catalog: false, runs: false, jobs: [] };

export function workflowRepositoryKey(
  ref: Pick<ProviderRouteRef, "provider" | "platformHost" | "owner" | "name">,
): string {
  const provider = canonicalProvider(ref.provider);
  return [provider, resolvedPlatformHost(provider, ref.platformHost).toLowerCase(), ref.owner, ref.name]
    .map(encodeURIComponent)
    .join("|");
}

function emptySnapshot(ref: ProviderRouteRef): WorkflowActionsSnapshot {
  return {
    ref,
    catalog: null,
    selectedWorkflow: null,
    runs: [],
    runsPage: { nextCursor: null, exhausted: false, loadingMore: false },
    jobs: {},
    loading: notLoading,
    dispatches: {},
    catalogRefreshErrors: {},
    error: null,
  };
}

function isWorkflowRun(value: unknown): value is WorkflowRun {
  return typeof value === "object" && value !== null && typeof (value as { id?: unknown }).id === "string";
}

function upsertRun(runs: readonly WorkflowRun[], run: WorkflowRun): readonly WorkflowRun[] {
  const index = runs.findIndex((candidate) => candidate.id === run.id);
  if (index === -1) return [run, ...runs];
  return runs.map((candidate) => (candidate.id === run.id ? run : candidate));
}

/** A dispatch response may name a run with only its id; never let it replace a listed run. */
function isPartialRun(run: WorkflowRun): boolean {
  return run.status === "" && run.run_number === 0;
}

function mergeNamedRun(
  runs: readonly WorkflowRun[],
  run: WorkflowRun,
): { runs: readonly WorkflowRun[]; run: WorkflowRun } {
  const existing = runs.find((candidate) => candidate.id === run.id);
  if (existing && isPartialRun(run)) return { runs, run: existing };
  return { runs: upsertRun(runs, run), run };
}

function isOutcomeUncertain(error: WorkflowActionsError): boolean {
  return (
    error._tag === "TransientTransportError" ||
    (error._tag === "ApiProblemError" && error.problem.code === "mutationOutcomeUnknown")
  );
}

interface Generations {
  catalog: number;
  runs: number;
}

export function createWorkflowActionsStore(options: WorkflowActionsStoreOptions): WorkflowActionsStore {
  const runtime = options.runtime;
  let enabled = true;
  let snapshots = $state.raw<Readonly<Record<string, WorkflowActionsSnapshot>>>({});
  const generations = new Map<string, Generations>();
  const executions = new Map<string, AppExecution<unknown, never>>();
  // Progress events can arrive before the POST response names the dispatch;
  // hold them until the cycle owns that dispatch id.
  const earlyProgress = new Map<string, WorkflowDispatchProgressEvent>();

  function snapshotFor(ref: ProviderRouteRef): WorkflowActionsSnapshot {
    return snapshots[workflowRepositoryKey(ref)] ?? emptySnapshot(ref);
  }

  function update(ref: ProviderRouteRef, change: (snapshot: WorkflowActionsSnapshot) => WorkflowActionsSnapshot): void {
    const key = workflowRepositoryKey(ref);
    snapshots = { ...snapshots, [key]: change(snapshots[key] ?? emptySnapshot(ref)) };
  }

  function nextGeneration(ref: ProviderRouteRef, field: keyof Generations): number {
    const key = workflowRepositoryKey(ref);
    const current = generations.get(key) ?? { catalog: 0, runs: 0 };
    const next = { ...current, [field]: current[field] + 1 };
    generations.set(key, next);
    return next[field];
  }

  function isCurrent(ref: ProviderRouteRef, field: keyof Generations, generation: number): boolean {
    return (generations.get(workflowRepositoryKey(ref))?.[field] ?? 0) === generation;
  }

  function run<A>(
    slot: string,
    operation: string,
    ref: ProviderRouteRef,
    request: (api: GeneratedApiService) => Effect.Effect<A, WorkflowActionsError>,
    onSettled: { onFailure: (error: WorkflowActionsError) => void; onSuccess: (value: A) => void },
  ): void {
    executions.get(slot)?.interrupt();
    const execution = runtime.runCommand(
      Effect.gen(function* () {
        const api = yield* GeneratedApi;
        return yield* request(api);
      }).pipe(
        Effect.matchEffect({
          onFailure: (error) => Effect.sync(() => onSettled.onFailure(error)),
          onSuccess: (value) => Effect.sync(() => onSettled.onSuccess(value)),
        }),
      ),
      {
        operation,
        safeContext: { provider: ref.provider, owner: ref.owner, name: ref.name },
        onFailure: () => {},
      },
    );
    executions.set(slot, execution);
  }

  function readCatalog(
    ref: ProviderRouteRef,
    onDone: (catalog: WorkflowCatalog | null, error: WorkflowActionsError | null) => void,
  ): void {
    const generation = nextGeneration(ref, "catalog");
    update(ref, (snapshot) => ({ ...snapshot, loading: { ...snapshot.loading, catalog: true } }));
    run(
      `${workflowRepositoryKey(ref)}:catalog`,
      "GET workflow catalog",
      ref,
      (api) =>
        api.execute("GET workflow catalog", (signal) =>
          api.client.GET(providerActionsPath(ref, "/workflows"), {
            params: { path: providerRouteParams(ref) },
            signal,
          }),
        ),
      {
        onFailure: (error) => {
          if (!isCurrent(ref, "catalog", generation)) return;
          update(ref, (snapshot) => ({ ...snapshot, loading: { ...snapshot.loading, catalog: false } }));
          onDone(null, error);
        },
        onSuccess: (catalog) => {
          if (!isCurrent(ref, "catalog", generation)) return;
          update(ref, (snapshot) => ({
            ...snapshot,
            catalog,
            selectedWorkflow:
              catalog.workflows?.find((workflow) => workflow.id === snapshot.selectedWorkflow?.id) ??
              snapshot.selectedWorkflow,
            loading: { ...snapshot.loading, catalog: false },
          }));
          onDone(catalog, null);
        },
      },
    );
  }

  function loadCatalog(ref: ProviderRouteRef): void {
    if (!enabled) return;
    const existing = snapshotFor(ref);
    if (existing.catalog !== null || existing.loading.catalog) return;
    readCatalog(ref, (_catalog, error) => {
      if (error) update(ref, (snapshot) => ({ ...snapshot, error }));
    });
  }

  function refreshCatalog(ref: ProviderRouteRef, workflowId: string): void {
    if (!enabled) return;
    readCatalog(ref, (_catalog, error) => {
      update(ref, (snapshot) => {
        const { [workflowId]: _cleared, ...refreshErrors } = snapshot.catalogRefreshErrors;
        if (error) return { ...snapshot, catalogRefreshErrors: { ...refreshErrors, [workflowId]: error } };
        const { [workflowId]: _cycle, ...dispatches } = snapshot.dispatches;
        return { ...snapshot, catalogRefreshErrors: refreshErrors, dispatches, error: null };
      });
    });
  }

  function clearCatalogRefreshError(ref: ProviderRouteRef, workflowId: string): void {
    update(ref, (snapshot) => {
      const { [workflowId]: _cleared, ...catalogRefreshErrors } = snapshot.catalogRefreshErrors;
      return { ...snapshot, catalogRefreshErrors };
    });
  }

  function readRuns(ref: ProviderRouteRef, workflowId: string, cursor: string | undefined): void {
    const generation =
      cursor === undefined ? nextGeneration(ref, "runs") : (generations.get(workflowRepositoryKey(ref))?.runs ?? 0);
    run(
      `${workflowRepositoryKey(ref)}:runs`,
      "GET workflow runs",
      ref,
      (api) =>
        api.execute("GET workflow runs", (signal) =>
          api.client.GET(providerActionsPath(ref, "/runs"), {
            params: {
              path: providerRouteParams(ref),
              query: { workflow_id: workflowId, per_page: runsPageSize, ...(cursor !== undefined && { cursor }) },
            },
            signal,
          }),
        ),
      {
        onFailure: (error) => {
          if (!isCurrent(ref, "runs", generation)) return;
          update(ref, (snapshot) => ({
            ...snapshot,
            error,
            loading: { ...snapshot.loading, runs: false },
            runsPage: { ...snapshot.runsPage, loadingMore: false },
          }));
        },
        onSuccess: (page) => {
          if (!isCurrent(ref, "runs", generation)) return;
          update(ref, (snapshot) => {
            if (snapshot.selectedWorkflow?.id !== workflowId) return snapshot;
            const items = page.items ?? [];
            return {
              ...snapshot,
              runs:
                cursor === undefined
                  ? items
                  : [...snapshot.runs, ...items.filter((item) => !snapshot.runs.some((run) => run.id === item.id))],
              runsPage: { nextCursor: page.next_cursor ?? null, exhausted: page.exhausted, loadingMore: false },
              loading: { ...snapshot.loading, runs: false },
              error: null,
            };
          });
        },
      },
    );
  }

  function selectWorkflow(ref: ProviderRouteRef, workflowId: string | null): void {
    if (!enabled) return;
    const snapshot = snapshotFor(ref);
    const workflow =
      workflowId === null
        ? null
        : (snapshot.catalog?.workflows?.find((candidate) => candidate.id === workflowId) ?? null);
    executions.get(`${workflowRepositoryKey(ref)}:runs`)?.interrupt();
    nextGeneration(ref, "runs");
    update(ref, (current) => ({
      ...current,
      selectedWorkflow: workflow,
      runs: [],
      jobs: {},
      runsPage: { nextCursor: null, exhausted: false, loadingMore: false },
      loading: { ...current.loading, runs: workflow !== null, jobs: [] },
    }));
    if (workflow) readRuns(ref, workflow.id, undefined);
  }

  function loadMoreRuns(ref: ProviderRouteRef): void {
    if (!enabled) return;
    const snapshot = snapshotFor(ref);
    const cursor = snapshot.runsPage.nextCursor;
    if (!snapshot.selectedWorkflow || !cursor || snapshot.runsPage.exhausted || snapshot.runsPage.loadingMore) return;
    update(ref, (current) => ({ ...current, runsPage: { ...current.runsPage, loadingMore: true } }));
    readRuns(ref, snapshot.selectedWorkflow.id, cursor);
  }

  function loadJobs(ref: ProviderRouteRef, runId: string): void {
    if (!enabled) return;
    update(ref, (snapshot) => ({
      ...snapshot,
      loading: {
        ...snapshot.loading,
        jobs: snapshot.loading.jobs.includes(runId) ? snapshot.loading.jobs : [...snapshot.loading.jobs, runId],
      },
    }));
    run(
      `${workflowRepositoryKey(ref)}:jobs:${runId}`,
      "GET workflow run jobs",
      ref,
      (api) =>
        api.execute("GET workflow run jobs", (signal) =>
          api.client.GET(providerActionsPath(ref, "/runs/{run_id}/jobs"), {
            params: { path: { ...providerRouteParams(ref), run_id: runId } },
            signal,
          }),
        ),
      {
        onFailure: (error) =>
          update(ref, (snapshot) => ({
            ...snapshot,
            error,
            loading: { ...snapshot.loading, jobs: snapshot.loading.jobs.filter((id) => id !== runId) },
          })),
        onSuccess: (page) =>
          update(ref, (snapshot) => ({
            ...snapshot,
            jobs: { ...snapshot.jobs, [runId]: page.items ?? [] },
            loading: { ...snapshot.loading, jobs: snapshot.loading.jobs.filter((id) => id !== runId) },
          })),
      },
    );
  }

  function setDispatch(ref: ProviderRouteRef, workflowId: string, state: WorkflowDispatchState | null): void {
    update(ref, (snapshot) => {
      const { [workflowId]: _previous, ...dispatches } = snapshot.dispatches;
      return { ...snapshot, dispatches: state === null ? dispatches : { ...dispatches, [workflowId]: state } };
    });
  }

  function dispatch(input: WorkflowDispatchInput): void {
    if (!enabled) return;
    const { ref, workflowId } = input;
    if (snapshotFor(ref).dispatches[workflowId]?.kind === "pending") return;
    setDispatch(ref, workflowId, { kind: "pending" });
    run(
      `${workflowRepositoryKey(ref)}:dispatch:${workflowId}`,
      "POST workflow dispatch",
      ref,
      (api) =>
        api.execute("POST workflow dispatch", (signal) =>
          api.client.POST(providerActionsPath(ref, "/workflows/{workflow_id}/dispatch"), {
            params: { path: { ...providerRouteParams(ref), workflow_id: workflowId } },
            body: {
              expected_definition_sha: input.expectedDefinitionSha,
              inputs: input.inputs,
              ref: input.dispatchRef,
            },
            signal,
          }),
        ),
      {
        onFailure: (error) =>
          setDispatch(
            ref,
            workflowId,
            isOutcomeUncertain(error) ? { kind: "uncertain", error } : { kind: "failed", error },
          ),
        onSuccess: (response) => {
          const dispatchId = response.dispatch_id;
          if (response.run) {
            let named = response.run;
            update(ref, (snapshot) => {
              if (snapshot.selectedWorkflow?.id !== workflowId) return snapshot;
              const merged = mergeNamedRun(snapshot.runs, named);
              named = merged.run;
              return { ...snapshot, runs: merged.runs };
            });
            setDispatch(ref, workflowId, { kind: "succeeded", dispatchId, run: named });
          } else {
            setDispatch(ref, workflowId, { kind: "locating", dispatchId });
          }
          const early = earlyProgress.get(dispatchId);
          if (early) {
            earlyProgress.delete(dispatchId);
            applyDispatchProgress(early);
          }
        },
      },
    );
  }

  function newDispatchCycle(ref: ProviderRouteRef, workflowId: string): void {
    setDispatch(ref, workflowId, null);
  }

  function applyDispatchProgress(event: WorkflowDispatchProgressEvent): void {
    const key = workflowRepositoryKey({
      provider: event.provider,
      platformHost: event.platform_host,
      owner: event.owner,
      name: event.name,
    });
    const snapshot = snapshots[key];
    if (!snapshot) return;
    const run = isWorkflowRun(event.run) ? event.run : undefined;
    const cycle = snapshot.dispatches[event.workflow_id];
    if (cycle?.kind === "pending") earlyProgress.set(event.dispatch_id, event);
    update(snapshot.ref, (current) => {
      const runs =
        run && current.selectedWorkflow?.id === event.workflow_id ? upsertRun(current.runs, run) : current.runs;
      const owned = cycle !== undefined && "dispatchId" in cycle && cycle.dispatchId === event.dispatch_id;
      let dispatches = current.dispatches;
      if (owned) {
        const state: WorkflowDispatchState =
          event.status === "unresolved"
            ? { kind: "unresolved", dispatchId: event.dispatch_id }
            : { kind: "succeeded", dispatchId: event.dispatch_id, ...(run !== undefined && { run }) };
        dispatches = { ...current.dispatches, [event.workflow_id]: state };
      }
      return { ...current, runs, dispatches };
    });
  }

  function setEnabled(nextEnabled: boolean): void {
    enabled = nextEnabled;
    if (nextEnabled) return;
    for (const execution of executions.values()) execution.interrupt();
    executions.clear();
    generations.clear();
    earlyProgress.clear();
    snapshots = {};
  }

  return {
    loadCatalog,
    refreshCatalog,
    clearCatalogRefreshError,
    selectWorkflow,
    loadMoreRuns,
    loadJobs,
    dispatch,
    newDispatchCycle,
    applyDispatchProgress,
    setEnabled,
    getSnapshot: (ref) => snapshots[workflowRepositoryKey(ref)] ?? null,
    getCatalog: (ref) => snapshots[workflowRepositoryKey(ref)]?.catalog ?? null,
    getEnvironments: (ref) => snapshots[workflowRepositoryKey(ref)]?.catalog?.environments ?? [],
    getSelectedWorkflow: (ref) => snapshots[workflowRepositoryKey(ref)]?.selectedWorkflow ?? null,
    getRuns: (ref) => snapshots[workflowRepositoryKey(ref)]?.runs ?? [],
    getJobs: (ref, runId) => snapshots[workflowRepositoryKey(ref)]?.jobs[runId] ?? [],
    getLoading: (ref) => snapshots[workflowRepositoryKey(ref)]?.loading ?? notLoading,
    getDispatch: (ref, workflowId) => snapshots[workflowRepositoryKey(ref)]?.dispatches[workflowId] ?? null,
  };
}
