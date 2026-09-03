import { Context, Effect, Fiber, FiberHandle, FiberMap, Layer, Ref, Semaphore } from "effect";
import type { CreateIssueInputBody, IssueResponse } from "../../api/generated/models/index.js";
import type { ApiProblemError, TransientTransportError } from "../../api/effect-errors.js";
import { GeneratedApi } from "../../api/generated-api.js";
import {
  canonicalProvider,
  providerRouteParams,
  resolvedPlatformHost,
  type ProviderRouteRef,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../../api/provider-routes.js";
import type { RepoSummary } from "../../api/types.js";
import { CommandQueueClosed, makeOrderedCommandQueue } from "../../effect/ordered-command-queue.js";

export type RepoSummaryReadError = ApiProblemError | TransientTransportError;
export type RepoSummaryIssueError = ApiProblemError | TransientTransportError;
export type RepoSummaryIssueResponse = IssueResponse;
export type RepoSummaryIssueBody = Pick<CreateIssueInputBody, "title" | "body">;

export interface RepoSummaryIssueRequest extends RepoSummaryIssueBody {
  readonly ref: ProviderRouteRef;
}

interface AcceptedRepoSummaryIssueRequest extends RepoSummaryIssueRequest {
  readonly key: string;
  readonly submissionID: number;
}

export type RepoSummaryIssueState =
  | { readonly kind: "pending"; readonly request: AcceptedRepoSummaryIssueRequest }
  | {
      readonly kind: "succeeded";
      readonly request: AcceptedRepoSummaryIssueRequest;
      readonly issue: RepoSummaryIssueResponse;
    }
  | {
      readonly kind: "failed";
      readonly request: AcceptedRepoSummaryIssueRequest;
      readonly error: RepoSummaryIssueError;
    }
  | {
      readonly kind: "uncertain";
      readonly request: AcceptedRepoSummaryIssueRequest;
      readonly error: RepoSummaryIssueError;
    };

export type RepoSummaryIssueObserver = (state: RepoSummaryIssueState) => Effect.Effect<boolean>;

interface RepoSummaryIssuePresenter {
  readonly sessionID: string;
  readonly observe: RepoSummaryIssueObserver;
}

interface RepoSummaryIssueRegistry {
  readonly entries: ReadonlyMap<string, RepoSummaryIssueState>;
  readonly presenter?: RepoSummaryIssuePresenter | undefined;
}

let nextRepoSummaryPresenter = 0;

export function makeRepoSummaryPresenterID(): string {
  nextRepoSummaryPresenter += 1;
  return `repo-summary:${nextRepoSummaryPresenter}`;
}

function normalizedIdentityPart(provider: string, value: string): string {
  const trimmed = value.trim();
  return provider === "github" ? trimmed.toLowerCase() : trimmed;
}

export function repoSummaryIssueKey(ref: ProviderRouteRef): string {
  const provider = canonicalProvider(ref.provider.trim());
  return JSON.stringify([
    provider,
    resolvedPlatformHost(provider, ref.platformHost).trim().toLowerCase(),
    normalizedIdentityPart(provider, ref.owner),
    normalizedIdentityPart(provider, ref.name),
    normalizedIdentityPart(provider, ref.repoPath.replace(/^\/+|\/+$/g, "")),
  ]);
}

function issueOutcomeIsUncertain(error: RepoSummaryIssueError): boolean {
  return error._tag === "TransientTransportError" || error.problem.code === "mutationOutcomeUnknown";
}

export class RepoSummaryWorkflow extends Context.Service<
  RepoSummaryWorkflow,
  {
    readonly read: Effect.Effect<RepoSummary[], RepoSummaryReadError>;
    readonly createIssue: (request: RepoSummaryIssueRequest) => Effect.Effect<void, CommandQueueClosed>;
    readonly claimIssuePresenter: (sessionID: string, observe: RepoSummaryIssueObserver) => Effect.Effect<void>;
    readonly releaseIssuePresenter: (sessionID: string) => Effect.Effect<void>;
    readonly acknowledgeUncertainIssue: (key: string, submissionID: number, sessionID: string) => Effect.Effect<void>;
  }
>()("kenn-forge/RepoSummaryWorkflow") {}

export const RepoSummaryWorkflowLive = Layer.effect(RepoSummaryWorkflow)(
  Effect.gen(function* () {
    const api = yield* GeneratedApi;
    const reads = yield* FiberHandle.make<RepoSummary[], RepoSummaryReadError>();
    const request = api
      .execute("load repository summaries", (signal) => api.client.RepositoriesService.listRepoSummaries({ signal }))
      .pipe(Effect.map((summaries) => summaries ?? []));
    const issueSequence = yield* Ref.make(1);
    const issueRegistry = yield* Ref.make<RepoSummaryIssueRegistry>({ entries: new Map() });
    const issueRegistryLock = yield* Semaphore.make(1);
    const issuePresentations = yield* FiberMap.make<string, void, never>();

    const presentStoredIssueState = Effect.fn("RepoSummaryWorkflow.presentStoredIssueState")(function* (
      state: RepoSummaryIssueState,
      expectedSessionID?: string,
    ) {
      const presentation = yield* FiberMap.run(
        issuePresentations,
        state.request.key,
        Effect.yieldNow.pipe(
          Effect.andThen(
            Effect.gen(function* () {
              const presenter = yield* issueRegistryLock.withPermit(
                Ref.get(issueRegistry).pipe(
                  Effect.map((current) => {
                    if (current.entries.get(state.request.key) !== state) return undefined;
                    if (expectedSessionID !== undefined && current.presenter?.sessionID !== expectedSessionID) {
                      return undefined;
                    }
                    return current.presenter;
                  }),
                ),
              );
              if (presenter === undefined) return;

              const acknowledged = yield* presenter.observe(state);
              if (!acknowledged) return;
              yield* issueRegistryLock.withPermit(
                Ref.update(issueRegistry, (current) => {
                  if (
                    current.presenter?.sessionID !== presenter.sessionID ||
                    current.entries.get(state.request.key) !== state
                  ) {
                    return current;
                  }
                  const entries = new Map(current.entries);
                  entries.delete(state.request.key);
                  return { entries, presenter: current.presenter };
                }),
              );
            }),
          ),
        ),
      );
      yield* Fiber.await(presentation);
    });

    const storeAndPresentIssueState = Effect.fn("RepoSummaryWorkflow.storeAndPresentIssueState")(function* (
      state: RepoSummaryIssueState,
    ) {
      yield* issueRegistryLock.withPermit(
        Ref.update(issueRegistry, (current) => ({
          entries: new Map(current.entries).set(state.request.key, state),
          ...(current.presenter === undefined ? {} : { presenter: current.presenter }),
        })),
      );
      yield* presentStoredIssueState(state);
    });

    const issueCommands = yield* makeOrderedCommandQueue(
      "repository issue creation",
      Effect.fn("RepoSummaryWorkflow.createAcceptedIssue")(function* (accepted: AcceptedRepoSummaryIssueRequest) {
        const result = yield* api
          .execute("create repository issue", (signal) =>
            providerUsesHostRoute(accepted.ref)
              ? api.client.IssuesService.createIssueOnHost(
                  { ...providerHostRouteParams(accepted.ref) },
                  { title: accepted.title, body: accepted.body },
                  { signal },
                )
              : api.client.IssuesService.createIssue(
                  { ...providerRouteParams(accepted.ref) },
                  { title: accepted.title, body: accepted.body },
                  { signal },
                ),
          )
          .pipe(
            Effect.match({
              onFailure: (error): RepoSummaryIssueState => ({
                kind: issueOutcomeIsUncertain(error) ? "uncertain" : "failed",
                request: accepted,
                error,
              }),
              onSuccess: (issue): RepoSummaryIssueState => ({ kind: "succeeded", request: accepted, issue }),
            }),
          );
        yield* storeAndPresentIssueState(result);
      }),
    );

    const createIssue = Effect.fn("RepoSummaryWorkflow.createIssue")(function* (submitted: RepoSummaryIssueRequest) {
      const admission = yield* issueRegistryLock.withPermit(
        Effect.gen(function* () {
          const key = repoSummaryIssueKey(submitted.ref);
          const current = yield* Ref.get(issueRegistry);
          if (current.entries.has(key)) return undefined;
          const submissionID = yield* Ref.getAndUpdate(issueSequence, (sequence) => sequence + 1);
          const accepted = { ...submitted, key, submissionID } satisfies AcceptedRepoSummaryIssueRequest;
          const pending = { kind: "pending", request: accepted } satisfies RepoSummaryIssueState;
          yield* Ref.set(issueRegistry, {
            entries: new Map(current.entries).set(key, pending),
            ...(current.presenter === undefined ? {} : { presenter: current.presenter }),
          });
          return { accepted, pending };
        }),
      );
      if (admission === undefined) return;

      const rollbackAdmission = issueRegistryLock.withPermit(
        Ref.update(issueRegistry, (current) => {
          if (current.entries.get(admission.accepted.key) !== admission.pending) return current;
          const entries = new Map(current.entries);
          entries.delete(admission.accepted.key);
          return {
            entries,
            ...(current.presenter === undefined ? {} : { presenter: current.presenter }),
          };
        }),
      );
      const acknowledgement = yield* issueCommands.accept(admission.accepted).pipe(
        Effect.tapError(() => rollbackAdmission),
        Effect.onInterrupt(() => rollbackAdmission),
      );
      // Admission is the command boundary here. The ordered queue owns execution,
      // while this caller immediately publishes the accepted pending state.
      void acknowledgement;
      yield* presentStoredIssueState(admission.pending);
    });

    const claimIssuePresenter = Effect.fn("RepoSummaryWorkflow.claimIssuePresenter")(function* (
      sessionID: string,
      observe: RepoSummaryIssueObserver,
    ) {
      const states = yield* issueRegistryLock.withPermit(
        Effect.gen(function* () {
          const presenter = { sessionID, observe } satisfies RepoSummaryIssuePresenter;
          const registry = yield* Ref.updateAndGet(issueRegistry, (current) => ({
            entries: current.entries,
            presenter,
          }));
          return Array.from(registry.entries.values());
        }),
      );
      yield* Effect.forEach(states, (state) => presentStoredIssueState(state, sessionID), { discard: true });
    });

    const releaseIssuePresenter = (sessionID: string): Effect.Effect<void> =>
      issueRegistryLock.withPermit(
        Ref.update(issueRegistry, (current) =>
          current.presenter?.sessionID === sessionID ? { entries: current.entries } : current,
        ),
      );

    const acknowledgeUncertainIssue = (key: string, submissionID: number, sessionID: string): Effect.Effect<void> =>
      issueRegistryLock.withPermit(
        Ref.update(issueRegistry, (current) => {
          const state = current.entries.get(key);
          if (
            current.presenter?.sessionID !== sessionID ||
            state?.kind !== "uncertain" ||
            state.request.submissionID !== submissionID
          ) {
            return current;
          }
          const entries = new Map(current.entries);
          entries.delete(key);
          return { entries, presenter: current.presenter };
        }),
      );

    return {
      read: FiberHandle.run(reads, request).pipe(Effect.flatMap(Fiber.join)),
      createIssue,
      claimIssuePresenter,
      releaseIssuePresenter,
      acknowledgeUncertainIssue,
    };
  }),
);
