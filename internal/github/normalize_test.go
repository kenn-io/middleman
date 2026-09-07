package github

import (
	"encoding/json"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	platformgithub "go.kenn.io/forge/platform/github"
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

func TestNormalizePRNilInputReturnsError(t *testing.T) {
	pr, err := NormalizePR(7, nil)
	require.Error(t, err)
	require.Nil(t, pr)
	require.ErrorContains(t, err, "nil pull request")
}

func TestNormalizeIssueNilInputReturnsError(t *testing.T) {
	issue, err := NormalizeIssue(10, nil)
	require.Error(t, err)
	require.Nil(t, issue)
	require.ErrorContains(t, err, "nil issue")
}

func TestNormalizeCommentVisibilityMetadata(t *testing.T) {
	comment := &gh.IssueComment{ID: new(int64(73))}
	visibility := platformgithub.CommentVisibility{Hidden: true, Reason: "OFF_TOPIC"}

	tests := []struct {
		name      string
		normalize func() string
	}{
		{
			name: "pull request comment",
			normalize: func() string {
				return NormalizeCommentEventWithVisibility(1, comment, visibility).MetadataJSON
			},
		},
		{
			name: "issue comment",
			normalize: func() string {
				return NormalizeIssueCommentEventWithVisibility(1, comment, visibility).MetadataJSON
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.JSONEq(t, `{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`, tt.normalize())
		})
	}
}

func TestNormalizePR_OpenPR(t *testing.T) {
	assert := assert.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	ghPR := &gh.PullRequest{
		ID:        new(int64(1001)),
		Number:    new(42),
		HTMLURL:   new("https://github.com/owner/repo/pull/42"),
		Title:     new("My PR"),
		User:      &gh.User{Login: new("alice")},
		State:     new("open"),
		Draft:     new(false),
		Locked:    new(true),
		Body:      new("description"),
		Additions: new(10),
		Deletions: new(5),
		CreatedAt: ghTimestamp(now),
		UpdatedAt: ghTimestamp(now),
		Head: &gh.PullRequestBranch{
			Ref: new("feature"),
			Repo: &gh.Repository{
				CloneURL: new("https://github.com/fork/repo.git"),
			},
		},
		Base: &gh.PullRequestBranch{Ref: new("main")},
	}

	pr, err := NormalizePR(7, ghPR)
	require.NoError(t, err)

	assert.Equal(int64(7), pr.RepoID)
	assert.Equal(int64(1001), pr.PlatformID)
	assert.Equal(42, pr.Number)
	assert.Equal("https://github.com/owner/repo/pull/42", pr.URL)
	assert.Equal("My PR", pr.Title)
	assert.Equal("alice", pr.Author)
	assert.Equal(db.MergeRequestStateOpen, pr.State)
	assert.False(pr.IsDraft)
	assert.True(pr.IsLocked)
	assert.Equal("description", pr.Body)
	assert.Equal(10, pr.Additions)
	assert.Equal(5, pr.Deletions)
	assert.Equal("feature", pr.HeadBranch)
	assert.Equal("main", pr.BaseBranch)
	assert.Equal("https://github.com/fork/repo.git", pr.HeadRepoCloneURL)
	assert.True(pr.CreatedAt.Equal(now))
	assert.True(pr.UpdatedAt.Equal(now))
	assert.True(pr.LastActivityAt.Equal(now))
	assert.Nil(pr.MergedAt)
}

func TestNormalizePR_MergedPR(t *testing.T) {
	assert := assert.New(t)
	mergedAt := time.Now().UTC().Truncate(time.Second)
	ghPR := &gh.PullRequest{
		ID:       new(int64(2002)),
		Number:   new(99),
		State:    new("closed"),
		Merged:   new(true),
		MergedAt: ghTimestamp(mergedAt),
		User:     &gh.User{Login: new("bob")},
	}

	pr, err := NormalizePR(3, ghPR)
	require.NoError(t, err)

	assert.Equal(db.MergeRequestStateMerged, pr.State)
	require.NotNil(t, pr.MergedAt)
	assert.True(pr.MergedAt.Equal(mergedAt))
}

func TestNormalizePR_Labels(t *testing.T) {
	require := require.New(t)
	updatedAt := time.Date(2024, 6, 7, 10, 11, 12, 0, time.UTC)
	ghPR := &gh.PullRequest{
		ID:        new(int64(3003)),
		Number:    new(11),
		UpdatedAt: ghTimestamp(updatedAt),
		Labels: []*gh.Label{
			githubLabel(5001, "needs-review", "Needs another reviewer", "fbca04", true),
		},
	}

	pr, err := NormalizePR(9, ghPR)
	require.NoError(err)

	require.Equal([]db.Label{{
		PlatformID:  5001,
		Name:        "needs-review",
		Description: "Needs another reviewer",
		Color:       "fbca04",
		IsDefault:   true,
		UpdatedAt:   updatedAt,
	}}, pr.Labels)
}

func TestNormalizeIssue_Labels(t *testing.T) {
	require := require.New(t)
	updatedAt := time.Date(2024, 6, 8, 9, 10, 11, 0, time.UTC)
	ghIssue := &gh.Issue{
		ID:        new(int64(4004)),
		Number:    new(12),
		UpdatedAt: ghTimestamp(updatedAt),
		Labels: []*gh.Label{
			githubLabel(6001, "bug", "Something is broken", "d73a4a", false),
		},
	}

	issue, err := NormalizeIssue(10, ghIssue)
	require.NoError(err)

	require.Equal([]db.Label{{
		PlatformID:  6001,
		Name:        "bug",
		Description: "Something is broken",
		Color:       "d73a4a",
		IsDefault:   false,
		UpdatedAt:   updatedAt,
	}}, issue.Labels)
}

func TestNormalizePR_LabelsSkipsMalformedLabels(t *testing.T) {
	require := require.New(t)
	updatedAt := time.Date(2024, 6, 9, 10, 11, 12, 0, time.UTC)
	blankName := "   "
	ghPR := &gh.PullRequest{
		ID:        new(int64(3004)),
		Number:    new(13),
		UpdatedAt: ghTimestamp(updatedAt),
		Labels: []*gh.Label{
			nil,
			{Name: &blankName},
			githubLabel(5002, "ready", "Ready to merge", "0e8a16", false),
		},
	}

	pr, err := NormalizePR(9, ghPR)
	require.NoError(err)

	require.Equal([]db.Label{{
		PlatformID:  5002,
		Name:        "ready",
		Description: "Ready to merge",
		Color:       "0e8a16",
		IsDefault:   false,
		UpdatedAt:   updatedAt,
	}}, pr.Labels)
}

func TestNormalizeIssue_LabelsFallbackToCreatedAt(t *testing.T) {
	require := require.New(t)
	createdAt := time.Date(2024, 6, 10, 9, 10, 11, 0, time.UTC)
	ghIssue := &gh.Issue{
		ID:        new(int64(4005)),
		Number:    new(14),
		CreatedAt: ghTimestamp(createdAt),
		Labels: []*gh.Label{
			githubLabel(6002, "triage", "Needs triage", "ededed", false),
		},
	}

	issue, err := NormalizeIssue(10, ghIssue)
	require.NoError(err)

	require.Equal([]db.Label{{
		PlatformID:  6002,
		Name:        "triage",
		Description: "Needs triage",
		Color:       "ededed",
		IsDefault:   false,
		UpdatedAt:   createdAt,
	}}, issue.Labels)
}

func TestNormalizeCommentEvent(t *testing.T) {
	assert := assert.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	c := &gh.IssueComment{
		ID:        new(int64(555)),
		HTMLURL:   new("https://github.com/acme/widget/pull/10#issuecomment-555"),
		User:      &gh.User{Login: new("carol")},
		Body:      new("looks good"),
		CreatedAt: ghTimestamp(now),
	}

	event := NormalizeCommentEvent(10, c)

	assert.Equal(int64(10), event.MergeRequestID)
	assert.Equal("issue_comment", event.EventType)
	assert.Equal("comment-555", event.DedupeKey)
	assert.Equal("carol", event.Author)
	assert.Equal("looks good", event.Body)
	assert.Equal("https://github.com/acme/widget/pull/10#issuecomment-555", event.DirectURL)
	require.NotNil(t, event.PlatformID)
	assert.Equal(int64(555), *event.PlatformID)
	assert.True(event.CreatedAt.Equal(now))
}

func TestNormalizeForcePushEvent(t *testing.T) {
	require := require.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	event := NormalizeForcePushEvent(17, platformgithub.ForcePushEvent{
		Actor:     "alice",
		BeforeSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AfterSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Ref:       "feature",
		CreatedAt: createdAt,
	})

	require.Equal(int64(17), event.MergeRequestID)
	require.Equal("force_push", event.EventType)
	require.Equal("alice", event.Author)
	require.Equal("aaaaaaa -> bbbbbbb", event.Summary)
	require.Equal("force-push-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", event.DedupeKey)
	require.Equal(createdAt, event.CreatedAt)
	require.Contains(event.MetadataJSON, `"before_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`)
	require.Contains(event.MetadataJSON, `"after_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`)
	require.Contains(event.MetadataJSON, `"ref":"feature"`)
}

func TestNormalizeTimelineEventCrossReferenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 15, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:            "CRE_1",
		EventType:         "cross_referenced",
		Actor:             "alice",
		CreatedAt:         createdAt,
		SourceType:        "Issue",
		SourceOwner:       "other",
		SourceRepo:        "repo",
		SourceNumber:      77,
		SourceTitle:       "Related bug",
		SourceURL:         "https://github.com/other/repo/issues/77",
		IsCrossRepository: true,
		WillCloseTarget:   false,
	})

	require.NotNil(event)
	assert.Equal(int64(17), event.MergeRequestID)
	assert.Equal("cross_referenced", event.EventType)
	assert.Equal("alice", event.Author)
	assert.Equal("Referenced from other/repo#77", event.Summary)
	assert.Equal(createdAt, event.CreatedAt)
	assert.Equal("timeline-CRE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"source_title":"Related bug"`)
	assert.Contains(event.MetadataJSON, `"is_cross_repository":true`)
}

