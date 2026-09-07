//go:build integration

package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// gitRun runs a git command in the given dir and returns trimmed stdout.
// It filters inherited GIT_* variables so that pre-commit hooks don't
// cause test git operations to mutate the host repo's config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := gitcmd.New().Command(t.Context(), dir, args...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// initTestRepo creates a fresh git repo in dir with one initial commit on main.
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, "", "init", "-b", "main", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644))
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
}

// setupBareClone creates a bare clone of sourceDir under clonesDir/owner/name.git,
// adds the pull refspec, and fetches.
func setupBareClone(t *testing.T, sourceDir, clonesDir, owner, name string) *gitclone.Manager {
	t.Helper()
	mgr := gitclone.New(clonesDir, nil)
	legacyPath, err := mgr.ClonePath("github", "github.com", owner, name)
	require.NoError(t, err)
	identityPath := syncTestClonePath(t, mgr, owner, name)
	for _, barePath := range []string{legacyPath, identityPath} {
		gitRun(t, "", "clone", "--bare", sourceDir, barePath)
		gitRun(t, barePath, "config", "--add", "remote.origin.fetch",
			"+refs/pull/*/head:refs/pull/*/head")
		gitRun(t, barePath, "remote", "set-url", "origin", sourceDir)
		gitRun(t, barePath, "fetch", "--prune", "origin")
	}
	return mgr
}

func syncTestClonePath(
	t *testing.T, mgr *gitclone.Manager, owner, name string,
) string {
	t.Helper()
	path, err := mgr.ClonePathForContext(
		gitclone.WithRepositoryIdentity(
			t.Context(), "repo-"+strings.ToLower(owner+"-"+name),
		),
		"github", "github.com", owner, name,
	)
	require.NoError(t, err)
	return path
}

// setupSyncer creates a syncer with a real DB, bare clone manager, and mock client.
// It runs one sync cycle to create the repo row, then returns the syncer and repo ID.
func setupSyncer(t *testing.T, ctx context.Context, mgr *gitclone.Manager) (*Syncer, int64) {
	t.Helper()
	d := openTestDB(t)
	mc := &mockClient{}
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, mgr, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx) // creates repo row
	repoRow, err := d.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"))
	require.NoError(t, err)
	require.NotNil(t, repoRow)
	return syncer, repoRow.ID
}

// insertMergedPR inserts a merged PR with empty diff SHAs into the DB.
func insertMergedPR(t *testing.T, ctx context.Context, d *db.DB, repoID int64, number int, headSHA string) (int64, int64) {
	t.Helper()
	now := time.Now().UTC()
	mrID, revision, accepted, err := d.UpsertMergeRequestSnapshotWithLabels(ctx, &db.MergeRequest{
		RepoID:          repoID,
		Number:          number,
		Title:           fmt.Sprintf("test PR #%d", number),
		State:           "merged",
		URL:             fmt.Sprintf("https://github.com/owner/repo/pull/%d", number),
		PlatformHeadSHA: headSHA,
		UpdatedAt:       now,
		CreatedAt:       now,
	})
	require.NoError(t, err)
	require.True(t, accepted)
	return mrID, revision
}

func TestIntegrationComputeMergedPRDiffSHAs_MergeCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	// Create repo: initial commit on main.
	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Feature branch with a change.
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Merge commit on main (--no-ff ensures a merge commit).
	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Create pull ref.
	gitRun(t, sourceDir, "update-ref", "refs/pull/1/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")
	syncer, repoID := setupSyncer(t, ctx, mgr)
	mrID, revision := insertMergedPR(t, ctx, syncer.db, repoID, 1, prHead)

	require.NoError(syncer.computeMergedMRDiffSHAs(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, mrID, revision, 1, prHead, "", mergeCommit, false))

	shas, err := syncer.db.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal(prHead, shas.DiffHeadSHA, "diff_head_sha should be the PR head")
	assert.Equal(forkPoint, shas.DiffBaseSHA, "diff_base_sha should be the fork point")
	assert.Equal(forkPoint, shas.MergeBaseSHA, "merge_base_sha should be the fork point")
	assert.NotEqual(prHead, shas.MergeBaseSHA, "diff should not be empty")
}

