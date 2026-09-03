import { afterEach, beforeEach, describe, expect, it, vi } from "@effect/vitest";
import { Effect, Fiber, Layer } from "effect";
import { TestClock } from "effect/testing";
import type { RoborevClient } from "../../api/roborev/client.js";
import { makeGeneratedApiLayer, type GeneratedClient } from "../../api/generated-api.js";
import type { OwnedAppRuntime } from "../../app/runtime.js";
import { makeTestAppRuntime } from "../../testing/effect-layers.js";
import { makeGeneratedClient } from "../../testing/generated-client.js";
import { RoborevDaemonWorkflowLive } from "./daemon-workflow.js";
import { createDaemonStore } from "./daemon.svelte.js";

let runtime: OwnedAppRuntime | undefined;

function daemonStore(forgeClient: GeneratedClient, client: RoborevClient) {
  runtime = makeTestAppRuntime(forgeClient);
  return createDaemonStore({ client, runtime });
}

function forgeClient(get: ReturnType<typeof vi.fn>): GeneratedClient {
  return makeGeneratedClient({
    RoborevService: {
      getRoborevStatus: async (options) => (await get("/roborev/status", options)).data,
    },
  });
}

function startPolling(store: ReturnType<typeof daemonStore>) {
  if (runtime === undefined) throw new Error("test runtime was not initialized");
  return runtime.runCommand(store.pollingEffect, {
    operation: "test Roborev daemon polling",
    safeContext: {},
    onFailure: () => {},
  });
}

beforeEach(() => {
  runtime = undefined;
});

