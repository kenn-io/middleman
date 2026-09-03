import { assert, describe, it } from "@effect/vitest";
import { Effect } from "effect";
import { makeGeneratedApiLayer } from "./generated-api.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import type { PullRequest } from "./types.js";

import { createPullRequestWorkspace } from "./onboarding.ts";

describe("onboarding API", () => {
  it.effect("creates a workspace with the pull request's full provider identity", () => {
    let receivedBody: unknown;
    const createWorkspace = async (body: unknown) => {
      receivedBody = body;
      return { id: "ws-42", status: "provisioning" };
    };
    const pull = {
      Number: 42,
      repo: {
        provider: "github",
        platform_host: "ghe.example.com",
        owner: "acme",
        name: "forge",
        repo_path: "acme/forge",
      },
    } as PullRequest;

    return Effect.gen(function* () {
      const workspace = yield* createPullRequestWorkspace(pull);

      assert.deepStrictEqual(workspace, { id: "ws-42", status: "provisioning" });
      assert.deepStrictEqual(receivedBody, {
        provider: "github",
        platform_host: "ghe.example.com",
        owner: "acme",
        name: "forge",
        mr_number: 42,
      });
    }).pipe(Effect.provide(makeGeneratedApiLayer(makeGeneratedClient({ WorkspacesService: { createWorkspace } }))));
  });
});
