import { canonicalProvider } from "../api/provider-routes.js";
import { buildIssueRoute, buildRoutedItemRoute, type RepositoryRouteRef } from "../routes.js";

export type ItemReferenceType = "pr" | "issue";

export type ResolvableItemReference = RepositoryRouteRef & {
  number: number;
  itemType?: ItemReferenceType | undefined;
  externalUrl?: string | undefined;
};

export type ItemReferenceDataAttributes = {
  "data-provider": string;
  "data-owner": string;
  "data-name": string;
  "data-repo-path": string;
  "data-number": string;
  "data-platform-host"?: string | undefined;
  "data-item-type"?: ItemReferenceType | undefined;
  "data-external-url"?: string | undefined;
};

export type ItemReferenceLink = {
  href: string;
  dataAttributes: ItemReferenceDataAttributes;
};

const defaultHosts: Record<string, string> = {
  github: "github.com",
  gitlab: "gitlab.com",
};

function providerHost(provider: string, platformHost: string | undefined): string | null {
  return platformHost?.trim() || defaultHosts[canonicalProvider(provider)] || null;
}

function encodeRepoPath(repoPath: string): string {
  return repoPath
    .split("/")
    .filter(Boolean)
    .map((part) => encodeURIComponent(part))
    .join("/");
}

export function buildCanonicalProviderItemURL(ref: ResolvableItemReference): string | undefined {
  const host = providerHost(ref.provider, ref.platformHost);
  const repoPath = encodeRepoPath(ref.repoPath);
  if (!host || !repoPath) return undefined;
  const provider = canonicalProvider(ref.provider);
  const number = encodeURIComponent(ref.number.toString());
  let itemPath: string;
  if (ref.itemType === "pr") {
    if (provider === "gitlab") {
      itemPath = `/-/merge_requests/${number}`;
    } else if (provider === "github") {
      itemPath = `/pull/${number}`;
    } else {
      itemPath = `/pulls/${number}`;
    }
  } else {
    itemPath = provider === "gitlab" ? `/-/issues/${number}` : `/issues/${number}`;
  }
  return `https://${host}/${repoPath}${itemPath}`;
}

// Recognizes a pasted provider URL that points at a pull request or issue on
// the same host as the repository being rendered, so it can become an
// in-app item reference instead of an external link. Returns null for any
// other URL, including other hosts and non-item pages.
export function parseProviderItemURL(
  raw: string,
  repo: { provider: string; platformHost?: string | undefined },
): ResolvableItemReference | null {
  const host = providerHost(repo.provider, repo.platformHost);
  if (!host) return null;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }
  if (url.protocol !== "https:" || url.host.toLowerCase() !== host.toLowerCase()) return null;
  const provider = canonicalProvider(repo.provider);
  const segments = url.pathname
    .split("/")
    .filter(Boolean)
    .map((part) => {
      try {
        return decodeURIComponent(part);
      } catch {
        return part;
      }
    });
  let itemType: ItemReferenceType;
  let repoSegments: string[];
  let numberText: string;
  if (provider === "gitlab") {
    const marker = segments.lastIndexOf("-");
    if (marker < 1 || segments.length !== marker + 3) return null;
    const kind = segments[marker + 1];
    if (kind === "merge_requests") itemType = "pr";
    else if (kind === "issues") itemType = "issue";
    else return null;
    repoSegments = segments.slice(0, marker);
    numberText = segments[marker + 2]!;
  } else {
    if (segments.length !== 4) return null;
    const kind = segments[2];
    if (kind === "issues") itemType = "issue";
    else if (kind === (provider === "github" ? "pull" : "pulls")) itemType = "pr";
    else return null;
    repoSegments = segments.slice(0, 2);
    numberText = segments[3]!;
  }
  if (!/^\d+$/.test(numberText) || repoSegments.length < 2) return null;
  const name = repoSegments[repoSegments.length - 1]!;
  const owner = repoSegments.slice(0, -1).join("/");
  return {
    provider: repo.provider,
    platformHost: repo.platformHost,
    owner,
    name,
    repoPath: repoSegments.join("/"),
    number: parseInt(numberText, 10),
    itemType,
    externalUrl: url.toString(),
  };
}

// Recognizes a provider URL against the hosts of the configured repositories,
// so surfaces without a repository context (such as a terminal) can turn a
// printed pull request or issue link into an in-app item reference. Each
// distinct provider/host pair is tried once; whether the matched repository is
// actually tracked is decided by the resolve endpoint, not here.
export function parseConfiguredProviderItemURL(
  raw: string,
  repos: ReadonlyArray<{ provider: string; platform_host?: string | undefined }>,
): ResolvableItemReference | null {
  const seen = new Set<string>();
  for (const repo of repos) {
    const provider = canonicalProvider(repo.provider);
    const platformHost = repo.platform_host?.trim() || undefined;
    const key = `${provider}\u0000${(platformHost ?? "").toLowerCase()}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const ref = parseProviderItemURL(raw, { provider, platformHost });
    if (ref) return ref;
  }
  return null;
}

export function buildItemReferenceHref(ref: ResolvableItemReference): string {
  if (ref.itemType === "pr") {
    return buildRoutedItemRoute({ ...ref, itemType: "pr" });
  }
  if (ref.itemType === "issue") {
    return buildRoutedItemRoute({ ...ref, itemType: "issue" });
  }
  return buildIssueRoute(ref);
}

export function itemReferenceDataAttributes(ref: ResolvableItemReference): ItemReferenceDataAttributes {
  const externalUrl = ref.externalUrl ?? buildCanonicalProviderItemURL(ref);
  return {
    "data-provider": ref.provider,
    ...(ref.platformHost && {
      "data-platform-host": ref.platformHost,
    }),
    "data-owner": ref.owner,
    "data-name": ref.name,
    "data-repo-path": ref.repoPath,
    "data-number": ref.number.toString(),
    ...(ref.itemType && {
      "data-item-type": ref.itemType,
    }),
    ...(externalUrl && {
      "data-external-url": externalUrl,
    }),
  };
}

export function buildItemReferenceLink(ref: ResolvableItemReference): ItemReferenceLink {
  return {
    href: buildItemReferenceHref(ref),
    dataAttributes: itemReferenceDataAttributes(ref),
  };
}

function escapeAttribute(value: string): string {
  return value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

export function itemReferenceAnchorAttributes(ref: ResolvableItemReference, className = "item-ref"): string {
  const link = buildItemReferenceLink(ref);
  const attrs: Array<[string, string | undefined]> = [
    ["class", className],
    ["href", link.href],
    ["data-provider", link.dataAttributes["data-provider"]],
    ["data-platform-host", link.dataAttributes["data-platform-host"]],
    ["data-owner", link.dataAttributes["data-owner"]],
    ["data-name", link.dataAttributes["data-name"]],
    ["data-repo-path", link.dataAttributes["data-repo-path"]],
    ["data-number", link.dataAttributes["data-number"]],
    ["data-item-type", link.dataAttributes["data-item-type"]],
    ["data-external-url", link.dataAttributes["data-external-url"]],
  ];
  return attrs
    .filter(([, value]) => value !== undefined)
    .map(([name, value]) => `${name}="${escapeAttribute(value!)}"`)
    .join(" ");
}
