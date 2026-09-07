package archive

import (
	"context"
	"encoding/json"
	"errors"
	"go.kenn.io/forge/internal/platformdb"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestArchiveRetryClassifierTreatsAttemptBudgetRefusalAsTransient(t *testing.T) {
	assert := assert.New(t)
	now := archiveTestTime()
	// A budget-transport refusal may reach the classifier bare or wrapped by a
	// provider error mapper. GitLab maps unclassified transport errors to its
	// default invalid_repo_ref code, which would otherwise classify as a
	// repository-blocking contract error. The refusal must stay a transient
	// budget deferral in every form.
	wrapped := &platform.Error{
		Code: platform.ErrCodeInvalidRepoRef, Err: platform.ErrArchiveAttemptBudget,
	}
	for name, cause := range map[string]error{
		"bare":              platform.ErrArchiveAttemptBudget,
		"provider-contract": wrapped,
		"deeply-wrapped":    errors.Join(errors.New("list historical issues"), wrapped),
	} {
		t.Run(name, func(t *testing.T) {
			decision := defaultArchiveRetryDecision(cause, 0, now)
			assert.Equal(db.ArchiveErrorCodeTransient, decision.Code)
			assert.NotNil(decision.RetryAt)
		})
	}
}

func TestArchiveTerminalSyncOutcomeRetriesGenericPermissionDenied(t *testing.T) {
	assert := assert.New(t)
	outcome, destination, terminal := archiveTerminalSyncOutcome(
		platform.PermissionDenied(platform.KindGitLab, "gitlab.example.com", errors.New("expired token")),
	)

	assert.False(terminal)
	assert.Empty(outcome)
	assert.Nil(destination)
}

func TestArchiveTerminalSyncOutcomeRetriesGenericNotFound(t *testing.T) {
	assert := assert.New(t)
	outcome, destination, terminal := archiveTerminalSyncOutcome(platform.ErrNotFound)

	assert.False(terminal)
	assert.Empty(outcome)
	assert.Nil(destination)
}

func TestArchiveTerminalSyncOutcomeAcceptsExplicitInaccessibleLookup(t *testing.T) {
	assert := assert.New(t)
	outcome, destination, terminal := archiveTerminalSyncOutcome(
		platform.PermissionDenied(
			platform.KindGitHub,
			"github.com",
			errors.Join(platform.ErrLookupInaccessible, errors.New("lookup denied")),
		),
	)

	assert.True(terminal)
	assert.Equal(db.ArchiveLookupInaccessible, outcome)
	assert.Nil(destination)
}

func TestArchiveServiceStartValidatesAllRepositoriesBeforePromotion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	valid := archiveServiceRef(platform.KindGitHub, "github.test", "one")
	missing := archiveServiceRef(platform.KindGitLab, "gitlab.test", "two")
	validID := archiveServiceSeedRepo(t, database, valid)
	archiveServiceSeedRepo(t, database, missing)
	provider := newArchiveServiceProvider(valid.Platform, valid.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{valid, missing}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{valid})

	_, err = service.Start(t.Context(), []platform.RepoRef{valid, missing})
	require.Error(err)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{validID})
	require.NoError(err)
	assert.Equal(db.ArchiveCollectionModeDiscovery, states[0].CollectionMode)
}

func TestArchiveServiceEnsureConfiguredSkipsUnresolvableRef(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	good := archiveServiceRef(platform.KindGitHub, "github.test", "good")
	// No provider id, no stored route, and the provider fake has no
	// repository reader: the seed path cannot identify this ref.
	ghost := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "owner", Name: "ghost", RepoPath: "owner/ghost",
	}
	provider := newArchiveServiceProvider(good.Platform, good.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{good, ghost}, nil, now)

	seeded, err := service.EnsureConfigured(t.Context(), []platform.RepoRef{good, ghost})
	require.NoError(err, "an unresolvable ref must degrade, not fail the pass")
	assert.Equal([]platform.RepoRef{good}, seeded)

	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(good))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState)

	ghostRepo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ghost))
	require.NoError(err)
	assert.Nil(ghostRepo)
}

