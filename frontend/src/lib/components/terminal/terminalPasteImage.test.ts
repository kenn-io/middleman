import { afterEach, describe, expect, it, vi } from "vite-plus/test";

import {
  MAX_TERMINAL_PASTE_IMAGE_BYTES,
  terminalPastePathToken,
  uploadTerminalPasteImage,
} from "./terminalPasteImage.js";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("terminal paste image upload", () => {
  it("keeps each uploaded path as one shell-style token", () => {
    expect(terminalPastePathToken("/var/lib/forge/paste-image.png")).toBe("/var/lib/forge/paste-image.png");
    expect(terminalPastePathToken("/data/Forge Images/paste'image.png")).toBe(
      `'/data/Forge Images/paste'"'"'image.png'`,
    );
  });

  it.each([
    String.raw`C:\Forge Images\paste'image.png`,
    "D:/Forge Images/paste-image.png",
    String.raw`\\files.example.test\Forge Images\paste-image.png`,
  ])("quotes native Windows path %s for cmd.exe and PowerShell", (path) => {
    expect(terminalPastePathToken(path)).toBe(`"${path}"`);
  });

  it.each(["%TEMP%", "$cache", "`cache", "!cache!", "“cache”"])(
    "rejects Windows paths requiring shell-specific expansion escaping: %s",
    (directory) => {
      expect(() => terminalPastePathToken(`C:\\${directory}\\paste-image.png`)).toThrow(
        "Windows image path contains characters that require shell-specific quoting",
      );
    },
  );

  it("posts raw image bytes to the local host endpoint", async () => {
    const fetchMock = vi.fn<typeof fetch>(
      async () =>
        new Response(JSON.stringify({ path: "/var/lib/forge/paste-image.png" }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const image = new Blob(["png bytes"], { type: "image/png" });

    await expect(uploadTerminalPasteImage(image)).resolves.toBe("/var/lib/forge/paste-image.png");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`${window.location.origin}/api/v1/terminal/paste-image`);
    expect(init).toEqual({
      method: "POST",
      headers: { "Content-Type": "application/octet-stream" },
      body: image,
    });
  });

  it("uses the explicit fleet host rather than parsing terminal websocket paths", async () => {
    const fetchMock = vi.fn<typeof fetch>(
      async () => new Response(JSON.stringify({ path: "/remote/paste-image.webp" }), { status: 201 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await uploadTerminalPasteImage(new Blob(["webp bytes"], { type: "image/webp" }), "Host A/Primary");

    expect(fetchMock.mock.calls[0]![0]).toBe(
      `${window.location.origin}/api/v1/fleet/hosts/Host%20A%2FPrimary/terminal/paste-image`,
    );
  });

  it("rejects oversized images before starting a request", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      uploadTerminalPasteImage(new Blob([new Uint8Array(MAX_TERMINAL_PASTE_IMAGE_BYTES + 1)])),
    ).rejects.toThrow("20 MiB");
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
