import { Effect } from "effect";
import { InvalidExternalPayload } from "./effect-errors.js";
import { executeGeneratedApiRequest } from "./generated-api.js";
import type { PullRequest } from "./types.js";

export const createPullRequestWorkspace = Effect.fn("Onboarding.createPullRequestWorkspace")(function* (
  pull: PullRequest,
) {
  const data = yield* executeGeneratedApiRequest("create pull request workspace", (client, signal) =>
    client.WorkspacesService.createWorkspace(
      {
        provider: pull.repo.provider,
        platform_host: pull.repo.platform_host,
        owner: pull.repo.owner,
        name: pull.repo.name,
        mr_number: pull.Number,
      },
      { signal },
    ),
  );
  if (!data.id) {
    return yield* Effect.fail(
      InvalidExternalPayload.make({
        operation: "decode created pull request workspace",
        cause: new Error("workspace response did not include an id"),
      }),
    );
  }
  return {
    id: data.id,
    status: data.status,
  };
});
