import type {
  KataCreateLinkRequest as GeneratedKataCreateLinkRequest,
  KataEffectiveLink as GeneratedKataEffectiveLink,
  KataEffectiveLinksResponse as GeneratedKataEffectiveLinksResponse,
  KataIssueDetailResponse as GeneratedKataIssueDetailResponse,
  KataLinkDiagnostic as GeneratedKataLinkDiagnostic,
} from "../api/generated/models/index.js";
import type { GeneratedClient } from "../api/generated-api.js";
import { reconcileKataWorkspaceCreated } from "./kata-workspace-create.svelte.js";
import { nextWorkspaceLifecycleTick } from "./workspace-create-pending.svelte.js";

export type KataEffectiveLink = GeneratedKataEffectiveLink;
export type KataEffectiveLinksResponse = GeneratedKataEffectiveLinksResponse;
export type KataIssueDetailResponse = GeneratedKataIssueDetailResponse;
export type KataLinkDiagnostic = GeneratedKataLinkDiagnostic;

export type KataLinksSubject =
  | {
      kind: "pull_request" | "issue";
      provider: string;
      platformHost?: string;
      owner: string;
      name: string;
      number: number;
    }
  | { kind: "workspace"; workspaceID: string };

export interface KataLinksStoreOptions {
  client: GeneratedClient;
  subject: KataLinksSubject;
  clock?: () => number;
  visible?: boolean;
}

export interface KataLinksStore {
  links(): readonly KataEffectiveLink[];
  diagnostics(): readonly KataLinkDiagnostic[];
  state(): KataEffectiveLinksResponse["state"] | null;
  selected(): KataEffectiveLink | null;
  detail(): KataIssueDetailResponse | null;
  loading(): boolean;
  refreshingDetail(): boolean;
  error(): string | null;
  setSubject(subject: KataLinksSubject): void;
  loadLinks(): Promise<void>;
  select(daemonID: string, issueUID: string): Promise<void>;
  refreshDetail(): Promise<void>;
  activate(active: boolean): void;
  noteFocus(): void;
  noteVisibility(visible: boolean): void;
  destroy(): void;
}

const ASSOCIATION_REFRESH_AGE_MS = 15_000;
const DETAIL_POLL_INTERVAL_MS = 30_000;

function subjectKey(subject: KataLinksSubject): string {
  if (subject.kind === "workspace") return `workspace:${subject.workspaceID}`;
  return [
    subject.kind,
    subject.provider,
    subject.platformHost ?? "",
    subject.owner,
    subject.name,
    String(subject.number),
  ].join(":");
}

function linkKey(link: Pick<KataEffectiveLink, "daemon_id" | "issue_uid">): string {
  return `${link.daemon_id}:${link.issue_uid}`;
}

