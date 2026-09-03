import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { Effect } from "effect";

import { STORES_KEY } from "../../context.js";
import type { OwnedAppRuntime } from "../../app/runtime.js";
import type { GeneratedClient } from "../../api/generated-api.js";
import type { ProviderRouteRef } from "../../api/provider-routes.js";
import { createDiffReviewDraftStore } from "../../stores/diff-review-draft.svelte.js";
import { makeTestAppRuntime } from "../../testing/effect-layers.js";
import DiffReviewDraftInlineComment from "./DiffReviewDraftInlineCommentRuntimeHarness.svelte";
import DiffReviewDraftTray from "./DiffReviewDraftTrayRuntimeHarness.svelte";

const runtimes = new Set<OwnedAppRuntime>();

function providerRef(): ProviderRouteRef {
  return {
    provider: "github",
    platformHost: "github.com",
    owner: "acme",
    name: "widgets",
    repoPath: "acme/widgets",
  };
}

function draftComment() {
  return {
    id: "1",
    body: "Draft note",
    path: "src/foo.ts",
    side: "right",
    line: 12,
    new_line: 12,
    line_type: "add",
    diff_head_sha: "head-sha",
    created_at: "2026-03-30T14:01:00Z",
    updated_at: "2026-03-30T14:01:00Z",
  };
}

async function renderTray(publishResult: boolean) {
  const publish = vi.fn(() => Promise.resolve(publishResult));
  const discard = vi.fn(() => Promise.resolve(true));
  const editComment = vi.fn(() => Promise.resolve(true));
  const comment = draftComment();
  const client = {
    GET: vi.fn(() =>
      Promise.resolve({
        data: {
          comments: [comment],
          supported_actions: ["comment"],
          native_multiline_ranges: true,
        },
        response: { ok: true, status: 200 },
      }),
    ),
    POST: vi.fn((_path: string, opts: { body?: { action?: string; body?: string } }) => {
      publish(opts.body?.action, opts.body?.body);
      if (publishResult) {
        return Promise.resolve({
          data: { status: "published" },
          response: { ok: true, status: 200 },
        });
      }
      return Promise.resolve({
        error: { title: "publish failed" },
        response: { ok: false, status: 500 },
      });
    }),
    PATCH: vi.fn((_path: string, opts: { body?: { body?: string } }) => {
      editComment(comment, opts.body?.body);
      return Promise.resolve({
        data: { ...comment, body: opts.body?.body ?? comment.body },
        response: { ok: true, status: 200 },
      });
    }),
    DELETE: vi.fn(() => {
      discard();
      return Promise.resolve({
        data: undefined,
        response: { ok: true, status: 200 },
      });
    }),
  } as unknown as GeneratedClient;
  const runtime = makeTestAppRuntime(client);
  runtimes.add(runtime);
  const diffReviewDraft = createDiffReviewDraftStore({ runtime });
  diffReviewDraft.setContext(providerRef(), 12, true, "head-sha");
  await waitFor(() => {
    expect(diffReviewDraft.getComments()).toHaveLength(1);
  });
  const context = new Map([[STORES_KEY, { diffReviewDraft }]]);
  const rendered = render(DiffReviewDraftTray, {
    props: { runtime },
    context,
  });
  return { ...rendered, context, diffReviewDraft, discard, editComment, publish };
}

