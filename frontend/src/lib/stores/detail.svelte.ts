import { Effect, Result } from "effect";
import type { AppExecution, AppRuntime } from "../app/runtime.js";
import { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import { executeGeneratedApiRequest, type GeneratedApi } from "../api/generated-api.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import type {
  ApprovePRInputBody,
  EditPRContentInputBody,
  GithubStateInputBody,
  KanbanStatus,
  Label,
  MergeParams,
  PREvent,
  PullDetail,
  RequestChangesPRInputBody,
  UnsetStarredParams,
} from "../api/types.js";
import type { ApplySuggestionRequest } from "../utils/markdown-suggestions.js";
import {
  isProblem,
  ProblemCodes,
  problemConflictContext,
  problemConflictReason,
  problemRetryAfter,
  type ConflictReason,
  type ProblemBody,
} from "../api/problems.js";
import {
  canonicalProvider,
  providerDefaultHost,
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { DetailWorkflow, type DetailReadError } from "./detail-workflow.js";
import { showFlash } from "./flash.svelte.js";
import { nextWorkspaceLifecycleTick } from "./workspace-create-pending.svelte.js";
import { providerItemKey, providerMutationKey } from "./provider-key.js";
import {
  invokeMutationCallback,
  invokeMutationFailure,
  ProviderMutations,
  type ProviderMutationError,
  providerMutationFailureMessage,
  providerMutationProblem,
  type MutationCallbacks,
  type ProviderMutationFailure,
} from "./ordered-mutations.js";
import { normalizeKanbanStatus } from "./workflow.svelte.js";

export type DetailSyncMode = boolean | "background";

export interface DetailRequestOptions {
  sync?: DetailSyncMode;
  workflowApprovalSync?: boolean;
  provider: string;
  platformHost?: string | undefined;
  repoPath: string;
}

type DetailRequestRef = {
  owner: string;
  name: string;
  number: number;
  provider: string;
  platformHost?: string | undefined;
  repoPath: string;
};

interface PullCommentMutationState {
  readonly event: PREvent;
  readonly index: number;
  readonly present: boolean;
}

export interface DetailStoreOptions {
  runtime: AppRuntime;
  getPage?: () => string;
  onDetailSynchronized?: () => void;
  pulls?: {
    loadPulls: () => void;
    optimisticKanbanUpdate?: (ref: ProviderRouteRef, number: number, status: KanbanStatus) => void;
    getPullKanbanStatus?: (ref: ProviderRouteRef, number: number) => KanbanStatus | undefined;
    optimisticStarUpdate?: (ref: ProviderRouteRef, number: number, starred: boolean) => void;
  };
  sync?: {
    subscribeSyncComplete: (cb: () => void) => () => void;
    refreshSyncStatus?: () => void;
  };
}

export interface ProviderActionCallbacks extends MutationCallbacks {
  readonly onProblem?: (problem: ProblemBody) => void;
}

export type MergePullOutcome =
  | { readonly _tag: "Queued" }
  | { readonly _tag: "Merged"; readonly cleanupWarning?: string };

export type MergePullCallbacks = Omit<ProviderActionCallbacks, "onSuccess"> & {
  readonly onSuccess?: (outcome: MergePullOutcome) => void;
};

export interface DetailRefreshCallbacks extends MutationCallbacks {}

export interface DetailSyncCallbacks {
  readonly onSuccess?: (refreshed: boolean) => void;
  readonly onFailure?: (message: string) => void;
  readonly onSettled?: () => void;
}

interface DetailRefreshResult {
  readonly applied: boolean;
  readonly fetchedAt?: string;
}

export interface ApplySuggestionConflict {
  reason: Exclude<ConflictReason, "conflict">;
  context?: string | undefined;
  expectedHeadSha: string;
  ref: ProviderRouteRef;
  number: number;
}

export interface ApplySuggestionCallbacks {
  readonly onConflict?: (conflict: ApplySuggestionConflict) => void;
  readonly onResult?: (applied: boolean) => void;
  readonly onSettled?: () => void;
}

function apiErrorMessage(error: { detail?: string; title?: string }, fallback: string): string {
  const message = error.detail ?? error.title ?? fallback;
  if (!isProblem(error)) return message;

  const retryAt = problemRetryAfter(error);
  if (!retryAt) return message;

  const localRetryTime = retryAt.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  });
  return `${message}; retry at ${localRetryTime}`;
}

function detailReadErrorMessage(error: ApiProblemError | TransientTransportError): string {
  if (error._tag === "ApiProblemError") {
    if (error.problem.code === ProblemCodes.repoNotFound) {
      return "This repository is not available in Forge. Add it under Settings → Repositories, then retry.";
    }
    if (error.problem.code === ProblemCodes.pullNotFound) {
      return "This pull request is not available from the provider.";
    }
    return apiErrorMessage(error.problem, "failed to load pull request");
  }
  return "Could not reach Kenn Forge";
}

const invokeDetailSyncSuccess = (
  callback: ((refreshed: boolean) => void) | undefined,
  refreshed: boolean,
): Effect.Effect<void> =>
  callback === undefined
    ? Effect.void
    : Effect.sync(() => callback(refreshed)).pipe(Effect.catchCause(() => Effect.void));

const invokeApplySuggestionConflict = (
  callback: ((conflict: ApplySuggestionConflict) => void) | undefined,
  conflict: ApplySuggestionConflict,
): Effect.Effect<void> =>
  callback === undefined
    ? Effect.void
    : Effect.sync(() => callback(conflict)).pipe(Effect.catchCause(() => Effect.void));

function applySuggestionRefreshReason(problem: ProblemBody): Exclude<ConflictReason, "conflict"> | undefined {
  const reason: ConflictReason | undefined = problemConflictReason(problem);
  if (
    reason === "stale_state" ||
    reason === "head_unknown" ||
    reason === "not_open" ||
    reason === "head_repo_unknown"
  ) {
    return reason;
  }
  return undefined;
}

function syncIntentRank(mode: DetailSyncMode): number {
  if (mode === true) return 2;
  if (mode === "background") return 1;
  return 0;
}

function strongerSyncMode(a: DetailSyncMode, b: DetailSyncMode): DetailSyncMode {
  return syncIntentRank(b) > syncIntentRank(a) ? b : a;
}

function needsWorkflowApprovalSync(detail: PullDetail | null, enabled: boolean): boolean {
  if (!enabled || !detail) return false;
  const pr = detail.merge_request;
  return Boolean(
    detail.repo?.capabilities?.workflow_approval &&
    pr?.State === "open" &&
    detail.workflow_approval?.checked === false &&
    pr.CIHadPending,
  );
}

