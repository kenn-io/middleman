import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import type { EventsStoreOptions } from "./lib/stores/events.svelte.js";
import type { DetailStoreOptions } from "./lib/stores/detail.svelte.js";
import type { IssuesStoreOptions } from "./lib/stores/issues.svelte.js";
import { DEFAULT_TERMINAL_SETTINGS, type ConfigRepo, type Settings, type SyncStatus } from "./lib/api/types.js";
import type { AppServices, OwnedAppRuntime } from "./lib/app/runtime.js";
import { createAppStores, type AppStoreOptions } from "./lib/app-stores.svelte.js";
import { makeTestAppRuntime } from "./lib/testing/effect-layers.js";
import { makeGeneratedClient } from "./lib/testing/generated-client.js";
import { makeStartupSnapshot } from "./test/startupSnapshot.js";

type LaunchTargets = NonNullable<Settings["launch_targets"]>;

interface CapturedEventsStore {
  options: EventsStoreOptions;
  connect: ReturnType<typeof vi.fn>;
  disconnect: ReturnType<typeof vi.fn>;
  isConnected: ReturnType<typeof vi.fn>;
}

interface CapturedSettingsStore {
  getLaunchTargets: () => LaunchTargets;
  setLaunchTargets: (targets: LaunchTargets) => void;
}

const captured: {
  store: CapturedEventsStore | null;
  settings: CapturedSettingsStore | null;
  detailOptions: DetailStoreOptions | null;
  issuesOptions: IssuesStoreOptions | null;
} = {
  store: null,
  settings: null,
  detailOptions: null,
  issuesOptions: null,
};

const { notifyWorkspaceDeleted } = vi.hoisted(() => ({
  notifyWorkspaceDeleted: vi.fn(),
}));

vi.mock("./lib/stores/workspace-host.svelte.js", () => ({ notifyWorkspaceDeleted }));

async function acceptEvent(result: Effect.Effect<void, unknown, AppServices> | void): Promise<void> {
  if (result === undefined) return;
  const execution = runtime.runCommand(result, {
    operation: "accept test provider event",
    safeContext: {},
    onFailure: () => undefined,
  });
  await Effect.runPromise(execution.await.pipe(Effect.flatMap((exit) => exit)));
}

vi.mock("./lib/stores/events.svelte.js", () => ({
  createEventsStore: (opts: EventsStoreOptions) => {
    const store: CapturedEventsStore = {
      options: opts,
      connect: vi.fn(),
      disconnect: vi.fn(),
      isConnected: vi.fn(() => false),
    };
    captured.store = store;
    return store;
  },
}));

const loadPulls = vi.fn(async () => undefined);
const loadPullsEffect = vi.fn(() => Effect.promise(() => loadPulls()));
const reconcilePullsEffect = vi.fn(() => Effect.promise(() => loadPulls()));
const loadIssues = vi.fn(async () => undefined);
const loadIssuesEffect = vi.fn(() => Effect.promise(() => loadIssues()));
const reconcileIssuesEffect = vi.fn(() => Effect.promise(() => loadIssues()));
const loadActivity = vi.fn(async () => undefined);
const loadActivityEffect = vi.fn(() => Effect.promise(() => loadActivity()));
const reconcileActivityEffect = vi.fn(() => Effect.promise(() => loadActivity()));
const setSyncStatus = vi.fn();
const setProviderAvailable = vi.fn();
const refreshDetailOnly = vi.fn(async () => undefined);
const refreshDetailOnlyEffect = vi.fn((...args: Parameters<typeof refreshDetailOnly>) =>
  Effect.promise(() => refreshDetailOnly(...args)),
);
let currentDetail: unknown = null;
let configuredRepos: ConfigRepo[] = [];

