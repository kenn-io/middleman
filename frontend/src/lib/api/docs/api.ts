import type {
  CrossFolderHit,
  DocsSearchAllOutputBody,
  GitChangesResponse as GeneratedGitChangesResponse,
  GitStatusResponse as GeneratedGitStatusResponse,
  Node,
  ProblemError,
  PublishChange,
  PublishResponse,
} from "../generated/models/index.js";
import * as api from "../generated/index.js";
import { Effect, Schema } from "effect";
import { configuredAPIBaseURL } from "../runtime-base.js";
import { transientRetrySchedule } from "../retry-policy.js";
import type {
  AddFolderInput,
  BrowseResponse,
  CrossFolderSearchHit,
  CrossFolderSearchResponse,
  GitChangesResponse,
  GitFileStatus,
  GitPublishChange,
  GitPublishChangeStatus,
  GitPublishResponse,
  GitPullResponse,
  GitStatusResponse,
  SearchResponse,
  TreeNode,
  Folder,
} from "./types";

import { apiErrorMessage, GeneratedProblemResponse, type OrvalRequestOptions } from "../runtime.js";

/**
 * Typed wrapper around the kenn-forge Go server's /api/docs/* endpoints.
 *
 * Image blob URLs aren't fetched through this API — markdown <img src=...>
 * tags request them directly. `blobURL` builds the right URL.
 */
export interface DocsAPI {
  listFolders(signal?: AbortSignal): Promise<Folder[]>;
  // Register a new folder. Server canonicalizes the path (tilde expansion,
  // symlink resolution) and defaults name/id when omitted. Throws
  // DocsAPIError with status 409 / code "duplicate_folder_id" on collision
  // and 404 / "settingsUnavailable" when the server was started without a
  // writable config path.
  addFolder(input: AddFolderInput, signal?: AbortSignal): Promise<Folder>;
  removeFolder(id: string, signal?: AbortSignal): Promise<void>;
  renameFolder(id: string, name: string, signal?: AbortSignal): Promise<Folder>;
  // List subdirectories at path (defaults to the user's home dir on the
  // server). Used by the add-folder folder picker.
  browseDirectories(path?: string, signal?: AbortSignal): Promise<BrowseResponse>;
  tree(folderID: string, signal?: AbortSignal): Promise<TreeNode>;
  readFile(folderID: string, relPath: string, signal?: AbortSignal): Promise<string>;
  writeFile(folderID: string, relPath: string, content: string, signal?: AbortSignal): Promise<void>;
  // Create a new file. Throws DocsAPIError with status 409 / code
  // "already_exists" if the destination is in use.
  createFile(folderID: string, relPath: string, content?: string, signal?: AbortSignal): Promise<void>;
  deleteFile(folderID: string, relPath: string, signal?: AbortSignal): Promise<void>;
  renameFile(folderID: string, fromPath: string, toPath: string, signal?: AbortSignal): Promise<void>;
  search(folderID: string, query: string, limit?: number, signal?: AbortSignal): Promise<SearchResponse>;
  searchAll(query: string, limit?: number, signal?: AbortSignal): Promise<CrossFolderSearchResponse>;
  gitStatus(folderID: string, signal?: AbortSignal): Promise<GitStatusResponse>;
  gitChanges(folderID: string, signal?: AbortSignal): Promise<GitChangesResponse>;
  gitPublish(folderID: string, message: string, signal?: AbortSignal): Promise<GitPublishResponse>;
  // Fast-forward the folder's branch to its upstream. Throws DocsAPIError
  // with code "diverged" when local and remote history have both moved.
  gitPull(folderID: string, signal?: AbortSignal): Promise<GitPullResponse>;
  blobURL(folderID: string, relPath: string): string;
}

export interface DocsAPIClientOptions {
  baseURL?: string;
  fetch?: typeof fetch;
}

