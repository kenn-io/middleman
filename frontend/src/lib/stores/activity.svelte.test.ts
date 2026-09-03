import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { GeneratedClient } from "../api/generated-api.js";
import type { OwnedAppRuntime } from "../app/runtime.js";
import type { ActivityItem, ActivitySettings, ActivitySubject, WorkspaceActivitySubject } from "../api/types.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import {
  buildActivityItemTypeFilter,
  buildActivityFilterTypes,
  createActivityStore as createRuntimeActivityStore,
  type ActivityStoreOptions,
  DEFAULT_ACTIVITY_ITEM_TYPES,
  DEFAULT_EVENT_TYPES,
  isActivityItemTypeEnabled,
  notificationDbId,
} from "./activity.svelte.js";
import { dismissFlash, getFlash, getFlashes } from "./flash.svelte.js";
import { involvesMeFilterStorageKey } from "./involves-me-filter.js";
import { unassignedFilterStorageKey } from "./unassigned-filter.js";

let runtime: OwnedAppRuntime | undefined;

const fakeClient = {
  GET: async () => ({
    data: { items: [], capped: false },
    error: null,
  }),
} as unknown as GeneratedClient;

type TestActivityStoreOptions = Omit<ActivityStoreOptions, "runtime"> & { readonly client: GeneratedClient };

function createActivityStore(options: TestActivityStoreOptions) {
  const { client, ...storeOptions } = options;
  runtime = makeTestAppRuntime(client);
  return createRuntimeActivityStore({ ...storeOptions, runtime });
}

function settings(collapse: boolean, useWorkspaceActivityForRecency = false): ActivitySettings {
  return {
    view_mode: "threaded",
    time_range: "7d",
    hide_closed: false,
    hide_bots: false,
    collapse_threads: collapse,
    default_branch_retention_days: 90,
    default_branch_max_commits: 5000,
    use_workspace_activity_for_recency: useWorkspaceActivityForRecency,
  };
}

function makeStore() {
  return createActivityStore({ client: fakeClient });
}

function workspaceActivity(itemNumber: number): WorkspaceActivitySubject {
  return {
    activity_at: `2026-08-09T12:00:0${itemNumber}Z`,
    item_number: itemNumber,
    item_state: "open",
    item_title: `PR ${itemNumber}`,
    item_type: "pr",
    item_url: `https://github.com/acme/widgets/pull/${itemNumber}`,
    platform_host: "github.com",
    repo: { host: "github.com", owner: "acme", name: "widgets" },
    repo_name: "widgets",
    repo_owner: "acme",
    workspace: { id: `workspace-${itemNumber}`, status: "ready" },
  };
}

function itemActivity(itemNumber: number): ActivitySubject {
  return {
    activity_at: `2026-08-09T12:00:0${itemNumber}Z`,
    item_number: itemNumber,
    item_state: "open",
    item_title: `PR ${itemNumber}`,
    item_type: "pr",
    item_url: `https://github.com/acme/widgets/pull/${itemNumber}`,
    platform_host: "github.com",
    repo: { host: "github.com", owner: "acme", name: "widgets" },
    repo_name: "widgets",
    repo_owner: "acme",
  };
}

beforeEach(() => {
  runtime = undefined;
  localStorage.clear();
  window.history.replaceState(null, "", "/");
});

afterEach(async () => {
  for (const item of getFlashes()) dismissFlash(item.id);
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

describe("activity store workspace activity", () => {
  it("persists and sends the Involves me filter", async () => {
    const get = vi.fn(async () => ({ data: { items: [], capped: false }, error: null }));
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });

    store.setInvolvesMe(true);
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(localStorage.getItem(involvesMeFilterStorageKey("activity"))).toBe("1");
    expect(get).toHaveBeenCalledWith(
      "/activity",
      expect.objectContaining({
        params: { query: expect.objectContaining({ involves_me: true }) },
      }),
    );
  });

  it("persists and sends the Unassigned filter", async () => {
    const get = vi.fn(async () => ({ data: { items: [], capped: false }, error: null }));
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });

    store.setUnassigned(true);
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(localStorage.getItem(unassignedFilterStorageKey("activity"))).toBe("1");
    expect(get).toHaveBeenCalledWith(
      "/activity",
      expect.objectContaining({
        params: { query: expect.objectContaining({ unassigned: true }) },
      }),
    );
  });

  it("retains the complete workspace snapshot returned with an activity read", async () => {
    const snapshot = [workspaceActivity(7)];
    const client = {
      GET: async () => ({ data: { items: [], capped: false, workspace_activity: snapshot }, error: null }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });

    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(store.getWorkspaceActivity()).toEqual(snapshot);
  });

  it("retains the authoritative item snapshot returned with an activity read", async () => {
    const snapshot = [itemActivity(7)];
    const client = {
      GET: async () => ({ data: { items: [], capped: false, item_activity: snapshot }, error: null }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });

    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(store.getItemActivity()).toEqual(snapshot);
  });

  it("retains parent-snapshot truncation independently from event truncation", async () => {
    const client = {
      GET: async () => ({
        data: { items: [], capped: false, item_activity: [itemActivity(7)], item_activity_capped: true },
        error: null,
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });

    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(store.isActivityCapped()).toBe(false);
    expect(store.isItemActivityCapped()).toBe(true);
  });
});

