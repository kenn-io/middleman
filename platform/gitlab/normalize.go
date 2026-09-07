package gitlab

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func NormalizeProject(host string, p *gitlab.Project) (platform.Repository, error) {
	if p == nil {
		return platform.Repository{}, nil
	}

	repoPath, err := normalizeSafeProjectPath(p.PathWithNamespace)
	if err != nil {
		return platform.Repository{}, err
	}
	owner := path.Dir(repoPath)
	if owner == "." {
		owner = ""
	}
	name := p.Path
	if name == "" {
		name = path.Base(repoPath)
	}
	if err := validateSafeProjectName(name); err != nil {
		return platform.Repository{}, err
	}
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               host,
		Owner:              owner,
		Name:               name,
		RepoPath:           repoPath,
		PlatformID:         p.ID,
		PlatformExternalID: strconv.FormatInt(p.ID, 10),
		WebURL:             p.WebURL,
		CloneURL:           p.HTTPURLToRepo,
		DefaultBranch:      p.DefaultBranch,
	}
	return platform.Repository{
		Ref:                ref,
		PlatformID:         p.ID,
		PlatformExternalID: strconv.FormatInt(p.ID, 10),
		Description:        p.Description,
		Private:            p.Visibility == gitlab.PrivateVisibility,
		Archived:           p.Archived,
		Features: platform.RepositoryFeatures{
			IssuesEnabled:        gitLabFeatureEnabled(p.IssuesAccessLevel),
			MergeRequestsEnabled: gitLabFeatureEnabled(p.MergeRequestsAccessLevel),
		},
		ViewerCanMerge: gitLabViewerCanMerge(p.Permissions),
		// GitLab accepts MRs under the project's configured merge method
		// with squash as a per-merge flag; there is no per-merge rebase,
		// so clients must not offer "Rebase and merge". squash_option
		// bounds the per-merge squash flag in both directions: "never"
		// forbids squashing and "always" forbids accepting without it.
		MergeSettings: &platform.RepositoryMergeSettings{
			AllowSquashMerge: p.SquashOption != gitlab.SquashOptionNever,
			AllowMergeCommit: p.SquashOption != gitlab.SquashOptionAlways,
			AllowRebaseMerge: false,
		},
		DefaultBranch: p.DefaultBranch,
		WebURL:        p.WebURL,
		CloneURL:      p.HTTPURLToRepo,
		CreatedAt:     timeValue(p.CreatedAt),
		UpdatedAt:     timeValue(p.UpdatedAt),
	}, nil
}

func gitLabFeatureEnabled(accessLevel gitlab.AccessControlValue) *bool {
	var enabled bool
	switch accessLevel {
	case gitlab.DisabledAccessControl:
		enabled = false
	case gitlab.EnabledAccessControl, gitlab.PrivateAccessControl, gitlab.PublicAccessControl:
		enabled = true
	default:
		return nil
	}
	return &enabled
}

func gitLabViewerCanMerge(perms *gitlab.Permissions) *bool {
	if perms == nil {
		return nil
	}
	level := gitlab.NoPermissions
	if perms.ProjectAccess != nil && perms.ProjectAccess.AccessLevel > level {
		level = perms.ProjectAccess.AccessLevel
	}
	if perms.GroupAccess != nil && perms.GroupAccess.AccessLevel > level {
		level = perms.GroupAccess.AccessLevel
	}
	canMerge := level >= gitlab.DeveloperPermissions
	return &canMerge
}

func normalizeSafeProjectPath(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("unsafe GitLab project path %q", raw)
	}
	if strings.HasPrefix(raw, "/") || strings.Contains(raw, "\\") {
		return "", fmt.Errorf("unsafe GitLab project path %q", raw)
	}
	cleaned := path.Clean(raw)
	if cleaned != raw || cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("unsafe GitLab project path %q", raw)
	}
	parts := strings.Split(raw, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("unsafe GitLab project path %q", raw)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe GitLab project path %q", raw)
		}
	}
	return raw, nil
}

