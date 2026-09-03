import { Cache, Context, Duration, Effect, Layer, Ref } from "effect";
import type { TimeoutError } from "effect/Cause";
import type { SettingsResponse } from "../api/generated/models/index.js";
import { GeneratedApi } from "../api/generated-api.js";
import type { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import { waitUntilBackendReady } from "../utils/backendReadiness.js";

export { waitUntilBackendReady } from "../utils/backendReadiness.js";

export type StartupSnapshot = SettingsResponse;
export type StartupError = ApiProblemError | TransientTransportError | TimeoutError;

const STARTUP_CACHE_KEY = "settings";

const loadStartupSettings = Effect.fn("StartupWorkflow.loadSettings")(function* () {
  const api = yield* GeneratedApi;
  return yield* Effect.scoped(
    Effect.gen(function* () {
      const controller = yield* Effect.acquireRelease(
        Effect.sync(() => new AbortController()),
        (owned) => Effect.sync(() => owned.abort()),
      );
      return yield* api.execute("GET /settings", () =>
        api.client.SettingsService.getSettings({ signal: controller.signal }),
      );
    }),
  );
});

export class StartupWorkflow extends Context.Service<
  StartupWorkflow,
  {
    readonly start: Effect.Effect<StartupSnapshot, StartupError>;
    readonly invalidate: Effect.Effect<void>;
  }
>()("kenn-forge/StartupWorkflow") {}

export const StartupWorkflowLive = Layer.effect(StartupWorkflow)(
  Effect.gen(function* () {
    const generation = yield* Ref.make(0);
    const cache = yield* Cache.make({
      capacity: 1,
      lookup: () => waitUntilBackendReady.pipe(Effect.andThen(loadStartupSettings().pipe(Effect.timeout("8 seconds")))),
      // Startup and embedded-shell callers share the last settings snapshot.
      // Every settings write invalidates this entry through SettingsWorkflow.
      timeToLive: Duration.infinity,
    });
    const start: Effect.Effect<StartupSnapshot, StartupError> = Effect.suspend(() =>
      Effect.gen(function* () {
        const requestedGeneration = yield* Ref.get(generation);
        const snapshot = yield* Cache.get(cache, STARTUP_CACHE_KEY).pipe(
          Effect.tapError(() => Cache.invalidate(cache, STARTUP_CACHE_KEY)),
        );
        const currentGeneration = yield* Ref.get(generation);
        return requestedGeneration === currentGeneration ? snapshot : yield* start;
      }),
    );
    return {
      start,
      invalidate: Ref.update(generation, (current) => current + 1).pipe(
        Effect.andThen(Cache.invalidate(cache, STARTUP_CACHE_KEY)),
      ),
    };
  }),
);

export function startupErrorMessage(failure: StartupError): string {
  switch (failure._tag) {
    case "ApiProblemError":
      return failure.problem.detail ?? failure.problem.title ?? "Could not load settings";
    case "TimeoutError":
      return "Timed out loading settings";
    case "TransientTransportError":
      return "Could not reach Kenn Forge";
  }
}
