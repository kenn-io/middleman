import { describe, expect, it, beforeEach, afterEach, vi } from "vite-plus/test";
import {
  navigate,
  replaceUrl,
  getRoute,
  getDetailTab,
  isDiffView,
  getSelectedPRFromRoute,
  getPage,
  getLastActivityRoute,
  getLastWorkspaceRoute,
  forgetWorkspaceRoute,
  buildMobileWorkspaceRoute,
  buildMobileWorkspaceItemRoute,
} from "./router.svelte.js";

const prRoute = "/pulls/github/acme/widgets/42";
const prFilesRoute = "/pulls/github/acme/widgets/42/files";
const focusPrRoute = "/focus/pulls/github/acme/widgets/42";
const focusPrFilesRoute = "/focus/pulls/github/acme/widgets/42/files";
const prRef = {
  provider: "github",
  platformHost: "github.com",
  owner: "acme",
  name: "widgets",
  repoPath: "acme/widgets",
  number: 42,
};

describe("router /pulls files route", () => {
  beforeEach(() => {
    navigate("/pulls");
  });

  it("parses provider pull files route as list view with files tab", () => {
    navigate(prFilesRoute);
    expect(getRoute()).toEqual({
      page: "pulls",
      view: "list",
      selected: prRef,
      tab: "files",
    });
  });

  it("getDetailTab returns files for files route", () => {
    navigate(prFilesRoute);
    expect(getDetailTab()).toBe("files");
  });

  it("getDetailTab returns conversation for non-files PR route", () => {
    navigate(prRoute);
    expect(getDetailTab()).toBe("conversation");
  });

  it("isDiffView returns true for files route", () => {
    navigate(prFilesRoute);
    expect(isDiffView()).toBe(true);
  });

  it("isDiffView returns false for conversation route", () => {
    navigate(prRoute);
    expect(isDiffView()).toBe(false);
  });

  it("getSelectedPRFromRoute returns PR for files route", () => {
    navigate(prFilesRoute);
    expect(getSelectedPRFromRoute()).toEqual(prRef);
  });

  it("getSelectedPRFromRoute returns PR for conversation route", () => {
    navigate(prRoute);
    expect(getSelectedPRFromRoute()).toEqual(prRef);
  });

  it("getSelectedPRFromRoute returns null for pull list without selection", () => {
    navigate("/pulls");
    expect(getSelectedPRFromRoute()).toBeNull();
  });

  it("parses focus provider pull files route as focused PR with files tab", () => {
    navigate(focusPrFilesRoute);
    expect(getRoute()).toEqual({
      page: "focus",
      itemType: "pr",
      ...prRef,
      tab: "files",
    });
  });

  it("parses hosted focus provider pull files route as focused PR with files tab", () => {
    navigate("/focus/host/ghe.example.com/pulls/github/acme/widgets/42/files");
    expect(getRoute()).toEqual({
      page: "focus",
      itemType: "pr",
      provider: "github",
      platformHost: "ghe.example.com",
      owner: "acme",
      name: "widgets",
      repoPath: "acme/widgets",
      number: 42,
      tab: "files",
    });
  });

  it("focus PR files route participates in detail-tab helpers", () => {
    navigate(focusPrFilesRoute);
    expect(getDetailTab()).toBe("files");
    expect(isDiffView()).toBe(true);
  });

  it("focus PR conversation route remains the conversation tab", () => {
    navigate(focusPrRoute);
    expect(getDetailTab()).toBe("conversation");
    expect(isDiffView()).toBe(false);
  });

  it("getPage returns pulls for files route", () => {
    navigate(prFilesRoute);
    expect(getPage()).toBe("pulls");
  });
});