export function createDetailStore(opts: DetailStoreOptions) {
  const runtime = opts.runtime;
  const getPage = opts.getPage ?? (() => "");
  const onDetailSynchronized = opts.onDetailSynchronized ?? (() => {});
  const pullsDep = opts.pulls;
  const syncDep = opts.sync;

  // --- state ---

  // Detail envelopes are replaced immutably. Keeping the large event array raw
  // avoids proxying thousands of timeline objects that are never mutated in place.
  let detail = $state.raw<PullDetail | null>(null);
  // Lifecycle tick captured when the request that produced the current
  // envelope STARTED (not when it landed). Workspace-create reconciliation
  // compares it against a creation confirmation's tick to tell a stale
  // pre-create "no workspace" apart from an authoritative post-create one.
  // Reactive so reconcile effects rerun even when a refreshed envelope's
  // content is identical and `detail` itself is not reassigned.
  let detailEnvelopeTick = $state(0);
  let loading = $state(false);
  let syncing = $state(false);
  let storeError = $state<string | null>(null);
  let detailLoaded = $state(false);
  let syncGeneration = 0;
  let selectionGeneration = 0;
  let detailRequestSequence = 0;
  const latestSuccessfulDetailRequestSequenceBySelection = new Map<string, number>();
  let activeSelectionKey: string | null = null;
  // Tracks the PR (if any) whose local body has been edited since
  // the last server confirmation. While set, background sync paths
  // preserve the local body when applying refreshed server data for
  // THIS specific PR — a poll on a different PR is unaffected, and
  // navigating away doesn't strand the flag on the wrong target.
  type UnsavedTarget = {
    provider: string;
    platformHost: string | undefined;
    owner: string;
    name: string;
    number: number;
    body: string;
  };
  let unsavedLocalBody = $state<UnsavedTarget | null>(null);
  let activeLoad: {
    key: string;
    execution: AppExecution<void, DetailReadError> | null;
    syncMode: DetailSyncMode;
    workflowApprovalSync: boolean;
  } | null = null;
  // Latest detail_fetched_at seen in any server payload for the current
  // selection. The store's own detail_fetched_at intentionally freezes
  // while refreshes are content-identical, so background-sync convergence
  // must baseline against this value — the frozen store timestamp would
  // make the previous cycle's sync look like this cycle's completion and
  // end the convergence loop before the new sync has landed.
  let lastObservedFetchedAt: string | undefined;
  // Provider synchronization is eventually complete. Keep a successfully
  // deleted comment hidden locally until an ordinary sync no longer returns it.
  const hiddenDeletedCommentIDs: Record<string, number[]> = {};
  const pendingCommentMutationStates = new Map<
    number,
    { readonly baseline: PullCommentMutationState; readonly count: number }
  >();

  // --- polling ---

  let detailPollingGeneration = 0;
  let unsubSyncComplete: (() => void) | null = null;

  // --- reads ---

  function getDetail(): PullDetail | null {
    return detail;
  }

  function getDetailEnvelopeTick(): number {
    return detailEnvelopeTick;
  }

  function isDetailLoading(): boolean {
    return loading;
  }

  function isDetailSyncing(): boolean {
    return syncing;
  }

  function getDetailError(): string | null {
    return storeError;
  }

  function getDetailLoaded(): boolean {
    return detailLoaded;
  }

  // --- internal helpers ---

  function prKey(ref: DetailRequestRef): string {
    return providerItemKey({
      provider: ref.provider,
      platformHost: concretePlatformHost(ref),
      owner: ref.owner,
      name: ref.name,
      number: ref.number,
    });
  }

  function detailRequestRef(
    owner: string,
    name: string,
    number: number,
    options: DetailRequestOptions | DetailRequestRef,
  ): DetailRequestRef {
    return {
      owner,
      name,
      number,
      provider: options.provider,
      platformHost: options.platformHost,
      repoPath: options.repoPath,
    };
  }

  // Apply a fresh PullDetail from the server. When the user has an
  // unsynced local body edit on the same PR, keep that body so a
  // polling refresh can't revert a pending optimistic toggle. Match on
  // provider + platformHost too so an unrelated repo with the same
  // owner/name/number (different host or provider) doesn't inherit
  // another repo's pending body.
  function withPreservedLocalBody(next: PullDetail): PullDetail {
    next = withHiddenDeletedComments(next);
    if (!unsavedLocalBody) return next;
    if (!detail) return next;
    if (
      !sameBodyTarget(
        unsavedLocalBody.provider,
        unsavedLocalBody.platformHost,
        next.repo?.provider,
        next.repo?.platform_host,
      ) ||
      unsavedLocalBody.owner !== next.repo_owner ||
      unsavedLocalBody.name !== next.repo_name ||
      unsavedLocalBody.number !== next.merge_request?.Number
    ) {
      return next;
    }
    if (
      detail.repo_owner !== next.repo_owner ||
      detail.repo_name !== next.repo_name ||
      detail.merge_request?.Number !== next.merge_request?.Number
    ) {
      return next;
    }
    return {
      ...next,
      merge_request: {
        ...next.merge_request,
        Body: detail.merge_request.Body,
      },
    };
  }

  function withHiddenDeletedComments(next: PullDetail): PullDetail {
    if (Object.keys(hiddenDeletedCommentIDs).length === 0) return next;
    const key = providerItemKey({
      provider: next.repo.provider,
      platformHost: resolvedPlatformHost(next.repo.provider, next.repo.platform_host),
      owner: next.repo_owner,
      name: next.repo_name,
      number: next.merge_request.Number,
    });
    const hidden = hiddenDeletedCommentIDs[key];
    if (!hidden || hidden.length === 0) return next;
    const events = next.events ?? [];
    const stillHidden = hidden.filter((id) =>
      events.some((event) => event.EventType === "issue_comment" && event.PlatformID === id),
    );
    if (stillHidden.length === 0) {
      delete hiddenDeletedCommentIDs[key];
      return next;
    }
    hiddenDeletedCommentIDs[key] = stillHidden;
    return {
      ...next,
      events: events.filter(
        (event) =>
          event.EventType !== "issue_comment" || event.PlatformID === null || !stillHidden.includes(event.PlatformID),
      ),
    };
  }

  function hideDeletedComment(ref: DetailRequestRef, commentID: number): void {
    const key = prKey(ref);
    const hidden = hiddenDeletedCommentIDs[key] ?? [];
    if (!hidden.includes(commentID)) hiddenDeletedCommentIDs[key] = [...hidden, commentID];
    if (!isDetailShowingRef(ref) || !detail) return;
    detail = {
      ...detail,
      events: (detail.events ?? []).filter(
        (event) => event.EventType !== "issue_comment" || event.PlatformID !== commentID,
      ),
    };
  }

  function hasUnsavedLocalBody(): boolean {
    return unsavedLocalBody !== null;
  }

  // Callers pass route vocabulary (gh, omitted default host) while
  // payloads carry canonical values; every provider/host comparison
  // between the two must normalize both sides or a mutation on an
  // aliased route silently no-ops against its own current detail.
  function sameBodyTarget(
    aProvider: string | undefined,
    aHost: string | undefined,
    bProvider: string | undefined,
    bHost: string | undefined,
  ): boolean {
    const a = canonicalProvider(aProvider ?? "");
    const b = canonicalProvider(bProvider ?? "");
    return a === b && resolvedPlatformHost(a, aHost) === resolvedPlatformHost(b, bHost);
  }

  function isDetailShowingRef(ref: DetailRequestRef): boolean {
    return (
      detail !== null &&
      detail.repo_owner === ref.owner &&
      detail.repo_name === ref.name &&
      detail.merge_request.Number === ref.number &&
      sameBodyTarget(detail.repo?.provider, detail.repo?.platform_host, ref.provider, ref.platformHost) &&
      detail.repo?.repo_path === ref.repoPath
    );
  }

  function currentDetailRef(owner: string, name: string, number: number): DetailRequestRef {
    if (!detail?.repo?.provider || !detail.repo.repo_path) {
      throw new Error("pull detail missing provider repo identity");
    }
    return detailRequestRef(owner, name, number, {
      provider: detail.repo.provider,
      platformHost: detail.repo.platform_host,
      repoPath: detail.repo.repo_path,
    });
  }

  function concretePlatformHost(ref: Pick<DetailRequestRef, "provider" | "platformHost">): string {
    const host = ref.platformHost ?? providerDefaultHost(ref.provider);
    if (!host) throw new Error("pull detail missing platform host");
    return host;
  }

  function pullMutationKey(ref: DetailRequestRef, family: string): string {
    return providerMutationKey(
      "pull",
      {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number: ref.number,
      },
      family,
    );
  }

  function pullCommentState(
    events: ReadonlyArray<PREvent>,
    commentID: number,
    fallback: PullCommentMutationState,
  ): PullCommentMutationState {
    const index = events.findIndex((event) => event.EventType === "issue_comment" && event.PlatformID === commentID);
    const event = events[index];
    return index === -1 || event === undefined ? { ...fallback, present: false } : { event, index, present: true };
  }

  function applyPullCommentState(
    ref: DetailRequestRef,
    commentID: number,
    state: PullCommentMutationState,
  ): Effect.Effect<void> {
    return Effect.sync(() => {
      if (!isDetailShowingRef(ref) || detail === null) return;
      const events = [...(detail.events ?? [])];
      const currentIndex = events.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      if (!state.present) {
        if (currentIndex !== -1) events.splice(currentIndex, 1);
      } else if (currentIndex === -1) {
        events.splice(Math.min(state.index, events.length), 0, state.event);
      } else {
        events[currentIndex] = state.event;
      }
      detail = { ...detail, events };
    });
  }

  function trackPullCommentMutation(commentID: number, baseline: PullCommentMutationState): void {
    const current = pendingCommentMutationStates.get(commentID);
    pendingCommentMutationStates.set(commentID, {
      baseline: current?.baseline ?? baseline,
      count: (current?.count ?? 0) + 1,
    });
  }

  function releasePullCommentMutation(commentID: number): void {
    const current = pendingCommentMutationStates.get(commentID);
    if (current === undefined || current.count === 1) {
      pendingCommentMutationStates.delete(commentID);
      return;
    }
    pendingCommentMutationStates.set(commentID, { ...current, count: current.count - 1 });
  }

  function rebasePullMutations(ref: DetailRequestRef, authoritative: PullDetail, installEnvelope: () => boolean) {
    return Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const labels = authoritative.merge_request.labels ?? [];
      const rebaseEntries: Array<{ readonly key: string; readonly confirmed: Effect.Effect<void> }> = [
        {
          key: pullMutationKey(ref, "labels"),
          confirmed: Effect.sync(() => {
            if (isDetailShowingRef(ref) && detail !== null) {
              detail = { ...detail, merge_request: { ...detail.merge_request, labels } };
            }
          }),
        },
      ];
      const assignees = authoritative.merge_request.assignees ?? [];
      rebaseEntries.push({
        key: pullMutationKey(ref, "assignees"),
        confirmed: Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail !== null) {
            detail = { ...detail, merge_request: { ...detail.merge_request, assignees } };
          }
        }),
      });
      const reviewers = authoritative.merge_request.requested_reviewers ?? [];
      rebaseEntries.push({
        key: pullMutationKey(ref, "reviewers"),
        confirmed: Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail !== null) {
            detail = { ...detail, merge_request: { ...detail.merge_request, requested_reviewers: reviewers } };
          }
        }),
      });
      const content = {
        title: authoritative.merge_request.Title,
        body: authoritative.merge_request.Body,
      };
      rebaseEntries.push({
        key: pullMutationKey(ref, "content"),
        confirmed: Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail !== null) {
            detail = {
              ...detail,
              merge_request: {
                ...detail.merge_request,
                Title: content.title,
                Body: content.body,
              },
            };
          }
        }),
      });
      const kanban = normalizeKanbanStatus(authoritative.merge_request.KanbanStatus);
      rebaseEntries.push({
        key: pullMutationKey(ref, "kanban"),
        confirmed: Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail !== null) {
            detail = { ...detail, merge_request: { ...detail.merge_request, KanbanStatus: kanban } };
          }
          pullsDep?.optimisticKanbanUpdate?.(ref, ref.number, kanban);
        }),
      });
      const starred = Boolean(authoritative.merge_request.Starred);
      rebaseEntries.push({
        key: pullMutationKey(ref, "star"),
        confirmed: Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail !== null) {
            detail = { ...detail, merge_request: { ...detail.merge_request, Starred: starred } };
          }
          pullsDep?.optimisticStarUpdate?.(ref, ref.number, starred);
        }),
      });
      for (const [commentID, tracked] of pendingCommentMutationStates) {
        const state = pullCommentState(authoritative.events ?? [], commentID, tracked.baseline);
        rebaseEntries.push({
          key: pullMutationKey(ref, `comment\u0000${commentID}`),
          confirmed: applyPullCommentState(ref, commentID, state),
        });
      }
      return yield* mutations.rebaseAll(Effect.sync(installEnvelope), rebaseEntries);
    });
  }

  function refreshPullsIfActive(): void {
    if (["pulls", "mobile-pulls", "focus"].includes(getPage()) && pullsDep) {
      pullsDep.loadPulls();
    }
  }

  function reconcileListsAfterDetailSync(): void {
    refreshPullsIfActive();
    onDetailSynchronized();
  }

  function runPullAction(
    routeRef: ProviderRouteRef,
    number: number,
    operation: string,
    commit: (ref: DetailRequestRef) => Effect.Effect<void, ProviderMutationError, GeneratedApi>,
    callbacks: ProviderActionCallbacks,
  ): void {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    if (!isDetailShowingRef(ref)) {
      const message = "The selected pull request changed before the provider action started.";
      invokeMutationFailure(callbacks.onFailure, message);
      try {
        callbacks.onSettled?.();
      } catch {
        // Presentation callbacks do not own command acceptance.
      }
      return;
    }
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      yield* mutations.submit({
        key: pullMutationKey(ref, "actions"),
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit: commit(ref),
        refreshOnStale: executeGeneratedApiRequest(`sync after ${operation}`, (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(Effect.asVoid),
      });
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isDetailShowingRef(ref);
      });
      yield* Effect.sync(refreshPullsIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) yield* refreshDetailOnlyEffect(ref.owner, ref.name, number, ref);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation,
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("The provider action completed, but the latest pull request could not be refreshed.", {
            tone: "danger",
          });
          return;
        }
        const message = providerMutationFailureMessage(failure, `failed to ${operation}`);
        if (callbacks.onFailure === undefined) showFlash(message, { tone: "danger" });
        const problem = providerMutationProblem(failure);
        if (problem !== undefined) {
          try {
            callbacks.onProblem?.(problem);
          } catch {
            // Presentation callbacks must not change the mutation acknowledgement.
          }
        }
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function approvePull(
    ref: ProviderRouteRef,
    number: number,
    input: ApprovePRInputBody,
    callbacks: ProviderActionCallbacks = {},
  ): void {
    runPullAction(
      ref,
      number,
      "approve pull request",
      (ref) =>
        executeGeneratedApiRequest("POST approve pull request", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.approvePullOnHost({ ...providerHostRouteParams(ref), number: number }, input, {
                signal,
              })
            : client.PullRequestsService.approvePull({ ...providerRouteParams(ref), number: number }, input, {
                signal,
              }),
        ).pipe(Effect.asVoid),
      callbacks,
    );
  }

  function markPullReady(ref: ProviderRouteRef, number: number, callbacks: ProviderActionCallbacks = {}): void {
    runPullAction(
      ref,
      number,
      "mark pull request ready for review",
      (ref) =>
        executeGeneratedApiRequest("POST mark pull request ready for review", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.markPullReadyForReviewOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { signal },
              )
            : client.PullRequestsService.markPullReadyForReview(
                { ...providerRouteParams(ref), number: number },
                { signal },
              ),
        ).pipe(Effect.asVoid),
      callbacks,
    );
  }

  function requestPullChanges(
    ref: ProviderRouteRef,
    number: number,
    input: RequestChangesPRInputBody,
    callbacks: ProviderActionCallbacks = {},
  ): void {
    runPullAction(
      ref,
      number,
      "request changes on pull request",
      (ref) =>
        executeGeneratedApiRequest("POST request changes on pull request", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.requestPullChangesOnHost(
                { ...providerHostRouteParams(ref), number: number },
                input,
                { signal },
              )
            : client.PullRequestsService.requestPullChanges({ ...providerRouteParams(ref), number: number }, input, {
                signal,
              }),
        ).pipe(Effect.asVoid),
      callbacks,
    );
  }

  function approvePullWorkflows(ref: ProviderRouteRef, number: number, callbacks: ProviderActionCallbacks = {}): void {
    runPullAction(
      ref,
      number,
      "approve pull request workflows",
      (ref) =>
        executeGeneratedApiRequest("POST approve pull request workflows", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.approvePullWorkflowsOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { signal },
              )
            : client.PullRequestsService.approvePullWorkflows(
                { ...providerRouteParams(ref), number: number },
                { signal },
              ),
        ).pipe(Effect.asVoid),
      callbacks,
    );
  }

  function mergePull(
    ref: ProviderRouteRef,
    number: number,
    input: MergeParams,
    deferred: boolean,
    callbacks: MergePullCallbacks = {},
  ): void {
    let workspaceCleanupWarning: string | undefined;
    const commit = (ref: DetailRequestRef) =>
      deferred
        ? executeGeneratedApiRequest("POST deferred pull request merge", (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.PullRequestsService.deferMergePullOnHost(
                  { ...providerHostRouteParams(ref), number: number },
                  input,
                  { signal },
                )
              : client.PullRequestsService.deferMergePull({ ...providerRouteParams(ref), number: number }, input, {
                  signal,
                }),
          ).pipe(Effect.asVoid)
        : executeGeneratedApiRequest("POST pull request merge", (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.PullRequestsService.mergePullOnHost({ ...providerHostRouteParams(ref), number: number }, input, {
                  signal,
                })
              : client.PullRequestsService.mergePull({ ...providerRouteParams(ref), number: number }, input, {
                  signal,
                }),
          ).pipe(
            Effect.flatMap((result) => {
              if (!result.merged) {
                return Effect.fail(
                  new ApiProblemError({
                    operation: "POST pull request merge",
                    problem: {
                      code: ProblemCodes.conflict,
                      detail: result.message || "The pull request could not be merged.",
                      details: { reason: "conflict" },
                      status: 409,
                      title: "Merge did not complete",
                      type: "about:blank",
                    },
                  }),
                );
              }
              return Effect.sync(() => {
                workspaceCleanupWarning = result.workspace_cleanup_warning;
              });
            }),
            Effect.asVoid,
          );
    runPullAction(ref, number, deferred ? "schedule pull request merge" : "merge pull request", commit, {
      ...callbacks,
      onSuccess: () => {
        if (workspaceCleanupWarning) {
          showFlash(`Pull request merged, but the workspace was not pruned: ${workspaceCleanupWarning}`, {
            tone: "warning",
          });
        }
        callbacks.onSuccess?.(
          deferred
            ? { _tag: "Queued" }
            : workspaceCleanupWarning === undefined
              ? { _tag: "Merged" }
              : { _tag: "Merged", cleanupWarning: workspaceCleanupWarning },
        );
      },
    });
  }

  // A refreshed payload whose content matches the displayed detail must
  // not replace it: the sync timestamp moves on every background poll,
  // and swapping in an equal-but-new object re-rendered the whole PR
  // panel (and re-ran the scroll-restore effect) every polling cycle
  // even though nothing about the PR changed.
  function detailContentUnchanged(next: PullDetail): boolean {
    if (detail === null) return false;
    // Only the fetch timestamp is volatile-by-design; everything else —
    // including warnings, which the PR panel renders — is content.
    const strip = (d: PullDetail): string => {
      const { detail_fetched_at: _fetchedAt, ...rest } = d;
      return JSON.stringify(rest);
    };
    return strip(detail) === strip(next);
  }

  function applyRefreshedDetail(next: PullDetail): void {
    if (detailContentUnchanged(next)) return;
    detail = next;
  }

  // Applies an envelope payload and its request-start tick atomically. A
  // response whose request started before the currently applied envelope's
  // request is stale: applying its payload while the newer tick stands
  // would let pre-creation "no workspace" data masquerade as authoritative.
  // Rejecting it keeps last-started-wins consistent across every
  // detail-producing path, including sync and PATCH responses that lack
  // the GET paths' per-selection sequence guards.
  function applyEnvelopeAt(envelopeTick: number, assign: () => void): boolean {
    if (envelopeTick < detailEnvelopeTick) return false;
    assign();
    detailEnvelopeTick = envelopeTick;
    return true;
  }

  function noteObservedFetchedAt(fetchedAt: string | null | undefined): void {
    if (fetchedAt != null) lastObservedFetchedAt = fetchedAt;
  }

  function observedFetchedAtBaseline(): string | undefined {
    return lastObservedFetchedAt ?? detail?.detail_fetched_at;
  }

  function failClosedAfterApplySuggestionConflict(reason: Exclude<ConflictReason, "conflict">): void {
    if (!detail) return;
    if (reason === "not_open") {
      detail = {
        ...detail,
        merge_request: {
          ...detail.merge_request,
          State: "closed",
        },
      };
      return;
    }
    detail = {
      ...detail,
      platform_head_sha: "",
    };
  }

  // --- writes ---

  function clearDetail(): void {
    ++syncGeneration;
    ++selectionGeneration;
    activeSelectionKey = null;
    activeLoad = null;
    latestSuccessfulDetailRequestSequenceBySelection.clear();
    detail = null;
    loading = false;
    syncing = false;
    storeError = null;
    detailLoaded = false;
    unsavedLocalBody = null;
    lastObservedFetchedAt = undefined;
    runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* DetailWorkflow;
        yield* workflow.clear;
      }),
      {
        operation: "clear pull request detail",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function startDetailLoad(owner: string, name: string, number: number, options: DetailRequestOptions): void {
    const syncMode = options.sync ?? true;
    const requestRef = detailRequestRef(owner, name, number, options);
    // Dedup by item identity only. A second caller with a different
    // sync mode joins the in-flight load and may promote the sync
    // intent if its requested mode is stronger.
    const key = prKey(requestRef);
    if (activeSelectionKey !== key) {
      activeSelectionKey = key;
      ++selectionGeneration;
      // The observed-timestamp baseline belongs to the previous selection;
      // carrying it over would let another PR's sync clock gate this one.
      lastObservedFetchedAt = undefined;
    }
    if (loading && activeLoad?.key === key && activeLoad.execution !== null) {
      activeLoad.syncMode = strongerSyncMode(activeLoad.syncMode, syncMode);
      activeLoad.workflowApprovalSync ||= options.workflowApprovalSync ?? true;
      return;
    }

    const gen = ++syncGeneration;
    const currentLoad: {
      key: string;
      execution: AppExecution<void, DetailReadError> | null;
      syncMode: DetailSyncMode;
      workflowApprovalSync: boolean;
    } = {
      key,
      execution: null,
      syncMode,
      workflowApprovalSync: options.workflowApprovalSync ?? true,
    };
    activeLoad = currentLoad;

    // Keep the previously loaded detail visible while the new one
    // is in flight. Nulling `detail` here flipped consumers to a
    // "Loading…" empty state for every prop change, which produced
    // a visible flash when, for example, the workspace right
    // sidebar updates from one PR to another. Consumers that need
    // a "first load" empty state should check `detail === null`
    // alongside `loading`.
    loading = true;
    syncing = false;
    storeError = null;
    detailLoaded = false;
    const envelopeTick = nextWorkspaceLifecycleTick();
    const read = executeGeneratedApiRequest("GET pull request", (client, signal) =>
      providerUsesHostRoute(requestRef)
        ? client.PullRequestsService.getPullOnHost(
            { ...providerHostRouteParams(requestRef), number: requestRef.number },
            { signal },
          )
        : client.PullRequestsService.getPull(
            { ...providerRouteParams(requestRef), number: requestRef.number },
            { signal },
          ),
    ).pipe(
      retryIdempotentRead,
      Effect.map((data): PullDetail => ({ ...data, events: data.events ?? [] })),
    );
    const program = Effect.gen(function* () {
      const workflow = yield* DetailWorkflow;
      const initialRead = yield* Effect.result(workflow.read(key, read));
      let recoveredBySync = false;
      let data: PullDetail;
      if (Result.isSuccess(initialRead)) {
        data = initialRead.success;
      } else if (
        currentLoad.syncMode !== false &&
        initialRead.failure._tag === "ApiProblemError" &&
        initialRead.failure.problem.code === ProblemCodes.pullNotFound
      ) {
        data = yield* executeGeneratedApiRequest("POST synchronize missing pull request", (client, signal) =>
          providerUsesHostRoute(requestRef)
            ? client.PullRequestsService.syncPullOnHost(
                { ...providerHostRouteParams(requestRef), number: requestRef.number },
                { signal },
              )
            : client.PullRequestsService.syncPull(
                { ...providerRouteParams(requestRef), number: requestRef.number },
                { signal },
              ),
        ).pipe(Effect.map((synced): PullDetail => ({ ...synced, events: synced.events ?? [] })));
        recoveredBySync = true;
      } else {
        return yield* Effect.fail(initialRead.failure);
      }
      reconcileListsAfterDetailSync();
      if (gen !== syncGeneration) return;
      const applied = yield* rebasePullMutations(requestRef, data, () => {
        if (gen !== syncGeneration || activeSelectionKey !== key) return false;
        const didApply = applyEnvelopeAt(envelopeTick, () => {
          detail = withPreservedLocalBody(data);
          detailLoaded = data.detail_loaded;
        });
        if (didApply) noteObservedFetchedAt(data.detail_fetched_at);
        return didApply;
      });
      if (applied && !recoveredBySync) {
        const finalSyncMode = currentLoad.syncMode;
        if (finalSyncMode === true) {
          launchDetailSync(owner, name, number, requestRef);
        } else if (finalSyncMode === "background") {
          if (needsWorkflowApprovalSync(detail, currentLoad.workflowApprovalSync)) {
            launchDetailSync(owner, name, number, requestRef);
          } else {
            launchBackgroundDetailSync(owner, name, number, gen, observedFetchedAtBaseline(), requestRef);
          }
        }
      }
    }).pipe(
      Effect.ensuring(
        Effect.sync(() => {
          if (gen === syncGeneration) loading = false;
          if (activeLoad === currentLoad) activeLoad = null;
        }),
      ),
    );
    const execution = runtime.runCommand(program, {
      operation: "load pull request detail",
      safeContext: {
        provider: requestRef.provider,
        platformHost: concretePlatformHost(requestRef),
        owner,
        name,
        number,
      },
      onFailure: (failure) => {
        if (gen !== syncGeneration) return;
        storeError = detailReadErrorMessage(failure);
      },
    });
    currentLoad.execution = execution;
  }

  function loadDetail(owner: string, name: string, number: number, options: DetailRequestOptions): void {
    startDetailLoad(owner, name, number, options);
  }

  function enqueueBackgroundDetailSyncEffect(
    owner: string,
    name: string,
    number: number,
    gen: number,
    previousFetchedAt: string | undefined,
    identity: DetailRequestRef,
  ): Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations> {
    return Effect.suspend(() => {
      const ref = detailRequestRef(owner, name, number, identity);
      if (gen === syncGeneration) syncing = true;
      return executeGeneratedApiRequest("POST asynchronous pull request synchronization", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.enqueuePrSyncOnHost(
              { ...providerHostRouteParams(ref), number: ref.number },
              { signal },
            )
          : client.PullRequestsService.enqueuePrSync({ ...providerRouteParams(ref), number: ref.number }, { signal }),
      ).pipe(
        Effect.andThen(refreshAfterBackgroundDetailSyncEffect(owner, name, number, gen, previousFetchedAt, ref)),
        Effect.ensuring(
          Effect.sync(() => {
            if (gen === syncGeneration) syncing = false;
            syncDep?.refreshSyncStatus?.();
          }),
        ),
      );
    });
  }

  function refreshAfterBackgroundDetailSyncEffect(
    owner: string,
    name: string,
    number: number,
    gen: number,
    previousFetchedAt: string | undefined,
    identity: DetailRequestRef,
  ): Effect.Effect<void, never, GeneratedApi | ProviderMutations> {
    return Effect.gen(function* () {
      for (const ms of [300, 700, 1_500, 3_000, 5_000]) {
        yield* Effect.sleep(ms);
        // Convergence is judged on the response's fetch timestamp, not the
        // store's: content-identical refreshes intentionally leave the
        // store timestamp untouched while the server has advanced.
        const result = yield* Effect.result(refreshDetailOnlyEffect(owner, name, number, identity, gen, true));
        if (Result.isSuccess(result) && result.success.fetchedAt && result.success.fetchedAt !== previousFetchedAt) {
          reconcileListsAfterDetailSync();
          return;
        }
      }
    });
  }

  function launchBackgroundDetailSync(
    owner: string,
    name: string,
    number: number,
    gen: number,
    previousFetchedAt: string | undefined,
    identity: DetailRequestRef,
  ): void {
    runtime.runCommand(enqueueBackgroundDetailSyncEffect(owner, name, number, gen, previousFetchedAt, identity), {
      operation: "synchronize pull request detail in background",
      safeContext: { owner, name, number },
      onFailure: () => {},
    });
  }

  function refreshDetailOnly(
    owner: string,
    name: string,
    number: number,
    identity: DetailRequestOptions,
    callbacks: DetailRefreshCallbacks = {},
  ): void {
    const program = refreshDetailOnlyEffect(owner, name, number, identity).pipe(
      Effect.tap(() => invokeMutationCallback(callbacks.onSuccess)),
      Effect.ensuring(invokeMutationCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "refresh pull request detail",
      safeContext: { owner, name, number },
      onFailure: (failure) => invokeMutationFailure(callbacks.onFailure, detailReadErrorMessage(failure)),
    });
  }

  function refreshDetailOnlyEffect(
    owner: string,
    name: string,
    number: number,
    identity: DetailRequestOptions,
    expectedGeneration = syncGeneration,
    observeStaleSuccess = false,
  ): Effect.Effect<DetailRefreshResult, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations> {
    return Effect.suspend(() => {
      const ref = detailRequestRef(owner, name, number, identity);
      const key = prKey(ref);
      const requestSequence = ++detailRequestSequence;
      const envelopeTick = nextWorkspaceLifecycleTick();
      const ownership = (): "current" | "irrelevant" | "superseded" => {
        if (activeSelectionKey !== key) return "irrelevant";
        if (
          expectedGeneration !== syncGeneration ||
          requestSequence < (latestSuccessfulDetailRequestSequenceBySelection.get(key) ?? 0)
        ) {
          return "superseded";
        }
        return "current";
      };
      const superseded = () =>
        TransientTransportError.make({
          operation: "reconcile pull request detail after superseded provider event",
          cause: new Error("a foreground detail read replaced event reconciliation"),
        });
      const read = executeGeneratedApiRequest("GET pull request detail after provider event", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.getPullOnHost(
              { ...providerHostRouteParams(ref), number: ref.number },
              { signal },
            )
          : client.PullRequestsService.getPull({ ...providerRouteParams(ref), number: ref.number }, { signal }),
      );
      return read.pipe(
        Effect.matchEffect({
          onFailure: (failure) =>
            Effect.sync(ownership).pipe(
              Effect.flatMap((status) =>
                status === "current"
                  ? Effect.fail(failure)
                  : status === "superseded"
                    ? Effect.fail(superseded())
                    : Effect.succeed<DetailRefreshResult>({ applied: false }),
              ),
            ),
          onSuccess: (data) =>
            Effect.gen(function* () {
              const status = ownership();
              const observed = {
                applied: false,
                ...(data.detail_fetched_at != null && { fetchedAt: data.detail_fetched_at }),
              };
              if (status === "irrelevant" || (status === "superseded" && observeStaleSuccess)) return observed;
              if (status === "superseded") return yield* Effect.fail(superseded());
              latestSuccessfulDetailRequestSequenceBySelection.set(key, requestSequence);
              const next: PullDetail = { ...data, events: data.events ?? [] };
              let supersededWhileInstalling = false;
              const applied = yield* rebasePullMutations(ref, next, () => {
                const installStatus = ownership();
                if (installStatus === "irrelevant") return false;
                if (installStatus === "superseded") {
                  supersededWhileInstalling = true;
                  return false;
                }
                const didApply = applyEnvelopeAt(envelopeTick, () => {
                  applyRefreshedDetail(withPreservedLocalBody(next));
                  detailLoaded = data.detail_loaded ?? detailLoaded;
                });
                if (didApply) noteObservedFetchedAt(data.detail_fetched_at);
                return didApply;
              });
              if (supersededWhileInstalling) return yield* Effect.fail(superseded());
              return {
                applied,
                ...(data.detail_fetched_at != null && { fetchedAt: data.detail_fetched_at }),
              };
            }),
        }),
      );
    });
  }

  function syncDetailEffect(
    owner: string,
    name: string,
    number: number,
    identity: DetailRequestOptions,
  ): Effect.Effect<boolean, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations> {
    return Effect.suspend(() => {
      const ref = detailRequestRef(owner, name, number, identity);
      const expectedGeneration = syncGeneration;
      const envelopeTick = nextWorkspaceLifecycleTick();
      const key = prKey(ref);
      const isCurrent = () => expectedGeneration === syncGeneration && activeSelectionKey === key;
      if (isCurrent()) syncing = true;
      return executeGeneratedApiRequest("POST synchronize pull request detail", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.syncPullOnHost(
              { ...providerHostRouteParams(ref), number: ref.number },
              { signal },
            )
          : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: ref.number }, { signal }),
      ).pipe(
        Effect.map((data): PullDetail => ({ ...data, events: data.events ?? [] })),
        Effect.tap(() => Effect.sync(reconcileListsAfterDetailSync)),
        Effect.flatMap((next) =>
          Effect.gen(function* () {
            const applied = yield* rebasePullMutations(ref, next, () => {
              if (!isCurrent()) return false;
              storeError = null;
              const didApply = applyEnvelopeAt(envelopeTick, () => {
                applyRefreshedDetail(withPreservedLocalBody(next));
                detailLoaded = next.detail_loaded ?? detailLoaded;
              });
              if (didApply) noteObservedFetchedAt(next.detail_fetched_at);
              return didApply;
            });
            return applied;
          }),
        ),
        Effect.ensuring(
          Effect.sync(() => {
            if (expectedGeneration === syncGeneration) syncing = false;
            syncDep?.refreshSyncStatus?.();
          }),
        ),
      );
    });
  }

  function launchDetailSync(owner: string, name: string, number: number, identity: DetailRequestOptions): void {
    runtime.runCommand(syncDetailEffect(owner, name, number, identity), {
      operation: "synchronize pull request detail",
      safeContext: { owner, name, number },
      onFailure: () => {},
    });
  }

  function syncDetailNow(
    owner: string,
    name: string,
    number: number,
    identity: DetailRequestOptions,
    callbacks: DetailSyncCallbacks = {},
  ): void {
    const ref = detailRequestRef(owner, name, number, identity);
    activeLoad = null;
    activeSelectionKey = prKey(ref);
    syncGeneration += 1;
    const program = syncDetailEffect(owner, name, number, ref).pipe(
      Effect.tap((refreshed) => invokeDetailSyncSuccess(callbacks.onSuccess, refreshed)),
      Effect.ensuring(invokeMutationCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "synchronize pull request detail",
      safeContext: { owner, name, number },
      onFailure: (failure) => invokeMutationFailure(callbacks.onFailure, detailReadErrorMessage(failure)),
    });
  }

  function refreshPendingCI(
    owner: string,
    name: string,
    number: number,
    identity: DetailRequestOptions,
    callbacks: DetailRefreshCallbacks = {},
  ): void {
    const ref = detailRequestRef(owner, name, number, identity);
    if (syncing || !isDetailShowingRef(ref)) {
      callbacks.onSettled?.();
      return;
    }
    const key = prKey(ref);
    const gen = syncGeneration;
    const envelopeTick = nextWorkspaceLifecycleTick();
    if (gen === syncGeneration) syncing = true;
    const request = executeGeneratedApiRequest("POST refresh pull request CI checks", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.PullRequestsService.refreshPullCiOnHost(
            { ...providerHostRouteParams(ref), number: ref.number },
            { signal },
          )
        : client.PullRequestsService.refreshPullCi({ ...providerRouteParams(ref), number: ref.number }, { signal }),
    ).pipe(Effect.map((data): PullDetail => ({ ...data, events: data.events ?? [] })));
    const program = Effect.gen(function* () {
      const workflow = yield* DetailWorkflow;
      const next = yield* workflow.refreshCI(key, request);
      if (gen !== syncGeneration) return;
      yield* rebasePullMutations(ref, next, () => {
        if (gen !== syncGeneration || !isDetailShowingRef(ref)) return false;
        storeError = null;
        const didApply = applyEnvelopeAt(envelopeTick, () => {
          applyRefreshedDetail(withPreservedLocalBody(next));
          detailLoaded = next.detail_loaded ?? detailLoaded;
        });
        if (didApply) {
          noteObservedFetchedAt(next.detail_fetched_at);
          const warning = next.warnings?.[0];
          if (warning) showFlash(warning, { tone: "warning" });
        }
        return didApply;
      });
      if (needsWorkflowApprovalSync(detail, identity.workflowApprovalSync ?? true)) {
        yield* syncDetailEffect(owner, name, number, ref);
      }
      if (gen === syncGeneration) refreshPullsIfActive();
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          yield* Effect.sync(() => {
            if (gen === syncGeneration) syncing = false;
            syncDep?.refreshSyncStatus?.();
          });
          yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "refresh pull request CI checks",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = detailReadErrorMessage(failure);
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function updateKanbanState(routeRef: ProviderRouteRef, number: number, status: KanbanStatus): void {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    const prevDetailStatus =
      isDetailShowingRef(ref) && detail !== null ? normalizeKanbanStatus(detail.merge_request.KanbanStatus) : undefined;
    const prevPullsStatus = pullsDep?.getPullKanbanStatus?.(ref, number);
    type KanbanProjection = {
      readonly detail?: KanbanStatus;
      readonly pulls?: KanbanStatus;
    };
    let mutationSettled = false;
    let mutationTick = 0;
    const apply = (projection: KanbanProjection) =>
      Effect.sync(() => {
        if (projection.detail !== undefined && isDetailShowingRef(ref) && detail !== null) {
          detail = {
            ...detail,
            merge_request: { ...detail.merge_request, KanbanStatus: projection.detail },
          };
          detailEnvelopeTick = Math.max(detailEnvelopeTick, mutationTick);
        }
        if (projection.pulls !== undefined) {
          pullsDep?.optimisticKanbanUpdate?.(ref, number, projection.pulls);
        }
      });
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      mutationTick = nextWorkspaceLifecycleTick();
      const commit = executeGeneratedApiRequest<void>("PUT pull request kanban state", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.setKanbanStateOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { status },
              { signal },
            )
          : client.PullRequestsService.setKanbanState(
              { ...providerRouteParams(ref), number: number },
              { status },
              { signal },
            ),
      ).pipe(Effect.as<KanbanProjection>({ detail: status, pulls: status }));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale kanban mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(
        Effect.map((next): KanbanProjection => {
          const confirmed = normalizeKanbanStatus(next.merge_request.KanbanStatus);
          return { detail: confirmed, pulls: confirmed };
        }),
      );
      yield* mutations.submit({
        key: pullMutationKey(ref, "kanban"),
        baseline: {
          ...(prevDetailStatus !== undefined && { detail: prevDetailStatus }),
          ...(prevPullsStatus !== undefined && { pulls: prevPullsStatus }),
        },
        optimistic: {
          ...(prevDetailStatus !== undefined && { detail: status }),
          pulls: status,
        },
        apply,
        commit,
        refreshOnStale,
      });
      mutationSettled = true;
      refreshPullsIfActive();
      if (isDetailShowingRef(ref)) {
        yield* refreshDetailOnlyEffect(ref.owner, ref.name, number, ref);
      }
    });
    runtime.runCommand(program, {
      operation: "update pull request kanban state",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Kanban state was updated, but the latest pull request could not be refreshed.", {
            tone: "danger",
          });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to update kanban state");
        showFlash(message, { tone: "danger" });
        pullsDep?.loadPulls();
        if (isDetailShowingRef(ref)) loadDetail(ref.owner, ref.name, number, ref);
      },
    });
  }

  function setPullState(
    routeRef: ProviderRouteRef,
    number: number,
    state: GithubStateInputBody["state"],
    callbacks: ProviderActionCallbacks = {},
  ): void {
    runPullAction(
      routeRef,
      number,
      "change pull request state",
      (ref) =>
        executeGeneratedApiRequest("POST pull request state", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.setPrGithubStateOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { state },
                { signal },
              )
            : client.PullRequestsService.setPrGithubState(
                { ...providerRouteParams(ref), number: number },
                { state },
                { signal },
              ),
        ).pipe(Effect.asVoid),
      callbacks,
    );
  }

  function setPullLabels(
    owner: string,
    name: string,
    number: number,
    labels: Label[],
    callbacks: MutationCallbacks = {},
  ): void {
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "update pull request labels", cause }),
      });
      const key = `pull\u0000${providerItemKey({
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      })}\u0000labels`;
      const previousLabels = detail?.merge_request.labels ?? [];
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PUT pull request labels", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.setPrLabelsOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { labels: labels.map((label) => label.name) },
              { signal },
            )
          : client.PullRequestsService.setPrLabels(
              { ...providerRouteParams(ref), number: number },
              { labels: labels.map((label) => label.name) },
              { signal },
            ),
      ).pipe(Effect.map((response) => response.labels ?? []));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale label mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.merge_request.labels ?? []));
      const apply = (nextLabels: Label[]) =>
        Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail) {
            detail = { ...detail, merge_request: { ...detail.merge_request, labels: nextLabels } };
          }
        });
      yield* mutations.submit({
        key,
        baseline: previousLabels,
        optimistic: labels,
        apply,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(refreshPullsIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));

    runtime.runCommand(program, {
      operation: "update pull request labels",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update labels");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function setPullAssignees(
    owner: string,
    name: string,
    number: number,
    assignees: string[],
    callbacks: MutationCallbacks = {},
  ): void {
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "update pull request assignees", cause }),
      });
      const key = `pull\u0000${providerItemKey({
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      })}\u0000assignees`;
      const previousAssignees = detail?.merge_request.assignees ?? [];
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PUT pull request assignees", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.setPrAssigneesOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { assignees },
              { signal },
            )
          : client.PullRequestsService.setPrAssignees(
              { ...providerRouteParams(ref), number: number },
              { assignees },
              { signal },
            ),
      ).pipe(Effect.map((response) => response.assignees ?? []));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale assignee mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.merge_request.assignees ?? []));
      const apply = (nextAssignees: string[]) =>
        Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail) {
            detail = {
              ...detail,
              merge_request: { ...detail.merge_request, assignees: nextAssignees },
            };
          }
        });
      yield* mutations.submit({
        key,
        baseline: previousAssignees,
        optimistic: assignees,
        apply,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(refreshPullsIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));

    runtime.runCommand(program, {
      operation: "update pull request assignees",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update assignees");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function setPullReviewers(
    owner: string,
    name: string,
    number: number,
    reviewers: string[],
    callbacks: MutationCallbacks = {},
  ): void {
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "update pull request reviewers", cause }),
      });
      const key = `pull\u0000${providerItemKey({
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      })}\u0000reviewers`;
      const previousReviewers = detail?.merge_request.requested_reviewers ?? [];
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PUT pull request reviewers", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.setPrReviewersOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { reviewers },
              { signal },
            )
          : client.PullRequestsService.setPrReviewers(
              { ...providerRouteParams(ref), number: number },
              { reviewers },
              { signal },
            ),
      ).pipe(Effect.map((response) => response.reviewers ?? []));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale reviewer mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.merge_request.requested_reviewers ?? []));
      const apply = (nextReviewers: string[]) =>
        Effect.sync(() => {
          if (isDetailShowingRef(ref) && detail) {
            detail = {
              ...detail,
              merge_request: { ...detail.merge_request, requested_reviewers: nextReviewers },
            };
          }
        });
      yield* mutations.submit({
        key,
        baseline: previousReviewers,
        optimistic: reviewers,
        apply,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(refreshPullsIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));

    runtime.runCommand(program, {
      operation: "update pull request reviewers",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update reviewers");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  type PRContentProjection = {
    readonly title?: string;
    readonly body?: string;
  };

  type PreparedPRContentUpdate = {
    readonly ref: DetailRequestRef;
    readonly state: { settled: boolean };
    readonly program: Effect.Effect<void, ProviderMutationFailure, ProviderMutations>;
  };

  function preparePRContentUpdate(
    routeRef: ProviderRouteRef,
    number: number,
    fields: EditPRContentInputBody,
    callbacks: MutationCallbacks = {},
    requireVisible: boolean,
  ): PreparedPRContentUpdate | undefined {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    const visibleDetail = isDetailShowingRef(ref) ? detail : null;
    if (requireVisible && visibleDetail === null) return undefined;
    const baseline: PRContentProjection = {
      ...(fields.title !== undefined && { title: visibleDetail?.merge_request.Title ?? fields.title }),
      ...(fields.body !== undefined && { body: visibleDetail?.merge_request.Body ?? fields.body }),
    };
    const optimistic: PRContentProjection = { ...fields };
    let mutationTick = 0;
    const apply = (projection: PRContentProjection) =>
      Effect.sync(() => {
        if (!isDetailShowingRef(ref) || detail === null) return;
        const projectedBody =
          projection.body !== undefined &&
          unsavedLocalBody !== null &&
          sameBodyTarget(unsavedLocalBody.provider, unsavedLocalBody.platformHost, ref.provider, ref.platformHost) &&
          unsavedLocalBody.owner === ref.owner &&
          unsavedLocalBody.name === ref.name &&
          unsavedLocalBody.number === number &&
          unsavedLocalBody.body !== projection.body
            ? unsavedLocalBody.body
            : projection.body;
        detail = {
          ...detail,
          merge_request: {
            ...detail.merge_request,
            ...(projection.title !== undefined && { Title: projection.title }),
            ...(projectedBody !== undefined && { Body: projectedBody }),
          },
        };
        detailEnvelopeTick = Math.max(detailEnvelopeTick, mutationTick);
      });
    const state = { settled: false };
    let confirmedDetail: { readonly detail: PullDetail; readonly envelopeTick: number } | undefined;
    let acknowledgedUnsavedTarget: UnsavedTarget | null = null;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      mutationTick = nextWorkspaceLifecycleTick();
      const commit = Effect.suspend(() => {
        const envelopeTick = nextWorkspaceLifecycleTick();
        return executeGeneratedApiRequest("PATCH pull request content", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.editPrContentOnHost(
                { ...providerHostRouteParams(ref), number: number },
                fields,
                { signal },
              )
            : client.PullRequestsService.editPrContent({ ...providerRouteParams(ref), number: number }, fields, {
                signal,
              }),
        ).pipe(
          Effect.map((response): PullDetail => ({ ...response, events: response.events ?? [] })),
          Effect.tap((response) =>
            Effect.sync(() => {
              confirmedDetail = { detail: response, envelopeTick };
              if (
                fields.body !== undefined &&
                unsavedLocalBody !== null &&
                unsavedLocalBody.body === fields.body &&
                sameBodyTarget(
                  unsavedLocalBody.provider,
                  unsavedLocalBody.platformHost,
                  ref.provider,
                  ref.platformHost,
                ) &&
                unsavedLocalBody.owner === ref.owner &&
                unsavedLocalBody.name === ref.name &&
                unsavedLocalBody.number === number
              ) {
                acknowledgedUnsavedTarget = unsavedLocalBody;
              }
            }),
          ),
          Effect.map(
            (response): PRContentProjection => ({
              ...(fields.title !== undefined && { title: response.merge_request.Title }),
              ...(fields.body !== undefined && { body: response.merge_request.Body }),
            }),
          ),
        );
      });
      const refreshOnStale = Effect.suspend(() => {
        const refreshTick = nextWorkspaceLifecycleTick();
        return executeGeneratedApiRequest("sync pull request after stale content mutation", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(
          Effect.map((response): PullDetail => ({ ...response, events: response.events ?? [] })),
          Effect.tap((response) =>
            rebasePullMutations(ref, response, () => {
              if (!isDetailShowingRef(ref)) return false;
              return applyEnvelopeAt(refreshTick, () => {
                applyRefreshedDetail(withPreservedLocalBody(response));
                detailLoaded = response.detail_loaded ?? detailLoaded;
                noteObservedFetchedAt(response.detail_fetched_at);
              });
            }),
          ),
          Effect.map(
            (response): PRContentProjection => ({
              title: response.merge_request.Title,
              body: response.merge_request.Body,
            }),
          ),
        );
      }).pipe(Effect.provideService(ProviderMutations, mutations));
      yield* mutations.submit({
        key: pullMutationKey(ref, "content"),
        baseline,
        optimistic,
        apply,
        commit,
        refreshOnStale,
      });
      state.settled = true;
      if (acknowledgedUnsavedTarget !== null && unsavedLocalBody === acknowledgedUnsavedTarget) {
        unsavedLocalBody = null;
      }
      const confirmed = confirmedDetail;
      if (confirmed !== undefined) {
        yield* rebasePullMutations(ref, confirmed.detail, () => {
          if (!isDetailShowingRef(ref)) return false;
          return applyEnvelopeAt(confirmed.envelopeTick, () => {
            applyRefreshedDetail(withPreservedLocalBody(confirmed.detail));
            detailLoaded = confirmed.detail.detail_loaded ?? detailLoaded;
            noteObservedFetchedAt(confirmed.detail.detail_fetched_at);
          });
        });
      }
      refreshPullsIfActive();
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));
    return { ref, state, program };
  }

  function updatePRContent(
    routeRef: ProviderRouteRef,
    number: number,
    fields: EditPRContentInputBody,
    callbacks: MutationCallbacks = {},
  ): void {
    const update = preparePRContentUpdate(routeRef, number, fields, callbacks, true);
    if (update === undefined) {
      invokeMutationFailure(callbacks.onFailure, "The selected pull request changed before the edit started.");
      try {
        callbacks.onSettled?.();
      } catch {
        // Presentation callbacks do not own command acceptance.
      }
      return;
    }
    let admitted = false;
    const body = fields.body;
    const program =
      body === undefined
        ? update.program
        : Effect.gen(function* () {
            if (
              unsavedLocalBody !== null &&
              sameBodyTarget(
                unsavedLocalBody.provider,
                unsavedLocalBody.platformHost,
                update.ref.provider,
                update.ref.platformHost,
              ) &&
              unsavedLocalBody.owner === update.ref.owner &&
              unsavedLocalBody.name === update.ref.name &&
              unsavedLocalBody.number === number
            ) {
              unsavedLocalBody = null;
            }
            const workflow = yield* DetailWorkflow;
            const mutations = yield* ProviderMutations;
            yield* workflow.submitLatestWrite(
              prKey(update.ref),
              Effect.sync(() => {
                admitted = true;
              }).pipe(Effect.andThen(update.program), Effect.provideService(ProviderMutations, mutations)),
            );
            if (!admitted) yield* invokeMutationCallback(callbacks.onSettled);
          });
    runtime.runCommand(program, {
      operation: "update pull request content",
      safeContext: {
        provider: update.ref.provider,
        platformHost: concretePlatformHost(update.ref),
        owner: update.ref.owner,
        name: update.ref.name,
        number,
      },
      onFailure: (failure) => {
        if (update.state.settled) {
          showFlash("Pull request content was updated, but the latest detail could not be reconciled.", {
            tone: "danger",
          });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to update pull request");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  // Replaces the in-memory PR body without touching the server. Pair
  // with savePRBodyInBackground for instant-feedback edits (e.g. task-
  // list checkbox clicks): apply the change locally first, then push
  // it asynchronously so the click never blocks on the network. Marks
  // the body as unsaved so a background refresh can't revert it
  // before the debounced PATCH lands.
  function setLocalPRBody(
    provider: string,
    platformHost: string | undefined,
    owner: string,
    name: string,
    number: number,
    body: string,
  ): void {
    if (
      !detail ||
      !sameBodyTarget(detail.repo?.provider, detail.repo?.platform_host, provider, platformHost) ||
      detail.repo_owner !== owner ||
      detail.repo_name !== name ||
      detail.merge_request.Number !== number
    ) {
      return;
    }
    unsavedLocalBody = { provider, platformHost, owner, name, number, body };
    detail = {
      ...detail,
      merge_request: { ...detail.merge_request, Body: body },
    };
  }

  // Task-list interactions have already projected the body locally.
  // Submit the captured edit through the same acknowledged per-pull
  // content queue as explicit title/body saves so transport order and
  // response ownership cannot escape the application runtime.
  function savePRBodyInBackground(routeRef: ProviderRouteRef, number: number, body: string): void {
    const update = preparePRContentUpdate(routeRef, number, { body }, {}, false);
    if (update === undefined) return;
    const program = Effect.gen(function* () {
      const workflow = yield* DetailWorkflow;
      const mutations = yield* ProviderMutations;
      yield* workflow.submitLatestWrite(
        prKey(update.ref),
        update.program.pipe(Effect.provideService(ProviderMutations, mutations)),
      );
    });
    runtime.runCommand(program, {
      operation: "save pull request body in background",
      safeContext: {
        provider: update.ref.provider,
        platformHost: concretePlatformHost(update.ref),
        owner: update.ref.owner,
        name: update.ref.name,
        number,
      },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update pull request body");
        showFlash(message, { tone: "danger" });
      },
    });
  }

  function startDetailPolling(owner: string, name: string, number: number, identity: DetailRequestOptions): void {
    const ref = detailRequestRef(owner, name, number, identity);
    const pollingGeneration = ++detailPollingGeneration;
    if (unsubSyncComplete !== null) {
      unsubSyncComplete();
      unsubSyncComplete = null;
    }
    const pollOnce = Effect.suspend(() =>
      syncing
        ? Effect.void
        : enqueueBackgroundDetailSyncEffect(owner, name, number, syncGeneration, observedFetchedAtBaseline(), ref),
    ).pipe(Effect.catch(() => Effect.void));
    const program = Effect.gen(function* () {
      const workflow = yield* DetailWorkflow;
      yield* workflow.stopPolling;
      if (pollingGeneration !== detailPollingGeneration) return;
      yield* Effect.sleep("60 seconds");
      if (pollingGeneration !== detailPollingGeneration) return;
      yield* workflow.poll(pollOnce, "60 seconds");
    });
    runtime.runCommand(program, {
      operation: "poll pull request detail",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      },
      onFailure: () => {},
    });
    if (syncDep) {
      unsubSyncComplete = syncDep.subscribeSyncComplete(() => {
        if (syncing) return;
        refreshDetailOnly(owner, name, number, ref);
      });
    }
  }

  function stopDetailPolling(): void {
    detailPollingGeneration += 1;
    if (unsubSyncComplete !== null) {
      unsubSyncComplete();
      unsubSyncComplete = null;
    }
    const program = Effect.gen(function* () {
      const workflow = yield* DetailWorkflow;
      yield* workflow.stopPolling;
    });
    runtime.runCommand(program, {
      operation: "stop pull request detail polling",
      safeContext: {},
      onFailure: () => {},
    });
  }

  function toggleDetailPRStar(routeRef: ProviderRouteRef, number: number, currentlyStarred: boolean): void {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    if (!isDetailShowingRef(ref) || detail === null) return;
    const baseline = detail.merge_request.Starred ?? currentlyStarred;
    const optimistic = !baseline;
    let mutationTick = 0;
    const apply = (starred: boolean) =>
      Effect.sync(() => {
        optimisticDetailStarUpdate(ref, number, starred, mutationTick);
        pullsDep?.optimisticStarUpdate?.(ref, number, starred);
      });
    const starredItem: UnsetStarredParams = {
      item_type: "pr",
      provider: ref.provider,
      platform_host: concretePlatformHost(ref),
      owner: ref.owner,
      name: ref.name,
      number,
    };
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      mutationTick = nextWorkspaceLifecycleTick();
      const commit = (
        baseline
          ? executeGeneratedApiRequest<void>("DELETE pull request star", (client, signal) =>
              client.SettingsService.unsetStarred(starredItem, { signal }),
            )
          : executeGeneratedApiRequest<void>("PUT pull request star", (client, signal) =>
              client.SettingsService.setStarred(starredItem, { signal }),
            )
      ).pipe(Effect.as(optimistic));
      const refreshOnStale = executeGeneratedApiRequest(
        "GET pull request after stale star mutation",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.getPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.getPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.merge_request.Starred ?? baseline));
      yield* mutations.submit({
        key: pullMutationKey(ref, "star"),
        baseline,
        optimistic,
        apply,
        commit,
        refreshOnStale,
      });
      refreshPullsIfActive();
    });
    runtime.runCommand(program, {
      operation: baseline ? "unstar pull request" : "star pull request",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(
          failure,
          baseline ? "failed to unstar pull request" : "failed to star pull request",
        );
        showFlash(message, { tone: "danger" });
      },
    });
  }

  function optimisticDetailStarUpdate(
    routeRef: ProviderRouteRef,
    number: number,
    starred: boolean,
    envelopeTick = 0,
  ): void {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    if (!isDetailShowingRef(ref) || detail === null) return;
    detail = { ...detail, merge_request: { ...detail.merge_request, Starred: starred } };
    detailEnvelopeTick = Math.max(detailEnvelopeTick, envelopeTick);
  }

  function submitComment(
    owner: string,
    name: string,
    number: number,
    body: string,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "post pull request comment", cause }),
      });
      const key = pullMutationKey(ref, "comment-posts");
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("POST pull request comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.postPrCommentOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { body },
              { signal },
            )
          : client.PullRequestsService.postPrComment(
              { ...providerRouteParams(ref), number: number },
              { body },
              { signal },
            ),
      ).pipe(Effect.asVoid);
      yield* mutations.submit({
        key,
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit,
        refreshOnStale: executeGeneratedApiRequest(
          "sync pull request after stale comment submission",
          (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.PullRequestsService.syncPullOnHost(
                  { ...providerHostRouteParams(ref), number: number },
                  { signal },
                )
              : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(Effect.asVoid),
      });
      const gen = yield* Effect.sync(() => {
        mutationSettled = true;
        if (!isDetailShowingRef(ref)) return undefined;
        syncing = false;
        return ++syncGeneration;
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (gen === undefined) return;
      yield* refreshDetailOnlyEffect(owner, name, number, ref);
      if (gen === syncGeneration) {
        yield* syncDetailEffect(owner, name, number, ref);
      }
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "post pull request comment",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was posted, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to post comment");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function editComment(
    owner: string,
    name: string,
    number: number,
    commentID: number,
    body: string,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "edit pull request comment", cause }),
      });
      const previousEvents = detail?.events ?? [];
      const previousIndex = previousEvents.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      const previousEvent = previousEvents[previousIndex];
      if (previousIndex === -1 || previousEvent === undefined) {
        return yield* Effect.fail(
          TransientTransportError.make({
            operation: "edit pull request comment",
            cause: new Error("comment is no longer present in the selected pull request"),
          }),
        );
      }
      const key = pullMutationKey(ref, `comment\u0000${commentID}`);
      const baseline: PullCommentMutationState = { event: previousEvent, index: previousIndex, present: true };
      const optimistic: PullCommentMutationState = { ...baseline, event: { ...previousEvent, Body: body } };
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PATCH pull request comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.editPrCommentOnHost(
              { ...providerHostRouteParams(ref), number: number, commentId: commentID },
              { body },
              { signal },
            )
          : client.PullRequestsService.editPrComment(
              { ...providerRouteParams(ref), number: number, commentId: commentID },
              { body },
              { signal },
            ),
      ).pipe(Effect.as(optimistic));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale comment edit",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => pullCommentState(response.events ?? [], commentID, baseline)));
      const apply = (state: PullCommentMutationState) => applyPullCommentState(ref, commentID, state);
      yield* Effect.sync(() => trackPullCommentMutation(commentID, baseline));
      yield* mutations
        .submit({
          key,
          baseline,
          optimistic,
          apply,
          commit,
          refreshOnStale,
        })
        .pipe(Effect.ensuring(Effect.sync(() => releasePullCommentMutation(commentID))));
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isDetailShowingRef(ref);
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) yield* refreshDetailOnlyEffect(owner, name, number, ref);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "edit pull request comment",
      safeContext: { owner, name, number, commentID },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was edited, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to edit comment");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function deleteComment(
    owner: string,
    name: string,
    number: number,
    commentID: number,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    let mutationRef: DetailRequestRef | undefined;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "delete pull request comment", cause }),
      });
      mutationRef = ref;
      const previousEvents = detail?.events ?? [];
      const commentIndex = previousEvents.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      const previousEvent = previousEvents[commentIndex];
      if (commentIndex === -1 || previousEvent === undefined) {
        return yield* Effect.fail(
          TransientTransportError.make({
            operation: "delete pull request comment",
            cause: new Error("comment is no longer present in the selected pull request"),
          }),
        );
      }
      const key = pullMutationKey(ref, `comment\u0000${commentID}`);
      const baseline: PullCommentMutationState = { event: previousEvent, index: commentIndex, present: true };
      const optimistic: PullCommentMutationState = { ...baseline, present: false };
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("DELETE pull request comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.deletePrCommentOnHost(
              { ...providerHostRouteParams(ref), number: number, commentId: commentID },
              { headers: { "Content-Type": "application/json" }, signal },
            )
          : client.PullRequestsService.deletePrComment(
              { ...providerRouteParams(ref), number: number, commentId: commentID },
              { headers: { "Content-Type": "application/json" }, signal },
            ),
      ).pipe(Effect.as(optimistic));
      const refreshOnStale = executeGeneratedApiRequest(
        "sync pull request after stale comment deletion",
        (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.syncPullOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => pullCommentState(response.events ?? [], commentID, baseline)));
      const apply = (state: PullCommentMutationState) => applyPullCommentState(ref, commentID, state);
      yield* Effect.sync(() => {
        storeError = null;
        trackPullCommentMutation(commentID, baseline);
      });
      yield* mutations
        .submit({
          key,
          baseline,
          optimistic,
          apply,
          commit,
          refreshOnStale,
        })
        .pipe(Effect.ensuring(Effect.sync(() => releasePullCommentMutation(commentID))));
      yield* Effect.sync(() => hideDeletedComment(ref, commentID));
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isDetailShowingRef(ref);
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) {
        const reconciled = yield* syncDetailEffect(owner, name, number, ref);
        if (!reconciled && isDetailShowingRef(ref)) {
          return yield* Effect.fail(
            TransientTransportError.make({
              operation: "refresh pull request after comment deletion",
              cause: new Error("pull request synchronization did not return detail"),
            }),
          );
        }
      }
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "delete pull request comment",
      safeContext: { owner, name, number, commentID },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was deleted, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to delete comment");
        if (mutationRef !== undefined && isDetailShowingRef(mutationRef)) storeError = message;
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function replyToDiscussion(
    owner: string,
    name: string,
    number: number,
    discussionID: string,
    body: string,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "reply to pull request discussion", cause }),
      });
      const key = pullMutationKey(ref, `discussion\u0000${discussionID}\u0000replies`);
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("POST pull request discussion reply", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.replyToDiscussionOnHost(
              { ...providerHostRouteParams(ref), number: number, discussionId: discussionID },
              { body },
              { signal },
            )
          : client.PullRequestsService.replyToDiscussion(
              { ...providerRouteParams(ref), number: number, discussionId: discussionID },
              { body },
              { signal },
            ),
      ).pipe(Effect.asVoid);
      yield* mutations.submit({
        key,
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit,
        refreshOnStale: executeGeneratedApiRequest(
          "sync pull request after stale discussion reply",
          (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.PullRequestsService.syncPullOnHost(
                  { ...providerHostRouteParams(ref), number: number },
                  { signal },
                )
              : client.PullRequestsService.syncPull({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(Effect.asVoid),
      });
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isDetailShowingRef(ref);
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) yield* refreshDetailOnlyEffect(owner, name, number, ref);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "reply to pull request discussion",
      safeContext: { owner, name, number, discussionID },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Reply was posted, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to reply to thread");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function applyReviewSuggestions(
    routeRef: ProviderRouteRef,
    number: number,
    input: ApplySuggestionRequest,
    callbacks: ApplySuggestionCallbacks = {},
  ): void {
    const ref = detailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    const requestSelectionGeneration = selectionGeneration;
    const expectedHeadSHA = isDetailShowingRef(ref) ? (detail?.platform_head_sha ?? "") : "";
    if (!isDetailShowingRef(ref)) {
      try {
        callbacks.onResult?.(false);
      } catch {
        // Presentation callbacks do not own command acceptance.
      }
      try {
        callbacks.onSettled?.();
      } catch {
        // Presentation callbacks do not own command acceptance.
      }
      return;
    }
    const program = Effect.gen(function* () {
      if (requestSelectionGeneration !== selectionGeneration || !isDetailShowingRef(ref)) return false;
      const request = executeGeneratedApiRequest("POST apply pull request review suggestions", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.applyPrReviewSuggestionsOnHost(
              { ...providerHostRouteParams(ref), number: number },
              {
                expected_head_sha: expectedHeadSHA,
                ...(input.message ? { message: input.message } : {}),
                suggestions: input.suggestions.map((suggestion) => ({
                  thread_id: suggestion.threadID,
                  replacement: suggestion.replacement,
                })),
              },
              { signal },
            )
          : client.PullRequestsService.applyPrReviewSuggestions(
              { ...providerRouteParams(ref), number: number },
              {
                expected_head_sha: expectedHeadSHA,
                ...(input.message ? { message: input.message } : {}),
                suggestions: input.suggestions.map((suggestion) => ({
                  thread_id: suggestion.threadID,
                  replacement: suggestion.replacement,
                })),
              },
              { signal },
            ),
      );
      const requestResult = yield* Effect.result(request);
      if (requestSelectionGeneration !== selectionGeneration || !isDetailShowingRef(ref)) {
        if (
          Result.isFailure(requestResult) &&
          requestResult.failure._tag === "ApiProblemError" &&
          applySuggestionRefreshReason(requestResult.failure.problem)
        ) {
          return false;
        }
        if (Result.isFailure(requestResult)) {
          return yield* Effect.fail(requestResult.failure);
        }
        showFlash("Suggestion was applied after navigation. Refresh before applying it again.", {
          tone: "warning",
        });
        return true;
      }
      if (Result.isFailure(requestResult)) {
        if (requestResult.failure._tag !== "ApiProblemError") {
          return yield* Effect.fail(requestResult.failure);
        }
        const problem = requestResult.failure.problem;
        const message = apiErrorMessage(problem, "failed to apply suggestion");
        const refreshReason = applySuggestionRefreshReason(problem);
        if (refreshReason) {
          const conflict: ApplySuggestionConflict = {
            reason: refreshReason,
            context: problemConflictContext(problem),
            expectedHeadSha: expectedHeadSHA,
            ref,
            number,
          };
          if (callbacks.onConflict !== undefined) {
            yield* invokeApplySuggestionConflict(callbacks.onConflict, conflict);
          } else if (isDetailShowingRef(ref)) {
            syncGeneration += 1;
            const syncResult = yield* Effect.result(syncDetailEffect(ref.owner, ref.name, number, ref));
            const refreshed = Result.isSuccess(syncResult) && syncResult.success;
            if (!refreshed && isDetailShowingRef(ref)) {
              failClosedAfterApplySuggestionConflict(refreshReason);
            }
          }
          storeError = message;
          return false;
        }
        return yield* Effect.fail(requestResult.failure);
      }
      syncGeneration += 1;
      yield* Effect.result(syncDetailEffect(ref.owner, ref.name, number, ref));
      return true;
    }).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          showFlash(detailReadErrorMessage(failure), { tone: "danger" });
          return false;
        }),
      ),
      Effect.tap((applied) => invokeDetailSyncSuccess(callbacks.onResult, applied)),
      Effect.ensuring(invokeMutationCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "apply pull request review suggestions",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: () => {},
    });
  }

  return {
    getDetail,
    getDetailEnvelopeTick,
    isDetailLoading,
    isDetailSyncing,
    getDetailError,
    getDetailLoaded,
    clearDetail,
    loadDetail,
    refreshDetailOnly,
    refreshDetailOnlyEffect,
    syncDetailEffect,
    syncDetailNow,
    refreshPendingCI,
    updateKanbanState,
    setPullState,
    setPullLabels,
    setPullAssignees,
    setPullReviewers,
    approvePull,
    requestPullChanges,
    markPullReady,
    approvePullWorkflows,
    mergePull,
    updatePRContent,
    setLocalPRBody,
    savePRBodyInBackground,
    hasUnsavedLocalBody,
    startDetailPolling,
    stopDetailPolling,
    toggleDetailPRStar,
    optimisticDetailStarUpdate,
    submitComment,
    editComment,
    deleteComment,
    replyToDiscussion,
    applyReviewSuggestions,
  };
}

export type DetailStore = ReturnType<typeof createDetailStore>;
