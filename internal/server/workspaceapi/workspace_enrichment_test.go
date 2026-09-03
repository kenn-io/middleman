package workspaceapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestApplyWorktreeDivergenceReportsMissingConfiguredUpstream(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	work := gitfixture.DivergenceWorktree(t)
	gitfixture.Run(t, work, "update-ref", "-d", "refs/remotes/origin/feature")
	resp := workspaceResponse{ID: "ws-first-push"}

	err := applyWorktreeDivergence(t.Context(), &resp, work)

	require.NoError(err)
	require.NotNil(resp.BranchUpstreamMissing)
	assert.True(*resp.BranchUpstreamMissing)
	assert.Nil(resp.CommitsAhead)
	assert.Nil(resp.CommitsBehind)
}

func newEnrichmentTestHandler(t *testing.T, tmuxScript string) *Handler {
	t.Helper()
	database := dbtest.Open(t)
	manager := workspace.NewManager(database, t.TempDir())
	if tmuxScript != "" {
		manager.SetTmuxCommand([]string{tmuxScript})
	}
	ctx, cancel := context.WithCancel(context.Background())
	handler := New(Deps{
		DB:         database,
		Workspaces: manager,
	})
	handler.Start(ctx, true)
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		require.NoError(t, handler.Shutdown(shutdownCtx))
	})
	return handler
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_, stderr, err := gitcmd.New().WithConfig("init.defaultBranch", "main").Run(
		t.Context(), dir, nil, args...,
	)
	require.NoError(t, err, "git %v failed: %s", args, stderr)
}

func TestApplyWorktreeDirtyDoesNotWriteTheGitIndex(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	trackedPath := filepath.Join(dir, "tracked.txt")
	require.NoError(os.WriteFile(trackedPath, []byte("base\n"), 0o644))
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "base")
	indexPath := filepath.Join(dir, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	require.NoError(err)
	future := time.Now().Add(2 * time.Hour)
	require.NoError(os.Chtimes(trackedPath, future, future))

	var resp workspaceResponse
	require.NoError(applyWorktreeDirty(t.Context(), &resp, dir))
	require.NotNil(resp.WorktreeDirty)
	require.False(*resp.WorktreeDirty)
	indexAfter, err := os.ReadFile(indexPath)
	require.NoError(err)
	require.Equal(indexBefore, indexAfter)
}

func TestFormatAgentActivityUpdatedAtPreservesSubsecondPrecision(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, 7, 28, 12, 0, 0, 123456789, time.UTC)

	assert.Equal(t, "2026-07-28T12:00:00.123456789Z", formatAgentActivityUpdatedAt(updatedAt))
}

func TestWorkspaceEnrichmentRestoresDivergenceAfterObserverHealsUpstream(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	worktree := filepath.Join(dir, "worktree")

	runGit(t, dir, "init", "--bare", "--initial-branch=main", remote)
	runGit(t, dir, "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "test@test.com")
	runGit(t, seed, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644))
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644))
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "feature")
	runGit(t, seed, "push", "-u", "origin", "feature")
	runGit(t, dir, "clone", remote, worktree)
	runGit(t, worktree, "checkout", "feature")
	runGit(t, worktree, "config", "user.email", "test@test.com")
	runGit(t, worktree, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(worktree, "ahead.txt"), []byte("ahead\n"), 0o644))
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "ahead")
	runGit(t, worktree, "config", "--unset", "branch.feature.remote")
	runGit(t, worktree, "config", "--unset", "branch.feature.merge")

	database := dbtest.Open(t)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(
		t.Context(), identity,
	)
	require.NoError(err)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	seedMR := func(headRepoCloneURL string) {
		_, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
			RepoID:           repoID,
			PlatformID:       1000,
			Number:           1,
			URL:              "https://github.com/acme/widget/pull/1",
			Title:            "Test PR #1",
			Author:           "testuser",
			State:            db.MergeRequestStateOpen,
			HeadBranch:       "feature",
			HeadRepoCloneURL: headRepoCloneURL,
			BaseBranch:       "main",
			CreatedAt:        now,
			UpdatedAt:        now,
			LastActivityAt:   now,
		})
		require.NoError(err)
	}
	seedMR("https://github.com/contributor/widget.git")
	require.NoError(database.InsertWorkspace(t.Context(), &db.Workspace{
		ID:              "ws-upstream-heal",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      1,
		GitHeadRef:      "feature",
		WorkspaceBranch: "feature",
		WorktreePath:    worktree,
		Status:          "ready",
		CreatedAt:       now,
	}))
	launchSpec := workspaceLaunchSpecForRequest(
		providerplane.WorkspaceLaunchRequest{
			Repository: providerplane.RepositoryRoute{
				Provider: "github", PlatformHost: "github.com",
				Owner: "acme", Name: "widget",
			},
			ItemType:   db.WorkspaceItemTypePullRequest,
			ItemNumber: 1, ItemKey: "1", GitHeadRef: "feature",
		},
		now,
	)
	launchSpec.Pull.HeadRepoKind = "fork"
	launchSpec.Pull.HeadRepoCloneURL = "https://github.com/contributor/widget.git"
	require.NoError(database.PutWorkspaceLaunchSpec(
		t.Context(), "ws-upstream-heal", launchSpec,
	))
	summary, err := database.GetWorkspaceSummary(t.Context(), "ws-upstream-heal")
	require.NoError(err)
	require.NotNil(summary)

	clockNow := now
	manager := workspace.NewManager(database, filepath.Join(dir, "managed-worktrees"))
	handler := New(Deps{
		DB:         database,
		Workspaces: manager,
		Now:        func() time.Time { return clockNow },
	})
	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx, true)
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		require.NoError(handler.Shutdown(shutdownCtx))
	})

	broken := handler.refreshWorkspaceResponse(t.Context(), summary)
	assert.Nil(broken.CommitsAhead)
	assert.Nil(broken.CommitsBehind)
	assert.Equal(workspaceEnrichmentFresh, broken.EnrichmentStatus)

	seedMR("https://github.com/acme/widget.git")
	handler.runWorkspacePushedHeadObserverPass(t.Context())
	assert.Equal("origin", runGitOutput(t, worktree, "config", "--get", "branch.feature.remote"))
	assert.Equal(
		"refs/heads/feature",
		runGitOutput(t, worktree, "config", "--get", "branch.feature.merge"),
	)
	clockNow = now.Add(workspaceEnrichmentTTL + time.Second)

	var healed workspaceResponse
	require.Eventually(func() bool {
		healed = handler.toCachedWorkspaceResponse(summary)
		return healed.CommitsAhead != nil && healed.CommitsBehind != nil &&
			*healed.CommitsAhead == 1 && *healed.CommitsBehind == 0 &&
			healed.EnrichmentStatus == workspaceEnrichmentFresh
	}, 2*time.Second, 10*time.Millisecond)
	require.NotNil(healed.CommitsAhead)
	require.NotNil(healed.CommitsBehind)
	assert.Equal(1, *healed.CommitsAhead)
	assert.Equal(0, *healed.CommitsBehind)
}

