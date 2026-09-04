import { Clipboard } from "@effect/platform-browser";
import { Layer } from "effect";
import { GeneratedApiLive } from "../api/generated-api.js";
import type { GeneratedApi } from "../api/generated-api.js";
import { EventSourceFactoryLive } from "../browser/event-source.js";
import { BrowserObserversLive } from "../browser/observers.js";
import { AnimationFramesLive } from "../browser/animation-frame.js";
import { MicrotasksLive } from "../browser/microtask.js";
import { LocalStorageLive, SessionStorageLive } from "../browser/storage.js";
import { StreamingFetchLive } from "../browser/streaming-fetch.js";
import { WebSocketLive } from "../browser/web-socket.js";
import { DiffContextPrefetchLive } from "../components/diff/diff-context-prefetch.js";
import { SettingsWorkflowLive } from "../stores/settings-workflow.js";
import { ActivityWorkflowLive } from "../stores/activity-workflow.js";
import { DetailWorkflowLive } from "../stores/detail-workflow.js";
import { DiffWorkflowLive } from "../stores/diff-workflow.js";
import { FilePreviewWorkflowLive } from "../stores/diff-preview-workflow.js";
import { DocsWorkflowLive } from "../stores/docs-workflow.js";
import { IssuesWorkflowLive } from "../stores/issues-workflow.js";
import { ProviderMutationsLive } from "../stores/ordered-mutations.js";
import { PullsWorkflowLive } from "../stores/pulls-workflow.js";
import { ProviderEventsCheckpointLive } from "../stores/provider-events-workflow.js";
import { SyncWorkflowLive } from "../stores/sync-workflow.js";
import { RoborevDaemonWorkflowLive } from "../stores/roborev/daemon-workflow.js";
import { RoborevWorkflowLive } from "../stores/roborev/roborev-workflow.js";
import { RepoBrowserWorkflowLive } from "../stores/repo-browser-workflow.js";
import { StartupWorkflowLive } from "./startup-workflow.js";
import { PierreDiffWorkerPoolLive } from "../components/diff/pierre-worker-pool.js";
import { WorkspaceListWorkflowLive } from "../components/terminal/workspace-list-workflow.js";
import { ProjectMutationWorkflowLive } from "../components/terminal/project-mutation-workflow.js";
import { WorkspaceRuntimeWorkflowLive } from "../components/terminal/workspace-runtime-workflow.js";
import { RepoSummaryWorkflowLive } from "../components/repositories/repo-summary-workflow.js";
import { ToolingStatusWorkflowLive } from "../stores/tooling-status-workflow.js";

export function makeAppLiveLayer(generatedApiLayer: Layer.Layer<GeneratedApi>) {
  const browserBoundaryLive = Layer.mergeAll(
    generatedApiLayer,
    AnimationFramesLive,
    EventSourceFactoryLive,
    BrowserObserversLive,
    LocalStorageLive,
    MicrotasksLive,
    SessionStorageLive,
    StreamingFetchLive,
    WebSocketLive,
    Clipboard.layer,
    PierreDiffWorkerPoolLive,
    WorkspaceListWorkflowLive,
  );

  const startupLive = Layer.provideMerge(StartupWorkflowLive, browserBoundaryLive);
  const providerWorkflowsLive = Layer.mergeAll(
    PullsWorkflowLive,
    IssuesWorkflowLive,
    ActivityWorkflowLive,
    SyncWorkflowLive,
    DetailWorkflowLive,
    DiffWorkflowLive,
    DiffContextPrefetchLive,
    FilePreviewWorkflowLive,
    DocsWorkflowLive,
    ProviderEventsCheckpointLive,
    ProviderMutationsLive,
    RoborevDaemonWorkflowLive,
    RoborevWorkflowLive,
    RepoBrowserWorkflowLive,
    ProjectMutationWorkflowLive,
    WorkspaceRuntimeWorkflowLive,
    RepoSummaryWorkflowLive,
    ToolingStatusWorkflowLive,
  );
  const applicationWorkflowsLive = Layer.mergeAll(SettingsWorkflowLive, providerWorkflowsLive);

  return Layer.provideMerge(applicationWorkflowsLive, startupLive);
}

export const AppLiveLayer = makeAppLiveLayer(GeneratedApiLive);
