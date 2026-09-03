import { describe, expect, test, vi } from "vite-plus/test";
import { createDocsAPI } from "./api";

// fakeFetch records every call so tests can assert the URL/method/body the
// client sent and choose what to return per request. openapi-fetch invokes
// the injected fetch with a single Request object, so unpack url/method/body
// from it into the {url, init} shape the assertions read. Each `respond`
// entry is consumed in FIFO order; trailing requests fall back to 200/{}.
// Error responses (>=400) carry the kenn-forge problem+json content type so
// they exercise the same parse path the live server's errors take.
function fakeFetch(respond: Array<{ status: number; body?: unknown }>) {
  const calls: Array<{ url: string; init?: RequestInit }> = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : undefined;
    const url = request ? request.url : String(input);
    const method = request?.method ?? init?.method;
    const body = request ? await request.clone().text() : init?.body;
    const recordedInit: RequestInit = { ...init, body: body as BodyInit };
    if (method !== undefined) recordedInit.method = method;
    calls.push({ url, init: recordedInit });
    const next = respond.shift() ?? { status: 200, body: {} };
    const contentType = next.status >= 400 ? "application/problem+json" : "application/json";
    return new Response(next.body === undefined ? null : JSON.stringify(next.body), {
      status: next.status,
      headers: { "content-type": contentType },
    });
  });
  return { fn, calls };
}

// problem+json error body matching the shape kenn-forge's Huma layer emits.
function problem(status: number, code: string, detail: string) {
  return { type: "about:blank", title: code, status, detail, code };
}

function problemWithDetails(status: number, code: string, detail: string, details: Record<string, unknown>) {
  return { ...problem(status, code, detail), details };
}

describe("createDocsAPI folder edits", () => {
  test("addFolder POSTs JSON and returns the unwrapped folder", async () => {
    const { fn, calls } = fakeFetch([
      { status: 201, body: { folder: { id: "research", name: "Research", path: "/abs/research" } } },
    ]);
    const api = createDocsAPI({ fetch: fn });
    const added = await api.addFolder({ path: "~/Research", name: "Research" });
    expect(added).toEqual({ id: "research", name: "Research", path: "/abs/research" });
    expect(calls[0]!.url).toContain("/api/v1/docs/folders");
    expect(calls[0]!.init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0]!.init?.body))).toEqual({
      path: "~/Research",
      name: "Research",
    });
  });

  test("addFolder propagates server errors with code and status", async () => {
    // Real wire shape: the server sends a generic top-level code with the
    // actionable reason under details.reason (camelCase).
    const { fn } = fakeFetch([
      { status: 409, body: problemWithDetails(409, "conflict", "id taken", { reason: "duplicateFolderID" }) },
    ]);
    const api = createDocsAPI({ fetch: fn });
    await expect(api.addFolder({ path: "/x" })).rejects.toMatchObject({
      status: 409,
      code: "duplicate_folder_id",
      message: "id taken",
    });
  });

  test("removeFolder DELETEs the folder path and resolves on 204", async () => {
    const { fn, calls } = fakeFetch([{ status: 204 }]);
    const api = createDocsAPI({ fetch: fn });
    await api.removeFolder("notes");
    expect(calls[0]!.url).toContain("/api/v1/docs/folders/notes");
    expect(calls[0]!.init?.method).toBe("DELETE");
  });

  test("removeFolder surfaces settingsUnavailable as a DocsAPIError", async () => {
    const { fn } = fakeFetch([{ status: 404, body: problem(404, "settingsUnavailable", "no writer") }]);
    const api = createDocsAPI({ fetch: fn });
    await expect(api.removeFolder("notes")).rejects.toMatchObject({
      status: 404,
      code: "settingsUnavailable",
    });
  });

  test("renameFolder PATCHes the folder and returns the updated record", async () => {
    const { fn, calls } = fakeFetch([{ status: 200, body: { folder: { id: "notes", name: "Personal", path: "/p" } } }]);
    const api = createDocsAPI({ fetch: fn });
    const updated = await api.renameFolder("notes", "Personal");
    expect(updated.name).toBe("Personal");
    expect(calls[0]!.init?.method).toBe("PATCH");
    expect(JSON.parse(String(calls[0]!.init?.body))).toEqual({ name: "Personal" });
  });

  test("browseDirectories passes the path query and returns entries", async () => {
    const { fn, calls } = fakeFetch([
      {
        status: 200,
        body: {
          path: "/Users/me",
          parent: "/Users",
          entries: [
            { name: "Notes", path: "/Users/me/Notes" },
            { name: ".config", path: "/Users/me/.config", hidden: true },
          ],
        },
      },
    ]);
    const api = createDocsAPI({ fetch: fn });
    const result = await api.browseDirectories("/Users/me");
    expect(calls[0]!.url).toContain("path=%2FUsers%2Fme");
    expect(result.path).toBe("/Users/me");
    expect(result.entries).toHaveLength(2);
    expect(result.entries[1]).toEqual({
      name: ".config",
      path: "/Users/me/.config",
      hidden: true,
    });
  });

  test("browseDirectories omits the path query when no argument given", async () => {
    const { fn, calls } = fakeFetch([{ status: 200, body: { path: "/home/x", parent: "/home", entries: [] } }]);
    const api = createDocsAPI({ fetch: fn });
    await api.browseDirectories();
    expect(calls[0]!.url).not.toContain("path=");
  });

  test("blobURL uses the runtime API base", () => {
    const api = createDocsAPI();
    expect(api.blobURL("notes", "images/logo.png")).toBe("/api/v1/docs/folders/notes/blob?path=images%2Flogo.png");
  });

  test("blobURL accepts a relative API base", () => {
    const api = createDocsAPI({ baseURL: "/kenn-forge/api/v1" });
    expect(api.blobURL("notes", "images/logo.png")).toBe(
      "/kenn-forge/api/v1/docs/folders/notes/blob?path=images%2Flogo.png",
    );
  });
});

