package workspacetest

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/workspace"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestWorkspaceForceDeleteWaitsForInFlightSetupE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := t.Context()

	lockManager := workspace.NewFileLockManager()
	held, err := lockManager.Acquire(ctx, fixture.bare)
	require.NoError(err)
	unlocked := false
	defer func() {
		if !unlocked {
			_ = held.Unlock()
		}
	}()

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	ws := createResp.JSON202

	type deleteResult struct {
		status int
		err    error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		force := true
		resp, deleteErr := fixture.client.HTTP.DeleteWorkspaceWithResponse(
			ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
		)
		if deleteErr != nil {
			deleteDone <- deleteResult{err: deleteErr}
			return
		}
		deleteDone <- deleteResult{status: resp.StatusCode()}
	}()

	select {
	case result := <-deleteDone:
		require.NoError(result.err)
		require.FailNow("force-delete returned while workspace setup was paused")
	case <-time.After(500 * time.Millisecond):
	}

	require.NoError(held.Unlock())
	unlocked = true

	var result deleteResult
	select {
	case result = <-deleteDone:
	case <-time.After(10 * time.Second):
		require.FailNow("force-delete did not finish after workspace setup resumed")
	}
	require.NoError(result.err)
	assert.Equal(http.StatusNoContent, result.status)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	_, err = os.Lstat(ws.WorktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	assert.Equal(1, listBareWorktrees(t, fixture.bare))
}

func TestWorkspaceForceDeleteRejectsConcurrentRetrySetupE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	if len(workspaceTestTmuxCommand) == 0 {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	started := dir + "/delete-started"
	release := dir + "/delete-release"
	claim := dir + "/delete-claim"
	wrapper := dir + "/tmux-wrapper.sh"
	require.NoError(os.WriteFile(wrapper, []byte(`#!/bin/sh
started=$1
release=$2
claim=$3
shift 3
is_kill=false
for arg in "$@"; do
  if [ "$arg" = "kill-session" ]; then
    is_kill=true
  fi
done
if [ "$is_kill" = true ] && mkdir "$claim" 2>/dev/null; then
  : > "$started"
  while [ ! -e "$release" ]; do
    sleep 0.01
  done
fi
exec "$@"
`), 0o700))
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
	}()

	tmuxCommand := []string{wrapper, started, release, claim}
	tmuxCommand = append(tmuxCommand, workspaceTestTmuxCommand...)
	cfg := &config.Config{}
	cfg.Tmux.Command = tmuxCommand
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	ws := waitForWorkspaceReady(t, ctx, fixture.client, createResp.JSON202.Id)

	msg := "forced error for delete and retry overlap"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, ws.Id, "error", &msg,
	))

	type deleteResult struct {
		status int
		err    error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		force := true
		resp, deleteErr := fixture.client.HTTP.DeleteWorkspaceWithResponse(
			ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
		)
		if deleteErr != nil {
			deleteDone <- deleteResult{err: deleteErr}
			return
		}
		deleteDone <- deleteResult{status: resp.StatusCode()}
	}()

	require.Eventually(func() bool {
		_, statErr := os.Stat(started)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusConflict, retryResp.StatusCode())
	_, err = os.Lstat(ws.WorktreePath)
	require.NoError(err, "rejected retry must not alter the worktree")

	require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
	var result deleteResult
	select {
	case result = <-deleteDone:
	case <-time.After(10 * time.Second):
		require.FailNow("force-delete did not finish after terminal cleanup resumed")
	}
	require.NoError(result.err)
	assert.Equal(http.StatusNoContent, result.status)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	_, err = os.Lstat(ws.WorktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	assert.Equal(1, listBareWorktrees(t, fixture.bare))
}

// TestWorkspaceRetryDuringFailedDeleteIsRejectedE2E covers a retry attempted
// while an admitted DELETE is in flight and the DELETE then fails. The retry
// must not move the durable row back to creating or attach a setup worker to
// resources that teardown already owns.
func TestWorkspaceRetryDuringFailedDeleteIsRejectedE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	if len(workspaceTestTmuxCommand) == 0 {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	started := dir + "/delete-started"
	release := dir + "/delete-release"
	claim := dir + "/delete-claim"
	wrapper := dir + "/tmux-wrapper.sh"
	// The first kill-session pauses until released and then fails, so the
	// DELETE reaches destructive cleanup and errors out of it.
	require.NoError(os.WriteFile(wrapper, []byte(`#!/bin/sh
started=$1
release=$2
claim=$3
shift 3
is_kill=false
for arg in "$@"; do
  if [ "$arg" = "kill-session" ]; then
    is_kill=true
  fi
done
if [ "$is_kill" = true ] && mkdir "$claim" 2>/dev/null; then
  : > "$started"
  while [ ! -e "$release" ]; do
    sleep 0.01
  done
  echo "forced kill-session failure" >&2
  exit 1
fi
exec "$@"
`), 0o700))
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
	}()

	tmuxCommand := []string{wrapper, started, release, claim}
	tmuxCommand = append(tmuxCommand, workspaceTestTmuxCommand...)
	cfg := &config.Config{}
	cfg.Tmux.Command = tmuxCommand
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	ws := waitForWorkspaceReady(t, ctx, fixture.client, createResp.JSON202.Id)

	msg := "forced error for retry during failed delete"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, ws.Id, "error", &msg,
	))

	type deleteResult struct {
		status int
		err    error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		force := true
		resp, deleteErr := fixture.client.HTTP.DeleteWorkspaceWithResponse(
			ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
		)
		if deleteErr != nil {
			deleteDone <- deleteResult{err: deleteErr}
			return
		}
		deleteDone <- deleteResult{status: resp.StatusCode()}
	}()

	require.Eventually(func() bool {
		_, statErr := os.Stat(started)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusConflict, retryResp.StatusCode())
	_, err = os.Lstat(ws.WorktreePath)
	require.NoError(err, "rejected retry must not alter the worktree")

	require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
	var result deleteResult
	select {
	case result = <-deleteDone:
	case <-time.After(10 * time.Second):
		require.FailNow("delete did not finish after terminal cleanup resumed")
	}
	require.NoError(result.err)
	require.Equal(http.StatusInternalServerError, result.status)

	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("deletion_failed", stored.Status)
	_, err = os.Lstat(stored.WorktreePath)
	require.NoError(err)
	assert.Equal(2, listBareWorktrees(t, fixture.bare))
}