func TestWorkspaceEnrichmentSupersedeRejectsOlderRefreshAndPreservesCache(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	srv := &Handler{
		now:                            func() time.Time { return now },
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: make(map[string]uint64),
	}

	oldGeneration := srv.workspaceEnrichmentGeneration("ws-1")
	ahead := 1
	srv.workspaceEnrichmentCache["ws-1"] = workspaceEnrichmentCacheEntry{
		response:              workspaceResponse{CommitsAhead: &ahead},
		hasDivergence:         true,
		divergenceRefreshedAt: now,
	}
	srv.supersedeWorkspaceEnrichment("ws-1")
	entry, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		"ws-1",
		oldGeneration,
		workspaceEnrichmentProbeResult{
			response:           workspaceResponse{CommitsAhead: &ahead},
			divergenceComplete: true,
		},
	)

	assert.False(recorded)
	assert.Equal(&ahead, entry.response.CommitsAhead)
	assert.Contains(srv.workspaceEnrichmentCache, "ws-1")
	assert.Equal(&ahead, srv.workspaceEnrichmentCache["ws-1"].response.CommitsAhead)
}

func TestWorkspaceEnrichmentRejectsResultAfterGenerationIsTrimmed(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		now:                            time.Now,
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: make(map[string]uint64),
	}
	ahead := 1

	_, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		"deleted-workspace",
		0,
		workspaceEnrichmentProbeResult{
			response:           workspaceResponse{CommitsAhead: &ahead},
			divergenceComplete: true,
		},
	)

	assert.False(recorded)
	assert.NotContains(srv.workspaceEnrichmentCache, "deleted-workspace")
}

func TestWorkspaceEnrichmentSupersededResponseUsesCurrentCacheState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	srv := &Handler{now: func() time.Time { return now }}
	summary := db.WorkspaceSummary{
		ID:     "ws-superseded",
		Status: "ready"}
	currentAhead := 7
	entry := workspaceEnrichmentCacheEntry{
		response:              workspaceResponse{CommitsAhead: &currentAhead},
		hasDivergence:         true,
		divergenceRefreshedAt: now,
	}
	rejectedAhead := 1
	rejectedError := "rejected probe failed"
	result := workspaceEnrichmentProbeResult{response: workspaceResponse{
		CommitsAhead:     &rejectedAhead,
		TmuxWorking:      true,
		EnrichmentStatus: workspaceEnrichmentFailed,
		EnrichmentError:  &rejectedError,
	}}

	response := srv.workspaceResponseAfterEnrichmentAttempt(
		&summary, result, entry, false,
	)

	require.NotNil(response.CommitsAhead)
	assert.Equal(currentAhead, *response.CommitsAhead)
	assert.False(response.TmuxWorking)
	assert.Equal(workspaceEnrichmentPending, response.EnrichmentStatus)
	assert.Nil(response.EnrichmentError)
}

