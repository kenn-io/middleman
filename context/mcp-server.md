# MCP Companion

- MCP is an optional daemon-owned secondary listener enabled by
  `[mcp].enabled`; an omitted or zero port uses the backend port plus one, while
  a nonzero port overrides it (`internal/config/config.go::Config.MCPPort`).
- The listener is startup-bound and loopback-only. Discovery publishes
  `mcp_listen_addr` and `/api/ping` publishes `mcp_url`; changing listener
  settings requires a restart (`cmd/kenn-forge/main.go::bindDaemonListeners`,
  `internal/server/daemon_ping.go::Server.daemonPing`).
- Settings exposes the saved MCP listener/cache configuration separately from
  the boot-active endpoint and authentication policy. Saving MCP settings never
  hot-restarts the companion: report restart drift, keep showing an endpoint
  that remains active until restart, and provide only token-file guidance rather
  than returning the daemon bearer (`internal/server/settings_handlers.go::Server.buildLocalSettingsResponse`).
- `kenn-forge mcp quickstart` is the canonical agent discovery path for the
  active connector and saved restart drift; expose token paths and environment
  placeholders there, never bearer contents (`cmd/kenn-forge/mcp_cli.go::newMCPCommand`).
- MCP serves only `/mcp` over stateless Streamable HTTP. Authentication follows
  `[api].require_auth`; direct loopback peer, exact loopback authority, absent
  forwarding headers, and optional same-origin HTTP Origin are required
  (`internal/mcpserver/server.go::Server.HTTPHandler`,
  `internal/server/mcp_http.go::NewMCPHTTPGuard`).
- Tool implementations call the typed in-process Forge backend and never the
  daemon's public HTTP API (`internal/mcpserver/backend.go::Backend`,
  `internal/server/mcp_backend.go::Server.MCPBackend`).
- The backend composes provider and spoke-local interfaces. Provider reads and
  workflow state use the hub; clone, workspace, runtime, and
  agent-session tools stay local, and hub outages remain typed
  (`internal/mcpserver/backend.go::NewFederatedBackend`).
- Every MCP repository and item reference carries provider-verified
  `platform_repo_id`. Resolve the mutable route and reject it unless the stable
  ID still matches; never fall back to route-only identity
  (`internal/mcpserver/types.go::repoFilterInput.repositoryIdentity`,
  `internal/server/mcp_backend.go::mcpBackend.resolveRepository`).
- Provider reads resolve and recheck stable routes at the hub. Local
  route reads recheck the captured generation, and workspace writes carry that
  fence in context (`internal/server/mcp_backend.go::mcpBackend.confirmProviderRepositoryRoute`).
- Target MCP `2026-07-28`; do not advertise deprecated logging or catalog
  change notifications for the static surface (`internal/mcpserver/server.go::New`).
- Use only canonical `kenn-forge` command/resource/prompt names and
  `kenn_forge_*` tools; do not add aliases (`internal/mcpserver/server.go::Server.registerTools`).
- Workflow reads and writes are hub-owned provider-adjacent state;
  workspace state stays local. The surface must not expose provider mutations,
  arbitrary commands, terminal bytes, lifecycle cleanup, or `removed_upstream`
  items through workflow reads or writes
  (`internal/server/mcp_backend.go::mcpBackend.ListWorkflowStates`,
  `internal/server/mcp_backend.go::mcpBackend.SetWorkflowState`).
- Candidate output defaults to 25 and caps at 100. Apply candidate `item_types`
  to Activity before its 5,000-row internal safety window
  (`internal/mcpserver/tools_candidates.go::Server.findReviewCandidates`,
  `internal/server/mcp_backend.go::mcpBackend.ListActivity`).
- Treat only typed not-stacked responses as absence; surface other evidence
  failures with structured retry and ambiguity state
  (`internal/mcpserver/tools_stack.go::isStackAbsentError`).
- Tool failures set MCP `isError` and preserve kind, code, retryability,
  ambiguity, and details as JSON content plus `io.kenn.forge/error` metadata;
  never reduce typed backend failures to message-only errors
  (`internal/mcpserver/tools_read.go::wrapTool`).
