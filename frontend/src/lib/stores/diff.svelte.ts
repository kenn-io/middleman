import { Effect, Exit, Result } from "effect";
import type { CommitInfo, DiffFile, DiffHunk, DiffLine, DiffResult, FilePreview, FilesResult } from "../api/types.js";
import { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import { executeGeneratedApiRequest, GeneratedApi, type GeneratedClient } from "../api/generated-api.js";
import type { AppExecution, AppRuntime } from "../app/runtime.js";
import {
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import type {
  CommitsResponse as GeneratedCommitsResponse,
  DiffFile as GeneratedDiffFile,
  DiffResponse as GeneratedDiffResponse,
  FilePreviewResponse as GeneratedFilePreviewResponse,
  FilesResponse as GeneratedFilesResponse,
  Hunk,
  Line,
} from "../api/generated/models/index.js";
import { isProblem, ProblemCodes } from "../api/problems.js";
import { GeneratedProblemResponse } from "../api/runtime.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import {
  countDiffFilesByCategory,
  filterDiffFilesByCategory,
  type DiffFileCategoryCounts,
  type DiffFileCategoryFilter,
} from "../utils/diff-categories.js";
import { DiffWorkflow, type DiffReadError, type ProviderDiffRead } from "./diff-workflow.js";
import { FilePreviewUnavailable, FilePreviewWorkflow, type FilePreviewReadError } from "./diff-preview-workflow.js";
import { providerItemKey } from "./provider-key.js";

export type DiffScope =
  | { kind: "head" }
  | { kind: "commit"; sha: string }
  | { kind: "range"; fromSha: string; toSha: string };

export type WorkspaceDiffBase = "head" | "pushed" | "merge-target";
export type DiffViewMode = "unified" | "split";

export interface LoadWorkspaceDiffOptions {
  refreshCommits?: boolean;
  workspaceHostKey?: string | undefined;
  preserveVisible?: boolean;
  loadToken?: object | undefined;
}

interface LoadCommitsOptions {
  force?: boolean;
}

export interface DiffLoadCallbacks {
  readonly onSuccess?: () => void;
  readonly onFailure?: (message: string) => void;
  readonly onSettled?: () => void;
}

export interface FilePreviewCallbacks {
  readonly onSuccess?: (preview: FilePreview) => void;
  readonly onFailure?: (message: string) => void;
  readonly onSettled?: () => void;
}

export interface FileContextPreviews {
  readonly old: FilePreview | null;
  readonly new: FilePreview | null;
}

export interface FileContextPreviewCallbacks {
  readonly onSuccess?: (previews: FileContextPreviews) => void;
  readonly onFailure?: (message: string) => void;
  readonly onSettled?: () => void;
}

export type DiffScrollTarget = {
  path: string;
  line?: number | undefined;
  side?: "left" | "right" | undefined;
};

export interface DiffStoreOptions {
  runtime: AppRuntime;
}

interface ProviderDiffReadOptions {
  readonly invalidate: boolean;
  readonly loadFiles: boolean;
}

function apiErrorMessage(error: { detail?: string; title?: string } | undefined, fallback: string): string {
  return error?.detail ?? error?.title ?? fallback;
}

function diffReadErrorMessage(error: ApiProblemError | TransientTransportError): string {
  if (error._tag === "ApiProblemError") {
    return apiErrorMessage(error.problem, "failed to load diff");
  }
  return "Could not reach Kenn Forge";
}

function requestErrorMessage(error: ApiProblemError | TransientTransportError, fallback: string): string {
  if (error._tag === "ApiProblemError") {
    return apiErrorMessage(error.problem, fallback);
  }
  return "Could not reach Kenn Forge";
}

function filePreviewErrorMessage(error: FilePreviewReadError): string {
  if (error._tag === "FilePreviewUnavailable") return error.message;
  return requestErrorMessage(error, "failed to load file preview");
}

const executeGeneratedDefaultResponse = Effect.fn("GeneratedApi.executeDefaultResponse")(function* <A>(
  operation: string,
  request: (client: GeneratedClient, signal: AbortSignal) => Promise<unknown>,
  isSuccess: (value: unknown) => value is A,
) {
  const api = yield* GeneratedApi;
  const result = yield* Effect.tryPromise({
    try: (signal) => request(api.client, signal),
    catch: (cause) =>
      cause instanceof GeneratedProblemResponse
        ? new ApiProblemError({ operation, problem: cause.problem })
        : TransientTransportError.make({ operation, cause }),
  });
  if (isSuccess(result)) return result;
  return yield* Effect.fail(
    TransientTransportError.make({
      operation,
      cause: new Error("Generated request returned an invalid response body"),
    }),
  );
});

const invokeLoadCallback = (callback: (() => void) | undefined): Effect.Effect<void> =>
  callback === undefined ? Effect.void : Effect.sync(callback).pipe(Effect.catchCause(() => Effect.void));

const invokeFilePreviewSuccess = (
  callback: ((preview: FilePreview) => void) | undefined,
  preview: FilePreview,
): Effect.Effect<void> =>
  callback === undefined
    ? Effect.void
    : Effect.sync(() => callback(preview)).pipe(Effect.catchCause(() => Effect.void));

const invokeFileContextPreviewSuccess = (
  callback: ((previews: FileContextPreviews) => void) | undefined,
  previews: FileContextPreviews,
): Effect.Effect<void> =>
  callback === undefined
    ? Effect.void
    : Effect.sync(() => callback(previews)).pipe(Effect.catchCause(() => Effect.void));

function invokeLoadFailure(callback: ((message: string) => void) | undefined, message: string): void {
  try {
    callback?.(message);
  } catch {
    // Presentation callbacks do not own the request outcome.
  }
}

function isSnapshotChanged(error: unknown): boolean {
  return isProblem(error) && error.code === ProblemCodes.conflict && error.details?.["reason"] === "snapshot_changed";
}

type DiffResponse = GeneratedDiffResponse;
type FilesResponse = GeneratedFilesResponse;
type CommitsResponse = GeneratedCommitsResponse;
type FilePreviewResponse = GeneratedFilePreviewResponse;

function isFilePreviewResponse(value: unknown): value is FilePreviewResponse {
  return (
    typeof value === "object" &&
    value !== null &&
    !isProblem(value) &&
    "content" in value &&
    typeof value.content === "string" &&
    "encoding" in value &&
    typeof value.encoding === "string" &&
    "media_type" in value &&
    typeof value.media_type === "string" &&
    "path" in value &&
    typeof value.path === "string" &&
    "size" in value &&
    typeof value.size === "number"
  );
}

function normalizeDiffLine(line: Line): DiffLine {
  switch (line.type) {
    case "context":
    case "add":
    case "delete":
      return { ...line, type: line.type };
    default:
      throw new Error(`Unsupported diff line type: ${line.type}`);
  }
}

function normalizeDiffHunk(hunk: Hunk): DiffHunk {
  return {
    ...hunk,
    lines: (hunk.lines ?? []).map(normalizeDiffLine),
  };
}

function normalizeDiffFile(file: GeneratedDiffFile): DiffFile {
  switch (file.status) {
    case "added":
    case "modified":
    case "deleted":
    case "renamed":
    case "copied":
      return {
        ...file,
        status: file.status,
        hunks: (file.hunks ?? []).map(normalizeDiffHunk),
      };
    default:
      throw new Error(`Unsupported diff file status: ${file.status}`);
  }
}

function normalizeDiffResult(data: DiffResponse): DiffResult {
  return {
    ...data,
    files: (data.files ?? []).map(normalizeDiffFile),
  };
}

function normalizeFilesResult(data: FilesResponse): FilesResult {
  return {
    ...data,
    files: (data.files ?? []).map(normalizeDiffFile),
  };
}

function withVisibleFiles<T extends DiffResult | FilesResult>(result: T, files: T["files"]): T {
  return {
    ...result,
    files,
  };
}

function safeGetItem(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function safeSetItem(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* ignore */
  }
}

const VALID_TAB_WIDTHS = [1, 2, 4, 8];
const VALID_DIFF_VIEW_MODES: DiffViewMode[] = ["unified", "split"];
const workspaceDiffRetryInitialDelay = 1_000;
const workspaceDiffRetryMaxDelay = 30_000;

function loadTabWidth(): number {
  const raw = parseInt(safeGetItem("diff-tab-width") ?? "4", 10);
  return VALID_TAB_WIDTHS.includes(raw) ? raw : 4;
}

function loadDiffViewMode(): DiffViewMode {
  const raw = safeGetItem("diff-view-mode");
  return VALID_DIFF_VIEW_MODES.includes(raw as DiffViewMode) ? (raw as DiffViewMode) : "unified";
}

function loadCollapsedFiles(): Record<string, string[]> {
  try {
    const raw = safeGetItem("diff-collapsed-files");
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return {};
    const result: Record<string, string[]> = {};
    for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
      if (Array.isArray(value) && value.every((v) => typeof v === "string")) {
        result[key] = value as string[];
      }
    }
    return result;
  } catch {
    return {};
  }
}

