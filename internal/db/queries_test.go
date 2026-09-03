package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartWorkspaceRetryTransitionsOnlyOneConcurrentCaller(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := t.Context()
	errMsg := "ensure clone failed"
	ws := &Workspace{
		ID:              "ws-retry-race",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-retry-race",
		TmuxSession:     "kenn-forge-ws-retry-race",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	const callers = 16
	start := make(chan struct{})
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			ok, err := d.StartWorkspaceRetry(
				ctx, "ws-retry-race",
			)
			errs <- err
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(err)
	}

	var successes int
	for ok := range results {
		if ok {
			successes++
		}
	}
	assert.Equal(1, successes)

	got, err := d.GetWorkspace(ctx, "ws-retry-race")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("creating", got.Status)
	assert.Nil(got.ErrorMessage)
	assert.Equal("kenn-forge/pr-42", got.WorkspaceBranch)
}

func TestStartWorkspaceRetryPreservesBranchUntilCleanupSucceeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := context.Background()
	errMsg := "tmux new-session failed"
	ws := &Workspace{
		ID:              "ws-retry-preserve-branch",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-retry-preserve-branch",
		TmuxSession:     "kenn-forge-ws-retry-preserve-branch",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	started, err := d.StartWorkspaceRetry(ctx, ws.ID)
	require.NoError(err)
	assert.True(started)

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("creating", got.Status)
	assert.Nil(got.ErrorMessage)
	assert.Equal("kenn-forge/pr-42", got.WorkspaceBranch)
}

func insertTestRepo(t *testing.T, d *DB, owner, name string) int64 {
	t.Helper()
	id, err := d.UpsertRepo(t.Context(), verifiedTestRepoIdentity(
		"github", "github.com", owner, name,
	))
	require.NoErrorf(t, err, "UpsertRepo(%s/%s)", owner, name)
	return id
}

// insertTestRepoWithHost inserts a repo with a specific platform_host.
func insertTestRepoWithHost(
	t *testing.T, d *DB, owner, name, host string,
) int64 {
	t.Helper()
	id, err := d.UpsertRepo(t.Context(), verifiedTestRepoIdentity(
		"github", host, owner, name,
	))
	require.NoErrorf(t, err, "UpsertRepo(%s/%s on %s)", owner, name, host)
	return id
}

func verifiedTestRepoIdentity(platform, host, owner, name string) RepoIdentity {
	identity := RepoIdentity{
		Platform: platform, PlatformHost: host, Owner: owner, Name: name,
	}
	identity.PlatformRepoID = strings.ToLower(
		"test-" + platform + "-" + host + "-" + owner + "-" + name,
	)
	return identity
}

func TestPurgeOtherHosts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	// Insert repos for two hosts.
	ghRepoID := insertTestRepoWithHost(
		t, d, "acme", "widget", "github.com",
	)
	gheRepoID := insertTestRepoWithHost(
		t, d, "corp", "internal", "ghes.company.com",
	)
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID: "ws-ghe-identified", Platform: "github",
		PlatformHost: "ghes.company.com", RepoOwner: "corp", RepoName: "internal",
		RepoID: gheRepoID, ItemType: WorkspaceItemTypePullRequest, ItemNumber: 2,
		GitHeadRef: "feature/two", WorkspaceBranch: "kenn-forge/pr-2",
		WorktreePath: "/tmp/ws-ghe-identified", TmuxSession: "forge-ws-ghe-identified",
		Status: "ready",
	}))
	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces (
			id, platform, platform_host, repo_owner, repo_name, repo_id,
			repo_owner_key, repo_name_key, repo_path_key,
			item_type, item_number, item_key, git_head_ref, workspace_branch,
			worktree_path, tmux_session, status
		)
		SELECT
			'ws-ghe-legacy', platform, platform_host, repo_owner, repo_name, NULL,
			repo_owner_key, repo_name_key, repo_path_key,
			item_type, item_number, item_key, git_head_ref, workspace_branch,
			'/tmp/ws-ghe-legacy', 'forge-ws-ghe-legacy', status
		FROM forge_workspaces WHERE id = 'ws-ghe-identified'`)
	require.NoError(err)

	// Insert MRs for both hosts.
	ghMRID := insertTestMR(
		t, d, ghRepoID, 1, "gh PR", base,
	)
	gheMRID := insertTestMR(
		t, d, gheRepoID, 2, "ghe PR", base,
	)

	// Insert events for both MRs.
	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{
			MergeRequestID: ghMRID,
			EventType:      "comment",
			Author:         "alice",
			CreatedAt:      base,
			DedupeKey:      "gh-evt-1",
		},
	}))
	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{
			MergeRequestID: gheMRID,
			EventType:      "comment",
			Author:         "bob",
			CreatedAt:      base,
			DedupeKey:      "ghe-evt-1",
		},
	}))

	// Insert worktree links for both MRs.
	require.NoError(d.SetWorktreeLinks(ctx, []WorktreeLink{
		{
			MergeRequestID: ghMRID,
			WorktreeKey:    "wt-gh",
			LinkedAt:       base,
		},
		{
			MergeRequestID: gheMRID,
			WorktreeKey:    "wt-ghe",
			LinkedAt:       base,
		},
	}))

	// Insert starred items for both repos.
	require.NoError(d.SetStarred(ctx, "pr", ghRepoID, 1))
	require.NoError(d.SetStarred(ctx, "pr", gheRepoID, 2))

	// Insert rate limits for both hosts.
	require.NoError(d.UpsertRateLimit(
		"github.com", "user:1", "rest", 10, base, 4990, -1, nil,
	))
	require.NoError(d.UpsertRateLimit(
		"ghes.company.com", "user:1", "rest", 5, base, 4995, -1, nil,
	))

	// Purge all hosts except github.com.
	require.NoError(d.PurgeOtherHosts(ctx, "github.com"))

	// github.com data should remain.
	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal("github.com", repos[0].PlatformHost)
	assert.Equal("acme", repos[0].Owner)

	// github.com MR should remain.
	ghMR, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 1)
	require.NoError(err)
	require.NotNil(ghMR)

	// github.com events should remain.
	ghEvents, err := d.ListMREvents(ctx, ghMRID)
	require.NoError(err)
	assert.Len(ghEvents, 1)

	// github.com worktree links should remain.
	ghLinks, err := d.GetWorktreeLinksForMR(ctx, ghMRID)
	require.NoError(err)
	assert.Len(ghLinks, 1)

	// github.com starred items should remain.
	starred, err := d.IsStarred(ctx, "pr", ghRepoID, 1)
	require.NoError(err)
	assert.True(starred)

	// The ghes.company.com repository survives only as an inactive identity
	// tombstone for its workspace.
	var gheCount int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_repos
		 WHERE platform_host = 'ghes.company.com'
		   AND id = ? AND lifecycle_state = 'inactive'`, gheRepoID,
	).Scan(&gheCount)
	require.NoError(err)
	assert.Equal(1, gheCount)
	var identifiedRepoID int64
	require.NoError(d.ReadDB().QueryRowContext(ctx, `
		SELECT repo_id FROM forge_workspaces WHERE id = 'ws-ghe-identified'`,
	).Scan(&identifiedRepoID))
	assert.Equal(gheRepoID, identifiedRepoID)
	_, err = d.WriteDB().ExecContext(ctx,
		`DELETE FROM forge_repos WHERE id = ?`, gheRepoID,
	)
	require.Error(err, "workspace repository identities must not be nulled by deletion")

	// ghes.company.com MR should be gone.
	var gheMRCount int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_merge_requests
		 WHERE repo_id = ?`, gheRepoID,
	).Scan(&gheMRCount)
	require.NoError(err)
	assert.Equal(0, gheMRCount)

	// ghes.company.com events should be gone.
	var gheEvtCount int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_mr_events
		 WHERE dedupe_key = 'ghe-evt-1'`,
	).Scan(&gheEvtCount)
	require.NoError(err)
	assert.Equal(0, gheEvtCount)

	// github.com rate limits should remain.
	ghRL, err := d.GetRateLimit("github.com", "user:1", "rest")
	require.NoError(err)
	require.NotNil(ghRL)
	assert.Equal(10, ghRL.RequestsHour)

	// ghes.company.com rate limits should be gone.
	gheRL, err := d.GetRateLimit("ghes.company.com", "user:1", "rest")
	require.NoError(err)
	assert.Nil(gheRL)
}

