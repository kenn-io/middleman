package pullapi

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func TestDecodeCIChecks(t *testing.T) {
	require := require.New(t)

	none, err := decodeCIChecks("")
	require.NoError(err)
	require.Nil(none, "empty json yields no checks")

	blank, err := decodeCIChecks("   ")
	require.NoError(err)
	require.Nil(blank, "whitespace-only json yields no checks")

	checks, err := decodeCIChecks(
		`[{"name":"build","status":"completed","conclusion":"success",` +
			`"url":"https://ci/1","app":"GitHub Actions"}]`,
	)
	require.NoError(err)
	require.Len(checks, 1)
	require.Equal("build", checks[0].Name)
	require.Equal("completed", checks[0].Status)
	require.Equal("success", checks[0].Conclusion)
	require.Equal("https://ci/1", checks[0].URL)
	require.Equal("GitHub Actions", checks[0].App)

	_, err = decodeCIChecks("not json")
	require.Error(err, "malformed json is an error the caller decides how to handle")
}

func TestPendingDeferredMergeCheckKeysCapturesOnlyPendingChecks(t *testing.T) {
	checksJSON := mustDeferredMergeChecksJSON(t, []db.CICheck{
		{App: "GitHub Actions", Name: "unit", Status: "in_progress"},
		{App: "Buildkite", Name: "integration", Status: "queued"},
		{App: "GitHub Actions", Name: "lint", Status: "completed", Conclusion: "success"},
	})

	keys, err := pendingDeferredMergeCheckKeys(checksJSON)
	require.NoError(t, err)
	require.Equal(t, []deferredMergeCheckKey{
		{App: "GitHub Actions", Name: "unit"},
		{App: "Buildkite", Name: "integration"},
	}, keys)
}

func TestDeferredMergeCheckStateRequiresCapturedChecksToPass(t *testing.T) {
	keys := []deferredMergeCheckKey{
		{App: "GitHub Actions", Name: "unit"},
		{App: "Buildkite", Name: "integration"},
	}

	tests := []struct {
		name            string
		aggregateStatus string
		checks          []db.CICheck
		want            string
	}{
		{
			name:            "missing captured check stays pending",
			aggregateStatus: "success",
			checks: []db.CICheck{{
				App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success",
			}},
			want: "pending",
		},
		{
			name:            "in progress captured check stays pending",
			aggregateStatus: "pending",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "in_progress"},
			},
			want: "pending",
		},
		{
			name:            "captured checks pass with aggregate success",
			aggregateStatus: "success",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "skipped"},
			},
			want: "passed",
		},
		{
			name: "captured failure blocks merge",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "failure"},
			},
			want: "failed",
		},
		{
			name: "non captured failure blocks merge",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "success"},
				{App: "GitHub Actions", Name: "security", Status: "completed", Conclusion: "failure"},
			},
			want: "failed",
		},
		{
			name:            "non captured pending keeps waiting",
			aggregateStatus: "pending",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "success"},
				{App: "GitHub Actions", Name: "deploy", Status: "in_progress"},
			},
			want: "pending",
		},
		{
			name: "unknown aggregate blocks passing rows",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "success"},
			},
			want: "unknown",
		},
		{
			name:            "aggregate pending keeps passing rows pending",
			aggregateStatus: "pending",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "success"},
			},
			want: "pending",
		},
		{
			name:            "aggregate failure blocks passing rows",
			aggregateStatus: "failure",
			checks: []db.CICheck{
				{App: "GitHub Actions", Name: "unit", Status: "completed", Conclusion: "success"},
				{App: "Buildkite", Name: "integration", Status: "completed", Conclusion: "success"},
			},
			want: "failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deferredMergeCheckState(
				tt.aggregateStatus,
				keys,
				mustDeferredMergeChecksJSON(t, tt.checks),
			)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestClearDeferredMergeInFlightKeepsNewerHandle(t *testing.T) {
	require := require.New(t)
	handler := New(Deps{})
	key := "gitlab:gitlab.example.com:group/project#7"

	stale, marked := handler.markDeferredMergeInFlight(key)
	require.True(marked)
	// Terminal paths clear the key before broadcasting, so a new deferred
	// merge can be queued before the old worker goroutine runs its deferred
	// cleanup for the same key.
	handler.clearDeferredMergeInFlight(key, stale)
	current, marked := handler.markDeferredMergeInFlight(key)
	require.True(marked)

	// The old worker's deferred cleanup must not delete the newer handle;
	// otherwise the active deferred merge becomes untracked and a duplicate
	// can be queued.
	handler.clearDeferredMergeInFlight(key, stale)
	handler.deferredMergeMu.Lock()
	got := handler.deferredMergeInFlight[key]
	handler.deferredMergeMu.Unlock()
	require.Same(current, got)

	handler.clearDeferredMergeInFlight(key, current)
	handler.deferredMergeMu.Lock()
	require.Empty(handler.deferredMergeInFlight)
	handler.deferredMergeMu.Unlock()
}

func TestSpokePreparationTracksDeferredMergeUntilClearOrSupersede(t *testing.T) {
	require := require.New(t)
	gate := providerplane.NewProviderWriteGate(dbtest.Open(t), true)
	handler := New(Deps{ProviderWriteGate: gate})
	key := "github:github.com:acme/widget#7"

	release, err := gate.BeginDeferredMerge(t.Context())
	require.NoError(err)
	handle, marked := handler.markDeferredMergeInFlight(key, release)
	require.True(marked)
	status, err := gate.Status(t.Context())
	require.NoError(err)
	require.Equal(1, status.ActiveDeferredMerges)
	handler.clearDeferredMergeInFlight(key, handle)
	status, err = gate.Status(t.Context())
	require.NoError(err)
	require.Zero(status.ActiveDeferredMerges)

	release, err = gate.BeginDeferredMerge(t.Context())
	require.NoError(err)
	_, marked = handler.markDeferredMergeInFlight(key, release)
	require.True(marked)
	handler.supersedeDeferredMerge(key)
	status, err = gate.Status(t.Context())
	require.NoError(err)
	require.Zero(status.ActiveDeferredMerges)
}
