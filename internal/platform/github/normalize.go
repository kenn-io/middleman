package github

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/platform"
)

func NormalizePullRequest(repo platform.RepoRef, ghPR *gh.PullRequest) (platform.MergeRequest, error) {
	if ghPR == nil {
		return platform.MergeRequest{}, ErrNilPullRequest
	}
	mr := platform.MergeRequest{
		Repo:               repo,
		PlatformID:         ghPR.GetID(),
		PlatformExternalID: ghPR.GetNodeID(),
		Number:             ghPR.GetNumber(),
		URL:                ghPR.GetHTMLURL(),
		Title:              ghPR.GetTitle(),
		Author:             loginOrEmpty(ghPR.GetUser()),
		AuthorDisplayName:  nameOrEmpty(ghPR.GetUser()),
		State:              ghPR.GetState(),
		IsDraft:            ghPR.GetDraft(),
		IsLocked:           ghPR.GetLocked(),
		Body:               ghPR.GetBody(),
		Additions:          ghPR.GetAdditions(),
		AdditionsKnown:     ghPR.Additions != nil,
		Deletions:          ghPR.GetDeletions(),
		DeletionsKnown:     ghPR.Deletions != nil,
		FilesChanged:       ghPR.ChangedFiles,
		MergeCommitSHA:     ghPR.GetMergeCommitSHA(),
		CommentCount:       ghPR.GetComments(),
		MergeableState:     ghPR.GetMergeableState(),
	}
	if ghPR.GetMerged() {
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
	mr.MergedBy = loginOrEmpty(ghPR.GetMergedBy())
	if ghPR.ClosedAt != nil {
		t := ghPR.ClosedAt.Time
		mr.ClosedAt = &t
	}
	if ghPR.GetHead() != nil {
		mr.HeadBranch = ghPR.GetHead().GetRef()
		mr.HeadSHA = ghPR.GetHead().GetSHA()
		if ghPR.GetHead().GetRepo() != nil {
			mr.HeadRepoCloneURL = ghPR.GetHead().GetRepo().GetCloneURL()
		}
	}
	if ghPR.GetBase() != nil {
		mr.BaseBranch = ghPR.GetBase().GetRef()
		mr.BaseSHA = ghPR.GetBase().GetSHA()
	}
	mr.Labels = normalizeLabels(repo, ghPR.Labels)
	mr.Assignees = normalizeUserLogins(ghPR.Assignees)
	mr.RequestedReviewers = normalizeUserLogins(ghPR.RequestedReviewers)
	return mr, nil
}

// normalizeUserLogins preserves the distinction between a provider
// response without the field (nil, unknown) and an explicit empty list.
// GitHub team review requests are intentionally not represented here;
// only user logins are tracked.
func normalizeUserLogins(users []*gh.User) []string {
	if users == nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u == nil || u.GetLogin() == "" {
			continue
		}
		out = append(out, u.GetLogin())
	}
	return out
}

func NormalizeIssue(repo platform.RepoRef, ghIssue *gh.Issue) (platform.Issue, error) {
	if ghIssue == nil {
		return platform.Issue{}, ErrNilIssue
	}

	var assignees []string
	for _, a := range ghIssue.Assignees {
		if a != nil && a.Login != nil {
			assignees = append(assignees, *a.Login)
		}
	}

	issue := platform.Issue{
		Repo:               repo,
		PlatformID:         ghIssue.GetID(),
		PlatformExternalID: ghIssue.GetNodeID(),
		Number:             ghIssue.GetNumber(),
		URL:                ghIssue.GetHTMLURL(),
		Title:              ghIssue.GetTitle(),
		Author:             loginOrEmpty(ghIssue.GetUser()),
		State:              ghIssue.GetState(),
		Body:               ghIssue.GetBody(),
		CommentCount:       ghIssue.GetComments(),
		Assignees:          assignees,
	}
	if ghIssue.CreatedAt != nil {
		issue.CreatedAt = ghIssue.CreatedAt.Time
	}
	if ghIssue.UpdatedAt != nil {
		issue.UpdatedAt = ghIssue.UpdatedAt.Time
		issue.LastActivityAt = ghIssue.UpdatedAt.Time
	} else {
		issue.UpdatedAt = issue.CreatedAt
		issue.LastActivityAt = issue.CreatedAt
	}
	if ghIssue.ClosedAt != nil {
		t := ghIssue.ClosedAt.Time
		issue.ClosedAt = &t
	}
	issue.Labels = normalizeLabels(repo, ghIssue.Labels)
	return issue, nil
}

