package gitealike

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestProviderCapabilitiesEnableSharedReadBehavior(t *testing.T) {
	provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{}, WithReadActions())

	assert.Equal(t, platform.Capabilities{
		ReadRepositories:  true,
		ReadMergeRequests: true,
		ReadIssues:        true,
		ReadComments:      true,
		ReadReleases:      true,
		ReadCI:            true,
	}, provider.Capabilities())
}

func TestProviderCapabilitiesEnableProvenMutations(t *testing.T) {
	provider := NewProvider(
		platform.KindForgejo,
		"codeberg.org",
		&fakeTransport{},
		WithReadActions(),
		WithMutations(),
	)

	assert.Equal(t, platform.Capabilities{
		ReadRepositories:    true,
		ReadMergeRequests:   true,
		ReadIssues:          true,
		ReadComments:        true,
		ReadReleases:        true,
		ReadCI:              true,
		CommentMutation:     true,
		StateMutation:       true,
		MergeMutation:       true,
		ReviewMutation:      true,
		IssueMutation:       true,
		AssigneeMutation:    true,
		MutationHeadBinding: true,
		SupportedReviewActions: []platform.ReviewAction{
			platform.ReviewActionComment,
			platform.ReviewActionApprove,
			platform.ReviewActionRequestChanges,
		},
		// ReviewerMutation stays false: fakeTransport does not
		// implement ReviewRequestTransport.
	}, provider.Capabilities())
}

func TestProviderCapabilitiesDoNotAdvertiseInlineReviewDraftSupportByDefault(t *testing.T) {
	assert := assert.New(t)
	provider := NewProvider(
		platform.KindForgejo,
		"codeberg.org",
		&fakeTransport{},
		WithReadActions(),
		WithMutations(),
	)

	caps := provider.Capabilities()

	assert.False(caps.ReviewDraftMutation)
	assert.False(caps.ReviewThreadResolution)
	assert.False(caps.ReadReviewThreads)
	assert.False(caps.NativeMultilineRanges)
}

func TestProviderMutationsNormalizeTransportResponses(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "codeberg.org", Owner: "forgejo", Name: "forgejo", RepoPath: "forgejo/forgejo"}
	transport := &fakeTransport{
		comment: CommentDTO{ID: 10, User: UserDTO{UserName: "alice"}, Body: "done", Created: base},
		pr: PullRequestDTO{
			ID: 11, Index: 7, Title: "edited", User: UserDTO{UserName: "bob"}, State: "open",
			Created: base, Updated: base,
		},
		issue: IssueDTO{ID: 12, Index: 8, Title: "issue", User: UserDTO{UserName: "carol"}, State: "closed", Created: base, Updated: base},
		merge: MergeResultDTO{Merged: true, SHA: "abc", Message: "merged"},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

	mrComment, err := provider.CreateMergeRequestComment(context.Background(), ref, 7, "done")
	require.NoError(err)
	issueComment, err := provider.CreateIssueComment(context.Background(), ref, 8, "done")
	require.NoError(err)
	editedComment, err := provider.EditIssueComment(context.Background(), ref, 8, 10, "edited")
	require.NoError(err)
	require.NoError(provider.DeleteMergeRequestComment(context.Background(), ref, 7, 10))
	require.NoError(provider.DeleteIssueComment(context.Background(), ref, 8, 10))
	issue, err := provider.CreateIssue(context.Background(), ref, "issue", "body")
	require.NoError(err)
	closedPR, err := provider.SetMergeRequestState(context.Background(), ref, 7, "closed")
	require.NoError(err)
	closedIssue, err := provider.SetIssueState(context.Background(), ref, 8, "closed")
	require.NoError(err)
	merged, err := provider.MergeMergeRequest(context.Background(), ref, 7, "title", "body", "squash", "")
	require.NoError(err)
	prTitle := "new title"
	prBody := "new body"
	editedPR, err := provider.EditMergeRequestContent(context.Background(), ref, 7, &prTitle, &prBody)
	require.NoError(err)

	assert.Equal("done", mrComment.Body)
	assert.Equal(7, mrComment.MergeRequestNumber)
	assert.Equal("done", issueComment.Body)
	assert.Equal(8, issueComment.IssueNumber)
	assert.Equal("edited", editedComment.Body)
	assert.Equal(8, editedComment.IssueNumber)
	assert.Equal("issue", issue.Title)
	assert.Equal("edited", closedPR.Title)
	assert.Equal("closed", closedIssue.State)
	assert.True(merged.Merged)
	assert.Equal("abc", merged.SHA)
	assert.Equal("edited", editedPR.Title)
	assert.Equal([]string{
		"create_pr_comment", "create_issue_comment", "edit_issue_comment", "delete_comment", "delete_comment", "create_issue",
		"edit_pull:closed", "edit_issue:closed", "merge:squash", "edit_pull:",
	}, transport.mutationCalls)
}

