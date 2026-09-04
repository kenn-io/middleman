import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { makeAppRuntime, type OwnedAppRuntime } from "../app/runtime.js";
import {
  createMockApiFetch,
  jsonResponse,
  type MockApiHandle,
  type MockRouteOverride,
} from "../../test/mockApiFetch.js";
import { WorkflowDispatchProgressEvent } from "./provider-events-workflow.js";
import { createWorkflowActionsStore, type WorkflowActionsStore } from "./workflow-actions.svelte.js";

const ref = { provider: "github", platformHost: "github.com", owner: "acme", name: "app", repoPath: "acme/app" };
const otherRef = { ...ref, name: "other", repoPath: "acme/other" };

const repo = {
  provider: "github",
  platform_host: "github.com",
  owner: "acme",
  name: "app",
  repo_path: "acme/app",
  default_branch: "main",
  capabilities: {},
  operations: { dispatch_workflow: { available: true } },
};

function run(id: string, status: string, conclusion = "") {
  return {
    actor: "maintainer",
    conclusion,
    created_at: "2026-08-27T12:30:00Z",
    event: "workflow_dispatch",
    head_sha: `head-${id}`,
    id,
    name: "Deploy",
    ref: "main",
    run_number: 1,
    status,
    workflow_id: "deploy.yml",
  };
}

interface Fixture {
  catalogReads: number;
  runReads: string[];
  dispatchResponse: () => Response;
}

function routes(fixture: Fixture): MockRouteOverride {
  return (request) => {
    const path = request.url.pathname;
    if (request.method === "GET" && path === "/api/v1/actions/github/acme/app/workflows") {
      fixture.catalogReads += 1;
      return jsonResponse({
        repo,
        environments: [],
        workflows: [
          {
            id: "deploy.yml",
            name: "Deploy",
            path: ".github/workflows/deploy.yml",
            state: "active",
            available: true,
            definition_sha: `definition-${fixture.catalogReads}`,
            inputs: [],
          },
        ],
      });
    }
    if (request.method === "GET" && path === "/api/v1/actions/github/acme/app/runs") {
      fixture.runReads.push(request.url.searchParams.get("workflow_id") ?? "");
      return jsonResponse({ repo, exhausted: true, items: [run("run-old", "completed", "success")] });
    }
    if (request.method === "POST" && path.endsWith("/workflows/deploy.yml/dispatch")) {
      return fixture.dispatchResponse();
    }
    return null;
  };
}

function progress(
  status: "located" | "updated" | "unresolved",
  dispatchId: string,
  runPayload?: ReturnType<typeof run>,
): WorkflowDispatchProgressEvent {
  return new WorkflowDispatchProgressEvent({
    provider: "github",
    platform_host: "github.com",
    repo_path: "acme/app",
    owner: "acme",
    name: "app",
    workflow_id: "deploy.yml",
    dispatch_id: dispatchId,
    status,
    ...(runPayload !== undefined && { run: runPayload }),
  });
}