function aborted(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

class KataLinksStoreImpl implements KataLinksStore {
  #client: GeneratedClient;
  #subject: KataLinksSubject;
  #subjectKey: string;
  #clock: () => number;

  #response = $state.raw<KataEffectiveLinksResponse | null>(null);
  #selectedKey = $state<string | null>(null);
  #detailEnvelope = $state.raw<KataIssueDetailResponse | null>(null);
  #loading = $state(false);
  #detailLoading = $state(false);
  #error = $state<string | null>(null);
  #selected = $derived(this.#response?.links.find((candidate) => linkKey(candidate) === this.#selectedKey) ?? null);

  #active = false;
  #visible: boolean;
  #destroyed = false;
  #lastAssociationRefreshAt: number | null = null;
  #linksGeneration = 0;
  #detailGeneration = 0;
  #linksController: AbortController | null = null;
  #detailController: AbortController | null = null;
  #pollHandle: ReturnType<typeof setInterval> | null = null;

  constructor(options: KataLinksStoreOptions) {
    this.#client = options.client;
    this.#subject = options.subject;
    this.#subjectKey = subjectKey(options.subject);
    this.#clock = options.clock ?? Date.now;
    this.#visible = options.visible ?? true;
  }

  links(): readonly KataEffectiveLink[] {
    return this.#response?.links ?? [];
  }

  diagnostics(): readonly KataLinkDiagnostic[] {
    return this.#response?.diagnostics ?? [];
  }

  state(): KataEffectiveLinksResponse["state"] | null {
    return this.#response?.state ?? null;
  }

  selected(): KataEffectiveLink | null {
    return this.#selected;
  }

  detail(): KataIssueDetailResponse | null {
    return this.#detailEnvelope;
  }

  loading(): boolean {
    return this.#loading;
  }

  refreshingDetail(): boolean {
    return this.#detailLoading;
  }

  error(): string | null {
    return this.#error;
  }

  setSubject(subject: KataLinksSubject): void {
    const nextKey = subjectKey(subject);
    if (nextKey === this.#subjectKey) return;

    this.#subject = subject;
    this.#subjectKey = nextKey;
    this.#linksGeneration += 1;
    this.#detailGeneration += 1;
    this.#linksController?.abort();
    this.#detailController?.abort();
    this.#stopPolling();
    this.#response = null;
    this.#selectedKey = null;
    this.#detailEnvelope = null;
    this.#loading = false;
    this.#detailLoading = false;
    this.#error = null;
    this.#lastAssociationRefreshAt = null;
    this.#startPolling();
  }

  async loadLinks(): Promise<void> {
    if (this.#destroyed) return;
    const responseTick = nextWorkspaceLifecycleTick();
    const generation = ++this.#linksGeneration;
    const requestSubject = this.#subject;
    const requestSubjectKey = this.#subjectKey;
    this.#linksController?.abort();
    const controller = new AbortController();
    this.#linksController = controller;
    this.#loading = true;
    this.#error = null;

    try {
      const result = await this.#fetchLinks(requestSubject, controller.signal);
      if (!this.#acceptLinksResponse(generation, requestSubjectKey, controller)) return;

      const previousSelection = this.#selectedKey;
      for (const link of result.links) {
        reconcileKataWorkspaceCreated(
          { daemonID: link.daemon_id, issueUID: link.issue_uid },
          link.workspace?.existing_workspace,
          responseTick,
        );
      }
      this.#response = result;
      this.#lastAssociationRefreshAt = this.#clock();
      const nextSelection =
        result.links.find((candidate) => linkKey(candidate) === previousSelection) === undefined
          ? result.links[0] === undefined
            ? null
            : linkKey(result.links[0])
          : previousSelection;
      if (nextSelection !== previousSelection) {
        this.#detailGeneration += 1;
        this.#detailController?.abort();
        this.#detailController = null;
        this.#detailEnvelope = null;
        this.#detailLoading = false;
      }
      this.#selectedKey = nextSelection;

      if (this.#selectedKey === null) {
        this.#detailGeneration += 1;
        this.#detailController?.abort();
        this.#detailEnvelope = null;
      } else {
        await this.refreshDetail();
      }
    } catch (error) {
      if (!aborted(error) && this.#acceptLinksResponse(generation, requestSubjectKey, controller)) {
        this.#error = error instanceof Error ? error.message : "Unable to load linked Kata issues.";
      }
    } finally {
      if (this.#acceptLinksResponse(generation, requestSubjectKey, controller)) {
        this.#loading = false;
        this.#linksController = null;
        this.#startPolling();
      }
    }
  }

  async select(daemonID: string, issueUID: string): Promise<void> {
    const next = this.#response?.links.find(
      (candidate) => candidate.daemon_id === daemonID && candidate.issue_uid === issueUID,
    );
    if (!next) return;
    const nextKey = linkKey(next);
    if (nextKey === this.#selectedKey && this.#detailEnvelope !== null) return;
    this.#selectedKey = nextKey;
    this.#detailEnvelope = null;
    await this.refreshDetail();
  }

  async refreshDetail(): Promise<void> {
    const selected = this.#selected;
    if (this.#destroyed || !selected) return;
    if (selected.unavailable_reason) {
      this.#detailEnvelope = null;
      return;
    }

    const generation = ++this.#detailGeneration;
    const requestKey = linkKey(selected);
    const requestSubjectKey = this.#subjectKey;
    this.#detailController?.abort();
    const controller = new AbortController();
    this.#detailController = controller;
    this.#detailLoading = true;
    this.#error = null;

    try {
      const result = await this.#client.KataService.getKataIssueDetail(
        { daemonId: selected.daemon_id, issueUid: selected.issue_uid },
        { signal: controller.signal },
      );
      if (!this.#acceptDetailResponse(generation, requestSubjectKey, requestKey, controller)) return;
      this.#detailEnvelope = result;
    } catch (error) {
      if (!aborted(error) && this.#acceptDetailResponse(generation, requestSubjectKey, requestKey, controller)) {
        this.#detailEnvelope = null;
        this.#error = error instanceof Error ? error.message : "Unable to load Kata issue detail.";
      }
    } finally {
      if (this.#acceptDetailResponse(generation, requestSubjectKey, requestKey, controller)) {
        this.#detailLoading = false;
        this.#detailController = null;
      }
    }
  }

  activate(active: boolean): void {
    if (this.#destroyed || this.#active === active) return;
    this.#active = active;
    if (!active) {
      this.#stopPolling();
      return;
    }
    this.#refreshAssociationsWhenStale();
    this.#startPolling();
  }

  noteFocus(): void {
    if (!this.#active || !this.#visible || this.#destroyed) return;
    this.#refreshAssociationsWhenStale();
  }

  noteVisibility(visible: boolean): void {
    if (this.#destroyed || this.#visible === visible) return;
    this.#visible = visible;
    if (!visible) {
      this.#stopPolling();
      return;
    }
    if (this.#active) this.#refreshAssociationsWhenStale();
    this.#startPolling();
  }

  destroy(): void {
    if (this.#destroyed) return;
    this.#destroyed = true;
    this.#linksGeneration += 1;
    this.#detailGeneration += 1;
    this.#linksController?.abort();
    this.#detailController?.abort();
    this.#stopPolling();
  }

  #refreshAssociationsWhenStale(): void {
    if (!this.#active || !this.#visible || this.#destroyed) return;
    if (this.#linksController !== null) return;
    if (
      this.#lastAssociationRefreshAt !== null &&
      this.#clock() - this.#lastAssociationRefreshAt < ASSOCIATION_REFRESH_AGE_MS
    )
      return;
    void this.loadLinks();
  }

  #startPolling(): void {
    if (this.#pollHandle !== null || !this.#active || !this.#visible || this.#destroyed) return;
    if (this.#selected === null) return;
    this.#pollHandle = setInterval(() => {
      void this.refreshDetail();
    }, DETAIL_POLL_INTERVAL_MS);
  }

  #stopPolling(): void {
    if (this.#pollHandle === null) return;
    clearInterval(this.#pollHandle);
    this.#pollHandle = null;
  }

  #acceptLinksResponse(generation: number, requestSubjectKey: string, controller: AbortController): boolean {
    return (
      !this.#destroyed &&
      generation === this.#linksGeneration &&
      requestSubjectKey === this.#subjectKey &&
      !controller.signal.aborted
    );
  }

  #acceptDetailResponse(
    generation: number,
    requestSubjectKey: string,
    requestKey: string,
    controller: AbortController,
  ): boolean {
    return (
      !this.#destroyed &&
      generation === this.#detailGeneration &&
      requestSubjectKey === this.#subjectKey &&
      requestKey === this.#selectedKey &&
      !controller.signal.aborted
    );
  }

  #fetchLinks(subject: KataLinksSubject, signal: AbortSignal) {
    if (subject.kind === "workspace") {
      return this.#client.KataService.listWorkspaceKataLinks({ id: subject.workspaceID }, { signal });
    }

    if (subject.platformHost) {
      if (subject.kind === "pull_request") {
        return this.#client.KataService.listPullRequestKataLinksOnHost(
          {
            platformHost: subject.platformHost,
            provider: subject.provider,
            owner: subject.owner,
            name: subject.name,
            number: subject.number,
          },
          { signal },
        );
      }
      return this.#client.KataService.listIssueKataLinksOnHost(
        {
          platformHost: subject.platformHost,
          provider: subject.provider,
          owner: subject.owner,
          name: subject.name,
          number: subject.number,
        },
        { signal },
      );
    }

    if (subject.kind === "pull_request") {
      return this.#client.KataService.listPullRequestKataLinks(
        {
          provider: subject.provider,
          owner: subject.owner,
          name: subject.name,
          number: subject.number,
        },
        { signal },
      );
    }
    return this.#client.KataService.listIssueKataLinks(
      {
        provider: subject.provider,
        owner: subject.owner,
        name: subject.name,
        number: subject.number,
      },
      { signal },
    );
  }
}

export function createKataLinksStore(options: KataLinksStoreOptions): KataLinksStore {
  return new KataLinksStoreImpl(options);
}

export type KataCreateLinkRequest = GeneratedKataCreateLinkRequest;

export function createKataLink(client: GeneratedClient, subject: KataLinksSubject, body: KataCreateLinkRequest) {
  if (subject.kind === "workspace") {
    return client.KataService.createWorkspaceKataLink({ id: subject.workspaceID }, body);
  }
  if (subject.platformHost) {
    if (subject.kind === "pull_request") {
      return client.KataService.createPullRequestKataLinkOnHost(
        {
          platformHost: subject.platformHost,
          provider: subject.provider,
          owner: subject.owner,
          name: subject.name,
          number: subject.number,
        },
        body,
      );
    }
    return client.KataService.createIssueKataLinkOnHost(
      {
        platformHost: subject.platformHost,
        provider: subject.provider,
        owner: subject.owner,
        name: subject.name,
        number: subject.number,
      },
      body,
    );
  }
  if (subject.kind === "pull_request") {
    return client.KataService.createPullRequestKataLink(
      {
        provider: subject.provider,
        owner: subject.owner,
        name: subject.name,
        number: subject.number,
      },
      body,
    );
  }
  return client.KataService.createIssueKataLink(
    {
      provider: subject.provider,
      owner: subject.owner,
      name: subject.name,
      number: subject.number,
    },
    body,
  );
}

export function deleteKataLink(client: GeneratedClient, subject: KataLinksSubject, linkID: number) {
  if (subject.kind === "workspace") {
    return client.KataService.deleteWorkspaceKataLink({ id: subject.workspaceID, linkId: linkID });
  }
  if (subject.platformHost) {
    if (subject.kind === "pull_request") {
      return client.KataService.deletePullRequestKataLinkOnHost({
        platformHost: subject.platformHost,
        provider: subject.provider,
        owner: subject.owner,
        name: subject.name,
        number: subject.number,
        linkId: linkID,
      });
    }
    return client.KataService.deleteIssueKataLinkOnHost({
      platformHost: subject.platformHost,
      provider: subject.provider,
      owner: subject.owner,
      name: subject.name,
      number: subject.number,
      linkId: linkID,
    });
  }
  if (subject.kind === "pull_request") {
    return client.KataService.deletePullRequestKataLink({
      provider: subject.provider,
      owner: subject.owner,
      name: subject.name,
      number: subject.number,
      linkId: linkID,
    });
  }
  return client.KataService.deleteIssueKataLink({
    provider: subject.provider,
    owner: subject.owner,
    name: subject.name,
    number: subject.number,
    linkId: linkID,
  });
}
