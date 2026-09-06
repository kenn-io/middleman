package gitealike

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/platform"
)

func NormalizeRepository(
	kind platform.Kind,
	host string,
	repo RepositoryDTO,
) (platform.Repository, error) {
	repoPath := strings.TrimSpace(repo.FullName)
	if repoPath == "" {
		repoPath = OwnerRepoPath(repo.Owner.UserName, repo.Name)
	}
	owner, name, err := splitRepoPath(repoPath, repo.Name)
	if err != nil {
		return platform.Repository{}, err
	}
	ref := platform.RepoRef{
		Platform:           kind,
		Host:               host,
		Owner:              owner,
		Name:               name,
		RepoPath:           repoPath,
		PlatformID:         repo.ID,
		PlatformExternalID: strconv.FormatInt(repo.ID, 10),
		WebURL:             repo.HTMLURL,
		CloneURL:           repo.CloneURL,
		DefaultBranch:      repo.DefaultBranch,
	}
	return platform.Repository{
		Ref:                ref,
		PlatformID:         repo.ID,
		PlatformExternalID: strconv.FormatInt(repo.ID, 10),
		Description:        repo.Description,
		Private:            repo.Private,
		Archived:           repo.Archived,
		Features: platform.RepositoryFeatures{
			IssuesEnabled:        repo.IssuesEnabled,
			MergeRequestsEnabled: repo.MergeRequestsEnabled,
		},
		MergeSettings: &platform.RepositoryMergeSettings{
			AllowSquashMerge: repo.AllowSquash,
			AllowMergeCommit: repo.AllowMerge,
			AllowRebaseMerge: repo.AllowRebase,
		},
		ViewerCanMerge: viewerCanMerge(repo.CanPush, repo.CanAdmin),
		DefaultBranch:  repo.DefaultBranch,
		WebURL:         repo.HTMLURL,
		CloneURL:       repo.CloneURL,
		CreatedAt:      repo.Created.UTC(),
		UpdatedAt:      repo.Updated.UTC(),
	}, nil
}

func viewerCanMerge(canPush, canAdmin *bool) *bool {
	if canPush == nil && canAdmin == nil {
		return nil
	}
	canMerge := (canPush != nil && *canPush) || (canAdmin != nil && *canAdmin)
	return &canMerge
}

func NormalizePullRequest(repo platform.RepoRef, pr PullRequestDTO) platform.MergeRequest {
	state := NormalizeState(pr.State)
	if pr.Merged || pr.MergedAt != nil {
		state = "merged"
	}
	return platform.MergeRequest{
		Repo:               repo,
		PlatformID:         pr.ID,
		PlatformExternalID: strconv.FormatInt(pr.ID, 10),
		Number:             pr.Index,
		URL:                pr.HTMLURL,
		Title:              pr.Title,
		Author:             pr.User.UserName,
		AuthorDisplayName:  pr.User.FullName,
		State:              state,
		IsDraft:            pr.Draft,
		IsLocked:           pr.IsLocked,
		Body:               pr.Body,
		HeadBranch:         pr.Head.Ref,
		BaseBranch:         pr.Base.Ref,
		HeadSHA:            pr.Head.SHA,
		BaseSHA:            pr.Base.SHA,
		HeadRepoCloneURL:   pr.Head.RepoCloneURL,
		CommentCount:       pr.Comments,
		MergeableState:     normalizeMergeable(pr.Mergeable),
		Additions:          pr.Additions,
		AdditionsKnown:     pr.AdditionsKnown,
		Deletions:          pr.Deletions,
		DeletionsKnown:     pr.DeletionsKnown,
		FilesChanged:       pr.FilesChanged,
		CreatedAt:          pr.Created.UTC(),
		UpdatedAt:          pr.Updated.UTC(),
		LastActivityAt:     pr.Updated.UTC(),
		MergedAt:           timePtrUTC(pr.MergedAt),
		MergedBy:           pr.MergedBy.UserName,
		ClosedAt:           timePtrUTC(pr.Closed),
		Labels:             NormalizeLabels(repo, pr.Labels),
		Assignees:          userDTONames(pr.Assignees),
		RequestedReviewers: userDTONames(pr.RequestedReviewers),
	}
}

