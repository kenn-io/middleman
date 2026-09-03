import { Deferred, Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { ProblemCodes, type ProblemBody } from "../api/problems.js";
import type { PullDetail } from "../api/types.js";
import type { OwnedAppRuntime } from "../app/runtime.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { GeneratedProblemResponse } from "../api/runtime.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import type { ApplySuggestionRequest } from "../utils/markdown-suggestions.js";
import {
  type ApplySuggestionConflict,
  createDetailStore as createRuntimeDetailStore,
  type DetailRequestOptions,
  type DetailStore,
  type DetailStoreOptions,
} from "./detail.svelte.js";
import { dismissFlash, getFlash, getFlashes } from "./flash.svelte.js";

let runtime: OwnedAppRuntime | undefined;

type TestDetailStoreOptions = Omit<DetailStoreOptions, "runtime"> & { readonly client: GeneratedClient };

function createDetailStore(options: TestDetailStoreOptions) {
  const { client, ...storeOptions } = options;
  runtime = makeTestAppRuntime(client);
  return createRuntimeDetailStore({ ...storeOptions, runtime });
}

async function loadDetail(store: DetailStore, ...args: Parameters<DetailStore["loadDetail"]>): Promise<void> {
  store.loadDetail(...args);
  await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
}

function refreshDetail(
  store: DetailStore,
  owner: string,
  name: string,
  number: number,
  identity: DetailRequestOptions,
): Promise<void> {
  const settled = Promise.withResolvers<void>();
  const result = store.refreshDetailOnly(owner, name, number, identity, {
    onSettled: settled.resolve,
  });
  expect(result).toBeUndefined();
  return settled.promise;
}

function syncDetail(
  store: DetailStore,
  owner: string,
  name: string,
  number: number,
  identity: DetailRequestOptions,
): Promise<boolean> {
  const settled = Promise.withResolvers<boolean>();
  const result = store.syncDetailNow(owner, name, number, identity, {
    onSuccess: settled.resolve,
    onFailure: () => settled.resolve(false),
  });
  expect(result).toBeUndefined();
  return settled.promise;
}

function applyReviewSuggestions(
  store: DetailStore,
  owner: string,
  name: string,
  number: number,
  input: ApplySuggestionRequest,
  onConflict?: (conflict: ApplySuggestionConflict) => void,
): Promise<boolean> {
  const settled = Promise.withResolvers<boolean>();
  const result = store.applyReviewSuggestions(
    {
      provider: "github",
      platformHost: "github.com",
      owner,
      name,
      repoPath: `${owner}/${name}`,
    },
    number,
    input,
    {
      ...(onConflict !== undefined && { onConflict }),
      onResult: settled.resolve,
    },
  );
  expect(result).toBeUndefined();
  return settled.promise;
}

function pullDetail(headSHA: string): PullDetail {
  return {
    merge_request: {
      Number: 7,
      State: "open",
      IsDraft: false,
      MergeableState: "",
      platform_head_sha: headSHA,
    },
    platform_head_sha: headSHA,
    reviewed_head_sha: headSHA,
    repo: {
      provider: "github",
      platform_host: "github.com",
      repo_path: "acme/widget",
    },
    events: [],
    detail_loaded: true,
    repo_owner: "acme",
    repo_name: "widget",
  } as unknown as PullDetail;
}

function pullDetailFor(name: string, number: number, headSHA: string): PullDetail {
  const result = pullDetail(headSHA);
  result.repo_name = name;
  result.repo.repo_path = `acme/${name}`;
  result.merge_request.Number = number;
  return result;
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function conflictProblem(reason: string): ProblemBody {
  return {
    code: ProblemCodes.conflict,
    type: "about:blank",
    title: "Conflict",
    detail: "pull request state changed",
    details: { reason },
  };
}

function mockClient(overrides: Partial<GeneratedClient> = {}): GeneratedClient {
  return {
    GET: vi.fn(),
    POST: vi.fn(),
    PUT: vi.fn(),
    PATCH: vi.fn(),
    DELETE: vi.fn(),
    OPTIONS: vi.fn(),
    HEAD: vi.fn(),
    TRACE: vi.fn(),
    ...overrides,
  } as unknown as GeneratedClient;
}

describe("createDetailStore", () => {
  beforeEach(() => {
    runtime = undefined;
  });

  afterEach(async () => {
    for (const item of getFlashes()) dismissFlash(item.id);
    localStorage.clear();
    vi.restoreAllMocks();
    vi.useRealTimers();
    if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
  });

  it("recovers a missing cached pull with one targeted synchronization", async () => {
    const get = vi.fn().mockResolvedValue({
      error: {
        code: ProblemCodes.pullNotFound,
        type: "about:blank",
        title: "Not Found",
        detail: "pull request not found",
      },
    });
    const post = vi.fn().mockResolvedValue({ data: pullDetail("fresh-head") });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });

    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: "background",
    });

    expect(store.getDetail()?.platform_head_sha).toBe("fresh-head");
    expect(store.getDetailError()).toBeNull();
    expect(post).toHaveBeenCalledOnce();
    expect(post).toHaveBeenCalledWith("/pulls/{provider}/{owner}/{name}/{number}/sync", {
      params: {
        path: {
          provider: "github",
          owner: "acme",
          name: "widget",
          number: 7,
        },
      },
      signal: expect.any(AbortSignal),
    });
  });

  it("explains how to recover when the repository is unavailable", async () => {
    const store = createDetailStore({
      client: mockClient({
        GET: vi.fn().mockResolvedValue({
          error: {
            code: ProblemCodes.repoNotFound,
            type: "about:blank",
            title: "Not Found",
            detail: "repo not found",
          },
        }),
      }),
    });

    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: "background",
    });

    expect(store.getDetail()).toBeNull();
    expect(store.getDetailError()).toBe(
      "This repository is not available in Forge. Add it under Settings → Repositories, then retry.",
    );
  });

  it("keeps the displayed detail object when a refresh returns identical content", async () => {
    const routeIdentity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    };
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:00:00Z" } })
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:01:00Z" } })
      .mockResolvedValueOnce({ data: { ...pullDetail("new-head"), detail_fetched_at: "2026-07-15T10:02:00Z" } });
    const store = createDetailStore({
      client: mockClient({ GET: get }),
      getPage: () => "pulls",
    });
    await loadDetail(store, "acme", "widget", 7, { ...routeIdentity, sync: false });
    const displayed = store.getDetail();

    // Only the sync timestamp moved: the polling refresh must not swap
    // in an equal-but-new object, or the PR panel re-renders every cycle.
    await refreshDetail(store, "acme", "widget", 7, routeIdentity);
    expect(store.getDetail()).toBe(displayed);
    expect(store.getDetail()?.detail_fetched_at).toBe("2026-07-15T10:00:00Z");

    // Real content changes still apply.
    await refreshDetail(store, "acme", "widget", 7, routeIdentity);
    expect(store.getDetail()).not.toBe(displayed);
    expect(store.getDetail()?.platform_head_sha).toBe("new-head");
  });

  it("rejects an old selection refresh that resolves after the new selection", async () => {
    const loadB = deferred<{ data: PullDetail }>();
    const refreshA = deferred<{ data: PullDetail }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: pullDetailFor("widget-a", 7, "head-a") })
      .mockReturnValueOnce(loadB.promise)
      .mockReturnValueOnce(refreshA.promise);
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    const identity = (name: string) => ({
      provider: "github",
      platformHost: "github.com",
      repoPath: `acme/${name}`,
      sync: false as const,
    });
    await loadDetail(store, "acme", "widget-a", 7, identity("widget-a"));

    store.loadDetail("acme", "widget-b", 8, identity("widget-b"));
    const refreshingA = refreshDetail(store, "acme", "widget-a", 7, identity("widget-a"));
    loadB.resolve({ data: pullDetailFor("widget-b", 8, "head-b") });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    refreshA.resolve({ data: pullDetailFor("widget-a", 7, "late-head-a") });
    await refreshingA;

    expect(store.getDetail()?.repo_name).toBe("widget-b");
    expect(store.getDetail()?.merge_request.Number).toBe(8);
    expect(store.getDetail()?.platform_head_sha).toBe("head-b");
  });

  it("aborts the previous detail request when the selection changes", async () => {
    let firstSignal: AbortSignal | undefined;
    const firstStarted = deferred<void>();
    const get = vi.fn((_path: string, request: { signal?: AbortSignal; params: { path: { number: number } } }) => {
      if (request.params.path.number === 7) {
        firstSignal = request.signal;
        firstStarted.resolve();
        return new Promise<never>(() => {});
      }
      return Promise.resolve({ data: pullDetailFor("widget-b", 8, "head-b") });
    });
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    const identity = (name: string) => ({
      provider: "github",
      platformHost: "github.com",
      repoPath: `acme/${name}`,
      sync: false as const,
    });

    store.loadDetail("acme", "widget-a", 7, identity("widget-a"));
    await firstStarted.promise;
    store.loadDetail("acme", "widget-b", 8, identity("widget-b"));

    await vi.waitFor(() => expect(firstSignal?.aborted).toBe(true));
    await vi.waitFor(() => expect(store.getDetail()?.repo_name).toBe("widget-b"));
  });

  it("rejects an initial load that resolves after a newer refresh for the same selection", async () => {
    const initialLoad = deferred<{ data: PullDetail }>();
    const newerRefresh = deferred<{ data: PullDetail }>();
    const get = vi.fn().mockReturnValueOnce(initialLoad.promise).mockReturnValueOnce(newerRefresh.promise);
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    const identity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false as const,
    };

    store.loadDetail("acme", "widget", 7, identity);
    const refreshing = refreshDetail(store, "acme", "widget", 7, identity);
    newerRefresh.resolve({ data: pullDetail("newer-head") });
    await refreshing;
    initialLoad.resolve({ data: pullDetail("older-head") });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));

    expect(store.getDetail()?.platform_head_sha).toBe("newer-head");
  });

  it("an older-started sync response cannot overwrite a newer envelope", async () => {
    // The sync path has no per-selection sequence guard; without atomic
    // payload+tick application its stale response would replace newer
    // detail while the newer tick stands — letting pre-creation "no
    // workspace" data masquerade as an authoritative post-create absence.
    const routeIdentity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    };
    const withWorkspace = { ...pullDetail("head"), workspace: { id: "ws-1", status: "ready" } };
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: pullDetail("head") })
      .mockResolvedValueOnce({ data: withWorkspace });
    const syncPost = deferred<{ data: PullDetail }>();
    const post = vi.fn().mockReturnValueOnce(syncPost.promise);
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    await loadDetail(store, "acme", "widget", 7, { ...routeIdentity, sync: false });

    // The sync's request starts before the refresh below applies a newer
    // envelope carrying the workspace.
    const syncing = syncDetail(store, "acme", "widget", 7, routeIdentity);
    await refreshDetail(store, "acme", "widget", 7, routeIdentity);
    expect(store.getDetail()?.workspace?.id).toBe("ws-1");
    const newerTick = store.getDetailEnvelopeTick();

    syncPost.resolve({ data: pullDetail("head") });
    await syncing;

    expect(store.getDetail()?.workspace?.id).toBe("ws-1");
    expect(store.getDetailEnvelopeTick()).toBe(newerTick);
  });

  it("applies a pending initial load when a newer refresh fails for the same selection", async () => {
    const initialLoad = deferred<{ data?: PullDetail; error?: ProblemBody }>();
    const newerRefresh = deferred<{ data?: PullDetail; error?: ProblemBody }>();
    const get = vi.fn().mockReturnValueOnce(initialLoad.promise).mockReturnValueOnce(newerRefresh.promise);
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    const identity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false as const,
    };

    store.loadDetail("acme", "widget", 7, identity);
    const refreshing = refreshDetail(store, "acme", "widget", 7, identity);
    newerRefresh.resolve({ error: conflictProblem("detail_refresh_failed") });
    await refreshing;
    initialLoad.resolve({ data: pullDetail("loaded-head") });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));

    expect(store.getDetail()?.platform_head_sha).toBe("loaded-head");
  });

  it.each([
    {
      loaded: { provider: "gh", platformHost: undefined },
      refreshed: { provider: "github", platformHost: "github.com" },
    },
    {
      loaded: { provider: "github", platformHost: "github.com" },
      refreshed: { provider: "gh", platformHost: undefined },
    },
  ])("treats provider aliases and omitted default hosts as the same selection", async ({ loaded, refreshed }) => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: pullDetail("head") })
      .mockResolvedValueOnce({ data: pullDetail("refreshed-head") });
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    const identity = { repoPath: "acme/widget", sync: false as const };
    await loadDetail(store, "acme", "widget", 7, { ...identity, ...loaded });

    await refreshDetail(store, "acme", "widget", 7, {
      repoPath: identity.repoPath,
      ...refreshed,
    });

    expect(store.getDetail()?.platform_head_sha).toBe("refreshed-head");
  });

  it("applies a warnings-only change since the panel renders warnings", async () => {
    const routeIdentity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    };
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:00:00Z" } })
      .mockResolvedValueOnce({
        data: { ...pullDetail("head"), warnings: ["diff unavailable"], detail_fetched_at: "2026-07-15T10:01:00Z" },
      })
      .mockResolvedValueOnce({
        data: { ...pullDetail("head"), warnings: ["diff unavailable"], detail_fetched_at: "2026-07-15T10:02:00Z" },
      });
    const store = createDetailStore({
      client: mockClient({ GET: get }),
      getPage: () => "pulls",
    });
    await loadDetail(store, "acme", "widget", 7, { ...routeIdentity, sync: false });

    await refreshDetail(store, "acme", "widget", 7, routeIdentity);
    expect((store.getDetail() as { warnings?: string[] } | null)?.warnings).toEqual(["diff unavailable"]);

    // The same warnings again are not a change.
    const displayed = store.getDetail();
    await refreshDetail(store, "acme", "widget", 7, routeIdentity);
    expect(store.getDetail()).toBe(displayed);
  });

  it("baselines polling convergence on the latest observed timestamp, not the frozen store one", async () => {
    vi.useFakeTimers();
    const routeIdentity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    };
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:00:00Z" } })
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:01:00Z" } })
      .mockResolvedValueOnce({ data: { ...pullDetail("head"), detail_fetched_at: "2026-07-15T10:01:00Z" } })
      .mockResolvedValue({ data: { ...pullDetail("new-head"), detail_fetched_at: "2026-07-15T10:02:00Z" } });
    const post = vi.fn().mockResolvedValue({ data: undefined, error: undefined });
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: post }),
      getPage: () => "pulls",
    });
    await loadDetail(store, "acme", "widget", 7, { ...routeIdentity, sync: false });
    // Content-identical refresh: the store timestamp freezes at 10:00
    // while the server clock is at 10:01.
    await refreshDetail(store, "acme", "widget", 7, routeIdentity);

    // The next polling cycle's first re-GET still returns 10:01. Judged
    // against the frozen store timestamp that would look like completion
    // and drop the real change; against the observed baseline the loop
    // keeps going and applies the new head from the finished sync.
    store.startDetailPolling("acme", "widget", 7, routeIdentity);
    await vi.advanceTimersByTimeAsync(60_000);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(300);
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(3));
    await vi.advanceTimersByTimeAsync(700);
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(4));

    expect(store.getDetail()?.platform_head_sha).toBe("new-head");
    store.stopDetailPolling();
  });

  it("flashes failed optimistic state changes without poisoning detail load state", async () => {
    const optimisticKanbanUpdate = vi.fn();
    const store = createDetailStore({
      client: mockClient({
        GET: vi.fn().mockResolvedValue({ data: pullDetail("head") }),
        PUT: vi.fn().mockResolvedValue({ error: { detail: "permission denied" } }),
      }),
      getPage: () => "pulls",
      pulls: {
        loadPulls: vi.fn().mockResolvedValue(undefined),
        getPullKanbanStatus: vi.fn(() => "new"),
        optimisticKanbanUpdate,
      },
    });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    const result = store.updateKanbanState(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      7,
      "reviewing",
    );
    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(getFlash()?.message).toBe("permission denied"));

    expect(getFlash()).toMatchObject({ message: "permission denied", tone: "danger" });
    expect(store.getDetailError()).toBeNull();
    expect(optimisticKanbanUpdate).toHaveBeenLastCalledWith(expect.anything(), 7, "new");
  });

  it("launches pull content updates synchronously and settles from the canonical response", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Title = "Old title";
    initial.merge_request.Body = "Old body";
    const canonical = pullDetail("head");
    canonical.merge_request.Title = "Canonical title";
    canonical.merge_request.Body = "Old body";
    const store = createDetailStore({
      client: mockClient({
        GET: vi.fn().mockResolvedValue({ data: initial }),
        PATCH: vi.fn().mockResolvedValue({ data: canonical, error: undefined }),
      }),
    });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    const settled = Promise.withResolvers<void>();
    const onSuccess = vi.fn();

    const result = store.updatePRContent(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      7,
      { title: "Draft title" },
      { onSuccess, onSettled: settled.resolve },
    );

    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(store.getDetail()?.merge_request.Title).toBe("Draft title"));
    await settled.promise;
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(store.getDetail()?.merge_request.Title).toBe("Canonical title");
  });

  it("launches star updates synchronously and restores the confirmed value on failure", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Starred = false;
    const store = createDetailStore({
      client: mockClient({
        GET: vi.fn().mockResolvedValue({ data: initial }),
        PUT: vi.fn().mockResolvedValue({ error: { detail: "could not star" } }),
      }),
    });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    const result = store.toggleDetailPRStar(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      7,
      false,
    );

    expect(result).toBeUndefined();
    await vi.waitFor(() => expect(getFlash()?.message).toBe("could not star"));
    expect(store.getDetail()?.merge_request.Starred).toBe(false);
  });

  it("rebases pending content, kanban, and star mutations over a refreshed envelope", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Title = "Old title";
    initial.merge_request.Body = "Old body";
    initial.merge_request.KanbanStatus = "new";
    initial.merge_request.Starred = false;
    const refreshed = pullDetail("head");
    refreshed.merge_request.Title = "Server title";
    refreshed.merge_request.Body = "Server body";
    refreshed.merge_request.KanbanStatus = "new";
    refreshed.merge_request.Starred = false;
    const contentCommit = deferred<{ data: PullDetail; error: undefined }>();
    const kanbanCommit = deferred<{ data: undefined; error: undefined }>();
    const starCommit = deferred<{ data: undefined; error: undefined }>();
    const get = vi.fn().mockResolvedValueOnce({ data: initial }).mockResolvedValue({ data: refreshed });
    const put = vi.fn((path: string) => (path === "/starred" ? starCommit.promise : kanbanCommit.promise));
    const pulls = {
      loadPulls: vi.fn(),
      getPullKanbanStatus: vi.fn(() => "new" as const),
      optimisticKanbanUpdate: vi.fn(),
    };
    const store = createDetailStore({
      client: mockClient({ GET: get, PATCH: vi.fn(() => contentCommit.promise), PUT: put }),
      pulls,
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.updatePRContent(routeRef, 7, { title: "Draft title" });
    store.updateKanbanState(routeRef, 7, "reviewing");
    store.toggleDetailPRStar(routeRef, 7, false);
    await vi.waitFor(() => expect(store.getDetail()?.merge_request.Title).toBe("Draft title"));
    await vi.waitFor(() => expect(store.getDetail()?.merge_request.KanbanStatus).toBe("reviewing"));
    await vi.waitFor(() => expect(store.getDetail()?.merge_request.Starred).toBe(true));

    await refreshDetail(store, "acme", "widget", 7, routeRef);

    expect(store.getDetail()?.merge_request.Title).toBe("Draft title");
    expect(store.getDetail()?.merge_request.KanbanStatus).toBe("reviewing");
    expect(store.getDetail()?.merge_request.Starred).toBe(true);
    const confirmedContent = pullDetail("head");
    confirmedContent.merge_request.Title = "Draft title";
    confirmedContent.merge_request.Body = "Old body";
    contentCommit.resolve({ data: confirmedContent, error: undefined });
    kanbanCommit.resolve({ data: undefined, error: undefined });
    starCommit.resolve({ data: undefined, error: undefined });
  });

  it("does not let an older refresh overwrite a settled star mutation", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Starred = false;
    const stale = pullDetail("head");
    stale.merge_request.Starred = false;
    const refresh = deferred<{ data: PullDetail }>();
    const get = vi.fn().mockResolvedValueOnce({ data: initial }).mockReturnValueOnce(refresh.promise);
    const store = createDetailStore({
      client: mockClient({
        GET: get,
        PUT: vi.fn().mockResolvedValue({ data: undefined, error: undefined }),
      }),
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });
    const refreshing = refreshDetail(store, "acme", "widget", 7, routeRef);
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));

    store.toggleDetailPRStar(routeRef, 7, false);
    await vi.waitFor(() => expect(store.getDetail()?.merge_request.Starred).toBe(true));
    refresh.resolve({ data: stale });
    await refreshing;

    expect(store.getDetail()?.merge_request.Starred).toBe(true);
  });

  it("acknowledges pull state changes through a synchronous store action", async () => {
    const store = createDetailStore({
      client: mockClient({
        GET: vi.fn().mockResolvedValue({ data: pullDetail("head") }),
        POST: vi.fn().mockResolvedValue({ data: undefined, error: undefined }),
      }),
    });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    const settled = Promise.withResolvers<void>();
    const onSuccess = vi.fn();

    const result = store.setPullState(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      7,
      "closed",
      { onSuccess, onSettled: settled.resolve },
    );

    expect(result).toBeUndefined();
    await settled.promise;
    expect(onSuccess).toHaveBeenCalledTimes(1);
  });

  it("refreshes the pull list when state changed even if detail reconciliation fails", async () => {
    const pulls = { loadPulls: vi.fn() };
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: pullDetail("head") })
      .mockRejectedValueOnce(new Error("detail unavailable"));
    const store = createDetailStore({
      client: mockClient({
        GET: get,
        POST: vi.fn().mockResolvedValue({ data: undefined, error: undefined }),
      }),
      getPage: () => "pulls",
      pulls,
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.setPullState(routeRef, 7, "closed");

    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(pulls.loadPulls).toHaveBeenCalledTimes(2));
  });

  it("syncs detail and resolves after applying the refreshed head", async () => {
    const syncPull = vi.fn().mockResolvedValue(pullDetail("fresh-head"));
    const pulls = { loadPulls: vi.fn().mockResolvedValue(undefined) };
    const store = createDetailStore({
      client: makeGeneratedClient({ PullRequestsService: { syncPull } }),
      getPage: () => "pulls",
      pulls,
    });

    const refreshed = await syncDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    expect(refreshed).toBe(true);
    expect(store.getDetail()?.platform_head_sha).toBe("fresh-head");
    expect(pulls.loadPulls).toHaveBeenCalledTimes(1);
  });

  it("reconciles visible lists after the initial detail read exposes persisted activity", async () => {
    const loadPulls = vi.fn();
    const onDetailSynchronized = vi.fn();
    const store = createDetailStore({
      client: mockClient({ GET: vi.fn().mockResolvedValue({ data: pullDetail("persisted-head") }) }),
      getPage: () => "pulls",
      pulls: { loadPulls },
      onDetailSynchronized,
    });

    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    expect(loadPulls).toHaveBeenCalledOnce();
    expect(onDetailSynchronized).toHaveBeenCalledOnce();
  });

  it("reconciles a successful initial read after its presentation generation is superseded", async () => {
    const detailRead = Promise.withResolvers<{ data: PullDetail }>();
    const get = vi.fn(() => detailRead.promise);
    const post = vi.fn(() => new Promise<never>(() => {}));
    const loadPulls = vi.fn();
    const onDetailSynchronized = vi.fn();
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: post }),
      getPage: () => "pulls",
      pulls: { loadPulls },
      onDetailSynchronized,
    });

    store.loadDetail("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    await vi.waitFor(() => expect(get).toHaveBeenCalledOnce());

    detailRead.resolve({ data: pullDetail("persisted-head") });
    store.syncDetailNow("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    await vi.waitFor(() => expect(onDetailSynchronized).toHaveBeenCalledOnce());
    expect(post).toHaveBeenCalledOnce();
    expect(loadPulls).toHaveBeenCalledOnce();
    expect(store.getDetail()).toBeNull();
  });

  it("skips a scheduled poll while an explicit detail sync is pending", async () => {
    vi.useFakeTimers();
    const syncResponse = Promise.withResolvers<{ data: PullDetail; error: undefined }>();
    const post = vi.fn((path: string) =>
      path.endsWith("/sync") ? syncResponse.promise : Promise.resolve({ data: undefined, error: undefined }),
    );
    const store = createDetailStore({ client: mockClient({ POST: post }) });
    store.startDetailPolling("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    const syncing = syncDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    await vi.waitFor(() => expect(post).toHaveBeenCalledOnce());
    await vi.advanceTimersByTimeAsync(60_000);

    expect(post).toHaveBeenCalledTimes(1);
    syncResponse.resolve({ data: pullDetail("fresh-head"), error: undefined });
    await expect(syncing).resolves.toBe(true);
    expect(store.getDetail()?.platform_head_sha).toBe("fresh-head");
    await vi.advanceTimersByTimeAsync(60_000);
    expect(post).toHaveBeenCalledTimes(2);
    store.stopDetailPolling();
  });

  it("skips sync-completion refreshes while an explicit detail sync is pending", async () => {
    const syncResponse = Promise.withResolvers<{ data: PullDetail; error: undefined }>();
    const get = vi.fn().mockResolvedValue({ data: pullDetail("cached-head"), error: undefined });
    const post = vi.fn(() => syncResponse.promise);
    let notifySyncComplete: (() => void) | undefined;
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: post }),
      sync: {
        subscribeSyncComplete: (callback) => {
          notifySyncComplete = callback;
          return vi.fn();
        },
      },
    });
    store.startDetailPolling("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    const syncing = syncDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    await vi.waitFor(() => expect(store.isDetailSyncing()).toBe(true));
    notifySyncComplete?.();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(get).not.toHaveBeenCalled();
    syncResponse.resolve({ data: pullDetail("fresh-head"), error: undefined });
    await expect(syncing).resolves.toBe(true);
    expect(store.getDetail()?.platform_head_sha).toBe("fresh-head");
    store.stopDetailPolling();
  });

  it("skips pending CI refreshes while an explicit detail sync is pending", async () => {
    const syncResponse = Promise.withResolvers<{ data: PullDetail; error: undefined }>();
    const get = vi.fn().mockResolvedValue({ data: pullDetail("cached-head"), error: undefined });
    const post = vi.fn((path: string) =>
      path.endsWith("/sync")
        ? syncResponse.promise
        : Promise.resolve({ data: pullDetail("cached-head"), error: undefined }),
    );
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    const routeIdentity = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeIdentity, sync: false });

    const syncing = syncDetail(store, "acme", "widget", 7, routeIdentity);
    await vi.waitFor(() => expect(store.isDetailSyncing()).toBe(true));
    const ciSettled = vi.fn();
    store.refreshPendingCI("acme", "widget", 7, routeIdentity, { onSettled: ciSettled });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(post).toHaveBeenCalledOnce();
    expect(ciSettled).toHaveBeenCalledOnce();
    syncResponse.resolve({ data: pullDetail("fresh-head"), error: undefined });
    await expect(syncing).resolves.toBe(true);
    expect(store.getDetail()?.platform_head_sha).toBe("fresh-head");
  });

  it("reconciles visible lists when selection closes during an explicit detail sync", async () => {
    const syncPost = Promise.withResolvers<{ data: PullDetail; error: undefined }>();
    const post = vi.fn(() => syncPost.promise);
    const loadPulls = vi.fn();
    const onDetailSynchronized = vi.fn();
    const store = createDetailStore({
      client: mockClient({ POST: post }),
      getPage: () => "pulls",
      pulls: { loadPulls },
      onDetailSynchronized,
    });

    const syncing = syncDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    await vi.waitFor(() => expect(post).toHaveBeenCalledOnce());
    store.clearDetail();

    syncPost.resolve({ data: pullDetail("fresh-head"), error: undefined });

    await expect(syncing).resolves.toBe(false);
    expect(loadPulls).toHaveBeenCalledOnce();
    expect(onDetailSynchronized).toHaveBeenCalledOnce();
    expect(store.getDetail()).toBeNull();
  });

  it("reports when an explicit detail sync cannot refresh state", async () => {
    const store = createDetailStore({
      client: mockClient({
        POST: vi.fn().mockResolvedValue({ error: { detail: "provider unavailable" } }),
      }),
    });

    const refreshed = await syncDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    expect(refreshed).toBe(false);
    expect(store.getDetail()).toBeNull();
  });

  it("enqueues background sync when active detail polling fires", async () => {
    vi.useFakeTimers();
    const post = vi.fn().mockResolvedValue({ data: undefined, error: undefined });
    const get = vi.fn().mockResolvedValue({ data: pullDetail("cached-head") });
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: post }),
    });

    store.startDetailPolling("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });

    await vi.advanceTimersByTimeAsync(60_000);

    expect(post).toHaveBeenCalledWith("/pulls/{provider}/{owner}/{name}/{number}/sync/async", {
      params: {
        path: {
          provider: "github",
          owner: "acme",
          name: "widget",
          number: 7,
        },
      },
      signal: expect.any(AbortSignal),
    });
  });

  it("reconciles visible activity and pull lists after a background detail sync converges", async () => {
    vi.useFakeTimers();
    const cached = pullDetail("cached-head");
    cached.detail_fetched_at = "2026-08-12T21:00:00Z";
    const fresh = pullDetail("fresh-head");
    fresh.detail_fetched_at = "2026-08-12T21:03:00Z";
    const get = vi.fn().mockResolvedValueOnce({ data: cached }).mockResolvedValue({ data: fresh });
    const loadPulls = vi.fn();
    const onDetailSynchronized = vi.fn();
    const store = createDetailStore({
      client: mockClient({
        GET: get,
        POST: vi.fn().mockResolvedValue({ data: undefined, error: undefined }),
      }),
      getPage: () => "pulls",
      pulls: { loadPulls },
      onDetailSynchronized,
    });

    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: "background",
    });
    await vi.advanceTimersByTimeAsync(300);

    await vi.waitFor(() => expect(store.getDetail()?.merge_request.platform_head_sha).toBe("fresh-head"));
    expect(loadPulls).toHaveBeenCalledTimes(2);
    expect(onDetailSynchronized).toHaveBeenCalledTimes(2);
  });

  it("reconciles visible lists when selection changes during a background detail sync", async () => {
    vi.useFakeTimers();
    const cached = pullDetailFor("widget-a", 7, "cached-head");
    cached.detail_fetched_at = "2026-08-12T21:00:00Z";
    const other = pullDetailFor("widget-b", 8, "other-head");
    const fresh = pullDetailFor("widget-a", 7, "fresh-head");
    fresh.detail_fetched_at = "2026-08-12T21:03:00Z";
    const syncPost = Promise.withResolvers<{ data: undefined; error: undefined }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: cached })
      .mockResolvedValueOnce({ data: other })
      .mockResolvedValueOnce({ data: fresh });
    const loadPulls = vi.fn();
    const onDetailSynchronized = vi.fn();
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: vi.fn(() => syncPost.promise) }),
      getPage: () => "pulls",
      pulls: { loadPulls },
      onDetailSynchronized,
    });

    await loadDetail(store, "acme", "widget-a", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget-a",
      sync: "background",
    });
    await loadDetail(store, "acme", "widget-b", 8, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget-b",
      sync: false,
    });

    syncPost.resolve({ data: undefined, error: undefined });
    await vi.advanceTimersByTimeAsync(300);

    await vi.waitFor(() => expect(onDetailSynchronized).toHaveBeenCalledTimes(3));
    expect(loadPulls).toHaveBeenCalledTimes(3);
    expect(store.getDetail()?.repo_name).toBe("widget-b");
    expect(store.getDetail()?.merge_request.platform_head_sha).toBe("other-head");
  });

  it("does not overlap detail polling iterations", async () => {
    vi.useFakeTimers();
    const firstSync = deferred<{ data: undefined; error: undefined }>();
    const post = vi.fn(() => firstSync.promise);
    const store = createDetailStore({ client: mockClient({ POST: post }) });

    store.startDetailPolling("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    await vi.advanceTimersByTimeAsync(60_000);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(120_000);

    expect(post).toHaveBeenCalledTimes(1);
    store.stopDetailPolling();
    firstSync.resolve({ data: undefined, error: undefined });
  });

  it("continues detail polling after one synchronization failure", async () => {
    vi.useFakeTimers();
    const post = vi
      .fn()
      .mockResolvedValueOnce({ error: conflictProblem("poll_failed") })
      .mockResolvedValue({ data: undefined, error: undefined });
    const store = createDetailStore({ client: mockClient({ POST: post }) });

    store.startDetailPolling("acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
    });
    await vi.advanceTimersByTimeAsync(60_000);
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(60_000);

    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(2));
    store.stopDetailPolling();
  });

  it("awaits a sync-enabled refresh after apply-suggestion success", async () => {
    const get = vi.fn().mockResolvedValue({ data: pullDetail("old-head") });
    const post = vi.fn(async (path: string) => {
      if (path.endsWith("/review-suggestions/apply")) {
        return { data: { status: "applied" }, error: undefined };
      }
      if (path.endsWith("/sync")) {
        return { data: pullDetail("new-head"), error: undefined };
      }
      return { error: undefined };
    });
    const store = createDetailStore({
      client: mockClient({ GET: get, POST: post }),
      getPage: () => "pulls",
      pulls: { loadPulls: vi.fn().mockResolvedValue(undefined) },
    });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    const ok = await applyReviewSuggestions(store, "acme", "widget", 7, {
      suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
    });

    expect(ok).toBe(true);
    expect(store.getDetail()?.platform_head_sha).toBe("new-head");
    expect(post).toHaveBeenCalledWith(
      "/pulls/{provider}/{owner}/{name}/{number}/sync",
      expect.objectContaining({
        params: expect.objectContaining({
          path: expect.objectContaining({ provider: "github", owner: "acme", name: "widget", number: 7 }),
        }),
      }),
    );
  });

  it("shows rate-limit retry timing in local time when applying a suggestion", async () => {
    const toLocaleTimeString = vi.spyOn(Date.prototype, "toLocaleTimeString").mockReturnValue("09:35");
    const get = vi.fn().mockResolvedValue({ data: pullDetail("reviewed-head") });
    const post = vi.fn(async (path: string) => {
      if (path.endsWith("/review-suggestions/apply")) {
        return {
          error: {
            code: ProblemCodes.rateLimited,
            type: "about:blank",
            title: "Too Many Requests",
            detail: "github.com rate-limited",
            details: { retryAfter: "2026-05-19T14:35:00Z" },
          },
        };
      }
      return { error: undefined };
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    const ok = await applyReviewSuggestions(store, "acme", "widget", 7, {
      suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
    });

    expect(ok).toBe(false);
    expect(getFlash()).toMatchObject({
      message: "github.com rate-limited; retry at 09:35",
      tone: "danger",
    });
    expect(toLocaleTimeString).toHaveBeenCalledWith(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
  });

  it("syncs detail before returning false for apply-suggestion state conflicts", async () => {
    for (const reason of ["stale_state", "head_unknown", "not_open", "head_repo_unknown"] as const) {
      const problem = conflictProblem(reason);
      const get = vi.fn().mockResolvedValue({ data: pullDetail("old-head") });
      const post = vi.fn(async (path: string) => {
        if (path.endsWith("/review-suggestions/apply")) {
          return { error: problem };
        }
        if (path.endsWith("/sync")) {
          return { data: pullDetail(`fresh-${reason}`), error: undefined };
        }
        return { error: undefined };
      });
      const store = createDetailStore({
        client: mockClient({ GET: get, POST: post }),
        getPage: () => "pulls",
        pulls: { loadPulls: vi.fn().mockResolvedValue(undefined) },
      });
      await loadDetail(store, "acme", "widget", 7, {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
        sync: false,
      });

      const ok = await applyReviewSuggestions(store, "acme", "widget", 7, {
        suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
      });

      expect(ok).toBe(false);
      expect(store.getDetail()?.platform_head_sha).toBe(`fresh-${reason}`);
      expect(store.getDetailError()).toBe("pull request state changed");
      expect(post).toHaveBeenCalledWith(
        "/pulls/{provider}/{owner}/{name}/{number}/sync",
        expect.objectContaining({
          params: expect.objectContaining({
            path: expect.objectContaining({ provider: "github", owner: "acme", name: "widget", number: 7 }),
          }),
        }),
      );
    }
  });

  it("reports a typed suggestion conflict with the submitted head and route identity", async () => {
    const get = vi.fn().mockResolvedValue({ data: pullDetail("reviewed-head") });
    const post = vi.fn(async (path: string) => {
      if (path.endsWith("/review-suggestions/apply")) {
        return { error: conflictProblem("stale_state") };
      }
      return { error: undefined };
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    const onConflict = vi.fn();

    const ok = await applyReviewSuggestions(
      store,
      "acme",
      "widget",
      7,
      { suggestions: [{ threadID: "thread-1", replacement: "return publish();" }] },
      onConflict,
    );

    expect(ok).toBe(false);
    expect(onConflict).toHaveBeenCalledWith({
      reason: "stale_state",
      context: undefined,
      expectedHeadSha: "reviewed-head",
      ref: {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
        number: 7,
      },
      number: 7,
    });
    expect(post.mock.calls.some(([path]) => String(path).endsWith("/sync"))).toBe(false);
  });

  it("does not retarget a queued suggestion after navigation to a colliding provider identity", async () => {
    const commandGate = Effect.runSync(Deferred.make<void>());
    const get = vi.fn(
      async (_path: string, options: { params: { path: { provider: string; name: string; number: number } } }) => {
        const loaded = pullDetail(options.params.path.provider === "github" ? "github-head" : "gitlab-head");
        loaded.repo.provider = options.params.path.provider;
        loaded.repo.platform_host = options.params.path.provider === "github" ? "github.com" : "gitlab.example.com";
        loaded.repo.repo_path = `acme/${options.params.path.name}`;
        loaded.repo_name = options.params.path.name;
        loaded.merge_request.Number = options.params.path.number;
        return { data: loaded };
      },
    );
    const post = vi.fn().mockResolvedValue({ data: { status: "applied" }, error: undefined });
    const baseRuntime = makeTestAppRuntime(mockClient({ GET: get, POST: post }));
    runtime = {
      disposeEffect: baseRuntime.disposeEffect,
      runCommand: (program, options) =>
        baseRuntime.runCommand(
          options.operation === "apply pull request review suggestions"
            ? Deferred.await(commandGate).pipe(Effect.andThen(program))
            : program,
          options,
        ),
    };
    const store = createRuntimeDetailStore({ runtime });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });
    const settled = Promise.withResolvers<boolean>();

    const result = store.applyReviewSuggestions(
      {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widget",
        repoPath: "acme/widget",
      },
      7,
      { suggestions: [{ threadID: "thread-1", replacement: "return publish();" }] },
      { onResult: settled.resolve },
    );
    expect(result).toBeUndefined();
    await loadDetail(store, "acme", "widget", 7, {
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      repoPath: "acme/widget",
      sync: false,
    });
    await Effect.runPromise(Deferred.succeed(commandGate, undefined));

    await expect(settled.promise).resolves.toBe(false);
    expect(post.mock.calls.some(([path]) => String(path).endsWith("/review-suggestions/apply"))).toBe(false);
    expect(store.getDetail()?.repo.provider).toBe("gitlab");
  });

  it("ignores a delayed suggestion conflict after an A-to-B-to-A route cycle", async () => {
    let resolveApply!: (value: { error: ProblemBody }) => void;
    const applyResponse = new Promise<{ error: ProblemBody }>((resolve) => {
      resolveApply = resolve;
    });
    const get = vi.fn(async (_path: string, options: { params: { path: { name: string; number: number } } }) => {
      const loaded = pullDetail("reviewed-head");
      loaded.repo_name = options.params.path.name;
      loaded.repo.repo_path = `acme/${options.params.path.name}`;
      loaded.merge_request.Number = options.params.path.number;
      return { data: loaded };
    });
    const post = vi.fn((path: string) => {
      if (path.endsWith("/review-suggestions/apply")) return applyResponse;
      return Promise.resolve({ error: undefined });
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    const load = (name: string, number: number) =>
      loadDetail(store, "acme", name, number, {
        provider: "github",
        platformHost: "github.com",
        repoPath: `acme/${name}`,
        sync: false,
      });
    await load("widget", 7);
    const onConflict = vi.fn();
    const applying = applyReviewSuggestions(
      store,
      "acme",
      "widget",
      7,
      { suggestions: [{ threadID: "thread-1", replacement: "return publish();" }] },
      onConflict,
    );

    await load("other-widget", 8);
    await load("widget", 7);
    resolveApply({ error: conflictProblem("stale_state") });

    await expect(applying).resolves.toBe(false);
    expect(onConflict).not.toHaveBeenCalled();
    expect(store.getDetailError()).toBeNull();
    expect(store.getDetail()?.repo_name).toBe("widget");
  });

  it("accepts a delayed suggestion conflict after a same-pull refresh", async () => {
    let resolveApply!: (value: { error: ProblemBody }) => void;
    const applyResponse = new Promise<{ error: ProblemBody }>((resolve) => {
      resolveApply = resolve;
    });
    const get = vi.fn().mockResolvedValue({ data: pullDetail("reviewed-head") });
    const post = vi.fn((path: string) => {
      if (path.endsWith("/review-suggestions/apply")) return applyResponse;
      return Promise.resolve({ error: undefined });
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    const options = {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false as const,
    };
    await loadDetail(store, "acme", "widget", 7, options);
    const onConflict = vi.fn();
    const applying = applyReviewSuggestions(
      store,
      "acme",
      "widget",
      7,
      { suggestions: [{ threadID: "thread-1", replacement: "return publish();" }] },
      onConflict,
    );

    await loadDetail(store, "acme", "widget", 7, options);
    resolveApply({ error: conflictProblem("stale_state") });

    await expect(applying).resolves.toBe(false);
    expect(onConflict).toHaveBeenCalledWith(
      expect.objectContaining({
        reason: "stale_state",
        expectedHeadSha: "reviewed-head",
      }),
    );
    expect(store.getDetailError()).toBe("pull request state changed");
  });

  it("does not retain a pending suggestion reconciliation after navigation", async () => {
    let resolveApply!: (value: { data: { status: string }; error: undefined }) => void;
    const applyResponse = new Promise<{ data: { status: string }; error: undefined }>((resolve) => {
      resolveApply = resolve;
    });
    const get = vi.fn(async (_path: string, options: { params: { path: { name: string; number: number } } }) => {
      const loaded = pullDetail(options.params.path.name === "widget" ? "reviewed-head" : "other-head");
      loaded.repo_name = options.params.path.name;
      loaded.repo.repo_path = `acme/${options.params.path.name}`;
      loaded.merge_request.Number = options.params.path.number;
      return { data: loaded };
    });
    const post = vi.fn((path: string) => {
      if (path.endsWith("/review-suggestions/apply")) return applyResponse;
      return Promise.resolve({ error: undefined });
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    const load = (name: string, number: number) =>
      loadDetail(store, "acme", name, number, {
        provider: "github",
        platformHost: "github.com",
        repoPath: `acme/${name}`,
        sync: false,
      });
    await load("widget", 7);
    const applying = applyReviewSuggestions(store, "acme", "widget", 7, {
      suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
    });

    await load("other-widget", 8);
    resolveApply({ data: { status: "applied" }, error: undefined });

    await expect(applying).resolves.toBe(true);
    expect(store.getDetail()?.repo_name).toBe("other-widget");
    await load("widget", 7);
    expect(store.getDetail()?.platform_head_sha).toBe("reviewed-head");
    expect(post.mock.calls.some(([path]) => String(path).endsWith("/sync"))).toBe(false);
    expect(getFlash()).toMatchObject({
      message: "Suggestion was applied after navigation. Refresh before applying it again.",
      tone: "warning",
    });
  });

  it("flashes a delayed ordinary suggestion failure without changing the new selection", async () => {
    let resolveApply!: (value: { error: { detail: string } }) => void;
    const applyResponse = new Promise<{ error: { detail: string } }>((resolve) => {
      resolveApply = resolve;
    });
    const get = vi.fn(async (_path: string, options: { params: { path: { name: string; number: number } } }) => {
      const loaded = pullDetail(options.params.path.name === "widget" ? "reviewed-head" : "other-head");
      loaded.repo_name = options.params.path.name;
      loaded.repo.repo_path = `acme/${options.params.path.name}`;
      loaded.merge_request.Number = options.params.path.number;
      return { data: loaded };
    });
    const post = vi.fn((path: string) => {
      if (path.endsWith("/review-suggestions/apply")) return applyResponse;
      return Promise.resolve({ error: undefined });
    });
    const store = createDetailStore({ client: mockClient({ GET: get, POST: post }) });
    const load = (name: string, number: number) =>
      loadDetail(store, "acme", name, number, {
        provider: "github",
        platformHost: "github.com",
        repoPath: `acme/${name}`,
        sync: false,
      });
    await load("widget", 7);
    const applying = applyReviewSuggestions(store, "acme", "widget", 7, {
      suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
    });

    await load("other-widget", 8);
    resolveApply({ error: { detail: "provider rejected suggestion" } });

    await expect(applying).resolves.toBe(false);
    expect(store.getDetail()?.repo_name).toBe("other-widget");
    expect(store.getDetail()?.merge_request.Number).toBe(8);
    expect(getFlashes()).toHaveLength(1);
    expect(getFlash()).toMatchObject({ message: "provider rejected suggestion", tone: "danger" });
  });

  it("fails closed when apply-suggestion conflict refresh fails", async () => {
    const tests = [
      {
        reason: "stale_state",
        assertDetail: (detail: PullDetail | null) => {
          expect(detail?.platform_head_sha).toBe("");
          expect(detail?.merge_request.State).toBe("open");
        },
      },
      {
        reason: "not_open",
        assertDetail: (detail: PullDetail | null) => {
          expect(detail?.platform_head_sha).toBe("old-head");
          expect(detail?.merge_request.State).toBe("closed");
        },
      },
    ] as const;
    for (const tt of tests) {
      const conflict = conflictProblem(tt.reason);
      const client = makeGeneratedClient({
        PullRequestsService: {
          getPull: vi.fn().mockResolvedValue(pullDetail("old-head")),
          applyPrReviewSuggestions: vi
            .fn()
            .mockRejectedValue(new GeneratedProblemResponse(conflict, Response.json(conflict, { status: 409 }))),
          syncPull: vi.fn().mockRejectedValue(new Error("sync failed")),
        },
      });
      const store = createDetailStore({
        client,
      });
      await loadDetail(store, "acme", "widget", 7, {
        provider: "github",
        platformHost: "github.com",
        repoPath: "acme/widget",
        sync: false,
      });

      const ok = await applyReviewSuggestions(store, "acme", "widget", 7, {
        suggestions: [{ threadID: "thread-1", replacement: "return publish();" }],
      });

      expect(ok).toBe(false);
      expect(store.getDetailError()).toBe("pull request state changed");
      tt.assertDetail(store.getDetail());
    }
  });

  it("applies a local body edit addressed through a provider alias and omitted host", async () => {
    // Task-list checkbox clicks pass the caller's route vocabulary; the
    // loaded payload is canonical ("github"/"github.com"). An exact
    // comparison would silently drop the optimistic toggle.
    const get = vi.fn().mockResolvedValueOnce({ data: pullDetail("head") });
    const store = createDetailStore({ client: mockClient({ GET: get }) });
    await loadDetail(store, "acme", "widget", 7, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget",
      sync: false,
    });

    store.setLocalPRBody("gh", undefined, "acme", "widget", 7, "- [x] done");

    expect(store.getDetail()?.merge_request.Body).toBe("- [x] done");
    expect(store.hasUnsavedLocalBody()).toBe(true);
  });

  it("clears the matching unsaved body after the server normalizes it", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Body = "initial";
    const canonical = pullDetail("head");
    canonical.merge_request.Body = "submitted\n";
    const patch = vi.fn().mockResolvedValue({ data: canonical, error: undefined });
    const store = createDetailStore({
      client: mockClient({ GET: vi.fn().mockResolvedValue({ data: initial }), PATCH: patch }),
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "submitted");
    store.savePRBodyInBackground(routeRef, 7, "submitted");

    await vi.waitFor(() => expect(store.hasUnsavedLocalBody()).toBe(false));
    expect(store.getDetail()?.merge_request.Body).toBe("submitted\n");
  });

  it("submits a captured background body after navigation", async () => {
    const first = pullDetailFor("widget-a", 7, "first-head");
    first.merge_request.Body = "initial";
    const second = pullDetailFor("widget-b", 8, "second-head");
    const canonical = pullDetailFor("widget-a", 7, "first-head");
    canonical.merge_request.Body = "captured edit";
    const get = vi.fn().mockResolvedValueOnce({ data: first }).mockResolvedValueOnce({ data: second });
    const patch = vi.fn().mockResolvedValue({ data: canonical, error: undefined });
    const store = createDetailStore({ client: mockClient({ GET: get, PATCH: patch }) });
    const firstRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget-a",
      repoPath: "acme/widget-a",
    };
    await loadDetail(store, "acme", "widget-a", 7, { ...firstRef, sync: false });
    store.setLocalPRBody("github", "github.com", "acme", "widget-a", 7, "captured edit");
    await loadDetail(store, "acme", "widget-b", 8, {
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widget-b",
      sync: false,
    });

    store.savePRBodyInBackground(firstRef, 7, "captured edit");

    await vi.waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    expect(store.getDetail()?.repo_name).toBe("widget-b");
  });

  it("coalesces pending background body saves to the latest value", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Body = "initial";
    const firstResponse = deferred<{ data: PullDetail; error: undefined }>();
    const patch = vi.fn(
      (_path: string, options: { body?: { body?: string } }): Promise<{ data: PullDetail; error: undefined }> => {
        const response = pullDetail("head");
        response.merge_request.Body = options.body?.body ?? "";
        return options.body?.body === "first"
          ? firstResponse.promise
          : Promise.resolve({ data: response, error: undefined });
      },
    );
    const store = createDetailStore({
      client: mockClient({ GET: vi.fn().mockResolvedValue({ data: initial }), PATCH: patch }),
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "first");
    const first = store.savePRBodyInBackground(routeRef, 7, "first");
    expect(first).toBeUndefined();
    await vi.waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "second");
    store.savePRBodyInBackground(routeRef, 7, "second");
    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "third");
    const latest = store.savePRBodyInBackground(routeRef, 7, "third");

    expect(latest).toBeUndefined();
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    const confirmedFirst = pullDetail("head");
    confirmedFirst.merge_request.Body = "first";
    firstResponse.resolve({ data: confirmedFirst, error: undefined });
    await vi.waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(store.hasUnsavedLocalBody()).toBe(false));
    expect(patch.mock.calls.map(([, options]) => options.body?.body)).toEqual(["first", "third"]);
    expect(store.getDetail()?.merge_request.Body).toBe("third");
  });

  it("does not let a queued background body overwrite a newer explicit save", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Body = "initial";
    const firstResponse = deferred<{ data: PullDetail; error: undefined }>();
    const patch = vi.fn(
      (_path: string, options: { body?: { body?: string } }): Promise<{ data: PullDetail; error: undefined }> => {
        const response = pullDetail("head");
        response.merge_request.Body = options.body?.body ?? "";
        return options.body?.body === "first"
          ? firstResponse.promise
          : Promise.resolve({ data: response, error: undefined });
      },
    );
    const store = createDetailStore({
      client: mockClient({ GET: vi.fn().mockResolvedValue({ data: initial }), PATCH: patch }),
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "first");
    store.savePRBodyInBackground(routeRef, 7, "first");
    await vi.waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    store.setLocalPRBody("github", "github.com", "acme", "widget", 7, "queued");
    store.savePRBodyInBackground(routeRef, 7, "queued");
    const settled = Promise.withResolvers<void>();
    store.updatePRContent(routeRef, 7, { body: "explicit" }, { onSettled: settled.resolve });

    const confirmedFirst = pullDetail("head");
    confirmedFirst.merge_request.Body = "first";
    firstResponse.resolve({ data: confirmedFirst, error: undefined });
    await settled.promise;
    await vi.waitFor(() => expect(patch).toHaveBeenCalledTimes(2));

    expect(patch.mock.calls.map(([, options]) => options.body?.body)).toEqual(["first", "explicit"]);
    expect(store.getDetail()?.merge_request.Body).toBe("explicit");
  });

  it("rolls back a failed explicit body save so the same draft can be retried", async () => {
    const initial = pullDetail("head");
    initial.merge_request.Body = "initial";
    const canonical = pullDetail("head");
    canonical.merge_request.Body = "edited";
    const patch = vi
      .fn()
      .mockResolvedValueOnce({ error: { detail: "save failed" } })
      .mockResolvedValueOnce({ data: canonical, error: undefined });
    const store = createDetailStore({
      client: mockClient({ GET: vi.fn().mockResolvedValue({ data: initial }), PATCH: patch }),
    });
    const routeRef = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widget",
      repoPath: "acme/widget",
    };
    await loadDetail(store, "acme", "widget", 7, { ...routeRef, sync: false });

    store.updatePRContent(routeRef, 7, { body: "edited" });
    await vi.waitFor(() => expect(getFlash()?.message).toBe("save failed"));
    expect(store.getDetail()?.merge_request.Body).toBe("initial");
    expect(store.hasUnsavedLocalBody()).toBe(false);

    const settled = Promise.withResolvers<void>();
    store.updatePRContent(routeRef, 7, { body: "edited" }, { onSettled: settled.resolve });
    await settled.promise;

    expect(patch).toHaveBeenCalledTimes(2);
    expect(store.getDetail()?.merge_request.Body).toBe("edited");
  });
});
