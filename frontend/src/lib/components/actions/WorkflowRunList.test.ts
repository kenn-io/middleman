import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { expect, it, vi } from "vitest";
import type { components } from "../../api/generated/schema.js";
import WorkflowRunList from "./WorkflowRunList.svelte";

type Run = components["schemas"]["WorkflowRunResponse"];
type Job = components["schemas"]["WorkflowRunJobResponse"];

const runs: Run[] = [
  {
    actor: "octocat",
    conclusion: "success",
    created_at: "2026-08-27T12:30:00Z",
    event: "workflow_dispatch",
    head_sha: "0123456789abcdef",
    id: "run-2",
    name: "Deploy",
    ref: "main",
    run_number: 42,
    status: "completed",
    updated_at: "2026-08-27T12:32:00Z",
    web_url: "https://github.com/acme/app/actions/runs/2",
    workflow_id: "deploy.yml",
  },
];
const jobs: Record<string, readonly Job[]> = {
  "run-2": [
    { id: "job-9", name: "Verify", status: "completed", conclusion: "success", steps: [] },
    {
      id: "job-2",
      name: "Publish",
      status: "completed",
      conclusion: "success",
      steps: [
        { number: 2, name: "Upload", status: "completed", conclusion: "success" },
        { number: 1, name: "Build", status: "completed", conclusion: "success" },
      ],
    },
  ],
};

it("exposes compact textual run data, local time, and secure provider links", () => {
  render(WorkflowRunList, { runs, jobs: {}, loadingJobs: [], onexpand: vi.fn() });
  const row = screen.getByRole("button", { name: /Run 42 Deploy/ });
  expect(row.textContent).toContain("#42");
  expect(row.textContent).toContain("Deploy");
  expect(row.textContent).toContain("main");
  expect(row.textContent).toContain("octocat");
  expect(row.textContent).toContain("completed · success");
  expect(row.textContent).toContain("0123456");
  expect(row.textContent).toContain(new Date("2026-08-27T12:30:00Z").toLocaleString());
  expect(screen.getByRole("link", { name: "Open on GitHub" }).getAttribute("rel")).toBe("noopener");
  expect(screen.getByRole("link", { name: "Open on GitHub" }).getAttribute("target")).toBe("_blank");
});

it.each([
  ["https://gitlab.com/acme/app/-/pipelines/2", "GitLab"],
  ["https://forge.acme.test/acme/app/actions/runs/2", "forge.acme.test"],
])("labels provider links for %s", (webURL, providerLabel) => {
  render(WorkflowRunList, {
    runs: [{ ...runs[0]!, web_url: webURL }],
    jobs: {},
    loadingJobs: [],
    onexpand: vi.fn(),
  });
  expect(screen.getByRole("link", { name: `Open on ${providerLabel}` })).toBeTruthy();
});

it("omits unsafe provider links", () => {
  render(WorkflowRunList, {
    runs: [{ ...runs[0]!, web_url: "javascript:alert(document.domain)" }],
    jobs: {},
    loadingJobs: [],
    onexpand: vi.fn(),
  });
  expect(screen.queryByRole("link")).toBeNull();
});

it("requests jobs only when a run expands and preserves provider order", async () => {
  const onexpand = vi.fn();
  const view = render(WorkflowRunList, { runs, jobs, loadingJobs: [], onexpand });
  const disclosure = screen.getByRole("button", { name: /Run 42 Deploy/ });
  expect(disclosure.getAttribute("aria-expanded")).toBe("false");

  await fireEvent.click(disclosure);
  expect(onexpand).toHaveBeenCalledTimes(1);
  expect(onexpand).toHaveBeenCalledWith("run-2");
  expect(disclosure.getAttribute("aria-expanded")).toBe("true");
  expect(screen.getAllByRole("button", { name: /Verify|Publish/ }).map((item) => item.textContent)).toEqual([
    expect.stringContaining("Verify"),
    expect.stringContaining("Publish"),
  ]);
  const job = screen.getByRole("button", { name: /Publish/ });
  expect(job.textContent).toContain("completed · success");
  await fireEvent.click(job);
  const steps = screen.getByRole("list", { name: "Publish steps" });
  expect(
    within(steps)
      .getAllByRole("listitem")
      .map((item) => item.textContent),
  ).toEqual([expect.stringContaining("Upload"), expect.stringContaining("Build")]);

  await fireEvent.click(disclosure);
  expect(disclosure.getAttribute("aria-expanded")).toBe("false");
  expect(onexpand).toHaveBeenCalledTimes(1);
  await fireEvent.click(disclosure);
  expect(onexpand).toHaveBeenCalledTimes(2);

  await view.rerender({ runs, jobs, loadingJobs: ["run-2"], onexpand });
  expect(screen.getByText("Loading jobs…").getAttribute("role")).toBe("status");
});
