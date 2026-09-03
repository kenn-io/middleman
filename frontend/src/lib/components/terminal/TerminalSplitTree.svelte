<script lang="ts">
  import { Effect } from "effect";
  import { untrack } from "svelte";
  import {
    HarnessIcon,
    SplitResizeHandle,
    type HarnessIconId,
    type SplitResizeEvent,
  } from "@kenn-io/kit-ui";
  import { clearActiveTabbedPanelDrag, startTabbedPanelTabDrag } from "../shared/tabbed-panel-drag.js";
  import type { RuntimeSession } from "../../api/types.js";
  import XIcon from "@lucide/svelte/icons/x";
  import MoveIcon from "@lucide/svelte/icons/move";
  import PanelRightIcon from "@lucide/svelte/icons/panel-right";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import SparklesIcon from "@lucide/svelte/icons/sparkles";
  import TerminalIcon from "@lucide/svelte/icons/terminal";
  import Self from "./TerminalSplitTree.svelte";
  import SessionTerminalSlot from "./SessionTerminalSlot.svelte";
  import type { PaneNode, SplitDirection, SplitEdge } from "./terminal-layout";
  import {
    clampRatio,
    splitEdgeFromPoint,
    splitPlacementForEdge,
  } from "./terminal-layout";
  import {
    clearActiveTerminalDrag,
    onTerminalDragEnd,
    readRuntimeSessionDrag,
    startRuntimeSessionDrag,
  } from "./terminal-drag";
  import { sessionHostKey } from "../../stores/session-host.svelte.ts";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { observeResize } from "../../browser/observers.js";
  import {
    beginTerminalGeometryIntent,
    extendTerminalGeometryIntent,
  } from "./terminalGeometryIntent.js";
  import { launchTargetHarness } from "./agentHarness";

  const runtime = getAppRuntime();

  interface BorderTrim {
    top?: boolean;
    right?: boolean;
    bottom?: boolean;
    left?: boolean;
  }

  type BorderEdge = keyof BorderTrim;

  interface Props {
    workspaceId: string;
    workspaceHostKey?: string | undefined;
    node: PaneNode;
    sessions: RuntimeSession[];
    displayLabels: Record<string, string>;
    activeSessionKey: string | null;
    borderTrim?: BorderTrim | undefined;
    disabled?: boolean;
    // Whether this tree is painted at all: false while the owning
    // WorkspaceTerminalView is parked in a hidden host, or while the dock
    // holding it sits behind another workflow tab. It is every leaf's whole
    // visibility, because a split paints all of its leaves at once — gating a
    // leaf on `activeSessionKey` instead would leave the unfocused halves of a
    // split inert, so their ResizeObserver never pushed a size and their tmux
    // pane kept the launch default until the user clicked into them.
    hostVisible?: boolean;
    /** Shared detail-surface scope when terminal sessions may become top-level panes. */
    dragScope?: string | undefined;
    /** Maps a runtime session to its top-level detail-pane key. */
    paneKeyForSession?: ((sessionKey: string) => string | null) | undefined;
    onSelect?: ((sessionKey: string) => void) | undefined;
    onClose?: ((session: RuntimeSession) => void) | undefined;
    onRename?: ((session: RuntimeSession) => void) | undefined;
    onMoveToWorkflow?: ((sessionKey: string) => void) | undefined;
    /** Reads local runtime drags plus promoted detail-pane sessions when embedded. */
    readSessionDrag?: ((event: DragEvent) => string | null) | undefined;
    /** Absent unless a detail surface is hosting this workspace and can hold a pane. */
    onPromoteSession?: ((sessionKey: string) => void) | undefined;
    onRatioChange?: ((splitId: string, ratio: number) => void) | undefined;
    onSplitSession?:
      | ((
          sessionKey: string,
          targetLeafID: string,
          direction: SplitDirection,
          placement: "before" | "after",
        ) => void)
      | undefined;
  }

  const {
    workspaceId,
    workspaceHostKey = undefined,
    node,
    sessions,
    displayLabels,
    activeSessionKey,
    borderTrim = {},
    disabled = false,
    hostVisible = true,
    dragScope = undefined,
    paneKeyForSession = undefined,
    onSelect,
    onClose,
    onRename,
    onMoveToWorkflow,
    readSessionDrag,
    onPromoteSession,
    onRatioChange,
    onSplitSession,
  }: Props = $props();

  const MIN_RATIO = 0.12;
  const MAX_RATIO = 0.88;

  let splitEl = $state<HTMLDivElement | null>(null);
  let splitSize = $state(0);
  let resizeStartRatio = 0.5;
  let resizeStartSize = 0;
  let resizeIntentStarted = false;
  let dropTargetsVisible = $state(false);
  let activeSplitEdge = $state<SplitEdge | null>(null);

  function sessionForKey(sessionKey: string): RuntimeSession | null {
    return sessions.find((session) => session.key === sessionKey) ?? null;
  }

  function labelFor(session: RuntimeSession): string {
    return displayLabels[session.key] ?? session.label;
  }

  function sessionHarness(session: RuntimeSession): HarnessIconId | null {
    return launchTargetHarness({ kind: session.kind, key: session.target_key });
  }

  function startSessionDrag(
    event: DragEvent,
    session: RuntimeSession,
  ): void {
    if (disabled) return;
    startRuntimeSessionDrag(event, {
      workspaceId: session.workspace_id,
      sessionKey: session.key,
    });
    const paneKey = paneKeyForSession?.(session.key) ?? null;
    if (dragScope !== undefined && paneKey !== null) {
      startTabbedPanelTabDrag(event, { scope: dragScope, tabKey: paneKey }, "Kenn Forge session tab");
    }
  }

  function clearDrag(): void {
    clearActiveTerminalDrag();
    clearActiveTabbedPanelDrag();
  }

  function readDroppedSession(event: DragEvent): string | null {
    if (disabled) return null;
    const sessionKey = readSessionDrag
      ? readSessionDrag(event)
      : readRuntimeSessionDrag(event, workspaceId);
    if (
      sessionKey === null ||
      (node.type === "leaf" && sessionKey === node.sessionKey) ||
      (readSessionDrag === undefined && !sessionForKey(sessionKey))
    ) {
      return null;
    }
    return sessionKey;
  }

  function splitEdgeFromEvent(event: DragEvent): SplitEdge | null {
    const target = event.currentTarget;
    if (!(target instanceof HTMLElement)) return null;
    const rect = target.getBoundingClientRect();
    return splitEdgeFromPoint(rect, event.clientX, event.clientY);
  }

  function handleDragOver(event: DragEvent): void {
    if (disabled) return;
    if (node.type !== "leaf" || readDroppedSession(event) === null) return;
    event.preventDefault();
    event.stopPropagation();
    dropTargetsVisible = true;
    activeSplitEdge = splitEdgeFromEvent(event);
    if (event.dataTransfer) {
      event.dataTransfer.dropEffect = "move";
    }
  }

  function hideDropTargets(): void {
    dropTargetsVisible = false;
    activeSplitEdge = null;
  }

  // Every split drops its overlay when the drag ends, not only the one that handled
  // the drop: a drop on a sibling restructures the tree under the pointer, so the
  // dragleave that would have hidden this one never arrives.
  $effect(() => onTerminalDragEnd(() => untrack(() => hideDropTargets())));

  function handleDragLeave(event: DragEvent): void {
    if (disabled) return;
    const current = event.currentTarget;
    const next = event.relatedTarget;
    if (
      current instanceof HTMLElement &&
      next instanceof Node &&
      current.contains(next)
    ) {
      return;
    }
    hideDropTargets();
  }

  function dropSplit(event: DragEvent): void {
    if (disabled) return;
    if (node.type !== "leaf") return;
    const sessionKey = readDroppedSession(event);
    const edge = splitEdgeFromEvent(event);
    if (sessionKey === null || edge === null) return;
    event.preventDefault();
    event.stopPropagation();
    hideDropTargets();
    const { direction, placement } = splitPlacementForEdge(edge);
    onSplitSession?.(sessionKey, node.id, direction, placement);
    clearActiveTerminalDrag();
    clearActiveTabbedPanelDrag();
  }

  function measureSplit(): number {
    if (node.type !== "split" || !splitEl) return 0;
    const rect = splitEl.getBoundingClientRect();
    return node.direction === "horizontal" ? rect.width : rect.height;
  }

  $effect(() => {
    if (node.type !== "split" || !splitEl) return;
    const splitTarget = splitEl;
    splitSize = measureSplit();
    if (typeof ResizeObserver === "undefined") return;
    const execution = untrack(() =>
      runtime.runCommand(
        Effect.scoped(
          observeResize(splitTarget, () => {
            splitSize = measureSplit();
          }).pipe(Effect.andThen(Effect.never)),
        ),
        { operation: "observe terminal split", safeContext: { workspaceId }, onFailure: () => {} },
      ),
    );
    return execution.interrupt;
  });

  function startResize(): void {
    if (node.type !== "split") return;
    resizeIntentStarted = false;
    resizeStartRatio = node.ratio;
    splitSize = measureSplit();
    resizeStartSize = splitSize;
  }

  function handleResize(event: SplitResizeEvent): void {
    if (node.type !== "split") return;
    const ratio = resizeStartRatio + event.delta / Math.max(1, resizeStartSize);
    const nextRatio = clampRatio(ratio);
    if (nextRatio !== node.ratio) {
      if (resizeIntentStarted) {
        extendTerminalGeometryIntent();
      } else {
        beginTerminalGeometryIntent();
        resizeIntentStarted = true;
      }
    }
    onRatioChange?.(node.id, nextRatio);
  }

  function inheritTrim(target: BorderTrim, edge: BorderEdge): void {
    if (borderTrim[edge]) {
      target[edge] = true;
    }
  }

  function firstChildTrim(direction: SplitDirection): BorderTrim {
    if (direction === "horizontal") {
      const trim: BorderTrim = { right: true };
      inheritTrim(trim, "top");
      inheritTrim(trim, "bottom");
      inheritTrim(trim, "left");
      return trim;
    }

    const trim: BorderTrim = { bottom: true };
    inheritTrim(trim, "top");
    inheritTrim(trim, "right");
    inheritTrim(trim, "left");
    return trim;
  }

  function secondChildTrim(direction: SplitDirection): BorderTrim {
    if (direction === "horizontal") {
      const trim: BorderTrim = { left: true };
      inheritTrim(trim, "top");
      inheritTrim(trim, "right");
      inheritTrim(trim, "bottom");
      return trim;
    }

    const trim: BorderTrim = { top: true };
    inheritTrim(trim, "right");
    inheritTrim(trim, "bottom");
    inheritTrim(trim, "left");
    return trim;
  }
