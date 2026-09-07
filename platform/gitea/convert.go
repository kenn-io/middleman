package gitea

import (
	"fmt"
	"net/url"
	"time"

	giteasdk "code.gitea.io/sdk/gitea"
	"go.kenn.io/forge/platform/gitealike"
)

func convertRepository(repo *giteasdk.Repository) (gitealike.RepositoryDTO, error) {
	if repo == nil {
		return gitealike.RepositoryDTO{}, fmt.Errorf("gitea repository is nil")
	}
	var canPush, canAdmin *bool
	if repo.Permissions != nil {
		canPush = &repo.Permissions.Push
		canAdmin = &repo.Permissions.Admin
	}
	issuesEnabled := repo.HasIssues
	mergeRequestsEnabled := repo.HasPullRequests
	return gitealike.RepositoryDTO{
		ID:                   repo.ID,
		Owner:                convertUser(repo.Owner),
		Name:                 repo.Name,
		FullName:             repo.FullName,
		HTMLURL:              repo.HTMLURL,
		CloneURL:             repo.CloneURL,
		DefaultBranch:        repo.DefaultBranch,
		Private:              repo.Private,
		Archived:             repo.Archived,
		Description:          repo.Description,
		AllowSquash:          repo.AllowSquash,
		AllowMerge:           repo.AllowMerge,
		AllowRebase:          repo.AllowRebase || repo.AllowRebaseMerge,
		CanPush:              canPush,
		CanAdmin:             canAdmin,
		IssuesEnabled:        &issuesEnabled,
		MergeRequestsEnabled: &mergeRequestsEnabled,
		Created:              repo.Created,
		Updated:              repo.Updated,
	}, nil
}

