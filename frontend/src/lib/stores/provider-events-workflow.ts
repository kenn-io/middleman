import { Cause, Context, Deferred, Effect, Layer, Queue, Ref, Schema, Semaphore, Stream } from "effect";
import { ApiProblemError, InvalidExternalPayload, TransientTransportError } from "../api/effect-errors.js";
import { ProblemCodes, type ProblemCode } from "../api/problems.js";
import { isTransientFailure, reconnectSchedule, transientRetrySchedule } from "../api/retry-policy.js";
import { openEventSource } from "../browser/event-source.js";
import type { EventSourceFactory } from "../browser/event-source.js";

export type ProviderEventsConnectionState = "connecting" | "connected" | "reconnecting" | "disconnected";

export class ConfigChangedEvent extends Schema.Class<ConfigChangedEvent>("ConfigChangedEvent")({
  valid: Schema.Boolean,
  error: Schema.optionalKey(Schema.String),
  restart_required: Schema.Boolean,
}) {}

const providerItemFields = {
  provider: Schema.String,
  platform_host: Schema.String,
  repo_path: Schema.String,
  owner: Schema.String,
  name: Schema.String,
  number: Schema.Number,
};

export class WorkspacePushedHeadChangedEvent extends Schema.Class<WorkspacePushedHeadChangedEvent>(
  "WorkspacePushedHeadChangedEvent",
)({
  workspace_id: Schema.String,
  ...providerItemFields,
  old_sha: Schema.String,
  new_sha: Schema.String,
  remote: Schema.String,
  branch: Schema.String,
  tracking_ref: Schema.String,
  observed_at: Schema.String,
}) {}

export class WorkspacePRAssociatedEvent extends Schema.Class<WorkspacePRAssociatedEvent>("WorkspacePRAssociatedEvent")({
  workspace_id: Schema.String,
  provider: Schema.String,
  platform_host: Schema.String,
  repo_path: Schema.String,
  owner: Schema.String,
  name: Schema.String,
  issue_number: Schema.Number,
  pr_number: Schema.Number,
  associated_at: Schema.String,
}) {}

export class WorkspaceCreatedEvent extends Schema.Class<WorkspaceCreatedEvent>("WorkspaceCreatedEvent")({
  id: Schema.String,
  created: Schema.Boolean,
}) {}

export class WorkspaceStatusEvent extends Schema.Class<WorkspaceStatusEvent>("WorkspaceStatusEvent")({
  id: Schema.optionalKey(Schema.String),
  status: Schema.optionalKey(Schema.String),
}) {}

export class WorkspaceDiffEvent extends Schema.Class<WorkspaceDiffEvent>("WorkspaceDiffEvent")({
  workspace_id: Schema.String,
  version: Schema.String,
}) {}

export class WorkspaceDeletedEvent extends Schema.Class<WorkspaceDeletedEvent>("WorkspaceDeletedEvent")({
  workspace_id: Schema.String,
  ...providerItemFields,
  item_type: Schema.String,
}) {}

export class WorkspacePRRefreshQueuedEvent extends Schema.Class<WorkspacePRRefreshQueuedEvent>(
  "WorkspacePRRefreshQueuedEvent",
)({
  workspace_id: Schema.String,
  ...providerItemFields,
  head_sha: Schema.String,
  priority: Schema.String,
  queued_at: Schema.String,
}) {}

export class PRDetailRefreshedEvent extends Schema.Class<PRDetailRefreshedEvent>("PRDetailRefreshedEvent")({
  ...providerItemFields,
  head_sha: Schema.String,
  synced_at: Schema.String,
  warnings: Schema.Array(Schema.String),
}) {}

export class PRCIRefreshQueuedEvent extends Schema.Class<PRCIRefreshQueuedEvent>("PRCIRefreshQueuedEvent")({
  ...providerItemFields,
  head_sha: Schema.String,
  priority: Schema.String,
  queued_at: Schema.String,
}) {}

export class PRCIRefreshedEvent extends Schema.Class<PRCIRefreshedEvent>("PRCIRefreshedEvent")({
  ...providerItemFields,
  head_sha: Schema.String,
  refreshed_at: Schema.String,
  warnings: Schema.Array(Schema.String),
}) {}

