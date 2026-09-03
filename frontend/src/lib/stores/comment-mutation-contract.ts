import { describe, expect, it, vi } from "vite-plus/test";

import type { GeneratedClient } from "../api/generated-api.js";
import type { ProblemBody } from "../api/problems.js";
import { getFlash } from "./flash.svelte.js";
import type { MutationCallbacks } from "./ordered-mutations.js";

export interface ContractCommentEvent {
  readonly EventType?: unknown;
  readonly PlatformID?: unknown;
  readonly Body?: unknown;
  readonly ID?: unknown;
  readonly Kind?: unknown;
}

interface CommentMutationSnapshot {
  readonly number: number;
  readonly events: readonly ContractCommentEvent[];
}

export interface CommentMutationStoreAdapter {
  load(number: number, sync: boolean): Promise<void>;
  snapshot(): CommentMutationSnapshot | null;
  isSyncing(): boolean;
  error(): string | null;
  submit(number: number, body: string, callbacks?: MutationCallbacks): void;
  edit(number: number, commentID: number, body: string, callbacks?: MutationCallbacks): void;
  delete(number: number, commentID: number, callbacks?: MutationCallbacks): void;
}

export interface CommentMutationContractAdapter {
  readonly name: string;
  readonly commentMemberPath:
    | "/pulls/{provider}/{owner}/{name}/{number}/comments/{comment_id}"
    | "/issues/{provider}/{owner}/{name}/{number}/comments/{comment_id}";
  readonly syncPath:
    | "/pulls/{provider}/{owner}/{name}/{number}/sync"
    | "/issues/{provider}/{owner}/{name}/{number}/sync";
  makeDetail(events?: ContractCommentEvent[], number?: number): unknown;
  create(client: GeneratedClient): CommentMutationStoreAdapter;
}

