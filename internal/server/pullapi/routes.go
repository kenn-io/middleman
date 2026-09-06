package pullapi

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gh "github.com/google/go-github/v89/github"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/platform/gitealike"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/workspace"
)

var discussionIDPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

const (
	maxFilePreviewBytes          int64 = 4 * 1024 * 1024
	availabilityCodeSelfApproval       = "self_approval"
)

type listPullsInput struct {
	Repo       string `query:"repo" doc:"Repository filter. Accepts provider|platform_host/repo_path, with comma-separated values for multiple repositories."`
	State      string `query:"state"`
	Kanban     string `query:"kanban"`
	Starred    bool   `query:"starred"`
	InvolvesMe bool   `query:"involves_me" doc:"Only include pull requests involving the authenticated viewer."`
	Unassigned bool   `query:"unassigned" doc:"Only include pull requests with no assignees."`
	Q          string `query:"q"`
	Limit      int    `query:"limit"`
	Offset     int    `query:"offset"`
}

type listPullsOutput = httpapi.BodyOutput[[]MergeRequestResponse]

type repoNumberInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
}

type getPullOutput = httpapi.BodyOutput[MergeRequestDetailResponse]

type getMRImportMetadataOutput = httpapi.BodyOutput[mrImportMetadataResponse]

type setKanbanStateInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Status string `json:"status"`
	}
}

type statusOnlyOutput = httpapi.OKStatusOutput

type postCommentInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Body string `json:"body"`
	}
}

type postCommentOutput = httpapi.CreatedOutput[mergeRequestEventResponse]

type editCommentInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	CommentID    int64  `path:"comment_id"`
	Body         struct {
		Body string `json:"body"`
	}
}

type editCommentOutput = httpapi.BodyOutput[mergeRequestEventResponse]

type deleteCommentInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	CommentID    int64  `path:"comment_id"`
}

type deleteCommentOutput struct {
	Status int `status:"204"`
}

type getDiffReviewDraftOutput = httpapi.BodyOutput[diffReviewDraftResponse]

type createDiffReviewDraftCommentInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Body  string              `json:"body"`
		Range diffReviewLineRange `json:"range"`
	}
}

type createDiffReviewDraftCommentOutput = httpapi.CreatedOutput[diffReviewDraftComment]

type editDiffReviewDraftCommentInput struct {
	Provider       string `path:"provider"`
	PlatformHost   string
	Owner          string `path:"owner"`
	Name           string `path:"name"`
	Number         int    `path:"number"`
	DraftCommentID string `path:"draft_comment_id"`
	Body           struct {
		Body  string              `json:"body"`
		Range diffReviewLineRange `json:"range"`
	}
}

type editDiffReviewDraftCommentOutput = httpapi.BodyOutput[diffReviewDraftComment]

type deleteDiffReviewDraftCommentInput struct {
	Provider       string `path:"provider"`
	PlatformHost   string
	Owner          string `path:"owner"`
	Name           string `path:"name"`
	Number         int    `path:"number"`
	DraftCommentID string `path:"draft_comment_id"`
}

type publishDiffReviewDraftInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Body   string `json:"body,omitempty"`
		Action string `json:"action"`
	}
}

type applyReviewSuggestionInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		ExpectedHeadSHA string                             `json:"expected_head_sha,omitempty"`
		Message         string                             `json:"message,omitempty"`
		Suggestions     []applyReviewSuggestionRequestItem `json:"suggestions"`
	}
}

type applyReviewSuggestionRequestItem struct {
	ThreadID    string `json:"thread_id"`
	Replacement string `json:"replacement"`
}

type ApplyReviewSuggestionResponse struct {
	Status    string `json:"status"`
	CommitSHA string `json:"commit_sha,omitempty"`
	CommitURL string `json:"commit_url,omitempty"`
}

type applyReviewSuggestionOutput = httpapi.BodyOutput[ApplyReviewSuggestionResponse]

type discardDiffReviewDraftInput = repoNumberInput

type resolveDiffReviewThreadInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	ThreadID     string `path:"thread_id"`
}

type replyToDiscussionInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	DiscussionID string `path:"discussion_id"`
	Body         struct {
		Body string `json:"body"`
	}
}

type replyToDiscussionOutput = httpapi.CreatedOutput[mergeRequestEventResponse]

type resolveDiscussionInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	DiscussionID string `path:"discussion_id"`
	Body         struct {
		Resolved bool `json:"resolved"`
	}
}

type resolveDiscussionOutput = httpapi.OKStatusOutput

// discussionIDPattern validates GitLab discussion IDs which are 40-char lowercase hex strings.
func validateDiscussionID(discussionID string) error {
	if !discussionIDPattern.MatchString(discussionID) {
		return httpapi.Validation("path.discussion_id", "discussion_id must be a 40-character lowercase hex string")
	}
	return nil
}

type approvePRInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Body string `json:"body"`
		// ExpectedHeadSHA is the provider head the client intends to approve.
		// Current clients capture platform_head_sha when the approval UI opens;
		// if omitted, the server falls back to the best stored provider head
		// for compatibility with older clients.
		ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	}
}

type requestChangesPRInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Body            string `json:"body"`
		ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	}
}

type ActionStatusBody struct {
	Status        string `json:"status"`
	ApprovedCount int    `json:"approved_count,omitempty"`
}

type actionStatusOutput = httpapi.BodyOutput[ActionStatusBody]

type mergePRInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         mergePRInputBody
}

type deferMergePRInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         mergePRInputBody
}

type mergePRInputBody struct {
	CommitTitle   string `json:"commit_title"`
	CommitMessage string `json:"commit_message"`
	Method        string `json:"method"`
	// ExpectedHeadSHA is the reviewed diff head the client rendered.
	// For head-binding providers, merge rejects missing, stale, or
	// mismatched reviewed-head assertions before provider mutation.
	ExpectedHeadSHA   string `json:"expected_head_sha,omitempty"`
	DeleteWorkspaceID string `json:"delete_workspace_id,omitempty"`
	workspaceHostKey  string
}

type mergePRBody struct {
	Merged                  bool   `json:"merged"`
	SHA                     string `json:"sha"`
	Message                 string `json:"message"`
	WorkspaceCleanupPending bool   `json:"workspace_cleanup_pending,omitempty"`
	WorkspaceCleanupWarning string `json:"workspace_cleanup_warning,omitempty"`
}

type mergePROutput = httpapi.BodyOutput[mergePRBody]

type deferMergePRBody struct {
	Status        string `json:"status"`
	PendingChecks int    `json:"pending_checks"`
}

type deferMergePROutput = httpapi.AcceptedBodyOutput[deferMergePRBody]

type editPRContentInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		Title *string `json:"title,omitempty"`
		Body  *string `json:"body,omitempty"`
	}
}

type editPRContentOutput = httpapi.BodyOutput[MergeRequestDetailResponse]

type githubStateInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		State string `json:"state"`
	}
}

type githubStateOutput = httpapi.BodyOutput[httpapi.GithubStateOutputBody]

type listStacksInput struct {
	Repo string `query:"repo"`
}

type listStacksOutput = httpapi.BodyOutput[[]stackResponse]

type getStackForPROutput = httpapi.BodyOutput[stackContextResponse]

