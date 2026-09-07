package github

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
)

type livenessTestHistory struct {
	sourceDir string
	manager   *gitclone.Manager
	base      string
	a1        string
	a2        string
	a3        string
	b1        string
	b2        string
}

type commitLivenessFixture struct {
	syncer   *Syncer
	database *db.DB
	repo     RepoRef
	repoID   int64
	mrID     int64
	history  livenessTestHistory
}

func livenessTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v: %s%s", args, out, stderr)
	return strings.TrimSpace(string(out))
}

func setupLivenessTestHistory(t *testing.T) livenessTestHistory {
	t.Helper()
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	run := func(args ...string) string {
		return livenessTestGit(t, sourceDir, args...)
	}
	run("init", "-b", "main")
	run("config", "user.email", "fixture@example.invalid")
	run("config", "user.name", "Fixture")

	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "base.txt"), []byte("m1\n"), 0o644))
	run("add", "base.txt")
	run("commit", "-m", "base m1")
	base := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	lineageCommit := func(path, contents, message string) string {
		require.NoError(t, os.WriteFile(filepath.Join(sourceDir, path), []byte(contents), 0o644))
		run("add", path)
		run("commit", "-m", message)
		return run("rev-parse", "HEAD")
	}
	a1 := lineageCommit("lineage-a.txt", "a1\n", "lineage a1")
	a2 := lineageCommit("lineage-a.txt", "a2\n", "lineage a2")
	a3 := lineageCommit("lineage-a.txt", "a3\n", "lineage a3")

	run("checkout", "-b", "feature-b", base)
	b1 := lineageCommit("lineage-b.txt", "b1\n", "lineage b1")
	b2 := lineageCommit("lineage-b.txt", "b2\n", "lineage b2")

	// Advance main after both feature lineages fork. Liveness must depend only
	// on reachability from the MR head, never on the moving base branch.
	run("checkout", "main")
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "base.txt"), []byte("m2\n"), 0o644))
	run("add", "base.txt")
	run("commit", "-m", "advance base")

	manager := gitclone.New(filepath.Join(dir, "clones"), nil)
	barePath, err := manager.ClonePath("github", "github.com", "owner", "repo")
	require.NoError(t, err)
	livenessTestGit(t, "", "clone", "--bare", sourceDir, barePath)

	return livenessTestHistory{
		sourceDir: sourceDir,
		manager:   manager,
		base:      base,
		a1:        a1,
		a2:        a2,
		a3:        a3,
		b1:        b1,
		b2:        b2,
	}
}

func setupCommitLivenessFixture(t *testing.T) commitLivenessFixture {
	t.Helper()
	history := setupLivenessTestHistory(t)
	database := openTestDB(t)
	repo := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		PlatformExternalID: "repo-owner-repo",
		Owner:              "owner",
		Name:               "repo",
		RepoPath:           "owner/repo",
		CloneURL:           history.sourceDir,
	}
	repoID, err := database.UpsertRepo(t.Context(), verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(t, err)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mrID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:             repoID,
		PlatformID:         1,
		PlatformExternalID: "mr-1",
		Number:             1,
		URL:                "https://github.com/owner/repo/pull/1",
		Title:              "Synthetic merge request",
		Author:             "developer",
		State:              db.MergeRequestStateOpen,
		HeadBranch:         "feature",
		BaseBranch:         "main",
		PlatformHeadSHA:    history.a3,
		PlatformBaseSHA:    history.base,
		CreatedAt:          now.Add(-time.Hour),
		UpdatedAt:          now,
		LastActivityAt:     now,
	})
	require.NoError(t, err)
	return commitLivenessFixture{
		syncer:   &Syncer{db: database, clones: history.manager},
		database: database,
		repo:     repo,
		repoID:   repoID,
		mrID:     mrID,
		history:  history,
	}
}

func seedLivenessCommitEvents(t *testing.T, fixture commitLivenessFixture, shas ...string) {
	t.Helper()
	events := make([]db.MREvent, 0, len(shas))
	createdAt := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	for i, sha := range shas {
		events = append(events, db.MREvent{
			MergeRequestID: fixture.mrID,
			EventType:      "commit",
			Summary:        sha,
			Body:           fmt.Sprintf("synthetic commit %d", i+1),
			MetadataJSON:   fmt.Sprintf(`{"commit_order_key":%d}`, i+1),
			CreatedAt:      createdAt.Add(time.Duration(i) * time.Minute),
			DedupeKey:      sha,
		})
	}
	require.NoError(t, fixture.database.UpsertMREvents(t.Context(), events))
}

func setLivenessFixtureHead(
	t *testing.T,
	fixture commitLivenessFixture,
	headSHA string,
) *db.MergeRequest {
	t.Helper()
	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, mr)
	mr.PlatformHeadSHA = headSHA
	mr.UpdatedAt = mr.UpdatedAt.Add(time.Minute)
	mr.LastActivityAt = mr.UpdatedAt
	_, err = fixture.database.UpsertMergeRequest(t.Context(), mr)
	require.NoError(t, err)
	fresh, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	return fresh
}