</script>

{#if node.type === "leaf"}
  {@const session = sessionForKey(node.sessionKey)}
  <div
    class={[
      "terminal-leaf",
      {
        active: activeSessionKey === node.sessionKey,
        "single-session": sessions.length <= 1,
        "trim-top": borderTrim.top,
        "trim-right": borderTrim.right,
        "trim-bottom": borderTrim.bottom,
        "trim-left": borderTrim.left,
      },
    ]}
  >
    {#if session}
      {@const harness = sessionHarness(session)}
      {#if sessions.length > 1}
        <div
          class="leaf-header"
          role="group"
          aria-label={`${labelFor(session)} terminal pane`}
          draggable={!disabled}
          ondragstart={(event) => startSessionDrag(event, session)}
          ondragend={clearDrag}
        >
          <button
            class="leaf-title"
            draggable={!disabled}
            ondragstart={(event) => startSessionDrag(event, session)}
            ondragend={clearDrag}
            onclick={() => {
              if (disabled) return;
              onSelect?.(session.key);
            }}
            aria-label={`Focus ${labelFor(session)}`}
            disabled={disabled}
          >
            <span class="leaf-icon" aria-hidden="true">
              {#if session.kind === "plain_shell"}
                <TerminalIcon size="12" strokeWidth="2" />
              {:else if harness}
                <HarnessIcon {harness} size={12} decorative />
              {:else}
                <SparklesIcon size="12" strokeWidth="2" />
              {/if}
            </span>
            <span class="leaf-label">{labelFor(session)}</span>
            <span class={["leaf-dot", session.status]}></span>
          </button>
          <div class="leaf-actions">
            <button
              class="leaf-action"
              title="Rename"
              aria-label={`Rename ${labelFor(session)}`}
              disabled={disabled}
              onclick={() => {
                if (disabled) return;
                onRename?.(session);
              }}
            >
              <PencilIcon size="12" strokeWidth="2" aria-hidden="true" />
            </button>
            <button
              class="leaf-action"
              title="Move to workflow"
              aria-label={`Move ${labelFor(session)} to workflow`}
              disabled={disabled}
              onclick={() => {
                if (disabled) return;
                onMoveToWorkflow?.(session.key);
              }}
            >
              <MoveIcon size="12" strokeWidth="2" aria-hidden="true" />
            </button>
            {#if onPromoteSession}
              <button
                class="leaf-action"
                title="Move to a pane"
                aria-label={`Move ${labelFor(session)} to a pane`}
                disabled={disabled}
                onclick={() => {
                  if (disabled) return;
                  onPromoteSession?.(session.key);
                }}
              >
                <PanelRightIcon size="12" strokeWidth="2" aria-hidden="true" />
              </button>
            {/if}
            <button
              class="leaf-action"
              title="Close"
              aria-label={`Close ${labelFor(session)}`}
              disabled={disabled}
              onclick={() => {
                if (disabled) return;
                onClose?.(session);
              }}
            >
              <XIcon size="12" strokeWidth="2.2" aria-hidden="true" />
            </button>
          </div>
        </div>
      {/if}
      {#key session.key}
        <div
          class={[
            "terminal-leaf-body",
            { "show-drop-targets": dropTargetsVisible },
          ]}
          role="group"
          aria-label={`${labelFor(session)} split drop targets`}
          onpointerdown={() => {
            if (disabled) return;
            onSelect?.(session.key);
          }}
          onfocusin={() => {
            if (disabled) return;
            onSelect?.(session.key);
          }}
          ondragover={handleDragOver}
          ondragleave={handleDragLeave}
          ondrop={dropSplit}
        >
          <SessionTerminalSlot
            hostKey={sessionHostKey(
              workspaceId,
              workspaceHostKey,
              session.key,
              session.created_at,
            )}
            visible={hostVisible}
          />
          <div
            class={[
              "split-preview",
              activeSplitEdge,
              { active: dropTargetsVisible && activeSplitEdge !== null },
            ]}
            aria-hidden="true"
          ></div>
        </div>
      {/key}
    {:else}
      <div class="missing-session">Session unavailable</div>
    {/if}
  </div>
{:else}
  <div
    bind:this={splitEl}
    class={["terminal-split", node.direction]}
    style={`--first-ratio: ${node.ratio}; --second-ratio: ${1 - node.ratio};`}
  >
    <div class="split-child first">
      <Self
        {workspaceId}
        {workspaceHostKey}
        node={node.first}
        {sessions}
        {displayLabels}
        {activeSessionKey}
        {disabled}
        {hostVisible}
        {dragScope}
        {paneKeyForSession}
        borderTrim={firstChildTrim(node.direction)}
        {onSelect}
        {onClose}
        {onRename}
        {onMoveToWorkflow}
        {readSessionDrag}
        {onPromoteSession}
        {onRatioChange}
        {onSplitSession}
      />
    </div>
    <SplitResizeHandle
      class="split-divider"
      ariaLabel="Resize split"
      orientation={node.direction}
      ariaValueMin={Math.round(MIN_RATIO * splitSize)}
      ariaValueMax={Math.round(MAX_RATIO * splitSize)}
      ariaValueNow={Math.round(node.ratio * splitSize)}
      {disabled}
      onResizeStart={startResize}
      onResize={handleResize}
    />
    <div class="split-child second">
      <Self
        {workspaceId}
        {workspaceHostKey}
        node={node.second}
        {sessions}
        {displayLabels}
        {activeSessionKey}
        {disabled}
        {hostVisible}
        {dragScope}
        {paneKeyForSession}
        borderTrim={secondChildTrim(node.direction)}
        {onSelect}
        {onClose}
        {onRename}
        {onMoveToWorkflow}
        {readSessionDrag}
        {onPromoteSession}
        {onRatioChange}
        {onSplitSession}
      />
    </div>
  </div>
{/if}

<style>
  .terminal-split,
  .terminal-leaf {
    min-width: 0;
    min-height: 0;
    height: 100%;
  }

  .terminal-split {
    display: flex;
    overflow: hidden;
  }

  .terminal-split.horizontal {
    flex-direction: row;
  }

  .terminal-split.vertical {
    flex-direction: column;
  }

  .split-child {
    min-width: 0;
    min-height: 0;
    overflow: hidden;
  }

  .split-child.first {
    flex: var(--first-ratio) 1 0;
  }

  .split-child.second {
    flex: var(--second-ratio) 1 0;
  }

  :global(.split-divider) {
    flex: 0 0 var(--chrome-pane-divider-width);
  }

  .terminal-leaf {
    display: flex;
    flex-direction: column;
    overflow: hidden;
    background: var(--terminal-bg);
    border: var(--chrome-border-width) solid var(--border-muted);
    border-top: 0;
  }

  .terminal-leaf.trim-right {
    border-right: 0;
  }

  .terminal-leaf.trim-left {
    border-left: 0;
  }

  .terminal-leaf.trim-bottom {
    border-bottom: 0;
  }

  .terminal-leaf.trim-top {
    border-top: 0;
  }

  .leaf-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: 26px;
    flex-shrink: 0;
    border-bottom: var(--chrome-border-width) solid var(--border-muted);
    background: var(--bg-inset);
    cursor: grab;
  }

  .terminal-leaf.active .leaf-header {
    box-shadow: inset 0 var(--chrome-active-accent-width) 0 var(--accent-blue);
  }

  .leaf-title {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    height: 100%;
    padding: 0 8px;
    border: 0;
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-xs);
    font-weight: 650;
    cursor: grab;
  }

  .leaf-title:disabled {
    color: var(--text-muted);
    cursor: default;
    opacity: var(--opacity-disabled);
  }

  .terminal-leaf.active .leaf-title {
    color: var(--text-primary);
  }

  .terminal-leaf-body {
    position: relative;
    display: flex;
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }

  .leaf-icon {
    display: inline-flex;
    color: var(--accent-blue);
    flex-shrink: 0;
  }

  .leaf-label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 22ch;
  }

  .leaf-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--text-muted);
    flex-shrink: 0;
  }

  .leaf-dot.running {
    background: var(--accent-green);
  }

  .leaf-dot.starting {
    background: var(--accent-amber);
  }

  .leaf-actions {
    display: flex;
    align-items: center;
    padding-right: 4px;
  }

  .leaf-action {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 20px;
    height: 20px;
    border: 0;
    border-radius: 3px;
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
  }

  .leaf-action:hover,
  .leaf-action:focus-visible {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    outline: none;
  }

  .leaf-action:disabled {
    cursor: default;
    opacity: var(--opacity-disabled);
  }

  .leaf-action:disabled:hover,
  .leaf-action:disabled:focus-visible {
    background: transparent;
    color: var(--text-muted);
  }

  .missing-session {
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100%;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .split-preview {
    position: absolute;
    z-index: 4;
    inset: 0;
    border: var(--chrome-border-width) solid
      color-mix(in srgb, var(--accent-blue) 44%, transparent);
    opacity: 0;
    pointer-events: none;
    background: color-mix(in srgb, var(--accent-blue) 14%, transparent);
    -webkit-backdrop-filter: blur(3px) saturate(1.05);
    backdrop-filter: blur(3px) saturate(1.05);
    box-shadow: inset 0 0 0 var(--chrome-border-width)
      color-mix(in srgb, var(--accent-blue) 18%, transparent);
    transition:
      opacity 90ms ease,
      inset 90ms ease;
  }

  .terminal-leaf-body.show-drop-targets .split-preview.active {
    opacity: 1;
  }

  .split-preview.top {
    top: 0;
    right: 0;
    bottom: 50%;
    left: 0;
    border-width: 0 0 var(--chrome-active-accent-width);
    border-bottom-color: var(--accent-blue);
  }

  .split-preview.right {
    top: 0;
    right: 0;
    bottom: 0;
    left: 50%;
    border-width: 0 0 0 var(--chrome-active-accent-width);
    border-left-color: var(--accent-blue);
  }

  .split-preview.bottom {
    top: 50%;
    right: 0;
    bottom: 0;
    left: 0;
    border-width: var(--chrome-active-accent-width) 0 0;
    border-top-color: var(--accent-blue);
  }

  .split-preview.left {
    top: 0;
    right: 50%;
    bottom: 0;
    left: 0;
    border-width: 0 var(--chrome-active-accent-width) 0 0;
    border-right-color: var(--accent-blue);
  }
</style>
