package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func TestAPIArchiveRoutesRemainRegisteredWithoutController(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/status", http.NoBody)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(http.StatusServiceUnavailable, rr.Code)
	var problem httpapi.ProblemError
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &problem))
	assert.Equal(httpapi.CodeServiceUnavailable, problem.Code)
}

func TestAPIArchiveStartPauseStatusAndReport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, provider, wakeCount, ref := setupArchiveTestServer(t, nil)
	client := setupTestClient(t, srv)
	repositories := []generated.ArchiveRepositoryRef{archiveGeneratedRef(ref)}

	started, err := client.HTTP.StartArchivesWithResponse(t.Context(), generated.ArchiveMutationBody{
		Repositories: &repositories,
	})
	require.NoError(err)
	require.NotNil(started.JSON200)
	require.Len(*started.JSON200, 1)
	assert.Equal(generated.ArchiveStatusResponseStatusRunning, (*started.JSON200)[0].Status)
	assert.Equal("github.test", (*started.JSON200)[0].Repository.PlatformHost)
	assert.Equal([]generated.ArchiveStatusResponseActivePhases{
		generated.IssueInventory, generated.MergeRequestInventory,
	}, (*started.JSON200)[0].ActivePhases)
	assert.Equal(generated.ArchiveCoverageResponseCommentsSupported, (*started.JSON200)[0].Coverage.Comments)
	assert.NotNil((*started.JSON200)[0].InitialStartedAt)
	assert.Equal(int32(1), wakeCount.Load())
	assert.Zero(provider.calls.Load(), "start must only mutate durable state and wake the worker")

	startedAgain, err := client.HTTP.StartArchivesWithResponse(t.Context(), generated.ArchiveMutationBody{
		Repositories: &repositories,
	})
	require.NoError(err)
	require.NotNil(startedAgain.JSON200)
	assert.Equal((*started.JSON200)[0].Status, (*startedAgain.JSON200)[0].Status)

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	repo, err := database.GetRepoByIdentity(t.Context(), platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	issueResult, err := database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_issues (
			repo_id, platform_id, platform_external_id, number, url, title, author,
			state, body, comment_count, created_at, updated_at, last_activity_at, closed_at
		) VALUES (?, 7, 'issue-7', 7, 'https://github.test/owner/repo/issues/7',
			'Synthetic issue', 'alice', 'closed', 'body', 3, ?, ?, ?, ?)`,
		repo.ID, now, now, now, now.Add(10*time.Minute))
	require.NoError(err)
	issueID, err := issueResult.LastInsertId()
	require.NoError(err)
	require.NoError(database.UpsertIssueEvents(t.Context(), []db.IssueEvent{{
		IssueID: issueID, PlatformExternalID: "issue-closed-7", EventType: "closed",
		Author: "closer", CreatedAt: now.Add(10 * time.Minute), DedupeKey: "closed:7",
	}}))
	filesChanged := 5
	mergedAt := now.Add(20 * time.Minute)
	mrID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repo.ID, PlatformID: 8, PlatformExternalID: "mr-8", Number: 8,
		URL: "https://github.test/owner/repo/pull/8", Title: "Synthetic merge request",
		Author: "bob", State: db.MergeRequestStateMerged, Body: "body",
		Additions: 20, Deletions: 4, FilesChanged: &filesChanged, MergeCommitSHA: "abc123",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: mergedAt,
		LastActivityAt: mergedAt, MergedAt: &mergedAt,
	})
	require.NoError(err)
	require.NoError(database.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: mrID, PlatformExternalID: "mr-merged-8", EventType: "merged",
		Author: "merger", CreatedAt: mergedAt, DedupeKey: "merged:8",
	}}))
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET issues_coverage = 'supported', merge_requests_coverage = 'supported'
		WHERE repo_id = ?`, repo.ID)
	require.NoError(err)
	verbose := true
	reportResponse, err := client.HTTP.GetArchiveReportWithResponse(t.Context(), &generated.GetArchiveReportParams{
		Start: now.Add(-time.Hour).Format(time.RFC3339),
		End:   now.Add(time.Hour).Format(time.RFC3339), Verbose: &verbose,
	})
	require.NoError(err)
	require.NotNil(reportResponse.JSON200)
	assert.Equal(report.Schema, reportResponse.JSON200.ReportSchema)
	assert.Equal(int64(1), reportResponse.JSON200.Totals.IssuesOpened)
	assert.Equal(int64(1), reportResponse.JSON200.Totals.IssuesClosed)
	assert.Equal(int64(1), reportResponse.JSON200.Totals.MergeRequestsMerged)
	require.NotNil(reportResponse.JSON200.Repositories)
	require.Len(reportResponse.JSON200.Repositories, 1)
	assert.Equal(generated.ArchiveReportCoverageResponseIssuesSupported,
		reportResponse.JSON200.Repositories[0].Coverage.Issues)
	assert.Equal(generated.ArchiveReportCoverageResponseMergeRequestsSupported,
		reportResponse.JSON200.Repositories[0].Coverage.MergeRequests)
	require.NotNil(reportResponse.JSON200.Activity)
	require.Len(*reportResponse.JSON200.Activity, 3)
	assert.Equal("issue-7", (*reportResponse.JSON200.Activity)[0].ProviderExternalId)
	assert.Equal(generated.ArchiveReportActivityResponseKindIssueClosed,
		(*reportResponse.JSON200.Activity)[1].Kind)
	require.NotNil((*reportResponse.JSON200.Activity)[1].Actor)
	assert.Equal("closer", *(*reportResponse.JSON200.Activity)[1].Actor)
	merged := (*reportResponse.JSON200.Activity)[2]
	assert.Equal(generated.ArchiveReportActivityResponseKindMergeRequestMerged, merged.Kind)
	require.NotNil(merged.Actor)
	assert.Equal("merger", *merged.Actor)
	require.NotNil(merged.Additions)
	assert.Equal(int64(20), *merged.Additions)
	require.NotNil(merged.Deletions)
	assert.Equal(int64(4), *merged.Deletions)
	require.NotNil(merged.FilesChanged)
	assert.Equal(int64(5), *merged.FilesChanged)
	require.NotNil(merged.MergeCommitSha)
	assert.Equal("abc123", *merged.MergeCommitSha)
	assert.Zero(provider.calls.Load(), "reports must read SQLite only")

	filter := []string{"github|github.test/owner/repo"}
	statusResponse, err := client.HTTP.ListArchiveStatusWithResponse(
		t.Context(), &generated.ListArchiveStatusParams{Repo: &filter},
	)
	require.NoError(err)
	require.NotNil(statusResponse.JSON200)
	require.Len(*statusResponse.JSON200, 1)
	assert.Equal("github", (*statusResponse.JSON200)[0].Repository.Provider)

	resetAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET last_error_code = 'budget_exhausted',
			last_error_detail = '/private/path?token=should-not-leak', next_retry_at = ?
		WHERE repo_id = ?`, resetAt, repo.ID)
	require.NoError(err)
	budgetStatus, err := client.HTTP.ListArchiveStatusWithResponse(
		t.Context(), &generated.ListArchiveStatusParams{Repo: &filter},
	)
	require.NoError(err)
	require.NotNil(budgetStatus.JSON200)
	require.Len(*budgetStatus.JSON200, 1)
	assert.Equal(generated.ArchiveStatusResponseStatusWaitingForBudget, (*budgetStatus.JSON200)[0].Status)
	require.NotNil((*budgetStatus.JSON200)[0].BudgetWaitUntil)
	assert.Equal(resetAt, *(*budgetStatus.JSON200)[0].BudgetWaitUntil)
	require.NotNil((*budgetStatus.JSON200)[0].Failure)
	assert.Equal("budget_exhausted", (*budgetStatus.JSON200)[0].Failure.Code)
	assert.NotContains(string(budgetStatus.Body), "should-not-leak")
	assert.NotContains(string(budgetStatus.Body), "/private/path")

	paused, err := client.HTTP.PauseArchivesWithResponse(t.Context(), generated.ArchiveMutationBody{
		Repositories: &repositories,
	})
	require.NoError(err)
	require.NotNil(paused.JSON200)
	assert.Equal(generated.ArchiveStatusResponseStatusPaused, (*paused.JSON200)[0].Status)

	startedAll, err := client.HTTP.StartArchivesWithResponse(
		t.Context(), generated.ArchiveMutationBody{All: true},
	)
	require.NoError(err)
	require.NotNil(startedAll.JSON200)
	require.Len(*startedAll.JSON200, 1)
	assert.Equal(generated.ArchiveStatusResponseStatusWaitingForBudget, (*startedAll.JSON200)[0].Status,
		"idempotent start preserves an active provider budget wait")
}

func TestAPIArchiveReportExcludesOnlyRemovedUpstreamParents(t *testing.T) {
	require := require.New(t)
	srv, database, provider, _, ref := setupArchiveTestServer(t, nil)
	client := setupTestClient(t, srv)
	ctx := t.Context()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	repo, err := database.GetRepoByIdentity(ctx, platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)

	insertIssue := func(number int, externalID, lifecycle string) int64 {
		result, insertErr := database.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_issues (
				repo_id, platform_id, platform_external_id, number, url, title, author,
				state, body, created_at, updated_at, last_activity_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, 'open', 'body', ?, ?, ?)`,
			repo.ID, number, externalID, number,
			"https://github.test/owner/repo/issues/"+fmt.Sprint(number),
			"Synthetic issue "+fmt.Sprint(number), "issue-author", now, now, now,
		)
		require.NoError(insertErr)
		id, insertErr := result.LastInsertId()
		require.NoError(insertErr)
		_, insertErr = database.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (?, 'issue', ?, ?, ?, ?, ?)`,
			repo.ID, number, externalID, now, now, lifecycle,
		)
		require.NoError(insertErr)
		return id
	}
	insertMR := func(number int, externalID, lifecycle string) int64 {
		id, insertErr := database.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repo.ID, PlatformID: int64(number), PlatformExternalID: externalID,
			Number: number, URL: "https://github.test/owner/repo/pull/" + fmt.Sprint(number),
			Title: "Synthetic merge request " + fmt.Sprint(number), Author: "pr-author",
			State: db.MergeRequestStateOpen, Body: "body",
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		})
		require.NoError(insertErr)
		_, insertErr = database.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (?, 'merge_request', ?, ?, ?, ?, ?)`,
			repo.ID, number, externalID, now, now, lifecycle,
		)
		require.NoError(insertErr)
		return id
	}

	inaccessibleIssueID := insertIssue(1, "inaccessible-issue-1", "inaccessible")
	removedIssueID := insertIssue(2, "removed-issue-2", "removed_upstream")
	inaccessibleMRID := insertMR(3, "inaccessible-pr-3", "inaccessible")
	removedMRID := insertMR(4, "removed-pr-4", "removed_upstream")
	require.NoError(database.UpsertIssueEvents(ctx, []db.IssueEvent{
		{IssueID: inaccessibleIssueID, PlatformExternalID: "inaccessible-issue-comment", EventType: "issue_comment", Author: "commenter", CreatedAt: now.Add(time.Minute), DedupeKey: "inaccessible-issue-comment"},
		{IssueID: removedIssueID, PlatformExternalID: "removed-issue-comment", EventType: "issue_comment", Author: "commenter", CreatedAt: now.Add(time.Minute), DedupeKey: "removed-issue-comment"},
	}))
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{
		{MergeRequestID: inaccessibleMRID, PlatformExternalID: "inaccessible-pr-review", EventType: "review", Author: "reviewer", CreatedAt: now.Add(2 * time.Minute), DedupeKey: "inaccessible-pr-review"},
		{MergeRequestID: removedMRID, PlatformExternalID: "removed-pr-review", EventType: "review", Author: "reviewer", CreatedAt: now.Add(2 * time.Minute), DedupeKey: "removed-pr-review"},
	}))
	_, err = database.WriteDB().ExecContext(ctx, `
		UPDATE forge_archive_repos
		SET issues_coverage = 'supported', merge_requests_coverage = 'supported'
		WHERE repo_id = ?`, repo.ID)
	require.NoError(err)

	var removedCount int
	require.NoError(database.ReadDB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM forge_archive_items
		WHERE repo_id = ? AND lifecycle_state = 'removed_upstream'`, repo.ID,
	).Scan(&removedCount))
	require.Equal(2, removedCount)

	verbose := true
	response, err := client.HTTP.GetArchiveReportWithResponse(ctx, &generated.GetArchiveReportParams{
		Start:   now.Add(-time.Hour).Format(time.RFC3339),
		End:     now.Add(time.Hour).Format(time.RFC3339),
		Verbose: &verbose,
	})
	require.NoError(err)
	require.Equal(http.StatusOK, response.StatusCode())
	require.NotNil(response.JSON200)
	require.EqualValues(1, response.JSON200.Totals.IssuesOpened)
	require.EqualValues(1, response.JSON200.Totals.MergeRequestsOpened)
	require.EqualValues(1, response.JSON200.Totals.OrdinaryComments)
	require.EqualValues(1, response.JSON200.Totals.ReviewsSubmitted)
	require.NotNil(response.JSON200.Activity)
	require.Len(*response.JSON200.Activity, 4)
	for _, activity := range *response.JSON200.Activity {
		require.NotEqualValues(2, activity.ItemNumber)
		require.NotEqualValues(4, activity.ItemNumber)
	}
	require.Zero(provider.calls.Load(), "reports must read SQLite only")
}

func TestAPIArchiveValidationAndLimitProblemDetails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database, _, _, ref := setupArchiveTestServer(t, nil)
	client := setupTestClient(t, srv)
	repositories := []generated.ArchiveRepositoryRef{archiveGeneratedRef(ref)}

	invalidMutation, err := client.HTTP.StartArchivesWithResponse(t.Context(), generated.ArchiveMutationBody{
		All: true, Repositories: &repositories,
	})
	require.NoError(err)
	require.NotNil(invalidMutation.ApplicationproblemJSONDefault)
	assert.Equal(generated.ValidationError, invalidMutation.ApplicationproblemJSONDefault.Code)

	emptyMutation, err := client.HTTP.StartArchivesWithResponse(
		t.Context(), generated.ArchiveMutationBody{},
	)
	require.NoError(err)
	require.NotNil(emptyMutation.ApplicationproblemJSONDefault)
	assert.Equal(generated.ValidationError, emptyMutation.ApplicationproblemJSONDefault.Code)

	missingRepository := archiveGeneratedRef(ref)
	missingRepository.Name = "missing"
	missingRepository.RepoPath = "owner/missing"
	mixedRepositories := []generated.ArchiveRepositoryRef{
		archiveGeneratedRef(ref), missingRepository,
	}
	mixedMutation, err := client.HTTP.StartArchivesWithResponse(t.Context(), generated.ArchiveMutationBody{
		Repositories: &mixedRepositories,
	})
	require.NoError(err)
	require.NotNil(mixedMutation.ApplicationproblemJSONDefault)
	assert.Equal(generated.BadRequest, mixedMutation.ApplicationproblemJSONDefault.Code)
	repo, err := database.GetRepoByIdentity(t.Context(), platform.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveCollectionModeDiscovery, states[0].CollectionMode,
		"membership validation must finish before any repository is promoted")

	offsetReport, err := client.HTTP.GetArchiveReportWithResponse(t.Context(), &generated.GetArchiveReportParams{
		Start: "2026-07-01T00:00:00+01:00", End: "2026-07-02T00:00:00Z",
	})
	require.NoError(err)
	require.NotNil(offsetReport.ApplicationproblemJSONDefault)
	assert.Equal(generated.ValidationError, offsetReport.ApplicationproblemJSONDefault.Code)

	missing := []string{"github|github.test/owner/missing"}
	missingReport, err := client.HTTP.GetArchiveReportWithResponse(t.Context(), &generated.GetArchiveReportParams{
		Start: "2026-07-01T00:00:00Z", End: "2026-07-02T00:00:00Z", Repo: &missing,
	})
	require.NoError(err)
	require.NotNil(missingReport.ApplicationproblemJSONDefault)
	assert.Equal(generated.BadRequest, missingReport.ApplicationproblemJSONDefault.Code)

	limitSrv, _, _, _, _ := setupArchiveTestServer(t, archiveLimitController{})
	limitClient := setupTestClient(t, limitSrv)
	tooLarge, err := limitClient.HTTP.GetArchiveReportWithResponse(t.Context(), &generated.GetArchiveReportParams{
		Start: "2026-07-01T00:00:00Z", End: "2026-07-02T00:00:00Z",
	})
	require.NoError(err)
	assert.Equal(http.StatusRequestEntityTooLarge, tooLarge.StatusCode())
	require.NotNil(tooLarge.ApplicationproblemJSONDefault)
	assert.Equal(generated.PayloadTooLarge, tooLarge.ApplicationproblemJSONDefault.Code)
	require.NotNil(tooLarge.ApplicationproblemJSONDefault.Details)
	details := *tooLarge.ApplicationproblemJSONDefault.Details
	encodedDetails, err := json.Marshal(details)
	require.NoError(err)
	assert.JSONEq(`{
		"reason": "reportTooLarge",
		"observedRecords": 10001,
		"maxRecords": 10000,
		"observedTextBytes": 33554433,
		"maxTextBytes": 33554432
	}`, string(encodedDetails))
}

func TestAPIArchiveRoutesObeyHostAuthAndCSRFGuards(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _, _, _ := setupArchiveTestServer(t, nil)

	crossSite := httptest.NewRequest(
		http.MethodPost, "/api/v1/archive/start", strings.NewReader(`{"all":true}`),
	)
	crossSite.Host = "forge.test"
	crossSite.Header.Set("Content-Type", "application/json")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteRecorder := httptest.NewRecorder()
	srv.ServeHTTP(crossSiteRecorder, crossSite)
	assert.Equal(http.StatusForbidden, crossSiteRecorder.Code)

	badHost := httptest.NewRequest(http.MethodGet, "/api/v1/archive/status", http.NoBody)
	badHost.Host = "attacker.example"
	badHostRecorder := httptest.NewRecorder()
	srv.ServeHTTP(badHostRecorder, badHost)
	assert.Equal(http.StatusForbidden, badHostRecorder.Code)

	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	authServer := server.New(database, syncer, nil, "/", nil, server.ServerOptions{
		DaemonAccess: server.DaemonAccessOptions{
			Token: "archive-test-token", RequireAPIAuth: true,
		},
		Archive: archiveStatusController{},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(authServer.Shutdown(ctx))
	})
	authClient := setupTestClient(t, authServer)
	unauthorized, err := authClient.HTTP.ListArchiveStatusWithResponse(
		t.Context(), &generated.ListArchiveStatusParams{},
	)
	require.NoError(err)
	assert.Equal(http.StatusUnauthorized, unauthorized.StatusCode())
	require.NotNil(unauthorized.ApplicationproblemJSONDefault)
	assert.Equal(generated.Unauthorized, unauthorized.ApplicationproblemJSONDefault.Code)

	authorized, err := authClient.HTTP.ListArchiveStatusWithResponse(
		t.Context(), &generated.ListArchiveStatusParams{},
		func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer archive-test-token")
			return nil
		},
	)
	require.NoError(err)
	require.NotNil(authorized.JSON200)
	assert.Empty(*authorized.JSON200)
}

type archiveAPITestProvider struct{ calls atomic.Int32 }

func (p *archiveAPITestProvider) Platform() platform.Kind { return platform.KindGitHub }
func (p *archiveAPITestProvider) Host() string            { return "github.test" }
func (p *archiveAPITestProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		Archive: platform.ArchiveCapabilities{
			HistoricalIssues: true, HistoricalMergeRequests: true,
			OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
		},
	}
}
func (p *archiveAPITestProvider) pageCall() { p.calls.Add(1) }
func (p *archiveAPITestProvider) ListIssuesPage(context.Context, platform.RepoRef, platform.ItemPageQuery) (platform.Page[platform.Issue], error) {
	p.pageCall()
	return platform.Page[platform.Issue]{Exhausted: true}, nil
}
func (p *archiveAPITestProvider) ListMergeRequestsPage(context.Context, platform.RepoRef, platform.ItemPageQuery) (platform.Page[platform.MergeRequest], error) {
	p.pageCall()
	return platform.Page[platform.MergeRequest]{Exhausted: true}, nil
}

type archiveAPITestSource struct{ refs []platform.RepoRef }

func (s archiveAPITestSource) ConfiguredRepositories(context.Context) ([]platform.RepoRef, error) {
	return s.refs, nil
}

type archiveLimitController struct{ archive.Controller }

func (archiveLimitController) Report(context.Context, archive.ReportOptions) (report.Model, error) {
	return report.Model{}, &report.LimitError{
		ObservedRecords: 10_001, MaxRecords: 10_000,
		ObservedTextBytes: 33_554_433, MaxTextBytes: 33_554_432,
	}
}

type archiveStatusController struct{ archive.Controller }

func (archiveStatusController) Status(context.Context, []platform.RepoRef) ([]archive.Status, error) {
	return []archive.Status{}, nil
}

func setupArchiveTestServer(
	t *testing.T,
	controller archive.Controller,
) (*server.Server, *db.DB, *archiveAPITestProvider, *atomic.Int32, platform.RepoRef) {
	t.Helper()
	database := dbtest.Open(t)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test", Owner: "owner",
		Name: "repo", RepoPath: "owner/repo", PlatformExternalID: "repo-owner-repo",
	}
	_, err := database.UpsertRepo(t.Context(), platform.DBRepoIdentity(ref))
	require.NoError(t, err)
	provider := &archiveAPITestProvider{}
	wakeCount := &atomic.Int32{}
	if controller == nil {
		registry, registryErr := platform.NewRegistry(provider)
		require.NoError(t, registryErr)
		service, serviceErr := archive.NewService(
			database, registry, nil,
			archiveAPITestSource{refs: []platform.RepoRef{ref}}, nil, nil,
		)
		require.NoError(t, serviceErr)
		service.SetWake(func() { wakeCount.Add(1) })
		requireEnsureConfigured(t, service, []platform.RepoRef{ref})
		controller = service
	}
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := server.New(database, syncer, nil, "/", nil, server.ServerOptions{Archive: controller})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})
	return srv, database, provider, wakeCount, ref
}

func archiveGeneratedRef(ref platform.RepoRef) generated.ArchiveRepositoryRef {
	return generated.ArchiveRepositoryRef{
		Provider: string(ref.Platform), PlatformHost: ref.Host, Owner: ref.Owner,
		Name: ref.Name, RepoPath: ref.RepoPath,
	}
}

func TestAPIArchivePacingReportsProviderHeadroom(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	registry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.test", Principal: "user:7"}
	reset := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	registry.UpdateSnapshot(identity, ghclient.QuotaResourceREST,
		ghclient.Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	registry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL,
		ghclient.Rate{Limit: 5000, Remaining: 4200, Reset: reset})
	syncer.SetQuotaRegistry(registry)
	srv := server.New(database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/pacing", http.NoBody)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var rows []struct {
		Provider     string `json:"provider"`
		PlatformHost string `json:"platform_host"`
		Principal    string `json:"principal"`
		Source       string `json:"source"`
		Known        bool   `json:"known"`
		Limit        int    `json:"limit"`
		Remaining    int    `json:"remaining"`
		Reserve      int    `json:"reserve"`
		Available    int    `json:"available"`
		ResetAt      string `json:"reset_at"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &rows))
	require.Len(rows, 1)
	row := rows[0]
	assert.Equal("github", row.Provider)
	assert.Equal("github.test", row.PlatformHost)
	assert.Equal("user:7", row.Principal)
	assert.Equal("provider", row.Source)
	assert.True(row.Known)
	// Limit and remaining are the min across REST and GraphQL, matching what
	// archive admission consumes.
	assert.Equal(5000, row.Limit)
	assert.Equal(4200, row.Remaining)
	assert.Equal(1000, row.Reserve)
	assert.Equal(3200, row.Available)
	assert.Equal(reset.Format(time.RFC3339), row.ResetAt)
}

