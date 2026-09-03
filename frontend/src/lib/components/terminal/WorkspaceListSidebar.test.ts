import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import WorkspaceListSidebar from "./WorkspaceListSidebarTestHarness.svelte";
import {
  getNewWorkspaceSeedRepo,
  isNewWorkspaceDialogOpen,
  resetNewWorkspaceDialogState,
} from "../../stores/new-workspace.svelte.js";
import {
  getWorkspaceRepoCatalog,
  isWorkspaceRepoCatalogReady,
  setWorkspaceRepoCatalog,
} from "../../stores/workspace-repo-catalog.svelte.js";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockDelete = vi.fn();
const mockNavigate = vi.fn();
const subscribeWorkspaceEvents = vi.fn();
let workspaceEventsSubscriber: ((event: unknown) => void) | undefined;

vi.mock("../../context.js", () => ({
  getStores: () => ({
    events: { subscribeWorkspaceEvents },
  }),
}));

vi.mock("../../app/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.js")>();
  const { makeGeneratedClientFromRouteMocks } = await import("../../testing/test/route-mock-client.js");
  const client = {
    DELETE: (...args: unknown[]) => mockDelete(...args),
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  };
  return {
    ...actual,
    makeAppRuntime: () => actual.makeAppRuntime(makeGeneratedClientFromRouteMocks(client)),
  };
});

vi.mock("../../stores/router.svelte.ts", () => ({
  navigate: (path: string) => mockNavigate(path),
}));

function emitWorkspaceStatus(): void {
  workspaceEventsSubscriber?.({ type: "workspace_status", payload: {} });
}

interface WorkspaceFixtureOptions {
  id: string;
  provider: string;
  platformHost: string;
  owner: string;
  name: string;
  number: number;
  title?: string;
  branch?: string;
  itemType?: "pull_request" | "issue" | "kata_task" | "adhoc";
  itemKey?: string;
  isDraft?: boolean;
  kata?: {
    daemon_id: string;
    project_uid: string;
    project_name?: string;
    issue_uid: string;
    short_id?: string;
    qualified_id?: string;
    title?: string;
  };
  createdAt?: string;
  tmuxLastOutputAt?: string | null;
  itemLastActivityAt?: string | null;
  additions?: number | null;
  deletions?: number | null;
  commitsAhead?: number | null;
  commitsBehind?: number | null;
  tmuxWorking?: boolean;
  tmuxPaneTitle?: string | null;
  tmuxActivitySource?: string;
  agentState?: "idle" | "working" | "input" | "approval" | "done" | null;
  agentStateUpdatedAt?: string | null;
  status?: string;
  errorMessage?: string | null;
  associatedPRNumber?: number | null;
  sourceItemVisible?: boolean;
  worktreeDirty?: boolean;
}

function workspaceFixture({
  id,
  provider,
  platformHost,
  owner,
  name,
  number,
  title = `PR ${number}`,
  branch = `feature-${number}`,
  itemType = "pull_request",
  itemKey,
  isDraft = false,
  kata,
  createdAt = "2026-05-12T12:00:00Z",
  tmuxLastOutputAt = null,
  itemLastActivityAt = null,
  additions = null,
  deletions = null,
  commitsAhead = null,
  commitsBehind = null,
  tmuxWorking = false,
  tmuxPaneTitle = null,
  tmuxActivitySource = "unknown",
  agentState = null,
  agentStateUpdatedAt = null,
  status = "ready",
  errorMessage = null,
  associatedPRNumber = null,
  sourceItemVisible = true,
  worktreeDirty,
}: WorkspaceFixtureOptions) {
  // Kata and ad-hoc workspaces carry no joined provider item metadata.
  const noProviderItem = itemType === "kata_task" || itemType === "adhoc";
  return {
    id,
    repo: {
      provider,
      platform_host: platformHost,
      owner,
      name,
      repo_path: `${owner}/${name}`,
    },
    platform_host: platformHost,
    repo_owner: owner,
    repo_name: name,
    item_type: itemType,
    item_number: number,
    ...(itemKey === undefined ? {} : { item_key: itemKey }),
    ...(kata === undefined ? {} : { kata }),
    git_head_ref: branch,
    worktree_path: `/tmp/${id}`,
    tmux_session: id,
    tmux_working: tmuxWorking,
    tmux_pane_title: tmuxPaneTitle,
    tmux_activity_source: tmuxActivitySource,
    agent_state: agentState,
    agent_state_updated_at: agentStateUpdatedAt,
    status,
    error_message: errorMessage,
    created_at: createdAt,
    tmux_last_output_at: tmuxLastOutputAt,
    item_last_activity_at: itemLastActivityAt,
    mr_title: noProviderItem ? null : title,
    mr_state: noProviderItem ? null : "open",
    mr_is_draft: noProviderItem ? null : isDraft,
    mr_additions: additions,
    mr_deletions: deletions,
    commits_ahead: commitsAhead,
    commits_behind: commitsBehind,
    associated_pr_number: associatedPRNumber,
    source_item_visible: sourceItemVisible,
    ...(worktreeDirty === undefined ? {} : { worktree_dirty: worktreeDirty }),
  };
}

// Three workspaces across two repos with distinct creation and
// activity timestamps, listed in API order (created_at DESC).
function sortFixtures() {
  return [
    workspaceFixture({
      id: "ws-new",
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "kenn-forge",
      number: 3,
      title: "Newest created",
      createdAt: "2026-05-12T12:00:00Z",
      tmuxLastOutputAt: "2026-05-12T13:00:00Z",
    }),
    workspaceFixture({
      id: "ws-mid",
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "agentsview",
      number: 2,
      title: "Most recently active",
      createdAt: "2026-05-11T12:00:00Z",
      tmuxLastOutputAt: "2026-05-14T09:00:00Z",
    }),
    workspaceFixture({
      id: "ws-old",
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "kenn-forge",
      number: 1,
      title: "Oldest without activity",
      createdAt: "2026-05-10T12:00:00Z",
    }),
  ];
}

function rowTitles(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll(".ws-name")).map((el) => el.textContent?.trim() ?? "");
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, reject, resolve };
}