// TestWorkspaceFailedConcurrentDeleteKeepsSetupBlockedE2E covers two DELETEs
// racing on one workspace: one fails fast on the dirty preflight while the
// other is still inside destructive cleanup. The failed request must not
// reopen setup admission, and a retry during the overlap must remain rejected
// until the surviving DELETE finishes.
func TestWorkspaceFailedConcurrentDeleteKeepsSetupBlockedE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)
	if len(workspaceTestTmuxCommand) == 0 {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	started := dir + "/delete-started"
	release := dir + "/delete-release"
	claim := dir + "/delete-claim"
	wrapper := dir + "/tmux-wrapper.sh"
	require.NoError(os.WriteFile(wrapper, []byte(`#!/bin/sh
started=$1
release=$2
claim=$3
shift 3
is_kill=false
for arg in "$@"; do
  if [ "$arg" = "kill-session" ]; then
    is_kill=true
  fi
done
if [ "$is_kill" = true ] && mkdir "$claim" 2>/dev/null; then
  : > "$started"
  while [ ! -e "$release" ]; do
    sleep 0.01
  done
fi
exec "$@"
`), 0o700))
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
	}()

	tmuxCommand := []string{wrapper, started, release, claim}
	tmuxCommand = append(tmuxCommand, workspaceTestTmuxCommand...)
	cfg := &config.Config{}
	cfg.Tmux.Command = tmuxCommand
	fixture := setupWorkspaceServerFixture(t, cfg)
	ctx := t.Context()

	createResp, err := fixture.client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider:     "github",
			PlatformHost: "github.com",
			Owner:        "acme",
			Name:         "widget",
			MrNumber:     1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	ws := waitForWorkspaceReady(t, ctx, fixture.client, createResp.JSON202.Id)

	msg := "forced error for concurrent delete overlap"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, ws.Id, "error", &msg,
	))
	// Keep the worktree dirty while the first DELETE is paused in destructive
	// cleanup. The second, non-force DELETE must still report the authoritative
	// deleting lifecycle instead of offering force recovery for the dirty tree.
	require.NoError(os.WriteFile(
		ws.WorktreePath+"/untracked.txt", []byte("dirty\n"), 0o600,
	))

	type deleteResult struct {
		status int
		err    error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		force := true
		resp, deleteErr := fixture.client.HTTP.DeleteWorkspaceWithResponse(
			ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &force},
		)
		if deleteErr != nil {
			deleteDone <- deleteResult{err: deleteErr}
			return
		}
		deleteDone <- deleteResult{status: resp.StatusCode()}
	}()

	require.Eventually(func() bool {
		_, statErr := os.Stat(started)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	noForce := false
	conflictResp, err := fixture.client.HTTP.DeleteWorkspaceWithResponse(
		ctx, ws.Id, &generated.DeleteWorkspaceParams{Force: &noForce},
	)
	require.NoError(err)
	require.Equal(http.StatusConflict, conflictResp.StatusCode())
	require.NotNil(conflictResp.ApplicationproblemJSONDefault)
	assert.Equal(
		generated.WorkspaceDeletionInProgress,
		conflictResp.ApplicationproblemJSONDefault.Code,
	)

	retryResp, err := fixture.client.HTTP.RetryWorkspaceWithResponse(ctx, ws.Id)
	require.NoError(err)
	require.Equal(http.StatusConflict, retryResp.StatusCode())
	_, err = os.Lstat(ws.WorktreePath)
	require.NoError(err, "rejected retry must not alter the worktree")

	require.NoError(os.WriteFile(release, []byte("release\n"), 0o600))
	var result deleteResult
	select {
	case result = <-deleteDone:
	case <-time.After(10 * time.Second):
		require.FailNow("force-delete did not finish after terminal cleanup resumed")
	}
	require.NoError(result.err)
	require.Equal(http.StatusNoContent, result.status)

	require.Never(func() bool {
		_, statErr := os.Lstat(ws.WorktreePath)
		return statErr == nil
	}, time.Second, 10*time.Millisecond,
		"queued setup resurrected the worktree after successful deletion")
	stored, err := fixture.database.GetWorkspace(ctx, ws.Id)
	require.NoError(err)
	assert.Nil(stored)
	assert.Equal(1, listBareWorktrees(t, fixture.bare))
}

// TestWorkspaceConcurrentSameRepoOperationsE2E exercises the per-repo
// worktree lock through the public API and SQLite. Two PRs in the same
// repo are created concurrently, then one is retried while the other is
// deleted at the same time. The lock must serialize the underlying
// `git worktree add/remove/prune` calls so the bare clone ends in a
// consistent state — no wedged worktree, no half-created branch, no
// corrupt `worktrees/` metadata.
func TestWorkspaceConcurrentSameRepoOperationsE2E(t *testing.T) {
	runParallelWorkspaceGitTest(t)

	require := require.New(t)
	assert := assert.New(t)

	fixture := setupWorkspaceServerFixture(t, nil)
	ctx := context.Background()
	client := fixture.client

	// Fixture already seeded PR #1; add PR #2 in the same repo so both
	// concurrent creates target the same bare clone but different
	// worktree paths. Same-repo head evidence keeps setup on the branch
	// path; without it the fixture remote would need refs/pull/2/head.
	seedPROnHost(t, fixture.database, "github.com", "acme", "widget", 2)

	type createResult struct {
		num int
		ws  *generated.WorkspaceResponse
		err error
	}
	created := make(chan createResult, 2)
	var wg sync.WaitGroup
	for _, num := range []int{1, 2} {
		wg.Go(func() {
			resp, err := client.HTTP.CreateWorkspaceWithResponse(
				ctx,
				generated.CreateWorkspaceInputBody{
					Provider:     "github",
					PlatformHost: "github.com",
					Owner:        "acme",
					Name:         "widget",
					MrNumber:     int64(num),
				},
			)
			if err != nil {
				created <- createResult{num: num, err: err}
				return
			}
			if resp.StatusCode() != http.StatusAccepted || resp.JSON202 == nil {
				created <- createResult{
					num: num,
					err: assertableStatusErr(resp.StatusCode()),
				}
				return
			}
			ready := waitForWorkspaceReady(t, ctx, client, resp.JSON202.Id)
			created <- createResult{num: num, ws: ready}
		})
	}
	wg.Wait()
	close(created)

	wsByNumber := map[int]*generated.WorkspaceResponse{}
	for r := range created {
		require.NoError(r.err, "create PR #%d", r.num)
		require.NotNil(r.ws, "create PR #%d", r.num)
		assert.Equal("ready", r.ws.Status)
		wsByNumber[r.num] = r.ws
	}
	require.Len(wsByNumber, 2)

	// Sanity check: the bare clone should now report two managed worktrees
	// in addition to itself. If the lock failed to serialize the two
	// concurrent `worktree add` calls, this list would be missing entries
	// or have duplicate paths.
	worktreeListed := listBareWorktrees(t, fixture.bare)
	require.Equal(3, worktreeListed,
		"expected bare clone + two workspace worktrees")

	// Phase 2: drive a retry and a delete against the same bare clone at
	// the same time. The lock must keep their worktree-mutation paths
	// from clobbering each other.
	retryTarget := wsByNumber[1]
	deleteTarget := wsByNumber[2]
	msg := "forced error for retry overlap"
	require.NoError(fixture.database.UpdateWorkspaceStatus(
		ctx, retryTarget.Id, "error", &msg,
	))

	force := true
	type opResult struct {
		op  string
		err error
	}
	opOut := make(chan opResult, 2)

	var phase2 sync.WaitGroup
	phase2.Go(func() {
		resp, err := client.HTTP.DeleteWorkspaceWithResponse(
			ctx, deleteTarget.Id,
			&generated.DeleteWorkspaceParams{Force: &force},
		)
		switch {
		case err != nil:
			opOut <- opResult{op: "delete", err: err}
		case resp.StatusCode() != http.StatusNoContent:
			opOut <- opResult{op: "delete", err: assertableStatusErr(resp.StatusCode())}
		default:
			opOut <- opResult{op: "delete"}
		}
	})
	phase2.Go(func() {
		resp, err := client.HTTP.RetryWorkspaceWithResponse(ctx, retryTarget.Id)
		switch {
		case err != nil:
			opOut <- opResult{op: "retry", err: err}
		case resp.StatusCode() != http.StatusAccepted || resp.JSON202 == nil:
			opOut <- opResult{op: "retry", err: assertableStatusErr(resp.StatusCode())}
		default:
			waitForWorkspaceReady(t, ctx, client, retryTarget.Id)
			opOut <- opResult{op: "retry"}
		}
	})
	phase2.Wait()
	close(opOut)
	for r := range opOut {
		require.NoError(r.err, "phase2 op %s", r.op)
	}

	// Verify final state. Deleted workspace is gone from the DB;
	// retried workspace is ready again.
	deletedRow, err := fixture.database.GetWorkspace(ctx, deleteTarget.Id)
	require.NoError(err)
	assert.Nil(deletedRow, "deleted workspace must be absent from DB")

	retriedRow, err := fixture.database.GetWorkspace(ctx, retryTarget.Id)
	require.NoError(err)
	require.NotNil(retriedRow)
	assert.Equal("ready", retriedRow.Status)
	assert.Nil(retriedRow.ErrorMessage)

	// The bare clone should now report itself + the surviving worktree
	// (the retried one). Anything else points at a corrupt metadata
	// state — exactly the failure mode the lock is supposed to prevent.
	worktreeListed = listBareWorktrees(t, fixture.bare)
	assert.Equal(2, worktreeListed,
		"bare clone left with corrupt worktree list after concurrent ops")
}

// assertableStatusErr lets the goroutines surface unexpected status
// codes without calling t.Fatal off the test goroutine.
func assertableStatusErr(status int) error {
	return &unexpectedStatusError{status: status}
}

type unexpectedStatusError struct {
	status int
}

func (e *unexpectedStatusError) Error() string {
	return "unexpected HTTP status: " + strings.TrimSpace(http.StatusText(e.status))
}

// listBareWorktrees counts entries in `git worktree list --porcelain`
// for the given bare clone. The bare clone itself counts as one entry;
// each managed worktree adds one more.
func listBareWorktrees(t *testing.T, bare string) int {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), bare, nil, "worktree", "list", "--porcelain")
	require.NoError(t, err, "git worktree list: %s%s", out, stderr)
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			count++
		}
	}
	return count
}
