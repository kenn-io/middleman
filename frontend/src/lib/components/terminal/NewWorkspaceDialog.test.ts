import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect } from "effect";
import {
  discardWorkspaceLaunch,
  resetWorkspaceCreatePendingForTest,
} from "../../stores/workspace-create-pending.svelte.js";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { STORES_KEY } from "../../context.js";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";

import NewWorkspaceDialog from "./NewWorkspaceDialog.svelte";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockNavigate = vi.fn();
const runtimeCapture = vi.hoisted(() => ({ current: undefined as OwnedAppRuntime | undefined }));
const launchTargets = [
  {
    key: "codex",
    label: "Codex",
    kind: "agent" as const,
    source: "builtin" as const,
    command: ["codex"],
    available: true,
    disabled_reason: "",
  },
];

vi.mock("../../api/generated/index.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/generated/index.js")>();
  return {
    ...actual,
    KataService: {
      ...actual.KataService,
      listKataDaemons: async () => (await mockGet("/kata/daemons")).data,
    },
  };
});

vi.mock("../../app/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.js")>();
  const { makeGeneratedClientFromRouteMocks } = await import("../../testing/test/route-mock-client.js");
  const client = {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  };
  return {
    ...actual,
    makeAppRuntime: () => actual.makeAppRuntime(makeGeneratedClientFromRouteMocks(client)),
  };
});

vi.mock("../../app/runtime-context.js", () => ({
  getAppRuntime: () => {
    const runtime = runtimeCapture.current;
    if (runtime === undefined) throw new Error("new workspace dialog test runtime is not initialized");
    return runtime;
  },
}));

vi.mock("../../stores/router.svelte.js", () => ({
  navigate: (path: string) => mockNavigate(path),
}));

function repoFixture(owner: string, name: string, platformHost = "github.com", platform = "github") {
  return {
    ID: 1,
    Name: name,
    Owner: owner,
    Platform: platform,
    PlatformHost: platformHost,
  };
}

async function renderDialog(props: Record<string, unknown> = {}) {
  const view = render(NewWorkspaceDialog, {
    props: { open: true, onClose: vi.fn(), ...props },
    context: new Map([
      [
        STORES_KEY,
        {
          settings: { getLaunchTargets: () => launchTargets },
        },
      ],
    ]),
  });
  await waitFor(() =>
    expect(mockGet).toHaveBeenCalledWith("/repos", expect.objectContaining({ signal: expect.any(AbortSignal) })),
  );
  return view;
}

function repoPicker(): HTMLElement {
  return screen.getByRole("button", { name: "Filter repositories" });
}

// The repository picker is kit-ui's Typeahead: the trigger opens the list and
// its option rows commit on mousedown, before focusout can dismiss the panel.
async function pickRepo(label: string): Promise<void> {
  await fireEvent.click(repoPicker());
  await fireEvent.mouseDown(await screen.findByRole("option", { name: new RegExp(label) }));
}