func TestProviderPaginatesAndNormalizesReadMethods(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindForgejo,
		Host:     "codeberg.org",
		Owner:    "forgejo",
		Name:     "forgejo",
		RepoPath: "forgejo/forgejo",
	}
	transport := &fakeTransport{
		repo: RepositoryDTO{
			ID:            1,
			Owner:         UserDTO{UserName: "forgejo"},
			Name:          "forgejo",
			FullName:      "forgejo/forgejo",
			DefaultBranch: "main",
			Created:       base,
			Updated:       base,
		},
		userRepos: [][]RepositoryDTO{
			{{ID: 2, Owner: UserDTO{UserName: "forgejo"}, Name: "one", FullName: "forgejo/one"}},
			{{ID: 3, Owner: UserDTO{UserName: "forgejo"}, Name: "two", FullName: "forgejo/two"}},
		},
		pulls: [][]PullRequestDTO{
			{{ID: 4, Index: 1, Title: "one", User: UserDTO{UserName: "alice"}, State: "open", Created: base, Updated: base}},
			{{ID: 5, Index: 2, Title: "two", User: UserDTO{UserName: "bob"}, State: "open", Created: base, Updated: base}},
		},
		issues: [][]IssueDTO{
			{
				{ID: 6, Index: 3, Title: "issue", User: UserDTO{UserName: "carol"}, State: "open", Created: base, Updated: base},
				{ID: 7, Index: 4, Title: "pull duplicate", User: UserDTO{UserName: "dan"}, State: "open", Created: base, Updated: base, IsPullRequest: true},
			},
		},
		releases: [][]ReleaseDTO{{{ID: 8, TagName: "v1", Title: "one", CreatedAt: base}}},
		tags:     [][]TagDTO{{{Name: "v1", Commit: CommitDTO{SHA: "abc"}}}},
		statuses: [][]StatusDTO{{{ID: 9, Context: "ci", State: "success", Created: base}}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport)

	repo, err := provider.GetRepository(context.Background(), ref)
	require.NoError(err)
	assert.Equal("forgejo/forgejo", repo.Ref.RepoPath)

	repos, err := provider.ListRepositories(context.Background(), "forgejo", platform.RepositoryListOptions{})
	require.NoError(err)
	assert.Equal([]string{"one", "two"}, []string{repos[0].Ref.Name, repos[1].Ref.Name})
	assert.Equal([]int{1, 2}, transport.userRepoPages)

	mrs, err := provider.ListOpenMergeRequests(context.Background(), ref)
	require.NoError(err)
	assert.Equal([]int{1, 2}, []int{mrs[0].Number, mrs[1].Number})
	assert.Equal([]int{1, 2}, transport.pullPages)

	issues, err := provider.ListOpenIssues(context.Background(), ref)
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal(3, issues[0].Number)

	releases, err := provider.ListReleases(context.Background(), ref)
	require.NoError(err)
	require.Len(releases, 1)
	assert.Equal("v1", releases[0].TagName)

	tags, err := provider.ListTags(context.Background(), ref)
	require.NoError(err)
	require.Len(tags, 1)
	assert.Equal("abc", tags[0].SHA)

	checks, err := provider.ListCIChecks(context.Background(), ref, "abc")
	require.NoError(err)
	require.Len(checks, 1)
	assert.Equal("success", checks[0].Conclusion)
}

