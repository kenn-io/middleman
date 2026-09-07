package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func (c *Client) PublishDiffReviewDraft(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	input platform.PublishDiffReviewDraftInput,
) (*platform.PublishedDiffReview, error) {
	pid, err := projectLookupArg(ref)
	if err != nil {
		return nil, err
	}
	createdDraftIDs := make([]int64, 0, len(input.Comments))
	publishedCommentIDs := make([]int64, 0, len(input.Comments))
	submittedAt := time.Now().UTC()
	for _, comment := range input.Comments {
		draftNote, _, err := c.api.DraftNotes.CreateDraftNote(
			pid,
			int64(number),
			&gitlab.CreateDraftNoteOptions{
				Note:     new(comment.Body),
				CommitID: nonEmptyStringPtr(comment.Range.CommitSHA, comment.Range.DiffHeadSHA),
				Position: gitlabPositionOptions(comment.Range),
			},
			gitlab.WithContext(ctx),
		)
		if err != nil {
			if cleanupErr := c.deleteDraftNotes(ctx, pid, int64(number), createdDraftIDs); cleanupErr != nil {
				return nil, fmt.Errorf("%w; cleanup failed: %v", c.mapGitLabError("create_draft_note", err), cleanupErr)
			}
			return nil, c.mapGitLabError("create_draft_note", err)
		}
		if draftNote != nil && draftNote.ID > 0 {
			createdDraftIDs = append(createdDraftIDs, draftNote.ID)
		}
	}
	publishedAnyDraft := false
	for i, draftID := range createdDraftIDs {
		if _, err := c.api.DraftNotes.PublishDraftNote(pid, int64(number), draftID, gitlab.WithContext(ctx)); err != nil {
			if publishedAnyDraft {
				mappedErr := c.mapGitLabError("publish_draft_note", err)
				if cleanupErr := c.deleteDraftNotes(ctx, pid, int64(number), createdDraftIDs[i:]); cleanupErr != nil {
					mappedErr = fmt.Errorf("%w; cleanup failed: %v", mappedErr, cleanupErr)
				}
				return &platform.PublishedDiffReview{SubmittedAt: submittedAt}, &platform.DiffReviewPublishPartialError{
					Err:                 mappedErr,
					PublishedCommentIDs: publishedCommentIDs,
				}
			}
			if cleanupErr := c.deleteDraftNotes(ctx, pid, int64(number), createdDraftIDs); cleanupErr != nil {
				return nil, fmt.Errorf("%w; cleanup failed: %v", c.mapGitLabError("publish_draft_note", err), cleanupErr)
			}
			return nil, c.mapGitLabError("publish_draft_note", err)
		}
		publishedAnyDraft = true
		if i < len(input.Comments) && input.Comments[i].ID > 0 {
			publishedCommentIDs = append(publishedCommentIDs, input.Comments[i].ID)
		}
	}
	publishedAny := publishedAnyDraft
	publishedSummary := false
	if body := strings.TrimSpace(input.Body); body != "" {
		if _, _, err := c.api.Notes.CreateMergeRequestNote(
			pid,
			int64(number),
			&gitlab.CreateMergeRequestNoteOptions{Body: &body},
			gitlab.WithContext(ctx),
		); err != nil {
			return gitlabPublishFailure(submittedAt, publishedAny, publishedCommentIDs, c.mapGitLabError("create_merge_request_note", err))
		}
		publishedAny = true
		publishedSummary = true
	}
	if input.Action == platform.ReviewActionApprove {
		sha := reviewHeadSHA(input)
		if sha == "" {
			return gitlabPublishFailure(submittedAt, publishedAny, publishedCommentIDs, fmt.Errorf("approve_merge_request: missing review head sha"))
		}
		_, _, err := c.api.MergeRequestApprovals.ApproveMergeRequest(
			pid,
			int64(number),
			&gitlab.ApproveMergeRequestOptions{SHA: nonEmptyStringPtr(sha)},
			gitlab.WithContext(ctx),
		)
		if err != nil {
			mappedErr := mapGitLabMutationError(c.host, "approve_merge_request", sha, err)
			var platformErr *platform.Error
			if publishedSummary &&
				errors.As(mappedErr, &platformErr) &&
				platformErr.Code == platform.ErrCodeStaleState {
				platformErr.Hint = "the review summary was already posted; retrying will repeat it"
			}
			return gitlabPublishFailure(submittedAt, publishedAny, publishedCommentIDs, mappedErr)
		}
	}
	return &platform.PublishedDiffReview{SubmittedAt: submittedAt}, nil
}