// userDTONames preserves nil (field unknown to the transport) versus an
// empty non-nil slice (provider-confirmed empty set).
func userDTONames(users []UserDTO) []string {
	if users == nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user.UserName != "" {
			out = append(out, user.UserName)
		}
	}
	return out
}

func normalizeMergeable(mergeable *bool) string {
	if mergeable == nil {
		return ""
	}
	if *mergeable {
		return "clean"
	}
	return "dirty"
}

func NormalizeIssue(repo platform.RepoRef, issue IssueDTO) platform.Issue {
	var assignees []string
	for _, a := range issue.Assignees {
		if a.UserName != "" {
			assignees = append(assignees, a.UserName)
		}
	}

	return platform.Issue{
		Repo:               repo,
		PlatformID:         issue.ID,
		PlatformExternalID: strconv.FormatInt(issue.ID, 10),
		Number:             issue.Index,
		URL:                issue.HTMLURL,
		Title:              issue.Title,
		Author:             issue.User.UserName,
		State:              NormalizeState(issue.State),
		Body:               issue.Body,
		CommentCount:       issue.Comments,
		CreatedAt:          issue.Created.UTC(),
		UpdatedAt:          issue.Updated.UTC(),
		LastActivityAt:     issue.Updated.UTC(),
		ClosedAt:           timePtrUTC(issue.Closed),
		Labels:             NormalizeLabels(repo, issue.Labels),
		Assignees:          assignees,
	}
}

func NormalizeLabels(repo platform.RepoRef, labels []LabelDTO) []platform.Label {
	if len(labels) == 0 {
		return nil
	}
	out := make([]platform.Label, 0, len(labels))
	for _, label := range labels {
		out = append(out, platform.Label{
			Repo:               repo,
			PlatformID:         label.ID,
			PlatformExternalID: strconv.FormatInt(label.ID, 10),
			Name:               label.Name,
			Description:        label.Description,
			Color:              label.Color,
			IsDefault:          label.IsDefault,
		})
	}
	return out
}

func NormalizeIssueComments(
	kind platform.Kind,
	repo platform.RepoRef,
	number int,
	comments []CommentDTO,
) []platform.IssueEvent {
	events := make([]platform.IssueEvent, 0, len(comments))
	for _, comment := range comments {
		events = append(events, platform.IssueEvent{
			Repo:               repo,
			PlatformID:         comment.ID,
			PlatformExternalID: strconv.FormatInt(comment.ID, 10),
			IssueNumber:        number,
			EventType:          "issue_comment",
			Author:             comment.User.UserName,
			Body:               comment.Body,
			CreatedAt:          comment.Created.UTC(),
			DirectURL:          comment.HTMLURL,
			DedupeKey: NoteDedupeKey(
				kind, repo.Host, repo.RepoPath, "issue", number, "issue_comment",
				strconv.FormatInt(comment.ID, 10),
			),
		})
	}
	return events
}

func NormalizeIssueTimelineEvents(
	kind platform.Kind,
	repo platform.RepoRef,
	number int,
	timeline []TimelineEventDTO,
) []platform.IssueEvent {
	events := make([]platform.IssueEvent, 0, len(timeline))
	for _, item := range timeline {
		eventType, summary, metadataJSON, ok := normalizeTimelineEvent(repo, item)
		if !ok {
			continue
		}
		externalID := strconv.FormatInt(item.ID, 10)
		events = append(events, platform.IssueEvent{
			Repo:               repo,
			PlatformID:         item.ID,
			PlatformExternalID: externalID,
			IssueNumber:        number,
			EventType:          eventType,
			Author:             item.User.UserName,
			Summary:            summary,
			MetadataJSON:       metadataJSON,
			CreatedAt:          item.Created.UTC(),
			DirectURL:          item.HTMLURL,
			DedupeKey: NoteDedupeKey(
				kind, repo.Host, repo.RepoPath, "issue", number, eventType,
				externalID,
			),
		})
	}
	return events
}

