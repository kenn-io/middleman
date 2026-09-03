# Database Migrations

Use this document before creating, editing, reviewing, or validating database
schema migrations.

- **Shipped migrations are immutable.** A migration has shipped when it exists on `origin/main`, a tag or release branch, or a production database; correct it with a new forward migration.
- **Each PR introduces at most one migration.** Amend a PR-local migration in place instead of stacking fix-ups.

`go run ./tools/migrationhistorycheck` enforces edits to the comparison base, duplicate migration numbers, and multiple new migrations. In GitHub's synthetic PR merge checkout, CI compares against the merge commit's first parent so migrations added to the target branch after the PR opened remain inherited; other PR and local-hook runs use the supplied comparison ref. This preserves one migration per stacked PR while local hooks inspect staged changes. The checker cannot identify production use or refs it was not given, so refresh tags and release branches before deciding a migration is PR-local. When uncertain, treat it as immutable.

## Rules

- New migrations use the next sequential `NNNNNN_description.up.sql` and matching `.down.sql` file.
- Applying a PR-local migration to a resettable local or preview database does not make it immutable. Reset that database to the schema baseline after rewriting the migration so its state matches the revised history.
- Do not add compatibility columns, dual-read/write paths, repair gates, or backfills for schema states that have never shipped. Amend the PR-local migration and current code paths directly.
- Keep `.down.sql` honest. If the data cleanup is one-way, say that in the down migration and only undo reversible schema artifacts such as triggers or indexes.
- Validate migrations through `db.Open()` and application-level tests. Do not test `golang-migrate` internals.
- For SQLite, remember that adding constraints to existing columns usually requires a table rebuild. Do not add fill, repair, or validation triggers as a shortcut around fixing current write paths or rebuilding a table when a real schema invariant is required.
- SQLite `CHECK` expressions accept a `NULL` result; when a nullable value is
  conditionally required, assert `IS NOT NULL` before validating its content
  (`internal/db/migrations/000048_kata_issue_links.up.sql:16`).
- A SQLite parent-table rebuild must drop and recreate dependent triggers, and
  foreign keys must be disabled before golang-migrate starts its transaction
  (`internal/db/migrations.go::runRepositoryCatalogMigration`).
- A recorded migration version and the physical schema must match. There is no
  supported "partially upgraded" schema state for new migrations.
- Never use a no-op SQL migration as a version marker for schema work that is
  actually performed later in Go. New schema artifacts belong in the numbered
  SQL migration that introduces them.
- Never make new schema migrations tolerate duplicate pre-existing columns with
  conditional `ADD COLUMN` or Go-side `ensureColumn` repair. If applying the
  migration would hit a duplicate column, the database is already claiming an
  impossible version/schema combination and should fail instead of being
  papered over.
- Do not add migration triggers, defaults, or repair hooks to compensate for
  application or test insert paths that omit newly required columns. Update
  every current-schema insert to write the new column explicitly.
- Do not write tests that "downgrade" or rewind a latest/current test database
  by editing `schema_migrations` or dropping post-target schema artifacts.
  Historical schema or data fixtures are acceptable only in migration/upgrade
  tests that build the older shape directly and then verify the forward
  migration behavior. They are not acceptable in ordinary query, API, or UI
  tests.
- When changing persisted data, test with real SQLite tables and representative child rows. Include dependent records that can be lost through foreign keys, uniqueness conflicts, or `INSERT OR IGNORE`.

## Connection Layer

- Package queries go through the `*DB` helpers or the pool-specific
  `stmtCache`, never `d.ro`/`d.rw` directly outside `BeginTx`; direct pool
  calls skip the prepared-statement cache and recompile on every call
  (`internal/db/stmt_cache.go::stmtCache`).
- `rwExecContext` and its query siblings intentionally bypass the repository
  route fence that `execContext` enforces; keep that split when moving a write
  between them (`internal/db/db.go::rwExecContext`).
- Per-connection pragmas live only in `connectionDSN`; idle limits equal open
  limits so pooled connections and their compiled statements persist
  (`internal/db/db.go::openPool`).
- `Open` refreshes planner statistics with `PRAGMA optimize`, and the daemon
  repeats it every 24 hours. Optimization checks every table, then every pooled
  reader reloads the stored statistics (`internal/db/db.go::Optimize`).
- Migrations must never drop or rebuild `sqlite_stat1`, and a query-plan assertion
  needs seeded rows plus `DB.Optimize` first (`internal/db/db.go::Optimize`).
- Lookups keyed by an unbounded ID list, such as pull-list enrichment, must not
  expand the list into `IN (?, ?, ...)` placeholders: SQLite caps a statement at
  32,766 bound variables. Bind the list once as a JSON array through
  `json_each(?)`, or batch it
  (`internal/db/queries_stacks.go::ListStackPlacementsForMRs`,
  `internal/db/queries.go::GetWorktreeLinksForMRs`).

## Federation Spoke Preparation

Migration `000054_federated_spoke_preparation` is the atomic standalone-to-spoke
preparation boundary. It owns versioned workspace launch specifications, the
singleton durable quiesce/seal state, content-addressed provider-state handoff
receipts, hub-issued seals, and notification acknowledgement admission
generations.

- Backfill launch specifications only when existing repository and provider-item
  rows contain every required fact. Missing facts remain explicitly discoverable
  through `DB.ListUnpreparedProviderWorkspaces`; never synthesize readiness.
- Backfilled launch specifications preserve local identity facts but start with
  an expired visibility lease, so spoke preparation must refresh them from the
  hub (`internal/db/migrations/000054_federated_spoke_preparation.up.sql:138`).
- Ad-hoc and Kata workspaces do not require provider launch specifications.
- The down migration removes only migration-54 tables and triggers. It does not
  rewrite provider, workspace, review, workflow, or notification source rows.
- Review drafts and workflow rows remain on the source spoke as an audit trail
  after handoff; the active spoke runtime stops reading them in the role switch.
- Migration `000056_repair_notification_admission_triggers` recreates admission
  triggers against `forge_spoke_preparation` for preview databases that applied
  migration 54 before the node-to-spoke naming change.

## Migration Review Checklist

- [ ] The migration runs from the previous schema version to the new version.
- [ ] Existing rows are transformed before new constraints or triggers are installed.
- [ ] Foreign-key child rows are moved or merged before parent rows are deleted, unless the product decision deletes the parent and its owned state as one unit.
- [ ] Unique-index conflicts are handled intentionally: true duplicates are deleted, non-duplicate children are preserved.
- [ ] `PRAGMA integrity_check` and `PRAGMA foreign_key_check` are clean on migrated test data.
- [ ] Any real-data validation uses a copy or SQLite backup, never the live database.
