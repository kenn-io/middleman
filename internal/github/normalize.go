package github

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

// sanitizeURL returns the URL if it uses a safe scheme, or empty string.
func sanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "https" || scheme == "http" {
		return raw
	}
	return ""
}

var ErrNilPullRequest = platformgithub.ErrNilPullRequest

// NormalizePR converts a GitHub PullRequest to a db.MergeRequest.
// If the PR is merged, State is set to "merged". LastActivityAt is
// initialized to UpdatedAt.
func NormalizePR(repoID int64, ghPR *gh.PullRequest) (*db.MergeRequest, error) {
	if ghPR == nil {
		return nil, ErrNilPullRequest
	}
	platformMR, err := platformgithub.NormalizePullRequest(platform.RepoRef{}, ghPR)
	if err != nil {
		return nil, err
	}
	mr := &db.MergeRequest{
		RepoID:             repoID,
		PlatformID:         platformMR.PlatformID,
		PlatformExternalID: platformMR.PlatformExternalID,
		Number:             platformMR.Number,
		URL:                platformMR.URL,
		Title:              platformMR.Title,
		Author:             platformMR.Author,
		AuthorDisplayName:  platformMR.AuthorDisplayName,
		State:              db.MergeRequestState(platformMR.State),
		IsDraft:            platformMR.IsDraft,
		IsLocked:           platformMR.IsLocked,
		Body:               platformMR.Body,
		HeadBranch:         platformMR.HeadBranch,
		BaseBranch:         platformMR.BaseBranch,
		PlatformHeadSHA:    platformMR.HeadSHA,
		PlatformBaseSHA:    platformMR.BaseSHA,
		Additions:          platformMR.Additions,
		AdditionsKnown:     platformMR.AdditionsKnown,
		Deletions:          platformMR.Deletions,
		DeletionsKnown:     platformMR.DeletionsKnown,
		FilesChanged:       platformMR.FilesChanged,
		MergeCommitSHA:     platformMR.MergeCommitSHA,
		ReviewDecision:     platformMR.ReviewDecision,
		CIStatus:           platformMR.CIStatus,
		CreatedAt:          platformMR.CreatedAt,
		UpdatedAt:          platformMR.UpdatedAt,
		LastActivityAt:     platformMR.LastActivityAt,
		MergedAt:           platformMR.MergedAt,
		ClosedAt:           platformMR.ClosedAt,
	}

	if pullRequestWasMerged(ghPR) {
		mr.State = "merged"
	}

	if ghPR.CreatedAt != nil {
		mr.CreatedAt = ghPR.CreatedAt.Time
	}
	if ghPR.UpdatedAt != nil {
		mr.UpdatedAt = ghPR.UpdatedAt.Time
		mr.LastActivityAt = ghPR.UpdatedAt.Time
	}
	if ghPR.MergedAt != nil {
		t := ghPR.MergedAt.Time
		mr.MergedAt = &t
	}
	if ghPR.ClosedAt != nil {
		t := ghPR.ClosedAt.Time
		mr.ClosedAt = &t
	}
	if ghPR.GetHead() != nil {
		if ghPR.GetHead().GetRepo() != nil {
			mr.HeadRepoCloneURL = ghPR.GetHead().GetRepo().GetCloneURL()
		}
	}
	mr.MergeableState = ghPR.GetMergeableState()
	mr.Labels = dbLabels(platformMR.Labels, itemLabelUpdatedAt(mr.UpdatedAt, mr.CreatedAt))
	mr.Assignees = platformMR.Assignees
	mr.RequestedReviewers = platformMR.RequestedReviewers
	mr.AssigneesJSON = platformdb.MarshalUserNamesJSON(platformMR.Assignees)
	mr.ReviewersJSON = platformdb.MarshalUserNamesJSON(platformMR.RequestedReviewers)

	return mr, nil
}

func pullRequestWasMerged(ghPR *gh.PullRequest) bool {
	return ghPR.GetMerged() || ghPR.MergedAt != nil
}

// NormalizeCommentEvent converts a GitHub IssueComment to a db.MREvent.
func NormalizeCommentEvent(mrID int64, c *gh.IssueComment) db.MREvent {
	event := platformgithub.NormalizeCommentEvent(platform.RepoRef{}, 0, c)
	return dbMREvent(mrID, event)
}

