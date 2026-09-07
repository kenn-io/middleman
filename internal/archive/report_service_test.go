package archive

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestArchiveServiceReportBuildsOfflineCountsCoverageAndDetails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	completeArchiveInitial(t, service)
	_, err = database.UpsertIssue(t.Context(), &db.Issue{
		RepoID: repoID, Number: 1, Title: "Synthetic issue", Author: "sam", Body: "issue body",
		State: "closed", CreatedAt: archiveTestTime(), UpdatedAt: archiveTestTime(),
	})
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, Number: 2, Title: "Synthetic MR", Author: "sam", Body: "mr body",
		State: db.MergeRequestStateClosed, CreatedAt: archiveTestTime(), UpdatedAt: archiveTestTime(),
	})
	require.NoError(err)
	providerCalls := len(provider.calls)

	model, err := service.Report(t.Context(), ReportOptions{
		Start: archiveTestTime().Add(-time.Hour), End: archiveTestTime().Add(time.Hour),
		Detailed: true,
	})
	require.NoError(err)
	assert.Len(provider.calls, providerCalls, "reports must not issue provider requests")
	require.Len(model.Repositories, 1)
	assert.Equal("github", model.Repositories[0].Repository.Provider)
	assert.Equal("github.test", model.Repositories[0].Repository.PlatformHost)
	assert.Equal("owner/repo", model.Repositories[0].Repository.RepoPath)
	assert.Equal("current", model.Repositories[0].Coverage.Status)
	assert.Equal(1, model.Totals.IssuesOpened)
	assert.Equal(1, model.Totals.MergeRequestsOpened)
	assert.Zero(model.Totals.OrdinaryComments)
	assert.Zero(model.Totals.ReviewsSubmitted)
	assert.Zero(model.Totals.InlineReviewComments)
	require.Len(model.Activity, 2)
	assert.Equal(report.ActivityIssue, model.Activity[0].Kind)
	assert.Equal(report.ActivityMergeRequest, model.Activity[1].Kind)
	assert.Equal("sam", model.Activity[0].Author)
	assertContributorCounts(t, model.Contributors, "sam", report.Counts{
		IssuesOpened: 1, MergeRequestsOpened: 1,
	})
}

func TestArchiveServiceReportFiltersFullRepositoryIdentityAndRejectsEmptyScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	first := archiveServiceRef(platform.KindGitHub, "github.test", "one")
	second := archiveServiceRef(platform.KindGitHub, "enterprise.test", "two")
	archiveServiceSeedRepo(t, database, first)
	archiveServiceSeedRepo(t, database, second)
	firstProvider := newArchiveServiceProvider(first.Platform, first.Host)
	secondProvider := newArchiveServiceProvider(second.Platform, second.Host)
	registry, err := platform.NewRegistry(firstProvider, secondProvider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{first, second}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{first, second})

	all, err := service.Report(t.Context(), ReportOptions{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour),
	})
	require.NoError(err)
	require.Len(all.Repositories, 2)
	assert.Equal("enterprise.test", all.Repositories[0].Repository.PlatformHost)
	assert.Equal("github.test", all.Repositories[1].Repository.PlatformHost)

	filtered, err := service.Report(t.Context(), ReportOptions{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour), Repositories: []platform.RepoRef{second},
	})
	require.NoError(err)
	require.Len(filtered.Repositories, 1)
	assert.Equal("enterprise.test", filtered.Repositories[0].Repository.PlatformHost)

	emptyDB := dbtest.Open(t)
	emptyService := newArchiveTestService(t, emptyDB, registry, nil, nil, now)
	_, err = emptyService.Report(t.Context(), ReportOptions{Start: now.Add(-time.Hour), End: now})
	assert.ErrorIs(err, ErrEmptyReportScope)
}

func TestArchiveServiceReportUsesRangeEndAsDeterministicStatusTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	before := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, end.Add(-time.Minute))
	requireEnsureConfigured(t, before, []platform.RepoRef{ref})
	retryAt := end.Add(30 * time.Minute)
	require.NoError(database.RecordArchiveRepositoryFailure(
		t.Context(), repoID, db.ArchiveErrorCodeBudgetExhausted, "waiting", &retryAt, end,
	))
	after := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, retryAt.Add(time.Minute))

	first, err := before.Report(t.Context(), ReportOptions{Start: start, End: end})
	require.NoError(err)
	second, err := after.Report(t.Context(), ReportOptions{Start: start, End: end})
	require.NoError(err)
	assert.Equal(first, second)
	assert.Equal(string(db.ArchiveStatusWaitingForBudget), first.Repositories[0].Coverage.Status)
}

func TestArchiveServiceReportRejectsPartiallyMissingExplicitScope(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	first := archiveServiceRef(platform.KindGitHub, "github.test", "one")
	second := archiveServiceRef(platform.KindGitHub, "github.test", "two")
	archiveServiceSeedRepo(t, database, first)
	secondID := archiveServiceSeedRepo(t, database, second)
	provider := newArchiveServiceProvider(first.Platform, first.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{first, second}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{first})

	_, err = service.Report(t.Context(), ReportOptions{
		Start: now.Add(-time.Hour), End: now, Repositories: []platform.RepoRef{first, second},
	})
	var missing *db.ArchiveRepoStateNotFoundError
	require.ErrorAs(err, &missing)
	require.Equal([]int64{secondID}, missing.RepoIDs)
}

