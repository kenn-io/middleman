import { ProblemCodes } from "../../api/problems.js";
import { apiErrorMessage } from "../../api/runtime.js";
import type {
  WorkflowActionsError,
  WorkflowActionsSnapshot,
  WorkflowRun,
} from "../../stores/workflow-actions.svelte.js";

export type WorkflowDispatchPresentationState =
  | { readonly kind: "idle" }
  | { readonly kind: "pending" }
  | { readonly kind: "locating" }
  | { readonly kind: "succeeded"; readonly run?: WorkflowRun; readonly message?: string }
  | { readonly kind: "failed"; readonly message: string }
  | { readonly kind: "uncertain"; readonly message: string }
  | { readonly kind: "conflict"; readonly reloadError?: string };

const outcomeFallback = "The workflow outcome could not be confirmed.";
const reloadFallback = "Workflow data could not be refreshed.";

export function workflowActionsErrorMessage(error: WorkflowActionsError, fallback: string): string {
  if (error._tag === "ApiProblemError") {
    return apiErrorMessage(error.problem, fallback);
  }
  if (error.cause instanceof Error) return error.cause.message;
  return fallback;
}

export function workflowDispatchPresentation(
  snapshot: WorkflowActionsSnapshot | null,
  workflowId: string | null,
): WorkflowDispatchPresentationState {
  if (!workflowId) return { kind: "idle" };
  const dispatch = snapshot?.dispatches[workflowId];
  if (!dispatch) return { kind: "idle" };
  switch (dispatch.kind) {
    case "pending":
      return { kind: "pending" };
    case "locating":
      return { kind: "locating" };
    case "succeeded":
      return dispatch.run === undefined ? { kind: "succeeded" } : { kind: "succeeded", run: dispatch.run };
    case "unresolved":
      return { kind: "succeeded", message: "The provider accepted the workflow, but its run was not observed." };
    case "uncertain":
      return { kind: "uncertain", message: workflowActionsErrorMessage(dispatch.error, outcomeFallback) };
    case "failed": {
      const { error } = dispatch;
      if (
        error._tag === "ApiProblemError" &&
        error.problem.code === ProblemCodes.conflict &&
        error.problem.details?.["reason"] === "workflow_definition_changed"
      ) {
        const reloadError = snapshot?.catalogRefreshErrors[workflowId];
        return reloadError
          ? { kind: "conflict", reloadError: workflowActionsErrorMessage(reloadError, reloadFallback) }
          : { kind: "conflict" };
      }
      return { kind: "failed", message: workflowActionsErrorMessage(error, outcomeFallback) };
    }
  }
}
