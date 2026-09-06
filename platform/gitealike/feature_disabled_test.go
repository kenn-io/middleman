package gitealike

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

type featureFailureTransport struct {
	*fakeTransport
	repository      RepositoryDTO
	repositoryErr   error
	repositoryCalls int
	failAt          string
	operationErr    error
}

func (t *featureFailureTransport) GetRepository(context.Context, string, string) (RepositoryDTO, error) {
	t.repositoryCalls++
	return t.repository, t.repositoryErr
}

func (t *featureFailureTransport) failure(name string) error {
	if t.failAt == name {
		return t.operationErr
	}
	return nil
}

func (t *featureFailureTransport) ListOpenPullRequests(
	context.Context,
	platform.RepoRef,
	PageOptions,
) ([]PullRequestDTO, Page, error) {
	return nil, Page{}, t.failure("open_pull_requests")
}

func (t *featureFailureTransport) GetPullRequest(
	context.Context,
	platform.RepoRef,
	int,
) (PullRequestDTO, error) {
	return PullRequestDTO{}, t.failure("pull_request")
}

func (t *featureFailureTransport) ListPullRequestComments(
	context.Context,
	platform.RepoRef,
	int,
	PageOptions,
) ([]CommentDTO, Page, error) {
	return nil, Page{}, t.failure("pull_comments")
}

func (t *featureFailureTransport) ListPullRequestReviews(
	context.Context,
	platform.RepoRef,
	int,
	PageOptions,
) ([]ReviewDTO, Page, error) {
	return nil, Page{}, t.failure("pull_reviews")
}

func (t *featureFailureTransport) ListPullRequestCommits(
	context.Context,
	platform.RepoRef,
	int,
	PageOptions,
) ([]CommitDTO, Page, error) {
	return nil, Page{}, t.failure("pull_commits")
}

func (t *featureFailureTransport) ListIssueTimeline(
	context.Context,
	platform.RepoRef,
	int,
	PageOptions,
) ([]TimelineEventDTO, Page, error) {
	return nil, Page{}, t.failure("timeline")
}

func (t *featureFailureTransport) ListOpenIssues(
	context.Context,
	platform.RepoRef,
	PageOptions,
) ([]IssueDTO, Page, error) {
	return nil, Page{}, t.failure("open_issues")
}

func (t *featureFailureTransport) GetIssue(
	context.Context,
	platform.RepoRef,
	int,
) (IssueDTO, error) {
	return IssueDTO{}, t.failure("issue")
}

func (t *featureFailureTransport) ListIssueComments(
	context.Context,
	platform.RepoRef,
	int,
	PageOptions,
) ([]CommentDTO, Page, error) {
	return nil, Page{}, t.failure("issue_comments")
}

func (t *featureFailureTransport) ListIssuesPage(
	context.Context,
	platform.RepoRef,
	ArchiveListOptions,
) ([]IssueDTO, Page, error) {
	return nil, Page{}, t.failure("archive_issues")
}

func (t *featureFailureTransport) ListPullRequestsPage(
	context.Context,
	platform.RepoRef,
	ArchiveListOptions,
) ([]PullRequestDTO, Page, error) {
	return nil, Page{}, t.failure("archive_pull_requests")
}

