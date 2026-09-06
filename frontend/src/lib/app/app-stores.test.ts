import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { makeAppRuntime, type OwnedAppRuntime } from "./runtime.js";
import { createAppStores } from "../app-stores.svelte.js";

let runtime: OwnedAppRuntime;
let eventSources: EventSourceStub[];

class EventSourceStub {
  readonly handlers = new Map<string, Set<(event: Event) => void>>();

  constructor(readonly url: string) {
    eventSources.push(this);
  }

  addEventListener(name: string, handler: (event: Event) => void): void {
    const handlers = this.handlers.get(name) ?? new Set();
    handlers.add(handler);
    this.handlers.set(name, handlers);
  }

  removeEventListener(name: string, handler: (event: Event) => void): void {
    this.handlers.get(name)?.delete(handler);
  }

  close(): void {}
}

function emit(source: EventSourceStub, name: string, payload: unknown): void {
  const event = new MessageEvent(name, { data: JSON.stringify(payload) });
  for (const handler of source.handlers.get(name) ?? []) handler(event);
}

beforeEach(() => {
  eventSources = [];
  vi.stubGlobal("EventSource", EventSourceStub);
  runtime = makeAppRuntime();
});

afterEach(async () => {
  await Effect.runPromise(runtime.disposeEffect);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("app store composition", () => {
  it("builds the provider store graph directly from the application runtime", () => {
    const composition = createAppStores({
      runtime,
      getPage: () => "pulls",
      hostState: {
        getGlobalRepo: () => "github/github.com/acme/widgets",
        getGroupByRepo: () => false,
      },
    });

    expect(composition.stores.pulls.getPulls()).toEqual([]);
    expect(composition.stores.issues.getIssues()).toEqual([]);
    expect(composition.stores.activity.getActivityItems()).toEqual([]);
    expect(composition.stores.grouping.getGroupByRepo()).toBe(true);
    expect(
      composition.stores.workflowActions.getRuns({
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
      }),
    ).toEqual([]);
  });

  it("keeps provider data unavailable when reconnect reconciliation fails", async () => {
    const failures: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            type: "about:blank",
            title: "Not Found",
            status: 404,
            detail: "provider snapshot unavailable",
            code: "notFound",
          }),
          { status: 404, headers: { "content-type": "application/problem+json" } },
        ),
      ),
    );
    const composition = createAppStores({ runtime, onError: (message) => failures.push(message) });
    runtime.runCommand(composition.stores.events.streamEffect, {
      operation: "test provider events",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(eventSources).toHaveLength(1));

    emit(eventSources[0]!, "hub_connection_changed", { connected: true });

    await vi.waitFor(() => expect(failures).toHaveLength(1));
    expect(composition.stores.sync.getProviderAvailable()).toBe(false);
  });

  it("restores provider availability when the hub probe succeeds after a projection failure", async () => {
    const failures: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL) => {
        const url = input instanceof Request ? input.url : String(input);
        if (url.endsWith("/api/v1/sync/status")) {
          return Promise.resolve(
            new Response(JSON.stringify({ running: false, last_run_at: "", last_error: "" }), {
              status: 200,
              headers: { "content-type": "application/json" },
            }),
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              type: "about:blank",
              title: "Not Found",
              status: 404,
              detail: "provider projection unavailable",
              code: "notFound",
            }),
            { status: 404, headers: { "content-type": "application/problem+json" } },
          ),
        );
      }),
    );
    const composition = createAppStores({ runtime, onError: (message) => failures.push(message) });
    runtime.runCommand(composition.stores.events.streamEffect, {
      operation: "test provider events",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(eventSources).toHaveLength(1));

    emit(eventSources[0]!, "hub_connection_changed", { connected: true });

    await vi.waitFor(() => expect(failures).toHaveLength(1));
    expect(composition.stores.sync.getProviderAvailable()).toBe(true);
  });

  it("marks provider data unavailable when a live refresh cannot reach the hub", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            type: "about:blank",
            title: "Hub unavailable",
            status: 503,
            detail: "provider data is unavailable because the federation hub cannot be reached",
            code: "hubUnavailable",
          }),
          { status: 503, headers: { "content-type": "application/problem+json" } },
        ),
      ),
    );
    const composition = createAppStores({ runtime, getPage: () => "pulls" });
    runtime.runCommand(composition.stores.events.streamEffect, {
      operation: "test provider events",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(eventSources).toHaveLength(1));

    emit(eventSources[0]!, "data_changed", {});

    await vi.waitFor(() => expect(composition.stores.sync.getProviderAvailable()).toBe(false));
  });
});