describe("activity store collapse state", () => {
  async function expectInitialExpandedFeedToPage(
    initialize: (store: ReturnType<typeof createActivityStore>) => void,
  ): Promise<void> {
    const newest = notificationItem("ntf:initial-expanded-newest", "unread");
    const older = notificationItem("ntf:initial-expanded-older", "read");
    const projections: Array<string | undefined> = [];
    const get = vi.fn(async (path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      const projection = options?.params?.query?.projection;
      projections.push(projection);
      if (projection === "events") {
        return {
          data: {
            items: [newest, older],
            item_activity: [],
            workspace_activity: [],
            capped: false,
            event_cursor: "snapshot",
          },
          error: null,
        };
      }
      return {
        data: {
          items: [newest],
          item_activity: [],
          workspace_activity: [],
          capped: true,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    initialize(store);

    store.loadActivity();

    await vi.waitFor(() => expect(projections).toEqual(["full", "events"]));
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));
    expect(store.getActivityItems().map((item) => item.id)).toEqual([newest.id, older.id]);
  }

  it("requests the collapsed projection for the default collapsed threaded view", async () => {
    const get = vi.fn(async () => ({
      data: { items: [], item_activity: [], workspace_activity: [], capped: false, event_cursor: "hidden:9" },
      error: null,
    }));
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));

    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(get).toHaveBeenCalledWith(
      "/activity",
      expect.objectContaining({
        params: { query: expect.objectContaining({ projection: "collapsed" }) },
      }),
    );
    expect(store.getActivityEventCursor()).toBe("hidden:9");
  });

  it("requests a bounded collapsed projection for mobile", async () => {
    const get = vi.fn(async () => ({
      data: { items: [], item_activity: [], workspace_activity: [], capped: false, event_cursor: "hidden:9" },
      error: null,
    }));
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));

    store.setActivityPageLimit(30);
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(get).toHaveBeenCalledWith(
      "/activity",
      expect.objectContaining({
        params: { query: expect.objectContaining({ projection: "collapsed", limit: 30 }) },
      }),
    );
  });

  it("loads one bounded event-preview page for a visible mobile card", async () => {
    const subject = {
      ...itemActivity(7),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        repo_path: "acme/widgets",
        owner: "acme",
        name: "widgets",
        host: "github.com",
      },
    } satisfies ActivitySubject;
    const threadQueries: Array<Record<string, unknown>> = [];
    const get = vi.fn(async (path: string, options: { params?: { query?: Record<string, unknown> } }) => {
      if (path === "/activity/thread-events") {
        threadQueries.push(options.params?.query ?? {});
        return {
          data: {
            items: [],
            item_activity: [],
            workspace_activity: [],
            capped: true,
            next_cursor: "another-page",
          },
          error: null,
        };
      }
      return {
        data: { items: [], item_activity: [subject], workspace_activity: [], capped: false, event_cursor: "hidden:9" },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(false));
    store.setActivityPageLimit(30);
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([subject]));
    expect(threadQueries).toHaveLength(0);

    store.loadThreadPreview("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadQueries).toHaveLength(1));

    expect(threadQueries[0]).toEqual(expect.objectContaining({ limit: 10, at_or_before: "hidden:9" }));
    expect(threadQueries[0]).not.toHaveProperty("before");
  });

  it("pages a capped feed when persisted settings initialize threaded mode expanded", async () => {
    await expectInitialExpandedFeedToPage((store) => {
      store.hydrateDefaults(settings(false));
      store.initializeFromMount();
    });
  });

  it("pages a capped feed when the URL initializes threaded mode expanded", async () => {
    window.history.replaceState(null, "", "/?collapsed=0");
    await expectInitialExpandedFeedToPage((store) => {
      store.hydrateDefaults(settings(true));
      store.initializeFromMount();
    });
  });

  it("requests full events while a mobile activity consumer is mounted", async () => {
    const get = vi.fn(async () => ({
      data: { items: [], item_activity: [], workspace_activity: [], capped: false, event_cursor: "hidden:9" },
      error: null,
    }));
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));

    store.setFullEventProjectionRequired(true);
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    expect(get).toHaveBeenCalledWith(
      "/activity",
      expect.objectContaining({
        params: { query: expect.objectContaining({ projection: "full" }) },
      }),
    );
  });

  it("keeps authoritative mobile reconciliation on the full projection", async () => {
    const projections: Array<string | undefined> = [];
    const get = vi.fn(async (path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      projections.push(options?.params?.query?.projection);
      return {
        data: { items: [], item_activity: [], workspace_activity: [], capped: false, event_cursor: "hidden:9" },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.setFullEventProjectionRequired(true);

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile mobile activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    expect(projections).toEqual(["full"]);
  });

  it("loads every frozen page of a collapsed thread once by stable provider repository identity", async () => {
    const subject = {
      ...itemActivity(7),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        repo_path: "acme/widgets",
        owner: "acme",
        name: "widgets",
        host: "github.com",
      },
    } satisfies ActivitySubject;
    const threadQueries: Array<Record<string, unknown>> = [];
    const get = vi.fn(async (path: string, options: { params?: { query?: Record<string, unknown> } }) => {
      if (path !== "/activity/thread-events") {
        return {
          data: {
            items: [],
            item_activity: [subject],
            workspace_activity: [],
            capped: false,
            event_cursor: "hidden:9",
          },
          error: null,
        };
      }
      const query = options.params?.query ?? {};
      threadQueries.push(query);
      return {
        data: {
          items: [],
          item_activity: [],
          workspace_activity: [],
          capped: query.before === undefined,
          event_cursor: "hidden:9",
          ...(query.before === undefined ? { next_cursor: "thread-page-2" } : {}),
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.setUnassigned(true);
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    const key = "github|github.com|id|repo-7:pr:7";
    store.toggleThreadItem(key);
    await vi.waitFor(() => expect(threadQueries).toHaveLength(2));
    store.toggleThreadItem(key);
    store.toggleThreadItem(key);

    expect(threadQueries).toEqual([
      expect.objectContaining({
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        item_type: "pr",
        item_number: 7,
        unassigned: true,
        at_or_before: "hidden:9",
      }),
      expect.objectContaining({ before: "thread-page-2", at_or_before: "hidden:9", unassigned: true }),
    ]);
  });

  it("reloads expanded threads after a foreground collapsed-scope reload", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const subject = { ...itemActivity(7), repo } satisfies ActivitySubject;
    const firstEvent = {
      ...notificationItem("ntf:before-scope-reload", "read"),
      item_number: 7,
      body_preview: "before scope reload",
      repo,
    } as ActivityItem;
    const reloadedEvent = {
      ...notificationItem("ntf:after-scope-reload", "read"),
      item_number: 7,
      body_preview: "after scope reload",
      repo,
    } as ActivityItem;
    let activityReads = 0;
    let threadReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return {
          data: {
            items: [threadReads === 1 ? firstEvent : reloadedEvent],
            capped: false,
            event_cursor: `thread-snapshot-${threadReads}`,
          },
          error: null,
        };
      }
      activityReads += 1;
      return {
        data: {
          items: [],
          item_activity: [subject],
          workspace_activity: [],
          capped: false,
          event_cursor: `activity-snapshot-${activityReads}`,
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([subject]));

    const key = "github|github.com|id|repo-7:pr:7";
    store.toggleThreadItem(key);
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([firstEvent.id]));

    store.setTimeRange("30d");
    store.loadActivity();

    await vi.waitFor(() => expect(threadReads).toBe(2));
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([reloadedEvent.id]));
    expect(store.isThreadItemExpanded(key)).toBe(true);
  });

  it("reloads expanded child projections when filtered reconciliation supersedes a foreground load", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const subject = { ...itemActivity(7), event_ledger_revision: "unchanged", repo } satisfies ActivitySubject;
    const staleComment = {
      ...notificationItem("pre:stale-comment", "read"),
      activity_type: "comment",
      item_number: 7,
      repo,
    } as ActivityItem;
    const filteredReview = {
      ...notificationItem("pre:filtered-review", "read"),
      activity_type: "review",
      item_number: 7,
      repo,
    } as ActivityItem;
    const pendingForeground = Promise.withResolvers<{
      data: {
        items: ActivityItem[];
        item_activity: ActivitySubject[];
        workspace_activity: never[];
        capped: false;
        event_cursor: string;
      };
      error: null;
    }>();
    let activityReads = 0;
    const threadQueries: Array<Record<string, unknown>> = [];
    const get = vi.fn((path: string, options?: { params?: { query?: Record<string, unknown> } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (path === "/activity/thread-events") {
        threadQueries.push(options?.params?.query ?? {});
        return Promise.resolve({
          data: {
            items: [threadQueries.length === 1 ? staleComment : filteredReview],
            capped: false,
            event_cursor: "thread-snapshot",
          },
          error: null,
        });
      }
      activityReads += 1;
      if (activityReads === 2) return pendingForeground.promise;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [subject],
          workspace_activity: [],
          capped: false as const,
          event_cursor: `activity-snapshot-${activityReads}`,
        },
        error: null as const,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([subject]));

    const key = "github|github.com|id|repo-7:pr:7";
    store.toggleThreadItem(key);
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([staleComment.id]));

    store.setActivityFilterTypes(["review"]);
    store.loadActivity();
    await vi.waitFor(() => expect(activityReads).toBe(2));
    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile filtered activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(threadQueries).toHaveLength(2));
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([filteredReview.id]));
    expect(threadQueries[1]).toEqual(expect.objectContaining({ types: ["review"] }));

    pendingForeground.resolve({
      data: {
        items: [],
        item_activity: [subject],
        workspace_activity: [],
        capped: false,
        event_cursor: "stale-foreground",
      },
      error: null,
    });
  });

  it("forwards active event and search filters when loading a collapsed thread", async () => {
    const subject = {
      ...itemActivity(7),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        repo_path: "acme/widgets",
        owner: "acme",
        name: "widgets",
        host: "github.com",
      },
    } satisfies ActivitySubject;
    let threadQuery: Record<string, unknown> | undefined;
    const store = createActivityStore({
      client: makeGeneratedClient({
        ActivityService: {
          listActivityThreadEvents: async (params) => {
            threadQuery = params;
            return { items: [], capped: false, event_cursor: "snapshot" };
          },
          listActivity: async () => ({
            items: [],
            item_activity: [subject],
            workspace_activity: [],
            capped: false,
            item_activity_capped: false,
            use_workspace_activity_for_recency: true,
            event_cursor: "snapshot",
          }),
        },
      }),
    });
    store.hydrateDefaults(settings(true));
    store.setActivityFilterTypes(["comment", "notification"]);
    store.setActivitySearch("reviewer");
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadQuery).toBeDefined());

    expect(threadQuery).toEqual(
      expect.objectContaining({
        types: ["comment", "notification"],
        search: "reviewer",
      }),
    );
  });

  it("loads complete history for a parent discovered while Expand all is active", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const firstSubject = { ...itemActivity(7), repo } satisfies ActivitySubject;
    const newSubject = { ...itemActivity(8), repo } satisfies ActivitySubject;
    const newEvent = {
      ...notificationItem("ntf:new-thread", "unread"),
      item_number: 8,
      repo,
    } as ActivityItem;
    let snapshotReads = 0;
    const threadQueries: Array<Record<string, unknown>> = [];
    const bulkQueries: Array<Record<string, unknown>> = [];
    const get = vi.fn(async (path: string, options: { params?: { query?: Record<string, unknown> } }) => {
      const query = options.params?.query ?? {};
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadQueries.push(query);
        return { data: { items: [newEvent], capped: false, event_cursor: "snapshot" }, error: null };
      }
      if (query.projection === "events") {
        bulkQueries.push(query);
        return {
          data: { items: bulkQueries.length === 1 ? [] : [newEvent], capped: false, event_cursor: "snapshot" },
          error: null,
        };
      }
      snapshotReads += 1;
      return {
        data: {
          items: [],
          item_activity: snapshotReads === 1 ? [firstSubject] : [firstSubject, newSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([firstSubject]));
    store.expandAllThreads();
    await vi.waitFor(() => expect(bulkQueries).toHaveLength(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile expanded activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(bulkQueries).toHaveLength(2));
    expect(threadQueries).toHaveLength(0);
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toContain(newEvent.id));
  });

  it("restarts an in-flight thread load when authoritative recency advances", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = { ...itemActivity(7), repo } satisfies ActivitySubject;
    const advancedSubject = {
      ...originalSubject,
      activity_at: "2026-08-09T13:00:07Z",
    } satisfies ActivitySubject;
    const staleEvent = {
      ...notificationItem("ntf:stale-thread-page", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const freshEvent = {
      ...notificationItem("ntf:fresh-thread-page", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const pendingOldThread = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: false; event_cursor: string };
      error: null;
    }>();
    const threadQueries: Array<Record<string, unknown>> = [];
    let activityReads = 0;
    const get = vi.fn((path: string, options: { params?: { query?: Record<string, unknown> } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (path === "/activity/thread-events") {
        const query = options.params?.query ?? {};
        threadQueries.push(query);
        if (threadQueries.length === 1) return pendingOldThread.promise;
        return Promise.resolve({
          data: { items: [freshEvent], capped: false, event_cursor: "new-snapshot" },
          error: null,
        });
      }
      activityReads += 1;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [activityReads === 1 ? originalSubject : advancedSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: activityReads === 1 ? "old-snapshot" : "new-snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadQueries).toHaveLength(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity during thread load in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;
    pendingOldThread.resolve({
      data: { items: [staleEvent], capped: false, event_cursor: "old-snapshot" },
      error: null,
    });

    await vi.waitFor(() => expect(threadQueries).toHaveLength(2));
    expect(threadQueries.map((query) => query.at_or_before)).toEqual(["old-snapshot", "new-snapshot"]);
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([freshEvent.id]));
  });

  it("discards a superseded thread failure after its replacement succeeds", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = { ...itemActivity(7), repo } satisfies ActivitySubject;
    const advancedSubject = {
      ...originalSubject,
      activity_at: "2026-08-09T13:00:07Z",
    } satisfies ActivitySubject;
    const freshEvent = {
      ...notificationItem("ntf:fresh-thread-page", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const pendingOldThread = Promise.withResolvers<{
      error: {
        code: string;
        detail: string;
        title: string;
        type: string;
      };
      response: Response;
    }>();
    let activityReads = 0;
    let threadReads = 0;
    const get = vi.fn((path: string) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (path === "/activity/thread-events") {
        threadReads += 1;
        if (threadReads === 1) return pendingOldThread.promise;
        return Promise.resolve({
          data: { items: [freshEvent], capped: false, event_cursor: "new-snapshot" },
          error: null,
        });
      }
      activityReads += 1;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [activityReads === 1 ? originalSubject : advancedSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: activityReads === 1 ? "old-snapshot" : "new-snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadReads).toBe(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity during superseded thread failure test",
      safeContext: {},
      onFailure: () => {},
    }).exit;
    await vi.waitFor(() => expect(threadReads).toBe(2));
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([freshEvent.id]));

    pendingOldThread.resolve({
      error: {
        code: "serviceUnavailable",
        detail: "stale thread failure",
        title: "Service unavailable",
        type: "about:blank",
      },
      response: new Response(null, { status: 503 }),
    });

    await vi.waitFor(() => expect(pendingOldThread.promise).resolves.toBeDefined());
    expect(store.getThreadLoadError()).toBeNull();
  });

  it("reloads a loaded thread when an older event changes its ledger revision", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = {
      ...itemActivity(7),
      repo,
      event_ledger_revision: "pre:1",
    } as ActivitySubject;
    const backfilledSubject = {
      ...originalSubject,
      event_ledger_revision: "pre:2",
    } as ActivitySubject;
    const recentEvent = {
      ...notificationItem("ntf:recent-thread-event", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const olderEvent = {
      ...notificationItem("ntf:older-backfilled-event", "unread"),
      item_number: 7,
      repo,
      created_at: "2026-08-08T12:00:00Z",
    } as ActivityItem;
    let activityReads = 0;
    let threadReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return {
          data: {
            items: threadReads === 1 ? [recentEvent] : [recentEvent, olderEvent],
            capped: false,
            event_cursor: "snapshot",
          },
          error: null,
        };
      }
      activityReads += 1;
      return {
        data: {
          items: [],
          item_activity: [activityReads === 1 ? originalSubject : backfilledSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadReads).toBe(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile backfilled activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(threadReads).toBe(2));
    await vi.waitFor(() =>
      expect(store.getActivityItems().map((item) => item.id)).toEqual([recentEvent.id, olderEvent.id]),
    );
  });

  it("reloads an expanded thread after its parent disappears and reappears", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = {
      ...itemActivity(7),
      repo,
      event_ledger_revision: "pre:1",
    } as ActivitySubject;
    const reappearedSubject = {
      ...originalSubject,
      event_ledger_revision: "pre:2",
    } as ActivitySubject;
    const originalEvent = {
      ...notificationItem("ntf:original-thread-event", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const reappearedEvent = {
      ...notificationItem("ntf:reappeared-thread-event", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    let activityReads = 0;
    let threadReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return {
          data: {
            items: threadReads === 1 ? [originalEvent] : [reappearedEvent],
            capped: false,
            event_cursor: "snapshot",
          },
          error: null,
        };
      }
      activityReads += 1;
      return {
        data: {
          items: [],
          item_activity: activityReads === 1 ? [originalSubject] : activityReads === 2 ? [] : [reappearedSubject],
          item_activity_capped: false,
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([originalEvent.id]));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile disappeared activity parent in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");

    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile reappeared activity parent in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    expect(threadReads).toBe(1);
    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");

    await vi.waitFor(() => expect(threadReads).toBe(2));
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([reappearedEvent.id]));
  });

  it("uses bulk paging when collapsed reconciliation discovers globally expanded parents", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const subjects = [7, 8, 9].map((number) => ({ ...itemActivity(number), repo }));
    let snapshotReads = 0;
    let bulkReads = 0;
    let threadReads = 0;
    const get = vi.fn(async (path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return { data: { items: [], capped: false, event_cursor: "snapshot" }, error: null };
      }
      if (options?.params?.query?.projection === "events") {
        bulkReads += 1;
        return { data: { items: [], capped: false, event_cursor: "snapshot" }, error: null };
      }
      snapshotReads += 1;
      return {
        data: {
          items: [],
          item_activity: snapshotReads === 1 ? [] : subjects,
          item_activity_capped: false,
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(false));
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile new globally expanded parents in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(bulkReads).toBe(1));
    expect(threadReads).toBe(0);
  });

  it("replaces edited and deleted cached events after a capped ledger revision change", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = {
      ...itemActivity(7),
      repo,
      event_ledger_revision: "pre:1",
    } as ActivitySubject;
    const changedSubject = {
      ...originalSubject,
      event_ledger_revision: "pre:2",
    } as ActivitySubject;
    const cachedEditedEvent = {
      ...notificationItem("ntf:edited-event", "read"),
      item_number: 7,
      body_preview: "old comment body",
      repo,
    } as ActivityItem;
    const cachedDeletedEvent = {
      ...notificationItem("ntf:deleted-event", "read"),
      item_number: 7,
      body_preview: "deleted comment body",
      repo,
    } as ActivityItem;
    const refreshedEditedEvent = {
      ...cachedEditedEvent,
      body_preview: "new comment body",
    } as ActivityItem;
    let activityReads = 0;
    let threadReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return {
          data: {
            items: threadReads === 1 ? [cachedEditedEvent, cachedDeletedEvent] : [refreshedEditedEvent],
            capped: false,
            event_cursor: "snapshot",
          },
          error: null,
        };
      }
      activityReads += 1;
      return {
        data: {
          items: [],
          item_activity: [activityReads === 1 ? originalSubject : changedSubject],
          item_activity_capped: activityReads > 1,
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(threadReads).toBe(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile capped edited activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(threadReads).toBe(2));
    await vi.waitFor(() =>
      expect(store.getActivityItems().map((item) => [item.id, item.body_preview])).toEqual([
        [refreshedEditedEvent.id, refreshedEditedEvent.body_preview],
      ]),
    );
  });

  it("keeps thread pages atomic across a later failure and retry", async () => {
    const subject = {
      ...itemActivity(7),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        repo_path: "acme/widgets",
        owner: "acme",
        name: "widgets",
        host: "github.com",
      },
    } satisfies ActivitySubject;
    const staleEditedEvent = {
      ...notificationItem("ntf:edited-thread-event", "unread"),
      item_number: 7,
      body_preview: "stale body",
      repo: subject.repo,
    } as ActivityItem;
    const staleDeletedEvent = {
      ...notificationItem("ntf:deleted-thread-event", "unread"),
      item_number: 7,
      body_preview: "deleted body",
      repo: subject.repo,
    } as ActivityItem;
    const refreshedEditedEvent = { ...staleEditedEvent, body_preview: "refreshed body" };
    let threadReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (path === "/activity/thread-events") {
        threadReads += 1;
        if (threadReads === 1) {
          return {
            data: {
              items: [staleEditedEvent, staleDeletedEvent],
              capped: true,
              event_cursor: "snapshot",
              next_cursor: "page-2",
            },
            error: null,
          };
        }
        if (threadReads === 2) {
          return {
            error: {
              code: "serviceUnavailable",
              detail: "thread history unavailable",
              title: "Service unavailable",
              type: "about:blank",
            },
            response: new Response(null, { status: 503 }),
          };
        }
        return {
          data: { items: [refreshedEditedEvent], capped: false, event_cursor: "snapshot" },
          error: null,
        };
      }
      return {
        data: {
          items: [],
          item_activity: [subject],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([subject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() => expect(store.getThreadLoadError()).toBe("thread history unavailable"));
    expect(store.getActivityItems()).toEqual([]);

    store.retryFailedThreadLoads();

    await vi.waitFor(() => expect(threadReads).toBe(3));
    await vi.waitFor(() => expect(store.getThreadLoadError()).toBeNull());
    expect(store.getActivityItems().map((item) => [item.id, item.body_preview])).toEqual([
      [refreshedEditedEvent.id, refreshedEditedEvent.body_preview],
    ]);
  });

  it("discards a thread page that resolves after the activity scope changes", async () => {
    const subject = {
      ...itemActivity(7),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-7",
        repo_path: "acme/widgets",
        owner: "acme",
        name: "widgets",
        host: "github.com",
      },
    } satisfies ActivitySubject;
    const staleEvent = {
      ...notificationItem("ntf:stale-thread", "unread"),
      item_number: 7,
      repo: subject.repo,
    } as ActivityItem;
    const pendingThread = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: false; event_cursor: string };
      error: null;
    }>();
    let activityReads = 0;
    const get = vi.fn((path: string) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (path === "/activity/thread-events") return pendingThread.promise;
      activityReads += 1;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: activityReads === 1 ? [subject] : [],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([subject]));

    store.toggleThreadItem("github|github.com|id|repo-7:pr:7");
    await vi.waitFor(() =>
      expect(get.mock.calls.filter(([path]) => path === "/activity/thread-events")).toHaveLength(1),
    );
    store.setTimeRange("30d");
    store.loadActivity();
    await vi.waitFor(() => expect(activityReads).toBe(2));

    pendingThread.resolve({ data: { items: [staleEvent], capped: false, event_cursor: "snapshot" }, error: null });
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(store.getActivityItems()).toEqual([]);
  });

  it("expands all through sequential frozen event pages", async () => {
    const first = notificationItem("ntf:71", "unread");
    const second = notificationItem("ntf:70", "unread");
    const eventQueries: Array<Record<string, unknown>> = [];
    const get = vi.fn(async (path: string, options: { params?: { query?: Record<string, unknown> } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      const query = options.params?.query ?? {};
      if (query.projection === "events") {
        eventQueries.push(query);
        if (query.before === undefined) {
          return {
            data: {
              items: [first],
              item_activity: [],
              workspace_activity: [],
              capped: true,
              event_cursor: "snapshot",
              next_cursor: "page-2",
            },
            error: null,
          };
        }
        return {
          data: { items: [second], item_activity: [], workspace_activity: [], capped: false, event_cursor: "snapshot" },
          error: null,
        };
      }
      return {
        data: {
          items: [],
          item_activity: [itemActivity(7)],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.expandAllThreads();

    await vi.waitFor(() => expect(eventQueries).toHaveLength(2));
    expect(eventQueries[0]).toEqual(
      expect.objectContaining({
        projection: "events",
        limit: 500,
        at_or_before: "snapshot",
      }),
    );
    expect(eventQueries[1]).toEqual(expect.objectContaining({ before: "page-2" }));
    expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:71", "ntf:70"]);
  });

  it("keeps bulk expansion when an older collapsed foreground load resolves later", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const firstSubject = { ...itemActivity(7), repo } satisfies ActivitySubject;
    const staleSubject = { ...itemActivity(8), repo } satisfies ActivitySubject;
    const bulkEvent = {
      ...notificationItem("ntf:bulk-after-expand", "unread"),
      item_number: 7,
      repo,
    } as ActivityItem;
    const pendingCollapsed = Promise.withResolvers<{
      data: {
        items: never[];
        item_activity: ActivitySubject[];
        workspace_activity: never[];
        capped: false;
        event_cursor: string;
      };
      error: null;
    }>();
    let snapshotReads = 0;
    let bulkReads = 0;
    let threadReads = 0;
    const get = vi.fn((path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (path === "/activity/thread-events") {
        threadReads += 1;
        return Promise.resolve({ data: { items: [], capped: false, event_cursor: "stale-snapshot" }, error: null });
      }
      if (options?.params?.query?.projection === "events") {
        bulkReads += 1;
        return Promise.resolve({
          data: { items: [bulkEvent], capped: false, event_cursor: "initial-snapshot" },
          error: null,
        });
      }
      snapshotReads += 1;
      if (snapshotReads === 2) return pendingCollapsed.promise;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [firstSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: "initial-snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([firstSubject]));

    store.loadActivity();
    await vi.waitFor(() => expect(snapshotReads).toBe(2));
    store.expandAllThreads();
    await vi.waitFor(() => expect(bulkReads).toBe(1));
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([bulkEvent.id]));

    pendingCollapsed.resolve({
      data: {
        items: [],
        item_activity: [firstSubject, staleSubject],
        workspace_activity: [],
        capped: false,
        event_cursor: "stale-snapshot",
      },
      error: null,
    });
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(store.getActivityItems().map((item) => item.id)).toEqual([bulkEvent.id]);
    expect(threadReads).toBe(0);
    expect(store.isActivityLoading()).toBe(false);
  });

  it("restarts bulk expansion when reconciliation advances a parent ledger revision", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-7",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
      host: "github.com",
    };
    const originalSubject = {
      ...itemActivity(7),
      repo,
      event_ledger_revision: "pre:1",
    } satisfies ActivitySubject;
    const advancedSubject = {
      ...originalSubject,
      event_ledger_revision: "pre:2",
    } satisfies ActivitySubject;
    const staleEvent = {
      ...notificationItem("ntf:bulk-sync-race", "unread"),
      body_preview: "stale body",
      item_number: 7,
      repo,
    } as ActivityItem;
    const freshEvent = { ...staleEvent, body_preview: "fresh body" };
    const pendingOldBulk = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: false; event_cursor: string };
      error: null;
    }>();
    let snapshotReads = 0;
    let bulkReads = 0;
    const get = vi.fn((path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (options?.params?.query?.projection === "events") {
        bulkReads += 1;
        if (bulkReads === 1) return pendingOldBulk.promise;
        return Promise.resolve({
          data: { items: [freshEvent], capped: false, event_cursor: "new-snapshot" },
          error: null,
        });
      }
      snapshotReads += 1;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [snapshotReads === 1 ? originalSubject : advancedSubject],
          workspace_activity: [],
          capped: false,
          event_cursor: snapshotReads === 1 ? "old-snapshot" : "new-snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getItemActivity()).toEqual([originalSubject]));

    store.expandAllThreads();
    await vi.waitFor(() => expect(bulkReads).toBe(1));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity during bulk expansion in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() => expect(bulkReads).toBe(2));
    await vi.waitFor(() =>
      expect(store.getActivityItems().map((item) => [item.id, item.body_preview])).toEqual([
        [freshEvent.id, freshEvent.body_preview],
      ]),
    );

    pendingOldBulk.resolve({
      data: { items: [staleEvent], capped: false, event_cursor: "old-snapshot" },
      error: null,
    });
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(store.getActivityItems().map((item) => [item.id, item.body_preview])).toEqual([
      [freshEvent.id, freshEvent.body_preview],
    ]);
  });

  it("atomically replaces edited and deleted events after a failed bulk retry", async () => {
    const staleEditedEvent = notificationItem("ntf:edited-bulk-event", "unread");
    const staleDeletedEvent = notificationItem("ntf:deleted-bulk-event", "unread");
    const refreshedEditedEvent = { ...staleEditedEvent, body_preview: "refreshed body" };
    let bulkReads = 0;
    const get = vi.fn(async (path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (options?.params?.query?.projection === "events") {
        bulkReads += 1;
        if (bulkReads === 1) {
          return {
            data: {
              items: [refreshedEditedEvent],
              capped: true,
              event_cursor: "snapshot",
              next_cursor: "page-2",
            },
            error: null,
          };
        }
        if (bulkReads === 2) {
          return {
            error: {
              code: "serviceUnavailable",
              detail: "bulk history unavailable",
              title: "Service unavailable",
              type: "about:blank",
            },
            response: new Response(null, { status: 503 }),
          };
        }
        return {
          data: { items: [refreshedEditedEvent], capped: false, event_cursor: "snapshot" },
          error: null,
        };
      }
      return {
        data: {
          items: [staleEditedEvent, staleDeletedEvent],
          item_activity: [itemActivity(1)],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.expandAllThreads();
    await vi.waitFor(() => expect(store.getActivityError()).toBe("bulk history unavailable"));
    expect(store.getActivityItems().map((item) => item.id)).toEqual([staleEditedEvent.id, staleDeletedEvent.id]);

    store.expandAllThreads();
    await vi.waitFor(() => expect(bulkReads).toBe(3));
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));
    expect(store.getActivityItems().map((item) => [item.id, item.body_preview])).toEqual([
      [refreshedEditedEvent.id, refreshedEditedEvent.body_preview],
    ]);
  });

  it("discards bulk pages that resolve after the activity scope changes", async () => {
    const staleEvent = { ...notificationItem("ntf:stale-bulk", "unread"), created_at: new Date().toISOString() };
    const freshEvent = { ...notificationItem("ntf:fresh-scope", "unread"), created_at: new Date().toISOString() };
    const pendingBulk = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: false; event_cursor: string };
      error: null;
    }>();
    const pendingForeground = Promise.withResolvers<{
      data: {
        items: ActivityItem[];
        item_activity: never[];
        workspace_activity: never[];
        capped: false;
        event_cursor: string;
      };
      error: null;
    }>();
    let snapshotReads = 0;
    const get = vi.fn((path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      if (options?.params?.query?.projection === "events") return pendingBulk.promise;
      snapshotReads += 1;
      if (snapshotReads === 2) return pendingForeground.promise;
      return Promise.resolve({
        data: {
          items: [],
          item_activity: [],
          workspace_activity: [],
          capped: false,
          event_cursor: "snapshot",
        },
        error: null,
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(snapshotReads).toBe(1));

    store.expandAllThreads();
    await vi.waitFor(() =>
      expect(get.mock.calls.some(([, options]) => options?.params?.query?.projection === "events")).toBe(true),
    );
    store.setTimeRange("30d");
    store.loadActivity();
    await vi.waitFor(() => expect(snapshotReads).toBe(2));

    pendingBulk.resolve({ data: { items: [staleEvent], capped: false, event_cursor: "snapshot" }, error: null });
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    expect(store.isActivityLoading()).toBe(true);
    expect(store.getActivityItems()).toEqual([]);

    pendingForeground.resolve({
      data: {
        items: [freshEvent],
        item_activity: [],
        workspace_activity: [],
        capped: false,
        event_cursor: "snapshot",
      },
      error: null,
    });
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([freshEvent.id]));

    expect(store.getActivityItems().map((item) => item.id)).toEqual([freshEvent.id]);
  });

  it("reports a failure from the current bulk expansion", async () => {
    const get = vi.fn(async (path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return { data: { authors: [] }, error: null };
      if (options?.params?.query?.projection === "events") {
        return {
          error: {
            code: "serviceUnavailable",
            detail: "bulk activity unavailable",
            title: "Service unavailable",
            type: "about:blank",
          },
          response: new Response(null, { status: 503 }),
        };
      }
      return {
        data: { items: [], item_activity: [], workspace_activity: [], capped: false, event_cursor: "snapshot" },
        error: null,
      };
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.expandAllThreads();

    await vi.waitFor(() => expect(store.getActivityError()).toBe("bulk activity unavailable"));
    expect(store.isActivityLoading()).toBe(false);
  });

  it("hydrates the workspace activity recency preference", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(false, true));
    expect(s.getUseWorkspaceActivityForRecency()).toBe(true);
  });

  it("treats threads as expanded when the collapse default is false", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(false));
    expect(s.getCollapseThreads()).toBe(false);
    expect(s.isThreadItemExpanded("k1")).toBe(true);
  });

  it("collapseAllThreads collapses everything and clears overrides", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(false));
    s.toggleThreadItem("k1");
    expect(s.isThreadItemExpanded("k1")).toBe(false);
    s.collapseAllThreads();
    expect(s.getCollapseThreads()).toBe(true);
    expect(s.isThreadItemExpanded("k1")).toBe(false);
    expect(s.isThreadItemExpanded("k2")).toBe(false);
  });

  it("toggleThreadItem expands a single item when globally collapsed", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(true));
    expect(s.isThreadItemExpanded("k1")).toBe(false);
    s.toggleThreadItem("k1");
    expect(s.isThreadItemExpanded("k1")).toBe(true);
    expect(s.isThreadItemExpanded("k2")).toBe(false);
  });

  it("toggleThreadItem twice returns an item to the global state", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(false));
    s.toggleThreadItem("k1");
    expect(s.isThreadItemExpanded("k1")).toBe(false);
    s.toggleThreadItem("k1");
    expect(s.isThreadItemExpanded("k1")).toBe(true);
  });

  it("writes collapsed to the URL only when it differs from the server default", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(false));
    s.collapseAllThreads();
    expect(new URLSearchParams(window.location.search).get("collapsed")).toBe("1");
    s.expandAllThreads();
    expect(new URLSearchParams(window.location.search).has("collapsed")).toBe(false);
  });

  it("writes collapsed=0 when expanding against a collapsed server default", () => {
    const s = makeStore();
    s.hydrateDefaults(settings(true));
    s.expandAllThreads();
    expect(new URLSearchParams(window.location.search).get("collapsed")).toBe("0");
    s.collapseAllThreads();
    expect(new URLSearchParams(window.location.search).has("collapsed")).toBe(false);
  });

  it("applies collapsed=0 from the URL over a collapsed server default", () => {
    window.history.replaceState(null, "", "/?collapsed=0");
    const s = makeStore();
    s.hydrateDefaults(settings(true));
    s.initializeFromMount();
    expect(s.getCollapseThreads()).toBe(false);
  });

  it("preserves a live collapsed override when settings reload after init", () => {
    window.history.replaceState(null, "", "/?collapsed=0");
    const s = makeStore();
    s.hydrateDefaults(settings(true));
    s.initializeFromMount();
    expect(s.getCollapseThreads()).toBe(false);
    s.hydrateDefaults(settings(true));
    expect(s.getCollapseThreads()).toBe(false);
  });

  it("clears a redundant collapsed param when the default catches up to it", () => {
    window.history.replaceState(null, "", "/?collapsed=1");
    const s = makeStore();
    s.hydrateDefaults(settings(false));
    s.initializeFromMount();
    expect(s.getCollapseThreads()).toBe(true);
    expect(new URLSearchParams(window.location.search).get("collapsed")).toBe("1");

    // The server default changes to match the live override; the now-redundant
    // param is dropped so a later default change is not shadowed by it.
    s.hydrateDefaults(settings(true));
    expect(s.getCollapseThreads()).toBe(true);
    expect(new URLSearchParams(window.location.search).has("collapsed")).toBe(false);

    s.hydrateDefaults(settings(false));
    expect(s.getCollapseThreads()).toBe(false);
  });
});

describe("buildActivityFilterTypes", () => {
  const allItemTypes = new Set(DEFAULT_ACTIVITY_ITEM_TYPES);
  const allEvents = new Set<string>(DEFAULT_EVENT_TYPES);

  it("returns no filter when everything is selected", () => {
    expect(buildActivityFilterTypes(allItemTypes, allEvents, false)).toEqual([]);
  });

  it("drops default-branch commits but keeps PR commits when Commits is deselected", () => {
    const enabled = new Set(["comment", "review", "force_push"]);
    expect(buildActivityFilterTypes(allItemTypes, enabled, false)).toEqual([
      "new_pr",
      "new_issue",
      "default_branch_force_push",
      "comment",
      "review",
      "commit",
      "force_push",
      "notification",
    ]);
  });

  it("drops default-branch force pushes when the force-push event is deselected", () => {
    const enabled = new Set(["comment", "review", "commit"]);
    expect(buildActivityFilterTypes(allItemTypes, enabled, false)).toEqual([
      "new_pr",
      "new_issue",
      "default_branch_commit",
      "comment",
      "review",
      "commit",
      "notification",
    ]);
  });

  it("excludes all default-branch activity when it is hidden", () => {
    expect(buildActivityFilterTypes(allItemTypes, allEvents, true)).toEqual([
      "new_pr",
      "new_issue",
      "comment",
      "review",
      "commit",
      "force_push",
      "notification",
    ]);
  });

  it("independently controls PR and issue opening events", () => {
    expect(buildActivityFilterTypes(new Set(["issue"]), allEvents, false)).toEqual([
      "new_issue",
      "default_branch_commit",
      "default_branch_force_push",
      "comment",
      "review",
      "force_push",
      "notification",
    ]);
  });

  it("keeps the all-selected shortcut only while notifications stay on", () => {
    expect(buildActivityFilterTypes(allItemTypes, allEvents, false, true)).toEqual([]);
  });

  it("builds an explicit list omitting notifications when they are hidden", () => {
    expect(buildActivityFilterTypes(allItemTypes, allEvents, false, false)).toEqual([
      "new_pr",
      "new_issue",
      "default_branch_commit",
      "default_branch_force_push",
      "comment",
      "review",
      "commit",
      "force_push",
    ]);
  });

  it("gates issue opening rows on comments, the only issue timeline event", () => {
    expect(buildActivityFilterTypes(allItemTypes, new Set(["review"]), true, false)).toEqual([
      "new_pr",
      "review",
      "commit",
    ]);
    expect(buildActivityFilterTypes(allItemTypes, new Set(["force_push"]), true, false)).toEqual([
      "new_pr",
      "commit",
      "force_push",
    ]);
    expect(buildActivityFilterTypes(allItemTypes, new Set(["comment"]), true, false)).toEqual([
      "new_pr",
      "new_issue",
      "comment",
      "commit",
    ]);
  });

  it("keeps PR commits when every top-level event filter is deselected", () => {
    expect(buildActivityFilterTypes(allItemTypes, new Set(), false, true)).toEqual(["commit", "notification"]);
  });

  it("does not include opening events when only default-branch commits are selected", () => {
    expect(buildActivityFilterTypes(allItemTypes, new Set(["commit"]), false, true)).toEqual([
      "default_branch_commit",
      "commit",
      "notification",
    ]);
  });

  it.each([
    {
      name: "notifications off",
      itemTypes: allItemTypes,
      showNotifications: false,
      expected: ["commit"],
    },
    {
      name: "pull requests only",
      itemTypes: new Set<"pr" | "issue">(["pr"]),
      showNotifications: true,
      expected: ["commit", "notification"],
    },
    {
      name: "issues only with notifications",
      itemTypes: new Set<"pr" | "issue">(["issue"]),
      showNotifications: true,
      expected: ["notification"],
    },
    {
      name: "issues only without notifications",
      itemTypes: new Set<"pr" | "issue">(["issue"]),
      showNotifications: false,
      expected: ["none"],
    },
  ])("keeps opening events hidden with no event toggles when $name", ({ itemTypes, showNotifications, expected }) => {
    expect(buildActivityFilterTypes(itemTypes, new Set(), false, showNotifications)).toEqual(expected);
  });

  it("supports repository-level commits with both item types hidden", () => {
    expect(buildActivityFilterTypes(new Set(), new Set(["commit"]), false, false)).toEqual([
      "none",
      "default_branch_commit",
    ]);
  });

  it("marks an empty item scope when notifications remain enabled", () => {
    expect(buildActivityFilterTypes(new Set(), new Set(), true, true)).toEqual(["none", "notification"]);
  });

  it("encodes a fully empty selection as an explicit nonmatching filter", () => {
    expect(buildActivityFilterTypes(new Set(), new Set(), true, false)).toEqual(["none"]);
  });
});

describe("buildActivityItemTypeFilter", () => {
  it("keeps repository activity eligible while filtering item-scoped rows before the cap", () => {
    expect(buildActivityItemTypeFilter(new Set(["issue"]))).toEqual(["issue", "repo"]);
    expect(buildActivityItemTypeFilter(new Set())).toEqual(["repo"]);
  });
});

describe("isActivityItemTypeEnabled", () => {
  it("filters PR and issue rows without filtering repository-level rows", () => {
    const issuesOnly = new Set<"pr" | "issue">(["issue"]);
    expect(isActivityItemTypeEnabled("pr", issuesOnly)).toBe(false);
    expect(isActivityItemTypeEnabled("issue", issuesOnly)).toBe(true);
    expect(isActivityItemTypeEnabled("", new Set())).toBe(true);
  });
});

describe("activity store URL hydration", () => {
  it("round trips the selected author", () => {
    window.history.replaceState(null, "", "/?author=Alice");
    const s = makeStore();
    s.initializeFromMount();

    expect(s.getActivityAuthor()).toBe("Alice");
    s.setActivityAuthor(undefined);
    s.syncToURL();
    expect(new URLSearchParams(window.location.search).has("author")).toBe(false);
  });

  it("restores mandatory PR commits without re-enabling default-branch commits", () => {
    window.history.replaceState(null, "", "/?event_types=comment,review,force_push");
    const s = makeStore();
    s.initializeFromMount();
    expect(s.getActivityFilterTypes()).toEqual([
      "new_pr",
      "new_issue",
      "default_branch_force_push",
      "comment",
      "review",
      "commit",
      "force_push",
      "notification",
    ]);
    expect(new URLSearchParams(window.location.search).get("event_types")).toBe("comment,review,force_push");
    expect(new URLSearchParams(window.location.search).has("types")).toBe(false);
    expect(s.getEnabledEvents().has("commit")).toBe(false);
  });

  it.each([
    {
      name: "the all-items empty-event encoding",
      legacyTypes: "notification",
      expectedItems: ["pr", "issue"],
      expectedEvents: [],
      expectedItemParam: null,
      expectedEventParam: "none",
    },
    {
      name: "a notified commit-only filter",
      legacyTypes: "commit,notification",
      expectedItems: [],
      expectedEvents: ["commit"],
      expectedItemParam: "none",
      expectedEventParam: "commit",
    },
    {
      name: "a manually bookmarked commit-only filter",
      legacyTypes: "commit",
      expectedItems: [],
      expectedEvents: ["commit"],
      expectedItemParam: "none",
      expectedEventParam: "commit",
    },
    {
      name: "an issue-only comment filter",
      legacyTypes: "new_issue,comment,notification",
      expectedItems: ["issue"],
      expectedEvents: ["comment"],
      expectedItemParam: "issue",
      expectedEventParam: "comment",
    },
    {
      name: "a pull-request filter with default-branch commits enabled",
      legacyTypes: "new_pr,default_branch_commit,comment,commit",
      expectedItems: ["pr"],
      expectedEvents: ["comment", "commit"],
      expectedItemParam: "pr",
      expectedEventParam: "comment,commit",
    },
  ])(
    "migrates bookmarked legacy types for $name",
    ({ legacyTypes, expectedItems, expectedEvents, expectedItemParam, expectedEventParam }) => {
      window.history.replaceState(null, "", `/?types=${legacyTypes}`);
      const first = makeStore();
      first.initializeFromMount();

      expect([...first.getEnabledItemTypes()]).toEqual(expectedItems);
      expect([...first.getEnabledEvents()]).toEqual(expectedEvents);
      const normalized = new URLSearchParams(window.location.search);
      expect(normalized.has("types")).toBe(false);
      expect(normalized.get("item_types")).toBe(expectedItemParam);
      expect(normalized.get("event_types")).toBe(expectedEventParam);

      const fresh = makeStore();
      fresh.initializeFromMount();
      expect([...fresh.getEnabledItemTypes()]).toEqual(expectedItems);
      expect([...fresh.getEnabledEvents()]).toEqual(expectedEvents);
    },
  );

  it("normalizes explicit default item and event selections out of the URL", () => {
    window.history.replaceState(null, "", "/?item_types=pr,issue&event_types=comment,review,commit,force_push");
    const s = makeStore();
    s.initializeFromMount();
    expect(s.getActivityFilterTypes()).toEqual([]);
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.has("item_types")).toBe(false);
    expect(normalized.has("event_types")).toBe(false);
  });

  it.each([
    {
      name: "PRs only",
      itemTypes: "pr",
      eventTypes: "comment",
      expected: ["pr"],
      filterTypes: ["new_pr", "comment", "commit", "notification"],
    },
    {
      name: "issues only",
      itemTypes: "issue",
      eventTypes: "comment",
      expected: ["issue"],
      filterTypes: ["new_issue", "comment", "notification"],
    },
    {
      name: "neither item type",
      itemTypes: "none",
      eventTypes: "commit",
      expected: [],
      filterTypes: ["none", "default_branch_commit", "notification"],
    },
  ])("hydrates $name independently from event selections", ({ itemTypes, eventTypes, expected, filterTypes }) => {
    window.history.replaceState(null, "", `/?item_types=${itemTypes}&event_types=${eventTypes}`);
    const s = makeStore();
    s.initializeFromMount();
    expect([...s.getEnabledItemTypes()]).toEqual(expected);
    expect(s.getActivityFilterTypes()).toEqual(filterTypes);
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.get("item_types")).toBe(itemTypes);
    expect(normalized.get("event_types")).toBe(eventTypes);
    expect(normalized.has("types")).toBe(false);
  });

  it("round trips default item scope with every event toggle disabled after URL normalization", () => {
    window.history.replaceState(null, "", "/?item_types=pr,issue&event_types=none");
    const first = makeStore();
    first.initializeFromMount();

    expect([...first.getEnabledItemTypes()]).toEqual(DEFAULT_ACTIVITY_ITEM_TYPES);
    expect([...first.getEnabledEvents()]).toEqual([]);
    expect(first.getActivityFilterTypes()).toEqual(["commit", "notification"]);
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.has("item_types")).toBe(false);
    expect(normalized.get("event_types")).toBe("none");
    expect(normalized.has("types")).toBe(false);

    const fresh = makeStore();
    fresh.initializeFromMount();
    expect([...fresh.getEnabledItemTypes()]).toEqual(DEFAULT_ACTIVITY_ITEM_TYPES);
    expect([...fresh.getEnabledEvents()]).toEqual([]);
    expect(fresh.getActivityFilterTypes()).toEqual(["commit", "notification"]);
  });

  it("round trips item scope independently from the default-branch commit toggle", () => {
    window.history.replaceState(null, "", "/?item_types=issue&event_types=comment,review,force_push");
    const first = makeStore();
    first.initializeFromMount();

    expect([...first.getEnabledItemTypes()]).toEqual(["issue"]);
    expect([...first.getEnabledEvents()]).toEqual(["comment", "review", "force_push"]);
    expect(first.getActivityFilterTypes()).toEqual([
      "new_issue",
      "default_branch_force_push",
      "comment",
      "review",
      "force_push",
      "notification",
    ]);

    const fresh = makeStore();
    fresh.initializeFromMount();
    expect([...fresh.getEnabledItemTypes()]).toEqual(["issue"]);
    expect([...fresh.getEnabledEvents()]).toEqual(["comment", "review", "force_push"]);
  });

  it("round trips a fully empty selection without restoring defaults", () => {
    window.history.replaceState(null, "", "/?item_types=none&event_types=none&notif=0&hide_branch=1");
    const s = makeStore();
    s.initializeFromMount();

    expect([...s.getEnabledItemTypes()]).toEqual([]);
    expect([...s.getEnabledEvents()]).toEqual([]);
    expect(s.getShowNotifications()).toBe(false);
    expect(s.getHideDefaultBranchActivity()).toBe(true);
    expect(s.getActivityFilterTypes()).toEqual(["none"]);
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.get("item_types")).toBe("none");
    expect(normalized.get("event_types")).toBe("none");
    expect(normalized.has("types")).toBe(false);
  });
});

