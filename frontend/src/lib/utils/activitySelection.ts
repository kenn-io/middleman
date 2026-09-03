import { buildIssueRoute, buildPullRequestFilesRoute, buildPullRequestRoute, type RoutedItemRef } from "../routes.js";

export type ActivitySelectionItemType = "pr" | "issue";
export type ActivityDetailTab = "conversation" | "files";

export type ActivityItemSelection = RoutedItemRef & {
  detailTab: ActivityDetailTab;
};

/**
 * A default-branch commit selected from the Activity feed. It carries repo
 * identity, the branch it landed on, and the commit SHA — everything
 * CommitDiffPanel needs and nothing derived. `title` is display-only and is
 * NOT serialized: putting the commit subject in the query string would make
 * the URL a second owner of text the feed already supplies, and a restored
 * pane falls back to the short SHA exactly as handleSelectBranchCommit does
 * for a commit with no body preview.
 */
export type ActivityCommitSelection = {
  itemType: "commit";
  provider: string;
  platformHost?: string | undefined;
  repoPath: string;
  owner: string;
  name: string;
  branchName: string;
  commitSha: string;
  title: string;
};

export type ActivitySelection = ActivityItemSelection | ActivityCommitSelection;

type Destination = "pulls" | "issues";

function searchParams(search: string): URLSearchParams {
  return new URLSearchParams(search.startsWith("?") ? search.slice(1) : search);
}

export function parseActivitySelection(search: string): ActivitySelection | null {
  const sp = searchParams(search);
  const selected = sp.get("selected");
  if (!selected) return null;

  const provider = sp.get("provider")?.trim();
  const platformHost = sp.get("platform_host") ?? undefined;
  const repoPath = sp.get("repo_path")?.replace(/^\/+|\/+$/g, "");
  if (!provider || !repoPath) return null;

  const pathParts = repoPath.split("/").filter(Boolean);
  if (pathParts.length < 2) return null;
  const name = pathParts[pathParts.length - 1]!;
  const owner = pathParts.slice(0, -1).join("/");

  const commitMatch = selected.match(/^commit:([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$/);
  if (commitMatch) {
    const commitSha = commitMatch[1]!.toLowerCase();
    return {
      itemType: "commit",
      owner,
      name,
      repoPath,
      commitSha,
      branchName: sp.get("branch")?.trim() || "default branch",
      title: commitSha.slice(0, 12),
      provider,
      ...(platformHost && { platformHost }),
    };
  }

  const providerMatch = selected.match(/^(pr|issue):(\d+)$/);
  if (!providerMatch) return null;

  const itemType = providerMatch[1] as ActivitySelectionItemType;
  const number = parseInt(providerMatch[2]!, 10);

  const detailTab: ActivityDetailTab =
    itemType === "pr" && sp.get("selected_tab") === "files" ? "files" : "conversation";

  return {
    itemType,
    owner,
    name,
    number,
    detailTab,
    provider,
    ...(platformHost && { platformHost }),
    repoPath,
  };
}

export function buildActivitySelectionSearch(
  currentSearch: string,
  selection: ActivitySelection | null,
): URLSearchParams {
  const sp = searchParams(currentSearch);
  sp.delete("selected");
  sp.delete("selected_tab");
  sp.delete("provider");
  sp.delete("platform_host");
  sp.delete("repo_path");
  sp.delete("branch");

  if (!selection) return sp;

  if (selection.itemType === "commit") {
    sp.set("selected", `commit:${selection.commitSha}`);
    sp.set("provider", selection.provider);
    if (selection.platformHost) sp.set("platform_host", selection.platformHost);
    sp.set("repo_path", selection.repoPath);
    sp.set("branch", selection.branchName);
    return sp;
  }

  sp.set("selected", `${selection.itemType}:${selection.number}`);
  sp.set("provider", selection.provider);
  if (selection.platformHost) sp.set("platform_host", selection.platformHost);
  sp.set("repo_path", selection.repoPath);
  if (selection.itemType === "pr" && selection.detailTab === "files") {
    sp.set("selected_tab", "files");
  }
  return sp;
}

export function activitySelectionToRoute(selection: ActivitySelection | null, destination: Destination): string | null {
  if (!selection) return null;
  if (selection.itemType === "commit") return null;
  if (destination === "pulls") {
    if (selection.itemType !== "pr") return null;
    return selection.detailTab === "files" ? buildPullRequestFilesRoute(selection) : buildPullRequestRoute(selection);
  }

  if (selection.itemType !== "issue") return null;
  return buildIssueRoute(selection);
}
