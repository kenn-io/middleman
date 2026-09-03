import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vite-plus/test";

import type { Repo, RepoPreset } from "../api/types.js";
import { createSettingsStore } from "../stores/settings.svelte.js";
import { getGlobalRepo, setGlobalRepoPresetSelection } from "../stores/filter.svelte.js";
import { dismissFlash, getFlash, getFlashes } from "../stores/flash.svelte.js";
import { setWorkspaceRepoCatalog } from "../stores/workspace-repo-catalog.svelte.js";
import RepoTypeahead from "./RepoTypeaheadRuntimeHarness.svelte";

let settingsStore: ReturnType<typeof createSettingsStore>;
const apiMocks = vi.hoisted(() => ({
  client: {
    GET: vi.fn(() => Promise.resolve({ data: [], error: undefined })),
    POST: vi.fn(),
    PUT: vi.fn(),
    DELETE: vi.fn(),
  },
}));

vi.mock("../context.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../context.js")>();
  return {
    ...actual,
    getStores: () => ({
      settings: settingsStore,
    }),
  };
});

vi.mock("../api/runtime.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/runtime.js")>()),
  client: apiMocks.client,
}));

vi.mock("../api/generated/index.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/generated/index.js")>();
  return {
    ...actual,
    RepositoriesService: {
      ...actual.RepositoriesService,
      listRepos: async (options?: { signal?: AbortSignal }) =>
        unwrapMockResult(await apiMocks.client.GET("/repos", options)),
    },
    SettingsService: {
      ...actual.SettingsService,
      createRepoPreset: async (body: unknown, options?: { signal?: AbortSignal }) =>
        unwrapMockResult(await apiMocks.client.POST("/settings/repo-presets", { body, ...options })),
      deleteRepoPreset: async (params: { name: string }, options?: { signal?: AbortSignal }) =>
        unwrapMockResult(
          await apiMocks.client.DELETE("/settings/repo-presets/{name}", {
            params: { path: params },
            ...options,
          }),
        ),
    },
  };
});

async function unwrapMockResult(result: unknown): Promise<unknown> {
  if (typeof result !== "object" || result === null) return result;
  const response = result as { data?: unknown; error?: { detail?: string; title?: string }; response?: Response };
  if (response.error !== undefined) {
    const { GeneratedProblemResponse } = await import("../api/runtime.js");
    const status = response.response?.status ?? 500;
    throw new GeneratedProblemResponse(
      {
        type: "about:blank",
        title: response.error.title ?? "Request failed",
        status,
        detail: response.error.detail,
        code: "internalError",
      },
      response.response ?? new Response(null, { status }),
    );
  }
  return response.data;
}

const getRepos = apiMocks.client.GET as Mock<
  (path: string, options?: { signal?: AbortSignal }) => Promise<{ data: Repo[]; error: undefined }>
>;
const putSettings = apiMocks.client.PUT;
const postSettings = apiMocks.client.POST;
const deleteSettings = apiMocks.client.DELETE;

function presetRepo(repoPath: string, platformRepoId: string) {
  return {
    provider: "github",
    platform_host: "github.com",
    platform_repo_id: platformRepoId,
    repo_path: repoPath,
  };
}

