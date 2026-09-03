import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { GeneratedProblemResponse } from "../../api/runtime.js";

import WorkspaceProjectCard from "./WorkspaceProjectCardRuntimeHarness.svelte";

const win = window as any;

const mocks = vi.hoisted(() => ({
  showFlash: vi.fn(),
}));

vi.mock("../../stores/flash.svelte.js", () => ({
  showFlash: mocks.showFlash,
}));

const { projectGet, worktreesGet } = vi.hoisted(() => ({ projectGet: vi.fn(), worktreesGet: vi.fn() }));

vi.mock("../../app/runtime.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../app/runtime.ts")>();
  const { makeGeneratedClient } = await import("../../testing/generated-client.js");
  const client = makeGeneratedClient({
    ProjectsService: { getProject: projectGet, listWorktrees: worktreesGet },
    FleetService: { getFleetProject: projectGet, listFleetProjectWorktrees: worktreesGet },
  });
  return {
    ...actual,
    makeAppRuntime: () => actual.makeAppRuntime(client),
  };
});

interface ProjectFixture {
  id: string;
  display_name: string;
  local_path: string;
  default_branch?: string;
  platform_identity?: {
    platform?: string;
    platform_host: string;
    owner: string;
    name: string;
  };
}

function setProjectResponse(project: ProjectFixture | { error: string }): void {
  projectGet.mockReset();
  if ("error" in project) {
    const problem = { type: "about:blank", title: "Project not found", status: 404, detail: project.error } as const;
    projectGet.mockRejectedValue(new GeneratedProblemResponse(problem, new Response(null, { status: 404 })));
    return;
  }
  projectGet.mockResolvedValue({
    created_at: "2026-08-04T00:00:00Z",
    updated_at: "2026-08-04T00:00:00Z",
    ...project,
  });
}

function setWorktreesResponse(
  worktrees: Array<{
    id: string;
    project_id: string;
    branch: string;
    path: string;
  }>,
): void {
  worktreesGet.mockReset();
  worktreesGet.mockResolvedValue({ worktrees });
}