describe("router basic routes", () => {
  it("parses local and Fleet mobile workspace routes", () => {
    navigate("/m/workspaces");
    expect(getRoute()).toEqual({ page: "mobile-workspaces" });

    navigate("/m/workspaces/local/ws-1");
    expect(getRoute()).toEqual({
      page: "mobile-workspace-terminal",
      workspaceId: "ws-1",
    });

    navigate("/m/workspaces/fleet/laptop/ws-2/item/files");
    expect(getRoute().page).not.toBe("mobile-workspace-item");

    navigate("/m/workspaces/local/itemized-workspace");
    expect(getRoute()).toEqual({
      page: "mobile-workspace-terminal",
      workspaceId: "itemized-workspace",
    });

    navigate("/m/workspaces/fleet/itemized-host/ws-3");
    expect(getRoute()).toEqual({
      page: "mobile-workspace-terminal",
      workspaceId: "ws-3",
      hostKey: "itemized-host",
    });
  });

  it("builds escaped local and Fleet mobile workspace routes", () => {
    expect(buildMobileWorkspaceRoute("ws/one")).toBe("/m/workspaces/local/ws%2Fone");
    expect(buildMobileWorkspaceRoute("ws two", "dev/laptop")).toBe("/m/workspaces/fleet/dev%2Flaptop/ws%20two");
    expect(buildMobileWorkspaceItemRoute("ws two", "dev/laptop", "files")).toBe(
      "/m/workspaces/fleet/dev%2Flaptop/ws%20two/item/files",
    );
  });

  it("parses /design-system", () => {
    navigate("/design-system");
    expect(getRoute()).toEqual({ page: "design-system" });
    expect(getPage()).toBe("design-system");
  });

  it("parses and navigates to the top-level Actions workspace", () => {
    navigate("/actions");

    expect(getRoute()).toEqual({ page: "actions" });
    expect(getPage()).toBe("actions");
    expect(window.location.pathname + window.location.search).toBe("/actions");
  });

  it("parses /pulls as list view", () => {
    navigate("/pulls");
    expect(getRoute()).toEqual({ page: "pulls", view: "list" });
  });

  it("does not recognize /pulls/board as a separate view", () => {
    navigate("/pulls/board");
    expect(getRoute()).toEqual({ page: "pulls", view: "list" });
  });

  it("does not parse legacy owner/name pull detail routes", () => {
    navigate("/pulls/org/repo/7");
    expect(getRoute()).toEqual({ page: "pulls", view: "list" });
  });

  it("does not parse old query-param pull detail routes", () => {
    navigate("/pulls/detail?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&number=42");
    expect(getRoute()).toEqual({ page: "pulls", view: "list" });
  });

  it("parses provider pull routes with escaped nested repo paths", () => {
    navigate("/host/gitlab.example.com%3A8443/pulls/gitlab/Group%2FSubGroup%2FSubGroup%202/My_Project.v2/12");

    expect(getRoute()).toEqual({
      page: "pulls",
      view: "list",
      selected: {
        owner: "Group/SubGroup/SubGroup 2",
        name: "My_Project.v2",
        provider: "gitlab",
        platformHost: "gitlab.example.com:8443",
        repoPath: "Group/SubGroup/SubGroup 2/My_Project.v2",
        number: 12,
      },
    });
  });

  it("uses public Forgejo and Gitea hosts for provider pull routes", () => {
    navigate("/pulls/forgejo/forgejo/forgejo/12/files");
    expect(getRoute()).toEqual({
      page: "pulls",
      view: "list",
      selected: {
        owner: "forgejo",
        name: "forgejo",
        provider: "forgejo",
        platformHost: "codeberg.org",
        repoPath: "forgejo/forgejo",
        number: 12,
      },
      tab: "files",
    });

    navigate("/pulls/gitea/gitea/tea/3");
    expect(getRoute()).toEqual({
      page: "pulls",
      view: "list",
      selected: {
        owner: "gitea",
        name: "tea",
        provider: "gitea",
        platformHost: "gitea.com",
        repoPath: "gitea/tea",
        number: 3,
      },
    });
  });

  it("parses provider pull files routes with escaped nested repo paths", () => {
    navigate("/host/gitlab.example.com%3A8443/pulls/gitlab/Group%2FSubGroup%2FSubGroup%202/My_Project.v2/12/files");

    expect(getRoute()).toEqual({
      page: "pulls",
      view: "list",
      selected: {
        owner: "Group/SubGroup/SubGroup 2",
        name: "My_Project.v2",
        provider: "gitlab",
        platformHost: "gitlab.example.com:8443",
        repoPath: "Group/SubGroup/SubGroup 2/My_Project.v2",
        number: 12,
      },
      tab: "files",
    });
  });

  it("parses / as activity", () => {
    navigate("/");
    expect(getRoute()).toEqual({ page: "activity" });
  });

  it("parses /repos", () => {
    navigate("/repos");
    expect(getRoute()).toEqual({ page: "repos" });
  });

  it("parses /repos/", () => {
    navigate("/repos/");
    expect(getRoute()).toEqual({ page: "repos" });
  });

  it("parses repo browser routes with selected ref, path, and view mode", () => {
    navigate(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=Group%2FSub%20Team%2FProject&ref_type=branch&ref_name=feature%2Frepo-browser&ref_sha=abc123&path=docs%2FREADME.md&mode=preview",
    );

    expect(getRoute()).toEqual({
      page: "repo-browser",
      provider: "gitlab",
      platformHost: "gitlab.example.com",
      repoPath: "Group/Sub Team/Project",
      owner: "Group/Sub Team",
      name: "Project",
      refType: "branch",
      refName: "feature/repo-browser",
      refSHA: "abc123",
      path: "docs/README.md",
      mode: "preview",
    });
    expect(getPage()).toBe("repo-browser");
  });

  it("parses repo browser routes without letting URL fragments corrupt query params", () => {
    navigate("/repo/browser?provider=github&repo_path=acme%2Fwidgets&path=docs%2Fguide.md&mode=preview#api-reference");

    expect(getRoute()).toEqual({
      page: "repo-browser",
      provider: "github",
      platformHost: "github.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      path: "docs/guide.md",
      mode: "preview",
      anchor: "api-reference",
    });
  });
  it("parses /docs route state", () => {
    navigate("/docs?folder=notes&doc=Daily%2Ftoday.md");
    expect(getRoute()).toEqual({
      page: "docs",
      folder: "notes",
      doc: "Daily/today.md",
    });
    expect(getPage()).toBe("docs");
  });
  it("does not parse legacy issue detail routes", () => {
    navigate("/issues/org/repo/3");
    expect(getRoute()).toEqual({ page: "issues" });
  });

  it("does not parse old query-param issue detail routes", () => {
    navigate("/issues/detail?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&number=42");
    expect(getRoute()).toEqual({ page: "issues" });
  });

  it("parses provider issue routes with special characters in repo paths", () => {
    navigate("/host/gitlab.example.test%3A8443/issues/gitlab/Team%20One%2FSub%20Team/project%2B%231/7");

    expect(getRoute()).toEqual({
      page: "issues",
      selected: {
        owner: "Team One/Sub Team",
        name: "project+#1",
        provider: "gitlab",
        platformHost: "gitlab.example.test:8443",
        repoPath: "Team One/Sub Team/project+#1",
        number: 7,
      },
    });
  });

  it("parses focus provider pull and issue routes", () => {
    navigate("/focus/pulls/github/acme/widgets/42");
    expect(getRoute()).toEqual({
      page: "focus",
      itemType: "pr",
      ...prRef,
    });

    navigate("/focus/host/ghe.example.com/issues/github/acme/widgets/7");
    expect(getRoute()).toEqual({
      page: "focus",
      itemType: "issue",
      provider: "github",
      platformHost: "ghe.example.com",
      owner: "acme",
      name: "widgets",
      repoPath: "acme/widgets",
      number: 7,
    });
  });

  it("does not parse old query-param focus detail routes", () => {
    navigate("/focus/pr?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&number=42");
    expect(getRoute()).toEqual({ page: "activity" });
  });

  it("treats legacy /workspaces/panel routes as workspaces page", () => {
    navigate("/workspaces/panel/github.com/acme/widgets/42");
    expect(getRoute()).toEqual({ page: "workspaces" });
    expect(getPage()).toBe("workspaces");
  });
});

