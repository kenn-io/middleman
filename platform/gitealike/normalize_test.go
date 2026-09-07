package gitealike

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestNormalizeRepositoryMapsSharedDTO(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	updated := created.Add(time.Hour)
	canPush := false
	canAdmin := false

	repo, err := NormalizeRepository(platform.KindForgejo, "codeberg.org", RepositoryDTO{
		ID:            42,
		Owner:         UserDTO{UserName: "forgejo"},
		Name:          "forgejo",
		FullName:      "forgejo/forgejo",
		HTMLURL:       "https://codeberg.org/forgejo/forgejo",
		CloneURL:      "https://codeberg.org/forgejo/forgejo.git",
		DefaultBranch: "forgejo",
		Private:       true,
		Archived:      true,
		Description:   "git forge",
		AllowSquash:   true,
		AllowMerge:    true,
		AllowRebase:   true,
		CanPush:       &canPush,
		CanAdmin:      &canAdmin,
		Created:       created,
		Updated:       updated,
	})
	require.NoError(err)

	assert.Equal(platform.KindForgejo, repo.Ref.Platform)
	assert.Equal("codeberg.org", repo.Ref.Host)
	assert.Equal("forgejo", repo.Ref.Owner)
	assert.Equal("forgejo", repo.Ref.Name)
	assert.Equal("forgejo/forgejo", repo.Ref.RepoPath)
	assert.Equal(int64(42), repo.Ref.PlatformID)
	assert.Equal("42", repo.Ref.PlatformExternalID)
	assert.Equal("https://codeberg.org/forgejo/forgejo", repo.Ref.WebURL)
	assert.Equal("https://codeberg.org/forgejo/forgejo.git", repo.Ref.CloneURL)
	assert.Equal("forgejo", repo.Ref.DefaultBranch)
	assert.Equal("git forge", repo.Description)
	assert.True(repo.Private)
	assert.True(repo.Archived)
	require.NotNil(repo.MergeSettings)
	assert.True(repo.MergeSettings.AllowSquashMerge)
	assert.True(repo.MergeSettings.AllowMergeCommit)
	assert.True(repo.MergeSettings.AllowRebaseMerge)
	require.NotNil(repo.ViewerCanMerge)
	assert.False(*repo.ViewerCanMerge)
	assert.Equal(created, repo.CreatedAt)
	assert.Equal(updated, repo.UpdatedAt)
}

func TestNormalizeRepositoryPreservesFeatureState(t *testing.T) {
	issuesEnabled := false
	mergeRequestsEnabled := true
	repo, err := NormalizeRepository(platform.KindGitea, "gitea.example", RepositoryDTO{
		ID:                   1,
		Owner:                UserDTO{UserName: "owner"},
		Name:                 "repo",
		FullName:             "owner/repo",
		IssuesEnabled:        &issuesEnabled,
		MergeRequestsEnabled: &mergeRequestsEnabled,
	})
	require := Require.New(t)
	require.NoError(err)
	require.NotNil(repo.Features.IssuesEnabled)
	require.NotNil(repo.Features.MergeRequestsEnabled)
	require.False(*repo.Features.IssuesEnabled)
	require.True(*repo.Features.MergeRequestsEnabled)
}