func TestRepositoryFeatureError(t *testing.T) {
	enabled := true
	disabled := false
	metadataFailure := errors.New("repository metadata failed")

	tests := []struct {
		name              string
		feature           string
		operationErr      error
		issuesEnabled     *bool
		mergeRequests     *bool
		repositoryErr     error
		wantTarget        error
		wantOriginal      bool
		wantMetadataCalls int
	}{
		{
			name: "forbidden disabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusForbidden}, issuesEnabled: &disabled,
			wantTarget: platform.ErrRepositoryFeatureDisabled, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "not found disabled merge requests", feature: platform.RepositoryFeatureMergeRequests,
			operationErr: &HTTPError{StatusCode: http.StatusNotFound}, mergeRequests: &disabled,
			wantTarget: platform.ErrRepositoryFeatureDisabled, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "gone disabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusGone}, issuesEnabled: &disabled,
			wantTarget: platform.ErrRepositoryFeatureDisabled, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "forbidden enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusForbidden}, issuesEnabled: &enabled,
			wantTarget: platform.ErrPermissionDenied, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "not found enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusNotFound}, issuesEnabled: &enabled,
			wantTarget: platform.ErrNotFound, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "gone enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusGone}, issuesEnabled: &enabled,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "unknown metadata", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusNotFound},
			wantTarget:   platform.ErrNotFound, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "metadata lookup failure", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusNotFound}, repositoryErr: metadataFailure,
			wantTarget: platform.ErrNotFound, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "unauthorized", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusUnauthorized},
			wantTarget:   platform.ErrPermissionDenied,
		},
		{
			name: "rate limited", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusTooManyRequests},
			wantTarget:   platform.ErrRateLimited,
		},
		{
			name: "server failure", feature: platform.RepositoryFeatureIssues,
			operationErr: &HTTPError{StatusCode: http.StatusInternalServerError}, wantOriginal: true,
		},
		{
			name: "context canceled", feature: platform.RepositoryFeatureIssues,
			operationErr: context.Canceled, wantTarget: context.Canceled,
		},
		{
			name: "deadline exceeded", feature: platform.RepositoryFeatureIssues,
			operationErr: context.DeadlineExceeded, wantTarget: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transport := &featureFailureTransport{
				fakeTransport: &fakeTransport{},
				repository: RepositoryDTO{
					ID: 1, Owner: UserDTO{UserName: "owner"}, Name: "repo", FullName: "owner/repo",
					IssuesEnabled: tt.issuesEnabled, MergeRequestsEnabled: tt.mergeRequests,
				},
				repositoryErr: tt.repositoryErr,
			}
			provider := NewProvider(platform.KindForgejo, "forge.example", transport)
			ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo"}

			classified := provider.repositoryFeatureError(t.Context(), ref, tt.feature, tt.operationErr)

			require := require.New(t)
			assert := assert.New(t)
			if tt.wantTarget != nil {
				require.ErrorIs(classified, tt.wantTarget)
			}
			if tt.wantOriginal {
				require.ErrorIs(classified, tt.operationErr)
			}
			assert.Equal(tt.wantMetadataCalls, transport.repositoryCalls)
			if errors.Is(classified, platform.ErrRepositoryFeatureDisabled) {
				var platformErr *platform.Error
				require.ErrorAs(classified, &platformErr)
				assert.Equal(platform.KindForgejo, platformErr.Provider)
				assert.Equal("forge.example", platformErr.PlatformHost)
				assert.Equal(tt.feature, platformErr.Capability)
			}
		})
	}
}

func TestRepositoryItemLookupReusesFeatureMetadataConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	enabled := true
	operationErr := &HTTPError{StatusCode: http.StatusNotFound}
	transport := &featureFailureTransport{
		fakeTransport: &fakeTransport{},
		repository: RepositoryDTO{
			ID: 1, Owner: UserDTO{UserName: "owner"}, Name: "repo", FullName: "owner/repo",
			IssuesEnabled: &enabled,
		},
		failAt:       "issue",
		operationErr: operationErr,
	}
	provider := NewProvider(platform.KindForgejo, "forge.example", transport)
	ref := platform.RepoRef{
		Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo",
	}

	_, err := provider.GetIssue(t.Context(), ref, 7)

	require.ErrorIs(err, platform.ErrLookupNotPresent)
	require.ErrorIs(err, platform.ErrNotFound)
	require.ErrorIs(err, operationErr)
	assert.Equal(1, transport.repositoryCalls)
}

func TestRepositoryFeatureReadBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		failAt  string
		feature string
		read    func(context.Context, *Provider, platform.RepoRef) error
	}{
		{
			name: "open merge requests", failAt: "open_pull_requests", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListOpenMergeRequests(ctx, ref)
				return err
			},
		},
		{
			name: "merge request detail", failAt: "pull_request", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.GetMergeRequest(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request comments", failAt: "pull_comments", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListMergeRequestEvents(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request reviews", failAt: "pull_reviews", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListMergeRequestEvents(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request commits", failAt: "pull_commits", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListMergeRequestEvents(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request timeline", failAt: "timeline", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListMergeRequestEvents(ctx, ref, 7)
				return err
			},
		},
		{
			name: "open issues", failAt: "open_issues", feature: platform.RepositoryFeatureIssues,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListOpenIssues(ctx, ref)
				return err
			},
		},
		{
			name: "issue detail", failAt: "issue", feature: platform.RepositoryFeatureIssues,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.GetIssue(ctx, ref, 5)
				return err
			},
		},
		{
			name: "issue comments", failAt: "issue_comments", feature: platform.RepositoryFeatureIssues,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListIssueEvents(ctx, ref, 5)
				return err
			},
		},
		{
			name: "issue timeline", failAt: "timeline", feature: platform.RepositoryFeatureIssues,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListIssueEvents(ctx, ref, 5)
				return err
			},
		},
		{
			name: "archive issues", failAt: "archive_issues", feature: platform.RepositoryFeatureIssues,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListIssuesPage(ctx, ref, platform.ItemPageQuery{Order: platform.ItemOrderCreated})
				return err
			},
		},
		{
			name: "archive merge requests", failAt: "archive_pull_requests", feature: platform.RepositoryFeatureMergeRequests,
			read: func(ctx context.Context, provider *Provider, ref platform.RepoRef) error {
				_, err := provider.ListMergeRequestsPage(ctx, ref, platform.ItemPageQuery{Order: platform.ItemOrderCreated})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issuesEnabled := tt.feature != platform.RepositoryFeatureIssues
			mergeRequestsEnabled := tt.feature != platform.RepositoryFeatureMergeRequests
			transport := &featureFailureTransport{
				fakeTransport: &fakeTransport{},
				repository: RepositoryDTO{
					ID: 1, Owner: UserDTO{UserName: "owner"}, Name: "repo", FullName: "owner/repo",
					IssuesEnabled: &issuesEnabled, MergeRequestsEnabled: &mergeRequestsEnabled,
				},
				failAt: tt.failAt, operationErr: &HTTPError{StatusCode: http.StatusNotFound},
			}
			provider := NewProvider(platform.KindGitea, "gitea.example", transport)
			ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example", Owner: "owner", Name: "repo"}

			err := tt.read(t.Context(), provider, ref)

			require.ErrorIs(t, err, platform.ErrRepositoryFeatureDisabled)
			assert.Equal(t, 1, transport.repositoryCalls)
		})
	}
}

func TestRepositoryFeatureTimelineSuppression(t *testing.T) {
	issuesEnabled := true
	transport := &featureFailureTransport{
		fakeTransport: &fakeTransport{},
		repository: RepositoryDTO{
			ID: 1, Owner: UserDTO{UserName: "owner"}, Name: "repo", FullName: "owner/repo",
			IssuesEnabled: &issuesEnabled,
		},
		failAt: "timeline", operationErr: &HTTPError{StatusCode: http.StatusNotFound},
	}
	provider := NewProvider(platform.KindForgejo, "forge.example", transport)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "forge.example", Owner: "owner", Name: "repo"}

	events, err := provider.ListIssueEvents(t.Context(), ref, 5)

	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Equal(t, 1, transport.repositoryCalls)
}

func TestRepositoryFeatureSuccessfulReadsDoNotFetchMetadata(t *testing.T) {
	transport := &featureFailureTransport{fakeTransport: &fakeTransport{}}
	provider := NewProvider(platform.KindGitea, "gitea.example", transport)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example", Owner: "owner", Name: "repo"}

	_, issueErr := provider.ListOpenIssues(t.Context(), ref)
	_, mergeRequestErr := provider.ListOpenMergeRequests(t.Context(), ref)

	require.NoError(t, issueErr)
	require.NoError(t, mergeRequestErr)
	assert.Zero(t, transport.repositoryCalls)
}
