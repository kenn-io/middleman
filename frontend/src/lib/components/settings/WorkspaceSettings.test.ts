import { getShowListAgentStatus, setShowListAgentStatus } from "../../stores/list-agent-status.svelte.js";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect, Layer } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { Settings } from "../../api/types.js";

type WorkspaceSettings = Settings["workspaces"];

const { mockPersistSettings, workspaceStore } = vi.hoisted(() => ({
  mockPersistSettings: vi.fn(),
  workspaceStore: {
    current: undefined as unknown as {
      getWorkspaceSettings: () => WorkspaceSettings;
      setWorkspaceSettings: (settings: WorkspaceSettings) => void;
      getRoborevSettings: () => Settings["roborev"];
      setRoborevSettings: (settings: Settings["roborev"]) => void;
    },
  },
}));

vi.mock("../../context.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../context.js")>()),
  getStores: () => ({ settings: workspaceStore.current }),
}));

vi.mock("../../stores/settings-workflow.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../stores/settings-workflow.js")>();
  return {
    ...actual,
    SettingsWorkflowLive: Layer.mock(actual.SettingsWorkflow)({
      persist: (request) => mockPersistSettings(request),
    }),
  };
});

vi.mock("../../stores/embed-config.svelte.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../stores/embed-config.svelte.js")>()),
  isEmbedded: () => false,
}));

import WorkspaceSettings from "./WorkspaceSettings.svelte";
import SettingsRuntimeHarness from "./SettingsRuntimeHarness.svelte";
import { createSettingsStore } from "../../stores/settings.svelte.js";

const initial: WorkspaceSettings = { auto_assign_on_create: false, default_sidebar_view: "diff" };

describe("WorkspaceSettings", () => {
  beforeEach(() => {
    workspaceStore.current = createSettingsStore();
  });

  afterEach(() => {
    cleanup();
    setShowListAgentStatus(false);
    mockPersistSettings.mockReset();
  });

  it("persists the list agent status toggle in this browser", async () => {
    setShowListAgentStatus(false);
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate: vi.fn() } },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Show agent status in lists" }));
    expect(getShowListAgentStatus()).toBe(true);
    expect(localStorage.getItem("kenn-forge:show-list-agent-status")).toBe("true");
    expect(mockPersistSettings).not.toHaveBeenCalled();
  });

  it("saves automatic assignment for new workspace items", async () => {
    const onUpdate = vi.fn();
    const saved = { ...initial, auto_assign_on_create: true };
    mockPersistSettings.mockReturnValue(Effect.succeed({ workspaces: saved }));
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate } },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Assign new workspace items to me" }));

    await waitFor(() => expect(mockPersistSettings).toHaveBeenCalledOnce());
    expect(mockPersistSettings.mock.calls[0]?.[0]()).toEqual({ workspaces: { auto_assign_on_create: true } });
    expect(onUpdate).toHaveBeenNthCalledWith(1, saved);
    expect(onUpdate).toHaveBeenLastCalledWith(saved);
  });

  it("saves managed-clone Roborev initialization through the Roborev settings object", async () => {
    const onUpdate = vi.fn();
    const onRoborevUpdate = vi.fn();
    const saved = { init_managed_clones: true };
    mockPersistSettings.mockReturnValue(Effect.succeed({ roborev: saved }));
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate, onRoborevUpdate } },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Initialize Roborev in managed clones" }));

    await waitFor(() => expect(mockPersistSettings).toHaveBeenCalledOnce());
    expect(mockPersistSettings.mock.calls[0]?.[0]()).toEqual({ roborev: { init_managed_clones: true } });
    expect(onRoborevUpdate).toHaveBeenNthCalledWith(1, saved);
    expect(onRoborevUpdate).toHaveBeenLastCalledWith(saved);
  });

  it("rolls back a failed Roborev save without changing workspace preferences", async () => {
    const onUpdate = vi.fn();
    const onRoborevUpdate = vi.fn((roborev: Settings["roborev"]) => workspaceStore.current.setRoborevSettings(roborev));
    mockPersistSettings.mockReturnValue(
      Effect.fail({ _tag: "TransientTransportError", operation: "save settings", cause: new Error("save failed") }),
    );
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate, onRoborevUpdate } },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Initialize Roborev in managed clones" }));

    await waitFor(() => expect(onRoborevUpdate).toHaveBeenCalledTimes(2));
    expect(onRoborevUpdate).toHaveBeenLastCalledWith({ init_managed_clones: false });
    expect(workspaceStore.current.getWorkspaceSettings()).toEqual(initial);
    expect(onUpdate).not.toHaveBeenCalled();
  });

  it("saves the default sidebar view without changing automatic assignment", async () => {
    const onUpdate = vi.fn();
    const saved = { ...initial, default_sidebar_view: "item" as const };
    mockPersistSettings.mockReturnValue(Effect.succeed({ workspaces: saved }));
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate } },
    });

    await fireEvent.click(screen.getByRole("combobox", { name: "Default sidebar view: Diff" }));
    await fireEvent.click(screen.getByRole("option", { name: "PR/Issue" }));

    await waitFor(() => expect(mockPersistSettings).toHaveBeenCalledOnce());
    expect(mockPersistSettings.mock.calls[0]?.[0]()).toEqual({ workspaces: { default_sidebar_view: "item" } });
    expect(onUpdate).toHaveBeenNthCalledWith(1, saved);
    expect(onUpdate).toHaveBeenLastCalledWith(saved);
  });

  it("restores the prior setting when saving fails", async () => {
    const onUpdate = vi.fn((workspaces: typeof initial) => workspaceStore.current.setWorkspaceSettings(workspaces));
    mockPersistSettings.mockReturnValue(
      Effect.fail({ _tag: "TransientTransportError", operation: "save settings", cause: new Error("save failed") }),
    );
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate } },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Assign new workspace items to me" }));

    await waitFor(() => expect(onUpdate).toHaveBeenCalledTimes(2));
    expect(onUpdate).toHaveBeenLastCalledWith(initial);
  });

  it("preserves a hydrated sibling setting when saving the sidebar view", async () => {
    const hydrated = { ...initial, auto_assign_on_create: true };
    const saved = { ...hydrated, default_sidebar_view: "item" as const };
    const onUpdate = vi.fn();
    mockPersistSettings.mockReturnValue(Effect.succeed({ workspaces: saved }));
    render(SettingsRuntimeHarness, {
      props: { component: WorkspaceSettings, componentProps: { onUpdate } },
    });

    workspaceStore.current.setWorkspaceSettings(hydrated);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: "Assign new workspace items to me" }).getAttribute("aria-pressed"),
      ).toBe("true");
    });

    await fireEvent.click(screen.getByRole("combobox", { name: "Default sidebar view: Diff" }));
    await fireEvent.click(screen.getByRole("option", { name: "PR/Issue" }));

    await waitFor(() => expect(mockPersistSettings).toHaveBeenCalledOnce());
    expect(onUpdate).toHaveBeenNthCalledWith(1, saved);
  });
});
