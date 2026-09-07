package pullapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func (s *Handler) getDiffReviewDraft(
	ctx context.Context,
	input *repoNumberInput,
) (*getDiffReviewDraftOutput, error) {
	repo, mr, err := s.lookupReviewDraftTarget(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number,
	)
	if err != nil {
		return nil, err
	}
	draft, err := s.db.GetMRReviewDraft(ctx, mr.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("get review draft failed")
	}
	return &getDiffReviewDraftOutput{Body: s.diffReviewDraftResponse(ctx, *repo, *mr, draft)}, nil
}

func (s *Handler) createDiffReviewDraftComment(
	ctx context.Context,
	input *createDiffReviewDraftCommentInput,
) (*createDiffReviewDraftCommentOutput, error) {
	body := strings.TrimSpace(input.Body.Body)
	if body == "" {
		return nil, huma.Error400BadRequest("comment body must not be empty")
	}
	lineRange, err := dbReviewLineRange(input.Body.Range)
	if err != nil {
		return nil, err
	}
	_, mr, err := s.lookupReviewDraftMutationTarget(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number,
	)
	if err != nil {
		return nil, err
	}
	draft, err := s.db.GetOrCreateMRReviewDraft(ctx, mr.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("create review draft failed")
	}
	if draft == nil {
		return nil, huma.Error500InternalServerError("create review draft failed")
	}
	comment, err := s.db.CreateMRReviewDraftComment(ctx, draft.ID, db.MRReviewDraftCommentInput{
		Body:  body,
		Range: lineRange,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("create review draft comment failed")
	}
	return &createDiffReviewDraftCommentOutput{
		Status: http.StatusCreated,
		Body:   diffReviewDraftCommentResponse(*comment),
	}, nil
}

func (s *Handler) editDiffReviewDraftComment(
	ctx context.Context,
	input *editDiffReviewDraftCommentInput,
) (*editDiffReviewDraftCommentOutput, error) {
	body := strings.TrimSpace(input.Body.Body)
	if body == "" {
		return nil, huma.Error400BadRequest("comment body must not be empty")
	}
	lineRange, err := dbReviewLineRange(input.Body.Range)
	if err != nil {
		return nil, err
	}
	commentID, err := parseReviewLocalID(input.DraftCommentID, "draft comment")
	if err != nil {
		return nil, err
	}
	_, mr, err := s.lookupReviewDraftMutationTarget(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number,
	)
	if err != nil {
		return nil, err
	}
	draft, err := s.db.GetMRReviewDraft(ctx, mr.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("get review draft failed")
	}
	if draft == nil {
		return nil, huma.Error404NotFound("review draft not found")
	}
	comment, err := s.db.UpdateMRReviewDraftComment(ctx, draft.ID, commentID, db.MRReviewDraftCommentInput{
		Body:  body,
		Range: lineRange,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("review draft comment not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("edit review draft comment failed")
	}
	return &editDiffReviewDraftCommentOutput{Body: diffReviewDraftCommentResponse(*comment)}, nil
}

func (s *Handler) deleteDiffReviewDraftComment(
	ctx context.Context,
	input *deleteDiffReviewDraftCommentInput,
) (*statusOnlyOutput, error) {
	commentID, err := parseReviewLocalID(input.DraftCommentID, "draft comment")
	if err != nil {
		return nil, err
	}
	_, mr, err := s.lookupReviewDraftMutationTarget(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number,
	)
	if err != nil {
		return nil, err
	}
	draft, err := s.db.GetMRReviewDraft(ctx, mr.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("get review draft failed")
	}
	if draft == nil {
		return nil, huma.Error404NotFound("review draft not found")
	}
	if err := s.db.DeleteMRReviewDraftComment(ctx, draft.ID, commentID); errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("review draft comment not found")
	} else if err != nil {
		return nil, huma.Error500InternalServerError("delete review draft comment failed")
	}
	return &statusOnlyOutput{Status: http.StatusOK}, nil
}

func (s *Handler) discardDiffReviewDraft(
	ctx context.Context,
	input *discardDiffReviewDraftInput,
) (*statusOnlyOutput, error) {
	_, mr, err := s.lookupReviewDraftMutationTarget(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number,
	)
	if err != nil {
		return nil, err
	}
	if err := s.db.DeleteMRReviewDraft(ctx, mr.ID); err != nil {
		return nil, huma.Error500InternalServerError("discard review draft failed")
	}
	return &statusOnlyOutput{Status: http.StatusOK}, nil
}

func (s *Handler) applyReviewSuggestions(
	ctx context.Context,
	input *applyReviewSuggestionInput,
) (*applyReviewSuggestionOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReviewSuggestionApplication,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityReviewSuggestionApplication); err != nil {
		return nil, err
	}
	if err := s.requireReviewSuggestionCapabilities(*repo); err != nil {
		return nil, err
	}
	if len(input.Body.Suggestions) == 0 {
		return nil, httpapi.Validation("body.suggestions", "at least one suggestion is required")
	}
	caps := s.capabilitiesForRepo(*repo)
	expectedHeadSHA := strings.TrimSpace(input.Body.ExpectedHeadSHA)
	if expectedHeadSHA == "" && caps.MutationHeadBinding {
		return nil, httpapi.Validation(
			"body.expected_head_sha",
			"required for this provider: echo the platform_head_sha you rendered",
		)
	}
	applier, err := s.syncer.ReviewSuggestionApplier(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	if availability := s.operations(*repo).ApplyReviewSuggestion; !availability.Available &&
		availability.Code == "rate_limited" {
		return nil, operationRateLimitedProblem(*repo, availability)
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, huma.Error500InternalServerError("get pull request failed")
	}
	if mr == nil {
		return nil, huma.Error404NotFound("pull request not found")
	}
	if mr.State != db.MergeRequestStateOpen {
		return nil, httpapi.Conflict(
			httpapi.CodeConflict,
			"pull request is not open",
			map[string]any{"reason": "not_open"},
		)
	}
	if repoProviderKind(*repo) == platform.KindGitHub && platformgithub.ParseHeadRepoFullName(mr.HeadRepoCloneURL) == "" {
		return nil, httpapi.Conflict(
			httpapi.CodeConflict,
			"pull request head repository is unknown",
			map[string]any{"reason": "head_repo_unknown"},
		)
	}
	if err := verifyClientReviewedHeadWithoutRefresh(expectedHeadSHA, mr.PlatformHeadSHA); err != nil {
		return nil, err
	}
	suggestions := make([]platform.ReviewSuggestion, 0, len(input.Body.Suggestions))
	seenThreadIDs := make(map[int64]struct{}, len(input.Body.Suggestions))
	for i, request := range input.Body.Suggestions {
		field := "body.suggestions[" + strconv.Itoa(i) + "].thread_id"
		threadID, err := parseReviewLocalID(request.ThreadID, "review thread")
		if err != nil {
			return nil, httpapi.Validation(field, "review thread id must be a positive integer")
		}
		if _, ok := seenThreadIDs[threadID]; ok {
			return nil, httpapi.Validation(field, "duplicate review thread id")
		}
		seenThreadIDs[threadID] = struct{}{}
		thread, err := s.db.GetMRReviewThread(ctx, mr.ID, threadID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, huma.Error404NotFound("review thread not found")
			}
			return nil, huma.Error500InternalServerError("get review thread failed")
		}
		if thread == nil {
			return nil, huma.Error404NotFound("review thread not found")
		}
		if err := validateReviewSuggestionThread(*thread, expectedHeadSHA); err != nil {
			return nil, err
		}
		replacement, err := verifyReviewSuggestionReplacement(thread.Body, request.Replacement)
		if err != nil {
			return nil, httpapi.Validation(
				"body.suggestions["+strconv.Itoa(i)+"].replacement",
				err.Error(),
			)
		}
		suggestions = append(suggestions, platform.ReviewSuggestion{
			ProviderThreadID:  thread.ProviderThreadID,
			ProviderCommentID: thread.ProviderCommentID,
			Range:             platformReviewLineRange(thread.Range),
			Replacement:       replacement,
		})
	}
	result, err := applier.ApplyReviewSuggestions(
		ctx,
		platformRepoRefFromDB(*repo),
		input.Number,
		platform.ApplyReviewSuggestionsInput{
			HeadBranch:       mr.HeadBranch,
			HeadRepoCloneURL: mr.HeadRepoCloneURL,
			ExpectedHeadSHA:  expectedHeadSHA,
			Message:          strings.TrimSpace(input.Body.Message),
			Suggestions:      suggestions,
		},
	)
	s.syncAfterReviewSuggestionApply(*repo, input.Number)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)),
			repoProviderHost(*repo),
			"apply review suggestions on provider failed",
		)
	}
	response := ApplyReviewSuggestionResponse{Status: "applied"}
	if result != nil {
		response.CommitSHA = result.CommitSHA
		response.CommitURL = result.CommitURL
	}
	return &applyReviewSuggestionOutput{Body: response}, nil
}

