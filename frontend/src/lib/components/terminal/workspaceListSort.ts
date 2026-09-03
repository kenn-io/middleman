import type { WorkspaceListItem } from "./workspace-list-schema.js";

export type WorkspaceListSort = "repo" | "created" | "activity" | "item-activity" | "agent-status";

export interface WorkspaceListDisplayOptions {
  showOrgNames: boolean;
  showDiffStats: boolean;
}

export const workspaceListSortOptions: {
  value: WorkspaceListSort;
  label: string;
  description: string;
}[] = [
  {
    value: "repo",
    label: "Org / repo",
    description: "Group by repository, with newest workspaces first inside each repo.",
  },
  {
    value: "created",
    label: "Created",
    description: "Sort all workspaces by when the workspace was created.",
  },
  {
    value: "activity",
    label: "Activity",
    description: "Sort by latest terminal output, falling back to workspace creation.",
  },
  {
    value: "item-activity",
    label: "Item activity",
    description: "Sort by latest linked PR or issue activity, falling back to workspace creation.",
  },
  {
    value: "agent-status",
    label: "Agent status",
    description: "Group by agent status, with workspaces needing attention first.",
  },
];

export const defaultWorkspaceListSort: WorkspaceListSort = "repo";

export const defaultWorkspaceListDisplayOptions: WorkspaceListDisplayOptions = {
  showOrgNames: true,
  showDiffStats: true,
};

const sortStorageKey = "kenn-forge:workspaceListSort";
const displayStorageKey = "kenn-forge:workspaceListDisplayOptions";

const validSorts = new Set<WorkspaceListSort>(workspaceListSortOptions.map((option) => option.value));

export function workspaceAgentStatePriority(state: WorkspaceListItem["agent_state"]): number {
  switch (state) {
    case "approval":
      return 5;
    case "input":
      return 4;
    case "working":
      return 3;
    case "done":
      return 2;
    case "idle":
      return 1;
    default:
      return 0;
  }
}

function workspaceAgentStateTimestamp(workspace: WorkspaceListItem): { at: string; label: string } {
  const hookTimestamp = workspace.agent_state_updated_at?.trim();
  if (hookTimestamp) return { at: hookTimestamp, label: "Agent hook" };

  const itemTimestamp = workspace.item_last_activity_at?.trim();
  return itemTimestamp ? { at: itemTimestamp, label: "Item activity" } : { at: workspace.created_at, label: "Created" };
}

export function workspaceListSortTimestamp(
  workspace: WorkspaceListItem,
  sort: WorkspaceListSort,
): { at: string; label: string } | null {
  switch (sort) {
    case "created":
      return { at: workspace.created_at, label: "Created" };
    case "activity":
      return workspace.tmux_last_output_at
        ? { at: workspace.tmux_last_output_at, label: "Terminal activity" }
        : { at: workspace.created_at, label: "Created" };
    case "item-activity":
      return workspace.item_last_activity_at
        ? { at: workspace.item_last_activity_at, label: "Item activity" }
        : { at: workspace.created_at, label: "Created" };
    case "agent-status":
      return workspaceAgentStateTimestamp(workspace);
    default:
      return null;
  }
}

export function workspaceAgentStateSortTime(workspace: WorkspaceListItem): string {
  return workspaceAgentStateTimestamp(workspace).at;
}

function getStorage(): Storage | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

export function loadWorkspaceListSort(): WorkspaceListSort {
  const storage = getStorage();
  if (!storage) return defaultWorkspaceListSort;

  try {
    const value = storage.getItem(sortStorageKey) as WorkspaceListSort | null;
    return value && validSorts.has(value) ? value : defaultWorkspaceListSort;
  } catch {
    return defaultWorkspaceListSort;
  }
}

export function saveWorkspaceListSort(sort: WorkspaceListSort): void {
  const storage = getStorage();
  if (!storage) return;

  try {
    storage.setItem(sortStorageKey, sort);
  } catch {
    // Storage blocked - the sort still applies for the current page instance.
  }
}

export function loadWorkspaceListDisplayOptions(): WorkspaceListDisplayOptions {
  const storage = getStorage();
  if (!storage) return { ...defaultWorkspaceListDisplayOptions };

  try {
    const raw = storage.getItem(displayStorageKey);
    if (!raw) return { ...defaultWorkspaceListDisplayOptions };

    const value = JSON.parse(raw) as Partial<WorkspaceListDisplayOptions>;
    return {
      showOrgNames:
        typeof value.showOrgNames === "boolean" ? value.showOrgNames : defaultWorkspaceListDisplayOptions.showOrgNames,
      showDiffStats:
        typeof value.showDiffStats === "boolean"
          ? value.showDiffStats
          : defaultWorkspaceListDisplayOptions.showDiffStats,
    };
  } catch {
    return { ...defaultWorkspaceListDisplayOptions };
  }
}

export function saveWorkspaceListDisplayOptions(options: WorkspaceListDisplayOptions): void {
  const storage = getStorage();
  if (!storage) return;

  try {
    storage.setItem(displayStorageKey, JSON.stringify(options));
  } catch {
    // Storage blocked - the display options still apply for this page instance.
  }
}
