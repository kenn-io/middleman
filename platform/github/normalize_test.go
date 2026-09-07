package github

import (
	"encoding/json"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func TestNormalizeReviewCommentEventUsesReviewThreadDedupeKey(t *testing.T) {
	id := int64(222)
	comment := &gh.PullRequestComment{ID: &id}

	event := NormalizeReviewCommentEvent(platform.RepoRef{
		Owner: "acme",
		Name:  "widget",
	}, 7, comment)

	assert.Equal(t, "review_comment:222", event.DedupeKey)
}

func TestNormalizeCommentEventsPreserveHTMLURL(t *testing.T) {
	assert := assert.New(t)
	commentID := int64(123)
	commentURL := "https://github.com/acme/widget/pull/7#issuecomment-123"
	issueComment := &gh.IssueComment{
		ID:      &commentID,
		HTMLURL: &commentURL,
	}

	prEvent := NormalizeCommentEvent(platform.RepoRef{Owner: "acme", Name: "widget"}, 7, issueComment)
	issueEvent := NormalizeIssueCommentEvent(platform.RepoRef{Owner: "acme", Name: "widget"}, 9, issueComment)

	assert.Equal(commentURL, prEvent.DirectURL)
	assert.Equal(commentURL, issueEvent.DirectURL)
}

func TestNormalizeReviewCommentEventPreservesHTMLURL(t *testing.T) {
	commentID := int64(456)
	commentURL := "https://github.com/acme/widget/pull/7#discussion_r456"
	comment := &gh.PullRequestComment{
		ID:      &commentID,
		HTMLURL: &commentURL,
	}

	event := NormalizeReviewCommentEvent(platform.RepoRef{Owner: "acme", Name: "widget"}, 7, comment)

	assert.Equal(t, commentURL, event.DirectURL)
}

func TestNormalizeCommitEventUsesCommitterForRebasedCommit(t *testing.T) {
	const payload = `{
  "sha": "abcdef1234567890",
  "author": {"login": "original-author"},
  "committer": {"login": "rebase-committer"},
  "commit": {
    "message": "feat: rewrite commit",
    "author": {"name": "original-author", "date": "2026-09-03T02:21:43Z"},
    "committer": {"name": "rebase-committer", "date": "2026-09-03T04:52:04Z"}
  }
}`
	var commit gh.RepositoryCommit
	require.NoError(t, json.Unmarshal([]byte(payload), &commit))

	event := NormalizeCommitEvent(platform.RepoRef{Owner: "acme", Name: "widget"}, 7, &commit)
	assert := assert.New(t)
	assert.Equal("rebase-committer", event.Author)
	assert.Equal(time.Date(2026, 9, 3, 4, 52, 4, 0, time.UTC), event.CreatedAt)
	assert.JSONEq(`{"commit_author":"original-author"}`, event.MetadataJSON)
	assert.Equal("feat: rewrite commit", event.Body)
	assert.Equal("abcdef1234567890", event.Summary)
	assert.Equal("commit-abcdef123456", event.DedupeKey)
}

func TestNormalizeCommitEventFallbacks(t *testing.T) {
	authoredAt := time.Date(2026, 9, 3, 2, 21, 43, 0, time.UTC)
	committedAt := time.Date(2026, 9, 3, 4, 52, 4, 0, time.UTC)
	user := func(login string) *gh.User { return &gh.User{Login: new(login)} }
	signature := func(name string, date time.Time) *gh.CommitAuthor {
		return &gh.CommitAuthor{Name: new(name), Date: &gh.Timestamp{Time: date}}
	}

	cases := []struct {
		name            string
		author          *gh.User
		committer       *gh.User
		commitAuthor    *gh.CommitAuthor
		commitCommitter *gh.CommitAuthor
		wantAuthor      string
		wantCreatedAt   time.Time
		wantMetadata    string
	}{
		{
			name:            "associated committer login is preferred",
			author:          user("original-author"),
			committer:       user("rebase-committer"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: signature("rebase-name", committedAt),
			wantAuthor:      "rebase-committer",
			wantCreatedAt:   committedAt,
			wantMetadata:    `{"commit_author":"original-author"}`,
		},
		{
			name:            "web-flow committer is displayed as reported",
			author:          user("original-author"),
			committer:       user("web-flow"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: signature("web-flow", committedAt),
			wantAuthor:      "web-flow",
			wantCreatedAt:   committedAt,
			wantMetadata:    `{"commit_author":"original-author"}`,
		},
		{
			name:            "nested committer name is used without associated user",
			author:          user("original-author"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: signature("rebase-committer", committedAt),
			wantAuthor:      "rebase-committer",
			wantCreatedAt:   committedAt,
			wantMetadata:    `{"commit_author":"original-author"}`,
		},
		{
			name:          "author login is used when committer identity is absent",
			author:        user("original-author"),
			commitAuthor:  signature("original-name", authoredAt),
			wantAuthor:    "original-author",
			wantCreatedAt: authoredAt,
		},
		{
			name:          "nested author name is used without associated users",
			commitAuthor:  signature("original-author", authoredAt),
			wantAuthor:    "original-author",
			wantCreatedAt: authoredAt,
		},
		{
			name:            "author date is used when committer date is absent",
			author:          user("original-author"),
			committer:       user("rebase-committer"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: &gh.CommitAuthor{Name: new("rebase-name")},
			wantAuthor:      "rebase-committer",
			wantCreatedAt:   authoredAt,
			wantMetadata:    `{"commit_author":"original-author"}`,
		},
		{
			name:            "committer date is used without committer identity",
			author:          user("original-author"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: &gh.CommitAuthor{Date: &gh.Timestamp{Time: committedAt}},
			wantAuthor:      "original-author",
			wantCreatedAt:   committedAt,
		},
		{
			name:            "same normalized identity omits redundant metadata",
			author:          user("same-user"),
			committer:       user("same-user"),
			commitAuthor:    signature("original-name", authoredAt),
			commitCommitter: signature("committer-name", committedAt),
			wantAuthor:      "same-user",
			wantCreatedAt:   committedAt,
		},
		{
			name: "all identity and date fields absent remain empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commit := &gh.RepositoryCommit{
				Author:    tc.author,
				Committer: tc.committer,
				Commit: &gh.Commit{
					Author:    tc.commitAuthor,
					Committer: tc.commitCommitter,
				},
			}
			event := NormalizeCommitEvent(platform.RepoRef{}, 1, commit)
			assert := assert.New(t)
			assert.Equal(tc.wantAuthor, event.Author)
			assert.Equal(tc.wantCreatedAt, event.CreatedAt)
			assert.Equal(tc.wantMetadata, event.MetadataJSON)
		})
	}
}

func TestNormalizePullRequestPreservesOptionalMergeMetrics(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	changedFiles := 17
	mergeCommitSHA := "abc123"
	pull := &gh.PullRequest{
		ID: new(int64(123)), Number: new(42), State: new("closed"),
		ChangedFiles: &changedFiles, MergeCommitSHA: &mergeCommitSHA,
	}

	normalized, err := NormalizePullRequest(platform.RepoRef{}, pull)
	require.NoError(err)
	require.NotNil(normalized.FilesChanged)
	assert.Equal(changedFiles, *normalized.FilesChanged)
	assert.Equal(mergeCommitSHA, normalized.MergeCommitSHA)

	unknown, err := NormalizePullRequest(platform.RepoRef{}, &gh.PullRequest{})
	require.NoError(err)
	assert.Nil(unknown.FilesChanged)
	assert.Empty(unknown.MergeCommitSHA)
}

func TestNormalizeIssueTimelineEventCrossReferenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)

	event := NormalizeIssueTimelineEvent(platform.RepoRef{Owner: "kenn-io", Name: "roborev"}, 859, PullRequestTimelineEvent{
		NodeID:            "CRE_1",
		EventType:         "cross_referenced",
		Actor:             "mariusvniekerk",
		CreatedAt:         createdAt,
		SourceType:        "PullRequest",
		SourceOwner:       "kenn-io",
		SourceRepo:        "roborev",
		SourceNumber:      860,
		SourceTitle:       "Add global review guidelines",
		SourceURL:         "https://github.com/kenn-io/roborev/pull/860",
		IsCrossRepository: false,
		WillCloseTarget:   false,
	})

	require.NotNil(event)
	assert.Equal("cross_referenced", event.EventType)
	assert.Equal("mariusvniekerk", event.Author)
	assert.Equal("Referenced from kenn-io/roborev#860", event.Summary)
	assert.Equal(createdAt, event.CreatedAt)
	assert.Equal("timeline-CRE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"source_type":"PullRequest"`)
	assert.Contains(event.MetadataJSON, `"source_title":"Add global review guidelines"`)
	assert.Contains(event.MetadataJSON, `"source_url":"https://github.com/kenn-io/roborev/pull/860"`)
}

func TestNormalizeIssue_ExtractsAssignees(t *testing.T) {
	require := require.New(t)

	ghIssue := &gh.Issue{
		ID:      new(int64(123)),
		Number:  new(42),
		Title:   new("Test issue"),
		State:   new("open"),
		HTMLURL: new("https://github.com/owner/repo/issues/42"),
		Body:    new("Issue body"),
		User:    &gh.User{Login: new("author")},
		Assignees: []*gh.User{
			{Login: new("alice")},
			{Login: new("bob")},
		},
		CreatedAt: &gh.Timestamp{Time: time.Now()},
		UpdatedAt: &gh.Timestamp{Time: time.Now()},
	}

	issue, err := NormalizeIssue(platform.RepoRef{}, ghIssue)
	require.NoError(err)
	require.Equal([]string{"alice", "bob"}, issue.Assignees)
}

func TestNormalizeIssue_EmptyAssignees(t *testing.T) {
	require := require.New(t)

	ghIssue := &gh.Issue{
		ID:        new(int64(123)),
		Number:    new(42),
		Title:     new("Test issue"),
		State:     new("open"),
		HTMLURL:   new("https://github.com/owner/repo/issues/42"),
		Body:      new("Issue body"),
		User:      &gh.User{Login: new("author")},
		CreatedAt: &gh.Timestamp{Time: time.Now()},
		UpdatedAt: &gh.Timestamp{Time: time.Now()},
	}

	issue, err := NormalizeIssue(platform.RepoRef{}, ghIssue)
	require.NoError(err)
	require.Empty(issue.Assignees)
}

func TestNormalizeIssue_NilAssigneeInList(t *testing.T) {
	require := require.New(t)

	ghIssue := &gh.Issue{
		ID:      new(int64(123)),
		Number:  new(42),
		Title:   new("Test issue"),
		State:   new("open"),
		HTMLURL: new("https://github.com/owner/repo/issues/42"),
		Body:    new("Issue body"),
		User:    &gh.User{Login: new("author")},
		Assignees: []*gh.User{
			nil,
			{Login: new("alice")},
			{Login: nil},
		},
		CreatedAt: &gh.Timestamp{Time: time.Now()},
		UpdatedAt: &gh.Timestamp{Time: time.Now()},
	}

	issue, err := NormalizeIssue(platform.RepoRef{}, ghIssue)
	require.NoError(err)
	require.Equal([]string{"alice"}, issue.Assignees)
}

func TestNormalizePullRequestExtractsAssigneesAndRequestedReviewers(t *testing.T) {
	tests := []struct {
		name          string
		assignees     []*gh.User
		reviewers     []*gh.User
		wantAssignees []string
		wantReviewers []string
	}{
		{
			name:          "fields absent stay nil so persistence preserves prior values",
			assignees:     nil,
			reviewers:     nil,
			wantAssignees: nil,
			wantReviewers: nil,
		},
		{
			name:          "explicit empty arrays become known-empty sets",
			assignees:     []*gh.User{},
			reviewers:     []*gh.User{},
			wantAssignees: []string{},
			wantReviewers: []string{},
		},
		{
			name: "logins extracted and nil or empty entries skipped",
			assignees: []*gh.User{
				{Login: new("alice")},
				nil,
				{Login: new("")},
				{Login: new("bob")},
			},
			reviewers:     []*gh.User{{Login: new("carol")}},
			wantAssignees: []string{"alice", "bob"},
			wantReviewers: []string{"carol"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghPR := &gh.PullRequest{
				ID:                 new(int64(1)),
				Number:             new(7),
				Assignees:          tc.assignees,
				RequestedReviewers: tc.reviewers,
			}
			mr, err := NormalizePullRequest(platform.RepoRef{Owner: "acme", Name: "widget"}, ghPR)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAssignees, mr.Assignees)
			assert.Equal(t, tc.wantReviewers, mr.RequestedReviewers)
		})
	}
}