describe("activity store author candidates", () => {
  it("keeps a URL-selected author available when it is absent from the current candidates", () => {
    window.history.replaceState(null, "", "/?author=FormerUser");
    const s = makeStore();
    s.initializeFromMount();

    expect(s.getActivityAuthors()).toEqual(["FormerUser"]);
  });

  it("preserves the selected spelling when a candidate differs only by case", async () => {
    window.history.replaceState(null, "", "/?author=Alice");
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") {
        return { data: { authors: ["ALICE", "Bob"] }, error: null };
      }
      return { data: { items: [], capped: false }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
    });
    s.initializeFromMount();

    s.loadActivityAuthors();

    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Alice", "Bob"]));
    expect(s.getActivityAuthor()).toBe("Alice");
  });

  it("filters the feed by author while candidate requests only use repo and time range", async () => {
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors: ["Alice"] }, error: null };
      return { data: { items: [], capped: false }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
      getGlobalRepo: () => "github|github.com/acme/widget",
    });
    s.setActivityAuthor("Alice");
    s.setActivitySearch("cache");
    s.setActivityFilterTypes(["comment"]);

    s.loadActivityAuthors();
    s.loadActivity();
    await vi.waitFor(() => expect(get.mock.calls.some(([path]) => path === "/activity/authors")).toBe(true));
    await vi.waitFor(() => expect(get.mock.calls.some(([path]) => path === "/activity")).toBe(true));

    const authorCall = get.mock.calls.find(([path]) => path === "/activity/authors");
    expect(authorCall?.[1]).toEqual({
      params: {
        query: {
          repo: "github|github.com/acme/widget",
          since: expect.any(String),
        },
      },
      signal: expect.any(AbortSignal),
    });
    const feedCall = get.mock.calls.find(([path]) => path === "/activity");
    expect(feedCall?.[1]).toEqual({
      params: {
        query: expect.objectContaining({
          author: "Alice",
          search: "cache",
          types: ["comment"],
        }),
      },
      signal: expect.any(AbortSignal),
    });
  });

  it("reports candidate errors independently from feed errors", async () => {
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") {
        return { error: { detail: "authors unavailable" }, response: new Response(null, { status: 500 }) };
      }
      return { data: { items: [], capped: false }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
    });

    s.loadActivityAuthors();
    await vi.waitFor(() => expect(s.getActivityAuthorsError()).toBe("authors unavailable"));

    expect(s.getActivityAuthorsError()).toBe("authors unavailable");
    expect(s.getActivityError()).toBeNull();
  });

  it("clears candidates for a new scope and retries that scope after failure", async () => {
    let repo = "github|github.com/acme/widgets";
    let secondScopeFails = true;
    const get = vi.fn(async (path: string) => {
      if (path !== "/activity/authors") {
        return { data: { items: [], capped: false }, error: null };
      }
      if (repo.endsWith("widgets")) {
        return { data: { authors: ["Alice"] }, error: null };
      }
      if (secondScopeFails) {
        return { error: { detail: "authors unavailable" }, response: new Response(null, { status: 500 }) };
      }
      return { data: { authors: ["Bob"] }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
      getGlobalRepo: () => repo,
    });

    s.loadActivityAuthors();
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Alice"]));

    repo = "github|github.com/acme/tools";
    s.setTimeRange("30d");
    s.loadActivityAuthors();
    await vi.waitFor(() => expect(s.getActivityAuthorsError()).toBe("authors unavailable"));
    expect(s.getActivityAuthors()).toEqual([]);

    secondScopeFails = false;
    s.loadActivityAuthors();
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Bob"]));
    expect(s.getActivityAuthorsError()).toBeNull();
    expect(get.mock.calls.filter(([path]) => path === "/activity/authors")).toHaveLength(3);
  });

  it("refreshes same-scope candidates during activity reconciliation", async () => {
    let authors = ["Alice"];
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors }, error: null };
      return { data: { items: [], capped: false }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
    });

    s.loadActivityAuthors();
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Alice"]));

    authors = ["Alice", "FreshActor"];
    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(s.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    expect(s.getActivityAuthors()).toEqual(["Alice", "FreshActor"]);
    expect(get.mock.calls.filter(([path]) => path === "/activity/authors")).toHaveLength(2);
  });

  it("force-refreshes same-scope candidates with an Activity load", async () => {
    let authors = ["Alice"];
    let feedReads = 0;
    const get = vi.fn(async (path: string) => {
      if (path === "/activity/authors") return { data: { authors }, error: null };
      feedReads += 1;
      return { data: { items: [], capped: false }, error: null };
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
    });

    s.loadActivity();
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Alice"]));

    authors = ["Bob"];
    s.loadActivity(true);
    await vi.waitFor(() => expect(feedReads).toBe(2));
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Bob"]));
    expect(get.mock.calls.filter(([path]) => path === "/activity/authors")).toHaveLength(2);
  });

  it("replaces an in-flight same-scope author refresh during a foreground activity load", async () => {
    type ActivityResponse = { data: { items: never[]; capped: false }; error: null };

    let activityRequests = 0;
    let authorRequests = 0;
    let resolveReconciliation!: (response: ActivityResponse) => void;
    const pendingReconciliation = new Promise<ActivityResponse>((resolve) => {
      resolveReconciliation = resolve;
    });
    const get = vi.fn((path: string, options?: { signal?: AbortSignal }) => {
      if (path === "/activity/authors") {
        authorRequests += 1;
        if (authorRequests > 1) {
          return Promise.resolve({ data: { authors: ["Bob"] }, error: null });
        }
        return new Promise((_, reject) => {
          options?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), {
            once: true,
          });
        });
      }

      activityRequests += 1;
      if (activityRequests === 1) return pendingReconciliation;
      return Promise.resolve({ data: { items: [], capped: false }, error: null });
    });
    const s = createActivityStore({
      client: { GET: get } as unknown as Parameters<typeof createActivityStore>[0]["client"],
    });

    if (runtime === undefined) throw new Error("test runtime was not created");
    const reconciliation = runtime.runCommand(s.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => {
      expect(activityRequests).toBe(1);
      expect(authorRequests).toBe(1);
      expect(s.isActivityAuthorsLoading()).toBe(true);
    });

    s.loadActivity();
    await vi.waitFor(() => {
      expect(activityRequests).toBe(2);
      expect(authorRequests).toBe(2);
    });
    resolveReconciliation({ data: { items: [], capped: false }, error: null });
    await reconciliation.exit;

    await vi.waitFor(() => expect(s.isActivityAuthorsLoading()).toBe(false));
    await vi.waitFor(() => expect(s.getActivityAuthors()).toEqual(["Bob"]));
    expect(authorRequests).toBe(2);
  });
});