vi.mock("./lib/stores/pulls.svelte.js", () => ({
  createPullsStore: () => ({
    loadPulls,
    loadPullsEffect,
    reconcilePullsEffect,
    optimisticKanbanUpdate: vi.fn(),
    getPullKanbanStatus: vi.fn(),
    getPulls: () => [],
    isLoading: () => false,
  }),
}));

vi.mock("./lib/stores/issues.svelte.js", () => ({
  createIssuesStore: (options: IssuesStoreOptions) => {
    captured.issuesOptions = options;
    return {
      loadIssues,
      loadIssuesEffect,
      reconcileIssuesEffect,
      hydrateDefaults: vi.fn(),
      getIssues: () => [],
      isLoading: () => false,
    };
  },
}));

vi.mock("./lib/stores/activity.svelte.js", () => ({
  createActivityStore: () => ({
    loadActivity,
    loadActivityEffect,
    reconcileActivityEffect,
    hydrateDefaults: vi.fn(),
    getActivity: () => [],
    isLoading: () => false,
  }),
}));

vi.mock("./lib/stores/sync.svelte.js", () => ({
  createSyncStore: () => ({
    getSyncState: () => null,
    getProviderAvailable: () => true,
    setProviderAvailable,
    onNextSyncComplete: vi.fn(),
    subscribeSyncComplete: vi.fn(() => () => undefined),
    refreshSyncStatus: vi.fn(async () => undefined),
    refreshSyncStatusEffect: Effect.void,
    reconcileSyncStatusEffect: Effect.void,
    setSyncStatus,
    triggerSync: vi.fn(async () => undefined),
    startPolling: vi.fn(),
    stopPolling: vi.fn(),
  }),
}));

vi.mock("./lib/stores/detail.svelte.js", () => ({
  createDetailStore: (options: DetailStoreOptions) => {
    captured.detailOptions = options;
    return {
      loadDetail: vi.fn(),
      refreshDetailOnly,
      refreshDetailOnlyEffect,
      isDetailLoading: () => false,
      getDetail: () => currentDetail,
    };
  },
}));

vi.mock("./lib/stores/diff.svelte.js", () => ({
  createDiffStore: () => ({
    loadDiff: vi.fn(),
    getDiff: () => null,
  }),
}));

vi.mock("./lib/stores/grouping.svelte.js", () => ({
  createGroupingStore: () => ({
    getGroupByRepo: () => false,
    setGroupByRepo: vi.fn(),
  }),
}));

vi.mock("./lib/stores/settings.svelte.js", () => ({
  createSettingsStore: () => {
    let launchTargets: LaunchTargets = [];
    const store = {
      getConfiguredRepos: () => configuredRepos,
      setConfiguredRepos: vi.fn(),
      setRepoPresets: vi.fn(),
      getPullRequestSettings: () => ({
        allow_mid_stack_merges: false,
        prefer_github_native_stacks: false,
      }),
      setPullRequestSettings: vi.fn(),
      getDetailSettings: () => ({ initial_timeline_entry_limit: 50 }),
      setDetailSettings: vi.fn(),
      getWorkspaceSettings: () => ({ auto_assign_on_create: false, default_sidebar_view: "diff" as const }),
      setWorkspaceSettings: vi.fn(),
      getRoborevSettings: () => ({ init_managed_clones: false }),
      setRoborevSettings: vi.fn(),
      getModeVisibility: () => ({
        activity: true,
        repos: true,
        docs: false,
        pulls: true,
        issues: true,
        reviews: true,
        workspaces: true,
      }),
      setModeVisibility: vi.fn(),
      isModeVisible: vi.fn(() => true),
      getTerminalSettings: () => ({
        font_family: "",
        font_size: 12,
        scrollback: 1000,
        line_height: 1,
        letter_spacing: 0,
        cursor_blink: false,
        font_ligatures: false,
        hide_tmux_status: false,
        graphics: true,
        tmux_mouse: true,
      }),
      setTerminalSettings: vi.fn(),
      getTerminalFontFamily: () => "",
      setTerminalFontFamily: vi.fn(),
      getLaunchTargets: () => launchTargets,
      setLaunchTargets: vi.fn((targets: LaunchTargets) => {
        launchTargets = [...targets];
      }),
      hasConfiguredRepos: () => false,
      isSettingsLoaded: () => true,
    };
    captured.settings = store;
    return store;
  },
}));

