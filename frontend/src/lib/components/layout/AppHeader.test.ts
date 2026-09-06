import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { compile } from "svelte/compiler";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import headerIconButtonSource from "./HeaderIconButton.svelte?raw";

const mockedContainerSize = vi.hoisted(() => ({
  value: "wide" as "narrow" | "medium" | "wide",
}));

type ModeKey = "activity" | "repos" | "docs" | "actions" | "pulls" | "issues" | "reviews" | "workspaces";

const mockedReviewsDaemonAvailable = vi.hoisted(() => ({ value: true }));

const mockedSync = vi.hoisted(() => ({
  running: false,
  providerAvailable: true,
  triggerSync: vi.fn(() => Promise.resolve()),
  triggerRepoSync: vi.fn((_repo: string) => Promise.resolve()),
}));

const mockedSettings = vi.hoisted(() => ({
  value: undefined as
    | {
        isModeVisible: (mode: ModeKey) => boolean;
        getModeVisibility: () => Record<ModeKey, boolean>;
        setModeVisibility: (visibility: Record<ModeKey, boolean>) => void;
      }
    | undefined,
}));

// Prevent RepoTypeahead from making real API calls in the test environment.
vi.mock("../../api/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...actual,
    client: {
      GET: () => Promise.resolve({ data: [], error: undefined }),
    },
  };
});

vi.mock("../../stores/container.svelte.js", () => ({
  getContainerSize: () => mockedContainerSize.value,
  isNarrow: () => mockedContainerSize.value === "narrow",
}));

// AppHeader reads sync state from the frontend context.
vi.mock("../../context.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../context.js")>();
  return {
    ...actual,
    getStores: () => ({
      sync: {
        getSyncState: () => (mockedSync.running ? { running: true } : null),
        getProviderAvailable: () => mockedSync.providerAvailable,
        triggerSync: mockedSync.triggerSync,
        triggerRepoSync: mockedSync.triggerRepoSync,
      },
      settings: mockedSettings.value,
      roborevDaemon: {
        isAvailable: () => mockedReviewsDaemonAvailable.value,
      },
    }),
  };
});

import AppHeader from "./AppHeaderRuntimeHarness.svelte";
import { initTheme, cleanupTheme } from "../../stores/theme.svelte.js";
import { setGlobalRepo } from "../../stores/filter.svelte.js";
import { setSidebarCollapsed } from "../../stores/sidebar.svelte.ts";
import { navigate } from "../../stores/router.svelte.ts";
import { createSettingsStore } from "../../stores/settings.svelte.js";
import { isPaletteOpen, resetPaletteState } from "../../stores/keyboard/palette-state.svelte.js";

function compiledStyle(source: string, selector: string): CSSStyleDeclaration {
  const css = compile(source, { filename: "component.svelte" }).css?.code ?? "";
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);

  for (const rule of Array.from(style.sheet?.cssRules ?? [])) {
    if (!("selectorText" in rule) || !("style" in rule)) continue;
    if (String(rule.selectorText).includes(selector)) {
      return rule.style as CSSStyleDeclaration;
    }
  }
  throw new Error(`Could not find compiled style rule for ${selector}`);
}

type MediaChangeCallback = (event: MediaQueryListEvent) => void;

function mockMatchMedia(matches: boolean, listeners?: MediaChangeCallback[]): void {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn().mockImplementation((_event: string, cb: MediaChangeCallback) => {
        listeners?.push(cb);
      }),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
}

function showImportedModes(): void {
  mockedSettings.value?.setModeVisibility({
    ...mockedSettings.value.getModeVisibility(),
    docs: true,
  });
}

function expectReservedRepoSelectorSlot(container: HTMLElement): void {
  const slot = container.querySelector(".repo-selector-placeholder");

  expect(screen.queryByTitle("Select repository")).toBeNull();
  expect(slot?.getAttribute("aria-hidden")).toBe("true");
}