describe("WorkspaceListSidebar", () => {
  beforeEach(() => {
    mockGet.mockReset();
    mockPost.mockReset();
    mockDelete.mockReset();
    mockNavigate.mockReset();
    workspaceEventsSubscriber = undefined;
    subscribeWorkspaceEvents.mockReset();
    subscribeWorkspaceEvents.mockImplementation((subscriber: (event: unknown) => void) => {
      workspaceEventsSubscriber = subscriber;
      return () => {
        if (workspaceEventsSubscriber === subscriber) workspaceEventsSubscriber = undefined;
      };
    });
    resetNewWorkspaceDialogState();
    setWorkspaceRepoCatalog(undefined, false);
    localStorage.clear();
    sessionStorage.clear();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockResolvedValue(undefined) },
    });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("loads without secure-context crypto APIs", async () => {
    vi.stubGlobal("crypto", {});
    mockGet.mockResolvedValue({ data: { workspaces: [] } });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });

    await waitFor(() => expect(mockGet).toHaveBeenCalledWith("/snapshot", expect.anything()));
    expect(screen.getByText("Workspaces")).toBeTruthy();
  });

  it("labels workspace rows with their execution machine on the hub", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "Studio hub",
                federationRole: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "79e90262-7426-4dd5-9ef1-0d511af84e12",
                diagnostics: [],
                id: "79e90262-7426-4dd5-9ef1-0d511af84e12",
                kind: "remote",
                name: "Build spoke",
                federationRole: "spoke",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "local-ws",
                provider: "github",
                platformHost: "github.com",
                owner: "acme",
                name: "service",
                number: 11,
                title: "Local workspace",
              }),
              {
                ...workspaceFixture({
                  id: "remote-ws",
                  provider: "github",
                  platformHost: "github.com",
                  owner: "acme",
                  name: "service",
                  number: 12,
                  title: "Remote workspace",
                }),
                fleet_host_key: "79e90262-7426-4dd5-9ef1-0d511af84e12",
              },
            ],
          },
        });
      }
      return Promise.resolve({ data: { workspaces: [] } });
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    await screen.findByText("Remote workspace");
    expect(screen.getByTitle("Runs on Studio hub")).toBeTruthy();
    expect(screen.getByText("Build spoke")).toBeTruthy();
    expect(screen.getByTitle("Runs on Build spoke")).toBeTruthy();
    expect(screen.queryByLabelText("Fleet hosts")).toBeNull();
    expect(screen.queryByText("79e90262-7426-4dd5-9ef1-0d511af84e12")).toBeNull();
  });

  it("coalesces workspace completion bursts into one list refresh", async () => {
    mockGet.mockResolvedValue({ data: { workspaces: [] } });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    await waitFor(() => expect(mockGet).toHaveBeenCalledWith("/snapshot", expect.anything()));
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));
    mockGet.mockClear();

    emitWorkspaceStatus();
    emitWorkspaceStatus();
    emitWorkspaceStatus();
    const workspaceRequestCount = () => mockGet.mock.calls.filter(([path]) => path === "/snapshot").length;

    await new Promise((resolve) => setTimeout(resolve, 15));
    expect(workspaceRequestCount()).toBe(0);
    await waitFor(() => expect(workspaceRequestCount()).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 75));
    expect(workspaceRequestCount()).toBe(1);
  });

  it("hides the fleet status block when only the local host is present", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "self",
                name: "member",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [],
          },
        });
      }
      return Promise.resolve({ data: { workspaces: [] } });
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith(
        "/snapshot",
        expect.objectContaining({
          params: { query: { include_peers: true } },
        }),
      );
    });
    expect(screen.queryByText("Fleet")).toBeNull();
    expect(screen.queryByText("1/1")).toBeNull();
    expect(screen.queryByText("member")).toBeNull();
    expect(screen.queryByText("self")).toBeNull();
    expect(screen.queryByText("local")).toBeNull();
  });

  it("reports when no workspaces exist", async () => {
    const onWorkspaceListStateChange = vi.fn();
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "self",
                name: "member",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [],
          },
        });
      }
      return Promise.resolve({ data: { workspaces: [] } });
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "", onWorkspaceListStateChange },
    });

    expect(await screen.findByText("No workspaces yet.")).toBeTruthy();
    await waitFor(() => {
      expect(onWorkspaceListStateChange).toHaveBeenLastCalledWith({
        status: "loaded",
        total: 0,
      });
    });
  });

  it("applies repository scope before the workspace text filter", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "api-workspace",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "api",
            number: 1,
            title: "API cleanup",
          }),
          workspaceFixture({
            id: "web-workspace",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "web",
            number: 2,
            title: "Web cleanup",
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: {
        selectedId: "",
        selectedRepos: "github|github.com/acme/api",
      },
    });

    expect(await screen.findByText("API cleanup")).toBeTruthy();
    expect(screen.queryByText("Web cleanup")).toBeNull();
    await fireEvent.input(screen.getByRole("searchbox", { name: "Filter workspaces" }), {
      target: { value: "web" },
    });
    expect(screen.queryByText("API cleanup")).toBeNull();
    expect(screen.queryByText("Web cleanup")).toBeNull();
  });

  it("applies repository scope to local and fleet workspace rows", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "remote",
                name: "member",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "local-api",
                provider: "github",
                platformHost: "github.com",
                owner: "acme",
                name: "api",
                number: 1,
                title: "Local API",
              }),
              workspaceFixture({
                id: "local-web",
                provider: "github",
                platformHost: "github.com",
                owner: "acme",
                name: "web",
                number: 2,
                title: "Local Web",
              }),
              {
                ...workspaceFixture({
                  id: "remote-api",
                  provider: "gitlab",
                  platformHost: "gitlab.example.com",
                  owner: "platform",
                  name: "api",
                  number: 3,
                  title: "Remote API",
                }),
                fleet_host_key: "member",
                fleet_host_name: "member",
              },
              {
                ...workspaceFixture({
                  id: "remote-web",
                  provider: "gitlab",
                  platformHost: "gitlab.example.com",
                  owner: "platform",
                  name: "web",
                  number: 4,
                  title: "Remote Web",
                }),
                fleet_host_key: "member",
                fleet_host_name: "member",
              },
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, {
      props: {
        selectedId: "",
        selectedRepos: "github|github.com/acme/api,gitlab|gitlab.example.com/platform/api",
      },
    });

    expect(await screen.findByText("Local API")).toBeTruthy();
    expect(await screen.findByText("Remote API")).toBeTruthy();
    expect(screen.queryByText("Local Web")).toBeNull();
    expect(screen.queryByText("Remote Web")).toBeNull();
    expect(mockGet).not.toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces", expect.anything());
  });

  it("keeps the repository catalog incomplete until the projected snapshot loads", async () => {
    const snapshot = deferred<{
      data: { hosts: Array<Record<string, unknown>>; workspaces: unknown[] };
    }>();
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") return snapshot.promise;
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(isWorkspaceRepoCatalogReady()).toBe(false);

    snapshot.resolve({
      data: {
        hosts: [],
        workspaces: [
          workspaceFixture({
            id: "local-ws",
            provider: "github",
            platformHost: "github.com",
            owner: "local",
            name: "service",
            number: 1,
          }),
          {
            ...workspaceFixture({
              id: "remote-ws",
              provider: "github",
              platformHost: "github.com",
              owner: "remote",
              name: "service",
              number: 2,
            }),
            fleet_host_key: "member",
            fleet_host_name: "member",
          },
        ],
      },
    });

    await waitFor(() => expect(isWorkspaceRepoCatalogReady()).toBe(true));
    expect(getWorkspaceRepoCatalog().map((repo) => repo.repo_path)).toEqual(["local/service", "remote/service"]);
  });

  it("loads projected workspaces from reachable federation members without fan-out", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "epyc",
                diagnostics: [],
                id: "epyc",
                kind: "remote",
                name: "epyc",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              {
                ...workspaceFixture({
                  id: "remote-ws",
                  provider: "github",
                  platformHost: "github.com",
                  owner: "remote",
                  name: "service",
                  number: 12,
                  title: "Remote workspace",
                }),
                fleet_host_key: "epyc",
                fleet_host_name: "epyc",
              },
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    expect(await screen.findByText("Remote workspace")).toBeTruthy();
    expect(mockGet).not.toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces", expect.anything());
  });

  it("refreshes inline workspace summaries after a workspace event", async () => {
    let snapshotLoads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotLoads += 1;
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "local-ws",
                provider: "github",
                platformHost: "github.com",
                owner: "local",
                name: "service",
                number: 1,
                title: snapshotLoads === 1 ? "Initial local workspace" : "Updated local workspace",
              }),
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Initial local workspace")).toBeTruthy();
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));

    emitWorkspaceStatus();

    expect(await screen.findByText("Updated local workspace")).toBeTruthy();
    expect(snapshotLoads).toBe(2);
  });

  it("keeps the last projected rows after an invalid snapshot refresh", async () => {
    let snapshotLoads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotLoads += 1;
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "remote",
                name: "member",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces:
              snapshotLoads === 1
                ? [
                    {
                      ...workspaceFixture({
                        id: "remote-ws",
                        provider: "github",
                        platformHost: "github.com",
                        owner: "remote",
                        name: "service",
                        number: 12,
                        title: "Last known remote workspace",
                      }),
                      fleet_host_key: "member",
                      fleet_host_name: "member",
                    },
                  ]
                : [{ id: "malformed" }],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Last known remote workspace")).toBeTruthy();
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));

    emitWorkspaceStatus();

    await waitFor(() => expect(snapshotLoads).toBe(2));
    expect(screen.getByText("Last known remote workspace")).toBeTruthy();
    expect(isWorkspaceRepoCatalogReady()).toBe(false);
  });

  it("keeps member workspaces when a spoke loses its hub aggregate", async () => {
    let snapshotLoads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotLoads += 1;
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "spoke-a",
                diagnostics: [],
                id: "spoke-a",
                kind: "self",
                name: "spoke-a",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "hub",
                diagnostics: [],
                error: snapshotLoads === 1 ? undefined : "Hub aggregate unavailable",
                id: "hub",
                kind: "remote",
                name: "hub",
                operationAvailability: {},
                platform: "linux",
                preferredTransport: "http",
                reachable: snapshotLoads === 1,
                tmuxSessions: [],
              },
              ...(snapshotLoads === 1
                ? [
                    {
                      configKey: "member",
                      diagnostics: [],
                      id: "member",
                      kind: "remote",
                      name: "member",
                      operationAvailability: {},
                      platform: "linux",
                      preferredTransport: "http",
                      reachable: true,
                      tmuxSessions: [],
                    },
                  ]
                : []),
            ],
            aggregateIncomplete: snapshotLoads !== 1,
            workspaces:
              snapshotLoads === 1
                ? [
                    {
                      ...workspaceFixture({
                        id: "remote-ws",
                        provider: "github",
                        platformHost: "github.com",
                        owner: "remote",
                        name: "service",
                        number: 12,
                        title: "Degraded remote workspace",
                      }),
                      fleet_host_key: "member",
                      fleet_host_name: "member",
                    },
                  ]
                : [],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Degraded remote workspace")).toBeTruthy();
    expect(isWorkspaceRepoCatalogReady()).toBe(true);
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));

    emitWorkspaceStatus();

    await waitFor(() => expect(snapshotLoads).toBe(2));
    expect(screen.getByText("Degraded remote workspace")).toBeTruthy();
    expect(screen.getByText(/Hub aggregate unavailable/)).toBeTruthy();
    expect(isWorkspaceRepoCatalogReady()).toBe(false);
  });

  it("removes a revoked member while an enrolled member is degraded", async () => {
    let snapshotLoads = 0;
    mockGet.mockImplementation((path: string) => {
      if (path !== "/snapshot") return Promise.resolve({ data: {} });
      snapshotLoads += 1;
      return Promise.resolve({
        data: {
          aggregateIncomplete: false,
          hosts: [
            {
              configKey: "hub",
              diagnostics: [],
              id: "hub",
              kind: "self",
              name: "hub",
              operationAvailability: {},
              platform: "linux",
              preferredTransport: "local",
              reachable: true,
              tmuxSessions: [],
            },
            ...(snapshotLoads === 1
              ? [
                  {
                    configKey: "revoked",
                    diagnostics: [],
                    id: "revoked",
                    kind: "remote",
                    name: "revoked",
                    operationAvailability: {},
                    platform: "linux",
                    preferredTransport: "http",
                    reachable: true,
                    tmuxSessions: [],
                  },
                ]
              : [
                  {
                    configKey: "offline",
                    diagnostics: [],
                    error: "timeout",
                    id: "offline",
                    kind: "remote",
                    name: "offline",
                    operationAvailability: {},
                    platform: "linux",
                    preferredTransport: "http",
                    reachable: false,
                    tmuxSessions: [],
                  },
                ]),
          ],
          workspaces:
            snapshotLoads === 1
              ? [
                  {
                    ...workspaceFixture({
                      id: "revoked-ws",
                      provider: "github",
                      platformHost: "github.com",
                      owner: "old",
                      name: "service",
                      number: 12,
                      title: "Revoked workspace",
                    }),
                    fleet_host_key: "revoked",
                    fleet_host_name: "revoked",
                  },
                ]
              : [],
        },
      });
    });

    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Revoked workspace")).toBeTruthy();
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));

    emitWorkspaceStatus();

    await waitFor(() => expect(snapshotLoads).toBe(2));
    expect(screen.queryByText("Revoked workspace")).toBeNull();
  });

  it("removes remote workspaces when the fleet snapshot becomes local-only", async () => {
    let snapshotCalls = 0;

    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        snapshotCalls += 1;
        return Promise.resolve({
          data: {
            hosts:
              snapshotCalls === 1
                ? [
                    {
                      configKey: "hub",
                      diagnostics: [],
                      id: "hub",
                      kind: "self",
                      name: "hub",
                      operationAvailability: {},
                      platform: "darwin",
                      preferredTransport: "local",
                      reachable: true,
                      tmuxSessions: [],
                    },
                    {
                      configKey: "epyc",
                      diagnostics: [],
                      id: "epyc",
                      kind: "remote",
                      name: "epyc",
                      operationAvailability: {},
                      platform: "linux",
                      preferredTransport: "http",
                      reachable: true,
                      tmuxSessions: [],
                    },
                  ]
                : [
                    {
                      configKey: "hub",
                      diagnostics: [],
                      id: "hub",
                      kind: "self",
                      name: "hub",
                      operationAvailability: {},
                      platform: "darwin",
                      preferredTransport: "local",
                      reachable: true,
                      tmuxSessions: [],
                    },
                  ],
            workspaces: [
              workspaceFixture({
                id: "local-ws",
                provider: "github",
                platformHost: "github.com",
                owner: "local",
                name: "service",
                number: 1,
                title: "Local workspace",
              }),
              ...(snapshotCalls === 1
                ? [
                    {
                      ...workspaceFixture({
                        id: "remote-ws",
                        provider: "github",
                        platformHost: "github.com",
                        owner: "remote",
                        name: "service",
                        number: 12,
                        title: "Remote workspace",
                      }),
                      fleet_host_key: "epyc",
                      fleet_host_name: "epyc",
                    },
                  ]
                : []),
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    expect(await screen.findByText("Remote workspace")).toBeTruthy();
    await waitFor(() => expect(workspaceEventsSubscriber).toBeTypeOf("function"));

    emitWorkspaceStatus();

    await waitFor(() => expect(screen.queryByText("Remote workspace")).toBeNull());
    expect(screen.getByText("Local workspace")).toBeTruthy();
  });

  it("shows provider icons in repo groups when multiple providers are present", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-github",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 42,
          }),
          workspaceFixture({
            id: "ws-gitlab",
            provider: "gitlab",
            platformHost: "gitlab.com",
            owner: "platform",
            name: "api",
            number: 7,
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "ws-github" },
    });

    await screen.findByText("acme/widgets");
    expect(screen.getByRole("img", { name: "GitHub" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "GitLab" })).toBeTruthy();
  });

  it("does not render a blank rail while the workspace list is loading", async () => {
    mockGet.mockReturnValue(new Promise(() => {}));

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    expect(screen.getByText("Loading workspaces...")).toBeTruthy();
  });

  it("shows a retrying state when the initial workspace list hangs", async () => {
    vi.useFakeTimers();
    let aborted = false;
    mockGet.mockImplementation(
      (_path: string, opts?: { signal?: AbortSignal }) =>
        new Promise((_resolve, reject) => {
          opts?.signal?.addEventListener("abort", () => {
            aborted = true;
            reject(new DOMException("Aborted", "AbortError"));
          });
        }),
    );

    render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    expect(screen.getByText("Loading workspaces...")).toBeTruthy();
    await vi.advanceTimersByTimeAsync(10_000);
    await tick();

    expect(aborted).toBe(true);
    expect(screen.getByText("Still loading workspaces. Retrying...")).toBeTruthy();
  });

  it("hides provider icons in repo groups when one provider is present", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-github",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 42,
          }),
          workspaceFixture({
            id: "ws-ghe",
            provider: "github",
            platformHost: "ghe.example.com",
            owner: "enterprise",
            name: "service",
            number: 9,
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "ws-github" },
    });

    await screen.findByText("acme/widgets");
    expect(screen.queryByRole("img", { name: "GitHub" })).toBeNull();
  });

  it("keeps same-host repos from different providers in separate groups", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-gitea",
            provider: "gitea",
            platformHost: "code.example.com",
            owner: "acme",
            name: "widgets",
            number: 1,
            title: "Gitea widgets",
          }),
          workspaceFixture({
            id: "ws-forgejo",
            provider: "forgejo",
            platformHost: "code.example.com",
            owner: "acme",
            name: "widgets",
            number: 2,
            title: "Forgejo widgets",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-gitea" },
    });
    await screen.findByText("Gitea widgets");

    // Identical host and repo path, different providers: the rows must
    // not collapse into a single group whose label and icon come from
    // only the first item. Each provider keeps its own group + icon.
    expect(container.querySelectorAll(".sidebar-group-header")).toHaveLength(2);
    expect(screen.getByRole("img", { name: "Gitea" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "Forgejo" })).toBeTruthy();
  });

  it("filters workspaces by title, repo, and item number", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-title",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "taskboard",
            number: 9,
            title: "Migrate native HTTP surface to Huma v2",
            branch: "feat/huma-adoption",
          }),
          workspaceFixture({
            id: "ws-repo",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "project-platform",
            number: 2,
            title: "Hosted code fetch and caching strategy",
          }),
          workspaceFixture({
            id: "ws-number",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 224,
            title: "Add notification inbox triage",
            itemType: "issue",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-title" },
    });
    const filter = await screen.findByLabelText("Filter workspaces");
    await screen.findByText("Migrate native HTTP surface to Huma v2");

    await fireEvent.input(filter, {
      target: { value: "huma" },
    });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
    expect(screen.getByText("Migrate native HTTP surface to Huma v2")).toBeTruthy();

    await fireEvent.input(filter, {
      target: { value: "project-platform" },
    });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
    expect(screen.getByText("Hosted code fetch and caching strategy")).toBeTruthy();

    await fireEvent.input(filter, {
      target: { value: "#224" },
    });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
    expect(screen.getByText("Add notification inbox triage")).toBeTruthy();
  });

  it("shows matching workspaces in collapsed groups while filtering", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-hidden",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 224,
            title: "Add notification inbox triage",
            itemType: "issue",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-hidden" },
    });
    const groupHeader = await screen.findByRole("button", {
      name: /kenn-io\/kenn-forge/,
    });
    const filter = screen.getByLabelText("Filter workspaces");

    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
    await fireEvent.click(groupHeader);
    expect(container.querySelectorAll(".ws-row")).toHaveLength(0);

    await fireEvent.input(filter, {
      target: { value: "#224" },
    });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
    expect(screen.getByText("Add notification inbox triage")).toBeTruthy();

    await fireEvent.input(filter, {
      target: { value: "" },
    });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(0);
  });

  it("shows a full-detail popover on row focus only while the name is truncated", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-popover",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 41,
            title: "A workspace title too long for the sidebar rail",
            branch: "feature/full-sidebar-popover-details",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });
    await waitFor(() => expect(container.querySelectorAll(".ws-row")).toHaveLength(1));
    const row = container.querySelector<HTMLElement>(".ws-row")!;
    const name = row.querySelector(".ws-name")!;

    // jsdom reports zero layout widths, so the test chooses whether the name
    // is ellipsis-truncated by stubbing the measurements the popover reads.
    Object.defineProperty(name, "scrollWidth", { configurable: true, value: 180 });
    Object.defineProperty(name, "clientWidth", { configurable: true, value: 180 });
    await fireEvent.focusIn(row);
    expect(screen.queryByRole("tooltip")).toBeNull();
    await fireEvent.focusOut(row);

    Object.defineProperty(name, "scrollWidth", { configurable: true, value: 360 });
    await fireEvent.focusIn(row);
    const tooltip = screen.getByRole("tooltip");
    expect(Array.from(tooltip.children, (line) => line.textContent)).toEqual([
      "A workspace title too long for the sidebar rail",
      "kenn-io/kenn-forge",
      "feature/full-sidebar-popover-details",
    ]);
    expect(row.contains(tooltip)).toBe(false);

    await fireEvent.focusOut(row);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("sorts flat by creation time and drops group headers", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: sortFixtures() },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("Newest created");

    // Default org/repo mode groups rows under repo headers.
    expect(screen.getByText("kenn-io/kenn-forge")).toBeTruthy();
    expect(container.querySelectorAll(".repo-context")).toHaveLength(0);

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Created" }));

    expect(rowTitles(container)).toEqual(["Newest created", "Most recently active", "Oldest without activity"]);
    expect(container.querySelectorAll(".sidebar-group-header")).toHaveLength(0);
    // Flat rows carry their own repo context instead of a header.
    expect(container.querySelectorAll(".repo-context")).toHaveLength(3);
  });

  it("keeps provider and host identity visible in flat rows", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-github",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 1,
            title: "GitHub workspace",
            createdAt: "2026-05-12T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-gitlab",
            provider: "gitlab",
            platformHost: "gitlab.example.com",
            owner: "acme",
            name: "widgets",
            number: 2,
            title: "GitLab workspace",
            createdAt: "2026-05-11T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-other",
            provider: "gitlab",
            platformHost: "gitlab.example.com",
            owner: "platform",
            name: "api",
            number: 3,
            title: "Unambiguous workspace",
            createdAt: "2026-05-10T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-github" },
    });
    await screen.findByText("GitHub workspace");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Created" }));

    // Provider icons survive the loss of group headers.
    expect(container.querySelectorAll(".repo-context")).toHaveLength(3);
    expect(screen.getByRole("img", { name: "GitHub" })).toBeTruthy();
    expect(screen.getAllByRole("img", { name: "GitLab" })).toHaveLength(2);

    // acme/widgets exists on two hosts, so its rows carry the host;
    // platform/api is unique and stays short.
    const contextNames = container.querySelectorAll(".repo-context-name");
    expect(contextNames[0]?.textContent?.trim()).toBe("github.com/acme/widgets");
    expect(contextNames[1]?.textContent?.trim()).toBe("gitlab.example.com/acme/widgets");
    expect(contextNames[2]?.textContent?.trim()).toBe("platform/api");
  });

  it("sorts flat by last activity with creation time as fallback", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: sortFixtures() },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("Newest created");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));

    // ws-old has no tmux output, so it sorts by its creation time.
    expect(rowTitles(container)).toEqual(["Most recently active", "Newest created", "Oldest without activity"]);
  });

  it("sorts flat by item activity with creation time as fallback", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-created-newest",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 1,
            title: "Newest created fallback",
            createdAt: "2026-05-15T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-pr-active",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 2,
            title: "PR recently changed",
            createdAt: "2026-05-10T12:00:00Z",
            itemLastActivityAt: "2026-05-16T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-issue-active",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "agentsview",
            number: 3,
            title: "Issue recently changed",
            itemType: "issue",
            createdAt: "2026-05-09T12:00:00Z",
            itemLastActivityAt: "2026-05-17T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-created-newest" },
    });
    await screen.findByText("Newest created fallback");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    const itemActivitySort = screen.getByRole("button", { name: "Item activity" });
    expect(itemActivitySort.getAttribute("title")).toBe(
      "Sort by latest linked PR or issue activity, falling back to workspace creation.",
    );
    await fireEvent.click(itemActivitySort);

    expect(rowTitles(container)).toEqual(["Issue recently changed", "PR recently changed", "Newest created fallback"]);
    expect(container.querySelectorAll(".sidebar-group-header")).toHaveLength(0);
  });

  it("sorts flat by hook-reported agent status", async () => {
    const states: WorkspaceFixtureOptions["agentState"][] = ["idle", "done", "working", "input", "approval", null];
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          ...states.map((agentState, index) =>
            workspaceFixture({
              id: agentState ?? "unreported",
              provider: "github",
              platformHost: "github.com",
              owner: "kenn-io",
              name: "kenn-forge",
              number: index + 1,
              title: agentState ?? "unreported",
              agentState,
              agentStateUpdatedAt: agentState === "done" ? "2026-05-12T12:00:00Z" : null,
            }),
          ),
          workspaceFixture({
            id: "done-newer-hook",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 99,
            title: "done-newer-hook",
            agentState: "done",
            createdAt: "2026-05-01T12:00:00Z",
            agentStateUpdatedAt: "2026-05-13T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "approval" },
    });
    await screen.findByText("approval");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    const agentStatusSort = screen.getByRole("button", { name: "Agent status" });
    expect(agentStatusSort.getAttribute("title")).toBe(
      "Group by agent status, with workspaces needing attention first.",
    );
    await fireEvent.click(agentStatusSort);

    expect(rowTitles(container)).toEqual([
      "approval",
      "input",
      "working",
      "done-newer-hook",
      "done",
      "idle",
      "unreported",
    ]);
    expect(container.querySelectorAll(".sidebar-group-header")).toHaveLength(0);
    expect(container.textContent).toContain("Idle");
    expect(Array.from(container.querySelectorAll(".agent-state"), (state) => state.textContent)).not.toContain(
      "Unreported",
    );
    const unreportedRow = screen.getByText("unreported").closest<HTMLElement>(".ws-row");
    expect(unreportedRow?.querySelector(".workspace-sort-time")?.getAttribute("datetime")).toBe("2026-05-12T12:00:00Z");

    const doneRow = screen.getByText("done-newer-hook").closest<HTMLElement>(".ws-row");
    expect(doneRow).toBeTruthy();
    expect(doneRow?.querySelector(".workspace-sort-time")?.getAttribute("datetime")).toBe("2026-05-13T12:00:00Z");
    await fireEvent.click(doneRow!);

    expect(mockNavigate).toHaveBeenCalledWith("/terminal/done-newer-hook");
    expect(doneRow?.querySelector(".agent-state")?.textContent).toContain("Done");
    expect(doneRow?.querySelector(".workspace-sort-time")?.getAttribute("datetime")).toBe("2026-05-13T12:00:00Z");
    expect(rowTitles(container)).toEqual([
      "approval",
      "input",
      "working",
      "done-newer-hook",
      "done",
      "idle",
      "unreported",
    ]);

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Created" }));
    expect(doneRow?.querySelector(".agent-state")?.textContent).toContain("Done");
  });

  it("uses item activity as the agent-status fallback timestamp", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "newer-created",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 1,
            title: "Newer creation",
            createdAt: "2026-05-12T12:00:00Z",
            itemLastActivityAt: "2026-05-13T12:00:00Z",
          }),
          workspaceFixture({
            id: "newer-item-activity",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 2,
            title: "Newer item activity",
            createdAt: "2026-05-11T12:00:00Z",
            itemLastActivityAt: "2026-05-14T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "newer-created" },
    });
    await screen.findByText("Newer creation");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Agent status" }));

    expect(rowTitles(container)).toEqual(["Newer item activity", "Newer creation"]);
    expect(
      screen
        .getByText("Newer item activity")
        .closest<HTMLElement>(".ws-row")
        ?.querySelector(".workspace-sort-time")
        ?.getAttribute("datetime"),
    ).toBe("2026-05-14T12:00:00Z");
  });

  it("shows the timestamp used by each flat sort below the linked item", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-times",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 7,
            title: "Timestamped workspace",
            createdAt: "2026-05-10T12:00:00Z",
            tmuxLastOutputAt: "2026-05-11T12:00:00Z",
            itemLastActivityAt: "2026-05-12T12:00:00Z",
            agentState: "working",
            agentStateUpdatedAt: "2026-05-13T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-times" },
    });
    await screen.findByText("Timestamped workspace");
    expect(container.querySelector(".workspace-sort-time")).toBeNull();

    for (const [sort, timestamp] of [
      ["Created", "2026-05-10T12:00:00Z"],
      ["Activity", "2026-05-11T12:00:00Z"],
      ["Item activity", "2026-05-12T12:00:00Z"],
      ["Agent status", "2026-05-13T12:00:00Z"],
    ]) {
      await fireEvent.click(screen.getByTitle("View workspace options"));
      await fireEvent.click(screen.getByRole("button", { name: sort }));
      expect(
        container.querySelector(".ws-row-aside > .item-bubble + .workspace-sort-time")?.getAttribute("datetime"),
      ).toBe(timestamp);
    }
  });

  it("persists the selected sort across mounts", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: sortFixtures() },
    });

    const first = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("Newest created");

    await fireEvent.click(screen.getByTitle("View workspace options"));
    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));
    first.unmount();

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("Newest created");

    expect(rowTitles(container)).toEqual(["Most recently active", "Newest created", "Oldest without activity"]);
    expect(container.querySelectorAll(".sidebar-group-header")).toHaveLength(0);
  });

  it("folds sort choices into the workspace view menu", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: sortFixtures() },
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("Newest created");

    await fireEvent.click(screen.getByRole("button", { name: "View" }));

    expect(screen.getByText("Sorting")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Org / repo" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Created" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Activity" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Agent status" })).toBeTruthy();
    expect(screen.getByText("Visibility")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Hide org name" }).classList.contains("active")).toBe(false);
    expect(screen.getByRole("button", { name: "Show PR diff stats" })).toBeTruthy();
  });

  it("can hide org names in grouped and flat workspace labels", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: sortFixtures() },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-new" },
    });
    await screen.findByText("kenn-io/kenn-forge");

    await fireEvent.click(screen.getByRole("button", { name: "View" }));
    const hideOrgName = screen.getByRole("button", { name: "Hide org name" });
    await fireEvent.click(hideOrgName);
    expect(hideOrgName.classList.contains("active")).toBe(true);

    expect(screen.queryByText("kenn-io/kenn-forge")).toBeNull();
    expect(screen.getByText("kenn-forge")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Created" }));

    expect(container.querySelectorAll(".repo-context-name")[0]?.textContent?.trim()).toBe("kenn-forge");
    expect(container.querySelectorAll(".repo-context-name")[1]?.textContent?.trim()).toBe("agentsview");
  });

  it("keeps hidden-org workspace repo labels distinguishable", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-github-acme",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 1,
            title: "GitHub acme widgets",
            createdAt: "2026-05-12T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-ghe-acme",
            provider: "github",
            platformHost: "ghe.example.com",
            owner: "acme",
            name: "widgets",
            number: 2,
            title: "GHE acme widgets",
            createdAt: "2026-05-11T12:00:00Z",
          }),
          workspaceFixture({
            id: "ws-platform",
            provider: "gitlab",
            platformHost: "gitlab.example.com",
            owner: "platform",
            name: "widgets",
            number: 3,
            title: "Platform widgets",
            createdAt: "2026-05-10T12:00:00Z",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-github-acme" },
    });
    await screen.findByText("GitHub acme widgets");

    await fireEvent.click(screen.getByRole("button", { name: "View" }));
    await fireEvent.click(screen.getByRole("button", { name: "Hide org name" }));

    expect(
      Array.from(container.querySelectorAll(".sidebar-group-header__name")).map((el) => el.textContent?.trim()),
    ).toEqual(["github.com/acme/widgets", "ghe.example.com/acme/widgets", "platform/widgets"]);

    await fireEvent.click(screen.getByRole("button", { name: "Created" }));

    expect(Array.from(container.querySelectorAll(".repo-context-name")).map((el) => el.textContent?.trim())).toEqual([
      "github.com/acme/widgets",
      "ghe.example.com/acme/widgets",
      "platform/widgets",
    ]);
  });

  it("keeps same-host different-provider repo groups separate", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-github-acme",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 1,
            title: "GitHub acme widgets",
          }),
          workspaceFixture({
            id: "ws-gitea-acme",
            provider: "gitea",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 2,
            title: "Gitea acme widgets",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-github-acme" },
    });
    await screen.findByText("GitHub acme widgets");

    expect(
      Array.from(container.querySelectorAll(".sidebar-group-header__name")).map((el) => el.textContent?.trim()),
    ).toEqual(["github/github.com/acme/widgets", "gitea/github.com/acme/widgets"]);
    expect(
      Array.from(container.querySelectorAll(".sidebar-group-header__count")).map((el) => el.textContent?.trim()),
    ).toEqual(["1", "1"]);
  });

  it("can hide PR diff stats while keeping branch metadata visible", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-diff",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Diff-heavy workspace",
            additions: 42,
            deletions: 7,
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-diff" },
    });
    await screen.findByText("Diff-heavy workspace");

    expect(container.querySelector(".workspace-diff-stats")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "View" }));
    await fireEvent.click(screen.getByRole("button", { name: "Show PR diff stats" }));

    expect(container.querySelector(".workspace-diff-stats")).toBeNull();
    expect(container.querySelector(".branch-chip")).toBeTruthy();
  });

  it("marks dirty worktrees without marking clean rows", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-dirty",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Dirty workspace",
            worktreeDirty: true,
          }),
          workspaceFixture({
            id: "ws-clean",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 10,
            title: "Clean workspace",
            worktreeDirty: false,
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-dirty" },
    });
    await screen.findByText("Dirty workspace");

    expect(screen.queryByText("Dirty")).toBeNull();
    expect(screen.getByLabelText("Dirty worktree")).toBeTruthy();
    expect(container.querySelector('[title="Dirty worktree"]')).toBeTruthy();
    expect(container.querySelectorAll(".worktree-dirty")).toHaveLength(1);
    expect(container.querySelector(".worktree-dirty svg.lucide-pencil")).toBeTruthy();
    expect(container.querySelector(".ws-row-aside > .item-bubble + .worktree-dirty")).toBeTruthy();
    expect(container.querySelectorAll(".worktree-dirty-slot")).toHaveLength(0);
  });

  it("opens a host-aware context menu for local macOS workspaces", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "ws-local",
                provider: "github",
                platformHost: "github.com",
                owner: "kenn-io",
                name: "kenn-forge",
                number: 555,
                title: "Local mac workspace",
              }),
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-local" },
    });
    await screen.findByText("Local mac workspace");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    expect(screen.getByRole("menu", { name: "Workspace actions" })).toBeTruthy();
    expect(screen.queryByText("Copy")).toBeNull();
    expect(screen.queryByText("Provider")).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Copy worktree path" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Reveal in Finder" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Refresh git status" })).toBeTruthy();
  });

  function kataWorkspaceFixture(overrides: Partial<WorkspaceFixtureOptions> = {}) {
    return workspaceFixture({
      id: "ws-kata",
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "kenn-forge",
      number: 0,
      branch: "kenn-forge/kata/task-123-abcd1234",
      itemType: "kata_task",
      itemKey: "kata:ZGVza3RvcA:cHJvamVjdC1rYXRh:aXNzdWUta2F0YS0x",
      kata: {
        daemon_id: "desktop",
        project_uid: "project-kata",
        project_name: "Kenn Forge",
        issue_uid: "issue-kata-1",
        short_id: "task-123",
        qualified_id: "Kata#task-123",
        title: "Wire kata workspace sidebar",
      },
      ...overrides,
    });
  }

  it("renders Kata task identity and opens the Kata links sidebar tab", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: [kataWorkspaceFixture()] },
    });
    const onOpenItemSidebar = vi.fn();

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-kata", onOpenItemSidebar },
    });
    await screen.findByText("Wire kata workspace sidebar");

    const bubble = container.querySelector(".item-bubble");
    expect(bubble).not.toBeNull();
    expect(bubble!.classList.contains("kata")).toBe(true);
    expect(bubble!.textContent?.trim()).toBe("task-123");
    // A Kata task has no provider item number, so the row must not show #0.
    expect(container.textContent).not.toContain("#0");

    await fireEvent.click(bubble!);
    expect(onOpenItemSidebar).toHaveBeenCalledWith("ws-kata", "kata", undefined);
  });

  it("gives a draft pull request bubble the draft state class, not open", async () => {
    // A draft PR must read as draft in the sidebar bubble (amber draft
    // styling) instead of falling through to the open/green treatment, so
    // the chip reflects the same draft status shown in the PR detail view.
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-draft",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 241,
            title: "Plan ACP agent chat integration",
            isDraft: true,
          }),
          workspaceFixture({
            id: "ws-open",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 242,
            title: "Ready for review PR",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-draft" },
    });
    await screen.findByText("Plan ACP agent chat integration");

    const bubbles = Array.from(container.querySelectorAll(".item-bubble"));
    const draftBubble = bubbles.find((b) => b.textContent?.trim() === "#241");
    const openBubble = bubbles.find((b) => b.textContent?.trim() === "#242");

    expect(draftBubble?.classList.contains("draft")).toBe(true);
    expect(draftBubble?.classList.contains("open")).toBe(false);
    expect(openBubble?.classList.contains("open")).toBe(true);
    expect(openBubble?.classList.contains("draft")).toBe(false);
  });

  it("gives an issue-backed workspace bubble the issue state class, not open", async () => {
    // An open issue-backed workspace must not reuse the open/green PR
    // treatment; the blue issue styling tells it apart from PR-backed
    // rows at a glance.
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-issue",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 968,
            title: "Keep local base branches in sync",
            itemType: "issue",
          }),
          workspaceFixture({
            id: "ws-pr",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 242,
            title: "Ready for review PR",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-issue" },
    });
    await screen.findByText("Keep local base branches in sync");

    const bubbles = Array.from(container.querySelectorAll(".item-bubble"));
    const issueBubble = bubbles.find((b) => b.textContent?.trim() === "#968");
    const prBubble = bubbles.find((b) => b.textContent?.trim() === "#242");

    expect(issueBubble?.classList.contains("issue")).toBe(true);
    expect(issueBubble?.classList.contains("open")).toBe(false);
    expect(prBubble?.classList.contains("open")).toBe(true);
    expect(prBubble?.classList.contains("issue")).toBe(false);
  });

  it("omits provider item actions in the Kata workspace context menu", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: [kataWorkspaceFixture()] },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-kata" },
    });
    await screen.findByText("Wire kata workspace sidebar");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    expect(screen.getByRole("menu", { name: "Workspace actions" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: /Open item on/ })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Copy item URL" })).toBeNull();
  });

  it("filters a Kata workspace by its task identity", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: [kataWorkspaceFixture()] },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-kata" },
    });
    const filter = await screen.findByLabelText("Filter workspaces");
    await screen.findByText("Wire kata workspace sidebar");

    await fireEvent.input(filter, { target: { value: "task-123" } });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);

    await fireEvent.input(filter, { target: { value: "no-such-task" } });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(0);
  });

  it("finds a Kata workspace by its task UID when it has no short ID", async () => {
    // A Kata task without a short/qualified ID renders the generic "Kata"
    // bubble, so it must stay findable by its durable identifiers.
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          kataWorkspaceFixture({
            kata: {
              daemon_id: "desktop",
              project_uid: "project-kata",
              project_name: "Kenn Forge",
              issue_uid: "issue-kata-1",
            },
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-kata" },
    });
    const filter = await screen.findByLabelText("Filter workspaces");
    await waitFor(() => expect(container.querySelector(".item-bubble")).not.toBeNull());

    const bubble = container.querySelector(".item-bubble");
    expect(bubble!.textContent?.trim()).toBe("Kata");

    await fireEvent.input(filter, { target: { value: "issue-kata-1" } });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);

    await fireEvent.input(filter, { target: { value: "project-kata" } });
    expect(container.querySelectorAll(".ws-row")).toHaveLength(1);
  });

  it.each([
    ["ready", "Workspace ready", "kit-status-dot--idle"],
    ["creating", "Creating workspace", "kit-status-dot--working"],
    ["error", "Workspace error", "kit-status-dot--unclean"],
    ["deleting", "Deleting workspace", "kit-status-dot--working"],
    ["deletion_failed", "Deletion failed", "kit-status-dot--unclean"],
    ["pending", "Workspace pending", "kit-status-dot--stale"],
  ] as const)("maps %s workspace state to the animated semantic status", async (status, label, className) => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: `ws-${status}`,
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            status,
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: `ws-${status}` },
    });

    const statusDot = await screen.findByLabelText(label);
    expect(statusDot.classList.contains(className)).toBe(true);
    expect(statusDot.classList.contains("kit-status-dot--animated")).toBe(true);
  });

  it("opens a failed issue workspace for retry and confirmed force-delete recovery", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-delete-failed",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            itemType: "issue",
            status: "deletion_failed",
            errorMessage: "workspace has uncommitted changes: notes.txt",
          }),
        ],
      },
    });
    mockDelete.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { ok: true, status: 204 },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-delete-failed" },
    });

    expect((await screen.findByText("Deletion failed")).getAttribute("title")).toBe(
      "workspace has uncommitted changes: notes.txt",
    );
    await fireEvent.click(container.querySelector(".ws-row")!);
    expect(mockNavigate).toHaveBeenCalledWith("/terminal/ws-delete-failed");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    expect((screen.getByRole("menuitem", { name: "Retry deletion..." }) as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(screen.getByRole("menuitem", { name: "Force delete workspace..." }));

    const dialog = await screen.findByRole("dialog", { name: "Force delete workspace?" });
    expect(dialog.textContent).toContain("This discards uncommitted changes");
    await fireEvent.click(within(dialog).getByRole("button", { name: "Force delete workspace" }));

    await waitFor(() => {
      expect(mockDelete).toHaveBeenCalledWith("/workspaces/{id}", {
        params: { path: { id: "ws-delete-failed" }, query: { force: true } },
        signal: expect.any(AbortSignal),
      });
    });
  });

  it("does not describe an externally disabled workspace as active work", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-disabled",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: {
        selectedId: "ws-disabled",
        isWorkspaceActionDisabled: () => true,
      },
    });

    await screen.findByText("PR 9");
    expect(screen.queryByLabelText("Deleting workspace")).toBeNull();
  });

  it("labels active terminal work with its pane title", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-working",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Working workspace",
            tmuxWorking: true,
            tmuxPaneTitle: "Running focused tests",
            tmuxActivitySource: "title",
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, {
      props: { selectedId: "ws-working" },
    });

    expect(await screen.findByLabelText("Working (title): Running focused tests")).toBeTruthy();
  });

  it.each([
    ["working", "Working", "Agent working"],
    ["approval", "Approval", "Agent approval"],
    ["input", "Input", "Agent input"],
  ] as const)("shows hook-reported agent %s state", async (agentState, label, ariaLabel) => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: `ws-${agentState}`,
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widget",
            number: 9,
            agentState,
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, { props: { selectedId: `ws-${agentState}` } });

    expect(await screen.findByText(label)).toBeTruthy();
    expect(screen.getByLabelText(ariaLabel)).toBeTruthy();
  });

  it("keeps a completed agent visible until its workspace row is opened", async () => {
    let agentStateUpdatedAt = "2026-07-28T12:00:00.000000001Z";
    mockGet.mockImplementation(() =>
      Promise.resolve({
        data: {
          workspaces: [
            workspaceFixture({
              id: "ws-done",
              provider: "github",
              platformHost: "github.com",
              owner: "acme",
              name: "widget",
              number: 9,
              agentState: "done",
              agentStateUpdatedAt,
            }),
          ],
        },
      }),
    );

    const first = render(WorkspaceListSidebar, {
      props: { selectedId: "" },
    });

    expect(await screen.findByText("Done")).toBeTruthy();
    expect(screen.getByLabelText("Agent done")).toBeTruthy();

    const row = first.container.querySelector<HTMLElement>(".ws-row");
    expect(row).toBeTruthy();
    await fireEvent.click(row!);

    expect(screen.queryByText("Done")).toBeNull();
    expect(mockNavigate).toHaveBeenCalledWith("/terminal/ws-done");

    first.unmount();
    const remounted = render(WorkspaceListSidebar, { props: { selectedId: "" } });
    await screen.findByText("PR 9");
    expect(screen.queryByText("Done")).toBeNull();

    remounted.unmount();
    agentStateUpdatedAt = "2026-07-28T12:00:00.000000002Z";
    render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Done")).toBeTruthy();
  });

  it("does not dismiss an unversioned completed agent state", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-unversioned-done",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widget",
            number: 10,
            agentState: "done",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, { props: { selectedId: "" } });
    expect(await screen.findByText("Done")).toBeTruthy();

    const row = container.querySelector<HTMLElement>(".ws-row");
    expect(row).toBeTruthy();
    await fireEvent.click(row!);

    expect(screen.getByText("Done")).toBeTruthy();
    expect(mockNavigate).toHaveBeenCalledWith("/terminal/ws-unversioned-done");
  });

  it("lets hook-reported idle override recent tmux output", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-idle-agent",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widget",
            number: 9,
            agentState: "idle",
            tmuxWorking: true,
            tmuxPaneTitle: "stale output",
            tmuxActivitySource: "output",
          }),
        ],
      },
    });

    render(WorkspaceListSidebar, { props: { selectedId: "ws-idle-agent" } });

    await screen.findByText("PR 9");
    expect(screen.queryByLabelText("Working (output): stale output")).toBeNull();
  });

  it("pushes an ahead workspace branch and shows a busy state while pending", async () => {
    const push = deferred<{
      data?: unknown;
      error?: unknown;
      response: { ok: boolean; status: number };
    }>();
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-ahead",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Ahead workspace",
            commitsAhead: 2,
          }),
        ],
      },
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/workspaces/{id}/push") return push.promise;
      return Promise.resolve({ data: {}, response: { ok: true, status: 200 } });
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-ahead" },
    });
    await screen.findByText("Ahead workspace");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    await fireEvent.click(screen.getByRole("menuitem", { name: /Push branch/ }));

    expect(mockPost).toHaveBeenCalledWith("/workspaces/{id}/push", {
      params: { path: { id: "ws-ahead" } },
      signal: expect.any(AbortSignal),
    });
    expect((screen.getByRole("menuitem", { name: /Pushing\.\.\./ }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByLabelText("Pushing branch")).toBeTruthy();

    push.resolve({ data: undefined, response: { ok: true, status: 200 } });
    await waitFor(() => {
      expect(screen.queryByRole("menuitem", { name: /Pushing\.\.\./ })).toBeNull();
      expect(screen.queryByLabelText("Pushing branch")).toBeNull();
    });
  });

  it("offers the first push when the configured upstream branch is missing", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          {
            ...workspaceFixture({
              id: "ws-first-push",
              provider: "github",
              platformHost: "github.com",
              owner: "acme",
              name: "widgets",
              number: 9,
              title: "First push workspace",
            }),
            branch_upstream_missing: true,
          },
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, { props: { selectedId: "ws-first-push" } });
    await screen.findByText("First push workspace");
    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    expect(screen.getByRole("menuitem", { name: "Push branch" })).toBeTruthy();
  });

  it("pulls a behind workspace branch and shows a busy state while pending", async () => {
    const pull = deferred<{
      data?: unknown;
      error?: unknown;
      response: { ok: boolean; status: number };
    }>();
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-behind",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Behind workspace",
            commitsBehind: 1,
          }),
        ],
      },
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/workspaces/{id}/pull") return pull.promise;
      return Promise.resolve({ data: {}, response: { ok: true, status: 200 } });
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-behind" },
    });
    await screen.findByText("Behind workspace");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    await fireEvent.click(screen.getByRole("menuitem", { name: /Pull remote changes/ }));

    expect(mockPost).toHaveBeenCalledWith("/workspaces/{id}/pull", {
      params: { path: { id: "ws-behind" } },
      signal: expect.any(AbortSignal),
    });
    expect((screen.getByRole("menuitem", { name: /Pulling\.\.\./ }) as HTMLButtonElement).disabled).toBe(true);

    pull.resolve({ data: undefined, response: { ok: true, status: 200 } });
    await waitFor(() => {
      expect(screen.queryByRole("menuitem", { name: /Pulling\.\.\./ })).toBeNull();
    });
  });

  it("opens a local workspace path from the context menu", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "ws-reveal",
                provider: "github",
                platformHost: "github.com",
                owner: "kenn-io",
                name: "kenn-forge",
                number: 12,
                title: "Reveal me",
              }),
            ],
          },
        });
      }
      return Promise.resolve({ data: {} });
    });
    mockPost.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { ok: true, status: 204 },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-reveal" },
    });
    await screen.findByText("Reveal me");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    await fireEvent.click(screen.getByRole("menuitem", { name: "Reveal in Finder" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/workspaces/{id}/reveal", {
        params: { path: { id: "ws-reveal" } },
        signal: expect.any(AbortSignal),
      });
    });
  });

  it("deletes a workspace from the context menu after in-app confirmation", async () => {
    vi.stubGlobal(
      "confirm",
      vi.fn(() => {
        throw new Error("native confirm should not be used");
      }),
    );
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [
              workspaceFixture({
                id: "ws-delete",
                provider: "github",
                platformHost: "github.com",
                owner: "kenn-io",
                name: "kenn-forge",
                number: 10,
                title: "Delete me",
              }),
            ],
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

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-delete" },
    });
    await screen.findByText("Delete me");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    await fireEvent.click(screen.getByRole("menuitem", { name: "Delete workspace..." }));

    const dialog = await screen.findByRole("dialog", { name: "Delete workspace?" });
    expect(dialog.textContent).toContain('Delete workspace "Delete me"?');
    expect(dialog.textContent).toContain("This removes its managed worktree and runtime sessions.");
    expect(window.confirm).not.toHaveBeenCalled();
    await fireEvent.click(within(dialog).getByRole("button", { name: "Delete workspace" }));

    await waitFor(() => {
      expect(mockDelete).toHaveBeenCalledWith("/workspaces/{id}", {
        params: { path: { id: "ws-delete" } },
        signal: expect.any(AbortSignal),
      });
    });
    expect(mockNavigate).toHaveBeenCalledWith("/workspaces");
  });

  it("keeps a workspace when context menu deletion is cancelled in-app", async () => {
    vi.stubGlobal(
      "confirm",
      vi.fn(() => {
        throw new Error("native confirm should not be used");
      }),
    );
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-keep",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 11,
            title: "Keep me",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-keep" },
    });
    await screen.findByText("Keep me");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    await fireEvent.click(screen.getByRole("menuitem", { name: "Delete workspace..." }));
    const dialog = await screen.findByRole("dialog", { name: "Delete workspace?" });
    await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog", { name: "Delete workspace?" })).toBeNull();
    expect(window.confirm).not.toHaveBeenCalled();
    expect(mockDelete).not.toHaveBeenCalled();
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("omits local filesystem actions and offers force-delete recovery for remote workspaces", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/snapshot") {
        return Promise.resolve({
          data: {
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {},
                platform: "darwin",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: "epyc",
                diagnostics: [],
                id: "epyc",
                kind: "remote",
                name: "epyc",
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
            workspaces: [
              {
                ...workspaceFixture({
                  id: "ws-remote",
                  provider: "github",
                  platformHost: "github.com",
                  owner: "remote",
                  name: "service",
                  number: 12,
                  title: "Remote workspace",
                  status: "deletion_failed",
                  errorMessage: "workspace has uncommitted changes: notes.txt",
                }),
                fleet_host_key: "epyc",
                fleet_host_name: "epyc",
              },
            ],
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

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-remote", selectedHostKey: "epyc" },
    });
    await screen.findByText("Remote workspace");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    expect(screen.getByRole("menu", { name: "Workspace actions" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Copy worktree path" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Reveal in Finder" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Refresh git status" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("menuitem", { name: "Force delete workspace..." }));

    const dialog = await screen.findByRole("dialog", { name: "Force delete workspace?" });
    await fireEvent.click(within(dialog).getByRole("button", { name: "Force delete workspace" }));

    await waitFor(() => {
      expect(mockDelete).toHaveBeenCalledWith("/fleet/hosts/{host_key}/workspaces/{id}", {
        params: {
          path: { host_key: "epyc", id: "ws-remote" },
          query: { force: true },
        },
        signal: expect.any(AbortSignal),
      });
    });
  });

  it("does not show push or pull commands for diverged workspace branches", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-diverged",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 9,
            title: "Diverged workspace",
            commitsAhead: 1,
            commitsBehind: 2,
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-diverged" },
    });
    await screen.findByText("Diverged workspace");

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);

    expect(screen.queryByRole("menuitem", { name: "Push branch" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Pull remote changes" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Refresh git status" })).toBeTruthy();
  });

  function adHocWorkspaceFixture(overrides: Partial<WorkspaceFixtureOptions> = {}) {
    return workspaceFixture({
      id: "ws-adhoc",
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "kenn-forge",
      number: 0,
      branch: "spike/rate-limits",
      itemType: "adhoc",
      itemKey: "adhoc:spike/rate-limits",
      ...overrides,
    });
  }

  it("labels an ad-hoc workspace by branch and shows no item bubble", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: [adHocWorkspaceFixture({ worktreeDirty: true })] },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-adhoc" },
    });
    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));

    // No provider item and no Kata task: there is nothing for a bubble to
    // open, and the row must never advertise #0.
    expect(container.querySelector(".item-bubble")).toBeNull();
    expect(container.querySelector(".ws-row-aside > .item-bubble-slot")).toBeTruthy();
    expect(container.querySelector(".ws-row-aside > .item-bubble-slot + .worktree-dirty")).toBeTruthy();
    expect(container.textContent).not.toContain("#0");
  });

  it("shows the pull request detected for an ad-hoc workspace", async () => {
    // A workspace created directly gains a PR once its branch is pushed and
    // the backend links it. The detail pane already resolves that PR, so the
    // row must advertise it instead of staying permanently bubble-less.
    mockGet.mockResolvedValue({
      data: {
        workspaces: [adHocWorkspaceFixture({ associatedPRNumber: 840 })],
      },
    });
    const onOpenItemSidebar = vi.fn();

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-adhoc", onOpenItemSidebar },
    });
    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));

    const bubble = container.querySelector(".item-bubble");
    expect(bubble).not.toBeNull();
    expect(bubble!.textContent?.trim()).toBe("#840");
    expect(bubble!.getAttribute("title")).toBe("Open PR #840");

    await fireEvent.click(bubble!);
    expect(onOpenItemSidebar).toHaveBeenCalledWith("ws-adhoc", "pr", undefined);
  });

  it("hides actions for a removed workspace source item", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          workspaceFixture({
            id: "ws-removed",
            provider: "github",
            platformHost: "github.com",
            owner: "acme",
            name: "widgets",
            number: 840,
            sourceItemVisible: false,
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-removed" },
    });
    await waitFor(() => expect(rowTitles(container)).toEqual(["PR 840"]));

    expect(container.querySelector(".item-bubble")).toBeNull();
    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    expect(screen.queryByRole("menuitem", { name: "Open item on GitHub" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Copy item URL" })).toBeNull();
  });

  it("offers provider item actions for an ad-hoc workspace with a detected PR", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [adHocWorkspaceFixture({ associatedPRNumber: 840 })],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-adhoc" },
    });
    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));

    await fireEvent.contextMenu(container.querySelector(".ws-row")!);
    await fireEvent.click(screen.getByRole("menuitem", { name: "Copy item URL" }));

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith("https://github.com/kenn-io/kenn-forge/pull/840");
  });

  it("finds an ad-hoc workspace by its detected PR number", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          adHocWorkspaceFixture({ associatedPRNumber: 840 }),
          workspaceFixture({
            id: "ws-pr",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 4,
            title: "Some pull request",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-adhoc" },
    });
    await screen.findByText("Some pull request");

    await fireEvent.input(screen.getByLabelText("Filter workspaces"), {
      target: { value: "#840" },
    });

    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));
  });

  it("finds an ad-hoc workspace by branch", async () => {
    mockGet.mockResolvedValue({
      data: {
        workspaces: [
          adHocWorkspaceFixture(),
          workspaceFixture({
            id: "ws-pr",
            provider: "github",
            platformHost: "github.com",
            owner: "kenn-io",
            name: "kenn-forge",
            number: 4,
            title: "Some pull request",
          }),
        ],
      },
    });

    const { container } = render(WorkspaceListSidebar, {
      props: { selectedId: "ws-adhoc" },
    });
    await screen.findByText("Some pull request");

    await fireEvent.input(screen.getByLabelText("Filter workspaces"), {
      target: { value: "rate-limits" },
    });

    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));
  });

  it("opens the new-workspace dialog seeded with the selected workspace repo", async () => {
    mockGet.mockResolvedValue({
      data: { workspaces: [adHocWorkspaceFixture()] },
    });

    const { container } = render(WorkspaceListSidebar, { props: { selectedId: "ws-adhoc" } });
    await waitFor(() => expect(rowTitles(container)).toEqual(["spike/rate-limits"]));
    expect(isNewWorkspaceDialogOpen()).toBe(false);

    await fireEvent.click(screen.getByRole("button", { name: "New workspace" }));

    expect(isNewWorkspaceDialogOpen()).toBe(true);
    expect(getNewWorkspaceSeedRepo()).toEqual({
      provider: "github",
      platformHost: "github.com",
      owner: "kenn-io",
      name: "kenn-forge",
    });
  });
});