export class WorkflowDispatchProgressEvent extends Schema.Class<WorkflowDispatchProgressEvent>(
  "WorkflowDispatchProgressEvent",
)({
  provider: Schema.String,
  platform_host: Schema.String,
  repo_path: Schema.String,
  owner: Schema.String,
  name: Schema.String,
  workflow_id: Schema.String,
  dispatch_id: Schema.String,
  status: Schema.Literals(["located", "updated", "unresolved"]),
  run: Schema.optionalKey(Schema.Unknown),
}) {}

export class DeferredMergeCompletedEvent extends Schema.Class<DeferredMergeCompletedEvent>(
  "DeferredMergeCompletedEvent",
)({
  ...providerItemFields,
  head_sha: Schema.String,
  status: Schema.Literals(["merged", "failed"]),
  merged: Schema.optionalKey(Schema.Boolean),
  sha: Schema.optionalKey(Schema.String),
  message: Schema.optionalKey(Schema.String),
  error: Schema.optionalKey(Schema.String),
  workspace_cleanup_warning: Schema.optionalKey(Schema.String),
  workspace_cleanup_pending: Schema.optionalKey(Schema.Boolean),
  completed_at: Schema.String,
}) {}

export class SyncStatusEvent extends Schema.Class<SyncStatusEvent>("SyncStatusEvent")({
  running: Schema.Boolean,
  current_repo: Schema.optionalKey(Schema.String),
  last_error: Schema.optionalKey(Schema.String),
  last_run_at: Schema.optionalKey(Schema.String),
  progress: Schema.optionalKey(Schema.String),
}) {}

export class HubConnectionChangedEvent extends Schema.Class<HubConnectionChangedEvent>("HubConnectionChangedEvent")({
  connected: Schema.Boolean,
}) {}

export class ReconnectStaleEvent extends Schema.Class<ReconnectStaleEvent>("ReconnectStaleEvent")({
  hub_connected: Schema.optionalKey(Schema.Boolean),
}) {}

export type ProviderEvent =
  | { readonly type: "data_changed" }
  | { readonly type: "sync_status"; readonly payload: SyncStatusEvent }
  | {
      readonly type: "hub_connection_changed";
      readonly payload: HubConnectionChangedEvent;
    }
  | { readonly type: "config.changed"; readonly payload: ConfigChangedEvent }
  | { readonly type: "reconnect.stale"; readonly payload: ReconnectStaleEvent }
  | { readonly type: "workspace_created"; readonly payload: WorkspaceCreatedEvent }
  | { readonly type: "workspace_status"; readonly payload: WorkspaceStatusEvent }
  | { readonly type: "workspace_diff_ready"; readonly payload: WorkspaceDiffEvent }
  | { readonly type: "workspace_diff_changed"; readonly payload: WorkspaceDiffEvent }
  | { readonly type: "workspace_deleted"; readonly payload: WorkspaceDeletedEvent }
  | { readonly type: "workspace_pushed_head_changed"; readonly payload: WorkspacePushedHeadChangedEvent }
  | { readonly type: "workspace_pr_associated"; readonly payload: WorkspacePRAssociatedEvent }
  | { readonly type: "workspace_pr_refresh_queued"; readonly payload: WorkspacePRRefreshQueuedEvent }
  | { readonly type: "pr_detail_refreshed"; readonly payload: PRDetailRefreshedEvent }
  | { readonly type: "pr_ci_refresh_queued"; readonly payload: PRCIRefreshQueuedEvent }
  | { readonly type: "pr_ci_refreshed"; readonly payload: PRCIRefreshedEvent }
  | { readonly type: "deferred_merge_completed"; readonly payload: DeferredMergeCompletedEvent }
  | { readonly type: "workflow_dispatch_progress"; readonly payload: WorkflowDispatchProgressEvent };

export type ProviderEventsError = ApiProblemError | InvalidExternalPayload | TransientTransportError;

interface ProviderEventsCheckpointLease {
  readonly cancelled: Effect.Effect<void>;
  readonly get: Effect.Effect<string>;
  readonly set: (checkpoint: string, resetEpoch: boolean) => Effect.Effect<void>;
}

interface ProviderEventsCheckpointShape {
  readonly acquire: Effect.Effect<ProviderEventsCheckpointLease>;
}

