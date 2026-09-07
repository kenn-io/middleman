package forgejo

import (
	"context"
	"fmt"
	"strconv"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/forge/platform"
)

func (c *Client) PublishDiffReviewDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.PublishDiffReviewDraftInput,
) (*platform.PublishedDiffReview, error) {
	return c.transport.PublishDiffReviewDraft(ctx, c.host, ref, number, input)
}

func (c *Client) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	threads, err := c.transport.ListMergeRequestReviewThreads(ctx, ref, number)
	if err != nil {
		return nil, c.ClassifyRepositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	return threads, nil
}

func (t *transport) PublishDiffReviewDraft(
	ctx context.Context,
	host string,
	ref platform.RepoRef,
	number int,
	input platform.PublishDiffReviewDraftInput,
) (*platform.PublishedDiffReview, error) {
	comments := make([]forgejosdk.CreatePullReviewComment, 0, len(input.Comments))
	commitID := input.HeadSHA
	for _, comment := range input.Comments {
		if commitID == "" {
			commitID = comment.Range.CommitSHA
			if commitID == "" {
				commitID = comment.Range.DiffHeadSHA
			}
		}
		comments = append(comments, forgejoReviewComments(comment)...)
	}
	var review *forgejosdk.PullReview
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		review, resp, err = t.api.CreatePullReview(ref.Owner, ref.Name, int64(number), forgejosdk.CreatePullReviewOptions{
			State:    forgejoReviewState(input.Action),
			Body:     input.Body,
			CommitID: commitID,
			Comments: comments,
		})
		return err
	})
	if err != nil {
		return nil, forgejoHTTPError(resp, err)
	}
	if review == nil {
		return nil, fmt.Errorf("forgejo create pull review returned nil review")
	}
	published := &platform.PublishedDiffReview{
		ProviderReviewID: strconv.FormatInt(review.ID, 10),
		SubmittedAt:      review.Submitted.UTC(),
	}
	return published, nil
}

func (t *transport) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	reviews, err := t.listAllPullReviews(ctx, ref, number)
	if err != nil {
		return nil, err
	}
	threads := make([]platform.MergeRequestReviewThread, 0)
	for _, review := range reviews {
		comments, err := t.listPullReviewComments(ctx, ref, number, review.ID)
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			threads = append(threads, forgejoReviewThread(review, comment))
		}
	}
	return threads, nil
}

func (t *transport) listAllPullReviews(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]*forgejosdk.PullReview, error) {
	return platform.CollectAllPages(ctx, "1", func(
		ctx context.Context,
		cursor string,
	) (platform.Page[*forgejosdk.PullReview], error) {
		page, err := strconv.Atoi(cursor)
		if err != nil {
			return platform.Page[*forgejosdk.PullReview]{}, fmt.Errorf(
				"parse Forgejo review page cursor: %w", err,
			)
		}
		var reviews []*forgejosdk.PullReview
		var resp *forgejosdk.Response
		err = t.withRequestContext(ctx, func() error {
			var err error
			reviews, resp, err = t.api.ListPullReviews(ref.Owner, ref.Name, int64(number), forgejosdk.ListPullReviewsOptions{
				Page: page, PageSize: 100,
			})
			return err
		})
		if err != nil {
			return platform.Page[*forgejosdk.PullReview]{}, forgejoHTTPError(resp, err)
		}
		if resp == nil || resp.NextPage == 0 {
			return platform.Page[*forgejosdk.PullReview]{Items: reviews, Exhausted: true}, nil
		}
		return platform.Page[*forgejosdk.PullReview]{
			Items: reviews, NextCursor: strconv.Itoa(resp.NextPage),
		}, nil
	})
}

func (t *transport) listPullReviewComments(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	reviewID int64,
) ([]*forgejosdk.PullReviewComment, error) {
	var comments []*forgejosdk.PullReviewComment
	var resp *forgejosdk.Response
	err := t.withRequestContext(ctx, func() error {
		var err error
		comments, resp, err = t.api.ListPullReviewComments(ref.Owner, ref.Name, int64(number), reviewID)
		return err
	})
	if err != nil {
		return nil, forgejoHTTPError(resp, err)
	}
	return comments, nil
}

func forgejoReviewState(action platform.ReviewAction) forgejosdk.ReviewStateType {
	switch action {
	case platform.ReviewActionApprove:
		return forgejosdk.ReviewStateApproved
	case platform.ReviewActionRequestChanges:
		return forgejosdk.ReviewStateRequestChanges
	default:
		return forgejosdk.ReviewStateComment
	}
}

func forgejoReviewComments(comment platform.LocalDiffReviewDraftComment) []forgejosdk.CreatePullReviewComment {
	next := forgejosdk.CreatePullReviewComment{
		Path: comment.Range.Path,
		Body: comment.Body,
	}
	if comment.Range.Side == "left" {
		next.OldLineNum = int64(comment.Range.Line)
	} else {
		next.NewLineNum = int64(comment.Range.Line)
	}
	return []forgejosdk.CreatePullReviewComment{next}
}

func forgejoReviewThread(
	review *forgejosdk.PullReview,
	comment *forgejosdk.PullReviewComment,
) platform.MergeRequestReviewThread {
	if review == nil {
		review = &forgejosdk.PullReview{}
	}
	if comment == nil {
		comment = &forgejosdk.PullReviewComment{}
	}
	line := int(comment.LineNum)
	side := "right"
	lineType := "add"
	var oldLine *int
	var newLine *int
	if comment.OldLineNum > 0 && comment.LineNum > 0 {
		old := int(comment.OldLineNum)
		new := int(comment.LineNum)
		line = new
		lineType = "context"
		oldLine = &old
		newLine = &new
	} else if comment.OldLineNum > 0 {
		old := int(comment.OldLineNum)
		line = old
		side = "left"
		lineType = "delete"
		oldLine = &old
	} else if comment.LineNum > 0 {
		new := int(comment.LineNum)
		newLine = &new
	}
	resolvedAt := (*time.Time)(nil)
	resolved := comment.Resolver != nil
	if resolved {
		updated := comment.Updated.UTC()
		resolvedAt = &updated
	}
	return platform.MergeRequestReviewThread{
		ProviderThreadID:  strconv.FormatInt(comment.ID, 10),
		ProviderReviewID:  strconv.FormatInt(review.ID, 10),
		ProviderCommentID: strconv.FormatInt(comment.ID, 10),
		Body:              comment.Body,
		AuthorLogin:       convertUser(comment.Reviewer).UserName,
		DirectURL:         comment.HTMLURL,
		Range: platform.DiffReviewLineRange{
			Path:        comment.Path,
			Side:        side,
			Line:        line,
			OldLine:     oldLine,
			NewLine:     newLine,
			LineType:    lineType,
			DiffHeadSHA: comment.CommitID,
			CommitSHA:   comment.CommitID,
		},
		Resolved:   resolved,
		CreatedAt:  comment.Created.UTC(),
		UpdatedAt:  comment.Updated.UTC(),
		ResolvedAt: resolvedAt,
	}
}
