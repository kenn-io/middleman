package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	return openTemplateTestDB(t)
}

func openDBWithMigrations(t *testing.T) *DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenAndSchema(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	d := openDBWithMigrations(t)
	tables := []string{
		"forge_archive_repos",
		"forge_archive_items",
		"forge_repos",
		"forge_merge_requests",
		"forge_hot_merge_requests",
		"forge_mr_events",
		"forge_item_workflow_state",
		"forge_labels",
		"forge_merge_request_labels",
		"forge_issue_labels",
		"forge_repo_overviews",
		"forge_mr_review_drafts",
		"forge_mr_review_draft_comments",
		"forge_mr_review_threads",
		"forge_project_worktree_runtime_sessions",
		"forge_host_runtime_sessions",
		"forge_notification_items",
		"forge_notification_sync_watermarks",
		"kata_issue_links",
	}
	for _, tbl := range tables {
		var name string
		err := d.ReadDB().QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		require.NoErrorf(err, "table %s should exist", tbl)
	}

	assertIndexForTest(
		t, d.ReadDB(), "kata_issue_links", "kata_issue_links_provider_identity",
		[]string{"subject_kind", "repo_id", "provider_item_external_id", "daemon_id", "issue_uid"},
		true,
	)
	assertUniqueIndexForTest(
		t, d.ReadDB(), "kata_issue_links", "kata_issue_links_provider_identity", true,
	)
	assertIndexForTest(
		t, d.ReadDB(), "kata_issue_links", "kata_issue_links_workspace_identity",
		[]string{"workspace_id", "daemon_id", "issue_uid"}, true,
	)
	assertUniqueIndexForTest(
		t, d.ReadDB(), "kata_issue_links", "kata_issue_links_workspace_identity", true,
	)

	for _, column := range []string{"workspace_branch", "associated_pr_number", "terminal_backend"} {
		var found string
		err := d.ReadDB().QueryRow(
			`SELECT name
			 FROM pragma_table_info('forge_workspaces')
			 WHERE name = ?`,
			column,
		).Scan(&found)
		require.NoError(err)
		require.Equal(column, found)
	}

	runtimeSessionColumns := map[string][]string{
		"forge_project_worktree_runtime_sessions": {
			"runtime_backend",
			"backend_session_key",
		},
		"forge_host_runtime_sessions": {
			"runtime_backend",
			"backend_session_key",
		},
	}
	for table, columns := range runtimeSessionColumns {
		for _, column := range columns {
			var found string
			err := d.ReadDB().QueryRow(
				`SELECT name
				 FROM pragma_table_info(?)
				 WHERE name = ?`,
				table, column,
			).Scan(&found)
			require.NoError(err)
			require.Equal(column, found)
		}
	}

	for table := range map[string]struct{}{
		"forge_archive_repos": {},
		"forge_archive_items": {},
	} {
		var foreignKeyCount int
		err := d.ReadDB().QueryRow(`
			SELECT COUNT(*)
			FROM pragma_foreign_key_list(?)
			WHERE "table" = 'forge_repos'
			  AND "from" = 'repo_id'
			  AND on_delete = 'CASCADE'`, table,
		).Scan(&foreignKeyCount)
		require.NoError(err)
		require.Equal(1, foreignKeyCount)
	}

	assertIndexForTest(t, d.ReadDB(), "forge_archive_repos", "idx_archive_repos_due_work", []string{
		"operator_state", "next_retry_at", "updated_at", "repo_id",
	}, false)
	assertIndexForTest(t, d.ReadDB(), "forge_archive_items", "idx_archive_items_due_work", []string{
		"repo_id", "provider_created_at", "item_type", "item_number",
	}, true)
	assertIndexForTest(t, d.ReadDB(), "forge_archive_items", "idx_archive_items_stable_order", []string{
		"repo_id", "provider_created_at", "item_type", "item_number",
	}, false)

	_, err := d.ReadDB().Exec(`INSERT INTO forge_repos (
		id, platform, platform_host, owner, name, repo_path,
		owner_key, name_key, repo_path_key, created_at
	) VALUES (
		1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
		'acme', 'widget', 'acme/widget', datetime('now')
	)`)
	require.NoError(err)

	for _, tc := range []struct {
		name      string
		statement string
	}{
		{
			name: "collection_mode",
			statement: `INSERT INTO forge_archive_repos (
				repo_id, collection_mode, operator_state, created_at, updated_at
			 ) VALUES (1, 'invalid', 'active', datetime('now'), datetime('now'))`,
		},
		{
			name: "operator_state",
			statement: `INSERT INTO forge_archive_repos (
				repo_id, collection_mode, operator_state, created_at, updated_at
			 ) VALUES (1, 'discovery', 'invalid', datetime('now'), datetime('now'))`,
		},
		{
			name: "comments_coverage",
			statement: `INSERT INTO forge_archive_repos (
				repo_id, collection_mode, operator_state, comments_coverage, created_at, updated_at
			 ) VALUES (1, 'discovery', 'active', 'invalid', datetime('now'), datetime('now'))`,
		},
	} {
		_, err := d.ReadDB().Exec(tc.statement)
		require.Error(err, tc.name)
	}

	_, err = d.ReadDB().Exec(`INSERT INTO forge_archive_repos (
		repo_id, collection_mode, operator_state, created_at, updated_at
	) VALUES (1, 'discovery', 'active', datetime('now'), datetime('now'))`)
	require.NoError(err)

	for _, tc := range []struct {
		name      string
		statement string
	}{
		{
			name:      "item_type",
			statement: archiveItemInsertForTest("invalid", 10, "invalid-type", "active"),
		},
		{
			name:      "lifecycle_state",
			statement: archiveItemInsertForTest("issue", 11, "invalid-lifecycle", "invalid"),
		},
	} {
		_, err := d.ReadDB().Exec(tc.statement)
		require.Error(err, tc.name)
	}

	_, err = d.ReadDB().Exec(archiveItemInsertForTest("issue", 100, "valid-issue", "active"))
	require.NoError(err)
	_, err = d.ReadDB().Exec(archiveItemInsertForTest("merge_request", 1, "item-1", "active"))
	require.NoError(err)

	_, err = d.ReadDB().Exec(archiveItemInsertForTest("merge_request", 1, "different-provider-id", "active"))
	require.Error(err)
	_, err = d.ReadDB().Exec(archiveItemInsertForTest("merge_request", 99, "item-1", "active"))
	require.Error(err)
}

func TestOpenBackfillsIssuePRReferences(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "issue-pr-reference-backfill.db")
	openAtVersionForTest(t, dbPath, 51, func(*sql.DB) {})

	previous, err := OpenPreparedForTest(dbPath)
	require.NoError(err)
	repoID, err := previous.UpsertRepo(t.Context(), verifiedTestRepoIdentity(
		"github", "github.com", "acme", "widget",
	))
	require.NoError(err)
	issueID, err := previous.UpsertIssue(t.Context(), testIssue(repoID, 7))
	require.NoError(err)
	_, err = previous.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_issue_events (
			issue_id, event_type, metadata_json, created_at, dedupe_key
		) VALUES (?, 'cross_referenced', ?, ?, ?)`,
		issueID,
		`{"source_type":"PullRequest","source_owner":"acme","source_repo":"client","source_number":42,"source_url":"https://github.com/acme/client/pull/42"}`,
		baseTime(),
		"cross-reference-42",
	)
	require.NoError(err)
	require.NoError(previous.Close())

	migrated, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { migrated.Close() })
	issues, err := migrated.ListIssues(t.Context(), ListIssuesOpts{ReferencedByPR: true})
	require.NoError(err)
	require.Len(issues, 1)
	require.Equal(issueID, issues[0].ID)
	assertDatabaseIntegrityForTest(t, migrated.ReadDB())
}

func TestMigration54BackfillsWorkspaceLaunchSpecs(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "workspace-launch-spec-v53.db")
	openAtVersionForTest(t, dbPath, 53, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				clone_url, default_branch, lifecycle_state, created_at
			) VALUES
			(
				1, 'github', 'github.com', 'provider-repo-1',
				'acme', 'widget', 'acme/widget', 'acme', 'widget', 'acme/widget',
				'https://github.com/acme/widget.git', 'main', 'active', datetime('now')
			),
			(
				2, 'github', 'github.com', 'provider-repo-renamed',
				'acme', 'renamed', 'acme/renamed', 'acme', 'renamed', 'acme/renamed',
				'https://github.com/acme/renamed.git', 'main', 'active', datetime('now')
			);
			INSERT INTO forge_repo_routes (
				repo_id, platform, platform_host, owner, name, repo_path,
				owner_key, name_key, repo_path_key, is_current,
				first_seen_at, last_seen_at, generation
			) VALUES
			(
				1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
				'acme', 'widget', 'acme/widget', 1,
				datetime('now'), datetime('now'), 1
			),
			(
				2, 'github', 'github.com', 'acme', 'legacy', 'acme/legacy',
				'acme', 'legacy', 'acme/legacy', 0,
				datetime('now', '-1 day'), datetime('now', '-1 hour'), 1
			),
			(
				2, 'github', 'github.com', 'acme', 'renamed', 'acme/renamed',
				'acme', 'renamed', 'acme/renamed', 1,
				datetime('now'), datetime('now'), 1
			);
			INSERT INTO forge_merge_requests (
				id, repo_id, platform_id, number, head_branch, snapshot_revision,
				created_at, updated_at, last_activity_at
			) VALUES
				(11, 1, 101, 7, 'feature/seven', 2,
				 datetime('now'), datetime('now'), datetime('now')),
				(13, 1, 103, 9, 'contributor/nine', 3,
				 datetime('now'), datetime('now'), datetime('now')),
				(14, 2, 104, 10, 'feature/ten', 1,
				 datetime('now'), datetime('now'), datetime('now'));
			INSERT INTO forge_issues (
				id, repo_id, platform_id, number, snapshot_revision,
				created_at, updated_at, last_activity_at
			) VALUES (
				12, 1, 102, 8, 2,
				datetime('now'), datetime('now'), datetime('now')
			);
			INSERT INTO forge_workspaces (
				id, platform, platform_host, repo_owner, repo_name,
				repo_owner_key, repo_name_key, repo_path_key,
				item_type, item_number, item_key, git_head_ref,
				workspace_branch, worktree_path, tmux_session, status
			) VALUES
				('ws-pr', 'github', 'github.com', 'acme', 'widget',
				 'acme', 'widget', 'acme/widget',
				 'pull_request', 7, '7', 'feature/seven',
				 'feature/seven', '/tmp/ws-pr', 'ws-pr', 'ready'),
				('ws-issue', 'github', 'github.com', 'acme', 'widget',
				 'acme', 'widget', 'acme/widget',
				 'issue', 8, '8', 'work/issue-8',
				 'work/issue-8', '/tmp/ws-issue', 'ws-issue', 'ready'),
				('ws-fork', 'github', 'github.com', 'acme', 'widget',
				 'acme', 'widget', 'acme/widget',
				 'pull_request', 9, '9', 'contributor/nine',
				 'contributor/nine', '/tmp/ws-fork', 'ws-fork', 'ready'),
				('ws-incomplete', 'github', 'github.com', 'acme', 'widget',
				 'acme', 'widget', 'acme/widget',
				 'pull_request', 99, '99', 'missing',
				 'missing', '/tmp/ws-incomplete', 'ws-incomplete', 'ready'),
				('ws-renamed', 'github', 'github.com', 'acme', 'legacy',
				 'acme', 'legacy', 'acme/legacy',
				 'pull_request', 10, '10', 'feature/ten',
				 'feature/ten', '/tmp/ws-renamed', 'ws-renamed', 'ready'),
				('ws-renamed-incomplete', 'github', 'github.com', 'acme', 'legacy',
				 'acme', 'legacy', 'acme/legacy',
				 'pull_request', 11, '11', 'feature/eleven',
				 'feature/eleven', '/tmp/ws-renamed-incomplete', 'ws-renamed-incomplete', 'ready'),
				('ws-adhoc', 'github', 'github.com', 'acme', 'widget',
				 'acme', 'widget', 'acme/widget',
				 'adhoc', 0, 'adhoc:work/local', 'work/local',
				 'work/local', '/tmp/ws-adhoc', 'ws-adhoc', 'ready')
		`)
		require.NoError(err)
		_, err = raw.Exec(`
			UPDATE forge_workspaces
			SET mr_head_repo = 'https://github.com/contributor/widget.git'
			WHERE id = 'ws-fork'`)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	for _, workspaceID := range []string{"ws-pr", "ws-issue", "ws-fork", "ws-renamed"} {
		spec, err := database.GetWorkspaceLaunchSpec(t.Context(), workspaceID)
		require.NoError(err)
		require.NotNil(spec)
		assert.Equal(WorkspaceLaunchSpecVersion, spec.Version)
	}
	fork, err := database.GetWorkspaceLaunchSpec(t.Context(), "ws-fork")
	require.NoError(err)
	require.NotNil(fork)
	require.NotNil(fork.Pull)
	assert.Equal("fork", fork.Pull.HeadRepoKind)
	assert.Equal("https://github.com/contributor/widget.git", fork.Pull.HeadRepoCloneURL)
	renamed, err := database.GetWorkspaceLaunchSpec(t.Context(), "ws-renamed")
	require.NoError(err)
	require.NotNil(renamed)
	assert.Equal("provider-repo-renamed", renamed.Repository.PlatformRepoID)
	assert.Equal("renamed", renamed.Repository.Name)
	adhoc, err := database.GetWorkspaceLaunchSpec(t.Context(), "ws-adhoc")
	require.NoError(err)
	assert.Nil(adhoc)
	unprepared, err := database.ListUnpreparedProviderWorkspaces(t.Context())
	require.NoError(err)
	require.Len(unprepared, 6)
	reasons := make(map[string]string, len(unprepared))
	stableIDs := make(map[string]string, len(unprepared))
	for _, item := range unprepared {
		reasons[item.Workspace.ID] = item.Reason
		stableIDs[item.Workspace.ID] = item.PlatformRepoID
	}
	assert.Equal(map[string]string{
		"ws-pr":                 "sourceVisibilityExpired",
		"ws-issue":              "sourceVisibilityExpired",
		"ws-fork":               "sourceVisibilityExpired",
		"ws-incomplete":         "launchSpecMissing",
		"ws-renamed":            "sourceVisibilityExpired",
		"ws-renamed-incomplete": "launchSpecMissing",
	}, reasons)
	assert.Equal("provider-repo-renamed", stableIDs["ws-renamed-incomplete"])
	assertDatabaseIntegrityForTest(t, database.ReadDB())
}