func TestProviderMergesActionRunsWithStatusesWithoutDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	ref := platform.RepoRef{
		Platform: platform.KindForgejo,
		Host:     "codeberg.org",
		Owner:    "forgejo",
		Name:     "forgejo",
		RepoPath: "forgejo/forgejo",
	}
	transport := &fakeTransport{
		statuses: [][]StatusDTO{{
			{ID: 9, Context: "Build", State: "success", TargetURL: "https://ci.test/build", Created: base, Updated: base},
		}},
		actionRuns: [][]ActionRunDTO{{
			{
				ID:         10,
				Title:      "Build",
				Status:     "success",
				CommitSHA:  "abc",
				HTMLURL:    "https://ci.test/build",
				Started:    &base,
				Stopped:    &base,
				WorkflowID: "build.yml",
			},
			{
				ID:         12,
				Title:      "Build",
				Status:     "completed",
				Conclusion: "cancelled",
				CommitSHA:  "abc",
				HTMLURL:    "https://ci.test/actions/build",
				Started:    &base,
				Stopped:    &base,
				WorkflowID: "build-action.yml",
			},
			{
				ID:         11,
				Title:      "Deploy",
				Status:     "completed",
				Conclusion: "failure",
				CommitSHA:  "abc",
				HTMLURL:    "https://ci.test/deploy",
				Started:    &base,
				Stopped:    &base,
				WorkflowID: "deploy.yml",
			},
		}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithReadActions())

	checks, err := provider.ListCIChecks(context.Background(), ref, "abc")
	require.NoError(err)

	require.Len(checks, 3)
	assert.Equal("Build", checks[0].Name)
	assert.Equal("status", checks[0].App)
	assert.Equal("success", checks[0].Conclusion)
	assert.Equal("Build", checks[1].Name)
	assert.Equal("action", checks[1].App)
	assert.Equal("completed", checks[1].Status)
	assert.Equal("failure", checks[1].Conclusion)
	assert.Equal("Deploy", checks[2].Name)
	assert.Equal("action", checks[2].App)
	assert.Equal("completed", checks[2].Status)
	assert.Equal("failure", checks[2].Conclusion)
	assert.Equal([]int{1}, transport.actionPages)
}

func TestProviderFallsBackFromUserToOrgRepositoryImport(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	transport := &fakeTransport{
		userRepoErr: &HTTPError{StatusCode: 404, Message: "user missing"},
		orgRepos: [][]RepositoryDTO{
			{{ID: 10, Owner: UserDTO{UserName: "org"}, Name: "repo", FullName: "org/repo"}},
		},
	}
	provider := NewProvider(platform.KindGitea, "gitea.com", transport)

	repos, err := provider.ListRepositories(context.Background(), "org", platform.RepositoryListOptions{})
	require.NoError(err)

	require.Len(repos, 1)
	assert.Equal("org/repo", repos[0].Ref.RepoPath)
	assert.Equal([]int{1}, transport.userRepoPages)
	assert.Equal([]int{1}, transport.orgRepoPages)
}

