package gitlab

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func TestNormalizeProjectPreservesGitLabIdentity(t *testing.T) {
	assert := assert.New(t)
	createdAt := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)

	repo, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID:                42,
		Description:       "project description",
		Path:              "project",
		PathWithNamespace: "Group/SubGroup/Project",
		DefaultBranch:     "main",
		WebURL:            "https://gitlab.example.com/Group/SubGroup/Project",
		HTTPURLToRepo:     "https://gitlab.example.com/Group/SubGroup/Project.git",
		Visibility:        gitlab.PrivateVisibility,
		Archived:          true,
		Permissions: &gitlab.Permissions{
			ProjectAccess: &gitlab.ProjectAccess{
				AccessLevel: gitlab.ReporterPermissions,
			},
		},
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	})
	require.NoError(t, err)

	assert.Equal(platform.KindGitLab, repo.Ref.Platform)
	assert.Equal("gitlab.example.com", repo.Ref.Host)
	assert.Equal("Group/SubGroup/Project", repo.Ref.RepoPath)
	assert.Equal("Group/SubGroup", repo.Ref.Owner)
	assert.Equal("project", repo.Ref.Name)
	assert.Equal(int64(42), repo.Ref.PlatformID)
	assert.Equal("42", repo.Ref.PlatformExternalID)
	assert.True(repo.Private)
	assert.True(repo.Archived)
	require.NotNil(t, repo.ViewerCanMerge)
	assert.False(*repo.ViewerCanMerge)
	assert.Equal(createdAt, repo.CreatedAt)
	assert.Equal(updatedAt, repo.UpdatedAt)
	require.NotNil(t, repo.MergeSettings)
	assert.True(repo.MergeSettings.AllowSquashMerge)
	assert.True(repo.MergeSettings.AllowMergeCommit)
	assert.False(repo.MergeSettings.AllowRebaseMerge, "GitLab has no per-merge rebase")
}

func TestNormalizeProjectPreservesFeatureState(t *testing.T) {
	assert := assert.New(t)
	repo, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID:                       42,
		Path:                     "project",
		PathWithNamespace:        "group/project",
		IssuesAccessLevel:        gitlab.DisabledAccessControl,
		MergeRequestsAccessLevel: gitlab.PrivateAccessControl,
	})
	require.NoError(t, err)

	issuesEnabled, known := repo.FeatureEnabled(platform.RepositoryFeatureIssues)
	assert.True(known)
	assert.False(issuesEnabled)
	mergeRequestsEnabled, known := repo.FeatureEnabled(platform.RepositoryFeatureMergeRequests)
	assert.True(known)
	assert.True(mergeRequestsEnabled)

	unknown, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID: 43, Path: "project", PathWithNamespace: "group/project",
	})
	require.NoError(t, err)
	_, known = unknown.FeatureEnabled(platform.RepositoryFeatureIssues)
	assert.False(known)
}

func TestNormalizeProjectMapsSquashNeverToSquashDisallowed(t *testing.T) {
	assert := assert.New(t)
	repo, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID:                42,
		Path:              "project",
		PathWithNamespace: "group/project",
		SquashOption:      gitlab.SquashOptionNever,
	})
	require.NoError(t, err)
	require.NotNil(t, repo.MergeSettings)
	assert.False(repo.MergeSettings.AllowSquashMerge)
	assert.True(repo.MergeSettings.AllowMergeCommit)
	assert.False(repo.MergeSettings.AllowRebaseMerge)
}

func TestNormalizeProjectMapsSquashAlwaysToMergeCommitDisallowed(t *testing.T) {
	assert := assert.New(t)
	repo, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID:                42,
		Path:              "project",
		PathWithNamespace: "group/project",
		SquashOption:      gitlab.SquashOptionAlways,
	})
	require.NoError(t, err)
	require.NotNil(t, repo.MergeSettings)
	assert.True(repo.MergeSettings.AllowSquashMerge)
	assert.False(repo.MergeSettings.AllowMergeCommit, "squash_option=always forbids non-squash accepts")
	assert.False(repo.MergeSettings.AllowRebaseMerge)
}