func TestArchiveServiceEnsureConfiguredDefersRemovalPausingWhenRefUnresolvable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	removed := archiveServiceRef(platform.KindGitHub, "github.test", "removed")
	good := archiveServiceRef(platform.KindGitHub, "github.test", "good")
	ghost := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "owner", Name: "ghost", RepoPath: "owner/ghost",
	}
	provider := newArchiveServiceProvider(good.Platform, good.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{good}, nil, now)

	requireEnsureConfigured(t, service, []platform.RepoRef{removed})
	removedRepo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(removed))
	require.NoError(err)
	require.NotNil(removedRepo)

	// An unresolvable ref could correspond to any existing row, so removal
	// pausing must be deferred rather than pause the wrong archive.
	seeded, err := service.EnsureConfigured(t.Context(), []platform.RepoRef{good, ghost})
	require.NoError(err)
	assert.Equal([]platform.RepoRef{good}, seeded)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{removedRepo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState,
		"removal pausing must be deferred while a ref is unresolvable")

	// A clean pass applies the deferred removal.
	requireEnsureConfigured(t, service, []platform.RepoRef{good})
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{removedRepo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveOperatorStatePaused, states[0].OperatorState)
	require.NotNil(states[0].LastErrorCode)
	assert.Equal(string(db.ArchiveErrorCodeConfigurationRemoved), *states[0].LastErrorCode)
}

func TestArchiveServiceEnsureConfiguredSeedsFreshRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "fresh")
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)

	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	repo, err := database.GetRepoByIdentity(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(err)
	require.NotNil(repo)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repo.ID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(db.ArchiveCollectionModeDiscovery, states[0].CollectionMode)
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState)
}

func TestArchiveServiceEnsureConfiguredPreservesRenamedRepositoryAtExistingDestination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	previous := archiveServiceRef(platform.KindGitHub, "github.test", "previous")
	previous.PlatformExternalID = "repo-current"
	current := archiveServiceRef(platform.KindGitHub, "github.test", "current")
	current.PlatformExternalID = "repo-current"
	staleDestination := current
	staleDestination.PlatformExternalID = "repo-obsolete"

	sourceID, err := database.UpsertRepoByProviderID(t.Context(), platformdb.DBRepoIdentity(previous))
	require.NoError(err)
	destinationID, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(staleDestination))
	require.NoError(err)
	provider := newArchiveServiceProvider(current.Platform, current.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{current}, nil, now)

	requireEnsureConfigured(t, service, []platform.RepoRef{current})
	repos, err := database.ListRepositoryCatalog(
		t.Context(), db.RepositoryCatalogFilter{},
	)
	require.NoError(err)
	require.Len(repos, 2)
	assert.NotEqual(sourceID, destinationID)

	active, err := database.ResolveActiveRepositoryRoute(
		t.Context(), platformdb.DBRepoIdentity(current),
	)
	require.NoError(err)
	require.NotNil(active)
	assert.Equal(sourceID, active.Repository.ID)
	assert.Equal("repo-current", active.Repository.PlatformRepoID)
	assert.Equal("current", active.Repository.Name)

	stale, err := database.GetRepositoryByProviderID(
		t.Context(), "github", "github.test", "repo-obsolete",
	)
	require.NoError(err)
	require.NotNil(stale)
	assert.Equal(destinationID, stale.Repository.ID)
	assert.Equal(db.RepositoryLifecycleInactive, stale.Lifecycle)

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{sourceID, destinationID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(sourceID, states[0].RepoID)
}

func TestArchiveServiceAllScopeAndWakeLifecycle(t *testing.T) {
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
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{second, first}, nil, now,
	)
	wakeCount := 0
	service.SetWake(func() { wakeCount++ })
	requireEnsureConfigured(t, service, []platform.RepoRef{first, second})

	started, err := service.StartAll(t.Context())
	require.NoError(err)
	require.Len(started, 2)
	assert.Equal("enterprise.test", started[0].Repo.Host)
	assert.Equal("github.test", started[1].Repo.Host)
	assert.Equal(1, wakeCount)

	statuses, err := service.Status(t.Context(), nil)
	require.NoError(err)
	require.Len(statuses, 2)
	assert.Equal("enterprise.test", statuses[0].Repo.Host)
	assert.Equal("github.test", statuses[1].Repo.Host)

	paused, err := service.PauseAll(t.Context())
	require.NoError(err)
	require.Len(paused, 2)
	assert.Equal(db.ArchiveStatusPaused, paused[0].Progress.Status)
	assert.Equal(db.ArchiveStatusPaused, paused[1].Progress.Status)
}

