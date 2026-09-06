// Guards the review drawer footer layout invariant: the action buttons are a
// single horizontal group and never stack, when the footer runs out of room it
// is the token usage summary that drops to a second row, and once even that is
// not enough the group downgrades to icon-only controls that stay inside the
// footer.
//
// The original bug was exactly this failure: a long non-wrapping usage string
// squeezed the actions until they wrapped one-per-line, turning the drawer's
// primary controls into what read as a vertical list of links. The narrow case
// is the same bug's other half -- a non-shrinking row wider than the drawer is
// clipped by the workspace sidebar's overflow, putting actions out of reach.
// Nothing about either is observable in jsdom -- both need real wrapping and
// real rects -- and neither needs a backend, so this belongs in the browser
// lane rather than Playwright.

import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, render } from "vitest-browser-svelte";
import { Effect } from "effect";

// Layout assertions are only meaningful under the production reset and tokens.
import "./app.css";
import { makeAppRuntime } from "./lib/app/runtime.js";
import { STORES_KEY } from "./lib/context.js";
import ReviewDrawerRuntimeHarness from "./lib/components/roborev/ReviewDrawerRuntimeHarness.svelte";

// A realistic worst case: the blob that originally overflowed the footer.
const job = {
  id: 8549,
  agent: "codex",
  agentic: false,
  enqueued_at: "2026-07-15T00:00:00Z",
  finished_at: "2026-07-15T00:01:00Z",
  git_ref: "f64141d9aa",
  branch: "t3code/session-start-workspace-context",
  job_type: "review",
  prompt_prebuilt: false,
  repo_id: 1,
  repo_name: "example/repo",
  retry_count: 0,
  started_at: "2026-07-15T00:00:10Z",
  status: "done",
  token_usage: JSON.stringify({
    input_tokens: 231582,
    cached_input_tokens: 189952,
    total_output_tokens: 2542,
    peak_context_tokens: 47248,
    cost_usd: 0.347212,
    has_cost: true,
  }),
};

function mountAt(widthPx: number): { unmount: () => Promise<void> } {
  const wrapper = document.createElement("div");
  wrapper.style.width = `${widthPx}px`;
  document.body.appendChild(wrapper);
  const runtime = makeAppRuntime();

  const { unmount } = render(ReviewDrawerRuntimeHarness, {
    target: wrapper,
    props: { runtime },
    context: new Map<symbol, unknown>([
      [
        STORES_KEY,
        {
          roborevJobs: {
            getVisibleJobs: () => [job],
            getSelectedJobId: () => job.id,
            deselectJob: vi.fn(),
            rerunJob: vi.fn(),
            cancelJob: vi.fn(),
            getPanelMemberError: () => undefined,
            isLoadingMembers: () => false,
            getPanelMembers: () => undefined,
            setPanelMemberInterest: vi.fn(),
            refreshPanelMembers: vi.fn(),
          },
          // The real tab bodies render against this too; the drawer footer is
          // measured with its actual siblings in place rather than stubs.
          roborevReview: {
            getSelectedJob: () => null,
            getSelectedJobId: () => job.id,
            getOutput: () => "No issues found.",
            getPrompt: () => "",
            getResponses: () => [],
            getReview: () => ({ id: 1, job_id: job.id, output: "", closed: false }),
            isLoading: () => false,
            isReviewNotFound: () => false,
            isClosed: () => false,
            addComment: vi.fn(),
            closeReview: vi.fn(),
          },
        },
      ],
    ]),
  });

  return {
    unmount: async () => {
      unmount();
      wrapper.remove();
      await Effect.runPromise(runtime.disposeEffect);
    },
  };
}

// FitStages renders the active stage ahead of its hidden measurement probes,
// so the first group in document order is the one the user sees.
function actionButtons(): HTMLElement[] {
  const group = document.querySelector<HTMLElement>(".footer-actions")!;
  return [...group.querySelectorAll<HTMLElement>("button")];
}

function actionRects(): DOMRect[] {
  return actionButtons().map((el) => el.getBoundingClientRect());
}

function actionNames(): (string | null)[] {
  return actionButtons().map((el) => el.getAttribute("aria-label") ?? el.textContent!.trim());
}

function usageRect(): DOMRect {
  return document.querySelector<HTMLElement>(".token-usage")!.getBoundingClientRect();
}

function footerRect(): DOMRect {
  return document.querySelector<HTMLElement>(".kit-bottom-dock__footer")!.getBoundingClientRect();
}

