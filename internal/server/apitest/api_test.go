package apitest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
)

func TestAPIListPullsIncludesLabels(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	description := "Needs a fix"
	seedPRWithLabels(t, database, "acme", "widget", 1, []db.Label{{
		Name:        "bug",
		Description: description,
		Color:       "d73a4a",
		IsDefault:   true,
	}})
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ListPullsWithResponse(t.Context(), nil)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.NotNil((*resp.JSON200)[0].Labels)
	require.Equal([]generated.Label{{
		Name:        "bug",
		Description: &description,
		Color:       "d73a4a",
		IsDefault:   true,
	}}, *(*resp.JSON200)[0].Labels)
}

func TestAPIPullsHideRemovedUpstreamArchiveRows(t *testing.T) {
	req := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	inaccessiblePRID := seedPR(t, database, "acme", "widget", 1)
	removedPRID := seedPR(t, database, "acme", "widget", 2)
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	req.NoError(err)
	req.NotNil(repo)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 1, 'inaccessible-pr-1', ?, ?, 'inaccessible'),
			(?, 'merge_request', 2, 'removed-pr-2', ?, ?, 'removed_upstream')`,
		repo.ID, now, now, repo.ID, now, now,
	)
	req.NoError(err)

	client := setupTestClient(t, srv)
	state := "all"
	listed, err := client.HTTP.ListPullsWithResponse(
		ctx, &generated.ListPullsParams{State: &state},
	)
	req.NoError(err)
	req.Equal(http.StatusOK, listed.StatusCode())
	req.NotNil(listed.JSON200)
	req.Len(*listed.JSON200, 1)
	req.EqualValues(1, (*listed.JSON200)[0].Number)

	detail, err := client.HTTP.GetPullWithResponse(
		ctx, "gh", "acme", "widget", 2,
	)
	req.NoError(err)
	req.Equal(http.StatusNotFound, detail.StatusCode())

	stackID, err := database.UpsertStack(ctx, repo.ID, 1, "Synthetic stack")
	req.NoError(err)
	req.NoError(database.ReplaceStackMembers(ctx, stackID, []db.StackMember{
		{MergeRequestID: inaccessiblePRID, Position: 1},
		{MergeRequestID: removedPRID, Position: 2},
	}))

	previewPath := "README.md"
	readPaths := map[string]func() (int, error){
		"commits": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullCommitsWithResponse(
				ctx, "gh", "acme", "widget", 2,
			)
			return resp.StatusCode(), requestErr
		},
		"diff": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullDiffWithResponse(
				ctx, "gh", "acme", "widget", 2, &generated.GetPullDiffParams{},
			)
			return resp.StatusCode(), requestErr
		},
		"file preview": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullFilePreviewWithResponse(
				ctx, "gh", "acme", "widget", 2,
				&generated.GetPullFilePreviewParams{Path: &previewPath},
			)
			return resp.StatusCode(), requestErr
		},
		"files": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullFilesWithResponse(
				ctx, "gh", "acme", "widget", 2,
			)
			return resp.StatusCode(), requestErr
		},
		"import metadata": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullImportMetadataWithResponse(
				ctx, "gh", "acme", "widget", 2,
			)
			return resp.StatusCode(), requestErr
		},
		"review draft": func() (int, error) {
			resp, requestErr := client.HTTP.GetPrReviewDraftWithResponse(
				ctx, "gh", "acme", "widget", 2,
			)
			return resp.StatusCode(), requestErr
		},
		"stack": func() (int, error) {
			resp, requestErr := client.HTTP.GetPullStackWithResponse(
				ctx, "gh", "acme", "widget", 2,
			)
			return resp.StatusCode(), requestErr
		},
	}
	for name, readPath := range readPaths {
		t.Run(name, func(t *testing.T) {
			status, requestErr := readPath()
			require.NoError(t, requestErr)
			assert.Equal(t, http.StatusNotFound, status)
		})
	}

	repoFilter := "acme/widget"
	stacks, err := client.HTTP.ListStacksWithResponse(
		ctx, &generated.ListStacksParams{Repo: &repoFilter},
	)
	req.NoError(err)
	req.Equal(http.StatusOK, stacks.StatusCode())
	req.NotNil(stacks.JSON200)
	req.Len(*stacks.JSON200, 1)
	req.NotNil((*stacks.JSON200)[0].Members)
	req.Len((*stacks.JSON200)[0].Members, 1)
	req.EqualValues(1, ((*stacks.JSON200)[0].Members)[0].Number)
}

func TestAPIActivityAndRepoSummariesHideRemovedUpstreamArchiveRows(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	inaccessiblePRID := seedPR(t, database, "acme", "widget", 1)
	removedPRID := seedPR(t, database, "acme", "widget", 2)
	inaccessibleIssueID := seedIssue(t, database, "acme", "widget", 3, "open")
	removedIssueID := seedIssue(t, database, "acme", "widget", 4, "open")
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID:           "removed-pull-workspace",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   2,
		WorktreePath: t.TempDir(),
		Status:       "ready",
		CreatedAt:    now,
	}))
	require.NotNil(repo)
	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_merge_requests
		SET author = CASE number WHEN 1 THEN 'visible-pr-author' ELSE 'removed-pr-author' END,
		    is_draft = 1,
		    created_at = CASE number WHEN 1 THEN ? ELSE ? END,
		    updated_at = CASE number WHEN 1 THEN ? ELSE ? END,
		    last_activity_at = CASE number WHEN 1 THEN ? ELSE ? END
		WHERE repo_id = ? AND number IN (1, 2)`,
		now.Add(-4*time.Minute), now.Add(-time.Minute),
		now.Add(-4*time.Minute), now.Add(-time.Minute),
		now.Add(-4*time.Minute), now.Add(-time.Minute), repo.ID,
	)
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_issues
		SET author = CASE number WHEN 3 THEN 'visible-issue-author' ELSE 'removed-issue-author' END,
		    title = CASE number WHEN 3 THEN 'Visible issue' ELSE 'Removed issue' END,
		    created_at = CASE number WHEN 3 THEN ? ELSE ? END,
		    updated_at = CASE number WHEN 3 THEN ? ELSE ? END,
		    last_activity_at = CASE number WHEN 3 THEN ? ELSE ? END
		WHERE repo_id = ? AND number IN (3, 4)`,
		now.Add(-3*time.Minute), now,
		now.Add(-3*time.Minute), now,
		now.Add(-3*time.Minute), now, repo.ID,
	)
	require.NoError(err)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{
		{MergeRequestID: inaccessiblePRID, PlatformExternalID: "visible-pr-comment", EventType: "issue_comment", Author: "visible-commenter", CreatedAt: now.Add(-2 * time.Minute), DedupeKey: "visible-pr-comment"},
		{MergeRequestID: removedPRID, PlatformExternalID: "removed-pr-comment", EventType: "issue_comment", Author: "removed-commenter", CreatedAt: now, DedupeKey: "removed-pr-comment"},
	}))
	require.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{
		{IssueID: inaccessibleIssueID, PlatformExternalID: "visible-issue-comment", EventType: "issue_comment", Author: "visible-commenter", CreatedAt: now.Add(-2 * time.Minute), DedupeKey: "visible-issue-comment"},
		{IssueID: removedIssueID, PlatformExternalID: "removed-issue-comment", EventType: "issue_comment", Author: "removed-commenter", CreatedAt: now, DedupeKey: "removed-issue-comment"},
	}))
	_, err = database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 1, 'inaccessible-pr-1', ?, ?, 'inaccessible'),
			(?, 'merge_request', 2, 'removed-pr-2', ?, ?, 'removed_upstream'),
			(?, 'issue', 3, 'inaccessible-issue-3', ?, ?, 'inaccessible'),
			(?, 'issue', 4, 'removed-issue-4', ?, ?, 'removed_upstream')`,
		repo.ID, now, now, repo.ID, now, now,
		repo.ID, now, now, repo.ID, now, now,
	)
	require.NoError(err)

	client := setupTestClient(t, srv)
	since := now.Add(-time.Hour).Format(time.RFC3339)
	activity, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Since: &since},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, activity.StatusCode())
	require.NotNil(activity.JSON200)
	require.NotNil(activity.JSON200.Items)
	require.Len(activity.JSON200.Items, 4)
	for _, item := range activity.JSON200.Items {
		require.NotEqualValues(2, item.ItemNumber)
		require.NotEqualValues(4, item.ItemNumber)
	}
	require.NotNil(activity.JSON200.ItemActivity)
	require.NotEmpty(activity.JSON200.ItemActivity)
	for _, subject := range activity.JSON200.ItemActivity {
		require.NotEqualValues(2, subject.ItemNumber, "removed parents must not surface as parent-only threads")
		require.NotEqualValues(4, subject.ItemNumber, "removed parents must not surface as parent-only threads")
	}
	authors, err := client.HTTP.ListActivityAuthorsWithResponse(
		ctx, &generated.ListActivityAuthorsParams{Since: &since},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, authors.StatusCode())
	require.NotNil(authors.JSON200)
	require.NotNil(authors.JSON200.Authors)
	require.NotContains(authors.JSON200.Authors, "removed-pr-author")
	require.NotContains(authors.JSON200.Authors, "removed-issue-author")

	summaries, err := client.HTTP.ListRepoSummariesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, summaries.StatusCode())
	require.NotNil(summaries.JSON200)
	require.Len(*summaries.JSON200, 1)
	summary := (*summaries.JSON200)[0]
	require.EqualValues(1, summary.CachedPrCount)
	require.EqualValues(1, summary.OpenPrCount)
	require.EqualValues(1, summary.DraftPrCount)
	require.EqualValues(1, summary.CachedIssueCount)
	require.EqualValues(1, summary.OpenIssueCount)
	require.NotNil(summary.ActiveAuthors)
	require.Len(summary.ActiveAuthors, 2)
	require.ElementsMatch(
		[]string{"visible-pr-author", "visible-issue-author"},
		[]string{summary.ActiveAuthors[0].Login, summary.ActiveAuthors[1].Login},
	)
	require.NotNil(summary.RecentIssues)
	require.Len(summary.RecentIssues, 1)
	require.EqualValues(3, summary.RecentIssues[0].Number)
	require.Equal("Visible issue", summary.RecentIssues[0].Title)
}