// runLivenessRound performs one dataset-commit sync round against the MR's
// current snapshot revision: the incoming events (if any) are persisted and
// commit-liveness flags ride the same revision-guarded transaction, exactly as
// production rounds do.
func runLivenessRound(
	t *testing.T,
	fixture commitLivenessFixture,
	headSHA string,
	incoming []db.MREvent,
) bool {
	t.Helper()
	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, mr)
	applied, err := fixture.syncer.commitMergeRequestDatasets(
		t.Context(), fixture.repo, fixture.mrID, 1, mr.SnapshotRevision,
		nil, false, nil, nil, nil, false, incoming, nil, headSHA,
	)
	require.NoError(t, err)
	return applied
}

func assertLivenessCommitFlags(
	t *testing.T,
	fixture commitLivenessFixture,
	want map[string]bool,
) {
	t.Helper()
	assert := assert.New(t)
	events, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(t, err)
	byKey := make(map[string]db.MREvent, len(events))
	for _, event := range events {
		byKey[event.DedupeKey] = event
	}
	for key, obsolete := range want {
		event, ok := byKey[key]
		require.True(t, ok, "missing event %s", key)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal([]byte(event.MetadataJSON), &metadata))
		assert.Contains(metadata, "commit_order_key", "event %s lost existing metadata", key)
		value, present := metadata["obsolete"]
		if obsolete {
			assert.True(present, "event %s has no obsolete flag", key)
			assert.Equal(true, value, "event %s has the wrong obsolete value", key)
		} else {
			assert.False(present, "event %s unexpectedly has an obsolete flag", key)
		}
	}
}

func TestCommitLivenessReplaceAndRestore(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3, h.b1, h.b2)

	setLivenessFixtureHead(t, fixture, h.b2)
	require.True(t, runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true, h.b1: false, h.b2: false,
	})

	setLivenessFixtureHead(t, fixture, h.a3)
	require.True(t, runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false, h.b1: true, h.b2: true,
	})

	setLivenessFixtureHead(t, fixture, h.a2)
	require.True(t, runLivenessRound(t, fixture, h.a2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: true, h.b1: true, h.b2: true,
	})
}

func TestCommitLivenessIgnoresBaseAdvance(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)

	require.True(t, runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{h.a1: false, h.a2: false, h.a3: false})
}

func TestCommitLivenessSkipsWhenHeadMissing(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	require.NoError(t, fixture.database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: fixture.mrID,
		EventType:      "commit",
		Summary:        h.a1,
		MetadataJSON:   `{"commit_order_key":1,"obsolete":true}`,
		CreatedAt:      time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		DedupeKey:      h.a1,
	}}))

	// The head is absent from the clone, so the round commits without liveness
	// changes and previously verified flags survive untouched.
	missingHead := strings.Repeat("d", 40)
	setLivenessFixtureHead(t, fixture, missingHead)
	require.True(t, runLivenessRound(t, fixture, missingHead, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{h.a1: true})
}

func TestCommitLivenessFlagsShaAbsentFromClone(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	absentSHA := strings.Repeat("f", 40)
	seedLivenessCommitEvents(t, fixture, absentSHA)

	require.True(t, runLivenessRound(t, fixture, fixture.history.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{absentSHA: true})
}

func TestCommitLivenessSkipsNonShaSummaries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	events := []db.MREvent{
		{
			MergeRequestID: fixture.mrID,
			EventType:      "commit",
			Summary:        "not a commit SHA",
			MetadataJSON:   `{"commit_order_key":1}`,
			CreatedAt:      time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
			DedupeKey:      "non-sha",
		},
		{
			MergeRequestID: fixture.mrID,
			EventType:      "review",
			Summary:        h.a1,
			MetadataJSON:   `{"review":"approved"}`,
			CreatedAt:      time.Date(2026, 8, 5, 10, 1, 0, 0, time.UTC),
			DedupeKey:      "non-commit",
		},
	}
	require.NoError(fixture.database.UpsertMREvents(t.Context(), events))

	setLivenessFixtureHead(t, fixture, h.b2)
	require.True(runLivenessRound(t, fixture, h.b2, nil))
	stored, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(err)
	require.Len(stored, 2)
	byKey := map[string]db.MREvent{stored[0].DedupeKey: stored[0], stored[1].DedupeKey: stored[1]}
	assert.JSONEq(`{"commit_order_key":1}`, byKey["non-sha"].MetadataJSON)
	assert.JSONEq(`{"review":"approved"}`, byKey["non-commit"].MetadataJSON)
}

