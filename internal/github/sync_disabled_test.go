package github

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

type disabledSyncReviewThreadProvider struct {
	calls *atomic.Int32
}

func (p disabledSyncReviewThreadProvider) Platform() platform.Kind {
	return platform.KindGitHub
}

func (p disabledSyncReviewThreadProvider) Host() string {
	return platform.DefaultGitHubHost
}

func (p disabledSyncReviewThreadProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{ReadReviewThreads: true}
}

func (p disabledSyncReviewThreadProvider) ListMergeRequestReviewThreads(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestReviewThread, error) {
	p.calls.Add(1)
	return nil, nil
}

func TestDisabledSyncRejectsCapturedProviderReaderWhenInvoked(t *testing.T) {
	require := require.New(t)
	var calls atomic.Int32
	registry, err := platform.NewRegistry(disabledSyncReviewThreadProvider{calls: &calls})
	require.NoError(err)
	syncer := NewSyncerWithRegistry(registry, nil, nil, nil, time.Minute, nil, nil)
	reader, err := syncer.MergeRequestReviewThreadReader(
		platform.KindGitHub, platform.DefaultGitHubHost,
	)
	require.NoError(err)

	syncer.DisableSync()
	_, err = syncer.SyncRegistry().Provider(platform.KindGitHub, platform.DefaultGitHubHost)
	require.ErrorIs(err, platform.ErrSyncDisabled)
	_, err = syncer.Registry().Provider(platform.KindGitHub, platform.DefaultGitHubHost)
	require.NoError(err)
	_, err = reader.ListMergeRequestReviewThreads(t.Context(), platform.RepoRef{}, 7)
	require.ErrorIs(err, platform.ErrSyncDisabled)
	require.Zero(calls.Load())
}