func TestAPIResolveAndAutocompleteHideOnlyRemovedUpstreamItems(t *testing.T) {
	require := require.New(t)
	srv, database, providerClient, _ := setupTestServerWithFixtureClient(t)
	ctx := t.Context()
	seedPR(t, database, "acme", "widget", 1)
	seedPR(t, database, "acme", "widget", 2)
	seedIssue(t, database, "acme", "widget", 3, "open")
	seedIssue(t, database, "acme", "widget", 4, "open")
	repo, err := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 1, db.ArchiveLifecycleStateInaccessible)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 2, db.ArchiveLifecycleStateRemovedUpstream)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeIssue, 3, db.ArchiveLifecycleStateInaccessible)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeIssue, 4, db.ArchiveLifecycleStateRemovedUpstream)
	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_merge_requests SET author = CASE number
			WHEN 1 THEN 'visible-pull-author' ELSE 'removed-pull-author' END;
		UPDATE forge_issues SET author = CASE number
			WHEN 3 THEN 'visible-issue-author' ELSE 'removed-issue-author' END`)
	require.NoError(err)

	now := gh.Timestamp{Time: time.Now().UTC().Truncate(time.Second)}
	providerClient.PRs["acme/widget"] = []*gh.PullRequest{{
		ID: new(int64(2002)), Number: new(2), Title: new("removed pull"), State: new("open"),
		CreatedAt: &now, UpdatedAt: &now,
	}}
	providerClient.Issues["acme/widget"] = []*gh.Issue{{
		ID: new(int64(4004)), Number: new(4), Title: new("removed issue"), State: new("open"),
		CreatedAt: &now, UpdatedAt: &now,
	}}

	client := setupTestClient(t, srv)
	for _, tc := range []struct {
		number int64
		status int
	}{
		{number: 1, status: http.StatusOK},
		{number: 2, status: http.StatusNotFound},
		{number: 3, status: http.StatusOK},
		{number: 4, status: http.StatusNotFound},
	} {
		resp, resolveErr := client.HTTP.ResolveRepoItemWithResponse(
			ctx, "github", "acme", "widget", tc.number, nil,
		)
		require.NoError(resolveErr)
		require.Equal(tc.status, resp.StatusCode(), string(resp.Body))
	}

	trigger, query := "#", ""
	autocomplete, err := client.HTTP.GetCommentAutocompleteWithResponse(
		ctx, "github", "acme", "widget",
		&generated.GetCommentAutocompleteParams{Trigger: &trigger, Q: &query},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, autocomplete.StatusCode(), string(autocomplete.Body))
	require.NotNil(autocomplete.JSON200)
	require.NotNil(autocomplete.JSON200.References)
	require.ElementsMatch([]generated.CommentAutocompleteReference{
		{Kind: "pull", Number: 1, Title: "Test PR #1", State: "open"},
		{Kind: "issue", Number: 3, Title: "Test Issue", State: "open"},
	}, *autocomplete.JSON200.References)

	trigger = "@"
	autocomplete, err = client.HTTP.GetCommentAutocompleteWithResponse(
		ctx, "github", "acme", "widget",
		&generated.GetCommentAutocompleteParams{Trigger: &trigger, Q: &query},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, autocomplete.StatusCode(), string(autocomplete.Body))
	require.NotNil(autocomplete.JSON200)
	require.NotNil(autocomplete.JSON200.Users)
	require.ElementsMatch(
		[]string{"visible-pull-author", "visible-issue-author"},
		*autocomplete.JSON200.Users,
	)
}

func TestAPIRemovedIssueMutationsReturnNotFoundWithoutProviderWrites(t *testing.T) {
	req := require.New(t)
	srv, database, providerClient, _ := setupTestServerWithFixtureClient(t)
	ctx := t.Context()
	issueID := seedIssue(t, database, "acme", "widget", 7, "open")
	repo, err := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"))
	req.NoError(err)
	req.NotNil(repo)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeIssue, 7, db.ArchiveLifecycleStateRemovedUpstream)
	req.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{{
		IssueID: issueID, PlatformExternalID: "99", EventType: "issue_comment",
		CreatedAt: time.Now().UTC(), DedupeKey: "removed-comment-99",
	}}))
	now := gh.Timestamp{Time: time.Now().UTC().Truncate(time.Second)}
	providerIssue := &gh.Issue{
		ID: new(int64(7007)), Number: new(7), Title: new("original title"), Body: new("original body"),
		State: new("open"), CreatedAt: &now, UpdatedAt: &now,
	}
	providerClient.Issues["acme/widget"] = []*gh.Issue{providerIssue}
	providerClient.OpenIssues["acme/widget"] = []*gh.Issue{providerIssue}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "post comment", method: http.MethodPost, path: "/api/v1/issues/github/acme/widget/7/comments", body: map[string]any{"body": "new comment"}},
		{name: "edit comment", method: http.MethodPatch, path: "/api/v1/issues/github/acme/widget/7/comments/99", body: map[string]any{"body": "edited comment"}},
		{name: "delete comment", method: http.MethodDelete, path: "/api/v1/issues/github/acme/widget/7/comments/99", body: map[string]any{}},
		{name: "edit content", method: http.MethodPatch, path: "/api/v1/issues/github/acme/widget/7", body: map[string]any{"title": "changed title"}},
		{name: "set labels", method: http.MethodPut, path: "/api/v1/issues/github/acme/widget/7/labels", body: map[string]any{"labels": []string{}}},
		{name: "set assignees", method: http.MethodPut, path: "/api/v1/issues/github/acme/widget/7/assignees", body: map[string]any{"assignees": []string{}}},
		{name: "set state", method: http.MethodPost, path: "/api/v1/issues/github/acme/widget/7/github-state", body: map[string]any{"state": "closed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			rr := doLabelAPIRequest(t, srv, tc.method, tc.path, tc.body)
			require.Equal(http.StatusNotFound, rr.Code, rr.Body.String())
			var problem generated.ProblemError
			require.NoError(json.Unmarshal(rr.Body.Bytes(), &problem))
			require.Equal(generated.ProblemErrorCode("issueNotFound"), problem.Code)
		})
	}
	req.Empty(providerClient.Comments["acme/widget#7"])
	req.Equal("original title", providerIssue.GetTitle())
	req.Equal("original body", providerIssue.GetBody())
	req.Equal("open", providerIssue.GetState())
}

func TestAPIRefreshPullCIHidesOnlyRemovedUpstreamItems(t *testing.T) {
	require := require.New(t)
	srv, database, providerClient, _ := setupTestServerWithFixtureClient(t)
	ctx := t.Context()
	seedPRWithHeadSHA(t, database, "acme", "widget", 1, "visible-head")
	seedPRWithHeadSHA(t, database, "acme", "widget", 2, "removed-head")
	repo, err := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 1, db.ArchiveLifecycleStateInaccessible)
	markArchiveItemLifecycle(t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 2, db.ArchiveLifecycleStateRemovedUpstream)
	providerClient.CheckRuns["acme/widget@visible-head"] = []*gh.CheckRun{}

	client := setupTestClient(t, srv)
	visible, err := client.HTTP.RefreshPullCiWithResponse(ctx, "github", "acme", "widget", 1)
	require.NoError(err)
	require.Equal(http.StatusOK, visible.StatusCode(), string(visible.Body))
	removed, err := client.HTTP.RefreshPullCiWithResponse(ctx, "github", "acme", "widget", 2)
	require.NoError(err)
	require.Equal(http.StatusNotFound, removed.StatusCode(), string(removed.Body))
	require.NotNil(removed.ApplicationproblemJSONDefault)
	require.Equal(
		generated.ProblemErrorCode("pullNotFound"),
		removed.ApplicationproblemJSONDefault.Code,
	)
}

func TestAPIRemovedPullMutationsReturnNotFoundWithoutProviderWrites(t *testing.T) {
	req := require.New(t)
	srv, database, providerClient, _ := setupTestServerWithFixtureClient(t)
	ctx := t.Context()
	seedPR(t, database, "acme", "widget", 7)
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	req.NoError(err)
	req.NotNil(repo)
	markArchiveItemLifecycle(
		t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 7,
		db.ArchiveLifecycleStateRemovedUpstream,
	)

	now := gh.Timestamp{Time: time.Now().UTC().Truncate(time.Second)}
	providerPR := &gh.PullRequest{
		ID: new(int64(7007)), Number: new(7), Title: new("removed pull"),
		State: new("open"), Draft: new(true), CreatedAt: &now, UpdatedAt: &now,
	}
	providerClient.PRs["acme/widget"] = []*gh.PullRequest{providerPR}
	providerClient.OpenPRs["acme/widget"] = []*gh.PullRequest{providerPR}
	client := setupTestClient(t, srv)

	t.Run("post comment", func(t *testing.T) {
		require := require.New(t)
		resp, requestErr := client.HTTP.PostPrCommentWithResponse(
			ctx, "github", "acme", "widget", 7,
			generated.PostPrCommentJSONRequestBody{Body: "must not post"},
		)
		require.NoError(requestErr)
		require.Equal(http.StatusNotFound, resp.StatusCode(), string(resp.Body))
		require.NotNil(resp.ApplicationproblemJSONDefault)
		require.Equal(
			generated.ProblemErrorCode("pullNotFound"),
			resp.ApplicationproblemJSONDefault.Code,
		)
	})

	t.Run("ready for review", func(t *testing.T) {
		require := require.New(t)
		resp, requestErr := client.HTTP.MarkPullReadyForReviewWithResponse(
			ctx, "github", "acme", "widget", 7,
		)
		require.NoError(requestErr)
		require.Equal(http.StatusNotFound, resp.StatusCode(), string(resp.Body))
		require.NotNil(resp.ApplicationproblemJSONDefault)
		require.Equal(
			generated.ProblemErrorCode("pullNotFound"),
			resp.ApplicationproblemJSONDefault.Code,
		)
	})

	req.Empty(providerClient.Comments["acme/widget#7"])
	req.True(providerPR.GetDraft())
}

func TestAPISynchronousSyncRejectsRemovedUpstreamTombstones(t *testing.T) {
	require := require.New(t)
	srv, database, providerClient, _ := setupTestServerWithFixtureClient(t)
	ctx := t.Context()
	seedPR(t, database, "acme", "widget", 9)
	seedIssue(t, database, "acme", "widget", 10, "open")
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	markArchiveItemLifecycle(
		t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 9,
		db.ArchiveLifecycleStateRemovedUpstream,
	)
	markArchiveItemLifecycle(
		t, database, repo.ID, db.ArchiveItemTypeIssue, 10,
		db.ArchiveLifecycleStateRemovedUpstream,
	)

	now := gh.Timestamp{Time: time.Now().UTC().Truncate(time.Second)}
	providerClient.PRs["acme/widget"] = []*gh.PullRequest{{
		ID: new(int64(9009)), Number: new(9), Title: new("reappeared pull"),
		State: new("open"), CreatedAt: &now, UpdatedAt: &now,
		User: &gh.User{Login: new("provider-user")},
		Head: &gh.PullRequestBranch{
			Ref: new("feature"), SHA: new("head-sha"),
			Repo: &gh.Repository{FullName: new("acme/widget")},
		},
		Base: &gh.PullRequestBranch{Ref: new("main"), SHA: new("base-sha")},
	}}
	providerClient.Issues["acme/widget"] = []*gh.Issue{{
		ID: new(int64(1010)), Number: new(10), Title: new("reappeared issue"),
		State: new("open"), CreatedAt: &now, UpdatedAt: &now,
		User: &gh.User{Login: new("provider-user")},
	}}
	client := setupTestClient(t, srv)

	pull, err := client.HTTP.SyncPullWithResponse(
		ctx, "github", "acme", "widget", 9,
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, pull.StatusCode(), string(pull.Body))
	require.NotNil(pull.ApplicationproblemJSONDefault)
	require.Equal(
		generated.ProblemErrorCode("pullNotFound"),
		pull.ApplicationproblemJSONDefault.Code,
	)

	issue, err := client.HTTP.SyncIssueWithResponse(
		ctx, "github", "acme", "widget", 10,
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, issue.StatusCode(), string(issue.Body))
	require.NotNil(issue.ApplicationproblemJSONDefault)
	require.Equal(
		generated.ProblemErrorCode("issueNotFound"),
		issue.ApplicationproblemJSONDefault.Code,
	)
}

func TestAPIIssuesHideRemovedUpstreamArchiveRows(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedIssue(t, database, "acme", "widget", 1, "open")
	seedIssue(t, database, "acme", "widget", 2, "closed")
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'issue', 1, 'inaccessible-issue-1', ?, ?, 'inaccessible'),
			(?, 'issue', 2, 'removed-issue-2', ?, ?, 'removed_upstream')`,
		repo.ID, now, now, repo.ID, now, now,
	)
	require.NoError(err)

	client := setupTestClient(t, srv)
	state := "all"
	listed, err := client.HTTP.ListIssuesWithResponse(
		ctx, &generated.ListIssuesParams{State: &state},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, listed.StatusCode())
	require.NotNil(listed.JSON200)
	require.Len(*listed.JSON200, 1)
	require.EqualValues(1, (*listed.JSON200)[0].Number)

	detail, err := client.HTTP.GetIssueWithResponse(
		ctx, "gh", "acme", "widget", 2,
	)
	require.NoError(err)
	require.Equal(http.StatusNotFound, detail.StatusCode())
}