func TestNormalizeTimelineEventCommentDeleted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 18, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:               "CDE_1",
		EventType:            "comment_deleted",
		Actor:                "maintainer",
		DeletedCommentAuthor: "reviewer",
		CreatedAt:            createdAt,
	})

	require.NotNil(event)
	assert.Equal(int64(17), event.MergeRequestID)
	assert.Equal("comment_deleted", event.EventType)
	assert.Equal("maintainer", event.Author)
	assert.Equal("deleted a comment from reviewer", event.Summary)
	assert.Equal(createdAt, event.CreatedAt)
	assert.Equal("timeline-CDE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"deleted_comment_author":"reviewer"`)
}

func TestNormalizeTimelineEventRenamedTitle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 5, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:        "RTE_1",
		EventType:     "renamed_title",
		Actor:         "bob",
		CreatedAt:     createdAt,
		PreviousTitle: "Old title",
		CurrentTitle:  "New title",
	})

	require.NotNil(event)
	assert.Equal("renamed_title", event.EventType)
	assert.Equal("bob", event.Author)
	assert.Equal(`"Old title" -> "New title"`, event.Summary)
	assert.Contains(event.MetadataJSON, `"previous_title":"Old title"`)
	assert.Contains(event.MetadataJSON, `"current_title":"New title"`)
}

func TestNormalizeTimelineEventBaseRefChanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 10, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:          "BRC_1",
		EventType:       "base_ref_changed",
		Actor:           "carol",
		CreatedAt:       createdAt,
		PreviousRefName: "main",
		CurrentRefName:  "release",
	})

	require.NotNil(event)
	assert.Equal("base_ref_changed", event.EventType)
	assert.Equal("carol", event.Author)
	assert.Equal("main -> release", event.Summary)
	assert.Contains(event.MetadataJSON, `"previous_ref_name":"main"`)
	assert.Contains(event.MetadataJSON, `"current_ref_name":"release"`)
}

func TestNormalizeTimelineEventAssigned(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 22, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:    "AE_1",
		EventType: "assigned",
		Actor:     "wesm",
		Assignee:  "wesm",
		CreatedAt: createdAt,
	})

	require.NotNil(event)
	assert.Equal("assigned", event.EventType)
	assert.Equal("wesm", event.Author)
	assert.Equal("self-assigned this", event.Summary)
	assert.Equal("timeline-AE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"assignee":"wesm"`)
}

func TestNormalizeTimelineEventLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		eventType string
		nodeID    string
		summary   string
	}{
		{name: "merged", eventType: "merged", nodeID: "ME_1", summary: "merged this"},
		{name: "closed", eventType: "closed", nodeID: "CE_1", summary: "closed this"},
		{name: "reopened", eventType: "reopened", nodeID: "RE_1", summary: "reopened this"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
				NodeID:    tt.nodeID,
				EventType: tt.eventType,
				Actor:     "maintainer",
				CreatedAt: createdAt,
			})

			require.NotNil(event)
			assert.Equal(int64(17), event.MergeRequestID)
			assert.Equal(tt.eventType, event.EventType)
			assert.Equal("maintainer", event.Author)
			assert.Equal(tt.summary, event.Summary)
			assert.Equal(createdAt, event.CreatedAt)
			assert.Equal("timeline-"+tt.nodeID, event.DedupeKey)
		})
	}
}

func TestNormalizeIssueTimelineEventAssigned(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 22, 0, 0, time.UTC)

	event := NormalizeIssueTimelineEvent(23, platformgithub.PullRequestTimelineEvent{
		NodeID:    "AE_1",
		EventType: "assigned",
		Actor:     "alice",
		Assignee:  "bob",
		CreatedAt: createdAt,
	})

	require.NotNil(event)
	assert.Equal(int64(23), event.IssueID)
	assert.Equal("assigned", event.EventType)
	assert.Equal("alice", event.Author)
	assert.Equal("assigned bob", event.Summary)
	assert.Equal("timeline-AE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"assignee":"bob"`)
}