func validateReviewSuggestionThread(thread db.MRReviewThread, expectedHeadSHA string) error {
	lineRange := thread.Range
	if strings.TrimSpace(lineRange.Path) == "" {
		return huma.Error400BadRequest("review thread path is missing")
	}
	if strings.ToLower(strings.TrimSpace(lineRange.Side)) != "right" {
		return huma.Error400BadRequest("suggestions on removed lines cannot be applied")
	}
	if lineRange.Line <= 0 {
		return huma.Error400BadRequest("review thread line is missing")
	}
	if lineRange.StartLine != nil && *lineRange.StartLine <= 0 {
		return huma.Error400BadRequest("review thread start line is invalid")
	}
	if lineRange.StartLine != nil && *lineRange.StartLine > lineRange.Line {
		return huma.Error400BadRequest("review thread start line must be before line")
	}
	if strings.TrimSpace(lineRange.DiffHeadSHA) == "" {
		return httpapi.Conflict(
			httpapi.CodeConflict,
			"review suggestion is missing a reviewed head commit",
			map[string]any{"reason": "head_unknown"},
		)
	}
	if expectedHeadSHA != "" && lineRange.DiffHeadSHA != expectedHeadSHA {
		return httpapi.Conflict(
			httpapi.CodeConflict,
			"target changed since it was reviewed; refresh and retry",
			map[string]any{"reason": "stale_state"},
		)
	}
	return nil
}