func TestAPIGetPull(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPRWithHeadSHA(t, database, "acme", "widget", 1, "abc123def456")
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.MergeRequest)
	require.EqualValues(1, resp.JSON200.MergeRequest.Number)
	require.Equal("acme", resp.JSON200.RepoOwner)
	require.Equal("widget", resp.JSON200.RepoName)
	require.Equal("abc123def456", resp.JSON200.PlatformHeadSha)
}

func TestAPIGetPullAcceptsMixedCaseRepoPath(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "Acme", "Widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Equal("acme", resp.JSON200.RepoOwner)
	require.Equal("widget", resp.JSON200.RepoName)
}

func TestAPIListPullsAcceptsMixedCaseProviderQualifiedRepoFilter(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	repo := "github|github.com/Acme/Widget"
	resp, err := client.HTTP.ListPullsWithResponse(
		t.Context(), &generated.ListPullsParams{Repo: &repo},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.Equal("acme", (*resp.JSON200)[0].RepoOwner)
	require.Equal("widget", (*resp.JSON200)[0].RepoName)
}

func TestAPIListPullsAcceptsProviderAndHostQualifiedRepoFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupTestServer(t)
	seedPROnHost(t, database, "github.com", "acme", "widget", 1)
	seedPROnHost(t, database, "ghe.example.com", "acme", "widget", 2)
	client := setupTestClient(t, srv)

	repo := "github|ghe.example.com/acme/widget"
	resp, err := client.HTTP.ListPullsWithResponse(
		t.Context(), &generated.ListPullsParams{Repo: &repo},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	assert.Equal("ghe.example.com", (*resp.JSON200)[0].PlatformHost)
	assert.Equal("acme", (*resp.JSON200)[0].RepoOwner)
	assert.Equal("widget", (*resp.JSON200)[0].RepoName)
	assert.EqualValues(2, (*resp.JSON200)[0].Number)
}

func TestAPIGetPullIncludesBranches(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	mr := resp.JSON200.MergeRequest
	require.NotNil(mr)
	require.Equal("feature", mr.HeadBranch)
	require.Equal("main", mr.BaseBranch)
}

func TestAPIGetPullIncludesLabels(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPRWithLabels(t, database, "acme", "widget", 1, []db.Label{{
		Name:      "enhancement",
		Color:     "a2eeef",
		IsDefault: false,
	}})
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.MergeRequest.Labels)
	require.Equal([]generated.Label{{
		Name:      "enhancement",
		Color:     "a2eeef",
		IsDefault: false,
	}}, *resp.JSON200.MergeRequest.Labels)
}

