package workspacetest

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestWorkspaceListReportsCommitsAheadBehindE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	var clockNow atomic.Int64
	clockNow.Store(now.UnixNano())
	fixture := setupWorkspaceServerFixture(t, nil, server.ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
		PtyOwnerInProcess:                  true,
		WorkspaceNow: func() time.Time {
			return time.Unix(0, clockNow.Load()).UTC()
		},
	})
	ws := createReadyWorkspace(t, context.Background(), fixture.client)
	workspaceByID := func() *generated.WorkspaceResponse {
		resp, err := fixture.client.HTTP.ListWorkspacesWithResponse(t.Context())
		if err != nil || resp.JSON200 == nil || resp.JSON200.Workspaces == nil {
			return nil
		}
		for i := range resp.JSON200.Workspaces {
			candidate := &resp.JSON200.Workspaces[i]
			if candidate.Id == ws.Id {
				return candidate
			}
		}
		return nil
	}
	require.Eventually(func() bool {
		initial := workspaceByID()
		return initial != nil && initial.CommitsAhead != nil && initial.CommitsBehind != nil &&
			initial.WorktreeDirty != nil && *initial.CommitsAhead == 0 &&
			*initial.CommitsBehind == 0 && !*initial.WorktreeDirty
	}, 10*time.Second, 10*time.Millisecond)

	gitfixture.Run(t, ws.WorktreePath, "update-ref", "-d", "refs/remotes/origin/feature")
	clockNow.Store(now.Add(workspaceapi.EnrichmentTTL + time.Second).UnixNano())
	require.Eventually(func() bool {
		found := workspaceByID()
		return found != nil && found.BranchUpstreamMissing != nil && *found.BranchUpstreamMissing
	}, 10*time.Second, 10*time.Millisecond)

	includePeers := false
	fleetResponse, err := fixture.client.HTTP.GetSnapshotWithResponse(
		t.Context(), &generated.GetSnapshotParams{IncludePeers: &includePeers},
	)
	require.NoError(err)
	require.NotNil(fleetResponse.JSON200)
	require.NotNil(fleetResponse.JSON200.Workspaces)
	var fleetWorkspace *generated.WorkspaceSummary
	for i := range fleetResponse.JSON200.Workspaces {
		candidate := &fleetResponse.JSON200.Workspaces[i]
		if candidate.Id == ws.Id {
			fleetWorkspace = candidate
			break
		}
	}
	require.NotNil(fleetWorkspace)
	require.NotNil(fleetWorkspace.BranchUpstreamMissing)
	assert.True(*fleetWorkspace.BranchUpstreamMissing)

	gitfixture.Run(t, ws.WorktreePath, "fetch", "origin")

	gitfixture.Run(t, ws.WorktreePath, "config", "user.email", "test@test.com")
	gitfixture.Run(t, ws.WorktreePath, "config", "user.name", "Test")
	for _, name := range []string{"ahead-1.txt", "ahead-2.txt"} {
		require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, name), []byte(name+"\n"), 0o644))
		gitfixture.Run(t, ws.WorktreePath, "add", ".")
		gitfixture.Run(t, ws.WorktreePath, "commit", "-m", name)
	}
	require.NoError(os.WriteFile(filepath.Join(ws.WorktreePath, "uncommitted.txt"), []byte("dirty\n"), 0o644))
	clockNow.Store(now.Add(2*workspaceapi.EnrichmentTTL + 2*time.Second).UnixNano())

	var found *generated.WorkspaceResponse
	require.Eventually(func() bool {
		found = workspaceByID()
		return found != nil && found.CommitsAhead != nil && found.CommitsBehind != nil &&
			found.WorktreeDirty != nil && *found.CommitsAhead == 2 &&
			*found.CommitsBehind == 0 && *found.WorktreeDirty
	}, 10*time.Second, 10*time.Millisecond)
	require.NotNil(found)
	assert.Equal(int64(2), *found.CommitsAhead)
	assert.Equal(int64(0), *found.CommitsBehind)
	assert.True(*found.WorktreeDirty)
}