func TestNormalizeMergeRequestIssueEventsAndArtifacts(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	closed := base.Add(2 * time.Hour)
	mergeable := true
	unmergeable := false
	repo := platform.RepoRef{
		Platform: platform.KindGitea,
		Host:     "gitea.com",
		Owner:    "gitea",
		Name:     "tea",
		RepoPath: "gitea/tea",
	}

	pr := NormalizePullRequest(repo, PullRequestDTO{
		ID:             100,
		Index:          7,
		HTMLURL:        "https://gitea.com/gitea/tea/pulls/7",
		Title:          "Add tea",
		User:           UserDTO{UserName: "alice", FullName: "Alice"},
		State:          "closed",
		Body:           "body",
		Head:           BranchDTO{Ref: "feature", SHA: "abc123", RepoCloneURL: "https://example/head.git"},
		Base:           BranchDTO{Ref: "main", SHA: "def456"},
		Labels:         []LabelDTO{{ID: 1, Name: "kind/feature", Color: "00ff00", Description: "feature", IsDefault: true}},
		Created:        base,
		Updated:        base.Add(time.Hour),
		Mergeable:      &mergeable,
		AdditionsKnown: true,
		Deletions:      5,
		DeletionsKnown: true,
		Merged:         true,
		MergedAt:       &closed,
		MergedBy:       UserDTO{UserName: "merge-admin"},
		Closed:         &closed,
	})
	assert.Equal("merged", pr.State)
	assert.Equal(7, pr.Number)
	assert.Equal("alice", pr.Author)
	assert.Equal("merge-admin", pr.MergedBy)
	assert.Equal("Alice", pr.AuthorDisplayName)
	assert.Equal("clean", pr.MergeableState)
	assert.True(pr.AdditionsKnown)
	assert.Zero(pr.Additions)
	assert.True(pr.DeletionsKnown)
	assert.Equal(5, pr.Deletions)
	assert.Equal("feature", pr.HeadBranch)
	assert.Equal("abc123", pr.HeadSHA)
	assert.False(pr.IsDraft)

	unmergedPR := NormalizePullRequest(repo, PullRequestDTO{
		Index: 8,
		State: "open",
	})
	assert.Empty(unmergedPR.MergedBy)

	draftPR := NormalizePullRequest(repo, PullRequestDTO{
		ID:       101,
		Index:    8,
		Title:    "Draft tea",
		User:     UserDTO{UserName: "alice"},
		State:    "open",
		Draft:    true,
		IsLocked: true,
		Created:  base,
		Updated:  base,
	})
	assert.True(draftPR.IsDraft)

	lockedPR := NormalizePullRequest(repo, PullRequestDTO{
		ID:       102,
		Index:    9,
		Title:    "Locked tea",
		User:     UserDTO{UserName: "alice"},
		State:    "open",
		IsLocked: true,
		Created:  base,
		Updated:  base,
	})
	assert.False(lockedPR.IsDraft)
	assert.True(lockedPR.IsLocked)

	conflictedPR := NormalizePullRequest(repo, PullRequestDTO{
		ID:        103,
		Index:     10,
		Title:     "Conflicted tea",
		User:      UserDTO{UserName: "alice"},
		State:     "open",
		Mergeable: &unmergeable,
		Created:   base,
		Updated:   base,
	})
	assert.Equal("dirty", conflictedPR.MergeableState)

	unknownMergeablePR := NormalizePullRequest(repo, PullRequestDTO{
		ID:      104,
		Index:   11,
		Title:   "Unchecked tea",
		User:    UserDTO{UserName: "alice"},
		State:   "open",
		Created: base,
		Updated: base,
	})
	assert.Empty(unknownMergeablePR.MergeableState)

	assert.Equal("main", pr.BaseBranch)
	assert.Equal("def456", pr.BaseSHA)
	assert.Equal("https://example/head.git", pr.HeadRepoCloneURL)
	assert.Equal("kind/feature", pr.Labels[0].Name)
	assert.Equal(&closed, pr.MergedAt)
	assert.Equal(&closed, pr.ClosedAt)

	issue := NormalizeIssue(repo, IssueDTO{
		ID:       200,
		Index:    9,
		HTMLURL:  "https://gitea.com/gitea/tea/issues/9",
		Title:    "Bug",
		User:     UserDTO{UserName: "bob"},
		State:    "closed",
		Body:     "issue body",
		Comments: 3,
		Labels:   []LabelDTO{{ID: 2, Name: "bug"}},
		Created:  base,
		Updated:  base.Add(time.Hour),
		Closed:   &closed,
	})
	assert.Equal("closed", issue.State)
	assert.Equal(9, issue.Number)
	assert.Equal("bob", issue.Author)
	assert.Equal(3, issue.CommentCount)
	assert.Equal(&closed, issue.ClosedAt)

	mrEvents := NormalizeMergeRequestEvents(
		platform.KindGitea,
		repo,
		7,
		[]CommentDTO{{ID: 300, User: UserDTO{UserName: "carol"}, Body: "comment", Created: base}},
		[]ReviewDTO{{ID: 301, User: UserDTO{UserName: "dave"}, State: "APPROVED", Body: "review", Submitted: base.Add(time.Minute)}},
		[]CommitDTO{{SHA: "abc123", AuthorName: "eve", Message: "commit", Created: base.Add(2 * time.Minute)}},
	)
	assert.Len(mrEvents, 3)
	assert.Equal("issue_comment", mrEvents[0].EventType)
	assert.Equal("review", mrEvents[1].EventType)
	assert.Equal("commit", mrEvents[2].EventType)
	assert.Contains(mrEvents[0].DedupeKey, "gitea/gitea.com/gitea/tea")

	issueEvents := NormalizeIssueComments(
		platform.KindGitea,
		repo,
		9,
		[]CommentDTO{{ID: 400, User: UserDTO{UserName: "frank"}, Body: "issue comment", Created: base}},
	)
	assert.Len(issueEvents, 1)
	assert.Equal("issue_comment", issueEvents[0].EventType)
	assert.Equal("frank", issueEvents[0].Author)

	mrTimelineEvents := NormalizeMergeRequestTimelineEvents(
		platform.KindGitea,
		repo,
		7,
		[]TimelineEventDTO{
			{ID: 401, User: UserDTO{UserName: "grace"}, Type: "assigned", Assignee: UserDTO{UserName: "grace"}, Created: base},
			{ID: 403, User: UserDTO{UserName: "ivy"}, Type: "change_title", PreviousTitle: "Old tea", CurrentTitle: "New tea", Created: base.Add(time.Minute)},
			{ID: 405, User: UserDTO{UserName: "mona"}, Type: "merged", Created: base.Add(2 * time.Minute)},
			{ID: 406, User: UserDTO{UserName: "nina"}, Type: "close", Created: base.Add(3 * time.Minute)},
			{ID: 407, User: UserDTO{UserName: "otto"}, Type: "reopen", Created: base.Add(4 * time.Minute)},
		},
	)
	require.Len(mrTimelineEvents, 5)
	assert.Equal("assigned", mrTimelineEvents[0].EventType)
	assert.Equal("grace", mrTimelineEvents[0].Author)
	assert.Equal("self-assigned this", mrTimelineEvents[0].Summary)
	assert.Equal("gitea/gitea.com/gitea/tea/mr/7/assigned/401", mrTimelineEvents[0].DedupeKey)
	assert.Equal("renamed_title", mrTimelineEvents[1].EventType)
	assert.Equal("ivy", mrTimelineEvents[1].Author)
	assert.Equal(`"Old tea" -> "New tea"`, mrTimelineEvents[1].Summary)
	assert.JSONEq(`{"previous_title":"Old tea","current_title":"New tea"}`, mrTimelineEvents[1].MetadataJSON)
	assert.Equal("gitea/gitea.com/gitea/tea/mr/7/renamed_title/403", mrTimelineEvents[1].DedupeKey)
	assert.Equal("merged", mrTimelineEvents[2].EventType)
	assert.Equal("mona", mrTimelineEvents[2].Author)
	assert.Equal("merged this", mrTimelineEvents[2].Summary)
	assert.Equal("gitea/gitea.com/gitea/tea/mr/7/merged/405", mrTimelineEvents[2].DedupeKey)
	assert.Equal("closed", mrTimelineEvents[3].EventType)
	assert.Equal("nina", mrTimelineEvents[3].Author)
	assert.Equal("closed this", mrTimelineEvents[3].Summary)
	assert.Equal("gitea/gitea.com/gitea/tea/mr/7/closed/406", mrTimelineEvents[3].DedupeKey)
	assert.Equal("reopened", mrTimelineEvents[4].EventType)
	assert.Equal("otto", mrTimelineEvents[4].Author)
	assert.Equal("reopened this", mrTimelineEvents[4].Summary)
	assert.Equal("gitea/gitea.com/gitea/tea/mr/7/reopened/407", mrTimelineEvents[4].DedupeKey)

	issueTimelineEvents := NormalizeIssueTimelineEvents(
		platform.KindGitea,
		repo,
		9,
		[]TimelineEventDTO{
			{ID: 402, User: UserDTO{UserName: "hank"}, Type: "unassigned", Created: base},
			{ID: 404, User: UserDTO{UserName: "jane"}, Type: "change_title", PreviousTitle: "Old issue", CurrentTitle: "New issue", Created: base.Add(time.Minute)},
			{
				ID: 408, User: UserDTO{UserName: "kai"}, Type: "pull_ref", Created: base.Add(2 * time.Minute),
				Reference: &IssueReferenceDTO{
					Owner: "acme", Repo: "tools", Number: 11, Title: "Fix the issue",
					HTMLURL: "https://gitea.com/acme/tools/pulls/11", IsPullRequest: true,
				},
			},
			{
				ID: 409, Type: "pull_ref", Created: base.Add(3 * time.Minute),
				Reference: &IssueReferenceDTO{
					Owner: "acme", Repo: "tools", Number: 12,
					HTMLURL: "https://github.com/acme/tools/pull/12", IsPullRequest: true,
				},
			},
		},
	)
	require.Len(issueTimelineEvents, 3)
	assert.Equal("unassigned", issueTimelineEvents[0].EventType)
	assert.Equal("hank", issueTimelineEvents[0].Author)
	assert.Equal("removed an assignment", issueTimelineEvents[0].Summary)
	assert.Equal("gitea/gitea.com/gitea/tea/issue/9/unassigned/402", issueTimelineEvents[0].DedupeKey)
	assert.Equal("renamed_title", issueTimelineEvents[1].EventType)
	assert.Equal("jane", issueTimelineEvents[1].Author)
	assert.Equal(`"Old issue" -> "New issue"`, issueTimelineEvents[1].Summary)
	assert.JSONEq(`{"previous_title":"Old issue","current_title":"New issue"}`, issueTimelineEvents[1].MetadataJSON)
	assert.Equal("gitea/gitea.com/gitea/tea/issue/9/renamed_title/404", issueTimelineEvents[1].DedupeKey)
	assert.Equal("cross_referenced", issueTimelineEvents[2].EventType)
	assert.Contains(issueTimelineEvents[2].MetadataJSON, `"source_owner":"acme"`)
	assert.Contains(issueTimelineEvents[2].MetadataJSON, `"source_number":11`)

	release := NormalizeRelease(repo, ReleaseDTO{
		ID:          500,
		TagName:     "v1.0.0",
		Title:       "One",
		HTMLURL:     "https://gitea.com/gitea/tea/releases/v1.0.0",
		Target:      "main",
		Prerelease:  true,
		PublishedAt: &closed,
		CreatedAt:   base,
	})
	assert.Equal("v1.0.0", release.TagName)
	assert.Equal("One", release.Name)
	assert.Equal(&closed, release.PublishedAt)

	tag := NormalizeTag(repo, TagDTO{Name: "v1.0.0", Commit: CommitDTO{SHA: "abc123"}})
	assert.Equal("v1.0.0", tag.Name)
	assert.Equal("abc123", tag.SHA)
}

