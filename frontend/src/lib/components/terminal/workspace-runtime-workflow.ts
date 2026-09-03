import {
  Cause,
  Clock,
  Context,
  Duration,
  Effect,
  Exit,
  Fiber,
  FiberMap,
  Layer,
  Option,
  Ref,
  Result,
  Schema,
  Semaphore,
  SynchronizedRef,
} from "effect";
import type { Scope } from "effect/Scope";
import { ApiProblemError, InvalidExternalPayload, TransientTransportError } from "../../api/effect-errors.js";
import { GeneratedApi } from "../../api/generated-api.js";
import { isProblem, type ProblemBody } from "../../api/problems.js";
import { orvalRequest } from "../../api/runtime.js";
import { type WorkspaceRuntimeState } from "../../api/workspace-runtime.js";
import type { LaunchTarget, RuntimeSession, WorkspaceRuntime } from "../../api/types.js";
import type { WorkspaceItemIdentity } from "../../workspace-inline.js";
import { notifyWorkspaceDeleted } from "../../stores/workspace-host.svelte.js";
import {
  completeAcceptedWorkspaceLaunch,
  expireAcceptedWorkspaceLaunch,
  pendingWorkspaceLaunch,
} from "../../stores/workspace-create-pending.svelte.js";
import type { WorkflowPreset } from "./workflow-presets.js";
import { decodeWorkspaceDetail, type WorkspaceDetail } from "./workspace-detail.js";

const acceptedLaunchReconciliationWindowMillis = 15_000;
const workspaceRuntimeStopTimeout = Duration.seconds(10);

export interface WorkspaceRuntimeTarget {
  readonly workspaceId: string;
  readonly hostKey?: string | undefined;
}

export interface WorkspaceRuntimePort {
  readonly read: (target: WorkspaceRuntimeTarget) => Effect.Effect<WorkspaceRuntimeState, WorkspaceRuntimePortFailure>;
  readonly launch: (
    target: WorkspaceRuntimeTarget,
    targetKey: string,
    region: "workflow" | "terminal",
  ) => Effect.Effect<RuntimeSession, WorkspaceRuntimePortFailure>;
  readonly rename: (
    target: WorkspaceRuntimeTarget,
    sessionKey: string,
    label: string,
  ) => Effect.Effect<RuntimeSession, WorkspaceRuntimePortFailure>;
  readonly stop: (
    target: WorkspaceRuntimeTarget,
    sessionKey: string,
  ) => Effect.Effect<void, WorkspaceRuntimePortFailure>;
  readonly refresh: (target: WorkspaceRuntimeTarget) => Effect.Effect<WorkspaceDetail, WorkspaceRuntimeMutationFailure>;
  readonly retry: (target: WorkspaceRuntimeTarget) => Effect.Effect<WorkspaceDetail, WorkspaceRuntimeMutationFailure>;
  readonly delete: (
    target: WorkspaceRuntimeTarget,
    force: boolean,
  ) => Effect.Effect<WorkspaceRuntimeDeleteResult, TransientTransportError>;
  readonly isDeleted?:
    | ((target: WorkspaceRuntimeTarget) => Effect.Effect<boolean, WorkspaceRuntimeMutationFailure>)
    | undefined;
}

export class WorkspaceRuntimeMutationOutcomeUnknown extends Schema.TaggedError<WorkspaceRuntimeMutationOutcomeUnknown>()(
  "WorkspaceRuntimeMutationOutcomeUnknown",
  {
    operation: Schema.String,
    cause: Schema.Defect(),
    reconciliationCause: Schema.Defect(),
  },
) {}

export type WorkspaceRuntimePortFailure = ApiProblemError | InvalidExternalPayload | TransientTransportError;

export type WorkspaceRuntimeMutationFailure = WorkspaceRuntimePortFailure | WorkspaceRuntimeMutationOutcomeUnknown;

export interface WorkspaceRuntimeDeleteResult {
  readonly error?: ProblemBody | undefined;
  readonly response: Response;
}

export interface WorkspaceRuntimeDeleteOptions {
  readonly force: boolean;
  readonly identity?: WorkspaceItemIdentity | undefined;
  readonly presenterID: string;
}

export interface WorkspaceRuntimeWorkflowOptions {
  readonly publishDeleted: (
    target: WorkspaceRuntimeTarget,
    identity: WorkspaceItemIdentity | undefined,
  ) => Effect.Effect<void>;
}

export interface WorkspaceRuntimeReadOptions {
  readonly force?: boolean | undefined;
}

export interface AcceptedWorkspaceLaunchReconciliation {
  readonly target: WorkspaceRuntimeTarget;
  readonly sessionKey: string;
  readonly acceptedAt: number;
  readonly onExpired: Effect.Effect<void>;
}

export type WorkspaceRuntimeLaunchPlacement =
  | {
      readonly _tag: "Workflow";
      readonly onSettled?: ((settlement: WorkspaceRuntimeLaunchSettlement) => void) | undefined;
    }
  | { readonly _tag: "Terminal"; readonly insertIntoTree: boolean }
  | {
      readonly _tag: "TerminalSplit";
      readonly direction: "horizontal" | "vertical";
      readonly groupID: string;
      readonly targetLeafID?: string | undefined;
    };

export type WorkspaceRuntimeLaunchSettlement =
  | { readonly _tag: "Accepted"; readonly sessionKey: string }
  | { readonly _tag: "Rejected" };

interface WorkspaceRuntimeMutationRequestBase {
  readonly target: WorkspaceRuntimeTarget;
}

interface WorkspaceRuntimeLaunchRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "Launch";
  readonly targetKey: string;
  readonly region: "workflow" | "terminal";
  readonly placement: WorkspaceRuntimeLaunchPlacement;
}

interface WorkspaceRuntimeStopRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "Stop";
  readonly sessionKey: string;
}

interface WorkspaceRuntimeRenameRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "Rename";
  readonly sessionKey: string;
  readonly label: string;
}

interface WorkspaceRuntimeApplyPresetRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "ApplyPreset";
  readonly preset: WorkflowPreset;
}

interface WorkspaceRuntimeRefreshRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "Refresh";
}

interface WorkspaceRuntimeRetrySetupRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "RetrySetup";
}

interface WorkspaceRuntimeDeleteRequest extends WorkspaceRuntimeMutationRequestBase {
  readonly _tag: "Delete";
  readonly options: WorkspaceRuntimeDeleteOptions;
}

type WorkspaceRuntimeMutationRequest =
  | WorkspaceRuntimeLaunchRequest
  | WorkspaceRuntimeStopRequest
  | WorkspaceRuntimeRenameRequest
  | WorkspaceRuntimeApplyPresetRequest
  | WorkspaceRuntimeRefreshRequest
  | WorkspaceRuntimeRetrySetupRequest
  | WorkspaceRuntimeDeleteRequest;

type AcceptedWorkspaceRuntimeMutationRequest<Request extends WorkspaceRuntimeMutationRequest> = Request & {
  readonly key: string;
  readonly mutationID: number;
  readonly originPresenterID?: string | undefined;
};

type AcceptedWorkspaceRuntimeLaunchRequest = AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeLaunchRequest>;
type AcceptedWorkspaceRuntimeStopRequest = AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeStopRequest>;
type AcceptedWorkspaceRuntimeRenameRequest = AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeRenameRequest>;
type AcceptedWorkspaceRuntimeApplyPresetRequest =
  AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeApplyPresetRequest>;
type AcceptedWorkspaceRuntimeRefreshRequest = AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeRefreshRequest>;
type AcceptedWorkspaceRuntimeRetrySetupRequest =
  AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeRetrySetupRequest>;
