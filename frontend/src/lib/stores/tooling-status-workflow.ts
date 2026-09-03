import { Cache, Context, Duration, Effect, Layer } from "effect";
import type { ToolingStatusBody } from "../api/generated/models/index.js";
import { GeneratedApi } from "../api/generated-api.js";

export type ServerToolingStatus = ToolingStatusBody;

interface ToolingStatusWorkflowService {
  readonly load: Effect.Effect<ServerToolingStatus | undefined, never>;
}

export class ToolingStatusWorkflow extends Context.Service<ToolingStatusWorkflow, ToolingStatusWorkflowService>()(
  "kenn-forge/ToolingStatusWorkflow",
) {}

export const ToolingStatusWorkflowLive = Layer.effect(ToolingStatusWorkflow)(
  Effect.gen(function* () {
    const api = yield* GeneratedApi;
    const cache = yield* Cache.make<string, ServerToolingStatus | undefined>({
      capacity: 1,
      timeToLive: Duration.infinity,
      lookup: () =>
        api
          .execute("load tooling status", (signal) => api.client.SystemService.getToolingStatus({ signal }))
          .pipe(Effect.catch(() => Effect.succeed(undefined))),
    });

    return {
      load: Cache.get(cache, "standalone"),
    };
  }),
);
