package github

import (
	"errors"
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

func TestCollectPagesAccumulatesAllPages(t *testing.T) {
	callPages := []int{}

	items, err := collectPages(t.Context(), func(opts *gh.ListOptions) ([]int, *gh.Response, error) {
		callPages = append(callPages, opts.Page)
		switch opts.Page {
		case 0:
			return []int{1, 2}, &gh.Response{NextPage: 2}, nil
		case 2:
			return []int{3}, &gh.Response{}, nil
		default:
			return nil, nil, errors.New("unexpected page")
		}
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3}, items)
	require.Equal(t, []int{0, 2}, callPages)
}

func TestCollectPagesStopsOnError(t *testing.T) {
	wantErr := errors.New("boom")

	_, err := collectPages(t.Context(), func(opts *gh.ListOptions) ([]int, *gh.Response, error) {
		if opts.Page == 0 {
			return []int{1}, &gh.Response{NextPage: 2}, nil
		}
		return nil, nil, wantErr
	}, nil)
	require.ErrorIs(t, err, wantErr)
}
