// @vitest-environment jsdom

import { Effect } from "effect";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import type { GeneratedClient } from "../api/generated-api.js";
import type { AppServices, OwnedAppRuntime } from "../app/runtime.js";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { createRepoBrowserStore } from "./repo-browser.svelte.js";

const repo = {
  provider: "github",
  platformHost: "github.com",
  owner: "acme",
  name: "widgets",
  repoPath: "acme/widgets",
};

type TestClient = GeneratedClient;
type TestGetOptions = {
  params?: { path?: Record<string, string>; query?: Record<string, unknown> };
  signal?: AbortSignal;
};

function testClient(): TestClient {
  return {
    GET: vi.fn(async (path: string, options?: TestGetOptions) => {
      const url = testURL(path, options);
      if (url === "/repo/github/acme/widgets/browser/refs?repo_path=acme%2Fwidgets") {
        return {
          data: {
            repo,
            refs: [
              { type: "branch", name: "main", sha: "main-sha", stale: false },
              { type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false },
            ],
            default_ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=branch&ref_name=main&ref_sha=main-sha"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            entries: [
              { path: "README.md", type: "blob", size: 12 },
              { path: "src/app.ts", type: "blob", size: 30 },
            ],
            truncated: false,
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=tag&ref_name=v1.0.0&ref_sha=tag-sha"
      ) {
        return {
          data: {
            repo,
            ref: { type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false },
            entries: [
              { path: "src/app.ts", type: "blob", size: 30 },
              { path: "docs/guide.md", type: "blob", size: 20 },
            ],
            truncated: false,
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/last-changed?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&path=src%2Fapp.ts"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            commits: {
              "README.md": commit("readme changed"),
              "src/app.ts": commit("app changed"),
            },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/last-changed?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=tag-sha&path=src%2Fapp.ts&path=docs%2Fguide.md"
      ) {
        return {
          data: {
            repo,
            ref: { type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false },
            commits: {
              "src/app.ts": commit("tag app changed"),
              "docs/guide.md": commit("guide changed"),
            },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            blob: {
              path: "README.md",
              sha: "blob-sha",
              size: 12,
              media_type: "text/markdown; charset=utf-8",
              encoding: "utf-8",
              content: "# Widgets\n",
              binary: false,
              too_large: false,
            },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            path: "README.md",
            commits: [commit("readme changed")],
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            blob: {
              path: "src/app.ts",
              sha: "app-blob-sha",
              size: 30,
              media_type: "text/typescript; charset=utf-8",
              encoding: "utf-8",
              content: "export const app = true;\n",
              binary: false,
              too_large: false,
            },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=tag-sha&path=src%2Fapp.ts"
      ) {
        return {
          data: {
            repo,
            ref: { type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false },
            blob: {
              path: "src/app.ts",
              sha: "tag-app-blob-sha",
              size: 26,
              media_type: "text/typescript; charset=utf-8",
              encoding: "utf-8",
              content: "export const tag = true;\n",
              binary: false,
              too_large: false,
            },
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
      ) {
        return {
          data: {
            repo,
            ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
            path: "src/app.ts",
            commits: [commit("app changed")],
          },
          response: new Response(null, { status: 200 }),
        };
      }
      if (
        url ===
        "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=tag-sha&path=src%2Fapp.ts"
      ) {
        return {
          data: {
            repo,
            ref: { type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false },
            path: "src/app.ts",
            commits: [commit("tag app changed")],
          },
          response: new Response(null, { status: 200 }),
        };
      }
      return {
        error: { detail: `unexpected ${url}` },
        response: new Response(null, { status: 404 }),
      };
    }),
  } as unknown as TestClient;
}

let runtime: OwnedAppRuntime | undefined;

function createTestRepoBrowserStore(client: TestClient) {
  runtime = makeTestAppRuntime(client);
  return createRepoBrowserStore().mount();
}

async function runStoreEffect<A, E>(program: Effect.Effect<A, E, AppServices>): Promise<A> {
  if (runtime === undefined) throw new Error("repo browser test runtime is not initialized");
  const execution = runtime.runCommand(program, {
    operation: "test repository browser command",
    safeContext: {},
    onFailure: () => {},
  });
  return Effect.runPromise(execution.await.pipe(Effect.flatMap((exit) => exit)));
}

afterEach(async () => {
  localStorage.clear();
  vi.restoreAllMocks();
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
  runtime = undefined;
});

describe("createRepoBrowserStore", () => {
  it("loads refs, tree metadata, first blob, and file history for a repo", async () => {
    const store = createTestRepoBrowserStore(testClient());

    await runStoreEffect(store.loadRepo(repo));

    expect(store.getDefaultRef()?.name).toBe("main");
    expect(store.getSelectedPath()).toBe("README.md");
    expect(store.getBlob()?.content).toBe("# Widgets\n");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["readme changed"]);
    await vi.waitFor(() => {
      expect(store.getFileEntries().map((entry) => [entry.path, entry.lastChanged?.subject])).toEqual([
        ["README.md", "readme changed"],
        ["src/app.ts", "app changed"],
      ]);
    });
  });

  it("loads last-changed metadata for file trees larger than one backend batch", async () => {
    const entries = Array.from({ length: 251 }, (_, index) => ({
      path: `src/file-${index.toString().padStart(3, "0")}.ts`,
      type: "blob",
      size: 10,
    }));
    const lastChangedBatches: string[][] = [];
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=branch&ref_name=main&ref_sha=main-sha"
        ) {
          return Promise.resolve({
            data: {
              repo,
              ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
              entries,
              truncated: false,
            },
            response: new Response(null, { status: 200 }),
          });
        }
        if (url.startsWith("/repo/github/acme/widgets/browser/last-changed?")) {
          const params = new URL(`http://forge.test${url}`).searchParams;
          const paths = params.getAll("path");
          lastChangedBatches.push(paths);
          return Promise.resolve({
            data: {
              repo,
              ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
              commits: Object.fromEntries(paths.map((filePath) => [filePath, commit(`changed ${filePath}`)])),
            },
            response: new Response(null, { status: 200 }),
          });
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Ffile-250.ts"
        ) {
          return Promise.resolve(blobResponse("src/file-250.ts", "export const selected = true;\n"));
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Ffile-250.ts"
        ) {
          return Promise.resolve(historyResponse("src/file-250.ts", "changed src/file-250.ts"));
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo, { path: "src/file-250.ts" }));

    expect(store.getBlob()?.path).toBe("src/file-250.ts");
    await vi.waitFor(() => {
      expect(lastChangedBatches.map((batch) => batch.length)).toEqual([250, 1]);
      expect(lastChangedBatches[0]?.[0]).toBe("src/file-250.ts");
      expect(store.getFileEntries()[0]?.lastChanged?.subject).toBe("changed src/file-000.ts");
      expect(store.getFileEntries()[250]?.lastChanged?.subject).toBe("changed src/file-250.ts");
    });
  });

  it("loads the selected blob before delayed last-changed metadata", async () => {
    const base = testClient();
    const lastChanged = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/last-changed?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts&path=README.md"
        ) {
          return lastChanged.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo, { path: "src/app.ts" }));

    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
    expect(store.getFileEntries().map((entry) => entry.lastChanged)).toEqual([undefined, undefined]);

    lastChanged.resolve({
      data: {
        repo,
        ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
        commits: {
          "README.md": commit("readme changed"),
          "src/app.ts": commit("app changed"),
        },
      },
      response: new Response(null, { status: 200 }),
    });
    await vi.waitFor(() => {
      expect(store.getFileEntries().map((entry) => entry.lastChanged?.subject)).toEqual([
        "readme changed",
        "app changed",
      ]);
    });
  });

  it("persists source and preview view mode", () => {
    const store = createTestRepoBrowserStore(testClient());

    store.setViewMode("preview");

    expect(store.getViewMode()).toBe("preview");
    expect(localStorage.getItem("repo-browser-view-mode")).toBe("preview");
  });

  it("ignores stale blob and history responses after selecting another path", async () => {
    const base = testClient();
    const readmeBlob = deferredResponse();
    const readmeHistory = deferredResponse();
    let deferReadme = false;
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          deferReadme &&
          url ===
            "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
        ) {
          return readmeBlob.promise;
        }
        if (
          deferReadme &&
          url ===
            "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
        ) {
          return readmeHistory.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    deferReadme = true;
    const staleReadme = runStoreEffect(store.selectPath("README.md"));
    await runStoreEffect(store.selectPath("src/app.ts"));
    readmeBlob.resolve(blobResponse("README.md", "# stale\n"));
    readmeHistory.resolve(historyResponse("README.md", "stale readme"));
    await staleReadme.catch(() => undefined);

    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.getBlob()?.content).toBe("export const app = true;\n");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
    expect(store.isBlobLoading()).toBe(false);
  });

  it("keeps the tree usable when a user supersedes the initial path read", async () => {
    const base = testClient();
    const initialSignals: AbortSignal[] = [];
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
            "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md" ||
          url ===
            "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
        ) {
          const signal = options?.signal;
          if (signal) initialSignals.push(signal);
          return new Promise((_resolve, reject) => {
            signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    const initialLoad = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => expect(initialSignals).toHaveLength(2));
    await runStoreEffect(store.selectPath("src/app.ts"));
    await initialLoad;

    expect(initialSignals.every((signal) => signal.aborted)).toBe(true);
    expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.isLoading()).toBe(false);
    expect(store.isBlobLoading()).toBe(false);
  });

  it("keeps the loaded tree when the initial path read fails", async () => {
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md"
        ) {
          return Promise.resolve({
            error: { detail: "blob unavailable" },
            response: new Response(null, { status: 503 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo));

    expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    expect(store.getSelectedRef()?.name).toBe("main");
    expect(store.getError()).toBe("blob unavailable");
    expect(store.isLoading()).toBe(false);
  });

  it("aborts a repository read when a newer repository load supersedes it", async () => {
    let firstSignal: AbortSignal | undefined;
    const nextRepo = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "gadgets",
      repoPath: "acme/gadgets",
    };
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (url === "/repo/github/acme/widgets/browser/refs?repo_path=acme%2Fwidgets") {
          firstSignal = options?.signal;
          return new Promise((_resolve, reject) => {
            firstSignal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
          });
        }
        if (url === "/repo/github/acme/gadgets/browser/refs?repo_path=acme%2Fgadgets") {
          return Promise.resolve({
            data: {
              repo: nextRepo,
              refs: [],
              default_ref: null,
            },
            response: new Response(null, { status: 200 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    const firstLoad = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => expect(firstSignal).toBeDefined());
    await runStoreEffect(store.loadRepo(nextRepo));

    expect(firstSignal?.aborted).toBe(true);
    expect(store.getRepo()).toEqual(nextRepo);
    await firstLoad.catch(() => undefined);
  });

  it("does not let a stale mount publish after its successor", async () => {
    const staleRefs = deferredResponse();
    const nextRepo = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "gadgets",
      repoPath: "acme/gadgets",
    };
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (url === "/repo/github/acme/widgets/browser/refs?repo_path=acme%2Fwidgets") return staleRefs.promise;
        if (url === "/repo/github/acme/gadgets/browser/refs?repo_path=acme%2Fgadgets") {
          return Promise.resolve({
            data: { repo: nextRepo, refs: [], default_ref: null },
            response: new Response(null, { status: 200 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    runtime = makeTestAppRuntime(client);
    const rootStore = createRepoBrowserStore();
    const store = rootStore.mount();

    const staleLoad = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledOnce());
    const successor = rootStore.mount();
    await runStoreEffect(successor.loadRepo(nextRepo));
    staleRefs.resolve({
      data: {
        repo,
        refs: [{ type: "branch", name: "main", sha: "main-sha", stale: false }],
        default_ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
      },
      response: new Response(null, { status: 200 }),
    });
    await staleLoad.catch(() => undefined);

    expect(successor.getRepo()).toEqual(nextRepo);
    expect(successor.getRefs()).toEqual([]);
  });

  it("does not let a stale ref failure roll back its successor", async () => {
    const staleTree = deferredResponse();
    let staleTreeRequested = false;
    const nextRepo = {
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "gadgets",
      repoPath: "acme/gadgets",
    };
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=tag&ref_name=v1.0.0&ref_sha=tag-sha"
        ) {
          staleTreeRequested = true;
          return staleTree.promise;
        }
        if (url === "/repo/github/acme/gadgets/browser/refs?repo_path=acme%2Fgadgets") {
          return Promise.resolve({
            data: { repo: nextRepo, refs: [], default_ref: null },
            response: new Response(null, { status: 200 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    runtime = makeTestAppRuntime(client);
    const rootStore = createRepoBrowserStore();
    const stale = rootStore.mount();
    await runStoreEffect(stale.loadRepo(repo));

    const staleSelection = runStoreEffect(
      stale.selectRef({ type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false }),
    );
    await vi.waitFor(() => expect(staleTreeRequested).toBe(true));
    const successor = rootStore.mount();
    await runStoreEffect(successor.loadRepo(nextRepo));
    staleTree.resolve({
      error: { detail: "stale tree failed" },
      response: new Response(null, { status: 500 }),
    });
    await staleSelection.catch(() => undefined);

    expect(successor.getRepo()).toEqual(nextRepo);
    expect(successor.getRefs()).toEqual([]);
    expect(successor.getSelectedRef()).toBeNull();
    expect(successor.getTree()).toEqual([]);
    expect(successor.getError()).toBeNull();
  });

  it("prefers a root README for a pathless repository load", async () => {
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=branch&ref_name=main&ref_sha=main-sha"
        ) {
          return Promise.resolve({
            data: {
              repo,
              ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
              entries: [
                { path: "src/app.ts", type: "blob", size: 30 },
                { path: "README.md", type: "blob", size: 12 },
              ],
              truncated: false,
            },
            response: new Response(null, { status: 200 }),
          });
        }
        if (url.includes("/browser/last-changed?") && url.includes("path=src%2Fapp.ts&path=README.md")) {
          return Promise.resolve({
            data: { repo, ref: { type: "branch", name: "main", sha: "main-sha", stale: false }, commits: {} },
            response: new Response(null, { status: 200 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo));

    expect(store.getSelectedPath()).toBe("README.md");
  });

  it("aborts repository transport when the route owner stops", async () => {
    let requestSignal: AbortSignal | undefined;
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (url === "/repo/github/acme/widgets/browser/refs?repo_path=acme%2Fwidgets") {
          requestSignal = options?.signal;
          return new Promise((_resolve, reject) => {
            requestSignal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), {
              once: true,
            });
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    const load = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => expect(requestSignal).toBeDefined());
    await runStoreEffect(store.stop());

    expect(requestSignal?.aborted).toBe(true);
    expect(store.isLoading()).toBe(false);
    await load.catch(() => undefined);
  });

  it("does not auto-select over a user path selection while last-changed metadata loads", async () => {
    const base = testClient();
    const lastChanged = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/last-changed?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&path=src%2Fapp.ts"
        ) {
          return lastChanged.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    const load = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => {
      expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    });
    const selectedPath = runStoreEffect(store.selectPath("src/app.ts"));
    lastChanged.resolve({
      data: {
        repo,
        ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
        commits: {
          "README.md": commit("readme changed"),
          "src/app.ts": commit("app changed"),
        },
      },
      response: new Response(null, { status: 200 }),
    });

    await Promise.all([load, selectedPath]);

    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
  });

  it("keeps a user path selection when last-changed metadata fails after tree load", async () => {
    const base = testClient();
    const lastChanged = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/last-changed?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&path=src%2Fapp.ts"
        ) {
          return lastChanged.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    const load = runStoreEffect(store.loadRepo(repo));
    await vi.waitFor(() => {
      expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    });
    const selectedPath = runStoreEffect(store.selectPath("src/app.ts"));
    lastChanged.resolve({
      error: { detail: "last changed failed" },
      response: new Response(null, { status: 500 }),
    });

    await Promise.all([load, selectedPath]);

    expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
    expect(store.getError()).toBeNull();
  });

  it("clears path-dependent data while a new path is loading", async () => {
    const base = testClient();
    const srcBlob = deferredResponse();
    const srcHistory = deferredResponse();
    let deferSrc = false;
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          deferSrc &&
          url ===
            "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
        ) {
          return srcBlob.promise;
        }
        if (
          deferSrc &&
          url ===
            "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
        ) {
          return srcHistory.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    deferSrc = true;
    const pending = runStoreEffect(store.selectPath("src/app.ts"));

    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()).toBeNull();
    expect(store.getFileHistory()).toEqual([]);
    expect(store.getSelectedCommit()).toBeNull();
    expect(store.isBlobLoading()).toBe(true);

    srcBlob.resolve(blobResponse("src/app.ts", "export const app = true;\n"));
    srcHistory.resolve(historyResponse("src/app.ts", "app changed"));
    await pending;

    expect(store.getBlob()?.path).toBe("src/app.ts");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
    expect(store.isBlobLoading()).toBe(false);
  });

  it("ignores stale commit-detail responses and reports current commit errors", async () => {
    const base = testClient();
    const slowCommit = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/commit?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&sha=slow-sha"
        ) {
          return slowCommit.promise;
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/commit?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&sha=fast-sha"
        ) {
          return Promise.resolve(commitResponse("fast-sha", "fast commit"));
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/commit?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=README.md&sha=missing-sha"
        ) {
          return Promise.resolve({
            error: { detail: "commit failed" },
            response: new Response(null, { status: 404 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    const stale = runStoreEffect(store.selectCommit("slow-sha"));
    expect(store.getSelectedCommit()).toBeNull();
    await runStoreEffect(store.selectCommit("fast-sha"));
    expect(store.getSelectedCommit()?.sha).toBe("fast-sha");
    slowCommit.resolve(commitResponse("slow-sha", "slow commit"));
    await stale.catch(() => undefined);
    expect(store.getSelectedCommit()?.sha).toBe("fast-sha");

    await runStoreEffect(store.selectCommit("missing-sha"));
    expect(store.getSelectedCommit()).toBeNull();
    expect(store.getError()).toBe("commit failed");
  });

  it("clears dependent state and reports errors when ref switching fails", async () => {
    const base = testClient();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=tag&ref_name=v1.0.0&ref_sha=tag-sha"
        ) {
          return Promise.resolve({
            error: { detail: "tree failed" },
            response: new Response(null, { status: 500 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));
    await runStoreEffect(store.selectPath("src/app.ts"));

    const selected = await runStoreEffect(
      store.selectRef({ type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false }),
    );

    expect(selected).toBe(false);
    expect(store.getSelectedRef()?.name).toBe("main");
    expect(store.getTree().map((entry) => entry.path)).toEqual(["README.md", "src/app.ts"]);
    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.content).toBe("export const app = true;\n");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["app changed"]);
    expect(store.getError()).toBeNull();
    expect(store.isLoading()).toBe(false);
  });

  it("restores the last usable file when a ref switch interrupts a path load", async () => {
    const base = testClient();
    const srcBlob = deferredResponse();
    const srcHistory = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
        ) {
          return srcBlob.promise;
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src%2Fapp.ts"
        ) {
          return srcHistory.promise;
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=tag&ref_name=v1.0.0&ref_sha=tag-sha"
        ) {
          return Promise.resolve({
            error: { detail: "tree failed" },
            response: new Response(null, { status: 500 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    const pendingPath = runStoreEffect(store.selectPath("src/app.ts"));
    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.isBlobLoading()).toBe(true);

    expect(await runStoreEffect(store.selectRef({ type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false }))).toBe(
      false,
    );

    expect(store.getSelectedRef()?.name).toBe("main");
    expect(store.getSelectedPath()).toBe("README.md");
    expect(store.getBlob()?.content).toBe("# Widgets\n");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["readme changed"]);
    expect(store.isBlobLoading()).toBe(false);
    expect(store.getError()).toBeNull();

    srcBlob.resolve(blobResponse("src/app.ts", "export const stale = true;\n"));
    srcHistory.resolve(historyResponse("src/app.ts", "stale app"));
    await pendingPath.catch(() => undefined);

    expect(store.getSelectedPath()).toBe("README.md");
    expect(store.getBlob()?.content).toBe("# Widgets\n");
  });

  it("preserves refs when an initial requested ref tree fails", async () => {
    const base = testClient();
    const initialRef = { type: "branch" as const, name: "deleted", sha: "deleted-sha", stale: false };
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/tree?repo_path=acme%2Fwidgets&ref_type=branch&ref_name=deleted&ref_sha=deleted-sha"
        ) {
          return Promise.resolve({
            error: { detail: "tree failed" },
            response: new Response(null, { status: 404 }),
          });
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo, { ref: initialRef, path: "README.md" }));

    expect(store.getRefs().map((ref) => ref.name)).toEqual(["main", "v1.0.0"]);
    expect(store.getDefaultRef()?.name).toBe("main");
    expect(store.getSelectedRef()).toEqual(initialRef);
    expect(store.getTree()).toEqual([]);
    expect(store.getSelectedPath()).toBeNull();
    expect(store.getBlob()).toBeNull();
    expect(store.getFileHistory()).toEqual([]);
    expect(store.getError()).toBe("tree failed");
    expect(store.isLoading()).toBe(false);
  });

  it("preserves the selected path when switching refs if that path still exists", async () => {
    const store = createTestRepoBrowserStore(testClient());
    await runStoreEffect(store.loadRepo(repo));
    await runStoreEffect(store.selectPath("src/app.ts"));

    expect(await runStoreEffect(store.selectRef({ type: "tag", name: "v1.0.0", sha: "tag-sha", stale: false }))).toBe(
      true,
    );

    expect(store.getSelectedRef()?.name).toBe("v1.0.0");
    expect(store.getSelectedPath()).toBe("src/app.ts");
    expect(store.getBlob()?.content).toBe("export const tag = true;\n");
    expect(store.getFileHistory().map((item) => item.subject)).toEqual(["tag app changed"]);
  });

  it("retains an explicit missing initial path instead of selecting an unrelated file", async () => {
    const store = createTestRepoBrowserStore(testClient());

    await runStoreEffect(store.loadRepo(repo, { path: "missing.md" }));

    expect(store.getSelectedPath()).toBe("missing.md");
    expect(store.getBlob()).toBeNull();
    expect(store.getFileHistory()).toEqual([]);
    expect(store.getSelectedCommit()).toBeNull();
    expect(store.getError()).toBe("Path not found: missing.md");
  });

  it("retains an explicit directory path without loading it as a blob", async () => {
    const client = testClient();
    const store = createTestRepoBrowserStore(client);

    await runStoreEffect(store.loadRepo(repo, { path: "src" }));

    expect(store.getSelectedPath()).toBe("src");
    expect(store.getBlob()).toBeNull();
    expect(store.getFileHistory()).toEqual([]);
    expect(store.getSelectedCommit()).toBeNull();
    expect(store.getError()).toBeNull();
    expect(requestedURLs(client)).not.toContain(
      "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src",
    );
    expect(requestedURLs(client)).not.toContain(
      "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src",
    );
  });

  it("selects an implicit directory path without loading it as a blob", async () => {
    const client = testClient();
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    await runStoreEffect(store.selectPath("src"));

    expect(store.getSelectedPath()).toBe("src");
    expect(store.getBlob()).toBeNull();
    expect(store.getFileHistory()).toEqual([]);
    expect(store.getSelectedCommit()).toBeNull();
    expect(store.getError()).toBeNull();
    expect(requestedURLs(client)).not.toContain(
      "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src",
    );
    expect(requestedURLs(client)).not.toContain(
      "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=src",
    );
  });

  it("keeps unknown path selections loading while probing blob and history", async () => {
    const base = testClient();
    const missingBlob = deferredResponse();
    const missingHistory = deferredResponse();
    const client = {
      GET: vi.fn((path: string, options?: TestGetOptions) => {
        const url = testURL(path, options);
        if (
          url ===
          "/repo/github/acme/widgets/browser/blob?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=missing.md"
        ) {
          return missingBlob.promise;
        }
        if (
          url ===
          "/repo/github/acme/widgets/browser/history?repo_path=acme%2Fwidgets&ref_type=commit&ref_sha=main-sha&path=missing.md"
        ) {
          return missingHistory.promise;
        }
        return base.GET(path, options);
      }),
    } as unknown as TestClient;
    const store = createTestRepoBrowserStore(client);
    await runStoreEffect(store.loadRepo(repo));

    const pending = runStoreEffect(store.selectPath("missing.md"));

    expect(store.getSelectedPath()).toBe("missing.md");
    expect(store.isBlobLoading()).toBe(true);

    missingBlob.resolve({
      error: { detail: "git object not found" },
      response: new Response(null, { status: 404 }),
    });
    missingHistory.resolve(historyResponse("missing.md", "unreachable"));
    await pending;

    expect(store.isBlobLoading()).toBe(false);
    expect(store.getError()).toBe("git object not found");
  });
});

function testURL(path: string, options?: TestGetOptions): string {
  let url = path;
  for (const [key, value] of Object.entries(options?.params?.path ?? {})) {
    url = url.replace(`{${key}}`, encodeURIComponent(String(value)));
  }
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(options?.params?.query ?? {})) {
    if (value === undefined) continue;
    for (const item of Array.isArray(value) ? value : [value]) query.append(key, String(item));
  }
  const qs = query.toString();
  return qs ? `${url}?${qs}` : url;
}

function requestedURLs(client: TestClient): string[] {
  return (client.GET as unknown as { mock: { calls: Array<[string, TestGetOptions | undefined]> } }).mock.calls.map(
    ([path, options]) => testURL(path, options),
  );
}

function commit(subject: string) {
  return {
    sha: `${subject}-sha`,
    subject,
    body: "",
    author_name: "Alice",
    author_email: "alice@example.com",
    authored_at: "2026-06-01T00:00:00Z",
  };
}

function blobResponse(path: string, content: string) {
  return {
    data: {
      repo,
      ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
      blob: {
        path,
        sha: `${path}-blob-sha`,
        size: content.length,
        media_type: "text/plain; charset=utf-8",
        encoding: "utf-8",
        content,
        binary: false,
        too_large: false,
      },
    },
    response: new Response(null, { status: 200 }),
  };
}

function historyResponse(path: string, subject: string) {
  return {
    data: {
      repo,
      ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
      path,
      commits: [commit(subject)],
    },
    response: new Response(null, { status: 200 }),
  };
}

function commitResponse(sha: string, subject: string) {
  return {
    data: {
      repo,
      ref: { type: "branch", name: "main", sha: "main-sha", stale: false },
      path: "README.md",
      commit: {
        ...commit(subject),
        sha,
      },
    },
    response: new Response(null, { status: 200 }),
  };
}

function deferredResponse() {
  let resolve!: (value: unknown) => void;
  const promise = new Promise<unknown>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}
