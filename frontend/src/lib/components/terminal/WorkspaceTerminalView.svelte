<script lang="ts">
  import { EmptyState, IconButton, Spinner } from "@kenn-io/kit-ui";
  import { Context, Deferred, Duration, Effect, Fiber, Option, Schedule, Stream } from "effect";
  import PlayIcon from "@lucide/svelte/icons/play";
  import { onDestroy, tick, untrack } from "svelte";
  import { navigate } from "../../stores/router.svelte.ts";
  import { isNarrow } from "../../stores/container.svelte.js";
  import WorkspaceListSidebar from "./WorkspaceListSidebar.svelte";
  import SessionTerminalSlot from "./SessionTerminalSlot.svelte";
  import Modal from "../shared/Modal.svelte";
  import ConfirmDialog from "../shared/ConfirmDialog.svelte";
  import DialogButton from "../shared/DialogButton.svelte";
  import WorkspaceHome from "./WorkspaceHome.svelte";
  import WorkspaceLauncherOverlay from "./WorkspaceLauncherOverlay.svelte";
  import LaunchMenu from "./LaunchMenu.svelte";
  import TerminalOptionsMenu from "./TerminalOptionsMenu.svelte";
  import TerminalZoomControl from "./TerminalZoomControl.svelte";
  import DockedTerminalPanel from "./DockedTerminalPanel.svelte";
  import WorkspacePaneControls from "./WorkspacePaneControls.svelte";
  import WorkflowSplitTree, {
    type WorkflowTabDescriptor,
  } from "./WorkflowSplitTree.svelte";
  import WorkflowPresetMenu from "./WorkflowPresetMenu.svelte";
  import PackagePlusIcon from "@lucide/svelte/icons/package-plus";
  import type {
    LaunchTarget,
    RuntimeSession,
  } from "../../api/types.js";
  import {
    workspaceSessionWebSocketPath,
    type WorkspaceRuntimeState,
  } from "../../api/workspace-runtime.js";
  import {
    mountedSessions,
    noteSessionDiscarded,
    noteSessionMounted,
    noteSessionReleased,
    isSessionSlotVisible,
    onSessionExited,
    requestSessionFocus,
    sessionHostKey,
    sessionHostPrefix,
    type MountedSession,
    type SessionHostKey,
  } from "../../stores/session-host.svelte.ts";
  import {
    publishHostedSessions,
    registerWorkspaceControls,
    registerWorkspaceLauncher,
    setWorkspaceControlsBusy,
  } from "../../stores/workspace-host.svelte.ts";
  import {
    beginWorkspaceSwitch,
    cancelWorkspaceSwitch,
    recordWorkspaceSwitchPhase,
  } from "../../instrumentation/workspaceSwitchTiming.js";
  import {
    activateWorkflowTab,
    addTerminalGroup,
    appendWorkflowTabToLeaf,
    activeTerminalGroup,
    clampTerminalHeight,
    closeSessionInTerminalGroups,
    collectSessionKeys,
    createTerminalGroup,
    defaultTerminalLayout,
    findLeafBySession,
    findWorkflowLeafByTab,
    firstLeaf,
    moveWorkflowTabBefore,
    normalizeTerminalLayout,
    normalizeWorkflowTree,
    parseTerminalLayout,
    pruneTree,
    pruneWorkflowTreeToAvailable,
    splitPane,
    splitSessionIntoPane,
    splitWorkflowTabIntoLeaf,
    terminalGroupForSession,
    updateSplitRatio,
    updateTerminalGroupTree,
    updateWorkflowSplitRatio,
    type PaneNode,
    type SessionRegion,
    type SplitDirection,
    type TerminalGroup,
    type TerminalDock,
    type TerminalLayoutState,
    type WorkflowNode,
    type WorkflowTabKey,
  } from "./terminal-layout";
  import {
    mapWorkflowNodeSessionKeys,
    type WorkflowPreset,
    type WorkflowPresetSession,
  } from "./workflow-presets";
  import {
    clearActiveTerminalDrag,
    readRuntimeSessionDrag,
  } from "./terminal-drag";
  import { watchFleetWorkspaceDiff } from "./fleet-diff-watch.js";
  import { workspaceEventStream } from "./workspace-event-stream.js";
  import { decodeWorkspaceDetail, type WorkspaceDetail } from "./workspace-detail.js";
  import { reconnectSchedule } from "../../api/retry-policy.js";
  import { Button, CollapsibleSidebar, SplitResizeHandle, type SplitResizeEvent } from "@kenn-io/kit-ui";
  import { clearActiveTabbedPanelDrag, readTabbedPanelTabDrag } from "../shared/tabbed-panel-drag.js";
  import { getPaneLayoutStore, promoteSessionBesideWorkspace, type PaneSurfaceKey } from "../../stores/paneLayout.svelte.js";
  import { getStores } from "../../context.js";
  import { parseSessionPaneKey, sessionPaneKey, sessionPaneKeyMatchesWorkspace } from "../../stores/session-pane-key.js";
  import WorkspaceRightSidebar from "../workspace/WorkspaceRightSidebar.svelte";
  import type { InlineDockMode, WorkspaceItemIdentity } from "../../workspace-inline.js";
  import { defaultWorkspaceSidebarTab, type WorkspaceSidebarTab } from "./workspace-sidebar-default.js";
  import { getStackDepth } from "../../stores/keyboard/modal-stack.svelte.js";
  import ChevronsDownIcon from "@lucide/svelte/icons/chevrons-down";
  import ChevronsUpIcon from "@lucide/svelte/icons/chevrons-up";
  import PanelBottomCloseIcon from "@lucide/svelte/icons/panel-bottom-close";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import {
    AlertIcon,
    RefreshIcon,
  } from "../../icons.ts";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { ProblemCodes } from "../../api/problems.js";
  import {
    executeGeneratedApiRequest,
    executeOpaqueGeneratedApiRequest,
    GeneratedApi,
  } from "../../api/generated-api.js";
  import {
    TransientTransportError,
    type ApiProblemError,
    type InvalidExternalPayload,
  } from "../../api/effect-errors.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppServices } from "../../app/runtime.js";
  import { settingsErrorMessage } from "../../stores/settings-workflow.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import {
    claimWorkspaceLaunch,
    acceptWorkspaceLaunch,
    completeAcceptedWorkspaceLaunch,
    discardWorkspaceLaunch,
    failWorkspaceLaunch,
    isWorkspaceDeletionPending,
    pendingWorkspaceCreateLaunch,
    pendingWorkspaceLaunch,
    type WorkspaceLaunchClaim,
  } from "../../stores/workspace-create-pending.svelte.js";
  import { createTerminalZoomController } from "./terminalZoom";
  import {
    makeWorkspaceRuntimeOwner,
    makeWorkspaceRuntimePresenterID,
    WorkspaceRuntimeMutationOutcomeUnknown,
    WorkspaceRuntimeWorkflow,
    type WorkspaceRuntimeLaunchPlacement,
    type WorkspaceRuntimeMutationState,
    type WorkspaceRuntimeTarget,
  } from "./workspace-runtime-workflow.js";

  type Workspace = WorkspaceDetail;

  interface ClosedRuntimeSession {
    workspaceId: string;
    key: string;
    createdAt: string;
  }

  // hideWorkspaceList / hideRightSidebar let an embedding host
  // render only the terminal/home/empty surface and compose the
  // workspace list and per-item detail sidebar separately via
  // the /workspaces/embed/list and /workspaces/embed/detail
  // routes. Both default to false to preserve the standalone
  // /workspaces and /terminal/{id} layout.
  interface Props {
    workspaceId: string;
    workspaceHostKey?: string | undefined;
    selectedRepos?: string | undefined;
    isSidebarCollapsed?: boolean;
    sidebarWidth?: number | undefined;
    onSidebarResize?: ((width: number) => void) | undefined;
    isSidebarToggleEnabled?: boolean;
    onToggleSidebar?: (() => void) | undefined;
    hideWorkspaceList?: boolean;
    hideRightSidebar?: boolean;
    // False while this instance is parked in a hidden host (Plan 2b):
    // every dialog unmounts (its state persists for reopen) and every
    // TerminalPane deactivates. Defaults to true so standalone/embedded
    // usage is unaffected.
    hostVisible?: boolean;
    // Reports a successful delete (normal or forced) of the given
    // workspace ID, regardless of whether this instance was on a
    // terminal route at the time. Lets a hosting shell (e.g. an inline
    // claimant) react even when the delete doesn't trigger navigation.
    onWorkspaceDeleted?: (workspaceId: string, hostKey?: string, identity?: WorkspaceItemIdentity) => void;
    // Set only when this instance is embedded in an inline dock slot
    // (activity/prs/issues), not the Workspaces tab or standalone route.
    // Backs the toolbar's expand/show-details/collapse controls, which
    // replace the inline dock's own removed header bar.
    inlineDock?: { getMode(): InlineDockMode; setMode(mode: InlineDockMode): void } | null;
    // The detail surface this instance is embedded in, if any. Its pane layout is
    // the only record of which of this workspace's sessions have been promoted
    // out of here into a top-level pane, so it is also what tells this view which
    // sessions NOT to render. Unset on the Workspaces tab and the embed routes,
    // which have no detail panes: a session promoted in one surface is still at
    // home in every other place the workspace is shown.
    paneSurface?: PaneSurfaceKey | undefined;
    terminalSettingsReady?: boolean;
  }

  const {
    workspaceId,
    workspaceHostKey = undefined,
    selectedRepos = undefined,
    isSidebarCollapsed = false,
    sidebarWidth: externalWorkspaceListWidth = undefined,
    onSidebarResize = undefined,
    isSidebarToggleEnabled = false,
    onToggleSidebar = undefined,
    hideWorkspaceList = false,
    hideRightSidebar = false,
    hostVisible = true,
    onWorkspaceDeleted = undefined,
    inlineDock = null,
    paneSurface = undefined,
    terminalSettingsReady = true,
  }: Props = $props();

  const basePath = (
    window.__BASE_PATH__ ?? "/"
  ).replace(/\/$/, "");
  const { events: eventsStore, settings: settingsStore } = getStores();
  const appRuntime = getAppRuntime();
  const runtimeOwner = makeWorkspaceRuntimeOwner("workspace-view");
  const runtimePresenterID = makeWorkspaceRuntimePresenterID();

  function runtimeTarget(workspaceId: string, hostKey: string | undefined): WorkspaceRuntimeTarget {
    return { workspaceId, ...(hostKey === undefined ? {} : { hostKey }) };
  }

  function launchRuntimeSessionProgram(
    workspaceId: string,
    hostKey: string | undefined,
    targetKey: string,
    region: "workflow" | "terminal",
    placement: WorkspaceRuntimeLaunchPlacement,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      return yield* workflow.launch(
        runtimeTarget(workspaceId, hostKey),
        targetKey,
        region,
        placement,
      );
    });
  }

  function stopRuntimeSessionProgram(
    workspaceId: string,
    hostKey: string | undefined,
    sessionKey: string,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      yield* workflow.stop(
        runtimeTarget(workspaceId, hostKey),
        sessionKey,
      );
    });
  }

  function refreshWorkspaceMutationProgram(
    workspaceId: string,
    hostKey: string | undefined,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      yield* workflow.refresh(runtimeTarget(workspaceId, hostKey));
    });
  }

  function retryWorkspaceSetupMutationProgram(
    workspaceId: string,
    hostKey: string | undefined,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      yield* workflow.retrySetup(runtimeTarget(workspaceId, hostKey));
    });
  }

  function deleteWorkspaceMutationProgram(
    workspaceId: string,
    hostKey: string | undefined,
    force: boolean,
    identity: WorkspaceItemIdentity | undefined,
    presenterID: string,
    failurePresentationIsCurrent: () => boolean,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      const context = yield* Effect.context<AppServices>();
      const target = runtimeTarget(workspaceId, hostKey);
      yield* workflow.claimPresenter(
        target,
        presenterID,
        (state) =>
          presentRuntimeMutation(state, presenterID, failurePresentationIsCurrent).pipe(Effect.provide(context)),
        {
          releaseWhenAcknowledged: true,
          presentationIsCurrent: failurePresentationIsCurrent,
        },
      );
      yield* workflow.delete(target, {
        force,
        presenterID,
        ...(identity === undefined ? {} : { identity }),
      });
    });
  }

  function renameRuntimeSessionProgram(
    workspaceId: string,
    hostKey: string | undefined,
    sessionKey: string,
    label: string,
  ) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      return yield* workflow.rename(
        runtimeTarget(workspaceId, hostKey),
        sessionKey,
        label,
      );
    });
  }
  // Launcher, controls, and pending-write state is keyed by workspace identity
  // rather than held as bare flags: one embedded view serves every selection on its
  // surface, so a switch would otherwise inherit the previous workspace's open
  // overlay and its in-flight writes.
  const viewWorkspaceKey = $derived(`${workspaceId}\u0000${workspaceHostKey ?? ""}`);
  // Terminal font size and terminal options are APP settings, not workspace state:
  // one write, one single-flight controller, and it is in flight for every workspace
  // at once. So these two are plain booleans that follow the controller. Keying them
  // by workspace would report the next workspace's control enabled while the shared
  // controller is still refusing input.
  let terminalZoomSaving = $state(false);
  let terminalOptionsSaving = $state(false);
  const terminalZoom = createTerminalZoomController({
    runtime: appRuntime,
    store: settingsStore,
    reportError: (error) => {
      const detail = settingsErrorMessage(error);
      showFlash(`Couldn't save terminal font size: ${detail}`, {
        tone: "danger",
      });
    },
    onPendingChange: (pending) => {
      terminalZoomSaving = pending;
    },
  });
  const terminalFontSize = $derived(
    settingsStore.getTerminalFontSize(),
  );

  let workspace = $state<Workspace | null>(null);
  let runtime = $state.raw<WorkspaceRuntimeState | null>(null);
  let appliedRuntimeState:
    | {
        workspaceId: string;
        hostKey: string | undefined;
        fingerprint: string;
      }
    | null = null;
  // The workspace ID that `runtime` was fetched for. Stored
  // alongside the payload so we never render or operate on
  // sessions/targets that belong to a previous workspace
  // (during the in-place transition between workspaces, runtime
  // briefly outlives the workspace it was fetched for).
  let runtimeForId = $state<string>("");
  let runtimeForHostKey = $state<string | undefined>(undefined);
  let runtimeSnapshotAuthoritative = $state(false);
  let loadError = $state<string | null>(null);
  let retryingSetup = $state(false);
  let refreshingWorkspace = $state(false);
  type DeletingWorkspaceTarget = {
    id: string;
    hostKey: string | undefined;
  };
  type RetainedRuntimePresenterLease = {
    target: WorkspaceRuntimeTarget;
    presenterID: string;
  };
  let deletingWorkspaceTargets = $state<DeletingWorkspaceTarget[]>([]);
  let retainedRuntimePresenterLeases: RetainedRuntimePresenterLease[] = [];
  let workspacePresentationGeneration = 0;
  const deleteTriggerElements = new Map<string, HTMLElement | null>();
  let emptyLaunchTargetsExecution: { interrupt: () => void } | null = null;
  let sidebarRefreshToken = $state(0);
  let diffRefreshToken = $state(0);
  let lastDiffSnapshotVersion = "";
  let forcePromptMessage = $state<string | null>(null);
  let forcePromptForId = $state<string | null>(null);
  let forcePromptHostKey = $state<string | undefined>(undefined);
  // Identity snapshot captured when the 409 arrived, while the loaded
  // envelope still described the delete target: the user can switch
  // workspaces before confirming the force delete, after which the live
  // envelope belongs to someone else.
  let forcePromptIdentity: WorkspaceItemIdentity | undefined;
  let forceDeleting = $state(false);
  let stopPromptSession = $state<RuntimeSession | null>(null);
  let deletePromptOpen = $state(false);
  let stopSessionStopping = $state(false);
  let renamePrompt = $state<{
    sessionKey: string;
    originalLabel: string;
  } | null>(null);
  let renameInputValue = $state("");
  let renameSaving = $state(false);
  let renameInputEl = $state<HTMLInputElement | null>(null);
  onDestroy(() => {
    terminalZoom.dispose();
    emptyLaunchTargetsExecution?.interrupt();
    emptyLaunchTargetsExecution = null;
    const leases = retainedRuntimePresenterLeases;
    retainedRuntimePresenterLeases = [];
    if (leases.length === 0) return;
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* Effect.forEach(
          leases,
          (lease) => workflow.releasePresenter(lease.target, lease.presenterID),
          { discard: true },
        );
      }),
      {
        operation: "workspace.runtime.presenter.release",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
  });
  let runtimeError = $state<string | null>(null);
  let workspacePolling:
    | { key: string; interrupt: () => void }
    | null = null;
  let runtimePolling:
    | { key: string; interrupt: () => void }
    | null = null;
  let activeTabKey = $state<WorkflowTabKey>("home");
  let mountedSessionKeys = $state<string[]>([]);
  let closedSessions = $state<ClosedRuntimeSession[]>([]);
  let launchingKey = $state<string | null>(null);
  let terminalLayout = $state<TerminalLayoutState>(
    defaultTerminalLayout(),
  );
  let terminalLayoutWorkspaceId = $state("");
  let terminalLaunching = $state(false);
  let workspaceListState = $state<{
    status: "loading" | "retrying" | "loaded";
    total: number;
  }>({ status: "loading", total: 0 });

  const SIDEBAR_TAB_KEY_PREFIX = "kenn-forge-workspace-sidebar-tab:";
  const SIDEBAR_OPEN_KEY = "kenn-forge-workspace-sidebar-open";
  const SIDEBAR_WIDTH_KEY = "kenn-forge-workspace-sidebar-width";
  const WORKSPACE_LIST_WIDTH_KEY =
    "kenn-forge-workspace-list-sidebar-width";
  const ACTIVE_WORKSPACE_TAB_KEY_PREFIX =
    "kenn-forge-workspace-active-tab:";
  const TERMINAL_LAYOUT_KEY_PREFIX =
    "kenn-forge-workspace-terminal-layout:";
  const WORKFLOW_PRESETS_KEY = "kenn-forge-workspace-layout-presets";
  const PLAIN_SHELL_TARGET = "plain_shell";
  type EmptyLaunchTargetsState = "idle" | "loading" | "loaded" | "error";
  let emptyLaunchTargets = $state.raw<LaunchTarget[]>([]);
  let emptyLaunchTargetsState = $state<EmptyLaunchTargetsState>("idle");

  let workflowPresets = $state<WorkflowPreset[]>(loadWorkflowPresets());
  let selectedWorkflowPresetId = $state<string | null>(null);
  // A preset apply IS workspace work - it launches that workspace's sessions - so it
  // is tracked per workspace, and as a set rather than one owner: two workspaces can
  // have an apply in flight at once, and whichever finishes first must not re-enable
  // the other's control while its sessions are still being launched.
  let applyingWorkflowPresetFor = $state<string[]>([]);
  const applyingWorkflowPreset = $derived(applyingWorkflowPresetFor.includes(viewWorkspaceKey));

  type SidebarTab = WorkspaceSidebarTab;

  const MIN_WORKSPACE_LIST_WIDTH = 220;
  const DEFAULT_WORKSPACE_LIST_WIDTH = 260;
  const MAX_WORKSPACE_LIST_WIDTH = 420;

  function clampWorkspaceListWidth(
    value: number,
  ): number {
    return Math.max(
      MIN_WORKSPACE_LIST_WIDTH,
      Math.min(
        MAX_WORKSPACE_LIST_WIDTH,
        Math.round(value),
      ),
    );
  }

  function updateWorkspaceListState(
    state: { status: "loading" | "retrying" | "loaded"; total: number },
  ): void {
    workspaceListState = state;
  }

  function loadEmptyLaunchTargets(): void {
    if (
      emptyLaunchTargetsState === "loaded" ||
      emptyLaunchTargetsState === "loading"
    ) {
      return;
    }
    emptyLaunchTargetsState = "loading";
    emptyLaunchTargetsExecution = appRuntime.runCommand(
      Effect.gen(function* () {
        const api = yield* GeneratedApi;
        const settings = yield* api.execute("load workspace launch targets", (signal) =>
          api.client.SettingsService.getSettings({ signal }),
        );
        yield* Effect.sync(() => {
          emptyLaunchTargets = settings.launch_targets ?? [];
          emptyLaunchTargetsState = "loaded";
        });
      }).pipe(
        Effect.catch(() =>
          Effect.sync(() => {
            emptyLaunchTargets = [];
            emptyLaunchTargetsState = "error";
          }),
        ),
      ),
      {
        operation: "workspace.launch-targets.read",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
  }

  function readLocalStorage(key: string): string | null {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  }

  function writeLocalStorage(key: string, value: string): void {
    try {
      localStorage.setItem(key, value);
    } catch {
      // Best-effort UI preference persistence; keep the in-memory state.
    }
  }

  function loadWorkspaceListWidth(): number {
    const value = parseInt(
      readLocalStorage(WORKSPACE_LIST_WIDTH_KEY) ?? "",
      10,
    );
    return Number.isFinite(value)
      ? clampWorkspaceListWidth(value)
      : DEFAULT_WORKSPACE_LIST_WIDTH;
  }

  function sidebarTabStorageKey(storageId: string): string {
    return `${SIDEBAR_TAB_KEY_PREFIX}${storageId}`;
  }

  function loadSidebarOpen(): boolean {
    return readLocalStorage(SIDEBAR_OPEN_KEY) === "true";
  }

  const MIN_SIDEBAR_WIDTH = 280;
  const MIN_TERMINAL_WIDTH = 300;
  const DEFAULT_SIDEBAR_WIDTH = 640;
  const RIGHT_SIDEBAR_RESIZE_HANDLE_WIDTH = 4;

  function loadSidebarWidth(): number {
    const v = parseInt(
      readLocalStorage(SIDEBAR_WIDTH_KEY) ?? "",
      10,
    );
    return Number.isFinite(v)
      ? Math.max(MIN_SIDEBAR_WIDTH, v)
      : DEFAULT_SIDEBAR_WIDTH;
  }

  let selectedSidebarTabs = $state.raw<Record<string, SidebarTab>>({});
  const sidebarTab = $derived.by(() => {
    const storageId = workspaceStorageId(workspaceId, workspaceHostKey);
    const selected = selectedSidebarTabs[storageId];
    if (selected !== undefined) return selected;
    const saved = readLocalStorage(sidebarTabStorageKey(storageId));
    if (saved === "diff" || saved === "pr" || saved === "issue" || saved === "reviews" || saved === "kata") {
      return saved;
    }
    return workspace?.id === workspaceId && selectedWorkspaceHostKey(workspace) === workspaceHostKey
      ? defaultWorkspaceSidebarTab(settingsStore.getWorkspaceSettings().default_sidebar_view, workspace.item_type)
      : "diff";
  });
  let sidebarOpen = $state(loadSidebarOpen());
  let preferredRightSidebarWidth = $state(loadSidebarWidth());
  let workspaceListWidth = $state(loadWorkspaceListWidth());
  const currentWorkspaceListWidth = $derived(
    clampWorkspaceListWidth(
      externalWorkspaceListWidth ?? workspaceListWidth,
    ),
  );

  // Runtime is only "live" when both the runtime fetch and the
  // workspace fetch resolve for the current route. Without the
  // workspace.id check, a runtime that lands first for the new
  // workspace can render its sessions/launch targets next to the
  // previous workspace's still-cached header/home data.
  const workspaceLive = $derived(
    workspace?.id === workspaceId && selectedWorkspaceHostKey(workspace) === workspaceHostKey,
  );
  const runtimeLive = $derived(
    runtime !== null && runtimeForId === workspaceId && runtimeForHostKey === workspaceHostKey && workspaceLive,
  );
  const workspaceDetailsReady = $derived(workspaceLive && (runtimeLive || runtimeError !== null));

  function hasAppliedRuntimeFor(
    id: string,
    hostKey: string | undefined,
  ): boolean {
    return (
      appliedRuntimeState?.workspaceId === id &&
      appliedRuntimeState.hostKey === hostKey
    );
  }

  function releaseRuntimeRead(): void {
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.release(runtimeOwner);
      }),
      {
        operation: "workspace.runtime.release",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
  }

  function invalidateRuntimeSnapshot(): void {
    appliedRuntimeState = null;
    releaseRuntimeRead();
  }
  const runtimeSessions = $derived(
    runtimeLive
      ? (runtime?.sessions ?? []).filter(
          (session) =>
            !closedSessions.some((closed) =>
              sessionGenerationMatches(closed, session),
            ),
        )
      : [],
  );
  const launchTargets = $derived(
    runtimeLive ? (runtime?.launch_targets ?? []) : [],
  );
  const sessionDisplayLabels = $derived.by(() => {
    const labels: Record<string, string> = {};
    for (const session of runtimeSessions) {
      labels[session.key] = session.label;
    }
    return labels;
  });
  // The surface this view is embedded in, whose stored pane tree is the sole
  // record of which sessions have been promoted out of this container.
  const surfaceLayout = $derived(paneSurface ? getPaneLayoutStore(paneSurface) : null);
  type WorkspaceInputRegion = "workflow" | "details" | "terminal";
  let workspaceRoot = $state<HTMLElement | null>(null);
  let workspaceInputRegion = $state<WorkspaceInputRegion | null>(null);
  let focusedWorkflowTabKey = $state<WorkflowTabKey | null>(null);
  let focusedWorkflowInputElement: HTMLElement | null = null;
  const workspaceContainerInputActive = $derived(
    surfaceLayout === null || surfaceLayout.paneRender()?.activeInputTabKey === "workspace",
  );
  const renderedWorkspaceInputRegion = $derived.by<WorkspaceInputRegion | null>(() => {
    if (!hostVisible) return null;
    if (workspaceInputRegion === "workflow" && !runtimeLive) return null;
    if (
      workspaceInputRegion === "details" &&
      (!workspaceDetailsReady || hideRightSidebar || !sidebarOpen)
    ) {
      return null;
    }
    if (workspaceInputRegion === "terminal" && terminalLayout.dock !== "bottom") {
      return null;
    }
    return workspaceInputRegion;
  });

  $effect.pre(() => {
    const workflowVisible = hostVisible && runtimeLive;
    const detailsVisible =
      hostVisible && workspaceDetailsReady && !hideRightSidebar && sidebarOpen;
    const terminalVisible = hostVisible && terminalLayout.dock === "bottom";
    const focusedRegion = untrack(() => workspaceInputRegion);
    const focusedRegionDisappears =
      (focusedRegion === "workflow" && !workflowVisible) ||
      (focusedRegion === "details" && !detailsVisible) ||
      (focusedRegion === "terminal" && !terminalVisible);
    if (!focusedRegionDisappears || !hostVisible) return;

    // Capture disappearance before Svelte removes the focused region. Chromium
    // then reports focusout after the DOM update, which is too late to know which
    // workspace sibling owned focus. Reclaim only when the browser has nowhere
    // better to put it; a replacement control or modal always keeps ownership.
    // Readiness settles in phases during a workspace switch. Keep this bounded
    // one-tick command independent of effect reruns so the next phase cannot
    // interrupt the recovery before it observes the updated DOM.
    appRuntime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(Effect.sync(() => {
          if (!hostVisible || document.activeElement !== document.body) return;
          workspaceRoot?.focus();
        })),
      ),
      {
        operation: "workspace.restore.focus",
        safeContext: { surface: "workspace", region: focusedRegion },
        onFailure: () => undefined,
      },
    );
  });

  $effect(() => {
    if (workspaceInputRegion !== null && renderedWorkspaceInputRegion === null) {
      if (workspaceInputRegion === "workflow") {
        focusedWorkflowTabKey = null;
        focusedWorkflowInputElement = null;
      }
      workspaceInputRegion = null;
    }
  });

  function activateWorkspaceInputRegion(region: WorkspaceInputRegion): void {
    workspaceInputRegion = region;
  }

  function deactivateWorkspaceInputRegion(
    region: WorkspaceInputRegion,
    event: FocusEvent & { currentTarget: HTMLElement },
  ): void {
    const next = event.relatedTarget;
    if (next instanceof Node && event.currentTarget.contains(next)) return;
    if (workspaceInputRegion === region) {
      if (region === "workflow") {
        focusedWorkflowTabKey = null;
        focusedWorkflowInputElement = null;
      }
      workspaceInputRegion = null;
    }
  }

  function sessionPaneKeyFor(session: RuntimeSession): string {
    return sessionPaneKey(workspaceId, workspaceHostKey, session.key);
  }

  function detailPaneKeyForSession(sessionKey: string): string | null {
    const session = runtimeSessions.find((candidate) => candidate.key === sessionKey);
    return session ? sessionPaneKeyFor(session) : null;
  }

  const promotedSessionKeys = $derived(
    new Set(
      surfaceLayout === null
        ? []
        : runtimeSessions
            .filter((session) => surfaceLayout.hasTab(sessionPaneKeyFor(session)))
            .map((session) => session.key),
    ),
  );

  function isPromoted(session: RuntimeSession): boolean {
    return promotedSessionKeys.has(session.key);
  }

  /**
   * The one session this view is embedded to show, or null.
   *
   * A pane whose only content is a single terminal needs no chrome of its own: the
   * pane's tab strip already names it and carries its controls, so the header bar
   * and the one-tab strip under it were two bars saying what the tab above them
   * says. Null unless the surface actually gives this view that strip - a flattened
   * surface suppresses per-leaf chrome, so there the toolbar is the only thing left
   * to carry the controls.
   */
  const soleEmbeddedSession = $derived.by(() => {
    if (!controlsInPane || runtimeSessions.length !== 1) return null;
    const session = runtimeSessions[0]!;
    if (isPromoted(session)) return null;
    // Only while the surface's strip actually names this session: a leaf holding
    // the workspace alone drops that strip (solo chrome), and rendering a
    // workflow session bare there leaves NOTHING on screen naming it - the inner
    // session strip is the one bar that pane has. The dock's own sole session
    // stays bare: its pane renders as a plain terminal, and the workflow tree it
    // would otherwise mount has no tab for a docked session anyway.
    if (
      sessionRegion(session) !== "terminal" &&
      surfaceLayout?.paneRender()?.soloChromeTabs.includes("workspace")
    ) {
      return null;
    }
    return session;
  });

  /**
   * Its registry key, derived rather than computed where it is rendered.
   *
   * The slot's prop is its own derived, so it re-runs on the flush that clears this
   * session (the host left the pane for the Workspaces tab) BEFORE the block
   * rendering it is torn down. Computing the key from the session down there threw
   * on null, and a throw mid-flush takes the whole app's render with it: the host
   * stayed in its parking node and the workspace tab came up empty.
   */
  const soleEmbeddedSessionHostKey = $derived(
    soleEmbeddedSession === null ? null : sessionHostKeyFor(soleEmbeddedSession),
  );

  /**
   * Whether the one session filling a chrome-free pane is the dock's own.
   *
   * The dock otherwise stays on screen in a chrome-free pane, but a docked sole
   * session is already what the stage is showing - rendering the dock under it would
   * point a second slot at the same registry key, and one terminal host cannot be in
   * two places at once.
   */
  const soleEmbeddedSessionIsDocked = $derived(
    soleEmbeddedSession !== null && sessionRegion(soleEmbeddedSession) === "terminal",
  );

  // The two directions of the cross-tree drag: a session tab carries its pane key
  // out, and a promoted pane's key resolves back to the tab it belongs to. Both
  // reject anything that is not a live session of THIS workspace on this host — a
  // session key is unique only within one workspace.
  const workflowPromotion = $derived(
    surfaceLayout === null
      ? undefined
      : {
          paneKeyFor: (tabKey: WorkflowTabKey) => {
            const sessionKey = sessionKeyFromWorkflowTab(tabKey);
            if (sessionKey === null) return null;
            const session = runtimeSessions.find((candidate) => candidate.key === sessionKey);
            return session ? sessionPaneKeyFor(session) : null;
          },
          tabKeyFor: (paneKey: string) => {
            if (!sessionPaneKeyMatchesWorkspace(paneKey, workspaceId, workspaceHostKey)) return null;
            const sessionKey = parseSessionPaneKey(paneKey)?.sessionKey ?? null;
            if (sessionKey === null) return null;
            if (!runtimeSessions.some((candidate) => candidate.key === sessionKey)) return null;
            return workflowTabKeyForSession(sessionKey);
          },
        },
  );

  // Publish what a detail surface may promote. Only this view knows the runtime,
  // the display labels, and each session's generation, and the registry key needs
  // the generation so a relaunched session is not handed the dead one's terminal.
  $effect(() => {
    const sessions = runtimeSessions.map((session) => ({
      paneKey: sessionPaneKeyFor(session),
      label: sessionDisplayLabels[session.key] ?? session.label,
      hostKey: sessionHostKeyFor(session),
      active: session.key === currentSessionKey,
    }));
    const key = { workspaceId, hostKey: workspaceHostKey };
    untrack(() => publishHostedSessions(key, sessions));
  });

  // A detail pane trades the Home tab for the launcher overlay. Keyed off the pane
  // surface rather than the flatten width: unlike the controls, the overlay is a
  // modal and needs no chrome of its own to live in.
  const launcherMode = $derived(paneSurface !== undefined);
  // `auto` records who opened it: an overlay the view raised over an empty pane is
  // its own to take back once there is something to show, while one the user asked
  // for stays until they dismiss it - they may be picking a second session.
  let launcherState = $state<{ workspaceKey: string; auto: boolean } | null>(null);
  const launcherOpen = $derived(launcherState?.workspaceKey === viewWorkspaceKey);
  // Which workspaces the overlay has auto-opened for, so selecting the same item
  // twice does not reopen a launcher the user dismissed, while a different workspace
  // with no session still gets one. A list rather than a single slot: A, then B,
  // then back to A must not reopen A's launcher.
  let launcherAutoOpenedFor = $state<string[]>([]);

  function openLauncher(): void {
    if (!launcherMode) {
      selectWorkspaceTab("home");
      return;
    }
    launcherState = { workspaceKey: viewWorkspaceKey, auto: false };
  }

  function explicitLaunchIntentPending(): boolean {
    const identity = workspaceIdentitySnapshot(workspaceId);
    return (
      (identity !== undefined && pendingWorkspaceCreateLaunch(identity) !== null) ||
      pendingWorkspaceLaunch(workspaceId, workspaceHostKey) !== null
    );
  }

  function createOrLaunchPending(): boolean {
    return explicitLaunchIntentPending() || launchingKey !== null;
  }

  const automaticLauncherBlocked = $derived(explicitLaunchIntentPending());
  const launcherOverlayAllowed = $derived(
    launcherState?.auto !== true || !automaticLauncherBlocked,
  );

  /**
   * The view's own fallback when a pane has nothing left to render.
   *
   * Once per workspace, for every automatic path: the empty pane on arrival and the
   * one left behind when the last session goes away are the same situation, and a
   * launcher that came back each time would trap a user who dismissed it - revisit
   * the item, close a session, and it is in the way again. Their own route back is
   * the Launch button in the pane's controls.
   */
  function autoOpenLauncher(): void {
    // Only over a workspace that can actually host a session, on every automatic
    // path. A worktree still being created, or one whose setup failed - or whose tmux
    // server dropped the session out from under it - reports zero sessions for the
    // same reason it reports its state, and the launcher answered that by covering the
    // state message, and the Retry and Delete beside it, with an invitation to start
    // an agent inside something that cannot run one.
    if (workspace?.status !== "ready") return;
    // Runtime teardown can reach any of the automatic fallback paths before the
    // deleted inline host is removed. It is not an empty workspace to relaunch.
    if (deletingSelectedWorkspace || forceDeleting) return;
    // A split-button selection already chose what to launch. Creation can publish
    // the ready workspace before its POST response hands the choice to the
    // workspace-ID launch queue, and the runtime stays empty until that launch
    // produces its first session.
    if (createOrLaunchPending()) return;
    if (launcherAutoOpenedFor.includes(viewWorkspaceKey)) return;
    launcherAutoOpenedFor = [...launcherAutoOpenedFor, viewWorkspaceKey];
    launcherState = { workspaceKey: viewWorkspaceKey, auto: true };
  }

  function closeLauncher(): void {
    launcherState = null;
  }

  /**
   * Withdraw an automatic launcher the view should never have shown.
   *
   * The marker goes with it, unlike a dismissal: it exists to stop a launcher the
   * USER closed from coming back, and holding it for one the view took back itself
   * would mean a workspace that recovers - Retry, setup finishes, still no sessions -
   * never gets the launcher it should have had.
   */
  function withdrawAutoLauncher(): void {
    if (launcherState?.workspaceKey !== viewWorkspaceKey || !launcherState.auto) return;
    launcherAutoOpenedFor = launcherAutoOpenedFor.filter((key) => key !== viewWorkspaceKey);
    launcherState = null;
  }

  /**
   * Where to go when the tab the user was on disappears - a session stopped, the
   * terminal panel closed, a tab moved to the dock.
   *
   * Home is that place outside a pane. Inside one there is no Home, so it is
   * whatever workflow tab is left, and the launcher when the workspace has nothing
   * left to show: a pane rendering an empty strip is a dead end.
   */
  function selectFallbackTab(): void {
    if (!launcherMode) {
      selectWorkspaceTab("home");
      return;
    }
    const next = workflowTabDescriptors.find((tab) => tab.key !== activeTabKey);
    if (next !== undefined) {
      selectWorkspaceTab(next.key);
      return;
    }
    // The dock counts as something to show: its sessions are not workflow tabs, so
    // a workspace whose only terminal is docked has an empty strip and nothing
    // missing.
    if (runtimeSessions.length === 0) autoOpenLauncher();
  }

  // Reachable from outside the view: the palette command, and a Focus Terminal that
  // finds no session to focus. Only while embedded, since that is the only mode with
  // an overlay to open.
  $effect(() => {
    if (!launcherMode) return;
    untrack(() => registerWorkspaceLauncher(openLauncher));
    return () => untrack(() => registerWorkspaceLauncher(null));
  });

  // A pane whose only tab was Home has nothing to show, and a remembered Home tab
  // names a tab that no longer exists here. Both resolve the same way: show whatever
  // session is there, and open the launcher when there is none.
  $effect(() => {
    if (!launcherMode || !runtimeLive) return;
    // A workspace that turns out not to be ready takes its launcher back. The runtime
    // load lands before the workspace record does, so the overlay is already up by the
    // time the state is known - and the guard in autoOpenLauncher cannot undo what it
    // did not do.
    if (workspace !== null && workspace.status !== "ready") {
      untrack(() => withdrawAutoLauncher());
      return;
    }
    const tabs = workflowTabDescriptors;
    const activeMissing = !tabs.some((tab) => tab.key === activeTabKey);
    const workspaceKey = viewWorkspaceKey;
    const anySession = runtimeSessions.length > 0;
    const openState = launcherState;
    const autoOpened = launcherAutoOpenedFor.includes(workspaceKey);
    const deletionPending = deletingSelectedWorkspace || forceDeleting;
    const explicitLaunchIsPending = explicitLaunchIntentPending();
    const createOrLaunchIsPending = createOrLaunchPending();
    untrack(() => {
      if (tabs.length > 0 && activeMissing) selectWorkspaceTab(tabs[0]!.key);
      // A docked terminal is not a workflow tab but is very much on screen, so an
      // empty strip alone does not mean the workspace has nothing to show.
      if (tabs.length > 0 || anySession) {
        // Take back what the view opened, and only that: a reconnect, a relaunch,
        // or a first runtime load that lands before its sessions do all report zero
        // sessions for a moment, and an auto-opened launcher left over that gap
        // would then cover the terminal it was standing in for.
        if (openState?.workspaceKey === workspaceKey && openState.auto) closeLauncher();
        return;
      }
      // Deletion tears down the runtime before the inline host disappears. That
      // sessionless gap is not an empty workspace asking what to launch next.
      if (createOrLaunchIsPending) {
        // The explicit target can arrive just after the empty-runtime pass opened
        // the fallback. Retract only that automatic overlay immediately; waiting
        // for the launched session to appear leaves the redundant picker flashing
        // over the terminal startup the user already requested.
        if (explicitLaunchIsPending && openState?.workspaceKey === workspaceKey && openState.auto) {
          withdrawAutoLauncher();
        }
        return;
      }
      if (deletionPending) return;
      // Once per workspace: reopening a launcher the user dismissed would trap them
      // in it, while a different session-less workspace still gets one.
      if (openState?.workspaceKey === workspaceKey || autoOpened) return;
      autoOpenLauncher();
    });
  });

  // Below the flatten width a detail surface shows one tab strip for every pane and
  // suppresses per-leaf chrome, so there is nowhere to hang the controls button and
  // the toolbar is the only thing left that can carry them.
  const paneFlattened = $derived(surfaceLayout?.paneRender()?.flattened ?? false);
  const controlsInPane = $derived(paneSurface !== undefined && !paneFlattened);
  // Reported against the workspace whose controls are on screen, and released on the
  // way out. The workspace-scoped half of this (a preset apply) is tracked per
  // workspace so a write that lands after a switch cannot pin the next workspace's
  // popover open; the settings writes are global and hold whichever controls show
  // them, which is where their feedback is.
  $effect(() => {
    const workspaceKey = viewWorkspaceKey;
    // Only what a pane's popover can actually start. Presets are not offered there,
    // so a preset apply cannot be the write that must not be interrupted.
    const busy = terminalOptionsSaving || terminalZoomSaving;
    untrack(() => setWorkspaceControlsBusy(workspaceKey, controlsInPane && busy));
    return () => untrack(() => setWorkspaceControlsBusy(workspaceKey, false));
  });

  // The session a keyboard promote acts on: whichever one the user is looking at.
  // A workflow tab wins because it fills the pane, and the dock's active tab only
  // counts while the dock is open - a collapsed dock shows no terminal at all.
  const currentSessionKey = $derived(
    // A chrome-free pane shows its one session whatever region it belongs to and
    // whether or not that region's dock is collapsed, so it is the current one by
    // definition. Deriving "current" from the layout alone left a terminal the user
    // is looking at reported as inactive, which took the promote command away from
    // the only session there was to promote.
    soleEmbeddedSession?.key ??
      sessionKeyFromWorkflowTab(activeTabKey) ??
      (terminalLayout.open ? terminalLayout.activeSessionKey : null),
  );

  // Entering a detail item that hosts a live terminal asks for the keyboard,
  // softly: the pool declines while focus is somewhere sacred, so navigation
  // never pulls the user out of a form field or dialog. Without this, only a
  // terminal the user had personally focused ever re-acquired focus (the
  // pool's ownership restore), which made acquisition look random across PRs.
  // Fires once per claim while the surface stays visible; leaving and coming
  // back — another item, another tab — arms it again.
  let acquiredFocusClaim: string | null = null;
  $effect(() => {
    if (paneSurface === undefined || !hostVisible) {
      acquiredFocusClaim = null;
      return;
    }
    const session = runtimeSessions.find((candidate) => candidate.key === currentSessionKey);
    if (session === undefined) return;
    const sessionHost = sessionHostKeyFor(session);
    if (!isSessionSlotVisible(sessionHost)) return;
    const claim = `${workspaceId} ${workspaceHostKey ?? ""}`;
    if (acquiredFocusClaim === claim) return;
    acquiredFocusClaim = claim;
    requestSessionFocus(sessionHost, { soft: true });
  });

  // Promotion implies mounted. Demotion hands the session back to the workflow
  // region, whose slot only renders for a mounted session, so a session promoted
  // without ever having been opened here would go dark the instant it came home -
  // its pool entry dropped in the same flush that gave it a tab.
  $effect(() => {
    const promoted = [...promotedSessionKeys];
    untrack(() => {
      for (const sessionKey of promoted) mountSessionTerminal(sessionKey);
    });
  });

  /**
   * The dock's own way out: give this session a pane of its own on the surface
   * hosting the workspace. Undefined when nothing is hosting one, which is what
   * keeps the control off the standalone tab and the embed routes.
   */
  const promoteSessionToPane = $derived(
    surfaceLayout === null
      ? undefined
      : (sessionKey: string) => {
          const session = runtimeSessions.find((candidate) => candidate.key === sessionKey);
          if (!session) return;
          promoteSessionBesideWorkspace(surfaceLayout, sessionPaneKeyFor(session));
        },
  );

  /** Bring a promoted session home before a container edit places it. */
  function demoteWorkflowTab(tabKey: WorkflowTabKey): void {
    const sessionKey = sessionKeyFromWorkflowTab(tabKey);
    if (sessionKey === null) return;
    const session = runtimeSessions.find((candidate) => candidate.key === sessionKey);
    if (!session || !isPromoted(session)) return;
    surfaceLayout?.demoteTab(sessionPaneKeyFor(session));
  }

  const terminalSessions = $derived(
    runtimeSessions.filter(
      (session) => sessionRegion(session) === "terminal" && !isPromoted(session),
    ),
  );

  // An open dock with nothing in it is a saved-height hole in the stage: the last
  // docked session exiting (or moving to the workflow) leaves open=true behind, and
  // the collapsed row is the only honest rendering of "no terminals here". Not
  // while a launch is in flight - toggling the panel open auto-launches, and
  // closing it under that race would flicker the dock shut on its own opening.
  $effect(() => {
    if (!runtimeLive || terminalLaunching) return;
    // Bottom only. Docked to the top the dock is a workflow TAB, and an empty one
    // is the drop target for moving a session into the terminal region - closing
    // it there takes away the affordance instead of a hole.
    if (terminalLayout.dock !== "bottom") return;
    if (!terminalLayout.open || terminalSessions.length > 0) return;
    untrack(() => {
      terminalLayout = { ...terminalLayout, open: false };
    });
  });

  // Masked, not pruned: the stored trees keep a promoted session's tab and leaf so
  // demotion hands back the placement the user chose, rather than dropping it back
  // wherever normalization would put a session it has never seen.
  const dockTree = $derived(
    promotedSessionKeys.size === 0
      ? terminalLayout.tree
      : pruneTree(
          terminalLayout.tree,
          terminalSessions.map((session) => session.key),
        ),
  );

  function upsertRuntimeSession(session: RuntimeSession): RuntimeSession[] {
    const currentRuntime =
      runtime !== null &&
      runtimeForId === workspaceId &&
      runtimeForHostKey === workspaceHostKey
        ? runtime
        : null;
    if (currentRuntime === null) runtimeSnapshotAuthoritative = false;
    const sessions = [
      ...(currentRuntime?.sessions ?? []).filter(
        (candidate) =>
          candidate.key !== session.key &&
          !closedSessions.some((closed) =>
            sessionGenerationMatches(closed, candidate),
          ),
      ),
      session,
    ];
    invalidateRuntimeSnapshot();
    runtime = {
      launch_targets: currentRuntime?.launch_targets ?? [],
      sessions: [
        ...(currentRuntime?.sessions ?? []).filter((candidate) => candidate.key !== session.key),
        session,
      ],
    };
    runtimeForId = workspaceId;
    runtimeForHostKey = workspaceHostKey;
    return sessions;
  }
  const currentTerminalGroup = $derived(activeTerminalGroup(terminalLayout));
  const workflowSessions = $derived(
    runtimeSessions.filter(
      (session) => sessionRegion(session) === "workflow" && !isPromoted(session),
    ),
  );
  function workflowSessionStatus(
    session: RuntimeSession,
    label: string,
  ): WorkflowTabDescriptor["status"] {
    if (session.status === "running") {
      return { value: "idle", label: `${label} running` };
    }
    if (session.status === "starting") {
      return { value: "stale", label: `${label} starting` };
    }
    if (session.status === "error") {
      return { value: "unclean", label: `${label} unavailable` };
    }
    return undefined;
  }

  const workflowTabDescriptors = $derived.by<WorkflowTabDescriptor[]>(() => {
    // No Home tab inside a detail pane: the workspace gets one pane there, and
    // spending half its height on a surface only used to start something is the
    // trade this mode exists to undo. The launcher overlay replaces it.
    const tabs: WorkflowTabDescriptor[] = launcherMode
      ? []
      : [
          {
            key: "home",
            label: "Home",
            kind: "home",
          },
        ];
    if (
      terminalLayout.dock === "top" &&
      (terminalLayout.open || terminalSessions.length > 0)
    ) {
      tabs.push({
        key: "terminal",
        label: "Terminal",
        kind: "terminal",
        closable: true,
      });
    }
    for (const session of workflowSessions) {
      const label = sessionDisplayLabels[session.key] ?? session.label;
      tabs.push({
        key: workflowTabKeyForSession(session.key),
        label,
        kind: session.kind === "plain_shell" ? "plain_shell" : "agent",
        targetKey: session.target_key,
        status: workflowSessionStatus(session, label),
        renamable: true,
        movableToTerminal: true,
        closable: true,
      });
    }
    return tabs;
  });
  const renderedWorkflowTree = $derived(
    promotedSessionKeys.size === 0 && !launcherMode
      ? terminalLayout.workflowTree
      : pruneWorkflowTreeToAvailable(
          // Embedded too, not just when something is promoted: the stored tree still
          // names Home, and a leaf whose only tab has no descriptor renders a strip
          // with nothing in it.
          terminalLayout.workflowTree,
          workflowTabDescriptors.map((tab) => tab.key),
      ),
  );
  function workflowContentKeyFor(tabKey: WorkflowTabKey | null): string | null {
    if (tabKey === null) return soleEmbeddedSessionHostKey;
    const leaf = findWorkflowLeafByTab(renderedWorkflowTree, tabKey);
    return leaf === null ? null : `${leaf.id}|${leaf.activeTabKey}`;
  }

  const workflowInputContentKey = $derived(workflowContentKeyFor(focusedWorkflowTabKey));
  let lastWorkflowInputContentKey = untrack(() => workflowInputContentKey);

  $effect.pre(() => {
    const contentKey = workflowInputContentKey;
    const contentChanged = contentKey !== lastWorkflowInputContentKey;
    lastWorkflowInputContentKey = contentKey;
    const shouldRestore = untrack(() => workspaceInputRegion === "workflow" && hostVisible);
    if (!contentChanged || !shouldRestore) return;
    const focusedInput = untrack(() => focusedWorkflowInputElement);
    // A focused session can disappear without focusout. Inspect the updated DOM
    // before clearing ownership or moving focus. Connected pooled terminals own
    // their reparenting handoff; only disconnected content falls back to the root.
    // Focusout can clear the recorded tab while the DOM update is settling.
    // Keep this bounded recovery independent of that effect rerun so it still
    // gets to inspect the updated DOM and restore a disconnected descendant.
    appRuntime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(Effect.sync(() => {
          if (!hostVisible) return;
          const focused = document.activeElement;
          if (focused !== null && focused !== document.body) return;
          focusedWorkflowTabKey = null;
          focusedWorkflowInputElement = null;
          workspaceInputRegion = null;
          if (focusedInput?.isConnected) return;
          workspaceRoot?.focus();
        })),
      ),
      {
        operation: "workspace.restore.focus",
        safeContext: { surface: "workspace", region: "workflow" },
        onFailure: () => undefined,
      },
    );
  });
  const workspacePaneEmpty = $derived(
    controlsInPane &&
      runtimeSessions.length > 0 &&
      promotedSessionKeys.size === runtimeSessions.length,
  );
  const workspacePaneRowOnly = $derived(
    controlsInPane &&
      runtimeLive &&
      soleEmbeddedSessionHostKey === null &&
      renderedWorkflowTree === null &&
      terminalLayout.dock === "bottom" &&
      terminalSessions.length > 0,
  );
  const externalDockVisible = $derived(workspacePaneEmpty || workspacePaneRowOnly);

  $effect(() => {
    const layout = surfaceLayout;
    if (layout === null) return;
    if (!externalDockVisible) untrack(() => layout.setExternalInputActive(false));
    return () => untrack(() => layout.setExternalInputActive(false));
  });
  const externalControlsVisible = $derived(
    workspacePaneEmpty ||
      workspacePaneRowOnly ||
      (surfaceLayout?.paneRender()?.onScreenTabs.some((tabKey) =>
        sessionPaneKeyMatchesWorkspace(tabKey, workspaceId, workspaceHostKey),
      ) ??
        false),
  );
  // A parked terminal host stays inactive, but controls actually rendered in a
  // promoted pane or external dock still own portalled UI. Do not use the broad
  // `controlsInPane` flag here: it is also true during a selection handoff before
  // the destination slot mounts, and opening a launcher in that parked window
  // prevents Focus Terminal from completing the reveal on Firefox.
  const interactionVisible = $derived(hostVisible || externalControlsVisible);

  // Handed to the detail pane's controls popover, which is where the controls live
  // once this view is embedded. Registered with the workspace it acts on, because
  // one embedded view serves every selection on its surface: the snippet is the
  // same object after a switch, so only the key can tell a popover that its
  // subject changed. The row-only fact rides the same lifecycle so it cannot leak
  // from a parked or previously selected workspace.
  $effect(() => {
    if (!controlsInPane) return;
    const workspaceKey = viewWorkspaceKey;
    const rowOnly = workspacePaneRowOnly;
    // Untracked because the store compares against what is already registered:
    // reading that inside a tracked effect that also writes it is the read-write
    // loop Svelte aborts with effect_update_depth_exceeded.
    untrack(() =>
      registerWorkspaceControls({
        snippet: workspaceControls,
        paneActions: workspacePaneActions,
        stripActions: workspaceStripActions,
        dockRow: workspaceDockRow,
        workspacePaneRowOnly: rowOnly,
        workspaceKey,
      }),
    );
    return () => untrack(() => registerWorkspaceControls(null));
  });

  /**
   * Session tabs the workflow tree is showing right now: one per rendered leaf.
   *
   * A leaf shows its active tab and nothing else, so this is what needs a terminal
   * on screen -- the other tabs in the leaf are a click away and stay unmounted.
   */
  function activeWorkflowSessionKeys(node: WorkflowNode | null): string[] {
    if (node === null) return [];
    if (node.type === "leaf") {
      const sessionKey = sessionKeyFromWorkflowTab(node.activeTabKey);
      return sessionKey === null ? [] : [sessionKey];
    }
    return [...activeWorkflowSessionKeys(node.first), ...activeWorkflowSessionKeys(node.second)];
  }

  /**
   * Mount whatever the workflow tree is showing, without waiting for a click.
   *
   * Mounting used to happen only in the tab strip's select handler, so a workspace
   * whose session was ALREADY the active tab -- every workspace the user opens with
   * an agent running in it -- rendered an empty pane. Nothing in the view said the
   * terminal was one click away, so it read as a broken pane rather than a closed
   * one, and any interaction that re-selected the tab fixed it by accident.
   */
  $effect(() => {
    if (!runtimeLive) return;
    const live = new Set(runtimeSessions.map((session) => session.key));
    const showing = activeWorkflowSessionKeys(renderedWorkflowTree).filter((key) => live.has(key));
    // Untracked: mountSessionTerminal writes the mounted list that the tree's own
    // descriptors read, and it is a no-op for a session already mounted, so this
    // settles instead of re-running itself.
    untrack(() => {
      for (const key of showing) mountSessionTerminal(key);
    });
  });

  const terminalPanelInStage = $derived(
    terminalLayout.open && terminalLayout.dock === "top",
  );

  // While `workspaceId` has moved on but the previous workspace's
  // data is still on screen (the in-place transition), mutating
  // actions must not run — they would target the new id while the
  // user is looking at the old one. The window is small (≤ a few
  // hundred ms) but observable, so guard every action handler with
  // this and disable the buttons.
  const transitioning = $derived(
    workspaceId !== "" &&
      workspace !== null &&
      (workspace.id !== workspaceId ||
        workspaceHostKey !== selectedWorkspaceHostKey(workspace)),
  );
  const deletingSelectedWorkspace = $derived(
    deletingWorkspaceTargets.some((target) =>
      isDeletingWorkspaceTarget(target, workspaceId, workspaceHostKey),
    ) || isWorkspaceDeletionPending(workspaceId, workspaceHostKey),
  );
  const workspaceDeletionLifecycleActive = $derived(
    workspaceLive &&
      (workspace?.status === "deleting" || workspace?.status === "deletion_failed"),
  );
  const actionsBlocked = $derived(
    transitioning ||
      deletingSelectedWorkspace ||
      workspaceDeletionLifecycleActive ||
      forceDeleting,
  );
  const inlineDockMode = $derived(inlineDock?.getMode() ?? null);
  const inlineDockExpandBlocked = $derived(getStackDepth() > 0);
  const modalOpen = $derived(
    forcePromptMessage !== null ||
      stopPromptSession !== null ||
      deletePromptOpen ||
      renamePrompt !== null,
  );

  $effect(() => {
    writeLocalStorage(
      SIDEBAR_OPEN_KEY,
      String(sidebarOpen),
    );
  });
  $effect(() => {
    writeLocalStorage(
      SIDEBAR_WIDTH_KEY,
      String(preferredRightSidebarWidth),
    );
  });
  $effect(() => {
    if (externalWorkspaceListWidth !== undefined) return;
    writeLocalStorage(
      WORKSPACE_LIST_WIDTH_KEY,
      String(workspaceListWidth),
    );
  });
  $effect(() => {
    if (!workspaceId) return;
    const storageId = workspaceStorageId(workspaceId, workspaceHostKey);
    if (terminalLayoutWorkspaceId !== storageId) return;
    writeLocalStorage(
      terminalLayoutStorageKey(storageId),
      JSON.stringify(terminalLayout),
    );
  });
  $effect(() => {
    writeLocalStorage(WORKFLOW_PRESETS_KEY, JSON.stringify(workflowPresets));
  });

  function handleSidebarToggleClick(tab: SidebarTab): void {
    if (actionsBlocked) return;
    if (sidebarOpen && sidebarTab === tab) {
      sidebarOpen = false;
    } else {
      setSidebarTab(tab);
      sidebarOpen = true;
    }
  }

  function setSidebarTab(
    tab: SidebarTab,
    targetId: string | undefined = undefined,
    targetHostKey?: string,
  ): void {
    const storageId = workspaceStorageId(
      targetId ?? workspaceId,
      targetId === undefined ? workspaceHostKey : targetHostKey,
    );
    selectedSidebarTabs = { ...selectedSidebarTabs, [storageId]: tab };
    writeLocalStorage(sidebarTabStorageKey(storageId), tab);
  }

  function openItemSidebar(
    targetId: string,
    tab: SidebarTab,
    targetHostKey?: string,
  ): void {
    // Cross-workspace click: navigate first, then ensure
    // the sidebar is open for the target tab.
    if (
      targetId !== workspaceId ||
      (targetHostKey ?? undefined) !== workspaceHostKey
    ) {
      setSidebarTab(tab, targetId, targetHostKey);
      sidebarOpen = true;
      if (targetHostKey) {
        navigate(
          `/terminal/fleet/${encodeURIComponent(targetHostKey)}/${encodeURIComponent(targetId)}`,
        );
      } else {
        navigate(`/terminal/${encodeURIComponent(targetId)}`);
      }
      return;
    }

    handleSidebarToggleClick(tab);
  }

  function toggleRightSidebar(): void {
    sidebarOpen = !sidebarOpen;
  }

  function handleWorkspaceListResize(width: number): void {
    const clamped = clampWorkspaceListWidth(width);
    if (onSidebarResize) {
      onSidebarResize(clamped);
    } else {
      workspaceListWidth = clamped;
    }
  }

  let containerEl = $state<HTMLElement | null>(null);
  let containerWidth = $state(0);

  function maxRightSidebarWidth(
    containerWidth: number,
  ): number {
    return Math.max(
      0,
      containerWidth -
        MIN_TERMINAL_WIDTH -
        RIGHT_SIDEBAR_RESIZE_HANDLE_WIDTH,
    );
  }

  const rightSidebarAriaMax = $derived(
    containerWidth > 0
      ? maxRightSidebarWidth(containerWidth)
      : Math.max(MIN_SIDEBAR_WIDTH, preferredRightSidebarWidth),
  );
  const rightSidebarAriaMin = $derived(
    Math.min(MIN_SIDEBAR_WIDTH, rightSidebarAriaMax),
  );
  const rightSidebarResizeDisabled = $derived(
    rightSidebarAriaMax < MIN_SIDEBAR_WIDTH,
  );
  // The saved width records user intent. A narrow window or a wider workspace
  // list may temporarily leave less room, but that layout constraint must not
  // replace the preference and strand the pane at its constrained width after
  // space returns.
  const renderedRightSidebarWidth = $derived(
    containerWidth > 0
      ? Math.min(preferredRightSidebarWidth, maxRightSidebarWidth(containerWidth))
      : preferredRightSidebarWidth,
  );

  let sidebarResizeStartWidth = 0;
  let sidebarResizeMinWidth = MIN_SIDEBAR_WIDTH;
  let sidebarResizeMaxWidth = 9999;

  function handleSidebarResizeStart(): void {
    sidebarResizeStartWidth = renderedRightSidebarWidth;
    sidebarResizeMaxWidth = containerEl
      ? maxRightSidebarWidth(containerEl.clientWidth)
      : 9999;
    sidebarResizeMinWidth = Math.min(
      MIN_SIDEBAR_WIDTH,
      sidebarResizeMaxWidth,
    );
  }

  function handleSidebarResize(event: SplitResizeEvent): void {
    if (rightSidebarResizeDisabled) return;
    preferredRightSidebarWidth = Math.max(
      sidebarResizeMinWidth,
      Math.min(
        sidebarResizeMaxWidth,
        sidebarResizeStartWidth - event.delta,
      ),
    );
  }

  // Window-level shortcut, so it must not stay registered while this view
  // is parked in a hidden host (it would swallow Cmd/Ctrl+] on unrelated
  // pages) or while the right sidebar isn't rendered at all.
  $effect(() => {
    if (!hostVisible || hideRightSidebar || !workspaceContainerInputActive) return;
    function onKeydown(e: KeyboardEvent): void {
      const focusTarget = e.target instanceof Node ? e.target : document.activeElement;
      if (
        focusTarget !== null &&
        focusTarget !== document.body &&
        workspaceRoot?.contains(focusTarget) !== true
      ) {
        return;
      }
      if (
        e.key === "]" &&
        (e.metaKey || e.ctrlKey) &&
        !e.defaultPrevented &&
        getStackDepth() === 0
      ) {
        e.preventDefault();
        toggleRightSidebar();
      }
    }
    window.addEventListener("keydown", onKeydown);
    return () =>
      window.removeEventListener("keydown", onKeydown);
  });

  function displayName(ws: Workspace): string {
    return ws.mr_title ?? ws.git_head_ref;
  }

  function mountSessionTerminal(sessionKey: string): void {
    if (!mountedSessionKeys.includes(sessionKey)) {
      mountedSessionKeys = [...mountedSessionKeys, sessionKey];
    }
  }

  function unmountSessionTerminal(sessionKey: string): void {
    mountedSessionKeys = mountedSessionKeys.filter(
      (key) => key !== sessionKey,
    );
  }

  function sessionHostKeyFor(session: RuntimeSession): SessionHostKey {
    return sessionHostKey(
      workspaceId,
      workspaceHostKey,
      session.key,
      session.created_at,
    );
  }

  // Prefixes this view has claimed terminals under. Moving to another workspace
  // releases the previous claims into the app-level bounded retention cache.
  const ownedSessionPrefixes = new Set<string>();

  function releaseOwnedSessions(except?: string): void {
    for (const prefix of ownedSessionPrefixes) {
      if (prefix === except) continue;
      for (const session of mountedSessions()) {
        if (session.hostKey.startsWith(prefix)) noteSessionReleased(session.hostKey, except);
      }
      ownedSessionPrefixes.delete(prefix);
    }
  }

  // Sessions the terminal dock puts on screen: the leaves of its tree, and only
  // while the panel is open. A terminal-region session with no leaf has no
  // terminal today and must not gain one just because the pool could park it.
  const dockedSessionKeys = $derived(
    terminalLayout.open ? new Set(collectSessionKeys(dockTree)) : new Set<string>(),
  );

  // Mirror the sessions this workspace puts on screen into the app-level pool,
  // which owns the live terminals. Both regions render pooled slots, so a shell
  // dragged between the dock and the workflow area keeps its tmux attachment
  // and its scrollback instead of being torn down and reattached.
  //
  // Reconciled from state rather than pushed from each mount/unmount call site:
  // a session changes region without either side calling anything, and a missed
  // a missed release would leave a claimed socket attached to nothing.
  $effect(() => {
    const prefix = sessionHostPrefix(workspaceId, workspaceHostKey);
    const desired = new Map<SessionHostKey, MountedSession>();
    for (const session of runtimeSessions) {
      // A promoted session is on screen in a detail pane, which renders its slot
      // but cannot mount it: only this view knows the websocket path, the
      // generation, and whether actions are blocked.
      const onScreen =
        isPromoted(session) ||
        soleEmbeddedSession?.key === session.key ||
        (sessionRegion(session) === "workflow"
          ? mountedSessionKeys.includes(session.key)
          : dockedSessionKeys.has(session.key));
      if (!onScreen) continue;
      const hostKey = sessionHostKeyFor(session);
      desired.set(hostKey, {
        hostKey,
        websocketPath: workspaceSessionWebSocketPath(
          workspaceId,
          session.key,
          workspaceHostKey,
        ),
        status: session.status,
        cursorWheelInput: session.kind === "agent",
        disabled: actionsBlocked,
      });
    }
    // Absences are authoritative only after a server runtime read. During a
    // route switch `runtimeSessions` is empty, and launch replay can publish a
    // live snapshot containing only its confirmed session. Neither may tombstone
    // retained peers before hydration reconciles the complete session list.
    const liveGenerationKeys = runtimeLive && runtimeSnapshotAuthoritative
      ? new Set(runtimeSessions.map(sessionHostKeyFor))
      : null;
    untrack(() => {
      releaseOwnedSessions(prefix);
      if (desired.size > 0) ownedSessionPrefixes.add(prefix);
      for (const session of desired.values()) noteSessionMounted(session);
      // Only this workspace's entries. Another surface's claimed workspace keeps
      // its parked terminals until it stops being claimed.
      for (const session of mountedSessions()) {
        if (!session.hostKey.startsWith(prefix)) continue;
        if (desired.has(session.hostKey)) continue;
        if (liveGenerationKeys === null || liveGenerationKeys.has(session.hostKey)) {
          noteSessionReleased(session.hostKey, liveGenerationKeys === null ? prefix : undefined);
        } else {
          noteSessionDiscarded(session.hostKey);
        }
      }
    });
  });

  // A destroyed view releases its claims; the app-level pool applies the
  // configured retention limit and owns final disposal.
  $effect(() => () => untrack(() => releaseOwnedSessions()));

  // Pooled terminals report an exit by key; only this view can map that back to
  // a runtime session, and the generation in the key keeps a relaunched session
  // from being mistaken for the dead one.
  $effect(() =>
    onSessionExited((hostKey) => {
      const session = runtimeSessions.find(
        (candidate) => sessionHostKeyFor(candidate) === hostKey,
      );
      if (session) handleSessionExit(session);
    }),
  );

  function sessionGenerationMatches(
    closed: ClosedRuntimeSession,
    session: RuntimeSession,
  ): boolean {
    return (
      closed.workspaceId === session.workspace_id &&
      closed.key === session.key &&
      closed.createdAt === session.created_at
    );
  }

  function markSessionClosed(session: RuntimeSession): void {
    if (
      !closedSessions.some((closed) =>
        sessionGenerationMatches(closed, session),
      )
    ) {
      closedSessions = [
        ...closedSessions,
        {
          workspaceId: session.workspace_id,
          key: session.key,
          createdAt: session.created_at,
        },
      ];
    }
  }

  function clearClosedSession(session: RuntimeSession): void {
    closedSessions = closedSessions.filter(
      (closed) => !sessionGenerationMatches(closed, session),
    );
  }

  function isSessionTerminalMounted(
    sessionKey: string,
  ): boolean {
    return mountedSessionKeys.includes(sessionKey);
  }

  function defaultSessionRegion(session: RuntimeSession): SessionRegion {
    if (session.display_region === "workflow" || session.display_region === "terminal") {
      return session.display_region;
    }
    return session.target_key === PLAIN_SHELL_TARGET ? "terminal" : "workflow";
  }

  function isActiveRuntimeSession(session: RuntimeSession): boolean {
    return session.status === "running" || session.status === "starting";
  }

  function sessionRegion(session: RuntimeSession): SessionRegion {
    return terminalLayout.sessionRegions[session.key] ?? defaultSessionRegion(session);
  }

  function workflowTabKeyForSession(sessionKey: string): WorkflowTabKey {
    return `session:${sessionKey}`;
  }

  function sessionKeyFromWorkflowTab(tabKey: WorkflowTabKey): string | null {
    return tabKey.startsWith("session:") ? tabKey.slice("session:".length) : null;
  }

  function workspaceStorageId(
    id: string,
    hostKey: string | undefined,
  ): string {
    return hostKey ? `fleet:${encodeURIComponent(hostKey)}:${id}` : id;
  }

  function terminalLayoutStorageKey(storageId: string): string {
    return `${TERMINAL_LAYOUT_KEY_PREFIX}${storageId}`;
  }

  function loadTerminalLayout(storageId: string): TerminalLayoutState {
    return parseTerminalLayout(
      readLocalStorage(terminalLayoutStorageKey(storageId)),
    );
  }

  function loadWorkflowPresets(): WorkflowPreset[] {
    const raw = readLocalStorage(WORKFLOW_PRESETS_KEY);
    if (!raw) return [];
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (!Array.isArray(parsed)) return [];
      return parsed.flatMap((item) => {
        const preset = parseWorkflowPreset(item);
        return preset ? [preset] : [];
      });
    } catch {
      return [];
    }
  }

  function parseWorkflowPreset(value: unknown): WorkflowPreset | null {
    if (value === null || typeof value !== "object" || Array.isArray(value)) {
      return null;
    }
    const record = value as Record<string, unknown>;
    if (
      typeof record.id !== "string" ||
      typeof record.name !== "string" ||
      typeof record.createdAt !== "string" ||
      typeof record.updatedAt !== "string" ||
      !Array.isArray(record.sessions)
    ) {
      return null;
    }
    const sessions = record.sessions.flatMap((item) => {
      if (item === null || typeof item !== "object" || Array.isArray(item)) {
        return [];
      }
      const session = item as Record<string, unknown>;
      if (
        typeof session.sourceKey !== "string" ||
        typeof session.targetKey !== "string" ||
        typeof session.label !== "string" ||
        (session.region !== "workflow" && session.region !== "terminal")
      ) {
        return [];
      }
      return [
        {
          sourceKey: session.sourceKey,
          targetKey: session.targetKey,
          label: session.label,
          region: session.region,
        } satisfies WorkflowPresetSession,
      ];
    });
    return {
      id: record.id,
      name: record.name,
      createdAt: record.createdAt,
      updatedAt: record.updatedAt,
      sessions,
      layout: parseTerminalLayout(JSON.stringify(record.layout)),
    };
  }

  function terminalSessionKeysFrom(
    sessions: RuntimeSession[],
    layout: TerminalLayoutState = terminalLayout,
  ): string[] {
    return sessions
      .filter(
        (session) =>
          (layout.sessionRegions[session.key] ?? defaultSessionRegion(session)) ===
          "terminal",
      )
      .map((session) => session.key);
  }

  function workflowTabKeysFrom(
    sessions: RuntimeSession[],
    layout: TerminalLayoutState = terminalLayout,
  ): WorkflowTabKey[] {
    const keys: WorkflowTabKey[] = ["home"];
    if (
      layout.dock === "top" &&
      (layout.open || terminalSessionKeysFrom(sessions, layout).length > 0)
    ) {
      keys.push("terminal");
    }
    for (const session of sessions) {
      const region =
        layout.sessionRegions[session.key] ?? defaultSessionRegion(session);
      if (region === "workflow") {
        keys.push(workflowTabKeyForSession(session.key));
      }
    }
    return keys;
  }

  function normalizeLayoutForSessions(
    sessions: RuntimeSession[],
    base: TerminalLayoutState = terminalLayout,
    activeWorkflowTab: WorkflowTabKey = activeTabKey,
  ): TerminalLayoutState {
    const allKeys = sessions.map((session) => session.key);
    let next = normalizeTerminalLayout(base, allKeys);
    const sessionRegions = { ...next.sessionRegions };
    for (const session of sessions) {
      sessionRegions[session.key] =
        sessionRegions[session.key] ?? defaultSessionRegion(session);
    }
    next = { ...next, sessionRegions };
    const terminalKeys = terminalSessionKeysFrom(sessions, next);
    let terminalGroups = next.terminalGroups.filter((group) =>
      collectTerminalGroupKeys(group).some((key) => terminalKeys.includes(key)),
    );
    const groupedKeys = terminalGroups.flatMap((group) =>
      collectTerminalGroupKeys(group),
    );
    for (const key of terminalKeys) {
      if (!groupedKeys.includes(key)) {
        terminalGroups = [...terminalGroups, createTerminalGroup(key)];
        groupedKeys.push(key);
      }
    }
    const activeGroup =
      (next.activeTerminalGroupID
        ? terminalGroups.find((group) => group.id === next.activeTerminalGroupID)
        : null) ??
      (next.activeSessionKey
        ? terminalGroups.find((group) =>
            collectTerminalGroupKeys(group).includes(next.activeSessionKey!),
          )
        : null) ??
      terminalGroups[0] ??
      null;
    const activeSessionKey =
      activeGroup?.activeSessionKey ??
      firstLeaf(activeGroup?.tree ?? null)?.sessionKey ??
      null;
    const normalized = {
      ...next,
      activeSessionKey,
      tree: activeGroup?.tree ?? null,
      terminalGroups,
      activeTerminalGroupID: activeGroup?.id ?? null,
    };
    const workflowTree = activateWorkflowTab(
      normalizeWorkflowTree(
        normalized.workflowTree,
        workflowTabKeysFrom(sessions, normalized),
      ),
      activeWorkflowTab,
    );
    return {
      ...normalized,
      workflowTree,
    };
  }

  function collectTerminalGroupKeys(group: TerminalGroup): string[] {
    return group.tree ? collectSessionKeys(group.tree) : [];
  }

  function layoutWithTerminalGroups(
    base: TerminalLayoutState,
    groups: TerminalGroup[],
    activeGroupID: string | null,
  ): TerminalLayoutState {
    const activeGroup =
      (activeGroupID
        ? groups.find((group) => group.id === activeGroupID)
        : null) ??
      groups[0] ??
      null;
    return {
      ...base,
      terminalGroups: groups,
      activeTerminalGroupID: activeGroup?.id ?? null,
      activeSessionKey: activeGroup?.activeSessionKey ?? null,
      tree: activeGroup?.tree ?? null,
    };
  }

  function rememberActiveTab(key: WorkflowTabKey): void {
    if (!workspaceId) return;
    writeLocalStorage(
      `${ACTIVE_WORKSPACE_TAB_KEY_PREFIX}${workspaceStorageId(workspaceId, workspaceHostKey)}`,
      key,
    );
  }

  function selectWorkspaceTab(key: WorkflowTabKey): void {
    if (terminalLayout.workflowMode === "grid") {
      terminalLayout = { ...terminalLayout, workflowMode: "tabs" };
    }
    terminalLayout = {
      ...terminalLayout,
      workflowTree: activateWorkflowTab(terminalLayout.workflowTree, key),
    };
    activeTabKey = key;
    rememberActiveTab(key);
  }

  function handleWorkflowTabActivation(key: WorkflowTabKey): void {
    if (key === "terminal") {
      terminalLayout = { ...terminalLayout, open: true };
    }
    if (key === activeTabKey) return;
    const sessionKey = sessionKeyFromWorkflowTab(key);
    if (sessionKey) mountSessionTerminal(sessionKey);
    selectWorkspaceTab(key);
  }

  function handleWorkflowPaneFocus(key: WorkflowTabKey): void {
    focusedWorkflowTabKey = key;
    lastWorkflowInputContentKey = workflowContentKeyFor(key);
    handleWorkflowTabActivation(key);
  }

  function restoreWorkspaceTabSelection(key: WorkflowTabKey): void {
    activeTabKey = key;
    rememberActiveTab(key);
  }

  function restoreWorkspaceTab(storageId: string): WorkflowTabKey {
    const remembered = readLocalStorage(
      `${ACTIVE_WORKSPACE_TAB_KEY_PREFIX}${storageId}`,
    );
    if (remembered === "diff") return "home";
    if (
      remembered === "home" ||
      remembered === "terminal" ||
      remembered?.startsWith("session:")
    ) {
      return remembered as WorkflowTabKey;
    }
    return "home";
  }

  function defaultSidebarTab(ws: Workspace): SidebarTab {
    return defaultWorkspaceSidebarTab(settingsStore.getWorkspaceSettings().default_sidebar_view, ws.item_type);
  }

  function isSidebarTabSupported(
    ws: Workspace,
    tab: SidebarTab,
  ): boolean {
    if (tab === "diff") return true;
    if (tab === "issue") {
      return ws.item_type === "issue";
    }
    if (tab === "kata") return workspaceHostKey === undefined;
    if (tab === "reviews") {
      return ws.item_type === "pull_request";
    }
    return getWorkspacePRNumber(ws) !== null;
  }

  function syncSidebarTabForWorkspace(ws: Workspace): void {
    if (!isSidebarTabSupported(ws, sidebarTab)) {
      setSidebarTab(defaultSidebarTab(ws));
    }
  }

  function getWorkspacePRNumber(ws: Workspace): number | null {
    if (ws.item_type === "pull_request") return ws.item_number;
    return ws.associated_pr_number ?? null;
  }

  function terminalRoute(id: string): string {
    if (!workspaceHostKey) return `/terminal/${encodeURIComponent(id)}`;
    return `/terminal/fleet/${encodeURIComponent(workspaceHostKey)}/${encodeURIComponent(id)}`;
  }

  function isCurrentWorkspace(id: string, hostKey: string | undefined): boolean {
    return id === workspaceId && hostKey === workspaceHostKey;
  }

  function selectedWorkspaceHostKey(ws: Workspace): string | undefined {
    return ws.fleet_host_key;
  }

  // A 404 on the workspace fetch is authoritative: the workspace no longer
  // exists (deleted by another client). Snapshot the identity from the
  // cached envelope BEFORE dropping it — the deletion callback needs it to
  // tombstone controller-less cached detail — then clear the cached
  // envelope so liveness rendering shows the error state instead of
  // continuing to display the deleted workspace.
  function handleWorkspaceGone(id: string, hostKey: string | undefined): void {
    onWorkspaceDeleted?.(id, hostKey, workspaceIdentitySnapshot(id));
    if (workspace?.id === id) {
      workspace = null;
      stopPolling();
      stopRuntimePolling();
    }
  }

  function workspaceReadFailureMessage(
    failure: ApiProblemError | InvalidExternalPayload | TransientTransportError,
  ): string {
    if (failure._tag === "ApiProblemError") {
      const fallback =
        failure.problem.status === undefined
          ? "Failed to load workspace"
          : `Failed to load workspace (${failure.problem.status})`;
      return apiErrorMessage(failure.problem, fallback);
    }
    return failure.cause instanceof Error ? failure.cause.message : "Network error";
  }

  function fetchWorkspaceProgram(
    id = workspaceId,
    hostKey = workspaceHostKey,
  ): Effect.Effect<Workspace | null, never, AppServices> {
    // Capture the id at call time. With workspaceId changing across
    // navigations, a slow in-flight fetch for the previous id could
    // otherwise resolve after a newer fetch and overwrite the new
    // workspace's data with stale content (causing a perceived flash
    // back to the previous workspace).
    return Effect.gen(function* () {
      recordWorkspaceSwitchPhase("workspace-request-start", id, hostKey);
      const data = hostKey
        ? yield* executeOpaqueGeneratedApiRequest("load fleet workspace", (generatedClient, signal) =>
            generatedClient.FleetService.getFleetWorkspace({ hostKey, id }, { signal }),
          )
        : yield* executeGeneratedApiRequest("load workspace", (generatedClient, signal) =>
            generatedClient.WorkspacesService.getWorkspace({ id }, { signal }),
          );
      const nextWorkspace = yield* decodeWorkspaceDetail(data, hostKey);
      yield* Effect.sync(() => {
        recordWorkspaceSwitchPhase("workspace-request-end", id, hostKey, {
          status: 200,
        });
        if (!isCurrentWorkspace(id, hostKey)) return;
        workspace = nextWorkspace;
        syncSidebarTabForWorkspace(nextWorkspace);
        loadError = null;

        if (nextWorkspace.status !== "creating") {
          stopPolling();
        }
        if (nextWorkspace.status === "ready") {
          startRuntimePolling();
          if (!hasAppliedRuntimeFor(id, hostKey)) {
            requestRuntime();
          }
        } else {
          stopRuntimePolling();
        }
      });
      return nextWorkspace;
    }).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          recordWorkspaceSwitchPhase("workspace-request-end", id, hostKey, { error: true });
          if (!isCurrentWorkspace(id, hostKey)) return null;
          if (failure._tag === "ApiProblemError" && failure.problem.status === 404) {
            handleWorkspaceGone(id, hostKey);
          }
          loadError = workspaceReadFailureMessage(failure);
          return null;
        }),
      ),
    );
  }

  function requestWorkspace(): void {
    appRuntime.runCommand(fetchWorkspaceProgram().pipe(Effect.asVoid), {
      operation: "workspace.details.read",
      safeContext: { surface: "workspace" },
      onFailure: () => undefined,
    });
  }

  interface FetchRuntimeOptions {
    force?: boolean;
  }

  function fetchRuntimeProgram(
    options: FetchRuntimeOptions = {},
  ) {
    if (!workspaceId) return Effect.succeed<WorkspaceRuntimeState | null>(null);
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    return Effect.gen(function* () {
      recordWorkspaceSwitchPhase("runtime-request-start", id, hostKey);
      const workflow = yield* WorkspaceRuntimeWorkflow;
      const result = yield* workflow.read(runtimeOwner, id, hostKey, options);
      if (Option.isNone(result)) return null;
      const data = result.value;
      return yield* Effect.sync(() => {
        recordWorkspaceSwitchPhase("runtime-request-end", id, hostKey, {
          sessions: data.sessions.length,
        });
        if (!isCurrentWorkspace(id, hostKey)) return null;
        const fingerprint = JSON.stringify(data);
        const acceptedLaunch = pendingWorkspaceLaunch(id, hostKey);
        if (
          acceptedLaunch?.phase === "awaiting_session" &&
          acceptedLaunch.sessionKey !== undefined &&
          data.sessions.some((session) => session.key === acceptedLaunch.sessionKey)
        ) {
          completeAcceptedWorkspaceLaunch(id, hostKey, acceptedLaunch.sessionKey);
        }
        runtimeSnapshotAuthoritative = true;
        if (
          hasAppliedRuntimeFor(id, hostKey) &&
          appliedRuntimeState?.fingerprint === fingerprint
        ) {
          runtimeError = null;
          return data;
        }
        runtime = data;
        runtimeForId = id;
        runtimeForHostKey = hostKey;
        appliedRuntimeState = { workspaceId: id, hostKey, fingerprint };
        runtimeError = null;
        terminalLayout = normalizeLayoutForSessions(data.sessions);
        if (
          activeTabKey.startsWith("session:") &&
          !data.sessions.some(
            (session) =>
              session.key === activeTabKey.slice("session:".length) &&
              sessionRegion(session) === "workflow",
          )
        ) {
          selectFallbackTab();
        }
        mountedSessionKeys = mountedSessionKeys.filter(
          (key) => data.sessions.some((session) => session.key === key),
        );
        return data;
      });
    }).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          recordWorkspaceSwitchPhase("runtime-request-end", id, hostKey, {
            error: true,
          });
          if (!isCurrentWorkspace(id, hostKey)) return null;
          runtimeError = workspaceMutationFailureMessage(
            failure,
            "Runtime load failed",
          );
          return null;
        }),
      ),
    );
  }

  function requestRuntime(options: FetchRuntimeOptions = {}): void {
    appRuntime.runCommand(fetchRuntimeProgram(options), {
      operation: "workspace.runtime.read",
      safeContext: { surface: "workspace" },
      onFailure: () => undefined,
    });
  }

  function runtimeMutationTargetKey(target: WorkspaceRuntimeTarget): string {
    return JSON.stringify([target.hostKey ?? null, target.workspaceId]);
  }

  function clearRetainedDeletePresentation(target: WorkspaceRuntimeTarget): void {
    const key = runtimeMutationTargetKey(target);
    deleteTriggerElements.delete(key);
    retainedRuntimePresenterLeases = retainedRuntimePresenterLeases.filter(
      (lease) => runtimeMutationTargetKey(lease.target) !== key,
    );
  }

  function clearRuntimeMutationPending(state: WorkspaceRuntimeMutationState): void {
    switch (state.operation) {
      case "Launch":
        if (state.request.placement._tag === "Workflow") {
          if (launchingKey === state.request.targetKey) launchingKey = null;
        } else {
          terminalLaunching = false;
        }
        return;
      case "Stop":
        if (stopPromptSession?.key === state.request.sessionKey) stopPromptSession = null;
        stopSessionStopping = false;
        return;
      case "Rename":
        renameSaving = false;
        return;
      case "ApplyPreset": {
        const index = applyingWorkflowPresetFor.indexOf(viewWorkspaceKey);
        if (index !== -1) applyingWorkflowPresetFor = applyingWorkflowPresetFor.toSpliced(index, 1);
        return;
      }
      case "Refresh":
        refreshingWorkspace = false;
        return;
      case "RetrySetup":
        retryingSetup = false;
        return;
      case "Delete":
        removeDeletingWorkspaceTarget(
          state.request.target.workspaceId,
          state.request.target.hostKey,
        );
        if (state.request.options.force) forceDeleting = false;
        if (state.kind !== "uncertain") {
          clearRetainedDeletePresentation(state.request.target);
        }
    }
  }

  function presentRuntimeMutation(
    state: WorkspaceRuntimeMutationState,
    presenterID: string,
    failurePresentationIsCurrent: () => boolean = () => true,
  ): Effect.Effect<boolean, never, AppServices> {
    const { workspaceId: id, hostKey } = state.request.target;
    const current = isCurrentWorkspace(id, hostKey);
    if (!current && state.operation !== "Delete") return Effect.succeed(false);
    if (state.kind === "pending") {
      return Effect.sync(() => {
        switch (state.operation) {
          case "Launch":
            if (state.request.placement._tag === "Workflow") launchingKey = state.request.targetKey;
            else terminalLaunching = true;
            break;
          case "Stop":
            stopSessionStopping = true;
            break;
          case "Rename":
            renameSaving = true;
            break;
          case "ApplyPreset":
            if (!applyingWorkflowPresetFor.includes(viewWorkspaceKey)) {
              applyingWorkflowPresetFor = [...applyingWorkflowPresetFor, viewWorkspaceKey];
            }
            break;
          case "Refresh":
            refreshingWorkspace = true;
            break;
          case "RetrySetup":
            retryingSetup = true;
            break;
          case "Delete":
            addDeletingWorkspaceTarget(id, hostKey);
            if (state.request.options.force) forceDeleting = true;
            break;
        }
        return false;
      });
    }
    if (state.kind === "failed" || state.kind === "uncertain") {
      return Effect.sync(() => {
        if (!current && state.operation !== "Delete") return false;
        if (
          state.operation === "Delete" &&
          state.kind === "failed" &&
          (state.request.options.presenterID !== presenterID || !failurePresentationIsCurrent())
        ) {
          clearRuntimeMutationPending(state);
          return true;
        }
        const fallback =
          state.operation === "Launch"
            ? state.request.region === "terminal" ? "Terminal launch failed" : "Launch failed"
            : state.operation === "Stop"
              ? "Stop failed"
              : state.operation === "Rename"
                ? "Rename failed"
                : state.operation === "ApplyPreset"
                  ? "Preset launch failed"
                  : state.operation === "Refresh"
                    ? "Refresh failed"
                    : state.operation === "RetrySetup"
                      ? "Retry failed"
                      : "Delete failed";
        if (current) {
          showFlash(workspaceMutationFailureMessage(state.error, fallback), { tone: "danger" });
        }
        clearRuntimeMutationPending(state);
        return true;
      });
    }

    switch (state.operation) {
      case "Launch": {
        if (state.request.placement._tag === "TerminalSplit") {
          const placement = state.request.placement;
          return Effect.sync(() => {
            const session = state.session;
            clearClosedSession(session);
            const group = terminalLayout.terminalGroups.find((candidate) => candidate.id === placement.groupID);
            const groups = group === undefined
              ? [
                  ...terminalLayout.terminalGroups,
                  createTerminalGroup(session.key, placement.groupID),
                ]
              : updateTerminalGroupTree(terminalLayout.terminalGroups, placement.groupID, (candidate) => ({
                  ...candidate,
                  activeSessionKey: session.key,
                  tree: splitPane(candidate.tree, placement.targetLeafID ?? null, session.key, placement.direction),
                }));
            terminalLayout = normalizeLayoutForSessions(
              upsertRuntimeSession(session),
              layoutWithTerminalGroups(
                {
                  ...terminalLayout,
                  open: true,
                  sessionRegions: { ...terminalLayout.sessionRegions, [session.key]: "terminal" },
                },
                groups,
                placement.groupID,
              ),
            );
            if (terminalLayout.dock === "top") selectWorkspaceTab("terminal");
            clearRuntimeMutationPending(state);
            return true;
          });
        }
        if (state.request.placement._tag === "Workflow") {
          // A successful launch response names the session the server already
          // recorded. Publish it now; the runtime reload only reconciles peers.
          return Effect.sync(() => {
            if (!isCurrentWorkspace(id, hostKey)) return false;
            const session = state.session;
            clearClosedSession(session);
            applySessionToWorkflow(session.key, upsertRuntimeSession(session));
            closeLauncher();
            requestSessionFocus(sessionHostKeyFor(session));
            clearRuntimeMutationPending(state);
            requestRuntime({ force: true });
            return true;
          });
        }
        const placement = state.request.placement;
        return Effect.gen(function* () {
          yield* fetchRuntimeProgram({ force: true });
          if (!isCurrentWorkspace(id, hostKey)) return false;
          yield* Effect.sync(() => {
            const session = state.session;
            clearClosedSession(session);
            if (placement.insertIntoTree) {
              const sessionsWithLaunch = upsertRuntimeSession(session);
              const groups = addTerminalGroup(terminalLayout.terminalGroups, session.key);
              const activeGroupID = groups.at(-1)?.id ?? terminalLayout.activeTerminalGroupID;
              terminalLayout = normalizeLayoutForSessions(
                sessionsWithLaunch,
                layoutWithTerminalGroups(
                  {
                    ...terminalLayout,
                    open: true,
                    sessionRegions: { ...terminalLayout.sessionRegions, [session.key]: "terminal" },
                  },
                  groups,
                  activeGroupID,
                ),
              );
            }
            if (terminalLayout.dock === "top") selectWorkspaceTab("terminal");
            clearRuntimeMutationPending(state);
          });
          return true;
        });
      }
      case "Stop":
        return Effect.gen(function* () {
          const stoppedSession = runtimeSessions.find((session) => session.key === state.request.sessionKey);
          yield* Effect.sync(() => {
            if (stoppedSession !== undefined) markSessionClosed(stoppedSession);
            unmountSessionTerminal(state.request.sessionKey);
            const groups = closeSessionInTerminalGroups(terminalLayout.terminalGroups, state.request.sessionKey);
            terminalLayout = normalizeLayoutForSessions(
              runtimeSessions,
              layoutWithTerminalGroups(terminalLayout, groups, terminalLayout.activeTerminalGroupID),
            );
            if (activeTabKey === `session:${state.request.sessionKey}`) selectFallbackTab();
            clearRuntimeMutationPending(state);
          });
          yield* fetchRuntimeProgram({ force: true });
          if (!isCurrentWorkspace(id, hostKey)) return false;
          return true;
        });
      case "Rename":
        return Effect.sync(() => {
          if (runtime) invalidateRuntimeSnapshot();
          runtime = runtime
            ? {
                ...runtime,
                sessions: runtime.sessions.map((session) =>
                  session.key === state.request.sessionKey ? state.session : session,
                ),
              }
            : runtime;
          renamePrompt = null;
          renameInputValue = "";
          clearRuntimeMutationPending(state);
          return true;
        });
      case "ApplyPreset":
        return Effect.gen(function* () {
          const mappedLayout = mapPresetLayout(state.request.preset.layout, state.keyMap);
          const refreshed = yield* fetchRuntimeProgram({ force: true });
          if (!isCurrentWorkspace(id, hostKey) || !refreshed) return false;
          yield* Effect.sync(() => {
            const presetActiveTab = firstWorkflowTab(mappedLayout) ?? "home";
            terminalLayout = normalizeLayoutForSessions(refreshed.sessions, mappedLayout, presetActiveTab);
            mountedSessionKeys = refreshed.sessions
              .filter((session) => sessionRegionForLayout(session, terminalLayout) === "workflow")
              .map((session) => session.key);
            selectedWorkflowPresetId = state.request.preset.id;
            selectWorkspaceTab(firstWorkflowTab(terminalLayout) ?? "home");
            clearRuntimeMutationPending(state);
          });
          return true;
        });
      case "Refresh":
        return Effect.sync(() => {
          if (!isCurrentWorkspace(id, hostKey) || state.workspace.id !== id) return false;
          workspace = state.workspace;
          syncSidebarTabForWorkspace(state.workspace);
          sidebarRefreshToken += 1;
          if (state.workspace.status === "ready") requestRuntime();
          clearRuntimeMutationPending(state);
          return true;
        });
      case "RetrySetup":
        return Effect.gen(function* () {
          if (!isCurrentWorkspace(id, hostKey) || state.workspace.id !== id) return false;
          yield* Effect.sync(() => {
            workspace = state.workspace;
            if (state.workspace.status === "creating") startPolling();
            clearRuntimeMutationPending(state);
          });
          if (state.workspace.status === "creating") yield* fetchWorkspaceProgram(id, hostKey);
          return true;
        });
      case "Delete":
        return Effect.sync(() => {
          const { error, response } = state.result;
          const force = state.request.options.force;
          const responseFailed = force
            ? !response.ok && response.status !== 204
            : response.status === 409 || (!response.ok && response.status !== 204);
          if (
            responseFailed &&
            (state.request.options.presenterID !== presenterID || !failurePresentationIsCurrent())
          ) {
            clearRuntimeMutationPending(state);
            return true;
          }
          if (!responseFailed) {
            onWorkspaceDeleted?.(id, hostKey, state.request.options.identity);
          }
          if (!isCurrentWorkspace(id, hostKey)) {
            clearRuntimeMutationPending(state);
            return true;
          }
          if (!force && response.status === 409) {
            if (error?.code === ProblemCodes.worktreeDirty) {
              previouslyFocusedEl = deleteTriggerElements.get(runtimeMutationTargetKey(state.request.target)) ?? null;
              forcePromptForId = id;
              forcePromptHostKey = hostKey;
              forcePromptIdentity = state.request.options.identity;
              forcePromptMessage = apiErrorMessage(error, "Workspace has uncommitted changes.");
            } else if (
              error?.code === ProblemCodes.workspaceSetupInProgress ||
              error?.code === ProblemCodes.workspaceDeletionInProgress
            ) {
              requestWorkspace();
            } else {
              showFlash(apiErrorMessage(error, "Workspace deletion could not start."), {
                tone: "danger",
              });
            }
            clearRuntimeMutationPending(state);
            return true;
          }
          if (responseFailed) {
            showFlash(apiErrorMessage(error, `Delete failed (${response.status})`), { tone: "danger" });
          } else if (isCurrentTerminalRoute(id)) {
            navigate("/workspaces");
          }
          if (force) {
            forcePromptMessage = null;
            forcePromptForId = null;
            forcePromptHostKey = undefined;
            forcePromptIdentity = undefined;
          }
          clearRuntimeMutationPending(state);
          return true;
        });
    }
  }

  function handleLaunch(
    targetKey: string,
    launchClaim?: WorkspaceLaunchClaim,
  ): void {
    if (!workspaceId || launchingKey || actionsBlocked) return;
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    launchingKey = targetKey;
    appRuntime.runCommand(
      launchRuntimeSessionProgram(id, hostKey, targetKey, "workflow", {
        _tag: "Workflow",
        ...(launchClaim === undefined
          ? {}
          : {
              onSettled: (settlement) => {
                if (settlement._tag === "Accepted") {
                  const acceptedAt = Date.now();
                  if (acceptWorkspaceLaunch(launchClaim, settlement.sessionKey, acceptedAt)) {
                    const label = launchTargets.find((target) => target.key === launchClaim.targetKey)?.label
                      ?? launchClaim.targetKey;
                    startAcceptedWorkspaceLaunchReconciliation(
                      launchClaim.workspaceId,
                      launchClaim.workspaceHostKey,
                      settlement.sessionKey,
                      acceptedAt,
                      label,
                    );
                  }
                } else {
                  failWorkspaceLaunch(launchClaim);
                }
              },
            }),
      }),
      {
        operation: "workspace.session.launch",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          if (isCurrentWorkspace(id, hostKey)) launchingKey = null;
          if (launchClaim !== undefined) failWorkspaceLaunch(launchClaim);
        },
      },
    );
  }

  function startAcceptedWorkspaceLaunchReconciliation(
    acceptedWorkspaceId: string,
    acceptedWorkspaceHostKey: string | undefined,
    acceptedSessionKey: string,
    acceptedAt: number,
    label: string,
  ): void {
    const program = Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      yield* workflow.reconcileAcceptedLaunch({
        target: runtimeTarget(acceptedWorkspaceId, acceptedWorkspaceHostKey),
        sessionKey: acceptedSessionKey,
        acceptedAt,
        onExpired: Effect.sync(() => {
          showFlash(`${label} launched, but its session did not become available`, { tone: "danger" });
        }),
      });
    });
    appRuntime.runCommand(program, {
      operation: "reconcile accepted workspace launch",
      safeContext: { surface: "workspace" },
      onFailure: () => undefined,
    });
  }

  function openSession(sessionKey: string): void {
    const session = runtimeSessions.find((s) => s.key === sessionKey);
    if (session && sessionRegion(session) === "terminal") {
      selectTerminalSession(sessionKey);
      return;
    }
    mountSessionTerminal(sessionKey);
    selectWorkspaceTab(workflowTabKeyForSession(sessionKey));
  }

  function closeSession(session: RuntimeSession): void {
    if (actionsBlocked) return;
    if (session.status === "running") {
      const triggerEl =
        document.activeElement instanceof HTMLElement
          ? document.activeElement
          : null;
      previouslyFocusedEl = triggerEl;
      stopPromptSession = session;
      return;
    }
    stopSession(session);
  }

  function confirmStopSession(): void {
    if (stopSessionStopping || stopPromptSession === null) return;
    stopSessionStopping = true;
    const session = stopPromptSession;
    stopSession(session);
  }

  function cancelStopSession(): void {
    if (stopSessionStopping) return;
    stopPromptSession = null;
  }

  function stopSession(session: RuntimeSession): void {
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    appRuntime.runCommand(
      stopRuntimeSessionProgram(id, hostKey, session.key),
      {
        operation: "workspace.session.stop",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          stopSessionStopping = false;
        },
      },
    );
  }

  function handleSessionExit(session: RuntimeSession): void {
    if (session.workspace_id !== workspaceId) return;
    markSessionClosed(session);
    unmountSessionTerminal(session.key);
    const terminalGroups = closeSessionInTerminalGroups(
      terminalLayout.terminalGroups,
      session.key,
    );
    terminalLayout = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithTerminalGroups(
        terminalLayout,
        terminalGroups,
        terminalLayout.activeTerminalGroupID,
      ),
    );
    if (activeTabKey === `session:${session.key}`) {
      selectFallbackTab();
    }
    requestRuntime({ force: true });
  }

  function launchTerminalSession(
    placement: WorkspaceRuntimeLaunchPlacement = { _tag: "Terminal", insertIntoTree: true },
  ): void {
    if (!workspaceId || terminalLaunching || actionsBlocked) return;
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    terminalLaunching = true;
    appRuntime.runCommand(
      launchRuntimeSessionProgram(id, hostKey, PLAIN_SHELL_TARGET, "terminal", placement),
      {
        operation: "workspace.terminal.launch",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          if (isCurrentWorkspace(id, hostKey)) terminalLaunching = false;
        },
      },
    );
  }

  function toggleTerminalPanel(): void {
    if (actionsBlocked) return;
    if (terminalLayout.open) {
      terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
        ...terminalLayout,
        open: false,
      });
      if (activeTabKey === "terminal") {
        selectFallbackTab();
      }
      return;
    }
    terminalLayout = { ...terminalLayout, open: true };
    if (!terminalSessions.some(isActiveRuntimeSession)) {
      launchTerminalSession();
    } else if (terminalLayout.dock === "top") {
      selectWorkspaceTab("terminal");
    }
  }

  function selectTerminalSession(sessionKey: string): void {
    if (actionsBlocked) return;
    const group = terminalGroupForSession(terminalLayout.terminalGroups, sessionKey);
    const groups = terminalLayout.terminalGroups.map((candidate) =>
      candidate.id === group?.id
        ? { ...candidate, activeSessionKey: sessionKey }
        : candidate,
    );
    terminalLayout = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithTerminalGroups(
        {
          ...terminalLayout,
          open: true,
        },
        groups,
        group?.id ?? terminalLayout.activeTerminalGroupID,
      ),
    );
    if (terminalLayout.dock === "top") {
      selectWorkspaceTab("terminal");
    }
  }

  function selectTerminalGroup(groupID: string): void {
    if (actionsBlocked) return;
    terminalLayout = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithTerminalGroups(
        { ...terminalLayout, open: true },
        terminalLayout.terminalGroups,
        groupID,
      ),
    );
    if (terminalLayout.dock === "top") {
      selectWorkspaceTab("terminal");
    }
  }

  function moveSessionToTerminal(sessionKey: string): void {
    if (actionsBlocked) return;
    const session = runtimeSessions.find((s) => s.key === sessionKey);
    if (!session) return;
    if (isPromoted(session)) surfaceLayout?.demoteTab(sessionPaneKeyFor(session));
    const groups = addTerminalGroup(terminalLayout.terminalGroups, sessionKey);
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...layoutWithTerminalGroups(
        {
          ...terminalLayout,
          open: true,
          sessionRegions: {
            ...terminalLayout.sessionRegions,
            [sessionKey]: "terminal",
          },
        },
        groups,
        terminalGroupForSession(groups, sessionKey)?.id ?? null,
      ),
    });
    if (terminalLayout.dock === "top") {
      selectWorkspaceTab("terminal");
    } else if (activeTabKey === `session:${sessionKey}`) {
      selectFallbackTab();
    }
  }

  function moveSessionToWorkflow(
    sessionKey: string,
    sessions: RuntimeSession[] = runtimeSessions,
  ): void {
    if (actionsBlocked) return;
    applySessionToWorkflow(sessionKey, sessions);
  }

  function applySessionToWorkflow(sessionKey: string, sessions: RuntimeSession[]): void {
    const session = sessions.find((candidate) => candidate.key === sessionKey);
    if (!session) return;
    if (isPromoted(session)) surfaceLayout?.demoteTab(sessionPaneKeyFor(session));
    const terminalGroups = closeSessionInTerminalGroups(
      terminalLayout.terminalGroups,
      sessionKey,
    );
    terminalLayout = normalizeLayoutForSessions(sessions, {
      ...layoutWithTerminalGroups(
        {
          ...terminalLayout,
          workflowMode: "tabs",
        },
        terminalGroups,
        terminalLayout.activeTerminalGroupID,
      ),
      sessionRegions: {
        ...terminalLayout.sessionRegions,
        [sessionKey]: "workflow",
      },
    });
    mountSessionTerminal(sessionKey);
    selectWorkspaceTab(workflowTabKeyForSession(sessionKey));
  }

  function layoutWithWorkflowTab(
    tabKey: WorkflowTabKey,
    base: TerminalLayoutState,
  ): TerminalLayoutState {
    if (tabKey === "terminal") {
      return { ...base, open: true, dock: "top" };
    }
    const sessionKey = sessionKeyFromWorkflowTab(tabKey);
    if (sessionKey === null) return base;
    mountSessionTerminal(sessionKey);
    const terminalGroups = closeSessionInTerminalGroups(
      base.terminalGroups,
      sessionKey,
    );
    return {
      ...layoutWithTerminalGroups(
        {
          ...base,
          workflowMode: "tabs",
        },
        terminalGroups,
        base.activeTerminalGroupID,
      ),
      sessionRegions: {
        ...base.sessionRegions,
        [sessionKey]: "workflow",
      },
    };
  }

  function moveWorkflowTabBeforeTarget(
    sourceTabKey: WorkflowTabKey,
    targetTabKey: WorkflowTabKey,
  ): void {
    if (actionsBlocked) return;
    demoteWorkflowTab(sourceTabKey);
    if (sourceTabKey === targetTabKey) return;
    const prepared = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithWorkflowTab(sourceTabKey, terminalLayout),
    );
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...prepared,
      workflowTree: moveWorkflowTabBefore(
        prepared.workflowTree,
        sourceTabKey,
        targetTabKey,
      ),
    });
    selectWorkspaceTab(sourceTabKey);
  }

  function appendWorkflowTabToGroup(
    sourceTabKey: WorkflowTabKey,
    leafID: string,
  ): void {
    if (actionsBlocked) return;
    demoteWorkflowTab(sourceTabKey);
    const prepared = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithWorkflowTab(sourceTabKey, terminalLayout),
    );
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...prepared,
      workflowTree: appendWorkflowTabToLeaf(
        prepared.workflowTree,
        sourceTabKey,
        leafID,
      ),
    });
    selectWorkspaceTab(sourceTabKey);
  }

  function splitWorkflowTabIntoGroup(
    sourceTabKey: WorkflowTabKey,
    leafID: string,
    direction: SplitDirection,
    placement: "before" | "after",
  ): void {
    if (actionsBlocked) return;
    demoteWorkflowTab(sourceTabKey);
    const prepared = normalizeLayoutForSessions(
      runtimeSessions,
      layoutWithWorkflowTab(sourceTabKey, terminalLayout),
    );
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...prepared,
      workflowTree: splitWorkflowTabIntoLeaf(
        prepared.workflowTree,
        sourceTabKey,
        leafID,
        direction,
        placement,
      ),
    });
    selectWorkspaceTab(sourceTabKey);
  }

  function closeWorkflowTab(tabKey: WorkflowTabKey): void {
    if (actionsBlocked) return;
    if (tabKey === "terminal") {
      terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
        ...terminalLayout,
        open: false,
      });
      if (activeTabKey === "terminal") {
        selectFallbackTab();
      }
      return;
    }
    const sessionKey = sessionKeyFromWorkflowTab(tabKey);
    if (sessionKey === null) return;
    const session = runtimeSessions.find((s) => s.key === sessionKey);
    if (session) {
      void closeSession(session);
    }
  }

  function moveWorkflowTabToTerminal(tabKey: WorkflowTabKey): void {
    if (actionsBlocked) return;
    const sessionKey = sessionKeyFromWorkflowTab(tabKey);
    if (sessionKey !== null) {
      moveSessionToTerminal(sessionKey);
    }
  }

  function renameWorkflowTab(tabKey: WorkflowTabKey): void {
    if (actionsBlocked) return;
    const sessionKey = sessionKeyFromWorkflowTab(tabKey);
    if (sessionKey === null) return;
    const session = runtimeSessions.find((s) => s.key === sessionKey);
    if (!session) return;
    openRenamePrompt(session);
  }

  function openRenamePrompt(session: RuntimeSession): void {
    const triggerEl =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    previouslyFocusedEl = triggerEl;
    renamePrompt = {
      sessionKey: session.key,
      originalLabel: session.label,
    };
    renameInputValue = session.label;
  }

  function saveRenamePrompt(): void {
    if (renamePrompt === null || renameSaving) return;
    const trimmed = renameInputValue.trim();
    if (!trimmed) return;
    if (trimmed === renamePrompt.originalLabel) {
      cancelRenamePrompt();
      return;
    }

    const id = workspaceId;
    const hostKey = workspaceHostKey;
    const sessionKey = renamePrompt.sessionKey;
    renameSaving = true;
    appRuntime.runCommand(
      renameRuntimeSessionProgram(id, hostKey, sessionKey, trimmed),
      {
        operation: "workspace.session.rename",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          if (isCurrentWorkspace(id, hostKey)) renameSaving = false;
        },
      },
    );
  }

  function cancelRenamePrompt(): void {
    if (renameSaving) return;
    renamePrompt = null;
    renameInputValue = "";
  }

  function renameSession(session: RuntimeSession): void {
    renameWorkflowTab(workflowTabKeyForSession(session.key));
  }

  function createPresetSnapshot(name: string, id = newPresetID()): WorkflowPreset {
    const now = new Date().toISOString();
    return {
      id,
      name,
      createdAt:
        workflowPresets.find((preset) => preset.id === id)?.createdAt ?? now,
      updatedAt: now,
      sessions: runtimeSessions.map((session) => ({
        sourceKey: session.key,
        targetKey: session.target_key,
        region: sessionRegion(session),
        label: sessionDisplayLabels[session.key] ?? session.label,
      })),
      layout: terminalLayout,
    };
  }

  function saveWorkflowPreset(): void {
    const name = prompt("Preset name", "Review workspace");
    if (name === null) return;
    const trimmed = name.trim();
    if (!trimmed) return;
    const preset = createPresetSnapshot(trimmed);
    workflowPresets = [...workflowPresets, preset];
    selectedWorkflowPresetId = preset.id;
  }

  function updateWorkflowPreset(presetID: string): void {
    const existing = workflowPresets.find((preset) => preset.id === presetID);
    if (!existing) return;
    const preset = createPresetSnapshot(existing.name, existing.id);
    workflowPresets = workflowPresets.map((candidate) =>
      candidate.id === presetID ? preset : candidate,
    );
    selectedWorkflowPresetId = preset.id;
  }

  function applyWorkflowPreset(presetID: string): void {
    if (!workspaceId || applyingWorkflowPreset || actionsBlocked) return;
    const preset = workflowPresets.find((candidate) => candidate.id === presetID);
    if (!preset) return;
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    const presetOwner = viewWorkspaceKey;
    applyingWorkflowPresetFor = [...applyingWorkflowPresetFor, presetOwner];
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.applyPreset(runtimeTarget(id, hostKey), preset);
      }),
      {
        operation: "workspace.preset.apply",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          const index = applyingWorkflowPresetFor.indexOf(presetOwner);
          if (index !== -1) applyingWorkflowPresetFor = applyingWorkflowPresetFor.toSpliced(index, 1);
        },
      },
    );
  }

  function deleteWorkflowPreset(presetID: string): void {
    workflowPresets = workflowPresets.filter((preset) => preset.id !== presetID);
    if (selectedWorkflowPresetId === presetID) {
      selectedWorkflowPresetId = null;
    }
  }

  function mapPresetLayout(
    layout: TerminalLayoutState,
    keyMap: Record<string, string>,
  ): TerminalLayoutState {
    const sessionRegions: Record<string, SessionRegion> = {};
    for (const [sourceKey, region] of Object.entries(layout.sessionRegions)) {
      const mappedKey = keyMap[sourceKey];
      if (mappedKey) sessionRegions[mappedKey] = region;
    }
    return {
      ...layout,
      activeSessionKey:
        layout.activeSessionKey ? keyMap[layout.activeSessionKey] ?? null : null,
      tree: mapPaneNodeSessionKeys(layout.tree, keyMap),
      terminalGroups: mapTerminalGroupSessionKeys(layout.terminalGroups, keyMap),
      workflowTree: mapWorkflowNodeSessionKeys(layout.workflowTree, keyMap),
      sessionRegions,
      customSessionLabels: {},
    };
  }

  function mapTerminalGroupSessionKeys(
    groups: TerminalGroup[],
    keyMap: Record<string, string>,
  ): TerminalGroup[] {
    return groups.flatMap((group) => {
      const tree = mapPaneNodeSessionKeys(group.tree, keyMap);
      if (!tree) return [];
      return [
        {
          ...group,
          activeSessionKey: group.activeSessionKey
            ? keyMap[group.activeSessionKey] ?? firstLeaf(tree)?.sessionKey ?? null
            : firstLeaf(tree)?.sessionKey ?? null,
          tree,
        },
      ];
    });
  }

  function mapPaneNodeSessionKeys(
    node: PaneNode | null,
    keyMap: Record<string, string>,
  ): PaneNode | null {
    if (!node) return null;
    if (node.type === "leaf") {
      const mappedKey = keyMap[node.sessionKey];
      return mappedKey ? { ...node, sessionKey: mappedKey } : null;
    }
    const first = mapPaneNodeSessionKeys(node.first, keyMap);
    const second = mapPaneNodeSessionKeys(node.second, keyMap);
    if (!first) return second;
    if (!second) return first;
    return { ...node, first, second };
  }

  function firstWorkflowTab(layout: TerminalLayoutState): WorkflowTabKey | null {
    if (!layout.workflowTree) return null;
    if (layout.workflowTree.type === "leaf") {
      return layout.workflowTree.activeTabKey;
    }
    const leaf = firstWorkflowLeafFrom(layout.workflowTree);
    return leaf?.activeTabKey ?? null;
  }

  function firstWorkflowLeafFrom(
    node: NonNullable<TerminalLayoutState["workflowTree"]>,
  ): Extract<NonNullable<TerminalLayoutState["workflowTree"]>, { type: "leaf" }> | null {
    if (node.type === "leaf") return node;
    return firstWorkflowLeafFrom(node.first) ?? firstWorkflowLeafFrom(node.second);
  }

  function sessionRegionForLayout(
    session: RuntimeSession,
    layout: TerminalLayoutState,
  ): SessionRegion {
    return layout.sessionRegions[session.key] ?? defaultSessionRegion(session);
  }

  function newPresetID(): string {
    if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
      return `preset-${crypto.randomUUID()}`;
    }
    return `preset-${Date.now().toString(36)}-${Math.random()
      .toString(16)
      .slice(2)}`;
  }

  function splitTerminal(direction: SplitDirection): void {
    if (terminalLaunching || actionsBlocked) return;
    const groupBeforeLaunch = currentTerminalGroup;
    const treeBeforeLaunch = groupBeforeLaunch?.tree ?? null;
    const targetLeaf =
      terminalLayout.activeSessionKey !== null
        ? findLeafBySession(treeBeforeLaunch, terminalLayout.activeSessionKey)
        : firstLeaf(treeBeforeLaunch);
    launchTerminalSession({
      _tag: "TerminalSplit",
      direction,
      groupID: groupBeforeLaunch?.id ?? terminalLayout.activeTerminalGroupID ?? newPaneGroupID(),
      ...(targetLeaf === null ? {} : { targetLeafID: targetLeaf.id }),
    });
  }

  function splitTerminalSessionIntoPane(
    sessionKey: string,
    targetLeafID: string,
    direction: SplitDirection,
    placement: "before" | "after",
  ): void {
    if (actionsBlocked) return;
    const session = runtimeSessions.find((candidate) => candidate.key === sessionKey);
    const groupID = terminalLayout.activeTerminalGroupID;
    const group = currentTerminalGroup;
    if (!session || !groupID || !group) return;
    if (isPromoted(session)) surfaceLayout?.demoteTab(sessionPaneKeyFor(session));
    const sourceLeaf = findLeafBySession(group.tree, sessionKey);
    if (sourceLeaf?.id === targetLeafID) {
      selectTerminalSession(sessionKey);
      return;
    }
    const groupsWithoutSource = closeSessionInTerminalGroups(
      terminalLayout.terminalGroups,
      sessionKey,
    );
    const targetGroup =
      groupsWithoutSource.find((candidate) => candidate.id === groupID) ?? group;
    const tree = splitSessionIntoPane(
      targetGroup.tree,
      targetLeafID,
      sessionKey,
      direction,
      placement,
    );
    const groups = updateTerminalGroupTree(
      groupsWithoutSource.some((candidate) => candidate.id === groupID)
        ? groupsWithoutSource
        : [...groupsWithoutSource, targetGroup],
      groupID,
      (candidate) => ({
        ...candidate,
        activeSessionKey: sessionKey,
        tree,
      }),
    );
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...layoutWithTerminalGroups(
        {
          ...terminalLayout,
          open: true,
          sessionRegions: {
            ...terminalLayout.sessionRegions,
            [sessionKey]: "terminal",
          },
        },
        groups,
        groupID,
      ),
    });
  }

  function newPaneGroupID(): string {
    if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
      return `terminal-group-${crypto.randomUUID()}`;
    }
    return `terminal-group-${Date.now().toString(36)}-${Math.random()
      .toString(16)
      .slice(2)}`;
  }

  function dockTerminalPanel(dock: TerminalDock): void {
    if (actionsBlocked) return;
    terminalLayout = normalizeLayoutForSessions(runtimeSessions, {
      ...terminalLayout,
      dock,
      open: true,
    });
    if (dock === "top") {
      selectWorkspaceTab("terminal");
    } else if (activeTabKey === "terminal") {
      selectFallbackTab();
    }
  }

  function resizeTerminalPanel(height: number): void {
    if (actionsBlocked) return;
    terminalLayout = {
      ...terminalLayout,
      height: clampTerminalHeight(height),
    };
  }

  function updateActiveTerminalTree(tree: PaneNode | null): void {
    if (actionsBlocked) return;
    const activeGroupID = terminalLayout.activeTerminalGroupID;
    terminalLayout = {
      ...terminalLayout,
      tree,
      terminalGroups: activeGroupID
        ? updateTerminalGroupTree(
            terminalLayout.terminalGroups,
            activeGroupID,
            (group) => ({ ...group, tree }),
          )
        : terminalLayout.terminalGroups,
    };
  }

  function readDroppedSession(event: DragEvent): string | null {
    const runtimeSessionKey = readRuntimeSessionDrag(event, workspaceId);
    if (runtimeSessionKey !== null) return runtimeSessionKey;
    if (surfaceLayout === null) return null;
    const paneKey = readTabbedPanelTabDrag(event, surfaceLayout.dragScope);
    if (
      paneKey === null ||
      !sessionPaneKeyMatchesWorkspace(paneKey, workspaceId, workspaceHostKey)
    ) {
      return null;
    }
    const sessionKey = parseSessionPaneKey(paneKey)?.sessionKey ?? null;
    return sessionKey !== null &&
      runtimeSessions.some((session) => session.key === sessionKey)
      ? sessionKey
      : null;
  }

  function handleWorkflowDragOver(event: DragEvent): void {
    if (actionsBlocked) return;
    if (readDroppedSession(event) === null) return;
    event.preventDefault();
    if (event.dataTransfer) {
      event.dataTransfer.dropEffect = "move";
    }
  }

  function handleWorkflowDrop(event: DragEvent): void {
    if (actionsBlocked) return;
    const sessionKey = readDroppedSession(event);
    if (sessionKey === null) return;
    event.preventDefault();
    event.stopPropagation();
    moveSessionToWorkflow(sessionKey);
    clearActiveTerminalDrag();
    clearActiveTabbedPanelDrag();
  }

  function startPolling(): void {
    if (!workspaceId) return;
    const key = JSON.stringify([workspaceHostKey ?? null, workspaceId]);
    if (workspacePolling?.key === key) return;
    stopPolling();
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    const execution = appRuntime.runCommand(
      Stream.fromSchedule(Schedule.spaced("3 seconds")).pipe(
        Stream.runForEach(() =>
          isCurrentWorkspace(id, hostKey)
            ? fetchWorkspaceProgram(id, hostKey).pipe(Effect.asVoid)
            : Effect.void,
        ),
      ),
      {
        operation: "workspace.details.poll",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
    workspacePolling = { key, interrupt: execution.interrupt };
  }

  function stopPolling(): void {
    workspacePolling?.interrupt();
    workspacePolling = null;
  }

  function startRuntimePolling(): void {
    if (!workspaceId) return;
    const key = JSON.stringify([workspaceHostKey ?? null, workspaceId]);
    if (runtimePolling?.key === key) return;
    stopRuntimePolling();
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    const execution = appRuntime.runCommand(
      Stream.fromSchedule(Schedule.spaced("3 seconds")).pipe(
        Stream.runForEach(() =>
          isCurrentWorkspace(id, hostKey)
            ? fetchRuntimeProgram().pipe(Effect.asVoid)
            : Effect.void,
        ),
      ),
      {
        operation: "workspace.runtime.poll",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
    runtimePolling = { key, interrupt: execution.interrupt };
  }

  function stopRuntimePolling(): void {
    runtimePolling?.interrupt();
    runtimePolling = null;
  }

  function workspaceMutationFailureMessage(
    failure:
      | ApiProblemError
      | InvalidExternalPayload
      | TransientTransportError
      | WorkspaceRuntimeMutationOutcomeUnknown,
    fallback: string,
  ): string {
    return failure instanceof WorkspaceRuntimeMutationOutcomeUnknown
      ? `Could not confirm whether the ${failure.operation.toLowerCase()} completed. Retry will check workspace state before sending anything.`
      : failure._tag === "ApiProblemError"
      ? apiErrorMessage(
          failure.problem,
          failure.problem.status === undefined ? fallback : `${fallback} (${failure.problem.status})`,
        )
      : failure.cause instanceof Error
        ? failure.cause.message
        : fallback;
  }

  function handleRetrySetup(): void {
    if (!workspace || retryingSetup || actionsBlocked) return;

    const id = workspaceId;
    const hostKey = workspaceHostKey;
    retryingSetup = true;
    appRuntime.runCommand(
      retryWorkspaceSetupMutationProgram(id, hostKey),
      {
        operation: "workspace.setup.retry",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          if (isCurrentWorkspace(id, hostKey)) retryingSetup = false;
        },
      },
    );
  }

  function handleRefreshWorkspace(): void {
    if (!workspace || refreshingWorkspace || actionsBlocked) return;

    const id = workspace.id;
    const hostKey = workspaceHostKey;
    refreshingWorkspace = true;
    appRuntime.runCommand(
      refreshWorkspaceMutationProgram(id, hostKey),
      {
        operation: "workspace.refresh",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          if (isCurrentWorkspace(id, hostKey)) refreshingWorkspace = false;
        },
      },
    );
  }

  // Provider-aware identity of the loaded envelope, but only while it still
  // describes the given workspace: deletion must tombstone the inline
  // claim/override slot even for workspaces that were never claimed inline,
  // and the store has no identity metadata of its own for those.
  function workspaceIdentitySnapshot(targetId: string): WorkspaceItemIdentity | undefined {
    const ws = workspace;
    if (!ws || ws.id !== targetId) return undefined;
    return {
      provider: ws.repo.provider,
      platformHost: ws.repo.platform_host,
      owner: ws.repo.owner,
      name: ws.repo.name,
      repoPath: ws.repo.repo_path,
      number: ws.item_number,
      // Envelope vocabulary ("pull_request"/"issue"/"kata_task");
      // canonicalItemType maps it for identity comparison.
      itemType: ws.item_type,
    };
  }

  function openForceDeleteRecovery(triggerEl: HTMLElement | null = null): void {
    if (
      !workspaceLive ||
      !workspace ||
      workspace.status !== "deletion_failed" ||
      forceDeleting
    ) {
      return;
    }
    previouslyFocusedEl =
      triggerEl ??
      (document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null);
    forcePromptForId = workspace.id;
    forcePromptHostKey = workspaceHostKey;
    forcePromptIdentity = workspaceIdentitySnapshot(workspace.id);
    forcePromptMessage =
      workspace.error_message ??
      "Workspace deletion failed. Force deletion can remove the remaining workspace artifacts.";
  }

  /**
   * Every workspace Delete confirms first. The strip's trash is a 13px icon beside
   * the controls trigger, one slip from firing, and the backend refuses only a
   * DIRTY worktree - a clean workspace with unpushed commits deletes silently. One
   * uniform gate rather than a per-button one: the same action must not be safe
   * from one button and instant from another.
   */
  function handleDelete(triggerEl: HTMLElement | null = null): void {
    if (actionsBlocked) return;
    previouslyFocusedEl =
      triggerEl ??
      (document.activeElement instanceof HTMLElement ? document.activeElement : null);
    deletePromptOpen = true;
  }

  function cancelDeletePrompt(): void {
    deletePromptOpen = false;
  }

  function confirmDeletePrompt(): void {
    const triggerEl = previouslyFocusedEl;
    deletePromptOpen = false;
    void performDelete(triggerEl);
  }

  function releaseRetainedRuntimePresenter(lease: RetainedRuntimePresenterLease): void {
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.releasePresenter(lease.target, lease.presenterID);
      }),
      {
        operation: "workspace.runtime.presenter.release",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );
  }

  function performDelete(
    triggerEl: HTMLElement | null = null,
  ): void {
    if (actionsBlocked) return;
    const targetId = workspaceId;
    const targetHostKey = workspaceHostKey;
    const targetIdentity = workspaceIdentitySnapshot(targetId);
    // Capture the trigger synchronously: the click handler runs
    // before `inert` is applied to .terminal-view, so this is the
    // last point we can read the originating focused element. By
    // the time the post-await effect runs, the browser has cleared
    // focus to document.body.
    triggerEl ??=
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    const target = runtimeTarget(targetId, targetHostKey);
    const lease = { target, presenterID: makeWorkspaceRuntimePresenterID() };
    const presentationGeneration = workspacePresentationGeneration;
    retainedRuntimePresenterLeases = [...retainedRuntimePresenterLeases, lease];
    deleteTriggerElements.set(runtimeMutationTargetKey(target), triggerEl);
    addDeletingWorkspaceTarget(targetId, targetHostKey);
    appRuntime.runCommand(
      deleteWorkspaceMutationProgram(
        targetId,
        targetHostKey,
        false,
        targetIdentity,
        lease.presenterID,
        () => workspacePresentationGeneration === presentationGeneration,
      ),
      {
        operation: "workspace.delete",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          removeDeletingWorkspaceTarget(targetId, targetHostKey);
          clearRetainedDeletePresentation(target);
          releaseRetainedRuntimePresenter(lease);
        },
      },
    );
  }

  function isDeletingWorkspaceTarget(
    target: DeletingWorkspaceTarget,
    id: string,
    hostKey: string | undefined,
  ): boolean {
    return target.id === id && target.hostKey === hostKey;
  }

  function addDeletingWorkspaceTarget(
    id: string,
    hostKey: string | undefined,
  ): void {
    if (
      deletingWorkspaceTargets.some((target) =>
        isDeletingWorkspaceTarget(target, id, hostKey),
      )
    ) {
      return;
    }
    deletingWorkspaceTargets = [...deletingWorkspaceTargets, { id, hostKey }];
  }

  function removeDeletingWorkspaceTarget(
    id: string,
    hostKey: string | undefined,
  ): void {
    deletingWorkspaceTargets = deletingWorkspaceTargets.filter(
      (target) => !isDeletingWorkspaceTarget(target, id, hostKey),
    );
  }

  function isCurrentTerminalRoute(targetId: string): boolean {
    return window.location.pathname.endsWith(terminalRoute(targetId));
  }

  function confirmForceDelete(): void {
    if (forceDeleting) return;
    const targetId = forcePromptForId;
    if (targetId === null) return;
    const targetHostKey = forcePromptHostKey;
    // Prefer the snapshot taken at 409 time; the live envelope may belong
    // to a different workspace after an A -> B switch.
    const targetIdentity = forcePromptIdentity ?? workspaceIdentitySnapshot(targetId);
    forceDeleting = true;
    const target = runtimeTarget(targetId, targetHostKey);
    const lease = { target, presenterID: makeWorkspaceRuntimePresenterID() };
    const presentationGeneration = workspacePresentationGeneration;
    retainedRuntimePresenterLeases = [...retainedRuntimePresenterLeases, lease];
    addDeletingWorkspaceTarget(targetId, targetHostKey);
    appRuntime.runCommand(
      deleteWorkspaceMutationProgram(
        targetId,
        targetHostKey,
        true,
        targetIdentity,
        lease.presenterID,
        () => workspacePresentationGeneration === presentationGeneration,
      ),
      {
        operation: "workspace.force-delete",
        safeContext: { surface: "workspace" },
        onFailure: () => {
          removeDeletingWorkspaceTarget(targetId, targetHostKey);
          forceDeleting = false;
          clearRetainedDeletePresentation(target);
          releaseRetainedRuntimePresenter(lease);
        },
      },
    );
  }

  function cancelForceDelete(): void {
    if (forceDeleting) return;
    forcePromptMessage = null;
    forcePromptForId = null;
    forcePromptHostKey = undefined;
    forcePromptIdentity = undefined;
  }

  let previouslyFocusedEl: HTMLElement | null = null;

  $effect(() => {
    let afterTick: Effect.Effect<void> | null = null;
    if (renamePrompt !== null) {
      afterTick = Effect.sync(() => {
        renameInputEl?.focus();
        renameInputEl?.select();
      });
    } else if (!modalOpen && previouslyFocusedEl !== null) {
      const triggerEl = previouslyFocusedEl;
      previouslyFocusedEl = null;
      afterTick = Effect.sync(() => {
        if (document.contains(triggerEl)) {
          triggerEl.focus();
        }
      });
    }
    if (afterTick === null) return;
    const execution = appRuntime.runCommand(Effect.promise(() => tick()).pipe(Effect.andThen(afterTick)), {
      operation: "workspace.dialog.focus",
      safeContext: { surface: "workspace" },
      onFailure: () => undefined,
    });
    return execution.interrupt;
  });

  $effect(() => {
    if (!workspaceLive || !workspace) return;
    if (!isSidebarTabSupported(workspace, sidebarTab)) {
      setSidebarTab(defaultSidebarTab(workspace));
    }
  });

  // React to workspaceId changes (including / from "" on the
  // bare /workspaces route) without remounting the entire view.
  // Removing the {#key} that previously wrapped this component in
  // App.svelte means the lifecycle is now driven entirely by this
  // effect.
  //
  // Keep the previous workspace and runtime available to the workflow
  // stage until their replacements arrive. The right sidebar gates on
  // runtimeLive separately, so it cannot mix those retained values with
  // the newly selected route.
  $effect(() => {
    const id = workspaceId;
    const hostKey = workspaceHostKey;
    workspacePresentationGeneration += 1;
    runtimeSnapshotAuthoritative = false;
    if (
      appliedRuntimeState?.workspaceId !== id ||
      appliedRuntimeState.hostKey !== hostKey
    ) {
      appliedRuntimeState = null;
    }
    // Route selection is the zero point for workspace-switch timing:
    // every workspace-switch:* measure is a duration from this call.
    // The token lets the cleanup below cancel exactly this switch when
    // the user leaves the workspace surface entirely, without being
    // able to cancel a newer switch begun elsewhere.
    let switchToken: string | null = null;
    if (id) {
      switchToken = beginWorkspaceSwitch(appRuntime, id, hostKey);
    } else {
      cancelWorkspaceSwitch();
    }
    const storageId = id ? workspaceStorageId(id, hostKey) : "";
    const restoredLayout = id ? loadTerminalLayout(storageId) : defaultTerminalLayout();
    const restoredTab = restoreWorkspaceTab(storageId);
    const restoredActiveTab =
      restoredTab === "terminal" &&
      !(restoredLayout.open && restoredLayout.dock === "top")
        ? "home"
        : restoredTab;
    const layoutForActiveTab =
      restoredActiveTab === "home"
        ? restoredLayout
        : { ...restoredLayout, workflowMode: "tabs" as const };

    // Tab state from the previous workspace can't be valid for a
    // different workspace's runtime, so reset these even though
    // workspace/runtime themselves are kept.
    restoreWorkspaceTabSelection(restoredActiveTab);
    terminalLayout = layoutForActiveTab;
    terminalLayoutWorkspaceId = storageId;
    launchingKey = null;
    terminalLaunching = false;
    closedSessions = [];
    // Retry/refresh requests guard their own state mutations on
    // isCurrentWorkspace, so an in-flight one that resolves after a
    // route change skips its finally cleanup and would otherwise
    // leave these flags stuck true on the next workspace.
    retryingSetup = false;
    refreshingWorkspace = false;
    lastDiffSnapshotVersion = "";

    // Errors/transient flags from the prior workspace should not
    // bleed across — clear them but don't touch workspace/runtime.
    loadError = null;
    runtimeError = null;
    // A 409 force-delete prompt is bound to the workspace that
    // produced it. Dismiss it on any route change so the user
    // can't confirm a destructive action targeting a workspace
    // they're no longer looking at.
    forcePromptMessage = null;
    forcePromptForId = null;
    forcePromptHostKey = undefined;
    forcePromptIdentity = undefined;
    stopPromptSession = null;
    stopSessionStopping = false;
    renamePrompt = null;
    renameInputValue = "";
    renameSaving = false;
    mountedSessionKeys = restoredActiveTab.startsWith("session:")
      ? [restoredActiveTab.slice("session:".length)]
      : [];

    if (!id) {
      // /workspaces route: drop workspace data so the empty-state
      // message renders rather than continuing to show whatever
      // the previous /terminal/{id} session left behind.
      workspace = null;
      runtime = null;
      runtimeForId = "";
      runtimeForHostKey = undefined;
      stopRuntimePolling();
      return;
    }

    const mutationPresenter = appRuntime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          const workflow = yield* WorkspaceRuntimeWorkflow;
          const target = runtimeTarget(id, hostKey);
          const context = yield* Effect.context<AppServices>();
          yield* Effect.acquireRelease(
            workflow.claimPresenter(
              target,
              runtimePresenterID,
              (state) => presentRuntimeMutation(state, runtimePresenterID).pipe(Effect.provide(context)),
            ),
            () => workflow.releasePresenter(target, runtimePresenterID),
          );
          yield* Effect.never;
        }),
      ),
      {
        operation: "workspace.runtime.presenter",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );

    const fleetDiffWatch = hostKey
      ? appRuntime.runCommand(
          watchFleetWorkspaceDiff(id, hostKey, (version) =>
            Effect.sync(() => {
              if (!isCurrentWorkspace(id, hostKey) || version === lastDiffSnapshotVersion) return;
              lastDiffSnapshotVersion = version;
              diffRefreshToken += 1;
            }),
          ),
          {
            operation: "workspace.fleet-diff.watch",
            safeContext: { surface: "workspace" },
            onFailure: () => undefined,
          },
        )
      : null;

    const workspaceLifecycle = appRuntime.runCommand(
      Effect.gen(function* () {
        const initialWorkspace = yield* Deferred.make<Workspace | null>();
        const events = Stream.runForEach(
          workspaceEventStream(
            eventsStore.subscribeWorkspaceEvents,
            hostKey ? undefined : () => eventsStore.selectWorkspace(id),
          ),
          (signal) => {
            switch (signal._tag) {
              case "Open":
                return Deferred.await(initialWorkspace).pipe(
                  Effect.flatMap((loaded) =>
                    isCurrentWorkspace(id, hostKey) &&
                    loaded?.id === id &&
                    selectedWorkspaceHostKey(loaded) === hostKey &&
                    loaded.enrichment_status === "pending"
                      ? fetchWorkspaceProgram(id, hostKey).pipe(Effect.asVoid)
                      : Effect.void,
                  ),
                );
              case "Status":
                return signal.workspaceId === undefined || signal.workspaceId === id
                  ? fetchWorkspaceProgram(id, hostKey).pipe(Effect.asVoid)
                  : Effect.void;
              case "Associated":
                return signal.workspaceId === id
                  ? fetchWorkspaceProgram(id, hostKey).pipe(Effect.asVoid)
                  : Effect.void;
              case "ReconnectStale":
                return Effect.sync(() => {
                  diffRefreshToken += 1;
                }).pipe(
                  Effect.andThen(
                    Effect.all([fetchWorkspaceProgram(id, hostKey), fetchRuntimeProgram()], {
                      concurrency: "unbounded",
                      discard: true,
                    }),
                  ),
                );
              case "DiffReady":
                return Effect.sync(() => {
                  if (
                    signal.workspaceId !== id ||
                    signal.version === undefined ||
                    signal.version === ""
                  ) {
                    return;
                  }
                  lastDiffSnapshotVersion = signal.version;
                });
              case "DiffChanged":
                return Effect.sync(() => {
                  if (
                    signal.workspaceId !== id ||
                    signal.version === undefined ||
                    signal.version === "" ||
                    signal.version === lastDiffSnapshotVersion
                  ) {
                    return;
                  }
                  lastDiffSnapshotVersion = signal.version;
                  diffRefreshToken += 1;
                });
            }
          },
        ).pipe(Effect.retry({ schedule: reconnectSchedule }));
        const eventFiber = yield* Effect.forkChild(events, { startImmediately: true });
        yield* Effect.forkChild(fetchRuntimeProgram(), { startImmediately: true });
        const loaded = yield* fetchWorkspaceProgram(id, hostKey);
        yield* Deferred.succeed(initialWorkspace, loaded);
        yield* Effect.sync(() => {
          if (!isCurrentWorkspace(id, hostKey)) return;
          if (loaded?.status === "creating") startPolling();
          else if (loaded?.status === "ready") startRuntimePolling();
        });
        yield* Fiber.join(eventFiber);
      }),
      {
        operation: "workspace.lifecycle",
        safeContext: { surface: "workspace" },
        onFailure: () => undefined,
      },
    );

    return () => {
      stopPolling();
      stopRuntimePolling();
      releaseRuntimeRead();
      mutationPresenter.interrupt();
      fleetDiffWatch?.interrupt();
      workspaceLifecycle.interrupt();
      // Leaving the workspace surface (view unmount) must end the
      // switch so late responses and pane callbacks cannot append
      // phases. On a switch to another workspace this cleanup runs
      // just before the next effect run, cancelling the old switch the
      // new beginWorkspaceSwitch was about to supersede anyway; the
      // token guard only prevents cancelling a switch this run does
      // not own.
      if (switchToken !== null) {
        cancelWorkspaceSwitch(switchToken);
      }
    };
  });

  $effect(() => {
    if (
      workspaceId ||
      workspaceListState.status !== "loaded" ||
      workspaceListState.total !== 0
    ) {
      return;
    }
    void loadEmptyLaunchTargets();
  });

  $effect(() => {
    if (!workspaceId || !runtimeLive || workspace?.status !== "ready") return;
    if (actionsBlocked || launchingKey !== null) return;
    const pendingLaunch = pendingWorkspaceLaunch(workspaceId, workspaceHostKey);
    const targetKey = pendingLaunch?.targetKey ?? null;
    if (targetKey === null) return;
    if (pendingLaunch?.phase === "awaiting_session") return;
    if (runtimeSessions.length > 0) {
      discardWorkspaceLaunch(workspaceId, workspaceHostKey);
      return;
    }
    const target = launchTargets.find(
      (candidate) => candidate.key === targetKey,
    );
    if (!target || target.kind !== "agent" || !target.available) {
      if (discardWorkspaceLaunch(workspaceId, workspaceHostKey) === null) return;
      const reason =
        target?.disabled_reason ?? "is not available in this workspace";
      showFlash(`Agent "${targetKey}" could not launch: ${reason}`, {
        tone: "danger",
      });
      return;
    }
    const claim = claimWorkspaceLaunch(workspaceId, workspaceHostKey);
    if (claim === null) return;
    handleLaunch(claim.targetKey, claim);
  });