func (s *Handler) listPulls(ctx context.Context, input *listPullsInput) (*listPullsOutput, error) {
	query := ListQuery{
		Repo: input.Repo, State: input.State, Kanban: input.Kanban,
		Starred: input.Starred, InvolvesMe: input.InvolvesMe, Unassigned: input.Unassigned, Text: input.Q,
		Limit: input.Limit, Offset: input.Offset,
	}
	var rows []MergeRequestResponse
	var err error
	if _, federationRequest := federationauth.PrincipalFromContext(ctx); federationRequest {
		rows, err = s.ListProviderService(ctx, query)
	} else {
		rows, err = s.ListService(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	return &listPullsOutput{Body: rows}, nil
}

func (s *Handler) listPullsRouteCore(ctx context.Context, input *listPullsInput) (*listPullsOutput, error) {
	if input.State != "" {
		valid := map[string]bool{
			"open": true, "closed": true, "all": true,
		}
		if !valid[input.State] {
			return nil, httpapi.Validation(
				"query.state",
				"state must be one of: open, closed, all",
				"open", "closed", "all",
			)
		}
	}
	if hasInvalidRepoFilter(input.Repo) {
		return nil, httpapi.Validation("query.repo", "repo filter must be provider|platform_host/repo_path")
	}

	snapshot := workspaceapi.WorkspaceSubjectSnapshot{
		OwnReferences: map[db.WorkspaceSubjectKey]workspaceapi.WorkspaceRef{},
		Subjects:      map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{},
	}
	var err error
	if s.workspaceSubjects != nil {
		snapshot, err = s.workspaceSubjects(ctx)
		if err != nil {
			return nil, httpapi.Internal("load workspace activity failed")
		}
	}
	var overrides []db.ItemActivityOverride
	if s.ConfigSnapshot().UseWorkspaceActivityForRecency {
		overrides = make([]db.ItemActivityOverride, 0, len(snapshot.Subjects))
		for key, activity := range snapshot.Subjects {
			if key.ItemType == db.WorkspaceItemTypePullRequest && activity.ActivityAt != nil {
				overrides = append(overrides, db.ItemActivityOverride{
					RepoID: key.RepoID, ItemNumber: key.ItemNumber, ActivityAt: *activity.ActivityAt,
				})
			}
		}
	}
	opts := db.ListMergeRequestsOpts{
		State:             input.State,
		KanbanState:       input.Kanban,
		Starred:           input.Starred,
		Unassigned:        input.Unassigned,
		Search:            input.Q,
		Limit:             input.Limit,
		Offset:            input.Offset,
		RepoFilters:       parseRepoFilters(input.Repo),
		WorkspaceActivity: overrides,
	}
	if input.InvolvesMe {
		if s.viewerLogins == nil {
			return nil, httpapi.Internal("authenticated viewer lookup unavailable")
		}
		opts.ViewerLogins, err = s.viewerLogins(ctx, opts.RepoFilters)
		if err != nil {
			return nil, err
		}
	}

	mrs, err := s.db.ListMergeRequests(ctx, opts)
	if err != nil {
		return nil, httpapi.Internal("list pulls failed")
	}

	repoByID, err := s.lookupRepoMap(ctx)
	if err != nil {
		return nil, httpapi.Internal("repo lookup failed")
	}

	mrIDs := make([]int64, len(mrs))
	for i, mr := range mrs {
		mrIDs[i] = mr.ID
	}
	stackConflictBlocked, err := s.db.ListMRsBlockedByStackConflicts(ctx, mrIDs)
	if err != nil {
		return nil, httpapi.Internal("load stack conflict state failed")
	}
	stackPlacements, err := s.db.ListStackPlacementsForMRs(ctx, mrIDs)
	if err != nil {
		return nil, httpapi.Internal("load stack placements failed")
	}
	links, err := s.db.GetWorktreeLinksForMRs(ctx, mrIDs)
	if err != nil {
		return nil, httpapi.Internal("load worktree links failed")
	}
	linksByMR := indexWorktreeLinksByMR(links, s.selfFleetKey(""))
	out := make([]MergeRequestResponse, 0, len(mrs))
	for _, mr := range mrs {
		rp, ok := repoByID[mr.RepoID]
		if !ok {
			continue
		}
		wl := linksByMR[mr.ID]
		if wl == nil {
			wl = []worktreeLinkResponse{}
		}
		responseMR := mr
		if stackConflictBlocked[mr.ID] {
			responseMR.MergeableState = "dirty"
		}
		responseMR = mergeRequestResponseModel(responseMR)
		key := db.WorkspaceSubjectKey{RepoID: mr.RepoID, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: mr.Number}
		var workspaceRef *workspaceapi.WorkspaceRef
		if activity, ok := snapshot.Subjects[key]; ok {
			copy := activity.Workspace
			workspaceRef = &copy
		}
		resp := MergeRequestResponse{
			MergeRequest:  responseMR,
			Repo:          s.repoRefFromRepo(rp),
			RepoOwner:     rp.Owner,
			RepoName:      rp.Name,
			PlatformHost:  rp.PlatformHost,
			WorktreeLinks: wl,
			Workspace:     workspaceRef,
			DetailLoaded:  mr.DetailFetchedAt != nil,
		}
		if activity, ok := snapshot.Subjects[key]; ok && activity.ActivityAt != nil {
			resp.LastWorkspaceActivityAt = formatUTCRFC3339(*activity.ActivityAt)
		}
		if mr.DetailFetchedAt != nil {
			resp.DetailFetchedAt = formatUTCRFC3339(*mr.DetailFetchedAt)
		}
		if placement, ok := stackPlacements[mr.ID]; ok && placement.Size > 1 {
			resp.Stack = &StackPlacementResponse{Position: placement.Position, Size: placement.Size}
		}
		out = append(out, resp)
	}

	return &listPullsOutput{Body: out}, nil
}

func (s *Handler) getPull(ctx context.Context, input *repoNumberInput) (*getPullOutput, error) {
	item := ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	}
	var body MergeRequestDetailResponse
	var err error
	if _, federationRequest := federationauth.PrincipalFromContext(ctx); federationRequest {
		body, err = s.GetProviderService(ctx, item)
	} else {
		body, err = s.GetService(ctx, item)
	}
	if err != nil {
		return nil, err
	}
	return &getPullOutput{Body: body}, nil
}

func (s *Handler) getPullRouteCore(ctx context.Context, input *repoNumberInput) (*getPullOutput, error) {
	repo, err := s.lookupRepoByProviderRoute(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	body, err := s.buildPullDetailResponse(ctx, mr)
	if err != nil {
		return nil, err
	}
	if err := s.db.RecordHotMergeRequestView(ctx, mr.ID, s.now().UTC()); err != nil {
		slog.Warn("record hot pull request view",
			"merge_request_id", mr.ID,
			"err", err,
		)
	}

	return &getPullOutput{Body: body}, nil
}

func (s *Handler) buildPullDetailResponse(
	ctx context.Context,
	mr *db.MergeRequest,
) (MergeRequestDetailResponse, error) {
	events, err := s.db.ListMREvents(ctx, mr.ID)
	if err != nil {
		return MergeRequestDetailResponse{}, httpapi.Internal("list mr events failed")
	}
	if events == nil {
		events = []db.MREvent{}
	}
	events = withSyntheticMRLifecycleEvents(*mr, events)
	eventResponses, err := s.mergeRequestEventResponses(ctx, mr.ID, events)
	if err != nil {
		return MergeRequestDetailResponse{}, huma.Error500InternalServerError(
			"attach review thread metadata failed",
		)
	}

	dbLinks, err := s.db.GetWorktreeLinksForMR(ctx, mr.ID)
	if err != nil {
		return MergeRequestDetailResponse{}, httpapi.Internal("load worktree links failed")
	}

	repo, err := s.db.GetRepoByID(ctx, mr.RepoID)
	if err != nil || repo == nil {
		return MergeRequestDetailResponse{}, httpapi.Internal("load repo failed")
	}
	resp := MergeRequestDetailResponse{
		Events:               eventResponses,
		Repo:                 s.repoRefWithMergeRequestOperations(ctx, *repo, *mr),
		RepoOwner:            repo.Owner,
		RepoName:             repo.Name,
		PlatformHost:         repo.PlatformHost,
		HeadRepoKind:         classifyHeadRepoKind(*repo, *mr),
		PlatformHeadSHA:      mr.PlatformHeadSHA,
		PlatformBaseSHA:      mr.PlatformBaseSHA,
		ReviewedHeadSHA:      verifiedReviewedHeadSHA(mr),
		DiffHeadSHA:          mr.DiffHeadSHA,
		MergeBaseSHA:         mr.MergeBaseSHA,
		WorktreeLinks:        toWorktreeLinkResponses(dbLinks, s.selfFleetKey("")),
		WorkflowApproval:     s.workflowApprovalState(ctx, repo.Owner, repo.Name, mr),
		Warnings:             s.diffWarnings(mr),
		DetailLoaded:         mr.DetailFetchedAt != nil,
		DeferredMergePending: s.isDeferredMergePending(*repo, mr.Number),
	}
	if mr.DetailFetchedAt != nil {
		resp.DetailFetchedAt = formatUTCRFC3339(*mr.DetailFetchedAt)
	}

	stack, members, err := s.db.GetStackForPRByRepoID(ctx, mr.RepoID, mr.Number)
	if err != nil {
		return MergeRequestDetailResponse{}, httpapi.Internal("get stack for pr failed")
	}
	if stack != nil {
		stackContext := stackContextForPR(mr.Number, stack, members)
		resp.Stack = &stackContext
	}
	responseMR := *mr
	if stack != nil {
		blockedBy := computeConflictBlockedBy(members)
		if _, ok := blockedBy[mr.Number]; ok && mr.State == db.MergeRequestStateOpen {
			responseMR.MergeableState = "dirty"
		}
	}
	responseMR = mergeRequestResponseModel(responseMR)
	resp.MergeRequest = &responseMR

	checks, err := decodeCIChecks(mr.CIChecksJSON)
	if err != nil {
		slog.Warn(
			"decode merge request ci checks for detail failed",
			"merge_request_id", mr.ID, "err", err,
		)
	}
	resp.Checks = checks

	if s.workspaceSubjects != nil {
		snapshot, snapshotErr := s.workspaceSubjects(ctx)
		if snapshotErr != nil {
			slog.Warn(
				"load workspace activity for pull detail failed",
				"merge_request_id", mr.ID, "err", snapshotErr,
			)
		} else {
			key := db.WorkspaceSubjectKey{
				RepoID: mr.RepoID, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: mr.Number,
			}
			if activity, ok := snapshot.Subjects[key]; ok {
				workspaceRef := activity.Workspace
				resp.Workspace = &workspaceRef
			}
		}
	}

	return resp, nil
}

func classifyHeadRepoKind(repo db.Repo, mr db.MergeRequest) HeadRepoKind {
	headRepo := workspace.WorkspaceHeadRepo(
		string(httpapi.ProviderKind(repo)),
		httpapi.ProviderHost(repo),
		repo.Owner,
		repo.Name,
		mr.HeadRepoCloneURL,
	)
	if headRepo == nil {
		return HeadRepoKindSameRepo
	}
	if *headRepo == "" {
		return HeadRepoKindUnknown
	}
	return HeadRepoKindFork
}

// BuildDetail assembles the canonical Pull detail response for adjacent HTTP
// domains such as explicit sync without duplicating Pull presentation logic.
func (s *Handler) BuildDetail(
	ctx context.Context,
	mr *db.MergeRequest,
) (MergeRequestDetailResponse, error) {
	return s.buildPullDetailResponse(ctx, mr)
}

func mergeRequestResponseModel(mr db.MergeRequest) db.MergeRequest {
	mr.KanbanStatus = mergeRequestResponseKanbanStatus(mr)
	return mr
}

func mergeRequestResponseKanbanStatus(mr db.MergeRequest) db.KanbanStatus {
	switch mr.KanbanStatus {
	case db.KanbanStatusNew, db.KanbanStatusReviewing, db.KanbanStatusWaiting, db.KanbanStatusAwaitingMerge:
		return mr.KanbanStatus
	case "":
		return db.KanbanStatusNew
	default:
		slog.Warn("normalizing unexpected kanban status in merge request response", "merge_request_id", mr.ID, "status", mr.KanbanStatus)
		return db.KanbanStatusNew
	}
}

func verifiedReviewedHeadSHA(mr *db.MergeRequest) string {
	if mr == nil || mr.DiffHeadSHA == "" {
		return ""
	}
	if diffSnapshotStale(mr) {
		return ""
	}
	return mr.DiffHeadSHA
}

func diffSnapshotStale(mr *db.MergeRequest) bool {
	shas := diffSHAsForMergeRequest(mr)
	return shas.Stale()
}

func diffSHAsForMergeRequest(mr *db.MergeRequest) db.DiffSHAs {
	return db.DiffSHAs{
		PlatformHeadSHA: mr.PlatformHeadSHA,
		PlatformBaseSHA: mr.PlatformBaseSHA,
		DiffHeadSHA:     mr.DiffHeadSHA,
		DiffBaseSHA:     mr.DiffBaseSHA,
		State:           string(mr.State),
	}
}

func withSyntheticMRLifecycleEvents(mr db.MergeRequest, events []db.MREvent) []db.MREvent {
	hasLifecycleEvent := func(eventType string, createdAt time.Time) bool {
		createdAt = createdAt.UTC()
		for _, event := range events {
			if event.EventType != eventType {
				continue
			}
			if event.CreatedAt.UTC().Equal(createdAt) ||
				(eventType == "merged" && strings.TrimSpace(event.Author) != "") {
				return true
			}
		}
		return false
	}
	hasReopenedEventAfterClose := func(closedAt time.Time) bool {
		closedAt = closedAt.UTC()
		for _, event := range events {
			if event.EventType != "reopened" {
				continue
			}
			eventAt := event.CreatedAt.UTC()
			if eventAt.Equal(closedAt) || eventAt.After(closedAt) {
				return true
			}
		}
		return false
	}

	out := append([]db.MREvent{}, events...)
	switch mr.State {
	case db.MergeRequestStateMerged:
		if mr.MergedAt != nil && !hasLifecycleEvent("merged", *mr.MergedAt) {
			out = append(out, syntheticMRLifecycleEvent(mr, -1, "merged", "merged this", *mr.MergedAt))
		}
	case db.MergeRequestStateClosed:
		if mr.ClosedAt != nil && !hasLifecycleEvent("closed", *mr.ClosedAt) {
			out = append(out, syntheticMRLifecycleEvent(mr, -2, "closed", "closed this", *mr.ClosedAt))
		}
	case db.MergeRequestStateOpen:
		if mr.ClosedAt != nil && !hasReopenedEventAfterClose(*mr.ClosedAt) {
			out = append(out, syntheticMRLifecycleEvent(mr, -3, "reopened", "reopened this", mr.UpdatedAt))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func syntheticMRLifecycleEvent(
	mr db.MergeRequest,
	id int64,
	eventType string,
	summary string,
	createdAt time.Time,
) db.MREvent {
	return db.MREvent{
		ID:             id,
		MergeRequestID: mr.ID,
		EventType:      eventType,
		Summary:        summary,
		CreatedAt:      createdAt.UTC(),
		DedupeKey:      "synthetic:mr:lifecycle:" + eventType,
	}
}

// diffWarnings returns warnings inferred from the persisted PR row. The
// resolveItem and syncPR paths log diff sync failures via slog and (in
// syncPR's case) surface them in the immediate response, but neither
// persists the failure. Without inferring from the row state, a client
// that lands on the PR detail page after resolveItem (which has no
// warnings field) or after a refresh would see no indication that the
// diff is unavailable. We therefore emit a sanitized warning whenever a
// PR that should have diff data is missing it.
func (s *Handler) diffWarnings(mr *db.MergeRequest) []string {
	if mr == nil {
		return nil
	}
	if !s.syncer.HasDiffSync() {
		return nil
	}
	// Closed (including merged) PRs also get diff SHAs populated via
	// fetchAndUpdateClosed, so the warning logic must cover every state
	// that getDiff would render, not just open and merged.
	if mr.DiffHeadSHA == "" {
		return []string{"Diff data is unavailable for this pull request."}
	}
	if diffSnapshotStale(mr) {
		return []string{"Diff data is out of date for this pull request."}
	}
	return nil
}

// workflowApprovalState reads the persisted workflow-approval
// snapshot from the merge request row. Sync (SyncMROnProvider) is
// the only writer; this read path makes no live calls so detail
// GETs stay cheap. The snapshot is keyed by head SHA: if the head
// has moved since the snapshot was taken, treat it as unchecked so
// the UI doesn't render an approve-workflows button against a SHA
// that no longer has pending runs.
func (s *Handler) workflowApprovalState(
	_ context.Context,
	_, _ string,
	mr *db.MergeRequest,
) workflowApprovalResponse {
	if mr == nil {
		return workflowApprovalResponse{}
	}
	// Closed or merged PRs cannot have pending workflow approvals,
	// regardless of what the persisted snapshot says.
	if mr.State != "open" {
		return workflowApprovalResponse{Checked: true}
	}
	if mr.PlatformHeadSHA == "" {
		return workflowApprovalResponse{}
	}
	if mr.WorkflowApprovalCheckedAt == nil ||
		mr.WorkflowApprovalHeadSHA != mr.PlatformHeadSHA {
		return workflowApprovalResponse{}
	}
	return workflowApprovalResponse{
		Checked:  true,
		Required: mr.WorkflowApprovalRequired,
		Count:    mr.WorkflowApprovalCount,
	}
}

func (s *Handler) getMRImportMetadata(
	ctx context.Context, input *repoNumberInput,
) (*getMRImportMetadataOutput, error) {
	repo, err := s.lookupRepoByProviderRoute(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("failed to query merge request")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "merge request not found", nil)
	}
	return &getMRImportMetadataOutput{
		Body: mrImportMetadataResponse{
			Number:           mr.Number,
			HeadBranch:       mr.HeadBranch,
			PlatformHeadSHA:  mr.PlatformHeadSHA,
			HeadRepoCloneURL: mr.HeadRepoCloneURL,
			State:            string(mr.State),
			IsDraft:          mr.IsDraft,
			Title:            mr.Title,
		},
	}, nil
}

func (s *Handler) setKanbanState(ctx context.Context, input *setKanbanStateInput) (*statusOnlyOutput, error) {
	if !validKanbanStates[input.Body.Status] {
		return nil, httpapi.Validation(
			"body.status",
			"status must be one of: new, reviewing, waiting, awaiting_merge",
			"new", "reviewing", "waiting", "awaiting_merge",
		)
	}

	repo, err := s.lookupRepoByProviderRoute(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	ref := repoNumberPathRef{
		repoID:       repo.ID,
		owner:        repo.Owner,
		name:         repo.Name,
		number:       input.Number,
		platformHost: repo.PlatformHost,
	}
	mrID, err := s.lookupMRID(ctx, ref)
	if err != nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, err.Error(), nil)
	}
	if err := s.db.SetKanbanState(ctx, mrID, input.Body.Status); err != nil {
		return nil, httpapi.Internal("set kanban state failed")
	}

	return &statusOnlyOutput{Status: http.StatusOK}, nil
}

func (s *Handler) editPRContent(
	ctx context.Context, input *editPRContentInput,
) (*editPRContentOutput, error) {
	if input.Body.Title == nil && input.Body.Body == nil {
		return nil, httpapi.Validation("body",
			"at least one of title or body must be provided",
		)
	}
	if input.Body.Title != nil && strings.TrimSpace(*input.Body.Title) == "" {
		return nil, httpapi.Validation("body.title", "title must not be blank")
	}

	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityStateMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityStateMutation); err != nil {
		return nil, err
	}

	mutator, err := s.syncer.MergeRequestContentMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityStateMutation)
	}

	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	updatedMR, err := mutator.EditMergeRequestContent(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.Body.Title, input.Body.Body,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"provider API error: "+err.Error(),
		)
	}

	newTitle := mr.Title
	if updatedMR.Title != "" {
		newTitle = updatedMR.Title
	} else if input.Body.Title != nil {
		newTitle = *input.Body.Title
	}
	newBody := mr.Body
	if updatedMR.Body != "" {
		newBody = updatedMR.Body
	} else if input.Body.Body != nil {
		newBody = *input.Body.Body
	}
	updatedAt := updatedMR.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt, err = s.providerMergeRequestUpdatedAt(ctx, *repo, input.Number)
		if err != nil {
			return nil, httpapi.ProviderCallProblemWithDetail(
				err,
				string(repoProviderKind(*repo)), repoProviderHost(*repo),
				"provider omitted merge request updated time and refresh failed",
			)
		}
	}
	if err := s.db.UpdateMRTitleBody(
		ctx, mr.ID, newTitle, newBody, updatedAt,
	); err != nil {
		return nil, httpapi.Internal("update title/body failed")
	}

	mr, err = s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil || mr == nil {
		return nil, httpapi.Internal("re-read pull request failed")
	}

	body, err := s.buildPullDetailResponse(ctx, mr)
	if err != nil {
		return nil, err
	}

	return &editPRContentOutput{Body: body}, nil
}

