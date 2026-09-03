import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";

import { projectIssueDetail } from "@kenn-io/kata-ui/packages/kata-ui/src/index.ts";

import type { KataEffectiveLink, KataEffectiveLinksResponse } from "../../api/generated/models/index.js";
import type { GeneratedClient } from "../../api/generated-api.js";
import { makeGeneratedClient } from "../../testing/generated-client.js";
import { NAVIGATE_KEY } from "../../context.js";
import type { KataLinksSubject } from "../../stores/kata-links.svelte.js";
import { resetKataWorkspaceCreateForTest } from "../../stores/kata-workspace-create.svelte.js";
import KataLinksPanel from "./KataLinksPanel.svelte";

vi.mock("@kenn-io/kata-ui/packages/kata-ui/src/index.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@kenn-io/kata-ui/packages/kata-ui/src/index.ts")>();
  return { ...actual, projectIssueDetail: vi.fn(actual.projectIssueDetail) };
});

type Link = KataEffectiveLink;
type LinksResponse = KataEffectiveLinksResponse;

const subject: KataLinksSubject = { kind: "workspace", workspaceID: "workspace-1" };

function link(overrides: Partial<Link> = {}): Link {
  return {
    daemon_id: "daemon-a",
    daemon_health: "ok",
    api_schema_version: "0.10.0",
    issue_uid: "issue-1",
    project_uid: "project-1",
    provenance: ["direct"],
    reference: "KT-1",
    status: "open",
    title: "Keep one Kata UI",
    direct_link_id: 41,
    ...overrides,
  };
}

function response(links: Link[], overrides: Partial<LinksResponse> = {}): LinksResponse {
  return { state: "complete", diagnostics: [], links, ...overrides };
}

function detail(apiSchemaVersion = "0.10.0", title = "Keep one Kata UI") {
  return {
    api_schema_version: apiSchemaVersion,
    daemon_health: "ok",
    detail: {
      issue: {
        uid: "issue-1",
        project_uid: "project-1",
        qualified_id: "KT-1",
        title,
        body: "Use the shared component.",
        status: "open",
      },
    },
  };
}

type KataClientMethods = {
  listLinks?: ReturnType<typeof vi.fn>;
  getDetail?: ReturnType<typeof vi.fn>;
  getLaunchTarget?: ReturnType<typeof vi.fn>;
  createWorkspace?: ReturnType<typeof vi.fn>;
  deleteLink?: ReturnType<typeof vi.fn>;
};

function forgeClient(methods: KataClientMethods): GeneratedClient {
  return makeGeneratedClient({
    KataService: {
      ...(methods.listLinks && { listWorkspaceKataLinks: methods.listLinks }),
      ...(methods.getDetail && { getKataIssueDetail: methods.getDetail }),
      ...(methods.getLaunchTarget && { getKataLaunchTarget: methods.getLaunchTarget }),
      ...(methods.createWorkspace && { createKataWorkspace: methods.createWorkspace }),
      ...(methods.deleteLink && { deleteWorkspaceKataLink: methods.deleteLink }),
    },
  });
}

function renderPanel(client: GeneratedClient, props: { active?: boolean; disabled?: boolean } = {}) {
  const navigate = vi.fn();
  const rendered = render(KataLinksPanel, {
    props: { subject, active: props.active ?? true, disabled: props.disabled ?? false, apiClient: client },
    context: new Map<symbol, unknown>([[NAVIGATE_KEY, navigate]]),
  });
  return { ...rendered, navigate };
}

function popupFixture() {
  const popupDocument = document.implementation.createHTMLDocument("Kata launch");
  const replace = vi.fn();
  const close = vi.fn();
  const popup = {
    close,
    document: popupDocument,
    location: { replace },
    opener: window,
  } as unknown as Window;
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  return { popup, popupDocument, replace, close };
}

function listAndDetailClient(links: Link[], detailEnvelope = detail(), linksResponse?: Partial<LinksResponse>) {
  const get = vi.fn();
  const methods = {
    listLinks: vi.fn((...args) => {
      get("listWorkspaceKataLinks", ...args);
      return Promise.resolve(response(links, linksResponse));
    }),
    getDetail: vi.fn((...args) => {
      get("getKataIssueDetail", ...args);
      return Promise.resolve(structuredClone(detailEnvelope));
    }),
  };
  return { client: forgeClient(methods), get, methods };
}