func TestWorkspaceEnrichmentPendingJobUsesLatestSummary(t *testing.T) {
	require := require.New(t)
	srv := newEnrichmentTestHandler(t, "")
	srv.workspaceEnrichmentDisabled = false
	for range cap(srv.workspaceEnrichmentSlots) {
		srv.workspaceEnrichmentSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(srv.workspaceEnrichmentSlots) {
			<-srv.workspaceEnrichmentSlots
		}
	})

	srv.scheduleWorkspaceEnrichment(db.WorkspaceSummary{
		ID: "ws-latest", Status: "ready", WorktreePath: "/old"})
	srv.scheduleWorkspaceEnrichment(db.WorkspaceSummary{
		ID: "ws-latest", Status: "ready", WorktreePath: "/new"})

	srv.workspaceEnrichmentMu.Lock()
	pending := srv.workspaceEnrichmentPending["ws-latest"]
	srv.workspaceEnrichmentMu.Unlock()
	require.Equal("/new", pending.summary.WorktreePath)
}

func TestTrimWorkspaceEnrichmentCacheDropsDeletedPendingState(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		workspaceEnrichmentCache: map[string]workspaceEnrichmentCacheEntry{
			"keep": {},
			"drop": {},
		},
		workspaceEnrichmentGenerations: map[string]uint64{
			"keep":            1,
			"drop":            2,
			"generation-only": 3,
		},
		workspaceEnrichmentPending: map[string]workspaceEnrichmentJob{
			"drop": {generation: 2},
		},
	}

	srv.trimWorkspaceEnrichmentCache([]db.WorkspaceSummary{{ID: "keep"}})

	assert.Contains(srv.workspaceEnrichmentCache, "keep")
	assert.NotContains(srv.workspaceEnrichmentCache, "drop")
	assert.NotContains(srv.workspaceEnrichmentGenerations, "drop")
	assert.NotContains(srv.workspaceEnrichmentGenerations, "generation-only")
	assert.NotContains(srv.workspaceEnrichmentPending, "drop")
}

func TestCachedWorkspaceEnrichmentReportsStaleAndFailedState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newEnrichmentTestHandler(t, "")
	srv.workspaceEnrichmentDisabled = false
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	for range cap(srv.workspaceEnrichmentSlots) {
		srv.workspaceEnrichmentSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(srv.workspaceEnrichmentSlots) {
			<-srv.workspaceEnrichmentSlots
		}
	})
	summary := db.WorkspaceSummary{
		ID:     "ws-status",
		Status: "ready"}
	ahead := 2
	srv.workspaceEnrichmentCache[summary.ID] = workspaceEnrichmentCacheEntry{
		response: workspaceResponse{
			CommitsAhead: &ahead,
		},
		hasDivergence:         true,
		divergenceRefreshedAt: now.Add(-workspaceEnrichmentTTL - time.Second),
	}

	stale := srv.toCachedWorkspaceResponse(&summary)
	require.NotNil(stale.CommitsAhead)
	assert.Equal(2, *stale.CommitsAhead)
	assert.Equal("stale", stale.EnrichmentStatus)
	require.NotNil(stale.EnrichmentRefreshedAt)
	assert.Nil(stale.EnrichmentError)

	srv.workspaceEnrichmentMu.Lock()
	entry := srv.workspaceEnrichmentCache[summary.ID]
	entry.lastAttemptAt = now
	entry.tmuxError = "tmux activity probe failed"
	srv.workspaceEnrichmentCache[summary.ID] = entry
	srv.workspaceEnrichmentMu.Unlock()

	failed := srv.toCachedWorkspaceResponse(&summary)
	assert.Equal("failed", failed.EnrichmentStatus)
	require.NotNil(failed.EnrichmentError)
	assert.Equal("tmux activity probe failed", *failed.EnrichmentError)
}

func TestCachedWorkspaceEnrichmentTracksComponentStaleness(t *testing.T) {
	assert := assert.New(t)
	srv := newEnrichmentTestHandler(t, "")
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	srv.workspaceEnrichmentCache["ws"] = workspaceEnrichmentCacheEntry{
		hasDivergence:         true,
		hasTmux:               true,
		divergenceRefreshedAt: now.Add(-workspaceEnrichmentTTL - time.Second),
		tmuxRefreshedAt:       now,
		divergenceAttemptAt:   now.Add(-workspaceEnrichmentTTL - time.Second),
		tmuxAttemptAt:         now,
	}

	_, fullDue := srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentFull)
	_, tmuxDue := srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentTmux)
	assert.True(fullDue)
	assert.False(tmuxDue)

	entry := srv.workspaceEnrichmentCache["ws"]
	entry.divergenceRefreshedAt = now
	entry.divergenceAttemptAt = now
	entry.tmuxRefreshedAt = now.Add(-workspaceEnrichmentTTL - time.Second)
	entry.tmuxAttemptAt = entry.tmuxRefreshedAt
	srv.workspaceEnrichmentCache["ws"] = entry
	_, fullDue = srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentFull)
	_, tmuxDue = srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentTmux)
	assert.True(fullDue)
	assert.True(tmuxDue)
}

