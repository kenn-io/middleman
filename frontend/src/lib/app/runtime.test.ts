import { Effect, Exit } from "effect";
import { afterEach, describe, expect, it } from "vite-plus/test";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import type { OwnedAppRuntime } from "./runtime.js";

let runtime: OwnedAppRuntime | undefined;

describe("AppRuntime", () => {
  afterEach(async () => {
    if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
    runtime = undefined;
  });

  it("exposes an owned command result to Promise-only integrations", async () => {
    runtime = makeTestAppRuntime(makeGeneratedClient());
    const execution = runtime.runCommand(Effect.succeed("accepted"), {
      operation: "test Promise integration",
      safeContext: {},
      onFailure: () => {},
    });

    const exit = await execution.exit;

    expect(Exit.isSuccess(exit) && exit.value).toBe("accepted");
  });

  it("does not publish an interrupted microtask command", async () => {
    runtime = makeTestAppRuntime(makeGeneratedClient());
    let published = false;
    const execution = runtime.runMicrotask(
      () => {
        published = true;
      },
      { operation: "test deferred publication", safeContext: {} },
    );

    execution.interrupt();
    await execution.exit;

    expect(published).toBe(false);
  });
});
