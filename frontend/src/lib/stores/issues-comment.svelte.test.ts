import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import type { OwnedAppRuntime } from "../app/runtime.js";
import {
  createIssuesStore as createRuntimeIssuesStore,
  type IssuesStore,
  type IssuesStoreOptions,
} from "./issues.svelte.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { runCommentMutationContract, type ContractCommentEvent } from "./comment-mutation-contract.js";
import { dismissFlash, getFlashes } from "./flash.svelte.js";

let runtime: OwnedAppRuntime | undefined;

type TestIssuesStoreOptions = Omit<IssuesStoreOptions, "runtime"> & { readonly client: GeneratedClient };

function createIssuesStore(options: TestIssuesStoreOptions) {
  const { client, ...storeOptions } = options;
  runtime = makeTestAppRuntime(client);
  return createRuntimeIssuesStore({ ...storeOptions, runtime });
}

async function loadIssueDetail(store: IssuesStore, ...args: Parameters<IssuesStore["loadIssueDetail"]>): Promise<void> {
  store.loadIssueDetail(...args);
  await vi.waitFor(() => expect(store.isIssueDetailLoading()).toBe(false));
}

function submitIssueComment(
  store: IssuesStore,
  owner: string,
  name: string,
  number: number,
  body: string,
): Promise<boolean> {
  const completion = Promise.withResolvers<boolean>();
  store.submitIssueComment(owner, name, number, body, {
    onSuccess: () => completion.resolve(true),
    onFailure: () => completion.resolve(false),
  });
  return completion.promise;
}

beforeEach(() => {
  runtime = undefined;
  for (const flash of getFlashes()) dismissFlash(flash.id);
});

afterEach(async () => {
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

const issueRef = {
  provider: "github",
  platformHost: "github.com",
  repoPath: "octo/repo",
};

interface MockIssueDetail {
  repo_owner: string;
  repo_name: string;
  repo: {
    provider: string;
    platform_host: string;
    owner: string;
    name: string;
    repo_path: string;
  };
  issue: { Number: number };
  events: unknown[];
}

function makeDetail(events: unknown[] = [], number = 1): MockIssueDetail {
  return {
    repo_owner: "octo",
    repo_name: "repo",
    repo: {
      provider: issueRef.provider,
      platform_host: issueRef.platformHost,
      owner: "octo",
      name: "repo",
      repo_path: issueRef.repoPath,
    },
    issue: { Number: number },
    events,
  };
}

runCommentMutationContract({
  name: "createIssuesStore",
  commentMemberPath: "/issues/{provider}/{owner}/{name}/{number}/comments/{comment_id}",
  syncPath: "/issues/{provider}/{owner}/{name}/{number}/sync",
  makeDetail,
  create(client) {
    const store = createIssuesStore({ client });
    return {
      load: (number, sync) => loadIssueDetail(store, "octo", "repo", number, { ...issueRef, sync }),
      snapshot: () => {
        const detail = store.getIssueDetail();
        return detail === null
          ? null
          : {
              number: detail.issue.Number,
              events: detail.events as ContractCommentEvent[],
            };
      },
      isSyncing: store.isIssueDetailSyncing,
      error: store.getIssueDetailError,
      submit: (number, body, callbacks) => store.submitIssueComment("octo", "repo", number, body, callbacks),
      edit: (number, commentID, body, callbacks) =>
        store.editIssueComment("octo", "repo", number, commentID, body, callbacks),
      delete: (number, commentID, callbacks) => store.deleteIssueComment("octo", "repo", number, commentID, callbacks),
    };
  },
});

describe("createIssuesStore submitIssueComment", () => {
  it("refreshes the issues list after posting a comment when on the issues page", async () => {
    const detailData = makeDetail();
    const getCalls: string[] = [];
    const client = {
      GET: vi.fn(async (path: string) => {
        getCalls.push(path);
        if (path === "/issues") return { data: [] };
        return { data: detailData };
      }),
      POST: vi.fn(async (path: string) => {
        if (path.includes("/sync")) return { data: detailData };
        if (path.includes("/comments")) return { data: { ID: 42 } };
        return { data: undefined };
      }),
      PUT: vi.fn(),
      DELETE: vi.fn(),
    } as unknown as GeneratedClient;

    const store = createIssuesStore({
      client,
      getPage: () => "issues",
    });

    await loadIssueDetail(store, "octo", "repo", 1, issueRef);
    // Drain the background syncIssueDetail fired by loadIssueDetail.
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    const listCallsBefore = getCalls.filter((p) => p === "/issues").length;

    await submitIssueComment(store, "octo", "repo", 1, "hi");
    // Drain the background syncIssueDetail fired by submitIssueComment.
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    await vi.waitFor(() => {
      const listCallsAfter = getCalls.filter((p) => p === "/issues").length;
      expect(listCallsAfter).toBeGreaterThan(listCallsBefore);
    });
  });

  it("does not refresh the issues list when on a different page", async () => {
    const detailData = makeDetail();
    const getCalls: string[] = [];
    const client = {
      GET: vi.fn(async (path: string) => {
        getCalls.push(path);
        return { data: detailData };
      }),
      POST: vi.fn(async (path: string) => {
        if (path.includes("/sync")) return { data: detailData };
        if (path.includes("/comments")) return { data: { ID: 42 } };
        return { data: undefined };
      }),
      PUT: vi.fn(),
      DELETE: vi.fn(),
    } as unknown as GeneratedClient;

    const store = createIssuesStore({
      client,
      getPage: () => "pulls",
    });

    await loadIssueDetail(store, "octo", "repo", 1, issueRef);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    await submitIssueComment(store, "octo", "repo", 1, "hi");
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(getCalls.some((p) => p === "/issues")).toBe(false);
  });
});