func TestCachedWorkspaceEnrichmentDoesNotTreatTmuxAttemptAsDivergenceAttempt(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	srv := newEnrichmentTestHandler(t, "")
	srv.now = func() time.Time { return now }
	srv.workspaceEnrichmentCache["ws"] = workspaceEnrichmentCacheEntry{
		hasTmux:         true,
		tmuxRefreshedAt: now,
		tmuxAttemptAt:   now,
		lastAttemptAt:   now,
	}

	_, fullDue := srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentFull)
	_, tmuxDue := srv.cachedWorkspaceEnrichment("ws", workspaceEnrichmentTmux)

	assert.True(fullDue)
	assert.False(tmuxDue)
}

func TestCachedWorkspaceEnrichmentKeepsFreshTmuxOnlyResultPending(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	srv := &Handler{now: func() time.Time { return now }}
	summary := db.WorkspaceSummary{
		ID: "ws-tmux-only", Status: "ready"}
	entry := workspaceEnrichmentCacheEntry{
		hasTmux: true, tmuxRefreshedAt: now, tmuxAttemptAt: now,
	}

	response := srv.workspaceResponseFromEnrichmentCacheEntry(&summary, &entry)

	assert.Equal(t, workspaceEnrichmentPending, response.EnrichmentStatus)
	assert.Nil(t, response.CommitsAhead)
	assert.Nil(t, response.CommitsBehind)
}

func TestWorkspaceEnrichmentTmuxSuccessPreservesDivergenceFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	srv := &Handler{
		now:                            func() time.Time { return now },
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: map[string]uint64{"ws-component-errors": 1},
	}
	summary := db.WorkspaceSummary{
		ID: "ws-component-errors", Status: "ready"}

	_, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		summary.ID,
		1,
		workspaceEnrichmentProbeResult{
			divergenceErr: errors.New("git divergence probe failed"),
			tmuxErr:       errors.New("tmux activity probe failed"),
			err: errors.Join(
				errors.New("git divergence probe failed"),
				errors.New("tmux activity probe failed"),
			),
			kind: workspaceEnrichmentFull,
		},
	)
	require.True(recorded)
	failed := srv.workspaceResponseFromEnrichmentCacheEntry(
		&summary, new(srv.workspaceEnrichmentCache[summary.ID]),
	)
	require.NotNil(failed.EnrichmentError)
	assert.Contains(*failed.EnrichmentError, "git divergence probe failed")
	assert.Contains(*failed.EnrichmentError, "tmux activity probe failed")

	now = now.Add(time.Second)
	_, recorded, _ = srv.recordWorkspaceEnrichmentResult(
		summary.ID,
		1,
		workspaceEnrichmentProbeResult{
			tmuxComplete: true,
			kind:         workspaceEnrichmentTmux,
		},
	)
	require.True(recorded)
	stillFailed := srv.workspaceResponseFromEnrichmentCacheEntry(
		&summary, new(srv.workspaceEnrichmentCache[summary.ID]),
	)
	require.NotNil(stillFailed.EnrichmentError)
	assert.Contains(*stillFailed.EnrichmentError, "git divergence probe failed")
	assert.NotContains(*stillFailed.EnrichmentError, "tmux activity probe failed")
}

func TestNextWorkspaceEnrichmentJobLeavesUpgradeQueuedDuringActiveFlight(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		workspaceEnrichmentInFlight: map[string]uint64{"ws-upgrade": 3},
		workspaceEnrichmentFlightKinds: map[string]workspaceEnrichmentKind{
			"ws-upgrade": workspaceEnrichmentTmux,
		},
		workspaceEnrichmentGenerations: map[string]uint64{"ws-upgrade": 3},
		workspaceEnrichmentPending: map[string]workspaceEnrichmentJob{
			"ws-upgrade": {
				summary:    db.WorkspaceSummary{Workspace: db.Workspace{ID: "ws-upgrade", Status: "ready"}},
				generation: 3,
				kind:       workspaceEnrichmentFull,
			},
		},
		workspaceEnrichmentWorkers: 2,
	}

	_, prune, ok := srv.nextWorkspaceEnrichmentJob()

	assert.False(ok)
	assert.False(prune)
	assert.Contains(srv.workspaceEnrichmentPending, "ws-upgrade")
	assert.Equal(uint64(3), srv.workspaceEnrichmentInFlight["ws-upgrade"])
	assert.Equal(workspaceEnrichmentTmux, srv.workspaceEnrichmentFlightKinds["ws-upgrade"])
}

