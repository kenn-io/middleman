import { Effect } from "effect";
import type { HostSummary as GeneratedHostSummary, Snapshot, WorkspaceSummary } from "./generated/models/index.js";

import { executeGeneratedApiRequest } from "./generated-api.js";

export type HostSummary = GeneratedHostSummary;
export type FleetSnapshot = Snapshot;
export type FleetWorkspaceSummary = WorkspaceSummary;

export const loadFleetSnapshot = Effect.fn("FleetSnapshot.load")(function* () {
  return yield* executeGeneratedApiRequest("load fleet snapshot", (client, signal) =>
    client.FleetService.getSnapshot({ include_peers: true }, { signal }),
  );
});

export const loadSnapshotHosts = Effect.fn("FleetSnapshot.loadHosts")(function* () {
  const data = yield* loadFleetSnapshot();
  return data.hosts ?? [];
});
