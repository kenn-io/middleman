package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
)

type labelAPIResponse struct {
	Labels  []db.Label `json:"labels"`
	Stale   bool       `json:"stale"`
	Syncing bool       `json:"syncing"`
}

type blockingLabelClient struct {
	*testutil.FixtureClient
	entered chan struct{}
	release chan struct{}
}

func (c *blockingLabelClient) ListRepoLabels(
	ctx context.Context, owner, repo string,
) ([]*gh.Label, error) {
	select {
	case <-c.entered:
	default:
		close(c.entered)
	}
	select {
	case <-c.release:
		return c.FixtureClient.ListRepoLabels(ctx, owner, repo)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func setupLabelTestServer(t *testing.T) (*server.Server, *db.DB, *testutil.FixtureClient, *ghclient.Syncer) {
	t.Helper()
	database := dbtest.Open(t)
	client := testutil.NewFixtureClient().(*testutil.FixtureClient)
	client.Labels["acme/widget"] = []*gh.Label{
		{Name: new("bug"), Description: new("Something is broken"), Color: new("d73a4a"), Default: new(true)},
		{Name: new("triage"), Description: new("Needs review"), Color: new("fbca04")},
	}
	pr := &gh.PullRequest{
		ID:        new(int64),
		Number:    new(int),
		Title:     new(string),
		State:     new(string),
		CreatedAt: &gh.Timestamp{Time: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)},
		UpdatedAt: &gh.Timestamp{Time: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)},
	}
	*pr.ID = 101
	*pr.Number = 1
	*pr.Title = "Improve widgets"
	*pr.State = "open"
	client.PRs["acme/widget"] = []*gh.PullRequest{pr}
	client.OpenPRs["acme/widget"] = []*gh.PullRequest{pr}
	client.Issues["acme/widget"] = []*gh.Issue{{Number: new(int)}}
	*client.Issues["acme/widget"][0].Number = 7
	syncer := ghclient.NewSyncer(map[string]ghclient.Client{"github.com": client}, database, nil, defaultTestRepos, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})
	return srv, database, client, syncer
}

func doLabelAPIRequest(
	t *testing.T,
	srv http.Handler,
	method string,
	path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	req := httptest.NewRequest(method, path, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func seedRepoLabelCatalog(
	t *testing.T,
	database *db.DB,
	owner string,
	name string,
) int64 {
	t.Helper()
	ctx := t.Context()
	repo, err := database.GetRepoByIdentity(ctx, db.GitHubRepoIdentity("github.com", owner, name))
	require.NoError(t, err)
	require.NotNil(t, repo)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	require.NoError(t, database.ReplaceRepoLabelCatalog(ctx, repo.ID, []db.Label{
		{Name: "bug", Description: "Something is broken", Color: "d73a4a", IsDefault: true, UpdatedAt: now},
		{Name: "triage", Description: "Needs review", Color: "fbca04", UpdatedAt: now},
	}, now))
	return repo.ID
}

func TestAPIListRepoLabelsRefreshesStaleCatalogFromProvider(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	repo, err := database.GetRepoByIdentity(t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)

	rr := doLabelAPIRequest(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget/labels", nil)
	require.Equal(http.StatusOK, rr.Code)

	require.Eventually(func() bool {
		labels, _, err := database.ListRepoLabelCatalog(t.Context(), repo.ID)
		return err == nil && len(labels) == 2 && labels[0].Name == "bug" && labels[1].Name == "triage"
	}, time.Second, 10*time.Millisecond)
}

func TestAPIListRepoLabelsReturnsCachedCatalogWhileRefreshRuns(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	baseClient := testutil.NewFixtureClient().(*testutil.FixtureClient)
	baseClient.Labels["acme/widget"] = []*gh.Label{
		{Name: new("bug"), Description: new("Something is broken"), Color: new("d73a4a"), Default: new(true)},
		{Name: new("triage"), Description: new("Needs review"), Color: new("fbca04")},
	}
	client := &blockingLabelClient{
		FixtureClient: baseClient,
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	defer close(client.release)
	syncer := ghclient.NewSyncer(map[string]ghclient.Client{"github.com": client}, database, nil, defaultTestRepos, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})
	seedPR(t, database, "acme", "widget", 1)
	seedRepoLabelCatalog(t, database, "acme", "widget")

	type result struct {
		rr *httptest.ResponseRecorder
	}
	resultCh := make(chan result, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repo/github/acme/widget/labels", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		resultCh <- result{rr: rr}
	}()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		require.Fail("label catalog refresh did not start")
	}

	select {
	case got := <-resultCh:
		require.Equal(http.StatusOK, got.rr.Code)
		var body labelAPIResponse
		require.NoError(json.Unmarshal(got.rr.Body.Bytes(), &body))
		require.True(body.Syncing)
		require.True(body.Stale)
		require.Len(body.Labels, 2)
	case <-time.After(5 * time.Second):
		require.Fail("GET /labels blocked on provider refresh")
	}
}

func TestAPIListRepoLabelsReturnsCachedCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget/labels", nil)
	require.Equal(http.StatusOK, rr.Code)

	var body labelAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(body.Labels, 2)
	assert.Equal("bug", body.Labels[0].Name)
	assert.Equal("triage", body.Labels[1].Name)
}

func TestAPISetPullLabelsReplacesAssignedLabelsFromCatalog(t *testing.T) {
	require := require.New(t)
	srv, database, providerClient, syncer := setupLabelTestServer(t)
	seedPRWithLabels(t, database, "acme", "widget", 1, []db.Label{{Name: "bug", Color: "d73a4a"}})
	repoID := seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/labels", map[string][]string{
		"labels": {"triage"},
	})
	require.Equal(http.StatusOK, rr.Code)

	var body labelAPIResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(body.Labels, 1)
	require.Equal("triage", body.Labels[0].Name)

	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(pr)
	require.Len(pr.Labels, 1)
	require.Equal("triage", pr.Labels[0].Name)
	require.Len(providerClient.PRs["acme/widget"][0].Labels, 1)
	require.Equal("triage", providerClient.PRs["acme/widget"][0].Labels[0].GetName())

	syncer.RunOnce(t.Context())
	resynced, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 1)
	require.NoError(err)
	require.NotNil(resynced)
	require.Len(resynced.Labels, 1)
	require.Equal("triage", resynced.Labels[0].Name)
}