describe("activity store projection scope", () => {
  it("clears a foreground error when same-scope reconciliation succeeds", async () => {
    const pendingReconciliation = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const freshItem = notificationItem("ntf:fresh", "unread");
    let feedReads = 0;
    const get = vi.fn((path: string) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      feedReads += 1;
      if (feedReads === 1) return pendingReconciliation.promise;
      return Promise.resolve({
        error: {
          code: "serviceUnavailable",
          detail: "activity temporarily unavailable",
          title: "Service unavailable",
          type: "about:blank",
        },
        response: new Response(null, { status: 503 }),
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });

    if (runtime === undefined) throw new Error("test runtime was not created");
    const reconciliation = runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(feedReads).toBe(1));

    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityError()).toBe("activity temporarily unavailable"));

    pendingReconciliation.resolve({ data: { items: [freshItem], capped: false }, error: null });
    await reconciliation.exit;

    expect(store.getActivityItems()).toEqual([freshItem]);
    expect(store.getActivityError()).toBeNull();
  });

  it("rejects an older unfiltered reconciliation after an Involves me load fails", async () => {
    const pendingReconciliation = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const staleItem = notificationItem("ntf:stale", "unread");
    let feedReads = 0;
    const get = vi.fn((path: string, options?: { params?: { query?: { involves_me?: boolean } } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      feedReads += 1;
      if (feedReads === 1) {
        expect(options?.params?.query?.involves_me).toBeUndefined();
        return pendingReconciliation.promise;
      }
      expect(options?.params?.query?.involves_me).toBe(true);
      return Promise.resolve({
        error: {
          code: "validationError",
          detail: "filtered activity unavailable",
          title: "Invalid request",
          type: "about:blank",
        },
        response: new Response(null, { status: 400 }),
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });

    if (runtime === undefined) throw new Error("test runtime was not created");
    const reconciliation = runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(feedReads).toBe(1));

    store.setInvolvesMe(true);
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityError()).toBe("filtered activity unavailable"));

    pendingReconciliation.resolve({ data: { items: [staleItem], capped: false }, error: null });
    await reconciliation.exit;

    expect(store.getActivityItems()).toEqual([]);
  });

  it("rejects an older collapsed reconciliation after a full projection load fails", async () => {
    const pendingReconciliation = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const staleItem = notificationItem("ntf:stale-collapsed", "unread");
    let feedReads = 0;
    const get = vi.fn((path: string, options?: { params?: { query?: { projection?: string } } }) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      feedReads += 1;
      if (feedReads === 1) {
        expect(options?.params?.query?.projection).toBe("collapsed");
        return pendingReconciliation.promise;
      }
      expect(options?.params?.query?.projection).toBe("full");
      return Promise.resolve({
        error: {
          code: "serviceUnavailable",
          detail: "full activity unavailable",
          title: "Service unavailable",
          type: "about:blank",
        },
        response: new Response(null, { status: 503 }),
      });
    });
    const store = createActivityStore({ client: { GET: get } as unknown as GeneratedClient });
    store.hydrateDefaults(settings(true));

    if (runtime === undefined) throw new Error("test runtime was not created");
    const reconciliation = runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile collapsed activity in test",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(feedReads).toBe(1));

    store.setViewMode("flat");
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityError()).toBe("full activity unavailable"));

    pendingReconciliation.resolve({ data: { items: [staleItem], capped: false }, error: null });
    await reconciliation.exit;

    expect(store.getActivityItems()).toEqual([]);
  });
});

