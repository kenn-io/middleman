import { afterEach, describe, expect, it, vi } from "vite-plus/test";

import type { KataEffectiveLink, KataEffectiveLinksResponse } from "../api/generated/models/index.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { GeneratedProblemResponse } from "../api/runtime.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import { createKataLinksStore, type KataLinksSubject } from "./kata-links.svelte.js";

type EffectiveLinks = KataEffectiveLinksResponse;

const workspaceSubject: KataLinksSubject = { kind: "workspace", workspaceID: "workspace-1" };
const issueSubject: KataLinksSubject = {
  kind: "issue",
  provider: "github",
  platformHost: "github.example",
  owner: "acme",
  name: "widget",
  number: 17,
};

function link(uid: string, daemonID = "daemon-a"): KataEffectiveLink {
  return {
    daemon_id: daemonID,
    daemon_health: "ok",
    api_schema_version: "0.10.0",
    issue_uid: uid,
    project_uid: "project-1",
    provenance: ["direct"],
    reference: `KT-${uid}`,
    title: `Task ${uid}`,
  };
}

function envelope(links: EffectiveLinks["links"]): EffectiveLinks {
  return { state: "complete", diagnostics: [], links };
}

function detail(uid: string) {
  return {
    api_schema_version: "0.10.0",
    daemon_health: "ok",
    detail: { issue: { uid, title: `Task ${uid}`, status: "open" } },
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function clientWith(get: ReturnType<typeof vi.fn>): GeneratedClient {
  const data = async (path: string, options: Record<string, unknown>) => {
    const result = await get(path, options);
    if (result.error !== undefined) {
      const status = result.response?.status ?? 500;
      throw new GeneratedProblemResponse(
        {
          type: "about:blank",
          title: result.error.title ?? "Request failed",
          status,
          detail: result.error.detail,
          code: "internalError",
        },
        result.response ?? new Response(null, { status }),
      );
    }
    return result.data;
  };
  return makeGeneratedClient({
    KataService: {
      listWorkspaceKataLinks: (params, options) =>
        data("/workspaces/{id}/kata-links", {
          params: { path: { id: params.id } },
          ...(options?.signal === undefined ? {} : { signal: options.signal }),
        }),
      listIssueKataLinksOnHost: (params, options) =>
        data("/host/{platform_host}/issues/{provider}/{owner}/{name}/{number}/kata-links", {
          params: {
            path: {
              platform_host: params.platformHost,
              provider: params.provider,
              owner: params.owner,
              name: params.name,
              number: params.number,
            },
          },
          ...(options?.signal === undefined ? {} : { signal: options.signal }),
        }),
      getKataIssueDetail: (params, options) =>
        data("/kata/daemons/{daemon_id}/issues/{issue_uid}", {
          params: { path: { daemon_id: params.daemonId, issue_uid: params.issueUid } },
          ...(options?.signal === undefined ? {} : { signal: options.signal }),
        }),
    },
  });
}

describe("createKataLinksStore", () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("loads the subject links and selected detail", async () => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: envelope([link("one")]) })
      .mockResolvedValueOnce({ data: detail("one") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    await store.loadLinks();

    expect(get).toHaveBeenNthCalledWith(1, "/workspaces/{id}/kata-links", {
      params: { path: { id: "workspace-1" } },
      signal: expect.any(AbortSignal),
    });
    expect(get).toHaveBeenNthCalledWith(2, "/kata/daemons/{daemon_id}/issues/{issue_uid}", {
      params: { path: { daemon_id: "daemon-a", issue_uid: "one" } },
      signal: expect.any(AbortSignal),
    });
    expect(store.links()).toEqual([link("one")]);
    expect(store.selected()?.issue_uid).toBe("one");
    expect(store.detail()?.detail).toEqual(detail("one").detail);
  });

  it("resets on a subject change and rejects responses from the prior subject", async () => {
    const oldLoad = deferred<{ data: EffectiveLinks }>();
    const get = vi
      .fn()
      .mockReturnValueOnce(oldLoad.promise)
      .mockResolvedValueOnce({ data: envelope([link("new")]) })
      .mockResolvedValueOnce({ data: detail("new") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    const stale = store.loadLinks();
    store.setSubject(issueSubject);
    expect(store.links()).toEqual([]);
    expect(store.selected()).toBeNull();
    const current = store.loadLinks();
    oldLoad.resolve({ data: envelope([link("old")]) });
    await Promise.all([stale, current]);

    expect(get).toHaveBeenNthCalledWith(
      2,
      "/host/{platform_host}/issues/{provider}/{owner}/{name}/{number}/kata-links",
      {
        params: {
          path: {
            platform_host: "github.example",
            provider: "github",
            owner: "acme",
            name: "widget",
            number: 17,
          },
        },
        signal: expect.any(AbortSignal),
      },
    );
    expect(store.links().map((item) => item.issue_uid)).toEqual(["new"]);
  });

  it("preserves selection when refreshed links still contain it", async () => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: envelope([link("one"), link("two")]) })
      .mockResolvedValueOnce({ data: detail("one") })
      .mockResolvedValueOnce({ data: detail("two") })
      .mockResolvedValueOnce({ data: envelope([link("two"), link("three")]) })
      .mockResolvedValueOnce({ data: detail("two") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    await store.loadLinks();
    await store.select("daemon-a", "two");
    await store.loadLinks();

    expect(store.selected()?.issue_uid).toBe("two");
    expect(store.detail()?.detail).toEqual(detail("two").detail);
  });

  it("clears prior detail before fetching a replacement selected by association refresh", async () => {
    const replacementDetail = deferred<{ data: ReturnType<typeof detail> }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: envelope([link("one")]) })
      .mockResolvedValueOnce({ data: detail("one") })
      .mockResolvedValueOnce({ data: envelope([link("two")]) })
      .mockReturnValueOnce(replacementDetail.promise);
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });
    await store.loadLinks();

    const refreshing = store.loadLinks();
    await vi.waitFor(() => expect(store.selected()?.issue_uid).toBe("two"));

    expect(store.detail()).toBeNull();
    replacementDetail.resolve({ data: detail("two") });
    await refreshing;
    expect(store.detail()?.detail).toEqual(detail("two").detail);
  });

  it("fences a stale detail response after a newer selection", async () => {
    const oldDetail = deferred<{ data: ReturnType<typeof detail> }>();
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: envelope([link("one"), link("two")]) })
      .mockReturnValueOnce(oldDetail.promise)
      .mockResolvedValueOnce({ data: detail("two") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    const loading = store.loadLinks();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    const selecting = store.select("daemon-a", "two");
    oldDetail.resolve({ data: detail("one") });
    await Promise.all([loading, selecting]);

    expect(store.selected()?.issue_uid).toBe("two");
    expect(store.detail()?.detail).toEqual(detail("two").detail);
  });

  it("manual refresh reloads associations and selected detail", async () => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: envelope([link("one")]) })
      .mockResolvedValueOnce({ data: detail("one") })
      .mockResolvedValueOnce({ data: envelope([link("one")]) })
      .mockResolvedValueOnce({ data: detail("one") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    await store.loadLinks();
    await store.loadLinks();

    expect(get).toHaveBeenCalledTimes(4);
  });

  it("refreshes stale associations on activation and focus but not before 15 seconds", async () => {
    vi.useFakeTimers();
    const get = vi.fn().mockImplementation((path: string) =>
      Promise.resolve({
        data: path.includes("/issues/{issue_uid}") ? detail("one") : envelope([link("one")]),
      }),
    );
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });
    await store.loadLinks();
    expect(get).toHaveBeenCalledTimes(2);

    store.activate(false);
    await vi.advanceTimersByTimeAsync(14_999);
    store.activate(true);
    await Promise.resolve();
    expect(get).toHaveBeenCalledTimes(2);

    store.activate(false);
    await vi.advanceTimersByTimeAsync(1);
    store.activate(true);
    await Promise.resolve();
    await Promise.resolve();
    expect(get).toHaveBeenCalledTimes(4);

    await vi.advanceTimersByTimeAsync(14_999);
    store.noteFocus();
    await Promise.resolve();
    expect(get).toHaveBeenCalledTimes(4);

    await vi.advanceTimersByTimeAsync(1);
    store.noteFocus();
    await Promise.resolve();
    await Promise.resolve();
    expect(get).toHaveBeenCalledTimes(6);
  });

  it.each(["focus", "activation"] as const)("retries an initial association failure after %s", async (trigger) => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ error: { detail: "daemon unavailable" } })
      .mockResolvedValueOnce({ data: envelope([link("one")]) })
      .mockResolvedValueOnce({ data: detail("one") });
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });

    const initial = store.loadLinks();
    if (trigger === "focus") store.activate(true);
    await initial;
    expect(store.error()).toBe("daemon unavailable");

    if (trigger === "focus") store.noteFocus();
    else store.activate(true);

    await vi.waitFor(() => expect(store.links()).toEqual([link("one")]));
    expect(store.error()).toBeNull();
    expect(get).toHaveBeenCalledTimes(3);
  });

  it("polls only selected detail every 30 seconds while active and visible", async () => {
    vi.useFakeTimers();
    const get = vi.fn().mockImplementation((path: string) =>
      Promise.resolve({
        data: path.includes("/issues/{issue_uid}") ? detail("one") : envelope([link("one")]),
      }),
    );
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });
    await store.loadLinks();
    store.activate(true);

    await vi.advanceTimersByTimeAsync(60_000);

    const paths = get.mock.calls.map(([path]) => path);
    expect(paths.filter((path) => path === "/workspaces/{id}/kata-links")).toHaveLength(1);
    expect(paths.filter((path) => path.includes("/issues/{issue_uid}"))).toHaveLength(3);
  });

  it("does not refresh an inactive store when a hidden view becomes visible", async () => {
    vi.useFakeTimers();
    const get = vi.fn().mockImplementation((path: string) =>
      Promise.resolve({
        data: path.includes("/issues/{issue_uid}") ? detail("one") : envelope([link("one")]),
      }),
    );
    const store = createKataLinksStore({
      client: clientWith(get),
      subject: workspaceSubject,
      visible: false,
    });
    await store.loadLinks();
    await vi.advanceTimersByTimeAsync(15_000);

    store.noteVisibility(true);
    await Promise.resolve();

    expect(get).toHaveBeenCalledTimes(2);
  });

  it("cancels polling immediately when inactive, hidden, or destroyed", async () => {
    vi.useFakeTimers();
    const get = vi.fn().mockImplementation((path: string) =>
      Promise.resolve({
        data: path.includes("/issues/{issue_uid}") ? detail("one") : envelope([link("one")]),
      }),
    );
    const store = createKataLinksStore({ client: clientWith(get), subject: workspaceSubject });
    await store.loadLinks();
    store.activate(true);
    store.noteVisibility(false);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(get).toHaveBeenCalledTimes(2);

    store.noteVisibility(true);
    await Promise.resolve();
    store.activate(false);
    await vi.advanceTimersByTimeAsync(60_000);
    store.activate(true);
    const callsBeforeDestroy = get.mock.calls.length;
    store.destroy();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(get).toHaveBeenCalledTimes(callsBeforeDestroy);
  });
});