func (s *Handler) requireReviewSuggestionCapabilities(repo db.Repo) error {
	caps := s.capabilitiesForRepo(repo)
	for _, capability := range []string{
		capabilityMutationHeadBinding,
		capabilityReadReviewThreads,
	} {
		if !capabilityEnabled(caps, capability) {
			return httpapi.UnsupportedCapability(repo, capability)
		}
	}
	return nil
}

func verifyReviewSuggestionReplacement(body, replacement string) (string, error) {
	for _, stored := range markdownSuggestionReplacements(body) {
		if replacement == stored {
			return stored, nil
		}
	}
	return "", errors.New("must match a stored review suggestion")
}

type markdownFence struct {
	marker byte
	length int
	info   string
}

func markdownSuggestionReplacements(body string) []string {
	if body == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	replacements := make([]string, 0)
	for lineIndex := 0; lineIndex < len(lines); {
		fence, ok := openingMarkdownFence(lines[lineIndex])
		if !ok {
			lineIndex++
			continue
		}
		closeIndex := closingMarkdownFenceIndex(lines, lineIndex, fence)
		if closeIndex == -1 {
			break
		}
		if !markdownFenceInfoIsSuggestion(fence.info) {
			lineIndex = closeIndex + 1
			continue
		}
		replacements = append(replacements, strings.Join(lines[lineIndex+1:closeIndex], "\n"))
		lineIndex = closeIndex + 1
	}
	return replacements
}

func closingMarkdownFenceIndex(lines []string, openIndex int, fence markdownFence) int {
	for i := openIndex + 1; i < len(lines); i++ {
		if closesMarkdownFence(lines[i], fence) {
			return i
		}
	}
	return -1
}

func markdownFenceContent(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	indent := 0
	for indent < len(line) && indent < 3 && line[indent] == ' ' {
		indent++
	}
	if indent < len(line) && line[indent] == ' ' {
		return "", false
	}
	return line[indent:], true
}

func openingMarkdownFence(line string) (markdownFence, bool) {
	line, ok := markdownFenceContent(line)
	if !ok {
		return markdownFence{}, false
	}
	if len(line) < 3 {
		return markdownFence{}, false
	}
	marker := line[0]
	if marker != '`' && marker != '~' {
		return markdownFence{}, false
	}
	length := 0
	for length < len(line) && line[length] == marker {
		length++
	}
	if length < 3 {
		return markdownFence{}, false
	}
	return markdownFence{
		marker: marker,
		length: length,
		info:   strings.TrimSpace(line[length:]),
	}, true
}

func markdownFenceInfoIsSuggestion(info string) bool {
	lower := strings.ToLower(info)
	if !strings.HasPrefix(lower, "suggestion") {
		return false
	}
	if len(lower) == len("suggestion") {
		return true
	}
	next := rune(lower[len("suggestion")])
	return next == ':' || next == ' ' || next == '\t'
}

func closesMarkdownFence(line string, fence markdownFence) bool {
	line, ok := markdownFenceContent(line)
	if !ok {
		return false
	}
	count := 0
	for count < len(line) && line[count] == fence.marker {
		count++
	}
	if count < fence.length {
		return false
	}
	return strings.TrimSpace(line[count:]) == ""
}

func (s *Handler) syncAfterReviewSuggestionApply(repo db.Repo, number int) {
	kind := repoProviderKind(repo)
	host := repoProviderHost(repo)
	key := "pr:" + string(kind) + ":" + host + ":" + repo.RepoPath +
		"#" + strconv.Itoa(number)
	s.enqueueDetailSyncOrRerun(
		key,
		[]any{
			"type", "pr",
			"provider", string(kind),
			"platform_host", host,
			"repo_path", repo.RepoPath,
			"owner", repo.Owner,
			"name", repo.Name,
			"number", number,
			"source", "review_suggestion_apply",
		},
		func(ctx context.Context) error {
			return s.syncMergeRequestIfVisible(ctx, repo, number)
		},
	)
}

func (s *Handler) syncMergeRequestIfVisible(
	ctx context.Context,
	repo db.Repo,
	number int,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repo.ID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return fmt.Errorf("check pull request visibility: %w", err)
	}
	if removed {
		return nil
	}
	return s.syncer.SyncMROnProvider(
		ctx,
		repoProviderKind(repo), repoProviderHost(repo),
		repo.Owner, repo.Name, number,
	)
}

