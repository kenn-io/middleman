import { cleanup, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, expect, it } from "vite-plus/test";
import AgentStatusIndicator from "./AgentStatusIndicator.svelte";
import { setShowListAgentStatus } from "../../stores/list-agent-status.svelte.js";

afterEach(() => {
  cleanup();
  setShowListAgentStatus(false);
  localStorage.clear();
});

it("updates existing list indicators when the display preference changes", async () => {
  render(AgentStatusIndicator, { state: "working" });
  expect(screen.queryByText("Working")).toBeNull();
  setShowListAgentStatus(true);
  await tick();
  expect(screen.getByText("Working")).toBeTruthy();
  setShowListAgentStatus(false);
  await tick();
  expect(screen.queryByText("Working")).toBeNull();
});

it.each([
  ["working", "Working"],
  ["approval", "Approval"],
  ["input", "Input"],
  ["done", "Done"],
])("shows the linked agent's %s state", (agentState, label) => {
  setShowListAgentStatus(true);
  render(AgentStatusIndicator, { state: agentState });
  expect(screen.getByText(label)).toBeTruthy();
  expect(screen.getByLabelText(`Agent ${label.toLowerCase()}`)).toBeTruthy();
});

it("removes the label when the live agent state clears", async () => {
  setShowListAgentStatus(true);
  const { rerender } = render(AgentStatusIndicator, { state: "done" });
  await rerender({ state: undefined });
  expect(screen.queryByText("Done")).toBeNull();
});
