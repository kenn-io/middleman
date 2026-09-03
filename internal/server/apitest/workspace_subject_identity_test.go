package apitest

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
)

func TestWorkspaceReferencesUseStableRepositoryIdentityAcrossRenameAndRouteReuse(t *testing.T) {
	require := require.New(t)
	srv, database := setupTestServer(t)
	client := setupTestClient(t, srv)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	oldIdentity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	oldIdentity.PlatformRepoID = "repo-old-widget"
	oldRepo, accepted, err := database.ReconcileRepositoryObservation(ctx, oldIdentity, now)
	require.NoError(err)
	require.True(accepted)
	seedWorkspaceIdentitySubjects(t, database, oldRepo.Repository.ID, "acme", "widget", "old", now)

	renamedIdentity := db.GitHubRepoIdentity("github.com", "acme", "gadget")
	renamedIdentity.PlatformRepoID = oldIdentity.PlatformRepoID
	_, accepted, err = database.ReconcileRepositoryObservation(ctx, renamedIdentity, now.Add(time.Minute))
	require.NoError(err)
	require.True(accepted)

	assertWorkspaceIdentitySurfaces(t, client.HTTP, "gadget", true)

	replacementIdentity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	replacementIdentity.PlatformRepoID = "repo-replacement-widget"
	replacement, accepted, err := database.ReconcileRepositoryObservation(
		ctx, replacementIdentity, now.Add(2*time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	seedWorkspaceIdentitySubjects(
		t, database, replacement.Repository.ID, "acme", "widget", "replacement", now.Add(2*time.Minute),
	)

	assertWorkspaceIdentitySurfaces(t, client.HTTP, "gadget", true)
	assertWorkspaceIdentitySurfaces(t, client.HTTP, "widget", false)
}

func seedWorkspaceIdentitySubjects(
	t *testing.T,
	database *db.DB,
	repoID int64,
	owner string,
	name string,
	prefix string,
	now time.Time,
) {
	t.Helper()
	prID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: repoID*1000 + 61, Number: 61,
		URL:   "https://github.com/" + owner + "/" + name + "/pull/61",
		Title: prefix + " pull", Author: "alice", State: db.MergeRequestStateOpen,
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	require.NoError(t, database.EnsureKanbanState(t.Context(), prID))
	_, err = database.UpsertIssue(t.Context(), &db.Issue{
		RepoID: repoID, PlatformID: repoID*1000 + 62, Number: 62,
		URL:   "https://github.com/" + owner + "/" + name + "/issues/62",
		Title: prefix + " issue", Author: "bob", State: "open",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)

	if prefix != "old" {
		return
	}
	for _, workspace := range []*db.Workspace{
		{
			ID: "ws-stable-pr", Platform: "github", PlatformHost: "github.com",
			RepoOwner: owner, RepoName: name, ItemType: db.WorkspaceItemTypePullRequest,
			ItemNumber: 61, WorktreePath: t.TempDir(), Status: "ready",
		},
		{
			ID: "ws-stable-issue", Platform: "github", PlatformHost: "github.com",
			RepoOwner: owner, RepoName: name, ItemType: db.WorkspaceItemTypeIssue,
			ItemNumber: 62, WorktreePath: t.TempDir(), Status: "ready",
		},
	} {
		require.NoError(t, database.InsertWorkspace(t.Context(), workspace))
	}
}

func assertWorkspaceIdentitySurfaces(
	t *testing.T,
	client *generated.ClientWithResponses,
	repoName string,
	wantWorkspace bool,
) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()

	activityResponse, err := client.ListActivityWithResponse(ctx, &generated.ListActivityParams{})
	require.NoError(err)
	require.Equal(http.StatusOK, activityResponse.StatusCode())
	require.NotNil(activityResponse.JSON200)
	require.NotNil(activityResponse.JSON200.Items)
	activityByType := make(map[string]generated.ActivityItemResponse)
	for _, item := range activityResponse.JSON200.Items {
		if item.RepoName == repoName && (item.ItemNumber == 61 || item.ItemNumber == 62) {
			activityByType[item.ItemType] = item
		}
	}
	require.Contains(activityByType, "pr")
	require.Contains(activityByType, "issue")
	assertWorkspaceRef(t, activityByType["pr"].Workspace, "ws-stable-pr", wantWorkspace)
	assertWorkspaceRef(t, activityByType["issue"].Workspace, "ws-stable-issue", wantWorkspace)

	pullsResponse, err := client.ListPullsWithResponse(ctx, nil)
	require.NoError(err)
	require.Equal(http.StatusOK, pullsResponse.StatusCode())
	require.NotNil(pullsResponse.JSON200)
	var pull *generated.MergeRequestResponse
	for i := range *pullsResponse.JSON200 {
		candidate := &(*pullsResponse.JSON200)[i]
		if candidate.RepoName == repoName && candidate.Number == 61 {
			pull = candidate
			break
		}
	}
	require.NotNil(pull)
	assertWorkspaceRef(t, pull.Workspace, "ws-stable-pr", wantWorkspace)
	pullDetail, err := client.GetPullWithResponse(ctx, "gh", "acme", repoName, 61)
	require.NoError(err)
	require.Equal(http.StatusOK, pullDetail.StatusCode())
	require.NotNil(pullDetail.JSON200)
	assertWorkspaceRef(t, pullDetail.JSON200.Workspace, "ws-stable-pr", wantWorkspace)

	issuesResponse, err := client.ListIssuesWithResponse(ctx, nil)
	require.NoError(err)
	require.Equal(http.StatusOK, issuesResponse.StatusCode())
	require.NotNil(issuesResponse.JSON200)
	var issue *generated.IssueResponse
	for i := range *issuesResponse.JSON200 {
		candidate := &(*issuesResponse.JSON200)[i]
		if candidate.RepoName == repoName && candidate.Number == 62 {
			issue = candidate
			break
		}
	}
	require.NotNil(issue)
	assertWorkspaceRef(t, issue.Workspace, "ws-stable-issue", wantWorkspace)
	issueDetail, err := client.GetIssueWithResponse(ctx, "gh", "acme", repoName, 62)
	require.NoError(err)
	require.Equal(http.StatusOK, issueDetail.StatusCode())
	require.NotNil(issueDetail.JSON200)
	assertWorkspaceRef(t, issueDetail.JSON200.Workspace, "ws-stable-issue", wantWorkspace)
}

func assertWorkspaceRef(t *testing.T, ref *generated.WorkspaceRef, wantID string, wantWorkspace bool) {
	t.Helper()
	if !wantWorkspace {
		require.Nil(t, ref)
		return
	}
	require.NotNil(t, ref)
	require.Equal(t, wantID, ref.Id)
}
