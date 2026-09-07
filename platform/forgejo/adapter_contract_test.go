package forgejo

import (
	"context"
	"net/http"
	"testing"

	ghsync "go.kenn.io/forge/internal/github"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/gitealiketest"
	"go.kenn.io/forge/platform"
)

func TestGiteaLikeAdapterContract(t *testing.T) {
	capabilities := gitealiketest.BaseCapabilities()
	capabilities.ReviewDraftMutation = true
	capabilities.ReadReviewThreads = true

	gitealiketest.Run(t, gitealiketest.Adapter{
		Kind:         platform.KindForgejo,
		Host:         "forgejo.test",
		Token:        "forgejo-token",
		Capabilities: capabilities,
		NewClient: func(
			t *testing.T,
			baseURL string,
			token string,
			options gitealiketest.ClientOptions,
		) gitealiketest.TestClient {
			clientOptions := []ClientOption{WithBaseURLForTesting(baseURL), WithTransport(http.DefaultTransport)}
			if options.ForegroundTimeout > 0 {
				clientOptions = append(clientOptions, WithForegroundTimeoutForTesting(options.ForegroundTimeout))
			}
			if options.RateTracker != nil {
				clientOptions = append(clientOptions, WithRateTracker(options.RateTracker))
			}
			if options.SyncBudget != nil {
				clientOptions = append(clientOptions, WithTransport(ghsync.WrapSyncBudgetTransport(http.DefaultTransport, options.SyncBudget)))
			}
			client, err := NewClient("forgejo.test", testTokenSource(token), clientOptions...)
			require.NoError(t, err)
			return gitealiketest.TestClient{
				Client: client,
				LookupRepository: func(ctx context.Context, owner, name string) (string, error) {
					repository, err := client.transport.getRepositoryRaw(ctx, owner, name)
					if err != nil || repository == nil {
						return "", err
					}
					return repository.Name, nil
				},
			}
		},
	})
}
