package gitea

import (
	"context"
	"strings"

	giteasdk "code.gitea.io/sdk/gitea"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
)

func (t *transport) GetRepository(
	ctx context.Context,
	owner, repo string,
) (gitealike.RepositoryDTO, error) {
	var repository *giteasdk.Repository
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repository, resp, err = t.api.GetRepo(owner, repo)
		return err
	})
	if err != nil {
		return gitealike.RepositoryDTO{}, giteaHTTPError(resp, err)
	}
	return convertRepository(repository)
}

func (t *transport) GetAuthenticatedUser(
	ctx context.Context,
) (gitealike.UserDTO, error) {
	var user *giteasdk.User
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		user, resp, err = t.api.GetMyUserInfo()
		return err
	})
	if err != nil {
		return gitealike.UserDTO{}, giteaHTTPError(resp, err)
	}
	return convertUser(user), nil
}

func (t *transport) ListUserRepositories(
	ctx context.Context,
	owner string,
	opts gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	var repos []*giteasdk.Repository
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repos, resp, err = t.api.ListUserRepos(owner, giteasdk.ListReposOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertRepositories(repos, giteaPage(resp))
}

func (t *transport) ListOrgRepositories(
	ctx context.Context,
	owner string,
	opts gitealike.PageOptions,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	var repos []*giteasdk.Repository
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		repos, resp, err = t.api.ListOrgRepos(owner, giteasdk.ListOrgReposOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertRepositories(repos, giteaPage(resp))
}

func (t *transport) ListOpenPullRequests(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.PullRequestDTO, gitealike.Page, error) {
	var prs []*giteasdk.PullRequest
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		prs, resp, err = t.api.ListRepoPullRequests(ref.Owner, ref.Name, giteasdk.ListPullRequestsOptions{
			ListOptions: giteaListOptions(opts),
			State:       giteasdk.StateOpen,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertPullRequests(prs, t.mergeableForPullRequest), giteaPage(resp), nil
}

func (t *transport) GetPullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (gitealike.PullRequestDTO, error) {
	var pr *giteasdk.PullRequest
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		pr, resp, err = t.api.GetPullRequest(ref.Owner, ref.Name, int64(number))
		return err
	})
	if err != nil {
		return gitealike.PullRequestDTO{}, giteaHTTPError(resp, err)
	}
	return convertPullRequest(pr, t.mergeableForPullRequest(pr)), nil
}

func (t *transport) mergeableForPullRequest(pr *giteasdk.PullRequest) *bool {
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

func prBranchSHA(branch *giteasdk.PRBranchInfo) string {
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
	var comments []*giteasdk.Comment
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		comments, resp, err = t.api.ListIssueComments(ref.Owner, ref.Name, int64(number), giteasdk.ListIssueCommentOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertComments(comments), giteaPage(resp), nil
}

func (t *transport) ListPullRequestReviews(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.ReviewDTO, gitealike.Page, error) {
	var reviews []*giteasdk.PullReview
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		reviews, resp, err = t.api.ListPullReviews(ref.Owner, ref.Name, int64(number), giteasdk.ListPullReviewsOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertReviews(reviews), giteaPage(resp), nil
}

func (t *transport) ListPullRequestCommits(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PageOptions,
) ([]gitealike.CommitDTO, gitealike.Page, error) {
	var commits []*giteasdk.Commit
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		commits, resp, err = t.api.ListPullRequestCommits(ref.Owner, ref.Name, int64(number), giteasdk.ListPullRequestCommitsOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertCommits(commits), giteaPage(resp), nil
}

func (t *transport) ListOpenIssues(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.IssueDTO, gitealike.Page, error) {
	var issues []*giteasdk.Issue
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issues, resp, err = t.api.ListRepoIssues(ref.Owner, ref.Name, giteasdk.ListIssueOption{
			ListOptions: giteaListOptions(opts),
			State:       giteasdk.StateOpen,
			Type:        giteasdk.IssueTypeIssue,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertIssues(issues), giteaPage(resp), nil
}

func (t *transport) ListIssuesPage(ctx context.Context, ref platform.RepoRef, opts gitealike.ArchiveListOptions) ([]gitealike.IssueDTO, gitealike.Page, error) {
	var issues []*giteasdk.Issue
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issues, resp, err = t.api.ListRepoIssues(ref.Owner, ref.Name, giteasdk.ListIssueOption{
			ListOptions: giteaListOptions(opts.PageOptions), State: giteasdk.StateAll,
			Type: giteasdk.IssueTypeIssue, Since: opts.Since, Before: opts.Before,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertIssues(issues), giteaPage(resp), nil
}

func (t *transport) ListPullRequestsPage(ctx context.Context, ref platform.RepoRef, opts gitealike.ArchiveListOptions) ([]gitealike.PullRequestDTO, gitealike.Page, error) {
	var prs []*giteasdk.PullRequest
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		prs, resp, err = t.api.ListRepoPullRequests(ref.Owner, ref.Name, giteasdk.ListPullRequestsOptions{
			ListOptions: giteaListOptions(opts.PageOptions), State: giteasdk.StateAll, Sort: opts.Sort,
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertPullRequests(prs, t.mergeableForPullRequest), giteaPage(resp), nil
}

func (t *transport) GetIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) (gitealike.IssueDTO, error) {
	var issue *giteasdk.Issue
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issue, resp, err = t.api.GetIssue(ref.Owner, ref.Name, int64(number))
		return err
	})
	if err != nil {
		return gitealike.IssueDTO{}, giteaHTTPError(resp, err)
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
	var releases []*giteasdk.Release
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		releases, resp, err = t.api.ListReleases(ref.Owner, ref.Name, giteasdk.ListReleasesOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertReleases(releases), giteaPage(resp), nil
}

func (t *transport) ListTags(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.TagDTO, gitealike.Page, error) {
	var tags []*giteasdk.Tag
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		tags, resp, err = t.api.ListRepoTags(ref.Owner, ref.Name, giteasdk.ListRepoTagsOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertTags(tags), giteaPage(resp), nil
}

func (t *transport) ListRepoLabels(
	ctx context.Context,
	ref platform.RepoRef,
	opts gitealike.PageOptions,
) ([]gitealike.LabelDTO, gitealike.Page, error) {
	var labels []*giteasdk.Label
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		labels, resp, err = t.api.ListRepoLabels(ref.Owner, ref.Name, giteasdk.ListLabelsOptions{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertLabels(labels), giteaPage(resp), nil
}

func (t *transport) ListStatuses(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
	opts gitealike.PageOptions,
) ([]gitealike.StatusDTO, gitealike.Page, error) {
	var statuses []*giteasdk.Status
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		statuses, resp, err = t.api.ListStatuses(ref.Owner, ref.Name, sha, giteasdk.ListStatusesOption{
			ListOptions: giteaListOptions(opts),
		})
		return err
	})
	if err != nil {
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	return convertStatuses(statuses), giteaPage(resp), nil
}

func (t *transport) ListActionRuns(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
	opts gitealike.PageOptions,
) ([]gitealike.ActionRunDTO, gitealike.Page, error) {
	var runs *giteasdk.ActionWorkflowRunsResponse
	var resp *giteasdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		runs, resp, err = t.api.ListRepoActionRuns(ref.Owner, ref.Name, giteasdk.ListRepoActionRunsOptions{
			ListOptions: giteaListOptions(opts),
			HeadSHA:     sha,
		})
		return err
	})
	if err != nil {
		if isActionsUnsupportedVersionError(err) {
			return nil, gitealike.Page{}, nil
		}
		return nil, gitealike.Page{}, giteaHTTPError(resp, err)
	}
	if runs == nil {
		return nil, giteaPage(resp), nil
	}
	return convertActionRuns(runs.WorkflowRuns), giteaPage(resp), nil
}

func isActionsUnsupportedVersionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is older than 1.26.0") ||
		strings.Contains(msg, "does not satisfy version constraint >= 1.26.0")
}

func giteaListOptions(opts gitealike.PageOptions) giteasdk.ListOptions {
	return giteasdk.ListOptions{Page: opts.Page, PageSize: opts.PageSize}
}

func giteaPage(resp *giteasdk.Response) gitealike.Page {
	if resp == nil {
		return gitealike.Page{}
	}
	return gitealike.Page{Next: resp.NextPage, Last: resp.LastPage}
}

func giteaHTTPError(resp *giteasdk.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.Response != nil {
		return &gitealike.HTTPError{StatusCode: resp.StatusCode, Err: err}
	}
	return err
}