describe("WorkspaceProjectCard", () => {
  beforeEach(() => {
    delete win.__kenn_forge_config;
    mocks.showFlash.mockReset();
  });

  afterEach(() => {
    cleanup();
    projectGet.mockReset();
    worktreesGet.mockReset();
  });

  it("renders project metadata and the create-first-worktree CTA when empty", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/Users/wesm/code/myrepo",
      default_branch: "main",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });

    expect(await screen.findByText("myrepo")).toBeTruthy();
    expect(screen.getByText("/Users/wesm/code/myrepo")).toBeTruthy();
    expect(screen.getByText("main")).toBeTruthy();
    expect(screen.getByText("This project has no worktrees yet.")).toBeTruthy();
    expect(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    ).toBeTruthy();
  });

  it("aborts a pending project read when the card unmounts", async () => {
    let requestSignal: AbortSignal | undefined;
    projectGet.mockImplementation((_path: unknown, options: { signal?: AbortSignal }) => {
      requestSignal = options.signal;
      return new Promise(() => {});
    });
    setWorktreesResponse([]);
    const view = render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await waitFor(() => {
      expect(projectGet).toHaveBeenCalledOnce();
    });

    view.unmount();

    expect(requestSignal?.aborted).toBe(true);
  });

  it("hides the platform chip row when platform_identity is absent", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "no-remote-repo",
      local_path: "/tmp/no-remote",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("no-remote-repo");
    // Chip uses platform host / owner / name format; absence is the no-platform path.
    expect(screen.queryByText(/github\.com \/ /)).toBeNull();
  });

  it("renders platform identity from platform_host", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "remote-repo",
      local_path: "/tmp/remote-repo",
      platform_identity: {
        platform_host: "gitlab.example.com",
        owner: "group/subgroup",
        name: "project",
      },
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });

    expect(await screen.findByText("gitlab.example.com / group/subgroup / project")).toBeTruthy();
  });

  it("renders provider brand icon beside platform identity when platform is present", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "remote-repo",
      local_path: "/tmp/remote-repo",
      platform_identity: {
        platform: "gitlab",
        platform_host: "gitlab.example.com",
        owner: "group/subgroup",
        name: "project",
      },
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });

    expect(await screen.findByRole("img", { name: "GitLab" })).toBeTruthy();
  });

  it("renders existing worktrees and switches the CTA label", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([
      {
        id: "wtr_1",
        project_id: "prj_1",
        branch: "feature-x",
        path: "/tmp/myrepo-worktrees/feature-x",
      },
    ]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("feature-x");
    expect(screen.getByText("/tmp/myrepo-worktrees/feature-x")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Create another worktree/i })).toBeTruthy();
  });

  it("loads project data through fleet routes when host scoped", async () => {
    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/srv/myrepo",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, {
      props: { projectId: "prj_1", hostKey: " epyc " },
    });

    expect(await screen.findByText("myrepo")).toBeTruthy();
    expect(projectGet).toHaveBeenCalledWith(
      { hostKey: "epyc", projectId: "prj_1" },
      { signal: expect.any(AbortSignal) },
    );
    expect(worktreesGet).toHaveBeenCalledWith(
      { hostKey: "epyc", projectId: "prj_1" },
      { signal: expect.any(AbortSignal) },
    );
  });

  it("renders an error and a retry button when the project fetch fails", async () => {
    setProjectResponse({ error: "project not found" });
    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    expect(await screen.findByText("project not found")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Retry/i })).toBeTruthy();
  });

  it("invokes the new-worktree action with the project id when clicked", async () => {
    const newWorktreeHandler = vi.fn().mockResolvedValue({ ok: true });
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: newWorktreeHandler,
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();

    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("myrepo");

    await fireEvent.click(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    );
    await waitFor(() => {
      expect(newWorktreeHandler).toHaveBeenCalledWith({
        surface: "project-card",
        projectId: "prj_1",
      });
    });
  });

  it("includes the host key in new-worktree action context", async () => {
    const newWorktreeHandler = vi.fn().mockResolvedValue({ ok: true });
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: newWorktreeHandler,
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();

    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, {
      props: { projectId: "prj_1", hostKey: " epyc " },
    });
    await screen.findByText("myrepo");

    await fireEvent.click(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    );
    await waitFor(() => {
      expect(newWorktreeHandler).toHaveBeenCalledWith({
        surface: "project-card",
        projectId: "prj_1",
        hostKey: "epyc",
      });
    });
  });

  it("adopts a retained action before offering a new worktree intent", async () => {
    let completeAction: ((result: CommandResult) => void) | undefined;
    const pendingAction = new Promise<CommandResult>((resolve) => {
      completeAction = resolve;
    });
    const newWorktreeHandler = vi.fn(() => pendingAction);
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: newWorktreeHandler,
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();
    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);
    const view = render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("myrepo");
    await fireEvent.click(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    );
    await waitFor(() => {
      expect(newWorktreeHandler).toHaveBeenCalledWith({
        surface: "project-card",
        projectId: "prj_1",
      });
    });

    await view.rerender({ projectId: "prj_2" });
    await waitFor(() => {
      expect(projectGet).toHaveBeenCalledTimes(2);
    });
    if (!completeAction) throw new Error("project action did not start");
    completeAction({ ok: true });

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(projectGet).toHaveBeenCalledTimes(2);

    await view.rerender({ projectId: "prj_1" });
    await waitFor(() => expect(projectGet).toHaveBeenCalledTimes(4));
    await fireEvent.click(screen.getByRole("button", { name: /Create your first worktree/i }));
    await waitFor(() => expect(newWorktreeHandler).toHaveBeenCalledTimes(2));

    expect(projectGet).toHaveBeenCalledTimes(5);
  });

  it("keeps the current retained-action owner fenced while an earlier waiter settles", async () => {
    let completeAction: ((result: CommandResult) => void) | undefined;
    const pendingAction = new Promise<CommandResult>((resolve) => {
      completeAction = resolve;
    });
    const newWorktreeHandler = vi.fn(() => pendingAction);
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: newWorktreeHandler,
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();

    const refreshResolvers: Array<(response: ProjectFixture & { created_at: string; updated_at: string }) => void> = [];
    let firstProjectLoads = 0;
    projectGet.mockImplementation(({ projectId }: { projectId: string }) => {
      const project = {
        id: projectId,
        display_name: projectId === "prj_1" ? "Retained Project" : "Navigation Target",
        local_path: `/tmp/${projectId}`,
        created_at: "2026-08-04T00:00:00Z",
        updated_at: "2026-08-04T00:00:00Z",
      };
      if (projectId === "prj_1") {
        firstProjectLoads += 1;
        if (firstProjectLoads >= 3) {
          return new Promise((resolve) => {
            refreshResolvers.push(resolve);
          });
        }
      }
      return Promise.resolve(project);
    });
    setWorktreesResponse([]);

    const view = render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("Retained Project");
    await fireEvent.click(screen.getByRole("button", { name: /Create your first worktree/i }));
    await waitFor(() => expect(newWorktreeHandler).toHaveBeenCalledOnce());

    await view.rerender({ projectId: "prj_2" });
    await screen.findByText("Navigation Target");
    await view.rerender({ projectId: "prj_1" });
    await waitFor(() => expect(firstProjectLoads).toBe(2));

    if (!completeAction) throw new Error("project action did not start");
    completeAction({ ok: true });
    await waitFor(() => expect(refreshResolvers).toHaveLength(2));
    refreshResolvers[0]?.({
      id: "prj_1",
      display_name: "Retained Project",
      local_path: "/tmp/prj_1",
      created_at: "2026-08-04T00:00:00Z",
      updated_at: "2026-08-04T00:00:00Z",
    });

    await new Promise((resolve) => setTimeout(resolve, 0));
    const button = screen.queryByRole("button", { name: /Create (your first|another) worktree/i });
    expect(button === null || button.hasAttribute("disabled")).toBe(true);

    for (const resolve of refreshResolvers.slice(1)) {
      resolve({
        id: "prj_1",
        display_name: "Retained Project",
        local_path: "/tmp/prj_1",
        created_at: "2026-08-04T00:00:00Z",
        updated_at: "2026-08-04T00:00:00Z",
      });
    }
  });

  it("surfaces an invalid new-worktree acknowledgement", async () => {
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: vi.fn().mockResolvedValue(undefined),
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();
    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);
    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("myrepo");

    await fireEvent.click(screen.getByRole("button", { name: /Create your first worktree/i }));

    await waitFor(() => {
      expect(mocks.showFlash).toHaveBeenCalledWith("The host returned an invalid worktree acknowledgement.", {
        tone: "danger",
      });
    });
  });

  it("surfaces a failure message when the new-worktree action returns ok: false", async () => {
    win.__kenn_forge_config = {
      actions: {
        project: [
          {
            id: "new-worktree",
            label: "New Worktree",
            handler: () =>
              Promise.resolve({
                ok: false,
                message: "user cancelled the sheet",
              }),
          },
        ],
      },
    };
    win.__kenn_forge_notify_config_changed?.();

    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("myrepo");
    await fireEvent.click(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    );
    await waitFor(() => {
      expect(mocks.showFlash).toHaveBeenCalledWith("user cancelled the sheet", {
        tone: "danger",
      });
    });
    expect(screen.queryByText("user cancelled the sheet")).toBeNull();
  });

  it("renders an upgrade-host hint when the new-worktree action is missing", async () => {
    win.__kenn_forge_config = { actions: { project: [] } };
    win.__kenn_forge_notify_config_changed?.();

    setProjectResponse({
      id: "prj_1",
      display_name: "myrepo",
      local_path: "/tmp/myrepo",
    });
    setWorktreesResponse([]);

    render(WorkspaceProjectCard, { props: { projectId: "prj_1" } });
    await screen.findByText("myrepo");
    await fireEvent.click(
      screen.getByRole("button", {
        name: /Create your first worktree/i,
      }),
    );
    expect(mocks.showFlash).toHaveBeenCalledWith(
      "New Worktree is not available in this build. Please update the host application.",
      { tone: "danger" },
    );
    expect(screen.queryByText(/not available in this build/i)).toBeNull();
  });
});
