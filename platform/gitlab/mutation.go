package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

// gitlabHeadMismatchPhrase is the documented GitLab rejection for
// sha-bound merge/approve when the source branch HEAD moved past the
// supplied sha.
const gitlabHeadMismatchPhrase = "sha does not match head"

// mapGitLabMutationError classifies a 409 as stale_state only when the
// request was actually sha-bound and GitLab's message is the known head
// mismatch rejection. Any other 409 keeps the generic conflict mapping
// so unrelated provider conflicts are not presented as staleness.
func mapGitLabMutationError(platformHost, capability, expectedHeadSHA string, err error) error {
	var gitlabErr *gitlab.ErrorResponse
	if expectedHeadSHA != "" &&
		errors.As(err, &gitlabErr) &&
		gitlabErr.HasStatusCode(http.StatusConflict) &&
		strings.Contains(strings.ToLower(gitlabErr.Message), gitlabHeadMismatchPhrase) {
		return &platform.Error{
			Code:         platform.ErrCodeStaleState,
			Provider:     platform.KindGitLab,
			PlatformHost: platformHost,
			Capability:   capability,
			Err:          err,
		}
	}
	return mapGitLabErrorForHost(platformHost, capability, err)
}

// discussionIDPattern validates GitLab discussion IDs which are 40-char hex strings.
var discussionIDPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func validateDiscussionID(discussionID string) error {
	if !discussionIDPattern.MatchString(discussionID) {
		return fmt.Errorf("invalid discussion ID format: must be 40-character hex string")
	}
	return nil
}

func (c *Client) ReplyToThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	discussionID string,
	body string,
) (platform.MergeRequestEvent, error) {
	if err := validateDiscussionID(discussionID); err != nil {
		return platform.MergeRequestEvent{}, err
	}

	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}

	note, _, err := c.api.Discussions.AddMergeRequestDiscussionNote(
		pid,
		int64(number),
		discussionID,
		&gitlab.AddMergeRequestDiscussionNoteOptions{Body: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.MergeRequestEvent{}, c.mapGitLabError("reply_to_discussion", err)
	}

	return mergeRequestNoteEvent(normalizedRef, number, note, discussionID), nil
}

func (c *Client) ResolveThread(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	discussionID string,
	resolved bool,
) error {
	if err := validateDiscussionID(discussionID); err != nil {
		return err
	}

	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return err
	}

	_, _, err = c.api.Discussions.ResolveMergeRequestDiscussion(
		pid,
		int64(number),
		discussionID,
		&gitlab.ResolveMergeRequestDiscussionOptions{Resolved: &resolved},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return c.mapGitLabError("resolve_discussion", err)
	}
	return nil
}

func (c *Client) CreateMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.MergeRequestEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	note, _, err := c.api.Notes.CreateMergeRequestNote(
		pid,
		int64(number),
		&gitlab.CreateMergeRequestNoteOptions{Body: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.MergeRequestEvent{}, c.mapGitLabError("create_merge_request_comment", err)
	}
	return mergeRequestNoteEvent(normalizedRef, number, note, ""), nil
}

func (c *Client) EditMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.MergeRequestEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}
	note, _, err := c.api.Notes.UpdateMergeRequestNote(
		pid,
		int64(number),
		commentID,
		&gitlab.UpdateMergeRequestNoteOptions{Body: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.MergeRequestEvent{}, c.mapGitLabError("edit_merge_request_comment", err)
	}
	return mergeRequestNoteEvent(normalizedRef, number, note, ""), nil
}

func (c *Client) DeleteMergeRequestComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
) error {
	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return err
	}
	_, err = c.api.Notes.DeleteMergeRequestNote(
		pid, int64(number), commentID, gitlab.WithContext(ctx),
	)
	if err != nil {
		return c.mapGitLabError("delete_merge_request_comment", err)
	}
	return nil
}