func TestFinishWorkspaceEnrichmentRequiresMatchingFlightID(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		workspaceEnrichmentInFlight:    map[string]uint64{"ws-flight": 4},
		workspaceEnrichmentFlightKinds: map[string]workspaceEnrichmentKind{"ws-flight": workspaceEnrichmentFull},
		workspaceEnrichmentFlightIDs:   map[string]uint64{"ws-flight": 12},
	}

	srv.finishWorkspaceEnrichment("ws-flight", 4, 11)
	assert.Contains(srv.workspaceEnrichmentInFlight, "ws-flight")
	assert.Equal(uint64(12), srv.workspaceEnrichmentFlightIDs["ws-flight"])

	srv.finishWorkspaceEnrichment("ws-flight", 4, 12)
	assert.NotContains(srv.workspaceEnrichmentInFlight, "ws-flight")
	assert.NotContains(srv.workspaceEnrichmentFlightKinds, "ws-flight")
	assert.NotContains(srv.workspaceEnrichmentFlightIDs, "ws-flight")
}

func TestWorkspaceEnrichmentRefreshFailurePreservesLastKnownGood(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	require.NoError(os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	srv := newEnrichmentTestHandler(t, script)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	worktree := filepath.Join(dir, "worktree")
	remote := filepath.Join(dir, "remote.git")
	runGit(t, dir, "init", "--bare", "--initial-branch=main", remote)
	require.NoError(os.MkdirAll(worktree, 0o755))
	runGit(t, worktree, "init", "--initial-branch=main")
	runGit(t, worktree, "config", "user.email", "test@test.com")
	runGit(t, worktree, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(worktree, "base.txt"), []byte("base\n"), 0o644))
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "base")
	runGit(t, worktree, "remote", "add", "origin", remote)
	runGit(t, worktree, "push", "-u", "origin", "main")
	title := "last known title"
	trackerNow := now.Add(-tmuxSampleMinInterval - time.Second)
	srv.tmuxActivity = newTmuxActivityTracker(func() time.Time { return trackerNow })
	srv.tmuxActivity.Update("missing-session", tmuxActivityObservation{
		PaneTitle: title,
		Output:    "last known output",
		HasOutput: true,
	})
	trackerNow = now
	lastGood := workspaceEnrichmentCacheEntry{
		response: workspaceResponse{
			TmuxPaneTitle:      &title,
			TmuxWorking:        true,
			TmuxActivitySource: tmuxActivitySourceTitle,
		},
		hasTmux:         true,
		tmuxRefreshedAt: now.Add(-workspaceEnrichmentTTL - time.Second),
	}
	srv.workspaceEnrichmentMu.Lock()
	srv.workspaceEnrichmentCache["ws-failed-refresh"] = lastGood
	srv.workspaceEnrichmentMu.Unlock()

	srv.scheduleWorkspaceEnrichment(db.WorkspaceSummary{
		ID:           "ws-failed-refresh",
		WorktreePath: worktree,
		TmuxSession:  "missing-session",
		Status:       "ready"})

	require.Eventually(func() bool {
		srv.workspaceEnrichmentMu.Lock()
		defer srv.workspaceEnrichmentMu.Unlock()
		_, pending := srv.workspaceEnrichmentPending["ws-failed-refresh"]
		_, inFlight := srv.workspaceEnrichmentInFlight["ws-failed-refresh"]
		entry := srv.workspaceEnrichmentCache["ws-failed-refresh"]
		return !pending && !inFlight && !entry.lastAttemptAt.IsZero()
	}, 2*time.Second, 10*time.Millisecond)
	srv.workspaceEnrichmentMu.Lock()
	got := srv.workspaceEnrichmentCache["ws-failed-refresh"]
	srv.workspaceEnrichmentMu.Unlock()
	assert.Equal(lastGood.response.TmuxPaneTitle, got.response.TmuxPaneTitle)
	assert.Equal(lastGood.response.TmuxWorking, got.response.TmuxWorking)
	assert.Equal(lastGood.response.TmuxActivitySource, got.response.TmuxActivitySource)
	assert.Equal(now, got.divergenceRefreshedAt)
	assert.Equal(lastGood.tmuxRefreshedAt, got.tmuxRefreshedAt)
	assert.Equal(now, got.lastAttemptAt)
	assert.Contains(got.tmuxError, "tmux display-message:")

	missingSummary := db.WorkspaceSummary{
		ID:           "ws-partial-refresh",
		WorktreePath: worktree,
		TmuxSession:  "missing-session-2",
		Status:       "ready"}
	srv.scheduleWorkspaceEnrichment(missingSummary)
	require.Eventually(func() bool {
		srv.workspaceEnrichmentMu.Lock()
		defer srv.workspaceEnrichmentMu.Unlock()
		_, pending := srv.workspaceEnrichmentPending[missingSummary.ID]
		_, inFlight := srv.workspaceEnrichmentInFlight[missingSummary.ID]
		entry := srv.workspaceEnrichmentCache[missingSummary.ID]
		return !pending && !inFlight && !entry.lastAttemptAt.IsZero()
	}, 2*time.Second, 10*time.Millisecond)
	partial := srv.toCachedWorkspaceResponse(&missingSummary)
	assert.Equal(tmuxActivitySourceUnknown, partial.TmuxActivitySource)
	assert.Equal(workspaceEnrichmentFailed, partial.EnrichmentStatus)

	synchronousSummary := missingSummary
	synchronousSummary.ID = "ws-synchronous-refresh"
	srv.workspaceEnrichmentMu.Lock()
	srv.workspaceEnrichmentCache[synchronousSummary.ID] = lastGood
	srv.workspaceEnrichmentMu.Unlock()
	synchronous := srv.refreshWorkspaceResponse(context.Background(), &synchronousSummary)
	assert.Equal(workspaceEnrichmentFailed, synchronous.EnrichmentStatus)
	require.NotNil(synchronous.EnrichmentError)
	assert.Contains(*synchronous.EnrichmentError, "tmux display-message:")
	require.NotNil(synchronous.CommitsAhead)
	require.NotNil(synchronous.CommitsBehind)
	require.NotNil(synchronous.TmuxPaneTitle)
	assert.Equal(title, *synchronous.TmuxPaneTitle)
	assert.True(synchronous.TmuxWorking)
	assert.Equal(tmuxActivitySourceTitle, synchronous.TmuxActivitySource)
	require.NotNil(synchronous.EnrichmentRefreshedAt)
	srv.workspaceEnrichmentMu.Lock()
	synchronousEntry := srv.workspaceEnrichmentCache[synchronousSummary.ID]
	srv.workspaceEnrichmentMu.Unlock()
	assert.True(synchronousEntry.hasTmux)
	assert.Equal(lastGood.response.TmuxPaneTitle, synchronousEntry.response.TmuxPaneTitle)
}

