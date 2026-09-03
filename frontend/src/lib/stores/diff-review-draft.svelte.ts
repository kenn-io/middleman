import { Effect } from "effect";
import type { AppExecution, AppRuntime } from "../app/runtime.js";
import { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import { executeGeneratedApiRequest, type GeneratedApi } from "../api/generated-api.js";
import type {
  DiffReviewDraftComment as GeneratedDiffReviewDraftComment,
  DiffReviewDraftResponse,
  DiffReviewLineRange as GeneratedDiffReviewLineRange,
} from "../api/generated/models/index.js";
import {
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { showFlash } from "./flash.svelte.js";
import {
  invokeMutationCallback,
  invokeMutationFailure,
  ProviderMutations,
  providerMutationFailureMessage,
  type MutationCallbacks,
  type ProviderMutationError,
} from "./ordered-mutations.js";
import { providerItemKey } from "./provider-key.js";

export type DiffReviewDraft = DiffReviewDraftResponse;
export type DiffReviewDraftComment = GeneratedDiffReviewDraftComment;
export type DiffReviewLineRange = GeneratedDiffReviewLineRange;

export interface DiffReviewDraftCommentEditState {
  active: boolean;
  dirty: boolean;
}

export interface DiffReviewDraftStoreOptions {
  runtime: AppRuntime;
  onPublished?: (
    ref: ProviderRouteRef,
    number: number,
  ) => Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations>;
  onStalePublish?: (
    ref: ProviderRouteRef,
    number: number,
  ) => Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations>;
}

export function createDiffReviewDraftStore(opts: DiffReviewDraftStoreOptions) {
  const runtime = opts.runtime;

  let enabled = $state(false);
  let ref = $state<ProviderRouteRef | null>(null);
  let number = $state(0);
  let diffHeadSHA = $state<string | undefined>(undefined);
  let draft = $state<DiffReviewDraft | null>(null);
  let loading = $state(false);
  let submitting = $state(false);
  let storeError = $state<string | null>(null);
  let storeWarning = $state<string | null>(null);
  let commentEditStates = $state<Record<string, DiffReviewDraftCommentEditState>>({});
  let wasEnabled = false;
  let draftVersion = 0;
  let submitVersion = 0;
  let activeDraftLoad: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;

  function isEnabled(): boolean {
    return enabled;
  }

  function getDraft(): DiffReviewDraft | null {
    return draft;
  }

  function getComments(): DiffReviewDraftComment[] {
    return draft?.comments ?? [];
  }

  function isLoading(): boolean {
    return loading;
  }

  function isSubmitting(): boolean {
    return submitting;
  }

  function getError(): string | null {
    return storeError;
  }

  function getWarning(): string | null {
    return storeWarning;
  }

  function setCommentEditState(editID: string, state: DiffReviewDraftCommentEditState): void {
    if (!state.active && !state.dirty) {
      clearCommentEditState(editID);
      return;
    }
    commentEditStates = {
      ...commentEditStates,
      [editID]: {
        active: state.active,
        dirty: state.dirty,
      },
    };
  }

  function clearCommentEditState(editID: string): void {
    if (commentEditStates[editID] === undefined) return;
    const next = { ...commentEditStates };
    delete next[editID];
    commentEditStates = next;
  }

  function clearCommentEditStates(): void {
    commentEditStates = {};
  }

  function hasPendingCommentEdits(): boolean {
    return Object.values(commentEditStates).some((state) => state.active || state.dirty);
  }

  function requestKey(): string {
    if (!ref || !number) return "";
    return [
      enabled ? "enabled" : "disabled",
      ref.provider,
      ref.platformHost ?? "",
      ref.repoPath,
      number,
      diffHeadSHA ?? "",
    ].join(":");
  }

  function mutationKey(selectedRef: ProviderRouteRef, selectedNumber: number): string {
    return `review-draft\u0000${providerItemKey({
      provider: selectedRef.provider,
      platformHost: resolvedPlatformHost(selectedRef.provider, selectedRef.platformHost),
      owner: selectedRef.owner,
      name: selectedRef.name,
      number: selectedNumber,
    })}`;
  }

  function beginSubmit(): number {
    const token = ++submitVersion;
    submitting = true;
    return token;
  }

  function finishSubmit(token: number): void {
    if (submitVersion === token) {
      submitting = false;
    }
  }

  function cancelSubmit(): void {
    submitVersion += 1;
    submitting = false;
  }

  function settleRejectedMutation(callbacks: MutationCallbacks): void {
    try {
      callbacks.onSettled?.();
    } catch {
      // Presentation callbacks do not own command acceptance.
    }
  }

  function draftCommentRange(comment: DiffReviewDraftComment): DiffReviewLineRange {
    const range: DiffReviewLineRange = {
      path: comment.path,
      side: comment.side,
      line: comment.line,
      line_type: comment.line_type,
    };
    if (comment.old_path !== undefined) range.old_path = comment.old_path;
    if (comment.start_side !== undefined) range.start_side = comment.start_side;
    if (comment.start_line !== undefined) range.start_line = comment.start_line;
    if (comment.old_line !== undefined) range.old_line = comment.old_line;
    if (comment.new_line !== undefined) range.new_line = comment.new_line;
    if (comment.diff_head_sha !== undefined) range.diff_head_sha = comment.diff_head_sha;
    if (comment.commit_sha !== undefined) range.commit_sha = comment.commit_sha;
    return range;
  }

  function setContext(
    nextRef: ProviderRouteRef,
    nextNumber: number,
    nextEnabled: boolean,
    nextDiffHeadSHA?: string,
  ): void {
    const changed =
      !ref ||
      ref.provider !== nextRef.provider ||
      ref.platformHost !== nextRef.platformHost ||
      ref.repoPath !== nextRef.repoPath ||
      number !== nextNumber ||
      diffHeadSHA !== nextDiffHeadSHA;
    const enabling = !wasEnabled && nextEnabled;
    ref = nextRef;
    number = nextNumber;
    diffHeadSHA = nextDiffHeadSHA;
    enabled = nextEnabled;
    wasEnabled = nextEnabled;
    if (!enabled) {
      draft = null;
      storeError = null;
      storeWarning = null;
      clearCommentEditStates();
      cancelSubmit();
      return;
    }
    if (changed || enabling) {
      draft = null;
      storeWarning = null;
      clearCommentEditStates();
      cancelSubmit();
      loadDraft();
    }
  }

  function setRouteContext(nextRef: ProviderRouteRef, nextNumber: number): void {
    const changed =
      !ref ||
      ref.provider !== nextRef.provider ||
      ref.platformHost !== nextRef.platformHost ||
      ref.repoPath !== nextRef.repoPath ||
      number !== nextNumber;
    ref = nextRef;
    number = nextNumber;
    if (changed) {
      draftVersion += 1;
      cancelSubmit();
      clearCommentEditStates();
      storeError = null;
      storeWarning = null;
    }
  }

  function invalidateDraftLoad(): void {
    activeDraftLoad?.interrupt();
    activeDraftLoad = null;
    draftVersion += 1;
    loading = false;
  }

  function clear(): void {
    invalidateDraftLoad();
    cancelSubmit();
    enabled = false;
    wasEnabled = false;
    ref = null;
    number = 0;
    diffHeadSHA = undefined;
    draft = null;
    loading = false;
    storeError = null;
    storeWarning = null;
    clearCommentEditStates();
  }

  function normalizeDraft(next: DiffReviewDraft): DiffReviewDraft {
    return {
      ...next,
      comments: next.comments ?? [],
      supported_actions: next.supported_actions ?? [],
    };
  }

  function readDraft(
    selectedRef: ProviderRouteRef,
    selectedNumber: number,
  ): Effect.Effect<DiffReviewDraft, ApiProblemError | TransientTransportError, GeneratedApi> {
    return executeGeneratedApiRequest("GET pull request review draft", (client, signal) =>
      providerUsesHostRoute(selectedRef)
        ? client.PullRequestsService.getPrReviewDraftOnHost(
            { ...providerHostRouteParams(selectedRef), number: selectedNumber },
            { signal },
          )
        : client.PullRequestsService.getPrReviewDraft(
            { ...providerRouteParams(selectedRef), number: selectedNumber },
            { signal },
          ),
    ).pipe(Effect.map(normalizeDraft));
  }

  function draftReadFailureMessage(failure: ApiProblemError | TransientTransportError): string {
    return failure._tag === "ApiProblemError"
      ? (failure.problem.detail ?? failure.problem.title ?? "failed to load review draft")
      : "Could not reach Kenn Forge";
  }

  function loadDraft(): void {
    if (!enabled || !ref) return;
    const selectedRef = ref;
    const selectedNumber = number;
    const key = requestKey();
    const version = ++draftVersion;
    const isCurrent = () => requestKey() === key && draftVersion === version;
    activeDraftLoad?.interrupt();
    loading = true;
    storeError = null;
    const program = readDraft(selectedRef, selectedNumber).pipe(
      Effect.tap((next) =>
        Effect.sync(() => {
          if (isCurrent()) draft = next;
        }),
      ),
      Effect.ensuring(
        Effect.sync(() => {
          if (isCurrent()) loading = false;
        }),
      ),
      Effect.asVoid,
    );
    const execution = runtime.runCommand(program, {
      operation: "load pull request review draft",
      safeContext: { owner: selectedRef.owner, name: selectedRef.name, number: selectedNumber },
      onFailure: (failure) => {
        if (isCurrent()) storeError = draftReadFailureMessage(failure);
      },
    });
    activeDraftLoad = execution;
  }

  function launchDraftMutation({
    selectedRef,
    selectedNumber,
    operation,
    fallback,
    commit,
    reconciliation,
    callbacks,
  }: {
    selectedRef: ProviderRouteRef;
    selectedNumber: number;
    operation: string;
    fallback: string;
    commit: Effect.Effect<void, ProviderMutationError, GeneratedApi>;
    reconciliation: "refresh" | "clear" | "none";
    callbacks: MutationCallbacks;
  }): void {
    const key = requestKey();
    invalidateDraftLoad();
    const version = draftVersion;
    const isCurrent = () => requestKey() === key && draftVersion === version;
    const submitToken = beginSubmit();
    storeError = null;
    storeWarning = null;
    let mutationAcknowledged = false;
    const refreshDraft = readDraft(selectedRef, selectedNumber).pipe(
      Effect.tap((next) =>
        Effect.sync(() => {
          if (isCurrent()) draft = next;
        }),
      ),
      Effect.asVoid,
    );
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      yield* mutations.submit({
        key: mutationKey(selectedRef, selectedNumber),
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit,
        refreshOnStale: refreshDraft,
      });
      yield* Effect.sync(() => {
        mutationAcknowledged = true;
        finishSubmit(submitToken);
        if (reconciliation === "clear" && isCurrent()) draft = null;
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (reconciliation === "refresh" && isCurrent()) yield* refreshDraft;
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          yield* Effect.sync(() => finishSubmit(submitToken));
          if (!mutationAcknowledged) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation,
      safeContext: { owner: selectedRef.owner, name: selectedRef.name, number: selectedNumber },
      onFailure: (failure) => {
        if (mutationAcknowledged) {
          showFlash("The change was saved, but the latest review draft could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, fallback);
        if (isCurrent()) {
          if (failure._tag === "MutationNeedsReview") {
            storeError = failure.problem.detail ?? failure.problem.title ?? message;
          } else if (callbacks.onFailure === undefined) {
            showFlash(message, { tone: "danger" });
          }
        }
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function createComment(body: string, range: DiffReviewLineRange, callbacks: MutationCallbacks = {}): void {
    if (!enabled || !ref || !number) {
      settleRejectedMutation(callbacks);
      return;
    }
    const selectedRef = ref;
    const selectedNumber = number;
    launchDraftMutation({
      selectedRef,
      selectedNumber,
      operation: "create pull request review draft comment",
      fallback: "failed to create review draft comment",
      commit: executeGeneratedApiRequest("POST pull request review draft comment", (client, signal) =>
        providerUsesHostRoute(selectedRef)
          ? client.PullRequestsService.createPrReviewDraftCommentOnHost(
              { ...providerHostRouteParams(selectedRef), number: selectedNumber },
              { body, range },
              { signal },
            )
          : client.PullRequestsService.createPrReviewDraftComment(
              { ...providerRouteParams(selectedRef), number: selectedNumber },
              { body, range },
              { signal },
            ),
      ).pipe(Effect.asVoid),
      reconciliation: "refresh",
      callbacks,
    });
  }

  function deleteComment(commentID: string, callbacks: MutationCallbacks = {}): void {
    if (!enabled || !ref || !number) {
      settleRejectedMutation(callbacks);
      return;
    }
    const selectedRef = ref;
    const selectedNumber = number;
    launchDraftMutation({
      selectedRef,
      selectedNumber,
      operation: "delete pull request review draft comment",
      fallback: "failed to delete review draft comment",
      commit: executeGeneratedApiRequest("DELETE pull request review draft comment", (client, signal) =>
        providerUsesHostRoute(selectedRef)
          ? client.PullRequestsService.deletePrReviewDraftCommentOnHost(
              { ...providerHostRouteParams(selectedRef), number: selectedNumber, draftCommentId: commentID },
              { signal },
            )
          : client.PullRequestsService.deletePrReviewDraftComment(
              { ...providerRouteParams(selectedRef), number: selectedNumber, draftCommentId: commentID },
              { signal },
            ),
      ).pipe(Effect.asVoid),
      reconciliation: "refresh",
      callbacks,
    });
  }

  function editComment(comment: DiffReviewDraftComment, body: string, callbacks: MutationCallbacks = {}): void {
    if (!enabled || !ref || !number) {
      settleRejectedMutation(callbacks);
      return;
    }
    const selectedRef = ref;
    const selectedNumber = number;
    launchDraftMutation({
      selectedRef,
      selectedNumber,
      operation: "edit pull request review draft comment",
      fallback: "failed to edit review draft comment",
      commit: executeGeneratedApiRequest("PATCH pull request review draft comment", (client, signal) =>
        providerUsesHostRoute(selectedRef)
          ? client.PullRequestsService.editPrReviewDraftCommentOnHost(
              { ...providerHostRouteParams(selectedRef), number: selectedNumber, draftCommentId: comment.id },
              { body, range: draftCommentRange(comment) },
              { signal },
            )
          : client.PullRequestsService.editPrReviewDraftComment(
              { ...providerRouteParams(selectedRef), number: selectedNumber, draftCommentId: comment.id },
              { body, range: draftCommentRange(comment) },
              { signal },
            ),
      ).pipe(Effect.asVoid),
      reconciliation: "refresh",
      callbacks,
    });
  }

  function publish(action: string, body = "", callbacks: MutationCallbacks = {}): void {
    if (!enabled || !ref || !number || hasPendingCommentEdits()) {
      settleRejectedMutation(callbacks);
      return;
    }
    const publishedRef = ref;
    const publishedNumber = number;
    const key = requestKey();
    invalidateDraftLoad();
    const version = draftVersion;
    const isCurrent = () => requestKey() === key && draftVersion === version;
    const submitToken = beginSubmit();
    storeError = null;
    storeWarning = null;
    let mutationAcknowledged = false;
    let partial = false;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("POST publish pull request review draft", (client, signal) =>
        providerUsesHostRoute(publishedRef)
          ? client.PullRequestsService.publishPrReviewDraftOnHost(
              { ...providerHostRouteParams(publishedRef), number: publishedNumber },
              { action, body },
              { signal },
            )
          : client.PullRequestsService.publishPrReviewDraft(
              { ...providerRouteParams(publishedRef), number: publishedNumber },
              { action, body },
              { signal },
            ),
      ).pipe(
        Effect.tap((response) =>
          Effect.sync(() => {
            partial = response.status === "partially_published";
          }),
        ),
        Effect.asVoid,
      );
      const refreshOnStale = Effect.gen(function* () {
        if (opts.onStalePublish !== undefined) {
          yield* opts.onStalePublish(publishedRef, publishedNumber);
        }
        const refreshed = yield* readDraft(publishedRef, publishedNumber);
        yield* Effect.sync(() => {
          if (requestKey() === key) draft = refreshed;
        });
      }).pipe(Effect.provideService(ProviderMutations, mutations));
      yield* mutations.submit({
        key: mutationKey(publishedRef, publishedNumber),
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(() => {
        mutationAcknowledged = true;
        finishSubmit(submitToken);
        if (!isCurrent()) return;
        draft = null;
        if (partial) {
          storeWarning =
            "Review was partially published. Some inline comments or the selected review action may not have been submitted.";
        }
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (opts.onPublished !== undefined) {
        yield* opts.onPublished(publishedRef, publishedNumber).pipe(
          Effect.catch(() =>
            Effect.sync(() => {
              showFlash("Review was published, but the pull request timeline could not be refreshed.", {
                tone: "danger",
              });
            }),
          ),
        );
      }
      if (!isCurrent()) return;
      const refreshed = yield* readDraft(publishedRef, publishedNumber);
      yield* Effect.sync(() => {
        if (requestKey() === key) draft = refreshed;
      });
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          yield* Effect.sync(() => finishSubmit(submitToken));
          if (!mutationAcknowledged) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "publish pull request review draft",
      safeContext: { owner: publishedRef.owner, name: publishedRef.name, number: publishedNumber },
      onFailure: (failure) => {
        if (mutationAcknowledged) {
          showFlash("Review was published, but the latest draft could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to publish review draft");
        if (isCurrent()) {
          if (failure._tag === "MutationNeedsReview") {
            storeError = failure.problem.detail ?? failure.problem.title ?? message;
          } else if (callbacks.onFailure === undefined) {
            showFlash(message, { tone: "danger" });
          }
        }
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function discard(callbacks: MutationCallbacks = {}): void {
    if (!enabled || !ref || !number || hasPendingCommentEdits()) {
      settleRejectedMutation(callbacks);
      return;
    }
    const selectedRef = ref;
    const selectedNumber = number;
    launchDraftMutation({
      selectedRef,
      selectedNumber,
      operation: "discard pull request review draft",
      fallback: "failed to discard review draft",
      commit: executeGeneratedApiRequest("DELETE pull request review draft", (client, signal) =>
        providerUsesHostRoute(selectedRef)
          ? client.PullRequestsService.discardPrReviewDraftOnHost(
              { ...providerHostRouteParams(selectedRef), number: selectedNumber },
              { signal },
            )
          : client.PullRequestsService.discardPrReviewDraft(
              { ...providerRouteParams(selectedRef), number: selectedNumber },
              { signal },
            ),
      ).pipe(Effect.asVoid),
      reconciliation: "clear",
      callbacks,
    });
  }

  function setThreadResolved(threadID: string, resolved: boolean, callbacks: MutationCallbacks = {}): void {
    if (!ref || !number) {
      settleRejectedMutation(callbacks);
      return;
    }
    const selectedRef = ref;
    const selectedNumber = number;
    launchDraftMutation({
      selectedRef,
      selectedNumber,
      operation: resolved ? "resolve pull request review thread" : "unresolve pull request review thread",
      fallback: resolved ? "failed to resolve review thread" : "failed to unresolve review thread",
      commit: executeGeneratedApiRequest(
        resolved ? "POST resolve pull request review thread" : "POST unresolve pull request review thread",
        (client, signal) => {
          const route = { number: selectedNumber, threadId: threadID };
          if (providerUsesHostRoute(selectedRef)) {
            const params = { ...providerHostRouteParams(selectedRef), ...route };
            return resolved
              ? client.PullRequestsService.resolvePrReviewThreadOnHost(params, { signal })
              : client.PullRequestsService.unresolvePrReviewThreadOnHost(params, { signal });
          }
          const params = { ...providerRouteParams(selectedRef), ...route };
          return resolved
            ? client.PullRequestsService.resolvePrReviewThread(params, { signal })
            : client.PullRequestsService.unresolvePrReviewThread(params, { signal });
        },
      ).pipe(Effect.asVoid),
      reconciliation: "none",
      callbacks,
    });
  }

  return {
    isEnabled,
    getDraft,
    getComments,
    isLoading,
    isSubmitting,
    getError,
    getWarning,
    hasPendingCommentEdits,
    setCommentEditState,
    setContext,
    setRouteContext,
    clear,
    loadDraft,
    createComment,
    deleteComment,
    editComment,
    publish,
    discard,
    setThreadResolved,
  };
}

export type DiffReviewDraftStore = ReturnType<typeof createDiffReviewDraftStore>;