func NormalizeCommentEvent(
	repo platform.RepoRef,
	mrNumber int,
	c *gh.IssueComment,
) platform.MergeRequestEvent {
	event := normalizeIssueCommentBase(repo, c)
	event.MergeRequestNumber = mrNumber
	event.DedupeKey = fmt.Sprintf("comment-%d", c.GetID())
	return event
}

func NormalizeReviewEvent(
	repo platform.RepoRef,
	mrNumber int,
	r *gh.PullRequestReview,
) platform.MergeRequestEvent {
	event := platform.MergeRequestEvent{
		Repo:               repo,
		PlatformID:         r.GetID(),
		PlatformExternalID: r.GetNodeID(),
		MergeRequestNumber: mrNumber,
		EventType:          "review",
		DedupeKey:          fmt.Sprintf("review-%d", r.GetID()),
		Author:             loginOrEmpty(r.GetUser()),
		Body:               r.GetBody(),
		Summary:            r.GetState(),
		DirectURL:          r.GetHTMLURL(),
	}
	if r.SubmittedAt != nil {
		event.CreatedAt = r.SubmittedAt.Time
	}
	return event
}

func NormalizeReviewCommentEvent(
	repo platform.RepoRef,
	mrNumber int,
	c *gh.PullRequestComment,
) platform.MergeRequestEvent {
	event := platform.MergeRequestEvent{
		Repo:               repo,
		PlatformID:         c.GetID(),
		PlatformExternalID: fmt.Sprintf("%d", c.GetID()),
		MergeRequestNumber: mrNumber,
		EventType:          "review_comment",
		DedupeKey:          fmt.Sprintf("review_comment:%d", c.GetID()),
		Author:             loginOrEmpty(c.GetUser()),
		Body:               c.GetBody(),
		DirectURL:          c.GetHTMLURL(),
	}
	if c.CreatedAt != nil {
		event.CreatedAt = c.CreatedAt.Time
	}
	return event
}

type commitEventMetadata struct {
	CommitAuthor string `json:"commit_author,omitempty"`
}

func commitIdentity(user *gh.User, signature *gh.CommitAuthor) string {
	if login := loginOrEmpty(user); login != "" {
		return login
	}
	if signature != nil {
		return signature.GetName()
	}
	return ""
}

func NormalizeCommitEvent(
	repo platform.RepoRef,
	mrNumber int,
	c *gh.RepositoryCommit,
) platform.MergeRequestEvent {
	sha := c.GetSHA()
	dedupeKey := sha
	if len(sha) > 12 {
		dedupeKey = sha[:12]
	}

	commit := c.GetCommit()
	authorSignature := (*gh.CommitAuthor)(nil)
	committerSignature := (*gh.CommitAuthor)(nil)
	if commit != nil {
		authorSignature = commit.GetAuthor()
		committerSignature = commit.GetCommitter()
	}
	author := commitIdentity(c.GetAuthor(), authorSignature)
	committer := commitIdentity(c.GetCommitter(), committerSignature)
	actor := committer
	if actor == "" {
		actor = author
	}

	event := platform.MergeRequestEvent{
		Repo:               repo,
		MergeRequestNumber: mrNumber,
		EventType:          "commit",
		DedupeKey:          fmt.Sprintf("commit-%s", dedupeKey),
		Author:             actor,
		Summary:            sha,
	}
	if author != "" && author != actor {
		metadata, _ := json.Marshal(commitEventMetadata{CommitAuthor: author})
		event.MetadataJSON = string(metadata)
	}
	if commit != nil {
		event.Body = commit.GetMessage()
		if committerSignature != nil && committerSignature.Date != nil {
			event.CreatedAt = committerSignature.Date.UTC()
		} else if authorSignature != nil && authorSignature.Date != nil {
			event.CreatedAt = authorSignature.Date.UTC()
		}
	}
	return event
}

