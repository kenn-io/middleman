import { Effect } from "effect";
import { GeneratedApi } from "../api/generated-api.js";
import {
  canonicalProvider,
  providerRouteParams,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { GeneratedProblemResponse } from "../api/runtime.js";
import type { AppExecution, AppRuntime } from "../app/runtime.js";
import { navigate, buildItemRoute } from "../stores/router.svelte.js";
import { showFlash } from "../stores/flash.svelte.js";
import type { ResolvableItemReference } from "./item-reference.js";

function safeExternalURL(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const url = new URL(raw);
    if (url.protocol === "http:" || url.protocol === "https:") {
      return url.href;
    }
  } catch {
    return null;
  }
  return null;
}

function findItemRef(target: EventTarget | null): HTMLAnchorElement | null {
  let el = target instanceof HTMLElement ? target : null;
  while (el) {
    if (el instanceof HTMLAnchorElement && el.classList.contains("item-ref")) {
      return el;
    }
    el = el.parentElement;
  }
  return null;
}

function resolveAndNavigate(ref: ResolvableItemReference): Effect.Effect<void, unknown, GeneratedApi> {
  const { provider, platformHost, owner, name, repoPath, number, itemType, externalUrl } = ref;
  return Effect.gen(function* () {
    const api = yield* GeneratedApi;
    const result = yield* Effect.tryPromise({
      try: (signal) => {
        const routeRef = { provider, platformHost, owner, name, repoPath };
        const itemTypeHint = canonicalProvider(provider) === "gitlab" ? itemType : undefined;
        const query = itemTypeHint === undefined ? undefined : { item_type: itemTypeHint };
        const request = providerUsesHostRoute(routeRef)
          ? api.client.RepositoriesService.resolveRepoItemOnHost(
              { ...providerHostRouteParams(routeRef), number },
              query,
              {
                signal,
              },
            )
          : api.client.RepositoriesService.resolveRepoItem({ ...providerRouteParams(routeRef), number }, query, {
              signal,
            });
        return request.then(
          (data) => ({ data }) as const,
          (cause: unknown) => {
            if (cause instanceof GeneratedProblemResponse) return { problem: cause.problem } as const;
            throw cause;
          },
        );
      },
      catch: (cause) => cause,
    });
    yield* Effect.sync(() => {
      if ("problem" in result) {
        if (result.problem.status === 404) {
          showFlash(`Item ${owner}/${name}#${number} not found.`, { tone: "danger" });
        } else {
          showFlash(`Failed to resolve ${owner}/${name}#${number}. Try again later.`, { tone: "danger" });
        }
        return;
      }

      if (!result.data.repo_tracked) {
        const safeExternalUrl = safeExternalURL(externalUrl);
        if (safeExternalUrl) {
          window.open(safeExternalUrl, "_blank", "noopener,noreferrer");
          return;
        }
        showFlash(`${owner}/${name} is not tracked. Add it in Settings to navigate here.`, { tone: "danger" });
        return;
      }

      const path = buildItemRoute({
        itemType: result.data.item_type === "pr" ? "pr" : "issue",
        provider,
        platformHost,
        owner,
        name,
        repoPath,
        number,
      });
      navigate(path);
    });
  });
}

// Resolves an item reference through the repo resolve endpoint and either
// navigates to the internal item route (tracked repo) or opens the provider
// URL externally (untracked repo). Shared by rendered item-ref anchors and
// the terminal link handler.
export function resolveItemReference(runtime: AppRuntime, ref: ResolvableItemReference): AppExecution<void, unknown> {
  return runtime.runCommand(resolveAndNavigate(ref), {
    operation: "resolve item reference",
    safeContext: {
      provider: ref.provider,
      platformHost: ref.platformHost ?? "",
      owner: ref.owner,
      name: ref.name,
      number: ref.number.toString(),
    },
    onFailure: () => {
      showFlash("Failed to resolve item reference. Check your connection.", { tone: "danger" });
    },
  });
}

export function initItemRefHandler(runtime: AppRuntime): () => void {
  let execution: AppExecution<void, unknown> | null = null;

  function handleClick(e: MouseEvent): void {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;

    const anchor = findItemRef(e.target);
    if (!anchor) return;

    const provider = anchor.dataset.provider;
    const platformHost = anchor.dataset.platformHost;
    const owner = anchor.dataset.owner;
    const name = anchor.dataset.name;
    const repoPath = anchor.dataset.repoPath;
    const numberStr = anchor.dataset.number;
    const itemType =
      anchor.dataset.itemType === "pr" || anchor.dataset.itemType === "issue" ? anchor.dataset.itemType : undefined;
    const externalUrl = anchor.dataset.externalUrl;
    if (!provider || !owner || !name || !repoPath || !numberStr) return;

    e.preventDefault();
    execution?.interrupt();
    execution = resolveItemReference(runtime, {
      provider,
      platformHost,
      owner,
      name,
      repoPath,
      number: parseInt(numberStr, 10),
      itemType,
      externalUrl,
    });
  }

  document.addEventListener("click", handleClick);
  return () => {
    execution?.interrupt();
    document.removeEventListener("click", handleClick);
  };
}