func validateSafeProjectName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("unsafe GitLab project path name %q", name)
	}
	return nil
}

func NormalizeMergeRequest(
	repo platform.RepoRef,
	mr *gitlab.BasicMergeRequest,
	pipeline *gitlab.PipelineInfo,
) platform.MergeRequest {
	return normalizeMergeRequest(repo, mr, pipeline, false)
}

func NormalizeDetailedMergeRequest(repo platform.RepoRef, mr *gitlab.MergeRequest) platform.MergeRequest {
	if mr == nil {
		return platform.MergeRequest{}
	}
	out := normalizeMergeRequest(repo, &mr.BasicMergeRequest, pipelineInfo(mr), detailedMRWorkInProgress(mr))
	// Only the detail payload carries diff_refs. BaseSHA feeds the
	// reviewed diff snapshot (merge-base + head pin); without it
	// head-bound mutations stay disabled with 409 head_unknown.
	if out.HeadSHA == "" {
		out.HeadSHA = mr.DiffRefs.HeadSha
	}
	out.BaseSHA = mr.DiffRefs.BaseSha
	return out
}

func detailedMRWorkInProgress(mr *gitlab.MergeRequest) bool {
	if mr == nil {
		return false
	}
	field := reflect.ValueOf(mr).Elem().FieldByName("WorkInProgress")
	return field.IsValid() && field.Kind() == reflect.Bool && field.Bool()
}

func normalizeMergeRequest(
	repo platform.RepoRef,
	mr *gitlab.BasicMergeRequest,
	pipeline *gitlab.PipelineInfo,
	workInProgress bool,
) platform.MergeRequest {
	if mr == nil {
		return platform.MergeRequest{}
	}
	out := platform.MergeRequest{
		Repo:               repo,
		PlatformID:         mr.ID,
		PlatformExternalID: strconv.FormatInt(mr.ID, 10),
		Number:             int(mr.IID),
		URL:                mr.WebURL,
		Title:              mr.Title,
		Author:             basicUsername(mr.Author),
		AuthorDisplayName:  sanitizeDisplayName(basicName(mr.Author)),
		State:              normalizeMergeRequestState(mr.State),
		IsDraft:            mr.Draft || workInProgress,
		Body:               mr.Description,
		HeadBranch:         mr.SourceBranch,
		BaseBranch:         mr.TargetBranch,
		HeadSHA:            mr.SHA,
		MergeCommitSHA:     firstNonEmpty(mr.MergeCommitSHA, mr.SquashCommitSHA),
		CommentCount:       int(mr.UserNotesCount),
		MergeableState:     normalizeMergeableState(mr.DetailedMergeStatus, mr.HasConflicts),
		CIStatus:           pipelineStatusFromInfo(pipeline),
		CreatedAt:          timeValue(mr.CreatedAt),
		UpdatedAt:          timeValue(mr.UpdatedAt),
		LastActivityAt:     timeValue(mr.UpdatedAt),
		MergedBy:           basicUsername(mr.MergeUser),
		Labels:             normalizeLabelNames(repo, mr.Labels),
		Assignees:          basicUsernames(mr.Assignees),
		RequestedReviewers: basicUsernames(mr.Reviewers),
	}
	if out.MergedBy == "" {
		//nolint:staticcheck // GitLab < 14.7 can populate merged_by without merge_user.
		out.MergedBy = basicUsername(mr.MergedBy)
	}
	if mr.MergedAt != nil {
		t := mr.MergedAt.UTC()
		out.MergedAt = &t
	}
	if mr.ClosedAt != nil {
		t := mr.ClosedAt.UTC()
		out.ClosedAt = &t
	}
	return out
}

