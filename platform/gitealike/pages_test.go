package gitealike

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func pagesTestRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo",
	}
}

func TestUpdatedOrderWithoutWatermarkUsesUpdatedTraversal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	future := time.Now().UTC().Add(48 * time.Hour)
	transport := &archiveFakeTransport{
		fakeTransport: &fakeTransport{},
		pullPages: map[int][]PullRequestDTO{1: {
			{ID: 1, Index: 1, Created: base, Updated: future},
			{ID: 2, Index: 2, Created: base, Updated: base.Add(time.Hour)},
		}},
		pullPage:   map[int]Page{},
		issuePages: map[int][]IssueDTO{1: {{ID: 3, Index: 3, Created: base, Updated: base}}},
		issueLast:  1,
	}
	provider := NewProvider(platform.KindForgejo, "forge.example", transport)
	ref := pagesTestRef()

	mrPage, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated,
	})
	require.NoError(err)
	issuePage, err := provider.ListIssuesPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated,
	})
	require.NoError(err)

	assert.Equal("recentupdate", transport.pullOptions[0].Sort,
		"updated-order traversal must request update-time sort even without a watermark")
	require.Len(mrPage.Items, 1,
		"the scan-start pin must bound updated-order traversal by update time, not creation time")
	assert.Equal(2, mrPage.Items[0].Number)
	assert.True(mrPage.Exhausted)
	assert.True(transport.issueOptions[0].Since.IsZero(),
		"an updated-order query without a watermark carries no since filter")
	require.Len(issuePage.Items, 1)
	assert.Equal(3, issuePage.Items[0].Number)
}

func TestCreatedOrderResumeCoversBoundaryTimestampTies(t *testing.T) {
	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	tie := base.Add(time.Hour)

	t.Run("merge requests", func(t *testing.T) {
		require := require.New(t)
		a := PullRequestDTO{ID: 1, Index: 1, Created: base, Updated: base}
		b := PullRequestDTO{ID: 2, Index: 2, Created: tie, Updated: tie}
		c := PullRequestDTO{ID: 3, Index: 3, Created: tie, Updated: tie}
		d := PullRequestDTO{ID: 4, Index: 4, Created: tie.Add(time.Hour), Updated: tie.Add(time.Hour)}
		transport := &archiveFakeTransport{
			fakeTransport: &fakeTransport{},
			pullPages:     map[int][]PullRequestDTO{1: {a, b}, 2: {c, d}},
			pullPage:      map[int]Page{1: {Next: 2}},
		}
		provider := NewProvider(platform.KindForgejo, "forge.example", transport)
		query := func(cursor string) platform.ItemPageQuery {
			return platform.ItemPageQuery{
				Order: platform.ItemOrderCreated, Cursor: cursor,
			}
		}

		first, err := provider.ListMergeRequestsPage(t.Context(), pagesTestRef(), query(""))
		require.NoError(err)
		require.NotEmpty(first.NextCursor)
		delivered := make([]int, 0, 8)
		for _, item := range first.Items {
			delivered = append(delivered, item.Number)
		}
		// Interruption: the provider re-orders the equal-created items across
		// the consumed page boundary while the scan is suspended.
		transport.pullPages = map[int][]PullRequestDTO{1: {a, c}, 2: {b, d}}

		cursor := first.NextCursor
		for range 20 {
			page, err := provider.ListMergeRequestsPage(t.Context(), pagesTestRef(), query(cursor))
			require.NoError(err)
			for _, item := range page.Items {
				delivered = append(delivered, item.Number)
			}
			if page.Exhausted {
				cursor = ""
				break
			}
			require.NotEmpty(page.NextCursor)
			cursor = page.NextCursor
		}
		require.Empty(cursor, "traversal must reach exhaustion")
		assert.Subset(t, delivered, []int{1, 2, 3, 4},
			"equal-created items across the resume boundary must survive an interrupted scan")
	})

	t.Run("issues", func(t *testing.T) {
		require := require.New(t)
		a := IssueDTO{ID: 1, Index: 1, Created: base, Updated: base}
		b := IssueDTO{ID: 2, Index: 2, Created: tie, Updated: tie}
		c := IssueDTO{ID: 3, Index: 3, Created: tie, Updated: tie}
		d := IssueDTO{ID: 4, Index: 4, Created: tie.Add(time.Hour), Updated: tie.Add(time.Hour)}
		transport := &archiveFakeTransport{
			fakeTransport: &fakeTransport{},
			issuePages:    map[int][]IssueDTO{1: {d, c}, 2: {b, a}},
			issueLast:     2,
		}
		provider := NewProvider(platform.KindForgejo, "forge.example", transport)
		query := func(cursor string) platform.ItemPageQuery {
			return platform.ItemPageQuery{
				Order: platform.ItemOrderCreated, Cursor: cursor,
			}
		}

		discovery, err := provider.ListIssuesPage(t.Context(), pagesTestRef(), query(""))
		require.NoError(err)
		require.True(discovery.ProgressOnly)
		oldest, err := provider.ListIssuesPage(t.Context(), pagesTestRef(), query(discovery.NextCursor))
		require.NoError(err)
		require.NotEmpty(oldest.NextCursor)
		delivered := make([]int, 0, 8)
		for _, item := range oldest.Items {
			delivered = append(delivered, item.Number)
		}
		// Interruption: the provider re-orders the equal-created items across
		// the consumed page boundary while the scan is suspended.
		transport.issuePages = map[int][]IssueDTO{1: {d, b}, 2: {c, a}}

		cursor := oldest.NextCursor
		for range 20 {
			page, err := provider.ListIssuesPage(t.Context(), pagesTestRef(), query(cursor))
			require.NoError(err)
			for _, item := range page.Items {
				delivered = append(delivered, item.Number)
			}
			if page.Exhausted {
				cursor = ""
				break
			}
			require.NotEmpty(page.NextCursor)
			cursor = page.NextCursor
		}
		require.Empty(cursor, "traversal must reach exhaustion")
		assert.Subset(t, delivered, []int{1, 2, 3, 4},
			"equal-created items across the resume boundary must survive an interrupted scan")
	})
}

func TestCollectTransportPagesRejectsRepeatedPages(t *testing.T) {
	_, err := collectTransportPages(t.Context(), func(_ context.Context, opts PageOptions) ([]int, Page, error) {
		return []int{opts.Page}, Page{Next: opts.Page}, nil
	})
	require.ErrorIs(t, err, platform.ErrProviderContract)
}

func TestCollectTransportPagesBoundsPageBudget(t *testing.T) {
	_, err := collectTransportPages(t.Context(), func(_ context.Context, opts PageOptions) ([]int, Page, error) {
		return []int{opts.Page}, Page{Next: opts.Page + 1}, nil
	})
	require.ErrorIs(t, err, platform.ErrPageLimit)
}