func NormalizeCommentEventWithVisibility(
	mrID int64,
	c *gh.IssueComment,
	visibility platformgithub.CommentVisibility,
) db.MREvent {
	event := NormalizeCommentEvent(mrID, c)
	event.MetadataJSON = platformgithub.NormalizeCommentVisibilityMetadata(visibility)
	return event
}

// NormalizeReviewEvent converts a GitHub PullRequestReview to a db.MREvent.
func NormalizeReviewEvent(mrID int64, r *gh.PullRequestReview) db.MREvent {
	event := platformgithub.NormalizeReviewEvent(platform.RepoRef{}, 0, r)
	return dbMREvent(mrID, event)
}

// NormalizeCommitEvent converts a GitHub RepositoryCommit to a db.MREvent.
// Author is the committer login or name when available, falling back to the
// commit author identity; a distinct original author is retained in metadata.
func NormalizeCommitEvent(mrID int64, c *gh.RepositoryCommit) db.MREvent {
	event := platformgithub.NormalizeCommitEvent(platform.RepoRef{}, 0, c)
	return dbMREvent(mrID, event)
}

func NormalizeForcePushEvent(mrID int64, fp platformgithub.ForcePushEvent) db.MREvent {
	event := platformgithub.NormalizeForcePushEvent(platform.RepoRef{}, 0, platformgithub.ForcePushEvent{
		Actor:     fp.Actor,
		BeforeSHA: fp.BeforeSHA,
		AfterSHA:  fp.AfterSHA,
		Ref:       fp.Ref,
		CreatedAt: fp.CreatedAt,
	})
	return dbMREvent(mrID, event)
}

func NormalizeTimelineEvent(mrID int64, event platformgithub.PullRequestTimelineEvent) *db.MREvent {
	normalized := platformgithub.NormalizeTimelineEvent(
		platform.RepoRef{},
		0,
		platformgithub.PullRequestTimelineEvent{
			NodeID:               event.NodeID,
			EventType:            event.EventType,
			Actor:                event.Actor,
			Assignee:             event.Assignee,
			CreatedAt:            event.CreatedAt,
			DeletedCommentAuthor: event.DeletedCommentAuthor,
			BeforeSHA:            event.BeforeSHA,
			AfterSHA:             event.AfterSHA,
			Ref:                  event.Ref,
			PreviousTitle:        event.PreviousTitle,
			CurrentTitle:         event.CurrentTitle,
			PreviousRefName:      event.PreviousRefName,
			CurrentRefName:       event.CurrentRefName,
			SourceType:           event.SourceType,
			SourceOwner:          event.SourceOwner,
			SourceRepo:           event.SourceRepo,
			SourceNumber:         event.SourceNumber,
			SourceTitle:          event.SourceTitle,
			SourceURL:            event.SourceURL,
			IsCrossRepository:    event.IsCrossRepository,
			WillCloseTarget:      event.WillCloseTarget,
		},
	)
	if normalized == nil {
		return nil
	}
	eventDB := dbMREvent(mrID, *normalized)
	return &eventDB
}

func NormalizeIssueTimelineEvent(issueID int64, event platformgithub.PullRequestTimelineEvent) *db.IssueEvent {
	normalized := platformgithub.NormalizeIssueTimelineEvent(
		platform.RepoRef{},
		0,
		platformgithub.PullRequestTimelineEvent{
			NodeID:            event.NodeID,
			EventType:         event.EventType,
			Actor:             event.Actor,
			Assignee:          event.Assignee,
			CreatedAt:         event.CreatedAt,
			SourceType:        event.SourceType,
			SourceOwner:       event.SourceOwner,
			SourceRepo:        event.SourceRepo,
			SourceNumber:      event.SourceNumber,
			SourceTitle:       event.SourceTitle,
			SourceURL:         event.SourceURL,
			IsCrossRepository: event.IsCrossRepository,
			WillCloseTarget:   event.WillCloseTarget,
		},
	)
	if normalized == nil {
		return nil
	}
	eventDB := dbIssueEvent(issueID, *normalized)
	return &eventDB
}