func TestIntegrationComputeMergedPRDiffSHAs_ForceOverwritesIncorrectSHAs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "update-ref", "refs/pull/1/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")
	syncer, repoID := setupSyncer(t, ctx, mgr)
	mrID, revision := insertMergedPR(t, ctx, syncer.db, repoID, 1, prHead)

	// Seed incorrect diff SHAs (simulating prior SyncMR regression).
	require.NoError(syncer.db.UpdateDiffSHAs(ctx, repoID, 1, "bad-head", "bad-base", "bad-merge-base"))

	// Without force, the existing (incorrect) SHAs are preserved.
	require.NoError(syncer.computeMergedMRDiffSHAs(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, mrID, revision, 1, prHead, "", mergeCommit, false))
	shas, err := syncer.db.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	assert.Equal("bad-head", shas.DiffHeadSHA, "force=false should not overwrite existing SHAs")

	// With force, the incorrect SHAs are replaced with correct values.
	require.NoError(syncer.computeMergedMRDiffSHAs(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, mrID, revision, 1, prHead, "", mergeCommit, true))
	shas, err = syncer.db.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", 1)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal(prHead, shas.DiffHeadSHA, "force=true should overwrite with correct PR head")
	assert.Equal(forkPoint, shas.DiffBaseSHA, "force=true should overwrite with correct fork point")
	assert.Equal(forkPoint, shas.MergeBaseSHA, "force=true should overwrite with correct merge base")
}

func TestIntegrationComputeMergedPRDiffSHAs_SquashMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Squash merge.
	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--squash", "feature")
	gitRun(t, sourceDir, "commit", "-m", "squash: add feature")
	squashCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "update-ref", "refs/pull/2/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")
	syncer, repoID := setupSyncer(t, ctx, mgr)
	mrID, revision := insertMergedPR(t, ctx, syncer.db, repoID, 2, prHead)

	require.NoError(syncer.computeMergedMRDiffSHAs(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, mrID, revision, 2, prHead, "", squashCommit, false))

	shas, err := syncer.db.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", 2)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal(prHead, shas.DiffHeadSHA)
	assert.Equal(forkPoint, shas.DiffBaseSHA)
	assert.Equal(forkPoint, shas.MergeBaseSHA)
	assert.NotEqual(prHead, shas.MergeBaseSHA)
}

func TestIntegrationComputeMergedPRDiffSHAs_RebaseMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Feature branch with two commits.
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("a\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add a")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "b.txt"), []byte("b\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add b")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Advance main so rebase produces new SHAs.
	gitRun(t, sourceDir, "checkout", "main")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "main.txt"), []byte("main\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "advance main")

	// Simulate rebase merge.
	gitRun(t, sourceDir, "checkout", "feature")
	gitRun(t, sourceDir, "rebase", "main")
	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--ff-only", "feature")
	rebaseLastCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Pull ref points to original (pre-rebase) PR head.
	gitRun(t, sourceDir, "update-ref", "refs/pull/3/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")
	syncer, repoID := setupSyncer(t, ctx, mgr)
	mrID, revision := insertMergedPR(t, ctx, syncer.db, repoID, 3, prHead)

	require.NoError(syncer.computeMergedMRDiffSHAs(ctx, RepoRef{Owner: "owner", Name: "repo", PlatformHost: "github.com"}, repoID, mrID, revision, 3, prHead, "", rebaseLastCommit, false))

	shas, err := syncer.db.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", 3)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal(prHead, shas.DiffHeadSHA)
	assert.Equal(forkPoint, shas.DiffBaseSHA)
	assert.Equal(forkPoint, shas.MergeBaseSHA,
		"merge-base should be the original fork point, not the advanced main commit")
	assert.NotEqual(prHead, shas.MergeBaseSHA)
}