func convertPullRequest(pr *giteasdk.PullRequest, mergeable *bool) gitealike.PullRequestDTO {
	if pr == nil {
		return gitealike.PullRequestDTO{}
	}
	return gitealike.PullRequestDTO{
		ID:             pr.ID,
		Index:          int(pr.Index),
		HTMLURL:        pr.HTMLURL,
		Title:          pr.Title,
		User:           convertUser(pr.Poster),
		State:          string(pr.State),
		Draft:          pr.Draft,
		IsLocked:       pr.IsLocked,
		Body:           pr.Body,
		Head:           convertBranch(pr.Head),
		Base:           convertBranch(pr.Base),
		Labels:         convertLabels(pr.Labels),
		Comments:       pr.Comments,
		Mergeable:      mergeable,
		Additions:      intValue(pr.Additions),
		AdditionsKnown: pr.Additions != nil,
		Deletions:      intValue(pr.Deletions),
		DeletionsKnown: pr.Deletions != nil,
		FilesChanged:   cloneInt(pr.ChangedFiles),
		Created:        timeValue(pr.Created),
		Updated:        timeValue(pr.Updated),
		Merged:         pr.HasMerged,
		MergedAt:       timePtrValue(pr.Merged),
		MergedBy:       convertUser(pr.MergedBy),
		Closed:         timePtrValue(pr.Closed),
		// convertUsers never returns nil, so both fields read as
		// provider-confirmed sets: the Gitea API always serializes
		// assignees and requested reviewers on pull requests.
		Assignees:          convertUsers(pr.Assignees),
		RequestedReviewers: convertUsers(pr.RequestedReviewers),
	}
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func convertIssue(issue *giteasdk.Issue) gitealike.IssueDTO {
	if issue == nil {
		return gitealike.IssueDTO{}
	}
	return gitealike.IssueDTO{
		ID:            issue.ID,
		Index:         int(issue.Index),
		HTMLURL:       issue.HTMLURL,
		Title:         issue.Title,
		User:          convertUser(issue.Poster),
		State:         string(issue.State),
		Body:          issue.Body,
		Comments:      issue.Comments,
		Labels:        convertLabels(issue.Labels),
		Assignees:     convertUsers(issue.Assignees),
		Created:       issue.Created,
		Updated:       issue.Updated,
		Closed:        timePtrValue(issue.Closed),
		IsPullRequest: issue.PullRequest != nil,
	}
}

func convertUsers(users []*giteasdk.User) []gitealike.UserDTO {
	out := make([]gitealike.UserDTO, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		out = append(out, convertUser(u))
	}
	return out
}

func convertComment(comment *giteasdk.Comment) gitealike.CommentDTO {
	if comment == nil {
		return gitealike.CommentDTO{}
	}
	return gitealike.CommentDTO{
		ID:      comment.ID,
		HTMLURL: comment.HTMLURL,
		User:    convertUser(comment.Poster),
		Body:    comment.Body,
		Created: comment.Created,
		Updated: comment.Updated,
	}
}

func convertReview(review *giteasdk.PullReview) gitealike.ReviewDTO {
	if review == nil {
		return gitealike.ReviewDTO{}
	}
	return gitealike.ReviewDTO{
		ID:        review.ID,
		User:      convertUser(review.Reviewer),
		State:     string(review.State),
		Body:      review.Body,
		Submitted: review.Submitted,
	}
}

func convertRelease(release *giteasdk.Release) gitealike.ReleaseDTO {
	if release == nil {
		return gitealike.ReleaseDTO{}
	}
	return gitealike.ReleaseDTO{
		ID:          release.ID,
		TagName:     release.TagName,
		Title:       release.Title,
		HTMLURL:     release.HTMLURL,
		Target:      release.Target,
		Prerelease:  release.IsPrerelease,
		PublishedAt: nonZeroTimePtr(release.PublishedAt),
		CreatedAt:   release.CreatedAt,
	}
}

func convertTag(tag *giteasdk.Tag) gitealike.TagDTO {
	if tag == nil {
		return gitealike.TagDTO{}
	}
	return gitealike.TagDTO{
		Name:   tag.Name,
		Commit: convertCommitMeta(tag.Commit),
	}
}

func convertStatus(status *giteasdk.Status) gitealike.StatusDTO {
	if status == nil {
		return gitealike.StatusDTO{}
	}
	return gitealike.StatusDTO{
		ID:          status.ID,
		Context:     status.Context,
		State:       string(status.State),
		TargetURL:   safeStatusTargetURL(status.TargetURL),
		Description: status.Description,
		Created:     status.Created,
		Updated:     status.Updated,
	}
}

func safeStatusTargetURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "http", "https":
		return rawURL
	default:
		return ""
	}
}

func convertActionRun(run *giteasdk.ActionWorkflowRun) gitealike.ActionRunDTO {
	if run == nil {
		return gitealike.ActionRunDTO{}
	}
	return gitealike.ActionRunDTO{
		ID:         run.ID,
		RunNumber:  run.RunNumber,
		WorkflowID: run.Path,
		Title:      run.DisplayTitle,
		Status:     run.Status,
		Conclusion: run.Conclusion,
		CommitSHA:  run.HeadSha,
		HTMLURL:    run.HTMLURL,
		Created:    run.StartedAt,
		Updated:    run.CompletedAt,
		Started:    nonZeroTimePtr(run.StartedAt),
		Stopped:    nonZeroTimePtr(run.CompletedAt),
	}
}

func convertCommit(commit *giteasdk.Commit) gitealike.CommitDTO {
	if commit == nil {
		return gitealike.CommitDTO{}
	}
	out := convertCommitMeta(commit.CommitMeta)
	out.URL = commit.HTMLURL
	if commit.RepoCommit != nil {
		out.Message = commit.RepoCommit.Message
		if commit.RepoCommit.Author != nil {
			out.AuthorName = commit.RepoCommit.Author.Name
		}
	}
	return out
}

func convertRepositories(
	repos []*giteasdk.Repository,
	page gitealike.Page,
) ([]gitealike.RepositoryDTO, gitealike.Page, error) {
	out := make([]gitealike.RepositoryDTO, 0, len(repos))
	for _, repo := range repos {
		item, err := convertRepository(repo)
		if err != nil {
			return nil, gitealike.Page{}, err
		}
		out = append(out, item)
	}
	return out, page, nil
}

