<script lang="ts">
  import { HarnessIcon, type HarnessIconId } from "@kenn-io/kit-ui";
  import type { Snippet } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import MoveIcon from "@lucide/svelte/icons/move";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import SparklesIcon from "@lucide/svelte/icons/sparkles";
  import TerminalIcon from "@lucide/svelte/icons/terminal";
  import HouseIcon from "@lucide/svelte/icons/house";
  import { clearActiveTabbedPanelDrag, readTabbedPanelTabDrag, startTabbedPanelTabDrag, workspaceTabDragScope } from "../shared/tabbed-panel-drag.js";
  import TabbedPanelTree from "../shared/TabbedPanelTree.svelte";
  import type { TabbedPanelDescriptor } from "../shared/tabbed-panel-layout.js";
  import type { SplitDirection, WorkflowNode, WorkflowTabKey } from "./terminal-layout";
  import {
    clearActiveTerminalDrag,
    readWorkflowTabDrag,
    startWorkflowTabDrag,
  } from "./terminal-drag";
  import { launchTargetHarness } from "./agentHarness";

  /**
   * How this tree's session tabs relate to the detail panes they can be promoted
   * to. Set only while the tree is embedded in a detail surface; the standalone
   * Workspaces tab has nowhere to promote to.
   */
  export interface WorkflowPromotion {
    /** The detail-pane key this tab takes when promoted, or null if it has none. */
    paneKeyFor: (tabKey: WorkflowTabKey) => string | null;
    /** The tab a promoted pane key belongs to, or null if it is not this workspace's. */
    tabKeyFor: (paneKey: string) => WorkflowTabKey | null;
  }

  export interface WorkflowTabDescriptor extends TabbedPanelDescriptor {
    key: WorkflowTabKey;
    kind: "home" | "terminal" | "agent" | "plain_shell";
    targetKey?: string | undefined;
    renamable?: boolean | undefined;
    movableToTerminal?: boolean | undefined;
    closable?: boolean | undefined;
  }

  interface Props {
    workspaceId: string;
    // The surface's scope while embedded, so this tree and the detail tree can
    // exchange tabs. Defaults to the workspace's own scope, which is deliberately
    // isolated: the Workspaces tab has no detail panes to trade with.
    dragScope?: string | undefined;
    promotion?: WorkflowPromotion | undefined;
    node: WorkflowNode;
    tabs: WorkflowTabDescriptor[];
    activeTabKey: WorkflowTabKey;
    inputActive?: boolean;
    renderTab: Snippet<[WorkflowTabKey, boolean]>;
    disabled?: boolean;
    onSelectTab?: ((tabKey: WorkflowTabKey) => void) | undefined;
    onFocusPane?: ((tabKey: WorkflowTabKey) => void) | undefined;
    onMoveTabBefore?:
      | ((sourceTabKey: WorkflowTabKey, targetTabKey: WorkflowTabKey) => void)
      | undefined;
    onAppendTabToLeaf?: ((sourceTabKey: WorkflowTabKey, leafID: string) => void) | undefined;
    onSplitTab?:
      | ((
          sourceTabKey: WorkflowTabKey,
          leafID: string,
          direction: SplitDirection,
          placement: "before" | "after",
        ) => void)
      | undefined;
    onMoveTabToTerminal?: ((tabKey: WorkflowTabKey) => void) | undefined;
    onCloseTab?: ((tabKey: WorkflowTabKey) => void) | undefined;
    onRenameTab?: ((tabKey: WorkflowTabKey) => void) | undefined;
    onRatioChange?: ((splitID: string, ratio: number) => void) | undefined;
  }

  const {
    workspaceId,
    dragScope = undefined,
    promotion = undefined,
    node,
    tabs,
    activeTabKey,
    inputActive = true,
    renderTab: renderWorkflowTab,
    disabled = false,
    onSelectTab,
    onFocusPane,
    onMoveTabBefore,
    onAppendTabToLeaf,
    onSplitTab,
    onMoveTabToTerminal,
    onCloseTab,
    onRenameTab,
    onRatioChange,
  }: Props = $props();

  const resolvedDragScope = $derived(dragScope ?? workspaceTabDragScope(workspaceId));

  function workflowTabFrom(tabKey: string): WorkflowTabKey {
    return tabKey as WorkflowTabKey;
  }

  // Two payloads, because this tree keeps its own drag module for intra-workspace
  // drops (the dock reads it too) while the detail tree only understands the
  // shared one. A promoted pane's key is the canonical cross-tree form, which a
  // workspace tab key is not.
  function startTabDrag(event: DragEvent, tab: TabbedPanelDescriptor): void {
    const tabKey = workflowTabFrom(tab.key);
    startWorkflowTabDrag(event, { workspaceId, tabKey });
    const paneKey = promotion?.paneKeyFor(tabKey) ?? null;
    if (paneKey !== null) {
      startTabbedPanelTabDrag(event, { scope: resolvedDragScope, tabKey: paneKey }, "Kenn Forge session tab");
    }
  }

  function readDraggedTab(event: DragEvent): WorkflowTabKey | null {
    const own = readWorkflowTabDrag(event, workspaceId);
    if (own !== null) return own;
    // A promoted pane coming home. The drop handlers demote it before placing it,
    // so its stored placement is only overridden when the user names a target.
    const foreign = readTabbedPanelTabDrag(event, resolvedDragScope);
    return foreign === null ? null : (promotion?.tabKeyFor(foreign) ?? null);
  }

  function clearDrag(): void {
    clearActiveTerminalDrag();
    clearActiveTabbedPanelDrag();
  }

  function splitDirectionFrom(direction: string): SplitDirection {
    return direction === "vertical" ? "vertical" : "horizontal";
  }

  function tabKind(tab: TabbedPanelDescriptor): WorkflowTabDescriptor["kind"] {
    return (tab as WorkflowTabDescriptor).kind;
  }

  function tabHarness(tab: TabbedPanelDescriptor): HarnessIconId | null {
    const workflowTab = tab as WorkflowTabDescriptor;
    if (workflowTab.kind !== "agent" || workflowTab.targetKey === undefined) return null;
    return launchTargetHarness({ kind: "agent", key: workflowTab.targetKey });
  }

  function isRenamable(tab: TabbedPanelDescriptor): boolean {
    return (tab as WorkflowTabDescriptor).renamable === true;
  }

  function isMovableToTerminal(tab: TabbedPanelDescriptor): boolean {
    return (tab as WorkflowTabDescriptor).movableToTerminal === true;
  }

  function isClosable(tab: TabbedPanelDescriptor): boolean {
    return (tab as WorkflowTabDescriptor).closable === true;
  }
