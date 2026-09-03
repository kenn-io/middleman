import { Context, Data, Effect, Layer, Option, Ref } from "effect";
import type {
  AddRepoInputBody,
  BulkAddRepoRequest,
  FleetSettingsResponse,
  RepoPreset as GeneratedRepoPreset,
  RepoPresetRepository as GeneratedRepoPresetRepository,
  RepoPreviewResponse as GeneratedRepoPreviewResponse,
  RepoPreviewRow as GeneratedRepoPreviewRow,
  SettingsResponse,
  UpdateFleetSettingsInputBody,
  UpdateSettingsRequest as GeneratedUpdateSettingsRequest,
} from "../api/generated/models/index.js";
import { GeneratedApi } from "../api/generated-api.js";
import type { ApiProblemError, TransientTransportError } from "../api/effect-errors.js";
import {
  canonicalProvider,
  providerRouteParams,
  resolvedPlatformHost,
  providerHostRouteParams,
  providerUsesHostRoute,
} from "../api/provider-routes.js";
import { retryIdempotentRead } from "../api/retry-policy.js";
import { StartupWorkflow } from "../app/startup-workflow.js";
import { CommandQueueClosed, makeOrderedCommandQueue } from "../effect/ordered-command-queue.js";

export type UpdateSettingsRequest = GeneratedUpdateSettingsRequest;
export type SettingsSnapshot = SettingsResponse;
export type FleetSettingsUpdate = UpdateFleetSettingsInputBody;
export type FleetSettingsSnapshot = FleetSettingsResponse;
export type RepoRequestOptions = Pick<AddRepoInputBody, "provider" | "host">;
export type RepoInput = BulkAddRepoRequest;
export type RepoPromotionInput = RepoInput & Required<Pick<RepoInput, "owner" | "name">>;
export type RepoPreviewResponse = GeneratedRepoPreviewResponse;
export type RepoPreviewRow = GeneratedRepoPreviewRow;
export type RepoPreset = GeneratedRepoPreset;
export type RepoPresetRepository = GeneratedRepoPresetRepository;
type SettingsCommand =
  | { readonly _tag: "Partial"; readonly request: () => UpdateSettingsRequest }
  | { readonly _tag: "Fleet"; readonly request: FleetSettingsUpdate }
  | { readonly _tag: "CreateRepoPreset"; readonly preset: RepoPreset }
  | { readonly _tag: "UpdateRepoPreset"; readonly name: string; readonly repos: readonly RepoPresetRepository[] }
  | { readonly _tag: "DeleteRepoPreset"; readonly name: string }
  | { readonly _tag: "AddRepo"; readonly owner: string; readonly name: string; readonly options: RepoRequestOptions }
  | { readonly _tag: "RemoveRepo"; readonly owner: string; readonly name: string; readonly options: RepoRequestOptions }
  | {
      readonly _tag: "RefreshRepo";
      readonly owner: string;
      readonly name: string;
      readonly options: RepoRequestOptions;
    }
  | {
      readonly _tag: "UpdateRepoWorktreeBase";
      readonly owner: string;
      readonly name: string;
      readonly options: RepoRequestOptions;
      readonly worktreeBasePath: string;
    }
  | {
      readonly _tag: "UpdateRepoUIVisibility";
      readonly owner: string;
      readonly name: string;
      readonly options: RepoRequestOptions;
      readonly hidden: boolean;
    }
  | { readonly _tag: "BulkAddRepos"; readonly repos: readonly RepoInput[] }
  | {
      readonly _tag: "PromoteRepo";
      readonly repo: RepoPromotionInput;
      readonly worktreeBasePath: string;
      readonly exactRepoAlreadyAdded: boolean;
    };
type SettingsCommandResult =
  | { readonly _tag: "Settings"; readonly settings: SettingsSnapshot }
  | { readonly _tag: "Fleet"; readonly fleet: FleetSettingsSnapshot }
  | { readonly _tag: "RepoRemoved" };

function settingsCommandResult(settings: SettingsSnapshot): SettingsCommandResult {
  return { _tag: "Settings", settings };
}

function fleetCommandResult(fleet: FleetSettingsSnapshot): SettingsCommandResult {
  return { _tag: "Fleet", fleet };
}

function repoRemovedResult(): SettingsCommandResult {
  return { _tag: "RepoRemoved" };
}
type SettingsTransportError = ApiProblemError | TransientTransportError;