// TestCascadeDeleteRepo verifies that deleting a repo on a fresh DB
// cascades to all dependent tables (mr_events, workflow_state, issue_events).
func TestCascadeDeleteRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")

	// Create MR with events and kanban state.
	mrID := insertTestMR(t, d, repoID, 1, "test PR", base)
	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{
			MergeRequestID: mrID,
			EventType:      "comment",
			Author:         "alice",
			CreatedAt:      base,
			DedupeKey:      "cascade-mr-evt",
		},
	}))
	require.NoError(d.SetKanbanState(ctx, mrID, "reviewing"))

	// Create issue with events.
	issueID := insertTestIssue(t, d, repoID, 10, "test issue", base)
	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{
		{
			IssueID:   issueID,
			EventType: "comment",
			Author:    "bob",
			CreatedAt: base,
			DedupeKey: "cascade-issue-evt",
		},
	}))

	// Direct delete of the repo should cascade through all dependents.
	_, err := d.WriteDB().ExecContext(ctx,
		`DELETE FROM forge_repos WHERE id = ?`, repoID,
	)
	require.NoError(err)

	// All dependent rows should be gone.
	var count int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_merge_requests`,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(0, count)

	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_mr_events`,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(0, count)

	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_item_workflow_state`,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(0, count)

	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_issues`,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(0, count)

	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_issue_events`,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(0, count)
}

func TestUpsertMREventsUpdatesExistingEventBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "edited comment", base)
	platformID := int64(101)

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "alice",
		Body:           "original body",
		CreatedAt:      base,
		DedupeKey:      "comment-101",
	}}))

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "alice",
		Body:           "edited body",
		CreatedAt:      base,
		DedupeKey:      "comment-101",
	}}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("edited body", events[0].Body)
}

func TestUpsertMREventsPreservesEnrichedMergedActor(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "pr", baseTime())

	event := MREvent{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      baseTime(),
		DedupeKey:      "provider-merged-event",
	}
	require.NoError(d.UpsertMREvents(ctx, []MREvent{event}))
	event.Author = ""
	require.NoError(d.UpsertMREvents(ctx, []MREvent{event}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal("merge-admin", events[0].Author)
}

func TestUpsertMREventsRejectsDistinctActorlessMergeAfterAuthoredMerge(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "pr", baseTime())

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      baseTime(),
		DedupeKey:      "authored-merge",
	}}))
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		EventType:      "merged",
		Summary:        "merged this",
		CreatedAt:      baseTime().Add(time.Minute),
		DedupeKey:      "actorless-provider-refresh",
	}}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal("authored-merge", events[0].DedupeKey)
	require.Equal("merge-admin", events[0].Author)
}

func TestUpsertMREventsDeduplicatesDistinctAuthoredMergeEvents(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "pr", baseTime())

	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{
			MergeRequestID: mrID,
			EventType:      "merged",
			Author:         "first-admin",
			Summary:        "merged this",
			CreatedAt:      baseTime(),
			DedupeKey:      "first-authored-merge",
		},
		{
			MergeRequestID: mrID,
			EventType:      "merged",
			Author:         "second-admin",
			Summary:        "merged this",
			CreatedAt:      baseTime().Add(time.Minute),
			DedupeKey:      "second-authored-merge",
		},
	}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal("first-authored-merge", events[0].DedupeKey)
	require.Equal("second-admin", events[0].Author)
	require.Equal(baseTime().Add(time.Minute), events[0].CreatedAt)
}

func TestUpsertMergedActorEventUsesCurrentParentMergedAt(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	initialMergedAt := baseTime()
	mrID := insertTestMRWithOptions(t, d, testMR(repoID, 1, func(mr *MergeRequest) {
		mr.State = MergeRequestStateMerged
		mr.MergedAt = &initialMergedAt
	}))
	currentMergedAt := initialMergedAt.Add(time.Hour)
	require.NoError(d.UpdateMRState(
		ctx, repoID, 1, "merged", &currentMergedAt, &currentMergedAt,
	))

	changed, err := d.UpsertMergedActorEvent(ctx, MREvent{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      initialMergedAt,
		DedupeKey:      "stale-provider-event",
	})
	require.NoError(err)
	require.True(changed)
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal(currentMergedAt, events[0].CreatedAt)
}

func TestUpsertMergedActorEventAcceptsLegacyParentWithMergedAt(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	mergedAt := baseTime()
	mrID := insertTestMRWithOptions(t, d, testMR(repoID, 1, func(mr *MergeRequest) {
		mr.State = MergeRequestStateOpen
		mr.MergedAt = &mergedAt
	}))

	changed, err := d.UpsertMergedActorEvent(ctx, MREvent{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      mergedAt,
		DedupeKey:      "legacy-merged-event",
	})
	require.NoError(err)
	require.True(changed)
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.Equal("merge-admin", events[0].Author)
	require.Equal(mergedAt, events[0].CreatedAt)
}

func TestUpsertMergedActorEventRejectsNonMergedParent(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "pr", baseTime())

	changed, err := d.UpsertMergedActorEvent(ctx, MREvent{
		MergeRequestID: mrID,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      baseTime(),
		DedupeKey:      "stale-provider-event",
	})
	require.NoError(err)
	require.False(changed)
	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Empty(events)
}

func TestUpsertMREventsUpdatesExistingReviewState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "dismissed review", base)
	platformID := int64(202)

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "review",
		Author:         "carol",
		Summary:        "APPROVED",
		CreatedAt:      base,
		DedupeKey:      "review-202",
	}}))

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "review",
		Author:         "carol",
		Summary:        "DISMISSED",
		CreatedAt:      base.Add(time.Hour),
		DedupeKey:      "review-202",
	}}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("DISMISSED", events[0].Summary)
	assert.Equal(base.Add(time.Hour), events[0].CreatedAt)
}

func TestUpsertMREventsWithThreadID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "discussion test", base)
	platformID := int64(101)
	threadID := "abc123def"

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "needs fix",
		CreatedAt:      base,
		DedupeKey:      "note-101",
		ThreadID:       &threadID,
		PositionJSON:   `{"new_path":"main.go","new_line":42}`,
		Resolvable:     true,
		Resolved:       false,
	}}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.NotNil(events[0].ThreadID)
	assert.Equal("abc123def", *events[0].ThreadID)
	assert.JSONEq(`{"new_path":"main.go","new_line":42}`, events[0].PositionJSON)
	assert.True(events[0].Resolvable)
	assert.False(events[0].Resolved)
}

// Comment edits flow back through the upsert without a thread id because
// provider note responses (GitLab) do not include the discussion id. The
// stored thread id must survive such updates instead of detaching the
// comment from its discussion until the next sync.
func TestUpsertMREventsKeepsThreadIDWhenIncomingIsNil(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 1, "thread preservation", base)
	platformID := int64(101)
	threadID := "abc123def"

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "original body",
		CreatedAt:      base,
		DedupeKey:      "note-101",
		ThreadID:       &threadID,
		PositionJSON:   `{"new_path":"main.go","new_line":42}`,
		Resolvable:     true,
		Resolved:       true,
	}}))

	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "edited body",
		CreatedAt:      base,
		DedupeKey:      "note-101",
	}}))

	events, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("edited body", events[0].Body)
	require.NotNil(events[0].ThreadID)
	assert.Equal(threadID, *events[0].ThreadID)
	assert.JSONEq(`{"new_path":"main.go","new_line":42}`, events[0].PositionJSON)
	assert.True(events[0].Resolvable, "resolvable must survive a threadless update")
	assert.True(events[0].Resolved, "resolved must survive a threadless update")

	// Updates that do carry a thread id (sync) must still refresh the
	// discussion metadata, including unresolving.
	newThreadID := "def456abc"
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "edited body",
		CreatedAt:      base,
		DedupeKey:      "note-101",
		ThreadID:       &newThreadID,
		Resolvable:     true,
		Resolved:       false,
	}}))

	events, err = d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	require.NotNil(events[0].ThreadID)
	assert.Equal(newThreadID, *events[0].ThreadID)
	assert.Empty(events[0].PositionJSON)
	assert.True(events[0].Resolvable)
	assert.False(events[0].Resolved)
}

func TestUpsertIssueEventsKeepsThreadIDWhenIncomingIsNil(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     301,
		Number:         2,
		URL:            "https://gitlab.com/acme/widget/-/issues/2",
		Title:          "issue thread preservation",
		Author:         "alice",
		State:          "open",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: base,
	})
	require.NoError(err)
	platformID := int64(401)
	threadID := "issue-thread-1"

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:    issueID,
		PlatformID: &platformID,
		EventType:  "issue_comment",
		Author:     "alice",
		Body:       "original body",
		CreatedAt:  base,
		DedupeKey:  "issue-note-401",
		ThreadID:   &threadID,
	}}))

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:    issueID,
		PlatformID: &platformID,
		EventType:  "issue_comment",
		Author:     "alice",
		Body:       "edited body",
		CreatedAt:  base,
		DedupeKey:  "issue-note-401",
	}}))

	events, err := d.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("edited body", events[0].Body)
	require.NotNil(events[0].ThreadID)
	assert.Equal(threadID, *events[0].ThreadID)
}

func TestUpsertIssueEventsUpdatesExistingEventBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     201,
		Number:         1,
		URL:            "https://github.com/acme/widget/issues/1",
		Title:          "edited issue comment",
		Author:         "alice",
		State:          "open",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: base,
	})
	require.NoError(err)
	platformID := int64(202)

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:    issueID,
		PlatformID: &platformID,
		EventType:  "issue_comment",
		Author:     "alice",
		Body:       "original body",
		CreatedAt:  base,
		DedupeKey:  "issue-comment-202",
	}}))

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:    issueID,
		PlatformID: &platformID,
		EventType:  "issue_comment",
		Author:     "alice",
		Body:       "edited body",
		CreatedAt:  base,
		DedupeKey:  "issue-comment-202",
	}}))

	events, err := d.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("edited body", events[0].Body)
}

func TestIssuePRReferencesMaterializeAndFilterIssues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	referencedID := insertTestIssue(t, d, repoID, 1, "referenced", base)
	insertTestIssue(t, d, repoID, 2, "not referenced", base.Add(time.Minute))

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:   referencedID,
		EventType: "cross_referenced",
		MetadataJSON: `{
			"source_type":"PullRequest",
			"source_owner":"acme",
			"source_repo":"client",
			"source_number":42,
			"source_url":"https://github.com/acme/client/pull/42"
		}`,
		CreatedAt: base,
		DedupeKey: "cross-reference-1",
	}}))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{ReferencedByPR: true})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(referencedID, issues[0].ID)

	// A second observation of the same graph edge refreshes its evidence
	// instead of manufacturing a duplicate edge.
	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:   referencedID,
		EventType: "cross_referenced",
		MetadataJSON: `{
			"source_type":"PullRequest",
			"source_owner":"acme",
			"source_repo":"client",
			"source_number":42,
			"source_url":"https://github.com/acme/client/pulls/42"
		}`,
		CreatedAt: base.Add(time.Hour),
		DedupeKey: "cross-reference-2",
	}}))

	var count int
	var sourceURL, observedKey string
	require.NoError(d.ReadDB().QueryRowContext(ctx, `
		SELECT COUNT(*), source_url, observed_event_key
		FROM forge_issue_pr_references
		WHERE issue_id = ?`, referencedID,
	).Scan(&count, &sourceURL, &observedKey))
	assert.Equal(1, count)
	assert.Equal("https://github.com/acme/client/pulls/42", sourceURL)
	assert.Equal("cross-reference-2", observedKey)

	// Reference rows are historical evidence. Removing an event does not
	// reconcile the graph until provider unlink detection is implemented.
	_, err = d.WriteDB().ExecContext(ctx,
		`DELETE FROM forge_issue_events WHERE issue_id = ?`, referencedID,
	)
	require.NoError(err)
	issues, err = d.ListIssues(ctx, ListIssuesOpts{ReferencedByPR: true})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(referencedID, issues[0].ID)
}

func TestIssuePRReferenceMaterializationIgnoresIncompleteEvidence(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID := insertTestIssue(t, d, repoID, 1, "issue", baseTime())

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{
		{
			IssueID: issueID, EventType: "cross_referenced",
			MetadataJSON: "not-json", CreatedAt: baseTime(), DedupeKey: "malformed",
		},
		{
			IssueID: issueID, EventType: "cross_referenced",
			MetadataJSON: `{"source_type":"Issue","source_owner":"acme","source_repo":"client","source_number":2,"source_url":"https://github.com/acme/client/issues/2"}`,
			CreatedAt:    baseTime(), DedupeKey: "issue-source",
		},
	}))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{ReferencedByPR: true})
	require.NoError(err)
	require.Empty(issues)
}

func TestIssueEventsDedupeIsScopedToIssue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "o", "r")
	firstIssueID := insertTestIssue(t, d, repoID, 1, "issue one", base)
	secondIssueID := insertTestIssue(t, d, repoID, 2, "issue two", base.Add(time.Minute))
	firstPlatformID := int64(5001)
	secondPlatformID := int64(5002)

	sharedDedupeKey := "gitlab:gitlab.example.com:o/r:issue:note:shared"
	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{
		{
			IssueID:            firstIssueID,
			PlatformID:         &firstPlatformID,
			PlatformExternalID: "gid://gitlab/Note/5001",
			EventType:          "issue_comment",
			Author:             "alice",
			CreatedAt:          base,
			DedupeKey:          sharedDedupeKey,
		},
		{
			IssueID:            secondIssueID,
			PlatformID:         &secondPlatformID,
			PlatformExternalID: "gid://gitlab/Note/5002",
			EventType:          "issue_comment",
			Author:             "bob",
			CreatedAt:          base.Add(time.Minute),
			DedupeKey:          sharedDedupeKey,
		},
	}))

	firstEvents, err := d.ListIssueEvents(ctx, firstIssueID)
	require.NoError(err)
	require.Len(firstEvents, 1)
	assert.Equal(sharedDedupeKey, firstEvents[0].DedupeKey)
	assert.Equal("gid://gitlab/Note/5001", firstEvents[0].PlatformExternalID)

	secondEvents, err := d.ListIssueEvents(ctx, secondIssueID)
	require.NoError(err)
	require.Len(secondEvents, 1)
	assert.Equal(sharedDedupeKey, secondEvents[0].DedupeKey)
	assert.Equal("gid://gitlab/Note/5002", secondEvents[0].PlatformExternalID)
}

func TestUpsertIssueEventsWithThreadID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     301,
		Number:         5,
		URL:            "https://gitlab.com/acme/widget/-/issues/5",
		Title:          "discussion test",
		Author:         "reporter",
		State:          "open",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: base,
	})
	require.NoError(err)

	platformID := int64(501)
	threadID := "issue-disc-xyz"

	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:    issueID,
		PlatformID: &platformID,
		EventType:  "issue_comment",
		Author:     "commenter",
		Body:       "issue comment",
		CreatedAt:  base,
		DedupeKey:  "issue-note-501",
		ThreadID:   &threadID,
	}}))

	events, err := d.ListIssueEvents(ctx, issueID)
	require.NoError(err)
	require.Len(events, 1)
	assert.NotNil(events[0].ThreadID)
	assert.Equal("issue-disc-xyz", *events[0].ThreadID)
}

func TestItemsPersistPlatformExternalID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	_, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:             repoID,
		PlatformID:         1001,
		PlatformExternalID: "gid://gitlab/MergeRequest/1001",
		Number:             1,
		URL:                "https://gitlab.example.com/acme/widget/-/merge_requests/1",
		Title:              "external mr",
		Author:             "alice",
		State:              "open",
		CreatedAt:          base,
		UpdatedAt:          base,
		LastActivityAt:     base,
	})
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, &Issue{
		RepoID:             repoID,
		PlatformID:         2001,
		PlatformExternalID: "gid://gitlab/Issue/2001",
		Number:             2,
		URL:                "https://gitlab.example.com/acme/widget/-/issues/2",
		Title:              "external issue",
		Author:             "bob",
		State:              "open",
		CreatedAt:          base,
		UpdatedAt:          base,
		LastActivityAt:     base,
	})
	require.NoError(err)

	mr, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("gid://gitlab/MergeRequest/1001", mr.PlatformExternalID)

	issue, err := d.GetIssueByRepoIDAndNumber(ctx, repoID, 2)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("gid://gitlab/Issue/2001", issue.PlatformExternalID)
}

func TestUpsertAndListRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	id1, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "alice", "alpha"))
	require.NoError(err)
	id2, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "bob", "beta"))
	require.NoError(err)
	assert.NotEqual(id1, id2)

	// Idempotency: re-inserting should return the same ID.
	id1Again, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "alice", "alpha"))
	require.NoError(err)
	assert.Equal(id1, id1Again)

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 2)
	// Ordered by owner, name: alice/alpha, bob/beta.
	assert.Equal("alice", repos[0].Owner)
	assert.Equal("alpha", repos[0].Name)
	assert.Equal("bob", repos[1].Owner)
	assert.Equal("beta", repos[1].Name)
}

func TestUpsertRepoDefaultsToGitHubIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	id, err := d.UpsertRepo(ctx, GitHubRepoIdentity("github.com", "Alice", "Alpha"))
	require.NoError(err)

	repo, err := d.GetRepoByID(ctx, id)
	require.NoError(err)
	require.NotNil(repo)
	assert.Equal("github", repo.Platform)
	assert.Equal("github.com", repo.PlatformHost)
	assert.Equal("alice", repo.Owner)
	assert.Equal("alpha", repo.Name)
	assert.Equal("alice/alpha", repo.RepoPath)
	assert.Equal("alice", repo.OwnerKey)
	assert.Equal("alpha", repo.NameKey)
	assert.Equal("alice/alpha", repo.RepoPathKey)
	assert.Empty(repo.PlatformRepoID)
	assert.Empty(repo.WebURL)
	assert.Empty(repo.CloneURL)
	assert.Empty(repo.DefaultBranch)
}

func TestUpsertRepoSupportsProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	githubID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "github",
		PlatformHost:   "example.com",
		PlatformRepoID: "github-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	gitlabID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "example.com",
		PlatformRepoID: "gitlab-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)

	assert.NotEqual(githubID, gitlabID)

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 2)
	assert.Equal("github", repos[0].Platform)
	assert.Equal("gitlab", repos[1].Platform)
	for _, repo := range repos {
		assert.Equal("example.com", repo.PlatformHost)
		assert.Equal("acme/widget", repo.RepoPath)
		assert.Equal("acme", repo.OwnerKey)
		assert.Equal("widget", repo.NameKey)
		assert.Equal("acme/widget", repo.RepoPathKey)
	}
}

func TestUpsertRepoPreservesNonGitHubDisplayIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	id, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.example.com",
		Owner:        "Group/SubGroup",
		Name:         "ProjectName",
		RepoPath:     "Group/SubGroup/ProjectName",
	})
	require.NoError(err)

	repo, err := d.GetRepoByID(ctx, id)
	require.NoError(err)
	require.NotNil(repo)
	assert.Equal("Group/SubGroup", repo.Owner)
	assert.Equal("ProjectName", repo.Name)
	assert.Equal("Group/SubGroup/ProjectName", repo.RepoPath)
	assert.Equal("group/subgroup", repo.OwnerKey)
	assert.Equal("projectname", repo.NameKey)
	assert.Equal("group/subgroup/projectname", repo.RepoPathKey)
}

func TestProviderCanonicalReadPathsUseLookupKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gitlab-project-name",
		Owner:          "Group/SubGroup",
		Name:           "ProjectName",
		RepoPath:       "Group/SubGroup/ProjectName",
	})
	require.NoError(err)
	mrID := insertTestMRWithOptions(t, d, testMR(repoID, 7, withMRTitle("GitLab PR")))
	issueID := insertTestIssueWithOptions(t, d, testIssue(repoID, 8, withIssueTitle("GitLab issue")))

	gotMR, err := d.GetMergeRequest(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 7)
	require.NoError(err)
	require.NotNil(gotMR)
	assert.Equal(mrID, gotMR.ID)

	listedMRs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		PlatformHost: "gitlab.example.com",
		RepoOwner:    "GROUP/SubGroup",
		RepoName:     "PROJECTName",
	})
	require.NoError(err)
	require.Len(listedMRs, 1)
	assert.Equal(mrID, listedMRs[0].ID)

	require.NoError(d.UpdateDiffSHAs(ctx, repoID, 7, "head", "base", "merge"))
	shas, err := d.GetDiffSHAs(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 7)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal("head", shas.DiffHeadSHA)

	gotIssue, err := d.GetIssue(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 8)
	require.NoError(err)
	require.NotNil(gotIssue)
	assert.Equal(issueID, gotIssue.ID)

	listedIssues, err := d.ListIssues(ctx, ListIssuesOpts{
		PlatformHost: "gitlab.example.com",
		RepoOwner:    "GROUP/SubGroup",
		RepoName:     "PROJECTName",
	})
	require.NoError(err)
	require.Len(listedIssues, 1)
	assert.Equal(issueID, listedIssues[0].ID)

	require.NoError(d.UpdateMRDetailFetched(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 7, true))
	refreshedMR, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(refreshedMR)
	require.NotNil(refreshedMR.DetailFetchedAt)
	assert.True(refreshedMR.CIHadPending)

	require.NoError(d.UpdateIssueDetailFetched(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 8))
	refreshedIssue, err := d.GetIssueByRepoIDAndNumber(ctx, repoID, 8)
	require.NoError(err)
	require.NotNil(refreshedIssue)
	assert.NotNil(refreshedIssue.DetailFetchedAt)

	users, err := d.ListCommentAutocompleteUsers(
		ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", "auth", nil, 10,
	)
	require.NoError(err)
	assert.Equal([]string{"author"}, users)

	refs, err := d.ListCommentAutocompleteReferences(
		ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", "GitLab", "", 10,
	)
	require.NoError(err)
	require.Len(refs, 2)
	assert.Equal([]int{8, 7}, []int{refs[0].Number, refs[1].Number})

	activity, err := d.ListActivity(ctx, ListActivityOpts{
		Repo:  "gitlab.example.com/group/subgroup/projectname",
		Limit: 10,
	})
	require.NoError(err)
	require.Len(activity, 2)
	assert.ElementsMatch([]int{7, 8}, []int{activity[0].ItemNumber, activity[1].ItemNumber})

	stackID, err := d.UpsertStack(ctx, repoID, 7, "stack")
	require.NoError(err)
	require.NoError(d.ReplaceStackMembers(ctx, stackID, []StackMember{{
		MergeRequestID: mrID,
		Position:       0,
	}}))
	stacks, members, err := d.ListStacksWithMembers(ctx, "group/subgroup/projectname")
	require.NoError(err)
	require.Len(stacks, 1)
	assert.Equal(stackID, stacks[0].ID)
	require.Len(members[stackID], 1)
	assert.Equal(mrID, members[stackID][0].MergeRequestID)

	stack, stackMembers, err := d.GetStackForPR(ctx, "gitlab", "gitlab.example.com", "group/subgroup", "projectname", 7)
	require.NoError(err)
	require.NotNil(stack)
	assert.Equal(stackID, stack.ID)
	require.Len(stackMembers, 1)
	assert.Equal(mrID, stackMembers[0].MergeRequestID)
}

func TestUpdateRepoProviderMetadataPreservesIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID, err := d.UpsertRepo(ctx, GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	err = d.UpdateRepoProviderMetadata(ctx, repoID, RepoProviderMetadata{
		PlatformRepoID: "R_123",
		WebURL:         "https://github.com/acme/widget",
		CloneURL:       "https://github.com/acme/widget.git",
		DefaultBranch:  "main",
	})
	require.NoError(err)

	repo, err := d.GetRepoByID(ctx, repoID)
	require.NoError(err)
	require.NotNil(repo)
	assert.Equal("github", repo.Platform)
	assert.Equal("github.com", repo.PlatformHost)
	assert.Equal("acme", repo.Owner)
	assert.Equal("widget", repo.Name)
	assert.Equal("acme/widget", repo.RepoPath)
	assert.Equal("R_123", repo.PlatformRepoID)
	assert.Equal("https://github.com/acme/widget", repo.WebURL)
	assert.Equal("https://github.com/acme/widget.git", repo.CloneURL)
	assert.Equal("main", repo.DefaultBranch)

	sameID, err := d.UpsertRepo(ctx, GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	assert.Equal(repoID, sameID)
}

// UpsertRepoByProviderID deliberately does not apply renames: cached
// identities resolve read-only, and route moves happen only through
// ReconcileRepositoryObservation (see
// TestUpsertRepoCachedIdentityDoesNotReclaimReusedRoute and
// TestReconcileRepositoryObservationRenamesSameProviderID).

func TestReplaceRepoLabelCatalogKeepsAssignedHistoricalLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	now := baseTime()

	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{
		{PlatformID: 1, Name: "bug", Description: "Broken", Color: "d73a4a", IsDefault: true, UpdatedAt: now},
		{PlatformID: 2, Name: "triage", Description: "Needs triage", Color: "fbca04", UpdatedAt: now},
	}, now))

	catalog, freshness, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 2)
	assert.Equal("bug", catalog[0].Name)
	assert.Equal("triage", catalog[1].Name)
	require.NotNil(freshness.SyncedAt)
	assert.Equal(now, *freshness.SyncedAt)

	issueID := insertTestIssueWithOptions(t, d, testIssue(repoID, 7, withIssueTitle("issue")))
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{
		{PlatformID: 3, Name: "old", Description: "Removed upstream", Color: "cccccc", UpdatedAt: now},
	}))

	next := now.Add(time.Minute)
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{
		{PlatformID: 1, Name: "bug", Description: "Broken", Color: "d73a4a", IsDefault: true, UpdatedAt: next},
	}, next))

	catalog, _, err = d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("bug", catalog[0].Name)

	issue, err := d.GetIssueByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	assert.Equal("old", issue.Labels[0].Name)
}

func TestLabelMergePreservesCatalogMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	now := baseTime()
	issueID := insertTestIssueWithOptions(t, d, testIssue(repoID, 7, withIssueTitle("issue")))

	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID: 7,
		Name:       "provider-label",
		Color:      "cccccc",
		UpdatedAt:  now,
	}}))
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{
		Name:      "bug",
		Color:     "d73a4a",
		UpdatedAt: now,
	}}, now))

	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID: 7,
		Name:       "bug",
		Color:      "d73a4a",
		UpdatedAt:  now.Add(time.Minute),
	}}))

	catalog, _, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("bug", catalog[0].Name)
	assert.True(catalog[0].CatalogPresent)
	require.NotNil(catalog[0].CatalogSeenAt)
	assert.Equal(now, *catalog[0].CatalogSeenAt)
}

func TestLabelMergeDoesNotLetItemRowOverwriteCatalogMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	catalogAt := baseTime()
	itemAt := catalogAt.Add(time.Hour)

	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{
		PlatformID:  7,
		Name:        "catalog",
		Description: "catalog metadata",
		Color:       "0e8a16",
		UpdatedAt:   catalogAt,
	}}, catalogAt))
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{Name: "stale", Description: "item metadata", Color: "d73a4a", UpdatedAt: itemAt}}))
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{PlatformID: 7, Name: "stale", Description: "item metadata", Color: "d73a4a", UpdatedAt: itemAt}}))

	catalog, _, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("catalog", catalog[0].Name)
	assert.Equal("catalog metadata", catalog[0].Description)
	assert.Equal("0e8a16", catalog[0].Color)
}

func TestCatalogMetadataOverridesItemMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID := insertTestIssueWithOptions(t, d, testIssue(repoID, 7))
	catalogUpdated := baseTime()
	itemUpdated := catalogUpdated.Add(time.Hour)
	syncedAt := itemUpdated.Add(time.Hour)

	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID:  7,
		Name:        "bug",
		Description: "item snapshot",
		Color:       "cccccc",
		IsDefault:   false,
		UpdatedAt:   itemUpdated,
	}}))
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{
		PlatformID:  7,
		Name:        "bug",
		Description: "catalog",
		Color:       "0e8a16",
		IsDefault:   true,
		UpdatedAt:   catalogUpdated,
	}}, syncedAt))
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID:  7,
		Name:        "bug",
		Description: "newer item snapshot",
		Color:       "d73a4a",
		UpdatedAt:   syncedAt.Add(time.Hour),
	}}))

	catalog, _, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("catalog", catalog[0].Description)
	assert.Equal("0e8a16", catalog[0].Color)
	assert.True(catalog[0].IsDefault)
}

func TestLabelMergeKeepsFresherCatalogMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	oldSeenAt := baseTime()
	newSeenAt := oldSeenAt.Add(time.Hour)

	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{
		PlatformID: 7,
		Name:       "provider-label",
		Color:      "cccccc",
		UpdatedAt:  oldSeenAt,
	}}, oldSeenAt))
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{
		Name:        "bug",
		Description: "fresh catalog",
		Color:       "0e8a16",
		UpdatedAt:   newSeenAt,
	}}, newSeenAt))

	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  7,
		Name:        "bug",
		Description: "stale item",
		Color:       "d73a4a",
		UpdatedAt:   oldSeenAt,
	}}))

	catalog, _, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	require.NotNil(catalog[0].CatalogSeenAt)
	assert.Equal(newSeenAt, *catalog[0].CatalogSeenAt)
	assert.Equal("bug", catalog[0].Name)
	assert.Equal("fresh catalog", catalog[0].Description)
	assert.Equal("0e8a16", catalog[0].Color)
}

func TestRepoLabelCatalogWritesIgnoreStaleResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	oldTime := baseTime()
	newTime := oldTime.Add(time.Hour)

	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{Name: "new", Color: "0e8a16", UpdatedAt: newTime}}, newTime))
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{Name: "old", Color: "cccccc", UpdatedAt: oldTime}}, oldTime))
	catalog, freshness, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("new", catalog[0].Name)
	require.NotNil(freshness.SyncedAt)
	assert.Equal(newTime, *freshness.SyncedAt)

	require.NoError(d.UpdateRepoLabelCatalogCheck(ctx, repoID, oldTime, "old failure"))
	_, freshness, err = d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	assert.Empty(freshness.SyncError)
	require.NotNil(freshness.CheckedAt)
	assert.Equal(newTime, *freshness.CheckedAt)
}

func TestRepoLabelCatalogOlderSuccessKeepsNewerFailedCheck(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	oldSuccess := baseTime()
	newFailure := oldSuccess.Add(time.Hour)

	require.NoError(d.UpdateRepoLabelCatalogCheck(ctx, repoID, newFailure, "provider down"))
	require.NoError(d.ReplaceRepoLabelCatalog(ctx, repoID, []Label{{Name: "bug", Color: "d73a4a", UpdatedAt: oldSuccess}}, oldSuccess))

	catalog, freshness, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.Len(catalog, 1)
	assert.Equal("bug", catalog[0].Name)
	require.NotNil(freshness.SyncedAt)
	assert.Equal(oldSuccess, *freshness.SyncedAt)
	require.NotNil(freshness.CheckedAt)
	assert.Equal(newFailure, *freshness.CheckedAt)
	assert.Equal("provider down", freshness.SyncError)
}

func TestRepoLabelCatalogFreshnessTracksCheckedSyncedAndErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "acme", "widget")
	checked := baseTime()
	synced := checked.Add(time.Second)

	require.NoError(d.UpdateRepoLabelCatalogCheck(ctx, repoID, checked, "provider down"))
	_, freshness, err := d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.NotNil(freshness.CheckedAt)
	assert.Equal(checked, *freshness.CheckedAt)
	assert.Nil(freshness.SyncedAt)
	assert.Equal("provider down", freshness.SyncError)

	require.NoError(d.MarkRepoLabelCatalogSynced(ctx, repoID, synced))
	_, freshness, err = d.ListRepoLabelCatalog(ctx, repoID)
	require.NoError(err)
	require.NotNil(freshness.SyncedAt)
	assert.Equal(synced, *freshness.SyncedAt)
	assert.Empty(freshness.SyncError)
}

func TestUpsertRepoCasefoldsOwnerAndName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	id, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "Org", "Foo"))
	require.NoError(err)

	sameID, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "org", "foo"))
	require.NoError(err)
	assert.Equal(id, sameID)

	repos, err := d.ListRepos(ctx)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal("org", repos[0].Owner)
	assert.Equal("foo", repos[0].Name)
}

func TestUpdateRepoSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	id, err := d.UpsertRepoByProviderID(ctx, RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-o-r",
		Owner:          "o",
		Name:           "r",
	})
	require.NoError(err)
	now := baseTime()

	require.NoError(d.UpdateRepoSyncStarted(ctx, id, now))
	later := now.Add(time.Minute)
	require.NoError(d.UpdateRepoSyncCompleted(ctx, id, later, ""))

	r, err := d.GetRepoByIdentity(ctx, GitHubRepoIdentity("github.com", "o", "r"))
	require.NoError(err)
	require.NotNil(r)
	require.NotNil(r.LastSyncStartedAt)
	require.NotNil(r.LastSyncCompletedAt)
	assert.True(r.LastSyncStartedAt.Equal(now))
	assert.True(r.LastSyncCompletedAt.Equal(later))
	assert.Empty(r.LastSyncError)

	// Record a sync error.
	require.NoError(d.UpdateRepoSyncCompleted(ctx, id, later, "rate limited"))
	r2, _ := d.GetRepoByIdentity(ctx, GitHubRepoIdentity("github.com", "o", "r"))
	require.NotNil(r2)
	assert.Equal("rate limited", r2.LastSyncError)
}

func TestUpsertAndGetPullRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	now := baseTime()

	pr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     42,
		Number:         7,
		URL:            "https://github.com/owner/repo/pull/7",
		Title:          "fix: something",
		Author:         "alice",
		State:          "open",
		IsDraft:        false,
		IsLocked:       true,
		Body:           "body text",
		HeadBranch:     "fix/something",
		BaseBranch:     "main",
		Additions:      10,
		AdditionsKnown: true,
		Deletions:      3,
		DeletionsKnown: true,
		FilesChanged:   new(17),
		MergeCommitSHA: "abc123",
		CommentCount:   2,
		ReviewDecision: "APPROVED",
		CIStatus:       "SUCCESS",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}

	id, err := d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)
	assert.NotZero(id)

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(id, got.ID)
	assert.Equal(pr.Title, got.Title)
	assert.Equal(pr.Author, got.Author)
	assert.Equal(pr.Additions, got.Additions)
	require.NotNil(got.FilesChanged)
	assert.Equal(17, *got.FilesChanged)
	assert.Equal("abc123", got.MergeCommitSHA)
	assert.True(got.IsLocked)
	assert.Empty(got.KanbanStatus)

	// Update via upsert — change title and additions.
	pr.Title = "fix: something updated"
	pr.Additions = 20
	pr.UpdatedAt = now.Add(time.Hour)
	pr.LastActivityAt = now.Add(time.Hour)

	id2, err := d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)
	assert.Equal(id, id2)

	got2, _ := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NotNil(got2)
	assert.Equal("fix: something updated", got2.Title)
	assert.Equal(20, got2.Additions)
	assert.True(got2.IsLocked)
	// created_at must not change.
	assert.True(got2.CreatedAt.Equal(now))

	// Omitted metrics preserve the last provider-confirmed value, while an
	// explicit zero remains authoritative.
	pr.Additions = 999
	pr.AdditionsKnown = false
	pr.UpdatedAt = now.Add(2 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)
	preserved, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NotNil(preserved)
	assert.Equal(20, preserved.Additions)

	pr.Additions = 0
	pr.AdditionsKnown = true
	pr.UpdatedAt = now.Add(3 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)
	zeroed, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 7)
	require.NoError(err)
	require.NotNil(zeroed)
	assert.Zero(zeroed.Additions)

	// Missing PR returns nil.
	missing, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", 999)
	require.NoError(err)
	assert.Nil(missing)
}

func TestUpsertMergeRequestUsesAuthoritativeProviderActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	staleDerivedActivity := base.Add(2 * time.Hour)
	providerActivity := base.Add(time.Hour)
	pr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     42,
		Number:         7,
		URL:            "https://github.com/owner/repo/pull/7",
		Title:          "fix: something",
		Author:         "alice",
		State:          "open",
		Body:           "body text",
		HeadBranch:     "fix/something",
		BaseBranch:     "main",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: staleDerivedActivity,
	}

	_, err := d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)

	pr.Title = "fix: something synced"
	pr.UpdatedAt = providerActivity
	pr.LastActivityAt = providerActivity
	_, err = d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("fix: something synced", got.Title)
	assert.Equal(providerActivity, got.LastActivityAt)
}

// TestUpsertMergeRequestCloneURLUnknownPreservesStoredValue proves an
// observation whose head clone URL could not be determined (a failed
// best-effort fork enrichment) never overwrites a previously known clone URL
// with an authoritative empty, while an authoritative empty without the
// unknown marker still clears the stored value.
func TestUpsertMergeRequestCloneURLUnknownPreservesStoredValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	mr := &MergeRequest{
		RepoID:           repoID,
		PlatformID:       42,
		Number:           7,
		Title:            "fork MR",
		Author:           "alice",
		State:            "open",
		HeadRepoCloneURL: "https://gitlab.example.com/fork/project.git",
		CreatedAt:        base,
		UpdatedAt:        base,
		LastActivityAt:   base,
	}
	_, err := d.UpsertMergeRequest(ctx, mr)
	require.NoError(err)

	unknown := *mr
	unknown.HeadRepoCloneURL = ""
	unknown.HeadRepoCloneURLUnknown = true
	unknown.UpdatedAt = base.Add(time.Hour)
	unknown.LastActivityAt = base.Add(time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &unknown)
	require.NoError(err)

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("https://gitlab.example.com/fork/project.git", got.HeadRepoCloneURL,
		"an unknown clone URL must preserve the stored value")

	cleared := *mr
	cleared.HeadRepoCloneURL = ""
	cleared.UpdatedAt = base.Add(2 * time.Hour)
	cleared.LastActivityAt = base.Add(2 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &cleared)
	require.NoError(err)

	got, err = d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Empty(got.HeadRepoCloneURL,
		"an authoritative empty clone URL must still clear the stored value")
}

func TestUpsertMergeRequestSnapshotReportsTimestampRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	newer := testMR(repoID, 7, withMRTitle("newer"), withMRActivity(baseTime().Add(time.Hour)))
	_, err := d.UpsertMergeRequest(ctx, newer)
	require.NoError(err)
	older := testMR(repoID, 7, withMRTitle("older"), withMRActivity(baseTime()))

	_, accepted, err := d.UpsertMergeRequestSnapshot(ctx, older)
	require.NoError(err)
	assert.False(accepted)
	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("newer", got.Title)
}

func TestListPullRequests(t *testing.T) {
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	// Insert 3 PRs with different last_activity_at.
	insertTestMR(t, d, repoID, 1, "oldest", base)
	insertTestMR(t, d, repoID, 2, "middle", base.Add(time.Hour))
	insertTestMR(t, d, repoID, 3, "newest", base.Add(2*time.Hour))

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{})
	require.NoError(t, err)
	require.Len(t, prs, 3)
	// Newest first.
	assert.Equal(t, []int{3, 2, 1}, []int{prs[0].Number, prs[1].Number, prs[2].Number})
}

func TestListPullRequestsTreatsLockedAsClosed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "owner", "repo")
	locked := testMR(repoID, 1)
	locked.IsLocked = true
	_, err := d.UpsertMergeRequest(t.Context(), locked)
	require.NoError(err)

	open, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{State: "open"})
	require.NoError(err)
	assert.Empty(open)

	closed, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{State: "closed"})
	require.NoError(err)
	require.Len(closed, 1)
	assert.Equal(locked.Number, closed[0].Number)
}

func TestListPullRequestsFilterByRepo(t *testing.T) {
	d := openTestDB(t)

	repo1 := insertTestRepo(t, d, "owner", "repo1")
	repo2 := insertTestRepo(t, d, "owner", "repo2")
	base := baseTime()

	insertTestMR(t, d, repo1, 1, "pr in repo1", base)
	insertTestMR(t, d, repo2, 1, "pr in repo2", base)

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{RepoOwner: "owner", RepoName: "repo1"})
	require.NoError(t, err)
	require.Len(t, prs, 1)
	assert.Equal(t, repo1, prs[0].RepoID)
}

func TestListPullRequestsFilterByRepoID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	githubRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "github",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)
	insertTestMR(t, d, githubRepoID, 1, "github PR", base)
	insertTestMR(t, d, gitlabRepoID, 2, "gitlab MR", base.Add(time.Hour))

	prs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		RepoID: gitlabRepoID,
	})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(gitlabRepoID, prs[0].RepoID)
	assert.Equal("gitlab MR", prs[0].Title)
}

func TestListPullRequestsFilterByMultipleRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	firstRepo := insertTestRepoWithHost(t, d, "owner", "repo1", "github.com")
	secondRepo := insertTestRepoWithHost(t, d, "team", "repo2", "ghe.example.com")
	thirdRepo := insertTestRepoWithHost(t, d, "owner", "repo3", "github.com")
	insertTestMR(t, d, firstRepo, 1, "first", base)
	insertTestMR(t, d, secondRepo, 2, "second", base.Add(time.Hour))
	insertTestMR(t, d, thirdRepo, 3, "third", base.Add(2*time.Hour))

	prs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		RepoFilters: []RepoFilter{
			{PlatformHost: "github.com", RepoPath: "owner/repo1"},
			{PlatformHost: "ghe.example.com", RepoPath: "team/repo2"},
		},
	})
	require.NoError(err)
	require.Len(prs, 2)
	assert.Equal([]int64{secondRepo, firstRepo}, []int64{
		prs[0].RepoID,
		prs[1].RepoID,
	})
}

func TestListPullRequestsFilterByRepoIncludesAllHostsByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	githubRepo := insertTestRepo(t, d, "owner", "repo")
	enterpriseRepo := insertTestRepoWithHost(
		t, d, "owner", "repo", "ghe.example.com",
	)
	insertTestMR(t, d, githubRepo, 1, "github pr", base)
	insertTestMR(t, d, enterpriseRepo, 2, "enterprise pr", base.Add(time.Hour))

	allHosts, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		RepoOwner: "owner",
		RepoName:  "repo",
	})
	require.NoError(err)
	require.Len(allHosts, 2)
	assert.Equal([]int64{enterpriseRepo, githubRepo}, []int64{
		allHosts[0].RepoID,
		allHosts[1].RepoID,
	})

	enterpriseOnly, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		PlatformHost: "ghe.example.com",
		RepoOwner:    "owner",
		RepoName:     "repo",
	})
	require.NoError(err)
	require.Len(enterpriseOnly, 1)
	assert.Equal(enterpriseRepo, enterpriseOnly[0].RepoID)
}

func TestListPullRequestsFilterByHostedRepoPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	nestedRepo := insertTestRepoWithHost(
		t, d, "Group/SubGroup", "Project.Special", "ghe.example.com",
	)
	otherRepo := insertTestRepoWithHost(
		t, d, "Other", "Project.Special", "ghe.example.com",
	)
	insertTestMR(t, d, nestedRepo, 1, "nested pr", base)
	insertTestMR(t, d, otherRepo, 2, "other pr", base.Add(time.Hour))

	filtered, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		PlatformHost: "GHE.EXAMPLE.COM",
		RepoPath:     "Group/SubGroup/Project.Special",
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(nestedRepo, filtered[0].RepoID)
}

func TestPullRequestRepoScopedQueriesCanonicalizeOwnerName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	prID := insertTestMR(t, d, repoID, 7, "mixed case path", baseTime())
	require.NoError(d.UpdateDiffSHAs(ctx, repoID, 7, "head", "base", "merge"))

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "Owner", "Repo", 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(prID, got.ID)

	filtered, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{
		RepoOwner: "Owner",
		RepoName:  "Repo",
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(prID, filtered[0].ID)

	shas, err := d.GetDiffSHAs(ctx, "github", "github.com", "Owner", "Repo", 7)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal("head", shas.DiffHeadSHA)
}

func TestListPullRequestsFilterBySearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestMR(t, d, repoID, 1, "add feature", base)
	insertTestMR(t, d, repoID, 2, "fix bug", base.Add(time.Hour))
	insertTestMR(t, d, repoID, 3, "feature cleanup", base.Add(2*time.Hour))

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "feature"})
	require.NoError(err)
	require.Len(prs, 2)
	assert.ElementsMatch([]int{1, 3}, []int{prs[0].Number, prs[1].Number})

	prs, err = d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "feature add"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(1, prs[0].Number)
}

func TestListPullRequestsFilterBySearchPreservesApostrophesInTerms(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestMR(t, d, repoID, 1, "can't reproduce", base)
	insertTestMR(t, d, repoID, 2, "O'Reilly docs update", base.Add(time.Hour))

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "can't"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(1, prs[0].Number)

	prs, err = d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "O'Reilly"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(2, prs[0].Number)
}

func TestListPullRequestsFilterBySearchRepoFragment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	apiRepoID := insertTestRepo(t, d, "acme", "api")
	workerRepoID := insertTestRepo(t, d, "tools", "worker")
	base := baseTime()

	insertTestMR(t, d, apiRepoID, 1, "fix bug", base)
	insertTestMR(t, d, workerRepoID, 2, "fix bug", base.Add(time.Hour))

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "acm"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(1, prs[0].Number)

	prs, err = d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "work bug"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(2, prs[0].Number)
}

func TestListPullRequestsFilterBySearchNumber(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestMR(t, d, repoID, 12, "add feature", base)
	insertTestMR(t, d, repoID, 278, "fix bug", base.Add(time.Hour))
	insertTestMR(t, d, repoID, 290, "another change", base.Add(2*time.Hour))

	prs, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "278"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(278, prs[0].Number)

	prs, err = d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "#278"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(278, prs[0].Number)

	// Substring of number matches multiple.
	prs, err = d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Search: "2"})
	require.NoError(err)
	require.Len(prs, 3)
}

func TestListPullRequestsFilterBySearchLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestMR(t, d, repoID, 1, "add feature", base)
	prID := insertTestMR(t, d, repoID, 2, "fix bug", base.Add(time.Hour))
	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, prID, []Label{{
		PlatformID: 200,
		Name:       "needs-review",
		Color:      "fbca04",
		UpdatedAt:  base,
	}}))

	prs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{Search: "needs-review"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(2, prs[0].Number)
}

func TestListPullRequestsPaginationUsesStableTieBreaker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	activity := baseTime()
	insertTestMR(t, d, repoID, 1, "oldest id", activity)
	insertTestMR(t, d, repoID, 2, "middle id", activity)
	insertTestMR(t, d, repoID, 3, "newest id", activity)

	firstPage, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Limit: 1})
	require.NoError(err)
	require.Len(firstPage, 1)
	assert.Equal(3, firstPage[0].Number)

	secondPage, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{Limit: 1, Offset: 1})
	require.NoError(err)
	require.Len(secondPage, 1)
	assert.Equal(2, secondPage[0].Number)
}

func TestListMergeRequestsWorkspaceActivitySortsBeforePagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	insertTestMR(t, d, repoID, 1, "provider newest", base.Add(2*time.Hour))
	insertTestMR(t, d, repoID, 2, "workspace newest", base)

	first, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{
		Limit: 1,
		WorkspaceActivity: []ItemActivityOverride{{
			RepoID: repoID, ItemNumber: 2, ActivityAt: base.Add(3 * time.Hour),
		}},
	})
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal(2, first[0].Number)
	assert.Equal(base, first[0].LastActivityAt)

	second, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{
		Limit: 1, Offset: 1,
		WorkspaceActivity: []ItemActivityOverride{{
			RepoID: repoID, ItemNumber: 2, ActivityAt: base.Add(3 * time.Hour),
		}},
	})
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal(1, second[0].Number)
}

func TestListMergeRequestsWorkspaceActivitySupportsLargeSubjectSets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	insertTestMR(t, d, repoID, 1, "provider newest", base.Add(2*time.Hour))
	insertTestMR(t, d, repoID, 2, "workspace newest", base)

	overrides := make([]ItemActivityOverride, 11_000)
	for i := range overrides {
		overrides[i] = ItemActivityOverride{
			RepoID: repoID, ItemNumber: i + 1, ActivityAt: base.Add(time.Hour),
		}
	}
	overrides[1].ActivityAt = base.Add(3 * time.Hour)

	got, err := d.ListMergeRequests(t.Context(), ListMergeRequestsOpts{
		Limit: 1, WorkspaceActivity: overrides,
	})
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal(2, got[0].Number)
}

func TestListPullRequestsFilterByKanban(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestMR(t, d, repoID, 1, "pr 1", base)
	id2 := insertTestMR(t, d, repoID, 2, "pr 2", base.Add(time.Hour))
	id3 := insertTestMR(t, d, repoID, 3, "pr 3", base.Add(2*time.Hour))

	// Set PR 2 to "reviewing".
	require.NoError(d.SetKanbanState(ctx, id2, "reviewing"))
	// Leave PR 1 without a workflow-state row; missing status defaults to "new".
	require.NoError(d.EnsureKanbanState(ctx, id3))

	prs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{KanbanState: "reviewing"})
	require.NoError(err)
	require.Len(prs, 1)
	assert.Equal(2, prs[0].Number)
	assert.Equal(KanbanStatusReviewing, prs[0].KanbanStatus)

	prs, err = d.ListMergeRequests(ctx, ListMergeRequestsOpts{KanbanState: "new"})
	require.NoError(err)
	require.Len(prs, 2)
	assert.Equal([]int{3, 1}, []int{prs[0].Number, prs[1].Number})
}

func TestListMergeRequests_AttachesLabels(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "acme", "widget"))
	require.NoError(err)

	mrID, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:         repoID,
		PlatformID:     101,
		Number:         7,
		URL:            "https://github.com/acme/widget/pull/7",
		Title:          "Add labels",
		Author:         "alice",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, mrID, []Label{{
		PlatformID:  5001,
		Name:        "needs-review",
		Description: "Needs another reviewer",
		Color:       "fbca04",
		IsDefault:   true,
		UpdatedAt:   now,
	}}))

	mrs, err := d.ListMergeRequests(ctx, ListMergeRequestsOpts{})
	require.NoError(err)
	require.Len(mrs, 1)
	require.Len(mrs[0].Labels, 1)
	require.Equal("needs-review", mrs[0].Labels[0].Name)
	require.Equal("Needs another reviewer", mrs[0].Labels[0].Description)
	require.Equal("fbca04", mrs[0].Labels[0].Color)
	require.True(mrs[0].Labels[0].IsDefault)
	require.True(mrs[0].Labels[0].UpdatedAt.Equal(now))
}

func TestGetMergeRequest_AttachesLabels(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:         repoID,
		PlatformID:     102,
		Number:         8,
		URL:            "https://github.com/acme/widget/pull/8",
		Title:          "Detail labels",
		Author:         "alice",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, mrID, []Label{{
		PlatformID:  5002,
		Name:        "backend",
		Description: "Backend changes",
		Color:       "5319e7",
		IsDefault:   false,
		UpdatedAt:   now,
	}}))

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 8)
	require.NoError(err)
	require.NotNil(mr)
	require.Len(mr.Labels, 1)
	require.Equal("backend", mr.Labels[0].Name)
	require.Equal("Backend changes", mr.Labels[0].Description)
	require.Equal("5319e7", mr.Labels[0].Color)
	require.False(mr.Labels[0].IsDefault)
	require.True(mr.Labels[0].UpdatedAt.Equal(now))
}

func TestReplaceMergeRequestLabels_RejectsWrongRepoID(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoA := insertTestRepo(t, d, "acme", "widget")
	repoB := insertTestRepo(t, d, "acme", "gadget")
	mrID := insertTestMR(t, d, repoA, 9, "repo guarded", now)

	err := d.ReplaceMergeRequestLabels(ctx, repoB, mrID, []Label{{
		PlatformID:  9009,
		Name:        "wrong-repo",
		Description: "should fail",
		Color:       "ff0000",
		UpdatedAt:   now,
	}})
	require.Error(err)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 9)
	require.NoError(err)
	require.NotNil(mr)
	require.Empty(mr.Labels)
}

func TestUpsertLabels_UsesPlatformIDForRename(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  41,
		Name:        "old-name",
		Description: "before rename",
		Color:       "111111",
		UpdatedAt:   now,
	}}))
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  41,
		Name:        "new-name",
		Description: "after rename",
		Color:       "222222",
		IsDefault:   true,
		UpdatedAt:   now.Add(time.Minute),
	}}))

	var count int
	err := d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_labels WHERE repo_id = ?`,
		repoID,
	).Scan(&count)
	require.NoError(err)
	require.Equal(1, count)

	var name, description, color string
	var isDefault bool
	var updatedAt time.Time
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT name, description, color, is_default, updated_at
		 FROM forge_labels
		 WHERE repo_id = ? AND platform_id = ?`,
		repoID, 41,
	).Scan(&name, &description, &color, &isDefault, &updatedAt)
	require.NoError(err)
	require.Equal("new-name", name)
	require.Equal("after rename", description)
	require.Equal("222222", color)
	require.True(isDefault)
	require.True(updatedAt.Equal(now.Add(time.Minute)))
}

func TestUpsertLabels_UsesPlatformExternalIDForRename(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformExternalID: "gid://gitlab/Label/bug",
		Name:               "old-name",
		Description:        "before rename",
		Color:              "111111",
		UpdatedAt:          now,
	}}))
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformExternalID: "gid://gitlab/Label/bug",
		Name:               "new-name",
		Description:        "after rename",
		Color:              "222222",
		IsDefault:          true,
		UpdatedAt:          now.Add(time.Minute),
	}}))

	var count int
	err := d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_labels WHERE repo_id = ?`,
		repoID,
	).Scan(&count)
	require.NoError(err)
	require.Equal(1, count)

	var name, externalID string
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT name, platform_external_id
		 FROM forge_labels
		 WHERE repo_id = ? AND platform_external_id = ?`,
		repoID, "gid://gitlab/Label/bug",
	).Scan(&name, &externalID)
	require.NoError(err)
	require.Equal("new-name", name)
	require.Equal("gid://gitlab/Label/bug", externalID)
}

func TestUpsertLabels_MergesStaleNameOnlyRowIntoPlatformRow(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 17, "rename labels", now)
	issueID := insertTestIssue(t, d, repoID, 23, "rename labels", now)

	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  200,
		Name:        "old-name",
		Description: "old platform label",
		Color:       "111111",
		UpdatedAt:   now,
	}}))
	require.NoError(d.ReplaceMergeRequestLabels(ctx, repoID, mrID, []Label{{
		Name:        "new-name",
		Description: "stale name-only label",
		Color:       "222222",
		UpdatedAt:   now,
	}}))
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		Name:        "new-name",
		Description: "stale name-only label",
		Color:       "222222",
		UpdatedAt:   now,
	}}))

	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  200,
		Name:        "new-name",
		Description: "renamed label",
		Color:       "333333",
		IsDefault:   true,
		UpdatedAt:   now.Add(time.Minute),
	}}))

	var count int
	err := d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_labels WHERE repo_id = ?`,
		repoID,
	).Scan(&count)
	require.NoError(err)
	require.Equal(1, count)

	var labelID int64
	var platformID int64
	var name, description, color string
	var isDefault bool
	err = d.ReadDB().QueryRowContext(ctx, `
		SELECT id, platform_id, name, description, color, is_default
		FROM forge_labels
		WHERE repo_id = ?`, repoID,
	).Scan(&labelID, &platformID, &name, &description, &color, &isDefault)
	require.NoError(err)
	require.Equal(int64(200), platformID)
	require.Equal("new-name", name)
	require.Equal("renamed label", description)
	require.Equal("333333", color)
	require.True(isDefault)

	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 17)
	require.NoError(err)
	require.NotNil(mr)
	require.Len(mr.Labels, 1)
	require.Equal(labelID, mr.Labels[0].ID)
	require.Equal("new-name", mr.Labels[0].Name)

	issue, err := d.GetIssue(ctx, "github", "github.com", "acme", "widget", 23)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	require.Equal(labelID, issue.Labels[0].ID)
	require.Equal("new-name", issue.Labels[0].Name)
}