func TestNormalizeStatusesMapsCommitStatusesAndActionRuns(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	started := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	stopped := started.Add(time.Minute)
	laterStopped := stopped.Add(time.Minute)
	repo := platform.RepoRef{Platform: platform.KindForgejo, Host: "codeberg.org", RepoPath: "forgejo/forgejo"}

	checks := NormalizeStatuses(repo, []StatusDTO{
		{ID: 1, Context: "ci/success", State: "success", TargetURL: "https://ci/success", Description: "ok", Created: started, Updated: stopped},
		{ID: 2, Context: "ci/pending", State: "pending", TargetURL: "https://ci/pending", Created: started},
		{ID: 3, Context: "ci/error", State: "error", TargetURL: "https://ci/error", Created: started},
		{ID: 5, Context: "ci/unsafe", State: "success", TargetURL: "javascript:alert(1)", Created: started},
	}, []ActionRunDTO{
		{ID: 4, WorkflowID: "build", Title: "Build", Status: "failure", CommitSHA: "abc123", HTMLURL: "https://actions/build", Started: &started, Stopped: &stopped, NeedApproval: true},
		{ID: 5, WorkflowID: "build", Title: "Build", Status: "completed", Conclusion: "success", CommitSHA: "abc123", HTMLURL: "https://actions/build-rerun", Started: &started, Stopped: &laterStopped},
	})

	require.Len(checks, 5)
	assert.Equal("ci/success", checks[0].Name)
	assert.Equal("completed", checks[0].Status)
	assert.Equal("success", checks[0].Conclusion)
	assert.Equal("pending", checks[1].Status)
	assert.Empty(checks[1].Conclusion)
	assert.Equal("failure", checks[2].Conclusion)
	assert.Equal("ci/unsafe", checks[3].Name)
	assert.Empty(checks[3].URL)
	assert.Equal("Build", checks[4].Name)
	assert.Equal("action", checks[4].App)
	assert.Equal("success", checks[4].Conclusion)
	assert.Equal("https://actions/build-rerun", checks[4].URL)
	assert.Equal(&started, checks[4].StartedAt)
	assert.Equal(&laterStopped, checks[4].CompletedAt)
}

