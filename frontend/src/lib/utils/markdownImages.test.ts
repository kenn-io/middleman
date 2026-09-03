import { afterEach, beforeEach, describe, expect, test, vi } from "vite-plus/test";
import { Effect } from "effect";
import { makeTestAppRuntime } from "../testing/effect-layers.js";
import { makeGeneratedClient } from "../testing/generated-client.js";
import { getStackDepth, getTopFrame, resetModalStack } from "../stores/keyboard/modal-stack.svelte.js";
import { expandMarkdownImages, observeMarkdownImageExpansion } from "./markdownImages";

describe("expandMarkdownImages", () => {
  beforeEach(() => {
    resetModalStack();
  });

  afterEach(() => {
    resetModalStack();
    document.querySelectorAll(".markdown-image-lightbox").forEach((node) => node.remove());
  });

  test("adds a top-right control that opens the markdown image in an overlay", () => {
    const root = document.createElement("div");
    root.innerHTML = '<div class="markdown-body"><p><img src="/shots/dashboard.png" alt="Quality dashboard"></p></div>';

    const enhanced = expandMarkdownImages(root);
    const button = root.querySelector<HTMLButtonElement>('button[aria-label="Open image in expanded view"]');

    expect(enhanced).toBe(1);
    expect(button).not.toBeNull();
    expect(button?.closest(".markdown-image-expander")?.querySelector("img")?.getAttribute("src")).toBe(
      "/shots/dashboard.png",
    );

    button?.click();

    const overlay = document.querySelector<HTMLElement>(".markdown-image-lightbox");
    const expanded = overlay?.querySelector<HTMLImageElement>("img");
    expect(overlay?.getAttribute("role")).toBe("dialog");
    expect(overlay?.getAttribute("aria-modal")).toBe("true");
    expect(expanded?.getAttribute("src")).toBe("/shots/dashboard.png");
    expect(expanded?.getAttribute("alt")).toBe("Quality dashboard");
    expect(document.activeElement).toBe(overlay);
  });

  test("enhances markdown images while the app runtime owns the observer", async () => {
    const runtime = makeTestAppRuntime(makeGeneratedClient());
    const root = document.createElement("div");
    const execution = runtime.runCommand(Effect.scoped(observeMarkdownImageExpansion(root)), {
      operation: "test markdown image observer",
      safeContext: {},
      onFailure: () => {},
    });

    try {
      root.innerHTML = '<div class="markdown-body"><img src="/shots/runtime.png" alt="Runtime image"></div>';

      await vi.waitFor(() => {
        expect(root.querySelector('button[aria-label="Open image in expanded view"]')).not.toBeNull();
      });
    } finally {
      execution.interrupt();
      await execution.exit;
      await Effect.runPromise(runtime.disposeEffect);
    }
  });

  test("keeps zoom controls outside linked markdown images", () => {
    const root = document.createElement("div");
    root.innerHTML = [
      '<div class="markdown-body"><p>',
      '<a href="/shots/dashboard-full.png"><img src="/shots/dashboard.png" alt="Quality dashboard"></a>',
      "</p></div>",
    ].join("");

    const enhanced = expandMarkdownImages(root);
    const link = root.querySelector<HTMLAnchorElement>("a");
    const button = root.querySelector<HTMLButtonElement>('button[aria-label="Open image in expanded view"]');

    expect(enhanced).toBe(1);
    expect(link?.parentElement?.classList.contains("markdown-image-expander")).toBe(true);
    expect(link?.querySelector("img")).not.toBeNull();
    expect(button?.closest("a")).toBeNull();
  });

  test("skips images inside links that also contain text", () => {
    const root = document.createElement("div");
    root.innerHTML = [
      '<div class="markdown-body"><p>',
      '<a href="/shots/dashboard-full.png">Open <img src="/shots/dashboard.png" alt="Quality dashboard"> full size</a>',
      "</p></div>",
    ].join("");

    const enhanced = expandMarkdownImages(root);

    expect(enhanced).toBe(0);
    expect(root.querySelector(".markdown-image-expander")).toBeNull();
    expect(root.querySelector('button[aria-label="Open image in expanded view"]')).toBeNull();
  });

  test("blocks global shortcuts while the expanded image overlay is open", () => {
    const root = document.createElement("div");
    root.innerHTML = '<div class="markdown-body"><p><img src="/shots/dashboard.png" alt="Quality dashboard"></p></div>';
    const windowShortcut = vi.fn();
    window.addEventListener("keydown", windowShortcut);

    try {
      expandMarkdownImages(root);
      root.querySelector<HTMLButtonElement>('button[aria-label="Open image in expanded view"]')?.click();

      const overlay = document.querySelector<HTMLElement>(".markdown-image-lightbox");
      const closeButton = overlay?.querySelector<HTMLButtonElement>('button[aria-label="Close expanded image"]');
      expect(getTopFrame()?.frameId).toBe("markdown-image-lightbox");
      expect(getStackDepth()).toBe(1);

      closeButton?.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "j" }));
      expect(windowShortcut).not.toHaveBeenCalled();

      const escape = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "Escape" });
      closeButton?.dispatchEvent(escape);
      expect(escape.defaultPrevented).toBe(true);
      expect(document.querySelector(".markdown-image-lightbox")).toBeNull();
      expect(getStackDepth()).toBe(0);
    } finally {
      window.removeEventListener("keydown", windowShortcut);
    }
  });

  test("keeps keyboard focus inside the expanded image overlay", () => {
    const root = document.createElement("div");
    root.innerHTML = '<div class="markdown-body"><p><img src="/shots/dashboard.png" alt="Quality dashboard"></p></div>';

    expandMarkdownImages(root);
    root.querySelector<HTMLButtonElement>('button[aria-label="Open image in expanded view"]')?.click();

    const overlay = document.querySelector<HTMLElement>(".markdown-image-lightbox");
    const closeButton = overlay?.querySelector<HTMLButtonElement>('button[aria-label="Close expanded image"]');
    expect(document.activeElement).toBe(overlay);

    Object.defineProperty(closeButton, "offsetParent", {
      configurable: true,
      value: overlay,
    });

    const shiftTabFromOverlay = new KeyboardEvent("keydown", {
      bubbles: true,
      cancelable: true,
      key: "Tab",
      shiftKey: true,
    });
    overlay?.dispatchEvent(shiftTabFromOverlay);
    expect(shiftTabFromOverlay.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(closeButton);

    const tabFromClose = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "Tab" });
    closeButton?.dispatchEvent(tabFromClose);
    expect(tabFromClose.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(closeButton);
  });

  test("traps focus and scroll in the image owner's document", () => {
    const iframe = document.createElement("iframe");
    document.body.append(iframe);
    const doc = iframe.contentDocument!;
    doc.body.innerHTML = '<div class="markdown-body"><p><img src="/shots/frame.png" alt="Frame image"></p></div>';

    try {
      expandMarkdownImages(doc);
      const opener = doc.querySelector<HTMLButtonElement>('button[aria-label="Open image in expanded view"]')!;
      opener.focus();
      opener.click();

      const overlay = doc.querySelector<HTMLElement>(".markdown-image-lightbox");
      expect(overlay).not.toBeNull();
      expect(doc.activeElement).toBe(overlay);
      expect(doc.body.style.overflow).toBe("hidden");
      expect(document.body.style.overflow).toBe("");

      overlay?.querySelector<HTMLButtonElement>('button[aria-label="Close expanded image"]')?.click();
      expect(doc.body.style.overflow).toBe("");
      expect(doc.activeElement).toBe(opener);
    } finally {
      iframe.remove();
    }
  });
});
