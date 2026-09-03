import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { getPaneLayoutStore, resetPaneLayoutStoresForTest, type PaneLayoutStore } from "../paneLayout.svelte.js";
import { sessionPaneKey } from "../session-pane-key.js";
import { pendingSessionFocus } from "../session-host.svelte.js";

import { defaultActions, setStoreInstances } from "./actions.js";
import { navigate } from "../router.svelte.ts";
import {
  getInlineWorkspaceController,
  publishHostedSessions,
  registerWorkspaceLauncher,
  resetWorkspaceHostForTest,
} from "../workspace-host.svelte.ts";
import { isSidebarCollapsed, setSidebarCollapsed } from "../sidebar.svelte.js";
import { OPEN_LABEL_PICKER_EVENT, type OpenLabelPickerDetail } from "../../components/detail/labelPickerCommand.js";
import {
  getNewWorkspaceSeedRepo,
  isNewWorkspaceDialogOpen,
  resetNewWorkspaceDialogState,
} from "../new-workspace.svelte.js";
import type { Context } from "./types.js";

function ctx(page: Context["page"], overrides: Partial<Context> = {}): Context {
  return {
    page,
    route: { page } as never,
    selectedPR: null,
    selectedIssue: null,
    isDiffView: false,
    detailTab: "conversation",
    sidebarTargetAvailable: true,
    ...overrides,
  };
}

const repo = {
  provider: "github",
  platform_host: "github.com",
  owner: "octo",
  name: "repo",
  repo_path: "octo/repo",
  capabilities: { read_labels: true, label_mutation: true },
};

const selected = {
  provider: "github",
  platformHost: "github.com",
  owner: "octo",
  name: "repo",
  repoPath: "octo/repo",
  number: 1,
};

const staleSelected = {
  ...selected,
  owner: "stale",
  name: "selection",
  repoPath: "stale/selection",
};

function command(id: string) {
  const action = defaultActions.find((a) => a.id === id);
  expect(action).toBeDefined();
  return action!;
}

function locationPath(): string {
  return window.location.pathname + window.location.search;
}

function configuredRepo(overrides: {
  provider?: string;
  platformHost?: string;
  owner: string;
  name: string;
  repoPath?: string;
  isGlob?: boolean;
}) {
  const repoPath = overrides.repoPath ?? `${overrides.owner}/${overrides.name}`;
  return {
    provider: overrides.provider ?? "github",
    platform_host: overrides.platformHost ?? "github.com",
    owner: overrides.owner,
    name: overrides.name,
    repo_path: repoPath,
    is_glob: overrides.isGlob ?? false,
    matched_repo_count: 1,
  };
}

function setConfiguredRepos(repos: ReturnType<typeof configuredRepo>[]): void {
  setStoreInstances(
    () =>
      ({
        settings: {
          getConfiguredRepos: () => repos,
        },
      }) as never,
  );
}

