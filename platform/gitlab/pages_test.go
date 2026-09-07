package gitlab

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghsync "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
)

func gitLabPagesTestRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindGitLab, Host: "gitlab.example.com",
		Owner: "group", Name: "project", RepoPath: "group/project", PlatformID: 42,
		PlatformExternalID: "42", WebURL: "https://gitlab.example.com/group/project",
	}
}

func TestGitLabInventoryUsesAllStatesOldestFirstAndBoundedPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	var issueKeysetCursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("all", r.URL.Query().Get("state"))
		assert.Equal("asc", r.URL.Query().Get("sort"))
		assert.Equal("created_at", r.URL.Query().Get("order_by"))
		assert.Equal("100", r.URL.Query().Get("per_page"))
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/issues":
			assert.Equal("keyset", r.URL.Query().Get("pagination"))
			issueKeysetCursors = append(issueKeysetCursors, r.URL.Query().Get("cursor"))
			if r.URL.Query().Get("cursor") == "" {
				w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/42/issues?pagination=keyset&order_by=created_at&sort=asc&cursor=tie-break-1>; rel="next"`)
			}
			writeJSON(w, `[{"id":101,"iid":1,"title":"issue","state":"closed","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()
	historical := platform.ItemPageQuery{Order: platform.ItemOrderCreated}

	issues, err := client.ListIssuesPage(t.Context(), ref, historical)
	require.NoError(err)
	require.Len(issues.Items, 1)
	assert.Equal(1, issues.Items[0].Number)
	assert.False(issues.Exhausted)
	assert.NotEmpty(issues.NextCursor)
	resumed := historical
	resumed.Cursor = issues.NextCursor
	issues2, err := client.ListIssuesPage(t.Context(), ref, resumed)
	require.NoError(err)
	assert.True(issues2.Exhausted)
	assert.Equal([]string{"", "tie-break-1"}, issueKeysetCursors,
		"resumption must replay the provider's keyset continuation parameters")

	assert.Equal(2, requests)
}

// TestGitLabUpdatedInventoryBindsCursorToQueryShape proves a maintenance cursor
// only resumes the exact enumeration that produced it: watermark, repository,
// and host all participate in the binding.
func TestGitLabUpdatedInventoryBindsCursorToQueryShape(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/42/issues?pagination=keyset&cursor=tie-break-1>; rel="next"`)
		writeJSON(w, `[{"id":101,"iid":1,"title":"issue","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:04Z"}]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()
	watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	query := func(since time.Time, cursor string) platform.ItemPageQuery {
		return platform.ItemPageQuery{
			Order:        platform.ItemOrderUpdated,
			UpdatedSince: &since, Cursor: cursor,
		}
	}

	issues, err := client.ListIssuesPage(t.Context(), ref, query(watermark, ""))
	require.NoError(err)
	require.NotEmpty(issues.NextCursor)

	_, err = client.ListIssuesPage(t.Context(), ref, query(watermark.Add(time.Second), issues.NextCursor))
	require.ErrorIs(err, platform.ErrProviderContract)
	otherRepo := ref
	otherRepo.Name = "other"
	otherRepo.RepoPath = "group/other"
	otherRepo.PlatformID = 43
	_, err = client.ListIssuesPage(t.Context(), otherRepo, query(watermark, issues.NextCursor))
	require.ErrorIs(err, platform.ErrProviderContract)
	// Ref hydration pins every read to the client's own host, so cross-host
	// replay is guarded by the minting client's identity: a client for a
	// different GitLab instance must refuse the cursor.
	otherHostClient, err := NewClient(
		"other.gitlab.example.com", testTokenSource("token"),
		WithBaseURLForTesting(server.URL+"/api/v4"), WithoutRetriesForTesting(), WithTransport(http.DefaultTransport))
	require.NoError(err)
	otherHostRef := ref
	otherHostRef.Host = "other.gitlab.example.com"
	_, err = otherHostClient.ListIssuesPage(t.Context(), otherHostRef, query(watermark, issues.NextCursor))
	require.ErrorIs(err, platform.ErrProviderContract)
	_, err = client.ListIssuesPage(t.Context(), ref, query(watermark, issues.NextCursor))
	require.NoError(err)
}

// TestGitLabUpdatedMergeRequestsDoNotSkipItemsMovedBetweenPages proves the
// descending offset maintenance traversal keeps mid-scan updates inside the
// consumed prefix so offset pagination cannot permanently skip an unseen item.
func TestGitLabUpdatedMergeRequestsDoNotSkipItemsMovedBetweenPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/api/v4/projects/42/merge_requests", r.URL.EscapedPath())
		assert.Equal("desc", r.URL.Query().Get("sort"))
		if requests == 1 {
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, `[{"id":202,"iid":2,"title":"newest","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:06Z"}]`)
			return
		}
		// Item 2 changed after page one was read. Descending pagination
		// keeps it in the consumed prefix, so item 1 remains on page two.
		writeJSON(w, `[{"id":201,"iid":1,"title":"unseen","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:05Z"}]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()
	query := func(cursor string) platform.ItemPageQuery {
		return platform.ItemPageQuery{
			Order:        platform.ItemOrderUpdated,
			UpdatedSince: &watermark, Cursor: cursor,
		}
	}

	first, err := client.ListMergeRequestsPage(t.Context(), ref, query(""))
	require.NoError(err)
	require.Len(first.Items, 1)
	assert.Equal(2, first.Items[0].Number)
	assert.False(first.Exhausted)
	second, err := client.ListMergeRequestsPage(t.Context(), ref, query(first.NextCursor))
	require.NoError(err)
	require.Len(second.Items, 1)
	assert.Equal(1, second.Items[0].Number)
	assert.True(second.Exhausted)
	assert.Equal(2, requests)
}

// TestGitLabUpdatedIssuesReserveMidScanMovesThroughKeysetCursor proves the
// ascending keyset maintenance traversal re-serves an item whose updated_at
// moved mid-scan: under a keyset cursor the bump only moves it forward past
// the cursor, so it reappears later in the same scan instead of being
// skipped.
func TestGitLabUpdatedIssuesReserveMidScanMovesThroughKeysetCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/api/v4/projects/42/issues", r.URL.EscapedPath())
		assert.Equal("keyset", r.URL.Query().Get("pagination"))
		assert.Equal("asc", r.URL.Query().Get("sort"))
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/42/issues?pagination=keyset&cursor=after-issue-1>; rel="next"`)
			writeJSON(w, `[{"id":201,"iid":1,"title":"oldest update","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:05Z"}]`)
			return
		}
		// Issue 1 was updated again after page one was consumed: the keyset
		// cursor keeps traversal position, so the moved item is re-served on
		// a later page alongside the still-unseen issue 2.
		assert.Equal("after-issue-1", r.URL.Query().Get("cursor"))
		writeJSON(w, `[
			{"id":202,"iid":2,"title":"unseen","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:06Z"},
			{"id":201,"iid":1,"title":"moved forward","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:07Z"}
		]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()
	query := func(cursor string) platform.ItemPageQuery {
		return platform.ItemPageQuery{
			Order:        platform.ItemOrderUpdated,
			UpdatedSince: &watermark, Cursor: cursor,
		}
	}

	first, err := client.ListIssuesPage(t.Context(), ref, query(""))
	require.NoError(err)
	require.Len(first.Items, 1)
	assert.Equal(1, first.Items[0].Number)
	assert.False(first.Exhausted)
	second, err := client.ListIssuesPage(t.Context(), ref, query(first.NextCursor))
	require.NoError(err)
	require.Len(second.Items, 2)
	assert.Equal(2, second.Items[0].Number)
	assert.Equal(1, second.Items[1].Number, "moved item is re-served, never skipped")
	assert.True(second.Exhausted)
	assert.Equal(2, requests)
}

func TestGitLabIssueKeysetCursorReplaysOnlyContinuationToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var resumeQueries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link",
				`<https://evil.example.com/api/v4/projects/999/issues?cursor=tok-1&order_by=updated_at&sort=desc&per_page=1&state=opened&updated_after=2030-01-01T00:00:00Z&pagination=offset>; rel="next"`)
		} else {
			resumeQueries = append(resumeQueries, r.URL.Query())
		}
		writeJSON(w, `[{"id":101,"iid":1,"title":"issue","state":"closed","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()
	historical := platform.ItemPageQuery{Order: platform.ItemOrderCreated}

	first, err := client.ListIssuesPage(t.Context(), ref, historical)
	require.NoError(err)
	require.NotEmpty(first.NextCursor)
	resumed := historical
	resumed.Cursor = first.NextCursor
	_, err = client.ListIssuesPage(t.Context(), ref, resumed)
	require.NoError(err)

	require.Len(resumeQueries, 1)
	query := resumeQueries[0]
	assert.Equal("tok-1", query.Get("cursor"), "the continuation token is replayed")
	assert.Equal("created_at", query.Get("order_by"), "order comes from the validated cursor, not the link")
	assert.Equal("asc", query.Get("sort"))
	assert.Equal("100", query.Get("per_page"))
	assert.Equal("all", query.Get("state"))
	assert.Equal("keyset", query.Get("pagination"))
	assert.Empty(query.Get("updated_after"), "a smuggled watermark must not survive")

	// A tampered token stays one opaque cursor parameter: it cannot be split
	// into extra query parameters that reshape the request.
	tampered, err := encodeGitLabPageCursor(gitLabPageCursor{
		Mode: "historical_issues", Host: ref.Host, RepoPath: "group/project",
		KeysetCursor: "evil&sort=desc",
	})
	require.NoError(err)
	resumed.Cursor = tampered
	_, err = client.ListIssuesPage(t.Context(), ref, resumed)
	require.NoError(err)
	require.Len(resumeQueries, 2)
	query = resumeQueries[1]
	assert.Equal("evil&sort=desc", query.Get("cursor"))
	assert.Equal("asc", query.Get("sort"))

	// A cursor carrying a full provider link (the previous cursor schema, or
	// a hand-crafted URL) does not carry keyset continuation state.
	legacy := base64.RawURLEncoding.EncodeToString([]byte(
		`{"mode":"historical_issues","host":"gitlab.example.com","repo_path":"group/project",` +
			`"link":"https://evil.example.com/api/v4/projects/999/issues?cursor=tok-1&sort=desc"}`,
	))
	resumed.Cursor = legacy
	_, err = client.ListIssuesPage(t.Context(), ref, resumed)
	require.ErrorIs(err, platform.ErrProviderContract)
}

// TestGitLabIssueKeysetUnsupportedServerReturnsTypedError proves a server
// that ignores the keyset request (GitLab before 18.3 for project issues)
// and answers with offset pagination is detected by its response shape and
// rejected with a typed unsupported_capability error instead of silently
// degrading to a skippable offset traversal. A single complete page is
// accepted: with no continuation needed, both pagination modes serve the
// identical full result.
func TestGitLabIssueKeysetUnsupportedServerReturnsTypedError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	multiPage := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The oldest supported response shape: offset headers and a
		// page-numbered next link, no keyset cursor parameter.
		w.Header().Set("X-Page", "1")
		w.Header().Set("X-Per-Page", "100")
		if multiPage {
			w.Header().Set("X-Total", "250")
			w.Header().Set("X-Total-Pages", "3")
			w.Header().Set("X-Next-Page", "2")
			w.Header().Set("Link",
				`<https://gitlab.example.com/api/v4/projects/42/issues?page=2&per_page=100&order_by=created_at&sort=asc>; rel="next"`)
		} else {
			w.Header().Set("X-Total", "1")
			w.Header().Set("X-Total-Pages", "1")
		}
		writeJSON(w, `[{"id":101,"iid":1,"title":"issue","state":"closed","created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-01T02:03:04Z"}]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()

	_, err := client.ListIssuesPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated,
	})
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = client.ListIssuesPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated, UpdatedSince: &watermark,
	})
	require.ErrorIs(err, platform.ErrUnsupportedCapability,
		"the maintenance traversal shares the keyset requirement")

	multiPage = false
	single, err := client.ListIssuesPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated,
	})
	require.NoError(err, "a complete single page needs no continuation and is accepted")
	assert.True(single.Exhausted)
	require.Len(single.Items, 1)
}

func TestGitLabPaginationChargesEveryMarkedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Link", `<https://gitlab.example.com/api/v4/projects/42/issues?pagination=keyset&cursor=next>; rel="next"`)
		}
		writeJSON(w, `[{"id":101,"iid":1,"title":"issue","state":"opened","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}]`)
	}))
	defer server.Close()
	budget := ghsync.NewSyncBudget(100)
	client := newTestClient(t, server.URL, WithTransport(ghsync.WrapSyncBudgetTransport(http.DefaultTransport, budget)))
	ctx := ghsync.WithSyncBudget(t.Context())
	historical := platform.ItemPageQuery{Order: platform.ItemOrderCreated}

	first, err := client.ListIssuesPage(ctx, gitLabPagesTestRef(), historical)
	require.NoError(err)
	resumed := historical
	resumed.Cursor = first.NextCursor
	_, err = client.ListIssuesPage(ctx, gitLabPagesTestRef(), resumed)
	require.NoError(err)
	assert.Equal(2, requests)
	assert.Equal(2, budget.Spent())
}
func TestGitLabLiveIssueEventsCollectCanonicalCommentPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var discussionPages []string
	var relatedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/issues/7/discussions":
			discussionPages = append(discussionPages, r.URL.Query().Get("page"))
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(w, `[{"id":"first","notes":[
					{"id":301,"body":"first comment","author":{"username":"ivy"},"created_at":"2026-07-01T00:00:00Z"},
					{"id":303,"body":"closed","system":true,"author":{"username":"closer"},"created_at":"2026-07-01T12:00:00Z"}
				]}]`)
				return
			}
			writeJSON(w, `[{"id":"second","notes":[{"id":302,"body":"second comment","author":{"username":"joe"},"created_at":"2026-07-02T00:00:00Z"}]}]`)
		case "/api/v4/projects/42/issues/7/related_merge_requests":
			relatedPages = append(relatedPages, r.URL.Query().Get("page"))
			writeJSON(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := gitLabPagesTestRef()

	events, err := client.ListIssueEvents(t.Context(), ref, 7)
	require.NoError(err)
	require.Len(events, 3)
	assert.Equal("first comment", events[0].Body)
	assert.Equal("closed", events[1].EventType)
	assert.Equal("closer", events[1].Author)
	assert.Equal("second comment", events[2].Body)
	assert.Equal([]string{"1", "2"}, discussionPages)
	assert.Equal([]string{"1"}, relatedPages)

	discussionPages = nil
	comments, err := client.ListIssueComments(t.Context(), ref, 7)
	require.NoError(err)
	require.Len(comments, 2)
	assert.Equal("first comment", comments[0].Body)
	assert.Equal("second comment", comments[1].Body)
	assert.Equal([]string{"1", "2"}, discussionPages)
	assert.Equal([]string{"1"}, relatedPages)
}
func TestGitLabArchiveCapabilities(t *testing.T) {
	assert.Equal(t, platform.ArchiveCapabilities{
		HistoricalIssues: true,
		OrdinaryComments: true, InlineReviewComments: true,
	}, newTestClient(t, "http://127.0.0.1:1").Capabilities().Archive)
}