func gitlabPublishFailure(
	submittedAt time.Time,
	publishedAny bool,
	publishedCommentIDs []int64,
	err error,
) (*platform.PublishedDiffReview, error) {
	if publishedAny {
		return &platform.PublishedDiffReview{SubmittedAt: submittedAt}, &platform.DiffReviewPublishPartialError{
			Err:                 err,
			PublishedCommentIDs: publishedCommentIDs,
		}
	}
	return nil, err
}

func (c *Client) deleteDraftNotes(ctx context.Context, pid any, number int64, draftIDs []int64) error {
	errs := make([]error, 0)
	for _, draftID := range draftIDs {
		if _, err := c.api.DraftNotes.DeleteDraftNote(pid, number, draftID, gitlab.WithContext(ctx)); err != nil {
			errs = append(errs, c.mapGitLabError("delete_draft_note", err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) ListMergeRequestReviewThreads(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.MergeRequestReviewThread, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	parentURL := gitLabMergeRequestURL(normalizedRef, number)
	return collectGitLabPages(ctx, func(ctx context.Context, page int64) ([]platform.MergeRequestReviewThread, int64, error) {
		discussions, nextPage, err := c.listMergeRequestDiscussionsPage(
			ctx, pid, normalizedRef, number, page,
		)
		if err != nil {
			return nil, 0, err
		}
		var out []platform.MergeRequestReviewThread
		for _, discussion := range discussions {
			out = append(out, gitlabReviewThreads(parentURL, discussion)...)
		}
		return out, nextPage, nil
	})
}

func (c *Client) ResolveDiffReviewThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	providerThreadID string,
) error {
	return c.ResolveThread(ctx, ref, number, providerThreadID, true)
}

func (c *Client) UnresolveDiffReviewThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	providerThreadID string,
) error {
	return c.ResolveThread(ctx, ref, number, providerThreadID, false)
}

func gitlabPositionOptions(lineRange platform.DiffReviewLineRange) *gitlab.PositionOptions {
	positionType := "text"
	headSHA := firstNonEmpty(lineRange.DiffHeadSHA, lineRange.CommitSHA)
	baseSHA := firstNonEmpty(lineRange.DiffBaseSHA, lineRange.MergeBaseSHA, headSHA)
	startSHA := firstNonEmpty(lineRange.MergeBaseSHA, lineRange.DiffBaseSHA, baseSHA)
	next := &gitlab.PositionOptions{
		PositionType: &positionType,
		HeadSHA:      nonEmptyStringPtr(headSHA),
		BaseSHA:      nonEmptyStringPtr(baseSHA),
		StartSHA:     nonEmptyStringPtr(startSHA),
		NewPath:      new(lineRange.Path),
		OldPath:      new(firstNonEmpty(lineRange.OldPath, lineRange.Path)),
	}
	if lineRange.Side == "left" {
		next.OldLine = new(int64(lineRange.Line))
	} else {
		next.NewLine = new(int64(lineRange.Line))
	}
	if lineRange.StartLine != nil && lineRange.StartSide != "" {
		next.LineRange = &gitlab.LineRangeOptions{
			Start: gitlabLinePosition(lineRange.StartSide, *lineRange.StartLine),
			End:   gitlabLinePosition(lineRange.Side, lineRange.Line),
		}
	}
	return next
}

func gitlabLinePosition(side string, line int) *gitlab.LinePositionOptions {
	positionType := "new"
	next := &gitlab.LinePositionOptions{Type: &positionType}
	if side == "left" {
		positionType = "old"
		next.OldLine = new(int64(line))
	} else {
		next.NewLine = new(int64(line))
	}
	return next
}

func gitlabReviewThreads(parentURL string, discussion *gitlab.Discussion) []platform.MergeRequestReviewThread {
	if discussion == nil {
		return nil
	}
	for _, note := range discussion.Notes {
		if note == nil || note.System || note.Position == nil {
			continue
		}
		return []platform.MergeRequestReviewThread{gitlabReviewThread(parentURL, discussion.ID, note)}
	}
	return nil
}

func gitlabReviewThread(parentURL string, discussionID string, note *gitlab.Note) platform.MergeRequestReviewThread {
	lineRange := gitlabReviewLineRange(note.Position)
	resolvedAt := (*time.Time)(nil)
	if note.Resolved && note.UpdatedAt != nil {
		updated := note.UpdatedAt.UTC()
		resolvedAt = &updated
	}
	createdAt := time.Time{}
	if note.CreatedAt != nil {
		createdAt = note.CreatedAt.UTC()
	}
	updatedAt := createdAt
	if note.UpdatedAt != nil {
		updatedAt = note.UpdatedAt.UTC()
	}
	return platform.MergeRequestReviewThread{
		ProviderThreadID:  discussionID,
		ProviderCommentID: strconv.FormatInt(note.ID, 10),
		Body:              note.Body,
		AuthorLogin:       note.Author.Username,
		DirectURL:         noteDirectURL(parentURL, note.ID),
		Range:             lineRange,
		Resolved:          note.Resolved,
		CreatedAt:         createdAt,
		UpdatedAt:         updatedAt,
		ResolvedAt:        resolvedAt,
	}
}

func gitlabReviewLineRange(position *gitlab.NotePosition) platform.DiffReviewLineRange {
	lineRange := platform.DiffReviewLineRange{
		Path:         firstNonEmpty(position.NewPath, position.OldPath),
		OldPath:      position.OldPath,
		Side:         "right",
		Line:         int(position.NewLine),
		LineType:     "add",
		DiffHeadSHA:  position.HeadSHA,
		DiffBaseSHA:  position.BaseSHA,
		MergeBaseSHA: position.StartSHA,
		CommitSHA:    position.HeadSHA,
	}
	if position.OldLine > 0 && position.NewLine > 0 {
		lineRange.LineType = "context"
		oldLine := int(position.OldLine)
		newLine := int(position.NewLine)
		lineRange.OldLine = &oldLine
		lineRange.NewLine = &newLine
	} else if position.OldLine > 0 && position.NewLine == 0 {
		lineRange.Side = "left"
		lineRange.Line = int(position.OldLine)
		lineRange.LineType = "delete"
		oldLine := int(position.OldLine)
		lineRange.OldLine = &oldLine
	} else if position.NewLine > 0 {
		newLine := int(position.NewLine)
		lineRange.NewLine = &newLine
	}
	if position.LineRange != nil && position.LineRange.StartRange != nil {
		start := position.LineRange.StartRange
		startSide := "right"
		startLine := int(start.NewLine)
		if start.Type == "old" {
			startSide = "left"
			startLine = int(start.OldLine)
		}
		if startLine > 0 {
			lineRange.StartSide = startSide
			lineRange.StartLine = &startLine
		}
	}
	return lineRange
}

func reviewHeadSHA(input platform.PublishDiffReviewDraftInput) string {
	if input.HeadSHA != "" {
		return input.HeadSHA
	}
	for _, comment := range input.Comments {
		if comment.Range.DiffHeadSHA != "" {
			return comment.Range.DiffHeadSHA
		}
		if comment.Range.CommitSHA != "" {
			return comment.Range.CommitSHA
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nonEmptyStringPtr(values ...string) *string {
	value := firstNonEmpty(values...)
	if value == "" {
		return nil
	}
	return &value
}