describe("router embed-workspace routes", () => {
  it("parses /workspaces/embed/list", () => {
    navigate("/workspaces/embed/list");
    expect(getRoute()).toEqual({ page: "embed-workspace-list" });
    expect(getPage()).toBe("embed-workspace-list");
  });

  it("parses /workspaces/embed/terminal without an id", () => {
    navigate("/workspaces/embed/terminal");
    expect(getRoute()).toEqual({
      page: "embed-workspace-terminal",
      workspaceId: "",
    });
  });

  it("parses /workspaces/embed/terminal/:workspaceId", () => {
    navigate("/workspaces/embed/terminal/abc-123");
    expect(getRoute()).toEqual({
      page: "embed-workspace-terminal",
      workspaceId: "abc-123",
    });
  });

  it("parses /workspaces/embed/detail/:provider/pr/:host/:number with repo_path", () => {
    navigate("/workspaces/embed/detail/github/pr/github.com/42?repo_path=acme%2Fwidgets");
    expect(getRoute()).toEqual({
      page: "embed-workspace-detail",
      provider: "github",
      itemType: "pr",
      platformHost: "github.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      number: 42,
    });
  });

  it("parses legacy /workspaces/embed/detail/:type/:host/:owner/:name/:number", () => {
    navigate("/workspaces/embed/detail/issue/gitlab.example.com/acme/widgets/7?tab=issue");
    expect(getRoute()).toEqual({
      page: "embed-workspace-detail",
      provider: "gitlab",
      itemType: "issue",
      platformHost: "gitlab.example.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      number: 7,
      tab: "issue",
    });
  });

  it("keeps legacy GitHub Enterprise embed detail URLs on GitHub", () => {
    navigate("/workspaces/embed/detail/pr/ghe.example.com/acme/widgets/42");
    expect(getRoute()).toEqual({
      page: "embed-workspace-detail",
      provider: "github",
      itemType: "pr",
      platformHost: "ghe.example.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      number: 42,
    });
  });

  it("parses legacy provider-explicit detail path without repo_path", () => {
    navigate("/workspaces/embed/detail/github/pr/github.com/acme/widgets/42?branch=main");
    expect(getRoute()).toEqual({
      page: "embed-workspace-detail",
      provider: "github",
      itemType: "pr",
      platformHost: "github.com",
      repoPath: "acme/widgets",
      owner: "acme",
      name: "widgets",
      number: 42,
      branch: "main",
    });
  });

  it("parses /workspaces/embed/detail with branch and tab query", () => {
    navigate(
      "/workspaces/embed/detail/gitlab/issue/git.example.com/7" +
        "?repo_path=org%2Fteam%2Frepo&branch=feature%2Fx&tab=reviews",
    );
    expect(getRoute()).toEqual({
      page: "embed-workspace-detail",
      provider: "gitlab",
      itemType: "issue",
      platformHost: "git.example.com",
      repoPath: "org/team/repo",
      owner: "org/team",
      name: "repo",
      number: 7,
      branch: "feature/x",
      tab: "reviews",
    });
  });

  it("ignores unknown tab values on the detail route", () => {
    navigate("/workspaces/embed/detail/github/pr/github.com/1?repo_path=o%2Fn&tab=garbage");
    const route = getRoute();
    expect(route).toEqual({
      page: "embed-workspace-detail",
      provider: "github",
      itemType: "pr",
      platformHost: "github.com",
      repoPath: "o/n",
      owner: "o",
      name: "n",
      number: 1,
    });
  });

  it("parses /workspaces/embed/empty/:reason for each known reason", () => {
    for (const reason of ["noSelection", "noRepo", "noWorkspace"] as const) {
      navigate(`/workspaces/embed/empty/${reason}`);
      expect(getRoute()).toEqual({
        page: "embed-workspace-empty",
        reason,
      });
    }
  });

  it("falls back to the standalone workspaces page for unknown empty reasons", () => {
    navigate("/workspaces/embed/empty/garbage");
    expect(getRoute()).toEqual({ page: "workspaces" });
  });

  it("parses /workspaces/embed/first-run", () => {
    navigate("/workspaces/embed/first-run");
    expect(getRoute()).toEqual({ page: "embed-workspace-first-run" });
    expect(getPage()).toBe("embed-workspace-first-run");
  });

  it("parses /workspaces/embed/project/:project_id", () => {
    navigate("/workspaces/embed/project/prj_abc123");
    expect(getRoute()).toEqual({
      page: "embed-workspace-project",
      projectId: "prj_abc123",
    });
    expect(getPage()).toBe("embed-workspace-project");

    navigate("/workspaces/embed/project/prj_abc123?host=epyc");
    expect(getRoute()).toEqual({
      page: "embed-workspace-project",
      projectId: "prj_abc123",
      hostKey: "epyc",
    });
  });

  it("parses /project-intake with an optional host", () => {
    navigate("/project-intake");
    expect(getRoute()).toEqual({ page: "project-intake" });

    navigate("/project-intake?host=epyc");
    expect(getRoute()).toEqual({
      page: "project-intake",
      hostKey: "epyc",
    });
  });

  it("falls back to the standalone workspaces page for unknown project_id shapes", () => {
    navigate("/workspaces/embed/project/has slash/extra");
    expect(getRoute()).toEqual({ page: "workspaces" });
  });
});

