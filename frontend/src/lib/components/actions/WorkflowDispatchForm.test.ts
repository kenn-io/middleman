import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import type { components } from "../../api/generated/schema.js";
import type { OperationAvailability } from "../../api/types.js";
import WorkflowDispatchForm from "./WorkflowDispatchForm.svelte";

type Workflow = components["schemas"]["WorkflowDefinitionResponse"];
type Environment = components["schemas"]["WorkflowEnvironmentResponse"];

const environments: Environment[] = [{ name: "staging" }, { name: "production" }];
const available: OperationAvailability = { available: true };

function dropdown(name: string): HTMLElement {
  return screen.getByRole("combobox", { name });
}

async function chooseDropdownOption(name: string, option: string): Promise<void> {
  await fireEvent.click(dropdown(name));
  await fireEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: option }));
  expect(dropdown(name).textContent).toContain(option);
}

function workflow(inputs: NonNullable<Workflow["inputs"]> = []): Workflow {
  return {
    available: true,
    definition_sha: "definition-1",
    id: "deploy.yml",
    inputs,
    name: "Deploy",
    path: ".github/workflows/deploy.yml",
    state: "active",
    web_url: "https://github.com/acme/app/actions/workflows/deploy.yml",
  };
}

describe("WorkflowDispatchForm", () => {
  it("renders and submits typed defaults in declared control order", async () => {
    const onsubmit = vi.fn();
    const { container } = render(WorkflowDispatchForm, {
      workflow: workflow([
        { name: "message", type: "string", required: false, has_default: true, default: "hello" },
        { name: "retries", type: "number", required: false, has_default: true, default: 3 },
        { name: "dry_run", type: "boolean", required: false, has_default: true, default: true },
        { name: "region", type: "choice", required: false, has_default: true, default: "eu", options: ["eu", "us"] },
        { name: "target", type: "environment", required: false, has_default: true, default: "staging" },
      ]),
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    expect(
      [...container.querySelectorAll(".field")].map((field) =>
        (field.matches("label") ? field : field.querySelector("label"))?.textContent?.trim(),
      ),
    ).toEqual(["Git ref *", "message", "retries", "dry_run", "region", "target"]);
    expect((screen.getByRole("textbox", { name: "message" }) as HTMLInputElement).value).toBe("hello");
    expect((screen.getByRole("spinbutton", { name: "retries" }) as HTMLInputElement).valueAsNumber).toBe(3);
    expect((screen.getByRole("checkbox", { name: "dry_run" }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.click(dropdown("region"));
    expect(
      within(screen.getByRole("listbox"))
        .getAllByRole("option")
        .map((option) => option.textContent?.trim()),
    ).toEqual(["eu", "us"]);
    await fireEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: "eu" }));
    await fireEvent.click(dropdown("target"));
    expect(
      within(screen.getByRole("listbox"))
        .getAllByRole("option")
        .map((option) => option.textContent?.trim()),
    ).toEqual(["Select an environment", "staging", "production"]);
    await fireEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: "staging" }));
    expect(container.querySelector("select")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(onsubmit).toHaveBeenCalledWith({
      ref: "main",
      inputs: { message: "hello", retries: 3, dry_run: true, region: "eu", target: "staging" },
    });
  });

  it("validates required inputs and editable ref, then submits one normalized request", async () => {
    const onsubmit = vi.fn();
    render(WorkflowDispatchForm, {
      workflow: workflow([
        { name: "version", type: "string", required: true, has_default: false },
        { name: "count", type: "number", required: true, has_default: false },
        { name: "approved", type: "boolean", required: false, has_default: false },
      ]),
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    const ref = screen.getByRole("textbox", { name: "Git ref" });
    expect((ref as HTMLInputElement).value).toBe("main");
    await fireEvent.input(ref, { target: { value: "" } });
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect((await screen.findByText("Git ref is required.")).getAttribute("role")).toBe("alert");
    expect(screen.getByText("version is required.")).toBeTruthy();
    expect(screen.getByText("count is required.")).toBeTruthy();
    expect(onsubmit).not.toHaveBeenCalled();

    await fireEvent.input(ref, { target: { value: " feature/test " } });
    await fireEvent.input(screen.getByRole("textbox", { name: "version" }), { target: { value: " v2 " } });
    await fireEvent.input(screen.getByRole("spinbutton", { name: "count" }), { target: { value: "4" } });
    await fireEvent.click(screen.getByRole("checkbox", { name: "approved" }));
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(onsubmit).toHaveBeenCalledTimes(1);
    expect(onsubmit).toHaveBeenCalledWith({ ref: "feature/test", inputs: { version: "v2", count: 4, approved: true } });
  });

  it("rejects whitespace-only required strings and wires stable inline errors", async () => {
    render(WorkflowDispatchForm, {
      workflow: workflow([{ name: "release name", type: "string", required: true, has_default: false }]),
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit: vi.fn(),
    });
    const input = screen.getByRole("textbox", { name: "release name" });
    await fireEvent.input(input, { target: { value: "   " } });
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    const error = screen.getByText("release name is required.");
    expect(error.id).toBe("workflow-input-release-name-0-error");
    expect(input.getAttribute("aria-describedby")).toBe(error.id);
  });

  it("preserves edits across presentation updates, resets on definition identity, and latches rapid admission", async () => {
    const onsubmit = vi.fn();
    const definition = workflow([
      { name: "version", type: "string", required: false, has_default: true, default: "v1" },
    ]);
    const props = {
      workflow: definition,
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" } as const,
      onsubmit,
    };
    const view = render(WorkflowDispatchForm, props);
    await fireEvent.input(screen.getByRole("textbox", { name: "Git ref" }), { target: { value: "feature" } });
    await fireEvent.input(screen.getByRole("textbox", { name: "version" }), { target: { value: "v2" } });
    const run = screen.getByRole("button", { name: "Run workflow" });
    await Promise.all([fireEvent.click(run), fireEvent.click(run)]);
    expect(onsubmit).toHaveBeenCalledTimes(1);

    await view.rerender({ ...props, state: { kind: "pending" } });
    await view.rerender({ ...props, state: { kind: "idle" } });
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("feature");
    expect((screen.getByRole("textbox", { name: "version" }) as HTMLInputElement).value).toBe("v2");

    await view.rerender({
      ...props,
      workflow: { ...definition, definition_sha: "definition-2" },
      initialRef: "release",
    });
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("release");
    expect((screen.getByRole("textbox", { name: "version" }) as HTMLInputElement).value).toBe("v1");
  });

  it.each([
    [{ id: "release.yml" }, "identity"],
    [{ definition_sha: "definition-2" }, "definition"],
  ])("derives new inputs when only workflow %s changes at the same ref", async (replacement) => {
    const original = workflow([{ name: "version", type: "string", required: false, has_default: true, default: "v1" }]);
    const view = render(WorkflowDispatchForm, {
      workflow: original,
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit: vi.fn(),
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "version" }), { target: { value: "edited" } });
    await view.rerender({
      workflow: {
        ...original,
        ...replacement,
        inputs: [
          {
            name: "channel",
            type: "choice",
            required: false,
            has_default: true,
            default: "beta",
            options: ["beta", "stable"],
          },
        ],
      },
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit: vi.fn(),
    });
    expect(screen.queryByRole("textbox", { name: "version" })).toBeNull();
    expect(dropdown("channel").getAttribute("aria-label")).toBe("channel");
    expect(dropdown("channel").textContent).toContain("beta");
  });

  it("releases admission only after owner leaves idle and starts a fresh idle cycle", async () => {
    const onsubmit = vi.fn();
    const props = { workflow: workflow(), environments, initialRef: "main", operation: available, onsubmit };
    const view = render(WorkflowDispatchForm, { ...props, state: { kind: "idle" } as const });
    const run = screen.getByRole("button", { name: "Run workflow" });
    await Promise.all([fireEvent.click(run), fireEvent.click(run)]);
    expect(onsubmit).toHaveBeenCalledTimes(1);
    await view.rerender({ ...props, state: { kind: "idle" } });
    await fireEvent.click(screen.getByRole("button", { name: "Running workflow…" }));
    expect(onsubmit).toHaveBeenCalledTimes(1);
    await view.rerender({ ...props, state: { kind: "pending" } });
    await view.rerender({ ...props, state: { kind: "succeeded" } });
    await view.rerender({ ...props, state: { kind: "idle" } });
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(onsubmit).toHaveBeenCalledTimes(2);
  });

  it("uses collision-free declared-index IDs for labels and described errors", async () => {
    render(WorkflowDispatchForm, {
      workflow: workflow([
        { name: "release-name", type: "string", required: true, has_default: false },
        { name: "release_name", type: "string", required: true, has_default: false },
      ]),
      environments,
      initialRef: "main",
      operation: available,
      state: { kind: "idle" },
      onsubmit: vi.fn(),
    });
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    const first = screen.getByRole("textbox", { name: "release-name" });
    const second = screen.getByRole("textbox", { name: "release_name" });
    expect(first.id).not.toBe(second.id);
    expect(first.getAttribute("aria-describedby")).toBe(`${first.id}-error`);
    expect(second.getAttribute("aria-describedby")).toBe(`${second.id}-error`);
    expect(document.getElementById(`${first.id}-error`)?.textContent).toBe("release-name is required.");
    expect(document.getElementById(`${second.id}-error`)?.textContent).toBe("release_name is required.");
  });

  it("uses fallback messages for explicit unavailable booleans", async () => {
    const props = {
      workflow: workflow(),
      environments,
      initialRef: "main",
      state: { kind: "idle" } as const,
      onsubmit: vi.fn(),
    };
    const view = render(WorkflowDispatchForm, { ...props, operation: { available: false, unavailable_reason: "" } });
    expect(screen.getByRole("alert").textContent).toBe("Workflow dispatch is unavailable.");
    await view.rerender({
      ...props,
      workflow: { ...workflow(), available: false, unavailable_reason: "" },
      operation: available,
    });
    expect(screen.getByRole("alert").textContent).toBe("This workflow is unavailable.");
  });

  it("surfaces unavailable, pending, uncertain, and conflict states without duplicate dispatch", async () => {
    const onsubmit = vi.fn();
    const props = {
      workflow: workflow(),
      environments,
      initialRef: "main",
      onsubmit,
      operation: { available: false, unavailable_reason: "No write credential" } as OperationAvailability,
      state: { kind: "idle" } as const,
    };
    const view = render(WorkflowDispatchForm, props);
    expect(screen.getByText("No write credential")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Run workflow" }) as HTMLButtonElement).disabled).toBe(true);

    await view.rerender({ ...props, operation: available, state: { kind: "pending" } });
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Running workflow…" }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole("button", { name: "Running workflow…" }));
    expect(onsubmit).not.toHaveBeenCalled();

    await view.rerender({
      ...props,
      operation: available,
      state: {
        kind: "uncertain",
        message: "The provider may have accepted this run. Verify on the provider before trying again.",
      },
    });
    expect(screen.getByRole("alert").textContent).toContain("may have accepted");
    expect(screen.queryByRole("button", { name: "Run workflow" })).toBeNull();

    await view.rerender({ ...props, operation: available, state: { kind: "conflict" } });
    expect(screen.getByRole("alert").textContent).toContain(
      "Workflow definition changed. Reload workflows before running it.",
    );
    expect(screen.queryByRole("textbox", { name: "Git ref" })).toBeNull();
  });

  it("requires explicit confirmation and submits an empty input map", async () => {
    const onsubmit = vi.fn();
    render(WorkflowDispatchForm, {
      workflow: workflow(),
      environments,
      initialRef: "release",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });
    expect(screen.getByRole("heading", { name: "Deploy" })).toBeTruthy();
    expect((screen.getByRole("textbox", { name: "Git ref" }) as HTMLInputElement).value).toBe("release");
    expect(onsubmit).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(onsubmit).toHaveBeenCalledWith({ ref: "release", inputs: {} });
  });

  it("exposes native required semantics without treating a required false boolean as missing", async () => {
    const onsubmit = vi.fn();
    const view = render(WorkflowDispatchForm, {
      workflow: workflow([
        { name: "message", type: "string", required: true, has_default: false },
        { name: "retries", type: "number", required: true, has_default: false },
        { name: "approved", type: "boolean", required: true, has_default: false },
        { name: "channel", type: "choice", required: true, has_default: false, options: ["stable", "beta"] },
        { name: "target", type: "environment", required: true, has_default: false },
      ]),
      environments,
      initialRef: "trunk",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    for (const control of [
      screen.getByRole("textbox", { name: "Git ref" }),
      screen.getByRole("textbox", { name: "message" }),
      screen.getByRole("spinbutton", { name: "retries" }),
    ]) {
      expect((control as HTMLInputElement).required).toBe(true);
      expect(control.getAttribute("aria-required")).toBe("true");
    }
    const channel = dropdown("channel");
    const target = dropdown("target");
    expect(channel.getAttribute("aria-required")).toBe("true");
    expect(target.getAttribute("aria-required")).toBe("true");
    expect(document.querySelector("select")).toBeNull();

    const approved = screen.getByRole("checkbox", { name: "approved" });
    expect((approved as HTMLInputElement).required).toBe(false);
    const requiredDescription = document.getElementById(approved.getAttribute("aria-describedby") ?? "");
    expect(requiredDescription?.textContent).toContain("Required input");

    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(channel.getAttribute("aria-describedby")).toBe("workflow-input-channel-3-error");
    expect(target.getAttribute("aria-describedby")).toBe("workflow-input-target-4-error");
    expect(channel.getAttribute("aria-invalid")).toBe("true");
    expect(target.getAttribute("aria-invalid")).toBe("true");

    await view.rerender({
      workflow: workflow([
        { name: "message", type: "string", required: true, has_default: false },
        { name: "retries", type: "number", required: true, has_default: false },
        { name: "approved", type: "boolean", required: true, has_default: false },
        { name: "channel", type: "choice", required: true, has_default: false, options: ["stable", "beta"] },
        { name: "target", type: "environment", required: true, has_default: false },
      ]),
      environments,
      initialRef: "trunk",
      operation: available,
      state: { kind: "pending" },
      onsubmit,
    });
    expect((dropdown("channel") as HTMLButtonElement).disabled).toBe(true);
    expect((dropdown("target") as HTMLButtonElement).disabled).toBe(true);
    await view.rerender({
      workflow: workflow([
        { name: "message", type: "string", required: true, has_default: false },
        { name: "retries", type: "number", required: true, has_default: false },
        { name: "approved", type: "boolean", required: true, has_default: false },
        { name: "channel", type: "choice", required: true, has_default: false, options: ["stable", "beta"] },
        { name: "target", type: "environment", required: true, has_default: false },
      ]),
      environments,
      initialRef: "trunk",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    await fireEvent.input(screen.getByRole("textbox", { name: "message" }), { target: { value: "release" } });
    await fireEvent.input(screen.getByRole("spinbutton", { name: "retries" }), { target: { value: "2" } });
    await chooseDropdownOption("channel", "stable");
    await chooseDropdownOption("target", "production");
    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));

    expect(onsubmit).toHaveBeenCalledWith({
      ref: "trunk",
      inputs: {
        message: "release",
        retries: 2,
        approved: false,
        channel: "stable",
        target: "production",
      },
    });
  });

  it("omits an optional blank number without treating it as non-finite", async () => {
    const onsubmit = vi.fn();
    render(WorkflowDispatchForm, {
      workflow: workflow([
        {
          name: "retries",
          type: "number",
          required: false,
          has_default: false,
        },
      ]),
      environments,
      initialRef: "trunk",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(onsubmit).toHaveBeenCalledWith({ ref: "trunk", inputs: {} });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it.each([
    ["NaN", Number.NaN],
    ["1e999", Number("1e999")],
  ])("rejects non-finite number default %s before dispatch", async (_label, value) => {
    const onsubmit = vi.fn();
    render(WorkflowDispatchForm, {
      workflow: workflow([
        {
          name: "retries",
          type: "number",
          required: true,
          has_default: true,
          default: value,
        },
      ]),
      environments,
      initialRef: "trunk",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    await fireEvent.click(screen.getByRole("button", { name: "Run workflow" }));
    expect(screen.getByRole("alert").textContent).toBe("retries must be a finite number.");
    expect(onsubmit).not.toHaveBeenCalled();
  });

  it.each([
    ["choice", [], "channel", "No choices are available for channel."],
    ["environment", [], "target", "No environments are available for target."],
  ] as const)(
    "disables dispatch when a %s input has zero provider options",
    (type, environmentOptions, name, message) => {
      const onsubmit = vi.fn();
      render(WorkflowDispatchForm, {
        workflow: workflow([
          {
            name,
            type,
            required: true,
            has_default: false,
            ...(type === "choice" && { options: [] }),
          },
        ]),
        environments: environmentOptions,
        initialRef: "trunk",
        operation: available,
        state: { kind: "idle" },
        onsubmit,
      });

      expect(screen.getByRole("alert").textContent).toContain(message);
      expect((screen.getByRole("button", { name: "Run workflow" }) as HTMLButtonElement).disabled).toBe(true);
      expect(screen.queryByText(`${name} is required.`)).toBeNull();
      expect(onsubmit).not.toHaveBeenCalled();
    },
  );

  it.each([
    ["choice", [], "channel"],
    ["environment", [], "target"],
  ] as const)("omits an optional %s input with zero provider options", async (type, environmentOptions, name) => {
    const onsubmit = vi.fn();
    render(WorkflowDispatchForm, {
      workflow: workflow([
        {
          name,
          type,
          required: false,
          has_default: false,
          ...(type === "choice" && { options: [] }),
        },
      ]),
      environments: environmentOptions,
      initialRef: "trunk",
      operation: available,
      state: { kind: "idle" },
      onsubmit,
    });

    expect(screen.queryByRole("alert")).toBeNull();
    const run = screen.getByRole("button", { name: "Run workflow" }) as HTMLButtonElement;
    expect(run.disabled).toBe(false);
    await fireEvent.click(run);
    expect(onsubmit).toHaveBeenCalledWith({ ref: "trunk", inputs: {} });
  });

  it("announces locating and renders concrete accepted run details without a running submit label", async () => {
    const props = {
      workflow: workflow(),
      environments,
      initialRef: "trunk",
      operation: available,
      onsubmit: vi.fn(),
    };
    const view = render(WorkflowDispatchForm, {
      ...props,
      state: { kind: "locating" } as const,
    });
    expect(screen.getByRole("status").textContent).toContain("Locating run…");
    expect(screen.queryByRole("button", { name: "Running workflow…" })).toBeNull();

    await view.rerender({
      ...props,
      state: {
        kind: "succeeded",
        run: {
          actor: "maintainer",
          conclusion: "",
          event: "workflow_dispatch",
          head_sha: "0123456789abcdef",
          id: "run-42",
          name: "Deploy",
          ref: "trunk",
          run_number: 42,
          status: "queued",
          web_url: "https://github.com/acme/app/actions/runs/42",
          workflow_id: "deploy.yml",
        },
      } as const,
    });
    expect(screen.getByText("run-42")).toBeTruthy();
    expect(screen.getByText("0123456789abcdef")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open accepted run on provider" }).getAttribute("href")).toBe(
      "https://github.com/acme/app/actions/runs/42",
    );
    expect(screen.queryByRole("button", { name: "Running workflow…" })).toBeNull();
  });
});