func (c *Client) CreateIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
) (platform.IssueEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	note, _, err := c.api.Notes.CreateIssueNote(
		pid,
		int64(number),
		&gitlab.CreateIssueNoteOptions{Body: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.IssueEvent{}, c.mapGitLabError("create_issue_comment", err)
	}
	return issueNoteEvent(normalizedRef, number, note), nil
}

func (c *Client) EditIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
	body string,
) (platform.IssueEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.IssueEvent{}, err
	}
	note, _, err := c.api.Notes.UpdateIssueNote(
		pid,
		int64(number),
		commentID,
		&gitlab.UpdateIssueNoteOptions{Body: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.IssueEvent{}, c.mapGitLabError("edit_issue_comment", err)
	}
	return issueNoteEvent(normalizedRef, number, note), nil
}

func (c *Client) DeleteIssueComment(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commentID int64,
) error {
	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return err
	}
	_, err = c.api.Notes.DeleteIssueNote(
		pid, int64(number), commentID, gitlab.WithContext(ctx),
	)
	if err != nil {
		return c.mapGitLabError("delete_issue_comment", err)
	}
	return nil
}

func (c *Client) SetMergeRequestState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.MergeRequest, error) {
	stateEvent, err := gitlabStateEvent(state)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	mr, _, err := c.api.MergeRequests.UpdateMergeRequest(
		pid,
		int64(number),
		&gitlab.UpdateMergeRequestOptions{StateEvent: &stateEvent},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.MergeRequest{}, c.mapGitLabError("set_merge_request_state", err)
	}
	return NormalizeDetailedMergeRequest(normalizedRef, mr), nil
}

func (c *Client) SetIssueState(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	state string,
) (platform.Issue, error) {
	stateEvent, err := gitlabStateEvent(state)
	if err != nil {
		return platform.Issue{}, err
	}
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Issue{}, err
	}
	issue, _, err := c.api.Issues.UpdateIssue(
		pid,
		int64(number),
		&gitlab.UpdateIssueOptions{StateEvent: &stateEvent},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.Issue{}, c.mapGitLabError("set_issue_state", err)
	}
	return NormalizeIssue(normalizedRef, issue), nil
}

// MergeMergeRequest accepts an MR. GitLab does not take a merge strategy
// per request: squash is a flag on accept, while merge-commit versus
// fast-forward behavior comes from the project's merge method setting.
// "squash" and "merge" map onto the squash flag; "rebase" cannot be
// honored per request and returns a typed capability error. A non-empty
// expectedHeadSHA is passed to GitLab, which rejects the merge with 409
// when the source branch HEAD has moved past the reviewed commit.
func (c *Client) MergeMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	commitTitle string,
	commitMessage string,
	method string,
	expectedHeadSHA string,
) (platform.MergeResult, error) {
	opts := &gitlab.AcceptMergeRequestOptions{}
	squash := false
	switch method {
	case "squash":
		squash = true
		opts.Squash = &squash
		if message := combineCommitMessage(commitTitle, commitMessage); message != "" {
			opts.SquashCommitMessage = &message
		}
	case "merge":
		opts.Squash = &squash
		if message := combineCommitMessage(commitTitle, commitMessage); message != "" {
			opts.MergeCommitMessage = &message
		}
	default:
		return platform.MergeResult{}, &platform.Error{
			Code:         platform.ErrCodeUnsupportedCapability,
			Provider:     platform.KindGitLab,
			PlatformHost: c.host,
			Capability:   "merge_method_" + method,
		}
	}

	opts.SHA = nonEmptyStringPtr(expectedHeadSHA)

	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeResult{}, err
	}
	mr, _, err := c.api.MergeRequests.AcceptMergeRequest(pid, int64(number), opts, gitlab.WithContext(ctx))
	if err != nil {
		return platform.MergeResult{}, mapGitLabMutationError(
			c.host, "merge_merge_request", expectedHeadSHA, err,
		)
	}
	if mr == nil {
		return platform.MergeResult{}, &platform.Error{
			Code:       platform.ErrCodeNotFound,
			Provider:   platform.KindGitLab,
			Capability: "merge_merge_request",
		}
	}
	return platform.MergeResult{
		Merged: normalizeMergeRequestState(mr.State) == "merged",
		SHA:    firstNonEmpty(mr.SquashCommitSHA, mr.MergeCommitSHA, mr.SHA),
	}, nil
}