describe("router navigation events", () => {
  beforeEach(() => {
    navigate("/pulls");
  });

  afterEach(() => {
    delete (window as unknown as { __kenn_forge_config?: unknown }).__kenn_forge_config;
    (
      window as unknown as {
        __kenn_forge_notify_config_changed?: () => void;
      }
    ).__kenn_forge_notify_config_changed?.();
  });

  function installOnNavigate(spy: ReturnType<typeof vi.fn>, config: Record<string, unknown> = {}): void {
    (window as unknown as { __kenn_forge_config?: unknown }).__kenn_forge_config = {
      ...config,
      onNavigate: spy,
    };
    (
      window as unknown as {
        __kenn_forge_notify_config_changed?: () => void;
      }
    ).__kenn_forge_notify_config_changed?.();
  }

  it("fires onNavigate with pull payload for files route", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate(prFilesRoute);

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("pulls");
    expect(payload.type).toBe("pull");
    expect(payload.focus).toBe(false);
    expect(payload.owner).toBe("acme");
    expect(payload.name).toBe("widgets");
    expect(payload.number).toBe(42);
    expect(payload.provider).toBe("github");
    expect(payload.platform_host).toBe("github.com");
    expect(payload.repo_path).toBe("acme/widgets");
    expect(payload.repo).toBe("acme/widgets");
  });

  it("fires onNavigate with pull payload for conversation route", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate(prRoute);

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("pulls");
    expect(payload.type).toBe("pull");
    expect(payload.owner).toBe("acme");
    expect(payload.name).toBe("widgets");
    expect(payload.number).toBe(42);
  });

  it("fires onNavigate with pulls page for focus pull route", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate(focusPrRoute);

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("pulls");
    expect(payload.type).toBe("pull");
    expect(payload.focus).toBe(true);
    expect(payload.owner).toBe("acme");
    expect(payload.name).toBe("widgets");
    expect(payload.number).toBe(42);
  });

  it("fires provider-aware repo payloads for focus list repo filters", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/focus/mrs?repo=gitlab%7Cgitlab.example.com%2Fgroup%2Fsubgroup%2Fproject");

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.page).toBe("pulls");
    expect(payload.type).toBe("pull");
    expect(payload.focus).toBe(true);
    expect(payload.provider).toBe("gitlab");
    expect(payload.platform_host).toBe("gitlab.example.com");
    expect(payload.repo_path).toBe("group/subgroup/project");
    expect(payload.owner).toBe("group/subgroup");
    expect(payload.name).toBe("project");
    expect(payload.repo).toBe("group/subgroup/project");
  });

  it("keeps legacy focus list repo filters opaque in navigation events", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/focus/issues?repo=acme%2Fwidgets");

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.page).toBe("issues");
    expect(payload.type).toBe("issue");
    expect(payload.focus).toBe(true);
    expect(payload.repo).toBe("acme/widgets");
    expect(payload.provider).toBeUndefined();
    expect(payload.platform_host).toBeUndefined();
    expect(payload.repo_path).toBeUndefined();
  });

  it("fires onNavigate without owner/name/number for /pulls list", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/pulls");

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload).toEqual({
      page: "pulls",
      type: "pull",
      focus: false,
      view: "/pulls",
    });
  });

  it("reports /pulls/board as the pull list fallback", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/pulls/board");

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("pulls");
    expect(payload.type).toBe("pull");
    expect(payload.focus).toBe(false);
  });

  it("fires onNavigate with issues page for issue list route", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/issues");

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("issues");
    expect(payload.type).toBe("issue");
    expect(payload.focus).toBe(false);
  });

  it("maps /design-system to activity navigation events", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/design-system");

    expect(spy).toHaveBeenCalledTimes(1);
    const payload = spy.mock.calls[0]![0];
    expect(payload.page).toBe("activity");
    expect(payload.type).toBe("activity");
    expect(payload.view).toBe("/design-system");
  });
  it("maps /docs to docs navigation events", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate("/docs?folder=notes&doc=Daily%2Ftoday.md");

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.page).toBe("docs");
    expect(payload.type).toBe("docs");
    expect(payload.view).toBe("/docs?folder=notes&doc=Daily%2Ftoday.md");
  });

  it("maps repo browser routes to provider-aware repos navigation events and preserves URL fragments", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    navigate(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fsubgroup%2Fproject&path=README.md&mode=preview#install",
    );

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.type).toBe("repos");
    expect(payload.provider).toBe("gitlab");
    expect(payload.platform_host).toBe("gitlab.example.com");
    expect(payload.repo_path).toBe("group/subgroup/project");
    expect(payload.owner).toBe("group/subgroup");
    expect(payload.name).toBe("project");
    expect(payload.repo).toBe("group/subgroup/project");
    expect(payload.view).toBe(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fsubgroup%2Fproject&path=README.md&mode=preview#install",
    );
  });

  it("prefers route repo identity over embed repo config for repo browser navigation events", () => {
    const spy = vi.fn();
    installOnNavigate(spy, {
      ui: {
        repo: {
          provider: "gitlab",
          platform_host: "gitlab.example.com",
          repo_path: "other/group/project",
        },
      },
    });

    navigate("/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fsubgroup%2Fproject");

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.type).toBe("repos");
    expect(payload.repo).toBe("group/subgroup/project");
    expect(payload.provider).toBe("gitlab");
    expect(payload.platform_host).toBe("gitlab.example.com");
    expect(payload.repo_path).toBe("group/subgroup/project");
  });

  it("falls back to embed owner and name for navigation event repo names", () => {
    const spy = vi.fn();
    installOnNavigate(spy, {
      ui: {
        repo: {
          provider: "github",
          platform_host: "github.com",
          owner: "acme",
          name: "widgets",
        },
      },
    });

    navigate("/repos");

    const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
    expect(payload.type).toBe("repos");
    expect(payload.repo).toBe("acme/widgets");
  });

  it("maps every embed-workspace route to a workspaces navigation event", () => {
    const spy = vi.fn();
    installOnNavigate(spy);

    const embedPaths = [
      "/workspaces/embed/list",
      "/workspaces/embed/terminal/ws-1",
      "/workspaces/embed/detail/github/pr/github.com/42?repo_path=acme%2Fwidget",
      "/workspaces/embed/empty/noWorkspace",
      "/workspaces/embed/first-run",
      "/workspaces/embed/project/prj_abc123",
      "/project-intake?host=epyc",
    ];

    for (const path of embedPaths) {
      spy.mockClear();
      navigate(path);
      const payload = spy.mock.calls[spy.mock.calls.length - 1]![0];
      expect(payload.page, `page for ${path}`).toBe("workspaces");
      expect(payload.type, `type for ${path}`).toBe("workspaces");
      expect(payload.focus, `focus for ${path}`).toBe(false);
    }
  });
});

