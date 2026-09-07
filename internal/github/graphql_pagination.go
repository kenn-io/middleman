package github

import (
	"context"
	"fmt"

	platformgithub "go.kenn.io/forge/platform/github"
)

// fetchAllPages accumulates all nodes from a paginated GraphQL
// connection. queryFn is called with a nil cursor for the first
// page and the previous endCursor for subsequent pages. Returns
// all accumulated nodes, or partial results plus the first error.
func fetchAllPages[T any](
	ctx context.Context,
	queryFn func(ctx context.Context, cursor *string) ([]T, platformgithub.GraphQLPageInfo, error),
) ([]T, error) {
	return fetchAllPagesWithProgress(ctx, queryFn, nil)
}

func fetchAllPagesWithProgress[T any](
	ctx context.Context,
	queryFn func(ctx context.Context, cursor *string) ([]T, platformgithub.GraphQLPageInfo, error),
	onPage func(int, bool),
) ([]T, error) {
	var all []T
	var cursor *string
	for {
		nodes, pi, err := queryFn(ctx, cursor)
		if err != nil {
			return all, err
		}
		all = append(all, nodes...)
		if onPage != nil {
			onPage(len(nodes), pi.HasNextPage)
		}
		if !pi.HasNextPage {
			break
		}
		if pi.EndCursor == "" {
			return all, fmt.Errorf(
				"graphql pagination: hasNextPage true but endCursor empty",
			)
		}
		if cursor != nil && pi.EndCursor == *cursor {
			return all, fmt.Errorf(
				"graphql pagination: endCursor unchanged (%q)",
				pi.EndCursor,
			)
		}
		c := pi.EndCursor
		cursor = &c
	}
	return all, nil
}