func TestCommitLivenessUsesPlatformExternalID(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	require.NoError(t, fixture.database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID:     fixture.mrID,
		PlatformExternalID: strings.ToUpper(h.a1),
		EventType:          "commit",
		Summary:            "synthetic commit message",
		MetadataJSON:       `{"commit_order_key":1}`,
		CreatedAt:          time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		DedupeKey:          "gitealike-commit",
	}}))

	setLivenessFixtureHead(t, fixture, h.b2)
	require.True(t, runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{"gitealike-commit": true})
}

func TestCommitLivenessSkipsUnparseableMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	require.NoError(fixture.database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: fixture.mrID,
		EventType:      "commit",
		Summary:        h.a1,
		MetadataJSON:   `[1,2]`,
		CreatedAt:      time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		DedupeKey:      h.a1,
	}}))

	setLivenessFixtureHead(t, fixture, h.b2)
	require.True(runLivenessRound(t, fixture, h.b2, nil))
	stored, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal(`[1,2]`, stored[0].MetadataJSON)
}

func TestCommitLivenessMemoServesSameHeadWithoutClone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)
	setLivenessFixtureHead(t, fixture, h.b2)

	require.True(runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true,
	})
	before, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(err)
	clonePath, err := h.manager.ClonePath("github", "github.com", "owner", "repo")
	require.NoError(err)
	require.NoError(os.RemoveAll(clonePath))

	// The verified walk was memoized for this exact (head, candidate set), so
	// the steady-state round replays the verdicts without touching the
	// now-missing clone; the flags are already correct, so no row changes.
	require.True(runLivenessRound(t, fixture, h.b2, nil))
	after, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(err)
	assert.Equal(before, after)

	// A different head misses the memo and attempts a fresh verification. The
	// clone is gone, so the head is unverifiable and the round commits without
	// liveness changes: the b2-round flags survive.
	setLivenessFixtureHead(t, fixture, h.a3)
	require.True(runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true,
	})
}

func TestCommitLivenessRelistedEventsKeepFlagsOnSameHead(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.b1, h.b2)
	setLivenessFixtureHead(t, fixture, h.b2)
	require.True(t, runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.b1: false, h.b2: false,
	})

	// Gitealike providers re-list historical commit events with fresh,
	// unflagged metadata on every detail refresh. Successive same-head rounds
	// must re-inject the verified flags into those incoming events — never
	// let the upsert wipe a stored obsolete flag (the collapse used to
	// flip-flop on alternating refreshes here).
	relisted := func() []db.MREvent {
		createdAt := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
		return []db.MREvent{
			{
				EventType:    "commit",
				Summary:      h.a1,
				MetadataJSON: `{"commit_order_key":1}`,
				CreatedAt:    createdAt,
				DedupeKey:    h.a1,
			},
			{
				EventType:    "commit",
				Summary:      h.b1,
				MetadataJSON: `{"commit_order_key":2}`,
				CreatedAt:    createdAt.Add(time.Minute),
				DedupeKey:    h.b1,
			},
		}
	}
	for range 3 {
		require.True(t, runLivenessRound(t, fixture, h.b2, relisted()))
		assertLivenessCommitFlags(t, fixture, map[string]bool{
			h.a1: true, h.b1: false, h.b2: false,
		})
	}
}

func TestCommitLivenessStaleRevisionRoundIsInert(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3, h.b1, h.b2)

	stale, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(err)
	require.NotNil(stale)

	// The MR advances to b2 after the a3 round captured its revision. The
	// revision CAS rejects the whole stale snapshot, liveness flags included.
	setLivenessFixtureHead(t, fixture, h.b2)
	applied, err := fixture.syncer.commitMergeRequestDatasets(
		t.Context(), fixture.repo, fixture.mrID, 1, stale.SnapshotRevision,
		nil, false, nil, nil, nil, false, nil, nil, h.a3,
	)
	require.NoError(err)
	assert.False(applied)
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false, h.b1: false, h.b2: false,
	})

	// A current-revision round for the real head must still compute freshly:
	// the rejected round may not have recorded any liveness hint.
	require.True(runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true, h.b1: false, h.b2: false,
	})
}