</script>

<div
  class="terminal-view"
  bind:this={workspaceRoot}
  tabindex="-1"
  inert={modalOpen}
>
  {#snippet inlineCollapseControl()}
    <!-- Collapsing the inline dock is pure local UI and must stay
         reachable in every workspace state: the toolbar that carries the
         dock controls only renders once the workspace is ready, so slow
         setup, a fetch failure, or a setup error would otherwise leave
         the dock permanently open short of deleting the workspace or
         navigating away. -->
    {#if inlineDock && inlineDockMode !== null}
      <button class="retry-btn" onclick={() => inlineDock?.setMode("collapsed")}>
        Collapse Terminal
      </button>
    {/if}
  {/snippet}
  {#snippet terminalMainContent()}
    <div class="terminal-main">
      {#if !workspaceId}
        {#if workspaceListState.status === "loaded" && workspaceListState.total === 0}
          <section class="workspace-zero-state" aria-label="Workspaces empty state">
            <div class="workspace-zero-copy">
              <p class="workspace-zero-eyebrow">Workspaces</p>
              <h2>Create a workspace to run agents on a branch</h2>
              <p>
                Workspaces are git worktrees: a PR workspace checks out the PR
                head, while issue-backed and unplanned work start from the
                repository's default branch.
              </p>
              <p>
                From a PR or issue, use the
                <span
                  class="workspace-zero-inline-action"
                  aria-label="Create Workspace example"
                >
                  <Button
                    class="workspace-zero-create-button"
                    disabled
                    tone="info"
                    surface="soft"
                    size="sm"
                    title="Create a PR or issue worktree, then open Workspaces to launch agents, shells, or local review sessions on that branch."
                    label="Create Workspace"
                    shortLabel="Create Workspace"
                  >
                    <PackagePlusIcon
                      size="14"
                      strokeWidth="2.2"
                      aria-hidden="true"
                    />
                  </Button>
                </span>
                button to launch a workspace.
              </p>
              <p>
                For unplanned work, use New workspace in the sidebar to pick a
                tracked repository and branch from its default head.
              </p>
              <p>
                Once it exists, this pane can start agents, local review
                sessions, or a shell inside that worktree.
              </p>
            </div>
            <div class="workspace-zero-example-card" aria-label="Workspace workflow example">
              <div class="workspace-zero-example" aria-label="Launch surface example">
                <span class="workspace-zero-example-label">
                  You can then launch configured agents via the buttons provided
                </span>
                {#if emptyLaunchTargets.length > 0}
                  <WorkspaceHome
                    launchTargets={emptyLaunchTargets}
                    sessions={[]}
                    readonly
                    showHeader={false}
                  />
                {:else if emptyLaunchTargetsState === "error"}
                  <p class="workspace-zero-example-empty">
                    Launch targets could not be loaded.
                  </p>
                {:else if emptyLaunchTargetsState === "loaded"}
                  <p class="workspace-zero-example-empty">
                    Launch targets appear here after agent tools are configured
                    or detected.
                  </p>
                {:else}
                  <p class="workspace-zero-example-empty">
                    Loading launch targets...
                  </p>
                {/if}
              </div>
            </div>
          </section>
        {:else}
          <div class="state-message">
            Select a workspace from the sidebar
          </div>
        {/if}
      {:else if loadError && !workspaceLive}
        <div class="state-message error">
          <span
            class="error-icon-badge"
            role="img"
            aria-label="Workspace load failed"
          >
            <AlertIcon
              class="error-icon"
              size="14"
              strokeWidth="2.4"
              aria-hidden="true"
            />
          </span>
          <span>{loadError}</span>
          <button
            class="retry-btn"
            onclick={() => {
              loadError = null;
              requestWorkspace();
            }}
          >
            Retry
          </button>
          {@render inlineCollapseControl()}
        </div>
      {:else if !workspace || !workspaceLive || workspace.status === "creating"}
        <!-- Liveness, not mere presence: during an in-place A→B switch the
             previous workspace stays cached while B loads, and rendering
             its ready toolbar here would show a stale header with action
             guards engaged — an uncollapsible dock if B is slow or fails. -->
        <div class="state-message">
          <Spinner size={18} />
          <span>Setting up workspace...</span>
          {@render inlineCollapseControl()}
        </div>
      {:else if workspace.status === "error"}
        <div class="state-message error">
          <span
            class="error-icon-badge"
            role="img"
            aria-label="Workspace setup failed"
          >
            <AlertIcon
              class="error-icon"
              size="14"
              strokeWidth="2.4"
              aria-hidden="true"
            />
          </span>
          <span>
            {workspace.error_message ??
              "Workspace setup failed"}
          </span>
          <button
            class="retry-btn"
            disabled={actionsBlocked || retryingSetup}
            onclick={() => void handleRetrySetup()}
          >
            Retry
          </button>
          <button
            class="retry-btn danger"
            disabled={actionsBlocked}
            onclick={(event) =>
              void handleDelete(event.currentTarget)}
          >
            Delete
          </button>
          {@render inlineCollapseControl()}
        </div>
      {:else if workspace.status === "deleting"}
        <div class="state-message">
          <Spinner size={18} />
          <span>Deleting workspace...</span>
          {@render inlineCollapseControl()}
        </div>
      {:else if workspace.status === "deletion_failed"}
        <div class="state-message error">
          <span
            class="error-icon-badge"
            role="img"
            aria-label="Workspace deletion failed"
          >
            <AlertIcon
              class="error-icon"
              size="14"
              strokeWidth="2.4"
              aria-hidden="true"
            />
          </span>
          <span>
            {workspace.error_message ??
              "Workspace deletion failed."}
          </span>
          <button
            class="retry-btn danger"
            disabled={forceDeleting}
            onclick={(event) =>
              openForceDeleteRecovery(event.currentTarget)}
          >
            Force delete workspace
          </button>
          {@render inlineCollapseControl()}
        </div>
      {:else}
        <!-- Never in a detail pane, whatever it holds. The pane's tab strip already
             names the workspace and carries its controls, so this bar only ever
             repeated them - and its Expand/Collapse Terminal pair duplicated the
             leaf's own maximize and close. A flattened surface is the exception it
             has always been: no per-leaf strip exists there, so the chrome is the
             only thing left to carry these. -->
        {#if !controlsInPane}
          <div class="header-bar">
            <div class="header-start">
              <span class="header-name">
                {displayName(workspace)}
              </span>
              <code class="header-branch">
                {workspace.git_head_ref}
              </code>
            </div>
            <div class="header-end">
              {#if !hideRightSidebar}
                <div class="panel-toggle-group">
                  <button
                    class="panel-toggle-btn"
                    class:active={sidebarOpen && sidebarTab === "diff"}
                    disabled={actionsBlocked}
                    onclick={() => handleSidebarToggleClick("diff")}
                  >
                    Diff
                  </button>
                  {#if workspace.item_type === "issue"}
                    <button
                      class="panel-toggle-btn"
                      class:active={sidebarOpen && sidebarTab === "issue"}
                      disabled={actionsBlocked}
                      onclick={() => handleSidebarToggleClick("issue")}
                    >
                      Issue
                    </button>
                  {/if}
                  {#if workspaceHostKey === undefined}
                    <button
                      class="panel-toggle-btn"
                      class:active={sidebarOpen && sidebarTab === "kata"}
                      disabled={actionsBlocked}
                      onclick={() => handleSidebarToggleClick("kata")}
                    >
                      Kata
                    </button>
                  {/if}
                  {#if getWorkspacePRNumber(workspace) !== null}
                    <button
                      class="panel-toggle-btn"
                      class:active={sidebarOpen && sidebarTab === "pr"}
                      disabled={actionsBlocked}
                      onclick={() => handleSidebarToggleClick("pr")}
                    >
                      PR
                    </button>
                  {/if}
                  {#if workspace.item_type === "pull_request"}
                    <button
                      class="panel-toggle-btn"
                      class:active={sidebarOpen && sidebarTab === "reviews"}
                      disabled={actionsBlocked}
                      onclick={() => handleSidebarToggleClick("reviews")}
                    >
                      Reviews
                    </button>
                  {/if}
                </div>
                <IconButton
                  class="workspace-refresh-button"
                  size="sm"
                  disabled={actionsBlocked || refreshingWorkspace}
                  ariaLabel="Refresh workspace details"
                  onclick={() => void handleRefreshWorkspace()}
                >
                  {#if refreshingWorkspace}
                    <Spinner size={14} label="Refreshing workspace" />
                  {:else}
                    <RefreshIcon
                      class="header-icon"
                      size="14"
                      strokeWidth="2.2"
                      aria-hidden="true"
                    />
                  {/if}
                </IconButton>
              {/if}
              {#if inlineDock && inlineDockMode !== null}
                <!-- Dock mode changes are pure local UI: they must stay
                     available while server-side actions are blocked
                     (deletes in flight), or the dock cannot be collapsed
                     out of the way. Only the modal guard applies, and only
                     to the expand direction. -->
                <button
                  class="header-btn"
                  disabled={inlineDockMode !== "expanded" && inlineDockExpandBlocked}
                  title={
                    inlineDockMode !== "expanded" && inlineDockExpandBlocked
                      ? "Close the open dialog first."
                      : undefined
                  }
                  onclick={() =>
                    inlineDock?.setMode(inlineDockMode === "expanded" ? "split" : "expanded")}
                >
                  {#if inlineDockMode === "expanded"}
                    <ChevronsDownIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                    Show Details
                  {:else}
                    <ChevronsUpIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                    Expand Terminal
                  {/if}
                </button>
                <button
                  class="header-btn"
                  onclick={() => inlineDock?.setMode("collapsed")}
                >
                  <PanelBottomCloseIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                  Collapse Terminal
                </button>
              {/if}
              <button
                class="header-btn danger"
                disabled={actionsBlocked}
                onclick={(event) =>
                  void handleDelete(event.currentTarget)}
              >
                Delete
              </button>
            </div>
          </div>
        {/if}
        <div
          class="terminal-and-sidebar"
          bind:this={containerEl}
          bind:clientWidth={containerWidth}
        >
          <div class="terminal-area">
            <div class="workspace-surface">
              {#if !controlsInPane}
                <!-- Kept for the standalone Workspaces tab, whose panes have no tab
                     strip to hold the controls, and for a flattened detail surface,
                     which suppresses per-leaf chrome. Otherwise the pane's own
                     popover renders these, and a bar here would be a second copy of
                     them above the terminal. -->
                <div class="workspace-toolbar">
                  <div class="workspace-toolbar-title">Workflow</div>
                  <div class="workspace-actions">{@render workspaceControls()}</div>
                </div>
              {/if}
              {#if runtimeError}
                <div class="runtime-error">{runtimeError}</div>
              {/if}
              <div
                class="workspace-stage"
                role="region"
                aria-label="Workflow panes"
                onfocusin={(event) => {
                  activateWorkspaceInputRegion("workflow");
                  focusedWorkflowInputElement =
                    event.target instanceof HTMLElement ? event.target : null;
                }}
                onfocusout={(event) => deactivateWorkspaceInputRegion("workflow", event)}
                ondragover={handleWorkflowDragOver}
                ondrop={handleWorkflowDrop}
              >
                {#if !runtimeLive}
                  <div class="state-message">
                    <Spinner size={18} />
                    <span>Loading workspace runtime...</span>
                  </div>
                {:else}
                  {#if soleEmbeddedSessionHostKey !== null}
                    <div class="sole-embedded-session">
                      <SessionTerminalSlot
                        hostKey={soleEmbeddedSessionHostKey}
                        visible={hostVisible}
                      />
                    </div>
                  {:else if renderedWorkflowTree}
                    <WorkflowSplitTree
                      {workspaceId}
                      dragScope={surfaceLayout?.dragScope}
                      promotion={workflowPromotion}
                      node={renderedWorkflowTree}
                      tabs={workflowTabDescriptors}
                      {activeTabKey}
                      inputActive={workspaceContainerInputActive && renderedWorkspaceInputRegion === "workflow"}
                      disabled={actionsBlocked}
                      onSelectTab={handleWorkflowTabActivation}
                      onFocusPane={handleWorkflowPaneFocus}
                      onMoveTabBefore={moveWorkflowTabBeforeTarget}
                      onAppendTabToLeaf={appendWorkflowTabToGroup}
                      onSplitTab={splitWorkflowTabIntoGroup}
                      onMoveTabToTerminal={moveWorkflowTabToTerminal}
                      onCloseTab={closeWorkflowTab}
                      onRenameTab={renameWorkflowTab}
                      onRatioChange={(splitId, ratio) => {
                        terminalLayout = {
                          ...terminalLayout,
                          workflowTree: updateWorkflowSplitRatio(
                            terminalLayout.workflowTree,
                            splitId,
                            ratio,
                          ),
                        };
                      }}
                    >
                      {#snippet renderTab(tabKey, active)}
                        {#if tabKey === "home"}
                          {#if workspace}
                            <WorkspaceHome
                              {workspace}
                              launchTargets={launchTargets}
                              sessions={runtimeSessions}
                              displayLabels={sessionDisplayLabels}
                              {launchingKey}
                              readonly={actionsBlocked}
                              onLaunch={(key) => void handleLaunch(key)}
                              onOpenSession={openSession}
                            />
                          {/if}
                        {:else if tabKey === "terminal" && terminalPanelInStage}
                          <DockedTerminalPanel
                            {workspaceId}
                            {workspaceHostKey}
                            dragScope={surfaceLayout?.dragScope}
                            paneKeyForSession={detailPaneKeyForSession}
                            sessions={terminalSessions}
                            displayLabels={sessionDisplayLabels}
                            tree={dockTree}
                            activeSessionKey={terminalLayout.activeSessionKey}
                            open={terminalLayout.open}
                            dock={terminalLayout.dock}
                            height={terminalLayout.height}
                            loading={terminalLaunching}
                            disabled={actionsBlocked}
                            hostVisible={active && hostVisible}
                            onToggle={() => void toggleTerminalPanel()}
                            onNewTerminal={() => void launchTerminalSession()}
                            onSplit={(direction) => void splitTerminal(direction)}
                            onSelect={selectTerminalSession}
                            onClose={(session) => void closeSession(session)}
                            onRename={renameSession}
                            onMoveToWorkflow={moveSessionToWorkflow}
                            readSessionDrag={readDroppedSession}
                            onPromoteSession={promoteSessionToPane}
                            onDock={dockTerminalPanel}
                            onResize={resizeTerminalPanel}
                            onDropSession={moveSessionToTerminal}
                            onSplitSession={splitTerminalSessionIntoPane}
                            onRatioChange={(splitId, ratio) => {
                              updateActiveTerminalTree(
                                updateSplitRatio(
                                  terminalLayout.tree,
                                  splitId,
                                  ratio,
                                ),
                              );
                            }}
                          />
                        {:else}
                          {@const sessionKey = sessionKeyFromWorkflowTab(tabKey)}
                          {@const session = runtimeSessions.find(
                            (candidate) => candidate.key === sessionKey,
                          )}
                          {#if session && isSessionTerminalMounted(session.key)}
                            <SessionTerminalSlot
                              hostKey={sessionHostKeyFor(session)}
                              visible={active && hostVisible}
                            />
                          {/if}
                        {/if}
                      {/snippet}
                    </WorkflowSplitTree>
                  {/if}
                {/if}
              </div>
              <!-- Kept even in a chrome-free pane. The header bar and the one-tab
                   workflow strip are chrome that only restated what the pane's own tab
                   already said, but the dock is a surface: it is the only route to a
                   second session, and a workspace running one agent with no way to
                   open a shell beside it is a dead end, not a simplification. -->
              {#if terminalLayout.dock === "bottom" &&
                !soleEmbeddedSessionIsDocked &&
                !workspacePaneEmpty &&
                !workspacePaneRowOnly}
                {@render workspaceDockRowBody(hostVisible, false)}
              {/if}
            </div>
          </div>
          {#if sidebarOpen && !hideRightSidebar}
            <SplitResizeHandle
              class="sidebar-resize-handle"
              ariaLabel="Resize workspace details"
              orientation="horizontal"
              ariaValueMin={rightSidebarAriaMin}
              ariaValueMax={rightSidebarAriaMax}
              ariaValueNow={renderedRightSidebarWidth}
              disabled={rightSidebarResizeDisabled}
              onResizeStart={handleSidebarResizeStart}
              onResize={handleSidebarResize}
            />
            <div
              class={[
                "right-sidebar",
                {
                  "input-active": workspaceContainerInputActive && renderedWorkspaceInputRegion === "details",
                },
              ]}
              style="width: {renderedRightSidebarWidth}px"
              role="region"
              aria-label="Workspace details pane"
              onfocusin={() => activateWorkspaceInputRegion("details")}
              onfocusout={(event) => deactivateWorkspaceInputRegion("details", event)}
            >
              {#if workspaceDetailsReady && workspace}
                <WorkspaceRightSidebar
                  activeTab={sidebarTab}
                  workspaceID={workspace.id}
                  {workspaceHostKey}
                  provider={workspace.repo.provider}
                  platformHost={workspace.repo.platform_host}
                  repoOwner={workspace.repo.owner}
                  repoName={workspace.repo.name}
                  repoPath={workspace.repo.repo_path}
                  ownerItemType={workspace.item_type}
                  ownerItemNumber={workspace.item_number}
                  associatedPRNumber={getWorkspacePRNumber(workspace)}
                  branch={workspace.git_head_ref}
                  roborevBaseUrl={basePath + "/api/roborev"}
                  refreshToken={sidebarRefreshToken}
                  {diffRefreshToken}
                  disabled={actionsBlocked}
                />
              {:else}
                <div class="state-message">
                  <Spinner size={18} />
                  <span>Loading workspace details...</span>
                </div>
              {/if}
            </div>
          {/if}
        </div>
      {/if}
    </div>
  {/snippet}

  <CollapsibleSidebar
    isCollapsed={isSidebarCollapsed}
    hideSidebar={hideWorkspaceList}
    sidebarWidth={currentWorkspaceListWidth}
    minSidebarWidth={MIN_WORKSPACE_LIST_WIDTH}
    maxSidebarWidth={MAX_WORKSPACE_LIST_WIDTH}
    onSidebarResize={handleWorkspaceListResize}
    overlay={isNarrow()}
    showCollapsedStrip={isSidebarToggleEnabled}
    onExpand={onToggleSidebar}
    mainOverflow="hidden"
  >
    {#snippet sidebar()}
      <WorkspaceListSidebar
        selectedId={workspaceId}
        selectedHostKey={workspaceHostKey}
        {selectedRepos}
        {isSidebarToggleEnabled}
        {hostVisible}
        onCollapseSidebar={onToggleSidebar}
        onOpenItemSidebar={openItemSidebar}
        onWorkspaceListStateChange={updateWorkspaceListState}
        isWorkspaceActionDisabled={(id, hostKey) =>
          deletingWorkspaceTargets.some((target) =>
            isDeletingWorkspaceTarget(target, id, hostKey),
          )}
        onWorkspaceDeletePendingChange={(id, hostKey, pending) => {
          if (pending) {
            addDeletingWorkspaceTarget(id, hostKey);
          } else {
            removeDeletingWorkspaceTarget(id, hostKey);
          }
        }}
        {onWorkspaceDeleted}
      />
    {/snippet}
    {@render terminalMainContent()}
  </CollapsibleSidebar>
</div>

{#if launcherMode && workspace !== null && launcherOverlayAllowed}
  <WorkspaceLauncherOverlay
    open={launcherOpen && interactionVisible}
    {workspace}
    launchTargets={launchTargets}
    sessions={runtimeSessions}
    displayLabels={sessionDisplayLabels}
    {launchingKey}
    readonly={actionsBlocked}
    onClose={closeLauncher}
    onLaunch={(key) => void handleLaunch(key)}
    onOpenSession={(sessionKey) => {
      closeLauncher();
      openSession(sessionKey);
    }}
  />
{/if}

{#if renamePrompt !== null && interactionVisible}
  <Modal
    open={renamePrompt !== null && interactionVisible}
    title="Rename tab"
    width={460}
    frameId="workspace-rename-session"
    onClose={cancelRenamePrompt}
  >
    <form
      id="rename-session-form"
      class="rename-form"
      onsubmit={(event) => {
        event.preventDefault();
        void saveRenamePrompt();
      }}
    >
      <p class="rename-message">
        Choose the label shown in the workflow and terminal panes.
      </p>
      <label class="rename-field">
        <span>Name</span>
        <input
          bind:this={renameInputEl}
          bind:value={renameInputValue}
          autocomplete="off"
          spellcheck="false"
        />
      </label>
    </form>

    {#snippet footer()}
      <DialogButton disabled={renameSaving} onclick={cancelRenamePrompt}>
        Cancel
      </DialogButton>
      <DialogButton
        type="submit"
        form="rename-session-form"
        tone="primary"
        disabled={renameSaving || renameInputValue.trim() === ""}
      >
        {renameSaving ? "Saving..." : "Save"}
      </DialogButton>
    {/snippet}
  </Modal>
{/if}

<ConfirmDialog
  open={stopPromptSession !== null && interactionVisible}
  title={stopPromptSession
    ? `Stop ${stopPromptSession.label}?`
    : "Stop session?"}
  message="This will stop the running session and close its pane."
  hint="Any foreground command running inside this session may be interrupted."
  confirmLabel="Stop session"
  pendingLabel="Stopping…"
  busy={stopSessionStopping}
  tone="danger"
  frameId="workspace-stop-session"
  onCancel={cancelStopSession}
  onConfirm={() => void confirmStopSession()}
/>

<ConfirmDialog
  open={deletePromptOpen && interactionVisible}
  title="Delete workspace?"
  message={workspace
    ? `This removes the worktree and tmux session for ${workspace.git_head_ref}.`
    : "This removes the worktree and tmux session."}
  hint="Commits that exist nowhere but this worktree are lost with it. Uncommitted changes are refused and prompt again."
  confirmLabel="Delete workspace"
  tone="danger"
  frameId="workspace-delete"
  onCancel={cancelDeletePrompt}
  onConfirm={confirmDeletePrompt}
/>

<ConfirmDialog
  open={forcePromptMessage !== null && interactionVisible}
  title="Force delete workspace?"
  message={forcePromptMessage ?? ""}
  hint="Force-deleting discards any uncommitted changes in the worktree. This cannot be undone."
  confirmLabel="Force delete"
  pendingLabel="Deleting…"
  busy={forceDeleting}
  tone="danger"
  frameId="workspace-force-delete"
  onCancel={cancelForceDelete}
  onConfirm={() => void confirmForceDelete()}
/>

<!-- The dock body has one definition for its internal and surface-hosted placements.
     The internal copy follows the workspace host's visibility; the external copy is
     visible even while that host is parked because the container pane retired. -->
{#snippet workspaceDockRowBody(dockHostVisible: boolean, external: boolean)}
  <DockedTerminalPanel
                {workspaceId}
                {workspaceHostKey}
                dragScope={surfaceLayout?.dragScope}
                paneKeyForSession={detailPaneKeyForSession}
                headerActions={external ? workspaceDockHeaderActions : undefined}
                sessions={terminalSessions}
                displayLabels={sessionDisplayLabels}
                tree={dockTree}
                activeSessionKey={terminalLayout.activeSessionKey}
                open={terminalLayout.open}
                dock={terminalLayout.dock}
                height={terminalLayout.height}
                loading={terminalLaunching}
                disabled={actionsBlocked}
                hostVisible={dockHostVisible}
                inputActive={external
                  ? (surfaceLayout?.externalInputActive() ?? false)
                  : workspaceContainerInputActive && renderedWorkspaceInputRegion === "terminal"}
                onInputActivate={() => {
                  activateWorkspaceInputRegion("terminal");
                  if (external) surfaceLayout?.setExternalInputActive(true);
                }}
                onInputDeactivate={() => {
                  if (workspaceInputRegion === "terminal") workspaceInputRegion = null;
                  if (external) surfaceLayout?.setExternalInputActive(false);
                }}
                onToggle={() => void toggleTerminalPanel()}
                onNewTerminal={() => void launchTerminalSession()}
                onSplit={(direction) => void splitTerminal(direction)}
                onSelect={selectTerminalSession}
                onClose={(session) => void closeSession(session)}
                onRename={renameSession}
                onMoveToWorkflow={moveSessionToWorkflow}
                readSessionDrag={readDroppedSession}
                onPromoteSession={promoteSessionToPane}
                onDock={dockTerminalPanel}
                onResize={resizeTerminalPanel}
                onDropSession={moveSessionToTerminal}
                onSplitSession={splitTerminalSessionIntoPane}
                onRatioChange={(splitId, ratio) => {
                  updateActiveTerminalTree(
                    updateSplitRatio(
                      terminalLayout.tree,
                      splitId,
                      ratio,
                    ),
                  );
                }}
              />
{/snippet}

<!-- Registered with the surface, which only renders it while the container pane is
     retired. Its own visible placement, not the parked host wrapper, owns whether
     terminal slots may attach. -->
{#snippet workspaceDockRow()}
  {@render workspaceDockRowBody(true, true)}
{/snippet}

{#snippet workspaceDockHeaderActions()}
  <WorkspacePaneControls showStripActions={true} />
{/snippet}

<!-- The workspace's own controls, defined here because every one of them is wired
     to this view's state. In a detail pane the pane's popover renders this, so the
     controls follow the workspace without the state leaving the view. -->
{#snippet workspaceControls()}
  {#if controlsInPane && inlineDock && inlineDockMode !== null && workspaceLive && workspace?.status === "ready"}
    <!-- The dock's own modes, which the header bar carries everywhere it still
         renders - so this copy is gated on exactly the case that hides it. A detail
         pane's close button is not a replacement: it puts one pane away, while a
         session the user promoted into a sibling pane stays on screen, so the
         workspace is still there while the button claims it is gone. Collapse is
         the only control that reaches every pane the workspace occupies. -->
    <Button
      size="sm"
      surface="soft"
      tone="neutral"
      label={inlineDockMode === "expanded" ? "Show Details" : "Expand Terminal"}
      disabled={inlineDockMode !== "expanded" && inlineDockExpandBlocked}
      title={
        inlineDockMode !== "expanded" && inlineDockExpandBlocked
          ? "Close the open dialog first."
          : undefined
      }
      onclick={() => inlineDock?.setMode(inlineDockMode === "expanded" ? "split" : "expanded")}
    >
      {#if inlineDockMode === "expanded"}
        <ChevronsDownIcon size="13" strokeWidth="2" aria-hidden="true" />
      {:else}
        <ChevronsUpIcon size="13" strokeWidth="2" aria-hidden="true" />
      {/if}
    </Button>
    <Button
      size="sm"
      surface="soft"
      tone="neutral"
      label="Collapse Terminal"
      onclick={() => inlineDock?.setMode("collapsed")}
    >
      <PanelBottomCloseIcon size="13" strokeWidth="2" aria-hidden="true" />
    </Button>
  {/if}
  {#if !launcherMode}
    <!-- Presets compose a whole multi-session workflow, which is what the standalone
         Workspaces tab is for. A PR or issue pane hosts one workspace beside the
         thing being reviewed, so saving and applying layouts there is a surface the
         maintainer never asked that pane for. -->
    <WorkflowPresetMenu
      presets={workflowPresets}
      selectedPresetId={selectedWorkflowPresetId}
      applying={applyingWorkflowPreset}
      onSaveNew={saveWorkflowPreset}
      onUpdate={updateWorkflowPreset}
      onApply={(presetId) => void applyWorkflowPreset(presetId)}
      onDelete={deleteWorkflowPreset}
      disabled={actionsBlocked}
      hostVisible={interactionVisible}
    />
  {/if}
  <TerminalZoomControl
    fontSize={terminalFontSize}
    disabled={actionsBlocked || !terminalSettingsReady || terminalOptionsSaving}
    onDecrease={terminalZoom.decrease}
    onIncrease={terminalZoom.increase}
    onReset={terminalZoom.reset}
  />
  <TerminalOptionsMenu
    disabled={actionsBlocked || !terminalSettingsReady || terminalZoomSaving}
    hostVisible={interactionVisible}
    onSavingChange={(saving) => {
      terminalOptionsSaving = saving;
    }}
  />
  {#if launcherMode}
    {#if soleEmbeddedSession !== null}
      <!-- The chrome that carried these is gone in this state, and only in this
           state: with the header bar or the session strip on screen they already
           have an owner there, and a second copy with its own disabled and pending
           behaviour is worse than none. Delete is not among them - it moved to the
           strip, where it is one click in every pane. -->
      <Button
        size="sm"
        surface="soft"
        tone="neutral"
        label="Rename session"
        disabled={actionsBlocked}
        onclick={() => renameSession(soleEmbeddedSession)}
      />
      <Button
        size="sm"
        surface="soft"
        tone="neutral"
        label="Stop session"
        disabled={actionsBlocked}
        onclick={() => closeSession(soleEmbeddedSession)}
      />
    {/if}
    {#if workspace}
      <code class="workspace-control-branch">{workspace.git_head_ref}</code>
    {/if}
    <!-- One opener rather than the menu: the overlay is the launch surface in a
         pane, and a second copy of the target list inside a popover inside a tab
         strip is the stacking this mode exists to remove. -->
    <Button size="sm" surface="soft" tone="neutral" label="Launch session" onclick={openLauncher}>
      <PlayIcon size="13" strokeWidth="2" aria-hidden="true" />
    </Button>
  {:else}
    <LaunchMenu
      launchTargets={launchTargets}
      {launchingKey}
      disabled={actionsBlocked}
      hostVisible={interactionVisible}
      onLaunch={(key) => void handleLaunch(key)}
    />
  {/if}
{/snippet}

<!-- Sits in every related pane's tab strip. Launching is non-destructive and useful
     from a promoted session too, so it must not disappear with owner-only actions. -->
{#snippet workspacePaneActions()}
  <!-- Ready only. A workspace whose setup failed renders its own actions beside the
       Retry in the error panel, which is where the user is already looking, and one
       still being created cannot launch yet. -->
  {#if workspaceLive && workspace?.status === "ready"}
    <IconButton
      size="sm"
      tone="neutral"
      disabled={actionsBlocked}
      ariaLabel="Launch session"
      title="Launch session"
      onclick={openLauncher}
    >
      <PlayIcon size="13" strokeWidth="2" aria-hidden="true" />
    </IconButton>
  {/if}
{/snippet}

<!-- The workspace pane alone owns destructive strip actions. A workspace split
     across promoted session panes must still present exactly one Delete button. -->
{#snippet workspaceStripActions()}
  {#if workspaceLive && workspace?.status === "ready"}
    <IconButton
      size="sm"
      tone="danger"
      disabled={actionsBlocked}
      ariaLabel={`Delete workspace ${workspace.git_head_ref}`}
      title={`Delete workspace ${workspace.git_head_ref}`}
      onclick={(event) => void handleDelete(event.currentTarget as HTMLElement)}
    >
      <Trash2Icon size="13" strokeWidth="2" aria-hidden="true" />
    </IconButton>
  {/if}
{/snippet}

<style>
  .terminal-view {
    display: flex;
    width: 100%;
    height: 100%;
    background: var(--bg-primary);
  }

  .terminal-main {
    flex: 1;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    min-width: 0;
  }

  .state-message {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-4);
    flex: 1;
    color: var(--text-muted);
    font-size: var(--font-size-lg);
  }

  .state-message.error {
    color: var(--accent-red);
  }

  .workspace-zero-state {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    justify-content: center;
    gap: var(--space-6);
    flex: 1;
    min-width: 0;
    overflow: auto;
    padding: 56px 28px;
    color: var(--text-primary);
    background: var(--bg-primary);
    max-width: 560px;
    margin: 0 auto;
  }

  .workspace-zero-copy {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .workspace-zero-eyebrow {
    margin: 0;
    color: var(--accent-green);
    font-size: var(--font-size-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }

  .workspace-zero-copy h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-xl);
    font-weight: 650;
    line-height: 1.25;
  }

  .workspace-zero-copy p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    line-height: 1.5;
  }

  .workspace-zero-inline-action {
    display: inline-flex;
    vertical-align: middle;
    margin-left: 5px;
    transform: translateY(-1px);
  }

  .workspace-zero-example-card {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 12px;
    align-self: flex-start;
    min-width: 280px;
    width: min(620px, 100%);
    padding: 12px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-surface);
    box-shadow: var(--shadow-lg);
  }

  .workspace-zero-example {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    line-height: 1.5;
  }

  .workspace-zero-example-label {
    align-self: flex-start;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .workspace-zero-inline-action :global(.workspace-zero-create-button:disabled) {
    cursor: default;
    opacity: var(--opacity-disabled);
  }

  .workspace-zero-example-empty {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .workspace-zero-example :global(.workspace-home) {
    width: 100%;
    height: auto;
    padding: 0;
    overflow: visible;
    background: transparent;
  }

  @media (max-width: 760px) {
    .workspace-zero-state {
      padding: 28px 18px;
      max-width: none;
    }

    .workspace-zero-example-card {
      min-width: 0;
      width: 100%;
    }
  }

  .error-icon-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    border-radius: 50%;
    background: var(--accent-red);
    color: var(--text-on-accent);
    font-size: var(--font-size-md);
    font-weight: 700;
    flex-shrink: 0;
  }

  :global(.error-icon) {
    display: block;
    width: 14px;
    height: 14px;
    overflow: visible;
  }

  .retry-btn {
    padding: 4px 12px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--bg-surface-hover);
  }

  .retry-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: wait;
  }

  .retry-btn.danger:hover {
    background: var(--accent-red);
    border-color: var(--accent-red);
    color: var(--text-on-accent);
  }

  .header-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: 34px;
    padding: 0 10px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
    border-left: 1px solid var(--border-default);
    gap: var(--space-4);
    flex-shrink: 0;
  }

  .header-start {
    display: flex;
    align-items: center;
    gap: 8px;
    overflow: hidden;
  }

  .header-name {
    font-size: var(--font-size-md);
    font-weight: 600;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    letter-spacing: 0.005em;
  }

  .header-branch {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    background: var(--bg-inset);
    padding: 1px 6px;
    border-radius: 3px;
    border: 1px solid var(--border-muted);
    white-space: nowrap;
    line-height: 1.5;
  }

  .header-end {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-shrink: 0;
  }

  .header-btn {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 22px;
    padding: 0 10px;
    border: 1px solid var(--border-default);
    border-radius: 3px;
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 500;
    cursor: pointer;
    transition: background-color 80ms ease, color 80ms ease,
      border-color 80ms ease;
  }

  :global(.workspace-refresh-button.kit-icon-button) {
    border: 1px solid var(--border-default);
  }

  :global(.header-icon) {
    display: block;
  }

  .header-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    border-color: color-mix(in srgb, var(--text-muted) 40%, var(--border-default));
  }

  .header-btn:disabled {
    cursor: not-allowed;
    opacity: var(--opacity-disabled);
  }

  .header-btn.danger:hover:not(:disabled) {
    background: var(--accent-red);
    color: var(--text-on-accent);
    border-color: var(--accent-red);
  }

  .terminal-area {
    flex: 1;
    overflow: hidden;
  }

  .workspace-surface {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-width: 0;
    background: var(--bg-primary);
  }

  .workspace-toolbar {
    display: flex;
    align-items: stretch;
    justify-content: space-between;
    gap: var(--space-4);
    height: 30px;
    padding: 0 6px 0 0;
    border-bottom: 1px solid var(--border-default);
    border-left: 1px solid var(--border-default);
    background: var(--bg-inset);
    flex-shrink: 0;
  }

  .workspace-toolbar-title {
    display: inline-flex;
    align-items: center;
    padding: 0 10px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }

  .workspace-actions {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-shrink: 0;
    padding-left: 6px;
    border-left: 1px solid var(--border-muted);
  }

  .runtime-error {
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-default);
    background: color-mix(in srgb, var(--accent-red) 12%, var(--bg-surface));
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .workspace-stage {
    position: relative;
    flex: 1;
    min-height: 0;
    overflow: hidden;
    background: var(--bg-primary);
  }

  .sole-embedded-session {
    display: flex;
    width: 100%;
    height: 100%;
    min-width: 0;
    min-height: 0;
  }

  .workspace-control-branch {
    max-width: 240px;
    overflow: hidden;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .panel-toggle-group {
    display: inline-flex;
    height: 22px;
    border: 1px solid var(--border-default);
    border-radius: 3px;
    overflow: hidden;
    background: var(--bg-surface);
  }

  .panel-toggle-btn {
    display: inline-flex;
    align-items: center;
    padding: 0 10px;
    border: none;
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 500;
    letter-spacing: 0.01em;
    cursor: pointer;
    font-family: inherit;
    transition: background-color 80ms ease, color 80ms ease;
  }

  .panel-toggle-btn + .panel-toggle-btn {
    border-left: 1px solid var(--border-default);
  }

  .panel-toggle-btn:hover:not(.active):not(:disabled) {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .panel-toggle-btn.active:not(:disabled) {
    background: var(--accent-blue);
    color: var(--text-on-accent);
    font-weight: 600;
  }

  .panel-toggle-btn:disabled {
    cursor: not-allowed;
    color: color-mix(in srgb, var(--text-muted) 75%, var(--bg-surface));
    background: var(--bg-surface);
    opacity: unset;
  }

  .panel-toggle-group .panel-toggle-btn.active:disabled {
    background: color-mix(in srgb, var(--text-muted) 28%, var(--bg-surface)) !important;
    color: color-mix(in srgb, var(--text-muted) 80%, var(--text-primary)) !important;
    box-shadow: inset 0 0 0 1px
      color-mix(in srgb, var(--text-muted) 35%, var(--border-muted));
  }

  .terminal-and-sidebar {
    flex: 1;
    display: flex;
    overflow: hidden;
  }

  .right-sidebar {
    position: relative;
    z-index: 2;
    flex-shrink: 0;
    overflow: hidden;
  }

  .right-sidebar.input-active::after {
    content: "";
    position: absolute;
    inset: 0;
    z-index: 90;
    border: var(--chrome-border-width) solid
      color-mix(in srgb, var(--accent-blue) 48%, var(--border-default));
    pointer-events: none;
  }

  .right-sidebar:has(:global(.kit-modal-overlay)) {
    z-index: 80;
  }


  .rename-form {
    display: grid;
    gap: 12px;
    margin: 0;
  }

  .rename-message {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }

  .rename-field {
    display: grid;
    gap: 6px;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .rename-field input {
    width: 100%;
    height: 34px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    color: var(--text-primary);
    padding: 0 10px;
    font: inherit;
    font-size: var(--font-size-sm);
    outline: none;
  }

  .rename-field input:focus {
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 22%, transparent);
  }

</style>