func TestAPIArchivePacingEmptyWithoutKnownPools(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	srv := server.New(database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/pacing", http.NoBody)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var rows []map[string]any
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &rows))
	require.Empty(rows)
}

// A credential whose pacing window cannot be combined (a pool missing,
// expired, or never observed) must still appear, marked unknown: those are
// exactly the identities archive admission defers as "provider quota
// unknown", and omitting them would hide why hydration is blocked.
func TestAPIArchivePacingReportsPartiallyKnownCredentials(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	registry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.test", Principal: "user:7"}
	reset := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	registry.UpdateSnapshot(identity, ghclient.QuotaResourceREST,
		ghclient.Rate{Limit: 5000, Remaining: 4500, Reset: reset})
	syncer.SetQuotaRegistry(registry)
	srv := server.New(database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/pacing", http.NoBody)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var rows []struct {
		Principal string `json:"principal"`
		Known     bool   `json:"known"`
		Available int    `json:"available"`
		ResetAt   string `json:"reset_at"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &rows))
	require.Len(rows, 1)
	assert.Equal("user:7", rows[0].Principal)
	assert.False(rows[0].Known)
	assert.Zero(rows[0].Available)
	assert.Empty(rows[0].ResetAt)
}

// With unequal pool limits, the reported reserve and availability come from
// per-pool headroom: a large pool at its own limit/5 floor zeroes archive
// availability even though the smallest pool still has headroom.
func TestAPIArchivePacingUsesPerPoolReserves(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	registry := ghclient.NewQuotaRegistry()
	identity := ghclient.IdentityKey{Host: "github.test", Principal: "user:7"}
	reset := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	registry.UpdateSnapshot(identity, ghclient.QuotaResourceREST,
		ghclient.Rate{Limit: 15000, Remaining: 3000, Reset: reset})
	registry.UpdateSnapshot(identity, ghclient.QuotaResourceGraphQL,
		ghclient.Rate{Limit: 5000, Remaining: 4800, Reset: reset})
	syncer.SetQuotaRegistry(registry)
	srv := server.New(database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(ctx))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/archive/pacing", http.NoBody)
	req.Host = "forge.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var rows []struct {
		Known     bool `json:"known"`
		Limit     int  `json:"limit"`
		Remaining int  `json:"remaining"`
		Reserve   int  `json:"reserve"`
		Available int  `json:"available"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &rows))
	require.Len(rows, 1)
	row := rows[0]
	assert.True(row.Known)
	// The binding pool is REST (least headroom): every reported number comes
	// from that one pool so limit, remaining, and reserve are consistent.
	assert.Equal(15000, row.Limit)
	assert.Equal(3000, row.Remaining)
	assert.Equal(3000, row.Reserve)
	assert.Zero(row.Available)
}

func requireEnsureConfigured(t *testing.T, s *archive.Service, refs []platform.RepoRef) {
	t.Helper()
	_, err := s.EnsureConfigured(t.Context(), refs)
	require.NoError(t, err)
}