export class DocsRequestError extends Schema.TaggedError<DocsRequestError>()("DocsRequestError", {
  operation: Schema.String,
  message: Schema.String,
  status: Schema.Number,
  code: Schema.optional(Schema.String),
  commit: Schema.optional(Schema.String),
  cause: Schema.Defect(),
}) {}

function docsRequestError(operation: string, cause: unknown): DocsRequestError {
  const message = cause instanceof Error ? cause.message : "Docs request failed";
  const status = cause instanceof Error && "status" in cause && typeof cause.status === "number" ? cause.status : 0;
  const code = cause instanceof Error && "code" in cause && typeof cause.code === "string" ? cause.code : undefined;
  const commit =
    cause instanceof Error && "commit" in cause && typeof cause.commit === "string" ? cause.commit : undefined;
  return DocsRequestError.make({
    operation,
    message,
    status,
    ...(code === undefined ? {} : { code }),
    ...(commit === undefined ? {} : { commit }),
    cause,
  });
}

export const executeDocsRequest = Effect.fn("DocsApi.execute")(function* <A>(
  operation: string,
  request: (signal: AbortSignal) => Promise<A>,
) {
  return yield* Effect.tryPromise({
    try: request,
    catch: (cause) => docsRequestError(operation, cause),
  });
});

export const retryIdempotentDocsRequest = <A, R>(effect: Effect.Effect<A, DocsRequestError, R>) =>
  effect.pipe(
    Effect.retry({
      schedule: transientRetrySchedule,
      while: (failure) => failure.status === 0,
    }),
  );

