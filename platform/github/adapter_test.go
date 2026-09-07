package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func ghTimestamp(t time.Time) *gh.Timestamp {
	return &gh.Timestamp{Time: t}
}

func githubLabel(id int64, name, description, color string, isDefault bool) *gh.Label {
	return &gh.Label{
		ID:          &id,
		Name:        &name,
		Description: &description,
		Color:       &color,
		Default:     &isDefault,
	}
}

func testRepoRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindGitHub,
		Host:     "github.com",
		Owner:    "owner",
		Name:     "repo",
		RepoPath: "owner/repo",
	}
}

func TestNormalizePullRequestMatchesPersistedDBFields(t *testing.T) {
	assert := assert.New(t)
	createdAt := time.Date(2026, 4, 8, 9, 10, 11, 0, time.UTC)
	updatedAt := createdAt.Add(2 * time.Hour)
	closedAt := updatedAt.Add(time.Hour)
	headSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mergeableState := "dirty"

	pr, err := NormalizePullRequest(testRepoRef(), &gh.PullRequest{
		ID:             new(int64(1001)),
		Number:         new(42),
		HTMLURL:        new("https://github.com/owner/repo/pull/42"),
		Title:          new("My PR"),
		User:           &gh.User{Login: new("alice"), Name: new("Alice A.")},
		State:          new("closed"),
		Merged:         new(true),
		Draft:          new(true),
		Body:           new("description"),
		Additions:      new(10),
		Deletions:      new(5),
		MergeableState: &mergeableState,
		CreatedAt:      ghTimestamp(createdAt),
		UpdatedAt:      ghTimestamp(updatedAt),
		ClosedAt:       ghTimestamp(closedAt),
		Head: &gh.PullRequestBranch{
			Ref: new("feature"),
			SHA: &headSHA,
		},
		Base: &gh.PullRequestBranch{
			Ref: new("main"),
			SHA: &baseSHA,
		},
		Labels: []*gh.Label{
			nil,
			githubLabel(5001, "needs-review", "Needs another reviewer", "fbca04", true),
		},
	})
	require.NoError(t, err)

	assert.Equal(testRepoRef(), pr.Repo)
	assert.Equal(int64(1001), pr.PlatformID)
	assert.Equal(42, pr.Number)
	assert.Equal("https://github.com/owner/repo/pull/42", pr.URL)
	assert.Equal("My PR", pr.Title)
	assert.Equal("alice", pr.Author)
	assert.Equal("Alice A.", pr.AuthorDisplayName)
	assert.Equal("merged", pr.State)
	assert.True(pr.IsDraft)
	assert.Equal("description", pr.Body)
	assert.Equal("feature", pr.HeadBranch)
	assert.Equal("main", pr.BaseBranch)
	assert.Equal(headSHA, pr.HeadSHA)
	assert.Equal(baseSHA, pr.BaseSHA)
	assert.Equal(10, pr.Additions)
	assert.Equal(5, pr.Deletions)
	assert.Equal("dirty", pr.MergeableState)
	assert.Equal(createdAt, pr.CreatedAt)
	assert.Equal(updatedAt, pr.UpdatedAt)
	assert.Equal(updatedAt, pr.LastActivityAt)
	require.NotNil(t, pr.ClosedAt)
	assert.Equal(closedAt, *pr.ClosedAt)
	require.Len(t, pr.Labels, 1)
	assert.Equal(platform.Label{
		Repo:        testRepoRef(),
		PlatformID:  5001,
		Name:        "needs-review",
		Description: "Needs another reviewer",
		Color:       "fbca04",
		IsDefault:   true,
	}, pr.Labels[0])
}