describe("activity store notification visibility", () => {
  it("shows notifications by default and persists hiding them via the notif param", () => {
    const s = makeStore();
    s.initializeFromMount();
    expect(s.getShowNotifications()).toBe(true);
    // Default-on, all-selected: no explicit type filter at all.
    expect(s.getActivityFilterTypes()).toEqual([]);
    expect(new URLSearchParams(window.location.search).has("notif")).toBe(false);

    s.setShowNotifications(false);
    s.setActivityFilterTypes(
      buildActivityFilterTypes(s.getEnabledItemTypes(), s.getEnabledEvents(), s.getHideDefaultBranchActivity(), false),
    );
    s.syncToURL();
    expect(new URLSearchParams(window.location.search).get("notif")).toBe("0");
    // Hiding notifications sends the explicit non-notification list.
    expect(s.getActivityFilterTypes()).not.toContain("notification");

    const next = makeStore();
    next.initializeFromMount();
    expect(next.getShowNotifications()).toBe(false);
    expect(next.getActivityFilterTypes()).not.toContain("notification");
  });
});

describe("activity store default-branch visibility", () => {
  it("shows default-branch activity by default and persists the hide flag", () => {
    const s = makeStore();
    s.initializeFromMount();
    expect(s.getHideDefaultBranchActivity()).toBe(false);

    s.setHideDefaultBranchActivity(true);
    s.syncToURL();
    expect(new URLSearchParams(window.location.search).get("hide_branch")).toBe("1");

    const next = makeStore();
    next.initializeFromMount();
    expect(next.getHideDefaultBranchActivity()).toBe(true);

    next.setHideDefaultBranchActivity(false);
    next.syncToURL();
    expect(new URLSearchParams(window.location.search).has("hide_branch")).toBe(false);
  });

  it("keeps the Commits toggle enabled while default-branch activity is hidden", () => {
    window.history.replaceState(null, "", "/?hide_branch=1&notif=0");
    const first = makeStore();
    first.initializeFromMount();
    expect(first.getEnabledEvents().has("commit")).toBe(true);
    expect(first.getActivityFilterTypes()).not.toContain("default_branch_commit");

    const fresh = makeStore();
    fresh.initializeFromMount();
    expect(fresh.getEnabledEvents().has("commit")).toBe(true);
    fresh.setHideDefaultBranchActivity(false);
    fresh.syncToURL();
    expect(fresh.getActivityFilterTypes()).toContain("default_branch_commit");
  });

  it("migrates a legacy hidden-branch URL without disabling the Commits toggle", () => {
    window.history.replaceState(
      null,
      "",
      "/?types=new_pr,new_issue,comment,review,commit,force_push,notification&hide_branch=1",
    );
    const first = makeStore();
    first.initializeFromMount();

    expect(first.getHideDefaultBranchActivity()).toBe(true);
    expect(first.getEnabledEvents()).toEqual(new Set(DEFAULT_EVENT_TYPES));
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.has("types")).toBe(false);
    expect(normalized.has("event_types")).toBe(false);
    expect(normalized.get("hide_branch")).toBe("1");

    const fresh = makeStore();
    fresh.initializeFromMount();
    expect(fresh.getEnabledEvents()).toEqual(new Set(DEFAULT_EVENT_TYPES));
  });

  it("drops a stale default-branch token when the legacy Commits toggle is absent", () => {
    window.history.replaceState(
      null,
      "",
      "/?types=new_pr,new_issue,default_branch_commit,comment,review,force_push,notification&hide_branch=1",
    );
    const first = makeStore();
    first.initializeFromMount();

    expect(first.getHideDefaultBranchActivity()).toBe(true);
    expect(first.getEnabledEvents()).toEqual(new Set(["comment", "review", "force_push"]));
    const normalized = new URLSearchParams(window.location.search);
    expect(normalized.has("types")).toBe(false);
    expect(normalized.get("event_types")).toBe("comment,review,force_push");
    expect(normalized.get("hide_branch")).toBe("1");

    const fresh = makeStore();
    fresh.initializeFromMount();
    expect(fresh.getEnabledEvents()).toEqual(new Set(["comment", "review", "force_push"]));
  });
});