func NormalizeMergeRequestEvents(
	kind platform.Kind,
	repo platform.RepoRef,
	number int,
	comments []CommentDTO,
	reviews []ReviewDTO,
	commits []CommitDTO,
) []platform.MergeRequestEvent {
	events := make([]platform.MergeRequestEvent, 0, len(comments)+len(reviews)+len(commits))
	for _, comment := range comments {
		externalID := strconv.FormatInt(comment.ID, 10)
		events = append(events, platform.MergeRequestEvent{
			Repo:               repo,
			PlatformID:         comment.ID,
			PlatformExternalID: externalID,
			MergeRequestNumber: number,
			EventType:          "issue_comment",
			Author:             comment.User.UserName,
			Body:               comment.Body,
			CreatedAt:          comment.Created.UTC(),
			DirectURL:          comment.HTMLURL,
			DedupeKey:          NoteDedupeKey(kind, repo.Host, repo.RepoPath, "mr", number, "issue_comment", externalID),
		})
	}
	for _, review := range reviews {
		externalID := strconv.FormatInt(review.ID, 10)
		events = append(events, platform.MergeRequestEvent{
			Repo:               repo,
			PlatformID:         review.ID,
			PlatformExternalID: externalID,
			MergeRequestNumber: number,
			EventType:          "review",
			Author:             review.User.UserName,
			Summary:            review.State,
			Body:               review.Body,
			CreatedAt:          review.Submitted.UTC(),
			DedupeKey:          NoteDedupeKey(kind, repo.Host, repo.RepoPath, "mr", number, "review", externalID),
		})
	}
	for _, commit := range commits {
		events = append(events, platform.MergeRequestEvent{
			Repo:               repo,
			PlatformExternalID: commit.SHA,
			MergeRequestNumber: number,
			EventType:          "commit",
			Author:             commit.AuthorName,
			Summary:            commit.Message,
			CreatedAt:          commit.Created.UTC(),
			DedupeKey:          NoteDedupeKey(kind, repo.Host, repo.RepoPath, "mr", number, "commit", commit.SHA),
		})
	}
	return events
}

func NormalizeMergeRequestTimelineEvents(
	kind platform.Kind,
	repo platform.RepoRef,
	number int,
	timeline []TimelineEventDTO,
) []platform.MergeRequestEvent {
	events := make([]platform.MergeRequestEvent, 0, len(timeline))
	for _, item := range timeline {
		eventType, summary, metadataJSON, ok := normalizeTimelineEvent(repo, item)
		if !ok {
			continue
		}
		externalID := strconv.FormatInt(item.ID, 10)
		events = append(events, platform.MergeRequestEvent{
			Repo:               repo,
			PlatformID:         item.ID,
			PlatformExternalID: externalID,
			MergeRequestNumber: number,
			EventType:          eventType,
			Author:             item.User.UserName,
			Summary:            summary,
			MetadataJSON:       metadataJSON,
			CreatedAt:          item.Created.UTC(),
			DirectURL:          item.HTMLURL,
			DedupeKey: NoteDedupeKey(
				kind, repo.Host, repo.RepoPath, "mr", number, eventType,
				externalID,
			),
		})
	}
	return events
}

type renamedTitleMetadata struct {
	PreviousTitle string `json:"previous_title"`
	CurrentTitle  string `json:"current_title"`
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

func normalizeTimelineEvent(repo platform.RepoRef, item TimelineEventDTO) (eventType, summary, metadataJSON string, ok bool) {
	if strings.EqualFold(strings.TrimSpace(item.Type), "pull_ref") && item.Reference != nil && item.Reference.IsPullRequest {
		ref := item.Reference
		if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Repo) == "" ||
			ref.Number <= 0 || strings.TrimSpace(ref.HTMLURL) == "" {
			return "", "", "", false
		}
		parsedSourceURL, err := url.Parse(ref.HTMLURL)
		if err != nil || (repo.Host != "" && !strings.EqualFold(parsedSourceURL.Host, repo.Host)) {
			return "", "", "", false
		}
		metadata, _ := json.Marshal(crossReferenceMetadata{
			SourceType:        "PullRequest",
			SourceOwner:       ref.Owner,
			SourceRepo:        ref.Repo,
			SourceNumber:      ref.Number,
			SourceTitle:       ref.Title,
			SourceURL:         ref.HTMLURL,
			IsCrossRepository: path.Join(ref.Owner, ref.Repo) != repo.RepoPath,
			WillCloseTarget:   strings.EqualFold(strings.TrimSpace(item.RefAction), "closes"),
		})
		return "cross_referenced", fmt.Sprintf("Referenced from %s/%s#%d", ref.Owner, ref.Repo, ref.Number), string(metadata), true
	}

	eventType, ok = normalizeAssignmentEventType(item.Type)
	if ok {
		return eventType, assignmentSummary(eventType, item.User.UserName, item.Assignee.UserName), "", true
	}

	eventType, ok = normalizeLifecycleEventType(item.Type)
	if ok {
		return eventType, lifecycleSummary(eventType), "", true
	}

	if !isTitleChangeEvent(item.Type) {
		return "", "", "", false
	}
	metadata, _ := json.Marshal(renamedTitleMetadata{
		PreviousTitle: item.PreviousTitle,
		CurrentTitle:  item.CurrentTitle,
	})
	return "renamed_title", fmt.Sprintf("%q -> %q", item.PreviousTitle, item.CurrentTitle), string(metadata), true
}

