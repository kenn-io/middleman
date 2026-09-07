package apitest

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

type assigneesAPIResponse struct {
	Assignees []string `json:"assignees"`
}

type reviewersAPIResponse struct {
	Reviewers []string `json:"reviewers"`
}

func setupAssigneeTestServer(t *testing.T) (*server.Server, *db.DB, *testutil.FixtureClient, *ghclient.Syncer) {
	t.Helper()
	database := dbtest.Open(t)
	client := testutil.NewFixtureClient().(*testutil.FixtureClient)

	now := gh.Timestamp{Time: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)}
	pr := &gh.PullRequest{
		ID:        new(int64(101)),
		Number:    new(1),
		Title:     new("Improve widgets"),
		State:     new("open"),
		CreatedAt: &now,
		UpdatedAt: &now,
		RequestedReviewers: []*gh.User{
			{Login: new("carol")},
		},
	}
	client.PRs["acme/widget"] = []*gh.PullRequest{pr}
	client.OpenPRs["acme/widget"] = []*gh.PullRequest{pr}

	issue := &gh.Issue{
		ID:        new(int64(201)),
		Number:    new(7),
		Title:     new("Widget issue"),
		State:     new("open"),
		CreatedAt: &now,
		UpdatedAt: &now,
	}
	client.Issues["acme/widget"] = []*gh.Issue{issue}
	client.OpenIssues["acme/widget"] = []*gh.Issue{issue}

	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": client},
		database, nil, defaultTestRepos, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})
	return srv, database, client, syncer
}

func TestAPISetPullAssigneesUpdatesProviderAndPersists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, syncer := setupAssigneeTestServer(t)
	repoID := seedPR(t, database, "acme", "widget", 1)

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/assignees", map[string][]string{
		"assignees": {"alice", "bob"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body assigneesAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"alice", "bob"}, body.Assignees)

	repo, err := database.GetRepoByIdentity(t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repo.ID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal([]string{"alice", "bob"}, pr.Assignees)

	providerPR := providerClient.PRs["acme/widget"][0]
	require.Len(providerPR.Assignees, 2)
	assert.Equal("alice", providerPR.Assignees[0].GetLogin())

	// A full sync cycle must not revert the persisted mutation.
	syncer.RunOnce(t.Context())
	resynced, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repo.ID, 1)
	require.NoError(err)
	require.NotNil(resynced)
	assert.Equal([]string{"alice", "bob"}, resynced.Assignees)
	_ = repoID
}

func TestAPISetPullAssigneesClearsAssignees(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, _, _ := setupAssigneeTestServer(t)
	seedPR(t, database, "acme", "widget", 1)

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/assignees", map[string][]string{
		"assignees": {"alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	rr = doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/assignees", map[string][]string{
		"assignees": {},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body assigneesAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(body.Assignees)

	repo, err := database.GetRepoByIdentity(t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repo.ID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Empty(pr.Assignees)
	assert.Equal("[]", pr.AssigneesJSON)
}

func TestAPISetIssueAssigneesUpdatesProviderAndPersists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, syncer := setupAssigneeTestServer(t)
	seedIssue(t, database, "acme", "widget", 7, "open")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/issues/github/acme/widget/7/assignees", map[string][]string{
		"assignees": {"dana"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body assigneesAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"dana"}, body.Assignees)

	repo, err := database.GetRepoByIdentity(t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repo.ID, 7)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal([]string{"dana"}, issue.Assignees)

	providerIssue := providerClient.Issues["acme/widget"][0]
	require.Len(providerIssue.Assignees, 1)
	assert.Equal("dana", providerIssue.Assignees[0].GetLogin())

	syncer.RunOnce(t.Context())
	resynced, err := database.GetIssueByRepoIDAndNumber(t.Context(), repo.ID, 7)
	require.NoError(err)
	require.NotNil(resynced)
	assert.Equal([]string{"dana"}, resynced.Assignees)
}

func TestAPISetPullReviewersRequestsAndRemoves(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, _ := setupAssigneeTestServer(t)
	repoID := seedPR(t, database, "acme", "widget", 1)
	prID, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(prID)
	// The synced state already has carol requested; replacing the set
	// with alice must request alice and remove carol.
	require.NoError(database.UpdateMergeRequestReviewers(t.Context(), repoID, prID.ID, []string{"carol"}))

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/reviewers", map[string][]string{
		"reviewers": {"alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body reviewersAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"alice"}, body.Reviewers)

	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal([]string{"alice"}, pr.RequestedReviewers)

	providerPR := providerClient.PRs["acme/widget"][0]
	require.Len(providerPR.RequestedReviewers, 1)
	assert.Equal("alice", providerPR.RequestedReviewers[0].GetLogin())
}

func TestAPISetPullReviewersRemovesAllRequests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, _ := setupAssigneeTestServer(t)
	repoID := seedPR(t, database, "acme", "widget", 1)
	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NoError(database.UpdateMergeRequestReviewers(t.Context(), repoID, pr.ID, []string{"carol"}))

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/reviewers", map[string][]string{
		"reviewers": {},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body reviewersAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(body.Reviewers)
	assert.Empty(providerClient.PRs["acme/widget"][0].RequestedReviewers)

	updated, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	assert.Equal("[]", updated.ReviewersJSON)
}

func TestAPISetPullReviewersRemovesProviderReviewersWhenSyncedStateIsUnknown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, _ := setupAssigneeTestServer(t)
	// seedPR leaves reviewers_json empty, so the last synced state is
	// unknown. The provider still has carol requested; clearing the set
	// must consult the provider and remove her rather than reporting
	// success without any provider call.
	repoID := seedPR(t, database, "acme", "widget", 1)

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/reviewers", map[string][]string{
		"reviewers": {},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body reviewersAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(body.Reviewers)
	assert.Empty(providerClient.PRs["acme/widget"][0].RequestedReviewers)

	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal("[]", pr.ReviewersJSON)
}