func TestMigration54DownDropsOnlyPreparationTables(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "node-preparation-down.db")
	database, err := Open(dbPath)
	require.NoError(err)
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	require.NoError(migrator.Migrate(53))
	for _, table := range []string{
		"forge_workspace_launch_specs", "forge_spoke_preparation",
		"forge_spoke_preparation_receipts", "forge_spoke_preparation_seals",
		"forge_notification_ack_admissions",
	} {
		assert.False(tableExistsForTest(t, raw, table), table)
	}
	for _, table := range []string{
		"forge_workspaces", "forge_repos", "forge_notification_items",
	} {
		assert.True(tableExistsForTest(t, raw, table), table)
	}
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestWorkspaceRepositoryIdentityMigration55BackfillsOnlyUnambiguousRoutes(
	t *testing.T,
) {
	t.Parallel()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "workspace-repository-identity-v54.db")

	openAtVersionForTest(t, dbPath, 54, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				lifecycle_state
			) VALUES
				(1, 'github', 'github.com', 'repo-safe',
				 'acme', 'safe', 'acme/safe', 'acme', 'safe', 'acme/safe', 'active'),
				(5, 'github', 'github.com', '',
				 'acme', 'route-only', 'acme/route-only', 'acme', 'route-only', 'acme/route-only', 'active'),
				(2, 'github', 'github.com', 'repo-displaced',
				 'acme', 'reused', 'acme/reused', 'acme', 'reused', 'acme/reused', 'inactive'),
				(3, 'github', 'github.com', 'repo-current',
				 'acme', 'reused', 'acme/reused', 'acme', 'reused', 'acme/reused', 'active'),
				(4, 'github', 'github.com', 'repo-renamed',
				 'acme', 'renamed', 'acme/renamed', 'acme', 'renamed', 'acme/renamed', 'active');

			INSERT INTO forge_repo_routes (
				repo_id, platform, platform_host,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				is_current, first_seen_at, last_seen_at
			) VALUES
				(1, 'github', 'github.com',
				 'acme', 'safe', 'acme/safe', 'acme', 'safe', 'acme/safe',
				 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'),
				(5, 'github', 'github.com',
				 'acme', 'route-only', 'acme/route-only', 'acme', 'route-only', 'acme/route-only',
				 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'),
				(2, 'github', 'github.com',
				 'acme', 'reused', 'acme/reused', 'acme', 'reused', 'acme/reused',
				 0, '2026-08-01T00:00:00Z', '2026-08-02T00:00:00Z'),
				(3, 'github', 'github.com',
				 'acme', 'reused', 'acme/reused', 'acme', 'reused', 'acme/reused',
				 1, '2026-08-03T00:00:00Z', '2026-08-03T00:00:00Z'),
				(4, 'github', 'github.com',
				 'acme', 'before-rename', 'acme/before-rename', 'acme', 'before-rename', 'acme/before-rename',
				 0, '2026-08-01T00:00:00Z', '2026-08-02T00:00:00Z'),
				(4, 'github', 'github.com',
				 'acme', 'renamed', 'acme/renamed', 'acme', 'renamed', 'acme/renamed',
				 1, '2026-08-03T00:00:00Z', '2026-08-03T00:00:00Z');

			INSERT INTO forge_workspaces (
				id, platform, platform_host, repo_owner, repo_name,
				repo_owner_key, repo_name_key, repo_path_key,
				item_type, item_number, item_key, git_head_ref,
				worktree_path, tmux_session, status, created_at
			) VALUES
				('workspace-safe', 'github', 'github.com', 'acme', 'safe',
				 'acme', 'safe', 'acme/safe',
				 'pull_request', 7, '7', 'feature/safe',
				 '/tmp/workspace-safe', 'workspace-safe', 'ready', '2026-08-01T00:00:00Z'),
				('workspace-route-only', 'github', 'github.com', 'acme', 'route-only',
				 'acme', 'route-only', 'acme/route-only',
				 'pull_request', 11, '11', 'feature/route-only',
				 '/tmp/workspace-route-only', 'workspace-route-only', 'ready', '2026-08-01T00:00:00Z'),
				('workspace-contested', 'github', 'github.com', 'acme', 'reused',
				 'acme', 'reused', 'acme/reused',
				 'pull_request', 8, '8', 'feature/contested',
				 '/tmp/workspace-contested', 'workspace-contested', 'ready', '2026-08-01T00:00:00Z'),
				('workspace-renamed-old', 'github', 'github.com', 'acme', 'before-rename',
				 'acme', 'before-rename', 'acme/before-rename',
				 'pull_request', 9, '9', 'feature/old',
				 '/tmp/workspace-renamed-old', 'workspace-renamed-old', 'ready', '2026-08-03T00:00:00Z'),
				('workspace-renamed-new', 'github', 'github.com', 'acme', 'renamed',
				 'acme', 'renamed', 'acme/renamed',
				 'pull_request', 9, '9', 'feature/new',
				 '/tmp/workspace-renamed-new', 'workspace-renamed-new', 'ready', '2026-08-03T00:00:00Z');

			INSERT INTO forge_workspace_setup_events (
				workspace_id, stage, outcome, message
			) VALUES
				('workspace-safe', 'setup', 'success', 'ready'),
				('workspace-contested', 'setup', 'success', 'ready'),
				('workspace-renamed-old', 'setup', 'success', 'ready'),
				('workspace-renamed-new', 'setup', 'success', 'ready');
		`)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)

	var safeRepoID sql.NullInt64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT repo_id FROM forge_workspaces WHERE id = 'workspace-safe'
	`).Scan(&safeRepoID))
	require.True(safeRepoID.Valid)
	require.Equal(int64(1), safeRepoID.Int64)

	var routeOnlyRepoID sql.NullInt64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT repo_id FROM forge_workspaces WHERE id = 'workspace-route-only'
	`).Scan(&routeOnlyRepoID))
	require.True(routeOnlyRepoID.Valid)
	require.Equal(int64(5), routeOnlyRepoID.Int64)

	var contestedRepoID sql.NullInt64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT repo_id FROM forge_workspaces WHERE id = 'workspace-contested'
	`).Scan(&contestedRepoID))
	require.False(contestedRepoID.Valid)

	var renamedRepoID int64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT repo_id FROM forge_workspaces WHERE id = 'workspace-renamed-new'
	`).Scan(&renamedRepoID))
	require.Equal(int64(4), renamedRepoID)
	var deletedDuplicateCount int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_workspaces WHERE id = 'workspace-renamed-old'
	`).Scan(&deletedDuplicateCount))
	require.Zero(deletedDuplicateCount)

	var setupEventCount int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_workspace_setup_events
	`).Scan(&setupEventCount))
	require.Equal(3, setupEventCount)
	assertDatabaseIntegrityForTest(t, database.ReadDB())
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	require.NoError(migrator.Migrate(54))
	var repoIDColumnCount int
	require.NoError(raw.QueryRow(`
		SELECT COUNT(*)
		FROM pragma_table_info('forge_workspaces')
		WHERE name = 'repo_id'
	`).Scan(&repoIDColumnCount))
	require.Zero(repoIDColumnCount)
	require.True(hasIndex(raw, "idx_workspaces_provider_item_key"))
	require.NoError(raw.QueryRow(`
		SELECT COUNT(*) FROM forge_workspace_setup_events
	`).Scan(&setupEventCount))
	require.Equal(3, setupEventCount)
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestMigration56RepairsNotificationAdmissionTriggers(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "notification-admission-trigger-v55.db")
	notificationInsert := `
		INSERT INTO forge_notification_items (
			platform, platform_host, platform_notification_id,
			repo_owner, repo_name, subject_type, subject_title,
			item_number, item_type, reason, unread,
			source_updated_at, synced_at, source_ack_queued_at
		) VALUES (
			'github', 'github.com', ?,
			'acme', 'widget', 'PullRequest', 'Trigger repair',
			1, 'pr', 'mention', 1,
			'2026-09-02T23:00:00Z', '2026-09-02T23:00:00Z', ?
		)`

	openAtVersionForTest(t, dbPath, 55, func(raw *sql.DB) {
		_, err := raw.Exec(`
			DROP TRIGGER forge_notification_ack_admission_update;
			DROP TRIGGER forge_notification_ack_admission_insert;

			CREATE TRIGGER forge_notification_ack_admission_insert
			AFTER INSERT ON forge_notification_items
			WHEN NEW.source_ack_queued_at IS NOT NULL
			 AND NEW.source_ack_synced_at IS NULL
			BEGIN
				UPDATE forge_node_preparation
				SET ack_generation = ack_generation + 1,
					updated_at = datetime('now')
				WHERE singleton_id = 1;

				INSERT INTO forge_notification_ack_admissions (
					notification_id, generation, queued_at
				)
				SELECT NEW.id, ack_generation, NEW.source_ack_queued_at
				FROM forge_node_preparation
				WHERE singleton_id = 1
				ON CONFLICT(notification_id) DO UPDATE SET
					generation = excluded.generation,
					queued_at = excluded.queued_at;
			END;

			CREATE TRIGGER forge_notification_ack_admission_update
			AFTER UPDATE OF source_ack_queued_at, source_ack_synced_at
			ON forge_notification_items
			WHEN NEW.source_ack_queued_at IS NOT NULL
			 AND NEW.source_ack_synced_at IS NULL
			 AND (
				 OLD.source_ack_queued_at IS NULL
				 OR OLD.source_ack_synced_at IS NOT NULL
				 OR OLD.source_ack_queued_at <> NEW.source_ack_queued_at
			 )
			BEGIN
				UPDATE forge_node_preparation
				SET ack_generation = ack_generation + 1,
					updated_at = datetime('now')
				WHERE singleton_id = 1;

				INSERT INTO forge_notification_ack_admissions (
					notification_id, generation, queued_at
				)
				SELECT NEW.id, ack_generation, NEW.source_ack_queued_at
				FROM forge_node_preparation
				WHERE singleton_id = 1
				ON CONFLICT(notification_id) DO UPDATE SET
					generation = excluded.generation,
					queued_at = excluded.queued_at;
			END;
		`)
		require.NoError(err)
		_, err = raw.Exec(notificationInsert, "stale-trigger", "2026-09-02T23:00:00Z")
		require.ErrorContains(err, "no such table: main.forge_node_preparation")
	})

	database, err := Open(dbPath)
	require.NoError(err)
	_, err = database.WriteDB().Exec(
		notificationInsert, "insert-admission", "2026-09-02T23:01:00Z",
	)
	require.NoError(err)
	_, err = database.WriteDB().Exec(notificationInsert, "update-admission", nil)
	require.NoError(err)
	_, err = database.WriteDB().Exec(`
		UPDATE forge_notification_items
		SET source_ack_queued_at = '2026-09-02T23:02:00Z'
		WHERE platform_notification_id = 'update-admission'
	`)
	require.NoError(err)

	var ackGeneration int64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT ack_generation
		FROM forge_spoke_preparation
		WHERE singleton_id = 1
	`).Scan(&ackGeneration))
	require.Equal(int64(2), ackGeneration)
	var admissionCount int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_notification_ack_admissions
	`).Scan(&admissionCount))
	require.Equal(2, admissionCount)
	assertDatabaseIntegrityForTest(t, database.ReadDB())
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	require.NoError(migrator.Migrate(55))
	_, err = raw.Exec(
		notificationInsert, "downgraded-admission", "2026-09-02T23:03:00Z",
	)
	require.NoError(err)
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestKataIssueLinksMigration48IsReversible(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "kata-issue-links-v47.db")

	openAtVersionForTest(t, dbPath, 47, func(raw *sql.DB) {
		assert.False(tableExistsForTest(t, raw, "kata_issue_links"))
	})

	database, err := Open(dbPath)
	require.NoError(err)
	assert.True(tableExistsForTest(t, database.ReadDB(), "kata_issue_links"))
	assertDatabaseIntegrityForTest(t, database.ReadDB())
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	require.NoError(migrator.Migrate(47))
	assert.False(tableExistsForTest(t, raw, "kata_issue_links"))
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestKataIssueLinksMigration48EnforcesIdentityConstraints(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	req := require.New(t)
	database := openDBWithMigrations(t)
	repoID := insertTestRepo(t, database, "acme", "widget")
	workspace := &Workspace{
		ID: "workspace-constraints", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: WorkspaceItemTypePullRequest,
		ItemNumber: 42, GitHeadRef: "feature", WorkspaceBranch: "feature",
		WorktreePath: "/tmp/workspace-constraints", TmuxSession: "workspace-constraints", Status: "ready",
	}
	req.NoError(database.InsertWorkspace(t.Context(), workspace))

	tests := []struct {
		name string
		args []any
	}{
		{name: "unknown kind", args: []any{"unknown", repoID, "provider-item-A", nil, "d", "p", "i"}},
		{name: "provider with workspace", args: []any{"issue", repoID, "provider-item-A", workspace.ID, "d", "p", "i"}},
		{name: "provider without external id", args: []any{"issue", repoID, nil, nil, "d", "p", "i"}},
		{name: "provider with blank external id", args: []any{"issue", repoID, " ", nil, "d", "p", "i"}},
		{name: "workspace with repo", args: []any{"workspace", repoID, nil, workspace.ID, "d", "p", "i"}},
		{name: "workspace with provider id", args: []any{"workspace", nil, "provider-item-A", workspace.ID, "d", "p", "i"}},
		{name: "blank daemon id", args: []any{"workspace", nil, nil, workspace.ID, " ", "p", "i"}},
		{name: "blank project uid", args: []any{"workspace", nil, nil, workspace.ID, "d", " ", "i"}},
		{name: "blank issue uid", args: []any{"workspace", nil, nil, workspace.ID, "d", "p", " "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := database.WriteDB().ExecContext(t.Context(), `
				INSERT INTO kata_issue_links (
					subject_kind, repo_id, provider_item_external_id, workspace_id,
					daemon_id, project_uid, issue_uid
				) VALUES (?, ?, ?, ?, ?, ?, ?)`, tt.args...)
			require.Error(t, err)
		})
	}
	assert.Zero(kataIssueLinkCountForTest(t, database))
}

func TestOpenMigratesHistoricalActivityArchive(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive-upgrade.db")
	backfillColumns := []string{
		"backfill_pr_page",
		"backfill_pr_complete",
		"backfill_pr_completed_at",
		"backfill_issue_page",
		"backfill_issue_complete",
		"backfill_issue_completed_at",
	}
	var before historicalArchiveUpgradeSnapshotForTest

	openAtVersionForTest(t, path, 38, func(raw *sql.DB) {
		_, err := raw.Exec(`INSERT INTO middleman_repos (
			id, platform, platform_host, owner, name, repo_path,
			owner_key, name_key, repo_path_key, created_at
		) VALUES (
			1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
			'acme', 'widget', 'acme/widget', datetime('now')
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_issues (
			id, repo_id, platform_id, platform_external_id, number,
			created_at, updated_at, last_activity_at
		) VALUES (
			11, 1, 101, 'issue-101', 7,
			'2026-07-01 10:00:00', '2026-07-02 10:00:00', '2026-07-02 10:00:00'
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_merge_requests (
			id, repo_id, platform_id, platform_external_id, number,
			created_at, updated_at, last_activity_at
		) VALUES (
			22, 1, 202, 'merge-request-202', 9,
			'2026-07-03 10:00:00', '2026-07-04 10:00:00', '2026-07-04 10:00:00'
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_mr_review_drafts (
			id, merge_request_id, body, action, created_at, updated_at
		) VALUES (
			33, 22, 'keep this draft', 'comment',
			'2026-07-05 10:00:00', '2026-07-05 10:00:00'
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_mr_worktree_links (
			id, merge_request_id, worktree_key, worktree_path,
			worktree_branch, linked_at
		) VALUES (
			44, 22, 'worktree-9', '/tmp/worktree-9', 'feature/archive',
			'2026-07-05 11:00:00'
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_stacks (
			id, repo_id, base_number, name, created_at, updated_at
		) VALUES (
			55, 1, 9, 'archive stack',
			'2026-07-05 12:00:00', '2026-07-05 12:00:00'
		)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_stack_members (
			stack_id, merge_request_id, position
		) VALUES (55, 22, 1)`)
		require.NoError(err)
		_, err = raw.Exec(`INSERT INTO middleman_item_workflow_state (
			repo_id, item_type, item_number, status, updated_at,
			updated_source, updated_actor, updated_reason
		) VALUES
			(1, 'issue', 7, 'waiting', '2026-07-05 13:00:00', 'local', 'alice', 'triage'),
			(1, 'pr', 9, 'reviewing', '2026-07-05 14:00:00', 'local', 'alice', 'review')`)
		require.NoError(err)
		before = readHistoricalArchiveUpgradeSnapshotForTest(t, raw, "middleman")
	})

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })
	assert.Equal(before, readHistoricalArchiveUpgradeSnapshotForTest(t, d.ReadDB(), "forge"))
	for _, column := range backfillColumns {
		exists, columnErr := hasColumn(d.ReadDB(), "forge_repos", column)
		require.NoError(columnErr)
		assert.False(exists, column)
	}

	assert.True(tableExistsForTest(t, d.ReadDB(), "forge_archive_repo_scans"))
	assert.True(tableExistsForTest(t, d.ReadDB(), "forge_archive_dataset_progress"))
	for table, columns := range map[string][]string{
		"forge_archive_repo_scans": {
			"scan", "scan_generation", "next_cursor", "last_input_cursor",
			"page_count", "status", "last_error_code", "last_error_detail",
		},
		"forge_archive_dataset_progress": {
			"dataset", "parent_revision", "scan_generation", "next_cursor",
			"last_input_cursor", "page_count", "status", "observed_count",
			"attempt_count", "next_retry_at", "started_at", "completed_at",
		},
		"forge_issue_events": {"ingest_generation"},
		"forge_mr_events":    {"ingest_generation"},
	} {
		for _, column := range columns {
			exists, columnErr := hasColumn(d.ReadDB(), table, column)
			require.NoError(columnErr)
			assert.True(exists, "%s.%s", table, column)
		}
	}

	var integrityCheck string
	err = d.ReadDB().QueryRow(`PRAGMA integrity_check`).Scan(&integrityCheck)
	require.NoError(err)
	assert.Equal("ok", integrityCheck)

	var foreignKeyViolations int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations)
	require.NoError(err)
	assert.Zero(foreignKeyViolations)
}

func TestOpenMigratesKanbanRowsToItemWorkflowState(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	openAtVersionForTest(t, path, 36, func(raw *sql.DB) {
		_, err := raw.Exec(`INSERT INTO middleman_repos (
				id, platform, platform_host, owner, name, repo_path,
				owner_key, name_key, repo_path_key, created_at
			)
			VALUES (
				1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
				'acme', 'widget', 'acme/widget', datetime('now')
			)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_merge_requests
			(id, repo_id, platform_id, number, created_at, updated_at, last_activity_at)
			VALUES (1, 1, 101, 7, datetime('now'), datetime('now'), datetime('now'))`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_kanban_state (merge_request_id, status, updated_at)
			VALUES (1, 'reviewing', '2026-07-01 10:00:00')`)
		require.NoError(t, err)
	})

	d, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	var itemType, status, source string
	var number int
	err = d.ReadDB().QueryRow(`SELECT item_type, item_number, status, updated_source
		FROM forge_item_workflow_state`).Scan(&itemType, &number, &status, &source)
	require.NoError(t, err)
	assert.Equal("pr", itemType)
	assert.Equal(7, number)
	assert.Equal("reviewing", status)
	assert.Empty(source)
	assert.False(tableExistsForTest(t, d.ReadDB(), "forge_kanban_state"))
	assert.True(tableExistsForTest(t, d.ReadDB(), "forge_item_workflow_state"))
}

func TestOpenResyncsKanbanRowsBeforeDroppingKanbanState(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	openAtVersionForTest(t, path, 37, func(raw *sql.DB) {
		_, err := raw.Exec(`INSERT INTO middleman_repos (
				id, platform, platform_host, owner, name, repo_path,
				owner_key, name_key, repo_path_key, created_at
			)
			VALUES (
				1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
				'acme', 'widget', 'acme/widget', datetime('now')
			)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_merge_requests
			(id, repo_id, platform_id, number, created_at, updated_at, last_activity_at)
			VALUES (1, 1, 101, 7, datetime('now'), datetime('now'), datetime('now'))`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_kanban_state (merge_request_id, status, updated_at)
			VALUES (1, 'waiting', '2026-07-01 10:00:00')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_item_workflow_state
			(repo_id, item_type, item_number, status, updated_at)
			VALUES (1, 'pr', 7, 'reviewing', '2026-07-01 10:00:00')`)
		require.NoError(t, err)
	})

	d, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	var status string
	err = d.ReadDB().QueryRow(`SELECT status FROM forge_item_workflow_state
		WHERE repo_id = 1 AND item_type = 'pr' AND item_number = 7`).Scan(&status)
	require.NoError(t, err)
	assert.Equal("waiting", status)
	assert.False(tableExistsForTest(t, d.ReadDB(), "forge_kanban_state"))
}

func TestOpenNormalizesInvalidWorkflowStatusesDuringCutover(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	openAtVersionForTest(t, path, 37, func(raw *sql.DB) {
		_, err := raw.Exec(`INSERT INTO middleman_repos (
				id, platform, platform_host, owner, name, repo_path,
				owner_key, name_key, repo_path_key, created_at
			)
			VALUES (
				1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
				'acme', 'widget', 'acme/widget', datetime('now')
			)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_merge_requests
			(id, repo_id, platform_id, number, created_at, updated_at, last_activity_at)
			VALUES (1, 1, 101, 7, datetime('now'), datetime('now'), datetime('now'))`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_kanban_state (merge_request_id, status, updated_at)
			VALUES (1, 'triage', '2026-07-01 10:00:00')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO middleman_item_workflow_state
			(repo_id, item_type, item_number, status, updated_at)
			VALUES (1, 'pr', 9, 'bogus', '2026-07-01 10:00:00')`)
		require.NoError(t, err)
	})

	d, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	var resynced, orphaned string
	err = d.ReadDB().QueryRow(`SELECT status FROM forge_item_workflow_state
		WHERE repo_id = 1 AND item_type = 'pr' AND item_number = 7`).Scan(&resynced)
	require.NoError(t, err)
	assert.Equal("new", resynced)
	err = d.ReadDB().QueryRow(`SELECT status FROM forge_item_workflow_state
		WHERE repo_id = 1 AND item_type = 'pr' AND item_number = 9`).Scan(&orphaned)
	require.NoError(t, err)
	assert.Equal("new", orphaned)
}

func TestOpenIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d1, err := Open(path)
	require.NoError(t, err)
	d1.Close()
	d2, err := Open(path)
	require.NoError(t, err)
	d2.Close()
}

