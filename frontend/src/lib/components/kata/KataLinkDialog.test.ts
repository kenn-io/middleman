import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";

import type { GeneratedClient } from "../../api/generated-api.js";
import { makeGeneratedClient } from "../../testing/generated-client.js";
import type { KataLinksSubject } from "../../stores/kata-links.svelte.js";
import KataLinkDialog from "./KataLinkDialog.svelte";

const subject: KataLinksSubject = { kind: "workspace", workspaceID: "workspace-1" };

function clientWith(
  listDaemons: ReturnType<typeof vi.fn>,
  listReferences = vi.fn(),
  createLink = vi.fn(),
): GeneratedClient {
  return makeGeneratedClient({
    KataService: {
      listKataDaemons: listDaemons,
      listKataReferences: listReferences,
      createWorkspaceKataLink: createLink,
    },
  });
}

function renderDialog(client: GeneratedClient, onlinked = vi.fn(), onclose = vi.fn()) {
  return {
    ...render(KataLinkDialog, {
      props: { subject, onlinked, onclose, apiClient: client },
    }),
    onlinked,
    onclose,
  };
}

describe("KataLinkDialog", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("uses the configured default initially and disables unhealthy daemons", async () => {
    const get = vi.fn().mockResolvedValue({
      daemons: [
        {
          id: "healthy",
          url: "http://healthy",
          health: "connected",
          auth: "none",
          default: true,
          api_schema_version: "0.13.0",
        },
        {
          id: "down",
          url: "http://down",
          health: "unreachable",
          auth: "none",
          default: false,
          api_schema_version: "0.13.0",
        },
      ],
    });
    renderDialog(clientWith(get));

    const trigger = await screen.findByRole("combobox", { name: /Kata daemon: healthy/ });
    await fireEvent.click(trigger);
    expect((screen.getByRole("option", { name: /down/ }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("searchbox", { name: "Search Kata issues" }) as HTMLInputElement).disabled).toBe(false);
    expect(screen.queryByLabelText(/daemon URL/i)).toBeNull();
    expect(screen.queryByLabelText(/issue UID/i)).toBeNull();
  });

  it("debounces reference search and submits the selected canonical identity", async () => {
    const get = vi.fn();
    const listDaemons = vi.fn(() => {
      get("listKataDaemons");
      return Promise.resolve({
        daemons: [
          {
            id: "healthy",
            url: "http://healthy",
            health: "connected",
            auth: "none",
            default: true,
            api_schema_version: "0.10.0",
          },
        ],
      });
    });
    const listReferences = vi.fn(() => {
      get("listKataReferences");
      return Promise.resolve({
        issues: [
          {
            uid: "issue-1",
            project_uid: "project-1",
            project_name: "Kata",
            qualified_id: "KATA-1",
            short_id: "KT-1",
            status: "open",
            title: "Keep one UI",
          },
        ],
      });
    });
    const post = vi.fn().mockResolvedValue({ state: "complete", diagnostics: [], links: [] });
    const rendered = renderDialog(clientWith(listDaemons, listReferences, post));
    await screen.findByRole("combobox", { name: /Kata daemon: healthy/ });
    vi.useFakeTimers();

    await fireEvent.input(screen.getByRole("searchbox", { name: "Search Kata issues" }), {
      target: { value: "keep" },
    });
    await vi.advanceTimersByTimeAsync(249);
    expect(get).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(get).toHaveBeenCalledTimes(2);

    await fireEvent.click(await screen.findByRole("button", { name: /KATA-1 Keep one UI/ }));
    await fireEvent.click(screen.getByRole("button", { name: "Link issue" }));

    await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    expect(post).toHaveBeenCalledWith(
      { id: "workspace-1" },
      { daemon_id: "healthy", issue_uid: "issue-1", project_uid: "project-1" },
    );
    expect(rendered.onlinked).toHaveBeenCalledTimes(1);
    expect(rendered.onclose).toHaveBeenCalledTimes(1);
  });
});