describe("RepoTypeahead", () => {
  beforeEach(() => {
    // The expansion store persists collapsed nodes to localStorage, so clear
    // it between tests to keep each case from inheriting another's tree state.
    localStorage.clear();
    for (const flash of getFlashes()) dismissFlash(flash.id);
    settingsStore = createSettingsStore();
    settingsStore.setConfiguredRepos([]);
    settingsStore.setRepoPresets([]);
    setWorkspaceRepoCatalog(undefined, false);
    setGlobalRepoPresetSelection(undefined, undefined);
    getRepos.mockReset();
    getRepos.mockResolvedValue({ data: [], error: undefined });
    putSettings.mockReset();
    postSettings.mockReset();
    deleteSettings.mockReset();
  });

  afterEach(() => {
    cleanup();
  });

  it("renders the mobile picker as one menu row with selection context", () => {
    render(RepoTypeahead, {
      props: {
        selected: undefined,
        onchange: vi.fn(),
        mobile: true,
      },
    });

    const trigger = screen.getByRole("button", { name: "Select repository: Global" });
    expect(trigger.textContent).toContain("Global");
    expect(trigger.textContent).toContain("All repositories");
    expect(trigger.querySelector(".typeahead-mobile-icon")).toBeTruthy();
  });

  it("renders a selected repository as owner/name with its full identity available", () => {
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "example-labs",
        name: "atlas",
        repo_path: "example-labs/atlas",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    const { container } = render(RepoTypeahead, {
      props: {
        selected: "github|github.com/example-labs/atlas",
        onchange: vi.fn(),
      },
    });

    const trigger = screen.getByRole("button", {
      name: "Select repository: github/github.com/example-labs/atlas",
    });
    expect(trigger.textContent?.trim()).toBe("example-labs/atlas");
    expect(container.querySelector(".typeahead-value")?.getAttribute("title")).toBe(
      "github/github.com/example-labs/atlas",
    );
    expect(container.querySelector(".typeahead-repo-owner")?.textContent).toBe("example-labs/");
    expect(container.querySelector(".typeahead-repo-name")?.textContent).toBe("atlas");
  });

  it("updates dropdown options when configured repos change", async () => {
    render(RepoTypeahead, {
      props: {
        selected: undefined,
        onchange: vi.fn(),
      },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    expect(screen.queryByRole("option", { name: /import-lab\/api/i })).toBeNull();

    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    await waitFor(() => {
      expect(screen.getByRole("option", { name: /import-lab\/api/i })).toBeTruthy();
    });
  });

  it("omits configured repos the server marked hidden from the UI", async () => {
    render(RepoTypeahead, {
      props: {
        selected: undefined,
        onchange: vi.fn(),
      },
    });

    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "archive",
        repo_path: "acme/archive",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: true,
      },
    ]);

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await waitFor(() => {
      expect(screen.getByRole("option", { name: /acme\/api/i })).toBeTruthy();
    });
    expect(screen.queryByRole("option", { name: /acme\/archive/i })).toBeNull();
  });

  it("omits hidden repositories returned by every catalog source", async () => {
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "archive",
        repo_path: "acme/archive",
        platform_repo_id: "R_archive",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: true,
      },
    ]);
    getRepos.mockResolvedValue({
      data: [
        {
          Platform: "github",
          PlatformHost: "github.com",
          PlatformRepoID: "R_archive",
          Owner: "acme",
          Name: "archive",
        },
      ] as Repo[],
      error: undefined,
    });
    setWorkspaceRepoCatalog(
      [
        {
          id: "archive-workspace",
          created_at: "2026-08-13T00:00:00Z",
          git_head_ref: "main",
          item_number: 1,
          item_type: "pull",
          platform_host: "github.com",
          repo_name: "archive",
          repo_owner: "acme",
          status: "active",
          tmux_activity_source: "none",
          tmux_last_output_at: null,
          tmux_working: false,
          worktree_path: "/tmp/archive",
          repo: {
            provider: "github",
            platform_host: "github.com",
            platform_repo_id: "R_archive",
            owner: "acme",
            name: "archive",
            repo_path: "acme/archive",
          },
        },
      ],
      true,
    );

    render(RepoTypeahead, { props: { selected: undefined, onchange: vi.fn() } });
    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await waitFor(() => expect(getRepos).toHaveBeenCalled());

    expect(screen.queryByRole("option", { name: /acme\/archive/i })).toBeNull();
  });

  it("aborts the repository request when the component unmounts", async () => {
    let signal: AbortSignal | undefined;
    getRepos.mockImplementation((_path, options) => {
      signal = options?.signal;
      return new Promise(() => undefined);
    });
    const view = render(RepoTypeahead, {
      props: { selected: undefined, onchange: vi.fn() },
    });
    await waitFor(() => expect(getRepos).toHaveBeenCalled());

    view.unmount();

    expect(signal?.aborted).toBe(true);
  });

  it("replaces a repository request when configured identity changes at the same count", async () => {
    let resolveFirst!: (value: { data: Repo[]; error: undefined }) => void;
    const first = new Promise<{ data: Repo[]; error: undefined }>((resolve) => {
      resolveFirst = resolve;
    });
    const replacement = [
      { Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "two" },
    ] as unknown as Repo[];
    getRepos.mockImplementationOnce(() => first).mockResolvedValueOnce({ data: replacement, error: undefined });
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "one",
        repo_path: "acme/one",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    render(RepoTypeahead, { props: { selected: undefined, onchange: vi.fn() } });
    await waitFor(() => expect(getRepos).toHaveBeenCalledTimes(1));

    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "two",
        repo_path: "acme/two",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    await waitFor(() => expect(getRepos).toHaveBeenCalledTimes(2));
    resolveFirst({
      data: [{ Platform: "github", PlatformHost: "github.com", Owner: "acme", Name: "one" }] as unknown as Repo[],
      error: undefined,
    });
    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await waitFor(() => expect(screen.getByRole("option", { name: /acme\/two/i })).toBeTruthy());
    expect(screen.queryByRole("option", { name: /acme\/one/i })).toBeNull();
  });

  it("keeps fetched repos for glob-backed settings entries", async () => {
    const fetchedRepos = [
      {
        Platform: "github",
        PlatformHost: "github.com",
        Owner: "roborev-dev",
        Name: "kenn-forge",
      },
      {
        Platform: "github",
        PlatformHost: "github.com",
        Owner: "roborev-dev",
        Name: "worker",
      },
    ] as unknown as Repo[];

    getRepos.mockResolvedValue({
      data: fetchedRepos,
      error: undefined,
    });

    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "roborev-dev",
        name: "*",
        repo_path: "roborev-dev/*",
        is_glob: true,
        matched_repo_count: 2,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: {
        selected: undefined,
        onchange: vi.fn(),
      },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));

    await waitFor(() => {
      expect(screen.getByRole("option", { name: /roborev-dev\/kenn-forge/i })).toBeTruthy();
      expect(screen.getByRole("option", { name: /roborev-dev\/worker/i })).toBeTruthy();
    });
  });

  it("allows selecting multiple repositories with checkboxes", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    const view = render(RepoTypeahead, {
      props: {
        selected: undefined,
        onchange,
      },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await fireEvent.mouseDown(
      screen.getByRole("option", {
        name: /github.com\/import-lab\/api/i,
      }),
    );
    expect(onchange).toHaveBeenLastCalledWith("github|github.com/import-lab/api");

    await view.rerender({
      selected: "github|github.com/import-lab/api",
      onchange,
    });
    await fireEvent.mouseDown(
      screen.getByRole("option", {
        name: /github.com\/import-lab\/web/i,
      }),
    );
    expect(onchange).toHaveBeenLastCalledWith("github|github.com/import-lab/api,github|github.com/import-lab/web");
  });

  it("selecting an owner row selects all repos beneath it", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, { props: { selected: undefined, onchange } });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    const ownerCheckbox = screen
      .getByRole("option", { name: "github.com/import-lab" })
      .querySelector("input[type='checkbox']") as HTMLInputElement;
    await fireEvent.mouseDown(ownerCheckbox);

    expect(onchange).toHaveBeenLastCalledWith("github|github.com/import-lab/api,github|github.com/import-lab/web");
  });

  it("filters to matching leaves while keeping their owner visible", async () => {
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: { selected: undefined, onchange: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await fireEvent.input(screen.getByPlaceholderText("Filter repos..."), {
      target: { value: "web" },
    });

    await waitFor(() => {
      expect(
        screen.getByRole("option", {
          name: "github/github.com/import-lab/web",
        }),
      ).toBeTruthy();
      expect(
        screen.queryByRole("option", {
          name: "github/github.com/import-lab/api",
        }),
      ).toBeNull();
    });
  });

  it("clicking an owner row body expands/collapses without selecting", async () => {
    const onchange = vi.fn();
    // Two repos under import-lab so it renders a collapsible owner row; a
    // single-repo owner auto-flattens to one leaf and has no caret to toggle.
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    render(RepoTypeahead, { props: { selected: undefined, onchange } });
    await fireEvent.click(screen.getByRole("button", { name: /global/i }));

    // leaves visible initially
    expect(screen.getByRole("option", { name: "github/github.com/import-lab/api" })).toBeTruthy();
    // click the owner row body (its caret button has aria-label "Toggle import-lab";
    // click the row <li> itself, not the caret) -> collapses, hides leaves, selects nothing
    await fireEvent.mouseDown(screen.getByRole("option", { name: "github.com/import-lab" }));
    // NOTE: owner row body mousedown should toggle EXPAND, not select. After collapse the leaves are gone.
    await waitFor(() => {
      expect(
        screen.queryByRole("option", {
          name: "github/github.com/import-lab/api",
        }),
      ).toBeNull();
    });
    expect(onchange).not.toHaveBeenCalled();
  });

  it("clicking a leaf checkbox selects only that leaf", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    render(RepoTypeahead, { props: { selected: undefined, onchange } });
    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    const leaf = screen.getByRole("option", {
      name: "github/github.com/import-lab/api",
    });
    const checkbox = leaf.querySelector("input[type='checkbox']") as HTMLInputElement;
    await fireEvent.mouseDown(checkbox);
    expect(onchange).toHaveBeenLastCalledWith("github|github.com/import-lab/api");
  });

  it("drops removed repos after settings remove matching entries", async () => {
    const fetchedRepos = [
      {
        Platform: "github",
        PlatformHost: "github.com",
        Owner: "roborev-dev",
        Name: "kenn-forge",
      },
    ] as unknown as Repo[];
    const onchange = vi.fn();

    getRepos
      .mockResolvedValueOnce({
        data: fetchedRepos,
        error: undefined,
      })
      .mockResolvedValueOnce({
        data: [],
        error: undefined,
      });

    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "roborev-dev",
        name: "*",
        repo_path: "roborev-dev/*",
        is_glob: true,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: {
        selected: "github.com/roborev-dev/kenn-forge",
        onchange,
      },
    });

    await fireEvent.click(
      screen.getByRole("button", {
        name: /github.com\/roborev-dev\/kenn-forge/i,
      }),
    );

    await waitFor(() => {
      expect(screen.getByRole("option", { name: /roborev-dev\/kenn-forge/i })).toBeTruthy();
    });

    settingsStore.setConfiguredRepos([]);

    await waitFor(() => {
      expect(
        screen.queryByRole("option", {
          name: /roborev-dev\/kenn-forge/i,
        }),
      ).toBeNull();
      expect(onchange).toHaveBeenCalledWith(undefined);
    });
  });

  it("collapses and expands the focused owner with arrow keys", async () => {
    // Two repos under import-lab so it renders a collapsible owner row; a
    // single-repo owner auto-flattens to one leaf and has no caret to toggle.
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: { selected: undefined, onchange: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    const input = screen.getByPlaceholderText("Filter repos...");

    // leaves visible by default
    expect(screen.getByRole("option", { name: "github/github.com/import-lab/api" })).toBeTruthy();

    // move highlight onto the owner row (index 1) and collapse it
    await fireEvent.keyDown(input, { key: "ArrowDown" });
    await fireEvent.keyDown(input, { key: "ArrowLeft" });

    await waitFor(() => {
      expect(
        screen.queryByRole("option", {
          name: "github/github.com/import-lab/api",
        }),
      ).toBeNull();
    });

    await fireEvent.keyDown(input, { key: "ArrowRight" });
    await waitFor(() => {
      expect(
        screen.getByRole("option", {
          name: "github/github.com/import-lab/api",
        }),
      ).toBeTruthy();
    });
  });

  it("moves focus from a leaf to its parent owner on ArrowLeft", async () => {
    // Single host auto-flattens, so rows are: [Global], import-lab (owner,
    // depth 0), api (leaf, depth 1), web (leaf, depth 1). ArrowLeft on a leaf
    // should jump focus up to the owner row, per the keyboard contract.
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: { selected: undefined, onchange: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    const input = screen.getByPlaceholderText("Filter repos...");

    // ArrowDown onto the owner row, then onto the first leaf (api).
    await fireEvent.keyDown(input, { key: "ArrowDown" });
    await fireEvent.keyDown(input, { key: "ArrowDown" });
    const leaf = screen.getByRole("option", {
      name: "github/github.com/import-lab/api",
    });
    await waitFor(() => expect(leaf.classList.contains("highlighted")).toBe(true));

    // ArrowLeft on the leaf moves focus to its parent owner.
    await fireEvent.keyDown(input, { key: "ArrowLeft" });
    await waitFor(() => {
      const owner = screen.getByRole("option", {
        name: "github.com/import-lab",
      });
      expect(owner.classList.contains("highlighted")).toBe(true);
      expect(
        screen.getByRole("option", { name: "github/github.com/import-lab/api" }).classList.contains("highlighted"),
      ).toBe(false);
    });
  });

  it("toggles selection of the focused row with space", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "api",
        repo_path: "import-lab/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "import-lab",
        name: "web",
        repo_path: "import-lab/web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, { props: { selected: undefined, onchange } });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    const input = screen.getByPlaceholderText("Filter repos...");

    // highlight the owner row and select its subtree
    await fireEvent.keyDown(input, { key: "ArrowDown" });
    await fireEvent.keyDown(input, { key: " " });

    expect(onchange).toHaveBeenLastCalledWith("github|github.com/import-lab/api,github|github.com/import-lab/web");
  });

  it("uses provider-qualified values when configured repos collide by host and path", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "gitea",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, { props: { selected: undefined, onchange } });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await fireEvent.input(screen.getByPlaceholderText("Filter repos..."), {
      target: { value: "gitea/widgets" },
    });
    const giteaRow = screen.getByRole("option", {
      name: "gitea/github.com/acme/widgets",
    });
    expect(giteaRow.querySelector(".repo-tree-label")?.textContent).toBe("gitea/widgets");
    expect(screen.queryByRole("option", { name: "github/github.com/acme/widgets" })).toBeNull();
    await fireEvent.mouseDown(
      screen.getByRole("option", {
        name: "gitea/github.com/acme/widgets",
      }),
    );

    expect(onchange).toHaveBeenLastCalledWith("gitea|github.com/acme/widgets");
  });

  it("keeps provider-qualified selected values valid in desktop validation", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "gitea",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: {
        selected: "gitea|github.com/acme/widgets",
        onchange,
      },
    });

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Select repository: gitea/github.com/acme/widgets" })).toBeTruthy();
    });
    expect(onchange).not.toHaveBeenCalled();
  });

  it("drops stale provider slash values before desktop validation removes missing repos", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "widgets",
        repo_path: "acme/widgets",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);

    render(RepoTypeahead, {
      props: {
        selected: "github/github.com/acme/widgets,github|github.com/acme/missing",
        onchange,
      },
    });

    await waitFor(() => {
      expect(onchange).toHaveBeenCalledWith("github|github.com/acme/missing");
    });
  });

  it("shows Global and saved presets above the scrollable repository tree", async () => {
    const selected = "github|github.com/acme/api";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }] as RepoPreset[]);

    const view = render(RepoTypeahead, {
      props: { selected, onchange: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Select repository: Backend" }));

    expect(screen.getByRole("option", { name: "Global" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Backend" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete preset Backend" })).toBeTruthy();
    expect(view.container.querySelector(".typeahead-repo-list")).toBeTruthy();
    expect(view.container.querySelector(".typeahead-footer")).toBeTruthy();
    expect(view.container.querySelector(".typeahead-footer")?.parentElement).toBe(
      view.container.querySelector(".typeahead-repo-list")?.parentElement,
    );
  });

  it("applies a saved preset and records it as the browser-local source", async () => {
    const onchange = vi.fn();
    const selected = "github|github.com/acme/api";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }]);
    render(RepoTypeahead, { props: { selected: undefined, onchange } });

    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "Backend" }));

    expect(onchange).toHaveBeenLastCalledWith(selected);
    expect(localStorage.getItem("kenn-forge-filter-repo-preset")).toBe("Backend");
  });

  it("can apply saved presets without exposing preset management", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }]);
    render(RepoTypeahead, {
      props: { selected: undefined, onchange, allowPresetManagement: false },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Select repository: Global" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "Backend" }));

    expect(onchange).toHaveBeenLastCalledWith("github|github.com/acme/api");
    expect(screen.queryByRole("button", { name: "Save preset" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete preset Backend" })).toBeNull();
  });

  it("resolves a renamed configured repository without selecting a replacement at its old route", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        tracked_repo_path: "acme/backend",
        platform_repo_id: "R_original",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_original")] }]);
    getRepos.mockResolvedValue({
      data: [
        {
          Platform: "github",
          PlatformHost: "github.com",
          PlatformRepoID: "R_replacement",
          Owner: "acme",
          Name: "api",
        },
      ] as Repo[],
      error: undefined,
    });

    render(RepoTypeahead, { props: { selected: undefined, onchange } });
    await fireEvent.click(screen.getByRole("button", { name: /global/i }));
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "github/github.com/acme/backend" })).toBeTruthy();
      expect(screen.getByRole("option", { name: "github/github.com/acme/api" })).toBeTruthy();
    });
    await fireEvent.mouseDown(screen.getByRole("option", { name: "Backend" }));

    expect(onchange).toHaveBeenLastCalledWith("github|github.com/acme/backend");
  });

  it("does not activate an unavailable preset from the keyboard", async () => {
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Missing", repos: [presetRepo("acme/missing", "R_missing")] }]);
    render(RepoTypeahead, { props: { selected: "github|github.com/acme/api", onchange } });

    await fireEvent.click(screen.getByRole("button", { name: /acme\/api/i }));
    const input = screen.getByRole("textbox", { name: "Filter repos" });
    await fireEvent.keyDown(input, { key: "ArrowDown" });
    await fireEvent.keyDown(input, { key: "Enter" });

    expect(screen.getByRole("option", { name: "Missing" }).getAttribute("aria-disabled")).toBe("true");
    expect(onchange).not.toHaveBeenCalled();
  });

  it("defaults an edited preset to overwriting its prior name", async () => {
    const api = "github|github.com/acme/api";
    const web = "github|github.com/acme/web";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "web",
        repo_path: "acme/web",
        platform_repo_id: "R_web",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }]);
    setGlobalRepoPresetSelection("Backend", `${api},${web}`);
    render(RepoTypeahead, {
      props: { selected: `${api},${web}`, onchange: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button", { name: /2 repos/i }));
    await fireEvent.click(screen.getByRole("button", { name: "Save preset" }));

    expect((screen.getByRole("radio", { name: "Overwrite preset" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.getByRole("combobox", { name: "Preset to overwrite: Backend" })).toBeTruthy();
  });

  it("saves a newly named preset through settings", async () => {
    const selected = "github|github.com/acme/api";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    const repos = [presetRepo("acme/api", "R_api")];
    postSettings.mockResolvedValue({
      data: { repo_presets: [{ name: "Review", repos }], repos: [] },
      response: new Response(),
    });
    render(RepoTypeahead, { props: { selected, onchange: vi.fn() } });

    await fireEvent.click(screen.getByRole("button", { name: /acme\/api/i }));
    await fireEvent.click(screen.getByRole("button", { name: "Save preset" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Preset name" }), {
      target: { value: "Review" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(postSettings).toHaveBeenCalledWith(
        "/settings/repo-presets",
        expect.objectContaining({
          body: { name: "Review", repos },
        }),
      );
      expect(settingsStore.getRepoPresets()).toEqual([{ name: "Review", repos }]);
    });
  });

  it("keeps the save dialog open and reports settings failures", async () => {
    const selected = "github|github.com/acme/api";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    postSettings.mockResolvedValue({
      error: {
        type: "about:blank",
        title: "Save failed",
        status: 500,
        detail: "Preset save failed",
        code: "internalError",
      },
      response: new Response(undefined, { status: 500 }),
    });
    render(RepoTypeahead, { props: { selected, onchange: vi.fn() } });

    await fireEvent.click(screen.getByRole("button", { name: /acme\/api/i }));
    await fireEvent.click(screen.getByRole("button", { name: "Save preset" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Preset name" }), {
      target: { value: "Review" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Preset save failed")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Save repository preset" })).toBeTruthy();
    expect(settingsStore.getRepoPresets()).toEqual([]);
  });

  it("confirms preset deletion while retaining the active repository selection", async () => {
    const selected = "github|github.com/acme/api";
    const onchange = vi.fn();
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }]);
    setGlobalRepoPresetSelection("Backend", selected);
    deleteSettings.mockResolvedValue({
      data: { repo_presets: [], repos: [] },
      response: new Response(),
    });
    render(RepoTypeahead, { props: { selected, onchange } });

    await fireEvent.click(screen.getByRole("button", { name: "Select repository: Backend" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete preset Backend" }));
    expect(screen.getByText("Delete the preset ‘Backend’?")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Delete preset" }));

    await waitFor(() => expect(settingsStore.getRepoPresets()).toEqual([]));
    expect(getGlobalRepo()).toBe(selected);
    expect(localStorage.getItem("kenn-forge-filter-repo-preset")).toBeNull();
    expect(onchange).not.toHaveBeenCalled();
  });

  it("keeps the delete dialog open and flashes settings failures", async () => {
    const selected = "github|github.com/acme/api";
    settingsStore.setConfiguredRepos([
      {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        platform_repo_id: "R_api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      },
    ]);
    settingsStore.setRepoPresets([{ name: "Backend", repos: [presetRepo("acme/api", "R_api")] }]);
    deleteSettings.mockResolvedValue({
      error: {
        type: "about:blank",
        title: "Delete failed",
        status: 500,
        detail: "Preset deletion failed",
        code: "internalError",
      },
      response: new Response(undefined, { status: 500 }),
    });
    render(RepoTypeahead, { props: { selected, onchange: vi.fn() } });

    await fireEvent.click(screen.getByRole("button", { name: "Select repository: Backend" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete preset Backend" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete preset" }));

    await waitFor(() => {
      expect(getFlash()).toMatchObject({ message: "Preset deletion failed", tone: "danger" });
    });
    expect(screen.getByRole("dialog", { name: "Delete repository preset?" })).toBeTruthy();
    expect(settingsStore.getRepoPresets()).toHaveLength(1);
  });
});