func TestNormalizeStatusesKeepsQueuedActionRerunAsLatest(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	started := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	stopped := started.Add(time.Minute)
	queuedAt := stopped.Add(time.Minute)
	repo := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.com", RepoPath: "gitea/tea"}

	checks := NormalizeStatuses(repo, nil, []ActionRunDTO{
		{
			ID:         10,
			RunNumber:  1,
			WorkflowID: "deploy.yml",
			Title:      "Deploy",
			Status:     "completed",
			Conclusion: "failure",
			HTMLURL:    "https://gitea/actions/10",
			Started:    &started,
			Stopped:    &stopped,
			Created:    started,
			Updated:    stopped,
		},
		{
			ID:         11,
			RunNumber:  2,
			WorkflowID: "deploy.yml",
			Title:      "Deploy",
			Status:     "queued",
			HTMLURL:    "https://gitea/actions/11",
			Created:    queuedAt,
			Updated:    queuedAt,
		},
	})

	require.Len(checks, 1)
	assert.Equal("Deploy", checks[0].Name)
	assert.Equal("pending", checks[0].Status)
	assert.Empty(checks[0].Conclusion)
	assert.Equal("https://gitea/actions/11", checks[0].URL)
}

func TestNormalizeEventsPreserveCommentHTMLURL(t *testing.T) {
	assert := assert.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	repo := platform.RepoRef{
		Platform: platform.KindGitea,
		Host:     "gitea.com",
		Owner:    "gitea",
		Name:     "tea",
		RepoPath: "gitea/tea",
	}

	mrEvents := NormalizeMergeRequestEvents(platform.KindGitea, repo, 7, []CommentDTO{{
		ID:      123,
		HTMLURL: "https://gitea.com/gitea/tea/pulls/7#issuecomment-123",
		User:    UserDTO{UserName: "alice"},
		Body:    "comment",
		Created: base,
	}}, nil, nil)
	assert.Len(mrEvents, 1)
	assert.Equal("https://gitea.com/gitea/tea/pulls/7#issuecomment-123", mrEvents[0].DirectURL)

	issueEvents := NormalizeIssueComments(platform.KindForgejo, repo, 9, []CommentDTO{{
		ID:      456,
		HTMLURL: "https://codeberg.org/gitea/tea/issues/9#issuecomment-456",
		User:    UserDTO{UserName: "bob"},
		Body:    "comment",
		Created: base,
	}})
	assert.Len(issueEvents, 1)
	assert.Equal("https://codeberg.org/gitea/tea/issues/9#issuecomment-456", issueEvents[0].DirectURL)
}

