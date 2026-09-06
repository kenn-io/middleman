export interface SettingsPanelMeta {
  id: string;
  label: string;
  title: string;
  group: string;
  description: string;
  /** Extra search-only terms; never rendered. */
  keywords: string;
}

export const SETTINGS_PANELS: SettingsPanelMeta[] = [
  {
    id: "settings-repositories",
    label: "Repositories",
    title: "Repositories",
    group: "Providers",
    description: "Tracked repositories and import tools",
    keywords: "repos repositories providers github gitlab forgejo gitea import glob",
  },
  {
    id: "settings-pull-requests",
    label: "Pull requests",
    title: "Pull request safeguards",
    group: "Workflow",
    description: "Merge safeguards for stacked branches",
    keywords: "pull requests merge stack stacked branches safety",
  },
  {
    id: "settings-detail",
    label: "Detail views",
    title: "Detail view performance",
    group: "Workflow",
    description: "Initial timeline rendering limit",
    keywords: "detail timeline entries events performance load full",
  },
  {
    id: "settings-activity",
    label: "Activity",
    title: "Activity feed defaults",
    group: "Workflow",
    description: "Default activity feed filters",
    keywords: "activity feed defaults filters time range closed bots",
  },
  {
    id: "settings-workspaces",
    label: "Workspaces",
    title: "Workspace creation",
    group: "Workspace",
    description: "Behavior when creating workspaces from provider items",
    keywords: "workspace create pull request issue assign self assignee ownership",
  },
  {
    id: "settings-terminal",
    label: "Terminal",
    title: "Workspace terminal",
    group: "Workspace",
    description: "Workspace terminal appearance and behavior",
    keywords: "workspace terminal font cursor scrollback ligatures",
  },
  {
    id: "settings-kata-projects",
    label: "Kata mappings",
    title: "Kata project mappings",
    group: "Workspace",
    description: "Kata project repository identity overrides",
    keywords: "kata projects repositories mappings workspaces daemon project uid",
  },
  {
    id: "settings-agents",
    label: "Workspace agents",
    title: "Workspace agents",
    group: "Workspace",
    description: "Agent commands available in workspaces",
    keywords: "workspace agents codex claude gemini opencode aider binary arguments",
  },
  {
    id: "settings-fleet",
    label: "Fleet federation",
    title: "Fleet federation",
    group: "Workspace",
    description: "Remote hosts and fleet membership",
    keywords: "fleet federation hub spokes https enrollment membership",
  },
  {
    id: "settings-modes",
    label: "Visible modes",
    title: "Visible modes",
    group: "Navigation",
    description: "Modes shown in the app header",
    keywords: "visible modes navigation tabs prs issues reviews docs kata actions github workflows release dispatch",
  },
  {
    id: "settings-mcp",
    label: "MCP companion",
    title: "MCP companion",
    group: "System",
    description: "Local MCP access for external clients",
    keywords: "mcp companion server streamable http endpoint agents clients cache port",
  },
];
