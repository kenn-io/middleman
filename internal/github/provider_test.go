package github

import (
	"github.com/stretchr/testify/require"
	platformgithub "go.kenn.io/forge/platform/github"
	"testing"
	"time"
)

func newTestGitHubProvider(t *testing.T, host string, client Client) *platformgithub.Provider {
	t.Helper()
	provider, err := platformgithub.NewProvider(platformgithub.ProviderConfig{
		Host: host, Client: client, Clock: time.Now, ViewerCacheTTL: authenticatedViewerLoginTTL,
	})
	require.NoError(t, err)
	return provider
}
