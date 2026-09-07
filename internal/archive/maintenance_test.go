package archive

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestPromptMaintenanceCommitsPagesBeforeAdvancingScanWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 123, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	completeArchiveInitial(t, service)

	advanced := archiveTestIssue(ref)
	advanced.UpdatedAt = advanced.UpdatedAt.Add(time.Hour)
	advanced.LastActivityAt = advanced.UpdatedAt
	newIssue := advanced
	newIssue.PlatformID = 3
	newIssue.PlatformExternalID = "issue-3"
	newIssue.Number = 3
	newIssue.UpdatedAt = advanced.UpdatedAt.Add(time.Minute)
	newIssue.LastActivityAt = newIssue.UpdatedAt
	provider.updatedIssuePages = map[string]platform.Page[platform.Issue]{
		"":   {Items: []platform.Issue{advanced}, NextCursor: "u2"},
		"u2": {Items: []platform.Issue{newIssue}, Exhausted: true},
	}
	provider.updatedMRPages = map[string]platform.Page[platform.MergeRequest]{
		"": {Items: []platform.MergeRequest{archiveTestMergeRequest(ref)}, Exhausted: true},
	}

	require.NoError(service.RunEligible(t.Context()))
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	require.NotNil(states[0].MaintenanceWatermark)
	assert.Equal(now, *states[0].MaintenanceWatermark)
	require.NotNil(states[0].MaintenanceSucceededAt)
	assert.Equal(now, *states[0].MaintenanceSucceededAt)
	assert.Equal([]time.Time{now, now}, provider.updatedIssueSince)
	assert.Equal([]time.Time{now}, provider.updatedMRSince)

	advanced2, err := database.GetDatasetProgress(
		t.Context(), repoID, db.ArchiveItemTypeIssue, 1, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressPending, advanced2.Status)
	boundary, err := database.GetDatasetProgress(
		t.Context(), repoID, db.ArchiveItemTypeMergeRequest, 2, db.ArchiveDatasetLookup,
	)
	require.NoError(err)
	assert.Equal(db.ArchiveDatasetProgressPending, boundary.Status,
		"inclusive boundary items must refresh for coarse provider timestamps")
}

