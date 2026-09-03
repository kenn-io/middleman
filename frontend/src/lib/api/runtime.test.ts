import { describe, expect, it } from "vite-plus/test";

import { getListActivityUrl } from "./generated/activity/activity.js";
import { getGetRepoBrowserLastChangedUrl } from "./generated/repositories/repositories.js";
import { readDocsBlob } from "./generated/docs/docs.js";
import { GeneratedProblemResponse, orvalFetch } from "./runtime.js";

describe("runtime", () => {
  it.each(["image/png", "application/octet-stream"])(
    "preserves binary bytes from generated Blob responses (%s)",
    async (contentType) => {
      const bytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x00, 0xff]);
      const result = await readDocsBlob(
        { id: "notes" },
        { path: "image.png" },
        {
          fetch: async () => new Response(bytes, { headers: { "Content-Type": contentType } }),
        },
      );

      expect(result).toBeInstanceOf(Blob);
      expect(new Uint8Array(await result.arrayBuffer())).toEqual(bytes);
      expect(result.type).toBe(contentType);
    },
  );

  it("keeps binary endpoint errors on the typed problem path", async () => {
    const problem = { status: 404, code: "notFound", detail: "Image not found" };
    const result = readDocsBlob(
      { id: "notes" },
      { path: "missing.png" },
      {
        fetch: async () => Response.json(problem, { status: 404 }),
      },
    );

    await expect(result).rejects.toBeInstanceOf(GeneratedProblemResponse);
    await expect(result).rejects.toMatchObject({ problem });
  });

  it("preserves text responses", async () => {
    const result = await orvalFetch<string>("/text", {
      fetch: async () => new Response("plain text", { headers: { "Content-Type": "text/plain" } }),
    });
    expect(result).toBe("plain text");
  });

  it("serializes array query parameters as comma-separated values for Huma", () => {
    const query = getListActivityUrl({ types: ["comment", "review"] });

    expect(query).toBe("/activity?types=comment%2Creview");
  });

  it("serializes repository browser paths as repeated keys", () => {
    const url = getGetRepoBrowserLastChangedUrl(
      { provider: "github", owner: "acme", name: "widgets" },
      { path: ["README.md", "docs/guide.md"] },
    );

    expect(url).toBe("/repo/github/acme/widgets/browser/last-changed?path=README.md&path=docs%2Fguide.md");
  });

  it("omits an optional query string when no parameters are supplied", () => {
    expect(getListActivityUrl()).toBe("/activity");
  });
});