// TestSyncOpenToMergedTransition is an end-to-end test that exercises the full
// open -> merged transition through RunOnce. First sync inserts the PR as open
// (before merge); second sync discovers it missing from ListOpenPullRequests,
// calls fetchAndUpdateClosed, and computes diff SHAs via the merged-PR path.
func TestIntegrationSyncOpenToMergedTransition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	// Create repo with initial commit.
	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Create feature branch with a change.
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Snapshot the base SHA while PR is still "open" (before merge).
	gitRun(t, sourceDir, "checkout", "main")
	premergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "HEAD")

	// Create pull ref before merging (as GitHub does for open PRs).
	gitRun(t, sourceDir, "update-ref", "refs/pull/10/head", prHead)

	// Set up bare clone BEFORE the merge -- reflects the pre-merge state.
	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")

	now := time.Now().UTC()
	number := 10
	title := "test merged PR"
	url := "https://github.com/owner/repo/pull/10"
	ghID := int64(10000)
	headRef := "feature"
	baseRef := "main"
	openState := "open"

	openPR := &gh.PullRequest{
		ID:        &ghID,
		Number:    &number,
		Title:     &title,
		HTMLURL:   &url,
		State:     &openState,
		UpdatedAt: makeTimestamp(now),
		CreatedAt: makeTimestamp(now),
		Head: &gh.PullRequestBranch{
			Ref: &headRef,
			SHA: &prHead,
		},
		Base: &gh.PullRequestBranch{
			Ref: &baseRef,
			SHA: &premergeBaseSHA,
		},
	}

	// First sync: PR is open. Diff SHAs should be computed from the
	// pre-merge state (correct fork point).
	mc := &mockClient{
		openPRs: []*gh.PullRequest{openPR},
	}

	d := openTestDB(t)
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, mgr, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, testBudget(500))
	syncer.RunOnce(ctx)

	pr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal(db.MergeRequestStateOpen, pr.State)

	// Verify diff SHAs were computed during open-state sync.
	shasBeforeMerge, err := d.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(shasBeforeMerge)
	assert.Equal(prHead, shasBeforeMerge.DiffHeadSHA, "open-state diff_head_sha")
	assert.Equal(forkPoint, shasBeforeMerge.DiffBaseSHA, "open-state diff_base_sha")
	assert.Equal(forkPoint, shasBeforeMerge.MergeBaseSHA, "open-state merge_base_sha")

	// Now perform the merge in the source repo.
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")
	postmergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "main")

	// Re-fetch the bare clone to pick up the merge commit.
	barePath := syncTestClonePath(t, mgr, "owner", "repo")
	gitRun(t, barePath, "fetch", "--prune", "origin")

	closedState := "closed"
	merged := true
	mergedPR := &gh.PullRequest{
		ID:             &ghID,
		Number:         &number,
		Title:          &title,
		HTMLURL:        &url,
		State:          &closedState,
		Merged:         &merged,
		MergeCommitSHA: &mergeCommit,
		UpdatedAt:      makeTimestamp(now.Add(time.Minute)),
		CreatedAt:      makeTimestamp(now),
		MergedAt:       makeTimestamp(now.Add(time.Minute)),
		ClosedAt:       makeTimestamp(now.Add(time.Minute)),
		Head: &gh.PullRequestBranch{
			Ref: &headRef,
			SHA: &prHead,
		},
		Base: &gh.PullRequestBranch{
			Ref: &baseRef,
			SHA: &postmergeBaseSHA,
		},
	}

	// Second sync: PR disappeared from open list, GetPullRequest returns merged.
	mc.openPRs = nil
	mc.singlePR = mergedPR
	syncer.RunOnce(ctx)

	// Verify PR transitioned to merged state.
	shas, err := d.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal("merged", shas.State, "PR state should be merged")
	// Diff SHAs should still reflect the correct fork point (either preserved
	// from open-state sync or recomputed correctly by the merged path).
	assert.Equal(prHead, shas.DiffHeadSHA, "diff_head_sha should be the PR head")
	assert.Equal(forkPoint, shas.DiffBaseSHA, "diff_base_sha should be the fork point")
	assert.Equal(forkPoint, shas.MergeBaseSHA, "merge_base_sha should be the fork point")
	assert.NotEqual(prHead, shas.MergeBaseSHA, "diff should not be empty")
}