describe("AppHeader", () => {
  beforeEach(() => {
    document.documentElement.classList.remove("dark");
    localStorage.clear();
    mockMatchMedia(false);
    setSidebarCollapsed(false);
    mockedContainerSize.value = "wide";
    mockedReviewsDaemonAvailable.value = true;
    mockedSync.running = false;
    mockedSync.providerAvailable = true;
    mockedSync.triggerSync.mockClear();
    mockedSync.triggerRepoSync.mockClear();
    setGlobalRepo(undefined);
    delete window.__kenn_forge_config;
    window.__kenn_forge_notify_config_changed?.();
    mockedSettings.value = createSettingsStore();
    resetPaletteState();
  });

  afterEach(() => {
    cleanupTheme();
    cleanup();
    navigate("/");
    document.documentElement.classList.remove("dark");
    localStorage.clear();
    setSidebarCollapsed(false);
    mockedContainerSize.value = "wide";
    mockedReviewsDaemonAvailable.value = true;
    mockedSync.running = false;
    mockedSync.providerAvailable = true;
    setGlobalRepo(undefined);
    delete window.__kenn_forge_config;
    window.__kenn_forge_notify_config_changed?.();
    mockedSettings.value = undefined;
    resetPaletteState();
  });

  it("keeps the primary segment wired to the existing full sync", async () => {
    initTheme();
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Sync" }));

    expect(mockedSync.triggerSync).toHaveBeenCalledOnce();
    expect(mockedSync.triggerRepoSync).not.toHaveBeenCalled();
  });

  it("disables sync while the hub is unavailable", () => {
    initTheme();
    mockedSync.providerAvailable = false;
    render(AppHeader);

    const syncButton = screen.getByRole("button", { name: "Sync unavailable" }) as HTMLButtonElement;
    expect(syncButton.disabled).toBe(true);
    expect(syncButton.title).toBe("Hub unavailable");
    expect((screen.getByRole("button", { name: "Sync options" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("keeps observing header height until the component unmounts", async () => {
    const originalResizeObserver = globalThis.ResizeObserver;
    const observers: Array<{ disconnected: boolean; observed: Element[] }> = [];
    globalThis.ResizeObserver = class ResizeObserverStub {
      readonly state = { disconnected: false, observed: [] as Element[] };

      constructor() {
        observers.push(this.state);
      }

      observe(target: Element): void {
        this.state.observed.push(target);
      }
      unobserve(): void {}
      disconnect(): void {
        this.state.disconnected = true;
      }
    };
    try {
      const view = render(AppHeader, { props: { onheightchange: vi.fn() } });
      const headerObserver = await waitFor(() => {
        const observer = observers.find((candidate) =>
          candidate.observed.some((target) => target.classList.contains("top-bar-frame")),
        );
        expect(observer).toBeTruthy();
        return observer!;
      });
      expect(headerObserver.disconnected).toBe(false);

      view.unmount();
      await waitFor(() => expect(headerObserver.disconnected).toBe(true));
    } finally {
      globalThis.ResizeObserver = originalResizeObserver;
    }
  });

  it("syncs the route repository before the global selector repository", async () => {
    initTheme();
    setGlobalRepo("gitlab|gitlab.com/other/project");
    navigate("/pulls/github/acme/widgets/7");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Sync options" }));
    const repoSyncAction = screen.getByRole("menuitem", { name: "Sync current repo" });
    expect(document.activeElement).toBe(repoSyncAction);
    await fireEvent.click(repoSyncAction);

    expect(mockedSync.triggerRepoSync).toHaveBeenCalledWith("github|github.com/acme/widgets");
    expect(mockedSync.triggerSync).not.toHaveBeenCalled();
    expect(screen.queryByRole("menu", { name: "Sync options" })).toBeNull();
  });

  it("uses one globally selected repository when the route has no repository", async () => {
    initTheme();
    setGlobalRepo("gitlab|gitlab.example.com/team/infra");
    navigate("/");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Sync options" }));
    await fireEvent.click(screen.getByRole("menuitem", { name: "Sync current repo" }));

    expect(mockedSync.triggerRepoSync).toHaveBeenCalledWith("gitlab|gitlab.example.com/team/infra");
  });

  it.each([
    ["all repositories", undefined],
    ["multiple repositories", "github|github.com/acme/widgets,gitlab|gitlab.com/team/infra"],
  ])("disables repository sync for %s", async (_label, selection) => {
    initTheme();
    setGlobalRepo(selection);
    navigate("/");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Sync options" }));

    expect((screen.getByRole("menuitem", { name: "Sync current repo" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("dismisses sync options outside and restores trigger focus after Escape", async () => {
    initTheme();
    setGlobalRepo("github|github.com/acme/widgets");
    render(AppHeader);
    const trigger = screen.getByRole("button", { name: "Sync options" });

    await fireEvent.click(trigger);
    await fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("menu", { name: "Sync options" })).toBeNull();

    await fireEvent.click(trigger);
    await fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("menu", { name: "Sync options" })).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("disables both sync segments while a sync is running", () => {
    initTheme();
    mockedSync.running = true;
    render(AppHeader);

    expect((screen.getByRole("button", { name: "Syncing" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Sync options" }) as HTMLButtonElement).disabled).toBe(true);
  });
  it("renders SVG icons for the header controls", () => {
    initTheme();
    const { container } = render(AppHeader);

    expect(container.querySelector("button[title='Open command palette'] svg")).toBeTruthy();
    expect(container.querySelector("button[title='Toggle theme'] svg")).toBeTruthy();
    expect(container.querySelector("button[title='Settings'] svg")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Select repository: Global/i }).querySelector("svg")).toBeTruthy();
  });

  it("spaces the command palette icon and shortcut hint", () => {
    const buttonStyle = compiledStyle(headerIconButtonSource, "button");

    expect(buttonStyle.getPropertyValue("gap")).toBe("var(--space-3)");
  });

  it("opens the command palette from the header trigger", async () => {
    initTheme();
    render(AppHeader);

    expect(isPaletteOpen()).toBe(false);

    await fireEvent.click(screen.getByRole("button", { name: "Open command palette" }));

    expect(isPaletteOpen()).toBe(true);
  });
  it("returns to the previous page when the settings button is clicked again", async () => {
    initTheme();
    navigate("/pulls/github/acme/widgets/1/files");
    render(AppHeader);

    await fireEvent.click(screen.getByTitle("Settings"));
    expect(window.location.pathname + window.location.search).toBe("/settings");

    await fireEvent.click(screen.getByTitle("Settings"));
    expect(window.location.pathname + window.location.search).toBe("/pulls/github/acme/widgets/1/files");
  });

  it("renders the collapsed sidebar toggle as a header icon button", () => {
    initTheme();
    setSidebarCollapsed(true);
    const { container } = render(AppHeader);

    expect(container.querySelector("button[title='Expand sidebar'] svg")).toBeTruthy();
  });

  it("places Reviews daemon status on the Reviews tab", () => {
    initTheme();
    mockedReviewsDaemonAvailable.value = false;
    render(AppHeader);

    const reviewsTab = screen.getByRole("button", {
      name: "Reviews Reviews daemon unavailable",
    });
    expect(within(reviewsTab).getByRole("img", { name: "Reviews daemon unavailable" })).toBeTruthy();
    expect(screen.getAllByRole("img", { name: "Reviews daemon unavailable" })).toHaveLength(1);
  });

  it("removes Reviews daemon status when available or hidden", () => {
    initTheme();
    render(AppHeader);
    expect(screen.queryByRole("img", { name: "Reviews daemon unavailable" })).toBeNull();

    cleanup();
    mockedReviewsDaemonAvailable.value = false;
    mockedSettings.value?.setModeVisibility({
      ...mockedSettings.value.getModeVisibility(),
      reviews: false,
    });
    render(AppHeader);

    expect(screen.queryByRole("img", { name: "Reviews daemon unavailable" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Reviews/ })).toBeNull();
  });

  it("does not offer a Board mode", () => {
    initTheme();
    render(AppHeader);

    expect(screen.queryByRole("button", { name: "Board" })).toBeNull();
  });

  it("exposes Actions only while the opt-in mode is enabled", async () => {
    initTheme();
    const view = render(AppHeader);
    expect(screen.queryByRole("button", { name: "Actions" })).toBeNull();

    mockedSettings.value?.setModeVisibility({
      ...mockedSettings.value.getModeVisibility(),
      actions: true,
    });
    await waitFor(() => expect(screen.getByRole("button", { name: "Actions" })).toBeTruthy());

    await fireEvent.click(screen.getByRole("button", { name: "Actions" }));
    expect(window.location.pathname).toBe("/actions");
    expect(screen.getByRole("button", { name: "Actions" }).getAttribute("aria-current")).toBe("page");

    mockedSettings.value?.setModeVisibility({
      ...mockedSettings.value.getModeVisibility(),
      actions: false,
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Actions" })).toBeNull());

    view.unmount();
  });

  it("never exposes Actions navigation in an embedded shell", () => {
    initTheme();
    mockedSettings.value?.setModeVisibility({
      ...mockedSettings.value.getModeVisibility(),
      actions: true,
    });
    window.__kenn_forge_config = { embed: {} };
    window.__kenn_forge_notify_config_changed?.();

    render(AppHeader);

    expect(screen.queryByRole("button", { name: "Actions" })).toBeNull();
  });

  it("marks the Workspaces tab current on terminal routes", () => {
    // One tabs list drives both the expanded tab row and kit's collapsed
    // dropdown, so the terminal → workspaces active mapping only needs
    // asserting once. jsdom cannot trigger kit TopBar's measurement
    // collapse (zero-width layout), so the expanded row is the testable
    // presentation here; the collapsed dropdown is covered by the
    // container-layout e2e spec.
    initTheme();
    navigate("/terminal/ws-123");
    render(AppHeader);

    const workspacesTab = screen.getByRole("button", { name: "Workspaces" });
    expect(workspacesTab.getAttribute("aria-current")).toBe("page");
  });

  it("renders the repository selector on Workspaces", () => {
    initTheme();
    navigate("/workspaces");
    const { container } = render(AppHeader);

    expect(screen.getByRole("button", { name: "Select repository: Global" })).toBeTruthy();
    expect(container.querySelector(".repo-selector-placeholder")).toBeNull();
  });

  it("does not show the collapsed sidebar shortcut hint on Activity", () => {
    initTheme();
    navigate("/");
    setSidebarCollapsed(true);
    const { container } = render(AppHeader);

    const expandButton = container.querySelector("button[title='Expand sidebar']");
    expect(expandButton).toBeTruthy();
    expect(expandButton!.querySelector("kbd[aria-label]")).toBeNull();
  });
  it("navigates to Docs from the desktop nav", async () => {
    initTheme();
    showImportedModes();
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Docs" }));

    expect(window.location.pathname + window.location.search).toBe("/docs");
  });

  it("reserves the provider repo selector slot on Docs without exposing the selector", () => {
    initTheme();
    navigate("/docs");
    const { container } = render(AppHeader);

    expectReservedRepoSelectorSlot(container);
  });

  it("does not reserve the repo selector slot when embed config hides it", () => {
    initTheme();
    window.__kenn_forge_config = { ui: { hideRepoSelector: true } };
    window.__kenn_forge_notify_config_changed?.();
    navigate("/docs");
    const { container } = render(AppHeader);

    expect(screen.queryByTitle("Select repository")).toBeNull();
    expect(container.querySelector(".repo-selector-placeholder")).toBeNull();
  });

  it("remembers the Docs route when the nav switches to Activity", async () => {
    initTheme();
    showImportedModes();
    navigate("/docs?folder=notes&doc=guide.md");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));

    expect(window.location.pathname + window.location.search).toBe("/");

    await fireEvent.click(screen.getByRole("button", { name: "Docs" }));

    expect(window.location.pathname + window.location.search).toBe("/docs?folder=notes&doc=guide.md");
  });

  it("opens selected Activity PR in PRs tab with files tab preserved", async () => {
    initTheme();
    navigate("/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&selected_tab=files");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "PRs" }));

    expect(window.location.pathname + window.location.search).toBe("/pulls/github/acme/widgets/1/files");
  });

  it("opens selected Activity issue in Issues tab with platform host preserved", async () => {
    initTheme();
    navigate("/?selected=issue:10&provider=github&platform_host=ghe.example.com&repo_path=acme%2Fwidgets");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Issues" }));

    expect(window.location.pathname + window.location.search).toBe(
      "/host/ghe.example.com/issues/github/acme/widgets/10",
    );
  });

  it("restores the previous Activity view when returning from PRs", async () => {
    initTheme();
    navigate("/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&range=30d");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "PRs" }));
    expect(window.location.pathname).toBe("/pulls/github/acme/widgets/1");

    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));

    expect(window.location.pathname + window.location.search).toBe(
      "/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&range=30d",
    );
  });

  it("restores the previous Activity view when returning from the settings gear", async () => {
    initTheme();
    navigate("/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets");
    render(AppHeader);

    // The settings gear leaves Activity without going through navigateTab.
    await fireEvent.click(screen.getByTitle("Settings"));
    expect(window.location.pathname).toBe("/settings");

    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));

    expect(window.location.pathname + window.location.search).toBe(
      "/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets",
    );
  });

  it("restores the previous Activity view when returning from Repos", async () => {
    initTheme();
    navigate("/?range=90d&view=threaded");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Repos" }));
    expect(window.location.pathname).toBe("/repos");

    await fireEvent.click(screen.getByRole("button", { name: "Activity" }));

    expect(window.location.pathname + window.location.search).toBe("/?range=90d&view=threaded");
  });

  it("opens Issues list when Activity selection is a PR", async () => {
    initTheme();
    navigate("/?selected=pr:1&provider=github&platform_host=github.com&repo_path=acme%2Fwidgets&selected_tab=files");
    render(AppHeader);

    await fireEvent.click(screen.getByRole("button", { name: "Issues" }));

    expect(window.location.pathname + window.location.search).toBe("/issues");
  });
});
