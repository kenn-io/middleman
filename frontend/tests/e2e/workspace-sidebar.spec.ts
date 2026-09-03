import { expect, test } from "@playwright/test";

import type { ProblemError } from "../../src/lib/api/generated/models/index.js";
import { createMockApiHandler } from "../../src/test/mockApiFetch.js";
import { mockApi } from "./support/mockApi";

type ProblemBody = ProblemError;

function problem(code: ProblemBody["code"], status: number, detail?: string): ProblemBody {
  return { type: "about:blank", code, status, ...(detail === undefined ? {} : { detail }) };
}

function clearWorkspaceSidebarTabStorage(): void {
  for (const key of Object.keys(localStorage)) {
    if (key.startsWith("kenn-forge-workspace-sidebar-tab:")) localStorage.removeItem(key);
  }
}

function workspaceRepoRef(owner = "acme", name = "widgets", host = "github.com", provider = "github") {
  return {
    provider,
    platform_host: host,
    owner,
    name,
    repo_path: `${owner}/${name}`,
  };
}

const testWorkspace = {
  id: "ws-123",
  platform_host: "github.com",
  repo_owner: "acme",
  repo_name: "widgets",
  repo: workspaceRepoRef(),
  item_type: "pull_request",
  item_number: 42,
  source_item_visible: true,
  git_head_ref: "feature/auth",
  worktree_path: "/tmp/worktrees/ws-123",
  tmux_session: "kenn-forge-ws-123",
  tmux_pane_title: null,
  tmux_working: false,
  tmux_activity_source: "unknown",
  tmux_last_output_at: null,
  status: "ready",
  created_at: "2026-04-10T12:00:00Z",
  enrichment_status: "fresh",
  mr_title: "Add auth middleware",
  mr_state: "open",
  mr_is_draft: false,
};

const testIssueWorkspace = {
  id: "ws-issue-7",
  platform_host: "github.com",
  repo_owner: "acme",
  repo_name: "widgets",
  repo: workspaceRepoRef(),
  item_type: "issue",
  item_number: 7,
  source_item_visible: true,
  git_head_ref: "kenn-forge/issue-7",
  worktree_path: "/tmp/worktrees/ws-issue-7",
  tmux_session: "kenn-forge-ws-issue-7",
  tmux_working: false,
  tmux_activity_source: "unknown",
  tmux_last_output_at: null,
  status: "ready",
  created_at: "2026-04-10T12:00:00Z",
  enrichment_status: "fresh",
  mr_title: "Theme toggle does not stick",
  mr_state: "open",
};

const testIssueWorkspaceWithAssociatedPR = {
  ...testIssueWorkspace,
  associated_pr_number: 42,
};

const roborevRepos = {
  repos: [
    {
      name: "widgets",
      root_path: "/home/dev/widgets",
      count: 5,
    },
  ],
  total_count: 1,
};

const roborevJobs = {
  jobs: [
    {
      id: 1,
      status: "done",
      verdict: "pass",
      agent: "code-review",
      job_type: "review",
      git_ref: "abc12345",
      commit_subject: "Add auth middleware",
      enqueued_at: "2026-04-10T12:00:00Z",
      branch: "feature/auth",
      repo_name: "widgets",
      repo_id: 1,
      agentic: false,
      prompt_prebuilt: false,
      retry_count: 0,
      token_usage: '{"total_output_tokens":28800,"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}',
    },
  ],
  has_more: false,
  stats: { done: 1, closed: 0, open: 0 },
};

const roborevReview = {
  id: 1,
  job_id: 1,
  output: "No issues found. Code follows project conventions.",
  closed: false,
};

const roborevStatus = {
  available: true,
  version: "0.52.0",
  endpoint: "http://127.0.0.1:17373",
  active_workers: 1,
  max_workers: 4,
  queued_jobs: 2,
  running_jobs: 1,
  completed_jobs: 5,
  failed_jobs: 0,
  canceled_jobs: 0,
};

const workspaceRuntime = {
  launch_targets: [
    {
      key: "codex",
      label: "Codex",
      kind: "agent",
      source: "builtin",
      command: ["codex"],
      available: true,
    },
    {
      key: "shell",
      label: "Shell",
      kind: "shell",
      source: "system",
      command: ["tmux"],
      available: false,
      disabled_reason: "tmux not found",
    },
    {
      key: "plain_shell",
      label: "Plain shell",
      kind: "plain_shell",
      source: "system",
      command: ["/bin/sh"],
      available: true,
    },
  ],
  sessions: [],
};

type RuntimeTarget = (typeof workspaceRuntime.launch_targets)[number];
type RuntimeSession = {
  key: string;
  workspace_id: string;
  target_key: string;
  label: string;
  kind: RuntimeTarget["kind"];
  status: "starting" | "running" | "exited" | "error";
  created_at: string;
};
type WorkspaceRuntime = Omit<typeof workspaceRuntime, "sessions"> & {
  sessions: RuntimeSession[];
};
type RuntimeEvents = {
  launches: string[];
  renames: Array<{ sessionKey: string; label: string }>;
  deletes: string[];
};

/**
 * Mock all routes needed for terminal view tests.
 * Registers mockApi first (catch-all), then layers
 * workspace and roborev routes on top so they take
 * priority (Playwright uses LIFO route matching).
 */
type WorkspaceFixture = typeof testWorkspace | typeof testIssueWorkspace | typeof testIssueWorkspaceWithAssociatedPR;
type WorkspaceCommitFixture = {
  sha: string;
  message: string;
  author_name: string;
  authored_at: string;
};

async function installLinkedItemWorkspaceDetail(
  page: import("@playwright/test").Page,
  itemType: "pull_request" | "issue",
  workspace: WorkspaceFixture,
): Promise<void> {
  const api = createMockApiHandler();
  const routePrefix = itemType === "pull_request" ? "/api/v1/pulls/" : "/api/v1/issues/";
  await page.route(
    (url) => url.pathname.startsWith(routePrefix) && url.pathname.endsWith(`/${workspace.item_number}`),
    async (route) => {
      const request = route.request();
      const response = api.handle({
        method: request.method().toUpperCase(),
        url: new URL(request.url()),
        bodyText: request.postData() ?? "",
      });
      const body = (await response.json()) as Record<string, unknown>;
      await route.fulfill({
        status: response.status,
        contentType: response.headers.get("Content-Type") ?? "application/json",
        body: JSON.stringify({
          ...body,
          workspace: { id: workspace.id, status: workspace.status },
        }),
      });
    },
  );
}

