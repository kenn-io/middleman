package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
)

func TestAPIClientConstruction(t *testing.T) {
	srv, _ := setupTestServer(t)
	client := setupTestClient(t, srv)
	require.NotNil(t, client)
	require.NotNil(t, client.HTTP)
}

func TestAPIGetPullIncludesDeletedCommentTimelineEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database := setupTestServer(t)
	mrID := seedPRWithHeadSHA(t, database, "acme", "widget", 1, "deadbeef")
	createdAt := time.Date(2024, 6, 1, 12, 18, 0, 0, time.UTC)
	require.NoError(database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: mrID,
		EventType:      "comment_deleted",
		Author:         "maintainer",
		Summary:        "deleted a comment from reviewer",
		MetadataJSON:   `{"deleted_comment_author":"reviewer"}`,
		CreatedAt:      createdAt,
		DedupeKey:      "timeline-CDE_1",
	}}))
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Events)
	require.Len(resp.JSON200.Events, 1)
	event := resp.JSON200.Events[0]
	assert.Equal("comment_deleted", event.EventType)
	assert.Equal("maintainer", event.Author)
	assert.Equal("deleted a comment from reviewer", event.Summary)
	assert.JSONEq(`{"deleted_comment_author":"reviewer"}`, event.MetadataJSON)
	assert.Equal(createdAt, event.CreatedAt)
}

func TestAPIGetPullNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullWithResponse(
		t.Context(), "gh", "acme", "widget", 999,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode())
	require.NotNil(t, resp.ApplicationproblemJSONDefault)
}

func TestAPISetKanbanState(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.SetKanbanStateWithResponse(
		t.Context(), "gh",
		"acme",
		"widget",
		1,
		generated.SetKanbanStateJSONRequestBody{Status: "reviewing"},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())

	pr, err := database.GetMergeRequest(t.Context(), "github", "github.com", "acme", "widget", 1)
	require.NoError(err)
	require.NotNil(pr)
	require.Equal(db.KanbanStatusReviewing, pr.KanbanStatus)
}

func TestAPISetKanbanStateRejectsInvalidStatus(t *testing.T) {
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.SetKanbanStateWithResponse(
		t.Context(), "gh",
		"acme",
		"widget",
		1,
		generated.SetKanbanStateJSONRequestBody{Status: "nonsense"},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode())
	require.NotNil(t, resp.ApplicationproblemJSONDefault)
}

func TestAPIListRepos(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)

	_, err := database.UpsertRepo(t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	resp, err := client.HTTP.ListReposWithResponse(t.Context())
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.Equal("acme", (*resp.JSON200)[0].Owner)
	require.Equal("widget", (*resp.JSON200)[0].Name)
}