func TestArchiveServicePauseRejectsInFlightInventoryCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.issueInventoryStarted = make(chan struct{})
	provider.issueInventoryRelease = make(chan struct{})
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	runDone := make(chan error, 1)
	go func() { runDone <- service.RunEligible(t.Context()) }()
	<-provider.issueInventoryStarted
	paused, err := service.Pause(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	require.Len(paused, 1)
	assert.Equal(db.ArchiveStatusPaused, paused[0].Progress.Status)
	close(provider.issueInventoryRelease)
	require.NoError(<-runDone)

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.False(states[0].IssueInventory.Complete())
	assert.Nil(states[0].IssueInventory.NextCursor)
	var itemCount int
	require.NoError(database.ReadDB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM forge_archive_items WHERE repo_id = ?`, repoID,
	).Scan(&itemCount))
	assert.Zero(itemCount)
}

func TestArchiveServiceRetryAuthenticationPreservesProgress(t *testing.T) {
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
	cursor := "issue-page-2"
	watermark := now.Add(-time.Hour)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repo_scans
		SET next_cursor = ?, status = 'running', page_count = 1
		WHERE repo_id = ? AND scan = 'issue_inventory'`, cursor, repoID)
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET maintenance_watermark = ?, last_error_code = 'authentication_failed',
			last_error_detail = 'expired token', next_retry_at = ?
		WHERE repo_id = ?`, watermark, now.Add(time.Hour), repoID)
	require.NoError(err)

	require.NoError(service.RetryAuthentication(t.Context(), []platform.RepoRef{ref}))
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(&cursor, states[0].IssueInventory.NextCursor)
	assert.Equal(&watermark, states[0].MaintenanceWatermark)
	assert.Nil(states[0].LastErrorCode)
	assert.Nil(states[0].LastErrorDetail)
	assert.Nil(states[0].NextRetryAt)
}

func TestArchiveAuthenticationFailureDefersPendingHydration(t *testing.T) {
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
	require.NoError(service.RunEligible(t.Context()))

	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET last_error_code = 'authentication_failed', last_error_detail = 'expired token',
			next_retry_at = NULL
		WHERE repo_id = ?`, repoID)
	require.NoError(err)
	provider.mu.Lock()
	provider.calls = nil
	provider.mu.Unlock()

	require.NoError(service.RunEligible(t.Context()))
	provider.mu.Lock()
	calls := append([]string(nil), provider.calls...)
	provider.mu.Unlock()
	assert.Empty(t, calls)
}

func TestArchiveIdlePollDoesNotReconcileConfiguredRepositories(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "idle")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Pause(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		CREATE TRIGGER reject_idle_repo_reconcile BEFORE UPDATE ON forge_repos
		BEGIN SELECT RAISE(ABORT, 'idle poll wrote repository'); END`)
	require.NoError(err)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		CREATE TRIGGER reject_idle_archive_reconcile BEFORE UPDATE ON forge_archive_repos
		BEGIN SELECT RAISE(ABORT, 'idle poll wrote archive state'); END`)
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
}

