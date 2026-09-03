<script lang="ts">
  import { Modal, SelectDropdown, Spinner, type SelectDropdownOption } from "@kenn-io/kit-ui";
  import ArrowDownIcon from "@lucide/svelte/icons/arrow-down";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import ArrowRightIcon from "@lucide/svelte/icons/arrow-right";
  import ArrowUpIcon from "@lucide/svelte/icons/arrow-up";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import ChevronUpIcon from "@lucide/svelte/icons/chevron-up";
  import CornerDownLeftIcon from "@lucide/svelte/icons/corner-down-left";
  import KeyboardIcon from "@lucide/svelte/icons/keyboard";
  import MoreHorizontalIcon from "@lucide/svelte/icons/ellipsis";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SpaceIcon from "@lucide/svelte/icons/space";
  import SquareIcon from "@lucide/svelte/icons/square";
  import { Effect, Option } from "effect";
  import { tick, untrack } from "svelte";
  import type { RuntimeSession, TerminalSettings as TerminalSettingsType } from "../../api/types.js";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { ApiProblemError } from "../../api/effect-errors.js";
  import { ProblemCodes } from "../../api/problems.js";
  import {
    workspaceSessionWebSocketPath,
    workspaceTmuxWebSocketPath,
    type WorkspaceRuntimeState,
  } from "../../api/workspace-runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { getStores } from "../../context.js";
  import {
    acceptWorkspaceLaunch,
    claimWorkspaceLaunch,
    completeAcceptedWorkspaceLaunch,
    discardWorkspaceLaunch,
    failWorkspaceLaunch,
    pendingWorkspaceLaunch,
    type WorkspaceLaunchClaim,
  } from "../../stores/workspace-create-pending.svelte.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";
  import {
    noteSessionDiscarded,
    noteSessionMounted,
    noteSessionReleased,
    onSessionExited,
    requestSessionFocus,
    sendSessionKey,
    sendSessionPastedInput,
    sessionHostKey,
    type SessionHostKey,
  } from "../../stores/session-host.svelte.js";
  import LaunchTargetName from "../terminal/LaunchTargetName.svelte";
  import SessionTerminalSlot from "../terminal/SessionTerminalSlot.svelte";
  import type { TerminalKey } from "../terminal/terminal-key.js";
  import TerminalSettings from "../settings/TerminalSettings.svelte";
  import ConfirmDialog from "../shared/ConfirmDialog.svelte";
  import type { WorkspaceDetail } from "../terminal/workspace-detail.js";
  import {
    makeWorkspaceRuntimeOwner,
    makeWorkspaceRuntimePresenterID,
    WorkspaceRuntimeWorkflow,
    type WorkspaceRuntimeMutationState,
    type WorkspaceRuntimeTarget,
  } from "../terminal/workspace-runtime-workflow.js";
  import {
    loadMobileWorkspaceDetail,
    mobileWorkspaceIdentity,
    mobileWorkspaceLinkedItem,
  } from "./mobile-workspace-detail.js";
  import {
    loadMobileWorkspaceSession,
    saveMobileWorkspaceSession,
    selectMobileWorkspaceSession,
  } from "./mobile-workspace-session.js";

  interface Props {
    workspaceId: string;
    hostKey?: string | undefined;
    visible?: boolean;
    onBack: () => void;
    onMissing: () => void;
    onOpenItem: () => void;
  }

  interface MobileTerminalSession {
    key: string;
    label: string;
    status: string;
    generation: string;
    websocketPath: string;
    cursorWheelInput: boolean;
    runtimeSession: RuntimeSession | null;
  }

  let { workspaceId, hostKey = undefined, visible = true, onBack, onMissing, onOpenItem }: Props = $props();
  const appRuntime = getAppRuntime();
  const { settings: settingsStore } = getStores();
  const runtimeOwner = makeWorkspaceRuntimeOwner("mobile-workspace");
  const presenterID = makeWorkspaceRuntimePresenterID();

  let workspace = $state.raw<WorkspaceDetail | null>(null);
  let runtime = $state.raw<WorkspaceRuntimeState | null>(null);
  let selectedSessionKey = $state<string | null>(null);
  let provisionalBaseSelection = false;
  let ownedHostKeys: SessionHostKey[] = [];
  let loadError = $state<string | null>(null);
  let runtimeError = $state<string | null>(null);
  let launchingTarget = $state<string | null>(null);
  let stoppingSession = $state<string | null>(null);
  let retryingSetup = $state(false);
  let stopSession = $state.raw<RuntimeSession | null>(null);
  let launchSheetOpen = $state(false);
  let terminalOptionsOpen = $state(false);
  let terminalOptionsSaving = $state(false);
  let terminalSettingsDialogOpen = $state(false);
  let terminalSettings = $state<TerminalSettingsType>(settingsStore.getTerminalSettings());
  let composedInput = $state("");
  let inputError = $state<string | null>(null);
  let composerInput = $state<HTMLTextAreaElement | null>(null);
  let composerOpen = $state(false);
  let specialKeysOpen = $state(false);
  let composerDragStartY: number | null = null;

  const sessions = $derived(mobileTerminalSessions(runtime, workspace));
  const launchTargets = $derived(runtime?.launch_targets ?? []);
  const sessionOptions = $derived<SelectDropdownOption[]>(
    sessions.map((session) => ({ value: session.key, label: session.label })),
  );
  const selectedSession = $derived(
    sessions.find((session) => session.key === selectedSessionKey) ?? null,
  );
  const selectedHostKey = $derived(
    selectedSession ? pooledHostKey(selectedSession) : null,
  );
  const pendingLaunch = $derived(pendingWorkspaceLaunch(workspaceId, hostKey));
  const linkedItem = $derived(workspace ? mobileWorkspaceLinkedItem(workspace) : null);
  const workspaceIdentity = $derived(
    workspace
      ? mobileWorkspaceIdentity(workspace, hostKey)
      : hostKey
        ? `Fleet · ${hostKey}`
        : "Workspace",
  );

  function target(): WorkspaceRuntimeTarget {
    return { workspaceId, ...(hostKey === undefined ? {} : { hostKey }) };
  }

  function baseSessionKey(id: string): string {
    return `${id}:@workspace`;
  }

  function mobileTerminalSessions(
    runtimeState: WorkspaceRuntimeState | null,
    detail: WorkspaceDetail | null,
  ): MobileTerminalSession[] {
    const launched = (runtimeState?.sessions ?? []).map((session) => ({
      key: session.key,
      label: session.label,
      status: session.status,
      generation: session.created_at,
      websocketPath: workspaceSessionWebSocketPath(workspaceId, session.key, hostKey),
      cursorWheelInput: session.kind === "agent",
      runtimeSession: session,
    }));
    if (detail?.status !== "ready" || hostKey !== undefined) return launched;
    return [
      ...launched,
      {
        key: baseSessionKey(detail.id),
        label: "Workspace",
        status: "running",
        generation: detail.created_at,
        websocketPath: workspaceTmuxWebSocketPath(workspaceId, hostKey),
        cursorWheelInput: false,
        runtimeSession: null,
      },
    ];
  }

  function pooledHostKey(session: MobileTerminalSession): SessionHostKey {
    return sessionHostKey(workspaceId, hostKey, session.key, session.generation);
  }

  function failureMessage(failure: unknown, fallback: string): string {
    if (failure instanceof ApiProblemError) return apiErrorMessage(failure.problem, fallback);
    return failure instanceof Error ? failure.message : fallback;
  }

  function handleWorkspaceMissing(): void {
    onMissing();
  }

  function releaseOwnedSessions(): void {
    for (const key of ownedHostKeys) noteSessionReleased(key);
    ownedHostKeys = [];
  }

  function reconcilePooledSessions(nextSessions: readonly MobileTerminalSession[]): void {
    const desiredKeys = nextSessions.map(pooledHostKey);
    const desired = new Set(desiredKeys);
    for (const session of nextSessions) {
      const pooledKey = pooledHostKey(session);
      noteSessionMounted({
        hostKey: pooledKey,
        websocketPath: session.websocketPath,
        status: session.status,
        cursorWheelInput: session.cursorWheelInput,
      });
    }
    for (const key of ownedHostKeys) {
      if (!desired.has(key)) noteSessionDiscarded(key);
    }
    ownedHostKeys = desiredKeys;
  }

  function applyVisibleSessions(
    nextSessions: readonly MobileTerminalSession[],
    {
      previousSelectedHostKey = selectedHostKey,
      preferred = selectedSessionKey ?? loadMobileWorkspaceSession(workspaceId, hostKey),
      persist = true,
    }: {
      previousSelectedHostKey?: SessionHostKey | null;
      preferred?: string | null;
      persist?: boolean;
    } = {},
  ): void {
    reconcilePooledSessions(nextSessions);
    const nextSelectedSessionKey = selectMobileWorkspaceSession(nextSessions, preferred);
    const nextSelectedSession = nextSessions.find((session) => session.key === nextSelectedSessionKey);
    const nextSelectedHostKey = nextSelectedSession ? pooledHostKey(nextSelectedSession) : null;
    if (previousSelectedHostKey !== null && nextSelectedHostKey !== previousSelectedHostKey) {
      composedInput = "";
      inputError = null;
      composerOpen = false;
      specialKeysOpen = false;
    }
    selectedSessionKey = nextSelectedSessionKey;
    if (persist) saveMobileWorkspaceSession(workspaceId, hostKey, selectedSessionKey);
  }

  function applyWorkspace(next: WorkspaceDetail): void {
    workspace = next;
    if (next.status === "ready" && hostKey === undefined) {
      const nextSessions = mobileTerminalSessions(runtime, next);
      if (runtime === null) {
        applyVisibleSessions(nextSessions, {
          preferred: loadMobileWorkspaceSession(workspaceId, hostKey),
          persist: false,
        });
        provisionalBaseSelection = true;
      } else {
        applyVisibleSessions(nextSessions);
      }
    }
  }

  function applyRuntime(next: WorkspaceRuntimeState, authoritative = true): void {
    const previousSelectedHostKey = selectedHostKey;
    runtime = next;
    if (
      stopSession &&
      !next.sessions.some(
        (session) => session.key === stopSession?.key && session.created_at === stopSession?.created_at,
      )
    ) {
      stopSession = null;
    }
    const preferred = provisionalBaseSelection
      ? loadMobileWorkspaceSession(workspaceId, hostKey)
      : selectedSessionKey ?? loadMobileWorkspaceSession(workspaceId, hostKey);
    provisionalBaseSelection = false;
    applyVisibleSessions(mobileTerminalSessions(next, workspace), {
      previousSelectedHostKey,
      preferred,
    });
    runtimeError = null;
    if (authoritative) {
      for (const session of next.sessions) {
        completeAcceptedWorkspaceLaunch(workspaceId, hostKey, session.key);
      }
    }
    reconcileQueuedWorkspaceLaunch(next);
  }

  function reconcileQueuedWorkspaceLaunch(next: WorkspaceRuntimeState): void {
    const pending = pendingWorkspaceLaunch(workspaceId, hostKey);
    if (!pending || pending.phase === "awaiting_session") return;
    if (next.sessions.length > 0) {
      discardWorkspaceLaunch(workspaceId, hostKey);
      return;
    }
    const launchTarget = next.launch_targets.find((candidate) => candidate.key === pending.targetKey);
    if (!launchTarget || launchTarget.kind !== "agent" || !launchTarget.available) {
      if (discardWorkspaceLaunch(workspaceId, hostKey) !== null) {
        showFlash(
          `Agent "${pending.targetKey}" could not launch: ${launchTarget?.disabled_reason ?? "not available"}`,
          { tone: "danger" },
        );
      }
      return;
    }
    const claim = claimWorkspaceLaunch(workspaceId, hostKey);
    if (claim) launch(claim.targetKey, claim);
  }

  function requestSessionFocusForSelection(): void {
    const session = sessions.find((candidate) => candidate.key === selectedSessionKey);
    if (session) requestSessionFocus(pooledHostKey(session), { soft: true });
  }

  function readRuntime(force = false) {
    return Effect.gen(function* () {
      const workflow = yield* WorkspaceRuntimeWorkflow;
      const result = yield* workflow.read(runtimeOwner, workspaceId, hostKey, { force });
      if (Option.isSome(result)) {
        yield* Effect.sync(() => applyRuntime(result.value));
      }
    }).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          if (failure instanceof ApiProblemError && failure.problem.code === ProblemCodes.workspaceNotFound) {
            handleWorkspaceMissing();
            return;
          }
          runtimeError = failureMessage(failure, "Runtime unavailable");
        }),
      ),
    );
  }

  function loadWorkspaceAndRuntime() {
    return Effect.gen(function* () {
      const detail = yield* loadMobileWorkspaceDetail(workspaceId, hostKey);
      yield* Effect.sync(() => {
        applyWorkspace(detail);
        loadError = null;
      });
      if (detail.status === "ready") yield* readRuntime(true);
    }).pipe(
      Effect.catch((failure) =>
        Effect.sync(() => {
          if (failure instanceof ApiProblemError && failure.problem.code === ProblemCodes.workspaceNotFound) {
            handleWorkspaceMissing();
            return;
          }
          loadError = failureMessage(failure, hostKey ? "Fleet workspace unavailable" : "Workspace unavailable");
        }),
      ),
    );
  }

  function observeMutation(state: WorkspaceRuntimeMutationState): Effect.Effect<boolean> {
    return Effect.sync(() => {
      if (state.operation === "Launch") {
        if (state.kind === "pending") launchingTarget = state.request.targetKey;
        else if (state.kind === "succeeded") {
          launchingTarget = null;
          launchSheetOpen = false;
          selectedSessionKey = state.session.key;
          const current = runtime;
          if (current) {
            applyRuntime(
              {
                ...current,
                sessions: [...current.sessions.filter((session) => session.key !== state.session.key), state.session],
              },
              false,
            );
          } else {
            saveMobileWorkspaceSession(workspaceId, hostKey, selectedSessionKey);
          }
          requestRuntimeRefresh();
        } else if (state.kind === "failed" || state.kind === "uncertain") {
          launchingTarget = null;
          showMutationFailure(state, "Launch failed");
        }
      }
      if (state.operation === "Stop") {
        if (state.kind === "pending") stoppingSession = state.request.sessionKey;
        else if (state.kind === "succeeded") {
          stoppingSession = null;
          stopSession = null;
          const current = runtime;
          if (current) {
            applyRuntime({
              ...current,
              sessions: current.sessions.filter((session) => session.key !== state.request.sessionKey),
            });
          }
          requestRuntimeRefresh();
        } else if (state.kind === "failed" || state.kind === "uncertain") {
          stoppingSession = null;
          stopSession = null;
          showMutationFailure(state, "Stop failed");
        }
      }
      if (state.operation === "RetrySetup") {
        if (state.kind === "pending") {
          retryingSetup = true;
          runtimeError = null;
        } else if (state.kind === "succeeded") {
          retryingSetup = false;
          applyWorkspace(state.workspace);
          if (state.workspace.status === "ready") requestRuntimeRefresh();
        } else if (state.kind === "failed" || state.kind === "uncertain") {
          retryingSetup = false;
          showMutationFailure(state, "Retry failed");
        }
      }
      return true;
    });
  }

  function showMutationFailure(
    state: Extract<WorkspaceRuntimeMutationState, { kind: "failed" | "uncertain" }>,
    fallback: string,
  ): void {
    showFlash(failureMessage(state.error, fallback), { tone: "danger" });
  }

  function requestRuntimeRefresh(): void {
    appRuntime.runCommand(readRuntime(true), {
      operation: "refresh mobile workspace runtime",
      safeContext: { workspaceId, remote: Boolean(hostKey) },
      onFailure: () => undefined,
    });
  }

  function refreshWorkspaceState() {
    return Effect.suspend(() =>
      workspace?.status === "ready" ? readRuntime(true) : loadWorkspaceAndRuntime(),
    );
  }

  function retryWorkspaceSetup(): void {
    if (!workspace || workspace.status !== "error" || retryingSetup) return;
    retryingSetup = true;
    runtimeError = null;
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.retrySetup(target());
      }),
      {
        operation: "retry mobile workspace setup",
        safeContext: { workspaceId, remote: Boolean(hostKey) },
        onFailure: (failure) => {
          retryingSetup = false;
          showFlash(failureMessage(failure, "Retry failed"), { tone: "danger" });
        },
      },
    );
  }

  function selectSession(sessionKey: string): void {
    provisionalBaseSelection = false;
    selectedSessionKey = sessionKey;
    composedInput = "";
    inputError = null;
    composerOpen = false;
    specialKeysOpen = false;
    terminalOptionsOpen = false;
    saveMobileWorkspaceSession(workspaceId, hostKey, selectedSessionKey);
    requestSessionFocusForSelection();
  }

  function sendComposedInput(): void {
    const key = selectedHostKey;
    if (!key) return;
    if (!sendSessionPastedInput(key, composedInput, "\r")) {
      inputError = "Terminal is reconnecting. Try again in a moment.";
      return;
    }
    composedInput = "";
    inputError = null;
    void tick().then(resizeComposer);
  }

  function sendSpecialKey(keyName: TerminalKey): void {
    const key = selectedHostKey;
    if (!key) return;
    if (!sendSessionKey(key, keyName)) {
      inputError = "Terminal is reconnecting. Try again in a moment.";
    } else {
      inputError = null;
    }
    composerInput?.focus({ preventScroll: true });
  }

  function toggleSpecialKeys(): void {
    specialKeysOpen = !specialKeysOpen;
    composerInput?.focus({ preventScroll: true });
  }

  function resizeComposer(): void {
    const input = composerInput;
    if (!input) return;
    input.style.height = "auto";
    input.style.height = `${input.scrollHeight}px`;
  }

  function handleComposerInput(): void {
    inputError = null;
    resizeComposer();
  }

  function openComposer(): void {
    composerOpen = true;
    void tick().then(() => {
      resizeComposer();
      composerInput?.focus();
    });
  }

  function closeComposer(): void {
    composerInput?.blur();
    composerOpen = false;
    specialKeysOpen = false;
    inputError = null;
  }

  function startComposerDrag(event: PointerEvent): void {
    composerDragStartY = event.clientY;
    const handle = event.currentTarget;
    if (handle instanceof HTMLButtonElement) handle.setPointerCapture(event.pointerId);
  }

  function finishComposerDrag(event: PointerEvent, direction: "open" | "close"): void {
    const handle = event.currentTarget;
    if (handle instanceof HTMLButtonElement && handle.hasPointerCapture(event.pointerId)) {
      handle.releasePointerCapture(event.pointerId);
    }
    const startY = composerDragStartY;
    composerDragStartY = null;
    if (startY === null) return;
    const distance = event.clientY - startY;
    if (direction === "open" && distance < -24) openComposer();
    if (direction === "close" && distance > 24) closeComposer();
  }

  function launch(targetKey: string, launchClaim?: WorkspaceLaunchClaim): void {
    if (launchingTarget || stoppingSession || (!launchClaim && pendingLaunch)) return;
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.launch(target(), targetKey, "workflow", {
          _tag: "Workflow",
          ...(launchClaim
            ? {
                onSettled: (settlement) => {
                  if (settlement._tag === "Accepted") {
                    const acceptedAt = Date.now();
                    if (acceptWorkspaceLaunch(launchClaim, settlement.sessionKey, acceptedAt)) {
                      const label = launchTargets.find((candidate) => candidate.key === launchClaim.targetKey)?.label
                        ?? launchClaim.targetKey;
                      startAcceptedWorkspaceLaunchReconciliation(
                        launchClaim,
                        settlement.sessionKey,
                        acceptedAt,
                        label,
                      );
                    }
                  } else {
                    failWorkspaceLaunch(launchClaim);
                  }
                },
              }
            : {}),
        });
      }),
      {
        operation: "launch mobile workspace session",
        safeContext: { workspaceId, targetKey, remote: Boolean(hostKey) },
        onFailure: (failure) => {
          launchingTarget = null;
          if (launchClaim) failWorkspaceLaunch(launchClaim);
          showFlash(failureMessage(failure, "Launch failed"), { tone: "danger" });
        },
      },
    );
  }

  function startAcceptedWorkspaceLaunchReconciliation(
    launchClaim: WorkspaceLaunchClaim,
    sessionKey: string,
    acceptedAt: number,
    label: string,
  ): void {
    const acceptedTarget: WorkspaceRuntimeTarget = {
      workspaceId: launchClaim.workspaceId,
      ...(launchClaim.workspaceHostKey === undefined ? {} : { hostKey: launchClaim.workspaceHostKey }),
    };
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.reconcileAcceptedLaunch({
          target: acceptedTarget,
          sessionKey,
          acceptedAt,
          onExpired: Effect.sync(() => {
            if (workspaceId === launchClaim.workspaceId && hostKey === launchClaim.workspaceHostKey) {
              showFlash(`${label} launched, but its session did not become available`, { tone: "danger" });
            }
          }),
        });
      }),
      {
        operation: "reconcile accepted mobile workspace launch",
        safeContext: { workspaceId: launchClaim.workspaceId, remote: Boolean(launchClaim.workspaceHostKey) },
        onFailure: () => undefined,
      },
    );
  }

  function openTerminalOptions(): void {
    if (!workspace || workspace.status !== "ready") return;
    terminalSettings = settingsStore.getTerminalSettings();
    terminalOptionsSaving = false;
    terminalSettingsDialogOpen = false;
    terminalOptionsOpen = true;
  }

  function closeTerminalOptions(): void {
    if (terminalOptionsSaving || terminalSettingsDialogOpen) return;
    terminalOptionsOpen = false;
  }

  function openLaunchSheet(): void {
    if (terminalOptionsSaving) return;
    terminalOptionsOpen = false;
    launchSheetOpen = true;
  }

  function promptStopSelectedSession(): void {
    if (selectedSession?.runtimeSession && !stoppingSession && !launchingTarget) {
      terminalOptionsOpen = false;
      stopSession = selectedSession.runtimeSession;
    }
  }

  function stopSelectedSession(): void {
    const session = stopSession;
    if (!session || stoppingSession || launchingTarget) return;
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* WorkspaceRuntimeWorkflow;
        yield* workflow.stop(target(), session.key);
      }),
      {
        operation: "stop mobile workspace session",
        safeContext: { workspaceId, sessionKey: session.key, remote: Boolean(hostKey) },
        onFailure: (failure) => {
          stoppingSession = null;
          stopSession = null;
          showFlash(failureMessage(failure, "Stop failed"), { tone: "danger" });
        },
      },
    );
  }

  $effect(() => {
    if (!terminalOptionsOpen) return;
    return untrack(() => pushModalFrame("mobile-workspace-terminal-options", []));
  });

  $effect(() => {
    if (!launchSheetOpen) return;
    return untrack(() => pushModalFrame("mobile-workspace-launch-session", []));
  });

  $effect(() => {
    const activeWorkspaceId = workspaceId;
    const activeHostKey = hostKey;
    const activeTarget: WorkspaceRuntimeTarget = {
      workspaceId: activeWorkspaceId,
      ...(activeHostKey === undefined ? {} : { hostKey: activeHostKey }),
    };
    workspace = null;
    runtime = null;
    selectedSessionKey = loadMobileWorkspaceSession(activeWorkspaceId, activeHostKey);
    provisionalBaseSelection = false;
    loadError = null;
    runtimeError = null;
    composedInput = "";
    inputError = null;
    composerOpen = false;
    specialKeysOpen = false;
    launchSheetOpen = false;
    terminalOptionsOpen = false;
    terminalOptionsSaving = false;
    terminalSettingsDialogOpen = false;
    retryingSetup = false;
    releaseOwnedSessions();

    const execution = untrack(() =>
      appRuntime.runCommand(
        Effect.scoped(
          Effect.gen(function* () {
            const workflow = yield* WorkspaceRuntimeWorkflow;
            yield* workflow.claimPresenter(activeTarget, presenterID, observeMutation, {
              presentationIsCurrent: () => workspaceId === activeWorkspaceId && hostKey === activeHostKey,
            });
            yield* Effect.addFinalizer(() =>
              workflow.releasePresenter(activeTarget, presenterID).pipe(
                Effect.andThen(workflow.release(runtimeOwner)),
              ),
            );
            yield* loadWorkspaceAndRuntime();
            yield* Effect.forever(
              Effect.sleep("5 seconds").pipe(Effect.andThen(refreshWorkspaceState())),
            );
          }),
        ),
        {
          operation: "run mobile workspace terminal",
          safeContext: { workspaceId: activeWorkspaceId, remote: Boolean(activeHostKey) },
          onFailure: (failure) => {
            loadError = failureMessage(failure, "Workspace unavailable");
          },
        },
      ),
    );

    const stopExitListener = onSessionExited((exitedHostKey) => {
      const current = runtime;
      if (!current || !ownedHostKeys.includes(exitedHostKey)) return;
      const nextSessions = current.sessions.filter(
        (session) => sessionHostKey(workspaceId, hostKey, session.key, session.created_at) !== exitedHostKey,
      );
      if (nextSessions.length === current.sessions.length) return;
      applyRuntime({ ...current, sessions: nextSessions });
      requestRuntimeRefresh();
    });
    return () => {
      stopExitListener();
      execution.interrupt();
      releaseOwnedSessions();
    };
  });
