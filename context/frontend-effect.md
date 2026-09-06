# Frontend Effect

Use this document for frontend async ownership, Effect workflows, services,
layers, schemas, typed errors, resource lifetimes, and Effect-specific tooling.
Interaction semantics remain in [`ui-interaction-contracts.md`](./ui-interaction-contracts.md),
and API error contracts remain in [`error-handling.md`](./error-handling.md).

## Installed References

Read the installed package that the frontend actually compiles against before
assuming an API from a newer Effect release:

- `frontend/node_modules/effect/README.md` for the installed package overview.
- `frontend/node_modules/effect/src/Effect.ts` for core constructors,
  composition, runtime, interruption, and scope APIs.
- `frontend/node_modules/effect/src/Stream.ts`, `Schema.ts`, `Layer.ts`, and
  `Schedule.ts` for those domains.
- `frontend/node_modules/@effect/platform-browser/` for browser platform APIs.
- `frontend/node_modules/@effect/vitest/` for Effect-aware tests.
- `frontend/node_modules/@effect/language-service/README.md` for the installed
  diagnostics, refactors, directives, and CLI.
- `frontend/node_modules/@effect/language-service/schema.json` for supported
  `tsconfig.json` plugin options.

The pinned Effect package does not currently contain an `AGENTS.md`; do not
invent or link that path. The language-service README says TypeScript 7 should
use `@effect/tsgo`, but kenn-forge uses TypeScript 5.9, so the installed language
service is the supported tool here.

## Application Ownership

- The browser app owns one `ManagedRuntime`, created by
  `frontend/src/lib/app/runtime.ts::makeAppRuntime` and supplied through Svelte
  context. Do not create feature or component runtimes.
- Accepted work belongs to an application-scoped workflow or service when it
  must survive route changes or component teardown. Components project state,
  capture user intent, and start synchronous commands; they do not own durable
  retry, ordering, uncertainty, or deadline state.
- Application ownership lasts for the browser runtime unless explicitly persisted.
  Reload may discard presentation and deferred launch intent, but persisted server
  authority must remain discoverable afterward.
- Use scoped acquisition and finalizers for listeners, streams, readers,
  abort controllers, timers, presenters, and workflow owners. Teardown must be
  explicit at the same lifetime boundary that acquired the resource.
- Publish an owner registry entry and install its finalizer in one
  uninterruptible acquisition handoff. After an ordered queue admits a
  non-idempotent command, pending-state publication and executor release are
  likewise one uninterruptible handoff.
- Workflow Actions is a plain app-scoped store that reads on demand and applies
  server events; it owns no polling loops, queues, or reconciliation. Dispatch
  follow-through (watching the returned run ID finish) lives on the server and
  arrives as `workflow_dispatch_progress` events keyed by `dispatch_id`
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::applyDispatchProgress`,
  `internal/server/workflowapi/dispatch_follow.go::Handler.followDispatch`).
- Reads are latest-wins per repository through generation counters; a stale catalog
  or run response never replaces newer data
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::selectWorkflow`).
- One dispatch cycle exists per workflow. Success, rejection, uncertainty, and
  definition conflict leave presentation only through the explicit new-cycle
  command; retry returns to fresh confirmation and never replays the POST
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::newDispatchCycle`).
- Definition-reload failures are cycle state separate from general read errors;
  a successful reload clears that workflow's cycle
  (`frontend/src/lib/stores/workflow-actions.svelte.ts::refreshCatalog`).
- Use Effect concurrency, queues, fibers, schedules, and interruption instead
  of bespoke Promise generations, overlapping timers, or boolean race guards.
  Preserve latest-wins, single-flight, ordered, or lossless semantics explicitly;
  these are different contracts.
- Timer polls use the shared visibility-aware helper: hidden documents stop
  polling and refresh at once when shown; event-driven refreshes ignore
  visibility (`frontend/src/lib/effect/poll-while-visible.ts::pollWhileVisible`).
- Run Effects only at host boundaries through the application runtime. Reusable
  business operations return `Effect` and normally use `Effect.fn`; inline
  orchestration may use `Effect.gen`.

## Errors And External Data

- Expected failures stay typed in the Effect error channel. Wrap browser,
  transport, and library failures in the existing tagged domain errors rather
  than returning `unknown`, global `Error`, or thrown exceptions.
- Decode untrusted event, storage, and non-generated payloads with Schema.
  Generated OpenAPI responses and request bodies should import the matching
  Orval model from `frontend/src/lib/api/generated/models/`, directly or through
  `frontend/src/lib/api/types.ts`; do not duplicate those shapes or add runtime
  guards around already-generated contracts.
- A non-idempotent transport failure may be uncertain. Retain a fence and read
  fresh authority before retrying. Definite rejection, uncertain outcome, and
  acknowledged success followed by refresh failure require different UI state.
- Presentation callbacks must not change command acknowledgement. Publish
  identity-scoped authoritative outcomes before component-liveness checks, and
  gate only local presentation on the current view.

## Svelte Boundary

- Keep Effect services and state machines in `.ts` or `.svelte.ts` modules when
  they own application work. Svelte components should derive visible state and
  delegate commands.
- When a Svelte `$effect` starts a runtime command, wrap the `runCommand` call in
  `untrack`; Effect fibers begin synchronously and can otherwise subscribe the
  Svelte effect to internal rune reads.
- An execution returned from `runCommand` is component-owned only when its work
  should stop with that component. Application-owned accepted work must not be
  returned as a component cleanup callback.

## Tooling

`frontend/tsconfig.json` loads `@effect/language-service` for editors. Use the
workspace TypeScript version. The normal frontend check also runs:

```sh
cd frontend
node node_modules/@effect/language-service/cli.js diagnostics \
  --project tsconfig.json --format text --severity error
```

This command analyzes the full TypeScript project without patching TypeScript.
Do not run `effect-language-service patch`: mutating installed compiler files is
not a reproducible repository check. Keep default warnings and suggestions as
editor guidance; correctness diagnostics are the non-mutating CI gate. A local
diagnostic override is acceptable only for a demonstrated tooling limitation,
and it must explain why TypeScript still preserves the relevant error and
requirements channels.

For every frontend Effect change, run the language-service diagnostics, the
affected Effect/Vitest tests, `svelte-check`, and the repository frontend lint.