afterEach(async () => {
  vi.useRealTimers();
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

describe("createDaemonStore", () => {
  it("shares an in-flight poll with a manual health check", async () => {
    let resolveHealth!: (response: {
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }) => void;
    const health = new Promise<{
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }>((resolve) => {
      resolveHealth = resolve;
    });
    const forgeGet = vi.fn().mockReturnValue(health);
    const roborevGet = vi.fn().mockResolvedValue({
      data: {
        active_workers: 0,
        applied_jobs: 0,
        canceled_jobs: 0,
        completed_jobs: 0,
        failed_jobs: 0,
        max_workers: 1,
        queue_paused: false,
        queued_jobs: 0,
        rebased_jobs: 0,
        running_jobs: 0,
        skipped_jobs: 0,
        version: "test",
      },
    });
    const store = daemonStore(forgeClient(forgeGet), { GET: roborevGet } as unknown as RoborevClient);

    const polling = startPolling(store);
    store.checkHealth();

    expect(forgeGet).toHaveBeenCalledTimes(1);

    resolveHealth({
      data: {
        available: true,
        endpoint: "http://roborev:7373",
        version: "test",
      },
    });
    await vi.waitFor(() => expect(store.isAvailable()).toBe(true));

    polling.interrupt();
  });

  it("reports availability before a stalled status read and aborts that read on stop", async () => {
    const forgeGet = vi.fn().mockResolvedValue({
      data: {
        available: true,
        endpoint: "http://roborev:7373",
        version: "test",
      },
    });
    let statusSignal: AbortSignal | undefined;
    const roborevGet = vi.fn((_path: string, options?: { signal?: AbortSignal }) => {
      statusSignal = options?.signal;
      return new Promise(() => {});
    });
    const store = daemonStore(forgeClient(forgeGet), { GET: roborevGet } as unknown as RoborevClient);

    const polling = startPolling(store);
    await vi.waitFor(() => {
      expect(store.isAvailable()).toBe(true);
      expect(store.getWasEverAvailable()).toBe(true);
      expect(statusSignal).toBeDefined();
    });

    polling.interrupt();
    await vi.waitFor(() => expect(statusSignal?.aborted).toBe(true));
  });

  it("ignores health responses from a stopped polling generation", async () => {
    let resolveOldHealth!: (response: {
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }) => void;
    const oldHealth = new Promise<{
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }>((resolve) => {
      resolveOldHealth = resolve;
    });
    let resolveCurrentHealth!: (response: {
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }) => void;
    const currentHealth = new Promise<{
      data: {
        available: boolean;
        endpoint: string;
        version: string;
      };
    }>((resolve) => {
      resolveCurrentHealth = resolve;
    });
    const unavailable = {
      data: {
        available: false,
        endpoint: "http://roborev:7373",
        version: "",
      },
    };
    const forgeGet = vi
      .fn()
      .mockReturnValueOnce(oldHealth)
      .mockResolvedValueOnce(unavailable)
      .mockReturnValueOnce(currentHealth);
    const roborevGet = vi.fn().mockResolvedValue({
      data: {
        active_workers: 1,
        applied_jobs: 2,
        canceled_jobs: 3,
        completed_jobs: 4,
        failed_jobs: 5,
        max_workers: 6,
        queue_paused: false,
        queued_jobs: 7,
        rebased_jobs: 8,
        running_jobs: 9,
        skipped_jobs: 10,
        version: "test",
      },
    });
    const store = daemonStore(forgeClient(forgeGet), { GET: roborevGet } as unknown as RoborevClient);

    const oldPolling = startPolling(store);
    expect(forgeGet).toHaveBeenCalledTimes(1);

    oldPolling.interrupt();
    const currentPolling = startPolling(store);
    await vi.waitFor(() => {
      expect(forgeGet).toHaveBeenCalledTimes(2);
      expect(store.isLoading()).toBe(false);
    });
    expect(store.isAvailable()).toBe(false);

    store.checkHealth();
    expect(store.isLoading()).toBe(true);

    resolveOldHealth({
      data: {
        available: true,
        endpoint: "http://roborev:7373",
        version: "stale",
      },
    });
    await oldHealth;
    await Promise.resolve();
    await Promise.resolve();

    expect(store.isAvailable()).toBe(false);
    expect(store.isLoading()).toBe(true);
    expect(store.getQueuedJobs()).toBe(0);
    expect(store.getWasEverAvailable()).toBe(false);
    expect(roborevGet).not.toHaveBeenCalled();

    resolveCurrentHealth(unavailable);
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    currentPolling.interrupt();
  });

  it.effect("polls quickly while unavailable and returns to the healthy cadence after recovery", () =>
    Effect.gen(function* () {
      const forgeGet = vi
        .fn()
        .mockResolvedValueOnce({
          data: {
            available: false,
            endpoint: "http://roborev:7373",
            version: "",
          },
        })
        .mockResolvedValue({
          data: {
            available: true,
            endpoint: "http://roborev:7373",
            version: "test",
          },
        });
      const roborevGet = vi.fn().mockResolvedValue({
        data: {
          active_workers: 0,
          applied_jobs: 0,
          canceled_jobs: 0,
          completed_jobs: 0,
          failed_jobs: 0,
          max_workers: 1,
          queue_paused: false,
          queued_jobs: 0,
          rebased_jobs: 0,
          running_jobs: 0,
          skipped_jobs: 0,
          version: "test",
        },
      });
      const generatedClient = forgeClient(forgeGet);
      const store = daemonStore(generatedClient, { GET: roborevGet } as unknown as RoborevClient);
      const daemonLayer = Layer.provideMerge(RoborevDaemonWorkflowLive, makeGeneratedApiLayer(generatedClient));
      const polling = yield* Effect.forkChild(store.pollingEffect.pipe(Effect.provide(daemonLayer)));

      yield* Effect.yieldNow;
      expect(forgeGet).toHaveBeenCalledTimes(1);
      yield* TestClock.adjust("999 millis");
      expect(forgeGet).toHaveBeenCalledTimes(1);
      yield* TestClock.adjust("1 millis");
      expect(forgeGet).toHaveBeenCalledTimes(2);
      expect(store.isAvailable()).toBe(true);
      expect(store.getWasEverAvailable()).toBe(true);
      expect(roborevGet).toHaveBeenCalledTimes(1);

      yield* TestClock.adjust("29999 millis");
      expect(forgeGet).toHaveBeenCalledTimes(2);
      yield* TestClock.adjust("1 millis");
      expect(forgeGet).toHaveBeenCalledTimes(3);
      yield* Fiber.interrupt(polling);
    }),
  );
});