// TestSyncFirstSeenMergedPR exercises the first-seen merged PR path through
// RunOnce. The PR is inserted as open WITHOUT a clone manager (no diff SHAs
// computed), then on the second sync (with clone manager) it transitions to
// merged and computeMergedMRDiffSHAs must fill in the diff SHAs.
func TestIntegrationSyncFirstSeenMergedPR(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "checkout", "main")
	premergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "HEAD")

	gitRun(t, sourceDir, "update-ref", "refs/pull/10/head", prHead)

	now := time.Now().UTC()
	number := 10
	title := "test PR"
	url := "https://github.com/owner/repo/pull/10"
	ghID := int64(10000)
	headRef := "feature"
	baseRef := "main"
	openState := "open"

	openPR := &gh.PullRequest{
		ID:        &ghID,
		Number:    &number,
		Title:     &title,
		HTMLURL:   &url,
		State:     &openState,
		UpdatedAt: makeTimestamp(now),
		CreatedAt: makeTimestamp(now),
		Head:      &gh.PullRequestBranch{Ref: &headRef, SHA: &prHead},
		Base:      &gh.PullRequestBranch{Ref: &baseRef, SHA: &premergeBaseSHA},
	}

	mc := &mockClient{openPRs: []*gh.PullRequest{openPR}}

	// First sync WITHOUT clone manager -- PR inserted as open, no diff SHAs.
	d := openTestDB(t)
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, nil, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)
	syncer.RunOnce(ctx)

	shasEmpty, err := d.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(shasEmpty)
	assert.Empty(shasEmpty.DiffHeadSHA, "diff SHAs should be empty without clone manager")

	// Merge the PR.
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")
	postmergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "main")

	// Set up bare clone (post-merge state).
	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")

	closedState := "closed"
	merged := true
	mergedPR := &gh.PullRequest{
		ID:             &ghID,
		Number:         &number,
		Title:          &title,
		HTMLURL:        &url,
		State:          &closedState,
		Merged:         &merged,
		MergeCommitSHA: &mergeCommit,
		UpdatedAt:      makeTimestamp(now.Add(time.Minute)),
		CreatedAt:      makeTimestamp(now),
		MergedAt:       makeTimestamp(now.Add(time.Minute)),
		ClosedAt:       makeTimestamp(now.Add(time.Minute)),
		Head:           &gh.PullRequestBranch{Ref: &headRef, SHA: &prHead},
		Base:           &gh.PullRequestBranch{Ref: &baseRef, SHA: &postmergeBaseSHA},
	}

	// Second sync WITH clone manager via a new syncer sharing the same DB.
	// PR transitions to merged, computeMergedMRDiffSHAs must run since
	// diff SHAs are empty.
	mc.openPRs = nil
	mc.singlePR = mergedPR
	syncer2 := NewSyncer(map[string]Client{"github.com": mc}, d, mgr, []RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}}, time.Minute, nil, nil)
	syncer2.RunOnce(ctx)

	shas, err := d.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal("merged", shas.State)
	assert.Equal(prHead, shas.DiffHeadSHA, "diff_head_sha should be the PR head")
	assert.Equal(forkPoint, shas.DiffBaseSHA, "diff_base_sha should be the fork point")
	assert.Equal(forkPoint, shas.MergeBaseSHA, "merge_base_sha should be the fork point")
	assert.NotEqual(prHead, shas.MergeBaseSHA, "diff should not be empty")
}

func TestIntegrationSyncClosedMROnProviderRepairsDiffFromStableIdentityClone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)
	forkPoint := gitRun(t, sourceDir, "rev-parse", "HEAD")
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(
		filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644,
	))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")
	gitRun(t, sourceDir, "update-ref", "refs/pull/91/head", prHead)
	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")

	mgr := gitclone.New(clonesDir, nil)
	stablePath := syncTestClonePath(t, mgr, "owner", "repo")
	gitRun(t, "", "clone", "--bare", sourceDir, stablePath)
	gitRun(t, stablePath, "config", "--add", "remote.origin.fetch",
		"+refs/pull/*/head:refs/pull/*/head")
	gitRun(t, stablePath, "remote", "set-url", "origin", sourceDir)
	gitRun(t, stablePath, "fetch", "--prune", "origin")
	legacyPath, err := mgr.ClonePath("github", "github.com", "owner", "repo")
	require.NoError(err)
	assert.NoDirExists(legacyPath,
		"the mutation resync must not depend on a legacy route clone")

	database := openTestDB(t)
	repoID, err := database.UpsertRepo(
		ctx, verifiedGitHubRepoIdentity("github.com", "owner", "repo"),
	)
	require.NoError(err)
	now := time.Now().UTC()
	openPR := buildOpenPR(91, now)
	openPR.Head.SHA = &prHead
	openPR.Base.SHA = &forkPoint
	normalized, err := NormalizePR(repoID, openPR)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, normalized)
	require.NoError(err)

	closedState := "closed"
	merged := true
	mergedAt := now.Add(time.Minute)
	mergedPR := buildOpenPR(91, mergedAt)
	mergedPR.State = &closedState
	mergedPR.Merged = &merged
	mergedPR.MergedAt = makeTimestamp(mergedAt)
	mergedPR.ClosedAt = makeTimestamp(mergedAt)
	mergedPR.MergeCommitSHA = &mergeCommit
	mergedPR.Head.SHA = &prHead
	mergedPR.Base.SHA = &mergeCommit
	client := &mockClient{singlePR: mergedPR}
	syncer := NewSyncer(
		map[string]Client{"github.com": client}, database, mgr,
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			PlatformExternalID: "repo-owner-repo",
			Owner:              "owner", Name: "repo", RepoPath: "owner/repo",
		}},
		time.Minute, nil, testBudget(100),
	)

	require.NoError(syncer.SyncClosedMROnProvider(ctx, repoID, 91))
	shas, err := database.GetDiffSHAsByRepoID(ctx, repoID, 91)
	require.NoError(err)
	require.NotNil(shas)
	assert.Equal(prHead, shas.DiffHeadSHA)
	assert.Equal(forkPoint, shas.DiffBaseSHA)
	assert.Equal(forkPoint, shas.MergeBaseSHA)
}

