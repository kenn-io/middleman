package workspaceapi

import (
	"context"
	"time"

	"go.kenn.io/forge/internal/db"
)

type SubjectActivity struct {
	Subject    db.WorkspaceSubjectMetadata
	Workspace  WorkspaceRef
	ActivityAt *time.Time
}

type WorkspaceSubjectSnapshot struct {
	OwnReferences map[db.WorkspaceSubjectKey]WorkspaceRef
	Subjects      map[db.WorkspaceSubjectKey]SubjectActivity
}

type workspaceSubjectCandidate struct {
	summary    db.WorkspaceSummary
	activityAt *time.Time
}

func (s *Handler) WorkspaceSubjectSnapshot(
	ctx context.Context,
) (WorkspaceSubjectSnapshot, error) {
	if s.db == nil {
		return emptyWorkspaceSubjectSnapshot(), nil
	}
	releaseReconciliation, err := s.db.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return WorkspaceSubjectSnapshot{}, err
	}
	defer releaseReconciliation()
	return s.WorkspaceSubjectSnapshotUnderRepositoryReconciliationRead(ctx)
}

// WorkspaceSubjectSnapshotUnderRepositoryReconciliationRead returns the same
// snapshot as WorkspaceSubjectSnapshot for a caller that already holds the
// repository-reconciliation read lock. Acquiring the lock again can deadlock
// behind a queued reconciliation writer.
func (s *Handler) WorkspaceSubjectSnapshotUnderRepositoryReconciliationRead(
	ctx context.Context,
) (WorkspaceSubjectSnapshot, error) {
	snapshot := emptyWorkspaceSubjectSnapshot()
	if s.db == nil {
		return snapshot, nil
	}
	s.scheduleWorkspaceTmuxPrune()
	summaries, err := s.db.ListWorkspaceSummaries(ctx)
	if err != nil {
		return WorkspaceSubjectSnapshot{}, err
	}
	if s.workspaceSubjectAfterSummariesForTest != nil {
		s.workspaceSubjectAfterSummariesForTest()
	}

	keys := make([]db.WorkspaceSubjectKey, 0, len(summaries)*2)
	resolved := make(map[db.WorkspaceSubjectKey]workspaceSubjectCandidate)
	own := make(map[db.WorkspaceSubjectKey]db.WorkspaceSummary)
	for i := range summaries {
		summary := summaries[i]
		if summary.RepoID == 0 {
			continue
		}
		ownKey, hasOwn := ownWorkspaceSubjectKey(summary)
		if hasOwn {
			keys = append(keys, ownKey)
			if current, ok := own[ownKey]; !ok || workspaceCandidatePreferred(summary, current, ownKey) {
				own[ownKey] = summary
			}
		}
		resolvedKey, ok := resolvedWorkspaceSubjectKey(summary)
		if !ok {
			continue
		}
		keys = append(keys, resolvedKey)
		entry, refreshDue := s.cachedWorkspaceEnrichment(summary.ID, workspaceEnrichmentTmux)
		if refreshDue && summary.Status == "ready" && !s.workspaceEnrichmentDisabled {
			s.scheduleWorkspaceTmuxEnrichment(summary)
		}
		var activityAt *time.Time
		if summary.Status == "ready" {
			activityAt = cachedTmuxActivityAt(entry)
		}
		candidate := workspaceSubjectCandidate{summary: summary, activityAt: activityAt}
		current, exists := resolved[resolvedKey]
		if !exists {
			resolved[resolvedKey] = candidate
			continue
		}
		if activityAt != nil && (current.activityAt == nil || activityAt.After(*current.activityAt)) {
			current.activityAt = activityAt
		}
		if workspaceCandidatePreferred(summary, current.summary, resolvedKey) {
			current.summary = summary
		}
		resolved[resolvedKey] = current
	}

	metadata, err := s.db.ListWorkspaceSubjectMetadata(ctx, keys)
	if err != nil {
		return WorkspaceSubjectSnapshot{}, err
	}
	for key, summary := range own {
		if _, ok := metadata[key]; !ok {
			continue
		}
		snapshot.OwnReferences[key] = s.workspaceReference(&summary)
	}
	for key, candidate := range resolved {
		subject, ok := metadata[key]
		if !ok {
			continue
		}
		snapshot.Subjects[key] = SubjectActivity{
			Subject:    subject,
			Workspace:  s.workspaceReference(&candidate.summary),
			ActivityAt: candidate.activityAt,
		}
	}
	return snapshot, nil
}

func emptyWorkspaceSubjectSnapshot() WorkspaceSubjectSnapshot {
	return WorkspaceSubjectSnapshot{
		OwnReferences: make(map[db.WorkspaceSubjectKey]WorkspaceRef),
		Subjects:      make(map[db.WorkspaceSubjectKey]SubjectActivity),
	}
}

func ownWorkspaceSubjectKey(summary db.WorkspaceSummary) (db.WorkspaceSubjectKey, bool) {
	if summary.ItemNumber <= 0 ||
		(summary.ItemType != db.WorkspaceItemTypePullRequest && summary.ItemType != db.WorkspaceItemTypeIssue) {
		return db.WorkspaceSubjectKey{}, false
	}
	return db.WorkspaceSubjectKey{
		RepoID: summary.RepoID, ItemType: summary.ItemType, ItemNumber: summary.ItemNumber,
	}, true
}

func resolvedWorkspaceSubjectKey(summary db.WorkspaceSummary) (db.WorkspaceSubjectKey, bool) {
	if summary.AssociatedPRVisible &&
		summary.AssociatedPRNumber != nil && *summary.AssociatedPRNumber > 0 {
		return db.WorkspaceSubjectKey{
			RepoID: summary.RepoID, ItemType: db.WorkspaceItemTypePullRequest,
			ItemNumber: *summary.AssociatedPRNumber,
		}, true
	}
	return ownWorkspaceSubjectKey(summary)
}

func cachedTmuxActivityAt(entry *workspaceEnrichmentCacheEntry) *time.Time {
	if entry == nil || !entry.hasTmux || entry.response.TmuxLastOutputAt == nil {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *entry.response.TmuxLastOutputAt)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func workspaceCandidatePreferred(
	candidate db.WorkspaceSummary,
	current db.WorkspaceSummary,
	key db.WorkspaceSubjectKey,
) bool {
	candidateDirect := candidate.ItemType == key.ItemType && candidate.ItemNumber == key.ItemNumber
	currentDirect := current.ItemType == key.ItemType && current.ItemNumber == key.ItemNumber
	if candidateDirect != currentDirect {
		return candidateDirect
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.ID > current.ID
}

// workspaceReference uses the same live-session filtering as workspace responses.
func (s *Handler) workspaceReference(summary *db.WorkspaceSummary) WorkspaceRef {
	var response workspaceResponse
	s.applyAgentActivity(&response, summary)
	return WorkspaceRef{ID: summary.ID, Status: summary.Status, AgentState: response.AgentState}
}
