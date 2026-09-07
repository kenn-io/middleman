package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

type remoteHeadKey struct {
	WorkspaceID  string
	RepoID       int64
	Provider     platform.Kind
	PlatformHost string
	RepoPath     string
	ItemType     string
	ItemNumber   int
	RemoteName   string
	BranchName   string
	TrackingRef  string
}

type remoteHeadObservation struct {
	SHA                    string
	ObservedAt             time.Time
	LastRefreshEnqueuedAt  time.Time
	LastRefreshSucceededAt time.Time
}

type PushedHeadUpdate struct {
	WorkspaceID  string
	RepoID       int64
	Provider     platform.Kind
	PlatformHost string
	RepoPath     string
	Owner        string
	Name         string
	Number       int
	OldSHA       string
	NewSHA       string
	RemoteName   string
	BranchName   string
	TrackingRef  string
	ObservedAt   time.Time
}

type WorkspacePRAssociation struct {
	WorkspaceID  string
	Provider     platform.Kind
	PlatformHost string
	RepoPath     string
	Owner        string
	Name         string
	IssueNumber  int
	PRNumber     int
	AssociatedAt time.Time
}

type PushedHeadPassResult struct {
	Associations []WorkspacePRAssociation
	HeadChanges  []PushedHeadUpdate
}

type remoteHeadGitReader interface {
	BranchName(ctx context.Context, dir string) (string, error)
	UpstreamState(ctx context.Context, dir, branch string) (upstreamState, error)
	RemoteTrackingSHA(ctx context.Context, dir, remote, branch string) (string, string, bool, error)
	SetBranchUpstream(ctx context.Context, dir, branch, remote, mergeRef string) error
}

const (
	pushedHeadGitTimeout           = 2 * time.Second
	pushedHeadRefreshRetryInterval = 30 * time.Second
)

type PushedHeadObserver struct {
	db       *db.DB
	monitor  *PRMonitor
	git      remoteHeadGitReader
	now      func() time.Time
	mu       sync.Mutex
	observed map[remoteHeadKey]remoteHeadObservation
	failures map[string]int
}

func NewPushedHeadObserver(
	database *db.DB, monitorOptions ...PRMonitorOptions,
) *PushedHeadObserver {
	return &PushedHeadObserver{
		db:       database,
		monitor:  NewPRMonitor(database, monitorOptions...),
		git:      newGitdirRemoteHeadReader(),
		now:      time.Now,
		observed: make(map[remoteHeadKey]remoteHeadObservation),
		failures: make(map[string]int),
	}
}

func (o *PushedHeadObserver) setGitReaderForTest(reader remoteHeadGitReader) {
	o.git = reader
}

func (o *PushedHeadObserver) setNowForTest(now func() time.Time) {
	o.now = now
}

// MarkRefreshEnqueued and MarkRefreshSucceeded stamp the observation only
// while it still tracks the SHA the refresh was enqueued for. A refresh
// completing after the tracking ref moved on must not stamp the new SHA's
// cycle: its timestamps would satisfy the stop-retrying gate and suppress
// the refresh the new SHA still needs.
func (o *PushedHeadObserver) MarkRefreshEnqueued(update PushedHeadUpdate, at time.Time) {
	key := update.remoteHeadKey()
	o.mu.Lock()
	defer o.mu.Unlock()
	obs := o.observed[key]
	if !strings.EqualFold(obs.SHA, update.NewSHA) {
		return
	}
	obs.LastRefreshEnqueuedAt = at
	o.observed[key] = obs
}

func (o *PushedHeadObserver) MarkRefreshSucceeded(update PushedHeadUpdate, at time.Time) {
	key := update.remoteHeadKey()
	o.mu.Lock()
	defer o.mu.Unlock()
	obs := o.observed[key]
	if !strings.EqualFold(obs.SHA, update.NewSHA) {
		return
	}
	obs.LastRefreshSucceededAt = at
	o.observed[key] = obs
}

func (u PushedHeadUpdate) remoteHeadKey() remoteHeadKey {
	return remoteHeadKey{
		WorkspaceID:  u.WorkspaceID,
		RepoID:       u.RepoID,
		Provider:     u.Provider,
		PlatformHost: u.PlatformHost,
		RepoPath:     u.RepoPath,
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   u.Number,
		RemoteName:   u.RemoteName,
		BranchName:   u.BranchName,
		TrackingRef:  u.TrackingRef,
	}
}