describe("router window bridges", () => {
  beforeEach(() => {
    navigate("/pulls");
  });

  it("exposes __kenn_forge_navigate_to_route as a window global", () => {
    const bridge = (
      window as unknown as {
        __kenn_forge_navigate_to_route?: (route: string) => void;
      }
    ).__kenn_forge_navigate_to_route;
    expect(typeof bridge).toBe("function");
  });

  it("__kenn_forge_navigate_to_route updates the SPA route", () => {
    const bridge = (
      window as unknown as {
        __kenn_forge_navigate_to_route: (route: string) => void;
      }
    ).__kenn_forge_navigate_to_route;

    bridge("/workspaces/embed/first-run");
    expect(getRoute()).toEqual({ page: "embed-workspace-first-run" });
    expect(getPage()).toBe("embed-workspace-first-run");

    bridge("/workspaces/embed/project/prj_xyz");
    expect(getRoute()).toEqual({
      page: "embed-workspace-project",
      projectId: "prj_xyz",
    });
  });
});

describe("router last activity route", () => {
  beforeEach(() => {
    navigate("/");
  });

  it("captures the Activity URL when leaving for another page", () => {
    navigate("/?selected=pr:1&provider=github&repo_path=acme%2Fwidgets");
    navigate("/pulls");
    expect(getLastActivityRoute()).toBe("/?selected=pr:1&provider=github&repo_path=acme%2Fwidgets");
  });

  it("captures the Activity URL on a non-tab exit such as the settings route", () => {
    navigate("/?selected=issue:10&provider=github&repo_path=acme%2Fwidgets");
    navigate("/settings");
    expect(getLastActivityRoute()).toBe("/?selected=issue:10&provider=github&repo_path=acme%2Fwidgets");
  });

  it("does not overwrite the cached route while navigating between non-Activity pages", () => {
    navigate("/?range=30d");
    navigate("/pulls");
    navigate("/issues");
    navigate("/settings");
    expect(getLastActivityRoute()).toBe("/?range=30d");
  });

  it("captures filter-only query changes written without a route update", () => {
    // Activity feed filters call history.replaceState directly, which does
    // not update the reactive route. Capturing at navigate() time reads the
    // live location, so those changes are still preserved on exit.
    history.replaceState(null, "", "/?range=90d&view=threaded");
    navigate("/pulls");
    expect(getLastActivityRoute()).toBe("/?range=90d&view=threaded");
  });

  it("captures direct Activity filter writes before leaving via Back/Forward", () => {
    navigate("/?range=7d");
    history.replaceState(null, "", "/?range=90d&view=threaded");
    history.replaceState(null, "", "/pulls");
    window.dispatchEvent(new Event("popstate"));
    expect(getPage()).toBe("pulls");
    expect(getLastActivityRoute()).toBe("/?range=90d&view=threaded");
  });

  it("refreshes the cache when a drawer selection is written via replaceUrl", () => {
    replaceUrl("/?selected=pr:2&provider=github&repo_path=acme%2Fwidgets");
    expect(getLastActivityRoute()).toBe("/?selected=pr:2&provider=github&repo_path=acme%2Fwidgets");
  });

  it("captures the Activity URL when browser Back/Forward lands on Activity", () => {
    navigate("/pulls");
    // Simulate the browser restoring an Activity history entry: the URL
    // changes underneath the app and a popstate fires without navigate().
    history.replaceState(null, "", "/?selected=pr:7&provider=github&repo_path=acme%2Fwidgets");
    window.dispatchEvent(new Event("popstate"));
    expect(getLastActivityRoute()).toBe("/?selected=pr:7&provider=github&repo_path=acme%2Fwidgets");
  });

  it("preserves a drawer change made before leaving Activity via Back/Forward", () => {
    // Reviewer scenario: change the drawer selection on Activity, then use
    // Back to leave without navigate(). The cache must hold the latest
    // Activity selection, not a stale one.
    replaceUrl("/?selected=pr:2&provider=github&repo_path=acme%2Fwidgets");
    history.replaceState(null, "", "/pulls");
    window.dispatchEvent(new Event("popstate"));
    expect(getPage()).toBe("pulls");
    expect(getLastActivityRoute()).toBe("/?selected=pr:2&provider=github&repo_path=acme%2Fwidgets");
  });
});