func (s *Handler) postComment(ctx context.Context, input *postCommentInput) (*postCommentOutput, error) {
	if strings.TrimSpace(input.Body.Body) == "" {
		return nil, httpapi.Validation("body.body", "comment body must not be empty")
	}

	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}
	mr, err := s.requireVisibleMergeRequest(ctx, repo, input.Number)
	if err != nil {
		return nil, err
	}

	mutator, err := s.syncer.CommentMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityCommentMutation)
	}

	platformEvent, err := mutator.CreateMergeRequestComment(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.Body.Body,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"create comment on provider failed",
		)
	}

	event := platform.DBMREvent(mr.ID, platformEvent)
	if err := s.db.UpsertMREvents(ctx, []db.MREvent{event}); err != nil {
		_ = err
	}

	return &postCommentOutput{Status: http.StatusCreated, Body: mergeRequestEventResponseFromDB(event)}, nil
}

func (s *Handler) editComment(ctx context.Context, input *editCommentInput) (*editCommentOutput, error) {
	if strings.TrimSpace(input.Body.Body) == "" {
		return nil, httpapi.Validation("body.body", "comment body must not be empty")
	}

	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}

	mutator, err := s.syncer.CommentMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityCommentMutation)
	}

	ref := repoNumberPathRef{
		repoID:       repo.ID,
		owner:        repo.Owner,
		name:         repo.Name,
		number:       input.Number,
		platformHost: repo.PlatformHost,
	}
	mrID, err := s.lookupMRID(ctx, ref)
	if err != nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, err.Error(), nil)
	}

	exists, err := s.db.MRCommentEventExists(ctx, mrID, input.CommentID)
	if err != nil {
		return nil, httpapi.Internal("validate comment target failed")
	}
	if !exists {
		return nil, httpapi.NotFound(httpapi.CodeCommentNotFound, "comment not found for pull request", nil)
	}

	platformEvent, err := mutator.EditMergeRequestComment(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.CommentID, input.Body.Body,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"edit comment on provider failed",
		)
	}
	platformEvent.MergeRequestNumber = input.Number

	event := platform.DBMREvent(mrID, platformEvent)
	existingEvents, err := s.db.ListMREvents(ctx, mrID)
	if err != nil {
		return nil, httpapi.Internal("load existing comment metadata failed")
	}
	for _, existing := range existingEvents {
		if existing.EventType == "issue_comment" && existing.PlatformID != nil &&
			*existing.PlatformID == input.CommentID {
			event.MetadataJSON = platform.PreserveProviderHiddenMetadata(
				existing.MetadataJSON, event.MetadataJSON,
			)
			break
		}
	}
	if err := s.db.UpsertMREvents(ctx, []db.MREvent{event}); err != nil {
		return nil, httpapi.Internal("persist edited comment failed")
	}

	return &editCommentOutput{Body: mergeRequestEventResponseFromDB(event)}, nil
}

func (s *Handler) deleteComment(ctx context.Context, input *deleteCommentInput) (*deleteCommentOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityCommentMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityCommentMutation); err != nil {
		return nil, err
	}
	mutator, err := s.syncer.CommentMutator(repoProviderKind(*repo), repoProviderHost(*repo))
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityCommentMutation)
	}
	ref := repoNumberPathRef{
		repoID: repo.ID, owner: repo.Owner, name: repo.Name,
		number: input.Number, platformHost: repo.PlatformHost,
	}
	mrID, err := s.lookupMRID(ctx, ref)
	if err != nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, err.Error(), nil)
	}
	commentExists, err := s.db.MRCommentEventExists(ctx, mrID, input.CommentID)
	if err != nil {
		return nil, httpapi.Internal("validate comment target failed")
	}
	if !commentExists {
		return nil, httpapi.NotFound(httpapi.CodeCommentNotFound, "comment not found for pull request", nil)
	}
	if err := mutator.DeleteMergeRequestComment(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.CommentID,
	); err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err, string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"delete comment on provider failed",
		)
	}
	return &deleteCommentOutput{Status: http.StatusNoContent}, nil
}