func (o *PushedHeadObserver) RunOnce(ctx context.Context) (PushedHeadPassResult, error) {
	workspaces, err := o.db.ListWorkspaces(ctx)
	if err != nil {
		return PushedHeadPassResult{}, fmt.Errorf("list workspaces: %w", err)
	}

	result := PushedHeadPassResult{}
	trackingCache := make(map[string]trackingLookup)
	for i := range workspaces {
		ws := workspaces[i]
		if retired, retireErr := o.monitor.retireUnresolvedRepositoryWorkspace(ctx, &ws); retired {
			if retireErr != nil {
				o.recordFailure(ws.ID, retireErr)
			}
			continue
		}
		if !pushedHeadWorkspaceEligible(&ws) {
			continue
		}

		assoc, repo, mr, ok, err := o.resolveWorkspacePR(ctx, &ws)
		if err != nil {
			o.recordFailure(ws.ID, err)
			continue
		}
		if assoc != nil {
			result.Associations = append(result.Associations, *assoc)
		}
		if !ok || repo == nil || mr == nil {
			continue
		}

		update, changed, observeErr := o.observeWorkspacePR(ctx, &ws, *repo, *mr, trackingCache)
		if observeErr != nil {
			o.recordFailure(ws.ID, observeErr)
			continue
		}
		o.clearFailure(ws.ID)
		if changed {
			result.HeadChanges = append(result.HeadChanges, update)
		}
	}
	return result, nil
}

func pushedHeadWorkspaceEligible(ws *Workspace) bool {
	if ws == nil || ws.Status != "ready" || strings.TrimSpace(ws.WorktreePath) == "" {
		return false
	}
	if strings.TrimSpace(ws.PlatformHost) == "" || strings.TrimSpace(ws.RepoOwner) == "" || strings.TrimSpace(ws.RepoName) == "" {
		return false
	}
	return ws.ItemType == db.WorkspaceItemTypePullRequest ||
		ws.ItemType == db.WorkspaceItemTypeIssue ||
		ws.ItemType == db.WorkspaceItemTypeKataTask ||
		ws.ItemType == db.WorkspaceItemTypeAdHoc
}

func (o *PushedHeadObserver) resolveWorkspacePR(
	ctx context.Context, ws *Workspace,
) (*WorkspacePRAssociation, *db.Repo, *db.MergeRequest, bool, error) {
	var launchSpec *WorkspaceLaunchSpec
	if o.monitor.launchSpecs != nil && providerBackedWorkspace(ws) {
		var err error
		launchSpec, err = o.monitor.launchSpecs.RequireWorkspaceLaunchSpec(ctx, ws)
		if err != nil {
			return nil, nil, nil, false, err
		}
	}
	repo, err := o.workspaceRepository(ctx, ws, launchSpec)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if repo == nil {
		return nil, nil, nil, false, nil
	}

	prNumber := 0
	var assoc *WorkspacePRAssociation
	switch ws.ItemType {
	case db.WorkspaceItemTypePullRequest:
		prNumber = ws.ItemNumber
	case db.WorkspaceItemTypeIssue, db.WorkspaceItemTypeKataTask, db.WorkspaceItemTypeAdHoc:
		if ws.AssociatedPRNumber != nil {
			prNumber = *ws.AssociatedPRNumber
		} else if ws.ItemType == db.WorkspaceItemTypeKataTask {
			return nil, repo, nil, false, nil
		} else {
			detected, ok, err := o.monitor.detectAssociatedPR(ctx, ws)
			if err != nil {
				return nil, repo, nil, false, err
			}
			if !ok {
				return nil, repo, nil, false, nil
			}
			changed, err := o.db.SetWorkspaceAssociatedPRNumberIfNull(ctx, ws.ID, detected)
			if err != nil {
				return nil, repo, nil, false, fmt.Errorf("set associated PR: %w", err)
			}
			prNumber = detected
			if changed {
				assoc = &WorkspacePRAssociation{
					WorkspaceID:  ws.ID,
					Provider:     workspaceProviderKind(ws),
					PlatformHost: repoProviderHost(*repo),
					RepoPath:     repo.RepoPath,
					Owner:        repo.Owner,
					Name:         repo.Name,
					IssueNumber:  ws.ItemNumber,
					PRNumber:     detected,
					AssociatedAt: o.now().UTC(),
				}
			}
		}
	default:
		return nil, repo, nil, false, nil
	}
	if prNumber == 0 {
		return assoc, repo, nil, false, nil
	}

	candidates, err := o.monitor.listOpenPullCandidates(ctx, ws)
	if err != nil {
		return assoc, repo, nil, false, fmt.Errorf("list open pull candidates: %w", err)
	}
	for i := range candidates {
		if candidates[i].Number == prNumber {
			return assoc, repo, &candidates[i], true, nil
		}
	}
	return assoc, repo, nil, false, nil
}