func TestProviderMapsHTTPStatusErrorsToTypedPlatformErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		target     error
		code       platform.PlatformErrorCode
	}{
		{"unauthorized", 401, platform.ErrPermissionDenied, platform.ErrCodePermissionDenied},
		{"forbidden", 403, platform.ErrPermissionDenied, platform.ErrCodePermissionDenied},
		{"not found", 404, platform.ErrNotFound, platform.ErrCodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{
				repoErr: &HTTPError{StatusCode: tt.statusCode, Message: "failed"},
			})

			_, err := provider.GetRepository(context.Background(), platform.RepoRef{
				Owner: "forgejo",
				Name:  "forgejo",
			})

			require.Error(err)
			require.ErrorIs(err, tt.target)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(tt.code, platformErr.Code)
			assert.Equal(platform.KindForgejo, platformErr.Provider)
			assert.Equal("codeberg.org", platformErr.PlatformHost)
		})
	}
}

type fakeTransport struct {
	repo        RepositoryDTO
	repoErr     error
	userRepos   [][]RepositoryDTO
	userRepoErr error
	orgRepos    [][]RepositoryDTO
	orgRepoErr  error
	pulls       [][]PullRequestDTO
	issues      [][]IssueDTO
	releases    [][]ReleaseDTO
	tags        [][]TagDTO
	statuses    [][]StatusDTO
	actionRuns  [][]ActionRunDTO
	mergeOpts   MergeOptions
	mergeErr    error
	comment     CommentDTO
	pr          PullRequestDTO
	prSequence  []PullRequestDTO
	prErrSeq    []error
	issue       IssueDTO
	merge       MergeResultDTO
	review      ReviewDTO
	reviewErr   error

	userRepoPages []int
	orgRepoPages  []int
	pullPages     []int
	actionPages   []int
	mutationCalls []string
}

func (t *fakeTransport) GetRepository(context.Context, string, string) (RepositoryDTO, error) {
	return t.repo, t.repoErr
}

func (t *fakeTransport) ListUserRepositories(
	_ context.Context,
	_ string,
	opts PageOptions,
) ([]RepositoryDTO, Page, error) {
	t.userRepoPages = append(t.userRepoPages, opts.Page)
	if t.userRepoErr != nil {
		return nil, Page{}, t.userRepoErr
	}
	return pageFor(t.userRepos, opts.Page)
}

func (t *fakeTransport) ListOrgRepositories(
	_ context.Context,
	_ string,
	opts PageOptions,
) ([]RepositoryDTO, Page, error) {
	t.orgRepoPages = append(t.orgRepoPages, opts.Page)
	if t.orgRepoErr != nil {
		return nil, Page{}, t.orgRepoErr
	}
	return pageFor(t.orgRepos, opts.Page)
}

func (t *fakeTransport) ListOpenPullRequests(
	_ context.Context,
	_ platform.RepoRef,
	opts PageOptions,
) ([]PullRequestDTO, Page, error) {
	t.pullPages = append(t.pullPages, opts.Page)
	return pageFor(t.pulls, opts.Page)
}

func (t *fakeTransport) GetPullRequest(context.Context, platform.RepoRef, int) (PullRequestDTO, error) {
	if len(t.prErrSeq) > 0 {
		err := t.prErrSeq[0]
		t.prErrSeq = t.prErrSeq[1:]
		if err != nil {
			return PullRequestDTO{}, err
		}
	}
	if len(t.prSequence) > 0 {
		pr := t.prSequence[0]
		t.prSequence = t.prSequence[1:]
		return pr, nil
	}
	return t.pr, nil
}

func (t *fakeTransport) ListPullRequestComments(context.Context, platform.RepoRef, int, PageOptions) ([]CommentDTO, Page, error) {
	return nil, Page{}, nil
}

func (t *fakeTransport) ListPullRequestReviews(context.Context, platform.RepoRef, int, PageOptions) ([]ReviewDTO, Page, error) {
	return nil, Page{}, nil
}

func (t *fakeTransport) ListPullRequestCommits(context.Context, platform.RepoRef, int, PageOptions) ([]CommitDTO, Page, error) {
	return nil, Page{}, nil
}

func (t *fakeTransport) ListOpenIssues(_ context.Context, _ platform.RepoRef, opts PageOptions) ([]IssueDTO, Page, error) {
	return pageFor(t.issues, opts.Page)
}