async function setupTerminalMocks(
  page: import("@playwright/test").Page,
  opts?: {
    workspace?: WorkspaceFixture;
    roborevRepos?: typeof roborevRepos;
    roborevJobs?: typeof roborevJobs;
    roborevStatus?: typeof roborevStatus;
    roborevReview?: typeof roborevReview;
    workspaceDetailResponses?: Array<{
      status: number;
      body?: unknown;
    }>;
    workspaceDeleteResponses?: Array<{
      status: number;
      body?: unknown;
    }>;
    workspaceRetryResponse?: {
      status: number;
      body?: unknown;
    };
    diffRequests?: string[];
    commitRequests?: string[];
    workspaceCommitResponses?: WorkspaceCommitFixture[][];
    runtime?: WorkspaceRuntime;
    runtimeEvents?: RuntimeEvents;
  },
): Promise<{ runtime: WorkspaceRuntime }> {
  const ws = opts?.workspace ?? testWorkspace;
  const rrRepos = opts?.roborevRepos ?? roborevRepos;
  const rrJobs = opts?.roborevJobs ?? roborevJobs;
  const rrStatus = opts?.roborevStatus ?? roborevStatus;
  const rrReview = opts?.roborevReview ?? roborevReview;
  const detailResponses = [...(opts?.workspaceDetailResponses ?? [])];
  const deleteResponses = [...(opts?.workspaceDeleteResponses ?? [])];
  const commitResponses = [
    ...(opts?.workspaceCommitResponses ?? [
      [
        {
          sha: "sha2",
          message: "second commit",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
        {
          sha: "sha1",
          message: "first commit",
          author_name: "Alice",
          authored_at: "2026-01-01T00:00:00Z",
        },
      ],
    ]),
  ];
  let commitResponseIndex = 0;
  const runtime = JSON.parse(JSON.stringify(opts?.runtime ?? workspaceRuntime)) as WorkspaceRuntime;

  // Register catch-all first — later routes override.
  await mockApi(page);

  await page.route("**/api/v1/events", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: "",
    });
  });

  // Register list route first, then specific route.
  // Playwright uses LIFO matching, so the specific
  // /workspaces/:id registered last takes priority
  // over the list-only pattern.
  await page.route("**/api/v1/snapshot**", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaces: [ws] }),
      });
      return;
    }
    await route.fulfill({ status: 200 });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/retry`, async (route) => {
    const response = opts?.workspaceRetryResponse ?? {
      status: 202,
      body: { ...ws, status: "creating" },
    };
    await route.fulfill({
      status: response.status,
      contentType: "application/json",
      body: JSON.stringify(response.body ?? {}),
    });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/refresh`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.fulfill({ status: 405 });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ws),
    });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/files*`, async (route) => {
    opts?.diffRequests?.push(route.request().url());
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        stale: false,
        whitespace_only_count: 0,
        files: [
          {
            path: "src/auth.go",
            old_path: "src/auth.go",
            status: "modified",
            is_binary: false,
            is_whitespace_only: false,
            additions: 1,
            deletions: 1,
            hunks: [],
            patch: "",
          },
        ],
      }),
    });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/diff*`, async (route) => {
    opts?.diffRequests?.push(route.request().url());
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        stale: false,
        whitespace_only_count: 0,
        files: [
          {
            path: "src/auth.go",
            old_path: "src/auth.go",
            status: "modified",
            is_binary: false,
            is_whitespace_only: false,
            additions: 1,
            deletions: 1,
            hunks: [],
            patch: "",
          },
        ],
      }),
    });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/commits`, async (route) => {
    opts?.commitRequests?.push(route.request().url());
    const commits = commitResponses[Math.min(commitResponseIndex, commitResponses.length - 1)] ?? [];
    commitResponseIndex += 1;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        commits,
      }),
    });
  });

  await page.route(
    (url) => url.pathname === `/api/v1/workspaces/${ws.id}`,
    async (route) => {
      if (route.request().method() === "GET") {
        const nextResponse = detailResponses.shift();
        if (nextResponse) {
          await route.fulfill({
            status: nextResponse.status,
            contentType: "application/json",
            body: JSON.stringify(nextResponse.body ?? {}),
          });
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(ws),
        });
        return;
      }
      // DELETE
      const nextDelete = deleteResponses.shift();
      if (nextDelete) {
        if (nextDelete.body === undefined) {
          await route.fulfill({ status: nextDelete.status });
          return;
        }
        await route.fulfill({
          status: nextDelete.status,
          contentType: "application/json",
          body: JSON.stringify(nextDelete.body),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    },
  );

  await page.route(`**/api/v1/workspaces/${ws.id}/runtime`, async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(runtime),
      });
      return;
    }
    await route.fulfill({ status: 405 });
  });

  await page.route(`**/api/v1/workspaces/${ws.id}/runtime/sessions`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.fulfill({ status: 405 });
      return;
    }
    const body = JSON.parse(route.request().postData() ?? "{}") as {
      target_key?: string;
    };
    const target = runtime.launch_targets.find((candidate) => candidate.key === body.target_key);
    if (!target || !target.available) {
      await route.fulfill({
        status: 400,
        contentType: "application/json",
        body: JSON.stringify({
          detail: "launch target unavailable",
        }),
      });
      return;
    }
    opts?.runtimeEvents?.launches.push(target.key);
    let session = runtime.sessions.find(
      (candidate) => candidate.target_key === target.key && ["running", "starting"].includes(candidate.status),
    );
    if (!session) {
      const previous = runtime.sessions.find((candidate) => candidate.target_key === target.key);
      session = {
        key: previous?.key ?? `${ws.id}:${target.key}`,
        workspace_id: ws.id,
        target_key: target.key,
        label: target.label,
        kind: target.kind,
        status: "running",
        created_at: "2026-04-10T12:00:00Z",
      };
      runtime.sessions = [...runtime.sessions.filter((candidate) => candidate.key !== session.key), session];
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(session),
    });
  });

  await page.route(
    (url) => url.pathname.startsWith(`/api/v1/workspaces/${ws.id}/runtime/sessions/`),
    async (route) => {
      const url = new URL(route.request().url());
      const sessionKey = decodeURIComponent(url.pathname.split("/").at(-1) ?? "");

      if (route.request().method() === "PATCH") {
        const body = JSON.parse(route.request().postData() ?? "{}") as {
          label?: string;
        };
        const label = body.label?.trim() ?? "";
        const index = runtime.sessions.findIndex((session) => session.key === sessionKey);
        if (index < 0 || !label) {
          await route.fulfill({
            status: 404,
            contentType: "application/json",
            body: JSON.stringify({ detail: "session not found" }),
          });
          return;
        }
        const updated = {
          ...runtime.sessions[index],
          label,
        };
        runtime.sessions = runtime.sessions.map((session) => (session.key === sessionKey ? updated : session));
        opts?.runtimeEvents?.renames.push({ sessionKey, label });
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(updated),
        });
        return;
      }

      if (route.request().method() !== "DELETE") {
        await route.continue();
        return;
      }
      runtime.sessions = runtime.sessions.filter((session) => session.key !== sessionKey);
      opts?.runtimeEvents?.deletes.push(sessionKey);
      await route.fulfill({ status: 204 });
    },
  );

  // Route roborev API calls using a predicate to avoid
  // matching Vite module URLs like /@fs/.../api/roborev/...
  await page.route(
    (url) => url.pathname === "/api/v1/roborev/status",
    async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(rrStatus),
      });
    },
  );

  await page.route(
    (url) => url.pathname.startsWith("/api/roborev/"),
    async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/api/repos")) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(rrRepos),
        });
        return;
      }
      if (url.pathname.endsWith("/api/jobs") || url.pathname.includes("/api/jobs?")) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(rrJobs),
        });
        return;
      }
      if (url.pathname.endsWith("/status")) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(rrStatus),
        });
        return;
      }
      // A fetched review is what makes the drawer offer Close Review, so the
      // footer can be exercised with its full action set.
      if (url.pathname.endsWith("/api/review")) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(rrReview),
        });
        return;
      }
      if (url.pathname.endsWith("/api/comments")) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ comments: [] }),
        });
        return;
      }
      if (url.pathname.includes("/stream/events")) {
        await route.fulfill({
          status: 200,
          contentType: "text/event-stream",
          body: "",
        });
        return;
      }
      await route.fulfill({ status: 404 });
    },
  );

  return { runtime };
}

async function installControllableTerminalWebSockets(page: import("@playwright/test").Page): Promise<void> {
  await page.addInitScript(() => {
    class ControllableTerminalWebSocket extends EventTarget {
      static CONNECTING = 0;
      static OPEN = 1;
      static CLOSING = 2;
      static CLOSED = 3;

      binaryType = "arraybuffer";
      extensions = "";
      onclose: ((event: CloseEvent) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      protocol = "";
      readyState = ControllableTerminalWebSocket.OPEN;
      readonly url: string;

      constructor(url: string | URL) {
        super();
        this.url = String(url);
        const sockets = (
          window as unknown as { __kenn_forgeControllableTerminalSockets: ControllableTerminalWebSocket[] }
        ).__kenn_forgeControllableTerminalSockets;
        sockets.push(this);
        queueMicrotask(() => {
          const opened = new Event("open");
          this.dispatchEvent(opened);
          this.onopen?.(opened);
          this.emitControl({ type: "replay_ready" });
        });
      }

      emitControl(message: Record<string, unknown>): void {
        const event = new MessageEvent("message", { data: JSON.stringify(message) });
        this.dispatchEvent(event);
        this.onmessage?.(event);
      }

      close(): void {
        this.readyState = ControllableTerminalWebSocket.CLOSED;
        const closed = new CloseEvent("close");
        this.dispatchEvent(closed);
        this.onclose?.(closed);
      }

      send(): void {}
    }

    Object.defineProperty(window, "__kenn_forgeControllableTerminalSockets", {
      configurable: true,
      value: [] as ControllableTerminalWebSocket[],
    });
    window.WebSocket = ControllableTerminalWebSocket as unknown as typeof WebSocket;
  });
}

async function emitTerminalControl(
  page: import("@playwright/test").Page,
  sessionKey: string,
  message: Record<string, unknown>,
): Promise<void> {
  await page.evaluate(
    ({ sessionKey, message }) => {
      const sockets = (
        window as unknown as {
          __kenn_forgeControllableTerminalSockets: Array<{
            url: string;
            emitControl: (message: Record<string, unknown>) => void;
          }>;
        }
      ).__kenn_forgeControllableTerminalSockets;
      const socket = sockets.find(({ url }) => url.includes(encodeURIComponent(sessionKey)));
      if (!socket) throw new Error(`Missing terminal socket for ${sessionKey}`);
      socket.emitControl(message);
    },
    { sessionKey, message },
  );
}

async function dragWorkflowTabToGroup(
  page: import("@playwright/test").Page,
  tabLabel: string,
  groupIndex: number,
  position: "center" | "left-edge",
): Promise<void> {
  await page.evaluate(
    ({ tabLabel, groupIndex, position }) => {
      const source = Array.from(document.querySelectorAll('[role="tab"]')).find((element) =>
        element.textContent?.includes(tabLabel),
      );
      const target = Array.from(document.querySelectorAll('[aria-label="Workflow group drop targets"]'))[groupIndex];
      if (!(source instanceof HTMLElement)) {
        throw new Error(`Missing workflow tab: ${tabLabel}`);
      }
      if (!(target instanceof HTMLElement)) {
        throw new Error(`Missing workflow group: ${groupIndex}`);
      }

      const transfer = new DataTransfer();
      source.dispatchEvent(
        new DragEvent("dragstart", {
          bubbles: true,
          cancelable: true,
          dataTransfer: transfer,
        }),
      );

      const rect = target.getBoundingClientRect();
      const clientX = position === "left-edge" ? rect.left + 4 : rect.left + rect.width / 2;
      const clientY = rect.top + rect.height / 2;
      target.dispatchEvent(
        new DragEvent("dragover", {
          bubbles: true,
          cancelable: true,
          clientX,
          clientY,
          dataTransfer: transfer,
        }),
      );
      target.dispatchEvent(
        new DragEvent("drop", {
          bubbles: true,
          cancelable: true,
          clientX,
          clientY,
          dataTransfer: transfer,
        }),
      );
      source.dispatchEvent(
        new DragEvent("dragend", {
          bubbles: true,
          cancelable: true,
          dataTransfer: transfer,
        }),
      );
    },
    { tabLabel, groupIndex, position },
  );
}

function workflowDragRuntime(): WorkspaceRuntime {
  return {
    ...workspaceRuntime,
    sessions: [
      {
        key: "ws-123:codex",
        workspace_id: "ws-123",
        target_key: "codex",
        label: "Codex",
        kind: "agent",
        status: "running",
        created_at: "2026-04-10T12:00:00Z",
      },
      {
        key: "ws-123:reviewer",
        workspace_id: "ws-123",
        target_key: "codex",
        label: "Reviewer",
        kind: "agent",
        status: "running",
        created_at: "2026-04-10T12:00:00Z",
      },
    ],
  };
}

function workflowDragLayout() {
  return {
    version: 1,
    open: false,
    dock: "bottom",
    height: 300,
    activeSessionKey: null,
    tree: null,
    terminalGroups: [],
    activeTerminalGroupID: null,
    sessionRegions: {
      "ws-123:codex": "workflow",
      "ws-123:reviewer": "workflow",
    },
    workflowMode: "tabs",
    workflowTree: {
      type: "split",
      id: "workflow-root",
      direction: "horizontal",
      ratio: 0.5,
      first: {
        type: "leaf",
        id: "workflow-left",
        tabs: ["home", "session:ws-123:codex"],
        activeTabKey: "session:ws-123:codex",
      },
      second: {
        type: "leaf",
        id: "workflow-right",
        tabs: ["session:ws-123:reviewer"],
        activeTabKey: "session:ws-123:reviewer",
      },
    },
    activeWorkflowLeafID: "workflow-left",
    recentWorkflowLeafIDs: ["workflow-left", "workflow-right"],
    customSessionLabels: {},
  };
}

function topDockedTerminalWorkflowLayout() {
  return {
    version: 1,
    open: true,
    dock: "top",
    height: 300,
    activeSessionKey: null,
    tree: null,
    terminalGroups: [],
    activeTerminalGroupID: null,
    sessionRegions: {
      "ws-123:codex": "workflow",
    },
    workflowMode: "tabs",
    workflowTree: {
      type: "leaf",
      id: "workflow-root",
      tabs: ["home", "terminal", "session:ws-123:codex"],
      activeTabKey: "home",
    },
    activeWorkflowLeafID: "workflow-root",
    recentWorkflowLeafIDs: ["workflow-root"],
    customSessionLabels: {},
  };
}

function closedTopDockedTerminalWorkflowLayout() {
  return {
    version: 1,
    open: false,
    dock: "top",
    height: 300,
    activeSessionKey: "ws-123:plain_shell",
    tree: {
      type: "leaf",
      id: "terminal-leaf",
      sessionKey: "ws-123:plain_shell",
    },
    terminalGroups: [
      {
        id: "terminal-group",
        activeSessionKey: "ws-123:plain_shell",
        tree: {
          type: "leaf",
          id: "terminal-leaf",
          sessionKey: "ws-123:plain_shell",
        },
      },
    ],
    activeTerminalGroupID: "terminal-group",
    sessionRegions: {
      "ws-123:plain_shell": "terminal",
    },
    workflowMode: "tabs",
    workflowTree: {
      type: "leaf",
      id: "workflow-root",
      tabs: ["home", "terminal"],
      activeTabKey: "home",
    },
    activeWorkflowLeafID: "workflow-root",
    recentWorkflowLeafIDs: ["workflow-root"],
    customSessionLabels: {},
  };
}

function hasWorkspaceDiffRequest(
  requests: string[],
  expected: {
    base: string;
    commit?: string | null;
  },
): boolean {
  return requests.some((requestURL) => {
    const url = new URL(requestURL);
    return (
      url.pathname === "/api/v1/workspaces/ws-123/diff" &&
      url.searchParams.get("base") === expected.base &&
      (expected.commit === undefined ||
        (expected.commit === null
          ? !url.searchParams.has("commit") && !url.searchParams.has("from") && !url.searchParams.has("to")
          : url.searchParams.get("commit") === expected.commit))
    );
  });
}

function shellWorkflowPreset() {
  const shellSourceKey = "preset-shell";
  return {
    id: "preset-shell",
    name: "Shell focus",
    createdAt: "2026-04-10T12:00:00.000Z",
    updatedAt: "2026-04-10T12:00:00.000Z",
    sessions: [
      {
        sourceKey: shellSourceKey,
        targetKey: "plain_shell",
        region: "workflow",
        label: "Shell",
      },
    ],
    layout: {
      version: 1,
      open: false,
      dock: "bottom",
      height: 300,
      activeSessionKey: null,
      tree: null,
      terminalGroups: [],
      activeTerminalGroupID: null,
      sessionRegions: {
        [shellSourceKey]: "workflow",
      },
      workflowMode: "tabs",
      workflowTree: {
        type: "leaf",
        id: "workflow-root",
        tabs: ["home", `session:${shellSourceKey}`],
        activeTabKey: `session:${shellSourceKey}`,
      },
      activeWorkflowLeafID: "workflow-root",
      recentWorkflowLeafIDs: ["workflow-root"],
      customSessionLabels: {},
    },
  };
}

test("roborev status mock ignores Vite module URLs", async ({ page }) => {
  await setupTerminalMocks(page);
  await page.goto("/");

  const response = await page.evaluate(async () => {
    const res = await fetch("/@fs/tmp/project/api/v1/roborev/status");
    return {
      status: res.status,
      body: await res.json(),
    };
  });

  expect(response).toEqual({
    status: 404,
    body: {
      error: "Unhandled GET /@fs/tmp/project/api/v1/roborev/status",
    },
  });
});

test("provider-aware detail mocks enforce provider and host identity", async ({ page }) => {
  await setupTerminalMocks(page);
  await page.goto("/");

  const statuses = await page.evaluate(async () => {
    const paths = [
      "/api/v1/pulls/github/acme/widgets/42",
      "/api/v1/pulls/github/acme/widgets/84",
      "/api/v1/host/example.com/pulls/github/acme/widgets/84",
      "/api/v1/pulls/gitlab/acme/widgets/42",
      "/api/v1/issues/github/acme/widgets/7",
      "/api/v1/issues/gitlab/acme/widgets/7",
    ];
    return Object.fromEntries(
      await Promise.all(
        paths.map(async (path) => {
          const response = await fetch(path);
          return [path, response.status];
        }),
      ),
    );
  });

  expect(statuses).toEqual({
    "/api/v1/pulls/github/acme/widgets/42": 200,
    "/api/v1/pulls/github/acme/widgets/84": 404,
    "/api/v1/host/example.com/pulls/github/acme/widgets/84": 200,
    "/api/v1/pulls/gitlab/acme/widgets/42": 404,
    "/api/v1/issues/github/acme/widgets/7": 200,
    "/api/v1/issues/gitlab/acme/widgets/7": 404,
  });
});

test("phone workspace list keeps its selected terminal alive through linked PR navigation", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.addInitScript(() => {
    type RecordedSocket = { url: string };
    const recordedSockets: RecordedSocket[] = [];
    Object.defineProperty(window, "__kenn_forgeMobileTerminalSockets", { value: recordedSockets });

    class MockTerminalWebSocket extends EventTarget {
      static CONNECTING = 0;
      static OPEN = 1;
      static CLOSING = 2;
      static CLOSED = 3;

      binaryType = "arraybuffer";
      extensions = "";
      onclose: ((event: CloseEvent) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      protocol = "";
      readyState = MockTerminalWebSocket.OPEN;
      readonly url: string;

      constructor(url: string | URL) {
        super();
        this.url = String(url);
        recordedSockets.push({ url: this.url });
        queueMicrotask(() => {
          const opened = new Event("open");
          this.dispatchEvent(opened);
          this.onopen?.(opened);
          const replayReady = new MessageEvent("message", {
            data: JSON.stringify({ type: "replay_ready" }),
          });
          this.dispatchEvent(replayReady);
          this.onmessage?.(replayReady);
        });
      }

      close(): void {
        this.readyState = MockTerminalWebSocket.CLOSED;
        const closed = new CloseEvent("close");
        this.dispatchEvent(closed);
        this.onclose?.(closed);
      }

      send(): void {}
    }

    window.WebSocket = MockTerminalWebSocket as unknown as typeof WebSocket;
  });
  await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces");

  await expect(page.getByRole("combobox", { name: /Phone mode/ })).toHaveText("Workspaces");
  await expect(page.getByRole("searchbox", { name: "Filter workspaces" })).toBeVisible();
  await page.getByRole("button", { name: "View workspace options" }).click();
  await expect(page.getByRole("dialog", { name: "View workspace options" })).toBeVisible();
  await expect(page.getByRole("radio", { name: /Org \/ repo/ })).toBeChecked();
  await page.getByRole("button", { name: "Close View options" }).click();

  await page.getByRole("searchbox", { name: "Filter workspaces" }).fill("feature/auth");
  await expect(page.getByRole("button", { name: "Open workspace Add auth middleware" })).toBeVisible();
  await page.getByRole("searchbox", { name: "Filter workspaces" }).fill("");
  await page.getByRole("button", { name: "Open workspace Add auth middleware" }).click();

  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123$/);
  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Codex");
  await expect(page.locator(".mobile-workspace-terminal__stage")).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __kenn_forgeMobileTerminalSockets: Array<{ url: string }>;
            }
          ).__kenn_forgeMobileTerminalSockets.filter(({ url }) => url.includes("/ws/v1/workspaces/")).length,
      ),
    )
    .toBe(2);

  await page.getByRole("button", { name: "Open linked PR #42" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123\/item$/);
  await expect(page.locator(".mobile-workspace-item .pull-detail .detail-title")).toContainText(
    "Add browser regression coverage",
  );
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __kenn_forgeMobileTerminalSockets: Array<{ url: string }>;
            }
          ).__kenn_forgeMobileTerminalSockets.filter(({ url }) => url.includes("/ws/v1/workspaces/")).length,
      ),
    )
    .toBe(2);

  await page.getByRole("button", { name: "Back to workspace terminal" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123$/);
  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Codex");
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __kenn_forgeMobileTerminalSockets: Array<{ url: string }>;
            }
          ).__kenn_forgeMobileTerminalSockets.filter(({ url }) => url.includes("/ws/v1/workspaces/")).length,
      ),
    )
    .toBe(2);
});

test("phone workspace terminal opens its linked issue and returns", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page, { workspace: testIssueWorkspace });

  await page.goto("/m/workspaces/local/ws-issue-7");

  await expect(page.getByRole("button", { name: "Open linked issue #7" })).toBeVisible();
  await page.getByRole("button", { name: "Open linked issue #7" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-issue-7\/item$/);
  await expect(page.locator(".mobile-workspace-item .issue-detail .detail-title")).toContainText(
    "Theme toggle does not stick",
  );

  await page.getByRole("button", { name: "Back to workspace terminal" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-issue-7$/);
  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Workspace");
});

test("phone offers the durable workspace terminal when no runtime session is launched", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  await setupTerminalMocks(page);

  await page.goto("/m/workspaces/local/ws-123");

  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Workspace");
  await expect(page.locator(".mobile-workspace-terminal__stage")).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(() =>
        (
          window as unknown as {
            __kenn_forgeControllableTerminalSockets: Array<{ url: string }>;
          }
        ).__kenn_forgeControllableTerminalSockets.map(({ url }) => new URL(url).pathname),
      ),
    )
    .toContain("/ws/v1/workspaces/ws-123/terminal");

  await page.getByRole("button", { name: "Terminal options" }).click();
  await expect(page.getByRole("button", { name: /Stop terminal/ })).toHaveCount(0);
});

test("phone keeps the durable workspace terminal available when runtime discovery fails", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  await setupTerminalMocks(page);
  await page.route("**/api/v1/workspaces/ws-123/runtime", async (route) => {
    await route.fulfill({
      status: 503,
      contentType: "application/problem+json",
      body: JSON.stringify(problem("serviceUnavailable", 503, "runtime discovery unavailable")),
    });
  });

  await page.goto("/m/workspaces/local/ws-123");

  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Workspace");
  await expect(page.locator(".mobile-workspace-terminal__stage")).toBeVisible();
  await expect(page.getByText("Runtime sessions unavailable")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry runtime sessions" })).toBeVisible();
});

test("phone dismisses stop confirmation when the selected session generation changes", async ({ page }) => {
  await page.clock.install();
  await page.setViewportSize({ width: 390, height: 844 });
  const runtimeEvents: RuntimeEvents = { launches: [], renames: [], deletes: [] };
  const mocked = await setupTerminalMocks(page, {
    runtimeEvents,
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces/local/ws-123");
  await page.getByRole("button", { name: "Terminal options" }).click();
  await page.getByRole("button", { name: "Stop terminal Codex" }).click();
  await expect(page.getByRole("dialog", { name: "Stop terminal?" })).toBeVisible();

  mocked.runtime.sessions = mocked.runtime.sessions.map((session) => ({
    ...session,
    created_at: "2026-04-10T12:05:00Z",
  }));
  await page.clock.fastForward(5_100);

  await expect(page.getByRole("dialog", { name: "Stop terminal?" })).toHaveCount(0);
  expect(runtimeEvents.deletes).toEqual([]);
});

test("phone applies a launched session before the runtime reconcile finishes", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const mocked = await setupTerminalMocks(page);
  let runtimeReads = 0;
  let releaseReconcile: () => void = () => {};
  const reconcilePending = new Promise<void>((resolve) => {
    releaseReconcile = resolve;
  });
  await page.route(
    (url) => url.pathname === "/api/v1/workspaces/ws-123/runtime",
    async (route) => {
      if (route.request().method() !== "GET") {
        await route.fallback();
        return;
      }
      runtimeReads += 1;
      if (runtimeReads > 2) await reconcilePending;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(mocked.runtime),
      });
    },
  );

  try {
    await page.goto("/m/workspaces/local/ws-123");
    await page.getByRole("button", { name: "Terminal options" }).click();
    await page.getByRole("button", { name: "New terminal" }).click();
    await page.getByRole("dialog", { name: "Launch workspace session" }).getByRole("button", { name: "Codex" }).click();

    await expect.poll(() => runtimeReads).toBe(3);
    await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Codex");
  } finally {
    releaseReconcile();
  }
});

test("phone removes a stopped session before the runtime reconcile finishes", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const mocked = await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });
  let runtimeReads = 0;
  let releaseReconcile: () => void = () => {};
  const reconcilePending = new Promise<void>((resolve) => {
    releaseReconcile = resolve;
  });
  await page.route(
    (url) => url.pathname === "/api/v1/workspaces/ws-123/runtime",
    async (route) => {
      if (route.request().method() !== "GET") {
        await route.fallback();
        return;
      }
      runtimeReads += 1;
      if (runtimeReads > 1) await reconcilePending;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(mocked.runtime),
      });
    },
  );

  try {
    await page.goto("/m/workspaces/local/ws-123");
    await page.getByRole("button", { name: "Terminal options" }).click();
    await page.getByRole("button", { name: "Stop terminal Codex" }).click();
    await page.getByRole("dialog", { name: "Stop terminal?" }).getByRole("button", { name: "Stop terminal" }).click();

    await expect.poll(() => runtimeReads).toBe(2);
    await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Workspace");
  } finally {
    releaseReconcile();
  }
});

test("phone runtime mutation failures remain visible with an existing session", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });
  await page.route(
    (url) => url.pathname.startsWith("/api/v1/workspaces/ws-123/runtime/sessions"),
    async (route) => {
      const message = route.request().method() === "POST" ? "launch service unavailable" : "stop service unavailable";
      await route.fulfill({
        status: 500,
        contentType: "application/problem+json",
        body: JSON.stringify(problem("internalError", 500, message)),
      });
    },
  );

  await page.goto("/m/workspaces/local/ws-123");
  await page.getByRole("button", { name: "Terminal options" }).click();
  await page.getByRole("button", { name: "New terminal" }).click();
  await page
    .getByRole("dialog", { name: "Launch workspace session" })
    .getByRole("button")
    .filter({ hasText: "system" })
    .click();
  const launchFailure = page.locator(".kit-flash-banner").filter({ hasText: /launch/i });
  await expect(launchFailure).toBeVisible();
  await expect(launchFailure).toHaveAttribute("data-kit-tone", "danger");

  await page.getByRole("button", { name: "Close launch session" }).click();
  await page.getByRole("button", { name: "Terminal options" }).click();
  await page.getByRole("button", { name: "Stop terminal Codex" }).click();
  await page.getByRole("dialog", { name: "Stop terminal?" }).getByRole("button", { name: "Stop terminal" }).click();
  const stopFailure = page.locator(".kit-flash-banner").filter({ hasText: /stop/i });
  await expect(stopFailure).toBeVisible();
  await expect(stopFailure).toHaveAttribute("data-kit-tone", "danger");
});

test("phone terminal sheets reserve global keyboard shortcuts", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces/local/ws-123");
  await page.getByRole("button", { name: "Terminal options" }).click();
  await page.keyboard.press("Meta+K");
  await expect(page.getByRole("dialog", { name: "Command palette" })).toHaveCount(0);

  await page.getByRole("button", { name: "New terminal" }).click();
  await page.keyboard.press("Meta+K");
  await expect(page.getByRole("dialog", { name: "Command palette" })).toHaveCount(0);
});

for (const [status, heading] of [
  ["deleting", "Deleting workspace…"],
  ["deletion_failed", "Workspace deletion failed"],
] as const) {
  test(`phone labels the ${status} workspace state accurately`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await setupTerminalMocks(page, {
      workspace: { ...testWorkspace, status } as WorkspaceFixture,
    });

    await page.goto("/m/workspaces/local/ws-123");

    await expect(page.locator(".mobile-workspace-terminal__state strong")).toHaveText(heading);
  });
}

test("phone terminal clears a draft before falling back to another session", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  const mocked = await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
        {
          key: "ws-123:plain_shell",
          workspace_id: "ws-123",
          target_key: "plain_shell",
          label: "Shell",
          kind: "shell",
          status: "running",
          created_at: "2026-04-10T12:01:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces/local/ws-123");
  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Codex");
  await page.getByRole("button", { name: "Open terminal composer" }).click();
  await page.getByRole("textbox", { name: "Terminal command" }).fill("do not send elsewhere");

  mocked.runtime.sessions = mocked.runtime.sessions.filter(({ key }) => key !== "ws-123:codex");
  await emitTerminalControl(page, "ws-123:codex", { type: "exited", code: 0 });

  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Shell");
  await expect(page.getByRole("textbox", { name: "Terminal command" })).toHaveCount(0);
  await page.getByRole("button", { name: "Open terminal composer" }).click();
  await expect(page.getByRole("textbox", { name: "Terminal command" })).toHaveValue("");
});

test("phone terminal clears a draft when the selected session is relaunched with the same key", async ({ page }) => {
  await page.clock.install();
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  const mocked = await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces/local/ws-123");
  await page.getByRole("button", { name: "Open terminal composer" }).click();
  await page.getByRole("textbox", { name: "Terminal command" }).fill("belongs to the old session");

  mocked.runtime.sessions = mocked.runtime.sessions.map((session) => ({
    ...session,
    created_at: "2026-04-10T12:05:00Z",
  }));
  await page.clock.fastForward(5_100);

  await expect(page.getByRole("textbox", { name: "Terminal command" })).toHaveCount(0);
  await page.getByRole("button", { name: "Open terminal composer" }).click();
  await expect(page.getByRole("textbox", { name: "Terminal command" })).toHaveValue("");
});

test("phone workspace list opens a linked item and returns to the list", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);

  await page.goto("/m/workspaces");
  await page.getByRole("button", { name: "Open linked item #42" }).click();

  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123\/item$/);
  await expect(page.getByRole("button", { name: "Back to workspaces" })).toBeVisible();
  await page.getByRole("button", { name: "Back to workspaces" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("phone item routes do not start terminal runtime work without a terminal origin", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  const runtimeRequests: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" && url.pathname.endsWith("/runtime")) runtimeRequests.push(url.pathname);
  });
  await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces");
  await page.getByRole("button", { name: "Open linked item #42" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123\/item$/);
  await expect(page.locator(".mobile-workspace-item .pull-detail .detail-title")).toBeVisible();

  await page.goto("/m/workspaces/local/ws-123/item");
  await expect(page.locator(".mobile-workspace-item .pull-detail .detail-title")).toBeVisible();
  expect(runtimeRequests).toEqual([]);
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __kenn_forgeControllableTerminalSockets: Array<{ url: string }>;
            }
          ).__kenn_forgeControllableTerminalSockets.filter(({ url }) => url.includes("/ws/v1/workspaces/")).length,
      ),
    )
    .toBe(0);
});

test("phone pull request workspace action stays in the mobile workspace workflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await installLinkedItemWorkspaceDetail(page, "pull_request", testWorkspace);

  await page.goto("/m/workspaces");
  await page.getByRole("button", { name: "Open linked item #42" }).click();
  await page.getByRole("button", { name: "Open Workspace" }).click();

  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123$/);
  await page.getByRole("button", { name: "Back to workspaces" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("phone list item tabs open a workspace that returns to the list", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await installLinkedItemWorkspaceDetail(page, "pull_request", testWorkspace);

  await page.goto("/m/workspaces");
  await page.getByRole("button", { name: "Open linked item #42" }).click();
  await page.getByRole("tab", { name: "Files changed" }).click();
  await page.getByRole("tab", { name: "Conversation" }).click();
  await page.getByRole("button", { name: "Open Workspace" }).click();

  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123$/);
  await page.getByRole("button", { name: "Back to workspaces" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("phone issue workspace action returns to its mobile terminal", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page, { workspace: testIssueWorkspace });
  await installLinkedItemWorkspaceDetail(page, "issue", testIssueWorkspace);

  await page.goto("/m/workspaces/local/ws-issue-7/item");
  await page.getByRole("button", { name: "Open Workspace" }).click();

  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-issue-7$/);
  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveText("Workspace");
});

test("direct phone workspace item tabs return to the workspace terminal", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);

  await page.goto("/m/workspaces");
  await page.goto("/m/workspaces/local/ws-123/item");
  await page.getByRole("tab", { name: "Files changed" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123\/item\/files$/);

  await page.getByRole("button", { name: "Back to workspace terminal" }).click();
  await expect(page).toHaveURL(/\/m\/workspaces\/local\/ws-123$/);

  await page.evaluate(() => history.back());
  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("phone Fleet workspace keeps its linked item as passive metadata", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await page.route("**/api/v1/fleet/hosts/member/workspaces/ws-123", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(testWorkspace),
    });
  });
  await page.route("**/api/v1/fleet/hosts/member/workspaces/ws-123/runtime", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(workspaceRuntime),
    });
  });

  await page.goto("/m/workspaces/fleet/member/ws-123");

  await expect(page.getByRole("combobox", { name: /Terminal session/ })).toHaveCount(0);
  await expect(page.getByText("No terminal sessions")).toBeVisible();
  await expect(page.locator(".mobile-workspace-terminal__item")).toContainText("#42");
  await expect(page.getByRole("button", { name: "Open linked PR #42" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Linked PR #42 unavailable for Fleet workspace" })).toBeDisabled();
});

test("phone workspace setup failure retries through the shared workflow", async ({ page }) => {
  let retryCalls = 0;
  let releaseRetry: () => void = () => {};
  const retryPending = new Promise<void>((resolve) => {
    releaseRetry = resolve;
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page, {
    workspace: {
      ...testWorkspace,
      status: "error",
      error_message: "tmux bootstrap failed",
    },
    workspaceRetryResponse: {
      status: 202,
      body: { ...testWorkspace, status: "creating" },
    },
  });
  await page.route("**/api/v1/workspaces/ws-123/retry", async (route) => {
    retryCalls += 1;
    await retryPending;
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({ ...testWorkspace, status: "creating" }),
    });
  });

  await page.goto("/m/workspaces/local/ws-123");
  const state = page.locator(".mobile-workspace-terminal__state");
  await expect(state).toContainText("tmux bootstrap failed");
  await state.getByRole("button", { name: "Retry" }).click();

  await expect.poll(() => retryCalls).toBe(1);
  await expect(state.getByRole("button", { name: "Retry" })).toBeDisabled();
  releaseRetry();
  await expect(state).toContainText("Setting up workspace…");
});

test("typed workspace deletion returns the phone workflow to the workspace list", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installControllableTerminalWebSockets(page);
  await page.addInitScript(() => {
    const instances: Array<{
      listeners: Map<string, Set<(event: MessageEvent) => void>>;
    }> = [];

    class FakeEventSource {
      listeners = new Map<string, Set<(event: MessageEvent) => void>>();

      constructor() {
        instances.push(this);
      }

      addEventListener(type: string, callback: (event: MessageEvent) => void): void {
        const bucket = this.listeners.get(type) ?? new Set();
        bucket.add(callback);
        this.listeners.set(type, bucket);
      }

      close(): void {}
    }

    (window as typeof window & { EventSource: typeof EventSource }).EventSource =
      FakeEventSource as unknown as typeof EventSource;
    (
      window as typeof window & {
        __emitWorkspaceDeleted: (payload: Record<string, unknown>) => void;
      }
    ).__emitWorkspaceDeleted = (payload) => {
      const event = new MessageEvent("workspace_deleted", { data: JSON.stringify(payload) });
      for (const instance of instances) {
        for (const listener of instance.listeners.get("workspace_deleted") ?? []) listener(event);
      }
    };
  });
  await setupTerminalMocks(page, {
    runtime: {
      ...workspaceRuntime,
      sessions: [
        {
          key: "ws-123:codex",
          workspace_id: "ws-123",
          target_key: "codex",
          label: "Codex",
          kind: "agent",
          status: "running",
          created_at: "2026-04-10T12:00:00Z",
        },
        {
          key: "ws-123:plain_shell",
          workspace_id: "ws-123",
          target_key: "plain_shell",
          label: "Shell",
          kind: "shell",
          status: "running",
          created_at: "2026-04-10T12:01:00Z",
        },
      ],
    },
  });

  await page.goto("/m/workspaces/local/ws-123");
  await expect(page.locator(".session-host-wrapper")).toHaveCount(3);
  await page.evaluate(() => {
    (
      window as typeof window & {
        __emitWorkspaceDeleted: (payload: Record<string, unknown>) => void;
      }
    ).__emitWorkspaceDeleted({
      workspace_id: "ws-123",
      provider: "github",
      platform_host: "github.com",
      owner: "acme",
      name: "widgets",
      repo_path: "acme/widgets",
      number: 42,
      item_type: "pull_request",
    });
  });

  await expect(page).toHaveURL(/\/m\/workspaces$/);
  await expect(page.locator(".session-host-wrapper")).toHaveCount(0);
});

test("missing phone workspace runtime returns to the workspace list", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await page.route("**/api/v1/workspaces/ws-123/runtime", async (route) => {
    await route.fulfill({
      status: 404,
      contentType: "application/problem+json",
      body: JSON.stringify({ status: 404, code: "workspaceNotFound", detail: "workspace not found" }),
    });
  });

  await page.goto("/m/workspaces/local/ws-123");

  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("missing phone workspace item returns to the workspace list", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await page.route(
    (url) => url.pathname === "/api/v1/workspaces/ws-123",
    async (route) => {
      await route.fulfill({
        status: 404,
        contentType: "application/problem+json",
        body: JSON.stringify({ status: 404, code: "workspaceNotFound", detail: "workspace not found" }),
      });
    },
  );

  await page.goto("/m/workspaces/local/ws-123/item");

  await expect(page).toHaveURL(/\/m\/workspaces$/);
});

test("unavailable Fleet runtime stays in context with Reconnect", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setupTerminalMocks(page);
  await page.route("**/api/v1/fleet/hosts/member/workspaces/ws-123", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(testWorkspace),
    });
  });
  await page.route("**/api/v1/fleet/hosts/member/workspaces/ws-123/runtime", async (route) => {
    await route.fulfill({
      status: 404,
      contentType: "application/problem+json",
      body: JSON.stringify({ status: 404, code: "notFound", detail: "Fleet host unavailable" }),
    });
  });

  await page.goto("/m/workspaces/fleet/member/ws-123");

  await expect(page).toHaveURL(/\/m\/workspaces\/fleet\/member\/ws-123$/);
  await expect(page.getByText("Terminal runtime unavailable")).toBeVisible();
  await expect(page.getByRole("button", { name: "Reconnect" })).toBeVisible();
});

test.describe("terminal state icons", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  });

  test("creating workspace shows spinner icon", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: {
        ...testWorkspace,
        status: "creating",
      },
    });

    await page.goto("/terminal/ws-123");

    const stateMessage = page.locator(".state-message");
    await expect(stateMessage).toContainText("Setting up workspace...");
    await expect(stateMessage.locator(".kit-spinner")).toBeVisible();
  });

  test("refresh preserves workspace diff target and commit selection", async ({ page }) => {
    const diffRequests: string[] = [];
    const commitRequests: string[] = [];
    await setupTerminalMocks(page, { diffRequests, commitRequests });

    await page.goto("/terminal/ws-123");
    await page.locator(".panel-toggle-btn", { hasText: "Diff" }).click();

    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "head" })).toBe(true);

    await page.getByRole("button", { name: "Compare with merge target" }).click();
    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target" })).toBe(true);

    await page.getByRole("button", { name: /Select commit range/ }).click();
    await page.getByRole("button", { name: /second commit/ }).click();
    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target", commit: "sha2" })).toBe(true);

    diffRequests.length = 0;
    commitRequests.length = 0;
    await page.getByRole("button", { name: "Refresh workspace details" }).click();

    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target", commit: "sha2" })).toBe(true);
    await expect.poll(() => commitRequests.length).toBeGreaterThan(0);
    await expect(page.getByRole("button", { name: "Compare with merge target" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  test("refresh clears workspace diff commit selection when the commit disappears", async ({ page }) => {
    const diffRequests: string[] = [];
    const commitRequests: string[] = [];
    await setupTerminalMocks(page, {
      diffRequests,
      commitRequests,
      workspaceCommitResponses: [
        [
          {
            sha: "sha2",
            message: "second commit",
            author_name: "Alice",
            authored_at: "2026-01-01T00:00:00Z",
          },
        ],
        [
          {
            sha: "sha3",
            message: "third commit",
            author_name: "Alice",
            authored_at: "2026-01-02T00:00:00Z",
          },
        ],
      ],
    });

    await page.goto("/terminal/ws-123");
    await page.locator(".panel-toggle-btn", { hasText: "Diff" }).click();

    await page.getByRole("button", { name: "Compare with merge target" }).click();
    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target" })).toBe(true);

    const scopeTrigger = page.getByRole("button", { name: /Select commit range/ });
    await scopeTrigger.click();
    await page.getByRole("button", { name: /second commit/ }).click();
    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target", commit: "sha2" })).toBe(true);
    await expect(scopeTrigger).toHaveAccessibleName(/Select commit range: sha2/);

    diffRequests.length = 0;
    commitRequests.length = 0;
    await page.getByRole("button", { name: "Refresh workspace details" }).click();

    await expect.poll(() => commitRequests.length).toBeGreaterThan(0);
    await expect.poll(() => hasWorkspaceDiffRequest(diffRequests, { base: "merge-target", commit: null })).toBe(true);
    expect(diffRequests.some((requestURL) => new URL(requestURL).searchParams.get("commit") === "sha2")).toBe(false);
    await expect(scopeTrigger).toHaveAccessibleName(/Select commit range: HEAD/);
    await expect(page.getByRole("button", { name: "Compare with merge target" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  test("workspace load failure shows alert icon and retry recovers", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspaceDetailResponses: [
        {
          status: 500,
          body: problem("internalError", 500),
        },
        {
          status: 200,
          body: testWorkspace,
        },
      ],
    });

    await page.goto("/terminal/ws-123");

    const stateMessage = page.locator(".state-message.error");
    await expect(stateMessage).toContainText("Failed to load workspace (500)");
    await expect(stateMessage.getByLabel("Workspace load failed")).toBeVisible();

    await stateMessage.getByRole("button", { name: "Retry" }).click();

    await expect(page.locator(".header-name")).toContainText("Add auth middleware");
  });

  test("workspace setup error retries setup and recovers", async ({ page }) => {
    let retryCalls = 0;
    await setupTerminalMocks(page, {
      workspaceDetailResponses: [
        {
          status: 200,
          body: {
            ...testWorkspace,
            status: "error",
            error_message: "tmux bootstrap failed",
          },
        },
        {
          status: 200,
          body: testWorkspace,
        },
      ],
      workspaceRetryResponse: {
        status: 202,
        body: { ...testWorkspace, status: "creating" },
      },
    });
    await page.route("**/api/v1/workspaces/ws-123/retry", async (route) => {
      retryCalls += 1;
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify({
          ...testWorkspace,
          status: "creating",
        }),
      });
    });

    await page.goto("/terminal/ws-123");

    const stateMessage = page.locator(".state-message.error");
    await expect(stateMessage).toContainText("tmux bootstrap failed");
    await expect(stateMessage.getByLabel("Workspace setup failed")).toBeVisible();

    await stateMessage.getByRole("button", { name: "Retry" }).click();

    await expect.poll(() => retryCalls).toBe(1);
    await expect(page.locator(".header-name")).toContainText("Add auth middleware");
  });

  test("workspace setup error can be deleted", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspaceDetailResponses: [
        {
          status: 200,
          body: {
            ...testWorkspace,
            status: "error",
            error_message: "ensure clone failed",
          },
        },
      ],
    });

    await page.goto("/terminal/ws-123");

    const stateMessage = page.locator(".state-message.error");
    await expect(stateMessage).toContainText("ensure clone failed");

    await stateMessage.getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    await expect(page).toHaveURL(/\/workspaces$/);
  });

  test("force-delete prompt confirms and retries delete with force=true", async ({ page }) => {
    const deleteRequests: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "DELETE" && req.url().includes("/api/v1/workspaces/ws-123")) {
        deleteRequests.push(req.url());
      }
    });

    await setupTerminalMocks(page, {
      workspaceDeleteResponses: [
        {
          status: 409,
          body: problem("worktreeDirty", 409, "Worktree has uncommitted changes."),
        },
        { status: 204 },
      ],
    });

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    const dialog = page.getByRole("dialog", {
      name: "Force delete workspace?",
    });
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("Worktree has uncommitted changes.");

    await dialog.getByRole("button", { name: "Force delete" }).click();

    await expect(page).toHaveURL(/\/workspaces$/);
    expect(deleteRequests).toHaveLength(2);
    expect(deleteRequests[1]).toContain("force=true");
  });

  test("in-flight successful force DELETE does not yank the user off the route they chose", async ({ page }) => {
    await setupTerminalMocks(page);

    let deleteCount = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/workspaces/ws-123",
      async (route) => {
        if (route.request().method() !== "DELETE") {
          await route.fallback();
          return;
        }
        deleteCount += 1;
        if (deleteCount === 1) {
          await route.fulfill({
            status: 409,
            contentType: "application/json",
            body: JSON.stringify(problem("worktreeDirty", 409, "Worktree has uncommitted changes.")),
          });
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 400));
        await route.fulfill({ status: 204 });
      },
    );

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    const dialog = page.getByRole("dialog", {
      name: "Force delete workspace?",
    });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Force delete" }).click();

    await page.evaluate(() => {
      history.pushState(null, "", "/pulls");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    await page.waitForTimeout(700);

    await expect(page).toHaveURL(/\/pulls$/);
  });

  test("force-delete prompt cancel keeps the workspace and the modal closes", async ({ page }) => {
    const deleteRequests: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "DELETE" && req.url().includes("/api/v1/workspaces/ws-123")) {
        deleteRequests.push(req.url());
      }
    });

    await setupTerminalMocks(page, {
      workspaceDeleteResponses: [
        {
          status: 409,
          body: problem("worktreeDirty", 409, "Worktree has uncommitted changes."),
        },
      ],
    });

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    const dialog = page.getByRole("dialog", {
      name: "Force delete workspace?",
    });
    await expect(dialog).toBeVisible();

    await dialog.getByRole("button", { name: "Cancel" }).click();

    await expect(dialog).toBeHidden();
    await expect(page).not.toHaveURL(/\/workspaces$/);
    expect(deleteRequests).toHaveLength(1);
  });

  test("force-delete prompt traps focus, makes background inert, and restores focus on cancel", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspaceDeleteResponses: [
        {
          status: 409,
          body: problem("worktreeDirty", 409, "Worktree has uncommitted changes."),
        },
      ],
    });

    await page.goto("/terminal/ws-123");

    const headerDelete = page.locator(".header-bar").getByRole("button", { name: "Delete" });
    await headerDelete.click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    const dialog = page.getByRole("dialog", {
      name: "Force delete workspace?",
    });
    await expect(dialog).toBeVisible();

    const cancel = dialog.getByRole("button", { name: "Cancel" });
    const force = dialog.getByRole("button", {
      name: "Force delete",
    });

    // Initial focus lands on Cancel — the safe default for a destructive action.
    await expect(cancel).toBeFocused();

    // The workspace shell beneath the modal is inert and unreachable.
    await expect(page.locator(".terminal-view")).toHaveAttribute("inert", "");

    // Tab cycles within the dialog (Cancel -> Force delete -> Cancel).
    await page.keyboard.press("Tab");
    await expect(force).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(cancel).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(force).toBeFocused();

    // Closing the dialog restores focus to the trigger.
    await cancel.click();
    await expect(dialog).toBeHidden();
    await expect(page.locator(".terminal-view")).not.toHaveAttribute("inert", "");
    await expect(headerDelete).toBeFocused();
  });

  test("force-delete prompt is dismissed when the workspace route changes", async ({ page }) => {
    const deleteRequests: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "DELETE") {
        deleteRequests.push(req.url());
      }
    });

    await setupTerminalMocks(page, {
      workspaceDeleteResponses: [
        {
          status: 409,
          body: problem("worktreeDirty", 409, "Worktree has uncommitted changes."),
        },
      ],
    });

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    const dialog = page.getByRole("dialog", {
      name: "Force delete workspace?",
    });
    await expect(dialog).toBeVisible();

    // The component persists across workspaceId changes (no {#key} wrapper),
    // so the prompt would otherwise stay open after navigation and leak a
    // destructive confirmation onto the next workspace.
    await page.evaluate(() => {
      history.pushState(null, "", "/workspaces");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    await expect(dialog).toBeHidden();
    // Only the initial DELETE fired — no force-delete reached the server
    // after the route change dismissed the prompt.
    expect(deleteRequests).toHaveLength(1);
    expect(deleteRequests[0]).not.toContain("force=true");
  });

  test("in-flight 409 response does not surface a prompt after the user has navigated away", async ({ page }) => {
    await setupTerminalMocks(page);

    // Replace the default DELETE handler with one that holds the
    // 409 response long enough for the test to navigate before
    // it lands.
    await page.route(
      (url) => url.pathname === "/api/v1/workspaces/ws-123",
      async (route) => {
        if (route.request().method() !== "DELETE") {
          await route.fallback();
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 400));
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify(problem("worktreeDirty", 409, "Worktree has uncommitted changes.")),
        });
      },
    );

    await page.goto("/terminal/ws-123");

    // Kick off the DELETE, then immediately leave the workspace.
    // The DELETE handler in handleDelete is async, so this is the
    // exact race condition the post-await guard exists to handle.
    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();
    await page.evaluate(() => {
      history.pushState(null, "", "/workspaces");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    // Wait past the route delay so the response settles either way.
    await page.waitForTimeout(700);

    await expect(
      page.getByRole("dialog", {
        name: "Force delete workspace?",
      }),
    ).toBeHidden();
  });

  test("stale 409 does not reopen the prompt after the user leaves and returns to the same workspace", async ({
    page,
  }) => {
    await setupTerminalMocks(page);

    await page.route(
      (url) => url.pathname === "/api/v1/workspaces/ws-123",
      async (route) => {
        if (route.request().method() !== "DELETE") {
          await route.fallback();
          return;
        }
        // Hold the response long enough for the test to navigate
        // away and back before it lands.
        await new Promise((resolve) => setTimeout(resolve, 500));
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify(problem("worktreeDirty", 409, "Worktree has uncommitted changes.")),
        });
      },
    );

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    // A round-trip back to the same workspace would defeat an
    // id-only guard: the captured targetId matches the current
    // workspaceId, but the prompt should still not reappear
    // because the user has explicitly left and returned. Two
    // separate evaluate calls mirror real user clicks — each
    // pops an event-loop turn so Svelte's effects flush between
    // them and the generation counter advances.
    await page.evaluate(() => {
      history.pushState(null, "", "/workspaces");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await page.evaluate(() => {
      history.pushState(null, "", "/terminal/ws-123");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    await page.waitForTimeout(800);

    await expect(
      page.getByRole("dialog", {
        name: "Force delete workspace?",
      }),
    ).toBeHidden();
  });

  test("in-flight successful DELETE does not yank the user off the route they chose", async ({ page }) => {
    await setupTerminalMocks(page);

    await page.route(
      (url) => url.pathname === "/api/v1/workspaces/ws-123",
      async (route) => {
        if (route.request().method() !== "DELETE") {
          await route.fallback();
          return;
        }
        // Delay long enough for the user to navigate away
        // before the 204 lands.
        await new Promise((resolve) => setTimeout(resolve, 400));
        await route.fulfill({ status: 204 });
      },
    );

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    // Without the post-await guard in handleDelete, the stale
    // 204 would call navigate("/workspaces") and the user would
    // be yanked away from /pulls.
    await page.evaluate(() => {
      history.pushState(null, "", "/pulls");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    await page.waitForTimeout(700);

    await expect(page).toHaveURL(/\/pulls$/);
  });

  test("successful DELETE after A→B→A still navigates away from the deleted workspace", async ({ page }) => {
    await setupTerminalMocks(page);

    await page.route(
      (url) => url.pathname === "/api/v1/workspaces/ws-123",
      async (route) => {
        if (route.request().method() !== "DELETE") {
          await route.fallback();
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 500));
        await route.fulfill({ status: 204 });
      },
    );

    await page.goto("/terminal/ws-123");

    await page.locator(".header-bar").getByRole("button", { name: "Delete" }).click();
    await page
      .getByRole("dialog", { name: "Delete workspace?" })
      .getByRole("button", { name: "Delete workspace" })
      .click();

    // Round-trip back to the same workspace before the 204 lands.
    // The generation token has advanced, but the workspace the
    // user is looking at has just been destroyed on the server —
    // we must still navigate away rather than leave them staring
    // at a dead workspace.
    await page.evaluate(() => {
      history.pushState(null, "", "/workspaces");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await page.evaluate(() => {
      history.pushState(null, "", "/terminal/ws-123");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    await expect(page).toHaveURL(/\/workspaces$/);
  });
});

test.describe("workspace launch home", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-list-sidebar-width");
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
    await setupTerminalMocks(page);
  });

  test("shows Worktree Home and does not attach a terminal by default", async ({ page }) => {
    const terminalSockets: string[] = [];
    page.on("websocket", (socket) => {
      const url = socket.url();
      if (url.includes("/terminal")) {
        terminalSockets.push(url);
      }
    });

    await page.goto("/terminal/ws-123");

    await expect(page.getByRole("tab", { name: "Home" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Launch" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Codex" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Shell", exact: true })).toBeEnabled();
    await expect(
      page.getByRole("button", {
        name: "Open terminal panel",
      }),
    ).toBeVisible();
    await expect(page.getByText("Plain shell")).toHaveCount(0);
    await expect.poll(() => terminalSockets.length).toBe(0);
  });

  test("does not attach restored runtime sessions until selected", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: {
        ...workspaceRuntime,
        sessions: [
          {
            key: "ws-123:codex",
            workspace_id: "ws-123",
            target_key: "codex",
            label: "Codex",
            kind: "agent",
            status: "running",
            created_at: "2026-04-10T12:00:00Z",
          },
        ],
      },
    });

    await page.addInitScript(() => {
      const OriginalWebSocket = window.WebSocket;
      const urls: string[] = [];
      Object.defineProperty(window, "__kenn_forgeWebSocketUrls", {
        value: urls,
      });
      window.WebSocket = class extends OriginalWebSocket {
        constructor(url: string | URL, protocols?: string | string[]) {
          urls.push(String(url));
          if (protocols === undefined) {
            super(url);
          } else {
            super(url, protocols);
          }
        }
      };
    });

    await page.goto("/terminal/ws-123");

    const tabs = page.getByRole("region", { name: "Workflow panes" });
    await expect(tabs.getByRole("tab", { name: "Codex" })).toBeVisible();
    const initialTerminalSockets = await page.evaluate(() =>
      (
        (
          window as unknown as {
            __kenn_forgeWebSocketUrls: string[];
          }
        ).__kenn_forgeWebSocketUrls ?? []
      ).filter((url) => url.includes("/ws/v1/workspaces/ws-123/")),
    );
    expect(initialTerminalSockets).toEqual([]);

    await tabs.getByRole("tab", { name: "Codex" }).click();

    await expect(page.locator(".terminal-container")).toBeVisible();
    await expect
      .poll(async () => {
        const urls = await page.evaluate(() =>
          (
            (
              window as unknown as {
                __kenn_forgeWebSocketUrls: string[];
              }
            ).__kenn_forgeWebSocketUrls ?? []
          ).filter((url) => url.includes("/ws/v1/workspaces/ws-123/")),
        );
        return urls.some((url) => url.includes("/runtime/sessions/ws-123%3Acodex/terminal"));
      })
      .toBe(true);
  });

  test("selects the top-docked terminal when moving an inactive workflow tab into it", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: {
        ...workspaceRuntime,
        sessions: [
          {
            key: "ws-123:codex",
            workspace_id: "ws-123",
            target_key: "codex",
            label: "Codex",
            kind: "agent",
            status: "running",
            created_at: "2026-04-10T12:00:00Z",
          },
        ],
      },
    });
    await page.addInitScript((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-123", "home");
    }, topDockedTerminalWorkflowLayout());

    await page.goto("/terminal/ws-123");

    const workflow = page.getByRole("region", {
      name: "Workflow panes",
    });
    await expect(workflow.getByRole("tab", { name: "Home" })).toHaveAttribute("aria-selected", "true");
    await expect(workflow.getByRole("tab", { name: "Terminal" })).toHaveAttribute("aria-selected", "false");

    await workflow.getByRole("button", { name: "Move Codex to terminal" }).click();

    await expect(workflow.getByRole("tab", { name: "Terminal" })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator(".tabbed-panel-leaf .terminal-container")).toBeVisible();
  });

  test("keeps a closed top-docked terminal reachable when terminal sessions exist", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: {
        ...workspaceRuntime,
        sessions: [
          {
            key: "ws-123:plain_shell",
            workspace_id: "ws-123",
            target_key: "plain_shell",
            label: "Shell",
            kind: "plain_shell",
            status: "running",
            created_at: "2026-04-10T12:00:00Z",
          },
        ],
      },
    });
    await page.addInitScript((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-123", "home");
    }, closedTopDockedTerminalWorkflowLayout());

    await page.goto("/terminal/ws-123");

    const workflow = page.getByRole("region", {
      name: "Workflow panes",
    });
    const terminalTab = workflow.getByRole("tab", {
      name: "Terminal",
    });
    await expect(terminalTab).toBeVisible();
    await expect(terminalTab).toHaveAttribute("aria-selected", "false");
    await expect(page.locator(".tabbed-panel-leaf .terminal-container")).toHaveCount(0);

    await terminalTab.click();

    await expect(terminalTab).toHaveAttribute("aria-selected", "true");
    await expect(page.locator(".tabbed-panel-leaf .terminal-container")).toBeVisible();
  });

  test("applies a workflow preset that restores the Shell workflow tab", async ({ page }) => {
    await page.addInitScript((preset) => {
      localStorage.removeItem("kenn-forge-workspace-terminal-layout:ws-123");
      localStorage.setItem("kenn-forge-workspace-layout-presets", JSON.stringify([preset]));
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-123", "home");
    }, shellWorkflowPreset());
    await setupTerminalMocks(page);

    await page.goto("/terminal/ws-123");

    await page.getByRole("button", { name: "Workflow presets" }).click();
    await page
      .getByRole("dialog", { name: "Workflow presets" })
      .getByRole("button", { name: "Shell focus", exact: true })
      .click();

    const workflow = page.getByRole("region", {
      name: "Workflow panes",
    });
    await expect(workflow.getByRole("tab", { name: "Shell" })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator(".tabbed-panel-leaf .terminal-container")).toBeVisible();
  });

  test("renders workflow panes flush with the workspace stage", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: workflowDragRuntime(),
    });
    await page.addInitScript((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
    }, workflowDragLayout());

    await page.goto("/terminal/ws-123");
    await expect(page.locator(".tabbed-panel-split")).toBeVisible();

    const metrics = await page.evaluate(() => {
      const titleBar = document.querySelector(".header-bar");
      const toolbar = document.querySelector(".workspace-toolbar");
      const stage = document.querySelector(".workspace-stage");
      const split = document.querySelector(".workspace-stage .tabbed-panel-split");
      const firstLeaf = document.querySelector(".workspace-stage .tabbed-panel-leaf");
      if (!titleBar || !toolbar || !stage || !split || !firstLeaf) {
        throw new Error("Missing workflow header, stage, or split panel");
      }

      const titleBarRect = titleBar.getBoundingClientRect();
      const toolbarRect = toolbar.getBoundingClientRect();
      const stageRect = stage.getBoundingClientRect();
      const splitRect = split.getBoundingClientRect();
      const firstLeafRect = firstLeaf.getBoundingClientRect();
      const titleBarStyles = getComputedStyle(titleBar);
      const toolbarStyles = getComputedStyle(toolbar);
      const stageStyles = getComputedStyle(stage);

      return {
        titleBorderLeft: titleBarStyles.borderLeftWidth,
        toolbarBorderLeft: toolbarStyles.borderLeftWidth,
        titleToToolbarLeft: toolbarRect.left - titleBarRect.left,
        toolbarToFirstLeafLeft: firstLeafRect.left - toolbarRect.left,
        padding: [stageStyles.paddingTop, stageStyles.paddingRight, stageStyles.paddingBottom, stageStyles.paddingLeft],
        delta: {
          left: splitRect.left - stageRect.left,
          top: splitRect.top - stageRect.top,
          right: stageRect.right - splitRect.right,
          bottom: stageRect.bottom - splitRect.bottom,
        },
      };
    });

    expect(metrics.titleBorderLeft).toBe("1px");
    expect(metrics.toolbarBorderLeft).toBe("1px");
    expect(Math.abs(metrics.titleToToolbarLeft)).toBeLessThanOrEqual(0.5);
    expect(Math.abs(metrics.toolbarToFirstLeafLeft)).toBeLessThanOrEqual(0.5);
    expect(metrics.padding).toEqual(["0px", "0px", "0px", "0px"]);
    expect(Math.abs(metrics.delta.left)).toBeLessThanOrEqual(0.5);
    expect(Math.abs(metrics.delta.top)).toBeLessThanOrEqual(0.5);
    expect(Math.abs(metrics.delta.right)).toBeLessThanOrEqual(0.5);
    expect(Math.abs(metrics.delta.bottom)).toBeLessThanOrEqual(0.5);
  });

  test("moves active focus between standalone workflow panes", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: workflowDragRuntime(),
    });
    await page.addInitScript((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
      localStorage.setItem("kenn-forge-workspace-active-tab:ws-123", "session:ws-123:codex");
    }, workflowDragLayout());

    await page.goto("/terminal/ws-123");
    const codexPane = page.locator('[data-pane-key="session:ws-123:codex"]');
    const reviewerPane = page.locator('[data-pane-key="session:ws-123:reviewer"]');
    const codexLeaf = codexPane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");
    const reviewerLeaf = reviewerPane.locator("xpath=ancestor::*[contains(@class, 'tabbed-panel-leaf')][1]");

    await expect(page.locator(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveCount(0);
    await codexLeaf.getByRole("tab", { name: /^Codex/ }).focus();
    await expect(codexLeaf).toHaveClass(/input-active/);
    const activeBorder = await codexLeaf.evaluate((leaf) => ({
      overlayContent: getComputedStyle(leaf, "::after").content,
      insetShadow: getComputedStyle(leaf).boxShadow,
    }));
    expect(activeBorder.overlayContent).toBe("none");
    expect(activeBorder.insetShadow).toContain("inset");

    // The inset focus shadow paints beneath descendants, and a hosted terminal
    // fills its pane with opaque layers. The marker survives only because the
    // leaf reserves the outer 1px of its padding box, so no child may reach it.
    const ringClearance = await codexLeaf.evaluate((leaf) => {
      const rect = leaf.getBoundingClientRect();
      const style = getComputedStyle(leaf);
      const inner = {
        left: rect.left + parseFloat(style.borderLeftWidth) + 1,
        top: rect.top + parseFloat(style.borderTopWidth) + 1,
        right: rect.right - parseFloat(style.borderRightWidth) - 1,
        bottom: rect.bottom - parseFloat(style.borderBottomWidth) - 1,
      };
      const epsilon = 0.01;
      return [...leaf.children].map((child) => {
        const box = child.getBoundingClientRect();
        return {
          className: child.className,
          insideRing:
            box.width === 0 ||
            box.height === 0 ||
            (box.left >= inner.left - epsilon &&
              box.top >= inner.top - epsilon &&
              box.right <= inner.right + epsilon &&
              box.bottom <= inner.bottom + epsilon),
        };
      });
    });
    for (const child of ringClearance) {
      expect(child.insideRing, `${child.className} must not cover the focus ring`).toBe(true);
    }

    await reviewerLeaf.getByRole("tab", { name: /^Reviewer/ }).focus();
    await expect(reviewerLeaf).toHaveClass(/input-active/);
    await expect(codexLeaf).not.toHaveClass(/input-active/);

    await codexPane.hover();
    await page.mouse.wheel(0, 120);
    await expect(reviewerLeaf).toHaveClass(/input-active/);
    await expect(codexLeaf).not.toHaveClass(/input-active/);

    await page.locator(".panel-toggle-btn", { hasText: "Diff" }).click();
    const rightSidebar = page.locator(".right-sidebar");
    await expect(rightSidebar).toBeVisible();
    await rightSidebar.locator(".kit-scrollbox__viewport").focus();
    await expect(rightSidebar).toHaveClass(/input-active/);
    await expect(page.locator(".workspace-stage .tabbed-panel-leaf.input-active")).toHaveCount(0);

    await codexLeaf.getByRole("tab", { name: /^Codex/ }).focus();
    await expect(codexLeaf).toHaveClass(/input-active/);
    await expect(rightSidebar).not.toHaveClass(/input-active/);
  });

  test("shows one Workspaces option in the compact page menu on terminal routes", async ({ page }) => {
    await page.setViewportSize({ width: 1000, height: 720 });
    await setupTerminalMocks(page, {
      runtime: workflowDragRuntime(),
    });

    await page.goto("/terminal/ws-123");

    const pageSelect = page.getByRole("combobox", {
      name: "Page: Workspaces",
    });
    await expect(pageSelect).toBeVisible();
    await pageSelect.click();

    const workspaceOption = page.getByRole("option", {
      name: "Workspaces",
    });
    await expect(workspaceOption).toHaveCount(1);
    await expect(workspaceOption).toHaveAttribute("aria-selected", "true");
  });

  test("workflow pane drops append in the center and split at the edge", async ({ page }) => {
    await setupTerminalMocks(page, {
      runtime: workflowDragRuntime(),
    });
    await page.addInitScript((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
    }, workflowDragLayout());

    await page.goto("/terminal/ws-123");

    await expect(page.locator(".tabbed-panel-leaf")).toHaveCount(2);
    await dragWorkflowTabToGroup(page, "Reviewer", 0, "center");
    await expect(page.locator(".tabbed-panel-leaf")).toHaveCount(1);
    await expect(
      page
        .locator(".tabbed-panel-leaf")
        .first()
        .getByRole("tab", { name: /Reviewer/ }),
    ).toBeVisible();

    await page.evaluate((layout) => {
      localStorage.setItem("kenn-forge-workspace-terminal-layout:ws-123", JSON.stringify(layout));
    }, workflowDragLayout());
    await page.reload();

    await expect(page.locator(".tabbed-panel-leaf")).toHaveCount(2);
    await dragWorkflowTabToGroup(page, "Reviewer", 0, "left-edge");
    await expect(page.locator(".tabbed-panel-leaf")).toHaveCount(2);
    await expect(
      page
        .locator(".tabbed-panel-leaf")
        .first()
        .getByRole("tab", { name: /Reviewer/ }),
    ).toBeVisible();
  });

  test("saves updates applies and deletes workflow presets", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-terminal-layout:ws-123");
      localStorage.removeItem("kenn-forge-workspace-layout-presets");
    });
    const runtimeEvents: RuntimeEvents = {
      launches: [],
      renames: [],
      deletes: [],
    };
    const mocked = await setupTerminalMocks(page, {
      runtimeEvents,
    });

    await page.goto("/terminal/ws-123");
    await page.getByRole("button", { name: "Codex" }).click();
    await expect(page.getByRole("tab", { name: "Codex" })).toBeVisible();
    await page
      .getByRole("button", {
        name: "Open terminal panel",
      })
      .click();
    await expect(page.locator(".terminal-panel.open .terminal-container")).toBeVisible();

    await page.getByRole("button", { name: "Rename Codex" }).click();
    const renameDialog = page.getByRole("dialog", {
      name: "Rename tab",
    });
    await renameDialog.getByLabel("Name").fill("Reviewer");
    await renameDialog.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("tab", { name: "Reviewer" })).toBeVisible();

    let presetPromptMessage = "";
    page.once("dialog", async (dialog) => {
      presetPromptMessage = dialog.message();
      await dialog.accept("Review pair");
    });
    await page.getByRole("button", { name: "Workflow presets" }).click();
    await page
      .getByRole("dialog", { name: "Workflow presets" })
      .getByRole("button", { name: "Save as preset" })
      .click();
    expect(presetPromptMessage).toBe("Preset name");
    await expect
      .poll(() =>
        page.evaluate(() => {
          const raw = localStorage.getItem("kenn-forge-workspace-layout-presets");
          const presets = raw ? JSON.parse(raw) : [];
          return presets.map((preset: { name: string }) => preset.name);
        }),
      )
      .toEqual(["Review pair"]);

    await page.getByRole("button", { name: "Rename Reviewer" }).click();
    await renameDialog.getByLabel("Name").fill("Navigator");
    await renameDialog.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("tab", { name: "Navigator" })).toBeVisible();

    await page.getByRole("button", { name: "Workflow presets" }).click();
    const presetDialog = page.getByRole("dialog", {
      name: "Workflow presets",
    });
    await expect(
      presetDialog.getByRole("button", {
        name: "Update selected",
      }),
    ).toBeEnabled();
    await presetDialog.getByRole("button", { name: "Update selected" }).click();
    await expect
      .poll(() =>
        page.evaluate(() => {
          const raw = localStorage.getItem("kenn-forge-workspace-layout-presets");
          const presets = raw ? JSON.parse(raw) : [];
          return presets[0]?.sessions.find((session: { targetKey: string }) => session.targetKey === "codex")?.label;
        }),
      )
      .toBe("Navigator");

    await page.locator('.terminal-panel .panel-action[aria-label="Close terminal panel"]').click();
    await expect(page.locator(".terminal-panel.open")).toHaveCount(0);
    mocked.runtime.sessions = [];
    runtimeEvents.launches = [];
    runtimeEvents.renames = [];
    runtimeEvents.deletes = [];

    await page.getByRole("button", { name: "Workflow presets" }).click();
    await page
      .getByRole("dialog", { name: "Workflow presets" })
      .getByRole("button", { name: "Review pair", exact: true })
      .click();
    await expect.poll(() => runtimeEvents.launches).toEqual(["codex", "plain_shell"]);
    await expect
      .poll(() => runtimeEvents.renames)
      .toEqual([
        {
          sessionKey: "ws-123:codex",
          label: "Navigator",
        },
      ]);
    await expect(page.getByRole("tab", { name: "Navigator" })).toBeVisible();
    await expect(page.locator(".terminal-panel.open .terminal-container")).toBeVisible();
    await expect
      .poll(() =>
        page.evaluate(() => {
          const raw = localStorage.getItem("kenn-forge-workspace-terminal-layout:ws-123");
          const layout = raw ? JSON.parse(raw) : null;
          return {
            open: layout?.open,
            codexRegion: layout?.sessionRegions?.["ws-123:codex"],
            shellRegion: layout?.sessionRegions?.["ws-123:plain_shell"],
          };
        }),
      )
      .toEqual({
        open: true,
        codexRegion: "workflow",
        shellRegion: "terminal",
      });

    await page.getByRole("button", { name: "Workflow presets" }).click();
    await page
      .getByRole("dialog", { name: "Workflow presets" })
      .getByRole("button", { name: "Delete Review pair" })
      .click();
    await expect
      .poll(() =>
        page.evaluate(() => {
          const raw = localStorage.getItem("kenn-forge-workspace-layout-presets");
          return raw ? JSON.parse(raw).length : 0;
        }),
      )
      .toBe(0);
    await expect(page.getByRole("dialog", { name: "Workflow presets" }).getByText("No presets saved")).toBeVisible();
  });

  test("launches an agent into a compact running tab", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    await page.getByRole("button", { name: "Codex" }).click();

    const tabs = page.getByRole("region", { name: "Workflow panes" });
    await expect(tabs.getByRole("tab", { name: "Codex" })).toBeVisible();
    await expect(page.locator(".terminal-container")).toBeVisible();
  });

  test("xterm workspace terminal sends resize frames after viewport changes", async ({ page }) => {
    test.setTimeout(60_000);
    await page.addInitScript(() => {
      type RecordedSocket = {
        receiveReplayReady: () => void;
        sent: unknown[];
        url: string;
      };
      const recordedSockets: RecordedSocket[] = [];
      Object.defineProperty(window, "__kenn_forgeRecordedTerminalSockets", {
        value: recordedSockets,
      });
      const NativeWebSocket = window.WebSocket;

      class MockTerminalWebSocket extends EventTarget {
        static CONNECTING = 0;
        static OPEN = 1;
        static CLOSING = 2;
        static CLOSED = 3;

        binaryType = "arraybuffer";
        extensions = "";
        onclose: ((event: CloseEvent) => void) | null = null;
        onerror: ((event: Event) => void) | null = null;
        onmessage: ((event: MessageEvent) => void) | null = null;
        onopen: ((event: Event) => void) | null = null;
        protocol = "";
        readyState = MockTerminalWebSocket.OPEN;
        readonly url: string;
        private readonly record?: RecordedSocket;

        constructor(url: string | URL, protocols?: string | string[]) {
          super();
          this.url = String(url);
          if (!this.url.includes("/ws/v1/workspaces/")) {
            return new NativeWebSocket(url, protocols);
          }
          this.record = {
            receiveReplayReady: () => {
              const replayReady = new MessageEvent("message", {
                data: JSON.stringify({ type: "replay_ready" }),
              });
              this.dispatchEvent(replayReady);
              this.onmessage?.(replayReady);
            },
            url: this.url,
            sent: [],
          };
          recordedSockets.push(this.record);
          queueMicrotask(() => {
            const event = new Event("open");
            this.dispatchEvent(event);
            this.onopen?.(event);
          });
        }

        close(): void {
          this.readyState = MockTerminalWebSocket.CLOSED;
          const event = new CloseEvent("close");
          this.dispatchEvent(event);
          this.onclose?.(event);
        }

        send(data: unknown): void {
          if (!this.record) return;
          if (typeof data === "string") {
            this.record.sent.push(data);
            return;
          }
          this.record.sent.push("[binary]");
        }
      }

      window.WebSocket = MockTerminalWebSocket as unknown as typeof WebSocket;
    });

    await page.goto("/terminal/ws-123", {
      waitUntil: "domcontentloaded",
    });
    const launchTarget = page.getByRole("button", { name: "Codex" });
    await expect(launchTarget).toBeVisible();
    await launchTarget.click();

    await expect(page.locator(".terminal-container .xterm")).toBeVisible();
    await page.evaluate(() => {
      for (const socket of (
        window as unknown as {
          __kenn_forgeRecordedTerminalSockets: Array<{
            receiveReplayReady: () => void;
            sent: unknown[];
          }>;
        }
      ).__kenn_forgeRecordedTerminalSockets) {
        socket.receiveReplayReady();
      }
    });
    await expect
      .poll(async () =>
        page.evaluate(() =>
          (
            window as unknown as {
              __kenn_forgeRecordedTerminalSockets: Array<{
                sent: unknown[];
              }>;
            }
          ).__kenn_forgeRecordedTerminalSockets.some((socket) =>
            socket.sent.some((frame) => {
              if (typeof frame !== "string") return false;
              try {
                return JSON.parse(frame).type === "claim_resize";
              } catch {
                return false;
              }
            }),
          ),
        ),
      )
      .toBe(true);
    await page.evaluate(() => {
      for (const socket of (
        window as unknown as {
          __kenn_forgeRecordedTerminalSockets: Array<{
            sent: unknown[];
          }>;
        }
      ).__kenn_forgeRecordedTerminalSockets) {
        socket.sent = [];
      }
    });

    await page.setViewportSize({ width: 900, height: 700 });
    await page.setViewportSize({ width: 1200, height: 800 });

    await expect
      .poll(async () =>
        page.evaluate(() =>
          (
            window as unknown as {
              __kenn_forgeRecordedTerminalSockets: Array<{
                sent: unknown[];
              }>;
            }
          ).__kenn_forgeRecordedTerminalSockets.some((socket) =>
            socket.sent.some((frame) => {
              if (typeof frame !== "string") return false;
              try {
                const type = JSON.parse(frame).type;
                return type === "resize" || type === "claim_resize";
              } catch {
                return false;
              }
            }),
          ),
        ),
      )
      .toBe(true);
  });

  test("xterm uses stable rendering for colored terminal diff output in Firefox", async ({ page, browserName }) => {
    test.skip(browserName !== "firefox", "Firefox-specific xterm rendering regression coverage");
    await page.addInitScript(() => {
      Object.defineProperty(window, "__kenn_forgeOpenedTerminalSockets", {
        value: [] as string[],
      });
      const NativeWebSocket = window.WebSocket;
      const diffOutput = [
        "\x1b[31m--- a/internal/db/queries.go\x1b[0m\r\n",
        "\x1b[41m\x1b[37m-        Number: 701,\x1b[0m\r\n",
        '\x1b[41m\x1b[37m-        URL: "https://github.com/acme/widget/pull/701",\x1b[0m\r\n',
        '\x1b[41m\x1b[37m-        Title: "Sort PR workspace",\x1b[0m\r\n',
      ].join("");

      class MockTerminalWebSocket extends EventTarget {
        static CONNECTING = 0;
        static OPEN = 1;
        static CLOSING = 2;
        static CLOSED = 3;

        binaryType = "arraybuffer";
        extensions = "";
        onclose: ((event: CloseEvent) => void) | null = null;
        onerror: ((event: Event) => void) | null = null;
        onmessage: ((event: MessageEvent) => void) | null = null;
        onopen: ((event: Event) => void) | null = null;
        protocol = "";
        readyState = MockTerminalWebSocket.OPEN;
        readonly url: string;

        constructor(url: string | URL, protocols?: string | string[]) {
          super();
          this.url = String(url);
          if (!this.url.includes("/ws/v1/workspaces/")) {
            return new NativeWebSocket(url, protocols);
          }
          queueMicrotask(() => {
            (
              window as unknown as { __kenn_forgeOpenedTerminalSockets: string[] }
            ).__kenn_forgeOpenedTerminalSockets.push(this.url);
            const event = new Event("open");
            this.dispatchEvent(event);
            this.onopen?.(event);
            const message = new MessageEvent("message", {
              data: new TextEncoder().encode(diffOutput).buffer,
            });
            this.dispatchEvent(message);
            this.onmessage?.(message);
          });
        }

        close(): void {
          this.readyState = MockTerminalWebSocket.CLOSED;
          const event = new CloseEvent("close");
          this.dispatchEvent(event);
          this.onclose?.(event);
        }

        send(): void {
          // The rendering assertion only needs inbound terminal output.
        }
      }

      window.WebSocket = MockTerminalWebSocket as unknown as typeof WebSocket;
    });

    await page.goto("/terminal/ws-123", {
      waitUntil: "domcontentloaded",
    });
    await page.getByRole("button", { name: "Codex" }).click();

    await expect(page.locator(".terminal-container .xterm")).toBeVisible();
    await expect
      .poll(() =>
        page.evaluate(() =>
          (window as unknown as { __kenn_forgeOpenedTerminalSockets: string[] }).__kenn_forgeOpenedTerminalSockets.some(
            (url) => url.includes("/runtime/sessions/ws-123%3Acodex/terminal"),
          ),
        ),
      )
      .toBe(true);
    await expect(page.locator(".terminal-container .xterm-screen canvas")).toHaveCount(0);
  });

  test("xterm workspace terminal normalizes Windows multiline paste as one payload", async ({ page }) => {
    await page.addInitScript(() => {
      type RecordedSocket = {
        sent: unknown[];
        url: string;
      };
      const recordedSockets: RecordedSocket[] = [];
      Object.defineProperty(window, "__kenn_forgeRecordedTerminalSockets", {
        value: recordedSockets,
      });
      const NativeWebSocket = window.WebSocket;

      class MockTerminalWebSocket extends EventTarget {
        static CONNECTING = 0;
        static OPEN = 1;
        static CLOSING = 2;
        static CLOSED = 3;

        binaryType = "arraybuffer";
        extensions = "";
        onclose: ((event: CloseEvent) => void) | null = null;
        onerror: ((event: Event) => void) | null = null;
        onmessage: ((event: MessageEvent) => void) | null = null;
        onopen: ((event: Event) => void) | null = null;
        protocol = "";
        readyState = MockTerminalWebSocket.OPEN;
        readonly url: string;
        private readonly record?: RecordedSocket;

        constructor(url: string | URL, protocols?: string | string[]) {
          super();
          this.url = String(url);
          if (!this.url.includes("/ws/v1/workspaces/")) {
            return new NativeWebSocket(url, protocols);
          }
          this.record = { url: this.url, sent: [] };
          recordedSockets.push(this.record);
          queueMicrotask(() => {
            const event = new Event("open");
            this.dispatchEvent(event);
            this.onopen?.(event);
          });
        }

        close(): void {
          this.readyState = MockTerminalWebSocket.CLOSED;
          const event = new CloseEvent("close");
          this.dispatchEvent(event);
          this.onclose?.(event);
        }

        send(data: unknown): void {
          if (!this.record) return;
          if (data instanceof ArrayBuffer) {
            this.record.sent.push(Array.from(new Uint8Array(data)));
            return;
          }
          if (ArrayBuffer.isView(data)) {
            this.record.sent.push(Array.from(new Uint8Array(data.buffer, data.byteOffset, data.byteLength)));
            return;
          }
          this.record.sent.push(data);
        }
      }

      window.WebSocket = MockTerminalWebSocket as unknown as typeof WebSocket;
    });

    await page.goto("/terminal/ws-123", {
      waitUntil: "domcontentloaded",
    });
    const launchTarget = page.getByRole("button", { name: "Codex" });
    await expect(launchTarget).toBeVisible();
    await launchTarget.click();

    await expect(page.locator(".terminal-container .xterm")).toBeVisible();
    await page.evaluate(() => {
      for (const socket of (
        window as unknown as {
          __kenn_forgeRecordedTerminalSockets: Array<{
            sent: unknown[];
          }>;
        }
      ).__kenn_forgeRecordedTerminalSockets) {
        socket.sent = [];
      }
    });

    const terminal = page.locator(".terminal-container").first();
    await terminal.evaluate((element) => {
      const event = new Event("paste", {
        bubbles: true,
        cancelable: true,
      }) as ClipboardEvent;
      Object.defineProperty(event, "clipboardData", {
        value: {
          getData: (type: string) => (type === "text/plain" ? "first\r\nsecond\r\nthird" : ""),
        },
      });
      element.dispatchEvent(event);
    });

    await expect
      .poll(async () =>
        page.evaluate(() => {
          const decoder = new TextDecoder();
          return (
            window as unknown as {
              __kenn_forgeRecordedTerminalSockets: Array<{
                sent: unknown[];
              }>;
            }
          ).__kenn_forgeRecordedTerminalSockets
            .flatMap((socket) => socket.sent)
            .map((frame) => (Array.isArray(frame) ? decoder.decode(new Uint8Array(frame)) : frame));
        }),
      )
      .toContainEqual("first\rsecond\rthird");
  });

  test("opens the plain shell from the bottom terminal panel", async ({ page }) => {
    const terminalSockets: string[] = [];
    page.on("websocket", (socket) => {
      terminalSockets.push(socket.url());
    });

    await page.goto("/terminal/ws-123");
    await page
      .getByRole("button", {
        name: "Open terminal panel",
      })
      .click();

    await expect(page.locator(".terminal-panel.open .terminal-container")).toBeVisible();
    const dividerMetrics = await page.evaluate(() => {
      const panel = document.querySelector(".terminal-panel.bottom.open");
      const resizer = document.querySelector(".terminal-panel.bottom.open .panel-resizer");
      const stage = document.querySelector(".workspace-stage");
      if (!panel || !resizer || !stage) {
        throw new Error("Missing bottom dock panel, resizer, or workspace stage");
      }

      const panelRect = panel.getBoundingClientRect();
      const resizerRect = resizer.getBoundingClientRect();
      const stageRect = stage.getBoundingClientRect();
      const panelStyles = getComputedStyle(panel);
      const resizerStyles = getComputedStyle(resizer);
      const stripeStyles = getComputedStyle(resizer, "::before");
      return {
        panelBorderTop: panelStyles.borderTopWidth,
        resizerCursor: resizerStyles.cursor,
        resizerHeight: Math.round(resizerRect.height),
        stageToPanel: Math.round(panelRect.top - stageRect.bottom),
        stripeHeight: stripeStyles.height,
      };
    });
    expect(dividerMetrics).toEqual({
      panelBorderTop: "0px",
      resizerCursor: "row-resize",
      resizerHeight: 4,
      stageToPanel: 0,
      stripeHeight: "3px",
    });
    await expect
      .poll(() => terminalSockets.some((url) => url.includes("/runtime/sessions/ws-123%3Aplain_shell/terminal")))
      .toBe(true);
  });

  test("restarts an exited shell session when opening the terminal panel", async ({ page }) => {
    const shellEnsures: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && request.url().includes("/runtime/sessions")) {
        shellEnsures.push(request.url());
      }
    });

    await setupTerminalMocks(page, {
      runtime: {
        ...workspaceRuntime,
        sessions: [
          {
            key: "ws-123:plain_shell",
            workspace_id: "ws-123",
            target_key: "plain_shell",
            label: "Plain shell",
            kind: "plain_shell",
            status: "exited",
            created_at: "2026-04-10T12:00:00Z",
          },
        ],
      },
    });

    await page.goto("/terminal/ws-123");
    await page
      .getByRole("button", {
        name: "Open terminal panel",
      })
      .click();

    await expect.poll(() => shellEnsures.length).toBe(1);
    await expect(page.locator(".terminal-panel.open .terminal-container")).toBeVisible();
  });

  test("renders bottom terminal split panes flush with consistent dividers", async ({ page }) => {
    const splitTree = {
      type: "split",
      id: "terminal-split-root",
      direction: "horizontal",
      ratio: 0.5,
      first: {
        type: "leaf",
        id: "terminal-left",
        sessionKey: "ws-123:plain_shell",
      },
      second: {
        type: "leaf",
        id: "terminal-right",
        sessionKey: "ws-123:shell_2",
      },
    };
    await setupTerminalMocks(page, {
      runtime: {
        ...workspaceRuntime,
        sessions: [
          {
            key: "ws-123:plain_shell",
            workspace_id: "ws-123",
            target_key: "plain_shell",
            label: "Shell",
            kind: "plain_shell",
            status: "running",
            created_at: "2026-04-10T12:00:00Z",
          },
          {
            key: "ws-123:shell_2",
            workspace_id: "ws-123",
            target_key: "plain_shell",
            label: "Shell 2",
            kind: "plain_shell",
            status: "running",
            created_at: "2026-04-10T12:00:00Z",
          },
        ],
      },
    });
    await page.addInitScript((tree) => {
      localStorage.setItem(
        "kenn-forge-workspace-terminal-layout:ws-123",
        JSON.stringify({
          version: 1,
          open: true,
          dock: "bottom",
          height: 300,
          activeSessionKey: "ws-123:shell_2",
          tree,
          terminalGroups: [
            {
              id: "terminal-group",
              activeSessionKey: "ws-123:shell_2",
              tree,
            },
          ],
          activeTerminalGroupID: "terminal-group",
          sessionRegions: {
            "ws-123:plain_shell": "terminal",
            "ws-123:shell_2": "terminal",
          },
          workflowMode: "tabs",
          workflowTree: {
            type: "leaf",
            id: "workflow-root",
            tabs: ["home"],
            activeTabKey: "home",
          },
          activeWorkflowLeafID: "workflow-root",
          recentWorkflowLeafIDs: ["workflow-root"],
          customSessionLabels: {},
        }),
      );
    }, splitTree);

    await page.goto("/terminal/ws-123");
    await expect(page.locator(".terminal-panel.open .terminal-leaf")).toHaveCount(2);
    await expect(page.locator(".terminal-panel.open .xterm-viewport")).toHaveCount(2);

    const splitMetrics = await page.evaluate(() => {
      const body = document.querySelector(".terminal-panel.bottom.open .panel-body");
      const tree = document.querySelector(".terminal-panel.bottom.open .terminal-tree");
      const selector = document.querySelector(".terminal-panel.bottom.open .terminal-selector");
      const split = document.querySelector(".terminal-panel.bottom.open .terminal-split");
      const activeHeader = document.querySelector(".terminal-panel.bottom.open .terminal-leaf.active .leaf-header");
      const activeLeaf = activeHeader?.closest(".terminal-leaf");
      const firstLeaf = document.querySelector(".terminal-panel.bottom.open .terminal-leaf");
      const firstViewport = firstLeaf?.querySelector(".xterm-viewport");
      const secondLeaf = document.querySelector(
        ".terminal-panel.bottom.open .terminal-split.horizontal > .split-child.second > .terminal-leaf",
      );
      const divider = document.querySelector(".terminal-panel.bottom.open .terminal-split.horizontal > .split-divider");
      if (
        !body ||
        !tree ||
        !selector ||
        !split ||
        !activeHeader ||
        !activeLeaf ||
        !firstLeaf ||
        !firstViewport ||
        !secondLeaf ||
        !divider
      ) {
        throw new Error("Missing bottom terminal split layout");
      }

      const bodyRect = body.getBoundingClientRect();
      const treeRect = tree.getBoundingClientRect();
      const selectorRect = selector.getBoundingClientRect();
      const splitRect = split.getBoundingClientRect();
      const firstLeafRect = firstLeaf.getBoundingClientRect();
      const firstViewportRect = firstViewport.getBoundingClientRect();
      const dividerRect = divider.getBoundingClientRect();
      const treeStyles = getComputedStyle(tree);
      const firstLeafStyles = getComputedStyle(firstLeaf);
      const firstViewportStyles = getComputedStyle(firstViewport);
      const secondLeafStyles = getComputedStyle(secondLeaf);
      const dividerStyles = getComputedStyle(divider);
      const activeLeafStyles = getComputedStyle(activeLeaf);
      const activeHeaderStyles = getComputedStyle(activeHeader);
      return {
        activeHeaderBoxShadow: activeHeaderStyles.boxShadow,
        activeLeftBorderUsesAccent: activeLeafStyles.borderLeftColor === "rgb(37, 99, 235)",
        activeLeafBorderTopWidth: activeLeafStyles.borderTopWidth,
        firstLeafBorderRight: firstLeafStyles.borderRightWidth,
        firstLeafToSplitLeft: Math.round(firstLeafRect.left - splitRect.left),
        firstLeafToSplitTop: Math.round(firstLeafRect.top - splitRect.top),
        firstViewportBackground: firstViewportStyles.backgroundColor,
        firstViewportToLeafRight: Math.round(firstLeafRect.right - firstViewportRect.right),
        secondLeafBorderLeft: secondLeafStyles.borderLeftWidth,
        splitToTreeBottom: Math.round(treeRect.bottom - splitRect.bottom),
        splitToTreeLeft: Math.round(splitRect.left - treeRect.left),
        splitToTreeTop: Math.round(splitRect.top - treeRect.top),
        splitterBackgroundVisible: dividerStyles.backgroundColor !== "rgba(0, 0, 0, 0)",
        splitterHitWidth: Math.round(dividerRect.width),
        treePadding: [treeStyles.paddingTop, treeStyles.paddingRight, treeStyles.paddingBottom, treeStyles.paddingLeft],
        treeToBodyLeft: Math.round(treeRect.left - bodyRect.left),
        treeToBodyTop: Math.round(treeRect.top - bodyRect.top),
        treeToSelector: Math.round(selectorRect.left - treeRect.right),
      };
    });
    expect(splitMetrics).toEqual({
      activeHeaderBoxShadow: "rgb(37, 99, 235) 0px 2px 0px 0px inset",
      activeLeafBorderTopWidth: "0px",
      activeLeftBorderUsesAccent: false,
      firstLeafBorderRight: "0px",
      firstLeafToSplitLeft: 0,
      firstLeafToSplitTop: 0,
      firstViewportBackground: "rgb(13, 17, 23)",
      firstViewportToLeafRight: 0,
      secondLeafBorderLeft: "0px",
      splitToTreeBottom: 0,
      splitToTreeLeft: 0,
      splitToTreeTop: 0,
      splitterBackgroundVisible: true,
      splitterHitWidth: 3,
      treePadding: ["0px", "0px", "0px", "0px"],
      treeToBodyLeft: 0,
      treeToBodyTop: 0,
      treeToSelector: 0,
    });

    const readHeaderMetrics = () =>
      page.evaluate(() =>
        Array.from(document.querySelectorAll(".terminal-panel.bottom.open .terminal-leaf")).map((leaf) => {
          const header = leaf.querySelector(".leaf-header");
          const label = leaf.querySelector(".leaf-label")?.textContent ?? "";
          if (!header) {
            throw new Error("Missing terminal leaf header");
          }
          const headerRect = header.getBoundingClientRect();
          const leafRect = leaf.getBoundingClientRect();
          return {
            active: leaf.classList.contains("active"),
            headerBoxShadow: getComputedStyle(header).boxShadow,
            headerHeight: Math.round(headerRect.height),
            headerTop: Math.round(headerRect.top),
            label,
            leafTop: Math.round(leafRect.top),
          };
        }),
      );
    const beforeSwitch = await readHeaderMetrics();
    const inactiveTitle = page.locator(".terminal-panel.bottom.open .terminal-leaf:not(.active) .leaf-title");
    await expect(inactiveTitle).toHaveCount(1);
    await inactiveTitle.click();
    const afterSwitch = await readHeaderMetrics();
    expect(
      afterSwitch.map(({ headerHeight, headerTop, label, leafTop }) => ({
        headerHeight,
        headerTop,
        label,
        leafTop,
      })),
    ).toEqual(
      beforeSwitch.map(({ headerHeight, headerTop, label, leafTop }) => ({
        headerHeight,
        headerTop,
        label,
        leafTop,
      })),
    );
    expect(afterSwitch).toMatchObject([
      {
        active: true,
        headerBoxShadow: "rgb(37, 99, 235) 0px 2px 0px 0px inset",
        label: "Shell",
      },
      {
        active: false,
        headerBoxShadow: "none",
        label: "Shell 2",
      },
    ]);
  });
});

// -------------------------------------------------------
// Group 1: Toggle Behavior
// -------------------------------------------------------

test.describe("sidebar toggle behavior", () => {
  test.beforeEach(async ({ page }) => {
    // Clear any persisted sidebar state before each test.
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-list-sidebar-width");
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
    await setupTerminalMocks(page);
  });

  test("workspace row shows working indicator with activity source", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: {
        ...testWorkspace,
        tmux_pane_title: "⠴ t3code-b5014b03",
        tmux_working: true,
        tmux_activity_source: "title",
      },
    });

    await page.goto("/terminal/ws-123");

    const row = page.locator(".ws-row", {
      hasText: "Add auth middleware",
    });
    const pulse = row.getByLabel("Working (title): ⠴ t3code-b5014b03");
    await expect(pulse).toBeVisible();
    await expect(pulse).toHaveAttribute("title", "Working (title): ⠴ t3code-b5014b03");
    await expect(pulse).toHaveAttribute("aria-label", "Working (title): ⠴ t3code-b5014b03");
  });

  test("workspace list polls while mounted", async ({ page }) => {
    await setupTerminalMocks(page);
    let listRequests = 0;
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        listRequests += 1;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            workspaces: [testWorkspace],
          }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });

    await page.goto("/terminal/ws-123");

    await expect.poll(() => listRequests).toBeGreaterThanOrEqual(1);
    await expect.poll(() => listRequests, { timeout: 6500 }).toBeGreaterThanOrEqual(2);
  });

  test("workspace list resize reclamps the right sidebar", async ({ page }) => {
    await page.setViewportSize({ width: 980, height: 720 });
    await page.goto("/terminal/ws-123");

    const listSidebar = page.locator(".workspace-list-sidebar");
    await expect(listSidebar).toBeVisible();

    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    await prBtn.click();
    const rightSidebar = page.locator(".right-sidebar");
    await expect(rightSidebar).toBeVisible();

    const initialListWidth = await listSidebar.evaluate((el) => el.getBoundingClientRect().width);
    const initialRightSidebarWidth = await rightSidebar.evaluate((el) => el.getBoundingClientRect().width);

    const handle = page.getByRole("separator", {
      name: "Resize sidebar",
    });
    await expect(handle).toBeVisible();
    await handle.focus();
    for (let i = 0; i < 8; i += 1) {
      await page.keyboard.press("ArrowRight");
    }

    await expect
      .poll(async () => rightSidebar.evaluate((el) => el.getBoundingClientRect().width))
      .toBeLessThan(initialRightSidebarWidth - 20);

    const resizedListWidth = await listSidebar.evaluate((el) => el.getBoundingClientRect().width);
    expect(resizedListWidth).toBeGreaterThan(initialListWidth + 40);

    const terminalWidth = await page.locator(".terminal-area").evaluate((el) => el.getBoundingClientRect().width);
    expect(terminalWidth).toBeGreaterThanOrEqual(300);
  });

  test("sidebar toggle group visible in terminal header", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    const segControl = page.locator(".panel-toggle-group");
    await expect(segControl).toBeVisible();
    await expect(segControl.locator(".panel-toggle-btn", { hasText: "PR" })).toBeVisible();
    await expect(
      segControl.locator(".panel-toggle-btn", {
        hasText: "Reviews",
      }),
    ).toBeVisible();
  });

  test("clicking the PR toggle opens sidebar with PR content", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    await expect(prBtn).toBeVisible();
    await prBtn.click();

    // Sidebar should now be visible
    await expect(page.locator(".right-sidebar")).toBeVisible();
    const workflowPanelMetrics = await page.evaluate(() => {
      const handle = document.querySelector(".sidebar-resize-handle");
      const tabPanel = document.querySelector(".workspace-stage .tabbed-panel-tab-panel.active");
      const workspaceHome = document.querySelector(".workspace-stage .workspace-home");
      if (!handle || !tabPanel || !workspaceHome) {
        throw new Error("Missing handle, active panel, or workspace home");
      }

      const handleRect = handle.getBoundingClientRect();
      const panelRect = tabPanel.getBoundingClientRect();
      const homeRect = workspaceHome.getBoundingClientRect();
      const panelStyles = getComputedStyle(tabPanel);
      return {
        handleWidth: Math.round(handleRect.width),
        homeToPanelRight: Math.round(panelRect.right - homeRect.right),
        homeToSplitter: Math.round(handleRect.left - homeRect.right),
        panelOverflowY: panelStyles.overflowY,
      };
    });
    expect(workflowPanelMetrics).toEqual({
      handleWidth: 4,
      homeToPanelRight: 0,
      // Leaf border plus the 1px ring the leaf reserves for the pane focus
      // marker (see TabbedPanelTree); pane content sits inside both.
      homeToSplitter: 2,
      panelOverflowY: "hidden",
    });
    // PR button should be active
    await expect(prBtn).toHaveClass(/active/);
  });

  test("merge modal overlays the bottom terminal splitter", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    await page
      .getByRole("button", {
        name: "Open terminal panel",
      })
      .click();
    await expect(page.locator(".terminal-panel.bottom.open .panel-resizer")).toBeVisible();

    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    await prBtn.click();

    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Add browser regression coverage");

    await page.locator(".right-sidebar .btn--merge").first().click();
    await expect(
      page.locator(".right-sidebar .kit-modal-title", {
        hasText: "Merge Pull Request",
      }),
    ).toBeVisible();

    const topElementIsModal = await page.evaluate(() => {
      const resizer = document.querySelector(".terminal-panel.bottom.open .panel-resizer");
      if (!(resizer instanceof HTMLElement)) {
        throw new Error("Missing bottom terminal panel resizer");
      }
      const rect = resizer.getBoundingClientRect();
      const topElement = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
      return topElement instanceof HTMLElement && topElement.closest(".kit-modal-overlay") !== null;
    });

    expect(topElementIsModal).toBe(true);
  });

  test("clicking the active toggle closes the sidebar", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    // Open
    await prBtn.click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Click the same toggle again — should close
    await prBtn.click();
    await expect(page.locator(".right-sidebar")).toHaveCount(0);
    await expect(prBtn).not.toHaveClass(/active/);
  });

  // The review drawer's own footer geometry is covered in the browser lane
  // (RoborevReviewDrawer.footer-layout.browser.svelte.ts). What only the real
  // app can answer is whether this pane actually hands the footer that width:
  // .right-sidebar clips its overflow, so an action past its edge is
  // unreachable rather than merely clipped-looking. Width comes from seeded
  // localStorage because loadSidebarWidth() clamps up to MIN_SIDEBAR_WIDTH,
  // which pins the exact production floor without a flaky pointer drag.
  test("review actions stay reachable in a minimum-width sidebar", async ({ page }) => {
    // A running job that already has a review is the widest the footer gets:
    // Close Review, Rerun, Cancel, and Copy Output all present at once. That
    // is the set the narrow sidebar has to survive.
    await setupTerminalMocks(page, {
      roborevJobs: {
        ...roborevJobs,
        jobs: [{ ...roborevJobs.jobs[0]!, status: "running", verdict: "" }],
      },
    });
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-workspace-sidebar-width", "0");
    });
    await page.goto("/terminal/ws-123");

    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    const sidebar = page.locator(".right-sidebar");
    await expect(sidebar).toBeVisible();
    expect((await sidebar.boundingBox())!.width).toBe(280);

    // Open the drawer on the seeded job so the footer renders.
    await sidebar.locator(".job-row").first().click();
    // FitStages keeps hidden measurement copies of the row; the first group in
    // document order is the stage actually on screen.
    const actions = sidebar.locator("[aria-label='Review actions']").first().locator("button");
    await expect(actions.first()).toBeVisible();

    // Pin the whole set, not just "some buttons exist": the point of seeding a
    // running job with a review is that all four are present, so losing one
    // must fail here rather than quietly shrink what the geometry covers. The
    // icon stage carries its names on aria-label.
    await expect
      .poll(async () =>
        Promise.all(
          (await actions.all()).map(
            async (a) => (await a.getAttribute("aria-label")) ?? (await a.textContent())!.trim(),
          ),
        ),
      )
      .toEqual(["Close Review", "Rerun", "Cancel", "Copy Output"]);

    const sidebarBox = (await sidebar.boundingBox())!;
    const count = await actions.count();
    for (let i = 0; i < count; i++) {
      const action = actions.nth(i);
      await expect(action).toBeVisible();
      const box = (await action.boundingBox())!;
      expect(box.width).toBeGreaterThan(0);
      expect(box.x).toBeGreaterThanOrEqual(sidebarBox.x - 0.5);
      expect(box.x + box.width).toBeLessThanOrEqual(sidebarBox.x + sidebarBox.width + 0.5);
    }

    // Reachable, not merely within bounds: a clipped control is not hittable.
    await actions.last().click({ trial: true });
  });

  test("clicking Reviews switches tab without closing", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    const reviewsBtn = page.locator(".panel-toggle-btn", {
      hasText: "Reviews",
    });

    // Open PR tab
    await prBtn.click();
    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(prBtn).toHaveClass(/active/);

    // Switch to Reviews
    await reviewsBtn.click();
    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(reviewsBtn).toHaveClass(/active/);
    await expect(prBtn).not.toHaveClass(/active/);
  });

  test("Cmd+] toggles sidebar open and closed", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    // Start closed
    await expect(page.locator(".right-sidebar")).toHaveCount(0);

    // Open via keyboard
    await page.keyboard.press("Meta+]");
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Close via keyboard
    await page.keyboard.press("Meta+]");
    await expect(page.locator(".right-sidebar")).toHaveCount(0);
  });

  test("Cmd+] leaves the workspace sidebar closed while the command palette owns focus", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    await page.keyboard.press("Meta+K");
    const palette = page.getByRole("dialog", { name: "Command palette" });
    await expect(palette).toBeVisible();
    await expect(palette.getByRole("textbox", { name: "Search command palette" })).toBeFocused();

    await page.keyboard.press("Meta+]");

    await expect(page.locator(".right-sidebar")).toHaveCount(0);
    await expect(palette.getByRole("textbox", { name: "Search command palette" })).toBeFocused();
  });

  test("Cmd+] leaves the workspace sidebar closed while application chrome owns focus", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    const settings = page.getByRole("button", { name: "Settings" });
    await settings.focus();
    await expect(settings).toBeFocused();

    await page.keyboard.press("Meta+]");

    await expect(page.locator(".right-sidebar")).toHaveCount(0);
    await expect(settings).toBeFocused();
  });

  test("closing focused workspace details returns focus to the workspace", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    await page.locator(".panel-toggle-btn", { hasText: "PR" }).click();

    const details = page.getByRole("region", { name: "Workspace details pane" });
    const detailsViewport = details.locator(".kit-scrollbox__viewport").first();
    await detailsViewport.focus();
    await expect(detailsViewport).toBeFocused();

    await page.keyboard.press("Meta+]");

    await expect(details).toHaveCount(0);
    await expect(page.locator(".terminal-view")).toBeFocused();
  });

  test("moving the focused bottom terminal into Workflow returns focus to the workspace", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    await page.getByRole("button", { name: "Open terminal panel" }).click();

    const moveToWorkflow = page.getByRole("button", { name: "Move terminal panel to workflow" });
    await moveToWorkflow.focus();
    await expect(moveToWorkflow).toBeFocused();
    await moveToWorkflow.click();

    await expect(page.locator(".terminal-panel.bottom")).toHaveCount(0);
    await expect(page.getByRole("tab", { name: "Terminal" })).toBeVisible();
    await expect(page.locator(".terminal-view")).toBeFocused();
  });
});

test.describe("workspace list fleet inventory", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-list-sidebar-width");
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
  });

  test("shows remote workspaces from reachable fleet peers", async ({ page }) => {
    const remoteHostKey = "a8f1c287d6be4fd9988d067e76d2554e";
    const remoteHostName = "Build spoke";
    const remoteWorkspace = {
      ...testIssueWorkspace,
      id: "member-ws-23",
      item_number: 23,
      git_head_ref: "kenn-forge/issue-23-federation-test",
      worktree_path: "/data/member/worktrees/github.com/kenn-io/kit/issue-23",
      mr_title: "Member workspace",
      repo_owner: "kenn-io",
      repo_name: "kit",
      repo: workspaceRepoRef("kenn-io", "kit"),
      fleet_host_key: remoteHostKey,
      fleet_host_name: remoteHostName,
    };

    await page.route(
      (url) => url.pathname === "/api/v1/snapshot",
      async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            hosts: [
              {
                configKey: "hub",
                diagnostics: [],
                id: "hub",
                kind: "self",
                name: "hub",
                operationAvailability: {
                  workspaceRead: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
              {
                configKey: remoteHostKey,
                diagnostics: [],
                id: remoteHostKey,
                kind: "remote",
                name: remoteHostName,
                operationAvailability: {
                  workspaceRead: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [remoteWorkspace],
          }),
        });
      },
    );
    await page.route(`**/api/v1/fleet/hosts/${remoteHostKey}/workspaces/member-ws-23`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(remoteWorkspace),
      });
    });
    await page.route(`**/api/v1/fleet/hosts/${remoteHostKey}/workspaces/member-ws-23/runtime`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          launch_targets: [],
          sessions: [],
        }),
      });
    });

    await page.goto("/workspaces");

    const sidebar = page.locator(".workspace-list-sidebar");
    await expect(sidebar).not.toContainText("Fleet");
    await expect(sidebar).not.toContainText(remoteHostKey);

    const row = sidebar.locator(".ws-row", { hasText: "Member workspace" });
    await expect(row).toBeVisible();
    await expect(row).not.toContainText(remoteHostName);
    await row.click();

    await expect(page).toHaveURL(new RegExp(`/terminal/fleet/${remoteHostKey}/member-ws-23$`));
    await expect(page.locator(".workspace-home")).toContainText("Member workspace");
  });

  test("hides singleton self fleet host status", async ({ page }) => {
    await page.route(
      (url) => url.pathname === "/api/v1/snapshot",
      async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            hosts: [
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "self",
                name: "member",
                operationAvailability: {
                  workspaceRead: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "local",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [],
          }),
        });
      },
    );

    await page.goto("/workspaces");

    const sidebar = page.locator(".workspace-list-sidebar");
    await expect(sidebar).toBeVisible();
    await expect(sidebar).not.toContainText("Fleet");
    await expect(sidebar).not.toContainText("1/1");
    await expect(sidebar).not.toContainText("member");
  });
});

// -------------------------------------------------------
// Group 2: Persistence
// -------------------------------------------------------

test.describe("sidebar persistence", () => {
  // Persistence tests reload the page, so we must NOT
  // use addInitScript (it re-runs on reload and would
  // clear the values we want to persist). Instead we
  // clear localStorage via evaluate after first goto.
  test.beforeEach(async ({ page }) => {
    await setupTerminalMocks(page);
  });

  async function clearSidebarStorage(page: import("@playwright/test").Page): Promise<void> {
    await page.evaluate(clearWorkspaceSidebarTabStorage);
    await page.evaluate(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  }

  test("sidebar open state persists across reload", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    await clearSidebarStorage(page);

    // Open sidebar
    const prBtn = page.locator(".panel-toggle-btn", {
      hasText: "PR",
    });
    await prBtn.click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Verify localStorage written
    const stored = await page.evaluate(() => localStorage.getItem("kenn-forge-workspace-sidebar-open"));
    expect(stored).toBe("true");

    // Reload — sidebar should still be open
    await page.reload();
    await expect(page.locator(".right-sidebar")).toBeVisible();
  });

  test("sidebar tab persists across reload", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    await clearSidebarStorage(page);

    // Open Reviews tab
    const reviewsBtn = page.locator(".panel-toggle-btn", {
      hasText: "Reviews",
    });
    await reviewsBtn.click();
    await expect(reviewsBtn).toHaveClass(/active/);

    // Verify localStorage
    const tab = await page.evaluate(() => localStorage.getItem("kenn-forge-workspace-sidebar-tab:ws-123"));
    expect(tab).toBe("reviews");

    // Reload
    await page.reload();
    const reviewsBtnAfter = page.locator(".panel-toggle-btn", {
      hasText: "Reviews",
    });
    await expect(reviewsBtnAfter).toHaveClass(/active/);
  });

  test("sidebar width persists after resize and reload", async ({ page }) => {
    await page.goto("/terminal/ws-123");
    await clearSidebarStorage(page);

    // Open sidebar
    await page.locator(".panel-toggle-btn", { hasText: "PR" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    const handle = page.locator(".sidebar-resize-handle");
    const box = await handle.boundingBox();
    expect(box).toBeTruthy();

    if (box) {
      // Drag left to make sidebar wider
      await page.mouse.move(box.x + 2, box.y + box.height / 2);
      await page.mouse.down();
      await page.mouse.move(box.x - 100, box.y + box.height / 2);
      await page.mouse.up();
    }

    // Width should have increased from default 360
    const width = await page.evaluate(() => localStorage.getItem("kenn-forge-workspace-sidebar-width"));
    expect(Number(width)).toBeGreaterThan(360);

    const savedWidth = Number(width);

    // Reload and check sidebar opens at saved width
    await page.reload();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    const actualWidth = await page.locator(".right-sidebar").evaluate((el) => el.offsetWidth);
    // Allow some tolerance for rounding
    expect(actualWidth).toBeGreaterThanOrEqual(savedWidth - 2);
    expect(actualWidth).toBeLessThanOrEqual(savedWidth + 2);
  });

  test("temporary layout constraints preserve and restore the preferred sidebar width", async ({ page }) => {
    await page.setViewportSize({ width: 1080, height: 720 });
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-workspace-list-sidebar-width", "260");
      localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
      localStorage.setItem("kenn-forge-workspace-sidebar-width", "400");
    });
    await page.goto("/terminal/ws-123");

    const rightSidebar = page.locator(".right-sidebar");
    await expect(rightSidebar).toBeVisible();
    await expect.poll(() => rightSidebar.evaluate((el) => el.offsetWidth)).toBe(400);

    await page.setViewportSize({ width: 900, height: 720 });
    await expect.poll(() => rightSidebar.evaluate((el) => el.offsetWidth)).toBeLessThan(280);

    const handle = page.locator(".sidebar-resize-handle");
    const box = await handle.boundingBox();
    expect(box).toBeTruthy();
    if (box) {
      await page.mouse.move(box.x + 2, box.y + box.height / 2);
      await page.mouse.down();
      await page.mouse.move(box.x - 40, box.y + box.height / 2);
      await page.mouse.up();
    }

    await expect
      .poll(() => page.evaluate(() => localStorage.getItem("kenn-forge-workspace-sidebar-width")))
      .toBe("400");

    await page.setViewportSize({ width: 1080, height: 720 });
    await expect.poll(() => rightSidebar.evaluate((el) => el.offsetWidth)).toBe(400);
    await expect
      .poll(() => page.evaluate(() => localStorage.getItem("kenn-forge-workspace-sidebar-width")))
      .toBe("400");
  });
});

// -------------------------------------------------------
// Group 3: PR Tab
// -------------------------------------------------------

test.describe("sidebar PR tab", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
    await setupTerminalMocks(page);
  });

  test("PR tab loads PR detail for workspace with linked PR", async ({ page }) => {
    await page.goto("/terminal/ws-123");

    // Open PR tab
    await page.locator(".panel-toggle-btn", { hasText: "PR" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // PR detail component should show PR title
    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Add browser regression coverage");
  });

  test("workspace without associated PR hides malformed PR tab", async ({ page }) => {
    const noLinkedPR = {
      ...testIssueWorkspace,
      associated_pr_number: null,
    };
    await setupTerminalMocks(page, {
      workspace: noLinkedPR,
    });

    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveCount(0);
  });
});

// -------------------------------------------------------
// Group 3.5: Workspace List Bubble
// -------------------------------------------------------

test.describe("workspace list bubble opens right sidebar", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  });

  test("clicking PR bubble opens PR tab in the right sidebar", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    // Sidebar should start collapsed.
    await expect(page.locator(".right-sidebar")).toHaveCount(0);

    await page.locator(".workspace-list-sidebar .ws-row .item-bubble").click();

    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveClass(/\bactive\b/);
  });

  test("clicking issue bubble opens Issue tab for issue workspace", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: testIssueWorkspace,
    });
    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".right-sidebar")).toHaveCount(0);

    await page.locator(".workspace-list-sidebar .ws-row .item-bubble").click();

    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "Issue" })).toHaveClass(/\bactive\b/);
  });

  test("Enter keypress on PR bubble does not navigate row", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    const bubble = page.locator(".workspace-list-sidebar .ws-row .item-bubble");
    await bubble.focus();
    await page.keyboard.press("Enter");

    // Sidebar should open without unintended navigation
    // (the row's Enter handler must not fire when the
    // event originates inside the bubble button).
    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page).toHaveURL(/\/terminal\/ws-123$/);
  });

  test("clicking bubble from /workspaces routes and keeps sidebar populated", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/workspaces");

    // The /workspaces route has no specific workspace selected
    // but still mounts the workspace list sidebar.
    await expect(page.locator(".workspace-list-sidebar .ws-row")).toHaveCount(1);
    await expect(page.locator(".terminal-main .state-message")).toContainText("Select a workspace from the sidebar");

    await page.locator(".workspace-list-sidebar .ws-row .item-bubble").click();

    // Navigation lands on the terminal route for the clicked
    // workspace, the sidebar stays populated rather than
    // emptying out, and the right sidebar opens to PR.
    await expect(page).toHaveURL(/\/terminal\/ws-123$/);
    await expect(page.locator(".workspace-list-sidebar .ws-row")).toHaveCount(1);
    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveClass(/\bactive\b/);
  });

  test("clicking bubble for a different workspace from /terminal navigates and keeps sidebar populated", async ({
    page,
  }) => {
    const wsA = { ...testWorkspace, id: "ws-aaa", item_number: 1 };
    const wsB = { ...testWorkspace, id: "ws-bbb", item_number: 2 };

    // First catch-all so unmocked detail routes resolve to a valid
    // workspace shape; specific routes below override.
    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsA, wsB] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
    for (const ws of [wsA, wsB]) {
      await page.route(`**/api/v1/workspaces/${ws.id}`, async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(ws),
          });
          return;
        }
        await route.fulfill({ status: 204 });
      });
      await page.route(`**/api/v1/workspaces/${ws.id}/runtime`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            launch_targets: [],
            sessions: [],
          }),
        });
      });
    }

    await page.goto(`/terminal/${wsA.id}`);
    await expect(page.locator(".workspace-list-sidebar .ws-row")).toHaveCount(2);
    await expect(page).toHaveURL(new RegExp(`/terminal/${wsA.id}$`));

    // Click the bubble for the other workspace.
    await page
      .locator(".workspace-list-sidebar .ws-row .item-bubble", {
        hasText: `#${wsB.item_number}`,
      })
      .click();

    // Should route to the other workspace, sidebar stays full,
    // right sidebar opens to PR.
    await expect(page).toHaveURL(new RegExp(`/terminal/${wsB.id}$`));
    await expect(page.locator(".workspace-list-sidebar .ws-row")).toHaveCount(2);
    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveClass(/\bactive\b/);
  });

  test("clicking bubble does not bubble up to row navigation", async ({ page }) => {
    // The row click handler must skip when the event originates
    // inside the bubble. If it didn't, the row would navigate
    // before the bubble could open the right sidebar — leaving
    // the sidebar closed.
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    let routeChanges = 0;
    page.on("framenavigated", () => {
      routeChanges += 1;
    });

    await page.locator(".workspace-list-sidebar .ws-row .item-bubble").click();

    await expect(page.locator(".right-sidebar")).toBeVisible();
    // Click on the bubble for the currently selected workspace
    // should not have triggered a frame/route navigation.
    expect(routeChanges).toBe(0);
  });

  test("clicking bubble twice toggles the right sidebar", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    const bubble = page.locator(".workspace-list-sidebar .ws-row .item-bubble");

    await bubble.click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    await bubble.click();
    await expect(page.locator(".right-sidebar")).toHaveCount(0);
  });

  test("PR bubble x-position stays stable across rows with varied meta", async ({ page }) => {
    // Regression: the bubble previously sat inside .ws-row-text and
    // its X position drifted left when the row had no push pills or
    // diff stats. Pinning the bubble to its own right column makes
    // the X position identical across rows regardless of meta.
    const wsBare = {
      ...testWorkspace,
      id: "ws-bare",
      item_number: 1,
      git_head_ref: "fix/x",
    };
    const wsBranchOnly = {
      ...testWorkspace,
      id: "ws-branch-long",
      item_number: 22,
      git_head_ref: "feature/very-long-branch-name-that-fills-the-row",
    };
    const wsAhead = {
      ...testWorkspace,
      id: "ws-ahead",
      item_number: 333,
      git_head_ref: "feature/ahead",
      commits_ahead: 7,
      commits_behind: 0,
    };
    const wsAheadBehindDiff = {
      ...testWorkspace,
      id: "ws-busy",
      item_number: 4444,
      git_head_ref: "feature/busy",
      commits_ahead: 12,
      commits_behind: 5,
      mr_additions: 1500,
      mr_deletions: 2400,
      worktree_dirty: true,
      agent_state: "working",
      agent_state_updated_at: "2026-04-11T12:05:00Z",
    };
    const wsAheadBehindDiffClean = {
      ...testWorkspace,
      id: "ws-busy-clean",
      item_number: 5555,
      git_head_ref: "feature/busy",
      commits_ahead: 12,
      commits_behind: 5,
      mr_additions: 1500,
      mr_deletions: 2400,
      worktree_dirty: false,
    };
    const wsAdHoc = {
      ...testWorkspace,
      id: "ws-adhoc",
      item_type: "adhoc",
      item_number: 0,
      git_head_ref: "feature/new-work",
      mr_title: null,
      mr_state: null,
      worktree_dirty: true,
      agent_state: "done",
      agent_state_updated_at: "2026-04-11T12:00:00Z",
    };
    const list = [wsBare, wsBranchOnly, wsAhead, wsAheadBehindDiff, wsAheadBehindDiffClean, wsAdHoc];

    await mockApi(page);
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge:workspaceListSort", "agent-status");
    });
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: list }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
    for (const ws of list) {
      await page.route(`**/api/v1/workspaces/${ws.id}`, async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(ws),
          });
          return;
        }
        await route.fulfill({ status: 204 });
      });
      await page.route(`**/api/v1/workspaces/${ws.id}/runtime`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            launch_targets: [],
            sessions: [],
          }),
        });
      });
    }

    await page.goto("/workspaces");
    await expect(page.locator(".workspace-list-sidebar .ws-row")).toHaveCount(list.length);
    const busyRow = page.locator(".workspace-list-sidebar .ws-row", {
      has: page.getByTitle("Open PR #4444"),
    });
    const cleanBusyRow = page.locator(".workspace-list-sidebar .ws-row", {
      has: page.getByTitle("Open PR #5555"),
    });
    await expect(busyRow.locator(".kit-diff-stats")).toHaveCount(1);
    await expect(page.locator(".workspace-list-sidebar .worktree-dirty")).toHaveCount(2);
    await expect(page.locator(".workspace-list-sidebar .worktree-dirty-slot")).toHaveCount(0);
    await expect(
      page.locator(".workspace-list-sidebar .ws-row-aside > .item-bubble + .workspace-sort-time + .worktree-dirty"),
    ).toHaveCount(1);

    const diffBox = await busyRow.locator(".workspace-diff-stats").boundingBox();
    const cleanDiffBox = await cleanBusyRow.locator(".workspace-diff-stats").boundingBox();
    const pushBox = await busyRow.locator(".push-state").boundingBox();
    const cleanPushBox = await cleanBusyRow.locator(".push-state").boundingBox();
    const pencilBox = await busyRow.locator(".worktree-dirty").boundingBox();
    const bubbleBox = await busyRow.locator(".item-bubble").boundingBox();
    expect(diffBox).not.toBeNull();
    expect(cleanDiffBox).not.toBeNull();
    expect(pushBox).not.toBeNull();
    expect(cleanPushBox).not.toBeNull();
    expect(pencilBox).not.toBeNull();
    expect(bubbleBox).not.toBeNull();
    if (
      diffBox != null &&
      cleanDiffBox != null &&
      pushBox != null &&
      cleanPushBox != null &&
      pencilBox != null &&
      bubbleBox != null
    ) {
      expect(Math.abs(diffBox.x + diffBox.width - (cleanDiffBox.x + cleanDiffBox.width))).toBeLessThanOrEqual(1);
      expect(Math.abs(pushBox.x - cleanPushBox.x)).toBeLessThanOrEqual(1);
      expect(pencilBox.x).toBeGreaterThan(diffBox.x + diffBox.width);
      expect(Math.abs(pencilBox.x + pencilBox.width - (bubbleBox.x + bubbleBox.width))).toBeLessThanOrEqual(1);
      expect(pencilBox.y).toBeGreaterThan(bubbleBox.y + bubbleBox.height + 2);
    }

    const adHocRow = page.locator(".workspace-list-sidebar .ws-row", { hasText: "feature/new-work" });
    const adHocSlotBox = await adHocRow.locator(".item-bubble-slot").boundingBox();
    const adHocTimeBox = await adHocRow.locator(".workspace-sort-time").boundingBox();
    const adHocPencilBox = await adHocRow.locator(".worktree-dirty").boundingBox();
    expect(adHocSlotBox).not.toBeNull();
    expect(adHocTimeBox).not.toBeNull();
    expect(adHocPencilBox).not.toBeNull();
    if (adHocSlotBox && adHocTimeBox && adHocPencilBox && bubbleBox) {
      const expectedRight = bubbleBox.x + bubbleBox.width;
      expect(Math.abs(adHocSlotBox.x + adHocSlotBox.width - expectedRight)).toBeLessThanOrEqual(1);
      expect(Math.abs(adHocTimeBox.x + adHocTimeBox.width - expectedRight)).toBeLessThanOrEqual(1);
      expect(Math.abs(adHocPencilBox.x + adHocPencilBox.width - expectedRight)).toBeLessThanOrEqual(1);
      expect(adHocTimeBox.y).toBeGreaterThanOrEqual(adHocSlotBox.y + adHocSlotBox.height);
      expect(adHocPencilBox.y).toBeGreaterThan(adHocTimeBox.y + adHocTimeBox.height);
    }

    const bubbles = page.locator(".workspace-list-sidebar .ws-row .item-bubble");
    const boxes: Array<{ right: number }> = [];
    for (let i = 0; i < (await bubbles.count()); i += 1) {
      const box = await bubbles.nth(i).boundingBox();
      expect(box).not.toBeNull();
      if (box != null) {
        boxes.push({ right: box.x + box.width });
      }
    }

    const rights = boxes.map((b) => b.right);
    const maxRight = Math.max(...rights);
    const minRight = Math.min(...rights);
    // All bubbles should align to the same right column. Allow a
    // sub-pixel tolerance for browser rounding.
    expect(maxRight - minRight).toBeLessThanOrEqual(1);
  });

  // Desktop and phone parity are separate tests: each full navigation through
  // the Vite dev server is slow under CI worker load, and four in one test
  // exhausted the per-test budget on the first attempt.
  test("workspace recency uses item-list typography on desktop", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge:workspaceListSort", "created");
    });

    await page.goto("/workspaces");
    const desktopTime = page.locator(".workspace-sort-time");
    await expect(desktopTime).toBeVisible();
    const desktopTypography = await desktopTime.evaluate((element) => {
      const style = getComputedStyle(element);
      return { fontFamily: style.fontFamily, fontSize: style.fontSize, lineHeight: style.lineHeight };
    });

    await page.goto("/pulls");
    const pullTime = page.locator(".pull-item .time").first();
    await expect(pullTime).toBeVisible();
    const pullTypography = await pullTime.evaluate((element) => {
      const style = getComputedStyle(element);
      return { fontFamily: style.fontFamily, fontSize: style.fontSize, lineHeight: style.lineHeight };
    });
    expect(desktopTypography).toEqual(pullTypography);
  });

  test("workspace recency uses item-list typography on phones without overflow", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge:workspaceListSort", "created");
    });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/m/pulls");
    const mobilePullTime = page.locator(".mobile-shell .pull-item .time").first();
    await expect(mobilePullTime).toBeVisible();
    const mobilePullTypography = await mobilePullTime.evaluate((element) => {
      const style = getComputedStyle(element);
      return { fontFamily: style.fontFamily, fontSize: style.fontSize, lineHeight: style.lineHeight };
    });

    await page.goto("/m/workspaces");
    const mobileTime = page.locator(".mobile-workspace-row__sort-time");
    const mobileRow = page.locator(".mobile-workspace-row");
    await expect(mobileTime).toBeVisible();
    const mobileTypography = await mobileTime.evaluate((element) => {
      const style = getComputedStyle(element);
      return { fontFamily: style.fontFamily, fontSize: style.fontSize, lineHeight: style.lineHeight };
    });
    expect(mobileTypography).toEqual(mobilePullTypography);

    const rowBox = await mobileRow.boundingBox();
    const timeBox = await mobileTime.boundingBox();
    expect(rowBox).not.toBeNull();
    expect(timeBox).not.toBeNull();
    if (rowBox && timeBox) {
      expect(timeBox.x).toBeGreaterThanOrEqual(rowBox.x);
      expect(timeBox.x + timeBox.width).toBeLessThanOrEqual(rowBox.x + rowBox.width);
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  });

  test("filters workspace rows by repo, title, and item number", async ({ page }) => {
    const wsTitle = {
      ...testWorkspace,
      id: "ws-title",
      repo_owner: "kenn-io",
      repo_name: "taskboard",
      repo: workspaceRepoRef("kenn-io", "taskboard"),
      item_number: 9,
      mr_title: "Migrate native HTTP surface to Huma v2",
    };
    const wsRepo = {
      ...testWorkspace,
      id: "ws-repo",
      repo_owner: "kenn-io",
      repo_name: "kenn-platform",
      repo: workspaceRepoRef("kenn-io", "kenn-platform"),
      item_number: 2,
      mr_title: "Hosted code fetch and caching strategy",
    };
    const wsNumber = {
      ...testIssueWorkspace,
      id: "ws-number",
      repo_owner: "kenn-io",
      repo_name: "kenn-forge",
      repo: workspaceRepoRef("kenn-io", "kenn-forge"),
      item_number: 224,
      mr_title: "Add notification inbox triage",
    };
    const list = [wsTitle, wsRepo, wsNumber];

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: list }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });

    await page.goto("/workspaces");

    const rows = page.locator(".workspace-list-sidebar .ws-row");
    const filter = page.getByLabel("Filter workspaces");
    await expect(rows).toHaveCount(3);

    await filter.fill("huma");
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText("Migrate native HTTP surface to Huma v2");

    await filter.fill("kenn-platform");
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText("Hosted code fetch and caching strategy");

    await filter.fill("#224");
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText("Add notification inbox triage");

    await filter.fill("not-present");
    await expect(rows).toHaveCount(0);
    await expect(page.locator(".workspace-list-sidebar")).toContainText("No workspaces match.");
  });
});