func TestAPISetPullLabelsChecksVisibilityBeforeCatalogRefresh(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	repo, err := database.GetRepoByIdentity(
		t.Context(), db.GitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	markArchiveItemLifecycle(
		t, database, repo.ID, db.ArchiveItemTypeMergeRequest, 1,
		db.ArchiveLifecycleStateRemovedUpstream,
	)

	rr := doLabelAPIRequest(
		t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/labels",
		map[string][]string{"labels": {"bug"}},
	)
	require.Equal(http.StatusNotFound, rr.Code, rr.Body.String())

	catalog, freshness, err := database.ListRepoLabelCatalog(t.Context(), repo.ID)
	require.NoError(err)
	require.Empty(catalog)
	require.Nil(freshness.CheckedAt)
}

func TestAPISetIssueLabelsReplacesAssignedLabelsViaProvider(t *testing.T) {
	require := require.New(t)
	srv, database, providerClient, _ := setupLabelTestServer(t)
	seedIssue(t, database, "acme", "widget", 7, "open")
	repoID := seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/issues/github/acme/widget/7/labels", map[string][]string{
		"labels": {"triage"},
	})
	require.Equal(http.StatusOK, rr.Code)

	issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	require.Equal("triage", issue.Labels[0].Name)
	require.Len(providerClient.Issues["acme/widget"][0].Labels, 1)
	require.Equal("triage", providerClient.Issues["acme/widget"][0].Labels[0].GetName())
}

func TestAPISetIssueLabelsRejectsLabelsOutsideCatalog(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedIssue(t, database, "acme", "widget", 7, "open")
	seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/issues/github/acme/widget/7/labels", map[string][]string{
		"labels": {"not-in-catalog"},
	})
	require.Equal(http.StatusBadRequest, rr.Code)
}

func TestAPISetPullLabelsRejectsMissingLabelsField(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/labels", map[string]any{})
	require.Equal(http.StatusUnprocessableEntity, rr.Code)
}

func TestAPISetPullLabelsRejectsNullLabels(t *testing.T) {
	require := require.New(t)
	srv, database, _, _ := setupLabelTestServer(t)
	seedPR(t, database, "acme", "widget", 1)
	seedRepoLabelCatalog(t, database, "acme", "widget")

	rr := doLabelAPIRequest(t, srv, http.MethodPut, "/api/v1/pulls/github/acme/widget/1/labels", map[string]any{"labels": nil})
	require.Equal(http.StatusUnprocessableEntity, rr.Code)
}