describe("activity store commit roll-up", () => {
  it("shows individual commits by default and persists the URL override for rolled-up commits", () => {
    const s = makeStore();
    s.initializeFromMount();
    expect(s.getRollUpCommits()).toBe(false);

    s.setRollUpCommits(true);
    s.syncToURL();
    expect(new URLSearchParams(window.location.search).get("rollup_commits")).toBe("1");

    const next = makeStore();
    next.initializeFromMount();
    expect(next.getRollUpCommits()).toBe(true);

    next.setRollUpCommits(false);
    next.syncToURL();
    expect(new URLSearchParams(window.location.search).has("rollup_commits")).toBe(false);
  });
});

function notificationItem(id: string, state: "unread" | "read"): ActivityItem {
  return {
    id,
    cursor: id,
    activity_type: "notification",
    item_type: "pr",
    item_number: 1,
    item_state: state,
    body_preview: "review_requested",
    repo_owner: "acme",
    repo_name: "widgets",
    platform_host: "github.com",
  } as unknown as ActivityItem;
}

describe("notificationDbId", () => {
  it("parses ntf-source ids and rejects everything else", () => {
    expect(notificationDbId("ntf:42")).toBe(42);
    expect(notificationDbId("pr:1")).toBeNull();
    expect(notificationDbId("ntf:0")).toBeNull();
    expect(notificationDbId("ntf:-3")).toBeNull();
    expect(notificationDbId("ntf:abc")).toBeNull();
    expect(notificationDbId("ntf:")).toBeNull();
  });
});