describe("NewWorkspaceDialog", () => {
  beforeEach(() => {
    runtimeCapture.current = makeAppRuntime();
    resetWorkspaceCreatePendingForTest();
    localStorage.clear();
    mockGet.mockReset();
    mockPost.mockReset();
    mockNavigate.mockReset();
    mockGet.mockResolvedValue({
      data: [repoFixture("acme", "widget"), repoFixture("acme", "gadget")],
    });
    mockPost.mockResolvedValue({ data: { id: "ws-new" } });
  });

  afterEach(async () => {
    resetWorkspaceCreatePendingForTest();
    cleanup();
    if (runtimeCapture.current) await Effect.runPromise(runtimeCapture.current.disposeEffect);
    runtimeCapture.current = undefined;
  });

  it("creates a workspace for the selected repo with no branch when the field is empty", async () => {
    const onClose = vi.fn();
    await renderDialog({ onClose });

    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    const [path, options] = mockPost.mock.calls[0] as [
      string,
      { params: { path: Record<string, string> }; body: Record<string, string> },
    ];
    expect(path).toBe("/repo/{provider}/{owner}/{name}/workspaces");
    expect(options.params.path).toEqual({ provider: "github", owner: "acme", name: "widget" });
    expect(options.body).toEqual({});
    expect(mockNavigate).toHaveBeenCalledWith("/terminal/ws-new");
    expect(onClose).toHaveBeenCalled();
  });

  it("keeps a native submit control so Enter submits the form", async () => {
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    const primary = screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement;
    expect(primary.type).toBe("submit");
    const form = screen.getByLabelText("Branch name").closest("form");
    expect(form).not.toBeNull();

    await fireEvent.submit(form!);

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
  });

  it("creates only from the primary action", async () => {
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBeNull();
  });

  it("retains an agent choice through suggested-branch retry", async () => {
    mockPost
      .mockResolvedValueOnce({
        error: {
          code: "branchConflict",
          detail: "branch exists",
          details: {
            branch: "spike/thing",
            suggestedBranch: "spike/thing-2",
          },
        },
      })
      .mockResolvedValueOnce({ data: { id: "ws-second" } });
    await renderDialog();
    await fireEvent.click(
      screen.getByRole("button", {
        name: "Create workspace options",
      }),
    );
    await fireEvent.click(screen.getByRole("menuitem", { name: "Codex" }));
    await fireEvent.click(
      await screen.findByRole("button", {
        name: /Use spike\/thing-2/,
      }),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(2));
    expect(discardWorkspaceLaunch("ws-second", undefined)).toBe("codex");
  });

  it("sends the typed branch name", async () => {
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    await fireEvent.input(screen.getByLabelText("Branch name"), {
      target: { value: "  spike/rate-limits  " },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    const [, options] = mockPost.mock.calls[0] as [string, { body: Record<string, string> }];
    expect(options.body).toEqual({ branch: "spike/rate-limits" });
  });

  it("creates against the repo the user picks from the list", async () => {
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await pickRepo("acme/gadget");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    const [, options] = mockPost.mock.calls[0] as [string, { params: { path: Record<string, string> } }];
    expect(options.params.path.name).toBe("gadget");
  });

  it("defaults to the repo work was last started in", async () => {
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await pickRepo("acme/gadget");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    cleanup();

    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/gadget"));
  });

  it("prefers the seeded repo over the last used one", async () => {
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await pickRepo("acme/gadget");
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    cleanup();

    await renderDialog({
      seedRepo: { provider: "github", platformHost: "github.com", owner: "acme", name: "widget" },
    });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
  });

  it("falls back to the first repo when the last used one is no longer tracked", async () => {
    localStorage.setItem("kenn-forge:workspace:new_repo", "github/github.com/acme/retired");
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
  });

  it("preselects the seeded repo", async () => {
    await renderDialog({
      seedRepo: { provider: "github", platformHost: "github.com", owner: "acme", name: "gadget" },
    });

    await waitFor(() => expect(repoPicker().textContent).toContain("acme/gadget"));
  });

  it("requires an explicit choice when the seeded repo is not offered", async () => {
    // A seed that no longer resolves (for example a repository hidden from
    // the UI) must not silently divert the workspace to another repository,
    // even when a last-used repo is remembered.
    localStorage.setItem("kenn-forge:workspace:new_repo", "github/github.com/acme/gadget");
    await renderDialog({
      seedRepo: { provider: "github", platformHost: "github.com", owner: "acme", name: "concealed" },
    });

    await waitFor(() =>
      expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true),
    );
    expect(repoPicker().textContent).not.toContain("acme/widget");
    expect(repoPicker().textContent).not.toContain("acme/gadget");
  });

  it("keeps requiring a choice for an unavailable seed after switching sources", async () => {
    // The seed promise holds across source switches: leaving for Kata and
    // returning to the repository source must not divert to the last-used
    // or first repository when the seed is still unavailable.
    localStorage.setItem("kenn-forge:workspace:new_repo", "github/github.com/acme/gadget");
    await renderDialog({
      seedRepo: { provider: "github", platformHost: "github.com", owner: "acme", name: "concealed" },
    });
    await waitFor(() =>
      expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true),
    );

    await fireEvent.click(screen.getByRole("button", { name: "Kata issue" }));
    await fireEvent.click(screen.getByRole("button", { name: "Repository" }));
    await waitFor(() => expect(mockGet.mock.calls.filter((c) => c[0] === "/repos")).toHaveLength(2));

    // Once the reload settles, a wrong automatic selection would surface in
    // the picker trigger and enable submission. The trigger renders as a
    // button while closed and a combobox while open, so accept either.
    await waitFor(() => {
      const trigger =
        screen.queryByRole("button", { name: "Filter repositories" }) ??
        screen.getByRole("combobox", { name: "Filter repositories" });
      expect(trigger.textContent).not.toContain("Loading repositories");
      expect(trigger.textContent).not.toContain("acme/gadget");
      expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true);
    });
  });

  it("accepts the current Kata API schema", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/repos") return Promise.resolve({ data: [repoFixture("acme", "widget")] });
      if (path === "/kata/daemons") {
        return Promise.resolve({
          data: {
            daemons: [
              {
                id: "current",
                url: "http://current",
                health: "connected",
                auth: "none",
                default: true,
                api_schema_version: "0.13.0",
              },
            ],
          },
        });
      }
      throw new Error(`unexpected GET ${path}`);
    });
    await renderDialog();

    await fireEvent.click(screen.getByRole("button", { name: "Kata issue" }));
    const search = (await screen.findByRole("searchbox", { name: "Search Kata issues" })) as HTMLInputElement;
    await waitFor(() => expect(search.disabled).toBe(false));
  });

  it("routes non-default hosts through the host-scoped path", async () => {
    mockGet.mockResolvedValue({ data: [repoFixture("acme", "widget", "git.example.test", "forgejo")] });
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    const [path, options] = mockPost.mock.calls[0] as [string, { params: { path: Record<string, string> } }];
    expect(path).toBe("/host/{platform_host}/repo/{provider}/{owner}/{name}/workspaces");
    expect(options.params.path.platform_host).toBe("git.example.test");
  });

  it("creates and opens repository workspaces on the selected spoke", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/repos") return Promise.resolve({ data: [repoFixture("acme", "widget")] });
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub-node",
                federationRole: "hub",
                kind: "self",
                name: "Studio hub",
                operationAvailability: { workspaceWrite: { available: true } },
              },
              {
                configKey: "build-node",
                federationRole: "spoke",
                kind: "remote",
                name: "Build spoke",
                operationAvailability: { workspaceWrite: { available: true } },
              },
            ],
          },
        });
      }
      throw new Error(`unexpected GET ${path}`);
    });
    await renderDialog();

    const machine = await screen.findByRole("combobox", {
      name: "Workspace machine: Studio hub (this machine)",
    });
    await fireEvent.click(machine);
    await fireEvent.click(screen.getByRole("option", { name: "Build spoke" }));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    const [path, options] = mockPost.mock.calls[0] as [
      string,
      { params: { path: Record<string, string> }; body: Record<string, string> },
    ];
    expect(path).toBe("/fleet/hosts/{host_key}/repo/{provider}/{owner}/{name}/workspaces");
    expect(options.params.path).toEqual({
      host_key: "build-node",
      provider: "github",
      owner: "acme",
      name: "widget",
    });
    expect(options.body).toEqual({});
    expect(mockNavigate).toHaveBeenCalledWith("/terminal/fleet/build-node/ws-new");
  });

  it("offers the suggested branch when the requested one already exists", async () => {
    mockPost.mockResolvedValue({
      error: {
        code: "branchConflict",
        detail: "A local branch with the requested name already exists.",
        details: { branch: "spike/thing", suggestedBranch: "spike/thing-2" },
      },
    });
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    await fireEvent.input(screen.getByLabelText("Branch name"), {
      target: { value: "spike/thing" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    const suggestion = await screen.findByRole("button", { name: /Use spike\/thing-2/ });
    expect(mockNavigate).not.toHaveBeenCalled();

    mockPost.mockResolvedValue({ data: { id: "ws-second" } });
    await fireEvent.click(suggestion);
    expect((screen.getByLabelText("Branch name") as HTMLInputElement).value).toBe("spike/thing-2");

    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(2));
    const [, options] = mockPost.mock.calls[1] as [string, { body: Record<string, string> }];
    expect(options.body).toEqual({ branch: "spike/thing-2" });
  });

  it("keeps the dialog open and reports the failure when the create fails", async () => {
    const onClose = vi.fn();
    mockPost.mockResolvedValue({ error: { detail: "repository not tracked" } });
    await renderDialog({ onClose });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    expect((await screen.findByRole("alert")).textContent).toContain("repository not tracked");
    expect(onClose).not.toHaveBeenCalled();
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("abandons the navigation when the dialog is dismissed mid-create", async () => {
    let resolvePost: (value: unknown) => void = () => {};
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve;
        }),
    );
    const { rerender } = await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    // Escape and backdrop clicks close the shared modal even while a create is
    // in flight, so the created workspace must not yank the user away.
    await rerender({ open: false });
    resolvePost({ data: { id: "ws-new" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("publishes an agent choice from a stale dialog session without affecting the fresh session", async () => {
    let resolveStale: (value: unknown) => void = () => {};
    mockPost.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveStale = resolve;
        }),
    );
    const onCreated = vi.fn();
    const { rerender } = await renderDialog({ onCreated });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace options" }));
    await fireEvent.click(screen.getByRole("menuitem", { name: "Codex" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    await rerender({ open: false, onCreated });
    await rerender({ open: true, onCreated });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    resolveStale({ data: { id: "ws-stale" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockNavigate).not.toHaveBeenCalled();
    expect(onCreated).not.toHaveBeenCalled();
    expect(discardWorkspaceLaunch("ws-stale", undefined)).toBe("codex");

    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("ws-new", undefined));
    expect(discardWorkspaceLaunch("ws-new", undefined)).toBeNull();
  });

  it("keeps the form locked when a stale create resolves under a newer one", async () => {
    const resolvers: ((value: unknown) => void)[] = [];
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(resolve);
        }),
    );
    const { rerender } = await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    // Dismiss mid-create, reopen, and start a second create; the first request
    // is still outstanding.
    await rerender({ open: false });
    await rerender({ open: true });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(2));

    resolvers[0]?.({ data: { id: "ws-stale" } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    // The second create still owns the dialog, so the form stays locked (the
    // submit button still reads "Creating…") and no third request can fire.
    const submit = screen.getByRole("button", { name: "Creating…" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    await fireEvent.click(submit);
    expect(mockPost).toHaveBeenCalledTimes(2);
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("cannot submit against the previous selection while a reopen reloads", async () => {
    const { rerender } = await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));
    await rerender({ open: false });

    let resolveGet: (value: unknown) => void = () => {};
    mockGet.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveGet = resolve;
        }),
    );
    await rerender({ open: true });

    await waitFor(() => expect(repoPicker().textContent).toContain("Loading repositories"));
    expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true);

    resolveGet({ data: [repoFixture("acme", "gadget")] });
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/gadget"));
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("interrupts the repository load when the dialog closes", async () => {
    let repositoryLoadInterrupted = false;
    mockGet.mockImplementation(
      (_path: string, options?: { signal?: AbortSignal }) =>
        new Promise(() => {
          options?.signal?.addEventListener("abort", () => {
            repositoryLoadInterrupted = true;
          });
        }),
    );

    const { rerender } = await renderDialog();
    await rerender({ open: false });

    expect(repositoryLoadInterrupted).toBe(true);
  });

  it("reports a rejected create instead of failing silently", async () => {
    mockPost.mockRejectedValue(new Error("network down"));
    await renderDialog();
    await waitFor(() => expect(repoPicker().textContent).toContain("acme/widget"));

    await fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));

    expect((await screen.findByRole("alert")).textContent).toContain("Could not create workspace");
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("stops loading and reports when the repository request is rejected", async () => {
    mockGet.mockRejectedValue(new Error("network down"));
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("No tracked repositories yet"));
    expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("cannot submit when the repository list fails to load", async () => {
    mockGet.mockResolvedValue({ error: { detail: "repositories unavailable" } });
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("No tracked repositories yet"));
    expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("cannot submit when no repository is tracked", async () => {
    mockGet.mockResolvedValue({ data: [] });
    await renderDialog();

    await waitFor(() => expect(repoPicker().textContent).toContain("No tracked repositories yet"));
    expect((screen.getByRole("button", { name: "Create workspace" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
