package github

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestLiveReviewThreadGraphQLQueriesValidateAgainstGitHub(t *testing.T) {
	if os.Getenv("KENN_FORGE_LIVE_GITHUB_TESTS") != "1" {
		t.Skip("set KENN_FORGE_LIVE_GITHUB_TESTS=1 to validate against GitHub")
	}
	token := strings.TrimSpace(os.Getenv("KENN_FORGE_GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if token == "" {
		t.Skip("set KENN_FORGE_GITHUB_TOKEN or GITHUB_TOKEN to validate against GitHub")
	}

	require := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpClient := oauth2.NewClient(
		context.Background(),
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
	)
	client, err := NewClient(ClientConfig{
		Host: "github.com", Read: httpClient, Write: httpClient,
		Notifications: httpClient, Clock: time.Now,
	})
	require.NoError(err)

	threads, _, _, err := client.ListInventoryReviewThreadsPage(
		ctx, "github.com", "kenn-io", "forge", 830, "",
	)
	require.NoError(err, "review-thread GraphQL query should validate against GitHub")
	require.NotEmpty(threads, "live review fixture should contain a review thread")
	require.NotEmpty(threads[0].Comments, "live review fixture should contain a review comment")
	require.False(threads[0].Comments[0].CreatedAt.IsZero(),
		"review-thread GraphQL query should return comment creation time")
	require.False(threads[0].Comments[0].UpdatedAt.IsZero(),
		"review-thread GraphQL query should return comment update time")

	comments, _, _, err := client.listReviewThreadCommentsPage(
		ctx, "kenn-io", "forge", 830,
		githubArchiveReviewCursor{
			Host: "github.com", Owner: "kenn-io", Repo: "forge", Number: 830,
			Phase: "comments", Thread: archiveReviewThreadCursor(threads[0]),
		},
	)
	require.NoError(err, "review-thread comment GraphQL query should validate against GitHub")
	require.NotEmpty(comments, "live review fixture should return its review thread")
	require.NotEmpty(comments[0].Comments, "live review fixture should contain a review comment")
	require.False(comments[0].Comments[0].CreatedAt.IsZero(),
		"paginated review-comment query should return creation time")
	require.False(comments[0].Comments[0].UpdatedAt.IsZero(),
		"paginated review-comment query should return update time")
}