func TestCommitLivenessRestampsRestoredHeadAfterUnverifiedRound(t *testing.T) {
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)
	require.True(t, runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
	})

	// Create a divergent lineage after the bare clone was made so its head is
	// genuinely unavailable to this round.
	livenessTestGit(t, h.sourceDir, "checkout", "-b", "unverified-lineage", h.base)
	commit := func(contents, message string) string {
		require.NoError(t, os.WriteFile(
			filepath.Join(h.sourceDir, "unverified.txt"), []byte(contents), 0o644,
		))
		livenessTestGit(t, h.sourceDir, "add", "unverified.txt")
		livenessTestGit(t, h.sourceDir, "commit", "-m", message)
		return livenessTestGit(t, h.sourceDir, "rev-parse", "HEAD")
	}
	unverifiedCommit1 := commit("replacement 1\n", "unverified replacement 1")
	unverifiedHead := commit("replacement 2\n", "unverified replacement 2")

	// The unverified round persists its incoming commit events without flags
	// (the provider listed them, so showing them is the safe direction). The
	// stored event set has now changed, so the a3 memo entry no longer
	// matches any future round's candidates.
	createdAt := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	incoming := []db.MREvent{
		{
			EventType:    "commit",
			Summary:      unverifiedCommit1,
			MetadataJSON: `{"commit_order_key":10}`,
			CreatedAt:    createdAt,
			DedupeKey:    unverifiedCommit1,
		},
		{
			EventType:    "commit",
			Summary:      unverifiedHead,
			MetadataJSON: `{"commit_order_key":11}`,
			CreatedAt:    createdAt.Add(time.Minute),
			DedupeKey:    unverifiedHead,
		},
	}
	setLivenessFixtureHead(t, fixture, unverifiedHead)
	require.True(t, runLivenessRound(t, fixture, unverifiedHead, incoming))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
		unverifiedCommit1: false, unverifiedHead: false,
	})

	// Restoring a3 must recompute despite a3 having been the last verified
	// head: the memo key covers the candidate set, so the grown set misses
	// and a fresh walk flags the unverified lineage as obsolete.
	setLivenessFixtureHead(t, fixture, h.a3)
	require.True(t, runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
		unverifiedCommit1: true, unverifiedHead: true,
	})
}

func TestCommitLivenessFailedRoundWritesNothing(t *testing.T) {
	require := require.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)
	require.True(runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
	})

	setLivenessFixtureHead(t, fixture, h.b2)
	_, err := fixture.database.WriteDB().ExecContext(t.Context(), `
		CREATE TRIGGER reject_obsolete_metadata
		BEFORE UPDATE OF metadata_json ON forge_mr_events
		BEGIN
			SELECT RAISE(ABORT, 'reject obsolete metadata');
		END`)
	require.NoError(err)

	// The b2 head verifies and the round reaches the snapshot transaction, but
	// the metadata write fails inside it: the transaction rolls back whole and
	// the database keeps the a3-era flags.
	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(err)
	require.NotNil(mr)
	_, err = fixture.syncer.commitMergeRequestDatasets(
		t.Context(), fixture.repo, fixture.mrID, 1, mr.SnapshotRevision,
		nil, false, nil, nil, nil, false, nil, nil, h.b2,
	)
	require.ErrorContains(err, "reject obsolete metadata")
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
	})
	_, err = fixture.database.WriteDB().ExecContext(
		t.Context(), `DROP TRIGGER reject_obsolete_metadata`,
	)
	require.NoError(err)

	// The failed round may have memoized its verified walk — the verdicts are
	// a pure function of git history, so that is sound. The retry must land
	// the b2 flags the failed transaction rolled back, whether it recomputes
	// or replays the memo against the still-stale stored rows.
	require.True(runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true,
	})
}

func TestCommitLivenessConcurrentMergeRequests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	first, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(err)
	require.NotNil(first)
	second := *first
	second.ID = 0
	second.PlatformID = 2
	second.PlatformExternalID = "mr-2"
	second.Number = 2
	second.URL = "https://github.com/owner/repo/pull/2"
	second.PlatformHeadSHA = h.b2
	second.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	second.LastActivityAt = second.UpdatedAt
	secondMRID, err := fixture.database.UpsertMergeRequest(t.Context(), &second)
	require.NoError(err)

	secondFixture := fixture
	secondFixture.mrID = secondMRID
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3, h.b1, h.b2)
	seedLivenessCommitEvents(t, secondFixture, h.a1, h.a2, h.a3, h.b1, h.b2)

	firstRow, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(err)
	require.NotNil(firstRow)
	secondRow, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 2,
	)
	require.NoError(err)
	require.NotNil(secondRow)

	start := make(chan struct{})
	errs := make(chan error, 2)
	round := func(mrID int64, number int, revision int64, headSHA string) {
		<-start
		_, err := fixture.syncer.commitMergeRequestDatasets(
			t.Context(), fixture.repo, mrID, number, revision,
			nil, false, nil, nil, nil, false, nil, nil, headSHA,
		)
		errs <- err
	}
	go round(fixture.mrID, 1, firstRow.SnapshotRevision, h.a3)
	go round(secondMRID, 2, secondRow.SnapshotRevision, h.b2)
	close(start)
	assert.NoError(<-errs)
	assert.NoError(<-errs)

	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false, h.b1: true, h.b2: true,
	})
	assertLivenessCommitFlags(t, secondFixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true, h.b1: false, h.b2: false,
	})
}