export class ProviderEventsCheckpoint extends Context.Service<
  ProviderEventsCheckpoint,
  ProviderEventsCheckpointShape
>()("kenn-forge/ProviderEventsCheckpoint") {}

export const ProviderEventsCheckpointLive = Layer.effect(
  ProviderEventsCheckpoint,
  Effect.gen(function* () {
    const checkpoint = yield* Ref.make<{
      readonly accepted: string;
      readonly current?: { readonly cancelled: Deferred.Deferred<void>; readonly id: number };
      readonly nextOwner: number;
    }>({ accepted: "", nextOwner: 0 });
    const ownership = yield* Semaphore.make(1);
    return {
      acquire: ownership.withPermit(
        Effect.gen(function* () {
          const cancelled = yield* Deferred.make<void>();
          const previous = yield* Ref.modify(checkpoint, (current) => {
            const id = current.nextOwner + 1;
            return [
              current.current,
              {
                ...current,
                current: { cancelled, id },
                nextOwner: id,
              },
            ];
          });
          if (previous !== undefined) yield* Deferred.succeed(previous.cancelled, undefined);
          const owner = yield* Ref.get(checkpoint).pipe(Effect.map((current) => current.nextOwner));
          return {
            cancelled: Deferred.await(cancelled),
            get: Ref.get(checkpoint).pipe(Effect.map((current) => current.accepted)),
            set: (accepted: string, resetEpoch: boolean) =>
              /^\d+$/.test(accepted)
                ? ownership.withPermit(
                    Ref.update(checkpoint, (current) => {
                      if (current.current?.id !== owner) return current;
                      if (resetEpoch || current.accepted === "" || BigInt(accepted) > BigInt(current.accepted)) {
                        return { ...current, accepted };
                      }
                      return current;
                    }),
                  )
                : Effect.die(new Error("Provider event checkpoint is not numeric")),
          };
        }),
      ),
    };
  }),
);

export interface ProviderEventsProgramOptions<Requirements = never> {
  readonly url: string;
  readonly onState?: (state: ProviderEventsConnectionState) => void;
  readonly onEvent?: (event: ProviderEvent) => Effect.Effect<void, ProviderEventsError, Requirements>;
  readonly onConsequenceFailure?: (failure: ApiProblemError, event: ProviderEvent) => void;
}

const retryableConsequenceProblemCodes = new Set<ProblemCode>([
  ProblemCodes.hubUnavailable,
  ProblemCodes.rateLimited,
  ProblemCodes.serviceUnavailable,
  ProblemCodes.upstreamError,
  ProblemCodes.internalError,
]);

function isRetryableProviderEventFailure(failure: unknown): failure is ApiProblemError | TransientTransportError {
  return (
    isTransientFailure(failure) ||
    (failure instanceof ApiProblemError && retryableConsequenceProblemCodes.has(failure.problem.code))
  );
}

type ProviderEventType = ProviderEvent["type"];

const providerEventTypes: ReadonlyArray<ProviderEventType> = [
  "data_changed",
  "sync_status",
  "hub_connection_changed",
  "config.changed",
  "reconnect.stale",
  "workspace_created",
  "workspace_status",
  "workspace_diff_ready",
  "workspace_diff_changed",
  "workspace_deleted",
  "workspace_pushed_head_changed",
  "workspace_pr_associated",
  "workspace_pr_refresh_queued",
  "pr_detail_refreshed",
  "pr_ci_refresh_queued",
  "pr_ci_refreshed",
  "deferred_merge_completed",
  "workflow_dispatch_progress",
];

interface ProviderEventFrame {
  readonly type: ProviderEventType;
  readonly data: string;
  readonly lastEventId: string;
}

type ProviderEventSourceSignal =
  | { readonly _tag: "Open" }
  | { readonly _tag: "Frame"; readonly frame: ProviderEventFrame };

const notifyState = (
  callback: ((state: ProviderEventsConnectionState) => void) | undefined,
  state: ProviderEventsConnectionState,
): Effect.Effect<void> => (callback === undefined ? Effect.void : Effect.sync(() => callback(state)));

const notifyEvent = <Requirements>(
  callback: ((event: ProviderEvent) => Effect.Effect<void, ProviderEventsError, Requirements>) | undefined,
  event: ProviderEvent,
): Effect.Effect<void, ProviderEventsError, Requirements> => (callback === undefined ? Effect.void : callback(event));

