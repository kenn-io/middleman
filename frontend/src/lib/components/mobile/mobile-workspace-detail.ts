import { Effect } from "effect";
import {
  executeGeneratedApiRequest,
  executeOpaqueGeneratedApiRequest,
  type GeneratedApi,
} from "../../api/generated-api.js";
import { decodeWorkspaceDetail, type WorkspaceDetail } from "../terminal/workspace-detail.js";

export function loadMobileWorkspaceDetail(
  workspaceId: string,
  hostKey?: string,
): Effect.Effect<WorkspaceDetail, unknown, GeneratedApi> {
  if (hostKey) {
    return executeOpaqueGeneratedApiRequest("load mobile Fleet workspace", (client, signal) =>
      client.FleetService.getFleetWorkspace({ hostKey: hostKey, id: workspaceId }, { signal }),
    ).pipe(Effect.flatMap((payload) => decodeWorkspaceDetail(payload, hostKey)));
  }
  return executeGeneratedApiRequest("load mobile workspace", (client, signal) =>
    client.WorkspacesService.getWorkspace({ id: workspaceId }, { signal }),
  ).pipe(Effect.flatMap((payload) => decodeWorkspaceDetail(payload)));
}

// The phone shell has one linked-item slot. A workspace created from an issue
// gains a PR once its branch is pushed, and that PR is what a maintainer
// triages from a phone, so it wins over the originating issue. The daemon
// already clears associated_pr_number when the PR is not visible.
export function mobileWorkspaceLinkedItem(workspace: WorkspaceDetail): {
  itemType: "pr" | "issue";
  number: number;
} | null {
  if (workspace.item_type === "kata_task") return null;
  if (workspace.item_type === "pull_request") {
    return workspace.item_number > 0 ? { itemType: "pr", number: workspace.item_number } : null;
  }
  const associated = workspace.associated_pr_number;
  if (associated !== null && associated !== undefined && associated > 0) {
    return { itemType: "pr", number: associated };
  }
  if (workspace.item_type === "issue" && workspace.item_number > 0) {
    return { itemType: "issue", number: workspace.item_number };
  }
  return null;
}

export function mobileWorkspaceIdentity(workspace: WorkspaceDetail, hostKey?: string): string {
  const local = `${workspace.repo_owner}/${workspace.repo_name} · ${workspace.git_head_ref}`;
  return hostKey ? `${local} · ${hostKey}` : local;
}
