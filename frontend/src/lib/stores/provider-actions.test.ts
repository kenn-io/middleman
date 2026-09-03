import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import type { OwnedAppRuntime } from "../app/runtime.js";
import type { ProblemBody } from "../api/problems.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { createDetailStore as createRuntimeDetailStore } from "./detail.svelte.js";

let runtime: OwnedAppRuntime | undefined;

function createDetailStore(options: { readonly client: GeneratedClient }) {
  runtime = makeTestAppRuntime(options.client);
  return createRuntimeDetailStore({ runtime });
}

beforeEach(() => {
  runtime = undefined;
});

afterEach(async () => {
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

const routeRef = {
  provider: "github",
  platformHost: "github.com",
  owner: "octo",
  name: "repo",
  repoPath: "octo/repo",
};

function detail() {
  return {
    repo_owner: "octo",
    repo_name: "repo",
    repo: {
      provider: "github",
      platform_host: "github.com",
      owner: "octo",
      name: "repo",
      repo_path: "octo/repo",
    },
    merge_request: { Number: 1 },
    events: [],
  };
}

function rejected(detail: string) {
  const error: ProblemBody = {
    code: "validationError",
    detail,
    status: 400,
    title: "Invalid request",
    type: "about:blank",
  };
  return { error, response: new Response(null, { status: 400 }) };
}

function conflict(detail: string) {
  const error: ProblemBody = {
    code: "conflict",
    detail,
    details: { reason: "conflict" },
    status: 409,
    title: "Pull request conflict",
    type: "about:blank",
  };
  return { error, response: new Response(null, { status: 409 }) };
}

describe("provider action mutations", () => {
  it("rejects a captured route that no longer matches the displayed pull request", async () => {
    const post = vi.fn();
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const failure = vi.fn();
    const settled = vi.fn();

    store.approvePull(
      { ...routeRef, provider: "gitlab", platformHost: "gitlab.example.com" },
      1,
      { body: "" },
      { onFailure: failure, onSettled: settled },
    );

    expect(post).not.toHaveBeenCalled();
    expect(failure).toHaveBeenCalledWith("The selected pull request changed before the provider action started.");
    expect(settled).toHaveBeenCalledOnce();
  });

  it("continues with a ready command after an acknowledged approval failure", async () => {
    const approve = Promise.withResolvers<ReturnType<typeof rejected>>();
    const post = vi
      .fn()
      .mockImplementationOnce(() => approve.promise)
      .mockResolvedValueOnce({ data: undefined, response: new Response(null, { status: 204 }) });
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const approvalSettled = Promise.withResolvers<void>();
    const readySettled = Promise.withResolvers<void>();
    const approvalFailure = vi.fn();
    const readySuccess = vi.fn();

    store.approvePull(
      routeRef,
      1,
      { body: "", expected_head_sha: "head-a" },
      {
        onFailure: approvalFailure,
        onSettled: approvalSettled.resolve,
      },
    );
    store.markPullReady(routeRef, 1, {
      onSuccess: readySuccess,
      onSettled: readySettled.resolve,
    });
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));

    approve.resolve(rejected("approval rejected"));
    await Promise.all([approvalSettled.promise, readySettled.promise]);

    expect(approvalFailure).toHaveBeenCalledWith("approval rejected");
    expect(readySuccess).toHaveBeenCalledOnce();
    expect(post).toHaveBeenCalledTimes(2);
    expect(post.mock.calls[1]?.[0]).toEqual(expect.stringContaining("/ready-for-review"));
  });

  it("acknowledges request changes before reconciling the pull request", async () => {
    const reconcile = Promise.withResolvers<{ data: ReturnType<typeof detail> }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: detail() })
      .mockImplementationOnce(() => reconcile.promise);
    const post = vi.fn().mockResolvedValue({
      data: { status: "changes_requested" },
      response: new Response(null, { status: 200 }),
    });
    const store = createDetailStore({
      client: {
        GET: get,
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const succeeded = vi.fn();
    const settled = Promise.withResolvers<void>();

    store.requestPullChanges(
      routeRef,
      1,
      { body: "Please update this.", expected_head_sha: "head-a" },
      { onSuccess: succeeded, onSettled: settled.resolve },
    );
    await settled.promise;

    expect(succeeded).toHaveBeenCalledOnce();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    expect(post).toHaveBeenCalledWith(
      expect.stringContaining("/request-changes"),
      expect.objectContaining({
        body: { body: "Please update this.", expected_head_sha: "head-a" },
      }),
    );
    reconcile.resolve({ data: detail() });
  });

  it("routes workflow approval and deferred merge through the ordered action key", async () => {
    const post = vi.fn().mockResolvedValue({
      data: { status: "ok" },
      response: new Response(null, { status: 200 }),
    });
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: post,
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const workflowsSettled = Promise.withResolvers<void>();
    const mergeSettled = Promise.withResolvers<void>();
    const mergeSucceeded = vi.fn();

    store.approvePullWorkflows(routeRef, 1, { onSettled: workflowsSettled.resolve });
    store.mergePull(
      routeRef,
      1,
      {
        commit_message: "",
        commit_title: "Merge pull request",
        expected_head_sha: "head-a",
        method: "squash",
      },
      true,
      { onSuccess: mergeSucceeded, onSettled: mergeSettled.resolve },
    );
    await Promise.all([workflowsSettled.promise, mergeSettled.promise]);

    expect(post.mock.calls.map(([path]) => path)).toEqual([
      expect.stringContaining("/approve-workflows"),
      expect.stringContaining("/merge/deferred"),
    ]);
    expect(mergeSucceeded).toHaveBeenCalledWith({ _tag: "Queued" });
  });

  it("delivers the typed merge problem for inline conflict presentation", async () => {
    const requestConflict = conflict("The pull request cannot be merged cleanly.");
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: vi.fn().mockResolvedValue(requestConflict),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const onProblem = vi.fn();
    const settled = Promise.withResolvers<void>();

    store.mergePull(routeRef, 1, { commit_message: "", commit_title: "Merge pull request", method: "merge" }, false, {
      onProblem,
      onSettled: settled.resolve,
    });
    await settled.promise;

    expect(onProblem).toHaveBeenCalledWith(requestConflict.error);
  });

  it("returns the workspace cleanup warning with a successful immediate merge", async () => {
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: vi.fn().mockResolvedValue({
          data: {
            merged: true,
            sha: "merge-sha",
            message: "merged",
            workspace_cleanup_warning: "workspace has uncommitted changes",
          },
          response: new Response(null, { status: 200 }),
        }),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const onSuccess = vi.fn();
    const settled = Promise.withResolvers<void>();

    store.mergePull(
      routeRef,
      1,
      { commit_message: "", commit_title: "Merge pull request", method: "merge", delete_workspace_id: "ws-1" },
      false,
      { onSuccess, onSettled: settled.resolve },
    );
    await settled.promise;

    expect(onSuccess).toHaveBeenCalledWith({
      _tag: "Merged",
      cleanupWarning: "workspace has uncommitted changes",
    });
  });

  it("reports a provider response that did not merge as a failed merge", async () => {
    const store = createDetailStore({
      client: {
        GET: vi.fn(async () => ({ data: detail() })),
        POST: vi.fn().mockResolvedValue({
          data: {
            merged: false,
            sha: "",
            message: "The pull request could not be merged",
          },
          response: new Response(null, { status: 200 }),
        }),
        PUT: vi.fn(),
        DELETE: vi.fn(),
      } as unknown as GeneratedClient,
    });
    store.loadDetail("octo", "repo", 1, { ...routeRef, sync: false });
    await vi.waitFor(() => expect(store.isDetailLoading()).toBe(false));
    const onSuccess = vi.fn();
    const onFailure = vi.fn();
    const settled = Promise.withResolvers<void>();

    store.mergePull(
      routeRef,
      1,
      { commit_message: "", commit_title: "Merge pull request", method: "merge", delete_workspace_id: "ws-1" },
      false,
      { onSuccess, onFailure, onSettled: settled.resolve },
    );
    await settled.promise;

    expect(onSuccess).not.toHaveBeenCalled();
    expect(onFailure).toHaveBeenCalledWith("The pull request could not be merged");
  });
});