func (s *Handler) replyToDiscussion(ctx context.Context, input *replyToDiscussionInput) (*replyToDiscussionOutput, error) {
	if strings.TrimSpace(input.Body.Body) == "" {
		return nil, httpapi.Validation("body.body", "reply body must not be empty")
	}

	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityThreadReply,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityThreadReply); err != nil {
		return nil, err
	}

	// Verify the MR exists locally before calling the provider to avoid
	// creating upstream replies for untracked or non-existent PRs.
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	provider, err := s.syncer.Registry().Provider(repoProviderKind(*repo), repoProviderHost(*repo))
	if err != nil {
		return nil, httpapi.Internal("provider lookup failed")
	}

	replier, ok := provider.(platform.ThreadReplier)
	if !ok {
		caps := provider.Capabilities()
		if !caps.ThreadReply {
			return nil, unsupportedCapabilityProblem(*repo, capabilityThreadReply)
		}
		return nil, httpapi.Internal("provider does not implement ThreadReplier")
	}

	providerDiscussionID := input.DiscussionID
	eventThreadID := ""
	if threadID, parseErr := strconv.ParseInt(input.DiscussionID, 10, 64); parseErr == nil && threadID > 0 {
		thread, err := s.db.GetMRReviewThread(ctx, mr.ID, threadID)
		if err != nil {
			return nil, httpapi.Internal("get review thread failed")
		}
		if thread == nil {
			if repoProviderKind(*repo) == platform.KindGitHub {
				return nil, httpapi.NotFound(httpapi.CodeNotFound, "review thread not found", nil)
			}
			if err := validateDiscussionID(input.DiscussionID); err != nil {
				return nil, err
			}
		} else if repoProviderKind(*repo) == platform.KindGitHub {
			if strings.TrimSpace(thread.ProviderCommentID) == "" {
				return nil, httpapi.Internal("review thread is missing provider comment id")
			}
			if strings.TrimSpace(thread.ProviderThreadID) == "" {
				return nil, httpapi.Internal("review thread is missing provider thread id")
			}
			providerDiscussionID = thread.ProviderCommentID
			eventThreadID = thread.ProviderThreadID
		} else {
			if strings.TrimSpace(thread.ProviderThreadID) == "" {
				return nil, httpapi.Internal("review thread is missing provider thread id")
			}
			providerDiscussionID = thread.ProviderThreadID
		}
	} else if repoProviderKind(*repo) == platform.KindGitHub {
		if _, err := parseReviewLocalID(input.DiscussionID, "review thread"); err != nil {
			return nil, err
		}
	} else {
		if err := validateDiscussionID(input.DiscussionID); err != nil {
			return nil, err
		}
	}

	platformEvent, err := replier.ReplyToThread(
		ctx, platformRepoRefFromDB(*repo), input.Number, providerDiscussionID, input.Body.Body,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"reply to discussion on provider failed",
		)
	}
	if eventThreadID != "" {
		platformEvent.ThreadID = eventThreadID
	}

	event := platform.DBMREvent(mr.ID, platformEvent)
	providerUpdatedAt, activityErr := s.providerMergeRequestUpdatedAt(ctx, *repo, input.Number)
	if activityErr != nil {
		slog.WarnContext(ctx, "failed to refresh pull request activity after discussion reply",
			"mr_id", mr.ID, "discussion_id", input.DiscussionID, "error", activityErr)
		if err := s.db.UpsertMREvents(ctx, []db.MREvent{event}); err != nil {
			slog.ErrorContext(ctx, "failed to persist discussion reply event",
				"mr_id", mr.ID, "discussion_id", input.DiscussionID, "error", err)
			return nil, httpapi.Internal("failed to persist reply event")
		}
		s.syncAfterReviewDraftPublish(*repo, input.Number)
		return &replyToDiscussionOutput{Status: http.StatusCreated, Body: mergeRequestEventResponseFromDB(event)}, nil
	}
	applied, err := s.db.CommitMergeRequestChildSnapshot(ctx, db.MergeRequestChildSnapshot{
		MergeRequestID:    mr.ID,
		ExpectedRevision:  mr.SnapshotRevision,
		ProviderUpdatedAt: &providerUpdatedAt,
		OtherEvents:       []db.MREvent{event},
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to persist discussion reply event",
			"mr_id", mr.ID, "discussion_id", input.DiscussionID, "error", err)
		return nil, httpapi.Internal("failed to persist reply event")
	}
	if !applied {
		s.syncAfterReviewDraftPublish(*repo, input.Number)
	}

	return &replyToDiscussionOutput{Status: http.StatusCreated, Body: mergeRequestEventResponseFromDB(event)}, nil
}

func (s *Handler) resolveDiscussion(ctx context.Context, input *resolveDiscussionInput) (*resolveDiscussionOutput, error) {
	if err := validateDiscussionID(input.DiscussionID); err != nil {
		return nil, err
	}

	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityThreadResolve,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityThreadResolve); err != nil {
		return nil, err
	}

	// Verify the MR exists locally before calling the provider to avoid
	// resolving discussions on untracked or non-existent PRs.
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	provider, err := s.syncer.Registry().Provider(repoProviderKind(*repo), repoProviderHost(*repo))
	if err != nil {
		return nil, httpapi.Internal("provider lookup failed")
	}

	resolver, ok := provider.(platform.ThreadResolver)
	if !ok {
		caps := provider.Capabilities()
		if !caps.ThreadResolve {
			return nil, unsupportedCapabilityProblem(*repo, capabilityThreadResolve)
		}
		return nil, httpapi.Internal("provider does not implement ThreadResolver")
	}

	if err := resolver.ResolveThread(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.DiscussionID, input.Body.Resolved,
	); err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"resolve discussion on provider failed",
		)
	}

	// Update local discussion events' resolved state to keep dashboard in sync.
	if err := s.db.UpdateThreadResolved(ctx, mr.ID, input.DiscussionID, input.Body.Resolved); err != nil {
		slog.ErrorContext(ctx, "failed to update local discussion resolved state",
			"mr_id", mr.ID, "discussion_id", input.DiscussionID, "error", err)
		// Don't fail the request since the upstream mutation succeeded;
		// the state will be corrected on the next sync.
	}

	return &resolveDiscussionOutput{Status: http.StatusOK}, nil
}

func (s *Handler) approvePR(ctx context.Context, input *approvePRInput) (*actionStatusOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReviewMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityReviewMutation); err != nil {
		return nil, err
	}
	mutator, err := s.syncer.ReviewMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityReviewMutation)
	}

	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}
	if s.mergeRequestAuthoredByViewer(ctx, *repo, *mr) {
		return nil, selfApprovalProblem(*repo)
	}

	expectedHeadSHA := approvalReviewHeadSHA(mr, input.Body.ExpectedHeadSHA)

	platformEvent, err := mutator.ApproveMergeRequest(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.Body.Body,
		expectedHeadSHA,
	)
	if err != nil {
		if errors.Is(err, platform.ErrStaleState) {
			// The MR head moved past the requested approval target; refresh
			// local state so the user retries against the current head.
			s.runBackground(func(bgCtx context.Context) {
				if syncErr := s.syncer.SyncMROnProvider(
					bgCtx,
					repoProviderKind(*repo), repoProviderHost(*repo),
					repo.Owner, repo.Name, input.Number,
				); syncErr != nil {
					slog.Warn("background sync after stale approval", "err", syncErr)
				}
			})
		}
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"provider API error",
		)
	}

	event := platform.DBMREvent(mr.ID, platformEvent)
	_ = s.db.UpsertMREvents(ctx, []db.MREvent{event})

	if syncErr := s.syncer.SyncMROnProvider(
		ctx,
		repoProviderKind(*repo), repoProviderHost(*repo),
		repo.Owner, repo.Name, input.Number,
	); syncErr != nil {
		slog.Warn("sync after approval", "err", syncErr)
	}

	return &actionStatusOutput{Body: ActionStatusBody{Status: "approved"}}, nil
}

func (s *Handler) requestChangesPR(ctx context.Context, input *requestChangesPRInput) (*actionStatusOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReviewMutation,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityReviewMutation); err != nil {
		return nil, err
	}
	caps := s.capabilitiesForRepo(*repo)
	if !reviewActionSupported(caps, platform.ReviewActionRequestChanges) {
		return nil, httpapi.UnsupportedCapability(*repo, "review_action_request_changes")
	}
	body := strings.TrimSpace(input.Body.Body)
	if body == "" {
		return nil, huma.Error400BadRequest("request changes review body is required")
	}

	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}
	if s.mergeRequestAuthoredByViewer(ctx, *repo, *mr) {
		return nil, httpapi.Forbidden(
			"You cannot request changes on your own pull request",
			map[string]any{"reason": availabilityCodeSelfApproval, "provider": string(repoProviderKind(*repo)), "platformHost": repoProviderHost(*repo)},
		)
	}
	mutator, err := s.syncer.RequestChangesMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*repo, "review_action_request_changes")
	}
	expectedHeadSHA := approvalReviewHeadSHA(mr, input.Body.ExpectedHeadSHA)
	err = mutator.RequestChanges(ctx, platformRepoRefFromDB(*repo), input.Number, body, expectedHeadSHA)
	if err != nil {
		if errors.Is(err, platform.ErrStaleState) {
			s.syncAfterReviewDraftPublish(*repo, input.Number)
		}
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"provider API error",
		)
	}

	if syncErr := s.syncer.SyncMROnProvider(
		ctx,
		repoProviderKind(*repo), repoProviderHost(*repo),
		repo.Owner, repo.Name, input.Number,
	); syncErr != nil {
		slog.Warn("sync after requesting changes", "err", syncErr)
	}
	return &actionStatusOutput{Body: ActionStatusBody{Status: "changes_requested"}}, nil
}

// approvalReviewHeadSHA resolves the provider commit to attach a direct
// review to. Direct /approve and /request-changes are provider-head
// mutations sharing this resolution on purpose: both come from the same
// review form, and a change request must not be pinned more strictly (or
// more loosely) than an approval. Clients should send the head captured
// when the review UI opened, normally platform_head_sha. Omitting the pin
// is a compatibility path for older clients; in that case kenn-forge binds
// the review to the best stored provider head rather than rejecting the
// request. Stale supplied pins are delegated to provider head-binding where
// available and mapped through the normal stale_state path. Merge and
// draft-review publish use reviewedHeadSHA instead because those paths
// require a verified diff snapshot.
func approvalReviewHeadSHA(mr *db.MergeRequest, clientSHA string) string {
	if sha := strings.TrimSpace(clientSHA); sha != "" {
		return sha
	}
	if mr == nil {
		return ""
	}
	if mr.DiffHeadSHA != "" && !diffSnapshotStale(mr) {
		return mr.DiffHeadSHA
	}
	return mr.PlatformHeadSHA
}