describe("activity store markNotificationSeen", () => {
  function storeWith(post: ReturnType<typeof vi.fn>) {
    const client = {
      GET: async () => ({ data: { items: [notificationItem("ntf:42", "unread")], capped: false }, error: null }),
      POST: post,
    } as unknown as GeneratedClient;
    return createActivityStore({ client });
  }

  it("flips the row to read and queues the upstream GitHub read", async () => {
    const post = vi.fn(async () => ({ data: { queued: [42], succeeded: [], failed: [] }, error: null }));
    const s = storeWith(post);
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));
    expect(s.getActivityItems()[0]!.item_state).toBe("unread");

    const result = s.markNotificationSeen(s.getActivityItems()[0]!);

    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    expect(post).toHaveBeenCalledWith("/notifications/read", {
      body: { ids: [42] },
      signal: expect.any(AbortSignal),
    });
    expect(s.getActivityItems()[0]!.item_state).toBe("read");
  });

  it("rolls back the optimistic flip when the request fails", async () => {
    const post = vi.fn(async () => ({ error: { detail: "boom" }, response: new Response(null, { status: 500 }) }));
    const s = storeWith(post);
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    const result = s.markNotificationSeen(s.getActivityItems()[0]!);

    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(s.getActivityItems()[0]!.item_state).toBe("unread"));
    expect(s.getActivityItems()[0]!.item_state).toBe("unread");
    expect(getFlash()).toMatchObject({ message: "boom", tone: "danger" });
  });

  it("rolls back when the bulk response reports the id as failed despite a 200", async () => {
    const post = vi.fn(async () => ({
      data: { succeeded: [], queued: [], failed: [{ id: 42, error: "not found" }] },
      error: null,
    }));
    const s = storeWith(post);
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    const result = s.markNotificationSeen(s.getActivityItems()[0]!);

    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(s.getActivityItems()[0]!.item_state).toBe("unread"));
    expect(s.getActivityItems()[0]!.item_state).toBe("unread");
    expect(getFlash()).toMatchObject({ message: "Failed to mark notification as read.", tone: "danger" });
  });

  it("does not let an older failed acknowledgement roll back a newer read", async () => {
    const first = Promise.withResolvers<{
      error: { detail: string };
      response: Response;
    }>();
    const post = vi
      .fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue({ data: { queued: [42], succeeded: [], failed: [] }, error: null });
    const s = storeWith(post);
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    s.markNotificationSeen(s.getActivityItems()[0]!);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    s.markNotificationSeen(s.getActivityItems()[0]!);
    first.resolve({ error: { detail: "boom" }, response: new Response(null, { status: 500 }) });

    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(s.getActivityItems()[0]!.item_state).toBe("read"));
  });

  it("does not let an activity read started before acknowledgement restore unread state", async () => {
    const olderRead = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    let feedReads = 0;
    const get = vi.fn((path: string) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      feedReads++;
      if (feedReads === 1) {
        return Promise.resolve({
          data: { items: [notificationItem("ntf:42", "unread")], capped: false },
          error: null,
        });
      }
      return olderRead.promise;
    });
    const post = vi.fn().mockResolvedValue({
      data: { queued: [42], succeeded: [], failed: [] },
      error: null,
    });
    const s = createActivityStore({ client: { GET: get, POST: post } as unknown as GeneratedClient });
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    s.loadActivity();
    await vi.waitFor(() => expect(feedReads).toBe(2));
    s.markNotificationSeen(s.getActivityItems()[0]!);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    olderRead.resolve({ data: { items: [notificationItem("ntf:42", "unread")], capped: false }, error: null });

    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));
    expect(s.getActivityItems()[0]!.item_state).toBe("read");
  });

  it("keeps the acknowledgement authoritative over a read started while the mutation is pending", async () => {
    const pendingRead = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const acknowledgement = Promise.withResolvers<{
      data: { queued: number[]; succeeded: number[]; failed: never[] };
      error: null;
    }>();
    let feedReads = 0;
    const get = vi.fn((path: string) => {
      if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
      feedReads++;
      if (feedReads === 1) {
        return Promise.resolve({
          data: { items: [notificationItem("ntf:42", "unread")], capped: false },
          error: null,
        });
      }
      return pendingRead.promise;
    });
    const post = vi.fn(() => acknowledgement.promise);
    const s = createActivityStore({ client: { GET: get, POST: post } as unknown as GeneratedClient });
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    s.markNotificationSeen(s.getActivityItems()[0]!);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    s.loadActivity();
    await vi.waitFor(() => expect(feedReads).toBe(2));
    acknowledgement.resolve({ data: { queued: [42], succeeded: [], failed: [] }, error: null });
    await vi.waitFor(() => expect(s.getActivityItems()[0]!.item_state).toBe("read"));
    pendingRead.resolve({ data: { items: [notificationItem("ntf:42", "unread")], capped: false }, error: null });

    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));
    expect(s.getActivityItems()[0]!.item_state).toBe("read");
  });

  it("ignores rows that are not notification feed rows", async () => {
    const post = vi.fn(async () => ({ data: { queued: [], succeeded: [], failed: [] }, error: null }));
    const s = storeWith(post);
    s.loadActivity();
    await vi.waitFor(() => expect(s.isActivityLoading()).toBe(false));

    const result = s.markNotificationSeen({ ...s.getActivityItems()[0]!, id: "pr:7" });

    expect(result).toBeUndefined();
    expect(post).not.toHaveBeenCalled();
  });
});

