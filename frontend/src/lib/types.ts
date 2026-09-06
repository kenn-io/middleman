import type { WorkspaceItemIdentity } from "./workspace-inline.js";

export interface Action {
  id: string;
  label: string;
  icon?: string;
  handler: (context: ActionContext) => void | Promise<void>;
}

export interface ActionContext {
  surface: string;
  owner: string;
  name: string;
  number: number;
  meta?: Record<string, unknown>;
}

export interface ActionRegistry {
  pull?: Action[];
  issue?: Action[];
  activity?: Action[];
}

export interface NavigateEvent {
  path: string;
  route: {
    page: "pulls" | "issues" | "activity" | "diff" | "reviews" | "inbox";
    view?: string;
    tab?: string;
    presentation?: "fullLayout" | "focus";
    owner?: string;
    name?: string;
    number?: number;
    jobId?: number;
  };
  repo?: { provider?: string; host?: string; platform_host?: string; repo_path?: string; owner: string; name: string };
}

export interface NavigateOptions {
  /**
   * Rewrite the current history entry instead of pushing a new one. Used when a
   * route change only records which of several simultaneously visible panes the
   * user is in, so moving between them does not fill the Back stack.
   */
  replace?: boolean;
  /**
   * History state to store on the entry this navigation writes, such as the
   * phone list origin that a detail's Back control reads. Left off the entry
   * when omitted.
   */
  state?: Record<string, unknown>;
}

export type NavigateCallback = (event: string | NavigateEvent, options?: NavigateOptions) => void;

export interface WorkspaceCommandResult {
  ok: boolean;
  message?: string;
}

export type WorkspaceCommandCallback = (
  command: string,
  payload: Record<string, unknown>,
) => WorkspaceCommandResult | Promise<WorkspaceCommandResult>;

export type WorkspaceDeletedCallback = (
  workspaceId: string,
  workspaceHostKey?: string,
  identity?: WorkspaceItemIdentity,
) => void;

export interface ForgeEvent {
  type: "pr-selected" | "issue-selected" | "pr-state-changed" | "sync-completed" | "detail-loaded";
  owner?: string;
  name?: string;
  number?: number;
  meta?: Record<string, unknown>;
}

export type EventCallback = (event: ForgeEvent) => void;

export type PrepareRouteCallback = (
  repo: { host?: string; owner: string; name: string },
  target?: { kind: "pull" | "issue"; number: number },
) => void | Promise<void>;

export interface HostStateAccessors {
  getGlobalRepo?: () => string | undefined;
  getGroupByRepo?: () => boolean;
  getActiveWorktreeKey?: () => string | undefined;
}

export interface UIConfig {
  hideStar?: boolean;
  hideSettings?: boolean;
  basePath?: string;
}

// Store types — re-exported from factory modules.
export type { PullsStore } from "./stores/pulls.svelte.js";
export type { IssuesStore } from "./stores/issues.svelte.js";
export type { DetailStore } from "./stores/detail.svelte.js";
export type { ActivityStore } from "./stores/activity.svelte.js";
export type { SyncStore } from "./stores/sync.svelte.js";
export type { DiffStore } from "./stores/diff.svelte.js";
export type { RepoBrowserStore } from "./stores/repo-browser.svelte.js";
export type { DiffReviewDraftStore } from "./stores/diff-review-draft.svelte.js";
export type { GroupingStore } from "./stores/grouping.svelte.js";
export type { DetailActivityViewStore } from "./stores/detail-activity-view.svelte.js";
export type { CollapsedReposStore } from "./stores/collapsedRepos.svelte.js";
export type { SettingsStore } from "./stores/settings.svelte.js";
export type { EventsStore } from "./stores/events.svelte.js";
export type { WorkflowActionsStore } from "./stores/workflow-actions.svelte.js";
export type { DaemonStore } from "./stores/roborev/daemon.svelte.js";
export type { JobsStore } from "./stores/roborev/jobs.svelte.js";
export type { ReviewStore } from "./stores/roborev/review.svelte.js";
export type { LogStore } from "./stores/roborev/log.svelte.js";

import type { PullsStore } from "./stores/pulls.svelte.js";
import type { IssuesStore } from "./stores/issues.svelte.js";
import type { DetailStore } from "./stores/detail.svelte.js";
import type { ActivityStore } from "./stores/activity.svelte.js";
import type { SyncStore } from "./stores/sync.svelte.js";
import type { DiffStore } from "./stores/diff.svelte.js";
import type { RepoBrowserStore } from "./stores/repo-browser.svelte.js";
import type { DiffReviewDraftStore } from "./stores/diff-review-draft.svelte.js";
import type { GroupingStore } from "./stores/grouping.svelte.js";
import type { DetailActivityViewStore } from "./stores/detail-activity-view.svelte.js";
import type { CollapsedReposStore } from "./stores/collapsedRepos.svelte.js";
import type { SettingsStore } from "./stores/settings.svelte.js";
import type { EventsStore } from "./stores/events.svelte.js";
import type { WorkflowActionsStore } from "./stores/workflow-actions.svelte.js";
import type { DaemonStore } from "./stores/roborev/daemon.svelte.js";
import type { JobsStore } from "./stores/roborev/jobs.svelte.js";
import type { ReviewStore } from "./stores/roborev/review.svelte.js";
import type { LogStore } from "./stores/roborev/log.svelte.js";

export interface StoreInstances {
  pulls: PullsStore;
  issues: IssuesStore;
  detail: DetailStore;
  activity: ActivityStore;
  sync: SyncStore;
  diff: DiffStore;
  repoBrowser: RepoBrowserStore;
  diffReviewDraft: DiffReviewDraftStore;
  grouping: GroupingStore;
  detailActivityView: DetailActivityViewStore;
  collapsedRepos: CollapsedReposStore;
  settings: SettingsStore;
  events: EventsStore;
  workflowActions: WorkflowActionsStore;
  roborevDaemon?: DaemonStore;
  roborevJobs?: JobsStore;
  roborevReview?: ReviewStore;
  roborevLog?: LogStore;
}

export interface SidebarAccessors {
  isEmbedded: () => boolean;
  isSidebarToggleEnabled: () => boolean;
  toggleSidebar: () => void;
}
