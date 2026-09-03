package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func plannerStatisticsRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	var rows int
	require.NoError(t, d.ReadDB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sqlite_stat1 WHERE tbl = ?", table).Scan(&rows))
	return rows
}

func TestOpenCreatesPlannerStatistics(t *testing.T) {
	d := openDBWithMigrations(t)
	var tables int
	require.NoError(t, d.ReadDB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1'").Scan(&tables))
	assert.Equal(t, 1, tables, "opening a store must leave planner statistics behind")
}

func TestOptimizeRecordsStatisticsForPopulatedTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	require.Zero(plannerStatisticsRows(t, d, "forge_merge_requests"),
		"the copied template fixture starts without merge request statistics")

	repoID, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "acme", "widget"))
	require.NoError(err)
	for number := 1; number <= 20; number++ {
		insertTestMR(t, d, repoID, number, "change", baseTime())
	}
	require.NoError(d.Optimize(ctx))

	assert.Positive(plannerStatisticsRows(t, d, "forge_merge_requests"))
	assert.Positive(plannerStatisticsRows(t, d, "forge_repos"))
}

func TestOptimizeReloadsStatisticsOnEveryReadConnection(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	var readConnections []*sql.Conn
	t.Cleanup(func() {
		for _, connection := range readConnections {
			require.NoError(connection.Close())
		}
	})
	_, err := d.WriteDB().ExecContext(ctx, `
		CREATE TABLE planner_probe(a INTEGER, b INTEGER);
		CREATE INDEX planner_probe_a ON planner_probe(a);
		CREATE INDEX planner_probe_b ON planner_probe(b);
		WITH RECURSIVE rows(n) AS (
			VALUES(1)
			UNION ALL
			SELECT n + 1 FROM rows WHERE n < 1000
		)
		INSERT INTO planner_probe SELECT 1, n FROM rows;
		ANALYZE planner_probe;
		DELETE FROM sqlite_stat4 WHERE tbl = 'planner_probe';
		UPDATE sqlite_stat1 SET stat = '10 1' WHERE idx = 'planner_probe_a';
		UPDATE sqlite_stat1 SET stat = '10 10' WHERE idx = 'planner_probe_b';
	`)
	require.NoError(err)

	readConnections = make([]*sql.Conn, 0, readPoolSize)
	for range readPoolSize {
		connection, err := d.ReadDB().Conn(ctx)
		require.NoError(err)
		readConnections = append(readConnections, connection)
		_, err = connection.ExecContext(ctx, "ANALYZE sqlite_schema")
		require.NoError(err)
	}
	for len(readConnections) > 0 {
		connection := readConnections[0]
		var id, parent, unused int
		var detail string
		require.NoError(connection.QueryRowContext(ctx,
			"EXPLAIN QUERY PLAN SELECT * FROM planner_probe WHERE a = ? AND b = ?", 1, 1,
		).Scan(&id, &parent, &unused, &detail))
		assert.Contains(t, detail, "planner_probe_a")
		require.NoError(connection.Close())
		readConnections = readConnections[1:]
	}

	require.NoError(d.Optimize(ctx))

	readConnections = readConnections[:0]
	for range readPoolSize {
		connection, err := d.ReadDB().Conn(ctx)
		require.NoError(err)
		readConnections = append(readConnections, connection)
	}
	for len(readConnections) > 0 {
		connection := readConnections[0]
		var id, parent, unused int
		var detail string
		require.NoError(connection.QueryRowContext(ctx,
			"EXPLAIN QUERY PLAN SELECT * FROM planner_probe WHERE a = ? AND b = ?", 1, 1,
		).Scan(&id, &parent, &unused, &detail))
		assert.Contains(t, detail, "planner_probe_b")
		require.NoError(connection.Close())
		readConnections = readConnections[1:]
	}
}