func TestNormalizeIssue_ExtractsAssignees(t *testing.T) {
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	repo := platform.RepoRef{
		Platform: platform.KindGitea,
		Host:     "gitea.com",
		Owner:    "owner",
		Name:     "repo",
		RepoPath: "owner/repo",
	}

	issue := NormalizeIssue(repo, IssueDTO{
		ID:      100,
		Index:   42,
		HTMLURL: "https://gitea.com/owner/repo/issues/42",
		Title:   "Test issue",
		User:    UserDTO{UserName: "author"},
		State:   "open",
		Body:    "issue body",
		Assignees: []UserDTO{
			{UserName: "alice"},
			{UserName: "bob"},
		},
		Created: base,
		Updated: base,
	})
	require.Equal([]string{"alice", "bob"}, issue.Assignees)
}

func TestNormalizeIssue_EmptyAssignees(t *testing.T) {
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	repo := platform.RepoRef{
		Platform: platform.KindGitea,
		Host:     "gitea.com",
		Owner:    "owner",
		Name:     "repo",
		RepoPath: "owner/repo",
	}

	issue := NormalizeIssue(repo, IssueDTO{
		ID:      100,
		Index:   42,
		HTMLURL: "https://gitea.com/owner/repo/issues/42",
		Title:   "Test issue",
		User:    UserDTO{UserName: "author"},
		State:   "open",
		Created: base,
		Updated: base,
	})
	require.Empty(issue.Assignees)
}