describe("defaultActions", () => {
  afterEach(() => {
    setSidebarCollapsed(false);
    resetNewWorkspaceDialogState();
    delete window.__kenn_forge_config;
    window.history.replaceState(null, "", "/");
  });

  it("includes the migrated globals", () => {
    const ids = defaultActions.map((a) => a.id);
    expect(ids).toEqual(
      expect.arrayContaining([
        "labels.edit",
        "go.next",
        "go.prev",
        "tab.toggle",
        "escape.list",
        "nav.pulls.list",
        "sidebar.toggle",
        "palette.open",
        "repo.browser.open",
        "cheatsheet.open",
        "sync.repos",
        "theme.toggle",
        "nav.settings",
        "nav.repos",
        "nav.reviews",
        "nav.workspaces",
        "nav.design-system",
      ]),
    );
    expect(ids).not.toContain("nav.pulls.board");
  });

  it("keeps the Pull requests list command palette-only", () => {
    window.history.replaceState(null, "", "/issues");
    const action = command("nav.pulls.list");

    expect(action.binding).toBeNull();
    action.handler(ctx("issues"));

    expect(locationPath()).toBe("/pulls");
  });

  it("palette.open binds the terminal-safe shifted K chord and the existing palette chords", () => {
    const palette = defaultActions.find((a) => a.id === "palette.open");
    expect(palette).toBeDefined();
    expect(palette!.binding).toEqual([
      { key: "k", ctrlOrMeta: true, shift: true },
      { key: "k", ctrlOrMeta: true },
      { key: "p", ctrlOrMeta: true },
      { key: "p", ctrlOrMeta: true, shift: true },
    ]);
  });

  it("cheatsheet.open binds shifted slash variants so the dispatcher matches the real keystroke", () => {
    // Browsers disagree on whether Shift+/ arrives as `?` or `/`.
    // The dispatcher's matcher treats omitted `shift` as `false`, so both
    // variants need an explicit `shift: true`.
    const cheatsheet = defaultActions.find((a) => a.id === "cheatsheet.open");
    expect(cheatsheet).toBeDefined();
    expect(cheatsheet!.binding).toEqual([
      { key: "?", shift: true },
      { key: "/", shift: true },
    ]);
  });

  it("dispatches Edit labels from PR detail context", () => {
    const action = defaultActions.find((a) => a.id === "labels.edit");
    expect(action).toBeDefined();
    setStoreInstances(
      () =>
        ({
          detail: {
            getDetail: () => ({
              repo_owner: "octo",
              repo_name: "repo",
              repo,
              merge_request: { Number: 1 },
            }),
          },
        }) as never,
    );
    const events: OpenLabelPickerDetail[] = [];
    const listener = (event: Event) => events.push((event as CustomEvent<OpenLabelPickerDetail>).detail);
    window.addEventListener(OPEN_LABEL_PICKER_EVENT, listener);
    try {
      const context = ctx("pulls", { selectedPR: selected });
      expect(action!.when(context)).toBe(true);
      action!.handler(context);
    } finally {
      window.removeEventListener(OPEN_LABEL_PICKER_EVENT, listener);
    }

    expect(events).toEqual([{ itemType: "pull", ...selected }]);
  });

  it("dispatches Edit labels from issue detail context", () => {
    const action = defaultActions.find((a) => a.id === "labels.edit");
    expect(action).toBeDefined();
    setStoreInstances(
      () =>
        ({
          issues: {
            getIssueDetail: () => ({
              repo_owner: "octo",
              repo_name: "repo",
              repo,
              issue: { Number: 1 },
            }),
          },
        }) as never,
    );
    const events: OpenLabelPickerDetail[] = [];
    const listener = (event: Event) => events.push((event as CustomEvent<OpenLabelPickerDetail>).detail);
    window.addEventListener(OPEN_LABEL_PICKER_EVENT, listener);
    try {
      const context = ctx("issues", { selectedIssue: selected });
      expect(action!.when(context)).toBe(true);
      action!.handler(context);
    } finally {
      window.removeEventListener(OPEN_LABEL_PICKER_EVENT, listener);
    }

    expect(events).toEqual([{ itemType: "issue", ...selected }]);
  });

  it("cheatsheet.open does not fire on the reviews page (roborev owns ?)", () => {
    // Roborev's ReviewsView has its own window-level `?` handler that
    // opens a help modal. If kenn-forge's cheatsheet also fires on `?`,
    // both modals open and the cheatsheet's filter input steals focus,
    // causing roborev's Escape handler to short-circuit on its
    // tag === "INPUT" guard. Gate the action by page to avoid that.
    const cheatsheet = defaultActions.find((a) => a.id === "cheatsheet.open");
    expect(cheatsheet).toBeDefined();
    expect(cheatsheet!.when(ctx("reviews"))).toBe(false);
    expect(cheatsheet!.when(ctx("pulls"))).toBe(true);
    expect(cheatsheet!.when(ctx("issues"))).toBe(true);
  });

  it("sidebar.toggle reserves the chord everywhere but only toggles pages with a sidebar target", () => {
    const action = defaultActions.find((a) => a.id === "sidebar.toggle");
    expect(action).toBeDefined();

    const visible = action!.visible ?? action!.when;

    expect(action!.when(ctx("activity"))).toBe(true);
    expect(visible(ctx("activity"))).toBe(false);
    setSidebarCollapsed(false);
    action!.handler(ctx("activity"));
    expect(isSidebarCollapsed()).toBe(false);

    expect(action!.when(ctx("repos"))).toBe(true);
    expect(visible(ctx("repos"))).toBe(false);
    expect(
      action!.when(
        ctx("pulls", {
          route: { page: "pulls", view: "list" } as never,
        }),
      ),
    ).toBe(true);
    expect(
      visible(
        ctx("pulls", {
          route: { page: "pulls", view: "list" } as never,
        }),
      ),
    ).toBe(true);
    action!.handler(
      ctx("pulls", {
        route: { page: "pulls", view: "list" } as never,
      }),
    );
    expect(isSidebarCollapsed()).toBe(true);
    setSidebarCollapsed(false);
    const compactPulls = ctx("pulls", {
      route: { page: "pulls", view: "list" } as never,
      sidebarTargetAvailable: false,
    });
    expect(action!.when(compactPulls)).toBe(true);
    expect(visible(compactPulls)).toBe(false);
    action!.handler(compactPulls);
    expect(isSidebarCollapsed()).toBe(false);
    expect(action!.when(ctx("issues"))).toBe(true);
    expect(visible(ctx("issues"))).toBe(true);
    expect(action!.when(ctx("workspaces"))).toBe(true);
    expect(visible(ctx("workspaces"))).toBe(true);
    expect(
      action!.when(
        ctx("terminal", {
          route: { page: "terminal", workspaceId: "1" } as never,
        }),
      ),
    ).toBe(true);
    expect(
      visible(
        ctx("terminal", {
          route: { page: "terminal", workspaceId: "1" } as never,
        }),
      ),
    ).toBe(true);
  });

  it("opens the repo browser from a selected pull request", () => {
    const action = command("repo.browser.open");
    const context = ctx("pulls", { selectedPR: selected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe("/repo/browser?provider=github&platform_host=github.com&repo_path=octo%2Frepo");
  });

  it("opens the repo browser from selected issue and activity contexts", () => {
    const action = command("repo.browser.open");

    const issueContext = ctx("issues", {
      selectedPR: staleSelected,
      selectedIssue: selected,
    });
    expect(action.when(issueContext)).toBe(true);
    action.handler(issueContext);
    expect(locationPath()).toBe("/repo/browser?provider=github&platform_host=github.com&repo_path=octo%2Frepo");

    window.history.replaceState(
      null,
      "",
      "/?selected=issue:8&provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fproject",
    );
    const context = ctx("activity", { selectedPR: staleSelected });
    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fproject",
    );

    window.history.replaceState(
      null,
      "",
      "/?selected=commit:abcdef1234567890abcdef1234567890abcdef12&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&branch=main",
    );
    const commitContext = ctx("activity", { selectedPR: staleSelected });
    expect(action.when(commitContext)).toBe(true);
    action.handler(commitContext);
    expect(locationPath()).toBe("/repo/browser?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets");
  });

  it("opens the repo browser from the route-selected issue before stale issue store state", () => {
    const action = command("repo.browser.open");
    const context = ctx("issues", {
      selectedIssue: staleSelected,
      route: {
        page: "issues",
        selected,
      },
    });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe("/repo/browser?provider=github&platform_host=github.com&repo_path=octo%2Frepo");
  });

  it("opens the repo browser for the current repo-browser route", () => {
    const action = command("repo.browser.open");
    const context = ctx("repo-browser", {
      route: {
        page: "repo-browser",
        provider: "forgejo",
        platformHost: "code.example.com",
        owner: "team",
        name: "tools",
        repoPath: "team/tools",
      },
    });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe("/repo/browser?provider=forgejo&platform_host=code.example.com&repo_path=team%2Ftools");
  });

  it("opens the repo browser from focus routes without stale selected item state", () => {
    const action = command("repo.browser.open");
    const pullContext = ctx("focus", {
      selectedPR: staleSelected,
      route: {
        page: "focus",
        itemType: "pr",
        provider: "gitlab",
        platformHost: "gitlab.example.com",
        owner: "group",
        name: "project",
        repoPath: "group/project",
        number: 42,
      } as never,
    });

    expect(action.when(pullContext)).toBe(true);
    action.handler(pullContext);
    expect(locationPath()).toBe(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fproject",
    );

    const issueContext = ctx("focus", {
      selectedIssue: staleSelected,
      route: {
        page: "focus",
        itemType: "issue",
        provider: "forgejo",
        platformHost: "code.example.com",
        owner: "team",
        name: "docs",
        repoPath: "team/docs",
        number: 7,
      } as never,
    });

    action.handler(issueContext);
    expect(locationPath()).toBe("/repo/browser?provider=forgejo&platform_host=code.example.com&repo_path=team%2Fdocs");
  });

  it("opens the repo browser for a uniquely configured workspace repo", () => {
    const action = command("repo.browser.open");
    window.__kenn_forge_config = {
      ui: {
        repo: { owner: "acme", name: "widgets" },
      },
    };
    setConfiguredRepos([
      configuredRepo({
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
      }),
    ]);
    const context = ctx("workspaces", { selectedPR: staleSelected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe("/repo/browser?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets");
  });

  it("opens the repo browser from fully qualified workspace repo config", () => {
    const action = command("repo.browser.open");
    window.__kenn_forge_config = {
      ui: {
        repo: {
          provider: "gitea",
          platform_host: "code.example.com",
          repo_path: "team/widgets",
          owner: "acme",
          name: "widgets",
        },
      },
    };
    setConfiguredRepos([
      configuredRepo({
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
      }),
      configuredRepo({
        provider: "gitlab",
        platformHost: "gitlab.example.com",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
      }),
    ]);
    const context = ctx("workspaces", { selectedPR: staleSelected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe("/repo/browser?provider=gitea&platform_host=code.example.com&repo_path=team%2Fwidgets");
  });

  it("opens the repo browser from canonical workspace repo identity", () => {
    const action = command("repo.browser.open");
    window.__kenn_forge_config = {
      ui: {
        repo: {
          provider: "gitlab",
          platform_host: "gitlab.example.com",
          repo_path: "group/subgroup/widgets",
        },
      },
    };
    setConfiguredRepos([]);
    const context = ctx("workspaces", { selectedPR: staleSelected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fsubgroup%2Fwidgets",
    );
  });

  it("uses workspace provider and host hints when matching configured repos", () => {
    const action = command("repo.browser.open");
    window.__kenn_forge_config = {
      ui: {
        repo: {
          provider: "gitlab",
          platform_host: "gitlab.example.com",
          owner: "acme",
          name: "widgets",
        },
      },
    };
    setConfiguredRepos([
      configuredRepo({
        provider: "github",
        platformHost: "github.com",
        owner: "acme",
        name: "widgets",
        repoPath: "acme/widgets",
      }),
      configuredRepo({
        provider: "gitlab",
        platformHost: "gitlab.example.com",
        owner: "acme",
        name: "widgets",
        repoPath: "group/widgets",
      }),
    ]);
    const context = ctx("workspaces", { selectedPR: staleSelected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(locationPath()).toBe(
      "/repo/browser?provider=gitlab&platform_host=gitlab.example.com&repo_path=group%2Fwidgets",
    );
  });

  it("hides the repo browser command when workspace repo context is ambiguous", () => {
    const action = command("repo.browser.open");
    window.__kenn_forge_config = {
      ui: {
        repo: { owner: "acme", name: "widgets" },
      },
    };
    setConfiguredRepos([
      configuredRepo({
        owner: "acme",
        name: "widgets",
        platformHost: "github.com",
      }),
      configuredRepo({
        owner: "acme",
        name: "widgets",
        platformHost: "ghe.example.com",
      }),
    ]);

    expect(action.when(ctx("workspaces"))).toBe(false);
  });

  it("offers New workspace everywhere and seeds the workspace-page repo", () => {
    const action = command("workspace.new");
    expect(action.when(ctx("pulls"))).toBe(true);

    // From a PR/issue page there is no workspace repo context, so the dialog
    // opens unseeded and the user picks a repository.
    action.handler(ctx("pulls"));
    expect(isNewWorkspaceDialogOpen()).toBe(true);
    expect(getNewWorkspaceSeedRepo()).toBeNull();

    resetNewWorkspaceDialogState();
    window.__kenn_forge_config = {
      ui: {
        repo: {
          provider: "gitea",
          platform_host: "code.example.com",
          repo_path: "acme/widgets",
          owner: "acme",
          name: "widgets",
        },
      },
    };

    action.handler(ctx("workspaces"));
    expect(isNewWorkspaceDialogOpen()).toBe(true);
    expect(getNewWorkspaceSeedRepo()).toEqual({
      provider: "gitea",
      platformHost: "code.example.com",
      owner: "acme",
      name: "widgets",
    });
  });
});

describe("session pane commands", () => {
  const identity = {
    provider: "github",
    platformHost: "github.com",
    owner: "octo",
    name: "repo",
    repoPath: "octo/repo",
    number: 1,
    itemType: "pull",
  };
  const paneKey = sessionPaneKey("ws-1", undefined, "ws-1:helper");
  const hosted = { paneKey, label: "Helper", hostKey: `${paneKey}/gen-1`, active: true };

  beforeEach(() => {
    localStorage.clear();
    resetPaneLayoutStoresForTest();
    resetWorkspaceHostForTest();
    navigate("/pulls");
    getInlineWorkspaceController("prs").claim(identity, { id: "ws-1", status: "ready" });
    publishHostedSessions({ workspaceId: "ws-1", hostKey: undefined }, [hosted]);
  });

  afterEach(() => {
    resetPaneLayoutStoresForTest();
    resetWorkspaceHostForTest();
    localStorage.clear();
  });

  /** Stand in for a mounted, unflattened DetailPaneLayout on the PRs surface. */
  function noteRendered(layout: PaneLayoutStore, tabs: readonly string[]): void {
    layout.notePaneRender({
      activeInputTabKey: tabs[0] ?? null,
      flattened: false,
      editableTabs: [...tabs],
      onScreenTabs: [...tabs],
      soloChromeTabs: [],
    });
  }

  it("promotes the shown session beside the workspace pane", () => {
    const layout = getPaneLayoutStore("prs");
    noteRendered(layout, ["conversation", "workspace"]);
    const action = command("session.promote");
    const context = ctx("pulls", { selectedPR: selected });

    expect(action.when(context)).toBe(true);
    action.handler(context);

    expect(layout.hasTab(paneKey)).toBe(true);
    // Its own leaf, not stacked behind the workspace pane: a promotion the user
    // cannot see reads as a command that did nothing.
    expect(layout.leafIDForTab(paneKey)).not.toBe(layout.leafIDForTab("workspace"));
    expect(pendingSessionFocus(hosted.hostKey)).toBe("explicit");
    // Offered once. Repeating it would move the tab the user just placed.
    expect(action.when(context)).toBe(false);
  });

  it("offers no promotion while the workspace pane is not on screen", () => {
    const layout = getPaneLayoutStore("prs");
    // Closed, tabbed behind a sibling, or under another leaf's zoom: the leaf is
    // still in the tree and the parked view keeps publishing its sessions, so only
    // the render report can say the terminal is not visible.
    noteRendered(layout, ["conversation", "workspace"]);
    layout.notePaneRender({
      activeInputTabKey: "conversation",
      flattened: false,
      editableTabs: ["conversation", "workspace"],
      onScreenTabs: ["conversation"],
      soloChromeTabs: [],
    });

    expect(command("session.promote").when(ctx("pulls", { selectedPR: selected }))).toBe(false);
  });

  it("offers no promotion while the surface is not hosting the workspace", () => {
    noteRendered(getPaneLayoutStore("issues"), ["conversation", "workspace"]);
    const action = command("session.promote");

    // Same claim, different page: the terminal is not on screen here.
    expect(action.when(ctx("issues", { selectedIssue: selected }))).toBe(false);
  });

  it("returns a promoted session to the workspace pane", () => {
    const layout = getPaneLayoutStore("prs");
    noteRendered(layout, ["conversation", "workspace"]);
    const promote = command("session.promote");
    const promoteContext = ctx("pulls", { selectedPR: selected });
    promote.handler(promoteContext);
    noteRendered(layout, ["conversation", "workspace", paneKey]);
    layout.noteFocused(paneKey);

    const demote = command("session.demote");
    const context = ctx("pulls", { selectedPR: selected });
    expect(demote.when(context)).toBe(true);
    demote.handler(context);

    expect(layout.hasTab(paneKey)).toBe(false);
  });

  it("opens the hosted workspace's launcher, and nothing when no pane hosts one", () => {
    const opener = vi.fn();
    registerWorkspaceLauncher(opener);
    const action = command("workspace.launcher");
    const context = ctx("pulls", { selectedPR: selected });

    expect(action.when(context)).toBe(true);
    action.handler(context);
    expect(opener).toHaveBeenCalled();

    // Surface-scoped: the same claim on another page must not offer to launch into
    // a pane that is not rendering the workspace.
    expect(action.when(ctx("issues", { selectedIssue: selected }))).toBe(false);
  });

  it("offers no demotion for a pane that is not a session", () => {
    const layout = getPaneLayoutStore("prs");
    noteRendered(layout, ["conversation", "workspace"]);
    layout.noteFocused("workspace");

    // Demotion is the session pane's own command: the workspace pane has nowhere
    // to go back to.
    expect(command("session.demote").when(ctx("pulls", { selectedPR: selected }))).toBe(false);
  });
});