func (s *Handler) approveWorkflows(ctx context.Context, input *repoNumberInput) (*actionStatusOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityWorkflowApproval,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityWorkflowApproval); err != nil {
		return nil, err
	}
	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	client, err := s.syncer.DirectClientForHost(repo.PlatformHost)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityWorkflowApproval)
	}
	mutator, err := s.syncer.WorkflowApprovalMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityWorkflowApproval)
	}

	pr, err := client.GetPullRequest(ctx, input.Owner, input.Name, input.Number)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"GitHub API error",
		)
	}
	if pr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}

	headSHA := pr.GetHead().GetSHA()
	if pr.GetState() != "open" || headSHA == "" {
		return &actionStatusOutput{Body: ActionStatusBody{Status: "approved_workflows"}}, nil
	}

	runs, err := client.ListWorkflowRunsForHeadSHA(ctx, input.Owner, input.Name, headSHA)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"GitHub API error",
		)
	}
	pending := ghclient.FilterWorkflowRunsAwaitingApproval(runs, ghclient.PRSource{
		Number:           input.Number,
		HeadSHA:          headSHA,
		HeadRepoFullName: pr.GetHead().GetRepo().GetFullName(),
		HeadRef:          pr.GetHead().GetRef(),
	})

	approvedCount := 0
	for _, run := range pending {
		if err := mutator.ApproveWorkflow(
			ctx, platformRepoRefFromDB(*repo), strconv.FormatInt(run.GetID(), 10),
		); err != nil {
			if approvedCount > 0 {
				if syncErr := s.syncer.SyncMROnProvider(
					context.WithoutCancel(ctx),
					repoProviderKind(*repo), repoProviderHost(*repo),
					repo.Owner, repo.Name, input.Number,
				); syncErr != nil {
					slog.Warn("sync after workflow approval failure", "err", syncErr)
				}
			}
			return nil, httpapi.ProviderCallProblemWithDetail(
				err,
				string(repoProviderKind(*repo)), repoProviderHost(*repo),
				err.Error(),
			)
		}
		approvedCount++
	}

	if syncErr := s.syncer.SyncMROnProvider(
		context.WithoutCancel(ctx),
		repoProviderKind(*repo), repoProviderHost(*repo),
		repo.Owner, repo.Name, input.Number,
	); syncErr != nil {
		slog.Warn("sync after workflow approval", "err", syncErr)
	}
	if err := s.db.UpdateMRWorkflowApproval(
		ctx, repo.ID, input.Number, s.now().UTC(), headSHA, false, 0,
	); err != nil {
		slog.Warn("clear workflow approval state after approval",
			"repo", repo.Owner+"/"+repo.Name,
			"number", input.Number,
			"err", err,
		)
	}

	return &actionStatusOutput{Body: ActionStatusBody{
		Status:        "approved_workflows",
		ApprovedCount: approvedCount,
	}}, nil
}

func (s *Handler) readyForReview(ctx context.Context, input *repoNumberInput) (*actionStatusOutput, error) {
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		capabilityReadyForReview,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityReadyForReview); err != nil {
		return nil, err
	}
	if _, err := s.requireVisibleMergeRequest(ctx, repo, input.Number); err != nil {
		return nil, err
	}
	mutator, err := s.syncer.ReadyForReviewMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityReadyForReview)
	}
	pr, err := mutator.MarkReadyForReview(ctx, platformRepoRefFromDB(*repo), input.Number)
	if err != nil {
		type readyForReviewFailure interface {
			StatusCode() int
			IsStaleState() bool
		}

		var readyErr readyForReviewFailure
		var ghErr *gh.ErrorResponse
		staleState := errors.As(err, &readyErr) && readyErr != nil && readyErr.IsStaleState()
		if !staleState {
			staleState = errors.As(err, &ghErr) && ghErr != nil && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
		}
		if staleState {
			if syncErr := s.syncer.SyncMROnProvider(
				context.WithoutCancel(ctx),
				repoProviderKind(*repo), repoProviderHost(*repo),
				repo.Owner, repo.Name, input.Number,
			); syncErr != nil {
				slog.Warn(
					"sync after ready for review stale state failed",
					"owner", input.Owner,
					"repo", input.Name,
					"number", input.Number,
					"err", syncErr,
				)
			} else {
				return &actionStatusOutput{Body: ActionStatusBody{Status: "ready_for_review"}}, nil
			}
		}
		slog.Warn(
			"ready for review failed",
			"owner", input.Owner,
			"repo", input.Name,
			"number", input.Number,
			"err", err,
		)
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			err.Error(),
		)
	}
	if pr.Number == 0 {
		return nil, httpapi.Upstream(
			"provider API returned no pull request",
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
		)
	}

	normalized := platform.DBMergeRequest(repo.ID, pr)
	if mrID, _, accepted, upsertErr := s.syncer.CommitMergeRequestParentSnapshot(
		ctx, mergeRequestRepoRef(*repo), normalized,
	); upsertErr == nil && accepted {
		_ = s.db.EnsureKanbanState(ctx, mrID)
	}

	return &actionStatusOutput{Body: ActionStatusBody{Status: "ready_for_review"}}, nil
}

func (s *Handler) mergePR(ctx context.Context, input *mergePRInput) (*mergePROutput, error) {
	result, err := s.mergePRWithBody(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Number, input.Body)
	if err != nil {
		return nil, err
	}
	return &mergePROutput{Body: result}, nil
}

func (s *Handler) mergePRWithBody(
	ctx context.Context,
	provider string,
	platformHost string,
	owner string,
	name string,
	number int,
	body mergePRInputBody,
) (mergePRBody, error) {
	body.bindWorkspaceHost(ctx)
	repo, err := s.requireRepoRouteCapability(
		ctx,
		provider, platformHost, owner, name,
		capabilityMergeMutation,
	)
	if err != nil {
		return mergePRBody{}, err
	}
	if err := s.requireSyncerCapability(*repo, capabilityMergeMutation); err != nil {
		return mergePRBody{}, err
	}
	mutator, err := s.syncer.MergeMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return mergePRBody{}, unsupportedCapabilityProblem(*repo, capabilityMergeMutation)
	}

	mr, err := s.visibleMergeRequest(ctx, repo.ID, number)
	if err != nil {
		return mergePRBody{}, httpapi.Internal("get pull request failed")
	}
	if mr == nil {
		return mergePRBody{}, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}
	if err := s.requireMidStackMergeAllowed(ctx, repo.ID, number); err != nil {
		return mergePRBody{}, err
	}
	expectedHeadSHA, err := s.preflightMergePR(repo, mr, number, body)
	if err != nil {
		return mergePRBody{}, err
	}

	result, err := mutator.MergeMergeRequest(
		ctx,
		platformRepoRefFromDB(*repo),
		number,
		body.CommitTitle,
		body.CommitMessage,
		body.Method,
		expectedHeadSHA,
	)
	if err != nil {
		if status, message, ok := mergeHTTPErrorStatus(err); ok {
			slog.Error("provider merge failed",
				"owner", owner, "repo", name,
				"number", number, "method", body.Method,
				"status", status,
				"message", message,
				"err", err)

			if status == http.StatusMethodNotAllowed || status == http.StatusConflict {
				reason := "conflict"
				if errors.Is(err, platform.ErrStaleState) {
					reason = "stale_state"
				}
				// Resync on stale heads (the user must re-review current
				// state) and on providers without hard head binding (the
				// refresh only improves the local mergeable view). For
				// head-binding providers a generic-conflict resync would
				// persist a newer head and let a retry from the same stale
				// UI mutate a commit nobody reviewed.
				if reason == "stale_state" || !s.capabilitiesForRepo(*repo).MutationHeadBinding {
					s.runBackground(func(bgCtx context.Context) {
						if syncErr := s.syncer.SyncMROnProvider(
							bgCtx,
							repoProviderKind(*repo), repoProviderHost(*repo),
							repo.Owner, repo.Name, number,
						); syncErr != nil {
							slog.Warn("background sync after merge failure", "err", syncErr)
						}
					})
				}
				return mergePRBody{}, httpapi.Conflict(httpapi.CodeConflict, message, map[string]any{"reason": reason})
			}

			// Forward 4xx provider errors as-is so the user sees the real cause
			// (e.g. 422 validation, 403 forbidden). 5xx becomes 502.
			if status >= 400 && status < 500 {
				return mergePRBody{}, httpapi.NewProblem(status, httpapi.CodeForStatus(status), message, nil)
			}
			return mergePRBody{}, httpapi.Upstream(
				"provider merge error: "+message,
				string(repoProviderKind(*repo)), repoProviderHost(*repo),
			)
		}
		slog.Error("provider merge transport error",
			"owner", owner, "repo", name,
			"number", number, "method", body.Method,
			"err", err)
		return mergePRBody{}, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"provider merge error: "+err.Error(),
		)
	}

	workspaceCleanup := workspaceCleanupResult{}
	if result.Merged {
		// Record the transition through the same close-detection flow the
		// periodic sync uses rather than an eager local state write. The sync
		// fetches the provider's own merged snapshot — state, provider
		// timestamps, and the merging actor — and commits it through the
		// parent-snapshot choke point, which finalizes commit liveness inside
		// the same transaction. An eager write would suppress that transition
		// and advance updated_at past the provider's, leaving later resyncs
		// rejected as stale. If this sync fails, the row stays open until the
		// next periodic round repairs it through the same path; the response
		// below still reports the provider's merge result.
		if syncErr := s.syncer.SyncClosedMROnProvider(
			ctx, repo.ID, number,
		); syncErr != nil {
			slog.Warn("sync after merge",
				"owner", repo.Owner, "repo", repo.Name,
				"number", number, "err", syncErr)
		}
		s.markClosedLinkedNotificationsDone(ctx)

		// The merge landed, so any deferred merge still queued for this pull
		// request is superseded: its worker stands down silently instead of
		// later reporting a failure for a pull request that is already merged.
		// (A deferred worker completing through this same path supersedes its
		// own handle, which is a no-op by the time it broadcasts completion.)
		s.supersedeDeferredMerge(deferredMergeKey(*repo, number))
		workspaceCleanup = s.queueMergedWorkspaceCleanup(
			ctx, body.workspaceHostKey, body.DeleteWorkspaceID,
		)
	}

	return mergePRBody{
		Merged:                  result.Merged,
		SHA:                     result.SHA,
		Message:                 result.Message,
		WorkspaceCleanupPending: workspaceCleanup.Pending,
		WorkspaceCleanupWarning: workspaceCleanup.Warning,
	}, nil
}

func (s *Handler) requireMidStackMergeAllowed(ctx context.Context, repoID int64, number int) error {
	if s.allowMidStackMerges() {
		return nil
	}

	stack, members, err := s.db.GetStackForPRByRepoID(ctx, repoID, number)
	if err != nil {
		return httpapi.Internal("get stack for pull request failed")
	}
	if stack == nil {
		return nil
	}

	for _, member := range members {
		if member.Number == number {
			return nil
		}
		if member.State != string(db.MergeRequestStateMerged) {
			return httpapi.Conflict(
				httpapi.CodeConflict,
				"mid-stack merges are disabled; merge the bottom unmerged branch first",
				map[string]any{
					"reason":          "mid_stack_merge_disallowed",
					"blocking_number": member.Number,
				},
			)
		}
	}
	return nil
}

