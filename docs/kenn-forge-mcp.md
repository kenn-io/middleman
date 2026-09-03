# Kenn Forge MCP

Forge can expose cached maintainer workflows to local MCP clients from the
running daemon. The companion reads the same in-process repository, activity,
workflow, and workspace services as the UI. It does not force provider refreshes
or add provider mutations.

## Enable the companion

Add this section to `~/.kenn/forge/config.toml`:

```toml
[mcp]
enabled = true
# port = 8092 # defaults to the main backend port plus one
```

Restart the daemon after changing either setting:

```sh
kenn-forge daemon restart
```

With the default backend port, the sessionless Streamable HTTP endpoint is:

```text
http://127.0.0.1:8092/mcp
```

The listener is loopback-only. It follows `[api].require_auth`: when API
authentication is enabled, send the daemon bearer token; when it is disabled,
the MCP endpoint does not require a token. Runtime discovery publishes the
resolved MCP listener and URL, and `kenn-forge daemon status --json` shows both
the listener address and the token file path.

Configure your MCP client with the HTTP URL. For clients that accept a JSON
server catalog, the shape is typically similar to:

```json
{
  "mcpServers": {
    "kenn-forge": {
      "type": "http",
      "url": "http://127.0.0.1:8092/mcp"
    }
  }
}
```

With `[api].require_auth` enabled, add the bearer token as an `Authorization`
header. The daemon mints the token into the `auth_token` file in its data
directory (mode 0600); read it from there rather than pasting it into shared
configuration:

```json
{
  "mcpServers": {
    "kenn-forge": {
      "type": "http",
      "url": "http://127.0.0.1:8092/mcp",
      "headers": {
        "Authorization": "Bearer ${KENN_FORGE_API_TOKEN}"
      }
    }
  }
}
```

Set the referenced environment variable from the token file, for example
`export KENN_FORGE_API_TOKEN="$(cat ~/.kenn/forge/auth_token)"`, using the
`token_path` reported by `kenn-forge daemon status --json`.

Earlier previews exposed a standalone `kenn-forge mcp` command with a stdio
transport. That command and transport are removed without a compatibility
shim: MCP is served only by the running daemon over the HTTP endpoint above,
so update any client configuration that launched the old command.

## Review workflow

Call `kenn_forge_list_repos` first to discover provider-aware repository filters,
stable `platform_repo_id` values, and sync freshness. Copy the returned stable
ID into every later repository or item reference; route-only references are not
accepted. Rediscover the current route after a repository rename. For review
triage:

1. Call `kenn_forge_find_review_candidates` with the desired time window and
   item types.
2. Inspect likely items with `kenn_forge_get_item_context`.
3. For pull requests, use `kenn_forge_get_item_diff` in summary mode first.
4. Use `kenn_forge_get_stack_context` before claiming work on a stacked pull
   request.
5. Set local workflow state with `expected_status` so a stale agent does not
   overwrite another actor. Use `force: true` only for a deliberate override.

`kenn_forge_search_items` finds quiet cached pull requests and issues that are
absent from recent activity. Candidate output defaults to 25 items and never
exceeds 100.

Diff files produced by `kenn_forge_get_item_diff` are temporary files on the
daemon host. Forge keeps the most recently requested files within the
`[mcp].diff_cache_mb` limit, which defaults to 128 MiB. Older files may be
deleted before daemon shutdown.

## Coding-agent handoff

Call `kenn_forge_list_agent_targets` to discover available coding agents. Then
call `kenn_forge_spawn_workspace_with_agent` with one source:

- a provider-aware pull request or issue; or
- a provider-aware repository with an optional ad-hoc branch.

The tool creates or reuses a workspace, launches a new agent runtime, submits
one initial message, and then waits for the resulting live hook session. It does
not clean up a workspace or runtime after a later failure. The response uses
`stage` and `initial_message.state` as the authoritative handoff evidence.

If prompt submission or hook observation times out after the response includes
a workspace ID and runtime session key, call the same tool with `resume` instead
of `source`. Pass those two IDs with the same `agent_target` and normalized
`initial_message`. Resume targets the existing runtime and does not launch a
second agent.

Use `kenn_forge_list_workspace_agent_sessions` to inspect live agent runtimes
and fresh coding sessions. A runtime with `hook_observed=false` has launched but
has not reported its first hook.

To send another instruction after the initial handoff, call
`kenn_forge_send_agent_message` with the persisted workspace ID, the live
runtime session key, and the message. The tool submits the prompt to that
running agent through the same runtime input path as the initial message. It
does not launch a runtime, resume a handoff, or wait for later agent activity.
Historical sessions and arbitrary terminal bytes are outside the MCP surface.

## Troubleshooting

If the endpoint is unavailable, confirm `[mcp].enabled = true`, restart the
daemon, and inspect daemon status for the resolved listener. An explicit MCP
port must differ from the backend port and must be free at startup.

A `401` response means `[api].require_auth` is enabled and the bearer token is
missing or incorrect. A `403` response means the request did not arrive as a
direct same-origin loopback request.