func TestUpsertLabels_RejectsAmbiguousNameAndPlatformIDMatch(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  100,
		Name:        "bug",
		Description: "by name",
		Color:       "111111",
		UpdatedAt:   now,
	}}))
	require.NoError(d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  200,
		Name:        "renamed",
		Description: "by platform",
		Color:       "222222",
		UpdatedAt:   now,
	}}))

	err := d.UpsertLabels(ctx, repoID, []Label{{
		PlatformID:  200,
		Name:        "bug",
		Description: "ambiguous",
		Color:       "333333",
		UpdatedAt:   now.Add(time.Minute),
	}})
	require.Error(err)
}

func TestKanbanState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	prID := insertTestMR(t, d, repoID, 1, "pr", baseTime())

	// Before EnsureKanbanState, GetKanbanState returns nil.
	k, err := d.GetKanbanState(ctx, prID)
	require.NoError(err)
	assert.Nil(k)

	// EnsureKanbanState creates "new".
	require.NoError(d.EnsureKanbanState(ctx, prID))
	k, err = d.GetKanbanState(ctx, prID)
	require.NoError(err)
	require.NotNil(k)
	assert.Equal("new", k.Status)

	// SetKanbanState changes the status.
	require.NoError(d.SetKanbanState(ctx, prID, "reviewing"))
	k, _ = d.GetKanbanState(ctx, prID)
	require.NotNil(k)
	assert.Equal("reviewing", k.Status)

	// EnsureKanbanState does NOT overwrite an existing row.
	require.NoError(d.EnsureKanbanState(ctx, prID))
	k, _ = d.GetKanbanState(ctx, prID)
	require.NotNil(k)
	assert.Equal("reviewing", k.Status)
}

func TestPREvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	prID := insertTestMR(t, d, repoID, 1, "pr", baseTime())
	base := baseTime()

	events := []MREvent{
		{
			MergeRequestID: prID,
			EventType:      "comment",
			Author:         "alice",
			Summary:        "LGTM",
			CreatedAt:      base,
			DedupeKey:      "comment-1",
		},
		{
			MergeRequestID: prID,
			EventType:      "review",
			Author:         "bob",
			Summary:        "approved",
			CreatedAt:      base.Add(time.Hour),
			DedupeKey:      "review-1",
		},
	}

	require.NoError(d.UpsertMREvents(ctx, events))

	got, err := d.ListMREvents(ctx, prID)
	require.NoError(err)
	require.Len(got, 2)
	// Newest first.
	assert.Equal("review-1", got[0].DedupeKey)
	assert.Equal("comment-1", got[1].DedupeKey)

	// Inserting duplicate dedupe_key must be silently ignored.
	dup := []MREvent{
		{
			MergeRequestID: prID,
			EventType:      "comment",
			Author:         "alice",
			Summary:        "dupe",
			CreatedAt:      base,
			DedupeKey:      "comment-1",
		},
	}
	require.NoError(d.UpsertMREvents(ctx, dup))
	got2, _ := d.ListMREvents(ctx, prID)
	assert.Len(got2, 2)
}

