import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { Effect, Layer } from "effect";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import type { ModeVisibility } from "../../api/types.js";

const { mockSetModeVisibility, mockPersistSettings } = vi.hoisted(() => ({
  mockSetModeVisibility: vi.fn(),
  mockPersistSettings: vi.fn(),
}));

vi.mock("../../context.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../context.js")>()),
  getStores: () => ({
    settings: {
      setModeVisibility: mockSetModeVisibility,
    },
  }),
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

import ModeVisibilitySettings from "./ModeVisibilitySettings.svelte";
import SettingsRuntimeHarness from "./SettingsRuntimeHarness.svelte";

function defaultModes(): ModeVisibility {
  return {
    activity: true,
    repos: true,
    actions: false,
    docs: false,
    pulls: true,
    issues: true,
    reviews: true,
    workspaces: true,
  };
}

describe("ModeVisibilitySettings", () => {
  afterEach(() => {
    cleanup();
    mockSetModeVisibility.mockReset();
    mockPersistSettings.mockReset();
  });

  it("persists visible mode changes", async () => {
    const modes = defaultModes();
    const updated = {
      ...modes,
      docs: true,
      actions: true,
      workspaces: false,
    };
    mockPersistSettings.mockReturnValue(Effect.succeed({ modes: updated }));
    const onUpdate = vi.fn();

    render(SettingsRuntimeHarness, {
      props: { component: ModeVisibilitySettings, componentProps: { modes, onUpdate } },
    });

    expect((screen.getByLabelText("Actions") as HTMLInputElement).checked).toBe(false);
    expect((screen.getByLabelText("Docs") as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByLabelText("Messages")).toBeNull();
    expect(screen.queryByLabelText("Board")).toBeNull();

    await fireEvent.click(screen.getByLabelText("Actions"));
    await fireEvent.click(screen.getByLabelText("Docs"));
    await fireEvent.click(screen.getByLabelText("Workspaces"));
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockPersistSettings).toHaveBeenCalledOnce();
    });
    expect(mockPersistSettings.mock.calls[0]?.[0]()).toEqual({ modes: updated });
    expect(mockSetModeVisibility).toHaveBeenCalledWith(updated);
    expect(onUpdate).toHaveBeenCalledWith(updated);
  });
});