</script>

<!-- The scope is namespaced because it IS compared: embedded, it is the surface's
     own scope, and plain string equality is all that keeps an unrelated tree's
     tab from landing here. -->
<TabbedPanelTree
  dragScope={resolvedDragScope}
  {node}
  {tabs}
  {activeTabKey}
  {inputActive}
  {disabled}
  tablistLabel="Workflow group tabs"
  leafLabel="Workflow group"
  dropTargetsLabel="Workflow group drop targets"
  resizeLabel="Resize workflow split"
  onSelectTab={(tabKey) => {
    if (disabled) return;
    onSelectTab?.(workflowTabFrom(tabKey));
  }}
  onFocusPane={(tabKey) => {
    onFocusPane?.(workflowTabFrom(tabKey));
  }}
  onMoveTabBefore={(source, target) =>
    !disabled && onMoveTabBefore?.(workflowTabFrom(source), workflowTabFrom(target))}
  onAppendTabToLeaf={(source, leafID) => !disabled && onAppendTabToLeaf?.(workflowTabFrom(source), leafID)}
  onSplitTab={(source, leafID, direction, placement) =>
    !disabled && onSplitTab?.(workflowTabFrom(source), leafID, splitDirectionFrom(direction), placement)}
  onTabDoubleClick={(tabKey) => {
    if (disabled) return;
    onRenameTab?.(workflowTabFrom(tabKey));
  }}
  onRatioChange={(splitID, ratio) => {
    if (disabled) return;
    onRatioChange?.(splitID, ratio);
  }}
  onStartTabDrag={(event, tab) => {
    if (disabled) return;
    startTabDrag(event, tab);
  }}
  onReadDraggedTab={(event) => (disabled ? null : readDraggedTab(event))}
  onClearDrag={clearDrag}
>
  {#snippet renderTab(tabKey, active)}
    {@render renderWorkflowTab(workflowTabFrom(tabKey), active)}
  {/snippet}

  {#snippet tabIcon(tab)}
    {@const harness = tabHarness(tab)}
    {#if tabKind(tab) === "home"}
      <HouseIcon size="13" strokeWidth="2" />
    {:else if tabKind(tab) === "plain_shell" || tabKind(tab) === "terminal"}
      <TerminalIcon size="13" strokeWidth="2" />
    {:else if harness}
      <HarnessIcon {harness} size={13} decorative />
    {:else}
      <SparklesIcon size="13" strokeWidth="2" />
    {/if}
  {/snippet}

  {#snippet tabActions(tab)}
    {@const tabKey = workflowTabFrom(tab.key)}
    {#if isRenamable(tab)}
      <button
        class="tabbed-panel-tab-tool"
        title="Rename"
        aria-label={`Rename ${tab.label}`}
        disabled={disabled}
        onclick={() => {
          if (disabled) return;
          onRenameTab?.(tabKey);
        }}
      >
        <PencilIcon size="11" strokeWidth="2.2" aria-hidden="true" />
      </button>
    {/if}
    {#if isMovableToTerminal(tab)}
      <button
        class="tabbed-panel-tab-tool"
        title="Move to terminal"
        aria-label={`Move ${tab.label} to terminal`}
        disabled={disabled}
        onclick={() => {
          if (disabled) return;
          onMoveTabToTerminal?.(tabKey);
        }}
      >
        <MoveIcon size="11" strokeWidth="2.2" aria-hidden="true" />
      </button>
    {/if}
    {#if isClosable(tab)}
      <button
        class="tabbed-panel-tab-tool"
        title="Close"
        aria-label={`Close ${tab.label}`}
        disabled={disabled}
        onclick={() => {
          if (disabled) return;
          onCloseTab?.(tabKey);
        }}
      >
        <XIcon size="11" strokeWidth="2.3" aria-hidden="true" />
      </button>
    {/if}
  {/snippet}
</TabbedPanelTree>