func TestWorkspaceEnrichmentThrottlesPublishedTmuxRecency(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	srv := &Handler{
		now:                            func() time.Time { return now },
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: map[string]uint64{"ws-recency": 1},
	}
	record := func(activityAt time.Time, title string, working bool) workspaceEnrichmentCacheEntry {
		formatted := activityAt.UTC().Format(time.RFC3339)
		entry, recorded, _ := srv.recordWorkspaceEnrichmentResult(
			"ws-recency",
			1,
			workspaceEnrichmentProbeResult{
				response: workspaceResponse{
					TmuxPaneTitle:      &title,
					TmuxWorking:        working,
					TmuxActivitySource: tmuxActivitySourceOutput,
					TmuxLastOutputAt:   &formatted,
				},
				tmuxComplete: true,
				kind:         workspaceEnrichmentTmux,
			},
		)
		require.True(recorded)
		return entry
	}

	firstActivityAt := now
	entry := record(firstActivityAt, "first title", true)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(firstActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)

	now = now.Add(30 * time.Second)
	suppressedActivityAt := now
	entry = record(suppressedActivityAt, "updated title", false)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(firstActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)
	require.NotNil(entry.response.TmuxPaneTitle)
	assert.Equal("updated title", *entry.response.TmuxPaneTitle)
	assert.False(entry.response.TmuxWorking)

	now = now.Add(20 * time.Second)
	entry, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		"ws-recency", 1, workspaceEnrichmentProbeResult{
			tmuxErr: errors.New("tmux probe failed"),
			kind:    workspaceEnrichmentTmux,
		},
	)
	require.True(recorded)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(firstActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)

	now = now.Add(10 * time.Second)
	olderRemainingSessionAt := firstActivityAt.Add(-time.Minute)
	entry = record(olderRemainingSessionAt, "quiet title", false)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(suppressedActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)

	now = now.Add(time.Minute)
	latestActivityAt := now
	entry = record(latestActivityAt, "working again", true)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(latestActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)
}

func TestWorkspaceEnrichmentDoesNotRegressPublishedTmuxRecency(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	srv := &Handler{
		now:                            func() time.Time { return now },
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: map[string]uint64{"ws-regression": 1},
	}
	record := func(activityAt time.Time, title string) workspaceEnrichmentCacheEntry {
		formatted := activityAt.UTC().Format(time.RFC3339)
		entry, recorded, _ := srv.recordWorkspaceEnrichmentResult(
			"ws-regression", 1, workspaceEnrichmentProbeResult{
				response: workspaceResponse{
					TmuxPaneTitle:      &title,
					TmuxActivitySource: tmuxActivitySourceNone,
					TmuxLastOutputAt:   &formatted,
				},
				tmuxComplete: true,
				kind:         workspaceEnrichmentTmux,
			},
		)
		require.True(recorded)
		return entry
	}

	newerActivityAt := now
	record(newerActivityAt, "newer session")
	now = now.Add(time.Minute)
	entry := record(newerActivityAt.Add(-time.Minute), "older remaining session")

	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(newerActivityAt.Format(time.RFC3339), *entry.response.TmuxLastOutputAt)
	require.NotNil(entry.response.TmuxPaneTitle)
	assert.Equal("older remaining session", *entry.response.TmuxPaneTitle)
}