// TestSyncMRWrapsDiffFailureAsDiffSyncError verifies that when SyncMR cannot
// compute diff SHAs for a merged PR (here: the bare clone is missing the
// merge commit), it still upserts the PR row, refreshes the timeline, and
// returns the failure as a *DiffSyncError. The handler distinguishes this
// from hard sync failures so the user sees a warning instead of a 502.
func TestIntegrationSyncMRWrapsDiffFailureAsDiffSyncError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)

	// Push a feature commit and create a pull ref so the bare clone has
	// a usable PR head; the merge commit will live only in source after
	// the bare clone snapshot, making computeMergedMRDiffSHAs fail to
	// rev-parse it.
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")
	gitRun(t, sourceDir, "update-ref", "refs/pull/42/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")

	// Now merge in the source repo *after* the bare clone has snapshotted,
	// so the merge commit is unreachable from the clone.
	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")
	postmergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "main")

	// Replace the clone's origin with a path that has no new commits, so
	// `git fetch` succeeds but the merge commit never enters the clone.
	emptyRemote := t.TempDir()
	initTestRepo(t, emptyRemote)
	barePath := syncTestClonePath(t, mgr, "owner", "repo")
	gitRun(t, barePath, "remote", "set-url", "origin", emptyRemote)

	number := 42
	title := "diff failure repro"
	url := "https://github.com/owner/repo/pull/42"
	ghID := int64(42000)
	headRef := "feature"
	baseRef := "main"
	state := "closed"
	merged := true
	changedFiles := 1
	now := time.Now().UTC()
	mergedPR := &gh.PullRequest{
		ID:             &ghID,
		Number:         &number,
		Title:          &title,
		HTMLURL:        &url,
		State:          &state,
		Merged:         &merged,
		MergeCommitSHA: &mergeCommit,
		ChangedFiles:   &changedFiles,
		UpdatedAt:      makeTimestamp(now),
		CreatedAt:      makeTimestamp(now),
		MergedAt:       makeTimestamp(now),
		ClosedAt:       makeTimestamp(now),
		Head:           &gh.PullRequestBranch{Ref: &headRef, SHA: &prHead},
		Base:           &gh.PullRequestBranch{Ref: &baseRef, SHA: &postmergeBaseSHA},
	}

	mc := &mockClient{singlePR: mergedPR}
	d := openTestDB(t)
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, mgr,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil)

	err := syncer.SyncMR(ctx, "owner", "repo", number)
	require.Error(err)
	var diffErr *DiffSyncError
	require.ErrorAs(err, &diffErr)
	require.Error(diffErr.Err)

	// The categorized code lets the handler render a sanitized message.
	// rev-parse on the missing merge commit's first parent fails, so the
	// diff path classifies this as commit_unreachable.
	assert.Equal(DiffSyncCodeCommitUnreachable, diffErr.Code)

	// UserMessage must NOT leak clone paths, refs, SHAs, or git stderr.
	userMsg := diffErr.UserMessage()
	assert.NotContains(userMsg, barePath, "user message should not leak local clone path")
	assert.NotContains(userMsg, mergeCommit, "user message should not leak SHAs")
	assert.NotContains(userMsg, "refs/pull/", "user message should not leak git refs")
	assert.NotContains(userMsg, "rev-parse", "user message should not leak git command stderr")

	// PR row was still upserted despite the diff failure.
	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(db.MergeRequestStateMerged, mr.State, "PR state should be persisted")

	// Diff SHAs are absent because rev-parse never found the merge commit.
	shas, err := d.GetDiffSHAs(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(shas)
	assert.Empty(shas.DiffHeadSHA, "diff SHAs should remain unset when computation failed")

	_, archiveErr := syncer.SyncArchiveItem(ctx, platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com", Owner: "owner", Name: "repo",
	}, db.ArchiveItemTypeMergeRequest, number)
	require.NoError(archiveErr, "archive hydration should accept fresh activity when only diff sync failed")
}