function resumeURL(url: string, checkpoint: string): string {
  if (checkpoint === "") return url;
  const separator = url.includes("?") ? "&" : "?";
  return `${url}${separator}since=${encodeURIComponent(checkpoint)}`;
}

function invalidFrame(frame: ProviderEventFrame, cause: unknown): InvalidExternalPayload {
  return InvalidExternalPayload.make({
    operation: `decode provider event ${frame.type}`,
    cause,
  });
}

const parseFrameData = Effect.fn("ProviderEvents.parseFrameData")(function* (frame: ProviderEventFrame) {
  return yield* Effect.try({
    try: (): unknown => JSON.parse(frame.data),
    catch: (cause) => invalidFrame(frame, cause),
  });
});

const decodeProviderEvent = Effect.fn("ProviderEvents.decodeFrame")(function* (frame: ProviderEventFrame) {
  const payload = yield* parseFrameData(frame);
  switch (frame.type) {
    case "data_changed":
      yield* Schema.decodeUnknownEffect(Schema.Struct({}))(payload).pipe(
        Effect.mapError((cause) => invalidFrame(frame, cause)),
      );
      return { type: "data_changed" } satisfies ProviderEvent;
    case "sync_status":
      return {
        type: "sync_status",
        payload: yield* Schema.decodeUnknownEffect(SyncStatusEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "hub_connection_changed":
      return {
        type: "hub_connection_changed",
        payload: yield* Schema.decodeUnknownEffect(HubConnectionChangedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "config.changed":
      return {
        type: "config.changed",
        payload: yield* Schema.decodeUnknownEffect(ConfigChangedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "reconnect.stale":
      return {
        type: "reconnect.stale",
        payload: yield* Schema.decodeUnknownEffect(ReconnectStaleEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_created":
      return {
        type: "workspace_created",
        payload: yield* Schema.decodeUnknownEffect(WorkspaceCreatedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_status":
      return {
        type: "workspace_status",
        payload: yield* Schema.decodeUnknownEffect(WorkspaceStatusEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_diff_ready":
    case "workspace_diff_changed":
      return {
        type: frame.type,
        payload: yield* Schema.decodeUnknownEffect(WorkspaceDiffEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_deleted":
      return {
        type: "workspace_deleted",
        payload: yield* Schema.decodeUnknownEffect(WorkspaceDeletedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_pushed_head_changed":
      return {
        type: "workspace_pushed_head_changed",
        payload: yield* Schema.decodeUnknownEffect(WorkspacePushedHeadChangedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_pr_associated":
      return {
        type: "workspace_pr_associated",
        payload: yield* Schema.decodeUnknownEffect(WorkspacePRAssociatedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workspace_pr_refresh_queued":
      return {
        type: "workspace_pr_refresh_queued",
        payload: yield* Schema.decodeUnknownEffect(WorkspacePRRefreshQueuedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "pr_detail_refreshed":
      return {
        type: "pr_detail_refreshed",
        payload: yield* Schema.decodeUnknownEffect(PRDetailRefreshedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "pr_ci_refresh_queued":
      return {
        type: "pr_ci_refresh_queued",
        payload: yield* Schema.decodeUnknownEffect(PRCIRefreshQueuedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "pr_ci_refreshed":
      return {
        type: "pr_ci_refreshed",
        payload: yield* Schema.decodeUnknownEffect(PRCIRefreshedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "deferred_merge_completed":
      return {
        type: "deferred_merge_completed",
        payload: yield* Schema.decodeUnknownEffect(DeferredMergeCompletedEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
    case "workflow_dispatch_progress":
      return {
        type: "workflow_dispatch_progress",
        payload: yield* Schema.decodeUnknownEffect(WorkflowDispatchProgressEvent)(payload).pipe(
          Effect.mapError((cause) => invalidFrame(frame, cause)),
        ),
      } satisfies ProviderEvent;
  }
});

const providerEventSourceStream = (url: string) =>
  Stream.callback<ProviderEventSourceSignal, TransientTransportError | InvalidExternalPayload, EventSourceFactory>(
    (queue) =>
      Effect.gen(function* () {
        const source = yield* openEventSource(url);
        const offer = (signal: ProviderEventSourceSignal): void => {
          if (Queue.offerUnsafe(queue, signal)) return;
          Queue.failCauseUnsafe(
            queue,
            Cause.fail(
              TransientTransportError.make({
                operation: `buffer provider events ${url}`,
                cause: new Error("Provider event buffer overflow"),
              }),
            ),
          );
        };
        const onOpen = (): void => offer({ _tag: "Open" });
        const onError = (cause: Event): void => {
          Queue.failCauseUnsafe(
            queue,
            Cause.fail(TransientTransportError.make({ operation: `read provider events ${url}`, cause })),
          );
        };
        const listeners = new Map<ProviderEventType, (event: Event) => void>();
        for (const type of providerEventTypes) {
          const listener = (event: Event): void => {
            if (!(event instanceof MessageEvent)) return;
            const data: unknown = event.data;
            if (typeof data !== "string") {
              Queue.failCauseUnsafe(
                queue,
                Cause.fail(
                  InvalidExternalPayload.make({
                    operation: `decode provider event ${type}`,
                    cause: new Error("Event data is not text"),
                  }),
                ),
              );
              return;
            }
            offer({
              _tag: "Frame",
              frame: {
                type,
                data,
                lastEventId: event.lastEventId,
              },
            });
          };
          listeners.set(type, listener);
          source.addEventListener(type, listener);
        }
        source.addEventListener("open", onOpen);
        source.addEventListener("error", onError, { once: true });
        yield* Effect.addFinalizer(() =>
          Effect.sync(() => {
            source.removeEventListener("open", onOpen);
            source.removeEventListener("error", onError);
            for (const [type, listener] of listeners) {
              source.removeEventListener(type, listener);
            }
          }),
        );
      }),
    { bufferSize: 64, strategy: "dropping" },
  );

export const providerEventsProgram = Effect.fn("ProviderEvents.run")(function* <Requirements>(
  options: ProviderEventsProgramOptions<Requirements>,
) {
  const checkpoint = yield* ProviderEventsCheckpoint;
  const lease = yield* checkpoint.acquire;
  yield* notifyState(options.onState, "connecting");

  const connectCycle = Effect.suspend(() => {
    let opened = false;
    const connectOnce = Effect.gen(function* () {
      const cursor = yield* lease.get;
      const url = resumeURL(options.url, cursor);
      yield* Stream.runForEach(providerEventSourceStream(url), (signal) => {
        if (signal._tag === "Open") {
          return Effect.sync(() => {
            opened = true;
          }).pipe(Effect.andThen(notifyState(options.onState, "connected")));
        }
        return Effect.gen(function* () {
          if (signal.frame.lastEventId !== "" && !/^\d+$/.test(signal.frame.lastEventId)) {
            return yield* Effect.fail(invalidFrame(signal.frame, new Error("Event id is not numeric")));
          }
          const event = yield* decodeProviderEvent(signal.frame);
          yield* notifyEvent(options.onEvent, event).pipe(
            Effect.retry({
              schedule: transientRetrySchedule,
              while: isRetryableProviderEventFailure,
            }),
            Effect.catchTag("ApiProblemError", (failure) =>
              isRetryableProviderEventFailure(failure)
                ? Effect.fail(failure)
                : Effect.sync(() => options.onConsequenceFailure?.(failure, event)),
            ),
          );
          if (signal.frame.lastEventId !== "") {
            yield* lease.set(signal.frame.lastEventId, event.type === "reconnect.stale");
          }
        });
      });
    });
    return connectOnce.pipe(
      Effect.tapError((failure) =>
        isRetryableProviderEventFailure(failure) ? notifyState(options.onState, "reconnecting") : Effect.void,
      ),
      Effect.retry({
        schedule: reconnectSchedule,
        while: (failure) => isRetryableProviderEventFailure(failure) && !opened,
      }),
      Effect.catchIf(
        (failure): failure is ApiProblemError | TransientTransportError =>
          isRetryableProviderEventFailure(failure) && opened,
        () => Effect.sleep("500 millis"),
      ),
    );
  });

  const run = Effect.forever(connectCycle).pipe(Effect.tapError(() => notifyState(options.onState, "disconnected")));
  return yield* Effect.raceFirst(run, lease.cancelled.pipe(Effect.andThen(Effect.interrupt)));
});