func TestWorkspaceEnrichmentAbsentTmuxRecencyClearsAndResetsThrottle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	srv := &Handler{
		now:                            func() time.Time { return now },
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: map[string]uint64{"ws-reset": 1},
	}
	initial := now
	formatted := initial.Format(time.RFC3339)
	_, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		"ws-reset", 1, workspaceEnrichmentProbeResult{
			response:     workspaceResponse{TmuxLastOutputAt: &formatted},
			tmuxComplete: true,
			kind:         workspaceEnrichmentTmux,
		},
	)
	require.True(recorded)

	now = now.Add(10 * time.Second)
	entry, recorded, _ := srv.recordWorkspaceEnrichmentResult(
		"ws-reset", 1, workspaceEnrichmentProbeResult{
			response:     workspaceResponse{TmuxActivitySource: tmuxActivitySourceNone},
			tmuxComplete: true,
			kind:         workspaceEnrichmentTmux,
		},
	)
	require.True(recorded)
	assert.Nil(entry.response.TmuxLastOutputAt)

	now = now.Add(time.Second)
	olderAfterReset := initial.Add(-time.Minute)
	formatted = olderAfterReset.Format(time.RFC3339)
	entry, recorded, _ = srv.recordWorkspaceEnrichmentResult(
		"ws-reset", 1, workspaceEnrichmentProbeResult{
			response:     workspaceResponse{TmuxLastOutputAt: &formatted},
			tmuxComplete: true,
			kind:         workspaceEnrichmentTmux,
		},
	)
	require.True(recorded)
	require.NotNil(entry.response.TmuxLastOutputAt)
	assert.Equal(formatted, *entry.response.TmuxLastOutputAt)
}

func TestWorkspaceEnrichmentBroadcastsOnlyDurableChanges(t *testing.T) {
	assert := assert.New(t)
	srv := &Handler{
		workspaceEnrichmentCache:       make(map[string]workspaceEnrichmentCacheEntry),
		workspaceEnrichmentGenerations: map[string]uint64{"ws-1": 1},
		now: func() time.Time {
			return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
		},
	}
	ahead := 1
	behind := 0
	clean := false
	dirty := true
	title := "pane"
	record := func(result workspaceEnrichmentProbeResult) bool {
		_, recorded, changed := srv.recordWorkspaceEnrichmentResult("ws-1", 1, result)
		assert.True(recorded)
		return changed
	}

	// First completion is the pending -> fresh transition clients wait on.
	assert.True(record(workspaceEnrichmentProbeResult{
		response:           workspaceResponse{CommitsAhead: &ahead, CommitsBehind: &behind, WorktreeDirty: &clean, TmuxPaneTitle: &title},
		divergenceComplete: true,
		tmuxComplete:       true,
	}))

	// Tmux-activity-only movement (a busy agent changes it every probe)
	// must not notify: broadcasting it re-poked every open view forever.
	spinnerTitle := "pane *"
	assert.False(record(workspaceEnrichmentProbeResult{
		response: workspaceResponse{
			CommitsAhead:  &ahead,
			CommitsBehind: &behind,
			WorktreeDirty: &clean,
			TmuxPaneTitle: &spinnerTitle,
			TmuxWorking:   true,
		},
		divergenceComplete: true,
		tmuxComplete:       true,
	}))

	// Divergence movement notifies.
	newBehind := 3
	assert.True(record(workspaceEnrichmentProbeResult{
		response:           workspaceResponse{CommitsAhead: &ahead, CommitsBehind: &newBehind, WorktreeDirty: &clean, TmuxPaneTitle: &spinnerTitle},
		divergenceComplete: true,
		tmuxComplete:       true,
	}))

	// Worktree cleanliness movement notifies.
	assert.True(record(workspaceEnrichmentProbeResult{
		response:           workspaceResponse{CommitsAhead: &ahead, CommitsBehind: &newBehind, WorktreeDirty: &dirty, TmuxPaneTitle: &spinnerTitle},
		divergenceComplete: true,
		tmuxComplete:       true,
	}))

	// A new failure notifies once; the same repeated failure stays silent.
	assert.True(record(workspaceEnrichmentProbeResult{
		divergenceErr: errors.New("boom"),
		kind:          workspaceEnrichmentFull,
	}))
	assert.False(record(workspaceEnrichmentProbeResult{
		divergenceErr: errors.New("boom"),
		kind:          workspaceEnrichmentFull,
	}))

	// Recovery notifies.
	assert.True(record(workspaceEnrichmentProbeResult{
		response:           workspaceResponse{CommitsAhead: &ahead, CommitsBehind: &newBehind, WorktreeDirty: &dirty, TmuxPaneTitle: &spinnerTitle},
		divergenceComplete: true,
		tmuxComplete:       true,
	}))

	// Tmux-only failures and recovery notify just like divergence failures;
	// only routine activity-field movement stays silent.
	assert.True(record(workspaceEnrichmentProbeResult{
		tmuxErr: errors.New("tmux boom"),
		kind:    workspaceEnrichmentTmux,
	}))
	assert.False(record(workspaceEnrichmentProbeResult{
		tmuxErr: errors.New("tmux boom"),
		kind:    workspaceEnrichmentTmux,
	}))
	assert.True(record(workspaceEnrichmentProbeResult{
		tmuxComplete: true,
		kind:         workspaceEnrichmentTmux,
	}))
}

