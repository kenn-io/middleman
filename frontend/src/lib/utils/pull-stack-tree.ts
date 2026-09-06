import type { PullRequest } from "../api/types.js";

export interface PullStackGroup {
  key: string;
  members: PullRequest[];
}

/** Keep each stack at its newest matching row, ordered from its root upward. */
export function groupPullStacks(items: PullRequest[]): PullStackGroup[] {
  const groups = new Map<string, PullStackGroup>();
  for (const pr of items) {
    const key = pr.stack ? JSON.stringify([pr.RepoID, pr.stack.stack_id]) : `pull:${pr.ID}`;
    const group = groups.get(key);
    if (group) group.members.push(pr);
    else groups.set(key, { key, members: [pr] });
  }
  for (const group of groups.values()) {
    group.members.sort((a, b) => (a.stack?.position ?? 0) - (b.stack?.position ?? 0));
  }
  return [...groups.values()];
}

export interface PullSidebarRow {
  pr: PullRequest;
  depth: number;
  stackKey: string;
  memberCount: number;
  expanded: boolean;
}