func normalizeMergeableState(detailedStatus string, hasConflicts bool) string {
	if hasConflicts {
		return "dirty"
	}
	switch strings.ToLower(strings.TrimSpace(detailedStatus)) {
	case "":
		return ""
	case "mergeable":
		return "clean"
	case "conflict":
		return "dirty"
	case "checking", "unchecked", "approvals_syncing", "preparing":
		return "unknown"
	case "need_rebase":
		return "behind"
	case "ci_must_pass", "ci_still_running", "status_checks_must_pass",
		"security_policy_pipeline_check":
		return "unstable"
	case "draft_status":
		return "draft"
	case "commits_status", "discussions_not_resolved",
		"jira_association_missing", "merge_request_blocked",
		"merge_time", "not_approved", "not_open", "requested_changes",
		"security_policy_violations", "locked_paths", "locked_lfs_files",
		"title_regex":
		return "blocked"
	default:
		return "unknown"
	}
}

func NormalizeIssue(repo platform.RepoRef, issue *gitlab.Issue) platform.Issue {
	if issue == nil {
		return platform.Issue{}
	}
	var assignees []string
	for _, a := range issue.Assignees {
		if a != nil && a.Username != "" {
			assignees = append(assignees, a.Username)
		}
	}
	out := platform.Issue{
		Repo:               repo,
		PlatformID:         issue.ID,
		PlatformExternalID: strconv.FormatInt(issue.ID, 10),
		Number:             int(issue.IID),
		URL:                issue.WebURL,
		Title:              issue.Title,
		Author:             issueUsername(issue.Author),
		State:              normalizeIssueState(issue.State),
		Body:               issue.Description,
		CommentCount:       int(issue.UserNotesCount),
		CreatedAt:          timeValue(issue.CreatedAt),
		UpdatedAt:          timeValue(issue.UpdatedAt),
		LastActivityAt:     timeValue(issue.UpdatedAt),
		Labels:             normalizeLabelNames(repo, issue.Labels),
		Assignees:          assignees,
	}
	if issue.ClosedAt != nil {
		t := issue.ClosedAt.UTC()
		out.ClosedAt = &t
	}
	return out
}

func normalizeMergeRequestState(state string) string {
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

func normalizeIssueState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened":
		return "open"
	case "closed":
		return "closed"
	default:
		return strings.TrimSpace(state)
	}
}

func NormalizeMergeRequestNotes(
	repo platform.RepoRef,
	mrNumber int,
	parentURL string,
	notes []*gitlab.Note,
) []platform.MergeRequestEvent {
	events := make([]platform.MergeRequestEvent, 0, len(notes))
	for _, note := range notes {
		if note == nil {
			continue
		}
		if note.System {
			event, ok := normalizeMergeRequestSystemNote(repo, mrNumber, note)
			if ok {
				events = append(events, event)
			}
			continue
		}
		if note.Position != nil {
			continue
		}
		events = append(events, platform.MergeRequestEvent{
			Repo:               repo,
			PlatformID:         note.ID,
			PlatformExternalID: strconv.FormatInt(note.ID, 10),
			MergeRequestNumber: mrNumber,
			EventType:          "issue_comment",
			Author:             note.Author.Username,
			Body:               note.Body,
			CreatedAt:          timeValue(note.CreatedAt),
			DirectURL:          noteDirectURL(parentURL, note.ID),
			DedupeKey:          noteDedupeKey(repo, "mr", mrNumber, "note", strconv.FormatInt(note.ID, 10)),
		})
	}
	return events
}