func (c *Client) CreateIssue(
	ctx context.Context,
	ref platform.RepoRef,
	title string,
	body string,
) (platform.Issue, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Issue{}, err
	}
	issue, _, err := c.api.Issues.CreateIssue(
		pid,
		&gitlab.CreateIssueOptions{Title: &title, Description: &body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.Issue{}, c.mapGitLabError("create_issue", err)
	}
	return NormalizeIssue(normalizedRef, issue), nil
}

func (c *Client) EditMergeRequestContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.MergeRequest, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequest{}, err
	}
	mr, _, err := c.api.MergeRequests.UpdateMergeRequest(
		pid,
		int64(number),
		&gitlab.UpdateMergeRequestOptions{Title: title, Description: body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.MergeRequest{}, c.mapGitLabError("edit_merge_request_content", err)
	}
	return NormalizeDetailedMergeRequest(normalizedRef, mr), nil
}

func (c *Client) EditIssueContent(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	title *string,
	body *string,
) (platform.Issue, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Issue{}, err
	}
	issue, _, err := c.api.Issues.UpdateIssue(
		pid,
		int64(number),
		&gitlab.UpdateIssueOptions{Title: title, Description: body},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.Issue{}, c.mapGitLabError("edit_issue_content", err)
	}
	return NormalizeIssue(normalizedRef, issue), nil
}

// ApproveMergeRequest approves an MR through the GitLab approvals API.
// A non-empty expectedHeadSHA binds the approval to the caller's target
// provider head: GitLab rejects the approval when the MR head has moved.
// The approval itself needs no client-side check, but when a body will be
// posted as a note (notes are not sha-bound) the current head is verified
// first so a stale approval comment does not leave an orphaned note (a small
// race window remains between the check and the note).
//
// GitLab approvals carry no body, so a non-empty body is posted as a
// regular MR note before approving; the synthesized approval event keeps
// an empty body because the note is synced into the timeline as its own
// comment. If approval fails after the note was posted, the error says so
// explicitly — a retry repeats the comment, which is preferable to
// re-approving first since GitLab rejects duplicate approvals by the same
// user. GitLab has no native "request changes" review state, so only
// approval is supported here.
func (c *Client) ApproveMergeRequest(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	body string,
	expectedHeadSHA string,
) (platform.MergeRequestEvent, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MergeRequestEvent{}, err
	}

	comment := strings.TrimSpace(body)
	if expectedHeadSHA != "" && comment != "" {
		current, _, err := c.api.MergeRequests.GetMergeRequest(pid, int64(number), nil, gitlab.WithContext(ctx))
		if err != nil {
			return platform.MergeRequestEvent{}, c.mapGitLabError("approve_merge_request", err)
		}
		if current != nil && current.SHA != "" && current.SHA != expectedHeadSHA {
			return platform.MergeRequestEvent{}, &platform.Error{
				Code:         platform.ErrCodeStaleState,
				Provider:     platform.KindGitLab,
				PlatformHost: c.host,
				Capability:   "approve_merge_request",
			}
		}
	}

	notePosted := false
	if comment != "" {
		if _, _, err := c.api.Notes.CreateMergeRequestNote(
			pid,
			int64(number),
			&gitlab.CreateMergeRequestNoteOptions{Body: &comment},
			gitlab.WithContext(ctx),
		); err != nil {
			return platform.MergeRequestEvent{}, c.mapGitLabError("approve_merge_request_comment", err)
		}
		notePosted = true
	}

	approvals, _, err := c.api.MergeRequestApprovals.ApproveMergeRequest(
		pid,
		int64(number),
		&gitlab.ApproveMergeRequestOptions{SHA: nonEmptyStringPtr(expectedHeadSHA)},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		mapped := mapGitLabMutationError(c.host, "approve_merge_request", expectedHeadSHA, err)
		if notePosted {
			// The note side effect must survive problem mapping so the
			// client knows a retry repeats the comment.
			if platformErr, ok := errors.AsType[*platform.Error](mapped); ok {
				platformErr.Hint = "the review comment was already posted; retrying will repeat it"
			}
			return platform.MergeRequestEvent{}, fmt.Errorf(
				"review comment was posted but the approval failed; retrying will repeat the comment: %w",
				mapped,
			)
		}
		return platform.MergeRequestEvent{}, mapped
	}

	author := c.currentUsername(ctx)
	createdAt := time.Now().UTC()
	if approvals != nil && approvals.UpdatedAt != nil {
		createdAt = approvals.UpdatedAt.UTC()
	}
	return platform.MergeRequestEvent{
		Repo:               normalizedRef,
		MergeRequestNumber: number,
		EventType:          "review",
		Author:             author,
		Summary:            "approved",
		CreatedAt:          createdAt,
		DedupeKey:          noteDedupeKey(normalizedRef, "mr", number, "approval", author),
	}, nil
}

