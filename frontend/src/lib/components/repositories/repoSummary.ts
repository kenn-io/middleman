import type {
  ProviderCapabilities,
  RepoSummary,
  RepoSummaryAuthor,
  RepoSummaryCommitPointResponse,
  RepoSummaryIssue,
  RepoSummaryReleaseResponse,
} from "../../api/types.js";
import { canonicalProvider } from "../../api/provider-routes.js";

export type RepoSummaryCard = Omit<RepoSummary, "active_authors" | "recent_issues" | "commit_timeline" | "releases"> & {
  active_authors: RepoSummaryAuthor[];
  recent_issues: RepoSummaryIssue[];
  commit_timeline: RepoSummaryCommitPointResponse[];
  releases: RepoSummaryReleaseResponse[];
};

export interface RepoMetric {
  label: string;
  value: number;
  tone?: "neutral" | "blue" | "amber" | "green" | "red";
  onclick?: () => void;
}

export type RepoFilter = "all" | "prs" | "issues" | "stale";
export type RepoSort = "name" | "open-prs" | "open-issues" | "activity" | "stale";

export const staleReleaseCommitThreshold = 50;

export const defaultProviderCapabilities: ProviderCapabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_issue_pr_references: false,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_workflows: false,
  read_workflow_runs: false,
  workflow_dispatch: false,
  read_labels: false,
  read_markdown_images: false,
  read_authenticated_user: false,
  comment_mutation: true,
  state_mutation: true,
  merge_mutation: true,
  review_mutation: true,
  workflow_approval: true,
  ready_for_review: true,
  draft_mutation: true,
  issue_mutation: true,
  label_mutation: false,
  assignee_mutation: false,
  reviewer_mutation: false,
  thread_reply: false,
  thread_resolve: false,
  review_draft_mutation: false,
  review_thread_resolution: false,
  review_suggestion_application: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: false,
  supported_review_actions: [],
};

export function repoKey(summary: {
  platform_host?: string;
  default_platform_host?: string | undefined;
  owner: string;
  name: string;
}): string {
  if (
    summary.platform_host &&
    shouldShowPlatformHost({
      platform_host: summary.platform_host,
      default_platform_host: summary.default_platform_host,
    })
  ) {
    return `${summary.platform_host}/${summary.owner}/${summary.name}`;
  }
  return `${summary.owner}/${summary.name}`;
}

export function repoStateKey(summary: {
  repo: { provider: string; platform_host: string; repo_path: string };
}): string {
  return providerRepoStateKey(summary.repo);
}

export function providerRepoStateKey(repo: { provider: string; platform_host: string; repo_path: string }): string {
  const provider = canonicalProvider(repo.provider.trim());
  const host = repo.platform_host.trim().toLowerCase();
  const rawPath = repo.repo_path.trim().replace(/^\/+|\/+$/g, "");
  const path = provider === "github" ? rawPath.toLowerCase() : rawPath;
  return `${provider}|${host}/${path}`;
}

export function shouldShowPlatformHost(summary: {
  platform_host: string;
  default_platform_host?: string | undefined;
}): boolean {
  const host = summary.platform_host.toLowerCase();
  const defaultHost = summary.default_platform_host?.toLowerCase();
  if (!defaultHost) return true;
  return host !== defaultHost;
}

export function localDateTimeLabel(dateStr: string): string {
  return new Date(dateStr).toLocaleString();
}

export function displayReleaseName(release: RepoSummaryReleaseResponse | undefined): string {
  if (!release) return "No release";
  return release.tag_name || release.name || "Release";
}

export function isStaleRelease(summary: RepoSummaryCard): boolean {
  return summary.latest_release !== undefined && (summary.commits_since_release ?? 0) >= staleReleaseCommitThreshold;
}

export function normalizeSummaries(data: RepoSummary[] | null | undefined): RepoSummaryCard[] {
  return (data ?? []).map((summary) => {
    if (!summary.repo) {
      throw new Error("repo summary missing provider repo identity");
    }
    return {
      ...summary,
      repo: {
        ...summary.repo,
        capabilities: summary.repo.capabilities ?? defaultProviderCapabilities,
      },
      default_platform_host: summary.default_platform_host,
      active_authors: summary.active_authors ?? [],
      recent_issues: summary.recent_issues ?? [],
      commit_timeline: summary.commit_timeline ?? [],
      releases: summary.releases ?? [],
    };
  });
}