func TestArchivePromotionBoundaryMigrationReopensMaintenanceGap(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive-promotion-gap-v49.db")
	discoveredAt := time.Date(2026, time.July, 20, 21, 35, 22, 827186000, time.UTC)
	promotedAt := time.Date(2026, time.July, 27, 2, 28, 59, 0, time.UTC)
	maintainedAt := time.Date(2026, time.August, 13, 16, 35, 52, 0, time.UTC)

	openAtVersionForTest(t, dbPath, 49, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				created_at
			) VALUES (
				1, 'github', 'github.com', 'provider-1',
				'acme', 'widget', 'acme/widget', 'acme', 'widget', 'acme/widget',
				'2026-07-20T21:35:00Z'
			);
			INSERT INTO forge_archive_repos (
				repo_id, collection_mode, operator_state,
				initial_started_at, initial_completed_at,
				maintenance_watermark, maintenance_succeeded_at,
				created_at, updated_at
			) VALUES (
				1, 'full', 'active', ?, '2026-07-27T19:22:51Z', ?, ?, ?, ?
			);
			INSERT INTO forge_archive_repo_scans (repo_id, scan, status, updated_at)
			VALUES
				(1, 'issue_inventory', 'complete', ?),
				(1, 'merge_request_inventory', 'complete', ?),
				(1, 'maintenance_issues', 'pending', ?),
				(1, 'maintenance_merge_requests', 'pending', ?)
		`, promotedAt, maintainedAt, maintainedAt, discoveredAt, maintainedAt,
			discoveredAt, discoveredAt, maintainedAt, maintainedAt)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	var initialStarted, watermark time.Time
	var maintenanceSucceeded sql.NullTime
	err = database.ReadDB().QueryRow(`
		SELECT initial_started_at, maintenance_watermark, maintenance_succeeded_at
		FROM forge_archive_repos WHERE repo_id = 1
	`).Scan(&initialStarted, &watermark, &maintenanceSucceeded)
	require.NoError(err)
	assert.Equal(discoveredAt, initialStarted)
	assert.Equal(discoveredAt, watermark)
	assert.False(maintenanceSucceeded.Valid)
	assertDatabaseIntegrityForTest(t, database.ReadDB())
}

func TestActivityEventMutationRevisionMigrationUpgradesPopulatedV50Database(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "activity-event-revision-v50.db")

	openAtVersionForTest(t, dbPath, 50, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				created_at
			) VALUES (
				1, 'github', 'github.com', 'provider-1',
				'acme', 'widgets', 'acme/widgets', 'acme', 'widgets', 'acme/widgets',
				'2026-08-01T00:00:00Z'
			);
			INSERT INTO forge_merge_requests (
				id, repo_id, platform_id, number, created_at, updated_at, last_activity_at
			) VALUES
				(11, 1, 101, 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'),
				(12, 1, 102, 2, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z');
			INSERT INTO forge_issues (
				id, repo_id, platform_id, number, created_at, updated_at, last_activity_at
			) VALUES
				(21, 1, 201, 3, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'),
				(22, 1, 202, 4, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z');
			INSERT INTO forge_mr_events (id, merge_request_id, event_type, body, created_at, dedupe_key)
			VALUES
				(101, 11, 'comment', 'first', '2026-08-01T01:00:00Z', 'mr-101'),
				(102, 11, 'comment', 'second', '2026-08-01T02:00:00Z', 'mr-102');
			INSERT INTO forge_issue_events (id, issue_id, event_type, body, created_at, dedupe_key)
			VALUES
				(201, 21, 'comment', 'first', '2026-08-01T01:00:00Z', 'issue-201'),
				(202, 21, 'comment', 'second', '2026-08-01T02:00:00Z', 'issue-202');
			INSERT INTO forge_notification_items (
				id, platform, platform_host, platform_notification_id, repo_id,
				repo_owner, repo_name, subject_type, subject_title, web_url,
				item_number, item_type, item_author, reason, unread,
				source_updated_at, synced_at
			) VALUES
				(301, 'github', 'github.com', 'notification-pr', 1,
				 'acme', 'widgets', 'PullRequest', 'PR notification', 'https://github.com/acme/widgets/pull/1',
				 1, 'pr', 'author', 'mention', 1,
				 '2026-08-01T01:30:00Z', '2026-08-01T01:30:00Z'),
				(302, 'github', 'github.com', 'notification-issue', 1,
				 'acme', 'widgets', 'Issue', 'Issue notification', 'https://github.com/acme/widgets/issues/3',
				 3, 'issue', 'author', 'mention', 1,
				 '2026-08-01T01:30:00Z', '2026-08-01T01:30:00Z'),
				(303, 'github', 'github.com', 'notification-author', 1,
				 'acme', 'widgets', 'PullRequest', 'Invisible author notification', 'https://github.com/acme/widgets/pull/1',
				 1, 'pr', 'author', 'author', 1,
				 '2026-08-01T01:45:00Z', '2026-08-01T01:45:00Z');
		`)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	var mrRevision, emptyMRRevision, issueRevision, emptyIssueRevision int
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_merge_requests WHERE id = 11`,
	).Scan(&mrRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_merge_requests WHERE id = 12`,
	).Scan(&emptyMRRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_issues WHERE id = 21`,
	).Scan(&issueRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_issues WHERE id = 22`,
	).Scan(&emptyIssueRevision))
	assert.Equal(3, mrRevision)
	assert.Zero(emptyMRRevision)
	assert.Equal(3, issueRevision)
	assert.Zero(emptyIssueRevision)

	_, err = database.WriteDB().Exec(`
		INSERT INTO forge_mr_events (id, merge_request_id, event_type, body, created_at, dedupe_key)
		VALUES (103, 11, 'comment', 'inserted', '2026-08-01T03:00:00Z', 'mr-103');
		UPDATE forge_mr_events SET body = 'edited' WHERE id = 101;
		DELETE FROM forge_mr_events WHERE id = 102;
		UPDATE forge_mr_events SET merge_request_id = 12 WHERE id = 101;
		INSERT INTO forge_issue_events (id, issue_id, event_type, body, created_at, dedupe_key)
		VALUES (203, 21, 'comment', 'inserted', '2026-08-01T03:00:00Z', 'issue-203');
		UPDATE forge_issue_events SET body = 'edited' WHERE id = 201;
		DELETE FROM forge_issue_events WHERE id = 202;
		UPDATE forge_issue_events SET issue_id = 22 WHERE id = 201;
		INSERT INTO forge_notification_items (
			id, platform, platform_host, platform_notification_id, repo_id,
			repo_owner, repo_name, subject_type, subject_title, web_url,
			item_number, item_type, item_author, reason, unread,
			source_updated_at, synced_at
		) VALUES
			(304, 'github', 'github.com', 'notification-pr-inserted', 1,
			 'acme', 'widgets', 'PullRequest', 'Inserted PR notification', 'https://github.com/acme/widgets/pull/1',
			 1, 'pr', 'author', 'mention', 1,
			 '2026-08-01T03:00:00Z', '2026-08-01T03:00:00Z'),
			(305, 'github', 'github.com', 'notification-issue-inserted', 1,
			 'acme', 'widgets', 'Issue', 'Inserted issue notification', 'https://github.com/acme/widgets/issues/3',
			 3, 'issue', 'author', 'mention', 1,
			 '2026-08-01T03:00:00Z', '2026-08-01T03:00:00Z');
		UPDATE forge_notification_items SET subject_title = 'Edited PR notification' WHERE id = 301;
		UPDATE forge_notification_items SET subject_title = 'Edited issue notification' WHERE id = 302;
		DELETE FROM forge_notification_items WHERE id IN (304, 305);
		UPDATE forge_notification_items SET item_number = 2 WHERE id = 301;
		UPDATE forge_notification_items SET item_number = 4 WHERE id = 302;
	`)
	require.NoError(err)

	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_merge_requests WHERE id = 11`,
	).Scan(&mrRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_merge_requests WHERE id = 12`,
	).Scan(&emptyMRRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_issues WHERE id = 21`,
	).Scan(&issueRevision))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT activity_event_revision FROM forge_issues WHERE id = 22`,
	).Scan(&emptyIssueRevision))
	assert.Equal(11, mrRevision)
	assert.Equal(2, emptyMRRevision)
	assert.Equal(11, issueRevision)
	assert.Equal(2, emptyIssueRevision)

	// The fixture holds only a couple of notification rows, and Open now
	// leaves real planner statistics behind, so the planner rightly scans
	// them instead of probing the index. Assert the migration created the
	// index rather than a plan choice that only holds at scale.
	var parentIndexes int
	require.NoError(database.ReadDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_forge_notification_items_activity_parent'`,
	).Scan(&parentIndexes))
	assert.Equal(1, parentIndexes, "migration must add the notification parent index")
	assertDatabaseIntegrityForTest(t, database.ReadDB())
}