func TestCommitLivenessOversizedCandidatesComputeWithoutMemo(t *testing.T) {
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	// Pre-flag a live commit as obsolete: only a real computation clears it,
	// so it proves oversized rounds still compute.
	require.NoError(t, fixture.database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: fixture.mrID,
		EventType:      "commit",
		Summary:        h.b1,
		MetadataJSON:   `{"commit_order_key":1,"obsolete":true}`,
		CreatedAt:      time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		DedupeKey:      h.b1,
	}}))
	seedLivenessCommitEvents(t, fixture, h.a1, h.b2)
	setLivenessFixtureHead(t, fixture, h.b2)

	// Three candidates exceed a cap of two: the round computes and repairs
	// exactly like any other round — the cap only forgoes memoization, so
	// oversized MRs never freeze at stale flags.
	fixture.syncer.livenessCandidateLimit = 2
	require.True(t, runLivenessRound(t, fixture, h.b2, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.b1: false, h.a1: true, h.b2: false,
	})
	fixture.syncer.livenessMemoMu.Lock()
	memoLen := len(fixture.syncer.livenessMemos)
	fixture.syncer.livenessMemoMu.Unlock()
	assert.Equal(0, memoLen, "oversized candidate sets must not be memoized")
}

func TestCommitLivenessMemoEviction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	fixture.syncer.livenessMemoLimit = 1

	first, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(err)
	require.NotNil(first)
	second := *first
	second.ID = 0
	second.PlatformID = 2
	second.PlatformExternalID = "mr-2"
	second.Number = 2
	second.URL = "https://github.com/owner/repo/pull/2"
	second.PlatformHeadSHA = h.b2
	second.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	second.LastActivityAt = second.UpdatedAt
	secondMRID, err := fixture.database.UpsertMergeRequest(t.Context(), &second)
	require.NoError(err)
	secondFixture := fixture
	secondFixture.mrID = secondMRID

	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)
	seedLivenessCommitEvents(t, secondFixture, h.a1, h.b2)

	require.True(runLivenessRound(t, fixture, h.a3, nil))
	secondRow, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 2,
	)
	require.NoError(err)
	require.NotNil(secondRow)
	_, err = fixture.syncer.commitMergeRequestDatasets(
		t.Context(), fixture.repo, secondMRID, 2, secondRow.SnapshotRevision,
		nil, false, nil, nil, nil, false, nil, nil, h.b2,
	)
	require.NoError(err)

	fixture.syncer.livenessMemoMu.Lock()
	memoLen := len(fixture.syncer.livenessMemos)
	fixture.syncer.livenessMemoMu.Unlock()
	assert.Equal(1, memoLen, "memo must stay at its entry cap")

	// The evicted MR just recomputes: flags remain correct.
	require.True(runLivenessRound(t, fixture, h.a3, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: false, h.a2: false, h.a3: false,
	})
	assertLivenessCommitFlags(t, secondFixture, map[string]bool{
		h.a1: true, h.b2: false,
	})
}

func TestCommitLivenessMemoEvictsLeastRecentlyUsed(t *testing.T) {
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	fixture.syncer.livenessMemoLimit = 2

	first, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, first)
	cloneMR := func(number int, platformID int64) int64 {
		t.Helper()
		mr := *first
		mr.ID = 0
		mr.PlatformID = platformID
		mr.PlatformExternalID = fmt.Sprintf("mr-%d", number)
		mr.Number = number
		mr.URL = fmt.Sprintf("https://github.com/owner/repo/pull/%d", number)
		mr.UpdatedAt = mr.UpdatedAt.Add(time.Duration(number) * time.Minute)
		mr.LastActivityAt = mr.UpdatedAt
		id, err := fixture.database.UpsertMergeRequest(t.Context(), &mr)
		require.NoError(t, err)
		return id
	}
	secondMRID := cloneMR(2, 2)
	thirdMRID := cloneMR(3, 3)
	seedLivenessCommitEvents(t, fixture, h.a1)
	secondFixture := fixture
	secondFixture.mrID = secondMRID
	seedLivenessCommitEvents(t, secondFixture, h.a2)
	thirdFixture := fixture
	thirdFixture.mrID = thirdMRID
	seedLivenessCommitEvents(t, thirdFixture, h.a3)

	round := func(mrID int64, number int) {
		t.Helper()
		row, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
			t.Context(), fixture.repoID, number,
		)
		require.NoError(t, err)
		require.NotNil(t, row)
		_, err = fixture.syncer.commitMergeRequestDatasets(
			t.Context(), fixture.repo, mrID, number, row.SnapshotRevision,
			nil, false, nil, nil, nil, false, nil, nil, h.a3,
		)
		require.NoError(t, err)
	}
	memoKeys := func() map[int64]bool {
		fixture.syncer.livenessMemoMu.Lock()
		defer fixture.syncer.livenessMemoMu.Unlock()
		keys := make(map[int64]bool, len(fixture.syncer.livenessMemos))
		for mrID := range fixture.syncer.livenessMemos {
			keys[mrID] = true
		}
		return keys
	}

	// Fill the memo (MR1, MR2), then touch MR1 so MR2 becomes the
	// least-recently-used entry; adding MR3 must evict MR2, not MR1.
	round(fixture.mrID, 1)
	round(secondMRID, 2)
	round(fixture.mrID, 1)
	round(thirdMRID, 3)
	assert.Equal(map[int64]bool{fixture.mrID: true, thirdMRID: true}, memoKeys())
}