function deletionSucceeded() {
  return { data: undefined, response: new Response(null, { status: 204 }) };
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

function mutationResult(start: (callbacks: MutationCallbacks) => void): Promise<boolean> {
  const result = Promise.withResolvers<boolean>();
  start({
    onSuccess: () => result.resolve(true),
    onFailure: () => result.resolve(false),
  });
  return result.promise;
}

export function runCommentMutationContract(adapter: CommentMutationContractAdapter): void {
  describe(`${adapter.name} comment mutation contract`, () => {
    it("acknowledges a posted comment before reporting reconciliation failure", async () => {
      let getCalls = 0;
      const store = adapter.create({
        GET: vi.fn(async () => {
          getCalls++;
          if (getCalls === 1) return { data: adapter.makeDetail() };
          throw new Error("offline");
        }),
        POST: vi.fn(async () => ({ data: { ID: 42 }, response: new Response(null, { status: 201 }) })),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient);
      await store.load(1, false);
      const onSuccess = vi.fn();
      const onFailure = vi.fn();
      const settled = Promise.withResolvers<void>();

      store.submit(1, "hello", { onSuccess, onFailure, onSettled: settled.resolve });
      await settled.promise;

      expect(onSuccess).toHaveBeenCalledOnce();
      expect(onFailure).not.toHaveBeenCalled();
      await vi.waitFor(() =>
        expect(getFlash()).toMatchObject({
          message: "Comment was posted, but the latest discussion could not be refreshed.",
          tone: "danger",
        }),
      );
    });

    it("does not reconcile an acknowledged comment after navigation changes the selection", async () => {
      const posted = Promise.withResolvers<void>();
      const get = vi
        .fn()
        .mockResolvedValueOnce({ data: adapter.makeDetail([], 1) })
        .mockResolvedValueOnce({ data: adapter.makeDetail([], 2) });
      const post = vi.fn(async (path: string) => {
        if (path.includes("/comments")) {
          await posted.promise;
          return { data: { ID: 42 }, response: new Response(null, { status: 201 }) };
        }
        return { data: adapter.makeDetail([], 1) };
      });
      const store = adapter.create({
        GET: get,
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient);
      await store.load(1, false);
      const settled = Promise.withResolvers<void>();
      store.submit(1, "hello", { onSettled: settled.resolve });
      await vi.waitFor(() => expect(post).toHaveBeenCalledOnce());

      await store.load(2, false);
      posted.resolve();
      await settled.promise;
      await Promise.resolve();

      expect(get).toHaveBeenCalledTimes(2);
      expect(post).toHaveBeenCalledTimes(1);
      expect(store.snapshot()?.number).toBe(2);
    });

    it("rolls back an optimistic edit when acknowledgement fails", async () => {
      const patch = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
      const original = { EventType: "issue_comment", PlatformID: 44, Body: "before" };
      const store = adapter.create({
        GET: vi.fn(async () => ({ data: adapter.makeDetail([original]) })),
        POST: vi.fn(),
        PUT: vi.fn(),
        PATCH: vi.fn(() => patch.promise),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient);
      const settled = Promise.withResolvers<void>();
      await store.load(1, false);

      store.edit(1, 44, "after", { onSettled: settled.resolve });
      await vi.waitFor(() => expect(store.snapshot()?.events[0]?.Body).toBe("after"));
      patch.resolve(deletionFailed("provider denied edit"));
      await settled.promise;

      expect(store.snapshot()?.events[0]?.Body).toBe("before");
    });

    it("does not confirm a rejected edit through deletion of another comment", async () => {
      const edit = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
      const deletion = Promise.withResolvers<ReturnType<typeof deletionSucceeded>>();
      const first = { EventType: "issue_comment", PlatformID: 44, Body: "before" };
      const second = { EventType: "issue_comment", PlatformID: 45, Body: "remove me" };
      const store = adapter.create({
        GET: vi.fn(async () => ({ data: adapter.makeDetail([first, second]) })),
        POST: vi.fn(),
        PUT: vi.fn(),
        PATCH: vi.fn(() => edit.promise),
        DELETE: vi.fn(() => deletion.promise),
      } as unknown as GeneratedClient);
      const editSettled = Promise.withResolvers<void>();
      const deletionSettled = Promise.withResolvers<void>();
      await store.load(1, false);

      store.edit(1, 44, "after", { onSettled: editSettled.resolve });
      store.delete(1, 45, { onSettled: deletionSettled.resolve });
      await vi.waitFor(() => expect(store.snapshot()?.events).toEqual([{ ...first, Body: "after" }]));

      edit.resolve(deletionFailed("provider denied edit"));
      await editSettled.promise;
      deletion.resolve(deletionSucceeded());
      await deletionSettled.promise;

      expect(store.snapshot()?.events).toEqual([first]);
    });

    it("rolls back an optimistic deletion and reports the failure", async () => {
      const pendingDelete = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
      const original = { EventType: "issue_comment", PlatformID: 44, Body: "keep me" };
      const get = vi.fn(async () => ({ data: adapter.makeDetail([original]) }));
      const store = adapter.create({
        GET: get,
        POST: vi.fn(async () => ({ data: undefined })),
        PUT: vi.fn(),
        DELETE: vi.fn(() => pendingDelete.promise),
      } as unknown as GeneratedClient);
      await store.load(1, false);
      get.mockClear();

      const result = mutationResult((callbacks) => store.delete(1, 44, callbacks));
      await vi.waitFor(() => expect(store.snapshot()?.events).toEqual([]));
      pendingDelete.resolve(deletionFailed("provider denied deletion"));

      expect(await result).toBe(false);
      expect(get).not.toHaveBeenCalled();
      expect(store.error()).toBe("provider denied deletion");
      expect(store.snapshot()?.events).toEqual([original]);
    });

    it("keeps an acknowledged deletion hidden while ordinary sync converges", async () => {
      const staleDetail = adapter.makeDetail([{ EventType: "issue_comment", PlatformID: 44 }]);
      const get = vi.fn(async () => ({ data: staleDetail }));
      const post = vi.fn(async () => ({ data: staleDetail }));
      const del = vi.fn(async () => deletionSucceeded());
      const store = adapter.create({
        GET: get,
        POST: post,
        PUT: vi.fn(),
        DELETE: del,
      } as unknown as GeneratedClient);
      await store.load(1, true);
      await vi.waitFor(() => expect(post).toHaveBeenCalled());
      await vi.waitFor(() => expect(store.isSyncing()).toBe(false));
      get.mockClear();
      post.mockClear();

      const result = await mutationResult((callbacks) => store.delete(1, 44, callbacks));

      expect(result).toBe(true);
      expect(del).toHaveBeenCalledWith(adapter.commentMemberPath, {
        params: {
          path: { provider: "github", owner: "octo", name: "repo", number: 1, comment_id: 44 },
        },
        signal: expect.any(AbortSignal),
      });
      expect(store.snapshot()?.events).toEqual([]);
      await vi.waitFor(() =>
        expect(post).toHaveBeenCalledWith(adapter.syncPath, {
          params: { path: { provider: "github", owner: "octo", name: "repo", number: 1 } },
          signal: expect.any(AbortSignal),
        }),
      );
      expect(get).not.toHaveBeenCalled();
    });

    it("does not expose a failed deletion after selection changes", async () => {
      const pendingDelete = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
      const get = vi.fn(async (_path: string, request: { params: { path: { number: number } } }) => ({
        data: adapter.makeDetail(
          request.params.path.number === 1 ? [{ EventType: "issue_comment", PlatformID: 44 }] : [],
          request.params.path.number,
        ),
      }));
      const del = vi.fn(() => pendingDelete.promise);
      const store = adapter.create({
        GET: get,
        POST: vi.fn(async () => ({ data: undefined })),
        PUT: vi.fn(),
        DELETE: del,
      } as unknown as GeneratedClient);
      await store.load(1, false);

      const result = mutationResult((callbacks) => store.delete(1, 44, callbacks));
      await vi.waitFor(() => expect(del).toHaveBeenCalledOnce());
      await vi.waitFor(() => expect(store.snapshot()?.events).toEqual([]));
      await store.load(2, false);
      pendingDelete.resolve(deletionFailed("old deletion failed"));

      expect(await result).toBe(false);
      expect(store.snapshot()?.number).toBe(2);
      expect(store.error()).toBeNull();
    });

    it("keeps a deletion failure after reloading the same selection", async () => {
      const pendingDelete = Promise.withResolvers<ReturnType<typeof deletionFailed>>();
      const del = vi.fn(() => pendingDelete.promise);
      const store = adapter.create({
        GET: vi.fn(async () => ({
          data: adapter.makeDetail([{ EventType: "issue_comment", PlatformID: 44 }]),
        })),
        POST: vi.fn(),
        PUT: vi.fn(),
        DELETE: del,
      } as unknown as GeneratedClient);
      await store.load(1, false);

      const result = mutationResult((callbacks) => store.delete(1, 44, callbacks));
      await vi.waitFor(() => expect(del).toHaveBeenCalledOnce());
      await store.load(1, false);
      pendingDelete.resolve(deletionFailed("provider denied deletion"));

      expect(await result).toBe(false);
      expect(store.error()).toBe("provider denied deletion");
    });

    it("does not apply an acknowledged deletion over a newer selection", async () => {
      const pendingDelete = Promise.withResolvers<ReturnType<typeof deletionSucceeded>>();
      const get = vi.fn(async (_path: string, request: { params: { path: { number: number } } }) => ({
        data: adapter.makeDetail(
          request.params.path.number === 1 ? [{ EventType: "issue_comment", PlatformID: 44 }] : [{ PlatformID: 99 }],
          request.params.path.number,
        ),
      }));
      const del = vi.fn(() => pendingDelete.promise);
      const store = adapter.create({
        GET: get,
        POST: vi.fn(async () => ({ data: undefined })),
        PUT: vi.fn(),
        DELETE: del,
      } as unknown as GeneratedClient);
      await store.load(1, false);

      const result = mutationResult((callbacks) => store.delete(1, 44, callbacks));
      await vi.waitFor(() => expect(del).toHaveBeenCalledOnce());
      await vi.waitFor(() => expect(store.snapshot()?.events).toEqual([]));
      await store.load(2, false);
      pendingDelete.resolve(deletionSucceeded());

      expect(await result).toBe(true);
      expect(store.snapshot()).toEqual({ number: 2, events: [{ PlatformID: 99 }] });
    });

    it("does not overwrite a newer selection when post-comment refresh resolves late", async () => {
      const detailA = adapter.makeDetail([], 1);
      const detailB = adapter.makeDetail([], 2);
      const refresh = Promise.withResolvers<unknown>();
      let getCallCount = 0;
      const store = adapter.create({
        GET: vi.fn(async () => {
          getCallCount++;
          if (getCallCount === 1) return { data: detailA };
          if (getCallCount === 2) return await refresh.promise;
          return { data: detailB };
        }),
        POST: vi.fn(async (path: string) => {
          if (path.includes("/sync")) return { data: undefined };
          if (path.includes("/comments")) return { data: { ID: 42 } };
          return { data: undefined };
        }),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient);
      await store.load(1, false);

      const submitted = mutationResult((callbacks) => store.submit(1, "hi", callbacks));
      await vi.waitFor(() => expect(getCallCount).toBe(2));
      await store.load(2, false);
      expect(store.snapshot()?.number).toBe(2);

      refresh.resolve({ data: detailA });
      await submitted;
      await Promise.resolve();
      await Promise.resolve();

      expect(store.snapshot()?.number).toBe(2);
    });

    it("does not let stale sync overwrite post-comment refresh", async () => {
      const staleDetail = adapter.makeDetail([]);
      const freshDetail = adapter.makeDetail([{ ID: 42, Kind: "comment" }]);
      const staleSync = Promise.withResolvers<unknown>();
      let getCallCount = 0;
      let syncCallCount = 0;
      const store = adapter.create({
        GET: vi.fn(async () => {
          getCallCount++;
          return { data: getCallCount === 1 ? staleDetail : freshDetail };
        }),
        POST: vi.fn(async (path: string) => {
          if (path.includes("/sync")) {
            syncCallCount++;
            if (syncCallCount === 1) return await staleSync.promise;
            return { data: freshDetail };
          }
          if (path.includes("/comments")) return { data: { ID: 42 } };
          return { data: undefined };
        }),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient);

      await store.load(1, true);
      await mutationResult((callbacks) => store.submit(1, "hello", callbacks));
      await vi.waitFor(() => expect(store.snapshot()?.events).toHaveLength(1));

      staleSync.resolve({ data: staleDetail, error: undefined });
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();

      expect(store.snapshot()?.events).toHaveLength(1);
    });
  });
}
