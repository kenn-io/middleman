import { Effect, Schema } from "effect";
import type {
  HostSummary as GeneratedHostSummary,
  WorkspaceRepositorySummary,
  WorkspaceSummary,
} from "../../api/generated/models/index.js";
import { InvalidExternalPayload } from "../../api/effect-errors.js";

type GeneratedWorkspace = WorkspaceSummary;
type GeneratedRepo = WorkspaceRepositorySummary;
type HostSummary = GeneratedHostSummary;

export type WorkspaceListItem = Pick<
  GeneratedWorkspace,
  | "created_at"
  | "git_head_ref"
  | "id"
  | "item_number"
  | "item_type"
  | "platform_host"
  | "repo_name"
  | "repo_owner"
  | "source_item_visible"
  | "status"
  | "tmux_activity_source"
  | "tmux_last_output_at"
  | "tmux_working"
  | "worktree_path"
> &
  Partial<Pick<GeneratedWorkspace, "item_key" | "kata" | "worktree_dirty">> & {
    readonly agent_state?: GeneratedWorkspace["agent_state"] | null;
    readonly agent_state_updated_at?: string | null;
    readonly associated_pr_number?: Exclude<GeneratedWorkspace["associated_pr_number"], undefined> | null;
    readonly branch_upstream_missing?: boolean;
    readonly commits_ahead?: number | null;
    readonly commits_behind?: number | null;
    readonly error_message?: GeneratedWorkspace["error_message"] | null;
    readonly item_last_activity_at?: string | null;
    readonly mr_additions?: number | null;
    readonly mr_deletions?: number | null;
    readonly mr_is_draft?: boolean | null;
    readonly mr_state?: string | null;
    readonly mr_title?: string | null;
    readonly repo?: Pick<
      GeneratedRepo,
      "name" | "owner" | "platform_host" | "platform_repo_id" | "provider" | "repo_path"
    >;
    readonly tmux_pane_title?: string | null;
    readonly fleet_host_key?: string;
    readonly fleet_host_name?: string;
    readonly visible?: boolean;
  };

const Repo = Schema.Struct({
  name: Schema.String,
  owner: Schema.String,
  platform_host: Schema.String,
  platform_repo_id: Schema.optionalKey(Schema.String),
  provider: Schema.String,
  repo_path: Schema.String,
});

const Kata = Schema.Struct({
  daemon_id: Schema.String,
  issue_uid: Schema.String,
  project_name: Schema.optionalKey(Schema.String),
  project_uid: Schema.String,
  qualified_id: Schema.optionalKey(Schema.String),
  short_id: Schema.optionalKey(Schema.String),
  title: Schema.optionalKey(Schema.String),
});

const Workspace = Schema.Struct({
  agent_state: Schema.optionalKey(Schema.NullOr(Schema.Literals(["idle", "working", "input", "approval", "done"]))),
  agent_state_updated_at: Schema.optionalKey(Schema.NullOr(Schema.String)),
  associated_pr_number: Schema.optionalKey(Schema.NullOr(Schema.Number)),
  branch_upstream_missing: Schema.optionalKey(Schema.Boolean),
  commits_ahead: Schema.optionalKey(Schema.NullOr(Schema.Number)),
  commits_behind: Schema.optionalKey(Schema.NullOr(Schema.Number)),
  created_at: Schema.String,
  error_message: Schema.optionalKey(Schema.NullOr(Schema.String)),
  fleet_host_key: Schema.optionalKey(Schema.String),
  fleet_host_name: Schema.optionalKey(Schema.String),
  git_head_ref: Schema.String,
  id: Schema.String,
  item_key: Schema.optionalKey(Schema.String),
  item_last_activity_at: Schema.optionalKey(Schema.NullOr(Schema.String)),
  item_number: Schema.Number,
  item_type: Schema.String,
  kata: Schema.optionalKey(Kata),
  mr_additions: Schema.optionalKey(Schema.NullOr(Schema.Number)),
  mr_deletions: Schema.optionalKey(Schema.NullOr(Schema.Number)),
  mr_is_draft: Schema.optionalKey(Schema.NullOr(Schema.Boolean)),
  mr_state: Schema.optionalKey(Schema.NullOr(Schema.String)),
  mr_title: Schema.optionalKey(Schema.NullOr(Schema.String)),
  platform_host: Schema.String,
  repo: Schema.optionalKey(Repo),
  repo_name: Schema.String,
  repo_owner: Schema.String,
  source_item_visible: Schema.Boolean,
  status: Schema.String,
  tmux_activity_source: Schema.String,
  tmux_last_output_at: Schema.NullOr(Schema.String),
  tmux_pane_title: Schema.optionalKey(Schema.NullOr(Schema.String)),
  tmux_working: Schema.Boolean,
  visible: Schema.optionalKey(Schema.Boolean),
  worktree_path: Schema.String,
  worktree_dirty: Schema.optionalKey(Schema.Boolean),
});

const WorkspaceList = Schema.Struct({
  workspaces: Schema.NullOr(Schema.Array(Workspace)),
});

export const decodeWorkspaceList = Effect.fn("WorkspaceList.decode")(function* (input: unknown) {
  const decoded = yield* Schema.decodeUnknownEffect(WorkspaceList)(input).pipe(
    Effect.mapError((cause) =>
      InvalidExternalPayload.make({
        operation: "decode fleet workspace list",
        cause,
      }),
    ),
  );
  const workspaces: readonly WorkspaceListItem[] = decoded.workspaces ?? [];
  return workspaces;
});

export function retainDegradedHostWorkspaces(
  previous: readonly WorkspaceListItem[],
  current: readonly WorkspaceListItem[],
  hosts: readonly HostSummary[],
  aggregateIncomplete: boolean,
): WorkspaceListItem[] {
  const degradedHosts = new Set(
    hosts.filter((host) => host.kind !== "self" && (host.error || !host.reachable)).map((host) => host.configKey),
  );
  if (degradedHosts.size === 0) return [...current];

  const currentHosts = new Set(hosts.map((host) => host.configKey));
  const currentKeys = new Set(current.map(workspaceProjectionKey));
  const retained = previous.filter(
    (workspace) =>
      workspace.fleet_host_key !== undefined &&
      (degradedHosts.has(workspace.fleet_host_key) ||
        (aggregateIncomplete && !currentHosts.has(workspace.fleet_host_key))) &&
      !currentKeys.has(workspaceProjectionKey(workspace)),
  );
  return [...current, ...retained];
}

function workspaceProjectionKey(workspace: WorkspaceListItem): string {
  return `${workspace.fleet_host_key ?? "self"}\u0000${workspace.id}`;
}
