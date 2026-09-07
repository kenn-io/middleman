package github

import (
	"context"
	"testing"
	"time"

	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"
	platformgithub "go.kenn.io/forge/platform/github"
	"golang.org/x/oauth2"
)

func TestLiveGraphQLQueriesValidateAgainstGitHub(t *testing.T) {
	skipUnlessLiveGitHubTests(t)
	require := require.New(t)
	token := requireLiveGitHubToken(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpClient := oauth2.NewClient(
		context.Background(),
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
	)
	client := githubv4.NewClient(httpClient)

	var prQuery gqlPRQuery[platformgithub.GraphQLPR]
	vars := map[string]any{
		"owner":    githubv4.String("kenn-io"),
		"name":     githubv4.String("middleman"),
		"pageSize": githubv4.Int(topLevelPageSize),
		"cursor":   (*githubv4.String)(nil),
	}
	err := client.Query(ctx, &prQuery, vars)
	require.NoError(err, "bulk PR GraphQL query should validate against GitHub")

	type prDetailQuery struct {
		Repository struct {
			PullRequest *platformgithub.GraphQLPR `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	var detailQuery prDetailQuery
	err = client.Query(ctx, &detailQuery, map[string]any{
		"owner":  githubv4.String("kenn-io"),
		"name":   githubv4.String("forge"),
		"number": githubv4.Int(830),
	})
	require.NoError(err, "combined PR detail GraphQL query should validate against GitHub")
	require.NotNil(detailQuery.Repository.PullRequest, "live PR fixture should exist")
	require.NotEmpty(detailQuery.Repository.PullRequest.Comments.Nodes,
		"combined live query should return conversation comments")
	require.NotEmpty(detailQuery.Repository.PullRequest.ReviewThreads.Nodes,
		"combined live query should return review threads")
	require.NotEmpty(detailQuery.Repository.PullRequest.ReviewThreads.Nodes[0].Comments.Nodes,
		"combined live query should return review-thread comments")
	require.False(
		detailQuery.Repository.PullRequest.ReviewThreads.Nodes[0].Comments.Nodes[0].CreatedAt.IsZero(),
		"combined live query should return review-comment creation time",
	)
	require.False(
		detailQuery.Repository.PullRequest.ReviewThreads.Nodes[0].Comments.Nodes[0].UpdatedAt.IsZero(),
		"combined live query should return review-comment update time",
	)

	var issueQuery gqlIssueQuery
	err = client.Query(ctx, &issueQuery, vars)
	require.NoError(err, "bulk issue GraphQL query should validate against GitHub")

}
