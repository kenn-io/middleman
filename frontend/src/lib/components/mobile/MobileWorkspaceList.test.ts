import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { getTopFrame, resetModalStack } from "../../stores/keyboard/modal-stack.svelte.js";
import { isNewWorkspaceDialogOpen, resetNewWorkspaceDialogState } from "../../stores/new-workspace.svelte.js";
import * as workspaceHost from "../../stores/workspace-host.svelte.js";
import MobileWorkspaceList from "./MobileWorkspaceListTestHarness.svelte";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockDelete = vi.fn();
let workspaceEventListener: EventListener | null = null;

vi.mock("../../app/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.js")>();
  const { makeGeneratedClientFromRouteMocks } = await import("../../testing/test/route-mock-client.js");
  const client = {
    DELETE: (...args: unknown[]) => mockDelete(...args),
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  };
  return { ...actual, makeAppRuntime: () => actual.makeAppRuntime(makeGeneratedClientFromRouteMocks(client)) };
});

class MockEventSource {
  addEventListener = vi.fn((type: string, listener: EventListener) => {
    if (type === "workspace_status") workspaceEventListener = listener;
  });
  removeEventListener = vi.fn();
  close = vi.fn();
}

const fixture = {
  id: "ws-1",
  created_at: "2026-08-11T12:00:00Z",
  git_head_ref: "feature/mobile-workspaces",
  item_number: 42,
  item_type: "pull_request",
  source_item_visible: true,
  platform_host: "github.com",
  repo_name: "widgets",
  repo_owner: "acme",
  status: "ready",
  tmux_activity_source: "unknown",
  tmux_last_output_at: null,
  tmux_working: false,
  worktree_path: "/tmp/ws-1",
  mr_title: "Build mobile workspaces",
  mr_state: "open",
  mr_additions: 120,
  mr_deletions: 12,
  repo: {
    provider: "github",
    platform_host: "github.com",
    owner: "acme",
    name: "widgets",
    repo_path: "acme/widgets",
  },
};