func TestOpenMigratesLegacyDatabase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		version int
	}{
		{name: "schema_version_1", version: 1},
		{name: "schema_version_2", version: 2},
		{name: "schema_version_3", version: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "legacy.db")

			raw, err := sql.Open("sqlite", path)
			require.NoError(err)
			_, err = raw.Exec(legacySchemaSQLForTest(t, tc.version))
			require.NoError(err)
			_, err = raw.Exec(
				`CREATE TABLE middleman_schema_version (version INTEGER NOT NULL)`,
			)
			require.NoError(err)
			_, err = raw.Exec(
				`INSERT INTO middleman_schema_version (version) VALUES (?)`,
				tc.version,
			)
			require.NoError(err)
			require.NoError(raw.Close())

			d, err := Open(path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(d.Close()) })

			version := latestMigrationVersionForTest(t)
			var actualVersion int
			var dirty bool
			err = d.ReadDB().QueryRow(
				`SELECT version, dirty FROM schema_migrations LIMIT 1`,
			).Scan(&actualVersion, &dirty)
			require.NoError(err)
			require.Equal(version, actualVersion)
			require.False(dirty)
			require.False(tableExistsForTest(t, d.ReadDB(), "middleman_schema_version"))
		})
	}
}

func TestSchemaIdentityMigrationPreservesDataAndIsReversible(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "schema-identity-v43.db")
	openAtVersionForTest(t, path, 43, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO middleman_repos (
				id, platform, platform_host, owner, name, repo_path,
				owner_key, name_key, repo_path_key, created_at
			) VALUES (
				1, 'github', 'github.com', 'acme', 'widget', 'acme/widget',
				'acme', 'widget', 'acme/widget', datetime('now')
			);
			INSERT INTO middleman_workspaces (
				id, platform, platform_host, repo_owner, repo_name,
				item_type, item_number, git_head_ref, worktree_path,
				tmux_session, status, workspace_branch,
				repo_owner_key, repo_name_key, repo_path_key, item_key
			) VALUES
				('unknown', 'github', 'github.com', 'acme', 'widget',
				 'issue', 1, 'issue-1', '/tmp/unknown', 'unknown', 'ready',
				 '__middleman_unknown__', 'acme', 'widget', 'acme/widget', 'issue:1'),
				('recovery', 'github', 'github.com', 'acme', 'widget',
				 'issue', 2, 'issue-2', '/tmp/recovery', 'recovery', 'ready',
				 '__middleman_recovery_pending__..state', 'acme', 'widget', 'acme/widget', 'issue:2')
		`)
		require.NoError(err)
	})

	database, err := Open(path)
	require.NoError(err)
	var legacySchemaObjects int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_schema
		WHERE name LIKE '%middleman%' OR tbl_name LIKE '%middleman%' OR sql LIKE '%middleman%'
	`).Scan(&legacySchemaObjects))
	assert.Zero(legacySchemaObjects)
	var temporarySchemaReferences int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_schema
		WHERE name LIKE '%_rename_legacy%'
		   OR tbl_name LIKE '%_rename_legacy%'
		   OR sql LIKE '%_rename_legacy%'
	`).Scan(&temporarySchemaReferences))
	assert.Zero(temporarySchemaReferences)
	_, err = database.WriteDB().Exec(`
		INSERT INTO forge_workspace_setup_events
			(workspace_id, stage, outcome, message)
		VALUES ('unknown', 'clone', 'success', 'ready');
		INSERT INTO forge_workspace_runtime_sessions
			(workspace_id, session_key, target_key, label, kind)
		VALUES ('unknown', 'session-1', 'plain_shell', 'Shell', 'plain_shell')
	`)
	require.NoError(err)
	var childRows int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM forge_workspace_setup_events) +
			(SELECT COUNT(*) FROM forge_workspace_runtime_sessions)
	`).Scan(&childRows))
	assert.Equal(2, childRows)
	rows, err := database.ReadDB().Query(`SELECT workspace_branch FROM forge_workspaces ORDER BY id`)
	require.NoError(err)
	var branches []string
	for rows.Next() {
		var branch string
		require.NoError(rows.Scan(&branch))
		branches = append(branches, branch)
	}
	require.NoError(rows.Close())
	assert.Equal([]string{"__kenn_forge_recovery_pending__..state", "__kenn_forge_unknown__"}, branches)
	assertDatabaseIntegrityForTest(t, database.ReadDB())
	require.NoError(database.Close())

	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(err)
	sourceDriver, err := iofs.New(migrationFiles, "migrations")
	require.NoError(err)
	databaseDriver, err := migratesqlite.WithInstance(raw, &migratesqlite.Config{MigrationsTable: migrationTableName})
	require.NoError(err)
	migrator, err := migrate.NewWithInstance("iofs", sourceDriver, "sqlite", databaseDriver)
	require.NoError(err)
	require.NoError(migrator.Migrate(43))
	assert.True(tableExistsForTest(t, raw, "middleman_workspaces"))
	assert.False(tableExistsForTest(t, raw, "forge_workspaces"))
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestRepositoryCatalogMigrationPreservesDataAndIndexes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "repository-catalog-v44.db")

	openAtVersionForTest(t, dbPath, 44, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				created_at
			) VALUES
				(1, 'github', 'github.com', 'provider-1',
				 'org-a', 'project-a', 'org-a/project-a',
				 'org-a', 'project-a', 'org-a/project-a',
				 '2026-01-01T00:00:00Z'),
				(2, 'github', 'github.com', '',
				 'org-a', 'legacy-project', 'org-a/legacy-project',
				 'org-a', 'legacy-project', 'org-a/legacy-project',
				 '2026-01-02T00:00:00Z');

			INSERT INTO forge_issues (
				id, repo_id, platform_id, number,
				created_at, updated_at, last_activity_at
			) VALUES (
				11, 1, 101, 1,
				'2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
				'2026-01-03T00:00:00Z'
			);

			INSERT INTO forge_merge_requests (
				id, repo_id, platform_id, number,
				created_at, updated_at, last_activity_at
			) VALUES (
				12, 1, 102, 2,
				'2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z',
				'2026-01-04T00:00:00Z'
			);

			INSERT INTO forge_archive_repos (
				repo_id, collection_mode, operator_state,
				created_at, updated_at
			) VALUES (
				1, 'full', 'active',
				'2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'
			);

			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (
				1, 'issue', 1, 'issue-101',
				'2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z', 'active'
			)
		`)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	rows, err := database.ReadDB().Query(`
		SELECT r.id, r.platform_repo_id, r.lifecycle_state,
		       rr.repo_path, rr.is_current
		FROM forge_repos r
		JOIN forge_repo_routes rr ON rr.repo_id = r.id
		ORDER BY r.id`)
	require.NoError(err)
	defer rows.Close()

	type migratedRoute struct {
		repoID         int64
		providerRepoID string
		lifecycle      string
		repoPath       string
		current        bool
	}
	var got []migratedRoute
	for rows.Next() {
		var row migratedRoute
		require.NoError(rows.Scan(
			&row.repoID,
			&row.providerRepoID,
			&row.lifecycle,
			&row.repoPath,
			&row.current,
		))
		got = append(got, row)
	}
	require.NoError(rows.Err())
	assert.Equal([]migratedRoute{
		{1, "provider-1", "active", "org-a/project-a", true},
		{2, "", "inactive", "org-a/legacy-project", false},
	}, got)

	var issueRepoID, mergeRequestRepoID, archiveRepoID, archiveItemRepoID int64
	require.NoError(database.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_issues WHERE id = 11`,
	).Scan(&issueRepoID))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_merge_requests WHERE id = 12`,
	).Scan(&mergeRequestRepoID))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_archive_repos WHERE repo_id = 1`,
	).Scan(&archiveRepoID))
	require.NoError(database.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_archive_items WHERE provider_item_id = 'issue-101'`,
	).Scan(&archiveItemRepoID))
	assert.Equal(int64(1), issueRepoID)
	assert.Equal(int64(1), mergeRequestRepoID)
	assert.Equal(int64(1), archiveRepoID)
	assert.Equal(int64(1), archiveItemRepoID)

	assertIndexForTest(t, database.ReadDB(), "forge_repos",
		"idx_repos_platform_repo_id",
		[]string{"platform", "platform_host", "platform_repo_id"}, true)
	assertIndexForTest(t, database.ReadDB(), "forge_repo_routes",
		"idx_repo_routes_current_path",
		[]string{"platform", "platform_host", "repo_path_key"}, true)
	assertIndexForTest(t, database.ReadDB(), "forge_repo_routes",
		"idx_repo_routes_current_repo", []string{"repo_id"}, true)
	assertUniqueIndexForTest(t, database.ReadDB(), "forge_repos",
		"idx_repos_platform_repo_id", true)
	assertUniqueIndexForTest(t, database.ReadDB(), "forge_repo_routes",
		"idx_repo_routes_current_path", true)
	assertUniqueIndexForTest(t, database.ReadDB(), "forge_repo_routes",
		"idx_repo_routes_current_repo", true)

	var removedPathIndex int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'index' AND name = 'idx_repos_provider_path_key'
	`).Scan(&removedPathIndex))
	assert.Zero(removedPathIndex)

	var repoTableSQL string
	require.NoError(database.ReadDB().QueryRow(`
		SELECT sql FROM sqlite_schema
		WHERE type = 'table' AND name = 'forge_repos'
	`).Scan(&repoTableSQL))
	assert.NotContains(repoTableSQL,
		"UNIQUE(platform, platform_host, owner, name)")
	for _, trigger := range []string{
		"forge_repos_casefold_insert",
		"forge_repos_casefold_update",
		"forge_workspaces_casefold_insert",
		"forge_workspaces_casefold_update",
	} {
		var triggerCount int
		require.NoError(database.ReadDB().QueryRow(`
			SELECT COUNT(*) FROM sqlite_schema
			WHERE type = 'trigger' AND name = ?`, trigger,
		).Scan(&triggerCount))
		assert.Equal(1, triggerCount)
	}
	assertDatabaseIntegrityForTest(t, database.ReadDB())
}

