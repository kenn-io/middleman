import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Cause, Effect } from "effect";
import { afterAll, afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mountApplication } from "./lib/app/mount.js";
import type { AppExecution, OwnedAppRuntime } from "./lib/app/runtime.js";
import { makeTestAppRuntime } from "./lib/testing/effect-layers.js";
import { makeGeneratedClient } from "./lib/testing/generated-client.js";
// Compile the root component during collection so Vite transform work is not
// charged against the first lazy-feature test's behavioral timeout.
import "./App.svelte";

const featureImports = vi.hoisted(() => ({
  docs: 0,
  failDocsOnce: false,
}));

const startup = vi.hoisted(() => ({
  autoReady: true,
  readyCallbacks: [] as Array<() => void>,
}));

const kataIntegration = vi.hoisted(() => ({
  search: vi.fn(),
  resolveReference: vi.fn(),
  resolveUID: vi.fn(),
  launch: vi.fn(),
}));

const appSurfaceProps = vi.hoisted(() => ({
  palette: null as Record<string, unknown> | null,
  docs: null as Record<string, unknown> | null,
  provider: null as Record<string, unknown> | null,
}));

vi.mock("./lib/Provider.svelte", async () => {
  const ProviderMock = (await import("./lib/testing/AppProviderMock.svelte")).default;
  return {
    default: (anchor: Parameters<typeof ProviderMock>[0], props: Parameters<typeof ProviderMock>[1]) => {
      appSurfaceProps.provider = props as Record<string, unknown>;
      return ProviderMock(anchor, props);
    },
  };
});
vi.mock("./lib/views/PRListView.svelte", async () => ({
  default: (await import("./lib/testing/AppNavigationContextProbe.svelte")).default,
}));
vi.mock("./lib/views/IssueListView.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/views/ActivityFeedView.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/views/MobileActivityView.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/views/ReviewsView.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/views/FocusListView.svelte", async () => ({
  default: (await import("./lib/testing/AppNavigationContextProbe.svelte")).default,
}));
vi.mock("./lib/components/workspace/WorkspaceCreateSplitButton.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/context.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./lib/context.js")>()),
  getStores: () => ({
    settings: {
      getLaunchTargets: () => [],
      getTerminalSettings: () => ({ retained_sessions: 10 }),
    },
  }),
}));
vi.mock("./lib/utils/repo-filter-values.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./lib/utils/repo-filter-values.js")>()),
  normalizeRepoFilterSelection: (repo: string | undefined) => repo,
}));

vi.mock("./lib/components/layout/AppHeader.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/layout/StatusBar.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/keyboard/Palette.svelte", async () => {
  const Stub = (await import("./lib/testing/AppViewStub.svelte")).default;
  return {
    default: (anchor: Parameters<typeof Stub>[0], props: Parameters<typeof Stub>[1]) => {
      appSurfaceProps.palette = props;
      return Stub(anchor, props);
    },
  };
});
vi.mock("./lib/components/keyboard/Cheatsheet.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/repositories/RepoSummaryPage.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/settings/SettingsPage.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/terminal/WorkspaceTerminalView.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/terminal/WorkspaceEmbedShell.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/components/design-system/DesignSystemPage.svelte", async () => ({
  default: (await import("./lib/testing/AppViewStub.svelte")).default,
}));
vi.mock("./lib/features/docs/DocsFeature.svelte", async () => {
  featureImports.docs += 1;
  if (featureImports.failDocsOnce) {
    featureImports.failDocsOnce = false;
    throw new Error("docs chunk unavailable");
  }
  const Feature = (await import("./lib/testing/AppDocsFeatureMock.svelte")).default;
  return {
    default: (anchor: Parameters<typeof Feature>[0], props: Parameters<typeof Feature>[1]) => {
      appSurfaceProps.docs = props as Record<string, unknown>;
      return Feature(anchor, props);
    },
  };
});
vi.mock("./lib/features/docs/DocsFeature.svelte?retry", async () => {
  featureImports.docs += 1;
  return {
    default: (await import("./lib/testing/AppDocsFeatureMock.svelte")).default,
  };
});
vi.mock("./lib/features/docs/DocsFeature.svelte?retry2", async () => {
  featureImports.docs += 1;
  return {
    default: (await import("./lib/testing/AppDocsFeatureMock.svelte")).default,
  };
});
vi.mock("./lib/api/kata/integration.js", () => ({
  fetchKataDaemons: vi.fn(async () => []),
  resolveKataIssueReference: kataIntegration.resolveUID,
  resolveKataTextReference: kataIntegration.resolveReference,
  searchKataReferences: kataIntegration.search,
  resolveKataLaunchTarget: kataIntegration.launch,
}));
vi.mock("./lib/api/docs/api.js", () => ({
  createDocsAPI: () => ({}),
}));
vi.mock("./lib/utils/appStartup.js", () => ({
  runAppStartup: (
    _runtime: OwnedAppRuntime,
    {
      afterBackendReady,
      onReady,
    }: {
      afterBackendReady?: Effect.Effect<void>;
      onReady: () => void;
    },
  ) => {
    const markReady = () => {
      if (afterBackendReady) Effect.runFork(afterBackendReady);
      onReady();
    };
    if (startup.autoReady) {
      queueMicrotask(markReady);
    } else {
      startup.readyCallbacks.push(markReady);
    }
    return vi.fn();
  },
}));