func TestNormalizeIssueTimelineEventCrossReferenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)

	event := NormalizeIssueTimelineEvent(23, platformgithub.PullRequestTimelineEvent{
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
	assert.Equal(int64(23), event.IssueID)
	assert.Equal("cross_referenced", event.EventType)
	assert.Equal("mariusvniekerk", event.Author)
	assert.Equal("Referenced from kenn-io/roborev#860", event.Summary)
	assert.Equal("timeline-CRE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"source_title":"Add global review guidelines"`)
	assert.Contains(event.MetadataJSON, `"source_url":"https://github.com/kenn-io/roborev/pull/860"`)
}

func TestNormalizeIssueTimelineEventLifecycle(t *testing.T) {
	createdAt := time.Date(2024, 6, 1, 12, 40, 0, 0, time.UTC)

	tests := []struct {
		name      string
		eventType string
		nodeID    string
		summary   string
	}{
		{
			name:      "closed",
			eventType: "closed",
			nodeID:    "CE_1",
			summary:   "closed this",
		},
		{
			name:      "reopened",
			eventType: "reopened",
			nodeID:    "RE_1",
			summary:   "reopened this",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			event := NormalizeIssueTimelineEvent(23, platformgithub.PullRequestTimelineEvent{
				NodeID:    tt.nodeID,
				EventType: tt.eventType,
				Actor:     "maintainer",
				CreatedAt: createdAt,
			})

			require.NotNil(event)
			assert.Equal(int64(23), event.IssueID)
			assert.Equal(tt.eventType, event.EventType)
			assert.Equal("maintainer", event.Author)
			assert.Equal(tt.summary, event.Summary)
			assert.Equal(createdAt, event.CreatedAt)
			assert.Equal("timeline-"+tt.nodeID, event.DedupeKey)
		})
	}
}

func TestNormalizeTimelineEventForcePush(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 20, 0, 0, time.UTC)

	event := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		NodeID:    "HRFPE_1",
		EventType: "force_push",
		Actor:     "dana",
		BeforeSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AfterSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Ref:       "feature",
		CreatedAt: createdAt,
	})

	require.NotNil(event)
	assert.Equal(int64(17), event.MergeRequestID)
	assert.Equal("force_push", event.EventType)
	assert.Equal("dana", event.Author)
	assert.Equal("aaaaaaa -> bbbbbbb", event.Summary)
	assert.Equal(createdAt, event.CreatedAt)
	assert.Equal("timeline-HRFPE_1", event.DedupeKey)
	assert.Contains(event.MetadataJSON, `"before_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`)
	assert.Contains(event.MetadataJSON, `"after_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`)
	assert.Contains(event.MetadataJSON, `"ref":"feature"`)
}

func TestNormalizeTimelineEventFallbackDedupeIgnoresCrossReferenceTitle(t *testing.T) {
	require := require.New(t)
	createdAt := time.Date(2024, 6, 1, 12, 25, 0, 0, time.UTC)

	first := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		EventType:         "cross_referenced",
		Actor:             "alice",
		CreatedAt:         createdAt,
		SourceType:        "Issue",
		SourceOwner:       "other",
		SourceRepo:        "repo",
		SourceNumber:      77,
		SourceTitle:       "Original title",
		SourceURL:         "https://github.com/other/repo/issues/77",
		IsCrossRepository: true,
		WillCloseTarget:   false,
	})
	second := NormalizeTimelineEvent(17, platformgithub.PullRequestTimelineEvent{
		EventType:         "cross_referenced",
		Actor:             "alice",
		CreatedAt:         createdAt,
		SourceType:        "Issue",
		SourceOwner:       "other",
		SourceRepo:        "repo",
		SourceNumber:      77,
		SourceTitle:       "Renamed title",
		SourceURL:         "https://github.com/other/repo/issues/77",
		IsCrossRepository: true,
		WillCloseTarget:   false,
	})

	require.NotNil(first)
	require.NotNil(second)
	require.Equal(first.DedupeKey, second.DedupeKey)
	require.Contains(first.MetadataJSON, `"source_title":"Original title"`)
	require.Contains(second.MetadataJSON, `"source_title":"Renamed title"`)
}

