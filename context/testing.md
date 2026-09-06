# Testing

Use this document when choosing test boundaries or lanes, changing provider or
HTTP contract tests, or working on race and integration-test architecture. For
everyday Go test construction and commands, also read
[`context/testing-basics.md`](./testing-basics.md).

Test server constructors that create a Workspace manager must apply their
isolated test tmux command; they must never fall back to the host tmux server
(`internal/server/kata/test_helpers_test.go::newKataTestServer`).

Persistent-config server fixtures start the real config watcher. When a request
saves config, provider fakes must resolve the post-save repository set and tests
must await the config event before asserting converged state
(`internal/server/settings_test.go::setupTestServerWithConfigContent`).

## Live GraphQL validation

GraphQL query shape changes must be validated against GitHub's live GraphQL API before they are merged. The local test suite includes a gated live test:

```sh
KENN_FORGE_LIVE_GITHUB_TESTS=1 go test ./internal/github -run TestLiveGraphQLQueriesValidateAgainstGitHub -shuffle=on
```

The test uses `KENN_FORGE_GITHUB_TOKEN` first, then `GITHUB_TOKEN`. It intentionally skips unless `KENN_FORGE_LIVE_GITHUB_TESTS=1` is set because live validation consumes GitHub GraphQL rate limit and requires network access.

When changing structs, fields, aliases, fragments, pagination arguments, or nested selections used by `internal/github/graphql.go`, enable `KENN_FORGE_LIVE_GITHUB_TESTS=1` and run the live validation test in addition to the normal Go tests.

CI runs the live GraphQL validation as a separate Go test step using the workflow `GITHUB_TOKEN` only in trusted contexts, such as pushes to `main`, manual `workflow_dispatch` runs, and same-repository pull requests. The general pull request Go test step does not receive a GitHub token.

A GraphQL stub must answer pull-request and issue queries with only that
query's field: `githubv4` fails the whole query on an unexpected response key,
and sync then falls back to the REST index, so a test meant to exercise the
GraphQL path passes without ever reaching it. Assert on a value only the
GraphQL path can produce, or confirm the branch runs, before trusting such a
test. Note `go test` hides a passing package's stdout, so instrumentation
needs `-v` to be visible.

Integration-tagged top-level tests must use the `TestIntegration...` prefix;
Go build tags are additive, so the dedicated lane relies on its matching run
filter to avoid rerunning ordinary tests (`Makefile::test-integration`).

## CI path-gated test jobs

The CI workflow classifies changed paths once in `.github/workflows/ci.yml::detect_changes`
and uses that result to gate expensive test jobs. Keep the path buckets
runtime-oriented rather than extension-only: the backend bucket includes Go
files, Go modules, migrations, generated API client inputs, embedded web
assets, and integration fixtures; the Rust bucket includes root Cargo manifests
and the Rust workspace; the e2e bucket includes Playwright config, e2e tests,
scripts, and integration fixtures. Frontend unit, browser, and Playwright e2e
jobs run when either frontend paths change or backend paths change, because
backend/API behavior can affect the SPA contract even when no TypeScript files
moved. Manual `workflow_dispatch` forces all buckets on so maintainers can
request a full test pass.

External forks may consume an existing private Playwright image but must never
receive package-write authority. If the content-addressed image is absent, the
fork run fails until a trusted same-repository run publishes it
(`.github/workflows/ci.yml:76`).

## Dependency versions in guard tests

Guard tests must not assert a literal dependency version (for example
`assert.equal(pkg.devDependencies["@playwright/test"], "1.61.0")`); every
Renovate bump then fails lint until someone hand-edits the test. Assert the
shape that actually matters (exact pin, correct source of the dependency) and
leave cross-file version lockstep to a dedicated checker such as
`scripts/check-playwright-version.mjs`, which reads the current pin instead of
hardcoding one.

## Provider work

When adding or changing a provider, pick tests at the boundary where users would
notice the regression:

- provider package tests for API normalization, pagination, auth/header shape,
  typed platform errors, and capability flags;
- config tests for provider defaults, host normalization, nested paths,
  duplicate detection, and token selection;
- DB/query tests for provider-aware identity and provider ID reconciliation;
- server e2e tests with real SQLite for route payloads, settings/import flows,
  and capability-gated actions;
- frontend store/component tests for provider refs and generated route helpers;
- optional live/container tests when fakes cannot validate provider API drift.

Provider container fixture image bumps must keep every launch default and baked
image tag on one release, or local e2e and bake paths can test different
provider versions (`internal/server/gitlab_container_e2e_test.go::TestGitLabContainerE2E`).

- Container fixtures must advertise the host-mapped port in canonical provider URLs while
  keeping the service listener on its internal container port (`scripts/e2e/gitlab/docker-compose.yml:10`).

