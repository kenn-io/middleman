import { describe, expect, it } from "vite-plus/test";
import {
  activitySelectionToRoute,
  buildActivitySelectionSearch,
  parseActivitySelection,
  type ActivitySelection,
} from "./activitySelection";

const githubWidgets = {
  provider: "github",
  platformHost: "github.com",
  owner: "acme",
  name: "widgets",
  repoPath: "acme/widgets",
} as const;

const githubCommit = {
  itemType: "commit" as const,
  ...githubWidgets,
  branchName: "main",
  commitSha: "abcdef1234567890abcdef1234567890abcdef12",
  title: "Bump dependency",
};

describe("activity selection URL state", () => {
  it("parses PR conversation selection", () => {
    expect(
      parseActivitySelection("?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets"),
    ).toEqual({
      itemType: "pr",
      ...githubWidgets,
      number: 1,
      detailTab: "conversation",
    });
  });

  it("parses search strings without a leading question mark", () => {
    expect(
      parseActivitySelection("selected=issue:10&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets"),
    ).toEqual({
      itemType: "issue",
      ...githubWidgets,
      number: 10,
      detailTab: "conversation",
    });
  });

  it("parses PR files selection", () => {
    expect(
      parseActivitySelection(
        "?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&selected_tab=files",
      ),
    ).toEqual({
      itemType: "pr",
      ...githubWidgets,
      number: 1,
      detailTab: "files",
    });
  });

  it("parses provider selection with nested repo path", () => {
    expect(
      parseActivitySelection(
        "?selected=issue:11&provider=gitlab&platform_host=gitlab.example.com:8443&repo_path=Group%2FSubGroup%2FProject.Special",
      ),
    ).toEqual({
      itemType: "issue",
      owner: "Group/SubGroup",
      name: "Project.Special",
      number: 11,
      provider: "gitlab",
      platformHost: "gitlab.example.com:8443",
      repoPath: "Group/SubGroup/Project.Special",
      detailTab: "conversation",
    });
  });

  it("parses a commit selection with its branch", () => {
    expect(
      parseActivitySelection(
        "?selected=commit:abcdef1234567890abcdef1234567890abcdef12&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&branch=main",
      ),
    ).toEqual({ ...githubCommit, title: "abcdef123456" });
  });

  it("uses the default branch when a commit selection has no branch", () => {
    expect(
      parseActivitySelection(
        "?selected=commit:abcdef1234567890abcdef1234567890abcdef12&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets",
      ),
    ).toEqual({ ...githubCommit, branchName: "default branch", title: "abcdef123456" });
  });

  it("parses a full SHA-256 commit selection", () => {
    const commitSha = "a".repeat(64);

    expect(
      parseActivitySelection(
        `?selected=commit:${commitSha}&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&branch=main`,
      ),
    ).toEqual({ ...githubCommit, commitSha, title: "aaaaaaaaaaaa" });
  });

  it("rejects an invalid commit SHA", () => {
    expect(
      parseActivitySelection("?selected=commit:zzz&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets"),
    ).toBeNull();
  });

  it("rejects an abbreviated commit SHA", () => {
    expect(
      parseActivitySelection(
        "?selected=commit:abcdef1234567890&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets",
      ),
    ).toBeNull();
  });

  it("round-trips every commit identity field", () => {
    const search = buildActivitySelectionSearch("?range=30d", githubCommit);

    expect(parseActivitySelection(`?${search.toString()}`)).toEqual({ ...githubCommit, title: "abcdef123456" });
  });

  it("preserves filters when writing a commit selection", () => {
    const search = buildActivitySelectionSearch("?item_types=commit&range=30d", githubCommit);

    expect(search.get("item_types")).toBe("commit");
    expect(search.get("range")).toBe("30d");
  });

  it("preserves existing Activity filters when writing selection", () => {
    const next = buildActivitySelectionSearch("?range=30d&view=threaded", {
      itemType: "pr",
      ...githubWidgets,
      number: 1,
      detailTab: "files",
    });

    expect(next.toString()).toBe(
      "range=30d&view=threaded&selected=pr%3A1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&selected_tab=files",
    );
  });

  it("clears selection-only params without clearing filters", () => {
    const next = buildActivitySelectionSearch(
      "?range=30d&selected=issue:10&provider=github&platform_host=ghe.example.com&repo_path=acme%2Fwidgets",
      null,
    );

    expect(next.toString()).toBe("range=30d");
  });

  it("writes provider selection with repo path outside selected value", () => {
    const next = buildActivitySelectionSearch("?range=30d", {
      itemType: "pr",
      owner: "Group/SubGroup",
      name: "Project.Special",
      number: 7,
      provider: "gitlab",
      platformHost: "gitlab.example.com:8443",
      repoPath: "Group/SubGroup/Project.Special",
      detailTab: "files",
    });

    expect(next.get("selected")).toBe("pr:7");
    expect(next.get("provider")).toBe("gitlab");
    expect(next.get("platform_host")).toBe("gitlab.example.com:8443");
    expect(next.get("repo_path")).toBe("Group/SubGroup/Project.Special");
    expect(next.get("selected_tab")).toBe("files");
    expect(next.get("range")).toBe("30d");
  });

  it("overwrites one PR selection with another and drops stale files tab", () => {
    const next = buildActivitySelectionSearch(
      "?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&selected_tab=files&range=7d",
      {
        itemType: "pr",
        provider: "github",
        platformHost: "github.com",
        owner: "octo",
        name: "tools",
        repoPath: "octo/tools",
        number: 2,
        detailTab: "conversation",
      },
    );

    expect(next.get("selected")).toBe("pr:2");
    expect(next.get("repo_path")).toBe("octo/tools");
    expect(next.get("range")).toBe("7d");
    expect(next.has("selected_tab")).toBe(false);
  });

  it("replaces a PR selection with a commit without stale item fields", () => {
    const next = buildActivitySelectionSearch(
      "?selected=pr:1&selected_tab=files&provider=github&repo_path=acme%2Fwidgets",
      githubCommit,
    );

    expect(next.get("selected")).toBe("commit:abcdef1234567890abcdef1234567890abcdef12");
    expect(next.get("branch")).toBe("main");
    expect(next.has("selected_tab")).toBe(false);
  });

  it("replaces a commit selection with a PR without a stale branch", () => {
    const next = buildActivitySelectionSearch(
      "?selected=commit:abcdef1234567890abcdef1234567890abcdef12&branch=main&provider=github&repo_path=acme%2Fwidgets",
      {
        itemType: "pr",
        ...githubWidgets,
        number: 1,
        detailTab: "conversation",
      },
    );

    expect(next.get("selected")).toBe("pr:1");
    expect(next.has("branch")).toBe(false);
  });

  it("overwrites an issue selection with a PR and drops platform host", () => {
    const next = buildActivitySelectionSearch(
      "?selected=issue:10&provider=github&platform_host=ghe.example.com&repo_path=acme%2Fwidgets&search=bug",
      {
        itemType: "pr",
        provider: "github",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
        number: 1,
        detailTab: "conversation",
      },
    );

    expect(next.get("selected")).toBe("pr:1");
    expect(next.get("search")).toBe("bug");
    expect(next.has("platform_host")).toBe(false);
  });

  it("builds destination routes for matching tabs", () => {
    const pr: ActivitySelection = {
      itemType: "pr",
      ...githubWidgets,
      number: 1,
      detailTab: "files",
    };

    expect(activitySelectionToRoute(pr, "pulls")).toBe("/pulls/github/acme/widgets/1/files");
    expect(activitySelectionToRoute(pr, "issues")).toBeNull();
  });

  it("builds issue destination routes with platform host", () => {
    const issue: ActivitySelection = {
      itemType: "issue",
      ...githubWidgets,
      platformHost: "ghe.example.com",
      number: 10,
      detailTab: "conversation",
    };

    expect(activitySelectionToRoute(issue, "issues")).toBe("/host/ghe.example.com/issues/github/acme/widgets/10");
    expect(activitySelectionToRoute(issue, "pulls")).toBeNull();
  });

  it("does not build destination routes for a commit selection", () => {
    expect(activitySelectionToRoute(githubCommit, "pulls")).toBeNull();
    expect(activitySelectionToRoute(githubCommit, "issues")).toBeNull();
  });

  it.each([
    "",
    "?selected=garbage",
    "?selected=pr:foo",
    "?selected=foo:1&provider=github&repo_path=acme%2Fwidgets",
    "?selected=pr:1",
    "?selected=pr:acme/widgets/1&provider=github&repo_path=acme%2Fwidgets",
    "?selected_tab=files",
  ])("returns null for invalid selection %s", (search) => {
    expect(parseActivitySelection(search)).toBeNull();
  });
});