// TestSyncItemByNumberReturnsTypeOnDiffSyncError verifies that when SyncMR
// returns a *DiffSyncError for a merged PR, SyncItemByNumber still reports
// the item type as "pr" so the resolve endpoint can route the user to the
// PR detail view. The diff failure is preserved in the returned error so
// the caller can surface it as a warning if it cares.
func TestIntegrationSyncItemByNumberReturnsTypeOnDiffSyncError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := t.Context()
	sourceDir := t.TempDir()
	clonesDir := t.TempDir()

	initTestRepo(t, sourceDir)

	// Set up a feature branch with a pull ref so the bare clone has a
	// usable PR head; the merge commit will live only in source after the
	// snapshot, making computeMergedMRDiffSHAs fail to rev-parse it.
	gitRun(t, sourceDir, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644))
	gitRun(t, sourceDir, "add", ".")
	gitRun(t, sourceDir, "commit", "-m", "add feature")
	prHead := gitRun(t, sourceDir, "rev-parse", "HEAD")
	gitRun(t, sourceDir, "update-ref", "refs/pull/77/head", prHead)

	mgr := setupBareClone(t, sourceDir, clonesDir, "owner", "repo")

	gitRun(t, sourceDir, "checkout", "main")
	gitRun(t, sourceDir, "merge", "--no-ff", "feature", "-m", "Merge feature")
	mergeCommit := gitRun(t, sourceDir, "rev-parse", "HEAD")
	postmergeBaseSHA := gitRun(t, sourceDir, "rev-parse", "main")

	// Cut the clone off from new commits so the merge commit can never
	// reach it via fetch.
	emptyRemote := t.TempDir()
	initTestRepo(t, emptyRemote)
	barePath := syncTestClonePath(t, mgr, "owner", "repo")
	gitRun(t, barePath, "remote", "set-url", "origin", emptyRemote)

	number := 77
	title := "diff failure resolve repro"
	url := "https://github.com/owner/repo/pull/77"
	ghID := int64(77000)
	headRef := "feature"
	baseRef := "main"
	state := "closed"
	merged := true
	now := time.Now().UTC()
	mergedPR := &gh.PullRequest{
		ID:             &ghID,
		Number:         &number,
		Title:          &title,
		HTMLURL:        &url,
		State:          &state,
		Merged:         &merged,
		MergeCommitSHA: &mergeCommit,
		UpdatedAt:      makeTimestamp(now),
		CreatedAt:      makeTimestamp(now),
		MergedAt:       makeTimestamp(now),
		ClosedAt:       makeTimestamp(now),
		Head:           &gh.PullRequestBranch{Ref: &headRef, SHA: &prHead},
		Base:           &gh.PullRequestBranch{Ref: &baseRef, SHA: &postmergeBaseSHA},
	}

	mc := &mockClient{
		singlePR: mergedPR,
		getIssueFn: func(_ context.Context, _, _ string, _ int) (*gh.Issue, error) {
			return &gh.Issue{
				Number:           &number,
				PullRequestLinks: &gh.PullRequestLinks{HTMLURL: &url},
			}, nil
		},
	}
	d := openTestDB(t)
	syncer := NewSyncer(map[string]Client{"github.com": mc}, d, mgr,
		[]RepoRef{{Owner: "owner", Name: "repo", PlatformHost: "github.com"}},
		time.Minute, nil, nil)

	itemType, err := syncer.SyncItemByNumber(ctx, "owner", "repo", number)
	require.Error(err)
	var diffErr *DiffSyncError
	require.ErrorAs(err, &diffErr)
	assert.Equal("pr", itemType, "type should be reported even when diff sync failed")

	// PR row was still upserted, so the resolve endpoint can route the user.
	mr, err := d.GetMergeRequest(ctx, "github", "github.com", "owner", "repo", number)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal(db.MergeRequestStateMerged, mr.State)
}
