import { hydrateTerminalSettings, type TerminalSettingsHydration } from "./terminal-settings-persistence.js";
import { hydrateWorkspaceSettings, type WorkspaceSettingsHydration } from "./workspace-settings-persistence.js";
import { hydrateRoborevSettings, type RoborevSettingsHydration } from "./roborev-settings-persistence.js";
import type { SettingsResponse as GeneratedSettingsResponse } from "../api/generated/models/index.js";

type SettingsResponse = GeneratedSettingsResponse;

// Minimal structural shapes rather than the full store types: this module
// only needs the setters it calls, and narrowing here keeps it testable
// without constructing a whole Provider store graph.
export interface SettingsHydrationStore {
  setConfiguredRepos: (repos: SettingsResponse["repos"]) => void;
  setRepoPresets: (presets: SettingsResponse["repo_presets"]) => void;
  setModeVisibility: (modes: SettingsResponse["modes"]) => void;
  setPullRequestSettings: (pullRequests: SettingsResponse["pull_requests"]) => void;
  setDetailSettings: (detail: SettingsResponse["detail"]) => void;
  setLaunchTargets: (targets: NonNullable<SettingsResponse["launch_targets"]>) => void;
  setWorkspaceSettings: (workspaces: SettingsResponse["workspaces"]) => void;
  getWorkspaceSettings: () => SettingsResponse["workspaces"];
}

export interface ActivityHydrationStore {
  hydrateDefaults: (activity: SettingsResponse["activity"]) => void;
}

export interface IssuesHydrationStore {
  hydrateDefaults: (issues: SettingsResponse["issues"]) => void;
}

export type SettingsHydrationPayload = SettingsResponse;

/**
 * Applies a `GET /settings` payload to the settings, activity, and issues
 * stores. Shared by startup hydration and config-hot-reload hydration so
 * both apply the same fields — a field added to one and not the other
 * silently goes stale after a hot reload until the next full page load.
 */
export function applySettingsHydration(
  stores: {
    settings: SettingsHydrationStore;
    activity: ActivityHydrationStore;
    issues: IssuesHydrationStore;
  },
  payload: SettingsHydrationPayload,
  terminalHydration: TerminalSettingsHydration,
  workspaceHydration: WorkspaceSettingsHydration,
  roborevHydration: RoborevSettingsHydration,
): void {
  stores.settings.setConfiguredRepos(payload.repos);
  stores.settings.setRepoPresets(payload.repo_presets);
  hydrateTerminalSettings(terminalHydration, payload.terminal);
  stores.settings.setModeVisibility(payload.modes);
  stores.settings.setPullRequestSettings(payload.pull_requests);
  stores.settings.setDetailSettings(payload.detail);
  stores.settings.setLaunchTargets(payload.launch_targets ?? []);
  hydrateWorkspaceSettings(workspaceHydration, payload.workspaces);
  hydrateRoborevSettings(roborevHydration, payload.roborev);
  stores.activity.hydrateDefaults(payload.activity);
  stores.issues.hydrateDefaults(payload.issues);
}