func TestReplaceCommentEventsRollsBackWhenDerivedUpdateFails(t *testing.T) {
	t.Run("merge request", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		database := openTestDB(t)
		repoID := insertTestRepo(t, database, "o", "r")
		mrID := insertTestMR(t, database, repoID, 1, "pr", baseTime())
		require.NoError(database.UpsertMREvents(t.Context(), []MREvent{{MergeRequestID: mrID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "old"}}))
		require.NoError(database.UpdateMRDerivedFields(t.Context(), repoID, 1, MRDerivedFields{CommentCount: 1}))
		_, err := database.WriteDB().ExecContext(t.Context(), `CREATE TRIGGER reject_mr_comment_count BEFORE UPDATE OF comment_count ON forge_merge_requests BEGIN SELECT RAISE(ABORT, 'reject count'); END`)
		require.NoError(err)

		err = database.ReplaceMRCommentEvents(t.Context(), mrID, []MREvent{{MergeRequestID: mrID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "new"}})
		require.Error(err)
		events, err := database.ListMREvents(t.Context(), mrID)
		require.NoError(err)
		require.Len(events, 1)
		assert.Equal("old", events[0].DedupeKey)
		mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(mr)
		assert.Equal(1, mr.CommentCount)
	})

	t.Run("issue", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		database := openTestDB(t)
		repoID := insertTestRepo(t, database, "o", "r")
		issueID := insertTestIssue(t, database, repoID, 1, "issue", baseTime())
		require.NoError(database.UpsertIssueEvents(t.Context(), []IssueEvent{{IssueID: issueID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "old"}}))
		require.NoError(database.UpdateIssueDerivedFields(t.Context(), repoID, 1, IssueDerivedFields{CommentCount: 1, LastActivityAt: baseTime()}))
		_, err := database.WriteDB().ExecContext(t.Context(), `CREATE TRIGGER reject_issue_comment_count BEFORE UPDATE OF comment_count ON forge_issues BEGIN SELECT RAISE(ABORT, 'reject count'); END`)
		require.NoError(err)

		err = database.ReplaceIssueCommentEvents(t.Context(), issueID, []IssueEvent{{IssueID: issueID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "new"}}, nil)
		require.Error(err)
		events, err := database.ListIssueEvents(t.Context(), issueID)
		require.NoError(err)
		require.Len(events, 1)
		assert.Equal("old", events[0].DedupeKey)
		issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(issue)
		assert.Equal(1, issue.CommentCount)
	})
}

func TestReplaceCommentEventsCountsPersistedUniqueRows(t *testing.T) {
	t.Run("merge request", func(t *testing.T) {
		require := require.New(t)
		database := openTestDB(t)
		repoID := insertTestRepo(t, database, "o", "r")
		mrID := insertTestMR(t, database, repoID, 1, "pr", baseTime())
		lastActivityAt := baseTime()
		require.NoError(database.UpdateMRDerivedFields(t.Context(), repoID, 1, MRDerivedFields{ReviewDecision: "approved"}))
		events := []MREvent{
			{MergeRequestID: mrID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "same", Body: "old"},
			{MergeRequestID: mrID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "same", Body: "new"},
		}

		require.NoError(database.ReplaceMRCommentEvents(t.Context(), mrID, events))
		require.NoError(database.UpdateMRReviewActivity(t.Context(), mrID, "changes_requested"))
		stored, err := database.ListMREvents(t.Context(), mrID)
		require.NoError(err)
		require.Len(stored, 1)
		require.Equal("new", stored[0].Body)
		mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(mr)
		require.Equal(1, mr.CommentCount)
		require.Equal("changes_requested", mr.ReviewDecision)
		require.Equal(lastActivityAt, mr.LastActivityAt)
	})

	t.Run("issue", func(t *testing.T) {
		require := require.New(t)
		database := openTestDB(t)
		repoID := insertTestRepo(t, database, "o", "r")
		issueID := insertTestIssue(t, database, repoID, 1, "issue", baseTime())
		lastActivityAt := baseTime().Add(time.Hour)
		require.NoError(database.UpdateIssueDerivedFields(t.Context(), repoID, 1, IssueDerivedFields{LastActivityAt: lastActivityAt}))
		events := []IssueEvent{
			{IssueID: issueID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "same", Body: "old"},
			{IssueID: issueID, EventType: "issue_comment", CreatedAt: baseTime(), DedupeKey: "same", Body: "new"},
		}

		require.NoError(database.ReplaceIssueCommentEvents(t.Context(), issueID, events, nil))
		newActivityAt := lastActivityAt.Add(time.Hour)
		require.NoError(database.UpdateIssueActivity(t.Context(), issueID, newActivityAt))
		stored, err := database.ListIssueEvents(t.Context(), issueID)
		require.NoError(err)
		require.Len(stored, 1)
		require.Equal("new", stored[0].Body)
		issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 1)
		require.NoError(err)
		require.NotNil(issue)
		require.Equal(1, issue.CommentCount)
		require.Equal(newActivityAt, issue.LastActivityAt)
	})
}

func TestMREventsDedupeIsScopedToMergeRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "o", "r")
	firstMRID := insertTestMR(t, d, repoID, 1, "pr one", base)
	secondMRID := insertTestMR(t, d, repoID, 2, "pr two", base.Add(time.Minute))

	sharedDedupeKey := "force-push-before-sha-after-sha"
	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{
			MergeRequestID: firstMRID,
			EventType:      "force_push",
			Author:         "alice",
			CreatedAt:      base,
			DedupeKey:      sharedDedupeKey,
		},
		{
			MergeRequestID: secondMRID,
			EventType:      "force_push",
			Author:         "bob",
			CreatedAt:      base.Add(time.Minute),
			DedupeKey:      sharedDedupeKey,
		},
	}))

	firstEvents, err := d.ListMREvents(ctx, firstMRID)
	require.NoError(err)
	require.Len(firstEvents, 1)
	assert.Equal(sharedDedupeKey, firstEvents[0].DedupeKey)

	secondEvents, err := d.ListMREvents(ctx, secondMRID)
	require.NoError(err)
	require.Len(secondEvents, 1)
	assert.Equal(sharedDedupeKey, secondEvents[0].DedupeKey)

	var total int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_mr_events WHERE dedupe_key = ?`,
		sharedDedupeKey,
	).Scan(&total)
	require.NoError(err)
	assert.Equal(2, total)
}

func TestMREventsPersistPlatformExternalID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "o", "r")
	mrID := insertTestMR(t, d, repoID, 1, "pr one", base)
	platformID := int64(5001)
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID:     mrID,
		PlatformID:         &platformID,
		PlatformExternalID: "gid://gitlab/Note/5001",
		EventType:          "issue_comment",
		Author:             "alice",
		CreatedAt:          base,
		DedupeKey:          "gitlab:gitlab.example.com:o/r:mr:1:note:5001",
	}}))

	got, err := d.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal("gid://gitlab/Note/5001", got[0].PlatformExternalID)
}

func TestListMREventsHandlesNonUTCTimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	prID := insertTestMR(t, d, repoID, 1, "pr one", baseTime())

	// Insert events with times in various non-UTC zones,
	// reproducing the formats the sqlite driver stores.
	//nolint:forbidigo // Test fixtures intentionally use non-UTC zones to verify normalization.
	edt := time.FixedZone("EDT", -4*3600)
	//nolint:forbidigo // Test fixtures intentionally use non-UTC zones to verify normalization.
	cdt := time.FixedZone("CDT", -5*3600)
	//nolint:forbidigo // Test fixtures intentionally use non-UTC zones to verify normalization.
	jst := time.FixedZone("JST", 9*3600)
	zones := []struct {
		key  string
		zone *time.Location
	}{
		{"commit-utc", time.UTC},
		{"commit-edt", edt},
		{"commit-cdt", cdt},
		{"commit-jst", jst},
	}
	var events []MREvent
	base := baseTime()
	for i, z := range zones {
		events = append(events, MREvent{
			MergeRequestID: prID,
			EventType:      "commit",
			Author:         "alice",
			CreatedAt:      base.Add(time.Duration(i) * time.Hour).In(z.zone),
			DedupeKey:      z.key,
		})
	}
	require.NoError(d.UpsertMREvents(ctx, events))

	got, err := d.ListMREvents(ctx, prID)
	require.NoError(err)
	require.Len(got, len(zones))

	for _, e := range got {
		assert.Equal(time.UTC, e.CreatedAt.Location(),
			"event %s should be returned in UTC", e.DedupeKey)
	}
}

func TestGetDiffSHAsByRepoIDScopesDuplicateProviderRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	githubID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "github",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)
	gitlabID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)
	now := time.Now().UTC()
	_, err = d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:          githubID,
		PlatformID:      1001,
		Number:          7,
		Title:           "github",
		State:           "merged",
		PlatformHeadSHA: "github-head",
		PlatformBaseSHA: "github-base",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	_, err = d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:          gitlabID,
		PlatformID:      2001,
		Number:          7,
		Title:           "gitlab",
		State:           "merged",
		PlatformHeadSHA: "gitlab-head",
		PlatformBaseSHA: "gitlab-base",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	require.NoError(d.UpdateDiffSHAs(
		ctx, githubID, 7,
		"github-diff-head", "github-diff-base", "github-merge-base",
	))

	gitlabSHAs, err := d.GetDiffSHAsByRepoID(ctx, gitlabID, 7)
	require.NoError(err)
	require.NotNil(gitlabSHAs)
	assert.Equal("gitlab-head", gitlabSHAs.PlatformHeadSHA)
	assert.Empty(gitlabSHAs.DiffHeadSHA)

	githubSHAs, err := d.GetDiffSHAsByRepoID(ctx, githubID, 7)
	require.NoError(err)
	require.NotNil(githubSHAs)
	assert.Equal("github-head", githubSHAs.PlatformHeadSHA)
	assert.Equal("github-diff-head", githubSHAs.DiffHeadSHA)
}

func TestUpdateMRCIStatusForHeadSkipsStaleHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "o", "r")
	now := baseTime()
	_, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:          repoID,
		PlatformID:      1001,
		Number:          7,
		Title:           "ci guarded",
		State:           "open",
		PlatformHeadSHA: "new-head",
		CIStatus:        "pending",
		CIChecksJSON:    `[{"name":"old","status":"in_progress"}]`,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)

	require.NoError(d.UpdateMRCIStatusForHead(ctx, repoID, 7, "old-head", "success", `[]`, false))
	stale, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stale)
	assert.Equal("pending", stale.CIStatus)

	require.NoError(d.UpdateMRCIStatusForHead(ctx, repoID, 7, "new-head", "pending", `[{"name":"build","status":"in_progress"}]`, true))
	fresh, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(fresh)
	assert.Equal("pending", fresh.CIStatus)
	assert.True(fresh.CIHadPending)

	require.NoError(d.UpdateMRCIStatusForHead(ctx, repoID, 7, "new-head", "success", `[]`, false))
	done, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(done)
	assert.Equal("success", done.CIStatus)
	assert.True(done.CIHadPending)
}

func TestGetPreviouslyOpenPRNumbers(t *testing.T) {
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "o", "r")
	base := baseTime()
	insertTestMR(t, d, repoID, 1, "pr1", base)
	insertTestMR(t, d, repoID, 2, "pr2", base.Add(time.Hour))
	insertTestMR(t, d, repoID, 3, "pr3", base.Add(2*time.Hour))

	// PRs 1 and 3 are still open; 2 was closed externally.
	stillOpen := map[int]bool{1: true, 3: true}
	closed, err := d.GetPreviouslyOpenMRNumbers(t.Context(), repoID, stillOpen)
	require.NoError(t, err)
	assert.Equal(t, []int{2}, closed)
}

func TestUpsertPullRequestMergeableState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "acme", "widget")
	now := baseTime()
	pr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     9001,
		Number:         42,
		State:          "open",
		MergeableState: "dirty",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}

	_, err := d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)

	got, err := d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("dirty", got.MergeableState)

	pr.MergeableState = "clean"
	_, err = d.UpsertMergeRequest(ctx, pr)
	require.NoError(err)

	got, err = d.GetMergeRequest(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	assert.Equal("clean", got.MergeableState)
}

func TestRateLimitCRUD(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	host := "github.com"
	hourStart := baseTime()
	resetAt := hourStart.Add(30 * time.Minute)

	// Insert REST
	require.NoError(d.UpsertRateLimit(host, "user:1", "rest", 5, hourStart, 4995, -1, &resetAt))

	got, err := d.GetRateLimit(host, "user:1", "rest")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(host, got.PlatformHost)
	assert.Equal("rest", got.APIType)
	assert.Equal(5, got.RequestsHour)
	assert.True(got.HourStart.Equal(hourStart))
	assert.Equal(4995, got.RateRemaining)
	require.NotNil(got.RateResetAt)
	assert.True(got.RateResetAt.Equal(resetAt))

	// Insert GraphQL for same host — separate row
	require.NoError(d.UpsertRateLimit(host, "user:1", "graphql", 2, hourStart, 4998, 5000, nil))

	gql, err := d.GetRateLimit(host, "user:1", "graphql")
	require.NoError(err)
	require.NotNil(gql)
	assert.Equal("graphql", gql.APIType)
	assert.Equal(2, gql.RequestsHour)
	assert.Equal(4998, gql.RateRemaining)

	// REST row unchanged
	rest, err := d.GetRateLimit(host, "user:1", "rest")
	require.NoError(err)
	require.NotNil(rest)
	assert.Equal(5, rest.RequestsHour)

	// Update via upsert
	laterStart := hourStart.Add(time.Hour)
	require.NoError(d.UpsertRateLimit(host, "user:1", "rest", 10, laterStart, 4990, -1, nil))

	got2, err := d.GetRateLimit(host, "user:1", "rest")
	require.NoError(err)
	require.NotNil(got2)
	assert.Equal(10, got2.RequestsHour)
	assert.True(got2.HourStart.Equal(laterStart))
	assert.Equal(4990, got2.RateRemaining)
	assert.Nil(got2.RateResetAt)

	// Not found
	missing, err := d.GetRateLimit("no.such.host", "user:1", "rest")
	require.NoError(err)
	assert.Nil(missing)
}

func TestRateLimitCRUDScopesByPlatform(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)

	host := "gitlab.example.com"
	hourStart := baseTime()

	require.NoError(d.UpsertPlatformRateLimit("github", host, "user:1", "rest", 1, hourStart, 4999, 5000, nil))
	require.NoError(d.UpsertPlatformRateLimit("github", host, "user:1", "graphql", 2, hourStart, 4998, 5000, nil))
	require.NoError(d.UpsertPlatformRateLimit("gitlab", host, "host", "rest", 3, hourStart, 599, 600, nil))

	ghRest, err := d.GetPlatformRateLimit("github", host, "user:1", "rest")
	require.NoError(err)
	require.NotNil(ghRest)
	assert.Equal("github", ghRest.Platform)
	assert.Equal("rest", ghRest.APIType)
	assert.Equal(1, ghRest.RequestsHour)

	ghGraphQL, err := d.GetPlatformRateLimit("github", host, "user:1", "graphql")
	require.NoError(err)
	require.NotNil(ghGraphQL)
	assert.Equal("github", ghGraphQL.Platform)
	assert.Equal("graphql", ghGraphQL.APIType)
	assert.Equal(2, ghGraphQL.RequestsHour)

	glRest, err := d.GetPlatformRateLimit("gitlab", host, "host", "rest")
	require.NoError(err)
	require.NotNil(glRest)
	assert.Equal("gitlab", glRest.Platform)
	assert.Equal("rest", glRest.APIType)
	assert.Equal(3, glRest.RequestsHour)

	require.NoError(d.UpsertPlatformRateLimit("gitlab", host, "host", "rest", 7, hourStart.Add(time.Hour), 593, 600, nil))
	ghRest, err = d.GetPlatformRateLimit("github", host, "user:1", "rest")
	require.NoError(err)
	require.NotNil(ghRest)
	assert.Equal(1, ghRest.RequestsHour)
	glRest, err = d.GetPlatformRateLimit("gitlab", host, "host", "rest")
	require.NoError(err)
	require.NotNil(glRest)
	assert.Equal(7, glRest.RequestsHour)
}

func TestUpdatePRState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	insertTestMR(t, d, repoID, 1, "pr", baseTime())

	mergedAt := baseTime().Add(time.Hour)
	require.NoError(d.UpdateMRState(ctx, repoID, 1, "merged", &mergedAt, nil))

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "o", "r", 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(MergeRequestStateMerged, pr.State)
	require.NotNil(pr.MergedAt)
	assert.True(pr.MergedAt.Equal(mergedAt))
}

func TestUpdateMRDraftStateUsesProviderTimestampToRejectStaleSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	base := time.Now().UTC().Add(time.Hour)
	insertTestMR(t, d, repoID, 1, "current pr", base)

	providerUpdatedAt := base.Add(time.Minute)
	require.NoError(d.UpdateMRDraftState(ctx, repoID, 1, true, providerUpdatedAt))

	staleSync := testMR(repoID, 1, withMRTitle("stale sync"), withMRActivity(base))
	staleSync.IsDraft = false
	_, err := d.UpsertMergeRequest(ctx, staleSync)
	require.NoError(err)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "o", "r", 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.True(pr.IsDraft)
	assert.Equal("current pr", pr.Title)
	assert.Equal(providerUpdatedAt, pr.UpdatedAt)
	assert.Equal(providerUpdatedAt, pr.LastActivityAt)
}

func TestUpdateMRDraftStateReturnsErrorWhenMissing(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	err := d.UpdateMRDraftState(ctx, repoID, 404, true, time.Now().UTC())
	require.Error(err)
}

func TestListIssues_AttachesLabels(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "acme", "widget"))
	require.NoError(err)

	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     201,
		Number:         3,
		URL:            "https://github.com/acme/widget/issues/3",
		Title:          "Bug",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID:  11,
		Name:        "bug",
		Description: "Something is broken",
		Color:       "d73a4a",
		IsDefault:   true,
		UpdatedAt:   now,
	}}))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{})
	require.NoError(err)
	require.Len(issues, 1)
	require.Len(issues[0].Labels, 1)
	require.Equal("bug", issues[0].Labels[0].Name)
	require.Equal("Something is broken", issues[0].Labels[0].Description)
	require.Equal("d73a4a", issues[0].Labels[0].Color)
	require.True(issues[0].Labels[0].IsDefault)
	require.True(issues[0].Labels[0].UpdatedAt.Equal(now))
}

func TestGetIssue_AttachesLabels(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     202,
		Number:         4,
		URL:            "https://github.com/acme/widget/issues/4",
		Title:          "Docs",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID:  12,
		Name:        "documentation",
		Description: "Docs updates",
		Color:       "0075ca",
		IsDefault:   false,
		UpdatedAt:   now,
	}}))

	issue, err := d.GetIssue(ctx, "github", "github.com", "acme", "widget", 4)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	require.Equal("documentation", issue.Labels[0].Name)
	require.Equal("Docs updates", issue.Labels[0].Description)
	require.Equal("0075ca", issue.Labels[0].Color)
	require.False(issue.Labels[0].IsDefault)
	require.True(issue.Labels[0].UpdatedAt.Equal(now))
}

func TestIssueRepoScopedQueriesCanonicalizeOwnerName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	issueID := insertTestIssue(t, d, repoID, 7, "mixed case issue", baseTime())

	got, err := d.GetIssue(ctx, "github", "github.com", "Owner", "Repo", 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(issueID, got.ID)

	filtered, err := d.ListIssues(ctx, ListIssuesOpts{
		RepoOwner: "Owner",
		RepoName:  "Repo",
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(issueID, filtered[0].ID)

}

func TestListIssuesFilterByHostedRepoPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	nestedRepo := insertTestRepoWithHost(
		t, d, "Group/SubGroup", "Project.Special", "ghe.example.com",
	)
	otherRepo := insertTestRepoWithHost(
		t, d, "Other", "Project.Special", "ghe.example.com",
	)
	insertTestIssue(t, d, nestedRepo, 1, "nested issue", base)
	insertTestIssue(t, d, otherRepo, 2, "other issue", base.Add(time.Hour))

	filtered, err := d.ListIssues(ctx, ListIssuesOpts{
		PlatformHost: "GHE.EXAMPLE.COM",
		RepoPath:     "Group/SubGroup/Project.Special",
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(nestedRepo, filtered[0].RepoID)
}

func TestListIssuesFilterByMultipleRepos(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	firstRepo := insertTestRepoWithHost(t, d, "owner", "repo1", "github.com")
	secondRepo := insertTestRepoWithHost(t, d, "team", "repo2", "ghe.example.com")
	thirdRepo := insertTestRepoWithHost(t, d, "owner", "repo3", "github.com")
	insertTestIssue(t, d, firstRepo, 1, "first", base)
	insertTestIssue(t, d, secondRepo, 2, "second", base.Add(time.Hour))
	insertTestIssue(t, d, thirdRepo, 3, "third", base.Add(2*time.Hour))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{
		RepoFilters: []RepoFilter{
			{PlatformHost: "github.com", RepoPath: "owner/repo1"},
			{PlatformHost: "ghe.example.com", RepoPath: "team/repo2"},
		},
	})
	require.NoError(err)
	require.Len(issues, 2)
	assert.Equal([]int64{secondRepo, firstRepo}, []int64{
		issues[0].RepoID,
		issues[1].RepoID,
	})
}

func TestListIssuesFilterBySearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestIssue(t, d, repoID, 12, "report a bug", base)
	insertTestIssue(t, d, repoID, 278, "filter broken", base.Add(time.Hour))
	insertTestIssue(t, d, repoID, 290, "another change", base.Add(2*time.Hour))

	issues, err := d.ListIssues(t.Context(), ListIssuesOpts{Search: "broken"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(278, issues[0].Number)

	issues, err = d.ListIssues(t.Context(), ListIssuesOpts{Search: "filter broken"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(278, issues[0].Number)

	issues, err = d.ListIssues(t.Context(), ListIssuesOpts{Search: "278"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(278, issues[0].Number)

	issues, err = d.ListIssues(t.Context(), ListIssuesOpts{Search: "#278"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(278, issues[0].Number)

	// Substring of number matches multiple.
	issues, err = d.ListIssues(t.Context(), ListIssuesOpts{Search: "2"})
	require.NoError(err)
	require.Len(issues, 3)
}

func TestListIssuesFilterBySearchRepoFragment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	apiRepoID := insertTestRepo(t, d, "acme", "api")
	workerRepoID := insertTestRepo(t, d, "tools", "worker")
	base := baseTime()

	insertTestIssue(t, d, apiRepoID, 1, "fix bug", base)
	insertTestIssue(t, d, workerRepoID, 2, "fix bug", base.Add(time.Hour))

	issues, err := d.ListIssues(t.Context(), ListIssuesOpts{Search: "acm"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(1, issues[0].Number)

	issues, err = d.ListIssues(t.Context(), ListIssuesOpts{Search: "work bug"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(2, issues[0].Number)
}

func TestListIssuesFilterBySearchLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()

	insertTestIssue(t, d, repoID, 12, "report a bug", base)
	issueID := insertTestIssue(t, d, repoID, 278, "filter broken", base.Add(time.Hour))
	require.NoError(d.ReplaceIssueLabels(ctx, repoID, issueID, []Label{{
		PlatformID: 300,
		Name:       "needs-triage",
		Color:      "d73a4a",
		UpdatedAt:  base,
	}}))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{Search: "needs-triage"})
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(278, issues[0].Number)
}

func TestListIssuesPaginationUsesStableTieBreaker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "owner", "repo")
	activity := baseTime()
	insertTestIssue(t, d, repoID, 1, "oldest id", activity)
	insertTestIssue(t, d, repoID, 2, "middle id", activity)
	insertTestIssue(t, d, repoID, 3, "newest id", activity)

	firstPage, err := d.ListIssues(t.Context(), ListIssuesOpts{Limit: 1})
	require.NoError(err)
	require.Len(firstPage, 1)
	assert.Equal(3, firstPage[0].Number)

	secondPage, err := d.ListIssues(t.Context(), ListIssuesOpts{Limit: 1, Offset: 1})
	require.NoError(err)
	require.Len(secondPage, 1)
	assert.Equal(2, secondPage[0].Number)
}

func TestListIssuesWorkspaceActivitySortsBeforePagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	insertTestIssue(t, d, repoID, 1, "provider newest", base.Add(2*time.Hour))
	insertTestIssue(t, d, repoID, 2, "workspace newest", base)

	first, err := d.ListIssues(t.Context(), ListIssuesOpts{
		Limit: 1,
		WorkspaceActivity: []ItemActivityOverride{{
			RepoID: repoID, ItemNumber: 2, ActivityAt: base.Add(3 * time.Hour),
		}},
	})
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal(2, first[0].Number)
	assert.Equal(base, first[0].LastActivityAt)
}

func TestListIssuesWorkspaceActivitySupportsLargeSubjectSets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "owner", "repo")
	base := baseTime()
	insertTestIssue(t, d, repoID, 1, "provider newest", base.Add(2*time.Hour))
	insertTestIssue(t, d, repoID, 2, "workspace newest", base)

	overrides := make([]ItemActivityOverride, 11_000)
	for i := range overrides {
		overrides[i] = ItemActivityOverride{
			RepoID: repoID, ItemNumber: i + 1, ActivityAt: base.Add(time.Hour),
		}
	}
	overrides[1].ActivityAt = base.Add(3 * time.Hour)

	got, err := d.ListIssues(t.Context(), ListIssuesOpts{
		Limit: 1, WorkspaceActivity: overrides,
	})
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal(2, got[0].Number)
}

func TestReplaceIssueLabels_RejectsWrongRepoID(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoA := insertTestRepo(t, d, "acme", "widget")
	repoB := insertTestRepo(t, d, "acme", "gadget")
	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoA,
		PlatformID:     204,
		Number:         6,
		URL:            "https://github.com/acme/widget/issues/6",
		Title:          "repo guarded issue",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	err = d.ReplaceIssueLabels(ctx, repoB, issueID, []Label{{
		PlatformID:  220,
		Name:        "wrong-repo",
		Description: "should fail",
		Color:       "ff0000",
		UpdatedAt:   now,
	}})
	require.Error(err)

	issue, err := d.GetIssue(ctx, "github", "github.com", "acme", "widget", 6)
	require.NoError(err)
	require.NotNil(issue)
	require.Empty(issue.Labels)
}

func TestListIssues_UsesRepoScopedLabels(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()

	repoA, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "acme", "widget"))
	require.NoError(err)
	repoB, err := d.UpsertRepo(ctx, verifiedTestRepoIdentity("github", "github.com", "acme", "gadget"))
	require.NoError(err)

	issueID, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoA,
		PlatformID:     203,
		Number:         5,
		URL:            "https://github.com/acme/widget/issues/5",
		Title:          "Repo scoped bug",
		Author:         "bob",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	require.NoError(d.ReplaceIssueLabels(ctx, repoA, issueID, []Label{{
		PlatformID:  21,
		Name:        "bug",
		Description: "Widget bug",
		Color:       "d73a4a",
		UpdatedAt:   now,
	}}))
	require.NoError(d.UpsertLabels(ctx, repoB, []Label{{
		PlatformID:  22,
		Name:        "bug",
		Description: "Gadget bug",
		Color:       "0e8a16",
		UpdatedAt:   now,
	}}))

	issues, err := d.ListIssues(ctx, ListIssuesOpts{})
	require.NoError(err)
	require.Len(issues, 1)
	require.Len(issues[0].Labels, 1)
	require.Equal("bug", issues[0].Labels[0].Name)
	require.Equal("d73a4a", issues[0].Labels[0].Color)
}

func TestSetWorktreeLinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	mrID1 := insertTestMR(t, d, repoID, 1, "pr1", baseTime())
	mrID2 := insertTestMR(t, d, repoID, 2, "pr2", baseTime().Add(time.Hour))

	now := baseTime()
	links := []WorktreeLink{
		{MergeRequestID: mrID1, WorktreeKey: "wt-1", WorktreePath: "/tmp/wt1", WorktreeBranch: "feature-1", LinkedAt: now},
		{MergeRequestID: mrID2, WorktreeKey: "wt-2", WorktreePath: "/tmp/wt2", WorktreeBranch: "feature-2", LinkedAt: now.Add(time.Hour)},
	}
	require.NoError(d.SetWorktreeLinks(ctx, links))

	all, err := d.GetAllWorktreeLinks(ctx)
	require.NoError(err)
	require.Len(all, 2)
	// Newest first.
	assert.Equal("wt-2", all[0].WorktreeKey)
	assert.Equal("wt-1", all[1].WorktreeKey)
	assert.Equal("/tmp/wt1", all[1].WorktreePath)
	assert.Equal("feature-1", all[1].WorktreeBranch)

	// Bulk replace with a different set.
	replacement := []WorktreeLink{
		{MergeRequestID: mrID1, WorktreeKey: "wt-3", WorktreePath: "/tmp/wt3", WorktreeBranch: "hotfix", LinkedAt: now},
	}
	require.NoError(d.SetWorktreeLinks(ctx, replacement))

	all2, err := d.GetAllWorktreeLinks(ctx)
	require.NoError(err)
	require.Len(all2, 1)
	assert.Equal("wt-3", all2[0].WorktreeKey)
}

func TestGetWorktreeLinksForMR(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	mrID1 := insertTestMR(t, d, repoID, 1, "pr1", baseTime())
	mrID2 := insertTestMR(t, d, repoID, 2, "pr2", baseTime().Add(time.Hour))

	now := baseTime()
	links := []WorktreeLink{
		{MergeRequestID: mrID1, WorktreeKey: "wt-a", LinkedAt: now},
		{MergeRequestID: mrID2, WorktreeKey: "wt-b", LinkedAt: now},
	}
	require.NoError(d.SetWorktreeLinks(ctx, links))

	forMR1, err := d.GetWorktreeLinksForMR(ctx, mrID1)
	require.NoError(err)
	require.Len(forMR1, 1)
	assert.Equal("wt-a", forMR1[0].WorktreeKey)

	// Empty when no links for a given MR.
	forMR999, err := d.GetWorktreeLinksForMR(ctx, 999)
	require.NoError(err)
	assert.Empty(forMR999)
}

func TestListCommentAutocompleteUsers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	mrID := insertTestMR(t, d, repoID, 12, "Polish mentions", base.Add(2*time.Hour))
	issueID := insertTestIssue(t, d, repoID, 7, "Mention bug", base.Add(time.Hour))

	_, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:         repoID,
		PlatformID:     9001,
		Number:         13,
		URL:            "https://github.com/acme/widget/pull/13",
		Title:          "Secondary author",
		Author:         "alex",
		State:          "open",
		HeadBranch:     "feature-13",
		BaseBranch:     "main",
		CreatedAt:      base.Add(3 * time.Hour),
		UpdatedAt:      base.Add(3 * time.Hour),
		LastActivityAt: base.Add(3 * time.Hour),
	})
	require.NoError(err)
	_, err = d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     9002,
		Number:         8,
		URL:            "https://github.com/acme/widget/issues/8",
		Title:          "Issue author",
		Author:         "alice",
		State:          "open",
		CreatedAt:      base.Add(4 * time.Hour),
		UpdatedAt:      base.Add(4 * time.Hour),
		LastActivityAt: base.Add(4 * time.Hour),
	})
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		EventType:      "comment",
		Author:         "albert",
		CreatedAt:      base.Add(5 * time.Hour),
		DedupeKey:      "mr-comment-1",
	}}))
	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{{
		IssueID:   issueID,
		EventType: "comment",
		Author:    "alice",
		CreatedAt: base.Add(6 * time.Hour),
		DedupeKey: "issue-comment-1",
	}}))

	users, err := d.ListCommentAutocompleteUsers(ctx, "github", "github.com", "acme", "widget", "al", nil, 10)
	require.NoError(err)
	assert.Equal([]string{"alice", "albert", "alex"}, users)

	users, err = d.ListCommentAutocompleteUsers(ctx, "github", "github.com", "acme", "widget", "bert", nil, 10)
	require.NoError(err)
	assert.Equal([]string{"albert"}, users)
}

func TestListCommentAutocompleteUsersRanksCurrentItemParticipantsFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	// The most recently active user in the repo is unrelated to the target
	// item; without an item hint they should lead the list.
	insertTestMRWithOptions(t, d, testMR(repoID, 30, withMRAuthor("zed"), withMRActivity(base.Add(10*time.Hour))))
	mrID := insertTestMRWithOptions(t, d, testMR(repoID, 12, withMRAuthor("alice"), withMRActivity(base.Add(2*time.Hour))))
	require.NoError(d.UpdateMergeRequestAssignees(ctx, repoID, mrID, []string{"bob"}))
	require.NoError(d.UpdateMergeRequestReviewers(ctx, repoID, mrID, []string{"carol"}))
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: mrID,
		EventType:      "comment",
		Author:         "dave",
		CreatedAt:      base.Add(time.Hour),
		DedupeKey:      "mr-12-comment-1",
	}}))
	issueID := insertTestIssueWithOptions(t, d, testIssue(repoID, 7, withIssueAuthor("erin"), withIssueActivity(base.Add(3*time.Hour))))
	require.NoError(d.UpdateIssueAssignees(ctx, repoID, issueID, []string{"frank"}))

	users, err := d.ListCommentAutocompleteUsers(ctx, "github", "github.com", "acme", "widget", "", nil, 10)
	require.NoError(err)
	assert.Equal("zed", users[0], "without an item hint recency wins")

	users, err = d.ListCommentAutocompleteUsers(
		ctx, "github", "github.com", "acme", "widget", "",
		&CommentAutocompleteItem{Kind: "pull", Number: 12}, 10,
	)
	require.NoError(err)
	assert.Equal(
		[]string{"alice", "bob", "carol", "dave", "zed", "erin"}, users,
		"author, assignee, reviewer, and commenter lead; the rest keep recency order",
	)

	users, err = d.ListCommentAutocompleteUsers(
		ctx, "github", "github.com", "acme", "widget", "",
		&CommentAutocompleteItem{Kind: "issue", Number: 7}, 10,
	)
	require.NoError(err)
	assert.Equal([]string{"erin", "frank", "zed", "alice", "dave"}, users, "issue author and assignee lead")

	// A participant that does not match the query is still filtered out.
	users, err = d.ListCommentAutocompleteUsers(
		ctx, "github", "github.com", "acme", "widget", "z",
		&CommentAutocompleteItem{Kind: "pull", Number: 12}, 10,
	)
	require.NoError(err)
	assert.Equal([]string{"zed"}, users)
}

func TestListCommentAutocompleteUsersScopesByProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	githubRepoID := insertTestRepo(t, d, "acme", "widget")
	giteaRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitea",
		PlatformHost:   "github.com",
		PlatformRepoID: "gitea-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	insertTestIssueWithOptions(t, d, testIssue(
		githubRepoID,
		1,
		withIssueTitle("GitHub collision issue"),
		withIssueAuthor("alice"),
		withIssueActivity(base.Add(time.Hour)),
	))
	insertTestIssueWithOptions(t, d, testIssue(
		giteaRepoID,
		901,
		withIssueTitle("Gitea collision issue"),
		withIssueAuthor("gina"),
		withIssueActivity(base.Add(2*time.Hour)),
	))

	users, err := d.ListCommentAutocompleteUsers(ctx, "gitea", "github.com", "acme", "widget", "", nil, 10)
	require.NoError(err)
	assert.Equal([]string{"gina"}, users)

	users, err = d.ListCommentAutocompleteUsers(ctx, "github", "github.com", "acme", "widget", "", nil, 10)
	require.NoError(err)
	assert.Equal([]string{"alice"}, users)
}

func TestListCommentAutocompleteUsersHidesOnlyRemovedUpstreamItems(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "acme", "widget")
	inaccessibleMRID := insertTestMR(t, d, repoID, 1, "inaccessible pull", now)
	removedMRID := insertTestMR(t, d, repoID, 2, "removed pull", now)
	inaccessibleIssueID := insertTestIssue(t, d, repoID, 3, "inaccessible issue", now)
	removedIssueID := insertTestIssue(t, d, repoID, 4, "removed issue", now)
	_, err := d.WriteDB().ExecContext(ctx, `
		UPDATE forge_merge_requests SET author = CASE number
			WHEN 1 THEN 'visible-pull-author' ELSE 'removed-pull-author' END;
		UPDATE forge_issues SET author = CASE number
			WHEN 3 THEN 'visible-issue-author' ELSE 'removed-issue-author' END;
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 1, 'pr-1', ?, ?, 'inaccessible'),
			(?, 'merge_request', 2, 'pr-2', ?, ?, 'removed_upstream'),
			(?, 'issue', 3, 'issue-3', ?, ?, 'inaccessible'),
			(?, 'issue', 4, 'issue-4', ?, ?, 'removed_upstream')`,
		repoID, now, now, repoID, now, now,
		repoID, now, now, repoID, now, now,
	)
	require.NoError(err)
	require.NoError(d.UpsertMREvents(ctx, []MREvent{
		{MergeRequestID: inaccessibleMRID, EventType: "comment", Author: "visible-pull-event", CreatedAt: now, DedupeKey: "visible-pull-event"},
		{MergeRequestID: removedMRID, EventType: "comment", Author: "removed-pull-event", CreatedAt: now, DedupeKey: "removed-pull-event"},
	}))
	require.NoError(d.UpsertIssueEvents(ctx, []IssueEvent{
		{IssueID: inaccessibleIssueID, EventType: "comment", Author: "visible-issue-event", CreatedAt: now, DedupeKey: "visible-issue-event"},
		{IssueID: removedIssueID, EventType: "comment", Author: "removed-issue-event", CreatedAt: now, DedupeKey: "removed-issue-event"},
	}))

	users, err := d.ListCommentAutocompleteUsers(
		ctx, "github", "github.com", "acme", "widget", "", nil, 20,
	)
	require.NoError(err)
	require.ElementsMatch([]string{
		"visible-pull-author", "visible-pull-event",
		"visible-issue-author", "visible-issue-event",
	}, users)
}

func TestListCommentAutocompleteReferences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "acme", "widget")
	insertTestMR(t, d, repoID, 12, "Polish mentions", base.Add(3*time.Hour))
	insertTestMR(t, d, repoID, 3, "Add docs", base)
	insertTestIssue(t, d, repoID, 17, "Mention bug", base.Add(2*time.Hour))
	insertTestIssue(t, d, repoID, 101, "Numbered item", base.Add(time.Hour))

	refs, err := d.ListCommentAutocompleteReferences(ctx, "github", "github.com", "acme", "widget", "1", "", 10)
	require.NoError(err)
	require.Len(refs, 3)
	assert.Equal(CommentAutocompleteReference{Kind: "pull", Number: 12, Title: "Polish mentions", State: "open"}, refs[0])
	assert.Equal(CommentAutocompleteReference{Kind: "issue", Number: 17, Title: "Mention bug", State: "open"}, refs[1])
	assert.Equal(CommentAutocompleteReference{Kind: "issue", Number: 101, Title: "Numbered item", State: "open"}, refs[2])

	refs, err = d.ListCommentAutocompleteReferences(ctx, "github", "github.com", "acme", "widget", "doc", "", 10)
	require.NoError(err)
	require.Len(refs, 1)
	assert.Equal(CommentAutocompleteReference{Kind: "pull", Number: 3, Title: "Add docs", State: "open"}, refs[0])

	refs, err = d.ListCommentAutocompleteReferences(ctx, "github", "github.com", "acme", "widget", "1", "issue", 10)
	require.NoError(err)
	assert.Equal([]CommentAutocompleteReference{
		{Kind: "issue", Number: 17, Title: "Mention bug", State: "open"},
		{Kind: "issue", Number: 101, Title: "Numbered item", State: "open"},
	}, refs)

	refs, err = d.ListCommentAutocompleteReferences(ctx, "github", "github.com", "acme", "widget", "1", "pull", 10)
	require.NoError(err)
	assert.Equal([]CommentAutocompleteReference{
		{Kind: "pull", Number: 12, Title: "Polish mentions", State: "open"},
	}, refs)
}

func TestListCommentAutocompleteReferencesHidesOnlyRemovedUpstreamItems(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "acme", "widget")
	insertTestMR(t, d, repoID, 1, "inaccessible pull", now)
	insertTestMR(t, d, repoID, 2, "removed pull", now)
	insertTestIssue(t, d, repoID, 3, "inaccessible issue", now)
	insertTestIssue(t, d, repoID, 4, "removed issue", now)
	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 1, 'pr-1', ?, ?, 'inaccessible'),
			(?, 'merge_request', 2, 'pr-2', ?, ?, 'removed_upstream'),
			(?, 'issue', 3, 'issue-3', ?, ?, 'inaccessible'),
			(?, 'issue', 4, 'issue-4', ?, ?, 'removed_upstream')`,
		repoID, now, now, repoID, now, now,
		repoID, now, now, repoID, now, now,
	)
	require.NoError(err)

	refs, err := d.ListCommentAutocompleteReferences(
		ctx, "github", "github.com", "acme", "widget", "", "", 10,
	)
	require.NoError(err)
	require.ElementsMatch([]CommentAutocompleteReference{
		{Kind: "pull", Number: 1, Title: "inaccessible pull", State: "open"},
		{Kind: "issue", Number: 3, Title: "inaccessible issue", State: "open"},
	}, refs)
}

func TestListCommentAutocompleteReferencesScopesByProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	githubRepoID := insertTestRepo(t, d, "acme", "widget")
	giteaRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitea",
		PlatformHost:   "github.com",
		PlatformRepoID: "gitea-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	insertTestIssueWithOptions(t, d, testIssue(
		githubRepoID,
		1,
		withIssueTitle("Provider collision issue"),
		withIssueActivity(base.Add(time.Hour)),
	))
	insertTestIssueWithOptions(t, d, testIssue(
		giteaRepoID,
		901,
		withIssueTitle("Provider collision issue"),
		withIssueActivity(base.Add(2*time.Hour)),
	))

	refs, err := d.ListCommentAutocompleteReferences(ctx, "gitea", "github.com", "acme", "widget", "collision", "", 10)
	require.NoError(err)
	assert.Equal([]CommentAutocompleteReference{
		{Kind: "issue", Number: 901, Title: "Provider collision issue", State: "open"},
	}, refs)

	refs, err = d.ListCommentAutocompleteReferences(ctx, "github", "github.com", "acme", "widget", "collision", "", 10)
	require.NoError(err)
	assert.Equal([]CommentAutocompleteReference{
		{Kind: "issue", Number: 1, Title: "Provider collision issue", State: "open"},
	}, refs)
}

func TestWorktreeLinksCascadeOnMRDelete(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "o", "r")
	mrID := insertTestMR(t, d, repoID, 1, "pr1", baseTime())

	links := []WorktreeLink{
		{MergeRequestID: mrID, WorktreeKey: "wt-del", LinkedAt: baseTime()},
	}
	require.NoError(d.SetWorktreeLinks(ctx, links))

	all, err := d.GetAllWorktreeLinks(ctx)
	require.NoError(err)
	require.Len(all, 1)

	// Delete the MR; the ON DELETE CASCADE should remove the link.
	_, err = d.WriteDB().ExecContext(ctx,
		`DELETE FROM forge_merge_requests WHERE id = ?`, mrID)
	require.NoError(err)

	after, err := d.GetAllWorktreeLinks(ctx)
	require.NoError(err)
	require.Empty(after)
}

// TestWorktreeAndPurgeRespectCanceledContext verifies a
// pre-canceled context aborts the database/sql call rather
// than running the query. Locks in the cancellation guarantee
// the ctx plumbing added for worktree-link and purge queries.
func TestWorktreeAndPurgeRespectCanceledContext(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	err := d.PurgeOtherHosts(canceled, "github.com")
	require.ErrorIs(err, context.Canceled)

	err = d.SetWorktreeLinks(canceled, nil)
	require.ErrorIs(err, context.Canceled)

	_, err = d.GetWorktreeLinksForMR(canceled, 1)
	require.ErrorIs(err, context.Canceled)

	_, err = d.GetWorktreeLinksForMRs(canceled, []int64{1, 2})
	require.ErrorIs(err, context.Canceled)

	_, err = d.GetAllWorktreeLinks(canceled)
	require.ErrorIs(err, context.Canceled)
}