Regenerate OpenAPI and generated clients with `make api-generate` after Huma
route or API type changes.

Fleet setup tests inject every host boundary and use temporary homes/data; they
must never inspect or mutate the live daemon, service manager, network ingress,
or credentials (`internal/fleetsetup/setup_test.go`).

## Frontend test lane selection

Do not treat Playwright or full-stack e2e as a universal "must have" for every
visible frontend fix. Pick the narrowest lane that observes the behavior's real
owner:

- Use Vitest component/store tests for UI-owned data flow, filtering, sorting,
  hidden/disabled states, menu contents, route-derived view state, and app-shell
  behavior that does not depend on real browser layout. A jsdom test can cover
  an entire frontend-owned flow end to end; the required lane follows the
  boundary under test, not the “end-to-end” label.
- Use Vitest browser tests (`*.browser.svelte.ts` with `vitest-browser-svelte`)
  when the behavior needs a real browser DOM, native focus or keyboard behavior,
  computed styles, layout, localStorage, matchMedia, or other browser primitives,
  but does not need an external server or Playwright navigation flow. A browser
  test that mounts a component directly (not via `mountBrowserApp`, which loads
  it) must `import "./app.css"` before measuring geometry, or it measures
  content-box sizing and fallback tokens the app never ships. Text-dependent
  layout thresholds move between the CI container's fonts and a developer
  machine's, so assert the invariant that holds either side of a wrap boundary,
  never which side a chosen width lands on
  (`frontend/src/RoborevReviewDrawer.footer-layout.browser.svelte.ts`).
- Browser specs live beside their components under `frontend/src`; the browser
  project includes `src/**/*.browser.svelte.ts`, while the jsdom unit project
  also includes GitHub App setup tests (`frontend/vite.config.ts::jsdomUnitTestProject`).
- jsdom lacks `ResizeObserver` and the CSS Font Loading API; `frontend/src/test/setup.ts`
  stubs both as inert so kit components that remeasure on resize or font load
  (`AdaptiveActionGrid`, popover auto-reposition) mount in unit tests. Add a stub there,
  not per suite, when a kit bump reaches a new browser primitive.
- Frontend unit tests default to jsdom; promote a suite to the exact Node inventory only after
  A/B runs prove identical test identities and outcomes, so indirect browser dependencies fail safe
  (`frontend/vitest.node-files.ts::nodeUnitTestFiles`).
- Effect schedule tests must advance `TestClock` from the `it.effect` runtime; a fiber started through
  a separate `ManagedRuntime` retains the live clock and reintroduces wall-time waits
  (`frontend/src/lib/stores/roborev/daemon.svelte.test.ts:224`).
- Use Playwright only when the regression depends on the rendered browser
  surface: viewport behavior, screenshots/video, computed visual styles,
  clipping or overflow, drag/scroll geometry, canvas/xterm rendering, or a
  browser-engine difference that jsdom cannot model. Route changes and
  multi-step frontend logic do not require Playwright when jsdom can drive them.
- Use pure Go server/API tests when the disputed fact is produced by backend
  persistence, SQLite, sync, capabilities, normalization, route middleware, or
  wire serialization. Add full-stack Playwright only when that backend result
  must also be verified through a real rendered-browser boundary.
- Real-tmux Playwright tests observe user-visible state through the per-instance socket;
  never replace global key bindings, which can leak into developer sessions and prove only event receipt
  (`frontend/tests/e2e-full/00-inline-workspace-continuity.spec.ts::expectWheelScroll`).
- Real-tmux websocket tests retry asynchronous resize probes on a bounded timer, never per repaint; repaint-coupled
  input creates a feedback loop that can overflow subscriber buffers
  (`internal/server/api_test.go::TestWorkspaceRuntimeSessionTerminalTmuxBackedWebSocketE2E`).
- PTY-owner quick-exit coverage must assert the exact nonzero status through the
  HTTP-launched runtime WebSocket and confirm SQLite session cleanup. Order process
  exit before PTY EOF in the fixture; accepting unknown `-1` does not cover the handoff
  (`internal/server/api_test.go::TestWorkspaceRuntimePtyOwnerQuickExitReportsExactStatusE2E`).
- Retiring or shutting down e2e state must stop its private tmux server before slower
  asynchronous cleanup; interrupted runners otherwise leave test-owned daemons behind
  (`cmd/e2e-server/main.go::run`).
- Clipboard-race tests must emit OSC 52 through the attached tmux client, not print
  an application OSC 52 sequence that tmux blocks, and must assert the socket observed
  OSC 52 before trusting clipboard ordering (`frontend/tests/e2e-full/00-tmux-browser-clipboard.spec.ts::typeScheduledTmuxClipboardWrite`).
