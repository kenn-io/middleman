import { describe, expect, it } from "vite-plus/test";
import { providerActionsPath, type ProviderRouteRef } from "./provider-routes.js";

const github: ProviderRouteRef = {
  provider: "gh",
  platformHost: "github.com",
  owner: "octo",
  name: "repo",
  repoPath: "octo/repo",
};

const enterprise: ProviderRouteRef = {
  provider: "github",
  platformHost: "github.example.com",
  owner: "octo",
  name: "repo",
  repoPath: "octo/repo",
};

describe("providerActionsPath", () => {
  it("uses the provider-default route for the default host", () => {
    const routes = [
      providerActionsPath(github, "/workflows"),
      providerActionsPath(github, "/runs"),
      providerActionsPath(github, "/runs/{run_id}/jobs"),
      providerActionsPath(github, "/workflows/{workflow_id}/dispatch"),
    ];

    expect(routes).toEqual([
      "/actions/{provider}/{owner}/{name}/workflows",
      "/actions/{provider}/{owner}/{name}/runs",
      "/actions/{provider}/{owner}/{name}/runs/{run_id}/jobs",
      "/actions/{provider}/{owner}/{name}/workflows/{workflow_id}/dispatch",
    ]);
  });

  it("uses the host-prefixed route for a non-default host", () => {
    expect(providerActionsPath(enterprise, "/workflows")).toBe(
      "/host/{platform_host}/actions/{provider}/{owner}/{name}/workflows",
    );
  });
});
