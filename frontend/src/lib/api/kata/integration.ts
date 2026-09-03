import type {
  KataDaemonResponse,
  KataIssueReference as GeneratedKataIssueReference,
  KataLaunchTarget as GeneratedKataLaunchTarget,
  KataProjectMappingDiagnostic as GeneratedKataProjectMappingDiagnostic,
  KataProjectMappingsResponse as GeneratedKataProjectMappingsResponse,
  KataResolvedIssueReference as GeneratedKataResolvedIssueReference,
  KataWorkspaceTaskRequest,
  WorkspaceResponse,
} from "../generated/models/index.js";

import * as client from "../generated/index.js";

export type KataDaemonInfo = KataDaemonResponse;
export type KataIssueReference = GeneratedKataIssueReference;
export type KataResolvedIssueReference = GeneratedKataResolvedIssueReference;
export type KataReferenceSearch = (
  daemonID: string,
  query: string,
  signal?: AbortSignal,
) => Promise<readonly KataIssueReference[]>;
export type KataWorkspaceIdentity = KataWorkspaceTaskRequest;
export type KataWorkspaceResponse = WorkspaceResponse;
export type KataLaunchTarget = GeneratedKataLaunchTarget;
export type KataProjectMappingDiagnostic = GeneratedKataProjectMappingDiagnostic;
export type KataProjectMappingsResponse = GeneratedKataProjectMappingsResponse;

export async function fetchKataDaemons(signal?: AbortSignal): Promise<KataDaemonInfo[]> {
  const result = await client.KataService.listKataDaemons(signal ? { signal } : {});
  return result.daemons ?? [];
}

export async function searchKataReferences(
  daemonID: string,
  query: string,
  signal?: AbortSignal,
): Promise<KataIssueReference[]> {
  const result = await client.KataService.listKataReferences(
    { daemonId: daemonID },
    { q: query, limit: 50 },
    signal ? { signal } : {},
  );
  return result.issues;
}

export async function resolveKataIssueReference(
  daemonID: string,
  issueUID: string,
  signal?: AbortSignal,
): Promise<KataIssueReference> {
  const result = await client.KataService.listKataReferences(
    { daemonId: daemonID },
    { issue_uid: [issueUID], limit: 2 },
    signal ? { signal } : {},
  );
  const matches = result.issues.filter((candidate) => candidate.uid === issueUID);
  if (matches.length !== 1) throw new Error("Unable to resolve Kata issue.");
  return matches[0]!;
}

export async function resolveKataTextReference(
  daemonID: string,
  project: string | undefined,
  reference: string,
  signal?: AbortSignal,
): Promise<KataResolvedIssueReference> {
  const result = await client.KataService.resolveKataIssueReference(
    { daemonId: daemonID },
    { ...(project ? { project } : {}), ref: reference },
    signal ? { signal } : {},
  );
  return result;
}

export async function createOrOpenKataWorkspace(identity: KataWorkspaceIdentity): Promise<KataWorkspaceResponse> {
  const result = await client.KataService.createKataWorkspace(identity);
  return result;
}

export async function resolveKataLaunchTarget(daemonID: string, issueUID: string): Promise<KataLaunchTarget> {
  const result = await client.KataService.getKataLaunchTarget({ daemonId: daemonID, issueUid: issueUID });
  return result;
}

export async function getKataProjectMappings(daemonID?: string): Promise<KataProjectMappingsResponse> {
  const result = await client.KataService.getKataProjectMappings(
    daemonID ? { "X-Kenn-Forge-Kata-Daemon": daemonID } : undefined,
  );
  return result;
}
