---
name: context-sync
description: Use before every agent-created commit in kenn-forge, when context docs may have drifted after a large refactor, when a maintainer states a durable decision, or when an agent hits a gotcha that context should have prevented.
---

# Context Sync (kenn-forge)

Keep Kenn Forge's context aligned with durable maintainer decisions and cross-cutting
invariants. Commit capture is narrow; audits require explicit scope.

## Modes

- `--commit`: inspect only the intended commit, current conversation, and mapped context
  areas. Use before every agent-created commit.
- `<area>`: audit one Area Map entry.
- `--changed [--base <ref>]`: audit only areas mapped from changes since the supplied
  base, or the merge base with the repository default branch when omitted.
- `--all`: run the repository-wide audit, including orphan discovery, recent decision
  research, and cross-area invariant-guard review.
- `--check`: run `scripts/context-sync --check` and stop after structural validation.
- `--audit-claims`: verify anchored claims only within the selected area or changed
  areas. Combine with `--all` only when a repository-wide claim audit was requested.

With no mode or area, show this list and ask for explicit scope. Never infer `--all` from
a general request to improve or review context.

## Commit Mode

Use commit mode immediately before the repository's normal commit skill. Do not stage
files, construct a commit message, or create the commit.

1. Run `scripts/context-sync --check` before semantic inspection. Repair clear structural
   drift and rerun the check; ask the maintainer only when the correct routing cannot be
   derived from repository evidence.
2. Inspect `git status --short`, the intended diff against `HEAD`, relevant untracked
   files, and the current conversation. Ignore unrelated user changes.
3. Look only for durable signals introduced or clarified by this work:
   - a maintainer correction or explicit design decision;
   - a changed cross-cutting invariant, workflow, or policy;
   - a gotcha that existing context should have prevented.
4. Map each real signal to the smallest Area Map scope. Read only that area's topic docs,
   the governing `CLAUDE.md` section, and `context-guide.md`.
5. Apply the guide's grep test and per-addition budget.
6. Choose one outcome:
   - **Continue:** no durable signal exists, or existing context already captures it.
     Proceed silently without a marker or no-update report.
   - **Updated:** the required addition, correction, or removal is clear. Apply the
     smallest context edit, then return control to the commit workflow.
   - **Needs maintainer input:** materially different durable rules remain plausible
     after inspecting the evidence. Stop and ask one focused question.

Do not dispatch subagents, scan history, validate unrelated anchors, or audit invariant
guards in commit mode. Wording, routing, document placement, and clear factual deletions
are agent-resolvable work, not reasons to block. Block only for missing maintainer
knowledge that changes what future agents should do.

## Area Map

| Area | Topic doc(s) | Code it tracks |
|------|--------------|----------------|
| `agent-bootstrap` | `context/agent-bootstrap.md` | `.claude/settings.json`, `.codex/hooks.json` |
| `config` | `context/config-persistence.md` | `internal/config/` save paths |
| `platform` | `context/provider-architecture.md`, `context/platform-sync-invariants.md` | `platform/`, `internal/platformdb/` |
| `github-sync` | `context/github-sync-invariants.md` | `internal/github/` |
| `notifications` | `context/notifications-in-activity.md` | notification-owned paths in `internal/github/`, `internal/db/`, `internal/server/`, `frontend/`, and `packages/ui/` |
| `db` | `context/db-migrations.md` | `internal/db/`, `internal/db/migrations/` |
| `deferred-merge` | `context/deferred-merge.md` | deferred merge paths in `internal/server/` |
| `embeds` | `context/embeds.md` | embed routes, shell, and host bridge paths |
| `fleet` | `context/fleet-architecture.md`, `context/workspace-runtime-lifecycle.md` | `internal/federation/`, `internal/federationauth/`, `internal/fleet/`, `internal/server/fleetapi/`; federation-owned settings, frontend, and tests |
| `server` | `context/server-runtime.md`, `context/workspace-apis.md`, `context/workspace-runtime-lifecycle.md` | `cmd/kenn-forge/`, `internal/daemonruntime/`, `internal/runtimelock/`; server/workspace-owned paths in `internal/config/`, `internal/workspace/`, `internal/agentactivity/`, `internal/server/`, `internal/apiclient/generated/`, `frontend/`, and `packages/ui/`, including shared app and configuration files |
| `errors` | `context/error-handling.md` | error envelopes and frontend error branching |
| `retries` | `context/retries-and-backoffs.md` | retry, backoff, and single-flight paths |
| `testing` | `context/testing-basics.md`, `context/testing.md` | repository test files; `Makefile`, `.golangci.yml`, CI/test tooling, and helpers under `.github/workflows/`, `tools/`, `scripts/`, `internal/testutil/`, `frontend/`, and `packages/ui/` |
| `docs-authoring` | `context/docs-authoring.md` | user docs, screenshots, and Zensical configuration |
| `pull-requests` | `context/pull-request-workflow.md` | push and pull-request delivery workflow |
| `frontend` | `context/ui-design-system.md`, `context/ui-interaction-contracts.md`, `context/vscode-workflow-panel-interaction-spec.md` | `frontend/src/`, `packages/ui/src/` |
| `inline-review` | `context/inline-review-comments.md` | review-owned paths in `internal/db/`, `internal/github/`, `platform/`, `internal/platformdb/`, `internal/server/`, and `packages/ui/` |
| `repo-browser` | `context/repository-source-browser.md`, `context/platform-sync-invariants.md` | `internal/gitclone/repo_browser.go`, `internal/server/repobrowserapi/`; repository-browser frontend, routes, and tests |
| `mobile` | `context/mobile-ux.md` | frontend `/m` routes and phone-first components |
| `kata` | `context/kata-mode.md`, `context/workspace-apis.md` | `internal/kata/`; Kata-owned paths in `internal/server/`, `internal/config/`, and `frontend/`, including shared app and configuration files |
| `docs` | `context/docs-mode.md` | `internal/docs/`; Docs-owned paths in `internal/server/`, `internal/config/`, and `frontend/`, including shared app and configuration files |