func TestRepositoryRouteGenerationMigrationClearsHistoricallyReusedRouteState(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "repository-route-generation-v46.db")

	openAtVersionForTest(t, dbPath, 46, func(raw *sql.DB) {
		_, err := raw.Exec(`
			INSERT INTO forge_repos (
				id, platform, platform_host, platform_repo_id,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				lifecycle_state
			) VALUES
				(1, 'github', 'github.com', 'provider-old',
				 'acme', 'renamed', 'acme/renamed',
				 'acme', 'renamed', 'acme/renamed', 'active'),
				(2, 'github', 'github.com', 'provider-current',
				 'acme', 'widget', 'acme/widget',
				 'acme', 'widget', 'acme/widget', 'active');

			INSERT INTO forge_repo_routes (
				repo_id, platform, platform_host,
				owner, name, repo_path, owner_key, name_key, repo_path_key,
				is_current, first_seen_at, last_seen_at
			) VALUES
				(1, 'github', 'github.com',
				 'acme', 'widget', 'acme/widget', 'acme', 'widget', 'acme/widget',
				 0, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z'),
				(1, 'github', 'github.com',
				 'acme', 'renamed', 'acme/renamed', 'acme', 'renamed', 'acme/renamed',
				 1, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
				(2, 'github', 'github.com',
				 'acme', 'widget', 'acme/widget', 'acme', 'widget', 'acme/widget',
				 1, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');

			INSERT INTO forge_notification_sync_watermarks (
				platform, platform_host, repo_owner, repo_name,
				last_successful_sync_at
			) VALUES
				('github', 'github.com', 'acme', 'widget', '2026-01-03T00:00:00Z'),
				('github', 'github.com', 'acme', 'safe', '2026-01-03T00:00:00Z');

			INSERT INTO forge_http_etags (
				platform, platform_host, owner_key, name_key,
				resource_type, resource_number, etag
			) VALUES
				('github', 'github.com', 'acme', 'widget', 'pull_request', 7, 'stale'),
				('github', 'github.com', 'acme', 'safe', 'pull_request', 8, 'keep');

			INSERT INTO forge_notification_items (
				platform, platform_host, platform_notification_id, repo_id,
				repo_owner, repo_name, subject_type, subject_title,
				reason, source_updated_at, synced_at
			) VALUES
				('github', 'github.com', 'reused-unlinked', NULL,
				 'acme', 'widget', 'PullRequest', 'stale',
				 'mention', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
				('github', 'github.com', 'reused-linked', 2,
				 'acme', 'widget', 'PullRequest', 'keep linked',
				 'mention', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
				('github', 'github.com', 'safe-unlinked', NULL,
				 'acme', 'safe', 'PullRequest', 'keep safe',
				 'mention', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');
		`)
		require.NoError(err)
	})

	database, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	entry, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "provider-current",
			Owner:          "acme", Name: "widget", RepoPath: "acme/widget",
		},
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(err)
	require.True(accepted)
	assert.Equal(int64(2), entry.Repository.ID)

	var reusedWatermarks, safeWatermarks int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_notification_sync_watermarks
		WHERE repo_owner = 'acme' AND repo_name = 'widget'
	`).Scan(&reusedWatermarks))
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_notification_sync_watermarks
		WHERE repo_owner = 'acme' AND repo_name = 'safe'
	`).Scan(&safeWatermarks))
	assert.Zero(reusedWatermarks)
	assert.Equal(1, safeWatermarks)

	var reusedETags, safeETags int
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_http_etags
		WHERE owner_key = 'acme' AND name_key = 'widget'
	`).Scan(&reusedETags))
	require.NoError(database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_http_etags
		WHERE owner_key = 'acme' AND name_key = 'safe'
	`).Scan(&safeETags))
	assert.Zero(reusedETags)
	assert.Equal(1, safeETags)

	rows, err := database.ReadDB().Query(`
		SELECT platform_notification_id
		FROM forge_notification_items
		ORDER BY platform_notification_id
	`)
	require.NoError(err)
	defer rows.Close()
	var notificationIDs []string
	for rows.Next() {
		var id string
		require.NoError(rows.Scan(&id))
		notificationIDs = append(notificationIDs, id)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"reused-linked", "safe-unlinked"}, notificationIDs)

	var minimumGeneration int64
	require.NoError(database.ReadDB().QueryRow(`
		SELECT MIN(generation) FROM forge_repo_routes
	`).Scan(&minimumGeneration))
	assert.Equal(int64(1), minimumGeneration)
	assertDatabaseIntegrityForTest(t, database.ReadDB())
}

func TestRepositoryCatalogMigrationDownRestoresRouteIdentity(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "repository-catalog-v45.db")
	database, err := Open(dbPath)
	require.NoError(err)
	_, err = database.WriteDB().Exec(`
		INSERT INTO forge_repos (
			platform, platform_host, platform_repo_id,
			owner, name, repo_path, owner_key, name_key, repo_path_key
		) VALUES (
			'github', 'github.com', 'provider-1',
			'org-a', 'project-a', 'org-a/project-a',
			'org-a', 'project-a', 'org-a/project-a'
		)`)
	require.NoError(err)
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	raw.SetMaxOpenConns(1)
	_, err = raw.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(err)
	require.NoError(migrator.Migrate(44))
	_, err = raw.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(err)

	assert.False(t, tableExistsForTest(t, raw, "forge_repo_routes"))
	hasLifecycle, err := hasColumn(raw, "forge_repos", "lifecycle_state")
	require.NoError(err)
	assert.False(t, hasLifecycle)
	assertIndexForTest(t, raw, "forge_repos", "idx_repos_platform_repo_id",
		[]string{"platform", "platform_host", "platform_repo_id"}, true)
	assertUniqueIndexForTest(t, raw, "forge_repos",
		"idx_repos_platform_repo_id", true)
	assertIndexForTest(t, raw, "forge_repos", "idx_repos_provider_path_key",
		[]string{"platform", "platform_host", "repo_path_key"}, true)
	assertUniqueIndexForTest(t, raw, "forge_repos",
		"idx_repos_provider_path_key", true)
	_, err = raw.Exec(`
		INSERT INTO forge_repos (
			platform, platform_host, platform_repo_id,
			owner, name, repo_path, owner_key, name_key, repo_path_key
		) VALUES (
			'github', 'github.com', 'provider-2',
			'org-a', 'project-a', 'org-a/project-b',
			'org-a', 'project-b', 'org-a/project-b'
		)`)
	require.Error(err)
	assertDatabaseIntegrityForTest(t, raw)
	require.NoError(raw.Close())
}