func TestPromptMaintenanceFailureRetainsPriorWatermarkAndCommittedPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	completeArchiveInitial(t, service)
	newIssue := archiveTestIssue(ref)
	newIssue.PlatformID, newIssue.PlatformExternalID, newIssue.Number = 3, "issue-3", 3
	provider.updatedIssuePages = map[string]platform.Page[platform.Issue]{
		"": {Items: []platform.Issue{newIssue}, NextCursor: "u2"},
	}
	provider.updatedIssueErrors = map[string]error{"u2": errors.New("updated issue page failed")}

	err := service.RunEligible(t.Context())
	require.Error(err)
	states, stateErr := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(stateErr)
	assert.Nil(states[0].MaintenanceWatermark)
	assert.Nil(states[0].MaintenanceSucceededAt)
	var count int
	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM forge_archive_items
		WHERE repo_id = ? AND item_type = 'issue' AND item_number = 3`, repoID).Scan(&count))
	assert.Equal(1, count, "the first page must remain durably committed")
}

func TestPromptMaintenanceResumesDurableCursorAfterBudgetDeferral(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	completeArchiveInitial(t, service)
	provider.updatedIssuePages = map[string]platform.Page[platform.Issue]{
		"":   {Items: []platform.Issue{archiveTestIssue(ref)}, NextCursor: "u2"},
		"u2": {Exhausted: true},
	}
	retryAt := now.Add(time.Hour)
	service.admission = &archiveTestAdmission{denyAfter: 1, retryAt: retryAt}

	require.NoError(service.RunEligible(t.Context()))
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.NotNil(states[0].MaintenanceIssues.NextCursor)
	assert.Equal("u2", *states[0].MaintenanceIssues.NextCursor)
	require.NotNil(states[0].PromptScanStartedAt)
	assert.Nil(states[0].MaintenanceSucceededAt)

	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service = newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, retryAt.Add(time.Minute))
	require.NoError(service.RunEligible(t.Context()))
	assert.Contains(provider.calls, "updated_issues:u2")
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Nil(states[0].PromptScanStartedAt)
	assert.Nil(states[0].MaintenanceIssues.NextCursor)
	require.NotNil(states[0].MaintenanceSucceededAt)
}

func TestPromptMaintenanceAdmissionReservesProviderConfirmationAttempts(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitLab, "gitlab.test", "repo")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	completeArchiveInitial(t, service)
	admission := &archiveTestAdmission{}
	service.admission = admission
	service.clock = fixedClock{value: now.Add(time.Hour)}
	service.SetMaintenanceInterval(time.Minute)

	require.NoError(service.RunEligible(t.Context()))

	require.NotEmpty(admission.costs)
	assert.Equal(t, 4, admission.costs[0])
}

func TestPromptMaintenanceCompletesDisabledRepositoryFeature(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	completeArchiveInitial(t, service)

	maintenanceAt := now.Add(time.Hour)
	provider.updatedIssueErrors = map[string]error{
		"": platform.RepositoryFeatureDisabled(
			ref.Platform, ref.Host, platform.RepositoryFeatureIssues,
			errors.New("issues disabled"),
		),
	}
	service.admission = &archiveTestAdmission{
		deferCompletedErrors: true,
		retryAt:              maintenanceAt.Add(time.Hour),
	}
	service.clock = fixedClock{value: maintenanceAt}
	service.SetMaintenanceInterval(time.Minute)

	require.NoError(service.RunEligible(t.Context()))

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Nil(states[0].PromptScanStartedAt)
	require.NotNil(states[0].MaintenanceWatermark)
	assert.Equal(maintenanceAt, *states[0].MaintenanceWatermark)
	require.NotNil(states[0].MaintenanceSucceededAt)
	assert.Equal(maintenanceAt, *states[0].MaintenanceSucceededAt)
	assert.Equal(db.ArchiveCoverageSupported, states[0].IssuesCoverage)
}

func TestPromptMaintenancePauseRejectsInFlightAvailabilityReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.issueInventoryErr = platform.RepositoryFeatureDisabled(
		ref.Platform, ref.Host, platform.RepositoryFeatureIssues,
		errors.New("issues disabled"),
	)
	service := archiveMaintenanceService(t, database, provider, ref, now)
	for range 8 {
		require.NoError(service.RunEligible(t.Context()))
	}

	before, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(before, 1)
	require.NotNil(before[0].InitialCompletedAt)
	assert.True(before[0].IssueInventory.Complete())
	assert.Equal(db.ArchiveCoverageUnsupported, before[0].IssuesCoverage)

	provider.issueInventoryErr = nil
	provider.updatedIssueStarted = make(chan struct{})
	provider.updatedIssueRelease = make(chan struct{})
	maintenanceAt := now.Add(time.Hour)
	service.clock = fixedClock{value: maintenanceAt}
	service.SetMaintenanceInterval(time.Minute)

	runDone := make(chan error, 1)
	go func() { runDone <- service.RunEligible(t.Context()) }()
	<-provider.updatedIssueStarted
	paused, err := service.Pause(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	require.Len(paused, 1)
	assert.Equal(db.ArchiveStatusPaused, paused[0].Progress.Status)
	close(provider.updatedIssueRelease)
	require.NoError(<-runDone)

	after, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(after, 1)
	assert.Equal(db.ArchiveCoverageUnsupported, after[0].IssuesCoverage)
	assert.True(after[0].IssueInventory.Complete())
	assert.Equal(before[0].IssueInventory.Generation, after[0].IssueInventory.Generation)
	assert.Equal(before[0].InitialCompletedAt, after[0].InitialCompletedAt)
	assert.False(after[0].MaintenanceIssues.Complete())
}

func archiveMaintenanceService(
	t *testing.T,
	database *db.DB,
	provider *archiveServiceProvider,
	ref platform.RepoRef,
	now time.Time,
) *Service {
	t.Helper()
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(t, err)
	return service
}

func completeArchiveInitial(t *testing.T, service *Service) {
	t.Helper()
	for range 4 {
		require.NoError(t, service.RunEligible(t.Context()))
	}
}