func TestAPISetPullReviewersRemovesDriftedProviderReviewers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, providerClient, _ := setupAssigneeTestServer(t)
	repoID := seedPR(t, database, "acme", "widget", 1)
	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	// The synced row believes only alice is requested, but the provider
	// drifted: carol was requested outside kenn-forge. Replacing the set
	// with alice must still remove carol because the handler diffs
	// against live provider state.
	require.NoError(database.UpdateMergeRequestReviewers(t.Context(), repoID, pr.ID, []string{"alice"}))

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/reviewers", map[string][]string{
		"reviewers": {"alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body reviewersAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"alice"}, body.Reviewers)

	providerReviewers := providerClient.PRs["acme/widget"][0].RequestedReviewers
	require.Len(providerReviewers, 1)
	assert.Equal("alice", providerReviewers[0].GetLogin())
}

func TestAPISyncPersistsAssigneesAndRequestedReviewers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _, providerClient, syncer := setupAssigneeTestServer(t)

	providerPR := providerClient.PRs["acme/widget"][0]
	providerPR.Assignees = []*gh.User{{Login: new("erin")}}
	providerPR.RequestedReviewers = []*gh.User{{Login: new("frank")}}
	providerIssue := providerClient.Issues["acme/widget"][0]
	providerIssue.Assignees = []*gh.User{{Login: new("grace")}}

	syncer.RunOnce(t.Context())

	rr := doLabelAPIRequest(t, srv, http.MethodGet, "/api/v1/pulls/github/acme/widget/1", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var prDetail struct {
		MergeRequest struct {
			Assignees          []string `json:"assignees"`
			RequestedReviewers []string `json:"requested_reviewers"`
		} `json:"merge_request"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &prDetail))
	assert.Equal([]string{"erin"}, prDetail.MergeRequest.Assignees)
	assert.Equal([]string{"frank"}, prDetail.MergeRequest.RequestedReviewers)

	rr = doLabelAPIRequest(t, srv, http.MethodGet, "/api/v1/issues/github/acme/widget/7", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var issueDetail struct {
		Issue struct {
			Assignees []string `json:"assignees"`
		} `json:"issue"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &issueDetail))
	assert.Equal([]string{"grace"}, issueDetail.Issue.Assignees)
}

func TestAPISetPullAssigneesRejectsDuplicatesAndEmptyNames(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupAssigneeTestServer(t)
	seedPR(t, database, "acme", "widget", 1)

	for _, payload := range []map[string][]string{
		{"assignees": {"alice", "Alice"}},
		{"assignees": {"  "}},
	} {
		rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/assignees", payload)
		require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

// readOnlyProvider advertises read capabilities only, standing in for a
// provider that cannot mutate assignees or reviewers.
type readOnlyProvider struct {
	kind platform.Kind
	host string
}

func (p readOnlyProvider) Platform() platform.Kind { return p.kind }
func (p readOnlyProvider) Host() string            { return p.host }
func (p readOnlyProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories:  true,
		ReadMergeRequests: true,
		ReadIssues:        true,
	}
}

func TestAPIAssigneeAndReviewerMutationsAreCapabilityGated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	registry, err := ghclient.NewProviderRegistry(nil, readOnlyProvider{
		kind: platform.KindForgejo,
		host: "codeberg.org",
	})
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})

	_, err = database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       "forgejo",
		PlatformHost:   "codeberg.org",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)

	for _, tc := range []struct {
		name string
		path string
		body map[string][]string
	}{
		{name: "pull assignees", path: "/api/v1/pulls/forgejo/acme/widget/1/assignees", body: map[string][]string{"assignees": {"alice"}}},
		{name: "issue assignees", path: "/api/v1/issues/forgejo/acme/widget/1/assignees", body: map[string][]string{"assignees": {"alice"}}},
		{name: "pull reviewers", path: "/api/v1/pulls/forgejo/acme/widget/1/reviewers", body: map[string][]string{"reviewers": {"alice"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doLabelAPIRequest(t, srv, http.MethodPut, tc.path, tc.body)
			require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
			var problem struct {
				Code string `json:"code"`
			}
			require.NoError(json.Unmarshal(rr.Body.Bytes(), &problem))
			assert.Equal("unsupportedCapability", problem.Code)
		})
	}
}