func convertPullRequests(
	prs []*giteasdk.PullRequest,
	mergeableFor func(*giteasdk.PullRequest) *bool,
) []gitealike.PullRequestDTO {
	out := make([]gitealike.PullRequestDTO, 0, len(prs))
	for _, pr := range prs {
		var mergeable *bool
		if mergeableFor != nil {
			mergeable = mergeableFor(pr)
		}
		out = append(out, convertPullRequest(pr, mergeable))
	}
	return out
}

func convertIssues(issues []*giteasdk.Issue) []gitealike.IssueDTO {
	out := make([]gitealike.IssueDTO, 0, len(issues))
	for _, issue := range issues {
		out = append(out, convertIssue(issue))
	}
	return out
}

func convertComments(comments []*giteasdk.Comment) []gitealike.CommentDTO {
	out := make([]gitealike.CommentDTO, 0, len(comments))
	for _, comment := range comments {
		out = append(out, convertComment(comment))
	}
	return out
}

func convertReviews(reviews []*giteasdk.PullReview) []gitealike.ReviewDTO {
	out := make([]gitealike.ReviewDTO, 0, len(reviews))
	for _, review := range reviews {
		out = append(out, convertReview(review))
	}
	return out
}

func convertCommits(commits []*giteasdk.Commit) []gitealike.CommitDTO {
	out := make([]gitealike.CommitDTO, 0, len(commits))
	for _, commit := range commits {
		out = append(out, convertCommit(commit))
	}
	return out
}

func convertReleases(releases []*giteasdk.Release) []gitealike.ReleaseDTO {
	out := make([]gitealike.ReleaseDTO, 0, len(releases))
	for _, release := range releases {
		out = append(out, convertRelease(release))
	}
	return out
}

func convertTags(tags []*giteasdk.Tag) []gitealike.TagDTO {
	out := make([]gitealike.TagDTO, 0, len(tags))
	for _, tag := range tags {
		out = append(out, convertTag(tag))
	}
	return out
}

func convertStatuses(statuses []*giteasdk.Status) []gitealike.StatusDTO {
	out := make([]gitealike.StatusDTO, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, convertStatus(status))
	}
	return out
}

func convertActionRuns(runs []*giteasdk.ActionWorkflowRun) []gitealike.ActionRunDTO {
	out := make([]gitealike.ActionRunDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, convertActionRun(run))
	}
	return out
}

func convertUser(user *giteasdk.User) gitealike.UserDTO {
	if user == nil {
		return gitealike.UserDTO{}
	}
	return gitealike.UserDTO{
		ID:       user.ID,
		UserName: user.UserName,
		FullName: user.FullName,
	}
}

func convertLabels(labels []*giteasdk.Label) []gitealike.LabelDTO {
	if len(labels) == 0 {
		return nil
	}
	out := make([]gitealike.LabelDTO, 0, len(labels))
	for _, label := range labels {
		if label == nil {
			continue
		}
		out = append(out, gitealike.LabelDTO{
			ID:          label.ID,
			Name:        label.Name,
			Description: label.Description,
			Color:       label.Color,
		})
	}
	return out
}

func convertBranch(branch *giteasdk.PRBranchInfo) gitealike.BranchDTO {
	if branch == nil {
		return gitealike.BranchDTO{}
	}
	out := gitealike.BranchDTO{
		Ref: branch.Ref,
		SHA: branch.Sha,
	}
	if branch.Repository != nil {
		out.RepoCloneURL = branch.Repository.CloneURL
	}
	return out
}

func convertCommitMeta(commit *giteasdk.CommitMeta) gitealike.CommitDTO {
	if commit == nil {
		return gitealike.CommitDTO{}
	}
	return gitealike.CommitDTO{
		SHA:     commit.SHA,
		URL:     commit.URL,
		Created: commit.Created,
	}
}

func timeValue(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func timePtrValue(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

func nonZeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
