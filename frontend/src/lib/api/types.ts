import type {
  Activity,
  ActivityAuthorsResponse as GeneratedActivityAuthorsResponse,
  ActivityItemResponse,
  ActivityResponse as GeneratedActivityResponse,
  ActivitySubjectResponse,
  Agent,
  ApprovePRInputBody as GeneratedApprovePRInputBody,
  CICheck as GeneratedCICheck,
  CommentAutocompleteReference as GeneratedCommentAutocompleteReference,
  CommentAutocompleteResponse as GeneratedCommentAutocompleteResponse,
  ConfiguredRepoStatus,
  Detail,
  DiffFile as GeneratedDiffFile,
  DiffResponse,
  EditPRContentInputBody as GeneratedEditPRContentInputBody,
  FilePreviewResponse,
  FilesResponse,
  FleetSettingsResponse,
  GithubStateInputBody as GeneratedGithubStateInputBody,
  Hunk,
  IssueDetailResponse,
  IssueEvent as GeneratedIssueEvent,
  IssueResponse,
  Issues,
  ItemLabelsResponse as GeneratedItemLabelsResponse,
  KataProjectRepoMapping as GeneratedKataProjectRepoMapping,
  Label as GeneratedLabel,
  LaunchTarget as GeneratedLaunchTarget,
  Line,
  LocalSyncCeilingStatus as GeneratedLocalSyncCeilingStatus,
  McpSettingsResponse,
  McpSettingsUpdate,
  MergePRInputBody,
  MergeRequestDetailResponse,
  MergeRequestEventResponse,
  MergeRequestResponse,
  ModeVisibility as GeneratedModeVisibility,
  NotificationBulkResponse as GeneratedNotificationBulkResponse,
  NotificationsResponse as GeneratedNotificationsResponse,
  OperationAvailability as GeneratedOperationAvailability,
  ProviderCapabilitiesResponse,
  PullRequests,
  RateLimitHostStatus as GeneratedRateLimitHostStatus,
  RateLimitResourceStatus as GeneratedRateLimitResourceStatus,
  RateLimitsResponse as GeneratedRateLimitsResponse,
  RepoBrowserBlob as GeneratedRepoBrowserBlob,
  RepoBrowserCommit as GeneratedRepoBrowserCommit,
  RepoBrowserRef as GeneratedRepoBrowserRef,
  RepoBrowserRefsResponse as GeneratedRepoBrowserRefsResponse,
  RepoBrowserTreeEntry as GeneratedRepoBrowserTreeEntry,
  RepoLabelsResponse as GeneratedRepoLabelsResponse,
  RepoOperations as GeneratedRepoOperations,
  RepoPreset as GeneratedRepoPreset,
  RepoResponse,
  RepoSummaryAuthorResponse,
  RepoSummaryCommitPointResponse as GeneratedRepoSummaryCommitPointResponse,
  RepoSummaryIssueResponse,
  RepoSummaryReleaseResponse as GeneratedRepoSummaryReleaseResponse,
  RepoSummaryResponse,
  RequestChangesPRInputBody as GeneratedRequestChangesPRInputBody,
  SessionInfo,
  SettingsResponse as GeneratedSettingsResponse,
  StarredRequest as GeneratedStarredRequest,
  SyncStatus as GeneratedSyncStatus,
  Terminal,
  UpdateFleetSettingsInputBody,
  WorkspaceActivitySubjectResponse,
  WorkspaceKataMetadata as GeneratedWorkspaceKataMetadata,
  WorkspaceRuntimeResponse,
  ListActivityParams,
  ListActivityAuthorsParams,
  ListPullsParams,
  ListIssuesParams,
  UnsetStarredParams as GeneratedUnsetStarredParams,
} from "./generated/models/index.js";