- Keep focus-click selection intent and delayed outside-copy revocation as separate real-tmux
  regressions; the delayed case must retain terminal DOM focus so `focusout` cannot mask a
  broken outside-pointerdown handler (`frontend/tests/e2e-full/00-tmux-browser-clipboard.spec.ts`).

Mock the API only when the behavior is owned by the frontend or the seeded
server cannot produce the state. Use `frontend/src/test/mockApiFetch.ts` rather
than forking the Playwright fixture, and do not assert backend-computed values
through hand-written frontend data.

A UI regression can be sufficiently covered by a backend/server test for the
real runtime path plus a component or Vitest browser test for presentation. Do
not require a duplicate full-stack browser test when it would only replay data
that is already proven at those two boundaries.

Agent lifecycle hooks have no full agent-launch E2E: kit covers config formats and
profile mappings, while kenn-forge tests all-profile selection, normalized relay, and HTTP
handling as independent external seams (`cmd/kenn-forge/agent_hook_cli_test.go::TestAgentHookRunNormalizesGeminiLifecyclePayload`, `internal/server/workspaceapi/agent_hook_test.go::TestReceiveAgentHookRecordsActivityAndGeneratesClaudeContext`).

A leaf renders a body per tab and hides the inactive ones, so which bodies exist
depends on the caller's `visible` gating, not on which tab is active: the
conversation body is ungated and stays mounted behind the diff, while the diff
body is gated and unmounts (asserting `.diff-view` has count 0 after switching to
Conversation is correct). The mounted-but-hidden conversation matters for
locators — its timeline renders review-thread diff snippets carrying the same
`pierre-diff`, `[data-diff-*]`, `gutter--selected`, and `.file-content` markup as
the diff, so scope diff locators to `.diff-area`.
Pane tab headers are `role="tab"` with an `aria-label`, so use
`getByRole("tab", { name })` — `getByRole(..., { hasText })` is not a valid
option and silently matches every tab.
The mock Playwright config's 30 s timeout covers the whole test, and every
`page.goto` is a full Vite dev-server load that slows several-fold under CI's
14 workers; keep a test to two navigations or split it, or it fails on the
first attempt and only passes on retry. (`frontend/playwright.config.ts`)
Sidebar item rows are `<button>`s whose accessible name concatenates every
descendant label, including indicator `aria-label`s such as "Stacked: 2/7", so
a page-wide `getByRole("button", { name })` for a detail chip also matches the
row; scope chip assertions to the chip's test id.

Every `@lucide/svelte/icons/<name>` import added anywhere in `frontend/src`
must also be added to the `optimizeDeps.include` list in
`frontend/vite.config.ts`. A missing entry passes locally on a warm Vite cache
but fails the browser-lane CI job on a cold one: the optimizer discovers the
icon mid-run, re-bundles, and the page reload breaks unrelated suites with
"Failed to fetch dynamically imported module". Verify with the grep documented
above that list in the config.

Vitest owns unhandled errors in the root runner, so `onUnhandledError` belongs
in the root `test` config rather than a browser project; keep any ignored error
exact by message and framework stack frame (`frontend/vite.config.ts:495`).

Playwright CI uses the private image from `ensure_playwright_image`; keep its
Playwright, Bun, and Vite+ pins in the recipe, cache only `/usr/local/install/cache`,
and materialize `node_modules` from the lockfile before invoking baked `vp`
(`.github/workflows/ci.yml::ensure_playwright_image`, `.github/docker/playwright/Dockerfile:3`).
Frontend unit tests use the runner's 14 guaranteed cores; the previous single-worker
cap was for the retired memory-constrained runner (`frontend/vite.config.ts::resolveUnitTestWorkers`).
Threaded unit tests must not start and stop a complete Vite/Rolldown dev server merely
to exercise plugin middleware; native handles can survive worker teardown under CI
concurrency (`frontend/src/lib/dev/healthcheckPlugin.test.ts::startServer`).
When a frontend unit run exits without a summary, download the
`frontend-unit-diagnostics` artifact: it contains any available Vitest output,
Python-collected wall and child CPU time, Node fatal reports, and pre/post
cgroup memory counters. If the exit is silent again, rerun it once with
repository variable `ACTIONS_RUNNER_DEBUG=true`, download the runner diagnostic
logs, then remove that variable.

Mock Playwright shares one Vite dev server, so cap CI workers at guaranteed cores;
burst-level worker counts starve navigation and reload requests into false 30-second
timeouts (`frontend/playwright.config.ts::ciWorkers`).

Non-container Vite+ jobs cache only Bun downloads with an exact OS, architecture, lockfile, and workspace-manifest key; do not use prefix restores, because stale package caches can poison a lockfile-correct install (`.github/workflows/ci.yml::build`).

