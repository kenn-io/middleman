import { Effect } from "effect";
import type { AppRuntime } from "../app/runtime.js";
import { TransientTransportError } from "../api/effect-errors.js";
import type { ApiProblemError } from "../api/effect-errors.js";
import { executeGeneratedApiRequest, type GeneratedApi } from "../api/generated-api.js";
import type {
  GithubStateInputBody,
  Issue,
  IssueDetail,
  IssueEvent,
  IssuesParams,
  IssueSettings,
  Label,
  UnsetStarredParams,
} from "../api/types.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import {
  canonicalProvider,
  providerDefaultHost,
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { showFlash } from "./flash.svelte.js";
import { IssuesWorkflow, type IssueDetailSyncMode } from "./issues-workflow.js";
import {
  invokeMutationCallback,
  invokeMutationFailure,
  ProviderMutations,
  providerMutationFailureMessage,
  type MutationCallbacks,
  type ProviderMutationFailure,
} from "./ordered-mutations.js";
import { providerItemKey } from "./provider-key.js";
import { SettingsWorkflow, settingsErrorMessage } from "./settings-workflow.js";
import { nextWorkspaceLifecycleTick } from "./workspace-create-pending.svelte.js";
import { readInvolvesMeFilter, writeInvolvesMeFilter } from "./involves-me-filter.js";
import { readUnassignedFilter, writeUnassignedFilter } from "./unassigned-filter.js";
import { readIssuePRReferenceFilter, writeIssuePRReferenceFilter } from "./issue-pr-reference-filter.js";

export type { IssueDetailSyncMode } from "./issues-workflow.js";

export interface IssueSelection {
  provider: string;
  platformHost?: string | undefined;
  owner: string;
  name: string;
  repoPath: string;
  number: number;
}

export interface IssueDetailRequestOptions {
  sync?: IssueDetailSyncMode;
  provider: string;
  platformHost?: string | undefined;
  repoPath: string;
}

type IssueDetailRequestRef = {
  owner: string;
  name: string;
  number: number;
  provider: string;
  platformHost?: string | undefined;
  repoPath: string;
};

interface IssueDetailRefreshResult {
  readonly applied: boolean;
  readonly fetchedAt?: string;
}

interface IssueCommentMutationState {
  readonly event: IssueEvent;
  readonly index: number;
  readonly present: boolean;
}

export interface IssuesStoreOptions {
  runtime: AppRuntime;
  getGlobalRepo?: () => string | undefined;
  getGroupByRepo?: () => boolean;
  getPage?: () => string;
  supportsIssuePRReferences?: () => boolean;
  onDetailSynchronized?: () => void;
  sync?: {
    refreshSyncStatus?: () => void;
  };
}

function apiErrorMessage(error: { detail?: string; title?: string }, fallback: string): string {
  return error.detail ?? error.title ?? fallback;
}

function readErrorMessage(error: ApiProblemError | TransientTransportError): string {
  if (error._tag === "ApiProblemError") {
    return apiErrorMessage(error.problem, "failed to load issues");
  }
  return "Could not reach Kenn Forge";
}

const GITLAB_RESOURCE_BOT_USERNAME = /^(?:project|group)_\d+_bot_[a-z0-9]+$/;

function isBotAuthor(issue: Issue): boolean {
  const author = issue.Author.toLowerCase();
  if (author.endsWith("-bot")) return true;
  if (issue.repo.provider === "github") return author.endsWith("[bot]");
  if (issue.repo.provider === "gitlab") return GITLAB_RESOURCE_BOT_USERNAME.test(author);
  return false;
}