func TestAPIListPullsStateFilter(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedPR(t, database, "acme", "widget", 1)
	seedPR(t, database, "acme", "widget", 2)
	seedPR(t, database, "acme", "widget", 3)

	repo, _ := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"))
	now := time.Now()
	require.NoError(database.UpdateMRState(ctx, repo.ID, 2, "closed", nil, &now))
	require.NoError(database.UpdateMRState(ctx, repo.ID, 3, "merged", &now, &now))

	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ListPullsWithResponse(ctx, nil)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.Len(*resp.JSON200, 1)

	state := "closed"
	resp, err = client.HTTP.ListPullsWithResponse(ctx, &generated.ListPullsParams{State: &state})
	require.NoError(err)
	require.Len(*resp.JSON200, 2)

	state = "all"
	resp, err = client.HTTP.ListPullsWithResponse(ctx, &generated.ListPullsParams{State: &state})
	require.NoError(err)
	require.Len(*resp.JSON200, 3)

	state = "bogus"
	resp, err = client.HTTP.ListPullsWithResponse(ctx, &generated.ListPullsParams{State: &state})
	require.NoError(err)
	require.Equal(http.StatusBadRequest, resp.StatusCode())
}

func TestAPIListIssuesStateFilter(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedIssue(t, database, "acme", "widget", 1, "open")
	seedIssue(t, database, "acme", "widget", 2, "closed")

	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ListIssuesWithResponse(ctx, nil)
	require.NoError(err)
	require.Len(*resp.JSON200, 1)

	state := "closed"
	resp, err = client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{State: &state})
	require.NoError(err)
	require.Len(*resp.JSON200, 1)

	state = "all"
	resp, err = client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{State: &state})
	require.NoError(err)
	require.Len(*resp.JSON200, 2)
}