const getSettings = vi.fn();

let runtime: OwnedAppRuntime;

beforeEach(() => {
  runtime = makeTestAppRuntime(makeGeneratedClient({ SettingsService: { getSettings: getSettings as never } }));
  captured.store = null;
  captured.settings = null;
  captured.detailOptions = null;
  captured.issuesOptions = null;
  getSettings.mockReset();
  loadPulls.mockClear();
  loadPullsEffect.mockClear();
  reconcilePullsEffect.mockClear();
  loadIssues.mockClear();
  loadIssuesEffect.mockClear();
  reconcileIssuesEffect.mockClear();
  loadActivity.mockClear();
  loadActivityEffect.mockClear();
  reconcileActivityEffect.mockClear();
  setSyncStatus.mockClear();
  setProviderAvailable.mockClear();
  refreshDetailOnly.mockClear();
  refreshDetailOnlyEffect.mockClear();
  notifyWorkspaceDeleted.mockClear();
  currentDetail = null;
  configuredRepos = [];
});

afterEach(async () => {
  vi.restoreAllMocks();
  await Effect.runPromise(runtime.disposeEffect);
});

function compose(options: Omit<AppStoreOptions, "runtime"> = {}) {
  return createAppStores({ runtime, ...options });
}

describe("issue PR-reference capability", () => {
  it("stays available when a capable provider-host is configured outside the selected repository scope", () => {
    configuredRepos = [
      {
        hidden_from_ui: false,
        is_glob: false,
        issue_pr_references: true,
        matched_repo_count: 1,
        name: "widgets",
        owner: "acme",
        platform_host: "github.example.com",
        provider: "github",
        repo_path: "acme/widgets",
      },
    ];

    compose({ getGlobalRepo: () => "gitlab|gitlab.example.com/acme/backend" });

    expect(captured.issuesOptions?.supportsIssuePRReferences?.()).toBe(true);
  });

  it("is unavailable only when no configured provider-host supplies reference edges", () => {
    compose({ getGlobalRepo: () => "gitlab|gitlab.example.com/acme/backend" });

    expect(captured.issuesOptions?.supportsIssuePRReferences?.()).toBe(false);
  });
});

