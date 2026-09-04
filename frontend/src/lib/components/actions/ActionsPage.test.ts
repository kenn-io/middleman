import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";
import { STORES_KEY } from "../../context.js";
import { createWorkflowActionsStore } from "../../stores/workflow-actions.svelte.js";
import { setGlobalRepo } from "../../stores/filter.svelte.js";
import {
  createMockApiFetch,
  jsonResponse,
  type MockApiHandle,
  type MockRouteOverride,
} from "../../../test/mockApiFetch.js";

const runtimeHolder = vi.hoisted(() => ({ value: undefined as OwnedAppRuntime | undefined }));
vi.mock("../../app/runtime-context.js", () => ({
  getAppRuntime: () => runtimeHolder.value,
}));

import ActionsPage from "./ActionsPage.svelte";

const capabilities = {
  read_repositories: true,
  read_merge_requests: true,
  read_issues: true,
  read_issue_pr_references: true,
  read_comments: true,
  read_releases: true,
  read_ci: true,
  read_workflows: true,
  read_workflow_runs: true,
  workflow_dispatch: true,
  read_labels: true,
  read_markdown_images: true,
  read_authenticated_user: true,
  comment_mutation: true,
  state_mutation: true,
  merge_mutation: true,
  label_mutation: true,
  assignee_mutation: true,
  reviewer_mutation: true,
  review_mutation: true,
  workflow_approval: true,
  ready_for_review: true,
  draft_mutation: true,
  issue_mutation: true,
  review_draft_mutation: false,
  review_thread_resolution: false,
  review_suggestion_application: false,
  read_review_threads: false,
  native_multiline_ranges: false,
  mutation_head_binding: false,
  thread_reply: false,
  thread_resolve: false,
  supported_review_actions: [],
};

const available = { available: true };

function repoSummary(name: string, supported = true) {
  return {
    owner: "acme",
    name,
    platform_host: "github.com",
    default_platform_host: "github.com",
    repo: {
      provider: "github",
      platform_host: "github.com",
      owner: "acme",
      name,
      repo_path: `acme/${name}`,
      capabilities: supported
        ? capabilities
        : {
            ...capabilities,
            read_workflows: false,
            read_workflow_runs: false,
            workflow_dispatch: false,
          },
      operations: { dispatch_workflow: available },
    },
    operations: { dispatch_workflow: available },
    cached_pr_count: 0,
    open_pr_count: 0,
    draft_pr_count: 0,
    cached_issue_count: 0,
    open_issue_count: 0,
    active_authors: [],
    recent_issues: [],
    commit_timeline: [],
    releases: [],
  };
}

function workflowFixtures(): MockRouteOverride {
  const summaries = [
    repoSummary("alpha"),
    repoSummary("beta"),
    repoSummary("legacy", false),
    repoSummary("filtered-out"),
  ];
  return (request) => {
    if (request.method === "GET" && request.url.pathname === "/api/v1/repos/summary") {
      return jsonResponse(summaries);
    }
    const catalog = request.url.pathname.match(/^\/api\/v1\/actions\/github\/acme\/([^/]+)\/workflows$/);
    if (request.method === "GET" && catalog) {
      const name = catalog[1]!;
      return jsonResponse({
        repo: { ...repoSummary(name).repo, default_branch: "trunk" },
        environments: [{ name: "production" }],
        workflows: [
          {
            id: `${name}-deploy.yml`,
            name: `${name} deploy`,
            path: `.github/workflows/${name}-deploy.yml`,
            state: "active",
            available: true,
            definition_sha: `${name}-definition`,
            inputs: [],
            web_url: `https://github.com/acme/${name}/actions/workflows/${name}-deploy.yml`,
          },
        ],
      });
    }
    const runs = request.url.pathname.match(/^\/api\/v1\/actions\/github\/acme\/([^/]+)\/runs$/);
    if (request.method === "GET" && runs) {
      const name = runs[1]!;
      if (request.url.searchParams.get("cursor") === "older-page") {
        return jsonResponse({
          repo: { ...repoSummary(name).repo, default_branch: "trunk" },
          exhausted: true,
          items: [
            {
              actor: "octocat",
              conclusion: "success",
              created_at: "2026-08-26T12:30:00Z",
              event: "workflow_dispatch",
              head_sha: "olderabcdef",
              id: `${name}-run-older`,
              name: `${name} deploy`,
              ref: "release/v1",
              run_number: 5,
              status: "completed",
              workflow_id: `${name}-deploy.yml`,
            },
          ],
        });
      }
      return jsonResponse({
        repo: { ...repoSummary(name).repo, default_branch: "trunk" },
        exhausted: false,
        next_cursor: "older-page",
        items: [
          {
            actor: "octocat",
            conclusion: "success",
            created_at: "2026-08-27T12:30:00Z",
            event: "workflow_dispatch",
            head_sha: "0123456789abcdef",
            id: `${name}-run-1`,
            name: `${name} deploy`,
            ref: "feature/recent-run",
            run_number: 7,
            status: "completed",
            workflow_id: `${name}-deploy.yml`,
          },
          {
            actor: "hubot",
            conclusion: "success",
            created_at: "2026-08-27T11:30:00Z",
            event: "workflow_dispatch",
            head_sha: "fedcba9876543210",
            id: `${name}-run-2`,
            name: `${name} deploy`,
            ref: "tag/v2",
            run_number: 6,
            status: "completed",
            workflow_id: `${name}-deploy.yml`,
          },
        ],
      });
    }
    const jobs = request.url.pathname.match(/^\/api\/v1\/actions\/github\/acme\/([^/]+)\/runs\/([^/]+)\/jobs$/);
    if (request.method === "GET" && jobs) {
      const runId = jobs[2]!;
      return jsonResponse({
        repo: repoSummary(jobs[1]!).repo,
        items: [
          {
            id: `${runId}-job`,
            name: runId.endsWith("run-2") ? "Verify" : "Publish",
            status: "completed",
            conclusion: "success",
            steps: [],
          },
        ],
      });
    }
    return null;
  };
}

