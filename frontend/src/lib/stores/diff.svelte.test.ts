import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { GeneratedClient } from "../api/generated-api.js";
import type { OwnedAppRuntime } from "../app/runtime.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import {
  createDiffStore as createRuntimeDiffStore,
  type DiffStore,
  type DiffStoreOptions,
  type LoadWorkspaceDiffOptions,
  type WorkspaceDiffBase,
} from "./diff.svelte.js";
import type { DiffFile, DiffResult, FilesResult } from "../api/types.js";

const ownerRepoRef = {
  provider: "github",
  platformHost: "github.com",
  owner: "owner",
  name: "repo",
  repoPath: "owner/repo",
};

type TestClient = GeneratedClient;

let runtime: OwnedAppRuntime | undefined;

type TestDiffStoreOptions = Omit<DiffStoreOptions, "runtime"> & { readonly client: GeneratedClient };

function createDiffStore(options: TestDiffStoreOptions) {
  const { client, ...storeOptions } = options;
  runtime = makeTestAppRuntime(client);
  return createRuntimeDiffStore({ ...storeOptions, runtime });
}

async function loadDiff(store: DiffStore, ...args: Parameters<DiffStore["loadDiff"]>): Promise<void> {
  store.loadDiff(...args);
  await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));
}

function loadWorkspaceDiff(
  store: DiffStore,
  workspaceID: string,
  base: WorkspaceDiffBase,
  stacked = false,
  options: LoadWorkspaceDiffOptions = {},
): Promise<void> {
  const settled = Promise.withResolvers<void>();
  const result = store.loadWorkspaceDiff(workspaceID, base, stacked, options, { onSettled: settled.resolve });
  expect(result).toBeUndefined();
  return settled.promise;
}

function loadCommits(store: DiffStore): Promise<void> {
  const settled = Promise.withResolvers<void>();
  const result = store.loadCommits({}, { onSettled: settled.resolve });
  expect(result).toBeUndefined();
  return settled.promise;
}

function loadFilePreview(
  store: DiffStore,
  owner: string,
  name: string,
  number: number,
  path: string,
  side?: "old" | "new",
): Promise<FilePreview> {
  const settled = Promise.withResolvers<FilePreview>();
  const result = store.loadFilePreview(owner, name, number, path, side, {
    onSuccess: settled.resolve,
    onFailure: (message) => settled.reject(new Error(message)),
  });
  expect(result).toBeUndefined();
  return settled.promise;
}

interface TestGetOptions {
  params?: {
    path?: Record<string, string | number>;
    query?: Record<string, string | number | boolean | undefined>;
  };
  signal?: AbortSignal;
}

function makeDiffResult(files: string[]): DiffResult {
  const diffFiles = files.map((path) => makeDiffFile(path, 1, 1));
  return {
    stale: false,
    whitespace_only_count: 0,
    files: diffFiles,
  };
}

function makeFilesResult(
  files: string[],
  overrides: Partial<FilesResult & { whitespace_only_count: number }> = {},
): FilesResult {
  const diffFiles = files.map((path) => makeDiffFile(path, 0, 0));
  return {
    stale: false,
    whitespace_only_count: 0,
    files: diffFiles,
    ...overrides,
  };
}

function makeDiffFile(path: string, additions: number, deletions: number): DiffFile {
  return {
    path,
    old_path: path,
    status: "modified",
    is_binary: false,
    is_generated: path === "bun.lock",
    is_whitespace_only: false,
    additions,
    deletions,
    hunks: [],
    patch: "",
  };
}

function makeFilePreview(path: string, content: string): FilePreview {
  return {
    path,
    content,
    encoding: "base64",
    media_type: "text/plain",
    size: content.length,
  };
}

function testClient(): TestClient {
  return {
    GET: vi.fn(async (path: string, options?: TestGetOptions) => {
      const response = await globalThis.fetch(
        testURL(path, options),
        options?.signal ? { signal: options.signal } : undefined,
      );
      if (!response.ok) {
        return {
          error: await response.json().catch(() => ({})),
          response,
        };
      }
      return {
        data: await response.json(),
        response,
      };
    }),
  } as unknown as TestClient;
}

function testURL(path: string, options?: TestGetOptions): string {
  let url = `/api/v1${path}`;
  for (const [key, value] of Object.entries(options?.params?.path ?? {})) {
    url = url.replace(`{${key}}`, encodeURIComponent(String(value)));
  }
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(options?.params?.query ?? {})) {
    if (value !== undefined) query.set(key, String(value));
  }
  const qs = query.toString();
  return qs ? `${url}?${qs}` : url;
}

beforeEach(() => {
  runtime = undefined;
});

afterEach(async () => {
  vi.restoreAllMocks();
  localStorage.removeItem("diff-hide-whitespace");
  localStorage.removeItem("diff-tab-width");
  localStorage.removeItem("diff-collapsed-files");
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
});

