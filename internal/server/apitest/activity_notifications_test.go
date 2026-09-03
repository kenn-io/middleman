package apitest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
)

// setupNotificationsAPIServer builds an API server with a non-nil config so
// notifications are enabled (they are always on once a config is loaded). The
// shared setupTestServer passes a nil config, which trips the nil-config
// safety guard and excludes notification rows from /activity.
func setupNotificationsAPIServer(t *testing.T) (*server.Server, *db.DB) {
	t.Helper()

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, defaultTestRepos, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)

	// A non-nil config enables notifications; the explicit HostCheck override
	// (which short-circuits config-derived host options) keeps the apitest
	// "forge.test" base URL accepted.
	srv := servertest.New(t, database, syncer, nil, "/", &config.Config{}, server.ServerOptions{
		HostCheck: server.HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
			Allowed: []config.HostKey{
				{Host: "forge.test", Port: ""},
				{Host: "example.com", Port: ""},
			},
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})

	return srv, database
}

// seedNotification inserts one notification row directly, bypassing sync, so
// the test exercises the activity query's own anchoring/author guards at the
// real SQLite boundary. Direct insertion mirrors rows that an older,
// pre-filter sync may have already persisted into forge_notification_items
// and that Activity must still hide.
func seedNotification(t *testing.T, database *db.DB, n db.Notification) {
	t.Helper()
	if n.SourceUpdatedAt.IsZero() {
		n.SourceUpdatedAt = time.Now().UTC().Truncate(time.Second)
	}
	if n.SyncedAt.IsZero() {
		n.SyncedAt = n.SourceUpdatedAt
	}
	require.NoError(t, database.UpsertNotifications(t.Context(), []db.Notification{n}))
}

func activityItemKey(it generated.ActivityItemResponse) string {
	return it.ItemType + ":" + strconv.FormatInt(it.ItemNumber, 10)
}