Timezone-sensitive Vitest tests must not mutate `process.env.TZ` after workers
start; launch the test process with `TZ` or stub the locale formatter instead
(`frontend/src/lib/components/detail/operation-gates.test.ts:8`).

Full-stack e2e serves the frontend embedded in the e2e-server binary, not live
sources. The Playwright runner must prepare those assets and build one run-owned
`cmd/e2e-server` with VCS stamping disabled; workers inherit its direct path so signal
cleanup targets the server instead of a `go run` wrapper. An explicit binary remains
externally owned
and must not be rebuilt or removed (`frontend/tests/e2e-full/support/e2eServer.ts::ensureE2EServerBinary`).

- E2E provider decorators must embed concrete fixtures rather than base interfaces;
  narrowing erases optional capability interfaces (`cmd/e2e-server/main_test.go::TestE2EWorkflowClientExercisesProviderWorkflowContract`).

- Full-stack Playwright workers must publish child ownership in the shared tmux root;
  the root removes it only after every published child exits (`frontend/tests/e2e-full/support/e2eServer.ts::waitForSharedServerOwners`).
- Give independent full-stack boundary scenarios separate Playwright tests; a loop shares
  one timeout and obscures the failing boundary (`frontend/tests/e2e-full/00-inline-workspace-continuity.spec.ts::replayBoundaryCases`).
- Once e2e-server tmux shutdown starts, no new session may be admitted; cleanup waits
  for an admitted creation before killing the private server (`cmd/e2e-server/main.go::tmuxCreationGate`).
- Keep explicit PTY-owner test mode unwrapped so its missing tmux command remains
  unavailable to backend selection (`cmd/e2e-server/main.go::buildAppState`).

Playwright regressions that require a non-loopback listener must be opt-in only
in the isolated CI container; local runs must skip before binding because the
full-stack fixture exposes mutation and runtime endpoints (`frontend/tests/e2e-full/00-workspace-sidebar.spec.ts:172`).

Contextual Kata tests should mock the narrow daemon roster, reference, detail,
and launch routes. Shared task presentation belongs to the Kata package; Forge
tests assert package integration, association controls, refresh gating, and
per-task error isolation rather than duplicating Kata's component suite.

Roborev `hide_classify_jobs` e2e fixtures must cover skipped design rows and classify-typed auto-design rows.
Seed classify rows terminal unless testing worker mutation; live workers can rewrite queued/running rows during browser assertions (`internal/testutil/roborev_fixtures.go::seedRoborevMutationFixtures`).
Keep injected Roborev `panel_run` failures controlled until the assertion observes them; drawer/list refresh demand can immediately retry member fetches and clear transient panel errors (`frontend/src/lib/stores/roborev/jobs.svelte.ts::wantsPanelMembers`).
Roborev `/api/stream/events` is NDJSON, not SSE: use an abortable Fetch reader with bounded reconnects, and abort both pre-header and active-body requests on teardown (`frontend/src/lib/stores/roborev/jobs.svelte.ts::connectEventStream`).
Playwright retries that write to a persistent authority must use per-attempt mutation identities or reset state; a committed first attempt otherwise poisons its retry (`frontend/tests/e2e-full/roborev-e2e.spec.ts:923`).
The `e2e-roborev` daemon pin (`ROBOREV_REF` in `.github/workflows/ci.yml`) must be at or after the roborev commit the generated schema (`frontend/src/lib/api/roborev/generated/`) was produced from; a stale pin makes the daemon silently omit newer response fields while seed inserts still succeed, because the seeder creates its own schema.
Managed-clone initialization must exercise the pinned real Roborev CLI and daemon;
a fake hook writer cannot validate linked-worktree registration
(`scripts/run-roborev-e2e.sh:72`).
Container fixtures must not fetch operating-system packages from public mirrors
during CI. Build small fixture-only helpers from the repository and reuse an
already-required toolchain base image instead (`tests/integration/roborev/Dockerfile`).

Playwright waits must observe the state consumed by the next assertion. Direct API reads after an optimistic
mutation wait for its response; rendered assertions wait for rendered state, since route refinement can pair new
controls with old content (`frontend/tests/e2e-full/utc-maintainer-flows.spec.ts:90`, `frontend/tests/e2e-full/repo-browser.spec.ts:480`).
Tests that compress browser polling must patch the timer primitive used by the scheduler and await the response
that carries the expected state (`frontend/tests/e2e-full/ci-dropdown.spec.ts:40`).

Playwright suites with `route.fetch()` proxies must unregister routes with
`page.unrouteAll({ behavior: "ignoreErrors" })` before page teardown; background refetches can otherwise fail outside the completed test (`frontend/tests/e2e-full/diff-view.spec.ts::mockReviewThreadsOnPreviewMarkdown`).