func TestNormalizeProjectUsesHighestGitLabAccessForMergePermission(t *testing.T) {
	repo, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
		ID:                42,
		Path:              "project",
		PathWithNamespace: "group/project",
		Permissions: &gitlab.Permissions{
			ProjectAccess: &gitlab.ProjectAccess{
				AccessLevel: gitlab.ReporterPermissions,
			},
			GroupAccess: &gitlab.GroupAccess{
				AccessLevel: gitlab.DeveloperPermissions,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.ViewerCanMerge)
	assert.True(t, *repo.ViewerCanMerge)
}

func TestNormalizeProjectRejectsUnsafePathWithNamespace(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		repoName string
	}{
		{name: "parent traversal", path: "group/../../outside/project", repoName: "project"},
		{name: "dot segment", path: "group/./project", repoName: "project"},
		{name: "empty segment", path: "group//project", repoName: "project"},
		{name: "absolute", path: "/group/project", repoName: "project"},
		{name: "backslash", path: `group\project`, repoName: "project"},
		{name: "separator in name", path: "group/project", repoName: "nested/project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeProject("gitlab.example.com", &gitlab.Project{
				ID:                42,
				Path:              tt.repoName,
				PathWithNamespace: tt.path,
			})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsafe GitLab project path")
		})
	}
}

func TestNormalizeMergeRequestUsesIIDAndPipelineStatus(t *testing.T) {
	assert := assert.New(t)
	createdAt := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	mergedAt := updatedAt.Add(time.Hour)

	mr := NormalizeMergeRequest(testGitLabRepoRef(), &gitlab.BasicMergeRequest{
		ID:                  1001,
		IID:                 7,
		ProjectID:           42,
		Title:               "Ship GitLab",
		State:               "merged",
		Description:         "body",
		SourceBranch:        "feature",
		TargetBranch:        "main",
		SHA:                 "abc123",
		UserNotesCount:      4,
		Draft:               true,
		WebURL:              "https://gitlab.example.com/group/project/-/merge_requests/7",
		Author:              &gitlab.BasicUser{Username: "alice", Name: "Alice A."},
		MergeUser:           &gitlab.BasicUser{Username: "merge-admin"},
		CreatedAt:           &createdAt,
		UpdatedAt:           &updatedAt,
		MergedAt:            &mergedAt,
		Labels:              gitlab.Labels{"bug"},
		DetailedMergeStatus: "mergeable",
	}, &gitlab.PipelineInfo{ID: 501, Status: "scheduled"})

	assert.Equal(int64(1001), mr.PlatformID)
	assert.Equal("1001", mr.PlatformExternalID)
	assert.Equal(7, mr.Number)
	assert.Equal("alice", mr.Author)
	assert.Equal("Alice A.", mr.AuthorDisplayName)
	assert.Equal("merge-admin", mr.MergedBy)
	assert.Equal("merged", mr.State)
	assert.True(mr.IsDraft)
	assert.Equal("feature", mr.HeadBranch)
	assert.Equal("main", mr.BaseBranch)
	assert.Equal("abc123", mr.HeadSHA)
	assert.Equal(4, mr.CommentCount)
	assert.Empty(mr.ReviewDecision)
	assert.Equal("clean", mr.MergeableState)
	assert.Equal("pending", mr.CIStatus)
	assert.Equal(createdAt, mr.CreatedAt)
	assert.Equal(updatedAt, mr.LastActivityAt)
	require.NotNil(t, mr.MergedAt)
	assert.Equal(mergedAt, *mr.MergedAt)
	require.Len(t, mr.Labels, 1)
	assert.Equal("bug", mr.Labels[0].Name)
}

func TestNormalizeDetailedMergeRequestPreservesResultCommitSHA(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mergeSHA  string
		squashSHA string
		want      string
	}{
		{
			name:     "merge commit takes precedence",
			mergeSHA: "merge-sha", squashSHA: "squash-sha", want: "merge-sha",
		},
		{
			name:      "squash commit is the fallback",
			squashSHA: "squash-sha", want: "squash-sha",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mr := NormalizeDetailedMergeRequest(testGitLabRepoRef(), &gitlab.MergeRequest{
				ID: 1001, IID: 7, State: "merged",
				MergeCommitSHA: tt.mergeSHA, SquashCommitSHA: tt.squashSHA,
			})

			assert.Equal(t, tt.want, mr.MergeCommitSHA)
		})
	}
}

func TestNormalizeMergeRequestLeavesMergedByEmptyWhenGitLabOmitsMergeUser(t *testing.T) {
	mr := NormalizeMergeRequest(testGitLabRepoRef(), &gitlab.BasicMergeRequest{
		ID:       1001,
		IID:      7,
		State:    "merged",
		MergedAt: &time.Time{},
	}, nil)

	assert.Empty(t, mr.MergedBy)
}

