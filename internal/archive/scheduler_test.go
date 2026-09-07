package archive

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func TestArchiveSchedulerDoesNotSerializeSameHostOutsideAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	scheduler := NewScheduler()
	groups := map[string][]resolvedRepository{"github\x00github.test": {{Ref: archiveServiceRef(platform.KindGitHub, "github.test", "repo")}}}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	work := func(context.Context, []resolvedRepository) (bool, error) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return true, nil
	}
	errCh := make(chan error, 2)
	go func() { _, err := scheduler.Run(t.Context(), groups, work); errCh <- err }()
	go func() { _, err := scheduler.Run(t.Context(), groups, work); errCh <- err }()
	require.Eventually(func() bool { return len(entered) == 2 }, time.Second, time.Millisecond)
	release <- struct{}{}
	release <- struct{}{}
	require.NoError(<-errCh)
	require.NoError(<-errCh)
	assert.Equal(int32(2), maximum.Load())
}

func TestArchiveSchedulerRunsIndependentHostsConcurrently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	scheduler := NewScheduler()
	groups := map[string][]resolvedRepository{
		"github\x00one.test": {{Ref: archiveServiceRef(platform.KindGitHub, "one.test", "one")}},
		"github\x00two.test": {{Ref: archiveServiceRef(platform.KindGitHub, "two.test", "two")}},
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	work := func(context.Context, []resolvedRepository) (bool, error) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return true, nil
	}
	done := make(chan error, 1)
	go func() { _, err := scheduler.Run(t.Context(), groups, work); done <- err }()
	require.Eventually(func() bool { return len(entered) == 2 }, time.Second, time.Millisecond)
	release <- struct{}{}
	release <- struct{}{}
	require.NoError(<-done)
	assert.Equal(int32(2), maximum.Load())
}

func TestArchiveWorkPrioritiesPreserveForegroundOrdering(t *testing.T) {
	assert := assert.New(t)
	assert.Less(PriorityNormalIndex, PriorityNotificationRefresh)
	assert.Less(PriorityNotificationRefresh, PriorityActiveDetail)
	assert.Less(PriorityActiveDetail, PriorityFullArchive)
	assert.Less(PriorityFullArchive, PriorityDiscoveryInventory)
}

func TestRunEligibleSkipsUnresolvableConfiguredRef(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	healthy := archiveServiceRef(platform.KindGitHub, "github.test", "healthy")
	ghost := platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.test",
		Owner: "owner", Name: "ghost", RepoPath: "owner/ghost",
	}
	healthyID := archiveServiceSeedRepo(t, database, healthy)
	provider := newArchiveServiceProvider(healthy.Platform, healthy.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{healthy, ghost}, &archiveTestAdmission{}, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{healthy})
	_, err = service.Start(t.Context(), []platform.RepoRef{healthy})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{healthyID})
	require.NoError(err)
	require.Len(states, 1)
	assert.True(states[0].IssueInventory.Complete())
}

func TestRunEligiblePropagatesStoreFailure(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	healthy := archiveServiceRef(platform.KindGitHub, "github.test", "healthy")
	archiveServiceSeedRepo(t, database, healthy)
	provider := newArchiveServiceProvider(healthy.Platform, healthy.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{healthy}, &archiveTestAdmission{}, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{healthy})

	// A broken store is an infrastructure failure, not a repository-scoped
	// one: the worker pass must surface it instead of reporting an idle,
	// successful iteration.
	require.NoError(database.Close())
	require.Error(service.RunEligible(t.Context()))
}

func TestArchiveBootstrapFeatureDeferralSkipsToNextRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	cooled := archiveServiceRef(platform.KindGitHub, "github.test", "a-cooled")
	ready := archiveServiceRef(platform.KindGitHub, "github.test", "b-ready")
	cooledID := archiveServiceSeedRepo(t, database, cooled)
	readyID := archiveServiceSeedRepo(t, database, ready)
	provider := newArchiveServiceProvider(cooled.Platform, cooled.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	admission := &archiveFeatureDeferringAdmission{
		repoName: cooled.Name,
		itemTypes: map[db.ArchiveItemType]bool{
			db.ArchiveItemTypeMergeRequest: true,
		},
	}
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{cooled, ready}, admission, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{cooled, ready})
	_, err = service.Start(t.Context(), []platform.RepoRef{cooled, ready})
	require.NoError(err)

	require.NoError(service.RunEligible(t.Context()))
	admission.enabled = true
	require.NoError(service.RunEligible(t.Context()))

	states, err := database.ListArchiveRepoStates(t.Context(), []int64{cooledID, readyID})
	require.NoError(err)
	require.Len(states, 2)
	assert.False(states[0].MergeRequestInventory.Complete())
	assert.True(states[1].IssueInventory.Complete())
}

func TestArchiveMaintenanceFeatureDeferralSkipsToNextRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	now := archiveTestTime()
	cooled := archiveServiceRef(platform.KindGitHub, "github.test", "a-cooled")
	ready := archiveServiceRef(platform.KindGitHub, "github.test", "b-ready")
	cooledID := archiveServiceSeedRepo(t, database, cooled)
	readyID := archiveServiceSeedRepo(t, database, ready)
	provider := newArchiveServiceProvider(cooled.Platform, cooled.Host)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	admission := &archiveFeatureDeferringAdmission{
		repoName: cooled.Name,
		itemTypes: map[db.ArchiveItemType]bool{
			db.ArchiveItemTypeIssue:        true,
			db.ArchiveItemTypeMergeRequest: true,
		},
	}
	service := newArchiveTestService(
		t, database, registry, []platform.RepoRef{cooled, ready}, admission, now,
	)
	requireEnsureConfigured(t, service, []platform.RepoRef{cooled, ready})
	_, err = service.Start(t.Context(), []platform.RepoRef{cooled, ready})
	require.NoError(err)

	var states []db.ArchiveRepoState
	for range 12 {
		states, err = database.ListArchiveRepoStates(t.Context(), []int64{cooledID, readyID})
		require.NoError(err)
		if states[0].InitialCompletedAt != nil && states[1].InitialCompletedAt != nil {
			break
		}
		require.NoError(service.RunEligible(t.Context()))
	}
	require.NotNil(states[0].InitialCompletedAt)
	require.NotNil(states[1].InitialCompletedAt)

	admission.enabled = true
	require.NoError(service.RunEligible(t.Context()))
	states, err = database.ListArchiveRepoStates(t.Context(), []int64{cooledID, readyID})
	require.NoError(err)
	require.NotNil(states[0].PromptScanStartedAt)
	assert.Nil(states[0].MaintenanceSucceededAt)
	assert.Nil(states[1].PromptScanStartedAt)
	assert.NotNil(states[1].MaintenanceSucceededAt)
}

type archiveFeatureDeferringAdmission struct {
	enabled   bool
	repoName  string
	itemTypes map[db.ArchiveItemType]bool
}

func (a *archiveFeatureDeferringAdmission) Admit(
	ctx context.Context,
	ref platform.RepoRef,
	itemType db.ArchiveItemType,
	_ int,
) (AdmissionResult, error) {
	if a.enabled && ref.Name == a.repoName && a.itemTypes[itemType] {
		return AdmissionResult{FeatureDeferred: &FeatureDeferral{
			RetryAt: archiveTestTime().Add(24 * time.Hour),
			Detail:  "repository feature cooldown active",
		}}, nil
	}
	return AdmissionResult{
		Allowed: true,
		Context: ctx,
		Complete: func(error, bool) *FeatureDeferral {
			return nil
		},
	}, nil
}
