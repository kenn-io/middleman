import { describe, expect, it } from "vite-plus/test";
import { parseConfiguredProviderItemURL } from "./item-reference.js";

describe("parseConfiguredProviderItemURL", () => {
  const repos = [
    { provider: "github", platform_host: "github.com" },
    { provider: "gh", platform_host: "github.com" },
    { provider: "gitlab", platform_host: "gitlab.example.com" },
    { provider: "forgejo", platform_host: "codeberg.org" },
  ];

  it("matches pull request and issue links on any configured provider host", () => {
    expect(parseConfiguredProviderItemURL("https://github.com/acme/widgets/pull/1028", repos)).toMatchObject({
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widgets",
      repoPath: "acme/widgets",
      number: 1028,
      itemType: "pr",
      externalUrl: "https://github.com/acme/widgets/pull/1028",
    });
    expect(
      parseConfiguredProviderItemURL("https://gitlab.example.com/group/sub/project/-/issues/9", repos),
    ).toMatchObject({
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      repoPath: "group/sub/project",
      number: 9,
      itemType: "issue",
    });
    expect(parseConfiguredProviderItemURL("https://codeberg.org/org/tool/pulls/4", repos)).toMatchObject({
      provider: "forgejo",
      repoPath: "org/tool",
      number: 4,
      itemType: "pr",
    });
  });

  it("ignores links on hosts without a configured repository and non-item pages", () => {
    expect(parseConfiguredProviderItemURL("https://gitlab.com/group/project/-/issues/9", repos)).toBeNull();
    expect(parseConfiguredProviderItemURL("https://github.com/acme/widgets/commit/abc123", repos)).toBeNull();
    expect(parseConfiguredProviderItemURL("https://github.com/acme/widgets/pull/1028", [])).toBeNull();
    expect(parseConfiguredProviderItemURL("not a url", repos)).toBeNull();
  });
});