// DeriveOverallCIStatus computes an aggregate CI status from check runs
// and the legacy combined status API. The combined status API only reports
// on commit statuses (the older mechanism); repos using only GitHub Actions
// check runs will have an empty or "pending" combined state even when all
// checks pass. This function merges both sources to produce the correct
// overall status.
func DeriveOverallCIStatus(
	runs []*gh.CheckRun,
	combined *gh.CombinedStatus,
) string {
	checks := platformgithub.NormalizeCIChecks(platform.RepoRef{}, runs, combined)
	return platformgithub.DeriveOverallCIStatus(checks, combined)
}

// DeriveReviewDecision computes the aggregate review decision from a list of
// reviews. It keeps the latest APPROVED or CHANGES_REQUESTED review per user.
// Returns "changes_requested" if any user has that state, "approved" if at
// least one approval exists, or "" if no actionable reviews are present.
func DeriveReviewDecision(reviews []*gh.PullRequestReview) string {
	// latest state per reviewer login
	latest := make(map[string]string)
	for _, r := range reviews {
		login := loginOrEmpty(r.GetUser())
		if login == "" {
			continue
		}
		state := r.GetState()
		if state == "APPROVED" || state == "CHANGES_REQUESTED" {
			latest[login] = state
		}
	}

	hasApproved := false
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return "changes_requested"
		}
		if state == "APPROVED" {
			hasApproved = true
		}
	}
	if hasApproved {
		return "approved"
	}
	return ""
}

// mapGraphQLReviewDecision converts GitHub's authoritative reviewDecision enum
// into kenn-forge's stored review decision. The second return reports whether
// the provider supplied an authoritative decision: a non-empty enum. A null or
// empty reviewDecision means the repository enforces no review decision, so
// callers must derive the aggregate from the reviews themselves rather than
// clearing an existing value.
func mapGraphQLReviewDecision(raw string) (string, bool) {
	switch raw {
	case "APPROVED":
		return "approved", true
	case "CHANGES_REQUESTED":
		return "changes_requested", true
	case "REVIEW_REQUIRED":
		// Reviews are required but no blocking change or sufficient approval
		// exists yet, which kenn-forge represents as no actionable decision.
		return "", true
	default:
		return "", false
	}
}

// NormalizeCheckRuns converts GitHub check runs to a JSON string of CICheck objects.
func NormalizeCheckRuns(runs []*gh.CheckRun) string {
	checks := normalizeCIChecks(runs, nil)
	if len(checks) == 0 {
		return ""
	}
	b, err := json.Marshal(checks)
	if err != nil {
		return ""
	}
	return string(b)
}

// NormalizeCIChecks merges check runs and commit statuses into a single
// JSON string of CICheck objects. Commit statuses (used by GitHub Apps
// like roborev) use the older status API and need to be mapped into the
// same shape as check runs.
func NormalizeCIChecks(
	runs []*gh.CheckRun,
	combined *gh.CombinedStatus,
) string {
	checks := normalizeCIChecks(runs, combinedStatuses(combined))
	if len(checks) == 0 {
		return ""
	}
	b, err := json.Marshal(checks)
	if err != nil {
		return ""
	}
	return string(b)
}

func normalizeCIChecks(runs []*gh.CheckRun, statuses []*gh.RepoStatus) []db.CICheck {
	return dbCIChecks(platformgithub.NormalizeCIChecks(
		platform.RepoRef{},
		runs,
		&gh.CombinedStatus{Statuses: statuses},
	))
}

func combinedStatuses(combined *gh.CombinedStatus) []*gh.RepoStatus {
	if combined == nil {
		return nil
	}
	return combined.Statuses
}

// --- Issues ---

// NormalizeIssue converts a GitHub Issue to a db.Issue.
func NormalizeIssue(repoID int64, ghIssue *gh.Issue) (*db.Issue, error) {
	platformIssue, err := platformgithub.NormalizeIssue(platform.RepoRef{}, ghIssue)
	if err != nil {
		return nil, err
	}

	issue := &db.Issue{
		RepoID:             repoID,
		PlatformID:         platformIssue.PlatformID,
		PlatformExternalID: platformIssue.PlatformExternalID,
		Number:             platformIssue.Number,
		URL:                platformIssue.URL,
		Title:              platformIssue.Title,
		Author:             platformIssue.Author,
		State:              platformIssue.State,
		Body:               platformIssue.Body,
		CommentCount:       platformIssue.CommentCount,
		CreatedAt:          platformIssue.CreatedAt,
		UpdatedAt:          platformIssue.UpdatedAt,
		LastActivityAt:     platformIssue.LastActivityAt,
		ClosedAt:           platformIssue.ClosedAt,
		AssigneesJSON:      platformdb.MarshalAssigneesJSON(platformIssue.Assignees),
		Assignees:          platformIssue.Assignees,
	}
	issue.Labels = dbLabels(platformIssue.Labels, itemLabelUpdatedAt(issue.UpdatedAt, issue.CreatedAt))
	return issue, nil
}