// -------------------------------------------------------
// Group 3.4: Workspace list sorting
// -------------------------------------------------------

test.describe("workspace list sorting", () => {
  // Three workspaces across two repos, in API order
  // (created_at DESC). The most recently active workspace is
  // neither the newest nor in the first repo group, so each sort
  // mode produces a distinct row order.
  const wsNew = {
    ...testWorkspace,
    id: "ws-new",
    item_number: 3,
    mr_title: "Newest created",
    created_at: "2026-05-12T12:00:00Z",
    tmux_last_output_at: "2026-05-12T13:00:00Z",
  };
  const wsMid = {
    ...testWorkspace,
    id: "ws-mid",
    repo_owner: "kenn-io",
    repo_name: "agentsview",
    repo: workspaceRepoRef("kenn-io", "agentsview"),
    item_number: 2,
    mr_title: "Most recently active",
    created_at: "2026-05-11T12:00:00Z",
    tmux_last_output_at: "2026-05-14T09:00:00Z",
  };
  const wsOld = {
    ...testWorkspace,
    id: "ws-old",
    item_number: 1,
    mr_title: "Oldest without activity",
    created_at: "2026-05-10T12:00:00Z",
  };

  test.beforeEach(async ({ page }) => {
    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsNew, wsMid, wsOld] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
  });

  test("offers creation and activity sorts and persists the choice across reloads", async ({ page }) => {
    await page.goto("/workspaces");

    const names = page.locator(".workspace-list-sidebar .ws-name");
    const headers = page.locator(".workspace-list-sidebar .sidebar-group-header");
    await expect(names).toHaveText(["Newest created", "Oldest without activity", "Most recently active"]);
    await expect(headers).toHaveCount(2);

    const sortTrigger = page.getByTitle("View workspace options");
    await sortTrigger.click();
    await page.locator(".kit-filter-dropdown__panel").getByRole("button", { name: "Created" }).click();

    await expect(names).toHaveText(["Newest created", "Most recently active", "Oldest without activity"]);
    await expect(headers).toHaveCount(0);
    // Without group headers each row carries its own repo context.
    await expect(page.locator(".workspace-list-sidebar .repo-context").first()).toContainText("acme/widgets");

    await sortTrigger.click();
    await page.locator(".kit-filter-dropdown__panel").getByRole("button", { name: "Activity", exact: true }).click();

    // ws-old has no tmux output and falls back to creation time.
    await expect(names).toHaveText(["Most recently active", "Newest created", "Oldest without activity"]);

    await page.reload();

    await expect(names).toHaveText(["Most recently active", "Newest created", "Oldest without activity"]);
    await expect(headers).toHaveCount(0);

    await sortTrigger.click();
    await page.locator(".kit-filter-dropdown__panel").getByRole("button", { name: "Org / repo" }).click();

    await expect(headers).toHaveCount(2);
    await expect(names).toHaveText(["Newest created", "Oldest without activity", "Most recently active"]);
  });

  test("toggles visibility options and persists provider-aware labels", async ({ page }) => {
    const wsGithub = {
      ...testWorkspace,
      id: "ws-github-provider",
      item_number: 10,
      mr_title: "GitHub provider workspace",
      mr_additions: 8,
      mr_deletions: 3,
    };
    const wsGitea = {
      ...testWorkspace,
      id: "ws-gitea-provider",
      repo: workspaceRepoRef("acme", "widgets", "github.com", "gitea"),
      item_number: 11,
      mr_title: "Gitea provider workspace",
      mr_additions: 5,
      mr_deletions: 1,
    };
    const wsForge = {
      ...testWorkspace,
      id: "ws-kenn-forge",
      repo_owner: "kenn-io",
      repo_name: "kenn-forge",
      repo: workspaceRepoRef("kenn-io", "kenn-forge"),
      item_number: 12,
      mr_title: "Kenn Forge workspace",
      mr_additions: 13,
      mr_deletions: 2,
    };

    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsGithub, wsGitea, wsForge] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });

    await page.goto("/workspaces");

    const groupLabels = page.locator(".workspace-list-sidebar .sidebar-group-header__name");
    await expect(groupLabels).toHaveText([
      "github/github.com/acme/widgets",
      "gitea/github.com/acme/widgets",
      "kenn-io/kenn-forge",
    ]);
    await expect(page.locator(".workspace-list-sidebar .workspace-diff-stats")).toHaveCount(3);

    const viewTrigger = page.getByTitle("View workspace options");
    await viewTrigger.click();
    await page.locator(".kit-filter-dropdown__panel").getByRole("button", { name: "Hide org name" }).click();

    await expect(groupLabels).toHaveText([
      "github/github.com/acme/widgets",
      "gitea/github.com/acme/widgets",
      "kenn-forge",
    ]);

    await page.locator(".kit-filter-dropdown__panel").getByRole("button", { name: "Show PR diff stats" }).click();
    await expect(page.locator(".workspace-list-sidebar .workspace-diff-stats")).toHaveCount(0);

    await page.reload();

    await expect(groupLabels).toHaveText([
      "github/github.com/acme/widgets",
      "gitea/github.com/acme/widgets",
      "kenn-forge",
    ]);
    await expect(page.locator(".workspace-list-sidebar .workspace-diff-stats")).toHaveCount(0);
  });

  test("view trigger hugs the collapse toggle and the dropdown opens end-aligned", async ({ page }) => {
    await page.goto("/workspaces");

    const trigger = page.getByTitle("View workspace options");
    const toggle = page.getByRole("button", { name: "Collapse Workspaces sidebar" });
    await expect(trigger).toBeVisible();
    await expect(toggle).toBeVisible();

    const triggerBox = await trigger.boundingBox();
    const toggleBox = await toggle.boundingBox();
    expect(triggerBox).not.toBeNull();
    expect(toggleBox).not.toBeNull();

    // Regression: two competing auto margins (sort wrapper + toggle
    // push class) once split the header's free space and stranded
    // the trigger mid-header. The trigger must sit directly beside
    // the toggle at the right edge.
    const gap = toggleBox!.x - (triggerBox!.x + triggerBox!.width);
    expect(gap).toBeGreaterThanOrEqual(0);
    expect(gap).toBeLessThanOrEqual(12);

    await trigger.click();
    const dropdown = page.locator(".kit-filter-dropdown__panel");
    await expect(dropdown).toBeVisible();
    const dropdownBox = await dropdown.boundingBox();
    expect(dropdownBox).not.toBeNull();

    // The wider View menu is end-aligned so it stays inside the
    // rail while the trigger remains beside the collapse toggle.
    expect(Math.abs(dropdownBox!.x + dropdownBox!.width - (triggerBox!.x + triggerBox!.width))).toBeLessThanOrEqual(
      1.5,
    );
  });
});