describe("DiffReviewDraftTray", () => {
  afterEach(async () => {
    cleanup();
    vi.unstubAllGlobals();
    await Promise.all(Array.from(runtimes, (runtime) => Effect.runPromise(runtime.disposeEffect)));
    runtimes.clear();
  });

  it("keeps review summary text when publishing fails", async () => {
    const { publish } = await renderTray(false);
    const summary = screen.getByPlaceholderText("Review summary") as HTMLTextAreaElement;

    await fireEvent.input(summary, {
      target: { value: "Keep this summary" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Publish review" }));

    await waitFor(() => expect(publish).toHaveBeenCalledWith("comment", "Keep this summary"));
    expect(summary.value).toBe("Keep this summary");
  });

  it("lets a draft comment body be edited before publishing", async () => {
    const { editComment } = await renderTray(true);

    await fireEvent.click(screen.getByRole("button", { name: "Edit draft comment" }));
    const editor = screen.getByLabelText("Draft comment body") as HTMLTextAreaElement;
    expect(editor.value).toBe("Draft note");

    await fireEvent.input(editor, {
      target: { value: "Updated draft note" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save draft comment" }));

    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(
        expect.objectContaining({ id: "1", body: "Draft note" }),
        "Updated draft note",
      ),
    );
    await waitFor(() => {
      expect(screen.queryByLabelText("Draft comment body")).toBeNull();
    });
  });

  it("keeps publish and discard unavailable while a draft comment edit is open", async () => {
    const { discard, publish } = await renderTray(true);

    await fireEvent.click(screen.getByRole("button", { name: "Edit draft comment" }));
    const editor = screen.getByLabelText("Draft comment body") as HTMLTextAreaElement;
    await fireEvent.input(editor, {
      target: { value: "Visible unsaved edit" },
    });

    const publishButton = screen.getByRole("button", { name: "Publish review" }) as HTMLButtonElement;
    const discardButton = screen.getByRole("button", { name: "Discard review draft" }) as HTMLButtonElement;
    expect(publishButton.disabled).toBe(true);
    expect(discardButton.disabled).toBe(true);

    await fireEvent.click(publishButton);
    await fireEvent.click(discardButton);

    expect(publish).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByRole("button", { name: "Cancel editing draft comment" }));

    expect(publishButton.disabled).toBe(false);
    expect(discardButton.disabled).toBe(false);
  });

  it("keeps publish and discard unavailable while an inline draft comment edit is open", async () => {
    const { context, diffReviewDraft, discard, publish } = await renderTray(true);
    render(DiffReviewDraftInlineComment, {
      props: { comment: diffReviewDraft.getComments()[0] },
      context,
    });
    const inlineComment = document.querySelector("[data-draft-comment-id='1']");
    expect(inlineComment).not.toBeNull();

    await fireEvent.click(within(inlineComment as HTMLElement).getByRole("button", { name: "Edit draft comment" }));
    const editor = within(inlineComment as HTMLElement).getByLabelText("Draft comment body") as HTMLTextAreaElement;
    await fireEvent.input(editor, {
      target: { value: "Inline visible unsaved edit" },
    });

    const publishButton = screen.getByRole("button", { name: "Publish review" }) as HTMLButtonElement;
    const discardButton = screen.getByRole("button", { name: "Discard review draft" }) as HTMLButtonElement;
    expect(publishButton.disabled).toBe(true);
    expect(discardButton.disabled).toBe(true);

    await fireEvent.click(publishButton);
    await fireEvent.click(discardButton);

    expect(publish).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();
  });

  it("interrupts pending measurement and editor focus when the tray unmounts", async () => {
    const frameCallbacks: FrameRequestCallback[] = [];
    const cancelFrame = vi.fn();
    vi.stubGlobal(
      "requestAnimationFrame",
      vi.fn((callback: FrameRequestCallback) => {
        frameCallbacks.push(callback);
        return frameCallbacks.length;
      }),
    );
    vi.stubGlobal("cancelAnimationFrame", cancelFrame);
    const focus = vi.spyOn(HTMLTextAreaElement.prototype, "focus");
    const view = await renderTray(true);
    await waitFor(() => expect(frameCallbacks.length).toBeGreaterThan(0));

    screen.getByRole("button", { name: "Edit draft comment" }).click();
    view.unmount();
    for (const callback of frameCallbacks) callback(performance.now());
    await Promise.resolve();
    await Promise.resolve();

    expect(cancelFrame).toHaveBeenCalled();
    expect(focus).not.toHaveBeenCalled();
    focus.mockRestore();
  });
});
