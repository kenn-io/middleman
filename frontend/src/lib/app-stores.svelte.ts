import { getShowListAgentStatus } from "./stores/list-agent-status.svelte.js";
import { Effect } from "effect";
import { pollWhileVisible } from "./effect/poll-while-visible.js";
import { ApiProblemError } from "./api/effect-errors.js";
import { createRoborevClient } from "./api/roborev/client.js";
import type { RoborevClient } from "./api/roborev/client.js";
import { executeGeneratedApiRequest } from "./api/generated-api.js";
import { ProblemCodes } from "./api/problems.js";
import type { ProviderRouteRef } from "./api/provider-routes.js";
import { retryIdempotentRead } from "./api/retry-policy.js";
import { createDaemonStore } from "./stores/roborev/daemon.svelte.js";
import { createJobsStore } from "./stores/roborev/jobs.svelte.js";
import { createReviewStore } from "./stores/roborev/review.svelte.js";
import { createLogStore } from "./stores/roborev/log.svelte.js";
import { makeRoborevOwner } from "./stores/roborev/roborev-workflow.js";
import type { NavigateCallback, HostStateAccessors, StoreInstances, UIConfig } from "./types.js";
import type { AppRuntime, AppServices } from "./app/runtime.js";
import type { ProviderEventsError } from "./stores/provider-events-workflow.js";
import type { PullsStoreOptions } from "./stores/pulls.svelte.js";
import type { IssuesStoreOptions } from "./stores/issues.svelte.js";
import type { DetailStoreOptions } from "./stores/detail.svelte.js";
import type { ActivityStoreOptions } from "./stores/activity.svelte.js";
import type { DiffStoreOptions } from "./stores/diff.svelte.js";
import { createPullsStore } from "./stores/pulls.svelte.js";
import { createIssuesStore } from "./stores/issues.svelte.js";
import { createDetailStore } from "./stores/detail.svelte.js";
import { createActivityStore } from "./stores/activity.svelte.js";
import { createSyncStore } from "./stores/sync.svelte.js";
import { createDiffStore } from "./stores/diff.svelte.js";
import { createDiffReviewDraftStore } from "./stores/diff-review-draft.svelte.js";
import { createRepoBrowserStore } from "./stores/repo-browser.svelte.js";
import { createGroupingStore } from "./stores/grouping.svelte.js";
import { createDetailActivityViewStore } from "./stores/detail-activity-view.svelte.js";
import { createCollapsedReposStore } from "./stores/collapsedRepos.svelte.js";
import { createSettingsStore } from "./stores/settings.svelte.js";
import { beginTerminalSettingsHydration } from "./stores/terminal-settings-persistence.js";
import { beginWorkspaceSettingsHydration } from "./stores/workspace-settings-persistence.js";
import { beginRoborevSettingsHydration } from "./stores/roborev-settings-persistence.js";
import { applySettingsHydration } from "./stores/settings-hydration.js";
import { createEventsStore } from "./stores/events.svelte.js";
import type { RoutedItemRef } from "./routes.js";
import { notifyWorkspaceDeleted } from "./stores/workspace-host.svelte.js";

export interface AppStoreOptions {
  runtime: AppRuntime;
  onNavigate?: NavigateCallback;
  hostState?: HostStateAccessors;
  config?: UIConfig;
  getPage?: () => string;
  getActivitySelection?: () => RoutedItemRef | null;
  roborevBaseUrl?: string;
  onError?: (msg: string) => void;
  onWarning?: (msg: string) => void;
  onNotification?: (msg: string) => void;
}

export interface AppStoreComposition {
  readonly stores: StoreInstances;
  readonly listAgentStatusPolling: Effect.Effect<void, never, AppServices>;
  readonly roborevClient?: RoborevClient;
}