// currentUsername resolves the token's user for attribution of synthesized
// approval events. Attribution is best effort: the approval itself has
// already succeeded, so lookup failures degrade to an empty author rather
// than failing the mutation.
func (c *Client) currentUsername(ctx context.Context) string {
	user, _, err := c.api.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil || user == nil {
		return ""
	}
	return user.Username
}

func gitlabStateEvent(state string) (string, error) {
	switch state {
	case "open":
		return "reopen", nil
	case "closed":
		return "close", nil
	default:
		// Typed like rawProjectPath's input validation so the server
		// boundary translates it to a stable badRequest envelope.
		return "", &platform.Error{
			Code:       platform.ErrCodeInvalidRepoRef,
			Provider:   platform.KindGitLab,
			Field:      "state",
			Capability: "state_mutation",
		}
	}
}

func combineCommitMessage(title, message string) string {
	title = strings.TrimSpace(title)
	message = strings.TrimSpace(message)
	switch {
	case title == "":
		return message
	case message == "":
		return title
	default:
		return title + "\n\n" + message
	}
}

func mergeRequestNoteEvent(
	ref platform.RepoRef,
	number int,
	note *gitlab.Note,
	threadID string,
) platform.MergeRequestEvent {
	return platform.MergeRequestEvent{
		Repo:               ref,
		PlatformID:         note.ID,
		PlatformExternalID: strconv.FormatInt(note.ID, 10),
		MergeRequestNumber: number,
		EventType:          "issue_comment",
		Author:             noteAuthorUsername(note),
		Body:               note.Body,
		CreatedAt:          timeValue(note.CreatedAt),
		DirectURL:          noteDirectURL(gitLabMergeRequestURL(ref, number), note.ID),
		DedupeKey:          noteDedupeKey(ref, "mr", number, "note", strconv.FormatInt(note.ID, 10)),
		ThreadID:           threadID,
		PositionJSON:       serializeNotePosition(note.Position),
		Resolvable:         note.Resolvable,
		Resolved:           note.Resolved,
	}
}

func issueNoteEvent(
	ref platform.RepoRef,
	number int,
	note *gitlab.Note,
) platform.IssueEvent {
	return platform.IssueEvent{
		Repo:               ref,
		PlatformID:         note.ID,
		PlatformExternalID: strconv.FormatInt(note.ID, 10),
		IssueNumber:        number,
		EventType:          "issue_comment",
		Author:             noteAuthorUsername(note),
		Body:               note.Body,
		CreatedAt:          timeValue(note.CreatedAt),
		DirectURL:          noteDirectURL(gitLabIssueURL(ref, number), note.ID),
		DedupeKey:          noteDedupeKey(ref, "issue", number, "note", strconv.FormatInt(note.ID, 10)),
	}
}
