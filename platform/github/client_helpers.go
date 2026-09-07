package github

import (
	"context"

	gh "github.com/google/go-github/v89/github"
)

// collectPages centralizes the standard go-github pagination loop for list
// endpoints that advance through a shared ListOptions page cursor.
// onResp is called for each page response (may be nil).
func collectPages[T any](
	ctx context.Context,
	listPage func(*gh.ListOptions) ([]T, *gh.Response, error),
	onResp func(*gh.Response),
) ([]T, error) {
	return collectPagesWithProgress(ctx, listPage, onResp, nil)
}

func collectPagesWithProgress[T any](
	ctx context.Context,
	listPage func(*gh.ListOptions) ([]T, *gh.Response, error),
	onResp func(*gh.Response),
	onPage func(int, bool),
) ([]T, error) {
	var all []T
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := listPage(opts)
		if onResp != nil {
			onResp(resp)
		}
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		hasMore := resp != nil && resp.NextPage != 0
		if onPage != nil {
			onPage(len(page), hasMore)
		}
		if !hasMore {
			return all, nil
		}
		opts.Page = resp.NextPage
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
}