func itemLabelUpdatedAt(updatedAt, createdAt time.Time) time.Time {
	if !updatedAt.IsZero() {
		return updatedAt
	}
	return createdAt
}

func dbMREvent(mrID int64, event platform.MergeRequestEvent) db.MREvent {
	dbEvent := db.MREvent{
		MergeRequestID:     mrID,
		PlatformExternalID: event.PlatformExternalID,
		EventType:          event.EventType,
		Author:             event.Author,
		Summary:            event.Summary,
		Body:               event.Body,
		MetadataJSON:       event.MetadataJSON,
		CreatedAt:          event.CreatedAt,
		DedupeKey:          event.DedupeKey,
		DirectURL:          event.DirectURL,
	}
	if event.PlatformID != 0 || event.EventType == "issue_comment" || event.EventType == "review" {
		platformID := event.PlatformID
		dbEvent.PlatformID = &platformID
	}
	return dbEvent
}

func dbIssueEvent(issueID int64, event platform.IssueEvent) db.IssueEvent {
	dbEvent := db.IssueEvent{
		IssueID:            issueID,
		PlatformExternalID: event.PlatformExternalID,
		EventType:          event.EventType,
		Author:             event.Author,
		Summary:            event.Summary,
		Body:               event.Body,
		MetadataJSON:       event.MetadataJSON,
		CreatedAt:          event.CreatedAt,
		DedupeKey:          event.DedupeKey,
		DirectURL:          event.DirectURL,
	}
	if event.PlatformID != 0 || event.EventType == "issue_comment" {
		platformID := event.PlatformID
		dbEvent.PlatformID = &platformID
	}
	return dbEvent
}

func dbLabels(labels []platform.Label, updatedAt time.Time) []db.Label {
	if len(labels) == 0 {
		return nil
	}
	out := make([]db.Label, 0, len(labels))
	for _, label := range labels {
		out = append(out, db.Label{
			PlatformID:         label.PlatformID,
			PlatformExternalID: label.PlatformExternalID,
			Name:               label.Name,
			Description:        label.Description,
			Color:              label.Color,
			IsDefault:          label.IsDefault,
			UpdatedAt:          updatedAt,
		})
	}
	return out
}

func dbCIChecks(checks []platform.CICheck) []db.CICheck {
	return platformdb.DBCIChecks(checks)
}

// NormalizeIssueCommentEvent converts a GitHub IssueComment to a db.IssueEvent.
func NormalizeIssueCommentEvent(issueID int64, c *gh.IssueComment) db.IssueEvent {
	event := platformgithub.NormalizeIssueCommentEvent(platform.RepoRef{}, 0, c)
	return dbIssueEvent(issueID, event)
}

func NormalizeIssueCommentEventWithVisibility(
	issueID int64,
	c *gh.IssueComment,
	visibility platformgithub.CommentVisibility,
) db.IssueEvent {
	event := NormalizeIssueCommentEvent(issueID, c)
	event.MetadataJSON = platformgithub.NormalizeCommentVisibilityMetadata(visibility)
	return event
}

// loginOrEmpty returns the GitHub login for a user, or "" if user is nil.
func loginOrEmpty(u *gh.User) string {
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

// nameOrEmpty returns the GitHub display name for a user, or "" if
// unavailable. Bot accounts (Type == "Bot") use their login as display name
// since they have no user-facing name on the GitHub API.
func nameOrEmpty(u *gh.User) string {
	if u == nil {
		return ""
	}
	if u.GetType() == "Bot" {
		return u.GetLogin()
	}
	return sanitizeDisplayName(u.GetName())
}

// sanitizeDisplayName strips characters that could inject trailers or
// corrupt git commit metadata when used in a Co-authored-by line.
func sanitizeDisplayName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\n', '\r', '<', '>':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