func (t *fakeTransport) GetIssue(context.Context, platform.RepoRef, int) (IssueDTO, error) {
	return IssueDTO{}, nil
}

func (t *fakeTransport) ListIssueComments(context.Context, platform.RepoRef, int, PageOptions) ([]CommentDTO, Page, error) {
	return nil, Page{}, nil
}

func (t *fakeTransport) ListReleases(_ context.Context, _ platform.RepoRef, opts PageOptions) ([]ReleaseDTO, Page, error) {
	return pageFor(t.releases, opts.Page)
}

func (t *fakeTransport) ListTags(_ context.Context, _ platform.RepoRef, opts PageOptions) ([]TagDTO, Page, error) {
	return pageFor(t.tags, opts.Page)
}

func (t *fakeTransport) ListStatuses(_ context.Context, _ platform.RepoRef, _ string, opts PageOptions) ([]StatusDTO, Page, error) {
	return pageFor(t.statuses, opts.Page)
}

func (t *fakeTransport) ListActionRuns(_ context.Context, _ platform.RepoRef, _ string, opts PageOptions) ([]ActionRunDTO, Page, error) {
	t.actionPages = append(t.actionPages, opts.Page)
	return pageFor(t.actionRuns, opts.Page)
}

func (t *fakeTransport) CreateIssueComment(_ context.Context, _ platform.RepoRef, number int, _ string) (CommentDTO, error) {
	if number == 7 {
		t.mutationCalls = append(t.mutationCalls, "create_pr_comment")
	} else {
		t.mutationCalls = append(t.mutationCalls, "create_issue_comment")
	}
	return t.comment, nil
}

func (t *fakeTransport) EditIssueComment(_ context.Context, _ platform.RepoRef, _ int64, body string) (CommentDTO, error) {
	t.mutationCalls = append(t.mutationCalls, "edit_issue_comment")
	comment := t.comment
	comment.Body = body
	return comment, nil
}

func (t *fakeTransport) DeleteIssueComment(context.Context, platform.RepoRef, int64) error {
	t.mutationCalls = append(t.mutationCalls, "delete_comment")
	return nil
}

func (t *fakeTransport) CreateIssue(context.Context, platform.RepoRef, string, string) (IssueDTO, error) {
	t.mutationCalls = append(t.mutationCalls, "create_issue")
	return t.issue, nil
}

func (t *fakeTransport) EditIssue(_ context.Context, _ platform.RepoRef, _ int, opts IssueMutationOptions) (IssueDTO, error) {
	state := ""
	if opts.State != nil {
		state = *opts.State
	}
	t.mutationCalls = append(t.mutationCalls, "edit_issue:"+state)
	return t.issue, nil
}

func (t *fakeTransport) EditPullRequest(_ context.Context, _ platform.RepoRef, _ int, opts PullRequestMutationOptions) (PullRequestDTO, error) {
	state := ""
	if opts.State != nil {
		state = *opts.State
	}
	t.mutationCalls = append(t.mutationCalls, "edit_pull:"+state)
	return t.pr, nil
}

func (t *fakeTransport) MergePullRequest(_ context.Context, _ platform.RepoRef, _ int, opts MergeOptions) (MergeResultDTO, error) {
	t.mutationCalls = append(t.mutationCalls, "merge:"+opts.Method)
	t.mergeOpts = opts
	if t.mergeErr != nil {
		return MergeResultDTO{}, t.mergeErr
	}
	return t.merge, nil
}

func (t *fakeTransport) CreatePullReview(_ context.Context, _ platform.RepoRef, _ int, opts ReviewOptions) (ReviewDTO, error) {
	t.mutationCalls = append(t.mutationCalls, "review:"+opts.State+":"+opts.CommitID)
	return t.review, t.reviewErr
}

