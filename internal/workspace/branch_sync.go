package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/tokenauth"
)

var (
	ErrWorktreeDirty      = errors.New("worktree has uncommitted changes")
	ErrWorktreeDiverged   = errors.New("worktree branch diverged from upstream")
	ErrWorktreeNoUpstream = errors.New("worktree branch has no upstream")
	ErrWorktreeInSync     = errors.New("worktree branch is already in sync")
)

type branchUpstream struct {
	remote string
	branch string
}

// networkedBranchGit runs a branch-sync git command that contacts the remote
// (fetch, push, or ls-remote) in the worktree dir. Successful stdout is used
// only for explicit read queries and is never returned through the API.
type networkedBranchGit func(ctx context.Context, dir string, args ...string) (string, error)

// branchSyncGit returns the runner used for the networked steps of branch
// push/pull. With clone management configured it routes fetch and push through
// the authenticated gitclone runner so the provider host's PAT or GitHub App
// credential is injected, expired tokens are re-resolved on auth failure, and
// raw git remote output is redacted out of returned errors. Without clone
// management (unmanaged local checkouts and unit tests) it falls back to plain
// git, which carries no injected credential.
func (m *Manager) branchSyncGit(
	platformName, platformHost, owner, name string,
) networkedBranchGit {
	if m.clones == nil {
		return func(ctx context.Context, dir string, args ...string) (string, error) {
			cmd := workspaceGitCommand(ctx, dir, args...)
			out, err := procutil.Output(ctx, cmd, "git subprocess capacity")
			if err == nil {
				return string(out), nil
			}
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				stderr := strings.TrimSpace(string(exitErr.Stderr))
				if stderr != "" {
					return string(out), fmt.Errorf("%w: %s", err, stderr)
				}
			}
			return string(out), err
		}
	}
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		out, err := m.clones.RunGitForNamedRemote(
			ctx, platformName, platformHost, owner, name,
			"origin", dir, args...,
		)
		return string(out), err
	}
}

// PushWorktreeBranch pushes the current branch to its configured upstream.
// This is a user-triggered git operation, so it intentionally uses normal git
// hook behavior instead of the internal no-hooks mutation helper. The fetch
// and push are networked: they run through the host's authenticated git runner
// so managed HTTPS workspaces inject the provider credential, and the push is
// marked as a mutation so it stays on the user's own PAT chain rather than a
// GitHub App installation token.
func (m *Manager) PushWorktreeBranch(
	ctx context.Context,
	workspaceID, platformName, platformHost, owner, name, dir string,
) error {
	validated, requireCredential, err := m.validateBranchSyncLaunchSpec(ctx, workspaceID)
	if err != nil {
		return err
	}
	if validated != nil {
		platformName = validated.Platform
		platformHost = validated.PlatformHost
		owner = validated.RepoOwner
		name = validated.RepoName
		dir = validated.WorktreePath
	}
	if requireCredential {
		ctx = gitclone.WithRequiredCredential(ctx)
	}
	if err := m.verifyRepoRouteUnoccupied(
		ctx, platformName, platformHost, owner, name,
	); err != nil {
		return err
	}
	return pushWorktreeBranch(
		ctx, m.branchSyncGit(platformName, platformHost, owner, name), dir,
	)
}

// PullWorktreeBranch fast-forwards the current branch from its configured
// upstream. It rejects dirty or diverged worktrees so the UI action cannot
// silently merge, rebase, or overwrite local work. The upstream refresh is
// networked and runs through the host's authenticated git runner; the merge
// itself is local against the already-fetched tracking ref.
func (m *Manager) PullWorktreeBranch(
	ctx context.Context,
	workspaceID, platformName, platformHost, owner, name, dir string,
) error {
	validated, requireCredential, err := m.validateBranchSyncLaunchSpec(ctx, workspaceID)
	if err != nil {
		return err
	}
	if validated != nil {
		platformName = validated.Platform
		platformHost = validated.PlatformHost
		owner = validated.RepoOwner
		name = validated.RepoName
		dir = validated.WorktreePath
	}
	if requireCredential {
		ctx = gitclone.WithRequiredCredential(ctx)
	}
	if err := m.verifyRepoRouteUnoccupied(
		ctx, platformName, platformHost, owner, name,
	); err != nil {
		return err
	}
	return pullWorktreeBranch(
		ctx, m.branchSyncGit(platformName, platformHost, owner, name), dir,
	)
}

func (m *Manager) validateBranchSyncLaunchSpec(
	ctx context.Context, workspaceID string,
) (*Workspace, bool, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, false, nil
	}
	if m == nil || m.db == nil {
		return nil, false, ErrWorkspaceNotFound
	}
	workspace, err := m.db.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, false, fmt.Errorf("get workspace for branch synchronization: %w", err)
	}
	if workspace == nil {
		return nil, false, ErrWorkspaceNotFound
	}
	spec, err := m.RequireWorkspaceLaunchSpec(ctx, workspace)
	return workspace, spec != nil && m.requireProviderCredential, err
}

func pushWorktreeBranch(
	ctx context.Context,
	run networkedBranchGit,
	dir string,
) error {
	if err := ensureBranchSyncClean(ctx, dir); err != nil {
		return err
	}
	upstream, err := currentBranchUpstream(ctx, dir)
	if err != nil {
		return err
	}
	upstreamExists, err := remoteBranchExists(ctx, run, dir, upstream)
	if err != nil {
		return err
	}
	if !upstreamExists {
		if err := pushBranch(ctx, run, dir, upstream); err != nil {
			return err
		}
		if err := refreshBranchUpstream(ctx, run, dir, upstream); err != nil {
			return fmt.Errorf("refresh after push: %w", err)
		}
		return nil
	}
	if err := refreshBranchUpstream(ctx, run, dir, upstream); err != nil {
		return err
	}
	div, err := branchSyncDivergence(ctx, dir)
	if err != nil {
		return err
	}
	if div.Behind > 0 {
		return fmt.Errorf("%w: %d ahead, %d behind", ErrWorktreeDiverged, div.Ahead, div.Behind)
	}
	if div.Ahead == 0 {
		return ErrWorktreeInSync
	}
	if err := pushBranch(ctx, run, dir, upstream); err != nil {
		return err
	}
	if err := refreshBranchUpstream(ctx, run, dir, upstream); err != nil {
		return fmt.Errorf("refresh after push: %w", err)
	}
	return nil
}