describe("createDocsAPI search", () => {
  test("search scopes the query to a folder endpoint and defaults missing hits to an empty list", async () => {
    const { fn, calls } = fakeFetch([{ status: 200, body: { query: "release notes" } }]);
    const api = createDocsAPI({ fetch: fn });
    const result = await api.search("notes", "release notes", 25);

    const url = new URL(calls[0]!.url);
    expect(url.pathname).toBe("/api/v1/docs/folders/notes/search");
    expect(url.searchParams.get("q")).toBe("release notes");
    expect(url.searchParams.get("limit")).toBe("25");
    expect(result).toEqual({ query: "release notes", hits: [] });
  });

  test("searchAll sends the global docs query and preserves cross-folder hit metadata", async () => {
    const { fn, calls } = fakeFetch([
      {
        status: 200,
        body: {
          query: "release",
          hits: [
            {
              folder: "notes",
              folder_name: "Notes",
              name: "README.md",
              rel_path: "README.md",
              score: 4,
              hit_type: "body",
              line: 3,
              snippet: { text: "release notes", matches: [{ start: 0, end: 7 }] },
            },
          ],
          truncated: true,
        },
      },
    ]);
    const api = createDocsAPI({ fetch: fn });
    const result = await api.searchAll("release", 10);

    const url = new URL(calls[0]!.url);
    expect(url.pathname).toBe("/api/v1/docs/search");
    expect(url.searchParams.get("q")).toBe("release");
    expect(url.searchParams.get("limit")).toBe("10");
    expect(result.hits[0]).toEqual({
      folder: "notes",
      folder_name: "Notes",
      name: "README.md",
      rel_path: "README.md",
      score: 4,
      hit_type: "body",
      line: 3,
      snippet: { text: "release notes", matches: [{ start: 0, end: 7 }] },
    });
    expect(result.truncated).toBe(true);
  });
});

test("gitChanges hits GET /git/changes and returns the parsed response", async () => {
  const calls: { url: string }[] = [];
  const fakeFetchFn: typeof fetch = async (input) => {
    calls.push({ url: input instanceof Request ? input.url : String(input) });
    return new Response(
      JSON.stringify({
        is_repo: true,
        branch: "main",
        upstream: "origin/main",
        changes: [{ path: "new.md", status: "untracked" }],
        ignored_non_markdown_count: 0,
        suggested_message: "docs: update new.md\n\n- new.md\n",
      }),
      { status: 200, headers: { "content-type": "application/json" } },
    );
  };
  const api = createDocsAPI({ fetch: fakeFetchFn });
  const res = await api.gitChanges("notes");
  expect(calls[0]!.url).toContain("/api/v1/docs/folders/notes/git/changes");
  expect(res.branch).toBe("main");
  expect(res.changes).toHaveLength(1);
});

