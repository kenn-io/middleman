import { createRepoLabelFormatter, repoIdentityKey } from "../../utils/repo-label.js";
import type { WorkspaceListItem } from "../terminal/workspace-list-schema.js";
import {
  workspaceAgentStatePriority,
  workspaceAgentStateSortTime,
  type WorkspaceListSort,
} from "../terminal/workspaceListSort.js";

export interface MobileWorkspaceGroup {
  key: string;
  label: string;
  items: WorkspaceListItem[];
}

function timeValue(value: string | null | undefined): number {
  if (!value) return 0;
  const milliseconds = Date.parse(value);
  return Number.isNaN(milliseconds) ? 0 : milliseconds;
}

export function mobileWorkspaceDisplayName(workspace: WorkspaceListItem): string {
  if (workspace.item_type === "kata_task") {
    return workspace.kata?.title?.trim() || workspace.git_head_ref;
  }
  return workspace.mr_title?.trim() || workspace.git_head_ref;
}

// Mirrors mobileWorkspaceLinkedItem for list rows: the PR a workspace produced
// outranks the issue it was created from, so the badge, search, and provider
// link all point at the same item the detail route opens.
export function mobileWorkspaceLinkedItem(workspace: WorkspaceListItem): {
  itemType: "pr" | "issue";
  number: number;
} | null {
  if (workspace.item_type === "kata_task") return null;
  if (workspace.item_type === "pull_request") {
    return workspace.source_item_visible && workspace.item_number > 0
      ? { itemType: "pr", number: workspace.item_number }
      : null;
  }
  const associated = workspace.associated_pr_number;
  if (associated !== null && associated !== undefined && associated > 0) {
    return { itemType: "pr", number: associated };
  }
  if (workspace.item_type === "issue" && workspace.source_item_visible && workspace.item_number > 0) {
    return { itemType: "issue", number: workspace.item_number };
  }
  return null;
}

export function mobileWorkspaceItemNumber(workspace: WorkspaceListItem): number | null {
  return mobileWorkspaceLinkedItem(workspace)?.number ?? null;
}

export function workspaceMatchesMobileSearch(workspace: WorkspaceListItem, rawQuery: string): boolean {
  const query = rawQuery.trim().toLowerCase();
  if (!query) return true;
  const number = mobileWorkspaceItemNumber(workspace);
  const values = [
    mobileWorkspaceDisplayName(workspace),
    workspace.git_head_ref,
    workspace.git_head_ref.replace(/^refs\/heads\//, ""),
    workspace.platform_host,
    workspace.repo_owner,
    workspace.repo_name,
    workspace.repo?.repo_path,
    `${workspace.repo_owner}/${workspace.repo_name}`,
    workspace.fleet_host_key,
    workspace.kata?.short_id,
    workspace.kata?.qualified_id,
    workspace.kata?.title,
    workspace.item_type === "adhoc" ? "new work" : undefined,
    number === null ? undefined : String(number),
    number === null ? undefined : `#${number}`,
  ];
  return values.some((value) => value?.toLowerCase().includes(query));
}

export function sortMobileWorkspaces(
  workspaces: readonly WorkspaceListItem[],
  sort: Exclude<WorkspaceListSort, "repo">,
): WorkspaceListItem[] {
  if (sort === "agent-status") {
    return [...workspaces].sort(
      (left, right) =>
        workspaceAgentStatePriority(right.agent_state) - workspaceAgentStatePriority(left.agent_state) ||
        timeValue(workspaceAgentStateSortTime(right)) - timeValue(workspaceAgentStateSortTime(left)) ||
        left.id.localeCompare(right.id),
    );
  }
  const stamp =
    sort === "activity"
      ? (workspace: WorkspaceListItem) => timeValue(workspace.tmux_last_output_at) || timeValue(workspace.created_at)
      : sort === "item-activity"
        ? (workspace: WorkspaceListItem) =>
            timeValue(workspace.item_last_activity_at) || timeValue(workspace.created_at)
        : (workspace: WorkspaceListItem) => timeValue(workspace.created_at);
  return [...workspaces].sort((left, right) => stamp(right) - stamp(left) || left.id.localeCompare(right.id));
}

export function groupMobileWorkspaces(
  workspaces: readonly WorkspaceListItem[],
  showOrgNames: boolean,
): MobileWorkspaceGroup[] {
  const identities = workspaces.map((workspace) => ({
    provider: workspace.repo?.provider ?? "",
    platformHost: workspace.platform_host,
    owner: workspace.repo_owner,
    name: workspace.repo_name,
    repoPath: workspace.repo?.repo_path,
  }));
  const formatter = createRepoLabelFormatter(identities, { showOrgNames });
  const groups = new Map<string, MobileWorkspaceGroup>();
  for (const workspace of workspaces) {
    const identity = {
      provider: workspace.repo?.provider ?? "",
      platformHost: workspace.platform_host,
      owner: workspace.repo_owner,
      name: workspace.repo_name,
      repoPath: workspace.repo?.repo_path,
    };
    const key = repoIdentityKey(identity);
    const existing = groups.get(key);
    if (existing) {
      existing.items.push(workspace);
    } else {
      groups.set(key, { key, label: formatter.format(identity), items: [workspace] });
    }
  }
  return Array.from(groups.values());
}
