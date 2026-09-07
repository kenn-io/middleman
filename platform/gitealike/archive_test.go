package gitealike

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestArchiveIssueInventoryWalksOldestPagesAndExcludesPullRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	transport := &archiveFakeTransport{
		fakeTransport: &fakeTransport{},
		issuePages: map[int][]IssueDTO{
			1: {{ID: 3, Index: 3, Created: base.Add(2 * time.Hour), Updated: base.Add(2 * time.Hour)}},
			3: {
				{ID: 2, Index: 2, Created: base.Add(time.Hour), Updated: base.Add(time.Hour), IsPullRequest: true},
				{ID: 1, Index: 1, Created: base, Updated: base},
			},
		},
		issueLast: 3,
	}
	provider := NewProvider(platform.KindForgejo, "forge.example", transport)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo"}

	discovery, err := provider.ListIssuesPage(context.Background(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated,
	})
	require.NoError(err)
	assert.True(discovery.ProgressOnly)
	assert.NotEmpty(discovery.NextCursor)

	oldest, err := provider.ListIssuesPage(context.Background(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated, Cursor: discovery.NextCursor,
	})
	require.NoError(err)
	assert.Equal([]int{1}, []int{oldest.Items[0].Number})
	assert.Equal([]int{1, 3}, transport.issueRequests)
	assert.NotEmpty(oldest.NextCursor)

	_, err = provider.ListIssuesPage(context.Background(), platform.RepoRef{
		Platform: platform.KindForgejo, Host: "other.example", Owner: "owner", Name: "repo",
	}, platform.ItemPageQuery{
		Order: platform.ItemOrderCreated, Cursor: oldest.NextCursor,
	})
	assert.Error(err)
}

func TestArchiveUpdatedInventoryUsesInclusiveWatermarkAndProviderSort(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	since := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	transport := &archiveFakeTransport{
		fakeTransport: &fakeTransport{},
		issuePages: map[int][]IssueDTO{1: {
			{ID: 1, Index: 1, Created: since.Add(-time.Hour), Updated: since},
			{ID: 3, Index: 3, Created: since.Add(-time.Hour), Updated: since.Add(-500 * time.Millisecond)},
		}},
		pullPages: map[int][]PullRequestDTO{1: {{ID: 2, Index: 2, Created: since.Add(-time.Hour), Updated: since}}},
	}
	provider := NewProvider(platform.KindGitea, "git.example", transport)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "git.example", Owner: "owner", Name: "repo"}

	issues, err := provider.ListIssuesPage(context.Background(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated, UpdatedSince: &since,
	})
	require.NoError(err)
	pulls, err := provider.ListMergeRequestsPage(context.Background(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated, UpdatedSince: &since,
	})
	require.NoError(err)

	assert.Len(issues.Items, 2)
	assert.Len(pulls.Items, 1)
	assert.Equal(since.Add(-time.Second), transport.issueOptions[0].Since)
	assert.Equal("recentupdate", transport.pullOptions[0].Sort)
	assert.Equal(2, transport.requests)
}

func TestArchiveUpdatedMergeRequestsStopAfterOverlappedWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	since := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	transport := &archiveFakeTransport{
		fakeTransport: &fakeTransport{},
		pullPages: map[int][]PullRequestDTO{
			1: {{ID: 3, Index: 3, Updated: since.Add(time.Minute)}},
			2: {
				{ID: 2, Index: 2, Updated: since.Add(-500 * time.Millisecond)},
				{ID: 1, Index: 1, Updated: since.Add(-2 * time.Second)},
			},
		},
		pullPage: map[int]Page{1: {Next: 2}, 2: {Next: 3}},
	}
	provider := NewProvider(platform.KindForgejo, "forge.example", transport)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo"}

	first, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated, UpdatedSince: &since,
	})
	require.NoError(err)
	second, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
		Order:        platform.ItemOrderUpdated,
		UpdatedSince: &since, Cursor: first.NextCursor,
	})
	require.NoError(err)

	assert.Equal([]int{3}, []int{first.Items[0].Number})
	require.Len(second.Items, 1)
	assert.Equal(2, second.Items[0].Number)
	assert.True(second.Exhausted)
	assert.Equal([]int{1, 2}, []int{transport.pullOptions[0].Page, transport.pullOptions[1].Page})
}

type archiveFakeTransport struct {
	*fakeTransport
	issuePages           map[int][]IssueDTO
	pullPages            map[int][]PullRequestDTO
	issueLast            int
	issueOptions         []ArchiveListOptions
	pullOptions          []ArchiveListOptions
	issueRequests        []int
	requests             int
	comments             []CommentDTO
	issueCommentPages    map[int][]CommentDTO
	pullCommentPages     map[int][]CommentDTO
	commentPage          map[int]Page
	issueCommentRequests []int
	pullCommentRequests  []int
	reviews              []ReviewDTO
	reviewPage           Page
	reviewPages          map[int][]ReviewDTO
	reviewPageMap        map[int]Page
	reviewRequests       []int
	commits              []CommitDTO
	pullPage             map[int]Page
	issueErr             error
}

func (t *archiveFakeTransport) ListIssuesPage(_ context.Context, _ platform.RepoRef, opts ArchiveListOptions) ([]IssueDTO, Page, error) {
	t.requests++
	t.issueRequests = append(t.issueRequests, opts.Page)
	t.issueOptions = append(t.issueOptions, opts)
	return t.issuePages[opts.Page], Page{Last: t.issueLast}, nil
}

func (t *archiveFakeTransport) ListPullRequestsPage(_ context.Context, _ platform.RepoRef, opts ArchiveListOptions) ([]PullRequestDTO, Page, error) {
	t.requests++
	t.pullOptions = append(t.pullOptions, opts)
	return t.pullPages[opts.Page], t.pullPage[opts.Page], nil
}

func (t *archiveFakeTransport) ListPullRequestComments(_ context.Context, _ platform.RepoRef, _ int, opts PageOptions) ([]CommentDTO, Page, error) {
	t.requests++
	t.pullCommentRequests = append(t.pullCommentRequests, opts.Page)
	if t.pullCommentPages != nil {
		return t.pullCommentPages[opts.Page], t.commentPage[opts.Page], nil
	}
	return t.comments, Page{}, nil
}

func (t *archiveFakeTransport) ListIssueComments(_ context.Context, _ platform.RepoRef, _ int, opts PageOptions) ([]CommentDTO, Page, error) {
	t.requests++
	t.issueCommentRequests = append(t.issueCommentRequests, opts.Page)
	return t.issueCommentPages[opts.Page], t.commentPage[opts.Page], nil
}

func (t *archiveFakeTransport) ListPullRequestReviews(_ context.Context, _ platform.RepoRef, _ int, opts PageOptions) ([]ReviewDTO, Page, error) {
	t.requests++
	t.reviewRequests = append(t.reviewRequests, opts.Page)
	if t.reviewPages != nil {
		return t.reviewPages[opts.Page], t.reviewPageMap[opts.Page], nil
	}
	return t.reviews, t.reviewPage, nil
}

func (t *archiveFakeTransport) ListPullRequestCommits(context.Context, platform.RepoRef, int, PageOptions) ([]CommitDTO, Page, error) {
	t.requests++
	return t.commits, Page{}, nil
}

func (t *archiveFakeTransport) GetIssue(context.Context, platform.RepoRef, int) (IssueDTO, error) {
	return IssueDTO{}, t.issueErr
}