func TestArchiveServiceRemovedRepositoryStopsWorkAndReaddResumesState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	source := &archiveMutableSource{refs: []platform.RepoRef{ref}}
	service, err := NewService(database, registry, nil, source, nil, fixedClock{value: now})
	require.NoError(err)
	requireEnsureConfigured(t, service, source.refs)
	_, err = service.Start(t.Context(), source.refs)
	require.NoError(err)
	cursor := "durable-cursor"
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repo_scans
		SET next_cursor = ?, status = 'running', page_count = 1
		WHERE repo_id = ? AND scan = 'issue_inventory'`, cursor, repoID)
	require.NoError(err)

	source.refs = nil
	requireEnsureConfigured(t, service, source.refs)
	require.NoError(service.RunEligible(t.Context()))
	assert.Empty(provider.calls)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Equal(db.ArchiveCollectionModeFull, states[0].CollectionMode)
	assert.Equal(db.ArchiveOperatorStatePaused, states[0].OperatorState)
	require.NotNil(states[0].LastErrorCode)
	assert.Equal(string(db.ArchiveErrorCodeConfigurationRemoved), *states[0].LastErrorCode)
	assert.Equal(&cursor, states[0].IssueInventory.NextCursor)

	source.refs = []platform.RepoRef{ref}
	requireEnsureConfigured(t, service, source.refs)
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Equal(db.ArchiveCollectionModeFull, states[0].CollectionMode)
	assert.Equal(db.ArchiveOperatorStateActive, states[0].OperatorState)
	assert.Nil(states[0].LastErrorCode)
	assert.Equal(&cursor, states[0].IssueInventory.NextCursor)

	require.NoError(database.PauseArchives(t.Context(), []int64{repoID}, now.Add(time.Minute)))
	source.refs = nil
	requireEnsureConfigured(t, service, source.refs)
	source.refs = []platform.RepoRef{ref}
	requireEnsureConfigured(t, service, source.refs)
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Equal(db.ArchiveOperatorStatePaused, states[0].OperatorState)
	assert.Nil(states[0].LastErrorCode)
}

func TestArchiveInventoryInvalidCursorBlocksScanWithoutRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "cycling")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.historicalIssuePages = map[string]platform.Page[platform.Issue]{
		"":   {ProgressOnly: true, NextCursor: "p2"},
		"p2": {ProgressOnly: true, NextCursor: "p2"},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	for range 4 {
		require.NoError(service.RunEligible(t.Context()))
	}
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.True(states[0].IssueInventory.Blocked())
	require.NotNil(states[0].IssueInventory.LastErrorCode)
	assert.Equal("invalid_cursor", *states[0].IssueInventory.LastErrorCode)
	assert.True(states[0].MergeRequestInventory.Complete(), "the unaffected inventory stream still completes")
	assert.Equal([]string{"", "p2"}, provider.issueInventoryCursors)

	// A blocked scan spends no further provider requests automatically.
	provider.issueInventoryCursors = nil
	require.NoError(service.RunEligible(t.Context()))
	assert.Empty(provider.issueInventoryCursors)
	status, err := service.Status(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	assert.Equal(db.ArchiveStatusBlocked, status[0].Progress.Status)
}

func TestArchiveResumesDiscoveryAppliesMaintenanceAndReportsDeterministically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.historicalIssuePages = map[string]platform.Page[platform.Issue]{
		"":             {Items: []platform.Issue{archiveTestIssue(ref)}, NextCursor: "issue-page-2"},
		"issue-page-2": {Exhausted: true},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	require.NoError(service.RunEligible(t.Context()))
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.NotNil(states[0].IssueInventory.NextCursor)
	assert.Equal("issue-page-2", *states[0].IssueInventory.NextCursor)

	service = newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	for range 10 {
		status, statusErr := service.Status(t.Context(), []platform.RepoRef{ref})
		require.NoError(statusErr)
		if status[0].Progress.Status == db.ArchiveStatusCurrent {
			break
		}
		require.NoError(service.RunEligible(t.Context()))
	}
	status, err := service.Status(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	assert.Equal(db.ArchiveStatusCurrent, status[0].Progress.Status)
	assert.Equal([]string{"", "issue-page-2"}, provider.issueInventoryCursors)

	edited := archiveTestIssue(ref)
	edited.UpdatedAt = edited.UpdatedAt.Add(time.Hour)
	edited.LastActivityAt = edited.UpdatedAt
	provider.updatedIssuePages = map[string]platform.Page[platform.Issue]{
		"": {Items: []platform.Issue{edited}, Exhausted: true},
	}
	service.clock = fixedClock{value: now.Add(5 * time.Minute)}
	initialStartedAt := now.Add(-2 * time.Hour)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET initial_started_at = ?, initial_completed_at = ?,
			maintenance_watermark = NULL, maintenance_succeeded_at = NULL
		WHERE repo_id = ?`, initialStartedAt, now.Add(-time.Hour), repoID)
	require.NoError(err)
	require.NoError(service.RunEligible(t.Context())) // prompt maintenance queues the edit
	require.NotEmpty(provider.updatedIssueSince)
	assert.Equal(initialStartedAt, provider.updatedIssueSince[0])
	require.NoError(service.RunEligible(t.Context())) // sync the edited issue
	beforeCalls := len(provider.calls)
	first, err := service.Report(t.Context(), ReportOptions{
		Start: archiveTestTime().Add(-time.Hour), End: archiveTestTime().Add(2 * time.Hour), Detailed: true,
	})
	require.NoError(err)
	second, err := service.Report(t.Context(), ReportOptions{
		Start: archiveTestTime().Add(-time.Hour), End: archiveTestTime().Add(2 * time.Hour), Detailed: true,
	})
	require.NoError(err)
	assert.Equal(first, second)
	assert.Len(provider.calls, beforeCalls, "reporting must remain provider-offline")
	firstMarkdown, err := report.RenderMarkdown(first)
	require.NoError(err)
	secondMarkdown, err := report.RenderMarkdown(second)
	require.NoError(err)
	assert.Equal(firstMarkdown, secondMarkdown)
	firstJSON, err := json.MarshalIndent(first, "", "  ")
	require.NoError(err)
	secondJSON, err := json.MarshalIndent(second, "", "  ")
	require.NoError(err)
	assert.JSONEq(string(firstJSON), string(secondJSON))
}