async function settle(): Promise<void> {
  for (let index = 0; index < 4; index += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

describe("workflow actions store", () => {
  let runtime: OwnedAppRuntime;
  let api: MockApiHandle;
  let originalFetch: typeof globalThis.fetch;
  let fixture: Fixture;
  let store: WorkflowActionsStore;

  beforeEach(() => {
    originalFetch = globalThis.fetch;
    fixture = {
      catalogReads: 0,
      runReads: [],
      dispatchResponse: () => jsonResponse({ accepted: true, dispatch_id: "dispatch-1", actor: "maintainer" }, 202),
    };
    api = createMockApiFetch([routes(fixture)]);
    globalThis.fetch = api.fetch;
    runtime = makeAppRuntime();
    store = createWorkflowActionsStore({ runtime });
  });

  afterEach(async () => {
    globalThis.fetch = originalFetch;
    await Effect.runPromise(runtime.disposeEffect);
  });

  it("reads the catalog once per repository and runs only for the selected workflow", async () => {
    store.loadCatalog(ref);
    store.loadCatalog(ref);
    await settle();
    store.loadCatalog(ref);
    await settle();
    expect(fixture.catalogReads).toBe(1);
    expect(store.getCatalog(ref)?.workflows?.[0]?.id).toBe("deploy.yml");
    expect(fixture.runReads).toEqual([]);

    store.selectWorkflow(ref, "deploy.yml");
    await settle();
    expect(fixture.runReads).toEqual(["deploy.yml"]);
    expect(store.getRuns(ref).map((item) => item.id)).toEqual(["run-old"]);
  });

  it("moves a dispatch from pending to locating and lets server progress events finish it", async () => {
    store.loadCatalog(ref);
    await settle();
    store.selectWorkflow(ref, "deploy.yml");
    await settle();

    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "pending" });
    await settle();
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "locating", dispatchId: "dispatch-1" });
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(1);

    store.applyDispatchProgress(progress("located", "dispatch-1", run("run-new", "queued")));
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({
      kind: "succeeded",
      dispatchId: "dispatch-1",
      run: run("run-new", "queued"),
    });
    expect(store.getRuns(ref).map((item) => item.id)).toEqual(["run-new", "run-old"]);

    store.applyDispatchProgress(progress("updated", "dispatch-1", run("run-new", "completed", "success")));
    expect(store.getRuns(ref)[0]?.conclusion).toBe("success");
    expect(store.getDispatch(ref, "deploy.yml")).toMatchObject({ kind: "succeeded", run: { conclusion: "success" } });
    expect(fixture.runReads).toEqual(["deploy.yml"]);

    store.newDispatchCycle(ref, "deploy.yml");
    expect(store.getDispatch(ref, "deploy.yml")).toBeNull();
    expect(store.getRuns(ref)).toHaveLength(2);
  });

  it("keeps the listed run when the response names only an id and applies progress that arrived first", async () => {
    fixture.dispatchResponse = () =>
      jsonResponse(
        {
          accepted: true,
          dispatch_id: "dispatch-1",
          actor: "maintainer",
          run: { ...run("run-new", ""), run_number: 0, name: "", conclusion: "" },
        },
        202,
      );
    store.loadCatalog(ref);
    await settle();
    store.selectWorkflow(ref, "deploy.yml");
    await settle();

    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    store.applyDispatchProgress(progress("located", "dispatch-1", run("run-new", "in_progress")));
    expect(store.getRuns(ref)[0]).toEqual(run("run-new", "in_progress"));
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "pending" });

    await settle();
    expect(store.getRuns(ref)[0]).toEqual(run("run-new", "in_progress"));
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({
      kind: "succeeded",
      dispatchId: "dispatch-1",
      run: run("run-new", "in_progress"),
    });
  });

  it("reports an unresolved dispatch and ignores progress for other dispatches or repositories", async () => {
    store.loadCatalog(ref);
    await settle();
    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    await settle();

    store.applyDispatchProgress(progress("located", "someone-elses-dispatch", run("run-x", "queued")));
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "locating", dispatchId: "dispatch-1" });

    store.applyDispatchProgress({
      ...progress("located", "dispatch-1", run("run-x", "queued")),
      name: "other",
    } as WorkflowDispatchProgressEvent);
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "locating", dispatchId: "dispatch-1" });
    expect(store.getSnapshot(otherRef)).toBeNull();

    store.applyDispatchProgress(progress("unresolved", "dispatch-1"));
    expect(store.getDispatch(ref, "deploy.yml")).toEqual({ kind: "unresolved", dispatchId: "dispatch-1" });
  });

  it("distinguishes rejected dispatches from uncertain outcomes and never replays the POST", async () => {
    fixture.dispatchResponse = () =>
      jsonResponse({ code: "forbidden", detail: "No.", status: 403, title: "Forbidden", type: "about:blank" }, 403);
    store.loadCatalog(ref);
    await settle();
    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    await settle();
    expect(store.getDispatch(ref, "deploy.yml")).toMatchObject({ kind: "failed", error: { _tag: "ApiProblemError" } });

    store.newDispatchCycle(ref, "deploy.yml");
    fixture.dispatchResponse = () =>
      jsonResponse(
        { code: "mutationOutcomeUnknown", detail: "Timed out.", status: 502, title: "Unknown", type: "about:blank" },
        502,
      );
    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    await settle();
    expect(store.getDispatch(ref, "deploy.yml")).toMatchObject({ kind: "uncertain" });
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(2);
  });

  it("refreshes the catalog after a definition conflict and clears that workflow's cycle", async () => {
    fixture.dispatchResponse = () =>
      jsonResponse(
        {
          code: "conflict",
          detail: "Changed.",
          details: { reason: "workflow_definition_changed" },
          status: 409,
          title: "Conflict",
          type: "about:blank",
        },
        409,
      );
    store.loadCatalog(ref);
    await settle();
    store.dispatch({
      ref,
      workflowId: "deploy.yml",
      expectedDefinitionSha: "definition-1",
      dispatchRef: "main",
      inputs: {},
    });
    await settle();
    expect(store.getDispatch(ref, "deploy.yml")).toMatchObject({ kind: "failed" });

    store.refreshCatalog(ref, "deploy.yml");
    await settle();
    expect(fixture.catalogReads).toBe(2);
    expect(store.getCatalog(ref)?.workflows?.[0]?.definition_sha).toBe("definition-2");
    expect(store.getDispatch(ref, "deploy.yml")).toBeNull();
    expect(api.requests.filter((request) => request.method === "POST")).toHaveLength(1);
  });

  it("drops all repository state when Actions is disabled", async () => {
    store.loadCatalog(ref);
    await settle();
    expect(store.getCatalog(ref)).not.toBeNull();
    store.setEnabled(false);
    expect(store.getSnapshot(ref)).toBeNull();
    store.loadCatalog(ref);
    await settle();
    expect(fixture.catalogReads).toBe(1);
    const spy = vi.fn();
    spy(store.getCatalog(ref));
    expect(spy).toHaveBeenCalledWith(null);
  });
});