func (s *Handler) publishDiffReviewDraft(
	ctx context.Context,
	input *publishDiffReviewDraftInput,
) (*actionStatusOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReviewDraftMutation,
	)
	if err != nil {
		return nil, err
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, huma.Error500InternalServerError("get pull request failed")
	}
	if mr == nil {
		return nil, huma.Error404NotFound("pull request not found")
	}
	action, err := parseReviewAction(input.Body.Action)
	if err != nil {
		return nil, err
	}
	caps := s.capabilitiesForRepo(*repo)
	if !reviewActionSupported(caps, action) {
		return nil, httpapi.UnsupportedCapability(*repo, "review_action_"+string(action))
	}
	if action == platform.ReviewActionApprove && s.mergeRequestAuthoredByViewer(ctx, *repo, *mr) {
		return nil, selfApprovalProblem(*repo)
	}
	draft, err := s.db.GetMRReviewDraft(ctx, mr.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("get review draft failed")
	}
	if draft == nil || len(draft.Comments) == 0 {
		return nil, huma.Error400BadRequest("review draft has no comments")
	}
	reviewHeadSHA := mr.DiffHeadSHA
	if reviewHeadSHA == "" {
		reviewHeadSHA = mr.PlatformHeadSHA
	}
	// Approve publishes include inline draft comments, so on providers
	// that enforce head binding they use the reviewedHeadSHA gate shared
	// with merge rather than direct /approve's provider-head target. This
	// fails closed on a missing or stale reviewed diff snapshot, including
	// a moved base SHA with an unchanged head. The per-comment head check
	// below only compares DiffHeadSHA, so a stale base would otherwise let
	// an approval land on an out-of-date diff.
	if action == platform.ReviewActionApprove && caps.MutationHeadBinding {
		gatedHead, gateErr := s.reviewedHeadSHA(repo, mr)
		if gateErr != nil {
			return nil, gateErr
		}
		reviewHeadSHA = gatedHead
	}
	if reviewHeadSHA == "" {
		return nil, huma.Error409Conflict("review diff is unavailable")
	}
	for _, comment := range draft.Comments {
		if comment.Range.DiffHeadSHA == "" || comment.Range.DiffHeadSHA != reviewHeadSHA {
			return nil, huma.Error409Conflict("review draft is stale")
		}
		if !caps.NativeMultilineRanges && (comment.Range.StartLine != nil || comment.Range.StartSide != "") {
			return nil, huma.Error400BadRequest("multiline review ranges are unsupported")
		}
	}
	mutator, err := s.syncer.DiffReviewDraftMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	comments := make([]platform.LocalDiffReviewDraftComment, 0, len(draft.Comments))
	for _, comment := range draft.Comments {
		lineRange := platformReviewLineRange(comment.Range)
		lineRange.DiffBaseSHA = mr.DiffBaseSHA
		lineRange.MergeBaseSHA = mr.MergeBaseSHA
		comments = append(comments, platform.LocalDiffReviewDraftComment{
			ID:        comment.ID,
			Body:      comment.Body,
			Range:     lineRange,
			CreatedAt: comment.CreatedAt,
			UpdatedAt: comment.UpdatedAt,
		})
	}
	if _, err := mutator.PublishDiffReviewDraft(ctx, platformRepoRefFromDB(*repo), input.Number, platform.PublishDiffReviewDraftInput{
		Body:     strings.TrimSpace(input.Body.Body),
		Action:   action,
		HeadSHA:  reviewHeadSHA,
		Comments: comments,
	}); err != nil {
		if partialErr, ok := errors.AsType[*platform.DiffReviewPublishPartialError](err); ok {
			if len(partialErr.PublishedCommentIDs) > 0 {
				if discardErr := s.deletePublishedReviewDraftComments(ctx, draft.ID, mr.ID, partialErr.PublishedCommentIDs); discardErr != nil {
					return nil, huma.Error500InternalServerError("discard partially published review draft comments failed")
				}
			}
			ingestErr := s.tryIngestPublishedReviewThreads(ctx, *repo, *mr)
			if errors.Is(partialErr, platform.ErrStaleState) || ingestErr != nil {
				s.syncAfterReviewDraftPublish(*repo, input.Number)
			}
			if mapped := diffReviewPartialPublishProblem(partialErr, *repo); mapped != nil {
				return nil, mapped
			}
			return &actionStatusOutput{Body: ActionStatusBody{Status: "partially_published"}}, nil
		}
		if errors.Is(err, platform.ErrStaleState) {
			s.syncAfterReviewDraftPublish(*repo, input.Number)
		}
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)),
			repoProviderHost(*repo),
			"publish review draft on provider failed",
		)
	}
	if err := s.db.DeleteMRReviewDraft(ctx, mr.ID); err != nil {
		return nil, huma.Error500InternalServerError("discard published review draft failed")
	}
	if err := s.tryIngestPublishedReviewThreads(ctx, *repo, *mr); err != nil {
		s.syncAfterReviewDraftPublish(*repo, input.Number)
	}
	return &actionStatusOutput{Body: ActionStatusBody{Status: "published"}}, nil
}