export function createDocsAPI(options: DocsAPIClientOptions = {}): DocsAPI {
  const generatedOptions = (signal?: AbortSignal): OrvalRequestOptions => ({
    ...(options.fetch === undefined ? {} : { fetch: options.fetch }),
    ...(options.baseURL === undefined ? {} : { baseURL: options.baseURL }),
    ...(signal === undefined ? {} : { signal }),
  });
  const request = async <A>(operation: () => Promise<A>): Promise<A> => {
    try {
      return await operation();
    } catch (error) {
      if (error instanceof GeneratedProblemResponse) {
        throwOnDocsError(error.problem, error.response);
      }
      throw error;
    }
  };

  // Build a blob URL by hand: it isn't fetched through the typed client —
  // markdown <img src=...> tags request it directly. Same shape as the old
  // url() helper: an absolute URL when baseURL is http(s), else path-only.
  function blobURLFor(folderID: string, relPath: string): string {
    const u = resourceURLFor(options.baseURL, `docs/folders/${encodeURIComponent(folderID)}/blob`);
    u.searchParams.set("path", relPath);
    return isSameRuntimeOrigin(u) ? u.pathname + u.search : u.toString();
  }

  return {
    async listFolders(signal) {
      const data = await request(() => api.DocsService.listDocsFolders(generatedOptions(signal)));
      return data.folders ?? [];
    },
    async addFolder(input, signal) {
      const data = await request(() => api.DocsService.createDocsFolder(input, generatedOptions(signal)));
      return data.folder;
    },
    async removeFolder(id, signal) {
      await request(() => api.DocsService.deleteDocsFolder({ id }, generatedOptions(signal)));
    },
    async renameFolder(id, name, signal) {
      const data = await request(() => api.DocsService.updateDocsFolder({ id }, { name }, generatedOptions(signal)));
      return data.folder;
    },
    async browseDirectories(path, signal) {
      const query: { path?: string } = {};
      if (path !== undefined) query.path = path;
      const payload = await request(() => api.DocsService.browseDocsFolders(query, generatedOptions(signal)));
      return { ...payload, parent: payload.parent ?? "", entries: payload.entries ?? [] };
    },
    async tree(folderID, signal) {
      const data = await request(() => api.DocsService.getDocsTree({ id: folderID }, generatedOptions(signal)));
      return normalizeTree(data);
    },
    async readFile(folderID, relPath, signal) {
      const data = await request(() =>
        api.DocsService.readDocsFile({ id: folderID }, { path: relPath }, generatedOptions(signal)),
      );
      return data.content;
    },
    async writeFile(folderID, relPath, content, signal) {
      await request(() =>
        api.DocsService.writeDocsFile({ id: folderID }, { content }, { path: relPath }, generatedOptions(signal)),
      );
    },
    async createFile(folderID, relPath, content = "", signal) {
      await request(() =>
        api.DocsService.createDocsFile({ id: folderID }, { content }, { path: relPath }, generatedOptions(signal)),
      );
    },
    async deleteFile(folderID, relPath, signal) {
      await request(() =>
        api.DocsService.deleteDocsFile({ id: folderID }, { path: relPath }, generatedOptions(signal)),
      );
    },
    async renameFile(folderID, fromPath, toPath, signal) {
      await request(() =>
        api.DocsService.renameDocsFile({ id: folderID }, { from: fromPath, to: toPath }, generatedOptions(signal)),
      );
    },
    async search(folderID, query, limit, signal) {
      const searchQuery: { q?: string; limit?: number } = { q: query };
      if (limit !== undefined) searchQuery.limit = limit;
      const payload = await request(() =>
        api.DocsService.searchDocsFolder({ id: folderID }, searchQuery, generatedOptions(signal)),
      );
      return { ...payload, hits: payload.hits ?? [] };
    },
    async searchAll(query, limit, signal) {
      const searchQuery: { q?: string; limit?: number } = { q: query };
      if (limit !== undefined) searchQuery.limit = limit;
      const data = await request(() => api.DocsService.searchDocs(searchQuery, generatedOptions(signal)));
      return normalizeCrossFolderSearch(data);
    },
    async gitStatus(folderID, signal) {
      const data = await request(() => api.DocsService.getDocsGitStatus({ id: folderID }, generatedOptions(signal)));
      return normalizeGitStatus(data);
    },
    async gitChanges(folderID, signal) {
      const data = await request(() => api.DocsService.getDocsGitChanges({ id: folderID }, generatedOptions(signal)));
      return normalizeGitChanges(data);
    },
    async gitPublish(folderID, message, signal) {
      const data = await request(() =>
        api.DocsService.publishDocsGit({ id: folderID }, { message }, generatedOptions(signal)),
      );
      return normalizeGitPublish(data);
    },
    async gitPull(folderID, signal) {
      return request(() => api.DocsService.pullDocsGit({ id: folderID }, generatedOptions(signal)));
    },
    blobURL(folderID, relPath) {
      return blobURLFor(folderID, relPath);
    },
  };
}

function normalizeTree(node: Node): TreeNode {
  const { children, ...rest } = node;
  const normalizedChildren = children?.map(normalizeTree);
  return {
    ...rest,
    ...(normalizedChildren === undefined ? {} : { children: normalizedChildren }),
  };
}

function normalizeGitStatus(payload: GeneratedGitStatusResponse): GitStatusResponse {
  const entries = (payload.entries ?? []).map((entry) => ({
    ...entry,
    status: normalizeGitFileStatus(entry.status),
  }));
  return { ...payload, entries };
}

function normalizeGitFileStatus(status: string): GitFileStatus {
  switch (status) {
    case "added":
    case "deleted":
    case "ignored":
    case "modified":
    case "renamed":
    case "untracked":
      return status;
    default:
      throw new Error(`Unsupported Docs git status: ${status}`);
  }
}

function normalizeGitChanges(payload: GeneratedGitChangesResponse): GitChangesResponse {
  return { ...payload, changes: (payload.changes ?? []).map(normalizeGitPublishChange) };
}

function normalizeGitPublish(payload: PublishResponse): GitPublishResponse {
  return { ...payload, files: (payload.files ?? []).map(normalizeGitPublishChange) };
}