Tests that redirect process-wide `slog` output must use a concurrency-safe writer; server background monitors can outlive their creating test and log during later assertions (`internal/server/tmux_wrapper_test.go::lockedBuffer`).

CI executes both Playwright suites in Chromium and Firefox. Frontend-owned
browser workflows using intercepted API responses belong in
`frontend/tests/e2e/` (`frontend/playwright.config.ts`); workflows that must
cross the built SPA, Go server, middleware, persistence, or provider fixture
boundaries belong in `frontend/tests/e2e-full/`
(`frontend/playwright-e2e.config.ts`). When a component change alters a
rendering contract, sweep both lanes for specs asserting the old contract
and run them locally — the mock lane can encode a contract without any of
its files appearing in the diff. Terminal-readiness assertions must be
renderer-agnostic (`canvas, .xterm-screen`): without WebGL — headless
Firefox — xterm.js silently falls back to its DOM renderer, which never
creates a canvas (`frontend/tests/e2e-full/00-inline-workspace-continuity.spec.ts:80`).
Derive xterm pointer coordinates from rendered cell geometry, not terminal
WebSocket dimensions; resize frames can lag an inline pane's rendered size
(`frontend/tests/e2e-full/00-tmux-browser-clipboard.spec.ts::dragTerminalCells`).
Canvas-backed xterm link tests must re-enter the target cell while polling; PTY output
can arrive before Chromium updates the painted link map
(`frontend/tests/e2e-full/00-tmux-browser-clipboard.spec.ts::hoverTerminalLink`).
When a real-tmux browser test moves one session between terminal hosts, close
the old WebSocket before mounting the next host and reassert mouse mode; concurrent
clients change tmux sizing and mode delivery (`frontend/tests/e2e-full/00-tmux-browser-clipboard.spec.ts:524`).
Reconnect tests must close and observe the WebSocket before enabling Playwright offline mode;
offline emulation can stall the close event and turn the assertion into a browser-network race
(`frontend/tests/e2e-full/00-inline-workspace-continuity.spec.ts:2231`).
Terminal-emulator protocol negotiation tests must use the PTY-owner e2e option;
tmux consumes application mode sequences and changes the boundary under test
(`frontend/tests/e2e-full/00-terminal-kitty-keyboard.spec.ts::kittyCursorProbeCommand`).

A full-stack test claiming a user-triggered mutation works must drive the actual
control and observe its request or visible result; `page.request` proves only the
API contract (`frontend/tests/e2e-full/detail-action-buttons.spec.ts:925`).

Failure-path e2e fixtures must inject the failure at the operation boundary
without mutating unrelated source data that controls whether the asserted row
still renders. For example, notification-read rollback coverage uses the
one-shot `/__e2e/notifications/fail-next-read` response; deleting its repository
also reloads activity and removes the notification row, turning the rollback
assertion into a timing race (`cmd/e2e-server/main.go`,
`frontend/tests/e2e-full/mobile-activity-notifications.spec.ts`).

An absence assertion against a disclosure's inner text is vacuous once the
disclosure is collapsed by default: the text is gone from the DOM whether or
not the underlying data actually cleared. Assert on the disclosure's own
`data-testid` instead, since that wrapper is gated on the data
(`frontend/src/lib/components/detail/MergeWarningsChip.svelte`).

## Huma API Contract

Every public operation in `/api/v1/openapi.json` must have explicit OpenAPI
metadata at the route registration site:

- stable kebab-case `OperationID`;
- short imperative `Summary`;
- exactly one tag from the API tag taxonomy enforced in
  `internal/server/route_metadata_test.go`.

Use `httpapi.DocumentOperation(...)` for Huma convenience helpers such as `huma.Get`
and `huma.Post`. Use inline `Summary`, `Tags`, and `OperationID` fields for
`huma.Register` blocks. Do not rely on Huma's generated summary or operation
ID; those names feed checked-in generated clients, so changing an
`OperationID` is a generated-client API change even when the HTTP path is
unchanged.

Health routes on the separate health Huma API intentionally disable OpenAPI and
docs output. Terminal and proxy routes registered through `Adapter().Handle`
must stay hidden or on a docs-disabled API unless they are promoted to public
REST operations with the same metadata and generation workflow.

For route metadata changes, run:

```sh
go test ./internal/server -run 'TestHumaContractMetadata|TestRouteMetadataWalker' -shuffle=on
make api-generate
```

Then review generated Go and TypeScript client diffs for operation-name
renames and update checked-in callers that use generated method/type names.

For long-lived transport registration changes, run:

```sh
go test ./internal/server -run 'TestTransportInventory|TestOTelTraceable' -shuffle=on
go run ./cmd/kenn-forge-transport-inventory
```