// TestActivityNotificationsFullStack drives the notification-in-activity
// behavior end to end over real /api/v1/activity and SQLite, asserting that
// the feed:
//   - returns only notification rows in a notifications-only view (no PR/issue
//     "Opened" anchor rows reintroduced),
//   - never surfaces unanchored ("ISSUE #0") or author ("Your thread")
//     notifications, even though they are persisted, and
//   - carries the linked PR/issue lifecycle state in subject_state so Hide
//     closed/merged can drop notifications on merged/closed subjects without a
//     sibling PR row loaded.
//
// This is the full-stack coverage (DB query -> Huma mapping -> OpenAPI
// serialization -> generated client) that the DB-unit and Svelte-component
// tests cannot give on their own.
func TestActivityNotificationsFullStack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupNotificationsAPIServer(t)
	client := setupTestClient(t, srv)
	ctx := t.Context()

	// Anchored, kept notification on an open PR.
	seedPR(t, database, "acme", "widget", 1)
	number1 := 1
	seedNotification(t, database, db.Notification{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "ntf-open",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "PullRequest", SubjectTitle: "Open PR",
		WebURL:     "https://github.com/acme/widget/pull/1",
		ItemNumber: &number1, ItemType: "pr", ItemAuthor: "carol",
		Reason: "review_requested", Unread: true,
	})

	// Anchored notification on a merged PR: it still appears unread, but
	// subject_state must report "merged" so Hide closed/merged can drop it.
	seedPR(t, database, "acme", "widget", 2, withSeedPRState(db.MergeRequestStateMerged))
	number2 := 2
	seedNotification(t, database, db.Notification{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "ntf-merged",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "PullRequest", SubjectTitle: "Merged PR",
		WebURL:     "https://github.com/acme/widget/pull/2",
		ItemNumber: &number2, ItemType: "pr", ItemAuthor: "carol",
		Reason: "review_requested", Unread: true,
	})

	// Unanchored CI/CheckSuite notification (no PR/issue, nil number): the
	// bogus "ISSUE #0" regression. Persisted, but must never reach the feed.
	seedNotification(t, database, db.Notification{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "ntf-ci",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "CheckSuite", SubjectTitle: "CI failed on main",
		ItemType: "", ItemNumber: nil, ItemAuthor: "",
		Reason: "ci_activity", Unread: true,
	})

	// Author notification on an open PR: "Your thread" feed duplication. The
	// PR row itself still exists, but its author notification is dropped.
	seedPR(t, database, "acme", "widget", 3)
	number3 := 3
	seedNotification(t, database, db.Notification{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "ntf-author",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "PullRequest", SubjectTitle: "Authored PR",
		WebURL:     "https://github.com/acme/widget/pull/3",
		ItemNumber: &number3, ItemType: "pr", ItemAuthor: "testuser",
		Reason: "author", Unread: true,
	})

	// Anchored notification whose parent has not synced yet. Its lifecycle is
	// unknown, so Hide closed/merged must retain it.
	number4 := 4
	seedNotification(t, database, db.Notification{
		Platform: "github", PlatformHost: "github.com",
		PlatformNotificationID: "ntf-unknown-state",
		RepoOwner:              "acme", RepoName: "widget",
		SubjectType: "PullRequest", SubjectTitle: "Unsynced PR",
		WebURL:     "https://github.com/acme/widget/pull/4",
		ItemNumber: &number4, ItemType: "pr", ItemAuthor: "carol",
		Reason: "mention", Unread: true,
	})

	// --- notifications-only view ---
	notifResp, err := client.HTTP.ListActivityWithResponse(ctx, &generated.ListActivityParams{
		Types: &[]string{"notification"},
	})
	require.NoError(err)
	require.Equal(200, notifResp.StatusCode())
	require.NotNil(notifResp.JSON200)
	require.NotNil(notifResp.JSON200.Items)

	notifByKey := map[string]generated.ActivityItemResponse{}
	for _, it := range notifResp.JSON200.Items {
		// A notifications-only filter must not reintroduce new_pr/new_issue
		// "Opened" anchor rows.
		assert.Equal("notification", it.ActivityType,
			"notifications-only feed leaked a %q row", it.ActivityType)
		assert.NotZero(it.ItemNumber, "unanchored notification leaked into activity")
		notifByKey[activityItemKey(it)] = it
	}

	// Only the three anchored, non-author notifications survive.
	assert.Len(notifByKey, 3)
	assert.Contains(notifByKey, "pr:1")
	assert.Contains(notifByKey, "pr:2")
	assert.Contains(notifByKey, "pr:4")
	assert.NotContains(notifByKey, "pr:3", "author notification must be dropped from activity")

	merged, ok := notifByKey["pr:2"]
	require.True(ok)
	assert.Equal("unread", merged.ItemState, "notification keeps its own unread state")
	require.NotNil(merged.SubjectState)
	assert.Equal("merged", *merged.SubjectState, "merged PR state must ride in subject_state")

	open := notifByKey["pr:1"]
	require.NotNil(open.SubjectState)
	assert.Equal("open", *open.SubjectState)
	assert.Nil(notifByKey["pr:4"].SubjectState, "unsynced subject lifecycle must remain unknown")

	// --- default collapsed view ---
	// Synced subjects are represented by authoritative parent summaries, but an
	// unsynced notification must remain as a directly rendered item row.
	collapsedProjection := generated.ListActivityParamsProjectionCollapsed
	collapsedResp, err := client.HTTP.ListActivityWithResponse(ctx, &generated.ListActivityParams{
		Projection: &collapsedProjection,
	})
	require.NoError(err)
	require.Equal(200, collapsedResp.StatusCode())
	require.NotNil(collapsedResp.JSON200)
	require.NotNil(collapsedResp.JSON200.Items)
	require.Len(collapsedResp.JSON200.Items, 1)
	assert.Equal("notification", collapsedResp.JSON200.Items[0].ActivityType)
	assert.Equal("pr:4", activityItemKey(collapsedResp.JSON200.Items[0]))

	// --- hide closed/merged notifications ---
	hideClosedMerged := true
	limit := int64(10)
	visibleResp, err := client.HTTP.ListActivityWithResponse(ctx, &generated.ListActivityParams{
		Types: &[]string{"notification"}, HideClosedMerged: &hideClosedMerged, Limit: &limit,
	})
	require.NoError(err)
	require.Equal(200, visibleResp.StatusCode())
	require.NotNil(visibleResp.JSON200)
	require.NotNil(visibleResp.JSON200.Items)
	visibleKeys := make([]string, 0, len(visibleResp.JSON200.Items))
	for _, it := range visibleResp.JSON200.Items {
		visibleKeys = append(visibleKeys, activityItemKey(it))
	}
	assert.ElementsMatch([]string{"pr:1", "pr:4"}, visibleKeys)

	// --- default (all types) view ---
	// Anchored notifications coexist with the new_pr rows the notifications-only
	// view filtered out, while unanchored/author notifications still never show.
	allResp, err := client.HTTP.ListActivityWithResponse(ctx, &generated.ListActivityParams{})
	require.NoError(err)
	require.Equal(200, allResp.StatusCode())
	require.NotNil(allResp.JSON200.Items)

	var notifRows, newPRRows int
	for _, it := range allResp.JSON200.Items {
		switch it.ActivityType {
		case "notification":
			notifRows++
			assert.NotZero(it.ItemNumber, "unanchored notification leaked into the full feed")
			assert.NotEqual("pr:3", activityItemKey(it), "author notification leaked into the full feed")
		case "new_pr":
			newPRRows++
		}
	}
	assert.Equal(3, notifRows, "only anchored non-author notifications appear in the full feed")
	assert.Equal(3, newPRRows, "all three seeded PRs contribute new_pr rows")
}