func NormalizeForcePushEvent(
	repo platform.RepoRef,
	mrNumber int,
	fp ForcePushEvent,
) platform.MergeRequestEvent {
	metadata, _ := json.Marshal(forcePushMetadata{
		BeforeSHA: fp.BeforeSHA,
		AfterSHA:  fp.AfterSHA,
		Ref:       fp.Ref,
	})

	return platform.MergeRequestEvent{
		Repo:               repo,
		MergeRequestNumber: mrNumber,
		EventType:          "force_push",
		Author:             fp.Actor,
		Summary:            shortSHA(fp.BeforeSHA) + " -> " + shortSHA(fp.AfterSHA),
		MetadataJSON:       string(metadata),
		CreatedAt:          fp.CreatedAt,
		DedupeKey:          fmt.Sprintf("force-push-%s-%s", fp.BeforeSHA, fp.AfterSHA),
	}
}

func NormalizeTimelineEvent(
	repo platform.RepoRef,
	mrNumber int,
	event PullRequestTimelineEvent,
) *platform.MergeRequestEvent {
	switch event.EventType {
	case "comment_deleted":
		metadata, _ := json.Marshal(commentDeletedMetadata{
			DeletedCommentAuthor: event.DeletedCommentAuthor,
		})
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          "comment_deleted",
			Author:             event.Actor,
			Summary:            deletedCommentSummary(event.DeletedCommentAuthor),
			MetadataJSON:       string(metadata),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	case "force_push":
		normalized := NormalizeForcePushEvent(repo, mrNumber, ForcePushEvent{
			Actor:     event.Actor,
			BeforeSHA: event.BeforeSHA,
			AfterSHA:  event.AfterSHA,
			Ref:       event.Ref,
			CreatedAt: event.CreatedAt,
		})
		if event.NodeID != "" {
			normalized.DedupeKey = timelineDedupeKey(event)
		}
		return &normalized
	case "cross_referenced":
		metadata, _ := json.Marshal(crossReferenceMetadata{
			SourceType:        event.SourceType,
			SourceOwner:       event.SourceOwner,
			SourceRepo:        event.SourceRepo,
			SourceNumber:      event.SourceNumber,
			SourceTitle:       event.SourceTitle,
			SourceURL:         event.SourceURL,
			IsCrossRepository: event.IsCrossRepository,
			WillCloseTarget:   event.WillCloseTarget,
		})
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          "cross_referenced",
			Author:             event.Actor,
			Summary:            fmt.Sprintf("Referenced from %s/%s#%d", event.SourceOwner, event.SourceRepo, event.SourceNumber),
			MetadataJSON:       string(metadata),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	case "renamed_title":
		metadata, _ := json.Marshal(renamedTitleMetadata{
			PreviousTitle: event.PreviousTitle,
			CurrentTitle:  event.CurrentTitle,
		})
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          "renamed_title",
			Author:             event.Actor,
			Summary:            fmt.Sprintf("%q -> %q", event.PreviousTitle, event.CurrentTitle),
			MetadataJSON:       string(metadata),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	case "base_ref_changed":
		metadata, _ := json.Marshal(baseRefChangedMetadata{
			PreviousRefName: event.PreviousRefName,
			CurrentRefName:  event.CurrentRefName,
		})
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          "base_ref_changed",
			Author:             event.Actor,
			Summary:            event.PreviousRefName + " -> " + event.CurrentRefName,
			MetadataJSON:       string(metadata),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	case "assigned", "unassigned":
		metadata, _ := json.Marshal(assignmentMetadata{
			Assignee: event.Assignee,
		})
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          event.EventType,
			Author:             event.Actor,
			Summary:            assignmentSummary(event.EventType, event.Actor, event.Assignee),
			MetadataJSON:       string(metadata),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	case "merged", "closed", "reopened":
		return &platform.MergeRequestEvent{
			Repo:               repo,
			MergeRequestNumber: mrNumber,
			EventType:          event.EventType,
			Author:             event.Actor,
			Summary:            lifecycleSummary(event.EventType),
			CreatedAt:          event.CreatedAt,
			DedupeKey:          timelineDedupeKey(event),
		}
	default:
		return nil
	}
}

func lifecycleSummary(eventType string) string {
	switch eventType {
	case "merged":
		return "merged this"
	case "closed":
		return "closed this"
	case "reopened":
		return "reopened this"
	default:
		return ""
	}
}