func (s *Handler) preflightMergePR(
	repo *db.Repo,
	mr *db.MergeRequest,
	number int,
	body mergePRInputBody,
) (string, error) {
	if mr.State == db.MergeRequestStateClosed || mr.State == db.MergeRequestStateMerged {
		return "", httpapi.Conflict(
			httpapi.CodeConflict,
			"pull request is not open",
			map[string]any{"reason": "not_open"},
		)
	}
	validMethods := map[string]bool{"merge": true, "squash": true, "rebase": true}
	if !validMethods[body.Method] {
		return "", httpapi.Validation(
			"body.method",
			"invalid merge method: must be merge, squash, or rebase",
			"merge", "squash", "rebase",
		)
	}

	// Bind the merge to the head commit the user reviewed locally so a
	// source-branch push between review and merge is rejected upstream
	// instead of merging unreviewed code.
	expectedHeadSHA, err := s.reviewedHeadSHA(repo, mr)
	if err != nil {
		return "", err
	}
	// Head-binding providers require the client to pin the head it
	// rendered: an omitted pin would silently bind to whatever the cache
	// holds now, which may be newer than what the user reviewed.
	if strings.TrimSpace(body.ExpectedHeadSHA) == "" &&
		s.capabilitiesForRepo(*repo).MutationHeadBinding {
		return "", httpapi.Validation(
			"body.expected_head_sha",
			"required for this provider: echo the platform_head_sha you rendered",
		)
	}
	if err := s.verifyClientReviewedHead(
		repo, number, body.ExpectedHeadSHA, expectedHeadSHA,
	); err != nil {
		return "", err
	}
	return expectedHeadSHA, nil
}

// reviewedHeadSHA resolves the head commit a mutation should be pinned
// to. For head-bound providers, this is the verified diff snapshot head,
// not the mutable platform head row: if diff sync is missing or stale,
// kenn-forge cannot prove the user reviewed the current code and must
// fail closed. Deliberately, this path never refreshes a missing diff,
// because persisting a fresh head here could arm a retry from the same
// stale UI to mutate a commit nobody reviewed.
func (s *Handler) reviewedHeadSHA(
	repo *db.Repo,
	mr *db.MergeRequest,
) (string, error) {
	if !s.capabilitiesForRepo(*repo).MutationHeadBinding {
		return mr.PlatformHeadSHA, nil
	}
	if mr.DiffHeadSHA == "" {
		return "", httpapi.Conflict(
			httpapi.CodeConflict,
			"reviewed diff data is unavailable for this pull request; refresh and re-review it once diff sync completes",
			map[string]any{"reason": "head_unknown"},
		)
	}
	if diffSnapshotStale(mr) {
		s.runBackground(func(bgCtx context.Context) {
			if syncErr := s.syncer.SyncMROnProvider(
				bgCtx,
				repoProviderKind(*repo), repoProviderHost(*repo),
				repo.Owner, repo.Name, mr.Number,
			); syncErr != nil {
				slog.Warn("background sync after stale reviewed diff", "err", syncErr)
			}
		})
		return "", httpapi.Conflict(
			httpapi.CodeConflict,
			"reviewed diff data is out of date for this pull request; refresh and re-review it",
			map[string]any{"reason": "stale_state"},
		)
	}
	return mr.DiffHeadSHA, nil
}

// verifyClientReviewedHead enforces the client's optional assertion of
// the head commit it rendered. The locally stored head is only a cache —
// any sync between render and click can move it — so when the client
// supplies expected_head_sha it must match the head the mutation will
// bind to, or the action is rejected before reaching the provider.
func (s *Handler) verifyClientReviewedHead(
	repo *db.Repo,
	number int,
	clientSHA, boundSHA string,
) error {
	err := verifyClientReviewedHeadWithoutRefresh(clientSHA, boundSHA)
	if err == nil || strings.TrimSpace(clientSHA) == "" || strings.TrimSpace(boundSHA) == "" ||
		strings.TrimSpace(clientSHA) == strings.TrimSpace(boundSHA) {
		return err
	}
	s.runBackground(func(bgCtx context.Context) {
		if syncErr := s.syncer.SyncMROnProvider(
			bgCtx,
			repoProviderKind(*repo), repoProviderHost(*repo),
			repo.Owner, repo.Name, number,
		); syncErr != nil {
			slog.Warn("background sync after stale client head", "err", syncErr)
		}
	})
	return err
}

func verifyClientReviewedHeadWithoutRefresh(clientSHA, boundSHA string) error {
	clientSHA = strings.TrimSpace(clientSHA)
	boundSHA = strings.TrimSpace(boundSHA)
	if clientSHA == "" {
		return nil
	}
	if boundSHA == "" {
		return httpapi.Conflict(
			httpapi.CodeConflict,
			"merge request head commit has not been synced; re-review it once the next sync completes",
			map[string]any{"reason": "head_unknown"},
		)
	}
	if clientSHA != boundSHA {
		return httpapi.Conflict(
			httpapi.CodeConflict,
			"target changed since it was reviewed; refresh and retry",
			map[string]any{"reason": "stale_state"},
		)
	}
	return nil
}

func mergeHTTPErrorStatus(err error) (int, string, bool) {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr != nil && ghErr.Response != nil {
		return ghErr.Response.StatusCode, githubErrorResponseMessage(err, ghErr), true
	}
	var httpErr *gitealike.HTTPError
	if errors.As(err, &httpErr) && httpErr != nil && httpErr.StatusCode != 0 {
		return httpErr.StatusCode, httpErr.Error(), true
	}
	var gitlabErr *gitlabapi.ErrorResponse
	if errors.As(err, &gitlabErr) && gitlabErr != nil && gitlabErr.Response != nil {
		return gitlabErr.Response.StatusCode, gitlabErr.Message, true
	}
	return 0, "", false
}

func githubErrorResponseMessage(err error, ghErr *gh.ErrorResponse) string {
	message := strings.TrimSpace(ghErr.Message)
	details := make([]string, 0, len(ghErr.Errors))
	seen := make(map[string]bool, len(ghErr.Errors)+1)
	if message != "" {
		seen[message] = true
	}
	for _, apiErr := range ghErr.Errors {
		detail := strings.TrimSpace(apiErr.Message)
		if detail == "" && strings.TrimSpace(apiErr.Code) != "" {
			detail = strings.TrimSpace(apiErr.Error())
		}
		if detail == "" || seen[detail] {
			continue
		}
		seen[detail] = true
		details = append(details, detail)
	}

	if len(details) > 0 {
		joined := strings.Join(details, "; ")
		if message == "" || isGenericGitHubErrorMessage(message, ghErr.Response.StatusCode) {
			return joined
		}
		return message + ": " + joined
	}
	if message != "" {
		return message
	}
	if err != nil {
		return err.Error()
	}
	return "GitHub API error"
}

func isGenericGitHubErrorMessage(message string, status int) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return normalized == "github server error" ||
		normalized == "server error" ||
		normalized == strings.ToLower(http.StatusText(status))
}

func (s *Handler) setPRGitHubState(
	ctx context.Context, input *githubStateInput,
) (*githubStateOutput, error) {
	if input.Body.State != "open" && input.Body.State != "closed" && input.Body.State != "draft" {
		return nil, httpapi.Validation(
			"body.state",
			"state must be 'open', 'closed', or 'draft'",
			"open", "closed", "draft",
		)
	}

	requiredCapability := capabilityStateMutation
	if input.Body.State == "draft" {
		requiredCapability = capabilityDraftMutation
	}
	repo, err := s.requireRepoRouteCapability(
		ctx,
		input.Provider, input.PlatformHost, input.Owner, input.Name,
		requiredCapability,
	)
	if err != nil {
		return nil, err
	}
	if err := s.requireSyncerCapability(*repo, requiredCapability); err != nil {
		return nil, err
	}

	mr, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get pull request: " + err.Error())
	}
	if mr == nil {
		return nil, httpapi.NotFound(httpapi.CodePullNotFound, "pull request not found", nil)
	}
	if mr.State == "merged" {
		return nil, httpapi.Conflict(
			httpapi.CodeConflict,
			"cannot change state of a merged pull request",
			nil,
		)
	}
	if input.Body.State == "draft" {
		mutator, err := s.syncer.DraftMutator(
			repoProviderKind(*repo), repoProviderHost(*repo),
		)
		if err != nil {
			return nil, unsupportedCapabilityProblem(*repo, capabilityDraftMutation)
		}
		providerUpdatedAt, err := mutator.ConvertMergeRequestToDraft(
			ctx, platformRepoRefFromDB(*repo), input.Number,
		)
		if err != nil {
			return nil, httpapi.ProviderCallProblemWithDetail(
				err,
				string(repoProviderKind(*repo)), repoProviderHost(*repo),
				"Provider API error: "+err.Error(),
			)
		}
		if err := s.db.UpdateMRDraftState(
			ctx, repo.ID, input.Number, true, providerUpdatedAt,
		); err != nil {
			return nil, httpapi.Internal("update mr draft state: " + err.Error())
		}
		out := &githubStateOutput{}
		out.Body.State = input.Body.State
		return out, nil
	}

	mutator, err := s.syncer.StateMutator(
		repoProviderKind(*repo), repoProviderHost(*repo),
	)
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityStateMutation)
	}
	updatedMR, err := mutator.SetMergeRequestState(
		ctx, platformRepoRefFromDB(*repo), input.Number, input.Body.State,
	)
	if err != nil {
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr != nil && ghErr.Response != nil &&
			ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
			// Re-fetch to sync local state and determine the real cause.
			repoID := repo.ID
			{
				client, clientErr := s.syncer.ClientForHost(repo.PlatformHost)
				if clientErr != nil {
					return nil, httpapi.ProviderCallProblemWithDetail(
						clientErr, string(repoProviderKind(*repo)), repoProviderHost(*repo),
						"GitHub API error: "+err.Error(),
					)
				}
				ghPR, fetchErr := client.GetPullRequest(
					ctx, input.Owner, input.Name, input.Number,
				)
				if fetchErr == nil {
					if ghPR == nil {
						return nil, httpapi.Upstream(
							"GitHub API returned no pull request",
							string(repoProviderKind(*repo)), repoProviderHost(*repo),
						)
					}
					normalized, normalizeErr := ghclient.NormalizePR(repoID, ghPR)
					if normalizeErr != nil {
						return nil, httpapi.Upstream(
							"GitHub API error: "+normalizeErr.Error(),
							string(repoProviderKind(*repo)), repoProviderHost(*repo),
						)
					}
					// Refetched snapshots cannot represent sync-derived
					// columns either; carry them like the success path so
					// a concurrent close recovered here does not erase
					// them from a row no later sync will refetch.
					ghclient.CarryMergeRequestDerivedFields(normalized, mr)
					_, _, _, _ = s.syncer.CommitMergeRequestParentSnapshot(
						ctx, mergeRequestRepoRef(*repo), normalized,
					)
					s.markClosedLinkedNotificationsDone(ctx)
					if ghPR.GetMerged() {
						return nil, httpapi.Conflict(
							httpapi.CodeConflict,
							"cannot change state of a merged pull request",
							nil,
						)
					}
					// Already in requested state (concurrent edit).
					if ghPR.GetState() == input.Body.State {
						out := &githubStateOutput{}
						out.Body.State = input.Body.State
						return out, nil
					}
				}
			}
		}
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"GitHub API error: "+err.Error(),
		)
	}

	// Commit the provider's own post-mutation snapshot through the
	// parent-snapshot choke point rather than writing local state eagerly:
	// a close transition finalizes commit liveness inside that snapshot's
	// transaction, and the provider's timestamps keep concurrent stale
	// syncs ordered behind this write instead of an eager local clock
	// suppressing them. If the commit is rejected or fails, the next sync
	// round repairs state through the same path; the provider mutation
	// itself succeeded.
	if updatedMR.Number == input.Number {
		normalized := platform.DBMergeRequest(repo.ID, updatedMR)
		// Edit responses cannot represent sync-derived columns (CI state,
		// review decision, comment count); carry them from the stored row
		// so a UI close does not erase them from a row no later sync will
		// ever refetch.
		ghclient.CarryMergeRequestDerivedFields(normalized, mr)
		if _, _, _, commitErr := s.syncer.CommitMergeRequestParentSnapshot(
			ctx, mergeRequestRepoRef(*repo), normalized,
		); commitErr != nil {
			slog.Warn("record state change snapshot",
				"owner", repo.Owner, "repo", repo.Name,
				"number", input.Number, "state", input.Body.State, "err", commitErr)
		}
	} else {
		slog.Warn("state change returned mismatched merge request; leaving local state to the next sync",
			"owner", repo.Owner, "repo", repo.Name,
			"number", input.Number, "returned_number", updatedMR.Number,
			"state", input.Body.State)
	}
	if input.Body.State == "closed" {
		s.markClosedLinkedNotificationsDone(ctx)
	}

	out := &githubStateOutput{}
	out.Body.State = input.Body.State
	return out, nil
}