The generated inventory must come from Huma registration metadata; do not add
a hand-maintained route snapshot.

Do not duplicate full-stack e2e tests across default-host and
`/host/{platform_host}` route forms when the host route is only a generic
wrapper. Add host-specific e2e coverage only for custom host logic, route
parsing, or provider identity changes.

## Race test runtime

Treat `go test -race` runtime as a test architecture concern, not a CI-only
concern. The main levers are:

- keep large black-box flows in separate test packages so Go can schedule them
  as separate race test binaries;
- replace fixed sleeps with explicit events, callbacks, readiness channels, or
  short polling loops that check immediately before waiting;
- reuse migrated SQLite template databases for isolated non-migration tests;
- prepare SQLite fixtures before starting a subprocess timeout and pass the file
  to the child; process-local template caches are cold after exec (`internal/workspace/manager_test.go::TestSyncWorkspaceBaseBranchSurvivesQueuedReconciliationWriter`);
- add `t.Parallel` only after proving the test does not touch process-global
  state, fixed external resources, shared tmux sessions, or shared database
  files.

Use `make race-times` to get a local package timing baseline for the current
slow packages. CI also writes race timing JSON and summarizes slow packages and
tests in the `go test -race` job summary. When a PR regresses race runtime, use
the CI timing artifact rather than guessing from local timings alone.

New full-stack server tests should default to `t.Parallel()` when they build
their own `t.TempDir` filesystem, SQLite fixture, provider fake, and server
fixture. This especially applies to workspace/git e2e coverage where each test
clones its own bare repo and worktree root. Keep tests serial when they call
`t.Setenv`, mutate process-global state, rely on fixed ports or shared external
resources, or intentionally verify ordering against another test-visible shared
resource.

DB-backed server fixtures must drain `Server.Shutdown` before SQLite `t.TempDir`
removal. Black-box tests use `internal/testutil/servertest`; same-package tests
register shutdown cleanup after DB creation (`internal/testutil/servertest/servertest.go::New`,
`internal/server/api_test.go::gracefulShutdown`).

Disable Git auto-GC and auto-maintenance in synthetic repositories under
`t.TempDir`; detached maintenance can recreate files during fixture cleanup
(`internal/gitclone/commits_test.go::commitTestRun`).

Real-Git test packages use `gitsafe.RunIsolatedMain` in `TestMain`: one empty config per binary.
Use `Runner` where code strips Git variables, and `MutableRunner` only for global config
mutations (`internal/testutil/gitsafe/gitsafe.go::RunIsolatedMain`).
Script tests that launch Git from repository hooks must strip Git's repository-local
environment first; a nested `git init` can otherwise rewrite shared worktree config
(`scripts/context-sync.test.mjs::isolatedGitEnv`).

Real tmux and PTY tests can still run in parallel when each test owns its
session names, temp dirs, sockets, and cleanup. If the bottleneck is external
resource pressure rather than correctness, keep `t.Parallel()` and gate the
expensive section with a package-level `golang.org/x/sync/semaphore.Weighted`
instead of serializing the whole test.

- E2e processes must stop private tmux servers before bounded graceful shutdown;
  Go defers and Node `exit` callbacks cannot clean up after forced termination
  (`cmd/e2e-server/main.go::appState.stopTmux`).
- Playwright workers share one run-owned socket directory; new runs reap those
  whose recorded owner PID is dead
  (`frontend/tests/e2e-full/support/e2eServer.ts::ensureE2ETmuxDir`).
- Keep every child owned and the shared socket root intact through child exit;
  terminating children can still daemonize tmux after the first cleanup sweep
  (`frontend/tests/e2e-full/support/e2eServer.ts::shutdownOwnedServers`).
- Stale recovery may connect only to tmux roots, owner files, and sockets owned
  by the current user; matching a private test name is not ownership
  (`frontend/tests/e2e-full/support/e2eServer.ts::cleanupE2ETmuxDir`).
- Real-tmux Go test binaries install signal cleanup because termination skips
  code after `m.Run` and `t.Cleanup` (`internal/testutil/testsignal.Install`).
- Long-lived package-level tmux servers must use `testtmux.Owner`; its reaper
  reaps servers after uncatchable owner death, which signal cleanup cannot handle.
  (`internal/testutil/testtmux/reaper_unix.go::startOwnerReaper`)

Windows test binaries that launch restart-durable PTY processes must contain
their descendants in a kill-on-close Job Object; test timeouts bypass normal
Go cleanup (`internal/testutil/processjob/processjob_windows.go::ContainCurrentProcessTree`).

Keep splitting new high-volume tests into the existing black-box packages when
they do not need unexported internals:

- `internal/server/apitest` for HTTP API behavior through the generated client;
- `internal/server/workspacetest` for workspace, runtime, terminal, and
  tmux-heavy HTTP flows;