func pageFor[T any](pages [][]T, page int) ([]T, Page, error) {
	if page < 1 || page > len(pages) {
		return nil, Page{}, nil
	}
	next := 0
	if page < len(pages) {
		next = page + 1
	}
	return pages[page-1], Page{Next: next}, nil
}

func TestProviderMergePinsExpectedHeadAndClassifiesConflict(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	ref := platform.RepoRef{Owner: "acme", Name: "widget"}

	transport := &fakeTransport{merge: MergeResultDTO{Merged: true}}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	_, err := provider.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "squash", "reviewed-head")
	require.NoError(err)
	assert.Equal("reviewed-head", transport.mergeOpts.ExpectedHeadSHA,
		"merge must send the reviewed head as head_commit_id")

	// Current Gitea/Forgejo reject with "head out of date"; older Gitea
	// releases said "head target does not match". Both must classify.
	for _, message := range []string{"head out of date", "Head target does not match. Please try again."} {
		transport.mergeErr = &HTTPError{StatusCode: 409, Message: message}
		_, err = provider.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "squash", "reviewed-head")
		var platformErr *platform.Error
		require.ErrorAs(err, &platformErr)
		assert.Equal(platform.ErrCodeStaleState, platformErr.Code, message)
	}

	// Gitea 1.24.6 returns only "Please try again later" for this rejection.
	// Confirm the current head before classifying that ambiguous response.
	transport.mergeErr = &HTTPError{StatusCode: 405, Message: "Please try again later"}
	transport.pr.Head.SHA = "current-head"
	_, err = provider.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "squash", "reviewed-head")
	require.ErrorIs(err, platform.ErrStaleState)

	transport.pr.Head.SHA = "reviewed-head"
	_, err = provider.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "squash", "reviewed-head")
	require.NotErrorIs(err, platform.ErrStaleState)

	// The merge endpoint answers 409 for ordinary merge conflicts too;
	// those must stay generic so the UI shows the provider message
	// instead of a stale-head re-review flow.
	transport.mergeErr = &HTTPError{StatusCode: 409, Message: "merge conflict detected"}
	_, err = provider.MergeMergeRequest(context.Background(), ref, 7, "t", "m", "squash", "reviewed-head")
	require.NotErrorIs(err, platform.ErrStaleState,
		"a non-head-mismatch 409 must not classify as stale_state")
	var conflictErr *HTTPError
	require.ErrorAs(err, &conflictErr)
	assert.Equal(409, conflictErr.StatusCode)
}

func TestProviderApproveSubmitsReview(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	ref := platform.RepoRef{Owner: "acme", Name: "widget"}

	transport := &fakeTransport{
		review: ReviewDTO{
			ID: 22, User: UserDTO{UserName: "dana"}, State: "APPROVED",
			Body: "ship it", Submitted: time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC),
		},
	}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	event, err := provider.ApproveMergeRequest(context.Background(), ref, 7, "ship it", "reviewed-head")
	require.NoError(err)
	assert.Equal("review", event.EventType)
	assert.Equal("APPROVED", event.Summary)
	assert.Equal("review:APPROVED:reviewed-head", transport.mutationCalls[len(transport.mutationCalls)-1])
}

func TestProviderRequestChangesSubmitsReviewAndMapsErrors(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	ref := platform.RepoRef{Owner: "acme", Name: "widget"}
	transport := &fakeTransport{}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	err := provider.RequestChanges(context.Background(), ref, 7, "needs work", "reviewed-head")
	require.NoError(err)
	assert.Equal("review:REQUEST_CHANGES:reviewed-head", transport.mutationCalls[len(transport.mutationCalls)-1])

	transport.reviewErr = &HTTPError{StatusCode: 403, Message: "forbidden"}
	err = provider.RequestChanges(context.Background(), ref, 7, "needs work", "reviewed-head")
	require.Error(err)
	assert.ErrorIs(err, platform.ErrPermissionDenied)
}
