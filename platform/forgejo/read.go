package forgejo

import (
	"context"
	"errors"
	"net/http"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
)

func (t *transport) GetRepository(
	ctx context.Context,
	owner, repo string,
) (gitealike.RepositoryDTO, error) {
	var repository *forgejosdk.Repository
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repository, resp, err = t.api.GetRepo(owner, repo)
		return err
	})
	if err != nil {
		return gitealike.RepositoryDTO{}, forgejoHTTPError(resp, err)
	}
	return convertRepository(repository)
}

func (t *transport) GetAuthenticatedUser(
	ctx context.Context,
) (gitealike.UserDTO, error) {
	var user *forgejosdk.User
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		user, resp, err = t.api.GetMyUserInfo()
		return err
	})
	if err != nil {
		return gitealike.UserDTO{}, forgejoHTTPError(resp, err)
	}
	return convertUser(user), nil
}

func (t *transport) ListUserRepositories(
	ctx context.Context,
	owner string,
	opts gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	var repos []*forgejosdk.Repository
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repos, resp, err = t.api.ListUserRepos(owner, forgejosdk.ListReposOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertRepositories(repos, forgejoPage(resp))
}

func (t *transport) ListOrgRepositories(
	ctx context.Context,
	owner string,
	opts gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	var repos []*forgejosdk.Repository
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repos, resp, err = t.api.ListOrgRepos(owner, forgejosdk.ListOrgReposOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertRepositories(repos, forgejoPage(resp))
}

func (t *transport) ListOpenPullRequests(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.PullRequestDTO, gitealike.Page, error) {
	var prs []*forgejosdk.PullRequest
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		prs, resp, err = t.api.ListRepoPullRequests(ref.Owner, ref.Name, forgejosdk.ListPullRequestsOptions{
			ListOptions: forgejoListOptions(opts),
			State:       forgejosdk.StateOpen,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertPullRequests(
		prs, t.mergeableForPullRequest, t.metricsForPullRequest,
	), forgejoPage(resp), nil
}

func (t *transport) GetPullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (gitealike.PullRequestDTO, error) {
	var pr *forgejosdk.PullRequest
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		pr, resp, err = t.api.GetPullRequest(ref.Owner, ref.Name, int64(number))
		return err
	})
	if err != nil {
		return gitealike.PullRequestDTO{}, forgejoHTTPError(resp, err)
	}
	return convertPullRequest(
		pr, t.mergeableForPullRequest(pr), t.metricsForPullRequest(pr),
	), nil
}

func (t *transport) mergeableForPullRequest(pr *forgejosdk.PullRequest) *bool {
	if pr == nil {
		return nil
	}
	mergeable, _ := t.mergeability.MergeableForPullRequest(
		pr.HTMLURL,
		prBranchSHA(pr.Head),
		prBranchSHA(pr.Base),
	)
	return mergeable
}

func (t *transport) metricsForPullRequest(
	pr *forgejosdk.PullRequest,
) gitealike.PullRequestMetrics {
	if pr == nil {
		return gitealike.PullRequestMetrics{}
	}
	metrics, _ := t.mergeability.MetricsForPullRequest(
		pr.HTMLURL,
		prBranchSHA(pr.Head),
		prBranchSHA(pr.Base),
	)
	return metrics
}

func prBranchSHA(branch *forgejosdk.PRBranchInfo) string {
	if branch == nil {
		return ""
	}
	return branch.Sha
}

func (t *transport) ListPullRequestComments(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.CommentDTO, gitealike.Page, error) {
	var comments []*forgejosdk.Comment
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		comments, resp, err = t.api.ListIssueComments(ref.Owner, ref.Name, int64(number), forgejosdk.ListIssueCommentOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertComments(comments), forgejoPage(resp), nil
}

func (t *transport) ListPullRequestReviews(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.ReviewDTO, gitealike.Page, error) {
	var reviews []*forgejosdk.PullReview
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		reviews, resp, err = t.api.ListPullReviews(ref.Owner, ref.Name, int64(number), forgejosdk.ListPullReviewsOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertReviews(reviews), forgejoPage(resp), nil
}

func (t *transport) ListPullRequestCommits(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.CommitDTO, gitealike.Page, error) {
	var commits []*forgejosdk.Commit
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		commits, resp, err = t.api.ListPullRequestCommits(ref.Owner, ref.Name, int64(number), forgejosdk.ListPullRequestCommitsOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertCommits(commits), forgejoPage(resp), nil
}

func (t *transport) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.IssueDTO, gitealike.Page, error) {
	var issues []*forgejosdk.Issue
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issues, resp, err = t.api.ListRepoIssues(ref.Owner, ref.Name, forgejosdk.ListIssueOption{
			ListOptions: forgejoListOptions(opts),
			State:       forgejosdk.StateOpen,
			Type:        forgejosdk.IssueTypeIssue,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertIssues(issues), forgejoPage(resp), nil
}

func (t *transport) ListIssuesPage(ctx context.Context, ref platform.RepoRef, opts gitealike.ArchiveListOptions) ([]gitealike.IssueDTO, gitealike.Page, error) {
	var issues []*forgejosdk.Issue
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issues, resp, err = t.api.ListRepoIssues(ref.Owner, ref.Name, forgejosdk.ListIssueOption{
			ListOptions: forgejoListOptions(opts.PageOptions), State: forgejosdk.StateAll,
			Type: forgejosdk.IssueTypeIssue, Since: opts.Since, Before: opts.Before,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertIssues(issues), forgejoPage(resp), nil
}

func (t *transport) ListPullRequestsPage(ctx context.Context, ref platform.RepoRef, opts gitealike.ArchiveListOptions) ([]gitealike.PullRequestDTO, gitealike.Page, error) {
	var prs []*forgejosdk.PullRequest
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		prs, resp, err = t.api.ListRepoPullRequests(ref.Owner, ref.Name, forgejosdk.ListPullRequestsOptions{
			ListOptions: forgejoListOptions(opts.PageOptions), State: forgejosdk.StateAll, Sort: opts.Sort,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertPullRequests(
		prs, t.mergeableForPullRequest, t.metricsForPullRequest,
	), forgejoPage(resp), nil
}

func (t *transport) GetIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (gitealike.IssueDTO, error) {
	var issue *forgejosdk.Issue
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issue, resp, err = t.api.GetIssue(ref.Owner, ref.Name, int64(number))
		return err
	})
	if err != nil {
		return gitealike.IssueDTO{}, forgejoHTTPError(resp, err)
	}
	return convertIssue(issue), nil
}

func (t *transport) ListIssueComments(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.CommentDTO, gitealike.Page, error) {
	return t.ListPullRequestComments(ctx, ref, number, opts)
}

func (t *transport) ListIssueTimeline(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.TimelineEventDTO, gitealike.Page, error) {
	return gitealike.ReadIssueTimelinePage(ctx, t.httpClient, t.baseURL, ref, number, opts)
}

func (t *transport) ListReleases(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.ReleaseDTO, gitealike.Page, error) {
	var releases []*forgejosdk.Release
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		releases, resp, err = t.api.ListReleases(ref.Owner, ref.Name, forgejosdk.ListReleasesOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertReleases(releases), forgejoPage(resp), nil
}

func (t *transport) ListTags(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.TagDTO, gitealike.Page, error) {
	var tags []*forgejosdk.Tag
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		tags, resp, err = t.api.ListRepoTags(ref.Owner, ref.Name, forgejosdk.ListRepoTagsOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertTags(tags), forgejoPage(resp), nil
}

func (t *transport) ListRepoLabels(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.LabelDTO, gitealike.Page, error) {
	var labels []*forgejosdk.Label
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		labels, resp, err = t.api.ListRepoLabels(ref.Owner, ref.Name, forgejosdk.ListLabelsOptions{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertLabels(labels), forgejoPage(resp), nil
}

func (t *transport) ListStatuses(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
	opts gitealike.PageOptions,
) ([]gitealike.StatusDTO, gitealike.Page, error) {
	var statuses []*forgejosdk.Status
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		statuses, resp, err = t.api.ListStatuses(ref.Owner, ref.Name, sha, forgejosdk.ListStatusesOption{
			ListOptions: forgejoListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, forgejoHTTPError(resp, err)
	}
	return convertStatuses(statuses), forgejoPage(resp), nil
}

func (t *transport) ListActionRuns(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
	opts gitealike.PageOptions,
) ([]gitealike.ActionRunDTO, gitealike.Page, error) {
	var runs *forgejosdk.ListActionRunsResponse
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		runs, resp, err = t.api.ListRepoActionRuns(ref.Owner, ref.Name, forgejosdk.ListActionRunsOption{
			ListOptions: forgejoListOptions(opts),
			HeadSHA:     sha,
		})
		return err
	})
	if err != nil {
		mappedErr := forgejoHTTPError(resp, err)
		if actionRunsUnavailable(mappedErr) {
			return nil, forgejoPage(resp), nil
		}
		return nil, gitealike.Page{}, mappedErr
	}
	if runs == nil {
		return nil, forgejoPage(resp), nil
	}
	return convertActionRuns(runs.WorkflowRuns), forgejoPage(resp), nil
}

func actionRunsUnavailable(err error) bool {
	var httpErr *gitealike.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

func forgejoListOptions(opts gitealike.PageOptions) forgejosdk.ListOptions {
	return forgejosdk.ListOptions{Page: opts.Page, PageSize: opts.PageSize}
}

func forgejoPage(resp *forgejosdk.Response) gitealike.Page {
	if resp == nil {
		return gitealike.Page{}
	}
	return gitealike.Page{Next: resp.NextPage, Last: resp.LastPage}
}

func forgejoHTTPError(resp *forgejosdk.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.Response != nil {
		return &gitealike.HTTPError{StatusCode: resp.StatusCode, Err: err}
	}
	return err
}