export function createIssuesStore(opts: IssuesStoreOptions) {
  const runtime = opts.runtime;
  const getGlobalRepo = opts.getGlobalRepo ?? (() => undefined);
  const getGroupByRepo = opts.getGroupByRepo ?? (() => false);
  const getPage = opts.getPage ?? (() => "");
  const supportsIssuePRReferences = opts.supportsIssuePRReferences ?? (() => false);
  const onDetailSynchronized = opts.onDetailSynchronized ?? (() => {});
  const syncDep = opts.sync;

  function refreshIssuesIfActive(): void {
    if (["issues", "mobile-issues", "focus"].includes(getPage())) {
      loadIssues();
    }
  }

  function reconcileListsAfterDetailSync(): void {
    refreshIssuesIfActive();
    onDetailSynchronized();
  }

  // --- list state ---

  let issues = $state<Issue[]>([]);
  let hideBots = $state(false);
  let confirmedHideBots = false;
  let hideBotsMutationGeneration = 0;
  let loading = $state(false);
  let listCapped = $state(false);
  let storeError = $state<string | null>(null);
  let filterStarred = $state(false);
  let involvesMe = $state(readInvolvesMeFilter("issues"));
  let unassigned = $state(readUnassignedFilter("issues"));
  let referencedByPR = $state(readIssuePRReferenceFilter());
  let filterState = $state<string>("open");
  let searchQuery = $state<string | undefined>(undefined);
  let selectedIssue = $state<IssueSelection | null>(null);
  let activeListParams: IssuesParams | undefined;

  // --- detail state ---

  // Detail envelopes are replaced immutably. Keeping the large event array raw
  // avoids proxying thousands of timeline objects that are never mutated in place.
  let issueDetail = $state.raw<IssueDetail | null>(null);
  // Lifecycle tick captured when the request that produced the current
  // envelope STARTED (not when it landed). Workspace-create reconciliation
  // compares it against a creation confirmation's tick to tell a stale
  // pre-create "no workspace" apart from an authoritative post-create one.
  let issueDetailEnvelopeTick = $state(0);
  let detailLoading = $state(false);
  let detailSyncing = $state(false);
  let detailError = $state<string | null>(null);
  let issueDetailLoaded = $state(false);
  // Tracks the issue (if any) whose local body has been edited since
  // the last server confirmation. While set, background sync paths
  // preserve the local body for THIS specific issue — a poll on a
  // different issue is unaffected, and navigating away doesn't
  // strand the flag on the wrong target.
  type UnsavedIssueTarget = {
    provider: string;
    platformHost: string | undefined;
    owner: string;
    name: string;
    number: number;
    body: string;
  };
  let unsavedLocalBody = $state<UnsavedIssueTarget | null>(null);
  let issueSyncGeneration = 0;
  let issuePollingGeneration = 0;
  // Provider synchronization is eventually complete. Keep a successfully
  // deleted comment hidden locally until an ordinary sync no longer returns it.
  const hiddenDeletedCommentIDs: Record<string, number[]> = {};
  const pendingCommentMutationStates = new Map<
    number,
    { readonly baseline: IssueCommentMutationState; readonly count: number }
  >();

  // --- list reads ---

  function getIssues(): Issue[] {
    if (!hideBots) return issues;
    return issues.filter((issue) => !isBotAuthor(issue));
  }

  function getHideBots(): boolean {
    return hideBots;
  }
  function isIssuesLoading(): boolean {
    return loading;
  }
  function isIssueListCapped(): boolean {
    return listCapped;
  }
  function getIssuesError(): string | null {
    return storeError;
  }
  function getSelectedIssue(): IssueSelection | null {
    return selectedIssue;
  }
  function getIssueFilterStarred(): boolean {
    return filterStarred;
  }
  function getInvolvesMe(): boolean {
    return involvesMe;
  }
  function getUnassigned(): boolean {
    return unassigned;
  }
  function getReferencedByPR(): boolean {
    return referencedByPR;
  }
  function canFilterReferencedByPR(): boolean {
    return supportsIssuePRReferences();
  }
  function getIssueSearchQuery(): string | undefined {
    return searchQuery;
  }

  function issuesByRepo(): Map<string, Issue[]> {
    const map = new Map<string, Issue[]>();
    for (const issue of getIssues()) {
      const key = issueIdentityKey(issueRef(issue));
      const existing = map.get(key);
      if (existing) existing.push(issue);
      else map.set(key, [issue]);
    }
    return map;
  }

  // --- detail reads ---

  function issueIdentityKey(ref: Pick<ProviderRouteRef, "provider" | "platformHost" | "repoPath">): string {
    return JSON.stringify([ref.provider, ref.platformHost ?? "", ref.repoPath]);
  }

  function issueRef(issue: Issue): ProviderRouteRef {
    return {
      provider: issue.repo.provider,
      platformHost: issue.repo.platform_host,
      owner: issue.repo.owner,
      name: issue.repo.name,
      repoPath: issue.repo.repo_path,
    };
  }

  function issueMatchesSelection(issue: Issue, sel: IssueSelection): boolean {
    return (
      issue.Number === sel.number &&
      issue.repo.provider === sel.provider &&
      issue.repo.platform_host === sel.platformHost &&
      issue.repo.repo_path === sel.repoPath &&
      issue.repo.owner === sel.owner &&
      issue.repo.name === sel.name
    );
  }

  function issueMatchesRef(issue: Issue, ref: ProviderRouteRef, number: number): boolean {
    return (
      issue.Number === number &&
      issue.repo.owner === ref.owner &&
      issue.repo.name === ref.name &&
      issue.repo.repo_path === ref.repoPath &&
      sameBodyTarget(issue.repo.provider, issue.repo.platform_host, ref.provider, ref.platformHost)
    );
  }

  function concretePlatformHost(ref: Pick<ProviderRouteRef, "provider" | "platformHost">): string {
    const host = ref.platformHost ?? providerDefaultHost(ref.provider);
    if (!host) throw new Error("issue missing platform host");
    return host;
  }

  function getIssueDetail(): IssueDetail | null {
    return issueDetail;
  }
  function getIssueDetailEnvelopeTick(): number {
    return issueDetailEnvelopeTick;
  }

  // Applies an envelope payload and its request-start tick atomically. A
  // response whose request started before the currently applied envelope's
  // request is stale: applying its payload while the newer tick stands
  // would let pre-creation "no workspace" data masquerade as authoritative.
  // Rejecting it keeps last-started-wins consistent across every
  // detail-producing path, including sync and PATCH responses.
  function applyEnvelopeAt(envelopeTick: number, assign: () => void): void {
    if (envelopeTick < issueDetailEnvelopeTick) return;
    assign();
    issueDetailEnvelopeTick = envelopeTick;
  }
  function isIssueDetailLoading(): boolean {
    return detailLoading;
  }
  function isIssueDetailSyncing(): boolean {
    return detailSyncing;
  }
  function getIssueDetailError(): string | null {
    return detailError;
  }

  function getIssueDetailLoaded(): boolean {
    return issueDetailLoaded;
  }

  function isIssueStaleRefreshing(): boolean {
    if (!issueDetail || !detailSyncing) return false;
    const fetchedAt = issueDetail.detail_fetched_at;
    if (!fetchedAt) return false;
    const fetchedMs = new Date(fetchedAt).getTime();
    const updatedMs = new Date(issueDetail.issue.UpdatedAt).getTime();
    const hourAgo = Date.now() - 3_600_000;
    return fetchedMs < hourAgo && updatedMs > fetchedMs;
  }

  // --- list writes ---

  function setIssueFilterStarred(v: boolean): void {
    filterStarred = v;
  }
  function setInvolvesMe(value: boolean): void {
    involvesMe = value;
    writeInvolvesMeFilter("issues", value);
  }
  function setUnassigned(value: boolean): void {
    unassigned = value;
    writeUnassignedFilter("issues", value);
  }
  function setReferencedByPR(value: boolean): void {
    referencedByPR = value;
    writeIssuePRReferenceFilter(value);
  }
  function setIssueSearchQuery(q: string | undefined): void {
    searchQuery = q;
  }
  function getIssueFilterState(): string {
    return filterState;
  }
  function setIssueFilterState(s: string): void {
    filterState = s;
  }

  function hydrateDefaults(settings: IssueSettings): void {
    hideBots = settings.hide_bots;
    confirmedHideBots = settings.hide_bots;
  }

  function setHideBots(value: boolean): void {
    hideBots = value;
    const generation = ++hideBotsMutationGeneration;
    const program = Effect.gen(function* () {
      const workflow = yield* SettingsWorkflow;
      const settings = yield* workflow.persist(() => ({ issues: { hide_bots: value } }));
      yield* Effect.sync(() => {
        confirmedHideBots = settings.issues.hide_bots;
        if (generation === hideBotsMutationGeneration) hideBots = confirmedHideBots;
      });
    });
    runtime.runCommand(program, {
      operation: "save issue visibility",
      safeContext: { hideBots: value },
      onFailure: (failure) => {
        if (generation !== hideBotsMutationGeneration) return;
        hideBots = confirmedHideBots;
        showFlash(settingsErrorMessage(failure), { tone: "danger" });
      },
    });
  }

  function selectIssue(
    owner: string,
    name: string,
    number: number,
    provider: string,
    platformHost: string | undefined,
    repoPath: string,
  ): void {
    selectedIssue = {
      provider,
      ...(platformHost && { platformHost }),
      owner,
      name,
      repoPath,
      number,
    };
  }
  function clearIssueSelection(): void {
    selectedIssue = null;
  }

  function loadIssuesEffect(params: IssuesParams | undefined = activeListParams) {
    const globalRepo = getGlobalRepo();
    const query: IssuesParams = {
      state: filterState,
      ...(globalRepo !== undefined && { repo: globalRepo }),
      ...(filterStarred && { starred: true }),
      ...(involvesMe && { involves_me: true }),
      ...(unassigned && { unassigned: true }),
      ...(referencedByPR && canFilterReferencedByPR() && { referenced_by_pr: true }),
      ...(searchQuery !== undefined && { q: searchQuery }),
      ...params,
    };
    const read = executeGeneratedApiRequest("GET /issues", (client, signal) =>
      client.IssuesService.listIssues(query, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.map((result) => result ?? []),
    );
    return Effect.sync(() => {
      loading = true;
      storeError = null;
    }).pipe(
      Effect.andThen(
        Effect.gen(function* () {
          const workflow = yield* IssuesWorkflow;
          return yield* workflow.list(read);
        }),
      ),
      Effect.tap((result) =>
        Effect.sync(() => {
          issues = result;
          listCapped = query.limit !== undefined && result.length === query.limit;
          loading = false;
        }),
      ),
      Effect.tapError((failure) =>
        Effect.sync(() => {
          storeError = readErrorMessage(failure);
          loading = false;
        }),
      ),
    );
  }

  function reconcileIssuesEffect(params: IssuesParams | undefined = activeListParams) {
    return Effect.suspend(() => {
      const globalRepo = getGlobalRepo();
      const query: IssuesParams = {
        state: filterState,
        ...(globalRepo !== undefined && { repo: globalRepo }),
        ...(filterStarred && { starred: true }),
        ...(involvesMe && { involves_me: true }),
        ...(unassigned && { unassigned: true }),
        ...(referencedByPR && canFilterReferencedByPR() && { referenced_by_pr: true }),
        ...(searchQuery !== undefined && { q: searchQuery }),
        ...params,
      };
      const read = executeGeneratedApiRequest("GET /issues after provider event", (client, signal) =>
        client.IssuesService.listIssues(query, { signal }),
      ).pipe(
        retryIdempotentRead,
        Effect.map((result) => result ?? []),
      );
      return Effect.gen(function* () {
        const workflow = yield* IssuesWorkflow;
        yield* workflow.reconcile(read, (result) =>
          Effect.sync(() => {
            issues = [...result];
            listCapped = query.limit !== undefined && result.length === query.limit;
          }),
        );
      });
    });
  }

  function loadIssues(params?: IssuesParams): void {
    activeListParams = params;
    runtime.runCommand(loadIssuesEffect(params), {
      operation: "load issues",
      safeContext: {},
      onFailure: (failure) => {
        storeError = readErrorMessage(failure);
        loading = false;
      },
    });
  }

  // --- detail writes ---

  // Apply a fresh IssueDetail from the server. When the user has an
  // unsynced local body edit on the same issue, keep that body so a
  // polling refresh can't revert a pending optimistic toggle. Match on
  // provider + platformHost too so an unrelated repo with the same
  // owner/name/number (different host or provider) doesn't inherit
  // another repo's pending body.
  function withPreservedLocalBody(next: IssueDetail): IssueDetail {
    next = withHiddenDeletedComments(next);
    if (!unsavedLocalBody) return next;
    if (!issueDetail) return next;
    if (
      !sameBodyTarget(
        unsavedLocalBody.provider,
        unsavedLocalBody.platformHost,
        next.repo?.provider,
        next.repo?.platform_host,
      ) ||
      unsavedLocalBody.owner !== next.repo_owner ||
      unsavedLocalBody.name !== next.repo_name ||
      unsavedLocalBody.number !== next.issue?.Number
    ) {
      return next;
    }
    if (
      issueDetail.repo_owner !== next.repo_owner ||
      issueDetail.repo_name !== next.repo_name ||
      issueDetail.issue?.Number !== next.issue?.Number
    ) {
      return next;
    }
    return {
      ...next,
      issue: { ...next.issue, Body: unsavedLocalBody.body },
    };
  }

  function withHiddenDeletedComments(next: IssueDetail): IssueDetail {
    if (Object.keys(hiddenDeletedCommentIDs).length === 0) return next;
    const key = `${next.repo.provider}:${next.repo.platform_host ?? ""}:${next.repo.repo_path}/${next.issue.Number}`;
    const hidden = hiddenDeletedCommentIDs[key];
    if (!hidden || hidden.length === 0) return next;
    const events = next.events ?? [];
    const stillHidden = hidden.filter((id) =>
      events.some((event) => event.EventType === "issue_comment" && event.PlatformID === id),
    );
    if (stillHidden.length === 0) {
      delete hiddenDeletedCommentIDs[key];
      return next;
    }
    hiddenDeletedCommentIDs[key] = stillHidden;
    return {
      ...next,
      events: events.filter(
        (event) =>
          event.EventType !== "issue_comment" || event.PlatformID === null || !stillHidden.includes(event.PlatformID),
      ),
    };
  }

  function hideDeletedComment(ref: IssueDetailRequestRef, commentID: number): void {
    const key = `${ref.provider}:${ref.platformHost ?? ""}:${ref.repoPath}/${ref.number}`;
    const hidden = hiddenDeletedCommentIDs[key] ?? [];
    if (!hidden.includes(commentID)) hiddenDeletedCommentIDs[key] = [...hidden, commentID];
    if (!isIssueDetailShowingRef(ref) || !issueDetail) return;
    issueDetail = {
      ...issueDetail,
      events: (issueDetail.events ?? []).filter(
        (event) => event.EventType !== "issue_comment" || event.PlatformID !== commentID,
      ),
    };
  }

  function hasUnsavedLocalBody(): boolean {
    return unsavedLocalBody !== null;
  }

  // Callers pass route vocabulary (gh, omitted default host) while
  // payloads carry canonical values; every provider/host comparison
  // between the two must normalize both sides or a mutation on an
  // aliased route silently no-ops against its own current detail.
  function sameBodyTarget(
    aProvider: string | undefined,
    aHost: string | undefined,
    bProvider: string | undefined,
    bHost: string | undefined,
  ): boolean {
    const a = canonicalProvider(aProvider ?? "");
    const b = canonicalProvider(bProvider ?? "");
    return a === b && resolvedPlatformHost(a, aHost) === resolvedPlatformHost(b, bHost);
  }

  function isIssueDetailShowingRef(ref: IssueDetailRequestRef): boolean {
    return (
      issueDetail !== null &&
      issueDetail.repo_owner === ref.owner &&
      issueDetail.repo_name === ref.name &&
      issueDetail.issue.Number === ref.number &&
      sameBodyTarget(issueDetail.repo?.provider, issueDetail.repo?.platform_host, ref.provider, ref.platformHost) &&
      issueDetail.repo?.repo_path === ref.repoPath
    );
  }

  function currentIssueDetailRef(owner: string, name: string, number: number): IssueDetailRequestRef {
    const provider = issueDetail?.repo?.provider ?? selectedIssue?.provider;
    const repoPath = issueDetail?.repo?.repo_path ?? selectedIssue?.repoPath;
    if (!provider || !repoPath) {
      throw new Error("issue detail missing provider repo identity");
    }
    return issueDetailRequestRef(owner, name, number, {
      provider,
      platformHost: issueDetail?.repo?.platform_host ?? selectedIssue?.platformHost,
      repoPath,
    });
  }

  function issueDetailRequestRef(
    owner: string,
    name: string,
    number: number,
    options: IssueDetailRequestOptions,
  ): IssueDetailRequestRef {
    return {
      owner,
      name,
      number,
      provider: options.provider,
      platformHost: options.platformHost,
      repoPath: options.repoPath,
    };
  }

  function issueMutationKey(ref: IssueDetailRequestRef, family: string): string {
    return `issue\u0000${providerItemKey({
      provider: ref.provider,
      platformHost: concretePlatformHost(ref),
      owner: ref.owner,
      name: ref.name,
      number: ref.number,
    })}\u0000${family}`;
  }

  function issueCommentState(
    events: ReadonlyArray<IssueEvent>,
    commentID: number,
    fallback: IssueCommentMutationState,
  ): IssueCommentMutationState {
    const index = events.findIndex((event) => event.EventType === "issue_comment" && event.PlatformID === commentID);
    const event = events[index];
    return index === -1 || event === undefined ? { ...fallback, present: false } : { event, index, present: true };
  }

  function applyIssueCommentState(
    ref: IssueDetailRequestRef,
    commentID: number,
    state: IssueCommentMutationState,
  ): Effect.Effect<void> {
    return Effect.sync(() => {
      if (!isIssueDetailShowingRef(ref) || issueDetail === null) return;
      const events = [...(issueDetail.events ?? [])];
      const currentIndex = events.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      if (!state.present) {
        if (currentIndex !== -1) events.splice(currentIndex, 1);
      } else if (currentIndex === -1) {
        events.splice(Math.min(state.index, events.length), 0, state.event);
      } else {
        events[currentIndex] = state.event;
      }
      issueDetail = { ...issueDetail, events };
    });
  }

  function trackIssueCommentMutation(commentID: number, baseline: IssueCommentMutationState): void {
    const current = pendingCommentMutationStates.get(commentID);
    pendingCommentMutationStates.set(commentID, {
      baseline: current?.baseline ?? baseline,
      count: (current?.count ?? 0) + 1,
    });
  }

  function releaseIssueCommentMutation(commentID: number): void {
    const current = pendingCommentMutationStates.get(commentID);
    if (current === undefined || current.count === 1) {
      pendingCommentMutationStates.delete(commentID);
      return;
    }
    pendingCommentMutationStates.set(commentID, { ...current, count: current.count - 1 });
  }

  function rebaseIssueMutations(
    ref: IssueDetailRequestRef,
    authoritative: IssueDetail,
    installEnvelope: () => boolean,
  ) {
    return Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const labels = authoritative.issue.labels ?? [];
      const entries: Array<{ readonly key: string; readonly confirmed: Effect.Effect<void> }> = [
        {
          key: issueMutationKey(ref, "body"),
          confirmed: applyIssueBody(ref, authoritative.issue.Body),
        },
        {
          key: issueMutationKey(ref, "star"),
          confirmed: applyIssueStar(ref, Boolean(authoritative.issue.Starred), 0),
        },
        {
          key: issueMutationKey(ref, "labels"),
          confirmed: Effect.sync(() => {
            if (isIssueDetailShowingRef(ref) && issueDetail !== null) {
              issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, labels } };
            }
          }),
        },
      ];
      const assignees = authoritative.issue.assignees ?? [];
      entries.push({
        key: issueMutationKey(ref, "assignees"),
        confirmed: Effect.sync(() => {
          if (isIssueDetailShowingRef(ref) && issueDetail !== null) {
            issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, assignees } };
          }
        }),
      });
      for (const [commentID, tracked] of pendingCommentMutationStates) {
        const state = issueCommentState(authoritative.events ?? [], commentID, tracked.baseline);
        entries.push({
          key: issueMutationKey(ref, `comment\u0000${commentID}`),
          confirmed: applyIssueCommentState(ref, commentID, state),
        });
      }
      return yield* mutations.rebaseAll(Effect.sync(installEnvelope), entries);
    });
  }

  function applyIssueBody(ref: IssueDetailRequestRef, body: string): Effect.Effect<void> {
    return Effect.sync(() => {
      if (!isIssueDetailShowingRef(ref) || issueDetail === null) return;
      const projectedBody =
        unsavedLocalBody !== null &&
        sameBodyTarget(unsavedLocalBody.provider, unsavedLocalBody.platformHost, ref.provider, ref.platformHost) &&
        unsavedLocalBody.owner === ref.owner &&
        unsavedLocalBody.name === ref.name &&
        unsavedLocalBody.number === ref.number &&
        unsavedLocalBody.body !== body
          ? unsavedLocalBody.body
          : body;
      issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, Body: projectedBody } };
    });
  }

  function applyIssueStar(ref: IssueDetailRequestRef, starred: boolean, envelopeTick: number): Effect.Effect<void> {
    return Effect.sync(() => {
      issues = issues.map((issue) =>
        issueMatchesRef(issue, ref, ref.number) ? { ...issue, Starred: starred } : issue,
      );
      if (isIssueDetailShowingRef(ref) && issueDetail !== null) {
        issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, Starred: starred } };
        issueDetailEnvelopeTick = Math.max(issueDetailEnvelopeTick, envelopeTick);
      }
    });
  }

  function applyIssueState(ref: IssueDetailRequestRef, state: string): Effect.Effect<void> {
    return Effect.sync(() => {
      issues = issues.map((issue) => (issueMatchesRef(issue, ref, ref.number) ? { ...issue, State: state } : issue));
      if (isIssueDetailShowingRef(ref) && issueDetail !== null) {
        issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, State: state } };
      }
    });
  }

  function issueDetailKey(ref: IssueDetailRequestRef): string {
    return providerItemKey({
      provider: ref.provider,
      platformHost: concretePlatformHost(ref),
      owner: ref.owner,
      name: ref.name,
      number: ref.number,
    });
  }

  function readIssueDetail(ref: IssueDetailRequestRef, operation: string) {
    return executeGeneratedApiRequest(operation, (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.IssuesService.getIssueOnHost({ ...providerHostRouteParams(ref), number: ref.number }, { signal })
        : client.IssuesService.getIssue({ ...providerRouteParams(ref), number: ref.number }, { signal }),
    ).pipe(Effect.map((data): IssueDetail => ({ ...data, events: data.events ?? [] })));
  }

  function installIssueDetail(
    ref: IssueDetailRequestRef,
    authoritative: IssueDetail,
    envelopeTick: number,
    expectedGeneration: number,
    requireVisible: boolean,
  ) {
    const next = withPreservedLocalBody(authoritative);
    return rebaseIssueMutations(ref, authoritative, () => {
      if (expectedGeneration !== issueSyncGeneration) return false;
      if (requireVisible && !isIssueDetailShowingRef(ref)) return false;
      let applied = false;
      applyEnvelopeAt(envelopeTick, () => {
        issueDetail = next;
        issueDetailLoaded = authoritative.detail_loaded ?? issueDetailLoaded;
        applied = true;
      });
      return applied;
    });
  }

  function refreshIssueDetailProgram(ref: IssueDetailRequestRef, expectedGeneration: number) {
    const envelopeTick = nextWorkspaceLifecycleTick();
    return readIssueDetail(ref, "GET issue detail refresh").pipe(
      Effect.flatMap((authoritative) =>
        installIssueDetail(ref, authoritative, envelopeTick, expectedGeneration, true).pipe(
          Effect.map((applied) => ({
            applied,
            ...(authoritative.detail_fetched_at != null && { fetchedAt: authoritative.detail_fetched_at }),
          })),
        ),
      ),
    );
  }

  function synchronizeIssueDetailEffect(ref: IssueDetailRequestRef, expectedGeneration: number) {
    const envelopeTick = nextWorkspaceLifecycleTick();
    const sync = executeGeneratedApiRequest("POST issue detail sync", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: ref.number }, { signal })
        : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: ref.number }, { signal }),
    ).pipe(
      Effect.map((data): IssueDetail => ({ ...data, events: data.events ?? [] })),
      Effect.tap(() => Effect.sync(reconcileListsAfterDetailSync)),
      Effect.flatMap((authoritative) =>
        installIssueDetail(ref, authoritative, envelopeTick, expectedGeneration, false),
      ),
      Effect.tap((applied) =>
        Effect.sync(() => {
          if (applied) {
            detailError = null;
          }
        }),
      ),
    );
    return Effect.sync(() => {
      if (expectedGeneration === issueSyncGeneration) detailSyncing = true;
    }).pipe(
      Effect.andThen(sync),
      Effect.ensuring(
        Effect.sync(() => {
          if (expectedGeneration === issueSyncGeneration) {
            detailSyncing = false;
          }
          syncDep?.refreshSyncStatus?.();
        }),
      ),
    );
  }

  function syncIssueDetailEffect(ref: IssueDetailRequestRef, expectedGeneration: number) {
    return synchronizeIssueDetailEffect(ref, expectedGeneration).pipe(Effect.catch(() => Effect.succeed(false)));
  }

  function syncIssueDetailNow(
    owner: string,
    name: string,
    number: number,
    identity: IssueDetailRequestOptions,
    callbacks: MutationCallbacks = {},
  ): void {
    const ref = issueDetailRequestRef(owner, name, number, identity);
    const generation = ++issueSyncGeneration;
    const program = synchronizeIssueDetailEffect(ref, generation).pipe(
      Effect.tap(() => invokeMutationCallback(callbacks.onSuccess)),
      Effect.ensuring(invokeMutationCallback(callbacks.onSettled)),
    );
    runtime.runCommand(program, {
      operation: "synchronize issue detail",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      },
      onFailure: (failure) => invokeMutationFailure(callbacks.onFailure, readErrorMessage(failure)),
    });
  }

  function enqueueBackgroundIssueSyncEffect(
    ref: IssueDetailRequestRef,
    expectedGeneration: number,
    previousFetchedAt: string | undefined,
  ) {
    const refreshUntilFresh = Effect.gen(function* () {
      for (const delay of [300, 700, 1_500, 3_000, 5_000]) {
        yield* Effect.sleep(delay);
        const result: IssueDetailRefreshResult = yield* refreshIssueDetailProgram(ref, expectedGeneration).pipe(
          Effect.catch(() => Effect.succeed({ applied: false })),
        );
        if (result.fetchedAt && result.fetchedAt !== previousFetchedAt) {
          reconcileListsAfterDetailSync();
          return;
        }
      }
    });
    const sync = executeGeneratedApiRequest("POST async issue detail sync", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.IssuesService.enqueueIssueSyncOnHost(
            { ...providerHostRouteParams(ref), number: ref.number },
            { signal },
          )
        : client.IssuesService.enqueueIssueSync({ ...providerRouteParams(ref), number: ref.number }, { signal }),
    ).pipe(
      Effect.andThen(refreshUntilFresh),
      Effect.catch(() => Effect.void),
    );
    return Effect.sync(() => {
      if (expectedGeneration === issueSyncGeneration) detailSyncing = true;
    }).pipe(
      Effect.andThen(sync),
      Effect.ensuring(
        Effect.sync(() => {
          if (expectedGeneration === issueSyncGeneration) detailSyncing = false;
          syncDep?.refreshSyncStatus?.();
        }),
      ),
    );
  }

  function loadIssueDetail(owner: string, name: string, number: number, options: IssueDetailRequestOptions): void {
    const ref = issueDetailRequestRef(owner, name, number, options);
    const syncMode = options.sync ?? true;
    const generation = ++issueSyncGeneration;
    const envelopeTick = nextWorkspaceLifecycleTick();
    detailLoading = true;
    detailSyncing = false;
    detailError = null;
    const program = Effect.gen(function* () {
      const workflow = yield* IssuesWorkflow;
      const result = yield* workflow.detail(issueDetailKey(ref), syncMode, readIssueDetail(ref, "GET issue detail"));
      reconcileListsAfterDetailSync();
      const applied = yield* installIssueDetail(ref, result.detail, envelopeTick, generation, false);
      if (generation === issueSyncGeneration) detailLoading = false;
      if (!applied || generation !== issueSyncGeneration) return;
      if (result.syncMode === true) {
        yield* syncIssueDetailEffect(ref, generation);
      } else if (result.syncMode === "background") {
        yield* enqueueBackgroundIssueSyncEffect(ref, generation, issueDetail?.detail_fetched_at);
      }
    }).pipe(
      Effect.ensuring(
        Effect.sync(() => {
          if (generation === issueSyncGeneration) detailLoading = false;
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "load issue detail",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      },
      onFailure: (failure) => {
        if (generation !== issueSyncGeneration) return;
        detailError = readErrorMessage(failure);
        detailLoading = false;
      },
    });
  }

  function startIssueDetailPolling(
    owner: string,
    name: string,
    number: number,
    options: IssueDetailRequestOptions,
  ): void {
    const ref = issueDetailRequestRef(owner, name, number, options);
    const pollingGeneration = ++issuePollingGeneration;
    const pollOnce = Effect.suspend(() =>
      detailSyncing ? Effect.void : refreshIssueDetailProgram(ref, issueSyncGeneration).pipe(Effect.asVoid),
    ).pipe(Effect.catch(() => Effect.void));
    const program = Effect.gen(function* () {
      const workflow = yield* IssuesWorkflow;
      yield* workflow.poll(pollingGeneration, pollOnce, "60 seconds");
    });
    runtime.runCommand(program, {
      operation: "poll issue detail",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      },
      onFailure: () => {},
    });
  }

  function stopIssueDetailPolling(): void {
    const pollingGeneration = ++issuePollingGeneration;
    const program = Effect.gen(function* () {
      const workflow = yield* IssuesWorkflow;
      yield* workflow.stopPolling(pollingGeneration);
    });
    runtime.runCommand(program, {
      operation: "stop issue detail polling",
      safeContext: {},
      onFailure: () => {},
    });
  }

  function clearIssueDetail(): void {
    ++issueSyncGeneration;
    issueDetail = null;
    detailLoading = false;
    detailSyncing = false;
    detailError = null;
    issueDetailLoaded = false;
    unsavedLocalBody = null;
    stopIssueDetailPolling();
  }

  function refreshIssueDetailEffect(
    owner: string,
    name: string,
    number: number,
    ref: IssueDetailRequestRef,
  ): Effect.Effect<void, ApiProblemError | TransientTransportError, GeneratedApi | ProviderMutations> {
    return Effect.suspend(() => {
      const expectedGeneration = issueSyncGeneration;
      const envelopeTick = nextWorkspaceLifecycleTick();
      const ownership = (): "current" | "irrelevant" | "superseded" => {
        if (!isIssueDetailShowingRef(ref)) return "irrelevant";
        return expectedGeneration === issueSyncGeneration ? "current" : "superseded";
      };
      const superseded = () =>
        TransientTransportError.make({
          operation: "reconcile issue detail after superseded provider event",
          cause: new Error("a foreground issue detail read replaced event reconciliation"),
        });
      const read = executeGeneratedApiRequest("GET issue detail after provider event", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.getIssueOnHost({ ...providerHostRouteParams(ref), number: ref.number }, { signal })
          : client.IssuesService.getIssue({ ...providerRouteParams(ref), number: ref.number }, { signal }),
      );
      return read.pipe(
        Effect.matchEffect({
          onFailure: (failure) =>
            Effect.sync(ownership).pipe(
              Effect.flatMap((status) =>
                status === "current"
                  ? Effect.fail(failure)
                  : status === "superseded"
                    ? Effect.fail(superseded())
                    : Effect.void,
              ),
            ),
          onSuccess: (data) =>
            Effect.gen(function* () {
              const status = ownership();
              if (status === "irrelevant") return;
              if (status === "superseded") return yield* Effect.fail(superseded());
              const next: IssueDetail = { ...data, events: data.events ?? [] };
              let supersededWhileInstalling = false;
              yield* rebaseIssueMutations(ref, next, () => {
                const installStatus = ownership();
                if (installStatus === "irrelevant") return false;
                if (installStatus === "superseded") {
                  supersededWhileInstalling = true;
                  return false;
                }
                let applied = false;
                applyEnvelopeAt(envelopeTick, () => {
                  issueDetail = withPreservedLocalBody(next);
                  issueDetailLoaded = data.detail_loaded ?? issueDetailLoaded;
                  applied = true;
                });
                return applied;
              });
              if (supersededWhileInstalling) return yield* Effect.fail(superseded());
            }),
        }),
      );
    }).pipe(Effect.asVoid);
  }

  function setIssueLabels(
    owner: string,
    name: string,
    number: number,
    labels: Label[],
    callbacks: MutationCallbacks = {},
  ): void {
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentIssueDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "update issue labels", cause }),
      });
      const key = `issue\u0000${providerItemKey({
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      })}\u0000labels`;
      const previousLabels = issueDetail?.issue.labels ?? [];
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PUT issue labels", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.setIssueLabelsOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { labels: labels.map((label) => label.name) },
              { signal },
            )
          : client.IssuesService.setIssueLabels(
              { ...providerRouteParams(ref), number: number },
              { labels: labels.map((label) => label.name) },
              { signal },
            ),
      ).pipe(Effect.map((response) => response.labels ?? []));
      const refreshOnStale = executeGeneratedApiRequest("sync issue after stale label mutation", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
          : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.issue.labels ?? []));
      const apply = (nextLabels: Label[]) =>
        Effect.sync(() => {
          if (isIssueDetailShowingRef(ref) && issueDetail) {
            issueDetail = { ...issueDetail, issue: { ...issueDetail.issue, labels: nextLabels } };
          }
        });
      yield* mutations.submit({
        key,
        baseline: previousLabels,
        optimistic: labels,
        apply,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(refreshIssuesIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));

    runtime.runCommand(program, {
      operation: "update issue labels",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update labels");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function setIssueAssignees(
    owner: string,
    name: string,
    number: number,
    assignees: string[],
    callbacks: MutationCallbacks = {},
  ): void {
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentIssueDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "update issue assignees", cause }),
      });
      const key = `issue\u0000${providerItemKey({
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner,
        name,
        number,
      })}\u0000assignees`;
      const previousAssignees = issueDetail?.issue.assignees ?? [];
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PUT issue assignees", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.setIssueAssigneesOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { assignees },
              { signal },
            )
          : client.IssuesService.setIssueAssignees(
              { ...providerRouteParams(ref), number: number },
              { assignees },
              { signal },
            ),
      ).pipe(Effect.map((response) => response.assignees ?? []));
      const refreshOnStale = executeGeneratedApiRequest("sync issue after stale assignee mutation", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
          : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => response.issue.assignees ?? []));
      const apply = (nextAssignees: string[]) =>
        Effect.sync(() => {
          if (isIssueDetailShowingRef(ref) && issueDetail) {
            issueDetail = {
              ...issueDetail,
              issue: { ...issueDetail.issue, assignees: nextAssignees },
            };
          }
        });
      yield* mutations.submit({
        key,
        baseline: previousAssignees,
        optimistic: assignees,
        apply,
        commit,
        refreshOnStale,
      });
      yield* Effect.sync(refreshIssuesIfActive);
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));

    runtime.runCommand(program, {
      operation: "update issue assignees",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        const message = providerMutationFailureMessage(failure, "failed to update assignees");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function submitIssueComment(
    owner: string,
    name: string,
    number: number,
    body: string,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentIssueDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "post issue comment", cause }),
      });
      const key = issueMutationKey(ref, "comment-posts");
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("POST issue comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.postIssueCommentOnHost(
              { ...providerHostRouteParams(ref), number: number },
              { body },
              { signal },
            )
          : client.IssuesService.postIssueComment(
              { ...providerRouteParams(ref), number: number },
              { body },
              { signal },
            ),
      ).pipe(Effect.asVoid);
      yield* mutations.submit({
        key,
        baseline: undefined,
        optimistic: undefined,
        apply: () => Effect.void,
        commit,
        refreshOnStale: executeGeneratedApiRequest("sync issue after stale comment submission", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(Effect.asVoid),
      });
      const gen = yield* Effect.sync(() => {
        mutationSettled = true;
        if (!isIssueDetailShowingRef(ref)) return undefined;
        detailSyncing = false;
        return ++issueSyncGeneration;
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (gen === undefined) return;
      yield* refreshIssueDetailEffect(owner, name, number, ref);
      if (gen === issueSyncGeneration) {
        yield* syncIssueDetailEffect(ref, gen);
      }
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "post issue comment",
      safeContext: { owner, name, number },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was posted, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to post comment");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function editIssueComment(
    owner: string,
    name: string,
    number: number,
    commentID: number,
    body: string,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentIssueDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "edit issue comment", cause }),
      });
      const previousEvents = issueDetail?.events ?? [];
      const previousIndex = previousEvents.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      const previousEvent = previousEvents[previousIndex];
      if (previousIndex === -1 || previousEvent === undefined) {
        return yield* Effect.fail(
          TransientTransportError.make({
            operation: "edit issue comment",
            cause: new Error("comment is no longer present in the selected issue"),
          }),
        );
      }
      const key = issueMutationKey(ref, `comment\u0000${commentID}`);
      const baseline: IssueCommentMutationState = { event: previousEvent, index: previousIndex, present: true };
      const optimistic: IssueCommentMutationState = { ...baseline, event: { ...previousEvent, Body: body } };
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("PATCH issue comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.editIssueCommentOnHost(
              { ...providerHostRouteParams(ref), number: number, commentId: commentID },
              { body },
              { signal },
            )
          : client.IssuesService.editIssueComment(
              { ...providerRouteParams(ref), number: number, commentId: commentID },
              { body },
              { signal },
            ),
      ).pipe(Effect.as(optimistic));
      const refreshOnStale = executeGeneratedApiRequest("sync issue after stale comment edit", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
          : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => issueCommentState(response.events ?? [], commentID, baseline)));
      const apply = (state: IssueCommentMutationState) => applyIssueCommentState(ref, commentID, state);
      yield* Effect.sync(() => trackIssueCommentMutation(commentID, baseline));
      yield* mutations
        .submit({
          key,
          baseline,
          optimistic,
          apply,
          commit,
          refreshOnStale,
        })
        .pipe(Effect.ensuring(Effect.sync(() => releaseIssueCommentMutation(commentID))));
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isIssueDetailShowingRef(ref);
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) yield* refreshIssueDetailEffect(owner, name, number, ref);
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "edit issue comment",
      safeContext: { owner, name, number, commentID },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was edited, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to edit comment");
        showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  function deleteIssueComment(
    owner: string,
    name: string,
    number: number,
    commentID: number,
    callbacks: MutationCallbacks = {},
  ): void {
    let mutationSettled = false;
    let mutationRef: IssueDetailRequestRef | undefined;
    const program = Effect.gen(function* () {
      const ref = yield* Effect.try({
        try: () => currentIssueDetailRef(owner, name, number),
        catch: (cause) => TransientTransportError.make({ operation: "delete issue comment", cause }),
      });
      mutationRef = ref;
      const previousEvents = issueDetail?.events ?? [];
      const commentIndex = previousEvents.findIndex(
        (event) => event.EventType === "issue_comment" && event.PlatformID === commentID,
      );
      const previousEvent = previousEvents[commentIndex];
      if (commentIndex === -1 || previousEvent === undefined) {
        return yield* Effect.fail(
          TransientTransportError.make({
            operation: "delete issue comment",
            cause: new Error("comment is no longer present in the selected issue"),
          }),
        );
      }
      const key = issueMutationKey(ref, `comment\u0000${commentID}`);
      const baseline: IssueCommentMutationState = { event: previousEvent, index: commentIndex, present: true };
      const optimistic: IssueCommentMutationState = { ...baseline, present: false };
      const mutations = yield* ProviderMutations;
      const commit = executeGeneratedApiRequest("DELETE issue comment", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.deleteIssueCommentOnHost(
              { ...providerHostRouteParams(ref), number: number, commentId: commentID },
              { headers: { "Content-Type": "application/json" }, signal },
            )
          : client.IssuesService.deleteIssueComment(
              { ...providerRouteParams(ref), number: number, commentId: commentID },
              { headers: { "Content-Type": "application/json" }, signal },
            ),
      ).pipe(Effect.as(optimistic));
      const refreshOnStale = executeGeneratedApiRequest("sync issue after stale comment deletion", (client, signal) =>
        providerUsesHostRoute(ref)
          ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
          : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
      ).pipe(Effect.map((response) => issueCommentState(response.events ?? [], commentID, baseline)));
      const apply = (state: IssueCommentMutationState) => applyIssueCommentState(ref, commentID, state);
      yield* Effect.sync(() => {
        detailError = null;
        trackIssueCommentMutation(commentID, baseline);
      });
      yield* mutations
        .submit({
          key,
          baseline,
          optimistic,
          apply,
          commit,
          refreshOnStale,
        })
        .pipe(Effect.ensuring(Effect.sync(() => releaseIssueCommentMutation(commentID))));
      yield* Effect.sync(() => hideDeletedComment(ref, commentID));
      const shouldReconcile = yield* Effect.sync(() => {
        mutationSettled = true;
        return isIssueDetailShowingRef(ref);
      });
      yield* invokeMutationCallback(callbacks.onSuccess);
      yield* invokeMutationCallback(callbacks.onSettled);
      if (shouldReconcile) {
        const reconciled = yield* syncIssueDetailEffect(ref, issueSyncGeneration);
        if (!reconciled && isIssueDetailShowingRef(ref)) {
          return yield* Effect.fail(
            TransientTransportError.make({
              operation: "refresh issue after comment deletion",
              cause: new Error("issue synchronization did not return detail"),
            }),
          );
        }
      }
    }).pipe(
      Effect.ensuring(
        Effect.gen(function* () {
          if (!mutationSettled) yield* invokeMutationCallback(callbacks.onSettled);
        }),
      ),
    );

    runtime.runCommand(program, {
      operation: "delete issue comment",
      safeContext: { owner, name, number, commentID },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("Comment was deleted, but the latest discussion could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to delete comment");
        if (mutationRef !== undefined && isIssueDetailShowingRef(mutationRef)) detailError = message;
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  // Replaces the in-memory issue body without touching the server. Pair
  // with saveIssueBodyInBackground for instant-feedback edits like
  // task-list checkbox clicks. Marks the body as unsaved so a
  // background refresh can't revert it before the debounced PATCH
  // lands.
  function setLocalIssueBody(
    provider: string,
    platformHost: string | undefined,
    owner: string,
    name: string,
    number: number,
    body: string,
  ): void {
    if (!issueDetail) return;
    if (
      !sameBodyTarget(issueDetail.repo?.provider, issueDetail.repo?.platform_host, provider, platformHost) ||
      issueDetail.repo_owner !== owner ||
      issueDetail.repo_name !== name ||
      issueDetail.issue.Number !== number
    ) {
      return;
    }
    unsavedLocalBody = { provider, platformHost, owner, name, number, body };
    issueDetail = {
      ...issueDetail,
      issue: { ...issueDetail.issue, Body: body },
    };
  }

  function issueBodyUpdate(
    owner: string,
    name: string,
    number: number,
    body: string,
    routeRef: {
      provider: string;
      platformHost?: string | undefined;
      repoPath: string;
    },
  ): {
    readonly ref: IssueDetailRequestRef;
    readonly program: Effect.Effect<void, ProviderMutationFailure, ProviderMutations>;
  } {
    const ref = issueDetailRequestRef(owner, name, number, routeRef);
    const baseline = isIssueDetailShowingRef(ref) && issueDetail !== null ? issueDetail.issue.Body : body;
    let confirmed: { readonly detail: IssueDetail; readonly envelopeTick: number } | undefined;
    let acknowledgedUnsavedTarget: UnsavedIssueTarget | null = null;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const commit = Effect.suspend(() => {
        const envelopeTick = nextWorkspaceLifecycleTick();
        return executeGeneratedApiRequest("PATCH issue body", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.IssuesService.editIssueContentOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { body },
                { signal },
              )
            : client.IssuesService.editIssueContent(
                { ...providerRouteParams(ref), number: number },
                { body },
                { signal },
              ),
        ).pipe(
          Effect.map((detail): IssueDetail => ({ ...detail, events: detail.events ?? [] })),
          Effect.tap((detail) =>
            Effect.sync(() => {
              confirmed = { detail, envelopeTick };
              if (
                unsavedLocalBody !== null &&
                unsavedLocalBody.body === body &&
                sameBodyTarget(
                  unsavedLocalBody.provider,
                  unsavedLocalBody.platformHost,
                  ref.provider,
                  ref.platformHost,
                ) &&
                unsavedLocalBody.owner === owner &&
                unsavedLocalBody.name === name &&
                unsavedLocalBody.number === number
              ) {
                acknowledgedUnsavedTarget = unsavedLocalBody;
              }
            }),
          ),
          Effect.map((detail) => detail.issue.Body),
        );
      });
      const refreshOnStale = Effect.suspend(() => {
        const envelopeTick = nextWorkspaceLifecycleTick();
        return executeGeneratedApiRequest("sync issue after stale body mutation", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(
          Effect.map((detail): IssueDetail => ({ ...detail, events: detail.events ?? [] })),
          Effect.tap((detail) =>
            rebaseIssueMutations(ref, detail, () => {
              if (!isIssueDetailShowingRef(ref)) return false;
              let applied = false;
              applyEnvelopeAt(envelopeTick, () => {
                issueDetail = withPreservedLocalBody(detail);
                issueDetailLoaded = detail.detail_loaded ?? issueDetailLoaded;
                applied = true;
              });
              return applied;
            }),
          ),
          Effect.map((detail) => detail.issue.Body),
        );
      }).pipe(Effect.provideService(ProviderMutations, mutations));
      yield* mutations.submit({
        key: issueMutationKey(ref, "body"),
        baseline,
        optimistic: body,
        apply: (nextBody) => applyIssueBody(ref, nextBody),
        commit,
        refreshOnStale,
      });
      if (acknowledgedUnsavedTarget !== null && unsavedLocalBody === acknowledgedUnsavedTarget) {
        unsavedLocalBody = null;
      }
      const response = confirmed;
      if (response !== undefined) {
        yield* rebaseIssueMutations(ref, response.detail, () => {
          if (!isIssueDetailShowingRef(ref)) return false;
          let applied = false;
          applyEnvelopeAt(response.envelopeTick, () => {
            issueDetail = withPreservedLocalBody(response.detail);
            issueDetailLoaded = response.detail.detail_loaded ?? issueDetailLoaded;
            applied = true;
          });
          return applied;
        });
      }
      refreshIssuesIfActive();
    });
    return { ref, program };
  }

  // Fire-and-forget PATCH for the issue body. Does NOT apply an
  // optimistic update or revert on failure — the caller already owns
  // local state. On error, the shared flash reports the failed save.
  //
  // The caller passes the full route ref so the PATCH always targets
  // the captured issue even if the user has since navigated away.
  // Only the response is gated on the currently displayed detail.
  // Saves for the same issue are serialized so older requests can't
  // overwrite newer bodies via out-of-order responses.
  function saveIssueBodyInBackground(
    owner: string,
    name: string,
    number: number,
    body: string,
    routeRef: {
      provider: string;
      platformHost?: string | undefined;
      repoPath: string;
    },
  ): void {
    const update = issueBodyUpdate(owner, name, number, body, routeRef);
    const program = Effect.gen(function* () {
      const workflow = yield* IssuesWorkflow;
      const mutations = yield* ProviderMutations;
      yield* workflow.submitLatestWrite(
        issueDetailKey(update.ref),
        update.program.pipe(Effect.provideService(ProviderMutations, mutations)),
      );
    });
    runtime.runCommand(program, {
      operation: "save issue body in background",
      safeContext: {
        provider: update.ref.provider,
        platformHost: concretePlatformHost(update.ref),
        owner,
        name,
        number,
      },
      onFailure: (failure) => {
        showFlash(providerMutationFailureMessage(failure, "failed to update issue body"), { tone: "danger" });
      },
    });
  }

  function toggleIssueStar(ref: ProviderRouteRef, number: number, currentlyStarred: boolean): void {
    const platformHost = concretePlatformHost(ref);
    const detailRef = issueDetailRequestRef(ref.owner, ref.name, number, ref);
    const starredItem: UnsetStarredParams = {
      item_type: "issue",
      provider: ref.provider,
      platform_host: platformHost,
      owner: ref.owner,
      name: ref.name,
      number,
    };
    const baseline =
      issues.find((issue) => issueMatchesRef(issue, ref, number))?.Starred ??
      (isIssueDetailShowingRef(detailRef) ? issueDetail?.issue.Starred : undefined) ??
      currentlyStarred;
    const nextStarred = !currentlyStarred;
    let mutationTick = 0;
    let mutationSettled = false;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      mutationTick = nextWorkspaceLifecycleTick();
      const commit = (
        currentlyStarred
          ? executeGeneratedApiRequest<void>("DELETE issue star", (client, signal) =>
              client.SettingsService.unsetStarred(starredItem, { signal }),
            )
          : executeGeneratedApiRequest<void>("PUT issue star", (client, signal) =>
              client.SettingsService.setStarred(starredItem, { signal }),
            )
      ).pipe(Effect.as(nextStarred));
      const refreshOnStale = readIssueDetail(detailRef, "GET issue after stale star mutation").pipe(
        Effect.map((detail) => Boolean(detail.issue.Starred)),
      );
      yield* mutations.submit({
        key: issueMutationKey(detailRef, "star"),
        baseline: Boolean(baseline),
        optimistic: nextStarred,
        apply: (starred) => applyIssueStar(detailRef, starred, mutationTick),
        commit,
        refreshOnStale,
      });
      mutationSettled = true;
      yield* loadIssuesEffect();
      if (isIssueDetailShowingRef(detailRef)) {
        yield* refreshIssueDetailProgram(detailRef, issueSyncGeneration).pipe(
          Effect.catch(() => Effect.succeed(false)),
        );
      }
    });
    runtime.runCommand(program, {
      operation: currentlyStarred ? "unstar issue" : "star issue",
      safeContext: { provider: ref.provider, platformHost, owner: ref.owner, name: ref.name, number },
      onFailure: (failure) => {
        if (mutationSettled) {
          storeError = providerMutationFailureMessage(failure, "failed to refresh issues");
          return;
        }
        showFlash(
          providerMutationFailureMessage(failure, currentlyStarred ? "failed to unstar issue" : "failed to star issue"),
          { tone: "danger" },
        );
      },
    });
  }

  function setIssueState(
    routeRef: ProviderRouteRef,
    number: number,
    state: GithubStateInputBody["state"],
    callbacks: MutationCallbacks = {},
  ): void {
    const ref = issueDetailRequestRef(routeRef.owner, routeRef.name, number, routeRef);
    if (!isIssueDetailShowingRef(ref)) {
      const message = "The selected issue changed before the state update started.";
      invokeMutationFailure(callbacks.onFailure, message);
      try {
        callbacks.onSettled?.();
      } catch {
        // Presentation callbacks do not own command acceptance.
      }
      return;
    }
    let mutationSettled = false;
    const baseline = issueDetail?.issue.State ?? state;
    const program = Effect.gen(function* () {
      const mutations = yield* ProviderMutations;
      const refreshOnStale = Effect.suspend(() => {
        const envelopeTick = nextWorkspaceLifecycleTick();
        return executeGeneratedApiRequest("sync issue after stale state mutation", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.IssuesService.syncIssueOnHost({ ...providerHostRouteParams(ref), number: number }, { signal })
            : client.IssuesService.syncIssue({ ...providerRouteParams(ref), number: number }, { signal }),
        ).pipe(
          Effect.map((detail): IssueDetail => ({ ...detail, events: detail.events ?? [] })),
          Effect.tap((detail) =>
            rebaseIssueMutations(ref, detail, () => {
              if (!isIssueDetailShowingRef(ref)) return false;
              let applied = false;
              applyEnvelopeAt(envelopeTick, () => {
                issueDetail = withPreservedLocalBody(detail);
                issueDetailLoaded = detail.detail_loaded ?? issueDetailLoaded;
                applied = true;
              });
              return applied;
            }),
          ),
          Effect.map((detail) => detail.issue.State),
        );
      }).pipe(Effect.provideService(ProviderMutations, mutations));
      yield* mutations.submit({
        key: issueMutationKey(ref, "actions"),
        baseline,
        optimistic: state,
        apply: (nextState) => applyIssueState(ref, nextState),
        commit: executeGeneratedApiRequest("POST issue state", (client, signal) =>
          providerUsesHostRoute(ref)
            ? client.IssuesService.setIssueGithubStateOnHost(
                { ...providerHostRouteParams(ref), number: number },
                { state },
                { signal },
              )
            : client.IssuesService.setIssueGithubState(
                { ...providerRouteParams(ref), number: number },
                { state },
                { signal },
              ),
        ).pipe(Effect.as(state)),
        refreshOnStale,
      });
      mutationSettled = true;
      yield* loadIssuesEffect();
      if (isIssueDetailShowingRef(ref)) {
        yield* refreshIssueDetailProgram(ref, issueSyncGeneration);
      }
      yield* invokeMutationCallback(callbacks.onSuccess);
    }).pipe(Effect.ensuring(invokeMutationCallback(callbacks.onSettled)));
    runtime.runCommand(program, {
      operation: "change issue state",
      safeContext: {
        provider: ref.provider,
        platformHost: concretePlatformHost(ref),
        owner: ref.owner,
        name: ref.name,
        number,
      },
      onFailure: (failure) => {
        if (mutationSettled) {
          showFlash("The issue state changed, but the latest issue could not be refreshed.", { tone: "danger" });
          return;
        }
        const message = providerMutationFailureMessage(failure, "failed to change issue state");
        if (callbacks.onFailure === undefined) showFlash(message, { tone: "danger" });
        invokeMutationFailure(callbacks.onFailure, message);
      },
    });
  }

  // --- navigation ---

  function getDisplayOrderIssues(): Issue[] {
    if (getGroupByRepo()) {
      const grouped = issuesByRepo();
      const ordered: Issue[] = [];
      for (const items of grouped.values()) {
        ordered.push(...items);
      }
      return ordered;
    }
    return getIssues();
  }

  function selectNextIssue(): void {
    const list = getDisplayOrderIssues();
    if (list.length === 0) return;
    if (selectedIssue === null) {
      const first = list[0];
      if (first !== undefined) {
        selectedIssue = {
          owner: first.repo_owner ?? "",
          name: first.repo_name ?? "",
          number: first.Number,
          provider: first.repo?.provider,
          platformHost: first.platform_host,
          repoPath: first.repo?.repo_path,
        };
      }
      return;
    }
    const idx = list.findIndex((i) => issueMatchesSelection(i, selectedIssue!));
    if (idx < list.length - 1) {
      const next = list[idx + 1];
      if (next !== undefined) {
        selectedIssue = {
          owner: next.repo_owner ?? "",
          name: next.repo_name ?? "",
          number: next.Number,
          provider: next.repo?.provider,
          platformHost: next.platform_host,
          repoPath: next.repo?.repo_path,
        };
      }
    }
  }

  function selectPrevIssue(): void {
    const list = getDisplayOrderIssues();
    if (list.length === 0) return;
    if (selectedIssue === null) {
      const last = list[list.length - 1];
      if (last !== undefined) {
        selectedIssue = {
          owner: last.repo_owner ?? "",
          name: last.repo_name ?? "",
          number: last.Number,
          provider: last.repo?.provider,
          platformHost: last.platform_host,
          repoPath: last.repo?.repo_path,
        };
      }
      return;
    }
    const idx = list.findIndex((i) => issueMatchesSelection(i, selectedIssue!));
    if (idx > 0) {
      const prev = list[idx - 1];
      if (prev !== undefined) {
        selectedIssue = {
          owner: prev.repo_owner ?? "",
          name: prev.repo_name ?? "",
          number: prev.Number,
          provider: prev.repo?.provider,
          platformHost: prev.platform_host,
          repoPath: prev.repo?.repo_path,
        };
      }
    }
  }

  return {
    getIssues,
    getHideBots,
    isIssuesLoading,
    isIssueListCapped,
    getIssuesError,
    getSelectedIssue,
    getIssueFilterStarred,
    setIssueFilterStarred,
    getInvolvesMe,
    setInvolvesMe,
    getUnassigned,
    setUnassigned,
    getReferencedByPR,
    setReferencedByPR,
    canFilterReferencedByPR,
    getIssueSearchQuery,
    setIssueSearchQuery,
    getIssueFilterState,
    setIssueFilterState,
    hydrateDefaults,
    setHideBots,
    issuesByRepo,
    selectIssue,
    clearIssueSelection,
    loadIssues,
    loadIssuesEffect,
    reconcileIssuesEffect,
    getIssueDetail,
    getIssueDetailEnvelopeTick,
    isIssueDetailLoading,
    isIssueDetailSyncing,
    getIssueDetailError,
    getIssueDetailLoaded,
    isIssueStaleRefreshing,
    loadIssueDetail,
    syncIssueDetailNow,
    startIssueDetailPolling,
    stopIssueDetailPolling,
    clearIssueDetail,
    setIssueLabels,
    setIssueAssignees,
    submitIssueComment,
    editIssueComment,
    deleteIssueComment,
    setLocalIssueBody,
    saveIssueBodyInBackground,
    hasUnsavedLocalBody,
    toggleIssueStar,
    setIssueState,
    selectNextIssue,
    selectPrevIssue,
  };
}

export type IssuesStore = ReturnType<typeof createIssuesStore>;
