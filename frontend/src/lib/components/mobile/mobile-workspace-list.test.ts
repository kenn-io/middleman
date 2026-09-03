import { describe, expect, it } from "vite-plus/test";
import type { WorkspaceListItem } from "../terminal/workspace-list-schema.js";
import {
  groupMobileWorkspaces,
  mobileWorkspaceItemNumber,
  mobileWorkspaceLinkedItem,
  sortMobileWorkspaces,
  workspaceMatchesMobileSearch,
} from "./mobile-workspace-list.js";

function workspace(
  id: string,
  owner: string,
  name: string,
  createdAt: string,
  terminalActivity: string | null,
): WorkspaceListItem {
  return {
    id,
    created_at: createdAt,
    git_head_ref: id === "active" ? "feature/branch" : "main",
    item_number: id === "active" ? 42 : 7,
    item_type: "pull_request",
    platform_host: "github.com",
    repo_name: name,
    repo_owner: owner,
    source_item_visible: true,
    status: "ready",
    tmux_activity_source: "unknown",
    tmux_last_output_at: terminalActivity,
    tmux_working: false,
    worktree_path: `/tmp/${id}`,
    mr_title: id === "active" ? "Feature branch" : "Newest work",
    repo: {
      provider: "github",
      platform_host: "github.com",
      owner,
      name,
      repo_path: `${owner}/${name}`,
    },
  };
}

describe("mobile workspace list model", () => {
  const items = [
    workspace("new", "acme", "widgets", "2026-08-11T12:00:00Z", null),
    workspace("active", "acme", "widgets", "2026-08-10T12:00:00Z", "2026-08-12T12:00:00Z"),
  ];

  it("sorts terminal activity with creation fallback", () => {
    expect(sortMobileWorkspaces(items, "activity").map((item) => item.id)).toEqual(["active", "new"]);
  });

  it("groups hook states by attention priority", () => {
    const hookStates: WorkspaceListItem[] = [
      { ...items[0]!, id: "idle", agent_state: "idle" },
      { ...items[0]!, id: "done", agent_state: "done" },
      { ...items[0]!, id: "working", agent_state: "working" },
      {
        ...items[0]!,
        id: "done-newer-hook",
        agent_state: "done",
        created_at: "2026-08-01T12:00:00Z",
        agent_state_updated_at: "2026-08-13T12:00:00Z",
      },
      { ...items[0]!, id: "input", agent_state: "input" },
      { ...items[0]!, id: "approval", agent_state: "approval" },
      { ...items[0]!, id: "unreported", agent_state: null },
    ];

    expect(sortMobileWorkspaces(hookStates, "agent-status").map((item) => item.id)).toEqual([
      "approval",
      "input",
      "working",
      "done-newer-hook",
      "done",
      "idle",
      "unreported",
    ]);
  });

  it("groups by stable repository identity", () => {
    const groups = groupMobileWorkspaces(items, true);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.label).toBe("acme/widgets");
    expect(groups[0]?.items).toHaveLength(2);
  });

  it("searches title, branch, item number, repository, and Fleet host", () => {
    const remote = { ...items[0]!, fleet_host_key: "phone-dev" };
    expect(workspaceMatchesMobileSearch(items[1]!, "feature branch")).toBe(true);
    expect(workspaceMatchesMobileSearch(items[1]!, "#42")).toBe(true);
    expect(workspaceMatchesMobileSearch(remote, "phone-dev")).toBe(true);
    expect(workspaceMatchesMobileSearch(items[1]!, "unrelated")).toBe(false);
  });

  it("links an issue workspace to the PR it produced once one exists", () => {
    const fromIssue = { ...items[0]!, item_type: "issue" as const, item_number: 7, associated_pr_number: 99 };
    expect(mobileWorkspaceLinkedItem(fromIssue)).toEqual({ itemType: "pr", number: 99 });
    expect(mobileWorkspaceItemNumber(fromIssue)).toBe(99);
    expect(workspaceMatchesMobileSearch(fromIssue, "#99")).toBe(true);
    expect(mobileWorkspaceLinkedItem({ ...fromIssue, associated_pr_number: null })).toEqual({
      itemType: "issue",
      number: 7,
    });
    expect(
      mobileWorkspaceLinkedItem({ ...fromIssue, source_item_visible: false, associated_pr_number: null }),
    ).toBeNull();
  });

  it("does not expose Kata tasks as mobile linked items", () => {
    expect(mobileWorkspaceItemNumber({ ...items[0]!, item_type: "kata_task" })).toBeNull();
  });
});
