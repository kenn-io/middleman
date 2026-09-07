package kata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

func TestKataLinkRoutesUseStableProviderSubjects(t *testing.T) {
	requireRoot := require.New(t)
	kataDaemon := newKataLinkTestDaemon(t)
	configureKataLinkTestDaemon(t, kataDaemon.URL)
	srv, database := setupTestServer(t)

	providers := []platform.Kind{
		platform.KindGitHub,
		platform.KindGitLab,
		platform.KindForgejo,
		platform.KindGitea,
	}
	for providerIndex, provider := range providers {
		defaultHost, ok := platform.DefaultHost(provider)
		requireRoot.True(ok)
		for _, explicitHost := range []bool{false, true} {
			host := defaultHost
			if explicitHost {
				host = fmt.Sprintf("%s.example.test", provider)
			}
			for _, subjectKind := range []db.KataLinkSubjectKind{
				db.KataLinkSubjectIssue,
				db.KataLinkSubjectPullRequest,
			} {
				name := fmt.Sprintf("%s/%s/explicit=%t", provider, subjectKind, explicitHost)
				t.Run(name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					number := 100 + providerIndex*10
					if subjectKind == db.KataLinkSubjectPullRequest {
						number++
					}
					repoName := fmt.Sprintf("widget-%s-%t", provider, explicitHost)
					insertKataProviderSubject(t, database, provider, host, repoName, number, subjectKind, "item-"+name)

					base := kataProviderLinkRoute(provider, host, repoName, number, subjectKind, explicitHost)
					created := doJSON(t, srv, http.MethodPost, base, map[string]any{
						"daemon_id": "primary", "project_uid": "project-a", "issue_uid": "issue-a",
					})
					require.Equal(http.StatusOK, created.Code, created.Body.String())
					createdBody := decodeKataEffectiveLinks(t, created)
					require.Len(createdBody.Links, 1)
					link := createdBody.Links[0]
					assert.Equal("primary", link.DaemonID)
					assert.Equal("project-a:A-1", link.Reference)
					assert.Equal([]kataLinkProvenance{kataLinkDirect}, link.Provenance)
					require.NotNil(link.DirectLinkID)
					require.NotNil(link.Workspace)
					assert.False(link.Workspace.Available)
					assert.Equal("unmapped", link.Workspace.ResolutionStatus)
					assert.Equal(
						"No repository mapping matches this Kata project. Configure a mapping in Settings.",
						link.Workspace.UnavailableReason,
					)

					listed := doJSON(t, srv, http.MethodGet, base, nil)
					require.Equal(http.StatusOK, listed.Code, listed.Body.String())
					listedBody := decodeKataEffectiveLinks(t, listed)
					require.Len(listedBody.Links, 1)
					assert.Equal(*link.DirectLinkID, *listedBody.Links[0].DirectLinkID)

					deleted := doJSON(t, srv, http.MethodDelete,
						fmt.Sprintf("%s/%d", base, *link.DirectLinkID), nil)
					require.Equal(http.StatusNoContent, deleted.Code, deleted.Body.String())
					listed = doJSON(t, srv, http.MethodGet, base, nil)
					require.Equal(http.StatusOK, listed.Code, listed.Body.String())
					assert.Empty(decodeKataEffectiveLinks(t, listed).Links)
				})
			}
		}
	}
}