- `internal/github/syncertest` for exported syncer contract behavior;
- `internal/db/projecttest` for project-package DB behavior that can avoid the
  core `internal/db` package.

Leave tests in the source package when they exercise unexported helpers,
migration state, dirty database handling, or other internal invariants.

### Cross-boundary concurrency contracts

- Prove multi-query snapshots at the service transaction boundary; query-only tests cannot expose torn reads. (`internal/archive/report_service_test.go::TestArchiveServiceReportUsesOneSQLiteSnapshot`)
- Prove pause by blocking provider I/O, pausing, then releasing and asserting no cursor or dataset commit. (`internal/archive/service_test.go::TestArchiveServicePauseRejectsInFlightInventoryCommit`)
- Prove worker readiness, wake, and shutdown through the owning loop rather than direct service calls. (`internal/github/archive_lifecycle_test.go::TestArchiveWorkerAdvancesRealServiceAfterStart`)
- Prove cross-ingestion child replacement with a newer dataset followed by an older complete replacement; parent-only races do not cover destructive child deletes. (`internal/github/sync_test.go::TestNormalSyncRejectsChildrenAfterArchivePublishesNewerSnapshot`)
- Prove lock ownership with a `TryLock` probe while the owner is provably parked inside the guarded section; an event emitted before a blocking `Lock` call proves nothing, because the goroutine can be descheduled before acquiring. (`internal/workspace/localruntime/manager.go::CommandSessionStartLockHeldForTest`)

### Boundary-value contracts

- Exercise persistence at exact advertised record ceilings, including repeated replacement; small fixtures do not expose SQLite bind or batch-size failures. (`internal/db/queries_archive_test.go::TestArchiveIssuePublicationSupportsMaximumDatasetSize`)
- Inclusive-watermark refreshes must reopen every equal-timestamp observation;
  without a provider revision or complete fingerprint, later-than-watermark
  equality cannot prove stasis. (`internal/db/queries_archive_test.go::TestArchivePromptReopensEqualOrNewerObservations`)
- Snapshot race coverage must include equal provider timestamps through a real sync workflow and generated HTTP client; helper-only tests miss ordering gaps where child I/O begins before the parent revision is committed. (`internal/server/e2etest/archive_snapshot_race_test.go::TestIssueSyncCannotReplaceEqualTimestampArchiveSnapshotE2E`)
- Seeded full-stack provider fakes must return every child family represented as provider-owned seed data. Complete mirroring legitimately deletes absent comments/reviews, so a DB-only synthetic child row is not stable across background or explicit sync. (`internal/testutil/fixtures.go::SeedFixtures`)

### SQLite Fixtures

Use the copied-template database fixture for ordinary DB-backed tests that only
need a fresh migrated schema:

- outside `internal/db`, prefer `internal/testutil/dbtest.Open(t)`;
- inside `internal/db`, use the package-local `openTestDB(t)` from
  `fixture_test.go`;
- keep migration, legacy repair, dirty migration, and schema-history tests on
  `dbtest.OpenWithMigrationsAt(t, path)`, `db.Open`, or the package-local
  `openDBWithMigrations(t)`.

The template fixture migrates once, checkpoints WAL, copies the database file
into each test's `t.TempDir`, and opens the copy with `OpenPreparedForTest`.
That preserves per-test isolation without paying migration setup for every
fixture.

### Sleep And Timer Tests

Do not add sleeps as a synchronization mechanism. Prefer a channel closed by
the fake or callback that observed the exact event, then await it with
`require.Eventually` and scheduling headroom under `-race`. If the behavior is
inherently observable only by polling, check immediately, then use a short ticker
bounded by a context deadline. (`internal/github/syncertest/syncer_test.go::TestSyncerTriggerRunRunsRunOnce`)

Async cache tests must observe the baseline through the consumer boundary
before mutating inputs or advancing a fake clock; initial in-flight work can
otherwise publish stale data at the new time. (`internal/server/workspacetest/workspace_enrichment_wire_test.go::TestWorkspaceListReportsCommitsAheadBehindE2E`)

When server e2e tests chain `POST /api/v1/sync` with another ad-hoc sync
trigger, treat the HTTP 202 and DB row timestamps as intermediate observations.
`TriggerRun` is non-blocking and single-flight; wait for `/api/v1/sync/status`
to report `running=false` with a `last_run_at` before issuing the next trigger,
or the next trigger can race the still-running sync and be skipped.

A production timeout exercised by a test also bounds dialling the outbound
request, so a sub-100ms value lets a loaded runner abandon the probe before the
fake server ever receives it. Give such timeouts headroom over scheduling noise
and await the fake's observation instead of reading its counter synchronously
(`internal/server/e2etest/settings_test.go::awaitPeerRequest`).

