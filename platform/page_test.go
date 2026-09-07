package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectPagesDrainsMultiplePages(t *testing.T) {
	assert := assert.New(t)
	calls := []string{}
	items, err := CollectPages(t.Context(), "", func(_ context.Context, cursor string) (Page[int], error) {
		calls = append(calls, cursor)
		switch cursor {
		case "":
			return Page[int]{Items: []int{1, 2}, NextCursor: "page-2"}, nil
		case "page-2":
			return Page[int]{ProgressOnly: true, NextCursor: "page-3"}, nil
		case "page-3":
			return Page[int]{Items: []int{3}, Exhausted: true}, nil
		default:
			return Page[int]{}, errors.New("unexpected cursor")
		}
	})
	require.NoError(t, err)
	assert.Equal([]int{1, 2, 3}, items)
	assert.Equal([]string{"", "page-2", "page-3"}, calls)
}

func TestCollectPagesRejectsMissingCursor(t *testing.T) {
	_, err := CollectPages(t.Context(), "", func(context.Context, string) (Page[int], error) {
		return Page[int]{Items: []int{1}}, nil
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrProviderContract)
}

func TestCollectPagesRejectsImmediateRepeat(t *testing.T) {
	_, err := CollectPages(t.Context(), "cursor", func(_ context.Context, cursor string) (Page[int], error) {
		return Page[int]{Items: []int{1}, NextCursor: cursor}, nil
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrProviderContract)
}

func TestCollectPagesDetectsAlternatingCycle(t *testing.T) {
	calls := 0
	_, err := CollectPages(t.Context(), "a", func(_ context.Context, cursor string) (Page[int], error) {
		calls++
		switch cursor {
		case "a":
			return Page[int]{Items: []int{1}, NextCursor: "b"}, nil
		default:
			return Page[int]{Items: []int{2}, NextCursor: "a"}, nil
		}
	})

	require.ErrorIs(t, err, ErrProviderContract)
	// a -> b -> a is detected the moment the seen "a" cursor recurs.
	assert.Equal(t, 2, calls)
}

func TestCollectPagesEnforcesPageBound(t *testing.T) {
	assert := assert.New(t)
	calls := 0
	_, err := CollectPages(t.Context(), "0", func(_ context.Context, cursor string) (Page[int], error) {
		calls++
		return Page[int]{Items: []int{calls}, NextCursor: cursor + "-next"}, nil
	})

	require.ErrorIs(t, err, ErrPageLimit)
	require.NotErrorIs(t, err, ErrProviderContract)
	// Distinct advancing cursors never repeat, so only the page bound stops it.
	assert.Equal(MaxCollectPages, calls)
}

func TestCollectPagesClassifiesCycleAtPageBoundAsContractError(t *testing.T) {
	// The cursor for page N is "cN"; the fetch that would exhaust the budget
	// instead revisits the very first cursor. Cycle detection must win over
	// the page bound.
	_, err := CollectPages(t.Context(), "c0", func(_ context.Context, cursor string) (Page[int], error) {
		var n int
		_, scanErr := fmt.Sscanf(cursor, "c%d", &n)
		require.NoError(t, scanErr)
		next := fmt.Sprintf("c%d", n+1)
		if n == MaxCollectPages-1 {
			next = "c0"
		}
		return Page[int]{Items: []int{n}, NextCursor: next}, nil
	})

	require.ErrorIs(t, err, ErrProviderContract)
	require.NotErrorIs(t, err, ErrPageLimit)
}

func TestCollectPagesAllowsBoundedProgressOnlyPages(t *testing.T) {
	assert := assert.New(t)
	items, err := CollectPages(t.Context(), "", func(_ context.Context, cursor string) (Page[int], error) {
		switch cursor {
		case "":
			return Page[int]{ProgressOnly: true, NextCursor: "p1"}, nil
		case "p1":
			return Page[int]{ProgressOnly: true, NextCursor: "p2"}, nil
		case "p2":
			return Page[int]{Items: []int{7}, Exhausted: true}, nil
		default:
			return Page[int]{}, errors.New("unexpected cursor")
		}
	})
	require.NoError(t, err)
	assert.Equal([]int{7}, items)
}

func TestCollectPagesBoundsProgressOnlyCycle(t *testing.T) {
	_, err := CollectPages(t.Context(), "", func(_ context.Context, cursor string) (Page[int], error) {
		if cursor == "" {
			return Page[int]{ProgressOnly: true, NextCursor: "loop"}, nil
		}
		return Page[int]{ProgressOnly: true, NextCursor: "loop"}, nil
	})

	require.ErrorIs(t, err, ErrProviderContract)
}

func TestCollectPagesStopsOnProviderError(t *testing.T) {
	want := errors.New("provider failed")
	_, err := CollectPages(t.Context(), "", func(context.Context, string) (Page[int], error) {
		return Page[int]{}, want
	})

	require.ErrorIs(t, err, want)
}

func TestCollectPagesHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	_, err := CollectPages(ctx, "", func(context.Context, string) (Page[int], error) {
		calls++
		return Page[int]{Exhausted: true}, nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, calls)
}

func TestValidateItemPageQuery(t *testing.T) {
	watermark := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		query     ItemPageQuery
		wantField string
	}{
		{name: "created is valid", query: ItemPageQuery{Order: ItemOrderCreated, Cursor: "c1"}},
		{name: "updated is valid", query: ItemPageQuery{Order: ItemOrderUpdated, UpdatedSince: &watermark}},
		{name: "zero order is rejected", query: ItemPageQuery{}, wantField: "order"},
		{name: "unknown order is rejected", query: ItemPageQuery{Order: ItemOrder("priority")}, wantField: "order"},
		{name: "watermark requires updated order", query: ItemPageQuery{Order: ItemOrderCreated, UpdatedSince: &watermark}, wantField: "updated_since"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			err := ValidateItemPageQuery(tt.query)
			if tt.wantField == "" {
				require.NoError(err)
				return
			}
			require.ErrorIs(err, ErrInvalidArgument)
			var typed *Error
			require.ErrorAs(err, &typed)
			assert.Equal(t, tt.wantField, typed.Field)
		})
	}
}