func (s *Handler) tryIngestPublishedReviewThreads(
	ctx context.Context,
	repo db.Repo,
	mr db.MergeRequest,
) error {
	if !capabilityEnabled(s.capabilitiesForRepo(repo), capabilityReadReviewThreads) {
		return nil
	}
	if err := s.ingestDiffReviewThreads(ctx, repo, mr); err != nil {
		slog.Warn("published review thread ingestion failed; scheduling background sync",
			"repo", repo.Owner+"/"+repo.Name,
			"number", mr.Number,
			"err", err,
		)
		return err
	}
	return nil
}

func (s *Handler) syncAfterReviewDraftPublish(repo db.Repo, number int) {
	s.runBackground(func(bgCtx context.Context) {
		if syncErr := s.syncMergeRequestIfVisible(
			bgCtx, repo, number,
		); syncErr != nil {
			slog.Warn("background sync after review draft publish", "err", syncErr)
		}
	})
}

func diffReviewPartialPublishProblem(
	err *platform.DiffReviewPublishPartialError,
	repo db.Repo,
) huma.StatusError {
	if err == nil || err.Err == nil {
		return nil
	}
	var platformErr *platform.Error
	if !errors.As(err.Err, &platformErr) {
		return nil
	}
	if platformErr.Code != platform.ErrCodeStaleState {
		return nil
	}
	mapped := httpapi.ProviderCallProblem(err.Err, string(repoProviderKind(repo)), repoProviderHost(repo))
	if problem, ok := mapped.(*httpapi.ProblemError); ok {
		if problem.Details == nil {
			problem.Details = map[string]any{}
		}
		problem.Details["partialPublish"] = true
		problem.Details["publishedCommentCount"] = len(err.PublishedCommentIDs)
	}
	return mapped
}

func (s *Handler) deletePublishedReviewDraftComments(
	ctx context.Context,
	draftID int64,
	mrID int64,
	commentIDs []int64,
) error {
	for _, commentID := range commentIDs {
		if err := s.db.DeleteMRReviewDraftComment(ctx, draftID, commentID); err != nil {
			return err
		}
	}
	remaining, err := s.db.ListMRReviewDraftComments(ctx, draftID)
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		return s.db.DeleteMRReviewDraft(ctx, mrID)
	}
	return nil
}

func (s *Handler) resolveDiffReviewThread(
	ctx context.Context,
	input *resolveDiffReviewThreadInput,
) (*statusOnlyOutput, error) {
	return s.setDiffReviewThreadResolved(ctx, input, true)
}

func (s *Handler) unresolveDiffReviewThread(
	ctx context.Context,
	input *resolveDiffReviewThreadInput,
) (*statusOnlyOutput, error) {
	return s.setDiffReviewThreadResolved(ctx, input, false)
}

func (s *Handler) setDiffReviewThreadResolved(
	ctx context.Context,
	input *resolveDiffReviewThreadInput,
	resolved bool,
) (*statusOnlyOutput, error) {
	threadID, err := parseReviewLocalID(input.ThreadID, "review thread")
	if err != nil {
		return nil, err
	}
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReviewThreadResolution,
	)
	if err != nil {
		return nil, err
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, huma.Error500InternalServerError("get pull request failed")
	}
	if mr == nil {
		return nil, huma.Error404NotFound("pull request not found")
	}
	thread, err := s.db.GetMRReviewThread(ctx, mr.ID, threadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("review thread not found")
		}
		return nil, huma.Error500InternalServerError("get review thread failed")
	}
	if thread == nil {
		return nil, huma.Error404NotFound("review thread not found")
	}
	resolver, err := s.syncer.DiffReviewThreadResolver(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	if resolved {
		err = resolver.ResolveDiffReviewThread(
			ctx, platformRepoRefFromDB(*repo), input.Number, thread.ProviderThreadID,
		)
	} else {
		err = resolver.UnresolveDiffReviewThread(
			ctx, platformRepoRefFromDB(*repo), input.Number, thread.ProviderThreadID,
		)
	}
	if err != nil {
		return nil, huma.Error502BadGateway("update review thread on provider failed")
	}
	var resolvedAt *time.Time
	if resolved {
		now := s.now().UTC()
		resolvedAt = &now
	}
	if err := s.db.SetMRReviewThreadResolved(ctx, mr.ID, thread.ID, resolved, resolvedAt); err != nil {
		return nil, huma.Error500InternalServerError("persist review thread state failed")
	}
	return &statusOnlyOutput{Status: http.StatusOK}, nil
}

