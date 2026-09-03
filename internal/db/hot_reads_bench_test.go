package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacyConnectionDSN is the DSN the daemon used before per-connection cache,
// mmap, and temp_store pragmas were added. The hot-read benchmark opens
// pools with it to isolate what each layer of the change buys.
const legacyConnectionDSN = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

type hotReadVariant struct {
	name       string
	dsn        string
	cacheLimit int
}

func hotReadVariants() []hotReadVariant {
	return []hotReadVariant{
		{name: "baseline", dsn: legacyConnectionDSN, cacheLimit: 0}, // raw pools, per-call compile
		{name: "pragmas-only", dsn: connectionDSN, cacheLimit: 0},
		{name: "stmt-cache-only", dsn: legacyConnectionDSN, cacheLimit: stmtCacheLimit},
		{name: "pragmas+stmt-cache", dsn: connectionDSN, cacheLimit: stmtCacheLimit},
	}
}

// openHotReadDB builds a DB over an already-seeded file with the requested
// DSN and cache capacity, mirroring open() without running migrations.
func openHotReadDB(b *testing.B, path string, variant hotReadVariant) *DB {
	b.Helper()
	openPoolWith := func(size int) *sql.DB {
		pool, err := sql.Open("sqlite", path+variant.dsn)
		require.NoError(b, err)
		pool.SetMaxOpenConns(size)
		pool.SetMaxIdleConns(size)
		return pool
	}
	rw := openPoolWith(writePoolSize)
	ro := openPoolWith(readPoolSize)
	d := &DB{
		rw:      rw,
		ro:      ro,
		rwStmts: newStmtCache(rw, variant.cacheLimit),
		roStmts: newStmtCache(ro, variant.cacheLimit),
	}
	b.Cleanup(func() { require.NoError(b, d.Close()) })
	return d
}

// seedHotReadDatabase writes a store shaped like a busy maintainer install:
// many repositories, thousands of merge requests, and tens of thousands of
// timeline events with comment-sized bodies. It returns the file path.
func seedHotReadDatabase(b *testing.B, repos, mrsPerRepo, eventsPerMR int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "hot-reads.db")
	d, err := Open(path)
	require.NoError(b, err)
	ctx := context.Background()
	body := make([]byte, 600)
	for i := range body {
		body[i] = 'a' + byte(i%26)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for r := range repos {
		identity := verifiedTestRepoIdentity("github", "github.com", "acme", fmt.Sprintf("service-%03d", r))
		repoID, err := d.UpsertRepo(ctx, identity)
		require.NoError(b, err)
		events := make([]MREvent, 0, mrsPerRepo*eventsPerMR)
		for n := 1; n <= mrsPerRepo; n++ {
			mr := testMR(repoID, n)
			mr.LastActivityAt = base.Add(time.Duration(r*mrsPerRepo+n) * time.Minute)
			mr.UpdatedAt = mr.LastActivityAt
			if n%3 == 0 {
				mr.State = "merged"
			}
			mrID, err := d.UpsertMergeRequest(ctx, mr)
			require.NoError(b, err)
			for e := range eventsPerMR {
				events = append(events, MREvent{
					MergeRequestID:     mrID,
					PlatformExternalID: fmt.Sprintf("evt-%d-%d", mrID, e),
					EventType:          "issue_comment",
					Author:             fmt.Sprintf("user-%d", e%7),
					Summary:            "commented",
					Body:               string(body),
					CreatedAt:          mr.LastActivityAt.Add(time.Duration(e) * time.Second),
					DedupeKey:          fmt.Sprintf("comment:%d:%d", mrID, e),
				})
			}
		}
		require.NoError(b, d.UpsertMREvents(ctx, events))
	}
	require.NoError(b, d.Close())
	return path
}