func TestKataLinkCreateKeepsResolvedSubjectAcrossRouteReuse(t *testing.T) {
	require := require.New(t)
	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	var started sync.Once
	kataDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.10.4"}`))
		case "/api/v1/issues/issue-a":
			started.Do(func() { close(validationStarted) })
			<-releaseValidation
			_, _ = w.Write([]byte(`{"issue":{"uid":"issue-a","project_uid":"project-a","title":"Linked task"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kataDaemon.Close)
	configureKataLinkTestDaemon(t, kataDaemon.URL)
	srv, database := setupTestServer(t)
	oldRepoID := insertKataProviderSubject(
		t, database, platform.KindGitHub, platform.DefaultGitHubHost,
		"widget", 42, db.KataLinkSubjectIssue, "item-original",
	)

	body := bytes.NewBufferString(`{"daemon_id":"primary","project_uid":"project-a","issue_uid":"issue-a"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/github/acme/widget/42/kata-links", body)
	req.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(created, req)
	}()

	select {
	case <-validationStarted:
	case <-time.After(2 * time.Second):
		require.FailNow("Kata validation did not start")
	}
	observedAt := time.Now().UTC().Add(time.Hour)
	renamed, accepted, err := database.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform:       string(platform.KindGitHub),
		PlatformHost:   platform.DefaultGitHubHost,
		PlatformRepoID: "repo-github-github.com-widget",
		Owner:          "acme",
		Name:           "widget-renamed",
	}, observedAt)
	require.NoError(err)
	require.True(accepted)
	require.Equal(oldRepoID, renamed.Repository.ID)
	replacement, accepted, err := database.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform:       string(platform.KindGitHub),
		PlatformHost:   platform.DefaultGitHubHost,
		PlatformRepoID: "repo-replacement",
		Owner:          "acme",
		Name:           "widget",
	}, observedAt.Add(time.Hour))
	require.NoError(err)
	require.True(accepted)
	_, err = database.UpsertIssue(t.Context(), &db.Issue{
		RepoID: replacement.Repository.ID, PlatformID: 42, PlatformExternalID: "item-replacement",
		Number: 42, URL: "https://github.com/acme/widget/issues/42",
		Title: "Replacement issue", Author: "user", State: "open",
		CreatedAt: observedAt, UpdatedAt: observedAt, LastActivityAt: observedAt,
	})
	require.NoError(err)

	close(releaseValidation)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow("Kata link creation did not finish")
	}
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	response := decodeKataEffectiveLinks(t, created)
	require.Len(response.Links, 1)
	require.Equal("issue-a", response.Links[0].IssueUID)

	links, err := database.ListKataIssueLinks(t.Context(), db.KataLinkSubject{
		Kind:                   db.KataLinkSubjectIssue,
		RepoID:                 oldRepoID,
		ProviderItemExternalID: "item-original",
	})
	require.NoError(err)
	require.Len(links, 1)
	require.Equal("issue-a", links[0].IssueUID)
}

func TestKataLinkRouteRequiresProviderExternalID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	kataDaemon := newKataLinkTestDaemon(t)
	configureKataLinkTestDaemon(t, kataDaemon.URL)
	srv, database := setupTestServer(t)
	insertKataProviderSubject(
		t, database, platform.KindGitHub, platform.DefaultGitHubHost,
		"widget", 42, db.KataLinkSubjectIssue, "",
	)

	rr := doJSON(t, srv, http.MethodPost,
		"/api/v1/issues/github/acme/widget/42/kata-links", map[string]any{
			"daemon_id": "primary", "project_uid": "project-a", "issue_uid": "issue-a",
		})
	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	problem := decodeProblem(t, rr)
	assert.Equal(httpapi.CodeResyncRequired, problem.Code)
	assert.Equal("issue", problem.Details["subject_kind"])
	assert.EqualValues(42, problem.Details["item_number"])

	var count int
	require.NoError(database.ReadDB().QueryRowContext(
		t.Context(), "SELECT COUNT(*) FROM kata_issue_links",
	).Scan(&count))
	assert.Zero(count)
}

