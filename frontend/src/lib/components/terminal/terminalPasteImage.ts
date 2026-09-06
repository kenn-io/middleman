import { configuredAPIBaseURL } from "../../api/runtime-base.js";

export const MAX_TERMINAL_PASTE_IMAGE_BYTES = 20 * 1024 * 1024;

export const SUPPORTED_TERMINAL_PASTE_IMAGE_TYPES = new Set(["image/jpeg", "image/png", "image/webp"]);

export function terminalPastePathToken(path: string): string {
  // Uploads return an absolute path on the terminal host, regardless of the
  // browser's OS. Native Windows shells share double-quoted literal paths,
  // except for expansion characters that need different shell-specific escapes.
  if (/^(?:[A-Za-z]:[\\/]|\\\\)/u.test(path)) {
    if (/[%!$`"“”\r\n]/u.test(path)) {
      throw new Error("Windows image path contains characters that require shell-specific quoting.");
    }
    return `"${path}"`;
  }
  if (/^[A-Za-z0-9_./:@%+=,-]+$/u.test(path)) return path;
  return `'${path.replaceAll("'", `'"'"'`)}'`;
}

export async function uploadTerminalPasteImage(image: Blob, fleetHostKey?: string): Promise<string> {
  if (image.size > MAX_TERMINAL_PASTE_IMAGE_BYTES) {
    throw new Error("Terminal paste images must be 20 MiB or smaller.");
  }
  const target =
    fleetHostKey === undefined
      ? "/terminal/paste-image"
      : `/fleet/hosts/${encodeURIComponent(fleetHostKey)}/terminal/paste-image`;
  const response = await fetch(`${configuredAPIBaseURL()}${target}`, {
    method: "POST",
    headers: { "Content-Type": "application/octet-stream" },
    body: image,
  });
  if (!response.ok) {
    throw new Error(`Terminal image upload failed (${response.status}).`);
  }
  const payload: unknown = await response.json();
  if (
    typeof payload !== "object" ||
    payload === null ||
    !("path" in payload) ||
    typeof payload.path !== "string" ||
    payload.path === ""
  ) {
    throw new Error("Terminal image upload returned an invalid path.");
  }
  return payload.path;
}
