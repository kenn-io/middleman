import { describe, expect, it } from "vitest";

import type {
  WorkflowActionsError,
  WorkflowActionsSnapshot,
  WorkflowDispatchState,
} from "../../stores/workflow-actions.svelte.js";
import { workflowActionsErrorMessage, workflowDispatchPresentation } from "./workflow-dispatch-presentation.js";

const ref = {
  provider: "github",
  platformHost: "github.com",
  owner: "acme",
  name: "app",
  repoPath: "acme/app",
} as const;

const run = {
  actor: "maintainer",
  conclusion: "success",
  event: "workflow_dispatch",
  head_sha: "head-a",
  id: "run-1",
  name: "Deploy",
  ref: "main",
  run_number: 1,
  status: "completed",
  workflow_id: "deploy.yml",
} as const;

const rejected = {
  _tag: "ApiProblemError",
  operation: "POST workflow dispatch",
  problem: {
    code: "validationError",
    detail: "The ref is invalid.",
    status: 400,
    title: "Bad Request",
    type: "about:blank",
  },
} as WorkflowActionsError;

const conflict = {
  _tag: "ApiProblemError",
  operation: "POST workflow dispatch",
  problem: {
    code: "conflict",
    detail: "Definition changed.",
    details: { reason: "workflow_definition_changed" },
    status: 409,
    title: "Conflict",
    type: "about:blank",
  },
} as WorkflowActionsError;

const reloadFailure = {
  _tag: "TransientTransportError",
  operation: "GET workflow catalog",
  cause: new Error("Catalog transport failed."),
} as WorkflowActionsError;

function snapshot(dispatch?: WorkflowDispatchState): WorkflowActionsSnapshot {
  return {
    ref,
    catalog: null,
    selectedWorkflow: null,
    runs: [],
    runsPage: { nextCursor: null, exhausted: true, loadingMore: false },
    jobs: {},
    loading: { catalog: false, runs: false, jobs: [] },
    dispatches: dispatch ? { "deploy.yml": dispatch } : {},
    catalogRefreshErrors: {},
    error: null,
  };
}

describe("workflow dispatch presentation", () => {
  it("projects idle, pending, locating, and succeeded states", () => {
    expect(workflowDispatchPresentation(null, "deploy.yml")).toEqual({ kind: "idle" });
    expect(workflowDispatchPresentation(snapshot({ kind: "pending" }), "other.yml")).toEqual({ kind: "idle" });
    expect(workflowDispatchPresentation(snapshot({ kind: "pending" }), "deploy.yml")).toEqual({ kind: "pending" });
    expect(workflowDispatchPresentation(snapshot({ kind: "locating", dispatchId: "d1" }), "deploy.yml")).toEqual({
      kind: "locating",
    });
    expect(workflowDispatchPresentation(snapshot({ kind: "succeeded", dispatchId: "d1" }), "deploy.yml")).toEqual({
      kind: "succeeded",
    });
    expect(workflowDispatchPresentation(snapshot({ kind: "succeeded", dispatchId: "d1", run }), "deploy.yml")).toEqual({
      kind: "succeeded",
      run,
    });
  });

  it("projects failed, unresolved, and uncertain recovery branches", () => {
    expect(workflowDispatchPresentation(snapshot({ kind: "failed", error: rejected }), "deploy.yml")).toEqual({
      kind: "failed",
      message: "The ref is invalid.",
    });
    expect(workflowDispatchPresentation(snapshot({ kind: "unresolved", dispatchId: "d1" }), "deploy.yml")).toEqual({
      kind: "succeeded",
      message: "The provider accepted the workflow, but its run was not observed.",
    });
    expect(workflowDispatchPresentation(snapshot({ kind: "uncertain", error: rejected }), "deploy.yml")).toEqual({
      kind: "uncertain",
      message: "The ref is invalid.",
    });
  });

  it("uses the shared reload fallback and cycle-specific conflict error", () => {
    expect(workflowDispatchPresentation(snapshot({ kind: "failed", error: conflict }), "deploy.yml")).toEqual({
      kind: "conflict",
    });
    const current: WorkflowActionsSnapshot = {
      ...snapshot({ kind: "failed", error: conflict }),
      catalogRefreshErrors: { "deploy.yml": reloadFailure },
    };
    expect(workflowDispatchPresentation(current, "deploy.yml")).toEqual({
      kind: "conflict",
      reloadError: "Catalog transport failed.",
    });
    expect(workflowActionsErrorMessage(reloadFailure, "Could not reload workflows.")).toBe("Catalog transport failed.");
  });
});