describe("createDiffStore loadDiff", () => {
  it("uses the generated client owned by the application runtime", async () => {
    vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("store-local transport must not run"));
    const result = makeDiffResult(["src/runtime.ts"]);
    const client = {
      GET: vi.fn(async () => ({ data: result, response: new Response(null, { status: 200 }) })),
    } as unknown as GeneratedClient;
    const store = createDiffStore({ client });

    await loadDiff(store, "owner", "repo", 7, ownerRepoRef);

    expect(store.getDiff()?.files.map((file) => file.path)).toEqual(["src/runtime.ts"]);
  });

  it("does not satisfy concurrent full demand with a diff-only refresh", async () => {
    const calls: string[] = [];
    const diff = makeDiffResult(["src/app.ts"]);
    const files = makeFilesResult(["src/app.ts"]);
    let diffCalls = 0;
    let releaseDiffOnly = () => {};
    const diffOnly = new Promise<Response>((resolve) => {
      releaseDiffOnly = () => resolve(Response.json(diff));
    });
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/files")) return Response.json(files);
      if (url.includes("/diff")) {
        diffCalls += 1;
        if (diffCalls === 2) return diffOnly;
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });
    const store = createDiffStore({ client: testClient() });

    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);
    store.setHideWhitespace(true);
    await vi.waitFor(() => expect(diffCalls).toBe(2));
    store.loadDiff("owner", "repo", 1, ownerRepoRef);
    await vi.waitFor(() => expect(calls.filter((url) => url.includes("/files"))).toHaveLength(2));
    releaseDiffOnly();
    await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));

    expect(calls.filter((url) => url.includes("/files"))).toHaveLength(2);
  });

  it("loads default branch commit diffs through the repo route", async () => {
    const calls: string[] = [];
    const diff = makeDiffResult(["internal/cache.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/repo/github/owner/repo/commits/abc123/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    const settled = Promise.withResolvers<void>();

    const result = store.loadCommitDiff(ownerRepoRef, "abc123", { onSettled: settled.resolve });

    expect(result).toBeUndefined();
    await settled.promise;
    expect(calls).toEqual(["/api/v1/repo/github/owner/repo/commits/abc123/diff"]);
    expect(store.getDiff()?.files[0]?.path).toBe("internal/cache.go");
  });

  it("refetches default branch commit diffs when toggling whitespace hiding", async () => {
    const calls: string[] = [];
    const diffAll = makeDiffResult(["a.ts", "b.ts"]);
    const diffHidden = makeDiffResult(["a.ts"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("whitespace=hide")) {
        return Response.json(diffHidden);
      }
      if (url.includes("/repo/github/owner/repo/commits/abc123/diff")) {
        return Response.json(diffAll);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    const settled = Promise.withResolvers<void>();
    store.loadCommitDiff(ownerRepoRef, "abc123", { onSettled: settled.resolve });
    await settled.promise;
    store.setHideWhitespace(true);

    await vi.waitFor(() => {
      expect(store.getDiff()?.files).toHaveLength(1);
    });
    expect(calls).toContain("/api/v1/repo/github/owner/repo/commits/abc123/diff?whitespace=hide");
  });

  it("loads workspace files and the full workspace diff", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go", "src/app_test.go", "docs/plan.md"]);
    const diff = makeDiffResult(["src/app.go", "src/app_test.go", "docs/plan.md"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    await loadWorkspaceDiff(store, "ws-1", "pushed");

    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=pushed");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=pushed");
    expect(store.getVisibleDiffFiles().map((file) => file.path)).toEqual([
      "src/app.go",
      "src/app_test.go",
      "docs/plan.md",
    ]);
    expect(store.getFileCategoryCounts()).toEqual({
      all: 3,
      plansDocs: 1,
      generated: 0,
      code: 1,
      tests: 1,
      other: 0,
    });
  });

  it("keeps a coherent workspace diff visible during a preserving refresh", async () => {
    const filesA = makeFilesResult(["a.ts"], { snapshot_version: "generation:1" });
    const diffA = { ...makeDiffResult(["a.ts"]), snapshot_version: "generation:1" };
    const filesB = makeFilesResult(["b.ts"], { snapshot_version: "generation:2" });
    const diffB = { ...makeDiffResult(["b.ts"]), snapshot_version: "generation:2" };
    let filesCalls = 0;
    let diffCalls = 0;
    let signalDiffStarted: () => void = () => {};
    let releaseDiff: () => void = () => {};
    const diffStarted = new Promise<void>((resolve) => {
      signalDiffStarted = resolve;
    });
    const diffGate = new Promise<void>((resolve) => {
      releaseDiff = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        return Response.json(filesCalls === 1 ? filesA : filesB);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        diffCalls += 1;
        if (diffCalls === 2) {
          signalDiffStarted();
          await diffGate;
        }
        return Response.json(diffCalls === 1 ? diffA : diffB);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    const refresh = loadWorkspaceDiff(store, "ws-1", "head", false, { preserveVisible: true });
    await diffStarted;
    expect(store.getFileList()?.files[0]?.path).toBe("a.ts");
    expect(store.getDiff()?.files[0]?.path).toBe("a.ts");

    releaseDiff();
    await refresh;
    expect(store.getFileList()?.files[0]?.path).toBe("b.ts");
    expect(store.getDiff()?.files[0]?.path).toBe("b.ts");
  });

  it("keeps retrying a preserving workspace refresh until both responses are fresh", async () => {
    vi.useFakeTimers();
    try {
      let filesCalls = 0;
      let diffCalls = 0;
      let signalStalePair: () => void = () => {};
      const stalePairSeen = new Promise<void>((resolve) => {
        signalStalePair = resolve;
      });

      vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes("/workspaces/ws-1/files")) {
          filesCalls += 1;
          return Response.json(
            makeFilesResult(["a.ts"], {
              stale: filesCalls === 2,
              snapshot_version: "generation:1",
            }),
          );
        }
        if (url.includes("/workspaces/ws-1/diff")) {
          diffCalls += 1;
          if (diffCalls === 2) signalStalePair();
          return Response.json({
            ...makeDiffResult(["a.ts"]),
            stale: diffCalls === 2,
            snapshot_version: "generation:1",
          });
        }
        return Response.json({}, { status: 404 });
      });

      const store = createDiffStore({ client: testClient() });
      await loadWorkspaceDiff(store, "ws-1", "head");
      const refresh = loadWorkspaceDiff(store, "ws-1", "head", false, { preserveVisible: true });
      await stalePairSeen;
      await vi.advanceTimersByTimeAsync(0);

      expect(store.getDiff()?.stale).toBe(false);

      await vi.runOnlyPendingTimersAsync();
      await refresh;
      expect(store.getDiff()?.stale).toBe(false);
      expect(filesCalls).toBe(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("retries a workspace diff pair when its pinned revision changes", async () => {
    const calls: string[] = [];
    let filesCalls = 0;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        return Response.json(
          makeFilesResult([filesCalls === 1 ? "old.ts" : "new.ts"], {
            snapshot_version: `generation:${filesCalls}`,
          }),
        );
      }
      if (url.includes("revision=generation%3A1")) {
        return Response.json(
          { code: "conflict", detail: "snapshot changed", details: { reason: "snapshot_changed" } },
          { status: 409 },
        );
      }
      if (url.includes("revision=generation%3A2")) {
        return Response.json({ ...makeDiffResult(["new.ts"]), snapshot_version: "generation:2" });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    expect(filesCalls).toBe(2);
    expect(store.getFileList()?.files[0]?.path).toBe("new.ts");
    expect(store.getDiff()?.files[0]?.path).toBe("new.ts");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=head&revision=generation%3A1");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=head&revision=generation%3A2");
  });

  it("loads remote workspace files, diff, commits, and previews through the fleet host route", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/remote.go"], { snapshot_version: "generation:1" });
    const diff = { ...makeDiffResult(["src/remote.go"]), snapshot_version: "generation:1" };

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/fleet/hosts/member/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [
            {
              sha: "sha2",
              message: "remote second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/fleet/hosts/member/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/fleet/hosts/member/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      if (url.includes("/fleet/hosts/member/workspaces/ws-1/file-preview")) {
        return Response.json(makeFilePreview("src/remote.go", "package remote"));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    await loadWorkspaceDiff(store, "ws-1", "merge-target", false, { workspaceHostKey: "member" });
    await loadCommits(store);
    await loadFilePreview(store, "owner", "repo", 1, "src/remote.go", "new");

    expect(calls).toContain("/api/v1/fleet/hosts/member/workspaces/ws-1/files?base=merge-target");
    expect(calls).toContain(
      "/api/v1/fleet/hosts/member/workspaces/ws-1/diff?base=merge-target&revision=generation%3A1",
    );
    expect(calls).toContain("/api/v1/fleet/hosts/member/workspaces/ws-1/commits");
    expect(calls).toContain(
      "/api/v1/fleet/hosts/member/workspaces/ws-1/file-preview?base=merge-target&path=src%2Fremote.go&side=new&revision=generation%3A1",
    );
    expect(calls.some((url) => url.includes("/api/v1/workspaces/ws-1/"))).toBe(false);
  });

  it("requests both sides of a renamed pull file through its current path", async () => {
    const calls: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      return Response.json(makeFilePreview("src/current.ts", "const renamed = true;"));
    });
    const store = createDiffStore({ client: testClient() });
    const settled = Promise.withResolvers<void>();

    const result = store.loadFileContextPreviews(
      "owner",
      "repo",
      1,
      {
        ...makeDiffFile("src/current.ts", 1, 1),
        old_path: "src/previous.ts",
        status: "renamed",
      },
      { onSettled: settled.resolve },
    );
    expect(result).toBeUndefined();
    await settled.promise;

    const previewPaths = calls.map((url) => {
      const parsed = new URL(url, "http://kenn-forge.test");
      return { path: parsed.searchParams.get("path"), side: parsed.searchParams.get("side") };
    });
    expect(previewPaths).toContainEqual({ path: "src/current.ts", side: "old" });
    expect(previewPaths).toContainEqual({ path: "src/current.ts", side: "new" });
    expect(previewPaths.some(({ path }) => path === "src/previous.ts")).toBe(false);
  });

  it("reloads the coherent workspace snapshot and retries a conflicted preview once", async () => {
    const calls: string[] = [];
    let filesCalls = 0;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        return Response.json(makeFilesResult(["src/app.go"], { snapshot_version: `generation:${filesCalls}` }));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json({ ...makeDiffResult(["src/app.go"]), snapshot_version: `generation:${filesCalls}` });
      }
      if (url.includes("revision=generation%3A1")) {
        return Response.json(
          { code: "conflict", detail: "snapshot changed", details: { reason: "snapshot_changed" } },
          { status: 409 },
        );
      }
      if (url.includes("revision=generation%3A2")) {
        return Response.json(makeFilePreview("src/app.go", "package refreshed"));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    const preview = await loadFilePreview(store, "owner", "repo", 1, "src/app.go", "new");

    expect(preview.content).toBe("package refreshed");
    expect(filesCalls).toBe(2);
    expect(calls).toContain(
      "/api/v1/workspaces/ws-1/file-preview?base=head&path=src%2Fapp.go&side=new&revision=generation%3A1",
    );
    expect(calls).toContain(
      "/api/v1/workspaces/ws-1/file-preview?base=head&path=src%2Fapp.go&side=new&revision=generation%3A2",
    );
  });

  it("rejects stale preview recovery after a newer same-workspace load takes ownership", async () => {
    let filesCalls = 0;
    let releasePreview: () => void = () => {};
    let signalPreviewStarted: () => void = () => {};
    const previewStarted = new Promise<void>((resolve) => {
      signalPreviewStarted = resolve;
    });
    const previewGate = new Promise<void>((resolve) => {
      releasePreview = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        const path = filesCalls === 1 ? "original.ts" : filesCalls === 2 ? "replacement.ts" : "stale.ts";
        return Response.json(makeFilesResult([path], { snapshot_version: `generation:${filesCalls}` }));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        const path = filesCalls === 1 ? "original.ts" : filesCalls === 2 ? "replacement.ts" : "stale.ts";
        return Response.json({ ...makeDiffResult([path]), snapshot_version: `generation:${filesCalls}` });
      }
      if (url.includes("/workspaces/ws-1/file-preview")) {
        signalPreviewStarted();
        await previewGate;
        return Response.json(
          { code: "conflict", detail: "snapshot changed", details: { reason: "snapshot_changed" } },
          { status: 409 },
        );
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    const initialToken = {};
    const replacementToken = {};
    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: initialToken });
    const preview = loadFilePreview(store, "owner", "repo", 1, "original.ts", "new");
    await previewStarted;

    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: replacementToken });
    releasePreview();

    await expect(preview).rejects.toThrow("Workspace changed while refreshing file preview");
    expect(store.getDiff()?.files[0]?.path).toBe("replacement.ts");
    expect(filesCalls).toBe(2);
  });

  it("rejects preview recovery superseded while its workspace reload is pending", async () => {
    let filesCalls = 0;
    let previewCalls = 0;
    let releaseRecovery: () => void = () => {};
    let signalRecoveryStarted: () => void = () => {};
    const recoveryStarted = new Promise<void>((resolve) => {
      signalRecoveryStarted = resolve;
    });
    const recoveryGate = new Promise<void>((resolve) => {
      releaseRecovery = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        if (filesCalls === 2) {
          signalRecoveryStarted();
          await recoveryGate;
        }
        return Response.json(makeFilesResult(["original.ts"], { snapshot_version: `generation:${filesCalls}` }));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json({
          ...makeDiffResult(["original.ts"]),
          snapshot_version: `generation:${filesCalls}`,
        });
      }
      if (url.includes("/workspaces/ws-1/file-preview")) {
        previewCalls += 1;
        if (previewCalls === 1) {
          return Response.json(
            { code: "conflict", detail: "snapshot changed", details: { reason: "snapshot_changed" } },
            { status: 409 },
          );
        }
        return Response.json(makeFilePreview("original.ts", "superseded retry"));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    const initialToken = {};
    const replacementToken = {};
    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: initialToken });
    const preview = loadFilePreview(store, "owner", "repo", 1, "original.ts", "new");
    await recoveryStarted;

    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: replacementToken });
    releaseRecovery();

    await expect(preview).rejects.toThrow("Workspace changed while refreshing file preview");
    expect(previewCalls).toBe(1);
    expect(store.getDiff()?.snapshot_version).toBe("generation:3");
  });

  it("rejects a recovered preview superseded while its retry is pending", async () => {
    let filesCalls = 0;
    let previewCalls = 0;
    let releaseRetry: () => void = () => {};
    let signalRetryStarted: () => void = () => {};
    const retryStarted = new Promise<void>((resolve) => {
      signalRetryStarted = resolve;
    });
    const retryGate = new Promise<void>((resolve) => {
      releaseRetry = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        return Response.json(makeFilesResult(["original.ts"], { snapshot_version: `generation:${filesCalls}` }));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json({
          ...makeDiffResult(["original.ts"]),
          snapshot_version: `generation:${filesCalls}`,
        });
      }
      if (url.includes("/workspaces/ws-1/file-preview")) {
        previewCalls += 1;
        if (previewCalls === 1) {
          return Response.json(
            { code: "conflict", detail: "snapshot changed", details: { reason: "snapshot_changed" } },
            { status: 409 },
          );
        }
        signalRetryStarted();
        await retryGate;
        return Response.json(makeFilePreview("original.ts", "superseded retry"));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    const loadToken = {};
    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken });
    const preview = loadFilePreview(store, "owner", "repo", 1, "original.ts", "new");
    await retryStarted;

    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken });
    releaseRetry();

    await expect(preview).rejects.toThrow("Workspace changed while refreshing file preview");
    expect(previewCalls).toBe(2);
    expect(store.getDiff()?.snapshot_version).toBe("generation:3");
  });

  it("loads workspace diffs against the merge target", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    await loadWorkspaceDiff(store, "ws-1", "merge-target");

    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target");
  });

  it("collapses and expands all visible files in workspace diffs", async () => {
    const files = makeFilesResult(["src/app.go", "src/app_test.go", "docs/plan.md"]);
    const diff = makeDiffResult(["src/app.go", "src/app_test.go", "docs/plan.md"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    expect(store.areAllVisibleFilesCollapsed()).toBe(false);

    store.setAllVisibleFilesCollapsed(true);

    expect(store.areAllVisibleFilesCollapsed()).toBe(true);
    expect(store.isFileCollapsed("owner", "repo", 1, "src/app.go")).toBe(true);
    expect(store.isFileCollapsed("owner", "repo", 1, "src/app_test.go")).toBe(true);
    expect(store.isFileCollapsed("owner", "repo", 1, "docs/plan.md")).toBe(true);

    store.setFileCategoryFilter("tests");
    store.setAllVisibleFilesCollapsed(false);

    expect(store.isFileCollapsed("owner", "repo", 1, "src/app.go")).toBe(true);
    expect(store.isFileCollapsed("owner", "repo", 1, "src/app_test.go")).toBe(false);
    expect(store.isFileCollapsed("owner", "repo", 1, "docs/plan.md")).toBe(true);
  });

  it("loads commits for the active workspace diff", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [
            {
              sha: "sha2",
              message: "second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
            {
              sha: "sha1",
              message: "first",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");
    await loadCommits(store);

    expect(calls).toContain("/api/v1/workspaces/ws-1/commits");
    expect(store.getCommits()?.map((commit) => commit.sha)).toEqual(["sha2", "sha1"]);
  });

  it("applies selected commit scope to workspace files and patch requests", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [
            {
              sha: "sha2",
              message: "second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
            {
              sha: "sha1",
              message: "first",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);

    store.selectCommit("sha2");

    await vi.waitFor(() => {
      expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target&commit=sha2");
      expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&commit=sha2");
    });
  });

  it("refreshes workspace commits without resetting the selected scope", async () => {
    const calls: string[] = [];
    const commitResponses = [
      [
        {
          sha: "sha2",
          message: "second",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
        {
          sha: "sha1",
          message: "first",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
      [
        {
          sha: "sha3",
          message: "third",
          author_name: "Alice",
          authored_at: "2026-01-02T00:00:00Z",
        },
        {
          sha: "sha2",
          message: "second",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
    ];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: commitResponses.shift() ?? [],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);
    store.selectCommit("sha2");

    await vi.waitFor(() => {
      expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&commit=sha2");
    });

    calls.length = 0;
    await loadWorkspaceDiff(store, "ws-1", "merge-target", false, { refreshCommits: true });

    expect(calls).toContain("/api/v1/workspaces/ws-1/commits");
    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target&commit=sha2");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&commit=sha2");
    expect(store.getCommits()?.map((commit) => commit.sha)).toEqual(["sha3", "sha2"]);
    expect(store.getScope()).toEqual({ kind: "commit", sha: "sha2" });
  });

  it("keeps workspace preview generation stable until refreshed diff load starts", async () => {
    const calls: string[] = [];
    let resolveRefreshCommits: () => void = () => {};
    let refreshCommitsStarted: () => void = () => {};
    const refreshCommitsStartedPromise = new Promise<void>((resolve) => {
      refreshCommitsStarted = resolve;
    });
    const refreshCommitsGate = new Promise<void>((resolve) => {
      resolveRefreshCommits = resolve;
    });
    const commitResponses = [
      [
        {
          sha: "sha2",
          message: "second",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
      [
        {
          sha: "sha2",
          message: "second",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
    ];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);
    let store: ReturnType<typeof createDiffStore>;
    let generationAtRefreshedFilesRequest = -1;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        if (commitResponses.length === 1) {
          refreshCommitsStarted();
          await refreshCommitsGate;
        }
        return Response.json({
          commits: commitResponses.shift() ?? [],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        if (calls.includes("/api/v1/workspaces/ws-1/commits")) {
          generationAtRefreshedFilesRequest = store.getFilePreviewGeneration();
        }
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);
    const generationBeforeRefresh = store.getFilePreviewGeneration();

    const refresh = loadWorkspaceDiff(store, "ws-1", "merge-target", false, { refreshCommits: true });
    await refreshCommitsStartedPromise;

    expect(store.getFilePreviewGeneration()).toBe(generationBeforeRefresh);

    resolveRefreshCommits();
    await refresh;

    expect(generationAtRefreshedFilesRequest).toBe(generationBeforeRefresh + 1);
    expect(store.getFilePreviewGeneration()).toBe(generationBeforeRefresh + 1);
  });

  it("drops a workspace refresh when the fleet host changes during commit refresh", async () => {
    const calls: string[] = [];
    let memberACommitRequests = 0;
    let resolveRefreshCommits: () => void = () => {};
    let refreshCommitsStarted: () => void = () => {};
    const refreshCommitsStartedPromise = new Promise<void>((resolve) => {
      refreshCommitsStarted = resolve;
    });
    const refreshCommitsGate = new Promise<void>((resolve) => {
      resolveRefreshCommits = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/fleet/hosts/member-a/workspaces/ws-1/commits")) {
        memberACommitRequests += 1;
        if (memberACommitRequests === 2) {
          refreshCommitsStarted();
          await refreshCommitsGate;
        }
        return Response.json({
          commits: [
            {
              sha: "sha2",
              message: "second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/fleet/hosts/member-a/workspaces/ws-1/files")) {
        return Response.json(makeFilesResult(["member-a.ts"]));
      }
      if (url.includes("/fleet/hosts/member-a/workspaces/ws-1/diff")) {
        return Response.json(makeDiffResult(["member-a.ts"]));
      }
      if (url.includes("/fleet/hosts/member-b/workspaces/ws-1/files")) {
        return Response.json(makeFilesResult(["member-b.ts"]));
      }
      if (url.includes("/fleet/hosts/member-b/workspaces/ws-1/diff")) {
        return Response.json(makeDiffResult(["member-b.ts"]));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target", false, { workspaceHostKey: "member-a" });
    await loadCommits(store);
    calls.length = 0;

    const staleRefresh = loadWorkspaceDiff(store, "ws-1", "merge-target", false, {
      workspaceHostKey: "member-a",
      refreshCommits: true,
    });
    await refreshCommitsStartedPromise;

    await loadWorkspaceDiff(store, "ws-1", "merge-target", false, { workspaceHostKey: "member-b" });
    expect(store.getDiff()?.files[0]?.path).toBe("member-b.ts");

    resolveRefreshCommits();
    await staleRefresh;

    expect(calls).toContain("/api/v1/fleet/hosts/member-a/workspaces/ws-1/commits");
    expect(calls).toContain("/api/v1/fleet/hosts/member-b/workspaces/ws-1/files?base=merge-target");
    expect(calls).toContain("/api/v1/fleet/hosts/member-b/workspaces/ws-1/diff?base=merge-target");
    expect(calls).not.toContain("/api/v1/fleet/hosts/member-a/workspaces/ws-1/files?base=merge-target");
    expect(calls).not.toContain("/api/v1/fleet/hosts/member-a/workspaces/ws-1/diff?base=merge-target");
    expect(store.getDiff()?.files[0]?.path).toBe("member-b.ts");
  });

  it("does not resume a canceled workspace load after commit refresh", async () => {
    const calls: string[] = [];
    let commitCalls = 0;
    let releaseRefresh: () => void = () => {};
    let signalRefresh: () => void = () => {};
    const refreshStarted = new Promise<void>((resolve) => {
      signalRefresh = resolve;
    });
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        commitCalls += 1;
        if (commitCalls === 2) {
          signalRefresh();
          await refreshGate;
        }
        return Response.json({ commits: [] });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(makeFilesResult(["a.ts"]));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(makeDiffResult(["a.ts"]));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");
    await loadCommits(store);
    calls.length = 0;

    const refresh = loadWorkspaceDiff(store, "ws-1", "head", false, { refreshCommits: true });
    await refreshStarted;
    store.cancelWorkspaceDiff("ws-1");
    releaseRefresh();
    await refresh;

    expect(calls).toEqual(["/api/v1/workspaces/ws-1/commits"]);
    expect(store.isDiffLoading()).toBe(false);
  });

  it("does not let a superseded same-workspace owner cancel the replacement load", async () => {
    const tokenA = {};
    const tokenB = {};
    let commitCalls = 0;
    let filesCalls = 0;
    let releaseCommitRefresh: () => void = () => {};
    let signalCommitRefresh: () => void = () => {};
    const commitRefreshStarted = new Promise<void>((resolve) => {
      signalCommitRefresh = resolve;
    });
    const commitRefreshGate = new Promise<void>((resolve) => {
      releaseCommitRefresh = resolve;
    });

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/commits")) {
        commitCalls += 1;
        if (commitCalls === 2) {
          signalCommitRefresh();
          await commitRefreshGate;
        }
        return Response.json({ commits: [] });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        filesCalls += 1;
        return Response.json(makeFilesResult([filesCalls === 1 ? "original.ts" : "replacement.ts"]));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(makeDiffResult([filesCalls === 1 ? "original.ts" : "replacement.ts"]));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: tokenA });
    await loadCommits(store);

    const replacement = loadWorkspaceDiff(store, "ws-1", "head", false, {
      loadToken: tokenB,
      refreshCommits: true,
    });
    await commitRefreshStarted;
    store.cancelWorkspaceDiff("ws-1", undefined, tokenA);
    releaseCommitRefresh();
    await replacement;

    expect(store.getDiff()?.files[0]?.path).toBe("replacement.ts");
    expect(store.getDiffError()).toBeNull();
  });

  it("keeps the workspace owner token through a commit-scope reload", async () => {
    const token = {};
    let scopeSignal: AbortSignal | undefined;
    let signalScopeRequest: () => void = () => {};
    const scopeRequestStarted = new Promise<void>((resolve) => {
      signalScopeRequest = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [{ sha: "sha2", message: "second", author_name: "Alice" }],
        });
      }
      if (url.includes("commit=sha2")) {
        scopeSignal = init?.signal ?? undefined;
        signalScopeRequest();
        return new Promise<Response>(() => {});
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(makeFilesResult(["original.ts"]));
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(makeDiffResult(["original.ts"]));
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head", false, { loadToken: token });
    await loadCommits(store);
    store.selectCommit("sha2");
    await scopeRequestStarted;

    store.cancelWorkspaceDiff("ws-1", undefined, token);

    expect(scopeSignal?.aborted).toBe(true);
  });

  it("resets workspace commit scope when refresh removes the selected commit", async () => {
    const calls: string[] = [];
    const commitResponses = [
      [
        {
          sha: "sha2",
          message: "second",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
      [
        {
          sha: "sha3",
          message: "third",
          author_name: "Alice",
          authored_at: "2026-01-02T00:00:00Z",
        },
      ],
    ];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: commitResponses.shift() ?? [],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);
    store.selectCommit("sha2");

    await vi.waitFor(() => {
      expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&commit=sha2");
    });

    calls.length = 0;
    await loadWorkspaceDiff(store, "ws-1", "merge-target", false, { refreshCommits: true });

    expect(calls).toContain("/api/v1/workspaces/ws-1/commits");
    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target");
    expect(calls.some((url) => url.includes("commit=sha2"))).toBe(false);
    expect(store.getCommits()?.map((commit) => commit.sha)).toEqual(["sha3"]);
    expect(store.getScope()).toEqual({ kind: "head" });
  });

  it("applies selected range scope to workspace files and patch requests", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [
            {
              sha: "sha3",
              message: "third",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
            {
              sha: "sha2",
              message: "second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
            {
              sha: "sha1",
              message: "first",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);

    store.selectRange("sha3", "sha2");

    await vi.waitFor(() => {
      expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target&from=sha2&to=sha3");
      expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&from=sha2&to=sha3");
    });
  });

  it("preserves workspace scope when switching between single-file and stacked diffs", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["src/app.go"]);
    const diff = makeDiffResult(["src/app.go"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);
      if (url.includes("/workspaces/ws-1/commits")) {
        return Response.json({
          commits: [
            {
              sha: "sha2",
              message: "second",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
            {
              sha: "sha1",
              message: "first",
              author_name: "Alice",
              authored_at: "2026-01-01T00:00:00Z",
            },
          ],
        });
      }
      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "merge-target");
    await loadCommits(store);

    store.selectCommit("sha2");

    await vi.waitFor(() => {
      expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target&commit=sha2");
    });

    calls.length = 0;
    await loadWorkspaceDiff(store, "ws-1", "merge-target", true);

    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=merge-target&commit=sha2");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=merge-target&commit=sha2");
    expect(store.getScope()).toEqual({ kind: "commit", sha: "sha2" });
  });

  it("refetches workspace files when toggling whitespace hiding", async () => {
    const calls: string[] = [];
    const filesAll = makeFilesResult(["a.ts", "whitespace.ts"]);
    const filesHidden = makeFilesResult(["a.ts"]);
    const diffAll = makeDiffResult(["a.ts", "whitespace.ts"]);
    const diffHidden = makeDiffResult(["a.ts"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);

      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(url.includes("whitespace=hide") ? filesHidden : filesAll);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(url.includes("whitespace=hide") ? diffHidden : diffAll);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    expect(store.getFileList()?.files.map((file) => file.path)).toEqual(["a.ts", "whitespace.ts"]);

    store.setHideWhitespace(true);
    await vi.waitFor(() => {
      expect(store.getFileList()?.files.map((file) => file.path)).toEqual(["a.ts"]);
    });
    await vi.waitFor(() => {
      expect(store.isDiffLoading()).toBe(false);
    });

    expect(calls).toContain("/api/v1/workspaces/ws-1/files?base=head&whitespace=hide");
    expect(calls).toContain("/api/v1/workspaces/ws-1/diff?base=head&whitespace=hide");
  });

  it("scrolls to workspace files without reloading the diff", async () => {
    const calls: string[] = [];
    const files = makeFilesResult(["a.ts", "b.ts"]);
    const diff = makeDiffResult(["a.ts", "b.ts"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push(url);

      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    store.requestScrollToFile("b.ts");

    expect(store.getActiveFile()).toBe("b.ts");
    expect(store.getScrollTarget()).toEqual({ path: "b.ts" });
    expect(calls.filter((url) => url.includes("/api/v1/workspaces/ws-1/diff"))).toEqual([
      "/api/v1/workspaces/ws-1/diff?base=head",
    ]);
  });

  it("reveals active files only for explicit diff navigation", async () => {
    const store = createDiffStore({ client: testClient() });

    expect(store.getActiveFileRevealKey()).toBe(0);

    store.setActiveFile("a.ts");

    expect(store.getActiveFile()).toBe("a.ts");
    expect(store.getActiveFileRevealKey()).toBe(0);

    store.requestScrollToFile("b.ts");

    expect(store.getActiveFile()).toBe("b.ts");
    expect(store.getActiveFileRevealKey()).toBe(1);

    store.requestScrollToLine("c.ts", 12);

    expect(store.getActiveFile()).toBe("c.ts");
    expect(store.getActiveFileRevealKey()).toBe(2);
  });

  it("uses the workspace diff whitespace count", async () => {
    const files = makeFilesResult(["a.ts", "whitespace.ts"], {
      whitespace_only_count: 7,
    });
    const diff = makeDiffResult(["a.ts", "whitespace.ts"]);
    diff.whitespace_only_count = 7;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json(files);
      }
      if (url.includes("/workspaces/ws-1/diff")) {
        return Response.json(diff);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    expect(store.getDiff()?.whitespace_only_count).toBe(7);
  });

  it("clears workspace diff loading when the file list fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/workspaces/ws-1/files")) {
        return Response.json({ title: "workspace files failed" }, { status: 502 });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadWorkspaceDiff(store, "ws-1", "head");

    expect(store.isDiffLoading()).toBe(false);
    expect(store.getDiffError()).toBe("workspace files failed");
  });

  it("clears stale data when switching PRs", async () => {
    const filesA = makeFilesResult(["a.ts"]);
    const diffA = makeDiffResult(["a.ts"]);
    const filesB = makeFilesResult(["b.ts"]);
    const diffB = makeDiffResult(["b.ts"]);

    // Deferred responses to control resolution order.
    let resolveFilesB: () => void = () => {};
    let resolveDiffB: () => void = () => {};
    let filesBStarted = false;
    let diffBStarted = false;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      // PR A fetches resolve immediately.
      if (url.includes("/1/files")) {
        return Response.json(filesA);
      }
      if (url.includes("/1/diff")) {
        return Response.json(diffA);
      }
      // PR B: both deferred so we control timing explicitly.
      if (url.includes("/2/files")) {
        filesBStarted = true;
        return new Promise((resolve) => {
          resolveFilesB = () => resolve(Response.json(filesB));
        });
      }
      if (url.includes("/2/diff")) {
        diffBStarted = true;
        return new Promise((resolve) => {
          resolveDiffB = () => resolve(Response.json(diffB));
        });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    // Load PR A fully.
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);
    expect(store.getDiff()?.files[0]?.path).toBe("a.ts");
    expect(store.getFileList()?.files[0]?.path).toBe("a.ts");

    // Start loading PR B — don't await yet.
    store.loadDiff("owner", "repo", 2, ownerRepoRef);

    // Both stale PR A values must be null immediately.
    expect(store.getDiff()).toBeNull();
    expect(store.getFileList()).toBeNull();
    await vi.waitFor(() => {
      expect(filesBStarted).toBe(true);
      expect(diffBStarted).toBe(true);
    });

    // Release /files for B and let it settle.
    resolveFilesB();
    await vi.waitFor(() => {
      expect(store.getFileList()?.files[0]?.path).toBe("b.ts");
    });

    // Diff still null (not yet resolved).
    expect(store.getDiff()).toBeNull();

    // Release /diff for B.
    resolveDiffB();
    await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));

    expect(store.getDiff()?.files[0]?.path).toBe("b.ts");
    expect(store.getFileList()?.files[0]?.path).toBe("b.ts");
  });

  it("aborts in-flight requests when switching PRs", async () => {
    const diffB = makeDiffResult(["b.ts"]);
    const filesB = makeFilesResult(["b.ts"]);

    let diffAAborted = false;
    let filesAAborted = false;
    let diffAStarted = false;
    let filesAStarted = false;

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      const signal = input instanceof Request ? input.signal : init?.signal;

      if (url.includes("/1/files")) {
        filesAStarted = true;
        return new Promise((_resolve, reject) => {
          signal?.addEventListener("abort", () => {
            filesAAborted = true;
            reject(new DOMException("Aborted", "AbortError"));
          });
        });
      }
      if (url.includes("/1/diff")) {
        diffAStarted = true;
        return new Promise((_resolve, reject) => {
          signal?.addEventListener("abort", () => {
            diffAAborted = true;
            reject(new DOMException("Aborted", "AbortError"));
          });
        });
      }
      if (url.includes("/2/files")) {
        return Response.json(filesB);
      }
      if (url.includes("/2/diff")) {
        return Response.json(diffB);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });

    // Start loading PR A (will hang).
    store.loadDiff("owner", "repo", 1, ownerRepoRef);
    await vi.waitFor(() => {
      expect(diffAStarted).toBe(true);
      expect(filesAStarted).toBe(true);
    });

    // Switch to PR B — should abort PR A.
    await loadDiff(store, "owner", "repo", 2, ownerRepoRef);

    expect(diffAAborted).toBe(true);
    expect(filesAAborted).toBe(true);
    expect(store.getDiff()?.files[0]?.path).toBe("b.ts");
  });

  it("shows loading when /files fails but /diff still in flight", async () => {
    const diff = makeDiffResult(["a.ts"]);
    let resolveDiff: () => void = () => {};

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        return Response.json({ detail: "server error" }, { status: 500 });
      }
      if (url.includes("/diff")) {
        return new Promise((resolve) => {
          resolveDiff = () => resolve(Response.json(diff));
        });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    store.loadDiff("owner", "repo", 1, ownerRepoRef);

    // Wait for /files to fail.
    await vi.waitFor(() => {
      expect(store.getFileList()).toBeNull();
    });

    // isFileListLoading must stay true — /diff is still in flight.
    expect(store.isFileListLoading()).toBe(true);

    // Resolve /diff — file list falls through to diff.files.
    resolveDiff();
    await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));

    expect(store.isFileListLoading()).toBe(false);
    expect(store.getFileList()?.files[0]?.path).toBe("a.ts");
  });

  it("prefers diff.files over /files for whitespace filtering", async () => {
    // /files returns all files including whitespace-only ones.
    const filesResult = makeFilesResult(["a.ts", "b.ts"]);
    // /diff with whitespace=hide filters out whitespace-only file.
    const diffResult = makeDiffResult(["a.ts"]);

    const fetchedUrls: string[] = [];
    let resolveFiles: () => void = () => {};
    let resolveDiff: () => void = () => {};

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      fetchedUrls.push(url);

      if (url.includes("/files")) {
        return new Promise((resolve) => {
          resolveFiles = () => resolve(Response.json(filesResult));
        });
      }
      if (url.includes("/diff")) {
        return new Promise((resolve) => {
          resolveDiff = () => resolve(Response.json(diffResult));
        });
      }
      return Response.json({}, { status: 404 });
    });

    // Enable whitespace hiding before loading.
    localStorage.setItem("diff-hide-whitespace", "true");
    const store = createDiffStore({ client: testClient() });
    store.loadDiff("owner", "repo", 1, ownerRepoRef);

    // Verify /diff request includes whitespace=hide query param.
    await vi.waitFor(() => expect(fetchedUrls.some((u) => u.includes("diff?whitespace=hide"))).toBe(true));

    // /files arrives first — shows unfiltered preview.
    resolveFiles();
    await vi.waitFor(() => {
      expect(store.getFileList()?.files).toHaveLength(2);
    });

    // /diff arrives — authoritative, whitespace-filtered.
    resolveDiff();
    await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));

    expect(store.getFileList()?.files).toHaveLength(1);
    expect(store.getFileList()?.files[0]?.path).toBe("a.ts");
  });

  it("does not fall back to stale /files preview after whitespace toggle fails", async () => {
    const filesResult = makeFilesResult(["a.ts", "b.ts"]);
    const diffAll = makeDiffResult(["a.ts", "b.ts"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        return Response.json(filesResult);
      }
      if (url.includes("/diff")) {
        if (url.includes("whitespace=hide")) {
          // Whitespace-filtered diff request fails.
          return Response.json({ detail: "server error" }, { status: 500 });
        }
        return Response.json(diffAll);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);
    expect(store.getFileList()?.files).toHaveLength(2);

    // Toggle whitespace — /diff reload will fail.
    store.setHideWhitespace(true);
    await vi.waitFor(() => {
      expect(store.getDiffError()).toBeTruthy();
    });

    // fileList was cleared by reloadDiffOnly, diff is null from error.
    // Sidebar must NOT fall back to stale unfiltered /files preview.
    expect(store.getFileList()).toBeNull();
  });

  it("clears file list when /diff fails so sidebar shows no stale files", async () => {
    const filesResult = makeFilesResult(["a.ts", "b.ts"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        return Response.json(filesResult);
      }
      if (url.includes("/diff")) {
        return Response.json({ detail: "server error" }, { status: 500 });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);

    // /diff failed — sidebar must not show stale /files data.
    expect(store.getDiffError()).toBeTruthy();
    expect(store.getFileList()).toBeNull();
  });

  it("clears file list when /diff fails before /files resolves", async () => {
    const filesResult = makeFilesResult(["a.ts"]);
    let resolveFiles: () => void = () => {};

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        return new Promise((resolve) => {
          resolveFiles = () => resolve(Response.json(filesResult));
        });
      }
      if (url.includes("/diff")) {
        // /diff fails immediately.
        return Response.json({ detail: "server error" }, { status: 500 });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    store.loadDiff("owner", "repo", 1, ownerRepoRef);

    // /diff fails fast, /files still pending — release it.
    resolveFiles();
    await vi.waitFor(() => expect(store.isDiffLoading()).toBe(false));

    // Late /files must not repopulate sidebar after /diff error.
    expect(store.getDiffError()).toBeTruthy();
    expect(store.getFileList()).toBeNull();
  });

  it("normalizes null files from API to empty array", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        // API returns files: null (Go nil slice serialization).
        return Response.json({ stale: false, files: null });
      }
      if (url.includes("/diff")) {
        return Response.json({
          stale: false,
          whitespace_only_count: 0,
          files: null,
        });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);

    // getFileList must return [] not null, even when API sends null.
    const result = store.getFileList();
    expect(result).not.toBeNull();
    expect(result!.files).toEqual([]);
  });

  it("normalizes nullable nested diff collections", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      const fileWithNullLines = {
        path: "src/app.ts",
        old_path: "src/app.ts",
        status: "modified",
        is_binary: false,
        is_generated: false,
        is_whitespace_only: false,
        additions: 1,
        deletions: 0,
        patch: "",
        hunks: [
          {
            old_start: 1,
            old_count: 0,
            new_start: 1,
            new_count: 1,
            lines: null,
          },
        ],
      };
      const fileWithNullHunks = {
        ...fileWithNullLines,
        path: "src/empty.ts",
        old_path: "src/empty.ts",
        hunks: null,
      };

      if (url.includes("/files")) return Response.json({ stale: false, files: [fileWithNullHunks] });
      if (url.includes("/diff")) {
        return Response.json({
          stale: false,
          whitespace_only_count: 0,
          files: [fileWithNullHunks, fileWithNullLines],
        });
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);

    expect(store.getDiff()?.files[0]?.hunks).toEqual([]);
    expect(store.getDiff()?.files[1]?.hunks[0]?.lines).toEqual([]);
  });

  it("filters loaded diff and file list by selected file category", async () => {
    const result = makeDiffResult(["docs/review-plan.md", "src/App.svelte", "src/App.test.ts", "bun.lock"]);

    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

      if (url.includes("/files")) {
        return Response.json({ stale: false, files: result.files });
      }
      if (url.includes("/diff")) {
        return Response.json(result);
      }
      return Response.json({}, { status: 404 });
    });

    const store = createDiffStore({ client: testClient() });
    await loadDiff(store, "owner", "repo", 1, ownerRepoRef);

    expect(store.getFileCategoryFilter()).toBe("all");
    expect(store.getVisibleDiffFiles().map((file) => file.path)).toEqual([
      "docs/review-plan.md",
      "src/App.svelte",
      "src/App.test.ts",
      "bun.lock",
    ]);
    expect(store.getFileCategoryCounts()).toEqual({
      plansDocs: 1,
      generated: 1,
      code: 1,
      tests: 1,
      other: 0,
      all: 4,
    });

    store.setFileCategoryFilter("tests");

    expect(store.getVisibleDiffFiles().map((file) => file.path)).toEqual(["src/App.test.ts"]);
    expect(store.getVisibleFileList()?.files.map((file) => file.path)).toEqual(["src/App.test.ts"]);
  });
});