func TestRepoIdentifierCasefoldTriggers(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_repos (platform, platform_host, owner, name)
		VALUES ('github', 'github.com', 'Acme', 'widget')`)
	require.Error(err)
	require.Contains(err.Error(), "repo identifiers must be provider-canonical")

	repoID := insertTestRepo(t, d, "acme", "widget")
	_, err = d.WriteDB().ExecContext(ctx, `
		UPDATE forge_repos SET name = 'Widget' WHERE id = ?`,
		repoID,
	)
	require.Error(err)
	require.Contains(err.Error(), "repo identifiers must be provider-canonical")
}

func TestWorkspaceCRUD(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	ws := &Workspace{
		ID:              "ws-abc-123",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-abc-123",
		TmuxSession:     "ws-abc-123",
		Status:          "creating",
	}

	// Insert
	require.NoError(d.InsertWorkspace(ctx, ws))

	// Get by ID
	got, err := d.GetWorkspace(ctx, "ws-abc-123")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ws-abc-123", got.ID)
	assert.Equal("github.com", got.PlatformHost)
	assert.Equal("acme", got.RepoOwner)
	assert.Equal("widget", got.RepoName)
	assert.Equal(WorkspaceItemTypePullRequest, got.ItemType)
	assert.Equal(42, got.ItemNumber)
	assert.Equal("feature/thing", got.GitHeadRef)
	assert.Nil(got.MRHeadRepo)
	assert.Equal("kenn-forge/pr-42", got.WorkspaceBranch)
	assert.Equal("/tmp/ws-abc-123", got.WorktreePath)
	assert.Equal("ws-abc-123", got.TmuxSession)
	assert.Equal("creating", got.Status)
	assert.Nil(got.ErrorMessage)
	assert.False(got.CreatedAt.IsZero())

	// Get by MR coordinates
	byMR, err := d.GetWorkspaceByMR(
		ctx, "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(byMR)
	assert.Equal("ws-abc-123", byMR.ID)
	assert.Equal("kenn-forge/pr-42", byMR.WorkspaceBranch)

	// GetWorkspaceByMR miss
	missMR, err := d.GetWorkspaceByMR(
		ctx, "github.com", "acme", "widget", 999,
	)
	require.NoError(err)
	assert.Nil(missMR)

	// List (ordered by created_at DESC).
	// Force ws2 to have a later created_at.
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, git_head_ref,
		     worktree_path, tmux_session, status,
		     created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        datetime('now', '+1 minute'))`,
		"ws-def-456", "github.com", "acme", "gadget",
		WorkspaceItemTypePullRequest, 7, "7", "fix/bug",
		"/tmp/ws-def-456", "ws-def-456", "ready",
	)
	require.NoError(err)

	list, err := d.ListWorkspaces(ctx)
	require.NoError(err)
	require.Len(list, 2)
	// Newest first.
	assert.Equal("ws-def-456", list[0].ID)
	assert.Equal("ws-abc-123", list[1].ID)

	// UpdateWorkspaceStatus
	errMsg := "clone failed"
	require.NoError(d.UpdateWorkspaceStatus(
		ctx, "ws-abc-123", "error", &errMsg,
	))
	updated, err := d.GetWorkspace(ctx, "ws-abc-123")
	require.NoError(err)
	require.NotNil(updated)
	assert.Equal("error", updated.Status)
	require.NotNil(updated.ErrorMessage)
	assert.Equal("clone failed", *updated.ErrorMessage)

	require.NoError(d.UpdateWorkspaceBranch(
		ctx, "ws-abc-123", "feature/thing",
	))
	updated, err = d.GetWorkspace(ctx, "ws-abc-123")
	require.NoError(err)
	require.NotNil(updated)
	assert.Equal("feature/thing", updated.WorkspaceBranch)

	require.NoError(d.InsertWorkspaceSetupEvent(
		ctx,
		&WorkspaceSetupEvent{
			WorkspaceID: "ws-abc-123",
			Stage:       "clone",
			Outcome:     "failure",
			Message:     "ensure clone: clone failed",
		},
	))
	events, err := d.ListWorkspaceSetupEvents(
		ctx, "ws-abc-123",
	)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("ws-abc-123", events[0].WorkspaceID)
	assert.Equal("clone", events[0].Stage)
	assert.Equal("failure", events[0].Outcome)
	assert.Equal("ensure clone: clone failed", events[0].Message)
	assert.False(events[0].CreatedAt.IsZero())

	require.NoError(d.UpsertWorkspaceRuntimeSession(
		ctx,
		&WorkspaceRuntimeSession{
			WorkspaceID: "ws-abc-123",
			SessionKey:  "ws-abc-123_codex",
			TargetKey:   "codex",
			Label:       "Codex",
			Kind:        "agent",
			Scope:       "session",
			TmuxSession: "kenn-forge-ws-abc-123-codex",
		},
	))
	require.NoError(d.UpsertWorkspaceRuntimeSession(
		ctx,
		&WorkspaceRuntimeSession{
			WorkspaceID: "ws-abc-123",
			SessionKey:  "ws-abc-123_claude",
			TargetKey:   "claude",
			Label:       "Claude",
			Kind:        "agent",
			Scope:       "session",
			TmuxSession: "kenn-forge-ws-abc-123-claude",
		},
	))
	tmuxSessions, err := d.ListWorkspaceRuntimeTmuxSessions(ctx, "ws-abc-123")
	require.NoError(err)
	require.Len(tmuxSessions, 2)
	assert.Equal("kenn-forge-ws-abc-123-codex", tmuxSessions[0].TmuxSession)
	assert.Equal("codex", tmuxSessions[0].TargetKey)
	assert.False(tmuxSessions[0].CreatedAt.IsZero())

	allTmuxSessions, err := d.ListAllWorkspaceRuntimeTmuxSessions(ctx)
	require.NoError(err)
	require.Len(allTmuxSessions, 2)

	require.NoError(d.DeleteWorkspaceRuntimeSession(
		ctx, "ws-abc-123", "ws-abc-123_claude",
	))
	tmuxSessions, err = d.ListWorkspaceRuntimeTmuxSessions(ctx, "ws-abc-123")
	require.NoError(err)
	require.Len(tmuxSessions, 1)
	assert.Equal("kenn-forge-ws-abc-123-codex", tmuxSessions[0].TmuxSession)

	require.NoError(d.DeleteWorkspaceRuntimeSessions(ctx, "ws-abc-123"))
	tmuxSessions, err = d.ListWorkspaceRuntimeTmuxSessions(ctx, "ws-abc-123")
	require.NoError(err)
	assert.Empty(tmuxSessions)

	// Clear error message
	require.NoError(d.UpdateWorkspaceStatus(
		ctx, "ws-abc-123", "ready", nil,
	))
	cleared, err := d.GetWorkspace(ctx, "ws-abc-123")
	require.NoError(err)
	assert.Equal("ready", cleared.Status)
	assert.Nil(cleared.ErrorMessage)

	// Delete
	require.NoError(d.DeleteWorkspace(ctx, "ws-abc-123"))
	gone, err := d.GetWorkspace(ctx, "ws-abc-123")
	require.NoError(err)
	assert.Nil(gone)

	// Get missing ID returns nil
	noSuch, err := d.GetWorkspace(ctx, "nonexistent")
	require.NoError(err)
	assert.Nil(noSuch)
}

func TestListWorkspacesUsesOneReadConnection(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	now := baseTime()

	identity := GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repo, accepted, err := d.ReconcileRepositoryObservation(t.Context(), identity, now)
	require.NoError(err)
	require.True(accepted)
	require.NoError(d.InsertWorkspace(t.Context(), &Workspace{
		ID: "ws-list-one-connection", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", RepoID: repo.Repository.ID,
		ItemType: WorkspaceItemTypePullRequest, ItemNumber: 42,
		WorktreePath: "/tmp/ws-list-one-connection", Status: "ready",
	}))

	renamed := GitHubRepoIdentity("github.com", "acme", "gadget")
	renamed.PlatformRepoID = identity.PlatformRepoID
	_, accepted, err = d.ReconcileRepositoryObservation(t.Context(), renamed, now.Add(time.Minute))
	require.NoError(err)
	require.True(accepted)

	d.ReadDB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	workspaces, err := d.ListWorkspaces(ctx)
	require.NoError(err)
	require.Len(workspaces, 1)
	require.Equal("gadget", workspaces[0].RepoName)
}

func TestWorkspaceDeletionLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	insert := func(id, status string) {
		require.NoError(d.InsertWorkspace(ctx, &Workspace{
			ID:              id,
			PlatformHost:    "github.com",
			RepoOwner:       "acme",
			RepoName:        id,
			ItemType:        WorkspaceItemTypePullRequest,
			ItemNumber:      42,
			GitHeadRef:      "feature/delete",
			WorkspaceBranch: "kenn-forge/pr-42",
			WorktreePath:    "/tmp/" + id,
			TmuxSession:     "kenn-forge-" + id,
			Status:          status,
		}))
	}

	insert("ws-delete", "ready")
	started, err := d.BeginWorkspaceDeletion(ctx, "ws-delete")
	require.NoError(err)
	assert.True(started)
	started, err = d.BeginWorkspaceDeletion(ctx, "ws-delete")
	require.NoError(err)
	assert.False(started)

	got, err := d.GetWorkspace(ctx, "ws-delete")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("deleting", got.Status)
	assert.Nil(got.ErrorMessage)

	require.NoError(d.FailWorkspaceDeletion(ctx, "ws-delete", "worktree is dirty"))
	got, err = d.GetWorkspace(ctx, "ws-delete")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("deletion_failed", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Equal("worktree is dirty", *got.ErrorMessage)

	started, err = d.BeginWorkspaceRetirement(ctx, "ws-delete")
	require.NoError(err)
	assert.False(started)
	got, err = d.GetWorkspace(ctx, "ws-delete")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("deletion_failed", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Equal("worktree is dirty", *got.ErrorMessage)

	started, err = d.BeginWorkspaceDeletion(ctx, "ws-delete")
	require.NoError(err)
	assert.True(started)

	insert("ws-interrupted", "deleting")
	require.NoError(d.FailInterruptedWorkspaceDeletions(
		ctx, "deletion interrupted by server restart",
	))
	for _, id := range []string{"ws-delete", "ws-interrupted"} {
		got, err = d.GetWorkspace(ctx, id)
		require.NoError(err)
		require.NotNil(got)
		assert.Equal("deletion_failed", got.Status)
		require.NotNil(got.ErrorMessage)
		assert.Equal("deletion interrupted by server restart", *got.ErrorMessage)
	}
}

func TestBeginWorkspaceRetirementPreservesFailureConcurrently(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	message := "worktree is dirty"
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-retirement-failure",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/delete",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-retirement-failure",
		TmuxSession:     "kenn-forge-ws-retirement-failure",
		Status:          "deletion_failed",
		ErrorMessage:    &message,
	}))

	const callers = 16
	start := make(chan struct{})
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			started, err := d.BeginWorkspaceRetirement(ctx, "ws-retirement-failure")
			results <- started
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(err)
	}
	for started := range results {
		require.False(started)
	}
	got, err := d.GetWorkspace(ctx, "ws-retirement-failure")
	require.NoError(err)
	require.NotNil(got)
	require.Equal("deletion_failed", got.Status)
	require.NotNil(got.ErrorMessage)
	require.Equal(message, *got.ErrorMessage)
}

func TestFailInterruptedWorkspaceSetups(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	insert := func(id, status string) {
		require.NoError(d.InsertWorkspace(ctx, &Workspace{
			ID:           id,
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     id,
			ItemType:     WorkspaceItemTypePullRequest,
			ItemNumber:   42,
			GitHeadRef:   "feature/setup",
			WorktreePath: "/tmp/" + id,
			TmuxSession:  "kenn-forge-" + id,
			Status:       status,
		}))
	}
	insert("ws-creating", "creating")
	insert("ws-ready", "ready")

	require.NoError(d.FailInterruptedWorkspaceSetups(
		ctx, "setup interrupted by server restart",
	))

	creating, err := d.GetWorkspace(ctx, "ws-creating")
	require.NoError(err)
	require.NotNil(creating)
	assert.Equal("error", creating.Status)
	require.NotNil(creating.ErrorMessage)
	assert.Equal("setup interrupted by server restart", *creating.ErrorMessage)

	ready, err := d.GetWorkspace(ctx, "ws-ready")
	require.NoError(err)
	require.NotNil(ready)
	assert.Equal("ready", ready.Status)
	assert.Nil(ready.ErrorMessage)
}

func TestReadyWorkspaceErrorDoesNotOverwriteAdmittedDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-prune-delete-race",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/delete",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-prune-delete-race",
		TmuxSession:     "kenn-forge-ws-prune-delete-race",
		Status:          "ready",
	}))

	// Simulate pruning reading a ready workspace before deletion wins the
	// durable transition. Its stale error write must not replace deleting.
	stale, err := d.GetWorkspace(ctx, "ws-prune-delete-race")
	require.NoError(err)
	require.Equal("ready", stale.Status)
	started, err := d.BeginWorkspaceDeletion(ctx, stale.ID)
	require.NoError(err)
	require.True(started)

	updated, err := d.MarkReadyWorkspaceError(
		ctx, stale.ID, "tmux session is no longer running",
	)
	require.NoError(err)
	assert.False(updated)

	got, err := d.GetWorkspace(ctx, stale.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("deleting", got.Status)
}

func TestFailWorkspaceDeletionRequiresDeletingState(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "ws-ready",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/delete",
		WorktreePath: "/tmp/ws-ready",
		TmuxSession:  "kenn-forge-ws-ready",
		Status:       "ready",
	}))

	err := d.FailWorkspaceDeletion(ctx, "ws-ready", "teardown failed")
	require.ErrorContains(err, "workspace is not deleting")
}

func TestBeginWorkspaceDeletionRejectsCreatingWorkspace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-creating",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/setup",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-creating",
		TmuxSession:     "kenn-forge-ws-creating",
		Status:          "creating",
	}))

	started, err := d.BeginWorkspaceDeletion(ctx, "ws-creating")
	require.ErrorIs(err, ErrWorkspaceSetupInProgress)
	assert.False(started)

	got, err := d.GetWorkspace(ctx, "ws-creating")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("creating", got.Status)
}

func TestUpdateWorkspaceBranchRejectsMissingWorkspace(t *testing.T) {
	d := openTestDB(t)

	err := d.UpdateWorkspaceBranch(
		t.Context(), "missing-workspace", "feature/example",
	)

	require.Error(t, err)
}

func TestUpdateWorkspaceMRHeadRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	ws := &Workspace{
		ID:           "ws-head-repo",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		WorktreePath: "/tmp/ws-head-repo",
		TmuxSession:  "ws-head-repo",
		Status:       "creating",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	got, err := d.GetWorkspace(ctx, "ws-head-repo")
	require.NoError(err)
	require.NotNil(got)
	assert.Nil(got.MRHeadRepo)

	forkURL := "https://github.com/fork-owner/widget.git"
	require.NoError(d.UpdateWorkspaceMRHeadRepo(ctx, "ws-head-repo", &forkURL))
	got, err = d.GetWorkspace(ctx, "ws-head-repo")
	require.NoError(err)
	require.NotNil(got)
	require.NotNil(got.MRHeadRepo)
	assert.Equal(forkURL, *got.MRHeadRepo)

	unknown := ""
	require.NoError(d.UpdateWorkspaceMRHeadRepo(ctx, "ws-head-repo", &unknown))
	got, err = d.GetWorkspace(ctx, "ws-head-repo")
	require.NoError(err)
	require.NotNil(got)
	require.NotNil(got.MRHeadRepo)
	assert.Empty(*got.MRHeadRepo)

	require.NoError(d.UpdateWorkspaceMRHeadRepo(ctx, "ws-head-repo", nil))
	got, err = d.GetWorkspace(ctx, "ws-head-repo")
	require.NoError(err)
	require.NotNil(got)
	assert.Nil(got.MRHeadRepo)
}

func TestUpdateWorkspaceMRHeadRepoForSnapshotRejectsStaleRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	repoID := insertTestRepo(t, d, "acme", "widget")
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	mr := &MergeRequest{
		RepoID:           repoID,
		PlatformID:       7001,
		Number:           7,
		Title:            "Same-repo PR",
		State:            MergeRequestStateOpen,
		HeadBranch:       "feature",
		BaseBranch:       "main",
		HeadRepoCloneURL: "https://github.com/acme/widget.git",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}
	_, accepted, err := d.UpsertMergeRequestSnapshot(ctx, mr)
	require.NoError(err)
	require.True(accepted)
	stale, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, mr.Number)
	require.NoError(err)
	require.NotNil(stale)

	forkURL := "https://github.com/forker/widget.git"
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "ws-stale-head-repo-write",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   mr.Number,
		MRHeadRepo:   &forkURL,
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}))

	updated := *mr
	updated.Title = "Newer fork snapshot"
	updated.HeadRepoCloneURL = forkURL
	updated.UpdatedAt = now.Add(time.Minute)
	updated.LastActivityAt = updated.UpdatedAt
	_, accepted, err = d.UpsertMergeRequestSnapshot(ctx, &updated)
	require.NoError(err)
	require.True(accepted)

	applied, err := d.UpdateWorkspaceMRHeadRepoForSnapshot(
		ctx,
		"ws-stale-head-repo-write",
		repoID,
		mr.Number,
		stale.SnapshotRevision,
		false,
		nil,
	)
	require.NoError(err)
	assert.False(applied)

	stored, err := d.GetWorkspace(ctx, "ws-stale-head-repo-write")
	require.NoError(err)
	require.NotNil(stored)
	require.NotNil(stored.MRHeadRepo)
	assert.Equal(forkURL, *stored.MRHeadRepo)

	_, err = d.UpdateWorkspaceMRHeadRepoForSnapshot(
		ctx,
		"missing-workspace",
		repoID,
		mr.Number,
		updated.SnapshotRevision,
		false,
		nil,
	)
	require.Error(err)
	assert.Contains(err.Error(), "workspace \"missing-workspace\" not found")
}

func TestUpdateWorkspaceMRHeadRepoForSnapshotRejectsRepositoryMismatch(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	originalRepoID := insertTestRepo(t, database, "acme", "original")
	replacementRepoID := insertTestRepo(t, database, "acme", "replacement")
	insertTestMR(t, database, replacementRepoID, 7, "replacement pull", baseTime())
	replacementMR, err := database.GetMergeRequestByRepoIDAndNumber(
		t.Context(), replacementRepoID, 7,
	)
	require.NoError(err)
	require.NotNil(replacementMR)
	workspace := &Workspace{
		ID: "ws-repository-mismatch", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "original",
		ItemType: WorkspaceItemTypePullRequest, ItemNumber: 7,
		WorktreePath: t.TempDir(), Status: "ready",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.Equal(originalRepoID, workspace.RepoID)
	forkURL := "https://github.com/contributor/replacement.git"

	applied, err := database.UpdateWorkspaceMRHeadRepoForSnapshot(
		t.Context(), workspace.ID, replacementRepoID, 7,
		replacementMR.SnapshotRevision, false, &forkURL,
	)

	require.NoError(err)
	require.False(applied)
	stored, err := database.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.Nil(stored.MRHeadRepo)
}

func TestWorkspaceItemKeyDefaultsFromItemNumber(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-provider-issue",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeIssue,
		ItemNumber:      42,
		GitHeadRef:      "kenn-forge/issue-42",
		WorkspaceBranch: "kenn-forge/issue-42",
		WorktreePath:    "/tmp/ws-provider-issue",
		TmuxSession:     "ws-provider-issue",
		Status:          "ready",
	}))

	got, err := d.GetWorkspace(ctx, "ws-provider-issue")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(WorkspaceItemTypeIssue, got.ItemType)
	assert.Equal(42, got.ItemNumber)
	assert.Equal("42", got.ItemKey)

	byIssue, err := d.GetWorkspaceByIssue(ctx, "github.com", "acme", "widget", 42)
	require.NoError(err)
	require.NotNil(byIssue)
	assert.Equal("42", byIssue.ItemKey)
}

// Ad-hoc workspaces all carry item_number 0, so the number fallback would key
// every one of them in a repository as "0" and silently collide.
func TestAdHocWorkspaceRequiresItemKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	err := d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-adhoc-nokey",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeAdHoc,
		GitHeadRef:      "spike/thing",
		WorkspaceBranch: "spike/thing",
		WorktreePath:    "/tmp/ws-adhoc-nokey",
		TmuxSession:     "ws-adhoc-nokey",
		Status:          "ready",
	})
	require.Error(err)
	assert.Contains(err.Error(), "item_key is required")

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-adhoc",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeAdHoc,
		ItemKey:         AdHocWorkspaceItemKey("spike/thing"),
		GitHeadRef:      "spike/thing",
		WorkspaceBranch: "spike/thing",
		WorktreePath:    "/tmp/ws-adhoc",
		TmuxSession:     "ws-adhoc",
		Status:          "ready",
	}))

	got, err := d.GetWorkspace(ctx, "ws-adhoc")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("adhoc:spike/thing", got.ItemKey)

	summaries, err := d.ListWorkspaceSummaries(ctx)
	require.NoError(err)
	found := false
	for _, summary := range summaries {
		if summary.ID == "ws-adhoc" {
			found = true
			assert.Equal("adhoc:spike/thing", summary.ItemKey)
		}
	}
	assert.True(found, "ad-hoc workspace must appear in summaries")
}

func TestKataWorkspaceMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	metadata := WorkspaceKataMetadata{
		DaemonID:    "main",
		ProjectUID:  "project-kata",
		ProjectName: "Kata",
		IssueUID:    "kata-uid-1",
		ShortID:     "task-123",
		QualifiedID: "Kata#task-123",
		Title:       "Fix widget",
	}
	itemKey := KataWorkspaceItemKey(metadata)
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "ws-kata-task",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeKataTask,
		ItemKey:         itemKey,
		GitHeadRef:      "kenn-forge/kata/task-123-fix-widget",
		WorkspaceBranch: "kenn-forge/kata/task-123-fix-widget",
		WorktreePath:    "/tmp/ws-kata-task",
		TmuxSession:     "ws-kata-task",
		Status:          "ready",
		KataMetadata:    &metadata,
	}))

	got, err := d.GetWorkspace(ctx, "ws-kata-task")
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(WorkspaceItemTypeKataTask, got.ItemType)
	assert.Equal(0, got.ItemNumber)
	assert.Equal(itemKey, got.ItemKey)
	require.NotNil(got.KataMetadata)
	assert.Equal("main", got.KataMetadata.DaemonID)
	assert.Equal("project-kata", got.KataMetadata.ProjectUID)
	assert.Equal("Kata", got.KataMetadata.ProjectName)
	assert.Equal("kata-uid-1", got.KataMetadata.IssueUID)
	assert.Equal("task-123", got.KataMetadata.ShortID)
	assert.Equal("Kata#task-123", got.KataMetadata.QualifiedID)
	assert.Equal("Fix widget", got.KataMetadata.Title)

	summary, err := d.GetWorkspaceSummary(ctx, "ws-kata-task")
	require.NoError(err)
	require.NotNil(summary)
	assert.Equal(itemKey, summary.ItemKey)
	require.NotNil(summary.KataMetadata)
	require.NotNil(summary.MRTitle)
	assert.Equal("Fix widget", *summary.MRTitle)
}