func TestNormalizeCommitEvent_CreatedAtIsUTC(t *testing.T) {
	assert := assert.New(t)
	//nolint:forbidigo // Test fixture intentionally uses a non-UTC zone to verify normalization.
	edt := time.FixedZone("EDT", -4*3600)
	authorDate := time.Date(2024, 6, 1, 10, 30, 0, 0, edt)
	sha := "abcdef1234567890abcdef1234567890abcdef12"

	commit := &gh.RepositoryCommit{
		SHA: &sha,
		Commit: &gh.Commit{
			Message: new("fix things"),
			Author: &gh.CommitAuthor{
				Date: &gh.Timestamp{Time: authorDate},
				Name: new("Alice"),
			},
		},
		Author: &gh.User{Login: new("alice")},
	}

	event := NormalizeCommitEvent(5, commit)

	assert.Equal(time.UTC, event.CreatedAt.Location())
	assert.True(event.CreatedAt.Equal(authorDate))
}

func TestNormalizeIssueCommentEvent(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)
	id := int64(777)
	body := "needs follow-up"
	login := "dana"
	htmlURL := "https://github.com/acme/widget/issues/12#issuecomment-777"
	c := &gh.IssueComment{
		ID:        &id,
		HTMLURL:   &htmlURL,
		Body:      &body,
		User:      &gh.User{Login: &login},
		CreatedAt: &gh.Timestamp{Time: now},
	}

	event := NormalizeIssueCommentEvent(12, c)

	assert.Equal(int64(12), event.IssueID)
	assert.Equal("issue_comment", event.EventType)
	assert.Equal("issue-comment-777", event.DedupeKey)
	assert.Equal("dana", event.Author)
	assert.Equal("needs follow-up", event.Body)
	assert.Equal("https://github.com/acme/widget/issues/12#issuecomment-777", event.DirectURL)
	require.NotNil(t, event.PlatformID)
	assert.Equal(int64(777), *event.PlatformID)
	assert.True(event.CreatedAt.Equal(now))
}

func TestNormalizePR_MergeableState(t *testing.T) {
	tests := []struct {
		name  string
		state *string
		want  string
	}{
		{"dirty", new("dirty"), "dirty"},
		{"clean", new("clean"), "clean"},
		{"unknown", new("unknown"), "unknown"},
		{"blocked", new("blocked"), "blocked"},
		{"behind", new("behind"), "behind"},
		{"unstable", new("unstable"), "unstable"},
		{"has_hooks", new("has_hooks"), "has_hooks"},
		{"draft", new("draft"), "draft"},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ghPR := &gh.PullRequest{
				ID:             new(int64(1)),
				Number:         new(1),
				State:          new("open"),
				MergeableState: tt.state,
			}
			pr, err := NormalizePR(1, ghPR)
			require.NoError(t, err)
			assert.Equal(t, tt.want, pr.MergeableState)
		})
	}
}

func TestDeriveOverallCIStatus_NoChecksOrStatuses(t *testing.T) {
	result := DeriveOverallCIStatus(nil, nil)
	assert.Empty(t, result)
}

func TestDeriveOverallCIStatus_EmptyCombined(t *testing.T) {
	combined := &gh.CombinedStatus{State: new("pending")}
	result := DeriveOverallCIStatus(nil, combined)
	assert.Empty(t, result, "no actual statuses means empty, even if state says pending")
}

func TestDeriveOverallCIStatus_CheckRunsOnly(t *testing.T) {
	tests := []struct {
		name string
		runs []*gh.CheckRun
		want string
	}{
		{
			name: "all success",
			runs: []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new("success")},
				{Status: new("completed"), Conclusion: new("success")},
			},
			want: "success",
		},
		{
			name: "one pending",
			runs: []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new("success")},
				{Status: new("in_progress")},
			},
			want: "pending",
		},
		{
			name: "one queued",
			runs: []*gh.CheckRun{
				{Status: new("queued")},
			},
			want: "pending",
		},
		{
			name: "one failure",
			runs: []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new("success")},
				{Status: new("completed"), Conclusion: new("failure")},
			},
			want: "failure",
		},
		{
			name: "failure beats pending",
			runs: []*gh.CheckRun{
				{Status: new("in_progress")},
				{Status: new("completed"), Conclusion: new("failure")},
			},
			want: "failure",
		},
		{
			name: "timed out is failure",
			runs: []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new("timed_out")},
			},
			want: "failure",
		},
		{
			name: "skipped counts as success",
			runs: []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new("skipped")},
			},
			want: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveOverallCIStatus(tt.runs, nil)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeriveOverallCIStatus_NonSuccessConclusions(t *testing.T) {
	// Any completed conclusion not in {success, neutral, skipped} is failure.
	for _, conclusion := range []string{
		"action_required", "failure", "timed_out",
		"cancelled", "stale", "startup_failure",
	} {
		t.Run(conclusion, func(t *testing.T) {
			runs := []*gh.CheckRun{
				{Status: new("completed"), Conclusion: new(conclusion)},
			}
			assert.Equal(t, "failure", DeriveOverallCIStatus(runs, nil))
		})
	}
}

