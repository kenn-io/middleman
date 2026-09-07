package github_test

import (
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	legacygithub "go.kenn.io/forge/internal/github"
)

func TestLegacyNormalizePRLeavesCommentCountUnpopulated(t *testing.T) {
	pr, err := legacygithub.NormalizePR(7, &gh.PullRequest{
		ID:       new(int64(1001)),
		Number:   new(42),
		Comments: new(11),
	})
	require.NoError(t, err)

	assert.Zero(t, pr.CommentCount)
}

func TestLegacyEventNormalizersPreserveZeroPlatformIDPointers(t *testing.T) {
	assert := assert.New(t)

	comment := legacygithub.NormalizeCommentEvent(10, &gh.IssueComment{})
	require.NotNil(t, comment.PlatformID)
	assert.Equal(int64(0), *comment.PlatformID)

	review := legacygithub.NormalizeReviewEvent(10, &gh.PullRequestReview{})
	require.NotNil(t, review.PlatformID)
	assert.Equal(int64(0), *review.PlatformID)

	issueComment := legacygithub.NormalizeIssueCommentEvent(20, &gh.IssueComment{})
	require.NotNil(t, issueComment.PlatformID)
	assert.Equal(int64(0), *issueComment.PlatformID)
}
