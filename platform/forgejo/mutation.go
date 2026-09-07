package forgejo

import (
	"context"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
)

func (t *transport) CreatePullReview(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.ReviewOptions,
) (gitealike.ReviewDTO, error) {
	var review *forgejosdk.PullReview
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		review, resp, err = t.api.CreatePullReview(ref.Owner, ref.Name, int64(number), forgejosdk.CreatePullReviewOptions{
			State:    forgejosdk.ReviewStateType(opts.State),
			Body:     opts.Body,
			CommitID: opts.CommitID,
		})
		return err
	})
	if err != nil {
		return gitealike.ReviewDTO{}, forgejoHTTPError(resp, err)
	}
	return convertReview(review), nil
}

func (t *transport) CreateIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (gitealike.CommentDTO, error) {
	var comment *forgejosdk.Comment
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		comment, resp, err = t.api.CreateIssueComment(ref.Owner, ref.Name, int64(number), forgejosdk.CreateIssueCommentOption{
			Body: body,
		})
		return err
	})
	if err != nil {
		return gitealike.CommentDTO{}, forgejoHTTPError(resp, err)
	}
	return convertComment(comment), nil
}

func (t *transport) EditIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	commentID int64,
	body string,
) (gitealike.CommentDTO, error) {
	var comment *forgejosdk.Comment
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		comment, resp, err = t.api.EditIssueComment(ref.Owner, ref.Name, commentID, forgejosdk.EditIssueCommentOption{
			Body: body,
		})
		return err
	})
	if err != nil {
		return gitealike.CommentDTO{}, forgejoHTTPError(resp, err)
	}
	return convertComment(comment), nil
}

func (t *transport) DeleteIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	commentID int64,
) error {
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		resp, err = t.api.DeleteIssueComment(ref.Owner, ref.Name, commentID)
		return err
	})
	if err != nil {
		return forgejoHTTPError(resp, err)
	}
	return nil
}

func (t *transport) CreateIssue(
	ctx context.Context,
	ref platform.RepoRef,
	title string,
	body string,
) (gitealike.IssueDTO, error) {
	var issue *forgejosdk.Issue
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issue, resp, err = t.api.CreateIssue(ref.Owner, ref.Name, forgejosdk.CreateIssueOption{
			Title: title,
			Body:  body,
		})
		return err
	})
	if err != nil {
		return gitealike.IssueDTO{}, forgejoHTTPError(resp, err)
	}
	return convertIssue(issue), nil
}

func (t *transport) EditIssue(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.IssueMutationOptions,
) (gitealike.IssueDTO, error) {
	var issue *forgejosdk.Issue
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		issue, resp, err = t.api.EditIssue(ref.Owner, ref.Name, int64(number), forgejosdk.EditIssueOption{
			Title:     stringValue(opts.Title),
			Body:      opts.Body,
			State:     forgejoStatePtr(opts.State),
			Assignees: assigneesValue(opts.Assignees),
		})
		return err
	})
	if err != nil {
		return gitealike.IssueDTO{}, forgejoHTTPError(resp, err)
	}
	return convertIssue(issue), nil
}

func (t *transport) EditPullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.PullRequestMutationOptions,
) (gitealike.PullRequestDTO, error) {
	var pr *forgejosdk.PullRequest
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		pr, resp, err = t.api.EditPullRequest(ref.Owner, ref.Name, int64(number), forgejosdk.EditPullRequestOption{
			Title:     stringValue(opts.Title),
			Body:      opts.Body,
			State:     forgejoStatePtr(opts.State),
			Assignees: assigneesValue(opts.Assignees),
		})
		return err
	})
	if err != nil {
		return gitealike.PullRequestDTO{}, forgejoHTTPError(resp, err)
	}
	return convertPullRequest(
		pr, t.mergeableForPullRequest(pr), t.metricsForPullRequest(pr),
	), nil
}

func (t *transport) MergePullRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	opts gitealike.MergeOptions,
) (gitealike.MergeResultDTO, error) {
	var merged bool
	var resp *forgejosdk.Response
	var rejection *gitealike.MergeRejection
	err := t.withRequestContext(ctx, func() error {
		// The capture slot is shared per client, so the clear/request/
		// read sequence stays inside the serialized request section: a
		// concurrent merge must not drop or consume this call's
		// rejection.
		t.mergeRejections.Take()
		var err error
		merged, resp, err = t.api.MergePullRequest(ref.Owner, ref.Name, int64(number), forgejosdk.MergePullRequestOption{
			Style:        forgejoMergeStyle(opts.Method),
			Title:        opts.CommitTitle,
			Message:      opts.CommitMessage,
			HeadCommitId: opts.ExpectedHeadSHA,
		})
		if err == nil && !merged {
			rejection = t.mergeRejections.Take()
		}
		return err
	})
	if err != nil {
		return gitealike.MergeResultDTO{}, forgejoHTTPError(resp, err)
	}
	if !merged {
		// The SDK reports any non-2xx merge response as merged=false
		// with a nil error; the captured rejection restores the real
		// status and provider message so the failure classifies
		// instead of being recorded as a successful merge.
		statusCode := 0
		if resp != nil && resp.Response != nil {
			statusCode = resp.StatusCode
		}
		return gitealike.MergeResultDTO{}, gitealike.MergeRejectionError(rejection, statusCode)
	}
	return gitealike.MergeResultDTO{Merged: merged}, nil
}

func (t *transport) ReplaceIssueLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	labelIDs []int64,
) ([]gitealike.LabelDTO, error) {
	var labels []*forgejosdk.Label
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		labels, resp, err = t.api.ReplaceIssueLabels(ref.Owner, ref.Name, int64(number), forgejosdk.IssueLabelsOption{
			Labels: labelIDs,
		})
		return err
	})
	if err != nil {
		return nil, forgejoHTTPError(resp, err)
	}
	return convertLabels(labels), nil
}

func (t *transport) CreateReviewRequests(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	reviewers []string,
) error {
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		resp, err = t.api.CreateReviewRequests(ref.Owner, ref.Name, int64(number), forgejosdk.PullReviewRequestOptions{
			Reviewers: reviewers,
		})
		return err
	})
	if err != nil {
		return forgejoHTTPError(resp, err)
	}
	return nil
}

func (t *transport) DeleteReviewRequests(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	reviewers []string,
) error {
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		resp, err = t.api.DeleteReviewRequests(ref.Owner, ref.Name, int64(number), forgejosdk.PullReviewRequestOptions{
			Reviewers: reviewers,
		})
		return err
	})
	if err != nil {
		return forgejoHTTPError(resp, err)
	}
	return nil
}

// assigneesValue keeps the no-change semantics of a nil option: the SDK
// serializes a nil slice as JSON null, which the server ignores, while a
// non-nil (possibly empty) slice replaces the assignee set.
func assigneesValue(assignees *[]string) []string {
	if assignees == nil {
		return nil
	}
	if *assignees == nil {
		return []string{}
	}
	return *assignees
}

func forgejoStatePtr(state *string) *forgejosdk.StateType {
	if state == nil {
		return nil
	}
	value := forgejosdk.StateType(*state)
	return &value
}

func forgejoMergeStyle(method string) forgejosdk.MergeStyle {
	switch method {
	case "squash":
		return forgejosdk.MergeStyleSquash
	case "rebase":
		return forgejosdk.MergeStyleRebase
	case "rebase-merge":
		return forgejosdk.MergeStyleRebaseMerge
	case "fast-forward-only":
		return forgejosdk.MergeStyleFastForwardOnly
	default:
		return forgejosdk.MergeStyleMerge
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