describe("workspace route memory", () => {
  beforeEach(() => {
    navigate("/workspaces");
  });

  it("remembers a local terminal route left via navigate()", () => {
    navigate("/terminal/ws-1");
    navigate("/pulls");
    expect(getLastWorkspaceRoute()).toBe("/terminal/ws-1");
  });

  it("remembers a fleet terminal route", () => {
    navigate("/terminal/fleet/build-host/ws-9");
    navigate("/pulls");
    expect(getLastWorkspaceRoute()).toBe("/terminal/fleet/build-host/ws-9");
  });

  it("remembers the bare list when the user left from /workspaces", () => {
    navigate("/terminal/ws-1");
    navigate("/workspaces");
    navigate("/pulls");
    expect(getLastWorkspaceRoute()).toBe("/workspaces");
  });

  it("captures direct replaceState writes before leaving via Back/Forward", () => {
    navigate("/terminal/ws-1");
    history.replaceState(null, "", "/terminal/ws-2");
    history.replaceState(null, "", "/pulls");
    window.dispatchEvent(new Event("popstate"));
    expect(getPage()).toBe("pulls");
    expect(getLastWorkspaceRoute()).toBe("/terminal/ws-2");
  });

  it("captures the workspace URL when Back/Forward lands on a terminal route", () => {
    navigate("/pulls");
    history.replaceState(null, "", "/terminal/ws-3");
    window.dispatchEvent(new Event("popstate"));
    expect(getLastWorkspaceRoute()).toBe("/terminal/ws-3");
  });

  it("remembers a terminal route entered via navigate() and left via Back", () => {
    navigate("/pulls");
    navigate("/terminal/ws-back-1");
    // Browser Back skips navigate(): popstate lands on the previous route
    // and only remembers the route it lands on, so the terminal visit must
    // already be in route memory from the navigate() that entered it.
    history.replaceState(null, "", "/pulls");
    window.dispatchEvent(new Event("popstate"));
    expect(getPage()).toBe("pulls");
    expect(getLastWorkspaceRoute()).toBe("/terminal/ws-back-1");
  });

  it("forgets a deleted workspace's terminal route", () => {
    navigate("/terminal/ws-del-1");
    navigate("/pulls");
    forgetWorkspaceRoute("ws-del-1");
    expect(getLastWorkspaceRoute()).toBe("/workspaces");
  });

  it("requires a matching host key to forget a fleet terminal route", () => {
    navigate("/terminal/fleet/build-host/ws-del-2");
    navigate("/pulls");
    forgetWorkspaceRoute("ws-del-2"); // local identity: a different workspace
    expect(getLastWorkspaceRoute()).toBe("/terminal/fleet/build-host/ws-del-2");
    forgetWorkspaceRoute("ws-del-2", "build-host");
    expect(getLastWorkspaceRoute()).toBe("/workspaces");
  });

  it("never re-remembers a forgotten route", () => {
    // Deleting from the terminal page: forget fires first, then the app
    // navigates to /workspaces, which re-captures the dying URL and must
    // refuse it.
    navigate("/terminal/ws-del-3");
    forgetWorkspaceRoute("ws-del-3");
    navigate("/workspaces");
    expect(getLastWorkspaceRoute()).toBe("/workspaces");
    // Other workspaces are still remembered normally afterwards.
    navigate("/terminal/ws-del-4");
    navigate("/pulls");
    expect(getLastWorkspaceRoute()).toBe("/terminal/ws-del-4");
  });
});