describe("review drawer footer layout", () => {
  let mounted: { unmount: () => Promise<void> } | null = null;

  afterEach(async () => {
    await mounted?.unmount();
    mounted = null;
    cleanup();
  });

  function assertActionsOnOneRow(): DOMRect[] {
    const rects = actionRects();
    expect(rects.length).toBeGreaterThan(1);
    for (const rect of rects) {
      expect(rect.width).toBeGreaterThan(0);
      expect(Math.abs(rect.top - rects[0]!.top)).toBeLessThanOrEqual(0.5);
    }
    return rects;
  }

  // Every action has to be inside the footer's own box: the drawer sits in a
  // pane that clips its overflow, so anything past the edge is unreachable
  // rather than merely ugly.
  // The actions and the usage summary share the footer row until it runs out of
  // room. Neither may end up painted on top of the other, whichever of them
  // gives way.
  function assertActionsClearOfUsage(actions: DOMRect[], usage: DOMRect): void {
    for (const action of actions) {
      const overlaps =
        action.left < usage.right - 0.5 &&
        action.right > usage.left + 0.5 &&
        action.top < usage.bottom - 0.5 &&
        action.bottom > usage.top + 0.5;
      expect(overlaps).toBe(false);
    }
  }

  function assertActionsInsideFooter(actions: DOMRect[]): void {
    const footer = footerRect();
    // Browser layout may place a one-device-pixel antialiased edge on the
    // fractional boundary. Anything beyond that is genuine clipped overflow.
    expect(actions[0]!.left).toBeGreaterThanOrEqual(footer.left - 1);
    expect(Math.max(...actions.map((r) => r.right))).toBeLessThanOrEqual(footer.right + 1);
  }

  it("keeps the actions beside the usage summary when the footer has room", async () => {
    mounted = mountAt(1100);

    await vi.waitFor(() => expect(actionNames()).toEqual(["Close Review", "Rerun", "Copy Output"]));
    expect(actionButtons().every((el) => el.classList.contains("kit-button"))).toBe(true);

    const actions = assertActionsOnOneRow();
    assertActionsInsideFooter(actions);
    const usage = usageRect();
    const actionsBottom = Math.max(...actions.map((r) => r.bottom));

    expect(usage.top).toBeLessThan(actionsBottom);
    expect(usage.left).toBeGreaterThanOrEqual(Math.max(...actions.map((r) => r.right)));
  });

  // 260px is below the labelled group's intrinsic width, so this pins the
  // downgrade itself and the summary giving up the row.
  //
  // What it no longer pins is stacking. Before FitStages that was this test's
  // point, falsified against `flex-wrap: wrap` on .footer-actions. Now the
  // group is never handed less width than the stage it renders -- the fit
  // host's floor is the compact row's own width, and a richer stage is only
  // chosen when it already fits -- so wrapping has nothing to act on and that
  // falsification passes. The row assertion documents the property rather than
  // guarding a wrap rule; the guard that still bites is the floor, below.
  it("downgrades to icon-only actions and wraps the usage summary below when space is tight", async () => {
    mounted = mountAt(260);

    await vi.waitFor(() => expect(actionButtons().every((el) => el.classList.contains("kit-icon-button"))).toBe(true));

    const actions = assertActionsOnOneRow();
    assertActionsInsideFooter(actions);
    const usage = usageRect();
    const actionsBottom = Math.max(...actions.map((r) => r.bottom));

    expect(usage.top).toBeGreaterThanOrEqual(actionsBottom);
  });

  // The band between the two cases above is what the fit host's min-width floor
  // protects: without it the grow leaves the host narrower than even the icon
  // stage needs, and the icons paint over the usage text instead of the summary
  // moving out of the way.
  //
  // Assert non-overlap rather than "the summary wrapped to the next row".
  // Whether the summary answers the pressure by wrapping below or by shrinking
  // beside the actions depends on text metrics, and those differ between a
  // developer machine and the CI container -- pinning the row made this pass
  // locally and fail in CI at the same width. Non-overlap is the invariant in
  // either arrangement, and the band is swept because the wrap boundary itself
  // moves with those metrics.
  // 280 is the workspace right sidebar's own MIN_SIDEBAR_WIDTH, the narrowest
  // box the drawer is ever handed in the real app
  // (frontend/src/lib/components/terminal/WorkspaceTerminalView.svelte).
  for (const width of [280, 300, 340, 380, 420, 460]) {
    it(`keeps the actions clear of the usage summary at ${width}px`, async () => {
      mounted = mountAt(width);
      // Repeated first-paint geometry does not prove ResizeObserver has run.
      // Wait for the layout contract itself, regardless of the selected stage.
      await vi.waitFor(() => {
        const actions = assertActionsOnOneRow();
        assertActionsInsideFooter(actions);
        assertActionsClearOfUsage(actions, usageRect());
      });
    });
  }

  // The compact stage is a rendering of the same actions, not a reduced set:
  // the accessible names must not change with the drawer's width, or a
  // screen-reader user loses the action the sighted user still has.
  it("keeps every action reachable under the same name once it downgrades to icons", async () => {
    mounted = mountAt(260);

    await vi.waitFor(() => expect(actionButtons().every((el) => el.classList.contains("kit-icon-button"))).toBe(true));
    expect(actionNames()).toEqual(["Close Review", "Rerun", "Copy Output"]);
  });
});