func TestNormalizeMergeRequestUsesMergedByWhenGitLabOmitsMergeUser(t *testing.T) {
	mr := NormalizeMergeRequest(testGitLabRepoRef(), &gitlab.BasicMergeRequest{
		ID:    1001,
		IID:   7,
		State: "merged",
		//nolint:staticcheck // Exercise GitLab < 14.7 fallback when merge_user is absent.
		MergedBy: &gitlab.BasicUser{Username: "legacy-admin"},
	}, nil)

	assert.Equal(t, "legacy-admin", mr.MergedBy)
}

func TestNormalizeMergeRequestMapsGitLabMergeability(t *testing.T) {
	tests := []struct {
		name           string
		detailedStatus string
		hasConflicts   bool
		want           string
	}{
		{name: "mergeable", detailedStatus: "mergeable", want: "clean"},
		{name: "conflict", detailedStatus: "conflict", want: "dirty"},
		{name: "has conflicts", detailedStatus: "checking", hasConflicts: true, want: "dirty"},
		{name: "checking", detailedStatus: "checking", want: "unknown"},
		{name: "unchecked", detailedStatus: "unchecked", want: "unknown"},
		{name: "needs rebase", detailedStatus: "need_rebase", want: "behind"},
		{name: "ci must pass", detailedStatus: "ci_must_pass", want: "unstable"},
		{name: "status checks", detailedStatus: "status_checks_must_pass", want: "unstable"},
		{name: "not approved", detailedStatus: "not_approved", want: "blocked"},
		{name: "discussions", detailedStatus: "discussions_not_resolved", want: "blocked"},
		{name: "draft", detailedStatus: "draft_status", want: "draft"},
		{name: "empty", detailedStatus: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := NormalizeMergeRequest(testGitLabRepoRef(), &gitlab.BasicMergeRequest{
				ID:                  1001,
				IID:                 7,
				State:               "opened",
				DetailedMergeStatus: tt.detailedStatus,
				HasConflicts:        tt.hasConflicts,
			}, nil)

			assert.Equal(t, tt.want, mr.MergeableState)
			assert.Empty(t, mr.ReviewDecision)
		})
	}
}

func TestNormalizeMergeRequestMapsGitLabStates(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "opened", input: "opened", want: "open"},
		{name: "closed", input: "closed", want: "closed"},
		{name: "merged", input: "merged", want: "merged"},
		{name: "unknown", input: "locked", want: "locked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := NormalizeMergeRequest(testGitLabRepoRef(), &gitlab.BasicMergeRequest{
				ID:    1001,
				IID:   7,
				State: tt.input,
			}, nil)

			assert.Equal(t, tt.want, mr.State)
		})
	}
}

func TestNormalizeIssueUsesIIDAndLabels(t *testing.T) {
	assert := assert.New(t)
	closedAt := time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC)

	issue := NormalizeIssue(testGitLabRepoRef(), &gitlab.Issue{
		ID:             2001,
		IID:            5,
		Title:          "Fix issue",
		State:          "closed",
		Description:    "details",
		WebURL:         "https://gitlab.example.com/group/project/-/issues/5",
		Author:         &gitlab.IssueAuthor{Username: "bob"},
		UserNotesCount: 2,
		ClosedAt:       &closedAt,
		Labels:         gitlab.Labels{"bug", "p1"},
	})

	assert.Equal(int64(2001), issue.PlatformID)
	assert.Equal("2001", issue.PlatformExternalID)
	assert.Equal(5, issue.Number)
	assert.Equal("bob", issue.Author)
	assert.Equal("closed", issue.State)
	assert.Equal(2, issue.CommentCount)
	require.NotNil(t, issue.ClosedAt)
	assert.Equal(closedAt, *issue.ClosedAt)
	assert.Equal([]string{"bug", "p1"}, []string{issue.Labels[0].Name, issue.Labels[1].Name})
}

func TestNormalizeIssueMapsGitLabStates(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "opened", input: "opened", want: "open"},
		{name: "closed", input: "closed", want: "closed"},
		{name: "unknown", input: "reopened", want: "reopened"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := NormalizeIssue(testGitLabRepoRef(), &gitlab.Issue{
				ID:    2001,
				IID:   5,
				State: tt.input,
			})

			assert.Equal(t, tt.want, issue.State)
		})
	}
}