export type Repo = RepoResponse;
export type RepoSummary = RepoSummaryResponse;
export type RepoSummaryAuthor = RepoSummaryAuthorResponse;
export type RepoSummaryIssue = RepoSummaryIssueResponse;
export type RepoSummaryCommitPointResponse = GeneratedRepoSummaryCommitPointResponse;
export type RepoSummaryReleaseResponse = GeneratedRepoSummaryReleaseResponse;
export type PullRequest = MergeRequestResponse;
export type ProviderCapabilities = ProviderCapabilitiesResponse;
export type OperationAvailability = GeneratedOperationAvailability;
export type RepoOperations = GeneratedRepoOperations;
export type Issue = IssueResponse;
export type IssueEvent = GeneratedIssueEvent;
export type IssueDetail = IssueDetailResponse;
export type PREvent = MergeRequestEventResponse;
export type PullDetail = MergeRequestDetailResponse;
export type SyncStatus = GeneratedSyncStatus;
export type RateLimitHostStatus = GeneratedRateLimitHostStatus;
export type RateLimitResourceStatus = GeneratedRateLimitResourceStatus;
export type LocalSyncCeilingStatus = GeneratedLocalSyncCeilingStatus;
export type RateLimitsResponse = GeneratedRateLimitsResponse;
export type ActivityItem = ActivityItemResponse;
export type ActivityResponse = GeneratedActivityResponse;
export type ActivitySubject = ActivitySubjectResponse;
export type WorkspaceActivitySubject = WorkspaceActivitySubjectResponse;
export type ActivityAuthorsResponse = GeneratedActivityAuthorsResponse;
export type NotificationsResponse = GeneratedNotificationsResponse;
export type NotificationBulkResponse = GeneratedNotificationBulkResponse;
export type CommentAutocompleteResponse = GeneratedCommentAutocompleteResponse;
export type CommentAutocompleteReference = GeneratedCommentAutocompleteReference;
export type RepoBrowserBlob = GeneratedRepoBrowserBlob;
export type RepoBrowserCommit = GeneratedRepoBrowserCommit;
export type RepoBrowserRef = GeneratedRepoBrowserRef;
export type RepoBrowserRefsResponse = GeneratedRepoBrowserRefsResponse;
export type RepoBrowserTreeEntry = GeneratedRepoBrowserTreeEntry;
export type ActivityParams = ListActivityParams;
export type ActivityAuthorsParams = ListActivityAuthorsParams;
export type PullsParams = ListPullsParams;
export type IssuesParams = ListIssuesParams;
export type ApprovePRInputBody = GeneratedApprovePRInputBody;
export type RequestChangesPRInputBody = GeneratedRequestChangesPRInputBody;
export type MergeParams = MergePRInputBody;
export type EditPRContentInputBody = GeneratedEditPRContentInputBody;
export type StarredRequest = GeneratedStarredRequest;
export type UnsetStarredParams = GeneratedUnsetStarredParams;
export type GithubStateInputBody = GeneratedGithubStateInputBody;

export type LaunchTarget = GeneratedLaunchTarget;
export type RuntimeSession = SessionInfo;
export type WorkspaceRuntime = WorkspaceRuntimeResponse;

export type Label = GeneratedLabel;
export type RepoLabelsResponse = GeneratedRepoLabelsResponse;
export type ItemLabelsResponse = GeneratedItemLabelsResponse;

export type KanbanStatus = PullRequest["KanbanStatus"];

export type CICheckWire = GeneratedCICheck;
export type CICheck = CICheckWire & { readonly required?: boolean };

export type ActivitySettings = Activity;
export type IssueSettings = Issues;
export type PullRequestSettings = PullRequests;
export type DetailSettings = Detail;
export type TerminalSettings = Terminal;
export type ModeVisibility = GeneratedModeVisibility;

export const DEFAULT_TERMINAL_SETTINGS: TerminalSettings = {
  font_family: "",
  font_size: 12,
  scrollback: 1000,
  line_height: 1,
  letter_spacing: 0,
  cursor_blink: true,
  font_ligatures: false,
  hide_tmux_status: false,
  graphics: true,
  tmux_mouse: true,
  retained_sessions: 10,
};