func (s *Handler) lookupReviewDraftTarget(
	ctx context.Context,
	provider, platformHost, owner, name string,
	number int,
) (*db.Repo, *db.MergeRequest, error) {
	repo, err := s.lookupRepoByProviderRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, nil, providerRouteLookupError(err)
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, number)
	if err != nil {
		return nil, nil, huma.Error500InternalServerError("get pull request failed")
	}
	if mr == nil {
		return nil, nil, huma.Error404NotFound("pull request not found")
	}
	return repo, mr, nil
}

func (s *Handler) lookupReviewDraftMutationTarget(
	ctx context.Context,
	provider, platformHost, owner, name string,
	number int,
) (*db.Repo, *db.MergeRequest, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		provider, platformHost, owner, name,
		capabilityReviewDraftMutation,
	)
	if err != nil {
		return nil, nil, err
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, number)
	if err != nil {
		return nil, nil, huma.Error500InternalServerError("get pull request failed")
	}
	if mr == nil {
		return nil, nil, huma.Error404NotFound("pull request not found")
	}
	return repo, mr, nil
}

func (s *Handler) ingestDiffReviewThreads(
	ctx context.Context,
	repo db.Repo,
	mr db.MergeRequest,
) error {
	reader, err := s.syncer.MergeRequestReviewThreadReader(
		repoProviderKind(repo), repoProviderHost(repo),
	)
	if err != nil {
		return huma.Error404NotFound(err.Error())
	}
	threads, err := reader.ListMergeRequestReviewThreads(
		ctx, platformRepoRefFromDB(repo), mr.Number,
	)
	if err != nil {
		return huma.Error502BadGateway("read review threads from provider failed")
	}
	providerUpdatedAt, err := s.providerMergeRequestUpdatedAt(ctx, repo, mr.Number)
	if err != nil {
		return huma.Error502BadGateway("read pull request activity from provider failed")
	}
	dbThreads := make([]db.MRReviewThread, 0, len(threads))
	events := make([]db.MREvent, 0, len(threads))
	seenProviderThreadIDs := make(map[string]struct{}, len(threads))
	for _, thread := range threads {
		providerThreadID := thread.ProviderThreadID
		if providerThreadID == "" {
			providerThreadID = thread.ProviderCommentID
		}
		if providerThreadID == "" {
			continue
		}
		if _, ok := seenProviderThreadIDs[providerThreadID]; !ok {
			seenProviderThreadIDs[providerThreadID] = struct{}{}
			dbThread := db.MRReviewThread{
				ProviderThreadID:  providerThreadID,
				ProviderReviewID:  thread.ProviderReviewID,
				ProviderCommentID: thread.ProviderCommentID,
				Body:              thread.Body,
				AuthorLogin:       thread.AuthorLogin,
				Range:             dbReviewLineRangeFromPlatform(thread.Range),
				Resolved:          thread.Resolved,
				CreatedAt:         thread.CreatedAt,
				UpdatedAt:         thread.UpdatedAt,
				ResolvedAt:        thread.ResolvedAt,
				MetadataJSON:      thread.MetadataJSON,
			}
			dbThreads = append(dbThreads, dbThread)
		}
		eventExternalID := firstReviewThreadNonEmpty(thread.ProviderCommentID, providerThreadID)
		if eventExternalID != "" {
			createdAt := thread.CreatedAt
			if createdAt.IsZero() {
				return huma.Error502BadGateway("provider review thread is missing creation time")
			}
			dedupeKey := "review_comment:" + eventExternalID
			threadID := providerThreadID
			events = append(events, db.MREvent{
				MergeRequestID:     mr.ID,
				PlatformExternalID: eventExternalID,
				EventType:          "review_comment",
				Author:             thread.AuthorLogin,
				Body:               thread.Body,
				MetadataJSON:       thread.MetadataJSON,
				CreatedAt:          createdAt,
				DedupeKey:          dedupeKey,
				ThreadID:           &threadID,
			})
		}
	}
	applied, err := s.db.CommitMergeRequestChildSnapshot(ctx, db.MergeRequestChildSnapshot{
		MergeRequestID:         mr.ID,
		ExpectedRevision:       mr.SnapshotRevision,
		ProviderUpdatedAt:      &providerUpdatedAt,
		InlineComments:         events,
		ReviewThreads:          dbThreads,
		InlineCommentsComplete: true,
	})
	if err != nil {
		return huma.Error500InternalServerError("persist review threads failed")
	}
	if !applied {
		return huma.Error409Conflict("pull request changed during review thread refresh")
	}
	return nil
}

func (s *Handler) providerMergeRequestUpdatedAt(
	ctx context.Context,
	repo db.Repo,
	number int,
) (time.Time, error) {
	reader, err := s.syncer.SyncRegistry().MergeRequestReader(
		repoProviderKind(repo), repoProviderHost(repo),
	)
	if err != nil {
		return time.Time{}, err
	}
	current, err := reader.GetMergeRequest(ctx, platformRepoRefFromDB(repo), number)
	if err != nil {
		return time.Time{}, err
	}
	if current.Number != number {
		return time.Time{}, fmt.Errorf(
			"provider returned merge request %d while refreshing %d", current.Number, number,
		)
	}
	if current.UpdatedAt.IsZero() {
		return time.Time{}, errors.New("provider returned merge request without updated time")
	}
	return current.UpdatedAt.UTC(), nil
}