describe("ActionsPage", () => {
  let runtime: OwnedAppRuntime;
  let api: MockApiHandle;
  let originalFetch: typeof globalThis.fetch;

  beforeEach(() => {
    originalFetch = globalThis.fetch;
    api = createMockApiFetch([workflowFixtures()]);
    globalThis.fetch = api.fetch;
    runtime = makeAppRuntime();
    runtimeHolder.value = runtime;
    setGlobalRepo("github|github.com/acme/alpha,github|github.com/acme/beta,github|github.com/acme/legacy");
  });

  afterEach(async () => {
    cleanup();
    setGlobalRepo(undefined);
    globalThis.fetch = originalFetch;
    runtimeHolder.value = undefined;
    await Effect.runPromise(runtime.disposeEffect);
  });

  it("filters repository summaries, distinguishes unsupported repos, and demands only the selected capable repo", async () => {
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    const rail = await screen.findByRole("navigation", { name: "Actions repositories" });
    expect(within(rail).getByRole("button", { name: /alpha/ }).getAttribute("aria-current")).toBe("true");
    expect(within(rail).getByRole("button", { name: /beta/ })).toBeTruthy();
    expect(within(rail).getByLabelText("acme/legacy does not support workflow Actions")).toBeTruthy();
    expect(screen.queryByText("filtered-out")).toBeNull();

    await waitFor(() => {
      const paths = api.requests.map((request) => request.url.pathname);
      expect(paths).toContain("/api/v1/actions/github/acme/alpha/workflows");
      expect(paths.some((path) => path.includes("/legacy/"))).toBe(false);
    });
  });

  it("uses the shared workflow form and lazy run jobs for the selected repository", async () => {
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    expect(screen.getByRole("textbox", { name: "Git ref" })).toBeTruthy();
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("trunk");

    const run = await screen.findByRole("button", { name: /Run 7 alpha deploy/ });
    await fireEvent.click(run);
    expect(await screen.findByRole("button", { name: /Publish/ })).toBeTruthy();
    expect(api.requests.map((request) => request.url.pathname)).toContain(
      "/api/v1/actions/github/acme/alpha/runs/alpha-run-1/jobs",
    );
  });

  it("reads jobs once per expanded run and keeps other expanded runs open on collapse", async () => {
    const workflowActions = createWorkflowActionsStore({ runtime });
    const loadJobs = vi.spyOn(workflowActions, "loadJobs");
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    const newest = await screen.findByRole("button", { name: /Run 7 alpha deploy/ });
    const older = await screen.findByRole("button", { name: /Run 6 alpha deploy/ });
    await fireEvent.click(newest);
    await fireEvent.click(older);
    await screen.findByRole("button", { name: /Verify/ });
    expect(loadJobs.mock.calls.map(([, runId]) => runId)).toEqual(["alpha-run-1", "alpha-run-2"]);

    await fireEvent.click(newest);
    expect(loadJobs).toHaveBeenCalledTimes(2);
    expect(older.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: /Verify/ })).toBeTruthy();
  });

  it("resets runs and jobs when switching workflows without refetching old jobs", async () => {
    const twoWorkflows: MockRouteOverride = (request) => {
      if (request.method !== "GET" || request.url.pathname !== "/api/v1/actions/github/acme/alpha/workflows")
        return null;
      return jsonResponse({
        repo: { ...repoSummary("alpha").repo, default_branch: "trunk" },
        environments: [],
        workflows: [
          {
            id: "alpha-deploy.yml",
            name: "alpha deploy",
            path: ".github/workflows/alpha-deploy.yml",
            state: "active",
            available: true,
            definition_sha: "alpha-definition",
            inputs: [],
            web_url: "https://github.com/acme/alpha/actions/workflows/alpha-deploy.yml",
          },
          {
            id: "alpha-verify.yml",
            name: "alpha verify",
            path: ".github/workflows/alpha-verify.yml",
            state: "active",
            available: true,
            definition_sha: "alpha-verify-definition",
            inputs: [],
            web_url: "https://github.com/acme/alpha/actions/workflows/alpha-verify.yml",
          },
        ],
      });
    };
    api = createMockApiFetch([twoWorkflows, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    await fireEvent.click(await screen.findByRole("button", { name: /Run 7 alpha deploy/ }));
    await screen.findByRole("button", { name: /Publish/ });
    const jobReads = api.requests.filter((request) => request.url.pathname.endsWith("/jobs")).length;

    await fireEvent.click(screen.getByRole("button", { name: /alpha verify/ }));
    await waitFor(() => {
      const runReads = api.requests.filter((request) => request.url.pathname.endsWith("/runs"));
      expect(runReads.at(-1)?.url.searchParams.get("workflow_id")).toBe("alpha-verify.yml");
    });
    expect(screen.queryByRole("button", { name: /Publish/ })).toBeNull();
    expect(api.requests.filter((request) => request.url.pathname.endsWith("/jobs"))).toHaveLength(jobReads);
  });

  it("loads an older run page without changing the repository default ref seed", async () => {
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("trunk");
    await fireEvent.click(await screen.findByRole("button", { name: "Load more runs" }));
    expect(await screen.findByRole("button", { name: /Run 5 alpha deploy/ })).toBeTruthy();
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("trunk");
    const olderRequest = api.requests.find(
      (request) =>
        request.url.pathname.endsWith("/actions/github/acme/alpha/runs") &&
        request.url.searchParams.get("cursor") === "older-page",
    );
    expect(olderRequest).toBeTruthy();
  });

  it("renders an empty runs read failure as an error instead of a successful empty state", async () => {
    const runsFailure: MockRouteOverride = (request) => {
      if (request.method !== "GET" || request.url.pathname !== "/api/v1/actions/github/acme/alpha/runs") return null;
      return jsonResponse(
        {
          code: "internalError",
          detail: "Recent workflow runs could not be loaded.",
          status: 500,
        },
        500,
      );
    };
    api = createMockApiFetch([runsFailure, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });
    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));

    expect((await screen.findByRole("alert")).textContent).toContain("Recent workflow runs could not be loaded.");
    expect(screen.queryByText("No recent workflow runs.")).toBeNull();
  });

  it("keeps retained runs visible while surfacing a lazy jobs read failure as degraded", async () => {
    const jobsFailure: MockRouteOverride = (request) => {
      if (
        request.method !== "GET" ||
        request.url.pathname !== "/api/v1/actions/github/acme/alpha/runs/alpha-run-1/jobs"
      )
        return null;
      return jsonResponse(
        {
          code: "internalError",
          detail: "Workflow jobs could not be loaded.",
          status: 500,
        },
        500,
      );
    };
    api = createMockApiFetch([jobsFailure, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });
    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));

    const run = await screen.findByRole("button", { name: /Run 7 alpha deploy/ });
    await fireEvent.click(run);

    expect((await screen.findByRole("alert")).textContent).toContain("Workflow jobs could not be loaded.");
    expect(screen.getByRole("button", { name: /Run 7 alpha deploy/ })).toBe(run);
    expect(run.getAttribute("aria-expanded")).toBe("true");
  });

  it("reloads a changed workflow definition once after conflict without replaying dispatch", async () => {
    let catalogReads = 0;
    const conflictRecovery: MockRouteOverride = (request) => {
      if (request.method === "GET" && request.url.pathname === "/api/v1/actions/github/acme/alpha/workflows") {
        catalogReads += 1;
        return jsonResponse({
          repo: { ...repoSummary("alpha").repo, default_branch: "trunk" },
          environments: [],
          workflows: [
            {
              id: "alpha-deploy.yml",
              name: "alpha deploy",
              path: ".github/workflows/alpha-deploy.yml",
              state: "active",
              available: true,
              definition_sha: catalogReads === 1 ? "alpha-definition" : "alpha-definition-2",
              inputs:
                catalogReads === 1
                  ? []
                  : [
                      {
                        name: "channel",
                        type: "choice",
                        required: true,
                        has_default: true,
                        default: "stable",
                        options: ["stable", "beta"],
                      },
                    ],
              web_url: "https://github.com/acme/alpha/actions/workflows/alpha-deploy.yml",
            },
          ],
        });
      }
      if (
        request.method === "POST" &&
        request.url.pathname.endsWith("/actions/github/acme/alpha/workflows/alpha-deploy.yml/dispatch")
      ) {
        return jsonResponse(
          {
            code: "conflict",
            detail: "Workflow definition changed.",
            details: { reason: "workflow_definition_changed" },
            status: 409,
            title: "Conflict",
            type: "about:blank",
          },
          409,
        );
      }
      return null;
    };
    api = createMockApiFetch([conflictRecovery, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Reload workflows" }));
    expect(await screen.findByRole("combobox", { name: "channel" })).toBeTruthy();
    expect(catalogReads).toBe(2);
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(1);
  });

  it.each([
    ["forbidden", 403],
    ["rateLimited", 429],
    ["validationError", 400],
  ] as const)("presents %s rejection as a fresh-cycle recovery without automatic POST", async (code, status) => {
    const rejection: MockRouteOverride = (request) => {
      if (
        request.method !== "POST" ||
        !request.url.pathname.endsWith("/actions/github/acme/alpha/workflows/alpha-deploy.yml/dispatch")
      ) {
        return null;
      }
      return jsonResponse(
        {
          code,
          detail: `Rejected with ${code}.`,
          status,
          title: "Rejected",
          type: "about:blank",
        },
        status,
      );
    };
    api = createMockApiFetch([rejection, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect((await screen.findByRole("alert")).textContent).toContain(`Rejected with ${code}.`);
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(1);
    await fireEvent.click(screen.getByRole("button", { name: "Run again" }));
    expect(await screen.findByRole("button", { name: "Run workflow" })).toBeTruthy();
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(1);
  });

  it("dispatches the same workflow twice only after two deliberate confirmations", async () => {
    let dispatches = 0;
    const accepted: MockRouteOverride = (request) => {
      if (
        request.method !== "POST" ||
        !request.url.pathname.endsWith("/actions/github/acme/alpha/workflows/alpha-deploy.yml/dispatch")
      ) {
        return null;
      }
      dispatches += 1;
      return jsonResponse(
        {
          accepted: true,
          dispatch_id: `dispatch-${dispatches}`,
          actor: "maintainer",
          run: {
            actor: "maintainer",
            conclusion: "success",
            event: "workflow_dispatch",
            head_sha: `head-${dispatches}`,
            id: `repeat-run-${dispatches}`,
            name: "alpha deploy",
            ref: "trunk",
            run_number: 10 + dispatches,
            status: "completed",
            web_url: `https://github.com/acme/alpha/actions/runs/${dispatches}`,
            workflow_id: "alpha-deploy.yml",
          },
        },
        202,
      );
    };
    api = createMockApiFetch([accepted, workflowFixtures()]);
    globalThis.fetch = api.fetch;
    const workflowActions = createWorkflowActionsStore({ runtime });
    render(ActionsPage, {
      context: new Map([[STORES_KEY, { workflowActions }]]),
    });

    await fireEvent.click(await screen.findByRole("button", { name: /alpha deploy/ }));
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(await screen.findByText("repeat-run-1")).toBeTruthy();
    expect(dispatches).toBe(1);

    await fireEvent.click(screen.getByRole("button", { name: "Run again" }));
    expect(await screen.findByRole("button", { name: "Run workflow" })).toBeTruthy();
    expect(dispatches).toBe(1);
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(await screen.findByText("repeat-run-2")).toBeTruthy();
    expect(dispatches).toBe(2);
  });
});