func TestNormalizeIssueExtractsAssignees(t *testing.T) {
	assert := assert.New(t)

	issue := NormalizeIssue(testGitLabRepoRef(), &gitlab.Issue{
		ID:    2001,
		IID:   5,
		Title: "Test issue",
		State: "opened",
		Assignees: []*gitlab.IssueAssignee{
			{Username: "alice"},
			{Username: "bob"},
		},
	})

	assert.Equal([]string{"alice", "bob"}, issue.Assignees)
}

func TestNormalizeIssueHandlesNilAndEmptyAssignees(t *testing.T) {
	assert := assert.New(t)

	// Test nil assignee in list
	issue := NormalizeIssue(testGitLabRepoRef(), &gitlab.Issue{
		ID:    2001,
		IID:   5,
		State: "opened",
		Assignees: []*gitlab.IssueAssignee{
			nil,
			{Username: "alice"},
			{Username: ""},
		},
	})
	assert.Equal([]string{"alice"}, issue.Assignees)

	// Test empty assignees list
	issue2 := NormalizeIssue(testGitLabRepoRef(), &gitlab.Issue{
		ID:    2002,
		IID:   6,
		State: "opened",
	})
	assert.Empty(issue2.Assignees)
}

func TestNormalizeNotesKeepsAssignmentSystemNotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC)
	notes := []*gitlab.Note{
		{ID: 1, Body: "visible", System: false, Author: gitlab.NoteAuthor{Username: "alice"}, CreatedAt: &createdAt},
		{ID: 2, Body: "assigned to @bob", System: true, Author: gitlab.NoteAuthor{Username: "root"}, CreatedAt: &createdAt},
		{ID: 3, Body: "changed milestone", System: true, Author: gitlab.NoteAuthor{Username: "root"}, CreatedAt: &createdAt},
		{ID: 4, Body: "merged", System: true, Author: gitlab.NoteAuthor{Username: "maintainer"}, CreatedAt: &createdAt},
		{ID: 5, Body: "closed", System: true, Author: gitlab.NoteAuthor{Username: "closer"}, CreatedAt: &createdAt},
		{ID: 6, Body: "reopened", System: true, Author: gitlab.NoteAuthor{Username: "opener"}, CreatedAt: &createdAt},
	}

	mrEvents := NormalizeMergeRequestNotes(testGitLabRepoRef(), 7, "", notes)
	require.Len(mrEvents, 5)
	assert.Equal("issue_comment", mrEvents[0].EventType)
	assert.Equal("visible", mrEvents[0].Body)
	assert.Equal("gitlab:gitlab.example.com:group/project:mr:7:note:1", mrEvents[0].DedupeKey)
	assert.Equal("assigned", mrEvents[1].EventType)
	assert.Equal("assigned to @bob", mrEvents[1].Summary)
	assert.Equal("gitlab:gitlab.example.com:group/project:mr:7:system_note:2", mrEvents[1].DedupeKey)
	assert.Equal("merged", mrEvents[2].EventType)
	assert.Equal("merged", mrEvents[2].Summary)
	assert.Equal("maintainer", mrEvents[2].Author)
	assert.Equal("gitlab:gitlab.example.com:group/project:mr:7:system_note:4", mrEvents[2].DedupeKey)
	assert.Equal("closed", mrEvents[3].EventType)
	assert.Equal("closed", mrEvents[3].Summary)
	assert.Equal("closer", mrEvents[3].Author)
	assert.Equal("reopened", mrEvents[4].EventType)
	assert.Equal("reopened", mrEvents[4].Summary)
	assert.Equal("opener", mrEvents[4].Author)

	issueEvents := NormalizeIssueNotes(testGitLabRepoRef(), 5, "", notes)
	require.Len(issueEvents, 4)
	assert.Equal("issue_comment", issueEvents[0].EventType)
	assert.Equal("visible", issueEvents[0].Body)
	assert.Equal("gitlab:gitlab.example.com:group/project:issue:5:note:1", issueEvents[0].DedupeKey)
	assert.Equal("assigned", issueEvents[1].EventType)
	assert.Equal("assigned to @bob", issueEvents[1].Summary)
	assert.Equal("gitlab:gitlab.example.com:group/project:issue:5:system_note:2", issueEvents[1].DedupeKey)
	assert.Equal("closed", issueEvents[2].EventType)
	assert.Equal("closer", issueEvents[2].Author)
	assert.Equal("reopened", issueEvents[3].EventType)
	assert.Equal("opener", issueEvents[3].Author)
}