func (s *Handler) diffReviewDraftResponse(
	ctx context.Context,
	repo db.Repo,
	mr db.MergeRequest,
	draft *db.MRReviewDraft,
) diffReviewDraftResponse {
	caps := s.capabilitiesForRepo(repo)
	supportedActions := caps.SupportedReviewActions
	// A viewer cannot approve their own pull request, and the publish
	// endpoint rejects such approvals. Drop approve from the advertised
	// actions so the draft tray never offers an action that fails on submit.
	if s.mergeRequestAuthoredByViewer(ctx, repo, mr) {
		supportedActions = reviewActionsExcludingApprove(supportedActions)
	}
	resp := diffReviewDraftResponse{
		Comments:              []diffReviewDraftComment{},
		SupportedActions:      supportedActions,
		NativeMultilineRanges: caps.NativeMultilineRanges,
	}
	if draft == nil {
		return resp
	}
	resp.DraftID = strconv.FormatInt(draft.ID, 10)
	resp.Comments = make([]diffReviewDraftComment, 0, len(draft.Comments))
	for _, comment := range draft.Comments {
		resp.Comments = append(resp.Comments, diffReviewDraftCommentResponse(comment))
	}
	return resp
}

func diffReviewDraftCommentResponse(comment db.MRReviewDraftComment) diffReviewDraftComment {
	lineRange := comment.Range
	return diffReviewDraftComment{
		ID:          strconv.FormatInt(comment.ID, 10),
		Body:        comment.Body,
		Path:        lineRange.Path,
		OldPath:     lineRange.OldPath,
		Side:        lineRange.Side,
		StartSide:   lineRange.StartSide,
		StartLine:   lineRange.StartLine,
		Line:        lineRange.Line,
		OldLine:     lineRange.OldLine,
		NewLine:     lineRange.NewLine,
		LineType:    lineRange.LineType,
		DiffHeadSHA: lineRange.DiffHeadSHA,
		CommitSHA:   lineRange.CommitSHA,
		CreatedAt:   formatUTCRFC3339(comment.CreatedAt),
		UpdatedAt:   formatUTCRFC3339(comment.UpdatedAt),
	}
}

func diffReviewThreadResponseFromDB(thread db.MRReviewThread) diffReviewThreadResponse {
	lineRange := thread.Range
	return diffReviewThreadResponse{
		ID:                strconv.FormatInt(thread.ID, 10),
		ProviderCommentID: thread.ProviderCommentID,
		Path:              lineRange.Path,
		OldPath:           lineRange.OldPath,
		Side:              lineRange.Side,
		StartSide:         lineRange.StartSide,
		StartLine:         lineRange.StartLine,
		Line:              lineRange.Line,
		OldLine:           lineRange.OldLine,
		NewLine:           lineRange.NewLine,
		LineType:          lineRange.LineType,
		DiffHeadSHA:       lineRange.DiffHeadSHA,
		CommitSHA:         lineRange.CommitSHA,
		Body:              thread.Body,
		MetadataJSON:      thread.MetadataJSON,
		AuthorLogin:       thread.AuthorLogin,
		Resolved:          thread.Resolved,
		CanResolve:        true,
		CreatedAt:         formatUTCRFC3339(thread.CreatedAt),
		UpdatedAt:         formatUTCRFC3339(thread.UpdatedAt),
	}
}

func firstReviewThreadNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mergeRequestEventResponseFromDB(event db.MREvent) mergeRequestEventResponse {
	return mergeRequestEventResponse{
		ID:                 event.ID,
		MergeRequestID:     event.MergeRequestID,
		PlatformID:         event.PlatformID,
		PlatformExternalID: event.PlatformExternalID,
		EventType:          event.EventType,
		Author:             event.Author,
		Summary:            event.Summary,
		Body:               event.Body,
		MetadataJSON:       event.MetadataJSON,
		CreatedAt:          event.CreatedAt,
		DedupeKey:          event.DedupeKey,
		DirectURL:          event.DirectURL,
		ThreadID:           event.ThreadID,
		Resolvable:         event.Resolvable,
		Resolved:           event.Resolved,
	}
}