func TestAPIListIssuesFilterByAssignee(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedIssueWithAssignees(t, database, "acme", "widget", 1, "open", `["alice"]`)
	seedIssueWithAssignees(t, database, "acme", "widget", 2, "open", `["bob"]`)

	client := setupTestClient(t, srv)

	assignee := "alice"
	state := "all"
	resp, err := client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{
		Assignee: &assignee,
		State:    &state,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.EqualValues(1, (*resp.JSON200)[0].Number)
}

func TestAPIListIssuesResponseIncludesAssignees(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedIssueWithAssignees(t, database, "acme", "widget", 1, "open", `["alice","bob"]`)

	client := setupTestClient(t, srv)

	state := "all"
	resp, err := client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{
		State: &state,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.NotNil((*resp.JSON200)[0].Assignees)
	require.Equal([]string{"alice", "bob"}, *(*resp.JSON200)[0].Assignees)
}

func TestAPIGetIssueIncludesAssignees(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedIssueWithAssignees(t, database, "acme", "widget", 7, "open", `["alice","bob"]`)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetIssueWithResponse(
		t.Context(), "gh", "acme", "widget", 7,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Issue.Assignees)
	require.Equal([]string{"alice", "bob"}, *resp.JSON200.Issue.Assignees)
}

func TestAPISyncIssuePersistsAssigneesFromProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	srv, database, providerClient, syncer := setupTestServerWithFixtureClient(t)
	issueID := int64(7000)
	issueNumber := 7
	title := "Synced issue with assignees"
	state := "open"
	url := "https://github.com/acme/widget/issues/7"
	author := "octocat"
	alice := "alice"
	bob := "bob"
	createdAt := gh.Timestamp{Time: now}
	updatedAt := gh.Timestamp{Time: now}
	providerClient.Issues["acme/widget"] = []*gh.Issue{{
		ID:        &issueID,
		Number:    &issueNumber,
		Title:     &title,
		State:     &state,
		HTMLURL:   &url,
		User:      &gh.User{Login: &author},
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
		Assignees: []*gh.User{{Login: &alice}, {Login: &bob}},
	}}

	require.NoError(syncer.SyncIssue(ctx, "acme", "widget", issueNumber))

	repo, err := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	persisted, err := database.GetIssueByRepoIDAndNumber(ctx, repo.ID, issueNumber)
	require.NoError(err)
	require.NotNil(persisted)
	assert.JSONEq(`["alice","bob"]`, persisted.AssigneesJSON)
	assert.Equal([]string{"alice", "bob"}, persisted.Assignees)

	client := setupTestClient(t, srv)
	stateParam := "all"
	listResp, err := client.HTTP.ListIssuesWithResponse(
		ctx, &generated.ListIssuesParams{State: &stateParam},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, listResp.StatusCode())
	require.NotNil(listResp.JSON200)
	require.Len(*listResp.JSON200, 1)
	require.NotNil((*listResp.JSON200)[0].Assignees)
	assert.Equal([]string{"alice", "bob"}, *(*listResp.JSON200)[0].Assignees)

	assignee := "bob"
	filterResp, err := client.HTTP.ListIssuesWithResponse(
		ctx, &generated.ListIssuesParams{Assignee: &assignee, State: &stateParam},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, filterResp.StatusCode())
	require.NotNil(filterResp.JSON200)
	require.Len(*filterResp.JSON200, 1)
	assert.EqualValues(issueNumber, (*filterResp.JSON200)[0].Number)

	detailResp, err := client.HTTP.GetIssueWithResponse(
		ctx, "gh", "acme", "widget", int64(issueNumber),
	)
	require.NoError(err)
	require.Equal(http.StatusOK, detailResp.StatusCode())
	require.NotNil(detailResp.JSON200)
	require.NotNil(detailResp.JSON200.Issue.Assignees)
	assert.Equal([]string{"alice", "bob"}, *detailResp.JSON200.Issue.Assignees)
}

func TestAPIGetIssueIncludesLabels(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	description := "Customer reported"
	seedIssueWithLabels(t, database, "acme", "widget", 5, "open", []db.Label{{
		Name:        "bug",
		Description: description,
		Color:       "d73a4a",
		IsDefault:   true,
	}})
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetIssueWithResponse(
		t.Context(), "gh", "acme", "widget", 5,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Issue.Labels)
	require.Equal([]generated.Label{{
		Name:        "bug",
		Description: &description,
		Color:       "d73a4a",
		IsDefault:   true,
	}}, *resp.JSON200.Issue.Labels)
}