</script>

<section class="mobile-workspace-terminal" aria-label="Workspace terminal">
  <header class="mobile-workspace-terminal__toolbar">
    <small class="mobile-workspace-terminal__context" title={workspaceIdentity}>{workspaceIdentity}</small>
    <button type="button" class="mobile-workspace-terminal__back" aria-label="Back to workspaces" onclick={onBack}>
      <ArrowLeftIcon size="20" strokeWidth="2" aria-hidden="true" />
    </button>
    {#if sessions.length > 0}
      <div class="mobile-workspace-terminal__switcher">
        <SelectDropdown
          title="Terminal session"
          value={selectedSessionKey ?? ""}
          options={sessionOptions}
          onchange={selectSession}
        />
      </div>
    {:else}
      <div class="mobile-workspace-terminal__identity">
        <strong>Workspace terminal</strong>
      </div>
    {/if}
    <div class="mobile-workspace-terminal__actions">
      {#if linkedItem}
        <button
          type="button"
          class="mobile-workspace-terminal__item"
          aria-label={hostKey
            ? `Linked ${linkedItem.itemType === "pr" ? "PR" : "issue"} #${linkedItem.number} unavailable for Fleet workspace`
            : `Open linked ${linkedItem.itemType === "pr" ? "PR" : "issue"} #${linkedItem.number}`}
          onclick={onOpenItem}
          disabled={hostKey !== undefined}
          title={hostKey ? "Linked item details are not available for Fleet workspaces" : undefined}
        >#{linkedItem.number}</button>
      {/if}
      <button
        type="button"
        aria-label="Terminal options"
        aria-haspopup="dialog"
        aria-expanded={terminalOptionsOpen}
        onclick={openTerminalOptions}
        disabled={!workspace || workspace.status !== "ready"}
      >
        <MoreHorizontalIcon size="20" strokeWidth="2" aria-hidden="true" />
      </button>
    </div>
  </header>

  {#if loadError}
    <div class="mobile-workspace-terminal__state error">
      <strong>{hostKey ? `Fleet · ${hostKey}` : "Workspace unavailable"}</strong>
      <span>{loadError}</span>
      <button type="button" onclick={() => {
        loadError = null;
        appRuntime.runCommand(loadWorkspaceAndRuntime(), {
          operation: "retry mobile workspace",
          safeContext: { workspaceId, remote: Boolean(hostKey) },
          onFailure: () => undefined,
        });
      }}><RefreshCwIcon size="18" aria-hidden="true" />Retry</button>
    </div>
  {:else if !workspace}
    <div class="mobile-workspace-terminal__state"><Spinner size={18} /><span>Loading workspace…</span></div>
  {:else if workspace.status !== "ready"}
    <div class="mobile-workspace-terminal__state" class:error={workspace.status === "error"}>
      <strong>{workspace.status === "creating"
        ? "Setting up workspace…"
        : workspace.status === "deleting"
          ? "Deleting workspace…"
          : workspace.status === "deletion_failed"
            ? "Workspace deletion failed"
            : "Workspace setup failed"}</strong>
      {#if workspace.error_message}<span>{workspace.error_message}</span>{/if}
      {#if runtimeError}<span>{runtimeError}</span>{/if}
      {#if workspace.status === "error"}
        <button type="button" disabled={retryingSetup} onclick={retryWorkspaceSetup}>
          {#if retryingSetup}<Spinner size={16} />{:else}<RefreshCwIcon size="18" aria-hidden="true" />{/if}
          Retry
        </button>
      {/if}
    </div>
  {:else if !runtime && !runtimeError && !selectedHostKey}
    <div class="mobile-workspace-terminal__state"><Spinner size={18} /><span>Loading terminal sessions…</span></div>
  {:else if runtimeError && !runtime && !selectedHostKey}
    <div class="mobile-workspace-terminal__state error">
      <strong>Terminal runtime unavailable</strong><span>{runtimeError}</span>
      <button type="button" onclick={requestRuntimeRefresh}><RefreshCwIcon size="18" aria-hidden="true" />Reconnect</button>
    </div>
  {:else if selectedHostKey}
    <div class="mobile-workspace-terminal__stage" aria-label={`Selected terminal: ${selectedSession?.label ?? "Terminal"}`}>
      {#if runtimeError}
        <div class="mobile-workspace-terminal__runtime-error" role="status">
          <span><strong>Runtime sessions unavailable</strong><small>{runtimeError}</small></span>
          <button type="button" aria-label="Retry runtime sessions" onclick={requestRuntimeRefresh}>
            <RefreshCwIcon size="16" aria-hidden="true" />Retry
          </button>
        </div>
      {/if}
      <div class="mobile-workspace-terminal__viewport" data-terminal-software-keyboard="manual">
        <SessionTerminalSlot hostKey={selectedHostKey} {visible} />
      </div>
      {#if composerOpen}
        <form class="mobile-workspace-terminal__composer" onsubmit={(event) => {
          event.preventDefault();
          sendComposedInput();
        }}>
          <button
            type="button"
            class="mobile-workspace-terminal__composer-handle"
            aria-label="Collapse terminal composer"
            onclick={closeComposer}
            onpointerdown={startComposerDrag}
            onpointerup={(event) => finishComposerDrag(event, "close")}
            onpointercancel={() => (composerDragStartY = null)}
          >
            <span aria-hidden="true"></span>
            <ChevronDownIcon size="16" aria-hidden="true" />
          </button>
          {#if specialKeysOpen}
            <div class="mobile-workspace-terminal__special-keys" role="group" aria-label="Special terminal keys">
              <button type="button" aria-label="Escape" onclick={() => sendSpecialKey("Escape")}>Esc</button>
              <button type="button" onclick={() => sendSpecialKey("Tab")}>Tab</button>
              <button type="button" aria-label="Arrow left" onclick={() => sendSpecialKey("ArrowLeft")}>
                <ArrowLeftIcon size="18" aria-hidden="true" />
              </button>
              <button type="button" aria-label="Arrow up" onclick={() => sendSpecialKey("ArrowUp")}>
                <ArrowUpIcon size="18" aria-hidden="true" />
              </button>
              <button type="button" aria-label="Arrow down" onclick={() => sendSpecialKey("ArrowDown")}>
                <ArrowDownIcon size="18" aria-hidden="true" />
              </button>
              <button type="button" aria-label="Arrow right" onclick={() => sendSpecialKey("ArrowRight")}>
                <ArrowRightIcon size="18" aria-hidden="true" />
              </button>
              <button type="button" aria-label="Space" onclick={() => sendSpecialKey("Space")}>
                <SpaceIcon size="18" aria-hidden="true" />
              </button>
              <button type="button" aria-label="Return" onclick={() => sendSpecialKey("Enter")}>
                <CornerDownLeftIcon size="18" aria-hidden="true" />
              </button>
            </div>
          {/if}
          <textarea
            bind:this={composerInput}
            aria-label="Terminal command"
            bind:value={composedInput}
            autocomplete="off"
            autocapitalize="none"
            oninput={handleComposerInput}
            rows="1"
            spellcheck="false"
            placeholder="Type terminal input"
          ></textarea>
          <button
            type="button"
            class="mobile-workspace-terminal__special-keys-toggle"
            aria-label={specialKeysOpen ? "Hide special terminal keys" : "Show special terminal keys"}
            aria-expanded={specialKeysOpen}
            onclick={toggleSpecialKeys}
          >
            <KeyboardIcon size="20" aria-hidden="true" />
          </button>
          <button type="submit" class="mobile-workspace-terminal__send" aria-label="Send terminal input">Send</button>
          {#if inputError}<small role="status">{inputError}</small>{/if}
        </form>
      {:else}
        <button
          type="button"
          class="mobile-workspace-terminal__composer-toggle"
          aria-label="Open terminal composer"
          onclick={openComposer}
          onpointerdown={startComposerDrag}
          onpointerup={(event) => finishComposerDrag(event, "open")}
          onpointercancel={() => (composerDragStartY = null)}
        >
          <span aria-hidden="true"></span>
          Compose
          <ChevronUpIcon size="16" aria-hidden="true" />
        </button>
      {/if}
    </div>
  {:else}
    <div class="mobile-workspace-terminal__empty">
      <div><strong>No terminal sessions</strong><span>Launch an agent or shell in this workspace.</span></div>
      <div class="mobile-workspace-terminal__launch-grid">
        {#each launchTargets.filter((target) => target.available) as target (target.key)}
          <button type="button" disabled={launchingTarget !== null || pendingLaunch !== null} onclick={() => launch(target.key)}>
            {#if launchingTarget === target.key}<Spinner size={16} />{/if}
            {target.kind === "plain_shell" ? "Shell" : target.label}
          </button>
        {/each}
      </div>
    </div>
  {/if}
</section>

{#if terminalOptionsOpen}
  <Modal
    title="Terminal options"
    ariaLabel="Terminal options"
    closeLabel="Close terminal options"
    width="min(100%, 38rem)"
    maxWidth="100%"
    onclose={closeTerminalOptions}
  >
    <div class="mobile-terminal-options-sheet">
      <div class="mobile-terminal-options-sheet__actions">
        <button type="button" disabled={terminalOptionsSaving || launchingTarget !== null || stoppingSession !== null || pendingLaunch !== null} onclick={openLaunchSheet}>
          <span><PlusIcon size="18" strokeWidth="2" aria-hidden="true" />New terminal</span>
          <small>Launch a shell or configured agent.</small>
        </button>
      </div>

      {#if selectedSession?.runtimeSession}
        <div class="mobile-terminal-options-sheet__danger">
          <button
            type="button"
            aria-label={`Stop terminal ${selectedSession.runtimeSession.label}`}
            disabled={terminalOptionsSaving || stoppingSession !== null || launchingTarget !== null}
            onclick={promptStopSelectedSession}
          >
            {#if stoppingSession === selectedSession.runtimeSession.key}<Spinner size={16} />{:else}<SquareIcon size="17" strokeWidth="2" aria-hidden="true" />{/if}
            Stop terminal…
          </button>
          <small>Terminates the process running in {selectedSession.runtimeSession.label}.</small>
        </div>
      {/if}

      <section class="mobile-terminal-options-sheet__settings" aria-labelledby="mobile-terminal-settings-heading">
        <h3 id="mobile-terminal-settings-heading">Appearance and behavior</h3>
        <TerminalSettings
          terminal={terminalSettings}
          compact={true}
          livePreview={true}
          onUpdate={(updated) => {
            terminalSettings = updated;
          }}
          onSavingChange={(saving) => {
            terminalOptionsSaving = saving;
          }}
          onFontDialogOpenChange={(open) => {
            terminalSettingsDialogOpen = open;
          }}
        />
      </section>
    </div>
  </Modal>
{/if}

{#if launchSheetOpen}
  <Modal
    title="Launch session"
    ariaLabel="Launch workspace session"
    closeLabel="Close launch session"
    width="min(100%, 38rem)"
    maxWidth="100%"
    onclose={() => (launchSheetOpen = false)}
  >
    <div class="mobile-terminal-sheet">
      <small class="mobile-terminal-sheet__branch">{workspace?.git_head_ref ?? workspaceId}</small>
      <div class="mobile-terminal-sheet__targets">
        {#each launchTargets as target (target.key)}
          <button type="button" disabled={!target.available || launchingTarget !== null || pendingLaunch !== null} title={target.disabled_reason} onclick={() => launch(target.key)}>
            <span><strong><LaunchTargetName {target} label={target.kind === "plain_shell" ? "Shell" : target.label} iconSize={16} /></strong><small>{target.available ? target.source : target.disabled_reason}</small></span>
            {#if launchingTarget === target.key}<Spinner size={16} />{:else}<PlusIcon size="18" aria-hidden="true" />{/if}
          </button>
        {/each}
        {#if launchTargets.length === 0}<p>No launch targets are configured for this workspace.</p>{/if}
      </div>
    </div>
  </Modal>
{/if}

<ConfirmDialog
  open={stopSession !== null}
  title="Stop terminal?"
  message={stopSession ? `Stop terminal "${stopSession.label}"?` : ""}
  hint="This terminates the process running in this terminal session."
  confirmLabel="Stop terminal"
  pendingLabel="Stopping…"
  busy={stoppingSession !== null}
  tone="danger"
  frameId="mobile-workspace-stop-terminal"
  onCancel={() => (stopSession = null)}
  onConfirm={stopSelectedSession}
/>

<style>
  .mobile-workspace-terminal { flex: 1; min-height: 0; display: flex; flex-direction: column; background: var(--bg-primary); }
  .mobile-workspace-terminal__toolbar { min-height: 3.5rem; display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; column-gap: 0.5rem; row-gap: 0.25rem; padding: 0.375rem 0.5rem; border-bottom: thin solid var(--border-default); background: var(--bg-surface); }
  .mobile-workspace-terminal__toolbar button { min-width: 2.75rem; min-height: 2.75rem; display: inline-flex; align-items: center; justify-content: center; padding: 0 0.625rem; border: thin solid var(--border-default); border-radius: var(--radius-md); color: var(--text-secondary); background: var(--bg-inset); font: inherit; }
  .mobile-workspace-terminal__toolbar button:focus-visible, .mobile-workspace-terminal__empty button:focus-visible, .mobile-terminal-sheet button:focus-visible, .mobile-terminal-options-sheet button:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 2px; }
  .mobile-workspace-terminal__context { grid-column: 1 / -1; min-width: 0; overflow: hidden; padding: 0 0.25rem; color: var(--text-muted); font-size: var(--font-size-sm); line-height: 1.2; text-overflow: ellipsis; white-space: nowrap; }
  .mobile-workspace-terminal__back { padding: 0 !important; }
  .mobile-workspace-terminal__switcher { min-width: 0; position: relative; }
  .mobile-workspace-terminal__switcher :global(.kit-select-dropdown) { width: 100%; min-width: 0; }
  .mobile-workspace-terminal__switcher :global(.kit-select-dropdown__trigger) { min-height: 2.75rem; padding: 0 2rem 0 0.75rem; border-color: var(--border-default); border-radius: var(--radius-md); color: var(--text-primary); font-size: var(--font-size-md); font-weight: 650; }
  .mobile-workspace-terminal__switcher :global(.kit-select-dropdown__option) { min-height: 2.75rem; font-size: var(--font-size-md); }
  .mobile-workspace-terminal__identity { min-width: 0; }
  .mobile-workspace-terminal__identity strong { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mobile-workspace-terminal__identity strong { color: var(--text-primary); font-size: var(--font-size-md); }
  .mobile-workspace-terminal__actions { display: flex; gap: 0.375rem; }
  .mobile-workspace-terminal__item { min-width: auto !important; min-height: 2rem !important; color: var(--text-on-accent) !important; border-color: var(--accent-green) !important; background: var(--accent-green) !important; font-family: var(--font-mono) !important; font-weight: 700 !important; }
  .mobile-workspace-terminal__item:disabled { cursor: not-allowed; opacity: var(--opacity-disabled); }
  .mobile-workspace-terminal__stage { position: relative; flex: 1; min-height: 0; display: flex; flex-direction: column; background: var(--terminal-bg, var(--bg-primary)); }
  .mobile-workspace-terminal__runtime-error { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; padding: 0.5rem 0.75rem; color: var(--text-secondary); border-bottom: thin solid var(--border-default); background: var(--bg-surface); font-size: var(--font-size-sm); }
  .mobile-workspace-terminal__runtime-error span { min-width: 0; display: flex; flex-direction: column; }
  .mobile-workspace-terminal__runtime-error strong { color: var(--text-primary); }
  .mobile-workspace-terminal__runtime-error small { overflow: hidden; color: var(--accent-red); text-overflow: ellipsis; white-space: nowrap; }
  .mobile-workspace-terminal__runtime-error button { min-height: 2.75rem; display: inline-flex; align-items: center; gap: 0.375rem; padding: 0 0.75rem; color: var(--text-primary); border: thin solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); font: inherit; font-weight: 650; }
  .mobile-workspace-terminal__viewport { flex: 1; min-height: 0; display: flex; }
  .mobile-workspace-terminal__composer { display: grid; grid-template-columns: minmax(0, 1fr) 2.75rem auto; align-items: end; gap: 0.5rem; padding: 0.5rem 0.5rem max(0.5rem, env(safe-area-inset-bottom)); border-top: thin solid var(--border-default); background: var(--bg-surface); }
  .mobile-workspace-terminal__composer textarea, .mobile-workspace-terminal__send, .mobile-workspace-terminal__special-keys-toggle { min-height: 2.75rem; border: thin solid var(--border-default); border-radius: var(--radius-md); font: inherit; }
  .mobile-workspace-terminal__composer textarea { width: 100%; min-width: 0; max-height: 8rem; overflow-y: auto; resize: none; padding: 0.625rem 0.75rem; color: var(--text-primary); background: var(--bg-inset); font-family: var(--font-mono); font-size: var(--font-size-md); line-height: 1.4; }
  .mobile-workspace-terminal__special-keys-toggle { display: inline-flex; align-items: center; justify-content: center; padding: 0; color: var(--text-secondary); background: var(--bg-inset); }
  .mobile-workspace-terminal__special-keys-toggle[aria-expanded="true"] { color: var(--accent-blue); border-color: var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-inset)); }
  .mobile-workspace-terminal__send { padding: 0 1rem; color: var(--text-on-accent); border-color: var(--accent-blue); background: var(--accent-blue); font-weight: 700; }
  .mobile-workspace-terminal__special-keys { grid-column: 1 / -1; display: grid; grid-template-columns: repeat(8, minmax(2.75rem, 1fr)); gap: 0.125rem; overflow-x: auto; scrollbar-width: none; }
  .mobile-workspace-terminal__special-keys::-webkit-scrollbar { display: none; }
  .mobile-workspace-terminal__special-keys button { min-width: 2.75rem; min-height: 2.75rem; display: inline-flex; align-items: center; justify-content: center; padding: 0; color: var(--text-primary); border: thin solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); font: inherit; font-family: var(--font-mono); font-size: var(--font-size-sm); font-weight: 650; }
  .mobile-workspace-terminal__special-keys button:active { border-color: var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 16%, var(--bg-inset)); }
  .mobile-workspace-terminal__composer-handle { grid-column: 1 / -1; width: 100%; min-height: 1.5rem; display: flex; align-items: center; justify-content: center; gap: 0.375rem; padding: 0; color: var(--text-muted); border: 0; background: transparent; touch-action: none; }
  .mobile-workspace-terminal__composer-handle > span, .mobile-workspace-terminal__composer-toggle > span { width: 2.25rem; height: 0.25rem; border-radius: 999px; background: var(--border-strong); }
  .mobile-workspace-terminal__composer-toggle { position: absolute; z-index: 4; left: 50%; bottom: max(0.5rem, env(safe-area-inset-bottom)); min-height: 2.75rem; display: grid; grid-template-columns: 2.25rem auto 2.25rem; align-items: center; justify-content: center; gap: 0.5rem; padding: 0 0.75rem; color: var(--text-secondary); border: thin solid var(--border-default); border-radius: 999px; background: color-mix(in srgb, var(--bg-surface) 92%, transparent); box-shadow: var(--shadow-md); font: inherit; font-size: var(--font-size-sm); font-weight: 650; touch-action: none; transform: translateX(-50%); }
  .mobile-workspace-terminal__composer-toggle > :global(svg) { justify-self: center; }
  .mobile-workspace-terminal__composer small { grid-column: 1 / -1; color: var(--accent-red); font-size: var(--font-size-sm); }
  .mobile-workspace-terminal__composer textarea:focus-visible, .mobile-workspace-terminal__composer button:focus-visible, .mobile-workspace-terminal__composer-toggle:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 2px; }
  .mobile-workspace-terminal__state, .mobile-workspace-terminal__empty { flex: 1; min-height: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 0.75rem; padding: 2rem 1rem; color: var(--text-muted); text-align: center; font-size: var(--font-size-md); }
  .mobile-workspace-terminal__state strong, .mobile-workspace-terminal__empty strong { color: var(--text-primary); font-size: var(--font-size-xl); }
  .mobile-workspace-terminal__state button { min-height: 2.75rem; display: inline-flex; align-items: center; gap: 0.5rem; padding: 0 1rem; border: thin solid var(--border-default); border-radius: var(--radius-md); color: var(--text-primary); background: var(--bg-surface); font: inherit; font-weight: 650; }
  .mobile-workspace-terminal__state.error span { max-width: 32rem; color: var(--accent-red); }
  .mobile-workspace-terminal__empty > div:first-child { display: flex; flex-direction: column; gap: 0.375rem; }
  .mobile-workspace-terminal__launch-grid { width: min(100%, 28rem); display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.625rem; }
  .mobile-workspace-terminal__launch-grid button { min-height: 3rem; display: inline-flex; align-items: center; justify-content: center; gap: 0.5rem; padding: 0 0.75rem; border: thin solid var(--border-default); border-radius: var(--radius-md); color: var(--text-primary); background: var(--bg-surface); font: inherit; font-weight: 650; }
  :global(.kit-modal-overlay:has(.mobile-terminal-sheet)), :global(.kit-modal-overlay:has(.mobile-terminal-options-sheet)) { align-items: flex-end; }
  :global(.kit-modal-panel:has(.mobile-terminal-sheet)), :global(.kit-modal-panel:has(.mobile-terminal-options-sheet)) { max-height: 78vh; border-bottom: 0; border-radius: var(--radius-lg) var(--radius-lg) 0 0; }
  :global(.kit-modal-body:has(> .mobile-terminal-sheet)) { padding: 0 0 max(1rem, env(safe-area-inset-bottom)); }
  .mobile-terminal-sheet__branch { display: block; overflow: hidden; padding: 0.625rem 0.875rem; color: var(--text-muted); border-bottom: thin solid var(--border-muted); font-family: var(--font-mono); font-size: var(--font-size-sm); text-overflow: ellipsis; white-space: nowrap; }
  .mobile-terminal-sheet__targets { display: grid; padding: 0 0.875rem; }
  .mobile-terminal-sheet__targets > button { min-height: 4rem; display: flex; align-items: center; justify-content: space-between; gap: 1rem; padding: 0.5rem 0; border: 0; border-bottom: thin solid var(--border-muted); color: var(--text-primary); background: transparent; font: inherit; text-align: left; }
  .mobile-terminal-sheet__targets > button span { min-width: 0; display: flex; flex-direction: column; gap: 0.125rem; }
  .mobile-terminal-sheet__targets small { color: var(--text-muted); font-size: var(--font-size-sm); }
  .mobile-terminal-sheet__targets p { color: var(--text-muted); font-size: var(--font-size-md); }
  :global(.kit-modal-body:has(> .mobile-terminal-options-sheet)) { padding: 0; }
  .mobile-terminal-options-sheet { display: grid; gap: 0; padding-bottom: max(1rem, env(safe-area-inset-bottom)); }
  .mobile-terminal-options-sheet__actions { padding: 0 0.875rem; border-bottom: thin solid var(--border-muted); }
  .mobile-terminal-options-sheet__actions > button { width: 100%; min-height: 4rem; display: flex; flex-direction: column; align-items: flex-start; justify-content: center; gap: 0.125rem; padding: 0.5rem 0; color: var(--text-primary); border: 0; background: transparent; font: inherit; text-align: left; }
  .mobile-terminal-options-sheet__actions span { display: inline-flex; align-items: center; gap: 0.625rem; font-weight: 650; }
  .mobile-terminal-options-sheet__actions small, .mobile-terminal-options-sheet__danger small { color: var(--text-muted); font-size: var(--font-size-sm); }
  .mobile-terminal-options-sheet__settings { padding: 1rem 0.875rem 0; }
  .mobile-terminal-options-sheet__settings h3 { margin: 0 0 0.875rem; color: var(--text-muted); font-size: var(--font-size-sm); font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase; }
  .mobile-terminal-options-sheet__settings :global(.terminal-settings.compact) { width: 100%; }
  .mobile-terminal-options-sheet__danger { display: flex; flex-direction: column; gap: 0.375rem; padding: 0.75rem 0.875rem; border-bottom: thin solid var(--border-muted); }
  .mobile-terminal-options-sheet__danger > button { min-height: 2.75rem; display: inline-flex; align-items: center; justify-content: flex-start; gap: 0.5rem; padding: 0; color: var(--accent-red); border: 0; background: transparent; font: inherit; font-weight: 700; }
</style>