func TestDeriveOverallCIStatus_CombinedStatusOnly(t *testing.T) {
	combined := &gh.CombinedStatus{
		TotalCount: new(1),
		State:      new("success"),
		Statuses: []*gh.RepoStatus{
			{State: new("success"), Context: new("ci/build")},
		},
	}
	assert.Equal(t, "success", DeriveOverallCIStatus(nil, combined))
}

func TestDeriveOverallCIStatus_CombinedUsesAggregatedState(t *testing.T) {
	// Statuses slice may be truncated by pagination; the pre-aggregated
	// State field reflects all pages, so we rely on it instead.
	combined := &gh.CombinedStatus{
		TotalCount: new(50),
		State:      new("failure"),
		Statuses: []*gh.RepoStatus{
			{State: new("success"), Context: new("ci/build")},
		},
	}
	assert.Equal(t, "failure", DeriveOverallCIStatus(nil, combined))
}

func TestDeriveOverallCIStatus_MixedSources(t *testing.T) {
	runs := []*gh.CheckRun{
		{Status: new("completed"), Conclusion: new("success")},
	}
	combined := &gh.CombinedStatus{
		TotalCount: new(1),
		State:      new("pending"),
		Statuses: []*gh.RepoStatus{
			{State: new("pending"), Context: new("ci/deploy")},
		},
	}
	assert.Equal(t, "pending", DeriveOverallCIStatus(runs, combined))
}

func TestNormalizeCIChecks_ExpectedAndPendingStatus(t *testing.T) {
	assert := assert.New(t)

	combined := &gh.CombinedStatus{
		TotalCount: new(2),
		State:      new("pending"),
		Statuses: []*gh.RepoStatus{
			{State: new("pending"), Context: new("ci/build")},
			{State: new("expected"), Context: new("ci/required")},
		},
	}

	raw := NormalizeCIChecks(nil, combined)
	require.NotEmpty(t, raw)

	var checks []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 2)

	assert.Equal("ci/build", checks[0].Name)
	assert.Equal("in_progress", checks[0].Status)
	assert.Empty(checks[0].Conclusion)

	assert.Equal("ci/required", checks[1].Name)
	assert.Equal("in_progress", checks[1].Status)
	assert.Empty(checks[1].Conclusion)
}

func TestNormalizeCheckRuns_SortsByCasefoldedName(t *testing.T) {
	assert := assert.New(t)

	buildName := "build"
	zebraName := "Zebra"
	alphaName := "alpha"
	statusCompleted := "completed"
	conclusionSuccess := "success"

	raw := NormalizeCheckRuns([]*gh.CheckRun{
		{Name: &buildName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
		{Name: &zebraName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
		{Name: &alphaName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
	})
	require.NotEmpty(t, raw)

	var checks []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 3)

	assert.Equal("alpha", checks[0].Name)
	assert.Equal("build", checks[1].Name)
	assert.Equal("Zebra", checks[2].Name)
}

func TestNormalizeCheckRunsIncludesDurationWhenTimestampsPresent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	name := "build"
	status := "completed"
	conclusion := "success"
	started := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	completed := started.Add(2*time.Minute + 15*time.Second)

	raw := NormalizeCheckRuns([]*gh.CheckRun{{
		Name:        &name,
		Status:      &status,
		Conclusion:  &conclusion,
		StartedAt:   ghTimestamp(started),
		CompletedAt: ghTimestamp(completed),
	}})
	require.NotEmpty(raw)

	var checks []struct {
		Name            string `json:"name"`
		DurationSeconds *int64 `json:"duration_seconds,omitempty"`
	}
	require.NoError(json.Unmarshal([]byte(raw), &checks))
	require.Len(checks, 1)

	assert.Equal("build", checks[0].Name)
	require.NotNil(checks[0].DurationSeconds)
	assert.Equal(int64(135), *checks[0].DurationSeconds)
}

func TestNormalizeCIChecks_LatestCheckRunPerNameWins(t *testing.T) {
	assert := assert.New(t)

	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	statusCompleted := "completed"
	conclusionFailure := "failure"
	conclusionSuccess := "success"

	raw := NormalizeCIChecks([]*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionFailure,
			CompletedAt: ghTimestamp(older),
		},
		{
			ID:          new(int64(101)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionSuccess,
			CompletedAt: ghTimestamp(newer),
		},
	}, nil)
	require.NotEmpty(t, raw)

	var checks []db.CICheck
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 1)

	assert.Equal("build", checks[0].Name)
	assert.Equal("completed", checks[0].Status)
	assert.Equal("success", checks[0].Conclusion)
}