func TestGetWorkspaceByIssueForProviderDisambiguatesProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	for _, provider := range []string{"github", "gitlab"} {
		_, err := d.UpsertRepo(ctx, RepoIdentity{
			Platform:     provider,
			PlatformHost: "forge.example.com",
			Owner:        "acme",
			Name:         "widget",
		})
		require.NoError(err)
	}
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "github-issue-workspace",
		Platform:        "github",
		PlatformHost:    "forge.example.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeIssue,
		ItemNumber:      7,
		GitHeadRef:      "kenn-forge/issue-7",
		WorkspaceBranch: "kenn-forge/issue-7",
		WorktreePath:    "/tmp/github-issue-workspace",
		TmuxSession:     "github-issue-workspace",
		Status:          "ready",
	}))
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "gitlab-issue-workspace",
		Platform:        "gitlab",
		PlatformHost:    "forge.example.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypeIssue,
		ItemNumber:      7,
		GitHeadRef:      "kenn-forge/issue-7",
		WorkspaceBranch: "kenn-forge/issue-7",
		WorktreePath:    "/tmp/gitlab-issue-workspace",
		TmuxSession:     "gitlab-issue-workspace",
		Status:          "ready",
	}))

	got, err := d.GetWorkspaceByIssueForProvider(
		ctx, "gitlab", "forge.example.com", "acme", "widget", 7,
	)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("gitlab-issue-workspace", got.ID)
	assert.Equal("gitlab", got.Platform)

	miss, err := d.GetWorkspaceByIssueForProvider(
		ctx, "forgejo", "forge.example.com", "acme", "widget", 7,
	)
	require.NoError(err)
	assert.Nil(miss)
}

func TestGetWorkspaceByMRForProviderDisambiguatesProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	for _, provider := range []string{"github", "gitlab"} {
		_, err := d.UpsertRepo(ctx, RepoIdentity{
			Platform:     provider,
			PlatformHost: "forge.example.com",
			Owner:        "acme",
			Name:         "widget",
		})
		require.NoError(err)
	}
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "github-pr-workspace",
		Platform:        "github",
		PlatformHost:    "forge.example.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      7,
		GitHeadRef:      "feature",
		WorkspaceBranch: "kenn-forge/pr-7",
		WorktreePath:    "/tmp/github-pr-workspace",
		TmuxSession:     "github-pr-workspace",
		Status:          "ready",
	}))
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:              "gitlab-pr-workspace",
		Platform:        "gitlab",
		PlatformHost:    "forge.example.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        WorkspaceItemTypePullRequest,
		ItemNumber:      7,
		GitHeadRef:      "feature",
		WorkspaceBranch: "kenn-forge/pr-7",
		WorktreePath:    "/tmp/gitlab-pr-workspace",
		TmuxSession:     "gitlab-pr-workspace",
		Status:          "ready",
	}))

	got, err := d.GetWorkspaceByMRForProvider(
		ctx, "gitlab", "forge.example.com", "acme", "widget", 7,
	)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("gitlab-pr-workspace", got.ID)
	assert.Equal("gitlab", got.Platform)

	miss, err := d.GetWorkspaceByMRForProvider(
		ctx, "forgejo", "forge.example.com", "acme", "widget", 7,
	)
	require.NoError(err)
	assert.Nil(miss)
}

func workspaceLinkageTestDB(t *testing.T) *DB {
	t.Helper()
	d := openTestDB(t)
	_, err := d.UpsertRepo(t.Context(), RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(t, err)
	return d
}

func insertWorkspaceLinkageFixture(
	t *testing.T,
	d *DB,
	ws Workspace,
	createdAt time.Time,
) {
	t.Helper()
	require.NoError(t, d.InsertWorkspace(t.Context(), &ws))
	_, err := d.WriteDB().ExecContext(
		t.Context(),
		`UPDATE forge_workspaces SET created_at = ? WHERE id = ?`,
		createdAt.UTC(), ws.ID,
	)
	require.NoError(t, err)
}

func TestGetWorkspaceLinkedToMRForProviderSelection(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	t.Run("associated fallback keeps direct lookup isolated", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		d := workspaceLinkageTestDB(t)
		associatedPR := 42
		insertWorkspaceLinkageFixture(t, d, Workspace{
			ID:                 "associated-issue",
			Platform:           "github",
			PlatformHost:       "github.com",
			RepoOwner:          "acme",
			RepoName:           "widget",
			ItemType:           WorkspaceItemTypeIssue,
			ItemNumber:         7,
			AssociatedPRNumber: &associatedPR,
			GitHeadRef:         "issue-7",
			WorkspaceBranch:    "issue-7",
			WorktreePath:       "/tmp/associated-issue",
			TmuxSession:        "associated-issue",
			Status:             "ready",
		}, base)

		linked, err := d.GetWorkspaceLinkedToMRForProvider(
			t.Context(), "GITHUB", "GITHUB.COM", "ACME", "WIDGET", associatedPR,
		)
		require.NoError(err)
		require.NotNil(linked)
		assert.Equal("associated-issue", linked.ID)

		direct, err := d.GetWorkspaceByMRForProvider(
			t.Context(), "github", "github.com", "acme", "widget", associatedPR,
		)
		require.NoError(err)
		assert.Nil(direct)
	})

	t.Run("newest associated wins regardless of status with stable ID tie", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		d := workspaceLinkageTestDB(t)
		associatedPR := 42
		insertWorkspaceLinkageFixture(t, d, Workspace{
			ID:                 "associated-ready",
			Platform:           "github",
			PlatformHost:       "github.com",
			RepoOwner:          "acme",
			RepoName:           "widget",
			ItemType:           WorkspaceItemTypeIssue,
			ItemNumber:         7,
			AssociatedPRNumber: &associatedPR,
			GitHeadRef:         "issue-7",
			WorkspaceBranch:    "issue-7",
			WorktreePath:       "/tmp/associated-ready",
			TmuxSession:        "associated-ready",
			Status:             "ready",
		}, base)
		for _, fixture := range []struct {
			id     string
			status string
		}{
			{id: "associated-error", status: "error"},
			{id: "associated-z-creating", status: "creating"},
		} {
			insertWorkspaceLinkageFixture(t, d, Workspace{
				ID:                 fixture.id,
				Platform:           "github",
				PlatformHost:       "github.com",
				RepoOwner:          "acme",
				RepoName:           "widget",
				ItemType:           WorkspaceItemTypeAdHoc,
				ItemNumber:         0,
				ItemKey:            AdHocWorkspaceItemKey(fixture.id),
				AssociatedPRNumber: &associatedPR,
				GitHeadRef:         fixture.id,
				WorkspaceBranch:    fixture.id,
				WorktreePath:       "/tmp/" + fixture.id,
				TmuxSession:        fixture.id,
				Status:             fixture.status,
			}, base.Add(time.Minute))
		}

		linked, err := d.GetWorkspaceLinkedToMRForProvider(
			t.Context(), "github", "github.com", "acme", "widget", associatedPR,
		)
		require.NoError(err)
		require.NotNil(linked)
		assert.Equal("associated-z-creating", linked.ID)
		assert.Equal("creating", linked.Status)
	})

	t.Run("direct workspace wins regardless of creation time", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		d := workspaceLinkageTestDB(t)
		associatedPR := 42
		insertWorkspaceLinkageFixture(t, d, Workspace{
			ID:                 "new-associated",
			Platform:           "github",
			PlatformHost:       "github.com",
			RepoOwner:          "acme",
			RepoName:           "widget",
			ItemType:           WorkspaceItemTypeIssue,
			ItemNumber:         7,
			AssociatedPRNumber: &associatedPR,
			GitHeadRef:         "issue-7",
			WorkspaceBranch:    "issue-7",
			WorktreePath:       "/tmp/new-associated",
			TmuxSession:        "new-associated",
			Status:             "ready",
		}, base.Add(time.Hour))
		insertWorkspaceLinkageFixture(t, d, Workspace{
			ID:              "old-direct",
			Platform:        "github",
			PlatformHost:    "github.com",
			RepoOwner:       "acme",
			RepoName:        "widget",
			ItemType:        WorkspaceItemTypePullRequest,
			ItemNumber:      associatedPR,
			GitHeadRef:      "feature",
			WorkspaceBranch: "pr-42",
			WorktreePath:    "/tmp/old-direct",
			TmuxSession:     "old-direct",
			Status:          "ready",
		}, base)

		linked, err := d.GetWorkspaceLinkedToMRForProvider(
			t.Context(), "github", "github.com", "acme", "widget", associatedPR,
		)
		require.NoError(err)
		require.NotNil(linked)
		assert.Equal("old-direct", linked.ID)
	})
}

func TestFreshWorkspaceRuntimeSessionSchemaIncludesTmuxSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	rows, err := d.ReadDB().QueryContext(
		context.Background(),
		`PRAGMA table_info(forge_workspace_runtime_sessions)`,
	)
	require.NoError(err)
	defer rows.Close()

	columns := make(map[string]string)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		require.NoError(rows.Scan(
			&cid, &name, &columnType, &notNull, &defaultVal, &pk,
		))
		columns[name] = columnType
	}
	require.NoError(rows.Err())

	assert.Equal("TEXT", columns["display_region"])
	assert.Equal("TEXT", columns["tmux_session"])
	assert.Equal("DATETIME", columns["created_at"])
}

func TestWorkspaceIdentifierCasefoldTriggers(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, git_head_ref, worktree_path, tmux_session)
		VALUES ('mixed', 'github.com', 'Acme', 'widget', 'pull_request', 1, '1', 'feature',
		        '/tmp/mixed', 'mixed')`)
	require.Error(err)
	require.Contains(err.Error(), "workspace repo identifiers must be provider-canonical")

	ws := &Workspace{
		ID:           "lower",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature",
		WorktreePath: "/tmp/lower",
		TmuxSession:  "lower",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	_, err = d.WriteDB().ExecContext(ctx, `
		UPDATE forge_workspaces SET repo_name = 'Widget' WHERE id = 'lower'`)
	require.Error(err)
	require.Contains(err.Error(), "workspace repo identifiers must be provider-canonical")
}

func TestWorkspaceCanonicalizationPreservesGitLabRepoDisplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "gitlab-project-name",
		Owner:          "Group/SubGroup",
		Name:           "ProjectName",
		RepoPath:       "Group/SubGroup/ProjectName",
	})
	require.NoError(err)

	ws := &Workspace{
		ID:           "gitlab-workspace",
		PlatformHost: "gitlab.example.com",
		RepoOwner:    "group/subgroup",
		RepoName:     "projectname",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature",
		WorktreePath: "/tmp/gitlab-workspace",
		TmuxSession:  "gitlab-workspace",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("Group/SubGroup", got.RepoOwner)
	assert.Equal("ProjectName", got.RepoName)

	byMR, err := d.GetWorkspaceByMR(ctx, "gitlab.example.com", "GROUP/SubGroup", "PROJECTName", 7)
	require.NoError(err)
	require.NotNil(byMR)
	assert.Equal(ws.ID, byMR.ID)

	duplicate := *ws
	duplicate.ID = "gitlab-workspace-duplicate"
	duplicate.RepoOwner = "GROUP/SubGroup"
	duplicate.RepoName = "PROJECTName"
	err = d.InsertWorkspace(ctx, &duplicate)
	require.Error(err)
	require.Contains(err.Error(), "UNIQUE constraint failed")
}

func TestWorkspaceUniqueConstraint(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()

	t.Run("pull request duplicates conflict", func(t *testing.T) {
		ws := &Workspace{
			ID:           "ws-pr-1",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget",
			ItemType:     WorkspaceItemTypePullRequest,
			ItemNumber:   42,
			GitHeadRef:   "feat/pr-1",
			WorktreePath: "/tmp/ws-pr-1",
			TmuxSession:  "ws-pr-1",
			Status:       "creating",
		}
		require.NoError(t, d.InsertWorkspace(ctx, ws))

		dup := &Workspace{
			ID:           "ws-pr-2",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget",
			ItemType:     WorkspaceItemTypePullRequest,
			ItemNumber:   42,
			GitHeadRef:   "feat/pr-2",
			WorktreePath: "/tmp/ws-pr-2",
			TmuxSession:  "ws-pr-2",
			Status:       "creating",
		}
		err := d.InsertWorkspace(ctx, dup)
		require.Error(t, err)
	})

	t.Run("issue duplicates conflict", func(t *testing.T) {
		ws := &Workspace{
			ID:           "ws-issue-1",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget-issues",
			ItemType:     WorkspaceItemTypeIssue,
			ItemNumber:   42,
			GitHeadRef:   "kenn-forge/issue-42",
			WorktreePath: "/tmp/ws-issue-1",
			TmuxSession:  "ws-issue-1",
			Status:       "creating",
		}
		require.NoError(t, d.InsertWorkspace(ctx, ws))

		dup := &Workspace{
			ID:           "ws-issue-2",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget-issues",
			ItemType:     WorkspaceItemTypeIssue,
			ItemNumber:   42,
			GitHeadRef:   "kenn-forge/issue-42-copy",
			WorktreePath: "/tmp/ws-issue-2",
			TmuxSession:  "ws-issue-2",
			Status:       "creating",
		}
		err := d.InsertWorkspace(ctx, dup)
		require.Error(t, err)
	})

	t.Run("pull request and issue can coexist", func(t *testing.T) {
		pr := &Workspace{
			ID:           "ws-mixed-pr",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget-mixed",
			ItemType:     WorkspaceItemTypePullRequest,
			ItemNumber:   7,
			GitHeadRef:   "feat/mixed-pr",
			WorktreePath: "/tmp/ws-mixed-pr",
			TmuxSession:  "ws-mixed-pr",
			Status:       "creating",
		}
		require.NoError(t, d.InsertWorkspace(ctx, pr))

		issue := &Workspace{
			ID:           "ws-mixed-issue",
			PlatformHost: "github.com",
			RepoOwner:    "acme",
			RepoName:     "widget-mixed",
			ItemType:     WorkspaceItemTypeIssue,
			ItemNumber:   7,
			GitHeadRef:   "kenn-forge/issue-7",
			WorktreePath: "/tmp/ws-mixed-issue",
			TmuxSession:  "ws-mixed-issue",
			Status:       "creating",
		}
		require.NoError(t, d.InsertWorkspace(ctx, issue))
	})
}

func TestWorkspaceUniqueConstraintIncludesPlatform(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "github",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)
	_, err = d.UpsertRepo(ctx, RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})
	require.NoError(err)

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "github-workspace",
		Platform:     "github",
		PlatformHost: "code.example.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature",
		WorktreePath: "/tmp/github-workspace",
		TmuxSession:  "github-workspace",
	}))
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "gitlab-workspace",
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature",
		WorktreePath: "/tmp/gitlab-workspace",
		TmuxSession:  "gitlab-workspace",
	}))
}

func TestWorkspaceSummariesDoNotJoinAcrossProviders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	githubRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "github",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "github-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "gitlab-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	insertTestMRWithOptions(t, d, testMR(githubRepoID, 7, withMRTitle("github PR")))
	insertTestMRWithOptions(t, d, testMR(gitlabRepoID, 7, withMRTitle("gitlab MR")))

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "gitlab-workspace",
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature",
		WorktreePath: "/tmp/gitlab-workspace",
		TmuxSession:  "gitlab-workspace",
	}))

	summary, err := d.GetWorkspaceSummary(ctx, "gitlab-workspace")
	require.NoError(err)
	require.NotNil(summary)
	assert.Equal("gitlab", summary.Platform)
	require.NotNil(summary.MRTitle)
	assert.Equal("gitlab MR", *summary.MRTitle)
}

func TestWorkspaceSummaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	// Seed repo, issue, and PR.
	repoID := insertTestRepo(t, d, "acme", "widget")
	issueActivity := base.Add(3 * time.Hour)
	prActivity := base.Add(2 * time.Hour)
	insertTestIssue(
		t, d, repoID, 7,
		"Track workspace association",
		issueActivity,
	)
	_, err := d.UpsertMergeRequest(ctx, &MergeRequest{
		RepoID:         repoID,
		PlatformID:     5001,
		Number:         42,
		URL:            "https://github.com/acme/widget/pull/42",
		Title:          "Add workspace support",
		Author:         "alice",
		State:          "open",
		IsDraft:        true,
		CIStatus:       "pending",
		ReviewDecision: "REVIEW_REQUIRED",
		Additions:      100,
		Deletions:      20,
		CreatedAt:      base,
		UpdatedAt:      base.Add(time.Hour),
		LastActivityAt: prActivity,
	})
	require.NoError(err)

	// PR workspace with matching PR (earlier created_at).
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, git_head_ref,
		     worktree_path, tmux_session, status,
		     created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ws-with-mr", "github.com", "acme", "widget",
		WorkspaceItemTypePullRequest, 42, "42", "feat/workspace",
		"/tmp/ws-with-mr", "ws-with-mr", "ready",
		base,
	)
	require.NoError(err)

	// Issue workspace with owner issue metadata and associated PR metadata.
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, associated_pr_number, git_head_ref,
		     worktree_path, tmux_session, status,
		     created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ws-issue-with-pr", "github.com", "acme", "widget",
		WorkspaceItemTypeIssue, 7, "7", 42, "feature/from-issue",
		"/tmp/ws-issue-with-pr", "ws-issue-with-pr", "ready",
		base.Add(30*time.Minute),
	)
	require.NoError(err)

	// Workspace without matching PR (later created_at, no repo).
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, git_head_ref,
		     worktree_path, tmux_session, status,
		     created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ws-no-mr", "github.com", "acme", "gadget",
		WorkspaceItemTypePullRequest, 99, "99", "fix/thing",
		"/tmp/ws-no-mr", "ws-no-mr", "creating",
		base.Add(time.Hour),
	)
	require.NoError(err)

	// ListWorkspaceSummaries
	summaries, err := d.ListWorkspaceSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 3)

	// Newest first.
	noMR := summaries[0]
	issueWithPR := summaries[1]
	withMR := summaries[2]
	assert.Equal("ws-no-mr", noMR.ID)
	assert.Equal("ws-issue-with-pr", issueWithPR.ID)
	assert.Equal("ws-with-mr", withMR.ID)

	// Owner-derived fields nil when no owner match.
	assert.Nil(noMR.MRTitle)
	assert.Nil(noMR.MRState)
	assert.Nil(noMR.MRIsDraft)
	assert.Nil(noMR.MRCIStatus)
	assert.Nil(noMR.MRReviewDecision)
	assert.Nil(noMR.MRAdditions)
	assert.Nil(noMR.MRDeletions)
	assert.Nil(noMR.AssociatedPRNumber)
	assert.Nil(noMR.ItemLastActivityAt)
	assert.False(noMR.SourceItemVisible)
	assert.False(noMR.AssociatedPRVisible)

	// Issue workspace keeps issue-owned header metadata and the linked PR number.
	require.NotNil(issueWithPR.MRTitle)
	assert.Equal("Track workspace association", *issueWithPR.MRTitle)
	require.NotNil(issueWithPR.MRState)
	assert.Equal("open", *issueWithPR.MRState)
	assert.Nil(issueWithPR.MRIsDraft)
	assert.Nil(issueWithPR.MRCIStatus)
	assert.Nil(issueWithPR.MRReviewDecision)
	assert.Nil(issueWithPR.MRAdditions)
	assert.Nil(issueWithPR.MRDeletions)
	require.NotNil(issueWithPR.AssociatedPRNumber)
	assert.Equal(42, *issueWithPR.AssociatedPRNumber)
	require.NotNil(issueWithPR.ItemLastActivityAt)
	assert.Equal(issueActivity.UTC(), issueWithPR.ItemLastActivityAt.UTC())
	assert.True(issueWithPR.SourceItemVisible)
	assert.True(issueWithPR.AssociatedPRVisible)

	// PR workspace exposes PR metadata in the owner slots.
	require.NotNil(withMR.MRTitle)
	assert.Equal("Add workspace support", *withMR.MRTitle)
	require.NotNil(withMR.MRState)
	assert.Equal("open", *withMR.MRState)
	require.NotNil(withMR.MRIsDraft)
	assert.True(*withMR.MRIsDraft)
	require.NotNil(withMR.MRCIStatus)
	assert.Equal("pending", *withMR.MRCIStatus)
	require.NotNil(withMR.MRReviewDecision)
	assert.Equal("REVIEW_REQUIRED", *withMR.MRReviewDecision)
	require.NotNil(withMR.MRAdditions)
	assert.Equal(100, *withMR.MRAdditions)
	require.NotNil(withMR.MRDeletions)
	assert.Equal(20, *withMR.MRDeletions)
	assert.Nil(withMR.AssociatedPRNumber)
	require.NotNil(withMR.ItemLastActivityAt)
	assert.Equal(prActivity.UTC(), withMR.ItemLastActivityAt.UTC())
	assert.True(withMR.SourceItemVisible)
	assert.False(withMR.AssociatedPRVisible)

	// GetWorkspaceSummary by ID
	single, err := d.GetWorkspaceSummary(ctx, "ws-issue-with-pr")
	require.NoError(err)
	require.NotNil(single)
	assert.Equal("ws-issue-with-pr", single.ID)
	require.NotNil(single.MRTitle)
	assert.Equal("Track workspace association", *single.MRTitle)
	require.NotNil(single.AssociatedPRNumber)
	assert.Equal(42, *single.AssociatedPRNumber)
	require.NotNil(single.ItemLastActivityAt)
	assert.Equal(issueActivity.UTC(), single.ItemLastActivityAt.UTC())
	assert.True(single.SourceItemVisible)
	assert.True(single.AssociatedPRVisible)

	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', 42, 'pull-42', ?, ?, 'removed_upstream')`,
		repoID, base, base,
	)
	require.NoError(err)
	single, err = d.GetWorkspaceSummary(ctx, "ws-issue-with-pr")
	require.NoError(err)
	require.NotNil(single)
	assert.True(single.SourceItemVisible)
	assert.False(single.AssociatedPRVisible)
	_, err = d.WriteDB().ExecContext(ctx, `
		UPDATE forge_archive_items
		SET lifecycle_state = 'inaccessible'
		WHERE repo_id = ? AND item_type = 'merge_request' AND item_number = 42`,
		repoID,
	)
	require.NoError(err)
	single, err = d.GetWorkspaceSummary(ctx, "ws-issue-with-pr")
	require.NoError(err)
	require.NotNil(single)
	assert.True(single.SourceItemVisible)
	assert.True(single.AssociatedPRVisible)
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'issue', 7, 'issue-7', ?, ?, 'removed_upstream')`,
		repoID, base, base,
	)
	require.NoError(err)
	single, err = d.GetWorkspaceSummary(ctx, "ws-issue-with-pr")
	require.NoError(err)
	require.NotNil(single)
	assert.False(single.SourceItemVisible)
	assert.True(single.AssociatedPRVisible)

	// GetWorkspaceSummary miss
	missSum, err := d.GetWorkspaceSummary(ctx, "nonexistent")
	require.NoError(err)
	assert.Nil(missSum)
}

func TestWorkspaceSummariesRetainWorkspaceWithoutRemovedPullMetadata(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "acme", "widget")
	mr := testMR(repoID, 7, withMRTitle("Removed pull"), withMRBranches("feature", "main"))
	mr.CIStatus = "failure"
	mr.ReviewDecision = "changes_requested"
	mr.Additions = 12
	mr.Deletions = 3
	mr.CommentCount = 5
	mr.MergeableState = "dirty"
	insertTestMRWithOptions(t, d, mr)
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID: "ws-removed-pr", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: WorkspaceItemTypePullRequest, ItemNumber: 7,
		GitHeadRef: "feature", WorktreePath: "/tmp/ws-removed-pr",
		TmuxSession: "ws-removed-pr", Status: "ready",
	}))
	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', 7, 'pull-7', ?, ?, 'removed_upstream')`,
		repoID, now, now,
	)
	require.NoError(err)

	summary, err := d.GetWorkspaceSummary(ctx, "ws-removed-pr")
	require.NoError(err)
	require.NotNil(summary)
	require.Equal("ws-removed-pr", summary.ID)
	require.Equal(7, summary.ItemNumber)
	require.Nil(summary.SourceTitle)
	require.Nil(summary.SourceState)
	require.Nil(summary.SourceURL)
	require.Nil(summary.MRTitle)
	require.Nil(summary.MRState)
	require.Nil(summary.MRIsDraft)
	require.Nil(summary.MRCIStatus)
	require.Nil(summary.MRReviewDecision)
	require.Nil(summary.MRAdditions)
	require.Nil(summary.MRDeletions)
	require.Nil(summary.MRCommentCount)
	require.Nil(summary.MRMergeableState)
	require.Nil(summary.MRHeadBranch)
	require.Nil(summary.ItemLastActivityAt)
	require.False(summary.SourceItemVisible)
	require.False(summary.AssociatedPRVisible)

	summaries, err := d.ListWorkspaceSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 1)
	require.Equal("ws-removed-pr", summaries[0].ID)
	require.Nil(summaries[0].MRTitle)
}

