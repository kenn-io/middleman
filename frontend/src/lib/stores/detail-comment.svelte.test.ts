import { Cause, Effect, Exit, Option } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import type { OwnedAppRuntime } from "../app/runtime.js";
import type { ProblemBody } from "../api/problems.js";
import {
  createDetailStore as createRuntimeDetailStore,
  type DetailStore,
  type DetailStoreOptions,
} from "./detail.svelte.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { runCommentMutationContract, type ContractCommentEvent } from "./comment-mutation-contract.js";
import { dismissFlash, getFlashes } from "./flash.svelte.js";

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

function submitComment(
  store: DetailStore,
  owner: string,
  name: string,
  number: number,
  body: string,
): Promise<boolean> {
  const completion = Promise.withResolvers<boolean>();
  store.submitComment(owner, name, number, body, {
    onSuccess: () => completion.resolve(true),
    onFailure: () => completion.resolve(false),
  });
  return completion.promise;
}

function deletionFailed(detail: string) {
  const error: ProblemBody = {
    code: "validationError",
    detail,
    title: "Invalid request",
    type: "about:blank",
  };
  return { error, response: new Response(null, { status: 400 }) };
}

beforeEach(() => {
  runtime = undefined;
  for (const flash of getFlashes()) dismissFlash(flash.id);
});