func TestArchiveInventoryFailureIsDurableAndClearedBySuccessfulProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.issueInventoryErr = errors.New("inventory unavailable")
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	service.retries = archiveTestRetryClassifier{}
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	err = service.RunEligible(t.Context())
	require.Error(err)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.NotNil(states[0].LastErrorCode)
	assert.Equal(string(db.ArchiveErrorCodeTransient), *states[0].LastErrorCode)
	require.NotNil(states[0].LastErrorDetail)
	assert.Contains(*states[0].LastErrorDetail, "inventory unavailable")

	provider.issueInventoryErr = nil
	require.NoError(service.RunEligible(t.Context()))
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Nil(states[0].LastErrorCode)
	assert.Nil(states[0].NextRetryAt)
}

func TestArchiveStartAllowsPartialHistoricalInventory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.caps.Archive.HistoricalMergeRequests = false
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})

	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.Equal(db.ArchiveCollectionModeFull, states[0].CollectionMode)
}

func TestArchiveDiscoverySkipsUnsupportedInventoryStream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.caps.Archive.HistoricalMergeRequests = false
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	assert.True(states[0].IssueInventory.Complete())
	assert.True(states[0].MergeRequestInventory.Complete())
	assert.Equal(db.ArchiveCoverageSupported, states[0].IssuesCoverage)
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].MergeRequestsCoverage)
	assert.Equal([]string{"issues"}, provider.calls)
}

func TestArchiveMaintenanceDoesNotReopenStaticallyUnsupportedInventoryStream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitLab, "gitlab.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.caps.Archive.HistoricalMergeRequests = false
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))
	initialStartedAt := now.Add(-2 * time.Hour)
	_, err = database.WriteDB().ExecContext(t.Context(), `
		UPDATE forge_archive_repos
		SET initial_started_at = ?, initial_completed_at = ?,
			maintenance_watermark = NULL, maintenance_succeeded_at = NULL
		WHERE repo_id = ?`, initialStartedAt, now, repoID)
	require.NoError(err)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	require.NotNil(states[0].InitialCompletedAt)
	assert.True(states[0].MergeRequestInventory.Complete())
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].MergeRequestsCoverage)
	unsupportedGeneration := states[0].MergeRequestInventory.Generation

	service.clock = fixedClock{value: now.Add(5 * time.Minute)}
	resolved, err := service.resolveRepositories(t.Context(), []platform.RepoRef{ref}, true)
	require.NoError(err)
	require.Len(resolved, 1)
	require.NoError(service.promptMaintenance(t.Context(), resolved[0], states[0]))

	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.Contains(provider.calls, "updated_mrs:", "maintenance must still read updated merge requests")
	assert.Equal(unsupportedGeneration, states[0].MergeRequestInventory.Generation)
	assert.True(states[0].MergeRequestInventory.Complete())
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].MergeRequestsCoverage)
	assert.NotNil(states[0].InitialCompletedAt)
}