func NormalizeMergeRequestDiscussions(
	repo platform.RepoRef,
	mrNumber int,
	parentURL string,
	discussions []*gitlab.Discussion,
) []platform.MergeRequestEvent {
	var events []platform.MergeRequestEvent
	for _, discussion := range discussions {
		if discussion == nil {
			continue
		}
		for _, note := range discussion.Notes {
			if note == nil {
				continue
			}
			if note.System {
				event, ok := normalizeMergeRequestSystemNote(repo, mrNumber, note)
				if ok {
					events = append(events, event)
				}
				continue
			}
			if note.Position != nil {
				continue
			}
			events = append(events, platform.MergeRequestEvent{
				Repo:               repo,
				PlatformID:         note.ID,
				PlatformExternalID: strconv.FormatInt(note.ID, 10),
				MergeRequestNumber: mrNumber,
				EventType:          "issue_comment",
				Author:             noteAuthorUsername(note),
				Body:               note.Body,
				CreatedAt:          timeValue(note.CreatedAt),
				DirectURL:          noteDirectURL(parentURL, note.ID),
				DedupeKey:          noteDedupeKey(repo, "mr", mrNumber, "note", strconv.FormatInt(note.ID, 10)),
				ThreadID:           discussion.ID,
				PositionJSON:       serializeNotePosition(note.Position),
				Resolvable:         note.Resolvable,
				Resolved:           note.Resolved,
			})
		}
	}
	return events
}

func noteAuthorUsername(note *gitlab.Note) string {
	if note == nil || note.Author.Username == "" {
		return ""
	}
	return note.Author.Username
}

func serializeNotePosition(pos *gitlab.NotePosition) string {
	if pos == nil {
		return ""
	}
	data, err := json.Marshal(pos)
	if err != nil {
		return ""
	}
	return string(data)
}

func NormalizeIssueNotes(
	repo platform.RepoRef,
	issueNumber int,
	parentURL string,
	notes []*gitlab.Note,
) []platform.IssueEvent {
	events := make([]platform.IssueEvent, 0, len(notes))
	for _, note := range notes {
		if note == nil {
			continue
		}
		if note.System {
			event, ok := normalizeIssueSystemNote(repo, issueNumber, note)
			if ok {
				events = append(events, event)
			}
			continue
		}
		events = append(events, platform.IssueEvent{
			Repo:               repo,
			PlatformID:         note.ID,
			PlatformExternalID: strconv.FormatInt(note.ID, 10),
			IssueNumber:        issueNumber,
			EventType:          "issue_comment",
			Author:             note.Author.Username,
			Body:               note.Body,
			CreatedAt:          timeValue(note.CreatedAt),
			DirectURL:          noteDirectURL(parentURL, note.ID),
			DedupeKey:          noteDedupeKey(repo, "issue", issueNumber, "note", strconv.FormatInt(note.ID, 10)),
		})
	}
	return events
}

func NormalizeIssueDiscussions(
	repo platform.RepoRef,
	issueNumber int,
	parentURL string,
	discussions []*gitlab.Discussion,
) []platform.IssueEvent {
	return normalizeIssueDiscussions(
		repo, issueNumber, parentURL, discussions, true,
	)
}

type issueCrossReferenceMetadata struct {
	SourceType        string `json:"source_type"`
	SourceOwner       string `json:"source_owner"`
	SourceRepo        string `json:"source_repo"`
	SourceNumber      int    `json:"source_number"`
	SourceTitle       string `json:"source_title"`
	SourceURL         string `json:"source_url"`
	IsCrossRepository bool   `json:"is_cross_repository"`
}

