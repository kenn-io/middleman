import { Cache, Context, Duration, Effect, Layer } from "effect";
import type { ApiProblemError, TransientTransportError } from "../../api/effect-errors.js";
import { executeGeneratedApiRequest } from "../../api/generated-api.js";
import type { RoborevStatusResponse } from "../../api/generated/models/index.js";

export type RoborevHealth = RoborevStatusResponse;
export type RoborevHealthError = ApiProblemError | TransientTransportError;

export class RoborevDaemonWorkflow extends Context.Service<
  RoborevDaemonWorkflow,
  {
    readonly health: Effect.Effect<RoborevHealth, RoborevHealthError>;
  }
>()("kenn-forge/RoborevDaemonWorkflow") {}

export const RoborevDaemonWorkflowLive = Layer.effect(RoborevDaemonWorkflow)(
  Effect.gen(function* () {
    const cache = yield* Cache.make({
      capacity: 1,
      timeToLive: Duration.zero,
      lookup: () =>
        executeGeneratedApiRequest("GET /roborev/status", (client, signal) =>
          client.RoborevService.getRoborevStatus({ signal }),
        ),
    });

    return { health: Cache.get(cache, "health") };
  }),
);
