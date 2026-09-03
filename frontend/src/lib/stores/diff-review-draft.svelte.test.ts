import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { Effect } from "effect";

import type { OwnedAppRuntime } from "../app/runtime.js";
import { TransientTransportError } from "../api/effect-errors.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import type { ProviderRouteRef } from "../api/provider-routes.js";
import { createDiffReviewDraftStore, type DiffReviewDraftStoreOptions } from "./diff-review-draft.svelte.js";
import type { MutationCallbacks } from "./ordered-mutations.js";
import { dismissFlash, getFlash, getFlashes } from "./flash.svelte.js";

interface MockDraftLoad {
  data: {
    comments: Array<{ id: string; body: string }>;
    supported_actions: string[];
    native_multiline_ranges: boolean;
  };
  response: { status: number; ok: boolean };
}

interface MockMutation {
  data?: Record<string, unknown>;
  error?: { code?: string; detail?: string; title?: string; details?: Record<string, unknown> };
  response: { status: number; ok: boolean };
}

function providerRef(overrides: Partial<ProviderRouteRef> = {}): ProviderRouteRef {
  return {
    provider: "forgejo",
    platformHost: "codeberg.org",
    owner: "acme",
    name: "widgets",
    repoPath: "acme/widgets",
    ...overrides,
  };
}

function draftLoad({
  comments = [],
  supportedActions = ["comment"],
  nativeMultilineRanges = true,
  status = 200,
  ok = true,
}: {
  comments?: MockDraftLoad["data"]["comments"];
  supportedActions?: string[];
  nativeMultilineRanges?: boolean;
  status?: number;
  ok?: boolean;
} = {}): MockDraftLoad {
  return {
    data: {
      comments,
      supported_actions: supportedActions,
      native_multiline_ranges: nativeMultilineRanges,
    },
    response: { status, ok },
  };
}

function mutation(overrides: Partial<MockMutation> = {}): MockMutation {
  const response = overrides.response ?? { status: 200, ok: true };
  if (overrides.error !== undefined) {
    return { error: overrides.error, response };
  }
  return { data: overrides.data ?? { status: "ok" }, response };
}

function failedMutation(): MockMutation {
  return mutation({
    error: { title: "failed" },
    response: { status: 502, ok: false },
  });
}

function draftComment(overrides: Record<string, unknown> = {}) {
  return {
    id: "draft-1",
    body: "needs work",
    path: "src/main.ts",
    side: "right",
    line: 7,
    new_line: 7,
    line_type: "add",
    diff_head_sha: "head-sha",
    created_at: "2026-03-30T14:01:00Z",
    updated_at: "2026-03-30T14:01:00Z",
    ...overrides,
  };
}

function mockGet(result: MockDraftLoad | Promise<MockDraftLoad> = draftLoad()) {
  return vi.fn(() => Promise.resolve(result));
}

function mockPost(result: MockMutation | Promise<MockMutation> = mutation()) {
  return vi.fn(() => Promise.resolve(result));
}

function mockPatch(result: MockMutation | Promise<MockMutation> = mutation()) {
  return vi.fn(() => Promise.resolve(result));
}

function mockDelete(result: MockMutation | Promise<MockMutation> = mutation()) {
  return vi.fn(() => Promise.resolve(result));
}

