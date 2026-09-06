package platformdb

import (
	"encoding/json"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

// MarshalAssigneesJSON converts a list of assignee usernames to a JSON array string.
// Returns "[]" if the list is empty or if marshaling fails.
func MarshalAssigneesJSON(assignees []string) string {
	if len(assignees) == 0 {
		return "[]"
	}
	if b, err := json.Marshal(assignees); err == nil {
		return string(b)
	}
	return "[]"
}

func DBReviewThreads(threads []platform.MergeRequestReviewThread) ([]db.MREvent, []db.MRReviewThread) {
	events := make([]db.MREvent, 0, len(threads))
	dbThreads := make([]db.MRReviewThread, 0, len(threads))
	seenThreads := make(map[string]struct{}, len(threads))
	for _, thread := range threads {
		threadID := firstNonEmptyString(thread.ProviderThreadID, thread.ProviderCommentID)
		if threadID == "" {
			continue
		}
		if _, seen := seenThreads[threadID]; !seen {
			seenThreads[threadID] = struct{}{}
			dbThreads = append(dbThreads, db.MRReviewThread{
				ProviderThreadID: threadID, ProviderReviewID: thread.ProviderReviewID,
				ProviderCommentID: thread.ProviderCommentID, Body: thread.Body,
				AuthorLogin: thread.AuthorLogin, Range: DBReviewLineRange(thread.Range),
				Resolved: thread.Resolved, CreatedAt: thread.CreatedAt,
				UpdatedAt: thread.UpdatedAt, ResolvedAt: thread.ResolvedAt,
				MetadataJSON: thread.MetadataJSON,
			})
		}
		externalID := firstNonEmptyString(thread.ProviderCommentID, threadID)
		if externalID == "" {
			continue
		}
		threadIDCopy := threadID
		events = append(events, db.MREvent{
			PlatformExternalID: externalID, EventType: "review_comment",
			Author: thread.AuthorLogin, Body: thread.Body, CreatedAt: thread.CreatedAt,
			MetadataJSON: thread.MetadataJSON,
			DedupeKey:    "review_comment:" + externalID, DirectURL: thread.DirectURL,
			ThreadID: &threadIDCopy,
		})
	}
	return events, dbThreads
}

func DBReviewLineRange(input platform.DiffReviewLineRange) db.ReviewLineRange {
	return db.ReviewLineRange{
		Path: input.Path, OldPath: input.OldPath, Side: input.Side,
		StartSide: input.StartSide, StartLine: input.StartLine, Line: input.Line,
		OldLine: input.OldLine, NewLine: input.NewLine, LineType: input.LineType,
		DiffHeadSHA: input.DiffHeadSHA, CommitSHA: input.CommitSHA,
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// MarshalUserNamesJSON converts a username list to a JSON array string,
// preserving the unknown state: nil returns "" so merge-request upserts
// keep the previously stored value, while an empty non-nil slice returns
// "[]" to record that the provider reported no users.
func MarshalUserNamesJSON(names []string) string {
	if names == nil {
		return ""
	}
	if b, err := json.Marshal(names); err == nil {
		return string(b)
	}
	return ""
}

// PreserveProviderHiddenMetadata keeps moderation metadata when a provider
// mutation response omits visibility fields. REST edit responses do not carry
// GitHub's GraphQL-only minimized state, so an edit must not clear a previously
// confirmed hidden marker until a subsequent GraphQL sync explicitly observes
// the comment as visible.
func PreserveProviderHiddenMetadata(existingMetadata, incomingMetadata string) string {
	if incomingMetadata != "" || existingMetadata == "" {
		return incomingMetadata
	}
	var existing struct {
		Hidden bool `json:"provider_hidden"`
	}
	if err := json.Unmarshal([]byte(existingMetadata), &existing); err != nil || !existing.Hidden {
		return incomingMetadata
	}
	return existingMetadata
}

func DBRepoIdentity(ref platform.RepoRef) db.RepoIdentity {
	return db.RepoIdentity{
		Platform:       string(ref.Platform),
		PlatformHost:   ref.Host,
		PlatformRepoID: ref.PlatformExternalID,
		Owner:          ref.Owner,
		Name:           ref.Name,
		RepoPath:       ref.RepoPath,
	}
}

func DBRepositoryIdentity(repo platform.Repository) db.RepoIdentity {
	identity := DBRepoIdentity(repo.Ref)
	if identity.PlatformRepoID == "" {
		identity.PlatformRepoID = repo.PlatformExternalID
	}
	return identity
}

func DBMergeRequest(repoID int64, mr platform.MergeRequest) *db.MergeRequest {
	out := &db.MergeRequest{
		RepoID:                  repoID,
		PlatformID:              mr.PlatformID,
		PlatformExternalID:      mr.PlatformExternalID,
		Number:                  mr.Number,
		URL:                     mr.URL,
		Title:                   mr.Title,
		Author:                  mr.Author,
		AuthorDisplayName:       mr.AuthorDisplayName,
		State:                   db.MergeRequestState(mr.State),
		IsDraft:                 mr.IsDraft,
		IsLocked:                mr.IsLocked,
		Body:                    mr.Body,
		HeadBranch:              mr.HeadBranch,
		BaseBranch:              mr.BaseBranch,
		PlatformHeadSHA:         mr.HeadSHA,
		PlatformBaseSHA:         mr.BaseSHA,
		HeadRepoCloneURL:        mr.HeadRepoCloneURL,
		HeadRepoCloneURLUnknown: mr.HeadRepoCloneURLUnknown,
		Additions:               mr.Additions,
		AdditionsKnown:          mr.AdditionsKnown,
		Deletions:               mr.Deletions,
		DeletionsKnown:          mr.DeletionsKnown,
		FilesChanged:            mr.FilesChanged,
		MergeCommitSHA:          mr.MergeCommitSHA,
		CommentCount:            mr.CommentCount,
		ReviewDecision:          mr.ReviewDecision,
		CIStatus:                mr.CIStatus,
		MergeableState:          mr.MergeableState,
		CreatedAt:               mr.CreatedAt,
		UpdatedAt:               mr.UpdatedAt,
		LastActivityAt:          mr.LastActivityAt,
		MergedAt:                mr.MergedAt,
		ClosedAt:                mr.ClosedAt,
		AssigneesJSON:           MarshalUserNamesJSON(mr.Assignees),
		ReviewersJSON:           MarshalUserNamesJSON(mr.RequestedReviewers),
	}
	out.Labels = DBLabels(mr.Labels, itemLabelUpdatedAt(mr.UpdatedAt, mr.CreatedAt))
	return out
}

func DBIssue(repoID int64, issue platform.Issue) *db.Issue {
	out := &db.Issue{
		RepoID:             repoID,
		PlatformID:         issue.PlatformID,
		PlatformExternalID: issue.PlatformExternalID,
		Number:             issue.Number,
		URL:                issue.URL,
		Title:              issue.Title,
		Author:             issue.Author,
		State:              issue.State,
		Body:               issue.Body,
		CommentCount:       issue.CommentCount,
		CreatedAt:          issue.CreatedAt,
		UpdatedAt:          issue.UpdatedAt,
		LastActivityAt:     issue.LastActivityAt,
		ClosedAt:           issue.ClosedAt,
		AssigneesJSON:      MarshalAssigneesJSON(issue.Assignees),
	}
	out.Labels = DBLabels(issue.Labels, itemLabelUpdatedAt(issue.UpdatedAt, issue.CreatedAt))
	return out
}

func DBMREvent(mrID int64, event platform.MergeRequestEvent) db.MREvent {
	out := db.MREvent{
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
		PositionJSON:       event.PositionJSON,
		Resolvable:         event.Resolvable,
		Resolved:           event.Resolved,
	}
	if event.PlatformID != 0 || event.EventType == "issue_comment" || event.EventType == "review" {
		platformID := event.PlatformID
		out.PlatformID = &platformID
	}
	if event.ThreadID != "" {
		out.ThreadID = &event.ThreadID
	}
	return out
}

func DBIssueEvent(issueID int64, event platform.IssueEvent) db.IssueEvent {
	out := db.IssueEvent{
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
		out.PlatformID = &platformID
	}
	if event.ThreadID != "" {
		out.ThreadID = &event.ThreadID
	}
	return out
}

func DBLabels(labels []platform.Label, updatedAt time.Time) []db.Label {
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

func DBCIChecks(checks []platform.CICheck) []db.CICheck {
	if len(checks) == 0 {
		return nil
	}
	out := make([]db.CICheck, 0, len(checks))
	for _, check := range checks {
		out = append(out, db.CICheck{
			Name:            check.Name,
			Status:          check.Status,
			Conclusion:      check.Conclusion,
			URL:             check.URL,
			App:             check.App,
			DurationSeconds: CICheckDurationSeconds(check),
		})
	}
	return out
}

func CICheckDurationSeconds(check platform.CICheck) *int64 {
	if check.StartedAt == nil || check.CompletedAt == nil {
		return nil
	}
	duration := check.CompletedAt.Sub(*check.StartedAt)
	if duration < 0 {
		return nil
	}
	seconds := int64(duration.Seconds())
	return &seconds
}

func itemLabelUpdatedAt(updatedAt, createdAt time.Time) time.Time {
	if !updatedAt.IsZero() {
		return updatedAt
	}
	return createdAt
}