func TestRepositoryCatalogMigrationDownRejectsRouteCollisions(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "repository-catalog-collision-v45.db")
	database, err := Open(dbPath)
	require.NoError(err)
	_, err = database.WriteDB().Exec(`
		INSERT INTO forge_repos (
			id, platform, platform_host, platform_repo_id,
			owner, name, repo_path, owner_key, name_key, repo_path_key,
			lifecycle_state
		) VALUES
			(1, 'github', 'github.com', 'provider-old',
			 'org-a', 'project-a', 'org-a/project-a',
			 'org-a', 'project-a', 'org-a/project-a', 'inactive'),
			(2, 'github', 'github.com', 'provider-new',
			 'org-a', 'project-a', 'org-a/project-a',
			 'org-a', 'project-a', 'org-a/project-a', 'active');
		INSERT INTO forge_repo_routes (
			repo_id, platform, platform_host,
			owner, name, repo_path, owner_key, name_key, repo_path_key,
			is_current, first_seen_at, last_seen_at
		) VALUES
			(1, 'github', 'github.com',
			 'org-a', 'project-a', 'org-a/project-a',
			 'org-a', 'project-a', 'org-a/project-a',
			 0, '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z'),
			(2, 'github', 'github.com',
			 'org-a', 'project-a', 'org-a/project-a',
			 'org-a', 'project-a', 'org-a/project-a',
			 1, '2026-02-02T00:00:00Z', '2026-02-02T00:00:00Z');
		INSERT INTO forge_issues (
			id, repo_id, platform_id, number,
			created_at, updated_at, last_activity_at
		) VALUES (
			11, 1, 101, 1,
			'2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z',
			'2026-01-03T00:00:00Z'
		)`)
	require.NoError(err)
	require.NoError(database.Close())

	raw, migrator := openMigratorForTest(t, dbPath)
	raw.SetMaxOpenConns(1)
	_, err = raw.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(err)
	err = migrator.Migrate(44)
	require.Error(err)
	_, enableErr := raw.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(enableErr)

	var repositoryCount, issueCount int
	require.NoError(raw.QueryRow(`SELECT COUNT(*) FROM forge_repos`).Scan(
		&repositoryCount,
	))
	require.NoError(raw.QueryRow(`SELECT COUNT(*) FROM forge_issues`).Scan(
		&issueCount,
	))
	assert.Equal(t, 2, repositoryCount)
	assert.Equal(t, 1, issueCount)
	require.NoError(raw.Close())
}

func TestOpenMigratesRateLimitsToPrincipals(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "rate-v38.db")
	openAtVersionForTest(t, path, 38, func(raw *sql.DB) {
		hour := time.Now().UTC().Truncate(time.Hour)
		_, err := raw.Exec(`
			INSERT INTO middleman_rate_limits
			    (platform, platform_host, api_type, requests_hour, hour_start,
			     rate_remaining, rate_limit, updated_at)
			VALUES
			    ('github', 'github.com', 'rest', 4, ?, 4996, 5000, datetime('now')),
			    ('gitlab', 'gitlab.example.com', 'rest', 2, ?, 598, 600, datetime('now'))`,
			hour, hour,
		)
		require.NoError(err)
		_, err = raw.Exec(`
			INSERT INTO middleman_notification_sync_watermarks
			    (platform, platform_host, last_successful_sync_at, sync_cursor, tracked_repos_key)
			VALUES ('github', 'github.com', '2026-05-01T10:00:00Z', '', 'github/github.com/acme/widget')`)
		require.NoError(err)
	})

	database, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	var principal string
	err = database.ReadDB().QueryRow(`
		SELECT rate_principal FROM forge_rate_limits
		WHERE platform = 'gitlab' AND platform_host = 'gitlab.example.com'
	`).Scan(&principal)
	require.NoError(err)
	require.Equal("host", principal)

	var githubRows int
	err = database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_rate_limits WHERE platform = 'github'
	`).Scan(&githubRows)
	require.NoError(err)
	require.Zero(githubRows)

	var watermarkRows int
	err = database.ReadDB().QueryRow(`
		SELECT COUNT(*) FROM forge_notification_sync_watermarks
	`).Scan(&watermarkRows)
	require.NoError(err)
	require.Zero(watermarkRows,
		"host-wide watermarks cannot be attributed to repositories and are dropped")
	require.NoError(database.UpdateNotificationSyncWatermark(
		t.Context(), "github", "github.com", "acme", "widget",
		time.Now().UTC(), nil,
	))

	var integrity string
	require.NoError(database.ReadDB().QueryRow(`PRAGMA integrity_check`).Scan(&integrity))
	require.Equal("ok", integrity)
	rows, err := database.ReadDB().Query(`PRAGMA foreign_key_check`)
	require.NoError(err)
	defer rows.Close()
	require.False(rows.Next())
}

func TestOpenBackfillsLegacyIssueLabelsIntoNormalizedTables(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	path, raw := openSchemaVersion4DBForTest(t)
	defer func() { require.NoError(raw.Close()) }()
	seedLegacyIssueForTest(t, raw, 1, 1, 101, 7, `[{"name":"bug","color":"d73a4a"}]`)

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })

	var issueLabelCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_issue_labels WHERE issue_id = ?`, 1).Scan(&issueLabelCount)
	require.NoError(err)
	require.Equal(1, issueLabelCount)

	var platformID sql.NullInt64
	var name string
	var description string
	var color string
	var isDefault bool
	var updatedAt string
	err = d.ReadDB().QueryRow(
		`SELECT l.platform_id, l.name, l.description, l.color, l.is_default, l.updated_at
		 FROM forge_labels l
		 JOIN forge_issue_labels il ON il.label_id = l.id
		 WHERE il.issue_id = ?`,
		1,
	).Scan(&platformID, &name, &description, &color, &isDefault, &updatedAt)
	require.NoError(err)
	require.False(platformID.Valid)
	require.Equal("bug", name)
	require.Empty(description)
	require.Equal("d73a4a", color)
	require.False(isDefault)
	require.NotEmpty(updatedAt)
}

func TestOpenIgnoresMalformedLegacyIssueLabelsJSON(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	path, raw := openSchemaVersion4DBForTest(t)
	defer func() { require.NoError(raw.Close()) }()

	seedLegacyIssueForTest(t, raw, 1, 1, 101, 7, `[{"name":"bug","color":"d73a4a"}`)

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })

	var labelCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_labels`).Scan(&labelCount)
	require.NoError(err)
	require.Equal(0, labelCount)

	var issueLabelCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_issue_labels`).Scan(&issueLabelCount)
	require.NoError(err)
	require.Equal(0, issueLabelCount)
}