func NormalizeIssueRelatedMergeRequests(
	repo platform.RepoRef,
	issueNumber int,
	mergeRequests []*gitlab.BasicMergeRequest,
) []platform.IssueEvent {
	events := make([]platform.IssueEvent, 0, len(mergeRequests))
	for _, mr := range mergeRequests {
		if mr == nil || mr.IID <= 0 || strings.TrimSpace(mr.WebURL) == "" {
			continue
		}
		sourcePath, ok := gitLabRelatedMergeRequestPath(repo.Host, mr)
		if !ok {
			continue
		}
		owner := path.Dir(sourcePath)
		name := path.Base(sourcePath)
		metadata, _ := json.Marshal(issueCrossReferenceMetadata{
			SourceType:        "PullRequest",
			SourceOwner:       owner,
			SourceRepo:        name,
			SourceNumber:      int(mr.IID),
			SourceTitle:       mr.Title,
			SourceURL:         mr.WebURL,
			IsCrossRepository: sourcePath != repo.RepoPath,
		})
		observedAt := timeValue(mr.UpdatedAt)
		if observedAt.IsZero() {
			observedAt = timeValue(mr.CreatedAt)
		}
		externalID := strconv.FormatInt(mr.ID, 10)
		events = append(events, platform.IssueEvent{
			Repo:               repo,
			PlatformID:         mr.ID,
			PlatformExternalID: externalID,
			IssueNumber:        issueNumber,
			EventType:          "cross_referenced",
			Summary:            fmt.Sprintf("Referenced from %s#%d", sourcePath, mr.IID),
			MetadataJSON:       string(metadata),
			CreatedAt:          observedAt,
			DirectURL:          mr.WebURL,
			DedupeKey: noteDedupeKey(
				repo, "issue", issueNumber, "cross_reference",
				fmt.Sprintf("%s!%d", sourcePath, mr.IID),
			),
		})
	}
	return events
}

func gitLabRelatedMergeRequestPath(host string, mr *gitlab.BasicMergeRequest) (string, bool) {
	parsed, err := url.Parse(mr.WebURL)
	if err != nil || !strings.EqualFold(parsed.Host, host) {
		return "", false
	}
	if mr.References != nil {
		if full, _, ok := strings.Cut(strings.TrimSpace(mr.References.Full), "!"); ok {
			if normalized, err := normalizeSafeProjectPath(full); err == nil {
				return normalized, true
			}
		}
	}
	projectPath, _, ok := strings.Cut(strings.Trim(parsed.Path, "/"), "/-/merge_requests/")
	if !ok {
		return "", false
	}
	normalized, err := normalizeSafeProjectPath(projectPath)
	return normalized, err == nil
}

func normalizeIssueDiscussionComments(
	repo platform.RepoRef,
	issueNumber int,
	parentURL string,
	discussions []*gitlab.Discussion,
) []platform.IssueEvent {
	return normalizeIssueDiscussions(
		repo, issueNumber, parentURL, discussions, false,
	)
}

func normalizeIssueDiscussions(
	repo platform.RepoRef,
	issueNumber int,
	parentURL string,
	discussions []*gitlab.Discussion,
	includeSystem bool,
) []platform.IssueEvent {
	var events []platform.IssueEvent
	for _, discussion := range discussions {
		if discussion == nil {
			continue
		}
		for _, note := range discussion.Notes {
			if note == nil {
				continue
			}
			if note.System {
				if includeSystem {
					event, ok := normalizeIssueSystemNote(repo, issueNumber, note)
					if ok {
						events = append(events, event)
					}
				}
				continue
			}
			events = append(events, platform.IssueEvent{
				Repo:               repo,
				PlatformID:         note.ID,
				PlatformExternalID: strconv.FormatInt(note.ID, 10),
				IssueNumber:        issueNumber,
				EventType:          "issue_comment",
				Author:             noteAuthorUsername(note),
				Body:               note.Body,
				CreatedAt:          timeValue(note.CreatedAt),
				DirectURL:          noteDirectURL(parentURL, note.ID),
				DedupeKey:          noteDedupeKey(repo, "issue", issueNumber, "note", strconv.FormatInt(note.ID, 10)),
				ThreadID:           discussion.ID,
			})
		}
	}
	return events
}

func normalizeMergeRequestSystemNote(
	repo platform.RepoRef,
	mrNumber int,
	note *gitlab.Note,
) (platform.MergeRequestEvent, bool) {
	eventType, ok := gitLabAssignmentEventType(note.Body)
	if !ok {
		eventType, ok = gitLabLifecycleEventType(note.Body)
		if !ok {
			return platform.MergeRequestEvent{}, false
		}
	}
	externalID := strconv.FormatInt(note.ID, 10)
	return platform.MergeRequestEvent{
		Repo:               repo,
		PlatformID:         note.ID,
		PlatformExternalID: externalID,
		MergeRequestNumber: mrNumber,
		EventType:          eventType,
		Author:             note.Author.Username,
		Summary:            strings.TrimSpace(note.Body),
		CreatedAt:          timeValue(note.CreatedAt),
		DedupeKey:          noteDedupeKey(repo, "mr", mrNumber, "system_note", externalID),
	}, true
}

