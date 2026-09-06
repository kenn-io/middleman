# Claim Verifier — Subagent Instructions (kenn-forge)

You are a read-only subagent verifying the factual claims in a single context document
against the current state of kenn-forge's code. You MUST NOT modify any code or doc
files. Return findings; the parent run presents them to the maintainer.

## Inputs

- Path to one context document (a `context/*.md` topic doc, a section of `CLAUDE.md`, or
  a `docs/` spec).
- The anchored-claim format and four-tag scheme from `context-guide.md`.
- The worktree to verify against.

## Task

For each factual claim in the document:

1. Identify its anchor (`` `file.go::Symbol` ``, `` `file.go:line` ``, or a directory).
   If a factual claim has no anchor, note it as `MISSING-ANCHOR` and propose one.
2. Resolve the anchor with `rg` / file reads. Confirm the symbol, type, route,
   migration, or behavior still exists and matches what the claim asserts.
3. Tag the claim:
   - VERIFIED — found, accurate; if the symbol moved, give the refreshed anchor.
   - OUTDATED — found, but the claim no longer matches; include a `delta:` line stating
     what the code does now vs. what the doc says.
   - GONE — the referenced symbol/route/migration no longer exists; set anchor to `N/A`.
   - UNVERIFIABLE — behavioral/judgment claim that code cannot settle; include a
     `question:` line for the maintainer.

## Critical invariants — verify with extra care, tag CRITICAL

These are kenn-forge's load-bearing cross-cutting rules. When a document touches one,
re-check it directly against code rather than trusting the prose:

- **Provider identity tuple.** Claim that identity is `(platform, platform_host, owner,
  name)` everywhere and routes are provider-aware (`/pulls/{provider}/{owner}/{name}/
  {number}`, `/host/{platform_host}/...`). Re-check `platform/` (registry and types),
  `internal/platformdb/` (persistence), and representative `internal/server/` routes.
- **Capability gating.** Claim that provider differences go through `Capabilities()` and
  return typed `unsupported_capability` errors with no silent GitHub-only fallback.
  Re-check `platform/` capability declarations and their call sites before
  mutations.
- **GitHub-only isolation.** Claim that GraphQL bulk fetch / ETag recovery stay in
  `internal/github/` and remain optional around the neutral persistence path. Re-check
  `internal/github/` and confirm the neutral path still works without them.
- **UTC datetime policy.** Claim that timestamps are stored and emitted in UTC (RFC3339)
  with local conversion only in the Svelte layer. Cross-check against
  `docs/adr/0001-utc-datetime-policy.md` and storage/API boundaries.
- **Stable error envelopes.** Claim that clients branch on stable codes/details, not
  prose. Re-check `context/error-handling.md` against actual error construction in
  `internal/server/` and frontend error branching.
- **Canonical provider list.** The supported-provider set is stated once in `CLAUDE.md`.
  If any scanned doc restates the set, flag it as a poisoning/drift risk (OUTDATED).

## Return

Return a per-document summary, one line per claim:

`{anchor or N/A} — {TAG} — {claim summary}{; delta/question/refreshed-anchor as needed}`

End with a count line: `{doc}: V verified, O outdated, G gone, U unverifiable, M missing-anchor`.

## Constraints

- MUST NOT modify any code or documentation file.
- MUST attempt to resolve every anchor before tagging.
- MUST include a `delta:` line on every OUTDATED claim.
- MUST include a `question:` line on every UNVERIFIABLE claim.
- MUST propose an anchor for every MISSING-ANCHOR factual claim.
