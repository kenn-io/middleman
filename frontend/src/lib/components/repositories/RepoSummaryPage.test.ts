import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { makeAppRuntime, type OwnedAppRuntime } from "../../app/runtime.js";
import * as flash from "../../stores/flash.svelte.js";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockNavigate = vi.fn();
const mockSetGlobalRepo = vi.fn();
const runtimeCapture = vi.hoisted(() => ({ current: undefined as OwnedAppRuntime | undefined }));

vi.mock("../../app/runtime.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.js")>();
  const { makeGeneratedClientFromRouteMocks } = await import("../../testing/test/route-mock-client.js");
  const client = {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  };
  return {
    ...actual,
    makeAppRuntime: () => actual.makeAppRuntime(makeGeneratedClientFromRouteMocks(client)),
  };
});

vi.mock("../../app/runtime-context.js", () => ({
  getAppRuntime: () => {
    const runtime = runtimeCapture.current;
    if (runtime === undefined) throw new Error("repository summary test runtime is not initialized");
    return runtime;
  },
}));

vi.mock("../../stores/router.svelte.js", () => ({
  navigate: (path: string) => mockNavigate(path),
}));

vi.mock("../../stores/filter.svelte.js", () => ({
  setGlobalRepo: (repo: string | undefined) => mockSetGlobalRepo(repo),
}));

vi.mock("../../context.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../context.js")>();
  return {
    ...actual,
    getStores: () => ({
      sync: {
        subscribeSyncComplete: () => () => {},
      },
      settings: {
        isSettingsLoaded: () => true,
        hasConfiguredRepos: () => true,
      },
    }),
  };
});

vi.mock("../detail/CommentEditor.svelte", async () => ({
  default: (await import("../../../test/MockCommentEditor.svelte")).default,
}));

import RepoSummaryPage from "./RepoSummaryPage.svelte";

function repoSummaryFixture({
  provider,
  platformHost,
  owner,
  name,
}: {
  provider: string;
  platformHost: string;
  owner: string;
  name: string;
}) {
  return {
    owner,
    name,
    platform_host: platformHost,
    repo: {
      provider,
      platform_host: platformHost,
      owner,
      name,
      repo_path: `${owner}/${name}`,
    },
    cached_pr_count: 0,
    open_pr_count: 0,
    draft_pr_count: 0,
    cached_issue_count: 0,
    open_issue_count: 0,
    active_authors: [],
    recent_issues: [],
  };
}