func TestNormalizeNotesBuildsDirectNoteURLFromParentURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC)
	notes := []*gitlab.Note{
		{ID: 101, Body: "visible", System: false, Author: gitlab.NoteAuthor{Username: "alice"}, CreatedAt: &createdAt},
	}

	mrEvents := NormalizeMergeRequestNotes(
		testGitLabRepoRef(),
		7,
		"https://gitlab.example.com/group/project/-/merge_requests/7",
		notes,
	)
	require.Len(mrEvents, 1)
	assert.Equal("https://gitlab.example.com/group/project/-/merge_requests/7#note_101", mrEvents[0].DirectURL)

	issueEvents := NormalizeIssueNotes(
		testGitLabRepoRef(),
		5,
		"https://gitlab.example.com/group/project/-/issues/5",
		notes,
	)
	require.Len(issueEvents, 1)
	assert.Equal("https://gitlab.example.com/group/project/-/issues/5#note_101", issueEvents[0].DirectURL)
}

func TestNormalizeMergeRequestNotesDropsPositionedDiffNotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC)
	notes := []*gitlab.Note{
		{
			ID:        1,
			Body:      "top-level note",
			Author:    gitlab.NoteAuthor{Username: "alice"},
			CreatedAt: &createdAt,
		},
		{
			ID:        2,
			Body:      "inline diff note",
			Author:    gitlab.NoteAuthor{Username: "reviewer"},
			CreatedAt: &createdAt,
			Position: &gitlab.NotePosition{
				NewPath: "src/main.go",
				NewLine: 9,
			},
		},
	}

	events := NormalizeMergeRequestNotes(testGitLabRepoRef(), 7, "", notes)

	require.Len(events, 1)
	assert.Equal("issue_comment", events[0].EventType)
	assert.Equal("top-level note", events[0].Body)
	assert.Equal("gitlab:gitlab.example.com:group/project:mr:7:note:1", events[0].DedupeKey)
}

func TestNormalizeNotesDedupeKeyIncludesRepositoryAndParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	notes := []*gitlab.Note{
		{ID: 1, Body: "same note id", Author: gitlab.NoteAuthor{Username: "alice"}},
	}
	otherRepo := testGitLabRepoRef()
	otherRepo.PlatformID = 43
	otherRepo.RepoPath = "other/project"

	firstMR := NormalizeMergeRequestNotes(testGitLabRepoRef(), 7, "", notes)
	secondMR := NormalizeMergeRequestNotes(otherRepo, 7, "", notes)
	require.Len(firstMR, 1)
	require.Len(secondMR, 1)
	assert.NotEqual(firstMR[0].DedupeKey, secondMR[0].DedupeKey)

	firstIssue := NormalizeIssueNotes(testGitLabRepoRef(), 5, "", notes)
	secondIssue := NormalizeIssueNotes(testGitLabRepoRef(), 6, "", notes)
	require.Len(firstIssue, 1)
	require.Len(secondIssue, 1)
	assert.NotEqual(firstIssue[0].DedupeKey, secondIssue[0].DedupeKey)
}

func TestNormalizeCommitReleaseTagAndPipeline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	ref := testGitLabRepoRef()

	commitEvent := NormalizeCommitEvent(ref, 7, &gitlab.Commit{
		ID:         "abcdef123456",
		Title:      "first line",
		Message:    "first line\n\nbody",
		AuthorName: "Alice",
		CreatedAt:  &createdAt,
	})
	assert.Equal("commit", commitEvent.EventType)
	assert.Equal("abcdef123456", commitEvent.Summary)
	assert.Equal("first line\n\nbody", commitEvent.Body)
	assert.Equal("gitlab-commit-abcdef123456", commitEvent.DedupeKey)

	publishedAt := createdAt.Add(time.Hour)
	release := NormalizeRelease(ref, &gitlab.Release{
		TagName:    "v1.0.0",
		Name:       "One",
		CreatedAt:  &createdAt,
		ReleasedAt: &publishedAt,
		Commit:     gitlab.Commit{ID: "abcdef"},
	})
	assert.Equal("v1.0.0", release.TagName)
	assert.Equal("abcdef", release.TargetCommitish)
	require.NotNil(release.PublishedAt)
	assert.Equal(publishedAt, *release.PublishedAt)

	tag := NormalizeTag(ref, &gitlab.Tag{
		Name:   "v1.0.0",
		Target: "abcdef",
		Commit: &gitlab.Commit{WebURL: "https://gitlab.example.com/group/project/-/commit/abcdef"},
	})
	assert.Equal("v1.0.0", tag.Name)
	assert.Equal("abcdef", tag.SHA)
	assert.Equal("https://gitlab.example.com/group/project/-/commit/abcdef", tag.URL)

	check := NormalizePipeline(ref, &gitlab.PipelineInfo{
		ID:     501,
		Status: "failed",
		WebURL: "https://gitlab.example.com/group/project/-/pipelines/501",
	})
	assert.Equal("GitLab Pipeline", check.Name)
	assert.Equal("completed", check.Status)
	assert.Equal("failure", check.Conclusion)
	assert.Equal("gitlab", check.App)
}

