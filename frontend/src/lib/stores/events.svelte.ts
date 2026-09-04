import { Deferred, Effect } from "effect";
import type { AppRuntime, AppServices } from "../app/runtime.js";
import type { SyncStatus } from "../api/types.js";
import {
  providerEventsProgram,
  type ConfigChangedEvent,
  type HubConnectionChangedEvent,
  type DeferredMergeCompletedEvent,
  type PRCIRefreshedEvent,
  type PRCIRefreshQueuedEvent,
  type PRDetailRefreshedEvent,
  type ProviderEvent,
  type ProviderEventsConnectionState,
  type ProviderEventsError,
  type ReconnectStaleEvent,
  type WorkspacePRAssociatedEvent,
  type WorkspacePRRefreshQueuedEvent,
  type WorkspacePushedHeadChangedEvent,
  type WorkspaceCreatedEvent,
  type WorkspaceDeletedEvent,
  type WorkspaceStatusEvent,
  type WorkflowDispatchProgressEvent,
} from "./provider-events-workflow.js";

export interface EventsStoreOptions {
  readonly runtime: AppRuntime;
  readonly getBasePath?: () => string;
  readonly onDataChanged?: () => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onSyncStatus?: (status: SyncStatus) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onHubConnectionChanged?: (
    event: HubConnectionChangedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onConfigChanged?: (event: ConfigChangedEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onReconnectStale?: (event: ReconnectStaleEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspaceCreated?: (event: WorkspaceCreatedEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspaceStatus?: (event: WorkspaceStatusEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspaceDeleted?: (event: WorkspaceDeletedEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspacePushedHeadChanged?: (
    event: WorkspacePushedHeadChangedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspacePRAssociated?: (
    event: WorkspacePRAssociatedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkspacePRRefreshQueued?: (
    event: WorkspacePRRefreshQueuedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onPRDetailRefreshed?: (
    event: PRDetailRefreshedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onPRCIRefreshQueued?: (
    event: PRCIRefreshQueuedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onPRCIRefreshed?: (event: PRCIRefreshedEvent) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onDeferredMergeCompleted?: (
    event: DeferredMergeCompletedEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onWorkflowDispatchProgress?: (
    event: WorkflowDispatchProgressEvent,
  ) => Effect.Effect<void, ProviderEventsError, AppServices>;
  readonly onTerminalFailure?: (message: string) => void;
  readonly onRecoverableFailure?: (message: string) => void;
}

export type WorkspaceEventsNotification = { readonly type: "open" } | ProviderEvent;

export function createEventsStore(opts: EventsStoreOptions) {
  const getBasePath = opts.getBasePath ?? (() => "/");
  let connectionState = $state<ProviderEventsConnectionState>("disconnected");
  let lastError = $state<string | null>(null);
  let reconnectSignal: Deferred.Deferred<void> | null = null;
  let streamRestartSignal: Deferred.Deferred<void> | null = null;
  let workspaceSelectionSequence = 0;
  const workspaceSelections = new Map<number, string>();
  const workspaceEventSubscribers = new Set<(event: WorkspaceEventsNotification) => void>();

  function notifyWorkspaceEventSubscribers(event: WorkspaceEventsNotification): void {
    for (const subscriber of workspaceEventSubscribers) subscriber(event);
  }

  function selectedWorkspaceId(): string | undefined {
    return Array.from(workspaceSelections.values()).at(-1);
  }

  function buildURL(): string {
    const base = getBasePath().replace(/\/$/, "");
    const url = `${base}/api/v1/events`;
    const workspaceId = selectedWorkspaceId();
    return workspaceId === undefined ? url : `${url}?workspace_id=${encodeURIComponent(workspaceId)}`;
  }

  function dispatch(event: ProviderEvent): Effect.Effect<void, ProviderEventsError, AppServices> {
    const consequence = (() => {
      switch (event.type) {
        case "data_changed":
          return opts.onDataChanged?.() ?? Effect.void;
        case "sync_status":
          return opts.onSyncStatus?.(event.payload) ?? Effect.void;
        case "hub_connection_changed":
          return opts.onHubConnectionChanged?.(event.payload) ?? Effect.void;
        case "config.changed":
          return opts.onConfigChanged?.(event.payload) ?? Effect.void;
        case "reconnect.stale":
          return opts.onReconnectStale?.(event.payload) ?? Effect.void;
        case "workspace_created":
          return opts.onWorkspaceCreated?.(event.payload) ?? Effect.void;
        case "workspace_status":
          return opts.onWorkspaceStatus?.(event.payload) ?? Effect.void;
        case "workspace_deleted":
          return opts.onWorkspaceDeleted?.(event.payload) ?? Effect.void;
        case "workspace_pushed_head_changed":
          return opts.onWorkspacePushedHeadChanged?.(event.payload) ?? Effect.void;
        case "workspace_pr_associated":
          return opts.onWorkspacePRAssociated?.(event.payload) ?? Effect.void;
        case "workspace_pr_refresh_queued":
          return opts.onWorkspacePRRefreshQueued?.(event.payload) ?? Effect.void;
        case "pr_detail_refreshed":
          return opts.onPRDetailRefreshed?.(event.payload) ?? Effect.void;
        case "pr_ci_refresh_queued":
          return opts.onPRCIRefreshQueued?.(event.payload) ?? Effect.void;
        case "pr_ci_refreshed":
          return opts.onPRCIRefreshed?.(event.payload) ?? Effect.void;
        case "deferred_merge_completed":
          return opts.onDeferredMergeCompleted?.(event.payload) ?? Effect.void;
        case "workflow_dispatch_progress":
          return opts.onWorkflowDispatchProgress?.(event.payload) ?? Effect.void;
        case "workspace_diff_ready":
        case "workspace_diff_changed":
          return Effect.void;
      }
    })();
    return Effect.sync(() => notifyWorkspaceEventSubscribers(event)).pipe(Effect.andThen(consequence));
  }

  const streamAttempt = Effect.gen(function* () {
    lastError = null;
    const restart = yield* Deferred.make<void>();
    yield* Effect.sync(() => {
      streamRestartSignal = restart;
    });
    const url = buildURL();
    yield* Effect.raceFirst(
      providerEventsProgram({
        url,
        onState: (state) => {
          connectionState = state;
          if (state === "connected") notifyWorkspaceEventSubscribers({ type: "open" });
        },
        onEvent: dispatch,
        onConsequenceFailure: (failure) => {
          const message = `A live update could not refresh ${failure.operation}; later updates will continue.`;
          lastError = message;
          opts.onRecoverableFailure?.(message);
        },
      }),
      Deferred.await(restart),
    ).pipe(
      Effect.ensuring(
        Effect.sync(() => {
          if (streamRestartSignal === restart) streamRestartSignal = null;
        }),
      ),
    );
  });

  const waitForReconnect = Effect.gen(function* () {
    const signal = yield* Deferred.make<void>();
    yield* Effect.sync(() => {
      reconnectSignal = signal;
    });
    yield* Deferred.await(signal);
    yield* Effect.sync(() => {
      if (reconnectSignal === signal) reconnectSignal = null;
    });
  });

  const streamEffect = Effect.forever(
    streamAttempt.pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          connectionState = "disconnected";
          lastError = `Live updates stopped: ${failure.operation}`;
          opts.onTerminalFailure?.(lastError);
        }).pipe(Effect.andThen(waitForReconnect)),
      ),
    ),
  ).pipe(
    Effect.ensuring(
      Effect.sync(() => {
        reconnectSignal = null;
        connectionState = "disconnected";
      }),
    ),
  );

  function reconnect(): void {
    const signal = reconnectSignal;
    if (signal === null) return;
    opts.runtime.runCommand(Deferred.succeed(signal, undefined), {
      operation: "reconnect provider events",
      safeContext: {},
      onFailure: () => {},
    });
  }

  function restartForWorkspaceSelection(): void {
    const signal = streamRestartSignal;
    if (signal === null) return;
    opts.runtime.runCommand(Deferred.succeed(signal, undefined), {
      operation: "reconnect provider events for workspace selection",
      safeContext: {},
      onFailure: () => {},
    });
  }

  function selectWorkspace(workspaceId: string): () => void {
    const previous = selectedWorkspaceId();
    workspaceSelectionSequence += 1;
    const selection = workspaceSelectionSequence;
    workspaceSelections.set(selection, workspaceId);
    if (selectedWorkspaceId() !== previous) restartForWorkspaceSelection();
    return () => {
      const beforeRelease = selectedWorkspaceId();
      workspaceSelections.delete(selection);
      if (selectedWorkspaceId() !== beforeRelease) restartForWorkspaceSelection();
    };
  }

  function subscribeWorkspaceEvents(subscriber: (event: WorkspaceEventsNotification) => void): () => void {
    workspaceEventSubscribers.add(subscriber);
    if (connectionState === "connected") subscriber({ type: "open" });
    return () => workspaceEventSubscribers.delete(subscriber);
  }

  function getConnectionState(): ProviderEventsConnectionState {
    return connectionState;
  }

  function getLastError(): string | null {
    return lastError;
  }

  return {
    streamEffect,
    reconnect,
    getConnectionState,
    getLastError,
    selectWorkspace,
    subscribeWorkspaceEvents,
  };
}

export type EventsStore = ReturnType<typeof createEventsStore>;