describe("app store event wiring", () => {
  it("acknowledges a data change only after its visible refresh succeeds", async () => {
    const refresh = Promise.withResolvers<void>();
    loadPulls.mockImplementationOnce(() => refresh.promise);
    compose({ getPage: () => "pulls" });

    const event = captured.store?.options.onDataChanged?.();
    expect(event).toBeDefined();
    let acknowledged = false;
    const completion = Effect.runPromise(event ?? Effect.void).then(() => {
      acknowledged = true;
    });
    await vi.waitFor(() => expect(loadPulls).toHaveBeenCalledOnce());
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(acknowledged).toBe(false);
    refresh.resolve();
    await completion;
    expect(acknowledged).toBe(true);
  });

  it("replaces stale launch targets and refreshes only the visible phone list after a valid config reload", async () => {
    const codexTarget = {
      key: "codex",
      label: "Codex",
      kind: "agent",
      source: "builtin",
      command: ["codex"],
      available: true,
      disabled_reason: "",
    } satisfies LaunchTargets[number];
    const staleTarget = { ...codexTarget, key: "claude", label: "Claude", command: ["claude"] };
    const settings = makeStartupSnapshot({
      terminal: { ...DEFAULT_TERMINAL_SETTINGS, cursor_blink: false },
      launch_targets: [codexTarget],
    });
    getSettings.mockResolvedValue(settings);

    compose({ getPage: () => "mobile-activity" });
    captured.settings?.setLaunchTargets([staleTarget]);

    await acceptEvent(captured.store?.options.onConfigChanged?.({ valid: true, restart_required: false }));

    await vi.waitFor(() => {
      expect(captured.settings?.getLaunchTargets()).toEqual([codexTarget]);
    });
    expect(reconcilePullsEffect).not.toHaveBeenCalled();
    expect(reconcileIssuesEffect).not.toHaveBeenCalled();
    expect(reconcileActivityEffect).toHaveBeenCalledTimes(1);
  });

  it.each([
    { route: "pulls", pulls: 1, issues: 0, activity: 0 },
    { route: "mobile-pulls", pulls: 1, issues: 0, activity: 0 },
    { route: "issues", pulls: 0, issues: 1, activity: 0 },
    { route: "mobile-issues", pulls: 0, issues: 1, activity: 0 },
    { route: "activity", pulls: 0, issues: 0, activity: 1 },
    { route: "mobile-activity", pulls: 0, issues: 0, activity: 1 },
    { route: "focus", pulls: 1, issues: 1, activity: 0 },
    { route: "terminal", pulls: 0, issues: 0, activity: 0 },
    { route: "workspaces", pulls: 0, issues: 0, activity: 0 },
  ])("refreshes only the stores visible on the $route route", async ({ route, pulls, issues, activity }) => {
    compose({ getPage: () => route });

    expect(captured.store).not.toBeNull();
    const cb = captured.store?.options.onDataChanged;
    expect(cb).toBeTypeOf("function");

    await acceptEvent(cb?.());

    expect(loadPulls).toHaveBeenCalledTimes(pulls);
    expect(loadIssues).toHaveBeenCalledTimes(issues);
    expect(loadActivity).toHaveBeenCalledTimes(activity);
    expect(reconcilePullsEffect).toHaveBeenCalledTimes(pulls);
    expect(reconcileIssuesEffect).toHaveBeenCalledTimes(issues);
    expect(reconcileActivityEffect).toHaveBeenCalledTimes(activity);
    expect(loadPullsEffect).not.toHaveBeenCalled();
    expect(loadIssuesEffect).not.toHaveBeenCalled();
    expect(loadActivityEffect).not.toHaveBeenCalled();
  });

  it.each(["activity", "mobile-activity"])(
    "reconciles the Activity list when a background detail sync converges on %s",
    async (route) => {
      compose({ getPage: () => route });

      captured.detailOptions?.onDetailSynchronized?.();
      captured.issuesOptions?.onDetailSynchronized?.();

      await vi.waitFor(() => expect(reconcileActivityEffect).toHaveBeenCalledTimes(2));
      expect(loadActivity).toHaveBeenCalledTimes(2);
    },
  );

  it("refreshes the Activity drawer selection instead of stale displayed detail", async () => {
    currentDetail = {
      repo: {
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/old-widget",
      },
      repo_owner: "acme",
      repo_name: "old-widget",
      merge_request: { Number: 41 },
    };
    compose({
      getPage: () => "activity",
      getActivitySelection: () => ({
        itemType: "pr",
        provider: "gitlab",
        platformHost: "gitlab.example.com",
        repoPath: "group/widget",
        owner: "group",
        name: "widget",
        number: 42,
      }),
    });

    await acceptEvent(captured.store?.options.onDataChanged?.());

    expect(loadActivity).toHaveBeenCalledTimes(1);
    expect(refreshDetailOnly).toHaveBeenCalledTimes(1);
    expect(refreshDetailOnly).toHaveBeenCalledWith("group", "widget", 42, {
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      repoPath: "group/widget",
    });
  });

  it("refreshes the selected Activity PR after a stale reconnect", async () => {
    compose({
      getPage: () => "activity",
      getActivitySelection: () => ({
        itemType: "pr",
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 42,
      }),
    });

    await acceptEvent(captured.store?.options.onReconnectStale?.({}));

    expect(loadPulls).toHaveBeenCalledTimes(1);
    expect(loadIssues).toHaveBeenCalledTimes(1);
    expect(loadActivity).toHaveBeenCalledTimes(1);
    expect(refreshDetailOnly).toHaveBeenCalledTimes(1);
    expect(refreshDetailOnly).toHaveBeenCalledWith("acme", "widget", 42, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    expect(setProviderAvailable).toHaveBeenNthCalledWith(1, false);
    expect(setProviderAvailable).toHaveBeenLastCalledWith(true);
  });

  it("keeps provider projections unavailable after a stale reconnect during hub outage", async () => {
    compose({ getPage: () => "pulls" });

    await acceptEvent(captured.store?.options.onReconnectStale?.({ hub_connected: false }));

    expect(setProviderAvailable).toHaveBeenCalledOnce();
    expect(setProviderAvailable).toHaveBeenCalledWith(false);
    expect(loadPulls).not.toHaveBeenCalled();
    expect(loadIssues).not.toHaveBeenCalled();
    expect(loadActivity).not.toHaveBeenCalled();
  });

  it("passes onSyncStatus that pushes the received status into sync store", async () => {
    compose();

    const cb = captured.store?.options.onSyncStatus;
    expect(cb).toBeTypeOf("function");

    const status: SyncStatus = {
      running: true,
      last_run_at: "2026-04-08T12:00:00Z",
      last_error: "",
    };
    await acceptEvent(cb?.(status));

    expect(setSyncStatus).toHaveBeenCalledTimes(1);
    expect(setSyncStatus).toHaveBeenCalledWith(status);
  });

  it("restores provider availability after refreshing the selected projection", async () => {
    const refresh = Promise.withResolvers<void>();
    loadPulls.mockImplementationOnce(() => refresh.promise);
    compose({ getPage: () => "pulls" });
    const callback = captured.store?.options.onHubConnectionChanged;
    expect(callback).toBeTypeOf("function");

    await acceptEvent(callback?.({ connected: false }));
    expect(setProviderAvailable).toHaveBeenLastCalledWith(false);

    const effect = callback?.({ connected: true });
    expect(effect).toBeDefined();
    const execution = runtime.runCommand(effect ?? Effect.void, {
      operation: "test hub recovery",
      safeContext: {},
      onFailure: () => undefined,
    });
    await vi.waitFor(() => expect(loadPulls).toHaveBeenCalledOnce());
    expect(setProviderAvailable).toHaveBeenLastCalledWith(false);

    refresh.resolve();
    await Effect.runPromise(execution.await.pipe(Effect.flatMap((exit) => exit)));
    expect(loadIssues).toHaveBeenCalledOnce();
    expect(loadActivity).toHaveBeenCalledOnce();
    expect(setProviderAvailable).toHaveBeenLastCalledWith(true);
  });

  it("refreshes only the visible PR detail for matching targeted refresh events", async () => {
    currentDetail = {
      repo: {
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
      },
      repo_owner: "acme",
      repo_name: "widget",
      merge_request: { Number: 42 },
    };
    compose();

    await acceptEvent(
      captured.store?.options.onPRDetailRefreshed?.({
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 42,
        head_sha: "2222222",
        synced_at: "2026-05-20T14:15:04Z",
        warnings: [],
      }),
    );

    expect(refreshDetailOnly).toHaveBeenCalledTimes(1);
    expect(refreshDetailOnly).toHaveBeenCalledWith("acme", "widget", 42, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
  });

  it("ignores targeted PR refreshes while an issue detail is visible", async () => {
    currentDetail = {
      repo: {
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
      },
      repo_owner: "acme",
      repo_name: "widget",
      issue: { Number: 7 },
    };
    compose();

    await expect(
      acceptEvent(
        captured.store?.options.onPRDetailRefreshed?.({
          provider: "github",
          platform_host: "github.com",
          repo_path: "acme/widget",
          owner: "acme",
          name: "widget",
          number: 42,
          head_sha: "2222222",
          synced_at: "2026-05-20T14:15:04Z",
          warnings: [],
        }),
      ),
    ).resolves.toBeUndefined();
    await expect(
      acceptEvent(
        captured.store?.options.onPRCIRefreshed?.({
          provider: "github",
          platform_host: "github.com",
          repo_path: "acme/widget",
          owner: "acme",
          name: "widget",
          number: 42,
          head_sha: "2222222",
          refreshed_at: "2026-05-20T14:15:20Z",
          warnings: [],
        }),
      ),
    ).resolves.toBeUndefined();
    expect(refreshDetailOnly).not.toHaveBeenCalled();
  });

  it("ignores targeted PR detail refreshes for non-visible PRs", async () => {
    currentDetail = {
      repo: {
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
      },
      repo_owner: "acme",
      repo_name: "widget",
      merge_request: { Number: 42 },
    };
    compose();

    await acceptEvent(
      captured.store?.options.onPRDetailRefreshed?.({
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 99,
        head_sha: "2222222",
        synced_at: "2026-05-20T14:15:04Z",
        warnings: [],
      }),
    );

    expect(refreshDetailOnly).not.toHaveBeenCalled();
  });

  it("forwards basePath getter when config.basePath is set", () => {
    compose({ config: { basePath: "/prefix" } });

    const getBasePath = captured.store?.options.getBasePath;
    expect(getBasePath).toBeTypeOf("function");
    expect(getBasePath?.()).toBe("/prefix");
  });

  it("omits getBasePath when config has no basePath", () => {
    compose();
    expect(captured.store?.options.getBasePath).toBeUndefined();
  });

  it("routes deferred merge failures only through the error callback", async () => {
    const onError = vi.fn();
    const onNotification = vi.fn();
    compose({ onError, onNotification });

    await acceptEvent(
      captured.store?.options.onDeferredMergeCompleted?.({
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 42,
        head_sha: "2222222",
        status: "failed",
        error: "checks did not pass",
        completed_at: "2026-07-10T15:00:00Z",
      }),
    );

    expect(onError).toHaveBeenCalledWith("Deferred merge for acme/widget#42 failed: checks did not pass");
    expect(onNotification).not.toHaveBeenCalled();
  });

  it("routes merged workspace cleanup failures through the warning callback", async () => {
    const onWarning = vi.fn();
    const onNotification = vi.fn();
    compose({ onWarning, onNotification });

    await acceptEvent(
      captured.store?.options.onDeferredMergeCompleted?.({
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 42,
        head_sha: "2222222",
        status: "merged",
        merged: true,
        completed_at: "2026-07-10T15:00:00Z",
        workspace_cleanup_warning: "workspace has uncommitted changes",
      }),
    );

    expect(onWarning).toHaveBeenCalledWith(
      "acme/widget#42 merged, but the workspace was not pruned: workspace has uncommitted changes",
    );
    expect(onNotification).not.toHaveBeenCalled();
  });

  it("publishes confirmed workspace deletion with its item identity", async () => {
    compose({ getPage: () => "pulls" });

    await acceptEvent(
      captured.store?.options.onWorkspaceDeleted?.({
        workspace_id: "ws-1",
        provider: "github",
        platform_host: "github.com",
        repo_path: "acme/widget",
        owner: "acme",
        name: "widget",
        number: 42,
        item_type: "pull_request",
      }),
    );

    expect(notifyWorkspaceDeleted).toHaveBeenCalledWith("ws-1", undefined, {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
      number: 42,
      itemType: "pull_request",
    });
    expect(reconcilePullsEffect).toHaveBeenCalledOnce();
    expect(loadPulls).toHaveBeenCalledOnce();
  });
});