func TestGitLabCIStatusMappingTable(t *testing.T) {
	tests := []struct {
		name       string
		gitlab     string
		status     string
		conclusion string
		overall    string
	}{
		{name: "created", gitlab: "created", status: "in_progress", overall: "pending"},
		{name: "waiting for resource", gitlab: "waiting_for_resource", status: "in_progress", overall: "pending"},
		{name: "preparing", gitlab: "preparing", status: "in_progress", overall: "pending"},
		{name: "pending", gitlab: "pending", status: "in_progress", overall: "pending"},
		{name: "running", gitlab: "running", status: "in_progress", overall: "pending"},
		{name: "success", gitlab: "success", status: "completed", conclusion: "success", overall: "success"},
		{name: "failed", gitlab: "failed", status: "completed", conclusion: "failure", overall: "failure"},
		{name: "canceled", gitlab: "canceled", status: "completed", conclusion: "cancelled", overall: "failure"},
		{name: "cancelled", gitlab: "cancelled", status: "completed", conclusion: "cancelled", overall: "failure"},
		{name: "skipped", gitlab: "skipped", status: "completed", conclusion: "skipped", overall: "success"},
		{name: "manual", gitlab: "manual", status: "queued", overall: "pending"},
		{name: "scheduled", gitlab: "scheduled", status: "queued", overall: "pending"},
		{name: "unknown", gitlab: "archived", status: "completed", conclusion: "neutral", overall: "neutral"},
		{name: "empty", gitlab: "", status: "", conclusion: "", overall: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := NormalizePipeline(testGitLabRepoRef(), &gitlab.PipelineInfo{ID: 1, Status: tt.gitlab})

			assert.Equal(t, tt.status, check.Status)
			assert.Equal(t, tt.conclusion, check.Conclusion)
			assert.Equal(t, tt.overall, NormalizePipelineStatus(tt.gitlab))
		})
	}
}

func TestNormalizeMergeRequestDiscussions(t *testing.T) {
	assert := assert.New(t)
	createdAt1 := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	createdAt2 := time.Date(2026, 5, 22, 11, 0, 0, 0, time.UTC)
	createdAt3 := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	repo := platform.RepoRef{
		Platform: platform.KindGitLab,
		Host:     "gitlab.com",
		Owner:    "acme",
		Name:     "widget",
		RepoPath: "acme/widget",
	}

	discussions := []*gitlab.Discussion{
		{
			ID: "disc-abc123",
			Notes: []*gitlab.Note{
				{
					ID:         101,
					Body:       "needs fix on this line",
					System:     false,
					Resolvable: true,
					Resolved:   false,
					Author:     gitlab.NoteAuthor{Username: "reviewer"},
					CreatedAt:  &createdAt1,
					Position: &gitlab.NotePosition{
						NewPath: "main.go",
						NewLine: 42,
					},
				},
				{
					ID:         102,
					Body:       "fixed!",
					System:     false,
					Resolvable: true,
					Resolved:   true,
					Author:     gitlab.NoteAuthor{Username: "author"},
					CreatedAt:  &createdAt2,
				},
			},
		},
		{
			ID: "disc-def456",
			Notes: []*gitlab.Note{
				{
					ID:     201,
					Body:   "merged",
					System: true,
					Author: gitlab.NoteAuthor{Username: "maintainer"},
				},
				{
					ID:     203,
					Body:   "changed milestone",
					System: true,
					Author: gitlab.NoteAuthor{Username: "gitlab"},
				},
				{
					ID:         202,
					Body:       "general comment",
					System:     false,
					Resolvable: false,
					Author:     gitlab.NoteAuthor{Username: "commenter"},
					CreatedAt:  &createdAt3,
				},
			},
		},
	}

	events := NormalizeMergeRequestDiscussions(repo, 7, "", discussions)

	// Positioned diff notes are synced through review-thread reads, not timeline comments.
	assert.Len(events, 3)

	// Reply from first discussion
	assert.Equal("disc-abc123", events[0].ThreadID)
	assert.Equal("author", events[0].Author)
	assert.True(events[0].Resolvable)
	assert.True(events[0].Resolved)
	assert.Empty(events[0].PositionJSON)

	assert.Equal("merged", events[1].EventType)
	assert.Equal("maintainer", events[1].Author)
	assert.Equal("merged", events[1].Summary)

	// User note from second discussion
	assert.Equal("disc-def456", events[2].ThreadID)
	assert.Equal("commenter", events[2].Author)
	assert.False(events[2].Resolvable)
}