func NormalizeIssueTimelineEvent(
	repo platform.RepoRef,
	issueNumber int,
	event PullRequestTimelineEvent,
) *platform.IssueEvent {
	switch event.EventType {
	case "assigned", "unassigned":
		metadata, _ := json.Marshal(assignmentMetadata{
			Assignee: event.Assignee,
		})
		return &platform.IssueEvent{
			Repo:         repo,
			IssueNumber:  issueNumber,
			EventType:    event.EventType,
			Author:       event.Actor,
			Summary:      assignmentSummary(event.EventType, event.Actor, event.Assignee),
			MetadataJSON: string(metadata),
			CreatedAt:    event.CreatedAt,
			DedupeKey:    timelineDedupeKey(event),
		}
	case "cross_referenced":
		metadata, _ := json.Marshal(crossReferenceMetadata{
			SourceType:        event.SourceType,
			SourceOwner:       event.SourceOwner,
			SourceRepo:        event.SourceRepo,
			SourceNumber:      event.SourceNumber,
			SourceTitle:       event.SourceTitle,
			SourceURL:         event.SourceURL,
			IsCrossRepository: event.IsCrossRepository,
			WillCloseTarget:   event.WillCloseTarget,
		})
		return &platform.IssueEvent{
			Repo:         repo,
			IssueNumber:  issueNumber,
			EventType:    "cross_referenced",
			Author:       event.Actor,
			Summary:      fmt.Sprintf("Referenced from %s/%s#%d", event.SourceOwner, event.SourceRepo, event.SourceNumber),
			MetadataJSON: string(metadata),
			CreatedAt:    event.CreatedAt,
			DedupeKey:    timelineDedupeKey(event),
		}
	case "closed", "reopened":
		return &platform.IssueEvent{
			Repo:        repo,
			IssueNumber: issueNumber,
			EventType:   event.EventType,
			Author:      event.Actor,
			Summary:     lifecycleSummary(event.EventType),
			CreatedAt:   event.CreatedAt,
			DedupeKey:   timelineDedupeKey(event),
		}
	default:
		return nil
	}
}

func NormalizeIssueCommentEvent(
	repo platform.RepoRef,
	issueNumber int,
	c *gh.IssueComment,
) platform.IssueEvent {
	event := normalizeIssueCommentBase(repo, c)
	return platform.IssueEvent{
		Repo:               repo,
		IssueNumber:        issueNumber,
		PlatformID:         event.PlatformID,
		PlatformExternalID: event.PlatformExternalID,
		EventType:          event.EventType,
		Author:             event.Author,
		Summary:            event.Summary,
		Body:               event.Body,
		CreatedAt:          event.CreatedAt,
		DedupeKey:          fmt.Sprintf("issue-comment-%d", c.GetID()),
		DirectURL:          event.DirectURL,
	}
}

func NormalizeCIChecks(
	repo platform.RepoRef,
	runs []*gh.CheckRun,
	combined *gh.CombinedStatus,
) []platform.CICheck {
	return normalizeCIChecks(repo, runs, combinedStatuses(combined))
}

func DeriveOverallCIStatus(checks []platform.CICheck, combined *gh.CombinedStatus) string {
	status := deriveCIStatusFromChecks(checks)
	if combined == nil || combined.GetTotalCount() == 0 {
		return status
	}
	switch combined.GetState() {
	case "failure", "error":
		return "failure"
	case "pending":
		if status != "failure" {
			return "pending"
		}
	case "success":
		if status == "" {
			return "success"
		}
	}
	return status
}

type forcePushMetadata struct {
	BeforeSHA string `json:"before_sha"`
	AfterSHA  string `json:"after_sha"`
	Ref       string `json:"ref"`
}

type commentDeletedMetadata struct {
	DeletedCommentAuthor string `json:"deleted_comment_author"`
}

type crossReferenceMetadata struct {
	SourceType        string `json:"source_type"`
	SourceOwner       string `json:"source_owner"`
	SourceRepo        string `json:"source_repo"`
	SourceNumber      int    `json:"source_number"`
	SourceTitle       string `json:"source_title"`
	SourceURL         string `json:"source_url"`
	IsCrossRepository bool   `json:"is_cross_repository"`
	WillCloseTarget   bool   `json:"will_close_target"`
}

type renamedTitleMetadata struct {
	PreviousTitle string `json:"previous_title"`
	CurrentTitle  string `json:"current_title"`
}