describe("KataLinksPanel", () => {
  beforeEach(() => {
    vi.mocked(projectIssueDetail).mockClear();
    vi.mocked(projectIssueDetail).mockImplementation((wire) => ({
      issue: {
        uid: wire.issue.uid,
        projectUID: wire.issue.project_uid ?? "",
        projectName: wire.issue.project_name ?? "",
        reference: wire.issue.qualified_id ?? wire.issue.short_id ?? wire.issue.uid,
        title: wire.issue.title,
        body: wire.issue.body ?? "",
        status: wire.issue.status,
        checklist: [],
        labels: [],
      },
      comments: [],
      links: [],
      children: [],
      pendingClaims: [],
    }));
  });

  afterEach(() => {
    resetKataWorkspaceCreateForTest();
    cleanup();
    vi.restoreAllMocks();
  });

  it("offers explicit linking from the empty state", async () => {
    const { client } = listAndDetailClient([]);
    renderPanel(client);

    expect(await screen.findByText("No Kata issues linked yet.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Link Kata issue" })).toBeTruthy();
  });

  it("defers the initial load until activation and loads the subject only once", async () => {
    const { client, get } = listAndDetailClient([link()]);
    const panel = renderPanel(client, { active: false });

    await tick();
    expect(get).not.toHaveBeenCalled();

    await panel.rerender({ subject, active: true, apiClient: client });
    expect(await screen.findByLabelText("Kata issue detail")).toBeTruthy();
    expect(get).toHaveBeenCalledTimes(2);

    await panel.rerender({ subject, active: false, apiClient: client });
    await panel.rerender({ subject, active: true, apiClient: client });
    await tick();
    expect(get).toHaveBeenCalledTimes(2);
  });

  it("lists multiple links with daemon and combined provenance disambiguation", async () => {
    const links = [
      link({ provenance: ["intrinsic", "direct", "inherited"] }),
      link({ daemon_id: "daemon-b", issue_uid: "issue-2", reference: "KT-2", title: "Second task" }),
    ];
    const { client } = listAndDetailClient(links);
    renderPanel(client);

    expect(await screen.findByRole("button", { name: /KT-1 Keep one Kata UI/ })).toBeTruthy();
    expect(screen.getByText("daemon-a")).toBeTruthy();
    expect(screen.getByText("Intrinsic, Direct, Inherited")).toBeTruthy();
    expect(screen.getByRole("button", { name: /KT-2 Second task/ })).toBeTruthy();
    expect(screen.getByText("daemon-b")).toBeTruthy();
  });

  it("only exposes unlink for a direct link and preserves controls after deletion", async () => {
    const direct = link();
    const inherited = link({
      daemon_id: "daemon-b",
      issue_uid: "issue-2",
      reference: "KT-2",
      title: "Second task",
      direct_link_id: undefined,
      provenance: ["inherited"],
    });
    const listLinks = vi
      .fn()
      .mockResolvedValueOnce(response([direct, inherited]))
      .mockResolvedValueOnce(response([inherited]));
    const getDetail = vi.fn().mockResolvedValueOnce(detail()).mockResolvedValueOnce(detail("0.10.0", "Second task"));
    const remove = vi.fn().mockResolvedValue(undefined);
    renderPanel(forgeClient({ listLinks, getDetail, deleteLink: remove }));

    await screen.findByRole("button", { name: "Unlink KT-1" });
    expect(screen.queryByRole("button", { name: "Unlink KT-2" })).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Unlink KT-1" }));

    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    expect(remove).toHaveBeenCalledWith({ id: "workspace-1", linkId: 41 });
    expect(await screen.findByRole("button", { name: /KT-2 Second task/ })).toBeTruthy();
  });

  it("keeps unavailable persisted links visible and reports a total hydration outage", async () => {
    const unavailable = link({ unavailable_reason: "daemon unreachable", title: undefined });
    const { client } = listAndDetailClient([unavailable], detail(), {
      state: "unavailable",
      diagnostics: [{ daemon_id: "daemon-a", reason: "connection refused" }],
    });
    renderPanel(client);

    expect(await screen.findByText("Unavailable")).toBeTruthy();
    expect(await screen.findAllByText("daemon unreachable")).toHaveLength(2);
    expect(screen.getByRole("status").textContent).toContain("daemon-a: connection refused");
    expect(screen.getByRole("button", { name: "Unlink KT-1" })).toBeTruthy();
  });

  it("explains why workspace creation is unavailable for the selected task", async () => {
    const unavailableWorkspace: NonNullable<Link["workspace"]> = {
      available: false,
      resolution_status: "ambiguous",
      resolution_source: "configured_clone",
      unavailable_reason: "Multiple repositories match this Kata project. Configure an explicit mapping in Settings.",
    };
    const { client } = listAndDetailClient([link({ workspace: unavailableWorkspace })]);
    renderPanel(client);

    expect(await screen.findByText(unavailableWorkspace.unavailable_reason ?? "")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create workspace" })).toBeNull();
  });

  it("opens an existing workspace when live workspace creation is unavailable", async () => {
    const existing = link({
      unavailable_reason: "daemon unavailable",
      workspace: {
        available: false,
        existing_workspace: { id: "workspace-existing", status: "ready" },
      },
    });
    const { client } = listAndDetailClient([existing]);
    const panel = renderPanel(client);

    await fireEvent.click(await screen.findByRole("button", { name: "Open workspace" }));

    expect(panel.navigate).toHaveBeenCalledWith("/terminal/workspace-existing");
  });

  it("disables opening an existing workspace while the surrounding surface is disabled", async () => {
    const existing = link({
      unavailable_reason: "daemon unavailable",
      workspace: {
        available: false,
        existing_workspace: { id: "workspace-existing", status: "ready" },
      },
    });
    const { client } = listAndDetailClient([existing]);
    const panel = renderPanel(client, { disabled: true });

    const button = await screen.findByRole("button", { name: "Open workspace" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(button);

    expect(panel.navigate).not.toHaveBeenCalled();
  });

  it("disables workspace creation while the surrounding surface is disabled", async () => {
    const creatable = link({ workspace: { available: true } });
    const { methods } = listAndDetailClient([creatable]);
    const post = vi.fn();
    renderPanel(forgeClient({ ...methods, createWorkspace: post }), { disabled: true });

    const button = await screen.findByRole("button", { name: "Create workspace" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(button);

    expect(post).not.toHaveBeenCalled();
  });

  it.each(["0.11.0", "0.13.0"])("renders reported schema version %s through Kata UI", async (version) => {
    const { client } = listAndDetailClient([link({ api_schema_version: version })], detail(version));
    renderPanel(client);

    expect(await screen.findByLabelText("Kata issue detail")).toBeTruthy();
    expect(projectIssueDetail).toHaveBeenCalledTimes(1);
  });

  it("manually refreshes links and selected detail", async () => {
    const { client, get } = listAndDetailClient([link()]);
    renderPanel(client);
    await screen.findByLabelText("Kata issue detail");

    await fireEvent.click(screen.getByRole("button", { name: "Refresh Kata issue" }));
    await waitFor(() => expect(get).toHaveBeenCalledTimes(4));
  });

  it("opens the selected issue with the pinned daemon launch target", async () => {
    const { popup, popupDocument, replace } = popupFixture();
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    renderPanel(
      forgeClient({
        listLinks: vi.fn().mockResolvedValue(response([link()])),
        getDetail: vi.fn().mockResolvedValue(detail()),
        getLaunchTarget: vi.fn().mockResolvedValue({ available: true, url: "http://127.0.0.1:4222/issues/issue-1" }),
      }),
    );
    await screen.findByLabelText("Kata issue detail");

    await fireEvent.click(screen.getByRole("button", { name: "Open in Kata" }));

    expect(open).toHaveBeenCalledWith("about:blank", "_blank");
    await waitFor(() => expect(popupDocument.body.querySelector("a")).not.toBeNull());
    const launch = popupDocument.body.querySelector("a");
    expect(launch?.href).toBe("http://127.0.0.1:4222/issues/issue-1");
    expect(launch?.referrerPolicy).toBe("no-referrer");
    expect(launch?.rel).toBe("noreferrer");
    expect(popupDocument.head.querySelector('meta[name="referrer"]')?.getAttribute("content")).toBe("no-referrer");
    expect(replace).not.toHaveBeenCalled();
    expect(popup.opener).toBeNull();
  });

  it("rejects an unsafe daemon launch target before navigating the popup", async () => {
    const { popup, popupDocument } = popupFixture();
    vi.spyOn(window, "open").mockReturnValue(popup);
    renderPanel(
      forgeClient({
        listLinks: vi.fn().mockResolvedValue(response([link()])),
        getDetail: vi.fn().mockResolvedValue(detail()),
        getLaunchTarget: vi.fn().mockResolvedValue({ available: true, url: "javascript:alert(document.cookie)" }),
      }),
    );
    await screen.findByLabelText("Kata issue detail");

    await fireEvent.click(screen.getByRole("button", { name: "Open in Kata" }));

    await waitFor(() => expect(popup.close).toHaveBeenCalledTimes(1));
    expect(popupDocument.body.querySelector("a")).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain("safe HTTP or HTTPS URL");
  });

  it("closes a reserved popup when selection changes during launch lookup", async () => {
    let resolveLaunch!: (value: { available: true; url: string }) => void;
    const launch = new Promise<{ available: true; url: string }>((resolve) => {
      resolveLaunch = resolve;
    });
    const { popup, popupDocument, close } = popupFixture();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const links = [link(), link({ issue_uid: "issue-2", reference: "KT-2", title: "Second task" })];
    renderPanel(
      forgeClient({
        listLinks: vi.fn().mockResolvedValue(response(links)),
        getDetail: vi.fn(({ issueUid }: { issueUid: string }) =>
          Promise.resolve(detail("0.10.0", issueUid === "issue-2" ? "Second task" : "Keep one Kata UI")),
        ),
        getLaunchTarget: vi.fn(() => launch),
      }),
    );
    await screen.findByLabelText("Kata issue detail");

    await fireEvent.click(screen.getByRole("button", { name: "Open in Kata" }));
    await fireEvent.click(screen.getByRole("button", { name: /KT-2 Second task/ }));
    resolveLaunch({ available: true, url: "https://kata.example.test/issues/issue-1" });

    await waitFor(() => expect(close).toHaveBeenCalledTimes(1));
    expect(popupDocument.body.querySelector("a")).toBeNull();
    expect(screen.getByRole("button", { name: "Open in Kata" })).toBeTruthy();
  });

  it("creates or opens the mapped workspace", async () => {
    const createLink = link({
      project_name: "Forge",
      workspace: { available: true, item_key: "item-key", item_type: "kata_task" },
    });
    const createClient = listAndDetailClient([createLink]);
    const post = vi.fn().mockResolvedValue({ id: "workspace-new" });
    const created = renderPanel(forgeClient({ ...createClient.methods, createWorkspace: post }));
    await screen.findByLabelText("Kata issue detail");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(created.navigate).toHaveBeenCalledWith("/terminal/workspace-new"));
    expect(post).toHaveBeenCalledWith(expect.objectContaining({ project_name: "Forge" }));
    created.unmount();

    const existing = link({
      workspace: { available: true, existing_workspace: { id: "workspace-existing", status: "ready" } },
    });
    const existingClient = listAndDetailClient([existing]);
    const opened = renderPanel(existingClient.client);
    await screen.findByLabelText("Kata issue detail");
    await fireEvent.click(screen.getByRole("button", { name: "Open workspace" }));
    expect(opened.navigate).toHaveBeenCalledWith("/terminal/workspace-existing");
  });

  it("shares pending Kata workspace creation across remounts and only navigates the current panel", async () => {
    let resolveCreate!: (value: { id: string; status: string }) => void;
    const post = vi.fn(
      () =>
        new Promise<{ id: string; status: string }>((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const createLink = link({
      issue_uid: "issue-remount",
      reference: "KT-remount",
      workspace: { available: true, item_key: "item-remount", item_type: "kata_task" },
    });
    const firstClient = listAndDetailClient([createLink]);
    const first = renderPanel(forgeClient({ ...firstClient.methods, createWorkspace: post }));
    await screen.findByLabelText("Kata issue detail");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    first.unmount();

    const secondClient = listAndDetailClient([createLink]);
    const second = renderPanel(forgeClient({ ...secondClient.methods, createWorkspace: post }));
    await screen.findByLabelText("Kata issue detail");
    const pendingButton = screen.getByRole("button", { name: "Creating…" });
    expect((pendingButton as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(pendingButton);
    expect(post).toHaveBeenCalledTimes(1);

    resolveCreate({ id: "workspace-remount", status: "ready" });

    const openButton = await screen.findByRole("button", { name: "Open workspace" });
    expect(first.navigate).not.toHaveBeenCalled();
    await fireEvent.click(openButton);
    expect(second.navigate).toHaveBeenCalledWith("/terminal/workspace-remount");
  });

  it("does not let an A-to-B-to-A selection cycle reclaim an older workspace request", async () => {
    let resolveCreate!: (value: { id: string; status: string }) => void;
    const post = vi.fn(
      () =>
        new Promise<{ id: string; status: string }>((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const workspace = { available: true, item_key: "item-key", item_type: "kata_task" } as const;
    const links = [
      link({ workspace }),
      link({ issue_uid: "issue-2", reference: "KT-2", title: "Second task", workspace }),
    ];
    const panel = renderPanel(
      forgeClient({
        listLinks: vi.fn().mockResolvedValue(response(links)),
        getDetail: vi.fn(({ issueUid }: { issueUid: string }) =>
          Promise.resolve(detail("0.10.0", issueUid === "issue-2" ? "Second task" : "Keep one Kata UI")),
        ),
        createWorkspace: post,
      }),
    );
    await screen.findByLabelText("Kata issue detail");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await fireEvent.click(screen.getByRole("button", { name: /KT-2 Second task/ }));
    await fireEvent.click(screen.getByRole("button", { name: /KT-1 Keep one Kata UI/ }));

    resolveCreate({ id: "workspace-cycle", status: "ready" });

    await screen.findByRole("button", { name: "Open workspace" });
    expect(panel.navigate).not.toHaveBeenCalled();
  });

  it("ignores an unlink failure after the selected Kata issue changes", async () => {
    let rejectDelete!: (error: Error) => void;
    const remove = vi.fn(
      () =>
        new Promise<never>((_resolve, reject) => {
          rejectDelete = reject;
        }),
    );
    const links = [link(), link({ issue_uid: "issue-2", reference: "KT-2", title: "Second task" })];
    const { methods } = listAndDetailClient(links);
    renderPanel(forgeClient({ ...methods, deleteLink: remove }));
    await screen.findByLabelText("Kata issue detail");

    await fireEvent.click(screen.getByRole("button", { name: "Unlink KT-1" }));
    await fireEvent.click(screen.getByRole("button", { name: /KT-2 Second task/ }));
    rejectDelete(new Error("Old unlink failed."));

    await waitFor(() => expect(screen.getByRole("button", { name: "Open in Kata" })).toBeTruthy());
    expect(screen.queryByText("Old unlink failed.")).toBeNull();
  });

  it("keeps Forge link controls visible when Kata projection throws", async () => {
    vi.mocked(projectIssueDetail).mockImplementationOnce(() => {
      throw new Error("package projection failed");
    });
    const { client } = listAndDetailClient([link()]);
    renderPanel(client);

    expect(await screen.findByText("Kata issue detail is unavailable.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Unlink KT-1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open in Kata" })).toBeTruthy();
  });

  it("recovers the detail boundary after a successful refresh", async () => {
    vi.mocked(projectIssueDetail)
      .mockImplementationOnce(() => {
        throw new Error("package projection failed");
      })
      .mockImplementation((wire) => ({
        issue: {
          uid: wire.issue.uid,
          projectUID: wire.issue.project_uid ?? "",
          projectName: wire.issue.project_name ?? "",
          reference: wire.issue.qualified_id ?? wire.issue.uid,
          title: wire.issue.title,
          body: wire.issue.body ?? "",
          status: wire.issue.status,
          checklist: [],
          labels: [],
        },
        comments: [],
        links: [],
        children: [],
        pendingClaims: [],
      }));
    const { client } = listAndDetailClient([link()]);
    renderPanel(client);
    expect(await screen.findByText("Kata issue detail is unavailable.")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Refresh Kata issue" }));

    expect(await screen.findByLabelText("Kata issue detail")).toBeTruthy();
  });
});