func normalizeAssignmentEventType(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "assigned":
		return "assigned", true
	case "unassigned":
		return "unassigned", true
	default:
		return "", false
	}
}

func normalizeLifecycleEventType(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "merged":
		return "merged", true
	case "closed", "close":
		return "closed", true
	case "reopened", "reopen":
		return "reopened", true
	default:
		return "", false
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

func isTitleChangeEvent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "change_title", "renamed_title":
		return true
	default:
		return false
	}
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

func NormalizeRelease(repo platform.RepoRef, release ReleaseDTO) platform.Release {
	return platform.Release{
		Repo:               repo,
		PlatformID:         release.ID,
		PlatformExternalID: strconv.FormatInt(release.ID, 10),
		TagName:            release.TagName,
		Name:               release.Title,
		URL:                release.HTMLURL,
		TargetCommitish:    release.Target,
		Prerelease:         release.Prerelease,
		PublishedAt:        timePtrUTC(release.PublishedAt),
		CreatedAt:          release.CreatedAt.UTC(),
	}
}

func NormalizeTag(repo platform.RepoRef, tag TagDTO) platform.Tag {
	return platform.Tag{
		Repo:               repo,
		PlatformExternalID: tag.Commit.SHA,
		Name:               tag.Name,
		SHA:                tag.Commit.SHA,
		URL:                tag.URL,
	}
}

func NormalizeStatuses(
	repo platform.RepoRef,
	statuses []StatusDTO,
	actionRuns []ActionRunDTO,
) []platform.CICheck {
	checks := make([]platform.CICheck, 0, len(statuses)+len(actionRuns))
	statusURLs := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		checkStatus, conclusion := NormalizeCommitStatus(status.State)
		if strings.TrimSpace(status.TargetURL) != "" {
			statusURLs[casefoldKey(status.TargetURL)] = struct{}{}
		}
		checks = append(checks, platform.CICheck{
			Repo:               repo,
			PlatformID:         status.ID,
			PlatformExternalID: strconv.FormatInt(status.ID, 10),
			Name:               status.Context,
			Status:             checkStatus,
			Conclusion:         conclusion,
			URL:                safeLinkURL(status.TargetURL),
			App:                "status",
			StartedAt:          timePtr(status.Created),
			CompletedAt:        timePtr(status.Updated),
		})
	}
	for _, run := range latestActionRuns(actionRuns) {
		name := actionRunName(run)
		if strings.TrimSpace(run.HTMLURL) != "" {
			if _, ok := statusURLs[casefoldKey(run.HTMLURL)]; ok {
				continue
			}
		}
		checkStatus, conclusion := NormalizeActionRunStatus(run.Status, run.Conclusion)
		checks = append(checks, platform.CICheck{
			Repo:               repo,
			PlatformID:         run.ID,
			PlatformExternalID: strconv.FormatInt(run.ID, 10),
			Name:               name,
			Status:             checkStatus,
			Conclusion:         conclusion,
			URL:                safeLinkURL(run.HTMLURL),
			App:                "action",
			StartedAt:          timePtrUTC(run.Started),
			CompletedAt:        timePtrUTC(run.Stopped),
		})
	}
	return checks
}