func TestTmuxOnlyEnrichmentBroadcastsFirstCompletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newEnrichmentTestHandler(t, "")
	srv.workspaceEnrichmentDisabled = false
	var events []Event
	srv.hub.broadcast = func(event Event) uint64 {
		events = append(events, event)
		return uint64(len(events))
	}
	require.NoError(srv.db.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "ws-tmux-broadcast", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeAdHoc,
		ItemKey: "adhoc:tmux-broadcast", WorktreePath: t.TempDir(), Status: "ready",
	}))
	summary, err := srv.db.GetWorkspaceSummary(t.Context(), "ws-tmux-broadcast")
	require.NoError(err)
	require.NotNil(summary)
	srv.workspaceEnrichmentGenerations[summary.ID] = 0
	job := workspaceEnrichmentJob{
		summary:    *summary,
		generation: srv.workspaceEnrichmentGeneration("ws-tmux-broadcast"),
		kind:       workspaceEnrichmentTmux,
	}

	srv.runWorkspaceEnrichmentJob(t.Context(), job)

	require.Len(events, 1)
	assert.Equal(Event{
		Type: "workspace_status",
		Data: map[string]string{"id": "ws-tmux-broadcast"},
	}, events[0])
}

func TestWorkspaceEnrichmentUsesBoundedWorkersPastBackgroundCapacity(t *testing.T) {
	require := require.New(t)
	srv := newEnrichmentTestHandler(t, "")
	srv.workspaceEnrichmentDisabled = false
	for range cap(srv.workspaceEnrichmentSlots) {
		srv.workspaceEnrichmentSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(srv.workspaceEnrichmentSlots) {
			<-srv.workspaceEnrichmentSlots
		}
	})

	for i := range 12 {
		srv.scheduleWorkspaceEnrichment(db.WorkspaceSummary{
			ID:     "ws-" + string(rune('a'+i)),
			Status: "ready"})
	}

	srv.workspaceEnrichmentMu.Lock()
	pending := len(srv.workspaceEnrichmentPending)
	workers := srv.workspaceEnrichmentWorkers
	inFlight := len(srv.workspaceEnrichmentInFlight)
	srv.workspaceEnrichmentMu.Unlock()
	require.Equal(12, pending)
	require.Equal(cap(srv.workspaceEnrichmentSlots), workers)
	require.Zero(inFlight)
}

func TestWorkspaceTmuxPruneUsesEnrichmentBackgroundCapacity(t *testing.T) {
	srv := newEnrichmentTestHandler(t, "")
	srv.workspaceEnrichmentDisabled = false
	for range cap(srv.workspaceEnrichmentSlots) {
		srv.workspaceEnrichmentSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(srv.workspaceEnrichmentSlots) {
			<-srv.workspaceEnrichmentSlots
		}
	})

	srv.scheduleWorkspaceTmuxPrune()

	srv.workspaceEnrichmentMu.Lock()
	pending := srv.workspaceTmuxPrunePending
	inFlight := srv.workspaceTmuxPruneInFlight
	srv.workspaceEnrichmentMu.Unlock()
	assert.True(t, pending)
	assert.False(t, inFlight)
}

func TestWorkspaceRuntimeExitInvalidatesCachedTmuxEnrichment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newEnrichmentTestHandler(t, "")
	activityRoot := t.TempDir()
	workspace := t.TempDir()
	srv.agentActivity = agentactivity.NewStore(activityRoot)
	payload, err := json.Marshal(map[string]string{
		"session_id":      "agent-session",
		"cwd":             workspace,
		"hook_event_name": "UserPromptSubmit",
	})
	require.NoError(err)
	require.NoError(srv.agentActivity.HandleHook(
		"codex", bytes.NewReader(payload), "agent-runtime",
	))
	srv.workspaceEnrichmentCache["ws-runtime"] = workspaceEnrichmentCacheEntry{
		hasTmux:         true,
		tmuxRefreshedAt: srv.now(),
	}

	srv.HandleRuntimeSessionExit(localruntime.SessionInfo{
		WorkspaceID: "ws-runtime",
		Key:         "agent-runtime",
		CreatedAt:   srv.now(),
	})

	assert.NotContains(srv.workspaceEnrichmentCache, "ws-runtime")
	_, ok := srv.agentActivity.SnapshotForWorkspace(
		workspace, []string{"agent-runtime"},
	)
	assert.False(ok)
}