describe("activity polling recovery", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("reconciles retained item events from stable authoritative parent identities", async () => {
    const now = new Date().toISOString();
    const retained = {
      id: "pre:7",
      cursor: "pre:7",
      activity_type: "comment",
      activity_url: "https://github.com/acme/widgets/pull/7#issuecomment-42",
      author: "event-author",
      body_preview: "Existing comment",
      created_at: now,
      item_author: "old-parent-author",
      item_number: 7,
      item_state: "open",
      item_title: "Old title",
      item_type: "pr",
      item_url: "https://github.com/acme/widgets/pull/7",
      platform_host: "github.com",
      repo_owner: "acme",
      repo_name: "widgets",
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-original",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
      },
      workspace: { id: "old-workspace", status: "ready" },
    } as ActivityItem;
    const retainedNotification = {
      ...retained,
      id: "ntf:7",
      cursor: "ntf:7",
      activity_type: "notification",
      activity_url: "https://github.com/acme/widgets/pull/7",
      item_state: "unread",
    } as ActivityItem;
    const renamed = {
      activity_at: now,
      item_author: "current-parent-author",
      item_number: 7,
      item_state: "merged",
      item_title: "Current title",
      item_type: "pr",
      item_url: "https://github.com/acme/widgets-renamed/pull/7",
      platform_host: "github.com",
      repo_owner: "acme",
      repo_name: "widgets-renamed",
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-original",
        owner: "acme",
        name: "widgets-renamed",
        repo_path: "acme/widgets-renamed",
      },
      workspace: { id: "current-workspace", status: "ready" },
    } as ActivitySubject;
    const replacement = {
      ...renamed,
      item_title: "Replacement title",
      item_url: "https://github.com/acme/widgets/pull/7",
      repo_name: "widgets",
      repo: {
        ...renamed.repo,
        platform_repo_id: "repo-replacement",
        name: "widgets",
        repo_path: "acme/widgets",
      },
    } as ActivitySubject;
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1) {
          return {
            data: { items: [retained, retainedNotification], capped: false, event_cursor: retained.cursor },
            error: null,
          };
        }
        return {
          data: { items: [], item_activity: [renamed, replacement], capped: false, event_cursor: retained.cursor },
          error: null,
        };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems()).toEqual([retained, retainedNotification]));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    await vi.waitFor(() =>
      expect(store.getActivityItems().map((item) => item.repo.repo_path)).toEqual([
        "acme/widgets-renamed",
        "acme/widgets-renamed",
      ]),
    );
    const reconciledEvent = store.getActivityItems().find((item) => item.id === retained.id);
    const reconciledNotification = store.getActivityItems().find((item) => item.id === retainedNotification.id);
    expect(reconciledEvent).toEqual(
      expect.objectContaining({
        activity_url: retained.activity_url,
        author: "event-author",
        item_author: "current-parent-author",
        item_state: "merged",
        item_title: "Current title",
        item_url: "https://github.com/acme/widgets-renamed/pull/7",
        platform_host: "github.com",
        repo_owner: "acme",
        repo_name: "widgets-renamed",
        repo: renamed.repo,
        workspace: renamed.workspace,
      }),
    );
    expect(reconciledNotification).toEqual(
      expect.objectContaining({
        activity_url: renamed.item_url,
        item_url: renamed.item_url,
        repo: renamed.repo,
      }),
    );
  });

  it("replaces the feed on the scheduled full poll so cursor-behind activity becomes visible", async () => {
    vi.useFakeTimers();
    const now = new Date().toISOString();
    const stale = { ...notificationItem("ntf:stale", "unread"), created_at: now, item_type: "", item_number: 0 };
    const persistedBehindCursor = {
      ...notificationItem("ntf:persisted", "unread"),
      created_at: now,
      item_type: "",
      item_number: 0,
    };
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1)
          return { data: { items: [stale], capped: false, event_cursor: stale.cursor }, error: null };
        if (feedReads <= 4) return { data: { items: [], capped: false, event_cursor: stale.cursor }, error: null };
        if (feedReads === 5)
          return {
            data: { items: [persistedBehindCursor], capped: false, event_cursor: persistedBehindCursor.cursor },
            error: null,
          };
        throw new Error(`unexpected activity request ${feedReads}`);
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems()).toEqual([stale]));

    store.startActivityPolling();
    await vi.advanceTimersByTimeAsync(45_000);
    await vi.waitFor(() => expect(feedReads).toBe(5));

    expect(store.getActivityItems()).toEqual([persistedBehindCursor]);
  });

  it("clears a foreground error when a replacement poll succeeds afterward", async () => {
    const pendingPoll = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const initial = notificationItem("ntf:initial", "unread");
    const replacement = notificationItem("ntf:replacement", "unread");
    let feedReads = 0;
    const client = {
      GET: vi.fn((path: string) => {
        if (path === "/activity/authors") return Promise.resolve({ data: { authors: [] }, error: null });
        feedReads += 1;
        if (feedReads === 1)
          return Promise.resolve({
            data: { items: [initial], capped: false, event_cursor: initial.cursor },
            error: null,
          });
        if (feedReads === 2) return pendingPoll.promise;
        if (feedReads === 3) {
          return Promise.resolve({
            error: {
              code: "serviceUnavailable",
              detail: "activity temporarily unavailable",
              title: "Service unavailable",
              type: "about:blank",
            },
            response: new Response(null, { status: 503 }),
          });
        }
        if (feedReads === 4) {
          return Promise.resolve({
            data: { items: [replacement], capped: false, event_cursor: replacement.cursor },
            error: null,
          });
        }
        throw new Error(`unexpected activity request ${feedReads}`);
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([initial.id]));

    store.startActivityPolling();
    await vi.waitFor(() => expect(feedReads).toBe(2));
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityError()).toBe("activity temporarily unavailable"));

    pendingPoll.resolve({ data: { items: [], capped: true, event_cursor: initial.cursor }, error: null });

    await vi.waitFor(() => expect(store.getActivityItems()).toEqual([replacement]));
    expect(store.getActivityError()).toBeNull();
  });

  it("refreshes author candidates when polling appends new activity", async () => {
    let authors = ["Alice"];
    let feedReads = 0;
    const now = new Date().toISOString();
    const initial = { ...notificationItem("ntf:1", "unread"), author: "Alice", created_at: now };
    const fresh = { ...notificationItem("ntf:2", "unread"), author: "FreshActor", created_at: now };
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors }, error: null };
        feedReads += 1;
        if (feedReads === 1)
          return { data: { items: [initial], capped: false, event_cursor: initial.cursor }, error: null };
        return { data: { items: [fresh], capped: false, event_cursor: fresh.cursor }, error: null };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:1"]));
    await vi.waitFor(() => expect(store.getActivityAuthors()).toEqual(["Alice"]));

    authors = ["FreshActor", "Alice"];
    store.startActivityPolling();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:2", "ntf:1"]));
    await vi.waitFor(() => expect(store.getActivityAuthors()).toEqual(["FreshActor", "Alice"]));
  });

  it("does not project a poll started before a newer foreground search", async () => {
    const pendingPoll = Promise.withResolvers<{
      data: { items: ActivityItem[]; capped: boolean };
      error: null;
    }>();
    const pollReturned = Promise.withResolvers<void>();
    const now = new Date().toISOString();
    const initial = { ...notificationItem("ntf:1", "unread"), created_at: now };
    const stalePollItem = { ...notificationItem("ntf:2", "unread"), created_at: now };
    const foregroundItem = { ...notificationItem("ntf:3", "unread"), created_at: now };
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1)
          return { data: { items: [initial], capped: false, event_cursor: initial.cursor }, error: null };
        if (feedReads === 2) {
          const response = await pendingPoll.promise;
          pollReturned.resolve();
          return response;
        }
        if (feedReads === 3)
          return { data: { items: [foregroundItem], capped: false, event_cursor: foregroundItem.cursor }, error: null };
        throw new Error(`unexpected activity request ${feedReads}`);
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:1"]));

    store.startActivityPolling();
    await vi.waitFor(() => expect(feedReads).toBe(2));
    store.setActivitySearch("new selection");
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:3"]));

    pendingPoll.resolve({
      data: { items: [stalePollItem], capped: false, event_cursor: stalePollItem.cursor },
      error: null,
    });
    await pollReturned.promise;
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(store.getActivityItems().map((item) => item.id)).toEqual(["ntf:3"]);
  });

  it("clears loading after an empty-feed poll reload fails", async () => {
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1) return { data: { items: [], capped: false } };
        return {
          error: {
            code: "validationError",
            detail: "activity unavailable",
            title: "Invalid request",
            type: "about:blank",
          },
          response: new Response(null, { status: 400 }),
        };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.startActivityPolling();
    await vi.waitFor(() => expect(feedReads).toBe(2));

    expect(store.isActivityLoading()).toBe(false);
  });

  it("clears loading after a capped poll reload fails", async () => {
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1)
          return { data: { items: [notificationItem("ntf:42", "unread")], capped: false, event_cursor: "ntf:42" } };
        if (feedReads === 2) return { data: { items: [], capped: true, event_cursor: "ntf:42" } };
        return {
          error: {
            code: "validationError",
            detail: "activity unavailable",
            title: "Invalid request",
            type: "about:blank",
          },
          response: new Response(null, { status: 400 }),
        };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.isActivityLoading()).toBe(false));

    store.startActivityPolling();
    await vi.waitFor(() => expect(feedReads).toBe(3));

    expect(store.isActivityLoading()).toBe(false);
  });

  it("drops cached events whose parents are absent from an uncapped authoritative snapshot", async () => {
    const initial = {
      ...notificationItem("ntf:42", "unread"),
      created_at: new Date().toISOString(),
      repo: {
        provider: "github",
        platform_host: "github.com",
        platform_repo_id: "repo-1",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
      },
    } as ActivityItem;
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1) {
          return {
            data: {
              items: [initial],
              item_activity: [
                {
                  ...itemActivity(1),
                  repo: {
                    provider: "github",
                    platform_host: "github.com",
                    platform_repo_id: "repo-1",
                    owner: "acme",
                    name: "widgets",
                    repo_path: "acme/widgets",
                  },
                },
              ],
              item_activity_capped: false,
              capped: false,
              event_cursor: initial.cursor,
            },
            error: null,
          };
        }
        return {
          data: {
            items: [],
            item_activity: [],
            item_activity_capped: false,
            capped: false,
            event_cursor: initial.cursor,
          },
          error: null,
        };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([initial.id]));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    expect(store.getActivityItems()).toEqual([]);
  });

  it("retains event rows when an authoritative parent snapshot is capped", async () => {
    const initial = { ...notificationItem("ntf:42", "unread"), created_at: new Date().toISOString() };
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1) {
          return {
            data: { items: [initial], capped: false, item_activity_capped: false, event_cursor: initial.cursor },
            error: null,
          };
        }
        if (feedReads === 2) {
          return {
            data: {
              items: [],
              capped: false,
              item_activity: [itemActivity(7)],
              item_activity_capped: true,
              event_cursor: initial.cursor,
            },
            error: null,
          };
        }
        throw new Error(`unexpected activity request ${feedReads}`);
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems()).toEqual([initial]));

    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;
    await vi.waitFor(() => expect(store.isItemActivityCapped()).toBe(true));

    expect(feedReads).toBe(2);
    expect(store.isActivityCapped()).toBe(false);
    expect(store.getActivityItems()).toEqual([initial]);
  });

  it("drops absent cached threads from a capped snapshot when a parent filter is active", async () => {
    const repo = {
      provider: "github",
      platform_host: "github.com",
      platform_repo_id: "repo-1",
      owner: "acme",
      name: "widgets",
      repo_path: "acme/widgets",
    };
    const initial = {
      ...notificationItem("ntf:filtered-thread", "unread"),
      created_at: new Date().toISOString(),
      repo,
    } as ActivityItem;
    const subject = { ...itemActivity(1), repo } as ActivitySubject;
    let feedReads = 0;
    const client = {
      GET: vi.fn(async (path: string) => {
        if (path === "/activity/authors") return { data: { authors: [] }, error: null };
        feedReads += 1;
        if (feedReads === 1) {
          return {
            data: {
              items: [initial],
              item_activity: [subject],
              item_activity_capped: false,
              capped: false,
              event_cursor: initial.cursor,
            },
            error: null,
          };
        }
        return {
          data: {
            items: [],
            item_activity: [],
            item_activity_capped: true,
            capped: false,
            event_cursor: initial.cursor,
          },
          error: null,
        };
      }),
    } as unknown as GeneratedClient;
    const store = createActivityStore({ client });
    store.loadActivity();
    await vi.waitFor(() => expect(store.getActivityItems().map((item) => item.id)).toEqual([initial.id]));

    store.setActivitySearch("current match");
    if (runtime === undefined) throw new Error("test runtime was not created");
    await runtime.runCommand(store.reconcileActivityEffect(), {
      operation: "reconcile capped filtered activity in test",
      safeContext: {},
      onFailure: () => {},
    }).exit;

    expect(store.isItemActivityCapped()).toBe(true);
    expect(store.getActivityItems()).toEqual([]);
  });
});
