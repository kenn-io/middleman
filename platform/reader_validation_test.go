package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type testPageReadProvider struct {
	testProvider
	issues        Page[Issue]
	mergeRequests Page[MergeRequest]
}

func (p testPageReadProvider) ListIssuesPage(
	context.Context,
	RepoRef,
	ItemPageQuery,
) (Page[Issue], error) {
	return p.issues, nil
}

func (p testPageReadProvider) ListMergeRequestsPage(
	context.Context,
	RepoRef,
	ItemPageQuery,
) (Page[MergeRequest], error) {
	return p.mergeRequests, nil
}

func TestUpdatedMergeRequestInventoryDoesNotRequireHistoricalCapability(t *testing.T) {
	require := require.New(t)
	ref := RepoRef{
		Platform: KindGitLab, Host: "gitlab.example.com", Owner: "group",
		Name: "project", RepoPath: "group/project",
	}
	provider := testPageReadProvider{
		kind:          KindGitLab,
		host:          ref.Host,
		caps:          Capabilities{Archive: ArchiveCapabilities{HistoricalIssues: true}},
		mergeRequests: Page[MergeRequest]{Exhausted: true},
	}
	registry, err := NewRegistry(provider)
	require.NoError(err)
	reader, err := registry.MergeRequestPageReader(provider.kind, provider.host)
	require.NoError(err)

	_, err = reader.ListMergeRequestsPage(t.Context(), ref, ItemPageQuery{Order: ItemOrderUpdated})
	require.NoError(err)
	_, err = reader.ListMergeRequestsPage(t.Context(), ref, ItemPageQuery{Order: ItemOrderCreated})
	require.ErrorIs(err, ErrUnsupportedCapability)
}

func TestIssueInventoryRejectsItemsFromAnotherRepository(t *testing.T) {
	ref := RepoRef{
		Platform: KindGitHub, Host: "github.com", Owner: "octo-org",
		Name: "widgets", RepoPath: "octo-org/widgets",
	}
	foreign := ref
	foreign.Owner, foreign.RepoPath = "other", "other/widgets"
	provider := testPageReadProvider{
		kind: KindGitHub,
		host: "github.com",
		caps: Capabilities{Archive: ArchiveCapabilities{HistoricalIssues: true}},
		issues: Page[Issue]{
			Items:     []Issue{{Repo: foreign, Number: 7}},
			Exhausted: true,
		},
	}
	registry, err := NewRegistry(provider)
	require.NoError(t, err)
	reader, err := registry.IssuePageReader(provider.kind, provider.host)
	require.NoError(t, err)

	_, err = reader.ListIssuesPage(t.Context(), ref, ItemPageQuery{
		Order: ItemOrderCreated,
	})
	require.ErrorIs(t, err, ErrProviderContract)
}
