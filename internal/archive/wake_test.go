package archive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestRunPassIdleRepositoriesReportNoWorkWithoutRepositoryResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	first := archiveServiceRef(platform.KindGitHub, "github.test", "first")
	second := archiveServiceRef(platform.KindGitHub, "github.test", "second")
	firstID := archiveServiceSeedRepo(t, database, first)
	secondID := archiveServiceSeedRepo(t, database, second)
	provider := newArchiveServiceProvider(first.Platform, first.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{first, second}, &archiveTestAdmission{}, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{first, second})
	_, err = service.Start(t.Context(), []platform.RepoRef{first, second})
	require.NoError(err)

	// Drive passes until inventory, hydration, and the first maintenance
	// scan are done and a pass reports no attempted work.
	worked := true
	for range 20 {
		worked, err = service.RunPass(t.Context())
		require.NoError(err)
		if !worked {
			break
		}
	}
	require.False(worked, "archive worker did not become idle")
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{firstID, secondID})
	require.NoError(err)
	require.Len(states, 2)
	for _, state := range states {
		assert.NotNil(state.InitialCompletedAt)
		assert.NotNil(state.MaintenanceSucceededAt)
	}

	// Hide the repository catalog: any repository resolution query from the
	// following idle passes now fails, so a clean pass proves the cached
	// resolution was used.
	_, err = database.WriteDB().ExecContext(t.Context(),
		`ALTER TABLE forge_repos RENAME TO forge_repos_hidden`)
	require.NoError(err)
	for range 3 {
		worked, err = service.RunPass(t.Context())
		require.NoError(err)
		assert.False(worked)
	}

	// A cold service must resolve and therefore fail, which proves the hidden
	// catalog is what the cache avoided.
	cold := newArchiveTestService(
		t, database, registry, []platform.RepoRef{first, second}, &archiveTestAdmission{}, now,
	)
	_, err = cold.RunPass(t.Context())
	require.Error(err)
}

func TestWorkerRepositoriesCacheFollowsConfigurationAndReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	first := archiveServiceRef(platform.KindGitHub, "github.test", "first")
	second := archiveServiceRef(platform.KindGitHub, "github.test", "second")
	archiveServiceSeedRepo(t, database, first)
	provider := newArchiveServiceProvider(first.Platform, first.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	source := &archiveMutableSource{refs: []platform.RepoRef{first, second}}
	service, err := NewService(database, registry, &archiveTestAdmission{}, source, nil, fixedClock{value: now})
	require.NoError(err)

	resolved, err := service.workerRepositories(t.Context())
	require.NoError(err)
	require.Len(resolved, 1, "an unseeded configured repository is skipped")
	again, err := service.workerRepositories(t.Context())
	require.NoError(err)
	require.Len(again, 1)
	assert.Same(&resolved[0], &again[0], "unchanged configuration reuses the cached resolution")

	// Seeding the second repository is a repository reconciliation write and
	// must make the next pass resolve it.
	archiveServiceSeedRepo(t, database, second)
	resolved, err = service.workerRepositories(t.Context())
	require.NoError(err)
	require.Len(resolved, 2)

	// Dropping a configured repository takes effect without a store change.
	source.refs = []platform.RepoRef{second}
	resolved, err = service.workerRepositories(t.Context())
	require.NoError(err)
	require.Len(resolved, 1)
	assert.Equal(second.Name, resolved[0].Ref.Name)
}

func TestRunPassReportsAdmissionDenialAsIdle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")
	repoID := archiveServiceSeedRepo(t, database, ref)
	provider := newArchiveServiceProvider(ref.Platform, ref.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	retryAt := now.Add(time.Second)
	admission := &archiveTestAdmission{deny: true, retryAt: retryAt}
	service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, admission, now)
	requireEnsureConfigured(t, service, []platform.RepoRef{ref})
	_, err = service.Start(t.Context(), []platform.RepoRef{ref})
	require.NoError(err)

	// Live sync holding the provider denies admission before any request is
	// made. That is not work: the loop must back off rather than re-run at
	// the pacing interval for the whole sync.
	worked, err := service.RunPass(t.Context())
	require.NoError(err)
	assert.False(worked)
	states, err := database.ListArchiveRepoStates(t.Context(), []int64{repoID})
	require.NoError(err)
	require.Len(states, 1)
	require.NotNil(states[0].NextRetryAt)
	assert.Equal(retryAt, *states[0].NextRetryAt)
	assert.Equal(1, admission.calls)

	// Once admission opens again the pass reaches the provider and reports work.
	admission.deny = false
	service.clock = fixedClock{value: retryAt}
	worked, err = service.RunPass(t.Context())
	require.NoError(err)
	assert.True(worked)
}

// preemptingAdmission admits every request with an already-canceled context,
// the shape live work leaves behind when it preempts an archive request.
type preemptingAdmission struct{}

func (preemptingAdmission) Admit(
	context.Context,
	platform.RepoRef,
	db.ArchiveItemType,
	int,
) (AdmissionResult, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return AdmissionResult{
		Allowed: true, Context: ctx,
		Complete: func(error, bool) *FeatureDeferral { return nil },
	}, nil
}

func TestRunPassReportsProviderAttemptedDeferralsAsWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := archiveTestTime()
	ref := archiveServiceRef(platform.KindGitHub, "github.test", "repo")

	t.Run("feature deferral after a provider request", func(t *testing.T) {
		database := dbtest.Open(t)
		archiveServiceSeedRepo(t, database, ref)
		provider := newArchiveServiceProvider(ref.Platform, ref.Host)
		provider.issueInventoryErr = errors.New("issues disabled for repository")
		registry, err := platform.NewRegistry(provider)
		require.NoError(err)
		admission := &archiveTestAdmission{deferCompletedErrors: true, retryAt: now.Add(24 * time.Hour)}
		service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, admission, now)
		requireEnsureConfigured(t, service, []platform.RepoRef{ref})
		_, err = service.Start(t.Context(), []platform.RepoRef{ref})
		require.NoError(err)

		worked, err := service.RunPass(t.Context())
		require.NoError(err)
		assert.True(worked, "the provider was contacted, so sibling work must not wait out a backoff")
	})

	t.Run("request preempted by live work", func(t *testing.T) {
		database := dbtest.Open(t)
		archiveServiceSeedRepo(t, database, ref)
		provider := newArchiveServiceProvider(ref.Platform, ref.Host)
		provider.issueInventoryErr = context.Canceled
		registry, err := platform.NewRegistry(provider)
		require.NoError(err)
		service := newArchiveTestService(t, database, registry, []platform.RepoRef{ref}, preemptingAdmission{}, now)
		requireEnsureConfigured(t, service, []platform.RepoRef{ref})
		_, err = service.Start(t.Context(), []platform.RepoRef{ref})
		require.NoError(err)

		worked, err := service.RunPass(t.Context())
		require.NoError(err)
		assert.True(worked, "a preempted request reached the provider and stays claimable")
	})
}