export class RepoPromotionRollbackError extends Data.TaggedError("RepoPromotionRollbackError")<{
  readonly failure: SettingsTransportError;
  readonly rollbackFailure: SettingsTransportError;
  readonly settings: SettingsSnapshot;
}> {}

export class RepoPromotionStateUncertainError extends Data.TaggedError("RepoPromotionStateUncertainError")<{
  readonly failure: SettingsTransportError;
  readonly rollbackFailure: SettingsTransportError;
  readonly reconciliationFailure: SettingsTransportError;
}> {}

export class SettingsMutationStateUncertainError extends Data.TaggedError("SettingsMutationStateUncertainError")<{
  readonly operation: string;
  readonly failure: SettingsTransportError;
  readonly reconciliationFailure: SettingsTransportError;
}> {}

export type SettingsError =
  | SettingsTransportError
  | CommandQueueClosed
  | RepoPromotionRollbackError
  | RepoPromotionStateUncertainError
  | SettingsMutationStateUncertainError;
type RepoPromotionFailure =
  | SettingsTransportError
  | RepoPromotionRollbackError
  | RepoPromotionStateUncertainError
  | SettingsMutationStateUncertainError;
export type SettingsReadError = ApiProblemError | TransientTransportError;

export class SettingsWorkflow extends Context.Service<
  SettingsWorkflow,
  {
    readonly persist: (request: () => UpdateSettingsRequest) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly updateFleet: (request: FleetSettingsUpdate) => Effect.Effect<FleetSettingsSnapshot, SettingsError>;
    readonly createRepoPreset: (preset: RepoPreset) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly updateRepoPreset: (
      name: string,
      repos: readonly RepoPresetRepository[],
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly deleteRepoPreset: (name: string) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly addRepo: (
      owner: string,
      name: string,
      options: RepoRequestOptions,
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly removeRepo: (
      owner: string,
      name: string,
      options: RepoRequestOptions,
    ) => Effect.Effect<void, SettingsError>;
    readonly refreshRepo: (
      owner: string,
      name: string,
      options: RepoRequestOptions,
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly updateRepoWorktreeBasePath: (
      owner: string,
      name: string,
      options: RepoRequestOptions,
      worktreeBasePath: string,
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly updateRepoUIVisibility: (
      owner: string,
      name: string,
      options: RepoRequestOptions,
      hidden: boolean,
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly previewRepos: (
      owner: string,
      pattern: string,
      options: RepoRequestOptions,
    ) => Effect.Effect<RepoPreviewResponse, SettingsReadError>;
    readonly bulkAddRepos: (repos: readonly RepoInput[]) => Effect.Effect<SettingsSnapshot, SettingsError>;
    readonly promoteRepo: (
      repo: RepoPromotionInput,
      worktreeBasePath: string,
      exactRepoAlreadyAdded: boolean,
    ) => Effect.Effect<SettingsSnapshot, SettingsError>;
  }
>()("kenn-forge/SettingsWorkflow") {}

function repoRef(owner: string, name: string, options: RepoRequestOptions) {
  return {
    provider: options.provider,
    platformHost: options.host,
    owner,
    name,
    repoPath: `${owner}/${name}`,
  };
}

function containsExactRepo(
  settings: SettingsSnapshot,
  owner: string,
  name: string,
  options: RepoRequestOptions,
): boolean {
  return settings.repos.some(
    (repo) =>
      !repo.is_glob &&
      canonicalProvider(repo.provider) === canonicalProvider(options.provider) &&
      resolvedPlatformHost(repo.provider, repo.platform_host) ===
        resolvedPlatformHost(options.provider, options.host) &&
      repo.owner === owner &&
      repo.name === name,
  );
}

function containsRepoInput(settings: SettingsSnapshot, input: RepoInput): boolean {
  const requestedHost = input.host ?? input.platform_host;
  const requestedPath = input.repo_path ?? (input.owner && input.name ? `${input.owner}/${input.name}` : undefined);
  if (requestedPath === undefined) return false;
  return settings.repos.some(
    (repo) =>
      canonicalProvider(repo.provider) === canonicalProvider(input.provider) &&
      resolvedPlatformHost(repo.provider, repo.platform_host) === resolvedPlatformHost(input.provider, requestedHost) &&
      repo.repo_path === requestedPath,
  );
}

function sameValue(left: unknown, right: unknown): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

function settingsMatchRequest(settings: SettingsSnapshot, request: UpdateSettingsRequest): boolean {
  return (
    (request.activity === undefined || sameValue(settings.activity, request.activity)) &&
    (request.agents === undefined || sameValue(settings.agents, request.agents)) &&
    (request.detail === undefined || sameValue(settings.detail, request.detail)) &&
    (request.issues === undefined || sameValue(settings.issues, request.issues)) &&
    (request.kata_projects === undefined || sameValue(settings.kata_projects, request.kata_projects)) &&
    (request.modes === undefined || sameValue(settings.modes, request.modes)) &&
    (request.pull_requests === undefined || sameValue(settings.pull_requests, request.pull_requests)) &&
    (request.roborev === undefined ||
      Object.entries(request.roborev).every(
        ([key, value]) => settings.roborev[key as keyof typeof settings.roborev] === value,
      )) &&
    (request.terminal === undefined || sameValue(settings.terminal, request.terminal)) &&
    (request.workspaces === undefined ||
      Object.entries(request.workspaces).every(
        ([key, value]) => settings.workspaces[key as keyof typeof settings.workspaces] === value,
      ))
  );
}

function fleetMatchesRequest(fleet: FleetSettingsSnapshot, request: FleetSettingsUpdate): boolean {
  return (
    fleet.enabled === request.enabled &&
    (request.peer_timeout === undefined || fleet.peer_timeout === request.peer_timeout) &&
    sameValue(fleet.sessions, request.sessions)
  );
}

function exactRepoWorktreeBaseMatches(
  settings: SettingsSnapshot,
  owner: string,
  name: string,
  options: RepoRequestOptions,
  worktreeBasePath: string,
): boolean {
  return settings.repos.some(
    (repo) =>
      !repo.is_glob &&
      canonicalProvider(repo.provider) === canonicalProvider(options.provider) &&
      resolvedPlatformHost(repo.provider, repo.platform_host) ===
        resolvedPlatformHost(options.provider, options.host) &&
      repo.owner === owner &&
      repo.name === name &&
      (repo.worktree_base_path ?? "") === worktreeBasePath,
  );
}

function exactRepoUIVisibilityMatches(
  settings: SettingsSnapshot,
  owner: string,
  name: string,
  options: RepoRequestOptions,
  hidden: boolean,
): boolean {
  return settings.repos.some(
    (repo) =>
      !repo.is_glob &&
      canonicalProvider(repo.provider) === canonicalProvider(options.provider) &&
      resolvedPlatformHost(repo.provider, repo.platform_host) ===
        resolvedPlatformHost(options.provider, options.host) &&
      repo.owner === owner &&
      repo.name === name &&
      repo.hidden_from_ui === hidden,
  );
}

function promotionKey(owner: string, name: string, options: RepoRequestOptions): string {
  return JSON.stringify([
    canonicalProvider(options.provider),
    resolvedPlatformHost(options.provider, options.host),
    owner,
    name,
  ]);
}

function repoMutationKey(owner: string, name: string, options: RepoRequestOptions): string {
  return `repo:${promotionKey(owner, name, options)}`;
}

function repoInputMutationKey(input: RepoInput): string {
  const host = input.host ?? input.platform_host;
  const options: RepoRequestOptions = {
    provider: input.provider,
    ...(host === undefined ? {} : { host }),
  };
  if (input.owner && input.name) {
    return repoMutationKey(input.owner, input.name, options);
  }

  const path = (input.repo_path ?? "").replace(/^\/+|\/+$/g, "");
  const separator = path.lastIndexOf("/");
  const owner = separator < 0 ? "" : path.slice(0, separator);
  const name = separator < 0 ? path : path.slice(separator + 1);
  return repoMutationKey(owner, name, options);
}

function settingsMutationKeys(request: UpdateSettingsRequest): readonly string[] {
  const keys = Object.keys(request).map((field) => `settings:${field}`);
  return keys.length === 0 ? ["settings"] : keys;
}

interface RetainedSettingsUncertainty {
  readonly operation: string;
  readonly failure: SettingsTransportError;
}

export const SettingsWorkflowLive = Layer.effect(SettingsWorkflow)(
  Effect.gen(function* () {
    const api = yield* GeneratedApi;
    const startup = yield* StartupWorkflow;
    const uncertainPromotions = yield* Ref.make<ReadonlyMap<string, RepoPromotionStateUncertainError>>(new Map());
    const uncertainMutations = yield* Ref.make<ReadonlyMap<string, RetainedSettingsUncertainty>>(new Map());
    const readSettings = (operation: string) =>
      retryIdempotentRead(api.execute(operation, (signal) => api.client.SettingsService.getSettings({ signal })));
    const retainUncertainty = (keys: readonly string[], uncertainty: RetainedSettingsUncertainty) =>
      Ref.update(uncertainMutations, (entries) => {
        const next = new Map(entries);
        for (const key of keys) next.set(key, uncertainty);
        return next;
      });
    const clearUncertainty = (keys: readonly string[]) =>
      Ref.update(uncertainMutations, (entries) => {
        const next = new Map(entries);
        for (const key of keys) next.delete(key);
        return next;
      });
    const reconcileRetainedUncertainty = Effect.fn("SettingsWorkflow.reconcileRetainedUncertainty")(function* (
      keys: readonly string[],
    ) {
      const entries = yield* Ref.get(uncertainMutations);
      const retained = keys.map((key) => entries.get(key)).find((entry) => entry !== undefined);
      if (retained === undefined) return Option.none<SettingsSnapshot>();
      const settings = yield* readSettings(`GET /settings before retrying uncertain ${retained.operation}`).pipe(
        Effect.mapError(
          (reconciliationFailure) =>
            new SettingsMutationStateUncertainError({
              operation: retained.operation,
              failure: retained.failure,
              reconciliationFailure,
            }),
        ),
      );
      yield* clearUncertainty(keys);
      return Option.some(settings);
    });
    const recoverSettingsMutation = Effect.fn("SettingsWorkflow.recoverMutation")(function* (
      operation: string,
      keys: readonly string[],
      failure: SettingsTransportError,
      committed: (settings: SettingsSnapshot) => boolean,
    ) {
      const settings = yield* readSettings(`GET /settings after uncertain ${operation}`).pipe(
        Effect.mapError(
          (reconciliationFailure) =>
            new SettingsMutationStateUncertainError({ operation, failure, reconciliationFailure }),
        ),
        Effect.tapError(() => retainUncertainty(keys, { operation, failure })),
      );
      yield* clearUncertainty(keys);
      return committed(settings) ? settings : yield* Effect.fail(failure);
    });
    const runRecoverableSettingsMutation = Effect.fn("SettingsWorkflow.runRecoverableMutation")(function* <A>(
      operation: string,
      keys: readonly string[],
      request: Effect.Effect<A, SettingsTransportError>,
      committed: (settings: SettingsSnapshot) => boolean,
      fromSnapshot: (settings: SettingsSnapshot) => A,
    ) {
      const retained = yield* reconcileRetainedUncertainty(keys);
      if (Option.isSome(retained) && committed(retained.value)) return fromSnapshot(retained.value);
      return yield* request.pipe(
        Effect.catchTag("TransientTransportError", (failure) =>
          recoverSettingsMutation(operation, keys, failure, committed).pipe(Effect.map(fromSnapshot)),
        ),
      );
    });
    const persist = Effect.fn("SettingsWorkflow.persist")(function* (command: SettingsCommand) {
      return yield* Effect.gen(function* () {
        switch (command._tag) {
          case "Partial": {
            const request = yield* Effect.sync(command.request);
            const settings = yield* runRecoverableSettingsMutation(
              "settings save",
              settingsMutationKeys(request),
              api.execute("PUT /settings", (signal) => api.client.SettingsService.updateSettings(request, { signal })),
              (snapshot) => settingsMatchRequest(snapshot, request),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "Fleet": {
            const fleet = yield* runRecoverableSettingsMutation(
              "fleet settings save",
              ["fleet"],
              api.execute("PUT /settings/fleet", (signal) =>
                api.client.SettingsService.updateFleetSettings(command.request, { signal }),
              ),
              (settings) => fleetMatchesRequest(settings.fleet, command.request),
              (settings) => settings.fleet,
            );
            return fleetCommandResult(fleet);
          }
          case "CreateRepoPreset": {
            const settings = yield* runRecoverableSettingsMutation(
              "repository preset create",
              ["settings:repo_presets"],
              api.execute("POST /settings/repo-presets", (signal) =>
                api.client.SettingsService.createRepoPreset(command.preset, { signal }),
              ),
              (snapshot) =>
                snapshot.repo_presets.some(
                  (preset) => preset.name === command.preset.name && sameValue(preset.repos, command.preset.repos),
                ),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "UpdateRepoPreset": {
            const settings = yield* runRecoverableSettingsMutation(
              "repository preset update",
              ["settings:repo_presets"],
              api.execute("PUT /settings/repo-presets/{name}", (signal) =>
                api.client.SettingsService.updateRepoPreset(
                  { name: command.name },
                  { repos: [...command.repos] },
                  { signal },
                ),
              ),
              (snapshot) =>
                snapshot.repo_presets.some(
                  (preset) => preset.name === command.name && sameValue(preset.repos, command.repos),
                ),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "DeleteRepoPreset": {
            const settings = yield* runRecoverableSettingsMutation(
              "repository preset delete",
              ["settings:repo_presets"],
              api.execute("DELETE /settings/repo-presets/{name}", (signal) =>
                api.client.SettingsService.deleteRepoPreset({ name: command.name }, { signal }),
              ),
              (snapshot) => !snapshot.repo_presets.some((preset) => preset.name === command.name),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "AddRepo": {
            const settings = yield* runRecoverableSettingsMutation(
              "repository add",
              [repoMutationKey(command.owner, command.name, command.options)],
              api.execute("POST /repos", (signal) =>
                api.client.SettingsService.addRepo(
                  { ...command.options, owner: command.owner, name: command.name },
                  { signal },
                ),
              ),
              (snapshot) => containsExactRepo(snapshot, command.owner, command.name, command.options),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "RemoveRepo": {
            const ref = repoRef(command.owner, command.name, command.options);
            yield* runRecoverableSettingsMutation(
              "repository removal",
              [repoMutationKey(command.owner, command.name, command.options)],
              api.execute("DELETE repository", (signal) =>
                providerUsesHostRoute(ref)
                  ? api.client.SettingsService.deleteRepoOnHost({ ...providerHostRouteParams(ref) }, { signal })
                  : api.client.SettingsService.deleteRepo({ ...providerRouteParams(ref) }, { signal }),
              ),
              (settings) => !containsExactRepo(settings, command.owner, command.name, command.options),
              () => undefined,
            );
            return repoRemovedResult();
          }
          case "RefreshRepo": {
            const ref = repoRef(command.owner, command.name, command.options);
            const settings = yield* retryIdempotentRead(
              api.execute("POST repository refresh", (signal) =>
                providerUsesHostRoute(ref)
                  ? api.client.SettingsService.refreshRepoOnHost({ ...providerHostRouteParams(ref) }, { signal })
                  : api.client.SettingsService.refreshRepo({ ...providerRouteParams(ref) }, { signal }),
              ),
            );
            return settingsCommandResult(settings);
          }
          case "UpdateRepoWorktreeBase": {
            const ref = repoRef(command.owner, command.name, command.options);
            const settings = yield* runRecoverableSettingsMutation(
              "repository worktree-base save",
              [repoMutationKey(command.owner, command.name, command.options)],
              api.execute("PUT repository worktree base", (signal) =>
                providerUsesHostRoute(ref)
                  ? api.client.SettingsService.updateRepoWorktreeBaseOnHost(
                      { ...providerHostRouteParams(ref) },
                      { worktree_base_path: command.worktreeBasePath },
                      { signal },
                    )
                  : api.client.SettingsService.updateRepoWorktreeBase(
                      { ...providerRouteParams(ref) },
                      { worktree_base_path: command.worktreeBasePath },
                      { signal },
                    ),
              ),
              (snapshot) =>
                exactRepoWorktreeBaseMatches(
                  snapshot,
                  command.owner,
                  command.name,
                  command.options,
                  command.worktreeBasePath,
                ),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "UpdateRepoUIVisibility": {
            const ref = repoRef(command.owner, command.name, command.options);
            const settings = yield* runRecoverableSettingsMutation(
              "repository UI visibility save",
              [repoMutationKey(command.owner, command.name, command.options)],
              api.execute("PUT repository UI visibility", (signal) =>
                providerUsesHostRoute(ref)
                  ? api.client.SettingsService.updateRepoUiVisibilityOnHost(
                      { ...providerHostRouteParams(ref) },
                      { hidden: command.hidden },
                      { signal },
                    )
                  : api.client.SettingsService.updateRepoUiVisibility(
                      { ...providerRouteParams(ref) },
                      { hidden: command.hidden },
                      { signal },
                    ),
              ),
              (snapshot) =>
                exactRepoUIVisibilityMatches(snapshot, command.owner, command.name, command.options, command.hidden),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "BulkAddRepos": {
            const settings = yield* runRecoverableSettingsMutation(
              "bulk repository add",
              command.repos.map(repoInputMutationKey),
              api.execute("POST /repos/bulk", (signal) =>
                api.client.RepositoriesService.bulkAddRepos({ repos: [...command.repos] }, { signal }),
              ),
              (snapshot) => command.repos.every((repo) => containsRepoInput(snapshot, repo)),
              (snapshot) => snapshot,
            );
            return settingsCommandResult(settings);
          }
          case "PromoteRepo": {
            const owner = command.repo.owner;
            const name = command.repo.name;
            const options: RepoRequestOptions =
              command.repo.host === undefined
                ? { provider: command.repo.provider }
                : { provider: command.repo.provider, host: command.repo.host };
            const ref = repoRef(owner, name, options);
            const key = promotionKey(owner, name, options);
            let exactRepoAlreadyAdded = command.exactRepoAlreadyAdded;
            const previousUncertainty = (yield* Ref.get(uncertainPromotions)).get(key);
            if (previousUncertainty !== undefined) {
              const reconciled = yield* readSettings("GET /settings before retrying uncertain promotion").pipe(
                Effect.mapError(
                  (reconciliationFailure) =>
                    new RepoPromotionStateUncertainError({
                      failure: previousUncertainty.failure,
                      rollbackFailure: previousUncertainty.rollbackFailure,
                      reconciliationFailure,
                    }),
                ),
              );
              exactRepoAlreadyAdded = containsExactRepo(reconciled, owner, name, options);
              yield* Ref.update(uncertainPromotions, (entries) => {
                const next = new Map(entries);
                next.delete(key);
                return next;
              });
            }
            if (!exactRepoAlreadyAdded) {
              yield* runRecoverableSettingsMutation(
                "promotion repository add",
                [repoMutationKey(owner, name, options)],
                api.execute("POST /repos/bulk for promotion", (signal) =>
                  api.client.RepositoriesService.bulkAddRepos({ repos: [command.repo] }, { signal }),
                ),
                (settings) => containsExactRepo(settings, owner, name, options),
                (settings) => settings,
              );
            }
            const settings = yield* api
              .execute("PUT promoted repository worktree base", (signal) =>
                providerUsesHostRoute(ref)
                  ? api.client.SettingsService.updateRepoWorktreeBaseOnHost(
                      { ...providerHostRouteParams(ref) },
                      { worktree_base_path: command.worktreeBasePath },
                      { signal },
                    )
                  : api.client.SettingsService.updateRepoWorktreeBase(
                      { ...providerRouteParams(ref) },
                      { worktree_base_path: command.worktreeBasePath },
                      { signal },
                    ),
              )
              .pipe(
                Effect.catch((failure): Effect.Effect<SettingsSnapshot, RepoPromotionFailure> => {
                  if (exactRepoAlreadyAdded) return Effect.fail(failure);
                  return api
                    .execute("DELETE promoted repository after worktree-base failure", (signal) =>
                      providerUsesHostRoute(ref)
                        ? api.client.SettingsService.deleteRepoOnHost({ ...providerHostRouteParams(ref) }, { signal })
                        : api.client.SettingsService.deleteRepo({ ...providerRouteParams(ref) }, { signal }),
                    )
                    .pipe(
                      Effect.matchEffect({
                        onFailure: (rollbackFailure): Effect.Effect<never, RepoPromotionFailure> =>
                          retryIdempotentRead(
                            api.execute("GET /settings after uncertain promotion rollback", (signal) =>
                              api.client.SettingsService.getSettings({ signal }),
                            ),
                          ).pipe(
                            Effect.mapError(
                              (reconciliationFailure) =>
                                new RepoPromotionStateUncertainError({
                                  failure,
                                  rollbackFailure,
                                  reconciliationFailure,
                                }),
                            ),
                            Effect.tapError((uncertainty) =>
                              Ref.update(uncertainPromotions, (entries) => new Map(entries).set(key, uncertainty)),
                            ),
                            Effect.flatMap(
                              (settings): Effect.Effect<never, RepoPromotionFailure> =>
                                containsExactRepo(settings, owner, name, options)
                                  ? Effect.fail(new RepoPromotionRollbackError({ failure, rollbackFailure, settings }))
                                  : Effect.fail(failure),
                            ),
                          ),
                        onSuccess: (): Effect.Effect<never, RepoPromotionFailure> => Effect.fail(failure),
                      }),
                    );
                }),
              );
            return settingsCommandResult(settings);
          }
        }
      }).pipe(Effect.ensuring(startup.invalidate));
    });
    const queue = yield* makeOrderedCommandQueue("settings writes", persist);
    const submitSettings = (command: SettingsCommand) =>
      queue
        .submit(command)
        .pipe(
          Effect.flatMap((result) =>
            result._tag === "Settings"
              ? Effect.succeed(result.settings)
              : Effect.die(new Error(`Settings command ${command._tag} returned ${result._tag}`)),
          ),
        );
    return {
      persist: (request: () => UpdateSettingsRequest) => submitSettings({ _tag: "Partial", request }),
      updateFleet: (request: FleetSettingsUpdate) =>
        queue
          .submit({ _tag: "Fleet", request })
          .pipe(
            Effect.flatMap((result) =>
              result._tag === "Fleet"
                ? Effect.succeed(result.fleet)
                : Effect.die(new Error(`Fleet command returned ${result._tag}`)),
            ),
          ),
      createRepoPreset: (preset) => submitSettings({ _tag: "CreateRepoPreset", preset }),
      updateRepoPreset: (name, repos) => submitSettings({ _tag: "UpdateRepoPreset", name, repos }),
      deleteRepoPreset: (name) => submitSettings({ _tag: "DeleteRepoPreset", name }),
      addRepo: (owner, name, options) => submitSettings({ _tag: "AddRepo", owner, name, options }),
      removeRepo: (owner, name, options) =>
        queue
          .submit({ _tag: "RemoveRepo", owner, name, options })
          .pipe(
            Effect.flatMap((result) =>
              result._tag === "RepoRemoved"
                ? Effect.void
                : Effect.die(new Error(`Repository removal returned ${result._tag}`)),
            ),
          ),
      refreshRepo: (owner, name, options) => submitSettings({ _tag: "RefreshRepo", owner, name, options }),
      updateRepoWorktreeBasePath: (owner, name, options, worktreeBasePath) =>
        submitSettings({ _tag: "UpdateRepoWorktreeBase", owner, name, options, worktreeBasePath }),
      updateRepoUIVisibility: (owner, name, options, hidden) =>
        submitSettings({ _tag: "UpdateRepoUIVisibility", owner, name, options, hidden }),
      previewRepos: (owner, pattern, options) =>
        api.execute("POST /repos/preview", (signal) =>
          api.client.RepositoriesService.previewRepos({ ...options, owner, pattern }, { signal }),
        ),
      bulkAddRepos: (repos) => submitSettings({ _tag: "BulkAddRepos", repos }),
      promoteRepo: (repo, worktreeBasePath, exactRepoAlreadyAdded) =>
        submitSettings({ _tag: "PromoteRepo", repo, worktreeBasePath, exactRepoAlreadyAdded }),
    };
  }),
);

export function settingsErrorMessage(failure: SettingsError): string {
  switch (failure._tag) {
    case "ApiProblemError":
      return failure.problem.detail ?? failure.problem.title ?? "Failed to save settings";
    case "CommandQueueClosed":
      return "Settings are no longer available";
    case "TransientTransportError":
      return "Could not reach Kenn Forge";
    case "RepoPromotionRollbackError":
      return `${settingsErrorMessage(failure.failure)}; rollback failed: ${settingsErrorMessage(failure.rollbackFailure)}`;
    case "RepoPromotionStateUncertainError":
      return `${settingsErrorMessage(failure.failure)}; repository state could not be confirmed. Reload settings before retrying`;
    case "SettingsMutationStateUncertainError":
      return `${settingsErrorMessage(failure.failure)}; saved state could not be confirmed. Reload settings before retrying`;
  }
}