func TestArchiveInventoryReopensRepositoryFeatureAfterReenable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	provider.issueInventoryErr = platform.RepositoryFeatureDisabled(
		ref.Platform, ref.Host, platform.RepositoryFeatureIssues, errors.New("issues disabled"),
	)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	admission := &archiveTestAdmission{
		deferCompletedErrors: true,
		retryAt:              now.Add(time.Hour),
	}
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{ref}, admission, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})

	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.True(states[0].IssueInventory.Complete())
	assert.True(states[0].MergeRequestInventory.Complete())
	assert.Equal(db.ArchiveCoverageUnsupported, states[0].IssuesCoverage)
	assert.Equal(db.ArchiveCoverageSupported, states[0].MergeRequestsCoverage)
	assert.Nil(states[0].LastErrorCode)
	unsupportedGeneration := states[0].IssueInventory.Generation

	provider.issueInventoryErr = nil
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.False(states[0].IssueInventory.Complete())
	assert.Greater(states[0].IssueInventory.Generation, unsupportedGeneration)
	assert.Equal(db.ArchiveCoverageUnknown, states[0].IssuesCoverage)

	require.NoError(service.RunEligible(t.Context()))
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	assert.True(states[0].IssueInventory.Complete())
	assert.Equal(db.ArchiveCoverageSupported, states[0].IssuesCoverage)
}

func TestDefaultArchiveRetryClassifierDistinguishesTerminalProviderErrors(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		err       error
		wantCode  db.ArchiveErrorCode
		wantRetry bool
	}{
		{name: "authentication", err: platform.ErrPermissionDenied, wantCode: db.ArchiveErrorCodeAuthentication},
		{name: "contract", err: platform.ErrProviderContract, wantCode: db.ArchiveErrorCodeRepoBlocked},
		{name: "page limit", err: platform.ErrPageLimit, wantCode: db.ArchiveErrorCodeRepoBlocked},
		{name: "transient", err: errors.New("temporary"), wantCode: db.ArchiveErrorCodeTransient, wantRetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := (defaultRetryClassifier{}).Classify(tt.err, 0, now)
			assert.Equal(t, tt.wantCode, decision.Code)
			assert.Equal(t, tt.wantRetry, decision.RetryAt != nil)
		})
	}
}

func TestArchiveBudgetDeferralDoesNotIncrementAttempts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	retryAt := now.Add(time.Hour)
	admission := &archiveTestAdmission{deny: true, retryAt: retryAt}
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, admission, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
	status, err := service.Status(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	assert.Equal(db.ArchiveStatusWaitingForBudget, status[0].Progress.Status)
	require.NotNil(status[0].Progress.BudgetWaitUntil)
	assert.Equal(retryAt, *status[0].Progress.BudgetWaitUntil)
	assert.Empty(provider.calls)
}

func TestArchiveInventoryAdmissionReservesGitHubMergeRequestConfirmationAttempts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	admission := &archiveTestAdmission{}
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, admission, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
	require.NoError(service.RunEligible(t.Context()))

	require.Len(admission.costs, 2)
	assert.Equal([]int{2, 4}, admission.costs)
}

func TestArchivePausePreventsFutureProviderReads(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, nil, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	_, err = service.Pause(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)
	require.NoError(service.RunEligible(t.Context()))
	assert.Empty(t, provider.calls)
}

func TestArchiveHydrationRetryClassifierReceivesStoredAttemptCount(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	classifier := &recordingRetryClassifier{}
	service, err := NewService(
		database, registry, nil,
		archiveFailingItemSource{archiveTestSource{refs: []platform.RepoRef{ref}}},
		classifier, fixedClock{value: now},
	)
	require.NoError(err)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	// Early polls run inventory bootstrap pages and succeed; hydration
	// failures begin once an item is claimable, so poll until three
	// failures have been classified.
	for range 8 {
		_ = service.RunEligible(t.Context())
		if len(classifier.recorded()) >= 3 {
			break
		}
	}

	attempts := classifier.recorded()
	require.GreaterOrEqual(len(attempts), 3)
	assert.Equal(t, []int{0, 1, 2}, attempts[:3],
		"each retry must classify with the item's stored attempt count so backoff can grow")
}

type recordingRetryClassifier struct {
	mu       sync.Mutex
	attempts []int
}

