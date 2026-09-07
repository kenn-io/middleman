package platformdb

import (
	"go.kenn.io/forge/platform"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDBCIChecksComputesDurationFromProviderTimestamps(t *testing.T) {
	assert := assert.New(t)
	started := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	completed := started.Add(90 * time.Second)

	checks := DBCIChecks([]platform.CICheck{{
		Name:        "build",
		Status:      "completed",
		Conclusion:  "success",
		StartedAt:   &started,
		CompletedAt: &completed,
	}, {
		Name:        "pending",
		Status:      "in_progress",
		StartedAt:   &started,
		CompletedAt: nil,
	}})

	require.Len(t, checks, 2)
	require.NotNil(t, checks[0].DurationSeconds)
	assert.Equal(int64(90), *checks[0].DurationSeconds)
	assert.Nil(checks[1].DurationSeconds)
}

func TestDBMergeRequestCarriesProviderMergeableState(t *testing.T) {
	mr := DBMergeRequest(42, platform.MergeRequest{
		Number:         7,
		MergeableState: "dirty",
	})

	assert.Equal(t, "dirty", mr.MergeableState)
}

func TestDBReviewThreadsCarriesCommentMetadataToReplies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	events, threads := DBReviewThreads([]platform.MergeRequestReviewThread{
		{
			ProviderThreadID:  "thread-1",
			ProviderCommentID: "comment-1",
			MetadataJSON:      `{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`,
		},
		{
			ProviderThreadID:  "thread-1",
			ProviderCommentID: "comment-2",
			MetadataJSON:      `{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`,
		},
	})

	require.Len(threads, 1)
	require.Len(events, 2)
	assert.JSONEq(threads[0].MetadataJSON, events[0].MetadataJSON)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`, events[1].MetadataJSON)
}

func TestPreserveProviderHiddenMetadata(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		incoming string
		want     string
	}{
		{
			name:     "preserves hidden marker when edit omits metadata",
			existing: `{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`,
			want:     `{"provider_hidden":true,"provider_hidden_reason":"OFF_TOPIC"}`,
		},
		{
			name:     "keeps explicit incoming metadata",
			existing: `{"provider_hidden":true}`,
			incoming: `{"source":"edit"}`,
			want:     `{"source":"edit"}`,
		},
		{
			name:     "does not restore visible comments",
			existing: `{"provider_hidden":false}`,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PreserveProviderHiddenMetadata(tt.existing, tt.incoming))
		})
	}
}

// TestDBMergeRequestCarriesCloneURLUnknownMarker proves the persistence
// conversion threads the unknown-clone-URL marker through, so a degraded
// provider observation reaches the upsert with its preserve semantics intact.
func TestDBMergeRequestCarriesCloneURLUnknownMarker(t *testing.T) {
	mr := DBMergeRequest(42, platform.MergeRequest{
		Number:                  7,
		HeadRepoCloneURLUnknown: true,
	})

	assert.True(t, mr.HeadRepoCloneURLUnknown)
}