func TestAPISetStarred(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.SetStarredWithResponse(t.Context(), generated.SetStarredJSONRequestBody{
		ItemType:     "pr",
		Provider:     "github",
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
		Number:       1,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())

	starred, err := database.IsStarred(t.Context(), "pr", 1, 1)
	require.NoError(err)
	require.True(starred)
}

func TestAPIUnsetStarred(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	require.NoError(database.SetStarred(t.Context(), "pr", 1, 1))
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.UnsetStarredWithResponse(t.Context(), &generated.UnsetStarredParams{
		ItemType:     "pr",
		Provider:     "github",
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
		Number:       1,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())

	starred, err := database.IsStarred(t.Context(), "pr", 1, 1)
	require.NoError(err)
	require.False(starred)
}

func TestAPISetStarredRejectsInvalidItemType(t *testing.T) {
	require := require.New(t)
	srv, _ := setupTestServer(t)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.SetStarredWithResponse(t.Context(), generated.SetStarredJSONRequestBody{
		ItemType:     "repo",
		Provider:     "github",
		PlatformHost: "github.com",
		Owner:        "acme",
		Name:         "widget",
		Number:       1,
	})
	require.NoError(err)
	require.Equal(http.StatusBadRequest, resp.StatusCode())
	require.NotNil(resp.ApplicationproblemJSONDefault)
	require.NotNil(resp.ApplicationproblemJSONDefault.Detail)
	require.Contains(*resp.ApplicationproblemJSONDefault.Detail, "item_type must be 'pr' or 'issue'")
}

func TestOpenAPIEndpointReflectsHumaContract(t *testing.T) {
	require := require.New(t)
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	require.Contains(body, `"/activity"`)
	require.Contains(body, `"name":"since"`)
	require.Contains(body, `"capped"`)
	require.Contains(body, `"item_activity_capped"`)
	require.Contains(body, `"name":"projection"`)
	require.Contains(body, `"name":"before"`)
	require.Contains(body, `"name":"at_or_before"`)
	require.Contains(body, `"event_cursor"`)
	require.Contains(body, `"/activity/thread-events"`)
	require.NotContains(body, `"has_more"`)
}

func TestAPIClosePRRejectsMerged(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	ctx := t.Context()

	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	now := time.Now()
	require.NoError(database.UpdateMRState(ctx, repo.ID, 1, "merged", &now, &now))

	client := setupTestClient(t, srv)
	resp, err := client.HTTP.SetPrGithubStateWithResponse(
		ctx, "gh", "acme", "widget", 1,
		generated.SetPrGithubStateJSONRequestBody{State: "open"},
	)
	require.NoError(err)
	require.Equal(http.StatusConflict, resp.StatusCode())
}

func TestAPIClosePRInvalidState(t *testing.T) {
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.SetPrGithubStateWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
		generated.SetPrGithubStateJSONRequestBody{State: "nonsense"},
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode())
}

func TestAPIListItemsHonorsLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	seedPR(t, database, "acme", "widget", 12)
	seedPR(t, database, "acme", "widget", 278)
	seedIssue(t, database, "acme", "widget", 12, "open")
	seedIssue(t, database, "acme", "widget", 278, "open")

	client := setupTestClient(t, srv)
	limit := int64(1)

	pullsResp, err := client.HTTP.ListPullsWithResponse(ctx, &generated.ListPullsParams{Limit: &limit})
	require.NoError(err)
	require.Equal(http.StatusOK, pullsResp.StatusCode())
	require.NotNil(pullsResp.JSON200)
	require.Len(*pullsResp.JSON200, 1)
	assert.EqualValues(278, (*pullsResp.JSON200)[0].Number)

	offset := int64(1)
	secondPullResp, err := client.HTTP.ListPullsWithResponse(ctx, &generated.ListPullsParams{Limit: &limit, Offset: &offset})
	require.NoError(err)
	require.Equal(http.StatusOK, secondPullResp.StatusCode())
	require.NotNil(secondPullResp.JSON200)
	require.Len(*secondPullResp.JSON200, 1)
	assert.EqualValues(12, (*secondPullResp.JSON200)[0].Number)

	issuesResp, err := client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{Limit: &limit})
	require.NoError(err)
	require.Equal(http.StatusOK, issuesResp.StatusCode())
	require.NotNil(issuesResp.JSON200)
	require.Len(*issuesResp.JSON200, 1)
	assert.EqualValues(278, (*issuesResp.JSON200)[0].Number)

	secondIssueResp, err := client.HTTP.ListIssuesWithResponse(ctx, &generated.ListIssuesParams{Limit: &limit, Offset: &offset})
	require.NoError(err)
	require.Equal(http.StatusOK, secondIssueResp.StatusCode())
	require.NotNil(secondIssueResp.JSON200)
	require.Len(*secondIssueResp.JSON200, 1)
	assert.EqualValues(12, (*secondIssueResp.JSON200)[0].Number)
}

func TestAPIListIssuesIncludesLabels(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedIssueWithLabels(t, database, "acme", "widget", 5, "open", []db.Label{{
		Name:      "triage",
		Color:     "fbca04",
		IsDefault: false,
	}})
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ListIssuesWithResponse(t.Context(), nil)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.NotNil((*resp.JSON200)[0].Labels)
	require.Equal([]generated.Label{{
		Name:      "triage",
		Color:     "fbca04",
		IsDefault: false,
	}}, *(*resp.JSON200)[0].Labels)
}