func TestKataLinkRoutesHideRemovedProviderSubjects(t *testing.T) {
	kataDaemon := newKataLinkTestDaemon(t)
	configureKataLinkTestDaemon(t, kataDaemon.URL)
	srv, database := setupTestServer(t)

	for _, kind := range []db.KataLinkSubjectKind{
		db.KataLinkSubjectIssue,
		db.KataLinkSubjectPullRequest,
	} {
		t.Run(string(kind), func(t *testing.T) {
			require := require.New(t)
			number := 42
			externalID := "removed-item"
			repoID := insertKataProviderSubject(
				t, database, platform.KindGitHub, platform.DefaultGitHubHost,
				"widget-"+string(kind), number, kind, externalID,
			)
			created, err := database.CreateKataIssueLink(t.Context(), db.KataIssueLink{
				Subject: db.KataLinkSubject{
					Kind: kind, RepoID: repoID, ProviderItemExternalID: externalID,
				},
				DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
			})
			require.NoError(err)
			now := time.Now().UTC().Truncate(time.Second)
			itemType := db.ArchiveItemTypeIssue
			if kind == db.KataLinkSubjectPullRequest {
				itemType = db.ArchiveItemTypeMergeRequest
			}
			_, err = database.WriteDB().ExecContext(t.Context(), `
				INSERT INTO forge_archive_items (
					repo_id, item_type, item_number, provider_item_id,
					provider_created_at, provider_updated_at, lifecycle_state
				) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
				repoID, itemType, number, externalID, now, now,
			)
			require.NoError(err)

			base := kataProviderLinkRoute(
				platform.KindGitHub, platform.DefaultGitHubHost,
				"widget-"+string(kind), number, kind, false,
			)
			for _, request := range []struct {
				method string
				path   string
				body   any
			}{
				{method: http.MethodGet, path: base},
				{method: http.MethodPost, path: base, body: map[string]any{
					"daemon_id": "primary", "project_uid": "project-a", "issue_uid": "issue-a",
				}},
				{method: http.MethodDelete, path: fmt.Sprintf("%s/%d", base, created.ID)},
			} {
				rr := doJSON(t, srv, request.method, request.path, request.body)
				require.Equal(http.StatusNotFound, rr.Code, rr.Body.String())
			}
			links, err := database.ListKataIssueLinks(t.Context(), created.Subject)
			require.NoError(err)
			require.Len(links, 1)
			require.Equal(created.ID, links[0].ID)
		})
	}
}

func newKataLinkTestDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.10.4"}`))
		case "/api/v1/issues/issue-a":
			_, _ = w.Write([]byte(`{"issue":{"uid":"issue-a","project_uid":"project-a","title":"Linked task"},"comments":[],"links":[],"labels":[]}`))
		case "/api/v1/ui/references":
			_, _ = w.Write([]byte(`{"issues":[{"uid":"issue-a","project_uid":"project-a","project_name":"Project A","short_id":"A-1","qualified_id":"project-a:A-1","title":"Linked task","status":"open"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func configureKataLinkTestDaemon(t *testing.T, daemonURL string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	writeKataServerCatalog(t, home, `
[[daemon]]
name = "primary"
url = "`+daemonURL+`"
`)
}

func insertKataProviderSubject(
	t *testing.T,
	database *db.DB,
	provider platform.Kind,
	host, repoName string,
	number int,
	subjectKind db.KataLinkSubjectKind,
	externalID string,
) int64 {
	t.Helper()
	repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: string(provider), PlatformHost: host,
		PlatformRepoID: "repo-" + string(provider) + "-" + host + "-" + repoName,
		Owner:          "acme", Name: repoName,
	})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	switch subjectKind {
	case db.KataLinkSubjectIssue:
		_, err = database.UpsertIssue(t.Context(), &db.Issue{
			RepoID: repoID, PlatformID: int64(number), PlatformExternalID: externalID,
			Number: number, URL: "https://" + host + "/acme/" + repoName + "/issues/1",
			Title: "Issue", Author: "user", State: "open",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		})
	case db.KataLinkSubjectPullRequest:
		_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), PlatformExternalID: externalID,
			Number: number, URL: "https://" + host + "/acme/" + repoName + "/pulls/1",
			Title: "Pull", Author: "user", State: "open", HeadBranch: "feature", BaseBranch: "main",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		})
	}
	require.NoError(t, err)
	return repoID
}

func kataProviderLinkRoute(
	provider platform.Kind,
	host, repoName string,
	number int,
	subjectKind db.KataLinkSubjectKind,
	explicitHost bool,
) string {
	prefix := "/api/v1"
	if explicitHost {
		prefix += "/host/" + host
	}
	resource := "issues"
	if subjectKind == db.KataLinkSubjectPullRequest {
		resource = "pulls"
	}
	return fmt.Sprintf("%s/%s/%s/acme/%s/%d/kata-links", prefix, resource, provider, repoName, number)
}

func decodeKataEffectiveLinks(t *testing.T, rr *httptest.ResponseRecorder) kataEffectiveLinksResponse {
	t.Helper()
	var response kataEffectiveLinksResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
	return response
}