function saveCollapsedFiles(cf: Record<string, string[]>): void {
  safeSetItem("diff-collapsed-files", JSON.stringify(cf));
}

export function createDiffStore(opts: DiffStoreOptions) {
  const runtime = opts.runtime;

  let diff = $state<DiffResult | null>(null);
  let loading = $state(false);
  let storeError = $state<string | null>(null);
  let activeCommitLoad: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;
  let activeWorkspaceLoad: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;

  let fileList = $state<FilesResult | null>(null);
  let fileListLoading = $state(false);

  let tabWidth = $state(loadTabWidth());
  let wordWrap = $state(safeGetItem("diff-word-wrap") === "true");
  let richPreview = $state(safeGetItem("diff-rich-preview") === "true");
  let hideWhitespace = $state(safeGetItem("diff-hide-whitespace") === "true");
  let viewMode = $state<DiffViewMode>(loadDiffViewMode());
  let collapsedFiles = $state<Record<string, string[]>>(loadCollapsedFiles());
  let activeFile = $state<string | null>(null);
  let activeFileRevealKey = $state(0);
  let scrollTarget = $state<DiffScrollTarget | null>(null);
  let scrolling = $state(false);
  let fileCategoryFilter = $state<DiffFileCategoryFilter>("all");
  let commits = $state<CommitInfo[] | null>(null);
  let commitsLoading = $state(false);
  let commitsError = $state<string | null>(null);
  let commitsGeneration = 0;
  let activeCommitsLoad: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;
  let workspaceLoadGeneration = 0;
  let currentWorkspaceLoadToken: object | undefined;
  let scope = $state<DiffScope>({ kind: "head" });
  let filePreviewGeneration = $state(0);

  let currentOwner = $state("");
  let currentName = $state("");
  let currentNumber = $state(0);
  let currentWorkspaceID = $state("");
  let currentWorkspaceHostKey = $state<string | undefined>(undefined);
  let currentWorkspaceBase = $state<WorkspaceDiffBase>("head");
  let currentWorkspaceStacked = $state(false);
  let currentCommitSHA = $state("");
  let currentProvider = $state("");
  let currentPlatformHost = $state<string | undefined>(undefined);
  let currentRepoPath = $state("");

  function getCurrentPR(): (ProviderRouteRef & { number: number }) | null {
    if (!currentOwner) return null;
    return {
      provider: currentProvider,
      platformHost: currentPlatformHost,
      owner: currentOwner,
      name: currentName,
      repoPath: currentRepoPath,
      number: currentNumber,
    };
  }

  function currentRouteRef(): ProviderRouteRef {
    return {
      provider: currentProvider,
      platformHost: currentPlatformHost,
      owner: currentOwner,
      name: currentName,
      repoPath: currentRepoPath,
    };
  }

  // --- reads ---

  function getDiff(): DiffResult | null {
    return diff;
  }
  function isDiffLoading(): boolean {
    return loading;
  }
  function getDiffError(): string | null {
    return storeError;
  }
  function getFileList(): FilesResult | null {
    if (currentWorkspaceID && fileList) {
      return { ...fileList, files: fileList.files ?? [] };
    }
    // Prefer diff.files once available — it respects hideWhitespace
    // and is authoritative. The lightweight /files response is a fast
    // preview used only until the full diff arrives.
    if (diff) {
      return {
        stale: diff.stale,
        whitespace_only_count: diff.whitespace_only_count,
        files: diff.files ?? [],
      };
    }
    if (fileList) return { ...fileList, files: fileList.files ?? [] };
    return null;
  }
  function getVisibleFileList(): FilesResult | null {
    const list = getFileList();
    if (!list) return null;
    return withVisibleFiles(list, filterDiffFilesByCategory(list.files, fileCategoryFilter));
  }
  function getVisibleDiffFiles(): DiffResult["files"] {
    if (!diff) return [];
    return filterDiffFilesByCategory(diff.files ?? [], fileCategoryFilter);
  }
  function getFileCategoryCounts(): DiffFileCategoryCounts {
    return countDiffFilesByCategory(getFileList()?.files ?? []);
  }
  function isFileListLoading(): boolean {
    // Show loading until we have *some* file data. When /files fails
    // but /diff is still in flight, keep showing loading state.
    return !diff && (fileListLoading || loading);
  }
  function getTabWidth(): number {
    return tabWidth;
  }
  function getWordWrap(): boolean {
    return wordWrap;
  }
  function getRichPreview(): boolean {
    return richPreview;
  }
  function getFilePreviewGeneration(): number {
    return filePreviewGeneration;
  }
  function getHideWhitespace(): boolean {
    return hideWhitespace;
  }
  function getViewMode(): DiffViewMode {
    return viewMode;
  }
  function getFileCategoryFilter(): DiffFileCategoryFilter {
    return fileCategoryFilter;
  }
  function getActiveFile(): string | null {
    return activeFile;
  }
  function getActiveFileRevealKey(): number {
    return activeFileRevealKey;
  }
  function isScrolling(): boolean {
    return scrolling;
  }

  function isFileCollapsed(owner: string, name: string, number: number, filePath: string): boolean {
    const key = collapseKeyFor(owner, name, number);
    return (collapsedFiles[key] ?? []).includes(filePath);
  }

  function visibleCollapsibleFilePaths(): string[] {
    const visibleDiffFiles = getVisibleDiffFiles();
    const visibleFiles = visibleDiffFiles.length > 0 ? visibleDiffFiles : (getVisibleFileList()?.files ?? []);
    return visibleFiles.map((file) => file.path);
  }

  function currentCollapseKey(): string | null {
    if (currentWorkspaceID) {
      return `workspace:${currentWorkspaceHostKey ?? "self"}:${currentWorkspaceID}:${currentWorkspaceBase}`;
    }
    if (!currentOwner || !currentName || !currentNumber) return null;
    return collapseKeyFor(currentOwner, currentName, currentNumber);
  }

  function collapseKeyFor(owner: string, name: string, number: number): string {
    if (currentWorkspaceID) {
      return `workspace:${currentWorkspaceHostKey ?? "self"}:${currentWorkspaceID}:${currentWorkspaceBase}`;
    }
    return `${owner}/${name}#${number}`;
  }

  function areAllVisibleFilesCollapsed(): boolean {
    const key = currentCollapseKey();
    if (!key) return false;
    const paths = visibleCollapsibleFilePaths();
    if (paths.length === 0) return false;
    const current = collapsedFiles[key] ?? [];
    return paths.every((path) => current.includes(path));
  }

  // --- writes ---

  function setActiveFile(path: string | null): void {
    activeFile = path;
  }

  function setFileCategoryFilter(nextFilter: DiffFileCategoryFilter): void {
    fileCategoryFilter = nextFilter;
    const visibleFiles = getVisibleFileList()?.files ?? getVisibleDiffFiles();
    setActiveIfNeeded(visibleFiles);
  }

  function clearScrolling(): void {
    scrolling = false;
  }

  function requestScrollToFile(path: string): void {
    activeFile = path;
    activeFileRevealKey += 1;
    scrolling = true;
    scrollTarget = { path };
  }

  function requestScrollToLine(path: string, line: number, side: "left" | "right" = "right"): void {
    activeFile = path;
    activeFileRevealKey += 1;
    scrolling = true;
    scrollTarget = { path, line, side };
  }

  function getScrollTarget(): DiffScrollTarget | null {
    return scrollTarget;
  }

  function consumeScrollTarget(): void {
    scrollTarget = null;
  }

  function setTabWidth(w: number): void {
    tabWidth = w;
    safeSetItem("diff-tab-width", String(w));
  }

  function setWordWrap(v: boolean): void {
    wordWrap = v;
    safeSetItem("diff-word-wrap", String(v));
  }

  function setRichPreview(v: boolean): void {
    richPreview = v;
    safeSetItem("diff-rich-preview", String(v));
  }

  function setHideWhitespace(v: boolean): void {
    hideWhitespace = v;
    safeSetItem("diff-hide-whitespace", String(v));
    if (currentOwner && currentName && currentNumber) {
      reloadDiffOnly();
    } else if (currentOwner && currentName && currentCommitSHA) {
      reloadCommitDiffOnly();
    } else if (currentWorkspaceID) {
      reloadWorkspaceDiffOnly();
    }
  }

  function setViewMode(mode: DiffViewMode): void {
    viewMode = mode;
    safeSetItem("diff-view-mode", mode);
  }

  function clearProviderDiffRead(): void {
    runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* DiffWorkflow;
        yield* workflow.clear;
      }),
      {
        operation: "clear provider diff read",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function providerDiffReadKey(ref: ProviderRouteRef, number: number, includeFiles: boolean): string {
    const itemKey = providerItemKey({
      provider: ref.provider,
      platformHost: resolvedPlatformHost(ref.provider, ref.platformHost),
      owner: ref.owner,
      name: ref.name,
      number,
    });
    return [
      itemKey,
      scopeCacheKey(),
      hideWhitespace ? "hide-whitespace" : "show-whitespace",
      includeFiles ? "with-files" : "diff-only",
    ].join("\u0000");
  }

  function providerDiffRequest(
    ref: ProviderRouteRef,
    number: number,
    generation: number,
    includeFiles: boolean,
  ): Effect.Effect<ProviderDiffRead, DiffReadError, GeneratedApi> {
    const filesRead = includeFiles
      ? executeGeneratedApiRequest("GET pull request files", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.PullRequestsService.getPullFilesOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { signal },
              )
            : client.PullRequestsService.getPullFiles({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(
          retryIdempotentRead,
          Effect.catch(() => Effect.succeed(null)),
          Effect.tap((data) =>
            Effect.sync(() => {
              if (generation !== workspaceLoadGeneration) return;
              if (data !== null) applyFilesResult(data);
              fileListLoading = false;
            }),
          ),
        )
      : Effect.succeed(null);
    const diffRead = executeGeneratedApiRequest("GET pull request diff", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.PullRequestsService.getPullDiffOnHost(
            { ...providerHostRouteParams(ref), number: number },
            diffQuery(),
            { signal },
          )
        : client.PullRequestsService.getPullDiff({ ...providerRouteParams(ref), number: number }, diffQuery(), {
            signal,
          }),
    ).pipe(
      retryIdempotentRead,
      Effect.tap((data) =>
        Effect.sync(() => {
          if (generation !== workspaceLoadGeneration) return;
          applyDiffResult(data);
          loading = false;
        }),
      ),
    );
    return Effect.all({ diff: diffRead, files: filesRead }, { concurrency: "unbounded" });
  }

  function startProviderDiffRead(
    ref: ProviderRouteRef,
    number: number,
    generation: number,
    options: ProviderDiffReadOptions,
  ): void {
    prepareProviderDiffRead(options.loadFiles);
    const key = providerDiffReadKey(ref, number, options.loadFiles);
    const request = providerDiffRequest(ref, number, generation, options.loadFiles);
    const program = Effect.gen(function* () {
      const workflow = yield* DiffWorkflow;
      if (options.invalidate) yield* workflow.invalidate(key);
      const result = yield* workflow.read(key, request);
      if (generation !== workspaceLoadGeneration) return;
      yield* Effect.sync(() => {
        if (result.files !== null) applyFilesResult(result.files);
        applyDiffResult(result.diff);
      });
    }).pipe(
      Effect.ensuring(
        Effect.sync(() => {
          if (generation !== workspaceLoadGeneration) return;
          loading = false;
          fileListLoading = false;
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "load pull request diff",
      safeContext: {
        provider: ref.provider,
        platformHost: resolvedPlatformHost(ref.provider, ref.platformHost),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: (failure) => {
        if (generation !== workspaceLoadGeneration) return;
        storeError = diffReadErrorMessage(failure);
        diff = null;
        fileList = null;
      },
    });
  }

  function prepareProviderDiffRead(includeFiles: boolean): void {
    fileList = null;
    diff = null;
    clearFilePreviewCache();
    loading = true;
    fileListLoading = includeFiles;
    storeError = null;
  }

  function reloadDiffOnly(): void {
    const generation = ++workspaceLoadGeneration;
    const ref = currentRouteRef();
    startProviderDiffRead(ref, currentNumber, generation, { invalidate: true, loadFiles: false });
  }

  function reloadWorkspaceDiffOnly(): void {
    loadWorkspaceDiff(currentWorkspaceID, currentWorkspaceBase, currentWorkspaceStacked, currentWorkspaceOptions());
  }

  function reloadCommitDiffOnly(): void {
    loadCommitDiff(currentRouteRef(), currentCommitSHA);
  }

  function toggleFileCollapsed(owner: string, name: string, number: number, filePath: string): void {
    const key = collapseKeyFor(owner, name, number);
    const current = collapsedFiles[key] ?? [];
    if (current.includes(filePath)) {
      collapsedFiles = {
        ...collapsedFiles,
        [key]: current.filter((f) => f !== filePath),
      };
    } else {
      collapsedFiles = {
        ...collapsedFiles,
        [key]: [...current, filePath],
      };
    }
    saveCollapsedFiles(collapsedFiles);
  }

  function setAllVisibleFilesCollapsed(nextCollapsed: boolean): void {
    const key = currentCollapseKey();
    if (!key) return;
    const paths = visibleCollapsibleFilePaths();
    if (paths.length === 0) return;

    let next = collapsedFiles[key] ?? [];
    if (nextCollapsed) {
      next = [...next, ...paths.filter((path) => !next.includes(path))];
    } else {
      next = next.filter((path) => !paths.includes(path));
    }

    collapsedFiles = {
      ...collapsedFiles,
      [key]: next,
    };
    saveCollapsedFiles(collapsedFiles);
  }

  function diffQuery(): {
    whitespace?: "hide";
    commit?: string;
    from?: string;
    to?: string;
  } {
    return {
      ...(hideWhitespace && { whitespace: "hide" as const }),
      ...(scope.kind === "commit" && { commit: scope.sha }),
      ...(scope.kind === "range" && {
        from: scope.fromSha,
        to: scope.toSha,
      }),
    };
  }

  function scopeCacheKey(): string {
    if (scope.kind === "head") return "head";
    if (scope.kind === "commit") return `commit:${scope.sha}`;
    return `range:${scope.fromSha}:${scope.toSha}`;
  }

  function clearFilePreviewCache(): void {
    filePreviewGeneration += 1;
    runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* FilePreviewWorkflow;
        yield* workflow.invalidateAll;
      }),
      {
        operation: "invalidate file previews",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function providerFilePreviewEffect(
    owner: string,
    name: string,
    number: number,
    path: string,
    side?: "old" | "new",
  ): Effect.Effect<FilePreview, FilePreviewReadError, FilePreviewWorkflow> {
    const ref = currentRouteRef();
    const requestScope = scope;
    const generation = filePreviewGeneration;
    const key = [
      providerItemKey({
        provider: ref.provider,
        platformHost: resolvedPlatformHost(ref.provider, ref.platformHost),
        owner,
        name,
        number,
      }),
      ref.repoPath,
      generation,
      scopeCacheKey(),
      path,
      side ?? "preview",
    ].join("\u0000");
    const request = executeGeneratedApiRequest<FilePreviewResponse>(
      "GET pull request file preview",
      (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.PullRequestsService.getPullFilePreviewOnHost(
              { ...providerHostRouteParams(ref), number: number },
              {
                path,
                ...(side && { side }),
                ...(requestScope.kind === "commit" && { commit: requestScope.sha }),
                ...(requestScope.kind === "range" && {
                  from: requestScope.fromSha,
                  to: requestScope.toSha,
                }),
              },
              { signal },
            )
          : client.PullRequestsService.getPullFilePreview(
              { ...providerRouteParams(ref), number: number },
              {
                path,
                ...(side && { side }),
                ...(requestScope.kind === "commit" && { commit: requestScope.sha }),
                ...(requestScope.kind === "range" && {
                  from: requestScope.fromSha,
                  to: requestScope.toSha,
                }),
              },
              { signal },
            ),
    ).pipe(retryIdempotentRead);
    return Effect.gen(function* () {
      const workflow = yield* FilePreviewWorkflow;
      return yield* workflow.read(key, request);
    });
  }

  function workspaceFilePreviewEffect(
    path: string,
    side?: "old" | "new",
  ): Effect.Effect<FilePreview, FilePreviewReadError, FilePreviewWorkflow> {
    const workspaceID = currentWorkspaceID;
    const workspaceHostKey = currentWorkspaceHostKey;
    const workspaceBase = currentWorkspaceBase;
    const workspaceStacked = currentWorkspaceStacked;
    const workspaceLoadToken = currentWorkspaceLoadToken;
    const workspaceGeneration = workspaceLoadGeneration;
    const revision = fileList?.snapshot_version ?? diff?.snapshot_version;
    const generation = filePreviewGeneration;
    const requestScope = scope;
    const key = [
      "workspace",
      workspaceHostKey ?? "self",
      workspaceID,
      workspaceBase,
      generation,
      scopeCacheKey(),
      revision ?? "latest",
      path,
      side ?? "preview",
    ].join("\u0000");
    const workspaceIsCurrent = (expectedGeneration: number): boolean =>
      currentWorkspaceID === workspaceID &&
      currentWorkspaceHostKey === workspaceHostKey &&
      currentWorkspaceBase === workspaceBase &&
      currentWorkspaceLoadToken === workspaceLoadToken &&
      workspaceLoadGeneration === expectedGeneration;
    const request = Effect.gen(function* () {
      let previewGeneration = workspaceGeneration;
      for (let attempt = 0; attempt < 2; attempt += 1) {
        const currentRevision = fileList?.snapshot_version ?? diff?.snapshot_version;
        const query: {
          base: WorkspaceDiffBase;
          whitespace?: "hide";
          commit?: string;
          from?: string;
          to?: string;
          path: string;
          side?: "old" | "new";
          revision?: string;
        } = {
          base: workspaceBase,
          ...(hideWhitespace && { whitespace: "hide" }),
          ...(requestScope.kind === "commit" && { commit: requestScope.sha }),
          ...(requestScope.kind === "range" && {
            from: requestScope.fromSha,
            to: requestScope.toSha,
          }),
          path,
          ...(side && { side }),
          ...(currentRevision && { revision: currentRevision }),
        };
        const previewRequest = workspaceHostKey
          ? executeGeneratedDefaultResponse<FilePreviewResponse>(
              "GET remote workspace file preview",
              (client, signal) =>
                client.FleetService.getFleetWorkspaceFilePreview(
                  { hostKey: workspaceHostKey, id: workspaceID },
                  query,
                  { signal },
                ),
              isFilePreviewResponse,
            )
          : executeGeneratedApiRequest<FilePreviewResponse>("GET workspace file preview", (client, signal) =>
              client.WorkspacesService.getWorkspaceFilePreview({ id: workspaceID }, query, { signal }),
            );
        const previewResult = yield* Effect.result(retryIdempotentRead(previewRequest));
        if (!workspaceIsCurrent(previewGeneration)) {
          return yield* Effect.fail(
            new FilePreviewUnavailable({ message: "Workspace changed while refreshing file preview" }),
          );
        }
        if (Result.isSuccess(previewResult)) return previewResult.success;
        if (
          previewResult.failure._tag !== "ApiProblemError" ||
          !isSnapshotChanged(previewResult.failure.problem) ||
          attempt > 0
        ) {
          return yield* Effect.fail(previewResult.failure);
        }
        const recovery = startWorkspaceDiff(workspaceID, workspaceBase, workspaceStacked, {
          workspaceHostKey,
          preserveVisible: true,
          loadToken: workspaceLoadToken,
        });
        const recoveryGeneration = workspaceLoadGeneration;
        const recoveryExit = yield* recovery.await;
        if (!workspaceIsCurrent(recoveryGeneration)) {
          return yield* Effect.fail(
            new FilePreviewUnavailable({ message: "Workspace changed while refreshing file preview" }),
          );
        }
        if (Exit.isFailure(recoveryExit)) {
          return yield* Effect.fail(
            new FilePreviewUnavailable({ message: storeError ?? "Workspace file preview could not be refreshed" }),
          );
        }
        if (!getVisibleFileList()?.files.some((file) => file.path === path)) {
          return yield* Effect.fail(
            new FilePreviewUnavailable({ message: "File is no longer present in the workspace diff" }),
          );
        }
        previewGeneration = recoveryGeneration;
      }
      return yield* Effect.fail(
        new FilePreviewUnavailable({ message: "Workspace file preview could not be refreshed" }),
      );
    });
    return Effect.gen(function* () {
      const workflow = yield* FilePreviewWorkflow;
      return yield* workflow.read(key, request);
    });
  }

  function filePreviewEffect(
    owner: string,
    name: string,
    number: number,
    path: string,
    side?: "old" | "new",
  ): Effect.Effect<FilePreview, FilePreviewReadError, FilePreviewWorkflow> {
    return currentWorkspaceID
      ? workspaceFilePreviewEffect(path, side)
      : providerFilePreviewEffect(owner, name, number, path, side);
  }

  function loadFilePreview(
    owner: string,
    name: string,
    number: number,
    path: string,
    side?: "old" | "new",
    callbacks: FilePreviewCallbacks = {},
  ): void {
    const program = filePreviewEffect(owner, name, number, path, side).pipe(
      Effect.tap((preview) => invokeFilePreviewSuccess(callbacks.onSuccess, preview)),
      Effect.ensuring(invokeLoadCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "load file preview",
      safeContext: { owner, name, number, path, ...(side !== undefined && { side }) },
      onFailure: (failure) => invokeLoadFailure(callbacks.onFailure, filePreviewErrorMessage(failure)),
    });
  }

  function loadFileContextPreviews(
    owner: string,
    name: string,
    number: number,
    source: DiffFile,
    callbacks: FileContextPreviewCallbacks = {},
  ): void {
    const oldPreview =
      source.status === "added"
        ? Effect.succeed<FilePreview | null>(null)
        : filePreviewEffect(owner, name, number, source.path, "old");
    const newPreview =
      source.status === "deleted"
        ? Effect.succeed<FilePreview | null>(null)
        : filePreviewEffect(owner, name, number, source.path, "new");
    const program = Effect.all({ old: oldPreview, new: newPreview }, { concurrency: "unbounded" }).pipe(
      Effect.tap((previews) => invokeFileContextPreviewSuccess(callbacks.onSuccess, previews)),
      Effect.ensuring(invokeLoadCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "load file context previews",
      safeContext: { owner, name, number, path: source.path },
      onFailure: (failure) => invokeLoadFailure(callbacks.onFailure, filePreviewErrorMessage(failure)),
    });
  }

  function workspaceDiffQuery(base: WorkspaceDiffBase): {
    base: WorkspaceDiffBase;
    whitespace?: "hide";
    commit?: string;
    from?: string;
    to?: string;
  } {
    return {
      base,
      ...diffQuery(),
    };
  }

  function setActiveIfNeeded(files: { path: string }[] | undefined): void {
    if (!files?.some((f) => f.path === activeFile)) {
      activeFile = files?.[0]?.path ?? null;
    }
  }

  function resetDiffScopeState(): void {
    scope = { kind: "head" };
    fileCategoryFilter = "all";
    commits = null;
    commitsLoading = false;
    commitsError = null;
  }

  function currentWorkspaceOptions(): LoadWorkspaceDiffOptions {
    return {
      ...(currentWorkspaceHostKey ? { workspaceHostKey: currentWorkspaceHostKey } : {}),
      ...(currentWorkspaceLoadToken ? { loadToken: currentWorkspaceLoadToken } : {}),
    };
  }

  function resetScopeIfMissingFromLoadedCommits(): void {
    if (!commits || scope.kind === "head") return;
    const hasCommit = (sha: string) => commits?.some((commit) => commit.sha === sha) ?? false;
    const scopeStillExists =
      scope.kind === "commit" ? hasCommit(scope.sha) : hasCommit(scope.fromSha) && hasCommit(scope.toSha);
    if (scopeStillExists) return;
    scope = { kind: "head" };
  }

  function workspaceLoadIsCurrent(
    generation: number,
    workspaceID: string,
    workspaceHostKey: string | undefined,
    base: WorkspaceDiffBase,
  ): boolean {
    return (
      generation === workspaceLoadGeneration &&
      currentWorkspaceID === workspaceID &&
      currentWorkspaceHostKey === workspaceHostKey &&
      currentWorkspaceBase === base
    );
  }

  function applyFilesResult(data: FilesResponse): void {
    fileList = normalizeFilesResult(data);
    setActiveIfNeeded(getVisibleFileList()?.files);
  }

  function applyDiffResult(data: DiffResponse): void {
    diff = normalizeDiffResult(data);
    setActiveIfNeeded(getVisibleDiffFiles());
  }

  function loadDiff(owner: string, name: string, number: number, identity: ProviderRouteRef): void {
    const generation = ++workspaceLoadGeneration;
    activeWorkspaceLoad?.interrupt();
    activeWorkspaceLoad = null;
    currentWorkspaceLoadToken = undefined;
    const currentKey =
      currentOwner === ""
        ? ""
        : providerItemKey({
            provider: currentProvider,
            platformHost: resolvedPlatformHost(currentProvider, currentPlatformHost),
            owner: currentOwner,
            name: currentName,
            number: currentNumber,
          });
    const nextKey = providerItemKey({
      provider: identity.provider,
      platformHost: resolvedPlatformHost(identity.provider, identity.platformHost),
      owner,
      name,
      number,
    });
    const prChanged = nextKey !== currentKey;
    currentOwner = owner;
    currentName = name;
    currentNumber = number;
    currentWorkspaceID = "";
    currentWorkspaceHostKey = undefined;
    currentCommitSHA = "";
    currentProvider = identity.provider;
    currentPlatformHost = identity.platformHost;
    currentRepoPath = identity.repoPath;
    if (prChanged) {
      resetDiffScopeState();
    }

    startProviderDiffRead(currentRouteRef(), number, generation, { invalidate: false, loadFiles: true });
  }

  function startWorkspaceDiff(
    workspaceID: string,
    base: WorkspaceDiffBase,
    stacked = false,
    options: LoadWorkspaceDiffOptions = {},
    callbacks: DiffLoadCallbacks = {},
  ): AppExecution<void, ApiProblemError | TransientTransportError> {
    const generation = ++workspaceLoadGeneration;
    activeWorkspaceLoad?.interrupt();
    clearProviderDiffRead();
    const workspaceHostKey = options.workspaceHostKey;
    currentWorkspaceLoadToken = options.loadToken;
    const workspaceScopeChanged =
      workspaceID !== currentWorkspaceID ||
      base !== currentWorkspaceBase ||
      workspaceHostKey !== currentWorkspaceHostKey;
    const shouldRefreshCommits =
      options.refreshCommits === true &&
      !workspaceScopeChanged &&
      (commits !== null || commitsLoading || commitsError !== null);
    currentWorkspaceID = workspaceID;
    currentWorkspaceHostKey = workspaceHostKey;
    currentWorkspaceBase = base;
    currentWorkspaceStacked = stacked;
    currentOwner = "";
    currentName = "";
    currentNumber = 0;
    currentCommitSHA = "";
    if (workspaceScopeChanged) {
      resetDiffScopeState();
    }
    const isCurrent = () => workspaceLoadIsCurrent(generation, workspaceID, workspaceHostKey, base);
    let acknowledged = false;
    const program = Effect.gen(function* () {
      if (shouldRefreshCommits) {
        yield* loadCommitsEffect({ force: true });
        if (!isCurrent()) return;
        resetScopeIfMissingFromLoadedCommits();
      }

      if (!isCurrent()) return;
      clearFilePreviewCache();
      const visibleSnapshotVersion = diff?.snapshot_version;
      const preserveVisible =
        options.preserveVisible === true &&
        !workspaceScopeChanged &&
        visibleSnapshotVersion !== undefined &&
        fileList?.snapshot_version === visibleSnapshotVersion;
      let retryDelay = workspaceDiffRetryInitialDelay;
      while (isCurrent()) {
        let retry = false;
        for (let attempt = 0; attempt < 2; attempt += 1) {
          const preserveAttempt = preserveVisible || attempt > 0;
          yield* Effect.sync(() => {
            if (!preserveAttempt) {
              diff = null;
              fileList = null;
            }
            loading = true;
            fileListLoading = true;
            storeError = null;
          });
          const filesRequest = workspaceHostKey
            ? executeGeneratedDefaultResponse<FilesResponse>(
                "GET remote workspace diff files",
                (client, signal) =>
                  client.FleetService.getFleetWorkspaceFiles(
                    { hostKey: workspaceHostKey, id: workspaceID },
                    workspaceDiffQuery(base),
                    { signal },
                  ),
                (value): value is FilesResponse =>
                  typeof value === "object" && value !== null && "files" in value && !isProblem(value),
              )
            : executeGeneratedApiRequest<FilesResponse>("GET workspace diff files", (client, signal) =>
                client.WorkspacesService.getWorkspaceFiles({ id: workspaceID }, workspaceDiffQuery(base), { signal }),
              );
          const filesResult = yield* Effect.result(retryIdempotentRead(filesRequest));
          if (!isCurrent()) return;
          if (Result.isFailure(filesResult)) {
            if (!preserveVisible) return yield* Effect.fail(filesResult.failure);
            retry = true;
            break;
          }
          const pendingFiles = filesResult.success;
          if (!preserveAttempt) applyFilesResult(pendingFiles);
          fileListLoading = false;

          const query = {
            ...workspaceDiffQuery(base),
            ...(pendingFiles.snapshot_version && { revision: pendingFiles.snapshot_version }),
          };
          const diffRequest = workspaceHostKey
            ? executeGeneratedDefaultResponse<DiffResponse>(
                "GET remote workspace diff",
                (client, signal) =>
                  client.FleetService.getFleetWorkspaceDiff({ hostKey: workspaceHostKey, id: workspaceID }, query, {
                    signal,
                  }),
                (value): value is DiffResponse =>
                  typeof value === "object" && value !== null && "files" in value && !isProblem(value),
              )
            : executeGeneratedApiRequest<DiffResponse>("GET workspace diff", (client, signal) =>
                client.WorkspacesService.getWorkspaceDiff({ id: workspaceID }, query, { signal }),
              );
          const diffResult = yield* Effect.result(retryIdempotentRead(diffRequest));
          if (!isCurrent()) return;
          if (Result.isFailure(diffResult)) {
            if (
              diffResult.failure._tag === "ApiProblemError" &&
              isSnapshotChanged(diffResult.failure.problem) &&
              attempt === 0
            ) {
              continue;
            }
            if (!preserveVisible) return yield* Effect.fail(diffResult.failure);
            retry = true;
            break;
          }
          if (preserveVisible && (pendingFiles.stale || diffResult.success.stale)) {
            retry = true;
            break;
          }
          if (preserveAttempt) {
            fileList = normalizeFilesResult(pendingFiles);
            diff = normalizeDiffResult(diffResult.success);
            setActiveIfNeeded(getVisibleDiffFiles());
          } else {
            applyDiffResult(diffResult.success);
          }
          acknowledged = true;
          yield* invokeLoadCallback(callbacks.onSuccess);
          yield* invokeLoadCallback(callbacks.onSettled);
          return;
        }

        if (!retry) return;
        yield* Effect.sleep(retryDelay);
        retryDelay = Math.min(retryDelay * 2, workspaceDiffRetryMaxDelay);
      }
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          yield* Effect.sync(() => {
            if (isCurrent()) {
              loading = false;
              fileListLoading = false;
            }
          });
          if (!acknowledged) yield* invokeLoadCallback(callbacks.onSettled);
        }),
      ),
    );
    activeWorkspaceLoad = runtime.runCommand(program, {
      operation: "load workspace diff",
      safeContext: {
        workspaceID,
        ...(workspaceHostKey !== undefined && { workspaceHostKey }),
      },
      onFailure: (failure) => {
        if (!isCurrent()) return;
        const message = requestErrorMessage(failure, "failed to load workspace diff");
        storeError = message;
        if (!options.preserveVisible) {
          diff = null;
          fileList = null;
        }
        invokeLoadFailure(callbacks.onFailure, message);
      },
    });
    return activeWorkspaceLoad;
  }

  function loadWorkspaceDiff(
    workspaceID: string,
    base: WorkspaceDiffBase,
    stacked = false,
    options: LoadWorkspaceDiffOptions = {},
    callbacks: DiffLoadCallbacks = {},
  ): void {
    startWorkspaceDiff(workspaceID, base, stacked, options, callbacks);
  }

  function loadCommitDiff(identity: ProviderRouteRef, sha: string, callbacks: DiffLoadCallbacks = {}): void {
    const generation = ++workspaceLoadGeneration;
    activeWorkspaceLoad?.interrupt();
    activeWorkspaceLoad = null;
    clearProviderDiffRead();
    currentWorkspaceLoadToken = undefined;
    const commitChanged = identity.owner !== currentOwner || identity.name !== currentName || sha !== currentCommitSHA;
    currentOwner = identity.owner;
    currentName = identity.name;
    currentNumber = 0;
    currentWorkspaceID = "";
    currentWorkspaceHostKey = undefined;
    currentCommitSHA = sha;
    currentProvider = identity.provider;
    currentPlatformHost = identity.platformHost;
    currentRepoPath = identity.repoPath;
    if (commitChanged) {
      resetDiffScopeState();
    }
    clearFilePreviewCache();

    activeCommitLoad?.interrupt();
    fileListLoading = false;
    diff = null;
    fileList = null;
    loading = true;
    storeError = null;

    const ref = currentRouteRef();
    let settled = false;
    const isCurrent = () => generation === workspaceLoadGeneration && currentCommitSHA === sha;
    const program = executeGeneratedApiRequest("GET repository commit diff", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.RepositoriesService.getRepoCommitDiffOnHost(
            { ...providerHostRouteParams(ref), sha: sha },
            {
              ...(hideWhitespace && { whitespace: "hide" }),
            },
            { signal },
          )
        : client.RepositoriesService.getRepoCommitDiff(
            { ...providerRouteParams(ref), sha: sha },
            {
              ...(hideWhitespace && { whitespace: "hide" }),
            },
            { signal },
          ),
    ).pipe(
      retryIdempotentRead,
      Effect.tap((data) =>
        Effect.sync(() => {
          if (isCurrent()) applyDiffResult(data);
        }),
      ),
      Effect.tap(() =>
        Effect.sync(() => {
          settled = true;
        }),
      ),
      Effect.andThen(invokeLoadCallback(callbacks.onSuccess)),
      Effect.andThen(invokeLoadCallback(callbacks.onSettled)),
      Effect.ensuring(
        Effect.gen(function* () {
          yield* Effect.sync(() => {
            if (isCurrent()) loading = false;
          });
          if (!settled) yield* invokeLoadCallback(callbacks.onSettled);
        }),
      ),
    );
    activeCommitLoad = runtime.runCommand(program, {
      operation: "load repository commit diff",
      safeContext: {
        provider: ref.provider,
        platformHost: resolvedPlatformHost(ref.provider, ref.platformHost),
        owner: ref.owner,
        name: ref.name,
      },
      onFailure: (failure) => {
        if (!isCurrent()) return;
        const message = requestErrorMessage(failure, "failed to load commit diff");
        storeError = message;
        diff = null;
        fileList = null;
        invokeLoadFailure(callbacks.onFailure, message);
      },
    });
  }

  function clearDiff(): void {
    workspaceLoadGeneration += 1;
    clearProviderDiffRead();
    currentWorkspaceLoadToken = undefined;
    commitsGeneration += 1;
    activeCommitLoad?.interrupt();
    activeCommitLoad = null;
    activeWorkspaceLoad?.interrupt();
    activeWorkspaceLoad = null;
    activeCommitsLoad?.interrupt();
    activeCommitsLoad = null;
    diff = null;
    fileList = null;
    storeError = null;
    loading = false;
    fileListLoading = false;
    activeFile = null;
    scrollTarget = null;
    scrolling = false;
    fileCategoryFilter = "all";
    commits = null;
    commitsLoading = false;
    commitsError = null;
    scope = { kind: "head" };
    clearFilePreviewCache();
    currentOwner = "";
    currentName = "";
    currentNumber = 0;
    currentWorkspaceID = "";
    currentWorkspaceHostKey = undefined;
    currentWorkspaceBase = "head";
    currentWorkspaceStacked = false;
    currentCommitSHA = "";
    currentProvider = "";
    currentPlatformHost = undefined;
    currentRepoPath = "";
  }

  function cancelWorkspaceDiff(workspaceID: string, workspaceHostKey?: string, loadToken?: object): void {
    if (workspaceID !== currentWorkspaceID || workspaceHostKey !== currentWorkspaceHostKey) return;
    if (loadToken !== undefined && loadToken !== currentWorkspaceLoadToken) return;

    workspaceLoadGeneration += 1;
    currentWorkspaceLoadToken = undefined;
    commitsGeneration += 1;
    activeCommitLoad?.interrupt();
    activeCommitLoad = null;
    activeWorkspaceLoad?.interrupt();
    activeWorkspaceLoad = null;
    activeCommitsLoad?.interrupt();
    activeCommitsLoad = null;
    loading = false;
    fileListLoading = false;
    commitsLoading = false;
    storeError = null;
    clearFilePreviewCache();
  }

  function loadCommitsEffect(
    options: LoadCommitsOptions = {},
  ): Effect.Effect<boolean, ApiProblemError | TransientTransportError, GeneratedApi> {
    return Effect.suspend(() => {
      if (options.force) {
        activeCommitsLoad?.interrupt();
        commitsGeneration += 1;
        commits = null;
        commitsLoading = false;
        commitsError = null;
      } else if (commits || commitsLoading) {
        return Effect.succeed(false);
      }
      if (!currentWorkspaceID && (!currentOwner || !currentName || !currentNumber)) {
        return Effect.succeed(false);
      }

      commitsLoading = true;
      commitsError = null;
      const generation = commitsGeneration;
      const owner = currentOwner;
      const name = currentName;
      const number = currentNumber;
      const workspaceID = currentWorkspaceID;
      const workspaceHostKey = currentWorkspaceHostKey;
      const ref = currentRouteRef();
      const isCurrent = () =>
        currentWorkspaceID === workspaceID &&
        currentWorkspaceHostKey === workspaceHostKey &&
        currentOwner === owner &&
        currentName === name &&
        currentNumber === number &&
        generation === commitsGeneration;
      const request = workspaceID
        ? workspaceHostKey
          ? executeGeneratedDefaultResponse<CommitsResponse>(
              "GET remote workspace commits",
              (client, signal) =>
                client.FleetService.getFleetWorkspaceCommits(
                  { hostKey: workspaceHostKey, id: workspaceID },
                  { signal },
                ),
              (value): value is CommitsResponse =>
                typeof value === "object" && value !== null && "commits" in value && !isProblem(value),
            )
          : executeGeneratedApiRequest<CommitsResponse>("GET workspace commits", (client, signal) =>
              client.WorkspacesService.getWorkspaceCommits({ id: workspaceID }, { signal }),
            )
        : executeGeneratedApiRequest<CommitsResponse>("GET pull request commits", (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.PullRequestsService.getPullCommitsOnHost(
                  { ...providerHostRouteParams(ref), number: number },
                  { signal },
                )
              : client.PullRequestsService.getPullCommits({ ...providerRouteParams(ref), number: number }, { signal }),
          );
      return request.pipe(
        retryIdempotentRead,
        Effect.tap((data) =>
          Effect.sync(() => {
            if (isCurrent()) commits = data.commits ?? [];
          }),
        ),
        Effect.as(true),
        Effect.ensuring(
          Effect.sync(() => {
            if (isCurrent()) commitsLoading = false;
          }),
        ),
      );
    });
  }

  function loadCommits(options: LoadCommitsOptions = {}, callbacks: DiffLoadCallbacks = {}): void {
    let settled = false;
    const program = Effect.gen(function* () {
      const loaded = yield* loadCommitsEffect(options);
      if (loaded) yield* invokeLoadCallback(callbacks.onSuccess);
      settled = true;
      yield* invokeLoadCallback(callbacks.onSettled);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!settled) yield* invokeLoadCallback(callbacks.onSettled);
        }),
      ),
    );
    const workspaceID = currentWorkspaceID;
    const workspaceHostKey = currentWorkspaceHostKey;
    const owner = currentOwner;
    const name = currentName;
    const number = currentNumber;
    const generation = commitsGeneration + (options.force ? 1 : 0);
    const isCurrent = () =>
      currentWorkspaceID === workspaceID &&
      currentWorkspaceHostKey === workspaceHostKey &&
      currentOwner === owner &&
      currentName === name &&
      currentNumber === number &&
      generation === commitsGeneration;
    activeCommitsLoad = runtime.runCommand(program, {
      operation: "load diff commits",
      safeContext: {
        ...(workspaceID ? { workspaceID } : { owner, name, number }),
      },
      onFailure: (failure) => {
        if (!isCurrent()) return;
        const message = requestErrorMessage(failure, "failed to load commits");
        commitsError = message;
        invokeLoadFailure(callbacks.onFailure, message);
      },
    });
  }

  function getScope(): DiffScope {
    return scope;
  }

  function getCommits(): CommitInfo[] | null {
    return commits;
  }

  function isCommitsLoading(): boolean {
    return commitsLoading;
  }

  function getCommitsError(): string | null {
    return commitsError;
  }

  function selectCommit(sha: string): void {
    scope = { kind: "commit", sha };
    clearFilePreviewCache();
    if (currentOwner && currentName && currentNumber) {
      loadDiff(currentOwner, currentName, currentNumber, currentRouteRef());
    } else if (currentWorkspaceID) {
      loadWorkspaceDiff(currentWorkspaceID, currentWorkspaceBase, currentWorkspaceStacked, currentWorkspaceOptions());
    }
  }

  function selectRange(fromSha: string, toSha: string): void {
    if (!commits) return;
    const fromIdx = commits.findIndex((c) => c.sha === fromSha);
    const toIdx = commits.findIndex((c) => c.sha === toSha);
    if (fromIdx === -1 || toIdx === -1) return;
    const [older, newer] = fromIdx > toIdx ? [fromSha, toSha] : [toSha, fromSha];
    scope = { kind: "range", fromSha: older, toSha: newer };
    clearFilePreviewCache();
    if (currentOwner && currentName && currentNumber) {
      loadDiff(currentOwner, currentName, currentNumber, currentRouteRef());
    } else if (currentWorkspaceID) {
      loadWorkspaceDiff(currentWorkspaceID, currentWorkspaceBase, currentWorkspaceStacked, currentWorkspaceOptions());
    }
  }

  function resetToHead(): void {
    scope = { kind: "head" };
    clearFilePreviewCache();
    if (currentOwner && currentName && currentNumber) {
      loadDiff(currentOwner, currentName, currentNumber, currentRouteRef());
    } else if (currentWorkspaceID) {
      loadWorkspaceDiff(currentWorkspaceID, currentWorkspaceBase, currentWorkspaceStacked, currentWorkspaceOptions());
    }
  }

  function stepPrev(): void {
    if (!commits) {
      loadCommits();
      return;
    }
    if (commits.length === 0) return;
    const s = scope;
    if (s.kind === "head") {
      selectCommit(commits[0]!.sha);
    } else if (s.kind === "commit") {
      const idx = commits.findIndex((c) => c.sha === s.sha);
      if (idx < commits.length - 1) selectCommit(commits[idx + 1]!.sha);
    } else {
      selectCommit(s.fromSha);
    }
  }

  function stepNext(): void {
    if (!commits) {
      loadCommits();
      return;
    }
    if (commits.length === 0) return;
    const s = scope;
    if (s.kind === "head") {
      return;
    } else if (s.kind === "commit") {
      const idx = commits.findIndex((c) => c.sha === s.sha);
      if (idx > 0) {
        selectCommit(commits[idx - 1]!.sha);
      } else {
        resetToHead();
      }
    } else {
      selectCommit(s.toSha);
    }
  }

  return {
    getDiff,
    getCurrentPR,
    isDiffLoading,
    getDiffError,
    getFileList,
    getVisibleFileList,
    getVisibleDiffFiles,
    getFileCategoryCounts,
    isFileListLoading,
    getTabWidth,
    getWordWrap,
    getRichPreview,
    getFilePreviewGeneration,
    getHideWhitespace,
    getViewMode,
    getFileCategoryFilter,
    getActiveFile,
    getActiveFileRevealKey,
    setActiveFile,
    setFileCategoryFilter,
    isScrolling,
    clearScrolling,
    requestScrollToFile,
    requestScrollToLine,
    getScrollTarget,
    consumeScrollTarget,
    setTabWidth,
    setWordWrap,
    setRichPreview,
    setHideWhitespace,
    setViewMode,
    isFileCollapsed,
    areAllVisibleFilesCollapsed,
    toggleFileCollapsed,
    setAllVisibleFilesCollapsed,
    loadDiff,
    loadCommitDiff,
    loadFilePreview,
    loadFileContextPreviews,
    loadWorkspaceDiff,
    cancelWorkspaceDiff,
    clearDiff,
    getScope,
    getCommits,
    isCommitsLoading,
    getCommitsError,
    loadCommits,
    selectCommit,
    selectRange,
    resetToHead,
    stepPrev,
    stepNext,
  };
}

export type DiffStore = ReturnType<typeof createDiffStore>;