func TestArchiveServiceReportUsesOneSQLiteSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})

	coverageRead := make(chan struct{})
	writerDone := make(chan struct{})
	afterCoverage := func() error {
		close(coverageRead)
		<-writerDone
		return nil
	}
	result := make(chan report.Model, 1)
	errResult := make(chan error, 1)
	go func() {
		model, err := service.report(
			t.Context(),
			ReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
			afterCoverage,
		)
		if err != nil {
			errResult <- err
			return
		}
		result <- model
	}()
	<-coverageRead
	_, err = database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_issues (
			repo_id, platform_id, platform_external_id, number, title, state,
			created_at, updated_at, last_activity_at
		) VALUES (?, 99, 'issue-99', 99, 'concurrent', 'open', ?, ?, ?)`, repoID, now, now, now)
	require.NoError(err)
	close(writerDone)

	select {
	case err := <-errResult:
		require.NoError(err)
	case model := <-result:
		assert.Zero(model.Totals.IssuesOpened,
			"coverage and counts must observe the snapshot established before the writer")
	case <-time.After(5 * time.Second):
		require.Fail("report timed out")
	}
}

func TestArchiveServiceDetailedReportLimitDoesNotLimitSummary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = database.WriteDB().ExecContext(t.Context(), `
		WITH RECURSIVE sequence(n) AS (
			VALUES(1) UNION ALL SELECT n + 1 FROM sequence WHERE n < 10001
		)
		INSERT INTO forge_issues (
			repo_id, platform_id, platform_external_id, number, title, state,
			created_at, updated_at, last_activity_at
		)
		SELECT ?, n, 'bulk-' || n, n, '', 'closed', ?, ?, ? FROM sequence`, repoID, now, now, now)
	require.NoError(err)

	summary, err := service.Report(t.Context(), ReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	require.NoError(err)
	assert.Equal(10_001, summary.Totals.IssuesOpened)
	assert.Empty(summary.Activity)

	_, err = service.Report(t.Context(), ReportOptions{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour), Detailed: true,
	})
	var limit *report.LimitError
	require.ErrorAs(err, &limit)
	assert.Equal(10_001, limit.ObservedRecords)
	assert.Equal(report.MaxDetailedRecords, limit.MaxRecords)
}

func TestBuildArchiveReportKeepsContributorIdentityProviderScoped(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	repositories := []db.ArchiveReportRepositoryRow{
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Owner: "acme", Name: "one", RepoPath: "acme/one"},
		{RepoID: 2, Platform: "gitlab", PlatformHost: "gitlab.example", Owner: "acme", Name: "two", RepoPath: "acme/two"},
	}
	counts := []db.ArchiveReportCountRow{
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityIssue, Author: "sam", Count: 1},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityIssueClosed, Author: "sam", Count: 6},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityMergeRequest, Author: "sam", Count: 2},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityMergeRequestMerged, Author: "sam", Count: 9},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityOrdinaryComment, Author: "sam", Count: 3},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityReview, Author: "sam", Count: 4},
		{RepoID: 1, Platform: "github", PlatformHost: "github.example", Kind: db.ArchiveReportActivityInlineReviewComment, Author: "sam", Count: 5},
		{RepoID: 2, Platform: "gitlab", PlatformHost: "gitlab.example", Kind: db.ArchiveReportActivityIssue, Author: "sam", Count: 7},
	}

	model := buildArchiveReport(ReportOptions{Start: start, End: start.Add(time.Hour)}, repositories, counts, nil)
	assert.Equal(report.Counts{
		IssuesOpened: 8, IssuesClosed: 6, MergeRequestsOpened: 2, MergeRequestsMerged: 9, OrdinaryComments: 3,
		ReviewsSubmitted: 4, InlineReviewComments: 5,
	}, model.Totals)
	assert.Equal(report.Counts{
		IssuesOpened: 1, IssuesClosed: 6, MergeRequestsOpened: 2, MergeRequestsMerged: 9, OrdinaryComments: 3,
		ReviewsSubmitted: 4, InlineReviewComments: 5,
	}, model.Repositories[0].Counts)
	assert.Equal(report.Counts{IssuesOpened: 7}, model.Repositories[1].Counts)
	if assert.Len(model.Contributors, 2) {
		assert.Equal("github", model.Contributors[0].Provider)
		assert.Equal("sam", model.Contributors[0].Login)
		assert.Equal(30, model.Contributors[0].Counts.TotalActivity())
		assert.Equal("gitlab", model.Contributors[1].Provider)
		assert.Equal("sam", model.Contributors[1].Login)
		assert.Equal(7, model.Contributors[1].Counts.TotalActivity())
	}
}

func assertContributorCounts(
	t *testing.T,
	contributors []report.Contributor,
	login string,
	want report.Counts,
) {
	t.Helper()
	for _, contributor := range contributors {
		if contributor.Login == login {
			assert.Equal(t, want, contributor.Counts)
			return
		}
	}
	require.Failf(t, "contributor not found", "login=%q", login)
}
