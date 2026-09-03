import { Context, Deferred, Effect, Layer, ManagedRuntime } from "effect";
import * as Socket from "effect/unstable/socket/Socket";
import { makeAppLiveLayer } from "../app/layer.js";
import { makeAppRuntimeBoundary, type OwnedAppRuntime } from "../app/runtime.js";
import { makeGeneratedApiLayer, type GeneratedClient } from "../api/generated-api.js";
import { makeGeneratedClient } from "./generated-client.js";
import { makeGeneratedClientFromRouteMocks, type RouteMockClient } from "./test/route-mock-client.js";
import { EventSourceFactory, type EventSourceLike } from "../browser/event-source.js";
import { BrowserObservers } from "../browser/observers.js";

export class EventSourceProbe extends Context.Service<
  EventSourceProbe,
  {
    readonly awaitOpened: Effect.Effect<void>;
    readonly closeCount: Effect.Effect<number>;
    readonly emitMessages: (count: number) => Effect.Effect<void>;
    readonly opened: Deferred.Deferred<void>;
    readonly counter: { value: number };
    readonly source: { current: TestEventSource | undefined };
  }
>()("kenn-forge/testing/EventSourceProbe") {}

class TestEventSource extends EventTarget implements EventSourceLike {
  readonly url: string;
  readonly withCredentials = false;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSED = 2;
  readyState = this.OPEN;
  onerror: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onopen: ((event: Event) => void) | null = null;
  readonly onClose: () => void;

  constructor(url: string, onClose: () => void) {
    super();
    this.url = url;
    this.onClose = onClose;
  }

  close(): void {
    this.readyState = this.CLOSED;
    this.onClose();
  }
}

const probeLayer = Layer.effect(EventSourceProbe)(
  Effect.gen(function* () {
    const opened = yield* Deferred.make<void>();
    const counter = { value: 0 };
    const source: { current: TestEventSource | undefined } = { current: undefined };
    return {
      awaitOpened: Deferred.await(opened),
      closeCount: Effect.sync(() => counter.value),
      emitMessages: (count: number) =>
        Effect.sync(() => {
          for (let index = 0; index < count; index++) {
            source.current?.dispatchEvent(new MessageEvent("message", { data: String(index) }));
          }
        }),
      opened,
      counter,
      source,
    };
  }),
);

const factoryLayer = Layer.effect(EventSourceFactory)(
  Effect.gen(function* () {
    const probe = yield* EventSourceProbe;
    return {
      open: (url: string) =>
        Effect.gen(function* () {
          yield* Deferred.succeed(probe.opened, undefined);
          const source = new TestEventSource(url, () => {
            probe.counter.value += 1;
          });
          probe.source.current = source;
          return source;
        }),
    };
  }),
);

export const EventSourceFactoryTest = Layer.provideMerge(factoryLayer, probeLayer);

export class ObserverProbe extends Context.Service<
  ObserverProbe,
  {
    readonly disconnectCount: Effect.Effect<number>;
    readonly counter: { value: number };
  }
>()("kenn-forge/testing/ObserverProbe") {}

const observerProbeLayer = Layer.effect(ObserverProbe)(
  Effect.sync(() => {
    const counter = { value: 0 };
    return {
      disconnectCount: Effect.sync(() => counter.value),
      counter,
    };
  }),
);
const observerFactoryLayer = Layer.effect(BrowserObservers)(
  Effect.gen(function* () {
    const probe = yield* ObserverProbe;
    return {
      resize: (): ResizeObserver => ({
        disconnect: () => {
          probe.counter.value += 1;
        },
        observe: () => undefined,
        unobserve: () => undefined,
      }),
      mutation: () => {
        throw new Error("MutationObserver is not used by this test layer");
      },
      intersection: () => {
        throw new Error("IntersectionObserver is not used by this test layer");
      },
    };
  }),
);

export const BrowserObserversTest = Layer.provideMerge(observerFactoryLayer, observerProbeLayer);

function isRouteMockClient(client: GeneratedClient | RouteMockClient): client is RouteMockClient {
  return "GET" in client || "POST" in client || "PUT" in client || "PATCH" in client || "DELETE" in client;
}

export class WebSocketProbe extends Context.Service<
  WebSocketProbe,
  {
    readonly awaitOpened: Effect.Effect<void>;
    readonly closeCount: Effect.Effect<number>;
    readonly opened: Deferred.Deferred<void>;
    readonly counter: { value: number };
  }
>()("kenn-forge/testing/WebSocketProbe") {}

type WebSocketTestMode = "success" | "failure" | "interruption";

class TestCloseEvent extends Event {
  readonly code = 1000;
  readonly reason = "";
}

class TestWebSocket extends EventTarget implements WebSocket {
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;
  binaryType: BinaryType = "blob";
  readonly bufferedAmount = 0;
  readonly extensions = "";
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onopen: ((event: Event) => void) | null = null;
  readonly protocol = "";
  readyState: WebSocket["readyState"] = this.CONNECTING;
  readonly url: string;
  readonly onOpened: () => void;
  readonly onClosed: () => void;
  readonly mode: WebSocketTestMode;

  constructor(url: string, mode: WebSocketTestMode, onOpened: () => void, onClosed: () => void) {
    super();
    this.url = url;
    this.mode = mode;
    this.onOpened = onOpened;
    this.onClosed = onClosed;
    queueMicrotask(() => {
      this.readyState = this.OPEN;
      this.dispatchEvent(new Event("open"));
      this.onOpened();
      if (this.mode === "success") {
        queueMicrotask(() => this.dispatchEvent(new TestCloseEvent("close")));
      } else if (this.mode === "failure") {
        queueMicrotask(() => this.dispatchEvent(new MessageEvent("message", { data: "payload" })));
      }
    });
  }

  close(): void {
    this.readyState = this.CLOSED;
    this.onClosed();
  }

  send(): void {}
}

function makeWebSocketTestLayer(mode: WebSocketTestMode) {
  const probeLayer = Layer.effect(WebSocketProbe)(
    Effect.gen(function* () {
      const opened = yield* Deferred.make<void>();
      const counter = { value: 0 };
      return {
        awaitOpened: Deferred.await(opened),
        closeCount: Effect.sync(() => counter.value),
        opened,
        counter,
      };
    }),
  );
  const constructorLayer = Layer.effect(Socket.WebSocketConstructor)(
    Effect.gen(function* () {
      const probe = yield* WebSocketProbe;
      return (url: string, _protocols?: string | Array<string>) =>
        new TestWebSocket(
          url,
          mode,
          () => {
            Deferred.doneUnsafe(probe.opened, Effect.void);
          },
          () => {
            probe.counter.value += 1;
          },
        );
    }),
  );
  return Layer.provideMerge(constructorLayer, probeLayer);
}

export const WebSocketSuccessTest = makeWebSocketTestLayer("success");
export const WebSocketFailureTest = makeWebSocketTestLayer("failure");
export const WebSocketInterruptionTest = makeWebSocketTestLayer("interruption");

export function makeTestAppRuntime(client: GeneratedClient | RouteMockClient = makeGeneratedClient()): OwnedAppRuntime {
  return makeAppRuntimeBoundary(
    ManagedRuntime.make(
      makeAppLiveLayer(
        makeGeneratedApiLayer(isRouteMockClient(client) ? makeGeneratedClientFromRouteMocks(client) : client),
      ),
    ),
  );
}