describe("MobileWorkspaceList", () => {
  beforeEach(() => {
    mockGet.mockReset();
    mockPost.mockReset();
    mockDelete.mockReset();
    workspaceEventListener = null;
    localStorage.clear();
    resetModalStack();
    resetNewWorkspaceDialogState();
    vi.stubGlobal("EventSource", MockEventSource);
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({ data: { hosts: [], workspaces: [fixture] } });
      }
      return Promise.resolve({ data: {} });
    });
  });

  afterEach(() => {
    cleanup();
    resetModalStack();
    vi.unstubAllGlobals();
  });

  it("filters rows and opens the selected workspace", async () => {
    const onOpen = vi.fn();
    render(MobileWorkspaceList, { props: { onOpen, onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");

    await fireEvent.input(screen.getByRole("searchbox", { name: "Filter workspaces" }), {
      target: { value: "unrelated" },
    });
    expect(screen.queryByText("Build mobile workspaces")).toBeNull();

    await fireEvent.input(screen.getByRole("searchbox", { name: "Filter workspaces" }), {
      target: { value: "mobile" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Open workspace Build mobile workspaces" }));
    expect(onOpen).toHaveBeenCalledWith("ws-1", undefined);
  });

  it("offers the first push when the configured upstream branch is missing", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({ data: { hosts: [], workspaces: [{ ...fixture, branch_upstream_missing: true }] } });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");
    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Build mobile workspaces" }));

    const actions = await screen.findByRole("dialog", { name: "Workspace actions" });
    expect(within(actions).getByRole("button", { name: "Push branch" })).toBeTruthy();
  });

  it.each([
    ["working", "Working", "working"],
    ["approval", "Approval", "waiting for approval"],
    ["input", "Input", "waiting for input"],
    ["done", "Done", "done"],
  ] as const)("shows the hook-reported %s agent state", async (agentState, label, announcement) => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: { hosts: [], workspaces: [{ ...fixture, agent_state: agentState }] },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    expect(await screen.findByText(label)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: `Open workspace Build mobile workspaces, agent ${announcement}` }),
    ).toBeTruthy();
  });

  it("leaves the agent state empty when no hook has reported", async () => {
    localStorage.setItem("kenn-forge:workspaceListSort", "agent-status");

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    await screen.findByText("Build mobile workspaces");
    expect(screen.queryByText("Unreported")).toBeNull();
    expect(document.querySelector(".mobile-workspace-row__sort-time")?.getAttribute("datetime")).toBe(
      fixture.created_at,
    );
    expect(screen.getByRole("button", { name: "Open workspace Build mobile workspaces" })).toBeTruthy();
  });

  it("shows sort timestamps without exposing a dead linked-item action for Kata workspaces", async () => {
    localStorage.setItem("kenn-forge:workspaceListSort", "created");
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [],
            workspaces: [
              {
                ...fixture,
                item_type: "kata_task",
                item_number: 0,
                kata: {
                  daemon_id: "desktop",
                  project_uid: "project-7",
                  project_name: "Example project",
                  issue_uid: "issue-7",
                  short_id: "task-7",
                  title: "Build mobile workspaces",
                },
              },
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    await screen.findByText("Build mobile workspaces");
    expect(screen.queryByRole("button", { name: /Open linked item/ })).toBeNull();
    expect(document.querySelector(".mobile-workspace-row__sort-time")?.getAttribute("datetime")).toBe(
      fixture.created_at,
    );
  });

  it("hides actions for a removed workspace source item", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: { hosts: [], workspaces: [{ ...fixture, source_item_visible: false }] },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    await screen.findByText("Build mobile workspaces");
    expect(screen.queryByRole("button", { name: /Open linked item/ })).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Build mobile workspaces" }));
    const actions = await screen.findByRole("dialog", { name: "Workspace actions" });
    expect(within(actions).queryByRole("button", { name: "Open item on provider" })).toBeNull();
    expect(within(actions).queryByRole("button", { name: "Copy item URL" })).toBeNull();
  });

  it("keeps Fleet linked items as passive metadata", async () => {
    const fleetWorkspace = {
      ...fixture,
      id: "fleet-ws-1",
      mr_title: "Fleet workspace",
      fleet_host_key: "peer-a",
      fleet_host_name: "Peer A",
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "peer-a",
                diagnostics: [],
                id: "peer-a",
                kind: "remote",
                name: "Peer A",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [fleetWorkspace],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    await screen.findByText("Fleet workspace");
    expect(screen.getByText("#42")).toBeTruthy();
    const fleetItem = screen.getByRole("button", { name: "Open linked item #42" }) as HTMLButtonElement;
    expect(fleetItem.disabled).toBe(true);
    expect(mockGet).not.toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces", expect.anything());
  });

  it("exposes the View sheet and persists display choices", async () => {
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");

    await fireEvent.click(screen.getByRole("button", { name: "View workspace options" }));
    expect(screen.getByRole("dialog", { name: "View workspace options" })).toBeTruthy();
    expect(getTopFrame()?.frameId).toBe("mobile-workspace-view-options");
    expect(screen.getByRole("radio", { name: /^Terminal activity/ })).toBeTruthy();

    await fireEvent.click(screen.getByRole("radio", { name: /^Created/ }));
    expect(document.querySelector(".mobile-workspace-row__sort-time")?.getAttribute("datetime")).toBe(
      fixture.created_at,
    );

    await fireEvent.click(screen.getByRole("switch", { name: "Show organization names" }));
    await waitFor(() => {
      expect(localStorage.getItem("kenn-forge:workspaceListDisplayOptions")).toContain('"showOrgNames":false');
    });
  });

  it("opens New Workspace from the list header", async () => {
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");
    await fireEvent.click(screen.getByRole("button", { name: "New workspace" }));
    expect(isNewWorkspaceDialogOpen()).toBe(true);
  });

  it("invalidates shared terminal and route state after deletion", async () => {
    const notifyWorkspaceDeleted = vi.spyOn(workspaceHost, "notifyWorkspaceDeleted");
    mockDelete.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { ok: true, status: 204 },
    });
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");

    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Build mobile workspaces" }));
    const actions = await screen.findByRole("dialog", { name: "Workspace actions" });
    expect(getTopFrame()?.frameId).toBe("mobile-workspace-actions");
    await fireEvent.click(within(actions).getByRole("button", { name: "Delete workspace…" }));
    const confirmation = await screen.findByRole("dialog", { name: "Delete workspace?" });
    await fireEvent.click(within(confirmation).getByRole("button", { name: "Delete workspace" }));

    await waitFor(() => {
      expect(notifyWorkspaceDeleted).toHaveBeenCalledWith("ws-1", undefined, {
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
        number: 42,
        itemType: "pull_request",
      });
    });
    notifyWorkspaceDeleted.mockRestore();
  });

  it("requires separate confirmation before forcing a dirty workspace deletion", async () => {
    mockDelete
      .mockResolvedValueOnce({
        error: {
          type: "about:blank",
          title: "Conflict",
          status: 409,
          detail: "worktree has uncommitted changes; retry with force",
          code: "worktreeDirty",
        },
        response: new Response(null, { status: 409 }),
      })
      .mockResolvedValueOnce({
        data: undefined,
        error: undefined,
        response: new Response(null, { status: 204 }),
      });
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");

    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Build mobile workspaces" }));
    await fireEvent.click(
      within(await screen.findByRole("dialog", { name: "Workspace actions" })).getByRole("button", {
        name: "Delete workspace…",
      }),
    );
    await fireEvent.click(
      within(await screen.findByRole("dialog", { name: "Delete workspace?" })).getByRole("button", {
        name: "Delete workspace",
      }),
    );

    const forceConfirmation = await screen.findByRole("dialog", { name: "Force delete workspace?" });
    expect(forceConfirmation.textContent).toContain("discards uncommitted changes");
    expect(mockDelete).toHaveBeenNthCalledWith(1, "/workspaces/{id}", {
      params: { path: { id: "ws-1" } },
      signal: expect.any(AbortSignal),
    });

    await fireEvent.click(within(forceConfirmation).getByRole("button", { name: "Force delete workspace" }));
    await waitFor(() => {
      expect(mockDelete).toHaveBeenNthCalledWith(2, "/workspaces/{id}", {
        params: { path: { id: "ws-1" }, query: { force: true } },
        signal: expect.any(AbortSignal),
      });
    });
  });

  it("offers explicit force-delete recovery for a failed Fleet deletion", async () => {
    const fleetWorkspace = {
      ...fixture,
      id: "fleet-ws-failed",
      mr_title: "Failed Fleet deletion",
      status: "deletion_failed",
      error_message: "workspace has uncommitted changes: notes.txt",
      fleet_host_key: "peer-a",
      fleet_host_name: "Peer A",
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "peer-a",
                diagnostics: [],
                id: "peer-a",
                kind: "remote",
                name: "Peer A",
                operationAvailability: {
                  workspaceRead: { available: true },
                  workspaceWrite: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [fleetWorkspace],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });
    mockDelete.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response(null, { status: 204 }),
    });
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Failed Fleet deletion");

    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Failed Fleet deletion" }));
    const actions = await screen.findByRole("dialog", { name: "Workspace actions" });
    await fireEvent.click(within(actions).getByRole("button", { name: "Force delete workspace…" }));
    const confirmation = await screen.findByRole("dialog", { name: "Force delete workspace?" });
    await fireEvent.click(within(confirmation).getByRole("button", { name: "Force delete workspace" }));

    await waitFor(() => {
      expect(mockDelete).toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces/{id}", {
        params: {
          path: { host_key: "peer-a", id: "fleet-ws-failed" },
          query: { force: true },
        },
        signal: expect.any(AbortSignal),
      });
    });
  });

  it.each([
    ["deleting", "Delete workspace…", true],
    ["deletion_failed", "Retry deletion…", false],
  ] as const)("disables ordinary actions while workspace status is %s", async (status, deleteLabel, deleteDisabled) => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [],
            workspaces: [{ ...fixture, status, commits_ahead: 1 }],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });
    await screen.findByText("Build mobile workspaces");
    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Build mobile workspaces" }));

    const actions = await screen.findByRole("dialog", { name: "Workspace actions" });
    for (const label of ["Push branch", "Refresh workspace", "Reveal worktree"]) {
      expect((within(actions).getByRole("button", { name: label }) as HTMLButtonElement).disabled).toBe(true);
    }
    expect((within(actions).getByRole("button", { name: deleteLabel }) as HTMLButtonElement).disabled).toBe(
      deleteDisabled,
    );
    if (status === "deletion_failed") {
      expect(
        (within(actions).getByRole("button", { name: "Force delete workspace…" }) as HTMLButtonElement).disabled,
      ).toBe(false);
    }
  });

  it("renders and refreshes Fleet workspaces without workspace-list fan-out", async () => {
    const fleetWorkspace = {
      ...fixture,
      id: "fleet-ws-1",
      mr_title: "Fleet workspace",
      fleet_host_key: "peer-a",
      fleet_host_name: "Peer A",
    };
    let snapshotReads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotReads += 1;
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "self",
                diagnostics: [],
                id: "self",
                kind: "self",
                name: "This device",
                operationAvailability: {
                  workspaceRead: { available: true },
                  workspaceWrite: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "peer-a",
                diagnostics: [],
                id: "peer-a",
                kind: "remote",
                name: "Peer A",
                operationAvailability: {
                  workspaceRead: { available: true },
                  workspaceWrite: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: snapshotReads === 1 ? [fleetWorkspace] : [],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });
    mockDelete.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { ok: true, status: 204 },
    });
    render(MobileWorkspaceList, { props: { onOpen: vi.fn(), onOpenItem: vi.fn() } });

    await screen.findByText("Fleet workspace");
    await fireEvent.click(screen.getByRole("button", { name: "Workspace actions for Fleet workspace" }));
    await fireEvent.click(
      within(await screen.findByRole("dialog", { name: "Workspace actions" })).getByRole("button", {
        name: "Delete workspace…",
      }),
    );
    await fireEvent.click(
      within(await screen.findByRole("dialog", { name: "Delete workspace?" })).getByRole("button", {
        name: "Delete workspace",
      }),
    );

    await waitFor(() => expect(snapshotReads).toBeGreaterThan(1));
    expect(screen.queryByText("Fleet workspace")).toBeNull();
    expect(mockGet).not.toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces", expect.anything());
  });

  it("keeps member workspaces when a spoke loses its hub aggregate", async () => {
    const onOpen = vi.fn();
    const fleetWorkspace = {
      ...fixture,
      id: "fleet-ws-degraded",
      mr_title: "Degraded Fleet workspace",
      fleet_host_key: "peer-a",
      fleet_host_name: "Peer A",
    };
    let snapshotReads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotReads += 1;
        return Promise.resolve({
          data: {
            aggregateIncomplete: snapshotReads !== 1,
            hosts: [
              {
                configKey: "spoke-a",
                diagnostics: [],
                id: "spoke-a",
                kind: "self",
                name: "Spoke A",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "hub",
                diagnostics: [],
                error: snapshotReads === 1 ? undefined : "Hub aggregate unavailable",
                id: "hub",
                kind: "remote",
                name: "Hub",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: snapshotReads === 1,
                tmuxSessions: [],
              },
              ...(snapshotReads === 1
                ? [
                    {
                      configKey: "peer-a",
                      diagnostics: [],
                      id: "peer-a",
                      kind: "remote",
                      name: "Peer A",
                      operationAvailability: {
                        workspaceRead: { available: true },
                        workspaceWrite: { available: true },
                        terminalAttach: { available: true },
                      },
                      platform: "linux",
                      preferredTransport: "http",
                      reachable: true,
                      tmuxSessions: [],
                    },
                  ]
                : []),
            ],
            workspaces: snapshotReads === 1 ? [fleetWorkspace] : [],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });
    render(MobileWorkspaceList, { props: { onOpen, onOpenItem: vi.fn() } });

    await screen.findByText("Degraded Fleet workspace");
    await waitFor(() => expect(workspaceEventListener).not.toBeNull());
    workspaceEventListener?.(new MessageEvent("workspace_status"));

    await waitFor(() => expect(snapshotReads).toBeGreaterThan(1));
    expect(screen.getByText("Degraded Fleet workspace")).toBeTruthy();
    expect(screen.getByText("Degraded")).toBeTruthy();
    const open = screen.getByRole("button", { name: "Open workspace Degraded Fleet workspace" });
    expect((open as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(open);
    expect(onOpen).not.toHaveBeenCalled();
  });
});