function mockClient({
  GET = mockGet(),
  POST = mockPost(),
  PATCH = mockPatch(),
  DELETE = mockDelete(),
}: {
  GET?: ReturnType<typeof vi.fn>;
  POST?: ReturnType<typeof vi.fn>;
  PATCH?: ReturnType<typeof vi.fn>;
  DELETE?: ReturnType<typeof vi.fn>;
} = {}): GeneratedClient {
  return { GET, POST, PATCH, DELETE } as unknown as GeneratedClient;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

let runtime: OwnedAppRuntime | undefined;

type TestStoreOptions = Omit<DiffReviewDraftStoreOptions, "runtime"> & { readonly client: GeneratedClient };

function createStore(options: TestStoreOptions) {
  const { client, ...storeOptions } = options;
  runtime = makeTestAppRuntime(client);
  return createDiffReviewDraftStore({ ...storeOptions, runtime });
}

function runAcknowledged(launch: (callbacks: MutationCallbacks) => void): Promise<boolean> {
  return new Promise((resolve) => {
    launch({
      onSuccess: () => resolve(true),
      onFailure: () => resolve(false),
    });
  });
}

describe("createDiffReviewDraftStore", () => {
  beforeEach(() => {
    runtime = undefined;
  });

  afterEach(async () => {
    for (const flash of getFlashes()) dismissFlash(flash.id);
    if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
  });

  it("launches publish synchronously and settles from its acknowledged mutation", async () => {
    const client = mockClient();
    const store = createStore({ client });
    store.setContext(providerRef(), 42, true);
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    const settled = Promise.withResolvers<void>();
    const succeeded = vi.fn();

    const result = store.publish("comment", "summary", {
      onSuccess: succeeded,
      onSettled: settled.resolve,
    });

    expect(result).toBeUndefined();
    await settled.promise;
    expect(succeeded).toHaveBeenCalledOnce();
  });

  it("refreshes PR detail after a successful publish", async () => {
    const client = mockClient();
    const onPublished = vi.fn(() => Effect.void);
    const store = createStore({ client, onPublished });
    const ref = providerRef();

    store.setContext(ref, 42, true);
    await Promise.resolve();
    const ok = await runAcknowledged((callbacks) => store.publish("comment", "summary", callbacks));

    expect(ok).toBe(true);
    await vi.waitFor(() => expect(onPublished).toHaveBeenCalledWith(ref, 42));
  });

  it("still refreshes the draft when timeline reconciliation fails", async () => {
    const order: string[] = [];
    const client = mockClient({
      GET: vi.fn(() => {
        order.push("draft");
        return Promise.resolve(draftLoad());
      }),
    });
    const store = createStore({
      client,
      onPublished: () =>
        Effect.sync(() => order.push("timeline")).pipe(
          Effect.andThen(
            Effect.fail(
              TransientTransportError.make({
                operation: "refresh published review timeline",
                cause: new Error("offline"),
              }),
            ),
          ),
        ),
    });

    store.setContext(providerRef(), 42, true);
    await Promise.resolve();

    await expect(runAcknowledged((callbacks) => store.publish("comment", "summary", callbacks))).resolves.toBe(true);
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(2));
    expect(order).toEqual(["draft", "timeline", "draft"]);
    expect(store.getError()).toBeNull();
    expect(getFlash()).toMatchObject({
      message: "Review was published, but the pull request timeline could not be refreshed.",
      tone: "danger",
    });
  });

  it("does not refresh PR detail when publish fails", async () => {
    const client = mockClient({ POST: mockPost(failedMutation()) });
    const onPublished = vi.fn(() => Effect.void);
    const store = createStore({ client, onPublished });
    const settled = Promise.withResolvers<void>();

    store.setContext(providerRef(), 42, true);
    await Promise.resolve();
    store.publish("comment", "summary", { onSettled: settled.resolve });
    await settled.promise;

    expect(onPublished).not.toHaveBeenCalled();
    expect(store.getError()).toBeNull();
    expect(getFlash()).toMatchObject({ message: "failed", tone: "danger" });
  });

  it("reloads detail and draft after a stale publish conflict", async () => {
    const client = mockClient({
      GET: vi
        .fn()
        .mockResolvedValueOnce(
          draftLoad({
            comments: [{ id: "stale", body: "stale draft" }],
          }),
        )
        .mockResolvedValueOnce(
          draftLoad({
            comments: [{ id: "fresh", body: "fresh draft" }],
          }),
        ),
      POST: mockPost(
        mutation({
          error: {
            code: "conflict",
            detail: "review draft head is stale",
            details: { reason: "stale_state" },
          },
          response: { status: 409, ok: false },
        }),
      ),
    });
    const syncCompleted = Promise.withResolvers<void>();
    const onStalePublish = vi.fn(() => Effect.promise(() => syncCompleted.promise));
    const store = createStore({ client, onStalePublish });
    const ref = providerRef();

    store.setContext(ref, 42, true);
    await Promise.resolve();
    const result = runAcknowledged((callbacks) => store.publish("approve", "summary", callbacks));

    await vi.waitFor(() => expect(onStalePublish).toHaveBeenCalledWith(ref, 42));
    expect(client.GET).toHaveBeenCalledTimes(1);
    syncCompleted.resolve();
    expect(await result).toBe(false);
    expect(client.GET).toHaveBeenCalledTimes(2);
    expect(store.getComments()).toEqual([{ id: "fresh", body: "fresh draft" }]);
    expect(store.getError()).toBe("review draft head is stale");
  });

  it("ignores draft loads from an older diff head", async () => {
    const oldLoad = deferred<MockDraftLoad>();
    const newLoad = deferred<MockDraftLoad>();
    const client = mockClient({
      GET: vi.fn().mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(newLoad.promise),
    });
    const store = createStore({ client });
    const ref = providerRef({
      provider: "github",
      platformHost: "github.com",
    });

    store.setContext(ref, 42, true, "old-head");
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(1));
    store.setContext(ref, 42, true, "new-head");
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(2));

    newLoad.resolve(
      draftLoad({
        comments: [{ id: "new", body: "new draft" }],
      }),
    );
    await vi.waitFor(() => expect(store.getComments()).toEqual([{ id: "new", body: "new draft" }]));
    oldLoad.resolve(
      draftLoad({
        comments: [{ id: "old", body: "old draft" }],
      }),
    );
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(store.getComments()).toEqual([{ id: "new", body: "new draft" }]);
    expect(store.isLoading()).toBe(false);
  });

  it("settles every draft mutation rejected before command acceptance", () => {
    const store = createStore({ client: mockClient() });
    const launches: Array<(onSettled: () => void) => void> = [
      (onSettled) =>
        store.createComment(
          "comment",
          { path: "src/main.ts", side: "right", line: 7, line_type: "add" },
          { onSettled },
        ),
      (onSettled) => store.editComment(draftComment(), "updated", { onSettled }),
      (onSettled) => store.deleteComment("draft-1", { onSettled }),
      (onSettled) => store.publish("comment", "summary", { onSettled }),
      (onSettled) => store.discard({ onSettled }),
      (onSettled) => store.setThreadResolved("thread-1", true, { onSettled }),
    ];

    for (const launch of launches) {
      const settled = vi.fn();
      launch(settled);
      expect(settled).toHaveBeenCalledOnce();
    }
  });

  it("patches a draft comment body while preserving its review range", async () => {
    const original = draftComment({ id: "draft-7", body: "before" });
    const updated = draftComment({
      id: "draft-7",
      body: "after",
      updated_at: "2026-03-30T14:02:00Z",
    });
    const client = mockClient({
      GET: vi
        .fn()
        .mockResolvedValueOnce(draftLoad({ comments: [original] }))
        .mockResolvedValueOnce(draftLoad({ comments: [updated] })),
      PATCH: mockPatch(mutation({ data: updated })),
    });
    const store = createStore({ client });
    const ref = providerRef();

    store.setContext(ref, 42, true);
    await vi.waitFor(() => expect(store.getComments()).toEqual([original]));
    const comment = store.getComments()[0];
    const settled = Promise.withResolvers<void>();

    const result = store.editComment(comment, "after", { onSettled: settled.resolve });

    expect(result).toBeUndefined();
    await settled.promise;
    expect(client.PATCH).toHaveBeenCalledWith(
      "/pulls/{provider}/{owner}/{name}/{number}/review-draft/comments/{draft_comment_id}",
      expect.objectContaining({
        params: {
          path: {
            provider: "forgejo",
            owner: "acme",
            name: "widgets",
            number: 42,
            draft_comment_id: "draft-7",
          },
        },
        body: {
          body: "after",
          range: {
            path: "src/main.ts",
            side: "right",
            line: 7,
            new_line: 7,
            line_type: "add",
            diff_head_sha: "head-sha",
          },
        },
      }),
    );
    await vi.waitFor(() => expect(store.getComments()).toEqual([updated]));
  });

  it("launches draft comment creation synchronously and refreshes after acknowledgement", async () => {
    const created = draftComment({ id: "draft-created", body: "new comment" });
    const client = mockClient({
      GET: vi
        .fn()
        .mockResolvedValueOnce(draftLoad())
        .mockResolvedValueOnce(draftLoad({ comments: [created] })),
      POST: mockPost(mutation({ data: created })),
    });
    const store = createStore({ client });
    store.setContext(providerRef(), 42, true);
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    const settled = Promise.withResolvers<void>();

    const result = store.createComment(
      "new comment",
      {
        path: "src/main.ts",
        side: "right",
        line: 7,
        line_type: "add",
      },
      { onSettled: settled.resolve },
    );

    expect(result).toBeUndefined();
    await settled.promise;
    await vi.waitFor(() => expect(store.getComments()).toEqual([created]));
  });

  it("launches draft comment deletion synchronously and refreshes after acknowledgement", async () => {
    const existing = draftComment();
    const client = mockClient({
      GET: vi
        .fn()
        .mockResolvedValueOnce(draftLoad({ comments: [existing] }))
        .mockResolvedValueOnce(draftLoad()),
    });
    const store = createStore({ client });
    store.setContext(providerRef(), 42, true);
    await vi.waitFor(() => expect(store.getComments()).toEqual([existing]));
    const settled = Promise.withResolvers<void>();

    const result = store.deleteComment(existing.id, { onSettled: settled.resolve });

    expect(result).toBeUndefined();
    await settled.promise;
    await vi.waitFor(() => expect(store.getComments()).toEqual([]));
  });

  it("launches thread resolution synchronously from the provider mutation queue", async () => {
    const client = mockClient();
    const store = createStore({ client });
    store.setRouteContext(providerRef(), 42);
    const settled = Promise.withResolvers<void>();

    const result = store.setThreadResolved("thread-1", true, { onSettled: settled.resolve });

    expect(result).toBeUndefined();
    await settled.promise;
    expect(client.POST).toHaveBeenCalledWith(
      "/pulls/{provider}/{owner}/{name}/{number}/review-threads/{thread_id}/resolve",
      expect.objectContaining({
        params: {
          path: {
            provider: "forgejo",
            owner: "acme",
            name: "widgets",
            number: 42,
            thread_id: "thread-1",
          },
        },
      }),
    );
  });

  it("surfaces partial publish status while clearing the draft", async () => {
    const client = mockClient({
      GET: mockGet(draftLoad({ nativeMultilineRanges: false })),
      POST: mockPost(
        mutation({
          data: { status: "partially_published" },
        }),
      ),
    });
    const onPublished = vi.fn(() => Effect.void);
    const store = createStore({ client, onPublished });
    const ref = providerRef({
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      owner: "group",
      name: "project",
      repoPath: "group/project",
    });

    store.setContext(ref, 7, true);
    await Promise.resolve();
    const ok = await runAcknowledged((callbacks) => store.publish("approve", "summary", callbacks));

    expect(ok).toBe(true);
    expect(store.getDraft()).toBeNull();
    expect(store.getWarning()).toBe(
      "Review was partially published. Some inline comments or the selected review action may not have been submitted.",
    );
    expect(onPublished).toHaveBeenCalledWith(ref, 7);
  });

  it("ignores an older same-PR load after publish refreshes the draft", async () => {
    const staleLoad = deferred<MockDraftLoad>();
    const client = mockClient({
      GET: vi.fn().mockReturnValueOnce(staleLoad.promise).mockResolvedValueOnce(draftLoad()),
    });
    const store = createStore({ client });

    store.setContext(providerRef(), 42, true);
    await Promise.resolve();

    await expect(runAcknowledged((callbacks) => store.publish("comment", "summary", callbacks))).resolves.toBe(true);
    await vi.waitFor(() => expect(store.getComments()).toEqual([]));

    staleLoad.resolve(
      draftLoad({
        comments: [{ id: "stale", body: "old draft" }],
      }),
    );
    await staleLoad.promise;
    await Promise.resolve();

    expect(store.getComments()).toEqual([]);
  });

  it("does not stay loading when a mutation fails during an in-flight load", async () => {
    const staleLoad = deferred<MockDraftLoad>();
    const client = mockClient({
      GET: vi.fn().mockReturnValueOnce(staleLoad.promise),
      POST: mockPost(failedMutation()),
    });
    const store = createStore({ client });

    store.setContext(providerRef(), 42, true);
    await Promise.resolve();
    expect(store.isLoading()).toBe(true);

    await expect(runAcknowledged((callbacks) => store.publish("comment", "summary", callbacks))).resolves.toBe(false);
    expect(store.isLoading()).toBe(false);

    staleLoad.resolve(
      draftLoad({
        comments: [{ id: "stale", body: "old draft" }],
      }),
    );
    await staleLoad.promise;
    await Promise.resolve();

    expect(store.isLoading()).toBe(false);
    expect(store.getComments()).toEqual([]);
  });

  it("does not stay loading when discard succeeds during an in-flight load", async () => {
    const staleLoad = deferred<MockDraftLoad>();
    const client = mockClient({
      GET: vi.fn().mockReturnValueOnce(staleLoad.promise),
    });
    const store = createStore({ client });

    store.setContext(providerRef(), 42, true);
    await Promise.resolve();
    expect(store.isLoading()).toBe(true);
    const settled = Promise.withResolvers<void>();

    const result = store.discard({ onSettled: settled.resolve });

    expect(result).toBeUndefined();
    await settled.promise;
    expect(store.isLoading()).toBe(false);

    staleLoad.resolve(
      draftLoad({
        comments: [{ id: "stale", body: "old draft" }],
      }),
    );
    await staleLoad.promise;
    await Promise.resolve();

    expect(store.isLoading()).toBe(false);
    expect(store.getComments()).toEqual([]);
  });
});