func TestAPIGetIssueAcceptsMixedCaseRepoPath(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedIssue(t, database, "acme", "widget", 5, "open")
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetIssueWithResponse(
		t.Context(), "gh", "Acme", "Widget", 5,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Equal("acme", resp.JSON200.RepoOwner)
	require.Equal("widget", resp.JSON200.RepoName)
}

func TestAPIListIssuesAcceptsMixedCaseProviderQualifiedRepoFilter(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedIssue(t, database, "acme", "widget", 5, "open")
	client := setupTestClient(t, srv)

	repo := "github|github.com/Acme/Widget"
	resp, err := client.HTTP.ListIssuesWithResponse(
		t.Context(), &generated.ListIssuesParams{Repo: &repo},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.Len(*resp.JSON200, 1)
	require.Equal("acme", (*resp.JSON200)[0].RepoOwner)
	require.Equal("widget", (*resp.JSON200)[0].RepoName)
}

func TestResolveItem_UntrackedRepo(t *testing.T) {
	require := require.New(t)
	srv, _ := setupTestServer(t)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ResolveRepoItemWithResponse(
		t.Context(), "gh", "unknown", "repo", 1, nil,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.False(resp.JSON200.RepoTracked)
	require.EqualValues(1, resp.JSON200.Number)
	require.Empty(resp.JSON200.ItemType)
}

func TestAPIGetMRImportMetadata(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()

	repoID, err := database.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	pr := &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       42000,
		Number:           42,
		URL:              "https://github.com/acme/widget/pull/42",
		Title:            "Add feature X",
		Author:           "octocat",
		State:            "open",
		IsDraft:          true,
		Body:             "body",
		HeadBranch:       "feature-x",
		BaseBranch:       "main",
		PlatformHeadSHA:  "abc123def456",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}
	prID, err := database.UpsertMergeRequest(ctx, pr)
	require.NoError(err)
	require.NoError(database.EnsureKanbanState(ctx, prID))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/pulls/gh/acme/widget/42/import-metadata", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(body, `"number":42`)
	require.Contains(body, `"head_branch":"feature-x"`)
	require.Contains(body, `"platform_head_sha":"abc123def456"`)
	require.Contains(body, `"head_repo_clone_url":"https://github.com/fork/widget.git"`)
	require.Contains(body, `"state":"open"`)
	require.Contains(body, `"is_draft":true`)
	require.Contains(body, `"title":"Add feature X"`)
}

func TestAPIGetMRImportMetadataNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/pulls/gh/acme/widget/999/import-metadata", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestOpenAPIDocumentsCustomStatusCodes(t *testing.T) {
	require := require.New(t)
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	spec := rr.Body.String()
	require.Contains(spec, `"/sync":{"post":{"operationId":"trigger-sync"`)
	require.Contains(spec, `"/starred":{"delete":{"operationId":"unset-starred"`)
	require.Contains(spec, `"/pulls/{provider}/{owner}/{name}/{number}/comments":{"post":{"operationId":"post-pr-comment"`)
	require.Contains(spec, `"trigger-sync","parameters"`)
	require.Contains(spec, `"name":"priority_repo"`)
	require.Contains(spec, `"responses":{"202":{"description":"Accepted"}`)
	require.Contains(spec, `"set-starred","requestBody"`)
	require.Contains(spec, `"responses":{"200":{"description":"OK"}`)
	require.True(
		strings.Contains(spec, `"operationId":"post-pr-comment","parameters"`) ||
			strings.Contains(spec, `"operationId":"post-pr-comment","requestBody"`),
		"expected post-pr-comment operation to be present",
	)
	require.Contains(spec, `"responses":{"201":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/MergeRequestEventResponse"}}},"description":"Created"}`)
	require.Contains(spec, `"operationId":"post-issue-comment"`)
	require.Contains(spec, `"responses":{"201":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/IssueEvent"}}},"description":"Created"}`)
	for _, operationID := range []string{
		"get-version",
		"get-settings",
		"update-settings",
		"add-repo",
		"refresh-repo",
		"delete-repo",
		"stream-events",
		"get-roborev-status",
	} {
		require.Contains(spec, `"operationId":"`+operationID+`"`)
	}
}

func TestProviderIssueRouteGeneratedClientEscapesGitLabRepoPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	provider := "gitlab"
	host := "gitlab.example.test:8443"
	repoPath := "Team One/Sub Team/project+#1"
	number := int64(7)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       provider,
		PlatformHost:   host,
		PlatformRepoID: "gid://gitlab/Project/7000",
		Owner:          "Team One/Sub Team",
		Name:           "project+#1",
		RepoPath:       repoPath,
	})
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     7000,
		Number:         int(number),
		URL:            "https://gitlab.example.test/Team%20One/Sub%20Team/project%2B%231/-/issues/7",
		Title:          "Special chars issue",
		Author:         "testuser",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	resp, err := client.HTTP.GetIssueOnHostWithResponse(
		ctx, host, provider, "Team One/Sub Team", "project+#1", number,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON200)

	assert.Equal(provider, resp.JSON200.Repo.Provider)
	assert.Equal(host, resp.JSON200.Repo.PlatformHost)
	assert.Equal(repoPath, resp.JSON200.Repo.RepoPath)
	assert.Equal("Team One/Sub Team", resp.JSON200.Repo.Owner)
	assert.Equal("project+#1", resp.JSON200.Repo.Name)
	assert.Equal(number, resp.JSON200.Issue.Number)
}