func TestCommitLivenessConcurrentSameMergeRequestRounds(t *testing.T) {
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3, h.b1, h.b2)
	setLivenessFixtureHead(t, fixture, h.b2)

	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), fixture.repoID, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, mr)

	// Two rounds of the same MR at the same head and revision race each
	// other. Reachability is a pure function of (head, candidate set), so
	// both compute identical verdicts and both writes are idempotent — no
	// interleaving can publish liveness state that was never evaluated.
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := fixture.syncer.commitMergeRequestDatasets(
				t.Context(), fixture.repo, fixture.mrID, 1, mr.SnapshotRevision,
				nil, false, nil, nil, nil, false, nil, nil, h.b2,
			)
			errs <- err
		}()
	}
	close(start)
	assert.NoError(<-errs)
	assert.NoError(<-errs)
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true, h.b1: false, h.b2: false,
	})
}

func TestLivenessHeadForRound(t *testing.T) {
	assert := assert.New(t)
	open := &db.MergeRequest{State: db.MergeRequestStateOpen, PlatformHeadSHA: "head-open"}
	merged := &db.MergeRequest{State: db.MergeRequestStateMerged, PlatformHeadSHA: "head-final"}
	closed := &db.MergeRequest{State: db.MergeRequestStateClosed, PlatformHeadSHA: "head-final"}

	assert.Equal("head-open", livenessHeadForRound(open, nil), "open MRs always compute")
	assert.Equal("head-open", livenessHeadForRound(open, closed), "reopened MRs compute again")
	assert.Equal("head-final", livenessHeadForRound(merged, open),
		"the round that merges an MR computes once against the final head")
	assert.Equal("head-final", livenessHeadForRound(closed, open),
		"the round that closes an MR computes once against the final head")
	assert.Empty(livenessHeadForRound(merged, merged),
		"already-merged MRs are never recomputed")
	assert.Empty(livenessHeadForRound(closed, closed),
		"already-closed MRs are never recomputed")
	assert.Empty(livenessHeadForRound(merged, nil),
		"an MR first seen terminal has no transition round")
	assert.Empty(livenessHeadForRound(nil, open))
}

func TestCommitLivenessRepairsThroughUnchangedDetail(t *testing.T) {
	require := require.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1)

	livenessTestGit(t, h.sourceDir, "checkout", "-b", "repair-head", "feature-b")
	require.NoError(os.WriteFile(
		filepath.Join(h.sourceDir, "repair.txt"), []byte("repair\n"), 0o644,
	))
	livenessTestGit(t, h.sourceDir, "add", "repair.txt")
	livenessTestGit(t, h.sourceDir, "commit", "-m", "repair head")
	repairHead := livenessTestGit(t, h.sourceDir, "rev-parse", "HEAD")
	existing := setLivenessFixtureHead(t, fixture, repairHead)
	routeFence, found, err := fixture.database.CurrentRepositoryRouteFence(
		t.Context(), platformdb.DBRepoIdentity(platformRepoRef(fixture.repo)), fixture.repoID,
	)
	require.NoError(err)
	require.True(found)

	// The clone does not yet contain the head, so the unchanged-detail round
	// marks detail fetched without touching liveness metadata.
	_, err = fixture.syncer.markUnchangedMRDetailFetched(
		t.Context(), fixture.repo, fixture.repoID, 1, existing, routeFence, 1,
	)
	require.NoError(err)
	assertLivenessCommitFlags(t, fixture, map[string]bool{h.a1: false})

	clonePath, err := h.manager.ClonePath("github", "github.com", "owner", "repo")
	require.NoError(err)
	livenessTestGit(
		t, clonePath, "fetch", h.sourceDir,
		"refs/heads/repair-head:refs/heads/repair-head",
	)
	// Once the clone has the head, the next unchanged-detail round carries the
	// liveness updates with its marker under the same revision guard.
	_, err = fixture.syncer.markUnchangedMRDetailFetched(
		t.Context(), fixture.repo, fixture.repoID, 1, existing, routeFence, 1,
	)
	require.NoError(err)
	assertLivenessCommitFlags(t, fixture, map[string]bool{h.a1: true})
}