func TestNormalizeIssueAndCommentEventsUsePlatformModels(t *testing.T) {
	assert := assert.New(t)
	createdAt := time.Date(2026, 4, 9, 10, 11, 12, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)

	issue, err := NormalizeIssue(testRepoRef(), &gh.Issue{
		ID:        new(int64(2002)),
		Number:    new(77),
		HTMLURL:   new("https://github.com/owner/repo/issues/77"),
		Title:     new("Bug"),
		User:      &gh.User{Login: new("bob")},
		State:     new("open"),
		Body:      new("details"),
		Comments:  new(3),
		CreatedAt: ghTimestamp(createdAt),
		UpdatedAt: ghTimestamp(updatedAt),
		Labels: []*gh.Label{
			githubLabel(6001, "bug", "Something is broken", "d73a4a", false),
		},
	})
	require.NoError(t, err)

	assert.Equal(testRepoRef(), issue.Repo)
	assert.Equal(int64(2002), issue.PlatformID)
	assert.Equal(77, issue.Number)
	assert.Equal("Bug", issue.Title)
	assert.Equal("bob", issue.Author)
	assert.Equal("open", issue.State)
	assert.Equal(3, issue.CommentCount)
	assert.Equal(updatedAt, issue.LastActivityAt)
	require.Len(t, issue.Labels, 1)
	assert.Equal("bug", issue.Labels[0].Name)

	comment := NormalizeIssueCommentEvent(testRepoRef(), 77, &gh.IssueComment{
		ID:        new(int64(9001)),
		User:      &gh.User{Login: new("carol")},
		Body:      new("confirmed"),
		CreatedAt: ghTimestamp(createdAt),
	})

	assert.Equal(testRepoRef(), comment.Repo)
	assert.Equal(77, comment.IssueNumber)
	assert.Equal(int64(9001), comment.PlatformID)
	assert.Equal("issue_comment", comment.EventType)
	assert.Equal("carol", comment.Author)
	assert.Equal("confirmed", comment.Body)
	assert.Equal("issue-comment-9001", comment.DedupeKey)
	assert.Equal(createdAt, comment.CreatedAt)
}

func TestNormalizeCIChecksMatchesPersistedDBBehavior(t *testing.T) {
	assert := assert.New(t)
	older := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	appOne := "GitHub Actions"
	appTwo := "Buildkite"

	checks := NormalizeCIChecks(testRepoRef(), []*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      new("completed"),
			Conclusion:  new("failure"),
			HTMLURL:     new("https://github.com/owner/repo/actions/runs/100"),
			CompletedAt: ghTimestamp(older),
			App:         &gh.App{Name: &appOne},
		},
		{
			ID:          new(int64(101)),
			Name:        &name,
			Status:      new("completed"),
			Conclusion:  new("success"),
			HTMLURL:     new("https://github.com/owner/repo/actions/runs/101"),
			CompletedAt: ghTimestamp(newer),
			App:         &gh.App{Name: &appOne},
		},
		{
			ID:          new(int64(102)),
			Name:        &name,
			Status:      new("completed"),
			Conclusion:  new("neutral"),
			HTMLURL:     new("https://buildkite.com/owner/repo/builds/1"),
			CompletedAt: ghTimestamp(older),
			App:         &gh.App{Name: &appTwo},
		},
	}, &gh.CombinedStatus{
		TotalCount: new(1),
		State:      new("pending"),
		Statuses: []*gh.RepoStatus{
			{
				ID:        new(int64(201)),
				Context:   new("roborev"),
				State:     new("expected"),
				TargetURL: new("javascript:alert(1)"),
				UpdatedAt: ghTimestamp(newer),
			},
		},
	})

	require.Len(t, checks, 3)
	assert.Equal("build", checks[0].Name)
	assert.Equal("GitHub Actions", checks[0].App)
	assert.Equal("success", checks[0].Conclusion)
	assert.Equal("https://github.com/owner/repo/actions/runs/101", checks[0].URL)
	assert.Equal("build", checks[1].Name)
	assert.Equal("Buildkite", checks[1].App)
	assert.Equal("neutral", checks[1].Conclusion)
	assert.Equal("roborev", checks[2].Name)
	assert.Equal("in_progress", checks[2].Status)
	assert.Empty(checks[2].Conclusion)
	assert.Empty(checks[2].URL)
	assert.Equal("pending", DeriveOverallCIStatus(checks, &gh.CombinedStatus{
		TotalCount: new(1),
		State:      new("pending"),
	}))
}