function normalizeGitPublishChange(change: PublishChange): GitPublishChange {
  return { ...change, status: normalizeGitPublishStatus(change.status) };
}

function normalizeGitPublishStatus(status: string): GitPublishChangeStatus {
  switch (status) {
    case "added":
    case "deleted":
    case "modified":
    case "renamed":
    case "untracked":
      return status;
    default:
      throw new Error(`Unsupported Docs publish status: ${status}`);
  }
}

function normalizeCrossFolderSearch(payload: DocsSearchAllOutputBody): CrossFolderSearchResponse {
  const { hits, warnings, ...rest } = payload;
  return {
    ...rest,
    hits: (hits ?? []).map(normalizeCrossFolderSearchHit),
    ...(warnings === undefined || warnings === null ? {} : { warnings }),
  };
}

function normalizeCrossFolderSearchHit(hit: CrossFolderHit): CrossFolderSearchHit {
  const { hit_type, snippet, ...rest } = hit;
  return {
    ...rest,
    hit_type: normalizeCrossFolderHitType(hit_type),
    ...(snippet === undefined ? {} : { snippet: { ...snippet, matches: snippet.matches ?? [] } }),
  };
}

function normalizeCrossFolderHitType(hitType: string): CrossFolderSearchHit["hit_type"] {
  switch (hitType) {
    case "filename":
    case "body":
      return hitType;
    default:
      throw new Error(`Unsupported Docs search hit type: ${hitType}`);
  }
}

function resourceURLFor(baseURL: string | undefined, path: string): URL {
  const base = new URL(baseURL ?? defaultAPIBaseURL(), runtimeOrigin()).toString().replace(/\/+$/, "");
  return new URL(`${base}/${path.replace(/^\/+/, "")}`);
}

function defaultAPIBaseURL(): string {
  return configuredAPIBaseURL();
}

function runtimeOrigin(): string {
  return typeof window !== "undefined" ? window.location.origin : "http://localhost";
}

function isSameRuntimeOrigin(url: URL): boolean {
  return url.origin === runtimeOrigin();
}

function throwOnDocsError(
  error: Pick<Partial<ProblemError>, "code" | "detail" | "details" | "title"> | undefined,
  response: Response,
): void {
  if (response.ok) return;
  const code = docsErrorCodeFromEnvelope(error);
  const commit = error?.details?.["commit"];
  throw new DocsTransportError(
    apiErrorMessage(error, `${response.status}`),
    response.status,
    code,
    typeof commit === "string" ? commit : undefined,
  );
}

class DocsTransportError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly commit?: string;

  constructor(message: string, status: number, code: string | undefined, commit: string | undefined) {
    super(message);
    this.name = "DocsAPIError";
    this.status = status;
    if (code !== undefined) this.code = code;
    if (commit !== undefined) this.commit = commit;
  }
}

function docsErrorCodeFromEnvelope(
  error: Pick<Partial<ProblemError>, "code" | "details"> | undefined,
): string | undefined {
  const reason = error?.details?.["reason"];
  if (typeof reason === "string") {
    switch (reason) {
      case "indexNotClean":
        return "index_not_clean";
      case "noUpstream":
        return "no_upstream";
      case "pushFailedAfterCommit":
        return "push_failed_after_commit";
      case "unsafeGitConfig":
        return "unsafe_git_config";
      case "diverged":
        return "diverged";
      case "pullFailed":
        return "pull_failed";
      case "gitOperationInProgress":
        return "git_operation_in_progress";
      case "notGitRepo":
        return "not_a_git_repo";
      case "conflict":
        return "conflict";
      case "alreadyExists":
        return "already_exists";
      case "unsupportedExtension":
        return "unsupported_extension";
      case "outsideFolder":
        return "outside_folder";
      case "duplicateFolderID":
        return "duplicate_folder_id";
    }
  }
  return typeof error?.code === "string" ? error.code : undefined;
}