func (c *recordingRetryClassifier) Classify(_ error, attempt int, _ time.Time) RetryDecision {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts = append(c.attempts, attempt)
	return RetryDecision{Code: db.ArchiveErrorCodeTransient}
}

func (c *recordingRetryClassifier) recorded() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.attempts...)
}

type archiveFailingItemSource struct{ archiveTestSource }

func (archiveFailingItemSource) SyncArchiveItem(
	context.Context,
	platform.RepoRef,
	db.ArchiveItemType,
	int,
) (ItemSyncResult, error) {
	return ItemSyncResult{ProviderAttempted: true}, errors.New("transient provider failure")
}

type archiveTestSource struct{ refs []platform.RepoRef }

func (s archiveTestSource) ConfiguredRepositories(context.Context) ([]platform.RepoRef, error) {
	return s.refs, nil
}

func (archiveTestSource) ArchiveItemSyncCost(platform.Kind, db.ArchiveItemType) int { return 1 }

func (archiveTestSource) SyncArchiveItem(
	context.Context,
	platform.RepoRef,
	db.ArchiveItemType,
	int,
) (ItemSyncResult, error) {
	return ItemSyncResult{ProviderAttempted: true}, nil
}

type archiveMutableSource struct{ refs []platform.RepoRef }

func (s *archiveMutableSource) ConfiguredRepositories(context.Context) ([]platform.RepoRef, error) {
	return append([]platform.RepoRef(nil), s.refs...), nil
}

type archiveTestAdmission struct {
	mu                   sync.Mutex
	calls                int
	deny                 bool
	denyAfter            int
	retryAt              time.Time
	costs                []int
	deferCompletedErrors bool
}

type archiveTestRetryClassifier struct{}

func (archiveTestRetryClassifier) Classify(error, int, time.Time) RetryDecision {
	return RetryDecision{Code: db.ArchiveErrorCodeTransient}
}

func (a *archiveTestAdmission) Admit(
	ctx context.Context,
	_ platform.RepoRef,
	_ db.ArchiveItemType,
	cost int,
) (AdmissionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.costs = append(a.costs, cost)
	if a.deny || a.denyAfter > 0 && a.calls > a.denyAfter {
		return AdmissionResult{RetryAt: &a.retryAt, Detail: "test budget exhausted"}, nil
	}
	return AdmissionResult{
		Allowed: true,
		Context: ctx,
		Complete: func(err error, _ bool) *FeatureDeferral {
			if a.deferCompletedErrors && err != nil {
				return &FeatureDeferral{RetryAt: a.retryAt, Detail: "test feature deferred"}
			}
			return nil
		},
	}, nil
}

type archiveServiceProvider struct {
	kind                  platform.Kind
	host                  string
	caps                  platform.Capabilities
	mu                    sync.Mutex
	calls                 []string
	issueInventoryErr     error
	historicalIssuePages  map[string]platform.Page[platform.Issue]
	issueInventoryCursors []string
	issueInventoryStarted chan struct{}
	issueInventoryRelease chan struct{}
	updatedIssuePages     map[string]platform.Page[platform.Issue]
	updatedIssueErrors    map[string]error
	updatedIssueStarted   chan struct{}
	updatedIssueRelease   chan struct{}
	updatedMRPages        map[string]platform.Page[platform.MergeRequest]
	updatedMRErrors       map[string]error
	updatedIssueSince     []time.Time
	updatedMRSince        []time.Time
}

func newArchiveServiceProvider(kind platform.Kind, host string) *archiveServiceProvider {
	return &archiveServiceProvider{
		kind: kind, host: host,
		caps: platform.Capabilities{
			Archive: platform.ArchiveCapabilities{
				HistoricalIssues: true, HistoricalMergeRequests: true,
				OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
			},
		},
	}
}