func (o *PushedHeadObserver) workspaceRepository(
	ctx context.Context,
	ws *Workspace,
	launchSpec *WorkspaceLaunchSpec,
) (*db.Repo, error) {
	if ws.RepoID != 0 {
		repo, err := o.db.GetActiveRepoByID(ctx, ws.RepoID)
		if err != nil || repo == nil {
			return repo, err
		}
		if launchSpec != nil {
			if repo.PlatformRepoID != launchSpec.Repository.PlatformRepoID {
				return nil, errors.New("workspace launch repository identity changed")
			}
			repo.Platform = launchSpec.Repository.Provider
			repo.PlatformHost = launchSpec.Repository.PlatformHost
			repo.Owner = launchSpec.Repository.Owner
			repo.Name = launchSpec.Repository.Name
			repo.RepoPath = launchSpec.Repository.Owner + "/" + launchSpec.Repository.Name
			repo.CloneURL = launchSpec.Repository.CloneURL
			repo.DefaultBranch = launchSpec.Repository.DefaultBranch
		}
		return repo, nil
	}
	if launchSpec != nil {
		return &db.Repo{
			Platform:       launchSpec.Repository.Provider,
			PlatformHost:   launchSpec.Repository.PlatformHost,
			PlatformRepoID: launchSpec.Repository.PlatformRepoID,
			Owner:          launchSpec.Repository.Owner,
			Name:           launchSpec.Repository.Name,
			RepoPath:       launchSpec.Repository.Owner + "/" + launchSpec.Repository.Name,
			CloneURL:       launchSpec.Repository.CloneURL,
			DefaultBranch:  launchSpec.Repository.DefaultBranch,
		}, nil
	}
	collision, err := o.db.WorkspaceRepoRouteHasHistoricalOccupants(
		ctx, workspaceProvider(ws), ws.PlatformHost, ws.RepoOwner, ws.RepoName,
	)
	if err != nil || collision {
		return nil, err
	}
	repo, err := o.db.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     workspaceProvider(ws),
		PlatformHost: ws.PlatformHost,
		Owner:        ws.RepoOwner,
		Name:         ws.RepoName,
	})
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return repo, nil
}

type trackingLookup struct {
	sha string
	ref string
	ok  bool
	err error
}