describe("RepoSummaryPage", () => {
  beforeEach(() => {
    runtimeCapture.current = makeAppRuntime();
    mockGet.mockReset();
    mockPost.mockReset();
    mockNavigate.mockReset();
    mockSetGlobalRepo.mockReset();
    localStorage.clear();
  });

  afterEach(async () => {
    cleanup();
    if (runtimeCapture.current) await Effect.runPromise(runtimeCapture.current.disposeEffect);
    runtimeCapture.current = undefined;
    for (const item of flash.getFlashes()) flash.dismissFlash(item.id);
  });

  it("flashes a rejected repository refresh", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    mockPost.mockRejectedValue(new Error("sync transport unavailable"));

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh repositories" }));

    await waitFor(() => {
      expect(flash.getFlash()).toMatchObject({
        message: "sync transport unavailable",
        tone: "danger",
      });
    });
  });

  it("renders repository summaries from the API", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 6,
          open_pr_count: 3,
          draft_pr_count: 1,
          cached_issue_count: 4,
          open_issue_count: 2,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          latest_release: {
            tag_name: "v2.8.1",
            name: "Version 2.8.1",
            url: "https://github.com/acme/widgets/releases/tag/v2.8.1",
            target_commitish: "main",
            prerelease: false,
            published_at: "2026-04-15T12:00:00Z",
          },
          releases: [
            {
              tag_name: "v2.8.1",
              name: "Version 2.8.1",
              url: "https://github.com/acme/widgets/releases/tag/v2.8.1",
              target_commitish: "main",
              prerelease: false,
              published_at: "2026-04-15T12:00:00Z",
            },
            {
              tag_name: "v2.8.0",
              name: "Version 2.8.0",
              url: "https://github.com/acme/widgets/releases/tag/v2.8.0",
              target_commitish: "main",
              prerelease: false,
              published_at: "2026-04-10T12:00:00Z",
            },
            {
              tag_name: "v2.7.0",
              name: "Version 2.7.0",
              url: "https://github.com/acme/widgets/releases/tag/v2.7.0",
              target_commitish: "main",
              prerelease: false,
              published_at: "2026-04-05T12:00:00Z",
            },
          ],
          commits_since_release: 8,
          commit_timeline: [
            {
              sha: "abc123",
              message: "Ship repo overview timeline",
              committed_at: "2026-04-16T12:00:00Z",
            },
          ],
          active_authors: [
            { login: "alice", item_count: 3 },
            { login: "bob", item_count: 2 },
          ],
          recent_issues: [
            {
              number: 12,
              title: "Investigate repo summary card",
              author: "alice",
              state: "open",
              url: "https://github.com/acme/widgets/issues/12",
              last_activity_at: "2026-04-17T14:55:00Z",
            },
          ],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    expect(
      await screen.findByRole("button", {
        name: /acme\s*\/\s*widgets/,
      }),
    ).toBeTruthy();
    const repoLink = screen.getByRole("link", {
      name: "Open acme/widgets on github.com",
    });
    expect(repoLink.getAttribute("href")).toBe("https://github.com/acme/widgets");
    expect(repoLink.getAttribute("target")).toBe("_blank");
    expect(screen.getAllByText("Open PRs").length).toBeGreaterThan(1);
    expect(screen.getByText("v2.8.1")).toBeTruthy();
    expect(screen.getByText("8 commits")).toBeTruthy();
    expect(screen.getByText("Investigate repo summary card")).toBeTruthy();
    expect(screen.getByTitle("alice").getAttribute("src")).toContain("https://github.com/alice.png?size=40");
    expect(screen.getByRole("button", { name: "Release v2.8.0" })).toBeTruthy();

    const commitDot = screen.getByRole("button", {
      name: "Commit abc123",
    });
    await fireEvent.mouseEnter(commitDot);
    expect(screen.getByText("Ship repo overview timeline")).toBeTruthy();
    await fireEvent.click(commitDot);
    await fireEvent.mouseLeave(commitDot);
    expect(screen.getByText("Ship repo overview timeline")).toBeTruthy();
    await fireEvent.click(document.body);
    expect(screen.queryByText("Ship repo overview timeline")).toBeNull();
  });

  it("hides the configured default platform host on repo cards", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          default_platform_host: "github.com",
          cached_pr_count: 0,
          open_pr_count: 0,
          draft_pr_count: 0,
          cached_issue_count: 0,
          open_issue_count: 0,
          active_authors: [],
          recent_issues: [],
        },
        {
          owner: "enterprise",
          name: "service",
          platform_host: "ghe.example.com",
          repo: {
            provider: "github",
            platform_host: "ghe.example.com",
            owner: "enterprise",
            name: "service",
            repo_path: "enterprise/service",
          },
          default_platform_host: "github.com",
          cached_pr_count: 0,
          open_pr_count: 0,
          draft_pr_count: 0,
          cached_issue_count: 0,
          open_issue_count: 0,
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    expect(screen.queryByText("github.com")).toBeNull();
    expect(screen.getByText("ghe.example.com")).toBeTruthy();
  });

  it("shows provider brand icons on repo cards when multiple providers are present", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
        repoSummaryFixture({
          provider: "gitlab",
          platformHost: "gitlab.com",
          owner: "platform",
          name: "api",
        }),
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    expect(screen.getByRole("img", { name: "GitHub" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "GitLab" })).toBeTruthy();
  });

  it("hides provider brand icons on repo cards for a single provider", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
        repoSummaryFixture({
          provider: "github",
          platformHost: "ghe.example.com",
          owner: "enterprise",
          name: "service",
        }),
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    expect(screen.queryByRole("img", { name: "GitHub" })).toBeNull();
  });

  it("shows the Roborev hook indicator only on the matching repository card", async () => {
    const widgets = repoSummaryFixture({
      provider: "github",
      platformHost: "github.com",
      owner: "Acme",
      name: "Widgets",
    });
    const tools = repoSummaryFixture({
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "tools",
    });
    mockGet.mockImplementation((path: string) => {
      if (path === "/roborev/configured-repositories") {
        return Promise.resolve({
          data: {
            repositories: [
              {
                provider: "github",
                platform_host: "github.com",
                repo_path: "acme/widgets",
                owner: "acme",
                name: "widgets",
              },
            ],
          },
          error: undefined,
        });
      }
      return Promise.resolve({ data: [widgets, tools], error: undefined });
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /Acme\s*\/\s*Widgets/ });
    const indicator = await screen.findByRole("img", { name: "Roborev hooks installed" });
    expect(indicator.getAttribute("title")).toBe("Roborev hooks installed");
    expect(indicator.closest(".repo-card")?.querySelector(".repo-card__identity")?.textContent).toMatch(
      /Acme\s*\/\s*Widgets/,
    );
    expect(screen.getAllByRole("img", { name: "Roborev hooks installed" })).toHaveLength(1);
  });

  it("keeps repository cards visible when the optional Roborev lookup fails", async () => {
    const widgets = repoSummaryFixture({
      provider: "github",
      platformHost: "github.com",
      owner: "acme",
      name: "widgets",
    });
    mockGet.mockImplementation((path: string) => {
      if (path === "/roborev/configured-repositories") {
        return Promise.resolve({ data: undefined, error: { detail: "unavailable" } });
      }
      return Promise.resolve({ data: [widgets], error: undefined });
    });

    render(RepoSummaryPage);

    expect(await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ })).toBeTruthy();
    expect(screen.queryByRole("img", { name: "Roborev hooks installed" })).toBeNull();
    expect(screen.queryByText("Couldn’t load repositories")).toBeNull();
  });

  it("keeps cached output visible when a sync issue exists", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 6,
          open_pr_count: 3,
          draft_pr_count: 1,
          cached_issue_count: 4,
          open_issue_count: 2,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "rate limit exceeded",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });

    expect(screen.getByText("Cached PRs")).toBeTruthy();
    expect(screen.getByText("Cached issues")).toBeTruthy();
    expect(screen.getByText("Sync issue")).toBeTruthy();
    expect(screen.queryByText("rate limit exceeded")).toBeNull();
  });

  it("navigates from repo metric cells instead of separate view buttons", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 6,
          open_pr_count: 3,
          draft_pr_count: 1,
          cached_issue_count: 4,
          open_issue_count: 2,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });

    expect(screen.queryByRole("button", { name: "View PRs" })).toBeNull();
    expect(screen.queryByRole("button", { name: "View issues" })).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: /3\s+Open PRs/ }));
    expect(mockSetGlobalRepo).toHaveBeenCalledWith("github|github.com/acme/widgets");
    expect(mockNavigate).toHaveBeenCalledWith("/pulls");

    await fireEvent.click(screen.getByRole("button", { name: /2\s+Open issues/ }));
    expect(mockSetGlobalRepo).toHaveBeenCalledWith("github|github.com/acme/widgets");
    expect(mockNavigate).toHaveBeenCalledWith("/issues");
  });

  it("opens the source browser from repository cards", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(
      screen.getByRole("button", {
        name: "View repository source for acme widgets on github.com",
      }),
    );

    expect(mockSetGlobalRepo).toHaveBeenCalledWith("github|github.com/acme/widgets");
    expect(mockNavigate).toHaveBeenCalledWith(
      "/repo/browser?provider=github&platform_host=github.com&repo_path=acme%2Fwidgets",
    );
  });

  it("filters repositories by search and stale release state", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "fresh",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "fresh",
            repo_path: "acme/fresh",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 0,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
          latest_release: {
            tag_name: "v1.0.0",
            name: "",
            url: "",
            target_commitish: "main",
            prerelease: false,
            published_at: "2026-04-16T12:00:00Z",
          },
          commits_since_release: 2,
          commit_timeline: [],
        },
        {
          owner: "acme",
          name: "stale",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "stale",
            repo_path: "acme/stale",
          },
          cached_pr_count: 4,
          open_pr_count: 0,
          draft_pr_count: 0,
          cached_issue_count: 3,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
          latest_release: {
            tag_name: "v0.9.0",
            name: "",
            url: "",
            target_commitish: "main",
            prerelease: false,
            published_at: "2026-03-01T12:00:00Z",
          },
          commits_since_release: 72,
          commit_timeline: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    expect(await screen.findByRole("button", { name: /acme\s*\/\s*fresh/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /acme\s*\/\s*stale/ })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Stale" }));
    expect(screen.queryByRole("button", { name: /acme\s*\/\s*fresh/ })).toBeNull();
    expect(screen.getByRole("button", { name: /acme\s*\/\s*stale/ })).toBeTruthy();

    await fireEvent.input(screen.getByPlaceholderText("Filter repositories"), {
      target: { value: "fresh" },
    });
    expect(screen.getByText("No repositories match")).toBeTruthy();
  });

  it("remembers repository filters when the page remounts", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "fresh",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "fresh",
            repo_path: "acme/fresh",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 0,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
          latest_release: {
            tag_name: "v1.0.0",
            name: "",
            url: "",
            target_commitish: "main",
            prerelease: false,
            published_at: "2026-04-16T12:00:00Z",
          },
          commits_since_release: 2,
          commit_timeline: [],
        },
        {
          owner: "acme",
          name: "stale",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "stale",
            repo_path: "acme/stale",
          },
          cached_pr_count: 4,
          open_pr_count: 0,
          draft_pr_count: 0,
          cached_issue_count: 3,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
          latest_release: {
            tag_name: "v0.9.0",
            name: "",
            url: "",
            target_commitish: "main",
            prerelease: false,
            published_at: "2026-03-01T12:00:00Z",
          },
          commits_since_release: 72,
          commit_timeline: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*fresh/ });
    await fireEvent.click(screen.getByRole("button", { name: "Stale" }));
    await fireEvent.input(screen.getByPlaceholderText("Filter repositories"), {
      target: { value: "stale" },
    });

    cleanup();
    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*stale/ });
    expect(screen.queryByRole("button", { name: /acme\s*\/\s*fresh/ })).toBeNull();
    expect((screen.getByPlaceholderText("Filter repositories") as HTMLInputElement).value).toBe("stale");
    expect(screen.getByRole("button", { name: "Stale" }).className).toContain("repo-page__filter--active");
  });

  it("creates an issue from a repo card and navigates to it", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });
    mockPost.mockResolvedValue({
      data: {
        Number: 27,
        repo_owner: "acme",
        repo_name: "widgets",
        detail_loaded: false,
      },
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));

    expect(
      screen.getByRole("dialog", {
        name: "New issue in github.com/acme/widgets",
      }),
    ).toBeTruthy();
    const bodyEditor = screen.getByTestId("mock-comment-editor");
    expect(bodyEditor.getAttribute("data-owner")).toBe("acme");
    expect(bodyEditor.getAttribute("data-name")).toBe("widgets");
    expect(bodyEditor.getAttribute("data-platform-host")).toBe("github.com");

    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Ship repo summaries" },
    });
    await fireEvent.input(
      screen.getByRole("textbox", {
        name: "Describe the problem, context, or follow-up work",
      }),
      {
        target: { value: "Need a compact repo dashboard." },
      },
    );
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith(
        "/issues/{provider}/{owner}/{name}",
        expect.objectContaining({
          params: {
            path: {
              provider: "github",
              owner: "acme",
              name: "widgets",
            },
          },
          body: {
            title: "Ship repo summaries",
            body: "Need a compact repo dashboard.",
          },
        }),
      );
      expect(mockSetGlobalRepo).toHaveBeenCalledWith("github|github.com/acme/widgets");
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("does not send duplicate issue create requests while submitting", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });
    let resolvePost: ((value: { data: { Number: number }; error: undefined }) => void) | undefined;
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve;
        }),
    );

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Ship repo summaries" },
    });

    const createButton = screen.getByRole("button", {
      name: "Create issue",
    });
    await fireEvent.submit(createButton);
    await fireEvent.submit(createButton);

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    resolvePost?.({
      data: { Number: 27 },
      error: undefined,
    });
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("keeps an accepted issue creation alive until a replacement page presents its success", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    const request = Promise.withResolvers<{ data: { Number: number }; error: undefined }>();
    let requestSignal: AbortSignal | undefined;
    mockPost.mockImplementation((_path: string, options: { signal: AbortSignal }) => {
      requestSignal = options.signal;
      return request.promise;
    });

    const firstPage = render(RepoSummaryPage);
    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Survive repository navigation" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    firstPage.unmount();
    expect(requestSignal?.aborted).toBe(false);
    request.resolve({ data: { Number: 27 }, error: undefined });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockNavigate).not.toHaveBeenCalled();

    render(RepoSummaryPage);
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledTimes(1);
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("adopts a pending issue creation when the repository page is replaced", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    const request = Promise.withResolvers<{ data: { Number: number }; error: undefined }>();
    mockPost.mockImplementation(() => request.promise);

    const firstPage = render(RepoSummaryPage);
    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Keep the accepted draft" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    firstPage.unmount();
    render(RepoSummaryPage);

    expect(await screen.findByDisplayValue("Keep the accepted draft")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Creating..." })).toHaveProperty("disabled", true);

    request.resolve({ data: { Number: 27 }, error: undefined });
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledTimes(1);
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("retains an uncertain issue outcome until a replacement page acknowledges it", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    const request = Promise.withResolvers<never>();
    mockPost.mockImplementation(() => request.promise);

    const firstPage = render(RepoSummaryPage);
    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Fence an unknown outcome" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));

    firstPage.unmount();
    request.reject(new Error("issue transport unavailable"));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(flash.getFlash()).toBeNull();

    render(RepoSummaryPage);
    expect(await screen.findByDisplayValue("Fence an unknown outcome")).toBeTruthy();
    await waitFor(() => {
      expect(flash.getFlash()).toMatchObject({
        message: expect.stringMatching(/outcome is unknown.*check the issue list before retrying/i),
        tone: "danger",
      });
      expect(screen.getByRole("button", { name: "Create issue" })).toHaveProperty("disabled", true);
    });

    await fireEvent.click(
      screen.getByRole("checkbox", { name: "I checked the provider's issue list and want to retry." }),
    );
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Create issue" })).toHaveProperty("disabled", false);
    });
  });

  it("allows issue creation to retry after a rejected request", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    mockPost
      .mockRejectedValueOnce(new Error("issue transport unavailable"))
      .mockResolvedValueOnce({ data: { Number: 27 }, error: undefined });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Retry this issue" },
    });
    const createButton = screen.getByRole("button", { name: "Create issue" });
    await fireEvent.submit(createButton);

    await waitFor(() => {
      expect(flash.getFlash()).toMatchObject({
        message: expect.stringMatching(
          /issue transport unavailable.*outcome is unknown.*check the issue list before retrying/i,
        ),
        tone: "danger",
      });
      expect((screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement).disabled).toBe(true);
    });

    await fireEvent.keyDown(
      screen.getByRole("textbox", {
        name: "Describe the problem, context, or follow-up work",
      }),
      { key: "Enter", ctrlKey: true },
    );
    expect(mockPost).toHaveBeenCalledTimes(1);

    await fireEvent.click(
      screen.getByRole("checkbox", { name: "I checked the provider's issue list and want to retry." }),
    );
    expect((screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.keyDown(
      screen.getByRole("textbox", {
        name: "Describe the problem, context, or follow-up work",
      }),
      { key: "Enter", ctrlKey: true },
    );
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledTimes(2);
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("requires acknowledgement after an ambiguous issue creation response", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    mockPost
      .mockResolvedValueOnce({
        error: {
          code: "mutationOutcomeUnknown",
          detail: "provider response unavailable",
          status: 502,
          type: "about:blank",
        },
        response: new Response(null, { status: 502 }),
      })
      .mockResolvedValueOnce({ data: { Number: 27 }, error: undefined });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Retry this issue" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => {
      expect(flash.getFlash()).toMatchObject({
        message: expect.stringMatching(
          /provider response unavailable.*outcome is unknown.*check the issue list before retrying/i,
        ),
        tone: "danger",
      });
      expect((screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement).disabled).toBe(true);
    });

    await fireEvent.keyDown(
      screen.getByRole("textbox", {
        name: "Describe the problem, context, or follow-up work",
      }),
      { key: "Enter", ctrlKey: true },
    );
    expect(mockPost).toHaveBeenCalledTimes(1);

    await fireEvent.click(
      screen.getByRole("checkbox", { name: "I checked the provider's issue list and want to retry." }),
    );
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledTimes(2);
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("does not replay retained issue presentation on an ordinary summary refresh", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    mockPost.mockImplementation((path: string) =>
      path === "/sync"
        ? Promise.resolve({ data: undefined, error: undefined })
        : Promise.resolve({
            error: {
              code: "mutationOutcomeUnknown",
              detail: "provider response unavailable",
              status: 502,
              type: "about:blank",
            },
            response: new Response(null, { status: 502 }),
          }),
    );

    render(RepoSummaryPage);
    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Retain without replay" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await screen.findByRole("checkbox", { name: "I checked the provider's issue list and want to retry." });
    const flashCount = flash.getFlashes().length;

    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await fireEvent.click(screen.getByRole("button", { name: "Refresh repositories" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/sync", expect.anything()));

    expect(screen.queryByRole("dialog", { name: "New issue in github.com/acme/widgets" })).toBeNull();
    expect(flash.getFlashes()).toHaveLength(flashCount);
  });

  it("allows an immediate retry after a definite issue rejection", async () => {
    mockGet.mockResolvedValue({
      data: [
        repoSummaryFixture({
          provider: "github",
          platformHost: "github.com",
          owner: "acme",
          name: "widgets",
        }),
      ],
      error: undefined,
    });
    mockPost
      .mockResolvedValueOnce({
        error: {
          code: "upstreamError",
          detail: "provider rejected the issue",
          status: 502,
          type: "about:blank",
        },
        response: new Response(null, { status: 502 }),
      })
      .mockResolvedValueOnce({ data: { Number: 27 }, error: undefined });

    render(RepoSummaryPage);
    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Retry a definite rejection" },
    });
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => {
      expect(flash.getFlash()).toMatchObject({ message: "provider rejected the issue", tone: "danger" });
      expect(
        screen.queryByRole("checkbox", { name: "I checked the provider's issue list and want to retry." }),
      ).toBeNull();
      expect(screen.getByRole("button", { name: "Create issue" })).toHaveProperty("disabled", false);
    });

    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledTimes(2);
      expect(mockNavigate).toHaveBeenCalledWith("/issues/github/acme/widgets/27");
    });
  });

  it("keeps issue composer state separate for duplicate repos on different hosts", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
        {
          owner: "acme",
          name: "widgets",
          platform_host: "ghe.example.com",
          repo: {
            provider: "github",
            platform_host: "ghe.example.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 1,
          open_pr_count: 0,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });
    mockPost.mockResolvedValue({
      data: {
        Number: 42,
        repo_owner: "acme",
        repo_name: "widgets",
        detail_loaded: false,
      },
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findAllByRole("button", {
      name: /acme\s*\/\s*widgets/,
    });
    const issueButtons = screen.getAllByRole("button", {
      name: "New issue",
    });
    const firstIssueButton = issueButtons[0];
    const secondIssueButton = issueButtons[1];
    if (!firstIssueButton || !secondIssueButton) {
      throw new Error("expected issue buttons for both repo hosts");
    }

    await fireEvent.click(firstIssueButton);
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Enterprise draft" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await fireEvent.click(secondIssueButton);
    expect((screen.getByPlaceholderText("Issue title") as HTMLInputElement).value).toBe("");
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await fireEvent.click(firstIssueButton);
    expect((screen.getByPlaceholderText("Issue title") as HTMLInputElement).value).toBe("Enterprise draft");
    await fireEvent.submit(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith(
        "/host/{platform_host}/issues/{provider}/{owner}/{name}",
        expect.objectContaining({
          params: {
            path: {
              provider: "github",
              platform_host: "ghe.example.com",
              owner: "acme",
              name: "widgets",
            },
          },
          body: expect.objectContaining({
            title: "Enterprise draft",
          }),
        }),
      );
      expect(mockSetGlobalRepo).toHaveBeenCalledWith("github|ghe.example.com/acme/widgets");
      expect(mockNavigate).toHaveBeenCalledWith("/host/ghe.example.com/issues/github/acme/widgets/42");
    });
  });

  it("retains modal issue drafts when dismissed", async () => {
    mockGet.mockResolvedValue({
      data: [
        {
          owner: "acme",
          name: "widgets",
          platform_host: "github.com",
          repo: {
            provider: "github",
            platform_host: "github.com",
            owner: "acme",
            name: "widgets",
            repo_path: "acme/widgets",
          },
          cached_pr_count: 2,
          open_pr_count: 1,
          draft_pr_count: 0,
          cached_issue_count: 1,
          open_issue_count: 1,
          most_recent_activity_at: "2026-04-17T15:04:05Z",
          last_sync_completed_at: "2026-04-17T15:00:00Z",
          last_sync_started_at: "2026-04-17T14:59:00Z",
          last_sync_error: "",
          active_authors: [],
          recent_issues: [],
        },
      ],
      error: undefined,
    });

    render(RepoSummaryPage);

    await screen.findByRole("button", { name: /acme\s*\/\s*widgets/ });
    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    await fireEvent.input(screen.getByPlaceholderText("Issue title"), {
      target: { value: "Draft issue title" },
    });
    await fireEvent.input(
      screen.getByRole("textbox", {
        name: "Describe the problem, context, or follow-up work",
      }),
      {
        target: { value: "Draft issue body with @alice" },
      },
    );
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: "New issue" }));

    expect((screen.getByPlaceholderText("Issue title") as HTMLInputElement).value).toBe("Draft issue title");
    expect(
      (
        screen.getByRole("textbox", {
          name: "Describe the problem, context, or follow-up work",
        }) as HTMLTextAreaElement
      ).value,
    ).toBe("Draft issue body with @alice");
  });
});