type baseRefChangedMetadata struct {
	PreviousRefName string `json:"previous_ref_name"`
	CurrentRefName  string `json:"current_ref_name"`
}

type assignmentMetadata struct {
	Assignee string `json:"assignee"`
}

func assignmentSummary(eventType, actor, assignee string) string {
	switch eventType {
	case "assigned":
		if actor != "" && actor == assignee {
			return "self-assigned this"
		}
		if assignee != "" {
			return "assigned " + assignee
		}
		return "assigned someone"
	case "unassigned":
		if actor != "" && actor == assignee {
			return "unassigned themselves"
		}
		if assignee != "" {
			return "unassigned " + assignee
		}
		return "removed an assignment"
	default:
		return ""
	}
}

type ciCheckCandidate struct {
	check platform.CICheck
	key   string
	at    time.Time
	id    int64
	order int
}

func normalizeCIChecks(
	repo platform.RepoRef,
	runs []*gh.CheckRun,
	statuses []*gh.RepoStatus,
) []platform.CICheck {
	candidates := make([]ciCheckCandidate, 0, len(runs)+len(statuses))
	for _, r := range runs {
		candidates = append(candidates, ciCheckCandidate{
			check: platform.CICheck{
				Repo:       repo,
				PlatformID: r.GetID(),
				Name:       r.GetName(),
				Status:     r.GetStatus(),
				Conclusion: r.GetConclusion(),
				URL:        r.GetHTMLURL(),
				App:        appName(r),
				StartedAt:  timePtr(timestampTime(r.StartedAt)),
				CompletedAt: timePtr(
					timestampTime(r.CompletedAt),
				),
			},
			key:   checkRunDedupeKey(r),
			at:    checkRunRecency(r),
			id:    r.GetID(),
			order: len(candidates),
		})
	}
	for _, s := range statuses {
		candidates = append(candidates, ciCheckCandidate{
			check: platform.CICheck{
				Repo:       repo,
				PlatformID: s.GetID(),
				Name:       s.GetContext(),
				Status:     repoStatusCheckState(s),
				Conclusion: repoStatusConclusion(s),
				URL:        sanitizeURL(s.GetTargetURL()),
				App:        s.GetContext(),
			},
			key:   repoStatusDedupeKey(s),
			at:    repoStatusRecency(s),
			id:    s.GetID(),
			order: len(candidates),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	byName := make(map[string]ciCheckCandidate, len(candidates))
	orderedKeys := make([]string, 0, len(candidates))
	checks := make([]platform.CICheck, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.key == "" {
			checks = append(checks, candidate.check)
			continue
		}
		existing, ok := byName[candidate.key]
		if !ok {
			orderedKeys = append(orderedKeys, candidate.key)
			byName[candidate.key] = candidate
			continue
		}
		if ciCheckCandidateIsNewer(existing, candidate) {
			byName[candidate.key] = candidate
		}
	}
	for _, key := range orderedKeys {
		checks = append(checks, byName[key].check)
	}
	sortCIChecksByName(checks)
	return checks
}

func NormalizeLabels(repo platform.RepoRef, labels []*gh.Label) []platform.Label {
	return normalizeLabels(repo, labels)
}

func normalizeLabels(repo platform.RepoRef, labels []*gh.Label) []platform.Label {
	if len(labels) == 0 {
		return nil
	}
	out := make([]platform.Label, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		name := strings.TrimSpace(l.GetName())
		if name == "" {
			continue
		}
		out = append(out, platform.Label{
			Repo:               repo,
			PlatformID:         l.GetID(),
			PlatformExternalID: l.GetNodeID(),
			Name:               name,
			Description:        l.GetDescription(),
			Color:              l.GetColor(),
			IsDefault:          l.GetDefault(),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeIssueCommentBase(repo platform.RepoRef, c *gh.IssueComment) platform.MergeRequestEvent {
	event := platform.MergeRequestEvent{
		Repo:               repo,
		PlatformID:         c.GetID(),
		PlatformExternalID: c.GetNodeID(),
		EventType:          "issue_comment",
		Author:             loginOrEmpty(c.GetUser()),
		Body:               c.GetBody(),
		DirectURL:          c.GetHTMLURL(),
	}
	if c.CreatedAt != nil {
		event.CreatedAt = c.CreatedAt.Time
	}
	return event
}

func deletedCommentSummary(author string) string {
	if author == "" {
		return "deleted a comment"
	}
	return "deleted a comment from " + author
}

func timelineDedupeKey(event PullRequestTimelineEvent) string {
	if event.NodeID != "" {
		return "timeline-" + event.NodeID
	}
	raw := strings.Join([]string{
		event.EventType,
		event.CreatedAt.UTC().Format(time.RFC3339Nano),
		event.Actor,
		event.Assignee,
		event.DeletedCommentAuthor,
		event.BeforeSHA,
		event.AfterSHA,
		event.Ref,
		event.PreviousTitle,
		event.CurrentTitle,
		event.PreviousRefName,
		event.CurrentRefName,
		event.SourceType,
		event.SourceOwner,
		event.SourceRepo,
		fmt.Sprint(event.SourceNumber),
		event.SourceURL,
		fmt.Sprint(event.IsCrossRepository),
		fmt.Sprint(event.WillCloseTarget),
	}, "\x00")
	return "timeline-" + shortHash(raw)
}

func deriveCIStatusFromChecks(checks []platform.CICheck) string {
	if len(checks) == 0 {
		return ""
	}
	hasPending := false
	hasFailed := false
	for _, c := range checks {
		if c.Status != "completed" {
			hasPending = true
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			if c.Conclusion != "" {
				hasFailed = true
			}
		}
	}
	if hasFailed {
		return "failure"
	}
	if hasPending {
		return "pending"
	}
	return "success"
}

func ciCheckCandidateIsNewer(existing, candidate ciCheckCandidate) bool {
	if existing.at.IsZero() != candidate.at.IsZero() {
		return existing.at.IsZero()
	}
	if !existing.at.Equal(candidate.at) {
		return candidate.at.After(existing.at)
	}
	if existing.id != candidate.id {
		return candidate.id > existing.id
	}
	return candidate.order > existing.order
}

func combinedStatuses(combined *gh.CombinedStatus) []*gh.RepoStatus {
	if combined == nil {
		return nil
	}
	return combined.Statuses
}

func checkRunRecency(r *gh.CheckRun) time.Time {
	completedAt := timestampTime(r.CompletedAt)
	if !completedAt.IsZero() {
		return completedAt
	}
	startedAt := timestampTime(r.StartedAt)
	if !startedAt.IsZero() {
		return startedAt
	}
	if suite := r.GetCheckSuite(); suite != nil {
		return timestampTime(suite.CreatedAt)
	}
	return time.Time{}
}

func checkRunDedupeKey(r *gh.CheckRun) string {
	name := r.GetName()
	if name == "" {
		return ""
	}
	return "check-run\x00" + appName(r) + "\x00" + name
}

func repoStatusDedupeKey(s *gh.RepoStatus) string {
	context := s.GetContext()
	if context == "" {
		return ""
	}
	return "status\x00" + context
}

func repoStatusRecency(s *gh.RepoStatus) time.Time {
	updatedAt := timestampTime(s.UpdatedAt)
	if !updatedAt.IsZero() {
		return updatedAt
	}
	return timestampTime(s.CreatedAt)
}

func timestampTime(t *gh.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func repoStatusCheckState(s *gh.RepoStatus) string {
	switch s.GetState() {
	case "pending", "expected":
		return "in_progress"
	default:
		return "completed"
	}
}

func repoStatusConclusion(s *gh.RepoStatus) string {
	switch s.GetState() {
	case "pending", "expected":
		return ""
	case "failure", "error":
		return "failure"
	default:
		return s.GetState()
	}
}

func sortCIChecksByName(checks []platform.CICheck) {
	slices.SortStableFunc(checks, func(left, right platform.CICheck) int {
		leftFolded := strings.ToLower(left.Name)
		rightFolded := strings.ToLower(right.Name)
		if leftFolded != rightFolded {
			return strings.Compare(leftFolded, rightFolded)
		}
		return strings.Compare(left.Name, right.Name)
	})
}

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

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func appName(r *gh.CheckRun) string {
	if r.GetApp() != nil {
		return r.GetApp().GetName()
	}
	return ""
}

func loginOrEmpty(u *gh.User) string {
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

func nameOrEmpty(u *gh.User) string {
	if u == nil {
		return ""
	}
	if u.GetType() == "Bot" {
		return u.GetLogin()
	}
	return sanitizeDisplayName(u.GetName())
}

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