function installBrowserGlobals() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe = vi.fn();
      unobserve = vi.fn();
      disconnect = vi.fn();
    },
  );
}

function createAppTarget() {
  const target = document.createElement("div");
  target.id = "app";
  document.body.appendChild(target);
  return target;
}

function testAppRuntime(onDispose: () => void): OwnedAppRuntime {
  return {
    disposeEffect: Effect.sync(onDispose),
    runCommand: <A, E>(): AppExecution<A, E> => ({
      interrupt: () => {},
      await: Effect.never,
      exit: new Promise(() => {}),
    }),
    runMicrotask: (): AppExecution<void, never> => ({
      interrupt: () => {},
      await: Effect.never,
      exit: new Promise(() => {}),
    }),
  };
}

const appRuntime = makeTestAppRuntime(makeGeneratedClient());

describe("App feature routes", () => {
  beforeEach(async () => {
    vi.clearAllMocks();
    featureImports.docs = 0;
    featureImports.failDocsOnce = false;
    startup.autoReady = true;
    startup.readyCallbacks = [];
    appSurfaceProps.palette = null;
    appSurfaceProps.docs = null;
    appSurfaceProps.provider = null;
    kataIntegration.search.mockResolvedValue([]);
    kataIntegration.resolveReference.mockRejectedValue(new Error("reference not resolved"));
    kataIntegration.launch.mockResolvedValue({ available: false, reason: "browser_unavailable" });
    installBrowserGlobals();
    window.history.replaceState(null, "", "/pulls");
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/pulls");
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  afterAll(async () => {
    await Effect.runPromise(appRuntime.disposeEffect);
  });

  it("preserves structured replace navigation through application context", async () => {
    const replaceState = vi.spyOn(window.history, "replaceState");
    const pushState = vi.spyOn(window.history, "pushState");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByRole("button", { name: "Replace navigation probe" })).toBeTruthy());
    replaceState.mockClear();
    pushState.mockClear();

    await fireEvent.click(screen.getByRole("button", { name: "Replace navigation probe" }));

    expect(replaceState).toHaveBeenCalledWith(null, "", "/issues");
    expect(pushState).not.toHaveBeenCalled();
  });

  it("retries lazy feature imports after a chunk load failure", async () => {
    featureImports.failDocsOnce = true;
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });

    await waitFor(() => expect(featureImports.docs).toBe(1));
    expect(screen.getByText(/\[vitest\] There was an error when mocking a module/)).toBeTruthy();
    expect(featureImports.docs).toBe(1);

    await fireEvent.click(screen.getByRole("button", { name: "Retry loading Docs" }));

    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
    expect(featureImports.docs).toBe(2);
  }, 10_000);

  it("waits for app readiness before mounting lazy feature shells", async () => {
    startup.autoReady = false;
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByText("Loading")).toBeTruthy());
    await waitFor(() => expect(featureImports.docs).toBe(1));

    expect(screen.queryByTestId("docs-feature")).toBeNull();

    for (const onReady of startup.readyCallbacks) {
      onReady();
    }
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
  });

  it("keeps Docs mounted while hidden", async () => {
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.queryByText("Loading")).toBeNull());

    const { navigate } = await import("./lib/stores/router.svelte.ts");

    navigate("/docs?folder=notes&doc=README.md");
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());

    await fireEvent.click(screen.getByRole("button", { name: "Docs count 0" }));
    expect(document.querySelector("[data-testid='docs-feature'] button")?.textContent).toContain("Docs count 1");

    navigate("/repos");
    await waitFor(() => expect(document.querySelector(".docs-shell")?.hasAttribute("hidden")).toBe(true));
    expect(document.querySelector("[data-testid='docs-feature'] button")?.textContent).toContain("Docs count 1");

    navigate("/docs?folder=notes&doc=guide.md");
    await waitFor(() => expect(document.querySelector(".docs-shell")?.hasAttribute("hidden")).toBe(false));
    expect(document.querySelector("[data-testid='docs-feature'] button")?.textContent).toContain("Docs count 1");
  });

  it("renders flashes raised through the shared store in the app-mounted kit banner", async () => {
    const { default: App } = await import("./App.svelte");
    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.queryByText("Loading")).toBeNull());

    // Import through the same facade every caller uses. This guards the
    // single-module-instance invariant the flash unification depends on: if
    // If frontend modules ever resolve different kit-ui copies, the
    // flash lands in a store the mounted banner does not read and this fails.
    const { showFlash, getFlashes, dismissFlash } = await import("./lib/stores/flash.svelte.js");
    try {
      showFlash("first shared-store flash");
      await waitFor(() => expect(screen.getByText("first shared-store flash")).toBeTruthy());

      // Stacking (not latest-wins replacement) is the intended semantics of
      // the kit store; both flashes stay visible.
      showFlash("second shared-store flash");
      await waitFor(() => expect(screen.getByText("second shared-store flash")).toBeTruthy());
      expect(screen.getByText("first shared-store flash")).toBeTruthy();
    } finally {
      for (const flash of getFlashes()) dismissFlash(flash.id);
    }
  });

  it("opens a Docs Kata reference through its pinned daemon launch target", async () => {
    kataIntegration.resolveReference.mockResolvedValueOnce({
      uid: "issue-solo",
      project_uid: "project-a",
    });
    kataIntegration.launch.mockResolvedValueOnce({
      available: true,
      url: "https://kata.example.test/kata?issue=issue-solo#direct=1",
    });
    const replace = vi.fn();
    const close = vi.fn();
    const popup = { opener: window, close, location: { replace } } as unknown as Window;
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
    await fireEvent.click(screen.getByRole("button", { name: "Open Kata reference" }));

    await waitFor(() =>
      expect(replace).toHaveBeenCalledWith("https://kata.example.test/kata?issue=issue-solo#direct=1"),
    );
    expect(open).toHaveBeenCalledWith("about:blank", "_blank");
    expect(popup.opener).toBeNull();
    expect(close).not.toHaveBeenCalled();
    expect(kataIntegration.resolveReference).toHaveBeenCalledWith("docs-daemon", undefined, "solo");
    expect(kataIntegration.search).not.toHaveBeenCalled();
    expect(kataIntegration.launch).toHaveBeenCalledWith("docs-daemon", "issue-solo");
  });

  it("opens a completed qualified Docs Kata reference through exact resolution", async () => {
    kataIntegration.resolveReference.mockResolvedValueOnce({
      uid: "issue-closed",
      project_uid: "project-a",
    });
    kataIntegration.launch.mockResolvedValueOnce({
      available: true,
      url: "https://kata.example.test/issues/issue-closed",
    });
    const replace = vi.fn();
    const popup = { opener: window, close: vi.fn(), location: { replace } } as unknown as Window;
    vi.spyOn(window, "open").mockReturnValue(popup);
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
    await fireEvent.click(screen.getByRole("button", { name: "Open closed Kata reference" }));

    await waitFor(() => expect(replace).toHaveBeenCalledWith("https://kata.example.test/issues/issue-closed"));
    expect(kataIntegration.resolveReference).toHaveBeenCalledWith("docs-daemon", "Project A", "closed-task");
    expect(kataIntegration.search).not.toHaveBeenCalled();
    expect(kataIntegration.launch).toHaveBeenCalledWith("docs-daemon", "issue-closed");
  });

  it("opens a completed bare Docs Kata reference through unique exact resolution", async () => {
    kataIntegration.resolveReference.mockResolvedValueOnce({
      uid: "issue-completed-bare",
      project_uid: "project-a",
    });
    kataIntegration.launch.mockResolvedValueOnce({
      available: true,
      url: "https://kata.example.test/issues/issue-completed-bare",
    });
    const replace = vi.fn();
    const popup = { opener: window, close: vi.fn(), location: { replace } } as unknown as Window;
    vi.spyOn(window, "open").mockReturnValue(popup);
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
    await fireEvent.click(screen.getByRole("button", { name: "Open completed bare Kata reference" }));

    await waitFor(() => expect(replace).toHaveBeenCalledWith("https://kata.example.test/issues/issue-completed-bare"));
    expect(kataIntegration.resolveReference).toHaveBeenCalledWith("docs-daemon", undefined, "completed-bare");
    expect(kataIntegration.search).not.toHaveBeenCalled();
    expect(kataIntegration.launch).toHaveBeenCalledWith("docs-daemon", "issue-completed-bare");
  });

  it("rejects an unsafe Docs Kata launch target before navigating the reserved window", async () => {
    kataIntegration.resolveReference.mockResolvedValueOnce({
      uid: "issue-unsafe",
      project_uid: "project-a",
    });
    kataIntegration.launch.mockResolvedValueOnce({
      available: true,
      url: "javascript:alert(document.cookie)",
    });
    const replace = vi.fn();
    const close = vi.fn();
    const popup = { opener: window, close, location: { replace } } as unknown as Window;
    vi.spyOn(window, "open").mockReturnValue(popup);
    const { replaceUrl } = await import("./lib/stores/router.svelte.ts");
    replaceUrl("/docs?folder=notes&doc=README.md");
    const { default: App } = await import("./App.svelte");

    render(App, { target: createAppTarget(), props: { runtime: appRuntime } });
    await waitFor(() => expect(screen.getByTestId("docs-feature")).toBeTruthy());
    await fireEvent.click(screen.getByRole("button", { name: "Open Kata reference" }));

    const { getFlashes, dismissFlash } = await import("./lib/stores/flash.svelte.js");
    await waitFor(() => expect(close).toHaveBeenCalledTimes(1));
    expect(replace).not.toHaveBeenCalled();
    expect(getFlashes()).toContainEqual(
      expect.objectContaining({ message: "Kata returned an unsafe browser URL.", tone: "danger" }),
    );
    for (const flash of getFlashes()) dismissFlash(flash.id);
  });

  it("awaits Svelte unmount and managed runtime finalizers exactly once", async () => {
    const target = createAppTarget();
    const finalized = vi.fn();
    const mounted = mountApplication(target, testAppRuntime(finalized));
    await waitFor(() => expect(target.childElementCount).toBeGreaterThan(0));

    await Effect.runPromise(mounted.dispose);
    await Effect.runPromise(mounted.dispose);

    expect(target.childElementCount).toBe(0);
    expect(finalized).toHaveBeenCalledOnce();
  });

  it("reports a non-interruption root finalizer defect", async () => {
    const target = createAppTarget();
    const reportFailure = vi.fn();
    const mounted = mountApplication(
      target,
      testAppRuntime(() => {
        throw new Error("finalizer failed");
      }),
      reportFailure,
    );
    await waitFor(() => expect(target.childElementCount).toBeGreaterThan(0));

    await Effect.runPromiseExit(mounted.dispose);

    await waitFor(() => expect(reportFailure).toHaveBeenCalledOnce());
    expect(Cause.hasDies(reportFailure.mock.calls[0]?.[0])).toBe(true);
    expect(target.querySelector('[role="alert"]')?.textContent).toContain("Kenn Forge could not start");
  });
});