func TestCommitLivenessViaFetchProviderMRDetail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	providerRepo := RepoRef{
		Platform:           platform.KindForgejo,
		PlatformHost:       platform.DefaultForgejoHost,
		PlatformExternalID: "repo-1",
		Owner:              "owner",
		Name:               "repo",
		RepoPath:           "owner/repo",
		CloneURL:           h.sourceDir,
	}
	barePath, err := h.manager.ClonePath(
		string(platform.KindForgejo), platform.DefaultForgejoHost, "owner", "repo",
	)
	require.NoError(err)
	livenessTestGit(t, "", "clone", "--bare", h.sourceDir, barePath)
	providerRepoID, err := fixture.database.UpsertRepo(
		t.Context(), verifiedDBRepoIdentity(platformRepoRef(providerRepo)),
	)
	require.NoError(err)
	routeFence, found, err := fixture.database.CurrentRepositoryRouteFence(
		t.Context(), platformdb.DBRepoIdentity(platformRepoRef(providerRepo)), providerRepoID,
	)
	require.NoError(err)
	require.True(found)
	now := time.Date(2026, 8, 5, 12, 1, 0, 0, time.UTC)
	providerMRID, err := fixture.database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:             providerRepoID,
		PlatformID:         101,
		PlatformExternalID: "mr-1",
		Number:             1,
		URL:                "https://codeberg.org/owner/repo/pulls/1",
		Title:              "Synthetic merge request",
		Author:             "developer",
		State:              db.MergeRequestStateOpen,
		HeadBranch:         "feature",
		BaseBranch:         "main",
		PlatformHeadSHA:    h.a3,
		PlatformBaseSHA:    h.base,
		CreatedAt:          now.Add(-time.Hour),
		UpdatedAt:          now.Add(-time.Minute),
		LastActivityAt:     now.Add(-time.Minute),
	})
	require.NoError(err)
	providerFixture := fixture
	providerFixture.repo = providerRepo
	providerFixture.repoID = providerRepoID
	providerFixture.mrID = providerMRID
	seedLivenessCommitEvents(t, providerFixture, h.a1, h.a2, h.a3)

	providerRef := platformRepoRef(providerRepo)
	provider := &syncTestReadProvider{
		kind: platform.KindForgejo,
		host: platform.DefaultForgejoHost,
		mergeRequests: []platform.MergeRequest{{
			Repo:               providerRef,
			PlatformID:         101,
			PlatformExternalID: "mr-1",
			Number:             1,
			URL:                "https://codeberg.org/owner/repo/pulls/1",
			Title:              "Synthetic merge request",
			Author:             "developer",
			State:              "open",
			HeadBranch:         "feature",
			BaseBranch:         "main",
			HeadSHA:            h.b2,
			BaseSHA:            h.base,
			CreatedAt:          now.Add(-time.Hour),
			UpdatedAt:          now,
			LastActivityAt:     now,
		}},
		listMRMergeEvents: []platform.MergeRequestEvent{
			{
				Repo:               providerRef,
				PlatformExternalID: h.b1,
				MergeRequestNumber: 1,
				EventType:          "commit",
				Summary:            "lineage b1",
				CreatedAt:          now.Add(-2 * time.Minute),
				DedupeKey:          h.b1,
			},
			{
				Repo:               providerRef,
				PlatformExternalID: h.b2,
				MergeRequestNumber: 1,
				EventType:          "commit",
				Summary:            "lineage b2",
				CreatedAt:          now.Add(-time.Minute),
				DedupeKey:          h.b2,
			},
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, fixture.database, h.manager, []RepoRef{providerRepo},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	_, err = syncer.fetchProviderMRDetail(
		t.Context(), provider, providerRepo, providerRepoID, 1, routeFence,
	)
	require.NoError(err)
	assertLivenessCommitFlags(t, providerFixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true, h.b1: false, h.b2: false,
	})
	events, err := fixture.database.ListMREvents(t.Context(), providerMRID)
	require.NoError(err)
	seenReplacement := map[string]bool{h.b1: false, h.b2: false}
	for _, event := range events {
		if _, ok := seenReplacement[event.PlatformExternalID]; ok {
			seenReplacement[event.PlatformExternalID] = true
		}
	}
	assert.True(seenReplacement[h.b1], "replacement commit b1 must be persisted by the round")
	assert.True(seenReplacement[h.b2], "replacement commit b2 must be persisted by the round")
}