func (s *Handler) mergeRequestEventResponses(
	ctx context.Context,
	mrID int64,
	events []db.MREvent,
) ([]mergeRequestEventResponse, error) {
	threads, err := s.db.ListMRReviewThreads(ctx, mrID)
	if err != nil {
		return nil, err
	}
	threadsByProviderID := make(map[string]diffReviewThreadResponse, len(threads)*2)
	for _, thread := range threads {
		resp := diffReviewThreadResponseFromDB(thread)
		if thread.ProviderThreadID != "" {
			threadsByProviderID[thread.ProviderThreadID] = resp
		}
		if thread.ProviderCommentID != "" {
			threadsByProviderID[thread.ProviderCommentID] = resp
		}
	}
	out := make([]mergeRequestEventResponse, 0, len(events))
	for _, event := range events {
		resp := mergeRequestEventResponseFromDB(event)
		if event.EventType == "review_comment" {
			if thread, ok := threadsByProviderID[event.PlatformExternalID]; event.PlatformExternalID != "" && ok {
				resp.DiffThread = &thread
			} else if event.ThreadID != nil {
				if thread, ok := threadsByProviderID[*event.ThreadID]; ok {
					resp.DiffThread = &thread
				}
			}
		}
		out = append(out, resp)
	}
	return out, nil
}

func dbReviewLineRange(input diffReviewLineRange) (db.ReviewLineRange, error) {
	path := strings.TrimSpace(input.Path)
	if path == "" {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range path is required")
	}
	side := strings.ToLower(strings.TrimSpace(input.Side))
	if side != "left" && side != "right" {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range side must be left or right")
	}
	if input.Line <= 0 {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range line must be positive")
	}
	lineType := strings.TrimSpace(input.LineType)
	switch lineType {
	case "context", "add", "delete":
	default:
		return db.ReviewLineRange{}, huma.Error400BadRequest(
			"review range line_type must be context, add, or delete",
		)
	}
	diffHeadSHA := strings.TrimSpace(input.DiffHeadSHA)
	if diffHeadSHA == "" {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range diff_head_sha is required")
	}
	startSide := strings.ToLower(strings.TrimSpace(input.StartSide))
	if input.StartLine != nil && *input.StartLine <= 0 {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range start_line must be positive")
	}
	if (startSide == "") != (input.StartLine == nil) {
		return db.ReviewLineRange{}, huma.Error400BadRequest(
			"review range start_side and start_line must be supplied together",
		)
	}
	if startSide != "" && startSide != side {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range must stay on one side")
	}
	if input.StartLine != nil && *input.StartLine > input.Line {
		return db.ReviewLineRange{}, huma.Error400BadRequest("review range start_line must be before line")
	}
	return db.ReviewLineRange{
		Path:        path,
		OldPath:     strings.TrimSpace(input.OldPath),
		Side:        side,
		StartSide:   startSide,
		StartLine:   input.StartLine,
		Line:        input.Line,
		OldLine:     input.OldLine,
		NewLine:     input.NewLine,
		LineType:    lineType,
		DiffHeadSHA: diffHeadSHA,
		CommitSHA:   strings.TrimSpace(input.CommitSHA),
	}, nil
}

func platformReviewLineRange(input db.ReviewLineRange) platform.DiffReviewLineRange {
	return platform.DiffReviewLineRange{
		Path:        input.Path,
		OldPath:     input.OldPath,
		Side:        input.Side,
		StartSide:   input.StartSide,
		StartLine:   input.StartLine,
		Line:        input.Line,
		OldLine:     input.OldLine,
		NewLine:     input.NewLine,
		LineType:    input.LineType,
		DiffHeadSHA: input.DiffHeadSHA,
		CommitSHA:   input.CommitSHA,
	}
}

func dbReviewLineRangeFromPlatform(input platform.DiffReviewLineRange) db.ReviewLineRange {
	return db.ReviewLineRange{
		Path:        input.Path,
		OldPath:     input.OldPath,
		Side:        input.Side,
		StartSide:   input.StartSide,
		StartLine:   input.StartLine,
		Line:        input.Line,
		OldLine:     input.OldLine,
		NewLine:     input.NewLine,
		LineType:    input.LineType,
		DiffHeadSHA: input.DiffHeadSHA,
		CommitSHA:   input.CommitSHA,
	}
}

func parseReviewLocalID(value, label string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, huma.Error400BadRequest(label + " id must be a positive integer")
	}
	return id, nil
}

func parseReviewAction(value string) (platform.ReviewAction, error) {
	action := platform.ReviewAction(strings.ToLower(strings.TrimSpace(value)))
	switch action {
	case platform.ReviewActionComment, platform.ReviewActionApprove, platform.ReviewActionRequestChanges:
		return action, nil
	default:
		return "", huma.Error400BadRequest("review action must be comment, approve, or request_changes")
	}
}

func reviewActionSupported(caps httpapi.ProviderCapabilitiesResponse, action platform.ReviewAction) bool {
	return slices.Contains(caps.SupportedReviewActions, string(action))
}

// reviewActionsExcludingApprove returns a copy of actions with the approve
// action removed. Used when the viewer authored the merge request so the
// draft tray does not advertise an approval the publish endpoint will reject.
func reviewActionsExcludingApprove(actions []string) []string {
	filtered := make([]string, 0, len(actions))
	for _, action := range actions {
		if action == string(platform.ReviewActionApprove) {
			continue
		}
		filtered = append(filtered, action)
	}
	return filtered
}