// -------------------------------------------------------
// Group 3.5: Delayed-response navigation (no flash, no
// stale-action targets)
// -------------------------------------------------------

test.describe("delayed-response navigation", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  });

  test("switching workspaces shows the loading state until the new load resolves", async ({ page }) => {
    // Workspace A loads instantly. Workspace B's GET is held back
    // so the UI is forced into the transition window. Liveness
    // rendering replaces A's view with the loading state there —
    // a stale header with disabled (or worse, live) actions must
    // never linger while the route already points at B.
    const wsA = {
      ...testWorkspace,
      id: "ws-aaa",
      item_number: 1,
      mr_title: "A title",
    };
    const wsB = {
      ...testWorkspace,
      id: "ws-bbb",
      item_number: 2,
      mr_title: "B title",
    };

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsA, wsB] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });

    // wsA — instant.
    await page.route(`**/api/v1/workspaces/${wsA.id}`, async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(wsA),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    });
    await page.route(`**/api/v1/workspaces/${wsA.id}/runtime`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          launch_targets: [],
          sessions: [],
        }),
      });
    });

    // wsB — delayed. Resolved manually below so the test can
    // observe the in-place transition.
    let releaseB: () => void = () => {};
    const bDelay = new Promise<void>((resolve) => {
      releaseB = resolve;
    });
    await page.route(`**/api/v1/workspaces/${wsB.id}`, async (route) => {
      if (route.request().method() === "GET") {
        await bDelay;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(wsB),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    });
    await page.route(`**/api/v1/workspaces/${wsB.id}/runtime`, async (route) => {
      await bDelay;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          launch_targets: [],
          sessions: [],
        }),
      });
    });

    await page.goto(`/terminal/${wsA.id}`);

    // Confirm wsA is visible (its title sits in the header bar).
    await expect(page.locator(".terminal-main .header-name")).toContainText(wsA.mr_title);

    // Click row B from the sidebar.
    await page
      .locator(".workspace-list-sidebar .ws-row", {
        hasText: `#${wsB.item_number}`,
      })
      .click();

    // URL has switched to B, but B's data hasn't arrived yet — the
    // loading state owns the window. A's header (and its actions,
    // which could otherwise misfire at B's id) must be gone.
    await expect(page).toHaveURL(new RegExp(`/terminal/${wsB.id}$`));
    await expect(page.locator(".terminal-main .state-message")).toContainText("Setting up workspace...");
    await expect(page.locator(".terminal-main .header-name")).toHaveCount(0);
    await expect(page.locator(".terminal-main .header-btn.danger")).toHaveCount(0);

    // Release B's response — the UI resolves to wsB with live actions.
    releaseB();
    await expect(page.locator(".terminal-main .header-name")).toContainText(wsB.mr_title);
    await expect(page.locator(".terminal-main .header-btn.danger")).toBeEnabled();
  });

  test("terminal panel closes when navigating to a different workspace", async ({ page }) => {
    // Regression: keeping the bottom terminal open across a workspace
    // change kept the previous workspace's shell TerminalPane
    // mounted with its WebSocket pointing at workspace A. The
    // user could see workspace B but type into A's shell.
    const wsA = {
      ...testWorkspace,
      id: "ws-aaa",
      item_number: 1,
      mr_title: "A title",
    };
    const wsB = {
      ...testWorkspace,
      id: "ws-bbb",
      item_number: 2,
      mr_title: "B title",
    };
    const shellSession = (wsId: string) => ({
      key: `${wsId}:plain_shell`,
      workspace_id: wsId,
      target_key: "plain_shell",
      label: "Plain shell",
      kind: "plain_shell",
      status: "running" as const,
      created_at: "2026-04-10T12:00:00Z",
    });

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsA, wsB] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
    for (const ws of [wsA, wsB]) {
      await page.route(`**/api/v1/workspaces/${ws.id}`, async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(ws),
          });
          return;
        }
        await route.fulfill({ status: 204 });
      });
      await page.route(`**/api/v1/workspaces/${ws.id}/runtime`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            ...workspaceRuntime,
            sessions: [shellSession(ws.id)],
          }),
        });
      });
    }

    await page.goto(`/terminal/${wsA.id}`);
    // Open the terminal panel for A.
    await page.getByRole("button", { name: "Open terminal panel" }).click();
    await expect(page.locator(".terminal-panel.open .terminal-container")).toBeVisible();

    // Navigate to B by clicking its row.
    await page
      .locator(".workspace-list-sidebar .ws-row", {
        hasText: `#${wsB.item_number}`,
      })
      .click();
    await expect(page).toHaveURL(new RegExp(`/terminal/${wsB.id}$`));

    // The panel must close so the previous workspace's shell
    // pane unmounts and its WebSocket tears down. Otherwise
    // keystrokes from B's session would be routed to A's shell.
    await expect(page.locator(".terminal-panel.open .terminal-container")).toHaveCount(0);
  });

  test("previous workspace's runtime sessions are not visible while B's runtime is loading", async ({ page }) => {
    // Regression: after the workspace fetch resolved, runtime
    // still held the previous workspace's payload until its own
    // fetch completed. The workspace stage briefly rendered A's
    // session tabs (and launch targets) inside B's view, with
    // actionsBlocked already false.
    const wsA = {
      ...testWorkspace,
      id: "ws-aaa",
      item_number: 1,
      mr_title: "A title",
    };
    const wsB = {
      ...testWorkspace,
      id: "ws-bbb",
      item_number: 2,
      mr_title: "B title",
    };
    const sessionA = {
      key: "ws-aaa:helper",
      workspace_id: "ws-aaa",
      target_key: "helper",
      label: "Helper A",
      kind: "agent" as const,
      status: "running" as const,
      created_at: "2026-04-10T12:00:00Z",
    };

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsA, wsB] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
    // wsA: instant.
    await page.route(`**/api/v1/workspaces/${wsA.id}`, async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(wsA),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    });
    await page.route(`**/api/v1/workspaces/${wsA.id}/runtime`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ...workspaceRuntime,
          sessions: [sessionA],
        }),
      });
    });
    // wsB: workspace GET is fast, runtime GET is held.
    await page.route(`**/api/v1/workspaces/${wsB.id}`, async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(wsB),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    });
    let releaseBRuntime: () => void = () => {};
    const bRuntimeDelay = new Promise<void>((resolve) => {
      releaseBRuntime = resolve;
    });
    await page.route(`**/api/v1/workspaces/${wsB.id}/runtime`, async (route) => {
      await bRuntimeDelay;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(workspaceRuntime),
      });
    });

    await page.goto(`/terminal/${wsA.id}`);
    // A's session tab should be visible.
    await expect(page.locator(".workspace-stage .tabbed-panel-tab-panel")).not.toHaveCount(0);

    await page
      .locator(".workspace-list-sidebar .ws-row", {
        hasText: `#${wsB.item_number}`,
      })
      .click();

    // The header should have moved to B as soon as wsB's
    // workspace GET resolves. But because B's runtime fetch is
    // still in flight, the workspace stage must show the
    // "Loading workspace runtime..." state, not A's session
    // panes.
    await expect(page.locator(".terminal-main .header-name")).toContainText(wsB.mr_title);
    await expect(page.locator(".workspace-stage .state-message")).toContainText("Loading workspace runtime");

    releaseBRuntime();

    // Once B's runtime resolves, the loading state goes away.
    await expect(page.locator(".workspace-stage .state-message")).toHaveCount(0);
  });

  test("an in-flight retry does not leave the next workspace's controls stuck", async ({ page }) => {
    // Regression: retryingSetup is only cleared in its finally block
    // when the workspace is still current. Navigating away while a
    // retry is in flight skipped that cleanup, leaving the flag stuck
    // true and disabling the next workspace's Retry control.
    const wsA = {
      ...testWorkspace,
      id: "ws-aaa",
      item_number: 1,
      mr_title: "A title",
      status: "error",
      error_message: "setup failed A",
    };
    const wsB = {
      ...testWorkspace,
      id: "ws-bbb",
      item_number: 2,
      mr_title: "B title",
      status: "error",
      error_message: "setup failed B",
    };

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ workspaces: [wsA, wsB] }),
        });
        return;
      }
      await route.fulfill({ status: 200 });
    });
    for (const ws of [wsA, wsB]) {
      await page.route(`**/api/v1/workspaces/${ws.id}`, async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(ws),
          });
          return;
        }
        await route.fulfill({ status: 204 });
      });
      await page.route(`**/api/v1/workspaces/${ws.id}/runtime`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ launch_targets: [], sessions: [] }),
        });
      });
    }
    // A's retry never resolves so retryingSetup stays true across the
    // navigation to B.
    await page.route(`**/api/v1/workspaces/${wsA.id}/retry`, async () => {
      await new Promise(() => {});
    });

    await page.goto(`/terminal/${wsA.id}`);

    const errorA = page.locator(".terminal-main .state-message.error");
    await expect(errorA).toContainText("setup failed A");
    const retryA = errorA.getByRole("button", { name: "Retry" });
    await expect(retryA).toBeEnabled();
    await retryA.click();
    // The in-flight retry disables A's own Retry control.
    await expect(retryA).toBeDisabled();

    // Switch to workspace B while A's retry is still in flight.
    await page
      .locator(".workspace-list-sidebar .ws-row", {
        hasText: `#${wsB.item_number}`,
      })
      .click();
    await expect(page).toHaveURL(new RegExp(`/terminal/${wsB.id}$`));

    const errorB = page.locator(".terminal-main .state-message.error");
    await expect(errorB).toContainText("setup failed B");
    // B's Retry control must not inherit A's stale in-flight state.
    await expect(errorB.getByRole("button", { name: "Retry" })).toBeEnabled();
  });

  test("right sidebar diff keeps the rendered workspace's host during a cross-host switch", async ({ page }) => {
    // Regression: during the in-place transition the previous workspace
    // stays rendered while the route's host key already points at the
    // new workspace. The right sidebar combined the old workspace id
    // with the new host key, so the diff panel could fetch the local
    // workspace's id from the wrong fleet host.
    const localWorkspace = {
      ...testWorkspace,
      id: "ws-local",
      item_number: 1,
      mr_title: "Local A",
    };
    const memberWorkspace = {
      ...testWorkspace,
      id: "ws-member",
      item_number: 2,
      mr_title: "Member B",
      worktree_path: "/data/member/worktrees/ws-member",
      fleet_host_key: "member",
      fleet_host_name: "member",
    };

    const fleetMemberRequests: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path.startsWith("/api/v1/fleet/hosts/member/")) {
        fleetMemberRequests.push(path);
      }
    });

    await mockApi(page);
    await page.route("**/api/v1/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "",
      });
    });
    await page.route(
      (url) => url.pathname === "/api/v1/snapshot",
      async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
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
              {
                configKey: "member",
                diagnostics: [],
                id: "member",
                kind: "remote",
                name: "member",
                operationAvailability: {
                  workspaceRead: { available: true },
                  terminalAttach: { available: true },
                },
                platform: "linux",
                preferredTransport: "http",
                reachable: true,
                tmuxSessions: [],
              },
            ],
            workspaces: [localWorkspace, memberWorkspace],
          }),
        });
      },
    );
    // Local workspace A — instant.
    await page.route(`**/api/v1/workspaces/${localWorkspace.id}`, async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(localWorkspace),
        });
        return;
      }
      await route.fulfill({ status: 204 });
    });
    await page.route(`**/api/v1/workspaces/${localWorkspace.id}/runtime`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ launch_targets: [], sessions: [] }),
      });
    });
    for (const suffix of ["files", "diff"]) {
      await page.route(`**/api/v1/workspaces/${localWorkspace.id}/${suffix}*`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ stale: false, whitespace_only_count: 0, files: [] }),
        });
      });
    }

    // Member workspace B detail is held so the transition window stays
    // open: the URL points at B while A is still rendered.
    let releaseB: () => void = () => {};
    const bDelay = new Promise<void>((resolve) => {
      releaseB = resolve;
    });
    await page.route(
      (url) => url.pathname === `/api/v1/fleet/hosts/member/workspaces/${memberWorkspace.id}`,
      async (route) => {
        await bDelay;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(memberWorkspace),
        });
      },
    );
    await page.route(`**/api/v1/fleet/hosts/member/workspaces/${memberWorkspace.id}/runtime`, async (route) => {
      await bDelay;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ launch_targets: [], sessions: [] }),
      });
    });

    await page.goto(`/terminal/${localWorkspace.id}`);
    await expect(page.locator(".terminal-main .header-name")).toContainText("Local A");

    // Open the diff tab so the right sidebar's diff panel is active and
    // confirm it fetches the local workspace from the local host.
    const localFilesRequest = page.waitForRequest(
      (request) => new URL(request.url()).pathname === `/api/v1/workspaces/${localWorkspace.id}/files`,
    );
    await page.locator(".panel-toggle-btn", { hasText: "Diff" }).click();
    await localFilesRequest;

    // Switch to the member-hosted workspace; its detail is held, so the
    // route host is "member" while B's data hasn't arrived.
    const bDetailRequest = page.waitForRequest(
      (request) => new URL(request.url()).pathname === `/api/v1/fleet/hosts/member/workspaces/${memberWorkspace.id}`,
    );
    await page.locator(".workspace-list-sidebar .ws-row", { hasText: "Member B" }).click();
    await expect(page).toHaveURL(new RegExp(`/terminal/fleet/member/${memberWorkspace.id}$`));
    // Liveness rendering unmounts A's view (including its diff sidebar)
    // for the loading state while B's detail is held, so nothing can
    // combine A's id with the member host during the window.
    await expect(page.locator(".terminal-main .state-message")).toContainText("Setting up workspace...");
    await bDetailRequest;

    // The only member-host calls may be B's own detail/runtime. The
    // diff panel must not have fetched the local workspace's id from
    // the member host.
    await expect
      .poll(() => fleetMemberRequests.filter((path) => path.includes(`/workspaces/${localWorkspace.id}`)))
      .toEqual([]);

    releaseB();
    await expect(page.locator(".terminal-main .header-name")).toContainText("Member B");
  });
});