func noteDirectURL(parentURL string, noteID int64) string {
	if parentURL == "" || noteID == 0 {
		return ""
	}
	return parentURL + "#note_" + strconv.FormatInt(noteID, 10)
}

func normalizeIssueSystemNote(
	repo platform.RepoRef,
	issueNumber int, note *gitlab.Note,
) (platform.IssueEvent, bool) {
	eventType, ok := gitLabAssignmentEventType(note.Body)
	if !ok {
		eventType, ok = gitLabLifecycleEventType(note.Body)
		if !ok || eventType == "merged" {
			return platform.IssueEvent{}, false
		}
	}
	externalID := strconv.FormatInt(note.ID, 10)
	return platform.IssueEvent{
		Repo:               repo,
		PlatformID:         note.ID,
		PlatformExternalID: externalID,
		IssueNumber:        issueNumber,
		EventType:          eventType,
		Author:             note.Author.Username,
		Summary:            strings.TrimSpace(note.Body),
		CreatedAt:          timeValue(note.CreatedAt),
		DedupeKey:          noteDedupeKey(repo, "issue", issueNumber, "system_note", externalID),
	}, true
}

func gitLabAssignmentEventType(body string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(body))
	switch {
	case strings.HasPrefix(normalized, "assigned"):
		return "assigned", true
	case strings.HasPrefix(normalized, "unassigned"):
		return "unassigned", true
	default:
		return "", false
	}
}

func gitLabLifecycleEventType(body string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(body))
	switch {
	case strings.HasPrefix(normalized, "merged"):
		return "merged", true
	case strings.HasPrefix(normalized, "closed"):
		return "closed", true
	case strings.HasPrefix(normalized, "reopened"):
		return "reopened", true
	default:
		return "", false
	}
}

func noteDedupeKey(
	repo platform.RepoRef,
	parentKind string,
	parentNumber int,
	eventKind string,
	externalID string,
) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s:%d:%s:%s",
		platform.KindGitLab,
		repo.Host,
		repo.DisplayName(),
		parentKind,
		parentNumber,
		eventKind,
		externalID,
	)
}

func NormalizeCommitEvent(
	repo platform.RepoRef,
	mrNumber int,
	commit *gitlab.Commit,
) platform.MergeRequestEvent {
	if commit == nil {
		return platform.MergeRequestEvent{}
	}
	return platform.MergeRequestEvent{
		Repo:               repo,
		MergeRequestNumber: mrNumber,
		EventType:          "commit",
		Author:             commit.AuthorName,
		Summary:            commit.ID,
		Body:               commit.Message,
		CreatedAt:          commitTime(commit),
		DedupeKey:          "gitlab-commit-" + commit.ID,
	}
}

func NormalizeRelease(repo platform.RepoRef, release *gitlab.Release) platform.Release {
	if release == nil {
		return platform.Release{}
	}
	out := platform.Release{
		Repo:               repo,
		PlatformExternalID: release.TagName,
		TagName:            release.TagName,
		Name:               release.Name,
		TargetCommitish:    release.Commit.ID,
		CreatedAt:          timeValue(release.CreatedAt),
	}
	if release.ReleasedAt != nil {
		t := release.ReleasedAt.UTC()
		out.PublishedAt = &t
	}
	return out
}