func TestNormalizeDiscussionsBuildsDirectNoteURLFromParentURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC)
	discussions := []*gitlab.Discussion{
		{
			ID: "disc-abc123",
			Notes: []*gitlab.Note{
				{
					ID:        2001,
					Body:      "thread note",
					Author:    gitlab.NoteAuthor{Username: "author"},
					CreatedAt: &createdAt,
				},
			},
		},
	}

	mrEvents := NormalizeMergeRequestDiscussions(
		testGitLabRepoRef(),
		7,
		"https://gitlab.example.com/group/project/-/merge_requests/7",
		discussions,
	)
	require.Len(mrEvents, 1)
	assert.Equal("https://gitlab.example.com/group/project/-/merge_requests/7#note_2001", mrEvents[0].DirectURL)

	issueEvents := NormalizeIssueDiscussions(
		testGitLabRepoRef(),
		5,
		"https://gitlab.example.com/group/project/-/issues/5",
		discussions,
	)
	require.Len(issueEvents, 1)
	assert.Equal("https://gitlab.example.com/group/project/-/issues/5#note_2001", issueEvents[0].DirectURL)
}

func TestNormalizeIssueDiscussions(t *testing.T) {
	assert := assert.New(t)
	repo := platform.RepoRef{
		Platform: platform.KindGitLab,
		Host:     "gitlab.com",
		Owner:    "acme",
		Name:     "widget",
		RepoPath: "acme/widget",
	}

	discussions := []*gitlab.Discussion{
		{
			ID: "issue-disc-111",
			Notes: []*gitlab.Note{
				{
					ID:        301,
					Body:      "I can reproduce this",
					System:    false,
					Author:    gitlab.NoteAuthor{Username: "reporter"},
					CreatedAt: timePtr(time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)),
				},
			},
		},
	}

	events := NormalizeIssueDiscussions(repo, 10, "", discussions)

	assert.Len(events, 1)
	assert.Equal("issue-disc-111", events[0].ThreadID)
	assert.Equal("reporter", events[0].Author)
	assert.Equal(10, events[0].IssueNumber)
}

func TestNormalizeIssueRelatedMergeRequestsKeepsProviderHostBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := testGitLabRepoRef()

	events := NormalizeIssueRelatedMergeRequests(repo, 10, []*gitlab.BasicMergeRequest{
		{
			ID: 101, IID: 11, Title: "Fix the issue",
			WebURL:     "https://gitlab.example.com/acme/tools/-/merge_requests/11",
			References: &gitlab.IssueReferences{Full: "acme/tools!11"},
			UpdatedAt:  timePtr(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)),
		},
		{
			ID: 102, IID: 12, Title: "Foreign provider reference",
			WebURL:     "https://gitlab.other.example/acme/tools/-/merge_requests/12",
			References: &gitlab.IssueReferences{Full: "acme/tools!12"},
		},
	})

	require.Len(events, 1)
	assert.Equal("cross_referenced", events[0].EventType)
	assert.Contains(events[0].MetadataJSON, `"source_owner":"acme"`)
	assert.Contains(events[0].MetadataJSON, `"source_number":11`)
}

func testGitLabRepoRef() platform.RepoRef {
	return platform.RepoRef{
		Platform:   platform.KindGitLab,
		Host:       "gitlab.example.com",
		Owner:      "group",
		Name:       "project",
		RepoPath:   "group/project",
		PlatformID: 42,
	}
}
