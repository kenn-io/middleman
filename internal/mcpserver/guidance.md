# Kenn Forge MCP Guidance

Use kenn-forge's MCP companion as a cached maintainer console. The companion
reads from the running daemon and does not force provider refreshes.

Recommended flow:

1. Call `kenn_forge_list_repos` first to discover valid repo filters, stable
   `platform_repo_id` values, and sync freshness. Copy the stable ID into every
   later repository or item reference; do not reconstruct references from routes.
2. Use `kenn_forge_find_review_candidates` to find recent PR and issue activity.
3. Inspect details only for plausible items with `kenn_forge_get_item_context`.
4. Use `kenn_forge_get_item_diff` to check the size and shape of a PR before
   claiming it. Request a full diff file only when the summary is not enough.
5. Consult `kenn_forge_get_stack_context` before claiming a stacked PR so review
   order respects the stack.
6. Prefer cached evidence over assumptions, and report stale cache signals or
   uncertainty.
7. Avoid provider writes. MCP mutations are limited to Kenn Forge-local workflow
   state and an explicitly requested local workspace/agent handoff.
8. Set workflow state only when the reason is clear. Include `expected_status`
   when marking an item so a stale agent run does not overwrite humans or other
   agents. Use `force: true` only for a deliberate unconditional local
   override.
9. Treat `awaiting_merge` as a PR-oriented state. Avoid setting it on issues
   unless the user explicitly asks for that state.
10. Launch a coding agent only when the user explicitly requests a handoff.
    Discover valid keys with `kenn_forge_list_agent_targets`, then call
    `kenn_forge_spawn_workspace_with_agent` with one PR, issue, or ad-hoc source
    and one initial message. Report the workspace and runtime identifiers even
    when a later stage fails.
11. Use `kenn_forge_list_workspace_agent_sessions` for fresh hook-reported
    coding session IDs. Do not infer IDs from terminal text. To continue work in
    a live runtime, call `kenn_forge_send_agent_message` with its workspace ID,
    runtime session key, and the follow-up message.

Example guidance flow:

```text
1. Call kenn_forge_find_review_candidates with since equal to the scheduler's
   last successful run.
2. For the top candidates, call kenn_forge_get_item_context.
3. Decide whether the activity needs human or agent review.
4. If claiming the item, call kenn_forge_set_item_workflow_state with
   status="reviewing", expected_status from the candidate row, and a short
   reason.
5. Report what was claimed and what was skipped.
```

Handoff flow:

```text
1. Call kenn_forge_list_agent_targets and select an available coding agent.
2. Call kenn_forge_spawn_workspace_with_agent once with the selected source,
   target, and initial message.
3. Do not retry an ambiguous workspace or runtime mutation. If prompt delivery
   or hook observation times out after a runtime identifier is returned, call
   the tool with resume, the returned workspace and runtime identifiers, and
   the same target and initial message. Resume never launches another runtime.
4. Report every returned workspace, runtime, prompt-delivery, and coding-session
   identifier or state.
5. For later instructions, call `kenn_forge_send_agent_message` with the
   workspace ID and runtime session key. It submits the message to that running
   agent and does not launch or resume anything.
```