func TestProviderIssueRouteHandlesNestedGitLabRepoPathOverHTTP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "git.example.com",
		PlatformRepoID: "gid://gitlab/Project/7007",
		Owner:          "group/subgroup",
		Name:           "project",
		RepoPath:       "group/subgroup/project",
	})
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID:         repoID,
		PlatformID:     7007,
		Number:         7,
		URL:            "https://git.example.com/group/subgroup/project/-/issues/7",
		Title:          "Nested GitLab issue",
		Author:         "testuser",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/host/git.example.com/issues/gitlab/group%2Fsubgroup/project/7",
		nil,
	)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Issue struct {
			Number int64  `json:"number"`
			Title  string `json:"title"`
		} `json:"issue"`
		Repo struct {
			Provider     string `json:"provider"`
			PlatformHost string `json:"platform_host"`
			Owner        string `json:"owner"`
			Name         string `json:"name"`
			RepoPath     string `json:"repo_path"`
		} `json:"repo"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(int64(7), body.Issue.Number)
	assert.Equal("Nested GitLab issue", body.Issue.Title)
	assert.Equal("gitlab", body.Repo.Provider)
	assert.Equal("git.example.com", body.Repo.PlatformHost)
	assert.Equal("group/subgroup", body.Repo.Owner)
	assert.Equal("project", body.Repo.Name)
	assert.Equal("group/subgroup/project", body.Repo.RepoPath)
}

func TestMRListEmptyLinksWhenNone(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pulls", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)
	body := rr.Body.String()

	require.Contains(body, `"worktree_links":[]`)
}

func TestAPIGetFiles503WhenCloneManagerNil(t *testing.T) {
	require := require.New(t)

	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.GetPullFilesWithResponse(
		t.Context(), "gh", "acme", "widget", 1,
	)
	require.NoError(err)
	require.Equal(http.StatusServiceUnavailable, resp.StatusCode())
}

func TestSetActiveWorktreeKey(t *testing.T) {
	assert := assert.New(t)
	srv, _ := setupTestServer(t)

	key, set := srv.ActiveWorktreeKey()
	assert.Empty(key)
	assert.False(set)

	srv.SetActiveWorktreeKey("wt-abc")
	key, set = srv.ActiveWorktreeKey()
	assert.Equal("wt-abc", key)
	assert.True(set)

	srv.SetActiveWorktreeKey("")
	key, set = srv.ActiveWorktreeKey()
	assert.Empty(key)
	assert.True(set, "should still be 'set' even when cleared")
}

func TestAPIGetPullDetailRecordsHotView(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	srv, database := setupTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	client := setupTestClient(t, srv)

	repo, err := database.GetRepoByIdentity(
		ctx,
		db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1)
	require.NoError(err)
	require.NotNil(mr)

	resp, err := client.HTTP.GetPullWithResponse(ctx, "gh", "acme", "widget", 1)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())

	hotIDs, err := database.ListHotMergeRequestIDs(ctx, 10)
	require.NoError(err)
	assert.Equal([]int64{mr.ID}, hotIDs)

	closedAt := time.Now().UTC()
	require.NoError(database.UpdateMRState(
		ctx,
		repo.ID,
		1,
		string(db.MergeRequestStateClosed),
		nil,
		&closedAt,
	))

	resp, err = client.HTTP.GetPullWithResponse(ctx, "gh", "acme", "widget", 1)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())

	hotIDs, err = database.ListHotMergeRequestIDs(ctx, 10)
	require.NoError(err)
	assert.Empty(hotIDs, "reading a terminal PR must not restore hot membership")
}

func TestAPIGetPullDetailIncludesDiffSummaryRevisionFields(t *testing.T) {
	require := require.New(t)

	srv, database := setupTestServer(t)
	ctx := t.Context()
	repoID, err := database.UpsertRepo(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      1000,
		Number:          1,
		URL:             "https://github.com/acme/widget/pull/1",
		Title:           "Test PR #1",
		Author:          "testuser",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "platform-head",
		PlatformBaseSHA: "platform-base",
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(err)
	require.NoError(database.UpdateDiffSHAs(ctx, repoID, 1, "diff-head", "diff-base", "merge-base"))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/pulls/gh/acme/widget/1", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(body, `"platform_head_sha":"platform-head"`)
	require.Contains(body, `"platform_base_sha":"platform-base"`)
	require.Contains(body, `"diff_head_sha":"diff-head"`)
	require.Contains(body, `"merge_base_sha":"merge-base"`)
}

func TestAPIActivityCommentCarriesPRAuthor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)
	prID := seedPR(t, database, "acme", "widget", 1)
	ctx := t.Context()

	commentedAt := time.Now().UTC().Add(-30 * time.Minute).Round(time.Second)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: prID,
		EventType:      "issue_comment",
		Author:         "pr-reviewer",
		Body:           "looks good",
		CreatedAt:      commentedAt,
		DedupeKey:      "comment-item-author",
	}}))

	since := commentedAt.Add(-time.Hour).Format(time.RFC3339)
	resp, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Since: &since},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Items)

	var commentItem *generated.ActivityItemResponse
	for i := range resp.JSON200.Items {
		item := &resp.JSON200.Items[i]
		if item.ActivityType == "comment" && item.Author == "pr-reviewer" {
			commentItem = item
			break
		}
	}
	require.NotNil(commentItem)

	require.NotNil(commentItem.ItemAuthor)
	assert.Equal("testuser", *commentItem.ItemAuthor)
	assert.Equal("pr-reviewer", commentItem.Author)
}

func TestAPIListActivitySearchEventDeltaDoesNotReadBeforeCursor(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	prID := seedPR(t, database, "acme", "widget", 1)
	malformedCreatedAt := now.Add(-time.Hour).Format("2006-01-02 15:04:05") + " invalid"
	_, err := database.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_mr_events (
			merge_request_id, event_type, author, body, created_at, dedupe_key
		) VALUES (?, 'issue_comment', 'search-reviewer', 'needle', ?, 'malformed-before-cursor')`,
		prID, malformedCreatedAt)
	require.NoError(err)

	search := "needle"
	sinceTime := now.Add(-2 * time.Hour)
	deltaRows, err := database.ListActivity(ctx, db.ListActivityOpts{
		Search: search, Since: &sinceTime, AfterTime: &now, Limit: 11,
	})
	require.NoError(err, "the cursor-bounded event query must exclude the malformed older row")
	require.Empty(deltaRows)

	projection := generated.ListActivityParamsProjectionEvents
	since := sinceTime.Format(time.RFC3339)
	after := db.EncodeCursor(now, "pre", 0)
	limit := int64(10)
	resp, err := client.HTTP.ListActivityWithResponse(ctx, &generated.ListActivityParams{
		Search: &search, Since: &since, After: &after, Projection: &projection, Limit: &limit,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Items)
	require.Empty(resp.JSON200.Items)
	require.NotNil(resp.JSON200.ItemActivity)
	require.Empty(resp.JSON200.ItemActivity)
	require.NotNil(resp.JSON200.WorkspaceActivity)
	require.Empty(resp.JSON200.WorkspaceActivity)
}

func TestAPIListActivityAcceptsProviderAndHostQualifiedRepoFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)

	seedPROnHost(t, database, "github.com", "acme", "widget", 1)
	seedPROnHost(t, database, "ghe.example.com", "acme", "widget", 2)

	since := time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)
	repo := "github|ghe.example.com/acme/widget"
	resp, err := client.HTTP.ListActivityWithResponse(
		t.Context(), &generated.ListActivityParams{Since: &since, Repo: &repo},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Items)
	require.NotEmpty(resp.JSON200.Items)
	for _, item := range resp.JSON200.Items {
		assert.Equal("ghe.example.com", item.PlatformHost)
		assert.Equal("acme", item.RepoOwner)
		assert.Equal("widget", item.RepoName)
	}
}

func TestAPIListStacks_Empty(t *testing.T) {
	assert := assert.New(t)
	srv, _ := setupTestServer(t)
	client := setupTestClient(t, srv)

	resp, err := client.HTTP.ListStacksWithResponse(t.Context(), &generated.ListStacksParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	var stks []generated.StackResponse
	require.NoError(t, json.Unmarshal(resp.Body, &stks))
	assert.Empty(stks)
}