func TestOpenBackfillsDuplicateLegacyIssueLabelsDeterministically(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	path, raw := openSchemaVersion4DBForTest(t)
	defer func() { require.NoError(raw.Close()) }()

	seedLegacyIssueForTest(t, raw, 1, 1, 101, 7, `[{"name":"bug","color":"ff0000"}]`)
	seedLegacyIssueForTest(t, raw, 2, 1, 102, 8, `[{"name":"bug","color":"00ff00"}]`)

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })

	var labelCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_labels WHERE repo_id = ? AND name = ?`, 1, "bug").Scan(&labelCount)
	require.NoError(err)
	require.Equal(1, labelCount)

	var color string
	err = d.ReadDB().QueryRow(
		`SELECT color FROM forge_labels WHERE repo_id = ? AND name = ?`,
		1,
		"bug",
	).Scan(&color)
	require.NoError(err)
	require.Equal("00ff00", color)
}

func TestOpenCasefoldsDuplicateRepositoryRows(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(legacySchemaSQLForTest(t, 7))
	require.NoError(err)
	_, err = raw.Exec(`CREATE TABLE schema_migrations (version uint64, dirty bool)`)
	require.NoError(err)
	_, err = raw.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (7, FALSE)`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_repos (
			id, platform, platform_host, owner, name,
			created_at, backfill_pr_page, backfill_pr_complete,
			backfill_issue_page, backfill_issue_complete
		) VALUES
			(1, 'github', 'github.com', 'Org', 'Foo', datetime('now'), 0, 0, 0, 0),
			(2, 'github', 'github.com', 'org', 'foo', datetime('now'), 0, 0, 0, 0),
			(3, 'github', 'github.com', 'ORG', 'FOO', datetime('now'), 0, 0, 0, 0)`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_merge_requests (
			id, repo_id, platform_id, number, url, title, author, state,
			created_at, updated_at, last_activity_at
		) VALUES
			(1, 1, 100, 1, 'https://github.com/Org/Foo/pull/1', 'PR', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(2, 2, 100, 1, 'https://github.com/org/foo/pull/1', 'PR', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(4, 3, 100, 1, 'https://github.com/ORG/FOO/pull/1', 'PR', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(3, 2, 200, 2, 'https://github.com/org/foo/pull/2', 'Unique PR', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now'))`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_mr_events (
			merge_request_id, event_type, author, created_at, dedupe_key
		) VALUES
			(2, 'comment', 'octo', datetime('now'), 'duplicate-pr-comment'),
			(4, 'comment', 'octo', datetime('now'), 'duplicate-pr-comment'),
			(3, 'comment', 'octo', datetime('now'), 'unique-comment')`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_kanban_state (merge_request_id, status, updated_at)
		VALUES
			(1, 'new', '2024-01-01T00:00:00Z'),
			(2, 'reviewing', '2024-01-02T00:00:00Z'),
			(3, 'reviewing', '2024-01-03T00:00:00Z')`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_issues (
			id, repo_id, platform_id, number, url, title, author, state,
			created_at, updated_at, last_activity_at
		) VALUES
			(1, 1, 800, 8, 'https://github.com/Org/Foo/issues/8', 'Issue', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(2, 2, 800, 8, 'https://github.com/org/foo/issues/8', 'Issue', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(4, 3, 800, 8, 'https://github.com/ORG/FOO/issues/8', 'Issue', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now')),
			(3, 2, 900, 9, 'https://github.com/org/foo/issues/9', 'Unique issue', 'octo', 'open',
			 datetime('now'), datetime('now'), datetime('now'))`)
	require.NoError(err)
	_, err = raw.Exec(`
		DROP INDEX idx_issue_events_created;
		ALTER TABLE middleman_issue_events RENAME TO middleman_issue_events_strict;
		CREATE TABLE middleman_issue_events (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			issue_id     INTEGER NOT NULL REFERENCES middleman_issues(id) ON DELETE CASCADE,
			platform_id  INTEGER,
			event_type   TEXT NOT NULL,
			author       TEXT NOT NULL,
			summary      TEXT NOT NULL DEFAULT '',
			body         TEXT,
			metadata_json TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL,
			dedupe_key   TEXT NOT NULL,
			UNIQUE(issue_id, dedupe_key)
		);
		CREATE INDEX idx_issue_events_created
			ON middleman_issue_events(issue_id, created_at DESC);
		DROP TABLE middleman_issue_events_strict;`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_issue_events (
			issue_id, event_type, author, created_at, dedupe_key
		) VALUES
			(2, 'comment', 'octo', datetime('now'), 'duplicate-issue-comment'),
			(4, 'comment', 'octo', datetime('now'), 'duplicate-issue-comment')`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_labels (
			id, repo_id, platform_id, name, updated_at
		) VALUES
			(1, 1, 700, 'enhancement-renamed', datetime('now')),
			(2, 2, 700, 'enhancement', datetime('now')),
			(3, 2, 701, 'triage', datetime('now')),
			(4, 1, NULL, 'stale-label', datetime('now')),
			(5, 2, 702, 'stale-label', datetime('now'))`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_issue_labels (issue_id, label_id)
		VALUES
			(2, 3),
			(3, 2)`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_merge_request_labels (merge_request_id, label_id)
		VALUES (3, 2)`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_starred_items (item_type, repo_id, number)
		VALUES ('issue', 2, 9)`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_stacks (id, repo_id, base_number, name)
		VALUES (1, 2, 2, 'Unique stack')`)
	require.NoError(err)
	_, err = raw.Exec(`
		INSERT INTO middleman_workspaces (
			id, platform_host, repo_owner, repo_name, mr_number, mr_head_ref,
			worktree_path, tmux_session
		) VALUES
			('one', 'github.com', 'Org', 'Foo', 1, 'feature', '/tmp/one', 'one'),
			('two', 'github.com', 'org', 'foo', 1, 'feature', '/tmp/two', 'two'),
			('three', 'github.com', 'org', 'foo', 2, 'feature-2', '/tmp/three', 'three')`)
	require.NoError(err)
	require.NoError(raw.Close())

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })

	repos, err := d.ListRepositoryCatalog(
		t.Context(), RepositoryCatalogFilter{},
	)
	require.NoError(err)
	require.Len(repos, 1)
	require.Equal("org", repos[0].Repository.Owner)
	require.Equal("foo", repos[0].Repository.Name)
	require.Equal(RepositoryLifecycleInactive, repos[0].Lifecycle)

	var prCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_merge_requests`).Scan(&prCount)
	require.NoError(err)
	require.Equal(2, prCount)

	var uniquePRRepoID int
	err = d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_merge_requests WHERE number = 2`,
	).Scan(&uniquePRRepoID)
	require.NoError(err)
	require.Equal(1, uniquePRRepoID)

	var uniquePREventCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_mr_events e
		JOIN forge_merge_requests mr ON mr.id = e.merge_request_id
		WHERE mr.number = 2`,
	).Scan(&uniquePREventCount)
	require.NoError(err)
	require.Equal(1, uniquePREventCount)

	var duplicatePREventCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_mr_events e
		JOIN forge_merge_requests mr ON mr.id = e.merge_request_id
		WHERE mr.number = 1 AND e.dedupe_key = 'duplicate-pr-comment'`,
	).Scan(&duplicatePREventCount)
	require.NoError(err)
	require.Equal(1, duplicatePREventCount)

	var kanbanStatus string
	err = d.ReadDB().QueryRow(`
		SELECT ws.status
		FROM forge_item_workflow_state ws
		JOIN forge_merge_requests mr
			ON mr.repo_id = ws.repo_id AND mr.number = ws.item_number
		WHERE ws.item_type = 'pr'
		  AND mr.number = 2`,
	).Scan(&kanbanStatus)
	require.NoError(err)
	require.Equal("reviewing", kanbanStatus)

	var mergedKanbanStatus string
	err = d.ReadDB().QueryRow(`
		SELECT ws.status
		FROM forge_item_workflow_state ws
		JOIN forge_merge_requests mr
			ON mr.repo_id = ws.repo_id AND mr.number = ws.item_number
		WHERE ws.item_type = 'pr'
		  AND mr.number = 1`,
	).Scan(&mergedKanbanStatus)
	require.NoError(err)
	require.Equal("reviewing", mergedKanbanStatus)

	var duplicateIssueEventCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_issue_events e
		JOIN forge_issues i ON i.id = e.issue_id
		WHERE i.number = 8 AND e.dedupe_key = 'duplicate-issue-comment'`,
	).Scan(&duplicateIssueEventCount)
	require.NoError(err)
	require.Equal(1, duplicateIssueEventCount)

	var duplicateIssueLabelCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_issue_labels il
		JOIN forge_issues i ON i.id = il.issue_id
		JOIN forge_labels l ON l.id = il.label_id
		WHERE i.number = 8 AND l.name = 'triage'`,
	).Scan(&duplicateIssueLabelCount)
	require.NoError(err)
	require.Equal(1, duplicateIssueLabelCount)

	var issueRepoID int
	err = d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_issues WHERE number = 9`,
	).Scan(&issueRepoID)
	require.NoError(err)
	require.Equal(1, issueRepoID)

	var labelRepoID int
	err = d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_labels WHERE platform_id = 700`,
	).Scan(&labelRepoID)
	require.NoError(err)
	require.Equal(1, labelRepoID)

	var issuePlatformLabelCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_issue_labels il
		JOIN forge_issues i ON i.id = il.issue_id
		JOIN forge_labels l ON l.id = il.label_id
		WHERE i.number = 9 AND l.platform_id = 700`,
	).Scan(&issuePlatformLabelCount)
	require.NoError(err)
	require.Equal(1, issuePlatformLabelCount)

	var staleNamePlatformLabelCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_labels
		WHERE repo_id = 1 AND name = 'stale-label' AND platform_id = 702`,
	).Scan(&staleNamePlatformLabelCount)
	require.NoError(err)
	require.Equal(1, staleNamePlatformLabelCount)

	var mrPlatformLabelCount int
	err = d.ReadDB().QueryRow(`
		SELECT COUNT(*)
		FROM forge_merge_request_labels mrl
		JOIN forge_merge_requests mr ON mr.id = mrl.merge_request_id
		JOIN forge_labels l ON l.id = mrl.label_id
		WHERE mr.number = 2 AND l.platform_id = 700`,
	).Scan(&mrPlatformLabelCount)
	require.NoError(err)
	require.Equal(1, mrPlatformLabelCount)

	var starredRepoID int
	err = d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_starred_items WHERE item_type = 'issue' AND number = 9`,
	).Scan(&starredRepoID)
	require.NoError(err)
	require.Equal(1, starredRepoID)

	var stackRepoID int
	err = d.ReadDB().QueryRow(
		`SELECT repo_id FROM forge_stacks WHERE base_number = 2`,
	).Scan(&stackRepoID)
	require.NoError(err)
	require.Equal(1, stackRepoID)

	var workspaceCount int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM forge_workspaces`).Scan(&workspaceCount)
	require.NoError(err)
	require.Equal(2, workspaceCount)

	var integrityCheck string
	err = d.ReadDB().QueryRow(`PRAGMA integrity_check`).Scan(&integrityCheck)
	require.NoError(err)
	require.Equal("ok", integrityCheck)

	var foreignKeyViolations int
	err = d.ReadDB().QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations)
	require.NoError(err)
	require.Zero(foreignKeyViolations)
}

func TestOpenRepairsCurrentSchemaMissingWorkspaceTerminalBackend(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "broken-terminal-backend.db")

	d, err := Open(path)
	require.NoError(err)
	require.NoError(d.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(`ALTER TABLE forge_workspaces DROP COLUMN terminal_backend`)
	require.NoError(err)
	require.NoError(raw.Close())

	reopened, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })

	var terminalBackendColumn string
	err = reopened.ReadDB().QueryRow(
		`SELECT name
		 FROM pragma_table_info('forge_workspaces')
		 WHERE name = ?`,
		"terminal_backend",
	).Scan(&terminalBackendColumn)
	require.NoError(err)
	require.Equal("terminal_backend", terminalBackendColumn)

	err = reopened.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-terminal-backend",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/backend",
		WorktreePath:    "/tmp/ws-terminal-backend",
		TmuxSession:     "kenn-forge-ws-terminal-backend",
		Status:          "creating",
		WorkspaceBranch: "feature/backend",
	})
	require.NoError(err)
}

func TestOpenInitializesBranchActivitySchema(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "branch-activity.db")

	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })

	for table, columns := range map[string][]string{
		"forge_branch_commits":      {"observed_order", "created_at", "updated_at"},
		"forge_branch_tips":         {"created_at", "updated_at"},
		"forge_branch_force_pushes": {"before_observed_at", "created_at"},
	} {
		for _, column := range columns {
			hasColumn, err := hasColumn(d.ReadDB(), table, column)
			require.NoError(err)
			require.Truef(hasColumn, "%s.%s should exist after migration", table, column)
		}
	}

	repoID, err := d.UpsertRepo(ctx, GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NoError(d.UpsertBranchCommits(ctx, []BranchCommit{{
		RepoID:         repoID,
		BranchName:     "main",
		CommitSHA:      "shared-sha",
		AuthorName:     "Alice",
		AuthorEmail:    "alice@example.com",
		AuthoredAt:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		CommitterName:  "Alice",
		CommitterEmail: "alice@example.com",
		CommittedAt:    time.Date(2026, 5, 1, 12, 1, 0, 0, time.UTC),
		Subject:        "main work",
	}, {
		RepoID:         repoID,
		BranchName:     "release",
		CommitSHA:      "shared-sha",
		AuthorName:     "Alice",
		AuthorEmail:    "alice@example.com",
		AuthoredAt:     time.Date(2026, 5, 1, 12, 3, 0, 0, time.UTC),
		CommitterName:  "Alice",
		CommitterEmail: "alice@example.com",
		CommittedAt:    time.Date(2026, 5, 1, 12, 4, 0, 0, time.UTC),
		Subject:        "release work",
	}}))
	require.NoError(d.InsertBranchForcePush(ctx, BranchForcePush{
		RepoID:     repoID,
		BranchName: "main",
		BeforeSHA:  "before-sha",
		AfterSHA:   "after-sha",
		DetectedAt: time.Date(2026, 5, 1, 12, 2, 0, 0, time.UTC),
	}))
	require.NoError(d.InsertBranchForcePush(ctx, BranchForcePush{
		RepoID:     repoID,
		BranchName: "main",
		BeforeSHA:  "before-sha",
		AfterSHA:   "after-sha",
		DetectedAt: time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC),
	}))

	var commitRows int
	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM forge_branch_commits
		WHERE repo_id = ? AND commit_sha = ?`,
		repoID,
		"shared-sha",
	).Scan(&commitRows)
	require.NoError(err)
	require.Equal(2, commitRows)

	var forcePushRows int
	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM forge_branch_force_pushes
		WHERE repo_id = ? AND before_sha = ? AND after_sha = ?`,
		repoID,
		"before-sha",
		"after-sha",
	).Scan(&forcePushRows)
	require.NoError(err)
	require.Equal(2, forcePushRows)
}

func TestRepoTimestampWritesStoreUTC(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID, err := d.UpsertRepo(ctx, GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	//nolint:forbidigo // Test fixture intentionally uses a non-UTC zone to verify normalization.
	edt := time.FixedZone("EDT", -4*60*60)
	startedAt := time.Date(2026, 4, 11, 8, 0, 0, 0, edt)
	completedAt := time.Date(2026, 4, 11, 8, 30, 0, 0, edt)
	require.NoError(d.UpdateRepoSyncStarted(ctx, repoID, startedAt))
	require.NoError(d.UpdateRepoSyncCompleted(ctx, repoID, completedAt, ""))

	rows, err := d.ReadDB().QueryContext(ctx, `
		SELECT last_sync_started_at FROM forge_repos WHERE id = ?
		UNION ALL
		SELECT last_sync_completed_at FROM forge_repos WHERE id = ?`,
		repoID, repoID,
	)
	require.NoError(err)
	defer rows.Close()
	for rows.Next() {
		var value string
		require.NoError(rows.Scan(&value))
		require.NotContains(value, "EDT")
		require.NotContains(value, "-0400")
	}
	require.NoError(rows.Err())

	repo, err := d.GetRepoByID(ctx, repoID)
	require.NoError(err)
	require.NotNil(repo)
	require.NotNil(repo.LastSyncStartedAt)
	require.NotNil(repo.LastSyncCompletedAt)
	require.Equal(time.UTC, repo.LastSyncStartedAt.Location())
	require.Equal(time.UTC, repo.LastSyncCompletedAt.Location())
	require.Equal(startedAt.UTC(), *repo.LastSyncStartedAt)
	require.Equal(completedAt.UTC(), *repo.LastSyncCompletedAt)
}

func TestOpenRejectsUnsupportedLegacySchemaVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
	}{
		{name: "version_0", version: 0},
		{name: "version_99", version: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRejectsUnsupportedLegacySchemaVersion(t, tc.version)
		})
	}
}

func TestOpenReturnsRecreateGuidanceForDirtyMigrations(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(
		`CREATE TABLE schema_migrations (version uint64, dirty bool)`,
	)
	require.NoError(err)
	_, err = raw.Exec(
		`INSERT INTO schema_migrations (version, dirty) VALUES (1, TRUE)`,
	)
	require.NoError(err)
	require.NoError(raw.Close())

	_, err = Open(path)
	require.Error(err)
	require.Contains(err.Error(), recreateDatabaseInstruction)
}

func TestOpenRejectsIncompleteLegacyDatabase(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "broken-legacy.db")

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(`CREATE TABLE middleman_repos (id INTEGER PRIMARY KEY)`)
	require.NoError(err)
	require.NoError(raw.Close())

	_, err = Open(path)
	require.Error(err)
	require.Contains(err.Error(), recreateDatabaseInstruction)
}

func testRejectsUnsupportedLegacySchemaVersion(t *testing.T, version int) {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(legacySchemaSQLForTest(t, 3))
	require.NoError(err)
	_, err = raw.Exec(
		`CREATE TABLE middleman_schema_version (version INTEGER NOT NULL)`,
	)
	require.NoError(err)
	_, err = raw.Exec(
		`INSERT INTO middleman_schema_version (version) VALUES (?)`,
		version,
	)
	require.NoError(err)
	require.NoError(raw.Close())

	_, err = Open(path)
	require.Error(err)
	if version == 0 {
		require.Contains(err.Error(), recreateDatabaseInstruction)
		require.Contains(err.Error(), "is invalid")
		return
	}
	require.Contains(err.Error(), "newer than this binary")
}

func legacySchemaSQLForTest(t *testing.T, version int) string {
	t.Helper()
	parts := make([]string, 0, version)
	for i := 1; i <= version; i++ {
		contents, err := fs.ReadFile(
			migrationFiles,
			path.Join("migrations", legacyMigrationFilenameForTest(i)),
		)
		require.NoError(t, err)
		parts = append(parts, string(contents))
	}
	return strings.Join(parts, "\n")
}

func legacyMigrationFilenameForTest(version int) string {
	switch version {
	case 1:
		return "000001_initial_schema.up.sql"
	case 2:
		return "000002_update_mr_events_dedupe.up.sql"
	case 3:
		return "000003_add_backfill_and_detail_columns.up.sql"
	case 4:
		return "000004_drop_legacy_schema_version.up.sql"
	case 5:
		return "000005_graphql_sync_and_labels.up.sql"
	case 6:
		return "000006_add_stacks.up.sql"
	case 7:
		return "000007_add_workspaces.up.sql"
	default:
		return ""
	}
}

func latestMigrationVersionForTest(t *testing.T) int {
	t.Helper()
	version, err := latestMigrationVersion()
	require.NoError(t, err)
	return version
}

func tableExistsForTest(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name,
	).Scan(&count)
	require.NoError(t, err)
	return count > 0
}

func assertIndexForTest(
	t *testing.T,
	db *sql.DB,
	table string,
	index string,
	expectedColumns []string,
	partial bool,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	var actualPartial bool
	err := db.QueryRow(`
		SELECT partial
		FROM pragma_index_list(?)
		WHERE name = ?`, table, index,
	).Scan(&actualPartial)
	require.NoError(err)
	assert.Equal(partial, actualPartial)

	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	require.NoError(err)
	defer rows.Close()

	actualColumns := make([]string, 0, len(expectedColumns))
	for rows.Next() {
		var column string
		require.NoError(rows.Scan(&column))
		actualColumns = append(actualColumns, column)
	}
	require.NoError(rows.Err())
	assert.Equal(expectedColumns, actualColumns)
}

func assertUniqueIndexForTest(
	t *testing.T,
	db *sql.DB,
	table string,
	index string,
	expectedUnique bool,
) {
	t.Helper()
	var actualUnique bool
	err := db.QueryRow(`
		SELECT "unique"
		FROM pragma_index_list(?)
		WHERE name = ?`, table, index,
	).Scan(&actualUnique)
	require.NoError(t, err)
	assert.Equal(t, expectedUnique, actualUnique)
}

func archiveItemInsertForTest(
	itemType string,
	itemNumber int,
	providerItemID string,
	lifecycleState string,
) string {
	return fmt.Sprintf(`INSERT INTO forge_archive_items (
		repo_id, item_type, item_number, provider_item_id,
		provider_created_at, provider_updated_at, lifecycle_state
	) VALUES (
		1, '%s', %d, '%s', datetime('now'), datetime('now'), '%s'
	)`,
		itemType,
		itemNumber,
		providerItemID,
		lifecycleState,
	)
}

type historicalArchiveItemSnapshotForTest struct {
	ID                 int64
	RepoID             int64
	PlatformID         int64
	PlatformExternalID string
	Number             int
	CreatedAt          string
	UpdatedAt          string
	LastActivityAt     string
}

type historicalArchiveDraftSnapshotForTest struct {
	ID             int64
	MergeRequestID int64
	Body           string
	Action         string
	CreatedAt      string
	UpdatedAt      string
}

type historicalArchiveWorktreeSnapshotForTest struct {
	ID             int64
	MergeRequestID int64
	WorktreeKey    string
	WorktreePath   string
	WorktreeBranch string
	LinkedAt       string
}

type historicalArchiveStackSnapshotForTest struct {
	ID             int64
	RepoID         int64
	BaseNumber     int
	Name           string
	CreatedAt      string
	UpdatedAt      string
	MemberStackID  int64
	MergeRequestID int64
	Position       int
}

type historicalArchiveWorkflowSnapshotForTest struct {
	RepoID        int64
	ItemType      string
	ItemNumber    int
	Status        string
	UpdatedAt     string
	UpdatedSource string
	UpdatedActor  string
	UpdatedReason string
}

type historicalArchiveUpgradeSnapshotForTest struct {
	Issue        historicalArchiveItemSnapshotForTest
	MergeRequest historicalArchiveItemSnapshotForTest
	Draft        historicalArchiveDraftSnapshotForTest
	Worktree     historicalArchiveWorktreeSnapshotForTest
	Stack        historicalArchiveStackSnapshotForTest
	Workflows    []historicalArchiveWorkflowSnapshotForTest
}

func readHistoricalArchiveUpgradeSnapshotForTest(
	t *testing.T,
	db *sql.DB,
	tablePrefix string,
) historicalArchiveUpgradeSnapshotForTest {
	t.Helper()
	require := require.New(t)
	var snapshot historicalArchiveUpgradeSnapshotForTest

	for query, item := range map[string]*historicalArchiveItemSnapshotForTest{
		fmt.Sprintf(`SELECT
			id, repo_id, platform_id, platform_external_id, number,
			CAST(created_at AS TEXT), CAST(updated_at AS TEXT), CAST(last_activity_at AS TEXT)
			FROM %s_issues`, tablePrefix): &snapshot.Issue,
		fmt.Sprintf(`SELECT
			id, repo_id, platform_id, platform_external_id, number,
			CAST(created_at AS TEXT), CAST(updated_at AS TEXT), CAST(last_activity_at AS TEXT)
			FROM %s_merge_requests`, tablePrefix): &snapshot.MergeRequest,
	} {
		err := db.QueryRow(query).Scan(
			&item.ID,
			&item.RepoID,
			&item.PlatformID,
			&item.PlatformExternalID,
			&item.Number,
			&item.CreatedAt,
			&item.UpdatedAt,
			&item.LastActivityAt,
		)
		require.NoError(err)
	}

	err := db.QueryRow(fmt.Sprintf(`SELECT
		id, merge_request_id, body, action,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM %s_mr_review_drafts`, tablePrefix)).Scan(
		&snapshot.Draft.ID,
		&snapshot.Draft.MergeRequestID,
		&snapshot.Draft.Body,
		&snapshot.Draft.Action,
		&snapshot.Draft.CreatedAt,
		&snapshot.Draft.UpdatedAt,
	)
	require.NoError(err)

	err = db.QueryRow(fmt.Sprintf(`SELECT
		id, merge_request_id, worktree_key, worktree_path, worktree_branch,
		CAST(linked_at AS TEXT)
		FROM %s_mr_worktree_links`, tablePrefix)).Scan(
		&snapshot.Worktree.ID,
		&snapshot.Worktree.MergeRequestID,
		&snapshot.Worktree.WorktreeKey,
		&snapshot.Worktree.WorktreePath,
		&snapshot.Worktree.WorktreeBranch,
		&snapshot.Worktree.LinkedAt,
	)
	require.NoError(err)

	err = db.QueryRow(fmt.Sprintf(`SELECT
		s.id, s.repo_id, s.base_number, s.name,
		CAST(s.created_at AS TEXT), CAST(s.updated_at AS TEXT),
		sm.stack_id, sm.merge_request_id, sm.position
		FROM %[1]s_stacks s
		JOIN %[1]s_stack_members sm ON sm.stack_id = s.id`, tablePrefix)).Scan(
		&snapshot.Stack.ID,
		&snapshot.Stack.RepoID,
		&snapshot.Stack.BaseNumber,
		&snapshot.Stack.Name,
		&snapshot.Stack.CreatedAt,
		&snapshot.Stack.UpdatedAt,
		&snapshot.Stack.MemberStackID,
		&snapshot.Stack.MergeRequestID,
		&snapshot.Stack.Position,
	)
	require.NoError(err)

	rows, err := db.Query(fmt.Sprintf(`SELECT
		repo_id, item_type, item_number, status, CAST(updated_at AS TEXT),
		updated_source, updated_actor, updated_reason
		FROM %s_item_workflow_state
		ORDER BY item_type, item_number`, tablePrefix))
	require.NoError(err)
	defer rows.Close()
	for rows.Next() {
		var workflow historicalArchiveWorkflowSnapshotForTest
		require.NoError(rows.Scan(
			&workflow.RepoID,
			&workflow.ItemType,
			&workflow.ItemNumber,
			&workflow.Status,
			&workflow.UpdatedAt,
			&workflow.UpdatedSource,
			&workflow.UpdatedActor,
			&workflow.UpdatedReason,
		))
		snapshot.Workflows = append(snapshot.Workflows, workflow)
	}
	require.NoError(rows.Err())

	return snapshot
}

func openAtVersionForTest(t *testing.T, dbPath string, version uint, seed func(*sql.DB)) {
	t.Helper()
	require := require.New(t)

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	require.NoError(err)

	sourceDriver, err := iofs.New(migrationFiles, "migrations")
	require.NoError(err)
	databaseDriver, err := migratesqlite.WithInstance(raw, &migratesqlite.Config{
		MigrationsTable: migrationTableName,
	})
	require.NoError(err)
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "sqlite", databaseDriver)
	require.NoError(err)
	err = m.Migrate(version)
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(err)
	}
	seed(raw)
	require.NoError(raw.Close())
}

func openMigratorForTest(t *testing.T, dbPath string) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	require := require.New(t)
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	require.NoError(err)
	sourceDriver, err := iofs.New(migrationFiles, "migrations")
	require.NoError(err)
	databaseDriver, err := migratesqlite.WithInstance(raw, &migratesqlite.Config{
		MigrationsTable: migrationTableName,
	})
	require.NoError(err)
	migrator, err := migrate.NewWithInstance(
		"iofs", sourceDriver, "sqlite", databaseDriver,
	)
	require.NoError(err)
	return raw, migrator
}

func assertDatabaseIntegrityForTest(t *testing.T, raw *sql.DB) {
	t.Helper()
	require := require.New(t)
	var integrity string
	require.NoError(raw.QueryRow(`PRAGMA integrity_check`).Scan(&integrity))
	require.Equal("ok", integrity)
	var foreignKeyViolations int
	require.NoError(raw.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations))
	require.Zero(foreignKeyViolations)
}

func openSchemaVersion4DBForTest(t *testing.T) (string, *sql.DB) {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	raw, err := sql.Open("sqlite", path)
	require.NoError(err)
	_, err = raw.Exec(legacySchemaSQLForTest(t, 4))
	require.NoError(err)
	_, err = raw.Exec(`CREATE TABLE schema_migrations (version uint64, dirty bool)`)
	require.NoError(err)
	_, err = raw.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (4, FALSE)`)
	require.NoError(err)
	_, err = raw.Exec(
		`INSERT INTO middleman_repos (
			id, platform, platform_host, owner, name,
			created_at, backfill_pr_page, backfill_pr_complete,
			backfill_issue_page, backfill_issue_complete
		) VALUES (?, 'github', 'github.com', 'octo', 'repo', datetime('now'), 0, 0, 0, 0)`,
		1,
	)
	require.NoError(err)

	return path, raw
}

func seedLegacyIssueForTest(
	t *testing.T,
	raw *sql.DB,
	id int,
	repoID int,
	platformID int,
	number int,
	labelsJSON string,
) {
	t.Helper()
	_, err := raw.Exec(
		`INSERT INTO middleman_issues (
			id, repo_id, platform_id, number, url, title, author, state,
			body, comment_count, labels_json, created_at, updated_at, last_activity_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'), datetime('now'))`,
		id,
		repoID,
		platformID,
		number,
		"https://github.com/octo/repo/issues/test",
		"Backfill labels",
		"octocat",
		"open",
		"",
		0,
		labelsJSON,
	)
	require.NoError(t, err)
}
