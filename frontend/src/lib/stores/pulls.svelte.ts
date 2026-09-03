import { Effect } from "effect";
import { executeGeneratedApiRequest, GeneratedApi } from "../api/generated-api.js";
import { TransientTransportError, type ApiProblemError } from "../api/effect-errors.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import { GeneratedProblemResponse } from "../api/runtime.js";
import type { KanbanStatus, PullRequest, PullsParams, UnsetStarredParams } from "../api/types.js";
import type { AppRuntime } from "../app/runtime.js";
import {
  providerDefaultHost,
  providerRouteParams,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { bucketCIChecks, parseCIChecks } from "../utils/ci-buckets.js";
import { normalizeKanbanStatus } from "./workflow.svelte.js";
import { showFlash } from "./flash.svelte.js";
import { PullsWorkflow, type FetchPullResult } from "./pulls-workflow.js";
import { ProviderMutations, providerMutationFailureMessage } from "./ordered-mutations.js";
import { providerItemKey, providerMutationKey } from "./provider-key.js";
import { nextWorkspaceLifecycleTick } from "./workspace-create-pending.svelte.js";
import { readInvolvesMeFilter, writeInvolvesMeFilter } from "./involves-me-filter.js";
import { readUnassignedFilter, writeUnassignedFilter } from "./unassigned-filter.js";

export type { FetchPullResult } from "./pulls-workflow.js";

export interface PullSelection {
  provider: string;
  platformHost?: string | undefined;
  owner: string;
  name: string;
  repoPath: string;
  number: number;
}

type PullIdentityRef = ProviderRouteRef;

export type PullAttributeFilter = "approved" | "draft" | "ready" | "merge_conflicts" | "failed_ci" | "has_workspace";

export interface PullsStoreOptions {
  runtime: AppRuntime;
  getGlobalRepo?: () => string | undefined;
  getGroupByRepo?: () => boolean;
  optimisticDetailStarUpdate?: (ref: ProviderRouteRef, number: number, starred: boolean, envelopeTick: number) => void;
}

function apiErrorMessage(error: { detail?: string; title?: string }, fallback: string): string {
  return error.detail ?? error.title ?? fallback;
}

function readErrorMessage(error: ApiProblemError | TransientTransportError): string {
  if (error._tag === "ApiProblemError") {
    return apiErrorMessage(error.problem, "failed to load pulls");
  }
  return "Could not reach Kenn Forge";
}

export function createPullsStore(opts: PullsStoreOptions) {
  const runtime = opts.runtime;
  const getGlobalRepo = opts.getGlobalRepo ?? (() => undefined);
  const getGroupByRepo = opts.getGroupByRepo ?? (() => false);

  // --- state ---

  let pulls = $state<PullRequest[]>([]);
  let loading = $state(false);
  let listCapped = $state(false);
  let storeError = $state<string | null>(null);
  let filterKanban = $state<KanbanStatus | undefined>(undefined);
  let attributeFilters = $state<PullAttributeFilter[]>([]);
  let kanbanStatusFilters = $state<KanbanStatus[]>([]);
  let filterStarred = $state(false);
  let involvesMe = $state(readInvolvesMeFilter("pulls"));
  let unassigned = $state(readUnassignedFilter("pulls"));
  let filterState = $state<string>("open");
  let searchQuery = $state<string | undefined>(undefined);
  let selectedPR = $state<PullSelection | null>(null);
  let activeListParams: PullsParams | undefined;

  // --- reads ---

  function getPulls(): PullRequest[] {
    return pulls;
  }

  function getFilteredPulls(): PullRequest[] {
    return pulls.filter((pr) => matchesAttributeFilters(pr) && matchesKanbanStatusFilters(pr));
  }

  function isLoading(): boolean {
    return loading;
  }

  function isListCapped(): boolean {
    return listCapped;
  }

  function getError(): string | null {
    return storeError;
  }

  function getSelectedPR(): PullSelection | null {
    return selectedPR;
  }

  function pullIdentityKey(ref: Pick<PullIdentityRef, "provider" | "platformHost" | "repoPath">): string {
    return JSON.stringify([ref.provider, ref.platformHost ?? "", ref.repoPath]);
  }

  function pullRef(pr: PullRequest): PullIdentityRef {
    return {
      provider: pr.repo.provider,
      platformHost: pr.repo.platform_host,
      owner: pr.repo.owner,
      name: pr.repo.name,
      repoPath: pr.repo.repo_path,
    };
  }

  function pullMatchesRef(pr: PullRequest, ref: PullIdentityRef, number: number): boolean {
    return (
      pr.Number === number &&
      pr.repo.provider === ref.provider &&
      pr.repo.platform_host === ref.platformHost &&
      pr.repo.repo_path === ref.repoPath &&
      pr.repo.owner === ref.owner &&
      pr.repo.name === ref.name
    );
  }

  function pullMatchesSelection(pr: PullRequest, sel: PullSelection): boolean {
    return pullMatchesRef(pr, sel, sel.number);
  }

  function concretePlatformHost(ref: Pick<PullIdentityRef, "provider" | "platformHost">): string {
    const host = ref.platformHost ?? providerDefaultHost(ref.provider);
    if (!host) throw new Error("pull request missing platform host");
    return host;
  }

  /** Groups pulls by full provider identity into a Map. */
  function pullsByRepo(): Map<string, PullRequest[]> {
    const map = new Map<string, PullRequest[]>();
    for (const pr of getFilteredPulls()) {
      const key = pullIdentityKey(pullRef(pr));
      const existing = map.get(key);
      if (existing !== undefined) {
        existing.push(pr);
      } else {
        map.set(key, [pr]);
      }
    }
    return map;
  }

  function getFilterKanban(): KanbanStatus | undefined {
    return filterKanban;
  }

  function getAttributeFilters(): PullAttributeFilter[] {
    return attributeFilters;
  }

  function getKanbanStatusFilters(): KanbanStatus[] {
    return kanbanStatusFilters;
  }

  function getLocalFilterCount(): number {
    return attributeFilters.length + kanbanStatusFilters.length + Number(involvesMe) + Number(unassigned);
  }

  function getFilterStarred(): boolean {
    return filterStarred;
  }

  function setFilterStarred(v: boolean): void {
    filterStarred = v;
  }

  function getInvolvesMe(): boolean {
    return involvesMe;
  }

  function setInvolvesMe(value: boolean): void {
    involvesMe = value;
    writeInvolvesMeFilter("pulls", value);
  }

  function getUnassigned(): boolean {
    return unassigned;
  }

  function setUnassigned(value: boolean): void {
    unassigned = value;
    writeUnassignedFilter("pulls", value);
  }

  function getFilterState(): string {
    return filterState;
  }

  function setFilterState(s: string): void {
    filterState = s;
  }

  /** Returns PRs in display order: grouped by repo or flat chronological. */
  function getDisplayOrderPRs(): PullRequest[] {
    if (getGroupByRepo()) {
      const grouped = pullsByRepo();
      const ordered: PullRequest[] = [];
      for (const prs of grouped.values()) {
        ordered.push(...prs);
      }
      return ordered;
    }
    return getFilteredPulls();
  }

  function selectNextPR(): void {
    const list = getDisplayOrderPRs();
    if (list.length === 0) return;
    const sel = selectedPR;
    if (sel === null) {
      const first = list[0];
      if (first !== undefined) {
        selectPRFromPull(first);
      }
      return;
    }
    const idx = list.findIndex((pr) => pullMatchesSelection(pr, sel));
    const next = list[idx + 1];
    if (next !== undefined) {
      selectPRFromPull(next);
    }
  }

  function selectPrevPR(): void {
    const list = getDisplayOrderPRs();
    if (list.length === 0) return;
    const sel = selectedPR;
    if (sel === null) {
      const last = list[list.length - 1];
      if (last !== undefined) {
        selectPRFromPull(last);
      }
      return;
    }
    const idx = list.findIndex((pr) => pullMatchesSelection(pr, sel));
    if (idx > 0) {
      const prev = list[idx - 1];
      if (prev !== undefined) {
        selectPRFromPull(prev);
      }
    }
  }

  // --- writes ---

  function setFilterKanban(kanban: KanbanStatus | undefined): void {
    filterKanban = kanban;
  }

  function toggleAttributeFilter(filter: PullAttributeFilter): void {
    attributeFilters = toggleFilterValue(attributeFilters, filter);
  }

  function toggleKanbanStatusFilter(status: KanbanStatus): void {
    kanbanStatusFilters = toggleFilterValue(kanbanStatusFilters, status);
  }

  function clearLocalFilters(): void {
    attributeFilters = [];
    kanbanStatusFilters = [];
    setInvolvesMe(false);
    setUnassigned(false);
  }

  function getSearchQuery(): string | undefined {
    return searchQuery;
  }

  function setSearchQuery(q: string | undefined): void {
    searchQuery = q;
  }

  function selectPR(
    owner: string,
    name: string,
    number: number,
    provider: string,
    platformHost: string | undefined,
    repoPath: string,
  ): void {
    selectedPR = {
      provider,
      ...(platformHost && { platformHost }),
      owner,
      name,
      repoPath,
      number,
    };
  }

  function selectPRFromPull(pr: PullRequest): void {
    const ref = pullRef(pr);
    selectPR(ref.owner, ref.name, pr.Number, ref.provider, ref.platformHost, ref.repoPath);
  }

  function clearSelection(): void {
    selectedPR = null;
  }

  /** Returns the current kanban status for a PR. */
  function getPullKanbanStatus(ref: PullIdentityRef, number: number): KanbanStatus | undefined {
    const pr = pulls.find((p) => pullMatchesRef(p, ref, number));
    return pr?.KanbanStatus as KanbanStatus | undefined;
  }

  /** Optimistically update a single PR's kanban status. */
  function optimisticKanbanUpdate(ref: PullIdentityRef, number: number, status: KanbanStatus): void {
    pulls = pulls.map((pr) => (pullMatchesRef(pr, ref, number) ? { ...pr, KanbanStatus: status } : pr));
  }

  function optimisticStarUpdate(ref: PullIdentityRef, number: number, starred: boolean): void {
    pulls = pulls.map((pr) => (pullMatchesRef(pr, ref, number) ? { ...pr, Starred: starred } : pr));
  }

  function togglePRStar(ref: PullIdentityRef, number: number, currentlyStarred: boolean): void {
    const platformHost = concretePlatformHost(ref);
    const starredItem: UnsetStarredParams = {
      item_type: "pr",
      provider: ref.provider,
      platform_host: platformHost,
      owner: ref.owner,
      name: ref.name,
      number,
    };
    const nextStarred = !currentlyStarred;
    const mutationTick = nextWorkspaceLifecycleTick();
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const commit = (
        currentlyStarred
          ? executeGeneratedApiRequest<void>("DELETE pull request star", (client, signal) =>
              client.SettingsService.unsetStarred(starredItem, { signal }),
            )
          : executeGeneratedApiRequest<void>("PUT pull request star", (client, signal) =>
              client.SettingsService.setStarred(starredItem, { signal }),
            )
      ).pipe(Effect.as(nextStarred));
      const refreshOnStale = executeGeneratedApiRequest(
        "GET pull request after stale list star mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.getPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.getPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => Boolean(response.merge_request.Starred)));
      yield* mutations.submit({
        key: providerMutationKey(
          "pull",
          { provider: ref.provider, platformHost, owner: ref.owner, name: ref.name, number },
          "star",
        ),
        baseline: currentlyStarred,
        optimistic: nextStarred,
        apply: (starred) =>
          Effect.sync(() => {
            optimisticStarUpdate(ref, number, starred);
            opts.optimisticDetailStarUpdate?.(ref, number, starred, mutationTick);
          }),
        commit,
        refreshOnStale,
      });
      mutationSettled = true;
      const workflow = yield* PullsWorkflow;
      yield* workflow.invalidate(
        providerItemKey({ provider: ref.provider, platformHost, owner: ref.owner, name: ref.name, number }),
      );
      yield* loadPullsEffect();
    });
    runtime.runCommand(program, {
      operation: currentlyStarred ? "unstar pull request from list" : "star pull request from list",
      safeContext: { provider: ref.provider, platformHost, owner: ref.owner, name: ref.name, number },
      onFailure: (failure) => {
        if (mutationSettled) {
          storeError = providerMutationFailureMessage(failure, "failed to refresh pull requests");
          return;
        }
        showFlash(
          providerMutationFailureMessage(failure, currentlyStarred ? "failed to unstar PR" : "failed to star PR"),
          { tone: "danger" },
        );
      },
    });
  }

  function fetchSinglePull(
    owner: string,
    name: string,
    number: number,
    identity: ProviderRouteRef,
    onResult: (result: FetchPullResult) => void,
  ): void {
    const ref = identity;
    const key = providerItemKey({
      provider: ref.provider,
      platformHost: concretePlatformHost(ref),
      owner,
      name,
      number,
    });
    const program = Effect.gen(function* () {
      const api = yield* GeneratedApi;
      const request = Effect.tryPromise({
        try: async (signal): Promise<FetchPullResult> => {
          try {
            const data = providerUsesHostRoute(ref)
              ? await api.client.PullRequestsService.getPullOnHost(
                  { ...providerHostRouteParams(ref), number },
                  { signal },
                )
              : await api.client.PullRequestsService.getPull({ ...providerRouteParams(ref), number }, { signal });
            const pull: PullRequest = {
              ...data.merge_request,
              repo: data.repo,
              platform_host: data.platform_host,
              repo_owner: data.repo_owner,
              repo_name: data.repo_name,
              detail_loaded: data.detail_loaded,
              ...(data.detail_fetched_at !== undefined && { detail_fetched_at: data.detail_fetched_at }),
              worktree_links: data.worktree_links,
            };
            return { status: "found", pull };
          } catch (cause) {
            if (cause instanceof GeneratedProblemResponse && cause.problem.status === 404) {
              return { status: "not-found" };
            }
            throw cause;
          }
        },
        catch: (cause) => TransientTransportError.make({ operation: "GET pull request", cause }),
      }).pipe(
        Effect.catch((failure) => {
          const result: FetchPullResult = { status: "error", message: readErrorMessage(failure) };
          return Effect.succeed(result);
        }),
      );
      const workflow = yield* PullsWorkflow;
      const result = yield* workflow.refresh(key, request);
      yield* Effect.sync(() => onResult(result));
    });
    runtime.runCommand(program, {
      operation: "refresh pull request",
      safeContext: { provider: ref.provider, platformHost: concretePlatformHost(ref), owner, name, number },
      onFailure: () => {},
    });
  }

  function invalidatePullRefresh(ref: PullIdentityRef, number: number): void {
    const key = providerItemKey({
      provider: ref.provider,
      platformHost: concretePlatformHost(ref),
      owner: ref.owner,
      name: ref.name,
      number,
    });
    const program = Effect.gen(function* () {
      const workflow = yield* PullsWorkflow;
      yield* workflow.invalidate(key);
    });
    runtime.runCommand(program, {
      operation: "invalidate pull request refresh",
      safeContext: { provider: ref.provider, platformHost: concretePlatformHost(ref), number },
      onFailure: () => {},
    });
  }

  function loadPullsEffect(params: PullsParams | undefined = activeListParams) {
    const globalRepo = getGlobalRepo();
    const query: PullsParams = {
      state: filterState,
      ...(globalRepo !== undefined && { repo: globalRepo }),
      ...(filterKanban !== undefined && { kanban: filterKanban }),
      ...(filterStarred && { starred: true }),
      ...(involvesMe && { involves_me: true }),
      ...(unassigned && { unassigned: true }),
      ...(searchQuery !== undefined && { q: searchQuery }),
      ...params,
    };
    const read = executeGeneratedApiRequest("GET /pulls", (client, signal) =>
      client.PullRequestsService.listPulls(query, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.map((result) => result ?? []),
    );
    return Effect.sync(() => {
      loading = true;
      storeError = null;
    }).pipe(
      Effect.andThen(
        Effect.gen(function* () {
          const workflow = yield* PullsWorkflow;
          return yield* workflow.list(read);
        }),
      ),
      Effect.tap((result) =>
        Effect.sync(() => {
          pulls = result;
          listCapped = query.limit !== undefined && result.length === query.limit;
          loading = false;
        }),
      ),
      Effect.tapError((failure) =>
        Effect.sync(() => {
          storeError = readErrorMessage(failure);
          loading = false;
        }),
      ),
    );
  }

  function reconcilePullsEffect(params: PullsParams | undefined = activeListParams) {
    return Effect.suspend(() => {
      const globalRepo = getGlobalRepo();
      const query: PullsParams = {
        state: filterState,
        ...(globalRepo !== undefined && { repo: globalRepo }),
        ...(filterKanban !== undefined && { kanban: filterKanban }),
        ...(filterStarred && { starred: true }),
        ...(involvesMe && { involves_me: true }),
        ...(unassigned && { unassigned: true }),
        ...(searchQuery !== undefined && { q: searchQuery }),
        ...params,
      };
      const read = executeGeneratedApiRequest("GET /pulls after provider event", (client, signal) =>
        client.PullRequestsService.listPulls(query, { signal }),
      ).pipe(
        retryIdempotentRead,
        Effect.map((result) => result ?? []),
      );
      return Effect.gen(function* () {
        const workflow = yield* PullsWorkflow;
        yield* workflow.reconcile(read, (result) =>
          Effect.sync(() => {
            pulls = [...result];
            listCapped = query.limit !== undefined && result.length === query.limit;
          }),
        );
      });
    });
  }

  function loadPulls(params?: PullsParams): void {
    activeListParams = params;
    runtime.runCommand(loadPullsEffect(params), {
      operation: "load pull requests",
      safeContext: {},
      onFailure: (failure) => {
        storeError = readErrorMessage(failure);
        loading = false;
      },
    });
  }

  function toggleFilterValue<T extends string>(values: T[], value: T): T[] {
    if (values.includes(value)) {
      return values.filter((item) => item !== value);
    }
    return [...values, value];
  }

  function matchesAttributeFilters(pr: PullRequest): boolean {
    if (attributeFilters.length === 0) return true;
    return attributeFilters.every((filter) => matchesAttributeFilter(pr, filter));
  }

  function matchesAttributeFilter(pr: PullRequest, filter: PullAttributeFilter): boolean {
    if (filter === "approved") {
      return pr.ReviewDecision.trim().toUpperCase() === "APPROVED";
    }
    if (filter === "draft") {
      return pr.IsDraft;
    }
    if (filter === "ready") {
      return pr.State === "open" && !pr.IsDraft;
    }
    if (filter === "merge_conflicts") {
      return pr.MergeableState === "dirty";
    }
    if (filter === "failed_ci") {
      return hasFailedCI(pr);
    }
    return pr.workspace !== undefined;
  }

  function hasFailedCI(pr: PullRequest): boolean {
    const status = pr.CIStatus.trim().toLowerCase();
    if (status === "failure" || status === "failed" || status === "error") {
      return true;
    }
    const parsed = parseCIChecks(pr.CIChecksJSON);
    if (parsed.error !== null) return false;
    return bucketCIChecks(parsed.checks).failed.length > 0;
  }

  function matchesKanbanStatusFilters(pr: PullRequest): boolean {
    return kanbanStatusFilters.length === 0 || kanbanStatusFilters.includes(normalizeKanbanStatus(pr.KanbanStatus));
  }

  return {
    getPulls,
    getFilteredPulls,
    isLoading,
    isListCapped,
    getError,
    getSelectedPR,
    pullsByRepo,
    getFilterKanban,
    getAttributeFilters,
    getKanbanStatusFilters,
    getLocalFilterCount,
    getFilterStarred,
    setFilterStarred,
    getInvolvesMe,
    setInvolvesMe,
    getUnassigned,
    setUnassigned,
    getFilterState,
    setFilterState,
    getDisplayOrderPRs,
    selectNextPR,
    selectPrevPR,
    setFilterKanban,
    toggleAttributeFilter,
    toggleKanbanStatusFilter,
    clearLocalFilters,
    getSearchQuery,
    setSearchQuery,
    selectPR,
    clearSelection,
    getPullKanbanStatus,
    optimisticKanbanUpdate,
    optimisticStarUpdate,
    togglePRStar,
    loadPulls,
    loadPullsEffect,
    reconcilePullsEffect,
    fetchSinglePull,
    invalidatePullRefresh,
  };
}

export type PullsStore = ReturnType<typeof createPullsStore>;