type AcceptedWorkspaceRuntimeDeleteRequest = AcceptedWorkspaceRuntimeMutationRequest<WorkspaceRuntimeDeleteRequest>;
type AcceptedWorkspaceRuntimeRequest =
  | AcceptedWorkspaceRuntimeLaunchRequest
  | AcceptedWorkspaceRuntimeStopRequest
  | AcceptedWorkspaceRuntimeRenameRequest
  | AcceptedWorkspaceRuntimeApplyPresetRequest
  | AcceptedWorkspaceRuntimeRefreshRequest
  | AcceptedWorkspaceRuntimeRetrySetupRequest
  | AcceptedWorkspaceRuntimeDeleteRequest;

type WorkspaceRuntimeUncertainState<Request extends AcceptedWorkspaceRuntimeRequest = AcceptedWorkspaceRuntimeRequest> =
  Request extends AcceptedWorkspaceRuntimeRequest
    ? {
        readonly kind: "uncertain";
        readonly operation: Request["_tag"];
        readonly request: Request;
        readonly error: WorkspaceRuntimeMutationOutcomeUnknown;
      }
    : never;

export type WorkspaceRuntimeMutationState =
  | { readonly kind: "pending"; readonly operation: "Launch"; readonly request: AcceptedWorkspaceRuntimeLaunchRequest }
  | { readonly kind: "pending"; readonly operation: "Stop"; readonly request: AcceptedWorkspaceRuntimeStopRequest }
  | { readonly kind: "pending"; readonly operation: "Rename"; readonly request: AcceptedWorkspaceRuntimeRenameRequest }
  | {
      readonly kind: "pending";
      readonly operation: "ApplyPreset";
      readonly request: AcceptedWorkspaceRuntimeApplyPresetRequest;
    }
  | {
      readonly kind: "pending";
      readonly operation: "Refresh";
      readonly request: AcceptedWorkspaceRuntimeRefreshRequest;
    }
  | {
      readonly kind: "pending";
      readonly operation: "RetrySetup";
      readonly request: AcceptedWorkspaceRuntimeRetrySetupRequest;
    }
  | { readonly kind: "pending"; readonly operation: "Delete"; readonly request: AcceptedWorkspaceRuntimeDeleteRequest }
  | {
      readonly kind: "succeeded";
      readonly operation: "Launch";
      readonly request: AcceptedWorkspaceRuntimeLaunchRequest;
      readonly session: RuntimeSession;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "Stop";
      readonly request: AcceptedWorkspaceRuntimeStopRequest;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "Rename";
      readonly request: AcceptedWorkspaceRuntimeRenameRequest;
      readonly session: RuntimeSession;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "ApplyPreset";
      readonly request: AcceptedWorkspaceRuntimeApplyPresetRequest;
      readonly keyMap: Readonly<Record<string, string>>;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "Refresh";
      readonly request: AcceptedWorkspaceRuntimeRefreshRequest;
      readonly workspace: WorkspaceDetail;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "RetrySetup";
      readonly request: AcceptedWorkspaceRuntimeRetrySetupRequest;
      readonly workspace: WorkspaceDetail;
    }
  | {
      readonly kind: "succeeded";
      readonly operation: "Delete";
      readonly request: AcceptedWorkspaceRuntimeDeleteRequest;
      readonly result: WorkspaceRuntimeDeleteResult;
    }
  | {
      readonly kind: "failed";
      readonly operation: "Launch";
      readonly request: AcceptedWorkspaceRuntimeLaunchRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "Stop";
      readonly request: AcceptedWorkspaceRuntimeStopRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "Rename";
      readonly request: AcceptedWorkspaceRuntimeRenameRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "ApplyPreset";
      readonly request: AcceptedWorkspaceRuntimeApplyPresetRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "Refresh";
      readonly request: AcceptedWorkspaceRuntimeRefreshRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "RetrySetup";
      readonly request: AcceptedWorkspaceRuntimeRetrySetupRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | {
      readonly kind: "failed";
      readonly operation: "Delete";
      readonly request: AcceptedWorkspaceRuntimeDeleteRequest;
      readonly error: WorkspaceRuntimeMutationFailure;
    }
  | WorkspaceRuntimeUncertainState;

export type WorkspaceRuntimeMutationObserver = (state: WorkspaceRuntimeMutationState) => Effect.Effect<boolean>;

interface WorkspaceRuntimeMutationPresenter {
  readonly sessionID: string;
  readonly observe: WorkspaceRuntimeMutationObserver;
  readonly releaseWhenAcknowledged: boolean;
  readonly presentationIsCurrent?: (() => boolean) | undefined;
}

type WorkspaceRuntimeSucceededState = Extract<WorkspaceRuntimeMutationState, { readonly kind: "succeeded" }>;

type WorkspaceRuntimeReconciliation =
  | { readonly _tag: "Applied"; readonly state: WorkspaceRuntimeSucceededState }
  | { readonly _tag: "NotApplied" }
  | { readonly _tag: "Unknown"; readonly cause: unknown };

interface WorkspaceRuntimeMutationFence {
  readonly request: AcceptedWorkspaceRuntimeRequest;
  readonly error: TransientTransportError;
  readonly reconcile: Effect.Effect<WorkspaceRuntimeReconciliation>;
}

interface WorkspaceRuntimePresetProgress {
  readonly presetID: string;
  readonly keyMap: Readonly<Record<string, string>>;
}

export interface WorkspaceRuntimePresenterOptions {
  readonly releaseWhenAcknowledged?: boolean | undefined;
  readonly presentationIsCurrent?: (() => boolean) | undefined;
}

export interface WorkspaceRuntimeWorkflowService {
  readonly read: (
    owner: string,
    workspaceId: string,
    hostKey?: string | undefined,
    options?: WorkspaceRuntimeReadOptions,
  ) => Effect.Effect<Option.Option<WorkspaceRuntimeState>, WorkspaceRuntimePortFailure>;
  readonly launch: (
    target: WorkspaceRuntimeTarget,
    targetKey: string,
    region: "workflow" | "terminal",
    placement: WorkspaceRuntimeLaunchPlacement,
  ) => Effect.Effect<void>;
  readonly rename: (target: WorkspaceRuntimeTarget, sessionKey: string, label: string) => Effect.Effect<void>;
  readonly stop: (target: WorkspaceRuntimeTarget, sessionKey: string) => Effect.Effect<void>;
  readonly applyPreset: (target: WorkspaceRuntimeTarget, preset: WorkflowPreset) => Effect.Effect<void>;
  readonly refresh: (target: WorkspaceRuntimeTarget) => Effect.Effect<void>;
  readonly retrySetup: (target: WorkspaceRuntimeTarget) => Effect.Effect<void>;
  readonly delete: (target: WorkspaceRuntimeTarget, options: WorkspaceRuntimeDeleteOptions) => Effect.Effect<void>;
  readonly reconcileAcceptedLaunch: (request: AcceptedWorkspaceLaunchReconciliation) => Effect.Effect<void>;
  readonly claimPresenter: (
    target: WorkspaceRuntimeTarget,
    sessionID: string,
    observe: WorkspaceRuntimeMutationObserver,
    options?: WorkspaceRuntimePresenterOptions,
  ) => Effect.Effect<void>;
  readonly releasePresenter: (target: WorkspaceRuntimeTarget, sessionID: string) => Effect.Effect<void>;
  readonly release: (owner: string) => Effect.Effect<void>;
}

interface ActiveRuntimeRead {
  readonly targetKey: string;
  readonly fiber: Fiber.Fiber<WorkspaceRuntimeState, WorkspaceRuntimePortFailure>;
}

let nextWorkspaceRuntimeOwner = 0;
let nextWorkspaceRuntimePresenter = 0;

export function makeWorkspaceRuntimeOwner(prefix: string): string {
  nextWorkspaceRuntimeOwner += 1;
  return `${prefix}:${nextWorkspaceRuntimeOwner}`;
}

export function makeWorkspaceRuntimePresenterID(): string {
  nextWorkspaceRuntimePresenter += 1;
  return `workspace-runtime-presenter:${nextWorkspaceRuntimePresenter}`;
}

function runtimeTargetKey(workspaceId: string, hostKey: string | undefined): string {
  return JSON.stringify([hostKey ?? null, workspaceId]);
}

function pendingMutationState(request: AcceptedWorkspaceRuntimeRequest): WorkspaceRuntimeMutationState {
  switch (request._tag) {
    case "Launch":
      return { kind: "pending", operation: "Launch", request };
    case "Stop":
      return { kind: "pending", operation: "Stop", request };
    case "Rename":
      return { kind: "pending", operation: "Rename", request };
    case "ApplyPreset":
      return { kind: "pending", operation: "ApplyPreset", request };
    case "Refresh":
      return { kind: "pending", operation: "Refresh", request };
    case "RetrySetup":
      return { kind: "pending", operation: "RetrySetup", request };
    case "Delete":
      return { kind: "pending", operation: "Delete", request };
  }
}

function failedMutationState(
  request: AcceptedWorkspaceRuntimeRequest,
  error: WorkspaceRuntimeMutationFailure,
): WorkspaceRuntimeMutationState {
  switch (request._tag) {
    case "Launch":
      return { kind: "failed", operation: "Launch", request, error };
    case "Stop":
      return { kind: "failed", operation: "Stop", request, error };
    case "Rename":
      return { kind: "failed", operation: "Rename", request, error };
    case "ApplyPreset":
      return { kind: "failed", operation: "ApplyPreset", request, error };
    case "Refresh":
      return { kind: "failed", operation: "Refresh", request, error };
    case "RetrySetup":
      return { kind: "failed", operation: "RetrySetup", request, error };
    case "Delete":
      return { kind: "failed", operation: "Delete", request, error };
  }
}

function uncertainMutationState(
  request: AcceptedWorkspaceRuntimeRequest,
  error: TransientTransportError,
  reconciliationCause: unknown,
): WorkspaceRuntimeMutationState {
  const uncertainty = WorkspaceRuntimeMutationOutcomeUnknown.make({
    operation: request._tag,
    cause: error,
    reconciliationCause,
  });
  switch (request._tag) {
    case "Launch":
      return { kind: "uncertain", operation: "Launch", request, error: uncertainty };
    case "Stop":
      return { kind: "uncertain", operation: "Stop", request, error: uncertainty };
    case "Rename":
      return { kind: "uncertain", operation: "Rename", request, error: uncertainty };
    case "ApplyPreset":
      return { kind: "uncertain", operation: "ApplyPreset", request, error: uncertainty };
    case "Refresh":
      return { kind: "uncertain", operation: "Refresh", request, error: uncertainty };
    case "RetrySetup":
      return { kind: "uncertain", operation: "RetrySetup", request, error: uncertainty };
    case "Delete":
      return { kind: "uncertain", operation: "Delete", request, error: uncertainty };
  }
}

export function makeWorkspaceRuntimeWorkflow(
  port: WorkspaceRuntimePort,
  options: WorkspaceRuntimeWorkflowOptions = {
    publishDeleted: (target, identity) =>
      Effect.sync(() => notifyWorkspaceDeleted(target.workspaceId, target.hostKey, identity)),
  },
): Effect.Effect<WorkspaceRuntimeWorkflowService, never, Scope> {
  return Effect.gen(function* () {
    const scope = yield* Effect.scope;
    const reads = yield* FiberMap.make<string, WorkspaceRuntimeState, WorkspaceRuntimePortFailure>();
    const active = yield* SynchronizedRef.make<ReadonlyMap<string, ActiveRuntimeRead>>(new Map());
    const mutationSequence = yield* Ref.make(1);
    const mutationStates = yield* Ref.make<ReadonlyMap<string, WorkspaceRuntimeMutationState>>(new Map());
    const mutationPresenters = yield* Ref.make<ReadonlyMap<string, ReadonlyArray<WorkspaceRuntimeMutationPresenter>>>(
      new Map(),
    );
    const mutationFences = yield* Ref.make<ReadonlyMap<string, WorkspaceRuntimeMutationFence>>(new Map());
    const presetProgress = yield* Ref.make<ReadonlyMap<string, WorkspaceRuntimePresetProgress>>(new Map());
    const mutationLock = yield* Semaphore.make(1);
    const mutationRecoveryLock = yield* Semaphore.make(1);
    const mutationPresentations = yield* FiberMap.make<string, void, never>();

    const selectMutationPresenter = (
      state: WorkspaceRuntimeMutationState,
      presenters: ReadonlyArray<WorkspaceRuntimeMutationPresenter>,
    ): WorkspaceRuntimeMutationPresenter | undefined => {
      if (state.operation === "Delete") {
        const initiatingPresenter = presenters.find(
          (candidate) => candidate.sessionID === state.request.options.presenterID,
        );
        if (state.kind !== "uncertain") return initiatingPresenter;
        const currentPresenters = presenters.filter((candidate) => candidate.presentationIsCurrent?.() !== false);
        const currentInitiatingPresenter = currentPresenters.find(
          (candidate) => candidate.sessionID === state.request.options.presenterID,
        );
        return currentInitiatingPresenter ?? currentPresenters.at(-1);
      }
      const presenterID =
        state.kind === "failed" || state.kind === "uncertain"
          ? state.request.originPresenterID
          : presenters.at(-1)?.sessionID;
      return presenters.find((candidate) => candidate.sessionID === presenterID);
    };

    const mutationKey = (request: WorkspaceRuntimeMutationRequest): string => {
      const target = runtimeTargetKey(request.target.workspaceId, request.target.hostKey);
      switch (request._tag) {
        case "Launch":
          return JSON.stringify([target, "launch", request.targetKey, request.region]);
        case "Stop":
        case "Rename":
          return JSON.stringify([target, "session", request.sessionKey]);
        case "ApplyPreset":
          return JSON.stringify([target, "preset"]);
        case "Refresh":
          return JSON.stringify([target, "refresh"]);
        case "RetrySetup":
          return JSON.stringify([target, "retry-setup"]);
        case "Delete":
          return JSON.stringify([target, "delete"]);
      }
    };

    const presentStoredMutation = Effect.fn("WorkspaceRuntimeWorkflow.presentStoredMutation")(function* (
      state: WorkspaceRuntimeMutationState,
      expectedSessionID?: string,
    ) {
      const presentation = yield* FiberMap.run(
        mutationPresentations,
        state.request.key,
        Effect.yieldNow.pipe(
          Effect.andThen(
            Effect.gen(function* () {
              const targetKey = runtimeTargetKey(state.request.target.workspaceId, state.request.target.hostKey);
              const presenter = yield* mutationLock.withPermit(
                Effect.gen(function* () {
                  if ((yield* Ref.get(mutationStates)).get(state.request.key) !== state) return undefined;
                  const presenters = (yield* Ref.get(mutationPresenters)).get(targetKey) ?? [];
                  const selected = selectMutationPresenter(state, presenters);
                  if (selected === undefined && state.kind === "failed") {
                    yield* Ref.update(mutationStates, (current) => {
                      if (current.get(state.request.key) !== state) return current;
                      const next = new Map(current);
                      next.delete(state.request.key);
                      return next;
                    });
                  }
                  return selected;
                }),
              );
              if (
                presenter === undefined ||
                (expectedSessionID !== undefined && presenter.sessionID !== expectedSessionID)
              ) {
                return;
              }

              const observation = presenter.observe(state);
              const acknowledged = yield* state.kind === "succeeded" && state.operation === "Stop"
                ? observation.pipe(
                    Effect.timeoutOrElse({
                      duration: workspaceRuntimeStopTimeout,
                      orElse: () => Effect.succeed(true),
                    }),
                  )
                : observation;
              if (!acknowledged) return;
              yield* mutationLock.withPermit(
                Effect.gen(function* () {
                  const currentPresenters = (yield* Ref.get(mutationPresenters)).get(targetKey) ?? [];
                  if (selectMutationPresenter(state, currentPresenters)?.sessionID !== presenter.sessionID) return;
                  if (state.kind !== "uncertain") {
                    yield* Ref.update(mutationStates, (current) => {
                      if (current.get(state.request.key) !== state) return current;
                      const next = new Map(current);
                      next.delete(state.request.key);
                      return next;
                    });
                  }
                  if (state.kind !== "uncertain" && presenter.releaseWhenAcknowledged) {
                    yield* Ref.update(mutationPresenters, (current) => {
                      const presenters = current.get(targetKey);
                      if (!presenters?.some((candidate) => candidate.sessionID === presenter.sessionID)) return current;
                      const next = new Map(current);
                      const remaining = presenters.filter((candidate) => candidate.sessionID !== presenter.sessionID);
                      if (remaining.length === 0) next.delete(targetKey);
                      else next.set(targetKey, remaining);
                      return next;
                    });
                  }
                }),
              );
            }),
          ),
        ),
      );
      yield* Fiber.await(presentation);
    });

    const storeAndPresentMutation = Effect.fn("WorkspaceRuntimeWorkflow.storeAndPresentMutation")(function* (
      state: WorkspaceRuntimeMutationState,
    ) {
      if (state.operation === "Delete" && state.kind === "succeeded") {
        const { response } = state.result;
        const responseFailed = state.request.options.force
          ? !response.ok && response.status !== 204
          : response.status === 409 || (!response.ok && response.status !== 204);
        if (!responseFailed) {
          yield* options.publishDeleted(state.request.target, state.request.options.identity);
        }
      }
      yield* mutationLock.withPermit(
        Ref.update(mutationStates, (current) => new Map(current).set(state.request.key, state)),
      );
      if (state.operation === "Launch" && state.kind !== "uncertain" && state.request.placement._tag === "Workflow") {
        const onSettled = state.request.placement.onSettled;
        if (onSettled !== undefined) {
          yield* Effect.sync(() =>
            onSettled(
              state.kind === "succeeded" ? { _tag: "Accepted", sessionKey: state.session.key } : { _tag: "Rejected" },
            ),
          );
        }
      }
      yield* presentStoredMutation(state);
    });

    const clearFence = (fence: WorkspaceRuntimeMutationFence): Effect.Effect<void> =>
      Ref.update(mutationFences, (current) => {
        if (current.get(fence.request.key) !== fence) return current;
        const next = new Map(current);
        next.delete(fence.request.key);
        return next;
      });

    const removeStoredMutation = (key: string): Effect.Effect<void> =>
      mutationLock.withPermit(
        Ref.update(mutationStates, (current) => {
          if (!current.has(key)) return current;
          const next = new Map(current);
          next.delete(key);
          return next;
        }),
      );

    const runtimeReconciliation = (
      target: WorkspaceRuntimeTarget,
      project: (runtime: WorkspaceRuntimeState) => WorkspaceRuntimeReconciliation,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      Effect.suspend(() =>
        port.read(target).pipe(
          Effect.match({
            onFailure: (cause): WorkspaceRuntimeReconciliation => ({ _tag: "Unknown", cause }),
            onSuccess: project,
          }),
        ),
      );

    const reconcileLaunch = (
      request: AcceptedWorkspaceRuntimeLaunchRequest,
      baselineKeys: ReadonlySet<string>,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      runtimeReconciliation(request.target, (runtime) => {
        const candidates = runtime.sessions.filter(
          (session) =>
            !baselineKeys.has(session.key) &&
            session.target_key === request.targetKey &&
            session.display_region === request.region,
        );
        if (candidates.length === 0) return { _tag: "NotApplied" };
        if (candidates.length !== 1) {
          return {
            _tag: "Unknown",
            cause: new Error("runtime authority returned multiple matching launched sessions"),
          };
        }
        return {
          _tag: "Applied",
          state: { kind: "succeeded", operation: "Launch", request, session: candidates[0]! },
        };
      });

    const reconcileRename = (
      request: AcceptedWorkspaceRuntimeRenameRequest,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      runtimeReconciliation(request.target, (runtime) => {
        const session = runtime.sessions.find((candidate) => candidate.key === request.sessionKey);
        return session?.label === request.label
          ? { _tag: "Applied", state: { kind: "succeeded", operation: "Rename", request, session } }
          : { _tag: "NotApplied" };
      });

    const reconcileStop = (
      request: AcceptedWorkspaceRuntimeStopRequest,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      runtimeReconciliation(request.target, (runtime) => {
        const session = runtime.sessions.find((candidate) => candidate.key === request.sessionKey);
        return session === undefined || session.status === "exited"
          ? { _tag: "Applied", state: { kind: "succeeded", operation: "Stop", request } }
          : { _tag: "NotApplied" };
      }).pipe(
        Effect.timeoutOrElse({
          duration: workspaceRuntimeStopTimeout,
          orElse: () =>
            Effect.succeed({
              _tag: "Unknown",
              cause: new Error("timed out waiting for runtime authority after stopping the session"),
            } satisfies WorkspaceRuntimeReconciliation),
        }),
      );

    const reconcileDelete = (
      request: AcceptedWorkspaceRuntimeDeleteRequest,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> => {
      const checkDeleted = port.isDeleted;
      if (checkDeleted === undefined) {
        return Effect.succeed({
          _tag: "Unknown",
          cause: new Error("workspace deletion authority is unavailable"),
        });
      }
      return Effect.suspend(() =>
        checkDeleted(request.target).pipe(
          Effect.match({
            onFailure: (cause): WorkspaceRuntimeReconciliation => ({ _tag: "Unknown", cause }),
            onSuccess: (deleted): WorkspaceRuntimeReconciliation =>
              deleted
                ? {
                    _tag: "Applied",
                    state: {
                      kind: "succeeded",
                      operation: "Delete",
                      request,
                      result: { response: new Response(null, { status: 204 }) },
                    },
                  }
                : { _tag: "NotApplied" },
          }),
        ),
      );
    };

    const updatePresetMapping = (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
      sourceKey: string,
      sessionKey: string,
    ): Effect.Effect<Readonly<Record<string, string>>> =>
      Ref.modify(presetProgress, (current) => {
        const progress = current.get(request.key);
        const keyMap = {
          ...(progress?.presetID === request.preset.id ? progress.keyMap : {}),
          [sourceKey]: sessionKey,
        };
        return [keyMap, new Map(current).set(request.key, { presetID: request.preset.id, keyMap })];
      });

    const clearPresetProgress = (request: AcceptedWorkspaceRuntimeApplyPresetRequest): Effect.Effect<void> =>
      Ref.update(presetProgress, (current) => {
        const progress = current.get(request.key);
        if (progress?.presetID !== request.preset.id) return current;
        const next = new Map(current);
        next.delete(request.key);
        return next;
      });

    const presetReconciliationResult = (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
      keyMap: Readonly<Record<string, string>>,
    ): WorkspaceRuntimeReconciliation =>
      request.preset.sessions.every((spec) => keyMap[spec.sourceKey] !== undefined)
        ? {
            _tag: "Applied",
            state: { kind: "succeeded", operation: "ApplyPreset", request, keyMap },
          }
        : { _tag: "NotApplied" };

    const reconcilePresetLaunch = (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
      sourceKey: string,
      targetKey: string,
      region: "workflow" | "terminal",
      baselineKeys: ReadonlySet<string>,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      Effect.suspend(() =>
        port.read(request.target).pipe(
          Effect.matchEffect({
            onFailure: (cause) => Effect.succeed({ _tag: "Unknown", cause } satisfies WorkspaceRuntimeReconciliation),
            onSuccess: (runtime) => {
              const candidates = runtime.sessions.filter(
                (session) =>
                  !baselineKeys.has(session.key) &&
                  session.target_key === targetKey &&
                  session.display_region === region,
              );
              if (candidates.length === 0) {
                return Effect.succeed({ _tag: "NotApplied" } satisfies WorkspaceRuntimeReconciliation);
              }
              if (candidates.length !== 1) {
                return Effect.succeed({
                  _tag: "Unknown",
                  cause: new Error("runtime authority returned multiple matching preset sessions"),
                } satisfies WorkspaceRuntimeReconciliation);
              }
              return updatePresetMapping(request, sourceKey, candidates[0]!.key).pipe(
                Effect.map((keyMap) => presetReconciliationResult(request, keyMap)),
              );
            },
          }),
        ),
      );

    const reconcilePresetRename = (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
      sourceKey: string,
      sessionKey: string,
      label: string,
    ): Effect.Effect<WorkspaceRuntimeReconciliation> =>
      Effect.suspend(() =>
        port.read(request.target).pipe(
          Effect.matchEffect({
            onFailure: (cause) => Effect.succeed({ _tag: "Unknown", cause } satisfies WorkspaceRuntimeReconciliation),
            onSuccess: (runtime) => {
              const session = runtime.sessions.find((candidate) => candidate.key === sessionKey);
              if (session?.label !== label) {
                return Effect.succeed({ _tag: "NotApplied" } satisfies WorkspaceRuntimeReconciliation);
              }
              return updatePresetMapping(request, sourceKey, session.key).pipe(
                Effect.map((keyMap) => presetReconciliationResult(request, keyMap)),
              );
            },
          }),
        ),
      );

    const settleFence = Effect.fn("WorkspaceRuntimeWorkflow.settleFence")(function* (
      fence: WorkspaceRuntimeMutationFence,
      explicitRetry: boolean,
    ) {
      const reconciliation = yield* fence.reconcile;
      switch (reconciliation._tag) {
        case "Applied":
          yield* clearFence(fence);
          if (reconciliation.state.operation === "ApplyPreset") {
            yield* clearPresetProgress(reconciliation.state.request);
          }
          yield* storeAndPresentMutation(reconciliation.state);
          return true;
        case "Unknown":
          yield* storeAndPresentMutation(uncertainMutationState(fence.request, fence.error, reconciliation.cause));
          return true;
        case "NotApplied":
          yield* clearFence(fence);
          if (explicitRetry) {
            yield* removeStoredMutation(fence.request.key);
            return false;
          }
          yield* storeAndPresentMutation(failedMutationState(fence.request, fence.error));
          return true;
      }
    });

    const retainAndSettleFence = Effect.fn("WorkspaceRuntimeWorkflow.retainAndSettleFence")(function* (
      fence: WorkspaceRuntimeMutationFence,
    ) {
      yield* Ref.update(mutationFences, (current) => new Map(current).set(fence.request.key, fence));
      yield* settleFence(fence, false);
    });

    const runRecoverableMutation = Effect.fn("WorkspaceRuntimeWorkflow.runRecoverableMutation")(function* <A>(
      request: AcceptedWorkspaceRuntimeRequest,
      mutation: Effect.Effect<A, WorkspaceRuntimePortFailure>,
      reconcile: Effect.Effect<WorkspaceRuntimeReconciliation>,
      succeeded: (value: A) => WorkspaceRuntimeSucceededState,
    ) {
      yield* mutation.pipe(
        Effect.matchEffect({
          onFailure: (error) =>
            error instanceof TransientTransportError
              ? retainAndSettleFence({ request, error, reconcile })
              : storeAndPresentMutation(failedMutationState(request, error)),
          onSuccess: (value) => storeAndPresentMutation(succeeded(value)),
        }),
      );
    });

    const ensurePresetSessionLabel = Effect.fn("WorkspaceRuntimeWorkflow.ensurePresetSessionLabel")(function* (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
      sourceKey: string,
      session: RuntimeSession,
      label: string,
    ) {
      if (label === "" || session.label === label) return true;
      const renamed = yield* Effect.result(port.rename(request.target, session.key, label));
      if (Result.isSuccess(renamed)) return true;
      if (renamed.failure instanceof TransientTransportError) {
        yield* retainAndSettleFence({
          request,
          error: renamed.failure,
          reconcile: reconcilePresetRename(request, sourceKey, session.key, label),
        });
      } else {
        yield* storeAndPresentMutation(failedMutationState(request, renamed.failure));
      }
      return false;
    });

    const executePresetMutation = Effect.fn("WorkspaceRuntimeWorkflow.executePresetMutation")(function* (
      request: AcceptedWorkspaceRuntimeApplyPresetRequest,
    ) {
      const retained = (yield* Ref.get(presetProgress)).get(request.key);
      let keyMap: Readonly<Record<string, string>> = retained?.presetID === request.preset.id ? retained.keyMap : {};
      for (const spec of request.preset.sessions) {
        const label = spec.label.trim();
        const retainedKey = keyMap[spec.sourceKey];
        if (retainedKey !== undefined) {
          const runtime = yield* Effect.result(port.read(request.target));
          if (Result.isFailure(runtime)) {
            yield* storeAndPresentMutation(failedMutationState(request, runtime.failure));
            return;
          }
          const retainedSession = runtime.success.sessions.find((session) => session.key === retainedKey);
          if (retainedSession === undefined) {
            yield* storeAndPresentMutation(
              failedMutationState(
                request,
                InvalidExternalPayload.make({
                  operation: "reconcile retained preset session",
                  cause: new Error("a previously launched preset session is no longer present"),
                }),
              ),
            );
            return;
          }
          if (!(yield* ensurePresetSessionLabel(request, spec.sourceKey, retainedSession, label))) return;
          continue;
        }

        const baseline = yield* Effect.result(port.read(request.target));
        if (Result.isFailure(baseline)) {
          yield* storeAndPresentMutation(failedMutationState(request, baseline.failure));
          return;
        }
        const baselineKeys = new Set(baseline.success.sessions.map((session) => session.key));
        const launched = yield* Effect.result(port.launch(request.target, spec.targetKey, spec.region));
        if (Result.isFailure(launched)) {
          if (launched.failure instanceof TransientTransportError) {
            yield* retainAndSettleFence({
              request,
              error: launched.failure,
              reconcile: reconcilePresetLaunch(request, spec.sourceKey, spec.targetKey, spec.region, baselineKeys),
            });
          } else {
            yield* storeAndPresentMutation(failedMutationState(request, launched.failure));
          }
          return;
        }
        keyMap = yield* updatePresetMapping(request, spec.sourceKey, launched.success.key);
        if (!(yield* ensurePresetSessionLabel(request, spec.sourceKey, launched.success, label))) return;
      }
      yield* clearPresetProgress(request);
      yield* storeAndPresentMutation({ kind: "succeeded", operation: "ApplyPreset", request, keyMap });
    });

    const executeMutation = Effect.fn("WorkspaceRuntimeWorkflow.executeMutation")(function* (
      request: AcceptedWorkspaceRuntimeRequest,
    ) {
      switch (request._tag) {
        case "Launch": {
          yield* port.read(request.target).pipe(
            Effect.matchEffect({
              onFailure: (error) => storeAndPresentMutation(failedMutationState(request, error)),
              onSuccess: (baseline) => {
                const baselineKeys = new Set(baseline.sessions.map((session) => session.key));
                return runRecoverableMutation(
                  request,
                  port.launch(request.target, request.targetKey, request.region),
                  reconcileLaunch(request, baselineKeys),
                  (session) => ({ kind: "succeeded", operation: "Launch", request, session }),
                );
              },
            }),
          );
          return;
        }
        case "Stop": {
          const operation =
            request.target.hostKey === undefined ? "stop workspace session" : "stop fleet workspace session";
          yield* runRecoverableMutation(
            request,
            port.stop(request.target, request.sessionKey).pipe(
              Effect.timeoutOrElse({
                duration: workspaceRuntimeStopTimeout,
                orElse: () =>
                  Effect.fail(
                    TransientTransportError.make({
                      operation,
                      cause: new Error("timed out waiting for the session to stop"),
                    }),
                  ),
              }),
            ),
            reconcileStop(request),
            () => ({ kind: "succeeded", operation: "Stop", request }),
          );
          return;
        }
        case "Rename": {
          yield* runRecoverableMutation(
            request,
            port.rename(request.target, request.sessionKey, request.label),
            reconcileRename(request),
            (session) => ({ kind: "succeeded", operation: "Rename", request, session }),
          );
          return;
        }
        case "ApplyPreset": {
          yield* executePresetMutation(request);
          return;
        }
        case "Refresh": {
          const state = yield* port.refresh(request.target).pipe(
            Effect.match({
              onFailure: (error): WorkspaceRuntimeMutationState => ({
                kind: "failed",
                operation: "Refresh",
                request,
                error,
              }),
              onSuccess: (workspace): WorkspaceRuntimeMutationState => ({
                kind: "succeeded",
                operation: "Refresh",
                request,
                workspace,
              }),
            }),
          );
          yield* storeAndPresentMutation(state);
          return;
        }
        case "RetrySetup": {
          const state = yield* port.retry(request.target).pipe(
            Effect.match({
              onFailure: (error): WorkspaceRuntimeMutationState => ({
                kind: "failed",
                operation: "RetrySetup",
                request,
                error,
              }),
              onSuccess: (workspace): WorkspaceRuntimeMutationState => ({
                kind: "succeeded",
                operation: "RetrySetup",
                request,
                workspace,
              }),
            }),
          );
          yield* storeAndPresentMutation(state);
          return;
        }
        case "Delete": {
          yield* runRecoverableMutation(
            request,
            port.delete(request.target, request.options.force),
            reconcileDelete(request),
            (result) => ({ kind: "succeeded", operation: "Delete", request, result }),
          );
        }
      }
    });

    const submitMutation = Effect.fn("WorkspaceRuntimeWorkflow.submitMutation")(function* (
      request: WorkspaceRuntimeMutationRequest,
    ) {
      yield* mutationRecoveryLock.withPermit(
        Effect.gen(function* () {
          const key = mutationKey(request);
          const existingFence = (yield* Ref.get(mutationFences)).get(key);
          if (existingFence !== undefined && (yield* settleFence(existingFence, true))) return;

          const admission = yield* mutationLock.withPermit(
            Effect.gen(function* () {
              const current = yield* Ref.get(mutationStates);
              if (current.has(key)) return undefined;
              const mutationID = yield* Ref.getAndUpdate(mutationSequence, (value) => value + 1);
              const targetKey = runtimeTargetKey(request.target.workspaceId, request.target.hostKey);
              const originPresenterID = (yield* Ref.get(mutationPresenters)).get(targetKey)?.at(-1)?.sessionID;
              const accepted = {
                ...request,
                key,
                mutationID,
                ...(originPresenterID === undefined ? {} : { originPresenterID }),
              } satisfies AcceptedWorkspaceRuntimeRequest;
              const pending = pendingMutationState(accepted);
              yield* Ref.set(mutationStates, new Map(current).set(key, pending));
              return { accepted, pending };
            }),
          );
          if (admission === undefined) return;
          yield* Effect.forkIn(executeMutation(admission.accepted), scope);
          yield* presentStoredMutation(admission.pending);
        }),
      );
    });

    const read = Effect.fn("WorkspaceRuntimeWorkflow.read")(function* (
      owner: string,
      workspaceId: string,
      hostKey?: string,
      options: WorkspaceRuntimeReadOptions = {},
    ) {
      const target = { workspaceId, ...(hostKey === undefined ? {} : { hostKey }) };
      const targetKey = runtimeTargetKey(workspaceId, hostKey);
      const fiber = yield* SynchronizedRef.modifyEffect(active, (current) => {
        const existing = current.get(owner);
        if (options.force !== true && existing?.targetKey === targetKey) {
          return Effect.succeed<
            readonly [
              Fiber.Fiber<WorkspaceRuntimeState, WorkspaceRuntimePortFailure>,
              ReadonlyMap<string, ActiveRuntimeRead>,
            ]
          >([existing.fiber, current]);
        }
        return FiberMap.run(reads, owner, port.read(target)).pipe(
          Effect.map((nextFiber) => [nextFiber, new Map(current).set(owner, { targetKey, fiber: nextFiber })]),
        );
      });
      const exit = yield* Fiber.await(fiber).pipe(
        Effect.ensuring(
          SynchronizedRef.update(active, (current) => {
            if (current.get(owner)?.fiber !== fiber) return current;
            const next = new Map(current);
            next.delete(owner);
            return next;
          }),
        ),
      );
      if (Exit.isFailure(exit) && Cause.hasInterruptsOnly(exit.cause)) return Option.none();
      return Option.some(yield* exit);
    });

    const release = Effect.fn("WorkspaceRuntimeWorkflow.release")(function* (owner: string) {
      yield* SynchronizedRef.update(active, (current) => {
        if (!current.has(owner)) return current;
        const next = new Map(current);
        next.delete(owner);
        return next;
      });
      yield* FiberMap.remove(reads, owner);
    });

    const reconcileAcceptedLaunch = Effect.fn("WorkspaceRuntimeWorkflow.reconcileAcceptedLaunch")(function* (
      request: AcceptedWorkspaceLaunchReconciliation,
    ) {
      const owner = makeWorkspaceRuntimeOwner("accepted-launch");
      const stillAwaitingSession = () => {
        const current = pendingWorkspaceLaunch(request.target.workspaceId, request.target.hostKey);
        return (
          current?.phase === "awaiting_session" &&
          current.sessionKey === request.sessionKey &&
          current.acceptedAt === request.acceptedAt
        );
      };
      const remaining = Math.max(
        0,
        request.acceptedAt + acceptedLaunchReconciliationWindowMillis - (yield* Clock.currentTimeMillis),
      );
      const observed = yield* Effect.gen(function* () {
        while (stillAwaitingSession()) {
          const result = yield* read(owner, request.target.workspaceId, request.target.hostKey, { force: true }).pipe(
            Effect.catch(() => Effect.succeed(Option.none())),
          );
          if (Option.isSome(result) && result.value.sessions.some((session) => session.key === request.sessionKey)) {
            completeAcceptedWorkspaceLaunch(request.target.workspaceId, request.target.hostKey, request.sessionKey);
            return true;
          }
          yield* Effect.sleep("500 millis");
        }
        return false;
      }).pipe(Effect.timeoutOption(Duration.millis(remaining)), Effect.ensuring(release(owner)));
      if (Option.isSome(observed) && observed.value) return;
      if (
        expireAcceptedWorkspaceLaunch(
          request.target.workspaceId,
          request.target.hostKey,
          request.sessionKey,
          request.acceptedAt,
        )
      ) {
        yield* request.onExpired;
      }
    });

    const claimPresenter = Effect.fn("WorkspaceRuntimeWorkflow.claimPresenter")(function* (
      target: WorkspaceRuntimeTarget,
      sessionID: string,
      observe: WorkspaceRuntimeMutationObserver,
      options: WorkspaceRuntimePresenterOptions = {},
    ) {
      const targetKey = runtimeTargetKey(target.workspaceId, target.hostKey);
      const states = yield* mutationLock.withPermit(
        Effect.gen(function* () {
          yield* Ref.update(mutationPresenters, (current) =>
            new Map(current).set(targetKey, [
              ...(current.get(targetKey) ?? []).filter((presenter) => presenter.sessionID !== sessionID),
              {
                sessionID,
                observe,
                releaseWhenAcknowledged: options.releaseWhenAcknowledged === true,
                presentationIsCurrent: options.presentationIsCurrent,
              },
            ]),
          );
          return Array.from((yield* Ref.get(mutationStates)).values()).filter(
            (state) => runtimeTargetKey(state.request.target.workspaceId, state.request.target.hostKey) === targetKey,
          );
        }),
      );
      yield* Effect.forEach(states, (state) => presentStoredMutation(state, sessionID), { discard: true });
    });

    const releasePresenter = Effect.fn("WorkspaceRuntimeWorkflow.releasePresenter")(function* (
      target: WorkspaceRuntimeTarget,
      sessionID: string,
    ) {
      const targetKey = runtimeTargetKey(target.workspaceId, target.hostKey);
      yield* mutationLock.withPermit(
        Ref.update(mutationPresenters, (current) => {
          const presenters = current.get(targetKey);
          if (presenters === undefined || !presenters.some((presenter) => presenter.sessionID === sessionID)) {
            return current;
          }
          const next = new Map(current);
          const remaining = presenters.filter((presenter) => presenter.sessionID !== sessionID);
          if (remaining.length === 0) next.delete(targetKey);
          else next.set(targetKey, remaining);
          return next;
        }),
      );
    });

    return {
      read,
      launch: (target, targetKey, region, placement) =>
        submitMutation({ _tag: "Launch", target, targetKey, region, placement }),
      rename: (target, sessionKey, label) => submitMutation({ _tag: "Rename", target, sessionKey, label }),
      stop: (target, sessionKey) => submitMutation({ _tag: "Stop", target, sessionKey }),
      applyPreset: (target, preset) => submitMutation({ _tag: "ApplyPreset", target, preset }),
      refresh: (target) => submitMutation({ _tag: "Refresh", target }),
      retrySetup: (target) => submitMutation({ _tag: "RetrySetup", target }),
      delete: (target, options) => submitMutation({ _tag: "Delete", target, options }),
      reconcileAcceptedLaunch,
      claimPresenter,
      releasePresenter,
      release,
    };
  });
}

function normalizeWorkspaceRuntime(runtime: WorkspaceRuntime): WorkspaceRuntimeState {
  return {
    ...runtime,
    launch_targets: runtime.launch_targets ?? [],
    sessions: runtime.sessions ?? [],
  };
}

const FleetLaunchTargetPayload = Schema.Struct({
  available: Schema.Boolean,
  command: Schema.optionalKey(Schema.NullOr(Schema.Array(Schema.String))),
  disabled_reason: Schema.optionalKey(Schema.String),
  key: Schema.String,
  kind: Schema.String,
  label: Schema.String,
  source: Schema.String,
});

const FleetRuntimeSessionPayload = Schema.Struct({
  created_at: Schema.String,
  display_region: Schema.String,
  exit_code: Schema.optionalKey(Schema.Number),
  exited_at: Schema.optionalKey(Schema.String),
  key: Schema.String,
  kind: Schema.String,
  label: Schema.String,
  status: Schema.String,
  target_key: Schema.String,
  workspace_id: Schema.String,
});

const FleetWorkspaceRuntimePayload = Schema.Struct({
  launch_targets: Schema.NullOr(Schema.Array(FleetLaunchTargetPayload)),
  sessions: Schema.NullOr(Schema.Array(FleetRuntimeSessionPayload)),
});

const decodeFleetWorkspaceRuntime = Effect.fn("WorkspaceRuntimePort.decodeFleetRuntime")(function* (input: unknown) {
  const decoded = yield* Schema.decodeUnknownEffect(FleetWorkspaceRuntimePayload)(input).pipe(
    Effect.mapError((cause) => InvalidExternalPayload.make({ operation: "decode fleet workspace runtime", cause })),
  );
  const runtime: WorkspaceRuntime = {
    launch_targets: (decoded.launch_targets ?? []).map((target): LaunchTarget => {
      const { command, ...rest } = target;
      return {
        ...rest,
        ...(command === undefined || command === null ? {} : { command: [...command] }),
      };
    }),
    sessions: (decoded.sessions ?? []).map((session): RuntimeSession => ({ ...session })),
  };
  return runtime;
});

const decodeFleetRuntimeSession = Effect.fn("WorkspaceRuntimePort.decodeFleetSession")(function* (input: unknown) {
  const session: RuntimeSession = yield* Schema.decodeUnknownEffect(FleetRuntimeSessionPayload)(input).pipe(
    Effect.mapError((cause) =>
      InvalidExternalPayload.make({ operation: "decode fleet workspace runtime session", cause }),
    ),
  );
  return session;
});

function opaqueRuntimeFailure(
  operation: string,
  error: unknown,
): Effect.Effect<never, ApiProblemError | InvalidExternalPayload> {
  return isProblem(error)
    ? Effect.fail(new ApiProblemError({ operation, problem: error }))
    : Effect.fail(
        InvalidExternalPayload.make({
          operation: `decode ${operation} error response`,
          cause: error,
        }),
      );
}

async function rawResponseBody(response: Response): Promise<unknown> {
  if ([204, 205, 304].includes(response.status)) return undefined;
  const text = await response.text();
  if (text === "") return undefined;
  return response.headers.get("Content-Type")?.includes("json") ? JSON.parse(text) : text;
}

export const WorkspaceRuntimePortLive = Effect.gen(function* () {
  const api = yield* GeneratedApi;

  const read = Effect.fn("WorkspaceRuntimePort.read")(function* (target: WorkspaceRuntimeTarget) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const data = yield* api.execute("load fleet workspace runtime", (signal) =>
        api.client.FleetService.getFleetWorkspaceRuntime({ hostKey, id: workspaceId }, { signal }),
      );
      return normalizeWorkspaceRuntime(yield* decodeFleetWorkspaceRuntime(data));
    }
    const runtime = yield* api.execute("load workspace runtime", (signal) =>
      api.client.WorkspacesService.getWorkspaceRuntime({ id: workspaceId }, { signal }),
    );
    return normalizeWorkspaceRuntime(runtime);
  });

  const launch = Effect.fn("WorkspaceRuntimePort.launch")(function* (
    target: WorkspaceRuntimeTarget,
    targetKey: string,
    region: "workflow" | "terminal",
  ) {
    const { workspaceId, hostKey } = target;
    const body = { target_key: targetKey, display_region: region };
    if (hostKey !== undefined) {
      const data = yield* api.execute("launch fleet workspace session", (signal) =>
        api.client.FleetService.launchFleetWorkspaceRuntimeSession({ hostKey, id: workspaceId }, body, { signal }),
      );
      return yield* decodeFleetRuntimeSession(data);
    }
    return yield* api.execute("launch workspace session", (signal) =>
      api.client.WorkspacesService.launchWorkspaceRuntimeSession({ id: workspaceId }, body, { signal }),
    );
  });

  const rename = Effect.fn("WorkspaceRuntimePort.rename")(function* (
    target: WorkspaceRuntimeTarget,
    sessionKey: string,
    label: string,
  ) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const data = yield* api.execute("rename fleet workspace session", (signal) =>
        api.client.FleetService.renameFleetWorkspaceRuntimeSession(
          { hostKey, id: workspaceId, sessionKey },
          { label },
          { signal },
        ),
      );
      return yield* decodeFleetRuntimeSession(data);
    }
    return yield* api.execute("rename workspace session", (signal) =>
      api.client.WorkspacesService.renameWorkspaceRuntimeSession(
        { id: workspaceId, sessionKey: sessionKey },
        { label },
        { signal },
      ),
    );
  });

  const stop = Effect.fn("WorkspaceRuntimePort.stop")(function* (target: WorkspaceRuntimeTarget, sessionKey: string) {
    const { workspaceId, hostKey } = target;
    const operation = hostKey === undefined ? "stop workspace session" : "stop fleet workspace session";
    if (hostKey !== undefined) {
      yield* api.execute(operation, (signal) =>
        api.client.FleetService.stopFleetWorkspaceRuntimeSession({ hostKey, id: workspaceId, sessionKey }, { signal }),
      );
      return;
    }
    yield* api.execute(operation, (signal) =>
      api.client.WorkspacesService.stopWorkspaceRuntimeSession({ id: workspaceId, sessionKey }, { signal }),
    );
  });

  const refresh = Effect.fn("WorkspaceRuntimePort.refresh")(function* (target: WorkspaceRuntimeTarget) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const workspace = yield* api.execute("refresh fleet workspace", (signal) =>
        api.client.FleetService.refreshFleetWorkspace({ hostKey, id: workspaceId }, { signal }),
      );
      return yield* decodeWorkspaceDetail(workspace, hostKey);
    }
    const workspace = yield* api.execute("refresh workspace", (signal) =>
      api.client.WorkspacesService.refreshWorkspace({ id: workspaceId }, { signal }),
    );
    return yield* decodeWorkspaceDetail(workspace);
  });

  const retry = Effect.fn("WorkspaceRuntimePort.retrySetup")(function* (target: WorkspaceRuntimeTarget) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const workspace = yield* api.execute("retry fleet workspace setup", (signal) =>
        api.client.FleetService.retryFleetWorkspace({ hostKey, id: workspaceId }, { signal }),
      );
      return yield* decodeWorkspaceDetail(workspace, hostKey);
    }
    const workspace = yield* api.execute("retry workspace setup", (signal) =>
      api.client.WorkspacesService.retryWorkspace({ id: workspaceId }, { signal }),
    );
    return yield* decodeWorkspaceDetail(workspace);
  });

  const deleteWorkspace = Effect.fn("WorkspaceRuntimePort.delete")(function* (
    target: WorkspaceRuntimeTarget,
    force: boolean,
  ) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const response = yield* Effect.tryPromise({
        try: (signal) =>
          orvalRequest(
            api.client.FleetService.getDeleteFleetWorkspaceUrl(
              { hostKey, id: workspaceId },
              force ? { force: true } : undefined,
            ),
            {
              method: "DELETE",
              signal,
            },
          ),
        catch: (cause) => TransientTransportError.make({ operation: "delete fleet workspace", cause }),
      });
      const body = response.ok
        ? undefined
        : yield* Effect.tryPromise({
            try: () => rawResponseBody(response),
            catch: (cause) =>
              TransientTransportError.make({ operation: "decode delete fleet workspace response", cause }),
          });
      const error = isProblem(body) ? body : undefined;
      return { response, ...(error === undefined ? {} : { error }) };
    }
    const response = yield* Effect.tryPromise({
      try: (signal) =>
        orvalRequest(
          api.client.WorkspacesService.getDeleteWorkspaceUrl({ id: workspaceId }, force ? { force: true } : undefined),
          {
            method: "DELETE",
            signal,
          },
        ),
      catch: (cause) => TransientTransportError.make({ operation: "delete workspace", cause }),
    });
    const body = response.ok
      ? undefined
      : yield* Effect.tryPromise({
          try: () => rawResponseBody(response),
          catch: (cause) => TransientTransportError.make({ operation: "decode delete workspace response", cause }),
        });
    const error = isProblem(body) ? body : undefined;
    return { response, ...(error === undefined ? {} : { error }) };
  });

  const isDeleted = Effect.fn("WorkspaceRuntimePort.isDeleted")(function* (target: WorkspaceRuntimeTarget) {
    const { workspaceId, hostKey } = target;
    if (hostKey !== undefined) {
      const response = yield* Effect.tryPromise({
        try: (signal) =>
          orvalRequest(api.client.FleetService.getGetFleetWorkspaceUrl({ hostKey, id: workspaceId }), { signal }),
        catch: (cause) => TransientTransportError.make({ operation: "confirm fleet workspace deletion", cause }),
      });
      if (response.status === 404) return true;
      if (response.ok) return false;
      const body = yield* Effect.tryPromise({
        try: () => rawResponseBody(response),
        catch: (cause) =>
          TransientTransportError.make({ operation: "decode fleet workspace deletion authority", cause }),
      });
      return yield* opaqueRuntimeFailure("confirm fleet workspace deletion", body);
    }
    const response = yield* Effect.tryPromise({
      try: (signal) => orvalRequest(api.client.WorkspacesService.getGetWorkspaceUrl({ id: workspaceId }), { signal }),
      catch: (cause) => TransientTransportError.make({ operation: "confirm workspace deletion", cause }),
    });
    if (response.status === 404) return true;
    if (response.ok) return false;
    const body = yield* Effect.tryPromise({
      try: () => rawResponseBody(response),
      catch: (cause) => TransientTransportError.make({ operation: "decode workspace deletion authority", cause }),
    });
    return yield* opaqueRuntimeFailure("confirm workspace deletion", body);
  });

  return {
    read,
    launch,
    rename,
    stop,
    refresh,
    retry,
    delete: deleteWorkspace,
    isDeleted,
  } satisfies WorkspaceRuntimePort;
});

export class WorkspaceRuntimeWorkflow extends Context.Service<
  WorkspaceRuntimeWorkflow,
  WorkspaceRuntimeWorkflowService
>()("kenn-forge/WorkspaceRuntimeWorkflow") {}

export const WorkspaceRuntimeWorkflowLive = Layer.effect(WorkspaceRuntimeWorkflow)(
  WorkspaceRuntimePortLive.pipe(Effect.flatMap((port) => makeWorkspaceRuntimeWorkflow(port))),
);