type getCommitsOutput = httpapi.BodyOutput[commitsResponse]

type resolvedPullCloneSnapshot struct {
	ctx   context.Context
	repo  *db.Repo
	shas  *db.DiffSHAs
	stale bool
}

func (s *Handler) resolvePullCloneSnapshot(
	ctx context.Context, item ItemIdentity,
) (*resolvedPullCloneSnapshot, error) {
	if s.providerSource == nil {
		repo, err := s.lookupRepoByProviderRoute(
			ctx, item.Provider, item.PlatformHost, item.Owner, item.Name,
		)
		if err != nil {
			return nil, providerRouteLookupError(err)
		}
		shas, err := s.visibleDiffSHAs(ctx, repo.ID, item.Number)
		if err != nil {
			return nil, httpapi.Internal("failed to look up PR")
		}
		if shas == nil {
			return nil, httpapi.NotFound(
				httpapi.CodePullNotFound, "pull request not found", nil,
			)
		}
		if s.clones == nil {
			return nil, httpapi.ServiceUnavailable(
				"clone-backed pull request reads are not configured",
			)
		}
		return &resolvedPullCloneSnapshot{
			ctx:  gitclone.WithRepositoryIdentity(ctx, repo.PlatformRepoID),
			repo: repo, shas: shas, stale: shas.Stale(),
		}, nil
	}
	if s.clones == nil {
		return nil, httpapi.ServiceUnavailable(
			"clone-backed pull request reads are not configured",
		)
	}

	descriptor, err := s.providerSource.GetDiffDescriptor(ctx, item)
	if err != nil {
		return nil, err
	}
	repository := descriptor.Repository
	repo, err := s.lookupRepoByProviderRoute(
		ctx, repository.Provider, repository.PlatformHost,
		repository.Owner, repository.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	if repo.PlatformRepoID != repository.PlatformRepoID {
		return nil, httpapi.Upstream(
			"hub descriptor did not resolve to its stable repository",
			repository.Provider, repository.PlatformHost,
		)
	}
	fence, found, err := s.resolver.CaptureRepositoryRouteFence(ctx, *repo)
	if err != nil {
		return nil, httpapi.Internal("capture repository route failed")
	}
	if !found {
		return nil, httpapi.NotFound(
			httpapi.CodeRepoNotFound, "repository route changed", nil,
		)
	}
	if err := s.clones.RequireCredentialRoute(
		ctx, repository.Provider, repository.PlatformHost,
		repository.Owner, repository.Name,
	); err != nil {
		return nil, pullClonePreparationProblem(err, repository)
	}
	cloneCtx := gitclone.WithRequiredCredential(
		gitclone.WithRepositoryIdentity(ctx, repository.PlatformRepoID),
	)
	validate := func(validationCtx context.Context) error {
		matches, err := s.resolver.RepositoryRouteFenceMatches(
			validationCtx, *repo, fence,
		)
		if err != nil {
			return err
		}
		if !matches {
			return db.ErrRepositoryRouteFenceChanged
		}
		return nil
	}
	if err := s.clones.EnsureCloneValidated(
		cloneCtx, repository.Provider, repository.PlatformHost,
		repository.Owner, repository.Name, repository.CloneURL, validate,
	); err != nil {
		return nil, pullClonePreparationProblem(err, repository)
	}
	if platform.Kind(repository.Provider) == platform.KindGitLab {
		if err := s.clones.FetchMergeRequestHead(
			cloneCtx, repository.Provider, repository.PlatformHost,
			repository.Owner, repository.Name, item.Number,
		); err != nil {
			return nil, pullClonePreparationProblem(err, repository)
		}
		if err := validate(ctx); err != nil {
			return nil, pullClonePreparationProblem(err, repository)
		}
	}
	return &resolvedPullCloneSnapshot{
		ctx:  cloneCtx,
		repo: repo,
		shas: &db.DiffSHAs{
			PlatformHeadSHA: descriptor.PlatformHeadSHA,
			PlatformBaseSHA: descriptor.PlatformBaseSHA,
			DiffHeadSHA:     descriptor.DiffHeadSHA,
			DiffBaseSHA:     descriptor.DiffBaseSHA,
			MergeBaseSHA:    descriptor.MergeBaseSHA,
		},
		stale: descriptor.Stale,
	}, nil
}

func pullClonePreparationProblem(
	err error, repository providerplane.RepositoryDescriptor,
) error {
	if errors.Is(err, gitclone.ErrCredentialUnavailable) {
		return httpapi.GitCredentialUnavailable(
			repository.Provider, repository.PlatformHost,
			repository.Owner+"/"+repository.Name,
		)
	}
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return httpapi.NotFound(
			httpapi.CodeRepoNotFound, "repository route changed", nil,
		)
	}
	return httpapi.Upstream(
		"failed to prepare the spoke-local repository clone",
		repository.Provider, repository.PlatformHost,
	)
}

func (s *Handler) getCommits(ctx context.Context, input *repoNumberInput) (*getCommitsOutput, error) {
	resolved, err := s.resolvePullCloneSnapshot(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	})
	if err != nil {
		return nil, err
	}
	repo, shas := resolved.repo, resolved.shas
	if shas.DiffHeadSHA == "" || shas.MergeBaseSHA == "" {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "commits not available for this pull request", nil)
	}

	host := repoProviderHost(*repo)
	commits, err := s.clones.ListCommits(
		resolved.ctx, string(repoProviderKind(*repo)), host, repo.Owner, repo.Name,
		shas.MergeBaseSHA, shas.DiffHeadSHA,
	)
	if err != nil {
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "commits not available: referenced commit not found", nil)
		}
		return nil, httpapi.Upstream(
			"failed to list commits: "+err.Error(),
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
		)
	}

	resp := commitsResponse{Commits: make([]commitResponse, len(commits))}
	for i, c := range commits {
		resp.Commits[i] = commitResponse{
			SHA:        c.SHA,
			Message:    c.Message,
			AuthorName: c.AuthorName,
			AuthoredAt: c.AuthoredAt.UTC(),
		}
	}
	return &getCommitsOutput{Body: resp}, nil
}

// --- Diff ---

type getDiffInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Whitespace   string `query:"whitespace"`
	Commit       string `query:"commit" doc:"Scope to a single commit SHA"`
	From         string `query:"from"   doc:"Start SHA for range diff (inclusive)"`
	To           string `query:"to"     doc:"End SHA for range diff (inclusive)"`
}

type getDiffOutput = httpapi.BodyOutput[diffResponse]

type resolvedDiffRange struct {
	ctx      context.Context
	platform string
	host     string
	owner    string
	name     string
	fromSHA  string
	toSHA    string
	diffSHAs *db.DiffSHAs
	stale    bool
}

func (s *Handler) resolveDiffRange(
	ctx context.Context,
	input *getDiffInput,
) (*resolvedDiffRange, error) {
	resolved, err := s.resolvePullCloneSnapshot(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	})
	if err != nil {
		return nil, err
	}
	repo, shas := resolved.repo, resolved.shas
	if shas.DiffHeadSHA == "" || shas.MergeBaseSHA == "" {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "diff not available for this pull request", nil)
	}

	host := repoProviderHost(*repo)
	ctx = resolved.ctx
	diffFrom := shas.MergeBaseSHA
	diffTo := shas.DiffHeadSHA

	hasCommit := input.Commit != ""
	hasFrom := input.From != ""
	hasTo := input.To != ""

	switch {
	case !hasCommit && !hasFrom && !hasTo:
		// Default: full PR diff. diffFrom/diffTo already set.

	case hasCommit && !hasFrom && !hasTo:
		if _, err := s.validateSHAs(
			ctx, string(repoProviderKind(*repo)), host, input, shas, input.Commit,
		); err != nil {
			return nil, err
		}
		parent, err := s.clones.ParentOf(
			ctx, string(repoProviderKind(*repo)), host, repo.Owner, repo.Name, input.Commit,
		)
		if err != nil {
			return nil, httpapi.Internal("failed to resolve parent: " + err.Error())
		}
		diffFrom = parent
		diffTo = input.Commit

	case !hasCommit && hasFrom && hasTo:
		indexMap, err := s.validateSHAs(
			ctx, string(repoProviderKind(*repo)), host, input, shas,
			input.From, input.To,
		)
		if err != nil {
			return nil, err
		}
		// In newest-first order, "from" (older) must have a higher index than "to" (newer).
		if indexMap[input.From] <= indexMap[input.To] {
			return nil, httpapi.Validation("query", "invalid range: 'from' must be older than 'to'")
		}
		parent, err := s.clones.ParentOf(
			ctx, string(repoProviderKind(*repo)), host, repo.Owner, repo.Name, input.From,
		)
		if err != nil {
			return nil, httpapi.Internal("failed to resolve parent: " + err.Error())
		}
		diffFrom = parent
		diffTo = input.To

	default:
		return nil, httpapi.Validation("query", "invalid scope: use 'commit' alone or 'from'+'to' together")
	}

	return &resolvedDiffRange{
		ctx:      ctx,
		platform: string(repoProviderKind(*repo)),
		host:     host,
		owner:    repo.Owner,
		name:     repo.Name,
		fromSHA:  diffFrom,
		toSHA:    diffTo,
		diffSHAs: shas,
		stale:    resolved.stale,
	}, nil
}