func (o *PushedHeadObserver) observeWorkspacePR(ctx context.Context, ws *Workspace, repo db.Repo, mr db.MergeRequest, trackingCache map[string]trackingLookup) (PushedHeadUpdate, bool, error) {
	branch, err := o.git.BranchName(ctx, ws.WorktreePath)
	if err != nil {
		return PushedHeadUpdate{}, false, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return PushedHeadUpdate{}, false, nil
	}

	upstream, err := o.git.UpstreamState(ctx, ws.WorktreePath, branch)
	if err != nil {
		return PushedHeadUpdate{}, false, err
	}
	if !upstream.hasTracking || upstream.remoteName == "" || upstream.branchName == "" {
		healed, healErr := o.configureMissingUpstream(ctx, ws, mr, branch, trackingCache)
		if healErr != nil {
			return PushedHeadUpdate{}, false, healErr
		}
		if !healed {
			slog.Debug("workspace pushed-head observer missing upstream", "workspace_id", ws.ID, "branch", branch)
			return PushedHeadUpdate{}, false, nil
		}
		upstream = upstreamState{
			hasTracking: true,
			remoteName:  "origin",
			branchName:  mr.HeadBranch,
		}
	}
	if upstream.branchName != mr.HeadBranch {
		return PushedHeadUpdate{}, false, nil
	}

	lookup := o.lookupRemoteTrackingSHA(
		ctx, ws.WorktreePath, upstream.remoteName, upstream.branchName,
		trackingCache,
	)
	if lookup.err != nil {
		return PushedHeadUpdate{}, false, lookup.err
	}
	if !lookup.ok || lookup.sha == "" {
		slog.Debug("workspace pushed-head observer missing tracking ref", "workspace_id", ws.ID, "remote", upstream.remoteName, "branch", upstream.branchName)
		return PushedHeadUpdate{}, false, nil
	}

	provider := repoProviderKind(repo)
	host := repoProviderHost(repo)
	observedAt := o.now().UTC()
	key := remoteHeadKey{
		WorkspaceID:  ws.ID,
		RepoID:       repo.ID,
		Provider:     provider,
		PlatformHost: host,
		RepoPath:     repo.RepoPath,
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   mr.Number,
		RemoteName:   upstream.remoteName,
		BranchName:   upstream.branchName,
		TrackingRef:  lookup.ref,
	}
	update := PushedHeadUpdate{
		WorkspaceID:  ws.ID,
		RepoID:       repo.ID,
		Provider:     provider,
		PlatformHost: host,
		RepoPath:     repo.RepoPath,
		Owner:        repo.Owner,
		Name:         repo.Name,
		Number:       mr.Number,
		NewSHA:       lookup.sha,
		RemoteName:   upstream.remoteName,
		BranchName:   upstream.branchName,
		TrackingRef:  lookup.ref,
		ObservedAt:   observedAt,
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	prior, seen := o.observed[key]
	if !seen {
		o.observed[key] = remoteHeadObservation{SHA: lookup.sha, ObservedAt: observedAt}
		providerSHA := strings.TrimSpace(mr.PlatformHeadSHA)
		if providerSHA == "" || strings.EqualFold(lookup.sha, providerSHA) {
			return PushedHeadUpdate{}, false, nil
		}
		update.OldSHA = providerSHA
		return update, true, nil
	}
	if strings.EqualFold(prior.SHA, lookup.sha) {
		prior.ObservedAt = observedAt
		o.observed[key] = prior
		providerSHA := strings.TrimSpace(mr.PlatformHeadSHA)
		if providerSHA == "" || strings.EqualFold(providerSHA, lookup.sha) {
			return PushedHeadUpdate{}, false, nil
		}
		// A refresh enqueued for this same observed SHA already completed:
		// the provider authoritatively answered with a head that still
		// differs, meaning the local tracking ref is stale (the PR moved
		// somewhere else), not that the provider lags a local push.
		// Re-syncing cannot converge, so stop until the local ref moves;
		// retries stay reserved for refreshes that failed outright.
		if !prior.LastRefreshSucceededAt.IsZero() && !prior.LastRefreshSucceededAt.Before(prior.LastRefreshEnqueuedAt) {
			return PushedHeadUpdate{}, false, nil
		}
		if !prior.LastRefreshEnqueuedAt.IsZero() && observedAt.Sub(prior.LastRefreshEnqueuedAt) < pushedHeadRefreshRetryInterval {
			return PushedHeadUpdate{}, false, nil
		}
		update.OldSHA = providerSHA
		return update, true, nil
	}
	update.OldSHA = prior.SHA
	// The refresh timestamps belong to the previously observed SHA. If they
	// survived a SHA change and the enqueue for the new SHA is dropped (a
	// same-key detail sync already in flight), the old success stamp would
	// satisfy the stop-retrying gate above and permanently suppress the new
	// SHA's refresh.
	prior = remoteHeadObservation{SHA: lookup.sha, ObservedAt: observedAt}
	o.observed[key] = prior
	return update, true, nil
}

// lookupRemoteTrackingSHA resolves a remote-tracking ref through the pass's
// per-worktree cache so the heal probe and the observation share one git call.
func (o *PushedHeadObserver) lookupRemoteTrackingSHA(
	ctx context.Context, dir, remote, branch string,
	trackingCache map[string]trackingLookup,
) trackingLookup {
	cacheKey := dir + "\x00" + remote + "\x00" + branch
	lookup, cached := trackingCache[cacheKey]
	if !cached {
		sha, ref, ok, err := o.git.RemoteTrackingSHA(ctx, dir, remote, branch)
		lookup = trackingLookup{sha: strings.TrimSpace(sha), ref: ref, ok: ok, err: err}
		trackingCache[cacheKey] = lookup
	}
	return lookup
}

// configureMissingUpstream restores tracking configuration for a workspace
// branch that should follow the open PR's head branch but has no upstream —
// the state every synthetic fallback branch was created in before upstreams
// were configured at worktree add. Without an upstream, every derived surface
// (sidebar ahead/behind counts, push, pull, unpushed-commit flags) silently
// reports nothing.
//
// The rewiring demands positive evidence before touching config. A nil
// workspace MRHeadRepo is not proof of a same-repo head: it is also nil when
// head-repo metadata was unavailable at creation, and issue workspaces never
// set it even when their associated PR is fork-backed. The merge-request row
// must place the head branch in the base repository, the checked-out branch
// must be the PR head branch or kenn-forge's synthetic PR branch (an unrelated
// user branch must not be rewired), and the remote-tracking ref must already
// exist — mirroring worktree creation — so the branch never ends up tracking
// a ref that resolves to nothing.
func (o *PushedHeadObserver) configureMissingUpstream(
	ctx context.Context, ws *Workspace, mr db.MergeRequest, branch string,
	trackingCache map[string]trackingLookup,
) (bool, error) {
	head := strings.TrimSpace(mr.HeadBranch)
	if head == "" {
		return false, nil
	}
	if strings.TrimSpace(mr.HeadRepoCloneURL) == "" || WorkspaceHeadRepo(
		ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName, mr.HeadRepoCloneURL,
	) != nil {
		return false, nil
	}
	synthetic := ws.ItemType == db.WorkspaceItemTypePullRequest &&
		isSyntheticPRWorktreeBranch(ws.ItemNumber, branch)
	if branch != head && !synthetic {
		return false, nil
	}
	lookup := o.lookupRemoteTrackingSHA(
		ctx, ws.WorktreePath, "origin", head, trackingCache,
	)
	if lookup.err != nil {
		return false, lookup.err
	}
	if !lookup.ok || lookup.sha == "" {
		return false, nil
	}
	if err := o.git.SetBranchUpstream(
		ctx, ws.WorktreePath, branch, "origin", "refs/heads/"+head,
	); err != nil {
		return false, fmt.Errorf("configure branch upstream: %w", err)
	}
	slog.Info("configured missing workspace branch upstream",
		"workspace_id", ws.ID, "branch", branch, "head_branch", head)
	return true, nil
}

func (o *PushedHeadObserver) recordFailure(workspaceID string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures[workspaceID]++
	if o.failures[workspaceID] > 1 {
		slog.Warn("workspace pushed-head observer git inspection failed", "workspace_id", workspaceID, "err", err)
		return
	}
	slog.Debug("workspace pushed-head observer git inspection failed", "workspace_id", workspaceID, "err", err)
}

func (o *PushedHeadObserver) clearFailure(workspaceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.failures, workspaceID)
}

func workspaceProvider(ws *Workspace) string {
	provider := strings.TrimSpace(ws.Platform)
	if provider == "" {
		return string(platform.KindGitHub)
	}
	return provider
}

func workspaceProviderKind(ws *Workspace) platform.Kind {
	return platform.Kind(workspaceProvider(ws))
}

func repoProviderKind(repo db.Repo) platform.Kind {
	if strings.TrimSpace(repo.Platform) == "" {
		return platform.KindGitHub
	}
	return platform.Kind(repo.Platform)
}

func repoProviderHost(repo db.Repo) string {
	if strings.TrimSpace(repo.PlatformHost) != "" {
		return repo.PlatformHost
	}
	if host, ok := platform.DefaultHost(repoProviderKind(repo)); ok {
		return host
	}
	return platform.DefaultGitHubHost
}