func TestCommitLivenessFinalizedByPeriodicCloseDetection(t *testing.T) {
	require := require.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	providerRepo := RepoRef{
		Platform:           platform.KindForgejo,
		PlatformHost:       platform.DefaultForgejoHost,
		PlatformExternalID: "repo-1",
		Owner:              "owner",
		Name:               "repo",
		RepoPath:           "owner/repo",
		CloneURL:           h.sourceDir,
	}
	barePath, err := h.manager.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), providerRepo.PlatformExternalID),
		string(platform.KindForgejo), platform.DefaultForgejoHost, "owner", "repo",
	)
	require.NoError(err)
	livenessTestGit(t, "", "clone", "--bare", h.sourceDir, barePath)
	providerRepoID, err := fixture.database.UpsertRepo(
		t.Context(), verifiedDBRepoIdentity(platformRepoRef(providerRepo)),
	)
	require.NoError(err)
	now := time.Date(2026, 8, 5, 12, 1, 0, 0, time.UTC)
	providerMRID, err := fixture.database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:             providerRepoID,
		PlatformID:         101,
		PlatformExternalID: "mr-1",
		Number:             1,
		URL:                "https://codeberg.org/owner/repo/pulls/1",
		Title:              "Synthetic merge request",
		Author:             "developer",
		State:              db.MergeRequestStateOpen,
		HeadBranch:         "feature",
		BaseBranch:         "main",
		PlatformHeadSHA:    h.a3,
		PlatformBaseSHA:    h.base,
		CreatedAt:          now.Add(-time.Hour),
		UpdatedAt:          now.Add(-time.Minute),
		LastActivityAt:     now.Add(-time.Minute),
	})
	require.NoError(err)
	providerFixture := fixture
	providerFixture.repo = providerRepo
	providerFixture.repoID = providerRepoID
	providerFixture.mrID = providerMRID
	seedLivenessCommitEvents(t, providerFixture, h.a1, h.a2, h.a3)

	// The provider reports the MR force-pushed to b2 AND already closed —
	// the shape periodic sync sees when an open MR disappears from the
	// listing. The close-detection round is the terminal transition, so it
	// must compute liveness against the final head instead of freezing the
	// a3-era flags.
	providerRef := platformRepoRef(providerRepo)
	provider := &syncTestReadProvider{
		kind: platform.KindForgejo,
		host: platform.DefaultForgejoHost,
		mergeRequests: []platform.MergeRequest{{
			Repo:               providerRef,
			PlatformID:         101,
			PlatformExternalID: "mr-1",
			Number:             1,
			URL:                "https://codeberg.org/owner/repo/pulls/1",
			Title:              "Synthetic merge request",
			Author:             "developer",
			State:              "closed",
			HeadBranch:         "feature",
			BaseBranch:         "main",
			HeadSHA:            h.b2,
			BaseSHA:            h.base,
			CreatedAt:          now.Add(-time.Hour),
			UpdatedAt:          now,
			LastActivityAt:     now,
		}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, fixture.database, h.manager, []RepoRef{providerRepo},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	require.NoError(syncer.fetchAndUpdateClosedMergeRequest(
		t.Context(), provider, providerRepo, providerRepoID, 1, false,
	))
	assertLivenessCommitFlags(t, providerFixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true,
	})
}

func TestSyncMRForRepoComputesCommitLivenessWithTimeline(t *testing.T) {
	assert := assert.New(t)
	fixture := setupCommitLivenessFixture(t)
	h := fixture.history
	seedLivenessCommitEvents(t, fixture, h.a1, h.a2, h.a3)

	now := time.Date(2026, 8, 5, 12, 1, 0, 0, time.UTC)
	pr := buildOpenPRWithSHA(1, now, h.b2)
	pr.Base.SHA = &h.base
	commit := func(sha, message string, createdAt time.Time) *gh.RepositoryCommit {
		return &gh.RepositoryCommit{
			SHA: &sha,
			Commit: &gh.Commit{
				Message: &message,
				Author:  &gh.CommitAuthor{Name: new("developer"), Date: makeTimestamp(createdAt)},
			},
		}
	}
	ciState := "success"
	client := &mockClient{
		singlePR: pr,
		comments: []*gh.IssueComment{},
		reviews:  []*gh.PullRequestReview{},
		commits: []*gh.RepositoryCommit{
			commit(h.b1, "lineage b1", now.Add(-2*time.Minute)),
			commit(h.b2, "lineage b2", now.Add(-time.Minute)),
		},
		ciStatus:  &gh.CombinedStatus{State: &ciState},
		checkRuns: []*gh.CheckRun{},
	}
	syncer := NewSyncer(
		map[string]Client{"github.com": client},
		fixture.database,
		h.manager,
		[]RepoRef{fixture.repo},
		time.Minute,
		nil,
		testBudget(500),
	)
	t.Cleanup(syncer.Stop)

	require.NoError(t, syncer.syncMRForRepo(t.Context(), fixture.repo, 1, false, nil))
	assertLivenessCommitFlags(t, fixture, map[string]bool{
		h.a1: true, h.a2: true, h.a3: true,
	})
	events, err := fixture.database.ListMREvents(t.Context(), fixture.mrID)
	require.NoError(t, err)
	seenB := map[string]bool{h.b1: false, h.b2: false}
	for _, event := range events {
		if _, ok := seenB[event.Summary]; ok {
			var metadata map[string]any
			require.NoError(t, json.Unmarshal([]byte(event.MetadataJSON), &metadata))
			_, obsolete := metadata["obsolete"]
			assert.False(obsolete, "replacement commit %s must remain live", event.Summary)
			seenB[event.Summary] = true
		}
	}
	assert.True(seenB[h.b1], "replacement commit b1 must be persisted by the round")
	assert.True(seenB[h.b2], "replacement commit b2 must be persisted by the round")
}
