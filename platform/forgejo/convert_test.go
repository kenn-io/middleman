package forgejo

import (
	"testing"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform/gitealike"
)

func TestConvertRepositoryPreservesFeatureState(t *testing.T) {
	tests := []struct {
		name         string
		issues       bool
		pullRequests bool
	}{
		{name: "issues disabled", pullRequests: true},
		{name: "pull requests disabled", issues: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := convertRepository(&forgejosdk.Repository{
				ID: 1, Name: "repo", FullName: "owner/repo",
				HasIssues: tt.issues, HasPullRequests: tt.pullRequests,
			})
			require := Require.New(t)
			require.NoError(err)
			require.NotNil(repo.IssuesEnabled)
			require.NotNil(repo.MergeRequestsEnabled)
			assert := assert.New(t)
			assert.Equal(tt.issues, *repo.IssuesEnabled)
			assert.Equal(tt.pullRequests, *repo.MergeRequestsEnabled)
		})
	}
}

func TestConvertForgejoSDKRecords(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	created := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	updated := created.Add(time.Hour)
	closed := created.Add(2 * time.Hour)

	repo, err := convertRepository(&forgejosdk.Repository{
		ID:            1,
		Owner:         &forgejosdk.User{ID: 2, UserName: "forgejo", FullName: "Forgejo"},
		Name:          "forgejo",
		FullName:      "forgejo/forgejo",
		HTMLURL:       "https://codeberg.org/forgejo/forgejo",
		CloneURL:      "https://codeberg.org/forgejo/forgejo.git",
		DefaultBranch: "main",
		Private:       true,
		Archived:      true,
		Description:   "forge",
		AllowSquash:   true,
		AllowMerge:    true,
		AllowRebase:   true,
		Permissions:   &forgejosdk.Permission{Push: true, Admin: false, Pull: true},
		Created:       created,
		Updated:       updated,
	})
	require.NoError(err)
	assert.Equal(int64(1), repo.ID)
	assert.Equal("forgejo", repo.Owner.UserName)
	assert.Equal("forgejo/forgejo", repo.FullName)
	assert.Equal("main", repo.DefaultBranch)
	assert.True(repo.Private)
	assert.True(repo.Archived)
	assert.True(repo.AllowSquash)
	assert.True(repo.AllowMerge)
	assert.True(repo.AllowRebase)
	require.NotNil(repo.CanPush)
	assert.True(*repo.CanPush)
	require.NotNil(repo.CanAdmin)
	assert.False(*repo.CanAdmin)

	mergeable := true
	metrics := gitealike.PullRequestMetrics{
		AdditionsKnown: true,
		Deletions:      5,
		DeletionsKnown: true,
	}
	pr := convertPullRequest(&forgejosdk.PullRequest{
		ID:        3,
		Index:     4,
		Poster:    &forgejosdk.User{UserName: "alice", FullName: "Alice"},
		Title:     "Add thing",
		Body:      "body",
		State:     forgejosdk.StateType("open"),
		IsLocked:  true,
		Comments:  5,
		Mergeable: true,
		HTMLURL:   "https://codeberg.org/forgejo/forgejo/pulls/4",
		Head: &forgejosdk.PRBranchInfo{
			Ref:        "feature",
			Sha:        "abc",
			Repository: &forgejosdk.Repository{CloneURL: "https://example/head.git"},
		},
		Base:    &forgejosdk.PRBranchInfo{Ref: "main", Sha: "def"},
		Labels:  []*forgejosdk.Label{{ID: 6, Name: "feature", Color: "00ff00", Description: "desc"}},
		Created: &created,
		Updated: &updated,
		Closed:  &closed,
		Merged:  &closed,
		MergedBy: &forgejosdk.User{
			UserName: "merge-admin",
			FullName: "Merge Admin",
		},
	}, &mergeable, metrics)
	assert.Equal(4, pr.Index)
	assert.Equal("alice", pr.User.UserName)
	assert.Equal("merge-admin", pr.MergedBy.UserName)
	assert.Equal("open", pr.State)
	assert.True(pr.IsLocked)
	assert.False(pr.Draft)
	assert.NotNil(pr.Mergeable)
	assert.True(*pr.Mergeable)
	assert.True(pr.AdditionsKnown)
	assert.Zero(pr.Additions)
	assert.True(pr.DeletionsKnown)
	assert.Equal(5, pr.Deletions)
	assert.Equal("feature", pr.Head.Ref)
	assert.Equal("abc", pr.Head.SHA)
	assert.Equal("https://example/head.git", pr.Head.RepoCloneURL)

	draftPR := convertPullRequest(&forgejosdk.PullRequest{
		ID:      30,
		Index:   31,
		Poster:  &forgejosdk.User{UserName: "alice"},
		Title:   "WIP: Add thing",
		State:   forgejosdk.StateType("open"),
		Created: &created,
		Updated: &updated,
	}, nil, gitealike.PullRequestMetrics{})
	assert.True(draftPR.Draft)
	assert.Equal("main", pr.Base.Ref)
	assert.Equal("feature", pr.Labels[0].Name)
	assert.Equal(&closed, pr.MergedAt)

	unmergedPR := convertPullRequest(&forgejosdk.PullRequest{
		Index: 5,
	}, nil, gitealike.PullRequestMetrics{})
	assert.Empty(unmergedPR.MergedBy.UserName)

	issue := convertIssue(&forgejosdk.Issue{
		ID:          7,
		Index:       8,
		Poster:      &forgejosdk.User{UserName: "bob"},
		Title:       "Bug",
		Body:        "issue body",
		State:       forgejosdk.StateType("closed"),
		Comments:    9,
		HTMLURL:     "https://codeberg.org/forgejo/forgejo/issues/8",
		Labels:      []*forgejosdk.Label{{ID: 10, Name: "bug"}},
		Assignees:   []*forgejosdk.User{{ID: 20, UserName: "alice"}, nil, {ID: 21, UserName: "carol"}},
		Created:     created,
		Updated:     updated,
		Closed:      &closed,
		PullRequest: &forgejosdk.PullRequestMeta{},
	})
	assert.Equal(8, issue.Index)
	assert.Equal("bob", issue.User.UserName)
	assert.True(issue.IsPullRequest)
	assert.Equal("bug", issue.Labels[0].Name)
	require.Len(issue.Assignees, 2)
	assert.Equal("alice", issue.Assignees[0].UserName)
	assert.Equal("carol", issue.Assignees[1].UserName)

	comment := convertComment(&forgejosdk.Comment{ID: 11, Poster: &forgejosdk.User{UserName: "carol"}, Body: "comment", Created: created, Updated: updated})
	assert.Equal("carol", comment.User.UserName)
	assert.Equal("comment", comment.Body)

	review := convertReview(&forgejosdk.PullReview{ID: 12, Reviewer: &forgejosdk.User{UserName: "dave"}, State: forgejosdk.ReviewStateType("APPROVED"), Body: "review", Submitted: created})
	assert.Equal("APPROVED", review.State)
	assert.Equal("dave", review.User.UserName)

	release := convertRelease(&forgejosdk.Release{ID: 13, TagName: "v1", Title: "One", HTMLURL: "https://release", Target: "main", IsPrerelease: true, CreatedAt: created, PublishedAt: updated})
	assert.Equal("v1", release.TagName)
	assert.Equal(&updated, release.PublishedAt)

	tag := convertTag(&forgejosdk.Tag{Name: "v1", Commit: &forgejosdk.CommitMeta{SHA: "abc", URL: "https://commit", Created: created}})
	assert.Equal("abc", tag.Commit.SHA)

	status := convertStatus(&forgejosdk.Status{ID: 14, Context: "ci", State: forgejosdk.StatusState("success"), TargetURL: "https://ci", Description: "ok", Created: created, Updated: updated})
	assert.Equal("success", status.State)
	assert.Equal("ci", status.Context)
	assert.Equal("https://ci", status.TargetURL)

	unsafeStatus := convertStatus(&forgejosdk.Status{TargetURL: "javascript:alert(1)"})
	assert.Empty(unsafeStatus.TargetURL)
}

func TestConvertForgejoActionRun(t *testing.T) {
	assert := assert.New(t)
	created := time.Date(2026, 5, 1, 2, 0, 0, 0, time.UTC)
	started := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	stopped := started.Add(time.Minute)
	updated := stopped.Add(time.Second)

	run := convertActionRun(&forgejosdk.ActionRun{
		ID:           15,
		RunNumber:    16,
		WorkflowID:   "build.yaml",
		Title:        "Build",
		Status:       "failure",
		CommitSHA:    "abc",
		HTMLURL:      "https://actions/run",
		Created:      created,
		Started:      started,
		Stopped:      stopped,
		Updated:      updated,
		NeedApproval: true,
	})

	assert.Equal(int64(15), run.ID)
	assert.Equal(int64(16), run.RunNumber)
	assert.Equal("build.yaml", run.WorkflowID)
	assert.Equal("Build", run.Title)
	assert.Equal("failure", run.Status)
	assert.Equal("abc", run.CommitSHA)
	assert.Equal("https://actions/run", run.HTMLURL)
	assert.Equal(created, run.Created)
	assert.Equal(updated, run.Updated)
	assert.Equal(&started, run.Started)
	assert.Equal(&stopped, run.Stopped)
	assert.True(run.NeedApproval)
}
