package github

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

type completeReviewSyncProvider struct {
	*syncTestReadProvider
	threads       []platform.MergeRequestReviewThread
	reviewReadErr error
}

type completeReviewSyncFixture struct {
	database *db.DB
	syncer   *Syncer
	provider *completeReviewSyncProvider
	repo     RepoRef
	repoID   int64
	mrID     int64
}

func newCompleteReviewSyncFixture(t *testing.T, reviewCount int) completeReviewSyncFixture {
	t.Helper()
	require := require.New(t)
	database := openTestDB(t)
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	repo := RepoRef{
		Platform: platform.KindGitea, PlatformHost: "gitea.test",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
	repoID, err := database.UpsertRepo(t.Context(), verifiedDBRepoIdentity(platformRepoRef(repo)))
	require.NoError(err)
	mrID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 7, PlatformExternalID: "mr-7", Number: 7,
		URL: "https://gitea.test/acme/widget/pulls/7", Title: "cached MR",
		State: db.MergeRequestStateOpen, PlatformHeadSHA: "head-one",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		LastActivityAt: now.Add(-time.Hour),
	})
	require.NoError(err)
	require.NoError(database.UpsertMRReviewThreads(t.Context(), mrID, []db.MRReviewThread{{
		ProviderThreadID: "cached-thread", Body: "cached complete dataset",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}}))

	threads := make([]platform.MergeRequestReviewThread, reviewCount)
	for i := range reviewCount {
		reviewID := fmt.Sprintf("%d", i+1)
		threads[i] = platform.MergeRequestReviewThread{
			ProviderThreadID:  "thread-" + reviewID,
			ProviderReviewID:  reviewID,
			ProviderCommentID: "comment-" + reviewID,
			Body:              "review body " + reviewID, AuthorLogin: "reviewer",
			DirectURL: "https://gitea.test/acme/widget/pulls/7#issuecomment-" + reviewID,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	provider := &completeReviewSyncProvider{
		syncTestReadProvider: &syncTestReadProvider{
			kind: platform.KindGitea, host: "gitea.test",
			mergeRequests: []platform.MergeRequest{{
				Repo: platformRepoRef(repo), PlatformID: 7, PlatformExternalID: "mr-7",
				Number: 7, URL: "https://gitea.test/acme/widget/pulls/7", Title: "fresh MR",
				State: "open", HeadSHA: "head-one", CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now, LastActivityAt: now,
			}},
			readReviewThreads: true,
		},
		threads: threads,
	}
	provider.listReviewThreadsFn = func(context.Context, platform.RepoRef, int) ([]platform.MergeRequestReviewThread, error) {
		if provider.reviewReadErr != nil {
			return nil, provider.reviewReadErr
		}
		return append([]platform.MergeRequestReviewThread(nil), provider.threads...), nil
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := NewSyncerWithRegistry(
		registry, database, nil, []RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	return completeReviewSyncFixture{
		database: database, syncer: syncer, provider: provider,
		repo: repo, repoID: repoID, mrID: mrID,
	}
}

func (f completeReviewSyncFixture) sync(t *testing.T) error {
	t.Helper()
	return f.syncer.SyncMROnProvider(
		t.Context(), f.repo.Platform, f.repo.PlatformHost,
		f.repo.Owner, f.repo.Name, 7,
	)
}

func TestGitealikeReviewHydrationCompletesAtomicallyInOneSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newCompleteReviewSyncFixture(t, 17)

	require.NoError(fixture.sync(t))
	threads, err := fixture.database.ListMRReviewThreads(t.Context(), fixture.mrID)
	require.NoError(err)
	require.Len(threads, 17)
	assert.Equal("thread-1", threads[0].ProviderThreadID)
	assert.Equal("thread-17", threads[16].ProviderThreadID)
	assert.Equal(int32(1), fixture.provider.listReviewThreads.Load())
	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), fixture.repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.NotNil(mr.DetailFetchedAt)
}

func TestGitealikeReviewHydrationPreservesCompleteDatasetOnReadFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newCompleteReviewSyncFixture(t, 17)
	fixture.provider.reviewReadErr = errors.New("review hydration failed")

	require.Error(fixture.sync(t))
	threads, err := fixture.database.ListMRReviewThreads(t.Context(), fixture.mrID)
	require.NoError(err)
	require.Len(threads, 1)
	assert.Equal("cached-thread", threads[0].ProviderThreadID)
	mr, err := fixture.database.GetMergeRequestByRepoIDAndNumber(t.Context(), fixture.repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Nil(mr.DetailFetchedAt)
}