export function createAppStores(options: AppStoreOptions): AppStoreComposition {
  const {
    runtime,
    onNavigate = () => {},
    hostState = {},
    config = {},
    getPage = () => "",
    getActivitySelection = () => null,
    roborevBaseUrl,
    onError,
    onWarning,
    onNotification,
  } = options;
  const appRuntime = runtime;
  const hs = hostState;
  const cfg = config;
  const nav = onNavigate;
  const gp = getPage;
  const getSelectedActivity = getActivitySelection;
  const roborevBase = roborevBaseUrl;
  const errorCb = onError;
  const warningCb = onWarning;
  const notificationCb = onNotification;
  const grouping = createGroupingStore();
  const detailActivityView = createDetailActivityViewStore();
  const collapsedRepos = createCollapsedReposStore();
  const settingsStore = createSettingsStore();
  const detailStarProjection: {
    current?: (ref: ProviderRouteRef, number: number, starred: boolean, envelopeTick: number) => void;
  } = {};

  const pullsOpts: PullsStoreOptions = { runtime: appRuntime };
  if (hs.getGlobalRepo) {
    pullsOpts.getGlobalRepo = hs.getGlobalRepo;
  }
  pullsOpts.getGroupByRepo = hs.getGroupByRepo ?? grouping.getGroupByRepo;
  pullsOpts.optimisticDetailStarUpdate = (ref, number, starred, envelopeTick) => {
    detailStarProjection.current?.(ref, number, starred, envelopeTick);
  };
  const pullsStore = createPullsStore(pullsOpts);

  const syncStore = createSyncStore({
    runtime: appRuntime,
    getPriorityRepos: hs.getGlobalRepo,
  });

  const activityOpts: ActivityStoreOptions = { runtime: appRuntime };
  if (hs.getGlobalRepo) {
    activityOpts.getGlobalRepo = hs.getGlobalRepo;
  }
  if (cfg.basePath != null) {
    const bp = cfg.basePath;
    activityOpts.getBasePath = () => bp;
  }
  const activityStore = createActivityStore(activityOpts);

  function reconcileActivityAfterDetailSync(): void {
    if (gp() !== "activity" && gp() !== "mobile-activity") return;
    appRuntime.runCommand(activityStore.reconcileActivityEffect(), {
      operation: "reconcile activity after detail sync",
      safeContext: {},
      onFailure: () => {},
    });
  }

  const detailOpts: DetailStoreOptions = {
    runtime: appRuntime,
    getPage: gp,
    onDetailSynchronized: reconcileActivityAfterDetailSync,
    pulls: {
      loadPulls: pullsStore.loadPulls,
      optimisticKanbanUpdate: pullsStore.optimisticKanbanUpdate,
      getPullKanbanStatus: pullsStore.getPullKanbanStatus,
      optimisticStarUpdate: pullsStore.optimisticStarUpdate,
    },
    sync: syncStore,
  };
  const detailStore = createDetailStore(detailOpts);
  detailStarProjection.current = detailStore.optimisticDetailStarUpdate;

  const issuesOpts: IssuesStoreOptions = {
    runtime: appRuntime,
    getPage: gp,
    onDetailSynchronized: reconcileActivityAfterDetailSync,
    sync: {
      refreshSyncStatus: syncStore.refreshSyncStatus,
    },
  };
  if (hs.getGlobalRepo) {
    issuesOpts.getGlobalRepo = hs.getGlobalRepo;
  }
  issuesOpts.supportsIssuePRReferences = () => {
    return settingsStore.getConfiguredRepos().some((repo) => repo.issue_pr_references);
  };
  issuesOpts.getGroupByRepo = hs.getGroupByRepo ?? grouping.getGroupByRepo;
  const issuesStore = createIssuesStore(issuesOpts);

  const diffOpts: DiffStoreOptions = { runtime: appRuntime };
  const diffStore = createDiffStore(diffOpts);
  const repoBrowserStore = createRepoBrowserStore();
  const diffReviewDraftStore = createDiffReviewDraftStore({
    runtime: appRuntime,
    onPublished: (ref, number) =>
      detailStore.refreshDetailOnlyEffect(ref.owner, ref.name, number, {
        provider: ref.provider,
        platformHost: ref.platformHost,
        repoPath: ref.repoPath,
      }),
    onStalePublish: (ref, number) =>
      detailStore.syncDetailEffect(ref.owner, ref.name, number, {
        provider: ref.provider,
        platformHost: ref.platformHost,
        repoPath: ref.repoPath,
      }),
  });

  function handleConfigChanged(event: { valid: boolean }) {
    if (!event.valid) return Effect.void;
    return Effect.gen(function* () {
      const terminalHydration = yield* Effect.sync(() => beginTerminalSettingsHydration(settingsStore));
      const workspaceHydration = yield* Effect.sync(() => beginWorkspaceSettingsHydration(settingsStore));
      const roborevHydration = yield* Effect.sync(() => beginRoborevSettingsHydration(settingsStore));
      const settings = yield* executeGeneratedApiRequest("GET settings after config change", (client, signal) =>
        client.SettingsService.getSettings({ signal }),
      ).pipe(retryIdempotentRead);
      yield* Effect.sync(() => {
        applySettingsHydration(
          { settings: settingsStore, activity: activityStore, issues: issuesStore },
          settings,
          terminalHydration,
          workspaceHydration,
          roborevHydration,
        );
      });
      yield* refreshVisibleData();
    });
  }

  function refreshSelectedActivityDetail() {
    if (gp() !== "activity" && gp() !== "mobile-activity") return Effect.void;
    const selection = getSelectedActivity();
    if (selection?.itemType !== "pr") return Effect.void;
    return detailStore.refreshDetailOnlyEffect(selection.owner, selection.name, selection.number, {
      provider: selection.provider,
      platformHost: selection.platformHost,
      repoPath: selection.repoPath,
    });
  }

  function observeHubFailure<A, E, R>(effect: Effect.Effect<A, E, R>): Effect.Effect<A, E, R> {
    return effect.pipe(
      Effect.tapError((failure) =>
        failure instanceof ApiProblemError && failure.problem.code === ProblemCodes.hubUnavailable
          ? Effect.sync(() => syncStore.setProviderAvailable(false))
          : Effect.void,
      ),
    );
  }

  function refreshVisibleData(refreshDetail = true): Effect.Effect<void, ProviderEventsError, AppServices> {
    switch (gp()) {
      case "pulls":
      case "mobile-pulls":
        return observeHubFailure(pullsStore.reconcilePullsEffect());
      case "issues":
      case "mobile-issues":
        return observeHubFailure(issuesStore.reconcileIssuesEffect());
      case "activity":
      case "mobile-activity":
        return observeHubFailure(
          Effect.all(
            [activityStore.reconcileActivityEffect(), ...(refreshDetail ? [refreshSelectedActivityDetail()] : [])],
            {
              concurrency: "unbounded",
              discard: true,
            },
          ),
        );
      case "focus":
        return observeHubFailure(
          Effect.all([pullsStore.reconcilePullsEffect(), issuesStore.reconcileIssuesEffect()], {
            concurrency: "unbounded",
            discard: true,
          }),
        );
      default:
        return Effect.void;
    }
  }

  function reconcileProviderState() {
    return Effect.all(
      [
        pullsStore.reconcilePullsEffect(),
        issuesStore.reconcileIssuesEffect(),
        activityStore.reconcileActivityEffect(),
        refreshSelectedActivityDetail(),
        syncStore.reconcileSyncStatusEffect,
      ],
      { concurrency: "unbounded", discard: true },
    );
  }

  function reconcileProviderStateAfterHubConnection() {
    return reconcileProviderState().pipe(
      Effect.tapError(() =>
        executeGeneratedApiRequest("probe hub availability after projection refresh failure", (client, signal) =>
          client.SyncService.getSyncStatus({ signal }),
        ).pipe(
          Effect.andThen(Effect.sync(() => syncStore.setProviderAvailable(true))),
          Effect.catch(() => Effect.void),
        ),
      ),
    );
  }

  const eventBasePath = cfg.basePath;
  const eventsStore = createEventsStore({
    runtime: appRuntime,
    ...(eventBasePath != null && {
      getBasePath: () => eventBasePath,
    }),
    onDataChanged: refreshVisibleData,
    onWorkspaceStatus: () => (getShowListAgentStatus() ? refreshVisibleData(false) : Effect.void),
    onSyncStatus: (status) => Effect.sync(() => syncStore.setSyncStatus(status)),
    onHubConnectionChanged: ({ connected }) => {
      if (!connected) {
        return Effect.sync(() => syncStore.setProviderAvailable(false));
      }
      return Effect.sync(() => syncStore.setProviderAvailable(false)).pipe(
        Effect.andThen(reconcileProviderStateAfterHubConnection()),
        Effect.andThen(Effect.sync(() => syncStore.setProviderAvailable(true))),
      );
    },
    onConfigChanged: handleConfigChanged,
    onWorkspaceDeleted: (event) =>
      Effect.gen(function* () {
        yield* Effect.sync(() =>
          notifyWorkspaceDeleted(event.workspace_id, undefined, {
            provider: event.provider,
            platformHost: event.platform_host,
            owner: event.owner,
            name: event.name,
            repoPath: event.repo_path,
            number: event.number,
            itemType: event.item_type,
          }),
        );
        yield* refreshVisibleData();
      }),
    ...(errorCb !== undefined && { onTerminalFailure: errorCb, onRecoverableFailure: errorCb }),
    onPRDetailRefreshed: (ref) => {
      const detail = detailStore.getDetail();
      if (
        detail?.repo?.provider === ref.provider &&
        detail.repo.platform_host === ref.platform_host &&
        detail.repo.repo_path === ref.repo_path &&
        detail.repo_owner === ref.owner &&
        detail.repo_name === ref.name &&
        detail.merge_request?.Number === ref.number
      ) {
        return detailStore.refreshDetailOnlyEffect(ref.owner, ref.name, ref.number, {
          provider: ref.provider,
          platformHost: ref.platform_host,
          repoPath: ref.repo_path,
        });
      }
      return Effect.void;
    },
    onPRCIRefreshed: (ref) => {
      const detail = detailStore.getDetail();
      if (
        detail?.repo?.provider === ref.provider &&
        detail.repo.platform_host === ref.platform_host &&
        detail.repo.repo_path === ref.repo_path &&
        detail.repo_owner === ref.owner &&
        detail.repo_name === ref.name &&
        detail.merge_request?.Number === ref.number
      ) {
        return detailStore.refreshDetailOnlyEffect(ref.owner, ref.name, ref.number, {
          provider: ref.provider,
          platformHost: ref.platform_host,
          repoPath: ref.repo_path,
        });
      }
      return Effect.void;
    },
    onDeferredMergeCompleted: (event) =>
      Effect.gen(function* () {
        const refreshes: Array<Effect.Effect<void, ProviderEventsError, AppServices>> = [
          pullsStore.reconcilePullsEffect(),
          activityStore.reconcileActivityEffect(),
        ];
        const detail = detailStore.getDetail();
        if (
          detail?.repo?.provider === event.provider &&
          detail.repo.platform_host === event.platform_host &&
          detail.repo.repo_path === event.repo_path &&
          detail.repo_owner === event.owner &&
          detail.repo_name === event.name &&
          detail.merge_request?.Number === event.number
        ) {
          refreshes.push(
            detailStore.refreshDetailOnlyEffect(event.owner, event.name, event.number, {
              provider: event.provider,
              platformHost: event.platform_host,
              repoPath: event.repo_path,
            }),
          );
        }
        yield* Effect.all(refreshes, { concurrency: "unbounded", discard: true });
        yield* Effect.sync(() => {
          if (event.status === "merged") {
            if (event.workspace_cleanup_warning) {
              warningCb?.(
                `${event.owner}/${event.name}#${event.number} merged, but the workspace was not pruned: ${event.workspace_cleanup_warning}`,
              );
            } else {
              notificationCb?.(`${event.owner}/${event.name}#${event.number} merged after CI passed.`);
            }
          } else {
            errorCb?.(
              `Deferred merge for ${event.owner}/${event.name}#${event.number} failed: ${event.error ?? "checks did not pass"}`,
            );
          }
        });
      }),
    onReconnectStale: ({ hub_connected }) => {
      const markUnavailable = Effect.sync(() => syncStore.setProviderAvailable(false));
      if (hub_connected === false) return markUnavailable;
      // The replay ring rolled past the client's cursor while it was
      // disconnected. Hide cached provider projections until every
      // authoritative read succeeds because no missed event can be assumed
      // to replay.
      return markUnavailable.pipe(
        Effect.andThen(reconcileProviderStateAfterHubConnection()),
        Effect.andThen(Effect.sync(() => syncStore.setProviderAvailable(true))),
      );
    },
  });

  const si: StoreInstances = {
    pulls: pullsStore,
    issues: issuesStore,
    detail: detailStore,
    activity: activityStore,
    sync: syncStore,
    diff: diffStore,
    repoBrowser: repoBrowserStore,
    diffReviewDraft: diffReviewDraftStore,
    grouping,
    detailActivityView,
    collapsedRepos,
    settings: settingsStore,
    events: eventsStore,
  };

  let roborevClient: RoborevClient | undefined;
  if (roborevBase) {
    const bp = (cfg.basePath ?? "/").replace(/\/$/, "");
    roborevClient = createRoborevClient(bp + roborevBase);
    const roborevOwner = makeRoborevOwner("app-reviews");

    const jobsOpts: Parameters<typeof createJobsStore>[0] = {
      client: roborevClient,
      runtime: appRuntime,
      owner: roborevOwner,
      navigate: nav,
    };
    if (errorCb) jobsOpts.onError = errorCb;
    const jobsStore = createJobsStore(jobsOpts);
    si.roborevJobs = jobsStore;

    const reviewOpts: Parameters<typeof createReviewStore>[0] = {
      client: roborevClient,
      runtime: appRuntime,
      owner: roborevOwner,
    };
    if (errorCb) reviewOpts.onError = errorCb;
    const reviewStore = createReviewStore(reviewOpts);
    si.roborevReview = reviewStore;

    const logStore = createLogStore({
      runtime: appRuntime,
      baseUrl: bp + roborevBase,
      ...(errorCb !== undefined && { onError: errorCb }),
    });
    si.roborevLog = logStore;

    const daemon = createDaemonStore({
      client: roborevClient,
      runtime: appRuntime,
    });
    si.roborevDaemon = daemon;
  }

  const listAgentStatusPolling = pollWhileVisible(
    Effect.suspend(() => refreshVisibleData(false)).pipe(Effect.catch(() => Effect.void)),
    "5 seconds",
    { immediate: true },
  );

  return { stores: si, listAgentStatusPolling, ...(roborevClient !== undefined && { roborevClient }) };
}