func (s *Handler) getDiff(ctx context.Context, input *getDiffInput) (*getDiffOutput, error) {
	body, err := s.GetDiffService(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	}, DiffQuery{
		Whitespace: input.Whitespace, Commit: input.Commit, From: input.From, To: input.To,
	})
	if err != nil {
		return nil, err
	}
	return &getDiffOutput{Body: body}, nil
}

func (s *Handler) getDiffRouteCore(ctx context.Context, input *getDiffInput) (*getDiffOutput, error) {
	resolved, err := s.resolveDiffRange(ctx, input)
	if err != nil {
		return nil, err
	}
	ctx = resolved.ctx

	hideWhitespace := input.Whitespace == "hide"
	result, err := s.clones.Diff(
		ctx, resolved.platform, resolved.host, resolved.owner, resolved.name,
		resolved.fromSHA, resolved.toSHA, hideWhitespace,
	)
	if err != nil {
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "diff not available: referenced commit not found", nil)
		}
		slog.Error("failed to compute diff", "owner", input.Owner, "name", input.Name, "number", input.Number, "err", err)
		return nil, httpapi.Upstream("failed to compute diff", "", "")
	}

	result.Stale = resolved.stale

	return &getDiffOutput{Body: diffResponse{
		Stale:               result.Stale,
		WhitespaceOnlyCount: result.WhitespaceOnlyCount,
		Files:               result.Files,
		DiffHeadSHA:         resolved.diffSHAs.DiffHeadSHA,
	}}, nil
}

// --- File preview ---

type getFilePreviewInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Path         string `query:"path" doc:"Changed file path to preview"`
	Side         string `query:"side" enum:"old,new" doc:"Optional diff side to read for context expansion"`
	Commit       string `query:"commit" doc:"Scope to a single commit SHA"`
	From         string `query:"from"   doc:"Start SHA for range diff (inclusive)"`
	To           string `query:"to"     doc:"End SHA for range diff (inclusive)"`
}

type getFilePreviewOutput = httpapi.BodyOutput[filePreviewResponse]

func (s *Handler) getFilePreview(ctx context.Context, input *getFilePreviewInput) (*getFilePreviewOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, httpapi.Validation("query.path", "path is required")
	}
	side := strings.TrimSpace(input.Side)
	if side != "" && side != "old" && side != "new" {
		return nil, httpapi.Validation("query.side", "side must be old or new")
	}

	resolved, err := s.resolveDiffRange(ctx, &getDiffInput{
		Provider:     input.Provider,
		PlatformHost: input.PlatformHost,
		Owner:        input.Owner,
		Name:         input.Name,
		Number:       input.Number,
		Commit:       input.Commit,
		From:         input.From,
		To:           input.To,
	})
	if err != nil {
		return nil, err
	}
	ctx = resolved.ctx

	previewRef := resolved.toSHA
	previewPath := input.Path
	files, err := s.clones.DiffFiles(
		ctx,
		resolved.platform,
		resolved.host,
		resolved.owner,
		resolved.name,
		resolved.fromSHA,
		resolved.toSHA,
	)
	if err != nil {
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "file preview not available: referenced commit not found", nil)
		}
		slog.Error("failed to validate preview path", "owner", input.Owner, "name", input.Name, "number", input.Number, "path", input.Path, "err", err)
		return nil, httpapi.Upstream("failed to validate file preview", "", "")
	}
	found := false
	for _, file := range files {
		if file.Path != input.Path {
			continue
		}
		found = true
		if side == "old" {
			if file.Status == "added" {
				return nil, httpapi.NotFound(httpapi.CodeNotFound, "file preview not available: old side does not exist", nil)
			}
			previewRef = resolved.fromSHA
			previewPath = file.OldPath
			if previewPath == "" {
				previewPath = file.Path
			}
		} else if side == "new" {
			if file.Status == "deleted" {
				return nil, httpapi.NotFound(httpapi.CodeNotFound, "file preview not available: new side does not exist", nil)
			}
			previewRef = resolved.toSHA
			previewPath = file.Path
		} else if file.Status == "deleted" {
			previewRef = resolved.fromSHA
			previewPath = file.OldPath
			if previewPath == "" {
				previewPath = file.Path
			}
		}
		break
	}
	if !found {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "file preview not available: file is not changed in this diff", nil)
	}

	content, err := s.clones.FileContent(
		ctx,
		resolved.platform,
		resolved.host,
		resolved.owner,
		resolved.name,
		previewRef,
		previewPath,
		maxFilePreviewBytes,
	)
	if err != nil {
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "file preview not available: referenced file not found", nil)
		}
		if errors.Is(err, gitclone.ErrTooLarge) {
			return nil, httpapi.PayloadTooLarge("file preview is too large", maxFilePreviewBytes)
		}
		slog.Error("failed to read file preview", "owner", input.Owner, "name", input.Name, "number", input.Number, "path", input.Path, "err", err)
		return nil, httpapi.Upstream("failed to read file preview", "", "")
	}

	return &getFilePreviewOutput{Body: filePreviewResponse{
		Path:      content.Path,
		MediaType: previewMediaType(content.Path, content.Data),
		Encoding:  "base64",
		Content:   base64.StdEncoding.EncodeToString(content.Data),
		Size:      content.Size,
	}}, nil
}

func previewMediaType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".jsonc":
		return "application/jsonc; charset=utf-8"
	case ".toml":
		return "application/toml; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	}
	if mediaType := mime.TypeByExtension(filepath.Ext(path)); mediaType != "" {
		return mediaType
	}
	return http.DetectContentType(data)
}

// --- Files (lightweight) ---

type getFilesInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
}

type getFilesOutput = httpapi.BodyOutput[filesResponse]

func (s *Handler) getFiles(ctx context.Context, input *getFilesInput) (*getFilesOutput, error) {
	body, err := s.GetFilesService(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	})
	if err != nil {
		return nil, err
	}
	return &getFilesOutput{Body: body}, nil
}

func (s *Handler) getFilesRouteCore(ctx context.Context, input *getFilesInput) (*getFilesOutput, error) {
	resolved, err := s.resolvePullCloneSnapshot(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	})
	if err != nil {
		return nil, err
	}
	repo, shas := resolved.repo, resolved.shas
	if shas.DiffHeadSHA == "" || shas.MergeBaseSHA == "" {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "file list not available for this pull request", nil)
	}

	host := repoProviderHost(*repo)
	files, err := s.clones.DiffFiles(
		resolved.ctx, string(repoProviderKind(*repo)), host, repo.Owner, repo.Name,
		shas.MergeBaseSHA, shas.DiffHeadSHA,
	)
	if err != nil {
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "file list not available: referenced commit not found", nil)
		}
		slog.Error("failed to list files", "owner", input.Owner, "name", input.Name, "number", input.Number, "err", err)
		return nil, httpapi.Upstream("failed to list files", "", "")
	}

	return &getFilesOutput{Body: filesResponse{
		Stale: resolved.stale,
		Files: files,
	}}, nil
}

// validateSHAs checks that all provided SHAs are in the PR's first-parent commit list.
// Returns a SHA -> index map (newest-first order) so callers can check range ordering.
func (s *Handler) validateSHAs(
	ctx context.Context,
	platformName, host string,
	input *getDiffInput,
	shas *db.DiffSHAs,
	userSHAs ...string,
) (map[string]int, error) {
	commits, err := s.clones.ListCommits(
		ctx, platformName, host, input.Owner, input.Name,
		shas.MergeBaseSHA, shas.DiffHeadSHA,
	)
	if err != nil {
		return nil, httpapi.Internal("failed to list commits for validation: " + err.Error())
	}
	indexMap := make(map[string]int, len(commits))
	for i, c := range commits {
		indexMap[c.SHA] = i
	}
	for _, sha := range userSHAs {
		if _, ok := indexMap[sha]; !ok {
			return nil, httpapi.Validation("query", "sha not in pull request: "+sha)
		}
	}
	return indexMap, nil
}

// --- Stacks ---

func (s *Handler) listStacks(ctx context.Context, input *listStacksInput) (*listStacksOutput, error) {
	if input.Repo != "" {
		if strings.Count(input.Repo, "/") != 1 {
			return nil, httpapi.Validation("query.repo", "invalid repo filter: expected owner/name")
		}
		owner, name, _ := strings.Cut(input.Repo, "/")
		if owner == "" || name == "" {
			return nil, httpapi.Validation("query.repo", "invalid repo filter: expected owner/name")
		}
	}
	stackList, memberMap, err := s.db.ListStacksWithMembers(ctx, input.Repo)
	if err != nil {
		return nil, httpapi.Internal("list stacks failed")
	}

	out := make([]stackResponse, 0, len(stackList))
	for _, st := range stackList {
		members := memberMap[st.ID]
		out = append(out, stackResponse{
			ID:        st.ID,
			Name:      st.Name,
			RepoOwner: st.RepoOwner,
			RepoName:  st.RepoName,
			Health:    computeStackHealth(members),
			Members:   toStackMemberResponses(members),
		})
	}

	return &listStacksOutput{Body: out}, nil
}

func stackContextForPR(number int, stack *db.Stack, members []db.StackMemberWithPR) stackContextResponse {
	var position int
	for _, m := range members {
		if m.Number == number {
			position = m.Position
			break
		}
	}

	return stackContextResponse{
		StackID:   stack.ID,
		StackName: stack.Name,
		Position:  position,
		Size:      len(members),
		Health:    computeStackHealth(members),
		Members:   toStackMemberResponses(members),
	}
}

func (s *Handler) getStackForPR(ctx context.Context, input *repoNumberInput) (*getStackForPROutput, error) {
	body, err := s.GetStackService(ctx, ItemIdentity{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
	})
	if err != nil {
		return nil, err
	}
	return &getStackForPROutput{Body: body.routeResponse()}, nil
}

func (s *Handler) getStackForPRRouteCore(ctx context.Context, input *repoNumberInput) (*getStackForPROutput, error) {
	repo, err := s.lookupRepoByProviderRoute(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	stack, members, err := s.db.GetStackForPRByRepoID(ctx, repo.ID, input.Number)
	if err != nil {
		return nil, httpapi.Internal("get stack for pr failed")
	}
	if stack == nil {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "PR is not part of a stack", nil)
	}

	return &getStackForPROutput{
		Body: stackContextForPR(input.Number, stack, members),
	}, nil
}