func safeLinkURL(rawURL string) string {
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

func latestActionRuns(actionRuns []ActionRunDTO) []ActionRunDTO {
	latest := make(map[string]ActionRunDTO, len(actionRuns))
	order := make([]string, 0, len(actionRuns))
	for _, run := range actionRuns {
		key := actionRunKey(run)
		if _, seen := latest[key]; !seen {
			order = append(order, key)
			latest[key] = run
			continue
		}
		if actionRunIsNewer(run, latest[key]) {
			latest[key] = run
		}
	}
	out := make([]ActionRunDTO, 0, len(latest))
	for _, key := range order {
		out = append(out, latest[key])
	}
	return out
}

func actionRunKey(run ActionRunDTO) string {
	if workflowID := strings.TrimSpace(run.WorkflowID); workflowID != "" {
		return "workflow:" + casefoldKey(workflowID)
	}
	if name := actionRunName(run); name != "" {
		return "name:" + casefoldKey(name)
	}
	return "id:" + strconv.FormatInt(run.ID, 10)
}

func actionRunIsNewer(candidate, current ActionRunDTO) bool {
	if candidate.RunNumber != 0 && current.RunNumber != 0 && candidate.RunNumber != current.RunNumber {
		return candidate.RunNumber > current.RunNumber
	}
	candidateTime := actionRunSortTime(candidate)
	currentTime := actionRunSortTime(current)
	if !candidateTime.Equal(currentTime) {
		return candidateTime.After(currentTime)
	}
	return candidate.ID > current.ID
}

func actionRunSortTime(run ActionRunDTO) time.Time {
	if !run.Updated.IsZero() {
		return run.Updated.UTC()
	}
	if !run.Created.IsZero() {
		return run.Created.UTC()
	}
	if run.Stopped != nil && !run.Stopped.IsZero() {
		return run.Stopped.UTC()
	}
	if run.Started != nil && !run.Started.IsZero() {
		return run.Started.UTC()
	}
	return time.Time{}
}

func actionRunName(run ActionRunDTO) string {
	if title := strings.TrimSpace(run.Title); title != "" {
		return title
	}
	return strings.TrimSpace(run.WorkflowID)
}

func NormalizeState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened":
		return "open"
	case "closed":
		return "closed"
	case "merged":
		return "merged"
	default:
		return strings.TrimSpace(state)
	}
}

func OwnerRepoPath(owner, name string) string {
	if owner == "" {
		return name
	}
	return owner + "/" + name
}

func NoteDedupeKey(
	kind platform.Kind,
	host string,
	repoPath string,
	parentKind string,
	number int,
	eventKind string,
	externalID string,
) string {
	return fmt.Sprintf("%s/%s/%s/%s/%d/%s/%s", kind, host, repoPath, parentKind, number, eventKind, externalID)
}

func NormalizeCommitStatus(state string) (status string, conclusion string) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pending", "running", "queued", "waiting":
		return "pending", ""
	case "success", "successful", "passed":
		return "completed", "success"
	case "failure", "failed", "error", "cancelled", "canceled":
		return "completed", "failure"
	case "skipped":
		return "completed", "skipped"
	default:
		return NormalizeState(state), ""
	}
}

func NormalizeActionRunStatus(status, conclusion string) (string, string) {
	normalizedStatus, normalizedConclusion := NormalizeCommitStatus(status)
	if normalizedStatus == "pending" {
		return normalizedStatus, ""
	}
	if strings.TrimSpace(conclusion) == "" {
		return normalizedStatus, normalizedConclusion
	}
	return "completed", NormalizeCIConclusion(conclusion)
}

func NormalizeCIConclusion(conclusion string) string {
	normalized := strings.ToLower(strings.TrimSpace(conclusion))
	switch normalized {
	case "success", "successful", "passed":
		return "success"
	case "failure", "failed", "error", "cancelled", "canceled", "timed_out":
		return "failure"
	case "skipped", "neutral":
		return normalized
	default:
		return normalized
	}
}

func casefoldKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func NextPage(next int) int {
	if next < 0 {
		return 0
	}
	return next
}

func splitRepoPath(repoPath, fallbackName string) (string, string, error) {
	parts := strings.Split(repoPath, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid repository path %q", repoPath)
	}
	name := fallbackName
	if name == "" {
		name = parts[len(parts)-1]
	}
	return strings.Join(parts[:len(parts)-1], "/"), name, nil
}

func timePtrUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