- Each full-diff handoff stays within 10 MiB. The request-based LRU temp store
  defaults to 128 MiB through `[mcp].diff_cache_mb`. Stage replacements fully
  before mutating published state: a failed write never removes the same-name
  diff or evicts others; replacement is an atomic rename reusing the replaced
  entry's budget. Returned diff paths may be replaced or evicted by any later
  write. Represent files without text hunks using minimal Git-style headers,
  including `/dev/null` markers for empty added/deleted files and binary
  difference evidence for binary renames and copies; one empty text patch must
  not discard the rest of the diff. Temporary diff identity case-folds
  repository names only when the provider requires it
  (`internal/config/config.go::Config.MCPDiffCacheBytes`,
  `internal/mcpserver/difftmp.go::diffFileStore.write`,
  `internal/mcpserver/tools_diff.go::serializeDiffPatches`,
  `internal/mcpserver/tools_diff.go::canonicalDiffFileRef`).
- Initial-message attempts are process-local and retain the exact normalized
  prompt only in daemon memory. Same-daemon retries must match the live runtime
  target and prompt; daemon restart permits a fresh attempt
  (`internal/server/workspaceapi/initial_message.go::initialMessageAttempt`).
- Initial input requires an exact live agent runtime and matching target, LF or
  printable Unicode, and tracked bracketed paste for multiline text. Hook
  observation is not a submission precondition. If safe paste mode is not
  observed yet, release the no-write reservation and retry only that typed
  condition on the same runtime until the handoff deadline. Terminal writes
  honor the handoff context and do not hold the session lock while waiting.
  Other proven no-write rejection releases its reservation; a timed-out write
  that may have started remains uncertain
  (`internal/workspace/localruntime/manager.go::Manager.SubmitInitialMessage`,
  `internal/server/workspaceapi/initial_message.go::Handler.SubmitInitialMessageService`).
- Shutdown contract: stop MCP admission, wait the bounded grace period, cancel
  in-flight handler contexts and force-close connections, and only then close
  the MCP temp store and database; handlers must honor request-context
  cancellation (`cmd/kenn-forge/main.go::runMainShutdown`).
- MCP-created pull-request and issue workspaces suppress optional automatic
  assignment; ordinary UI omission preserves configured self-assignment
  (`internal/server/workspaceapi/routes_handlers.go::Handler.CreatePullWorkspace`,
  `internal/server/workspaceapi/routes_handlers.go::Handler.CreateIssueWorkspaceService`).
- MCP can create or reuse a pull-request, issue, or ad-hoc workspace and launch
  one new agent runtime with one initial message. It submits that message before
  waiting for the runtime's matching hook session. A resume names the existing
  workspace and runtime, repeats the same target and prompt through the
  runtime-scoped duplicate guard, and never launches another agent. Ambiguous
  workspace or runtime mutations are never retried or cleaned up. The exact
  `workspaceAlreadyExists` pull-workspace conflict receives one authoritative
  pull read and reuses the concurrent winner; only initial-message status
  receives a bounded, cancellation-independent read
  (`internal/mcpserver/tools_agent_spawn.go::Server.resolveOrCreatePRWorkspace`,
  `internal/mcpserver/tools_agent_spawn.go::Server.recoverInitialMessageStatus`).
- Agent-session inspection returns live agent runtimes separately from
  hook-authoritative sessions. `hook_observed=false` distinguishes a launched
  runtime awaiting its first hook from a workspace with no agent runtime
  (`internal/mcpserver/tools_agent.go::Server.listWorkspaceAgentSessions`).
- Follow-up MCP messages address one existing live agent runtime by workspace ID
  and runtime session key. They reuse the initial prompt's serialized
  bracketed-paste and Enter path, then return without launching, persisting, or
  waiting for hook activity
  (`internal/mcpserver/tools_agent.go::Server.sendAgentMessage`,
  `internal/workspace/localruntime/manager.go::Manager.SubmitAgentMessage`).
- An omitted MCP agent target selects the most-used available workspace agent
  from the prior 14 days; ties prefer recent use, then key, and empty history
  falls back to configured order (`internal/mcpserver/tools_agent_spawn.go::Server.defaultAgentTarget`).
- Handoff success and failure evidence uses `stage` plus
  `initial_message.state`; never add a separate `message_delivered` output or
  error detail (`internal/mcpserver/tools_agent_spawn.go::spawnWorkspaceWithAgentOutput`).
