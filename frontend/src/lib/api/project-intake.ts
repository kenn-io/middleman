import { Effect, Schema } from "effect";
import type {
  ProjectResponse as GeneratedProjectResponse,
  UserRepository as GeneratedUserRepository,
} from "./generated/models/index.js";

import { InvalidExternalPayload, type ApiProblemError, type TransientTransportError } from "./effect-errors.js";
import { executeGeneratedApiRequest, executeOpaqueGeneratedApiRequest, GeneratedApi } from "./generated-api.js";

export type ProjectResponse = GeneratedProjectResponse;
export type UserRepository = GeneratedUserRepository;
export interface DiscoveredUserRepository extends UserRepository {
  provider: "github";
  platform_host: string;
}

export interface ProjectIntakeOptions {
  readonly hostKey?: string | null;
}

export class InvalidProjectIntake extends Schema.TaggedError<InvalidProjectIntake>()("InvalidProjectIntake", {
  message: Schema.String,
}) {}

export type ProjectIntakeFailure =
  | ApiProblemError
  | InvalidExternalPayload
  | InvalidProjectIntake
  | TransientTransportError;

const RepoValidation = Schema.Struct({
  $schema: Schema.optionalKey(Schema.String),
  is_valid: Schema.Boolean,
  message: Schema.optionalKey(Schema.String),
  root_path: Schema.optionalKey(Schema.String),
});

const PlatformIdentity = Schema.Struct({
  name: Schema.String,
  owner: Schema.String,
  platform: Schema.String,
  platform_host: Schema.String,
});

const Project = Schema.Struct({
  $schema: Schema.optionalKey(Schema.String),
  created_at: Schema.String,
  default_branch: Schema.optionalKey(Schema.String),
  display_name: Schema.String,
  id: Schema.String,
  local_path: Schema.String,
  platform_identity: Schema.optionalKey(PlatformIdentity),
  updated_at: Schema.String,
});

const decodeRepoValidation = Effect.fn("ProjectIntake.decodeRepoValidation")(function* (input: unknown) {
  return yield* Schema.decodeUnknownEffect(RepoValidation)(input).pipe(
    Effect.mapError((cause) => InvalidExternalPayload.make({ operation: "decode fleet repository validation", cause })),
  );
});

export const decodeProjectResponse = Effect.fn("ProjectIntake.decodeProject")(function* (input: unknown) {
  const project: ProjectResponse = yield* Schema.decodeUnknownEffect(Project)(input).pipe(
    Effect.mapError((cause) => InvalidExternalPayload.make({ operation: "decode fleet project", cause })),
  );
  return project;
});

function normalizedHostKey(options?: ProjectIntakeOptions): string | undefined {
  const hostKey = options?.hostKey?.trim();
  return hostKey ? hostKey : undefined;
}

export function projectIntakeFailureMessage(failure: ProjectIntakeFailure): string {
  switch (failure._tag) {
    case "ApiProblemError":
      return failure.problem.detail ?? failure.problem.title ?? "The project request failed.";
    case "InvalidExternalPayload":
      return "The project service returned an invalid response.";
    case "InvalidProjectIntake":
      return failure.message;
    case "TransientTransportError":
      return "Couldn't reach the project service.";
  }
}

export const registerExistingProject = Effect.fn("ProjectIntake.registerExisting")(function* (
  path: string,
  options?: ProjectIntakeOptions,
) {
  const trimmed = path.trim();
  if (!trimmed) {
    return yield* Effect.fail(InvalidProjectIntake.make({ message: "Repository path is required." }));
  }

  const hostKey = normalizedHostKey(options);
  const validation = hostKey
    ? yield* executeOpaqueGeneratedApiRequest("validate fleet repository path", (client, signal) =>
        client.FleetService.validateFleetFilesystemRepo({ hostKey: hostKey }, { path: trimmed }, { signal }),
      ).pipe(Effect.flatMap(decodeRepoValidation))
    : yield* executeGeneratedApiRequest("validate repository path", (client, signal) =>
        client.SystemService.validateFilesystemRepo({ path: trimmed }, { signal }),
      );
  if (!validation.is_valid) {
    return yield* Effect.fail(InvalidProjectIntake.make({ message: validation.message ?? "Not a git repository." }));
  }

  const body = { local_path: validation.root_path ?? trimmed };
  return hostKey
    ? yield* executeOpaqueGeneratedApiRequest("register fleet project", (client, signal) =>
        client.FleetService.registerFleetProject({ hostKey: hostKey }, body, { signal }),
      ).pipe(Effect.flatMap(decodeProjectResponse))
    : yield* executeGeneratedApiRequest("register project", (client, signal) =>
        client.ProjectsService.registerProject(body, { signal }),
      );
});

export const cloneProject = Effect.fn("ProjectIntake.clone")(function* (
  url: string,
  path: string,
  branch?: string,
  options?: ProjectIntakeOptions,
) {
  const trimmedURL = url.trim();
  const trimmedPath = path.trim();
  const trimmedBranch = branch?.trim();
  if (!trimmedURL) {
    return yield* Effect.fail(InvalidProjectIntake.make({ message: "Repository URL is required." }));
  }
  if (!trimmedPath) {
    return yield* Effect.fail(InvalidProjectIntake.make({ message: "Destination path is required." }));
  }

  const hostKey = normalizedHostKey(options);
  const body = {
    url: trimmedURL,
    path: trimmedPath,
    ...(trimmedBranch ? { branch: trimmedBranch } : {}),
  };
  return hostKey
    ? yield* executeOpaqueGeneratedApiRequest("clone fleet project", (client, signal) =>
        client.FleetService.cloneFleetProject({ hostKey: hostKey }, body, { signal }),
      ).pipe(Effect.flatMap(decodeProjectResponse))
    : yield* executeGeneratedApiRequest("clone project", (client, signal) =>
        client.ProjectsService.cloneProject(body, { signal }),
      );
});

export interface UserRepositoryDiscoveryOptions {
  readonly provider: "github";
  readonly platformHost: string;
}

type UserRepositoryListEffect<A> = Effect.Effect<A, ApiProblemError | TransientTransportError, GeneratedApi>;

export function listUserRepositories(
  options: UserRepositoryDiscoveryOptions,
): UserRepositoryListEffect<DiscoveredUserRepository[]>;
export function listUserRepositories(): UserRepositoryListEffect<UserRepository[]>;
export function listUserRepositories(
  options?: UserRepositoryDiscoveryOptions,
): UserRepositoryListEffect<UserRepository[] | DiscoveredUserRepository[]> {
  return Effect.gen(function* () {
    const data = yield* executeGeneratedApiRequest("list user repositories", (client, signal) =>
      client.SystemService.listUserRepositories(
        {
          ...(options
            ? {
                provider: options.provider,
                platform_host: options.platformHost,
              }
            : {}),
          limit: 1000,
        },
        { signal },
      ),
    );
    const repositories = data.repositories ?? [];
    if (!options) return repositories;
    return repositories.map((repository) => ({
      ...repository,
      provider: options.provider,
      platform_host: options.platformHost,
    }));
  }).pipe(Effect.withSpan("ProjectIntake.listUserRepositories"));
}