Workspace fixtures with background monitors must seed observer inputs before
`server.New`; the pushed-head observer runs an immediate first pass and can
capture transitional fixture state (`internal/server/server.go::runWorkspacePushedHeadObserverLoop`).

`testing/synctest` is appropriate only when all goroutines and timers under test
are pure in-process work created inside the `synctest.Run` bubble. Good
candidates include fake-client backoff, cooldown, cancellation, and event-hub
tests. Do not use `synctest` around `httptest.Server`, WebSockets, tmux, PTYs,
git, shell commands, filesystem polling driven by external processes, or tests
that call `t.Run`, `t.Parallel`, or `t.Deadline` inside the bubble.
`synctest.Wait` is race-detector synchronization, so it is useful under
`go test -race` when the test is structurally eligible.

## HTTP testing discipline

A test of user-visible HTTP behavior is **wire-level** when the request flows
through `srv.ServeHTTP` (every middleware runs) and assertions read what a
client observes: status, response headers, and body bytes. The handler
function's return value is not consulted.

Two transports qualify:

- **`httptest.NewRecorder`** is the default for request/response tests, used
  by `internal/server/apitest/`. No `net.Conn`, so writes buffer until the
  handler returns and `Flush` does not push toward a reader — the recorder
  cannot honor streaming or hijack semantics.
- **`httptest.NewServer`** is required for streaming, hijack, or
  `Flush`-sensitive endpoints. Used by `internal/server/e2etest/` and the
  in-package `TestSSE_*` tests.

Defaults for new code:

- API request/response → `internal/server/apitest/` with the generated client;
  decoding through `generated.ErrorModel` catches OpenAPI drift.
- Streaming, hijack, `Flush`-sensitive → `internal/server/e2etest/`.
- Inputs the generated client cannot produce (wrong `Content-Type`, malformed
  body, preflight failure) → raw `http.Request` over the recorder; comment
  the reason.
- Direct handler-function calls (`s.handleSSE(w, r)`) skip routing and
  middleware. Allowed only to inject a fault into the `http.ResponseWriter`;
  comment the fault. The two `TestSSE_Terminates...` deadline tests are the
  legitimate exception.

Test cleanup must not delete a failing test that still covers repository-owned
behavior. Fix the test or production behavior; remove only obsolete assertions
or behavior already covered at the same boundary.

When mirrored frontend implementations share one behavior contract, run the owner-level
contract against every implementation; consumer suites retain only wiring, presentation,
and local differences (`frontend/src/lib/stores/comment-mutation-contract.ts::runCommentMutationContract`).

For TypeScript/Svelte cleanup, add tests only when user-visible behavior,
cross-module contracts, or an actual regression risk changes. Do not fabricate
large fake browser boundaries, console-spy cases, or component tests merely to
justify replacing assertions with better types. If the change is a local type
contract improvement and existing interaction tests still cover the behavior,
prefer focused typecheck/lint validation over new test scaffolding.

Handler-internal helper unit tests (URL parsing, label diffs, capability
resolution) stay as plain unit tests in `package server` and are out of
scope.

Bug classes wire-level catches:

| Bug class                                     | Why the wire matters                                 |
| --------------------------------------------- | ---------------------------------------------------- |
| Time serialization (`Z` vs `+00:00`)          | Response bytes, not `time.Time` before marshaling.   |
| Error missing from OpenAPI doc                | Generated client surfaces unknown status variants.   |
| Header rewritten or stripped by middleware    | `resp.Header`, not `w.Header()` inside the handler.  |
| Status overridden by middleware               | `resp.StatusCode`, not the handler return.           |
| Mutation guard short-circuits before dispatch | Only `srv.ServeHTTP` runs the full middleware chain. |
| SSE `Content-Type` / `Cache-Control` drift    | Real-socket read; the recorder is buffered.          |

Worked examples: `internal/server/e2etest/sse_contract_test.go` pins SSE
headers and the first cached `sync_status` frame;
`internal/server/apitest/mutation_guard_test.go` asserts the 403 response
for a cross-site browser mutation;
`internal/server/workspacetest/issue_workspace_conflict_test.go` reproduces
a 409 through `generated.ErrorModel` alongside the in-package original.

## Related context

- [`context/provider-architecture.md`](./provider-architecture.md) documents the
  provider package split and checklist for adding providers.
- [`context/platform-sync-invariants.md`](./platform-sync-invariants.md)
  documents provider identity and capability rules for GitHub, GitLab, and
  future providers.
- [`context/github-sync-invariants.md`](./github-sync-invariants.md) documents
  timeline freshness, SHA-sensitive CI, and fallback rules that usually
  determine which tests belong on a GitHub-specific sync change.
