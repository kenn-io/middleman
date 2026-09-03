import { Effect } from "effect";
import type { RepoBrowserBlob, RepoBrowserCommit, RepoBrowserRef, RepoBrowserTreeEntry } from "../api/types.js";
import {
  providerRouteParams,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { executeGeneratedApiRequest, type GeneratedApi } from "../api/generated-api.js";
import type { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import type {
  RepoBrowserBlobResponse as GeneratedRepoBrowserBlobResponse,
  RepoBrowserCommitResponse as GeneratedRepoBrowserCommitResponse,
  RepoBrowserHistoryResponse as GeneratedRepoBrowserHistoryResponse,
  RepoBrowserLastChangedResponse as GeneratedRepoBrowserLastChangedResponse,
  RepoBrowserRefsResponse as GeneratedRepoBrowserRefsResponse,
  RepoBrowserTreeResponse as GeneratedRepoBrowserTreeResponse,
} from "../api/generated/models/index.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import type { DiffFileCategoryCounts, DiffFileCategoryFilter } from "../utils/diff-categories.js";
import { chooseRepoBrowserInitialPath } from "../utils/repo-browser-path.js";
import {
  buildSourceBrowserFileEntries,
  countSourceBrowserFileEntriesByCategory,
  filterSourceBrowserFileEntriesByCategory,
  type SourceBrowserFileEntry,
} from "../utils/source-browser-files.js";
import {
  RepoBrowserWorkflow,
  type RepoBrowserOwner,
  type RepoBrowserWorkflowService,
} from "./repo-browser-workflow.js";

export type RepoBrowserViewMode = "source" | "preview";

type RepoBrowserRefsResponse = GeneratedRepoBrowserRefsResponse;
type RepoBrowserTreeResponse = GeneratedRepoBrowserTreeResponse;
type RepoBrowserBlobResponse = GeneratedRepoBrowserBlobResponse;
type RepoBrowserHistoryResponse = GeneratedRepoBrowserHistoryResponse;
type RepoBrowserLastChangedResponse = GeneratedRepoBrowserLastChangedResponse;
type RepoBrowserCommitResponse = GeneratedRepoBrowserCommitResponse;
type MissingRequestedPathBehavior = "fallback" | "retain";
type RepoBrowserPathKind = "file" | "directory" | "missing";
type RepoBrowserPathSnapshot = {
  selectedPath: string | null;
  blob: RepoBrowserBlob | null;
  fileHistory: RepoBrowserCommit[];
  selectedCommit: RepoBrowserCommit | null;
};

const viewModeStorageKey = "repo-browser-view-mode";
const lastChangedBatchSize = 250;
const validViewModes: RepoBrowserViewMode[] = ["source", "preview"];
let nextRepoBrowserOwner = 0;

function createRepoBrowserOwner(): RepoBrowserOwner {
  nextRepoBrowserOwner += 1;
  return `repo-browser:${nextRepoBrowserOwner}`;
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

function loadViewMode(): RepoBrowserViewMode {
  const raw = safeGetItem(viewModeStorageKey);
  return raw !== null && isRepoBrowserViewMode(raw) ? raw : "source";
}

function isRepoBrowserViewMode(value: string): value is RepoBrowserViewMode {
  return validViewModes.some((mode) => mode === value);
}

function readErrorMessage(error: ApiProblemError | TransientTransportError, fallback: string): string {
  if (error._tag === "ApiProblemError") {
    return error.problem.detail ?? error.problem.title ?? fallback;
  }
  return "Could not reach Kenn Forge";
}

function normalizeRepoBrowserTreePath(path: string): string {
  return path.replace(/^\/+|\/+$/g, "");
}

export function createRepoBrowserStore() {
  let activeOwner = createRepoBrowserOwner();
  let repo = $state<ProviderRouteRef | null>(null);
  let refs = $state.raw<RepoBrowserRef[]>([]);
  let defaultRef = $state<RepoBrowserRef | null>(null);
  let selectedRef = $state<RepoBrowserRef | null>(null);
  let tree = $state.raw<RepoBrowserTreeEntry[]>([]);
  let treeTruncated = $state(false);
  let lastChanged = $state.raw<Record<string, RepoBrowserCommit>>({});
  let selectedPath = $state<string | null>(null);
  let blob = $state<RepoBrowserBlob | null>(null);
  let fileHistory = $state.raw<RepoBrowserCommit[]>([]);
  let selectedCommit = $state<RepoBrowserCommit | null>(null);
  let lastUsablePathState: RepoBrowserPathSnapshot | null = null;
  let fileCategoryFilter = $state<DiffFileCategoryFilter>("all");
  let viewMode = $state<RepoBrowserViewMode>(loadViewMode());
  let loading = $state(false);
  let blobLoading = $state(false);
  let error = $state<string | null>(null);

  const fileEntries = $derived(buildSourceBrowserFileEntries(tree, lastChanged));
  const visibleFileEntries = $derived(filterSourceBrowserFileEntriesByCategory(fileEntries, fileCategoryFilter));
  const fileCategoryCounts = $derived(countSourceBrowserFileEntriesByCategory(fileEntries));

  function active(owner: RepoBrowserOwner): Effect.Effect<void> {
    return Effect.suspend(() => (owner === activeOwner ? Effect.void : Effect.interrupt));
  }

  function queryFor(ref: ProviderRouteRef, selected: RepoBrowserRef | null = selectedRef) {
    return {
      repo_path: ref.repoPath,
      ...(selected && {
        ref_type: selected.type,
        ...(selected.name ? { ref_name: selected.name } : {}),
      }),
      ...(selected?.sha ? { ref_sha: selected.sha } : {}),
    };
  }

  function treeContentQueryRef(): RepoBrowserRef | null {
    if (!selectedRef) return null;
    if (!selectedRef.sha) return selectedRef;
    return {
      type: "commit",
      name: "",
      sha: selectedRef.sha,
      stale: false,
    };
  }

  function prioritizePath(paths: string[], priorityPath: string | undefined): string[] {
    if (!priorityPath) return paths;
    const index = paths.indexOf(priorityPath);
    if (index <= 0) return paths;
    return [priorityPath, ...paths.slice(0, index), ...paths.slice(index + 1)];
  }

  function loadRepo(
    owner: RepoBrowserOwner,
    nextRepo: ProviderRouteRef,
    initial?: { ref?: RepoBrowserRef; path?: string | null },
  ) {
    return Effect.gen(function* () {
      const workflow = yield* RepoBrowserWorkflow;
      return yield* workflow.repo(
        owner,
        active(owner)
          .pipe(
            Effect.andThen(
              Effect.sync(() => {
                repo = nextRepo;
                loading = true;
                error = null;
                clearRepoData();
              }),
            ),
          )
          .pipe(
            Effect.andThen(
              executeGeneratedApiRequest("GET repository refs", (client, signal) =>
                providerUsesHostRoute(nextRepo)
                  ? client.RepositoriesService.listRepoBrowserRefsOnHost(
                      { ...providerHostRouteParams(nextRepo) },
                      { repo_path: nextRepo.repoPath },
                      { signal },
                    )
                  : client.RepositoriesService.listRepoBrowserRefs(
                      { ...providerRouteParams(nextRepo) },
                      { repo_path: nextRepo.repoPath },
                      { signal },
                    ),
              ).pipe(retryIdempotentRead),
            ),
            Effect.tap((response: RepoBrowserRefsResponse) =>
              active(owner).pipe(Effect.andThen(Effect.sync(() => applyRefs(response, initial?.ref)))),
            ),
            Effect.andThen(
              loadTree(workflow, owner, initial?.path ?? null, initial?.path ? "retain" : "fallback").pipe(
                Effect.catch((failure) =>
                  active(owner).pipe(
                    Effect.andThen(
                      Effect.sync(() => {
                        error = readErrorMessage(failure, "failed to load repository tree");
                        clearTreeData();
                      }),
                    ),
                  ),
                ),
              ),
            ),
            Effect.tapError((failure) =>
              active(owner).pipe(
                Effect.andThen(
                  Effect.sync(() => {
                    error = readErrorMessage(failure, "failed to load repository refs");
                    clearRepoData();
                    loading = false;
                  }),
                ),
              ),
            ),
            Effect.tap(() => active(owner).pipe(Effect.andThen(Effect.sync(() => (loading = false))))),
          ),
      );
    });
  }

  function applyRefs(response: RepoBrowserRefsResponse, requestedRef?: RepoBrowserRef | undefined): void {
    refs = response.refs ?? [];
    defaultRef = response.default_ref;
    selectedRef = chooseSelectedRef(requestedRef);
  }

  function chooseSelectedRef(requestedRef?: RepoBrowserRef | undefined): RepoBrowserRef | null {
    if (requestedRef) {
      if (!requestedRef.sha && requestedRef.type !== "commit") {
        const resolved = refs.find(
          (candidate) => candidate.type === requestedRef.type && candidate.name === requestedRef.name,
        );
        if (resolved) return resolved;
      }
      return requestedRef;
    }
    return defaultRef ?? refs[0] ?? null;
  }

  function loadTree(
    workflow: RepoBrowserWorkflowService,
    owner: RepoBrowserOwner,
    requestedPath: string | null = null,
    missingPathBehavior: MissingRequestedPathBehavior = "fallback",
  ) {
    return workflow.tree(
      owner,
      Effect.suspend(() => {
        const ref = repo;
        const requestedRef = selectedRef;
        if (!ref || !requestedRef) return Effect.void;
        return executeGeneratedApiRequest("GET repository tree", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.RepositoriesService.listRepoBrowserTreeOnHost(
                { ...providerHostRouteParams(ref) },
                queryFor(ref, requestedRef),
                { signal },
              )
            : client.RepositoriesService.listRepoBrowserTree(
                { ...providerRouteParams(ref) },
                queryFor(ref, requestedRef),
                { signal },
              ),
        ).pipe(
          retryIdempotentRead,
          Effect.tap(() => active(owner)),
          Effect.flatMap((payload: RepoBrowserTreeResponse) =>
            Effect.sync(() => {
              selectedRef = payload.ref ?? requestedRef;
              tree = payload.entries ?? [];
              treeTruncated = payload.truncated;
              lastChanged = {};
              const requestedPathKind = requestedPath ? repoBrowserPathKind(requestedPath) : "missing";
              const requestedPathExists = requestedPathKind === "file" || requestedPathKind === "directory";
              return requestedPathExists || !requestedPath || missingPathBehavior === "fallback"
                ? requestedPathExists
                  ? requestedPath
                  : chooseRepoBrowserInitialPath(tree)
                : null;
            }),
          ),
          Effect.flatMap((firstPath) => {
            if (firstPath) {
              return workflow.initialPath(
                owner,
                selectPathProgram(owner, firstPath).pipe(
                  Effect.catch((failure) =>
                    active(owner).pipe(Effect.andThen(Effect.sync(() => applyPathFailure(failure)))),
                  ),
                ),
              );
            }
            return Effect.sync(() => {
              if (requestedPath && missingPathBehavior === "retain") {
                selectedPath = requestedPath;
                blob = null;
                fileHistory = [];
                selectedCommit = null;
                error = `Path not found: ${requestedPath}`;
                return;
              }
              selectedPath = null;
              blob = null;
              fileHistory = [];
              selectedCommit = null;
              rememberUsablePathState();
            });
          }),
          Effect.tap(() =>
            workflow.startMetadata(owner, loadLastChanged(owner, selectedPath ?? requestedPath ?? undefined)),
          ),
        );
      }),
    );
  }

  function loadLastChanged(
    owner: RepoBrowserOwner,
    priorityPath?: string,
  ): Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi> {
    return Effect.suspend(() => {
      const ref = repo;
      const requestedRef = treeContentQueryRef();
      if (!ref || !requestedRef) return Effect.void;
      const paths = prioritizePath(
        tree.map((entry) => entry.path),
        priorityPath,
      );
      if (paths.length === 0) return Effect.sync(() => (lastChanged = {}));
      const commits: Record<string, RepoBrowserCommit> = {};
      return Effect.forEach(
        Array.from({ length: Math.ceil(paths.length / lastChangedBatchSize) }, (_, index) => index),
        (index) => {
          const batch = paths.slice(index * lastChangedBatchSize, (index + 1) * lastChangedBatchSize);
          return executeGeneratedApiRequest("GET repository last-changed metadata", (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.RepositoriesService.getRepoBrowserLastChangedOnHost(
                  { ...providerHostRouteParams(ref) },
                  {
                    ...queryFor(ref, requestedRef),
                    path: batch,
                  },
                  { signal },
                )
              : client.RepositoriesService.getRepoBrowserLastChanged(
                  { ...providerRouteParams(ref) },
                  {
                    ...queryFor(ref, requestedRef),
                    path: batch,
                  },
                  { signal },
                ),
          ).pipe(
            retryIdempotentRead,
            Effect.tap(() => active(owner)),
            Effect.tap((response: RepoBrowserLastChangedResponse) =>
              Effect.sync(() => {
                Object.assign(commits, response.commits ?? {});
                lastChanged = { ...commits };
              }),
            ),
          );
        },
        { concurrency: 1, discard: true },
      );
    });
  }

  function selectRef(owner: RepoBrowserOwner, ref: RepoBrowserRef) {
    return Effect.gen(function* () {
      const workflow = yield* RepoBrowserWorkflow;
      return yield* active(owner).pipe(
        Effect.andThen(
          Effect.suspend(() => {
            const previousPathState = blobLoading && lastUsablePathState ? lastUsablePathState : currentPathState();
            const previousState = {
              selectedRef,
              tree,
              treeTruncated,
              lastChanged,
              error,
              ...previousPathState,
            };
            selectedRef = ref;
            loading = true;
            error = null;
            clearTreeData();
            return loadTree(workflow, owner, previousState.selectedPath).pipe(
              Effect.as(true),
              Effect.catch(() =>
                active(owner).pipe(
                  Effect.andThen(
                    Effect.sync(() => {
                      ({
                        selectedRef,
                        tree,
                        treeTruncated,
                        lastChanged,
                        error,
                        selectedPath,
                        blob,
                        fileHistory,
                        selectedCommit,
                      } = previousState);
                      blobLoading = false;
                      rememberUsablePathState();
                      return false;
                    }),
                  ),
                ),
              ),
              Effect.tap(() => active(owner).pipe(Effect.andThen(Effect.sync(() => (loading = false))))),
            );
          }),
        ),
      );
    });
  }

  function selectPath(owner: RepoBrowserOwner, path: string) {
    return Effect.gen(function* () {
      const workflow = yield* RepoBrowserWorkflow;
      return yield* workflow
        .path(owner, selectPathProgram(owner, path))
        .pipe(
          Effect.catch((failure) => active(owner).pipe(Effect.andThen(Effect.sync(() => applyPathFailure(failure))))),
        );
    });
  }

  function applyPathFailure(failure: ApiProblemError | TransientTransportError): void {
    error = readErrorMessage(failure, "failed to load repository path");
    blob = null;
    fileHistory = [];
    selectedCommit = null;
    blobLoading = false;
  }

  function selectPathProgram(
    owner: RepoBrowserOwner,
    path: string,
  ): Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi> {
    return active(owner).pipe(
      Effect.andThen(
        Effect.suspend(() => {
          const ref = repo;
          const requestedRef = treeContentQueryRef();
          if (!ref || !requestedRef) return Effect.void;
          selectedPath = path;
          const pathKind = repoBrowserPathKind(path);
          blobLoading = pathKind !== "directory";
          error = null;
          blob = null;
          fileHistory = [];
          selectedCommit = null;
          if (pathKind === "directory") {
            rememberUsablePathState();
            return Effect.void;
          }
          const query = {
            ...queryFor(ref, requestedRef),
            path,
          };
          return Effect.all(
            {
              blobResponse: executeGeneratedApiRequest("GET repository blob", (client, signal) =>
                providerUsesHostRoute(ref)
                  ? client.RepositoriesService.getRepoBrowserBlobOnHost({ ...providerHostRouteParams(ref) }, query, {
                      signal,
                    })
                  : client.RepositoriesService.getRepoBrowserBlob({ ...providerRouteParams(ref) }, query, { signal }),
              ).pipe(retryIdempotentRead),
              historyResponse: executeGeneratedApiRequest("GET repository file history", (client, signal) =>
                providerUsesHostRoute(ref)
                  ? client.RepositoriesService.getRepoBrowserHistoryOnHost({ ...providerHostRouteParams(ref) }, query, {
                      signal,
                    })
                  : client.RepositoriesService.getRepoBrowserHistory({ ...providerRouteParams(ref) }, query, {
                      signal,
                    }),
              ).pipe(retryIdempotentRead),
            },
            { concurrency: "unbounded" },
          ).pipe(
            Effect.tap(() => active(owner)),
            Effect.tap(
              ({
                blobResponse,
                historyResponse,
              }: {
                blobResponse: RepoBrowserBlobResponse;
                historyResponse: RepoBrowserHistoryResponse;
              }) =>
                Effect.sync(() => {
                  blob = blobResponse.blob;
                  fileHistory = historyResponse.commits ?? [];
                  selectedCommit = fileHistory[0] ?? null;
                  blobLoading = false;
                  rememberUsablePathState();
                }),
            ),
          );
        }),
      ),
    );
  }

  function repoBrowserPathKind(path: string): RepoBrowserPathKind {
    const normalized = normalizeRepoBrowserTreePath(path);
    if (!normalized) return "missing";
    const entry = tree.find((candidate) => normalizeRepoBrowserTreePath(candidate.path) === normalized);
    if (entry) return isRepoBrowserFileEntry(entry) ? "file" : "directory";
    if (tree.some((candidate) => normalizeRepoBrowserTreePath(candidate.path).startsWith(`${normalized}/`))) {
      return "directory";
    }
    return "missing";
  }

  function isRepoBrowserFileEntry(entry: RepoBrowserTreeEntry): boolean {
    return entry.type === "blob" || entry.type === "file";
  }

  function selectCommit(owner: RepoBrowserOwner, sha: string) {
    return Effect.gen(function* () {
      const workflow = yield* RepoBrowserWorkflow;
      return yield* workflow
        .commit(
          owner,
          active(owner).pipe(
            Effect.andThen(
              Effect.suspend(() => {
                const ref = repo;
                const requestedRef = treeContentQueryRef();
                const path = selectedPath;
                if (!ref || !requestedRef || !path) return Effect.void;
                selectedCommit = null;
                error = null;
                return executeGeneratedApiRequest("GET repository commit", (client, signal) =>
                  providerUsesHostRoute(ref)
                    ? client.RepositoriesService.getRepoBrowserCommitOnHost(
                        { ...providerHostRouteParams(ref) },
                        {
                          ...queryFor(ref, requestedRef),
                          path,
                          sha,
                        },
                        { signal },
                      )
                    : client.RepositoriesService.getRepoBrowserCommit(
                        { ...providerRouteParams(ref) },
                        {
                          ...queryFor(ref, requestedRef),
                          path,
                          sha,
                        },
                        { signal },
                      ),
                ).pipe(
                  retryIdempotentRead,
                  Effect.tap(() => active(owner)),
                  Effect.tap((response: RepoBrowserCommitResponse) =>
                    Effect.sync(() => {
                      selectedCommit = response.commit;
                      rememberUsablePathState();
                    }),
                  ),
                );
              }),
            ),
          ),
        )
        .pipe(
          Effect.catch((failure) =>
            active(owner).pipe(
              Effect.andThen(
                Effect.sync(() => {
                  error = readErrorMessage(failure, "failed to load repository commit");
                  selectedCommit = null;
                }),
              ),
            ),
          ),
        );
    });
  }

  function clearRepoData(): void {
    refs = [];
    defaultRef = null;
    selectedRef = null;
    lastUsablePathState = null;
    clearTreeData();
  }

  function currentPathState(): RepoBrowserPathSnapshot {
    return { selectedPath, blob, fileHistory, selectedCommit };
  }

  function rememberUsablePathState(): void {
    lastUsablePathState = currentPathState();
  }

  function clearTreeData(): void {
    tree = [];
    treeTruncated = false;
    lastChanged = {};
    selectedPath = null;
    blob = null;
    fileHistory = [];
    selectedCommit = null;
    blobLoading = false;
  }

  function setFileCategoryFilter(filter: DiffFileCategoryFilter): void {
    fileCategoryFilter = filter;
  }

  function setViewMode(mode: RepoBrowserViewMode): void {
    viewMode = mode;
    safeSetItem(viewModeStorageKey, mode);
  }

  const view = {
    getRepo: () => repo,
    getRefs: () => refs,
    getDefaultRef: () => defaultRef,
    getSelectedRef: () => selectedRef,
    getTree: () => tree,
    isTreeTruncated: () => treeTruncated,
    getLastChanged: () => lastChanged,
    getSelectedPath: () => selectedPath,
    getBlob: () => blob,
    getFileHistory: () => fileHistory,
    getSelectedCommit: () => selectedCommit,
    getFileEntries: (): SourceBrowserFileEntry[] => fileEntries,
    getVisibleFileEntries: (): SourceBrowserFileEntry[] => visibleFileEntries,
    getFileCategoryFilter: () => fileCategoryFilter,
    getFileCategoryCounts: (): DiffFileCategoryCounts => fileCategoryCounts,
    getViewMode: () => viewMode,
    isLoading: () => loading,
    isBlobLoading: () => blobLoading,
    getError: () => error,
    setFileCategoryFilter,
    setViewMode,
  };

  function mount() {
    activeOwner = createRepoBrowserOwner();
    const owner = activeOwner;
    return {
      ...view,
      loadRepo: (nextRepo: ProviderRouteRef, initial?: { ref?: RepoBrowserRef; path?: string | null }) =>
        loadRepo(owner, nextRepo, initial),
      selectRef: (ref: RepoBrowserRef) => selectRef(owner, ref),
      selectPath: (path: string) => selectPath(owner, path),
      selectCommit: (sha: string) => selectCommit(owner, sha),
      stop: () => stop(owner),
    };
  }

  function stop(owner: RepoBrowserOwner) {
    return Effect.gen(function* () {
      const workflow = yield* RepoBrowserWorkflow;
      yield* workflow.stop(owner);
      if (owner !== activeOwner) return;
      yield* Effect.sync(() => {
        loading = false;
        blobLoading = false;
      });
    });
  }

  return { ...view, mount };
}

export type RepoBrowserStore = ReturnType<typeof createRepoBrowserStore>;