func NormalizeTag(repo platform.RepoRef, tag *gitlab.Tag) platform.Tag {
	if tag == nil {
		return platform.Tag{}
	}
	out := platform.Tag{
		Repo:               repo,
		PlatformExternalID: tag.Name,
		Name:               tag.Name,
		SHA:                tag.Target,
	}
	if tag.Commit != nil {
		if out.SHA == "" {
			out.SHA = tag.Commit.ID
		}
		out.URL = tag.Commit.WebURL
	}
	return out
}

func NormalizePipeline(repo platform.RepoRef, pipeline *gitlab.PipelineInfo) platform.CICheck {
	if pipeline == nil {
		return platform.CICheck{}
	}
	status, conclusion := NormalizePipelineCheckState(pipeline.Status)
	return platform.CICheck{
		Repo:               repo,
		PlatformID:         pipeline.ID,
		PlatformExternalID: strconv.FormatInt(pipeline.ID, 10),
		Name:               "GitLab Pipeline",
		Status:             status,
		Conclusion:         conclusion,
		URL:                pipeline.WebURL,
		App:                "gitlab",
		StartedAt:          timePtr(timeValue(pipeline.CreatedAt)),
		CompletedAt:        completedAtForPipeline(pipeline),
	}
}

func NormalizePipelineStatus(status string) string {
	switch normalizePipelineStatus(status) {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "manual", "scheduled":
		return "pending"
	case "success", "skipped":
		return "success"
	case "failed", "canceled", "cancelled":
		return "failure"
	case "":
		return ""
	default:
		return "neutral"
	}
}

func NormalizePipelineCheckState(status string) (string, string) {
	switch normalizePipelineStatus(status) {
	case "created", "waiting_for_resource", "preparing", "pending", "running":
		return "in_progress", ""
	case "manual", "scheduled":
		return "queued", ""
	case "success":
		return "completed", "success"
	case "failed":
		return "completed", "failure"
	case "canceled", "cancelled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	case "":
		return "", ""
	default:
		return "completed", "neutral"
	}
}

func normalizePipelineStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

func pipelineStatusFromInfo(pipeline *gitlab.PipelineInfo) string {
	if pipeline == nil {
		return ""
	}
	return NormalizePipelineStatus(pipeline.Status)
}

// normalizeLabelNames converts the bare label names GitLab embeds in
// MR/issue payloads. PlatformExternalID stays empty: the label catalog
// stores decimal label IDs in that field, so claiming the name here
// would let a label named like another label's ID match two different
// rows during persistence and fail the save.
func normalizeLabelNames(repo platform.RepoRef, labels gitlab.Labels) []platform.Label {
	out := make([]platform.Label, 0, len(labels))
	for _, name := range labels {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, platform.Label{
			Repo: repo,
			Name: name,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func timeValue(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func completedAtForPipeline(pipeline *gitlab.PipelineInfo) *time.Time {
	switch NormalizePipelineStatus(pipeline.Status) {
	case "success", "failure", "neutral":
		return timePtr(timeValue(pipeline.UpdatedAt))
	default:
		return nil
	}
}

func commitTime(commit *gitlab.Commit) time.Time {
	if t := timeValue(commit.CreatedAt); !t.IsZero() {
		return t
	}
	if t := timeValue(commit.CommittedDate); !t.IsZero() {
		return t
	}
	return timeValue(commit.AuthoredDate)
}

// basicUsernames always returns a non-nil slice: GitLab includes the
// assignees and reviewers fields on every merge request payload, so an
// empty result is a provider-confirmed empty set rather than unknown.
func basicUsernames(users []*gitlab.BasicUser) []string {
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user != nil && user.Username != "" {
			out = append(out, user.Username)
		}
	}
	return out
}

func basicUsername(user *gitlab.BasicUser) string {
	if user == nil {
		return ""
	}
	return user.Username
}

func basicName(user *gitlab.BasicUser) string {
	if user == nil {
		return ""
	}
	return user.Name
}

func issueUsername(user *gitlab.IssueAuthor) string {
	if user == nil {
		return ""
	}
	return user.Username
}

func sanitizeDisplayName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\n', '\r', '<', '>':
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