func remoteBranchExists(
	ctx context.Context,
	run networkedBranchGit,
	dir string,
	upstream branchUpstream,
) (bool, error) {
	ref := "refs/heads/" + upstream.branch
	out, err := run(ctx, dir, "ls-remote", "--heads", upstream.remote, ref)
	if err != nil {
		return false, fmt.Errorf("check remote branch: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != ref {
		return false, fmt.Errorf("check remote branch: unexpected ls-remote output")
	}
	return true, nil
}

func branchUpstreamExists(ctx context.Context, dir string, upstream branchUpstream) (bool, error) {
	ref := "refs/remotes/" + upstream.remote + "/" + upstream.branch
	out, err := gitCombinedOutput(ctx, dir, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, fmt.Errorf("check branch upstream: %w", err)
	}
	return strings.TrimSpace(out) == ref, nil
}

// WorktreeBranchUpstreamMissing reports whether the current branch has an
// origin upstream configured but its local remote-tracking ref does not exist.
func WorktreeBranchUpstreamMissing(ctx context.Context, dir string) (bool, error) {
	upstream, err := currentBranchUpstream(ctx, dir)
	if err != nil {
		if errors.Is(err, ErrWorktreeNoUpstream) {
			return false, nil
		}
		return false, err
	}
	exists, err := branchUpstreamExists(ctx, dir, upstream)
	return !exists, err
}

func pushBranch(ctx context.Context, run networkedBranchGit, dir string, upstream branchUpstream) error {
	// Writes stay on the user's own credential chain so the pushed commits
	// are attributed to the user instead of a GitHub App bot.
	if _, err := run(
		tokenauth.WithMutationAuth(ctx), dir,
		"push", upstream.remote, "HEAD:"+upstream.branch,
	); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

func pullWorktreeBranch(ctx context.Context, run networkedBranchGit, dir string) error {
	if err := ensureBranchSyncClean(ctx, dir); err != nil {
		return err
	}
	upstream, err := currentBranchUpstream(ctx, dir)
	if err != nil {
		return err
	}
	if err := refreshBranchUpstream(ctx, run, dir, upstream); err != nil {
		return err
	}
	div, err := branchSyncDivergence(ctx, dir)
	if err != nil {
		return err
	}
	if div.Ahead > 0 {
		return fmt.Errorf("%w: %d ahead, %d behind", ErrWorktreeDiverged, div.Ahead, div.Behind)
	}
	if div.Behind == 0 {
		return ErrWorktreeInSync
	}
	if _, err := gitCombinedOutput(ctx, dir, "merge", "--ff-only", "@{upstream}"); err != nil {
		return fmt.Errorf("git merge --ff-only upstream: %w", err)
	}
	return nil
}

func ensureBranchSyncClean(ctx context.Context, dir string) error {
	dirty, err := dirtyFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("check worktree dirty state: %w", err)
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%w: %s", ErrWorktreeDirty, strings.Join(dirty, ", "))
	}
	return nil
}

func currentBranchUpstream(ctx context.Context, dir string) (branchUpstream, error) {
	out, err := gitCombinedOutput(ctx, dir, "branch", "--show-current")
	if err != nil {
		return branchUpstream{}, fmt.Errorf("git branch --show-current: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return branchUpstream{}, ErrWorktreeNoUpstream
	}

	remote, err := gitCombinedOutput(ctx, dir, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return branchUpstream{}, fmt.Errorf("%w: branch %s", ErrWorktreeNoUpstream, branch)
	}
	mergeRef, err := gitCombinedOutput(ctx, dir, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return branchUpstream{}, fmt.Errorf("%w: branch %s", ErrWorktreeNoUpstream, branch)
	}
	upstream := branchUpstream{
		remote: strings.TrimSpace(remote),
		branch: strings.TrimPrefix(strings.TrimSpace(mergeRef), "refs/heads/"),
	}
	if upstream.remote == "" || upstream.branch == "" {
		return branchUpstream{}, fmt.Errorf("%w: branch %s", ErrWorktreeNoUpstream, branch)
	}
	if upstream.remote != "origin" {
		return branchUpstream{}, fmt.Errorf(
			"%w: branch %s tracks unsupported remote %s",
			ErrWorktreeNoUpstream, branch, upstream.remote,
		)
	}
	return upstream, nil
}

func refreshBranchUpstream(
	ctx context.Context, run networkedBranchGit, dir string, upstream branchUpstream,
) error {
	refspec := "+refs/heads/" + upstream.branch + ":refs/remotes/" + upstream.remote + "/" + upstream.branch
	if _, err := run(ctx, dir, "fetch", "--prune", upstream.remote, refspec); err != nil {
		return fmt.Errorf("git fetch %s %s: %w", upstream.remote, upstream.branch, err)
	}
	return nil
}

func branchSyncDivergence(ctx context.Context, dir string) (Divergence, error) {
	div, ok, err := WorktreeDivergence(ctx, dir)
	if err != nil {
		return Divergence{}, err
	}
	if !ok {
		return Divergence{}, ErrWorktreeNoUpstream
	}
	return div, nil
}