// -------------------------------------------------------
// Group 4: Issue Workspace Sidebar
// -------------------------------------------------------

test.describe("issue workspace sidebar", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  });

  test("issue workspaces show an Issue toggle instead of PR and Reviews when no PR is linked", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: testIssueWorkspace,
    });
    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".panel-toggle-btn", { hasText: "Issue" })).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveCount(0);
    await expect(page.locator(".panel-toggle-btn", { hasText: "Reviews" })).toHaveCount(0);
  });

  test("manual refresh rechecks issue workspace PR association", async ({ page }) => {
    let refreshRequests = 0;

    await setupTerminalMocks(page, {
      workspace: testIssueWorkspace,
    });
    await page.route("**/api/v1/workspaces/ws-issue-7/refresh", async (route) => {
      refreshRequests += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(testIssueWorkspaceWithAssociatedPR),
      });
    });
    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".panel-toggle-btn", { hasText: "Issue" })).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveCount(0);

    const refreshButton = page.getByRole("button", { name: "Refresh workspace details" });
    await expect(refreshButton).toHaveAttribute("aria-label", "Refresh workspace details");
    await refreshButton.click();

    await expect.poll(() => refreshRequests).toBe(1);
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toBeVisible();
  });

  test("issue toggle opens issue detail for issue-backed workspaces", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: testIssueWorkspace,
    });
    await page.goto("/terminal/ws-issue-7");

    await page.locator(".panel-toggle-btn", { hasText: "Issue" }).click();

    await expect(page.locator(".right-sidebar")).toBeVisible();
    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Theme toggle does not stick");
  });

  test("issue toggle includes workspace platform_host in detail requests", async ({ page }) => {
    const mirroredWorkspace = {
      ...testIssueWorkspace,
      platform_host: "example.com",
      repo: workspaceRepoRef("acme", "widgets", "example.com"),
    };
    const seenHosts: string[] = [];
    const mirroredIssueDetail = {
      issue: {
        ID: 2,
        RepoID: 2,
        GitHubID: 202,
        Number: 7,
        URL: "https://example.com/acme/widgets/issues/7",
        Title: "Mirror host issue",
        Author: "marius",
        State: "open",
        Body: "",
        CommentCount: 1,
        LabelsJSON: "[]",
        CreatedAt: "2026-03-28T14:00:00Z",
        UpdatedAt: "2026-03-30T14:00:00Z",
        LastActivityAt: "2026-03-30T14:00:00Z",
        ClosedAt: null,
        Starred: false,
      },
      events: [],
      platform_host: "example.com",
      repo_owner: "acme",
      repo_name: "widgets",
      detail_loaded: true,
      detail_fetched_at: "2026-03-30T14:00:00Z",
    };

    await setupTerminalMocks(page, {
      workspace: mirroredWorkspace,
    });

    await page.route("**/api/v1/host/example.com/issues/github/acme/widgets/7", async (route) => {
      seenHosts.push("example.com");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(mirroredIssueDetail),
      });
    });
    await page.route("**/api/v1/host/example.com/issues/github/acme/widgets/7/sync", async (route) => {
      seenHosts.push("example.com");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(mirroredIssueDetail),
      });
    });

    await page.goto("/terminal/ws-issue-7");
    await page.locator(".panel-toggle-btn", { hasText: "Issue" }).click();

    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Mirror host issue");
    await expect.poll(() => seenHosts).toEqual(["example.com"]);
  });

  test("issue workspace with associated PR shows Issue and PR tabs but no Reviews", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: testIssueWorkspaceWithAssociatedPR,
    });
    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".panel-toggle-btn", { hasText: "Issue" })).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toBeVisible();
    await expect(page.locator(".panel-toggle-btn", { hasText: "Reviews" })).toHaveCount(0);
  });

  test("issue workspace PR tab hides workspace create and open actions", async ({ page }) => {
    await setupTerminalMocks(page, {
      workspace: testIssueWorkspaceWithAssociatedPR,
    });
    await page.goto("/terminal/ws-issue-7");

    await page.locator(".panel-toggle-btn", { hasText: "PR" }).click();

    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Add browser regression coverage");
    await expect(
      page.locator(".right-sidebar").getByRole("button", {
        name: /^(Create|Open) Workspace/,
      }),
    ).toHaveCount(0);
  });

  test("issue workspace gains PR tab after workspace_status refetch and keeps manual PR selection", async ({
    page,
  }) => {
    let currentWorkspace: WorkspaceFixture = {
      ...testIssueWorkspace,
      associated_pr_number: null,
    };

    await page.addInitScript(() => {
      localStorage.setItem("kenn-forge-workspace-sidebar-open", "true");
      localStorage.setItem("kenn-forge-workspace-sidebar-tab:ws-issue-7", "issue");

      const instances: Array<{
        listeners: Map<string, Set<(event: MessageEvent) => void>>;
      }> = [];

      class FakeEventSource {
        listeners = new Map<string, Set<(event: MessageEvent) => void>>();

        constructor() {
          instances.push(this);
        }

        addEventListener(type: string, callback: (event: MessageEvent) => void): void {
          const bucket = this.listeners.get(type) ?? new Set();
          bucket.add(callback);
          this.listeners.set(type, bucket);
        }

        close(): void {}
      }

      (window as typeof window & { EventSource: typeof EventSource }).EventSource =
        FakeEventSource as unknown as typeof EventSource;
      (
        window as typeof window & {
          __emitWorkspaceStatus: (payload: { id: string }) => void;
        }
      ).__emitWorkspaceStatus = (payload) => {
        const event = new MessageEvent("workspace_status", {
          data: JSON.stringify(payload),
        });
        for (const instance of instances) {
          const listeners = instance.listeners.get("workspace_status") ?? new Set();
          for (const listener of listeners) {
            listener(event);
          }
        }
      };
    });

    await mockApi(page);
    await page.route("**/api/v1/snapshot**", async (route) => {
      if (route.request().method() !== "GET") {
        await route.fulfill({ status: 200 });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaces: [currentWorkspace] }),
      });
    });
    await page.route("**/api/v1/workspaces/ws-issue-7", async (route) => {
      if (route.request().method() !== "GET") {
        await route.fulfill({ status: 204 });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(currentWorkspace),
      });
    });

    await page.goto("/terminal/ws-issue-7");

    await expect(page.locator(".panel-toggle-btn", { hasText: "PR" })).toHaveCount(0);

    currentWorkspace = testIssueWorkspaceWithAssociatedPR;
    await page.evaluate(() => {
      (
        window as typeof window & {
          __emitWorkspaceStatus: (payload: { id: string }) => void;
        }
      ).__emitWorkspaceStatus({ id: "ws-issue-7" });
    });

    const issueButton = page.locator(".panel-toggle-btn", { hasText: "Issue" });
    const prButton = page.locator(".panel-toggle-btn", { hasText: "PR" });
    await expect(prButton).toBeVisible();
    await expect(issueButton).toBeVisible();
    await expect(issueButton).toHaveClass(/active/);

    await prButton.click();
    await expect(prButton).toHaveClass(/active/);
    await expect(page.locator(".right-sidebar .detail-title")).toContainText("Add browser regression coverage");

    await page.evaluate(() => {
      (
        window as typeof window & {
          __emitWorkspaceStatus: (payload: { id: string }) => void;
        }
      ).__emitWorkspaceStatus({ id: "ws-issue-7" });
    });
    await expect(prButton).toHaveClass(/active/);
  });
});