func TestNormalizeCIChecks_CheckRunMissingCompletedAtFallsBackToStartedAt(t *testing.T) {
	assert := assert.New(t)

	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	statusCompleted := "completed"
	statusInProgress := "in_progress"
	conclusionFailure := "failure"

	raw := NormalizeCIChecks([]*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionFailure,
			CompletedAt: ghTimestamp(older),
			StartedAt:   ghTimestamp(older.Add(-2 * time.Minute)),
		},
		{
			ID:        new(int64(101)),
			Name:      &name,
			Status:    &statusInProgress,
			StartedAt: ghTimestamp(newer),
		},
	}, nil)
	require.NotEmpty(t, raw)

	var checks []db.CICheck
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 1)

	assert.Equal("build", checks[0].Name)
	assert.Equal("in_progress", checks[0].Status)
	assert.Empty(checks[0].Conclusion)
}

func TestNormalizeCIChecks_CheckRunMissingStartedAtFallsBackToCreatedAt(t *testing.T) {
	assert := assert.New(t)

	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	statusQueued := "queued"
	statusCompleted := "completed"
	conclusionSuccess := "success"

	raw := NormalizeCIChecks([]*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionSuccess,
			CompletedAt: ghTimestamp(older),
			CheckSuite:  &gh.CheckSuite{CreatedAt: ghTimestamp(older.Add(-2 * time.Minute))},
		},
		{
			ID:         new(int64(101)),
			Name:       &name,
			Status:     &statusQueued,
			CheckSuite: &gh.CheckSuite{CreatedAt: ghTimestamp(newer)},
		},
	}, nil)
	require.NotEmpty(t, raw)

	var checks []db.CICheck
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 1)

	assert.Equal("build", checks[0].Name)
	assert.Equal("queued", checks[0].Status)
	assert.Empty(checks[0].Conclusion)
}

func TestNormalizeCIChecks_DeduplicatesBySourceAwareKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	statusCompleted := "completed"
	conclusionFailure := "failure"
	conclusionSuccess := "success"
	conclusionNeutral := "neutral"
	appOne := "GitHub Actions"
	appTwo := "Buildkite"

	raw := NormalizeCIChecks([]*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionFailure,
			CompletedAt: ghTimestamp(older),
			App:         &gh.App{Name: &appOne},
		},
		{
			ID:          new(int64(101)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionSuccess,
			CompletedAt: ghTimestamp(newer),
			App:         &gh.App{Name: &appOne},
		},
		{
			ID:          new(int64(102)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionNeutral,
			CompletedAt: ghTimestamp(older),
			App:         &gh.App{Name: &appTwo},
		},
	}, nil)
	require.NotEmpty(raw)

	var checks []db.CICheck
	require.NoError(json.Unmarshal([]byte(raw), &checks))
	require.Len(checks, 2)

	byApp := make(map[string]db.CICheck, len(checks))
	for _, check := range checks {
		byApp[check.App] = check
	}
	require.Contains(byApp, "Buildkite")
	require.Contains(byApp, "GitHub Actions")
	assert.Equal("build", byApp["Buildkite"].Name)
	assert.Equal("neutral", byApp["Buildkite"].Conclusion)
	assert.Equal("build", byApp["GitHub Actions"].Name)
	assert.Equal("success", byApp["GitHub Actions"].Conclusion)
}

func TestNormalizeCIChecks_StatusMissingUpdatedAtFallsBackToCreatedAt(t *testing.T) {
	assert := assert.New(t)

	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	context := "roborev"
	oldState := "success"
	newState := "failure"

	raw := NormalizeCIChecks(nil, &gh.CombinedStatus{
		TotalCount: new(2),
		State:      &newState,
		Statuses: []*gh.RepoStatus{
			{
				ID:        new(int64(100)),
				Context:   &context,
				State:     &oldState,
				UpdatedAt: ghTimestamp(older),
				CreatedAt: ghTimestamp(older.Add(-2 * time.Minute)),
			},
			{
				ID:        new(int64(101)),
				Context:   &context,
				State:     &newState,
				CreatedAt: ghTimestamp(newer),
			},
		},
	})
	require.NotEmpty(t, raw)

	var checks []db.CICheck
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 1)

	assert.Equal("roborev", checks[0].Name)
	assert.Equal("completed", checks[0].Status)
	assert.Equal("failure", checks[0].Conclusion)
}

