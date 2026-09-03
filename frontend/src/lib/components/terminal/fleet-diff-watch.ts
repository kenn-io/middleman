import { Duration, Effect, Option, Schedule, Schema } from "effect";
import { TransientTransportError } from "../../api/effect-errors.js";
import { GeneratedApi } from "../../api/generated-api.js";
import { orvalRequest } from "../../api/runtime.js";

export function shouldRetryFleetDiffWatch(status: number): boolean {
  if (status === 501) return false;
  return status === 408 || status === 409 || status === 425 || status === 429 || status >= 500;
}

export interface FleetDiffWatchResponse {
  readonly status: number;
  readonly body: unknown;
}

export interface FleetWorkspaceDiffWatchOptions {
  readonly workspaceId: string;
  readonly hostKey: string;
  readonly request: (
    workspaceId: string,
    hostKey: string,
    version: string,
  ) => Effect.Effect<FleetDiffWatchResponse, TransientTransportError>;
  readonly onChanged: (version: string) => Effect.Effect<void>;
}

class FleetDiffWatchUpdate extends Schema.Class<FleetDiffWatchUpdate>("FleetDiffWatchUpdate")({
  changed: Schema.optionalKey(Schema.Boolean),
  version: Schema.optionalKey(Schema.String),
}) {}

class RetryableFleetDiffWatchStatus extends Schema.TaggedError<RetryableFleetDiffWatchStatus>()(
  "RetryableFleetDiffWatchStatus",
  { status: Schema.Number },
) {}

const fleetDiffWatchRetrySchedule = Schedule.exponential("1 second").pipe(
  Schedule.jittered,
  Schedule.modifyDelay(({ duration }) => Effect.succeed(Duration.min(duration, Duration.seconds(30)))),
);

function isRetryableFleetDiffWatchFailure(failure: TransientTransportError | RetryableFleetDiffWatchStatus): boolean {
  return failure._tag === "TransientTransportError" || shouldRetryFleetDiffWatch(failure.status);
}

export const fleetWorkspaceDiffWatch = Effect.fn("FleetWorkspaceDiffWatch.run")(function* (
  options: FleetWorkspaceDiffWatchOptions,
) {
  const requestUpdate = Effect.fn("FleetWorkspaceDiffWatch.request")(function* (version: string) {
    const response = yield* options.request(options.workspaceId, options.hostKey, version);
    if (response.status < 200 || response.status >= 300) {
      return shouldRetryFleetDiffWatch(response.status)
        ? yield* Effect.fail(new RetryableFleetDiffWatchStatus({ status: response.status }))
        : Option.none<FleetDiffWatchUpdate>();
    }
    const decoded = yield* Schema.decodeUnknownEffect(FleetDiffWatchUpdate)(response.body).pipe(Effect.option);
    if (Option.isNone(decoded)) {
      return Option.none<FleetDiffWatchUpdate>();
    }
    const nextVersion = decoded.value.version;
    if (nextVersion === undefined || nextVersion === "") return Option.none<FleetDiffWatchUpdate>();
    return Option.some(decoded.value);
  });

  const loop: (version: string) => Effect.Effect<void, TransientTransportError | RetryableFleetDiffWatchStatus> =
    Effect.fn("FleetWorkspaceDiffWatch.loop")(function* (version: string) {
      const update = yield* requestUpdate(version).pipe(
        Effect.retry({
          schedule: fleetDiffWatchRetrySchedule,
          while: isRetryableFleetDiffWatchFailure,
        }),
      );
      if (Option.isNone(update)) return;
      const nextVersion = update.value.version;
      if (nextVersion === undefined || nextVersion === "") return;
      if (update.value.changed === true) {
        yield* options.onChanged(nextVersion);
      }
      return yield* Effect.suspend(() => loop(nextVersion));
    });

  return yield* loop("");
});

export const watchFleetWorkspaceDiff = Effect.fn("FleetWorkspaceDiffWatch.live")(function* (
  workspaceId: string,
  hostKey: string,
  onChanged: (version: string) => Effect.Effect<void>,
) {
  const api = yield* GeneratedApi;
  return yield* fleetWorkspaceDiffWatch({
    workspaceId,
    hostKey,
    request: (requestedWorkspaceId, requestedHostKey, version) =>
      Effect.tryPromise({
        try: async (signal) => {
          const response = await orvalRequest(
            api.client.FleetService.getWatchFleetWorkspaceDiffUrl(
              { hostKey: requestedHostKey, id: requestedWorkspaceId },
              version === "" ? {} : { version },
            ),
            { signal },
          );
          const text = [204, 205, 304].includes(response.status) ? "" : await response.text();
          const body = text === "" ? undefined : JSON.parse(text);
          return { status: response.status, body };
        },
        catch: (cause) => TransientTransportError.make({ operation: "watch fleet workspace diff", cause }),
      }),
    onChanged,
  });
});