Mode, notification, and inline-review scopes intentionally overlap generic areas. For
`--changed`, select every matching focused and generic area rather than choosing only the
broadest or most specific row.

## Audit Workflow

Follow every step in order for `<area>`, `--changed`, and `--all`. Do not collapse the
workflow into a structural check or a general code review.

### Step 0: Select the Scope

- `<area>` selects exactly one Area Map row.
- `--changed [--base <ref>]` resolves the merge base, maps changed files to candidate
  areas, then drops candidates with no durable context signal.
- `--all` selects every area and enables the explicitly broad work called out below.

When `--base` is omitted, use the merge base with the repository default branch. If
`--changed` leaves no selected area, report that no applicable audit scope remains and
stop. Never add areas merely because their code is adjacent.

### Step 1: Load the Guide

Read `context-guide.md` completely. Use its grep test, anchored-claim format, sorting
test, size limits, invariant-guard rules, and context-poisoning safeguards throughout the
audit. Do not propose or apply context changes before loading it.

### Step 2: Validate Routing

For `--all`, validate the full context manifest:

- every `context/*.md` and routed `docs/**/*.md` reference in `CLAUDE.md` exists;
- every context document an agent needs is reachable from `CLAUDE.md` or another
  reachable document;
- `AGENTS.md` resolves to `CLAUDE.md`.

For `<area>` and `--changed`, validate only the selected topic docs and their routing
references. Report broken or unreachable routing before continuing; unreachable context
is a high-priority finding.

For `--check`, run `scripts/context-sync --check`, report its result, and stop. The script
validates structure only; it is not a semantic audit or commit decision.

### Step 3: Scan the Selected Areas

Dispatch one read-only subagent per selected area when subagents are available. Give each
agent:

- the selected topic docs and governing `CLAUDE.md` sections;
- the mapped code paths from the Area Map;
- the anchored-claim and four-tag rules from `context-guide.md`.

Collect the same artifacts from every area:

- a proposed diff for the topic doc or governing `CLAUDE.md` section;
- one paragraph describing what drifted and why;
- knowledge-gap questions the code cannot answer;
- per-anchor results: resolves, moved, or gone;
- documented invariants that lack an appropriate Go guard or analyzer.

Do not dispatch area subagents in `--commit` mode. For focused audits, do not give agents
unselected docs or code paths.

### Step 4: Check Design Decisions

Compare the selected code and current conversation with `docs/adr/` and the selected
topic docs. Distill feature specs and implementation plans into current contracts in a
topic doc or `CLAUDE.md`; never preserve those transient artifacts by converting them
into ADRs. Reserve an ADR for independently required dated rationale that still binds
the architecture.

Only `--all` may search broader recent conversation history and recent commits. Focused
audits use the current conversation and selected change range only.

### Step 5: Research, Suggest, Then Ask

For every knowledge gap:

- research current best practice first when the question is general and technical;
- ask the maintainer directly when the answer is Kenn Forge-specific domain knowledge;
- present a confidence-tagged recommendation before asking for confirmation.

Do not ask a bare question that safe local inspection or focused research can answer.

### Step 6: Check Invariant Guards

For each invariant asserted by a selected context doc, confirm an appropriate Go test or
custom analyzer protects it. Examples include provider identity, capability gating, UTC
datetimes, stable error codes, Huma-only routing, migration history, and testify-only
assertions.

Flag documented-but-unguarded invariants and propose the smallest useful guard. When a
guard and a doc disagree, treat it as a high-priority finding. Only `--all` may review
guards outside the selected areas.

### Step 7: Present or Apply Diffs

Compress every addition to the guide's per-addition budget before presenting or applying
it. State the constraint future agents must respect, not the implementation walkthrough.

For `<area>` and `--changed`, apply clear scoped additions, factual corrections, moved
anchors, and removal of claims whose subjects are gone. Ask the maintainer only when the
correct durable rule cannot be derived from evidence.

For `--all`, present all proposed changes together. Separate deletions and modifications
from additions, justify each removal, and wait for approval before broad changes that
reinterpret policy.

### Step 8: Route Changes and Return to the Commit Workflow

When a new topic doc is created, route it from `CLAUDE.md` or another reachable context
doc. Keep `AGENTS.md` resolving to `CLAUDE.md`, keep the canonical provider list in its
single existing home, and add skill symlinks under both `.agents/skills/` and
`.claude/skills/` only when a new repository skill is created.

After context edits are ready, return to the repository's normal verification and commit
workflow. Do not stage, commit, amend, or push from this skill.

## Claim Audit

When `--audit-claims` is requested, read `claim-verifier.md` completely and add the
four-tag verification to Steps 3 and 7. Tag each anchored claim in the selected scope as
VERIFIED, OUTDATED, GONE, or UNVERIFIABLE. Refresh moved anchors, describe behavioral
deltas, and identify important unguarded invariants. Never widen claim auditing beyond
the selected scope unless `--all` was requested.