func TestDeriveOverallCIStatus_LatestCheckRunPerNameWins(t *testing.T) {
	older := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)
	name := "build"
	statusCompleted := "completed"
	conclusionFailure := "failure"
	conclusionSuccess := "success"

	result := DeriveOverallCIStatus([]*gh.CheckRun{
		{
			ID:          new(int64(100)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionFailure,
			CompletedAt: ghTimestamp(older),
		},
		{
			ID:          new(int64(101)),
			Name:        &name,
			Status:      &statusCompleted,
			Conclusion:  &conclusionSuccess,
			CompletedAt: ghTimestamp(newer),
		},
	}, nil)

	assert.Equal(t, "success", result)
}

func TestNormalizeCIChecks_SortsByCasefoldedName(t *testing.T) {
	assert := assert.New(t)

	buildName := "build"
	zebraName := "Zebra"
	alphaName := "alpha"
	statusCompleted := "completed"
	conclusionSuccess := "success"

	raw := NormalizeCIChecks([]*gh.CheckRun{
		{Name: &buildName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
		{Name: &zebraName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
		{Name: &alphaName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
	}, nil)
	require.NotEmpty(t, raw)

	var checks []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &checks))
	require.Len(t, checks, 3)

	assert.Equal("alpha", checks[0].Name)
	assert.Equal("build", checks[1].Name)
	assert.Equal("Zebra", checks[2].Name)
}

func TestDeriveReviewDecision_Empty(t *testing.T) {
	result := DeriveReviewDecision(nil)
	assert.Empty(t, result)
}

func TestDeriveReviewDecision_ApprovedOnly(t *testing.T) {
	reviews := []*gh.PullRequestReview{
		{User: &gh.User{Login: new("alice")}, State: new("APPROVED")},
		{User: &gh.User{Login: new("bob")}, State: new("COMMENTED")},
	}
	result := DeriveReviewDecision(reviews)
	assert.Equal(t, "approved", result)
}

func TestDeriveReviewDecision_ChangesRequestedWins(t *testing.T) {
	reviews := []*gh.PullRequestReview{
		{User: &gh.User{Login: new("alice")}, State: new("APPROVED")},
		{User: &gh.User{Login: new("bob")}, State: new("CHANGES_REQUESTED")},
	}
	result := DeriveReviewDecision(reviews)
	assert.Equal(t, "changes_requested", result)
}

func TestDeriveReviewDecision_CommentedOnlyIgnored(t *testing.T) {
	reviews := []*gh.PullRequestReview{
		{User: &gh.User{Login: new("alice")}, State: new("COMMENTED")},
		{User: &gh.User{Login: new("bob")}, State: new("DISMISSED")},
	}
	result := DeriveReviewDecision(reviews)
	assert.Empty(t, result)
}

func TestDeriveReviewDecision_LatestStatePerUser(t *testing.T) {
	// bob first requested changes, then approved — latest should be APPROVED
	reviews := []*gh.PullRequestReview{
		{User: &gh.User{Login: new("bob")}, State: new("CHANGES_REQUESTED")},
		{User: &gh.User{Login: new("bob")}, State: new("APPROVED")},
	}
	result := DeriveReviewDecision(reviews)
	assert.Equal(t, "approved", result)
}

func TestNormalizePR_BotUserDisplayName(t *testing.T) {
	assert := assert.New(t)
	ghPR := &gh.PullRequest{
		ID:     new(int64(3003)),
		Number: new(7),
		State:  new("open"),
		User: &gh.User{
			Login: new("renovate[bot]"),
			Type:  new("Bot"),
		},
	}

	pr, err := NormalizePR(1, ghPR)
	require.NoError(t, err)

	assert.Equal("renovate[bot]", pr.Author)
	assert.Equal("renovate[bot]", pr.AuthorDisplayName,
		"bot users should get login as display name")
}

func TestNameOrEmpty(t *testing.T) {
	tests := []struct {
		name string
		user *gh.User
		want string
	}{
		{
			name: "nil user",
			user: nil,
			want: "",
		},
		{
			name: "regular user with name",
			user: &gh.User{Login: new("alice"), Name: new("Alice Smith")},
			want: "Alice Smith",
		},
		{
			name: "regular user without name",
			user: &gh.User{Login: new("alice")},
			want: "",
		},
		{
			name: "bot user returns login",
			user: &gh.User{Login: new("dependabot[bot]"), Type: new("Bot")},
			want: "dependabot[bot]",
		},
		{
			name: "bot user with name still returns login",
			user: &gh.User{Login: new("mybot[bot]"), Type: new("Bot"), Name: new("My Bot")},
			want: "mybot[bot]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nameOrEmpty(tt.user))
		})
	}
}