afterEach(async () => {
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

const pullRef = {
  provider: "github",
  platformHost: "github.com",
  repoPath: "octo/repo",
};

interface MockDetail {
  repo_owner: string;
  repo_name: string;
  repo: {
    provider: string;
    platform_host: string;
    owner: string;
    name: string;
    repo_path: string;
  };
  merge_request: { Number: number };
  events: unknown[];
}

function makeDetail(events: unknown[] = [], number = 1): MockDetail {
  return {
    repo_owner: "octo",
    repo_name: "repo",
    repo: {
      provider: pullRef.provider,
      platform_host: pullRef.platformHost,
      owner: "octo",
      name: "repo",
      repo_path: pullRef.repoPath,
    },
    merge_request: { Number: number },
    events,
  };
}

runCommentMutationContract({
  name: "createDetailStore",
  commentMemberPath: "/pulls/{provider}/{owner}/{name}/{number}/comments/{comment_id}",
  syncPath: "/pulls/{provider}/{owner}/{name}/{number}/sync",
  makeDetail,
  create(client) {
    const store = createDetailStore({ client });
    return {
      load: (number, sync) => loadDetail(store, "octo", "repo", number, { ...pullRef, sync }),
      snapshot: () => {
        const detail = store.getDetail();
        return detail === null
          ? null
          : {
              number: detail.merge_request.Number,
              events: detail.events as ContractCommentEvent[],
            };
      },
      isSyncing: store.isDetailSyncing,
      error: store.getDetailError,
      submit: (number, body, callbacks) => store.submitComment("octo", "repo", number, body, callbacks),
      edit: (number, commentID, body, callbacks) =>
        store.editComment("octo", "repo", number, commentID, body, callbacks),
      delete: (number, commentID, callbacks) => store.deleteComment("octo", "repo", number, commentID, callbacks),
    };
  },
});

describe("createDetailStore submitComment", () => {
  it("preserves permanent API failures when reconciling provider events", async () => {
    let getCalls = 0;
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => {
          getCalls++;
          return getCalls === 1
            ? { data: makeDetail() }
            : {
                error: {
                  code: "pullNotFound",
                  detail: "pull request not found",
                  title: "Not found",
                  type: "about:blank",
                },
                response: new Response(null, { status: 404 }),
              };
        }),
        POST: vi.fn(),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    await loadDetail(store, "octo", "repo", 1, { ...pullRef, sync: false });
    const execution = runtime.runCommand(store.refreshDetailOnlyEffect("octo", "repo", 1, pullRef), {
      operation: "reconcile pull request after provider event",
      safeContext: {},
      onFailure: () => {},
    });
    const exit = await Effect.runPromise(execution.await);

    expect(Exit.isFailure(exit)).toBe(true);
    if (Exit.isFailure(exit)) {
      const failure = Cause.findErrorOption(exit.cause);
      expect(Option.isSome(failure)).toBe(true);
      if (Option.isSome(failure)) expect(failure.value).toMatchObject({ _tag: "ApiProblemError" });
    }
  });

  it("retries an event detail refresh superseded by a same-selection foreground load", async () => {
    const eventRead = Promise.withResolvers<{ data: MockDetail }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: makeDetail([], 1) })
      .mockImplementationOnce(() => eventRead.promise)
      .mockResolvedValueOnce({ data: makeDetail([], 1) });
    const store = createDetailStore({
      client: { GET: get, POST: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() } as unknown as GeneratedClient,
    });
    await loadDetail(store, "octo", "repo", 1, { ...pullRef, sync: false });
    const execution = runtime.runCommand(store.refreshDetailOnlyEffect("octo", "repo", 1, pullRef), {
      operation: "reconcile pull request after provider event",
      safeContext: {},
      onFailure: () => {},
    });
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));

    await loadDetail(store, "octo", "repo", 1, { ...pullRef, sync: false });
    eventRead.resolve({ data: makeDetail([], 1) });
    const exit = await Effect.runPromise(execution.await);

    expect(Exit.isFailure(exit)).toBe(true);
    if (Exit.isFailure(exit)) {
      const failure = Cause.findErrorOption(exit.cause);
      expect(Option.isSome(failure)).toBe(true);
      if (Option.isSome(failure)) expect(failure.value).toMatchObject({ _tag: "TransientTransportError" });
    }
  });

  it("settles a failed discussion reply without stopping later comment work", async () => {
    const post = vi
      .fn()
      .mockResolvedValueOnce(deletionFailed("provider denied reply"))
      .mockResolvedValueOnce({ data: { ID: 45 }, response: new Response(null, { status: 201 }) });
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: makeDetail() })),
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    const firstSettled = Promise.withResolvers<void>();
    const secondSettled = Promise.withResolvers<void>();
    const firstFailure = vi.fn();
    const secondSuccess = vi.fn();
    await loadDetail(store, "octo", "repo", 1, { ...pullRef, sync: false });

    store.replyToDiscussion("octo", "repo", 1, "thread-1", "first", {
      onFailure: firstFailure,
      onSettled: firstSettled.resolve,
    });
    store.replyToDiscussion("octo", "repo", 1, "thread-1", "second", {
      onSuccess: secondSuccess,
      onSettled: secondSettled.resolve,
    });
    await Promise.all([firstSettled.promise, secondSettled.promise]);

    expect(firstFailure).toHaveBeenCalledOnce();
    expect(secondSuccess).toHaveBeenCalledOnce();
    expect(post).toHaveBeenCalledTimes(2);
  });

  it("restores a rejected deletion accepted behind a discussion reply", async () => {
    const reply = Promise.withResolvers<{ data: { ID: number }; response: Response }>();
    const deletion = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
    const original = { EventType: "issue_comment", PlatformID: 44, Body: "keep me" };
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: makeDetail([original]) })),
        POST: vi.fn(() => reply.promise),
        PUT: vi.fn(),
        DELETE: vi.fn(() => deletion.promise),
      } as unknown as GeneratedClient,
    });
    const replySettled = Promise.withResolvers<void>();
    const deletionSettled = Promise.withResolvers<void>();
    await loadDetail(store, "octo", "repo", 1, { ...pullRef, sync: false });

    store.replyToDiscussion("octo", "repo", 1, "thread-1", "reply", { onSettled: replySettled.resolve });
    await vi.waitFor(() => expect(store.getDetail()?.events).toEqual([original]));
    store.deleteComment("octo", "repo", 1, 44, { onSettled: deletionSettled.resolve });
    await vi.waitFor(() => expect(store.getDetail()?.events).toEqual([]));

    reply.resolve({ data: { ID: 45 }, response: new Response(null, { status: 201 }) });
    await replySettled.promise;
    deletion.resolve(deletionFailed("provider denied deletion"));
    await deletionSettled.promise;

    expect(store.getDetail()?.events).toEqual([original]);
  });

  it("never flips loading flag while refreshing after a comment", async () => {
    const detailData = makeDetail();
    const loadingDuringRefresh: boolean[] = [];
    let getCallCount = 0;
    const holder: {
      store: ReturnType<typeof createDetailStore> | null;
    } = { store: null };

    const client = {
      GET: vi.fn(async () => {
        getCallCount++;
        if (getCallCount > 1 && holder.store) {
          loadingDuringRefresh.push(holder.store.isDetailLoading());
        }
        return { data: detailData };
      }),
      POST: vi.fn(async (path: string) => {
        if (path.includes("/sync")) {
          return { data: detailData };
        }
        if (path.includes("/comments")) {
          return { data: { ID: 42 } };
        }
        return { data: undefined };
      }),
      PUT: vi.fn(),
      DELETE: vi.fn(),
    } as unknown as GeneratedClient;

    holder.store = createDetailStore({ client });

    await loadDetail(holder.store, "octo", "repo", 1, pullRef);
    // Allow background syncDetail microtasks to settle.
    await Promise.resolve();
    await Promise.resolve();

    await submitComment(holder.store, "octo", "repo", 1, "hello");

    expect(getCallCount).toBeGreaterThan(1);
    expect(loadingDuringRefresh.length).toBeGreaterThan(0);
    expect(loadingDuringRefresh.every((v) => v === false)).toBe(true);
    expect(holder.store.isDetailLoading()).toBe(false);
  });
  it("triggers post-comment sync and pulls list refresh", async () => {
    const detailData = makeDetail([{ ID: 42, Kind: "comment" }]);
    const loadPulls = vi.fn(async () => {});
    const postCalls: string[] = [];

    const client = {
      GET: vi.fn(async () => ({ data: detailData })),
      POST: vi.fn(async (path: string) => {
        postCalls.push(path);
        if (path.includes("/sync")) return { data: detailData };
        if (path.includes("/comments")) return { data: { ID: 42 } };
        return { data: undefined };
      }),
      PUT: vi.fn(),
      DELETE: vi.fn(),
    } as unknown as GeneratedClient;

    const store = createDetailStore({
      client,
      getPage: () => "pulls",
      pulls: { loadPulls },
    });

    await loadDetail(store, "octo", "repo", 1, pullRef);
    // Drain the background syncDetail from the initial load.
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    loadPulls.mockClear();
    postCalls.length = 0;

    await submitComment(store, "octo", "repo", 1, "hi");
    // Drain the background syncDetail fired by submitComment.
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    await vi.waitFor(() => expect(postCalls.some((p) => p.includes("/sync"))).toBe(true));
    await vi.waitFor(() => expect(loadPulls).toHaveBeenCalled());
  });
});