export const DEFAULT_MODE_VISIBILITY: ModeVisibility = {
  activity: true,
  repos: true,
  docs: false,
  actions: false,
  pulls: true,
  issues: true,
  reviews: true,
  workspaces: true,
};

export const DEFAULT_PULL_REQUEST_SETTINGS: PullRequestSettings = {
  allow_mid_stack_merges: false,
  prefer_github_native_stacks: false,
};

export const DEFAULT_DETAIL_SETTINGS: DetailSettings = {
  initial_timeline_entry_limit: 50,
  collapse_single_line_breaks: false,
  render_commit_messages_as_markdown: false,
};

export type AgentSettings = Agent;
export type ConfigRepo = ConfiguredRepoStatus;
export type RepoPreset = GeneratedRepoPreset;
export type KataProjectRepoMapping = GeneratedKataProjectRepoMapping;
export type WorkspaceKataMetadata = GeneratedWorkspaceKataMetadata;
type SettingsResponse = GeneratedSettingsResponse;
export type Settings = Omit<SettingsResponse, "notifications"> & {
  notifications?: SettingsResponse["notifications"];
};
export type FleetSettings = FleetSettingsResponse;
export type FleetSettingsUpdate = UpdateFleetSettingsInputBody;
export type MCPSettings = McpSettingsResponse;
export type MCPSettingsUpdate = McpSettingsUpdate;

export type FilePreview = FilePreviewResponse;
export type DiffResponseWire = DiffResponse;
export type FilesResponseWire = FilesResponse;
export type DiffFileWire = GeneratedDiffFile;
export type DiffHunkWire = Hunk;
export type DiffLineWire = Line;

export type DiffLine = Omit<DiffLineWire, "type"> & {
  type: "context" | "add" | "delete";
};

export type DiffHunk = Omit<DiffHunkWire, "lines"> & {
  lines: DiffLine[];
};

export type DiffFile = Omit<DiffFileWire, "status" | "hunks"> & {
  status: "added" | "modified" | "deleted" | "renamed" | "copied";
  hunks: DiffHunk[];
};

export type DiffResult = Omit<DiffResponseWire, "files"> & {
  files: DiffFile[];
};

export type FilesResult = Omit<FilesResponseWire, "files"> & {
  files: DiffFile[];
};

export interface CommitInfo {
  sha: string;
  message: string;
  author_name: string;
  authored_at: string;
  // True when the commit is reachable from the workspace branch's upstream
  // tracking ref; false means it is local-only. Absent when push status is
  // unknown, such as pull request commits.
  pushed?: boolean;
}

export interface WorkspaceHost {
  key: string;
  label: string;
  connectionState: "connected" | "connecting" | "disconnected" | "error";
  transport?: "http" | "local";
  platform?: string;
  projects: WorkspaceProject[];
  sessions: WorkspaceSession[];
  resources: WorkspaceResources | null;
}

export interface WorkspaceProject {
  key: string;
  name: string;
  kind: "repository" | "scratch";
  repoKind: string;
  defaultBranch: string;
  platformRepo: string | null;
  platformURL?: string;
  worktrees: WorkspaceWorktree[];
}

export interface WorkspaceWorktree {
  key: string;
  name: string;
  branch: string;
  isPrimary: boolean;
  isHidden: boolean;
  isStale: boolean;
  sessionBackend: string | null;
  linkedPR: WorkspaceLinkedPR | null;
  activity: WorkspaceActivity;
  diff: WorkspaceDiff | null;
}

export interface WorkspaceLinkedPR {
  number: number;
  title: string;
  state: "open" | "closed" | "merged";
  checksStatus: string | null;
  updatedAt: string | null;
}

export interface WorkspaceActivity {
  state: "idle" | "active" | "running" | "needsAttention";
  lastOutputAt: string | null;
}

export interface WorkspaceDiff {
  added: number;
  removed: number;
}

export interface WorkspaceSession {
  key: string;
  name: string;
  worktreeKey: string | null;
  isHidden: boolean;
}

export interface WorkspaceResources {
  cpuPercent: number;
  residentMB: number;
}