// hotReadDatabase returns the file to benchmark. Set KENN_FORGE_BENCH_DB to a
// SQLite backup of a real store to measure against production-shaped data.
// The file is copied into the benchmark's private temp dir first, and only
// the copy is migrated and opened; the supplied file is never written.
// Without it the benchmark seeds a synthetic store.
func hotReadDatabase(b *testing.B) string {
	b.Helper()
	if snapshot := os.Getenv("KENN_FORGE_BENCH_DB"); snapshot != "" {
		path := filepath.Join(b.TempDir(), "snapshot.db")
		content, err := os.ReadFile(snapshot)
		require.NoError(b, err)
		require.NoError(b, os.WriteFile(path, content, 0o600))
		d, err := Open(path)
		require.NoError(b, err)
		require.NoError(b, d.Close())
		return path
	}
	const repos, mrsPerRepo, eventsPerMR = 50, 100, 10
	return seedHotReadDatabase(b, repos, mrsPerRepo, eventsPerMR)
}

// BenchmarkHotReads measures the read paths a running daemon repeats most:
// the repository summary list, the open merge request list, one merge
// request detail with its timeline, and the activity feed. Each variant
// toggles the connection pragmas and the statement cache independently so
// the contribution of each is visible.
func BenchmarkHotReads(b *testing.B) {
	path := hotReadDatabase(b)
	info, err := os.Stat(path)
	require.NoError(b, err)
	ctx := context.Background()

	probe, err := Open(path)
	require.NoError(b, err)
	var repos, mrs, events int
	require.NoError(b, probe.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM forge_repos").Scan(&repos))
	require.NoError(b, probe.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM forge_merge_requests").Scan(&mrs))
	require.NoError(b, probe.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM forge_mr_events").Scan(&events))
	openMRs, err := probe.ListMergeRequests(ctx, ListMergeRequestsOpts{State: "open", Limit: 1})
	require.NoError(b, err)
	require.NotEmpty(b, openMRs, "benchmark store needs at least one open merge request")
	detail := openMRs[0]
	detailRepo, err := probe.GetRepoByID(ctx, detail.RepoID)
	require.NoError(b, err)
	require.NotNil(b, detailRepo)
	activity, err := probe.ListActivity(ctx, ListActivityOpts{Limit: 50})
	require.NoError(b, err)
	require.NotEmpty(b, activity, "benchmark store needs activity rows")
	windowStart := activity[0].CreatedAt.AddDate(0, 0, -7)
	require.NoError(b, probe.Close())
	b.Logf("store: %d repos, %d merge requests, %d events, %.1f MB; newest activity is %q",
		repos, mrs, events, float64(info.Size())/1e6, activity[0].ActivityType)

	reads := []struct {
		name string
		run  func(d *DB) error
	}{
		{name: "ListRepoSummaries", run: func(d *DB) error {
			_, err := d.ListRepoSummaries(ctx)
			return err
		}},
		{name: "ListMergeRequests", run: func(d *DB) error {
			_, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{State: "open", Limit: 50})
			return err
		}},
		{name: "MergeRequestDetail", run: func(d *DB) error {
			mr, err := d.GetMergeRequest(ctx, detailRepo.Platform, detailRepo.PlatformHost, detailRepo.Owner, detailRepo.Name, detail.Number)
			if err != nil {
				return err
			}
			_, err = d.ListMREvents(ctx, mr.ID)
			return err
		}},
		{name: "ListActivity", run: func(d *DB) error {
			_, err := d.ListActivity(ctx, ListActivityOpts{Limit: 50})
			return err
		}},
		// The activity route always applies a seven-day window unless the
		// caller supplies one, so this is the shape production runs.
		{name: "ActivityFeedWindowed", run: func(d *DB) error {
			_, err := d.ListActivity(ctx, ListActivityOpts{Limit: 50, Since: &windowStart})
			return err
		}},
	}
	for _, read := range reads {
		b.Run(read.name, func(b *testing.B) {
			for _, variant := range hotReadVariants() {
				b.Run(variant.name, func(b *testing.B) {
					d := openHotReadDB(b, path, variant)
					require.NoError(b, read.run(d), "warm-up")
					b.ResetTimer()
					for b.Loop() {
						require.NoError(b, read.run(d))
					}
				})
			}
		})
	}
}