func TestWorkspaceSummariesFollowStableRepositoryAcrossReusedRoute(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	observedAt := baseTime()
	original := reconcileCatalogRepository(
		t, d, "provider-original", "org-a", "project-a", observedAt,
	)
	associatedPR := 42
	headRepo := "https://github.com/contributor/project-a.git"
	require.NoError(d.InsertWorkspace(t.Context(), &Workspace{
		ID: "ws-reused-route", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "org-a", RepoName: "project-a",
		ItemType: WorkspaceItemTypePullRequest, ItemNumber: 7,
		AssociatedPRNumber: &associatedPR,
		GitHeadRef:         "feature/stale",
		MRHeadRepo:         &headRepo,
		WorktreePath:       "/tmp/ws-reused-route",
		Status:             "ready",
	}))
	reconcileCatalogRepository(
		t, d, "provider-original", "org-a", "renamed-project", observedAt.Add(time.Minute),
	)
	reconcileCatalogRepository(
		t, d, "provider-replacement", "org-a", "project-a", observedAt.Add(2*time.Minute),
	)

	summary, err := d.GetWorkspaceSummary(t.Context(), "ws-reused-route")
	require.NoError(err)
	require.NotNil(summary)
	require.Equal(original.Repository.ID, summary.RepoID)
	require.Equal("renamed-project", summary.RepoName)
	require.True(summary.SourceItemVisible)
	require.True(summary.AssociatedPRVisible)

	summaries, err := d.ListWorkspaceSummaries(t.Context())
	require.NoError(err)
	require.Len(summaries, 1)
	require.True(summaries[0].SourceItemVisible)
	require.True(summaries[0].AssociatedPRVisible)
}

func TestSetWorkspaceAssociatedPRNumberIfNull(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := context.Background()

	_, err := d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform_host, repo_owner, repo_name,
		     item_type, item_number, item_key, git_head_ref,
		     worktree_path, tmux_session, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ws-issue", "github.com", "acme", "widget",
		WorkspaceItemTypeIssue, 7, "7", "feature/issue-7",
		"/tmp/ws-issue", "ws-issue", "ready",
	)
	require.NoError(err)

	changed, err := d.SetWorkspaceAssociatedPRNumberIfNull(
		ctx, "ws-issue", 42,
	)
	require.NoError(err)
	assert.True(changed)

	ws, err := d.GetWorkspace(ctx, "ws-issue")
	require.NoError(err)
	require.NotNil(ws)
	require.NotNil(ws.AssociatedPRNumber)
	assert.Equal(42, *ws.AssociatedPRNumber)

	changed, err = d.SetWorkspaceAssociatedPRNumberIfNull(
		ctx, "ws-issue", 99,
	)
	require.NoError(err)
	assert.False(changed)

	ws, err = d.GetWorkspace(ctx, "ws-issue")
	require.NoError(err)
	require.NotNil(ws)
	require.NotNil(ws.AssociatedPRNumber)
	assert.Equal(42, *ws.AssociatedPRNumber)
}

func TestUpdateMRTitleBody(t *testing.T) {
	assert := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "owner", "repo")
	mr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     12345,
		Number:         1,
		URL:            "https://github.com/owner/repo/pull/1",
		Title:          "original title",
		Author:         "author",
		State:          "open",
		Body:           "original body",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CommentCount:   5,
		CIStatus:       "success",
		ReviewDecision: "APPROVED",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: base,
	}
	id, err := d.UpsertMergeRequest(ctx, mr)
	assert.NoError(err)

	ghUpdatedAt := base.Add(10 * time.Minute)
	assert.NoError(d.UpdateMRTitleBody(ctx, id, "new title", "new body", ghUpdatedAt))

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 1)
	assert.NoError(err)
	assert.NotNil(got)
	assert.Equal("new title", got.Title)
	assert.Equal("new body", got.Body)
	assert.True(got.UpdatedAt.Equal(ghUpdatedAt), "UpdatedAt should be ghUpdatedAt")
	assert.True(got.LastActivityAt.Equal(ghUpdatedAt), "LastActivityAt should be ghUpdatedAt")
	// Derived fields must be preserved.
	assert.Equal(5, got.CommentCount)
	assert.Equal("success", got.CIStatus)
	assert.Equal("APPROVED", got.ReviewDecision)
}

func TestUpdateMRTitleBodyReplacesSyntheticActivityWithProviderTime(t *testing.T) {
	assert := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "owner2", "repo2")
	futureActivity := base.Add(1 * time.Hour)
	mr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     99999,
		Number:         2,
		URL:            "https://github.com/owner2/repo2/pull/2",
		Title:          "initial title",
		Author:         "author",
		State:          "open",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      base,
		UpdatedAt:      base,
		LastActivityAt: futureActivity,
	}
	id, err := d.UpsertMergeRequest(ctx, mr)
	assert.NoError(err)

	// updatedAt is 30 min, newer than base so the update applies.
	updatedAt := base.Add(30 * time.Minute)
	assert.NoError(d.UpdateMRTitleBody(ctx, id, "new title", "new body", updatedAt))

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 2)
	assert.NoError(err)
	assert.NotNil(got)
	// UpdatedAt gets the 30-min value.
	assert.True(got.UpdatedAt.Equal(updatedAt), "UpdatedAt should be updatedAt")
	// The provider parent timestamp is authoritative even when an older local
	// child-derived value had inflated activity beyond it.
	assert.True(got.LastActivityAt.Equal(updatedAt), "LastActivityAt should use provider updatedAt")
}

func TestUpdateMRTitleBodyIgnoresStaleUpdate(t *testing.T) {
	assert := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	base := baseTime()

	repoID := insertTestRepo(t, d, "owner3", "repo3")
	newerUpdatedAt := base.Add(1 * time.Hour)
	mr := &MergeRequest{
		RepoID:         repoID,
		PlatformID:     77777,
		Number:         3,
		URL:            "https://github.com/owner3/repo3/pull/3",
		Title:          "current title",
		Author:         "author",
		State:          "open",
		Body:           "current body",
		HeadBranch:     "feature",
		BaseBranch:     "main",
		CreatedAt:      base,
		UpdatedAt:      newerUpdatedAt,
		LastActivityAt: newerUpdatedAt,
	}
	id, err := d.UpsertMergeRequest(ctx, mr)
	assert.NoError(err)

	// Stale update: updatedAt is older than existing row.
	staleAt := base.Add(30 * time.Minute)
	assert.NoError(d.UpdateMRTitleBody(ctx, id, "stale title", "stale body", staleAt))

	got, err := d.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 3)
	assert.NoError(err)
	assert.NotNil(got)
	assert.Equal("current title", got.Title, "stale update should be ignored")
	assert.Equal("current body", got.Body, "stale update should be ignored")
	assert.True(got.UpdatedAt.Equal(newerUpdatedAt), "updated_at should not regress")
}

func TestHTTPEtagPersistence(t *testing.T) {
	assert := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	etag, err := d.GetHTTPEtag(
		ctx, "github", "github.com", "OWNER", "Repo",
		"pull_request", 7,
	)
	assert.NoError(err)
	assert.Empty(etag)

	assert.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "OWNER", "Repo",
		"pull_request", 7, `"etag-v1"`,
	))
	etag, err = d.GetHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 7,
	)
	assert.NoError(err)
	assert.Equal(`"etag-v1"`, etag)

	assert.NoError(d.UpsertHTTPEtag(
		ctx, "github", "github.com", "owner", "repo",
		"pull_request", 7, `"etag-v2"`,
	))
	etag, err = d.GetHTTPEtag(
		ctx, "github", "github.com", "OWNER", "Repo",
		"pull_request", 7,
	)
	assert.NoError(err)
	assert.Equal(`"etag-v2"`, etag)
}

func TestUpsertHTTPEtagIfRouteFenceRejectsConcurrentPathReuse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := openTestDB(t)
	observedAt := time.Now().UTC()
	originalIdentity := RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "original-repo",
		Owner: "acme", Name: "alpha", RepoPath: "acme/alpha",
	}
	original, _, err := database.ReconcileRepositoryObservation(
		ctx, originalIdentity, observedAt,
	)
	require.NoError(err)
	fence, found, err := database.CurrentRepositoryRouteFence(
		ctx, originalIdentity, original.Repository.ID,
	)
	require.NoError(err)
	require.True(found)

	started := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		committed bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		close(started)
		<-release
		committed, upsertErr := database.UpsertHTTPEtagIfRouteFence(
			ctx, originalIdentity, fence,
			"pull_request", 7, `"stale-etag"`,
		)
		done <- result{committed: committed, err: upsertErr}
	}()
	<-started
	_, _, err = database.ReconcileRepositoryObservation(ctx, RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "original-repo",
		Owner: "acme", Name: "beta", RepoPath: "acme/beta",
	}, observedAt.Add(time.Minute))
	require.NoError(err)
	_, _, err = database.ReconcileRepositoryObservation(ctx, RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "replacement-repo",
		Owner: "acme", Name: "alpha", RepoPath: "acme/alpha",
	}, observedAt.Add(2*time.Minute))
	require.NoError(err)
	_, _, err = database.ReconcileRepositoryObservation(ctx, RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "replacement-repo",
		Owner: "acme", Name: "gamma", RepoPath: "acme/gamma",
	}, observedAt.Add(3*time.Minute))
	require.NoError(err)
	_, _, err = database.ReconcileRepositoryObservation(ctx, originalIdentity,
		observedAt.Add(4*time.Minute))
	require.NoError(err)
	close(release)
	got := <-done
	require.NoError(got.err)
	assert.False(got.committed)

	etag, err := database.GetHTTPEtag(
		ctx, "github", "github.com", "acme", "alpha", "pull_request", 7,
	)
	require.NoError(err)
	assert.Empty(etag)
}

func TestUpsertIssue_StoresAssignees(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "owner", "repo")

	issue := &Issue{
		RepoID:         repoID,
		PlatformID:     123,
		Number:         42,
		URL:            "https://github.com/owner/repo/issues/42",
		Title:          "Test issue",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  `["alice","bob"]`,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}

	_, err := d.UpsertIssue(ctx, issue)
	require.NoError(err)

	// Verify stored value
	var stored string
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT assignees_json FROM forge_issues WHERE repo_id = ? AND number = ?`,
		repoID, 42,
	).Scan(&stored)
	require.NoError(err)
	assert.JSONEq(`["alice","bob"]`, stored)
}

func TestListIssues_FilterByAssignee(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "owner", "repo")

	// Issue assigned to alice
	_, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/issues/1",
		Title:          "Issue 1",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  `["alice"]`,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	// Issue assigned to bob
	_, err = d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     2,
		Number:         2,
		URL:            "https://github.com/owner/repo/issues/2",
		Title:          "Issue 2",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  `["bob"]`,
		CreatedAt:      now,
		UpdatedAt:      now.Add(time.Minute),
		LastActivityAt: now.Add(time.Minute),
	})
	require.NoError(err)

	// Issue assigned to both
	_, err = d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     3,
		Number:         3,
		URL:            "https://github.com/owner/repo/issues/3",
		Title:          "Issue 3",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  `["alice","bob"]`,
		CreatedAt:      now,
		UpdatedAt:      now.Add(2 * time.Minute),
		LastActivityAt: now.Add(2 * time.Minute),
	})
	require.NoError(err)

	// Filter by alice
	issues, err := d.ListIssues(ctx, ListIssuesOpts{Assignee: "alice", State: "all"})
	require.NoError(err)
	assert.Len(issues, 2)
	numbers := []int{issues[0].Number, issues[1].Number}
	assert.ElementsMatch([]int{1, 3}, numbers)

	// Filter by bob
	issues, err = d.ListIssues(ctx, ListIssuesOpts{Assignee: "bob", State: "all"})
	require.NoError(err)
	assert.Len(issues, 2)
	numbers = []int{issues[0].Number, issues[1].Number}
	assert.ElementsMatch([]int{2, 3}, numbers)
}

func TestListIssues_PopulatesAssignees(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "owner", "repo")

	_, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/issues/1",
		Title:          "Issue 1",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  `["alice","bob"]`,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	issues, err := d.ListIssues(ctx, ListIssuesOpts{State: "all"})
	require.NoError(err)
	require.Len(issues, 1)
	require.Equal([]string{"alice", "bob"}, issues[0].Assignees)
}

// TestUpsertIssue_NormalizesEmptyAssigneesJSON verifies that an empty
// AssigneesJSON (Go zero value) is stored as '[]' instead of an empty string, so that
// json_each-based filters (e.g. ListIssues with Assignee) don't choke on
// malformed JSON. Repro for roborev finding on commit 2b9ca4d.
func TestUpsertIssue_NormalizesEmptyAssigneesJSON(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	now := baseTime()
	repoID := insertTestRepo(t, d, "owner", "repo")

	// Insert with empty AssigneesJSON to simulate paths that don't set it.
	_, err := d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/issues/1",
		Title:          "Issue 1",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  "",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	var stored string
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT assignees_json FROM forge_issues WHERE repo_id = ? AND number = ?`,
		repoID, 1,
	).Scan(&stored)
	require.NoError(err)
	assert.Equal("[]", stored)

	// Re-upsert with empty value should also remain valid JSON, not "".
	_, err = d.UpsertIssue(ctx, &Issue{
		RepoID:         repoID,
		PlatformID:     1,
		Number:         1,
		URL:            "https://github.com/owner/repo/issues/1",
		Title:          "Issue 1 updated",
		Author:         "author",
		State:          "open",
		AssigneesJSON:  "",
		CreatedAt:      now,
		UpdatedAt:      now.Add(time.Minute),
		LastActivityAt: now.Add(time.Minute),
	})
	require.NoError(err)
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT assignees_json FROM forge_issues WHERE repo_id = ? AND number = ?`,
		repoID, 1,
	).Scan(&stored)
	require.NoError(err)
	assert.Equal("[]", stored)

	// Filtering should still work and return no rows for any assignee
	// without raising "malformed JSON".
	issues, err := d.ListIssues(ctx, ListIssuesOpts{Assignee: "anyone", State: "all"})
	require.NoError(err)
	assert.Empty(issues)
}

func TestPeriodicSyncCandidatesExcludeRemovedUpstream(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "acme", "widget")
	base := baseTime()

	insertTestMR(t, d, repoID, 1, "visible pull", base)
	insertTestMR(t, d, repoID, 2, "removed pull", base)
	insertTestIssue(t, d, repoID, 3, "visible issue", base)
	insertTestIssue(t, d, repoID, 4, "removed issue", base)
	mergedAt := base.Add(time.Hour)
	insertTestMRWithOptions(t, d, testMR(repoID, 5, func(mr *MergeRequest) {
		mr.State = MergeRequestStateMerged
		mr.MergedAt = &mergedAt
	}))
	insertTestMRWithOptions(t, d, testMR(repoID, 6, func(mr *MergeRequest) {
		mr.State = MergeRequestStateMerged
		mr.MergedAt = &mergedAt
	}))

	for _, item := range []struct {
		itemType ArchiveItemType
		number   int
	}{
		{itemType: ArchiveItemTypeMergeRequest, number: 2},
		{itemType: ArchiveItemTypeIssue, number: 4},
		{itemType: ArchiveItemTypeMergeRequest, number: 6},
	} {
		_, err := d.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
			repoID, item.itemType, item.number,
			fmt.Sprintf("%s-%d", item.itemType, item.number), base, base,
		)
		require.NoError(err)
	}

	closedMRs, err := d.GetPreviouslyOpenMRNumbers(ctx, repoID, map[int]bool{})
	require.NoError(err)
	require.Equal([]int{1}, closedMRs)
	closedIssues, err := d.GetPreviouslyOpenIssueNumbers(ctx, repoID, map[int]bool{})
	require.NoError(err)
	require.Equal([]int{3}, closedIssues)
	missingActors, err := d.GetMergedMRNumbersMissingMergedActor(
		ctx, repoID, base,
		MergedMRMissingActorCursor{
			MergedAt: mergedAt.Add(time.Hour), MergeRequestID: 1<<63 - 1,
		},
		10,
	)
	require.NoError(err)
	require.Len(missingActors, 1)
	require.Equal(5, missingActors[0].Number)
}

func TestGetMergedMRNumbersMissingMergedActor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "acme", "widget")
	base := baseTime()
	mergedOpt := func(at time.Time) testMROpt {
		return func(mr *MergeRequest) {
			mr.State = MergeRequestStateMerged
			mr.MergedAt = &at
		}
	}

	// #1: merged through forge — state merged, no events at all.
	insertTestMRWithOptions(t, d, testMR(repoID, 1, mergedOpt(base.Add(time.Hour))))
	// #2: merged with the actor already recorded.
	id2 := insertTestMRWithOptions(t, d, testMR(repoID, 2, mergedOpt(base.Add(2*time.Hour))))
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: id2,
		EventType:      "merged",
		Author:         "merge-admin",
		Summary:        "merged this",
		CreatedAt:      base.Add(2 * time.Hour),
		DedupeKey:      "evt-2",
	}}))
	// #3: still open.
	insertTestMR(t, d, repoID, 3, "open", base)
	// #4: merged with an actor-less merged event — still needs the actor.
	id4 := insertTestMRWithOptions(t, d, testMR(repoID, 4, mergedOpt(base.Add(3*time.Hour))))
	require.NoError(d.UpsertMREvents(ctx, []MREvent{{
		MergeRequestID: id4,
		EventType:      "merged",
		Summary:        "merged this",
		CreatedAt:      base.Add(3 * time.Hour),
		DedupeKey:      "evt-4",
	}}))
	// #5: merged before the cutoff — out of scope.
	insertTestMRWithOptions(t, d, testMR(repoID, 5, mergedOpt(base.Add(-time.Hour))))

	horizon := base.Add(100 * time.Hour)
	rows, err := d.GetMergedMRNumbersMissingMergedActor(ctx, repoID, base,
		MergedMRMissingActorCursor{MergedAt: horizon, MergeRequestID: 1<<63 - 1}, 10)
	require.NoError(err)
	require.Len(rows, 2)
	assert.Equal(4, rows[0].Number)
	assert.Equal(base.Add(3*time.Hour), rows[0].MergedAt)
	assert.Equal(1, rows[1].Number)
	assert.Equal(base.Add(time.Hour), rows[1].MergedAt)

	limited, err := d.GetMergedMRNumbersMissingMergedActor(ctx, repoID, base,
		MergedMRMissingActorCursor{MergedAt: horizon, MergeRequestID: 1<<63 - 1}, 1)
	require.NoError(err)
	require.Len(limited, 1)
	assert.Equal(4, limited[0].Number)

	// The mergedBefore bound is the sweep cursor: excluding the newest
	// candidate's merged_at must surface the older candidate it was hiding.
	older, err := d.GetMergedMRNumbersMissingMergedActor(
		ctx, repoID, base,
		MergedMRMissingActorCursor{
			MergedAt:       rows[0].MergedAt,
			MergeRequestID: rows[0].MergeRequestID,
		}, 10,
	)
	require.NoError(err)
	require.Len(older, 1)
	assert.Equal(1, older[0].Number)
}

func TestGetMergedMRNumbersMissingMergedActorPaginatesTiedTimestamps(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	d := openTestDB(t)

	repoID := insertTestRepo(t, d, "acme", "widget")
	mergedAt := baseTime().Add(time.Hour)
	for number := 1; number <= 12; number++ {
		insertTestMRWithOptions(t, d, testMR(repoID, number, func(mr *MergeRequest) {
			mr.State = MergeRequestStateMerged
			mr.MergedAt = &mergedAt
		}))
	}

	first, err := d.GetMergedMRNumbersMissingMergedActor(
		ctx, repoID, baseTime(), MergedMRMissingActorCursor{
			MergedAt: mergedAt.Add(time.Hour), MergeRequestID: 1<<63 - 1,
		}, 10,
	)
	require.NoError(err)
	require.Len(first, 10)

	second, err := d.GetMergedMRNumbersMissingMergedActor(
		ctx, repoID, baseTime(), MergedMRMissingActorCursor{
			MergedAt:       first[len(first)-1].MergedAt,
			MergeRequestID: first[len(first)-1].MergeRequestID,
		}, 10,
	)
	require.NoError(err)
	require.Len(second, 2,
		"the next page must retain rows tied with the previous page's timestamp")
}