test("gitPublish POSTs message to /git/publish and returns the parsed response", async () => {
  const calls: { url: string; method?: string; body: string }[] = [];
  const fakeFetchFn: typeof fetch = async (input) => {
    const request = input as Request;
    calls.push({ url: request.url, method: request.method, body: await request.clone().text() });
    return new Response(
      JSON.stringify({
        commit: "abcdef1234567890abcdef1234567890abcdef12",
        short_commit: "abcdef1",
        branch: "main",
        upstream: "origin/main",
        pushed: true,
        files: [{ path: "new.md", status: "untracked" }],
      }),
      { status: 200, headers: { "content-type": "application/json" } },
    );
  };
  const api = createDocsAPI({ fetch: fakeFetchFn });
  const res = await api.gitPublish("notes", "docs: x");
  expect(calls[0]!.url).toContain("/api/v1/docs/folders/notes/git/publish");
  expect(calls[0]!.method).toBe("POST");
  expect(calls[0]!.body).toBe(JSON.stringify({ message: "docs: x" }));
  expect(res.commit.length).toBe(40);
  expect(res.pushed).toBe(true);
});

test("gitPublish maps server publish reasons onto frontend error codes", async () => {
  const { fn } = fakeFetch([
    {
      status: 409,
      body: problemWithDetails(409, "conflict", "push failed", {
        reason: "pushFailedAfterCommit",
        commit: "abcdef1234567890abcdef1234567890abcdef12",
      }),
    },
  ]);
  const api = createDocsAPI({ fetch: fn });

  await expect(api.gitPublish("notes", "docs: x")).rejects.toMatchObject({
    status: 409,
    code: "push_failed_after_commit",
    commit: "abcdef1234567890abcdef1234567890abcdef12",
    message: "push failed",
  });
});

test("gitPublish maps an unsafe git config rejection onto its frontend code", async () => {
  const { fn } = fakeFetch([
    {
      status: 400,
      body: problemWithDetails(400, "badRequest", "unsafe git config", { reason: "unsafeGitConfig" }),
    },
  ]);
  const api = createDocsAPI({ fetch: fn });

  await expect(api.gitPublish("notes", "docs: x")).rejects.toMatchObject({
    status: 400,
    code: "unsafe_git_config",
  });
});

test("file operations map server reasons onto frontend error codes", async () => {
  const cases = [
    { status: 409, topCode: "conflict", reason: "alreadyExists", code: "already_exists" },
    { status: 415, topCode: "badRequest", reason: "unsupportedExtension", code: "unsupported_extension" },
    { status: 403, topCode: "forbidden", reason: "outsideFolder", code: "outside_folder" },
  ];
  for (const c of cases) {
    const { fn } = fakeFetch([
      { status: c.status, body: problemWithDetails(c.status, c.topCode, "nope", { reason: c.reason }) },
    ]);
    const api = createDocsAPI({ fetch: fn });
    await expect(api.createFile("notes", "a.md")).rejects.toMatchObject({
      status: c.status,
      code: c.code,
    });
  }
});

test("gitPull POSTs to /git/pull and returns the parsed response", async () => {
  const { fn, calls } = fakeFetch([
    {
      status: 200,
      body: {
        branch: "main",
        upstream: "origin/main",
        up_to_date: false,
        commit: "abcdef1234567890abcdef1234567890abcdef12",
        short_commit: "abcdef1",
      },
    },
  ]);
  const api = createDocsAPI({ fetch: fn });
  const res = await api.gitPull("notes");
  expect(calls[0]!.url).toContain("/api/v1/docs/folders/notes/git/pull");
  expect(calls[0]!.init?.method).toBe("POST");
  expect(res.up_to_date).toBe(false);
  expect(res.short_commit).toBe("abcdef1");
});

test("gitPull maps server pull reasons onto frontend error codes", async () => {
  const cases: Array<{ reason: string; code: string; status: number }> = [
    { reason: "diverged", code: "diverged", status: 409 },
    { reason: "pullFailed", code: "pull_failed", status: 502 },
    { reason: "gitOperationInProgress", code: "git_operation_in_progress", status: 409 },
    { reason: "noUpstream", code: "no_upstream", status: 400 },
    { reason: "notGitRepo", code: "not_a_git_repo", status: 400 },
  ];
  for (const { reason, code, status } of cases) {
    const { fn } = fakeFetch([
      { status, body: problemWithDetails(status, "conflict", `pull refused: ${reason}`, { reason }) },
    ]);
    const api = createDocsAPI({ fetch: fn });
    const err = await api.gitPull("notes").catch((e: unknown) => e as Error & { code?: string; status?: number });
    expect(err).toBeInstanceOf(Error);
    expect((err as { code?: string }).code).toBe(code);
    expect((err as { status?: number }).status).toBe(status);
  }
});
