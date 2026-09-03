import { beforeEach, describe, expect, it, vi } from "vite-plus/test";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));

vi.mock("../generated/index.js", async () => {
  const { GeneratedProblemResponse } = await import("../runtime.js");
  const value = async (request: Promise<unknown>) => {
    const result = (await request) as { data?: unknown; error?: { detail?: string } };
    if (result.error !== undefined) {
      const problem = {
        code: "notFound" as const,
        detail: result.error.detail,
        status: 404,
        title: "Not found",
      };
      throw new GeneratedProblemResponse(problem, Response.json(problem, { status: 404 }));
    }
    return result.data;
  };
  return {
    KataService: {
      listKataDaemons: (options?: unknown) => value(get("/kata/daemons", options)),
      listKataReferences: (path: { daemonId: string }, query: unknown, options?: unknown) =>
        value(
          get("/kata/daemons/{daemon_id}/references", {
            params: { path: { daemon_id: path.daemonId }, query },
            ...((options as object | undefined) ?? {}),
          }),
        ),
      resolveKataIssueReference: (path: { daemonId: string }, query: unknown, options?: unknown) =>
        value(
          get("/kata/daemons/{daemon_id}/issue-reference", {
            params: { path: { daemon_id: path.daemonId }, query },
            ...((options as object | undefined) ?? {}),
          }),
        ),
      createKataWorkspace: (body: unknown) => value(post("/kata/workspaces", { body })),
      getKataLaunchTarget: (path: { daemonId: string; issueUid: string }) =>
        value(
          get("/kata/daemons/{daemon_id}/issues/{issue_uid}/launch-target", {
            params: { path: { daemon_id: path.daemonId, issue_uid: path.issueUid } },
          }),
        ),
    },
  };
});

import {
  createOrOpenKataWorkspace,
  fetchKataDaemons,
  resolveKataIssueReference,
  resolveKataLaunchTarget,
  resolveKataTextReference,
  searchKataReferences,
} from "./integration.js";

describe("Kata integration API", () => {
  beforeEach(() => {
    get.mockReset();
    post.mockReset();
  });

  it("loads the daemon roster and searches references on an explicit daemon", async () => {
    const controller = new AbortController();
    get
      .mockResolvedValueOnce({ data: { daemons: [{ id: "work" }] } })
      .mockResolvedValueOnce({ data: { issues: [{ uid: "issue-1" }] } });

    await expect(fetchKataDaemons(controller.signal)).resolves.toEqual([{ id: "work" }]);
    await expect(searchKataReferences("work", "keep", controller.signal)).resolves.toEqual([{ uid: "issue-1" }]);

    expect(get).toHaveBeenNthCalledWith(1, "/kata/daemons", { signal: controller.signal });
    expect(get).toHaveBeenNthCalledWith(2, "/kata/daemons/{daemon_id}/references", {
      params: { path: { daemon_id: "work" }, query: { q: "keep", limit: 50 } },
      signal: controller.signal,
    });
  });

  it("creates or reuses a workspace from the exact selected identity", async () => {
    post.mockResolvedValue({ data: { id: "ws-existing", created: false } });
    const identity = {
      daemon_id: "work",
      project_uid: "project-1",
      project_name: "Kata",
      issue_uid: "issue-1",
      short_id: "KT-1",
      qualified_id: "Kata#KT-1",
      title: "Keep one UI",
    };

    await expect(createOrOpenKataWorkspace(identity)).resolves.toMatchObject({
      id: "ws-existing",
      created: false,
    });
    expect(post).toHaveBeenCalledWith("/kata/workspaces", { body: identity });
  });

  it("resolves a kata:// issue through the canonical UID query", async () => {
    const controller = new AbortController();
    get.mockResolvedValue({ data: { issues: [{ uid: "issue-1" }] } });

    await expect(resolveKataIssueReference("work", "issue-1", controller.signal)).resolves.toEqual({
      uid: "issue-1",
    });
    expect(get).toHaveBeenCalledWith("/kata/daemons/{daemon_id}/references", {
      params: { path: { daemon_id: "work" }, query: { issue_uid: ["issue-1"], limit: 2 } },
      signal: controller.signal,
    });
  });

  it("resolves a qualified textual reference through the exact all-status route", async () => {
    get.mockResolvedValue({ data: { uid: "issue-closed", project_uid: "project-a" } });

    await expect(resolveKataTextReference("work", "Project A", "closed-task")).resolves.toEqual({
      uid: "issue-closed",
      project_uid: "project-a",
    });
    expect(get).toHaveBeenCalledWith("/kata/daemons/{daemon_id}/issue-reference", {
      params: {
        path: { daemon_id: "work" },
        query: { project: "Project A", ref: "closed-task" },
      },
    });
  });

  it("resolves a bare textual reference through the unique exact all-status route", async () => {
    get.mockResolvedValue({ data: { uid: "issue-closed", project_uid: "project-a" } });

    await expect(resolveKataTextReference("work", undefined, "closed-task")).resolves.toEqual({
      uid: "issue-closed",
      project_uid: "project-a",
    });
    expect(get).toHaveBeenCalledWith("/kata/daemons/{daemon_id}/issue-reference", {
      params: {
        path: { daemon_id: "work" },
        query: { ref: "closed-task" },
      },
    });
  });

  it("resolves the standalone Kata target on the selected daemon", async () => {
    get.mockResolvedValue({ data: { available: true, url: "http://kata.test/issues/issue-1" } });

    await expect(resolveKataLaunchTarget("work", "issue-1")).resolves.toMatchObject({ available: true });
    expect(get).toHaveBeenCalledWith("/kata/daemons/{daemon_id}/issues/{issue_uid}/launch-target", {
      params: { path: { daemon_id: "work", issue_uid: "issue-1" } },
    });
  });

  it("surfaces typed Forge problem detail", async () => {
    post.mockResolvedValue({ response: { status: 404 }, error: { detail: "No repository mapping." } });

    await expect(
      createOrOpenKataWorkspace({ daemon_id: "work", project_uid: "project-1", issue_uid: "issue-1" }),
    ).rejects.toThrow("No repository mapping.");
  });
});
