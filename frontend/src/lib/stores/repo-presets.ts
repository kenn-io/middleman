import type { RepoPreset } from "../api/types.js";
import type { RepoPresetRepository as GeneratedRepoPresetRepository } from "../api/generated/models/index.js";
import { parseRepoFilterValue, serializeRepoFilterValue } from "./filter.svelte.js";

export type RepoPresetRepository = GeneratedRepoPresetRepository;

export interface RepoPresetCatalogEntry extends RepoPresetRepository {
  value: string;
}

function selectionKey(repos: readonly string[]): string {
  return [...new Set(repos)].sort().join("\n");
}

export function findMatchingRepoPreset(
  presets: readonly RepoPreset[],
  selected: string | undefined,
  availableRepos: readonly RepoPresetCatalogEntry[],
  affinity?: string | undefined,
): RepoPreset | undefined {
  const selectedRepos = parseRepoFilterValue(selected);
  if (selectedRepos.length === 0) return undefined;
  const selectedKey = selectionKey(selectedRepos);
  const matching = presets.filter(
    (preset) => selectionKey(parseRepoFilterValue(projectRepoPresetSelection(preset, availableRepos))) === selectedKey,
  );
  if (matching.length < 2 || !affinity) return matching[0];
  return matching.find((preset) => preset.name.toLowerCase() === affinity.toLowerCase()) ?? matching[0];
}

export function projectRepoPresetSelection(
  preset: RepoPreset,
  availableRepos: readonly RepoPresetCatalogEntry[],
): string | undefined {
  const values = preset.repos.flatMap((repo) => {
    const match = availableRepos.find(
      (candidate) =>
        candidate.provider === repo.provider &&
        candidate.platform_host === repo.platform_host &&
        candidate.platform_repo_id === repo.platform_repo_id,
    );
    return match ? [match.value] : [];
  });
  if (values.length === 0) return undefined;
  return serializeRepoFilterValue(values);
}

export function repoPresetRepositoriesForSelection(
  selected: string | undefined,
  availableRepos: readonly RepoPresetCatalogEntry[],
): RepoPresetRepository[] | undefined {
  const values = parseRepoFilterValue(selected);
  const repos = values.map((value) => availableRepos.find((repo) => repo.value === value));
  if (repos.some((repo) => !repo?.platform_repo_id)) return undefined;
  return repos.map((repo) => {
    if (!repo) throw new Error("repository catalog changed during preset save");
    return {
      provider: repo.provider,
      platform_host: repo.platform_host,
      platform_repo_id: repo.platform_repo_id,
      repo_path: repo.repo_path,
    };
  });
}

export function preferredRepoPreset(
  presets: readonly RepoPreset[],
  selected: string | undefined,
  affinity: string | undefined,
  availableRepos: readonly RepoPresetCatalogEntry[],
): RepoPreset | undefined {
  const matching = findMatchingRepoPreset(presets, selected, availableRepos, affinity);
  if (matching) return matching;
  if (!affinity) return undefined;
  const affinityKey = affinity.toLowerCase();
  return presets.find((preset) => preset.name.toLowerCase() === affinityKey);
}