func (p *archiveServiceProvider) Platform() platform.Kind             { return p.kind }
func (p *archiveServiceProvider) Host() string                        { return p.host }
func (p *archiveServiceProvider) Capabilities() platform.Capabilities { return p.caps }
func (p *archiveServiceProvider) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *archiveServiceProvider) ListIssuesPage(ctx context.Context, ref platform.RepoRef, query platform.ItemPageQuery) (platform.Page[platform.Issue], error) {
	if query.UpdatedSince != nil {
		p.record("updated_issues:" + query.Cursor)
		p.updatedIssueSince = append(p.updatedIssueSince, *query.UpdatedSince)
		if p.updatedIssueStarted != nil {
			select {
			case <-p.updatedIssueStarted:
			default:
				close(p.updatedIssueStarted)
			}
			select {
			case <-p.updatedIssueRelease:
			case <-ctx.Done():
				return platform.Page[platform.Issue]{}, ctx.Err()
			}
		}
		if err := p.updatedIssueErrors[query.Cursor]; err != nil {
			return platform.Page[platform.Issue]{}, err
		}
		if page, ok := p.updatedIssuePages[query.Cursor]; ok {
			return page, nil
		}
		return platform.Page[platform.Issue]{Exhausted: true}, nil
	}
	p.record("issues")
	p.issueInventoryCursors = append(p.issueInventoryCursors, query.Cursor)
	if p.issueInventoryStarted != nil {
		select {
		case <-p.issueInventoryStarted:
		default:
			close(p.issueInventoryStarted)
		}
		select {
		case <-p.issueInventoryRelease:
		case <-ctx.Done():
			return platform.Page[platform.Issue]{}, ctx.Err()
		}
	}
	if p.issueInventoryErr != nil {
		return platform.Page[platform.Issue]{}, p.issueInventoryErr
	}
	if page, ok := p.historicalIssuePages[query.Cursor]; ok {
		return page, nil
	}
	return platform.Page[platform.Issue]{Items: []platform.Issue{archiveTestIssue(ref)}, Exhausted: true}, nil
}
func (p *archiveServiceProvider) ListMergeRequestsPage(_ context.Context, ref platform.RepoRef, query platform.ItemPageQuery) (platform.Page[platform.MergeRequest], error) {
	if query.UpdatedSince != nil {
		p.record("updated_mrs:" + query.Cursor)
		p.updatedMRSince = append(p.updatedMRSince, *query.UpdatedSince)
		if err := p.updatedMRErrors[query.Cursor]; err != nil {
			return platform.Page[platform.MergeRequest]{}, err
		}
		if page, ok := p.updatedMRPages[query.Cursor]; ok {
			return page, nil
		}
		return platform.Page[platform.MergeRequest]{Exhausted: true}, nil
	}
	p.record("merge_requests")
	return platform.Page[platform.MergeRequest]{Items: []platform.MergeRequest{archiveTestMergeRequest(ref)}, Exhausted: true}, nil
}
func newArchiveTestService(t *testing.T, database *db.DB, registry *platform.Registry, refs []platform.RepoRef, admission Admission, now time.Time) *Service {
	t.Helper()
	service, err := NewService(database, registry, admission, archiveTestSource{refs: refs}, nil, fixedClock{value: now})
	require.NoError(t, err)
	return service
}

func archiveServiceRef(kind platform.Kind, host, name string) platform.RepoRef {
	return platform.RepoRef{
		Platform: kind, Host: host, Owner: "owner", Name: name,
		RepoPath: "owner/" + name, PlatformExternalID: "repo-" + host + "-" + name,
	}
}

func archiveServiceSeedRepo(t *testing.T, database *db.DB, ref platform.RepoRef) int64 {
	t.Helper()
	id, err := database.UpsertRepo(t.Context(), platformdb.DBRepoIdentity(ref))
	require.NoError(t, err)
	return id
}

func archiveTestTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func archiveTestIssue(ref platform.RepoRef) platform.Issue {
	return platform.Issue{Repo: ref, PlatformID: 1, PlatformExternalID: "issue-1", Number: 1, State: "closed", CreatedAt: archiveTestTime(), UpdatedAt: archiveTestTime(), LastActivityAt: archiveTestTime()}
}
func archiveTestMergeRequest(ref platform.RepoRef) platform.MergeRequest {
	created := archiveTestTime().Add(time.Minute)
	return platform.MergeRequest{Repo: ref, PlatformID: 2, PlatformExternalID: "mr-2", Number: 2, State: "closed", CreatedAt: created, UpdatedAt: created, LastActivityAt: created}
}

func requireEnsureConfigured(t *testing.T, s *Service, refs []platform.RepoRef) {
	t.Helper()
	_, err := s.EnsureConfigured(t.Context(), refs)
	require.NoError(t, err)
}