func TestNormalizeIssue_SkipsEmptyUsernames(t *testing.T) {
	require := Require.New(t)
	base := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	repo := platform.RepoRef{
		Platform: platform.KindGitea,
		Host:     "gitea.com",
		Owner:    "owner",
		Name:     "repo",
		RepoPath: "owner/repo",
	}

	issue := NormalizeIssue(repo, IssueDTO{
		ID:      100,
		Index:   42,
		HTMLURL: "https://gitea.com/owner/repo/issues/42",
		Title:   "Test issue",
		User:    UserDTO{UserName: "author"},
		State:   "open",
		Assignees: []UserDTO{
			{UserName: ""},
			{UserName: "alice"},
			{UserName: ""},
		},
		Created: base,
		Updated: base,
	})
	require.Equal([]string{"alice"}, issue.Assignees)
}

func TestSharedHelpersNormalizeStateDedupeAndPagination(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("open", NormalizeState("opened"))
	assert.Equal("closed", NormalizeState("closed"))
	assert.Equal("merged", NormalizeState("merged"))
	assert.Equal("custom", NormalizeState(" custom "))
	assert.Equal("owner/repo", OwnerRepoPath("owner", "repo"))
	assert.Equal(0, NextPage(0))
	assert.Equal(3, NextPage(3))
	assert.Equal(
		"forgejo/codeberg.org/forgejo/forgejo/mr/7/issue_comment/123",
		NoteDedupeKey(platform.KindForgejo, "codeberg.org", "forgejo/forgejo", "mr", 7, "issue_comment", "123"),
	)
}