// -------------------------------------------------------
// Group 5: Reviews Tab
// -------------------------------------------------------

test.describe("sidebar Reviews tab", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(clearWorkspaceSidebarTabStorage);
    await page.addInitScript(() => {
      localStorage.removeItem("kenn-forge-workspace-sidebar-open");
      localStorage.removeItem("kenn-forge-workspace-sidebar-width");
    });
  });

  test("Reviews tab preserves a daemon version that already starts with v", async ({ page }) => {
    await setupTerminalMocks(page, {
      roborevStatus: {
        ...roborevStatus,
        version: "v0.52.0",
      },
    });
    await page.goto("/terminal/ws-123");

    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    await expect(page.locator('.right-sidebar .daemon-status [title="Daemon version"]')).toHaveText("v0.52.0");
  });

  test("Reviews tab shows job list when roborev repo matches", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    // Open Reviews tab
    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Job list should render the mock job
    await expect(page.locator(".right-sidebar .job-row")).toBeVisible();
    await expect(page.locator(".right-sidebar .job-row")).toContainText("Add auth middleware");
    await expect(page.locator(".right-sidebar .job-table thead")).toContainText("Cost");
    await expect(page.locator(".right-sidebar .job-row")).toContainText("~$0.42");
  });

  test("Reviews tab shows empty state when no repo matches", async ({ page }) => {
    await setupTerminalMocks(page, {
      roborevRepos: { repos: [], total_count: 0 },
    });
    await page.goto("/terminal/ws-123");

    // Open Reviews tab
    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Should show empty/no-reviews message
    await expect(page.locator(".right-sidebar .kit-empty-state")).toContainText("No reviews");
  });

  test("branch picker shows and clears branch filter", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    // Open Reviews tab
    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    await expect(page.locator(".right-sidebar")).toBeVisible();

    // Branch filter should show workspace branch
    const picker = page.locator('.right-sidebar .picker-button[title="Filter by repository"]');
    await expect(picker).toContainText("feature/auth");

    // Selecting All Repos clears the branch filter
    await picker.click();
    await page
      .locator(".right-sidebar .dropdown-item", {
        hasText: "All Repos",
      })
      .click();
    await expect(picker).toContainText("All Repos");
  });

  test("selecting a job does not navigate to /reviews", async ({ page }) => {
    await setupTerminalMocks(page);
    await page.goto("/terminal/ws-123");

    // Open Reviews tab
    await page.locator(".panel-toggle-btn", { hasText: "Reviews" }).click();
    await expect(page.locator(".right-sidebar .job-row")).toBeVisible();

    // Click the job row
    await page.locator(".right-sidebar .job-row").first().click();

    // URL should stay on /terminal, not navigate
    await expect(page).toHaveURL(/\/terminal\/ws-123/);
    // Job row should get selected state
    await expect(page.locator(".right-sidebar .job-row").first()).toHaveClass(/selected/);
  });
});
